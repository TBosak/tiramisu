package audiobookimport

import (
	"reflect"
	"strings"
	"testing"
)

// TestEvaluate_A1_WorkIdentity covers A1: a release is eligible only when
// normalized title and at least one normalized author establish the
// requested work; substring-only, author-only, and title-only matches must
// remain ineligible.
func TestEvaluate_A1_WorkIdentity(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target         Target
		candidateTitle string
		wantEligible   bool
	}{
		{
			name:           "matching_title_and_author_with_punctuation_and_case_variance",
			target:         Target{Work: WorkFacts{Title: "The Fellowship of the Ring", Authors: []string{"J.R.R. Tolkien"}}},
			candidateTitle: "the fellowship OF THE ring -- j r r tolkien [unabridged]",
			wantEligible:   true,
		},
		{
			name:           "diacritics_normalize_in_author_name",
			target:         Target{Work: WorkFacts{Title: "The Notebook", Authors: []string{"José Saramago"}}},
			candidateTitle: "The Notebook by Jose Saramago (Unabridged)",
			wantEligible:   true,
		},
		{
			name:           "scene_separator_punctuation_normalizes",
			target:         Target{Work: WorkFacts{Title: "The Fellowship of the Ring", Authors: []string{"J.R.R. Tolkien"}}},
			candidateTitle: "The.Fellowship.of.the.Ring.J.R.R.Tolkien.UNABRIDGED",
			wantEligible:   true,
		},
		{
			// A complete declared author is required: a bare surname is not the
			// same evidence as "J.R.R. Tolkien" and must not be accepted as a
			// stand-in for it (an implementation that matches on any substring
			// surname would also confuse unrelated authors who share it).
			name:           "surname_only_author_match_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "The Fellowship of the Ring", Authors: []string{"J.R.R. Tolkien"}}},
			candidateTitle: "The Fellowship of the Ring - Tolkien (Unabridged)",
			wantEligible:   false,
		},
		{
			name:           "substring_only_title_match_without_author_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "Dune", Authors: []string{"Frank Herbert"}}},
			candidateTitle: "Dune Buggy Repair and Racing Handbook",
			wantEligible:   false,
		},
		{
			name:           "title_match_without_any_author_evidence_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "Foundation", Authors: []string{"Isaac Asimov"}}},
			candidateTitle: "Foundation (Unabridged Audiobook)",
			wantEligible:   false,
		},
		{
			name:           "author_only_without_title_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "Foundation", Authors: []string{"Isaac Asimov"}}},
			candidateTitle: "Isaac Asimov Complete Robot Short Story Collection",
			wantEligible:   false,
		},
		{
			name:           "same_title_different_author_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "Foundation", Authors: []string{"Isaac Asimov"}}},
			candidateTitle: "Foundation by Peter Hamilton (Unabridged)",
			wantEligible:   false,
		},
		{
			name:           "sequel_volume_mismatch_is_ineligible",
			target:         Target{Work: WorkFacts{Title: "Chronicles of Amber", Volume: "2", Authors: []string{"Roger Zelazny"}}},
			candidateTitle: "Chronicles of Amber Book 1 - Roger Zelazny",
			wantEligible:   false,
		},
	} {
		t.Run("A1_"+tc.name, func(t *testing.T) {
			candidate := ReleaseCandidate{Title: tc.candidateTitle, Seeders: 5, Files: singleM4BFiles()}
			d := Evaluate(tc.target, candidate)
			if d.Eligible != tc.wantEligible {
				t.Fatalf("Eligible = %v, reasons = %v, want %v for candidate title %q against work %+v",
					d.Eligible, d.Reasons, tc.wantEligible, tc.candidateTitle, tc.target.Work)
			}
			if !tc.wantEligible && len(d.Selected) != 0 {
				t.Errorf("got %d selected files for an ineligible identity match, want none", len(d.Selected))
			}
		})
	}
}

