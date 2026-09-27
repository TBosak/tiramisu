package audiobookimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	MaxAudioSiloResponseBytes      = 8 << 20
	MaxAudiobookshelfResponseBytes = 8 << 20
	DefaultAudioSiloSearchLimit    = 20
	DefaultAudioSiloLatestLimit    = 12
	MaxAudioSiloLimit              = 50

	maxProviderRedirects       = 8
	maxAudiobookshelfPageSize  = 1000
	maxAudiobookshelfPageCount = 1000
)

// ProviderError describes an HTTP response without retaining the response
// body or request credentials.
type ProviderError struct {
	Status        int
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider request failed with HTTP status %d", e.Status)
}

func IsNotFound(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && providerErr.Status == http.StatusNotFound
}

func IsRetryable(err error) bool {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	return providerErr.Status == http.StatusRequestTimeout ||
		providerErr.Status == http.StatusTooManyRequests ||
		providerErr.Status >= http.StatusInternalServerError
}

type AudioSiloPerson struct{ ID, Name string }
type AudioSiloSeriesRef struct{ ID, Name, Position string }
type AudioSiloWorkCard struct {
	ID, Title, ReleaseDate string
	Authors                []AudioSiloPerson
	Series                 *AudioSiloSeriesRef
	CoverURL, AddedAt      *string
}
type AudioSiloSearchHit struct {
	AudioSiloWorkCard
	Narrators []AudioSiloPerson
}
type AudioSiloASIN struct{ Region, ASIN string }
type AudioSiloRecording struct {
	ID, Publisher  string
	Narrators      []AudioSiloPerson
	Abridged       Abridgement
	RuntimeMinutes *int
	ChapterCount   int
	ASINs          []AudioSiloASIN
	ISBNs          []string
}
type AudioSiloWorkDetail struct {
	ID, Title, Language string
	Authors             []AudioSiloPerson
	Series              []AudioSiloSeriesRef
	Recordings          []AudioSiloRecording
}
type AudioSiloLookupResult struct {
	Work        AudioSiloWorkCard
	RecordingID string
}

type AudioSiloClient struct {
	baseURL *url.URL
	http    *http.Client
}

func NewAudioSiloClient(baseURL string, httpClient *http.Client) (*AudioSiloClient, error) {
	parsed, err := parseProviderBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &AudioSiloClient{baseURL: parsed, http: noRedirectClient(httpClient)}, nil
}

func (c *AudioSiloClient) SearchWorks(ctx context.Context, query string, limit int) ([]AudioSiloSearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("AudioSilo search query is required")
	}
	endpoint, err := providerEndpoint(c.baseURL, "/api/v1/works/search")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("q", query)
	values.Set("limit", strconv.Itoa(normalizeLimit(limit, DefaultAudioSiloSearchLimit)))
	endpoint.RawQuery = values.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results *[]rawSearchHit `json:"results"`
	}
	if err := decodeJSON(body, &response); err != nil {
		return nil, err
	}
	if response.Results == nil {
		return nil, errors.New("AudioSilo search response is missing results")
	}
	hits := make([]AudioSiloSearchHit, len(*response.Results))
	for i, raw := range *response.Results {
		card, err := raw.rawWorkCard.convert()
		if err != nil {
			return nil, fmt.Errorf("invalid AudioSilo search result %d: %w", i, err)
		}
		if raw.Narrators == nil {
			return nil, fmt.Errorf("invalid AudioSilo search result %d: narrators are required", i)
		}
		narrators, err := convertPeople(*raw.Narrators)
		if err != nil {
			return nil, fmt.Errorf("invalid AudioSilo search result %d narrators: %w", i, err)
		}
		hits[i] = AudioSiloSearchHit{AudioSiloWorkCard: card, Narrators: narrators}
	}
	return hits, nil
}

func (c *AudioSiloClient) LatestWorks(ctx context.Context, limit int) ([]AudioSiloWorkCard, error) {
	endpoint, err := providerEndpoint(c.baseURL, "/api/v1/works/latest")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("limit", strconv.Itoa(normalizeLimit(limit, DefaultAudioSiloLatestLimit)))
	endpoint.RawQuery = values.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var response struct {
		Works *[]rawWorkCard `json:"works"`
	}
	if err := decodeJSON(body, &response); err != nil {
		return nil, err
	}
	if response.Works == nil {
		return nil, errors.New("AudioSilo latest response is missing works")
	}
	works := make([]AudioSiloWorkCard, len(*response.Works))
	for i, raw := range *response.Works {
		works[i], err = raw.convert()
		if err != nil {
			return nil, fmt.Errorf("invalid AudioSilo latest work %d: %w", i, err)
		}
	}
	return works, nil
}

