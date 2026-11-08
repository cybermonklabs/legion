package delivery

// What a process that held the delivery durable does once its NATS connection comes back: the
// durable belongs to whichever process took it over while it was away, and the one that came back
// is never a second bound subscriber beside it (INFRA-1551, R1). One row drives each of the two
// paths a connection comes back by -- a reconnect in place, where nats.go re-sends the
// subscription itself, and a replaced connection, where the bus client binds its own registration
// again (internal/bus/recovery.go).
//
// What moves the durable is the hold, and nothing NATS knows (heldDurable): a holder renews its
// hold on Postgres, so a link that drops under it takes nothing away, and a standby takes the
// durable over only once that hold has lapsed. So each row here cuts its holder off both -- the
// link the row is about, and the lease pool the hold is renewed on, which is what a kernel that
// stops scheduling the process or a network that takes it off the cluster cuts together. The
// take-over mints a fresh deliver subject and recreates the durable on it inside the consumer
// lease, so a subscription the returned process's client sends again, by either path, is a
// subscription to a subject the durable no longer delivers to.
//
// A third row holds the take-over to account for the work the holder had in flight (R2): the
// message in the handler the lost hold abandons reaches the process that keeps the durable, and
// inside a bound well under the ack wait that message would otherwise have waited out in that
// process's window. What carries it there is the take-over's recreate at the durable's ack floor
// (takeOverConfig), not a hand-back the dead link could not publish.
//
// A fourth and a fifth row hold the other half of that accounting, one on each of the two paths
// a connection comes back by: a handler that finishes while the link is down has its own
// acknowledgement refused, so the keeper is handed that message too and redoes the work. What
// those two measure is that the redo costs a person nothing: the pull request's row, its
// issue.deployed event and the issue's events are each written exactly once, because every write
// of this handler is an upsert and the one write a person reads is guarded inside its own
// transaction.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// intakeHoldLostMessage is the line an intake writes when the durable's hold is no longer its own
// and it stops delivering (heldDurable.run). Written out here rather than read from a constant
// because held_durable.go logs it as a literal, and a row that reads the log reads what an
// operator reads.
const intakeHoldLostMessage = "dispatch delivery: the intake's hold is lost; it stops delivering"

// A process whose NATS connection drops and reconnects in place comes back to a durable the
// process standing by took over while it was away, and leaves it there: the one pull request
// envelope published afterwards is handled once, by the process that took the durable over, the
// server reports one subscriber on the durable's deliver subject, the returned process says its
// hold was lost, and it holds no delivery subscription of its own.
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
	holderBackFromALostHold(t, func(*connectionEventProcesses) {}, 30*time.Second)
}

// A process whose connection was replaced comes back to a durable another process took over, and
// leaves it there, exactly as the row above holds for a reconnect in place.
//
// This row drives the defect's second path: the holder is cut off, the process standing by takes
// the durable over once the hold lapses, and only then is the holder's connection closed, so its
// client dials a replacement and binds its own registration again (internal/bus/recovery.go's
// restoreSubscriptions) rather than having nats.go re-send it. That redial backs off from one
// second, so the holder's client is given 60 s to be connected again once the link is back.
//
// The assertions are the row above's, and for the row above's reason: PushBound is one bool
// however many subscribers deliver, so what says the two processes did not both deliver is the
// fan-out of one envelope counted per process, with the server's own subscriber count on the
// deliver subject beside it.
func TestAHolderWhoseConnectionWasReplacedLeavesTheDurableToTheProcessThatTookIt(t *testing.T) {
	holderBackFromALostHold(t, func(processes *connectionEventProcesses) {
		// The hand-over first: a connection replaced before the standby has the durable would be
		// dialled while the durable is free, which is not the state this row is about.
		processes.holderClient.Connection().Close()
	}, 60*time.Second)
}

