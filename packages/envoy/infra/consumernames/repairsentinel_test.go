package consumernames

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// repairWord is the command's word as the operator types it, so a row also fails when the usage
// does not take it.
const repairWord = "repair-sentinel"

// repairSentinelValue is the sentinel's value in the selection bucket, raw, as the operator reads
// it on a connection of its own (statusSelectionKeys), nil while the key holds none.
func repairSentinelValue(t *testing.T, h *stack) []byte {
	t.Helper()
	nc, _ := h.operator()
	return malformedValue(t, nc, selection.SentinelKey)
}

// repairPlant stores the server's STREAM.INFO answer of the notification stream over the
// sentinel, as as, any principal whose grant holds a JetStream API request, does by naming the
// sentinel key's subject as the request's reply subject, and answers the value the key then holds,
// read as the operator.
func repairPlant(t *testing.T, h *stack, as *nats.Conn) []byte {
	t.Helper()
	return storeStreamInfoAnswer(t, as, selection.SentinelKey, func() []byte { return repairSentinelValue(t, h) })
}

// repairRecordsSentinel is the sentinel the broker's records give, read by the row from the
// broker's routes: the session family's cutover row for the state, the hold that row names (for
// rolled_back, the cutover hold that rollback's saved set records), and that hold's cohort record
// for the boundary, as json.Marshal writes it under the sentinel's own key and family
// (ownSentinel). A row checks it against the bytes convert or restore --finish wrote before the
// damage, so it is no copy of the command's own reading.
func repairRecordsSentinel(t *testing.T, h *stack) []byte {
	t.Helper()
	ctx := context.Background()
	row, _ := h.cutoverRow(familySession)
	require.NotEmpty(t, row.HoldID, "the broker holds no session cutover row")
	hold := row.HoldID
	if row.State == selection.CutoverRolledBack {
		set, found, err := readRestoreSet(ctx, h.records(), row.HoldID)
		require.NoError(t, err)
		require.True(t, found, "rollback hold %s saved no set", row.HoldID)
		hold = set.CutoverHold
	}
	cohort, _, found, err := readPrepareCohort(ctx, h.records(), hold)
	require.NoError(t, err)
	require.True(t, found, "hold %s has no cohort record", hold)
	value, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: row.State, Boundary: cohort.Boundary, Hold: hold}))
	require.NoError(t, err)
	return value
}

// repairCheck is the rows' check of a sentinel: got must be want, the broker's records' sentinel,
// byte for byte.
func repairCheck(want, got []byte) error {
	if !bytes.Equal(want, got) {
		return fmt.Errorf("the sentinel %s is not the broker's records' %s", got, want)
	}
	return nil
}

// repairCall is one session consumer call of the listener's fresh mode for logical, as the
// listener makes it (internal/bus session_fresh.go newFreshSessions): selection.Names over the
// selection bucket through the listener's own connection, the rig's writer. Its create fails the
// call: a call that serves must select the session's recorded consumer, never make one.
func (rig *convertRig) repairCall(logical string) (selection.Selection, error) {
	names := selection.New(selection.Config{
		Store:    selection.NewKVStore(selection.ConnRequester{Conn: rig.writerConn}),
		Server:   convertTestServer{rig},
		Prefix:   func(string) string { return sessionFreshPrefix },
		Fixed:    selection.SessionNaming().Fixed,
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return convertRigStreamID, nil },
		Owner:    "the repair row's listener",
	})
	sel, _, err := names.Ensure(context.Background(), logical, selection.CreateRequest{
		Create: func(context.Context, string, selection.Start) (*jetstream.ConsumerInfo, error) {
			return nil, errors.New("the call created a consumer, where the session's record selects one")
		},
		First: func(context.Context) (selection.Start, error) { return selection.Start{Seq: 1}, nil },
	})
	return sel, err
}

