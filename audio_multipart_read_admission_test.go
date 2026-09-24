package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"tiramisu/internal/gostorm/native"
	"tiramisu/internal/vfs"
)

// Slice: audio-multipart-read-admission. These tests drive the real Open,
// startNativePump and Read call sites with channel-controlled pump and demand
// bodies (pumpBodyFn / demandFetchAheadFn). Nothing here touches a live torrent,
// tracker, peer or FUSE mount, and no test uses a sleep as synchronization.
//
// Two production hooks give channel-observable barriers:
//   - backgroundReserveHook fires after the reserve check and before the permit
//     is taken. The test parks every concurrent caller there (or counts it as
//     returned, when it was denied earlier) and releases them together, so a
//     check-then-reserve that is not one atomic decision admits more than the
//     limit on every run;
//   - demandWaitHook fires when a demand caller found no permit and is about to
//     block. Every demand body blocks on a token channel the test controls, so a
//     caller is "settled" once it has either started a body or signalled the
//     hook; from then on it can only enter its body if the test frees a permit.
//
// Every wait below is a channel receive; the wall-clock constants are deadlock
// failsafes only.
//
// The hooks and the fake body attribute events to a caller by (file id, read
// offset), so callers of one test must differ in at least one of the two.
//
// Upper bounds ("at most") and positive progress are asserted separately, so a
// conservative but valid admission policy is not rejected.
//
// The wall-clock constants below only bound how long a failing test waits for an
// event a correct implementation produces at once; a passing run is released by
// channels alone.

const (
	admissionDest     = 512
	admissionFailSafe = 5 * time.Second
	admissionQuick    = 2 * time.Second
	admissionSize     = int64(1) << 30
)

var (
	errAdmissionInjected = errors.New("injected demand failure")
	errAdmissionAborted  = errors.New("test aborted demand body")
)

// admissionBodies replaces the two data bodies coordinated by masterDataSemaphore
// and counts how many of each are running at any instant.
type admissionBodies struct {
	mu   sync.Mutex
	cond *sync.Cond

	bgNow, demandNow int
	peakCombined     int
	bgStarts         int
	demandStarts     int
	demandByKey      map[admissionKey]int
	hookedByKey      map[admissionKey]int
	alive            int
	fail             map[int]error

	// changed wakes awaitSettled after every demand body start or wait signal.
	changed chan struct{}

	// reserveRelease, when armed, parks every caller at backgroundReserveHook.
	reserveRelease chan struct{}
	reserveArrived chan struct{}

	pumpRelease   chan struct{}
	pumpEntered   chan string
	pumpDone      chan struct{}
	demandEntered chan int
	demandGate    chan struct{}
	abort         chan struct{}
}

func newAdmissionBodies() *admissionBodies {
	b := &admissionBodies{
		demandByKey:    map[admissionKey]int{},
		hookedByKey:    map[admissionKey]int{},
		fail:           map[int]error{},
		changed:        make(chan struct{}, 1),
		reserveArrived: make(chan struct{}, 4096),
		pumpRelease:    make(chan struct{}),
		pumpEntered:    make(chan string, 4096),
		pumpDone:       make(chan struct{}, 4096),
		demandEntered:  make(chan int, 4096),
		demandGate:     make(chan struct{}, 4096),
		abort:          make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *admissionBodies) notePeakLocked() {
	if n := b.bgNow + b.demandNow; n > b.peakCombined {
		b.peakCombined = n
	}
}

// pumpBody stands in for the long-lived background pump. It holds until the test
// releases it (or the pump is cancelled), then runs the real nativePump against an
// already-cancelled context so the production teardown returns the permit.
func (b *admissionBodies) pumpBody(h *MkvHandle, ctx context.Context, r *native.NativeReader, start int64, s *NativePumpState) {
	b.mu.Lock()
	b.alive++
	b.bgNow++
	b.bgStarts++
	b.notePeakLocked()
	release := b.pumpRelease
	b.mu.Unlock()
	b.pumpEntered <- h.path

	select {
	case <-release:
	case <-ctx.Done():
	case <-b.abort:
	}

	b.mu.Lock()
	b.bgNow--
	b.mu.Unlock()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	h.nativePump(cancelled, r, start, s)
	b.pumpDone <- struct{}{}

	b.mu.Lock()
	b.alive--
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *admissionBodies) releasePumps() {
	b.mu.Lock()
	close(b.pumpRelease)
	b.pumpRelease = make(chan struct{})
	b.mu.Unlock()
}

// demandBody stands in for FetchAhead. It completes only when the test hands it a
// token, unless a failure was injected for its file.
func (b *admissionBodies) demandBody(_ *native.NativeClient, _ string, file int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
	b.mu.Lock()
	b.demandNow++
	b.demandStarts++
	b.demandByKey[admissionKey{file, off}]++
	b.notePeakLocked()
	failure := b.fail[file]
	b.mu.Unlock()
	b.demandEntered <- file
	b.notify()

	if failure == nil {
		select {
		case <-b.demandGate:
		case <-b.abort:
			failure = errAdmissionAborted
		}
	}
	b.mu.Lock()
	b.demandNow--
	b.mu.Unlock()

	if failure != nil {
		onFill(0, true, failure)
		return 0, failure
	}
	n := copy(dest, admissionPattern(file, off))
	copy(buf, dest[:n])
	onFill(n, true, nil)
	return n, nil
}

func (b *admissionBodies) notify() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

// waitHook is demandWaitHook: the caller found no demand permit and is about to block.
func (b *admissionBodies) waitHook(h *MkvHandle, off int64) {
	b.mu.Lock()
	b.hookedByKey[admissionKey{h.fileID, off}]++
	b.mu.Unlock()
	b.notify()
}

// reserveHook is backgroundReserveHook. Unarmed it is a no-op; armed, every caller
// announces itself and parks until the test releases the whole fan-out at once.
func (b *admissionBodies) reserveHook(*MkvHandle) {
	b.mu.Lock()
	release := b.reserveRelease
	b.mu.Unlock()
	if release == nil {
		return
	}
	b.reserveArrived <- struct{}{}
	select {
	case <-release:
	case <-b.abort:
	}
}

func (b *admissionBodies) armReserve() {
	b.mu.Lock()
	b.reserveRelease = make(chan struct{})
	b.mu.Unlock()
}

func (b *admissionBodies) disarmReserve() {
	b.mu.Lock()
	if b.reserveRelease != nil {
		close(b.reserveRelease)
		b.reserveRelease = nil
	}
	b.mu.Unlock()
}

// signalled reports whether the caller for file has started a body or waited.
func (b *admissionBodies) signalled(k admissionKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.demandByKey[k] > 0 || b.hookedByKey[k] > 0
}

// queued reports whether the caller for file waited for a permit and has not entered a body.
func (b *admissionBodies) queued(k admissionKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hookedByKey[k] > 0 && b.demandByKey[k] == 0
}

func (b *admissionBodies) failFile(file int) {
	b.mu.Lock()
	b.fail[file] = errAdmissionInjected
	b.mu.Unlock()
}

type admissionStats struct {
	bgNow, demandNow, peak, bgStarts, demandStarts int
}

func (b *admissionBodies) stats() admissionStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return admissionStats{b.bgNow, b.demandNow, b.peakCombined, b.bgStarts, b.demandStarts}
}

