package consumernames

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// convertWith runs `consumer-names convert --hold holdID` in the rig on ctx under rule, as
// runCommand runs it, and answers its exit code and its standard error.
func (rig *convertRig) convertWith(ctx context.Context, rule convertRule, holdID string) (int, string) {
	rig.t.Helper()
	rig.closeOperators()
	full := append(append([]string{actionConvert, "--hold", holdID}, commonArgs...), writingArgs...)
	var stdout, stderr bytes.Buffer
	o, code := parse(full, &stderr)
	require.Equal(rig.t, -1, code, stderr.String())
	r := &run{o: o, e: rig.e, stdout: &stdout, stderr: &stderr}
	defer r.close()
	code = report(&stderr, o.action, rule.run(ctx, r))
	rig.t.Logf("envoy-infra consumer-names convert --hold %s (rule %+v): exit %d\n%s%s", holdID, rule, code, stdout.String(), stderr.String())
	return code, stderr.String()
}

// censusClean runs `streams census` (censusCommand: writerhold's streams census word with this
// package's readers) for families until a run prints CLEAN, three runs at most, and requires one
// does. The word writes its own journal entries: its intent at each run, a stop entry after each
// run that is not clean, and its done entry after the clean one.
func (rig *convertRig) censusClean(holdID string, families ...string) {
	rig.t.Helper()
	var extra []string
	for _, family := range families {
		extra = append(extra, "--family", family)
	}
	for range 3 {
		code, stdout, _ := rig.censusCommand(holdID, extra...)
		if code == 0 && slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN") {
			return
		}
	}
	rig.t.Fatal("no streams census run was clean in three")
}

// censusCommandName is `envoy-infra streams census` as the cutover's workflow table names it, its
// final command (writerhold.CommandStreamsCensus).
const censusCommandName = writerhold.CommandStreamsCensus

// admittedCensus runs `streams census` until a clean run (censusClean): the word admits each run
// itself (holdrecord.Client.Admit, its intent), and the clean run's done entry, the census being
// the cutover's final command, completes the record, which it requires.
func (rig *convertRig) admittedCensus(holdID string, families ...string) {
	rig.t.Helper()
	rig.censusClean(holdID, families...)
	require.Equal(rig.t, "done", rig.lastEntry(holdID, censusCommandName).State, "the clean census wrote no done entry")
}

// Completion covers the whole cutover: create-bucket succeeds and leaves the hold's record
// incomplete, so release is refused; a replay inside the hold does not complete it either; convert
// killed before its last listing leaves its intent, and release is refused; a streams census sent
// then exits naming convert and changes nothing; after convert's rerun and a clean census, release
// succeeds. Control: round 17's rule made create-bucket's done entry the final one, which would
// have completed the record right there: the record reads incomplete after it, and release is
// refused, where that rule accepted it. It fails when any step before convert and the census
// completes the hold, or the census runs before convert.
func TestCompletionCoversTheWholeCutover(t *testing.T) {
	rig := newConvertRig(t, 0)
	rig.publish("notifications.agent.c1", 3)
	rig.publish("notifications.agent.c2", 3)
	rig.fixed("s-c1", "notifications.agent.c1", 0)
	rig.fixed("s-c2", "notifications.agent.c2", 0)
	id := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(t, code, stderr)
	rig.closeWriter()
	rig.held(id)

	code, _, stderr = rig.run(actionCreateBucket, "--hold", id)
	require.Zero(t, code, stderr)
	require.Equal(t, "done", rig.lastEntry(id, actionCreateBucket).State)
	require.False(t, rig.journal(id).Complete, "control: create-bucket's done entry completed the record, as round 17's rule did")
	require.ErrorIs(t, rig.release(id), holdrecord.ErrOutOfOrder, "release after create-bucket alone")

	code, _, stderr = rig.run(actionReplay, "--hold", id, "--key", "c9", "--from", "1")
	require.Zero(t, code, stderr)
	require.ErrorIs(t, rig.release(id), holdrecord.ErrOutOfOrder, "release after a replay")

	killed, kill := context.WithCancel(context.Background())
	listings := 0
	code, stderr = rig.convertWith(killed, convertRule{listed: func(context.Context, []string) {
		if listings++; listings == 2 {
			kill()
		}
	}}, id)
	kill()
	require.NotZero(t, code, "the convert killed before its last listing ended clean")
	require.Equal(t, 2, listings, "convert was not killed at its last listing")
	require.Equal(t, "intent", rig.lastEntry(id, actionConvert).State)
	require.ErrorIs(t, rig.release(id), holdrecord.ErrBusy, "release after the killed convert")

	nc, _ := rig.operator()
	before := censusConsumers(t, nc)
	journal := rig.journal(id).Ops
	code, _, stderr = rig.censusCommand(id, "--family", familySession)
	require.NotZero(t, code, "the census ran before convert completed")
	require.Contains(t, stderr, "convert")
	require.Contains(t, stderr, holdrecord.ErrBusy.Error(), "the word's own admission refuses while convert's intent is pending")
	require.Equal(t, journal, rig.journal(id).Ops, "the refused census wrote to the journal")
	nc, _ = rig.operator()
	require.Equal(t, before, censusConsumers(t, nc), "the refused census changed the stream's consumers")
	nc.Close()
	_, err := rig.records().Admit(context.Background(), id, holdrecord.Request{Command: censusCommandName}, nil)
	require.ErrorIs(t, err, holdrecord.ErrBusy, "the census's admission while the killed convert's intent is pending")

	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	rig.admittedCensus(id, familySession)
	require.True(t, rig.journal(id).Complete)
	require.NoError(t, rig.release(id))
}

