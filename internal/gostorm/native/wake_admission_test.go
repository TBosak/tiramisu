package native

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// This file covers the audio-wake-admission slice's admission-boundary
// behavior using the client-local test seams documented in
// .tdd-state/audio-wake-admission/seam-decision.md:
// NativeClient.beforeWakeAdmission (called synchronously immediately before
// the real semaphore acquisition) and NativeClient.wakeActivation (replaces
// only the post-admission body when non-nil, after the post-acquire context
// check). Both are nil in production; NewNativeClient never sets them.
//
// Every ordering-sensitive assertion below is built from a `gate`: a hook
// that rendezvous-signals arrival (a real happens-before edge, not a
// scheduling guess) and then blocks until the test releases it. Because the
// hook itself is the only thing standing between the caller and the real,
// unmodified admission/activation code, releasing a gate lets that caller
// attempt the real code with the semaphore in exactly the state the test put
// it in -- no runtime.Gosched, no time.Sleep, no dependence on which
// goroutine the scheduler happens to run first.

const admissionFailSafe = 5 * time.Second

// gate is a one-shot rendezvous + barrier. wait() is called from inside a
// Wake caller's goroutine (as beforeWakeAdmission or wakeActivation): it
// blocks the caller at that exact point until the test calls release().
// waitArrived blocks the test until wait() has been entered -- proving the
// caller is now blocked there and cannot have progressed any further, since
// the only way out of wait() is release().
type gate struct {
	arrived chan struct{}
	proceed chan struct{}
}

func newGate() *gate {
	return &gate{arrived: make(chan struct{}), proceed: make(chan struct{})}
}

func (g *gate) wait() {
	g.arrived <- struct{}{}
	<-g.proceed
}

func (g *gate) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(admissionFailSafe):
		t.Fatal("gate: hook did not rendezvous within the fail-safe bound")
	}
}

func (g *gate) release() { close(g.proceed) }

// fillWakeSemaphore occupies n of the client's admission tokens directly,
// modelling n already-admitted Wake calls without running them. This is a
// plain, ordered sequence of channel sends on the calling (test) goroutine,
// performed before any concurrent Wake call exists, so it needs no
// synchronization of its own.
func fillWakeSemaphore(t *testing.T, c *NativeClient, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case c.wakeSemaphore <- struct{}{}:
		default:
			t.Fatalf("fillWakeSemaphore: could not occupy token %d/%d; capacity already exhausted", i+1, n)
		}
	}
}

// releaseWakeSemaphore hands back one occupied token, exactly like Wake's
// own deferred release, freeing one admission slot for a waiting caller.
func releaseWakeSemaphore(t *testing.T, c *NativeClient) {
	t.Helper()
	select {
	case <-c.wakeSemaphore:
	default:
		t.Fatalf("releaseWakeSemaphore: no occupied token available to release")
	}
}

func isWakeSentinelError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exhausted")
}

// A3 (structural half): the admission bound is fixed at 25 by construction
// -- NewNativeClient allocates wakeSemaphore with exactly that capacity, so
// no implementation can hold more than 25 tokens at once regardless of how
// admission is scheduled. Deterministic, no goroutines involved.
func TestWakeAdmission_SemaphoreCapacityIsExactly25_A3(t *testing.T) {
	c := NewNativeClient()
	if got := cap(c.wakeSemaphore); got != 25 {
		t.Fatalf("cap(wakeSemaphore) = %d, want 25", got)
	}
	fillWakeSemaphore(t, c, 25)
	select {
	case c.wakeSemaphore <- struct{}{}:
		t.Fatal("a 26th token was accepted into a semaphore already holding 25; the bound is not enforced")
	default:
	}
}

// E1: an already-cancelled context returns promptly with its context error
// and consumes no permit, even when every permit is already occupied. The
// ordering needs no seam: cancel() completes on the test goroutine, in
// program order, strictly before the `go` statement that starts the
// worker -- the Go memory model gives a real happens-before edge from
// everything before a `go` statement to the start of the spawned goroutine.
func TestWakeAdmission_AlreadyCancelledContextConsumesNoPermitWhenSaturated_E1(t *testing.T) {
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 25)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- c.Wake(ctx, invalidWakeLink, 1) }()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wake() error = %v, want context.Canceled", err)
	}
	if got := len(c.wakeSemaphore); got != 25 {
		t.Errorf("occupied tokens after an already-cancelled call = %d, want unchanged at 25", got)
	}
}

