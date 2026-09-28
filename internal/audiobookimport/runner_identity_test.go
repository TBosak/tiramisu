package audiobookimport

import (
	"context"
	"testing"
)

// Requirement A4: existing Audiobookshelf identities and committed Tiramisu
// controller identities are excluded before any Prowlarr/source selection.
// Recording identity wins over work identity where known; ASIN/ISBN/title
// alone never become controller identity. Exclusions and identity ambiguity
// produce safe explanations.
//
// Requirement E4: duplicate/owned/committed candidates never reach source
// selection.

func TestRunner_ExistingAudiobookshelfIdentity_ExcludesBeforeSelection(t *testing.T) {
	deps, provider, _, identities, selector, _, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	identities.existing[ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-1"}] = true

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 0 {
		t.Fatalf("selector called %d times, want 0: an existing Audiobookshelf identity must exclude before selection", len(selector.callList()))
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusExisting {
		t.Fatalf("Rows = %+v, want one row with Status = Existing", result.Rows)
	}
	if result.Rows[0].Reason != RunReasonExistingIdentity {
		t.Errorf("Reason = %v, want %v", result.Rows[0].Reason, RunReasonExistingIdentity)
	}
}

func TestRunner_CommittedIdentity_ExcludesBeforeSelection(t *testing.T) {
	deps, provider, _, identities, selector, _, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	identities.committed[ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-1"}] = true

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 0 {
		t.Fatalf("selector called, want 0: a committed identity must exclude before selection")
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusExisting || result.Rows[0].Reason != RunReasonCommittedIdentity {
		t.Fatalf("Rows = %+v, want one Existing row with reason %v", result.Rows, RunReasonCommittedIdentity)
	}
}

func TestRunner_RecordingIdentityWinsOverStaleWorkIdentity(t *testing.T) {
	deps, provider, _, identities, selector, publisher, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	// Only the work-level identity is (incorrectly/stale) marked committed;
	// the known recording identity is not. The known recording identity must
	// be what is actually checked, so this candidate proceeds.
	identities.committed[ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: "w1"}] = true
	work := WorkFacts{WorkID: "w1", Title: "Book", Authors: []string{"Author"}}
	selector.byWork["w1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 1 {
		t.Fatalf("selector called %d times, want 1: the known recording identity should be checked, not the stale work identity", len(selector.callList()))
	}
	if len(result.Rows) != 1 || result.Rows[0].Status == StatusExisting {
		t.Fatalf("Rows = %+v, want the candidate to proceed since its recording identity is not committed", result.Rows)
	}
}

func TestRunner_ASINAlone_NeverExcludesACandidate(t *testing.T) {
	deps, provider, _, identities, selector, publisher, _ := newRunnerDeps()
	detail := sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	detail.Recordings[0].ASINs = []AudioSiloASIN{{Region: "us", ASIN: "B000AY7HGY"}}
	provider.details["w1"] = detail
	// An identity keyed on a raw ASIN string can never be produced by
	// ResolveExternalIdentity, so seeding one here proves the runner does not
	// invent its own ASIN-based matching path.
	identities.committed[ExternalIdentity{Namespace: "asin", ID: "B000AY7HGY"}] = true
	work := WorkFacts{WorkID: "w1", Title: "Book", Authors: []string{"Author"}}
	selector.byWork["w1"] = func(ctx context.Context, target Target, limits SelectionLimits) (SelectionResult, error) {
		return sampleEligibleSelection(work), nil
	}
	configurePublishSuccess(publisher, work)

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 1 || len(result.Rows) != 1 || result.Rows[0].Status == StatusExisting {
		t.Fatalf("Rows = %+v, selector calls = %v, want the candidate to proceed: ASIN alone must never become controller identity", result.Rows, selector.callList())
	}
}

func TestRunner_AmbiguousRecordingIdentity_IsSafeSkip(t *testing.T) {
	deps, provider, _, identities, selector, _, _ := newRunnerDeps()
	detail := sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	detail.Recordings = append(detail.Recordings, AudioSiloRecording{ID: "rec-2", Narrators: []AudioSiloPerson{{ID: "n2", Name: "Other"}}, ASINs: []AudioSiloASIN{}, ISBNs: []string{}, ChapterCount: 5})
	provider.details["w1"] = detail
	// rec-1 is already owned, rec-2 is not: which recording should be
	// imported is ambiguous from provider data alone.
	identities.existing[ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-1"}] = true

	result, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(selector.callList()) != 0 {
		t.Fatalf("selector called, want 0 while recording identity is ambiguous")
	}
	if len(result.Rows) != 1 || result.Rows[0].Status != StatusSkipped {
		t.Fatalf("Rows = %+v, want one Skipped row for an ambiguous identity", result.Rows)
	}
}

func TestRunner_IdentitySourceError_FailsBeforeAnySelection(t *testing.T) {
	deps, provider, _, identities, selector, _, _ := newRunnerDeps()
	provider.details["w1"] = sampleWorkDetail("w1", "Book", "a1", "Author", "rec-1")
	identities.existingErr = errNonContextGapFailure

	_, err := Run(context.Background(), deps, RunRequest{ExplicitWorkIDs: []string{"w1"}, DryRun: true})
	if err == nil {
		t.Fatalf("Run() error = nil, want an error when the identity source cannot be read")
	}
	if len(selector.callList()) != 0 {
		t.Errorf("selector called, want 0 when identity cannot be determined")
	}
}
