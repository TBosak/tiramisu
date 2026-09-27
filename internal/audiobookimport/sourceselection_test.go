package audiobookimport

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// baseLimits is a fully valid, generous SelectionLimits fixture. Individual
// tests narrow exactly the field they are exercising.
func baseLimits() SelectionLimits {
	return SelectionLimits{
		MaxQueries:             2,
		MaxResultsPerQuery:     50,
		MaxCandidatesInspected: 10,
		MaxSourceBytes:         2 << 30,
		MinSeeders:             0,
		MaxReleaseSizeBytes:    10 << 30,
		MinConfidence:          0,
	}
}

// narrowedLimits is baseLimits with explicit, caller-configured category and
// indexer narrowing, used to prove that narrowing is actually forwarded
// end-to-end (A1) rather than merely applied consistently.
func narrowedLimits() SelectionLimits {
	limits := baseLimits()
	limits.Categories = []int{3030, 3040}
	limits.IndexerIDs = []int{7, 9}
	return limits
}

// emptyFakes returns a fully wired but empty-scripted set of dependencies,
// used whenever a test asserts that none of them may be called.
func emptyFakes() (*scriptedSearcher, *keyedFetcher, *keyedInspector) {
	return &scriptedSearcher{steps: noSteps()},
		&keyedFetcher{byGUID: noFetchOutcomes()},
		&keyedInspector{byHash: noInspectOutcomes()}
}

// ============================== A1 ==============================

// TestSourceSelection_A1_QueryPlan covers A1: a fixed, bounded, deterministic
// two-query plan (audiobook-qualified first, plain fallback second) derived
// from a complete title+author, never empty, bounded by MaxQueries.
func TestSourceSelection_A1_QueryPlan(t *testing.T) {
	t.Run("A1_two_query_plan_audiobook_qualified_first_plain_fallback_second", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{{}, {}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil for a search plan that simply finds nothing", err)
		}
		if len(searcher.queries) != 2 {
			t.Fatalf("issued %d queries, want exactly 2: %+v", len(searcher.queries), searcher.queries)
		}
		first, second := searcher.queries[0].Query, searcher.queries[1].Query
		if strings.TrimSpace(first) == "" || strings.TrimSpace(second) == "" {
			t.Fatalf("empty query in plan: first=%q second=%q", first, second)
		}
		for i, q := range []string{first, second} {
			n := normalize(q)
			if !strings.Contains(n, "fellowship") || !strings.Contains(n, "tolkien") {
				t.Errorf("query %d = %q (normalized %q), want it to contain the work title and author", i, q, n)
			}
		}
		if !strings.Contains(strings.ToLower(first), "audiobook") {
			t.Errorf("first query = %q, want the audiobook-qualified query first", first)
		}
		if strings.Contains(strings.ToLower(second), "audiobook") {
			t.Errorf("second query = %q, want the plain fallback query (no audiobook qualifier) second", second)
		}
		if first == second {
			t.Errorf("both queries are identical (%q); want audiobook-qualified vs plain fallback to differ", first)
		}
	})

	t.Run("A1_plan_never_exceeds_max_queries", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{{}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		limits := baseLimits()
		limits.MaxQueries = 1
		_, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(searcher.queries) != 1 {
			t.Fatalf("issued %d queries with MaxQueries=1, want exactly 1: %+v", len(searcher.queries), searcher.queries)
		}
		if !strings.Contains(strings.ToLower(searcher.queries[0].Query), "audiobook") {
			t.Errorf("only query = %q, want the audiobook-qualified query when the plan is bounded to one", searcher.queries[0].Query)
		}
	})

	t.Run("A1_configured_categories_and_indexer_ids_are_forwarded_unchanged_on_every_query", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{{}, {}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(context.Background(), baseTarget(), narrowedLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(searcher.queries) != 2 {
			t.Fatalf("issued %d queries, want 2", len(searcher.queries))
		}
		wantCategories := []int{3030, 3040}
		wantIndexerIDs := []int{7, 9}
		for i, q := range searcher.queries {
			if !reflect.DeepEqual(q.Categories, wantCategories) {
				t.Errorf("query %d Categories = %v, want the exact configured %v", i, q.Categories, wantCategories)
			}
			if !reflect.DeepEqual(q.IndexerIDs, wantIndexerIDs) {
				t.Errorf("query %d IndexerIDs = %v, want the exact configured %v", i, q.IndexerIDs, wantIndexerIDs)
			}
		}
	})

	t.Run("A1_max_results_per_query_bounds_which_answers_are_ever_fetched", func(t *testing.T) {
		limits := baseLimits()
		limits.MaxResultsPerQuery = 2
		limits.MaxCandidatesInspected = 10 // generous: isolates MaxResultsPerQuery from the separate inspect cap

		var results []SearchResult
		for i := 0; i < 4; i++ {
			results = append(results, goodSearchResult(fmtGUID(i), richAudiobookTitle, fmtHash(i), 10-i, 500<<20))
		}
		searcher := &scriptedSearcher{steps: []searchStep{{results: results}, {}}}
		// Only the first two GUIDs have a scripted fetch outcome; a fetch call
		// for g2/g3 would be recorded (and error) if MaxResultsPerQuery were
		// not enforced.
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			fmtGUID(0): {source: FetchedSource{Hash: fmtHash(0)}},
			fmtGUID(1): {source: FetchedSource{Hash: fmtHash(1)}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			fmtHash(0): {files: singleM4BInspection()},
			fmtHash(1): {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(fetcher.calls) > 2 {
			t.Fatalf("fetch called %d times, want at most MaxResultsPerQuery=2: %+v", len(fetcher.calls), fetcher.calls)
		}
		for _, call := range fetcher.calls {
			if call.GUID != fmtGUID(0) && call.GUID != fmtGUID(1) {
				t.Errorf("fetched %+v, want only the first MaxResultsPerQuery=2 answers ever considered", call)
			}
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection from within the first MaxResultsPerQuery answers")
		}
	})

	t.Run("A1_unicode_and_reserved_query_syntax_in_title_and_author_yields_nonempty_queries", func(t *testing.T) {
		target := Target{Work: WorkFacts{
			Title:   `Bartleby, the Scrivener — a "Wall-Street" Story`,
			Authors: []string{"Herman Melville"},
		}}
		searcher := &scriptedSearcher{steps: []searchStep{{}, {}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(context.Background(), target, baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil for unicode/quoted title+author", err)
		}
		if len(searcher.queries) != 2 {
			t.Fatalf("issued %d queries, want 2", len(searcher.queries))
		}
		for i, q := range searcher.queries {
			n := normalize(q.Query)
			if strings.TrimSpace(q.Query) == "" {
				t.Errorf("query %d is empty for a title containing punctuation/quotes", i)
			}
			if !strings.Contains(n, "melville") {
				t.Errorf("query %d = %q (normalized %q), want it to still contain the author", i, q.Query, n)
			}
		}
	})
}

// ============================== A2 / E2 ==============================

// TestSourceSelection_A2_E2_PartialSearchSuccessAndCancellation covers A2/E2:
// one failed query does not erase a successful query's results; all-failed
// queries return a redacted aggregate error; and caller cancellation stops
// later queries with an errors.Is-compatible error.
func TestSourceSelection_A2_E2_PartialSearchSuccessAndCancellation(t *testing.T) {
	t.Run("A2_first_query_fails_fallback_succeeds_is_usable", func(t *testing.T) {
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{
			{err: errors.New("indexer timeout")},
			{results: []SearchResult{good}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: good.Hash}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			good.Hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil: one query failing must not erase the other's usable result", err)
		}
		if result.Selected == nil {
			t.Fatalf("SelectionResult.Selected = nil, want a selection built from the surviving query's result")
		}
	})

	t.Run("A2_fallback_fails_after_first_succeeds_is_usable", func(t *testing.T) {
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{good}},
			{err: errors.New("indexer timeout")},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: good.Hash}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			good.Hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("SelectionResult.Selected = nil, want a selection from the first query despite the fallback failing")
		}
	})

	t.Run("E2_both_queries_fail_returns_redacted_aggregate_error", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{
			{err: errors.New("indexer said: apikey=SUPERSECRET123 rejected")},
			{err: errors.New("indexer 2 said: apikey=SUPERSECRET123 rejected")},
		}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an aggregate error when every query fails")
		}
		if strings.Contains(err.Error(), "SUPERSECRET123") {
			t.Errorf("aggregate error %q leaked a secret from an underlying query error", err.Error())
		}
		if result.Selected != nil {
			t.Errorf("SelectionResult.Selected = %+v, want nil when every query failed", result.Selected)
		}
		if len(fetcher.calls) != 0 || len(inspector.calls) != 0 {
			t.Errorf("fetch/inspect were called (%d/%d) despite no usable search result", len(fetcher.calls), len(inspector.calls))
		}
	})

	t.Run("E2_context_already_cancelled_before_any_call_fails_fast", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		searcher, fetcher, inspector := emptyFakes()

		_, err := SelectSource(ctx, baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SelectSource() error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if len(searcher.queries) != 0 {
			t.Errorf("issued %d queries against an already-cancelled context, want 0", len(searcher.queries))
		}
	})

	t.Run("A2_cancellation_after_first_query_result_prevents_the_fallback_query", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: nil, onAnswered: cancel},
			{}, // must never be reached
		}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(ctx, baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SelectSource() error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if len(searcher.queries) != 1 {
			t.Fatalf("issued %d queries after mid-plan cancellation, want exactly 1 (the fallback must be skipped)", len(searcher.queries))
		}
	})

	t.Run("A2_cancellation_after_search_prevents_fetch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		limits := baseLimits()
		limits.MaxQueries = 1
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{good}, onAnswered: cancel},
		}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(ctx, baseTarget(), limits, searcher, fetcher, inspector)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SelectSource() error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if len(fetcher.calls) != 0 {
			t.Errorf("fetch was called %d times after cancellation, want 0", len(fetcher.calls))
		}
	})

	t.Run("A2_cancellation_after_fetch_prevents_inspect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		limits := baseLimits()
		limits.MaxQueries = 1
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: good.Hash}, onAnswered: cancel},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		_, err := SelectSource(ctx, baseTarget(), limits, searcher, fetcher, inspector)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SelectSource() error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if len(inspector.calls) != 0 {
			t.Errorf("inspect was called %d times after cancellation, want 0", len(inspector.calls))
		}
	})
}