// A2/A3 (behavioral): 32 real Wake callers contend for the real 25-token
// semaphore. All 32 are held at the real admission boundary (via
// beforeWakeAdmission) until every one of them has genuinely arrived there,
// then released together. Exactly 25 can hold a real token at once --
// wakeActivation proves this by holding every admitted caller at a second
// barrier and letting the test count concurrent entries, with a
// non-blocking check that a 26th cannot structurally have entered while
// none of the 25 have released (entering wakeActivation requires holding
// one of exactly 25 tokens). Finally all 32 are let through and must all
// finish without the internal saturation error.
func TestWakeAdmission_32CallersBoundedActivationThenAllProgress_A2_A3(t *testing.T) {
	const total = 32
	const permits = 25

	c := NewNativeClient()

	admission := newGate()
	c.beforeWakeAdmission = admission.wait

	entered := make(chan struct{}, total)
	activationProceed := make(chan struct{})
	c.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		entered <- struct{}{}
		<-activationProceed // "entered" already reports arrival; no separate handshake needed here
		return nil
	}

	type callResult struct {
		idx int
		err error
	}
	resultCh := make(chan callResult, total)
	for i := 0; i < total; i++ {
		i := i
		go func() {
			err := c.Wake(context.Background(), invalidWakeLink, i)
			resultCh <- callResult{i, err}
		}()
	}

	// None of the 32 may proceed past the real admission point until every
	// one of them has arrived there -- otherwise "at most 25 concurrent"
	// would just be an artifact of how fast goroutines happened to start.
	for i := 0; i < total; i++ {
		admission.waitArrived(t)
	}
	admission.release()

	// Exactly 25 of the 32 can hold a real token at once; the channel's own
	// capacity makes this unconditional, not a timing accident.
	for i := 0; i < permits; i++ {
		select {
		case <-entered:
		case <-time.After(admissionFailSafe):
			t.Fatalf("only %d/%d callers reached activation within the fail-safe bound", i, permits)
		}
	}
	// With none of those 25 released yet, a 26th cannot structurally have
	// entered: entering wakeActivation requires holding one of exactly 25
	// tokens, all of which are still held by the goroutines blocked above.
	select {
	case <-entered:
		t.Fatal("more than 25 callers were concurrently inside activation -- the 25-permit bound was not enforced")
	default:
	}

	close(activationProceed)

	for i := 0; i < total; i++ {
		select {
		case r := <-resultCh:
			if isWakeSentinelError(r.err) {
				t.Errorf("caller %d: Wake() error = %v, want no exhaustion error once all 32 are allowed to progress (at least 32 concurrent audiobook-part opens must all make progress)", r.idx, r.err)
			}
		case <-time.After(admissionFailSafe):
			t.Fatalf("only received %d/%d results within the fail-safe bound", i, total)
		}
	}
}

// I2/E2: a caller cancelled strictly while genuinely queued -- rendezvoused
// at the real admission boundary with capacity provably full throughout,
// never released during this call -- must return its context error, never
// the internal saturation error, and must never reach activation. The
// current select has no ctx.Done() case at all, so whenever capacity is
// full at the moment it runs, the result is deterministically the sentinel
// regardless of when cancellation happened -- this needs no scheduling
// assumption because capacity never changes during the call.
func TestWakeAdmission_CancelledWhileGenuinelyQueuedNeverEntersActivation_I2_E2(t *testing.T) {
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 25)

	admission := newGate()
	c.beforeWakeAdmission = admission.wait

	entered := make(chan struct{}, 1)
	c.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		entered <- struct{}{}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() { resultCh <- c.Wake(ctx, invalidWakeLink, 1) }()

	admission.waitArrived(t) // reached the real admission point; capacity is still 25/25
	cancel()                 // cancel while genuinely queued, capacity still full
	admission.release()      // let it attempt the real, unmodified select now

	err := <-resultCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wake() error = %v, want context.Canceled for a caller cancelled while genuinely queued (capacity full throughout the call)", err)
	}
	if isWakeSentinelError(err) {
		t.Fatalf("Wake() error = %v, cancellation while queued must never surface the internal saturation error", err)
	}

	select {
	case <-entered:
		t.Fatal("a cancelled queued caller reached activation")
	default:
	}

	// Release capacity afterward, for hygiene -- the call above has already
	// returned and cannot be retroactively affected, and the check just
	// above already proves it never entered activation regardless.
	for i := 0; i < 25; i++ {
		releaseWakeSemaphore(t, c)
	}
}

