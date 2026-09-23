package utils

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const multipartFileCount = 32

// memFile adapts an in-memory buffer to multipart.File.
type memFile struct{ *bytes.Reader }

func (memFile) Close() error { return nil }

// fixture is a deterministic multipart metainfo plus the values ParseFile,
// the HTTP branch and the file branch must keep reporting unchanged.
type fixture struct {
	raw       []byte
	infoBytes []byte
	infoHash  metainfo.Hash
	name      string
	trackers  []string // flattened, in declaration order
}

// buildFixture bencodes a 32-file multipart torrent. urlList is written as a
// bencode string when scalar is true (BEP 19 single-string form) and as a
// list otherwise; a nil urlList omits the key entirely.
func buildFixture(t testing.TB, urlList []string, scalar bool, announce string, announceList [][]string) fixture {
	t.Helper()
	const pieceLen = 16384
	info := metainfo.Info{
		Name:        "Example Audiobook",
		PieceLength: pieceLen,
	}
	for i := 0; i < multipartFileCount; i++ {
		info.Files = append(info.Files, metainfo.FileInfo{
			Length: pieceLen,
			Path:   []string{fmt.Sprintf("Part %02d.mp3", i+1)},
		})
	}
	info.Pieces = bytes.Repeat([]byte{0xAB}, 20*multipartFileCount)
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}

	top := map[string]interface{}{"info": bencode.Bytes(infoBytes)}
	var trackers []string
	if announce != "" {
		top["announce"] = announce
		trackers = append(trackers, announce)
	}
	if announceList != nil {
		// A non-empty announce-list overrides the legacy announce key.
		trackers = nil
		top["announce-list"] = announceList
		for _, tier := range announceList {
			for _, tr := range tier {
				dup := false
				for _, have := range trackers {
					dup = dup || have == tr
				}
				if !dup {
					trackers = append(trackers, tr)
				}
			}
		}
	}
	if urlList != nil {
		if scalar {
			if len(urlList) != 1 {
				t.Fatalf("scalar fixture needs exactly one url")
			}
			top["url-list"] = urlList[0]
		} else {
			top["url-list"] = urlList
		}
	}
	raw, err := bencode.Marshal(top)
	if err != nil {
		t.Fatalf("marshal metainfo: %v", err)
	}
	sum := sha1.Sum(infoBytes)
	return fixture{
		raw:       raw,
		infoBytes: infoBytes,
		infoHash:  metainfo.Hash(sum),
		name:      info.Name,
		trackers:  trackers,
	}
}

func flatten(tiers [][]string) []string {
	var out []string
	for _, tier := range tiers {
		out = append(out, tier...)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// assertWebseeds checks the exact ordered webseed list, duplicates included.
func assertWebseeds(t *testing.T, spec *torrent.TorrentSpec, want []string) {
	t.Helper()
	if !sameStrings(spec.Webseeds, want) {
		t.Errorf("Webseeds = %q, want %q", spec.Webseeds, want)
	}
}

// assertMetainfoTuple checks the complete compatibility tuple of a metainfo
// ingestion: info hash, display name, info bytes and trackers.
func assertMetainfoTuple(t *testing.T, spec *torrent.TorrentSpec, fx fixture) {
	t.Helper()
	if spec.InfoHash != fx.infoHash {
		t.Errorf("InfoHash = %s, want %s", spec.InfoHash, fx.infoHash)
	}
	if spec.DisplayName != fx.name {
		t.Errorf("DisplayName = %q, want %q", spec.DisplayName, fx.name)
	}
	if !bytes.Equal(spec.InfoBytes, fx.infoBytes) {
		t.Errorf("InfoBytes differ from the fixture info dictionary (%d vs %d bytes)", len(spec.InfoBytes), len(fx.infoBytes))
	}
	if !sameStrings(flatten(spec.Trackers), fx.trackers) {
		t.Errorf("Trackers = %q, want %q", spec.Trackers, fx.trackers)
	}
}

// ingestion entry points that all consume the same metainfo bytes.
type entry struct {
	name  string
	parse func(t *testing.T, raw []byte) (*torrent.TorrentSpec, error)
}

func entries() []entry {
	return []entry{
		{"ParseFile multipart", func(t *testing.T, raw []byte) (*torrent.TorrentSpec, error) {
			return ParseFile(memFile{bytes.NewReader(raw)})
		}},
		{"ParseLink http", func(t *testing.T, raw []byte) (*torrent.TorrentSpec, error) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/x-bittorrent")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			spec, err := ParseLink(srv.URL + "/audiobook.torrent")
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("test server received %d requests, want exactly 1", got)
			}
			return spec, err
		}},
		{"ParseLink file", func(t *testing.T, raw []byte) (*torrent.TorrentSpec, error) {
			return ParseLink(fileURL(t, raw))
		}},
	}
}