// After a cutover (prepare, create-bucket, convert, a clean census, release), a listener, whose
// grant holds a JetStream API request, stores a STREAM.INFO answer over the selection bucket's
// sentinel through a reply subject. Every session consumer call of the listener's fresh mode then
// fails closed for a session with a record and one without: the selection store reads the answer as
// no sentinel of its own (selection.ErrMalformedSentinel). Control: a sentinel-repair hold set and
// released with no repair leaves every call failing, as today. repair-sentinel, in a
// sentinel-repair hold, writes the sentinel the broker's records give, its intent then its done
// entry completing the hold, and
// after the release the call serves again and selects the session's recorded consumer. The
// rewritten sentinel is byte for byte the one convert wrote before the damage, and the broker's
// records give exactly those bytes. Control of that check: a sentinel built from NATS state alone
// (the stream's end, its identity, the repair's hold) is refused by it.
func TestRepairSentinelRewritesAMalformedSentinelSoSessionsServeAgain(t *testing.T) {
	ctx := context.Background()
	rig := newConvertRig(t, 0)
	rig.publish(contracts.LegacyAgentSubject("r1"), 3)
	// The fixed name with the filters the listener gives a session's consumer, both topic forms of
	// its agent subject (bus.SessionConsumerFilters), which the census takes as a session's.
	_, err := rig.writer.CreateConsumer(ctx, notifications, jetstream.ConsumerConfig{
		Durable: sessionFixedPrefix + "r1", FilterSubjects: bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject("r1")}),
		AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Minute, DeliverPolicy: jetstream.DeliverAllPolicy, MaxDeliver: -1,
	})
	require.NoError(t, err)
	cutover := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", cutover)
	require.Zero(t, code, stderr)
	rig.closeWriter()
	rig.held(cutover)
	for _, action := range []string{actionCreateBucket, actionConvert} {
		code, _, stderr = rig.run(action, "--hold", cutover)
		require.Zero(t, code, "%s: %s", action, stderr)
	}
	rig.admittedCensus(cutover, familySession)
	require.NoError(t, rig.release(cutover))
	converted := repairSentinelValue(t, rig.stack)
	records := repairRecordsSentinel(t, rig.stack)
	require.NoError(t, repairCheck(records, converted), "the broker's records give the sentinel convert wrote")
	current := rig.record(familySession, "r1").Current
	require.Contains(t, current, sessionFreshPrefix+"~", "convert selected no fresh name for r1")

	rig.connectWriter()
	sel, err := rig.repairCall("r1")
	require.NoError(t, err, "before the damage the call serves")
	require.Equal(t, current, sel.Record.Current)
	malformed := repairPlant(t, rig.stack, rig.writerConn)
	_, _, _, err = selection.DecodeKVValue(selection.SentinelKey, malformed)
	require.ErrorIs(t, err, selection.ErrMalformedSentinel, "the planted value is the bucket's sentinel")
	for _, logical := range []string{"r1", "r-new"} {
		_, err := rig.repairCall(logical)
		require.ErrorIs(t, err, selection.ErrMalformedSentinel, "the call for %s over the malformed sentinel", logical)
	}

	rig.closeWriter()
	none := rig.heldHold(holdrecord.WorkflowSentinelRepair)
	require.NoError(t, rig.release(none))
	rig.connectWriter()
	_, err = rig.repairCall("r1")
	require.ErrorIs(t, err, selection.ErrMalformedSentinel, "control: with no repair the call still fails")
	require.Equal(t, malformed, repairSentinelValue(t, rig.stack), "control: the sentinel changed with no repair")

	rig.closeWriter()
	repair := rig.heldHold(holdrecord.WorkflowSentinelRepair)
	code, stdout, stderr := rig.run(repairWord, "--hold", repair)
	require.Zero(t, code, stderr)
	require.Contains(t, stdout, "(it held "+repairWasMalformed+")")
	require.Equal(t, []string{"intent", "done"}, createBucketEntries(rig.journal(repair), repairCommand))
	require.True(t, rig.journal(repair).Complete, "repair-sentinel's done entry completes the hold")
	written := repairSentinelValue(t, rig.stack)
	require.NoError(t, repairCheck(records, written))
	require.Equal(t, string(converted), string(written), "the rewritten sentinel is the one convert wrote")
	require.NoError(t, rig.release(repair))

	rig.connectWriter()
	sel, err = rig.repairCall("r1")
	require.NoError(t, err, "after the repair the call serves")
	require.Equal(t, current, sel.Record.Current, "the call selects r1's recorded consumer")
	require.False(t, sel.Created)

	nc, _ := rig.operator()
	stream, err := streamInfo(ctx, nc, notifications)
	require.NoError(t, err)
	fromNATS, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: stream.State.LastSeq + 1,
		StreamID: selection.StreamIdentity(stream), Hold: repair}))
	require.NoError(t, err)
	require.Error(t, repairCheck(records, fromNATS), "control: the check takes a sentinel built from NATS state alone")
}

