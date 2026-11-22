package consumernames

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// cleanupRetired reads logical's retired names in store.
func cleanupRetired(t *testing.T, store selection.Store, logical string) []string {
	t.Helper()
	record, _, err := store.Read(t.Context(), logical)
	require.NoError(t, err)
	return record.Retired
}

// cleanupPull creates, as the operator, a pull consumer named name on the notification stream,
// carrying the metadata the creator of logical's family writes (member.Metadata, of logical's
// member), which the owner check of every delete reads (deleteOwned).
func cleanupPull(t *testing.T, nc *nats.Conn, name, logical string) {
	t.Helper()
	m, err := replayMember(logical)
	require.NoError(t, err, "the member of %s", logical)
	makeConsumer(t, nc, jetstream.ConsumerConfig{Durable: name, AckPolicy: jetstream.AckExplicitPolicy,
		FilterSubject: "notifications.agent.cleanup." + name[len(name)-3:], Metadata: m.Metadata()})
}

// cleanup, with the row held in a consumer-cleanup hold, deletes the retired names of every family
// that the server holds: a session's, the listener durable's, and a daemon's, whose grant deletes
// no consumer; and drops from each record every retired name the server reports absent, a deleted
// one included, so Dispatch's absent retired name is dropped with no delete. It never deletes a
// record's current, even one its retired list names, and never a consumer no record retired (an
// unrecorded session~<nuid> stays). Each run writes its intent and then its done entry, which
// names each consumer it deleted; a second run deletes nothing, and its done entry says so.
// Neither completes the hold's record, so hold release is refused until a run with --final, whose
// done entry completes it, and the release then succeeds. Control: a run with no flag naming the
// store of a daemon the broker's cutover rows name is refused before it deletes anything, and its
// stop records that no run changed anything.
func TestTheCleanupDeletesOnlyRecordedRetiredNames(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	kv := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	dispatch := pgFamilyOf(t, familyDispatch, h.database(familyDispatch, dispatchMigrations...))
	daemonPool := h.database(daemonFamily("PROJ"), daemonMigrations...)
	daemon := pgFamilyOf(t, daemonFamily("PROJ"), daemonPool)
	// cleanup reads each daemon store by the census's rule, at the address its Deployment names.
	projFile := "PROJ=" + h.censusDaemonDeployed("PROJ", daemonPool)
	const (
		sessionCurrent  = "session~cleanupSessionCurrentAAA"
		sessionRetired  = "session~cleanupSessionRetiredBBB"
		sessionAbsent   = "session~cleanupSessionAbsentCCC"
		selfRetired     = "session~cleanupSelfRetiredDDDDD"
		unrecorded      = "session~cleanupUnrecordedEEEEEE"
		listenerCurrent = "listener-hub~cleanupListenerFFF"
		listenerOld     = "listener-hub~cleanupListenerGGG"
		daemonCurrent   = "legion-go-PROJ-github~cleanupHHH"
		daemonRetired   = "legion-go-PROJ-github~cleanupIII"
		dispatchCurrent = "delivery-events~cleanupDispaJJJ"
		dispatchAbsent  = "delivery-events~cleanupDispaKKK"
	)
	listenerKey := selection.DurableKey("listener-hub")
	for name, logical := range map[string]string{sessionCurrent: "ses_a", sessionRetired: "ses_a", selfRetired: "ses_b", unrecorded: "ses_c",
		listenerCurrent: listenerKey, listenerOld: listenerKey, daemonCurrent: "legion-go-PROJ-github", daemonRetired: "legion-go-PROJ-github",
		dispatchCurrent: "delivery-events"} {
		cleanupPull(t, nc, name, logical)
	}
	writeRecord(t, kv, "ses_a", selection.Record{Current: sessionCurrent, Start: 2, Retired: []string{sessionRetired, sessionAbsent}})
	writeRecord(t, kv, "ses_b", selection.Record{Current: selfRetired, Start: 2, Retired: []string{selfRetired}})
	writeRecord(t, kv, listenerKey, selection.Record{Current: listenerCurrent, Start: 2, Retired: []string{listenerOld}})
	writeRecord(t, daemon, "legion-go-PROJ-github", selection.Record{Current: daemonCurrent, Start: 2, Retired: []string{daemonRetired}})
	writeRecord(t, dispatch, "delivery-events", selection.Record{Current: dispatchCurrent, Start: 2, Retired: []string{dispatchAbsent}})
	for _, family := range []string{familySession, familyDispatch, daemonFamily("PROJ")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
	}
	before := len(h.journal(id).Ops)

	code, _, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url")
	require.Equal(t, 1, code, "control: no flag names daemon/PROJ's store")
	require.Contains(t, stderr, "no flag names the store of daemon/PROJ")
	stop := h.lastEntry(id, "")
	require.Equal(t, "consumer-names cleanup stop", stop.Command+" "+stop.State, "the refused cleanup ends in its stop entry")
	require.Equal(t, holdrecord.LeftUnchanged, stop.Outputs["left"], "the refused cleanup's stop records it changed nothing")
	// h.run closes the row's operator connections before a held command (closeOperators), so the
	// row connects again after each.
	nc, _ = h.operator()
	require.Contains(t, censusConsumers(t, nc), sessionRetired, "a refused cleanup deleted a name")

	code, stdout, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", projFile)
	require.Equal(t, 0, code, "%s", stderr)
	require.Contains(t, stdout, "cleanup: 5 records with retired names, 3 names deleted, 5 dropped from retired")
	nc, _ = h.operator()
	kv = selection.NewKVStore(selection.ConnRequester{Conn: nc})
	left := censusConsumers(t, nc)
	for _, name := range []string{sessionCurrent, selfRetired, unrecorded, listenerCurrent, daemonCurrent, dispatchCurrent} {
		require.Contains(t, left, name, "cleanup deleted %s", name)
	}
	for _, name := range []string{sessionRetired, listenerOld, daemonRetired} {
		require.NotContains(t, left, name, "cleanup left the retired %s", name)
	}
	require.Empty(t, cleanupRetired(t, kv, "ses_a"))
	require.Equal(t, []string{selfRetired}, cleanupRetired(t, kv, "ses_b"), "a retired name the record selects")
	require.Empty(t, cleanupRetired(t, kv, listenerKey))
	require.Empty(t, cleanupRetired(t, daemon, "legion-go-PROJ-github"))
	require.Empty(t, cleanupRetired(t, dispatch, "delivery-events"))
	journal := h.journal(id)
	require.Len(t, journal.Ops, before+4, "the refused run's intent and stop, and this run's intent and done entry")
	require.Equal(t, "consumer-names cleanup intent", journal.Ops[before+2].Command+" "+journal.Ops[before+2].State)
	done := journal.Ops[before+3]
	require.Equal(t, "consumer-names cleanup done", done.Command+" "+done.State)
	require.ElementsMatch(t, []string{sessionRetired, listenerOld, daemonRetired}, strings.Split(done.Outputs["deleted"], ", "),
		"the done entry names each consumer the run deleted")
	require.Equal(t, map[string]string{"records": "5", "dropped": "5"}, map[string]string{"records": done.Outputs["records"], "dropped": done.Outputs["dropped"]})
	require.False(t, journal.Complete, "a run with no --final completed the hold's record")
	require.ErrorContains(t, h.release(id), "the record is not complete", "hold release before a run with --final")

	code, stdout, stderr = h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", projFile)
	require.Equal(t, 0, code, "%s", stderr)
	require.Contains(t, stdout, "cleanup: 1 records with retired names, 0 names deleted, 0 dropped from retired")
	require.Equal(t, "none", h.lastEntry(id, "consumer-names cleanup").Outputs["deleted"], "the second run's done entry names no deletion")

	code, _, stderr = h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", projFile, "--final")
	require.Equal(t, 0, code, "%s", stderr)
	journal = h.journal(id)
	last := journal.Ops[len(journal.Ops)-1]
	require.Equal(t, "consumer-names cleanup --final done", last.Command+" "+last.State)
	require.True(t, journal.Complete, "the run with --final completes the hold's record")
	require.NoError(t, h.release(id), "hold release after the run with --final")
	require.Equal(t, "released", string(h.row().Phase))
}