// TestSourceSelection_A2_E4_DeduplicationIsStableAndAvoidsDuplicateWork covers
// A2's dedup-by-canonical-hash-then-GUID-then-URL contract and E4's
// "duplicate hashes with different URLs are inspected once".
func TestSourceSelection_A2_E4_DeduplicationIsStableAndAvoidsDuplicateWork(t *testing.T) {
	t.Run("A2_E4_duplicate_hash_different_casing_and_url_is_fetched_once", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		primary := goodSearchResult("g-primary", richAudiobookTitle, hash, 10, 500<<20)
		duplicate := goodSearchResult("g-dup", richAudiobookTitle, strings.ToLower(hash), 5, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{primary}},
			{results: []SearchResult{duplicate}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-primary": {source: FetchedSource{Hash: hash}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(fetcher.calls) != 1 {
			t.Fatalf("fetch called %d times for a duplicate-hash pair, want exactly 1: %+v", len(fetcher.calls), fetcher.calls)
		}
		if len(inspector.calls) != 1 {
			t.Fatalf("inspect called %d times for a duplicate-hash pair, want exactly 1", len(inspector.calls))
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want the deduplicated release to still be selected")
		}
	})

	t.Run("A2_duplicate_guid_when_hash_absent_is_fetched_once", func(t *testing.T) {
		r1 := SearchResult{GUID: "g1", Title: richAudiobookTitle, DownloadURL: "https://indexer.example/1", Seeders: 10, Size: 500 << 20}
		r2 := SearchResult{GUID: "g1", Title: richAudiobookTitle, DownloadURL: "https://indexer.example/2", Seeders: 3, Size: 500 << 20}
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{r1}},
			{results: []SearchResult{r2}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(fetcher.calls) != 1 {
			t.Fatalf("fetch called %d times for a duplicate-GUID pair, want exactly 1", len(fetcher.calls))
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want the deduplicated release to still be selected")
		}
	})

	t.Run("A2_E4_duplicate_url_when_hash_and_guid_absent_is_fetched_once_preserving_first_source_order", func(t *testing.T) {
		url := "https://indexer.example/only-link"
		first := SearchResult{Title: richAudiobookTitle, DownloadURL: url, Seeders: 10, Size: 500 << 20}
		second := SearchResult{Title: richAudiobookTitle, DownloadURL: url, Seeders: 3, Size: 500 << 20}
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{first}},
			{results: []SearchResult{second}},
		}}
		// Both duplicates carry an empty GUID, so they share fetcher's "" key;
		// what distinguishes them is which one is actually forwarded.
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"": {source: FetchedSource{Hash: hash}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(fetcher.calls) != 1 {
			t.Fatalf("fetch called %d times for a duplicate-URL pair with no hash/GUID, want exactly 1: %+v", len(fetcher.calls), fetcher.calls)
		}
		if fetcher.calls[0].Seeders != first.Seeders {
			t.Errorf("fetched %+v, want the first-in-source-order duplicate (Seeders=%d) preserved", fetcher.calls[0], first.Seeders)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want the deduplicated release to still be selected")
		}
	})
}

