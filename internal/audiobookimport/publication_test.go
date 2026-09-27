package audiobookimport

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func publishDeps(store StateStore, pub LibraryPublisher, scan AudiobookshelfScanner) PublicationDeps {
	return PublicationDeps{
		Publisher:               pub,
		Scanner:                 scan,
		Store:                   store,
		Limits:                  PublicationLimits{StateLimits: StateLimits{MaxEntries: 1000, MaxStringLength: 4096}, MaxAttempts: 5},
		AudiobookshelfLibraryID: testLibraryID,
	}
}

func freshDeps() (PublicationDeps, *scriptedPublisher, *scriptedScanner, *fakeStore) {
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{{result: LibraryPublishResult{}}}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	return publishDeps(store, pub, scan), pub, scan, store
}

// TestPublish_A1_ExactAdapterRequestShape covers A1: a selected coherent
// release becomes exactly one audiobook Library add carrying the exact
// canonical hash, exact torrent bytes, the stable PlanProjection paths, and
// the exact inspected source path for every selected file index, explicitly
// typed as audiobook at the adapter boundary.
func TestPublish_A1_ExactAdapterRequestShape(t *testing.T) {
	deps, pub, scan, _ := freshDeps()
	req := basePublicationRequest()

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("Publisher.Add called %d times, want exactly 1", len(pub.calls))
	}
	got := pub.calls[0]
	if got.Type != LibraryTypeAudiobook {
		t.Errorf("Type = %q, want %q (explicit audiobook type at the adapter boundary)", got.Type, LibraryTypeAudiobook)
	}
	if !strings.Contains(got.Title, req.Work.Title) {
		t.Errorf("Title = %q, want it to contain the work title %q", got.Title, req.Work.Title)
	}
	if got.Hash != req.Selection.Hash {
		t.Errorf("Hash = %q, want exact canonical hash %q", got.Hash, req.Selection.Hash)
	}
	if string(got.TorrentFile) != string(req.Selection.TorrentFile) {
		t.Errorf("TorrentFile = %v, want exact bytes %v", got.TorrentFile, req.Selection.TorrentFile)
	}
	if got.Magnet != "" {
		t.Errorf("Magnet = %q, want empty when torrent bytes are present", got.Magnet)
	}
	if len(got.Files) != len(req.Projected) {
		t.Fatalf("Files count = %d, want %d (one per PlanProjection entry)", len(got.Files), len(req.Projected))
	}
	for i, projected := range req.Projected {
		file := got.Files[i]
		if file.Path != projected.Path {
			t.Errorf("Files[%d].Path = %q, want stable PlanProjection path %q", i, file.Path, projected.Path)
		}
		if file.SourceIndex != projected.FileIndex {
			t.Errorf("Files[%d].SourceIndex = %d, want %d", i, file.SourceIndex, projected.FileIndex)
		}
		source, ok := findFile(req.Selection.Files, projected.FileIndex)
		if !ok {
			t.Fatalf("test fixture bug: no source file for index %d", projected.FileIndex)
		}
		if file.SourcePath != source.Path {
			t.Errorf("Files[%d].SourcePath = %q, want exact inspected source path %q", i, file.SourcePath, source.Path)
		}
	}
	if scan.calls == nil || len(scan.calls) != 1 {
		t.Errorf("Scanner.Scan called %d times, want exactly 1 after commit", len(scan.calls))
	}
	if result.Stage != StageScanned {
		t.Errorf("Stage = %q, want %q", result.Stage, StageScanned)
	}
}

