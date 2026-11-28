package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestBothIntakesKeepTheirSubscriptions starts the two intakes as StartIntakes does (startIntakes,
// beside a client of its own that owns the stream and publishes) and holds that each connects under
// its own name, keeps its durable push-bound and handles what is published to it. A bus.Client
// keeps one JetStream subscription, so two intakes binding through one client leave whichever bound
// first with its durable unbound: its messages stay pending and its handler never runs.
//
// The intakes run by the row's tuning, which holds production's values (tuningFor, newIntakeTuning),
// and on a fake clock the row sets on it and moves (intakeTuning.clock, which startIntakes gives
// both intakes): the job intake reads a run's jobs once its burst goes quiet, jobQuiet on that clock
// after the event, and the row moves the clock there once the read is armed. StartIntakes runs
// them by production's tuning (newIntakeTuning), whose real clock
// TestProductionsIntakeTuningIsTheShippedTimersAndNoHook pins.
func TestBothIntakesKeepTheirSubscriptions(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newJobsFake(t)

	intakesClock := fakeIntakeClock()
	tuning := tuningFor(t)
	tuning.clock = intakesClock
	runCtx, cancel := context.WithCancel(ctx)
	var names []string
	var clients []*bus.Client
	stop, err := startIntakes(runCtx, tuning, func(name string) (*bus.Client, error) {
		names = append(names, name)
		client, err := bus.Connect([]string{natsURL}, bus.WithName(name))
		if err == nil {
			clients = append(clients, client)
		}
		return client, err
	}, pool, deliveryTestIntakeLeases(t, pool), fake.newTestClient(), events.NewBroker(), nil, nil, unrenamed())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	// The test ends only once both intakes have stopped, so nothing they started outlives the
	// test's own cleanups (startIntake's reason).
	t.Cleanup(func() {
		cancel()
		testwait.Returns(t, "the intakes' stop to return once ctx ended", stop)
	})
	if want := []string{"dispatch-intake", "dispatch-job-intake"}; !slices.Equal(names, want) {
		t.Fatalf("startIntakes connected as %q, want %q", names, want)
	}

	durables := []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable}
	consumers := func() string {
		var states []string
		for _, durable := range durables {
			info, err := publisher.JS().ConsumerInfo(bus.Stream, durable)
			if err != nil {
				states = append(states, fmt.Sprintf("%s: %v", durable, err))
				continue
			}
			states = append(states, fmt.Sprintf("%s: push_bound=%t pending=%d ack_pending=%d delivered=%d",
				durable, info.PushBound, info.NumPending, info.NumAckPending, info.Delivered.Consumer))
		}
		return strings.Join(states, "; ")
	}
	pushBound := func(durable string) bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}
	waitFor(t, func() bool {
		return pushBound(bus.DispatchDeliveryDurable) && pushBound(bus.DispatchJobDurable)
	}, func() string {
		return "the intakes' durables are not both push-bound: " + consumers()
	})
	// The row's tuning holds production's values, which the delivery durable the intake created
	// carries: an ack wait of maxAckPending message timeouts plus a minute, and that maxAckPending.
	delivery, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the delivery durable: %v", err)
	}
	if got, want := delivery.Config.AckWait, time.Duration(intakeMaxAckPending)*intakeMessageTimeout+time.Minute; got != want {
		t.Errorf("the delivery durable the intakes created waits %s for an acknowledgement, want %s: production's tuning", got, want)
	}
	if got := delivery.Config.MaxAckPending; got != intakeMaxAckPending {
		t.Errorf("the delivery durable the intakes created has max_ack_pending %d, want %d: production's tuning", got, intakeMaxAckPending)
	}
	// The server can read the two durables while one registration's unsubscribe is still on its way
	// to it, so both are read again once a round trip of each intake's client has answered: the
	// server handles one connection's protocol in order, so whatever either client sent before its
	// flush, an unsubscribe included, has reached it by then, and nothing binds an unbound durable
	// again.
	for i, client := range clients {
		if err := client.Conn.FlushWithContext(testnats.Context(t)); err != nil {
			t.Fatalf("flush the %s client: %v", names[i], err)
		}
	}
	for _, durable := range durables {
		if !pushBound(durable) {
			t.Fatalf("%s lost its subscription once both intakes had bound: %s", durable, consumers())
		}
	}

	// A pull request the intake handles records the event's time (deliver), and a job of a tracked
	// deploy run has the job intake read the run's jobs from GitHub once the burst goes quiet: the
	// read is armed jobQuiet past the event on the intakes' clock, which the row moves there.
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	publishGitHubEnvelope(t, publisher, "notifications.github.acme.widgets.job.500.completed", jobPayload("acme/widgets", 500))
	waitFor(t, func() bool {
		settings, err := GetSettings(ctx, pool)
		return err == nil && settings.LastEventAt != nil
	}, func() string {
		return "the intake never handled the pull request published to it: " + consumers()
	})
	quiet := intakesClock.Now().Add(jobQuiet)
	intakesClock.AwaitArmedAt(t, "the job intake's read of run 500, armed once its burst goes quiet", quiet)
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("the job intake read the run's jobs %d times before its burst went quiet", calls)
	}
	intakesClock.Set(quiet)
	waitFor(t, func() bool { return fake.calls.Load() == 1 }, func() string {
		return fmt.Sprintf("the job intake read the run's jobs %d times, want 1: %s", fake.calls.Load(), consumers())
	})
	for _, durable := range durables {
		var info *natsgo.ConsumerInfo
		waitFor(t, func() bool {
			info, err = publisher.JS().ConsumerInfo(bus.Stream, durable)
			return err == nil && info.NumPending == 0 && info.NumAckPending == 0 && info.Delivered.Consumer > 0
		}, func() string {
			return fmt.Sprintf("%s holds messages its intake never acknowledged: %s", durable, consumers())
		})
	}

	// stop waits for both intakes, which stop once ctx is cancelled, and closes both clients.
	cancel()
	testwait.Returns(t, "the intakes' stop to return once ctx ended", stop)
	for i, client := range clients {
		if !client.Conn.IsClosed() {
			t.Errorf("the %s client is open after stop", names[i])
		}
	}
}

