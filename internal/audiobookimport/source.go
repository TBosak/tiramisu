package audiobookimport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const (
	reasonSearchFailed     Reason = "search_failed"
	reasonFetchFailed      Reason = "fetch_failed"
	reasonHashMismatch     Reason = "hash_mismatch"
	reasonMissingSource    Reason = "missing_source"
	reasonEmptySource      Reason = "empty_source"
	reasonOversizedSource  Reason = "oversized_source"
	reasonMalformedSource  Reason = "malformed_source"
	reasonInspectionFailed Reason = "inspection_failed"
	reasonBelowSeederFloor Reason = "below_seeder_floor"
	reasonOverSizeFloor    Reason = "over_size_floor"
	reasonBelowConfidence  Reason = "below_confidence"
	reasonInvalidLimits    Reason = "invalid_limits"
)

type SearchQuery struct {
	Query      string
	Categories []int
	IndexerIDs []int
}

type SearchResult struct {
	GUID        string
	Title       string
	Hash        string
	DownloadURL string
	Seeders     int
	Size        int64
}

type SourceSearcher interface {
	Search(ctx context.Context, query SearchQuery) ([]SearchResult, error)
}

type FetchedSource struct {
	Hash        string
	TorrentFile []byte
	Magnet      string
}

type SourceFetcher interface {
	Fetch(ctx context.Context, result SearchResult) (FetchedSource, error)
}

type InspectedFile struct {
	Index int
	Path  string
	Size  int64
}

type ReleaseInspector interface {
	Inspect(ctx context.Context, source FetchedSource) ([]InspectedFile, error)
}

type SelectionLimits struct {
	MaxQueries             int
	MaxResultsPerQuery     int
	MaxCandidatesInspected int
	MaxSourceBytes         int64
	MinSeeders             int
	MaxReleaseSizeBytes    int64
	MinConfidence          int
	Categories             []int
	IndexerIDs             []int
}

type RejectedRelease struct {
	Title  string
	Reason Reason
}

type SelectionResult struct {
	Selected    *Decision
	Hash        string
	TorrentFile []byte
	Magnet      string
	Files       []AudioFile
	Rejected    []RejectedRelease
}

var (
	narratorMarker = regexp.MustCompile(`(?i)\[\s*narrated\s+by\s+([^\]]+?)\s*\]`)
	languageMarker = regexp.MustCompile(`\[([A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*)\]`)
	asinMarker     = regexp.MustCompile(`(?i)\bASIN\s*:?\s*([A-Z0-9]{10})\b`)
	isbnMarker     = regexp.MustCompile(`(?i)\bISBN(?:-1[03])?\s*:?\s*([0-9Xx][0-9Xx -]{8,20}[0-9Xx])\b`)
	runtimeMarker  = regexp.MustCompile(`(?i)\b(\d{1,3})\s*(?:h|hr|hrs|hour|hours)\b(?:\s*(\d{1,2})\s*(?:m|min|mins|minute|minutes)\b)?`)
	magnetHash     = regexp.MustCompile(`(?i)(?:^|[?&])xt=urn:btih:([A-Za-z0-9]+)`)
)

func ExtractReleaseFacts(title string) RecordingFacts {
	var facts RecordingFacts
	for _, match := range narratorMarker.FindAllStringSubmatch(title, -1) {
		facts.Narrators = appendUnique(facts.Narrators, strings.TrimSpace(match[1]))
	}
	if match := languageMarker.FindStringSubmatch(title); len(match) == 2 {
		facts.Language = match[1]
	}
	for _, match := range asinMarker.FindAllStringSubmatch(title, -1) {
		facts.ASINs = appendUnique(facts.ASINs, strings.ToUpper(match[1]))
	}
	for _, match := range isbnMarker.FindAllStringSubmatch(title, -1) {
		value := strings.NewReplacer("-", "", " ", "").Replace(match[1])
		if len(value) == 10 || len(value) == 13 {
			facts.ISBNs = appendUnique(facts.ISBNs, strings.ToUpper(value))
		}
	}
	if match := runtimeMarker.FindStringSubmatch(title); len(match) >= 2 {
		hours, _ := strconv.Atoi(match[1])
		minutes := 0
		if len(match) == 3 && match[2] != "" {
			minutes, _ = strconv.Atoi(match[2])
		}
		if minutes < 60 {
			facts.RuntimeMinutes = hours*60 + minutes
		}
	}
	facts.Abridged = titleAbridgement(title)
	return facts
}

