package writerhold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// fakeStores are daemon stores whose StreamIDs the census reconciles, by family daemon/<P>, as
// consumernames.ReconcileDaemonStreamIDs does: it reads ENVOY_NOTIFICATIONS' identity on the
// census's connection and the daemons' Deployments through the listing the census hands it, and
// answers its StreamIDResult with each daemon family in exactly one list: each store gone (gone),
// each already at the identity (Reconciled), and each left unreconciled (unreconciled, and each
// family no --daemon-database entry names); while any is unreconciled it writes nothing, and each
// stale store is unreconciled too, not written; otherwise it writes the identity where a store's
// differs, a write failing for each store failWrite names, which leaves it and each stale store
// after it unreconciled.
type fakeStores struct {
	mu           sync.Mutex
	ids          map[string]string
	gone         map[string]GoneStore
	unreconciled map[string]string
	failWrite    map[string]string
	// listed are the names of the Deployments the last call's listing answered.
	listed []string
}

func (f *fakeStores) reconcile(ctx context.Context, nc *nats.Conn, deployments DaemonDeployments, families, daemonDatabases []string) (StreamIDResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	js, err := jetstream.New(nc)
	if err != nil {
		return StreamIDResult{}, err
	}
	s, err := js.Stream(ctx, bus.Stream)
	if err != nil {
		return StreamIDResult{}, err
	}
	id := s.CachedInfo().Config.Metadata[bus.StreamIDMetadataKey]
	if id == "" {
		return StreamIDResult{}, bus.ErrNoStreamID
	}
	if deployments == nil {
		return StreamIDResult{}, errors.New("no listing of the daemons' Deployments")
	}
	listed, err := deployments(ctx)
	if err != nil {
		return StreamIDResult{}, err
	}
	f.listed = nil
	for _, d := range listed {
		f.listed = append(f.listed, d.Name)
	}
	databases := map[string]bool{}
	for _, entry := range daemonDatabases {
		project, _, _ := strings.Cut(entry, "=")
		databases[project] = true
	}
	var projects []string
	for _, family := range families {
		if project, ok := strings.CutPrefix(family, daemonFamilyPrefix); ok {
			projects = append(projects, project)
		}
	}
	sort.Strings(projects)
	var result StreamIDResult
	// held is why a stale store is not written: a store unreconciled, or a write that failed before it.
	var held string
	for _, project := range projects {
		if _, unreconciled := f.unreconciled[project]; unreconciled || !databases[project] {
			held = "not written while a store is unreconciled"
		}
	}
	for _, project := range projects {
		why, unreconciled := f.unreconciled[project]
		gone, isGone := f.gone[project]
		switch {
		case !databases[project]:
			result.Unreconciled = append(result.Unreconciled, UnreconciledStore{Project: project, Reason: "no --daemon-database names it"})
		case unreconciled:
			result.Unreconciled = append(result.Unreconciled, UnreconciledStore{Project: project, Reason: why})
		case isGone:
			result.Gone = append(result.Gone, gone)
		case f.ids[project] == id:
			result.Reconciled = append(result.Reconciled, ReconciledStore{Project: project, StreamID: id})
		case held != "":
			result.Unreconciled = append(result.Unreconciled, UnreconciledStore{Project: project, Reason: held})
		default:
			if why, ok := f.failWrite[project]; ok {
				result.Unreconciled = append(result.Unreconciled, UnreconciledStore{Project: project, Reason: why})
				held = "not written"
				continue
			}
			result.Changed = append(result.Changed, StreamIDChange{Project: project, From: f.ids[project], To: id})
			f.ids[project] = id
		}
	}
	return result, nil
}

// snapshot is the stores' ids now.
func (f *fakeStores) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.ids {
		out[k] = v
	}
	return out
}

// set runs change on the stores under their lock.
func (f *fakeStores) set(change func(f *fakeStores)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// listedNames are the Deployments the last call's listing answered.
func (f *fakeStores) listedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.listed...)
}

