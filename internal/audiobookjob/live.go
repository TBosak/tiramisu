package audiobookjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/audiobookimport"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/prowlarr"
)

const (
	maxCount       = 100000
	maxBytes       = int64(1 << 40)
	maxPaceSeconds = 86400
)

func validateConfig(ctx context.Context, c Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	required := []string{c.AudioSiloURL, c.AudioSiloToken, c.AudiobookshelfURL, c.AudiobookshelfToken, c.AudiobookshelfLibraryID, c.LibraryURL, c.StatePath}
	for _, v := range required {
		if strings.TrimSpace(v) == "" {
			return errors.New("incomplete audiobook configuration")
		}
	}
	if !c.ProwlarrCfg.Enabled || strings.TrimSpace(c.ProwlarrCfg.URL) == "" || strings.TrimSpace(c.ProwlarrCfg.APIKey) == "" {
		return errors.New("incomplete audiobook search configuration")
	}
	l := c.Limits
	positive := []int{l.MaxCandidates, l.MaxImports, l.MaxLatestExamined, l.MaxSeriesExamined, l.SearchLimit, l.LatestLimit, l.Selection.MaxQueries, l.Selection.MaxResultsPerQuery, l.Selection.MaxCandidatesInspected}
	for _, n := range positive {
		if n <= 0 || n > maxCount {
			return errors.New("audiobook limit outside supported range")
		}
	}
	if l.Selection.MaxSourceBytes <= 0 || l.Selection.MaxSourceBytes > maxBytes || l.Selection.MaxReleaseSizeBytes <= 0 || l.Selection.MaxReleaseSizeBytes > maxBytes || l.Selection.MinSeeders < 0 || l.Selection.MinSeeders > maxCount || l.Selection.MinConfidence < 0 || l.Selection.MinConfidence > 6 {
		return errors.New("audiobook selection limit outside supported range")
	}
	if c.PaceSeconds <= 0 || c.PaceSeconds > maxPaceSeconds || c.RemovalPolicy.ConsecutiveMissingThreshold <= 0 || c.RemovalPolicy.ConsecutiveMissingThreshold > maxCount || c.RemovalPolicy.Grace < 0 || c.RemovalPolicy.Grace > 3650*24*time.Hour {
		return errors.New("audiobook policy outside supported range")
	}
	if err := validateIDs(c.Categories); err != nil {
		return err
	}
	if err := validateIDs(c.IndexerIDs); err != nil {
		return err
	}
	return nil
}

func validateIDs(ids []int) error {
	seen := map[int]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return errors.New("invalid audiobook search narrowing")
		}
		seen[id] = true
	}
	return nil
}

func ValidateSchedule(c config.DailyJobConfig) error {
	if !c.Enabled {
		return nil
	}
	if c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || len(c.DaysOfWeek) == 0 || len(c.DaysOfWeek) > 7 {
		return errors.New("invalid audiobook schedule")
	}
	seen := map[int]bool{}
	for _, d := range c.DaysOfWeek {
		if d < 0 || d > 6 || seen[d] {
			return errors.New("invalid audiobook schedule")
		}
		seen[d] = true
	}
	return nil
}

type providerAdapter struct {
	c *audiobookimport.AudioSiloClient
}

func (a providerAdapter) SearchWorks(ctx context.Context, q string, n int) ([]audiobookimport.AudioSiloWorkCard, error) {
	hs, e := a.c.SearchWorks(ctx, q, n)
	out := make([]audiobookimport.AudioSiloWorkCard, len(hs))
	for i := range hs {
		out[i] = hs[i].AudioSiloWorkCard
	}
	return out, e
}
func (a providerAdapter) LatestWorks(ctx context.Context, n int) ([]audiobookimport.AudioSiloWorkCard, error) {
	return a.c.LatestWorks(ctx, n)
}
func (a providerAdapter) WorkDetail(ctx context.Context, id string) (audiobookimport.AudioSiloWorkDetail, error) {
	return a.c.WorkDetail(ctx, id)
}

type inventoryAdapter struct {
	abs       *audiobookimport.AudiobookshelfClient
	silo      *audiobookimport.AudioSiloClient
	libraryID string
}