// startsFor counts demand bodies started for a torrent file at any offset.
func (b *admissionBodies) startsFor(file int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k, v := range b.demandByKey {
		if k.file == file {
			n += v
		}
	}
	return n
}

func admissionPattern(file int, off int64) []byte {
	p := make([]byte, admissionDest)
	for i := range p {
		p[i] = byte(file*7 + int(off>>20) + i)
	}
	return p
}

// admissionKey identifies one demand call: handles of one torrent file share the
// file id and are told apart by the offset they read.
type admissionKey struct {
	file int
	off  int64
}

type admissionRead struct {
	errno     syscall.Errno
	data      []byte
	hasResult bool
}

type admissionCaller struct {
	h      *MkvHandle
	file   int
	off    int64
	res    chan admissionRead
	cancel context.CancelFunc
}

type admissionRig struct {
	t        *testing.T
	e        *vfsEnv
	b        *admissionBodies
	capacity int
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu       sync.Mutex
	paths    []string
	nextFile int
}

// newAdmissionRig installs a master semaphore of the given capacity, a fresh
// read-ahead cache and the two fake bodies. Cleanups run before the globals are
// restored: they release every body and wait for the goroutines to leave.
func newAdmissionRig(t *testing.T, capacity int) *admissionRig {
	t.Helper()
	e := newVFSEnv(t)
	startupReadConfigureReaderGlobals(t)
	startupReadFreshCache(t)
	masterDataSemaphore = make(chan struct{}, capacity)
	nativeBridge = native.NewNativeClient()
	prevCleanup := globalCleanupManager
	globalCleanupManager = NewCleanupManager(logger, nil, nil, nativeBridge)
	prevPump, prevFetch := pumpBodyFn, demandFetchAheadFn
	prevReserve, prevWait := backgroundReserveHook, demandWaitHook

	ctx, cancel := context.WithCancel(context.Background())
	r := &admissionRig{t: t, e: e, b: newAdmissionBodies(), capacity: capacity, ctx: ctx, cancel: cancel, nextFile: 100}
	pumpBodyFn = r.b.pumpBody
	demandFetchAheadFn = r.b.demandBody
	backgroundReserveHook = r.b.reserveHook
	demandWaitHook = r.b.waitHook

	t.Cleanup(func() {
		r.cancel()
		close(r.b.abort)
		r.b.mu.Lock()
		for r.b.alive > 0 {
			r.b.cond.Wait()
		}
		r.b.mu.Unlock()
		done := make(chan struct{})
		go func() { r.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * admissionFailSafe):
			t.Log("admission rig: a Read goroutine did not leave after cancellation")
		}
		pumpBodyFn, demandFetchAheadFn = prevPump, prevFetch
		backgroundReserveHook, demandWaitHook = prevReserve, prevWait
		globalCleanupManager = prevCleanup
		r.mu.Lock()
		paths := append([]string(nil), r.paths...)
		r.mu.Unlock()
		for _, p := range paths {
			startupReadClearPath(p)
		}
	})
	return r
}

func (r *admissionRig) track(path string) string {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	return path
}

