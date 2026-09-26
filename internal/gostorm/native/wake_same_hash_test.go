package native

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apiUtils "tiramisu/internal/gostorm/web/api/utils"
)

// This file covers the audio-same-hash-wake slice: Wake callers that target
// sibling files of one torrent must not run their activation bodies in
// parallel, while distinct torrents keep running concurrently under the
// existing 25-call admission bound.
//
// Every scenario drives the real NativeClient.Wake from real goroutines and
// observes two things through client-local seams (see
// .tdd-state/audio-same-hash-wake/seam-decision.md):
//
//   - wakeActivation: the activation body. It reports "entered" on the events
//     channel before running the test-controlled body.
//   - sameHashWaitHook: called by a caller that is about to block behind an
//     occupied canonical-hash lane. It reports "queued" on the same channel.
//
// Contention is therefore proven by channel receipts, not by timing: a test
// that wants "N callers are queued behind one held holder" receives N queued
// events while treating any additional entered event as an immediate failure.
// Both outcomes are always reached by progress, never by waiting out a clock;
// admissionFailSafe only bounds how long a hung test may run.

var errSameHashBoom = errors.New("same-hash activation failed")

// hashN returns the n-th synthetic, valid 40-hex info hash.
func hashN(n int) string { return fmt.Sprintf("%040x", n+1) }

// magnetSpellings returns different spellings of a magnet for the same
// canonical info hash: parameter order, title, trackers, webseeds, hex case
// and base32 encoding all differ, but the info hash does not.
func magnetSpellings(t *testing.T, hash string) []string {
	t.Helper()
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != 20 {
		t.Fatalf("fixture hash %q is not 20 hex bytes: %v", hash, err)
	}
	b32 := base32.StdEncoding.EncodeToString(raw)
	return []string{
		"magnet:?xt=urn:btih:" + hash,
		"magnet:?xt=urn:btih:" + hash + "&dn=Pride+and+Prejudice&tr=http%3A%2F%2Ftracker.invalid%2Fannounce",
		"magnet:?tr=http%3A%2F%2Fother.invalid%2Fannounce&dn=Chapter+01&xt=urn:btih:" + hash,
		"magnet:?dn=Other+Title&xt=urn:btih:" + hash + "&ws=http%3A%2F%2Fseed.invalid%2Ffile.mp3",
		"magnet:?xt=urn:btih:" + strings.ToUpper(hash),
		"magnet:?xt=urn:btih:" + b32 + "&tr=udp%3A%2F%2Ftracker.invalid%3A1337",
		"magnet:?ws=http%3A%2F%2Fseed.invalid%2Fa&ws=http%3A%2F%2Fseed.invalid%2Fb&tr=http%3A%2F%2Fa.invalid%2Fannounce&tr=http%3A%2F%2Fb.invalid%2Fannounce&xt=urn:btih:" + hash,
	}
}

// laneProbe records same-hash activation concurrency from inside activation
// bodies.
type laneProbe struct {
	mu      sync.Mutex
	active  map[string]int
	peak    map[string]int
	entries map[string]int
}

func newLaneProbe() *laneProbe {
	return &laneProbe{active: map[string]int{}, peak: map[string]int{}, entries: map[string]int{}}
}

func (p *laneProbe) enter(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[hash]++
	p.entries[hash]++
	if p.active[hash] > p.peak[hash] {
		p.peak[hash] = p.active[hash]
	}
}

func (p *laneProbe) exit(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[hash]--
}

func (p *laneProbe) peakFor(hash string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak[hash]
}

func (p *laneProbe) entriesFor(hash string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[hash]
}

// laneEvent is one observation from a Wake caller: it either entered its
// activation body or reported being queued behind a same-hash lane.
type laneEvent struct {
	entered bool
	hash    string
	idx     int // valid for entered events only
}

// newSameHashClient builds a client whose activation body is `body`, wrapped
// so that concurrency per canonical hash is recorded and every entry and every
// queued wait is reported on the returned events channel. The canonical hash
// is derived independently of the production code via the shared link parser.
func newSameHashClient(t *testing.T, body func(ctx context.Context, hash string, idx int) error) (*NativeClient, *laneProbe, chan laneEvent) {
	t.Helper()
	probe := newLaneProbe()
	events := make(chan laneEvent, 512)
	c := NewNativeClient()
	c.sameHashWaitHook = func(hash string) { events <- laneEvent{hash: hash} }
	c.wakeActivation = func(ctx context.Context, magnetURL string, idx int) error {
		spec, err := apiUtils.ParseLink(magnetURL)
		if err != nil {
			t.Errorf("fixture magnet %q did not parse: %v", magnetURL, err)
			return err
		}
		hash := spec.InfoHash.HexString()
		probe.enter(hash)
		defer probe.exit(hash)
		events <- laneEvent{entered: true, hash: hash, idx: idx}
		return body(ctx, hash, idx)
	}
	return c, probe, events
}