func (c *AudioSiloClient) WorkDetail(ctx context.Context, id string) (AudioSiloWorkDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AudioSiloWorkDetail{}, errors.New("AudioSilo work id is required")
	}
	endpoint, err := providerEndpoint(c.baseURL, "/api/v1/works/"+url.PathEscape(id))
	if err != nil {
		return AudioSiloWorkDetail{}, err
	}
	seen := make(map[string]struct{}, maxProviderRedirects)
	for redirects := 0; ; redirects++ {
		if redirects > maxProviderRedirects {
			return AudioSiloWorkDetail{}, errors.New("AudioSilo redirect limit exceeded")
		}
		key := endpoint.String()
		if _, exists := seen[key]; exists {
			return AudioSiloWorkDetail{}, errors.New("AudioSilo redirect loop")
		}
		seen[key] = struct{}{}
		response, body, err := c.do(ctx, http.MethodGet, endpoint, "")
		if err != nil {
			return AudioSiloWorkDetail{}, err
		}
		if response.StatusCode == http.StatusMovedPermanently {
			location := response.Header.Get("Location")
			if location == "" {
				return AudioSiloWorkDetail{}, errors.New("AudioSilo redirect is missing Location")
			}
			reference, parseErr := url.Parse(location)
			if parseErr != nil {
				return AudioSiloWorkDetail{}, errors.New("AudioSilo redirect has invalid Location")
			}
			next := endpoint.ResolveReference(reference)
			if !sameOrigin(c.baseURL, next) {
				return AudioSiloWorkDetail{}, errors.New("AudioSilo redirect changed origin")
			}
			endpoint = next
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return AudioSiloWorkDetail{}, statusError(response)
		}
		var raw rawWorkDetail
		if err := decodeJSON(body, &raw); err != nil {
			return AudioSiloWorkDetail{}, err
		}
		return raw.convert()
	}
}

func (c *AudioSiloClient) Lookup(ctx context.Context, asin, isbn string) (AudioSiloLookupResult, error) {
	asin = strings.TrimSpace(asin)
	isbn = normalizeISBN(isbn)
	if asin == "" && isbn == "" {
		return AudioSiloLookupResult{}, errors.New("an ASIN or ISBN is required")
	}
	endpoint, err := providerEndpoint(c.baseURL, "/api/v1/lookup")
	if err != nil {
		return AudioSiloLookupResult{}, err
	}
	values := endpoint.Query()
	if asin != "" {
		values.Set("asin", asin)
	} else {
		values.Set("isbn", isbn)
	}
	endpoint.RawQuery = values.Encode()
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return AudioSiloLookupResult{}, err
	}
	var response struct {
		Work        *rawWorkCard `json:"work"`
		RecordingID *string      `json:"recording_id"`
	}
	if err := decodeJSON(body, &response); err != nil {
		return AudioSiloLookupResult{}, err
	}
	if response.Work == nil || response.RecordingID == nil {
		return AudioSiloLookupResult{}, errors.New("AudioSilo lookup response is missing required fields")
	}
	work, err := response.Work.convert()
	if err != nil {
		return AudioSiloLookupResult{}, fmt.Errorf("invalid AudioSilo lookup work: %w", err)
	}
	return AudioSiloLookupResult{Work: work, RecordingID: *response.RecordingID}, nil
}

func (c *AudioSiloClient) get(ctx context.Context, endpoint *url.URL) ([]byte, error) {
	response, body, err := c.do(ctx, http.MethodGet, endpoint, "")
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusError(response)
	}
	return body, nil
}

func (c *AudioSiloClient) do(ctx context.Context, method string, endpoint *url.URL, authorization string) (*http.Response, []byte, error) {
	return doProviderRequest(ctx, c.http, method, endpoint, authorization, MaxAudioSiloResponseBytes)
}

type AudiobookshelfItem struct{ ID, Title, AuthorName, SeriesName, ASIN, ISBN string }
type AudiobookshelfScanResult struct{ StatusCode int }
type AudiobookshelfClient struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

func NewAudiobookshelfClient(baseURL, token string, httpClient *http.Client) (*AudiobookshelfClient, error) {
	parsed, err := parseProviderBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &AudiobookshelfClient{baseURL: parsed, token: token, http: noRedirectClient(httpClient)}, nil
}