// I3: one cancelled waiter cannot prevent later independent callers from
// acquiring permits released after it. The cancelled waiter is resolved
// deterministically first (as in I2/E2 above); the healthy callers are then
// rendezvoused at the real admission boundary, capacity is released for
// them specifically, and only then are they let through -- so their
// admission is never a race against the cancellation.
func TestWakeAdmission_CancelledWaiterDoesNotBlockHealthyCallers_I3(t *testing.T) {
	const healthy = 5
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 25)

	cancelledGate := newGate()
	c.beforeWakeAdmission = cancelledGate.wait
	ctx, cancel := context.WithCancel(context.Background())
	cancelledResult := make(chan error, 1)
	go func() { cancelledResult <- c.Wake(ctx, invalidWakeLink, 0) }()
	cancelledGate.waitArrived(t)
	cancel()
	cancelledGate.release()
	if err := <-cancelledResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: Wake() error = %v, want context.Canceled", err)
	}

	healthyGate := newGate()
	c.beforeWakeAdmission = healthyGate.wait // safe: the cancelled call above has already returned
	resultCh := make(chan error, healthy)
	for i := 0; i < healthy; i++ {
		i := i
		go func() { resultCh <- c.Wake(context.Background(), invalidWakeLink, i+1) }()
	}
	for i := 0; i < healthy; i++ {
		healthyGate.waitArrived(t)
	}
	for i := 0; i < healthy; i++ {
		releaseWakeSemaphore(t, c)
	}
	healthyGate.release()

	for i := 0; i < healthy; i++ {
		select {
		case err := <-resultCh:
			if isWakeSentinelError(err) {
				t.Errorf("healthy caller: Wake() error = %v, want it admitted once capacity freed, unaffected by the earlier cancellation", err)
			}
		case <-time.After(admissionFailSafe):
			t.Fatalf("only received %d/%d healthy results within the fail-safe bound", i, healthy)
		}
	}
}

// I1 (ordinary failure): an admitted call that fails via the real parse
// path still releases exactly the one permit it took, and a caller queued
// behind it progresses once that permit frees.
func TestWakeAdmission_I1_OrdinaryFailureReleasesPermitAndUnblocksWaiter(t *testing.T) {
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 24)

	err1 := c.Wake(context.Background(), invalidWakeLink, 1)
	if err1 == nil {
		t.Fatal("caller #1: Wake() error = nil, want the deterministic parse failure")
	}
	if got := len(c.wakeSemaphore); got != 24 {
		t.Fatalf("occupied tokens after caller #1's ordinary failure = %d, want 24 (its permit released)", got)
	}

	fillWakeSemaphore(t, c, 1) // saturate again so caller #2 is genuinely queued
	admission := newGate()
	c.beforeWakeAdmission = admission.wait
	call2 := make(chan error, 1)
	go func() { call2 <- c.Wake(context.Background(), invalidWakeLink, 2) }()
	admission.waitArrived(t)
	releaseWakeSemaphore(t, c)
	admission.release()

	err2 := <-call2
	if isWakeSentinelError(err2) {
		t.Errorf("caller #2 (queued behind an ordinary parse failure): Wake() error = %v, want it admitted once the permit freed", err2)
	}
}

// I1 (admitted success): an admitted call injected to succeed releases its
// permit, and a caller queued behind it progresses afterward.
func TestWakeAdmission_I1_AdmittedSuccessReleasesPermitAndUnblocksWaiter(t *testing.T) {
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 24)

	activation := newGate()
	c.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		activation.wait()
		return nil
	}
	call1 := make(chan error, 1)
	go func() { call1 <- c.Wake(context.Background(), invalidWakeLink, 1) }()
	activation.waitArrived(t)
	if got := len(c.wakeSemaphore); got != 25 {
		t.Fatalf("occupied tokens while caller #1 is admitted and active = %d, want 25", got)
	}

	admission := newGate()
	c.beforeWakeAdmission = admission.wait
	c.wakeActivation = nil // caller #2 reaches the real parse path once admitted
	call2 := make(chan error, 1)
	go func() { call2 <- c.Wake(context.Background(), invalidWakeLink, 2) }()
	admission.waitArrived(t)

	activation.release()
	err1 := <-call1
	if err1 != nil {
		t.Fatalf("caller #1 (injected success): Wake() error = %v, want nil", err1)
	}
	if got := len(c.wakeSemaphore); got != 24 {
		t.Errorf("occupied tokens after caller #1's success = %d, want 24 (its permit released)", got)
	}

	admission.release()
	err2 := <-call2
	if isWakeSentinelError(err2) {
		t.Errorf("caller #2 (queued behind a successful caller): Wake() error = %v, want it admitted once the permit freed", err2)
	}
}

