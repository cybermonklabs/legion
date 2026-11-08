//go:build memory

package main

// INFRA-179: a caller's write is answered within a bound of the server's own - 200 or 503 - and
// never left unanswered until the caller's own deadline fires.
//
// The tester wrote this test (test handoff v9, 2026-11-08) against the design's sentence that a
// write waiting on memory "gets the request's deadline and is answered 503 at it". Measured at
// the head it was written on, e2b27fbc, that sentence named something the server did not have:
// instrumented at docs' liveWrite.coverFork, every write of a 320-write burst logged
// deadline=false, because HTTP carries no deadline, this API's http.Server leaves WriteTimeout
// zero and no handler wrapped a request in one. Of 320 writes, 18 were answered, 302 reached the
// 60 s deadline of the client every agent's `dispatch` command goes through with no answer at
// all, and the server answered 503 to none of them. That is what the tester's test caught.
//
// The architect ruled on 2026-11-08 what the contract is and how this test states it: "the
// contract is 'answered within the server's bound, never after the caller's deadline', 200 or
// 503. A write the pod serves inside its bound is correct; serving it late is the harm." The
// ruling came with the round's other change: the pod's copy heap became one weighed pool rather
// than k fixed slots (docs' copyWeight, loadSlots), so a probe behind a saturating burst is no
// longer certain to find the pool full - at this head it was served at 41.3 s, inside the bound -
// while a pool that genuinely has no room still refuses at the wait bound. The test holds both
// halves, in that order.
//
// The assertion below is the tester's own, dictated by the test's author on 2026-11-08 and
// authorised by the architect on the same day as an author-dictated edit the implementer applied:
// zero writes left unanswered, at least one answered 503, every answer within WriteRequestBound
// and a margin, and each write answered 200 in the store exactly once.
//
// What makes each half a reading rather than a hope: the first saturates with real writes and
// asks only what the bound promises, and the second fills the pool for real, with live writes
// parked after their fork at the production figures (docs' PauseAfterFirstAppend, whose write
// holds its grant while it is parked), so the probe's refusal is the pool's and not a timing
// accident.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// answerMargin is what the probe gets above the server's own bound before the test calls the
// answer late: the answer has to be written, read and parsed over the loopback while the pod is
// saturated, and the drive's own poll reads it every 200 ms.
const answerMargin = 5 * time.Second

