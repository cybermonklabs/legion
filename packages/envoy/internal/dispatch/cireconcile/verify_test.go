package cireconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
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
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, endingRecorder{Store: h.listener, check: check}, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
}

func (h *harness) mountWebhook() {
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, h.listener, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
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
	return h.waitForRequestOf(headSHA, after)
}

// waitForRequestOf is waitForRequest for the head at sha.
func (h *harness) waitForRequestOf(sha string, after uint64) contracts.CIVerifyRequest {
	h.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, request := range h.requests() {
			if request.Head.SHA == sha && request.Revision > after {
				return request
			}
		}
	}
	h.t.Fatalf("the listener wrote no verify request of the head at %s past revision %d", sha, after)
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
	h.seedProjectDefault(rowRepository)
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
			h.seedProjectDefault(rowRepository)
			h.listHead()
			settlements := h.subscribe()
			run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
			h.github.setCheckRuns(gitHubRepository, headSHA, run)
			h.post("check_run", webhookBody("check_run", run), "d1")
			request := h.waitForRequest(0)

			// GitHub's 502, which a later read may not meet: one failure, which ends no episode.
			h.github.answerStatus(row.path, http.StatusBadGateway)
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
	h.seedProjectDefault(rowRepository)
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
	h.seedProjectDefault(rowRepository)
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

// A ready head is published by the first verify pass that follows its request: the passes are
// driven here, one per head, so the verdict is the order of events and not the time the runner
// took. The time from the pass's start to the publication is logged for each head.
func TestAReadyHeadIsPublishedByTheFirstVerifyPassAfterItsRequest(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	pulls := []fakePull{head(21), head(22), head(23)}
	h.listHeads(pulls...)
	for i, pull := range pulls {
		settlements := h.subscribeTo(pull.number)
		run := runAt(pull, int64(100+i), "ci-ok", "completed", "success", "2026-11-22T00:05:00Z")
		h.github.setCheckRuns(gitHubRepository, pull.sha, run)
		h.post("check_run", webhookFor("check_run", run, pull.number), fmt.Sprintf("d%d", i))
		h.waitForRequestOf(pull.sha, 0)
		if i == 0 {
			h.noSettlement(settlements, "before any verify pass")
		}
		began := time.Now()
		h.verifyPass()
		if settled := h.settlement(settlements, fmt.Sprintf("after the verify pass for pull request %d", pull.number)); settled.Passed.Count != 1 {
			t.Fatalf("settlement of pull request %d = %+v, want ci-ok passed", pull.number, settled)
		}
		t.Logf("pull request %d: published %s after the verify pass began", pull.number, time.Since(began).Round(time.Millisecond))
	}
}

// A verify read whose write of what GitHub answered fails writes no answer: the record holds no
// verified_rev, the request stays, and nothing is published. A later answer -- here another
// Dispatch process, whose writes land -- publishes.
func TestAFailedWriteOfTheReadLeavesNoAnswerUntilALaterOneLands(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
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

// verifyGaveUp is the verify loop's ERROR line when it gives an episode up (permanentVerifyFailure).
const verifyGaveUp = "ci verify gave up an episode: no later read of the head mends its failure"

// A GitHub that fails every read of a head for longer than ten minutes, the point at which the
// verify loop once gave the episode up, does not end it: each failed answer is read again after a
// wait that doubles from verifyRetry up to verifyRetryMax, so the head is read 0, 1, 3, 7, 15 and
// 23 minutes after the first read (the pass's clock moves 30 seconds at each pass here, and the
// listener's request stays), with no ERROR and no answer; and the first read after GitHub answers
// again, at 31 minutes, writes the answer, and the head is published.
func TestAVerifyReadFailingPastTenMinutesBacksOffDoublingAndAnswersOnceGitHubDoes(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	start := time.Now()
	clock := start
	h.reconciler.now = func() time.Time { return clock }
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)
	h.github.answerStatus(checkRunsPath, http.StatusBadGateway)

	// reads is the clock's offset from start at each pass that read the head's check runs.
	var reads []time.Duration
	passesUntil := func(offset time.Duration) {
		for ; clock.Sub(start) <= offset; clock = clock.Add(30 * time.Second) {
			before := h.github.count()
			h.verifyPass()
			if len(h.github.sincePath(before, checkRunsPath)) != 0 {
				reads = append(reads, clock.Sub(start))
			}
		}
	}
	const minute = time.Minute
	passesUntil(25 * minute)
	if want := []time.Duration{0, minute, 3 * minute, 7 * minute, 15 * minute, 23 * minute}; !slices.Equal(reads, want) {
		t.Fatalf("with GitHub failing every read, the verify loop read the head at %v; want %v", reads, want)
	}
	if lines := h.lines(verifyGaveUp); len(lines) != 0 {
		t.Fatalf("the verify loop gave up %d times on failures a later read may mend", len(lines))
	}
	if !slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
		t.Fatalf("the request at revision %d is gone while GitHub failed every read", request.Revision)
	}
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d while GitHub failed every read, want 0", state.VerifiedRev)
	}
	h.noSettlement(settlements, "while GitHub failed every read")

	h.github.answerStatus(checkRunsPath, 0)
	reads = nil
	passesUntil(31 * minute)
	if want := []time.Duration{31 * minute}; !slices.Equal(reads, want) {
		t.Fatalf("once GitHub answered again, the verify loop read the head at %v; want %v", reads, want)
	}
	if state, _, _ := h.record(); state.VerifiedRev != request.Revision {
		t.Fatalf("the record's VerifiedRev is %d after GitHub answered again, want the request's %d", state.VerifiedRev, request.Revision)
	}
	if settled := h.settlement(settlements, "after GitHub answered again"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
}

