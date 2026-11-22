package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsversion"
)

// restoreStreamID is the identity the rows' notification stream carries (bus.StreamIDMetadataKey),
// which a record's last floor names to count.
const restoreStreamID = "restore-rows-stream"

// restoreRig is one rollback row's surrounding: the stack, a Postgres store of each family the row
// names beside the selection bucket, the flags that name those stores, ENVOY_NOTIFICATIONS with its
// identity, and the copies the stream and the selection bucket keep.
type restoreRig struct {
	*stack
	pools    map[string]*pgxpool.Pool
	flags    []string
	replicas int
}

// newRestoreRig starts a rig of one copy with a store of each of families (familyDispatch,
// daemonFamily(p)).
func newRestoreRig(t *testing.T, families ...string) *restoreRig {
	t.Helper()
	return newRestoreRigOf(t, 1, families...)
}

// newRestoreRigOf starts a rig whose stream and selection bucket keep replicas copies on the
// stack's three servers, with a store of each of families, and ENVOY_NOTIFICATIONS with the stream
// identity restoreStreamID, made as `envoy-infra streams update` makes it, by the operator inside
// a streams-update hold, which is released again.
func newRestoreRigOf(t *testing.T, replicas int, families ...string) *restoreRig {
	t.Helper()
	rig := restoreRigOn(newStack(t), families...)
	rig.replicas = replicas
	operatorInHold(t, rig.stack, holdrecord.WorkflowStreamsUpdate, func(nc *nats.Conn, js jetstream.JetStream) {
		config := notificationStreamConfig(t, replicas)
		config.Metadata = map[string]string{bus.StreamIDMetadataKey: restoreStreamID}
		stream, err := js.CreateStream(context.Background(), config)
		require.NoError(t, err)
		require.Equal(t, replicas, stream.CachedInfo().Config.Replicas)
		rig.waitLeader(nc, notifications)
	})
	return rig
}

// restoreRigOn is a rig of one copy on h, a stack that holds ENVOY_NOTIFICATIONS or will, with a
// store of each of families: each daemon's rendered with its Deployment and named by a real file
// (enrollDeployed), since every writer of a daemon store reads it at the address its one source
// names.
func restoreRigOn(h *stack, families ...string) *restoreRig {
	h.t.Helper()
	rig := &restoreRig{stack: h, pools: map[string]*pgxpool.Pool{}, replicas: 1}
	for _, family := range families {
		if project, ok := strings.CutPrefix(family, familyDaemonPrefix); ok {
			rig.pools[family] = rig.database(family, daemonMigrations...)
			rig.flags = append(rig.flags, "--daemon-database", project+"="+rig.enrollDeployed(project))
			continue
		}
		rig.pools[family] = rig.database(family, dispatchMigrations...)
		rig.flags = append(rig.flags, "--dispatch-database-file", "/dev/null")
	}
	return rig
}

// listener connects as the stack's first listener entry, which publishes every notification and
// creates, pulls and deletes any consumer of ENVOY_NOTIFICATIONS, as every listener build may; the
// callout admits it only while the row is released. The caller closes it before any held command,
// whose census reads every connection the hub holds.
func (rig *restoreRig) listener() (*nats.Conn, jetstream.JetStream) {
	rig.t.Helper()
	nc, err := rig.s.Connect(rig.s.Cluster.URL(), rig.s.ListenerCredential(holdtest.ListenerMachines[0]))
	require.NoError(rig.t, err)
	js, err := jetstream.New(nc, jetstream.WithDefaultTimeout(harnessAPITimeout))
	require.NoError(rig.t, err)
	return nc, js
}

// publish publishes n messages on subject as the listener and answers their sequences.
func (rig *restoreRig) publish(subject string, n int) []uint64 {
	rig.t.Helper()
	nc, js := rig.listener()
	defer nc.Close()
	var seqs []uint64
	for range n {
		ack, err := js.Publish(context.Background(), subject, []byte(subject))
		require.NoError(rig.t, err)
		seqs = append(seqs, ack.Sequence)
	}
	return seqs
}

// restoreFixedConfig is a fixed consumer's configuration as a build before the fresh names makes
// it: a durable pull consumer from the stream's start over filters.
func restoreFixedConfig(name string, filters ...string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{Durable: name, DeliverPolicy: jetstream.DeliverAllPolicy, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 30 * time.Second, MaxDeliver: -1, InactiveThreshold: time.Hour, FilterSubjects: filters}
}

// fixed creates the fixed consumer config describes, as the listener, and acks its first acked
// messages, waiting until its ack floor holds the last of them.
func (rig *restoreRig) fixed(config jetstream.ConsumerConfig, acked int) {
	rig.t.Helper()
	ctx := context.Background()
	nc, js := rig.listener()
	defer nc.Close()
	_, err := writeConsumer(ctx, nc, notifications, config, actionCreateConsumer)
	require.NoError(rig.t, err)
	if acked == 0 {
		return
	}
	consumer, err := js.Consumer(ctx, notifications, config.Durable)
	require.NoError(rig.t, err)
	var last uint64
	for done := 0; done < acked; {
		batch, err := consumer.Fetch(acked-done, jetstream.FetchMaxWait(5*time.Second))
		require.NoError(rig.t, err)
		for msg := range batch.Messages() {
			meta, err := msg.Metadata()
			require.NoError(rig.t, err)
			require.NoError(rig.t, msg.DoubleAck(ctx))
			last = max(last, meta.Sequence.Stream)
			done++
		}
		require.NoError(rig.t, batch.Error())
	}
	require.Eventually(rig.t, func() bool {
		info, err := consumer.Info(ctx)
		return err == nil && info.AckFloor.Stream >= last
	}, 10*time.Second, 50*time.Millisecond, "%s's ack floor never reached %d", config.Durable, last)
}

// restoreConversion is one fixed name a row's cutover converts. The row writes what convert leaves
// through the stores and the stream directly, as the operator in the cutover's hold: the record,
// the fresh name created at the saved start with F's configuration, F deleted, F's configuration
// in the hold's convert record, each converted family's broker cutover row and store marker done.
type restoreConversion struct {
	fixed string
	start uint64
	// interrupted is a convert that wrote op = {replace, old F, candidate N} and its start, and
	// stopped before it deleted F: F stays, nothing is created, and its family gets no cutover row.
	interrupted bool
	// edit changes the record after the conversion, fresh being its fresh name.
	edit func(record *selection.Record, fresh string)
	// deleteFresh deletes the fresh name outside the protocol after the conversion.
	deleteFresh bool
}

// waitLeader waits until stream has a leader, as nc reads it: a stream of three copies elects one
// a moment after its create, and a write before then finds no responder.
func (rig *restoreRig) waitLeader(nc *nats.Conn, stream string) {
	rig.t.Helper()
	require.Eventually(rig.t, func() bool {
		info, err := streamInfo(context.Background(), nc, stream)
		return err == nil && info.Cluster != nil && info.Cluster.Leader != ""
	}, 30*time.Second, 100*time.Millisecond, "%s elected no leader", stream)
}

