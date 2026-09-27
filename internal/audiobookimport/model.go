// Package audiobookimport contains audiobook-specific controller policy.
// It remains independent from music import policy and the torrent/FUSE engine.
package audiobookimport

// Abridgement represents explicit abridgement evidence. Unknown is neutral.
type Abridgement int

const (
	AbridgementUnknown Abridgement = iota
	AbridgementAbridged
	AbridgementUnabridged
)

type WorkFacts struct {
	WorkID  string
	Title   string
	Authors []string
	Series  string
	Volume  string
}

type RecordingFacts struct {
	RecordingID    string
	ASINs          []string
	ISBNs          []string
	Narrators      []string
	Language       string
	RuntimeMinutes int
	Publisher      string
	ChapterCount   int
	Abridged       Abridgement
}

type AudioFile struct {
	Index int
	Path  string
	Size  int64
}

type ReleaseCandidate struct {
	Title     string
	Seeders   int
	Recording RecordingFacts
	Files     []AudioFile
}

type Target struct {
	Work      WorkFacts
	Recording RecordingFacts
}

// Reason is a canonical status value. It never includes arbitrary source text.
type Reason string

const (
	reasonInvalidWork       Reason = "invalid_work"
	reasonWorkMismatch      Reason = "work_mismatch"
	reasonVolumeMismatch    Reason = "volume_mismatch"
	reasonRecordingMismatch Reason = "recording_mismatch"
	reasonRuntimeUncertain  Reason = "runtime_uncertain"
	reasonInvalidFiles      Reason = "invalid_files"
)

type Evidence struct {
	MatchedASIN       bool
	MatchedISBN       bool
	MatchedNarrator   bool
	MatchedLanguage   bool
	RuntimeCompatible bool
	AbridgementMatch  bool
}

type SelectedFile struct {
	FileIndex int
	Part      int
}

type Decision struct {
	Candidate ReleaseCandidate
	Eligible  bool
	Reasons   []Reason
	Evidence  Evidence
	Selected  []SelectedFile
}

type ProjectedFile struct {
	DecisionIndex int
	FileIndex     int
	Path          string
}