// A failure no later read mends gives its episode up at once, and for good: here GitHub answers
// the head's check runs 404. The first read logs one ERROR naming the head and deletes the
// request; the episode's request, written again before every pass for 31 minutes, past each point
// where a failure a later read may mend is read again, and after GitHub answers again, is deleted
// unread each time. A new observation of the head starts a new episode, which is read afresh and
// publishes.
func TestAPermanentVerifyFailureGivesItsEpisodeUpAtOnceUntilANewEpisode(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	start := time.Now()
	clock := start
	h.reconciler.now = func() time.Time { return clock }
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)
	// The request's own value, as the listener writes it again at its next timeout.
	js, err := h.bus.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue(bus.CIStateBucket)
	if err != nil {
		t.Fatalf("bind the CI bucket: %v", err)
	}
	requestKey := "verify." + cistore.Key(request.Head.Owner, request.Head.Repo, request.Head.Number, request.Head.SHA)
	asked, err := kv.Get(requestKey)
	if err != nil {
		t.Fatalf("read the request: %v", err)
	}
	h.github.answerStatus(checkRunsPath, http.StatusNotFound)

	h.verifyPass()
	if lines := h.lines(verifyGaveUp); len(lines) != 1 {
		t.Fatalf("a read GitHub answered 404 logged %d give-ups, want 1", len(lines))
	}
	if slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
		t.Fatalf("the request at revision %d is still held after a failure no read mends", request.Revision)
	}
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d after the episode was given up, want 0", state.VerifiedRev)
	}

	before := h.github.count()
	for clock = clock.Add(30 * time.Second); clock.Sub(start) <= 31*time.Minute; clock = clock.Add(30 * time.Second) {
		if clock.Sub(start) == 16*time.Minute {
			h.github.answerStatus(checkRunsPath, 0)
		}
		rewritten, err := kv.Put(requestKey, asked.Value())
		if err != nil {
			t.Fatalf("write the request again: %v", err)
		}
		h.verifyPass()
		if slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == rewritten }) {
			t.Fatalf("the request of the given-up episode written again at revision %d is still held at %v", rewritten, clock.Sub(start))
		}
	}
	if read := h.github.sincePath(before, checkRunsPath); len(read) != 0 {
		t.Fatalf("the verify loop read the head %d times for requests of an episode it gave up", len(read))
	}
	if lines := h.lines(verifyGaveUp); len(lines) != 1 {
		t.Fatalf("the given-up episode logged %d give-ups in all, want 1", len(lines))
	}
	h.noSettlement(settlements, "after the verify loop gave the episode up")

	// A new observation re-arms the record: the listener's next claim is a new episode.
	lint := checkRun(2, "lint", "completed", "success", otherApp, "2026-11-22T00:06:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run, lint)
	h.post("check_run", webhookBody("check_run", lint), "d2")
	renewed := h.waitForRequest(request.Revision)
	if renewed.Episode == request.Episode {
		t.Fatalf("the request after a new observation asks for episode %d, the given-up one's", renewed.Episode)
	}
	h.verifyPass()
	if settled := h.settlement(settlements, "after the new episode's answer"); settled.Passed.Count != 2 {
		t.Fatalf("settlement = %+v, want ci-ok and lint passed", settled)
	}
}