// Plain cleanup reads each daemon store by the census's rule for the daemon stores, at exactly the
// address the project's one source names, and skips a store the rule finds gone (SQLSTATE 3D000
// there): it names the store gone in its output and in its done entry, and deletes every other
// family's retired names. So a broker cutover row naming a dropped project, which no command
// removes, does not stop every later cleanup: before cleanup --gone --final and after it, in a
// later hold. Without the gone store's file it refuses, naming the flag, as the census does; a
// store the rule leaves unreconciled (a file that reaches another database than its Deployment
// names) is UNKNOWN, and cleanup deletes nothing. Control: de92b5a02e's cleanup, which opens the
// file's URL and stops UNKNOWN on the 3D000 (exit 2), in this hold and in every later one.
func TestPlainCleanupSkipsAGoneDaemonStore(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	id := h.cleanupHold()
	server := h.database(familyDispatch, dispatchMigrations...)
	const logical = "legion-go-LIVE-github"
	const liveCurrent, liveRetired, liveRetiredLater = "legion-go-LIVE-github~cleanupGoneLiveAAA", "legion-go-LIVE-github~cleanupGoneLiveBBB",
		"legion-go-LIVE-github~cleanupGoneLiveCCC"
	livePool := h.database(daemonFamily("LIVE"), daemonMigrations...)
	live := pgFamilyOf(t, daemonFamily("LIVE"), livePool)
	liveFile := "LIVE=" + h.censusDaemonDeployed("LIVE", livePool)
	nc, _ := h.operator()
	for _, name := range []string{liveCurrent, liveRetired} {
		cleanupPull(t, nc, name, logical)
	}
	_, err := live.Write(ctx, logical, selection.Record{Current: liveCurrent, Start: 2, Retired: []string{liveRetired}}, 0)
	require.NoError(t, err)
	dropped := censusDroppedDatabase()
	h.censusDeploy(censusDeployment("GONE", dropped, censusInstance))
	gone := censusStoreFileFor(t, "GONE", server, dropped, censusInstance)
	// The real env opens the file's URL (openDatabase); the stack's env hands over h.dbs, so it is
	// given a pool on the same URL, which de92b5a02e's cleanup reads.
	raw, err := os.ReadFile(strings.TrimPrefix(gone, "GONE="))
	require.NoError(t, err)
	url, _, _ := strings.Cut(string(raw), "\n")
	gonePool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(gonePool.Close)
	h.mu.Lock()
	h.dbs[daemonFamily("GONE")] = gonePool
	h.mu.Unlock()
	for _, family := range []string{familySession, familyDispatch, daemonFamily("LIVE"), daemonFamily("GONE")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
	}
	consumers := func() []string {
		nc, _ := h.operator()
		return censusConsumers(t, nc)
	}

	code, _, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", liveFile)
	require.Equal(t, 1, code, "with no file for daemon/GONE: %s", stderr)
	require.Contains(t, stderr, "no flag names the store of daemon/GONE")
	require.Contains(t, consumers(), liveRetired, "a refused cleanup deleted a name")

	code, stdout, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", liveFile, "--daemon-database", gone)
	require.Equal(t, 0, code, "cleanup beside a gone store: %s%s", stdout, stderr)
	require.Contains(t, stdout, "cleanup daemon/GONE: store gone: database "+dropped+" on instance "+censusInstance)
	require.NotContains(t, consumers(), liveRetired, "cleanup left daemon/LIVE's retired name")
	require.Contains(t, consumers(), liveCurrent)
	done := h.lastEntry(id, "")
	require.Equal(t, "consumer-names cleanup done", done.Command+" "+done.State)
	require.Equal(t, liveRetired, done.Outputs["deleted"])
	require.Contains(t, done.Outputs["gone.daemon/GONE"], "database "+dropped+" on instance "+censusInstance)
	require.Contains(t, done.Outputs["gone.daemon/GONE"], "SQLSTATE 3D000")

	// A store the rule leaves unreconciled is UNKNOWN: GONE's file reaches another database than
	// its Deployment names.
	elsewhere := censusStoreFileFor(t, "GONE", server, "cnames_elsewhere", censusInstance)
	nc, _ = h.operator()
	cleanupPull(t, nc, liveRetiredLater, logical)
	record, revision, err := live.Read(ctx, logical)
	require.NoError(t, err)
	record.Retired = []string{liveRetiredLater}
	_, err = live.Write(ctx, logical, record, revision)
	require.NoError(t, err)
	code, stdout, stderr = h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url", "--daemon-database", liveFile, "--daemon-database", elsewhere)
	require.Equal(t, 2, code, "cleanup beside an unreconciled store: %s%s", stdout, stderr)
	require.Contains(t, stderr, `reaches database "cnames_elsewhere"`)
	require.Contains(t, consumers(), liveRetiredLater, "cleanup beside an unreconciled store deleted a name")

	code, stdout, stderr = h.run(actionCleanup, "--hold", id, "--gone", "GONE", "--daemon-database", gone, "--final")
	require.Equal(t, 0, code, "cleanup --gone GONE --final: %s%s", stdout, stderr)
	require.NoError(t, h.release(id))
	row, ok := h.cutoverRow(daemonFamily("GONE"))
	require.True(t, ok)
	require.Equal(t, selection.CutoverDone, row.State, "the gone project's cutover row stays")

	later := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	code, stdout, stderr = h.run(actionCleanup, "--hold", later, "--dispatch-database-file", "dispatch.url", "--daemon-database", liveFile, "--daemon-database", gone, "--final")
	require.Equal(t, 0, code, "cleanup in a later hold beside the gone store: %s%s", stdout, stderr)
	require.Contains(t, stdout, "cleanup daemon/GONE: store gone: database "+dropped)
	require.NotContains(t, consumers(), liveRetiredLater, "the later cleanup left daemon/LIVE's retired name")
	require.True(t, h.journal(later).Complete)
	require.NoError(t, h.release(later))
}

