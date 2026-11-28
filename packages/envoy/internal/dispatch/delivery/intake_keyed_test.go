package delivery

// How the intake runs its messages: one key at a time per pull request or run, keys at once, the
// subjects it does not want acknowledged before any decode, and what it gives back when it stops
// delivering.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldPullRequests is a fake GitHub pull request read that records each fetch's start and end by
// number and holds the first fetch of each number it was given until that number is released.
// Every fetch answers 500, which the intake logs and acknowledges.
type heldPullRequests struct {
	mu     sync.Mutex
	events []string
	// holds is the hold on each number this fake was given, by number: each has an entered of
	// its own and a release of its own, so a row can have two pull requests inside their fetches
	// at once and let them go one at a time.
	holds map[string]*pullRequestHold
}

// pullRequestHold is one number's hold: that number's first fetch closes entered and then waits
// for held, which release closes. Its later fetches are not held, so the messages queueing
// behind the held one are handled as they always are.
type pullRequestHold struct {
	entered chan struct{}
	enter   sync.Once
	held    chan struct{}
	release func()
}

// holdPullRequestOne holds the first fetch of pull request 1, and of every number in also, each
// until that number is released (release, releaseNumber). A row that needs two handlers inside
// their fetches at once names the second number here rather than installing a fake of its own.
// The held fetch does not watch the request's context, so a row's client of this fake ends a call
// only at the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp): at
// githubapp.RequestTimeout (20 s) the client would give the held fetch up and ask again, unheld,
// and the handler would finish while the row stages a step that needs it held.
func holdPullRequestOne(fake *fakeGitHub, also ...string) *heldPullRequests {
	held := newHeldPullRequests(append([]string{"1"}, also...))
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number := r.PathValue("number")
		held.record("start " + number)
		if hold := held.holds[number]; hold != nil && hold.first() {
			<-hold.held
		}
		held.record("end " + number)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return held
}

// newHeldPullRequests is a fake's record with one unreleased hold per number, for the two fakes
// of this package, which differ only in what their held fetch waits on.
func newHeldPullRequests(numbers []string) *heldPullRequests {
	held := &heldPullRequests{holds: make(map[string]*pullRequestHold, len(numbers))}
	for _, number := range numbers {
		hold := &pullRequestHold{entered: make(chan struct{}), held: make(chan struct{})}
		hold.release = sync.OnceFunc(func() { close(hold.held) })
		held.holds[number] = hold
	}
	return held
}

// first reports whether this fetch is its number's first, and marks the hold entered as it does:
// only that fetch waits for the release.
func (h *pullRequestHold) first() bool {
	first := false
	h.enter.Do(func() {
		first = true
		close(h.entered)
	})
	return first
}

func (h *heldPullRequests) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

// of is the recorded events of pull request number, in order.
func (h *heldPullRequests) of(number string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var events []string
	for _, event := range h.events {
		if event == "start "+number || event == "end "+number {
			events = append(events, event)
		}
	}
	return events
}

// release lets every fetch this fake holds go, which for a fake holding one number is that
// number's: what a row registers as its cleanup.
func (h *heldPullRequests) release() {
	for _, hold := range h.holds {
		hold.release()
	}
}

// releaseNumber lets one number's held fetch go, for a row that holds more than one and releases
// them one at a time. A number this fake holds nothing of is the row's own mistake, and panics
// rather than passing as a release that released nothing.
func (h *heldPullRequests) releaseNumber(number string) {
	hold, holding := h.holds[number]
	if !holding {
		numbers := make([]string, 0, len(h.holds))
		for held := range h.holds {
			numbers = append(numbers, held)
		}
		slices.Sort(numbers)
		panic(fmt.Sprintf("release pull request %s, which this fake does not hold: it holds %q", number, numbers))
	}
	hold.release()
}

// awaitEntered waits for every number this fake holds to be inside its first fetch: a row that
// holds two of them measures nothing until both are there.
func (h *heldPullRequests) awaitEntered(t *testing.T) {
	t.Helper()
	for number, hold := range h.holds {
		testwait.Receive(t, fmt.Sprintf("the intake to fetch pull request %s", number), hold.entered)
	}
}

// mergedPullRequest is the envelope fields of a merged pull request of acme/widgets by octocat,
// which the intake fetches from GitHub.
func mergedPullRequest(number int) map[string]string {
	return map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(number), "author": "octocat",
	}
}

func deliveryConsumerInfo(t *testing.T, natsClient *bus.Client) *natsgo.ConsumerInfo {
	t.Helper()
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the delivery consumer: %v", err)
	}
	return info
}

// runIntake runs intake until the returned stop is called or the test ends. stop ends Run's context
// and waits for Run to return through the shared wait (testwait.Receive), whose give-up names it. A
// row may call it more than once, each call ending the one context and waiting for the one return,
// and from a goroutine of its own, where a give-up fails the test and ends that goroutine.
func runIntake(t *testing.T, intake *Intake) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	stop = func() {
		cancel()
		testwait.Receive(t, "the intake's Run to return once its context ended", done)
	}
	t.Cleanup(stop)
	return stop
}

// fakeIntakeClock is an intake's own clock (heldDurable.clock) for a row of this file: a clocktest
// fake that stands at one moment until the row moves it. The intake's stop deadline (stopContext)
// and each handler's (Intake.deliver) pass on it only where the row moves it, so a row that never
// moves it has its stop wait for a handler for as long as the row holds the handler at GitHub.
func fakeIntakeClock() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
}