// TestEvaluate_A2_RecordingEvidence covers A2: recording evidence accumulates
// separately from work evidence, missing optional evidence stays neutral, and
// plural ASIN/ISBN/Narrator evidence is exercised via partial overlap.
func TestEvaluate_A2_RecordingEvidence(t *testing.T) {
	t.Run("A2_matching_asin_among_plural_sets_sets_evidence", func(t *testing.T) {
		target := baseTarget()
		target.Recording.ASINs = []string{"B0001111", "B0002222"}
		candidate := baseEligibleCandidate()
		candidate.Recording.ASINs = []string{"B0009999", "B0002222"}
		d := Evaluate(target, candidate)
		assertEligible(t, d, "overlapping ASIN among plural sets")
		if !d.Evidence.MatchedASIN {
			t.Errorf("Evidence.MatchedASIN = false, want true for overlapping ASIN sets %v / %v",
				target.Recording.ASINs, candidate.Recording.ASINs)
		}
	})

	t.Run("A2_matching_isbn_among_plural_sets_sets_evidence", func(t *testing.T) {
		target := baseTarget()
		target.Recording.ISBNs = []string{"9781111111111", "9782222222222"}
		candidate := baseEligibleCandidate()
		candidate.Recording.ISBNs = []string{"9789999999999", "9782222222222"}
		d := Evaluate(target, candidate)
		assertEligible(t, d, "overlapping ISBN among plural sets")
		if !d.Evidence.MatchedISBN {
			t.Errorf("Evidence.MatchedISBN = false, want true for overlapping ISBN sets %v / %v",
				target.Recording.ISBNs, candidate.Recording.ISBNs)
		}
	})

	t.Run("A2_matching_narrator_among_plural_sets_sets_evidence", func(t *testing.T) {
		target := baseTarget()
		target.Recording.Narrators = []string{"Rob Inglis", "Andy Serkis"}
		candidate := baseEligibleCandidate()
		candidate.Recording.Narrators = []string{"Someone Else", "Andy Serkis"}
		d := Evaluate(target, candidate)
		assertEligible(t, d, "overlapping narrator among plural sets")
		if !d.Evidence.MatchedNarrator {
			t.Errorf("Evidence.MatchedNarrator = false, want true for overlapping narrator sets %v / %v",
				target.Recording.Narrators, candidate.Recording.Narrators)
		}
	})

	t.Run("A2_missing_target_recording_evidence_is_neutral_not_fabricated", func(t *testing.T) {
		target := baseTarget()
		target.Recording = RecordingFacts{}  // nothing declared beyond work identity
		candidate := baseEligibleCandidate() // has full recording evidence
		d := Evaluate(target, candidate)
		assertEligible(t, d, "target with no declared recording evidence")
		want := Evidence{}
		if d.Evidence != want {
			t.Errorf("Evidence = %+v, want all-false %+v: undeclared target evidence must never be fabricated as matched",
				d.Evidence, want)
		}
	})

	t.Run("A2_fully_matching_recording_sets_every_evidence_field", func(t *testing.T) {
		d := Evaluate(baseTarget(), baseEligibleCandidate())
		assertEligible(t, d, "fully matching recording evidence")
		want := Evidence{
			MatchedASIN:       true,
			MatchedISBN:       true,
			MatchedNarrator:   true,
			MatchedLanguage:   true,
			RuntimeCompatible: true,
			AbridgementMatch:  true,
		}
		if d.Evidence != want {
			t.Errorf("Evidence = %+v, want %+v", d.Evidence, want)
		}
	})
}

// TestEvaluate_A3_ExplicitContradictionVetoes covers A3: an explicit
// contradiction in language, narrator, abridgement, recording identifier, or
// runtime makes a release ineligible even with dominant seeders and size.
func TestEvaluate_A3_ExplicitContradictionVetoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(target *Target, candidate *ReleaseCandidate)
	}{
		{"contradictory_narrator", func(target *Target, candidate *ReleaseCandidate) {
			candidate.Recording.Narrators = []string{"A Completely Different Narrator"}
		}},
		{"contradictory_language", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.Language = "en"
			candidate.Recording.Language = "fr"
		}},
		{"contradictory_abridgement", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.Abridged = AbridgementUnabridged
			candidate.Recording.Abridged = AbridgementAbridged
		}},
		{"contradictory_recording_id", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.RecordingID = "rec-A"
			candidate.Recording.RecordingID = "rec-B"
		}},
		{"contradictory_asin_disjoint_explicit_sets", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.ASINs = []string{"B0001111111"}
			candidate.Recording.ASINs = []string{"B0009999999"} // both nonempty, no overlap
		}},
		{"contradictory_isbn_disjoint_explicit_sets", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.ISBNs = []string{"9781111111111"}
			candidate.Recording.ISBNs = []string{"9789999999999"} // both nonempty, no overlap
		}},
		{"contradictory_runtime", func(target *Target, candidate *ReleaseCandidate) {
			target.Recording.RuntimeMinutes = 600
			candidate.Recording.RuntimeMinutes = 60
		}},
	} {
		t.Run("A3_"+tc.name+"_vetoes_despite_high_seeders_and_size", func(t *testing.T) {
			target := baseTarget()
			candidate := baseEligibleCandidate()
			candidate.Seeders = 100000
			candidate.Files = []AudioFile{{Index: 0, Path: "Huge Rip.m4b", Size: 5 << 30}}
			tc.mutate(&target, &candidate)

			d := Evaluate(target, candidate)
			assertIneligible(t, d, "explicit contradiction: "+tc.name)
		})
	}
}