// cutover runs a cutover hold that leaves conversions as convert leaves them, completes its journal
// as create-bucket, convert and a clean streams census write it, releases it, and answers its id.
// The hold's cohort record holds B = 1, the boundary the row writes into the bucket's sentinel, as
// consumer-names prepare would have written it: repair-sentinel reads it after a rollback.
func (rig *restoreRig) cutover(conversions ...restoreConversion) string {
	t := rig.t
	t.Helper()
	ctx := context.Background()
	id := rig.heldHold(holdrecord.WorkflowCutover)
	createBucketCohort(t, rig.records(), id, 1)
	nc, js := rig.operator()
	if _, err := streamInfo(ctx, nc, selectionStreamName); err != nil {
		// selection.CreateSessionBucket writes the sentinel at once, which on three copies can
		// find no leader yet (no responders): the row creates the bucket, waits for its leader,
		// then writes the sentinel, as create-bucket's rerun adopts an empty bucket. A named leader
		// is not yet a responder on every server (createBucketFreshWrite), so a write that meets
		// no responder is sent again.
		_, err := js.CreateKeyValue(ctx, selection.SessionBucketConfig(rig.replicas))
		require.NoError(t, err)
		rig.waitLeader(nc, selectionStreamName)
		require.Eventually(t, func() bool {
			_, err = selection.NewKVStore(selection.ConnRequester{Conn: nc}).
				WriteSentinel(ctx, selection.StoreState{Boundary: 1, Hold: id}, 0)
			return !errors.Is(err, nats.ErrNoResponders)
		}, 30*time.Second, 100*time.Millisecond, "the sentinel's write met no responder for 30 s")
		require.NoError(t, err)
	}
	for family, pool := range rig.pools {
		st := pgFamilyOf(t, family, pool)
		if _, err := st.State(ctx); errors.Is(err, selection.ErrNoSentinel) {
			_, err := st.WriteSentinel(ctx, selection.StoreState{}, 0)
			require.NoError(t, err)
		}
	}
	set := convertSet{Complete: true}
	converted := map[string]bool{}
	for _, c := range conversions {
		m, ok := classify(c.fixed)
		require.True(t, ok, "%s is no fixed name", c.fixed)
		info, err := consumerInfo(ctx, nc, notifications, c.fixed)
		require.NoError(t, err)
		require.NotNil(t, info, "the row made no %s", c.fixed)
		fresh := selection.FreshName(m.Prefix)
		record := selection.Record{Start: c.start}
		if c.interrupted {
			record.Op = &selection.Op{Kind: selection.OpReplace, Owner: "the row's convert", Old: c.fixed, Candidate: fresh, ReservedAt: time.Now()}
		} else {
			require.NoError(t, deleteConsumer(ctx, nc, notifications, c.fixed))
			config := info.Config
			config.Name, config.Durable = fresh, fresh
			config.DeliverPolicy, config.OptStartSeq = jetstream.DeliverByStartSequencePolicy, c.start
			config.Metadata = m.Metadata()
			_, err := writeConsumer(ctx, nc, notifications, config, actionCreateConsumer)
			require.NoError(t, err)
			record.Current = fresh
			converted[m.Family] = true
		}
		if c.edit != nil {
			c.edit(&record, fresh)
		}
		if c.deleteFresh {
			require.NoError(t, deleteConsumer(ctx, nc, notifications, fresh))
		}
		_, err = rig.familyStore(nc, m.Family).Write(ctx, m.Logical, record, 0)
		require.NoError(t, err)
		config := info.Config
		set.Names = append(set.Names, convertSetName{Family: m.Family, Logical: m.Logical, Fixed: c.fixed, Config: &config})
	}
	value, err := json.Marshal(set)
	require.NoError(t, err)
	_, err = rig.records().PutRecord(ctx, id, convertSetKind, convertSetKey, 0, value)
	require.NoError(t, err)
	for family := range converted {
		_, err := rig.records().PutCutover(ctx, family, selection.CutoverDone, id)
		require.NoError(t, err)
		rig.mark(nc, family, selection.CutoverDone)
	}
	for _, command := range []string{actionCreateBucket, actionConvert, censusCommandName} {
		_, err := rig.records().Admit(ctx, id, holdrecord.Request{Command: command}, nil)
		require.NoError(t, err)
		require.NoError(t, rig.records().AppendDone(ctx, id, command, nil))
	}
	require.NoError(t, rig.release(id))
	return id
}

// mark writes family's store marker cutover = state, as the operator on nc.
func (rig *restoreRig) mark(nc *nats.Conn, family, state string) {
	rig.t.Helper()
	ctx := context.Background()
	st := rig.familyStore(nc, family)
	current, revision, err := convertSentinel(ctx, st, family)
	require.NoError(rig.t, err)
	current.Cutover = state
	_, err = st.WriteSentinel(ctx, current, revision)
	require.NoError(rig.t, err)
}

// restore runs `consumer-names restore <mode> --hold id` with the rig's store flags.
func (rig *restoreRig) restore(id string, mode ...string) (int, string, string) {
	rig.t.Helper()
	args := append([]string{actionRestore}, mode...)
	args = append(args, "--hold", id)
	return rig.run(append(args, rig.flags...)...)
}

// marker reads family's store marker.
func (rig *restoreRig) marker(family string) selection.StoreState {
	rig.t.Helper()
	nc, _ := rig.operator()
	state, err := rig.familyStore(nc, family).State(context.Background())
	require.NoError(rig.t, err)
	return state
}

// delivered pulls everything consumer name delivers, as the listener, the previous build's reach
// of it, acking each, until a pull of 2 s brings nothing, and answers the stream sequences sorted.
func (rig *restoreRig) delivered(name string) []uint64 {
	rig.t.Helper()
	ctx := context.Background()
	nc, js := rig.listener()
	defer nc.Close()
	consumer, err := js.Consumer(ctx, notifications, name)
	require.NoError(rig.t, err)
	var seqs []uint64
	for {
		batch, err := consumer.Fetch(100, jetstream.FetchMaxWait(2*time.Second))
		require.NoError(rig.t, err)
		got := 0
		for msg := range batch.Messages() {
			meta, err := msg.Metadata()
			require.NoError(rig.t, err)
			require.NoError(rig.t, msg.Ack())
			seqs = append(seqs, meta.Sequence.Stream)
			got++
		}
		if got == 0 {
			slices.Sort(seqs)
			return slices.Compact(seqs)
		}
	}
}

// sessionFamilyOrSkip skips a row of the session family on a server the session consumers refuse
// by design (natsversion: every 2.10 has the filter-scan bug), after it shows the refusal: this
// tree's session consumer calls, connected as the stack's listener entry, refuse to start. A skip is
// no pass. On any other server it returns.
func (rig *restoreRig) sessionFamilyOrSkip() {
	rig.t.Helper()
	ctx := context.Background()
	nc, _ := rig.listener()
	refusal := natsversion.ConnectedRefusal(ctx, nc)
	version := nc.ConnectedServerVersion()
	nc.Close()
	if refusal == nil {
		return
	}
	var o nats.Options
	require.NoError(rig.t, rig.s.ListenerCredential(holdtest.ListenerMachines[0])(&o))
	file := filepath.Join(rig.t.TempDir(), "listener.token")
	require.NoError(rig.t, os.WriteFile(file, []byte(o.Password), 0o600))
	lookup := func(name string) (string, bool) {
		if name == bus.ServiceTokenFileEnvVar {
			return file, true
		}
		return "", false
	}
	sc, err := bus.ConnectSessionConsumers(rig.s.Cluster.URLs(), 0, slog.New(slog.DiscardHandler), bus.WithEnvironment(lookup))
	if err == nil {
		sc.Close()
	}
	require.Error(rig.t, err, "the session consumers start against nats-server %s, which natsversion refuses: %v", version, refusal)
	rig.t.Skipf("the session family refuses nats-server %s by design (%v); the session consumers' connect answered: %v", version, refusal, err)
}

// restoreWithRules sets restore's rules for the rest of the test, the control's earlier design.
func restoreWithRules(t *testing.T, rules restoreRuleSet) {
	t.Helper()
	previous := restoreRules
	restoreRules = rules
	t.Cleanup(func() { restoreRules = previous })
}