// ============================== A3 / E3 ==============================

// TestSourceSelection_A3_E3_FetchBoundary covers A3/E3: torrent bytes and hash
// are preserved exactly, a magnet preserves its hash, a fetched hash that
// contradicts the inline search hash is a hard veto, and missing/oversized/
// empty/malformed sources plus fetch failures are terminal per-release rows
// that are never inspected or selected.
func TestSourceSelection_A3_E3_FetchBoundary(t *testing.T) {
	t.Run("A3_torrent_file_result_preserves_exact_bytes_and_hash", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		torrentBytes := []byte("d8:announce...fake-bencode-payload...e")
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash, TorrentFile: torrentBytes}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection")
		}
		if !strings.EqualFold(result.Hash, hash) {
			t.Errorf("Hash = %q, want %q (case-insensitive)", result.Hash, hash)
		}
		if !reflect.DeepEqual(result.TorrentFile, torrentBytes) {
			t.Errorf("TorrentFile = %v, want the exact fetched bytes %v", result.TorrentFile, torrentBytes)
		}
	})

	t.Run("A3_magnet_result_preserves_the_exact_full_magnet_reference_without_fabricating_torrent_bytes", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Fellowship&tr=http://tracker.example/announce"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash, Magnet: magnet}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection from a magnet-only fetch")
		}
		if !strings.EqualFold(result.Hash, hash) {
			t.Errorf("Hash = %q, want %q", result.Hash, hash)
		}
		if result.Magnet != magnet {
			t.Errorf("Magnet = %q, want the exact full fetched magnet reference %q", result.Magnet, magnet)
		}
		if len(result.TorrentFile) != 0 {
			t.Errorf("TorrentFile = %v, want empty when the fetch produced only a magnet reference", result.TorrentFile)
		}
	})

	t.Run("A3_A6_I4_selected_magnet_is_confined_to_the_selected_value_and_never_leaks_into_other_rows", func(t *testing.T) {
		hashA := "1111111111111111111111111111111111111111"
		hashB := "2222222222222222222222222222222222222222"
		magnetA := "magnet:?xt=urn:btih:" + hashA + "&dn=Fellowship&tr=http://tracker.example/announce?apikey=SECRETA"
		winner := goodSearchResult("g-a", richAudiobookTitle, hashA, 10, 500<<20)
		// loser's inline hash is contradicted by its fetch, so it is rejected
		// and must never reach the selected value.
		loser := goodSearchResult("g-b", richAudiobookTitle, hashB, 5, 500<<20)

		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{winner}},
			{results: []SearchResult{loser}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-a": {source: FetchedSource{Hash: hashA, Magnet: magnetA}},
			"g-b": {source: FetchedSource{Hash: "0000000000000000000000000000000000000000"}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hashA: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want the surviving release to be selected")
		}
		if result.Magnet != magnetA {
			t.Errorf("Magnet = %q, want the exact winning magnet %q", result.Magnet, magnetA)
		}
		row := requireRejection(t, result, loser.Title, reasonHashMismatch)
		if strings.Contains(string(row.Reason), "SECRETA") || strings.Contains(string(row.Reason), magnetA) {
			t.Errorf("Reason %q leaked the winning release's magnet reference", row.Reason)
		}
	})

	t.Run("A3_fetched_hash_contradicting_inline_hash_is_a_hard_veto", func(t *testing.T) {
		inline := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		fetched := "0000000000000000000000000000000000000000"
		good := goodSearchResult("g1", richAudiobookTitle, inline, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: fetched, TorrentFile: []byte("bytes")}},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil (a veto is a rejection row, not a fatal error)", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil: a fetched hash contradicting the inline hash must be vetoed", result.Selected)
		}
		if len(inspector.calls) != 0 {
			t.Errorf("inspect was called %d times for a hash-mismatched release, want 0", len(inspector.calls))
		}
		requireRejection(t, result, good.Title, reasonHashMismatch)
	})

	t.Run("A3_missing_hash_and_reference_is_rejected_without_inspection", func(t *testing.T) {
		good := goodSearchResult("g1", richAudiobookTitle, "", 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{}},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a source with no usable hash or reference", result.Selected)
		}
		if len(inspector.calls) != 0 {
			t.Errorf("inspect was called %d times for a sourceless release, want 0", len(inspector.calls))
		}
		requireRejection(t, result, good.Title, reasonMissingSource)
	})

	t.Run("A3_empty_torrent_bytes_with_no_magnet_is_rejected_as_malformed_or_empty", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash, TorrentFile: []byte{}}},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a fetch that produced zero torrent bytes and no magnet", result.Selected)
		}
		if len(inspector.calls) != 0 {
			t.Errorf("inspect was called %d times for an empty source, want 0", len(inspector.calls))
		}
		requireRejectionAnyOf(t, result, good.Title, reasonEmptySource, reasonMalformedSource, reasonMissingSource)
	})

	t.Run("A3_oversized_torrent_bytes_is_rejected_without_inspection", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		limits := baseLimits()
		limits.MaxSourceBytes = 10
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash, TorrentFile: make([]byte, 11)}},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a source larger than MaxSourceBytes", result.Selected)
		}
		if len(inspector.calls) != 0 {
			t.Errorf("inspect was called %d times for an oversized source, want 0", len(inspector.calls))
		}
		requireRejection(t, result, good.Title, reasonOversizedSource)
	})

	t.Run("A3_malformed_source_with_both_torrent_bytes_and_magnet_is_rejected", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash, TorrentFile: []byte("bytes"), Magnet: "magnet:?xt=urn:btih:" + hash}},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for an ambiguous fetched source carrying both bytes and a magnet", result.Selected)
		}
		requireRejection(t, result, good.Title, reasonMalformedSource)
	})

	t.Run("E3_fetch_failure_is_a_terminal_per_release_row_with_no_secret_leak", func(t *testing.T) {
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {err: errors.New("upstream said: apikey=SUPERSECRET123")},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil (a per-release fetch failure is a rejection row)", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil", result.Selected)
		}
		row := requireRejection(t, result, good.Title, reasonFetchFailed)
		if strings.Contains(string(row.Reason), "SUPERSECRET123") {
			t.Errorf("Reason %q leaked a secret from the fetch error", row.Reason)
		}
	})
}

