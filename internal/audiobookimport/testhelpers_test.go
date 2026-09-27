package audiobookimport

import (
	"strings"
	"testing"
)

// baseWork is the identity every fixture in this suite starts from unless a
// test is specifically exercising identity matching (A1) or projection naming
// (A6), which build their own WorkFacts explicitly.
func baseWork() WorkFacts {
	return WorkFacts{
		WorkID:  "work-fellowship-1",
		Title:   "The Fellowship of the Ring",
		Authors: []string{"J.R.R. Tolkien"},
	}
}

// baseTargetRecording is a fully specified recording preference: every
// optional field is known, so tests built on it can flip one field at a time
// to prove that field's contradiction/compatibility rule in isolation.
func baseTargetRecording() RecordingFacts {
	return RecordingFacts{
		RecordingID:    "rec-inglis-unabridged",
		ASINs:          []string{"B000AY7HGY"},
		ISBNs:          []string{"9780007171041"},
		Narrators:      []string{"Rob Inglis"},
		Language:       "en-US",
		RuntimeMinutes: 600,
		Publisher:      "Recorded Books",
		ChapterCount:   22,
		Abridged:       AbridgementUnabridged,
	}
}

// fullyMatchingCandidateRecording carries evidence that should match
// baseTargetRecording on every field, exercising plural (ASINs/ISBNs/
// Narrators) evidence via partial-overlap sets rather than identical slices.
func fullyMatchingCandidateRecording() RecordingFacts {
	return RecordingFacts{
		RecordingID:    "rec-inglis-unabridged",
		ASINs:          []string{"B000ZZZZ99", "B000AY7HGY"},
		ISBNs:          []string{"9780007171041"},
		Narrators:      []string{"Someone Else", "Rob Inglis"},
		Language:       "en-GB",
		RuntimeMinutes: 600,
		Publisher:      "Recorded Books",
		ChapterCount:   22,
		Abridged:       AbridgementUnabridged,
	}
}

func matchingCandidateTitle() string {
	return "The Fellowship of the Ring - J.R.R. Tolkien [Unabridged]"
}

func singleM4BFiles() []AudioFile {
	return []AudioFile{{Index: 0, Path: "The Fellowship of the Ring.m4b", Size: 500 << 20}}
}

func baseTarget() Target {
	return Target{Work: baseWork(), Recording: baseTargetRecording()}
}

func baseEligibleCandidate() ReleaseCandidate {
	return ReleaseCandidate{
		Title:     matchingCandidateTitle(),
		Seeders:   10,
		Recording: fullyMatchingCandidateRecording(),
		Files:     singleM4BFiles(),
	}
}

func assertEligible(t *testing.T, d Decision, msg string) {
	t.Helper()
	if !d.Eligible {
		t.Fatalf("%s: Eligible = false, reasons = %v, want true", msg, d.Reasons)
	}
}

func assertIneligible(t *testing.T, d Decision, msg string) {
	t.Helper()
	if d.Eligible {
		t.Fatalf("%s: Eligible = true, evidence = %+v, want false", msg, d.Evidence)
	}
	if len(d.Selected) != 0 {
		t.Errorf("%s: got %d selected files for an ineligible decision, want none", msg, len(d.Selected))
	}
	if len(d.Reasons) == 0 {
		t.Errorf("%s: got no reasons for an ineligible decision, want a stable reason", msg)
	}
}

func joinedReasons(reasons []Reason) string {
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = string(r)
	}
	return strings.Join(parts, " | ")
}

// controlAndUnsafeChars is every C0 control character plus DEL: none may
// appear in a projected path (E6).
const controlAndUnsafeChars = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f"

// assertSafeRelPath enforces E6's output-safety invariants without
// prescribing whether the implementation sanitizes or rejects: callers decide
// whether to invoke this at all for a given hostile-input case.
func assertSafeRelPath(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatalf("projected path is empty, want a nonempty portable relative path")
	}
	if strings.ContainsAny(path, controlAndUnsafeChars) {
		t.Errorf("path %q contains a control character", path)
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		t.Errorf("path %q has an absolute root prefix", path)
	}
	if len(path) >= 2 && path[1] == ':' && isASCIILetter(path[0]) {
		t.Errorf("path %q has a drive prefix", path)
	}
	if strings.Contains(path, "..") {
		t.Errorf("path %q contains \"..\"", path)
	}
	norm := strings.ReplaceAll(path, "\\", "/")
	for _, comp := range strings.Split(norm, "/") {
		if comp == "" {
			t.Errorf("path %q has an empty path component", path)
		}
	}
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
