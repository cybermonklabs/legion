package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// prepare, under the prepare row of its hold, saves the cohort: B = the stream's first retained
// sequence, and each fixed name it lists with its ack floor, and no fresh or other name; it raises
// each fixed name's shorter inactive threshold to 7 days, so a fixed name a creator stops pulling
// survives past its old threshold, and leaves a fixed name with no threshold as it is. A rerun keeps
// the lower B, each member's lower floor, and a member it no longer lists. Controls: prepare is
// refused while the row is released and while it is held, writing nothing; and a consumer of no
// family, with the same short threshold, is removed for inactivity in the same time, since prepare
// does not raise it. It fails when B is not the first retained sequence, a floor is not the one the
// server answered, a fixed name's threshold is left short, or a rerun raises a bound.
func TestPrepareSavesTheBoundaryAndEachFixedNamesFloor(t *testing.T) {
	const threshold = 15 * time.Second
	rig := newConvertRigWith(t, func(config *jetstream.StreamConfig) { config.MaxMsgs, config.Discard = 8, jetstream.DiscardOld })
	ctx := context.Background()
	rig.publish("notifications.agent.a", 6)
	rig.publish("notifications.github.x", 6)
	rig.fixed("s-a", "notifications.agent.a", threshold)
	rig.fixed("legion-go-p1-github", "notifications.github.x", 0)
	rig.fixed("other", "notifications.github.x", threshold)
	_, err := rig.writer.CreateConsumer(ctx, notifications, jetstream.ConsumerConfig{Name: "session~fresh", Durable: "session~fresh",
		FilterSubject: "notifications.agent.a", AckPolicy: jetstream.AckExplicitPolicy})
	require.NoError(t, err)
	consumer, err := rig.writer.Consumer(ctx, notifications, "s-a")
	require.NoError(t, err)
	require.NoError(t, rig.ackUpTo(consumer, 5))

	code, _, stderr := rig.run(actionPrepare, "--hold", "no-such-hold")
	require.Equal(t, 1, code, "control: prepare in a released row")
	require.Contains(t, stderr, "released")

	id := rig.prepared(holdrecord.WorkflowCutover)
	daemonFloor := convertFloor(rig.consumer("legion-go-p1-github"))
	code, stdout, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(t, code, stderr)
	require.Contains(t, stdout, "boundary 5")
	cohort, _, found, err := readPrepareCohort(ctx, rig.records(), id)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 5, cohort.Boundary, "B is not the stream's first retained sequence")
	require.Equal(t, []prepareCohortMember{
		{Family: daemonFamily("p1"), Logical: "legion-go-p1-github", Fixed: "legion-go-p1-github", Floor: daemonFloor},
		{Family: familySession, Logical: "a", Fixed: "s-a", Floor: 5, Threshold: threshold},
	}, cohort.Members)
	require.Equal(t, prepareThreshold, rig.consumer("s-a").Config.InactiveThreshold, "s-a's threshold was not raised")
	require.Zero(t, rig.consumer("legion-go-p1-github").Config.InactiveThreshold, "a fixed name with no threshold was given one")

	// The rerun: the daemon's consumer is gone, s-a's floor has risen and the stream has moved on.
	require.NoError(t, rig.writer.DeleteConsumer(ctx, notifications, "legion-go-p1-github"))
	rig.publish("notifications.agent.a", 2)
	require.NoError(t, rig.ackUpTo(consumer, 13))
	code, _, stderr = rig.run(actionPrepare, "--hold", id)
	require.Zero(t, code, stderr)
	rerun, _, _, err := readPrepareCohort(ctx, rig.records(), id)
	require.NoError(t, err)
	require.Equal(t, cohort, rerun, "the rerun raised a bound or dropped a member")

	time.Sleep(threshold + 5*time.Second)
	require.Nil(t, rig.consumer("other"), "control: the consumer of no family with the same threshold was not removed")
	require.NotNil(t, rig.consumer("s-a"), "the fixed name prepare raised was removed for inactivity")

	rig.held(id)
	code, _, stderr = rig.run(actionPrepare, "--hold", id)
	require.Equal(t, 1, code, "control: prepare in a held row")
	require.Contains(t, stderr, "held")
}

