package writerhold

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// streamsRun runs `envoy-infra streams <args>` in the stack with the hold's flags, recorded the
// census's reader of the recorded session consumers, a reader of fresh names that names none, and
// no writer of the daemon stores.
func (h *heldStack) streamsRun(recorded RecordedSessionConsumers, args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsRunWith(recorded, nil, args...)
}

// streamsRunWith is streamsRun with stores, the census's writer of the daemon stores' stream ids.
func (h *heldStack) streamsRunWith(recorded RecordedSessionConsumers, stores StreamIDWriter, args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsRunReaders(CensusReaders{Recorded: recorded, FreshNames: noFreshNames, Stores: stores}, args...)
}

// streamsRunReaders runs `envoy-infra streams <args>` in the stack with readers.
func (h *heldStack) streamsRunReaders(readers CensusReaders, args ...string) (int, string, string) {
	h.t.Helper()
	full := append(append([]string{}, args...), commonArgs...)
	full = append(full, "--minters-report", h.report, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null", "--replicas", "3")
	if !strings.Contains(strings.Join(args, " "), "--reason") {
		full = append(full, "--reason", "the row's streams "+args[0])
	}
	var stdout, stderr bytes.Buffer
	code := runStreams(context.Background(), full, &stdout, &stderr, streamsEnv{env: h.e, readers: readers})
	h.t.Logf("envoy-infra streams %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// noStores is a writer of the daemon stores' stream ids for a stack with no daemon store: it
// changes nothing.
func noStores(context.Context, *nats.Conn, DaemonDeployments, []string, []string) (StreamIDResult, error) {
	return StreamIDResult{}, nil
}

// noFreshNames is a reader of the recorded fresh names for a stack with no selection record: it
// names none.
func noFreshNames(context.Context, *nats.Conn, DaemonDeployments, []string, string, []string) (FreshNamesRead, error) {
	return FreshNamesRead{Names: map[string][]string{}}, nil
}

// journalCommands are the hold's journal entries, each "<command> <state>".
func (h *heldStack) journalCommands(id string) []string {
	h.t.Helper()
	var out []string
	for _, op := range h.journal(id).Ops {
		out = append(out, op.Command+" "+op.State)
	}
	return out
}

// streamReplicas reads stream's replica count as the operator.
func (h *heldStack) streamReplicas(stream string) int {
	h.t.Helper()
	nc, err := h.s.ConnectOperator(context.Background())
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	s, err := js.Stream(context.Background(), stream)
	require.NoError(h.t, err, "read %s", stream)
	return s.CachedInfo().Config.Replicas
}

// setReplicas updates stream to replicas copies as the operator under a hold of no steps.
func (h *heldStack) setReplicas(stream string, replicas int) {
	h.t.Helper()
	ctx := context.Background()
	id := h.s.Prepare(h.t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(h.t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	s, err := js.Stream(ctx, stream)
	require.NoError(h.t, err)
	config := s.CachedInfo().Config
	config.Replicas = replicas
	_, err = js.UpdateStream(ctx, config)
	require.NoError(h.t, err, "set %s to %d copies", stream, replicas)
	nc.Close()
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: CommandStreamsUpdate}, nil)
	require.NoError(h.t, err)
	require.NoError(h.t, h.s.Records.AppendDone(ctx, id, CommandStreamsUpdate, nil))
	h.s.Release(h.t, id)
	h.awaitStreamLeaders()
}

// deleteStream deletes stream as the operator under a hold of no steps, which it then releases.
func (h *heldStack) deleteStream(stream string) {
	h.t.Helper()
	ctx := context.Background()
	id := h.s.Prepare(h.t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(h.t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	require.NoError(h.t, js.DeleteStream(ctx, stream), "delete %s", stream)
	nc.Close()
	h.s.Release(h.t, id)
}

// The streams commands (t-e-58, review H3; option A): the operator's released grant reads only,
// so it makes no stream, ENVOY_MEASURE included, and changes none. As the `measure` entry,
// `streams check` reports a copied bucket at one replica fewer than the build's. Then `streams
// update` in its own hold makes the missing ENVOY_MEASURE and brings the bucket to the build's
// count, its postcheck passes, and it writes the hold's journal: hold set, then the update's intent
// and its done entry. Controls: under Operator(OperatorPhaseReleased), a STREAM.CREATE of
// ENVOY_MEASURE, KV_legion_agents, KV_envoy_webhook_redelivery and ENVOY_NOTIFICATIONS, and a
// STREAM.UPDATE of KV_envoy_sessions, are each refused. It fails when the operator's released grant
// can make or change a stream, or when the held update cannot repair what check reports.
func TestHoldTheStreamsCommands(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	settings := streamset.Settings{Replicas: 3}
	h.deleteStream(streamset.MeasureStream)
	bucket := kvStreamPrefix + bus.RoleClaimBucket
	h.setReplicas(bucket, 2)

	// The controls: the released grant makes and changes no stream. The test's own operator
	// connection ends before any hold: a second operator connection, reconnecting through the
	// hold's restarts, is one the hold's report refuses (TestHoldReportASecondOperator).
	released, err := h.s.ConnectOperator(ctx)
	require.NoError(t, err)
	for _, request := range []struct{ subject, stream string }{
		{"$JS.API.STREAM.CREATE." + streamset.MeasureStream, streamset.MeasureStream},
		{"$JS.API.STREAM.CREATE." + kvStreamPrefix + bus.LegionAgentBucket, kvStreamPrefix + bus.LegionAgentBucket},
		{"$JS.API.STREAM.CREATE." + kvStreamPrefix + bus.WebhookRedeliveryBucket, kvStreamPrefix + bus.WebhookRedeliveryBucket},
		{"$JS.API.STREAM.CREATE." + bus.Stream, bus.Stream},
		{"$JS.API.STREAM.UPDATE." + kvStreamPrefix + bus.SessionBucket, kvStreamPrefix + bus.SessionBucket},
	} {
		_, err := released.Request(request.subject, []byte(`{"name":"`+request.stream+`","subjects":["x.>"]}`), 3*time.Second)
		require.Error(t, err, "control: the released grant was answered on %s", request.subject)
	}
	released.Close()

	measure, err := nats.Connect(h.s.Cluster.URL(), nats.UserInfo("service", holdtest.MeasureToken), nats.CustomInboxPrefix("_INBOX_envoy_measure"), nats.Timeout(10*time.Second))
	require.NoError(t, err, "the measure entry was refused")
	checked := streams.Check(ctx, measure, settings)
	measure.Close()
	require.Contains(t, strings.Join(checked.Lines(), "\n"), bucket, "check did not report %s at 2 copies", bucket)
	require.Equal(t, 1, checked.ExitCode(), "check: %v", checked.Lines())

	code, stdout, _ := h.streamsRun(nil, "update")
	require.Zero(t, code, "streams update")
	require.Contains(t, stdout, "streams update done in hold")
	require.Equal(t, 3, h.streamReplicas(bucket), "the update did not bring %s to 3 copies", bucket)
	require.Equal(t, 3, h.streamReplicas(streamset.MeasureStream), "the update did not make %s", streamset.MeasureStream)
	id := h.row().HoldID
	require.Equal(t, []string{"hold set intent", "hold set done", "streams update intent", "streams update done"}, h.journalCommands(id))
	require.True(t, h.journal(id).Complete)
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the update is done")
	require.Zero(t, code)
}

// Each streams word runs only in a hold whose workflow names it: `streams update --hold` of a
// buckets-create hold is refused naming the workflow, and so is `streams census --hold` of a
// streams-update hold, the table admitting it in none but streams-census, the cutover and
// hub-restore; each writes nothing to that hold's journal. The control is each in its own hold,
// which writes its intent and then its done entry (TestHoldTheStreamsCommands for update,
// TestHoldReleaseAfterAStreamsCensus for the census).
func TestHoldTheStreamsWordsRunOnlyInTheirHolds(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()

	id := h.s.Prepare(t, holdrecord.WorkflowBucketsCreate)
	h.s.HoldWithoutTheSteps(t, id)
	before := h.journalCommands(id)
	code, _, stderr := h.streamsRun(nil, "update", "--hold", id)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "runs only in a streams-update hold")
	require.Equal(t, before, h.journalCommands(id), "the refused update wrote to the journal")
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: "buckets create"}, nil)
	require.NoError(t, err)
	require.NoError(t, h.s.Records.AppendDone(ctx, id, "buckets create", nil))
	h.s.Release(t, id)

	id = h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	before = h.journalCommands(id)
	code, _, stderr = h.streamsRun(func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil },
		"census", "--hold", id, "--family", "session")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "streams census")
	require.Equal(t, before, h.journalCommands(id), "the refused census wrote to the journal")
}