// restoreKilledAt makes restore end, as a kill does, at the first of points it reaches for each
// logical name, once each, for the rest of the test, and answers the points it ended at.
func restoreKilledAt(t *testing.T, points map[string]string) *[]string {
	t.Helper()
	var ended []string
	pending := maps.Clone(points)
	restoreHook = func(point, logical string) error {
		if pending[logical] != point {
			return nil
		}
		delete(pending, logical)
		ended = append(ended, logical+"@"+point)
		return errors.New("the row killed restore at " + point + " of " + logical)
	}
	t.Cleanup(func() { restoreHook = nil })
	return &ended
}

// restoreH4Name is one fixed name of the H4 row and the subject its messages are on.
type restoreH4Name struct {
	family, fixed, subject string
}

// restoreH4Project is the H4 row's Legion daemon project.
const restoreH4Project = "p1"

// restoreH4Names are the H4 row's fixed names: a session; Dispatch's two durables, both, since
// convert refuses a Dispatch store that lacks either (§2, "Missing selection state means stop");
// and a daemon's dispatch consumer of project p1.
var restoreH4Names = []restoreH4Name{
	{familySession, sessionFixedPrefix + "rollback-a", "notifications.agent.rollback-a"},
	{familyDispatch, "delivery-events", "notifications.dispatch.events"},
	{familyDispatch, "delivery-jobs", "notifications.dispatch.jobs"},
	{daemonFamily(restoreH4Project), daemonFixedPrefix + restoreH4Project + "-dispatch", "notifications.github." + restoreH4Project},
}

// restoreH4Families are the H4 row's families, in order.
var restoreH4Families = []string{familySession, familyDispatch, daemonFamily(restoreH4Project)}

// restoreH4Rig is the H4 row's state after the cutover: for each fixed name, four messages on its
// subject, the name acked through the second, converted at the third, then two messages more; and
// the rollback hold set. It answers the rig, the rollback hold, and each fixed name's start and the
// messages from it.
func restoreH4Rig(t *testing.T) (*restoreRig, string, map[string]uint64, map[string][]uint64) {
	rig := newRestoreRig(t, familyDispatch, daemonFamily(restoreH4Project))
	starts := map[string]uint64{}
	want := map[string][]uint64{}
	var conversions []restoreConversion
	for _, name := range restoreH4Names {
		seqs := rig.publish(name.subject, 4)
		rig.fixed(restoreFixedConfig(name.fixed, name.subject), 2)
		starts[name.fixed] = seqs[2]
		want[name.fixed] = seqs[2:]
		conversions = append(conversions, restoreConversion{fixed: name.fixed, start: seqs[2]})
	}
	rig.cutover(conversions...)
	for _, name := range restoreH4Names {
		want[name.fixed] = append(want[name.fixed], rig.publish(name.subject, 2)...)
	}
	return rig, rig.heldHold(holdrecord.WorkflowRollback), starts, want
}

// freshCreator is family's creator in the fresh mode over its store, reading as the operator: the
// call that admits or refuses every consumer call (selection.Names.Resolve).
func (rig *restoreRig) freshCreator(family string) *selection.Names {
	nc, _ := rig.operator()
	naming := selection.LogicalNaming()
	if family == familySession {
		naming = selection.SessionNaming()
	}
	return selection.New(selection.Config{Store: rig.familyStore(nc, family), Prefix: naming.Prefix, Fixed: naming.Fixed})
}

// TestCutoverRollbackThenUpgradeAgain: after the rollback every family's broker cutover row and
// store marker read rolled_back, a creator in the fresh mode refuses every consumer call,
// every restored fixed name delivers from its saved start, and a second cutover's convert converts
// the restored fixed names and marks the bucket's sentinel with that cutover's B and hold, as the
// session row names it; after a STREAM.INFO answer is stored over that sentinel, repair-sentinel
// writes it again byte for byte from the broker's records. Control: with the rows and markers left
// at done (restoreRuleSet's noMarkers), a creator in the fresh mode is not refused for the cutover
// (no selection.ErrCutoverNotRun): it reads the restored record, which serves the fixed name, and
// which the selection package now refuses as no record of the key (selection.ErrMalformedRecord: a
// record names the fixed name only as an open operation's old name). (The stream keeps its first
// message, so the second B equals the first, and the hold is what tells the two cutovers'
// sentinels apart.)
func TestCutoverRollbackThenUpgradeAgain(t *testing.T) {
	ctx := context.Background()
	t.Run("control: the rows and markers left at done", func(t *testing.T) {
		rig, rollback, _, _ := restoreH4Rig(t)
		restoreWithRules(t, restoreRuleSet{noMarkers: true})
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		for _, family := range restoreH4Families {
			row, ok := rig.cutoverRow(family)
			require.True(t, ok)
			require.Equal(t, selection.CutoverDone, row.State, "the control left %s's row", family)
			require.Equal(t, selection.CutoverDone, rig.marker(family).Cutover)
		}
		for _, name := range restoreH4Names {
			m, _ := classify(name.fixed)
			_, err := rig.freshCreator(name.family).Resolve(ctx, m.Logical)
			require.NotErrorIs(t, err, selection.ErrCutoverNotRun, "under the control a fresh creator of %s is not refused for the cutover", name.fixed)
			require.ErrorIs(t, err, selection.ErrMalformedRecord, "it reads the restored record of %s", name.fixed)
			require.Equal(t, name.fixed, rig.record(name.family, m.Logical).Current, "which serves the fixed name")
		}
	})
	t.Run("treatment", func(t *testing.T) {
		rig, rollback, starts, want := restoreH4Rig(t)
		code, stdout, stderr := rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		require.Contains(t, stdout, "4 names")
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.True(t, rig.journal(rollback).Complete, "restore --finish's done entry completes the rollback")
		for _, family := range restoreH4Families {
			row, ok := rig.cutoverRow(family)
			require.True(t, ok)
			require.Equal(t, holdrecord.CutoverRow{Family: family, State: selection.CutoverRolledBack, HoldID: rollback, HoldGen: row.HoldGen}, row)
			require.Equal(t, selection.CutoverRolledBack, rig.marker(family).Cutover)
		}
		for _, name := range restoreH4Names {
			m, _ := classify(name.fixed)
			_, err := rig.freshCreator(name.family).Resolve(ctx, m.Logical)
			require.ErrorIs(t, err, selection.ErrCutoverNotRun, "a fresh creator of %s refuses", name.fixed)
			info := rig.consumer(name.fixed)
			require.NotNil(t, info, "%s restored", name.fixed)
			require.Equal(t, jetstream.DeliverByStartSequencePolicy, info.Config.DeliverPolicy)
			require.Equal(t, starts[name.fixed], info.Config.OptStartSeq)
			record := rig.record(name.family, m.Logical)
			require.Equal(t, name.fixed, record.Current)
			require.Equal(t, &selection.Restore{Target: name.fixed, Start: starts[name.fixed], Phase: restoreCreated}, record.Restore)
		}
		require.Equal(t, uint64(1), rig.marker(familySession).Boundary, "the bucket's sentinel keeps B")
		require.NoError(t, rig.release(rollback))
		for _, name := range restoreH4Names {
			require.NotEmpty(t, want[name.fixed])
			require.Equal(t, want[name.fixed], rig.delivered(name.fixed), "%s delivers every message from its start", name.fixed)
		}

		// The upgrade again: the cutover's prepare and convert over the restored fixed names.
		again := rig.prepared(holdrecord.WorkflowCutover)
		code, _, stderr = rig.run(append([]string{actionPrepare, "--hold", again}, rig.flags...)...)
		require.Zero(t, code, stderr)
		rig.held(again)
		code, _, stderr = rig.run(append([]string{actionConvert, "--hold", again}, rig.flags...)...)
		require.Zero(t, code, stderr)
		for _, name := range restoreH4Names {
			m, _ := classify(name.fixed)
			require.Nil(t, rig.consumer(name.fixed), "the second convert deleted %s", name.fixed)
			record := rig.record(name.family, m.Logical)
			require.Contains(t, record.Current, "~", "the second convert converted %s to a fresh name", name.fixed)
		}
		for _, family := range restoreH4Families {
			row, ok := rig.cutoverRow(family)
			require.True(t, ok)
			require.Equal(t, selection.CutoverDone, row.State)
			require.Equal(t, again, row.HoldID)
		}
		cohort, _, found, err := readPrepareCohort(ctx, rig.records(), again)
		require.NoError(t, err)
		require.True(t, found, "the second cutover's prepare wrote no cohort record")
		require.Equal(t, ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: cohort.Boundary, Hold: again}), rig.marker(familySession),
			"the second cutover's sentinel names the cutover that converted it")
		rig.censusDone(again)
		require.NoError(t, rig.release(again))
		converted := repairSentinelValue(t, rig.stack)
		nc, _ := rig.listener()
		repairPlant(t, rig.stack, nc)
		nc.Close()
		repair := rig.heldHold(holdrecord.WorkflowSentinelRepair)
		code, _, stderr = rig.run(repairWord, "--hold", repair)
		require.Zero(t, code, stderr)
		require.Equal(t, string(converted), string(repairSentinelValue(t, rig.stack)), "repair-sentinel reproduces the second cutover's sentinel")
		require.NoError(t, rig.release(repair))
	})
}