func afterFailSafe() <-chan time.Time { return time.After(admissionFailSafe) }

func wakeAsync(c *NativeClient, ctx context.Context, magnet string, idx int) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- c.Wake(ctx, magnet, idx) }()
	return ch
}

func recvErr(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-afterFailSafe():
		t.Fatalf("%s: Wake did not return within the fail-safe bound", what)
		return nil
	}
}

func recvEvent(t *testing.T, what string, events <-chan laneEvent) laneEvent {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-afterFailSafe():
		t.Fatalf("%s: expected event never arrived within the fail-safe bound", what)
		return laneEvent{}
	}
}

// awaitEntered returns the index of the next caller to enter activation,
// ignoring queued-wait reports.
func awaitEntered(t *testing.T, what string, events <-chan laneEvent) int {
	t.Helper()
	for {
		if ev := recvEvent(t, what, events); ev.entered {
			return ev.idx
		}
	}
}

// awaitContention blocks until `wantEntered` callers have entered activation
// and `wantQueued` callers (all for `hash`) have reported being queued behind
// the same-hash lane. Any entry beyond wantEntered is an immediate failure:
// that caller ran an activation body while the lane's holder was still held.
// It returns the indexes of the callers that entered.
func awaitContention(t *testing.T, what, hash string, events <-chan laneEvent, wantEntered, wantQueued int) []int {
	t.Helper()
	var entered []int
	queued := 0
	for len(entered) < wantEntered || queued < wantQueued {
		ev := recvEvent(t, fmt.Sprintf("%s (have %d/%d entered, %d/%d queued)", what, len(entered), wantEntered, queued, wantQueued), events)
		if ev.entered {
			entered = append(entered, ev.idx)
			if len(entered) > wantEntered {
				t.Fatalf("%s: caller %d entered activation while the same-hash lane was still held (%d queued so far)", what, ev.idx, queued)
			}
			continue
		}
		if ev.hash != hash {
			t.Fatalf("%s: queued on hash %q, want the canonical hash %q", what, ev.hash, hash)
		}
		queued++
	}
	return entered
}

// finishQueuedSuccess completes a queued caller whose predecessor succeeded.
// Both conforming outcomes are accepted: the caller runs its own serialized
// activation (its body is released here), or it returns the shared success
// without running one. Either way it must return nil, and no other caller may
// enter in its place.
func finishQueuedSuccess(t *testing.T, what string, events <-chan laneEvent, res <-chan error, idx int, release chan struct{}) {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if !ev.entered {
				continue
			}
			if ev.idx != idx {
				t.Fatalf("%s: caller %d entered activation, want only caller %d", what, ev.idx, idx)
			}
			close(release)
		case err := <-res:
			if err != nil {
				t.Fatalf("%s: Wake() error = %v, want nil after a successful predecessor", what, err)
			}
			return
		case <-afterFailSafe():
			t.Fatalf("%s: caller did not finish after its predecessor succeeded", what)
		}
	}
}

type callResult struct {
	idx int
	err error
}

