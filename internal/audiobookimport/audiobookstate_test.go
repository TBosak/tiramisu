package audiobookimport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleStateEntry() StateEntry {
	return StateEntry{
		Namespace:          IdentityNamespaceAudioSiloRecording,
		ExternalID:         "rec-inglis-unabridged",
		ProjectedPaths:     []string{"J.R.R. Tolkien/The Fellowship of the Ring/The Fellowship of the Ring.m4b"},
		OwnedPrefix:        "J.R.R. Tolkien/The Fellowship of the Ring",
		SourceFingerprint:  "fp-deadbeef",
		ControllerOwned:    true,
		Stage:              StageScanned,
		ScanStatusCode:     200,
		ConsecutiveMissing: 2,
		MissingSince:       time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		AttemptCount:       3,
		LastReason:         "ok",
		UpdatedAt:          time.Now().UTC().Truncate(time.Second),
	}
}

func sampleState() State {
	return buildState(sampleStateEntry())
}

// TestAudiobookState_A6_LoadMissingFileReturnsFreshEmptyState covers A6: a
// store that has never been written must behave like a fresh, empty,
// current-version state rather than an error.
func TestAudiobookState_A6_LoadMissingFileReturnsFreshEmptyState(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(filepath.Join(dir, "state.json"))

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a never-written store", err)
	}
	if got.Version != CurrentStateVersion {
		t.Errorf("Version = %d, want %d", got.Version, CurrentStateVersion)
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %v, want empty", got.Entries)
	}
}

// TestAudiobookState_A6_SaveThenLoadRoundTrips covers A6: a saved state must
// be recoverable byte-for-byte in structure via Load.
func TestAudiobookState_A6_SaveThenLoadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)
	want := sampleState()

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if got.Version != want.Version {
		t.Errorf("Version = %d, want %d", got.Version, want.Version)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("Entries count = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for key, wantEntry := range want.Entries {
		gotEntry, ok := got.Entries[key]
		if !ok {
			t.Fatalf("Entries[%q] missing after round trip", key)
		}
		if gotEntry.Namespace != wantEntry.Namespace || gotEntry.ExternalID != wantEntry.ExternalID ||
			gotEntry.Stage != wantEntry.Stage || gotEntry.OwnedPrefix != wantEntry.OwnedPrefix ||
			gotEntry.ControllerOwned != wantEntry.ControllerOwned || gotEntry.ScanStatusCode != wantEntry.ScanStatusCode ||
			gotEntry.SourceFingerprint != wantEntry.SourceFingerprint ||
			gotEntry.AttemptCount != wantEntry.AttemptCount ||
			gotEntry.ConsecutiveMissing != wantEntry.ConsecutiveMissing ||
			!gotEntry.MissingSince.Equal(wantEntry.MissingSince) ||
			gotEntry.LastReason != wantEntry.LastReason {
			t.Errorf("Entries[%q] = %+v, want %+v", key, gotEntry, wantEntry)
		}
		if len(gotEntry.ProjectedPaths) != len(wantEntry.ProjectedPaths) {
			t.Errorf("Entries[%q].ProjectedPaths = %v, want %v", key, gotEntry.ProjectedPaths, wantEntry.ProjectedPaths)
		}
	}
}

// TestAudiobookState_A6_SerializedBytesAreOrderIndependent covers A6/I3:
// two logically identical states, built by inserting the same entries into
// the map in opposite order, must serialize to byte-identical output - the
// encoding must not leak Go's randomized map iteration order into the
// persisted (and potentially diffed/hashed) file.
func TestAudiobookState_A6_SerializedBytesAreOrderIndependent(t *testing.T) {
	entryA := sampleStateEntry()
	entryB := sampleStateEntry()
	entryB.Namespace, entryB.ExternalID = IdentityNamespaceAudioSiloWork, "work-two-towers-1"
	idA := ExternalIdentity{Namespace: entryA.Namespace, ID: entryA.ExternalID}
	idB := ExternalIdentity{Namespace: entryB.Namespace, ID: entryB.ExternalID}

	forward := State{Version: CurrentStateVersion, Entries: map[string]StateEntry{idA.Key(): entryA, idB.Key(): entryB}}
	reversed := State{Version: CurrentStateVersion, Entries: map[string]StateEntry{idB.Key(): entryB, idA.Key(): entryA}}

	dir := t.TempDir()
	pathForward := filepath.Join(dir, "forward.json")
	pathReversed := filepath.Join(dir, "reversed.json")
	if err := NewFileStateStore(pathForward).Save(context.Background(), forward); err != nil {
		t.Fatalf("Save(forward) error = %v, want nil", err)
	}
	if err := NewFileStateStore(pathReversed).Save(context.Background(), reversed); err != nil {
		t.Fatalf("Save(reversed) error = %v, want nil", err)
	}
	forwardBytes, err := os.ReadFile(pathForward)
	if err != nil {
		t.Fatalf("ReadFile(forward) error = %v", err)
	}
	reversedBytes, err := os.ReadFile(pathReversed)
	if err != nil {
		t.Fatalf("ReadFile(reversed) error = %v", err)
	}
	if string(forwardBytes) != string(reversedBytes) {
		t.Errorf("serialized bytes differ by map insertion order:\nforward:  %s\nreversed: %s", forwardBytes, reversedBytes)
	}
}