// restoreDaemonRig is a rig with daemon project rsa's store, rendered with its Deployment and named
// by a real file (restoreRigOn), whose fixed dispatch consumer a cutover converted, and the rollback
// hold set. It answers the rig, the rollback hold and rsa's logical name.
func restoreDaemonRig(t *testing.T) (*restoreRig, string, string) {
	rig := newRestoreRig(t, daemonFamily("rsa"))
	const fixed, subject = daemonFixedPrefix + "rsa-dispatch", "notifications.github.rsa"
	seqs := rig.publish(subject, 3)
	rig.fixed(restoreFixedConfig(fixed, subject), 1)
	rig.cutover(restoreConversion{fixed: fixed, start: seqs[1]})
	return rig, rig.heldHold(holdrecord.WorkflowRollback), fixed
}

// restore reads each daemon store a --daemon-database file names by the census's rule for the
// daemon stores, at exactly the address the project's one source names, before it writes
// anything: a file that reaches another database than the project's Deployment names makes
// restore --prepare UNKNOWN, naming the file's database and the Deployment's, with no set saved and
// no record touched. Control, in the same hold: with the Deployment naming the store, restore
// --prepare plans the name. And 76d0af0d8f's restore, which opens the file's URL with no rule,
// writes through the file whatever the Deployment names.
func TestRestoreReadsEachDaemonStoreByTheAddressRule(t *testing.T) {
	ctx := context.Background()
	rig, rollback, logical := restoreDaemonRig(t)
	db := rig.pools[daemonFamily("rsa")]
	before := rig.record(daemonFamily("rsa"), logical)
	deployments := rig.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace)
	setDatabase := func(database string) {
		t.Helper()
		rendered, err := deployments.Get(ctx, "legion-daemon-rsa", metav1.GetOptions{})
		require.NoError(t, err)
		rendered.Annotations[writerhold.DaemonStoreDatabaseAnnotation] = database
		_, err = deployments.Update(ctx, rendered, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	setDatabase("daemon_elsewhere")

	code, _, stderr := rig.restore(rollback, "--prepare")
	require.Equal(t, 2, code, "restore --prepare beside a file for another Deployment's database: %s", stderr)
	require.Contains(t, stderr, fmt.Sprintf("--daemon-database rsa=<file> reaches database %q, and Deployment legion/legion-daemon-rsa names database %q",
		db.Config().ConnConfig.Database, "daemon_elsewhere"))
	_, found, err := readRestoreSet(ctx, rig.records(), rollback)
	require.NoError(t, err)
	require.False(t, found, "restore --prepare saved a set beside a store the rule does not read")
	require.Equal(t, before, rig.record(daemonFamily("rsa"), logical), "restore --prepare touched rsa's record")

	setDatabase(db.Config().ConnConfig.Database)
	code, _, stderr = rig.restore(rollback, "--prepare")
	require.Zero(t, code, "control: restore --prepare with the Deployment naming the store: %s", stderr)
	require.NotNil(t, rig.record(daemonFamily("rsa"), logical).Restore, "control: restore --prepare planned rsa's name")
}

// A daemon store the census's rule finds gone (SQLSTATE 3D000 at the address its Deployment names)
// does not block a rollback: restore leaves that family out, writes nothing there, leaves its
// broker cutover row as it is (cleanup --gone removes its consumers), and names it in its output
// and in the done entries of restore --prepare and restore --finish; the other families roll back
// and the hold is released. Control: 76d0af0d8f's restore, which reads the gone store's records
// and stops on its 3D000.
func TestRestoreSkipsAGoneDaemonStore(t *testing.T) {
	ctx := context.Background()
	rig, rollback, logical := restoreDaemonRig(t)
	dropped := censusDroppedDatabase()
	rig.censusDeploy(censusDeployment("GONE", dropped, censusInstance))
	gone := censusStoreFileFor(t, "GONE", rig.pools[daemonFamily("rsa")], dropped, censusInstance)
	rig.flags = append(rig.flags, "--daemon-database", gone)
	// The real env opens the file's URL; the stack's env hands over h.dbs, so it gets a pool on the
	// same URL, which 76d0af0d8f's restore reads.
	raw, err := os.ReadFile(strings.TrimPrefix(gone, "GONE="))
	require.NoError(t, err)
	url, _, _ := strings.Cut(string(raw), "\n")
	gonePool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(gonePool.Close)
	rig.mu.Lock()
	rig.dbs[daemonFamily("GONE")] = gonePool
	rig.mu.Unlock()
	_, err = rig.records().PutCutover(ctx, daemonFamily("GONE"), selection.CutoverDone, rollback)
	require.NoError(t, err)
	goneWords := "restore: " + daemonFamily("GONE") + ": store gone: database " + dropped + " on instance " + censusInstance

	code, stdout, stderr := rig.restore(rollback, "--prepare")
	require.Zero(t, code, "restore --prepare beside a gone store: %s%s", stdout, stderr)
	require.Contains(t, stdout, goneWords)
	require.Contains(t, rig.lastEntry(rollback, restoreCommandPrepare).Outputs["gone."+daemonFamily("GONE")], "database "+dropped)
	code, stdout, stderr = rig.restore(rollback, "--finish")
	require.Zero(t, code, "restore --finish beside a gone store: %s%s", stdout, stderr)
	require.Contains(t, stdout, goneWords)
	require.Contains(t, rig.lastEntry(rollback, restoreCommandFinish).Outputs["gone."+daemonFamily("GONE")], "database "+dropped)
	row, ok := rig.cutoverRow(daemonFamily("GONE"))
	require.True(t, ok)
	require.Equal(t, holdrecord.CutoverRow{Family: daemonFamily("GONE"), State: selection.CutoverDone, HoldID: rollback, HoldGen: row.HoldGen}, row, "restore left the gone family's cutover row as it was")
	live, ok := rig.cutoverRow(daemonFamily("rsa"))
	require.True(t, ok)
	require.Equal(t, selection.CutoverRolledBack, live.State, "the live daemon family rolled back")
	require.Equal(t, selection.CutoverRolledBack, rig.marker(daemonFamily("rsa")).Cutover)
	require.Equal(t, logical, rig.record(daemonFamily("rsa"), logical).Current, "rsa's fixed name is restored")
	require.NoError(t, rig.release(rollback))
}

// The operator's readers' record fence (checkRecordFence) passes a record restore --finish left,
// and only that shape of a fixed current: its current equal to its restore target, which is the
// key's fixed name, at phase created, judged with its current cleared, so its other names still
// meet the creators' fence. A fixed current with no restore field, one at another phase, a target
// that is another key's fixed name, and a restored record whose retired name is another family's
// are each refused; and the creators' own fence (selection.CheckFamily) still refuses the restored
// record. Control: 3cef381b26's fence, the creators' alone, refuses the restored record too.
func TestTheReadersFencePassesOnlyARestoredRecord(t *testing.T) {
	const session, fixed, fresh = "ses_r", "s-ses_r", "session~RestoredRetiredAAAA"
	restored := func(phase string) *selection.Restore {
		return &selection.Restore{Target: fixed, Start: 3, Phase: phase}
	}
	record := selection.Record{Current: fixed, Start: 3, Retired: []string{fresh}, Restore: restored(restoreCreated)}
	require.NoError(t, checkRecordFence(familySession, session, record), "a restored session record")
	require.ErrorIs(t, selection.CheckFamily(familySession, session, record), selection.ErrMalformedRecord, "the creators' fence refuses the restored record")
	const daemon = "legion-go-p-wake"
	require.NoError(t, checkRecordFence(daemonFamily("p"), daemon,
		selection.Record{Current: daemon, Start: 3, Restore: &selection.Restore{Target: daemon, Start: 3, Phase: restoreCreated}}), "a restored daemon record")
	for name, refused := range map[string]selection.Record{
		"a fixed current with no restore field":            {Current: fixed, Start: 3},
		"a fixed current at phase kept":                    {Current: fixed, Start: 3, Restore: restored(restoreKept)},
		"a fixed current at phase source_deleted":          {Current: fixed, Start: 3, Restore: restored(restoreSourceDeleted)},
		"a restore target of another key's fixed name":     {Current: "s-ses_other", Start: 3, Restore: &selection.Restore{Target: "s-ses_other", Start: 3, Phase: restoreCreated}},
		"a restored record retiring another family's name": {Current: fixed, Start: 3, Retired: []string{"legion-go-p-dispatch"}, Restore: restored(restoreCreated)},
	} {
		require.ErrorIs(t, checkRecordFence(familySession, session, refused), selection.ErrMalformedRecord, name)
	}
}

// After a rollback, the operator's readers take the records restore --finish left, each selecting
// its fixed name with the fresh names the rollback retired: status lists no record its family's
// creator refuses, the census's reader of the recorded names reads those families, and plain
// cleanup deletes the retired fresh names and drops them from each record, keeping its current and
// its fixed consumer. Control: 3cef381b26's readers, which judge each restored record by the
// creators' fence alone, so status lists each as refused, the census's reader fails, and cleanup
// leaves the retired names for good.
func TestReadersTakeTheRecordsARollbackLeft(t *testing.T) {
	ctx := context.Background()
	rig := newRestoreRig(t, familyDispatch, daemonFamily("rsf"))
	names := map[string]string{sessionFixedPrefix + "rolled": "notifications.agent.rolled", daemonFixedPrefix + "rsf-dispatch": "notifications.github.rsf"}
	var conversions []restoreConversion
	for name, subject := range names {
		seqs := rig.publish(subject, 3)
		rig.fixed(restoreFixedConfig(name, subject), 1)
		conversions = append(conversions, restoreConversion{fixed: name, start: seqs[1]})
	}
	rig.cutover(conversions...)
	rollback := rig.heldHold(holdrecord.WorkflowRollback)
	for _, mode := range []string{"--prepare", "--finish"} {
		code, _, stderr := rig.restore(rollback, mode)
		require.Zero(t, code, "restore %s: %s", mode, stderr)
	}
	require.NoError(t, rig.release(rollback))
	retired := map[string][]string{}
	for name := range names {
		m, _ := classify(name)
		record := rig.record(m.Family, m.Logical)
		require.Equal(t, name, record.Current, "restore --finish selects %s", name)
		require.NotEmpty(t, record.Retired, "the rollback retired %s's fresh name", name)
		retired[name] = record.Retired
	}

	code, stdout, stderr := rig.run(append([]string{actionStatus}, rig.flags...)...)
	require.Zero(t, code, "status after the rollback: %s", stderr)
	require.NotContains(t, stdout, "its family's creator refuses the record", "status lists a restored record as refused")

	nc, _ := rig.operator()
	_, _, err := ReadRecordedNames(ctx, nc, writerhold.KubeDaemonDeployments(rig.s.Kube.Client), []string{familySession, daemonFamily("rsf")}, "",
		[]string{"rsf=" + rig.enrollFile(rig.pools[daemonFamily("rsf")])})
	require.NoError(t, err, "the census's reader of the recorded names after the rollback")

	cleanup := rig.heldHold(holdrecord.WorkflowConsumerCleanup)
	code, stdout, stderr = rig.run(append([]string{actionCleanup, "--hold", cleanup, "--final"}, rig.flags...)...)
	require.Zero(t, code, "cleanup after the rollback: %s%s", stdout, stderr)
	for name := range names {
		m, _ := classify(name)
		record := rig.record(m.Family, m.Logical)
		require.Empty(t, record.Retired, "cleanup left %s's retired names", name)
		require.Equal(t, name, record.Current, "cleanup changed %s's current", name)
		require.NotNil(t, rig.consumer(name), "cleanup deleted the fixed consumer %s", name)
		for _, fresh := range retired[name] {
			require.Nil(t, rig.consumer(fresh), "the retired %s is on the stream", fresh)
		}
	}
	require.NoError(t, rig.release(cleanup))
}

// The H5 row's sessions: one whose target expires after restore --finish created it, and one whose
// target round 17's prepare kept at a floor past its start. Each sorts in this order, the order
// restore --finish creates them in.
const (
	restoreH5Expires = "a-expires"
	restoreH5Kept    = "b-kept"
)

// restoreH5Rig is the H5 row's state with restore --prepare run: a-expires converted at its third
// message, its fixed name deleted; b-kept's convert interrupted with its start 1 saved and its
// fixed name at floor 20; five messages more on each; restore --prepare run with round 17's phase
// kept, so b-kept's target stays at floor 20 with its record at phase kept, the one way a kept
// target fails its start check at restore --finish with no ack inside the hold. Then restore
// --finish ended as a kill does after it created a-expires' target, before b-kept's.
func restoreH5Rig(t *testing.T) (*restoreRig, string, map[string][]uint64, uint64) {
	rig := newRestoreRig(t)
	expires := "notifications.agent." + restoreH5Expires
	kept := "notifications.agent." + restoreH5Kept
	keptSeqs := rig.publish(kept, 2)
	keptSeqs = append(keptSeqs, rig.publish("notifications.agent.other", 18)...)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+restoreH5Kept, kept, "notifications.agent.other"), 20)
	expiresSeqs := rig.publish(expires, 4)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+restoreH5Expires, expires), 2)
	rig.cutover(
		restoreConversion{fixed: sessionFixedPrefix + restoreH5Expires, start: expiresSeqs[2]},
		restoreConversion{fixed: sessionFixedPrefix + restoreH5Kept, start: 1, interrupted: true},
	)
	want := map[string][]uint64{
		restoreH5Expires: append(slices.Clone(expiresSeqs[2:]), rig.publish(expires, 5)...),
		restoreH5Kept:    append(slices.Clone(keptSeqs[:2]), rig.publish(kept, 5)...),
	}
	rollback := rig.heldHold(holdrecord.WorkflowRollback)
	previous := restoreRules
	restoreRules = restoreRuleSet{keepAnyTarget: true}
	code, _, stderr := rig.restore(rollback, "--prepare")
	restoreRules = previous
	require.Zero(t, code, stderr)
	require.Equal(t, restoreKept, rig.record(familySession, restoreH5Kept).Restore.Phase, "round 17's prepare kept b-kept's target")
	require.Equal(t, uint64(20), rig.consumer(sessionFixedPrefix+restoreH5Kept).AckFloor.Stream)

	ended := restoreKilledAt(t, map[string]string{restoreH5Expires: restoreCreated})
	code, _, _ = rig.restore(rollback, "--finish")
	require.NotZero(t, code)
	require.Equal(t, []string{restoreH5Expires + "@" + restoreCreated}, *ended, "the hold point: restore --finish ended after it created a-expires' target")
	restoreHook = nil
	require.Equal(t, holdrecord.Operation{Command: restoreCommandFinish, State: "intent"}, rig.lastEntry(rollback, restoreCommandFinish),
		"the killed finish leaves its intent pending")
	target := sessionFixedPrefix + restoreH5Expires
	info := rig.consumer(target)
	require.NotNil(t, info, "restore --finish created %s before it was killed", target)

	// F's inactivity threshold shortened in the test, by the operator's update, and F removed for
	// inactivity (consumer.go 2234-2275).
	nc, _ := rig.operator()
	config := info.Config
	config.InactiveThreshold = time.Second
	_, err := writeConsumer(context.Background(), nc, notifications, config, actionUpdateConsumer)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rig.consumer(target) == nil }, time.Minute, 200*time.Millisecond,
		"%s was never removed for inactivity", target)
	return rig, rollback, want, expiresSeqs[2]
}