// TestPublish_A1_SourceVariants covers the adversarial source shapes: hash
// source, full-query-string magnet, exact torrent bytes, and rejection of
// mixed or empty sources.
func TestPublish_A1_SourceVariants(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selection  func() SelectionResult
		wantErr    bool
		wantHash   string
		wantMagnet string
		wantBytes  []byte
	}{
		{name: "torrent_bytes", selection: torrentBytesSelection, wantHash: validHash, wantBytes: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		{name: "magnet_with_full_query", selection: magnetSelection, wantHash: validHash, wantMagnet: magnetSelection().Magnet},
		{name: "hash_only", selection: hashOnlySelection, wantHash: validHash},
		{
			name: "mixed_source_rejected",
			selection: func() SelectionResult {
				s := torrentBytesSelection()
				s.Magnet = magnetSelection().Magnet
				return s
			},
			wantErr: true,
		},
		{
			name: "empty_source_rejected",
			selection: func() SelectionResult {
				s := torrentBytesSelection()
				s.Hash, s.TorrentFile, s.Magnet = "", nil, ""
				return s
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, pub, _, _ := freshDeps()
			req := basePublicationRequest()
			req.Selection = tc.selection()

			result, err := Publish(context.Background(), deps, req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Publish() error = nil, want error for %s", tc.name)
				}
				if len(pub.calls) != 0 {
					t.Errorf("Publisher.Add called %d times, want 0 for a rejected source", len(pub.calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("Publish() error = %v, want nil", err)
			}
			if len(pub.calls) != 1 {
				t.Fatalf("Publisher.Add called %d times, want 1", len(pub.calls))
			}
			got := pub.calls[0]
			if got.Hash != tc.wantHash {
				t.Errorf("Hash = %q, want %q", got.Hash, tc.wantHash)
			}
			if got.Magnet != tc.wantMagnet {
				t.Errorf("Magnet = %q, want %q", got.Magnet, tc.wantMagnet)
			}
			if string(got.TorrentFile) != string(tc.wantBytes) {
				t.Errorf("TorrentFile = %v, want %v", got.TorrentFile, tc.wantBytes)
			}
			_ = result
		})
	}
}

// TestResolveExternalIdentity_A2 covers A2 directly: recording id wins when
// present, work id is the fallback, and an ASIN is never substituted as
// identity even when both real ids are absent.
func TestResolveExternalIdentity_A2(t *testing.T) {
	for _, tc := range []struct {
		name      string
		work      WorkFacts
		recording RecordingFacts
		want      ExternalIdentity
		wantErr   bool
	}{
		{
			name:      "recording_id_present_wins",
			work:      pubWork(),
			recording: pubRecordingWithID(),
			want:      ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-inglis-unabridged"},
		},
		{
			name:      "recording_id_absent_falls_back_to_work_id",
			work:      pubWork(),
			recording: pubRecordingWithoutID(),
			want:      ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: "work-fellowship-1"},
		},
		{
			name:      "asin_only_never_becomes_identity",
			work:      WorkFacts{Title: "The Fellowship of the Ring", Authors: []string{"J.R.R. Tolkien"}},
			recording: RecordingFacts{ASINs: []string{"B000AY7HGY"}},
			wantErr:   true,
		},
		{
			name:      "missing_both_ids_fails",
			work:      WorkFacts{Title: "The Fellowship of the Ring", Authors: []string{"J.R.R. Tolkien"}},
			recording: RecordingFacts{},
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveExternalIdentity(tc.work, tc.recording)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveExternalIdentity() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveExternalIdentity() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("ResolveExternalIdentity() = %+v, want %+v", got, tc.want)
			}
			if got.Namespace == "" || got.ID == "" {
				t.Errorf("ResolveExternalIdentity() = %+v, namespace and id must both be nonempty", got)
			}
		})
	}
}

// TestPublish_A2_IdentityIdenticalAcrossAllFiles covers A2's multipart
// obligation: every projected file of one recording carries the exact same
// external identity.
func TestPublish_A2_IdentityIdenticalAcrossAllFiles(t *testing.T) {
	deps, pub, _, _ := freshDeps()
	req := multipartPublicationRequest()

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("Publisher.Add called %d times, want 1", len(pub.calls))
	}
	files := pub.calls[0].Files
	if len(files) < 2 {
		t.Fatalf("test fixture bug: expected a multipart request, got %d files", len(files))
	}
	want := files[0].ExternalIdentity
	if want.Namespace == "" || want.ID == "" {
		t.Fatalf("ExternalIdentity = %+v, want nonempty namespace and id", want)
	}
	for i, f := range files {
		if f.ExternalIdentity != want {
			t.Errorf("Files[%d].ExternalIdentity = %+v, want %+v (identical across the recording)", i, f.ExternalIdentity, want)
		}
	}
	if result.Identity != want {
		t.Errorf("PublicationResult.Identity = %+v, want %+v", result.Identity, want)
	}
}

