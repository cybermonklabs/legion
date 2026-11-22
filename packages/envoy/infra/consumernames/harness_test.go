package consumernames

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/natsgrant"
)

// stack is the commands' whole surrounding in a test, real but for the platform tokens: the
// writer hold's stack (holdtest.Stack: the broker's hold routes on a test Postgres, a three-server
// cluster every client of which the NATS auth service decides, and that auth service reading the
// hold row at every decision), and a Postgres database of each family store a test names, migrated
// by the stores' own migrations. Like holdtest, it skips the test when BROKER_TEST_DATABASE_URL is
// unset.
type stack struct {
	t *testing.T
	s *holdtest.Stack
	e env
	// op is the operator's reach of the hub's Kubernetes objects over the stack's cluster, as
	// writerhold's own rows build it: what env.hub answers, and what a row runs writerhold.Set on.
	op *writerhold.Operator

	mu  sync.Mutex
	dbs map[string]*pgxpool.Pool
	// conns are the connections the rows opened through the stack (operator, replaceWriter,
	// enrollDaemonConnect) that are still open: run closes them before a held command, whose
	// re-read admits no connection but its own and the auth service's (heldReread's report).
	conns []*nats.Conn
}

// stackDisks is writerhold.Disks over holdtest's disks (holdtest.NewDisks), by the disk's name.
type stackDisks struct{ *holdtest.Disks }

func (d stackDisks) DiskID(_ context.Context, disk writerhold.DiskRef) (uint64, error) {
	return d.ID(disk.Name)
}

// newStack starts a stack, stopped when t ends.
func newStack(t *testing.T) *stack {
	t.Helper()
	s := holdtest.Start(t)
	op := &writerhold.Operator{
		Hub: writerhold.KubeHub{Client: s.Kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
			ConfigMap: holdtest.ConfigMap, Forward: s.Kube},
		Disks:        stackDisks{holdtest.NewDisks(len(s.Cluster.Servers))},
		URLs:         s.Cluster.URLs(),
		Connect:      s.ConnectOperator,
		Probe:        s.DialProbe,
		RestartLimit: 3 * time.Minute,
	}
	h := &stack{t: t, s: s, op: op, dbs: map[string]*pgxpool.Pool{}}
	h.e = env{
		records: func(context.Context, options, bool) (*holdrecord.Client, error) { return s.Broker.Client(t), nil },
		connect: func(context.Context, options) (*nats.Conn, error) {
			return nats.Connect(s.Cluster.URL(), append(s.OperatorCredential(), nats.Name("envoy-infra consumer-names"),
				nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second))...)
		},
		database: func(_ context.Context, _ options, family string) (Database, func(), error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			db, ok := h.dbs[family]
			if !ok {
				return nil, nil, fmt.Errorf("the test made no database for %s", family)
			}
			return db, nil, nil
		},
		hub: func(context.Context, options) (*writerhold.Operator, error) { return op, nil },
		now: time.Now,
	}
	return h
}

// commonArgs are the flags every command takes, which the stack's env stands in for.
var commonArgs = []string{"--broker-url", "https://broker.consumernames-test", "--token-file", "/dev/null",
	"--verify-keys", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "--nats", "nats://consumernames-test:4222"}

// writingArgs are the flags every command but status takes beside commonArgs, the hold probe's
// token among them: restore --finish requires it (options.usageProblem), and every command takes
// it as one of the hold's flags (writerhold.HubFlags). The stack's env probes with the stack's own
// credential (newStack's Operator.Probe), whatever file it names.
var writingArgs = []string{"--signing-key-secret", "projects/consumernames-test/secrets/operator-only-envoy-hold-signing",
	"--probe-token-file", "/dev/null"}

