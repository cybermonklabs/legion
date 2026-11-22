package consumernames

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// censusReaders are the census's readers and writer as cmd/envoy-infra passes them to
// writerhold.StreamsCommand: this package's reader of the recorded session consumers
// (RecordedSessionConsumers), of the recorded fresh names (RecordedFreshNames) and its writer of
// the daemon stores' stream ids (ReconcileDaemonStreamIDs).
func censusReaders() writerhold.CensusReaders {
	return writerhold.CensusReaders{Recorded: RecordedSessionConsumers, FreshNames: RecordedFreshNames, Stores: ReconcileDaemonStreamIDs}
}

// censusCommand runs `envoy-infra streams census --hold holdID` in the stack with this package's
// readers (censusReaders) and extra, the census's own flags: --family, --listener-machine-id,
// --daemon-consumer, --dispatch-database-file and --daemon-database. It returns the exit code and
// the output.
func (h *stack) censusCommand(holdID string, extra ...string) (int, string, string) {
	h.t.Helper()
	return h.censusCommandWith(censusReaders(), holdID, extra...)
}

// censusCommandWith is censusCommand with readers as the census's readers and writer; a nil one
// makes the census UNKNOWN (writerhold.CensusReaders).
//
// It is t-10's word itself, writerhold.StreamsCommandOn on the stack's hold routes and hub
// (streamsDeps), with the hold's flags, whose values it does not read. The word runs inside its
// hold (writerhold.RunHeld): it writes its intent, then its done entry on a clean run or its stop
// entry otherwise, and its held re-read and postcheck admit no connection to the hub but its own
// and the auth service's, so this first closes every connection the rows opened through the stack
// (closeOperators), as run does: a row connects again after it.
func (h *stack) censusCommandWith(readers writerhold.CensusReaders, holdID string, extra ...string) (int, string, string) {
	h.t.Helper()
	h.closeOperators()
	args := append([]string{"census", "--hold", holdID, "--reason", "the row's streams census"}, extra...)
	args = append(append(args, commonArgs...), writingArgs...)
	var stdout, stderr bytes.Buffer
	code := writerhold.StreamsCommandOn(context.Background(), args, &stdout, &stderr, h.streamsDeps(readers))
	h.t.Logf("envoy-infra streams census %s: exit %d\n%s%s", strings.Join(extra, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// censusStreamID is the notification stream's identity in the census rows
// (censusNotificationStream).
const censusStreamID = "the-census-notification-stream"

// censusNotificationStream creates ENVOY_NOTIFICATIONS on js with the build's configuration at one
// copy, as notificationStream does, and with the stream's identity
// (selection.StreamIdentityMetadataKey), as the listener writes it when it creates the stream: the
// census's writer of the daemon stores' stream ids (ReconcileDaemonStreamIDs), which the word runs
// after a clean census, refuses a stream that carries none.
func censusNotificationStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	config := notificationStreamConfig(t, 1)
	config.Metadata = map[string]string{selection.StreamIdentityMetadataKey: censusStreamID}
	_, err := js.CreateStream(t.Context(), config)
	require.NoError(t, err)
}

// censusDatabaseFile writes the URL of the store on pool, a database the stack made (database),
// to a file of the test's, as the census's --dispatch-database-file and --daemon-database name one:
// the census's readers open each store from its file (openDatabase), where the stack's commands
// take the pool itself.
func censusDatabaseFile(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "store.url")
	require.NoError(t, os.WriteFile(file, []byte(pool.Config().ConnString()+"\n"), 0o600))
	return file
}

// censusSessionConsumer creates, as the operator, session sessionID's consumer name on the
// notification stream as the listener's fresh mode creates it (bus.SessionConsumers freshCreate
// and applySettings): a pull consumer acknowledged one at a time, filtered on filters, carrying
// the session in its metadata.
func censusSessionConsumer(t *testing.T, nc *nats.Conn, sessionID, name string, filters []string) {
	t.Helper()
	makeConsumer(t, nc, jetstream.ConsumerConfig{
		Durable: name, AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1, MaxAckPending: bus.SessionConsumerMaxAckPending,
		AckWait: bus.SessionConsumerAckWait, InactiveThreshold: time.Hour, FilterSubjects: filters,
		Metadata: map[string]string{selection.LogicalMetadataKey: sessionID, bus.SessionIDMetadataKey: sessionID},
	})
}

// censusListenerDurable creates, as the operator, a listener's push durable name as
// cmd/listener's fresh mode creates it (listener_durable_names.go, applyListenerConsumerPolicy):
// every notification subject the build carries, delivered to a fresh reply inbox, acknowledged
// one at a time.
func censusListenerDurable(t *testing.T, nc *nats.Conn, durable, name string) {
	t.Helper()
	makeConsumer(t, nc, jetstream.ConsumerConfig{
		Durable: name, DeliverSubject: nats.NewInbox(), FilterSubjects: bus.StreamSubjects(), AckPolicy: jetstream.AckExplicitPolicy,
		Metadata: map[string]string{selection.LogicalMetadataKey: durable},
	})
}

// daemonConsumer creates, as the operator on nc, a consumer named name of the notification stream
// carrying logical as its logical name, as a daemon creates one.
func daemonConsumer(t *testing.T, nc *nats.Conn, name, logical string) {
	t.Helper()
	makeConsumer(t, nc, jetstream.ConsumerConfig{Durable: name, FilterSubject: bus.LegionWakeTopic,
		AckPolicy: jetstream.AckExplicitPolicy, Metadata: map[string]string{selection.LogicalMetadataKey: logical}})
}

// censusConsumers are the notification stream's consumer names.
func censusConsumers(t *testing.T, nc *nats.Conn) []string {
	t.Helper()
	names, err := consumerNames(t.Context(), nc, notifications)
	require.NoError(t, err)
	return names
}

// The streams census command (t-10's word, with this package's readers), given no reader of the
// recorded consumers, exits 2 printing UNKNOWN, removes nothing, and writes its stop entry,
// recording only this hold's barrier key, which it put before it reached the reader (a stop names
// every change its command's runs made and did not undo). With this package's reader it runs
// complete, in a held hold whose session family's cutover row is done, over a bucket holding three
// sessions' records: it keeps each recorded session~<nuid> consumer, whose filter holds its
// session's agent subject and a topic beside it, a retired one that cleanup has not yet deleted, the
// fresh listener durable that the listener's durable.<escaped durable> record names, and the fresh
// listener durable of a machine no --listener-machine-id names, which its record alone names (the
// recorded fresh names keep a listener-<m>~<nuid> only through a declared listener-<m>, so
// RecordedSessionConsumers' listener shapes keep this one). It removes a stray listener-<m>~<nuid>
// that no record names and an unrecorded session~<nuid> of a recorded session, in that session's
// own shape, exits 1 and writes its stop entry naming each removal. Control: a recorded
// session~<nuid> whose filter does not hold its session's agent subject is removed, so a recorded
// name is compared by its shape and never by name alone. A second run is clean, and its done entry
// completes the hold's record, the census being the streams-census workflow's final command.
func TestTheCensusReadsTheRecordedSessionConsumers(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})

	agent := func(sessionID string) []string {
		return bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject(sessionID), "notifications.github.pr.owner.repo.7"})
	}
	kept := map[string]string{}
	for _, sessionID := range []string{"ses_a", "ses_b", "ses_c"} {
		name := selection.FreshName(sessionFreshPrefix)
		censusSessionConsumer(t, nc, sessionID, name, agent(sessionID))
		record := selection.Record{Current: name, Start: 1}
		if sessionID == "ses_a" {
			retired := selection.FreshName(sessionFreshPrefix)
			censusSessionConsumer(t, nc, sessionID, retired, agent(sessionID))
			record.Retired = []string{retired}
			kept[retired] = "the retired name of ses_a, which cleanup deletes and the census keeps until then"
		}
		_, err := store.Write(ctx, sessionID, record, 0)
		require.NoError(t, err)
		kept[name] = "the current of " + sessionID
	}
	durable := bus.ListenerDurable("hub")
	listener := selection.FreshName(durable)
	censusListenerDurable(t, nc, durable, listener)
	_, err = store.Write(ctx, selection.DurableKey(durable), selection.Record{Current: listener, Start: 1}, 0)
	require.NoError(t, err)
	kept[listener] = "the listener durable its record names"
	undeclared := bus.ListenerDurable("undeclared")
	undeclaredListener := selection.FreshName(undeclared)
	censusListenerDurable(t, nc, undeclared, undeclaredListener)
	_, err = store.Write(ctx, selection.DurableKey(undeclared), selection.Record{Current: undeclaredListener, Start: 1}, 0)
	require.NoError(t, err)
	kept[undeclaredListener] = "the listener durable of a machine no --listener-machine-id names, which its record names"
	stray := selection.FreshName(durable)
	censusListenerDurable(t, nc, durable, stray)
	unrecordedSession := selection.FreshName(sessionFreshPrefix)
	censusSessionConsumer(t, nc, "ses_b", unrecordedSession, agent("ses_b"))
	planted := selection.FreshName(sessionFreshPrefix)
	censusSessionConsumer(t, nc, "ses_d", planted, []string{"notifications.dispatch.issue.PRIV.>"})
	_, err = store.Write(ctx, "ses_d", selection.Record{Current: planted, Start: 1}, 0)
	require.NoError(t, err)
	_, err = h.records().PutCutover(ctx, familySession, selection.CutoverDone, id)
	require.NoError(t, err)

	flags := []string{"--family", familySession, "--listener-machine-id", "hub"}
	noRecorded := censusReaders()
	noRecorded.Recorded = nil
	code, stdout, stderr := h.censusCommandWith(noRecorded, id, flags...)
	require.Equal(t, 2, code, "the census with no reader")
	require.Contains(t, stdout, "UNKNOWN")
	require.Contains(t, stderr, writerhold.ErrNoRecordReader.Error())
	last := h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" stop", last.Command+" "+last.State, "the census with no reader ends in its stop entry")
	require.Equal(t, barrierLeft, last.Outputs["left"], "the stop records the barrier key the census put, and nothing more")
	nc, _ = h.operator()
	require.Contains(t, censusConsumers(t, nc), stray, "a census with no reader removed a consumer")

	code, stdout, _ = h.censusCommand(id, flags...)
	require.Equal(t, 1, code, "the census with the reader is complete and removes the stray: %s", stdout)
	require.NotContains(t, stdout, "UNKNOWN")
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+stray+": removed")
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+planted+": removed", "control: the recorded name with another filter")
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+unrecordedSession+": removed", "the unrecorded session consumer")
	last = h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" stop", last.Command+" "+last.State, "the census that removed consumers ends in its stop entry")
	for _, removed := range []string{stray, planted, unrecordedSession} {
		require.Contains(t, last.Outputs["left"], removed, "the stop entry names each removal")
	}
	nc, _ = h.operator()
	left := censusConsumers(t, nc)
	for name, what := range kept {
		require.Contains(t, left, name, "the census removed %s", what)
	}
	require.NotContains(t, left, stray)
	require.NotContains(t, left, planted)
	require.NotContains(t, left, unrecordedSession)

	code, stdout, _ = h.censusCommand(id, flags...)
	require.Equal(t, 0, code, "the second census: %s", stdout)
	require.True(t, slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN"), stdout)
	last = h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" done", last.Command+" "+last.State)
	require.True(t, h.journal(id).Complete, "the clean census's done entry completes the streams-census hold's record")
}

