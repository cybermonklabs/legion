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
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
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
		select {
		case <-hold.entered:
		case <-time.After(20 * time.Second):
			t.Fatalf("the intake never fetched pull request %s", number)
		}
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

// runIntake runs intake until the returned stop is called or the test ends, and stop waits for Run
// to return.
func runIntake(t *testing.T, intake *Intake) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return stop
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
	github := fake.newTestClient()
	// After the fake's server, so it runs first: the server's Close waits for the fetch it holds.
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))

	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
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

// Every subject but a pull request's own and a workflow run's is acknowledged on its subject alone,
// with no decode: a comment and a review on a pull request, its checks, an issue and a job event.
// The pull request's own message is decoded, once.
func TestSubjectsTheIntakeDoesNotWantAreAcknowledgedWithoutADecode(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	var decodes atomic.Int64
	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	// This intake's own decode, so the count is of what it decoded and not of what an intake of a
	// test running beside this one did.
	intake.decode = func(data []byte) (contracts.Envelope, error) {
		decodes.Add(1)
		return decodeEnvelope(data)
	}
	startIntake(t, intake)
	awaitBoundDurable(t, natsClient)

	payload := `{"kind":"comment","action":"created","repo":"acme/widgets","number":"7","author":"octocat"}`
	for _, kind := range []string{"pr.7.comment", "pr.7.review", "pr.7.checks", "issue.3", "job.500.completed"} {
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
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	stopFirst := runIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
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
	waitFor(t, 20*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})
	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	if events := held.of("1"); !slices.Equal(events, []string{"start 1", "end 1"}) {
		t.Fatalf("the stopping intake's fetches of pull request 1 = %q, want the held one alone", events)
	}

	// The ack wait is minutes; the second process is handed the message within seconds.
	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// holdPullRequestOneUntilAbandoned is holdPullRequestOne, except that the held fetch also ends when
// the intake abandons it (the request's context ends), so its end is recorded as soon as the
// intake stops waiting on it, by a release or by giving the call up.
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
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	first := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	stopFirst := runIntake(t, first)
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before == 2 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("the second message of pull request 1 was not delivered behind the first: %+v", info)
	})

	// The standby is running before the stop, as the second envoy-dispatch process of a rolling
	// deploy is.
	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, 20*time.Second, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.delivering != nil && first.delivering.stopping.Load()
	}, func() string { return "the first intake never began stopping" })

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
	// The standby polls for the durable every durableBindInterval, so several of them give it the
	// time to bind and be handed whatever the stopping intake gave back.
	for deadline := time.Now().Add(4 * durableBindInterval); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if events := held.of("1"); overlapped(events) {
			held.release()
			t.Fatalf("pull request 1's events across the hand-over = %q: the standby started the pull request's next message "+
				"while the stopping intake's handler of the one before it was still waiting on GitHub", events)
		}
	}

	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q", events, want)
	}
}

// A stopping intake does not wait out a GitHub call that does not answer. Dispatch's whole
// shutdown has cmd/dispatch's stopGrace, 30 seconds (the pod's terminationGracePeriodSeconds less
// its pre-stop wait), and the intake's stop starts at the signal, so it returns well inside that
// however long GitHub holds the handler's call: here GitHub never answers.
func TestAStoppingIntakeDoesNotWaitOutAGitHubCallThatDoesNotAnswer(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	stop := runIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		held.release()
		t.Fatal("the stopping intake was still waiting on its handler's GitHub call 20 s after its context ended; Dispatch's shutdown has 30 s in all")
	}
}

// bindReturnsAfterSignal is how long after Run's ctx ends TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal
// holds the bind that has subscribed.
const bindReturnsAfterSignal = 2 * time.Second