// TestTheRestoreTargetExpiresBetweenFinishAndItsResume: restore --finish creates F, is killed
// before it completes the hold, and F is removed for inactivity. The resumed finish finds F
// absent, runs the restart again, creates F at S, and every message from S arrives on F, which
// the previous release pulls by name. A kept F that fails its start check is replaced at S.
// Control: round 17's rule skips the created record and completes with F missing.
func TestTheRestoreTargetExpiresBetweenFinishAndItsResume(t *testing.T) {
	t.Run("control: round 17's created record is final", func(t *testing.T) {
		rig, rollback, _, _ := restoreH5Rig(t)
		restoreWithRules(t, restoreRuleSet{createdIsFinal: true})
		code, _, stderr := rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.True(t, rig.journal(rollback).Complete)
		require.Nil(t, rig.consumer(sessionFixedPrefix+restoreH5Expires), "round 17's finish completes the hold with F missing")
	})
	t.Run("treatment", func(t *testing.T) {
		rig, rollback, want, start := restoreH5Rig(t)
		code, stdout, stderr := rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.True(t, rig.journal(rollback).Complete)
		for _, name := range []struct {
			logical string
			start   uint64
		}{{restoreH5Expires, start}, {restoreH5Kept, 1}} {
			info := rig.consumer(sessionFixedPrefix + name.logical)
			require.NotNil(t, info, "%s's target is present at its start", name.logical)
			require.Equal(t, name.start, info.Config.OptStartSeq)
			require.Equal(t, &selection.Restore{Target: sessionFixedPrefix + name.logical, Start: name.start, Phase: restoreCreated},
				rig.record(familySession, name.logical).Restore)
		}
		require.Contains(t, stdout, "2 targets created", "the resumed finish created the expired target and the replaced kept one")
		require.NoError(t, rig.release(rollback))
		for _, logical := range []string{restoreH5Expires, restoreH5Kept} {
			require.NotEmpty(t, want[logical])
			got := rig.delivered(sessionFixedPrefix + logical)
			for _, seq := range want[logical] {
				require.Contains(t, got, seq, "%s's target delivers %d", logical, seq)
			}
		}
	})
}