// The census reads a listener durable's record key back to the durable it was made from
// (selection.DurableKey), escapes included, and takes no other key for a durable's: a session id,
// the sentinel, a barrier key, and a key DurableKey would not write (lowercase hex, a byte it
// writes plain written escaped, a cut escape).
func TestTheCensusReadsADurableKeyBack(t *testing.T) {
	for _, durable := range []string{"listener-hub", "listener-a.b", "listener-a b", "listener-ünï~x"} {
		got, ok := censusDurableOf(selection.DurableKey(durable))
		require.True(t, ok, "the key of %q", durable)
		require.Equal(t, durable, got)
	}
	for _, key := range []string{"ses_a", selection.SentinelKey, "hold.barrier.h1", "durable.listener-a=2eb", "durable.listener=2Dhub",
		"durable.listener-a=2", "durable."} {
		_, ok := censusDurableOf(key)
		require.False(t, ok, "%q read as a durable's key", key)
	}
}

// censusDaemonStore makes daemon project's store a database of the test's own (censusStore) and
// answers the --daemon-database value naming it, <project>=<file>, and the store's pool.
func censusDaemonStore(t *testing.T, project string, migrations ...string) (string, *pgxpool.Pool) {
	t.Helper()
	file, pool := censusStore(t, project, migrations...)
	return project + "=" + file, pool
}

// censusInstance is the Cloud SQL instance connection name the rows' stores are on: the second
// line of each store's file (censusStore), and the store-instance annotation of each daemon's
// Deployment (censusDeploymentOf).
const censusInstance = "local:postgres:census-rows"

// censusStore makes a family's store a database of the test's own on the test's Postgres server,
// migrated by each of migrations (none: a store with no table), writes its URL to a file as the
// operator's --dispatch-database-file and --daemon-database name one, with censusInstance on its
// second line, the instance the operator's file states, and answers the file and the store's
// pool. It skips the test when BROKER_TEST_DATABASE_URL is unset, as the stack does.
func censusStore(t *testing.T, name string, migrations ...string) (string, *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("BROKER_TEST_DATABASE_URL") == "" {
		t.Skip("BROKER_TEST_DATABASE_URL is unset: the family stores are Postgres databases")
	}
	url, pool := migratedDatabase(t, "cnames", migrations...)
	file := filepath.Join(t.TempDir(), name+".url")
	require.NoError(t, os.WriteFile(file, []byte(url+"\n"+censusInstance+"\n"), 0o600))
	return file, pool
}

// censusDeployment is project's daemon Deployment as the deployment renders it, annotated with its
// project key, the database its store is (none when database is "") and the instance that holds
// it (none when instance is "").
func censusDeployment(project, database, instance string) writerhold.DaemonDeployment {
	annotations := map[string]string{writerhold.DaemonProjectAnnotation: project}
	if database != "" {
		annotations[writerhold.DaemonStoreDatabaseAnnotation] = database
	}
	if instance != "" {
		annotations[writerhold.DaemonStoreInstanceAnnotation] = instance
	}
	return writerhold.DaemonDeployment{Name: "legion-daemon-" + strings.ToLower(project), Annotations: annotations}
}

// censusDeploymentOf is project's daemon Deployment for the store on pool, on censusInstance.
func censusDeploymentOf(project string, pool *pgxpool.Pool) writerhold.DaemonDeployment {
	return censusDeployment(project, pool.Config().ConnConfig.Database, censusInstance)
}

// censusListing is a listing of the daemons' Deployments that answers deployments.
func censusListing(deployments ...writerhold.DaemonDeployment) writerhold.DaemonDeployments {
	return func(context.Context) ([]writerhold.DaemonDeployment, error) { return deployments, nil }
}

// censusDeploy renders d in the stack's Kubernetes, namespace writerhold.DaemonNamespace, where the
// census's word lists the daemons' Deployments (writerhold.KubeDaemonDeployments of the hold's own
// client).
func (h *stack) censusDeploy(d writerhold.DaemonDeployment) {
	h.t.Helper()
	_, err := h.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace).Create(h.t.Context(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: writerhold.DaemonNamespace, Annotations: d.Annotations},
	}, metav1.CreateOptions{})
	require.NoError(h.t, err)
}

// censusDaemonDeployed renders project's daemon Deployment for the store on pool in the stack's
// Kubernetes (censusDeploy, censusDeploymentOf), and answers that store's file with censusInstance
// on its second line: the census's word reads a daemon store only at the address its Deployment
// names.
func (h *stack) censusDaemonDeployed(project string, pool *pgxpool.Pool) string {
	h.t.Helper()
	file := filepath.Join(h.t.TempDir(), "store.url")
	require.NoError(h.t, os.WriteFile(file, []byte(pool.Config().ConnString()+"\n"+censusInstance+"\n"), 0o600))
	h.censusDeploy(censusDeploymentOf(project, pool))
	return file
}

// censusSentinel reads the sentinel row of family's store on pool: its state, through the store,
// and its revision.
func censusSentinel(t *testing.T, family string, pool *pgxpool.Pool) (selection.StoreState, uint64) {
	t.Helper()
	var revision int64
	require.NoError(t, pool.QueryRow(t.Context(), `select revision from `+selection.IntakeConsumersTable+` where logical = $1`,
		selection.PGSentinelKey(family)).Scan(&revision))
	state, err := pgFamilyOf(t, family, pool).State(t.Context())
	require.NoError(t, err)
	return state, uint64(revision)
}

// censusRefuseWrites makes pool's intake_consumers rows refuse every insert and update, by a trigger
// that raises "the row refuses writes": a store whose write fails.
func censusRefuseWrites(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `create function refuse_writes() returns trigger language plpgsql as $$
begin
	raise exception 'the row refuses writes';
end $$;
create trigger refuse_writes before insert or update on `+selection.IntakeConsumersTable+` for each row execute function refuse_writes()`)
	require.NoError(t, err)
}

// censusHeldOperator is a hub of one server holding ENVOY_NOTIFICATIONS with identity, and the
// operator's connection to it under exactly natsgrant.Operator(held), with its admin's JetStream.
func censusHeldOperator(t *testing.T, identity string) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	url := statusWalkServer(t, "operator", natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	stream := bus.NotificationStreamConfig(1, 0)
	stream.Metadata = map[string]string{selection.StreamIdentityMetadataKey: identity}
	_, err = js.AddStream(&stream)
	require.NoError(t, err)
	conn, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "operator"), nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn, js
}

