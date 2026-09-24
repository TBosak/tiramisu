package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"tiramisu/internal/gostorm/native"
)

// Tests for the shared-swarm verdict slice (AB4/AB9/AB11): overlapping sessions
// of one hash are one occasion whose verdict is governed by peer-backed success.
// Only reachability outcomes and session lifecycle are asserted.

type swarmOutcome struct {
	hash     string
	resolved bool
}

type swarmHarness struct {
	t *testing.T

	mu       sync.Mutex
	outcomes []swarmOutcome
	peers    map[string]int
	created  []string
}

// newSwarmHarness installs the reachability and peer-count seams and restores
// them, and clears every session it created, on cleanup.
func newSwarmHarness(t *testing.T) *swarmHarness {
	t.Helper()
	h := &swarmHarness{t: t, peers: map[string]int{}}

	prevOutcome := native.ReachabilityOutcome
	prevPeers := sessionActivePeers
	t.Cleanup(func() {
		native.ReachabilityOutcome = prevOutcome
		sessionActivePeers = prevPeers
	})
	native.ReachabilityOutcome = func(hash string, resolved bool) {
		h.mu.Lock()
		h.outcomes = append(h.outcomes, swarmOutcome{hash, resolved})
		h.mu.Unlock()
	}
	sessionActivePeers = func(hash string) int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.peers[hash]
	}
	t.Cleanup(func() {
		// Mute reporting so teardown cannot leak outcomes or touch other tests.
		native.ReachabilityOutcome = nil
		for _, p := range h.created {
			if v, ok := sessions.Load(p); ok {
				v.(*TTFFSession).closeSession()
			}
			sessions.Delete(p)
		}
	})
	return h
}

func (h *swarmHarness) setPeers(hash string, n int) {
	h.mu.Lock()
	h.peers[hash] = n
	h.mu.Unlock()
}

func (h *swarmHarness) count(hash string, resolved bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, o := range h.outcomes {
		if o.hash == hash && o.resolved == resolved {
			n++
		}
	}
	return n
}

// open registers a session on a unique path. age backdates openedAt.
func (h *swarmHarness) open(hash, name string, age time.Duration) (string, *TTFFSession) {
	h.t.Helper()
	path := fmt.Sprintf("/swarmverdict/%s/%s/%s", h.t.Name(), hash, name)
	h.created = append(h.created, path)
	ttffRegister(path, 100<<20, hash, false, false)
	v, ok := sessions.Load(path)
	if !ok {
		h.t.Fatalf("session for %s was not registered", path)
	}
	s := v.(*TTFFSession)
	s.openedAt.Store(time.Now().Add(-age).UnixNano())
	return path, s
}

var (
	oldEnough = 2 * sessionVerdictFloor
	tooYoung  = sessionVerdictFloor / 5
)

func TestTTFFSharedSwarmVerdict_SharedSuccessAcquitsOverlap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		netClosesAt  int // index in close order; siblings fill the rest
		siblingCount int
	}{
		{"failed siblings close before the network-backed session", 2, 2},
		{"network-backed session closes before failed siblings", 0, 2},
		{"network-backed session closes between failed siblings", 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSwarmHarness(t)
			const hash = "aaaa"
			h.setPeers(hash, 1)

			netPath, netS := h.open(hash, "net", oldEnough)
			var all []*TTFFSession
			var sibs []*TTFFSession
			for i := 0; i < tc.siblingCount; i++ {
				p, s := h.open(hash, fmt.Sprintf("sib%d", i), oldEnough)
				ttffRead(p, srcRACacheHit, 0, 4096, 0)
				ttffReadFailed(p)
				sibs = append(sibs, s)
			}
			ttffRead(netPath, srcFetchBlock, 0, 4096, 0)

			for i := 0; i <= tc.siblingCount; i++ {
				if i == tc.netClosesAt {
					all = append(all, netS)
				}
				if i < tc.siblingCount {
					all = append(all, sibs[i])
				}
			}
			for _, s := range all {
				s.closeSession()
				if n := h.count(hash, false); n != 0 {
					t.Fatalf("negative outcome emitted after %s closed despite peer-backed success: %d", s.path, n)
				}
			}
			if h.count(hash, false) != 0 {
				t.Fatalf("negative outcomes for shared-success occasion: %d", h.count(hash, false))
			}
			if h.count(hash, true) < 1 {
				t.Fatalf("shared-success occasion emitted no positive outcome")
			}
		})
	}
}