// TestAMessageInItsHandlerAtShutdownIsAcknowledged: a merged pull request whose GitHub fetch is
// still running when ctx is cancelled is acknowledged once the fetch returns, because the intake
// stops delivering by draining its subscription and waits for the message in its handler before
// Run returns and stop closes the client. A client closed as Run returned took the acknowledgement
// down with it, and the message stayed outstanding until the ack wait handed it to the next
// process. The held fetch does not watch the request's context, so the client holds its calls to
// the test's give-up (newHoldingClient): at its own bound the client would give the held fetch up
// while the row waits for the durable to lose its subscriber, and the row would read a handler
// ended by that bound, not by its release.
func TestAMessageInItsHandlerAtShutdownIsAcknowledged(t *testing.T) {
	t.Parallel()
	// The row requires that no component logged a line about an acknowledgement: both intakes, the
	// duty leases and holds they take, and every NATS client take the test's logger, and nothing
	// else on the path (the store, GitHub's client) writes one.
	logs, logger := captureTestLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t, bus.WithLogger(logger))
	fake := newFakeGitHub(t)
	entered, held := make(chan struct{}), make(chan struct{})
	var enter sync.Once
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		enter.Do(func() { close(entered) })
		<-held
		w.WriteHeader(http.StatusInternalServerError)
	})
	github := fake.newHoldingClient()
	release := sync.OnceFunc(func() { close(held) })
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	t.Cleanup(release)

	runCtx, cancel := context.WithCancel(ctx)
	// startIntakes rather than StartIntakes, whose intakes log to the default: the test's tuning
	// holds production's values and clock and the test's logger.
	stop, err := startIntakes(runCtx, tuningFor(t), func(string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithLogger(logger))
	}, pool, deliveryTestIntakeLeases(t, pool), github, events.NewBroker(), nil, nil, unrenamed())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitBoundDurable(t, publisher)
	consumer := func() string {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("push_bound=%t delivered=%d ack_floor=%d ack_pending=%d redelivered=%d",
			info.PushBound, info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumRedelivered)
	}

	publishPullRequestEnvelope(t, publisher, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	testwait.Receive(t, "the intake to fetch the merged pull request", entered)
	cancel()
	// The intake stops delivering as ctx ends: the durable loses its subscriber at once, while the
	// message in its handler is still running.
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && !info.PushBound
	}, func() string {
		return "the intake's durable kept its subscriber after ctx was cancelled: " + consumer()
	})
	release()
	// deliver logs an acknowledgement that failed, and the negative acknowledgement after it.
	failedAcks := func() []map[string]any {
		var failed []map[string]any
		for _, record := range logs() {
			if message, _ := record["msg"].(string); strings.Contains(message, "acknowledge") {
				failed = append(failed, record)
			}
		}
		return failed
	}
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.Delivered.Consumer == 1 && info.AckFloor.Consumer == 1 && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the message in its handler at shutdown was never acknowledged: %s; failed acknowledgements: %v",
			consumer(), failedAcks())
	})
	stop()
	if failed := failedAcks(); len(failed) > 0 {
		t.Fatalf("an acknowledgement failed at shutdown: %v", failed)
	}
}

