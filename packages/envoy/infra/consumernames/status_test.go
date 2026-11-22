package consumernames

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// statusWalkServer starts a lone server whose user service, with password token, holds grant, as
// the auth service answers the operator, and an admin every credential-less connection is.
func statusWalkServer(t *testing.T, token string, grant natsgrant.Permissions) string {
	t.Helper()
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for i, subject := range subjects {
			quoted[i] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	config := fmt.Sprintf(`jetstream {}
authorization {
  users = [
    { user: "admin", password: "admin" },
    { user: %q, password: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }
  ]
}
no_auth_user: "admin"
`, natscred.ServiceUser, token, quote(grant.Publish), quote(grant.Subscribe))
	_, url := testnats.StartConfig(t, config, []string{"-js"}, true)
	return url
}

// statusPublishKV publishes data on key of the selection bucket on nc, with the KV operation
// header op when op is not empty (a delete or purge marker, as nats.go's kv.go writes one).
func statusPublishKV(t *testing.T, nc *nats.Conn, key string, data []byte, op string) {
	t.Helper()
	msg := &nats.Msg{Subject: "$KV." + selection.SessionBucket + "." + key, Data: data, Header: nats.Header{}}
	if op != "" {
		msg.Header.Set(kvOperationHeader, op)
	}
	reply, err := nc.RequestMsg(msg, requestTimeout)
	require.NoError(t, err)
	require.NotContains(t, string(reply.Data), `"error"`, "the write of %s", key)
}

// The walk of the selection bucket status makes runs under exactly natsgrant.Operator(released):
// it reads every key by sequence, a key written twice once with its last value, a delete marker
// as deleted, and the sentinel and a barrier key among them. Control: under that grant without
// STREAM.MSG.GET of the bucket, the walk cannot end, and it is UNKNOWN rather than an empty
// listing; and on a server with no bucket the walk answers a missing store.
func TestTheStatusWalkReadsTheBucketUnderTheReleasedGrant(t *testing.T) {
	released := natsgrant.Operator(natsgrant.OperatorPhaseReleased)
	url := statusWalkServer(t, "walker", released)
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	walker, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "walker"), nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(walker.Close)

	_, err = statusSelectionKeys(t.Context(), walker)
	require.ErrorIs(t, err, selection.ErrNoStore, "a walk of no bucket")

	_, err = selection.CreateSessionBucket(t.Context(), admin, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 4})
	require.NoError(t, err)
	statusPublishKV(t, admin, "ses_a", []byte(`{"current":"session~first"}`), "")
	statusPublishKV(t, admin, "ses_gone", []byte(`{"current":"session~gone"}`), "")
	statusPublishKV(t, admin, "ses_a", []byte(`{"current":"session~second"}`), "")
	statusPublishKV(t, admin, "ses_gone", nil, "DEL")
	statusPublishKV(t, admin, selection.DurableKey("listener-hub"), []byte(`{"current":"listener-hub~x"}`), "")
	statusPublishKV(t, admin, "hold.barrier.h1", []byte("h1"), "")

	keys, err := statusSelectionKeys(t.Context(), walker)
	require.NoError(t, err)
	byKey := map[string]statusKey{}
	var names []string
	for _, key := range keys {
		byKey[key.Key] = key
		names = append(names, key.Key)
	}
	require.Equal(t, []string{"durable.listener-hub", selection.SentinelKey, "hold.barrier.h1", "ses_a", "ses_gone"}, names)
	require.True(t, slices.IsSortedFunc(keys, func(a, b statusKey) int { return strings.Compare(a.Key, b.Key) }))
	require.JSONEq(t, `{"current":"session~second"}`, string(byKey["ses_a"].Value), "the walk kept an earlier write of ses_a")
	require.True(t, byKey["ses_gone"].Deleted, "the delete marker")
	require.Nil(t, byKey["ses_gone"].Value)

	without := natsgrant.Permissions{Subscribe: released.Subscribe}
	for _, subject := range released.Publish {
		if subject != "$JS.API.STREAM.MSG.GET."+selectionStreamName {
			without.Publish = append(without.Publish, subject)
		}
	}
	require.Len(t, without.Publish, len(released.Publish)-1, "control: the grant names the bucket's message get")
	urlWithout := statusWalkServer(t, "walker", without)
	adminWithout := testnats.Connect(t, urlWithout)
	t.Cleanup(adminWithout.Close)
	_, err = selection.CreateSessionBucket(t.Context(), adminWithout, 1, selection.StoreState{Cutover: selection.CutoverDone})
	require.NoError(t, err)
	refused, err := nats.Connect(urlWithout, nats.UserInfo(natscred.ServiceUser, "walker"), nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(refused.Close)
	_, err = statusSelectionKeys(t.Context(), refused)
	var unknown *Unknown
	require.True(t, errors.As(err, &unknown), "the walk without the message get = %v, want UNKNOWN", err)
}