func TestTTFFSharedSwarmVerdict_SuccessOnLoneFailedSiblingsStillFromOneNetSession(t *testing.T) {
	h := newSwarmHarness(t)
	const hash = "bbbb"
	h.setPeers(hash, 3)
	// Many failed scanner handles, one network-backed reader.
	var sess []*TTFFSession
	for i := 0; i < 18; i++ {
		p, s := h.open(hash, fmt.Sprintf("f%d", i), oldEnough)
		ttffReadFailed(p)
		sess = append(sess, s)
	}
	p, s := h.open(hash, "net", oldEnough)
	ttffRead(p, srcFetchBlock, 0, 1, 0)
	sess = append(sess, s)
	for _, s := range sess {
		s.closeSession()
	}
	if n := h.count(hash, false); n != 0 {
		t.Fatalf("18 failed siblings of one healthy reader produced %d negatives", n)
	}
	if h.count(hash, true) < 1 {
		t.Fatalf("expected a positive outcome for the healthy occasion")
	}
}

func TestTTFFSharedSwarmVerdict_ConcurrentCloseOfSuccessCohortEmitsNoNegative(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		h := newSwarmHarness(t)
		hash := fmt.Sprintf("cccc%d", iter)
		h.setPeers(hash, 1)
		var sess []*TTFFSession
		for i := 0; i < 8; i++ {
			p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
			if i == 0 {
				ttffRead(p, srcFetchBlock, 0, 512, 0)
			} else {
				ttffReadFailed(p)
			}
			sess = append(sess, s)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range sess {
			wg.Add(1)
			go func(s *TTFFSession) {
				defer wg.Done()
				<-start
				s.closeSession()
			}(s)
		}
		close(start)
		wg.Wait()
		if n := h.count(hash, false); n != 0 {
			t.Fatalf("iteration %d: concurrent close emitted %d negatives for a peer-backed cohort", iter, n)
		}
	}
}

func TestTTFFSharedSwarmVerdict_AllFailedOccasionCountsOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"three overlapping failed sessions are one occasion", 3},
		{"thirty-two overlapping failed sessions are one occasion", 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSwarmHarness(t)
			const hash = "dddd"
			h.setPeers(hash, 0)
			var sess []*TTFFSession
			for i := 0; i < tc.n; i++ {
				p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
				ttffReadFailed(p)
				sess = append(sess, s)
			}
			for _, s := range sess {
				s.closeSession()
			}
			if got := h.count(hash, false); got != 1 {
				t.Fatalf("all-failed overlapping cohort of %d reported %d negatives, want exactly 1", tc.n, got)
			}
			if got := h.count(hash, true); got != 0 {
				t.Fatalf("all-failed cohort reported %d positives", got)
			}
		})
	}
}

func TestTTFFSharedSwarmVerdict_ConcurrentCloseOfAllFailedCohortCountsOnce(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		h := newSwarmHarness(t)
		hash := fmt.Sprintf("eeee%d", iter)
		var sess []*TTFFSession
		for i := 0; i < 8; i++ {
			p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
			ttffReadFailed(p)
			sess = append(sess, s)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range sess {
			wg.Add(1)
			go func(s *TTFFSession) {
				defer wg.Done()
				<-start
				s.closeSession()
			}(s)
		}
		close(start)
		wg.Wait()
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("iteration %d: concurrent close of failed cohort reported %d negatives, want 1", iter, got)
		}
	}
}

