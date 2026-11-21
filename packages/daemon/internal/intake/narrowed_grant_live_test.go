package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/natsgrant"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The daemon's consumers under the grant that holds a daemon to its projects
// (natsgrant.WithinProjects(natsgrant.Daemon(), scope)), on a server that admits the daemon as an
// nkey user with exactly that grant, beside an admin who seeds the stream and the consumers an
// earlier release left. The daemon connects with the reply prefix the grant names
// (natsgrant.InboxPrefix of its identity), as its connection does once its credential names one.

// narrowedIdentity is the account the narrowed daemon's grant names.
const narrowedIdentity = "legion-daemon-CAPTURE"

// narrowedRig is a server that admits an admin and a daemon held to project CAPTURE, with the
// notification stream made, and the Postgres the daemon applies facts on.
type narrowedRig struct {
	admin       jetstream.JetStream
	adminConn   *nats.Conn
	stream      jetstream.Stream
	daemon      jetstream.JetStream
	pool        *pgxpool.Pool
	consumerAPI *nats.Subscription
}

func newNarrowedRig(t *testing.T) *narrowedRig {
	t.Helper()
	grant, err := natsgrant.WithinProjects(natsgrant.Daemon(), natsgrant.Scope{Projects: []string{"CAPTURE"}, Identity: narrowedIdentity})
	if err != nil {
		t.Fatalf("WithinProjects: %v", err)
	}
	daemonSeed, daemonPublic := testnkeys.User(t)
	adminSeed, adminPublic := testnkeys.User(t)
	_, uri := testnats.StartNkeyAuthorizedUsers(t, testnats.Image,
		testnats.NkeyUser{Public: daemonPublic, Permissions: grantConfig(t, grant)},
		testnats.NkeyUser{Public: adminPublic})
	adminConn, err := natsauth.Connect([]string{uri}, adminSeed, t.Name()+" admin")
	if err != nil {
		t.Fatalf("connect the admin: %v", err)
	}
	t.Cleanup(adminConn.Close)
	admin, err := jetstream.New(adminConn)
	if err != nil {
		t.Fatalf("open the admin's JetStream: %v", err)
	}
	stream, err := admin.CreateStream(context.Background(), jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}})
	if err != nil {
		t.Fatalf("create the notification stream: %v", err)
	}
	// Every consumer create or update the server is asked for, by anyone, from here on.
	consumerAPI, err := adminConn.SubscribeSync("$JS.API.CONSUMER.*.>")
	if err != nil {
		t.Fatalf("watch the consumer calls: %v", err)
	}
	if err := adminConn.Flush(); err != nil {
		t.Fatalf("flush the watch: %v", err)
	}
	daemonConn, err := natsauth.Connect([]string{uri}, daemonSeed, t.Name()+" daemon", nats.CustomInboxPrefix(natsgrant.InboxPrefix(narrowedIdentity)))
	if err != nil {
		t.Fatalf("connect the narrowed daemon: %v", err)
	}
	t.Cleanup(daemonConn.Close)
	daemon, err := jetstream.New(daemonConn)
	if err != nil {
		t.Fatalf("open the narrowed daemon's JetStream: %v", err)
	}
	pool := migratedPool(t)
	if _, err := pool.Exec(context.Background(), `create table intake_test_writes (id bigserial primary key, handler text not null)`); err != nil {
		t.Fatalf("create write table: %v", err)
	}
	return &narrowedRig{admin: admin, adminConn: adminConn, stream: stream, daemon: daemon, pool: pool, consumerAPI: consumerAPI}
}