// I1 (admitted cancellation): a caller cancelled while already admitted and
// mid-activation still releases its permit, returning the context error,
// and a caller queued behind it progresses afterward.
func TestWakeAdmission_I1_AdmittedCancellationReleasesPermitAndUnblocksWaiter(t *testing.T) {
	c := NewNativeClient()
	fillWakeSemaphore(t, c, 24)

	activation := newGate()
	c.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		activation.wait()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	call1 := make(chan error, 1)
	go func() { call1 <- c.Wake(ctx, invalidWakeLink, 1) }()
	activation.waitArrived(t)
	if got := len(c.wakeSemaphore); got != 25 {
		t.Fatalf("occupied tokens while caller #1 is admitted and active = %d, want 25", got)
	}
	cancel() // cancel while admitted, mid-activation

	admission := newGate()
	c.beforeWakeAdmission = admission.wait
	c.wakeActivation = nil
	call2 := make(chan error, 1)
	go func() { call2 <- c.Wake(context.Background(), invalidWakeLink, 2) }()
	admission.waitArrived(t)

	activation.release()
	err1 := <-call1
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("caller #1 (admitted then cancelled mid-activation): Wake() error = %v, want context.Canceled", err1)
	}
	if got := len(c.wakeSemaphore); got != 24 {
		t.Errorf("occupied tokens after caller #1's admitted cancellation = %d, want 24 (its permit released)", got)
	}

	admission.release()
	err2 := <-call2
	if isWakeSentinelError(err2) {
		t.Errorf("caller #2 (queued behind an admitted-then-cancelled caller): Wake() error = %v, want it admitted once the permit freed", err2)
	}
}

// errWakeAdmissionTimeoutShaped stands in for the real 45s metadata-timeout
// error without waiting 45 real seconds or needing a live torrent.
var errWakeAdmissionTimeoutShaped = errors.New("torrent metadata timeout (45s): deadbeef")

// I1 (timeout-shaped error) / E3: an admitted call injected to fail the way
// the real metadata timeout does still releases its permit, unblocks a
// queued waiter, and -- E3 -- the underlying error reaching the caller is
// bit-for-bit the same whether or not the call had to queue first.
func TestWakeAdmission_I1_TimeoutShapedErrorReleasesPermitAndUnblocksWaiter_E3(t *testing.T) {
	direct := NewNativeClient()
	direct.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		return errWakeAdmissionTimeoutShaped
	}
	directErr := direct.Wake(context.Background(), invalidWakeLink, 0)
	if !errors.Is(directErr, errWakeAdmissionTimeoutShaped) {
		t.Fatalf("direct (unqueued) call: Wake() error = %v, want %v", directErr, errWakeAdmissionTimeoutShaped)
	}

	c := NewNativeClient()
	fillWakeSemaphore(t, c, 24)

	activation := newGate()
	c.wakeActivation = func(ctx context.Context, magnetUrl string, fileIdx int) error {
		activation.wait()
		return errWakeAdmissionTimeoutShaped
	}
	call1 := make(chan error, 1)
	go func() { call1 <- c.Wake(context.Background(), invalidWakeLink, 1) }()
	activation.waitArrived(t)
	if got := len(c.wakeSemaphore); got != 25 {
		t.Fatalf("occupied tokens while caller #1 is admitted and active = %d, want 25", got)
	}

	admission := newGate()
	c.beforeWakeAdmission = admission.wait
	c.wakeActivation = nil
	call2 := make(chan error, 1)
	go func() { call2 <- c.Wake(context.Background(), invalidWakeLink, 2) }()
	admission.waitArrived(t)

	activation.release()
	err1 := <-call1
	if !errors.Is(err1, errWakeAdmissionTimeoutShaped) {
		t.Fatalf("caller #1 (injected timeout-shaped error): Wake() error = %v, want %v -- admission must not rewrite it", err1, errWakeAdmissionTimeoutShaped)
	}
	if err1.Error() != directErr.Error() {
		t.Errorf("queued call error = %q, want the identical underlying error %q as the unqueued call", err1.Error(), directErr.Error())
	}
	if got := len(c.wakeSemaphore); got != 24 {
		t.Errorf("occupied tokens after caller #1's timeout-shaped failure = %d, want 24 (its permit released)", got)
	}

	admission.release()
	err2 := <-call2
	if isWakeSentinelError(err2) {
		t.Errorf("caller #2 (queued behind a timed-out caller): Wake() error = %v, want it admitted once the permit freed", err2)
	}
}