// The boundary rows' session K: its own agent subject and a shared topic its filter holds.
const (
	boundarySession = "k"
	boundaryAgent   = "notifications.agent.k"
	boundaryShared  = "notifications.github.k"
)

// boundaryWindow runs the cutover hold from prepare to release: hold prepare's row and journal,
// consumer-names prepare, then afterPrepare (what happens once prepare has listed the stream, while
// clients still pull), the move to held (set, when given, runs hold set's own steps; else its
// journal writes alone), beforeConvert, create-bucket, convert, the census's journal entries, and
// the release; then the writer connects again. It answers the hold id and the cohort.
func (rig *convertRig) boundaryWindow(afterPrepare, set func(id string), beforeConvert func()) (string, prepareCohort) {
	rig.t.Helper()
	id := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(rig.t, code, stderr)
	if afterPrepare != nil {
		afterPrepare(id)
	}
	rig.closeWriter()
	if set != nil {
		set(id)
	} else {
		rig.held(id)
	}
	if beforeConvert != nil {
		beforeConvert()
	}
	code, _, stderr = rig.run(actionCreateBucket, "--hold", id)
	require.Zero(rig.t, code, stderr)
	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Zero(rig.t, code, stderr)
	rig.censusDone(id)
	require.NoError(rig.t, rig.release(id))
	rig.connectWriter()
	cohort, _, found, err := readPrepareCohort(context.Background(), rig.records(), id)
	require.NoError(rig.t, err)
	require.True(rig.t, found)
	return id, cohort
}

// boundaryFirstStart is K's first-create rule, today's (internal/bus/session_consumers.go,
// SessionConsumers.create's default branch, firstOwnMessage): its agent subject's first stored
// message, else the stream's end.
func (rig *convertRig) boundaryFirstStart() uint64 {
	rig.t.Helper()
	ctx := context.Background()
	raw, err := apiRequest(ctx, rig.writerConn, "$JS.API.STREAM.MSG.GET."+notifications, map[string]any{"seq": 1, "next_by_subj": boundaryAgent})
	var api *jetstream.APIError
	if errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeMessageNotFound {
		stream, err := streamInfo(ctx, rig.writerConn, notifications)
		require.NoError(rig.t, err)
		return stream.State.LastSeq + 1
	}
	require.NoError(rig.t, err)
	var answer struct {
		Message struct {
			Seq uint64 `json:"seq"`
		} `json:"message"`
	}
	require.NoError(rig.t, json.Unmarshal(raw, &answer))
	return answer.Message.Seq
}

// boundaryRegister registers K in the fresh mode after the window, as the listener does: through
// selection.Names over the writer's connection and the selection bucket, clocked by now, with K's
// first-create rule (boundaryFirstStart), so a key with no record starts at the lower of that rule
// and the sentinel's B. Its consumer holds K's two subjects. It answers the consumer's name and the
// start it was created at.
func (rig *convertRig) boundaryRegister(now func() time.Time) (string, uint64) {
	rig.t.Helper()
	ctx := context.Background()
	names := selection.New(selection.Config{
		Store:    selection.NewKVStore(selection.ConnRequester{Conn: rig.writerConn}),
		Server:   convertTestServer{rig},
		Prefix:   func(string) string { return sessionFreshPrefix },
		Fixed:    selection.SessionNaming().Fixed,
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "", nil },
		Owner:    "the boundary row's listener",
		Now:      now,
	})
	first := rig.boundaryFirstStart()
	sel, _, err := names.Ensure(ctx, boundarySession, selection.CreateRequest{
		Create: func(ctx context.Context, name string, start selection.Start) (*jetstream.ConsumerInfo, error) {
			return writeConsumer(ctx, rig.writerConn, notifications, boundaryConsumer(name, start.Seq), actionCreateConsumer)
		},
		First: func(context.Context) (selection.Start, error) { return selection.Start{Seq: first}, nil },
	})
	require.NoError(rig.t, err, "K's registration")
	return sel.Record.Current, sel.Record.Start
}