// TestPublish_A2_MissingWorkIdentityFailsBeforeWrite covers A2: a selection
// whose work carries neither a recording id nor a work id must fail before
// any Library add.
func TestPublish_A2_MissingWorkIdentityFailsBeforeWrite(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()
	req.Work.WorkID = ""
	req.Recording = RecordingFacts{ASINs: []string{"B000AY7HGY"}}

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want error for missing work identity")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
	}
	if len(store.saveCalls) != 0 {
		t.Errorf("Store.Save called %d times, want 0 (no durable mutation on a failed validation)", len(store.saveCalls))
	}
}

// multipartPublicationRequest builds a 3-part MP3 book whose PlanProjection
// part order differs from the source-inspection order returned by
// SelectSource, exercising I4's exact index-based correlation.
func multipartPublicationRequest() PublicationRequest {
	// Source inspection order is shuffled relative to part order; PlanProjection
	// resolves each selected file's extension through Candidate.Files, so the
	// decision must carry them even though Publish itself looks the source
	// path up through SelectionResult.Files below.
	files := []AudioFile{
		{Index: 9, Path: "inbox/Fellowship/disc3/track.mp3", Size: 90 << 20},
		{Index: 5, Path: "inbox/Fellowship/disc1/track.mp3", Size: 91 << 20},
		{Index: 2, Path: "inbox/Fellowship/disc2/track.mp3", Size: 92 << 20},
	}
	decision := Decision{
		Candidate: ReleaseCandidate{Title: "The Fellowship of the Ring - J.R.R. Tolkien [Unabridged]", Recording: pubRecordingWithID(), Files: files},
		Eligible:  true,
		Selected: []SelectedFile{
			{FileIndex: 5, Part: 1},
			{FileIndex: 2, Part: 2},
			{FileIndex: 9, Part: 3},
		},
	}
	projected, err := PlanProjection(pubWork(), []Decision{decision})
	if err != nil {
		panic(err)
	}
	return PublicationRequest{
		Work:      pubWork(),
		Recording: pubRecordingWithID(),
		Selection: SelectionResult{Selected: &decision, Hash: validHash, TorrentFile: []byte{1, 2, 3}, Files: files},
		Projected: projected,
	}
}

// TestPublish_I4_MultipartOrderAndSourceCorrelation covers I4: the outgoing
// request preserves PlanProjection part order, and every file's source path
// is looked up by exact file index regardless of source-inspection order.
func TestPublish_I4_MultipartOrderAndSourceCorrelation(t *testing.T) {
	deps, pub, _, _ := freshDeps()
	req := multipartPublicationRequest()

	_, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("Publisher.Add called %d times, want 1", len(pub.calls))
	}
	files := pub.calls[0].Files
	if len(files) != 3 {
		t.Fatalf("Files count = %d, want 3", len(files))
	}
	wantOrder := []int{5, 2, 9} // PlanProjection/part order, not source order
	for i, wantIndex := range wantOrder {
		if files[i].SourceIndex != wantIndex {
			t.Errorf("Files[%d].SourceIndex = %d, want %d (part order preserved)", i, files[i].SourceIndex, wantIndex)
		}
		if files[i].Path != req.Projected[i].Path {
			t.Errorf("Files[%d].Path = %q, want %q", i, files[i].Path, req.Projected[i].Path)
		}
		source, _ := findFile(req.Selection.Files, wantIndex)
		if files[i].SourcePath != source.Path {
			t.Errorf("Files[%d].SourcePath = %q, want %q (exact index correlation, not list-position correlation)", i, files[i].SourcePath, source.Path)
		}
	}
}