func fileURL(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audiobook.torrent")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

var (
	seedA = "https://archive.example/download/book/Part%2001.mp3"
	seedB = "https://mirror.example/dl/book/"
	seedC = "http://cdn.example/z/a b.mp3"
)

// A1 + A2 + A3 + E1: every entry point returns every declared url-list entry,
// in declaration order, for both BEP 19 encodings.
func TestParseMetainfo_Webseeds_AllEntryPoints(t *testing.T) {
	cases := []struct {
		name    string
		urlList []string
		scalar  bool
	}{
		{"E1 scalar byte-string url-list yields one webseed", []string{seedA}, true},
		{"E1 list url-list yields all entries", []string{seedA, seedB, seedC}, false},
		{"A1 declaration order is not sorted", []string{seedC, seedA, seedB}, false},
		{"A1 duplicate entries are preserved in place", []string{seedB, seedA, seedB, seedB}, false},
		{"E1 single-element list is equivalent to scalar", []string{seedA}, false},
		{"A1 percent-escapes in url-list are passed through verbatim", []string{"https://h.example/a%2Fb%2520c"}, false},
	}
	for _, e := range entries() {
		for _, tc := range cases {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				fx := buildFixture(t, tc.urlList, tc.scalar, "http://tracker.example/announce", nil)
				spec, err := e.parse(t, fx.raw)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				assertWebseeds(t, spec, tc.urlList)
				assertMetainfoTuple(t, spec, fx)
			})
		}
	}
}

// A4 + E2: repeated magnet ws parameters, decoded exactly once, ordered,
// duplicates preserved.
func TestParseLink_MagnetWebseeds(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		link string
		want []string
	}{
		{
			"A4 repeated ws keep parameter order",
			"magnet:?xt=urn:btih:" + hash + "&ws=https%3A%2F%2Fb.example%2F&ws=https%3A%2F%2Fa.example%2F&ws=https%3A%2F%2Fc.example%2F",
			[]string{"https://b.example/", "https://a.example/", "https://c.example/"},
		},
		{
			"A4 single ws yields one webseed",
			"magnet:?xt=urn:btih:" + hash + "&ws=https%3A%2F%2Fa.example%2Fbook.mp3",
			[]string{"https://a.example/book.mp3"},
		},
		{
			"E2 percent-encoded value is decoded exactly once",
			"magnet:?xt=urn:btih:" + hash + "&ws=https%3A%2F%2Fa.example%2Fdir%2520name%2FPart%252001.mp3",
			[]string{"https://a.example/dir%20name/Part%2001.mp3"},
		},
		{
			"E2 double-encoded octet stays single-decoded not fully decoded",
			"magnet:?xt=urn:btih:" + hash + "&ws=https%3A%2F%2Fa.example%2F%2525",
			[]string{"https://a.example/%25"},
		},
		{
			"E2 duplicates are neither deduplicated nor reordered",
			"magnet:?xt=urn:btih:" + hash + "&ws=https%3A%2F%2Fa.example%2F&ws=https%3A%2F%2Fb.example%2F&ws=https%3A%2F%2Fa.example%2F&ws=https%3A%2F%2Fa.example%2F",
			[]string{"https://a.example/", "https://b.example/", "https://a.example/", "https://a.example/"},
		},
		{
			"A4 ws interleaved with other parameters keeps ws order",
			"magnet:?ws=https%3A%2F%2F1.example%2F&xt=urn:btih:" + hash + "&dn=Book&ws=https%3A%2F%2F2.example%2F&tr=http%3A%2F%2Ft.example%2Fannounce&ws=https%3A%2F%2F3.example%2F",
			[]string{"https://1.example/", "https://2.example/", "https://3.example/"},
		},
		{
			"E2 unencoded slashes and colons in ws are accepted",
			"magnet:?xt=urn:btih:" + hash + "&ws=https://a.example/x/y.mp3",
			[]string{"https://a.example/x/y.mp3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseLink(tc.link)
			if err != nil {
				t.Fatalf("ParseLink: %v", err)
			}
			assertWebseeds(t, spec, tc.want)
		})
	}
}