// A message the process holding the delivery durable had in flight, inside its handler, when the
// durable was taken away from it is handled by the process that keeps the durable, and soon
// (INFRA-1551, R2). The hold lapses under the cut-off holder, which stops delivering and gives
// its handler intakeHoldMargin to finish before it abandons it (heldDurable.run,
// stopDeliveringWithin); the message is left unacknowledged, since a hand-back is a publish and
// the link is down; and the take-over recreates the durable at its ack floor (takeOverConfig),
// which hands the keeper that message at once.
//
// What A2 is read as here: JetStream is at-least-once, and every write this package makes is an
// upsert the reconcile makes too, so a redelivery is a no-op rather than a duplicate (intake.go's
// package comment). What this row decides is therefore one handling by the keeper within the
// bound it states, and it says nothing about how many times NATS delivered the message.
//
// The bound is what makes the row a proof rather than a restatement of what NATS does anyway. A
// message the take-over did not carry over is redelivered when intakeAckWait() expires --
// intakeMaxAckPending (4) times intakeMessageTimeout (1 min) plus a minute, five minutes here --
// and it holds one of the keeper's four in-flight slots for every second of it, which is longer
// than the five-minute reconcile this intake exists to beat. A handling well inside that wait is
// the take-over's.
//
// R2 is this durable's alone: the job intake acknowledges each message as it receives it (jobs.go's
// deliver), so no handler of its own ever holds a message its stop must hand back.
func TestAMessageInFlightWhenTheDurableIsGivenUpReachesTheProcessThatKeepsIt(t *testing.T) {
	processes := startConnectionEventProcesses(t, holderHoldsItsFirstFetch)
	// Registered after the intakes are started, so it runs before their stops do, and here
	// rather than at the end of the row, so a row that fails leaves no request held.
	t.Cleanup(processes.holderHeld.release)

	// One envelope while the holder is the only process delivering, and nothing done until its
	// handler is inside its GitHub fetch: the message is in flight, in a handler, when the
	// durable is taken away below.
	publishPullRequestEnvelope(t, processes.keeperClient, "acme", "widgets", 1, mergedPullRequest(1))
	processes.holderHeld.awaitEntered(t)

	// The cut takes the holder off NATS and off the lease pool its hold is renewed on, with its
	// handler still in that fetch: the hold lapses, the holder stops delivering, and the standby
	// takes the durable over.
	processes.cutHolderOff()
	processes.awaitKeeper(t)
	processes.restoreHolder()
	awaitHolderConnectedAgain(t, processes.holderClient, processes.hooked, 60*time.Second)

	// 60 s: the hold's lapse (intakeHoldTTL, 15 s), the standby's poll (durableBindInterval, 2 s)
	// and the take-over's delete, subscribe and create under the consumer lease -- and a fifth of
	// the five-minute ack wait the row cannot change. A keeper that fetches the pull request
	// inside it was handed the message by the take-over; one that has not by then is waiting
	// those five minutes out with one of its four in-flight slots held.
	waitFor(t, 60*time.Second, func() bool {
		return processes.keeperFetches.Load() >= 1
	}, func() string {
		return fmt.Sprintf("the process that keeps the durable never handled the message the other had in flight: it fetched the pull request %d time(s), the process that lost the durable %d time(s), and the server reports the durable bound = %t",
			processes.keeperFetches.Load(), processes.holderFetches.Load(), deliveryDurableBound(processes.keeperClient))
	})
	// A second of quiet before the count is read, so a second handling of that one message --
	// which the server hands a bound subscriber within moments of deciding to -- is counted
	// rather than missed.
	time.Sleep(time.Second)
	if keeperHandled := processes.keeperFetches.Load(); keeperHandled != 1 {
		t.Errorf("the process that keeps the durable fetched the pull request %d time(s), want 1: the message the other had in flight is handled once there",
			keeperHandled)
	}
	// The holder's one fetch is the attempt its lost hold abandoned, so the row says plainly that
	// the work was left there and redone by the keeper rather than finished by both.
	if holderHandled := processes.holderFetches.Load(); holderHandled != 1 {
		t.Errorf("the process that lost the durable fetched the pull request %d time(s), want the one attempt the lost hold abandoned",
			holderHandled)
	}
}

