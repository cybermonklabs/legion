package delivery

// The delivery package's test harness: its NATS server, its Postgres pool, and the helpers the
// intake's tests (intake_route_test.go, intake_consumer_test.go) and the reconcile's share.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deliveryTestParallelism is how many of this package's tests run at once when the command line
// names no -parallel. Every test calls t.Parallel except those that change or read back something
// the whole process shares, which stay serial and run before the rest: a package variable a test
// sets (decodeEnvelope), the default logger (captureLogs), which a test reads when it needs a line
// of something that does not write to the intake's logger -- the bus client, duty -- or requires
// that no component wrote some line, t.Setenv, the process's goroutines, a select on a short
// time.After, or a read of Postgres's whole-server views. An intake's own timers, hooks and logger
// are its tuning (tuningFor, captureIntakeLogs), which no other test's intake reads. Each parallel
// test has a database of its own (storetest.Open's clone) and a JetStream account of its own
// (testnats.URL), and waits on NATS and Postgres far more than on a core. go test's default of
// GOMAXPROCS is four on CI's arm runner, but hundreds on a dev box, where every test's database
// and pools at once would open more connections than the test server allows.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const deliveryTestParallelism = 8

// tunings holds each running test's intake tuning, by the test's name (tuningFor).
var tunings = struct {
	sync.Mutex
	byTest map[string]*intakeTuning
}{byTest: map[string]*intakeTuning{}}

// tuningFor is the tuning (intakeTuning) of every intake test t builds, and the one t changes to
// shorten a timer, hook a step or read the lines of those intakes: made for t on its first use and
// dropped when t ends, as a copy of its nearest ancestor test's when one has a tuning and
// production's (newIntakeTuning) otherwise. Each test has its own, so a test that changes it runs
// beside tests that do not, and every intake one test builds shares it, as they shared the package
// variables it replaces. Set its durations, createDurable and log before the intakes start; its
// atomics at any time.
func tuningFor(t testing.TB) *intakeTuning {
	tunings.Lock()
	defer tunings.Unlock()
	name := t.Name()
	if tuning, ok := tunings.byTest[name]; ok {
		return tuning
	}
	tuning := newIntakeTuning()
	for ancestor := name; strings.Contains(ancestor, "/"); {
		ancestor = ancestor[:strings.LastIndex(ancestor, "/")]
		if parent, ok := tunings.byTest[ancestor]; ok {
			tuning = copyTuning(parent)
			break
		}
	}
	tunings.byTest[name] = tuning
	t.Cleanup(func() {
		tunings.Lock()
		defer tunings.Unlock()
		delete(tunings.byTest, name)
	})
	return tuning
}

// copyTuning is a tuning holding what from holds now: its fields copied one by one, since its
// atomics cannot be copied with it.
func copyTuning(from *intakeTuning) *intakeTuning {
	to := &intakeTuning{
		messageTimeout:         from.messageTimeout,
		maxAckPending:          from.maxAckPending,
		stopBudget:             from.stopBudget,
		headWriteMaxDeliveries: from.headWriteMaxDeliveries,
		createDurable:          from.createDurable,
		log:                    from.log,
	}
	to.callWait.Store(from.callWait.Load())
	to.drainWait.Store(from.drainWait.Load())
	to.stopArmed.Store(from.stopArmed.Load())
	to.beforeRelease.Store(from.beforeRelease.Load())
	return to
}

// TestMain sets the package's default -parallel, drops the databases the package's tests clone
// (storetest.Main) and removes the NATS server they share (testnats.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(deliveryTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(storetest.Main(func() int { return testnats.Main(m.Run) }))
}

// deliveryPools is each running test's database, so every deliveryTestPool of one test answers
// the same one.
var deliveryPools = struct {
	sync.Mutex
	byTest map[*testing.T]*store.Pool
}{byTest: map[*testing.T]*store.Pool{}}

// deliveryTestPool returns the pool of a migrated database of the test's own (storetest.Open),
// with the test's context; a test that needs it skips (not fails) when DISPATCH_TEST_DATABASE_URL
// is unset, matching every other Postgres-backed test in this module's convention. A second call
// in one test returns the same database, and a subtest gets one of its own.
//
// The database is never the one DISPATCH_TEST_DATABASE_URL names. Migrating that one runs each
// CREATE INDEX CONCURRENTLY there, and a concurrent build waits for every session of its own
// database holding a snapshot older than the build's (PostgreSQL's WaitForOlderSnapshots). In CI
// and on a shared machine that database is the server's postgres, where every other package's
// harness creates and drops its databases, so another package's create database could hold the
// build past its lock bound and fail whichever test migrated first (dispatch://INFRA-3904).
func deliveryTestPool(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	deliveryPools.Lock()
	defer deliveryPools.Unlock()
	pool, ok := deliveryPools.byTest[t]
	if !ok {
		pool = storetest.Open(t).Pool
		deliveryPools.byTest[t] = pool
		t.Cleanup(func() {
			deliveryPools.Lock()
			defer deliveryPools.Unlock()
			delete(deliveryPools.byTest, t)
		})
	}
	return pool, t.Context()
}