// While GitHub holds the fetch of pull request 1, a message of pull request 2 is handled and
// acknowledged, and a second message of pull request 1 waits behind the first: it starts only
// once the first has ended.
func TestAPullRequestHeldOnGitHubHoldsBackOnlyItsOwnMessages(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	// After the fake's server, so it runs first: the server's Close waits for the fetch it holds.
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	// GitHub holds pull request 1's fetch while the row stages its next steps, so the handler's own
	// deadline and the server's redelivery at the ack wait both lie past the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))

	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return slices.Equal(held.of("2"), []string{"start 2", "end 2"}) &&
			info.Delivered.Consumer-before == 3 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("pull request 2 was not handled and acknowledged while pull request 1 was held: events %q, consumer %+v", held.of("2"), info)
	})
	if events := held.of("1"); !slices.Equal(events, []string{"start 1"}) {
		t.Fatalf("pull request 1's events while its first fetch is held = %q, want its first start alone", events)
	}

	held.release()
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: its second message must start after its first ends", events, want)
	}
}

// Every subject but a pull request's own, a workflow run's and a push of a branch's is
// acknowledged on its subject alone, with no decode: a comment and a review on a pull request, its
// checks, an issue, a push of a tag and a job event. The pull request's own message is decoded,
// once.
func TestSubjectsTheIntakeDoesNotWantAreAcknowledgedWithoutADecode(t *testing.T) {
	// Not parallel: the row replaces the package variable decodeEnvelope, which every intake in
	// the process decodes with, so another test's intake would count in decodes.
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	var decodes atomic.Int64
	decode := decodeEnvelope
	decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
		decodes.Add(1)
		return decode(data)
	}
	// Before startIntake, so it is restored only once the intake has stopped.
	t.Cleanup(func() { decodeEnvelope = decode })
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)

	payload := `{"kind":"comment","action":"created","repo":"acme/widgets","number":"7","author":"octocat"}`
	for _, kind := range []string{"pr.7.comment", "pr.7.review", "pr.7.checks", "issue.3", "push.tag.v1_0_0", "job.500.completed"} {
		publishGitHubEnvelope(t, natsClient, contracts.GithubSubject("acme", "widgets", kind), payload)
	}
	awaitAcknowledged(t, natsClient)
	if got := decodes.Load(); got != 0 {
		t.Fatalf("decoded %d envelopes of subjects the intake does not want, want none", got)
	}

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	if got := decodes.Load(); got != 1 {
		t.Fatalf("decoded %d envelopes for one pull request message, want 1", got)
	}
}