// boundaryConsumer is K's consumer name at start, on its two subjects.
func boundaryConsumer(name string, start uint64) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{Name: name, Durable: name, FilterSubjects: []string{boundaryAgent, boundaryShared},
		AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: -1, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: start,
		Metadata: member{Family: familySession, Logical: boundarySession, Fixed: "s-" + boundarySession, Prefix: sessionFreshPrefix, SessionID: boundarySession}.Metadata()}
}

// boundaryDelivers is what a consumer of K's subjects created at start, as another start rule
// would create it, delivers: the rows' controls.
func (rig *convertRig) boundaryDelivers(start uint64) []uint64 {
	rig.t.Helper()
	name := selection.FreshName("control")
	_, err := writeConsumer(context.Background(), rig.writerConn, notifications, boundaryConsumer(name, start), actionCreateConsumer)
	require.NoError(rig.t, err)
	return rig.consume(name, 5, 0)
}

// boundaryGone waits up to a minute for consumer name to be removed, on a connection of its own,
// and reports whether it was: it runs on whatever goroutine asks.
func (rig *convertRig) boundaryGone(name string) bool {
	nc, err := nats.Connect(rig.s.Cluster.URL(), append(rig.s.OperatorCredential(), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))...)
	if err != nil {
		return false
	}
	defer nc.Close()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if info, err := consumerInfo(context.Background(), nc, notifications, name); err == nil && info == nil {
			return true
		}
	}
	return false
}

// The cutover boundary, expiry before the listing (red team R4, F4): after prepare listed the
// stream, an old listener creates F for K with a short threshold and a shared-topic message pends on
// it; hold set runs, and one eviction is held back by the hub's disruption budget past F's
// threshold, so F expires before convert lists, and K is in no cohort and gets no record. After the
// release K registers with only that pending shared-topic message, no resume record and no message
// on its agent subject: it starts at B, and the message arrives. Control: the first-create rule
// alone starts at the stream's end, past the message, and the message is lost. It fails when K
// starts past B, or F's expiry is not inside the hold.
func TestTheCutoverBoundaryExpiryBeforeTheListing(t *testing.T) { boundaryExpiryRow(t) }

func boundaryExpiryRow(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	fullCapSeedStreams(t, h, 0)
	rig := &convertRig{stack: h}
	rig.connectWriter()
	rig.publish("notifications.dispatch.other", 3)
	var message uint64
	_, cohort := rig.boundaryWindow(func(string) {
		config := boundaryConsumer("s-"+boundarySession, 0)
		config.Name, config.Metadata = "", nil
		config.DeliverPolicy, config.OptStartSeq, config.InactiveThreshold = jetstream.DeliverAllPolicy, 0, 3*time.Second
		_, err := writeConsumer(ctx, rig.writerConn, notifications, config, actionCreateConsumer)
		require.NoError(t, err, "the old listener's F")
		message = rig.publish(boundaryShared, 1)[0]
	}, func(id string) {
		rig.s.Kube.SpendBudget(true)
		expired := make(chan bool, 1)
		go func() {
			gone := rig.boundaryGone("s-" + boundarySession)
			rig.s.Kube.SpendBudget(false)
			expired <- gone
		}()
		_, err := writerhold.Set(ctx, rig.records(), rig.op, id, "the boundary row's hold", nil)
		require.NoError(t, err, "hold set")
		require.True(t, <-expired, "F did not expire during the hold")
		require.Positive(t, rig.s.Kube.Refused(), "no eviction was held back by the budget")
	}, func() {
		require.Nil(t, rig.consumer("s-"+boundarySession), "F is still there when convert lists")
	})
	require.Nil(t, cohort.member("s-"+boundarySession), "F is in the cohort")
	require.Greater(t, rig.boundaryFirstStart(), message, "control: the first-create rule")
	require.Empty(t, rig.boundaryDelivers(rig.boundaryFirstStart()), "control: the first-create rule alone delivered the message")
	name, start := rig.boundaryRegister(time.Now)
	require.Equal(t, cohort.Boundary, start, "K did not start at B")
	require.Equal(t, []uint64{message}, rig.consume(name, 5, 0), "the pending shared-topic message")
}