// The census's writer of the daemon stores' stream identity (ReconcileDaemonStreamIDs), as the
// operator under exactly natsgrant.Operator(held), reads ENVOY_NOTIFICATIONS' identity and every
// daemon store the census names, each at the address its one source names (its Deployment, or a
// retired project's entry), and rewrites, by a write conditional on its revision, each sentinel
// whose stream_id differs from it: a stale one
// and one that recorded none are rewritten, every other field kept, and each is named in the
// result's Changed; an equal one is left at its revision, named in Reconciled with its identity;
// a store with no sentinel yet (a store enrol has not written) is left without one, named in
// Reconciled with none. A second run changes nothing and names every store in Reconciled. A store
// it cannot read is named in the result's Unreconciled with why, and it then writes nothing, so
// the stale store is in Unreconciled too, not written: a store with no table, and a declared
// daemon family whose store no --daemon-database names. Each of those results places every
// daemon family in one list (writerhold.StreamIDResult.Covers). A malformed --daemon-database, a
// listing of the Deployments it cannot read and a stream that carries no identity fail the whole
// call. Controls: in each of those runs the stale store is still stale afterwards, so the writer
// that does not write reads before it writes; and the first result without Reconciled, as the
// writer answered before it, fails Covers for the equal store.
func TestTheCensusReconcilesEachDaemonStoresStreamID(t *testing.T) {
	ctx := t.Context()
	conn, js := censusHeldOperator(t, "stream-2")

	sentinel := func(project string, pool *pgxpool.Pool, state selection.StoreState) {
		t.Helper()
		_, err := pgFamilyOf(t, daemonFamily(project), pool).WriteSentinel(ctx, state, 0)
		require.NoError(t, err)
	}
	stale, staleDB := censusDaemonStore(t, "STALE", daemonMigrations...)
	sentinel("STALE", staleDB, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1", Hold: "h0"})
	equal, equalDB := censusDaemonStore(t, "EQUAL", daemonMigrations...)
	sentinel("EQUAL", equalDB, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, StreamID: "stream-2"})
	unrecorded, unrecordedDB := censusDaemonStore(t, "UNREC", daemonMigrations...)
	sentinel("UNREC", unrecordedDB, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 5})
	fresh, freshDB := censusDaemonStore(t, "FRESH", daemonMigrations...)
	noTable, noTableDB := censusDaemonStore(t, "NOTABLE")
	listing := censusListing(censusDeploymentOf("STALE", staleDB), censusDeploymentOf("EQUAL", equalDB), censusDeploymentOf("UNREC", unrecordedDB),
		censusDeploymentOf("FRESH", freshDB), censusDeploymentOf("NOTABLE", noTableDB))
	families := []string{familySession, familyDispatch, daemonFamily("STALE"), daemonFamily("EQUAL")}
	stored := append(slices.Clone(families), daemonFamily("UNREC"), daemonFamily("FRESH"))

	result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, equal, unrecorded, fresh})
	require.NoError(t, err)
	require.Equal(t, writerhold.StreamIDResult{
		Changed: []writerhold.StreamIDChange{
			{Project: "STALE", From: "stream-1", To: "stream-2"}, {Project: "UNREC", From: "", To: "stream-2"},
		},
		Reconciled: []writerhold.ReconciledStore{{Project: "EQUAL", StreamID: "stream-2"}, {Project: "FRESH"}},
	}, result)
	require.NoError(t, result.Covers(stored))
	before := result
	before.Reconciled = nil
	require.ErrorContains(t, before.Covers(families), daemonFamily("EQUAL")+" is in none of", "control: the result without Reconciled")
	state, revision := censusSentinel(t, daemonFamily("STALE"), staleDB)
	require.Equal(t, ownSentinel(daemonFamily("STALE"), selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-2", Hold: "h0"}), state)
	require.Equal(t, uint64(2), revision, "the stale sentinel's rewrite")
	state, revision = censusSentinel(t, daemonFamily("EQUAL"), equalDB)
	require.Equal(t, "stream-2", state.StreamID)
	require.Equal(t, uint64(1), revision, "the equal sentinel was written")
	state, _ = censusSentinel(t, daemonFamily("UNREC"), unrecordedDB)
	require.Equal(t, ownSentinel(daemonFamily("UNREC"), selection.StoreState{Cutover: selection.CutoverDone, Boundary: 5, StreamID: "stream-2"}), state)
	_, err = pgFamilyOf(t, daemonFamily("FRESH"), freshDB).State(ctx)
	require.ErrorIs(t, err, selection.ErrNoSentinel, "the writer made a sentinel for a store with none")

	result, err = ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, equal, unrecorded, fresh})
	require.NoError(t, err)
	require.Equal(t, writerhold.StreamIDResult{Reconciled: []writerhold.ReconciledStore{
		{Project: "EQUAL", StreamID: "stream-2"}, {Project: "FRESH"}, {Project: "STALE", StreamID: "stream-2"}, {Project: "UNREC", StreamID: "stream-2"},
	}}, result, "the second run")
	require.NoError(t, result.Covers(stored))
	_, revision = censusSentinel(t, daemonFamily("STALE"), staleDB)
	require.Equal(t, uint64(2), revision)

	_, err = pgFamilyOf(t, daemonFamily("STALE"), staleDB).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1", Hold: "h0"}, 2)
	require.NoError(t, err)
	stillStale := func(what string) {
		t.Helper()
		state, _ := censusSentinel(t, daemonFamily("STALE"), staleDB)
		require.Equal(t, "stream-1", state.StreamID, "control: %s wrote the stale store", what)
	}
	result, err = ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, equal, noTable})
	require.NoError(t, err)
	require.Equal(t, writerhold.StreamIDResult{
		Reconciled: []writerhold.ReconciledStore{{Project: "EQUAL", StreamID: "stream-2"}},
		Unreconciled: []writerhold.UnreconciledStore{
			{Project: "NOTABLE", Reason: "it has no intake_consumers table"},
			{Project: "STALE", Reason: "not written, since no store is written while any is unreconciled (daemon/NOTABLE)"},
		},
	}, result)
	require.NoError(t, result.Covers(append(slices.Clone(families), daemonFamily("NOTABLE"))))
	stillStale("a run with a store that has no table")

	unnamed := append(slices.Clone(families), daemonFamily("UNNAMED"))
	result, err = ReconcileDaemonStreamIDs(ctx, conn, listing, unnamed, []string{stale, equal})
	require.NoError(t, err)
	require.Equal(t, writerhold.StreamIDResult{
		Reconciled: []writerhold.ReconciledStore{{Project: "EQUAL", StreamID: "stream-2"}},
		Unreconciled: []writerhold.UnreconciledStore{
			{Project: "STALE", Reason: "not written, since no store is written while any is unreconciled (daemon/UNNAMED)"},
			{Project: "UNNAMED", Reason: "no --daemon-database UNNAMED=<file> names it"},
		},
	}, result)
	require.NoError(t, result.Covers(unnamed))
	stillStale("a run with a declared family whose store no flag names")

	_, err = ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, "EQUAL"})
	require.ErrorContains(t, err, `"EQUAL"`)
	stillStale("a run with a malformed --daemon-database")

	refused := func(context.Context) ([]writerhold.DaemonDeployment, error) {
		return nil, errors.New("deployments is forbidden")
	}
	_, err = ReconcileDaemonStreamIDs(ctx, conn, refused, families, []string{stale, equal})
	require.ErrorContains(t, err, "deployments is forbidden")
	stillStale("a run whose listing of the Deployments was refused")

	info, err := js.StreamInfo(bus.Stream)
	require.NoError(t, err)
	anonymous := info.Config
	anonymous.Metadata = map[string]string{}
	_, err = js.UpdateStream(&anonymous)
	require.NoError(t, err)
	_, err = ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, equal})
	require.ErrorContains(t, err, selection.StreamIdentityMetadataKey)
	stillStale("a run on a stream with no identity")
}

// censusDroppedDatabase is the name of a database no server holds, which the server answers
// SQLSTATE 3D000 for.
func censusDroppedDatabase() string {
	return "cnames_dropped_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// censusStoreFileFor writes a store's file as --daemon-database names one, and answers
// <project>=<file>: its first line the database URL of pool with its database replaced by
// database, or, when database is "", with its port replaced by one nothing listens on and its
// database by censusUnreachableDatabase (a store whose database the server answers 3D000 for, or
// one whose server never answers, at an address of its own: no two daemons' Deployments name one
// database), and its second line instance, the instance the operator states, none when instance
// is "".
func censusStoreFileFor(t *testing.T, project string, pool *pgxpool.Pool, database, instance string) string {
	t.Helper()
	config := pool.Config().ConnConfig.Copy()
	if database != "" {
		config.Database = database
	} else {
		config.Port = censusClosedPort(t)
		config.Database = censusUnreachableDatabase
	}
	url := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&connect_timeout=2",
		config.User, config.Password, net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), config.Database)
	content := url + "\n"
	if instance != "" {
		content += instance + "\n"
	}
	file := filepath.Join(t.TempDir(), "store.url")
	require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
	return project + "=" + file
}

// censusUnreachableDatabase is the database the file of a store whose server never answers names
// (censusStoreFileFor), which that store's Deployment names too.
const censusUnreachableDatabase = "cnames_unreachable"

// censusClosedPort is a TCP port on the loopback that nothing listens on: one a listener held a
// moment ago and gave back.
func censusClosedPort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	require.NoError(t, l.Close())
	return port
}