// `hold release` succeeds after a streams-census hold: the census, its only command, writes its
// intent and its done entry, which completes the record. The census runs with the stack's every
// family converted (each cutover row done) and a stub reader of the recorded session consumers.
// A census with no reader is UNKNOWN (exit 2) and, as every command can, writes a stop entry,
// which names this hold's barrier key the census put before it reached the reader
// (TestHoldACensusStopNamesTheBarrierKeyItPut). The control is a census killed after its intent,
// with no stop: release is refused while that intent is pending.
func TestHoldReleaseAfterAStreamsCensus(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }

	// No reader: UNKNOWN, and a stop entry that names the barrier key it put.
	code, _, stderr := h.streamsRun(nil, "census", "--family", "session")
	id := h.row().HoldID
	_, err := h.s.Records.PutCutover(ctx, "session", "done", id)
	require.NoError(t, err)
	if code != 2 {
		// The first run may stop at its cutover check before the reader; with the row done the
		// census reaches the reader.
		code, _, stderr = h.streamsRun(nil, "census", "--hold", id, "--family", "session")
	}
	require.Equal(t, 2, code, "a census with no reader")
	require.Contains(t, stderr, ErrNoRecordReader.Error())
	last := h.journal(id).Ops[len(h.journal(id).Ops)-1]
	require.Equal(t, CommandStreamsCensus+" stop", last.Command+" "+last.State)
	require.Equal(t, "this hold's barrier key in "+kvStreamPrefix+streams.SessionConsumerRecordsBucket, last.Outputs["left"])

	// The control: the census run again and killed after its intent, with no stop.
	_, err = h.s.Records.Admit(ctx, id, holdrecord.Request{Command: CommandStreamsCensus}, nil)
	require.NoError(t, err)
	code, _, stderr = h.run(actionRelease, "--hold", id, "--reason", "over a census killed after its intent")
	require.Equal(t, 1, code, "control: release over a pending census intent")
	require.Contains(t, stderr, holdrecord.ErrBusy.Error())

	code, stdout, _ := h.streamsRunWith(stub, noStores, "census", "--hold", id, "--family", "session")
	require.Zero(t, code, "the census with a reader")
	require.Contains(t, stdout, "streams census done in hold")
	require.Contains(t, h.journalCommands(id), "streams census done")
	require.True(t, h.journal(id).Complete, "the census did not complete the record")
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the census is done")
	require.Zero(t, code, "release after the census")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
}