// The cutover boundary, the red team's pre-B schedule: F holds an unhandled shared-topic message
// at 51 with the stream at 100; an old restart deletes F with start 51 saved in its memory; prepare
// lists (F absent, so in no cohort); the old process creates F at 51, and F expires before convert.
// K starts at B, the stream's first retained sequence, and message 51 arrives. Control: B =
// last_seq + 1, round 17's rule, starts at 101, and message 51 is lost. It fails when B is above the
// first retained sequence.
func TestTheCutoverBoundaryPreBoundaryStart(t *testing.T) { boundaryPreBoundaryRow(t) }

func boundaryPreBoundaryRow(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	rig.publish("notifications.dispatch.other", 50)
	message := rig.publish(boundaryShared, 1)[0]
	rig.publish("notifications.dispatch.other", 49)
	require.EqualValues(t, 51, message)
	fixed := boundaryConsumer("s-"+boundarySession, 0)
	fixed.Name, fixed.Metadata, fixed.DeliverPolicy, fixed.AckWait = "", nil, jetstream.DeliverAllPolicy, time.Hour
	_, err := writeConsumer(ctx, rig.writerConn, notifications, fixed, actionCreateConsumer)
	require.NoError(t, err)
	require.Equal(t, []uint64{51}, rig.consume("s-"+boundarySession, 1, 0), "F holds 51 unhandled")
	require.NoError(t, deleteConsumer(ctx, rig.writerConn, notifications, "s-"+boundarySession), "the old restart's delete")
	_, cohort := rig.boundaryWindow(func(string) {
		again := fixed
		again.DeliverPolicy, again.OptStartSeq, again.InactiveThreshold = jetstream.DeliverByStartSequencePolicy, 51, 3*time.Second
		_, err := writeConsumer(ctx, rig.writerConn, notifications, again, actionCreateConsumer)
		require.NoError(t, err, "the old process's F at 51")
		require.True(t, rig.boundaryGone("s-"+boundarySession), "F did not expire")
	}, nil, nil)
	require.Nil(t, cohort.member("s-"+boundarySession))
	require.EqualValues(t, 1, cohort.Boundary, "B is not the first retained sequence")
	require.Empty(t, rig.boundaryDelivers(min(rig.boundaryFirstStart(), 101)), "control: round 17's B of last_seq + 1 delivered 51")
	name, start := rig.boundaryRegister(time.Now)
	require.EqualValues(t, 1, start)
	require.Equal(t, []uint64{51}, rig.consume(name, 5, 0), "message 51")
}

// The cutover boundary has no end (review H2): hold release records nothing for it, so the
// sentinel holds the cutover marker and B alone. The stream then drops every message through the
// release's last sequence (here by its max_msgs of 1, as a purge through it would), and shared-topic
// message 101, published after the release, stays retained. K registers with no record, starts at
// B, which the server raises to the first retained sequence, and message 101 arrives. Control:
// round 17's end, first_seq above E (the release's last sequence), ends the boundary after the
// drop, and the first-create rule then starts past message 101. It fails when the release or the
// stream's retention ends the boundary.
func TestTheCutoverBoundaryHasNoEnd(t *testing.T) { boundaryNoEndRow(t) }