// TestEvaluate_A4_FileCoherence covers A4 (coherent single-file/multipart
// selection, sidecars ignored) and, in the natural-order subtests, I2
// (stable natural part order and index survival across input reordering).
func TestEvaluate_A4_FileCoherence(t *testing.T) {
	t.Run("A4_single_m4b_selected_sidecars_ignored", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "cover.jpg", Size: 200 << 10},
			{Index: 1, Path: "Fellowship.m4b", Size: 500 << 20},
			{Index: 2, Path: "Fellowship.cue", Size: 4 << 10},
			{Index: 3, Path: "Fellowship.nfo", Size: 2 << 10},
			{Index: 4, Path: "checksums.sfv", Size: 1 << 10},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "single m4b with sidecars")
		want := []SelectedFile{{FileIndex: 1, Part: 0}}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v (only the .m4b payload, sidecars ignored)", d.Selected, want)
		}
	})

	t.Run("A4_single_m4a_selected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{{Index: 0, Path: "Fellowship.m4a", Size: 400 << 20}}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "single m4a payload")
		want := []SelectedFile{{FileIndex: 0, Part: 0}}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v", d.Selected, want)
		}
	})

	t.Run("A4_multipart_mp3_in_nested_common_directory_is_coherent", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "The Fellowship of the Ring/Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "The Fellowship of the Ring/Book - Part 02.mp3", Size: 20 << 20},
			{Index: 2, Path: "The Fellowship of the Ring/Book - Part 03.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "multipart mp3 under one nested common directory")
		want := []SelectedFile{{FileIndex: 0, Part: 1}, {FileIndex: 1, Part: 2}, {FileIndex: 2, Part: 3}}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v", d.Selected, want)
		}
	})

	t.Run("A4_multipart_m4a_parts_selected_in_order", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Book - Part 01.m4a", Size: 60 << 20},
			{Index: 1, Path: "Book - Part 02.m4a", Size: 60 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "coherent multipart m4a set")
		want := []SelectedFile{{FileIndex: 0, Part: 1}, {FileIndex: 1, Part: 2}}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v", d.Selected, want)
		}
	})

	t.Run("A4_I2_ten_parts_selected_in_natural_order_not_lexical_with_indexes_preserved", func(t *testing.T) {
		// Deliberately shuffled slice order; Index ties each entry to its true
		// torrent file index, independent of the position it arrives in.
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 7, Path: "Book - Part 10.mp3", Size: 20 << 20},
			{Index: 2, Path: "Book - Part 03.mp3", Size: 20 << 20},
			{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 9, Path: "Book - Part 02.mp3", Size: 20 << 20},
			{Index: 1, Path: "Book - Part 04.mp3", Size: 20 << 20},
			{Index: 4, Path: "Book - Part 05.mp3", Size: 20 << 20},
			{Index: 5, Path: "Book - Part 06.mp3", Size: 20 << 20},
			{Index: 6, Path: "Book - Part 07.mp3", Size: 20 << 20},
			{Index: 8, Path: "Book - Part 08.mp3", Size: 20 << 20},
			{Index: 3, Path: "Book - Part 09.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "coherent 10-part mp3 set")
		want := []SelectedFile{
			{FileIndex: 0, Part: 1}, {FileIndex: 9, Part: 2}, {FileIndex: 2, Part: 3},
			{FileIndex: 1, Part: 4}, {FileIndex: 4, Part: 5}, {FileIndex: 5, Part: 6},
			{FileIndex: 6, Part: 7}, {FileIndex: 8, Part: 8}, {FileIndex: 3, Part: 9},
			{FileIndex: 7, Part: 10},
		}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v (natural numeric order 1,2,...,10, not lexical \"10\" < \"2\")",
				d.Selected, want)
		}
	})

	t.Run("I2_same_part_set_reordered_in_input_yields_the_same_final_mapping", func(t *testing.T) {
		orderA := []AudioFile{
			{Index: 5, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 2, Path: "Book - Part 02.mp3", Size: 20 << 20},
			{Index: 9, Path: "Book - Part 03.mp3", Size: 20 << 20},
		}
		orderB := []AudioFile{orderA[2], orderA[0], orderA[1]} // same set, different slice order
		want := []SelectedFile{{FileIndex: 5, Part: 1}, {FileIndex: 2, Part: 2}, {FileIndex: 9, Part: 3}}

		candidateA := baseEligibleCandidate()
		candidateA.Files = orderA
		dA := Evaluate(baseTarget(), candidateA)
		assertEligible(t, dA, "part set in original order")
		if !reflect.DeepEqual(dA.Selected, want) {
			t.Errorf("Selected (order A) = %+v, want %+v", dA.Selected, want)
		}

		candidateB := baseEligibleCandidate()
		candidateB.Files = orderB
		dB := Evaluate(baseTarget(), candidateB)
		assertEligible(t, dB, "part set in reordered input")
		if !reflect.DeepEqual(dB.Selected, want) {
			t.Errorf("Selected (order B) = %+v, want %+v: source file indexes must survive input reordering",
				dB.Selected, want)
		}
	})

	t.Run("A4_zero_byte_duplicate_file_is_ignored_not_selected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Fellowship.m4b", Size: 500 << 20},
			{Index: 1, Path: "Fellowship_broken.m4b", Size: 0},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "one substantive m4b plus a zero-byte duplicate")
		want := []SelectedFile{{FileIndex: 0, Part: 0}}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v (zero-byte file must not be selected or counted as a competing payload)",
				d.Selected, want)
		}
	})
}