// notificationsID reads ENVOY_NOTIFICATIONS' identity as the operator.
func (h *heldStack) notificationsID() string {
	h.t.Helper()
	nc, err := h.s.ConnectOperator(context.Background())
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	s, err := js.Stream(context.Background(), bus.Stream)
	require.NoError(h.t, err)
	return s.CachedInfo().Config.Metadata[bus.StreamIDMetadataKey]
}

// doneOutputs are command's latest done entry's outputs in d.
func doneOutputs(d holdrecord.Done, command string) map[string]string {
	var outputs map[string]string
	for _, op := range d.Ops {
		if op.Command == command && op.State == "done" {
			outputs = op.Outputs
		}
	}
	return outputs
}

// The census reconciles each daemon store's StreamID with ENVOY_NOTIFICATIONS' identity. After the
// stream is recreated (a new identity), the census has every daemon store's StreamID rewritten,
// prints each change as a CHANGED line and names it in its done entry (stream_id.daemon/<P>), and
// prints no RECONCILED line, since no store was already at the identity. The control is a census
// with no writer, which is UNKNOWN (exit 2), writes no done entry and leaves every store at the old
// identity. Run again on the stream unchanged, the census changes nothing, names no store in its
// done entry, and prints each store the writer found already at the identity as
// `RECONCILED daemon/<P>: stream_id "<id>" unchanged`. It fails when a recreated stream leaves a
// daemon store with the identity of a stream that is gone.
func TestHoldTheCensusReconcilesTheDaemonStoresStreamID(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	old := h.notificationsID()
	require.NotEmpty(t, old)
	stores := &fakeStores{ids: map[string]string{"A": old, "B": old}}
	families := []string{"--family", "session", "--family", "daemon/A", "--family", "daemon/B",
		"--daemon-database", "A=/dev/null", "--daemon-database", "B=/dev/null"}

	// ENVOY_NOTIFICATIONS recreated, as a hub-restore's snapshot or a streams update that made it
	// again would leave it: deleted under a hold of no steps, then made by `streams update`.
	id := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, js.DeleteStream(ctx, bus.Stream))
	nc.Close()
	code, _, _ := h.streamsRun(nil, "update", "--hold", id)
	require.Zero(t, code, "the update that makes the stream again")
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the stream is made again")
	require.Zero(t, code)
	current := h.notificationsID()
	require.NotEqual(t, old, current, "the recreated stream kept its identity")
	h.awaitStreamLeaders()

	// The first census sets its hold and stops at its convert check; every family's cutover row is
	// then written done under that hold, so each census after it reaches its reconcile.
	code, _, _ = h.streamsRun(stub, append([]string{"census"}, families...)...)
	require.Equal(t, 1, code, "the census before any family is converted")
	census := h.row().HoldID
	for _, family := range []string{"session", "daemon/A", "daemon/B"} {
		_, err := h.s.Records.PutCutover(ctx, family, "done", census)
		require.NoError(t, err)
	}

	// The control: no writer.
	code, _, stderr := h.streamsRun(stub, append([]string{"census", "--hold", census}, families...)...)
	require.Equal(t, 2, code, "control: a census with no writer of the stores")
	require.Contains(t, stderr, ErrNoStoreWriter.Error())
	require.NotContains(t, h.journalCommands(census), "streams census done")
	require.Equal(t, map[string]string{"A": old, "B": old}, stores.snapshot(), "control: the stores keep the old identity")

	code, stdout, _ := h.streamsRunWith(stub, stores.reconcile, append([]string{"census", "--hold", census}, families...)...)
	require.Zero(t, code, "the census with a writer")
	require.Equal(t, map[string]string{"A": current, "B": current}, stores.snapshot())
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/A: stream_id %q -> %q", old, current))
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/B: stream_id %q -> %q", old, current))
	require.NotContains(t, stdout, "GONE ", "a census with no store gone printed one")
	require.NotContains(t, stdout, "RECONCILED ", "a census that changed every store printed one as unchanged")
	outputs := doneOutputs(h.journal(census), CommandStreamsCensus)
	require.Contains(t, outputs, "stream_id.daemon/A")
	require.Contains(t, outputs, "stream_id.daemon/B")
	require.Equal(t, fmt.Sprintf("daemon/A: stream_id %q -> %q", old, current), outputs["stream_id.daemon/A"])
	code, _, _ = h.run(actionRelease, "--hold", census, "--reason", "the census is done")
	require.Zero(t, code)
	h.awaitStreamLeaders()

	// The stream unchanged: the next census changes nothing and names no store.
	code, stdout, _ = h.streamsRunWith(stub, stores.reconcile, append([]string{"census"}, families...)...)
	require.Zero(t, code, "the census of an unchanged stream")
	again := h.row().HoldID
	require.NotContains(t, stdout, "CHANGED daemon/")
	require.Contains(t, stdout, fmt.Sprintf("RECONCILED daemon/A: stream_id %q unchanged\n", current))
	require.Contains(t, stdout, fmt.Sprintf("RECONCILED daemon/B: stream_id %q unchanged\n", current))
	for key := range doneOutputs(h.journal(again), CommandStreamsCensus) {
		require.NotContains(t, key, "stream_id.", "the census of an unchanged stream named a store")
	}
}

