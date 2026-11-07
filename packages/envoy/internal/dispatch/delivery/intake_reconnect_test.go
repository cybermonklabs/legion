package delivery

// What a process that held the delivery durable does once its NATS connection comes back: the
// durable belongs to whichever process took it over while it was away, and the one that came back
// is never a second bound subscriber beside it (INFRA-1551, R1). One row drives each of the two
// paths a connection comes back by -- a reconnect in place, where nats.go re-sends the
// subscription itself, and a replaced connection, where the bus client binds its own registration
// again (internal/bus/recovery.go).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A process whose NATS connection drops and reconnects in place comes back to a durable the
// process standing by took over while it was away, and leaves it there: the one pull request
// envelope published afterwards is handled once, by the process that took the durable over, the
// server reports one subscriber on the durable's deliver subject, the returned process says it
// gave the durable up, and it holds no delivery subscription of its own.
//
// The drop is a cut link, restored while the holder's client is still dialling the same URL
// through it, so nats.go reconnects that client in place and re-sends its SUB inside its own
// reconnect, under no lease and with nothing asking whether the durable is still this process's
// to deliver (the bus leaves a subscription a reconnect in place kept valid alone,
// restoreSubscriptions). That is the first of the defect's two paths.
//
// The assertions are what they are because ConsumerInfo.PushBound is one bool for one subscriber
// and for two: it says somebody is bound, never how many deliver. So the row reads the fan-out
// instead -- one envelope, counted per process on a fake GitHub of that process's own -- and the
// server's own count of the subscribers on the deliver subject, and reads PushBound only to keep
// a run where nobody is bound from passing.
func TestAHolderBackFromAReconnectInPlaceLeavesTheDurableToTheProcessThatTookIt(t *testing.T) {
	holderBackFromAConnectionEvent(t, func(_ *bus.Client, proxy *testnats.Proxy, _ func()) {
		proxy.Cut()
	}, 30*time.Second)
}

// A process whose connection was replaced comes back to a durable another process took over, and
// leaves it there, exactly as the row above holds for a reconnect in place.
//
// This row drives the defect's second path: the link is cut, the process standing by takes the
// durable over, and only then is the holder's connection closed, so its client dials a
// replacement and binds its own registration again (internal/bus/recovery.go's
// restoreSubscriptions) rather than having nats.go re-send it. That redial backs off from one
// second, so the holder's client is given 60 s to be connected again once the link is back.
//
// The assertions are the row above's, and for the row above's reason: PushBound is one bool
// however many subscribers deliver, so what says the two processes did not both deliver is the
// fan-out of one envelope counted per process, with the server's own subscriber count on the
// deliver subject beside it.
func TestAHolderWhoseConnectionWasReplacedLeavesTheDurableToTheProcessThatTookIt(t *testing.T) {
	holderBackFromAConnectionEvent(t, func(holderClient *bus.Client, proxy *testnats.Proxy, awaitKeeper func()) {
		proxy.Cut()
		// The hand-over first: a connection replaced before the standby has the durable would be
		// dialled while the durable is free, which is not the state this row is about.
		awaitKeeper()
		holderClient.Connection().Close()
	}, 60*time.Second)
}