// TestRank_A5_DeterministicOrdering covers A5: eligible releases rank
// deterministically by recording confidence, coherence, preferred container,
// seeders, and stable input order; seeders cannot override identity or
// coherence vetoes.
func TestRank_A5_DeterministicOrdering(t *testing.T) {
	t.Run("A5_prefers_m4b_over_m4a_over_mp3_regardless_of_seeders", func(t *testing.T) {
		m4b := ReleaseCandidate{
			Title: "The Fellowship of the Ring - J.R.R. Tolkien [M4B Rip]", Seeders: 1,
			Recording: fullyMatchingCandidateRecording(),
			Files:     []AudioFile{{Index: 0, Path: "book.m4b", Size: 500 << 20}},
		}
		m4a := ReleaseCandidate{
			Title: "The Fellowship of the Ring - J.R.R. Tolkien [M4A Rip]", Seeders: 50,
			Recording: fullyMatchingCandidateRecording(),
			Files: []AudioFile{
				{Index: 0, Path: "Part 01.m4a", Size: 60 << 20},
				{Index: 1, Path: "Part 02.m4a", Size: 60 << 20},
			},
		}
		mp3 := ReleaseCandidate{
			Title: "The Fellowship of the Ring - J.R.R. Tolkien [MP3 Rip]", Seeders: 999,
			Recording: fullyMatchingCandidateRecording(),
			Files: []AudioFile{
				{Index: 0, Path: "Part 01.mp3", Size: 20 << 20},
				{Index: 1, Path: "Part 02.mp3", Size: 20 << 20},
			},
		}

		ranked := Rank(baseTarget(), []ReleaseCandidate{mp3, m4a, m4b})
		if len(ranked) != 3 {
			t.Fatalf("len(Rank()) = %d, want 3 (all three are eligible identity matches): %+v", len(ranked), ranked)
		}
		wantContainers := []string{"M4B", "M4A", "MP3"}
		for i, want := range wantContainers {
			tag := "[" + want + " Rip]"
			if !strings.Contains(ranked[i].Candidate.Title, tag) {
				t.Errorf("Rank()[%d].Candidate.Title = %q, want the %s container ranked at position %d",
					i, ranked[i].Candidate.Title, tag, i)
			}
		}
	})

	t.Run("A5_higher_recording_confidence_outranks_more_seeders", func(t *testing.T) {
		strongEvidence := ReleaseCandidate{
			Title: matchingCandidateTitle(), Seeders: 1,
			Recording: fullyMatchingCandidateRecording(),
			Files:     singleM4BFiles(),
		}
		weakEvidence := ReleaseCandidate{
			Title: matchingCandidateTitle(), Seeders: 5000,
			Recording: RecordingFacts{},
			Files:     []AudioFile{{Index: 0, Path: "book2.m4b", Size: 500 << 20}},
		}
		ranked := Rank(baseTarget(), []ReleaseCandidate{weakEvidence, strongEvidence})
		if len(ranked) != 2 {
			t.Fatalf("len(Rank()) = %d, want 2: %+v", len(ranked), ranked)
		}
		if ranked[0].Candidate.Seeders != 1 {
			t.Errorf("Rank()[0].Candidate.Seeders = %d, want 1: recording confidence must outrank seeders",
				ranked[0].Candidate.Seeders)
		}
	})

	t.Run("A5_ties_keep_forward_input_order", func(t *testing.T) {
		a := ReleaseCandidate{Title: "The Fellowship of the Ring - J.R.R. Tolkien [Copy A]", Seeders: 10,
			Recording: fullyMatchingCandidateRecording(), Files: singleM4BFiles()}
		b := ReleaseCandidate{Title: "The Fellowship of the Ring - J.R.R. Tolkien [Copy B]", Seeders: 10,
			Recording: fullyMatchingCandidateRecording(), Files: singleM4BFiles()}

		ranked := Rank(baseTarget(), []ReleaseCandidate{a, b})
		if len(ranked) != 2 {
			t.Fatalf("len(Rank()) = %d, want 2: %+v", len(ranked), ranked)
		}
		if !strings.Contains(ranked[0].Candidate.Title, "[Copy A]") || !strings.Contains(ranked[1].Candidate.Title, "[Copy B]") {
			t.Errorf("Rank() order = [%q, %q], want [Copy A, Copy B] preserved from input order",
				ranked[0].Candidate.Title, ranked[1].Candidate.Title)
		}
	})

	t.Run("A5_ties_keep_reversed_input_order", func(t *testing.T) {
		a := ReleaseCandidate{Title: "The Fellowship of the Ring - J.R.R. Tolkien [Copy A]", Seeders: 10,
			Recording: fullyMatchingCandidateRecording(), Files: singleM4BFiles()}
		b := ReleaseCandidate{Title: "The Fellowship of the Ring - J.R.R. Tolkien [Copy B]", Seeders: 10,
			Recording: fullyMatchingCandidateRecording(), Files: singleM4BFiles()}

		ranked := Rank(baseTarget(), []ReleaseCandidate{b, a})
		if len(ranked) != 2 {
			t.Fatalf("len(Rank()) = %d, want 2: %+v", len(ranked), ranked)
		}
		if !strings.Contains(ranked[0].Candidate.Title, "[Copy B]") || !strings.Contains(ranked[1].Candidate.Title, "[Copy A]") {
			t.Errorf("Rank() order = [%q, %q], want [Copy B, Copy A]: ties must follow input order, not a hidden secondary sort",
				ranked[0].Candidate.Title, ranked[1].Candidate.Title)
		}
	})

	t.Run("A5_veto_excludes_high_seeder_candidate_from_ranked_output", func(t *testing.T) {
		badNarrator := fullyMatchingCandidateRecording()
		badNarrator.Narrators = []string{"Wrong Narrator"} // explicit, no overlap
		vetoed := ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 100000, Recording: badNarrator, Files: singleM4BFiles()}
		good := ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 1, Recording: fullyMatchingCandidateRecording(), Files: singleM4BFiles()}

		ranked := Rank(baseTarget(), []ReleaseCandidate{vetoed, good})
		if len(ranked) != 1 {
			t.Fatalf("len(Rank()) = %d, want 1 (the vetoed candidate must be excluded entirely): %+v", len(ranked), ranked)
		}
		if ranked[0].Candidate.Seeders != 1 {
			t.Errorf("Rank()[0].Candidate.Seeders = %d, want 1: a contradiction must exclude the candidate regardless of its 100000 seeders",
				ranked[0].Candidate.Seeders)
		}
	})
}

