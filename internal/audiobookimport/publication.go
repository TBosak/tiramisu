package audiobookimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

const LibraryTypeAudiobook = "audiobook"

type ExternalIdentity struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

func (id ExternalIdentity) Key() string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(id.Namespace) + "\x00" + strings.TrimSpace(id.ID)))
	return hex.EncodeToString(sum[:])
}

const (
	IdentityNamespaceAudioSiloRecording = "audiosilo:recording"
	IdentityNamespaceAudioSiloWork      = "audiosilo:work"
)

func ResolveExternalIdentity(work WorkFacts, recording RecordingFacts) (ExternalIdentity, error) {
	if value := strings.TrimSpace(recording.RecordingID); value != "" {
		return ExternalIdentity{Namespace: IdentityNamespaceAudioSiloRecording, ID: value}, nil
	}
	if value := strings.TrimSpace(work.WorkID); value != "" {
		return ExternalIdentity{Namespace: IdentityNamespaceAudioSiloWork, ID: value}, nil
	}
	return ExternalIdentity{}, errors.New("missing audiosilo identity")
}

type LibraryFileRequest struct {
	Path             string
	SourceIndex      int
	SourcePath       string
	ExternalIdentity ExternalIdentity
}

type LibraryPublishRequest struct {
	Type        string
	Title       string
	Hash        string
	Magnet      string
	TorrentFile []byte
	Files       []LibraryFileRequest
}

type LibraryPublishResult struct {
	AlreadyPresent bool
}

type LibraryPublisher interface {
	Add(ctx context.Context, req LibraryPublishRequest) (LibraryPublishResult, error)
}

type LibraryRemoveRequest struct {
	Type   string
	Prefix string
}

type LibraryRemover interface {
	Remove(ctx context.Context, req LibraryRemoveRequest) error
}

type AudiobookshelfScanner interface {
	Scan(ctx context.Context, libraryID string) (AudiobookshelfScanResult, error)
}

type Stage string

const (
	StagePlanned        Stage = "planned"
	StagePublished      Stage = "published"
	StageScanned        Stage = "scanned"
	StageRemovalPlanned Stage = "removal-planned"
	StageRemoved        Stage = "removed"
	StageRemovalScanned Stage = "removal-scanned"
)