func TestEveryWriteOfASaturatingBurstIsAnsweredWithinTheServersBound(t *testing.T) {
	const rooms = 16
	const writes = 320
	// Generous, and far above the server's own bound: a write that reaches it was answered by
	// nothing, which is the defect this test locks (the tester's shape, 2026-11-08).
	const clientDeadline = 2 * time.Minute
	const with = "Section 0 burst"
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	sourceID, _ := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	// One document per write, so that what each answered write left is its own document's to
	// read (the "exactly once" below), and a browser on the first rooms of them, so the burst
	// is a mix of warm forks and cold ones as a real burst is.
	copies := memory.projectDocumentCopies(t, sourceID, writes)
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)

	held := make([]*browser, 0, rooms)
	t.Cleanup(func() {
		for _, opened := range held {
			opened.leave()
		}
	})
	for _, room := range copies[:rooms] {
		held = append(held, server.openBrowser(t, room))
	}

	// What each document holds before the burst, so that what the applied writes added to it is
	// a reading and not a guess (the tester's "in the store exactly once").
	_, storedBefore := memory.rewrittenDocuments(t, copies, "## "+with)

	type answer struct {
		status  int
		err     error
		elapsed time.Duration
	}
	answers := make([]answer, writes)
	var running sync.WaitGroup
	began := time.Now()
	for index := range writes {
		running.Add(1)
		go func() {
			defer running.Done()
			ctx, cancel := context.WithTimeout(context.Background(), clientDeadline)
			defer cancel()
			start := time.Now()
			got, err := server.editWithin(ctx, copies[index], "Section 0", with)
			answers[index] = answer{status: got.status, err: err, elapsed: time.Since(start)}
		}()
	}
	running.Wait()
	whole := time.Since(began)

	var applied, refused, unanswered, otherwise int
	var slowest time.Duration
	otherwiseSample := ""
	for _, got := range answers {
		slowest = max(slowest, got.elapsed)
		switch {
		case got.err != nil:
			unanswered++
		case got.status == http.StatusOK:
			applied++
		case got.status == http.StatusServiceUnavailable:
			refused++
		default:
			otherwise++
			if otherwiseSample == "" {
				otherwiseSample = fmt.Sprintf("%d in %s", got.status, got.elapsed.Round(time.Millisecond))
			}
		}
	}
	answerWithin := docs.WriteRequestBound + answerMargin
	t.Logf("%d concurrent writes, one to each of %d documents of a mebibyte, %d of them held open by a browser, each under its caller's %s deadline, in %s: %d applied, %d refused 503, %d left unanswered, %d answered otherwise; the slowest answer took %s against the server's %s bound",
		writes, writes, rooms, clientDeadline, whole.Round(time.Second), applied, refused, unanswered, otherwise, slowest.Round(time.Millisecond), docs.WriteRequestBound)

	// (a) The lock: nothing is left for the caller's own deadline to end. A write that hangs
	// reaches the two-minute client deadline and is counted here.
	if unanswered != 0 {
		t.Errorf("%d of the %d writes were answered by nothing and ended at their caller's %s deadline, want every one answered by the server: a write the pod cannot serve is refused 503, never left to hang (INFRA-179)",
			unanswered, writes, clientDeadline)
	}
	// (b) The bound refuses under saturation rather than serving everything slowly.
	if refused == 0 {
		t.Errorf("none of the %d writes was refused 503 while %d of them ran at once over documents of a mebibyte, want the server's bounds to refuse what it cannot serve in time: a burst that is served whole says the pod was never saturated and this drive proves nothing",
			writes, writes)
	}
	// (c) The whole-write bound holds: no answer, of either kind, comes late.
	if slowest > answerWithin {
		t.Errorf("the slowest write was answered after %s, want every answer within %s (the server's %s bound and a %s margin for the answer's own trip)",
			slowest.Round(time.Millisecond), answerWithin, docs.WriteRequestBound, answerMargin)
	}
	if otherwise != 0 {
		t.Errorf("%d writes were answered neither 200 nor 503 (%s), want a write applied or refused and nothing else", otherwise, otherwiseSample)
	}

	// An applied write is in the store, exactly once: the bound refuses a write that has not
	// committed and never one that has, so each 200 is one durable edit and not none of them and
	// not two. A compaction folds a document's whole history into one row while this runs
	// (PgVersioned's compaction), which is the one thing that leaves an applied write's update
	// anywhere but in a row of its own, so it is counted and named rather than asserted away.
	rewritten, storedAfter := memory.rewrittenDocuments(t, copies, "## "+with)
	added, folded, holding := 0, 0, 0
	for _, room := range copies {
		switch grew := storedAfter[room] - storedBefore[room]; {
		case grew < 0:
			folded++
		default:
			added += grew
		}
		if rewritten[room] {
			holding++
		}
	}
	t.Logf("the %d applied writes left %d new stored updates across the %d documents, %d of which hold the text they wrote; a compaction folded %d of them",
		applied, added, writes, holding, folded)
	if holding != applied && folded == 0 {
		t.Errorf("%d writes were answered 200 and %d documents hold the text they wrote, want one document each: a write answered 200 is in the store, and a write answered 503 left its document as it was", applied, holding)
	}
	if folded == 0 && added != applied {
		t.Errorf("the %d writes answered 200 left %d new stored updates, want one each: a write answered 200 is in the store exactly once", applied, added)
	}
}