// A message waiting behind its key when the intake stops is negatively acknowledged, so the next
// process to bind the durable is handed it at once rather than after the ack wait; the message in
// its handler then finishes and is acknowledged by the stopping intake.
func TestAMessageQueuedBehindItsKeyAtStopIsRedeliveredAtOnce(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	natsClient := intakeTestClient(t)
	ackWaitPastTheRun(t)
	first := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	// The stopping intake's clock is a fake the row never moves, so its stop never gives the held
	// handler up on its budget (intakeStopBudget): the handler ends only at the release below.
	first.clock = fakeIntakeClock()
	stopFirst := runIntake(t, first)
	// After the fake's server and runIntake's cleanup, so it runs before both: the server's Close
	// waits for the fetch it holds, and a stop begun with the fetch held waits for it on a clock
	// nothing moves.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before == 2 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("the second message of pull request 1 was not delivered behind the first: %+v", info)
	})

	// Run stops delivering as its context ends, and returns once the held fetch has ended.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})
	held.release()
	testwait.Receive(t, "the stopping intake to return once its handler ended", stopped)
	if events := held.of("1"); !slices.Equal(events, []string{"start 1", "end 1"}) {
		t.Fatalf("the stopping intake's fetches of pull request 1 = %q, want the held one alone", events)
	}

	// The ack wait is a day here (ackWaitPastTheRun), so only the message's hand-back brings it to
	// the second process inside the run.
	runIntake(t, newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// holdPullRequestOneUntilAbandoned is holdPullRequestOne, except that the held fetch also ends when
// the intake abandons it (the request's context ends), so its end is recorded as soon as the
// intake stops waiting on it, by a release or by giving the call up. A row's client of this fake
// ends a call only at the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp) too, so the
// client's own bound is not what gives the call up.
func holdPullRequestOneUntilAbandoned(fake *fakeGitHub) *heldPullRequests {
	held := newHeldPullRequests([]string{"1"})
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number := r.PathValue("number")
		held.record("start " + number)
		if hold := held.holds[number]; hold != nil && hold.first() {
			select {
			case <-hold.held:
			case <-r.Context().Done():
			}
		}
		held.record("end " + number)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return held
}

// A process standing by for the durable when the intake holding it stops does not start a pull
// request's next message while the stopping intake is still handling the one before it: the order
// of one key's messages holds across the hand-over, as it does inside one intake. The message the
// stopping intake gives back reaches the standby only once its handler of that key has ended or
// given up its GitHub call.
func TestAHandOverKeepsAPullRequestsMessagesInOrder(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	natsClient := intakeTestClient(t)
	// The message given back is what must reach the standby, so the server's own redelivery at the
	// ack wait never comes inside the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	first := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	// The stopping intake's clock is a fake the row never moves, so its stop never gives the held
	// handler up on its budget (intakeStopBudget): the handler ends only at the release below.
	first.clock = fakeIntakeClock()
	stopFirst := runIntake(t, first)
	// After the fake's server and the first intake's runIntake cleanup, so it runs before both: the
	// server's Close waits for the fetch it holds, and the first intake's stop waits for it on a
	// clock nothing moves.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before == 2 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("the second message of pull request 1 was not delivered behind the first: %+v", info)
	})

	// The standby is running before the stop, as the second envoy-dispatch process of a rolling
	// deploy is. It tries for the hold and the durable on its hold's clock (heldDurable.holdClock),
	// a fake the row moves one durableBindInterval at a time, and its tuning, a copy of the row's,
	// reports each try as it ends (intakeTuning.attempted): whether the try left the standby
	// delivering. The report runs on the standby's run loop, the one writer of what it reads.
	//
	// The hold the standby takes in the end is duty's, whose loss deadline is the wall clock's,
	// and the standby judges it on its hold's clock (heldDurable.holds), so the fake is a
	// clocktest.Held one, whose Now is the wall clock plus what the row moved it. The row moves it
	// triesWhileHeld bind intervals while the first intake's handler is held and one more for the
	// try that takes the hold, so it starts that far behind the wall: at the take its Now is the
	// wall clock's, as duty's deadline is.
	const triesWhileHeld = 4
	standbyTuning := copyTuning(tuningFor(t))
	standby := newIntake(standbyTuning, intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	standbyClock := clocktest.Held()
	standbyClock.Set(standbyClock.Now().Add(-(triesWhileHeld + 1) * durableBindInterval))
	standby.holdClock = standbyClock
	var tries testwait.Stream[bool]
	tried := func() { tries.Add(standby.subject != "") }
	standbyTuning.attempted.Store(&tried)
	runIntake(t, standby)
	anyTry := func(bool) bool { return true }
	if testwait.Next(t, "the standby's first try for the durable, as it starts", &tries, anyTry) {
		t.Fatal("the standby took the durable over at its start, while the first intake held it")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.delivering != nil && first.delivering.stopping.Load()
	}, func() string { return "the first intake never began stopping" })
	// The stop drains the subscription before it waits for its handlers, so from here on it is
	// waiting for pull request 1's.
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})

	// overlapped reports whether a fetch of pull request 1 started while an earlier one had not
	// yet ended.
	overlapped := func(events []string) bool {
		open := false
		for _, event := range events {
			switch event {
			case "start 1":
				if open {
					return true
				}
				open = true
			case "end 1":
				open = false
			}
		}
		return false
	}
	// The standby tries for the hold every durableBindInterval and finds it live: the stopping
	// intake holds it until its stop has ended, and the stop waits for its handler of pull request
	// 1, which GitHub holds. The row moves the standby's clock to each of triesWhileHeld tries in
	// turn, and each try has ended, with the pull request's next message started by no one and the
	// durable taken over by no one, before the row reads it.
	for try := 1; try <= triesWhileHeld; try++ {
		standbyClock.Advance(durableBindInterval)
		tookOver := testwait.Next(t, fmt.Sprintf("the standby's try %d after the first intake began stopping", try), &tries, anyTry)
		events := held.of("1")
		if overlapped(events) {
			held.release()
			t.Fatalf("pull request 1's events across the hand-over = %q: the standby started the pull request's next message "+
				"while the stopping intake's handler of the one before it was still waiting on GitHub", events)
		}
		if tookOver {
			held.release()
			t.Fatalf("the standby took the durable over at its try %d after the first intake began stopping, while that intake's handler of pull request 1 was still waiting on GitHub (events %q)", try, events)
		}
	}
	// What kept the standby out, and what it would need to start the pull request's next message:
	// the hold is still the stopping intake's, and both of the pull request's messages are still
	// outstanding with that intake, neither handed back.
	if !holdLive(t, pool, first.holder) {
		held.release()
		t.Fatal("the stopping intake let the durable's hold go while its handler of pull request 1 was still waiting on GitHub")
	}
	if outstanding := deliveryAckPending(natsClient); outstanding != 2 {
		held.release()
		t.Fatalf("the durable reports %d message(s) outstanding while the stopping intake's handler of pull request 1 waits on GitHub, want both of its messages: the one behind it was handed back before its handler ended",
			outstanding)
	}

	held.release()
	testwait.Receive(t, "the stopping intake to return once its handler ended", stopped)
	waitFor(t, func() bool { return !holdLive(t, pool, first.holder) }, func() string {
		return "the stopped intake still holds the durable's hold"
	})
	// The standby's next try takes the hold, which the stopped intake has let go, and with it the
	// durable, created again past its ack floor, where the message handed back lies.
	testwait.Until(t, "the standby to take the durable over at a try made once the stopped intake let the hold go", func() (string, bool) {
		standbyClock.Advance(durableBindInterval)
		tookOver := testwait.Next(t, "the standby's next try for the durable", &tries, anyTry)
		return fmt.Sprintf("the standby's last try took the durable over: %t", tookOver), tookOver
	})
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q", events, want)
	}
}