// A failure no later read mends ends the episode at once: here a check-runs listing that does not
// end within maxPages (ten thousand runs, a hundred full pages), which writes no answer, logs one
// ERROR and deletes the request.
func TestAVerifyListingThatDoesNotEndWithinMaxPagesIsGivenUpAtOnce(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)
	runs := make([]object, 0, maxPages*100)
	for i := range maxPages * 100 {
		runs = append(runs, checkRun(int64(1000+i), fmt.Sprintf("job-%d", i), "completed", "success", otherApp, "2026-11-22T00:05:00Z"))
	}
	h.github.setCheckRuns(gitHubRepository, headSHA, runs...)

	h.verifyPass()

	if lines := h.lines(verifyGaveUp); len(lines) != 1 {
		t.Fatalf("a listing past %d pages logged %d give-ups, want 1", maxPages, len(lines))
	}
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d after a listing that did not end, want 0", state.VerifiedRev)
	}
	if slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
		t.Fatalf("the request at revision %d is still held after a failure no read mends", request.Revision)
	}
}

// The failures that end an episode at once are the ones every later read meets again.
func TestPermanentVerifyFailuresAreTheOnesNoLaterReadMends(t *testing.T) {
	for _, row := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"the App is not installed", fmt.Errorf("token: %w", githubapp.ErrNoInstallation), true},
		{"a page past the response limit", &githubapp.ResponseTooLargeError{Limit: 1 << 20}, true},
		{"a listing past maxPages", errTooManyPages, true},
		{"the answer's own write past the record's bound", fmt.Errorf("mark verified: %w", bus.ErrTooLarge), true},
		{"GitHub's 403 or 404 on the head's read", fmt.Errorf("check runs: %w: Acme/Widgets", githubapp.ErrRepositoryUnreadable), true},
		{"GitHub's 404", &githubapp.StatusError{Status: http.StatusNotFound}, true},
		{"GitHub's 410", &githubapp.StatusError{Status: http.StatusGone}, true},
		{"GitHub's 502", &githubapp.StatusError{Status: http.StatusBadGateway}, false},
		{"GitHub's 403 on a token", &githubapp.StatusError{Status: http.StatusForbidden}, false},
		{"a write that did not land", errWriteNotLanded, false},
	} {
		if got := permanentVerifyFailure(row.err); got != row.permanent {
			t.Errorf("%s: permanentVerifyFailure = %v, want %v", row.name, got, row.permanent)
		}
	}
}

// While GitHub still runs a check suite of the head with runs in it, the verify read writes what it
// read and no answer, so the head is not announced on runs GitHub is about to add; once the suite
// completes, the read answers (the completed suite re-arms the record, so the listener claims and
// asks once more) and the head is published.
func TestAVerifyReadHoldsItsAnswerWhileGitHubStillRunsASuiteOfTheHead(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	clock := time.Now()
	h.reconciler.now = func() time.Time { return clock }
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	running := checkSuite(901, "in_progress", "", otherApp, "2026-11-22T00:05:01Z")
	running["latest_check_runs_count"] = 1
	h.github.setCheckSuites(gitHubRepository, headSHA, running)
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)

	h.verifyPass()

	h.noSettlement(settlements, "while GitHub runs a suite of the head")
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d while GitHub runs a suite of the head, want 0", state.VerifiedRev)
	}
	if !h.logged("ci verify held its answer: GitHub still runs a check suite of the head") {
		t.Fatal("the verify read logged no held answer")
	}

	h.github.setCheckSuites(gitHubRepository, headSHA, checkSuite(901, "completed", "success", otherApp, "2026-11-22T00:06:00Z"))
	clock = clock.Add(verifyRetry + time.Second)
	h.verifyPass()
	renewed := h.waitForRequest(request.Revision)
	h.verifyPass()
	if settled := h.settlement(settlements, "once the suite completed"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if state, _, _ := h.record(); state.VerifiedRev < renewed.Revision {
		t.Fatalf("the record's VerifiedRev is %d, want the renewed request's %d", state.VerifiedRev, renewed.Revision)
	}
}

