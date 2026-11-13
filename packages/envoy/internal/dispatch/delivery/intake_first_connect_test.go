package delivery

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// firstConnectNumber is the pull request the row below publishes once the first-connect callback
// has landed. The number is this file's own, so the row's fetch count is its own message's.
const firstConnectNumber = 7

// firstConnectWithin bounds the row's waits: the take-over's bind, and the first-connect callback
// running once the gate is open.
const firstConnectWithin = 20 * time.Second

// givingUpLines are the parts of the lines an intake writes when it stops delivering from a
// durable it bound: a lost hold, a durable delivering elsewhere (heldDurable.run, check), and a
// connection change, the line of an intake that gives the durable up on one.
var givingUpLines = []string{"NATS connection changed", "hold is lost", "stops delivering"}

// An intake whose client connects in the background keeps the durable it bound when the client's
// first-connect callback lands inside that bind. nats.go reports the connection CONNECTED as soon
// as its handshake ends, and runs ConnectedCB later, on its own callback goroutine; the bus's
// callback (connectInBackground, onConnect) then reports a connection event (ConnectionChanged).
// So an intake can see a live connection, bind its durable on it, and only then hear that the
// connection it bound on has connected. That event is the connection the binding was made on, not
// a new one, and the holder goes on delivering.
//
// The row makes that order happen on every run. The bus holds the first-connect callback
// (bus.HoldFirstConnectForTest); the intake starts while its client is CONNECTED and still waiting
// for the callback; the take-over's boundHook, which runs after the bind subscribed and created the
// durable, opens the gate and waits until the callback has run. Two bind intervals later the
// durable still delivers to the subject this intake bound, the hold is still this intake's, no line
// says it stopped delivering, and a message published after the callback is handled by it.
func TestAHolderKeepsTheDurableThroughAFirstConnectCallbackThatLandsInsideItsBind(t *testing.T) {
	logs := captureLogs(t)
	_, uri := testnats.Start(t)
	// A client that connects in the foreground creates the stream before the intake's client
	// exists: the intake's client ensures nothing until its held callback runs. It also reads the
	// durable and publishes for the row.
	observer := reconnectTestClient(t, uri)

	gate := make(chan struct{})
	t.Cleanup(bus.HoldFirstConnectForTest(gate))
	// The intake's client connects in the background, as every NATS client of envoy-dispatch does
	// (cmd/dispatch serverBus).
	client, err := bus.Connect([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect the intake's client in the background: %v", err)
	}
	t.Cleanup(client.Close)
	openGate := sync.OnceFunc(func() { close(gate) })
	// After the client's Close in registration, so it runs first: a row that fails before the
	// bind does not leave the callback held.
	t.Cleanup(openGate)
	if !client.Connected() || !client.AwaitingConnectionForTest() {
		t.Fatalf("the intake's client reports connected = %t and awaiting its first-connect callback = %t before the intake starts, want both true: the row needs a connection nats.go reports CONNECTED whose callback has not run",
			client.Connected(), client.AwaitingConnectionForTest())
	}

	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fetches, github := countingGitHub(t)
	intake := newIntake(client, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	type firstBind struct {
		subject string
		landed  bool
	}
	bound := make(chan firstBind, 1)
	intake.boundHook = func() {
		openGate()
		landed := testwait.Holds(firstConnectWithin, time.Millisecond, func() bool { return !client.AwaitingConnectionForTest() })
		// boundHook runs on the run loop after the take-over set the subject, so it reads it
		// there. Only the first bind is the row's.
		select {
		case bound <- firstBind{subject: intake.subject, landed: landed}:
		default:
		}
	}
	runIntake(t, intake)

	var first firstBind
	select {
	case first = <-bound:
	case <-time.After(firstConnectWithin):
		t.Fatalf("the intake never bound its durable within %s; the messages logged were %q", firstConnectWithin, loggedMessages(logs()))
	}
	if !first.landed {
		t.Fatalf("the first-connect callback did not run within %s of the gate opening inside the bind", firstConnectWithin)
	}
	boundAt, boundLogged := loggedAt(logs(), "dispatch delivery: NATS consumer bound")
	connectedAt, connectedLogged := loggedAt(logs(), "envoy nats connected")
	if !boundLogged || !connectedLogged || connectedAt.Before(boundAt) {
		t.Fatalf("bound logged at %s (%t), the client's connection at %s (%t); want the connection logged after the bind",
			boundAt.Format(time.RFC3339Nano), boundLogged, connectedAt.Format(time.RFC3339Nano), connectedLogged)
	}
	t.Logf("bound at %s, the client's first-connect callback logged at %s, %s later",
		boundAt.Format(time.RFC3339Nano), connectedAt.Format(time.RFC3339Nano), connectedAt.Sub(boundAt))
	// keptTheDurable fails the row on any line saying the intake stopped delivering, naming how
	// long after the bind it was logged.
	keptTheDurable := func(when string) {
		t.Helper()
		for _, record := range logs() {
			message, _ := record["msg"].(string)
			for _, line := range givingUpLines {
				if !strings.Contains(message, line) {
					continue
				}
				stamp, _ := record["time"].(string)
				at, _ := time.Parse(time.RFC3339Nano, stamp)
				t.Fatalf("%s the intake had logged %q at %s, %s after it logged the bind at %s; want it to keep the durable through a first-connect callback that landed inside its bind",
					when, message, stamp, at.Sub(boundAt), boundAt.Format(time.RFC3339Nano))
			}
		}
	}

	time.Sleep(2*durableBindInterval + 500*time.Millisecond)
	keptTheDurable("two bind intervals after the callback")

	info := deliveryConsumerInfo(t, observer)
	if !info.PushBound || info.Config.DeliverSubject != first.subject {
		t.Fatalf("two bind intervals after the callback the durable is push-bound = %t on %q, want bound on %q, the subject this intake bound; the messages logged were %q",
			info.PushBound, info.Config.DeliverSubject, first.subject, loggedMessages(logs()))
	}
	var holder, token string
	var live bool
	if err := pool.QueryRow(ctx, "select holder::text, token, expires_at > now() from duty_holds where name = $1",
		intakeHoldName(bus.DispatchDeliveryDurable)).Scan(&holder, &token, &live); err != nil {
		t.Fatalf("read the durable's hold: %v", err)
	}
	if holder != intake.holder.String() || !live || parseIntakeHoldToken(token).subject != first.subject {
		t.Fatalf("the durable's hold names holder %s, live = %t, token %q; want this intake's %s, live, recording %q",
			holder, live, token, intake.holder, first.subject)
	}

	publishPullRequestEnvelope(t, observer, "acme", "widgets", firstConnectNumber, mergedPullRequest(firstConnectNumber))
	waitFor(t, firstConnectWithin, func() bool { return fetches.Load() >= 1 }, func() string {
		return fmt.Sprintf("the intake never handled pull request %d published after the callback; the messages logged were %q", firstConnectNumber, loggedMessages(logs()))
	})
	if !intakeHoldsSubscription(intake) {
		t.Fatal("the intake handled the message and holds no live delivery subscription")
	}
	keptTheDurable("once the message published after the callback was handled")
}

// loggedAt is the time of the first captured record whose msg is message, and whether there is one.
func loggedAt(records []map[string]any, message string) (time.Time, bool) {
	for _, record := range records {
		if logged, _ := record["msg"].(string); logged != message {
			continue
		}
		stamp, _ := record["time"].(string)
		at, err := time.Parse(time.RFC3339Nano, stamp)
		return at, err == nil
	}
	return time.Time{}, false
}