func boundaryNoEndRow(t *testing.T) {
	rig := newConvertRigWith(t, func(config *jetstream.StreamConfig) { config.MaxMsgs, config.Discard = 1, jetstream.DiscardOld })
	ctx := context.Background()
	rig.publish("notifications.dispatch.other", 100)
	id, cohort := rig.boundaryWindow(nil, nil, nil)
	require.EqualValues(t, 100, cohort.Boundary)
	nc, _ := rig.operator()
	sentinel, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).State(ctx)
	require.NoError(t, err)
	nc.Close()
	require.Equal(t, ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 100, Hold: id}), sentinel,
		"the release recorded something for the boundary: the sentinel holds more than create-bucket's B and hold and convert's marker")
	message := rig.publish(boundaryShared, 1)[0]
	require.EqualValues(t, 101, message)
	stream, err := streamInfo(ctx, rig.writerConn, notifications)
	require.NoError(t, err)
	require.Greater(t, stream.State.FirstSeq, uint64(100), "control: first_seq is above the release's last sequence, where round 17's end ends the boundary")
	require.Empty(t, rig.boundaryDelivers(rig.boundaryFirstStart()), "control: the first-create rule after round 17's end delivered message 101")
	name, start := rig.boundaryRegister(time.Now)
	require.EqualValues(t, 100, start)
	require.Equal(t, []uint64{101}, rig.consume(name, 5, 0), "message 101")
}

// The cutover boundary long after the release: with the stream's retention past B, and the
// listener's clock 2 hours ahead of the servers', a new key still starts at B, which the server
// raises to the first retained sequence, and receives the first retained message on its subjects.
// Control: an end by time, the boundary ended an hour after the release as the listener's clock
// reads it, gives the first-create rule, which starts past that message. It fails when any clock
// or retention ends the boundary.
func TestTheCutoverBoundaryLongAfterTheRelease(t *testing.T) { boundaryLongAfterRow(t) }

func boundaryLongAfterRow(t *testing.T) {
	rig := newConvertRigWith(t, func(config *jetstream.StreamConfig) { config.MaxMsgs, config.Discard = 4, jetstream.DiscardOld })
	rig.publish("notifications.dispatch.other", 10)
	_, cohort := rig.boundaryWindow(nil, nil, nil)
	require.EqualValues(t, 7, cohort.Boundary)
	shared := rig.publish(boundaryShared, 3)
	rig.publish("notifications.dispatch.other", 3)
	retained := shared[2]
	ahead := func() time.Time { return time.Now().Add(2 * time.Hour) }
	require.True(t, ahead().Sub(time.Now()) > time.Hour, "the row's premise: the listener's clock reads two hours past the servers', past an hour-long end")
	require.Empty(t, rig.boundaryDelivers(rig.boundaryFirstStart()), "control: the first-create rule after an end by time delivered the retained message")
	name, start := rig.boundaryRegister(ahead)
	require.Equal(t, cohort.Boundary, start)
	require.Equal(t, []uint64{retained}, rig.consume(name, 5, 0), "the first retained message on K's subjects")
}

// The cutover boundary on an empty stream (red team G4): prepare on an empty ENVOY_NOTIFICATIONS
// saves B = 1, which create-bucket writes into the sentinel. A shared-topic message is published
// before K registers, and K starts at 1 and receives it. Control: a B of 0 makes the consumer create
// fail, since DeliverByStartSequence refuses a start of 0. It fails when B is 0.
func TestTheCutoverBoundaryOnAnEmptyStream(t *testing.T) { boundaryEmptyStreamRow(t) }

func boundaryEmptyStreamRow(t *testing.T) {
	rig := newConvertRig(t, 0)
	_, cohort := rig.boundaryWindow(nil, nil, nil)
	require.EqualValues(t, 1, cohort.Boundary)
	message := rig.publish(boundaryShared, 1)[0]
	_, err := writeConsumer(context.Background(), rig.writerConn, notifications, boundaryConsumer(selection.FreshName("control"), 0), actionCreateConsumer)
	require.Error(t, err, "control: a consumer by start sequence at a B of 0 was created")
	name, start := rig.boundaryRegister(time.Now)
	require.EqualValues(t, 1, start)
	require.Equal(t, []uint64{message}, rig.consume(name, 5, 0))
}