// TestPublish_I4_MalformedCorrelationRejected covers I4's adversarial cases:
// duplicate source indexes, a selected index missing from the inspected
// source, and mismatched paths must all be rejected before any Library
// call.
func TestPublish_I4_MalformedCorrelationRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PublicationRequest)
	}{
		{
			name: "duplicate_source_index",
			mutate: func(r *PublicationRequest) {
				r.Selection.Files = append(r.Selection.Files, r.Selection.Files[0])
			},
		},
		{
			name: "missing_selected_index_in_source",
			mutate: func(r *PublicationRequest) {
				r.Selection.Files = nil
			},
		},
		{
			name: "projected_index_not_in_selection_selected",
			mutate: func(r *PublicationRequest) {
				r.Projected[0].FileIndex = 999
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, pub, scan, _ := freshDeps()
			req := basePublicationRequest()
			tc.mutate(&req)

			_, err := Publish(context.Background(), deps, req)
			if err == nil {
				t.Fatalf("Publish() error = nil, want error for %s", tc.name)
			}
			if len(pub.calls) != 0 {
				t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
			}
			if len(scan.calls) != 0 {
				t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
			}
		})
	}
}

// TestPublish_I4_ProjectedIndexMustBeActuallySelected covers I4: a
// projected file index must correlate with the Decision's own Selected
// list, not merely happen to exist in SelectionResult.Files. An index the
// candidate evaluator never actually selected must never be published even
// if a well-formed source file at that index happens to exist.
func TestPublish_I4_ProjectedIndexMustBeActuallySelected(t *testing.T) {
	deps, pub, scan, _ := freshDeps()
	req := basePublicationRequest()
	// Index 7 is a real, well-formed source file - but Decision.Selected
	// never selected it (only index 3 was selected). An implementation that
	// checks only Selection.Files, and not Selection.Selected.Selected,
	// would wrongly accept this.
	req.Selection.Files = append(req.Selection.Files, AudioFile{Index: 7, Path: "inbox/Fellowship/bonus.m4b", Size: 10 << 20})
	req.Projected[0].FileIndex = 7

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want error: index 7 exists in Selection.Files but was never in Decision.Selected")
	}
	if len(pub.calls) != 0 || len(scan.calls) != 0 {
		t.Errorf("side effects occurred: add=%d scan=%d, want both 0", len(pub.calls), len(scan.calls))
	}
}

// TestPublish_A4_TamperedSafeProjectedPathRejected covers A4/I4/E5: Publish
// must not trust an arbitrary safe caller-supplied projected path. A path
// that is itself perfectly safe, but does not match what PlanProjection
// would produce for this Work/Decision, must be rejected before any side
// effect - otherwise a caller could redirect a publish to an arbitrary safe
// location outside the projection contract.
func TestPublish_A4_TamperedSafeProjectedPathRejected(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()
	tampered := "Someone Else/A Different Book/A Different Book.m4b"
	if tampered == req.Projected[0].Path {
		t.Fatalf("test fixture bug: tampered path equals the real projected path")
	}
	req.Projected[0].Path = tampered

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want error: a safe but unrelated projected path must be rejected")
	}
	if len(pub.calls) != 0 || len(scan.calls) != 0 || len(store.saveCalls) != 0 {
		t.Errorf("side effects occurred: add=%d scan=%d save=%d, want all 0", len(pub.calls), len(scan.calls), len(store.saveCalls))
	}
}

// TestExternalIdentity_I3_KeyProperties covers I3: Key must be nonempty,
// deterministic, portable (independent of pointer/object identity),
// namespace-sensitive, id-sensitive, and collision-free across the
// identities the rest of this suite relies on to address distinct entries
// in the same durable-state map.
func TestExternalIdentity_I3_KeyProperties(t *testing.T) {
	a := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-1"}
	b := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-2"} // id differs
	c := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: "rec-1"}      // namespace differs
	d := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: "rec-2"}

	type pair struct {
		id  ExternalIdentity
		key string
	}
	pairs := []pair{{a, a.Key()}, {b, b.Key()}, {c, c.Key()}, {d, d.Key()}}
	seen := make(map[string]ExternalIdentity, len(pairs))
	for _, p := range pairs {
		if strings.TrimSpace(p.key) == "" {
			t.Errorf("%+v.Key() = %q, want nonempty", p.id, p.key)
		}
		if other, exists := seen[p.key]; exists {
			t.Errorf("%+v and %+v both produced key %q, want collision-free keys for distinct identities", p.id, other, p.key)
		}
		seen[p.key] = p.id
	}
	if a.Key() != a.Key() {
		t.Errorf("Key() is not deterministic: %q != %q for the same identity", a.Key(), a.Key())
	}
	aAgain := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: "rec-1"}
	if aAgain.Key() != a.Key() {
		t.Errorf("Key() is not portable across independently constructed equal values: %+v.Key() = %q, want %q", aAgain, aAgain.Key(), a.Key())
	}
}