// A bind that subscribed before Run's ctx ended and returns after it -- a standby's bind poll under
// way at the signal, whose JetStream calls do not watch ctx -- is stopped under the one stop budget,
// which runs from ctx's end: the handler of the message the subscription was handed is given up
// intakeStopBudget after the signal, however late the bind returned, and its message handed back,
// so the next process to bind the durable redoes it at once. With the budget begun at the bind's
// return, a late bind pushed the whole stop -- and cmd/dispatch's deregister and pool close behind
// it -- that much later.
func TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal(t *testing.T) {
	budget := intakeStopBudget
	intakeStopBudget = 3 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	subscribed, bindReturns := make(chan struct{}), make(chan struct{})
	intake.boundHook = func() {
		close(subscribed)
		<-bindReturns
	}
	stop := runIntake(t, intake)
	// After runIntake's cleanup, so it runs first: Run waits for the bind it holds.
	letBindReturn := sync.OnceFunc(func() { close(bindReturns) })
	t.Cleanup(letBindReturn)
	select {
	case <-subscribed:
	case <-time.After(20 * time.Second):
		t.Fatal("the intake never subscribed to its durable")
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)

	signalled := time.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	time.Sleep(bindReturnsAfterSignal)
	letBindReturn()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return 20 s after its context ended")
	}
	if waited, within := time.Since(signalled), intakeStopBudget+time.Second; waited > within {
		t.Fatalf("Run returned %s after its ctx ended, its bind %s after it; want within %s: the stop budget ran from the bind's return, not from ctx's end",
			waited.Round(time.Millisecond), bindReturnsAfterSignal, within)
	}
	// The fake records the end once it sees the request cancelled, a moment after the client did.
	waitFor(t, 5*time.Second, func() bool { return slices.Equal(held.of("1"), []string{"start 1", "end 1"}) }, func() string {
		return fmt.Sprintf("pull request 1's events once Run returned = %q, want the held fetch given up", held.of("1"))
	})

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process redoes the message given up", events, want)
	}
}