// censusAnsweringServer is a server on the loopback that answers every connection's startup
// message with Postgres's FATAL ErrorResponse of SQLSTATE code (pgproto3), as a server answers a
// new connection while it shuts down (57P01 admin_shutdown, 57P03 cannot_connect_now) or to a
// database it does not hold (3D000), and answers the --daemon-database value naming it for
// project, <project>=<file>: the file's first line a URL of database on the server's port, with no
// TLS, so the startup message is the first thing sent, and its second line censusInstance.
func censusAnsweringServer(t *testing.T, project, database, code string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code,
					Message: "the row's server answers SQLSTATE " + code})
				_ = backend.Flush()
			}()
		}
	}()
	url := fmt.Sprintf("postgres://operator:secret@%s/%s?sslmode=disable&connect_timeout=2", l.Addr(), database)
	file := filepath.Join(t.TempDir(), "store.url")
	require.NoError(t, os.WriteFile(file, []byte(url+"\n"+censusInstance+"\n"), 0o600))
	return project + "=" + file
}

// A daemon store whose server answers 57P01 (admin_shutdown) or 57P03 (cannot_connect_now), as a
// dying server answers for a database it still holds in the seconds after its instance is deleted,
// is unreconciled, never gone: the census's writer of the stream identity (ReconcileDaemonStreamIDs)
// answers it in Unreconciled with the server's answer and nothing in Gone, and its reader of the
// recorded names (ReadRecordedNames) fails naming it, with no gone store. Controls: the same
// server answering 3D000 makes the store gone, so the row's server is one the rule reads; and each
// answer is a *pgconn.PgError, so a check on "is a PgError" alone would call each store gone (the
// overlay run with that check fails this row).
func TestTheCensusLeavesAShuttingDownStoreUnreconciled(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	// The reader of the recorded names reads the selection bucket too, which the held operator makes
	// as create-bucket does.
	_, err := selection.CreateSessionBucket(ctx, conn, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	const database = "daemon_down"
	listing := censusListing(censusDeployment("DOWN", database, censusInstance))
	families := []string{daemonFamily("DOWN")}
	pgErrorOf := func(t *testing.T, entry string) *pgconn.PgError {
		t.Helper()
		raw, err := os.ReadFile(strings.TrimPrefix(entry, "DOWN="))
		require.NoError(t, err)
		url, _, _ := strings.Cut(string(raw), "\n")
		_, err = pgx.Connect(ctx, url)
		var pg *pgconn.PgError
		require.True(t, errors.As(err, &pg), "control: the server's answer is no PgError: %v", err)
		return pg
	}
	for _, code := range []string{"57P01", "57P03"} {
		t.Run(code, func(t *testing.T) {
			entry := censusAnsweringServer(t, "DOWN", database, code)
			require.Equal(t, code, pgErrorOf(t, entry).Code, "control: a check on \"is a PgError\" alone would call the store gone")
			result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{entry})
			require.NoError(t, err)
			require.Empty(t, result.Gone, "a %s answer was called gone", code)
			require.Len(t, result.Unreconciled, 1, "%+v", result)
			require.Equal(t, "DOWN", result.Unreconciled[0].Project)
			require.Contains(t, result.Unreconciled[0].Reason, "SQLSTATE "+code)
			require.NoError(t, result.Covers(families))
			names, gone, err := ReadRecordedNames(ctx, conn, listing, families, "", []string{entry})
			require.ErrorContains(t, err, "SQLSTATE "+code)
			require.Empty(t, gone, "the recorded names' reader called a %s store gone", code)
			require.Nil(t, names)
		})
	}
	t.Run("control: 3D000 from the same server", func(t *testing.T) {
		entry := censusAnsweringServer(t, "DOWN", database, censusUndefinedDatabase)
		result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{entry})
		require.NoError(t, err)
		require.Empty(t, result.Unreconciled, "%+v", result)
		require.Len(t, result.Gone, 1)
		require.Equal(t, "DOWN", result.Gone[0].Project)
		require.Contains(t, result.Gone[0].Answer, "SQLSTATE 3D000")
	})
}

// The census's readers of the daemon stores refuse a store address, a database on an instance,
// that two daemons' Deployments name: with both daemons' files reaching that one database, the
// writer of the stream identity (ReconcileDaemonStreamIDs) leaves both stores unreconciled, each
// naming both Deployments and their projects, and writes nothing, and the reader of the recorded
// names (ReadRecordedNames) fails naming both. Control: the same stores with each Deployment
// naming its own database, which the writer rewrites, both, and the reader reads. The row is red
// at the rule without the shared-address check: there the first store is rewritten and the second
// reads as consistent, one database counted as two daemons'.
func TestTheCensusRefusesAStoreAddressTwoDeploymentsName(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	_, err := selection.CreateSessionBucket(ctx, conn, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	first, firstDB := censusDaemonStore(t, "FIRST", daemonMigrations...)
	second, secondDB := censusDaemonStore(t, "SECOND", daemonMigrations...)
	stale := selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}
	for project, pool := range map[string]*pgxpool.Pool{"FIRST": firstDB, "SECOND": secondDB} {
		_, err := pgFamilyOf(t, daemonFamily(project), pool).WriteSentinel(ctx, stale, 0)
		require.NoError(t, err)
	}
	families := []string{daemonFamily("FIRST"), daemonFamily("SECOND")}
	database := firstDB.Config().ConnConfig.Database
	sharing := censusStoreFileFor(t, "SECOND", firstDB, database, censusInstance)
	shared := censusListing(censusDeploymentOf("FIRST", firstDB), censusDeployment("SECOND", database, censusInstance))

	result, err := ReconcileDaemonStreamIDs(ctx, conn, shared, families, []string{first, sharing})
	require.NoError(t, err)
	require.Empty(t, result.Changed, "a store two Deployments name was written")
	require.Empty(t, result.Gone)
	require.Empty(t, result.Reconciled, "a store two Deployments name was read as consistent")
	require.Len(t, result.Unreconciled, 2, "%+v", result)
	for i, project := range []string{"FIRST", "SECOND"} {
		require.Equal(t, project, result.Unreconciled[i].Project)
		for _, named := range []string{"legion/legion-daemon-first (project FIRST)", "legion/legion-daemon-second (project SECOND)",
			fmt.Sprintf("each name database %q on instance %q", database, censusInstance)} {
			require.Contains(t, result.Unreconciled[i].Reason, named, "%s's reason", project)
		}
	}
	require.NoError(t, result.Covers(families))
	state, _ := censusSentinel(t, daemonFamily("FIRST"), firstDB)
	require.Equal(t, "stream-1", state.StreamID, "a run over a shared address wrote the store")
	_, _, err = ReadRecordedNames(ctx, conn, shared, families, "", []string{first, sharing})
	require.ErrorContains(t, err, "project FIRST")
	require.ErrorContains(t, err, "project SECOND")
	require.ErrorContains(t, err, "no one daemon's")

	// Control: each Deployment names its own database.
	own := censusListing(censusDeploymentOf("FIRST", firstDB), censusDeploymentOf("SECOND", secondDB))
	result, err = ReconcileDaemonStreamIDs(ctx, conn, own, families, []string{first, second})
	require.NoError(t, err)
	require.Equal(t, []writerhold.StreamIDChange{{Project: "FIRST", From: "stream-1", To: "stream-2"}, {Project: "SECOND", From: "stream-1", To: "stream-2"}},
		result.Changed, "control: %+v", result)
	names, _, err := ReadRecordedNames(ctx, conn, own, families, "", []string{first, second})
	require.NoError(t, err, "control: the reader over two addresses")
	require.Contains(t, names, daemonFamily("FIRST"))
	require.Contains(t, names, daemonFamily("SECOND"))
}

// A daemon store whose database the server answers SQLSTATE 3D000 for (the database does not
// exist), at the address its Deployment names (the database and instance it annotates, which the
// operator's file names and states), is gone: no daemon can use its floors, so the census's writer
// of the daemon stores' stream identity (ReconcileDaemonStreamIDs) answers it in the result's Gone,
// with that answer, its database, the instance its Deployment names and the instance the
// operator's file states, and reconciles the store beside it. A store whose server only does not
// answer is no gone one: it is unreconciled, naming why, and nothing is written, so the stale
// store beside it is unreconciled too, not written. Control (red at the writer before the
// gone-store rule): that writer counted the gone store as unread too, and failed the whole call
// with nothing written.
func TestTheCensusReportsAGoneDaemonStoreAndKeepsAnUnreachableOne(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	stale, staleDB := censusDaemonStore(t, "STALE", daemonMigrations...)
	_, err := pgFamilyOf(t, daemonFamily("STALE"), staleDB).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}, 0)
	require.NoError(t, err)
	dropped := censusDroppedDatabase()
	gone := censusStoreFileFor(t, "GONE", staleDB, dropped, censusInstance)
	unreachable := censusStoreFileFor(t, "AWAY", staleDB, "", censusInstance)
	listing := censusListing(censusDeploymentOf("STALE", staleDB), censusDeployment("GONE", dropped, censusInstance),
		censusDeployment("AWAY", censusUnreachableDatabase, censusInstance))

	result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, []string{daemonFamily("STALE"), daemonFamily("GONE")}, []string{stale, gone})
	require.NoError(t, err, "a gone store fails the call")
	require.Equal(t, []writerhold.StreamIDChange{{Project: "STALE", From: "stream-1", To: "stream-2"}}, result.Changed)
	require.Empty(t, result.Unreconciled)
	require.Empty(t, result.Reconciled)
	require.Len(t, result.Gone, 1)
	require.Equal(t, writerhold.GoneStore{Project: "GONE", Database: dropped, Instance: censusInstance, StatedInstance: censusInstance,
		Answer: result.Gone[0].Answer}, result.Gone[0])
	require.Contains(t, result.Gone[0].Answer, "SQLSTATE 3D000", "the gone store carries the server's answer")
	require.Contains(t, result.Gone[0].Answer, dropped)
	state, _ := censusSentinel(t, daemonFamily("STALE"), staleDB)
	require.Equal(t, "stream-2", state.StreamID, "the reachable store beside a gone one was reconciled")

	_, err = pgFamilyOf(t, daemonFamily("STALE"), staleDB).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}, 2)
	require.NoError(t, err)
	away := []string{daemonFamily("STALE"), daemonFamily("AWAY")}
	result, err = ReconcileDaemonStreamIDs(ctx, conn, listing, away, []string{stale, unreachable})
	require.NoError(t, err)
	require.Empty(t, result.Gone, "an unreachable store is no gone one")
	require.Empty(t, result.Changed)
	require.Empty(t, result.Reconciled)
	require.Len(t, result.Unreconciled, 2, "%+v", result.Unreconciled)
	require.Equal(t, "AWAY", result.Unreconciled[0].Project)
	require.NotContains(t, result.Unreconciled[0].Reason, "3D000")
	require.Equal(t, writerhold.UnreconciledStore{Project: "STALE", Reason: "not written, since no store is written while any is unreconciled (daemon/AWAY)"},
		result.Unreconciled[1])
	require.NoError(t, result.Covers(away))
	state, _ = censusSentinel(t, daemonFamily("STALE"), staleDB)
	require.Equal(t, "stream-1", state.StreamID, "a run with an unreachable store wrote nothing")
}