// TestPublish_I4_UnsafeProjectedPathRejected covers the unsafe-path half of
// E1/I4: a projected path escaping its root must be rejected before any
// Library call, even though Publish receives already-built ProjectedFile
// values rather than raw components.
func TestPublish_I4_UnsafeProjectedPathRejected(t *testing.T) {
	for _, path := range []string{"../../etc/passwd", "/etc/passwd", "C:\\Windows\\system32"} {
		t.Run(path, func(t *testing.T) {
			deps, pub, scan, _ := freshDeps()
			req := basePublicationRequest()
			req.Projected[0].Path = path

			_, err := Publish(context.Background(), deps, req)
			if err == nil {
				t.Fatalf("Publish() error = nil, want error for unsafe path %q", path)
			}
			if len(pub.calls) != 0 {
				t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
			}
			if len(scan.calls) != 0 {
				t.Errorf("Scanner.Scan called %d times, want 0", len(scan.calls))
			}
		})
	}
}

// TestPublish_A4_DryRunValid covers A4: a valid dry run returns the exact
// planned request/explanation but performs no add, remove, scan, or durable
// mutation.
func TestPublish_A4_DryRunValid(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()
	req.DryRun = true

	result, err := Publish(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0 on dry run", len(pub.calls))
	}
	if len(scan.calls) != 0 {
		t.Errorf("Scanner.Scan called %d times, want 0 on dry run", len(scan.calls))
	}
	if len(store.saveCalls) != 0 {
		t.Errorf("Store.Save called %d times, want 0 on dry run", len(store.saveCalls))
	}
	if result.Request.Type != LibraryTypeAudiobook {
		t.Errorf("planned Request.Type = %q, want %q", result.Request.Type, LibraryTypeAudiobook)
	}
	if result.Request.Hash != req.Selection.Hash {
		t.Errorf("planned Request.Hash = %q, want %q", result.Request.Hash, req.Selection.Hash)
	}
	if len(result.Request.Files) != len(req.Projected) {
		t.Errorf("planned Request.Files count = %d, want %d", len(result.Request.Files), len(req.Projected))
	}
	if strings.TrimSpace(result.Explanation) == "" {
		t.Errorf("Explanation is empty, want the planned explanation")
	}
}

// TestPublish_A4_DryRunInvalid covers A4's negative half: an invalid
// selection/projection under dry run still fails, with no side effects.
func TestPublish_A4_DryRunInvalid(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()
	req.DryRun = true
	req.Selection.Selected = nil // ineligible selection

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want error for an ineligible selection under dry run")
	}
	if len(pub.calls) != 0 || len(scan.calls) != 0 || len(store.saveCalls) != 0 {
		t.Errorf("side effects occurred on an invalid dry run: add=%d scan=%d save=%d, want all 0", len(pub.calls), len(scan.calls), len(store.saveCalls))
	}
}