// TestSourceSelection_I4_RawURLRedaction covers I4 directly: a distinctive,
// realistic raw download URL embedded in an underlying query or fetch error
// must never surface in any error or explanation value SelectSource returns
// - not the aggregate query error, and not a per-release rejection reason.
// This is deliberately independent of the magnet-preservation tests (A3/A6),
// which prove the opposite: that a winning release's own reference IS
// preserved, just confined to the selected machine value.
func TestSourceSelection_I4_RawURLRedaction(t *testing.T) {
	const distinctiveURL = "https://private-tracker.example/dl/abc123?passkey=SUPERSECRETPASSKEY000111222"

	t.Run("I4_aggregate_query_error_never_leaks_a_raw_download_url", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{
			{err: errors.New("indexer request to " + distinctiveURL + " failed: connection reset")},
			{err: errors.New("indexer request to " + distinctiveURL + " failed: timeout")},
		}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an aggregate error when every query fails")
		}
		if strings.Contains(err.Error(), distinctiveURL) {
			t.Errorf("aggregate error %q leaked the raw download URL %q from an underlying query error", err.Error(), distinctiveURL)
		}
		for _, row := range result.Rejected {
			if strings.Contains(string(row.Reason), distinctiveURL) {
				t.Errorf("Rejected row %+v leaked the raw download URL %q", row, distinctiveURL)
			}
		}
	})

	t.Run("I4_fetch_failure_row_never_leaks_a_raw_download_url", func(t *testing.T) {
		good := goodSearchResult("g1", richAudiobookTitle, "AABBCCDDEE00112233445566778899AABBCCDDEE", 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {err: errors.New("fetch of " + distinctiveURL + " returned HTTP 403")},
		}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil (a per-release fetch failure is a rejection row)", err)
		}
		row := requireRejection(t, result, good.Title, reasonFetchFailed)
		if strings.Contains(string(row.Reason), distinctiveURL) {
			t.Errorf("Rejected row %+v leaked the raw download URL %q from the fetch error", row, distinctiveURL)
		}
	})
}

// ============================== A4 ==============================

// TestSourceSelection_A4_Inspection covers A4: inspected files become exact
// AudioFile facts, the existing coherent-file policy still governs
// eligibility, inspection failure is isolated to its own release, and an
// all-failed run reports no match without any write.
func TestSourceSelection_A4_Inspection(t *testing.T) {
	t.Run("A4_inspected_path_index_and_size_become_audiofile_facts", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		files := []InspectedFile{{Index: 3, Path: "Fellowship/Fellowship.m4b", Size: 500 << 20}}
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: files}}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection")
		}
		want := []AudioFile{{Index: 3, Path: "Fellowship/Fellowship.m4b", Size: 500 << 20}}
		if !reflect.DeepEqual(result.Files, want) {
			t.Errorf("Files = %+v, want the inspected file facts exactly %+v", result.Files, want)
		}
	})

	t.Run("A4_unsafe_inspected_path_is_rejected_by_the_existing_coherent_file_policy", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: []InspectedFile{{Index: 0, Path: "../../etc/passwd.m4b", Size: 500 << 20}}},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a path-traversal source path", result.Selected)
		}
		requireRejection(t, result, good.Title, reasonInvalidFiles)
	})

	t.Run("A4_gap_in_multipart_numbering_is_rejected_by_the_existing_coherent_file_policy", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash: {files: []InspectedFile{
				{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
				{Index: 1, Path: "Book - Part 02.mp3", Size: 20 << 20},
				{Index: 2, Path: "Book - Part 04.mp3", Size: 20 << 20},
			}},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a gapped multipart set", result.Selected)
		}
		requireRejection(t, result, good.Title, reasonInvalidFiles)
	})

	t.Run("A4_inspection_failure_is_isolated_to_its_own_release", func(t *testing.T) {
		hash1 := "1111111111111111111111111111111111111111"
		hash2 := "2222222222222222222222222222222222222222"
		bad := goodSearchResult("g-bad", richAudiobookTitle, hash1, 10, 500<<20)
		okRelease := goodSearchResult("g-ok", richAudiobookTitle, hash2, 5, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{bad}},
			{results: []SearchResult{okRelease}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-bad": {source: FetchedSource{Hash: hash1}},
			"g-ok":  {source: FetchedSource{Hash: hash2}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash1: {err: errors.New("torrent has no readable pieces")},
			hash2: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want the surviving release to be selected despite the other's inspection failure")
		}
		if !strings.EqualFold(result.Hash, hash2) {
			t.Errorf("Hash = %q, want the surviving release's hash %q", result.Hash, hash2)
		}
		requireRejection(t, result, bad.Title, reasonInspectionFailed)
	})

	t.Run("A4_all_releases_failing_inspection_reports_no_match_without_a_write", func(t *testing.T) {
		hash1 := "1111111111111111111111111111111111111111"
		hash2 := "2222222222222222222222222222222222222222"
		r1 := goodSearchResult("g1", richAudiobookTitle, hash1, 10, 500<<20)
		r2 := goodSearchResult("g2", richAudiobookTitle, hash2, 5, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{r1}},
			{results: []SearchResult{r2}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g1": {source: FetchedSource{Hash: hash1}},
			"g2": {source: FetchedSource{Hash: hash2}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hash1: {err: errors.New("inspection failed")},
			hash2: {err: errors.New("inspection failed")},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil: no eligible release is not a fatal error", err)
		}
		if result.Selected != nil {
			t.Errorf("Selected = %+v, want nil", result.Selected)
		}
		if result.Hash != "" || len(result.TorrentFile) != 0 || len(result.Files) != 0 {
			t.Errorf("result = %+v, want every machine-value field empty when nothing was selected", result)
		}
		if len(result.Rejected) != 2 {
			t.Errorf("Rejected has %d rows, want 2 (one per failed release)", len(result.Rejected))
		}
	})
}

// ============================== A6 / E5 ==============================

// TestSourceSelection_A6_E5_ConfidenceGatedRanking covers A6/E5: a
// below-threshold candidate yields no selection, a candidate meeting the
// threshold is selected with its full machine value populated, and every
// gate (identity, vetoes, coherent files, confidence) must pass together.
func TestSourceSelection_A6_E5_ConfidenceGatedRanking(t *testing.T) {
	t.Run("A6_below_threshold_candidate_yields_no_selection", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		// noEvidenceTitle matches identity but contributes zero recording
		// evidence, so its confidence score is 0.
		weak := goodSearchResult("g1", noEvidenceTitle, hash, 10, 500<<20)
		limits := baseLimits()
		limits.MinConfidence = 1
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{weak}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil: zero-evidence candidate must not clear MinConfidence=1", result.Selected)
		}
		requireRejection(t, result, weak.Title, reasonBelowConfidence)
	})

	t.Run("A6_candidate_meeting_threshold_is_selected_with_full_machine_value", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		torrentBytes := []byte("fake-bencode")
		strong := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		limits := baseLimits()
		limits.MinConfidence = 6 // every Evidence field matches baseTarget()
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{strong}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash, TorrentFile: torrentBytes}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil || !result.Selected.Eligible {
			t.Fatalf("Selected = %+v, want a non-nil eligible decision at MinConfidence=6 for a fully-matching recording", result.Selected)
		}
		if !strings.EqualFold(result.Hash, hash) {
			t.Errorf("Hash = %q, want %q", result.Hash, hash)
		}
		if !reflect.DeepEqual(result.TorrentFile, torrentBytes) {
			t.Errorf("TorrentFile = %v, want %v", result.TorrentFile, torrentBytes)
		}
		if len(result.Files) == 0 {
			t.Errorf("Files is empty, want the inspected audio file facts")
		}
	})

	t.Run("E5_title_and_author_mismatch_yields_no_selection_regardless_of_confidence", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		mismatched := goodSearchResult("g1", "An Entirely Unrelated Cookbook by Someone Else", hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{mismatched}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a title/author identity mismatch", result.Selected)
		}
		requireRejection(t, result, mismatched.Title, reasonWorkMismatch)
	})

	t.Run("E5_explicit_veto_yields_no_selection_even_with_coherent_files_and_high_confidence_floor_disabled", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		vetoTitle := "The Fellowship of the Ring - J.R.R. Tolkien [Narrated by Someone Else Entirely] (Abridged)"
		veto := goodSearchResult("g1", vetoTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{veto}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		// baseTarget() pins narrator "Rob Inglis" and Abridged=Unabridged;
		// veto's title contradicts both explicitly.
		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil: an explicit narrator/abridgement contradiction must veto regardless of MinConfidence=0", result.Selected)
		}
		requireRejection(t, result, veto.Title, reasonRecordingMismatch)
	})
}

