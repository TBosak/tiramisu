package audiobookimport

import (
	"context"
	"errors"
	"testing"
)

// Requirement E2 (second half): partial discovery-source failure is safe
// (covered elsewhere by TestRunner_SeriesGap_InventoryErrorIsSafeNotFatal and
// TestRunner_Latest_ProviderErrorIsPartialFailureNotFatal, which also prove a
// successful explicit candidate survives a partial failure). This file
// covers the other half: when every enabled discovery source fails, Run must
// return a terminal, redacted error, and — per review-2 — the failure of
// each known discovery branch must still be operator-visible as a
// deterministic, correlated failed row with a canonical RunReasonProviderError,
// not silently collapsed into an empty Rows slice. A row that only proves
// "something failed" without saying *what* is not useful for an operator
// running two independent discovery paths (series-gap for one series, latest
// works overall); a row is correlated to its branch by reusing the existing
// WorkID field for a stable, non-secret source identifier — the owned
// series id for a series-gap branch failure, and the fixed literal "latest"
// for the latest-works branch, since that branch is not scoped to any one
// series or work. No new ResultRow field was needed for this.

const latestBranchRowID = "latest"

func TestRunner_TotalDiscoveryFailure_ReturnsErrorAndCorrelatedFailedRows(t *testing.T) {
	deps, provider, inventory, _, selector, _, _ := newRunnerDeps()
	inventory.series = map[string]bool{"s1": true}
	inventory.authors = map[string]bool{"au1": true}
	inventory.gapErrFor["s1"] = errors.New("prowlarr indexer failed apikey=SECRET-XYZ")
	provider.latestErr = errors.New("audiosilo token=SECRET-LATEST failed")

	result, err := Run(context.Background(), deps, RunRequest{})
	if err == nil {
		t.Fatalf("Run() error = nil, want an error: every enabled discovery source (series-gap and latest) failed and there was no explicit input")
	}
	if containsSubstring(err.Error(), "SECRET-XYZ") || containsSubstring(err.Error(), "SECRET-LATEST") {
		t.Fatalf("Run() error leaks raw provider text: %v", err)
	}

	if len(result.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2: one correlated failed row per failing discovery branch (series-gap for s1, and latest)", len(result.Rows))
	}
	seriesRow, latestRow := result.Rows[0], result.Rows[1]
	if seriesRow.WorkID != "s1" || seriesRow.Status != StatusFailed || seriesRow.Reason != RunReasonProviderError {
		t.Errorf("series branch row = %+v, want WorkID=s1 Status=Failed Reason=%v", seriesRow, RunReasonProviderError)
	}
	if len(seriesRow.Reasons) != 1 || seriesRow.Reasons[0] != DiscoveryReasonSeriesGap {
		t.Errorf("series branch Reasons = %v, want exactly [%v]", seriesRow.Reasons, DiscoveryReasonSeriesGap)
	}
	if latestRow.WorkID != latestBranchRowID || latestRow.Status != StatusFailed || latestRow.Reason != RunReasonProviderError {
		t.Errorf("latest branch row = %+v, want WorkID=%q Status=Failed Reason=%v", latestRow, latestBranchRowID, RunReasonProviderError)
	}
	if len(latestRow.Reasons) != 1 || latestRow.Reasons[0] != DiscoveryReasonLatest {
		t.Errorf("latest branch Reasons = %v, want exactly [%v]", latestRow.Reasons, DiscoveryReasonLatest)
	}
	for _, row := range result.Rows {
		if containsSubstring(string(row.Reason), "SECRET") {
			t.Errorf("row %+v leaks raw error text in Reason", row)
		}
		for _, reason := range row.Reasons {
			if containsSubstring(string(reason), "SECRET") {
				t.Errorf("row %+v leaks raw error text in Reasons", row)
			}
		}
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called, want 0: a total discovery failure must not reach selection")
	}
}

func TestRunner_TotalDiscoveryFailure_ExplicitCandidateStillSurvivesAlongsideFailedBranchRows(t *testing.T) {
	// Companion case required by E2: total failure of the series-gap/latest
	// discovery sources must not erase a successful, independently-resolved
	// explicit candidate, and the two branch failures are still reported as
	// rows even though the overall run succeeds (err == nil) because of the
	// surviving explicit candidate.
	deps, provider, inventory, _, selector, publisher, _ := newRunnerDeps()
	inventory.series = map[string]bool{"s1": true}
	inventory.authors = map[string]bool{"au1": true}
	inventory.gapErrFor["s1"] = errNonContextGapFailure
	provider.latestErr = errNonContextGapFailure
	provider.details["e1"] = sampleWorkDetail("e1", "Explicit", "a1", "Author", "rec-e1")
	work := WorkFacts{WorkID: "e1", Title: "Explicit", Authors: []string{"Author"}}
	selector.byWork["e1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"e1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a successful explicit candidate must survive even total series-gap/latest failure", err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("len(Rows) = %d, want 3 (explicit success + series-gap failure + latest failure)", len(result.Rows))
	}
	byWork := map[string]ResultRow{}
	for _, row := range result.Rows {
		byWork[row.WorkID] = row
	}
	if row, ok := byWork["e1"]; !ok || row.Status != StatusPlanned {
		t.Errorf("e1 row = %+v, want Planned", row)
	}
	if row, ok := byWork["s1"]; !ok || row.Status != StatusFailed || row.Reason != RunReasonProviderError {
		t.Errorf("s1 row = %+v, want Failed/%v", row, RunReasonProviderError)
	}
	if row, ok := byWork[latestBranchRowID]; !ok || row.Status != StatusFailed || row.Reason != RunReasonProviderError {
		t.Errorf("%s row = %+v, want Failed/%v", latestBranchRowID, row, RunReasonProviderError)
	}
}