// deliveryTestLeases is the lease pool an intake takes its durable's hold and consumer lease on
// (intakeHoldName, consumerLease), taken from the test's own pool. Each lease is held on a
// connection of its own, so a test can hold the lease from one caller while an intake tries for it
// from another.
func deliveryTestLeases(t *testing.T, pool *store.Pool) *pgxpool.Pool {
	t.Helper()
	return storetest.Leases(t, pool)
}

// deliveryTestIntakeLeases is the reservation StartIntakes takes in the server, made here on the
// test's own pool (ReserveIntakeLeases).
func deliveryTestIntakeLeases(t *testing.T, pool *store.Pool) IntakeLeases {
	t.Helper()
	callers, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	leases, err := ReserveIntakeLeases(callers)
	if err != nil {
		t.Fatalf("reserve the intakes' callers of the lease pool: %v", err)
	}
	return leases
}

// seedDeliverySettings writes a delivery_settings row with acme/widgets-shaped placeholders.
func seedDeliverySettings(t *testing.T, ctx context.Context, pool *store.Pool) DeliverySettings {
	t.Helper()
	settings, err := PutSettings(ctx, pool, DeliverySettings{
		DeployRepo:           "acme/widgets",
		DeployWorkflowPath:   ".github/workflows/deploy.yml",
		ProductionJobNames:   []string{"widgets-release / widgets-release"},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors:    []string{"octocat"},
		ExcludedRepos:        []string{"acme/playground"},
	}, model.Actor{Kind: "system", ID: "delivery-test"})
	if err != nil {
		t.Fatalf("seed delivery_settings: %v", err)
	}
	return settings
}

// deliveryProject is the project this package's issue fixtures write their issues in, and
// deliveryRepo its default repository. The attribution holds a pull request's issue to the
// repository the pull request is in (delivery's attributionHeld): an issue of a project with no
// default resolves to no repository and would attribute nobody. The key is a neutral one, since
// the head, the attribution and the deployed fixtures all seed into it.
const (
	deliveryProject = "DLVY"
	deliveryRepo    = "acme/widgets"
)

// Each fixture numbers its issues in a band of its own: they share deliveryProject and an issue
// is unique on (project_key, number) (migration 0001_init), while the keys a failure names —
// HEAD-<n>, ATTR-<n>, DEPL-<n> — stay each fixture's own.
const (
	headIssueBand        = 0
	attributionIssueBand = 1000
	deployedIssueBand    = 2000
)

