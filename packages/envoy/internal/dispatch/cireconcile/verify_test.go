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
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/webhook"
)

// endingRecorder is the listener's CI store as its webhook handler takes it, with the writes of
// one named check made to fail inside the store itself: each is handed to the store under a
// context that has already ended, so the store's write gives up, as it does while the bus does
// not answer, and the handler answers GitHub 503.
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
// fail (endingRecorder), and mountWebhook mounts it over the store itself again.
func (h *harness) failWebhookWritesOf(check string) {
	h.t.Helper()
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, endingRecorder{Store: h.listener, check: check})
}

func (h *harness) mountWebhook() {
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, h.listener)
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

// runAt is one check run of pull's head.
func runAt(pull fakePull, id int64, name, status, conclusion, observedAt string) object {
	return atHead(gitHubRepository, pull.sha, checkRun(id, name, status, conclusion, otherApp, observedAt))
}

// requests is every verify request the bucket holds, as the verify loop lists them.
func (h *harness) requests() []contracts.CIVerifyRequest {
	h.t.Helper()
	writer, err := h.reconciler.Open()
	if err != nil {
		h.t.Fatalf("open a writer: %v", err)
	}
	requests, err := writer.VerifyRequests(h.ctx)
	if err != nil {
		h.t.Fatalf("list the verify requests: %v", err)
	}
	return requests
}

// waitForRequest polls the bucket until it holds a verify request of the harness's head past
// after, the revision of an earlier one (zero for any), and answers it.
func (h *harness) waitForRequest(after uint64) contracts.CIVerifyRequest {
	h.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, request := range h.requests() {
			if request.Head.SHA == headSHA && request.Revision > after {
				return request
			}
		}
	}
	h.t.Fatalf("the listener wrote no verify request of the head past revision %d", after)
	return contracts.CIVerifyRequest{}
}

// waitForState polls the head's record until done holds of it.
func (h *harness) waitForState(why string, done func(cistore.State) bool) cistore.State {
	h.t.Helper()
	var state cistore.State
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var ok bool
		if state, _, ok = h.record(); ok && done(state) {
			return state
		}
	}
	h.t.Fatalf("%s: the record is %+v", why, state)
	return state
}

// The listener could not write the creation of a queued run (it answered GitHub 503), and then
// wrote the completion of another check: the record holds one finished check and nothing else,
// which settled before verify. With verify on, the listener claims the ready head and asks; it
// publishes nothing until a verify read that began after the request, and that read finds the
// queued run in GitHub's answer, writes it, and the head stays open. Once GitHub finishes the run
// and its webhook lands, the head settles after a verify read of its own.
func TestARefusedCreationWebhookIsFoundByTheVerifyReadAndTheHeadStaysOpen(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	settlements := h.subscribe()

	queued := unfinished(checkRun(2, "test", "queued", "", otherApp, "2026-11-22T00:02:06Z"))
	h.failWebhookWritesOf("test")
	if code, answer := h.serve("check_run", webhookBody("check_run", queued), "d1"); code != http.StatusServiceUnavailable {
		t.Fatalf("the webhook whose write failed answered %d (%s), want 503", code, answer)
	}
	lint := checkRun(3, "lint", "completed", "success", otherApp, "2026-11-22T00:02:30Z")
	h.post("check_run", webhookBody("check_run", lint), "d2")

	request := h.waitForRequest(0)
	h.noSettlement(settlements, "before any verify read")

	h.github.setCheckRuns(gitHubRepository, headSHA, queued, lint)
	h.verifyPass()

	h.noSettlement(settlements, "after the verify read that found the queued run")
	state := h.waitForState("the verify read was not answered", func(st cistore.State) bool { return st.VerifiedRev >= request.Revision })
	if got := state.Checks["test"]; got.CheckRunID != 2 || got.Status != "queued" {
		t.Fatalf("the record holds test as %+v, want run 2 queued", got)
	}

	done := checkRun(2, "test", "completed", "success", otherApp, "2026-11-22T00:09:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, done, lint)
	h.mountWebhook()
	h.post("check_run", webhookBody("check_run", done), "d3")
	h.waitForRequest(request.Revision)
	h.noSettlement(settlements, "before the verify read of the finished head")
	h.verifyPass()

	settled := h.settlement(settlements, "after the verify read of the finished head")
	if settled.Passed.Count != 2 || settled.Failed.Count != 0 {
		t.Fatalf("settlement = %+v, want test and lint passed", settled)
	}
}

// A verify read that fails, or that reads the check runs and then fails on the check suites,
// answers nothing: the record's VerifiedRev stays as it was, the request stays for a later read,
// and the head is not published.
func TestAVerifyReadThatFailsOrStopsPartWayWritesNoAnswer(t *testing.T) {
	for _, row := range []struct {
		name, path string
	}{
		{name: "the check runs listing fails", path: checkRunsPath},
		{name: "the check suites listing fails after the runs were read", path: checkSuitesPath},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newVerifyHarness(t, reviewerAppID)
			h.mapRepository(rowRepository)
			h.listHead()
			settlements := h.subscribe()
			run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
			h.github.setCheckRuns(gitHubRepository, headSHA, run)
			h.post("check_run", webhookBody("check_run", run), "d1")
			request := h.waitForRequest(0)

			h.github.answerStatus(row.path, http.StatusNotFound)
			before := h.github.count()
			h.verifyPass()
			// The read reached the listing that failed, and every listing before it answered: the
			// answer was cut there, not before GitHub was asked.
			if asked := h.github.sincePath(before, row.path); len(asked) == 0 {
				t.Fatalf("the verify pass never asked GitHub for %s", row.path)
			}

			h.noSettlement(settlements, "after a verify read that did not complete")
			if state, _, _ := h.record(); state.VerifiedRev != 0 {
				t.Fatalf("the record's VerifiedRev is %d after a read that did not complete, want 0", state.VerifiedRev)
			}
			if !slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
				t.Fatalf("the request at revision %d is gone after a read that did not complete", request.Revision)
			}
		})
	}
}