// The census's writer of the daemon stores' stream identity (ReconcileDaemonStreamIDs) writes the
// stale stores one at a time, in the order of their project keys, after it has read every one. A
// write that fails after an earlier store's write landed leaves that store written, and the result
// says so: the written store in Changed, the store whose write failed in Unreconciled with the
// failure, and each stale store after it in Unreconciled, not written. The second store's
// intake_consumers refuses every write through a trigger, which its read does not meet, so the
// writer reads all three stores whole and fails only at that write. Control: the stores' own rows
// afterwards, the first at the stream's identity and the other two still at their stale one, so
// the result names what the stores hold.
func TestTheCensusAnswersTheStoreItChangedBeforeAWriteFailed(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	stale := selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}
	projects := []string{"FIRST", "REFUSED", "THIRD"}
	pools := map[string]*pgxpool.Pool{}
	var entries, families []string
	var deployments []writerhold.DaemonDeployment
	for _, project := range projects {
		entry, pool := censusDaemonStore(t, project, daemonMigrations...)
		_, err := pgFamilyOf(t, daemonFamily(project), pool).WriteSentinel(ctx, stale, 0)
		require.NoError(t, err)
		pools[project] = pool
		entries = append(entries, entry)
		families = append(families, daemonFamily(project))
		deployments = append(deployments, censusDeploymentOf(project, pool))
	}
	censusRefuseWrites(t, pools["REFUSED"])

	result, err := ReconcileDaemonStreamIDs(ctx, conn, censusListing(deployments...), families, entries)
	require.NoError(t, err, "a write that fails after another landed is the result's, not the call's")
	require.Equal(t, []writerhold.StreamIDChange{{Project: "FIRST", From: "stream-1", To: "stream-2"}}, result.Changed,
		"the store written before the failed write")
	require.Empty(t, result.Gone)
	require.Empty(t, result.Reconciled)
	require.Len(t, result.Unreconciled, 2, "%+v", result.Unreconciled)
	require.Equal(t, "REFUSED", result.Unreconciled[0].Project)
	require.Contains(t, result.Unreconciled[0].Reason, selection.PGSentinelKey(daemonFamily("REFUSED"))+" row's write failed")
	require.Contains(t, result.Unreconciled[0].Reason, "the row refuses writes", "the failed write's reason")
	require.Equal(t, writerhold.UnreconciledStore{Project: "THIRD", Reason: "not written, since daemon/REFUSED's write failed before it"},
		result.Unreconciled[1])
	require.NoError(t, result.Covers(families))
	state, _ := censusSentinel(t, daemonFamily("FIRST"), pools["FIRST"])
	require.Equal(t, "stream-2", state.StreamID, "control: the first store's write did not land")
	for _, project := range projects[1:] {
		state, _ := censusSentinel(t, daemonFamily(project), pools[project])
		require.Equal(t, "stream-1", state.StreamID, "control: %s's sentinel was written", project)
	}
}

// Every result the census's writer of the daemon stores' stream identity
// (ReconcileDaemonStreamIDs) answers without an error places each daemon family it was given in
// exactly one of its lists (writerhold.StreamIDResult.Covers), the decision a hub-restore's abort
// reads: a store already at the stream's identity in Reconciled with it, and one with no sentinel
// yet in Reconciled with none; a stale store it wrote in Changed; a gone one in Gone; and one
// whose write failed in Unreconciled, with the stale store after it, not written, while the stores
// after it that are already consistent are still in Reconciled. With a store it cannot reach it
// writes nothing: that store and the stale one are in Unreconciled, and the consistent and gone
// ones keep their lists. Controls: each result without Reconciled, as the writer answered before
// it, fails Covers naming the consistent store; and the stale store is still stale after the run
// that could not reach a store.
func TestTheCensusPlacesEachDaemonFamilyInOneList(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	stale := selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}
	consistent := selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, StreamID: "stream-2"}
	pools := map[string]*pgxpool.Pool{}
	entries := map[string]string{}
	deployments := map[string]writerhold.DaemonDeployment{}
	for project, state := range map[string]*selection.StoreState{"EQUAL": &consistent, "FIRST": &stale, "REFUSED": &stale, "THIRD": &stale, "UNSET": nil, "WHOLE": &consistent} {
		entry, pool := censusDaemonStore(t, project, daemonMigrations...)
		if state != nil {
			_, err := pgFamilyOf(t, daemonFamily(project), pool).WriteSentinel(ctx, *state, 0)
			require.NoError(t, err)
		}
		pools[project], entries[project], deployments[project] = pool, entry, censusDeploymentOf(project, pool)
	}
	censusRefuseWrites(t, pools["REFUSED"])
	dropped := censusDroppedDatabase()
	entries["GONE"], deployments["GONE"] = censusStoreFileFor(t, "GONE", pools["EQUAL"], dropped, censusInstance), censusDeployment("GONE", dropped, censusInstance)
	entries["AWAY"], deployments["AWAY"] = censusStoreFileFor(t, "AWAY", pools["EQUAL"], "", censusInstance), censusDeployment("AWAY", censusUnreachableDatabase, censusInstance)
	listing := func(context.Context) ([]writerhold.DaemonDeployment, error) {
		var listed []writerhold.DaemonDeployment
		for _, d := range deployments {
			listed = append(listed, d)
		}
		return listed, nil
	}
	of := func(projects ...string) (families, daemonDatabases []string) {
		families = []string{familySession, familyDispatch}
		for _, project := range projects {
			families = append(families, daemonFamily(project))
			daemonDatabases = append(daemonDatabases, entries[project])
		}
		return families, daemonDatabases
	}

	families, daemonDatabases := of("EQUAL", "FIRST", "GONE", "REFUSED", "THIRD", "UNSET", "WHOLE")
	result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, families, daemonDatabases)
	require.NoError(t, err)
	require.Len(t, result.Gone, 1)
	require.Contains(t, result.Gone[0].Answer, "SQLSTATE 3D000")
	require.Len(t, result.Unreconciled, 2, "%+v", result.Unreconciled)
	require.Contains(t, result.Unreconciled[0].Reason, "the row refuses writes")
	require.Equal(t, writerhold.StreamIDResult{
		Changed: []writerhold.StreamIDChange{{Project: "FIRST", From: "stream-1", To: "stream-2"}},
		Gone: []writerhold.GoneStore{{Project: "GONE", Database: dropped, Instance: censusInstance, StatedInstance: censusInstance,
			Answer: result.Gone[0].Answer}},
		Reconciled: []writerhold.ReconciledStore{{Project: "EQUAL", StreamID: "stream-2"}, {Project: "UNSET"}, {Project: "WHOLE", StreamID: "stream-2"}},
		Unreconciled: []writerhold.UnreconciledStore{
			{Project: "REFUSED", Reason: result.Unreconciled[0].Reason},
			{Project: "THIRD", Reason: "not written, since daemon/REFUSED's write failed before it"},
		},
	}, result)
	require.NoError(t, result.Covers(families))
	before := result
	before.Reconciled = nil
	require.ErrorContains(t, before.Covers(families), daemonFamily("EQUAL")+" is in none of", "control: the result without Reconciled")

	// A store it cannot reach: nothing written, the stale store unreconciled, not written.
	_, err = pgFamilyOf(t, daemonFamily("FIRST"), pools["FIRST"]).WriteSentinel(ctx, stale, 2)
	require.NoError(t, err)
	families, daemonDatabases = of("AWAY", "EQUAL", "FIRST", "GONE")
	result, err = ReconcileDaemonStreamIDs(ctx, conn, listing, families, daemonDatabases)
	require.NoError(t, err)
	require.Len(t, result.Gone, 1)
	require.Len(t, result.Unreconciled, 2, "%+v", result.Unreconciled)
	require.Equal(t, writerhold.StreamIDResult{
		Gone:       []writerhold.GoneStore{{Project: "GONE", Database: dropped, Instance: censusInstance, StatedInstance: censusInstance, Answer: result.Gone[0].Answer}},
		Reconciled: []writerhold.ReconciledStore{{Project: "EQUAL", StreamID: "stream-2"}},
		Unreconciled: []writerhold.UnreconciledStore{
			{Project: "AWAY", Reason: result.Unreconciled[0].Reason},
			{Project: "FIRST", Reason: "not written, since no store is written while any is unreconciled (daemon/AWAY)"},
		},
	}, result)
	require.NoError(t, result.Covers(families))
	before = result
	before.Reconciled = nil
	require.ErrorContains(t, before.Covers(families), daemonFamily("EQUAL")+" is in none of", "control: the result without Reconciled")
	state, _ := censusSentinel(t, daemonFamily("FIRST"), pools["FIRST"])
	require.Equal(t, "stream-1", state.StreamID, "control: a run with a store it cannot reach wrote the stale store")
}