// run runs `envoy-infra consumer-names <args>` in the stack, with the common flags
// (createBucketRun), and returns its exit code and output. Before a held command (every word but
// status and prepare) it closes every connection the rows opened through the stack
// (closeOperators): a row connects again after it.
func (h *stack) run(args ...string) (int, string, string) {
	h.t.Helper()
	if args[0] != actionStatus && args[0] != actionPrepare {
		h.closeOperators()
	}
	return createBucketRun(h.t, context.Background(), h.e, args...)
}

// runDialed runs `envoy-infra consumer-names <args>` as run does a held command, with ctx in place
// of the stack's and every connection the command opens dialed through dialer: a run a test cuts
// part way on the wire.
func (h *stack) runDialed(ctx context.Context, dialer nats.CustomDialer, args ...string) (int, string, string) {
	h.t.Helper()
	e := h.e
	e.connect = func(context.Context, options) (*nats.Conn, error) {
		return nats.Connect(h.s.Cluster.URL(), append(h.s.OperatorCredential(), nats.SetCustomDialer(dialer), nats.Name("envoy-infra consumer-names"),
			nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second))...)
	}
	// The held re-read's report admits no connection but the command's and the auth service's,
	// which run sees to; this run is the test's own.
	h.closeOperators()
	return createBucketRun(h.t, ctx, e, args...)
}

// closeOperators closes every connection in conns that is still open, so a held command's
// re-read finds no connection but its own and the auth service's.
func (h *stack) closeOperators() {
	h.mu.Lock()
	conns := h.conns
	h.conns = nil
	h.mu.Unlock()
	for _, nc := range conns {
		nc.Close()
	}
}

// track adds nc to conns, the connections run closes before a held command (closeOperators).
func (h *stack) track(nc *nats.Conn) {
	h.mu.Lock()
	h.conns = append(h.conns, nc)
	h.mu.Unlock()
}

// streamsDeps are what writerhold.StreamsCommandOn reaches in place of the broker, Secret Manager
// and the hub its flags name, so the writer hold's held words run on the stack: the operator's
// client of the stack's hold routes, as env.records answers it, the stack's Operator (op), the
// wall clock, and readers, the census's readers and writer.
func (h *stack) streamsDeps(readers writerhold.CensusReaders) writerhold.StreamsDeps {
	return writerhold.StreamsDeps{
		Records:       func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(h.t), nil },
		Operator:      func(context.Context) (*writerhold.Operator, error) { return h.op, nil },
		Now:           time.Now,
		CensusReaders: readers,
	}
}

// records is the operator's client of the stack's hold routes.
func (h *stack) records() *holdrecord.Client { return h.s.Records }

// row reads the hold row.
func (h *stack) row() holdrecord.Row {
	h.t.Helper()
	row, err := h.s.Records.Row(context.Background())
	require.NoError(h.t, err)
	return row
}

// prepared moves the row from released to prepare under a new hold id, with the hold's journal
// for workflow, as `envoy-infra hold prepare` writes them (its minter preflight aside), and
// returns the id.
func (h *stack) prepared(w holdrecord.Workflow) string {
	h.t.Helper()
	ctx := context.Background()
	row := h.row()
	require.Equal(h.t, natsgrant.OperatorPhaseReleased, row.Phase, "a hold is prepared only from released")
	id := nuid.Next()
	_, err := h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhasePrepare, id, "the test's hold")
	require.NoError(h.t, err)
	require.NoError(h.t, h.s.Records.StartJournal(ctx, id, w, nil))
	return id
}

// held moves a prepared hold to held the way `hold set` writes its journal (its intent with the
// row prepare, the move, its done entry), without the steps it runs against the hub: a row that
// tests what comes after a set hold rather than the steps themselves. Two of those steps' results
// a held command's re-read reads (heldReread), and held stands in for them: it makes every
// protected stream the workflow's rows do not make themselves (protectedStreams), and its done
// entry records each declared server's id now as the id after hold set's restart
// (server_id.<pod>), which the skipped restart would have recorded. A connection opened before
// held stays open, as no restart closed it: a row closes it before a held command.
func (h *stack) held(id string) {
	h.t.Helper()
	ctx := context.Background()
	w, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandSet}, nil)
	require.NoError(h.t, err)
	row := h.row()
	_, err = h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseHeld, id, "the test's hold")
	require.NoError(h.t, err)
	h.protectedStreams(w)
	declared, err := writerhold.Discover(ctx, h.op.Hub)
	require.NoError(h.t, err, "the declared servers")
	outputs := map[string]string{}
	for _, server := range declared {
		outputs["server_id."+server.Pod] = server.Varz.ServerID
	}
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, holdrecord.CommandSet, outputs))
}