// grantConfig writes grant in the server configuration's permissions syntax, each subject quoted
// as JSON writes it with no HTML escape: the configuration's parser reads no \u003e.
func grantConfig(t *testing.T, grant natsgrant.Permissions) string {
	t.Helper()
	list := func(subjects []string) string {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(subjects); err != nil {
			t.Fatalf("encode %v: %v", subjects, err)
		}
		return strings.TrimSpace(out.String())
	}
	block := func(allow, deny []string) string {
		var parts []string
		if len(allow) > 0 {
			parts = append(parts, "allow: "+list(allow))
		}
		if len(deny) > 0 {
			parts = append(parts, "deny: "+list(deny))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return "{ publish: " + block(grant.Publish, grant.PublishDeny) + ", subscribe: " + block(grant.Subscribe, grant.SubscribeDeny) + " }"
}

// consumerCalls is the subjects of every consumer call the server was asked for since the last
// read, each once in order.
func (r *narrowedRig) consumerCalls(t *testing.T) []string {
	t.Helper()
	var calls []string
	for {
		message, err := r.consumerAPI.NextMsg(200 * time.Millisecond)
		if err != nil {
			return calls
		}
		calls = append(calls, message.Subject)
	}
}

// createsIn is the consumer creates and updates among calls.
func createsIn(calls []string) []string {
	var creates []string
	for _, call := range calls {
		if strings.Contains(call, ".CREATE.") {
			creates = append(creates, call)
		}
	}
	return creates
}

// applied takes back an earlier release's first message on consumer as that release applied it:
// fetched and acknowledged, so the consumer's ack floor stands past it.
func (r *narrowedRig) applied(t *testing.T, consumer string) {
	t.Helper()
	handle, err := r.stream.Consumer(context.Background(), consumer)
	if err != nil {
		t.Fatalf("open %s: %v", consumer, err)
	}
	batch, err := handle.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch %s's first message: %v", consumer, err)
	}
	for message := range batch.Messages() {
		if err := message.DoubleAck(context.Background()); err != nil {
			t.Fatalf("acknowledge %s's first message: %v", consumer, err)
		}
		return
	}
	t.Fatalf("%s had no first message to apply", consumer)
}

// marks is a handler that writes each fact it applies as a mark: a pull request's opened as
// pr-<number>, a listener wake as wake-<kind>-<role or session>, and any other fact by its type.
func marks() Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		var mark string
		switch fact := fact.(type) {
		case PullRequestOpened:
			mark = fmt.Sprintf("pr-%d", fact.Number)
		case ListenerWake:
			mark = "wake-" + fact.Kind + "-" + fact.Role + fact.Session
		default:
			mark = fmt.Sprintf("%T", fact)
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, mark)
		return Result{}, err
	})
}

// pullRequestOpenedOn is the opened of repo#number, repo spelt as GitHub's payload spells it.
func pullRequestOpenedOn(t *testing.T, repo string, number int) []byte {
	t.Helper()
	return envelopeJSON(t, fmt.Sprintf("pr-opened-%s-%d", repo, number), "github", fmt.Sprintf(`{"action":"opened","author":"author","base_ref":"main",`+
		`"body":"Dispatch: LEGION-208","head_ref":"legion/LEGION-208","head_sha":"head-%d","kind":"pr","merged":"false","number":"%d",`+
		`"repo":%q,"title":"a pull request","updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/%s/pull/%d"}`,
		number, number, repo, repo, number))
}

