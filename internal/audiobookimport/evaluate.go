package audiobookimport

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	volumePattern     = regexp.MustCompile(`(?i)\b(?:book|vol(?:ume)?)[\s._#-]*(\d+)\b`)
	abridgedPattern   = regexp.MustCompile(`(?i)\babridged\b`)
	unabridgedPattern = regexp.MustCompile(`(?i)\bunabridged\b`)
)

func Evaluate(target Target, candidate ReleaseCandidate) Decision {
	d := Decision{Candidate: candidate}
	reject := func(reason Reason) Decision {
		d.Reasons = []Reason{reason}
		d.Selected = nil
		return d
	}

	if normalize(target.Work.Title) == "" || firstUsable(target.Work.Authors) == "" || normalize(candidate.Title) == "" {
		return reject(reasonInvalidWork)
	}
	if !containsPhrase(candidate.Title, target.Work.Title) || !containsAnyAuthor(candidate.Title, target.Work.Authors) {
		return reject(reasonWorkMismatch)
	}
	if target.Work.Volume != "" {
		if stated := statedVolume(candidate.Title); stated != "" && normalizeNumber(stated) != normalizeNumber(target.Work.Volume) {
			return reject(reasonVolumeMismatch)
		}
	}

	a, b := target.Recording, candidate.Recording
	d.Evidence.MatchedASIN = overlaps(a.ASINs, b.ASINs)
	d.Evidence.MatchedISBN = overlaps(a.ISBNs, b.ISBNs)
	d.Evidence.MatchedNarrator = overlapsNormalized(a.Narrators, b.Narrators)
	if bothDifferent(a.RecordingID, b.RecordingID) || disjointExplicit(a.ASINs, b.ASINs) ||
		disjointExplicit(a.ISBNs, b.ISBNs) || disjointExplicitNormalized(a.Narrators, b.Narrators) {
		return reject(reasonRecordingMismatch)
	}

	langA, langB := primaryLanguage(a.Language), primaryLanguage(b.Language)
	if langA != "" && langB != "" {
		if langA != langB {
			return reject(reasonRecordingMismatch)
		}
		d.Evidence.MatchedLanguage = true
	}

	abridgement := b.Abridged
	if abridgement == AbridgementUnknown {
		abridgement = titleAbridgement(candidate.Title)
	}
	if a.Abridged != AbridgementUnknown && abridgement != AbridgementUnknown {
		if a.Abridged != abridgement {
			return reject(reasonRecordingMismatch)
		}
		d.Evidence.AbridgementMatch = true
	}

	if a.RuntimeMinutes > 0 && b.RuntimeMinutes > 0 {
		difference := abs(a.RuntimeMinutes - b.RuntimeMinutes)
		compatible := max(15, a.RuntimeMinutes*10/100)
		contradictory := max(30, a.RuntimeMinutes*15/100)
		switch {
		case difference <= compatible:
			d.Evidence.RuntimeCompatible = true
		case difference > contradictory:
			return reject(reasonRecordingMismatch)
		case !exactRecordingMatch(a, b):
			return reject(reasonRuntimeUncertain)
		}
	}

	selected, ok := coherentFiles(candidate.Files)
	if !ok {
		return reject(reasonInvalidFiles)
	}
	d.Eligible = true
	d.Selected = selected
	d.Reasons = nil
	return d
}

func Rank(target Target, candidates []ReleaseCandidate) []Decision {
	type ranked struct {
		decision  Decision
		evidence  int
		container int
	}
	rows := make([]ranked, 0, len(candidates))
	for _, candidate := range candidates {
		decision := Evaluate(target, candidate)
		if decision.Eligible {
			rows = append(rows, ranked{decision, evidenceScore(decision.Evidence), containerScore(decision)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].evidence != rows[j].evidence {
			return rows[i].evidence > rows[j].evidence
		}
		if rows[i].container != rows[j].container {
			return rows[i].container > rows[j].container
		}
		return rows[i].decision.Candidate.Seeders > rows[j].decision.Candidate.Seeders
	})
	result := make([]Decision, len(rows))
	for i := range rows {
		result[i] = rows[i].decision
	}
	return result
}

func normalize(value string) string {
	value = norm.NFD.String(strings.ToLower(value))
	var b strings.Builder
	space := true
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func containsPhrase(haystack, needle string) bool {
	h, n := strings.Fields(normalize(haystack)), strings.Fields(normalize(needle))
	for i := 0; len(n) > 0 && i+len(n) <= len(h); i++ {
		matched := true
		for j := range n {
			if h[i+j] != n[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func containsAnyAuthor(title string, authors []string) bool {
	for _, author := range authors {
		if containsPhrase(title, author) {
			return true
		}
	}
	return false
}

func statedVolume(title string) string {
	match := volumePattern.FindStringSubmatch(title)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func normalizeNumber(value string) string {
	value = strings.TrimLeft(strings.TrimSpace(value), "0")
	if value == "" {
		return "0"
	}
	return value
}

func bothDifferent(a, b string) bool {
	return strings.TrimSpace(a) != "" && strings.TrimSpace(b) != "" && !strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func overlaps(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range b {
		if _, ok := seen[strings.ToLower(strings.TrimSpace(value))]; ok {
			return true
		}
	}
	return false
}

func overlapsNormalized(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		if value = normalize(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range b {
		if _, ok := seen[normalize(value)]; ok {
			return true
		}
	}
	return false
}

func disjointExplicit(a, b []string) bool {
	return hasNonempty(a) && hasNonempty(b) && !overlaps(a, b)
}

func disjointExplicitNormalized(a, b []string) bool {
	return hasNonempty(a) && hasNonempty(b) && !overlapsNormalized(a, b)
}

func hasNonempty(values []string) bool { return firstUsable(values) != "" }

func firstUsable(values []string) string {
	for _, value := range values {
		if normalize(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func primaryLanguage(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func titleAbridgement(title string) Abridgement {
	if unabridgedPattern.MatchString(title) {
		return AbridgementUnabridged
	}
	if abridgedPattern.MatchString(title) {
		return AbridgementAbridged
	}
	return AbridgementUnknown
}

func exactRecordingMatch(a, b RecordingFacts) bool {
	return strings.TrimSpace(a.RecordingID) != "" && strings.EqualFold(strings.TrimSpace(a.RecordingID), strings.TrimSpace(b.RecordingID)) ||
		overlaps(a.ASINs, b.ASINs) || overlaps(a.ISBNs, b.ISBNs)
}

func evidenceScore(e Evidence) int {
	score := 0
	for _, matched := range []bool{e.MatchedASIN, e.MatchedISBN, e.MatchedNarrator, e.MatchedLanguage, e.RuntimeCompatible, e.AbridgementMatch} {
		if matched {
			score++
		}
	}
	return score
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