// A stopping intake does not wait out a GitHub call that does not answer. Dispatch's whole
// shutdown has cmd/dispatch's stopGrace, 30 seconds (the pod's terminationGracePeriodSeconds less
// its pre-stop wait), and the intake's stop starts at the signal, so it returns well inside that
// however long GitHub holds the handler's call: here GitHub never answers. The intake's clock is a
// fake the row moves: the stop arms its deadline the intake's stopBudget from the signal, and once
// the fake reaches it the stop returns, having given the handler up (its WARN line says so) rather
// than waited for the call, which nothing but the release at the row's end lets go.
func TestAStoppingIntakeDoesNotWaitOutAGitHubCallThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	natsClient := intakeTestClient(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	stopClock := fakeIntakeClock()
	intake.clock = stopClock
	stop := runIntake(t, intake)
	// After the fake's server and runIntake's cleanup, so it runs before both: a row that fails
	// before it moves stopClock past the stop's deadline has its stop find the fetch let go, rather
	// than wait for it on a clock nothing moves.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	signalled := stopClock.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	// The held handler's own deadline, the intake's message timeout from its start, is on the same
	// clock (Intake.deliver) and later than the stop's: the row waits for both and reads the earlier.
	budget := tuningFor(t).stopBudget
	if armed := stopClock.AwaitArmed(t, "the held handler's deadline and the stop's", 2); !armed[0].Equal(signalled.Add(budget)) {
		t.Fatalf("the stop armed its deadline for %v, want %v: the intake's stopBudget (%s) from the signal", armed[0], signalled.Add(budget), budget)
	}
	stopClock.Advance(budget)
	testwait.Receive(t, "the stopping intake to return once the fake clock reached its stop deadline", stopped)
	if events := held.of("1"); !slices.Equal(events, []string{"start 1"}) {
		t.Fatalf("pull request 1's events once the stop returned = %q, want its fetch still held by GitHub: the stop ended on the call, not on its budget", events)
	}
	if !loggedMessage(logs(), "dispatch delivery: the intake stopped before the event's handling finished; handing it back") {
		t.Fatalf("the stop returned without saying it gave the handler of pull request 1 up; the messages logged were %q", loggedMessages(logs()))
	}
}

// bindReturnsAfterSignal is how far TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal
// moves the intake's clock past Run's ctx's end before it lets the bind that has subscribed
// return: two thirds of the row's stop budget.
const bindReturnsAfterSignal = 2 * time.Second

// A bind that subscribed before Run's ctx ended and returns after it -- a standby's bind poll under
// way at the signal, whose JetStream calls do not watch ctx -- is stopped under the one stop budget,
// which runs from ctx's end: the handler of the message the subscription was handed is given up
// the intake's stopBudget after the signal, however late the bind returned, and its message handed back,
// so the next process to bind the durable redoes it at once. With the budget begun at the bind's
// return, a late bind pushed the whole stop -- and cmd/dispatch's deregister and pool close behind
// it -- that much later. The intake's clock is a fake the row moves: the stop's deadline is armed
// at the signal, while the bind is still held, and the fake reaching it ends the stop, the bind
// having returned part way to it.
func TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal(t *testing.T) {
	t.Parallel()
	tuning := tuningFor(t)
	tuning.stopBudget = 3 * time.Second
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	natsClient := intakeTestClient(t)
	// The message given back is what the next process must redo at once, so the server's own
	// redelivery at the ack wait never comes inside the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	stopClock := fakeIntakeClock()
	intake.clock = stopClock
	subscribed, bindReturns := make(chan struct{}), make(chan struct{})
	intake.boundHook = func() {
		close(subscribed)
		<-bindReturns
	}
	stop := runIntake(t, intake)
	// After runIntake's cleanup, so it runs first: Run waits for the bind it holds.
	letBindReturn := sync.OnceFunc(func() { close(bindReturns) })
	t.Cleanup(letBindReturn)
	// After the fake's server and runIntake's cleanup too, so it runs before both: a row that fails
	// before it moves stopClock past the stop's deadline has its stop find the fetch let go, rather
	// than wait for it on a clock nothing moves.
	t.Cleanup(held.release)
	testwait.Receive(t, "the intake to subscribe to its durable", subscribed)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)

	signalled := stopClock.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	// The deadline is armed as ctx ends, while the bind is still held: a stop that armed it at the
	// bind's return would leave only the held handler's own deadline armed here, which is on the
	// same clock (Intake.deliver) and six hours out (ackWaitPastTheRun). The row waits for both and
	// reads the earlier.
	if armed := stopClock.AwaitArmed(t, "the held handler's deadline and the stop's, armed as Run's ctx ended", 2); !armed[0].Equal(signalled.Add(tuning.stopBudget)) {
		t.Fatalf("the stop armed its deadline for %v, want %v: the intake's stopBudget (%s) from the signal, before the bind returned",
			armed[0], signalled.Add(tuning.stopBudget), tuning.stopBudget)
	}
	stopClock.Advance(bindReturnsAfterSignal)
	letBindReturn()
	// The rest of the budget the signal began: the stop ends here, bindReturnsAfterSignal earlier
	// than a budget begun at the bind's return would let it.
	stopClock.Advance(tuning.stopBudget - bindReturnsAfterSignal)
	testwait.Receive(t, "the stopping intake to return once the fake clock reached the deadline armed at the signal", stopped)
	// The fake records the end once it sees the request cancelled, a moment after the client did.
	waitFor(t, func() bool { return slices.Equal(held.of("1"), []string{"start 1", "end 1"}) }, func() string {
		return fmt.Sprintf("pull request 1's events once Run returned = %q, want the held fetch given up", held.of("1"))
	})

	runIntake(t, newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process redoes the message given up", events, want)
	}
}

