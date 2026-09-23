package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
	"tiramisu/internal/vfs"
)

const audiobookStartupGuard = 5 * time.Second

type startupCompletionLog struct {
	mu   sync.Mutex
	done chan struct{}
	once sync.Once
}

func newStartupCompletionLog() *startupCompletionLog {
	return &startupCompletionLog{done: make(chan struct{})}
}

func (w *startupCompletionLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// This is the last log emitted after the builder's final globalInodeMap read.
	// Each fixture registers one deterministic ghost so the log is guaranteed.
	if bytes.Contains(p, []byte("Startup GC: Pruned")) {
		w.once.Do(func() { close(w.done) })
	}
	return len(p), nil
}

func waitStartupCompletion(t *testing.T, done <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(audiobookStartupGuard)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("startup cache builder did not finish after the FIFO was released")
	}
}

// waitForFIFOReader is a filesystem handshake, not a timing guess. A nonblocking
// writer can open a FIFO only after the startup reader has entered its blocking open.
func waitForFIFOReader(t *testing.T, path string) *os.File {
	t.Helper()
	timer := time.NewTimer(audiobookStartupGuard)
	defer timer.Stop()
	for {
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return writer
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("open FIFO writer: %v", err)
		}
		select {
		case <-timer.C:
			t.Fatal("movie metadata reader never reached the FIFO")
		default:
			runtime.Gosched()
		}
	}
}

func releaseBlockedMovie(t *testing.T, writer *os.File) {
	t.Helper()
	if _, err := writer.Write([]byte(jsonStub(streamURL(hashV1, 1), 250_000_000))); err != nil {
		t.Fatalf("write movie metadata to FIFO: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close movie FIFO writer: %v", err)
	}
}

func audiobookRegistryRow(path, hash string, index int, size int64) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:         string(library.SectionAudiobooks),
		VirtualPath:     path,
		PortablePathKey: library.PortablePathKey(path),
		Hash:            hash,
		FileIndex:       index,
		SourcePath:      fmt.Sprintf("release/source-%02d%s", index, filepath.Ext(path)),
		Size:            size,
		MtimeNS:         rowMtime.Add(time.Duration(index) * time.Second).UnixNano(),
		Title:           "Lazy audiobook fixture",
		Magnet:          "magnet:?xt=urn:btih:" + hash,
	}
}

func commitAudiobookRows(t *testing.T, db *metadb.DB, root string, rows []metadb.AudioProjection) {
	t.Helper()
	for _, row := range rows {
		writeFile(t, filepath.Join(root, row.Section, filepath.FromSlash(row.VirtualPath)),
			jsonStub(streamURL(row.Hash, row.FileIndex), row.Size), time.Unix(0, row.MtimeNS))
	}
	if len(rows) == 0 {
		return
	}
	if err := db.StageAudioProjections("lazy-audiobook-startup", rows); err != nil {
		t.Fatalf("stage audiobook rows: %v", err)
	}
	if changed, err := db.CommitAudioProjections("lazy-audiobook-startup", rowMtime.UnixNano()); err != nil || changed != len(rows) {
		t.Fatalf("commit audiobook rows = (%d, %v), want (%d, nil)", changed, err, len(rows))
	}
}

// L1, L2, L4, L5, I1, I2, E1: audio reaches a terminal, coherent namespace
// before Start returns. A movie metadata read that is demonstrably blocked cannot
// leave audio Unreconciled, whether the registry is populated, empty, or failed.
func TestStartupCacheBuilder_AudioTerminalBeforeBlockedMovieCache(t *testing.T) {
	bookHash := "4444555566667777888899990000111122223333"
	rows := []metadb.AudioProjection{
		audiobookRegistryRow("Author/Single Book/Single Book_22223333.m4b", bookHash, 4, 81_000_001),
		audiobookRegistryRow("Author/Multi Book/Disc 01/01 - Opening_22223333.mp3", bookHash, 8, 21_000_001),
		audiobookRegistryRow("Author/Multi Book/Disc 01/02 - Continue_22223333.mp3", bookHash, 9, 22_000_001),
	}

	tests := []struct {
		name      string
		rows      []metadb.AudioProjection
		closeDB   bool
		wantState library.NamespaceState
	}{
		{name: "M4B and multipart MP3 registry is Ready", rows: rows, wantState: library.Ready},
		{name: "empty committed audiobook set is Ready", wantState: library.Ready},
		{name: "registry failure is Failed", closeDB: true, wantState: library.Failed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			e.inodes.AddFile(filepath.Join(e.root, "movies", "Ghost_aaaabbbb.mkv"), hashC, 99)
			previousDB := stateDB
			db, err := metadb.New(filepath.Join(t.TempDir(), "state.db"), log.New(os.Stderr, "", 0))
			if err != nil {
				t.Fatalf("open state DB: %v", err)
			}
			t.Cleanup(func() {
				stateDB = previousDB
				if !tc.closeDB {
					_ = db.Close()
				}
			})
			commitAudiobookRows(t, db, e.root, tc.rows)
			if tc.closeDB {
				if err := db.Close(); err != nil {
					t.Fatalf("close state DB fixture: %v", err)
				}
			}
			stateDB = db

			moviePath := filepath.Join(e.root, "movies", "Blocked_11112222.mkv")
			if err := os.MkdirAll(filepath.Dir(moviePath), 0o755); err != nil {
				t.Fatalf("create movies directory: %v", err)
			}
			if err := syscall.Mkfifo(moviePath, 0o600); err != nil {
				t.Fatalf("create movie metadata FIFO: %v", err)
			}

			completion := newStartupCompletionLog()
			builder := NewStartupCacheBuilder(e.root, metaCache, log.New(completion, "", 0))
			before := startupReadSnapshot()
			builder.Start()

			writer := waitForFIFOReader(t, moviePath)
			if got := e.ns.State(); got != tc.wantState {
				t.Errorf("namespace state while movie metadata is blocked = %v, want %v before Start returns", got, tc.wantState)
			}
			if tc.wantState == library.Ready {
				if got := e.ns.Len(); got != len(tc.rows) {
					t.Errorf("ready namespace entries = %d, want %d committed rows", got, len(tc.rows))
				}
				for _, row := range tc.rows {
					got, ok := e.ns.Lookup(library.SectionAudiobooks, row.VirtualPath)
					if !ok || got.Hash != row.Hash || got.FileIndex != row.FileIndex {
						t.Errorf("Lookup(%q) = (%#v, %v), want hash %s index %d", row.VirtualPath, got, ok, row.Hash, row.FileIndex)
					}
					startupReadRequireNoState(t, filepath.Join(e.root, row.Section, filepath.FromSlash(row.VirtualPath)), before)
				}
			}
			if _, cached := metaCache.Get(moviePath); cached {
				t.Error("blocked movie metadata unexpectedly reached the cache")
			}

			releaseBlockedMovie(t, writer)
			waitStartupCompletion(t, completion.done)
		})
	}
}