// TestEvaluate_E1_EmptyInputs covers E1: empty work title/authors, empty
// release title, or no substantive audio files are rejected with a stable
// reason and no selected files.
func TestEvaluate_E1_EmptyInputs(t *testing.T) {
	t.Run("E1_empty_target_title_is_rejected", func(t *testing.T) {
		target := baseTarget()
		target.Work.Title = ""
		d := Evaluate(target, baseEligibleCandidate())
		assertIneligible(t, d, "empty target title")
	})

	t.Run("E1_empty_target_authors_is_rejected", func(t *testing.T) {
		target := baseTarget()
		target.Work.Authors = nil
		d := Evaluate(target, baseEligibleCandidate())
		assertIneligible(t, d, "no target authors")
	})

	t.Run("E1_empty_candidate_title_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Title = ""
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "empty candidate release title")
	})

	t.Run("E1_no_audio_files_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = nil
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "no audio files at all")
	})

	t.Run("E1_only_sidecar_files_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "cover.jpg", Size: 200 << 10},
			{Index: 1, Path: "book.nfo", Size: 2 << 10},
			{Index: 2, Path: "book.cue", Size: 4 << 10},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "only sidecar files, no substantive audio")
	})

	t.Run("E1_rejection_reason_is_stable_across_repeated_calls", func(t *testing.T) {
		target := baseTarget()
		target.Work.Title = ""
		candidate := baseEligibleCandidate()
		first := Evaluate(target, candidate)
		second := Evaluate(target, candidate)
		if !reflect.DeepEqual(first.Reasons, second.Reasons) {
			t.Errorf("Reasons changed across repeated calls with identical input: %v vs %v, want a stable reason",
				first.Reasons, second.Reasons)
		}
	})
}

// TestEvaluate_E2_MalformedFileSets covers E2: duplicate/gapped multipart
// numbering, competing full-book payloads, bonus/sample-only audio, path
// traversal/absolute source paths, and mixed edition directories are
// rejected rather than partially published.
func TestEvaluate_E2_MalformedFileSets(t *testing.T) {
	t.Run("E2_duplicate_part_number_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "Book - Part 02.mp3", Size: 20 << 20},
			{Index: 2, Path: "Book - Part 02.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "duplicate part number 02")
	})

	t.Run("E2_gap_in_part_numbers_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "Book - Part 02.mp3", Size: 20 << 20},
			{Index: 2, Path: "Book - Part 04.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "missing part 03 between part 02 and part 04")
	})

	t.Run("E2_two_competing_full_book_payloads_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Fellowship - Rip A.m4b", Size: 500 << 20},
			{Index: 1, Path: "Fellowship - Rip B.m4b", Size: 480 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "two competing full-length single-file payloads")
	})

	t.Run("E2_bonus_and_sample_only_audio_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Sample - Chapter One Preview.mp3", Size: 3 << 20},
			{Index: 1, Path: "Bonus Interview With The Author.mp3", Size: 5 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "only bonus/sample audio, no full book or complete part set")
	})

	t.Run("E2_path_traversal_in_source_path_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{{Index: 0, Path: "../../etc/passwd.m4b", Size: 500 << 20}}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "path-traversal source path")
	})

	t.Run("E2_absolute_source_path_is_rejected", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{{Index: 0, Path: "/etc/passwd.m4b", Size: 500 << 20}}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "absolute source path")
	})

	t.Run("E2_mixed_edition_directories_are_rejected_not_merged", func(t *testing.T) {
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "2010 Edition/Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "2010 Edition/Book - Part 02.mp3", Size: 20 << 20},
			{Index: 2, Path: "2015 Edition/Book - Part 01.mp3", Size: 20 << 20},
			{Index: 3, Path: "2015 Edition/Book - Part 02.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "two distinct edition directories, each a plausible complete set")
	})

	t.Run("E2_two_disc_track_layout_is_a_coherent_ordered_multipart_book", func(t *testing.T) {
		// Deliberately shuffled input order; disc-major, track-minor natural
		// order is a valid multipart shape distinct from an explicit "Part N"
		// scheme, and source indexes must still survive the reordering.
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 3, Path: "Disc 2/Track 02.mp3", Size: 20 << 20},
			{Index: 0, Path: "Disc 1/Track 01.mp3", Size: 20 << 20},
			{Index: 2, Path: "Disc 2/Track 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "Disc 1/Track 02.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertEligible(t, d, "coherent two-disc, two-track-per-disc layout")
		want := []SelectedFile{
			{FileIndex: 0, Part: 1}, // Disc 1 Track 01
			{FileIndex: 1, Part: 2}, // Disc 1 Track 02
			{FileIndex: 2, Part: 3}, // Disc 2 Track 01
			{FileIndex: 3, Part: 4}, // Disc 2 Track 02
		}
		if !reflect.DeepEqual(d.Selected, want) {
			t.Errorf("Selected = %+v, want %+v (disc-major, track-minor order, indexes preserved)", d.Selected, want)
		}
	})

	t.Run("E2_unrelated_substantive_audio_alongside_a_complete_part_set_is_rejected", func(t *testing.T) {
		// A naive implementation could publish Parts 1-3 and silently drop the
		// unrelated file, or silently fold it in as a fourth part. Neither is
		// acceptable: the whole release must be rejected, not partially
		// published.
		candidate := baseEligibleCandidate()
		candidate.Files = []AudioFile{
			{Index: 0, Path: "Book - Part 01.mp3", Size: 20 << 20},
			{Index: 1, Path: "Book - Part 02.mp3", Size: 20 << 20},
			{Index: 2, Path: "Book - Part 03.mp3", Size: 20 << 20},
			{Index: 3, Path: "Unrelated Podcast Episode 42.mp3", Size: 20 << 20},
		}
		d := Evaluate(baseTarget(), candidate)
		assertIneligible(t, d, "otherwise complete 3-part set plus one substantive unrelated audio file")
	})
}

