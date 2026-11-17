package delivery

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A pull-request envelope whose head row could not be stored at all is handed back for
// redelivery. Nothing else repairs a row that was never stored: the reconcile's passes read the
// rows that exist (ListStalePullRequestHeads), so an acknowledged envelope loses that pull
// request's head for good, and with it every `before_merge` result measured against that head.
//
// The write that cannot be made is the one UpsertPullRequestHead reports as ErrHeadNotStored,
// which handlePullRequestEnvelope returns; here branch_commits is away, so the transaction that
// writes the head row and the merge's seed together rolls back and no head row lands.
//
// The test asks NATS, not the handler: the durable must deliver the message again. Measured by
// the consumer's delivery count rather than by its pending count, because a message is pending
// while its handler runs, and what is under test is what happens after the handler returns.
func TestAnEnvelopeWhoseHeadRowCouldNotBeStoredIsHandedBack(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedDeliveryProject(t, ctx, pool)
	withoutBranchCommits(t, ctx, pool)

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
		newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	// A merge, so the seed is written beside the head row and the write is the one that fails.
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 9101, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": "9101", "author": "octocat", "base_ref": "integration/x",
		"head_sha":         "1111111111111111111111111111111111111111",
		"merge_commit_sha": "2222222222222222222222222222222222222222",
	})

	var info *natsgo.ConsumerInfo
	waitFor(t, 45*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before >= 2
	}, func() string {
		return fmt.Sprintf("the envelope was delivered %d time(s) and never handed back, though no head row was stored: %+v",
			info.Delivered.Consumer-before, info)
	})

	var stored int
	if err := pool.QueryRow(ctx, "select count(*) from pull_request_heads where number = 9101").Scan(&stored); err != nil {
		t.Fatalf("read the head rows: %v", err)
	}
	if stored != 0 {
		t.Fatalf("head rows stored while branch_commits is away = %d, want none", stored)
	}
}

// headWriteHoldNumber is the pull request the row below publishes, its own, so that row reads
// its own envelope and no other's.
const headWriteHoldNumber = 9104

// headWriteProcess is one envoy-dispatch process of the row below: its intake, the lease pool
// its hold is renewed on, which the row cuts to lapse that hold, and the count of the commit
// reads its attribution made -- one per handling of the envelope, and the step the row holds a
// handler in.
type headWriteProcess struct {
	intake *Intake
	leases *cuttableLeases
	reads  atomic.Int64
	// release lets the held read answer.
	release func()
}

// startHeadWriteProcess runs a delivery intake as a process of its own on pool's database (a
// pool of its own, storetest.SecondProcess) and natsURL's server, until the test ends. Its
// GitHub answers the pull request's commit read with no commits, after counting it, and holds
// the first one when hold is set until the row releases it or the handler making it is
// abandoned. That read is where a handler can be held for this row: everything after it is the
// head write, which is the failure under test.
func startHeadWriteProcess(t *testing.T, pool *store.Pool, natsURL string, hold bool) *headWriteProcess {
	t.Helper()
	second := storetest.SecondProcess(t, &store.Store{Pool: pool})
	leasePool := storetest.Leases(t, second.Pool)
	client, err := bus.Connect([]string{natsURL}, bus.WithName(intakeConnectionName))
	if err != nil {
		t.Fatalf("connect the process's intake: %v", err)
	}
	t.Cleanup(client.Close)
	process := &headWriteProcess{leases: &cuttableLeases{pool: leasePool}}

	held := make(chan struct{})
	var first sync.Once
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		process.reads.Add(1)
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
		mustEncode(t, w, []any{})
	})
	github := fake.newTestClient()
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	process.release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(process.release)

	process.intake = newIntake(client, second.Pool, process.leases, github, events.NewBroker(),
		loadedSettingsCache(t, t.Context(), second.Pool))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		process.intake.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return process
}