// A census stop counts this hold's barrier key, which the census put in the session consumers'
// bucket before it read their records (streams.Census's Barrier): a stop names every change that
// its command's runs made and did not undo. With no reader of the recorded session consumers the
// census is UNKNOWN after its barrier and removes nothing; its stop names the key, and hold
// release is refused over it. Control: under the earlier rule that the key is no change
// (censusBarrierNoChange), the same census records unchanged, and hold release is admitted over it.
func TestHoldACensusStopNamesTheBarrierKeyItPut(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()

	// The first census sets its hold and stops at its convert check, before any barrier; the
	// session family's cutover row is then written done under that hold.
	code, _, _ := h.streamsRun(nil, "census", "--family", "session")
	require.Equal(t, 1, code, "the census before the family is converted")
	id := h.row().HoldID
	require.Equal(t, holdrecord.LeftUnchanged, h.journal(id).Ops[len(h.journal(id).Ops)-1].Outputs["left"], "a stop before the barrier")
	_, err := h.s.Records.PutCutover(ctx, "session", "done", id)
	require.NoError(t, err)
	release := holdrecord.Request{Command: holdrecord.CommandRelease}
	unknown := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := h.streamsRun(nil, "census", "--hold", id, "--family", "session")
		require.Equal(t, 2, code, "a census with no reader")
		require.Contains(t, stderr, ErrNoRecordReader.Error(), "the hold point: the census reached its reader after its barrier")
		last := h.journal(id).Ops[len(h.journal(id).Ops)-1]
		require.Equal(t, CommandStreamsCensus+" stop", last.Command+" "+last.State)
		return last
	}

	censusBarrierNoChange = true
	t.Cleanup(func() { censusBarrierNoChange = false })
	require.Equal(t, holdrecord.LeftUnchanged, unknown().Outputs["left"], "control: with the key no change the stop records unchanged")
	require.NoError(t, holdrecord.Decide(h.journal(id), release), "control: hold release is admitted over the unchanged stop")

	censusBarrierNoChange = false
	require.Equal(t, "this hold's barrier key in "+kvStreamPrefix+streams.SessionConsumerRecordsBucket, unknown().Outputs["left"])
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over a stop that left the key")
}

