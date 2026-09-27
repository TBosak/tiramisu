package audiobookimport

import (
	"reflect"
	"strings"
	"testing"
)

// eligibleDecisionSingleFile builds one accepted decision for one selected
// single file, matching the shape Evaluate would have produced.
func eligibleDecisionSingleFile(recording RecordingFacts, fileIndex int) Decision {
	return Decision{
		Candidate: ReleaseCandidate{
			Title:     "irrelevant for projection",
			Recording: recording,
			Files:     []AudioFile{{Index: fileIndex, Path: "book.m4b", Size: 500 << 20}},
		},
		Eligible: true,
		Selected: []SelectedFile{{FileIndex: fileIndex, Part: 0}},
	}
}

// TestPlanProjection_A6_StablePaths covers A6: stable projection paths encode
// author, optional series/volume, work title, and a recording disambiguator
// when multiple recordings of the work could collide; every selected source
// file maps to exactly one safe relative path while keeping its file index.
func TestPlanProjection_A6_StablePaths(t *testing.T) {
	t.Run("A6_single_recording_no_disambiguator_needed", func(t *testing.T) {
		work := WorkFacts{Title: "Solo Book", Authors: []string{"Jane Doe"}}
		decisions := []Decision{eligibleDecisionSingleFile(RecordingFacts{RecordingID: "r1"}, 0)}

		got, err := PlanProjection(work, decisions)
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil for one recording of a work", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(PlanProjection()) = %d, want 1: %+v", len(got), got)
		}
		p := got[0]
		if p.DecisionIndex != 0 || p.FileIndex != 0 {
			t.Errorf("ProjectedFile = %+v, want DecisionIndex=0 FileIndex=0", p)
		}
		assertSafeRelPath(t, p.Path)
		lower := strings.ToLower(p.Path)
		if !strings.Contains(lower, "jane doe") {
			t.Errorf("Path %q does not encode the author %q", p.Path, work.Authors[0])
		}
		if !strings.Contains(lower, "solo book") {
			t.Errorf("Path %q does not encode the work title %q", p.Path, work.Title)
		}
		if !strings.HasSuffix(lower, ".m4b") {
			t.Errorf("Path %q does not preserve the .m4b extension of the selected source file", p.Path)
		}
	})

	t.Run("A6_series_and_volume_encoded_when_known", func(t *testing.T) {
		work := WorkFacts{Title: "Ninth Volume", Authors: []string{"Ann Author"}, Series: "Amber Chronicles", Volume: "2"}
		decisions := []Decision{eligibleDecisionSingleFile(RecordingFacts{}, 0)}

		got, err := PlanProjection(work, decisions)
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(PlanProjection()) = %d, want 1: %+v", len(got), got)
		}
		lower := strings.ToLower(got[0].Path)
		for _, want := range []string{"amber chronicles", "ninth volume", "ann author", "2"} {
			if !strings.Contains(lower, want) {
				t.Errorf("Path %q does not encode %q", got[0].Path, want)
			}
		}
	})

	t.Run("A6_recording_disambiguator_identifies_its_own_recording_and_survives_reversed_order", func(t *testing.T) {
		// Each colliding decision's path must carry its own recording's narrator
		// or RecordingID, and that association must not depend on the
		// decision's position in the input slice.
		work := WorkFacts{Title: "Dual Recording Book", Authors: []string{"Multi Author"}}
		fry := eligibleDecisionSingleFile(RecordingFacts{RecordingID: "rec-fry", Narrators: []string{"Stephen Fry"}}, 0)
		dale := eligibleDecisionSingleFile(RecordingFacts{RecordingID: "rec-dale", Narrators: []string{"Jim Dale"}}, 0)

		hasFryTag := func(path string) bool {
			lower := strings.ToLower(path)
			return strings.Contains(lower, "stephen fry") || strings.Contains(lower, "rec-fry")
		}
		hasDaleTag := func(path string) bool {
			lower := strings.ToLower(path)
			return strings.Contains(lower, "jim dale") || strings.Contains(lower, "rec-dale")
		}

		// findTagged locates the single path matching hasTag in got, failing if
		// zero or more than one path matches. DecisionIndex may legitimately
		// change between the forward and reversed calls; only the returned
		// Path is compared across calls below.
		findTagged := func(t *testing.T, got []ProjectedFile, label string, hasTag func(string) bool) string {
			t.Helper()
			var found string
			matches := 0
			for _, p := range got {
				assertSafeRelPath(t, p.Path)
				if hasTag(p.Path) {
					found = p.Path
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("%s: found %d paths matching the tag, want exactly 1: %+v", label, matches, got)
			}
			return found
		}

		forward, err := PlanProjection(work, []Decision{fry, dale})
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil for two recordings of one work", err)
		}
		if len(forward) != 2 {
			t.Fatalf("forward order [fry, dale]: len(PlanProjection()) = %d, want 2: %+v", len(forward), forward)
		}
		if forward[0].Path == forward[1].Path {
			t.Errorf("forward order [fry, dale]: both recordings projected to the same path %q", forward[0].Path)
		}
		forwardFry := findTagged(t, forward, "forward order [fry, dale], Fry path", hasFryTag)
		forwardDale := findTagged(t, forward, "forward order [fry, dale], Dale path", hasDaleTag)

		reversed, err := PlanProjection(work, []Decision{dale, fry})
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil for two recordings of one work", err)
		}
		if len(reversed) != 2 {
			t.Fatalf("reversed order [dale, fry]: len(PlanProjection()) = %d, want 2: %+v", len(reversed), reversed)
		}
		if reversed[0].Path == reversed[1].Path {
			t.Errorf("reversed order [dale, fry]: both recordings projected to the same path %q", reversed[0].Path)
		}
		reversedFry := findTagged(t, reversed, "reversed order [dale, fry], Fry path", hasFryTag)
		reversedDale := findTagged(t, reversed, "reversed order [dale, fry], Dale path", hasDaleTag)

		// The recording, not its position in the input slice, owns the path:
		// DecisionIndex may differ between forward and reversed, but Path must
		// be byte-for-byte identical, or a naive "position + tag" scheme that
		// only happens to include the right narrator would still pass.
		if forwardFry != reversedFry {
			t.Errorf("Stephen Fry recording's path changed with input order: forward=%q, reversed=%q; Path must be stable across input order, only DecisionIndex may change",
				forwardFry, reversedFry)
		}
		if forwardDale != reversedDale {
			t.Errorf("Jim Dale recording's path changed with input order: forward=%q, reversed=%q; Path must be stable across input order, only DecisionIndex may change",
				forwardDale, reversedDale)
		}
	})

	t.Run("A6_every_selected_file_maps_to_one_path_preserving_file_index", func(t *testing.T) {
		work := WorkFacts{Title: "Multipart Book", Authors: []string{"Multi Part Author"}}
		d := Decision{
			Candidate: ReleaseCandidate{
				Title: "irrelevant",
				Files: []AudioFile{
					{Index: 5, Path: "Book - Part 01.mp3", Size: 20 << 20},
					{Index: 2, Path: "Book - Part 02.mp3", Size: 20 << 20},
					{Index: 9, Path: "Book - Part 03.mp3", Size: 20 << 20},
				},
			},
			Eligible: true,
			Selected: []SelectedFile{{FileIndex: 5, Part: 1}, {FileIndex: 2, Part: 2}, {FileIndex: 9, Part: 3}},
		}

		got, err := PlanProjection(work, []Decision{d})
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(PlanProjection()) = %d, want 3: %+v", len(got), got)
		}

		gotIndexes := map[int]bool{}
		seenPaths := map[string]bool{}
		for _, p := range got {
			if p.DecisionIndex != 0 {
				t.Errorf("ProjectedFile.DecisionIndex = %d, want 0: %+v", p.DecisionIndex, p)
			}
			gotIndexes[p.FileIndex] = true
			if seenPaths[p.Path] {
				t.Errorf("duplicate projected path %q for two different source files", p.Path)
			}
			seenPaths[p.Path] = true
			assertSafeRelPath(t, p.Path)
			if !strings.HasSuffix(strings.ToLower(p.Path), ".mp3") {
				t.Errorf("Path %q does not preserve the .mp3 extension of its selected mp3 part", p.Path)
			}
		}
		for _, want := range []int{5, 2, 9} {
			if !gotIndexes[want] {
				t.Errorf("missing a projected file for source file index %d; got indexes %v", want, gotIndexes)
			}
		}
	})
}