// A message NATS has delivered into the client's buffer, but the subscription's callback has not
// yet routed when the intake stops, is negatively acknowledged rather than handled, and the next
// process to bind the durable handles it.
func TestAMessageInTheClientBufferAtStopIsGivenBack(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	// This intake's first decode, the workflow run's in the callback, waits until unblock: the
	// messages behind it stay in the client's buffer meanwhile.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	var first sync.Once
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	// On this intake rather than on a package variable every intake reads, so the hold is on its
	// own first message and not on whichever intake of a test running beside this one decodes
	// first.
	intake.decode = func(data []byte) (contracts.Envelope, error) {
		hold := false
		first.Do(func() { hold = true })
		if hold {
			close(blocked)
			<-unblock
		}
		return decodeEnvelope(data)
	}
	stopFirst := runIntake(t, intake)
	release := sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	awaitBoundDurable(t, natsClient)

	// Another repository's run: whichever way the stop finds it, nothing is fetched for it.
	publishWorkflowEnvelope(t, natsClient, "other-org", "other-repo", ".github/workflows/deploy.yml", 9001)
	select {
	case <-blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the intake's callback never decoded the workflow run's envelope")
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 9, mergedPullRequest(9))
	intake.mu.Lock()
	delivery := intake.delivering
	intake.mu.Unlock()
	waitFor(t, 20*time.Second, func() bool {
		buffered, _, err := delivery.sub.Pending()
		return err == nil && buffered >= 2
	}, func() string { return "the pull request's message never reached the client's buffer" })

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, 20*time.Second, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its callback was free")
	}
	if events := held.of("9"); len(events) != 0 {
		t.Fatalf("the stopping intake fetched the buffered pull request: %q", events)
	}

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("9"), []string{"start 9", "end 9"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 9's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// A handler that finishes inside the stop of a live link has its message acknowledged there and
// then, not when the stop releases what it holds (intake_delivery.go's acknowledgeOrHold). Held
// to the end of the stop, that message keeps one of the next holder's four in-flight slots
// (intakeMaxAckPending) for every second the stop waits for the slowest handler -- during a
// rolling deploy, a slot of the very process this stop is handing the durable to.
//
// Two pull requests are held inside their fetches before the stop begins, so both are
// outstanding when it does, and the second is released while the stop still waits for the
// first: the durable's outstanding count has to fall to one there, with the stop still running.
// A stop that holds every acknowledgement of a finished handler leaves it at two until the
// first fetch is released too.
func TestAHandlerThatFinishesInsideTheStopIsAcknowledgedAtOnce(t *testing.T) {
	// The stop has to be waiting for pull request 1's handler still while the counts below are
	// read, and it is this row that releases that handler, so the budget has only to outlast
	// two reads of the durable on a loaded runner. Above githubapp.RequestTimeout (20 s) it
	// would buy nothing: the held fetch is given up by the GitHub client itself then, and the
	// stop would end on a handler this row had not released.
	budget := intakeStopBudget
	intakeStopBudget = 20 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })

	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake, "2")
	// A NATS server of this row's own rather than the package's shared one (intakeTestClient):
	// what the row reads is the durable's whole outstanding count, which on the shared server
	// counts whatever another row left behind as well.
	_, uri := testnats.Start(t)
	natsClient := reconnectTestClient(t, uri)
	stop := runIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	// Registered after runIntake, so it runs before the stop that cleanup makes: a stop begun
	// with both fetches still held waits out the whole budget before it gives them up.
	t.Cleanup(held.release)
	awaitBoundDurable(t, natsClient)

	// Two keys, so the two handlers run beside each other (intakeMaxAckPending is 4) rather than
	// one waiting behind the other.
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))
	held.awaitEntered(t)
	// 20 s, the bound every wait of this file on the durable's state carries: the server has
	// counted a message outstanding by the time its handler is inside its fetch, and this read
	// only has to see both.
	waitFor(t, 20*time.Second, func() bool { return deliveryAckPending(natsClient) == 2 }, func() string {
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
	waitFor(t, 20*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})
	held.releaseNumber("2")

	// 10 s rather than the 20 s above: the acknowledgement is published as the handler returns,
	// with no bind and no delivery to wait for, so this bound is for a loaded runner alone -- and
	// it has to leave the stop inside its budget, still waiting for pull request 1.
	waitFor(t, 10*time.Second, func() bool { return deliveryAckPending(natsClient) == 1 }, func() string {
		return fmt.Sprintf("the durable still reports %d message(s) outstanding while the stop waits for the other handler, want 1: the finished handler's acknowledgement was held for the stop rather than made where the handler finished",
			deliveryAckPending(natsClient))
	})
	select {
	case <-stopped:
		t.Fatal("the stop ended before pull request 1's fetch was released, so the count above was read of a stop that was waiting for nothing")
	default:
	}

	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once both handlers were released")
	}
}

// subscribe at a deliver subject it is given binds the durable created with that subject without
// looking it up, as a process taking the durable over after recreating it does; StopDelivering
// ends that subscription, and a second call returns at once.
func TestAnIntakeSubscribesAtTheDeliverSubjectItIsGiven(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	wanted := deliveryConsumerConfig()
	// Starting at new messages, so a durable recreated on the package's shared stream is handed
	// only this test's.
	wanted.DeliverPolicy = natsgo.DeliverNewPolicy
	if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("delete the delivery durable: %v", err)
	}
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	intake := newIntake(natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	if _, err := intake.subscribe(wanted.DeliverSubject); err != nil {
		t.Fatalf("subscribe at %s: %v", wanted.DeliverSubject, err)
	}
	t.Cleanup(func() { intake.StopDelivering(context.Background()) })

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Fatalf("settings after the handled message = %+v, %v: want last_event_at recorded", settings, err)
	}

	intake.StopDelivering(t.Context())
	intake.StopDelivering(t.Context())
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})
}

