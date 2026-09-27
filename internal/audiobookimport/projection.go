package audiobookimport

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var errUnsafeProjection = errors.New("unsafe audiobook projection input")

func PlanProjection(work WorkFacts, decisions []Decision) ([]ProjectedFile, error) {
	author, err := safeComponent(firstUsable(work.Authors))
	if err != nil {
		return nil, err
	}
	title, err := safeComponent(work.Title)
	if err != nil {
		return nil, err
	}
	components := []string{author}
	if strings.TrimSpace(work.Series) != "" {
		series, seriesErr := safeComponent(work.Series)
		if seriesErr != nil {
			return nil, seriesErr
		}
		components = append(components, series)
	}
	if strings.TrimSpace(work.Volume) != "" {
		volume, volumeErr := safeComponent(work.Volume)
		if volumeErr != nil {
			return nil, volumeErr
		}
		title = volume + " - " + title
	}

	result := make([]ProjectedFile, 0)
	for decisionIndex, decision := range decisions {
		if !decision.Eligible || len(decision.Selected) == 0 {
			return nil, errUnsafeProjection
		}
		bookDir := title
		if len(decisions) > 1 {
			disambiguator, disambiguatorErr := safeComponent(recordingDisambiguator(decision.Candidate.Recording))
			if disambiguatorErr != nil {
				return nil, disambiguatorErr
			}
			bookDir += " [" + disambiguator + "]"
		}
		for _, selected := range decision.Selected {
			source, found := findFile(decision.Candidate.Files, selected.FileIndex)
			if !found {
				return nil, errUnsafeProjection
			}
			ext := strings.ToLower(path.Ext(strings.ReplaceAll(source.Path, "\\", "/")))
			if ext != ".m4b" && ext != ".m4a" && ext != ".mp3" {
				return nil, errUnsafeProjection
			}
			filename := title + ext
			if len(decision.Selected) > 1 {
				filename = fmt.Sprintf("Part %03d%s", selected.Part, ext)
			}
			projected := path.Join(append(components, bookDir, filename)...)
			if !safeRelativePath(projected) {
				return nil, errUnsafeProjection
			}
			result = append(result, ProjectedFile{DecisionIndex: decisionIndex, FileIndex: selected.FileIndex, Path: projected})
		}
	}
	return result, nil
}

func recordingDisambiguator(recording RecordingFacts) string {
	if value := firstUsable(recording.Narrators); value != "" {
		return value
	}
	if value := strings.TrimSpace(recording.RecordingID); value != "" {
		return value
	}
	if value := firstUsable(recording.ASINs); value != "" {
		return value
	}
	return firstUsable(recording.ISBNs)
}

func safeComponent(value string) (string, error) {
	value = norm.NFC.String(strings.TrimSpace(value))
	var b strings.Builder
	space := false
	for _, r := range value {
		switch {
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`/\\:*?"<>|`, r):
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	result := strings.Trim(strings.Join(strings.Fields(b.String()), " "), ". ")
	for strings.Contains(result, "..") {
		result = strings.ReplaceAll(result, "..", ".")
	}
	result = strings.Trim(result, ". ")
	if result == "" || hasDrivePrefix(result) {
		return "", errUnsafeProjection
	}
	return result, nil
}

func safeRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") || hasDrivePrefix(value) || strings.Contains(value, "..") || containsControl(value) {
		return false
	}
	for _, component := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}