// TestEvaluate_E3_RuntimeToleranceBoundaries covers E3: runtime compatibility
// bands measured against the larger-of-minutes-or-percent rule, deterministic
// at every boundary, with unknown runtimes staying neutral. The middle band
// is insufficient on its own and cannot win without an exact identifier: it
// is ineligible without one, and eligible-but-not-runtime-compatible with an
// exact RecordingID shared by target and candidate. A true contradiction (the
// outer band) stays vetoed even when an exact identifier is also shared,
// since A3's veto is not identifier-rescuable.
func TestEvaluate_E3_RuntimeToleranceBoundaries(t *testing.T) {
	target := func(minutes int, withExactID bool) Target {
		rec := RecordingFacts{RuntimeMinutes: minutes}
		if withExactID {
			rec.RecordingID = "rec-shared"
		}
		return Target{Work: baseWork(), Recording: rec}
	}
	candidate := func(minutes int, withExactID bool) ReleaseCandidate {
		rec := RecordingFacts{RuntimeMinutes: minutes}
		if withExactID {
			rec.RecordingID = "rec-shared"
		}
		return ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 5, Recording: rec, Files: singleM4BFiles()}
	}

	for _, tc := range []struct {
		name           string
		targetMinutes  int
		candMinutes    int
		withExactID    bool
		wantEligible   bool
		wantCompatible bool
	}{
		{"compatible_at_60min_boundary_for_a_600min_target", 600, 540, false, true, true},
		{"middle_band_just_above_60min_boundary_without_identifier_is_ineligible", 600, 539, false, false, false},
		{"middle_band_at_90min_boundary_inclusive_without_identifier_is_ineligible", 600, 510, false, false, false},
		{"middle_band_just_above_60min_boundary_with_exact_identifier_is_eligible", 600, 539, true, true, false},
		{"middle_band_at_90min_boundary_inclusive_with_exact_identifier_is_eligible", 600, 510, true, true, false},
		{"contradictory_just_above_90min_boundary", 600, 509, false, false, false},
		{"contradictory_just_above_90min_boundary_with_exact_identifier_still_ineligible", 600, 509, true, false, false},
		{"compatible_symmetric_longer_candidate_at_60min", 600, 660, false, true, true},
		{"contradictory_symmetric_longer_candidate_at_92min", 600, 692, false, false, false},
		{"small_target_percent_floor_compatible_at_15min", 100, 85, false, true, true},
		{"small_target_percent_floor_middle_at_16min_without_identifier_is_ineligible", 100, 84, false, false, false},
		{"small_target_percent_floor_middle_at_16min_with_exact_identifier_is_eligible", 100, 84, true, true, false},
		{"small_target_percent_ceiling_middle_at_30min_boundary_without_identifier_is_ineligible", 100, 70, false, false, false},
		{"small_target_percent_ceiling_middle_at_30min_boundary_with_exact_identifier_is_eligible", 100, 70, true, true, false},
		{"small_target_percent_ceiling_contradictory_at_31min", 100, 69, false, false, false},
		{"unknown_candidate_runtime_is_neutral", 600, 0, false, true, false},
		{"unknown_target_runtime_is_neutral", 0, 540, false, true, false},
	} {
		t.Run("E3_"+tc.name, func(t *testing.T) {
			d := Evaluate(target(tc.targetMinutes, tc.withExactID), candidate(tc.candMinutes, tc.withExactID))
			if d.Eligible != tc.wantEligible {
				t.Fatalf("Eligible = %v, reasons = %v, want %v (target=%dmin candidate=%dmin withExactID=%v)",
					d.Eligible, d.Reasons, tc.wantEligible, tc.targetMinutes, tc.candMinutes, tc.withExactID)
			}
			if tc.wantEligible && d.Evidence.RuntimeCompatible != tc.wantCompatible {
				t.Errorf("Evidence.RuntimeCompatible = %v, want %v (target=%dmin candidate=%dmin withExactID=%v)",
					d.Evidence.RuntimeCompatible, tc.wantCompatible, tc.targetMinutes, tc.candMinutes, tc.withExactID)
			}
		})
	}
}