// protectedStreams makes, under the held grant, every protected stream (writerhold.ProtectedStreams)
// the hub lacks, with the build's own configuration at one copy (streamset.Declared, the
// configuration `streams update` compares a deployed stream against), but three kinds: the
// notification stream and the selection bucket, which rows make with their own configuration, and
// every stream workflow w declares absent (heldExpectedAbsent). The held re-read refuses a hub that
// lacks any other.
func (h *stack) protectedStreams(w holdrecord.Workflow) {
	h.t.Helper()
	ctx := context.Background()
	skip := append([]string{bus.Stream, selectionStreamName}, heldExpectedAbsent(w)...)
	declared := streamset.Declared(streamset.Settings{Replicas: 1})
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.OperatorCredential()...)
	require.NoError(h.t, err)
	defer nc.Close()
	for _, name := range writerhold.ProtectedStreams() {
		if slices.Contains(skip, name) {
			continue
		}
		info, err := streamInfo(ctx, nc, name)
		var api *jetstream.APIError
		switch {
		case err == nil && info != nil:
			continue
		case errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeStreamNotFound:
		default:
			require.NoError(h.t, err, "STREAM.INFO of %s", name)
		}
		stream, ok := streamset.Find(declared, name)
		require.True(h.t, ok, "the build declares no configuration of the protected stream %s", name)
		_, err = apiRequest(ctx, nc, "$JS.API.STREAM.CREATE."+name, stream.Config)
		require.NoError(h.t, err, "make the protected stream %s", name)
	}
}

// heldHold is a hold of workflow, prepared and set (held).
func (h *stack) heldHold(w holdrecord.Workflow) string {
	h.t.Helper()
	id := h.prepared(w)
	h.held(id)
	return id
}

// release releases hold id the way `hold release` writes its journal and the row (its admission,
// the row's move to released, its done entry), and answers the admission's refusal: a hold whose
// journal is not complete is refused, and the row stays held.
func (h *stack) release(id string) error {
	h.t.Helper()
	ctx := context.Background()
	if _, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandRelease}, nil); err != nil {
		return err
	}
	row := h.row()
	if _, err := h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, id, "the test's release"); err != nil {
		return err
	}
	return h.s.Records.AppendDone(ctx, id, holdrecord.CommandRelease, nil)
}

// censusDone writes `streams census`'s entries in hold id's journal as its contract writes them
// around a clean run (its admission and intent, then its done entry, which completes the record of
// a cutover or streams-census hold), without the run itself: these rows test what comes before or
// after the census, and the census's own rows are admittedCensus's (completion_test.go).
func (h *stack) censusDone(id string) {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.records().Admit(ctx, id, holdrecord.Request{Command: censusCommandName}, nil)
	require.NoError(h.t, err)
	require.NoError(h.t, h.records().AppendDone(ctx, id, censusCommandName, nil))
}

// journal reads hold id's journal.
func (h *stack) journal(id string) holdrecord.Done {
	h.t.Helper()
	return readJournal(h.t, h.s.Records, id)
}

// readJournal reads hold id's journal through records.
func readJournal(t *testing.T, records *holdrecord.Client, id string) holdrecord.Done {
	t.Helper()
	d, _, err := records.Journal(context.Background(), id)
	require.NoError(t, err)
	return d
}

