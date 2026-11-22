package cireconcile

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/webhook"
)

// endingRecorder is the listener's CI store as its webhook handler takes it, with the writes of
// one named check made to fail inside the store itself: each is handed to the store under a
// context that has already ended, so the store's write gives up, as it does while the bus does
// not answer, and the store sees its own failure (cistore's intake gaps).
type endingRecorder struct {
	*cistore.Store
	check string
}

func (r endingRecorder) Record(ctx context.Context, observation contracts.CIObservation) error {
	if observation.CheckName == r.check {
		ended, cancel := context.WithCancel(ctx)
		cancel()
		ctx = ended
	}
	return r.Store.Record(ctx, observation)
}

// failWebhookWritesOf mounts the listener's webhook handler over a store whose writes of check
// fail (endingRecorder).
func (h *harness) failWebhookWritesOf(check string) {
	h.t.Helper()
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, endingRecorder{Store: h.listener, check: check})
}

// subscribeTo listens for the settlements of pull request number of the harness's repository.
func (h *harness) subscribeTo(number int64) *nats.Subscription {
	h.t.Helper()
	subject := fmt.Sprintf("notifications.github.%s.%s.pr.%d.checks", gitHubOwner, gitHubName, number)
	sub, err := h.bus.Conn.SubscribeSync(subject)
	if err != nil {
		h.t.Fatalf("subscribe to %s: %v", subject, err)
	}
	h.t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// waitForRecord polls pull's record until done holds of it.
func (h *harness) waitForRecord(pull fakePull, why string, done func(cistore.State) bool) cistore.State {
	h.t.Helper()
	var state *cistore.State
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if state = h.recordOf(pull, pull.number); state != nil && done(*state) {
			return *state
		}
	}
	h.t.Fatalf("%s: the record is %+v", why, state)
	return cistore.State{}
}