// The census's word with its writer's typed result (StreamIDResult). A store the writer finds
// gone is printed as `GONE daemon/<P>: database <db> on instance <instance> (the operator's file
// states <instance>): <answer>` and named in the census's done entry as gone.daemon/<P>, beside
// each store it changed and each it found already at the stream's identity (`RECONCILED
// daemon/<P>: ...`, named in no output), and the census is done. The writer is handed the daemons'
// Deployments as the hold's own Kubernetes client lists them (KubeDaemonDeployments of namespace
// legion), which holds the Deployment the row renders there. A store the writer leaves
// unreconciled makes the census UNKNOWN (exit 2), naming the store and why, with no done entry and
// a stop that records only this hold's barrier key, which the census put before it listed, as the
// run's change (TestHoldACensusStopNamesTheBarrierKeyItPut); a store changed before one whose
// write failed is what that stop records the run left too, so the hold is not released over it,
// and the census's rerun is done. Controls: the first census, with no store gone, prints no GONE
// line (TestHoldTheCensusReconcilesTheDaemonStoresStreamID), and each assertion here fails against a
// writerhold that drops the GONE line, ignores an unreconciled store, or records the stop of a
// partial write as unchanged.
func TestHoldTheCensusReportsGoneAndUnreconciledStores(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	current := h.notificationsID()
	require.NotEmpty(t, current)
	_, err := h.s.Kube.Client.AppsV1().Deployments(DaemonNamespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "legion-daemon-g", Namespace: DaemonNamespace, Annotations: map[string]string{DaemonProjectAnnotation: "G"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	gone := GoneStore{Project: "G", Database: "daemon_g", Instance: "proj:region:daemons", StatedInstance: "proj:region:daemons",
		Answer: `FATAL: database "daemon_g" does not exist (SQLSTATE 3D000)`}
	stores := &fakeStores{ids: map[string]string{"A": "stream-0", "B": current}, gone: map[string]GoneStore{"G": gone}}
	families := []string{"--family", "session", "--family", "daemon/A", "--family", "daemon/B", "--family", "daemon/G",
		"--daemon-database", "A=/dev/null", "--daemon-database", "B=/dev/null", "--daemon-database", "G=/dev/null"}

	// The first census sets its hold and stops at its convert check; every family's cutover row is
	// then written done, so each census after it reaches its reconcile.
	code, _, _ := h.streamsRun(stub, append([]string{"census"}, families...)...)
	require.Equal(t, 1, code, "the census before any family is converted")
	census := h.row().HoldID
	for _, family := range []string{"session", "daemon/A", "daemon/B", "daemon/G"} {
		_, err := h.s.Records.PutCutover(ctx, family, "done", census)
		require.NoError(t, err)
	}

	code, stdout, stderr := h.streamsRunWith(stub, stores.reconcile, append([]string{"census", "--hold", census}, families...)...)
	require.Zero(t, code, "the census beside a gone store: %s", stderr)
	text := "database daemon_g on instance proj:region:daemons (the operator's file states proj:region:daemons): " + gone.Answer
	require.Contains(t, stdout, "GONE daemon/G: "+text+"\n")
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/A: stream_id %q -> %q", "stream-0", current))
	require.Contains(t, stdout, fmt.Sprintf("RECONCILED daemon/B: stream_id %q unchanged\n", current))
	outputs := doneOutputs(h.journal(census), CommandStreamsCensus)
	require.Equal(t, text, outputs["gone.daemon/G"])
	require.Contains(t, outputs, "stream_id.daemon/A")
	require.NotContains(t, outputs, "stream_id.daemon/G", "a gone store's identity was named as written")
	require.NotContains(t, outputs, "stream_id.daemon/B", "a store left unchanged was named as written")
	require.Contains(t, stores.listedNames(), "legion-daemon-g", "the writer was not handed the hold's own Deployments")
	code, _, _ = h.run(actionRelease, "--hold", census, "--reason", "the census beside a gone store is done")
	require.Zero(t, code)
	h.awaitStreamLeaders()

	// A store left unreconciled: UNKNOWN, no done entry, a stop recording only the barrier key.
	stores.set(func(f *fakeStores) {
		f.ids["A"] = "stream-0"
		f.unreconciled = map[string]string{"A": `Deployment legion/legion-daemon-a reads database "other"`}
	})
	code, stdout, stderr = h.streamsRunWith(stub, stores.reconcile, append([]string{"census"}, families...)...)
	unknown := h.row().HoldID
	require.Equal(t, 2, code, "a census with a store unreconciled: %s%s", stdout, stderr)
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, `daemon/A: Deployment legion/legion-daemon-a reads database "other"`)
	require.NotContains(t, h.journalCommands(unknown), CommandStreamsCensus+" done")
	last := h.journal(unknown).Ops[len(h.journal(unknown).Ops)-1]
	require.Equal(t, CommandStreamsCensus+" stop", last.Command+" "+last.State)
	require.Equal(t, "this hold's barrier key in "+kvStreamPrefix+streams.SessionConsumerRecordsBucket, last.Outputs["left"],
		"the stop records a store as written, or leaves out the barrier key the census put")
	require.Equal(t, "stream-0", stores.snapshot()["A"], "a store was written while one was unreconciled")

	// A write that failed after another: the store changed before it is what the stop records.
	stores.set(func(f *fakeStores) {
		f.unreconciled = nil
		f.ids["B"] = "stream-0"
		f.failWrite = map[string]string{"B": "its envoy.sentinel row's write failed: refused"}
	})
	code, stdout, stderr = h.streamsRunWith(stub, stores.reconcile, append([]string{"census", "--hold", unknown}, families...)...)
	require.Equal(t, 2, code, "a census whose write failed after another: %s%s", stdout, stderr)
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/A: stream_id %q -> %q", "stream-0", current))
	require.Contains(t, stderr, "daemon/B: its envoy.sentinel row's write failed: refused")
	last = h.journal(unknown).Ops[len(h.journal(unknown).Ops)-1]
	require.Equal(t, CommandStreamsCensus+" stop", last.Command+" "+last.State)
	require.Contains(t, last.Outputs["left"], fmt.Sprintf("stream_id of daemon/A: stream_id %q -> %q", "stream-0", current),
		"the stop recorded a run that rewrote a store as unchanged")
	code, _, _ = h.run(actionRelease, "--hold", unknown, "--reason", "over a census that rewrote a store and stopped")
	require.NotZero(t, code, "release over a stop that changed a store")

	stores.set(func(f *fakeStores) { f.failWrite = nil })
	code, stdout, stderr = h.streamsRunWith(stub, stores.reconcile, append([]string{"census", "--hold", unknown}, families...)...)
	require.Zero(t, code, "the census's rerun: %s%s", stdout, stderr)
	require.Contains(t, stdout, fmt.Sprintf("CHANGED daemon/B: stream_id %q -> %q", "stream-0", current))
	require.Contains(t, h.journalCommands(unknown), CommandStreamsCensus+" done")
}

// A StreamIDWriter's result without an error places each daemon family it was given in exactly one
// of its lists (StreamIDResult.Covers): a result that does passes, whatever families of no daemon
// (session, dispatch) and stores of no given family it carries, and so does an empty result for
// families of no daemon. A result missing a daemon family fails naming it, and one with a family in
// two lists, or twice in one, fails naming the family and the lists. Control: the result a writer
// answered before Reconciled, which named a store already at the stream's identity in no list,
// fails for that store.
func TestAStreamIDResultPlacesEachDaemonFamilyInOneList(t *testing.T) {
	families := []string{"session", "dispatch", "daemon/C", "daemon/G", "daemon/R", "daemon/U"}
	whole := func() StreamIDResult {
		return StreamIDResult{
			Changed:      []StreamIDChange{{Project: "C", From: "stream-1", To: "stream-2"}},
			Gone:         []GoneStore{{Project: "G", Database: "daemon_g"}},
			Reconciled:   []ReconciledStore{{Project: "R", StreamID: "stream-2"}, {Project: "X"}},
			Unreconciled: []UnreconciledStore{{Project: "U", Reason: "its server does not answer"}},
		}
	}
	require.NoError(t, whole().Covers(families))
	require.NoError(t, StreamIDResult{}.Covers([]string{"session", "dispatch"}), "an empty result for families of no daemon")

	missing := whole()
	missing.Gone = nil
	require.EqualError(t, missing.Covers(families), "the daemon stores' stream-id result places a daemon family in no list or in more than one: "+
		"daemon/G is in none of Changed, Gone, Reconciled and Unreconciled")

	twice := whole()
	twice.Unreconciled = append(twice.Unreconciled, UnreconciledStore{Project: "C", Reason: "not written"})
	twice.Reconciled = append(twice.Reconciled, ReconciledStore{Project: "R", StreamID: "stream-2"})
	err := twice.Covers(families)
	require.ErrorContains(t, err, "daemon/C is in 2 lists: Changed, Unreconciled")
	require.ErrorContains(t, err, "daemon/R is in 2 lists: Reconciled, Reconciled")
	require.NotContains(t, err.Error(), "daemon/G", "a family in one list was named")

	// Control: the result before Reconciled, a store already at the identity in no list.
	before := whole()
	before.Reconciled = nil
	require.ErrorContains(t, before.Covers(families), "daemon/R is in none of Changed, Gone, Reconciled and Unreconciled")
}

// The census's reconcile of the daemon stores (reconcileStreamIDs) prints each store its writer
// found already at the stream's identity, which it left as it was, as `RECONCILED daemon/<P>:
// stream_id "<id>" unchanged`, and one that records no identity yet as `RECONCILED daemon/<P>: no
// stream_id recorded, unchanged`, after each CHANGED line; it names neither in the done entry's
// outputs, which name what the census wrote or found gone. Red before the census printed the
// writer's Reconciled: both lines were missing.
func TestTheCensusPrintsEachStoreItsWriterReconciled(t *testing.T) {
	writer := func(context.Context, *nats.Conn, DaemonDeployments, []string, []string) (StreamIDResult, error) {
		return StreamIDResult{
			Changed:    []StreamIDChange{{Project: "A", From: "stream-1", To: "stream-2"}},
			Reconciled: []ReconciledStore{{Project: "B", StreamID: "stream-2"}, {Project: "F"}},
		}, nil
	}
	var stdout bytes.Buffer
	outputs, err := reconcileStreamIDs(context.Background(), nil, writer, nil, []string{"daemon/A", "daemon/B", "daemon/F"}, nil, &stdout)
	require.NoError(t, err)
	require.Equal(t, `CHANGED daemon/A: stream_id "stream-1" -> "stream-2"`+"\n"+
		`RECONCILED daemon/B: stream_id "stream-2" unchanged`+"\n"+
		"RECONCILED daemon/F: no stream_id recorded, unchanged\n", stdout.String())
	require.Equal(t, map[string]string{"stream_id.daemon/A": `daemon/A: stream_id "stream-1" -> "stream-2"`}, outputs)
}