// A1 + A2 + I1: 35 real Wake callers for sibling files of one torrent, using
// seven different spellings of the same magnet. Of the 35, 25 hold global
// permits: one enters activation and the other 24 must be observed queued
// behind it (with no second entry) before the holder is released. After that
// every one of the 35 public calls must return without error and no two
// same-hash activations may ever overlap.
func TestSameHashWake_35SiblingCallersNeverOverlapAndAllProgress_A1_A2_I1(t *testing.T) {
	const total, permits = 35, 25
	hash := hashN(1)
	spellings := magnetSpellings(t, hash)

	step := make(chan struct{})
	c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
		<-step
		return nil
	})

	results := make(chan callResult, total)
	for i := 0; i < total; i++ {
		i := i
		go func() { results <- callResult{i, c.Wake(context.Background(), spellings[i%len(spellings)], i)} }()
	}

	holders := awaitContention(t, "35 sibling callers", hash, events, 1, permits-1)
	t.Logf("holder = caller %d", holders[0])

	// Release the holder, then drive every later entrant through activation
	// one at a time until all 35 public calls have returned.
	step <- struct{}{}
	returned := 0
	for returned < total {
		select {
		case ev := <-events:
			if ev.entered {
				step <- struct{}{}
			}
		case r := <-results:
			returned++
			if r.err != nil {
				t.Errorf("caller %d: Wake() error = %v, want nil (no coalescing/saturation error)", r.idx, r.err)
			}
		case <-afterFailSafe():
			t.Fatalf("only %d/%d Wake calls returned after the holder was released", returned, total)
		}
	}

	if got := probe.peakFor(hash); got != 1 {
		t.Errorf("peak concurrent same-hash activations = %d, want exactly 1", got)
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("occupied admission tokens after all callers returned = %d, want 0", got)
	}
}

// I1: canonical-hash lane identity table. Each spelling names the same
// torrent as the plain magnet, so a caller using it must be observed queued
// behind the held plain-magnet caller (and must not enter) until release.
func TestSameHashWake_LaneIdentityIsCanonicalHash_I1(t *testing.T) {
	hash := hashN(2)
	spellings := magnetSpellings(t, hash)
	for i := 1; i < len(spellings); i++ {
		i := i
		t.Run(fmt.Sprintf("spelling_%d_shares_lane_with_plain_magnet", i), func(t *testing.T) {
			release := map[int]chan struct{}{0: make(chan struct{}), 1: make(chan struct{})}
			c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
				<-release[idx]
				return nil
			})

			first := wakeAsync(c, context.Background(), spellings[0], 0)
			if got := awaitEntered(t, "first caller", events); got != 0 {
				t.Fatalf("first entrant = %d, want 0", got)
			}
			second := wakeAsync(c, context.Background(), spellings[i], 1)
			awaitContention(t, "second spelling behind first", hash, events, 0, 1)

			close(release[0])
			if err := recvErr(t, "first caller", first); err != nil {
				t.Fatalf("first caller: Wake() error = %v", err)
			}
			finishQueuedSuccess(t, "second spelling after release", events, second, 1, release[1])
			if got := probe.peakFor(hash); got != 1 {
				t.Errorf("peak concurrent activations across spellings = %d, want 1", got)
			}
		})
	}
}

// A3: distinct hashes are never serialized against each other. n callers with
// n different hashes must all be inside activation at the same time, and none
// of them may report being queued.
func TestSameHashWake_DistinctHashesActivateConcurrently_A3(t *testing.T) {
	for _, n := range []int{2, 25} {
		n := n
		t.Run(fmt.Sprintf("%d_distinct_hashes", n), func(t *testing.T) {
			hold := make(chan struct{})
			c, _, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
				<-hold
				return nil
			})
			results := make([]<-chan error, n)
			for i := 0; i < n; i++ {
				results[i] = wakeAsync(c, context.Background(), magnetSpellings(t, hashN(100+i))[0], i)
			}
			for k := 0; k < n; k++ {
				ev := recvEvent(t, fmt.Sprintf("distinct-hash entry %d/%d (same-hash serialization must not be a global mutex)", k+1, n), events)
				if !ev.entered {
					t.Fatalf("distinct hash %q reported queued; distinct hashes must never share a lane", ev.hash)
				}
			}
			if got := len(c.wakeSemaphore); got != n {
				t.Errorf("occupied admission tokens with %d simultaneous activations = %d, want %d", n, got, n)
			}
			close(hold)
			for i := 0; i < n; i++ {
				if err := recvErr(t, fmt.Sprintf("caller %d", i), results[i]); err != nil {
					t.Errorf("caller %d: Wake() error = %v, want nil", i, err)
				}
			}
		})
	}
}

