package writerhold

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// setHistory sets bucket's stream's max_msgs_per_subject to history as the operator, under a
// hold of no steps, which it then releases: a bucket that differs from the build's configuration
// in a setting `streams update` does not raise.
func (h *heldStack) setHistory(bucket string, history int64) {
	h.t.Helper()
	ctx := context.Background()
	id := h.s.Prepare(h.t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(h.t, id)
	nc, err := h.s.ConnectOperator(ctx)
	require.NoError(h.t, err)
	js, err := jetstream.New(nc)
	require.NoError(h.t, err)
	s, err := js.Stream(ctx, kvStreamPrefix+bucket)
	require.NoError(h.t, err)
	config := s.CachedInfo().Config
	config.MaxMsgsPerSubject = history
	_, err = js.UpdateStream(ctx, config)
	require.NoError(h.t, err, "set %s's history to %d", bucket, history)
	nc.Close()
	h.s.Release(h.t, id)
	h.awaitStreamLeaders()
}

// lastEntry is hold id's journal's last entry.
func (h *heldStack) lastEntry(id string) holdrecord.Operation {
	h.t.Helper()
	ops := h.journal(id).Ops
	require.NotEmpty(h.t, ops)
	return ops[len(ops)-1]
}

// `streams update` whose own check refuses after its intent writes a stop entry through RunHeld.
// KV_envoy_roles' history differs from the build's, which update does not raise ("update stops
// here"), so the update refuses after its intent, with nothing changed: its stop records
// `unchanged`, and `hold release` then ends the hold. The control is an update killed after its
// intent, with no stop: release is refused while that intent is pending.
func TestHoldTheStreamsUpdateStopsThroughRunHeld(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	h.setHistory(bus.RoleClaimBucket, 5)

	code, stdout, _ := h.streamsRun(nil, "update")
	require.Equal(t, 1, code, "an update that meets a difference it does not raise")
	require.Contains(t, stdout, "update stops here")
	id := h.row().HoldID
	last := h.lastEntry(id)
	require.Equal(t, CommandStreamsUpdate+" stop", last.Command+" "+last.State)
	require.Equal(t, holdrecord.LeftUnchanged, last.Outputs["left"])
	code, _, _ = h.run(actionRelease, "--hold", id, "--reason", "the update stopped unchanged")
	require.Zero(t, code, "release after an update that stopped unchanged")

	// The control: an update killed after its intent.
	pending := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, pending)
	_, err := h.s.Records.Admit(ctx, pending, holdrecord.Request{Command: CommandStreamsUpdate}, nil)
	require.NoError(t, err)
	code, _, stderr := h.run(actionRelease, "--hold", pending, "--reason", "over a pending update")
	require.Equal(t, 1, code, "control: release over a pending update intent")
	require.Contains(t, stderr, holdrecord.ErrBusy.Error())
}

// rowText is the hold row as Postgres holds it now, as JSON.
func (h *heldStack) rowText() string {
	h.t.Helper()
	var text string
	require.NoError(h.t, h.s.Broker.Pool.QueryRow(context.Background(), `select row_to_json(t)::text from nats_hold t`).Scan(&text))
	return text
}

// putRowBack writes text, a row rowText read, back into Postgres: the older signed row a process
// that read the row before another ended the hold acts on, or a database writer puts back.
func (h *heldStack) putRowBack(text string) {
	h.t.Helper()
	_, err := h.s.Broker.Pool.Exec(context.Background(), `update nats_hold set (gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig) =
		(select gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig from json_populate_record(null::nats_hold, $1::json))`, text)
	require.NoError(h.t, err)
}

// Nothing is admitted after a done `hold release` or `hold abort`. After a release, `streams
// update --hold` is refused and writes no intent; and with the row as it stood before the release
// put back (held, under that hold id, signed), as a process that read the row before another
// ended the hold would act on it, the journal's end rule still refuses it: "the hold has ended:
// hold release is done". The same holds after an abort that ended a hold whose update stopped
// having made something. The control is the same command in a hold of the same workflow that has
// not ended, which runs and is done.
func TestHoldNoCommandAfterTheHoldEnded(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()

	released := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, released)
	before := h.rowText()
	code, _, _ := h.run(actionRelease, "--hold", released, "--reason", "nothing to do")
	require.Zero(t, code)
	after := h.rowText()
	code, _, _ = h.streamsRun(nil, "update", "--hold", released)
	require.Equal(t, 1, code, "an update after the hold's release")
	h.putRowBack(before)
	code, _, stderr := h.streamsRun(nil, "update", "--hold", released)
	require.Equal(t, 1, code, "an update after the hold's release, the held row put back")
	require.Contains(t, stderr, "the hold has ended: hold release is done")
	require.NotContains(t, h.journalCommands(released), CommandStreamsUpdate+" intent")
	h.putRowBack(after)

	aborted := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, aborted)
	_, err := h.s.Records.Admit(ctx, aborted, holdrecord.Request{Command: CommandStreamsUpdate}, nil)
	require.NoError(t, err)
	require.NoError(t, h.s.Records.AppendStopLeft(ctx, aborted, CommandStreamsUpdate, "the row's stop", "ENVOY_MEASURE made"))
	before = h.rowText()
	code, stdout, _ := h.run(actionAbort, "--hold", aborted, "--reason", "the update left a change")
	require.Zero(t, code, "abort after a stopped update")
	require.Contains(t, stdout, "ENVOY_MEASURE made")
	after = h.rowText()
	h.putRowBack(before)
	code, _, stderr = h.streamsRun(nil, "update", "--hold", aborted)
	require.Equal(t, 1, code, "an update after the hold's abort, the held row put back")
	require.Contains(t, stderr, "the hold has ended: hold abort is done")
	h.putRowBack(after)

	// The control: a hold of the same workflow that has not ended admits the update.
	open := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, open)
	code, _, _ = h.streamsRun(nil, "update", "--hold", open)
	require.Zero(t, code, "control: the update in a hold that has not ended")
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase)
}

