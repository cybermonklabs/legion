package delivery

// The delivery package's test harness: its NATS server, its Postgres pool, and the helpers the
// intake's tests (intake_route_test.go, intake_consumer_test.go) and the reconcile's share.

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestMain drops the databases the package's tests clone (storetest.Main) and removes the NATS
// server they share (testnats.Main).
func TestMain(m *testing.M) {
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
// deliveryRepo the repository it owns by default. One project owns it because repo_projects'
// primary key is the repository (migration 0005_repo_projects), so only one project can map
// acme/widgets, and the attribution now holds a pull request's issue to the repository the pull
// request is in (delivery's attributionHeld): an issue of a project that maps nothing resolves
// to no repository and would attribute nobody. The key is a neutral one, since the head, the
// attribution and the deployed fixtures all seed into it.
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

// seedProjectRepository writes project key, named name, and maps repo to it as that project's
// default, which is what makes an issue of the project resolve to repo (refs.IssueRepoTerms).
// Idempotent: a test can call more than one of the fixtures that call it.
func seedProjectRepository(t *testing.T, ctx context.Context, pool *store.Pool, key, name, repo string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $2, 'alice') on conflict do nothing
	`, key, name); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default)
		values ($1, $2, '{"kind":"system","id":"delivery-test"}'::jsonb, true)
		on conflict do nothing
	`, repo, key); err != nil {
		t.Fatalf("map the repository to the project: %v", err)
	}
}

// seedDeliveryProject writes deliveryProject and maps deliveryRepo to it as its default, so
// every issue the fixtures seed resolves to the repository their pull requests are in.
// Idempotent: each fixture calls it, and a test can call more than one fixture.
func seedDeliveryProject(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	seedProjectRepository(t, ctx, pool, deliveryProject, "Delivery", deliveryRepo)
}

// publishPullRequestEnvelope publishes one notifications.github.<owner>.<repo>.pr.<number>
// envelope (the shape intake.go's handlePullRequestEnvelope reads) and waits for the publish to
// be acknowledged by JetStream before returning.
func publishPullRequestEnvelope(t *testing.T, natsClient *bus.Client, owner, repo string, number int, fields map[string]string) {
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
	if err := natsClient.Publish(envelope); err != nil {
		t.Fatalf("publish envelope: %v", err)
	}
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

// captureLogs sends every slog record from here on to a buffer, as JSON, until the test ends, and
// answers the records written so far. Intake logs from NATS's delivery goroutine, so the buffer is
// locked. Call it before startIntake, so the default logger is restored only once intake stops.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buffer lockedBuffer
	logtest.SetDefault(t, slog.NewJSONHandler(&buffer, nil))
	return func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
			var record map[string]any
			if line != "" && json.Unmarshal([]byte(line), &record) == nil {
				records = append(records, record)
			}
		}
		return records
	}
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

// capHeadWriteDeliveriesAt shrinks headWriteMaxDeliveries to deliveries for one test: the shipped
// cap of thirty, at headWriteRedeliverAfter apiece, is nearly five minutes of wall clock, and what
// a test of the cap measures is that there is one at all, which two deliveries show as well as
// thirty. It caps a count, not an interval, so it stays a variable a test sets.
func capHeadWriteDeliveriesAt(t *testing.T, deliveries uint64) {
	t.Helper()
	was := headWriteMaxDeliveries
	headWriteMaxDeliveries = deliveries
	t.Cleanup(func() { headWriteMaxDeliveries = was })
}

// ackWaitPastTheRun makes the delivery durable's ack wait longer than any run of the package, for
// one test: intakeAckWait derives it from intakeMessageTimeout, here six hours, so the durable the
// intakes create (deliveryConsumerConfig, a take-over's takeOverConfig) waits a day for an
// acknowledgement. A row whose message must reach a process by a hand-back or a take-over's
// recreate at the ack floor calls it before it starts any intake: the server's own redelivery
// once the ack wait expires then never comes inside the run, so a wait the row leaves unbounded
// fails on a broken hand-back rather than passing on that redelivery. Its restore is registered
// before the row's intakes start, so it runs once they have stopped.
func ackWaitPastTheRun(t *testing.T) {
	t.Helper()
	was := intakeMessageTimeout
	intakeMessageTimeout = 6 * time.Hour
	t.Cleanup(func() { intakeMessageTimeout = was })
}

// loadedSettingsCache is a settings cache holding what delivery_settings holds now, read once and
// never again: what an intake under test routes by when nothing changes its settings meanwhile.
// With no row it holds nothing, as a server's cache does before the settings are first written.
func loadedSettingsCache(t *testing.T, ctx context.Context, pool *store.Pool) *SettingsCache {
	t.Helper()
	settings := newSettingsCache(pool, nil)
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

// startIntake runs intake until the test ends, and waits for Run to return before the test's own
// cleanups go on. Cancelling alone does not: Run reads the intake's package variables, and the
// next test's capHeadWriteDeliveriesAt writes one, so a Run still unwinding from the previous test
// is a data race on it.
func startIntake(t *testing.T, intake *Intake) {
	t.Helper()
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// intakeTestClient connects one NATS client for an intake test and closes it with the test.
func intakeTestClient(t *testing.T) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)
	return natsClient
}

// awaitBoundDurable waits for the intake to have created its durable and subscribed to it, so a
// test that means to publish into a bound consumer does not race the bind.
func awaitBoundDurable(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	var err error
	waitFor(t, func() bool {
		var info *natsgo.ConsumerInfo
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.MaxAckPending == intakeMaxAckPending
	}, func() string {
		return fmt.Sprintf("intake never bound its durable: %v", err)
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