// A 3D000 calls a daemon store gone only at the address its Deployment names. Each case is a
// store whose database the server answers 3D000 for, so a writer without the address check would
// call each gone; with it, each is unreconciled, naming why, nothing is gone, and the stale store
// beside it is not written, and is unreconciled for that: a Deployment with no store-instance
// annotation, one with no store-database annotation, a 3D000 from another instance (the operator's
// file states an instance the Deployment does not name), a file with no instance line, a
// Deployment naming another database, no Deployment for the project, and two of them. Control:
// each case fails against the writer with the address check taken out (census.go's
// censusReadDaemonStore).
func TestTheCensusCallsAStoreGoneOnlyAtItsDeploymentsAddress(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	stale, staleDB := censusDaemonStore(t, "STALE", daemonMigrations...)
	_, err := pgFamilyOf(t, daemonFamily("STALE"), staleDB).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "stream-1"}, 0)
	require.NoError(t, err)
	dropped := censusDroppedDatabase()
	other := censusDeployment("GONE", dropped, censusInstance)
	other.Name = "legion-daemon-gone-copy"
	for _, row := range []struct {
		name, instance string
		deployments    []writerhold.DaemonDeployment
		reason         string
	}{
		{"a Deployment with no store-instance annotation", censusInstance,
			[]writerhold.DaemonDeployment{censusDeployment("GONE", dropped, "")}, "has no annotation " + writerhold.DaemonStoreInstanceAnnotation},
		{"a Deployment with no store-database annotation", censusInstance,
			[]writerhold.DaemonDeployment{censusDeployment("GONE", "", censusInstance)}, "has no annotation " + writerhold.DaemonStoreDatabaseAnnotation},
		{"a 3D000 from another instance", "local:postgres:elsewhere",
			[]writerhold.DaemonDeployment{censusDeployment("GONE", dropped, censusInstance)}, `states instance "local:postgres:elsewhere"`},
		{"a file with no instance line", "",
			[]writerhold.DaemonDeployment{censusDeployment("GONE", dropped, censusInstance)}, "states no instance on its second line"},
		{"a Deployment naming another database", censusInstance,
			[]writerhold.DaemonDeployment{censusDeployment("GONE", "daemon_other", censusInstance)}, `names database "daemon_other"`},
		{"no Deployment for the project", censusInstance, nil, "0 Deployments of namespace legion name project GONE"},
		{"two Deployments for the project", censusInstance,
			[]writerhold.DaemonDeployment{censusDeployment("GONE", dropped, censusInstance), other}, "2 Deployments of namespace legion name project GONE"},
	} {
		t.Run(row.name, func(t *testing.T) {
			gone := censusStoreFileFor(t, "GONE", staleDB, dropped, row.instance)
			listing := censusListing(append([]writerhold.DaemonDeployment{censusDeploymentOf("STALE", staleDB)}, row.deployments...)...)
			families := []string{daemonFamily("STALE"), daemonFamily("GONE")}
			result, err := ReconcileDaemonStreamIDs(ctx, conn, listing, families, []string{stale, gone})
			require.NoError(t, err)
			require.Empty(t, result.Gone, "a 3D000 away from the Deployment's address was called gone")
			require.Empty(t, result.Changed)
			require.Len(t, result.Unreconciled, 2, "%+v", result.Unreconciled)
			require.Equal(t, "GONE", result.Unreconciled[0].Project)
			require.Contains(t, result.Unreconciled[0].Reason, row.reason)
			require.Equal(t, writerhold.UnreconciledStore{Project: "STALE", Reason: "not written, since no store is written while any is unreconciled (daemon/GONE)"},
				result.Unreconciled[1])
			require.NoError(t, result.Covers(families))
			state, _ := censusSentinel(t, daemonFamily("STALE"), staleDB)
			require.Equal(t, "stream-1", state.StreamID, "a run with a store unreconciled wrote the stale store")
		})
	}
}

// The census's readers keep the address check for a daemon store's sentinel too: a store whose
// sentinel row names another family than the store's, or none, is unreconciled, naming the family
// it names, and nothing is written. The check is the family's store's own
// (selection.PGStore.State, its selection.ErrSentinelFamily). Control: the reconcile with the check
// taken out (census.go's censusReadDaemonStore and censusReadSentinel reading the row without the
// store's check) rewrites the other family's sentinel.
func TestTheCensusRefusesAStoreOfAnotherFamily(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	daemon, pool := censusDaemonStore(t, "OWN", daemonMigrations...)
	_, err := pgFamilyOf(t, daemonFamily("OWN"), pool).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, StreamID: "stream-1"}, 0)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `update `+selection.IntakeConsumersTable+` set record = record || '{"family":"daemon/ELSE"}'::jsonb where logical = $1`, selection.PGSentinelKey(daemonFamily("OWN")))
	require.NoError(t, err)
	result, err := ReconcileDaemonStreamIDs(ctx, conn, censusListing(censusDeploymentOf("OWN", pool)), []string{daemonFamily("OWN")}, []string{daemon})
	require.NoError(t, err)
	require.Empty(t, result.Changed)
	require.Len(t, result.Unreconciled, 1)
	require.Contains(t, result.Unreconciled[0].Reason, `names the family "daemon/ELSE"`)
	var record []byte
	require.NoError(t, pool.QueryRow(ctx, `select record from `+selection.IntakeConsumersTable+` where logical = $1`, selection.PGSentinelKey(daemonFamily("OWN"))).Scan(&record))
	var state selection.StoreState
	require.NoError(t, json.Unmarshal(record, &state))
	require.Equal(t, "stream-1", state.StreamID, "the other family's sentinel was written")
}

// A family's database file is read by its lines (familyDatabaseFile): the first is the URL every
// command opens, the second, when there is one, the instance the operator's proxy reaches; a line
// ending of either form and the lines after the second are read alike. A file whose first line is
// empty is refused rather than opened on an empty URL, and so is a file no flag names.
func TestADatabaseFileIsReadByItsLines(t *testing.T) {
	dir := t.TempDir()
	file := func(content string) options {
		path := filepath.Join(dir, strconv.Itoa(len(content))+".url")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return options{daemonDatabases: []string{"P=" + path}}
	}
	for content, want := range map[string]databaseFile{
		"postgres://a/b":                           {url: "postgres://a/b"},
		"postgres://a/b\n":                         {url: "postgres://a/b"},
		"postgres://a/b\r\nproj:region:inst\r\n":   {url: "postgres://a/b", instance: "proj:region:inst"},
		"postgres://a/b\nproj:region:inst\nmore\n": {url: "postgres://a/b", instance: "proj:region:inst"},
	} {
		got, err := familyDatabaseFile(file(content), daemonFamily("P"))
		require.NoError(t, err, "%q", content)
		want.flag = "--daemon-database P=<file>"
		require.Equal(t, want, got, "%q", content)
	}
	_, err := familyDatabaseFile(file("\npostgres://a/b\n"), daemonFamily("P"))
	require.ErrorContains(t, err, "first line holds no database URL")
	_, err = familyDatabaseFile(options{}, daemonFamily("P"))
	require.ErrorContains(t, err, "needs --daemon-database P=<file>")
}