// A stopped intake stays stopped across a reconnect of its client: the reconnect does not bind its
// stopped subscription to the durable again, where its callback would hand every message straight
// back. A subscribe afterwards on the same client, at the deliver subject of the durable created
// again, as a process that lost and regained the durable does, binds it and handles its messages.
func TestAStoppedIntakeStaysStoppedAcrossAReconnectAndSubscribesAgain(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	recreate := func() natsgo.ConsumerConfig {
		t.Helper()
		wanted := deliveryConsumerConfig()
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
	intake := newIntake(natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	if _, err := intake.subscribe(recreate().DeliverSubject); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { intake.StopDelivering(context.Background()) })
	intake.StopDelivering(t.Context())
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})

	if err := natsClient.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the client's reconnect hooks never ran")
	}
	// The client restores its subscriptions before it runs the hooks, and nats-server applies a
	// subscriber's interest within moments, so a rebind would show by now.
	time.Sleep(time.Second)
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

// A stop after the client replaced its connection drains the subscription the intake bound again
// itself on the new connection: the connection event gave the subscription the client had
// restored up, and the bind poll took the durable afresh under its lease, so the handle the
// intake holds is its own and live rather than the one the closed connection took with it. The
// durable loses its subscriber as the stop begins, so a message published while a handler still
// runs is not handed to the stopping process, which would hand it back and be handed it again;
// the stop logs no failed drain, and the next process to bind the durable handles the message.
func TestAStopAfterAReplacedConnectionDrainsTheSubscriptionTheIntakeBoundAgain(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	stop := runIntake(t, intake)
	awaitBoundDurable(t, natsClient)
	// Read before the close, so the wait below can tell the delivery the intake binds afterwards
	// from the one the closed connection carried.
	intake.mu.Lock()
	replaced := intake.delivering
	intake.mu.Unlock()

	// The client dials a connection to replace the closed one and binds its registration there.
	natsClient.Conn.Close()
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the client's reconnect hooks never ran")
	}
	// The intake gives that restored subscription up on its connection event and binds the durable
	// again under its lease, so what it holds is a delivery of its own: a different object from the
	// one before the close, whose handle is live and whose durable the server reports bound. Until
	// all three hold, the stop below would be draining something other than what this row measures.
	var delivery *intakeDelivery
	waitFor(t, 30*time.Second, func() bool {
		intake.mu.Lock()
		delivery = intake.delivering
		intake.mu.Unlock()
		return delivery != replaced && delivery.sub.IsValid() && deliveryConsumerInfo(t, natsClient).PushBound
	}, func() string {
		return "the intake never gave the subscription the client restored up and bound the delivery durable again on its new connection"
	})

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	waitFor(t, 20*time.Second, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake's subscription stayed bound to the durable while its handler ran: the stop did not drain " +
			"the subscription the client restored on its new connection"
	})
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))
	// nats-server hands a bound consumer's subscriber a new message within moments.
	time.Sleep(time.Second)

	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer; delivered != before {
		t.Fatalf("the durable delivered %d message(s) to the stopping intake after its stop began, want none", delivered-before)
	}
	for _, record := range logs() {
		if message, _ := record["msg"].(string); strings.Contains(message, "drain") {
			t.Errorf("the stop logged %q (%v)", message, record["error"])
		}
	}

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
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
// envelope or a payload that does not decode, or a payload with no repo -- is wanted and keyed
// as it was keyed before the rule, since it cannot be judged.
func TestIntakeKeyWantsOnlyTheSubjectOfTheRepositoryThePayloadNames(t *testing.T) {
	const (
		prPayload       = `{"kind":"pr","action":"closed","merged":"true","repo":"cybermonklabs/legion","number":"42","author":"octocat"}`
		workflowPayload = `{"kind":"workflow","action":"completed","repo":"cybermonklabs/legion","run_id":"77"}`
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
		{name: "a push", subject: "notifications.github.cybermonklabs.legion.push.heads.main", payload: prPayload},
		{name: "a job", subject: "notifications.github.cybermonklabs.legion.job.500.completed", payload: prPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = githubEnvelopeData(t, tc.subject, tc.payload)
			}
			key, envelope, wanted := intakeKey(&natsgo.Msg{Subject: tc.subject, Data: data}, decodeEnvelope)
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
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)

	id := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
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
		if key, _, wanted := intakeKey(&natsgo.Msg{Subject: item.Topic, Data: data}, decodeEnvelope); wanted {
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