// status, with the row released after a hold, prints the hold's journal, the broker's cutover
// rows, each family's store and records (a session's current and retired names, each marked on
// the stream or absent; the listener durable's record; Dispatch's and a daemon's rows), skips the
// bucket's barrier key, and lists every blocker: a current the stream does not hold, an open
// operation, a deleted record, a fixed name on the stream, a store with no sentinel and a missing
// store. A complete reading exits 0 and counts the blockers. Control: a daemon the broker's
// cutover rows name whose store no flag names is UNKNOWN, exit 2; and status under a held row is
// refused, naming the phase.
func TestTheStatusListsEveryFamilysRecordsAndBlockers(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	require.NoError(t, holdrecord.Barrier(ctx, js, selection.SessionBucket, id))
	pull := func(name string) {
		t.Helper()
		makeConsumer(t, nc, jetstream.ConsumerConfig{Durable: name, AckPolicy: jetstream.AckExplicitPolicy,
			FilterSubject: "notifications.agent." + strings.NewReplacer("~", "_").Replace(name)})
	}
	kv := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	for _, name := range []string{"session~A", "session~R", "session~C", "listener-hub~L", "delivery-events~D", "s-ses_fixed"} {
		pull(name)
	}
	writeRecord(t, kv, "ses_a", selection.Record{Current: "session~A", Start: 3, Retired: []string{"session~R", "session~Q"}})
	writeRecord(t, kv, "ses_b", selection.Record{Current: "session~B", Start: 4})
	writeRecord(t, kv, "ses_c", selection.Record{Current: "session~C", Start: 5, Op: &selection.Op{Kind: selection.OpReplace, Candidate: "session~N", Old: "session~C", Owner: "o1", ReservedAt: time.Now()}})
	writeRecord(t, kv, selection.DurableKey("listener-hub"), selection.Record{Current: "listener-hub~L", Start: 6})
	writeRecord(t, kv, "ses_gone", selection.Record{Current: "session~G"})
	statusPublishKV(t, nc, "ses_gone", nil, "DEL")

	dispatch := pgFamilyOf(t, familyDispatch, h.database(familyDispatch, dispatchMigrations...))
	writeRecord(t, dispatch, "delivery-events", selection.Record{Current: "delivery-events~D", Start: 7})
	daemon := pgFamilyOf(t, daemonFamily("PROJ"), h.database(daemonFamily("PROJ"), daemonMigrations...))
	_, err = daemon.WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone}, 0)
	require.NoError(t, err)
	writeRecord(t, daemon, "legion-go-PROJ-wake", selection.Record{Current: "legion-go-PROJ-wake~W", Start: 8, Retired: []string{"legion-go-PROJ-wake~V"}})
	h.database(daemonFamily("MISS"))
	for _, family := range []string{familySession, familyDispatch, daemonFamily("PROJ")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
	}
	h.censusDone(id)

	code, _, stderr := h.run(actionStatus, "--dispatch-database-file", "dispatch.url", "--daemon-database", "PROJ=proj.url")
	require.Equal(t, 1, code, "control: status under a held row")
	require.Contains(t, stderr, "status runs while the hold row is released, and it is held")
	require.NoError(t, h.release(id))

	code, _, stderr = h.run(actionStatus, "--dispatch-database-file", "dispatch.url", "--daemon-database", "MISS=miss.url")
	require.Equal(t, 2, code, "control: a daemon the cutover rows name with no store flag")
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, "daemon/PROJ's store: no --daemon-database PROJ=<file> names it")

	code, stdout, stderr := h.run(actionStatus, "--hold", id, "--dispatch-database-file", "dispatch.url",
		"--daemon-database", "PROJ=proj.url", "--daemon-database", "MISS=miss.url")
	require.Equal(t, 0, code, "%s", stderr)
	for _, line := range []string{
		"hold " + id + ": workflow streams-census",
		"  4. streams census done\n",
		"  6. hold release done\n",
		"cutover session: done (hold " + id,
		"cutover daemon/PROJ: done (hold " + id,
		"store session (" + selectionStreamName + "): cutover=done boundary=1",
		"record session ses_a: current session~A (on the stream), start 3, retired [session~R (on the stream), session~Q (absent)]",
		"record session durable.listener-hub: current listener-hub~L (on the stream), start 6",
		"record session ses_gone: deleted",
		"store dispatch (intake_consumers): no sentinel",
		"record dispatch delivery-events: current delivery-events~D (on the stream), start 7",
		"store daemon/PROJ (intake_consumers): cutover=done",
		"record daemon/PROJ legion-go-PROJ-wake: current legion-go-PROJ-wake~W (absent), start 8, retired [legion-go-PROJ-wake~V (absent)]",
		"store daemon/MISS: missing",
		"BLOCKER session ses_b: its current session~B is not on " + notifications,
		"BLOCKER session ses_c: an open replace (candidate session~N, old session~C, owner o1",
		"BLOCKER session ses_gone: its record's last write is a delete marker",
		"BLOCKER session: the fixed name s-ses_fixed is on " + notifications,
		"BLOCKER dispatch: its store has no sentinel",
		"BLOCKER daemon/PROJ legion-go-PROJ-wake: its current legion-go-PROJ-wake~W is not on " + notifications,
		"BLOCKER daemon/MISS: its store is missing",
		"blockers: 7",
	} {
		require.Contains(t, stdout, line)
	}
	require.NotContains(t, stdout, "hold.barrier", "status read the barrier key as a record")
	require.NotContains(t, stdout, "BLOCKER session ses_a")
}

