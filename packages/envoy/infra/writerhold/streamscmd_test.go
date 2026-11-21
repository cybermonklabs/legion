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
func noStores(context.Context, *nats.Conn, []string, []string) ([]string, error) { return nil, nil }

// noFreshNames is a reader of the recorded fresh names for a stack with no selection record: it
// names none.
func noFreshNames(context.Context, *nats.Conn, []string, string, []string) (map[string][]string, error) {
	return map[string][]string{}, nil
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

// requireStreamAbsent reads stream as the operator and requires the server's `stream not found`.
func (h *heldStack) requireStreamAbsent(stream string) {
	h.t.Helper()
	nc, err := h.s.ConnectOperator(context.Background())
	require.NoError(h.t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	_, err = js.Stream(context.Background(), stream)
	require.ErrorIs(h.t, err, jetstream.ErrStreamNotFound, "read %s", stream)
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
	// The seed makes the protected streams alone (streamset.EnsureStreams), so ENVOY_MEASURE is
	// absent, and the update is its one maker.
	h.requireStreamAbsent(streamset.MeasureStream)
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

// t-10's `streams update` makes no copied bucket (#4687's rule, streams.Update): in a set
// streams-update hold with KV_envoy_roles gone, the update reports the bucket missing, naming
// `envoy-infra buckets create`, the one command that makes a copied bucket, and exits 1, its stop
// recorded; the bucket stays absent. The control is the same update once the bucket is back, as
// envoy-infra buckets create makes it (bus.CreateCopiedBucket): it is done. At 415f831b9a, before
// #4687's a1d6c726ea, the update made no bucket either, but only because #4296's open-only ensure
// refused it through a failed STREAM.INFO ("bucket is missing; only `envoy-infra buckets` creates
// it"), and this row fails there at the finding's text.
func TestHoldTheUpdateMakesNoCopiedBucket(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	roles := kvStreamPrefix + bus.RoleClaimBucket
	id := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	operator := func(do func(nc *nats.Conn, js jetstream.JetStream)) {
		t.Helper()
		nc, err := h.s.ConnectOperator(ctx)
		require.NoError(t, err)
		defer nc.Close()
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		do(nc, js)
	}
	operator(func(_ *nats.Conn, js jetstream.JetStream) { require.NoError(t, js.DeleteStream(ctx, roles)) })

	code, stdout, stderr := h.streamsRun(nil, "update", "--hold", id)
	require.Equal(t, 1, code, "an update with a copied bucket missing")
	require.Contains(t, stdout+stderr, roles)
	require.Contains(t, stdout+stderr, "only `envoy-infra buckets create` makes")
	h.requireStreamAbsent(roles)
	ops := h.journal(id).Ops
	last := ops[len(ops)-1]
	require.Equal(t, CommandStreamsUpdate+" stop", last.Command+" "+last.State, "the update's last entry")

	operator(func(nc *nats.Conn, _ jetstream.JetStream) {
		legacy, err := nc.JetStream(nats.MaxWait(time.Minute))
		require.NoError(t, err)
		_, err = bus.CreateCopiedBucket(ctx, nc, legacy, bus.RoleClaimBucket, 3)
		require.NoError(t, err, "make %s again as envoy-infra buckets create makes it", roles)
	})
	h.awaitStreamLeaders()
	code, stdout, _ = h.streamsRun(nil, "update", "--hold", id)
	require.Zero(t, code, "control: the update with every copied bucket there")
	require.Contains(t, stdout, "streams update done in hold")
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
// `unchanged`. The control is a census killed after its intent, with no stop: release is refused
// while that intent is pending.
func TestHoldReleaseAfterAStreamsCensus(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }

	// No reader: UNKNOWN, and a stop entry that left nothing changed.
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
	require.Equal(t, holdrecord.LeftUnchanged, last.Outputs["left"])

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

// The census reads the recorded fresh consumer names (consumernames.RecordedFreshNames, passed
// in) before it removes anything, and passes them to streams.Census as RecordedFreshNames. It gives
// the reader its --family values, --dispatch-database-file and each --daemon-database entry. With
// no reader, or a reader that cannot read a store, the census is UNKNOWN (exit 2) and writes no
// done entry. The control is the same census with a reader, which is done. That an unrecorded
// fresh-form consumer is removed and a recorded one kept is streams.Census's own rule, which #4687
// tests, and t-7's rows test with the real reader.
func TestHoldTheCensusReadsTheRecordedFreshNames(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }
	args := []string{"census", "--family", "session", "--dispatch-database-file", "/run/dispatch-db", "--daemon-database", "P=/run/daemon-db"}

	// The first census sets its hold and stops at its convert check, which streams.Census refuses
	// with a zero report, so it prints none; the session family's cutover row is then written done
	// under that hold.
	code, stdout, _ := h.streamsRunWith(stub, noStores, args...)
	require.Equal(t, 1, code, "the census before the family is converted")
	require.NotContains(t, stdout, "CLEAN", "a census refused at its convert check printed a clean report")
	id := h.row().HoldID
	_, err := h.s.Records.PutCutover(ctx, "session", "done", id)
	require.NoError(t, err)
	held := append([]string{"census", "--hold", id}, args[1:]...)

	// A census that stops before streams.Census answers prints no report: no "CLEAN" beside its
	// UNKNOWN.
	code, stdout, stderr := h.streamsRunReaders(CensusReaders{Recorded: stub, Stores: noStores}, held...)
	require.Equal(t, 2, code, "a census with no reader of the fresh names")
	require.Contains(t, stderr, ErrNoFreshNamesReader.Error())
	require.NotContains(t, stdout, "CLEAN", "an UNKNOWN census printed a clean report")
	failing := func(context.Context, *nats.Conn, []string, string, []string) (map[string][]string, error) {
		return nil, errors.New("the dispatch store cannot be read")
	}
	code, stdout, stderr = h.streamsRunReaders(CensusReaders{Recorded: stub, FreshNames: failing, Stores: noStores}, held...)
	require.Equal(t, 2, code, "a census whose fresh-names reader fails")
	require.Contains(t, stderr, "the dispatch store cannot be read")
	require.NotContains(t, stdout, "CLEAN", "an UNKNOWN census printed a clean report")
	require.NotContains(t, h.journalCommands(id), CommandStreamsCensus+" done")

	var gotFamilies, gotDaemons []string
	var gotDispatch string
	reader := func(_ context.Context, _ *nats.Conn, families []string, dispatch string, daemons []string) (map[string][]string, error) {
		gotFamilies, gotDispatch, gotDaemons = families, dispatch, daemons
		return map[string][]string{"session": {}}, nil
	}
	code, stdout, _ = h.streamsRunReaders(CensusReaders{Recorded: stub, FreshNames: reader, Stores: noStores}, held...)
	require.Zero(t, code, "the census with a reader of the fresh names")
	require.Contains(t, stdout, "CLEAN", "control: the census that ran prints its report")
	require.Equal(t, []string{"session"}, gotFamilies)
	require.Equal(t, "/run/dispatch-db", gotDispatch)
	require.Equal(t, []string{"P=/run/daemon-db"}, gotDaemons)
	require.Contains(t, h.journalCommands(id), CommandStreamsCensus+" done")
}

// A streams run's changes reach the postcheck for the protected streams alone: a made protected
// stream is recreated, and the made ENVOY_MEASURE, the one declared stream the hold does not
// protect (streamset.MeasureStream), is left out. The postcheck compares only what step 1 read, so
// a change it is handed to ENVOY_MEASURE stops every update that makes it UNKNOWN. A raised stream
// and a consumer's removal are not a stream's make.
func TestHoldTheMeasureStreamIsNotPostchecked(t *testing.T) {
	roles := kvStreamPrefix + bus.RoleClaimBucket
	changes := streamChanges(streams.Report{Changes: []streams.Finding{
		{Stream: streamset.MeasureStream, What: "made"},
		{Stream: roles, What: "made"},
		{Stream: bus.Stream, What: "raised"},
		{Stream: bus.Stream, Consumer: "old", What: "removed"},
	}})
	require.Equal(t, map[string]Change{roles: {Kind: ChangeRecreated}}, changes)
}