// A3: a held hot hash (with a sibling behind it) must not delay a caller for
// a different hash. Entries are keyed by hash: the test keeps receiving until
// the other hash has entered, while the hot holder is provably still blocked.
// Same-hash exclusion itself is asserted by the A1/I1 tests, not here.
func TestSameHashWake_BusyLaneDoesNotDelayOtherHash_A3(t *testing.T) {
	hashHot, hashOther := hashN(3), hashN(4)
	holdHot := make(chan struct{})
	c, _, events := newSameHashClient(t, func(ctx context.Context, hash string, idx int) error {
		if hash == hashHot {
			<-holdHot
		}
		return nil
	})

	holder := wakeAsync(c, context.Background(), magnetSpellings(t, hashHot)[0], 0)
	if ev := recvEvent(t, "hot holder", events); !ev.entered || ev.hash != hashHot {
		t.Fatalf("first event = %+v, want the hot holder entering", ev)
	}
	sibling := wakeAsync(c, context.Background(), magnetSpellings(t, hashHot)[1], 1)
	other := wakeAsync(c, context.Background(), magnetSpellings(t, hashOther)[0], 2)

	for sawOther := false; !sawOther; {
		ev := recvEvent(t, "other-hash caller while hot holder is still held", events)
		sawOther = ev.entered && ev.hash == hashOther
		if ev.hash == hashOther && !ev.entered {
			t.Fatalf("other hash reported queued while only the hot hash was held")
		}
	}
	if err := recvErr(t, "other-hash caller", other); err != nil {
		t.Fatalf("other-hash caller: Wake() error = %v, want nil while hot holder is blocked", err)
	}

	close(holdHot)
	if err := recvErr(t, "hot holder", holder); err != nil {
		t.Errorf("hot holder: Wake() error = %v", err)
	}
	if err := recvErr(t, "hot sibling", sibling); err != nil {
		t.Errorf("hot sibling: Wake() error = %v", err)
	}
}

// A3 adversarial: a full cohort of same-hash waiters (at least 25, the size of
// the global capacity) observed at the hash boundary must not consume the
// global activation permits. While the hot holder is still blocked, a caller
// for a different hash must enter activation and finish.
func TestSameHashWake_HotHashWaiterCohortDoesNotStarveOtherHash_A3(t *testing.T) {
	const waiters = 25
	hashHot, hashOther := hashN(9), hashN(10)
	hotSpellings := magnetSpellings(t, hashHot)
	holdHot := make(chan struct{})
	c, probe, events := newSameHashClient(t, func(ctx context.Context, hash string, idx int) error {
		if hash == hashHot {
			<-holdHot
		}
		return nil
	})

	results := make(chan callResult, waiters+2)
	go func() { results <- callResult{0, c.Wake(context.Background(), hotSpellings[0], 0)} }()
	if ev := recvEvent(t, "hot holder", events); !ev.entered || ev.hash != hashHot {
		t.Fatalf("first event = %+v, want the hot holder entering", ev)
	}
	for i := 1; i <= waiters; i++ {
		i := i
		go func() { results <- callResult{i, c.Wake(context.Background(), hotSpellings[i%len(hotSpellings)], i)} }()
	}
	awaitContention(t, "25 hot waiters behind the holder", hashHot, events, 0, waiters)

	go func() {
		results <- callResult{1000, c.Wake(context.Background(), magnetSpellings(t, hashOther)[0], 1000)}
	}()
	for entered := false; !entered; {
		ev := recvEvent(t, "other hash while 25 hot waiters are queued and the hot holder is blocked", events)
		if ev.hash == hashHot && ev.entered {
			t.Fatalf("hot caller %d entered while the hot holder was still blocked", ev.idx)
		}
		if ev.hash == hashOther && !ev.entered {
			t.Fatalf("other hash reported queued behind an unrelated hot lane")
		}
		entered = ev.hash == hashOther && ev.entered
	}

	close(holdHot)
	for returned := 0; returned < waiters+2; returned++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("caller %d: Wake() error = %v, want nil", r.idx, r.err)
			}
		case <-afterFailSafe():
			t.Fatalf("only %d/%d callers returned after the hot holder was released", returned, waiters+2)
		}
	}
	if got := probe.peakFor(hashHot); got != 1 {
		t.Errorf("peak concurrent hot-hash activations = %d, want 1", got)
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("occupied admission tokens after all callers returned = %d, want 0", got)
	}
}