// A message NATS has delivered into the client's buffer, but the subscription's callback has not
// yet routed when the intake stops, is negatively acknowledged rather than handled, and the next
// process to bind the durable handles it.
func TestAMessageInTheClientBufferAtStopIsGivenBack(t *testing.T) {
	// Not parallel: the row replaces the package variable decodeEnvelope, which every intake in
	// the process decodes with, so another test's intake would hold its first decode.
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	// The first decode, the workflow run's in the callback, waits until unblock: the messages
	// behind it stay in the client's buffer meanwhile.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	var first sync.Once
	decode := decodeEnvelope
	decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
		hold := false
		first.Do(func() { hold = true })
		if hold {
			close(blocked)
			<-unblock
		}
		return decode(data)
	}
	t.Cleanup(func() { decodeEnvelope = decode })
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	natsClient := intakeTestClient(t)
	// The message given back is what must bring pull request 9 to the next process, so the server's
	// own redelivery at the ack wait never comes inside the run: both intakes take a message timeout
	// past it (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	stopFirst := runIntake(t, intake)
	release := sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	awaitBoundDurable(t, natsClient)

	// Another repository's run: whichever way the stop finds it, nothing is fetched for it.
	publishWorkflowEnvelope(t, natsClient, "other-org", "other-repo", ".github/workflows/deploy.yml", 9001)
	testwait.Receive(t, "the intake's callback to decode the workflow run's envelope", blocked)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 9, mergedPullRequest(9))
	intake.mu.Lock()
	delivery := intake.delivering
	intake.mu.Unlock()
	waitFor(t, func() bool {
		buffered, _, err := delivery.sub.Pending()
		return err == nil && buffered >= 2
	}, func() string { return "the pull request's message never reached the client's buffer" })

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	release()
	testwait.Receive(t, "the stopping intake to return once its callback was free", stopped)
	if events := held.of("9"); len(events) != 0 {
		t.Fatalf("the stopping intake fetched the buffered pull request: %q", events)
	}

	runIntake(t, newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("9"), []string{"start 9", "end 9"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 9's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// A handler that finishes inside the stop has its message acknowledged there and then, where the
// handler finished (intake.go's deliver), not when the stop releases what it holds. Held to the
// end of the stop, that message keeps one of the next holder's four in-flight slots
// (intakeMaxAckPending) for every second the stop waits for the slowest handler -- during a
// rolling deploy, a slot of the very process this stop is handing the durable to.
//
// Two pull requests are held inside their fetches before the stop begins, so both are
// outstanding when it does, and the second is released while the stop still waits for the
// first: the durable's outstanding count has to fall to one there, with the stop still running.
// A stop that holds every acknowledgement of a finished handler leaves it at two until the
// first fetch is released too.
func TestAHandlerThatFinishesInsideTheStopIsAcknowledgedAtOnce(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake, "2")
	// What the row reads is the durable's whole outstanding count, which only this row's messages
	// make: the JetStream account testnats.URL gives the row is its own, and so are its stream and
	// durable.
	natsClient := intakeTestClient(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t))), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	// The stop has to be waiting for pull request 1's handler still while the counts below are
	// read, and it is this row that releases that handler: the intake's clock is a fake the row
	// never moves, so the stop's budget never gives the handler up first.
	intake.clock = fakeIntakeClock()
	stop := runIntake(t, intake)
	// Registered after runIntake, so it runs before the stop that cleanup makes: a stop begun
	// with both fetches still held waits for them on a clock nothing moves.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)

	// Two keys, so the two handlers run beside each other (intakeMaxAckPending is 4) rather than
	// one waiting behind the other.
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))
	held.awaitEntered(t)
	// The server has counted a message outstanding by the time its handler is inside its fetch,
	// and this read only has to see both.
	waitFor(t, func() bool { return deliveryAckPending(natsClient) == 2 }, func() string {
		return fmt.Sprintf("the durable reports %d message(s) outstanding with both handlers inside their fetches, want 2 (-1 reads as a consumer this test could not read)",
			deliveryAckPending(natsClient))
	})

	// The stop runs beside this turn, as TestAMessageInTheClientBufferAtStopIsGivenBack runs it:
	// it waits here for both handlers, until the budget.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	// The subscription goes before the stop waits for its handlers, so from here on what the
	// durable reports outstanding is what this stopping process still owes it.
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})
	held.releaseNumber("2")

	// The acknowledgement is published as the handler returns, with no bind and no delivery to
	// wait for, and the stop is still waiting for pull request 1 while it does: the check below
	// reads that the stop had not ended when the count fell.
	waitFor(t, func() bool { return deliveryAckPending(natsClient) == 1 }, func() string {
		return fmt.Sprintf("the durable still reports %d message(s) outstanding while the stop waits for the other handler, want 1: the finished handler's acknowledgement was held for the stop rather than made where the handler finished",
			deliveryAckPending(natsClient))
	})
	select {
	case <-stopped:
		t.Fatal("the stop ended before pull request 1's fetch was released, so the count above was read of a stop that was waiting for nothing")
	default:
	}

	held.release()
	testwait.Receive(t, "the stopping intake to return once both handlers were released", stopped)
}