func (r *admissionRig) file() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextFile++
	return r.nextFile
}

func (r *admissionRig) audioPath(name string) string {
	return r.track(r.e.phys("", "music/Book/"+name+"_01234567.flac"))
}

func (r *admissionRig) videoPath(name string) string {
	return r.track(r.e.phys("", "movies/"+name+"_01234567.mkv"))
}

func (r *admissionRig) markHealthy(path string) {
	playbackRegistry.Store(path, &PlaybackState{Path: path, Hash: hashA, IsHealthy: true, OpenedAt: time.Now()})
}

// handle builds a cold handle the way Open would leave it, minus the pump.
func (r *admissionRig) handle(path string, file int) *MkvHandle {
	h := startupReadHandle(path, admissionSize)
	h.fileID = file
	h.state.Store(stateStreaming)
	return h
}

// startOwner gives a handle a real background pump through startNativePump.
func (r *admissionRig) startOwner(t *testing.T, path string) *MkvHandle {
	t.Helper()
	h := r.handle(path, r.file())
	h.startNativePump(h.hash, h.fileID)
	if !h.hasSlot.Load() {
		t.Fatalf("startNativePump denied a pump to %s with the master pool empty", path)
	}
	r.awaitPumps(t, 1)
	return h
}

func (r *admissionRig) awaitPumps(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(admissionFailSafe)
	for i := 0; i < n; i++ {
		select {
		case <-r.b.pumpEntered:
		case <-deadline:
			t.Fatalf("only %d of %d expected background pump bodies started", i, n)
		}
	}
}

// releaseAllPumps lets n pump bodies exit through the real teardown and waits for
// each to finish, which is when its permit must be back.
func (r *admissionRig) releaseAllPumps(t *testing.T, n int) {
	t.Helper()
	r.b.releasePumps()
	deadline := time.After(admissionFailSafe)
	for i := 0; i < n; i++ {
		select {
		case <-r.b.pumpDone:
		case <-deadline:
			t.Fatalf("only %d of %d pump bodies finished after release", i, n)
		}
	}
}

func (r *admissionRig) awaitDemandEntered(t *testing.T, n int, what string) {
	t.Helper()
	deadline := time.After(admissionFailSafe)
	for i := 0; i < n; i++ {
		select {
		case <-r.b.demandEntered:
		case <-deadline:
			t.Fatalf("%s: only %d of %d expected demand bodies started (demand admission starved)", what, i, n)
		}
	}
}

func (r *admissionRig) awaitEntered(t *testing.T, file int, what string) {
	t.Helper()
	deadline := time.After(admissionFailSafe)
	for {
		select {
		case f := <-r.b.demandEntered:
			if f == file {
				return
			}
		case <-deadline:
			t.Fatalf("%s: demand body for file %d never started", what, file)
		}
	}
}

// awaitSettled blocks on channel wake-ups until every caller has started a demand
// body or signalled demandWaitHook. It returns nothing to poll: once it returns,
// a caller that has not entered a body can only do so if the test frees a permit.
func (r *admissionRig) awaitSettled(t *testing.T, cs ...*admissionCaller) {
	t.Helper()
	failSafe := time.After(admissionFailSafe)
	for {
		missing := 0
		for _, c := range cs {
			if !r.b.signalled(admissionKey{c.file, c.off}) {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		select {
		case <-r.b.changed:
		case <-failSafe:
			t.Fatalf("%d of %d demand callers neither started a body nor waited for a permit", missing, len(cs))
		}
	}
}

// requireQueued asserts the caller is waiting for a demand permit, not inside a body.
func (r *admissionRig) requireQueued(t *testing.T, c *admissionCaller, what string) {
	t.Helper()
	if !r.b.queued(admissionKey{c.file, c.off}) {
		t.Errorf("%s: the caller never waited for a demand permit (it started %d data bodies without admission)",
			what, r.b.startsFor(c.file))
	}
}

// barrierFanOut runs run(0..n-1) concurrently. Every caller that reaches
// backgroundReserveHook parks there; callers denied earlier simply return. Once
// arrived+returned == n all of them are past the reserve check and none can have
// reserved, and only then are they released together.
func (r *admissionRig) barrierFanOut(t *testing.T, n int, run func(i int)) {
	t.Helper()
	r.b.armReserve()
	defer r.b.disarmReserve()
	returned := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run(i)
			returned <- struct{}{}
		}(i)
	}
	failSafe := time.After(admissionFailSafe)
	arrived, done := 0, 0
	for arrived+done < n {
		select {
		case <-r.b.reserveArrived:
			arrived++
		case <-returned:
			done++
		case <-failSafe:
			t.Fatalf("fan-out never reached the reserve boundary: %d parked, %d returned, want %d", arrived, done, n)
		}
	}
	r.b.disarmReserve()
	wg.Wait()
}

func countSlots(hs []*MkvHandle) int {
	n := 0
	for _, h := range hs {
		if h.hasSlot.Load() {
			n++
		}
	}
	return n
}