// cleanup, cleanup --gone and replace run only in a consumer-cleanup hold: with the row held in a
// cutover hold, each, and each with --final, is refused at its admission, naming the workflow it
// runs in and the hold's, and writes nothing: no journal entry, and the retired name cleanup would
// delete stays on the stream. Control: the cleanup before the commands joined the workflow ran
// under any held row, in the cutover hold, and deleted that name (the red run); the same cleanup in
// a consumer-cleanup hold deletes it (TestTheCleanupDeletesOnlyRecordedRetiredNames).
func TestTheConsumerCleanupCommandsRunOnlyInItsHold(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	h.database(familyDispatch, dispatchMigrations...)
	id := h.heldHold(holdrecord.WorkflowCutover)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	const current, retired = "session~onlyInItsHoldCurrentAAA", "session~onlyInItsHoldRetiredBBB"
	cleanupPull(t, nc, current, "ses_a")
	cleanupPull(t, nc, retired, "ses_a")
	kv := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	_, err = kv.Write(ctx, "ses_a", selection.Record{Current: current, Start: 2, Retired: []string{retired}}, 0)
	require.NoError(t, err)
	before := len(h.journal(id).Ops)

	for _, args := range [][]string{
		{actionCleanup, "--dispatch-database-file", "dispatch.url"},
		{actionCleanup, "--dispatch-database-file", "dispatch.url", "--final"},
		{actionCleanup, "--gone", "GONE", "--daemon-database", "GONE=/dev/null"},
		{actionCleanup, "--gone", "GONE", "--daemon-database", "GONE=/dev/null", "--final"},
		{actionReplace, "--listener", "hub"},
		{actionReplace, "--listener", "hub", "--final"},
	} {
		code, stdout, stderr := h.run(append([]string{args[0], "--hold", id}, args[1:]...)...)
		require.Equal(t, 1, code, "%v in a cutover hold: %s%s", args, stdout, stderr)
		require.Contains(t, stderr, "runs only in a "+string(holdrecord.WorkflowConsumerCleanup)+" hold", "%v", args)
		require.Contains(t, stderr, string(holdrecord.WorkflowCutover), "%v: the refusal names the hold's workflow", args)
	}
	require.Len(t, h.journal(id).Ops, before, "a refused admission wrote to the hold's journal")
	nc, _ = h.operator()
	require.Contains(t, censusConsumers(t, nc), retired, "a cleanup refused in a cutover hold deleted a retired name")
}