// subscribe at a deliver subject it is given binds the durable created with that subject without
// looking it up, as a process taking the durable over after recreating it does; StopDelivering
// ends that subscription, and a second call returns at once.
func TestAnIntakeSubscribesAtTheDeliverSubjectItIsGiven(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	wanted := deliveryConsumerConfig(tuningFor(t))
	// Starting at new messages, so a durable recreated on the package's shared stream is handed
	// only this test's.
	wanted.DeliverPolicy = natsgo.DeliverNewPolicy
	if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("delete the delivery durable: %v", err)
	}
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	intake := newIntake(tuningFor(t), natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	if _, err := intake.subscribe(wanted.DeliverSubject); err != nil {
		t.Fatalf("subscribe at %s: %v", wanted.DeliverSubject, err)
	}
	t.Cleanup(func() { stopDeliveringJoined(t, context.Background(), intake) })

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Fatalf("settings after the handled message = %+v, %v: want last_event_at recorded", settings, err)
	}

	stopDeliveringJoined(t, t.Context(), intake)
	stopDeliveringJoined(t, t.Context(), intake)
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})
}

// A stopped intake stays stopped across a reconnect of its client: the reconnect does not bind its
// stopped subscription to the durable again, where its callback would hand every message straight
// back. A subscribe afterwards on the same client, at the deliver subject of the durable created
// again, as a process that lost and regained the durable does, binds it and handles its messages.
// The server's own subscription list is read for the rebind, after a round trip on the client's
// connection, which the server answers only once it has processed every SUB the reconnect sent.
func TestAStoppedIntakeStaysStoppedAcrossAReconnectAndSubscribesAgain(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	recreate := func() natsgo.ConsumerConfig {
		t.Helper()
		wanted := deliveryConsumerConfig(tuningFor(t))
		// Starting at new messages, so a durable recreated on the package's shared stream is handed
		// only this test's.
		wanted.DeliverPolicy = natsgo.DeliverNewPolicy
		if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
			t.Fatalf("delete the delivery durable: %v", err)
		}
		if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
			t.Fatalf("create the delivery durable: %v", err)
		}
		return wanted
	}
	intake := newIntake(tuningFor(t), natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	stoppedDurable := recreate()
	if _, err := intake.subscribe(stoppedDurable.DeliverSubject); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { stopDeliveringJoined(t, context.Background(), intake) })
	stopDeliveringJoined(t, t.Context(), intake)
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})

	if err := natsClient.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	testwait.Receive(t, "the client's reconnect hooks to run", hooked)
	// The client restores its subscriptions before it runs the hooks, so a SUB it re-sent went out
	// before this round trip, and the server has entered it in its subscription list by the answer:
	// from then on the list says whether the reconnect bound the stopped subscription again, and so
	// does the durable.
	if err := natsClient.Conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("a round trip on the reconnected connection: %v", err)
	}
	testnats.AwaitNoSubscription(t, natsClient.Conn, stoppedDurable.DeliverSubject)
	if deliveryConsumerInfo(t, natsClient).PushBound {
		t.Fatal("the reconnect bound the stopped intake's subscription to the durable again")
	}

	if _, err := intake.subscribe(recreate().DeliverSubject); err != nil {
		t.Fatalf("subscribe again on the same client: %v", err)
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 8, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "8", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Fatalf("settings after the message handled by the second subscription = %+v, %v: want last_event_at recorded", settings, err)
	}
}