// A verify read GitHub answers complete and empty -- no check run, no check suite -- is an answer
// all the same: it writes the request's revision as VerifiedRev, the head settles on what the
// record holds, and the request is deleted.
func TestAnEmptyCompleteVerifyReadAnswersTheRequest(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)

	h.verifyPass()

	settled := h.settlement(settlements, "after an empty, complete verify read")
	if settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if state, _, _ := h.record(); state.VerifiedRev != request.Revision {
		t.Fatalf("the record's VerifiedRev is %d, want the request's %d", state.VerifiedRev, request.Revision)
	}
	if left := h.requests(); len(left) != 0 {
		t.Fatalf("the answered request is still held: %+v", left)
	}
}

// With verify off, a head settles from its webhooks alone, as it did before verify, and the
// listener writes no verify request.
func TestWithVerifyOffAHeadSettlesFromItsWebhooksAndNoRequestIsWritten(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.post("check_run", webhookBody("check_run", run), "d1")

	if settled := h.settlement(settlements, "with verify off"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("the listener wrote verify requests with verify off: %+v", requests)
	}
}

// The time from a head being ready to settle to its settlement, with the verify loop running as
// Dispatch runs it (every VerifyInterval) and the listener's summary loop at the harness's
// debounce: one head after another, each measured from its last webhook plus the debounce.
func TestAReadyHeadIsPublishedWithinOneVerifyIntervalAndThreeSecondsOfBeingReady(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	pulls := []fakePull{head(21), head(22), head(23), head(24), head(25)}
	h.listHeads(pulls...)
	loop, stop := context.WithCancel(h.ctx)
	defer stop()
	go duty.RunWoken(loop, duty.WokenLoop{MinGap: VerifyInterval, Pass: h.reconciler.Verify})

	const debounce = 100 * time.Millisecond
	var worst, total time.Duration
	for i, pull := range pulls {
		// Each head goes ready a second later in the loop's period than the one before, so the
		// five heads meet the loop's next list at five points of its five seconds.
		time.Sleep(time.Duration(i) * time.Second)
		settlements := h.subscribeTo(pull.number)
		run := runAt(pull, int64(100+i), "ci-ok", "completed", "success", "2026-11-22T00:05:00Z")
		h.github.setCheckRuns(gitHubRepository, pull.sha, run)
		h.post("check_run", webhookFor("check_run", run, pull.number), fmt.Sprintf("d%d", i))
		ready := time.Now().Add(debounce)
		if settled := h.settlement(settlements, fmt.Sprintf("for pull request %d", pull.number)); settled.Passed.Count != 1 {
			t.Fatalf("settlement of pull request %d = %+v, want ci-ok passed", pull.number, settled)
		}
		took := time.Since(ready)
		t.Logf("pull request %d: published %s after it was ready to settle", pull.number, took.Round(time.Millisecond))
		worst, total = max(worst, took), total+took
	}
	t.Logf("ready to published over %d heads: worst %s, mean %s", len(pulls), worst.Round(time.Millisecond), (total / time.Duration(len(pulls))).Round(time.Millisecond))
	if bound := VerifyInterval + 3*time.Second; worst > bound {
		t.Fatalf("the worst head took %s from ready to published, past %s", worst, bound)
	}
}

// A verify read whose write of what GitHub answered fails writes no answer: the record holds no
// verified_rev, the request stays, and nothing is published. A later answer -- here another
// Dispatch process, whose writes land -- publishes.
func TestAFailedWriteOfTheReadLeavesNoAnswerUntilALaterOneLands(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)
	failing := h.failWrites(1)

	h.verifyPass()

	if failing.records != 0 {
		t.Fatalf("the verify read made no write of its runs that the bus refused (%d refusals left)", failing.records)
	}
	h.noSettlement(settlements, "after a verify read whose write failed")
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d after the write of the read failed, want 0", state.VerifiedRev)
	}
	if !slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
		t.Fatalf("the request at revision %d is gone after the write of the read failed", request.Revision)
	}

	h.replica().Verify(h.ctx)
	if settled := h.settlement(settlements, "after a later answer landed"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
}