func SelectSource(ctx context.Context, target Target, limits SelectionLimits, searcher SourceSearcher, fetcher SourceFetcher, inspector ReleaseInspector) (SelectionResult, error) {
	if err := validateSelection(target, limits, searcher, fetcher, inspector); err != nil {
		return SelectionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SelectionResult{}, err
	}

	queries := buildSearchQueries(target, limits)
	results := make([]SearchResult, 0)
	successfulQueries := 0
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			return SelectionResult{}, err
		}
		found, err := searcher.Search(ctx, query)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SelectionResult{}, ctxErr
		}
		if err != nil {
			continue
		}
		successfulQueries++
		if len(found) > limits.MaxResultsPerQuery {
			found = found[:limits.MaxResultsPerQuery]
		}
		results = append(results, found...)
	}
	if successfulQueries == 0 {
		return SelectionResult{}, errors.New("audiobook source search failed")
	}

	results = deduplicateResults(results)
	result := SelectionResult{}
	type eligibleSource struct {
		candidate ReleaseCandidate
		source    FetchedSource
		files     []AudioFile
	}
	eligible := make([]eligibleSource, 0)
	admitted := 0
	for _, searchResult := range results {
		if err := ctx.Err(); err != nil {
			return SelectionResult{}, err
		}
		if searchResult.Seeders < limits.MinSeeders {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reasonBelowSeederFloor)
			continue
		}
		if searchResult.Size > limits.MaxReleaseSizeBytes {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reasonOverSizeFloor)
			continue
		}
		if admitted >= limits.MaxCandidatesInspected {
			break
		}
		admitted++

		source, err := fetcher.Fetch(ctx, searchResult)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SelectionResult{}, ctxErr
		}
		if err != nil {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reasonFetchFailed)
			continue
		}
		if reason := validateFetchedSource(searchResult, &source, limits); reason != "" {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reason)
			continue
		}

		inspected, err := inspector.Inspect(ctx, source)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SelectionResult{}, ctxErr
		}
		if err != nil {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reasonInspectionFailed)
			continue
		}
		files := make([]AudioFile, len(inspected))
		for i, file := range inspected {
			files[i] = AudioFile{Index: file.Index, Path: file.Path, Size: file.Size}
		}
		candidate := ReleaseCandidate{
			Title:     searchResult.Title,
			Seeders:   searchResult.Seeders,
			Recording: ExtractReleaseFacts(searchResult.Title),
			Files:     files,
		}
		decision := Evaluate(target, candidate)
		if !decision.Eligible {
			reason := reasonInvalidFiles
			if len(decision.Reasons) > 0 {
				reason = decision.Reasons[0]
			}
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reason)
			continue
		}
		if evidenceScore(decision.Evidence) < limits.MinConfidence {
			result.Rejected = appendRejection(result.Rejected, searchResult.Title, reasonBelowConfidence)
			continue
		}
		eligible = append(eligible, eligibleSource{candidate: candidate, source: source, files: files})
	}

	if len(eligible) == 0 {
		return result, nil
	}
	candidates := make([]ReleaseCandidate, len(eligible))
	for i := range eligible {
		candidates[i] = eligible[i].candidate
	}
	ranked := Rank(target, candidates)
	if len(ranked) == 0 {
		return result, nil
	}
	winner := 0
	for i := range eligible {
		if reflect.DeepEqual(eligible[i].candidate, ranked[0].Candidate) {
			winner = i
			break
		}
	}
	decision := ranked[0]
	result.Selected = &decision
	result.Hash = eligible[winner].source.Hash
	result.TorrentFile = append([]byte(nil), eligible[winner].source.TorrentFile...)
	result.Magnet = eligible[winner].source.Magnet
	result.Files = append([]AudioFile(nil), eligible[winner].files...)
	return result, nil
}