// StreamsCommandOn runs the held words on a stack its caller names: `streams update` through it,
// with the stack's broker client and Operator, prepares and sets a streams-update hold, runs the
// update and writes its done entry, as StreamsCommand does through the flags. The control is the
// same run with no Operator to reach, which stops before any hold.
func TestStreamsCommandOnRunsAHeldWordOnTheCallersStack(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	args := append([]string{"update", "--reason", "the seam", "--replicas", "3", "--minters-report", h.report,
		"--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null"}, commonArgs...)
	deps := StreamsDeps{
		Records:  func(context.Context, bool) (*holdrecord.Client, error) { return h.s.Broker.Client(t), nil },
		Operator: func(context.Context) (*Operator, error) { return h.op, nil },
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, StreamsCommandOn(context.Background(), args, &stdout, &stderr, deps), "%s%s", stdout.String(), stderr.String())
	id := h.row().HoldID
	require.Contains(t, h.journalCommands(id), CommandStreamsUpdate+" done")
	code, _, _ := h.run(actionRelease, "--hold", id, "--reason", "the seam's update is done")
	require.Zero(t, code)

	deps.Operator = func(context.Context) (*Operator, error) { return nil, errors.New("no hub here") }
	stdout.Reset()
	stderr.Reset()
	require.Equal(t, 1, StreamsCommandOn(context.Background(), args, &stdout, &stderr, deps), "control")
	require.Contains(t, stderr.String(), "no hub here")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase, "control: a hold was set with no hub")
}