// TestAHandlerTheStopGivesUpIsRedeliveredAtOnce: the intakes' stop budget, counted from ctx's end,
// bounds how long the intakes' stop waits on a handler GitHub holds, and the message of the handler
// it gives up is handed back rather than acknowledged, so the next process to bind the durable
// redoes it within seconds instead of after the ack wait. The intakes run on a fake clock the row
// sets on their tuning and moves (intakeTuning.clock, which startIntakes gives both): each arms its
// stop deadline its tuning's stopBudget from ctx's end, and once the fake reaches them the stop
// gives the held handler up (its WARN line says so) and returns. The GitHub client holds its calls
// to the test's give-up (newHoldingClient), so only the stop gives the held fetch up.
func TestAHandlerTheStopGivesUpIsRedeliveredAtOnce(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	tuning := tuningFor(t)
	// A budget other than production's intakeStopBudget, so the deadlines read below are the
	// tuning's.
	tuning.stopBudget = time.Second
	// The message given back is what the next process must redo at once, so the server's own
	// redelivery at the ack wait never comes inside the run (ackWaitPastTheRun).
	ackWaitPastTheRun(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newHoldingClient()

	stopClock := fakeIntakeClock()
	tuning.clock = stopClock
	runCtx, cancel := context.WithCancel(ctx)
	stop, err := startIntakes(runCtx, tuning, func(string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL})
	}, pool, deliveryTestIntakeLeases(t, pool), github, events.NewBroker(), nil, nil, unrenamed())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		testwait.Returns(t, "the intakes' stop to return once ctx ended", stop)
	})
	// After the fake's server and the stop above, so it runs before both: a row that fails while
	// the handler is held has its stop find the fetch let go, rather than wait for it on stopClock,
	// which nothing moves once the row has ended.
	t.Cleanup(held.release)
	awaitBoundDurable(t, publisher)
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)

	signalled := stopClock.Now()
	cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	// One stop deadline per intake, each armed as ctx ended, the tuning's budget from then. The same
	// clock also holds the held handler's own deadline (Intake.deliver), six hours out
	// (ackWaitPastTheRun).
	stopClock.AwaitArmedCountAt(t, "the two intakes' stop deadlines, armed as ctx ended", signalled.Add(tuning.stopBudget), 2)
	stopClock.Advance(tuning.stopBudget)
	testwait.Receive(t, "the intakes' stop to return once the fake clock reached their stop deadlines", stopped)
	if !loggedMessage(logs(), "dispatch delivery: the intake stopped before the event's handling finished; handing it back") {
		t.Fatalf("the stop returned without saying it gave the held handler up; the messages logged were %q", loggedMessages(logs()))
	}
	// The fake records the end once it sees the request cancelled, a moment after the client did.
	waitFor(t, func() bool { return slices.Equal(held.of("1"), []string{"start 1", "end 1"}) }, func() string {
		return fmt.Sprintf("pull request 1's events once stop returned = %q, want the held fetch given up", held.of("1"))
	})

	runIntake(t, newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitAcknowledged(t, publisher)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process redoes the message given up", events, want)
	}
}

// TestIntakesWithoutTheAppConnectNothing: with no GitHub App neither intake reads anything, so
// StartIntakes opens no connection and its stop returns at once.
func TestIntakesWithoutTheAppConnectNothing(t *testing.T) {
	t.Parallel()
	stop, err := StartIntakes(t.Context(), func(string) (*bus.Client, error) {
		t.Error("connected a client for intakes that have no GitHub App")
		return nil, errors.New("no connection here")
	}, nil, IntakeLeases{}, nil, nil, nil, nil, unrenamed())
	if err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	testwait.Returns(t, "the stop of intakes with no GitHub App to return", stop)
}

// TestIntakesWhoseSecondConnectFailsStartNeither: the job intake's connect failing returns its
// error with neither intake started and the intake's client closed, so no durable is bound.
func TestIntakesWhoseSecondConnectFailsStartNeither(t *testing.T) {
	t.Parallel()
	// A real pool rather than none: the intakes' settings cache reads delivery_settings through
	// it, and their reservations of its lease pool are what they take their holds on.
	pool, _ := deliveryTestPool(t)
	publisher := intakeTestClient(t)
	natsURL := testnats.URL(t)
	var first *bus.Client
	refused := errors.New("refused")
	_, err := StartIntakes(t.Context(), func(string) (*bus.Client, error) {
		if first != nil {
			return nil, refused
		}
		var err error
		first, err = bus.Connect([]string{natsURL})
		return first, err
	}, pool, deliveryTestIntakeLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), nil, nil, unrenamed())
	if !errors.Is(err, refused) {
		t.Fatalf("start the intakes = %v, want the job intake's connect error", err)
	}
	if first == nil || !first.Conn.IsClosed() {
		t.Fatal("the intake's client was left open when the job intake's connect failed")
	}
	for _, durable := range []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable} {
		if _, err := publisher.JS().ConsumerInfo(bus.Stream, durable); !errors.Is(err, natsgo.ErrConsumerNotFound) {
			t.Errorf("%s after a failed start: %v, want no such durable", durable, err)
		}
	}
}