// TestEvaluate_E4_LanguageComparison covers E4: normalized BCP 47 primary
// language drives compatibility; regional/script variants of one primary
// language are compatible, different primary languages contradict, and
// unknown language on either side is neutral.
func TestEvaluate_E4_LanguageComparison(t *testing.T) {
	target := func(lang string) Target {
		return Target{Work: baseWork(), Recording: RecordingFacts{Language: lang}}
	}
	candidate := func(lang string) ReleaseCandidate {
		return ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 5, Recording: RecordingFacts{Language: lang}, Files: singleM4BFiles()}
	}

	for _, tc := range []struct {
		name           string
		targetLang     string
		candLang       string
		wantEligible   bool
		wantCompatible bool
	}{
		{"regional_variants_en_US_en_GB_are_compatible", "en-US", "en-GB", true, true},
		{"case_insensitive_region_variant_is_compatible", "EN-us", "en-gb", true, true},
		{"same_primary_language_different_script_is_compatible", "zh-Hans", "zh-Hant", true, true},
		{"different_primary_language_contradicts", "en", "fr", false, false},
		{"unknown_candidate_language_is_neutral", "en", "", true, false},
		{"unknown_target_language_is_neutral", "", "fr", true, false},
	} {
		t.Run("E4_"+tc.name, func(t *testing.T) {
			d := Evaluate(target(tc.targetLang), candidate(tc.candLang))
			if d.Eligible != tc.wantEligible {
				t.Fatalf("Eligible = %v, reasons = %v, want %v (target=%q candidate=%q)",
					d.Eligible, d.Reasons, tc.wantEligible, tc.targetLang, tc.candLang)
			}
			if tc.wantEligible && d.Evidence.MatchedLanguage != tc.wantCompatible {
				t.Errorf("Evidence.MatchedLanguage = %v, want %v (target=%q candidate=%q)",
					d.Evidence.MatchedLanguage, tc.wantCompatible, tc.targetLang, tc.candLang)
			}
		})
	}
}

// TestEvaluate_E5_Abridgement covers E5: unknown abridgement is neutral,
// explicit abridged/unabridged values contradict, and a release title token
// counts only as explicit candidate evidence with "unabridged" never misread
// as "abridged".
func TestEvaluate_E5_Abridgement(t *testing.T) {
	candidateWithTitle := func(titleSuffix string, abridged Abridgement) ReleaseCandidate {
		title := "The Fellowship of the Ring - J.R.R. Tolkien"
		if titleSuffix != "" {
			title += " " + titleSuffix
		}
		return ReleaseCandidate{Title: title, Seeders: 5, Recording: RecordingFacts{Abridged: abridged}, Files: singleM4BFiles()}
	}
	targetWith := func(abridged Abridgement) Target {
		return Target{Work: baseWork(), Recording: RecordingFacts{Abridged: abridged}}
	}

	for _, tc := range []struct {
		name           string
		targetAbridged Abridgement
		candTitleTag   string
		candAbridged   Abridgement
		wantEligible   bool
		wantMatch      bool
	}{
		{"unknown_both_is_neutral", AbridgementUnknown, "", AbridgementUnknown, true, false},
		{"explicit_abridged_vs_unabridged_contradicts", AbridgementUnabridged, "", AbridgementAbridged, false, false},
		{"explicit_unabridged_vs_unabridged_matches", AbridgementUnabridged, "", AbridgementUnabridged, true, true},
		{"title_token_unabridged_is_read_correctly_not_as_abridged", AbridgementUnabridged, "(Unabridged)", AbridgementUnknown, true, true},
		{"title_token_abridged_contradicts_unabridged_target", AbridgementUnabridged, "(Abridged)", AbridgementUnknown, false, false},
		{"title_token_abridged_matches_abridged_target", AbridgementAbridged, "(Abridged)", AbridgementUnknown, true, true},
	} {
		t.Run("E5_"+tc.name, func(t *testing.T) {
			d := Evaluate(targetWith(tc.targetAbridged), candidateWithTitle(tc.candTitleTag, tc.candAbridged))
			if d.Eligible != tc.wantEligible {
				t.Fatalf("Eligible = %v, reasons = %v, want %v (target abridged=%v, candidate title tag=%q, candidate abridged=%v)",
					d.Eligible, d.Reasons, tc.wantEligible, tc.targetAbridged, tc.candTitleTag, tc.candAbridged)
			}
			if tc.wantEligible && d.Evidence.AbridgementMatch != tc.wantMatch {
				t.Errorf("Evidence.AbridgementMatch = %v, want %v", d.Evidence.AbridgementMatch, tc.wantMatch)
			}
		})
	}
}