// consumer-names cleanup's and replace's flags: --final is replay's, cleanup's and replace's,
// --gone cleanup's alone, and cleanup --gone <project> needs that project's --daemon-database
// entry, the file it reads the store's address from; each misuse is a usage error naming it, exit
// 2. Control: the same flags otherwise pass the usage check (a run whose env reaches nothing then
// fails past it, exit 1).
func TestTheConsumerCleanupFlags(t *testing.T) {
	refused := env{records: func(context.Context, options, bool) (*holdrecord.Client, error) {
		return nil, fmt.Errorf("this run reaches no broker")
	}}
	for _, row := range []struct {
		name    string
		args    []string
		code    int
		problem string
	}{
		{"cleanup with --final", []string{"cleanup", "--hold", "h1", "--final"}, 1, ""},
		{"replace with --final", []string{"replace", "--hold", "h1", "--listener", "hub", "--final"}, 1, ""},
		{"cleanup --gone with its store's file", []string{"cleanup", "--hold", "h1", "--gone", "P", "--daemon-database", "P=/dev/null"}, 1, ""},
		{"cleanup --gone without its store's file", []string{"cleanup", "--hold", "h1", "--gone", "P"}, 2,
			"envoy-infra consumer-names cleanup: --daemon-database P=<file> is required: cleanup --gone reads that store at the address its Deployment, or its retired-stores entry, names"},
		{"--gone on another word", []string{"replace", "--hold", "h1", "--listener", "hub", "--gone", "P"}, 2,
			"envoy-infra consumer-names replace: --gone is cleanup's alone"},
		{"--final on another word", []string{"convert", "--hold", "h1", "--final"}, 2,
			"envoy-infra consumer-names convert: --final is replay's, cleanup's and replace's alone"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			full := append(append(append([]string{}, row.args...), commonArgs...), writingArgs...)
			code := runCommand(context.Background(), full, &stdout, &stderr, refused)
			require.Equal(t, row.code, code, stderr.String())
			if row.problem == "" {
				require.NotContains(t, stderr.String(), "usage:", "control: the flags did not pass the usage check")
				return
			}
			require.Contains(t, stderr.String(), row.problem+"\n")
		})
	}
}