// repair-sentinel refuses, before its intent and writing nothing, when no cutover's records give
// the sentinel: with no session cutover row it refuses, exit 1, naming the row; with a session row
// done under a hold that has no cohort record it is UNKNOWN, exit 2, naming that hold's cohort
// record. Either way the journal holds no entry of it, the malformed sentinel stays as it was, and
// hold release, which a journal holding hold set's entries alone admits, ends the hold. Control: the
// sentinel is malformed (the readers' decode refuses it), so a write would have changed it.
func TestRepairSentinelRefusesWhenNoCutoverRecordsGiveTheSentinel(t *testing.T) {
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 2)
	id := h.heldHold(holdrecord.WorkflowSentinelRepair)
	nc, _ := h.operator()
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1, Hold: "an-earlier-hold"})
	require.NoError(t, err)
	malformed := malformedReplyStore(t, nc, selection.SentinelKey)
	malformedRefused(t, selection.SentinelKey, malformed)

	code, _, stderr := h.run(repairWord, "--hold", id)
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "the broker holds no "+familySession+" cutover row")
	require.Empty(t, createBucketEntries(h.journal(id), repairCommand), "the refused repair wrote an entry")
	require.Equal(t, malformed, repairSentinelValue(t, h), "the refused repair wrote the sentinel")

	_, err = h.records().PutCutover(ctx, familySession, selection.CutoverDone, id)
	require.NoError(t, err)
	code, _, stderr = h.run(repairWord, "--hold", id)
	require.Equal(t, 2, code, stderr)
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, "hold "+id+", the session family's cutover, has no cohort record")
	require.Empty(t, createBucketEntries(h.journal(id), repairCommand), "the UNKNOWN repair wrote an entry")
	require.Equal(t, malformed, repairSentinelValue(t, h), "the UNKNOWN repair wrote the sentinel")
	require.NoError(t, h.release(id), "hold release after repairs that wrote nothing")
}

// repairRow takes the session family's row when it reads done or rolled_back, and refuses, naming
// it, a row that reads anything else and no row at all; the broker's table holds only done and
// rolled_back (its check constraint), so a third state is this unit's row alone. Control: the
// other families' rows are not the session's, so a hub whose only rows are theirs is refused.
func TestRepairRowTakesOnlyADoneOrRolledBackSessionRow(t *testing.T) {
	dispatch := holdrecord.CutoverRow{Family: familyDispatch, State: selection.CutoverDone, HoldID: "h-d"}
	for _, state := range []string{selection.CutoverDone, selection.CutoverRolledBack} {
		session := holdrecord.CutoverRow{Family: familySession, State: state, HoldID: "h-s"}
		row, err := repairRow([]holdrecord.CutoverRow{dispatch, session})
		require.NoError(t, err, state)
		require.Equal(t, session, row)
	}
	_, err := repairRow([]holdrecord.CutoverRow{dispatch, {Family: familySession, State: "pending", HoldID: "h-s"}})
	require.ErrorContains(t, err, `the broker's session cutover row reads "pending" from hold h-s, neither done nor rolled_back`)
	_, err = repairRow([]holdrecord.CutoverRow{dispatch})
	require.ErrorContains(t, err, "the broker holds no session cutover row")
}