// startPumpsTogether starts pumps for hs at the barrier and returns how many got a
// permit, after checking that exactly that many pump bodies started.
func (r *admissionRig) startPumpsTogether(t *testing.T, hs []*MkvHandle) int {
	t.Helper()
	before := r.b.stats().bgStarts
	r.barrierFanOut(t, len(hs), func(i int) { hs[i].startNativePump(hs[i].hash, hs[i].fileID) })
	granted := countSlots(hs)
	r.awaitPumps(t, granted)
	if started := r.b.stats().bgStarts - before; started != granted {
		t.Errorf("%d pump bodies started for %d granted pump permits", started, granted)
	}
	return granted
}

func (r *admissionRig) coldHandles(n int) []*MkvHandle {
	hs := make([]*MkvHandle, n)
	for i := range hs {
		hs[i] = r.handle(r.audioPath(fmt.Sprintf("Part%02d", i)), r.file())
	}
	return hs
}

func (r *admissionRig) release(n int) {
	for i := 0; i < n; i++ {
		r.b.demandGate <- struct{}{}
	}
}

// newCaller starts an actual concurrent Read; the derived context is cancellable.
func (r *admissionRig) newCaller(h *MkvHandle, off int64) *admissionCaller {
	ctx, cancel := context.WithCancel(r.ctx)
	c := &admissionCaller{h: h, file: h.fileID, off: off, res: make(chan admissionRead, 1), cancel: cancel}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		out := admissionRead{errno: syscall.ENOTRECOVERABLE}
		defer func() {
			if p := recover(); p != nil {
				out = admissionRead{errno: syscall.ENOTRECOVERABLE}
			}
			c.res <- out
		}()
		res, errno := h.Read(ctx, make([]byte, admissionDest), off)
		out = admissionRead{errno: errno}
		if res != nil {
			data, _ := res.Bytes(nil)
			out.data = append([]byte(nil), data...)
			out.hasResult = true
		}
	}()
	return c
}

func (r *admissionRig) coldCaller(name string) *admissionCaller {
	return r.newCaller(r.handle(r.audioPath(name), r.file()), 0)
}

func (r *admissionRig) await(t *testing.T, c *admissionCaller, within time.Duration, what string) admissionRead {
	t.Helper()
	select {
	case out := <-c.res:
		return out
	case <-time.After(within):
		t.Fatalf("%s: Read did not return", what)
		return admissionRead{}
	}
}

func (r *admissionRig) requireData(t *testing.T, c *admissionCaller, what string) {
	t.Helper()
	out := r.await(t, c, admissionFailSafe, what)
	if out.errno != 0 || !out.hasResult {
		t.Errorf("%s: Read = (result %v, errno %v), want its bytes and OK", what, out.hasResult, out.errno)
		return
	}
	if want := admissionPattern(c.file, c.off); !bytes.Equal(out.data, want) {
		t.Errorf("%s: Read returned the wrong bytes", what)
	}
}

// startBlockers occupies n demand permits with bodies held inside the fake.
func (r *admissionRig) startBlockers(t *testing.T, n int, prefix string) []*admissionCaller {
	t.Helper()
	cs := make([]*admissionCaller, n)
	for i := range cs {
		cs[i] = r.coldCaller(fmt.Sprintf("%s%03d", prefix, i))
	}
	r.awaitDemandEntered(t, n, prefix+" blockers")
	return cs
}

func (r *admissionRig) finish(t *testing.T, cs ...*admissionCaller) {
	t.Helper()
	for i, c := range cs {
		r.requireData(t, c, fmt.Sprintf("caller %d (file %d)", i, c.file))
	}
}

func (r *admissionRig) requireCeiling(t *testing.T, what string) {
	t.Helper()
	if s := r.b.stats(); s.peak > r.capacity {
		t.Errorf("%s: %d background+demand bodies ran at once, master capacity is %d", what, s.peak, r.capacity)
	}
}

// requireNow checks the instantaneous combined body count.
func (r *admissionRig) requireNow(t *testing.T, what string) {
	t.Helper()
	if s := r.b.stats(); s.bgNow+s.demandNow > r.capacity {
		t.Errorf("%s: %d background + %d demand bodies running at once, master capacity is %d",
			what, s.bgNow, s.demandNow, r.capacity)
	}
}

// assertCapacityRestored proves no permit leaked: with bgHeld background bodies
// still alive, every remaining permit must admit a brand-new independent caller.
func (r *admissionRig) assertCapacityRestored(t *testing.T, bgHeld int, what string) {
	t.Helper()
	n := r.capacity - bgHeld
	cs := make([]*admissionCaller, n)
	for i := range cs {
		cs[i] = r.coldCaller(fmt.Sprintf("Fresh%s%03d", what, i))
	}
	r.awaitDemandEntered(t, n, what+": fresh callers after the operation (a permit leaked)")
	r.release(n)
	r.finish(t, cs...)
}

// admissionOpen opens a path through the real VirtualMkvNode.Open. Safe to call
// from many goroutines: it reports through its return values only.
func admissionOpen(ctx context.Context, path string, file int) (*MkvHandle, syscall.Errno) {
	n := &VirtualMkvNode{
		vMeta: &vfs.Metadata{Path: path, URL: streamURL(hashA, file), Size: admissionSize},
		wake:  func(context.Context, string, int) error { return nil },
	}
	fh, _, errno := n.Open(ctx, 0)
	if errno != 0 {
		return nil, errno
	}
	h, ok := fh.(*MkvHandle)
	if !ok {
		return nil, syscall.EBADF
	}
	h.lastGlobalUpdate = time.Now() // this harness has no global cleanup manager to notify
	return h, 0
}