// statusGoneStore makes project's daemon store one whose database does not exist on the test's
// Postgres server, as a deleted project leaves it, and answers the --daemon-database value naming
// it: the stack's env hands status a pool on that database, which answers SQLSTATE 3D000, and the
// file names it on the server's address, with instance on its second line (censusStoreFileFor).
func (h *stack) statusGoneStore(project, database, instance string) string {
	h.t.Helper()
	_, server := censusStore(h.t, "SERVER-"+project)
	entry := censusStoreFileFor(h.t, project, server, database, instance)
	_, file, _ := strings.Cut(entry, "=")
	raw, err := os.ReadFile(file)
	require.NoError(h.t, err)
	url, _, _ := strings.Cut(string(raw), "\n")
	pool, err := pgxpool.New(h.t.Context(), url)
	require.NoError(h.t, err)
	h.t.Cleanup(pool.Close)
	h.mu.Lock()
	h.dbs[daemonFamily(project)] = pool
	h.mu.Unlock()
	return entry
}

// status, while a broker cutover row names a daemon whose store is gone (its database answers
// SQLSTATE 3D000 at exactly the address the daemon's Deployment names, by the census's rule for the
// daemon stores, censusReadDaemonStore), reports that store gone, with the database, the instance,
// the instance the operator's file states and the server's answer, and goes on: a complete reading
// exits 0. A 3D000 the rule does not confirm (the Deployment names another database) stays
// UNKNOWN, naming why. Control: under the earlier rule (statusRules.goneUnknown) the gone store is
// a failed read, and status is UNKNOWN for good while the cutover row names it, as no command
// removes a broker cutover row.
func TestTheStatusReportsAGoneDaemonStore(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	_, err = h.records().PutCutover(ctx, daemonFamily("GONE"), selection.CutoverDone, id)
	require.NoError(t, err)
	require.NoError(t, h.release(id))
	dropped := censusDroppedDatabase()
	h.censusDeploy(censusDeployment("GONE", dropped, censusInstance))
	gone := h.statusGoneStore("GONE", dropped, censusInstance)
	h.censusDeploy(censusDeployment("ELSEWHERE", "daemon_elsewhere", censusInstance))
	elsewhere := h.statusGoneStore("ELSEWHERE", censusDroppedDatabase(), censusInstance)
	h.database(familyDispatch, dispatchMigrations...)
	status := func(entries ...string) (int, string, string) {
		t.Helper()
		args := []string{actionStatus, "--dispatch-database-file", "dispatch.url"}
		for _, entry := range entries {
			args = append(args, "--daemon-database", entry)
		}
		return h.run(args...)
	}

	previous := statusRules
	t.Cleanup(func() { statusRules = previous })
	statusRules = statusRuleSet{goneUnknown: true}
	code, _, stderr := status(gone)
	statusRules = statusRuleSet{}
	require.Equal(t, 2, code, "control: under the earlier rule the gone store is a failed read: %s", stderr)
	require.Contains(t, stderr, "daemon/GONE's sentinel")
	require.Contains(t, stderr, "SQLSTATE 3D000")

	code, stdout, stderr := status(gone)
	require.Equal(t, 0, code, "status beside a gone store: %s%s", stdout, stderr)
	require.Contains(t, stdout, "cutover daemon/GONE: done (hold "+id)
	require.Contains(t, stdout, "store daemon/GONE: gone: database "+dropped+" on instance "+censusInstance+" (the operator's file states "+censusInstance+"): ")
	require.Contains(t, stdout, "(SQLSTATE 3D000)")
	require.NotContains(t, stdout, "BLOCKER daemon/GONE", "status counted the gone store as a blocker")
	require.Contains(t, stdout, "blockers: ")

	code, stdout, stderr = status(gone, elsewhere)
	require.Equal(t, 2, code, "a 3D000 the rule does not confirm: %s%s", stdout, stderr)
	require.Contains(t, stderr, "daemon/ELSEWHERE's store answers SQLSTATE 3D000, and the census's rule does not confirm it gone")
	require.Contains(t, stderr, "Deployment legion/legion-daemon-elsewhere names database \"daemon_elsewhere\"")
	require.Contains(t, stdout, "store daemon/GONE: gone: ", "status went on past the gone store")
}

