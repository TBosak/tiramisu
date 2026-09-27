package audiobookimport

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var (
	partPattern  = regexp.MustCompile(`(?i)\bpart[\s._-]*(\d+)\b`)
	discPattern  = regexp.MustCompile(`(?i)\bdisc[\s._-]*(\d+)\b`)
	trackPattern = regexp.MustCompile(`(?i)\btrack[\s._-]*(\d+)\b`)
)

type orderedAudio struct {
	file        AudioFile
	disc, track int
	part        int
	ext, root   string
}

func coherentFiles(files []AudioFile) ([]SelectedFile, bool) {
	seenIndexes := make(map[int]struct{}, len(files))
	audio := make([]orderedAudio, 0, len(files))
	for _, file := range files {
		if _, duplicate := seenIndexes[file.Index]; duplicate {
			return nil, false
		}
		seenIndexes[file.Index] = struct{}{}
		if !safeSourcePath(file.Path) {
			return nil, false
		}
		normalPath := strings.ReplaceAll(file.Path, "\\", "/")
		ext := strings.ToLower(path.Ext(normalPath))
		if ext != ".m4b" && ext != ".m4a" && ext != ".mp3" {
			continue
		}
		if file.Size <= 0 {
			continue
		}
		if containsAnyWord(normalize(path.Base(normalPath)), "sample", "preview", "bonus", "interview", "podcast") {
			return nil, false
		}
		audio = append(audio, orderedAudio{
			file: file, ext: ext, root: editionRoot(file.Path),
			part:  capturedNumber(partPattern, file.Path),
			disc:  capturedNumber(discPattern, file.Path),
			track: capturedNumber(trackPattern, file.Path),
		})
	}
	if len(audio) == 0 {
		return nil, false
	}
	if len(audio) == 1 {
		return []SelectedFile{{FileIndex: audio[0].file.Index, Part: 0}}, true
	}

	root, ext := audio[0].root, audio[0].ext
	for _, item := range audio[1:] {
		if item.root != root || item.ext != ext {
			return nil, false
		}
	}
	if ext == ".m4b" {
		return nil, false
	}

	multiDisc := true
	for _, item := range audio {
		if item.disc <= 0 || item.track <= 0 {
			multiDisc = false
			break
		}
	}
	if multiDisc {
		sort.Slice(audio, func(i, j int) bool {
			if audio[i].disc != audio[j].disc {
				return audio[i].disc < audio[j].disc
			}
			return audio[i].track < audio[j].track
		})
		lastDisc, lastTrack := 0, 0
		for _, item := range audio {
			if item.disc != lastDisc {
				if item.disc != lastDisc+1 || item.track != 1 {
					return nil, false
				}
				lastDisc, lastTrack = item.disc, 0
			}
			if item.track != lastTrack+1 {
				return nil, false
			}
			lastTrack = item.track
		}
	} else {
		for _, item := range audio {
			if item.part <= 0 {
				return nil, false
			}
		}
		sort.Slice(audio, func(i, j int) bool { return audio[i].part < audio[j].part })
		for i, item := range audio {
			if item.part != i+1 {
				return nil, false
			}
		}
	}

	selected := make([]SelectedFile, len(audio))
	for i, item := range audio {
		selected[i] = SelectedFile{FileIndex: item.file.Index, Part: i + 1}
	}
	return selected, true
}

func capturedNumber(pattern *regexp.Regexp, value string) int {
	match := pattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(match[1])
	return n
}

func editionRoot(value string) string {
	dir := path.Dir(strings.ReplaceAll(value, "\\", "/"))
	if dir == "." {
		return ""
	}
	parts := strings.Split(dir, "/")
	if len(parts) > 0 && discPattern.MatchString(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return normalize(strings.Join(parts, "/"))
}

func safeSourcePath(value string) bool {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" || strings.HasPrefix(value, "/") || hasDrivePrefix(value) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || containsControl(component) {
			return false
		}
	}
	return true
}

func containsAnyWord(normalized string, words ...string) bool {
	for _, field := range strings.Fields(normalized) {
		for _, word := range words {
			if field == word {
				return true
			}
		}
	}
	return false
}

func containerScore(decision Decision) int {
	if len(decision.Selected) == 0 {
		return 0
	}
	file, ok := findFile(decision.Candidate.Files, decision.Selected[0].FileIndex)
	if !ok {
		return 0
	}
	switch strings.ToLower(path.Ext(file.Path)) {
	case ".m4b":
		return 3
	case ".m4a":
		return 2
	case ".mp3":
		return 1
	}
	return 0
}

func findFile(files []AudioFile, index int) (AudioFile, bool) {
	for _, file := range files {
		if file.Index == index {
			return file, true
		}
	}
	return AudioFile{}, false
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z'))
}