// lastEntry is hold id's latest journal entry of command, or of any command when command is "":
// the zero Operation when it holds none.
func (h *stack) lastEntry(id, command string) holdrecord.Operation {
	h.t.Helper()
	return lastOf(h.journal(id), command)
}

// lastOf is d's latest entry of command, or of any command when command is "": the zero
// Operation when it holds none.
func lastOf(d holdrecord.Done, command string) holdrecord.Operation {
	var last holdrecord.Operation
	for _, op := range d.Ops {
		if command == "" || op.Command == command {
			last = op
		}
	}
	return last
}

// refuseEntries is a hook of the stack's broker (holdrecordtest.Broker.Intercept) that loses each
// write of a hold's journal whose last entry is command's in one of states: the write is answered
// 503 and does not land, as a broker the command can no longer reach leaves it.
func refuseEntries(command string, states ...string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/nats-hold/records" {
			return false
		}
		body, err := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil {
			return false
		}
		var put struct {
			Kind  string          `json:"kind"`
			Value holdrecord.Done `json:"value"`
		}
		if json.Unmarshal(body, &put) != nil || put.Kind != "done" || len(put.Value.Ops) == 0 {
			return false
		}
		if last := put.Value.Ops[len(put.Value.Ops)-1]; last.Command != command || !slices.Contains(states, last.State) {
			return false
		}
		http.Error(w, "the test's broker lost this write", http.StatusServiceUnavailable)
		return true
	}
}

// requireALostDoneStops runs command, by run, twice in hold id with its done entry lost
// (refuseEntries): an exit after its intent, UNKNOWN, as the write may have landed. The first run,
// the control, is under the earlier rule that an UNKNOWN exit writes no stop
// (leftRuleSet.unknownNoStop): its intent stays pending, and hold release and hold abort are both
// refused ErrBusy. The second, under the rule, writes its stop, recording what it left as
// leftUnknownPrefix with the done entry unread: hold release is refused over it (ErrOutOfOrder), and
// hold abort admitted.
func (h *stack) requireALostDoneStops(id, command string, run func() (int, string, string)) {
	h.t.Helper()
	release, abort := holdrecord.Request{Command: holdrecord.CommandRelease}, holdrecord.Request{Command: holdrecord.CommandAbort}
	lost := func() (holdrecord.Operation, int, string) {
		h.t.Helper()
		h.s.Broker.Intercept(refuseEntries(command, "done"))
		code, _, stderr := run()
		h.s.Broker.Intercept(nil)
		require.NotZero(h.t, code, "%s exited 0 with its done entry lost", command)
		return h.lastEntry(id, command), code, stderr
	}

	leftWith(h.t, leftRuleSet{unknownNoStop: true})
	last, _, _ := lost()
	require.Equal(h.t, "intent", last.State, "control: under the earlier rule a lost done entry leaves %s's intent pending", command)
	require.ErrorIs(h.t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrBusy, "control: hold release over %s's pending intent", command)
	require.ErrorIs(h.t, holdrecord.Decide(h.journal(id), abort), holdrecord.ErrBusy, "control: hold abort over %s's pending intent", command)

	leftWith(h.t, leftRuleSet{})
	last, code, stderr := lost()
	require.Equal(h.t, "stop", last.State, "%s's lost done entry ends in its stop entry", command)
	require.Equal(h.t, 2, code, "%s with its done entry lost is UNKNOWN: %s", command, stderr)
	require.Contains(h.t, stderr, "UNKNOWN whether "+command+"'s done entry was written")
	left := last.Outputs[stopLeftKey]
	require.True(h.t, strings.HasPrefix(left, leftUnknownPrefix), "%s's stop records %q, not %s...", command, left, leftUnknownPrefix)
	require.Contains(h.t, left, "; unread whether "+command+"'s done entry was written")
	require.ErrorIs(h.t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over %s's UNKNOWN stop", command)
	require.NoError(h.t, holdrecord.Decide(h.journal(id), abort), "hold abort after %s's UNKNOWN stop", command)
}

// abortHold ends hold id the way `hold abort` writes its journal and the row: its admission, whose
// intent carries each stop's record of what its command left (returned), the row's move to
// released, and its done entry.
func (h *stack) abortHold(id string) map[string]string {
	h.t.Helper()
	ctx := context.Background()
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandAbort}, nil)
	require.NoError(h.t, err, "hold abort is admitted once a command stopped")
	intent := h.lastEntry(id, "")
	require.Equal(h.t, holdrecord.CommandAbort, intent.Command)
	row := h.row()
	_, err = h.s.Records.MoveRow(ctx, row.Gen, natsgrant.OperatorPhaseReleased, id, "the test's abort")
	require.NoError(h.t, err)
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, holdrecord.CommandAbort, nil))
	return intent.Inputs
}