// holderBackFromAConnectionEvent runs the two processes both rows of this file hold to account:
// the holder, whose bus client reaches NATS through a proxy the row can take down, and the
// keeper, whose client reaches the server directly and which stands by for the durable until the
// holder is cut off. drop is the row's own way of taking the holder's connection away, and is
// handed awaitKeeper, the wait for the keeper to have taken the durable over, for a drop whose
// second step needs the hand-over to have happened. backWithin is how long the holder's client
// may take to be connected again once the link is restored.
//
// Each process gets a fake GitHub of its own, so one envelope's handling can be counted per
// process, and the whole measurement is that count: a durable two processes deliver answers
// ConsumerInfo exactly as one does.
func holderBackFromAConnectionEvent(t *testing.T, drop func(holderClient *bus.Client, proxy *testnats.Proxy, awaitKeeper func()), backWithin time.Duration) {
	logs := captureLogs(t)
	// A NATS server of this row's own rather than the package's shared one (testnats.URL): the
	// row reads the server's monitoring endpoint for the subscribers on one deliver subject, and
	// on the shared server that endpoint answers for every test running beside this one.
	server, uri := testnats.Start(t)
	proxy := testnats.StartProxy(t, uri)
	holderClient := reconnectTestClient(t, proxy.URL())
	keeperClient := reconnectTestClient(t, uri)

	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	holderFetches, holderGitHub := countingGitHub(t)
	keeperFetches, keeperGitHub := countingGitHub(t)
	holder := newIntake(holderClient, pool, deliveryTestLeases(t, pool), holderGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	keeper := newIntake(keeperClient, pool, deliveryTestLeases(t, pool), keeperGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))

	// The holder goes first and has the durable before the keeper starts, so the row knows whose
	// connection to drop. Its own subscription is waited for as well as the server's PushBound:
	// the bind is this process's only once both say so.
	runIntake(t, holder)
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(holder) && deliveryDurableBound(keeperClient)
	}, func() string {
		return fmt.Sprintf("the first process never took the delivery durable: its own subscription is live = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), deliveryDurableBound(keeperClient))
	})
	// The keeper stands by: the durable admits one subscriber, and its standby line is the proof
	// its bind poll is running before the holder is cut off.
	runIntake(t, keeper)
	waitFor(t, 30*time.Second, func() bool {
		return loggedMessage(logs(), standbyConsumerHeldMessage)
	}, func() string {
		return fmt.Sprintf("the second process never stood by for the durable the first holds (%q); the messages logged were %q",
			standbyConsumerHeldMessage, loggedMessages(logs()))
	})

	// A reconnect hook on the returned process's client, so the row can wait for the connection
	// to be back and for the client's own restore of its subscriptions to have run: the bus
	// restores them before it runs these hooks (internal/bus/recovery.go). Waiting on SubOK
	// instead would never return once the intake gives its registration up on the connection
	// event, which is exactly what this change makes it do.
	hooked := make(chan struct{}, 8)
	holderClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	awaitKeeper := func() { awaitDurableTakenOver(t, holder, keeper, keeperClient) }
	drop(holderClient, proxy, awaitKeeper)
	awaitKeeper()

	proxy.Restore()
	awaitHolderConnectedAgain(t, holderClient, hooked, backWithin)
	// A round trip on the returned process's own connection, so the server has processed
	// everything that connection sent on its way back -- the SUB nats.go re-sends inside its
	// reconnect among it -- before the envelope is published on the other process's connection.
	// Without it the row could publish into a server that had not yet read that SUB, and read a
	// fan-out that had not happened yet.
	if err := holderClient.Connection().FlushTimeout(5 * time.Second); err != nil {
		t.Logf("the returned process's connection did not answer a flush: %v", err)
	}

	publishPullRequestEnvelope(t, keeperClient, "acme", "widgets", 11, mergedPullRequest(11))
	waitFor(t, 30*time.Second, func() bool {
		return holderFetches.Load()+keeperFetches.Load() >= 1
	}, func() string {
		return fmt.Sprintf("neither process handled the pull request envelope: the returned process fetched it %d time(s), the process that took the durable over %d time(s), and the server reports the durable bound = %t",
			holderFetches.Load(), keeperFetches.Load(), deliveryDurableBound(keeperClient))
	})
	// The server hands every subscriber of a deliver subject the same message at the same moment,
	// so a second after the first handling is long enough for a second process's handling of that
	// one envelope to have landed and been counted.
	time.Sleep(time.Second)

	// The fan-out first, so it is measured and reported before anything else can stop the row:
	// it is the negative control of this whole change.
	holderHandled, keeperHandled := holderFetches.Load(), keeperFetches.Load()
	switch {
	case holderHandled == 1 && keeperHandled == 1:
		t.Errorf("both processes handled the one pull request envelope: the process whose connection came back fetched it %d time(s) and the process that took the durable over %d time(s), want 0 and 1",
			holderHandled, keeperHandled)
	case holderHandled != 0 || keeperHandled != 1:
		t.Errorf("the one pull request envelope was fetched %d time(s) by the process whose connection came back and %d time(s) by the process that took the durable over, want 0 and 1",
			holderHandled, keeperHandled)
	}

	deliverSubject := deliveryConsumerInfo(t, keeperClient).Config.DeliverSubject
	if subscribers := deliverSubjectSubscribers(t, server, deliverSubject); subscribers != 1 {
		t.Errorf("the server reports %d subscriber(s) on the durable's deliver subject %q, want 1", subscribers, deliverSubject)
	}

	if info := deliveryConsumerInfo(t, keeperClient); !info.PushBound {
		t.Errorf("the server reports the durable %s bound to no subscriber: delivered %d, ack floor %d, waiting %d",
			bus.DispatchDeliveryDurable, info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending)
	}

	// Only the process whose connection came back can log this: the other had no connection
	// event.
	if !loggedMessage(logs(), intakeConnectionChangedMessage) {
		t.Errorf("no process logged %q, so none gave the delivery durable up on its connection event; the messages logged were %q",
			intakeConnectionChangedMessage, loggedMessages(logs()))
	}

	holderDelivering, keeperDelivering := intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper)
	if holderDelivering || !keeperDelivering {
		t.Errorf("after the connection came back the process that had been away holds a live delivery subscription = %t and the process that took the durable over holds one = %t, want false and true",
			holderDelivering, keeperDelivering)
	}
}