func (r *admissionRig) sectionPath(section string, i int) string {
	name := fmt.Sprintf("Track%02d", i)
	switch section {
	case "video":
		return r.videoPath(name)
	case "mixed":
		if i%2 == 1 {
			return r.videoPath(name)
		}
	}
	return r.audioPath(name)
}

// A1 (upper bound): 32 concurrent cold paths, every one held at the
// pre-reservation boundary before any may reserve, admit no more than the limit.
func TestAdmissionA1_ConcurrentColdPathsNeverExceedBackgroundLimit(t *testing.T) {
	cases := []struct {
		name    string
		healthy bool
		limit   int
	}{
		{name: "no healthy playback admits at most capacity minus five (20 of 25)", limit: 20},
		{name: "an active healthy playback admits at most five", healthy: true, limit: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAdmissionRig(t, 25)
			if tc.healthy {
				r.markHealthy(r.audioPath("Playing"))
			}
			granted := r.startPumpsTogether(t, r.coldHandles(32))
			if granted > tc.limit {
				t.Errorf("%d of 32 simultaneous cold paths got a background pump, want at most %d (check and reservation must be one atomic decision)",
					granted, tc.limit)
			}
			r.releaseAllPumps(t, granted)
		})
	}
}

// A1 (progress): admission is not merely restrictive. Cold paths that arrive one
// at a time are served, the bound still holds, and a healthy path stays eligible
// after the scan reserve is spent.
func TestAdmissionA1_ColdPathsProgressAndHealthyPathRemainsEligible(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
		limit   int
	}{
		{name: "no healthy playback", limit: 20},
		{name: "healthy playback", healthy: true, limit: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAdmissionRig(t, 25)
			playing := r.audioPath("Playing")
			if tc.healthy {
				r.markHealthy(playing)
			}
			hs := r.coldHandles(32)
			for _, h := range hs {
				h.startNativePump(h.hash, h.fileID)
			}
			granted := countSlots(hs)
			if granted < 1 {
				t.Fatal("no cold path received a background pump with the master pool empty")
			}
			if granted > tc.limit {
				t.Errorf("%d sequential cold paths hold pumps, want at most %d", granted, tc.limit)
			}
			r.awaitPumps(t, granted)
			total := granted
			if tc.healthy {
				healthy := r.handle(playing, r.file())
				healthy.startNativePump(healthy.hash, healthy.fileID)
				if !healthy.hasSlot.Load() {
					t.Fatal("the healthy path itself was denied its pump")
				}
				r.awaitPumps(t, 1)
				total++
			}
			r.releaseAllPumps(t, total)
		})
	}
}

// A1/I3: the Open call site goes through the same admission for audio, video and
// both mixed, so nothing can bypass the reserve by starting its pump at Open.
func TestAdmissionA1_OpenFanOutHonoursTheBackgroundReserve(t *testing.T) {
	for _, section := range []string{"audio", "video", "mixed"} {
		for _, tc := range []struct {
			name    string
			healthy bool
			limit   int
		}{
			{name: "no healthy playback", limit: 20},
			{name: "healthy playback", healthy: true, limit: 5},
		} {
			t.Run(section+"/"+tc.name, func(t *testing.T) {
				r := newAdmissionRig(t, 25)
				if tc.healthy {
					r.markHealthy(r.audioPath("Playing"))
				}
				const files = 32
				paths := make([]string, files)
				for i := range paths {
					paths[i] = r.sectionPath(section, i)
				}
				hs := make([]*MkvHandle, files)
				errnos := make([]syscall.Errno, files)
				r.barrierFanOut(t, files, func(i int) { hs[i], errnos[i] = admissionOpen(r.ctx, paths[i], i+1) })
				for i := range hs {
					if errnos[i] != 0 || hs[i] == nil {
						t.Fatalf("Open(%s) = errno %v", paths[i], errnos[i])
					}
				}
				granted := countSlots(hs)
				r.awaitPumps(t, granted)
				if granted > tc.limit {
					t.Errorf("Open granted %d pumps to %d simultaneous files, want at most %d", granted, files, tc.limit)
				}
				if granted < 1 {
					t.Error("Open started no proactive pump for any of the files")
				}
				r.releaseAllPumps(t, granted)
			})
		}
	}
}

// A2: a handle that owns a pump slot must still obtain demand admission for its
// cache-miss fetch. With every remaining permit held by other demand bodies, its
// Read must queue: cancelling it returns EINTR and the body never starts.
func TestAdmissionA2_PumpOwnerCacheMissWaitsForDemandAdmission(t *testing.T) {
	const capacity = 4
	r := newAdmissionRig(t, capacity)
	owner := r.startOwner(t, r.audioPath("Owner"))
	blockers := r.startBlockers(t, capacity-1, "Blk")

	q := r.newCaller(owner, 0)
	r.awaitSettled(t, q)
	r.requireQueued(t, q, "pump owner cache miss beside a full pool")
	r.requireNow(t, "pump owner cache miss beside a full pool")
	q.cancel()

	out := r.await(t, q, admissionQuick, "cancelled pump-owner Read: it never queued for demand admission and is inside its data body")
	if out.errno != syscall.EINTR || out.hasResult {
		t.Errorf("cancelled pump-owner Read = (result %v, errno %v), want (no result, EINTR)", out.hasResult, out.errno)
	}
	if n := r.b.startsFor(owner.fileID); n != 0 {
		t.Errorf("the pump owner's demand body started %d times without demand admission", n)
	}
	r.requireCeiling(t, "pump owner cache miss")

	r.release(capacity - 1)
	r.finish(t, blockers...)
	r.releaseAllPumps(t, 1)
	r.assertCapacityRestored(t, 0, "A2")
}