func TestAWriteThatCannotGetRoomInTheCopyPoolIsRefusedAtTheWaitBound(t *testing.T) {
	const probeDeadline = 2 * time.Minute
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, podServerEnv()...)
	sourceID, _ := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	// Exactly as many parked forks as the calibration's pool has room for, at the production
	// figures: a fork of a mebibyte of prose is charged its state at docs' bytesOfHeapAStateByte,
	// 42,493,360 bytes, and fourteen of those are 594,907,040 of the pool's 600,000,000 - a
	// fifteenth needs 637 MB and cannot be admitted at all. Each parked write holds its grant
	// until it is released (docs' PauseAfterFirstAppend parks a write that has forked), so the
	// pool is held full by real writes rather than by a shrunken figure, and the probe below
	// cannot have room however long it waits. A filler that does not park inside thirty seconds
	// fails the test rather than being passed over: fourteen fit by the arithmetic above, so one
	// that does not park is a reading this test is built to catch.
	const fillers = 14
	copies := memory.projectDocumentCopies(t, sourceID, fillers+1)
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	probeRoom := copies[fillers]

	parked := make([]string, 0, fillers)
	t.Cleanup(func() {
		for _, room := range parked {
			server.releaseLiveWritePause(t, room, docs.PauseAfterFirstAppend)
		}
	})
	for _, room := range copies[:fillers] {
		server.armLiveWritePause(t, room, docs.PauseAfterFirstAppend)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
			defer cancel()
			server.editWithin(ctx, room, "Section 0", "Section 0 filler")
		}()
		server.awaitLiveWriteParked(t, room, docs.PauseAfterFirstAppend)
		parked = append(parked, room)
	}
	t.Logf("%d live writes are parked holding their forks' grants, which is all the room the pod's copy pool has", len(parked))

	// The probe cannot have room: every grant the pool has is held by a parked write that no
	// answer of this probe can release. It must be refused at the wait bound, not at the request
	// bound and not at its caller's deadline.
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
	defer cancel()
	answer, err := server.editWithin(ctx, probeRoom, "Section 0", "Section 0 probe")
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("the probe write returned a client error after %s, want 503 from the server: %v", elapsed.Round(time.Millisecond), err)
	}
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("the probe write was answered %d after %s with the copy pool held full, want 503: a write that cannot get room in the pool is refused, not served",
			answer.status, elapsed.Round(time.Millisecond))
	}
	if elapsed > docs.WriteWaitBound+answerMargin {
		t.Errorf("the probe write was refused after %s, want it refused within the server's %s wait bound: a write the pod has no room for is told so with its caller's patience left to retry with",
			elapsed.Round(time.Millisecond), docs.WriteWaitBound)
	}
	t.Logf("the probe was refused 503 after %s, within the %s wait bound", elapsed.Round(time.Millisecond), docs.WriteWaitBound)
}

// liveWritePausePath is the test hook route of the pause of the document's live writes at point.
func liveWritePausePath(artifactID string, point docs.LiveWritePausePoint) string {
	return "/api/v1/artifacts/" + artifactID + "/_test/live-write-pauses/" + string(point)
}

// armLiveWritePause makes the next live write of the document park at point until it is released.
func (p *memoryServer) armLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	if answer := p.send(t, http.MethodPut, liveWritePausePath(artifactID, point), "application/json", nil, http.Header{"X-Dispatch-User": {"alice"}}); answer.status != http.StatusNoContent {
		t.Fatalf("arm the live write pause of %s at %s: %d %s", artifactID, point, answer.status, answer.body)
	}
}

// awaitLiveWriteParked waits for a write of the document to park at the pause armed at point.
func (p *memoryServer) awaitLiveWriteParked(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	answer := p.send(t, http.MethodGet, liveWritePausePath(artifactID, point)+"?wait=30s", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	if answer.status != http.StatusOK {
		t.Fatalf("await the live write parked at %s of %s: %d %s", point, artifactID, answer.status, answer.body)
	}
	var state struct {
		Parked bool `json:"parked"`
	}
	if err := json.Unmarshal(answer.body, &state); err != nil {
		t.Fatalf("decode the live write pause of %s: %v: %s", artifactID, err, answer.body)
	}
	if !state.Parked {
		t.Fatalf("no live write of %s parked at %s within thirty seconds", artifactID, point)
	}
}

// releaseLiveWritePause lets the write parked at point go on and disarms the pause.
func (p *memoryServer) releaseLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	answer, err := p.trySend(http.MethodDelete, liveWritePausePath(artifactID, point), "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		t.Logf("release the live write pause of %s at %s: %v", artifactID, point, err)
		return
	}
	if answer.status != http.StatusOK && answer.status != http.StatusNoContent {
		t.Logf("release the live write pause of %s at %s: %d %s", artifactID, point, answer.status, answer.body)
	}
}