// handlerFinishedNumber is the pull request the two rows below publish, and handlerFinishedMerged
// when GitHub says it was merged. The number is this file's own, so a row reads its row and its
// events rather than another row's.
const (
	handlerFinishedNumber = 777
	handlerFinishedMerged = "2024-01-01T02:00:00Z"
)

// writingGitHub is a fake GitHub of one process's own that answers acme/widgets'
// handlerFinishedNumber whole -- merged, labelled, with one commit -- so the handler reading it
// writes the pull request's row and, with a shipping deploy stored, its issue.deployed event.
// The counting fakes of this file's other rows answer 500, which writes nothing and so says
// nothing about what a person sees. It counts every fetch of that pull request and, when hold is
// set, holds the first one until the row releases it or the handler making it is abandoned.
func writingGitHub(t *testing.T, hold bool) (fetches *atomic.Int64, release func(), github *githubapp.Client) {
	t.Helper()
	fetches = &atomic.Int64{}
	held := make(chan struct{})
	release = sync.OnceFunc(func() { close(held) })
	var first sync.Once
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("parse pull request number %q: %v", r.PathValue("number"), err)
		}
		fetches.Add(1)
		holding := false
		if hold {
			first.Do(func() { holding = true })
		}
		if holding {
			select {
			case <-held:
			case <-r.Context().Done():
			}
		}
		mustEncode(t, w, map[string]any{
			"number": number, "title": deployedIssue + ": a widget",
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			"user":     map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": handlerFinishedMerged,
			"merge_commit_sha": fmt.Sprintf("abc%d", number), "additions": 10, "deletions": 2, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{
			"message": "feat: a widget", "author": map[string]any{"date": "2024-01-01T00:00:00Z"},
		}}})
	})
	return fetches, release, fake.newTestClient()
}

// A handler that finishes while its process is cut off from NATS has its work redone by the
// process that took the durable over, and a person sees that work once (INFRA-1551, R2). The
// handler's own acknowledgement is a publish and the bus buffers none while the client reconnects
// (opts.ReconnectBufSize = -1, internal/bus/connect.go), so a handling that finishes inside the
// outage is never acknowledged; the hold lapses, the standby takes the durable over, and its
// take-over recreates the durable at its ack floor (takeOverConfig), which hands that message to
// the keeper. The keeper fetches the pull request again and writes it again.
//
// That redo is what the hold gives instead of an acknowledgement held for the link's return, and
// it is sound because every write the handler makes is an upsert the five-minute reconcile makes
// too (intake.go's package comment) and the one write a person reads is written at most once: the
// issue.deployed event is guarded by `deployed_event_at is null` inside its own transaction
// (deployed.go's recordDeployedEvent), and this intake makes no GitHub write at all. So the row
// measures both halves: the keeper redoes the work, and the pull request's row, its
// issue.deployed event and the issue's events are each exactly one. A second event of any kind
// here is a person reading the same deploy twice, and fails the row.
//
// It also reads the durable's own outstanding count, 0: the redo was acknowledged, so nothing
// waits out the five-minute ack wait in the next holder's four-slot window.
func TestAHandlerThatFinishedDuringTheOutageIsRedoneByTheKeeperAndShowsOnce(t *testing.T) {
	handlerFinishedDuringTheOutage(t, false)
}

// A handler that finishes once its client has closed the connection its message arrived on and
// dialled a replacement is the row above's measurement on the second of the two paths a
// connection comes back by. Neither path acknowledges that handling -- the message's own
// subscription is gone with the hold -- so both end in the keeper's redo, and in one row, one
// event and one thing a person reads.
func TestAHandlerThatFinishedWhenTheConnectionWasReplacedIsRedoneByTheKeeperAndShowsOnce(t *testing.T) {
	handlerFinishedDuringTheOutage(t, true)
}