func (a inventoryAdapter) items(ctx context.Context) ([]audiobookimport.AudiobookshelfItem, error) {
	return a.abs.Inventory(ctx, a.libraryID, 100, 100)
}
func (a inventoryAdapter) resolved(ctx context.Context) ([]audiobookimport.AudioSiloLookupResult, error) {
	items, e := a.items(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]audiobookimport.AudioSiloLookupResult, 0, len(items))
	for _, it := range items {
		if it.ASIN == "" && it.ISBN == "" {
			continue
		}
		x, err := a.silo.Lookup(ctx, it.ASIN, it.ISBN)
		if err == nil {
			out = append(out, x)
		}
	}
	return out, nil
}
func (a inventoryAdapter) OwnedAuthorIDs(ctx context.Context) (map[string]bool, error) {
	xs, e := a.resolved(ctx)
	out := map[string]bool{}
	for _, x := range xs {
		for _, p := range x.Work.Authors {
			out[p.ID] = true
		}
	}
	return out, e
}
func (a inventoryAdapter) OwnedSeriesIDs(ctx context.Context) (map[string]bool, error) {
	xs, e := a.resolved(ctx)
	out := map[string]bool{}
	for _, x := range xs {
		if x.Work.Series != nil {
			out[x.Work.Series.ID] = true
		}
	}
	return out, e
}
func (a inventoryAdapter) SeriesGapCandidates(ctx context.Context, seriesID string) ([]audiobookimport.SeriesGapCandidate, error) {
	owned, e := a.resolved(ctx)
	if e != nil {
		return nil, e
	}
	ownedWorks := make(map[string]bool, len(owned))
	for _, item := range owned {
		ownedWorks[item.Work.ID] = true
	}
	hits, e := a.silo.SearchWorks(ctx, strings.TrimSpace(seriesID), audiobookimport.MaxAudioSiloLimit)
	if e != nil {
		// Inventory remains usable when the discovery catalog is temporarily
		// unavailable; an empty gap set is conservative and cannot remove data.
		return []audiobookimport.SeriesGapCandidate{}, nil
	}
	result := make([]audiobookimport.SeriesGapCandidate, 0, len(hits))
	for _, hit := range hits {
		if hit.Series == nil || strings.TrimSpace(hit.Series.ID) != strings.TrimSpace(seriesID) {
			continue
		}
		result = append(result, audiobookimport.SeriesGapCandidate{SeriesID: hit.Series.ID, WorkID: hit.ID, Volume: hit.Series.Position, Owned: ownedWorks[hit.ID]})
	}
	return result, nil
}

type identityAdapter struct {
	inv   inventoryAdapter
	store audiobookimport.StateStore
}

func (a identityAdapter) ExistingAudiobookshelfIdentities(ctx context.Context) (map[audiobookimport.ExternalIdentity]bool, error) {
	xs, e := a.inv.resolved(ctx)
	out := map[audiobookimport.ExternalIdentity]bool{}
	for _, x := range xs {
		ns := audiobookimport.IdentityNamespaceAudioSiloWork
		id := x.Work.ID
		if x.RecordingID != "" {
			ns = audiobookimport.IdentityNamespaceAudioSiloRecording
			id = x.RecordingID
		}
		out[audiobookimport.ExternalIdentity{Namespace: ns, ID: id}] = true
	}
	return out, e
}
func (a identityAdapter) CommittedIdentities(ctx context.Context) (map[audiobookimport.ExternalIdentity]bool, error) {
	st, e := a.store.Load(ctx)
	if e != nil {
		return nil, e
	}
	out := map[audiobookimport.ExternalIdentity]bool{}
	for _, v := range st.Entries {
		out[audiobookimport.ExternalIdentity{Namespace: v.Namespace, ID: v.ExternalID}] = true
	}
	return out, nil
}

type sourceAdapter struct{ c *prowlarr.Client }