// A daemon whose store is gone (its database answers SQLSTATE 3D000 at the address its Deployment
// names) keeps its consumers through a plain census, which names each LEFT, and loses them to
// cleanup --gone in a consumer-cleanup hold: it reads the Deployment again, finds the store gone,
// deletes every consumer whose logical name is the daemon's, fixed or fresh, declared or not,
// names each removal, and its done entry names the store and each consumer it removed; another
// daemon's consumer stays. A run with --final then removes nothing more and completes the hold's
// record, and hold release follows; the gone daemon's broker cutover row still reads done after it,
// as cleanup --gone removes consumers and writes no cutover row. Controls, before it and in the
// same hold: --gone against a live store refuses, and so does --gone against a store whose server
// answers 57P01, a PgError that is no 3D000; each removes nothing and its stop records the run
// unchanged. The cutover row's control is a cleanup --gone --final that writes the row
// rolled_back, which fails it.
func TestTheCleanupGoneRemovesAGoneStoresConsumers(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	census := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	const wake, ready, dispatch = "legion-go-GONE-wake", "legion-go-GONE-ready", "legion-go-GONE-dispatch"
	declared, undeclared := selection.FreshName(wake), selection.FreshName(ready)
	goneConsumers := []string{declared, undeclared, dispatch}
	for name, logical := range map[string]string{declared: wake, undeclared: ready, dispatch: dispatch} {
		daemonConsumer(t, nc, name, logical)
	}
	for _, family := range []string{familySession, daemonFamily("GONE")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, census)
		require.NoError(t, err)
	}
	_, server := censusStore(t, "SERVER")
	dropped := censusDroppedDatabase()
	h.censusDeploy(censusDeployment("GONE", dropped, censusInstance))
	gone := censusStoreFileFor(t, "GONE", server, dropped, censusInstance)
	consumers := func() []string {
		nc, _ := h.operator()
		return censusConsumers(t, nc)
	}

	// A plain census keeps the gone store's consumers, naming each.
	code, stdout, stderr := h.censusCommand(census, "--family", familySession, "--family", daemonFamily("GONE"),
		"--daemon-consumer", wake+"="+bus.LegionWakeTopic, "--daemon-database", gone)
	require.Equal(t, 0, code, "the census beside a gone store: %s%s", stdout, stderr)
	for _, name := range goneConsumers {
		require.Contains(t, stdout, "LEFT "+notifications+" consumer "+name+": left, store gone: ")
	}
	require.Subset(t, consumers(), goneConsumers, "the census removed a gone daemon's consumer")
	require.NoError(t, h.release(census))

	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, _ = h.operator()
	live := selection.FreshName("legion-go-LIVE-wake")
	daemonConsumer(t, nc, live, "legion-go-LIVE-wake")
	liveEntry, liveDB := censusDaemonStore(t, "LIVE", daemonMigrations...)
	h.censusDeploy(censusDeploymentOf("LIVE", liveDB))
	down := censusAnsweringServer(t, "DOWN", "daemon_down", "57P01")
	h.censusDeploy(censusDeployment("DOWN", "daemon_down", censusInstance))

	for _, control := range []struct{ name, project, entry, why string }{
		{"a live store", "LIVE", liveEntry, "answers at the address its Deployment names, so it is not gone"},
		{"a store whose server answers 57P01", "DOWN", down, "SQLSTATE 57P01"},
	} {
		code, stdout, stderr := h.run(actionCleanup, "--hold", id, "--gone", control.project, "--daemon-database", control.entry)
		require.Equal(t, 1, code, "control: --gone against %s: %s%s", control.name, stdout, stderr)
		require.Contains(t, stderr, control.why, "control: --gone against %s", control.name)
		require.NotContains(t, stdout, "removed", "control: --gone against %s", control.name)
		stop := h.lastEntry(id, "")
		require.Equal(t, "consumer-names cleanup --gone stop", stop.Command+" "+stop.State, "control: --gone against %s", control.name)
		require.Equal(t, holdrecord.LeftUnchanged, stop.Outputs["left"], "control: --gone against %s", control.name)
		require.Subset(t, consumers(), append([]string{live}, goneConsumers...), "control: --gone against %s removed a consumer", control.name)
	}

	code, stdout, stderr = h.run(actionCleanup, "--hold", id, "--gone", "GONE", "--daemon-database", gone)
	require.Equal(t, 0, code, "cleanup --gone GONE: %s%s", stdout, stderr)
	for _, name := range goneConsumers {
		require.Contains(t, stdout, "cleanup --gone daemon/GONE: removed "+notifications+" consumer "+name+": store gone: database "+dropped)
	}
	left := consumers()
	require.Contains(t, left, live, "cleanup --gone removed another daemon's consumer")
	for _, name := range goneConsumers {
		require.NotContains(t, left, name, "cleanup --gone left the gone daemon's %s", name)
	}
	done := h.lastEntry(id, "")
	require.Equal(t, "consumer-names cleanup --gone done", done.Command+" "+done.State)
	require.ElementsMatch(t, goneConsumers, strings.Split(done.Outputs["removed"], ", "), "the done entry names each removal")
	require.Contains(t, done.Outputs["gone.daemon/GONE"], "SQLSTATE 3D000")
	require.Contains(t, done.Outputs["gone.daemon/GONE"], "database "+dropped+" on instance "+censusInstance)
	require.Equal(t, holdrecord.Operation{Command: "consumer-names cleanup --gone", State: "intent", Inputs: map[string]string{"daemon": "GONE"}},
		cleanupIntentOf(h.journal(id), "consumer-names cleanup --gone"), "its intent names the daemon")
	require.ErrorContains(t, h.release(id), "the record is not complete", "hold release before a run with --final")

	code, stdout, stderr = h.run(actionCleanup, "--hold", id, "--gone", "GONE", "--daemon-database", gone, "--final")
	require.Equal(t, 0, code, "cleanup --gone GONE --final: %s%s", stdout, stderr)
	require.Contains(t, stdout, "cleanup --gone daemon/GONE: 0 consumers removed")
	require.True(t, h.journal(id).Complete, "the run with --final completes the hold's record")
	row, ok := h.cutoverRow(daemonFamily("GONE"))
	require.True(t, ok, "the broker holds no %s cutover row after cleanup --gone --final", daemonFamily("GONE"))
	require.Equal(t, selection.CutoverDone, row.State, "cleanup --gone --final changed the %s cutover row", daemonFamily("GONE"))
	require.NoError(t, h.release(id))
	require.True(t, slices.Contains(consumers(), live))
}