// I3 (with A3): distinct-hash callers stay bounded by the global capacity of
// 25. 30 distinct-hash callers: exactly 25 enter; the rest enter only as
// tokens free up.
func TestSameHashWake_DistinctHashesRemainBoundedBy25_I3(t *testing.T) {
	const total, permits = 30, 25
	release := make(chan struct{}, total)
	c, _, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
		<-release
		return nil
	})
	results := make([]<-chan error, total)
	for i := 0; i < total; i++ {
		results[i] = wakeAsync(c, context.Background(), magnetSpellings(t, hashN(200+i))[0], i)
	}
	for k := 0; k < permits; k++ {
		if ev := recvEvent(t, fmt.Sprintf("admitted entry %d/%d", k+1, permits), events); !ev.entered {
			t.Fatalf("distinct hash %q reported queued", ev.hash)
		}
	}
	// All 25 tokens are held by blocked activations; entering a 26th body
	// would need a 26th token.
	select {
	case ev := <-events:
		t.Fatalf("event %+v after 25 concurrent activations; the 25-call bound was lost", ev)
	default:
	}
	release <- struct{}{}
	awaitEntered(t, "26th caller after one release", events)
	for k := 1; k < total; k++ {
		release <- struct{}{}
	}
	for i := 0; i < total; i++ {
		if err := recvErr(t, fmt.Sprintf("caller %d", i), results[i]); err != nil {
			t.Errorf("caller %d: Wake() error = %v, want nil", i, err)
		}
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("occupied admission tokens after all callers returned = %d, want 0", got)
	}
}

// E1: two same-hash callers are cancelled while observed queued behind a held
// holder (with two healthy callers queued beside them). The cancelled callers
// return their context error and never enter activation; the healthy ones
// still get through after release.
func TestSameHashWake_CancelledQueuedCallersNeverEnterAndDoNotBlockHealthy_E1(t *testing.T) {
	hash := hashN(5)
	spellings := magnetSpellings(t, hash)
	release := make([]chan struct{}, 5)
	for i := range release {
		release[i] = make(chan struct{})
	}
	c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
		select {
		case <-release[idx]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	holder := wakeAsync(c, context.Background(), spellings[0], 0)
	if got := awaitEntered(t, "holder", events); got != 0 {
		t.Fatalf("first entrant = %d, want holder 0", got)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()
	cancelled1 := wakeAsync(c, ctx1, spellings[1], 1)
	cancelled2 := wakeAsync(c, ctx2, spellings[2], 2)
	healthy3 := wakeAsync(c, context.Background(), spellings[3], 3)
	healthy4 := wakeAsync(c, context.Background(), spellings[4], 4)

	// All four are demonstrably at the same-hash wait, behind a held holder.
	awaitContention(t, "four waiters behind the holder", hash, events, 0, 4)

	cancel1()
	cancel2()
	for i, ch := range []<-chan error{cancelled1, cancelled2} {
		if err := recvErr(t, fmt.Sprintf("cancelled waiter %d", i+1), ch); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter %d: Wake() error = %v, want context.Canceled", i+1, err)
		}
	}

	close(release[0])
	if err := recvErr(t, "holder", holder); err != nil {
		t.Fatalf("holder: Wake() error = %v", err)
	}

	// After a successful holder each healthy waiter either runs its own
	// serialized activation or returns the shared success.
	healthyDone := make(chan callResult, 2)
	for idx, ch := range map[int]<-chan error{3: healthy3, 4: healthy4} {
		idx, ch := idx, ch
		go func() { healthyDone <- callResult{idx, <-ch} }()
	}
	for returned := 0; returned < 2; {
		select {
		case ev := <-events:
			if !ev.entered {
				continue
			}
			if ev.idx == 1 || ev.idx == 2 {
				t.Fatalf("cancelled caller %d entered activation", ev.idx)
			}
			close(release[ev.idx])
		case r := <-healthyDone:
			returned++
			if r.err != nil {
				t.Errorf("healthy waiter %d: Wake() error = %v, want nil", r.idx, r.err)
			}
		case <-afterFailSafe():
			t.Fatalf("healthy waiters did not finish after release (%d/2 done)", returned)
		}
	}
	if got := probe.peakFor(hash); got != 1 {
		t.Errorf("peak concurrent same-hash activations = %d, want 1", got)
	}
	if got := len(c.wakeSemaphore); got != 0 {
		t.Errorf("occupied admission tokens after all callers returned = %d, want 0", got)
	}
}

// E2: an already-cancelled caller returns its context error immediately, even
// when its lane is busy and/or global capacity is full, without occupying a
// permit or the lane (it is never reported queued). After the holder exits, a
// later caller enters.
func TestSameHashWake_AlreadyCancelledCallerOccupiesNothing_E2(t *testing.T) {
	cases := []struct {
		name       string
		prefill    int
		wantTokens int
	}{
		{"lane_busy", 0, 1},
		{"lane_busy_and_global_capacity_full", 24, 25},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			hash := hashN(6)
			spellings := magnetSpellings(t, hash)
			release := map[int]chan struct{}{0: make(chan struct{}), 1: make(chan struct{}), 2: make(chan struct{})}
			c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
				<-release[idx]
				return nil
			})
			fillWakeSemaphore(t, c, tc.prefill)

			holder := wakeAsync(c, context.Background(), spellings[0], 0)
			awaitEntered(t, "holder", events)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := recvErr(t, "already-cancelled caller", wakeAsync(c, ctx, spellings[1], 1))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("already-cancelled caller: Wake() error = %v, want context.Canceled", err)
			}
			select {
			case ev := <-events:
				t.Fatalf("already-cancelled caller produced event %+v; it must not touch the lane", ev)
			default:
			}
			if got := len(c.wakeSemaphore); got != tc.wantTokens {
				t.Errorf("occupied tokens after already-cancelled call = %d, want %d (no permit taken)", got, tc.wantTokens)
			}
			if got := probe.entriesFor(hash); got != 1 {
				t.Errorf("activations executed = %d, want 1 (cancelled caller never entered)", got)
			}

			close(release[0])
			if err := recvErr(t, "holder", holder); err != nil {
				t.Fatalf("holder: Wake() error = %v", err)
			}
			later := wakeAsync(c, context.Background(), spellings[2], 2)
			if got := awaitEntered(t, "later caller", events); got != 2 {
				t.Fatalf("later entrant = %d, want 2", got)
			}
			close(release[2])
			if err := recvErr(t, "later caller", later); err != nil {
				t.Errorf("later caller: Wake() error = %v, want nil", err)
			}
		})
	}
}