// restoreStopRig is the prepare stop row's state: Dispatch's two durables, each acked through the
// first of its three messages and converted at its second, and the rollback hold set.
func restoreStopRig(t *testing.T) (*restoreRig, string) {
	rig := newRestoreRig(t, familyDispatch)
	var conversions []restoreConversion
	for _, name := range []struct{ fixed, subject string }{
		{"delivery-events", "notifications.dispatch.events"},
		{"delivery-jobs", "notifications.dispatch.jobs"},
	} {
		seqs := rig.publish(name.subject, 3)
		rig.fixed(restoreFixedConfig(name.fixed, name.subject), 1)
		conversions = append(conversions, restoreConversion{fixed: name.fixed, start: seqs[1]})
	}
	rig.cutover(conversions...)
	return rig, rig.heldHold(holdrecord.WorkflowRollback)
}

// restoreBreakStoreAt makes restore's write after its deletes of logical's record find no
// intake_consumers table, once: at that point (restoreAt "deleted") Dispatch's table is renamed
// away, as a store moved under the command. restore answers it as a missing store
// (selection.ErrNoStore), a refusal. It answers the function that puts the table back, which
// first asserts the hold point was reached.
func (rig *restoreRig) restoreBreakStoreAt(logical string) func() {
	pool := rig.pools[familyDispatch]
	broken := false
	restoreHook = func(point, at string) error {
		if point == "deleted" && at == logical && !broken {
			broken = true
			_, err := pool.Exec(context.Background(), `alter table intake_consumers rename to intake_consumers_away`)
			require.NoError(rig.t, err)
		}
		return nil
	}
	rig.t.Cleanup(func() { restoreHook = nil })
	return func() {
		rig.t.Helper()
		require.True(rig.t, broken, "the hold point: restore reached its write after the deletes of %s", logical)
		_, err := pool.Exec(context.Background(), `alter table intake_consumers_away rename to intake_consumers`)
		require.NoError(rig.t, err)
	}
}

// TestARestorePrepareRefusalAfterItsIntentWritesAStopNamingWhatItLeft: restore --prepare, its
// intent written, deletes delivery-events' fresh current and then finds its store gone. Its stop
// entry names the record it left at planned and the consumer it deleted; the stop ends its intent,
// so hold release is refused only because the record is not complete, never as busy; and once the
// store is back its rerun is admitted and goes on from the planned record, restore --finish
// completes, and hold release succeeds. Control: the same failure with no stop entry
// (restoreRuleSet's noStopEntry) leaves its intent pending, and hold release is refused as busy.
func TestARestorePrepareRefusalAfterItsIntentWritesAStopNamingWhatItLeft(t *testing.T) {
	t.Run("control: no stop entry", func(t *testing.T) {
		rig, rollback := restoreStopRig(t)
		restoreWithRules(t, restoreRuleSet{noStopEntry: true})
		putBack := rig.restoreBreakStoreAt("delivery-events")
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Equal(t, 1, code, stderr)
		putBack()
		require.Equal(t, "intent", rig.lastEntry(rollback, restoreCommandPrepare).State, "with no stop entry the intent stays pending")
		require.ErrorIs(t, rig.release(rollback), holdrecord.ErrBusy, "hold release is refused as busy")
	})
	t.Run("treatment", func(t *testing.T) {
		requireStoppable(t, holdrecord.WorkflowRollback, restoreCommandPrepare)
		rig, rollback := restoreStopRig(t)
		fresh := rig.record(familyDispatch, "delivery-events").Current
		require.Contains(t, fresh, "~", "the hold point: delivery-events is converted to a fresh name")
		putBack := rig.restoreBreakStoreAt("delivery-events")
		code, _, stderr := rig.restore(rollback, "--prepare")
		require.Equal(t, 1, code, stderr)
		putBack()
		require.Contains(t, stderr, "the selection store is missing")
		require.Contains(t, stderr, "records at planned: dispatch/delivery-events", "the refusal names what it left")
		stop := rig.lastEntry(rollback, restoreCommandPrepare)
		require.Equal(t, "stop", stop.State, "the refusal after the intent writes its stop entry")
		reason := stop.Outputs["reason"]
		require.Contains(t, reason, "the selection store is missing")
		require.Contains(t, reason, "records at planned: dispatch/delivery-events")
		require.Contains(t, reason, "consumers deleted or found gone: "+fresh)
		require.Equal(t, restorePlanned, rig.record(familyDispatch, "delivery-events").Restore.Phase)
		require.Nil(t, rig.consumer(fresh), "the consumer the stop names is gone")
		err := rig.release(rollback)
		require.ErrorIs(t, err, holdrecord.ErrOutOfOrder, "the stop ended the intent: hold release is refused only because the record is not complete")
		require.NotErrorIs(t, err, holdrecord.ErrBusy)

		code, _, stderr = rig.restore(rollback, "--prepare")
		require.Zero(t, code, stderr)
		require.Equal(t, restoreSourceDeleted, rig.record(familyDispatch, "delivery-events").Restore.Phase, "the rerun went on from planned")
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.NoError(t, rig.release(rollback))
	})
}