// I2: adding magnet webseeds must not change hash, name or trackers.
func TestParseLink_MagnetWebseeds_CompatibilityTuple(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	wantHash := metainfo.NewHashFromHex(hash)
	const tr1, tr2 = "http://t1.example/announce", "udp://t2.example:6969"
	link := "magnet:?xt=urn:btih:" + hash +
		"&dn=My+Book&tr=" + url.QueryEscape(tr1) + "&tr=" + url.QueryEscape(tr2) +
		"&ws=https%3A%2F%2Fa.example%2F&ws=https%3A%2F%2Fa.example%2F"
	spec, err := ParseLink(link)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	assertWebseeds(t, spec, []string{"https://a.example/", "https://a.example/"})
	if spec.InfoHash != wantHash {
		t.Errorf("InfoHash = %s, want %s", spec.InfoHash, wantHash)
	}
	if spec.DisplayName != "My Book" {
		t.Errorf("DisplayName = %q, want %q", spec.DisplayName, "My Book")
	}
	if want := [][]string{{tr1, tr2}}; !reflect.DeepEqual(spec.Trackers, want) {
		t.Errorf("Trackers = %q, want %q", spec.Trackers, want)
	}
	if len(spec.InfoBytes) != 0 {
		t.Errorf("InfoBytes = %d bytes, want none for a magnet", len(spec.InfoBytes))
	}
}