// durableAnswers records every answer published to the delivery durable while the test runs --
// an acknowledgement and a hand-back alike, which a subscriber publishes at the message's own
// reply subject, $JS.ACK.<stream>.<consumer>.… -- and answers their bodies, in the order they
// were published. It is the one place a hand-back can be seen whether or not the server still
// has a consumer to act on it, which is what the row below needs: a hand-back published after
// a take-over has recreated the durable is exactly the fault it measures.
func durableAnswers(t *testing.T, client *bus.Client) func() []string {
	t.Helper()
	var mu sync.Mutex
	var answers []string
	sub, err := client.SubscribeCore("$JS.ACK."+bus.Stream+"."+bus.DispatchDeliveryDurable+".>",
		func(msg *natsgo.Msg) {
			mu.Lock()
			answers = append(answers, string(msg.Data))
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("subscribe to the delivery durable's answers: %v", err)
	}
	t.Cleanup(func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Logf("unsubscribe from the delivery durable's answers: %v", err)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(answers)
	}
}

// A holder whose hold has lapsed publishes no hand-back of its own, and the envelope it could
// not answer reaches the next holder once.
//
// The hand-back a head write that failed earns (ErrHeadNotStored, Intake.deliver) is this
// process's own answer to the durable, unlike every other answer of a handler the stop gave up,
// which the stop makes. It is sound only while the hold covers it. A take-over deletes the
// durable and creates it again under the same name at its ack floor (heldDurable.takeOver), and
// the server reads a message's reply subject for that name: a hand-back a lapsed holder
// publishes afterwards lands on the new holder's consumer, where it moves that message's
// pending entry and has the new holder delivered it a second time -- two processes fetching one
// pull request at once, which is the thing the hold exists to stop.
//
// The row drives the window the fault lives in. The holder's handler is held inside the commit
// read its attribution makes, its lease pool is then cut so no renewal of its hold lands, and
// the read is released the moment the holder reports the hold lost and begins to stop
// delivering (keyedRunnerStopped): the handler returns inside the budget that stop gives it, so
// it answers through deliver's own path rather than the stop's, with a hold that has already
// lapsed. Its head write fails because branch_commits is away, which is the failure the
// hand-back is for.
//
// What it measures is the answers published to the durable, read from the ack subject itself
// rather than from the consumer's counts: a hand-back is a publish, and whether the server
// still has a consumer to act on it depends on how far the take-over has got. None of them may
// be a hand-back. The keeper's own acknowledgement is there to be read beside them, so a
// recorder that saw nothing at all cannot pass the row by silence.
//
// The table is put back once the holder has stopped delivering and before the keeper is handed
// anything, so the keeper's handling is the ordinary one: it writes the head row the holder
// could not, acknowledges the envelope, and is handed it exactly once.
func TestAHolderWhoseHoldLapsedPublishesNoHandBackAndTheKeeperIsHandedTheEnvelopeOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedDeliveryProject(t, ctx, pool)
	restoreBranchCommits := withoutBranchCommits(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	answers := durableAnswers(t, publisher)

	holder := startHeadWriteProcess(t, pool, natsURL, true)
	awaitBoundDurable(t, publisher)
	keeper := startHeadWriteProcess(t, pool, natsURL, false)

	// A merge, so the head row and its branch seed are written together and the write is the
	// one that fails; its own head repository and a branch naming no issue, so the attribution
	// reads the commit messages and the handler can be held there.
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", headWriteHoldNumber, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(headWriteHoldNumber), "author": "octocat",
		"base_ref": "integration/x", "head_ref": "topic/none", "head_repo": "acme/widgets",
		"head_sha":         "5555555555555555555555555555555555555555",
		"merge_commit_sha": "6666666666666666666666666666666666666666",
	})
	waitFor(t, 30*time.Second, func() bool { return holder.reads.Load() == 1 }, func() string {
		return "the holder's handler never reached the read this row holds it in: " + consumerState(publisher)
	})

	// The cut takes the holder off the lease pool its hold is renewed on, as a network that
	// took it off Postgres would: the hold lapses, and the holder stops delivering.
	holder.leases.cut.Store(true)
	waitFor(t, 30*time.Second, func() bool { return keyedRunnerStopped(holder.intake) }, func() string {
		return "the holder never began stopping, though its hold is no longer renewed: " + consumerState(publisher)
	})
	holder.release()

	// The holder's subscription gone from the durable is its stop finished, so its handler has
	// returned and whatever it was going to publish is behind it.
	waitFor(t, 30*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && !info.PushBound
	}, func() string {
		return "the holder has not let the durable go: " + consumerState(publisher)
	})
	restoreBranchCommits()

	// 60 s: the hold's lapse (intakeHoldTTL, 15 s), the keeper's poll (durableBindInterval, 2 s)
	// and its take-over's delete, subscribe and create under the consumer lease.
	waitFor(t, 60*time.Second, func() bool { return keeper.reads.Load() >= 1 }, func() string {
		return fmt.Sprintf("the keeper was never handed the envelope the holder left unanswered: holder %d read(s), keeper %d; %s",
			holder.reads.Load(), keeper.reads.Load(), consumerState(publisher))
	})
	awaitEverythingAcknowledged(t, publisher)

	published := answers()
	var handedBack []string
	for _, answer := range published {
		if strings.HasPrefix(answer, "-NAK") {
			handedBack = append(handedBack, answer)
		}
	}
	if len(handedBack) > 0 {
		t.Errorf("hand-backs published to the delivery durable = %q, want none: the holder's hold had lapsed, so its hand-back lands on the durable the keeper's take-over recreated",
			handedBack)
	}
	if len(published) == 0 {
		t.Fatal("no answer to the delivery durable was recorded at all, so the row measured nothing: the keeper's acknowledgement is one of them")
	}
	if read := keeper.reads.Load(); read != 1 {
		t.Errorf("the keeper handled the envelope %d time(s), want the one the take-over hands it: %s", read, consumerState(publisher))
	}
	if read := holder.reads.Load(); read != 1 {
		t.Errorf("the holder handled the envelope %d time(s), want the one its lapsed hold left unanswered", read)
	}
	var stored int
	if err := pool.QueryRow(ctx, "select count(*) from pull_request_heads where number = $1",
		headWriteHoldNumber).Scan(&stored); err != nil {
		t.Fatalf("read the head rows: %v", err)
	}
	if stored != 1 {
		t.Errorf("head rows of the pull request the holder could not store = %d, want the one the keeper wrote: the envelope was lost with the hold", stored)
	}
}