// handlerFinishedDuringTheOutage runs the two rows above: the holder is inside its fetch when the
// outage begins, finishes it during the outage, loses the durable with the hold, and the keeper is
// handed the message by the take-over. replaceConnection closes the holder's connection once the
// keeper has the durable, so its client dials a replacement rather than reconnecting in place.
func handlerFinishedDuringTheOutage(t *testing.T, replaceConnection bool) {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	merged, err := time.Parse(time.RFC3339, handlerFinishedMerged)
	if err != nil {
		t.Fatalf("parse the merge time: %v", err)
	}
	// A production deploy created after the merge, which is what makes the merged pull request
	// shipped and so gives it an issue.deployed event to write: without it the row would read
	// zero events whatever happened, and measure nothing.
	seedDeployRun(t, ctx, pool, 9777, merged.Add(time.Hour), merged.Add(70*time.Minute), DeliveryJobConclusionSuccess)

	_, uri := testnats.Start(t)
	proxy := testnats.StartProxy(t, uri)
	holderClient := reconnectTestClient(t, proxy.URL())
	keeperClient := reconnectTestClient(t, uri)

	holderFetches, releaseHolder, holderGitHub := writingGitHub(t, true)
	keeperFetches, _, keeperGitHub := writingGitHub(t, false)
	holderLeases := &cuttableLeases{pool: deliveryTestLeases(t, pool)}
	holder := newIntake(holderClient, pool, holderLeases, holderGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	keeper := newIntake(keeperClient, pool, deliveryTestLeases(t, pool), keeperGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))

	runIntake(t, holder)
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(holder) && deliveryDurableBound(keeperClient)
	}, func() string {
		return fmt.Sprintf("the first process never took the delivery durable: its own subscription is live = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), deliveryDurableBound(keeperClient))
	})
	runIntake(t, keeper)
	// Registered after the intakes are started, so it runs before their stops do: a row that
	// fails before its own release leaves no request held.
	t.Cleanup(releaseHolder)

	publishPullRequestEnvelope(t, keeperClient, "acme", "widgets", handlerFinishedNumber, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(handlerFinishedNumber), "author": "octocat",
	})
	waitFor(t, 30*time.Second, func() bool { return holderFetches.Load() >= 1 }, func() string {
		return "the holder's handler never reached its fetch of the pull request"
	})

	// The outage takes the link and the lease pool the hold is renewed on together, as a network
	// that drops the process out of the cluster does; the handler is released at once, so it
	// finishes inside the outage and its own acknowledgement is refused.
	proxy.Cut()
	holderLeases.cut.Store(true)
	releaseHolder()
	waitFor(t, 30*time.Second, func() bool { return storedPullRequests(t, ctx, pool, handlerFinishedNumber) == 1 }, func() string {
		return "the holder's handler never finished writing the pull request's row during the outage"
	})

	waitFor(t, 45*time.Second, func() bool { return intakeHoldsSubscription(keeper) }, func() string {
		return fmt.Sprintf("the standby never took the delivery durable over while the holder was cut off: the holder's subscription is live = %t, the standby's = %t",
			intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper))
	})
	if replaceConnection {
		holderClient.Connection().Close()
	}
	holderLeases.cut.Store(false)
	proxy.Restore()

	// The redo, and the whole of it: the keeper is handed the message by the take-over, fetches
	// the pull request and acknowledges it, which is what leaves the durable with nothing
	// outstanding. 60 s is a fifth of the five-minute ack wait this row cannot change, so a
	// durable still holding the message at the end is one nothing handed over.
	waitFor(t, 60*time.Second, func() bool {
		return keeperFetches.Load() >= 1 && deliveryAckPending(keeperClient) == 0
	}, func() string {
		return fmt.Sprintf("the process that keeps the durable fetched the pull request %d time(s) and the durable has %d message(s) outstanding, want the keeper's redo and nothing left outstanding (the holder fetched it %d time(s))",
			keeperFetches.Load(), deliveryAckPending(keeperClient), holderFetches.Load())
	})
	// A second of quiet, so a second handling of that one message -- which the server hands a
	// bound subscriber within moments of deciding to -- is counted rather than missed.
	time.Sleep(time.Second)

	if holderHandled := holderFetches.Load(); holderHandled != 1 {
		t.Errorf("the process cut off fetched the pull request %d time(s), want the one handling it finished during the outage", holderHandled)
	}
	if keeperHandled := keeperFetches.Load(); keeperHandled != 1 {
		t.Errorf("the process that keeps the durable fetched the pull request %d time(s), want the one redo the take-over hands it: its acknowledgement was refused over the dead link, so the message is the next holder's",
			keeperHandled)
	}
	if rows := storedPullRequests(t, ctx, pool, handlerFinishedNumber); rows != 1 {
		t.Errorf("acme/widgets#%d has %d rows after both processes handled it, want 1: every write of this handler is an upsert", handlerFinishedNumber, rows)
	}
	// What a person reads. The issue.deployed event is the only thing this intake writes that
	// anybody is told about, and the issue carries no other event here, so one of each is the
	// whole of "the deploy shows once".
	if deployed := deployedEvents(t, ctx, pool, deployedIssue); len(deployed) != 1 {
		t.Errorf("%s carries %d issue.deployed events after the handling was redone, want 1: a person reads the deploy once however many processes handled its message",
			deployedIssue, len(deployed))
	}
	var allEvents int
	if err := pool.QueryRow(ctx, `select count(*) from events where issue_key = $1`, deployedIssue).Scan(&allEvents); err != nil {
		t.Fatalf("count the events of %s: %v", deployedIssue, err)
	}
	if allEvents != 1 {
		t.Errorf("%s carries %d events after the handling was redone, want the one issue.deployed: nothing else reaches a person twice either", deployedIssue, allEvents)
	}
	if marker := deployedEventAt(t, ctx, pool, "acme/widgets", handlerFinishedNumber); marker == nil {
		t.Errorf("acme/widgets#%d has no deployed_event_at, so the guard that writes the event once was never set", handlerFinishedNumber)
	}
}