// TestEvaluate_I1_PurityAndDeterminism covers I1: evaluation is pure and
// deterministic and must not mutate its inputs.
func TestEvaluate_I1_PurityAndDeterminism(t *testing.T) {
	t.Run("I1_evaluate_is_deterministic_across_repeated_calls", func(t *testing.T) {
		target := baseTarget()
		candidate := baseEligibleCandidate()
		first := Evaluate(target, candidate)
		for i := 0; i < 50; i++ {
			got := Evaluate(target, candidate)
			if !reflect.DeepEqual(got, first) {
				t.Fatalf("Evaluate() call %d = %+v, want identical result %+v", i, got, first)
			}
		}
	})

	t.Run("I1_evaluate_does_not_mutate_its_inputs", func(t *testing.T) {
		target := baseTarget()
		candidate := baseEligibleCandidate()
		wantTarget := baseTarget()
		wantCandidate := baseEligibleCandidate()

		_ = Evaluate(target, candidate)

		if !reflect.DeepEqual(target, wantTarget) {
			t.Errorf("target mutated by Evaluate: got %+v, want %+v", target, wantTarget)
		}
		if !reflect.DeepEqual(candidate, wantCandidate) {
			t.Errorf("candidate mutated by Evaluate: got %+v, want %+v", candidate, wantCandidate)
		}
	})

	t.Run("I1_rank_is_deterministic_across_repeated_calls", func(t *testing.T) {
		target := baseTarget()
		candidates := []ReleaseCandidate{baseEligibleCandidate(), baseEligibleCandidate()}
		candidates[1].Title = "The Fellowship of the Ring - J.R.R. Tolkien (Unabridged) [Alt Rip]"
		candidates[1].Seeders = 1

		first := Rank(target, candidates)
		for i := 0; i < 25; i++ {
			got := Rank(target, candidates)
			if !reflect.DeepEqual(got, first) {
				t.Fatalf("Rank() call %d = %+v, want identical result %+v", i, got, first)
			}
		}
	})
}

// TestEvaluate_I3_NoSecretLeak covers I3: the decision's reasons must not
// expose a provider token or arbitrary magnet query parameters, even when
// the release title itself contains one.
func TestEvaluate_I3_NoSecretLeak(t *testing.T) {
	t.Run("I3_reasons_never_echo_apikey_or_magnet_query_strings_from_the_release_title", func(t *testing.T) {
		target := baseTarget()
		target.Work.Title = "Foundation"
		target.Work.Authors = []string{"Isaac Asimov"}
		candidate := baseEligibleCandidate()
		candidate.Title = "Foundation by Someone Else magnet:?xt=urn:btih:ABCDEF&tr=http://tracker.example/announce?apikey=SUPERSECRET123"

		d := Evaluate(target, candidate)
		joined := joinedReasons(d.Reasons)
		for _, secret := range []string{"SUPERSECRET123", "apikey=", "btih:ABCDEF"} {
			if strings.Contains(joined, secret) {
				t.Errorf("Reasons %q leaked %q from the candidate title/URL", joined, secret)
			}
		}
	})
}

// TestEvaluate_I4_WorkAndRecordingIdentityDistinct covers I4: AudioSilo work
// id and recording id remain distinct, and an ASIN is recording evidence
// only, never a substitute for work identity.
func TestEvaluate_I4_WorkAndRecordingIdentityDistinct(t *testing.T) {
	t.Run("I4_matching_asin_alone_cannot_establish_a_different_work", func(t *testing.T) {
		target := baseTarget() // "The Fellowship of the Ring" by J.R.R. Tolkien
		candidate := ReleaseCandidate{
			Title:   "Some Entirely Unrelated Cookbook by A Different Author",
			Seeders: 5,
			Recording: RecordingFacts{
				ASINs: append([]string{}, target.Recording.ASINs...), // shares a marketplace id
			},
			Files: []AudioFile{{Index: 0, Path: "cookbook.m4b", Size: 300 << 20}},
		}
		d := Evaluate(target, candidate)
		assertIneligible(t, d, "shared ASIN with an unrelated title/author")
	})

	t.Run("I4_target_without_a_pinned_recording_id_accepts_distinct_recordings_of_the_same_work", func(t *testing.T) {
		target := baseTarget()
		target.Recording.RecordingID = "" // no pinned recording

		r1 := fullyMatchingCandidateRecording()
		r1.RecordingID = "rec-1"
		c1 := ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 5, Recording: r1, Files: singleM4BFiles()}

		r2 := fullyMatchingCandidateRecording()
		r2.RecordingID = "rec-2"
		c2 := ReleaseCandidate{Title: matchingCandidateTitle(), Seeders: 5, Recording: r2, Files: singleM4BFiles()}

		d1 := Evaluate(target, c1)
		d2 := Evaluate(target, c2)
		assertEligible(t, d1, "first recording, no pinned target recording id")
		assertEligible(t, d2, "second recording, no pinned target recording id")
	})
}