// restoreKeyStopRig is the key stop row's state: session keyed acked through the first of its
// three messages, converted at its second, the rollback hold set, and restore --prepare run, so
// the name has its start. It answers the rig, the hold and that start.
func restoreKeyStopRig(t *testing.T) (*restoreRig, string, uint64) {
	rig := newRestoreRig(t)
	seqs := rig.publish("notifications.agent.keyed", 3)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"keyed", "notifications.agent.keyed"), 1)
	rig.cutover(restoreConversion{fixed: sessionFixedPrefix + "keyed", start: seqs[1]})
	rollback := rig.heldHold(holdrecord.WorkflowRollback)
	code, _, stderr := rig.restore(rollback, "--prepare")
	require.Zero(t, code, stderr)
	restore := rig.record(familySession, "keyed").Restore
	require.NotNil(t, restore)
	require.NotZero(t, restore.Start, "the hold point: keyed has its start")
	return rig, rollback, restore.Start
}

// TestARestoreKeyRefusalAfterItsIntentWritesAStopSoFinishRuns: restore --key for a name that has
// its start already refuses after its intent, and its stop entry says the run changed nothing, so
// restore --finish is admitted and completes the hold, and hold release succeeds. Control: the
// same refusal with no stop entry (noStopEntry) leaves restore --key's intent pending: restore
// --finish and hold release are refused as busy, and the hold is stranded.
func TestARestoreKeyRefusalAfterItsIntentWritesAStopSoFinishRuns(t *testing.T) {
	t.Run("control: no stop entry", func(t *testing.T) {
		rig, rollback, start := restoreKeyStopRig(t)
		restoreWithRules(t, restoreRuleSet{noStopEntry: true})
		code, _, stderr := rig.restore(rollback, "--key", "keyed", "--from", formatSeq(start+7))
		require.Equal(t, 1, code, stderr)
		require.Equal(t, "intent", rig.lastEntry(rollback, restoreCommandKey).State, "with no stop entry the intent stays pending")
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "another command's intent is pending")
		require.ErrorIs(t, rig.release(rollback), holdrecord.ErrBusy, "hold release is refused: the hold is stranded")
	})
	t.Run("treatment", func(t *testing.T) {
		requireStoppable(t, holdrecord.WorkflowRollback, restoreCommandKey)
		rig, rollback, start := restoreKeyStopRig(t)
		wrong := formatSeq(start + 7)
		code, _, stderr := rig.restore(rollback, "--key", "keyed", "--from", wrong)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "not at --from "+wrong)
		stop := rig.lastEntry(rollback, restoreCommandKey)
		require.Equal(t, "stop", stop.State, "the refusal after the intent writes its stop entry")
		require.Contains(t, stop.Outputs["reason"], "not at --from "+wrong)
		require.Contains(t, stop.Outputs["reason"], "this run changed no selection record, consumer, cutover row or store marker")
		code, _, stderr = rig.restore(rollback, "--finish")
		require.Zero(t, code, stderr)
		require.True(t, rig.journal(rollback).Complete)
		require.Equal(t, start, rig.consumer(sessionFixedPrefix+"keyed").Config.OptStartSeq)
		require.NoError(t, rig.release(rollback))
	})
}

// TestRestoreJournalLeftCountsEveryRunOfTheCommand: what a restore command's stop records as left
// (holdrecord's AppendStopLeft, through admission.left's journalLeft over the run's restoreLeft)
// is unchanged only when no run of the command in this hold changed anything outside the journal,
// earlier runs cut after their intent included, and this hold's barrier key a run put counts as a
// change. A restore --prepare run cut before it saved the restore set counts too: it puts the
// barrier key before the save. Its control row is a rerun after a cut run that changed nothing in
// this run: recording unchanged there would let hold release end a hold whose records the cut run
// changed.
func TestRestoreJournalLeftCountsEveryRunOfTheCommand(t *testing.T) {
	intent := func(command string) holdrecord.Operation {
		return holdrecord.Operation{Command: command, State: "intent"}
	}
	stop := func(command, left string) holdrecord.Operation {
		outputs := map[string]string{"reason": "refused"}
		if left != "" {
			outputs[stopLeftKey] = left
		}
		return holdrecord.Operation{Command: command, State: "stop", Outputs: outputs}
	}
	done := func(command string) holdrecord.Operation {
		return holdrecord.Operation{Command: command, State: "done"}
	}
	journal := func(ops ...holdrecord.Operation) holdrecord.Done {
		set := []holdrecord.Operation{intent(holdrecord.CommandSet), done(holdrecord.CommandSet)}
		return holdrecord.Done{Workflow: holdrecord.WorkflowRollback, Ops: append(set, ops...)}
	}
	deleted := &restoreLeft{deleted: []string{"session~A"}}
	for _, row := range []struct {
		name      string
		command   string
		d         holdrecord.Done
		left      *restoreLeft
		unchanged bool
		names     []string
	}{
		{name: "control: a rerun after a run cut after its intent", command: restoreCommandKey, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandKey), intent(restoreCommandKey)), names: []string{"run 1 ended with no entry"}},
		{name: "the first run, which changed nothing", command: restoreCommandKey, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandKey)), unchanged: true},
		{name: "the first run, which deleted a consumer", command: restoreCommandKey, left: deleted,
			d: journal(done(restoreCommandPrepare), intent(restoreCommandKey)), names: []string{"consumers deleted or found gone: session~A"}},
		{name: "the first run, which put the barrier key", command: restoreCommandPrepare, left: &restoreLeft{barrier: true},
			d: journal(intent(restoreCommandPrepare)), names: []string{restoreNoBarrier}},
		{name: "a rerun after a stop that left nothing changed", command: restoreCommandKey, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandKey), stop(restoreCommandKey, holdrecord.LeftUnchanged), intent(restoreCommandKey)), unchanged: true},
		{name: "a rerun after a done run", command: restoreCommandKey, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandKey), done(restoreCommandKey), intent(restoreCommandKey)), names: []string{"run 1 is done"}},
		{name: "a rerun after a stop that recorded nothing of what it left", command: restoreCommandFinish, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandFinish), stop(restoreCommandFinish, ""), intent(restoreCommandFinish)), names: []string{"run 1 stopped, and its stop recorded nothing of what it left"}},
		{name: "a rerun after a stop that left changes", command: restoreCommandFinish, left: &restoreLeft{},
			d: journal(done(restoreCommandPrepare), intent(restoreCommandFinish), stop(restoreCommandFinish, "this run left targets created: s-a"), intent(restoreCommandFinish)), names: []string{"run 1 stopped, and its stop entry records what it left"}},
		{name: "prepare after a cut run, which may have put the barrier key before any set was saved", command: restoreCommandPrepare, left: &restoreLeft{},
			d: journal(intent(restoreCommandPrepare), intent(restoreCommandPrepare)), names: []string{"run 1 ended with no entry"}},
		{name: "other commands' entries are not this command's runs", command: restoreCommandKey, left: &restoreLeft{},
			d: journal(intent(restoreCommandPrepare), intent(restoreCommandPrepare), done(restoreCommandPrepare), intent(restoreCommandKey)), unchanged: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := journalLeft(row.d, row.command, row.left.empty(), row.left.String())
			if row.unchanged {
				require.Equal(t, holdrecord.LeftUnchanged, got)
				return
			}
			require.NotEqual(t, holdrecord.LeftUnchanged, got)
			require.NotEmpty(t, got, "AppendStopLeft refuses an empty left")
			for _, name := range row.names {
				require.Contains(t, got, name)
			}
		})
	}
}