// cutoverRow is the broker's cutover row of family, and false when there is none.
func (h *stack) cutoverRow(family string) (holdrecord.CutoverRow, bool) {
	h.t.Helper()
	rows, err := h.records().Cutovers(context.Background())
	require.NoError(h.t, err)
	for _, row := range rows {
		if row.Family == family {
			return row, true
		}
	}
	return holdrecord.CutoverRow{}, false
}

// consumer reads consumer name of the notification stream as the operator, under the row's phase
// now: nil when the server does not hold it.
func (h *stack) consumer(name string) *jetstream.ConsumerInfo {
	h.t.Helper()
	nc, _ := h.operator()
	defer nc.Close()
	return replaceInfo(h.t, nc, name)
}

// makeConsumer creates config on the notification stream through nc's connection, as the
// operator's or a listener's grant allows (CONSUMER.CREATE.<stream>.<name>), and answers its info.
func makeConsumer(t *testing.T, nc *nats.Conn, config jetstream.ConsumerConfig) *jetstream.ConsumerInfo {
	t.Helper()
	info, err := writeConsumer(context.Background(), nc, notifications, config, actionCreateConsumer)
	require.NoError(t, err, "create %s", config.Durable)
	return info
}

// consumersOf are the consumers of the notification stream whose logical name is one of logicals,
// in any form (the fixed name, or a fresh one cut at its last '~', selection.LogicalOf), in the
// listing's order.
func consumersOf(t *testing.T, nc *nats.Conn, logicals ...string) []string {
	t.Helper()
	var out []string
	for _, name := range censusConsumers(t, nc) {
		if slices.Contains(logicals, selection.LogicalOf(name)) {
			out = append(out, name)
		}
	}
	return out
}

// familyStore is family's store on nc, the operator's connection: the selection bucket for the
// session family, and the database the test made for any other (database).
func (h *stack) familyStore(nc *nats.Conn, family string) familyStore {
	h.t.Helper()
	if family == familySession {
		return newSessionStore(nc)
	}
	h.mu.Lock()
	pool, ok := h.dbs[family]
	h.mu.Unlock()
	require.True(h.t, ok, "the test made no store for %s", family)
	return pgFamilyOf(h.t, family, pool)
}

// pgFamilyOf is family's Postgres store on db (newPGFamilyStore), failing t when the library
// refuses to make it.
func pgFamilyOf(t testing.TB, family string, db Database) pgFamilyStore {
	t.Helper()
	store, err := newPGFamilyStore(family, db)
	require.NoError(t, err)
	return store
}

// ownSentinel is state as family's store writes it (selection.KVStore.WriteSentinel,
// selection.PGStore.WriteSentinel): under its own key, selection.SentinelKey for the session
// family and selection.PGSentinelKey of the family otherwise, and naming the family.
func ownSentinel(family string, state selection.StoreState) selection.StoreState {
	state.Key, state.Family = selection.SentinelKey, family
	if family != familySession {
		state.Key = selection.PGSentinelKey(family)
	}
	return state
}