// A2: the queued owner is admitted once a permit frees and then completes; combined
// bodies never exceed the master capacity.
func TestAdmissionA2_QueuedPumpOwnerRunsAfterAPermitFrees(t *testing.T) {
	const capacity = 4
	r := newAdmissionRig(t, capacity)
	owner := r.startOwner(t, r.audioPath("Owner"))
	blockers := r.startBlockers(t, capacity-1, "Blk")

	q := r.newCaller(owner, 0)
	r.awaitSettled(t, q)
	r.requireQueued(t, q, "owner queued behind a full pool")
	r.requireNow(t, "owner queued behind a full pool")
	r.release(1)
	r.awaitEntered(t, owner.fileID, "queued owner after one permit freed")
	// One freed permit admitted the owner, so exactly capacity-1 bodies are now
	// active (the two remaining blockers and the owner). Tokens are not addressed
	// to a body, so hand one to every active body before awaiting any of them.
	r.release(capacity - 1)
	r.finish(t, q)
	r.finish(t, blockers...)
	r.requireCeiling(t, "queued owner")
	r.releaseAllPumps(t, 1)
	r.assertCapacityRestored(t, 0, "A2b")
}

// A3: 32 real concurrent readers of different files of one torrent, with the
// background pumps admitted through Open, must all complete. Demand bodies are
// released by channel only; the maximum is asserted while all callers are settled,
// and progress beyond the first admitted wave is asserted separately.
func TestAdmissionA3_QueuedMultipartCallersAllMakeProgress(t *testing.T) {
	for _, section := range []string{"audio", "video", "mixed"} {
		for _, tc := range []struct {
			name    string
			healthy bool
			maxBg   int
		}{
			{name: "background reserve leaves the rest of 25 to demand", maxBg: 20},
			{name: "healthy playback keeps background at five", healthy: true, maxBg: 5},
		} {
			t.Run(section+"/"+tc.name, func(t *testing.T) {
				const files = 32
				r := newAdmissionRig(t, 25)
				if tc.healthy {
					r.markHealthy(r.audioPath("Playing"))
				}
				hs := make([]*MkvHandle, files)
				for i := range hs {
					h, errno := admissionOpen(r.ctx, r.sectionPath(section, i), i+1)
					if errno != 0 {
						t.Fatalf("Open file %d: errno %v", i, errno)
					}
					hs[i] = h
				}
				granted := countSlots(hs)
				if granted < 1 || granted > tc.maxBg {
					t.Fatalf("%d files hold background pumps, want between 1 and %d", granted, tc.maxBg)
				}
				r.awaitPumps(t, granted)

				cs := make([]*admissionCaller, files)
				for i, h := range hs {
					cs[i] = r.newCaller(h, 0)
				}
				r.awaitSettled(t, cs...)
				r.requireNow(t, "32 callers with the background pumps still running")
				r.awaitDemandEntered(t, 1, "first admitted wave")

				r.release(files)
				r.finish(t, cs...)
				if s := r.b.stats(); s.demandStarts != files {
					t.Errorf("%d demand bodies ran for %d callers, want exactly one each", s.demandStarts, files)
				}
				r.requireCeiling(t, "32 multipart callers")
				r.releaseAllPumps(t, granted)
				r.assertCapacityRestored(t, 0, "A3")
			})
		}
	}
}

// I1: a queued caller that is cancelled returns the interrupted result, never
// enters the body later, and leaves every permit reusable by a later caller.
func TestAdmissionI1_CancelledQueuedCallerNeverEntersBodyAndPermitsReturn(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "non-owner caller"
		if owner {
			name = "pump-owning caller"
		}
		t.Run(name, func(t *testing.T) {
			const capacity = 4
			r := newAdmissionRig(t, capacity)
			bg := 0
			var target *MkvHandle
			if owner {
				target = r.startOwner(t, r.audioPath("Owner"))
				bg = 1
			} else {
				target = r.handle(r.audioPath("Cold"), r.file())
			}
			blockers := r.startBlockers(t, capacity-bg, "Blk")

			q := r.newCaller(target, 0)
			r.awaitSettled(t, q)
			r.requireQueued(t, q, "caller behind a full pool")
			q.cancel()

			out := r.await(t, q, admissionQuick, "cancelled queued Read: still inside a data body")
			if out.errno != syscall.EINTR || out.hasResult {
				t.Errorf("cancelled queued Read = (result %v, errno %v), want (no result, EINTR)", out.hasResult, out.errno)
			}

			r.release(capacity - bg)
			r.finish(t, blockers...)
			if n := r.b.startsFor(target.fileID); n != 0 {
				t.Errorf("the cancelled caller entered the data body %d times after cancellation", n)
			}
			r.assertCapacityRestored(t, bg, "I1cancel")
			if n := r.b.startsFor(target.fileID); n != 0 {
				t.Errorf("the cancelled caller entered the data body %d times once permits were free", n)
			}
			if owner {
				r.releaseAllPumps(t, 1)
				r.assertCapacityRestored(t, 0, "I1cancelBg")
			}
		})
	}
}