func audiobookWakeCall(f *startupWakeFake, index int) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[index].magnet, f.calls[index].index
}

// L2-L4 and I2: startup publication, Readdir, Lookup, and Getattr are metadata
// only. Open is the first activation boundary and receives the exact registered
// torrent identity for both a whole-file M4B and every part of a multipart MP3.
func TestAudiobookMetadataIsLazyUntilExactOpen(t *testing.T) {
	e := newVFSEnv(t)
	startupReadConfigureReaderGlobals(t)
	hash := "7777888899990000111122223333444455556666"
	rows := []library.AudioProjection{
		{Section: library.SectionAudiobooks, VirtualPath: "Author/Single/Book_55556666.m4b", Hash: hash, FileIndex: 14, Size: 91_000_001, MtimeNS: rowMtime.UnixNano()},
		{Section: library.SectionAudiobooks, VirtualPath: "Author/Multi/Disc 01/01 - Opening_55556666.mp3", Hash: hash, FileIndex: 18, Size: 31_000_001, MtimeNS: rowMtime.Add(time.Second).UnixNano()},
		{Section: library.SectionAudiobooks, VirtualPath: "Author/Multi/Disc 01/02 - Continue_55556666.mp3", Hash: hash, FileIndex: 19, Size: 32_000_001, MtimeNS: rowMtime.Add(2 * time.Second).UnixNano()},
	}
	for _, row := range rows {
		writeFile(t, e.phys(row.Section, row.VirtualPath), jsonStub(streamURL(row.Hash, row.FileIndex), row.Size), time.Unix(0, row.MtimeNS))
	}
	e.publish(rows...)

	wantByDir := map[string][]string{
		"Author/Single":        {"Book_55556666.m4b"},
		"Author/Multi/Disc 01": {"01 - Opening_55556666.mp3", "02 - Continue_55556666.mp3"},
	}
	for dir, want := range wantByDir {
		got := readdirNames(nil, e.phys(library.SectionAudiobooks, dir))
		sort.Strings(want)
		if got.errno != 0 || !reflect.DeepEqual(got.names, want) {
			t.Errorf("Readdir(%q) = (%v, %v), want (%v, OK)", dir, got.names, got.errno, want)
		}
	}

	for _, row := range rows {
		row := row
		t.Run(filepath.Base(row.VirtualPath), func(t *testing.T) {
			physical := e.phys(row.Section, row.VirtualPath)
			before := startupReadSnapshot()
			wake := &startupWakeFake{}

			res := e.lookup(string(row.Section) + "/" + row.VirtualPath)
			if res.status != fuse.OK {
				t.Fatalf("Lookup status = %v, want OK", res.status)
			}
			attrs := e.getattr(t, res.nodeID)
			if int64(attrs.Size) != row.Size {
				t.Errorf("Getattr size = %d, want %d", attrs.Size, row.Size)
			}
			node := &VirtualMkvNode{vMeta: &vfs.Metadata{
				Path: physical, URL: streamURL(row.Hash, row.FileIndex), Size: row.Size,
				Mtime: time.Unix(0, row.MtimeNS), Audio: true,
			}, wake: wake.wake}
			var directAttrs fuse.AttrOut
			if errno := node.Getattr(context.Background(), nil, &directAttrs); errno != 0 {
				t.Fatalf("direct Getattr errno = %v, want OK", errno)
			}
			if wake.callCount() != 0 {
				t.Fatalf("metadata-only operations called Wake %d time(s), want 0", wake.callCount())
			}
			startupReadRequireNoState(t, physical, before)

			handle, errno := startupReadOpen(t, context.Background(), node)
			t.Cleanup(func() { startupReadClearPath(physical) })
			if errno != 0 || handle == nil {
				t.Fatalf("Open = (handle %T, errno %v), want a handle and OK", handle, errno)
			}
			if wake.callCount() != 1 {
				t.Fatalf("Wake calls after Open = %d, want 1", wake.callCount())
			}
			magnet, index := audiobookWakeCall(wake, 0)
			if magnet != "magnet:?xt=urn:btih:"+row.Hash || index != row.FileIndex {
				t.Errorf("Wake(%q, %d), want exact registered (%s, %d)", magnet, index, row.Hash, row.FileIndex)
			}
			if _, ok := playbackRegistry.Load(physical); !ok {
				t.Error("Open did not create playback state after successful activation")
			}
		})
	}
}