// repairDecide writes the records' sentinel over a malformed value and over another store state,
// leaves a value that is the records' sentinel already, and refuses a key with no value, a KV
// delete marker and a selection recovery's sentinel, each before any write. Control: the
// malformed value is one the library's lenient decode takes, as an empty store state.
func TestRepairDecideWritesOnlyOverAValue(t *testing.T) {
	want, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 4, Hold: "h-cutover"}))
	require.NoError(t, err)
	other, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 9, Hold: "h-other"}))
	require.NoError(t, err)
	recovery, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 4, ReplayFrom: 7, BackupSHA256: "none", Hold: "h-recovery"}))
	require.NoError(t, err)
	malformed := []byte(`{"type":"io.nats.jetstream.api.v1.stream_info_response","config":{"name":"ENVOY_NOTIFICATIONS"}}`)
	var lenient selection.StoreState
	require.NoError(t, json.Unmarshal(malformed, &lenient), "control: the lenient decode refused the answer")
	require.Equal(t, selection.StoreState{}, lenient)

	for _, row := range []struct {
		data  []byte
		write bool
		was   string
	}{
		{malformed, true, repairWasMalformed},
		{other, true, repairWasOther},
		{want, false, repairWasRecords},
	} {
		write, was, err := repairDecide(replayMessage{Seq: 3, Data: row.data}, true, want)
		require.NoError(t, err, row.was)
		require.Equal(t, row.write, write, row.was)
		require.Equal(t, row.was, was)
	}
	for _, row := range []struct {
		last   replayMessage
		found  bool
		refuse string
	}{
		{replayMessage{}, false, "holds no envoy.bucket"},
		{replayMessage{Seq: 5, Op: "DEL"}, true, "at sequence 5 is a KV DEL marker"},
		{replayMessage{Seq: 6, Data: recovery}, true, "at sequence 6 is a selection recovery's sentinel (hold h-recovery)"},
	} {
		write, _, err := repairDecide(row.last, row.found, want)
		require.ErrorContains(t, err, row.refuse)
		require.False(t, write)
	}
}

// restoreCutoverHold records the hold of a done session row, nothing for a hub with no session row
// (a convert that never finished, whose rollback the interrupted-conversion rows run), and refuses
// a rolled_back row, naming it. Control: another family's row is not read.
func TestRestoreCutoverHoldIsTheDoneSessionRowsHold(t *testing.T) {
	dispatch := holdrecord.CutoverRow{Family: familyDispatch, State: selection.CutoverRolledBack, HoldID: "h-d"}
	hold, err := restoreCutoverHold([]holdrecord.CutoverRow{dispatch, {Family: familySession, State: selection.CutoverDone, HoldID: "h-cutover"}})
	require.NoError(t, err)
	require.Equal(t, "h-cutover", hold)
	hold, err = restoreCutoverHold([]holdrecord.CutoverRow{dispatch})
	require.NoError(t, err)
	require.Empty(t, hold)
	_, err = restoreCutoverHold([]holdrecord.CutoverRow{{Family: familySession, State: selection.CutoverRolledBack, HoldID: "h-rollback"}})
	require.ErrorContains(t, err, "the broker's session cutover row reads rolled_back from hold h-rollback")
}

