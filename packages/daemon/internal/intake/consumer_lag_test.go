package intake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A consumer the daemon is not running counts every message published to it as waiting; it is
// stalled once 60 seconds pass with messages waiting and none handled since it opened, and not once
// it acknowledges one, until 60 more seconds pass with none handled — a message it keeps naking is
// not progress. The oldest waiting message's age is read from the consumer's own info, the moment
// its ack floor last moved: unknown before the consumer has acknowledged anything, and the moment
// of the acknowledgement that moved it after. A poison message it terminates is handled.
func TestConsumerLagCountsWhatWaitsAndCallsAConsumerStalledWhenNoneIsHandledFor60Seconds(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, _ := testJetStream(t)
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	github, dispatch := githubConsumerName(spec.Project), dispatchConsumerName(spec.Project)
	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	for number := 1; number <= 3; number++ {
		publish(t, js, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
	}

	lags := consumers.Lag(ctx)
	if names := lagNames(lags); !slices.Equal(names, []string{dispatch, github, wakeConsumerName(spec.Project), readyConsumerName(spec.Project)}) {
		t.Fatalf("Lag names %q, want the Dispatch, GitHub, wake and ready consumers", names)
	}
	for _, lag := range []ConsumerLag{lagNamed(t, lags, dispatch), lagNamed(t, lags, wakeConsumerName(spec.Project)), lagNamed(t, lags, readyConsumerName(spec.Project))} {
		if lag.Waiting != 0 || !lag.OldestAt.IsZero() || lag.Stalled || lag.Unavailable != "" {
			t.Errorf("%s = %+v, want nothing waiting and nothing unavailable", lag.Name, lag)
		}
	}
	got := lagNamed(t, lags, github)
	if got.Waiting != 3 || !got.OldestAt.IsZero() || got.Stalled || !got.LastHandledAt.IsZero() || got.Unavailable != "" {
		t.Fatalf("GitHub consumer before any is handled = %+v, want 3 waiting, the oldest's age unknown (the ack floor has never moved), not stalled, none handled", got)
	}

	clock.advance(consumerStallAfter - time.Second)
	if got := lagNamed(t, consumers.Lag(ctx), github); got.Stalled {
		t.Fatalf("GitHub consumer 59 seconds after it opened = %+v, want not stalled yet", got)
	}
	clock.advance(2 * time.Second)
	if got := lagNamed(t, consumers.Lag(ctx), github); !got.Stalled || got.Waiting != 3 {
		t.Fatalf("GitHub consumer 61 seconds after it opened with none handled = %+v, want 3 waiting and stalled", got)
	}

	// pr-2 fails every time it is applied, so it is naked again and again and keeps waiting.
	var naks atomic.Int32
	beforeRun := time.Now()
	stop := runConsumers(t, consumers, handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		opened, ok := fact.(PullRequestOpened)
		if !ok {
			return Result{}, fmt.Errorf("unexpected fact %T", fact)
		}
		if opened.Number == 2 {
			naks.Add(1)
			return Result{}, errors.New("pr-2 cannot be applied")
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, fmt.Sprintf("pr-%d", opened.Number))
		return Result{}, err
	}))
	defer stop()
	handledAt := clock.Now()
	testwait.Eventually(t, "pr-1 and pr-3 acknowledged and pr-2 naked twice", func() bool {
		return slices.Equal(writeHandlers(t, pool), []string{"pr-1", "pr-3"}) && naks.Load() >= 2 && lagNamed(t, consumers.Lag(ctx), github).Waiting == 1
	})
	afterAcks := time.Now()
	got = lagNamed(t, consumers.Lag(ctx), github)
	if got.Waiting != 1 || !got.LastHandledAt.Equal(handledAt) || got.Stalled || got.Unavailable != "" {
		t.Fatalf("GitHub consumer after two acknowledgements = %+v, want pr-2 waiting, last handled at %s, not stalled", got, handledAt)
	}
	// pr-1's acknowledgement moved the ack floor; pr-3's did not, since pr-2 is below it.
	if got.OldestAt.Before(beforeRun.Add(-time.Second)) || got.OldestAt.After(afterAcks.Add(time.Second)) || got.OldestAge != clock.Now().Sub(got.OldestAt) {
		t.Fatalf("GitHub consumer's oldest = %s (%s old), want the moment pr-1's acknowledgement moved its ack floor, between %s and %s, aged against the daemon's clock",
			got.OldestAt, got.OldestAge, beforeRun, afterAcks)
	}

	clock.advance(consumerStallAfter + time.Second)
	nakedBefore := naks.Load()
	testwait.Eventually(t, "pr-2 naked again after the clock moved", func() bool { return naks.Load() > nakedBefore })
	if got := lagNamed(t, consumers.Lag(ctx), github); !got.Stalled || got.Waiting != 1 || !got.LastHandledAt.Equal(handledAt) {
		t.Fatalf("GitHub consumer 61 seconds after its last acknowledgement, naking since = %+v, want stalled, last handled at %s", got, handledAt)
	}

	publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-208.issue.updated", []byte(`{`))
	testwait.Eventually(t, "the poison message terminated and counted as handled", func() bool {
		lag := lagNamed(t, consumers.Lag(ctx), dispatch)
		return lag.Waiting == 0 && lag.LastHandledAt.Equal(clock.Now())
	})
}