// I1: webseed preservation leaves the whole compatibility tuple untouched,
// including with announce-list tiers and with webseeds present.
func TestParseMetainfo_CompatibilityTuple_WithWebseeds(t *testing.T) {
	list := [][]string{{"http://t1.example/announce", "http://t2.example/announce"}, {"http://t3.example/announce", "http://t1.example/announce"}}
	fx := buildFixture(t, []string{seedA, seedB}, false, "http://legacy.example/announce", list)
	for _, e := range entries() {
		t.Run(e.name, func(t *testing.T) {
			spec, err := e.parse(t, fx.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			assertMetainfoTuple(t, spec, fx)
			assertWebseeds(t, spec, []string{seedA, seedB})
		})
	}
}

// I3: inputs without webseeds still yield an empty list and unchanged tuple.
func TestParse_NoWebseeds(t *testing.T) {
	for name, urlList := range map[string][]string{
		"metainfo without url-list":    nil,
		"metainfo with empty url-list": {},
	} {
		t.Run(name, func(t *testing.T) {
			fx := buildFixture(t, urlList, false, "http://tracker.example/announce", nil)
			for _, e := range entries() {
				t.Run(e.name, func(t *testing.T) {
					spec, err := e.parse(t, fx.raw)
					if err != nil {
						t.Fatalf("parse: %v", err)
					}
					assertMetainfoTuple(t, spec, fx)
					assertWebseeds(t, spec, nil)
				})
			}
		})
	}
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, link := range []string{
		"magnet:?xt=urn:btih:" + hash,
		"magnet:?xt=urn:btih:" + hash + "&dn=Plain&tr=http%3A%2F%2Ft.example%2Fannounce",
		hash, // bare hash falls back to the magnet branch
	} {
		t.Run("link "+link, func(t *testing.T) {
			spec, err := ParseLink(link)
			if err != nil {
				t.Fatalf("ParseLink: %v", err)
			}
			assertWebseeds(t, spec, nil)
			if spec.InfoHash != metainfo.NewHashFromHex(hash) {
				t.Errorf("InfoHash = %s, want %s", spec.InfoHash, hash)
			}
		})
	}
}

// E3: malformed metainfo and unsupported schemes keep failing, without any
// request being attempted for the unsupported scheme.
func TestParse_ErrorsRetained(t *testing.T) {
	good := buildFixture(t, []string{seedA}, false, "http://tracker.example/announce", nil)
	// Valid bencode whose info value is not a dictionary.
	badInfo, err := bencode.Marshal(map[string]interface{}{"info": "not-a-dict", "url-list": []string{seedA}})
	if err != nil {
		t.Fatal(err)
	}
	malformed := map[string][]byte{
		"non-bencode garbage":        []byte("this is not bencode at all"),
		"empty body":                 {},
		"truncated metainfo":         good.raw[:len(good.raw)/2],
		"info is not a dictionary":   badInfo,
		"unterminated url-list dict": []byte("d4:infod4:name1:xe8:url-listl3:abc"),
	}
	for _, e := range entries() {
		for name, raw := range malformed {
			t.Run(e.name+"/"+name, func(t *testing.T) {
				spec, err := e.parse(t, raw)
				if err == nil {
					t.Fatalf("expected an error, got spec %+v", spec)
				}
				if spec != nil {
					t.Errorf("spec = %+v, want nil alongside error", spec)
				}
			})
		}
	}

	t.Run("http non-200 status keeps its status error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))
		defer srv.Close()
		spec, err := ParseLink(srv.URL + "/x.torrent")
		if err == nil || spec != nil {
			t.Fatalf("got (%v, %v), want (nil, error)", spec, err)
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error %q does not carry the 404 status", err)
		}
	})

	t.Run("missing file keeps failing", func(t *testing.T) {
		missing := fileURL(t, good.raw) + ".missing"
		spec, err := ParseLink(missing)
		if err == nil || spec != nil {
			t.Fatalf("got (%v, %v), want (nil, error)", spec, err)
		}
	})

	t.Run("invalid magnet keeps failing", func(t *testing.T) {
		spec, err := ParseLink("magnet:?dn=NoInfoHash&ws=https%3A%2F%2Fa.example%2F")
		if err == nil || spec != nil {
			t.Fatalf("got (%v, %v), want (nil, error)", spec, err)
		}
	})

	for _, scheme := range []string{"ftp", "gopher", "ws", "data"} {
		t.Run("unsupported scheme "+scheme, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
			}))
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "http://")
			spec, err := ParseLink(scheme + "://" + host + "/book.torrent")
			if err == nil || spec != nil {
				t.Fatalf("got (%v, %v), want (nil, error)", spec, err)
			}
			if want := "unknown scheme: " + scheme; err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("server received %d requests, want 0", got)
			}
		})
	}
}

// Results of successive parses must not share or leak webseed state.
func TestParseFile_NoStateLeakBetweenCalls(t *testing.T) {
	first := buildFixture(t, []string{seedA, seedB}, false, "http://tracker.example/announce", nil)
	second := buildFixture(t, nil, false, "http://tracker.example/announce", nil)

	spec1, err := ParseFile(memFile{bytes.NewReader(first.raw)})
	if err != nil {
		t.Fatal(err)
	}
	spec2, err := ParseFile(memFile{bytes.NewReader(second.raw)})
	if err != nil {
		t.Fatal(err)
	}
	assertWebseeds(t, spec1, []string{seedA, seedB})
	assertWebseeds(t, spec2, nil)
}