// The census's reader of every family's recorded names (ReadRecordedNames), as the operator under
// exactly natsgrant.Operator(held), answers one set per declared family, each exactly the names
// its records hold (a current, an open operation's candidate, a retired name), so an unrecorded
// <family>~<nuid> is in none: the session consumers and the listener durables from the selection
// bucket, whose sentinel and barrier key name nothing; Dispatch's from its intake_consumers, whose
// sentinel row names nothing; and each daemon's from its own, read at the address its Deployment
// names, a declared daemon whose store holds no record answering an empty set rather than none.
// An open operation's predecessor is no recorded name: a convert's reserve whose current is empty
// names the fixed name there, which the set leaves out. A run that declares only the session
// family reads only the bucket. A daemon store whose database the server answers 3D000 for, at its
// Deployment's address, is gone: it has no set, it is answered beside the names, and the census's
// fresh names (RecordedFreshNames) name each of its daemon's consumers to leave, with why.
// Control: a store it cannot read fails the run, naming it (a declared Dispatch with no
// --dispatch-database-file, a daemon store with no table, a declared daemon no --daemon-database
// names, and a 3D000 away from the Deployment's address), and so does a bucket holding a deleted
// session record or a durable record of no listener, naming each.
func TestTheRecordedNamesOfEveryFamily(t *testing.T) {
	ctx := t.Context()
	url := statusWalkServer(t, "operator", natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	kv, err := selection.CreateSessionBucket(ctx, admin, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	statusPublishKV(t, admin, "hold.barrier.h1", []byte("h1"), "")
	conn, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "operator"), nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	set := func(names ...string) map[string]bool {
		out := map[string]bool{}
		for _, name := range names {
			out[name] = true
		}
		return out
	}
	fresh := selection.FreshName

	s0, s1, s2 := fresh(sessionFreshPrefix), fresh(sessionFreshPrefix), fresh(sessionFreshPrefix)
	writeRecord(t, kv, "ses_a", selection.Record{Current: s1, Start: 4, Retired: []string{s0},
		Op: &selection.Op{Kind: selection.OpReplace, Candidate: s2, Old: s1, ReservedAt: time.Now()}})
	s4 := fresh(sessionFreshPrefix)
	writeRecord(t, kv, "ses_e", selection.Record{Start: 4, Op: &selection.Op{Kind: selection.OpReplace, Old: sessionFixedPrefix + "ses_e", Candidate: s4, ReservedAt: time.Now()}})
	l0, l1 := fresh(bus.ListenerDurable("hub")), fresh(bus.ListenerDurable("hub"))
	writeRecord(t, kv, selection.DurableKey(bus.ListenerDurable("hub")), selection.Record{Current: l1, Start: 4, Retired: []string{l0}})
	dispatchFile, dispatchDB := censusStore(t, familyDispatch, dispatchMigrations...)
	dispatch := pgFamilyOf(t, familyDispatch, dispatchDB)
	_, err = dispatch.WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone}, 0)
	require.NoError(t, err)
	d0, d1, d2 := fresh("delivery-events"), fresh("delivery-events"), fresh("delivery-events")
	writeRecord(t, dispatch, "delivery-events", selection.Record{Current: d1, Start: 4, Retired: []string{d0},
		Op: &selection.Op{Kind: selection.OpReplace, Candidate: d2, Old: d1, ReservedAt: time.Now()}})
	daemon, daemonDB := censusDaemonStore(t, "PROJ", daemonMigrations...)
	w0, w1 := fresh("legion-go-PROJ-wake"), fresh("legion-go-PROJ-wake")
	writeRecord(t, pgFamilyOf(t, daemonFamily("PROJ"), daemonDB), "legion-go-PROJ-wake", selection.Record{Current: w1, Start: 4, Retired: []string{w0}})
	empty, emptyDB := censusDaemonStore(t, "EMPTY", daemonMigrations...)
	noTable, noTableDB := censusDaemonStore(t, "NOTABLE")
	dropped := censusDroppedDatabase()
	gone := censusStoreFileFor(t, "GONE", daemonDB, dropped, censusInstance)
	listing := censusListing(censusDeploymentOf("PROJ", daemonDB), censusDeploymentOf("EMPTY", emptyDB), censusDeploymentOf("NOTABLE", noTableDB),
		censusDeployment("GONE", dropped, censusInstance))
	families := []string{familySession, familyDispatch, daemonFamily("PROJ"), daemonFamily("EMPTY")}

	names, goneStores, err := ReadRecordedNames(ctx, conn, listing, families, dispatchFile, []string{daemon, empty})
	require.NoError(t, err)
	require.Equal(t, RecordedNames{
		recordedSessions: set(s0, s1, s2, s4), recordedListeners: set(l0, l1), familyDispatch: set(d0, d1, d2), daemonFamily("PROJ"): set(w0, w1),
		daemonFamily("EMPTY"): set(),
	}, names)
	require.Empty(t, goneStores)
	families = families[:3]
	names, _, err = ReadRecordedNames(ctx, conn, nil, []string{familySession}, "", nil)
	require.NoError(t, err)
	require.Equal(t, RecordedNames{recordedSessions: set(s0, s1, s2, s4), recordedListeners: set(l0, l1)}, names, "a run that declares the session family alone")

	names, goneStores, err = ReadRecordedNames(ctx, conn, listing, append(slices.Clone(families), daemonFamily("GONE")), dispatchFile, []string{daemon, gone})
	require.NoError(t, err, "a gone daemon store fails the read")
	require.NotContains(t, names, daemonFamily("GONE"), "a gone store has a set")
	require.Equal(t, set(w0, w1), names[daemonFamily("PROJ")])
	require.Len(t, goneStores, 1)
	require.Equal(t, "GONE", goneStores[0].Project)
	require.Contains(t, goneStores[0].Answer, "SQLSTATE 3D000")
	read, err := RecordedFreshNames(ctx, conn, listing, append(slices.Clone(families), daemonFamily("GONE")), dispatchFile, []string{daemon, gone})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{w0, w1}, read.Names[daemonFamily("PROJ")])
	why := "left, store gone: daemon/GONE's database " + dropped + " on instance " + censusInstance + " does not exist"
	require.Equal(t, map[string]string{"legion-go-GONE-dispatch": why, "legion-go-GONE-github": why, "legion-go-GONE-wake": why, "legion-go-GONE-ready": why}, read.Gone)

	_, _, err = ReadRecordedNames(ctx, conn, listing, families, "", []string{daemon})
	require.ErrorContains(t, err, "dispatch's store", "control: a declared Dispatch with no store file")
	_, _, err = ReadRecordedNames(ctx, conn, listing, families, dispatchFile, []string{daemon, noTable})
	require.ErrorContains(t, err, "daemon/NOTABLE", "control: a daemon store with no table")
	_, _, err = ReadRecordedNames(ctx, conn, listing, append(slices.Clone(families), daemonFamily("UNNAMED")), dispatchFile, []string{daemon})
	require.ErrorContains(t, err, "daemon/UNNAMED", "control: a declared daemon no flag names")
	elsewhere := censusStoreFileFor(t, "GONE", daemonDB, dropped, "local:postgres:elsewhere")
	_, _, err = ReadRecordedNames(ctx, conn, listing, families, dispatchFile, []string{daemon, elsewhere})
	require.ErrorContains(t, err, "daemon/GONE", "control: a 3D000 away from the Deployment's address")
	require.ErrorContains(t, err, `states instance "local:postgres:elsewhere"`)

	writeRecord(t, kv, selection.DurableKey("delivery-planted"), selection.Record{Current: fresh("delivery-planted")})
	statusPublishKV(t, admin, "ses_a", nil, "DEL")
	_, _, err = ReadRecordedNames(ctx, conn, listing, families, dispatchFile, []string{daemon})
	require.ErrorContains(t, err, "ses_a", "control: a deleted session record")
	require.ErrorContains(t, err, selection.DurableKey("delivery-planted"), "control: a durable record of no listener")
}

// censusCreateConfig creates, as the operator on nc, a consumer of the notification stream with a
// configuration in nats.go's older form (bus.DispatchDeliveryConsumer gives that form), through the
// one create the operator's grant names (writeConsumer).
func censusCreateConfig(t *testing.T, nc *nats.Conn, config nats.ConsumerConfig) {
	t.Helper()
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	var converted jetstream.ConsumerConfig
	require.NoError(t, json.Unmarshal(raw, &converted))
	makeConsumer(t, nc, converted)
}

// A census whose reader cannot read a declared family's store is UNKNOWN, exit 2, naming the
// family, removes nothing, and writes its stop entry, recording only this hold's barrier key: the
// census declares the session family and daemon/PROJ, and no --daemon-database names PROJ's store,
// so the recorded fresh names (RecordedFreshNames), which the census reads behind its barrier, are
// refused before it lists anything, and the stray listener-hub~<nuid> stays. Its standard output
// holds no CLEAN. Control: with the store named, the same census runs complete and removes the
// stray.
func TestTheCensusIsUnknownWhenADeclaredFamilyIsUnread(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	stray := selection.FreshName(bus.ListenerDurable("hub"))
	censusListenerDurable(t, nc, bus.ListenerDurable("hub"), stray)
	for _, family := range []string{familySession, daemonFamily("PROJ")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
	}
	flags := []string{"--family", familySession, "--family", daemonFamily("PROJ"), "--listener-machine-id", "hub"}

	code, stdout, stderr := h.censusCommand(id, flags...)
	require.Equal(t, 2, code, "a census with a declared family unread: %s%s", stdout, stderr)
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, daemonFamily("PROJ"))
	require.NotContains(t, stdout, "CHANGED")
	require.NotContains(t, stdout, "CLEAN", "an UNKNOWN census printed a report it did not make")
	last := h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" stop", last.Command+" "+last.State, "the UNKNOWN census ends in its stop entry")
	require.Equal(t, barrierLeft, last.Outputs["left"], "the stop records the barrier key the census put, and nothing more")
	nc, _ = h.operator()
	require.Contains(t, censusConsumers(t, nc), stray, "an UNKNOWN census removed the stray")

	daemon, daemonDB := censusDaemonStore(t, "PROJ", daemonMigrations...)
	h.censusDeploy(censusDeploymentOf("PROJ", daemonDB))
	code, stdout, _ = h.censusCommand(id, append(flags, "--daemon-database", daemon)...)
	require.Equal(t, 1, code, "control: the census with every declared family read: %s", stdout)
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+stray+": removed")
}