func (c *AudiobookshelfClient) Inventory(ctx context.Context, libraryID string, pageSize, maxPages int) ([]AudiobookshelfItem, error) {
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return nil, errors.New("Audiobookshelf library id is required")
	}
	if pageSize <= 0 || pageSize > maxAudiobookshelfPageSize {
		return nil, errors.New("Audiobookshelf page size is outside the supported range")
	}
	if maxPages <= 0 || maxPages > maxAudiobookshelfPageCount {
		return nil, errors.New("Audiobookshelf max pages is outside the supported range")
	}
	items := make([]AudiobookshelfItem, 0, pageSize)
	for page := 0; page < maxPages; page++ {
		endpoint, err := providerEndpoint(c.baseURL, "/api/libraries/"+url.PathEscape(libraryID)+"/items")
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		values.Set("limit", strconv.Itoa(pageSize))
		values.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = values.Encode()
		response, body, err := doProviderRequest(ctx, c.http, http.MethodGet, endpoint, c.authorization(), MaxAudiobookshelfResponseBytes)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, statusError(response)
		}
		pageItems, total, err := decodeAudiobookshelfPage(body)
		if err != nil {
			return nil, err
		}
		items = append(items, pageItems...)
		if total != nil && len(items) >= *total {
			return items, nil
		}
		if len(pageItems) < pageSize {
			return items, nil
		}
	}
	return nil, errors.New("Audiobookshelf inventory exceeded the configured page bound")
}