// TestSourceSelection_A6_DeterministicRankingAndTieOrder covers A6's
// requirement that eligible, above-threshold candidates rank deterministically
// with stable source-order tie breaking, cross-referencing I3's ordering
// stability.
func TestSourceSelection_A6_I3_DeterministicRankingAndTieOrder(t *testing.T) {
	hashA := "1111111111111111111111111111111111111111"
	hashB := "2222222222222222222222222222222222222222"
	a := goodSearchResult("g-a", richAudiobookTitle, hashA, 10, 500<<20)
	b := goodSearchResult("g-b", richAudiobookTitle, hashB, 10, 500<<20)

	build := func() (*scriptedSearcher, *keyedFetcher, *keyedInspector) {
		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{a}},
			{results: []SearchResult{b}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-a": {source: FetchedSource{Hash: hashA}},
			"g-b": {source: FetchedSource{Hash: hashB}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hashA: {files: singleM4BInspection()},
			hashB: {files: singleM4BInspection()},
		}}
		return searcher, fetcher, inspector
	}

	var first SelectionResult
	for i := 0; i < 5; i++ {
		searcher, fetcher, inspector := build()
		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("run %d: SelectSource() error = %v, want nil", i, err)
		}
		if result.Selected == nil {
			t.Fatalf("run %d: Selected = nil, want a tied-evidence candidate to still be selected", i)
		}
		if i == 0 {
			first = result
			if !strings.EqualFold(first.Hash, hashA) {
				t.Errorf("run 0: Hash = %q, want the first-in-source-order tie winner %q", first.Hash, hashA)
			}
			continue
		}
		if !reflect.DeepEqual(result, first) {
			t.Fatalf("run %d = %+v, want identical result to run 0 = %+v (I3 determinism)", i, result, first)
		}
	}
}