// A cleanup stop counts this hold's barrier key, which the run put before it walked the selection
// bucket (cleanupSessionTargets): a stop names every change its command's runs made and did not
// undo. The bucket holds a malformed value, so cleanup is UNKNOWN after its barrier and deletes
// nothing; its stop names the key, and hold release is refused over it. The refusal is UNKNOWN, so
// its stop records what it left as unknown (leftUnknownPrefix) whatever the key's rule. Controls:
// under the earlier rules that the key is no change (leftRuleSet.barrierNoChange) and an UNKNOWN's
// stop records its run's account (leftRuleSet.unknownAsAccount), the same refusal records
// unchanged, and hold release is admitted over it; under the first of them alone it records
// unknown, and hold release is refused.
func TestACleanupStopNamesTheBarrierKeyItPut(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	h.database(familyDispatch, dispatchMigrations...)
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	_, err = js.Publish(ctx, "$KV."+selection.SessionBucket+".ses_bad", []byte("no record"))
	require.NoError(t, err)
	release := holdrecord.Request{Command: holdrecord.CommandRelease}
	refused := func() holdrecord.Operation {
		t.Helper()
		code, stdout, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url")
		require.Equal(t, 2, code, "cleanup on a malformed value: %s%s", stdout, stderr)
		require.Contains(t, stderr, "ses_bad", "the hold point: the refusal follows the walk behind the barrier")
		stop := h.lastEntry(id, cleanupCommand)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{barrierNoChange: true, unknownAsAccount: true})
	require.Equal(t, holdrecord.LeftUnchanged, refused().Outputs["left"], "control: with the key no change and an UNKNOWN as its account, the stop records unchanged")
	require.NoError(t, holdrecord.Decide(h.journal(id), release), "control: hold release is admitted over the unchanged stop")

	leftWith(t, leftRuleSet{barrierNoChange: true})
	require.True(t, strings.HasPrefix(refused().Outputs["left"], leftUnknownPrefix), "with the key no change, the UNKNOWN's stop records unknown")
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over the UNKNOWN's stop")

	leftWith(t, leftRuleSet{})
	left := refused().Outputs["left"]
	require.Contains(t, left, "this run deleted no consumer and wrote no record; it put "+barrierLeft)
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over a stop that left the key")
}