// I1: permits return on success, on an injected body error, and when a pump is
// cancelled; each time a full complement of new independent callers is admitted.
func TestAdmissionI1_PermitsReleasedOnSuccessErrorAndPumpCancellation(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "non-owner demand"
		if owner {
			name = "pump-owning demand"
		}
		t.Run(name+"/injected body error", func(t *testing.T) {
			const capacity = 3
			r := newAdmissionRig(t, capacity)
			bg := 0
			var h *MkvHandle
			if owner {
				h = r.startOwner(t, r.audioPath("Owner"))
				bg = 1
			} else {
				h = r.handle(r.audioPath("Cold"), r.file())
			}
			r.b.failFile(h.fileID)
			c := r.newCaller(h, 0)
			out := r.await(t, c, 3*admissionFailSafe, "failing demand Read")
			if out.errno != syscall.EIO || out.hasResult {
				t.Errorf("Read with a failing body = (result %v, errno %v), want (no result, EIO)", out.hasResult, out.errno)
			}
			r.assertCapacityRestored(t, bg, "I1err")
			if owner {
				r.releaseAllPumps(t, 1)
				r.assertCapacityRestored(t, 0, "I1errBg")
			}
		})
	}

	t.Run("cancelled background pump returns its permit", func(t *testing.T) {
		const capacity = 3
		r := newAdmissionRig(t, capacity)
		h := r.startOwner(t, r.audioPath("Owner"))
		v, ok := activePumps.Load(h.path)
		if !ok {
			t.Fatal("no active pump registered for the owner")
		}
		v.(*NativePumpState).cancel()
		select {
		case <-r.b.pumpDone:
		case <-time.After(admissionFailSafe):
			t.Fatal("cancelled pump body never finished")
		}
		r.assertCapacityRestored(t, 0, "I1pump")
		again := r.handle(r.audioPath("Again"), r.file())
		again.startNativePump(again.hash, again.fileID)
		if !again.hasSlot.Load() {
			t.Error("a later independent path was denied a pump after the cancelled pump's permit should have returned")
		}
		r.awaitPumps(t, 1)
		r.releaseAllPumps(t, 1)
	})
}

// I2: a range that becomes cached while the caller is queued is returned as is,
// with no duplicate demand body, and the permit accounting stays balanced.
func TestAdmissionI2_RangeCachedWhileQueuedIsServedWithoutDuplicateBody(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "non-owner caller"
		if owner {
			name = "pump-owning caller"
		}
		t.Run(name, func(t *testing.T) {
			const capacity = 4
			r := newAdmissionRig(t, capacity)
			bg := 0
			var target *MkvHandle
			if owner {
				target = r.startOwner(t, r.audioPath("Owner"))
				bg = 1
			} else {
				target = r.handle(r.audioPath("Cold"), r.file())
			}
			blockers := r.startBlockers(t, capacity-bg, "Blk")

			q := r.newCaller(target, 0)
			r.awaitSettled(t, q)
			r.requireQueued(t, q, "caller behind a full pool")
			cached := bytes.Repeat([]byte{0xEE}, admissionDest)
			raCache.Put(target.path, 0, admissionDest-1, cached)
			r.release(1)

			out := r.await(t, q, admissionFailSafe, "queued Read whose range was cached meanwhile")
			if out.errno != 0 || !bytes.Equal(out.data, cached) {
				t.Errorf("queued Read = (errno %v, %d bytes, cached bytes match %v), want the cached bytes and OK",
					out.errno, len(out.data), bytes.Equal(out.data, cached))
			}
			if n := r.b.startsFor(target.fileID); n != 0 {
				t.Errorf("a duplicate demand body ran %d times for an already cached range", n)
			}
			r.release(capacity - bg - 1)
			r.finish(t, blockers...)
			r.assertCapacityRestored(t, bg, "I2")
			if owner {
				r.releaseAllPumps(t, 1)
			}
		})
	}
}

// I3: video keeps its proactive pump at Open and its blocking demand reads, and
// video and audio draw on the one shared ceiling.
func TestAdmissionI3_VideoKeepsProactivePumpAndDemandReads(t *testing.T) {
	r := newAdmissionRig(t, 6)
	h, errno := admissionOpen(r.ctx, r.videoPath("Film"), 1)
	if errno != 0 {
		t.Fatalf("video Open errno = %v", errno)
	}
	if !h.hasSlot.Load() {
		t.Fatal("video Open no longer starts its proactive pump")
	}
	r.awaitPumps(t, 1)

	c := r.newCaller(h, 0)
	r.awaitEntered(t, h.fileID, "video cache-miss demand read")
	r.release(1)
	r.requireData(t, c, "video demand read")

	audio := r.startBlockers(t, 5, "Aud")
	r.requireNow(t, "one video pump plus five audio demand bodies")
	r.release(5)
	r.finish(t, audio...)
	r.requireCeiling(t, "video and audio")
	r.releaseAllPumps(t, 1)
	r.assertCapacityRestored(t, 0, "I3")
}