// restoreNoBarrier is a restore run's own account when it changed nothing but put this hold's
// barrier key (restoreLeft.String).
const restoreNoBarrier = "this run changed no selection record, consumer, cutover row or store marker; it put " + barrierLeft

// A restore --prepare stop counts this hold's barrier key, which the run put before it listed the
// selection bucket's records, before it saved the restore set: a stop names every change its
// command's runs made and did not undo, and the key comes before the save. A malformed value in the
// bucket makes --prepare UNKNOWN after its barrier, saving no set and writing no record; its stop
// names the key, and hold release is refused over it. The refusal is UNKNOWN, so its stop records
// what it left as unknown (leftUnknownPrefix) whatever the key's rule. Controls: under the earlier
// rules that the key is no change (leftRuleSet.barrierNoChange) and an UNKNOWN's stop records its
// run's account (leftRuleSet.unknownAsAccount), the same refusal records unchanged, and hold release
// is admitted over it; under the first of them alone it records unknown, and hold release is
// refused.
func TestARestorePrepareStopNamesTheBarrierKeyItPut(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowRollback, restoreCommandPrepare)
	rig := newRestoreRig(t)
	seqs := rig.publish("notifications.agent.keyed", 3)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"keyed", "notifications.agent.keyed"), 1)
	rig.cutover(restoreConversion{fixed: sessionFixedPrefix + "keyed", start: seqs[1]})
	rollback := rig.heldHold(holdrecord.WorkflowRollback)
	_, js := rig.operator()
	_, err := js.Publish(context.Background(), "$KV."+selection.SessionBucket+".ses_bad", []byte("no record"))
	require.NoError(t, err)
	release := holdrecord.Request{Command: holdrecord.CommandRelease}
	refused := func() holdrecord.Operation {
		t.Helper()
		code, stdout, stderr := rig.restore(rollback, "--prepare")
		require.Equal(t, 2, code, "restore --prepare on a malformed value: %s%s", stdout, stderr)
		require.Contains(t, stderr, "ses_bad", "the hold point: the refusal follows the listing behind the barrier")
		_, saved, err := readRestoreSet(context.Background(), rig.records(), rollback)
		require.NoError(t, err)
		require.False(t, saved, "the hold point: the refused run saved no restore set")
		stop := rig.lastEntry(rollback, restoreCommandPrepare)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{barrierNoChange: true, unknownAsAccount: true})
	require.Equal(t, holdrecord.LeftUnchanged, refused().Outputs["left"], "control: with the key no change and an UNKNOWN as its account, the stop records unchanged")
	require.NoError(t, holdrecord.Decide(rig.journal(rollback), release), "control: hold release is admitted over the unchanged stop")

	leftWith(t, leftRuleSet{barrierNoChange: true})
	require.True(t, strings.HasPrefix(refused().Outputs["left"], leftUnknownPrefix), "with the key no change, the UNKNOWN's stop records unknown")
	require.ErrorIs(t, holdrecord.Decide(rig.journal(rollback), release), holdrecord.ErrOutOfOrder, "hold release over the UNKNOWN's stop")

	leftWith(t, leftRuleSet{})
	require.Contains(t, refused().Outputs["left"], restoreNoBarrier)
	require.ErrorIs(t, holdrecord.Decide(rig.journal(rollback), release), holdrecord.ErrOutOfOrder, "hold release over a stop that left the key")
}

// A restore --key stop counts this hold's barrier key, which the run put before it read the
// name's record: a stop names every change its command's runs made and did not undo. --key for a
// name that has its start already refuses after its barrier, writing nothing; its stop names the
// key. Control: under the earlier rule that the key is no change (leftRuleSet.barrierNoChange), the
// same refusal records unchanged.
func TestARestoreKeyStopNamesTheBarrierKeyItPut(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowRollback, restoreCommandKey)
	rig, rollback, start := restoreKeyStopRig(t)
	wrong := formatSeq(start + 7)
	refused := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := rig.restore(rollback, "--key", "keyed", "--from", wrong)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "not at --from "+wrong, "the hold point: the refusal follows the read behind the barrier")
		stop := rig.lastEntry(rollback, restoreCommandKey)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{barrierNoChange: true})
	require.Equal(t, holdrecord.LeftUnchanged, refused().Outputs["left"], "control: with the key no change the stop records unchanged")

	leftWith(t, leftRuleSet{})
	require.Contains(t, refused().Outputs["left"], restoreNoBarrier)
}

// A restore --finish stop counts this hold's barrier key, which the run put before it read each
// record's start: a stop names every change its command's runs made and did not undo. Session
// nostart, converted and its current deleted with no start saved, is left at no_start by --prepare,
// so --finish refuses after its barrier, restarting and creating nothing; its stop names the key.
// Control: under the earlier rule that the key is no change (leftRuleSet.barrierNoChange), the same
// refusal records unchanged.
func TestARestoreFinishStopNamesTheBarrierKeyItPut(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowRollback, restoreCommandFinish)
	rig := newRestoreRig(t)
	rig.fixed(restoreFixedConfig(sessionFixedPrefix+"nostart", "notifications.agent.nostart"), 0)
	rig.cutover(restoreConversion{fixed: sessionFixedPrefix + "nostart", start: 1, deleteFresh: true,
		edit: func(record *selection.Record, _ string) { record.Start = 0 }})
	rollback := rig.heldHold(holdrecord.WorkflowRollback)
	code, stdout, stderr := rig.restore(rollback, "--prepare")
	require.Zero(t, code, stderr)
	require.Contains(t, stdout, "session/nostart has no start", "the hold point: --prepare left nostart with no start")
	refused := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := rig.restore(rollback, "--finish")
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "these names have no settled start", "the hold point: the refusal follows the reads behind the barrier")
		stop := rig.lastEntry(rollback, restoreCommandFinish)
		require.Equal(t, "stop", stop.State)
		return stop
	}

	leftWith(t, leftRuleSet{barrierNoChange: true})
	require.Equal(t, holdrecord.LeftUnchanged, refused().Outputs["left"], "control: with the key no change the stop records unchanged")

	leftWith(t, leftRuleSet{})
	require.Contains(t, refused().Outputs["left"], restoreNoBarrier)
}