// A stop after the client replaced its connection drains the subscription the client bound again
// on the new connection, which is the one delivering, rather than the handle subscribe returned,
// which the closed connection took with it. The intake stays the durable's holder throughout: its
// hold is Postgres's and a replaced NATS connection takes nothing from it, so nothing here rebinds
// and the live handle is the client's own. The durable loses its subscriber as the stop begins, so
// a message published while a handler still runs is not handed to the stopping process, which would
// hand it back and be handed it again; the stop logs no failed drain, and the next process to bind
// the durable handles the message.
func TestAStopAfterTheConnectionWasReplacedDrainsTheRestoredSubscription(t *testing.T) {
	t.Parallel()
	// The row requires that no component logged a line about a drain: the intakes, the duty leases
	// and holds they take, and their NATS clients take the test's logger, and nothing else on the
	// path (the store, GitHub's client) writes one.
	logs, logger := captureTestLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	natsClient := intakeTestClient(t, bus.WithLogger(logger))
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	// The next process must be handed the message published during the stop, so the server's own
	// redelivery at the ack wait never comes inside the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	// A fake the row never moves, so the stop never gives the held handler up on its budget: the
	// handler ends at the release below, and its message is acknowledged rather than handed back.
	intake.clock = fakeIntakeClock()
	stop := runIntake(t, intake)
	// After the fake's server and runIntake's cleanup, so it runs before both: a stop begun with the
	// fetch held waits for it on a clock nothing moves.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)

	// The client dials a connection to replace the closed one and binds its registration there: a
	// handle of its own, not the one subscribe returned.
	natsClient.Conn.Close()
	testwait.Receive(t, "the client's reconnect hooks to run", hooked)
	waitFor(t, func() bool { return natsClient.SubOK() && deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the client never bound the intake's subscription again on its new connection"
	})
	intake.mu.Lock()
	delivery := intake.delivering
	intake.mu.Unlock()
	if delivery.sub.IsValid() {
		t.Fatal("the handle subscribe returned is still valid on the replaced connection; the test proves nothing")
	}

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	waitFor(t, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	waitFor(t, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake's subscription stayed bound to the durable while its handler ran: the stop did not drain " +
			"the subscription the client restored on its new connection"
	})
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))
	// The stream signals the durable of each message it stores, and a push durable delivers on that
	// signal only while it has a subscriber. Once the durable counts pull request 2 pending, it has
	// taken that signal: read then, unbound and with its delivered count where the drain left it, it
	// had no subscriber of the stopping intake's to hand the message to. The count after the stop,
	// below, holds that none came back later.
	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.NumPending > 0
	}, func() string {
		return fmt.Sprintf("the durable never counted pull request 2, published during the stop, pending: %+v", info)
	})
	if info.PushBound || info.Delivered.Consumer != before {
		t.Fatalf("with pull request 2 pending the durable reads push_bound=%t and %d message(s) delivered since the stop drained it, want unbound and none: the stopping intake was still subscribed",
			info.PushBound, info.Delivered.Consumer-before)
	}
	// What the row is about is a stop draining while a handler still runs, so pull request 1's
	// handler is still inside its held fetch here.
	if events := held.of("1"); !slices.Equal(events, []string{"start 1"}) {
		t.Fatalf("pull request 1's events as pull request 2 was published during the stop = %q, want its held fetch alone: the stop drained with no handler running", events)
	}

	held.release()
	testwait.Receive(t, "the stopping intake to return once its handler ended", stopped)
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer; delivered != before {
		t.Fatalf("the durable delivered %d message(s) to the stopping intake after its stop began, want none", delivered-before)
	}
	for _, record := range logs() {
		if message, _ := record["msg"].(string); strings.Contains(message, "drain") {
			t.Errorf("the stop logged %q (%v)", message, record["error"])
		}
	}

	runIntake(t, newIntake(tuningFor(t), intakeTestClient(t, bus.WithLogger(logger)), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("2"), []string{"start 2", "end 2"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 2's events = %q, want %q: the next process handles the message published during the stop", events, want)
	}
}

// githubEnvelopeData is one GitHub envelope's bytes as the bus carries them, for a test that
// hands intakeKey a message of its own rather than publishing one.
func githubEnvelopeData(t *testing.T, topic, payload string) []byte {
	t.Helper()
	data, err := json.Marshal(contracts.Envelope{
		EventID: "event-1", Source: "github", SourceEventID: "1", Topic: topic,
		DedupeKey: "github.1", Payload: payload, TraceID: "test-trace", IssuedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("encode the envelope of %s: %v", topic, err)
	}
	return data
}

// The repository a message's subject names decides whether the intake wants it: the one the
// payload names is the canonical one, and a subject naming another is the copy the listener
// publishes under a renamed repository's former name (internal/ghrename), which the original
// already carries to the same handler. A message that names no repository to compare -- an
// envelope or a payload that does not decode, or a payload with no repo -- is wanted, since it
// cannot be judged, and a pull request or a workflow run is keyed as it was keyed before the rule.
// A push of a branch is wanted under its subject's repository, whether or not its envelope
// decodes, and a push of a tag is not wanted at all.
func TestIntakeKeyWantsOnlyTheSubjectOfTheRepositoryThePayloadNames(t *testing.T) {
	t.Parallel()
	const (
		prPayload       = `{"kind":"pr","action":"closed","merged":"true","repo":"cybermonklabs/legion","number":"42","author":"octocat"}`
		workflowPayload = `{"kind":"workflow","action":"completed","repo":"cybermonklabs/legion","run_id":"77"}`
		pushPayload     = `{"kind":"push","repo":"cybermonklabs/legion","ref":"refs/heads/main"}`
		tagPushPayload  = `{"kind":"push","repo":"cybermonklabs/legion","ref":"refs/tags/v1.0.0"}`
	)
	for _, tc := range []struct {
		name     string
		subject  string
		payload  string
		data     []byte
		key      string
		wanted   bool
		envelope bool
	}{
		{
			name:    "a pull request under its payload's repository",
			subject: "notifications.github.cybermonklabs.legion.pr.42", payload: prPayload,
			key: "pr cybermonklabs/legion#42", wanted: true, envelope: true,
		},
		{
			name:    "the same pull request under the repository's former name",
			subject: "notifications.github.sjawhar.legion.pr.42", payload: prPayload,
		},
		{
			name:    "a workflow run under its payload's repository",
			subject: "notifications.github.cybermonklabs.legion.workflow.deploy_yml.completed", payload: workflowPayload,
			key: "workflow cybermonklabs/legion#77", wanted: true, envelope: true,
		},
		{
			name:    "the same workflow run under the repository's former name",
			subject: "notifications.github.sjawhar.legion.workflow.deploy_yml.completed", payload: workflowPayload,
		},
		{
			// A run of the default branch is published on its default-branch topic too, beside the
			// workflow topic above, with the same payload and dedupe key: its fifth token is no
			// subject the intake keys, so the run is handled once, under its workflow topic.
			name:    "the default-branch copy of a workflow run",
			subject: "notifications.github.cybermonklabs.legion.default_branch.workflow.deploy_yml.completed.push", payload: workflowPayload,
		},
		{
			name:    "a repository name holding a dot, which the subject sanitizes",
			subject: "notifications.github.acme.site_io.pr.7", payload: `{"kind":"pr","repo":"acme/site.io","number":"7"}`,
			key: "pr acme/site_io#7", wanted: true, envelope: true,
		},
		{
			name:    "a capital in the payload's repository against a lowercase subject",
			subject: "notifications.github.cybermonklabs.legion.pr.42", payload: `{"kind":"pr","repo":"CyberMonkLabs/Legion","number":"42"}`,
			key: "pr cybermonklabs/legion#42", wanted: true, envelope: true,
		},
		{
			name:    "a pull request whose envelope does not decode",
			subject: "notifications.github.sjawhar.legion.pr.42", data: []byte("{not an envelope"),
			key: "pr sjawhar/legion#42", wanted: true,
		},
		{
			name:    "a workflow run whose envelope does not decode",
			subject: "notifications.github.sjawhar.legion.workflow.deploy_yml.completed", data: []byte("{not an envelope"),
			key: "workflow notifications.github.sjawhar.legion.workflow.deploy_yml.completed", wanted: true,
		},
		{
			name:    "a pull request whose payload does not decode",
			subject: "notifications.github.sjawhar.legion.pr.42", payload: "not a payload",
			key: "pr sjawhar/legion#42", wanted: true, envelope: true,
		},
		{
			name:    "a workflow run whose payload does not decode",
			subject: "notifications.github.sjawhar.legion.workflow.deploy_yml.completed", payload: `{"run_id":["77"]}`,
			key: "workflow notifications.github.sjawhar.legion.workflow.deploy_yml.completed", wanted: true, envelope: true,
		},
		{
			name:    "a pull request whose payload names no repository",
			subject: "notifications.github.sjawhar.legion.pr.42", payload: `{"kind":"pr","repo":"","number":"42"}`,
			key: "pr sjawhar/legion#42", wanted: true, envelope: true,
		},
		{
			name:    "a workflow run whose payload names no repository",
			subject: "notifications.github.sjawhar.legion.workflow.deploy_yml.completed", payload: `{"kind":"workflow","repo":"","run_id":"77"}`,
			key: "workflow #77", wanted: true, envelope: true,
		},
		{name: "a review on a pull request", subject: "notifications.github.cybermonklabs.legion.pr.42.review", payload: prPayload},
		{name: "the checks of a pull request", subject: "notifications.github.cybermonklabs.legion.pr.42.checks", payload: prPayload},
		{
			name:    "a push of a branch under its payload's repository",
			subject: "notifications.github.cybermonklabs.legion.push.branch.main", payload: pushPayload,
			key: "push cybermonklabs/legion", wanted: true, envelope: true,
		},
		{name: "a push of a tag", subject: "notifications.github.cybermonklabs.legion.push.tag.v1_0_0", payload: tagPushPayload},
		{
			name:    "a push of a branch whose payload names another repository",
			subject: "notifications.github.sjawhar.legion.push.branch.main", payload: pushPayload,
		},
		{
			name:    "a push of a branch whose envelope does not decode",
			subject: "notifications.github.sjawhar.legion.push.branch.main", data: []byte("{not an envelope"),
			key: "push sjawhar/legion", wanted: true,
		},
		{name: "a job", subject: "notifications.github.cybermonklabs.legion.job.500.completed", payload: prPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = githubEnvelopeData(t, tc.subject, tc.payload)
			}
			key, envelope, wanted := intakeKey(&natsgo.Msg{Subject: tc.subject, Data: data})
			if wanted != tc.wanted || key != tc.key {
				t.Fatalf("intakeKey(%s) = key %q, wanted %t; want key %q, wanted %t", tc.subject, key, wanted, tc.key, tc.wanted)
			}
			if (envelope != nil) != tc.envelope {
				t.Fatalf("intakeKey(%s) handed on envelope %v, want handed on = %t: a decode the handler would otherwise redo is handed on, and one that failed is not",
					tc.subject, envelope, tc.envelope)
			}
		})
	}
}