// The census reads the recorded fresh consumer names (consumernames.RecordedFreshNames, passed in)
// through streams.Census's RecordedFreshNames, after its checks and its barrier and before it
// removes anything. It gives the reader its --family values, --dispatch-database-file, each
// --daemon-database entry, and the daemons' Deployments as the hold's own Kubernetes client lists
// them (KubeDaemonDeployments of namespace legion), which hold the Deployment the row renders there.
// With no reader, or a reader that cannot read a store, the census is UNKNOWN (exit 2) and writes no
// done entry. The control is the same census with a reader, which is done. That an unrecorded
// fresh-form consumer is removed and a recorded one kept is streams.Census's own rule, which #4687
// tests, and consumer-names' census rows (packages/envoy/infra/consumernames) test with the real
// reader, a gone daemon store's consumers too.
func TestHoldTheCensusReadsTheRecordedFreshNames(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	args := []string{"census", "--family", "session", "--dispatch-database-file", "/run/dispatch-db", "--daemon-database", "P=/run/daemon-db"}

	// The first census sets its hold and stops at its convert check; the session family's cutover
	// row is then written done under that hold.
	code, _, _ := h.streamsRunWith(stub, noStores, args...)
	require.Equal(t, 1, code, "the census before the family is converted")
	id := h.row().HoldID
	_, err := h.s.Records.PutCutover(ctx, "session", "done", id)
	require.NoError(t, err)
	held := append([]string{"census", "--hold", id}, args[1:]...)

	code, _, stderr := h.streamsRunReaders(CensusReaders{Recorded: stub, Stores: noStores}, held...)
	require.Equal(t, 2, code, "a census with no reader of the fresh names")
	require.Contains(t, stderr, ErrNoFreshNamesReader.Error())
	failing := func(context.Context, *nats.Conn, DaemonDeployments, []string, string, []string) (FreshNamesRead, error) {
		return FreshNamesRead{}, errors.New("the dispatch store cannot be read")
	}
	code, _, stderr = h.streamsRunReaders(CensusReaders{Recorded: stub, FreshNames: failing, Stores: noStores}, held...)
	require.Equal(t, 2, code, "a census whose fresh-names reader fails")
	require.Contains(t, stderr, "the dispatch store cannot be read")
	require.NotContains(t, h.journalCommands(id), CommandStreamsCensus+" done")

	_, err = h.s.Kube.Client.AppsV1().Deployments(DaemonNamespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "legion-daemon-p", Namespace: DaemonNamespace, Annotations: map[string]string{DaemonProjectAnnotation: "P"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	var gotFamilies, gotDaemons, gotDeployments []string
	var gotDispatch string
	reader := func(ctx context.Context, _ *nats.Conn, deployments DaemonDeployments, families []string, dispatch string, daemons []string) (FreshNamesRead, error) {
		gotFamilies, gotDispatch, gotDaemons = families, dispatch, daemons
		listed, err := deployments(ctx)
		if err != nil {
			return FreshNamesRead{}, err
		}
		for _, d := range listed {
			gotDeployments = append(gotDeployments, d.Name+" "+d.Annotations[DaemonProjectAnnotation])
		}
		return FreshNamesRead{Names: map[string][]string{"session": {}}}, nil
	}
	code, _, _ = h.streamsRunReaders(CensusReaders{Recorded: stub, FreshNames: reader, Stores: noStores}, held...)
	require.Zero(t, code, "the census with a reader of the fresh names")
	require.Equal(t, []string{"session"}, gotFamilies)
	require.Equal(t, "/run/dispatch-db", gotDispatch)
	require.Equal(t, []string{"P=/run/daemon-db"}, gotDaemons)
	require.Equal(t, []string{"legion-daemon-p P"}, gotDeployments, "the reader was not handed the hold's own Deployments")
	require.Contains(t, h.journalCommands(id), CommandStreamsCensus+" done")
}

// The census reads the recorded fresh names only behind its bucket barrier: its reader of them
// (consumernames.RecordedFreshNames, here a probe) is called once this hold's barrier key
// hold.barrier.<hold id> is in the session consumers' bucket, so every record it reads is one the
// bucket's leader has applied, and a listener durable a record names is never removed as an
// unrecorded <base>~<nuid>. The probe reads the key's last message when it is called; each census
// runs in a hold of its own, where no command put the key before it. Control: under the earlier
// order (censusFreshNamesFirst), the reader is called before the census's checks and barrier, and
// the probe finds no key.
func TestHoldTheCensusReadsTheFreshNamesBehindTheBarrier(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	var seen []bool
	probe := func(ctx context.Context, conn *nats.Conn, _ DaemonDeployments, _ []string, _ string, _ []string) (FreshNamesRead, error) {
		id := h.row().HoldID
		value, err := lastBySubject(ctx, conn, streams.SessionConsumerRecordsBucket, "hold.barrier."+id)
		seen = append(seen, err == nil && value == id)
		return FreshNamesRead{Names: map[string][]string{}}, nil
	}
	// census runs `streams census --family session` with no --hold, so it sets a hold of its own,
	// and with no writer of the daemon stores: past its convert check it lists and stops UNKNOWN at
	// its reconcile. abort ends its hold, so the next census sets another.
	census := func(converted bool) string {
		t.Helper()
		code, _, stderr := h.streamsRunReaders(CensusReaders{Recorded: stub, FreshNames: probe}, "census", "--family", "session")
		if !converted {
			require.Equal(t, 1, code, "the census before the family is converted: %s", stderr)
		} else {
			require.Equal(t, 2, code, "a census with no writer of the daemon stores: %s", stderr)
			require.Contains(t, stderr, ErrNoStoreWriter.Error(), "the hold point: the census listed, then stopped at its reconcile")
		}
		return h.row().HoldID
	}
	abort := func(id string) {
		t.Helper()
		code, _, stderr := h.run(actionAbort, "--hold", id, "--reason", "the row's census stopped")
		require.Zero(t, code, "hold abort after the census's stop: %s", stderr)
	}

	// The first census stops at its convert check, before its barrier, having read no fresh names;
	// the session family's cutover row is then written done.
	first := census(false)
	require.Empty(t, seen, "the census read the fresh names before its convert check and its barrier")
	_, err := h.s.Records.PutCutover(ctx, "session", "done", first)
	require.NoError(t, err)
	abort(first)

	abort(census(true))
	require.Equal(t, []bool{true}, seen, "the census read the fresh names before this hold's barrier key was in the bucket")

	censusFreshNamesFirst = true
	t.Cleanup(func() { censusFreshNamesFirst = false })
	abort(census(true))
	require.Equal(t, []bool{true, false}, seen, "control: in the earlier order the fresh names are read before the barrier")
}

// KubeDaemonDeployments lists the Deployments of namespace legion alone, each by its name with its
// annotations, and answers a list the API server refuses as its error. Control: a Deployment of
// another namespace is not listed.
func TestKubeDaemonDeploymentsListsTheDaemonsNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "legion-daemon-a", Namespace: DaemonNamespace,
			Annotations: map[string]string{DaemonProjectAnnotation: "A", DaemonStoreDatabaseAnnotation: "daemon_a"}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "envoy",
			Annotations: map[string]string{DaemonProjectAnnotation: "B"}}},
	)
	listed, err := KubeDaemonDeployments(client)(ctx)
	require.NoError(t, err)
	require.Equal(t, []DaemonDeployment{{Name: "legion-daemon-a",
		Annotations: map[string]string{DaemonProjectAnnotation: "A", DaemonStoreDatabaseAnnotation: "daemon_a"}}}, listed)

	client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("deployments is forbidden")
	})
	_, err = KubeDaemonDeployments(client)(ctx)
	require.ErrorContains(t, err, "deployments is forbidden")
	require.ErrorContains(t, err, "namespace legion")
}
