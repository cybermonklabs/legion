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
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestBothIntakesKeepTheirSubscriptions starts the two intakes as cmd/dispatch does (StartIntakes,
// beside a client of its own that owns the stream and publishes) and holds that each connects under
// its own name, keeps its durable push-bound and handles what is published to it. A bus.Client
// keeps one JetStream subscription, so two intakes binding through one client leave whichever bound
// first with its durable unbound: its messages stay pending and its handler never runs.
func TestBothIntakesKeepTheirSubscriptions(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newJobsFake(t)

	runCtx, cancel := context.WithCancel(ctx)
	var names []string
	var clients []*bus.Client
	stop, err := StartIntakes(runCtx, func(name string) (*bus.Client, error) {
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
	// test's own cleanups (runIntake's reason).
	t.Cleanup(func() {
		cancel()
		stop()
	})
	if want := []string{"dispatch-intake", "dispatch-job-intake"}; !slices.Equal(names, want) {
		t.Fatalf("StartIntakes connected as %q, want %q", names, want)
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
	// StartIntakes times both intakes by production's tuning (newIntakeTuning), which the delivery
	// durable it created carries: an ack wait of maxAckPending message timeouts plus a minute, and
	// that maxAckPending. A tuning of a test's, or none, gives the durable other values.
	delivery, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the delivery durable: %v", err)
	}
	if got, want := delivery.Config.AckWait, time.Duration(intakeMaxAckPending)*intakeMessageTimeout+time.Minute; got != want {
		t.Errorf("the delivery durable StartIntakes created waits %s for an acknowledgement, want %s: production's tuning", got, want)
	}
	if got := delivery.Config.MaxAckPending; got != intakeMaxAckPending {
		t.Errorf("the delivery durable StartIntakes created has max_ack_pending %d, want %d: production's tuning", got, intakeMaxAckPending)
	}

	// A pull request the intake handles records the event's time (deliver), and a job of a tracked
	// deploy run has the job intake read the run's jobs from GitHub once the burst goes quiet.
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
	// Both durables were handled through, so both kept their subscriptions while their messages
	// were delivered: a registration's unsubscribe still on its way to the server when they were
	// first read bound has landed by now, and nothing binds an unbound durable again.
	for _, durable := range durables {
		if !pushBound(durable) {
			t.Fatalf("%s lost its subscription once both intakes had bound: %s", durable, consumers())
		}
	}

	// stop waits for both intakes, which stop once ctx is cancelled, and closes both clients.
	cancel()
	stop()
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
// process.
func TestAMessageInItsHandlerAtShutdownIsAcknowledged(t *testing.T) {
	// The default logger, not the intake's: the row requires that no component logged a line about
	// an acknowledgement, the bus client among them, and StartIntakes' intakes log to the default.
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newFakeGitHub(t)
	entered, held := make(chan struct{}), make(chan struct{})
	var enter sync.Once
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		enter.Do(func() { close(entered) })
		<-held
		w.WriteHeader(http.StatusInternalServerError)
	})
	github := fake.newTestClient()
	release := sync.OnceFunc(func() { close(held) })
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	t.Cleanup(release)

	runCtx, cancel := context.WithCancel(ctx)
	stop, err := StartIntakes(runCtx, func(string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL})
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
// moves (startIntakes): each arms its stop deadline intakeStopBudget from ctx's end, and once the
// fake reaches it the stop gives the held handler up (its WARN line says so) and returns. Every
// intake of the row takes a message timeout past the run (its tuning, ackWaitPastTheRun), so
// only the hand-back brings the message to the next process inside the run, and the GitHub
// client's own bound is the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp), so only the
// stop gives the held fetch up.
func TestAHandlerTheStopGivesUpIsRedeliveredAtOnce(t *testing.T) {
	logs := captureLogs(t)
	tuning := tuningFor(t)
	tuning.messageTimeout = messageTimeoutPastTheRun
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient(githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))

	stopClock := clocktest.New(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	runCtx, cancel := context.WithCancel(ctx)
	stop, err := startIntakes(runCtx, tuning, func(string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL})
	}, pool, deliveryTestIntakeLeases(t, pool), github, events.NewBroker(), nil, nil, unrenamed(), stopClock)
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		testwait.Returns(t, "the intakes' stop to return once ctx ended", stop)
	})
	// The held fetch is let go as the row ends, before the stop above (registered after it, so run
	// first): a row that fails while the handler is held then has its stop find the handler
	// answered, rather than wait for it on stopClock, which nothing moves once the row has ended.
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
	// One stop deadline per intake, each armed as ctx ended. The same clock also holds the held
	// handler's own deadline (Intake.deliver) and, while it runs, the bound of the job intake's
	// release of its hold (releaseHold), which has no hand-back to wait for: the row waits until
	// two deadlines stand at intakeStopBudget from ctx's end.
	deadline := signalled.Add(intakeStopBudget)
	testwait.Until(t, fmt.Sprintf("the two intakes' stop deadlines, armed at %v: intakeStopBudget from ctx's end", deadline), func() ([]time.Time, bool) {
		armed := stopClock.Armed()
		at := 0
		for _, moment := range armed {
			if moment.Equal(deadline) {
				at++
			}
		}
		return armed, at == 2
	})
	stopClock.Advance(intakeStopBudget)
	testwait.Receive(t, "the intakes' stop to return once the fake clock reached their stop deadlines", stopped)
	if !loggedMessage(logs(), "dispatch delivery: the intake stopped before the event's handling finished; handing it back") {
		t.Fatalf("the stop returned without saying it gave the held handler up; the messages logged were %q", loggedMessages(logs()))
	}
	// The fake records the end once it sees the request cancelled, a moment after the client did.
	waitFor(t, func() bool { return slices.Equal(held.of("1"), []string{"start 1", "end 1"}) }, func() string {
		return fmt.Sprintf("pull request 1's events once stop returned = %q, want the held fetch given up", held.of("1"))
	})

	runIntake(t, ackWaitPastTheRun(newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())))
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
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	testwait.Receive(t, "the stop of intakes with no GitHub App to return", stopped)
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
	runIntake(t, newIntake(tuningFor(t), shared, pool, leases, github, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
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