// TestPlanProjection_E6_SafeNames covers E6: unsafe or unusable names are
// rejected or sanitized into nonempty portable path components; no result
// may contain "..", an absolute root, a drive prefix, or control characters.
// Either outcome (reject or sanitize) is accepted; when sanitized, the
// safety invariants must hold.
func TestPlanProjection_E6_SafeNames(t *testing.T) {
	t.Run("E6_ordinary_input_produces_a_nonempty_safe_path", func(t *testing.T) {
		work := WorkFacts{Title: "Ordinary Book", Authors: []string{"Normal Author"}}
		got, err := PlanProjection(work, []Decision{eligibleDecisionSingleFile(RecordingFacts{}, 0)})
		if err != nil {
			t.Fatalf("PlanProjection() error = %v, want nil for ordinary safe input", err)
		}
		if len(got) != 1 {
			t.Fatalf("len(PlanProjection()) = %d, want 1: %+v", len(got), got)
		}
		assertSafeRelPath(t, got[0].Path)
	})

	for _, tc := range []struct {
		name string
		work WorkFacts
	}{
		{
			name: "control_characters_and_nulls_in_author_and_title",
			work: WorkFacts{Title: "Evil\x00Title\x01", Authors: []string{"Bad\x02Author"}},
		},
		{
			name: "path_traversal_tokens_in_series_and_volume",
			work: WorkFacts{Title: "Book", Authors: []string{"Author"}, Series: "../../../etc", Volume: "../.."},
		},
		{
			name: "absolute_unix_path_in_title",
			work: WorkFacts{Title: "/etc/passwd", Authors: []string{"Author"}},
		},
		{
			name: "windows_drive_prefix_in_author",
			work: WorkFacts{Title: "Book", Authors: []string{`C:\Windows\System32`}},
		},
		{
			name: "unicode_bidi_and_zero_width_controls",
			work: WorkFacts{Title: "Evil\u202Etitle\u200b", Authors: []string{"Author\ufeff"}},
		},
		{
			name: "blank_title_and_author_after_trimming",
			work: WorkFacts{Title: "   ", Authors: []string{"   "}},
		},
	} {
		t.Run("E6_hostile_input_"+tc.name, func(t *testing.T) {
			decisions := []Decision{eligibleDecisionSingleFile(RecordingFacts{}, 0)}
			got, err := PlanProjection(tc.work, decisions)
			if err != nil {
				// Rejecting hostile/unusable input outright is an accepted
				// conforming outcome for E6.
				return
			}
			if len(got) == 0 {
				t.Fatalf("PlanProjection() returned no error and no projected files for %+v", tc.work)
			}
			for _, p := range got {
				assertSafeRelPath(t, p.Path)
			}
		})
	}
}

// TestPlanProjection_Determinism is a light purity cross-check specific to
// PlanProjection, complementing I1's Evaluate/Rank coverage: identical inputs
// must not depend on map order or any other hidden nondeterminism.
func TestPlanProjection_Determinism(t *testing.T) {
	work := WorkFacts{Title: "Repeatable Book", Authors: []string{"Repeatable Author"}}
	decisions := []Decision{eligibleDecisionSingleFile(RecordingFacts{RecordingID: "r1"}, 0)}

	first, firstErr := PlanProjection(work, decisions)
	for i := 0; i < 25; i++ {
		got, err := PlanProjection(work, decisions)
		if (err == nil) != (firstErr == nil) {
			t.Fatalf("PlanProjection() call %d error = %v, want error-ness to match first call (%v)", i, err, firstErr)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("PlanProjection() call %d = %+v, want identical result %+v", i, got, first)
		}
	}
}