func (c *AudiobookshelfClient) Scan(ctx context.Context, libraryID string) (AudiobookshelfScanResult, error) {
	libraryID = strings.TrimSpace(libraryID)
	if libraryID == "" {
		return AudiobookshelfScanResult{}, errors.New("Audiobookshelf library id is required")
	}
	endpoint, err := providerEndpoint(c.baseURL, "/api/libraries/"+url.PathEscape(libraryID)+"/scan")
	if err != nil {
		return AudiobookshelfScanResult{}, err
	}
	values := endpoint.Query()
	values.Set("force", "1")
	endpoint.RawQuery = values.Encode()
	response, _, err := doProviderRequest(ctx, c.http, http.MethodPost, endpoint, c.authorization(), MaxAudiobookshelfResponseBytes)
	if err != nil {
		return AudiobookshelfScanResult{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AudiobookshelfScanResult{}, statusError(response)
	}
	return AudiobookshelfScanResult{StatusCode: response.StatusCode}, nil
}

func (c *AudiobookshelfClient) authorization() string { return "Bearer " + c.token }

type rawPerson struct {
	ID   *string `json:"id"`
	Name *string `json:"name"`
}

type rawSeries struct {
	ID       *string `json:"id"`
	Name     *string `json:"name"`
	Position *string `json:"position"`
}

type rawWorkCard struct {
	ID          *string         `json:"id"`
	Title       *string         `json:"title"`
	Authors     *[]rawPerson    `json:"authors"`
	Series      json.RawMessage `json:"series"`
	ReleaseDate *string         `json:"release_date"`
	CoverURL    json.RawMessage `json:"cover_url"`
	AddedAt     json.RawMessage `json:"added_at"`
}

type rawSearchHit struct {
	rawWorkCard
	Narrators *[]rawPerson `json:"narrators"`
}

func (raw rawWorkCard) convert() (AudioSiloWorkCard, error) {
	if raw.ID == nil || strings.TrimSpace(*raw.ID) == "" || raw.Title == nil || strings.TrimSpace(*raw.Title) == "" || raw.Authors == nil {
		return AudioSiloWorkCard{}, errors.New("work id, title, and authors are required")
	}
	authors, err := convertPeople(*raw.Authors)
	if err != nil {
		return AudioSiloWorkCard{}, fmt.Errorf("invalid authors: %w", err)
	}
	series, err := convertNullableSeries(raw.Series)
	if err != nil {
		return AudioSiloWorkCard{}, err
	}
	coverURL, err := decodeRequiredNullableString(raw.CoverURL, "cover_url")
	if err != nil {
		return AudioSiloWorkCard{}, err
	}
	addedAt, err := decodeRequiredNullableString(raw.AddedAt, "added_at")
	if err != nil {
		return AudioSiloWorkCard{}, err
	}
	return AudioSiloWorkCard{ID: *raw.ID, Title: *raw.Title, ReleaseDate: valueOrEmpty(raw.ReleaseDate), Authors: authors, Series: series, CoverURL: coverURL, AddedAt: addedAt}, nil
}

type rawASIN struct {
	Region *string `json:"region"`
	ASIN   *string `json:"asin"`
}

type rawRecording struct {
	ID             *string      `json:"id"`
	Narrators      *[]rawPerson `json:"narrators"`
	Abridged       *bool        `json:"abridged"`
	RuntimeMinutes *int         `json:"runtime_min"`
	Publisher      *string      `json:"publisher"`
	ASINs          *[]rawASIN   `json:"asin"`
	ISBNs          *[]string    `json:"isbn"`
	ChapterCount   *int         `json:"chapter_count"`
}

type rawWorkDetail struct {
	ID         *string         `json:"id"`
	Title      *string         `json:"title"`
	Authors    *[]rawPerson    `json:"authors"`
	Language   *string         `json:"language"`
	Series     *[]rawSeries    `json:"series"`
	Recordings json.RawMessage `json:"recordings"`
}

func (raw rawWorkDetail) convert() (AudioSiloWorkDetail, error) {
	if raw.ID == nil || strings.TrimSpace(*raw.ID) == "" || raw.Title == nil || strings.TrimSpace(*raw.Title) == "" || raw.Authors == nil || raw.Language == nil || raw.Series == nil || raw.Recordings == nil {
		return AudioSiloWorkDetail{}, errors.New("work detail is missing required fields")
	}
	authors, err := convertPeople(*raw.Authors)
	if err != nil {
		return AudioSiloWorkDetail{}, fmt.Errorf("invalid authors: %w", err)
	}
	series := make([]AudioSiloSeriesRef, len(*raw.Series))
	for i, value := range *raw.Series {
		series[i], err = value.convert()
		if err != nil {
			return AudioSiloWorkDetail{}, fmt.Errorf("invalid series %d: %w", i, err)
		}
	}
	var recordings []AudioSiloRecording
	if string(raw.Recordings) != "null" {
		var rawRecordings []rawRecording
		if err := json.Unmarshal(raw.Recordings, &rawRecordings); err != nil {
			return AudioSiloWorkDetail{}, fmt.Errorf("invalid recordings: %w", err)
		}
		recordings = make([]AudioSiloRecording, len(rawRecordings))
		for i, value := range rawRecordings {
			recordings[i], err = value.convert()
			if err != nil {
				return AudioSiloWorkDetail{}, fmt.Errorf("invalid recording %d: %w", i, err)
			}
		}
	}
	return AudioSiloWorkDetail{ID: *raw.ID, Title: *raw.Title, Language: *raw.Language, Authors: authors, Series: series, Recordings: recordings}, nil
}

func (raw rawRecording) convert() (AudioSiloRecording, error) {
	if raw.ID == nil || strings.TrimSpace(*raw.ID) == "" || raw.Narrators == nil || raw.ASINs == nil || raw.ISBNs == nil || raw.ChapterCount == nil {
		return AudioSiloRecording{}, errors.New("recording is missing required fields")
	}
	narrators, err := convertPeople(*raw.Narrators)
	if err != nil {
		return AudioSiloRecording{}, fmt.Errorf("invalid narrators: %w", err)
	}
	asins := make([]AudioSiloASIN, len(*raw.ASINs))
	for i, value := range *raw.ASINs {
		if value.Region == nil || value.ASIN == nil || strings.TrimSpace(*value.ASIN) == "" {
			return AudioSiloRecording{}, fmt.Errorf("invalid ASIN %d", i)
		}
		asins[i] = AudioSiloASIN{Region: *value.Region, ASIN: *value.ASIN}
	}
	abridged := AbridgementUnknown
	if raw.Abridged != nil {
		if *raw.Abridged {
			abridged = AbridgementAbridged
		} else {
			abridged = AbridgementUnabridged
		}
	}
	return AudioSiloRecording{ID: *raw.ID, Publisher: valueOrEmpty(raw.Publisher), Narrators: narrators, Abridged: abridged, RuntimeMinutes: copyInt(raw.RuntimeMinutes), ChapterCount: *raw.ChapterCount, ASINs: asins, ISBNs: append([]string(nil), (*raw.ISBNs)...)}, nil
}

func (raw rawSeries) convert() (AudioSiloSeriesRef, error) {
	if raw.ID == nil || raw.Name == nil || raw.Position == nil {
		return AudioSiloSeriesRef{}, errors.New("series id, name, and position are required")
	}
	return AudioSiloSeriesRef{ID: *raw.ID, Name: *raw.Name, Position: *raw.Position}, nil
}

func convertPeople(raw []rawPerson) ([]AudioSiloPerson, error) {
	people := make([]AudioSiloPerson, len(raw))
	for i, person := range raw {
		if person.ID == nil || person.Name == nil || strings.TrimSpace(*person.ID) == "" || strings.TrimSpace(*person.Name) == "" {
			return nil, fmt.Errorf("person %d is missing id or name", i)
		}
		people[i] = AudioSiloPerson{ID: *person.ID, Name: *person.Name}
	}
	return people, nil
}

func convertNullableSeries(raw json.RawMessage) (*AudioSiloSeriesRef, error) {
	if raw == nil {
		return nil, errors.New("series is required")
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var value rawSeries
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid series: %w", err)
	}
	converted, err := value.convert()
	if err != nil {
		return nil, err
	}
	return &converted, nil
}

func decodeRequiredNullableString(raw json.RawMessage, field string) (*string, error) {
	if raw == nil {
		return nil, fmt.Errorf("%s is required", field)
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", field, err)
	}
	return &value, nil
}

type rawAudiobookshelfMetadata struct {
	Title      string `json:"title"`
	AuthorName string `json:"authorName"`
	SeriesName string `json:"seriesName"`
	ASIN       string `json:"asin"`
	ISBN       string `json:"isbn"`
}

type rawAudiobookshelfItem struct {
	ID    *string `json:"id"`
	Media struct {
		Metadata rawAudiobookshelfMetadata `json:"metadata"`
	} `json:"media"`
}

func decodeAudiobookshelfPage(body []byte) ([]AudiobookshelfItem, *int, error) {
	var response struct {
		Results      json.RawMessage `json:"results"`
		LibraryItems json.RawMessage `json:"libraryItems"`
		Total        *int            `json:"total"`
	}
	if err := decodeJSON(body, &response); err != nil {
		return nil, nil, err
	}
	rawItems := response.Results
	if rawItems == nil {
		rawItems = response.LibraryItems
	}
	if rawItems == nil || string(rawItems) == "null" {
		return []AudiobookshelfItem{}, response.Total, nil
	}
	var decoded []rawAudiobookshelfItem
	if err := json.Unmarshal(rawItems, &decoded); err != nil {
		return nil, nil, fmt.Errorf("invalid Audiobookshelf inventory: %w", err)
	}
	items := make([]AudiobookshelfItem, len(decoded))
	for i, raw := range decoded {
		if raw.ID == nil || strings.TrimSpace(*raw.ID) == "" {
			return nil, nil, fmt.Errorf("Audiobookshelf item %d is missing id", i)
		}
		items[i] = AudiobookshelfItem{ID: *raw.ID, Title: raw.Media.Metadata.Title, AuthorName: raw.Media.Metadata.AuthorName, SeriesName: raw.Media.Metadata.SeriesName, ASIN: raw.Media.Metadata.ASIN, ISBN: raw.Media.Metadata.ISBN}
	}
	return items, response.Total, nil
}

func parseProviderBaseURL(value string) (*url.URL, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("provider base URL is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("provider base URL must be an HTTP(S) origin")
	}
	if parsed.User != nil {
		return nil, errors.New("provider base URL must not contain user information")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func providerEndpoint(base *url.URL, escapedSuffix string) (*url.URL, error) {
	result := *base
	rawPath := strings.TrimSuffix(base.EscapedPath(), "/") + escapedSuffix
	decodedPath, err := url.PathUnescape(rawPath)
	if err != nil {
		return nil, errors.New("provider endpoint path is invalid")
	}
	result.Path = decodedPath
	result.RawPath = rawPath
	result.RawQuery = ""
	result.Fragment = ""
	return &result, nil
}

func noRedirectClient(source *http.Client) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	copy := *source
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func doProviderRequest(ctx context.Context, client *http.Client, method string, endpoint *url.URL, authorization string, maxBytes int64) (*http.Response, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return nil, nil, errors.New("could not create provider request")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, errors.New("provider request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, errors.New("provider response read failed")
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, errors.New("provider response exceeds size limit")
	}
	return response, body, nil
}

func statusError(response *http.Response) error {
	providerErr := &ProviderError{Status: response.StatusCode}
	if retryAfter, ok := parseRetryAfter(response.Header.Get("Retry-After")); ok {
		providerErr.RetryAfter = retryAfter
		providerErr.HasRetryAfter = true
	}
	return providerErr
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		return delay, true
	}
	return 0, false
}

func decodeJSON(body []byte, destination any) error {
	if err := json.Unmarshal(body, destination); err != nil {
		return errors.New("provider response contains invalid JSON")
	}
	return nil
}

func normalizeLimit(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > MaxAudioSiloLimit {
		return MaxAudioSiloLimit
	}
	return limit
}

func normalizeISBN(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "-", "")
	return strings.ReplaceAll(value, " ", "")
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func copyInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