func TestTTFFSharedSwarmVerdict_ConcurrentReadsAndClosesRace(t *testing.T) {
	h := newSwarmHarness(t)
	const hash = "ffff"
	h.setPeers(hash, 2)
	var sess []*TTFFSession
	var paths []string
	for i := 0; i < 6; i++ {
		p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
		sess = append(sess, s)
		paths = append(paths, p)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, s := range sess {
		wg.Add(2)
		go func(i int, p string) {
			defer wg.Done()
			<-start
			if i == 0 {
				ttffRead(p, srcFetchBlock, 0, 256, 0)
			} else {
				ttffReadFailed(p)
			}
		}(i, paths[i])
		go func(s *TTFFSession) {
			defer wg.Done()
			<-start
			// Closing may precede or follow the read; only the race detector
			// and post-condition on a re-run below are asserted here.
			s.closeSession()
		}(s)
	}
	close(start)
	wg.Wait()
	for _, s := range sess {
		if !s.closed.Load() {
			t.Fatalf("session %s not closed", s.path)
		}
	}
}

func TestTTFFSharedSwarmVerdict_LaterOccasionsRemainObservable(t *testing.T) {
	t.Run("later all-failed session after an all-failed occasion reports again", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "1111"
		var first []*TTFFSession
		for i := 0; i < 3; i++ {
			p, s := h.open(hash, fmt.Sprintf("a%d", i), oldEnough)
			ttffReadFailed(p)
			first = append(first, s)
		}
		for _, s := range first {
			s.closeSession()
		}
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("first occasion: %d negatives, want 1", got)
		}
		p, s := h.open(hash, "later", oldEnough)
		ttffReadFailed(p)
		s.closeSession()
		if got := h.count(hash, false); got != 2 {
			t.Fatalf("later occasion: %d negatives total, want 2", got)
		}
	})

	t.Run("later all-failed cohort after a shared-success occasion is condemned", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "2222"
		h.setPeers(hash, 1)
		np, ns := h.open(hash, "net", oldEnough)
		fp, fs := h.open(hash, "fail", oldEnough)
		ttffRead(np, srcFetchBlock, 0, 64, 0)
		ttffReadFailed(fp)
		fs.closeSession()
		ns.closeSession()
		if got := h.count(hash, false); got != 0 {
			t.Fatalf("success occasion emitted %d negatives", got)
		}

		h.setPeers(hash, 0)
		var later []*TTFFSession
		for i := 0; i < 3; i++ {
			p, s := h.open(hash, fmt.Sprintf("later%d", i), oldEnough)
			ttffReadFailed(p)
			later = append(later, s)
		}
		for _, s := range later {
			s.closeSession()
		}
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("later all-failed occasion: %d negatives, want exactly 1 (success must not persist)", got)
		}
	})

	t.Run("later cohort after a failed occasion is not acquitted by earlier state", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "3333"
		p1, s1 := h.open(hash, "one", oldEnough)
		ttffReadFailed(p1)
		s1.closeSession()
		for round := 2; round <= 4; round++ {
			p, s := h.open(hash, fmt.Sprintf("r%d", round), oldEnough)
			ttffReadFailed(p)
			s.closeSession()
			if got := h.count(hash, false); got != round {
				t.Fatalf("round %d: %d negatives, want %d", round, got, round)
			}
		}
	})
}

func TestTTFFSharedSwarmVerdict_HashesAreIsolated(t *testing.T) {
	t.Run("success on A does not suppress overlapping single failed session on B", func(t *testing.T) {
		h := newSwarmHarness(t)
		const a, b = "aaa1", "bbb1"
		h.setPeers(a, 2)
		h.setPeers(b, 0)
		ap, as := h.open(a, "net", oldEnough)
		bp, bs := h.open(b, "fail", oldEnough)
		ttffRead(ap, srcFetchBlock, 0, 128, 0)
		ttffReadFailed(bp)
		as.closeSession()
		bs.closeSession()
		if got := h.count(b, false); got != 1 {
			t.Fatalf("hash B negatives = %d, want 1", got)
		}
		if got := h.count(a, false); got != 0 {
			t.Fatalf("hash A negatives = %d, want 0", got)
		}
	})

	t.Run("interleaved cohorts on two hashes are judged independently", func(t *testing.T) {
		h := newSwarmHarness(t)
		const a, b = "aaa2", "bbb2"
		h.setPeers(a, 1)
		h.setPeers(b, 0)
		var aS, bS []*TTFFSession
		for i := 0; i < 3; i++ {
			ap, as := h.open(a, fmt.Sprintf("a%d", i), oldEnough)
			bp, bs := h.open(b, fmt.Sprintf("b%d", i), oldEnough)
			if i == 1 {
				ttffRead(ap, srcFetchBlock, 0, 32, 0)
			} else {
				ttffReadFailed(ap)
			}
			ttffReadFailed(bp)
			aS = append(aS, as)
			bS = append(bS, bs)
		}
		for i := 0; i < 3; i++ {
			bS[i].closeSession()
			aS[i].closeSession()
		}
		if got := h.count(a, false); got != 0 {
			t.Fatalf("hash A negatives = %d, want 0", got)
		}
		if got := h.count(b, false); got != 1 {
			t.Fatalf("hash B negatives = %d, want exactly 1", got)
		}
	})
}