// A CONSUMER.INFO that fails is named in each consumer's unavailable, and Lag still returns every
// consumer, with waiting and stalled unknown.
func TestConsumerLagNamesWhatNATSDidNotAnswer(t *testing.T) {
	ctx := context.Background()
	served, _ := testJetStream(t)
	standIn := &lagStandIn{JetStream: served}
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	consumers, err := OpenConsumers(ctx, standIn, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	publish(t, served, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	clock.advance(consumerStallAfter + time.Second)

	refused := errors.New("nats: permissions violation")
	standIn.info = refused
	lags := consumers.Lag(ctx)
	if len(lags) != 4 {
		t.Fatalf("Lag = %+v, want all four consumers", lags)
	}
	for _, lag := range lags {
		if lag.Waiting != 0 || lag.Stalled || !lag.OldestAt.IsZero() || lag.Unavailable != "read the consumer's info: "+refused.Error() {
			t.Errorf("%s = %+v, want nothing known and the refusal named", lag.Name, lag)
		}
	}
}

// Every read OpenConsumers and Lag make goes through a consumer of the daemon's own: under a user
// refused every read of the stream itself, its info (STREAM.INFO, which lists its subjects) and a
// stored message (STREAM.MSG.GET), which a daemon held to its projects is not granted, the consumers
// open and every one's waiting count and stalled flag are answered, nothing unavailable. The
// refusal is shown real on the same connection first: a stream info read is refused.
func TestConsumerLagReadsNoStreamState(t *testing.T) {
	ctx := context.Background()
	daemonSeed, daemonPublic := testnkeys.User(t)
	adminSeed, adminPublic := testnkeys.User(t)
	_, uri := testnats.StartNkeyAuthorizedUsers(t, testnats.Image,
		testnats.NkeyUser{Public: daemonPublic, Permissions: `{ publish: { deny: ["$JS.API.STREAM.MSG.GET.>", "$JS.API.STREAM.INFO.>", "$JS.API.DIRECT.GET.>"] } }`},
		testnats.NkeyUser{Public: adminPublic})
	admin := jetStreamAs(t, uri, adminSeed, "admin")
	if _, err := admin.CreateStream(ctx, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create the notification stream: %v", err)
	}
	daemon := jetStreamAs(t, uri, daemonSeed, "the daemon's user")
	refusedRead, cancel := context.WithTimeout(ctx, 2*time.Second)
	if _, err := daemon.Stream(refusedRead, notificationStream); err == nil {
		cancel()
		t.Fatal("the daemon's user read the stream's info, so this test shows nothing about reads it may not make")
	}
	cancel()
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	consumers, err := OpenConsumers(ctx, daemon, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers as a user refused every stream read: %v", err)
	}
	publish(t, admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	for number := 1; number <= 3; number++ {
		publish(t, admin, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
	}
	publish(t, admin, legionWakeSubject, envelopeJSON(t, "wake-1", "envoy",
		fmt.Sprintf(`{"kind":"listener_ready","at":%d}`, clock.Now().UnixMilli())))
	clock.advance(consumerStallAfter + time.Second)

	want := []ConsumerLag{
		{Name: dispatchConsumerName(spec.Project), Waiting: 1, Stalled: true},
		{Name: githubConsumerName(spec.Project), Waiting: 3, Stalled: true},
		{Name: wakeConsumerName(spec.Project)},
		{Name: readyConsumerName(spec.Project), Waiting: 1, Stalled: true},
	}
	if lags := consumers.Lag(ctx); !slices.Equal(lags, want) {
		t.Fatalf("Lag under a user refused every stream read =\n%+v\nwant\n%+v", lags, want)
	}
}

// jetStreamAs is a JetStream client of the server at uri, connected as the nkey user seed is the
// seed of and named for who it is on that server, closed when t ends.
func jetStreamAs(t *testing.T, uri, seed, who string) jetstream.JetStream {
	t.Helper()
	conn, err := natsauth.Connect([]string{uri}, seed, t.Name()+" "+who)
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// lagClock is a clock a test moves by hand (ConsumerSpec.Now).
type lagClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lagClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lagClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// lagStandIn is the test's NATS server whose consumers' info reads fail with info when it is set;
// everything else is the real server's. The test sets it only while no consumer runs, so nothing
// reads it concurrently.
type lagStandIn struct {
	jetstream.JetStream
	info error
}

func (s *lagStandIn) Consumer(ctx context.Context, stream, name string) (jetstream.Consumer, error) {
	if s.info != nil {
		return nil, s.info
	}
	return s.JetStream.Consumer(ctx, stream, name)
}

func lagNames(lags []ConsumerLag) []string {
	names := make([]string, len(lags))
	for i, lag := range lags {
		names[i] = lag.Name
	}
	return names
}

func lagNamed(t *testing.T, lags []ConsumerLag, name string) ConsumerLag {
	t.Helper()
	for _, lag := range lags {
		if lag.Name == name {
			return lag
		}
	}
	t.Fatalf("Lag = %+v names no consumer %s", lags, name)
	return ConsumerLag{}
}