// TestSourceSelection_A6_I3_RankingUsesExistingPolicyNotFirstEligible covers
// the review's A6/I3 gap: SelectSource must rank eligible candidates by the
// existing evidence/container/seeder policy (Rank), not by picking whichever
// eligible candidate happened to be seen first or has the most seeders.
func TestSourceSelection_A6_I3_RankingUsesExistingPolicyNotFirstEligible(t *testing.T) {
	t.Run("A6_higher_evidence_wins_over_a_first_seen_high_seeder_zero_evidence_candidate", func(t *testing.T) {
		hashWeak := "1111111111111111111111111111111111111111"
		hashStrong := "2222222222222222222222222222222222222222"
		weakBytes := []byte("weak-release-bytes")
		strongBytes := []byte("strong-release-bytes")
		// weakFirst is seen first and has vastly more seeders, but
		// contributes zero recording evidence.
		weakFirst := goodSearchResult("g-weak", noEvidenceTitle, hashWeak, 9999, 500<<20)
		// strongSecond is seen second, has only 1 seeder, but fully matches
		// baseTarget()'s recording facts.
		strongSecond := goodSearchResult("g-strong", richAudiobookTitle, hashStrong, 1, 500<<20)
		weakFiles := []InspectedFile{{Index: 0, Path: "weak.m4b", Size: 500 << 20}}
		strongFiles := []InspectedFile{{Index: 0, Path: "strong.m4b", Size: 500 << 20}}

		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{weakFirst}},
			{results: []SearchResult{strongSecond}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-weak":   {source: FetchedSource{Hash: hashWeak, TorrentFile: weakBytes}},
			"g-strong": {source: FetchedSource{Hash: hashStrong, TorrentFile: strongBytes}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hashWeak:   {files: weakFiles},
			hashStrong: {files: strongFiles},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection")
		}
		if !strings.EqualFold(result.Hash, hashStrong) {
			t.Fatalf("Hash = %q, want the higher-evidence candidate %q despite being second in source order with far fewer seeders", result.Hash, hashStrong)
		}
		if !reflect.DeepEqual(result.TorrentFile, strongBytes) {
			t.Errorf("TorrentFile = %v, want the winning candidate's own bytes %v, never the loser's", result.TorrentFile, strongBytes)
		}
		wantFiles := []AudioFile{{Index: 0, Path: "strong.m4b", Size: 500 << 20}}
		if !reflect.DeepEqual(result.Files, wantFiles) {
			t.Errorf("Files = %+v, want the winning candidate's own files %+v, never the loser's", result.Files, wantFiles)
		}
		// E4/A6: the Decision itself, not just the machine-value fields
		// computed alongside it, must belong to the same winning release.
		if result.Selected.Candidate.Title != strongSecond.Title {
			t.Errorf("Selected.Candidate.Title = %q, want the winning candidate's own title %q, never the loser's",
				result.Selected.Candidate.Title, strongSecond.Title)
		}
		if result.Selected.Candidate.Seeders != strongSecond.Seeders {
			t.Errorf("Selected.Candidate.Seeders = %d, want the winning candidate's own seeder count %d, never the loser's",
				result.Selected.Candidate.Seeders, strongSecond.Seeders)
		}
		if !reflect.DeepEqual(result.Selected.Candidate.Files, wantFiles) {
			t.Errorf("Selected.Candidate.Files = %+v, want the winning candidate's own files %+v, never the loser's",
				result.Selected.Candidate.Files, wantFiles)
		}
	})

	t.Run("A6_preferred_container_wins_over_a_first_seen_equal_evidence_candidate", func(t *testing.T) {
		hashMP3 := "3333333333333333333333333333333333333333"
		hashM4B := "4444444444444444444444444444444444444444"
		// Both candidates carry identical, fully-matching recording evidence
		// (richAudiobookTitle), so only container preference can break the tie.
		mp3First := goodSearchResult("g-mp3", richAudiobookTitle, hashMP3, 10, 40<<20)
		m4bSecond := goodSearchResult("g-m4b", richAudiobookTitle, hashM4B, 10, 40<<20)
		mp3Files := []InspectedFile{
			{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "Book - Part 02.mp3", Size: 20 << 20},
		}
		m4bFiles := []InspectedFile{{Index: 0, Path: "Book.m4b", Size: 40 << 20}}

		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{mp3First}},
			{results: []SearchResult{m4bSecond}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-mp3": {source: FetchedSource{Hash: hashMP3}},
			"g-m4b": {source: FetchedSource{Hash: hashM4B}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hashMP3: {files: mp3Files},
			hashM4B: {files: m4bFiles},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection")
		}
		if !strings.EqualFold(result.Hash, hashM4B) {
			t.Errorf("Hash = %q, want the preferred .m4b container %q despite being second in source order with equal evidence and seeders", result.Hash, hashM4B)
		}
	})

	t.Run("A6_higher_seeders_wins_a_first_seen_equal_evidence_and_container_tie", func(t *testing.T) {
		hashLow := "5555555555555555555555555555555555555555"
		hashHigh := "6666666666666666666666666666666666666666"
		// Both candidates carry identical, fully-matching recording evidence
		// (richAudiobookTitle) and the same .m4b container, so only seeder
		// count can break the tie. Source-order tie behavior (equal seeders)
		// is already covered separately by
		// TestSourceSelection_A6_I3_DeterministicRankingAndTieOrder.
		lowFirst := goodSearchResult("g-low", richAudiobookTitle, hashLow, 1, 500<<20)
		highSecond := goodSearchResult("g-high", richAudiobookTitle, hashHigh, 500, 500<<20)

		searcher := &scriptedSearcher{steps: []searchStep{
			{results: []SearchResult{lowFirst}},
			{results: []SearchResult{highSecond}},
		}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{
			"g-low":  {source: FetchedSource{Hash: hashLow}},
			"g-high": {source: FetchedSource{Hash: hashHigh}},
		}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{
			hashLow:  {files: singleM4BInspection()},
			hashHigh: {files: singleM4BInspection()},
		}}

		result, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a selection")
		}
		if !strings.EqualFold(result.Hash, hashHigh) {
			t.Errorf("Hash = %q, want the higher-seeder candidate %q despite being second in source order with equal evidence and container",
				result.Hash, hashHigh)
		}
	})
}

// ============================== I1 ==============================

// TestSourceSelection_I1_ContextPropagationAndNoRetry covers I1: search, fetch,
// and inspect all receive the caller's own context, and a query/fetch/
// inspect error is never retried internally.
func TestSourceSelection_I1_ContextPropagationAndNoRetry(t *testing.T) {
	t.Run("I1_dependencies_receive_the_callers_context", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		ctx := withMarker(context.Background(), "marker-123")
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		if _, err := SelectSource(ctx, baseTarget(), baseLimits(), searcher, fetcher, inspector); err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		for i, m := range searcher.markers {
			if m != "marker-123" {
				t.Errorf("Search call %d received a context without the caller's marker (got %q)", i, m)
			}
		}
		for i, m := range fetcher.markers {
			if m != "marker-123" {
				t.Errorf("Fetch call %d received a context without the caller's marker (got %q)", i, m)
			}
		}
		for i, m := range inspector.markers {
			if m != "marker-123" {
				t.Errorf("Inspect call %d received a context without the caller's marker (got %q)", i, m)
			}
		}
		if len(searcher.markers) == 0 || len(fetcher.markers) == 0 || len(inspector.markers) == 0 {
			t.Fatalf("one or more dependencies were never called; cannot prove context propagation")
		}
	})

	t.Run("I1_a_failed_query_is_never_retried", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: []searchStep{
			{err: errors.New("timeout")},
			{results: nil},
		}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		if _, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector); err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(searcher.queries) != 2 {
			t.Fatalf("issued %d total queries, want exactly 2 (one per plan slot, no retry of the failed one)", len(searcher.queries))
		}
	})

	t.Run("I1_a_failed_fetch_is_never_retried", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {err: errors.New("fetch failed")}}}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		if _, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, inspector); err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if len(fetcher.calls) != 1 {
			t.Fatalf("fetch called %d times for one candidate, want exactly 1 (no retry)", len(fetcher.calls))
		}
	})
}