// awaitDurableTakenOver waits for the process standing by to hold the delivery durable while the
// other process is cut off from NATS, and names what each of the two is when it never does. It is
// called twice in a row that closes the holder's connection after the hand-over, which costs one
// read: the wait returns at once once the keeper is delivering.
func awaitDurableTakenOver(t *testing.T, holder, keeper *Intake, keeperClient *bus.Client) {
	t.Helper()
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(keeper)
	}, func() string {
		return fmt.Sprintf("the standby process never took the delivery durable over while the holder was cut off: the holder's subscription is live = %t, the standby's = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper), deliveryDurableBound(keeperClient))
	})
}

// awaitHolderConnectedAgain waits for the client that was cut off to have come back: its
// reconnect hooks have run, which the bus runs after it has restored whatever subscriptions it
// still holds registrations for, and its connection reports live. Measuring the envelope before
// that restore has ended would read a fan-out that had not happened yet. The hook is what the
// wait reads, rather than the client holding a subscription, because a client whose registration
// the intake gave up comes back holding none.
func awaitHolderConnectedAgain(t *testing.T, holderClient *bus.Client, hooked <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-hooked:
	case <-time.After(within):
		t.Fatalf("the client cut off from NATS never ran its reconnect hooks in %s once the link was restored: connected = %t, holds a subscription = %t",
			within, holderClient.Connected(), holderClient.SubOK())
	}
	waitFor(t, 30*time.Second, holderClient.Connected, func() string {
		return "the client cut off from NATS ran its reconnect hooks and still reports no live connection"
	})
}

// intakeHoldsSubscription reports whether intake has a delivery subscription of its own that is
// still live: one its last subscribe installed, on a connection that still carries it. It is how
// a row tells which of two processes is delivering the durable, which the server's own
// ConsumerInfo cannot say.
func intakeHoldsSubscription(intake *Intake) bool {
	intake.mu.Lock()
	defer intake.mu.Unlock()
	return intake.delivering != nil && intake.delivering.sub.IsValid()
}

// deliveryDurableBound reports whether the server has a subscriber on the delivery durable, and
// false while the durable does not exist yet: a row reads it before the first intake has created
// it, where deliveryConsumerInfo would fail the test.
func deliveryDurableBound(natsClient *bus.Client) bool {
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	return err == nil && info.PushBound
}

// deliverSubjectSubscribers is how many subscribers the server itself reports on subject, read
// from its monitoring endpoint. ConsumerInfo.PushBound is one bool for one subscriber and for
// two, so the server's own subscription list is the only place a second subscriber on a durable's
// deliver subject can be counted.
func deliverSubjectSubscribers(t *testing.T, server *testnats.Server, subject string) int {
	t.Helper()
	endpoint := server.MonitorURL() + "/subsz?subs=1"
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s, want 200 OK", endpoint, response.Status)
	}
	var reported struct {
		Subscriptions []struct {
			Subject string `json:"subject"`
		} `json:"subscriptions_list"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reported); err != nil {
		t.Fatalf("decode %s: %v", endpoint, err)
	}
	subscribers := 0
	for _, subscription := range reported.Subscriptions {
		if subscription.Subject == subject {
			subscribers++
		}
	}
	return subscribers
}

// countingGitHub is a fake GitHub of one process's own, counting the pull request fetches that
// process makes and answering every one of them 500, which the intake logs and acknowledges
// (holdPullRequestOne). One per process is what lets a row say which process handled an envelope.
func countingGitHub(t *testing.T) (*atomic.Int64, *githubapp.Client) {
	t.Helper()
	fetches := &atomic.Int64{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return fetches, fake.newTestClient()
}

// reconnectTestClient connects one NATS client at uri for one process of a row of this file, and
// closes it with the test. intakeTestClient connects to the package's shared server, which no row
// here can cut a link to.
func reconnectTestClient(t *testing.T, uri string) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect NATS at %s: %v", uri, err)
	}
	t.Cleanup(natsClient.Close)
	return natsClient
}

// loggedMessage reports whether any captured record's msg is message.
func loggedMessage(records []map[string]any, message string) bool {
	for _, record := range records {
		if logged, _ := record["msg"].(string); logged == message {
			return true
		}
	}
	return false
}

// loggedMessages is each distinct msg the capture holds, in the order they were first logged:
// what a failure prints in place of the message it looked for and did not find.
func loggedMessages(records []map[string]any) []string {
	var messages []string
	for _, record := range records {
		logged, _ := record["msg"].(string)
		if logged != "" && !slices.Contains(messages, logged) {
			messages = append(messages, logged)
		}
	}
	return messages
}