// cleanupIntentOf is d's latest intent entry of command, its command, state and inputs alone.
func cleanupIntentOf(d holdrecord.Done, command string) holdrecord.Operation {
	for i := len(d.Ops) - 1; i >= 0; i-- {
		if op := d.Ops[i]; op.Command == command && op.State == "intent" {
			return holdrecord.Operation{Command: op.Command, State: op.State, Inputs: op.Inputs}
		}
	}
	return holdrecord.Operation{}
}

// cleanupHold is a consumer-cleanup hold, set, on a hub with the notification stream and the
// selection bucket, which every held re-read of the hold finds present.
func (h *stack) cleanupHold() string {
	h.t.Helper()
	id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	nc, js := h.operator()
	h.notificationStream(js)
	_, err := selection.CreateSessionBucket(h.t.Context(), nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(h.t, err)
	return id
}

// A cleanup whose done entry is lost, an exit after its intent that is UNKNOWN since the write may
// have landed, writes its stop, recording what it left as unknown, and hold abort is then the way
// out (requireALostDoneStops, with its control: the intent left pending under the earlier rule).
func TestACleanupWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	h.database(familyDispatch, dispatchMigrations...)
	id := h.cleanupHold()
	h.requireALostDoneStops(id, cleanupCommand, func() (int, string, string) {
		return h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url")
	})
}

// A cleanup --gone whose done entry is lost, after it found the daemon's store gone and removed
// what it lists, writes its stop, recording what it left as unknown, and hold abort is then the way
// out (requireALostDoneStops, with its control: the intent left pending under the earlier rule).
func TestACleanupGoneWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	id := h.cleanupHold()
	_, server := censusStore(t, "SERVER")
	dropped := censusDroppedDatabase()
	h.censusDeploy(censusDeployment("GONE", dropped, censusInstance))
	gone := censusStoreFileFor(t, "GONE", server, dropped, censusInstance)
	h.requireALostDoneStops(id, cleanupGoneCommand, func() (int, string, string) {
		return h.run(actionCleanup, "--hold", id, "--gone", "GONE", "--daemon-database", gone)
	})
}