// A commit status listing GitHub refuses costs the verify answer the head's statuses alone, as it
// costs the walk: the answer is written and the head is published.
func TestARefusedStatusListingCostsTheVerifyAnswerItsStatusesAlone(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.github.setTokenPermissions(map[string]string{"checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"})
	h.github.refuseStatuses(gitHubRepository, headSHA)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	h.waitForRequest(0)
	before := h.github.count()

	h.verifyPass()

	if asked := h.github.sincePath(before, "/repos/"+gitHubRepository+"/commits/"+headSHA+"/status"); len(asked) == 0 {
		t.Fatal("the verify read never asked GitHub for the head's statuses")
	}
	if settled := h.settlement(settlements, "after a verify read whose status listing GitHub refused"); settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want ci-ok passed", settled)
	}
	if !h.logged("ci reconcile read no commit status of a head") {
		t.Fatal("the refused status listing was named at no WARN")
	}
}

// A rate limit a verify read meets writes no answer and counts as no failure of the head: the
// request stays, nothing is published, and the episode is not brought nearer its end. A limit on
// one installation's budget, which a check-runs listing meets, ends no pass: the answer reports
// that the pass goes on to the next request, whose installation may be another.
func TestARateLimitOnAVerifyReadWritesNoAnswerAndCountsNoFailure(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	settlements := h.subscribe()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	request := h.waitForRequest(0)
	h.github.rateLimitNext(checkRunsPath)
	writer, err := h.reconciler.Open()
	if err != nil {
		t.Fatalf("open a writer: %v", err)
	}
	before := h.github.count()

	goesOn := h.reconciler.verify(h.ctx, writer, request)

	if asked := h.github.sincePath(before, checkRunsPath); len(asked) == 0 {
		t.Fatal("the verify read never met the limit: it did not ask for the head's check runs")
	}
	if !goesOn {
		t.Fatal("a limit on the installation's budget ended the verify pass; only one on the App's budget does")
	}
	h.noSettlement(settlements, "after a verify read a rate limit stopped")
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("the record's VerifiedRev is %d after a rate-limited read, want 0", state.VerifiedRev)
	}
	if !slices.ContainsFunc(h.requests(), func(listed contracts.CIVerifyRequest) bool { return listed.Revision == request.Revision }) {
		t.Fatalf("the request at revision %d is gone after a rate-limited read", request.Revision)
	}
	if failed := h.reconciler.verifyFailed[verifyHeadKey(request.Head)]; failed.failures != 0 || h.logged("ci verify did not answer a verify request") {
		t.Fatalf("a rate-limited read counted as %d failed answers", failed.failures)
	}
}

// countingLeases is a lease pool that counts the leases asked of it and grants none.
type countingLeases struct {
	mu    sync.Mutex
	asked int
}

func (l *countingLeases) Acquire(context.Context) (*pgxpool.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked++
	return nil, errors.New("this test grants no lease")
}

func (l *countingLeases) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.asked
}

// A verify pass that lists no request takes no lease, so a Dispatch whose listeners all run with
// ENVOY_CI_VERIFY off takes no lease connection for the loop; a pass that lists one answers it
// under the loop's lease, and a lease it cannot take answers nothing.
func TestTheVerifyLoopTakesItsLeaseOnlyWhenARequestWaits(t *testing.T) {
	h := newVerifyHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHead()
	leases := &countingLeases{}
	h.reconciler.VerifyLeases = leases

	h.verifyPass()
	if asked := leases.count(); asked != 0 {
		t.Fatalf("a verify pass with no request asked for the lease %d times", asked)
	}

	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-22T00:05:00Z")
	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.post("check_run", webhookBody("check_run", run), "d1")
	h.waitForRequest(0)
	h.verifyPass()
	if asked := leases.count(); asked != 1 {
		t.Fatalf("a verify pass with a request asked for the lease %d times, want once", asked)
	}
	if state, _, _ := h.record(); state.VerifiedRev != 0 {
		t.Fatalf("a verify pass that held no lease answered: VerifiedRev %d", state.VerifiedRev)
	}
}