// storedPullRequests is how many rows acme/widgets#number has.
func storedPullRequests(t *testing.T, ctx context.Context, pool *store.Pool, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where repo = 'acme/widgets' and number = $1`, number).Scan(&rows); err != nil {
		t.Fatalf("count the rows of acme/widgets#%d: %v", number, err)
	}
	return rows
}

// holderBackFromALostHold runs the sequence the first two rows of this file share, on the
// processes startConnectionEventProcesses leaves delivering and standing by: the holder is cut off
// NATS and Postgres, the standby takes the durable over once the hold lapses, and the holder is
// let back. afterTakeOver is the row's own step between the hand-over and the restore, for a row
// whose connection is replaced rather than reconnected in place. backWithin is how long the
// holder's client may take to be connected again once the link is restored.
func holderBackFromALostHold(t *testing.T, afterTakeOver func(*connectionEventProcesses), backWithin time.Duration) {
	t.Helper()
	processes := startConnectionEventProcesses(t, holderCountsItsFetches)
	server, logs := processes.server, processes.logs
	holder, keeper := processes.holder, processes.keeper
	holderClient, keeperClient := processes.holderClient, processes.keeperClient
	holderFetches, keeperFetches := processes.holderFetches, processes.keeperFetches

	processes.cutHolderOff()
	processes.awaitKeeper(t)
	afterTakeOver(processes)

	processes.restoreHolder()
	awaitHolderConnectedAgain(t, holderClient, processes.hooked, backWithin)
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

	// Only the process that was cut off can log this: the other renewed its hold throughout.
	if !loggedMessage(logs(), intakeHoldLostMessage) {
		t.Errorf("no process logged %q, so none gave the delivery durable up when its hold lapsed; the messages logged were %q",
			intakeHoldLostMessage, loggedMessages(logs()))
	}

	holderDelivering, keeperDelivering := intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper)
	if holderDelivering || !keeperDelivering {
		t.Errorf("after the connection came back the process that had been away holds a live delivery subscription = %t and the process that took the durable over holds one = %t, want false and true",
			holderDelivering, keeperDelivering)
	}
}

// connectionEventProcesses is the two processes every row of this file holds to account, set up
// and running: the holder, whose bus client reaches NATS through a proxy the row can take down
// and whose hold is renewed on a lease pool the row can cut with it, and the keeper, whose client
// reaches the server directly and which stands by for the durable until the holder's hold lapses.
//
// Each process gets a fake GitHub of its own, so one envelope's handling can be counted per
// process, and that count is the whole measurement of every row here: a durable two processes
// deliver answers ConsumerInfo exactly as one does.
type connectionEventProcesses struct {
	// server is the row's own NATS server, whose monitoring endpoint answers for this row's
	// subscribers alone (deliverSubjectSubscribers).
	server *testnats.Server
	// proxy carries the holder's connection and no other, so a row takes that one process off
	// NATS by cutting it.
	proxy *testnats.Proxy
	// holderLeases is the pool the holder's hold is taken and renewed on, cut beside the link: a
	// hold renewed through an outage keeps the durable, which is the point of holding it in
	// Postgres, so only a process cut off both loses it.
	holderLeases *cuttableLeases
	holderClient *bus.Client
	keeperClient *bus.Client
	holder       *Intake
	keeper       *Intake
	// holderFetches and keeperFetches are the pull request fetches each process has made.
	holderFetches fetchCount
	keeperFetches fetchCount
	// holderHeld is the hold on the holder's first fetch of pull request 1, and is nil unless the
	// row asked for a holding fake (holderHoldsItsFirstFetch).
	holderHeld *heldPullRequests
	// hooked takes one value per reconnect of the holder's client (awaitHolderConnectedAgain).
	hooked <-chan struct{}
	// logs is every record logged since before either process started.
	logs func() []map[string]any
}

// holderGitHubFake is what the holder's fake GitHub does with a pull request fetch: count it,
// which is all a row measuring one envelope's fan-out needs, or count it and hold the first fetch
// of pull request 1, for a row that needs a handler of the holder's still inside its fetch when
// the durable is taken away.
type holderGitHubFake int

const (
	holderCountsItsFetches holderGitHubFake = iota
	holderHoldsItsFirstFetch
)

// startConnectionEventProcesses sets the two processes up and leaves them where every row of this
// file begins: the holder delivering from the delivery durable under its hold, the keeper standing
// by for it, a reconnect hook on the holder's client, and the logs captured from before either
// process started. holderFake is what the holder's fake GitHub does with a fetch.
func startConnectionEventProcesses(t *testing.T, holderFake holderGitHubFake) *connectionEventProcesses {
	t.Helper()
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
	holderFetches, holderHeld, holderGitHub := holderFakeGitHub(t, holderFake)
	keeperFetches, keeperGitHub := countingGitHub(t)
	holderLeases := &cuttableLeases{pool: deliveryTestLeases(t, pool)}
	holder := newIntake(holderClient, pool, holderLeases, holderGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	keeper := newIntake(keeperClient, pool, deliveryTestLeases(t, pool), keeperGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))

	// The holder goes first and has the durable before the keeper starts, so the row knows whose
	// connection to drop. Its own subscription is waited for as well as the server's PushBound:
	// the durable is this process's only once both say so.
	runIntake(t, holder)
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(holder) && deliveryDurableBound(keeperClient)
	}, func() string {
		return fmt.Sprintf("the first process never took the delivery durable: its own subscription is live = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), deliveryDurableBound(keeperClient))
	})
	// The keeper stands by: the durable's hold admits one holder, and its standby line is the
	// proof its poll is running before the holder is cut off.
	runIntake(t, keeper)
	waitFor(t, 30*time.Second, func() bool {
		return loggedMessage(logs(), heldElsewhere)
	}, func() string {
		return fmt.Sprintf("the second process never stood by for the durable the first holds (%q); the messages logged were %q",
			heldElsewhere, loggedMessages(logs()))
	})

	// A reconnect hook on the holder's client, so a row can wait for the connection to be back
	// and for the client's own restore of its subscriptions to have run: the bus restores them
	// before it runs these hooks (internal/bus/recovery.go). Waiting on SubOK instead would never
	// return once the intake gives its registration up with its hold, which is exactly what a
	// holder cut off does.
	hooked := make(chan struct{}, 8)
	holderClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	return &connectionEventProcesses{
		server: server, proxy: proxy, holderLeases: holderLeases,
		holderClient: holderClient, keeperClient: keeperClient,
		holder: holder, keeper: keeper,
		holderFetches: holderFetches, keeperFetches: keeperFetches.Load,
		holderHeld: holderHeld, hooked: hooked, logs: logs,
	}
}

// cutHolderOff takes the holder off both the link and the lease pool, as a network that drops the
// process out of the cluster or a node that stops scheduling it takes both: the hold is renewed on
// Postgres, so a holder that still reaches it keeps the durable however long its NATS link is
// down, and only a holder that renews nothing lets the standby take it over.
func (p *connectionEventProcesses) cutHolderOff() {
	p.proxy.Cut()
	p.holderLeases.cut.Store(true)
}

// restoreHolder lets the holder reach both again, as the row's own restore of the link does: a
// process back from an outage reaches Postgres again too, and what it finds there is a hold the
// other process holds live.
func (p *connectionEventProcesses) restoreHolder() {
	p.holderLeases.cut.Store(false)
	p.proxy.Restore()
}

// awaitKeeper waits for the keeper to have taken the durable over from the holder
// (awaitDurableTakenOver).
func (p *connectionEventProcesses) awaitKeeper(t *testing.T) {
	t.Helper()
	awaitDurableTakenOver(t, p.holder, p.keeper, p.keeperClient)
}

// fetchCount is how many pull request fetches one process has made, read the same way whichever
// fake GitHub that process was given: a counting fake's atomic counter, or a holding fake's own
// record of the fetches it was asked for. Load, so that a row reads either of them as it reads an
// atomic.Int64.
type fetchCount func() int64

func (count fetchCount) Load() int64 { return count() }

// holderFakeGitHub is the holder's fake GitHub and the count of its fetches: a counting one
// (countingGitHub), or one that counts and holds its first fetch of pull request 1 until the hold
// is released or the handler making it is abandoned (holdPullRequestOneUntilAbandoned, whose
// record of the fetches it was asked for is this fake's count).
//
// That hold's wait on the request's own context, and not on the release alone, is what a row
// holding a handler through a lost hold needs: the stop a lost hold runs abandons a handler by
// cancelling it, waits for every handler to return before it hands the messages back, and a
// handler returns only once the fetch it is inside does.
func holderFakeGitHub(t *testing.T, which holderGitHubFake) (fetchCount, *heldPullRequests, *githubapp.Client) {
	t.Helper()
	if which == holderCountsItsFetches {
		fetches, github := countingGitHub(t)
		return fetches.Load, nil, github
	}
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	return func() int64 { return heldFetches(held, "1") }, held, fake.newTestClient()
}

// heldFetches is how many fetches of pull request number a holding fake GitHub was asked for: one
// "start" event each, recorded as the fetch begins, which is where a counting fake adds to its
// counter (heldPullRequests.of).
func heldFetches(held *heldPullRequests, number string) int64 {
	fetches := int64(0)
	for _, event := range held.of(number) {
		if event == "start "+number {
			fetches++
		}
	}
	return fetches
}

// awaitDurableTakenOver waits for the process standing by to hold the delivery durable while the
// other process is cut off, and names what each of the two is when it never does. The wait covers
// the hold's whole lapse (intakeHoldTTL, 15 s), the standby's poll (durableBindInterval, 2 s) and
// the take-over's own delete, subscribe and create under the consumer lease.
func awaitDurableTakenOver(t *testing.T, holder, keeper *Intake, keeperClient *bus.Client) {
	t.Helper()
	waitFor(t, 45*time.Second, func() bool {
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
// the intake gave up with its hold comes back holding none.
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

// deliveryAckPending is how many messages the delivery durable still has outstanding, and -1
// while the consumer cannot be read, so a row polling it reads a number rather than failing on
// one read a server took its time over.
func deliveryAckPending(natsClient *bus.Client) int {
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return -1
	}
	return info.NumAckPending
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
