package audiobookimport

import (
	"reflect"
	"testing"
)

// TestReleaseFacts_A5_ConservativeMarkers covers A5: each optional
// recording marker (narrator, BCP 47 language, ASIN, ISBN, runtime,
// abridged/unabridged) is extracted only when explicitly present, one field
// at a time, and an unmarked title yields a fully neutral, empty
// RecordingFacts rather than a guess.
func TestReleaseFacts_A5_ConservativeMarkers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  RecordingFacts
	}{
		{
			name:  "A5_no_markers_at_all_is_fully_neutral",
			title: "The Fellowship of the Ring - J.R.R. Tolkien",
			want:  RecordingFacts{},
		},
		{
			name:  "A5_explicit_narrator_marker",
			title: "Foundation - Isaac Asimov [Narrated by Scott Brick]",
			want:  RecordingFacts{Narrators: []string{"Scott Brick"}},
		},
		{
			name:  "A5_narrator_marker_is_case_insensitive",
			title: "Foundation - Isaac Asimov [narrated by Scott Brick]",
			want:  RecordingFacts{Narrators: []string{"Scott Brick"}},
		},
		{
			name:  "A5_bracketed_bcp47_region_tag",
			title: "Foundation - Isaac Asimov [en-US]",
			want:  RecordingFacts{Language: "en-US"},
		},
		{
			name:  "A5_bracketed_bcp47_primary_only_tag",
			title: "Foundation - Isaac Asimov [de]",
			want:  RecordingFacts{Language: "de"},
		},
		{
			name:  "A5_unbracketed_english_word_is_not_invented_as_a_language",
			title: "Foundation - Isaac Asimov (English)",
			want:  RecordingFacts{},
		},
		{
			name:  "A5_labelled_asin_marker",
			title: "Foundation - Isaac Asimov ASIN B0000AQV1O",
			want:  RecordingFacts{ASINs: []string{"B0000AQV1O"}},
		},
		{
			name:  "A5_labelled_asin_marker_is_case_insensitive",
			title: "Foundation - Isaac Asimov asin: B0000AQV1O",
			want:  RecordingFacts{ASINs: []string{"B0000AQV1O"}},
		},
		{
			name:  "A5_bare_asin_shaped_token_without_label_is_not_invented",
			title: "Foundation - Isaac Asimov B0000AQV1O",
			want:  RecordingFacts{},
		},
		{
			name:  "A5_labelled_isbn_marker",
			title: "Foundation - Isaac Asimov ISBN 9780007171041",
			want:  RecordingFacts{ISBNs: []string{"9780007171041"}},
		},
		{
			name:  "A5_bare_isbn_shaped_digits_without_label_is_not_invented",
			title: "Foundation - Isaac Asimov 9780007171041",
			want:  RecordingFacts{},
		},
		{
			name:  "A5_runtime_hours_and_minutes_form",
			title: "Foundation - Isaac Asimov (12 hrs 34 mins)",
			want:  RecordingFacts{RuntimeMinutes: 754},
		},
		{
			name:  "A5_runtime_hours_only_form",
			title: "Foundation - Isaac Asimov (10 hours)",
			want:  RecordingFacts{RuntimeMinutes: 600},
		},
		{
			name:  "A5_no_runtime_marker_stays_zero_not_guessed",
			title: "Foundation - Isaac Asimov",
			want:  RecordingFacts{},
		},
		{
			name:  "A5_explicit_unabridged_marker",
			title: "Foundation - Isaac Asimov (Unabridged)",
			want:  RecordingFacts{Abridged: AbridgementUnabridged},
		},
		{
			name:  "A5_explicit_abridged_marker",
			title: "Foundation - Isaac Asimov (Abridged)",
			want:  RecordingFacts{Abridged: AbridgementAbridged},
		},
		{
			name:  "A5_unabridged_is_never_misread_as_abridged",
			title: "Foundation - Isaac Asimov UNABRIDGED",
			want:  RecordingFacts{Abridged: AbridgementUnabridged},
		},
		{
			name:  "A5_no_abridgement_marker_stays_unknown",
			title: "Foundation - Isaac Asimov",
			want:  RecordingFacts{Abridged: AbridgementUnknown},
		},
		{
			name:  "A5_unicode_typographic_punctuation_does_not_break_or_invent_extraction",
			title: "Foundation — Isaac Asimov’s Robot Saga [Narrated by Scott Brick] (Unabridged)",
			want:  RecordingFacts{Narrators: []string{"Scott Brick"}, Abridged: AbridgementUnabridged},
		},
		{
			name:  "A5_every_marker_present_together_extracts_every_field",
			title: richAudiobookTitle,
			want:  richAudiobookRecordingFacts(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractReleaseFacts(tc.title)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractReleaseFacts(%q) = %+v, want %+v", tc.title, got, tc.want)
			}
		})
	}
}

// TestReleaseFacts_I1_Determinism covers I1 for this pure function:
// repeated calls with the same title must return identical results, and the
// input string is never mutated (strings are immutable in Go, but this also
// guards against a future signature change to a mutable type being misused).
func TestReleaseFacts_I1_Determinism(t *testing.T) {
	title := richAudiobookTitle
	first := ExtractReleaseFacts(title)
	for i := 0; i < 25; i++ {
		got := ExtractReleaseFacts(title)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("ExtractReleaseFacts(%q) call %d = %+v, want identical result %+v", title, i, got, first)
		}
	}
}

// TestReleaseFacts_E1_EmptyAndGarbageInput covers E1/A5's neutrality
// requirement for degenerate input: an empty or purely-garbage title must
// never panic and must never fabricate evidence.
func TestReleaseFacts_E1_EmptyAndGarbageInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{
		{"E1_empty_title", ""},
		{"E1_whitespace_only_title", "   "},
		{"E1_garbage_symbols_only_title", "###???!!!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractReleaseFacts(tc.title)
			want := RecordingFacts{}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ExtractReleaseFacts(%q) = %+v, want fully neutral %+v", tc.title, got, want)
			}
		})
	}
}