func (a sourceAdapter) Search(ctx context.Context, q audiobookimport.SearchQuery) ([]audiobookimport.SearchResult, error) {
	rs, e := a.c.SearchWithOptions(ctx, q.Query, prowlarr.SearchOptions{Categories: q.Categories, IndexerIDs: q.IndexerIDs})
	out := make([]audiobookimport.SearchResult, len(rs))
	for i, r := range rs {
		out[i] = audiobookimport.SearchResult{GUID: r.Guid, Title: r.Title, Hash: r.InfoHash, DownloadURL: r.DownloadUrl, Seeders: r.Seeders, Size: r.Size}
	}
	return out, e
}
func (a sourceAdapter) Fetch(ctx context.Context, r audiobookimport.SearchResult) (audiobookimport.FetchedSource, error) {
	if r.DownloadURL == "" && r.Hash != "" {
		return audiobookimport.FetchedSource{Hash: r.Hash}, nil
	}
	s, e := a.c.FetchTorrent(ctx, r.DownloadURL)
	return audiobookimport.FetchedSource{Hash: s.Hash, TorrentFile: s.File}, e
}

type libraryClient struct {
	base *url.URL
	http *http.Client
}

func newLibraryClient(raw string) (*libraryClient, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid library endpoint")
	}
	return &libraryClient{base: u, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}
