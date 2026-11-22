//go:build twobuild

package consumernames

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// mixedBuildSweepSessions is how many sessions the old sweep's row converts: more than the old
// pass's cap (mixedBuildSweepCap), so the control shows the cap and not the whole set.
const mixedBuildSweepSessions = 300

// mixedBuildSweepCap is how many consumers one pass of the previous release's sweep deletes
// (cmd/listener/session_consumer_sweep.go, sessionConsumerSweepCap, at legion-envoy-v32.31.1).
const mixedBuildSweepCap = 256

// mixedBuildSweepLine is the line each pass of the old sweep that listed a consumer logs, with how
// many it listed, judged gone and deleted (session_consumer_sweep.go, pass).
const mixedBuildSweepLine = "session consumer sweep pass"

// The listener entries of the stack (holdtest.ListenerMachines) the row's programs connect as: the
// previous release's listener, and this tree's session consumer calls.
var (
	mixedBuildOldMachine = holdtest.ListenerMachines[0]
	mixedBuildNewMachine = holdtest.ListenerMachines[1]
)

// mixedBuildSession is one session of the row: its id and agent subject, every message published
// on that subject in order, its fixed consumer's ack floor before the window, and, after it, the
// name its record selects, the start the record saved and that consumer's ack floor.
type mixedBuildSession struct {
	id, subject string
	seqs        []uint64
	floor       uint64
	current     string
	start       uint64
	ackFloor    uint64
}

// An old listener's sweep after the window (§3, "An old listener's sweep after the window"). After
// convert and release, the previous release's listener starts with its sweep interval at 24 hours,
// so only its startup pass runs, with its durable bound and its copy of the session bucket ready,
// so the pass is trusted. It lists every consumer named s-<rest> and deletes each whose session
// <rest> its copy of envoy_sessions reads as gone, up to 256 a pass. A fresh session consumer is
// named session~<nuid>, which that listing does not name: all 300 the records select are left at
// their floors, and this tree's relay calls deliver every message from each saved start. The one
// fixed s-<id> an old build made after the window, whose session has no record in envoy_sessions,
// is deleted, which shows the pass ran and was trusted. Control: round 17's name, s-~<nuid>, is
// listed as session ~<nuid>, which reads as gone, and the same pass deletes 256 consumers that
// records select, as the window rehearsal saw.
func TestAnOldListenerSweepAfterTheWindow(t *testing.T) {
	previous := mixedBuildPreviousRelease(t)

	t.Run("a fresh name session~<nuid> is outside the old sweep", func(t *testing.T) {
		h := newStack(t)
		h.mixedBuildProvision()
		sessions := h.mixedBuildFixedSessions(mixedBuildSweepSessions)
		h.mixedBuildCutoverWithTheCommands()
		orphan := h.mixedBuildAfterTheWindow(sessions, sessionFreshPrefix+"~")
		pass := h.mixedBuildOldSweepPass(previous)

		require.Equal(t, 1, pass["listed"], "the old pass listed %v; want only the fixed %s: no session~<nuid> is named s-<rest>", pass, orphan)
		require.Equal(t, 1, pass["deleted"], "the old pass deleted %v; want the fixed %s, which shows it ran trusted", pass, orphan)
		nc, _ := h.mixedBuildListenerConn(mixedBuildNewMachine)
		ctx := context.Background()
		info, err := consumerInfo(ctx, nc, notifications, orphan)
		require.NoError(t, err)
		require.Nil(t, info, "the fixed %s, whose session has no record, is still on the stream", orphan)
		for _, s := range sessions {
			info, err := consumerInfo(ctx, nc, notifications, s.current)
			require.NoError(t, err)
			require.NotNil(t, info, "the old pass deleted %s, which the record of %s selects", s.current, s.id)
			require.Equal(t, s.ackFloor, info.AckFloor.Stream, "%s left its floor", s.current)
		}

		sc := h.mixedBuildSessionConsumers(mixedBuildNewMachine, selection.ModeFresh)
		for _, s := range sessions {
			started, err := sc.Ensure(ctx, s.id, []string{s.subject}, 0)
			require.NoError(t, err, "this tree's relay calls for %s", s.id)
			require.Equal(t, s.current, started.Name, "this tree selected another consumer for %s than its record's", s.id)
			require.False(t, started.Created, "this tree created a consumer for %s, whose record's consumer the pass kept", s.id)
			want := mixedBuildFrom(s.seqs, s.start)
			require.Equal(t, want, mixedBuildPullSeqs(t, sc, s.id, started.Name, len(want), false),
				"the messages of %s from its saved start %d", s.id, s.start)
		}
	})

	t.Run("control: round 17's name s-~<nuid> is swept", func(t *testing.T) {
		h := newStack(t)
		h.mixedBuildProvision()
		sessions := h.mixedBuildFixedSessions(mixedBuildSweepSessions)
		h.mixedBuildCutoverByRound17(sessions)
		orphan := h.mixedBuildAfterTheWindow(sessions, sessionFixedPrefix+"~")
		pass := h.mixedBuildOldSweepPass(previous)

		require.Equal(t, mixedBuildSweepSessions+1, pass["listed"], "the old pass listed %v; want every s-~<nuid> and %s", pass, orphan)
		require.Equal(t, mixedBuildSweepCap, pass["deleted"], "the old pass deleted %v; want its cap", pass)
		ctx := context.Background()
		nc, _ := h.mixedBuildListenerConn(mixedBuildNewMachine)
		info, err := consumerInfo(ctx, nc, notifications, orphan)
		require.NoError(t, err)
		orphanDeleted := 0
		if info == nil {
			orphanDeleted = 1
		}
		records := selection.NewKVStore(selection.ConnRequester{Conn: h.mixedBuildOperatorConn()})
		lost := 0
		for _, s := range sessions {
			info, err := consumerInfo(ctx, nc, notifications, s.current)
			require.NoError(t, err)
			if info != nil {
				continue
			}
			lost++
			record, _, err := records.Read(ctx, s.id)
			require.NoError(t, err)
			require.Equal(t, s.current, record.Current, "the record of %s no longer selects the consumer the pass deleted", s.id)
		}
		require.Equal(t, mixedBuildSweepCap, lost+orphanDeleted,
			"the pass deleted %d consumers that records select and %d fixed; want %d in all", lost, orphanDeleted, mixedBuildSweepCap)
	})
}