// The listener publishes a renamed repository's envelopes a second time under the repository's
// former name (internal/ghrename): the same event_id, the same dedupe_key and the same payload,
// differing only in the topic. Both copies reach this durable. Only the one whose subject is the
// repository its payload names is wanted, so one merged pull request keys once and is fetched
// from GitHub once; keyed by their own subjects the two copies would key apart, and keyedRunner
// would run two handlers at once on that one pull request's row.
func TestTheCopyUnderARepositorysFormerNameRunsNoSecondHandler(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	var fetches atomic.Int64
	fake.handle("GET /repos/cybermonklabs/legion/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		// The fetch's answer decides nothing here; what is counted is that it happened once. A
		// 500 is logged and acknowledged, as every failed fetch of a merged pull request is.
		w.WriteHeader(http.StatusInternalServerError)
	})
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)

	id := testID(t)
	published := contracts.Envelope{
		EventID: id, Source: "github", SourceEventID: id, DedupeKey: "github." + id,
		Topic:   "notifications.github.cybermonklabs.legion.pr.42",
		Payload: `{"kind":"pr","action":"closed","merged":"true","repo":"cybermonklabs/legion","number":"42","author":"octocat"}`,
		TraceID: id, IssuedAt: time.Now().Unix(),
	}
	renamed := published
	renamed.Topic = "notifications.github.sjawhar.legion.pr.42"
	copies := []contracts.Envelope{published, renamed}

	keys := map[string]int{}
	for _, item := range copies {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode the envelope of %s: %v", item.Topic, err)
		}
		if key, _, wanted := intakeKey(&natsgo.Msg{Subject: item.Topic, Data: data}); wanted {
			keys[key]++
		}
	}
	if len(keys) != 1 || keys["pr cybermonklabs/legion#42"] != 1 {
		t.Fatalf("the two copies of one merged pull request key as %v, want only pr cybermonklabs/legion#42 once", keys)
	}

	for _, item := range copies {
		if err := natsClient.Publish(item); err != nil {
			t.Fatalf("publish %s: %v", item.Topic, err)
		}
	}
	awaitAcknowledged(t, natsClient)
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetched pull request 42 from GitHub %d times for one merged pull request published under its repository and under the repository's former name, want 1", got)
	}
}