// TestAudiobookState_A6_SaveCreatesParentDirectory covers A6: Save must
// create any missing parent directory rather than failing.
func TestAudiobookState_A6_SaveCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "controller", "state.json")
	store := NewFileStateStore(path)

	if err := store.Save(context.Background(), sampleState()); err != nil {
		t.Fatalf("Save() error = %v, want nil (parent directories must be created)", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file not found after Save: %v", err)
	}
}

// TestAudiobookState_A6_SaveIsAtomicNoTempFileLeftBehind covers A6: the
// write-temp-then-replace strategy must leave no stray temp file in the
// directory once Save returns successfully.
func TestAudiobookState_A6_SaveIsAtomicNoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)

	if err := store.Save(context.Background(), sampleState()); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("stray file %q left in state directory after Save, want only %q", e.Name(), filepath.Base(path))
		}
	}
}

// TestAudiobookState_A6_SaveOverwritesPreviousContentCompletely covers A6:
// a second Save with fewer entries must fully replace the file, not merge
// with or append to the previous content (an implementation that merely
// appends would leave stale entries visible after Load).
func TestAudiobookState_A6_SaveOverwritesPreviousContentCompletely(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)

	first := sampleState()
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatalf("first Save() error = %v, want nil", err)
	}
	second := State{Version: CurrentStateVersion, Entries: map[string]StateEntry{}}
	if err := store.Save(context.Background(), second); err != nil {
		t.Fatalf("second Save() error = %v, want nil", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %v after overwriting with an empty state, want empty", got.Entries)
	}
}

// TestAudiobookState_A6_CorruptJSONFailsClosed covers A6: a file that is not
// valid JSON must fail Load rather than silently returning a zero/partial
// state.
func TestAudiobookState_A6_CorruptJSONFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store := NewFileStateStore(path)

	_, err := store.Load(context.Background())
	if err == nil {
		t.Fatalf("Load() error = nil, want an error for corrupt JSON")
	}
}

// TestAudiobookState_A6_FutureVersionFailsClosed covers A6: a state file
// whose version is newer than this build understands must fail Load rather
// than being (mis)interpreted.
func TestAudiobookState_A6_FutureVersionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := map[string]any{"version": CurrentStateVersion + 1, "entries": map[string]any{}}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store := NewFileStateStore(path)

	_, err = store.Load(context.Background())
	if err == nil {
		t.Fatalf("Load() error = nil, want an error for a future state version")
	}
}