// The fence on that hand-back is the hold's own and not the stop's. A delivery whose hold has
// lapsed answers nothing of its own even where nothing has stopped it: from the loss deadline
// on, another process may take the hold and recreate the durable at any moment, and this one
// cannot know which side of that moment an answer of its own would land on.
//
// The row above drives the sequence a running intake makes of that, where the stop the lost
// hold starts has set its flag by the time the handler returns; this one holds the half that
// sequence cannot separate, since an intake stops delivering within intakeHoldMargin of
// reporting its hold lost.
func TestADeliveryWhoseHoldHasLapsedAnswersNothingOfItsOwn(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	hold, err := duty.TryHold(ctx, deliveryTestLeases(t, pool), "delivery-test-lapsing-hold", uuid.New(),
		duty.HoldOptions{TTL: intakeHoldTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin})
	if err != nil {
		t.Fatalf("take the hold this row lapses: %v", err)
	}
	if hold == nil {
		t.Fatal("the hold this row lapses is held by another process, though the harness truncates duty_holds")
	}
	delivery := &intakeDelivery{hold: hold, subject: natsgo.NewInbox()}
	if !delivery.mayAnswer() {
		t.Fatal("a delivery under a live hold, with nothing stopped, reports that it may not answer the durable itself")
	}

	if err := hold.Lapse(ctx); err != nil {
		t.Fatalf("lapse the hold: %v", err)
	}
	if delivery.mayAnswer() {
		t.Fatal("a delivery whose hold has lapsed reports that it may still answer the durable: a hand-back it publishes reaches the durable the next holder's take-over recreated")
	}
}
