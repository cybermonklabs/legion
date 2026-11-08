//go:build memory

package main

// INFRA-179: a write that cannot get a load slot must be answered 503 DOC_SERVICE_UNAVAILABLE at a
// server bound shorter than the caller's deadline, not left unanswered until the caller's own
// deadline fires.
//
// The design promises that a write waiting on memory "gets the request's deadline and is answered
// 503 at it", and that a request with no deadline gets a server bound of its own. Measured at the
// pushed head e2b27fbc, neither holds under a burst: the edit handler waits for the load slot on
// the request context alone (api.writeLimiter.take and docs' loadSlots both Acquire(ctx)), with no
// server bound shorter than the caller's deadline. At the 4 GiB calibration (one load slot) a
// fork's slot wait is 44-57 s; a caller with the real 60 s deadline is answered by nothing and
// gives up, and the server answers 503 to no one. On main the same burst answered every write.
//
// This test pins the contract: while the one load slot is held by a saturating burst, a probe
// write with a generous deadline is answered 503 well before that deadline. It is RED at e2b27fbc
// (the probe is left unanswered, so it reports no server status within the window) and GREEN once
// the edit handler answers 503 at a server bound shorter than the caller's deadline.
//
// It is timing-based because no test hook holds a load slot; the margins are wide (a 2 m probe
// deadline against a 45 s window, a slot whose measured wait is 44-57 s) so that a pass means the
// server answered 503 and not that the burst happened to drain.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestAWriteThatCannotGetALoadSlotIsAnswered503BeforeItsDeadline(t *testing.T) {
	const rooms = 16
	const probeDeadline = 2 * time.Minute
	const answerWithin = 45 * time.Second
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	sourceID, _ := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	copies := memory.projectDocumentCopies(t, sourceID, rooms)
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	held := make([]*browser, 0, rooms)
	t.Cleanup(func() {
		for _, opened := range held {
			opened.leave()
		}
	})
	for _, room := range copies {
		held = append(held, server.openBrowser(t, room))
	}

	// A background burst that keeps the pod's load concurrency saturated for the whole probe
	// window: each writer re-issues as soon as its last write returns, so a slot never sits idle.
	stop := make(chan struct{})
	var saturators sync.WaitGroup
	for index := range rooms {
		room := copies[index]
		saturators.Add(1)
		go func() {
			defer saturators.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
				server.editWithin(ctx, room, "Section 0", "Section 0 saturate")
				cancel()
				n++
				_ = n
			}
		}()
	}
	t.Cleanup(func() {
		close(stop)
		saturators.Wait()
	})
	// Let the burst take the slot before the probe joins the queue.
	time.Sleep(5 * time.Second)

	// The probe: one write with a generous deadline, into a room of its own that the burst is also
	// writing, so it must take a load slot behind the burst. The contract is that the server
	// answers it 503 at a bound well short of its deadline.
	type result struct {
		status  int
		err     error
		elapsed time.Duration
	}
	var done atomic.Pointer[result]
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
		defer cancel()
		began := time.Now()
		answer, err := server.editWithin(ctx, copies[0], "Section 0", "Section 0 probe")
		done.Store(&result{status: answer.status, err: err, elapsed: time.Since(began)})
	}()

	deadline := time.Now().Add(answerWithin)
	for time.Now().Before(deadline) {
		if done.Load() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	got := done.Load()
	switch {
	case got == nil:
		t.Fatalf("the probe write was not answered within %s while the load slot was saturated: the server has no bound shorter than the caller's deadline, so a write that cannot get a slot is left unanswered until the caller gives up (INFRA-179)", answerWithin)
	case got.err != nil:
		t.Fatalf("the probe write returned a client error after %s, want a server 503 DOC_SERVICE_UNAVAILABLE: %v", got.elapsed.Round(time.Millisecond), got.err)
	case got.status != http.StatusServiceUnavailable:
		t.Fatalf("the probe write was answered %d after %s, want 503 (DOC_SERVICE_UNAVAILABLE or SERVICE_BUSY): a write that cannot get a load slot within the server's bound must be refused, not served late",
			got.status, got.elapsed.Round(time.Millisecond))
	}
	t.Logf("the probe was answered 503 after %s, within the %s bound and well short of its %s deadline", got.elapsed.Round(time.Millisecond), answerWithin, probeDeadline)
}