type StateEntry struct {
	Namespace          string    `json:"namespace"`
	ExternalID         string    `json:"external_id"`
	ProjectedPaths     []string  `json:"projected_paths"`
	OwnedPrefix        string    `json:"owned_prefix"`
	SourceFingerprint  string    `json:"source_fingerprint"`
	ControllerOwned    bool      `json:"controller_owned"`
	Stage              Stage     `json:"stage"`
	ScanStatusCode     int       `json:"scan_status_code,omitempty"`
	ConsecutiveMissing int       `json:"consecutive_missing"`
	MissingSince       time.Time `json:"missing_since,omitempty"`
	AttemptCount       int       `json:"attempt_count"`
	LastReason         string    `json:"last_reason,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type State struct {
	Version int                   `json:"version"`
	Entries map[string]StateEntry `json:"entries"`
}

const CurrentStateVersion = 1

type StateLimits struct {
	MaxEntries      int
	MaxStringLength int
}

type StateStore interface {
	Load(ctx context.Context) (State, error)
	Save(ctx context.Context, state State) error
}

type FileStateStore struct {
	path string
	ops  FileOps
}

func NewFileStateStore(path string) *FileStateStore { return &FileStateStore{path: path} }

// FileOps exposes the filesystem operations required for crash-safe state
// replacement so their ordering and failures can be tested deterministically.
type FileOps interface {
	WriteFile(name string, data []byte, perm os.FileMode) error
	Sync(name string) error
	Rename(oldpath, newpath string) error
	SyncDir(dir string) error
	Remove(name string) error
}

func NewFileStateStoreWithOps(path string, ops FileOps) *FileStateStore {
	return &FileStateStore{path: path, ops: ops}
}

type osFileOps struct{}

func (osFileOps) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}
func (osFileOps) Sync(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (osFileOps) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (osFileOps) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if runtime.GOOS == "windows" {
		return nil
	}
	return err
}
func (osFileOps) Remove(name string) error { return os.Remove(name) }

func (s *FileStateStore) fileOps() FileOps {
	if s.ops != nil {
		return s.ops
	}
	return osFileOps{}
}

func (s *FileStateStore) Load(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: CurrentStateVersion, Entries: map[string]StateEntry{}}, nil
	}
	if err != nil {
		return State{}, errors.New("audiobook state load failed")
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, errors.New("invalid audiobook state")
	}
	if err := validateState(state, StateLimits{MaxEntries: 100000, MaxStringLength: 1 << 20}); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *FileStateStore) Save(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateState(state, StateLimits{MaxEntries: 100000, MaxStringLength: 1 << 20}); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return errors.New("audiobook state encoding failed")
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New("audiobook state directory failed")
	}
	temp := filepath.Join(dir, "."+filepath.Base(s.path)+".tmp")
	ops := s.fileOps()
	cleanup := func() { _ = ops.Remove(temp) }
	if err := ops.WriteFile(temp, data, 0o600); err != nil {
		cleanup()
		return errors.New("audiobook state write failed")
	}
	if err := ops.Sync(temp); err != nil {
		cleanup()
		return errors.New("audiobook state sync failed")
	}
	if err := ops.Rename(temp, s.path); err != nil {
		cleanup()
		return errors.New("audiobook state replace failed")
	}
	if err := ops.SyncDir(dir); err != nil {
		return errors.New("audiobook state directory sync failed")
	}
	return nil
}

type PublicationLimits struct {
	StateLimits
	MaxAttempts int
}

type PublicationDeps struct {
	Publisher               LibraryPublisher
	Scanner                 AudiobookshelfScanner
	Store                   StateStore
	Limits                  PublicationLimits
	AudiobookshelfLibraryID string
}

type PublicationRequest struct {
	Work      WorkFacts
	Recording RecordingFacts
	Selection SelectionResult
	Projected []ProjectedFile
	DryRun    bool
}

type PublicationResult struct {
	Identity       ExternalIdentity
	Request        LibraryPublishRequest
	Stage          Stage
	AlreadyPresent bool
	ScanStatusCode int
	Explanation    string
}

func Publish(ctx context.Context, deps PublicationDeps, req PublicationRequest) (PublicationResult, error) {
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	if err := validatePublicationDeps(deps); err != nil {
		return PublicationResult{}, err
	}
	id, libReq, fingerprint, prefix, err := buildPublicationRequest(req)
	if err != nil {
		return PublicationResult{}, err
	}
	result := PublicationResult{Identity: id, Request: libReq, Stage: StagePlanned, Explanation: "audiobook publication planned"}
	if req.DryRun {
		return result, nil
	}
	state, err := deps.Store.Load(ctx)
	if err != nil {
		return PublicationResult{}, safeContextError(ctx, "audiobook state load failed")
	}
	if err := validateState(state, deps.Limits.StateLimits); err != nil {
		return PublicationResult{}, err
	}
	key := id.Key()
	entry, exists := state.Entries[key]
	paths := projectedPathsForState(req.Projected)
	if exists {
		if entry.Namespace != id.Namespace || entry.ExternalID != id.ID || !equalStrings(entry.ProjectedPaths, paths) ||
			(entry.SourceFingerprint != "" && entry.SourceFingerprint != fingerprint) ||
			(entry.OwnedPrefix != "" && entry.OwnedPrefix != prefix) {
			return PublicationResult{}, errors.New("conflicting audiobook publication state")
		}
		switch entry.Stage {
		case StageScanned:
			result.Stage, result.ScanStatusCode = StageScanned, entry.ScanStatusCode
			return result, nil
		case StagePublished:
			// resume at scan
		case StagePlanned:
			if entry.AttemptCount >= deps.Limits.MaxAttempts {
				return PublicationResult{}, errors.New("audiobook publication attempt limit reached")
			}
		default:
			return PublicationResult{}, errors.New("invalid audiobook publication stage")
		}
	} else {
		if len(state.Entries) >= deps.Limits.MaxEntries {
			return PublicationResult{}, errors.New("audiobook state entry limit reached")
		}
		entry = StateEntry{Namespace: id.Namespace, ExternalID: id.ID, ProjectedPaths: paths, OwnedPrefix: prefix,
			SourceFingerprint: fingerprint, ControllerOwned: true, Stage: StagePlanned, UpdatedAt: time.Now().UTC()}
		state.Entries[key] = entry
		if err := deps.Store.Save(ctx, state); err != nil {
			return PublicationResult{}, safeContextError(ctx, "audiobook planned state save failed")
		}
	}
	if entry.Stage == StagePlanned {
		if err := ctx.Err(); err != nil {
			return PublicationResult{}, err
		}
		pubResult, addErr := deps.Publisher.Add(ctx, libReq)
		if addErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return PublicationResult{}, ctxErr
			}
			entry.AttemptCount++
			entry.LastReason = "add_failed"
			entry.UpdatedAt = time.Now().UTC()
			state.Entries[key] = entry
			if err := deps.Store.Save(ctx, state); err != nil {
				return PublicationResult{}, errors.New("audiobook add and state save failed")
			}
			return PublicationResult{}, errors.New("audiobook library add failed")
		}
		entry.AttemptCount++
		entry.Stage = StagePublished
		entry.LastReason = ""
		entry.UpdatedAt = time.Now().UTC()
		state.Entries[key] = entry
		if err := deps.Store.Save(ctx, state); err != nil {
			return PublicationResult{}, safeContextError(ctx, "audiobook published state save failed")
		}
		result.AlreadyPresent = pubResult.AlreadyPresent
	}
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	scanResult, err := deps.Scanner.Scan(ctx, deps.AudiobookshelfLibraryID)
	if err != nil {
		return PublicationResult{}, safeContextError(ctx, "audiobook scan failed")
	}
	entry.Stage = StageScanned
	entry.ScanStatusCode = scanResult.StatusCode
	entry.LastReason = ""
	entry.UpdatedAt = time.Now().UTC()
	state.Entries[key] = entry
	if err := deps.Store.Save(ctx, state); err != nil {
		return PublicationResult{}, safeContextError(ctx, "audiobook scanned state save failed")
	}
	result.Stage, result.ScanStatusCode = StageScanned, scanResult.StatusCode
	return result, nil
}

type RemovalPolicy struct {
	ConsecutiveMissingThreshold int
	Grace                       time.Duration
}

type RemovalDeps struct {
	Remover                 LibraryRemover
	Scanner                 AudiobookshelfScanner
	Store                   StateStore
	Policy                  RemovalPolicy
	Limits                  PublicationLimits
	AudiobookshelfLibraryID string
}

type RemovalResult struct {
	Removed        []ExternalIdentity
	ScanStatusCode int
}

func Reconcile(ctx context.Context, deps RemovalDeps, seenExternalIDs map[ExternalIdentity]bool, now time.Time) (RemovalResult, error) {
	if err := ctx.Err(); err != nil {
		return RemovalResult{}, err
	}
	if err := validateRemovalDeps(deps); err != nil {
		return RemovalResult{}, err
	}
	state, err := deps.Store.Load(ctx)
	if err != nil {
		return RemovalResult{}, safeContextError(ctx, "audiobook state load failed")
	}
	if err := validateState(state, deps.Limits.StateLimits); err != nil {
		return RemovalResult{}, err
	}
	keys := make([]string, 0, len(state.Entries))
	for key := range state.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := RemovalResult{}
	pendingScan := make([]string, 0)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entry := state.Entries[key]
		id := ExternalIdentity{Namespace: entry.Namespace, ID: entry.ExternalID}
		if !entry.ControllerOwned {
			continue
		}
		if !safeRelativePath(entry.OwnedPrefix) {
			return result, errors.New("unsafe audiobook owned prefix")
		}
		if entry.Stage == StageRemoved {
			pendingScan = append(pendingScan, key)
			result.Removed = append(result.Removed, id)
			continue
		}
		if entry.Stage == StageRemovalScanned {
			continue
		}
		if entry.Stage != StageScanned && entry.Stage != StageRemovalPlanned {
			continue
		}
		if entry.Stage == StageScanned && seenExternalIDs[id] {
			if entry.ConsecutiveMissing != 0 || !entry.MissingSince.IsZero() {
				entry.ConsecutiveMissing, entry.MissingSince, entry.UpdatedAt = 0, time.Time{}, now
				state.Entries[key] = entry
				if err := deps.Store.Save(ctx, state); err != nil {
					return result, safeContextError(ctx, "audiobook removal state save failed")
				}
			}
			continue
		}
		if entry.Stage == StageScanned {
			if entry.ConsecutiveMissing < int(^uint(0)>>1) {
				entry.ConsecutiveMissing++
			}
			if entry.MissingSince.IsZero() {
				entry.MissingSince = now
			}
			entry.UpdatedAt = now
			state.Entries[key] = entry
			if err := deps.Store.Save(ctx, state); err != nil {
				return result, safeContextError(ctx, "audiobook missing state save failed")
			}
			if entry.ConsecutiveMissing < deps.Policy.ConsecutiveMissingThreshold || now.Before(entry.MissingSince.Add(deps.Policy.Grace)) {
				continue
			}
			entry.Stage = StageRemovalPlanned
			state.Entries[key] = entry
			if err := deps.Store.Save(ctx, state); err != nil {
				return result, safeContextError(ctx, "audiobook removal plan save failed")
			}
		}
		if err := deps.Remover.Remove(ctx, LibraryRemoveRequest{Type: LibraryTypeAudiobook, Prefix: entry.OwnedPrefix}); err != nil {
			return result, safeContextError(ctx, "audiobook library remove failed")
		}
		entry.Stage = StageRemoved
		entry.UpdatedAt = now
		state.Entries[key] = entry
		if err := deps.Store.Save(ctx, state); err != nil {
			return result, safeContextError(ctx, "audiobook removed state save failed")
		}
		pendingScan = append(pendingScan, key)
		result.Removed = append(result.Removed, id)
	}
	if len(pendingScan) == 0 {
		return result, nil
	}
	scanResult, err := deps.Scanner.Scan(ctx, deps.AudiobookshelfLibraryID)
	if err != nil {
		return result, safeContextError(ctx, "audiobook removal scan failed")
	}
	for _, key := range pendingScan {
		entry := state.Entries[key]
		entry.Stage, entry.ScanStatusCode, entry.UpdatedAt = StageRemovalScanned, scanResult.StatusCode, now
		state.Entries[key] = entry
	}
	if err := deps.Store.Save(ctx, state); err != nil {
		return result, safeContextError(ctx, "audiobook removal scan state save failed")
	}
	result.ScanStatusCode = scanResult.StatusCode
	return result, nil
}

func validatePublicationDeps(deps PublicationDeps) error {
	if nilLike(deps.Publisher) || nilLike(deps.Scanner) || nilLike(deps.Store) {
		return errors.New("audiobook publication dependency is nil")
	}
	if err := validateLimits(deps.Limits); err != nil {
		return err
	}
	if strings.TrimSpace(deps.AudiobookshelfLibraryID) == "" {
		return errors.New("missing audiobookshelf library id")
	}
	return nil
}

func validateRemovalDeps(deps RemovalDeps) error {
	if nilLike(deps.Remover) || nilLike(deps.Scanner) || nilLike(deps.Store) {
		return errors.New("audiobook removal dependency is nil")
	}
	if err := validateLimits(deps.Limits); err != nil {
		return err
	}
	if deps.Policy.ConsecutiveMissingThreshold <= 0 || deps.Policy.Grace < 0 {
		return errors.New("invalid audiobook removal policy")
	}
	if strings.TrimSpace(deps.AudiobookshelfLibraryID) == "" {
		return errors.New("missing audiobookshelf library id")
	}
	return nil
}

func validateLimits(limits PublicationLimits) error {
	if limits.MaxEntries <= 0 || limits.MaxStringLength <= 0 || limits.MaxAttempts <= 0 {
		return errors.New("invalid audiobook publication limits")
	}
	return nil
}

func nilLike(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	return (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()
}

func buildPublicationRequest(req PublicationRequest) (ExternalIdentity, LibraryPublishRequest, string, string, error) {
	id, err := ResolveExternalIdentity(req.Work, req.Recording)
	if err != nil {
		return ExternalIdentity{}, LibraryPublishRequest{}, "", "", err
	}
	if req.Selection.Selected == nil || !req.Selection.Selected.Eligible || len(req.Selection.Selected.Selected) == 0 {
		return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("invalid audiobook selection")
	}
	if strings.TrimSpace(req.Selection.Hash) == "" || (len(req.Selection.TorrentFile) > 0 && req.Selection.Magnet != "") ||
		(req.Selection.TorrentFile != nil && len(req.Selection.TorrentFile) == 0) {
		return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("invalid audiobook source")
	}
	expected, err := PlanProjection(req.Work, []Decision{*req.Selection.Selected})
	if err != nil || !equalProjected(expected, req.Projected) {
		return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("invalid audiobook projection")
	}
	sources := make(map[int]AudioFile, len(req.Selection.Files))
	for _, source := range req.Selection.Files {
		if _, exists := sources[source.Index]; exists {
			return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("duplicate audiobook source index")
		}
		sources[source.Index] = source
	}
	selected := make(map[int]bool, len(req.Selection.Selected.Selected))
	for _, file := range req.Selection.Selected.Selected {
		selected[file.FileIndex] = true
	}
	files := make([]LibraryFileRequest, len(req.Projected))
	for i, projected := range req.Projected {
		if !safeRelativePath(projected.Path) || !selected[projected.FileIndex] {
			return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("invalid audiobook file correlation")
		}
		source, ok := sources[projected.FileIndex]
		if !ok {
			return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("missing audiobook source file")
		}
		files[i] = LibraryFileRequest{Path: projected.Path, SourceIndex: projected.FileIndex, SourcePath: source.Path, ExternalIdentity: id}
	}
	libReq := LibraryPublishRequest{Type: LibraryTypeAudiobook, Title: req.Work.Title, Hash: req.Selection.Hash,
		Magnet: req.Selection.Magnet, TorrentFile: append([]byte(nil), req.Selection.TorrentFile...), Files: files}
	fingerprint := publicationFingerprint(libReq)
	prefix := path.Dir(req.Projected[0].Path)
	if prefix == "." || !safeRelativePath(prefix) {
		return ExternalIdentity{}, LibraryPublishRequest{}, "", "", errors.New("invalid audiobook owned prefix")
	}
	return id, libReq, fingerprint, prefix, nil
}

func publicationFingerprint(req LibraryPublishRequest) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(req.Hash))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.Magnet))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(req.TorrentFile)
	return hex.EncodeToString(h.Sum(nil))
}

func equalProjected(a, b []ProjectedFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func projectedPathsForState(projected []ProjectedFile) []string {
	result := make([]string, len(projected))
	for i := range projected {
		result[i] = projected[i].Path
	}
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateState(state State, limits StateLimits) error {
	if state.Version != CurrentStateVersion {
		return errors.New("unsupported audiobook state version")
	}
	if state.Entries == nil {
		return errors.New("invalid audiobook state entries")
	}
	if limits.MaxEntries <= 0 || limits.MaxStringLength <= 0 || len(state.Entries) > limits.MaxEntries {
		return errors.New("invalid audiobook state limits")
	}
	for key, entry := range state.Entries {
		id := ExternalIdentity{Namespace: entry.Namespace, ID: entry.ExternalID}
		if strings.TrimSpace(entry.Namespace) == "" || strings.TrimSpace(entry.ExternalID) == "" || key != id.Key() {
			return errors.New("conflicting audiobook state identity")
		}
		values := []string{entry.Namespace, entry.ExternalID, entry.OwnedPrefix, entry.SourceFingerprint, entry.LastReason}
		values = append(values, entry.ProjectedPaths...)
		for _, value := range values {
			if len(value) > limits.MaxStringLength {
				return errors.New("audiobook state string limit exceeded")
			}
		}
		if entry.AttemptCount < 0 || entry.ConsecutiveMissing < 0 {
			return errors.New("invalid audiobook state counter")
		}
		switch entry.Stage {
		case StagePlanned, StagePublished, StageScanned, StageRemovalPlanned, StageRemoved, StageRemovalScanned:
		default:
			return errors.New("invalid audiobook state stage")
		}
	}
	return nil
}

func safeContextError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s", message)
}