// waitForGapHeld polls the bucket's list of heads held by a gap alone until it names pull.
func (h *harness) waitForGapHeld(pull fakePull) {
	h.t.Helper()
	writer, err := openWriter(h.bus)()
	if err != nil {
		h.t.Fatalf("open a writer: %v", err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		heads, err := writer.GapHeldHeads()
		if err == nil && slices.ContainsFunc(heads, func(head cistore.Head) bool { return head.SHA == pull.sha }) {
			return
		}
	}
	h.t.Fatalf("the bucket's gap-held list never named pull request %d", pull.number)
}

// runAt is one completed run of pull's head.
func runAt(pull fakePull, id int64, name, status, conclusion, observedAt string) object {
	return atHead(gitHubRepository, pull.sha, checkRun(id, name, status, conclusion, otherApp, observedAt))
}

// startWithReconcile is the harness's listener starting at startedAt over a bucket a reconcile
// serves, whose last cycle began at cycleBegan: the gaps hold only where a reconcile has completed
// a cycle (cistore's read cycle). The listener reads that cycle at its start, before anything
// settles.
func (h *harness) startWithReconcile(cycleBegan, startedAt time.Time) {
	h.t.Helper()
	writer, err := openWriter(h.bus)()
	if err != nil {
		h.t.Fatalf("open a writer: %v", err)
	}
	if err := writer.MarkReadCycle(h.ctx, cycleBegan); err != nil {
		h.t.Fatalf("write the read cycle: %v", err)
	}
	h.listener.MarkIntakeGap(startedAt)
}

// A head whose webhook the listener could not write waits for a reconcile read before it settles.
// The head settled on its first run. Then the listener could not write the creation of a queued
// run (it answered GitHub 503), and wrote the completion of another run that came after it: the
// record holds two finished checks and nothing else, which settled before this change. The store
// marks the head at the failure, so it holds it until a reconcile pass reads it from GitHub after
// the failure; that pass writes the queued run, and the head settles once GitHub finishes it.
func TestAHeadWhoseWebhookWriteFailedWaitsForAReconcileRead(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	// The listener started an hour ago, and a reconcile cycle has completed since.
	h.startWithReconcile(time.Now(), time.Now().Add(-time.Hour))
	settlements := h.subscribe()
	changes := checkRun(1, "changes", "completed", "success", otherApp, "2026-11-22T00:02:05Z")
	h.post("check_run", webhookBody("check_run", changes), "d1")
	h.settlement(settlements, "on the first webhook")
	h.settledRecord()

	queued := unfinished(checkRun(2, "test", "queued", "", otherApp, "2026-11-22T00:02:06Z"))
	h.failWebhookWritesOf("test")
	if code, answer := h.serve("check_run", webhookBody("check_run", queued), "d2"); code != http.StatusServiceUnavailable {
		t.Fatalf("the webhook whose write failed answered %d (%s), want 503", code, answer)
	}
	lint := checkRun(3, "lint", "completed", "success", otherApp, "2026-11-22T00:02:30Z")
	h.post("check_run", webhookBody("check_run", lint), "d3")

	h.noSettlement(settlements, "after a webhook the listener could not write")
	pull := fakePull{number: 7, sha: headSHA}
	h.waitForRecord(pull, "the store never wrote the head's gap", func(st cistore.State) bool { return st.GapAt != 0 })

	h.github.setCheckRuns(gitHubRepository, headSHA, changes, queued, lint)
	h.pass()

	h.noSettlement(settlements, "while GitHub runs test")
	state := h.waitForRecord(pull, "the pass did not write the head", func(st cistore.State) bool { return st.ReadAt > st.GapAt })
	if got := state.Checks["test"]; got.CheckRunID != 2 || got.Status != "queued" {
		t.Fatalf("the record holds test as %+v, want run 2 queued", got)
	}

	h.github.setCheckRuns(gitHubRepository, headSHA, changes, checkRun(2, "test", "completed", "success", otherApp, "2026-11-22T00:09:00Z"), lint)
	h.pass()

	settled := h.settlement(settlements, "once GitHub finished test")
	if settled.Passed.Count != 3 || settled.SupersededSettlement != "true" {
		t.Fatalf("settlement = %+v, want changes, test and lint passed, superseding the first", settled)
	}
}

// After a listener start, a head that is ready waits for one pass, which reads it first, before
// its walk of the order, over REST; a head that is not ready is not read early. The start stamps
// the whole intake (MarkIntakeGap): deliveries made while the listener was down are lost, and no
// record names their heads. The summary loop names the ready head in the bucket's gap-held list,
// and the pass reads the heads that list names first. The harness's one pass also completes a
// cycle, which lifts the stamp, so the settlement alone does not show the early read: the REST
// read, the pass's gap_heads and the record's own ReadAt do.
func TestAStartStampHoldsAReadyHeadUntilOnePassReadsItFirst(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	ready := fakePull{number: 7, sha: headSHA}
	waiting := head(8)
	h.listHeads(ready, waiting)
	h.startWithReconcile(time.Now().Add(-time.Hour), time.Now())
	settlements := h.subscribe()

	done := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.post("check_run", webhookBody("check_run", done), "d1")
	building := unfinished(runAt(waiting, 2, "build", "queued", "", "2026-11-22T00:05:01Z"))
	h.post("check_run", webhookFor("check_run", building, waiting.number), "d2")
	h.noSettlement(settlements, "for a ready head after the listener started")
	h.waitForGapHeld(ready)

	h.github.setCheckRuns(gitHubRepository, ready.sha, done)
	h.github.setCheckRuns(gitHubRepository, waiting.sha, building)
	before := h.github.count()

	h.pass()

	settled := h.settlement(settlements, "after one pass")
	if settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if got := number(t, h.passLine(), "gap_heads"); got != 1 {
		t.Fatalf("the pass read %d heads a gap held first, want the ready one", got)
	}
	if read := h.github.sincePath(before, "/repos/"+gitHubRepository+"/commits/"+ready.sha+"/check-runs"); len(read) == 0 {
		t.Fatal("the pass did not read the ready head over REST: it was not read first")
	}
	if read := h.github.sincePath(before, "/repos/"+gitHubRepository+"/commits/"+waiting.sha+"/check-runs"); len(read) != 0 {
		t.Fatalf("the pass read the head that is not ready over REST %d times: it was read early", len(read))
	}
	if state := h.recordOf(ready, ready.number); state == nil || state.ReadAt == 0 {
		t.Fatalf("the ready head's record is %+v, want the read that freed it (ReadAt)", state)
	}
}

// A reconcile cycle that began after the start stamp lifts it: it has read every open pull
// request's head, so a head first seen after it began has check runs the downtime could not have
// lost. A head the stamp holds is held until then (or until a read of its own), a head first seen
// after the cycle settles at once with no read of its own, and a head's own gap still holds after
// the lift.
func TestAReadCycleThatBeganAfterTheStampLiftsIt(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	open := fakePull{number: 7, sha: headSHA}
	h.listHeads(open)
	h.github.setCheckRuns(gitHubRepository, open.sha, checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z"))
	h.startWithReconcile(time.Now().Add(-time.Hour), time.Now())
	// The cycle must begin after the stamp, which is a millisecond.
	time.Sleep(5 * time.Millisecond)

	held := head(9)
	heldRun := runAt(held, 90, "ci-ok", "completed", "success", "2026-11-22T00:05:10Z")
	h.github.setCheckRuns(gitHubRepository, held.sha, heldRun)
	heldSettlements := h.subscribeTo(held.number)
	h.post("check_run", webhookFor("check_run", heldRun, held.number), "d1")
	h.noSettlement(heldSettlements, "for a head the stamp holds before a cycle completed")

	h.pass()
	if line := h.passLine(); line["cycle_complete"] != true {
		t.Fatalf("the pass did not complete a cycle: %v", line)
	}
	if settled := h.settlement(heldSettlements, "once a cycle that began after the stamp completed"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}

	fresh := head(10)
	freshSettlements := h.subscribeTo(fresh.number)
	h.post("check_run", webhookFor("check_run", runAt(fresh, 100, "ci-ok", "completed", "success", "2026-11-22T00:06:00Z"), fresh.number), "d2")
	if settled := h.settlement(freshSettlements, "for a head first seen after the cycle"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if state := h.recordOf(fresh, fresh.number); state == nil || state.ReadAt != 0 {
		t.Fatalf("the fresh head's record is %+v, want it settled with no read of its own", state)
	}

	gapped := head(11)
	gappedSettlements := h.subscribeTo(gapped.number)
	h.failWebhookWritesOf("test")
	if code, answer := h.serve("check_run", webhookFor("check_run", unfinished(runAt(gapped, 110, "test", "queued", "", "2026-11-22T00:07:00Z")), gapped.number), "d3"); code != http.StatusServiceUnavailable {
		t.Fatalf("the webhook whose write failed answered %d (%s), want 503", code, answer)
	}
	h.post("check_run", webhookFor("check_run", runAt(gapped, 111, "lint", "completed", "success", "2026-11-22T00:07:10Z"), gapped.number), "d4")
	h.noSettlement(gappedSettlements, "for a head whose own webhook failed, after the lift")
	state := h.waitForRecord(gapped, "the store never wrote the head's gap", func(st cistore.State) bool { return st.GapAt != 0 })
	if state.SettledEmitted {
		t.Fatalf("the head whose own webhook failed settled after the lift: %+v", state)
	}
}