// The census declares the session family, Dispatch and daemon/PROJ, each with its records:
// Dispatch's intake_consumers names delivery-events~<nuid> and PROJ's names
// legion-go-PROJ-wake~<nuid>, each a consumer in its family's own shape. The word passes the census
// the recorded fresh names (RecordedFreshNames) of every store its flags name, so the census keeps
// each recorded name and removes a stray delivery-events~<nuid> and a stray
// legion-go-PROJ-wake~<nuid> that no record names, in the same shapes, naming each removal; a
// second run is clean, and its done entry completes the hold's record. Control: the same census
// with no --dispatch-database-file cannot read a declared family's records, so it is UNKNOWN and
// removes neither stray.
func TestTheCensusRemovesAStrayDispatchAndDaemonName(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)

	events := bus.DispatchDeliveryConsumer()
	recordedD, strayD := selection.FreshName(events.Durable), selection.FreshName(events.Durable)
	for _, name := range []string{recordedD, strayD} {
		config := events
		config.Durable, config.DeliverSubject = name, nats.NewInbox()
		config.Metadata = map[string]string{selection.LogicalMetadataKey: events.Durable}
		censusCreateConfig(t, nc, config)
	}
	dispatchFile, dispatchDB := censusStore(t, familyDispatch, dispatchMigrations...)
	_, err = pgFamilyOf(t, familyDispatch, dispatchDB).Write(ctx, events.Durable, selection.Record{Current: recordedD, Start: 1}, 0)
	require.NoError(t, err)

	const wake = "legion-go-PROJ-wake"
	recordedW, strayW := selection.FreshName(wake), selection.FreshName(wake)
	for _, name := range []string{recordedW, strayW} {
		daemonConsumer(t, nc, name, wake)
	}
	daemon, daemonDB := censusDaemonStore(t, "PROJ", daemonMigrations...)
	h.censusDeploy(censusDeploymentOf("PROJ", daemonDB))
	_, err = pgFamilyOf(t, daemonFamily("PROJ"), daemonDB).Write(ctx, wake, selection.Record{Current: recordedW, Start: 1}, 0)
	require.NoError(t, err)

	flags := []string{"--daemon-consumer", wake + "=" + bus.LegionWakeTopic, "--daemon-database", daemon}
	for _, family := range []string{familySession, familyDispatch, daemonFamily("PROJ")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
		flags = append(flags, "--family", family)
	}

	code, stdout, stderr := h.censusCommand(id, flags...)
	require.Equal(t, 2, code, "control: the census with Dispatch's records unread: %s%s", stdout, stderr)
	require.Contains(t, stderr, familyDispatch+"'s store")
	require.NotContains(t, stdout, "CHANGED")
	nc, _ = h.operator()
	left := censusConsumers(t, nc)
	require.Contains(t, left, strayD, "control: an UNKNOWN census removed the stray Dispatch name")
	require.Contains(t, left, strayW, "control: an UNKNOWN census removed the stray daemon name")

	flags = append(flags, "--dispatch-database-file", dispatchFile)
	code, stdout, _ = h.censusCommand(id, flags...)
	require.Equal(t, 1, code, "the census removes the strays: %s", stdout)
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+strayD+": removed", "the stray Dispatch name")
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+strayW+": removed", "the stray daemon name")
	nc, _ = h.operator()
	left = censusConsumers(t, nc)
	require.Contains(t, left, recordedD)
	require.Contains(t, left, recordedW)
	require.NotContains(t, left, strayD)
	require.NotContains(t, left, strayW)
	code, stdout, _ = h.censusCommand(id, flags...)
	require.Equal(t, 0, code, "the second census: %s", stdout)
	require.True(t, slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN"), stdout)
	require.True(t, h.journal(id).Complete, "the clean census's done entry completes the streams-census hold's record")
}

// The census through its word, beside a daemon whose store is gone (its database answers 3D000 at
// the address its Deployment names): it removes none of that daemon's consumers, a fresh one of a
// declared shape and one of a kind no --daemon-consumer declares alike, and names each as left
// (`LEFT ENVOY_NOTIFICATIONS consumer <name>: left, store gone: ...`), neither a change nor a
// failure; it goes on to its verdict otherwise, removing a stray listener durable (exit 1), and its
// clean run prints `GONE daemon/GONE: database <db> on instance <instance> (the operator's file
// states <instance>): <answer>` and names the store in its done entry, gone.daemon/GONE. Controls:
// first, an unconfirmed 3D000 (the Deployment names another instance than the operator's file
// states) leaves the census UNKNOWN, removing nothing, with a stop that records only this hold's
// barrier key, which it put before it read the fresh names; and the census that deletes a gone
// store's consumers (streams' goneName taken out) fails this row.
func TestTheCensusLeavesAGoneDaemonsConsumers(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	stray := selection.FreshName(bus.ListenerDurable("hub"))
	censusListenerDurable(t, nc, bus.ListenerDurable("hub"), stray)
	const wake, ready = "legion-go-GONE-wake", "legion-go-GONE-ready"
	declared, undeclared := selection.FreshName(wake), selection.FreshName(ready)
	for name, logical := range map[string]string{declared: wake, undeclared: ready} {
		daemonConsumer(t, nc, name, logical)
	}
	for _, family := range []string{familySession, daemonFamily("GONE")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
	}
	_, server := censusStore(t, "SERVER")
	dropped := censusDroppedDatabase()
	deployment := censusDeployment("GONE", dropped, "local:postgres:elsewhere")
	h.censusDeploy(deployment)
	flags := []string{"--family", familySession, "--family", daemonFamily("GONE"), "--listener-machine-id", "hub",
		"--daemon-consumer", wake + "=" + bus.LegionWakeTopic, "--daemon-database", censusStoreFileFor(t, "GONE", server, dropped, censusInstance)}
	consumers := func() []string {
		nc, _ := h.operator()
		return censusConsumers(t, nc)
	}

	// Control: the Deployment names another instance than the operator's file states.
	code, stdout, stderr := h.censusCommand(id, flags...)
	require.Equal(t, 2, code, "a census with an unconfirmed 3D000: %s%s", stdout, stderr)
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, daemonFamily("GONE")+`'s store: --daemon-database GONE=<file> states instance "`+censusInstance+`"`)
	require.NotContains(t, stdout, "CHANGED")
	require.NotContains(t, stdout, "LEFT")
	require.NotContains(t, stdout, "GONE daemon/")
	require.Equal(t, barrierLeft, h.lastEntry(id, "").Outputs["left"], "the stop records the barrier key the census put, and nothing more")
	require.Subset(t, consumers(), []string{stray, declared, undeclared}, "an UNKNOWN census removed a consumer")

	deployments := h.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace)
	rendered, err := deployments.Get(ctx, deployment.Name, metav1.GetOptions{})
	require.NoError(t, err)
	rendered.Annotations[writerhold.DaemonStoreInstanceAnnotation] = censusInstance
	_, err = deployments.Update(ctx, rendered, metav1.UpdateOptions{})
	require.NoError(t, err)
	left := func(stdout string) {
		t.Helper()
		why := "left, store gone: daemon/GONE's database " + dropped + " on instance " + censusInstance + " does not exist"
		for _, name := range []string{declared, undeclared} {
			require.Contains(t, stdout, "LEFT "+notifications+" consumer "+name+": "+why+"\n")
			require.NotContains(t, stdout, "CHANGED "+notifications+" consumer "+name+": removed")
		}
		require.Subset(t, consumers(), []string{declared, undeclared}, "the census removed a gone daemon's consumer")
	}

	code, stdout, stderr = h.censusCommand(id, flags...)
	require.Equal(t, 1, code, "the census beside a gone store removes the stray: %s%s", stdout, stderr)
	require.Contains(t, stdout, "CHANGED "+notifications+" consumer "+stray+": removed")
	left(stdout)
	require.NotContains(t, consumers(), stray)
	last := h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" stop", last.Command+" "+last.State)
	require.Contains(t, last.Outputs["left"], stray)
	require.NotContains(t, last.Outputs["left"], declared, "the stop names a gone daemon's consumer as removed")

	code, stdout, stderr = h.censusCommand(id, flags...)
	require.Equal(t, 0, code, "the second census: %s%s", stdout, stderr)
	left(stdout)
	gone := "database " + dropped + " on instance " + censusInstance + " (the operator's file states " + censusInstance + "): "
	require.Contains(t, stdout, "GONE daemon/GONE: "+gone)
	require.True(t, slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN"), stdout)
	done := h.lastEntry(id, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" done", done.Command+" "+done.State)
	require.True(t, strings.HasPrefix(done.Outputs["gone.daemon/GONE"], gone), "%q", done.Outputs["gone.daemon/GONE"])
	require.Contains(t, done.Outputs["gone.daemon/GONE"], "SQLSTATE 3D000")
}