// ============================== I2 / E1 ==============================

// TestSourceSelection_I2_E1_LimitsValidatedBeforeAnyCall covers I2/E1: invalid
// or impossible bounds (and empty/invalid target identity, and nil
// dependencies) fail before any dependency is called, and extreme-but-valid
// bounds do not misbehave.
func TestSourceSelection_I2_E1_LimitsValidatedBeforeAnyCall(t *testing.T) {
	invalidLimits := []struct {
		name   string
		mutate func(*SelectionLimits)
	}{
		{"E1_I2_zero_max_queries", func(l *SelectionLimits) { l.MaxQueries = 0 }},
		{"E1_I2_negative_max_queries", func(l *SelectionLimits) { l.MaxQueries = -1 }},
		{"E1_I2_zero_max_results_per_query", func(l *SelectionLimits) { l.MaxResultsPerQuery = 0 }},
		{"E1_I2_zero_max_candidates_inspected", func(l *SelectionLimits) { l.MaxCandidatesInspected = 0 }},
		{"E1_I2_zero_max_source_bytes", func(l *SelectionLimits) { l.MaxSourceBytes = 0 }},
		{"E1_I2_zero_max_release_size_bytes", func(l *SelectionLimits) { l.MaxReleaseSizeBytes = 0 }},
		{"E1_I2_negative_min_seeders", func(l *SelectionLimits) { l.MinSeeders = -1 }},
		{"E1_I2_negative_min_confidence", func(l *SelectionLimits) { l.MinConfidence = -1 }},
		{"E1_impossible_confidence_threshold_above_max_possible_evidence", func(l *SelectionLimits) { l.MinConfidence = 7 }},
		// I2 requires bounds to be finite and bounded, not merely positive:
		// a pathologically large limit must be rejected up front rather than
		// risking overflow in whatever arithmetic later uses it (e.g. a byte
		// budget accumulator, or an inspected-candidate counter).
		{"I2_max_queries_at_int_overflow_boundary_is_rejected", func(l *SelectionLimits) { l.MaxQueries = math.MaxInt }},
		{"I2_max_results_per_query_at_int_overflow_boundary_is_rejected", func(l *SelectionLimits) { l.MaxResultsPerQuery = math.MaxInt }},
		{"I2_max_candidates_inspected_at_int_overflow_boundary_is_rejected", func(l *SelectionLimits) { l.MaxCandidatesInspected = math.MaxInt }},
		{"I2_max_source_bytes_at_int64_overflow_boundary_is_rejected", func(l *SelectionLimits) { l.MaxSourceBytes = math.MaxInt64 }},
		{"I2_max_release_size_bytes_at_int64_overflow_boundary_is_rejected", func(l *SelectionLimits) { l.MaxReleaseSizeBytes = math.MaxInt64 }},
	}
	for _, tc := range invalidLimits {
		t.Run(tc.name, func(t *testing.T) {
			limits := baseLimits()
			tc.mutate(&limits)
			searcher, fetcher, inspector := emptyFakes()

			_, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
			if err == nil {
				t.Fatalf("SelectSource() error = nil, want an error for invalid limits %+v", limits)
			}
			if len(searcher.queries) != 0 || len(fetcher.calls) != 0 || len(inspector.calls) != 0 {
				t.Errorf("dependencies were called (search=%d fetch=%d inspect=%d) despite invalid limits, want none",
					len(searcher.queries), len(fetcher.calls), len(inspector.calls))
			}
		})
	}

	t.Run("E1_empty_target_title_fails_before_any_call", func(t *testing.T) {
		target := baseTarget()
		target.Work.Title = ""
		searcher, fetcher, inspector := emptyFakes()

		_, err := SelectSource(context.Background(), target, baseLimits(), searcher, fetcher, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an error for an empty target title")
		}
		if len(searcher.queries) != 0 {
			t.Errorf("issued %d queries for an invalid target, want 0", len(searcher.queries))
		}
	})

	t.Run("E1_no_target_authors_fails_before_any_call", func(t *testing.T) {
		target := baseTarget()
		target.Work.Authors = nil
		searcher, fetcher, inspector := emptyFakes()

		_, err := SelectSource(context.Background(), target, baseLimits(), searcher, fetcher, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an error for a target with no authors")
		}
		if len(searcher.queries) != 0 {
			t.Errorf("issued %d queries for an invalid target, want 0", len(searcher.queries))
		}
	})

	t.Run("E1_nil_searcher_fails_without_panic_or_other_calls", func(t *testing.T) {
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}
		_, err := SelectSource(context.Background(), baseTarget(), baseLimits(), nil, fetcher, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an error for a nil searcher")
		}
		if len(fetcher.calls) != 0 || len(inspector.calls) != 0 {
			t.Errorf("fetch/inspect were called despite a nil searcher")
		}
	})

	t.Run("E1_nil_fetcher_fails_without_panic_or_other_calls", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: noSteps()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}
		_, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, nil, inspector)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an error for a nil fetcher")
		}
		if len(searcher.queries) != 0 || len(inspector.calls) != 0 {
			t.Errorf("search/inspect were called despite a nil fetcher")
		}
	})

	t.Run("E1_nil_inspector_fails_without_panic_or_other_calls", func(t *testing.T) {
		searcher := &scriptedSearcher{steps: noSteps()}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		_, err := SelectSource(context.Background(), baseTarget(), baseLimits(), searcher, fetcher, nil)
		if err == nil {
			t.Fatalf("SelectSource() error = nil, want an error for a nil inspector")
		}
		if len(searcher.queries) != 0 || len(fetcher.calls) != 0 {
			t.Errorf("search/fetch were called despite a nil inspector")
		}
	})

	t.Run("I2_generously_large_but_bounded_limits_do_not_panic_or_misbehave", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		good := goodSearchResult("g1", richAudiobookTitle, hash, 10, 500<<20)
		limits := baseLimits()
		limits.MaxCandidatesInspected = 1000
		limits.MaxSourceBytes = 1 << 30
		limits.MaxReleaseSizeBytes = 100 << 30
		limits.MaxResultsPerQuery = 1000
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{good}}, {}}}
		fetcher := &keyedFetcher{byGUID: map[string]fetchOutcome{"g1": {source: FetchedSource{Hash: hash}}}}
		inspector := &keyedInspector{byHash: map[string]inspectOutcome{hash: {files: singleM4BInspection()}}}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil for generously large but still-bounded limits", err)
		}
		if result.Selected == nil {
			t.Fatalf("Selected = nil, want a normal selection under generously large but bounded limits")
		}
	})
}