// status lists, as a blocker naming the field and the name, a record its family's creator refuses
// (checkRecordFence, selection.Names.Check): a session's record that retires its fixed name, and
// one whose current is a daemon's consumer name. A complete reading still exits 0. Control: the
// status before the fence (the previous status.go, by -overlay) lists both records with no blocker
// naming either field, as healthy.
func TestStatusListsARecordItsCreatorRefusesAsABlocker(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	kv := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	_, err = kv.Write(ctx, "ses_fixed", selection.Record{Current: selection.FreshName(sessionFreshPrefix), Start: 1,
		Retired: []string{sessionFixedPrefix + "ses_fixed"}}, 0)
	require.NoError(t, err)
	_, err = kv.Write(ctx, "ses_other", selection.Record{Current: selection.FreshName("legion-go-PROJ-wake"), Start: 1}, 0)
	require.NoError(t, err)
	h.database(familyDispatch, dispatchMigrations...)
	h.censusDone(id)
	require.NoError(t, h.release(id))

	code, stdout, stderr := h.run(actionStatus, "--dispatch-database-file", "dispatch.url")
	require.Equal(t, 0, code, "%s", stderr)
	for _, want := range []string{
		`BLOCKER session ses_fixed: its family's creator refuses the record: `,
		`the key ses_fixed's retired name "s-ses_fixed" is no session~<nuid> name`,
		`BLOCKER session ses_other: its family's creator refuses the record: `,
		`the key ses_other's current "legion-go-PROJ-wake~`,
	} {
		require.Contains(t, stdout, want)
	}
}
