package audiobookimport

import (
	"context"
	"fmt"
)

// ctxMarkerKey/withMarker/markerFrom let a test prove that the exact context
// SelectSource received - not some unrelated context.Background() built
// internally - is the one forwarded to every dependency call (I1).
type ctxMarkerKey struct{}

func withMarker(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, ctxMarkerKey{}, value)
}

func markerFrom(ctx context.Context) string {
	value, _ := ctx.Value(ctxMarkerKey{}).(string)
	return value
}

// searchStep is one scripted answer for a scriptedSearcher call, in call order.
type searchStep struct {
	results []SearchResult
	err     error
	// onAnswered runs synchronously after this step's answer is computed but
	// before it is returned to the caller - e.g. to cancel the caller's
	// context as a deterministic side effect of "this query's answer has
	// arrived", without any Sleep, channel, or goroutine.
	onAnswered func()
}

// scriptedSearcher answers exactly its scripted steps, in call order, and
// records every query and context marker it received. A call beyond the
// script is a distinct, loud error rather than a silent zero value, so an
// implementation that queries more than the declared plan fails clearly
// instead of being masked by a fake that always answers something.
type scriptedSearcher struct {
	steps   []searchStep
	queries []SearchQuery
	markers []string
}

func (s *scriptedSearcher) Search(ctx context.Context, query SearchQuery) ([]SearchResult, error) {
	idx := len(s.queries)
	s.queries = append(s.queries, query)
	s.markers = append(s.markers, markerFrom(ctx))
	if idx >= len(s.steps) {
		return nil, fmt.Errorf("scriptedSearcher: unscripted call %d for query %q", idx+1, query.Query)
	}
	step := s.steps[idx]
	results, err := step.results, step.err
	if step.onAnswered != nil {
		step.onAnswered()
	}
	return results, err
}

// fetchOutcome is a keyedFetcher's scripted answer for one SearchResult GUID.
type fetchOutcome struct {
	source FetchedSource
	err    error
	// onAnswered runs synchronously after this outcome is computed but before
	// it is returned, for the same reason as searchStep.onAnswered.
	onAnswered func()
}

// keyedFetcher answers by the SearchResult's GUID rather than call order, so
// tests remain valid under any conforming dedup/ordering choice an
// implementation makes, while still recording every call for count and
// order assertions. A GUID with no scripted outcome is a loud error, not a
// silent zero value.
type keyedFetcher struct {
	byGUID  map[string]fetchOutcome
	calls   []SearchResult
	markers []string
}

func (f *keyedFetcher) Fetch(ctx context.Context, result SearchResult) (FetchedSource, error) {
	f.calls = append(f.calls, result)
	f.markers = append(f.markers, markerFrom(ctx))
	outcome, ok := f.byGUID[result.GUID]
	if !ok {
		return FetchedSource{}, fmt.Errorf("keyedFetcher: no scripted outcome for GUID %q", result.GUID)
	}
	if outcome.onAnswered != nil {
		outcome.onAnswered()
	}
	return outcome.source, outcome.err
}

// inspectOutcome is a keyedInspector's scripted answer for one source hash.
type inspectOutcome struct {
	files []InspectedFile
	err   error
}

// keyedInspector answers by FetchedSource.Hash, for the same reason
// keyedFetcher answers by GUID.
type keyedInspector struct {
	byHash  map[string]inspectOutcome
	calls   []FetchedSource
	markers []string
}

func (i *keyedInspector) Inspect(ctx context.Context, source FetchedSource) ([]InspectedFile, error) {
	i.calls = append(i.calls, source)
	i.markers = append(i.markers, markerFrom(ctx))
	outcome, ok := i.byHash[source.Hash]
	if !ok {
		return nil, fmt.Errorf("keyedInspector: no scripted outcome for hash %q", source.Hash)
	}
	return outcome.files, outcome.err
}

// noSteps/noOutcomes build a fake that must never be called: any call
// records itself (so the test can still report what happened) but answers
// with a distinctive "unscripted" error, which is exactly what an
// implementation that skips the required up-front validation would surface.
func noSteps() []searchStep                        { return nil }
func noFetchOutcomes() map[string]fetchOutcome     { return map[string]fetchOutcome{} }
func noInspectOutcomes() map[string]inspectOutcome { return map[string]inspectOutcome{} }

// goodSearchResult is one plausible, well-formed indexer hit.
func goodSearchResult(guid, title, hash string, seeders int, size int64) SearchResult {
	return SearchResult{
		GUID:        guid,
		Title:       title,
		Hash:        hash,
		DownloadURL: "https://indexer.example/dl/" + guid,
		Seeders:     seeders,
		Size:        size,
	}
}

// singleM4BInspection is the InspectedFile list for one clean, coherent,
// single-file audiobook release.
func singleM4BInspection() []InspectedFile {
	return []InspectedFile{{Index: 0, Path: "The Fellowship of the Ring.m4b", Size: 500 << 20}}
}

// richAudiobookTitle is one release title carrying every marker
// ExtractReleaseFacts recognizes at once: an explicit narrator, a bracketed
// BCP 47 language tag, an ASIN, an ISBN, a runtime, and an explicit
// "Unabridged" token. releasefacts_test.go and sourceselection_test.go both
// build their "full evidence" fixtures from this single definition plus
// richAudiobookRecordingFacts, so the two files' expectations about what a
// fully-evidenced candidate looks like cannot silently drift apart.
const richAudiobookTitle = "The Fellowship of the Ring - J.R.R. Tolkien [Narrated by Rob Inglis] [en-GB] (Unabridged) ASIN B000AY7HGY ISBN 9780007171041 (10 hrs 0 mins)"

// richAudiobookRecordingFacts is the RecordingFacts ExtractReleaseFacts must
// derive from richAudiobookTitle. It intentionally matches baseTargetRecording
// (testhelpers_test.go) on every field that primaryLanguage/overlap treat as
// compatible (en-GB vs en-US share the "en" primary language), so a candidate
// built from richAudiobookTitle is a fully-matching recording against
// baseTarget() without either file needing to know the other's internals.
func richAudiobookRecordingFacts() RecordingFacts {
	return RecordingFacts{
		ASINs:          []string{"B000AY7HGY"},
		ISBNs:          []string{"9780007171041"},
		Narrators:      []string{"Rob Inglis"},
		Language:       "en-GB",
		RuntimeMinutes: 600,
		Abridged:       AbridgementUnabridged,
	}
}

// noEvidenceTitle carries the target's title+author (so Evaluate's identity
// gate passes) but no recognizable recording marker at all, so
// ExtractReleaseFacts must report an all-neutral RecordingFacts{} and the
// resulting Evidence score is exactly 0. Used to test confidence-threshold
// boundaries without depending on ExtractReleaseFacts' richer parsing rules.
const noEvidenceTitle = "The Fellowship of the Ring - J.R.R. Tolkien"