// I2: every class of activation exit releases the same-hash lane. For each
// class, a same-hash waiter observed queued behind the first caller must
// enter only after that exit, and a brand-new caller afterwards must too.
// Errors reach the exiting caller unrewritten (E3).
func TestSameHashWake_EveryExitClassReleasesLane_I2(t *testing.T) {
	cases := []struct {
		name    string
		wantErr error
		cancel  bool
	}{
		{name: "success"},
		{name: "ordinary_error", wantErr: errSameHashBoom},
		{name: "timeout_shaped_error", wantErr: errWakeAdmissionTimeoutShaped},
		{name: "admitted_context_cancellation", wantErr: context.Canceled, cancel: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			hash := hashN(7)
			spellings := magnetSpellings(t, hash)
			release := map[int]chan struct{}{0: make(chan struct{}), 1: make(chan struct{}), 2: make(chan struct{})}
			c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
				if idx == 0 {
					if tc.cancel {
						<-ctx.Done()
						return ctx.Err()
					}
					<-release[0]
					return tc.wantErr
				}
				<-release[idx]
				return nil
			})

			ctx0, cancel0 := context.WithCancel(context.Background())
			defer cancel0()
			first := wakeAsync(c, ctx0, spellings[0], 0)
			awaitEntered(t, "first caller", events)
			waiter := wakeAsync(c, context.Background(), spellings[1], 1)
			awaitContention(t, "waiter behind first caller", hash, events, 0, 1)

			if tc.cancel {
				cancel0()
			} else {
				close(release[0])
			}
			err := recvErr(t, "first caller", first)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("first caller: Wake() error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("first caller: Wake() error = %v, want %v unrewritten", err, tc.wantErr)
			}

			if tc.wantErr == nil {
				// Successful predecessor: serialized own entry or shared success.
				finishQueuedSuccess(t, "queued waiter after successful first exit", events, waiter, 1, release[1])
			} else {
				// Failed or cancelled predecessor: a fresh activation is required.
				if got := awaitEntered(t, "queued waiter after failed first exit", events); got != 1 {
					t.Fatalf("entrant after first exit = %d, want queued waiter 1", got)
				}
				close(release[1])
				if err := recvErr(t, "queued waiter", waiter); err != nil {
					t.Fatalf("queued waiter: Wake() error = %v, want nil", err)
				}
			}

			later := wakeAsync(c, context.Background(), spellings[2], 2)
			if got := awaitEntered(t, "later caller", events); got != 2 {
				t.Fatalf("later entrant = %d, want 2", got)
			}
			close(release[2])
			if err := recvErr(t, "later caller", later); err != nil {
				t.Errorf("later caller: Wake() error = %v, want nil", err)
			}
			if got := probe.peakFor(hash); got != 1 {
				t.Errorf("peak concurrent same-hash activations = %d, want 1", got)
			}
			if got := len(c.wakeSemaphore); got != 0 {
				t.Errorf("occupied admission tokens after all callers returned = %d, want 0", got)
			}
		})
	}
}