// ============================== E3 (seeder/size floor) ==============================

// TestSourceSelection_E3_SeederAndSizeFloors covers E3: a release below the
// configured seeder floor or above the configured size ceiling is a
// terminal per-release rejection, applied before the expensive fetch/inspect
// calls.
func TestSourceSelection_E3_SeederAndSizeFloors(t *testing.T) {
	t.Run("E3_below_min_seeders_is_rejected_without_fetch_or_inspect", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		low := goodSearchResult("g1", richAudiobookTitle, hash, 1, 500<<20)
		limits := baseLimits()
		limits.MinSeeders = 5
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{low}}, {}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a release below MinSeeders", result.Selected)
		}
		if len(fetcher.calls) != 0 {
			t.Errorf("fetch was called %d times for a release below the seeder floor, want 0", len(fetcher.calls))
		}
		requireRejection(t, result, low.Title, reasonBelowSeederFloor)
	})

	t.Run("E3_above_max_release_size_is_rejected_without_fetch_or_inspect", func(t *testing.T) {
		hash := "AABBCCDDEE00112233445566778899AABBCCDDEE"
		huge := goodSearchResult("g1", richAudiobookTitle, hash, 10, 100<<30)
		limits := baseLimits()
		limits.MaxReleaseSizeBytes = 10 << 30
		searcher := &scriptedSearcher{steps: []searchStep{{results: []SearchResult{huge}}, {}}}
		fetcher := &keyedFetcher{byGUID: noFetchOutcomes()}
		inspector := &keyedInspector{byHash: noInspectOutcomes()}

		result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
		if err != nil {
			t.Fatalf("SelectSource() error = %v, want nil", err)
		}
		if result.Selected != nil {
			t.Fatalf("Selected = %+v, want nil for a release above MaxReleaseSizeBytes", result.Selected)
		}
		if len(fetcher.calls) != 0 {
			t.Errorf("fetch was called %d times for an oversized release, want 0", len(fetcher.calls))
		}
		requireRejection(t, result, huge.Title, reasonOverSizeFloor)
	})
}

// ============================== A1/A4 candidate cap ==============================

// TestSourceSelection_A1_A4_InspectionCapIsNeverExceeded covers the brief's
// "12+ search answers with a lower inspect cap; rejected answers do not
// exceed it" adversarial case.
func TestSourceSelection_A1_A4_InspectionCapIsNeverExceeded(t *testing.T) {
	const total = 14
	const cap = 3

	var results []SearchResult
	fetchOutcomes := map[string]fetchOutcome{}
	inspectOutcomes := map[string]inspectOutcome{}
	for i := 0; i < total; i++ {
		guid := fmtGUID(i)
		hash := fmtHash(i)
		results = append(results, goodSearchResult(guid, richAudiobookTitle, hash, total-i, 500<<20))
		fetchOutcomes[guid] = fetchOutcome{source: FetchedSource{Hash: hash}}
		inspectOutcomes[hash] = inspectOutcome{files: singleM4BInspection()}
	}

	limits := baseLimits()
	limits.MaxCandidatesInspected = cap
	searcher := &scriptedSearcher{steps: []searchStep{{results: results}, {}}}
	fetcher := &keyedFetcher{byGUID: fetchOutcomes}
	inspector := &keyedInspector{byHash: inspectOutcomes}

	result, err := SelectSource(context.Background(), baseTarget(), limits, searcher, fetcher, inspector)
	if err != nil {
		t.Fatalf("SelectSource() error = %v, want nil", err)
	}
	if len(inspector.calls) > cap {
		t.Fatalf("inspect called %d times, want at most MaxCandidatesInspected=%d", len(inspector.calls), cap)
	}
	if len(fetcher.calls) > cap {
		t.Fatalf("fetch called %d times, want at most MaxCandidatesInspected=%d", len(fetcher.calls), cap)
	}
	if result.Selected == nil {
		t.Fatalf("Selected = nil, want a selection from within the inspected cap")
	}
}

func fmtGUID(i int) string { return "g" + itoa(i) }
func fmtHash(i int) string {
	digit := itoa(i % 10)
	return strings.Repeat(digit, 40)
}
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := "0123456789"
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

// ============================== helpers ==============================

// requireRejection asserts result has exactly one Rejected row for title
// with the given reason, and returns that row.
func requireRejection(t *testing.T, result SelectionResult, title string, want Reason) RejectedRelease {
	t.Helper()
	for _, row := range result.Rejected {
		if row.Title == title {
			if row.Reason != want {
				t.Errorf("Rejected row for %q has Reason = %q, want %q", title, row.Reason, want)
			}
			return row
		}
	}
	t.Fatalf("no Rejected row found for title %q (rows: %+v), want Reason %q", title, result.Rejected, want)
	return RejectedRelease{}
}

// requireRejectionAnyOf is requireRejection for a requirement whose brief
// wording groups several closely-related terminal reasons together (e.g.
// "malformed/empty" fetch outcomes) without mandating exactly one code.
func requireRejectionAnyOf(t *testing.T, result SelectionResult, title string, want ...Reason) {
	t.Helper()
	for _, row := range result.Rejected {
		if row.Title == title {
			for _, w := range want {
				if row.Reason == w {
					return
				}
			}
			t.Errorf("Rejected row for %q has Reason = %q, want one of %v", title, row.Reason, want)
			return
		}
	}
	t.Fatalf("no Rejected row found for title %q (rows: %+v), want one of %v", title, result.Rejected, want)
}