// mixedBuildFrom are the sequences of seqs from start on.
func mixedBuildFrom(seqs []uint64, start uint64) []uint64 {
	var from []uint64
	for _, seq := range seqs {
		if seq >= start {
			from = append(from, seq)
		}
	}
	return from
}

// mixedBuildFixedSessions makes n sessions' fixed consumers, s-<id>, as every listener before the
// fresh names made them: through this tree's session consumer calls in the fixed mode, which are
// today's implementation unchanged (bus.SessionConsumers.Ensure), as the listener entry
// mixedBuildNewMachine. It publishes two messages on each session's agent subject and acks the
// first, so each fixed consumer holds an ack floor and an unacked message past it, which the
// conversion's start must keep. It closes its connections before it returns, as every creator
// stops before the hold.
func (h *stack) mixedBuildFixedSessions(n int) []*mixedBuildSession {
	h.t.Helper()
	ctx := context.Background()
	sc := h.mixedBuildSessionConsumers(mixedBuildNewMachine, selection.ModeFixed)
	nc, js := h.mixedBuildListenerConn(mixedBuildNewMachine)
	sessions := make([]*mixedBuildSession, 0, n)
	for i := range n {
		s := &mixedBuildSession{id: fmt.Sprintf("mixed-%03d", i)}
		s.subject = contracts.LegacyAgentSubject(s.id)
		started, err := sc.Ensure(ctx, s.id, []string{s.subject}, 0)
		require.NoError(h.t, err, "the fixed consumer of %s", s.id)
		require.Equal(h.t, sessionFixedPrefix+s.id, started.Name)
		for m := range 2 {
			ack, err := js.Publish(ctx, s.subject, fmt.Appendf(nil, "%s before the window, %d", s.id, m))
			require.NoError(h.t, err)
			s.seqs = append(s.seqs, ack.Sequence)
		}
		require.Equal(h.t, s.seqs[0], mixedBuildFirstPull(h.t, sc, s.id, started.Name), "the first message of %s", s.id)
		s.floor = s.seqs[0]
		sessions = append(sessions, s)
	}
	for _, s := range sessions {
		require.Eventually(h.t, func() bool {
			info, err := consumerInfo(ctx, nc, notifications, sessionFixedPrefix+s.id)
			return err == nil && info != nil && info.AckFloor.Stream == s.floor
		}, 30*time.Second, 50*time.Millisecond, "the ack floor of s-%s never reached %d", s.id, s.floor)
	}
	sc.Close()
	nc.Close()
	return sessions
}