// E3: a failing activation does not poison later callers. The caller observed
// queued behind the failing one runs a fresh activation of its own after the
// failure and does not inherit the neighbour's error, and a later independent
// retry is admitted and runs a fresh activation as well.
func TestSameHashWake_ActivationErrorIsNotSharedOrPoisoning_E3(t *testing.T) {
	hash := hashN(8)
	spellings := magnetSpellings(t, hash)
	failFirst := make(chan struct{})
	c, probe, events := newSameHashClient(t, func(ctx context.Context, _ string, idx int) error {
		if idx == 0 {
			<-failFirst
			return fmt.Errorf("add torrent error: %w", errSameHashBoom)
		}
		return nil
	})

	first := wakeAsync(c, context.Background(), spellings[0], 0)
	awaitEntered(t, "first caller", events)
	queued := wakeAsync(c, context.Background(), spellings[1], 1)
	awaitContention(t, "waiter behind failing caller", hash, events, 0, 1)
	close(failFirst)

	err := recvErr(t, "first caller", first)
	if !errors.Is(err, errSameHashBoom) || !strings.Contains(err.Error(), "add torrent error") {
		t.Fatalf("first caller: Wake() error = %v, want the activation error preserved verbatim", err)
	}
	if got := awaitEntered(t, "queued caller after failure", events); got != 1 {
		t.Fatalf("entrant after failure = %d, want a fresh activation for queued caller 1", got)
	}
	if err := recvErr(t, "queued caller", queued); err != nil {
		t.Errorf("queued caller: Wake() error = %v, want nil (its own activation succeeded)", err)
	}

	retry := wakeAsync(c, context.Background(), spellings[2], 2)
	if got := awaitEntered(t, "retry", events); got != 2 {
		t.Fatalf("retry entrant = %d, want a fresh activation for retry 2", got)
	}
	if err := recvErr(t, "retry", retry); err != nil {
		t.Errorf("retry after failure: Wake() error = %v, want nil", err)
	}
	if got := probe.peakFor(hash); got != 1 {
		t.Errorf("peak concurrent same-hash activations = %d, want 1", got)
	}
}

// E3: parse errors from the real (unseamed) path keep their identity and are
// not rewritten as coalescing/admission errors; nothing is left occupied.
func TestSameHashWake_ParseErrorsArePreserved_E3(t *testing.T) {
	links := map[string]string{
		"unknown_scheme":       invalidWakeLink,
		"non_hex_infohash":     "magnet:?xt=urn:btih:not-a-valid-hash",
		"magnet_without_xt":    "magnet:?dn=no-hash-here",
		"too_short_infohash":   "magnet:?xt=urn:btih:abcd",
		"empty_link":           "",
		"whitespace_only_link": "   ",
	}
	for name, link := range links {
		link := link
		t.Run(name, func(t *testing.T) {
			c := NewNativeClient()
			var first string
			for attempt := 0; attempt < 2; attempt++ {
				err := c.Wake(context.Background(), link, 1)
				if err == nil {
					t.Fatalf("attempt %d: Wake(%q) error = nil, want a parse error", attempt, link)
				}
				msg := err.Error()
				if !strings.Contains(msg, "parse link error") {
					t.Errorf("attempt %d: error = %q, want the parse link error preserved", attempt, msg)
				}
				if errors.Unwrap(err) == nil {
					t.Errorf("attempt %d: error %q lost its wrapped cause", attempt, msg)
				}
				for _, banned := range []string{"coalesc", "lane", "exhausted", "saturat", "busy", "admission"} {
					if strings.Contains(strings.ToLower(msg), banned) {
						t.Errorf("attempt %d: error %q was rewritten as an internal %q error", attempt, msg, banned)
					}
				}
				if attempt == 0 {
					first = msg
				} else if msg != first {
					t.Errorf("retry error = %q, want identical to first %q (a failure must not poison retries)", msg, first)
				}
				if got := len(c.wakeSemaphore); got != 0 {
					t.Errorf("attempt %d: occupied tokens = %d, want 0", attempt, got)
				}
			}
		})
	}
}