func validateSelection(target Target, limits SelectionLimits, searcher SourceSearcher, fetcher SourceFetcher, inspector ReleaseInspector) error {
	if normalize(target.Work.Title) == "" || firstUsable(target.Work.Authors) == "" {
		return fmt.Errorf("%s", reasonInvalidWork)
	}
	if isNilDependency(searcher) || isNilDependency(fetcher) || isNilDependency(inspector) {
		return errors.New("audiobook source dependency is nil")
	}
	const (
		maxQueries             = 16
		maxResultsPerQuery     = 1000
		maxCandidatesInspected = 1000
		maxSourceBytes         = int64(4 << 30)
		maxReleaseBytes        = int64(16 << 40)
	)
	if limits.MaxQueries <= 0 || limits.MaxQueries > maxQueries ||
		limits.MaxResultsPerQuery <= 0 || limits.MaxResultsPerQuery > maxResultsPerQuery ||
		limits.MaxCandidatesInspected <= 0 || limits.MaxCandidatesInspected > maxCandidatesInspected ||
		limits.MaxSourceBytes <= 0 || limits.MaxSourceBytes > maxSourceBytes ||
		limits.MinSeeders < 0 || limits.MaxReleaseSizeBytes <= 0 || limits.MaxReleaseSizeBytes > maxReleaseBytes ||
		limits.MinConfidence < 0 || limits.MinConfidence > 6 {
		return fmt.Errorf("%s", reasonInvalidLimits)
	}
	return nil
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func buildSearchQueries(target Target, limits SelectionLimits) []SearchQuery {
	base := strings.TrimSpace(target.Work.Title + " " + firstUsable(target.Work.Authors))
	plan := []string{base + " audiobook", base}
	if len(plan) > limits.MaxQueries {
		plan = plan[:limits.MaxQueries]
	}
	queries := make([]SearchQuery, 0, len(plan))
	for _, query := range plan {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		queries = append(queries, SearchQuery{
			Query:      query,
			Categories: append([]int(nil), limits.Categories...),
			IndexerIDs: append([]int(nil), limits.IndexerIDs...),
		})
	}
	return queries
}

func deduplicateResults(results []SearchResult) []SearchResult {
	seen := make(map[string]struct{}, len(results))
	unique := make([]SearchResult, 0, len(results))
	for i, result := range results {
		key := ""
		switch {
		case strings.TrimSpace(result.Hash) != "":
			key = "hash:" + strings.ToLower(strings.TrimSpace(result.Hash))
		case strings.TrimSpace(result.GUID) != "":
			key = "guid:" + strings.ToLower(strings.TrimSpace(result.GUID))
		case strings.TrimSpace(result.DownloadURL) != "":
			key = "url:" + strings.TrimSpace(result.DownloadURL)
		default:
			key = fmt.Sprintf("answer:%d", i)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, result)
	}
	return unique
}

func validateFetchedSource(result SearchResult, source *FetchedSource, limits SelectionLimits) Reason {
	inlineHash := strings.TrimSpace(result.Hash)
	fetchedHash := strings.TrimSpace(source.Hash)
	if inlineHash != "" && fetchedHash != "" && !strings.EqualFold(inlineHash, fetchedHash) {
		return reasonHashMismatch
	}
	if source.TorrentFile != nil && source.Magnet != "" {
		return reasonMalformedSource
	}
	if source.TorrentFile != nil && len(source.TorrentFile) == 0 {
		return reasonEmptySource
	}
	if int64(len(source.TorrentFile)) > limits.MaxSourceBytes {
		return reasonOversizedSource
	}
	if fetchedHash == "" {
		fetchedHash = inlineHash
	}
	if fetchedHash == "" && source.Magnet != "" {
		if match := magnetHash.FindStringSubmatch(source.Magnet); len(match) == 2 {
			fetchedHash = match[1]
		}
	}
	if fetchedHash == "" {
		return reasonMissingSource
	}
	source.Hash = fetchedHash
	return ""
}

func appendRejection(rows []RejectedRelease, title string, reason Reason) []RejectedRelease {
	return append(rows, RejectedRelease{Title: title, Reason: reason})
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}