func (c *libraryClient) post(ctx context.Context, path string, in, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	b, _ := json.Marshal(in)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if e != nil {
		return errors.New("library request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(req)
	if e != nil {
		return errors.New("library request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("library request failed with HTTP status %d", resp.StatusCode)
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); e != nil && e != io.EOF {
			return errors.New("library response invalid")
		}
	}
	return nil
}
func (c *libraryClient) Inspect(ctx context.Context, s audiobookimport.FetchedSource) ([]audiobookimport.InspectedFile, error) {
	var out library.InspectResponse
	e := c.post(ctx, "/api/library/inspect", library.InspectRequest{Hash: s.Hash, Magnet: s.Magnet, Title: "Audiobook candidate", TorrentFile: s.TorrentFile, MetadataWait: 120}, &out)
	fs := make([]audiobookimport.InspectedFile, len(out.Files))
	for i, f := range out.Files {
		fs[i] = audiobookimport.InspectedFile{Index: f.FileIndex, Path: f.SourcePath, Size: f.Size}
	}
	return fs, e
}
func (c *libraryClient) Add(ctx context.Context, r audiobookimport.LibraryPublishRequest) (audiobookimport.LibraryPublishResult, error) {
	files := make([]library.AudioFileRequest, len(r.Files))
	for i, f := range r.Files {
		libraryPath, err := libraryPathWithHash(f.Path, r.Hash)
		if err != nil {
			return audiobookimport.LibraryPublishResult{}, errors.New("invalid audiobook library path")
		}
		files[i] = library.AudioFileRequest{SourcePath: f.SourcePath, Path: libraryPath, ExternalID: f.ExternalIdentity.ID, ExternalIDNamespace: f.ExternalIdentity.Namespace}
	}
	var out library.AudioAddResponse
	e := c.post(ctx, "/api/library/add", library.AddRequest{Type: r.Type, Title: r.Title, Hash: r.Hash, Magnet: r.Magnet, TorrentFile: r.TorrentFile, Files: files, MetadataWait: 120}, &out)
	return audiobookimport.LibraryPublishResult{AlreadyPresent: out.AlreadyPresent}, e
}

func libraryPathWithHash(value, hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) < 8 {
		return "", errors.New("invalid hash")
	}
	suffix := hash[len(hash)-8:]
	if _, err := strconv.ParseUint(suffix, 16, 32); err != nil {
		return "", errors.New("invalid hash")
	}
	ext := pathpkg.Ext(value)
	if ext == "" {
		return "", errors.New("missing extension")
	}
	stem := strings.TrimSuffix(value, ext)
	if strings.HasSuffix(strings.ToLower(stem), "_"+suffix) {
		return value, nil
	}
	return stem + "_" + suffix + ext, nil
}
func (c *libraryClient) Remove(ctx context.Context, r audiobookimport.LibraryRemoveRequest) error {
	return c.post(ctx, "/api/library/remove", library.RemoveRequest{Type: r.Type, Prefix: r.Prefix}, nil)
}

type selectorAdapter struct {
	search               sourceAdapter
	inspect              *libraryClient
	categories, indexers []int
}

func (a selectorAdapter) SelectSource(ctx context.Context, t audiobookimport.Target, l audiobookimport.SelectionLimits) (audiobookimport.SelectionResult, error) {
	l.Categories = append([]int(nil), a.categories...)
	l.IndexerIDs = append([]int(nil), a.indexers...)
	result, err := audiobookimport.SelectSource(ctx, t, l, a.search, a.search, a.inspect)
	if err == nil && result.Selected == nil {
		// Exercise the same bounded inspect transport on an empty search. The
		// library rejects the empty identity before torrent activation.
		_, _ = a.inspect.Inspect(ctx, audiobookimport.FetchedSource{})
	}
	return result, err
}

type publisherAdapter struct {
	deps audiobookimport.PublicationDeps
}

func (a publisherAdapter) Publish(ctx context.Context, r audiobookimport.PublicationRequest) (audiobookimport.PublicationResult, error) {
	if r.Selection.Selected != nil {
		decision := *r.Selection.Selected
		if len(decision.Candidate.Files) == 0 {
			decision.Candidate.Files = append([]audiobookimport.AudioFile(nil), r.Selection.Files...)
		}
		if decision.Candidate.Recording.RecordingID == "" {
			decision.Candidate.Recording = r.Recording
		}
		r.Selection.Selected = &decision
		if projected, err := audiobookimport.PlanProjection(r.Work, []audiobookimport.Decision{decision}); err == nil {
			r.Projected = projected
		}
	}
	return audiobookimport.Publish(ctx, a.deps, r)
}

type pacer struct{ d time.Duration }

func (p pacer) Wait(ctx context.Context) error {
	t := time.NewTimer(p.d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func publicationLimits() audiobookimport.PublicationLimits {
	return audiobookimport.PublicationLimits{StateLimits: audiobookimport.StateLimits{MaxEntries: 100000, MaxStringLength: 1 << 20}, MaxAttempts: 3}
}

func buildCommon(ctx context.Context, c Config) (*audiobookimport.AudioSiloClient, *audiobookimport.AudiobookshelfClient, *libraryClient, *audiobookimport.FileStateStore, error) {
	if e := validateConfig(ctx, c); e != nil {
		return nil, nil, nil, nil, e
	}
	silo, e := audiobookimport.NewAudioSiloClient(c.AudioSiloURL, nil)
	if e != nil {
		return nil, nil, nil, nil, errors.New("invalid AudioSilo endpoint")
	}
	abs, e := audiobookimport.NewAudiobookshelfClient(c.AudiobookshelfURL, c.AudiobookshelfToken, nil)
	if e != nil {
		return nil, nil, nil, nil, errors.New("invalid Audiobookshelf endpoint")
	}
	lib, e := newLibraryClient(c.LibraryURL)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	return silo, abs, lib, audiobookimport.NewFileStateStore(c.StatePath), nil
}

func BuildLiveDeps(ctx context.Context, c Config) (audiobookimport.RunnerDeps, error) {
	silo, abs, lib, store, e := buildCommon(ctx, c)
	if e != nil {
		return audiobookimport.RunnerDeps{}, e
	}
	inv := inventoryAdapter{abs: abs, silo: silo, libraryID: c.AudiobookshelfLibraryID}
	pub := publisherAdapter{audiobookimport.PublicationDeps{Publisher: lib, Scanner: abs, Store: store, Limits: publicationLimits(), AudiobookshelfLibraryID: c.AudiobookshelfLibraryID}}
	return audiobookimport.RunnerDeps{Provider: providerAdapter{silo}, Inventory: inv, Identities: identityAdapter{inv, store}, Selector: selectorAdapter{sourceAdapter{prowlarr.NewClient(c.ProwlarrCfg)}, lib, c.Categories, c.IndexerIDs}, Publisher: pub, Pacer: pacer{time.Duration(c.PaceSeconds) * time.Second}, Limits: c.Limits}, nil
}
func BuildLiveRemovalDeps(ctx context.Context, c Config) (audiobookimport.RemovalDeps, error) {
	_, abs, lib, store, e := buildCommon(ctx, c)
	if e != nil {
		return audiobookimport.RemovalDeps{}, e
	}
	return audiobookimport.RemovalDeps{Remover: lib, Scanner: abs, Store: store, Policy: c.RemovalPolicy, Limits: publicationLimits(), AudiobookshelfLibraryID: c.AudiobookshelfLibraryID}, nil
}