// E1 (bounds): at and below the five-slot reserve the background limit is a
// maximum of max(1, capacity-5) without healthy playback, never above the master
// ceiling with it, and never zero. Demand callers beside the pumps still make
// progress: at once when a permit is free, otherwise after an explicit pump release.
func TestAdmissionE1_BackgroundLimitNeverZeroAndDemandProgressesAtLowCapacity(t *testing.T) {
	cases := []struct {
		capacity int
		healthy  bool
		max      int
	}{
		{capacity: 1, max: 1},
		{capacity: 2, max: 1},
		{capacity: 3, max: 1},
		{capacity: 5, max: 1},
		{capacity: 6, max: 1},
		{capacity: 7, max: 2},
		{capacity: 1, healthy: true, max: 1},
		{capacity: 2, healthy: true, max: 2},
		{capacity: 3, healthy: true, max: 3},
		{capacity: 5, healthy: true, max: 5},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("capacity %d healthy=%v", tc.capacity, tc.healthy), func(t *testing.T) {
			r := newAdmissionRig(t, tc.capacity)
			if tc.healthy {
				r.markHealthy(r.audioPath("Playing"))
			}
			granted := r.startPumpsTogether(t, r.coldHandles(8))
			if granted > tc.max {
				t.Errorf("%d pumps admitted at capacity %d, want at most %d", granted, tc.capacity, tc.max)
			}
			if granted < 1 {
				t.Error("the background limit became zero: no cold path received a pump")
			}

			const callers = 2
			cs := []*admissionCaller{r.coldCaller("DemandA"), r.coldCaller("DemandB")}
			r.awaitSettled(t, cs...)
			r.requireNow(t, "demand callers beside the pumps")
			if granted < tc.capacity {
				r.awaitDemandEntered(t, 1, "a free permit next to the pumps")
			}
			r.releaseAllPumps(t, granted)
			r.release(callers)
			r.finish(t, cs...)
			r.requireCeiling(t, "low capacity")
			r.assertCapacityRestored(t, 0, "E1")
		})
	}
}

// E1 (ceiling): a pump owner and cold callers together contend for what the one
// pump leaves; the combined body count never exceeds the master capacity and
// demand still progresses.
func TestAdmissionE1_PumpOwnerAndColdCallersShareTheRemainingPermits(t *testing.T) {
	for _, capacity := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("capacity %d", capacity), func(t *testing.T) {
			r := newAdmissionRig(t, capacity)
			owner := r.startOwner(t, r.audioPath("Owner"))
			cs := []*admissionCaller{r.newCaller(owner, 0)}
			for i := 1; i < capacity; i++ {
				cs = append(cs, r.coldCaller(fmt.Sprintf("Cold%02d", i)))
			}
			r.awaitSettled(t, cs...)
			r.requireNow(t, fmt.Sprintf("%d demand callers and one pump", capacity))
			r.awaitDemandEntered(t, 1, "demand beside the single pump")
			r.release(capacity)
			r.finish(t, cs...)
			r.requireCeiling(t, "boundary capacity")
			r.releaseAllPumps(t, 1)
			r.assertCapacityRestored(t, 0, "E1b")
		})
	}
}

// E2: concurrent handles of one path attach to one shared pump state and are
// charged one background permit; their independent cache-miss bodies still obey
// demand admission. Attached handles need not each own the permit.
func TestAdmissionE2_SamePathHandlesShareOnePumpButNotDemandAdmission(t *testing.T) {
	const capacity = 3
	r := newAdmissionRig(t, capacity)
	path := r.audioPath("Shared")
	file := r.file() // every handle is a repeated open of the same torrent file
	chunk := raCache.ChunkSize(path)

	hs := make([]*MkvHandle, 3)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range hs {
		hs[i] = r.handle(path, file)
		wg.Add(1)
		go func(h *MkvHandle) {
			defer wg.Done()
			<-start
			h.startNativePump(h.hash, h.fileID)
		}(hs[i])
	}
	close(start)
	wg.Wait()
	r.awaitPumps(t, 1)
	v, ok := activePumps.Load(path)
	if !ok {
		t.Fatal("no active pump for the shared path")
	}
	state := v.(*NativePumpState)
	for i, h := range hs {
		h.mu.Lock()
		shared := h.pumpState == state
		h.mu.Unlock()
		if !shared {
			t.Errorf("handle %d is not attached to the path's one pump state", i)
		}
	}

	cs := make([]*admissionCaller, len(hs))
	for i, h := range hs {
		cs[i] = r.newCaller(h, int64(i)*chunk)
	}
	r.awaitSettled(t, cs...)
	r.requireNow(t, "three same-path handles reading beside one pump")
	r.awaitDemandEntered(t, capacity-1, "same-path demand bodies (one pump permit leaves two)")
	r.release(len(hs))
	r.finish(t, cs...)
	r.requireCeiling(t, "same-path handles")
	r.releaseAllPumps(t, 1)
	r.assertCapacityRestored(t, 0, "E2")
}