// record reads logical's record in family's store as the operator, under the row's phase now.
func (h *stack) record(family, logical string) selection.Record {
	h.t.Helper()
	nc, _ := h.operator()
	defer nc.Close()
	record, _, err := h.familyStore(nc, family).Read(context.Background(), logical)
	require.NoError(h.t, err, "%s's record in %s's store", logical, family)
	return record
}

// writeRecord writes record under logical in store, failing t, naming logical, when the store
// refuses it.
func writeRecord(t *testing.T, store selection.Store, logical string, record selection.Record) {
	t.Helper()
	_, err := store.Write(t.Context(), logical, record, 0)
	require.NoError(t, err, "write %s", logical)
}

// operator connects to the hub as the operator, decided under the row's phase now: a test that
// changes the phase connects again for the new phase's grant. run closes it before a held command.
// Its JetStream calls a row's setup makes wait up to harnessAPITimeout, not nats.go's 5 s default:
// a three-copy bucket's create under -race on a loaded box can take longer than 5 s, and the
// setup is not what a row asserts.
func (h *stack) operator() (*nats.Conn, jetstream.JetStream) {
	h.t.Helper()
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.OperatorCredential()...)
	require.NoError(h.t, err)
	js, err := jetstream.New(nc, jetstream.WithDefaultTimeout(harnessAPITimeout))
	require.NoError(h.t, err)
	h.track(nc)
	return nc, js
}

// harnessAPITimeout bounds each JetStream call a row's setup makes through the harness's clients.
const harnessAPITimeout = time.Minute

// notificationStream creates ENVOY_NOTIFICATIONS on js with the build's configuration at one copy,
// as `envoy-infra streams update` would under a hold, and answers its info.
func (h *stack) notificationStream(js jetstream.JetStream) *jetstream.StreamInfo {
	h.t.Helper()
	stream, err := js.CreateStream(context.Background(), notificationStreamConfig(h.t, 1))
	require.NoError(h.t, err)
	return stream.CachedInfo()
}

// notificationStreamConfig is the build's notification stream configuration at replicas copies,
// in jetstream's form.
func notificationStreamConfig(t testing.TB, replicas int) jetstream.StreamConfig {
	t.Helper()
	raw, err := json.Marshal(bus.NotificationStreamConfig(replicas, 0))
	require.NoError(t, err)
	var config jetstream.StreamConfig
	require.NoError(t, json.Unmarshal(raw, &config))
	return config
}

// The migrations of the families' stores, by the store's own files, so a test's table is the one
// each store's migration makes.
var (
	dispatchMigrations = []string{
		"../../internal/dispatch/store/migrations/20261101143201_duty_holds.up.sql",
		"../../internal/dispatch/store/migrations/20261121002206_intake_consumers.up.sql",
	}
	daemonMigrations = []string{
		"../../../daemon/internal/store/migrations/0063_consumer_positions.up.sql",
		"../../../daemon/internal/store/migrations/20261121001600_intake_consumers.up.sql",
	}
)

// database makes family's store a database of its own on the test's Postgres server
// (migratedDatabase), and answers its pool, which the stack's env hands the commands.
func (h *stack) database(family string, migrations ...string) *pgxpool.Pool {
	h.t.Helper()
	_, pool := migratedDatabase(h.t, "cnames", migrations...)
	h.mu.Lock()
	h.dbs[family] = pool
	h.mu.Unlock()
	return pool
}

// migratedDatabase makes a database of the test's own on the test's Postgres server
// (BROKER_TEST_DATABASE_URL), its name beginning prefix, runs each of migrations in it in order
// (none: a store with no table), and answers its URL and its pool, closed when t ends.
func migratedDatabase(t *testing.T, prefix string, migrations ...string) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	url := testdb.DatabaseOn(t, os.Getenv("BROKER_TEST_DATABASE_URL"), prefix)
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	for _, file := range migrations {
		sql, err := os.ReadFile(filepath.FromSlash(file))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(t, err, "migrate the store with %s", file)
	}
	return url, pool
}