// TestAudiobookState_A6_ConflictingEntryKeyFailsClosed covers the
// "duplicate/conflicting entries" adversarial case: an entry stored under a
// map key that does not match its own declared identity is an impossible
// saved state and must fail Load rather than being silently accepted under
// the wrong key.
func TestAudiobookState_A6_ConflictingEntryKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	entry := sampleStateEntry()
	raw := map[string]any{
		"version": CurrentStateVersion,
		"entries": map[string]any{
			"this-key-does-not-match-the-entrys-own-identity": entry,
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store := NewFileStateStore(path)

	_, err = store.Load(context.Background())
	if err == nil {
		t.Fatalf("Load() error = nil, want an error for a map key that conflicts with the entry's own identity")
	}
}

// TestAudiobookState_A6_JSONFieldNamesAreStable covers A6/I3: the encoding
// is deterministic and its keys are stable and portable across versions, not
// an accident of Go's default field-name casing.
func TestAudiobookState_A6_JSONFieldNamesAreStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)
	if err := store.Save(context.Background(), sampleState()); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{
		`"version"`, `"entries"`, `"namespace"`, `"external_id"`, `"projected_paths"`,
		`"owned_prefix"`, `"controller_owned"`, `"stage"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state JSON missing stable field %s; got: %s", want, raw)
		}
	}
}

// TestAudiobookState_A6_NoSecretFieldsInPersistedJSON covers A6/I5: even
// with a maximally populated entry, the persisted JSON must never contain a
// token, magnet, torrent bytes, raw download URL, or provider payload -
// StateEntry's declared field set has no such field, and this test pins
// that structural guarantee against silent additions.
func TestAudiobookState_A6_NoSecretFieldsInPersistedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)
	if err := store.Save(context.Background(), sampleState()); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, forbidden := range []string{"token", "magnet", "torrent_file", "download_url", "hash"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Errorf("persisted state JSON contains forbidden field/marker %q; got: %s", forbidden, raw)
		}
	}
}

// TestAudiobookState_A6_HonorsContextCancellation covers I1: Load/Save must
// respect an already-cancelled context rather than performing file I/O
// unconditionally.
func TestAudiobookState_A6_HonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewFileStateStore(path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.Load(ctx); err == nil {
		t.Errorf("Load() error = nil with a cancelled context, want an error")
	}
	if err := store.Save(ctx, sampleState()); err == nil {
		t.Errorf("Save() error = nil with a cancelled context, want an error")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("state file exists after a cancelled Save, want no file written")
	}
}

// TestPublication_A6_OversizedStateFailsBeforeCalls covers A6/E1: a loaded
// state exceeding the configured entry bound must fail before any Library
// call. The five entries below are each stored under their own valid,
// self-consistent key (Namespace/ExternalID -> ExternalIdentity.Key()), so
// a conflicting-key check (a distinct, narrower requirement) cannot be what
// rejects the very first entry and accidentally satisfy this test for the
// wrong reason; only the MaxEntries bound itself can be doing the work
// here.
func TestPublication_A6_OversizedStateFailsBeforeCalls(t *testing.T) {
	entries := make([]StateEntry, 5)
	for i := range entries {
		entries[i] = StateEntry{
			Namespace:  IdentityNamespaceAudioSiloWork,
			ExternalID: string(rune('a' + i)),
			Stage:      StageScanned,
		}
	}
	state := buildState(entries...)
	if len(state.Entries) != len(entries) {
		t.Fatalf("buildState produced %d entries from %d distinct identities, want %d: ExternalIdentity.Key() is not yet collision-free (see TestExternalIdentity_I3_KeyProperties)", len(state.Entries), len(entries), len(entries))
	}

	log := &callLog{}
	store := newFakeStore(state)
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)
	deps.Limits.MaxEntries = len(entries) - 1 // one fewer than what is already loaded

	req := basePublicationRequest()
	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error when the loaded state already exceeds MaxEntries")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
}

// TestPublication_A6_OversizedStringsFailBeforeCalls covers A6/I3: a
// persisted entry with a string field beyond the configured bound must fail
// before any Library call.
func TestPublication_A6_OversizedStringsFailBeforeCalls(t *testing.T) {
	id := ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: "work-fellowship-1"}
	oversized := strings.Repeat("x", 1<<20)
	log := &callLog{}
	store := newFakeStore(State{Version: CurrentStateVersion, Entries: map[string]StateEntry{
		id.Key(): {Namespace: id.Namespace, ExternalID: id.ID, Stage: StageScanned, LastReason: oversized},
	}})
	store.log = log
	pub := &scriptedPublisher{log: log}
	scan := &scriptedScanner{log: log}
	deps := publishDeps(store, pub, scan)
	deps.Limits.MaxStringLength = 64

	req := basePublicationRequest()
	req.Recording.RecordingID = "" // force fallback to the work id above so the loaded entry is addressed
	_, err := Publish(context.Background(), deps, req)
	if err == nil {
		t.Fatalf("Publish() error = nil, want an error when a persisted string field exceeds MaxStringLength")
	}
	if len(pub.calls) != 0 {
		t.Errorf("Publisher.Add called %d times, want 0", len(pub.calls))
	}
}

// --- FileOps-observed crash-safety tests ---------------------------------
//
// The tests below use the injected FileOps seam (NewFileStateStoreWithOps)
// to observe the write-temp -> file-sync -> atomic-rename -> parent-sync
// sequence deterministically, and to inject a failure at each meaningful
// step without real crash/fault injection. They are the A6 tests the prior
// review round flagged as needing a declaration-only seam; the seam is now
// in place (FileOps/NewFileStateStoreWithOps in publication.go), added
// verbatim by the lead with zero-default bodies, and is not modified here.

// fileOpCall is one recorded FileOps invocation, in call order.
type fileOpCall struct {
	op   string // "WriteFile", "Sync", "Rename", "SyncDir", "Remove"
	arg1 string // the path FileOps acted on (Rename's oldpath)
	arg2 string // Rename's newpath only; empty for every other op
}

// fakeFileOps is a deterministic, in-memory FileOps. Each of its five
// operations can be scripted to fail independently, so a test can isolate
// exactly one step of the write-temp -> sync -> rename -> syncdir sequence
// as the failure point and observe what FileStateStore.Save does in
// response - stop early, and clean up a temp file it already created, or
// not.
type fakeFileOps struct {
	calls []fileOpCall

	failWriteFile bool
	failSync      bool
	failRename    bool
	failSyncDir   bool
	failRemove    bool
}

func (f *fakeFileOps) WriteFile(name string, data []byte, perm os.FileMode) error {
	f.calls = append(f.calls, fileOpCall{op: "WriteFile", arg1: name})
	if f.failWriteFile {
		return errors.New("fakeFileOps: WriteFile failed")
	}
	return nil
}

func (f *fakeFileOps) Sync(name string) error {
	f.calls = append(f.calls, fileOpCall{op: "Sync", arg1: name})
	if f.failSync {
		return errors.New("fakeFileOps: Sync failed")
	}
	return nil
}

func (f *fakeFileOps) Rename(oldpath, newpath string) error {
	f.calls = append(f.calls, fileOpCall{op: "Rename", arg1: oldpath, arg2: newpath})
	if f.failRename {
		return errors.New("fakeFileOps: Rename failed")
	}
	return nil
}

func (f *fakeFileOps) SyncDir(dir string) error {
	f.calls = append(f.calls, fileOpCall{op: "SyncDir", arg1: dir})
	if f.failSyncDir {
		return errors.New("fakeFileOps: SyncDir failed")
	}
	return nil
}

func (f *fakeFileOps) Remove(name string) error {
	f.calls = append(f.calls, fileOpCall{op: "Remove", arg1: name})
	if f.failRemove {
		return errors.New("fakeFileOps: Remove failed")
	}
	return nil
}

func (f *fakeFileOps) opsSequence() []string {
	ops := make([]string, len(f.calls))
	for i, c := range f.calls {
		ops[i] = c.op
	}
	return ops
}

func (f *fakeFileOps) calledWith(op, arg1 string) bool {
	for _, c := range f.calls {
		if c.op == op && c.arg1 == arg1 {
			return true
		}
	}
	return false
}

// TestAudiobookState_A6_SaveOrdersWriteTempSyncRenameSyncDir covers A6: a
// successful Save must perform exactly write-temp, sync-temp, atomic
// rename, sync-parent-directory, in that order, and the temp file must live
// in the same directory as the destination (a cross-directory/cross-
// filesystem rename cannot be atomic) and be distinct from it.
func TestAudiobookState_A6_SaveOrdersWriteTempSyncRenameSyncDir(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{}
	store := NewFileStateStoreWithOps(finalPath, ops)

	if err := store.Save(context.Background(), sampleState()); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}
	wantOps := []string{"WriteFile", "Sync", "Rename", "SyncDir"}
	gotOps := ops.opsSequence()
	if len(gotOps) != len(wantOps) {
		t.Fatalf("FileOps call sequence = %v, want exactly %v", gotOps, wantOps)
	}
	for i, want := range wantOps {
		if gotOps[i] != want {
			t.Errorf("calls[%d].op = %q, want %q (full sequence: %v)", i, gotOps[i], want, gotOps)
		}
	}

	tempPath := ops.calls[0].arg1
	if tempPath == "" {
		t.Fatalf("WriteFile target is empty")
	}
	if tempPath == finalPath {
		t.Errorf("WriteFile target = %q, want a temp path distinct from the final destination", tempPath)
	}
	if filepath.Dir(tempPath) != filepath.Dir(finalPath) {
		t.Errorf("temp path %q is not in the destination's own directory %q; an atomic rename requires the same directory/filesystem", tempPath, filepath.Dir(finalPath))
	}
	if ops.calls[1].arg1 != tempPath {
		t.Errorf("Sync target = %q, want the just-written temp file %q", ops.calls[1].arg1, tempPath)
	}
	if ops.calls[2].arg1 != tempPath || ops.calls[2].arg2 != finalPath {
		t.Errorf("Rename = (%q -> %q), want (%q -> %q)", ops.calls[2].arg1, ops.calls[2].arg2, tempPath, finalPath)
	}
	if ops.calls[3].arg1 != filepath.Dir(finalPath) {
		t.Errorf("SyncDir target = %q, want the destination's parent directory %q", ops.calls[3].arg1, filepath.Dir(finalPath))
	}
}

// TestAudiobookState_A6_SaveWriteFailureStopsAndRemovesTempFile covers A6: a
// failure writing the temp file must stop immediately - Sync, Rename, and
// SyncDir must never run - but WriteFile can still have created or
// partially written the temp file before returning its error, so the
// orphaned temp file must be cleaned up with Remove exactly as it would be
// after a Sync or Rename failure.
func TestAudiobookState_A6_SaveWriteFailureStopsAndRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{failWriteFile: true}
	store := NewFileStateStoreWithOps(finalPath, ops)

	if err := store.Save(context.Background(), sampleState()); err == nil {
		t.Fatalf("Save() error = nil, want an error when the temp write fails")
	}
	for _, forbidden := range []string{"Sync", "Rename", "SyncDir"} {
		for _, c := range ops.calls {
			if c.op == forbidden {
				t.Errorf("%s was called after a failed WriteFile, want it never attempted; calls=%v", forbidden, ops.calls)
			}
		}
	}
	tempPath := ops.calls[0].arg1
	if !ops.calledWith("Remove", tempPath) {
		t.Errorf("no Remove(%q) cleanup call recorded after the write failure, want the possibly-partial temp file removed; calls=%v", tempPath, ops.calls)
	}
}

// TestAudiobookState_A6_SaveSyncFailureStopsAndRemovesTempFile covers A6: a
// failure syncing the freshly written temp file must stop before Rename or
// SyncDir (the unsynced temp file must never become the visible
// destination), and the orphaned temp file must be cleaned up with Remove.
func TestAudiobookState_A6_SaveSyncFailureStopsAndRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{failSync: true}
	store := NewFileStateStoreWithOps(finalPath, ops)

	if err := store.Save(context.Background(), sampleState()); err == nil {
		t.Fatalf("Save() error = nil, want an error when the temp-file sync fails")
	}
	for _, forbidden := range []string{"Rename", "SyncDir"} {
		for _, c := range ops.calls {
			if c.op == forbidden {
				t.Errorf("%s was called after a failed Sync, want it never attempted (an unsynced temp file must never become the destination); calls=%v", forbidden, ops.calls)
			}
		}
	}
	tempPath := ops.calls[0].arg1
	if !ops.calledWith("Remove", tempPath) {
		t.Errorf("no Remove(%q) cleanup call recorded after the sync failure, want the orphaned temp file removed; calls=%v", tempPath, ops.calls)
	}
}

// TestAudiobookState_A6_SaveRenameFailureStopsAndRemovesTempFile covers A6:
// a failure atomically replacing the destination must stop before
// SyncDir - the destination was never durably replaced, so syncing its
// parent directory would falsely vouch for a commit that never happened -
// and the orphaned temp file must be cleaned up.
func TestAudiobookState_A6_SaveRenameFailureStopsAndRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{failRename: true}
	store := NewFileStateStoreWithOps(finalPath, ops)

	if err := store.Save(context.Background(), sampleState()); err == nil {
		t.Fatalf("Save() error = nil, want an error when the atomic rename fails")
	}
	for _, c := range ops.calls {
		if c.op == "SyncDir" {
			t.Errorf("SyncDir was called despite a failed Rename, want it never attempted (the destination was never durably replaced); calls=%v", ops.calls)
		}
	}
	tempPath := ops.calls[0].arg1
	if !ops.calledWith("Remove", tempPath) {
		t.Errorf("no Remove(%q) cleanup call recorded after the rename failure, want the orphaned temp file removed; calls=%v", tempPath, ops.calls)
	}
}

// TestAudiobookState_A6_SaveReportsPrimaryFailureEvenWhenCleanupAlsoFails
// covers A6: when the best-effort Remove cleanup itself also fails after a
// write/sync/rename failure, Save must still report failure - a failed
// cleanup must never mask the primary failure or be treated as license to
// proceed. SyncDir must never run in any of these cases. For a write or
// sync primary failure, Rename must never run either. For a rename primary
// failure, Rename itself is the injected failure being tested and is
// expected to appear exactly once; nothing but a best-effort Remove may
// follow it.
func TestAudiobookState_A6_SaveReportsPrimaryFailureEvenWhenCleanupAlsoFails(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configure       func(*fakeFileOps)
		renameIsPrimary bool
	}{
		{name: "write_failure_cleanup_also_fails", configure: func(o *fakeFileOps) { o.failWriteFile = true; o.failRemove = true }},
		{name: "sync_failure_cleanup_also_fails", configure: func(o *fakeFileOps) { o.failSync = true; o.failRemove = true }},
		{name: "rename_failure_cleanup_also_fails", configure: func(o *fakeFileOps) { o.failRename = true; o.failRemove = true }, renameIsPrimary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			finalPath := filepath.Join(dir, "state.json")
			ops := &fakeFileOps{}
			tc.configure(ops)
			store := NewFileStateStoreWithOps(finalPath, ops)

			if err := store.Save(context.Background(), sampleState()); err == nil {
				t.Fatalf("Save() error = nil for %s, want an error (a failed cleanup must never mask the primary failure)", tc.name)
			}
			for _, c := range ops.calls {
				if c.op == "SyncDir" {
					t.Errorf("%s: SyncDir was called despite the primary failure, want it never attempted even when cleanup also failed; calls=%v", tc.name, ops.calls)
				}
			}

			if !tc.renameIsPrimary {
				for _, c := range ops.calls {
					if c.op == "Rename" {
						t.Errorf("%s: Rename was called despite the primary failure, want it never attempted; calls=%v", tc.name, ops.calls)
					}
				}
				return
			}

			// Rename is itself the injected primary failure here: it must
			// appear exactly once, and nothing may follow it except a
			// best-effort Remove cleanup attempt.
			renameSeq := -1
			for i, c := range ops.calls {
				if c.op != "Rename" {
					continue
				}
				if renameSeq != -1 {
					t.Errorf("%s: Rename was called more than once (again at index %d), want exactly 1; calls=%v", tc.name, i, ops.calls)
					continue
				}
				renameSeq = i
			}
			if renameSeq == -1 {
				t.Fatalf("%s: Rename was never called, want it attempted as the primary (failing) operation; calls=%v", tc.name, ops.calls)
			}
			for _, c := range ops.calls[renameSeq+1:] {
				if c.op != "Remove" {
					t.Errorf("%s: %s was called after the failed Rename, want only a best-effort Remove to follow it; calls=%v", tc.name, c.op, ops.calls)
				}
			}
		})
	}
}

// TestAudiobookState_A6_SaveSyncDirFailureIsReportedNotSwallowed covers A6:
// once Rename has succeeded the destination already holds the new content,
// so there is no temp file left to clean up - but a failure to durably
// sync the parent directory means the replacement is not yet crash-safe,
// and Save must still surface that as an error rather than reporting
// success (a caller that only checks the returned error must never
// mistake this for a fully durable commit).
func TestAudiobookState_A6_SaveSyncDirFailureIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{failSyncDir: true}
	store := NewFileStateStoreWithOps(finalPath, ops)

	if err := store.Save(context.Background(), sampleState()); err == nil {
		t.Fatalf("Save() error = nil, want an error when the parent-directory sync fails")
	}
	if ops.calls[len(ops.calls)-1].op != "SyncDir" {
		t.Errorf("last FileOps call = %+v, want the final attempted call to be SyncDir", ops.calls[len(ops.calls)-1])
	}
}

// TestAudiobookState_A6_SaveHonorsCancellationBeforeAnyFileOp covers I1 at
// the file-persistence boundary: an already-cancelled context must stop
// Save before it performs any FileOps call at all, not merely fail one of
// the operations after starting.
func TestAudiobookState_A6_SaveHonorsCancellationBeforeAnyFileOp(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	ops := &fakeFileOps{}
	store := NewFileStateStoreWithOps(finalPath, ops)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Save(ctx, sampleState()); err == nil {
		t.Errorf("Save() error = nil with a cancelled context, want an error")
	}
	if len(ops.calls) != 0 {
		t.Errorf("FileOps calls = %v after a cancelled Save, want none (cancellation must be checked before any file operation)", ops.calls)
	}
}