// TestPublish_E1_InvalidInputsFailBeforeCalls covers E1's enumerated
// up-front validation failures.
func TestPublish_E1_InvalidInputsFailBeforeCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PublicationDeps, *PublicationRequest)
	}{
		{
			name:   "nil_publisher",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.Publisher = nil },
		},
		{
			name:   "nil_scanner",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.Scanner = nil },
		},
		{
			name:   "nil_store",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.Store = nil },
		},
		{
			name:   "invalid_limits_negative_max_entries",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.Limits.MaxEntries = -1 },
		},
		{
			name:   "invalid_limits_negative_max_attempts",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.Limits.MaxAttempts = -1 },
		},
		{
			name:   "empty_library_id",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { d.AudiobookshelfLibraryID = "" },
		},
		{
			name:   "nil_selected_decision",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { r.Selection.Selected = nil },
		},
		{
			name:   "ineligible_selected_decision",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { r.Selection.Selected.Eligible = false },
		},
		{
			name:   "empty_selected_files",
			mutate: func(d *PublicationDeps, r *PublicationRequest) { r.Selection.Selected.Selected = nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, pub, scan, store := freshDeps()
			req := basePublicationRequest()
			tc.mutate(&deps, &req)

			_, err := Publish(context.Background(), deps, req)
			if err == nil {
				t.Fatalf("Publish() error = nil, want error for %s", tc.name)
			}
			if len(pub.calls) != 0 || len(scan.calls) != 0 || len(store.saveCalls) != 0 {
				t.Errorf("side effects occurred for invalid input %s: add=%d scan=%d save=%d, want all 0", tc.name, len(pub.calls), len(scan.calls), len(store.saveCalls))
			}
		})
	}
}

// TestPublish_I1_ContextPropagatesToEveryDependency covers I1: the caller's
// context (not some internally constructed one) reaches every dependency
// call.
func TestPublish_I1_ContextPropagatesToEveryDependency(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()

	ctx := withMarker(context.Background(), "publish-marker")
	if _, err := Publish(ctx, deps, req); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	for i, m := range pub.markers {
		if m != "publish-marker" {
			t.Errorf("Publisher.Add call %d marker = %q, want %q", i, m, "publish-marker")
		}
	}
	for i, m := range scan.markers {
		if m != "publish-marker" {
			t.Errorf("Scanner.Scan call %d marker = %q, want %q", i, m, "publish-marker")
		}
	}
	if len(store.loadMarkers) == 0 {
		t.Errorf("Store.Load was never called, want at least one call to prove context propagation")
	}
	for i, m := range store.loadMarkers {
		if m != "publish-marker" {
			t.Errorf("Store.Load call %d marker = %q, want %q", i, m, "publish-marker")
		}
	}
	if len(store.saveMarkers) == 0 {
		t.Errorf("Store.Save was never called, want at least one call to prove context propagation")
	}
	for i, m := range store.saveMarkers {
		if m != "publish-marker" {
			t.Errorf("Store.Save call %d marker = %q, want %q", i, m, "publish-marker")
		}
	}
}

// TestPublish_I1_CancellationBeforeStartPreventsAllSideEffects covers I1:
// an already-cancelled context stops every later, uncommitted effect and
// surfaces an errors.Is-compatible cancellation error.
func TestPublish_I1_CancellationBeforeStartPreventsAllSideEffects(t *testing.T) {
	deps, pub, scan, store := freshDeps()
	req := basePublicationRequest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Publish(ctx, deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(pub.calls) != 0 || len(scan.calls) != 0 || len(store.saveCalls) != 0 {
		t.Errorf("side effects occurred after cancellation: add=%d scan=%d save=%d, want all 0", len(pub.calls), len(scan.calls), len(store.saveCalls))
	}
}

// TestPublish_I5_ErrorsAndExplanationsNeverLeakSecrets covers I5: error
// text and the returned explanation never contain the magnet, hash, or
// torrent bytes, even when the underlying dependency error embeds them.
func TestPublish_I5_ErrorsAndExplanationsNeverLeakSecrets(t *testing.T) {
	const secretMarker = "SEKRIT-MAGNET-PAYLOAD-MARKER"
	log := &callLog{}
	store := newFakeStore(emptyState())
	store.log = log
	pub := &scriptedPublisher{log: log, steps: []publishStep{{err: errors.New("upstream failure embedding " + secretMarker)}}}
	scan := &scriptedScanner{log: log, steps: []scanStep{{result: AudiobookshelfScanResult{StatusCode: 200}}}}
	deps := publishDeps(store, pub, scan)

	req := basePublicationRequest()
	req.Selection.Magnet = ""
	req.Selection.TorrentFile = []byte(secretMarker)

	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error surfaced from the failing Add")
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Errorf("error %q leaks the secret marker", err.Error())
	}
}