// mixedBuildFirstPull is the setup's first pull of a fixed consumer it has just created, with this
// tree's pull (bus.SessionConsumers.Pull), acking the one message it takes, and answers its stream
// sequence. A pull that meets bus.ErrSessionConsumerNotFound is sent again until 30 seconds pass:
// the bus answers a pull that met no responder so, and on the three-server hub a consumer whose
// leader is named is not yet a responder on every server (seen twice, once in 300 sessions each,
// the second time after a wait for the consumer's leader). Any other answer fails the setup.
func mixedBuildFirstPull(t *testing.T, sc *bus.SessionConsumers, sessionID, name string) uint64 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		seq, err := func() (uint64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pull, err := sc.Pull(ctx, sessionID, name)
			if err != nil {
				return 0, err
			}
			defer pull.Stop()
			msg, err := pull.Next(ctx)
			if err != nil {
				return 0, err
			}
			return msg.StreamSeq, msg.Ack()
		}()
		if err == nil {
			return seq
		}
		if !errors.Is(err, bus.ErrSessionConsumerNotFound) || time.Now().After(deadline) {
			require.NoError(t, err, "the first pull of %s", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mixedBuildCutoverWithTheCommands runs the cutover window through the operator's commands (§3):
// `consumer-names prepare` in phase prepare, the hold set, `create-bucket` and `convert` under it,
// `streams census` until a run is clean, and the release, which the journal must allow.
func (h *stack) mixedBuildCutoverWithTheCommands() {
	h.t.Helper()
	id := h.prepared(holdrecord.WorkflowCutover)
	h.mixedBuildRun(0, actionPrepare, "--hold", id)
	h.held(id)
	h.mixedBuildRun(0, actionCreateBucket, "--hold", id)
	h.mixedBuildRun(0, actionConvert, "--hold", id)
	h.mixedBuildCensus(id, "--family", familySession)
	require.NoError(h.t, h.release(id), "release the cutover hold after convert and a clean census")
}

// mixedBuildCensus runs `envoy-infra streams census` (censusCommand: writerhold's streams census
// word with this package's readers) until a run is clean: the word writes its intent at each run,
// its stop entry after a run that removed something (exit 1), which is run again, and its done
// entry after the clean run, which completes the cutover's record. An UNKNOWN (exit 2) fails the
// test.
func (h *stack) mixedBuildCensus(id string, families ...string) {
	h.t.Helper()
	for run := 1; ; run++ {
		code, stdout, stderr := h.censusCommand(id, families...)
		if code == 0 {
			require.True(h.t, h.journal(id).Complete, "the clean census's done entry did not complete the record")
			return
		}
		if code != 1 || run == 5 {
			h.t.Fatalf("streams census run %d exited %d:\n%s%s", run, code, stdout, stderr)
		}
	}
}

// mixedBuildCutoverByRound17 is the control's window: round 17's conversion, which named each
// session's new consumer s-~<nuid> (selection.FreshName of the fixed prefix). Round 17's convert
// is in no build, so the row writes what it wrote, as the operator in the held hold: the bucket
// with its marker done, then for each session a consumer of the fixed one's configuration at its
// ack floor + 1, the record selecting it, and the fixed one's delete; the family's broker cutover
// row; and each command's journal entries, so the release is allowed as it was after round 17's.
// It runs no census and writes the cutover workflow's final command's entries
// (censusCommandName) itself, as `hold set`'s are written (harness_test.go, held); the window
// through the commands runs the word, which writes its own (mixedBuildCensus).
func (h *stack) mixedBuildCutoverByRound17(sessions []*mixedBuildSession) {
	h.t.Helper()
	ctx := context.Background()
	id := h.prepared(holdrecord.WorkflowCutover)
	h.held(id)
	nc, _ := h.operator()
	defer nc.Close()
	stream, err := streamInfo(ctx, nc, notifications)
	require.NoError(h.t, err)

	_, err = h.records().Admit(ctx, id, holdrecord.Request{Command: actionCreateBucket}, nil)
	require.NoError(h.t, err)
	store, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{
		Cutover: selection.CutoverDone, Boundary: max(1, stream.State.FirstSeq), Hold: id,
	})
	require.NoError(h.t, err)
	require.NoError(h.t, h.records().AppendDone(ctx, id, actionCreateBucket, nil))

	_, err = h.records().Admit(ctx, id, holdrecord.Request{Command: actionConvert}, nil)
	require.NoError(h.t, err)
	for _, s := range sessions {
		fixed := sessionFixedPrefix + s.id
		f, err := consumerInfo(ctx, nc, notifications, fixed)
		require.NoError(h.t, err)
		require.NotNil(h.t, f, "%s is gone before round 17's conversion", fixed)
		start := f.AckFloor.Stream + 1
		config := f.Config
		config.Name = selection.FreshName(sessionFixedPrefix)
		config.Durable = config.Name
		config.DeliverPolicy, config.OptStartSeq = jetstream.DeliverByStartSequencePolicy, start
		config.Metadata = member{Family: familySession, Logical: s.id, Fixed: fixed, Prefix: sessionFixedPrefix, SessionID: s.id}.Metadata()
		_, err = writeConsumer(ctx, nc, notifications, config, actionCreateConsumer)
		require.NoError(h.t, err, "create round 17's %s for %s", config.Name, s.id)
		_, err = store.Write(ctx, s.id, selection.Record{Current: config.Name, Start: start}, 0)
		require.NoError(h.t, err)
		require.NoError(h.t, deleteConsumer(ctx, nc, notifications, fixed))
	}
	_, err = h.records().PutCutover(ctx, familySession, selection.CutoverDone, id)
	require.NoError(h.t, err)
	require.NoError(h.t, h.records().AppendDone(ctx, id, actionConvert, nil))

	_, err = h.records().Admit(ctx, id, holdrecord.Request{Command: censusCommandName}, nil)
	require.NoError(h.t, err)
	require.NoError(h.t, h.records().AppendDone(ctx, id, censusCommandName, nil))
	require.NoError(h.t, h.release(id), "release round 17's cutover hold")
}

// mixedBuildOperatorConn is the operator's connection under the row's phase now, closed when the
// test ends.
func (h *stack) mixedBuildOperatorConn() *nats.Conn {
	h.t.Helper()
	nc, _ := h.operator()
	return nc
}

// mixedBuildAfterTheWindow reads, after the release, each session's record (the name it selects,
// which must begin with prefix, and its saved start, which must be the fixed consumer's floor + 1)
// and that consumer's ack floor; then, as an old build would after the window, makes the fixed
// consumer s-<id> of a session no record in envoy_sessions names, and publishes one more message
// on each session's subject. It answers that fixed consumer's name.
func (h *stack) mixedBuildAfterTheWindow(sessions []*mixedBuildSession, prefix string) string {
	h.t.Helper()
	ctx := context.Background()
	records := selection.NewKVStore(selection.ConnRequester{Conn: h.mixedBuildOperatorConn()})
	nc, js := h.mixedBuildListenerConn(mixedBuildNewMachine)
	for _, s := range sessions {
		record, _, err := records.Read(ctx, s.id)
		require.NoError(h.t, err, "the record of %s", s.id)
		require.True(h.t, strings.HasPrefix(record.Current, prefix), "the record of %s selects %q; want a name beginning %s", s.id, record.Current, prefix)
		require.Equal(h.t, s.floor+1, record.Start, "the saved start of %s", s.id)
		info, err := consumerInfo(ctx, nc, notifications, record.Current)
		require.NoError(h.t, err)
		require.NotNil(h.t, info, "the record of %s selects %s, which the stream does not hold", s.id, record.Current)
		s.current, s.start, s.ackFloor = record.Current, record.Start, info.AckFloor.Stream
	}
	orphanID := "mixed-unrecorded"
	orphan := sessionFixedPrefix + orphanID
	_, err := js.CreateConsumer(ctx, notifications, jetstream.ConsumerConfig{
		Durable: orphan, FilterSubject: contracts.LegacyAgentSubject(orphanID), AckPolicy: jetstream.AckExplicitPolicy,
	})
	require.NoError(h.t, err, "an old build's fixed %s after the window", orphan)
	for _, s := range sessions {
		ack, err := js.Publish(ctx, s.subject, fmt.Appendf(nil, "%s after the window", s.id))
		require.NoError(h.t, err)
		s.seqs = append(s.seqs, ack.Sequence)
	}
	nc.Close()
	return orphan
}

// mixedBuildOldSweepPass starts the previous release's listener as mixedBuildOldMachine with its
// sweep interval at 24 hours, so the pass it runs once its durable binds is the only one, reads
// that pass's line, stops the listener, and answers the line's counts: listed, gone and deleted.
// A pass that is not trusted logs nothing (session_consumer_sweep.go, trusted), so the line is
// itself the proof that the binding was held and the copy ready.
func (h *stack) mixedBuildOldSweepPass(previous mixedBuildRelease) map[string]int {
	h.t.Helper()
	old := h.mixedBuildStartListener(previous.listener(), mixedBuildOldMachine, "ENVOY_SESSION_CONSUMER_SWEEP_INTERVAL=24h")
	line := old.waitLine(h.t, mixedBuildSweepLine, 3*time.Minute)
	old.stop(h.t)
	h.t.Logf("the previous release's (%s) startup pass: %v", previous.commit, line)
	counts := map[string]int{}
	for _, field := range []string{"listed", "gone", "deleted"} {
		value, ok := line[field].(float64)
		require.True(h.t, ok, "the pass line has no %s: %v", field, line)
		counts[field] = int(value)
	}
	return counts
}