// After a cutover and its rollback (restore --prepare, restore --finish, release), a listener
// stores a STREAM.INFO answer over the sentinel, which the readers' decode refuses and the
// library's reads as no marker. repair-sentinel follows the rollback's saved set to the cutover
// hold (restoreSet's CutoverHold, recorded at restore --prepare) and that hold's cohort record, and
// writes {cutover rolled_back, boundary, the cutover's hold}, byte for byte what restore --finish
// left; after the release a fixed-mode listener's read of the sentinel (the listener's
// RefuseConvertedStore reads selection.Store.State) succeeds with rolled_back, which is no done, so
// fixed names run, and the readers' decode takes it. Control: the rollback run by the earlier
// restore (restoreRuleSet's noCutoverHold) saves no cutover hold, and the repair stops UNKNOWN
// naming it, before its intent, the malformed sentinel left as it was.
func TestRepairSentinelAfterARollbackLetsFixedListenersStart(t *testing.T) {
	ctx := context.Background()
	rolledBack := func(t *testing.T) (*restoreRig, string, string, []byte) {
		t.Helper()
		rig := newRestoreRig(t)
		const subject = "notifications.agent.rb"
		seqs := rig.publish(subject, 3)
		rig.fixed(restoreFixedConfig(sessionFixedPrefix+"rb", subject), 1)
		cutover := rig.cutover(restoreConversion{fixed: sessionFixedPrefix + "rb", start: seqs[1]})
		rollback := rig.heldHold(holdrecord.WorkflowRollback)
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.NoError(t, rig.release(rollback))
		left := repairSentinelValue(t, rig.stack)
		var state selection.StoreState
		require.NoError(t, json.Unmarshal(left, &state))
		require.Equal(t, ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverRolledBack, Boundary: 1, Hold: cutover}), state,
			"restore --finish keeps the cutover's boundary and hold")
		nc, _ := rig.listener()
		malformed := repairPlant(t, rig.stack, nc)
		nc.Close()
		malformedRefused(t, selection.SentinelKey, malformed)
		return rig, cutover, rollback, left
	}

	t.Run("control: the earlier restore saves no cutover hold, and the repair stops UNKNOWN", func(t *testing.T) {
		restoreWithRules(t, restoreRuleSet{noCutoverHold: true})
		rig, _, rollback, _ := rolledBack(t)
		set, found, err := readRestoreSet(ctx, rig.records(), rollback)
		require.NoError(t, err)
		require.True(t, found)
		require.Empty(t, set.CutoverHold)
		malformed := repairSentinelValue(t, rig.stack)
		repair := rig.heldHold(holdrecord.WorkflowSentinelRepair)
		code, _, stderr := rig.run(repairWord, "--hold", repair)
		require.Equal(t, 2, code, stderr)
		require.Contains(t, stderr, "UNKNOWN")
		require.Contains(t, stderr, "hold "+rollback+", whose rollback the broker's session cutover row names, records no cutover hold")
		require.Empty(t, createBucketEntries(rig.journal(repair), repairCommand))
		require.Equal(t, malformed, repairSentinelValue(t, rig.stack))
		require.NoError(t, rig.release(repair))
	})

	t.Run("treatment", func(t *testing.T) {
		rig, cutover, rollback, left := rolledBack(t)
		set, found, err := readRestoreSet(ctx, rig.records(), rollback)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, cutover, set.CutoverHold, "restore --prepare recorded the cutover hold")
		records := repairRecordsSentinel(t, rig.stack)
		require.NoError(t, repairCheck(records, left), "the broker's records give the sentinel restore --finish left")

		repair := rig.heldHold(holdrecord.WorkflowSentinelRepair)
		code, _, stderr := rig.run(repairWord, "--hold", repair)
		require.Zero(t, code, stderr)
		require.True(t, rig.journal(repair).Complete)
		written := repairSentinelValue(t, rig.stack)
		require.Equal(t, string(left), string(written), "the rewritten sentinel is the one restore --finish left")
		require.NoError(t, rig.release(repair))

		nc, _ := rig.listener()
		defer nc.Close()
		state, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).State(ctx)
		require.NoError(t, err, "the fixed-mode listener's read of the sentinel")
		require.Equal(t, selection.CutoverRolledBack, state.Cutover, "the fixed-mode listener reads rolled_back, which is no done, and runs")
		malformedRefused(t, selection.SentinelKey, []byte(`{"type":"io.nats.jetstream.api.v1.stream_info_response"}`))
		_, _, _, err = selection.DecodeKVValue(selection.SentinelKey, written)
		require.NoError(t, err, "the readers' decode takes the rewritten sentinel")
	})
}

// A repair-sentinel whose done entry is lost writes its stop, recording what it left as unknown,
// and hold abort is then the way out (requireALostDoneStops, with its control: the intent left
// pending under the earlier rule). The broker's records give the sentinel a cutover hold's
// create-bucket and convert would leave: its cohort record's B, and the session family's cutover
// row done under it; the bucket's sentinel already holds it, so each run leaves it as it is.
func TestARepairSentinelWhoseDoneEntryIsLostStops(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	createBucketNotifications(t, h, 0)
	cutover := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), cutover, 2)
	h.held(cutover)
	nc, _ := h.operator()
	createBucketMakeBucket(t, nc, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, Hold: cutover})
	_, err := h.records().PutCutover(ctx, familySession, selection.CutoverDone, cutover)
	require.NoError(t, err)
	require.NoError(t, h.release(cutover))
	repair := h.heldHold(holdrecord.WorkflowSentinelRepair)
	h.requireALostDoneStops(repair, repairCommand, func() (int, string, string) {
		return h.run(repairWord, "--hold", repair)
	})
}