func TestTTFFSharedSwarmVerdict_OnlyPeerBackedFetchAcquits(t *testing.T) {
	t.Run("warmup and read-ahead bytes alone do not acquit", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "4444"
		h.setPeers(hash, 2) // peers exist, but no fetch-block read happened
		var sess []*TTFFSession
		for i, src := range []ttffSource{srcWarmupHead, srcWarmupTail, srcRACacheHit} {
			p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
			ttffRead(p, src, 0, 4096, 0)
			ttffReadFailed(p)
			sess = append(sess, s)
		}
		for _, s := range sess {
			s.closeSession()
		}
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("negatives = %d, want exactly 1", got)
		}
		if got := h.count(hash, true); got != 0 {
			t.Fatalf("cache/warmup bytes acquitted the cohort (%d positives)", got)
		}
	})

	t.Run("fetch-block bytes with zero active peers do not acquit", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "5555"
		h.setPeers(hash, 0)
		var sess []*TTFFSession
		for i := 0; i < 3; i++ {
			p, s := h.open(hash, fmt.Sprintf("s%d", i), oldEnough)
			if i == 0 {
				ttffRead(p, srcFetchBlock, 0, 38, 0) // residue, nobody to have answered
			}
			ttffReadFailed(p)
			sess = append(sess, s)
		}
		for _, s := range sess {
			s.closeSession()
		}
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("negatives = %d, want exactly 1", got)
		}
		if got := h.count(hash, true); got != 0 {
			t.Fatalf("peerless fetch bytes acquitted the cohort (%d positives)", got)
		}
	})

	t.Run("zero-length fetch-block read does not acquit", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "6666"
		h.setPeers(hash, 2)
		p, s := h.open(hash, "s", oldEnough)
		ttffRead(p, srcFetchBlock, 0, 0, 0)
		ttffReadFailed(p)
		s.closeSession()
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("negatives = %d, want 1", got)
		}
		if got := h.count(hash, true); got != 0 {
			t.Fatalf("zero-length read acquitted (%d positives)", got)
		}
	})
}

func TestTTFFSharedSwarmVerdict_NonVerdictGuardsRemain(t *testing.T) {
	t.Run("session without a terminal read failure emits no negative", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "7777"
		_, s := h.open(hash, "clean", oldEnough)
		s.closeSession()
		if got := h.count(hash, false); got != 0 {
			t.Fatalf("negatives = %d, want 0", got)
		}
	})

	t.Run("failed session younger than the verdict floor emits no negative", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "8888"
		p, s := h.open(hash, "young", tooYoung)
		ttffReadFailed(p)
		s.closeSession()
		if got := h.count(hash, false); got != 0 {
			t.Fatalf("negatives = %d, want 0", got)
		}
	})

	t.Run("young failed siblings do not condemn an overlapping healthy-less cohort of clean sessions", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "9999"
		var sess []*TTFFSession
		for i := 0; i < 3; i++ {
			p, s := h.open(hash, fmt.Sprintf("y%d", i), tooYoung)
			ttffReadFailed(p)
			sess = append(sess, s)
		}
		_, clean := h.open(hash, "clean", oldEnough)
		sess = append(sess, clean)
		for _, s := range sess {
			s.closeSession()
		}
		if got := h.count(hash, false); got != 0 {
			t.Fatalf("negatives = %d, want 0", got)
		}
	})

	t.Run("ineligible sessions do not consume the later occasion's verdict", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "0000"
		p, s := h.open(hash, "young", tooYoung)
		ttffReadFailed(p)
		s.closeSession()
		p2, s2 := h.open(hash, "old", oldEnough)
		ttffReadFailed(p2)
		s2.closeSession()
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("negatives = %d, want 1", got)
		}
	})
}

func TestTTFFSharedSwarmVerdict_SingleSessionContractPreserved(t *testing.T) {
	t.Run("peer-backed single session acquits", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "s001"
		h.setPeers(hash, 1)
		p, s := h.open(hash, "one", oldEnough)
		ttffRead(p, srcFetchBlock, 0, 10, 0)
		s.closeSession()
		if h.count(hash, true) < 1 || h.count(hash, false) != 0 {
			t.Fatalf("outcomes true=%d false=%d, want positive and no negative", h.count(hash, true), h.count(hash, false))
		}
	})

	t.Run("eligible failed single session condemns once", func(t *testing.T) {
		h := newSwarmHarness(t)
		const hash = "s002"
		p, s := h.open(hash, "one", oldEnough)
		ttffReadFailed(p)
		s.closeSession()
		s.closeSession() // idempotent
		if got := h.count(hash, false); got != 1 {
			t.Fatalf("negatives = %d, want 1", got)
		}
	})
}