// A forged store marker completes nothing: in a cutover hold where convert has not run, every
// family's store marker is written cutover = done as the stores' own writers write it (the
// bucket's sentinel by a conditional KV write of its key, which a listener's KVWriteExisting grant
// admits, and Dispatch's and the daemon's intake_consumers rows through their databases), and no
// broker cutover row exists. streams census, with every store its readers read named, exits naming
// convert and changes nothing: the word's own admission is refused, convert having no done entry,
// before it reads any marker. create-bucket's entries leave the record incomplete, and release is
// refused.
// Control: every store marker reads done, which round 17's rule read, so that rule's census would
// have run and its release succeeded with every fixed name unconverted. It fails when any step
// decides on a store marker. The sentinel's write goes through the operator's connection: a
// listener connected across this harness's hold, which skips the restart, is refused by
// create-bucket's held re-read (TestConvertRereadCatchesAWriterConnectedAcrossTheHold).
func TestAForgedStoreMarkerCompletesNothing(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	dispatch := rig.database(familyDispatch, dispatchMigrations...)
	daemon := rig.database(daemonFamily("p1"), daemonMigrations...)
	rig.publish("notifications.agent.f1", 2)
	rig.fixed("s-f1", "notifications.agent.f1", 0)
	rig.fixed(dispatchDurables()[0], "notifications.dispatch.f", 0)
	rig.fixed("legion-go-p1-wake", "notifications.legion.p1.controller", 0)
	id := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(t, code, stderr)
	rig.closeWriter()
	rig.held(id)
	code, _, stderr = rig.run(actionCreateBucket, "--hold", id)
	require.Zero(t, code, stderr)

	forger, _ := rig.operator()
	store := newSessionStore(forger)
	state, revision, err := convertSentinel(ctx, store, familySession)
	require.NoError(t, err)
	state.Cutover = selection.CutoverDone
	_, err = store.WriteSentinel(ctx, state, revision)
	require.NoError(t, err, "the forged write of the sentinel, as the operator")
	for _, store := range []pgFamilyStore{pgFamilyOf(t, familyDispatch, dispatch), pgFamilyOf(t, daemonFamily("p1"), daemon)} {
		_, err := store.WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone}, 0)
		require.NoError(t, err)
	}
	cutovers, err := rig.records().Cutovers(ctx)
	require.NoError(t, err)
	require.Empty(t, cutovers, "a broker cutover row exists before convert")
	nc, _ := rig.operator()
	for name, store := range map[string]selection.Store{
		familySession:      selection.NewKVStore(selection.ConnRequester{Conn: nc}),
		familyDispatch:     pgFamilyOf(t, familyDispatch, dispatch),
		daemonFamily("p1"): pgFamilyOf(t, daemonFamily("p1"), daemon),
	} {
		state, err := store.State(ctx)
		require.NoError(t, err)
		require.Equal(t, selection.CutoverDone, state.Cutover, "control: %s's store marker reads done, which round 17's rule read", name)
	}

	before := censusConsumers(t, nc)
	journal := rig.journal(id).Ops
	code, _, stderr = rig.censusCommand(id, "--family", familySession, "--family", familyDispatch, "--family", daemonFamily("p1"),
		"--dispatch-database-file", censusDatabaseFile(t, dispatch), "--daemon-database", "p1="+censusDatabaseFile(t, daemon))
	require.NotZero(t, code, "the census ran on forged store markers")
	require.Contains(t, stderr, "convert")
	require.Contains(t, stderr, holdrecord.ErrOutOfOrder.Error(), "the word's own admission refuses before convert")
	require.Equal(t, journal, rig.journal(id).Ops, "the refused census wrote to the journal")
	nc, _ = rig.operator()
	require.Equal(t, before, censusConsumers(t, nc), "the refused census changed the stream's consumers")
	nc.Close()
	require.Len(t, rig.fixedNames(), 3, "a fixed name was converted or removed")
	require.False(t, rig.journal(id).Complete, "create-bucket's entries completed the record")
	_, err = rig.records().Admit(ctx, id, holdrecord.Request{Command: censusCommandName}, nil)
	require.ErrorIs(t, err, holdrecord.ErrOutOfOrder, "the census's admission before convert")
	require.ErrorIs(t, rig.release(id), holdrecord.ErrOutOfOrder)
}