// A boot under the narrowed grant moves each consumer an earlier release left onto its one filter
// in place, and loses and repeats nothing:
//
//   - the Dispatch consumer of the old two-filter shape (every project's issue events beside the
//     settings subject) is updated in place to this project's issue events alone: the event published
//     after its ack floor is applied once, the one before it not again, and another project's event
//     is never delivered;
//   - the GitHub consumer of main's shape (one filter per heard repository) is updated in place to
//     every repository's events: the events published after its applied one, of a heard repository
//     whose name holds a `.` and of one the daemon hears in GitHub's mixed case (`Acme/Widgets`), are
//     applied once each, and `acme/priv`'s, a repository it does not hear, is acknowledged with no
//     fact;
//   - the wake consumer, missing with a stored position, is created at that position, and the
//     report stored after it is applied;
//   - Lag answers all four consumers, nothing unavailable.
//
// Controls: a consumer created at the next message, as a consumer under a new name with no position
// starts, holds none of the events published while the daemon was down, so an update in place is
// what keeps them; and main's createConsumer, which read the stream's info, is refused by the grant.
func TestANarrowedDaemonMovesItsConsumersOntoOneFilterInPlace(t *testing.T) {
	ctx := context.Background()
	rig := newNarrowedRig(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.Repositories = []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("Acme/Widgets"), ghrepo.MustParse("acme/dotted.name")}
	dispatch, github, wake := dispatchConsumerName(spec.Project), githubConsumerName(spec.Project), wakeConsumerName(spec.Project)

	// What the earlier release left: its two consumers, each with one message applied.
	publish(t, rig.admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("sjawhar/legion"))+".pr.1", pullRequestOpenedOn(t, "sjawhar/legion", 1))
	for _, config := range []jetstream.ConsumerConfig{
		{Durable: dispatch, FilterSubjects: []string{"notifications.dispatch.issue.>", settingsSubject(spec.Project)}, AckPolicy: jetstream.AckExplicitPolicy},
		{Durable: github, FilterSubjects: []string{
			githubRepositoryPrefix(ghrepo.MustParse("sjawhar/legion")) + ".>",
			githubRepositoryPrefix(ghrepo.MustParse("acme/widgets")) + ".>",
			githubRepositoryPrefix(ghrepo.MustParse("acme/dotted.name")) + ".>",
		}, AckPolicy: jetstream.AckExplicitPolicy},
	} {
		if _, err := rig.stream.CreateConsumer(ctx, config); err != nil {
			t.Fatalf("create the earlier release's %s: %v", config.Durable, err)
		}
		rig.applied(t, config.Durable)
	}
	wakeFrom := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := rig.pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 1, $2)`, wake, wakeFrom); err != nil {
		t.Fatalf("store the wake consumer's position: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	// What was published while the daemon was down.
	publish(t, rig.admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	publish(t, rig.admin, "notifications.dispatch.issue.OTHER.OTHER-9.issue.updated", envelopeJSON(t, "dispatch-other-9", "dispatch",
		`{"id":99,"issue_key":"OTHER-9","seq":1,"notify":true,"type":"issue.updated","payload":{}}`))
	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("acme/dotted.name"))+".pr.2", pullRequestOpenedOn(t, "acme/dotted.name", 2))
	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("acme/widgets"))+".pr.3", pullRequestOpenedOn(t, "acme/widgets", 3))
	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("acme/priv"))+".pr.4", pullRequestOpenedOn(t, "acme/priv", 4))
	publish(t, rig.admin, "notifications.legion.capture.wake", envelopeJSON(t, "wake-capture-1", "envoy",
		fmt.Sprintf(`{"kind":"role_message","role":"legion-capture-capture-7-architect","at":%d}`, time.Now().UnixMilli())))

	// Control: a consumer started at the next message holds none of it.
	for _, filter := range []string{"notifications.dispatch.issue.CAPTURE.>", "notifications.github.>"} {
		fresh, err := rig.stream.CreateConsumer(ctx, jetstream.ConsumerConfig{FilterSubject: filter, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
		if err != nil {
			t.Fatalf("create the control consumer on %s: %v", filter, err)
		}
		if pending := fresh.CachedInfo().NumPending; pending != 0 {
			t.Fatalf("a consumer started at the next message on %s holds %d messages, want none, so it shows nothing", filter, pending)
		}
	}
	// Control: main's createConsumer read the stream's info, which the grant refuses.
	refused, cancel := context.WithTimeout(ctx, 3*time.Second)
	if _, err := rig.daemon.Stream(refused, notificationStream); err == nil {
		cancel()
		t.Fatal("the narrowed daemon read the stream's info: the grant does not refuse what main's createConsumer read")
	}
	cancel()
	rig.consumerCalls(t)

	consumers, err := OpenConsumers(ctx, rig.daemon, rig.pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers under the narrowed grant: %v", err)
	}
	for name, want := range map[string]string{dispatch: "notifications.dispatch.issue.CAPTURE.>", github: githubFilter, wake: "notifications.legion.capture.wake", readyConsumerName(spec.Project): legionWakeSubject} {
		info := consumerInfo(t, rig.stream, name)
		if info.Config.FilterSubject != want || len(info.Config.FilterSubjects) != 0 {
			t.Errorf("%s filters %q %v, want exactly %s", name, info.Config.FilterSubject, info.Config.FilterSubjects, want)
		}
	}
	if info := consumerInfo(t, rig.stream, wake); info.Config.DeliverPolicy != jetstream.DeliverByStartTimePolicy || info.Config.OptStartTime == nil || !info.Config.OptStartTime.Equal(wakeFrom) {
		t.Errorf("the wake consumer starts %v at %v, want at its stored position %s", info.Config.DeliverPolicy, info.Config.OptStartTime, wakeFrom)
	}

	stop := runConsumers(t, consumers, marks())
	defer stop()
	want := []string{"pr-2", "pr-3", "wake-role_message-legion-capture-capture-7-architect"}
	testwait.Eventually(t, "the events published while the daemon was down applied", func() bool {
		got := writeHandlers(t, rig.pool)
		return len(got) == len(want)+1
	})
	got := writeHandlers(t, rig.pool)
	slices.Sort(got)
	var dispatchFacts []string
	var rest []string
	for _, mark := range got {
		if strings.HasPrefix(mark, "pr-") || strings.HasPrefix(mark, "wake-") {
			rest = append(rest, mark)
		} else {
			dispatchFacts = append(dispatchFacts, mark)
		}
	}
	if !slices.Equal(rest, want) || len(dispatchFacts) != 1 {
		t.Fatalf("applied %q, want one Dispatch fact (CAPTURE-4) and %q, each once, and nothing of acme/priv, OTHER-9 or what was applied before", got, want)
	}
	for _, name := range []string{dispatch, github, wake} {
		waitDelivered(t, rig.stream, name)
	}
	if delivered := consumerInfo(t, rig.stream, dispatch).Delivered.Consumer; delivered != 2 {
		t.Errorf("the Dispatch consumer delivered %d messages, want 2: the earlier release's CAPTURE-3 and this boot's CAPTURE-4, never OTHER-9", delivered)
	}

	lags := consumers.Lag(ctx)
	if len(lags) != 4 {
		t.Fatalf("Lag = %+v, want the four consumers", lags)
	}
	for _, lag := range lags {
		if lag.Unavailable != "" {
			t.Errorf("Lag of %s under the narrowed grant: %s", lag.Name, lag.Unavailable)
		}
	}
}

// Under the narrowed grant, an event of a repository the daemon does not hear is acknowledged with
// no fact; SetRepositories then adds that repository while Run runs, an event of it published after
// the add is applied once, and the server is asked for no consumer create or update after the boot.
//
// Control, in this file: main's SetRepositories updated the GitHub consumer to one filter per
// repository; that create's subject names no filter, the grant refuses it, and it fails.
func TestARepositoryAddedMidRunIsHeardWithNoConsumerCall(t *testing.T) {
	ctx := context.Background()
	rig := newNarrowedRig(t)
	spec := consumerSpec(&lockedBuffer{})
	consumers, err := OpenConsumers(ctx, rig.daemon, rig.pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers under the narrowed grant: %v", err)
	}
	if creates := createsIn(rig.consumerCalls(t)); len(creates) != 4 {
		t.Fatalf("the boot asked for creates %q, want the four consumers', so the watch shows nothing", creates)
	}
	stop := runConsumers(t, consumers, marks())
	defer stop()
	github := githubConsumerName(spec.Project)

	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("acme/added"))+".pr.5", pullRequestOpenedOn(t, "acme/added", 5))
	waitDelivered(t, rig.stream, github)
	if got := writeCount(t, rig.pool); got != 0 {
		t.Fatalf("%d facts applied for a repository the daemon does not hear yet, want none", got)
	}
	if err := consumers.SetRepositories(ctx, append(slices.Clone(capturedRepositories), ghrepo.MustParse("acme/added"))); err != nil {
		t.Fatalf("SetRepositories: %v", err)
	}
	publish(t, rig.admin, githubRepositoryPrefix(ghrepo.MustParse("acme/added"))+".pr.6", pullRequestOpenedOn(t, "acme/added", 6))
	testwait.Eventually(t, "the added repository's event applied", func() bool { return writeCount(t, rig.pool) == 1 })
	waitDelivered(t, rig.stream, github)
	if got := writeHandlers(t, rig.pool); !slices.Equal(got, []string{"pr-6"}) {
		t.Fatalf("applied %q, want pr-6 once", got)
	}
	if creates := createsIn(rig.consumerCalls(t)); len(creates) != 0 {
		t.Fatalf("the server was asked for consumer creates %q after the boot, want none", creates)
	}

	// Control: main's update, one filter per repository.
	refused, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = rig.daemon.CreateOrUpdateConsumer(refused, notificationStream, jetstream.ConsumerConfig{
		Durable: github, AckPolicy: jetstream.AckExplicitPolicy,
		FilterSubjects: []string{githubRepositoryPrefix(capturedRepositories[0]) + ".>", githubRepositoryPrefix(ghrepo.MustParse("acme/added")) + ".>"},
	})
	if err == nil {
		t.Fatal("main's per-repository update of the GitHub consumer went through the narrowed grant, so this test shows nothing")
	}
	t.Logf("main's per-repository update under the narrowed grant: %v", err)
}

// Plan E v77's gate condition: a narrowed daemon's ready consumer starts at its own creation, never
// at the wake consumer's stored position, since it carries only the listener's liveness report,
// which says nothing about the past. A PRIV role_message a listener of an earlier release stored on
// the shared wake subject before the ready consumer is created is never delivered to it.
//
// Control: a consumer started at the wake consumer's stored position, as a consumer that takes the
// position of the one whose filter it splits off starts, delivers that message.
func TestAReadyConsumerNeverReadsAWakeStoredBeforeItsCreation(t *testing.T) {
	ctx := context.Background()
	rig := newNarrowedRig(t)
	spec := consumerSpec(&lockedBuffer{})
	ready, wake := readyConsumerName(spec.Project), wakeConsumerName(spec.Project)
	wakeFrom := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := rig.pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 1, $2)`, wake, wakeFrom); err != nil {
		t.Fatalf("store the wake consumer's position: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	const privRole = "legion-priv-priv-7-architect"
	publish(t, rig.admin, legionWakeSubject, envelopeJSON(t, "wake-priv-1", "envoy",
		fmt.Sprintf(`{"kind":"role_message","role":%q,"at":%d}`, privRole, time.Now().UnixMilli())))

	// Control: the split-off start delivers it.
	control, err := rig.stream.CreateConsumer(ctx, jetstream.ConsumerConfig{FilterSubject: legionWakeSubject,
		DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartTime: &wakeFrom, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatalf("create the control consumer: %v", err)
	}
	batch, err := control.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch from the control consumer: %v", err)
	}
	controlSaw := false
	for message := range batch.Messages() {
		controlSaw = controlSaw || strings.Contains(string(message.Data()), privRole)
	}
	if !controlSaw {
		t.Fatal("a consumer started at the wake consumer's stored position did not deliver the PRIV role_message, so this test shows nothing")
	}

	consumers, err := OpenConsumers(ctx, rig.daemon, rig.pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers under the narrowed grant: %v", err)
	}
	info := consumerInfo(t, rig.stream, ready)
	if info.Config.DeliverPolicy != jetstream.DeliverNewPolicy || info.NumPending != 0 {
		t.Fatalf("the ready consumer starts %v with %d pending, want at its creation with nothing pending", info.Config.DeliverPolicy, info.NumPending)
	}
	stop := runConsumers(t, consumers, marks())
	defer stop()
	publish(t, rig.admin, legionWakeSubject, envelopeJSON(t, "ready-1", "envoy",
		fmt.Sprintf(`{"kind":"listener_ready","at":%d}`, time.Now().UnixMilli())))
	testwait.Eventually(t, "the listener_ready published after the ready consumer's creation applied", func() bool {
		return slices.Contains(writeHandlers(t, rig.pool), "wake-listener_ready-")
	})
	waitDelivered(t, rig.stream, ready)
	if got := writeHandlers(t, rig.pool); slices.ContainsFunc(got, func(mark string) bool { return strings.Contains(mark, privRole) }) {
		t.Fatalf("applied %q: the ready consumer delivered the PRIV role_message stored before its creation", got)
	}
	if delivered := consumerInfo(t, rig.stream, ready).Delivered.Consumer; delivered != 1 {
		t.Fatalf("the ready consumer delivered %d messages, want only the listener_ready published after its creation", delivered)
	}
}