// TestTwoIntakesOnOneClientRefuseTheSecondBind: the intakes on one client, as cmd/dispatch wired
// them before StartIntakes, no longer lose a durable silently. The job intake binds first; the
// delivery intake's bind on the same client is refused with bus.ErrSubscriptionHeld and logged at
// ERROR, and the job intake's durable keeps its subscriber.
//
// The delivery intake's take-over tries the bind again until its bound, takeOverFinishWithin, and
// logs the refusal at ERROR as it gives up (retryHeld, failedStep). That bound and its waits between
// tries run on the intake's hold clock (heldDurable.holdClock), a fake the row moves to the bound
// once the take-over has armed it. Its stop's deadline is on its own clock (heldDurable.clock), a
// fake the row never moves, so the row reads its Run's context ended once that deadline is armed,
// and then moves the hold clock past the bound of the take-over the loop began again meanwhile,
// which the stop waits for.
func TestTwoIntakesOnOneClientRefuseTheSecondBind(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	shared := intakeTestClient(t)
	github := newFakeGitHub(t).newTestClient()
	leases := deliveryTestLeases(t, pool)
	pushBound := func(durable string) bool {
		info, err := shared.JS().ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}

	jobsCtx, stopJobs := context.WithCancel(ctx)
	jobsDone := make(chan struct{})
	jobs := newJobIntake(tuningFor(t), shared, pool, leases, github, clock.Real(), loadedSettingsCache(t, ctx, pool))
	go func() {
		defer close(jobsDone)
		jobs.Run(jobsCtx)
	}()
	t.Cleanup(func() {
		stopJobs()
		testwait.Receive(t, "the job intake's Run to return once its context ended", jobsDone)
	})
	waitFor(t, func() bool { return pushBound(bus.DispatchJobDurable) }, func() string {
		return "the job intake never bound its durable"
	})

	seedDeliverySettings(t, ctx, pool)
	holdClock, stopClock := fakeIntakeClock(), fakeIntakeClock()
	start := holdClock.Now()
	intake := newIntake(tuningFor(t), shared, pool, leases, github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	intake.clock, intake.holdClock = stopClock, holdClock
	stop := runIntake(t, intake)
	// Registered after runIntake's, so it runs first: the take-over the loop began again after the
	// one the row gave up holds Run until it gives up too.
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stop()
		}()
		stopClock.AwaitArmedAt(t, "the delivery intake's stop deadline, armed as its Run's context ended", stopClock.Now().Add(tuningFor(t).stopBudget))
		holdClock.Advance(takeOverFinishWithin)
		testwait.Receive(t, "the delivery intake's Run to return once its take-over gave up", stopped)
	})
	holdClock.AwaitArmedAt(t, "the delivery intake's take-over bound, takeOverFinishWithin past its start", start.Add(takeOverFinishWithin))
	holdClock.Set(start.Add(takeOverFinishWithin))
	// The move ends the take-over at its bound, and the loop begins it again at once (its tick came
	// due during the move), with a bound takeOverFinishWithin past then. The cleanup's move ends that
	// one, so it waits here for the bound to be armed: a move made before the take-over armed it would
	// push the bound past anything the cleanup reaches.
	holdClock.AwaitArmedAt(t, "the delivery intake's next take-over bound, takeOverFinishWithin past the first one's", start.Add(2*takeOverFinishWithin))
	var refusal map[string]any
	waitFor(t, func() bool {
		for _, record := range logs() {
			message, _ := record["error"].(string)
			if record["msg"] == "dispatch delivery: bind NATS consumer" && record["name"] == bus.DispatchDeliveryDurable &&
				strings.Contains(message, bus.ErrSubscriptionHeld.Error()) {
				refusal = record
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the delivery intake's bind on the job intake's client was not refused with %q; %s=%t %s=%t",
			bus.ErrSubscriptionHeld, bus.DispatchJobDurable, pushBound(bus.DispatchJobDurable), bus.DispatchDeliveryDurable, pushBound(bus.DispatchDeliveryDurable))
	})
	if refusal["level"] != "ERROR" {
		t.Fatalf("the refused bind was logged at %v, want ERROR: %v", refusal["level"], refusal)
	}
	if !pushBound(bus.DispatchJobDurable) {
		t.Fatal("the job intake's durable lost its subscriber to the refused bind")
	}
	if pushBound(bus.DispatchDeliveryDurable) {
		t.Fatal("the delivery intake's durable is bound on a client that holds the job intake's")
	}
}