// seedProjectRepository writes project key, named name, with repo as its default repository,
// which is what makes an issue of the project resolve to repo (refs.IssueRepoTerms).
// Idempotent: the fixtures that call it run per test, and one test can call it more than once.
func seedProjectRepository(t *testing.T, ctx context.Context, pool *store.Pool, key, name, repo string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, default_repo) values ($1, $2, 'alice', $3)
		on conflict (key) do update set default_repo = excluded.default_repo
	`, key, name, repo); err != nil {
		t.Fatalf("seed the project with its default repository: %v", err)
	}
}

// seedDeliveryProject writes deliveryProject with deliveryRepo as its default, so every issue
// the fixtures seed resolves to the repository their pull requests are in.
// Idempotent: each fixture calls it, and one test can use several fixtures.
func seedDeliveryProject(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	seedProjectRepository(t, ctx, pool, deliveryProject, "Delivery", deliveryRepo)
}

// publishPullRequestEnvelope publishes one notifications.github.<owner>.<repo>.pr.<number>
// envelope (the shape intake.go's handlePullRequestEnvelope reads), waits for the publish to be
// acknowledged by JetStream, and answers the stream sequence the stream stored it at. The publish
// takes the context a test's call to nats.go takes (testnats.Context), so it waits for its
// acknowledgement under the test binary's deadline and no bound of its own.
func publishPullRequestEnvelope(t *testing.T, natsClient *bus.Client, owner, repo string, number int, fields map[string]string) uint64 {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       fmt.Sprintf("test-%s-%s-%d-%d", owner, repo, number, time.Now().UnixNano()),
		Source:        "github",
		SourceEventID: fmt.Sprintf("%d", time.Now().UnixNano()),
		Topic:         contracts.GithubResourceSubject(owner, repo, "pr", fmt.Sprintf("%d", number)),
		Payload:       string(payload),
		TraceID:       "test-trace",
		IssuedAt:      time.Now().Unix(),
	}
	stored, err := natsClient.PublishAckWithin(testnats.Context(t), envelope)
	if err != nil {
		t.Fatalf("publish envelope: %v", err)
	}
	return stored.Sequence
}

// publishGitHubEnvelope publishes one envelope on topic carrying payload as it stands, for a payload
// no string map can hold (publishPullRequestEnvelope encodes one).
func publishGitHubEnvelope(t *testing.T, natsClient *bus.Client, topic, payload string) {
	t.Helper()
	id := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	if err := natsClient.Publish(contracts.Envelope{
		EventID: id, Source: "github", SourceEventID: id, Topic: topic, Payload: payload,
		TraceID: "test-trace", IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish %s: %v", topic, err)
	}
}

// awaitAcknowledged waits for the intake to have acknowledged every message the test's stream
// holds: the durable's ack floor at the stream's last sequence and nothing outstanding. deliver
// logs and writes before it acknowledges, so what a message caused is in place by then.
func awaitAcknowledged(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	stream, err := natsClient.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	last := stream.State.LastSeq
	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.AckFloor.Stream >= last && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("intake has not acknowledged through stream sequence %d: %+v, %v", last, info, err)
	})
}

// captureLogs sends every slog record from Info up from here on to a buffer, as JSON, until the
// test ends, and answers the records written so far: the default logger's, and the logger of every
// intake the test builds (tuningFor) and every intake StartIntakes builds, which log to the
// default. Intake logs from NATS's delivery goroutine, so the buffer is locked. Call it before
// runIntake, so the default logger is restored only once intake stops. It is for a test that reads
// a line of something that does not take the intake's logger -- the bus client, duty -- or that
// requires no component at all to have written some line; such a test is serial. A test that
// reads its intakes' lines alone calls captureIntakeLogs.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	return captureLogsFrom(t, slog.LevelInfo)
}

// captureLogsFrom is captureLogs from level up, for a row that reads a Debug line.
func captureLogsFrom(t *testing.T, level slog.Level) func() []map[string]any {
	t.Helper()
	var buffer lockedBuffer
	logtest.SetDefault(t, slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: level}))
	tuningFor(t).log = slog.Default()
	return func() []map[string]any { return loggedRecords(&buffer) }
}

// captureIntakeLogs points the logger of every intake the test builds after it (tuningFor), and
// of the settings caches it builds for them (loadedSettingsCache), at a buffer of the test's own,
// as JSON, and answers the records written so far. The default logger is left alone, so the test
// reads its own intakes' lines, none of another test's, and may run beside other tests; what does
// not take the intake's logger -- the bus client, the store, duty, the Attributor -- still writes
// to the default. Call it before the test builds its intakes, which read their logger once they
// run.
func captureIntakeLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buffer lockedBuffer
	tuningFor(t).log = slog.New(slog.NewJSONHandler(&buffer, nil))
	return func() []map[string]any { return loggedRecords(&buffer) }
}

// loggedRecords is every JSON record buffer holds, in the order written, skipping a line that does
// not decode.
func loggedRecords(buffer *lockedBuffer) []map[string]any {
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		var record map[string]any
		if line != "" && json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}

// lockedBuffer is a buffer safe to write from one goroutine while another reads it.
type lockedBuffer = logtest.Buffer

// pathBaseForTest is the workflow file name a workflow subject carries, as intake derives it from
// the configured workflow path.
func pathBaseForTest(workflowPath string) string {
	if i := strings.LastIndex(workflowPath, "/"); i >= 0 {
		return workflowPath[i+1:]
	}
	return workflowPath
}

// capHeadWriteDeliveriesAt lowers the headWriteMaxDeliveries of the intakes this test builds after
// it (tuningFor) to deliveries: the shipped cap of thirty, at headWriteRedeliverAfter apiece, is
// nearly five minutes of wall clock, and what a test of the cap measures is that there is one at
// all, which two deliveries show as well as thirty. It caps a count, not an interval, so it stays a
// value a test sets rather than a span a fake clock passes.
func capHeadWriteDeliveriesAt(t *testing.T, deliveries uint64) {
	t.Helper()
	tuningFor(t).headWriteMaxDeliveries = deliveries
}

// messageTimeoutPastTheRun is the message timeout (intakeTuning.messageTimeout) ackWaitPastTheRun
// gives an intake: six hours.
const messageTimeoutPastTheRun = 6 * time.Hour

// ackWaitPastTheRun gives in's tuning the message timeout messageTimeoutPastTheRun before its Run,
// and returns in. The tuning's ack wait derives from it (intakeTuning.ackWait), so the durable the
// intake creates (deliveryConsumerConfig, a take-over's takeOverConfig) waits a day for an
// acknowledgement, and the intake's handler deadline (Intake.deliver) is six hours out. A row
// whose message must reach a process by a hand-back or a take-over's recreate at the ack floor
// gives it to every intake it runs, since each take-over creates the durable with the taker's own
// config: the server's own redelivery once the ack wait expires then never comes inside the run,
// so a wait the row leaves unbounded fails on a broken hand-back rather than passing on that
// redelivery. Every intake its test builds shares that tuning (tuningFor), so it writes only a
// tuning that does not hold the timeout yet: a row that gives it to a second intake while its
// first runs reads, and does not write, what the first's Run reads.
func ackWaitPastTheRun(in *Intake) *Intake {
	if in.tuning.messageTimeout != messageTimeoutPastTheRun {
		in.tuning.messageTimeout = messageTimeoutPastTheRun
	}
	return in
}

// loadedSettingsCache is a settings cache holding what delivery_settings holds now, read once and
// never again: what an intake under test routes by when nothing changes its settings meanwhile.
// With no row it holds nothing, as a server's cache does before the settings are first written.
// It logs to the test's intake logger (tuningFor), as the cache startIntakes builds does.
func loadedSettingsCache(t *testing.T, ctx context.Context, pool *store.Pool) *SettingsCache {
	t.Helper()
	settings := newSettingsCache(pool, nil, tuningFor(t).log)
	if !settings.reload(ctx) {
		t.Fatal("read delivery_settings into the cache")
	}
	return settings
}

// publishWorkflowEnvelope publishes one workflow-run envelope built as Envoy's own GitHub
// normalizer builds it -- source "github", the delivery GUID as SourceEventID, and the dedupe
// key that pairs with it ("github.<guid>"), which is what earns the publish a JetStream MsgId
// (contracts.DedupeKeyNamesTheUpstreamEvent). Publishing through production's own path rather
// than an ad-hoc envelope is what makes a burst test say anything about production: an envelope
// carrying another dedupe key is stored under different rules. Reports JetStream's own duplicate
// verdict, so a caller can tell a message the stream already held from a new one.
func publishWorkflowEnvelope(t *testing.T, natsClient *bus.Client, owner, repo, workflowPath string, runID int64) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "workflow", "action": "in_progress", "repo": owner + "/" + repo,
		"path": workflowPath, "run_id": strconv.FormatInt(runID, 10),
	})
	if err != nil {
		t.Fatalf("encode workflow payload: %v", err)
	}
	deliveryID := fmt.Sprintf("%s-%d", t.Name(), runID)
	envelope := contracts.Envelope{
		EventID:       deliveryID,
		Source:        "github",
		SourceEventID: deliveryID,
		DedupeKey:     "github." + deliveryID,
		Topic:         contracts.GithubWorkflowSubject(owner, repo, pathBaseForTest(workflowPath), "in_progress"),
		Payload:       string(payload),
		TraceID:       deliveryID,
		IssuedAt:      time.Now().Unix(),
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(envelope) {
		t.Fatalf("the test envelope's dedupe key does not name its event, so it is not published the way production publishes")
	}
	duplicate, err := natsClient.PublishReportingDuplicate(envelope)
	if err != nil {
		t.Fatalf("publish workflow envelope: %v", err)
	}
	return duplicate
}

// intakeTestClient connects one NATS client for an intake test and closes it with the test.
func intakeTestClient(t *testing.T) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, natsClient) })
	return natsClient
}

// stopDeliveringJoined runs intake's StopDelivering on ctx, as a test's own stop of an intake does.
// The stop waits for every handler of the intake to return (keyedRunner.wait in
// intakeDelivery.stop), which only ctx's end gives up (abandon), so on a context that does not end
// before the stop must return - context.Background(), or t.Context() in the row's body - the call
// runs through the shared wait: a handler that never returns fails the test naming the stop rather
// than hanging the binary to its timeout.
func stopDeliveringJoined(t testing.TB, ctx context.Context, intake *Intake) {
	t.Helper()
	testwait.Returns(t, "the intake's stop to return once its handlers have", func() { intake.StopDelivering(ctx) })
}

// awaitBoundDurable waits for the intake to have created its durable and subscribed to it, so a
// test that means to publish into a bound consumer does not race the bind: awaitDurableBoundAt
// for an intake with production's in-flight bound (intakeMaxAckPending).
func awaitBoundDurable(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	awaitDurableBoundAt(t, natsClient, intakeMaxAckPending)
}

// awaitDurableBoundAt waits for the durable to be push-bound with maxAckPending in-flight slots,
// the bound of the intake that created it (intakeTuning.maxAckPending). Its one bound is the shared
// wait's (waitFor): a bind that is merely slow on a loaded machine still passes, and one that
// never comes fails the row naming the durable's last state.
func awaitDurableBoundAt(t *testing.T, natsClient *bus.Client, maxAckPending int) {
	t.Helper()
	var state string
	waitFor(t, func() bool {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil {
			state = err.Error()
			return false
		}
		state = fmt.Sprintf("push_bound=%t max_ack_pending=%d", info.PushBound, info.Config.MaxAckPending)
		return info.PushBound && info.Config.MaxAckPending == maxAckPending
	}, func() string {
		return fmt.Sprintf("intake never bound its durable with max_ack_pending=%d: %s", maxAckPending, state)
	})
}

// awaitRuns waits for the intake to have stored want delivery_runs rows, and fails with the
// stream's message count and the consumer's whole state when it does not -- the numbers that say
// whether a shortfall is a consumer that stopped being given messages or messages that never
// reached the stream.
func awaitRuns(t *testing.T, ctx context.Context, pool *store.Pool, natsClient *bus.Client, want int) {
	t.Helper()
	stored := 0
	waitFor(t, func() bool {
		stored = countRuns(t, ctx, pool)
		return stored >= want
	}, func() string {
		return fmt.Sprintf("delivery_runs rows = %d, want %d\n%s", stored, want, intakeStateReport(t, natsClient))
	})
}

// waitFor waits through the shared wait until done reports true. Should the wait give up, its
// last reading is the text failure builds at that moment -- so the message carries the state the
// wait gave up on, not the state it started from.
func waitFor(t *testing.T, done func() bool, failure func() string) {
	t.Helper()
	testwait.Until(t, "the condition whose failure the last reading names", func() (failureText, bool) {
		return failure, done()
	})
}

// failureText is a wait's failure text, built only when the wait prints it (fmt calls String),
// since some (intakeStateReport) ask NATS for what they say.
type failureText func() string

func (f failureText) String() string { return f() }

// intakeStateReport is the stream's message count per subject and the consumer's delivery state,
// one line each.
func intakeStateReport(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	report := ""
	if stream, err := natsClient.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: ">"}); err != nil {
		report += fmt.Sprintf("stream: %v\n", err)
	} else {
		report += fmt.Sprintf("stream: %d messages, by subject %v\n", stream.State.Msgs, stream.State.Subjects)
	}
	if info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable); err != nil {
		report += fmt.Sprintf("consumer: %v", err)
	} else {
		report += fmt.Sprintf("consumer: delivered=%d ack_floor=%d ack_pending=%d pending=%d redelivered=%d filter_subjects=%q",
			info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumPending, info.NumRedelivered, info.Config.FilterSubjects)
	}
	report += "\n" + singleFilterProbe(t, natsClient)
	return report
}

// singleFilterProbe reports what a consumer carrying one filter subject sees of the same stream.
// Read beside the intake consumer's own numbers it separates the two explanations of a shortfall:
// a stream that does not hold the messages, or a consumer that holds them pending and does not
// hand them over.
func singleFilterProbe(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	subject := contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", "in_progress")
	probe, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     natsgo.AckExplicitPolicy,
	})
	if err != nil {
		return fmt.Sprintf("single-filter probe on %q: %v", subject, err)
	}
	defer deleteProbeConsumer(t, natsClient, probe.Name)
	return fmt.Sprintf("single-filter probe on %q: pending=%d", subject, probe.NumPending)
}

// deleteProbeConsumer removes a consumer a test created only to probe the stream, and logs a
// delete that fails rather than failing the test over its own cleanup.
func deleteProbeConsumer(t *testing.T, natsClient *bus.Client, name string) {
	t.Helper()
	if err := natsClient.JS().DeleteConsumer(bus.Stream, name); err != nil {
		t.Logf("delete probe consumer %s: %v", name, err)
	}
}
