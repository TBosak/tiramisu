package staticwiring

// Requirement I5: internal/musicimport, its config meanings/defaults, and its
// scheduler job remain byte-for-byte untouched by the audiobook-scheduler
// slice. This is a real regression gate, not a placeholder: testdata/
// musicimport.sha256 is a snapshot of every internal/musicimport/*.go file's
// sha256 taken before this slice's implementation work began. If the lead's
// production change (in any package) modifies, adds, or removes a file under
// internal/musicimport, this test fails - it does not merely assert the
// package is unimported.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMusicImportPackage_RemainsByteForByteUnchanged(t *testing.T) {
	root := repoRoot(t)
	musicDir := filepath.Join(root, "internal", "musicimport")

	entries, err := os.ReadDir(musicDir)
	if err != nil {
		t.Fatalf("read internal/musicimport: %v", err)
	}
	got := make(map[string]string, len(entries))
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		sum, err := sha256File(filepath.Join(musicDir, e.Name()))
		if err != nil {
			t.Fatalf("hash %s: %v", e.Name(), err)
		}
		got[e.Name()] = sum
		names = append(names, e.Name())
	}
	sort.Strings(names)

	want := readSnapshot(t, filepath.Join(root, "internal", "staticwiring", "testdata", "musicimport.sha256"))

	var wantNames []string
	for name := range want {
		wantNames = append(wantNames, name)
	}
	sort.Strings(wantNames)

	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("internal/musicimport/*.go file set changed:\n got:  %v\n want: %v", names, wantNames)
	}
	for _, name := range names {
		if got[name] != want[name] {
			t.Fatalf("internal/musicimport/%s changed (sha256 %s, want %s) - this slice must not modify internal/musicimport", name, got[name], want[name])
		}
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readSnapshot parses the "sha256sum -b"-style lines this package's testdata
// was generated with: "<hex sum> *<filename>".
func readSnapshot(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("read snapshot %s: %v", path, err)
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " *", 2)
		if len(fields) != 2 {
			t.Fatalf("malformed snapshot line: %q", line)
		}
		result[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan snapshot %s: %v", path, err)
	}
	return result
}
