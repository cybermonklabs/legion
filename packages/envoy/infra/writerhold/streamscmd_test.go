package writerhold

import (
	"bytes"
	"context"
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
// census's reader of the recorded session consumers, and no writer of the daemon stores.
func (h *heldStack) streamsRun(recorded RecordedSessionConsumers, args ...string) (int, string, string) {
	h.t.Helper()
	return h.streamsRunWith(recorded, nil, args...)
}

// streamsRunWith is streamsRun with stores, the census's writer of the daemon stores' stream ids.
func (h *heldStack) streamsRunWith(recorded RecordedSessionConsumers, stores StreamIDWriter, args ...string) (int, string, string) {
	h.t.Helper()
	full := append(append([]string{}, args...), commonArgs...)
	full = append(full, "--minters-report", h.report, "--nats", h.s.Cluster.URL(), "--probe-token-file", "/dev/null", "--replicas", "3")
	if !strings.Contains(strings.Join(args, " "), "--reason") {
		full = append(full, "--reason", "the row's streams "+args[0])
	}
	var stdout, stderr bytes.Buffer
	code := runStreams(context.Background(), full, &stdout, &stderr, streamsEnv{env: h.e, recorded: recorded, stores: stores})
	h.t.Logf("envoy-infra streams %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// noStores is a writer of the daemon stores' stream ids for a stack with no daemon store: it
// changes nothing.
func noStores(context.Context, *nats.Conn, []string, []string) ([]string, error) { return nil, nil }

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

// The streams commands (t-e-58, review H3): under exactly Operator(OperatorPhaseReleased),
// `streams ensure` makes a missing ENVOY_MEASURE and nothing else. As the `measure` entry,
// `streams check` reports a copied bucket at one replica fewer than the build's. Then `streams
// update` in its own hold brings it to the build's count, and its postcheck passes, writing the
// hold's journal: hold set, then the update's intent and its done entry. Controls: under
// Operator(OperatorPhaseReleased), a STREAM.CREATE of KV_legion_agents, KV_envoy_webhook_redelivery
// and ENVOY_NOTIFICATIONS, and a STREAM.UPDATE of KV_envoy_sessions, are each refused. It fails when
// the operator's released grant can make or change a protected stream, or when the held update
// cannot repair the shortfall check reports.
func TestHoldTheStreamsCommands(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	settings := streamset.Settings{Replicas: 3}
	released, err := h.s.ConnectOperator(ctx)
	require.NoError(t, err)
	// The test's own operator connection ends before any hold: a second operator connection,
	// reconnecting through the hold's restarts, is one the hold's report refuses
	// (TestHoldReportASecondOperator).
	defer released.Close()
	rowPhase := func(ctx context.Context) (natsgrant.OperatorPhase, error) {
		row, err := h.s.Records.Row(ctx)
		return row.Phase, err
	}

	report, err := streams.Ensure(ctx, released, settings, rowPhase)
	require.NoError(t, err)
	require.Len(t, report.Changes, 1, "ensure changed more than ENVOY_MEASURE: %v", report.Lines())
	require.Equal(t, streamset.MeasureStream, report.Changes[0].Stream)

	// The controls: the released grant makes and changes no protected stream.
	for _, request := range []struct{ subject, stream string }{
		{"$JS.API.STREAM.CREATE." + kvStreamPrefix + bus.LegionAgentBucket, kvStreamPrefix + bus.LegionAgentBucket},
		{"$JS.API.STREAM.CREATE." + kvStreamPrefix + bus.WebhookRedeliveryBucket, kvStreamPrefix + bus.WebhookRedeliveryBucket},
		{"$JS.API.STREAM.CREATE." + bus.Stream, bus.Stream},
		{"$JS.API.STREAM.UPDATE." + kvStreamPrefix + bus.SessionBucket, kvStreamPrefix + bus.SessionBucket},
	} {
		_, err := released.Request(request.subject, []byte(`{"name":"`+request.stream+`","subjects":["x.>"]}`), 3*time.Second)
		require.Error(t, err, "control: the released grant was answered on %s", request.subject)
	}

	released.Close()
	bucket := kvStreamPrefix + bus.RoleClaimBucket
	h.setReplicas(bucket, 2)
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
// The control is a census with no reader, which is UNKNOWN (exit 2) and writes no done entry, so
// release is refused, naming the pending census.
func TestHoldReleaseAfterAStreamsCensus(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	stub := func(context.Context, *nats.Conn) ([]streams.ConsumerShape, error) { return nil, nil }

	// The control: no reader.
	code, _, stderr := h.streamsRun(nil, "census", "--family", "session")
	id := h.row().HoldID
	_, err := h.s.Records.PutCutover(ctx, "session", "done", id)
	require.NoError(t, err)
	if code != 2 {
		// The first run may stop at its cutover check before the reader; with the row done the
		// census reaches the reader.
		code, _, stderr = h.streamsRun(nil, "census", "--hold", id, "--family", "session")
	}
	require.Equal(t, 2, code, "control: a census with no reader")
	require.Contains(t, stderr, ErrNoRecordReader.Error())
	require.NotContains(t, h.journalCommands(id), "streams census done")
	code, _, stderr = h.run(actionRelease, "--hold", id, "--reason", "over a census with no done entry")
	require.Equal(t, 1, code, "control: release over a census with no done entry")

	code, stdout, _ := h.streamsRunWith(stub, noStores, "census", "--hold", id, "--family", "session")
	require.Zero(t, code, "the census with a reader")
	require.Contains(t, stdout, "streams census done in hold")
	require.Contains(t, h.journalCommands(id), "streams census done")
	require.True(t, h.journal(id).Complete, "the census did not complete the record")
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the census is done")
	require.Zero(t, code, "release after the census")
	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase)
}