// The broker keeps a floor for each hold record: an older signed journal put back into Postgres,
// below the gen the broker has served, answers 409 below_floor, which the operator's client reads
// as unknown. So `hold status` and `hold release` over it are UNKNOWN (exit 2), never a pass, and
// the row stays held. The control is the newest journal put back, after which release ends the hold.
func TestHoldAnOlderJournalPutBackIsUnknown(t *testing.T) {
	h := newHeldStack(t)
	ctx := context.Background()
	id := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	read := func() (string, uint64, string) {
		var value, sig string
		var gen uint64
		require.NoError(t, h.s.Broker.Pool.QueryRow(ctx,
			`select value::text, gen, sig from nats_hold_records where hold_id = $1 and kind = 'done'`, id).Scan(&value, &gen, &sig))
		return value, gen, sig
	}
	put := func(value string, gen uint64, sig string) {
		_, err := h.s.Broker.Pool.Exec(ctx,
			`update nats_hold_records set value = $1::jsonb, gen = $2, sig = $3 where hold_id = $4 and kind = 'done'`, value, gen, sig, id)
		require.NoError(t, err)
	}
	olderValue, olderGen, olderSig := read()
	// A stopped update moves the journal on twice, and the broker serves the newer journal.
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: CommandStreamsUpdate}, nil)
	require.NoError(t, err)
	require.NoError(t, h.s.Records.AppendStopLeft(ctx, id, CommandStreamsUpdate, "the row's stop", holdrecord.LeftUnchanged))
	_, newerGen, _ := read()
	require.Greater(t, newerGen, olderGen)
	newerValue, _, newerSig := read()
	_, _, err = h.s.Records.Journal(ctx, id)
	require.NoError(t, err)

	put(olderValue, olderGen, olderSig)
	for _, args := range [][]string{{actionStatus, "--hold", id}, {actionRelease, "--hold", id, "--reason", "over an older journal"}} {
		code, _, stderr := h.run(args...)
		require.Equal(t, 2, code, "%v over an older journal put back", args)
		require.Contains(t, stderr, "UNKNOWN")
		require.Contains(t, stderr, "below their floor")
	}
	require.Equal(t, natsgrant.OperatorPhaseHeld, h.row().Phase, "a release over an older journal moved the row")

	// The control: the newest journal back, and release ends the hold.
	put(newerValue, newerGen, newerSig)
	code, _, _ := h.run(actionRelease, "--hold", id, "--reason", "over the newest journal")
	require.Zero(t, code, "control: release over the newest journal")
}

// A `hold release` killed between its PUT and its done entry leaves the row released and its
// intent pending, so the journal holds no end and the end rule cannot apply. Its rerun sends no
// PUT, writes the done entry, and from then the end rule refuses a command even with the held row
// put back. It fails at a release that never writes its done entry, or whose rerun on a released
// row writes nothing.
func TestHoldAReleaseKilledBeforeItsDoneEntry(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	ctx := context.Background()
	id := h.s.Prepare(t, holdrecord.WorkflowStreamsUpdate)
	h.s.HoldWithoutTheSteps(t, id)
	held := h.rowText()

	// The release, killed after its PUT.
	_, err := h.s.Records.Admit(ctx, id, holdrecord.Request{Command: holdrecord.CommandRelease}, nil)
	require.NoError(t, err)
	_, err = h.s.Records.MoveRow(ctx, h.row().Gen, natsgrant.OperatorPhaseReleased, id, "the release, killed after its PUT")
	require.NoError(t, err)
	last := h.lastEntry(id)
	require.Equal(t, holdrecord.CommandRelease+" intent", last.Command+" "+last.State)
	released := h.rowText()

	code, stdout, _ := h.run(actionRelease, "--hold", id, "--reason", "the rerun of the killed release")
	require.Zero(t, code, "the rerun of a release killed before its done entry")
	require.Contains(t, stdout, "its release's done entry is now written")
	last = h.lastEntry(id)
	require.Equal(t, holdrecord.CommandRelease+" done", last.Command+" "+last.State, "the rerun wrote no done entry")
	require.Equal(t, released, h.rowText(), "the rerun moved the row")

	h.putRowBack(held)
	code, _, stderr := h.streamsRun(nil, "update", "--hold", id)
	require.Equal(t, 1, code, "an update after the rerun's done entry, the held row put back")
	require.Contains(t, stderr, "the hold has ended: hold release is done")
	h.putRowBack(released)
}
