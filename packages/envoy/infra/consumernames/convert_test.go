package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/sjawhar/envoy/natsgrant"
)

// convertRig is a stack whose notification stream the operator made under a streams-update hold
// of its own, released with nothing else changed, and a writer of today's build: a listener
// entry's connection, which publishes, makes fixed consumers, pulls and acknowledges while the
// row is released or prepared, as every creator before the cutover does.
type convertRig struct {
	*stack
	writerConn *nats.Conn
	writer     jetstream.JetStream
}

// convertRigStreamID is the rig's notification stream's identity (selection.StreamIdentityMetadataKey).
const convertRigStreamID = "the-rig-notification-stream"

// newConvertRig makes the rig, the stream's max_consumers maxConsumers when above 0.
func newConvertRig(t *testing.T, maxConsumers int) *convertRig {
	t.Helper()
	return newConvertRigWith(t, func(config *jetstream.StreamConfig) {
		if maxConsumers > 0 {
			config.MaxConsumers = maxConsumers
		}
	})
}

// newConvertRigWith makes the rig, its stream's configuration the build's at one copy as change
// leaves it.
func newConvertRigWith(t *testing.T, change func(*jetstream.StreamConfig)) *convertRig {
	t.Helper()
	h := newStack(t)
	id := h.heldHold(holdrecord.WorkflowStreamsUpdate)
	nc, js := h.operator()
	config := notificationStreamConfig(t, 1)
	// The stream's identity, as the listener writes it when it creates the stream.
	config.Metadata = map[string]string{selection.StreamIdentityMetadataKey: convertRigStreamID}
	change(&config)
	_, err := js.CreateStream(context.Background(), config)
	require.NoError(t, err)
	nc.Close()
	require.NoError(t, h.release(id))
	rig := &convertRig{stack: h}
	rig.connectWriter()
	return rig
}

// connectWriter connects the writer again: a connection decided before a hold keeps its grant
// until its server's restart, and the release admits it again.
func (rig *convertRig) connectWriter() {
	rig.t.Helper()
	nc, err := rig.s.Connect(rig.s.Cluster.URL(), rig.s.ListenerCredential(holdtest.ListenerMachines[0]),
		nats.Name("envoy-listener of today's build"), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	require.NoError(rig.t, err)
	js, err := jetstream.New(nc)
	require.NoError(rig.t, err)
	rig.writerConn, rig.writer = nc, js
}

// closeWriter closes the writer's connection, as hold set's restart closes every writer's and the
// held row refuses its reconnect: h.held skips that restart, and a held command's re-read refuses
// a connection left open across it (heldReread's report).
func (rig *convertRig) closeWriter() {
	if rig.writerConn != nil {
		rig.writerConn.Close()
	}
}

// publish stores n messages on subject and answers their sequences.
func (rig *convertRig) publish(subject string, n int) []uint64 {
	rig.t.Helper()
	var seqs []uint64
	for i := range n {
		ack, err := rig.writer.Publish(context.Background(), subject, []byte(fmt.Sprintf("%s #%d", subject, i+1)))
		require.NoError(rig.t, err)
		seqs = append(seqs, ack.Sequence)
	}
	return seqs
}

// fixed makes the fixed consumer name on filter from the stream's first message, as today's build
// makes one, with threshold its inactive threshold (0: none).
func (rig *convertRig) fixed(name, filter string, threshold time.Duration) {
	rig.t.Helper()
	_, err := rig.writer.CreateConsumer(context.Background(), notifications, jetstream.ConsumerConfig{
		Durable: name, FilterSubject: filter, AckPolicy: jetstream.AckExplicitPolicy, AckWait: time.Minute,
		DeliverPolicy: jetstream.DeliverAllPolicy, InactiveThreshold: threshold, MaxDeliver: -1,
	})
	require.NoError(rig.t, err, "make %s", name)
}

// consume pulls name's next n messages and acknowledges those at or below upTo, waiting for each
// acknowledgement, and answers the stream sequences delivered.
func (rig *convertRig) consume(name string, n int, upTo uint64) []uint64 {
	rig.t.Helper()
	ctx := context.Background()
	consumer, err := rig.writer.Consumer(ctx, notifications, name)
	require.NoError(rig.t, err, "the consumer %s", name)
	batch, err := consumer.Fetch(n, jetstream.FetchMaxWait(5*time.Second))
	require.NoError(rig.t, err)
	var seqs []uint64
	for msg := range batch.Messages() {
		meta, err := msg.Metadata()
		require.NoError(rig.t, err)
		seqs = append(seqs, meta.Sequence.Stream)
		if meta.Sequence.Stream <= upTo {
			require.NoError(rig.t, msg.DoubleAck(ctx))
		}
	}
	return seqs
}

// heldForConvert runs a cutover hold to where convert runs: hold prepare's row and journal,
// consumer-names prepare, the move to held as hold set writes it (without its steps on the hub),
// and the selection bucket with its sentinel holding the cohort's B, as create-bucket writes it.
// It answers the hold id and the cohort.
func (rig *convertRig) heldForConvert() (string, prepareCohort) {
	rig.t.Helper()
	ctx := context.Background()
	id := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(rig.t, code, stderr)
	rig.closeWriter()
	rig.held(id)
	cohort, _, found, err := readPrepareCohort(ctx, rig.records(), id)
	require.NoError(rig.t, err)
	require.True(rig.t, found, "prepare wrote no cohort record")
	nc, _ := rig.operator()
	_, err = selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Boundary: cohort.Boundary})
	require.NoError(rig.t, err)
	nc.Close()
	return id, cohort
}

// pgRecord reads logical's record in family's intake_consumers rows on db.
func pgRecord(t *testing.T, family string, db *pgxpool.Pool, logical string) selection.Record {
	t.Helper()
	record, _, err := pgFamilyOf(t, family, db).Read(context.Background(), logical)
	require.NoError(t, err, "%s's row", logical)
	return record
}

// fixedNames are the names of the stream's consumers that are a family's fixed names, as the
// operator lists them.
func (rig *convertRig) fixedNames() []string {
	rig.t.Helper()
	nc, _ := rig.operator()
	defer nc.Close()
	return slices.DeleteFunc(censusConsumers(rig.t, nc), func(name string) bool { _, ok := classify(name); return !ok })
}

// requireFresh requires record to select a fresh name of m started at start, carrying m's
// metadata, with m's fixed name not retired: the selection package refuses a record naming it
// anywhere but as an open operation's old name.
func (rig *convertRig) requireFresh(m member, record selection.Record, start uint64) *jetstream.ConsumerInfo {
	rig.t.Helper()
	require.Nil(rig.t, record.Op, "%s's record holds an open operation", m.Logical)
	require.True(rig.t, strings.HasPrefix(record.Current, m.Prefix+"~"), "%s's record selects %q, not a fresh name", m.Logical, record.Current)
	require.Equal(rig.t, start, record.Start, "%s's saved start", m.Logical)
	require.NotContains(rig.t, record.Retired, m.Fixed, "%s's record retires its fixed name %s", m.Logical, m.Fixed)
	info := rig.consumer(record.Current)
	require.NotNil(rig.t, info, "the server does not hold %s, %s's current", record.Current, m.Logical)
	require.Equal(rig.t, jetstream.DeliverByStartSequencePolicy, info.Config.DeliverPolicy)
	require.Equal(rig.t, start, info.Config.OptStartSeq)
	require.Equal(rig.t, m.logicalMetadata(), info.Config.Metadata[selection.LogicalMetadataKey])
	if m.SessionID != "" {
		require.Equal(rig.t, m.SessionID, info.Config.Metadata[bus.SessionIDMetadataKey])
	}
	return info
}

// freshResolve resolves m's record in store as m's family's fresh creator does
// (selection.Names.Resolve, with the family's prefix, fixed name and key check), which refuses a
// record naming a consumer outside the logical's own fresh names, a retired fixed name among them.
func freshResolve(m member, store selection.Store) (selection.Selection, error) {
	config := selection.Config{Store: store, Prefix: func(string) string { return m.Prefix }, Fixed: func(string) string { return m.Fixed }}
	if m.Family == familySession && m.SessionID == "" {
		config.CheckKey = selection.CheckDurableKey
	}
	return selection.New(config).Resolve(context.Background(), m.Logical)
}

// Every family's fixed names are converted: a session consumer, a listener's durable, Dispatch's
// two durables and a daemon's consumer, each with messages its fixed name acknowledged and messages
// it did not. convert writes each logical name's record with a fresh name created at its fixed
// name's ack floor + 1 from the fixed name's configuration, with the member's metadata, deletes
// every fixed name, writes each family's broker cutover row and store marker done (the bucket's
// sentinel holding this cutover's B and hold), marks its record complete and writes its done
// entry; each record convert wrote resolves through its family's fresh creator (freshResolve),
// with no fixed name retired; after the release every message the fixed names had not
// acknowledged arrives on the fresh names. Control, run first: the same command while the row is
// released is refused and converts nothing. It fails when a fixed name is left, a start is not its
// floor + 1, a record does not resolve, or a pending message does not arrive. The resolve's
// control is the convert that retired the fixed name (the previous convert.go, by -overlay),
// whose records the fresh creator refuses, naming the key's retired fixed name.
func TestConvertConvertsEveryFamilysFixedNamesAtTheirFloors(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	dispatch := rig.database(familyDispatch, dispatchMigrations...)
	daemon := rig.database(daemonFamily("p1"), daemonMigrations...)
	durables := dispatchDurables()
	cases := []struct {
		fixed, subject string
	}{
		{"s-ses1", "notifications.agent.ses1"},
		{"listener-hub-a", "notifications.github.hub-a"},
		{durables[0], "notifications.dispatch.one"},
		{durables[1], "notifications.dispatch.two"},
		{"legion-go-p1-dispatch", "notifications.legion.p1.controller"},
	}
	// Each fixed name acknowledges the first three of its six messages: its floor is the third's
	// sequence, and the three after it pend.
	floors := map[string]uint64{}
	for _, c := range cases {
		seqs := rig.publish(c.subject, 6)
		rig.fixed(c.fixed, c.subject, time.Hour)
		floors[c.fixed] = seqs[2]
	}
	for _, c := range cases {
		consumer, err := rig.writer.Consumer(ctx, notifications, c.fixed)
		require.NoError(t, err)
		require.NoError(t, rig.ackUpTo(consumer, floors[c.fixed]))
	}

	code, _, stderr := rig.run(actionConvert, "--hold", "no-such-hold")
	require.Equal(t, 1, code, "control: convert in a released row")
	require.Contains(t, stderr, "released")
	require.Len(t, rig.fixedNames(), len(cases), "control: the refused convert changed a fixed name")

	id, cohort := rig.heldForConvert()
	require.Len(t, cohort.Members, len(cases))
	code, stdout, stderr := rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	require.Contains(t, stdout, "the final listing holds no fixed name")
	require.Empty(t, rig.fixedNames(), "a fixed name is left after convert")

	fresh := map[string]string{}
	for _, c := range cases {
		m, ok := classify(c.fixed)
		require.True(t, ok)
		var record selection.Record
		var store selection.Store
		switch m.Family {
		case familySession:
			record = rig.record(familySession, m.Logical)
			nc, _ := rig.operator()
			store = newSessionStore(nc)
		case familyDispatch:
			record = pgRecord(t, familyDispatch, dispatch, m.Logical)
			store = pgFamilyOf(t, familyDispatch, dispatch)
		default:
			record = pgRecord(t, m.Family, daemon, m.Logical)
			store = pgFamilyOf(t, m.Family, daemon)
		}
		sel, err := freshResolve(m, store)
		require.NoError(t, err, "%s's fresh creator refuses the record convert wrote", m.Logical)
		require.Equal(t, record.Current, sel.Record.Current, "%s's fresh creator selects another name", m.Logical)
		require.NotContains(t, sel.Record.Retired, m.Fixed, "%s's record retires its fixed name", m.Logical)
		info := rig.requireFresh(m, record, floors[c.fixed]+1)
		require.Equal(t, c.subject, info.Config.FilterSubject, "%s's fresh name keeps its filter", c.fixed)
		require.Equal(t, time.Hour, info.Config.InactiveThreshold, "%s's fresh name does not carry its creator's inactive threshold", c.fixed)
		fresh[c.fixed] = record.Current
	}
	cutovers, err := rig.records().Cutovers(ctx)
	require.NoError(t, err)
	states := map[string]string{}
	for _, row := range cutovers {
		require.Equal(t, id, row.HoldID)
		states[row.Family] = row.State
	}
	require.Equal(t, map[string]string{familySession: "done", familyDispatch: "done", daemonFamily("p1"): "done"}, states)
	nc, _ := rig.operator()
	sentinel, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).State(ctx)
	require.NoError(t, err)
	require.Equal(t, ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: cohort.Boundary, Hold: id}), sentinel)
	stream, err := streamInfo(ctx, nc, notifications)
	require.NoError(t, err)
	dispatchState, err := pgFamilyOf(t, familyDispatch, dispatch).State(ctx)
	require.NoError(t, err)
	require.Equal(t, ownSentinel(familyDispatch, selection.StoreState{Cutover: selection.CutoverDone}), dispatchState)
	daemonState, err := pgFamilyOf(t, daemonFamily("p1"), daemon).State(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, selection.StreamIdentity(stream))
	require.Equal(t, ownSentinel(daemonFamily("p1"), selection.StoreState{Cutover: selection.CutoverDone, Boundary: stream.State.LastSeq + 1, StreamID: selection.StreamIdentity(stream)}), daemonState,
		"the daemon store's sentinel holds no boundary of the stream's end at the cutover, or not the stream's identity")
	set, _, found, err := readConvertSet(ctx, rig.records(), id)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, set.Complete, "the convert record is not marked complete")
	require.Len(t, set.Names, len(cases))
	for _, name := range set.Names {
		require.Equal(t, prepareThreshold, name.Config.InactiveThreshold,
			"control: %s's saved configuration carries prepare's raise, which its fresh name would carry without its creator's threshold", name.Fixed)
	}
	require.Equal(t, "done", rig.lastEntry(id, actionConvert).State)

	rig.censusDone(id)
	require.NoError(t, rig.release(id))
	rig.connectWriter()
	for _, c := range cases {
		got := rig.consume(fresh[c.fixed], 10, 0)
		require.Equal(t, []uint64{floors[c.fixed] + 1, floors[c.fixed] + 2, floors[c.fixed] + 3}, got,
			"%s's fresh name %s delivered %v", c.fixed, fresh[c.fixed], got)
	}
}

// ackUpTo acknowledges every message consumer holds unacknowledged at or below upTo: it pulls them
// again (each awaits its ack) and acknowledges each one, waiting for the acknowledgement.
func (rig *convertRig) ackUpTo(consumer jetstream.Consumer, upTo uint64) error {
	ctx := context.Background()
	for {
		info, err := consumer.Info(ctx)
		if err != nil {
			return err
		}
		if info.AckFloor.Stream >= upTo {
			return nil
		}
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			return err
		}
		got := false
		for msg := range batch.Messages() {
			got = true
			meta, err := msg.Metadata()
			if err != nil {
				return err
			}
			if meta.Sequence.Stream > upTo {
				return fmt.Errorf("%s handed %d before its floor reached %d", info.Name, meta.Sequence.Stream, upTo)
			}
			if err := msg.DoubleAck(ctx); err != nil {
				return err
			}
		}
		if !got {
			return fmt.Errorf("%s handed nothing before its floor reached %d (floor %d)", info.Name, upTo, info.AckFloor.Stream)
		}
	}
}

// An interrupted Dispatch take-over (§7 family 4): the take-over saved its start 51 in the intake's
// hold token and stopped, once after deleting the durable and creating nothing, and once before
// deleting it, with the durable at floor 60. In both, convert starts the durable's fresh name at 51,
// the token's start, and job 51 arrives; with neither the durable nor a token start, convert stops
// for an explicit replay with a stop entry, and the replay's start then converts it. Control: the
// durable's floor alone is 60, so its floor + 1, today's take-over start without the token, is 61,
// past job 51. It fails when convert starts past the token's start, or converts a durable with no
// known start.
func TestConvertStartsAtDispatchsHoldTokenStart(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	dispatch := rig.database(familyDispatch, dispatchMigrations...)
	durables := dispatchDurables()
	present, deleted := durables[0], durables[1]
	rig.publish("notifications.dispatch.jobs", 100)
	rig.fixed(present, "notifications.dispatch.jobs", time.Hour)
	rig.fixed(deleted, "notifications.dispatch.jobs", time.Hour)
	for _, name := range []string{present, deleted} {
		consumer, err := rig.writer.Consumer(ctx, notifications, name)
		require.NoError(t, err)
		require.NoError(t, rig.ackUpTo(consumer, 60))
	}
	for _, name := range []string{present, deleted} {
		_, err := dispatch.Exec(ctx, `insert into duty_holds (name, holder, token, expires_at) values ($1, gen_random_uuid(), $2, now() - interval '1 minute')`,
			"delivery-intake-"+name, "_INBOX.dispatch.taken start=51")
		require.NoError(t, err)
	}
	require.EqualValues(t, 60, rig.consumer(present).AckFloor.Stream, "control: the durable's floor alone")

	id, cohort := rig.heldForConvert()
	// The take-over that deleted its durable did so after prepare listed it, as one killed between
	// its delete and its create in the hold's last minutes would; prepare's cohort saved its floor.
	require.NotNil(t, cohort.member(deleted))
	nc, _ := rig.operator()
	require.NoError(t, deleteConsumer(ctx, nc, notifications, deleted))
	nc.Close()

	code, _, stderr := rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	m, _ := classify(present)
	rig.requireFresh(m, pgRecord(t, familyDispatch, dispatch, present), 51)
	gone := pgRecord(t, familyDispatch, dispatch, deleted)
	require.Empty(t, gone.Current, "the durable deleted before convert has a current")
	require.EqualValues(t, 51, gone.Start, "the durable deleted before convert starts past the token's start")

	rig.censusDone(id)
	require.NoError(t, rig.release(id))
	rig.connectWriter()
	got := rig.consume(pgRecord(t, familyDispatch, dispatch, present).Current, 1, 0)
	require.Equal(t, []uint64{51}, got, "job 51 did not arrive first")
}

// A cohort member removed for inactivity between prepare and convert's first listing (§7 family
// 5) gets its record with no current and start = its saved floor + 1, and its pending messages
// arrive once a creator registers it after the release; a Dispatch durable the stream does not
// hold, with no hold token start and no saved start, stops convert with a stop entry naming it
// (the fresh mode refuses a missing Dispatch row, so convert writes both), which admits a replay,
// and the rerun converts from the replay's start. Control: the expired member's fixed name is gone
// before convert lists, so a convert without the cohort would leave it no record and its first
// create would start past its messages. It fails when the expired member gets no start, a start past
// its floor + 1, or convert writes a durable with no safe start.
func TestConvertStartsAGoneCohortMemberAtItsSavedFloorAndStopsForANameWithNoStart(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	dispatch := rig.database(familyDispatch, dispatchMigrations...)
	rig.publish("notifications.agent.expiring", 4)
	rig.fixed("s-expiring", "notifications.agent.expiring", 0)
	consumer, err := rig.writer.Consumer(ctx, notifications, "s-expiring")
	require.NoError(t, err)
	require.NoError(t, rig.ackUpTo(consumer, 2))

	id, cohort := rig.heldForConvert()
	saved := cohort.member("s-expiring")
	require.NotNil(t, saved)
	require.EqualValues(t, 2, saved.Floor)
	nc, _ := rig.operator()
	// The inactivity removal, as the server makes it once the threshold passes.
	require.NoError(t, deleteConsumer(ctx, nc, notifications, "s-expiring"))
	nc.Close()
	require.Nil(t, rig.consumer("s-expiring"), "control: the expired member is gone before convert lists")

	code, _, stderr := rig.run(actionConvert, "--hold", id, "--dispatch-database-file", "/dev/null")
	require.Equal(t, 1, code, "convert wrote a durable with no safe start")
	for _, durable := range dispatchDurables() {
		require.Contains(t, stderr, "no safe start for "+durable)
		_, _, err := pgFamilyOf(t, familyDispatch, dispatch).Read(ctx, durable)
		require.ErrorIs(t, err, selection.ErrNoRecord, "convert wrote %s's row with no safe start", durable)
	}
	require.Equal(t, "stop", rig.lastEntry(id, actionConvert).State)
	expired := rig.record(familySession, "expiring")
	require.Empty(t, expired.Current)
	require.EqualValues(t, 3, expired.Start, "the expired member's start")

	// The replay the stop names, as replay writes it: the durable's row with its start, no current.
	_, err = rig.records().Admit(ctx, id, holdrecord.Request{Command: actionReplay}, map[string]string{"key": "delivery", "from": "1"})
	require.NoError(t, err, "the stop entry does not admit a replay")
	for _, durable := range dispatchDurables() {
		_, err := pgFamilyOf(t, familyDispatch, dispatch).Write(ctx, durable, selection.Record{Start: 1}, 0)
		require.NoError(t, err)
	}
	require.NoError(t, rig.records().AppendDone(ctx, id, actionReplay, nil))

	code, _, stderr = rig.run(actionConvert, "--hold", id, "--dispatch-database-file", "/dev/null")
	require.Zero(t, code, stderr)
	for _, durable := range dispatchDurables() {
		require.EqualValues(t, 1, pgRecord(t, familyDispatch, dispatch, durable).Start)
	}
	require.EqualValues(t, 3, rig.record(familySession, "expiring").Start)

	rig.censusDone(id)
	require.NoError(t, rig.release(id))
	rig.connectWriter()
	// A creator of the fresh mode registers the expired member: its record's start governs.
	names := selection.New(selection.Config{
		Store:    selection.NewKVStore(selection.ConnRequester{Conn: rig.writerConn}),
		Server:   convertTestServer{rig},
		Prefix:   func(string) string { return sessionFreshPrefix },
		Fixed:    selection.SessionNaming().Fixed,
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "", nil },
		Owner:    "the test's creator",
	})
	sel, _, err := names.Ensure(ctx, "expiring", selection.CreateRequest{
		Create: func(ctx context.Context, name string, start selection.Start) (*jetstream.ConsumerInfo, error) {
			c, err := rig.writer.CreateConsumer(ctx, notifications, jetstream.ConsumerConfig{Name: name, Durable: name,
				FilterSubject: "notifications.agent.expiring", AckPolicy: jetstream.AckExplicitPolicy,
				DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: start.Seq,
				Metadata: member{Family: familySession, Logical: "expiring", Fixed: "s-expiring", Prefix: sessionFreshPrefix, SessionID: "expiring"}.Metadata()})
			if err != nil {
				return nil, err
			}
			return c.CachedInfo(), nil
		},
		First: func(context.Context) (selection.Start, error) {
			return selection.Start{Native: selection.NativeNew}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{3, 4}, rig.consume(sel.Record.Current, 10, 0), "the expired member's pending messages")
}

// convertTestServer is the rig's writer as selection.Server.
type convertTestServer struct{ rig *convertRig }

func (s convertTestServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	info, err := consumerInfo(ctx, s.rig.writerConn, notifications, name)
	if err == nil && info == nil {
		return nil, selection.ErrNotFound
	}
	return info, err
}

func (s convertTestServer) Delete(ctx context.Context, name string) error {
	return deleteConsumer(ctx, s.rig.writerConn, notifications, name)
}

// A convert killed after it deleted F and before it created N leaves the record's replace open
// (old F, candidate N, start S) and F gone: the rerun settles it by Step 0, retiring N once the
// operation is 2 s old and creating a fresh N′ at S from F's configuration as the saved set holds
// it. Control, run first: the same state with the saved set holding no configuration of F is
// UNKNOWN, and creates nothing. It fails when the rerun loses S, reuses N, or creates N′ with
// another configuration than F's.
func TestConvertRerunAfterTheDeleteCreatesTheFreshNameFromTheSavedSet(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	rig.publish("notifications.agent.ses2", 5)
	rig.publish("notifications.agent.ses3", 5)
	rig.fixed("s-ses2", "notifications.agent.ses2", 0)
	rig.fixed("s-ses3", "notifications.agent.ses3", 0)
	id, _ := rig.heldForConvert()
	nc, _ := rig.operator()
	f2, f3 := rig.consumer("s-ses2"), rig.consumer("s-ses3")
	set := convertSet{Names: []convertSetName{
		{Family: familySession, Logical: "ses2", Fixed: "s-ses2"},
		{Family: familySession, Logical: "ses3", Fixed: "s-ses3", Config: &f3.Config},
	}}
	value, err := json.Marshal(set)
	require.NoError(t, err)
	_, err = rig.records().PutRecord(ctx, id, convertSetKind, convertSetKey, 0, value)
	require.NoError(t, err)
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	crashed := map[string]string{}
	for _, f := range []*jetstream.ConsumerInfo{f2, f3} {
		logical := strings.TrimPrefix(f.Name, "s-")
		candidate := selection.FreshName(sessionFreshPrefix)
		crashed[logical] = candidate
		// The record as convert writes it before F's delete: F as the replace's old name alone, with
		// no current (convertRule.convertName).
		_, err := store.Write(ctx, logical, selection.Record{Start: 2,
			Op: &selection.Op{Kind: selection.OpReplace, Old: f.Name, Candidate: candidate, ReservedAt: time.Now()}}, 0)
		require.NoError(t, err)
		require.NoError(t, deleteConsumer(ctx, nc, notifications, f.Name))
	}
	nc.Close()

	code, _, stderr := rig.run(actionConvert, "--hold", id)
	require.Equal(t, 2, code, "control: a rerun with no saved configuration of F")
	require.Contains(t, stderr, "saved set holds no configuration")
	require.NotNil(t, rig.record(familySession, "ses2").Op, "control: the open replace of ses2 was settled with no configuration")

	// The saved set as convert writes it, with F's configuration.
	set.Names[0].Config = &f2.Config
	value, err = json.Marshal(set)
	require.NoError(t, err)
	_, gen, _, err := readConvertSet(ctx, rig.records(), id)
	require.NoError(t, err)
	_, err = rig.records().PutRecord(ctx, id, convertSetKind, convertSetKey, gen, value)
	require.NoError(t, err)
	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	for _, logical := range []string{"ses2", "ses3"} {
		m, _ := classify("s-" + logical)
		record := rig.record(familySession, logical)
		info := rig.requireFresh(m, record, 2)
		require.NotEqual(t, crashed[logical], record.Current, "the rerun reused the crashed run's candidate")
		require.Contains(t, record.Retired, crashed[logical])
		require.Equal(t, "notifications.agent."+logical, info.Config.FilterSubject)
	}
}

// A daemon's fixed name whose project's store has no intake_consumers table, its daemon never
// booted the migration, stops convert with a stop entry naming the project, before convert changes
// anything. Control: the same project with its table converts. It fails when convert converts a
// family it cannot record, or changes anything before it stops.
func TestConvertStopsNamingADaemonProjectWithNoStore(t *testing.T) {
	rig := newConvertRig(t, 0)
	rig.publish("notifications.legion.p2.controller", 2)
	rig.fixed("legion-go-p2-wake", "notifications.legion.p2.controller", 0)
	rig.database(daemonFamily("p2"))
	id, _ := rig.heldForConvert()
	code, _, stderr := rig.run(actionConvert, "--hold", id)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "project p2")
	require.Equal(t, "stop", rig.lastEntry(id, actionConvert).State)
	require.Equal(t, []string{"legion-go-p2-wake"}, rig.fixedNames(), "convert changed a fixed name before it stopped")
	_, _, found, err := readConvertSet(context.Background(), rig.records(), id)
	require.NoError(t, err)
	require.False(t, found, "convert saved a set it cannot convert")

	db := rig.database(daemonFamily("p2"), daemonMigrations...)
	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, "control: the project with its table: %s", stderr)
	require.True(t, strings.HasPrefix(pgRecord(t, daemonFamily("p2"), db, "legion-go-p2-wake").Current, "legion-go-p2-wake~"))
}

// convert reads each daemon store a --daemon-database file names by the census's rule for the
// daemon stores, at exactly the address the project's one source names, before it changes
// anything, and writes that store alone: a file that reaches another database than the project's
// Deployment names stops convert, naming the project, the file and the Deployment, with no set
// saved, no fixed name changed and nothing written to the store. Control, in the same hold: with
// the Deployment naming the store the file reaches, convert converts the project. And b061ae27e4's
// convert, which opens the file's URL with no rule, converts through the file whatever the
// Deployment names.
func TestConvertReadsEachDaemonStoreByTheAddressRule(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	rig.publish("notifications.legion.cva.controller", 2)
	rig.fixed("legion-go-cva-wake", "notifications.legion.cva.controller", 0)
	db := rig.database(daemonFamily("cva"), daemonMigrations...)
	rig.censusDeploy(censusDeployment("cva", "daemon_elsewhere", censusInstance))
	file := "cva=" + rig.enrollFile(db)
	id, _ := rig.heldForConvert()

	code, _, stderr := rig.run(actionConvert, "--hold", id, "--daemon-database", file)
	require.Equal(t, 1, code, "convert beside a file another Deployment's database: %s", stderr)
	require.Contains(t, stderr, fmt.Sprintf("%s's store is unreconciled, so convert writes none of its records: --daemon-database cva=<file> reaches database %q, and Deployment legion/legion-daemon-cva names database %q",
		daemonFamily("cva"), db.Config().ConnConfig.Database, "daemon_elsewhere"))
	require.Equal(t, "stop", rig.lastEntry(id, actionConvert).State)
	require.Equal(t, []string{"legion-go-cva-wake"}, rig.fixedNames(), "convert changed a fixed name before it stopped")
	_, _, found, err := readConvertSet(ctx, rig.records(), id)
	require.NoError(t, err)
	require.False(t, found, "convert saved a set beside a store the rule does not read")
	_, err = enrollStore(t, "cva", db).State(ctx)
	require.ErrorIs(t, err, selection.ErrNoSentinel, "convert marked the store")

	deployments := rig.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace)
	rendered, err := deployments.Get(ctx, "legion-daemon-cva", metav1.GetOptions{})
	require.NoError(t, err)
	rendered.Annotations[writerhold.DaemonStoreDatabaseAnnotation] = db.Config().ConnConfig.Database
	_, err = deployments.Update(ctx, rendered, metav1.UpdateOptions{})
	require.NoError(t, err)
	code, _, stderr = rig.run(actionConvert, "--hold", id, "--daemon-database", file)
	require.Zero(t, code, "control: convert with the Deployment naming the store: %s", stderr)
	require.True(t, strings.HasPrefix(pgRecord(t, daemonFamily("cva"), db, "legion-go-cva-wake").Current, "legion-go-cva-wake~"))
}

// The held re-read catches a writer connected across the hold (plan §3a, "Every held command",
// and its line on the enroll hold's restart: a connection admitted before the row turned held
// keeps its grant). A listener connected before the row turned held, which this harness's hold
// does not close (h.held skips hold set's restart), is a writer: convert, run while it is
// connected, is refused by its re-read's report, which names that connection, before convert
// writes its intent or changes anything; once the connection is gone, convert runs. Control, run
// first: the auth service does not catch it, since the listener's connection kept its grant across
// the hold and still makes a fixed name, while a new connect of the same listener is refused. It
// fails when a held command acts beside a connection other than its own and the auth service's.
func TestConvertRereadCatchesAWriterConnectedAcrossTheHold(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	rig.publish("notifications.agent.w1", 2)
	rig.fixed("s-w1", "notifications.agent.w1", 0)
	id := rig.prepared(holdrecord.WorkflowCutover)
	code, _, stderr := rig.run(actionPrepare, "--hold", id)
	require.Zero(t, code, stderr)
	rig.held(id)

	rig.fixed("s-w2", "notifications.agent.w2", 0)
	refused, err := nats.Connect(rig.s.Cluster.URL(), rig.s.ListenerCredential(holdtest.ListenerMachines[0]), nats.NoReconnect(), nats.Timeout(10*time.Second))
	if err == nil {
		refused.Close()
	}
	require.ErrorIs(t, err, nats.ErrAuthorization, "control: a new connect of the listener during the hold")
	nc, _ := rig.operator()
	_, err = selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Boundary: 1})
	require.NoError(t, err)
	nc.Close()

	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Equal(t, 1, code, "convert acted beside the writer connected across the hold")
	require.Contains(t, stderr, "the held re-read, the report")
	require.Contains(t, stderr, `name "envoy-listener of today's build"`, "the re-read does not name the writer's connection")
	require.Empty(t, rig.lastEntry(id, actionConvert).State, "convert wrote an entry before its re-read passed")
	require.Equal(t, []string{"s-w1", "s-w2"}, rig.fixedNames(), "convert changed a fixed name before its re-read passed")

	rig.closeWriter()
	code, _, stderr = rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	require.Empty(t, rig.fixedNames())
}

// convert's store check reads Dispatch's store, whose sentinel row holds no store state, so convert
// cannot tell whether that store can hold the conversion: UNKNOWN (the default case of
// convertRule.checkStores), an exit after its intent. convert writes its stop, recording what it
// left as unknown (leftUnknownPrefix), naming what it read and the store it could not read, never
// unchanged: hold release is refused over it, and hold abort is admitted and ends the hold, its
// intent carrying what the stop left. Control, run first: under the earlier rule that an UNKNOWN
// exit writes no stop (leftRuleSet.unknownNoStop), convert's intent stays pending, and hold release
// and hold abort are both refused: a repeating UNKNOWN left the hold only convert's rerun.
func TestConvertStopsOnAnUnknownStoreAndHoldAbortEndsIt(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	rig.publish("notifications.agent.u1", 2)
	rig.fixed("s-u1", "notifications.agent.u1", 0)
	db := rig.database(familyDispatch, dispatchMigrations...)
	_, err := db.Exec(ctx, `insert into intake_consumers (logical, record, revision) values ($1, '"no store state"', 1)`, selection.PGSentinelKey(familyDispatch))
	require.NoError(t, err)
	id, _ := rig.heldForConvert()
	release, abort := holdrecord.Request{Command: holdrecord.CommandRelease}, holdrecord.Request{Command: holdrecord.CommandAbort}
	unknown := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := rig.run(actionConvert, "--hold", id, "--dispatch-database-file", "dispatch.url")
		require.Equal(t, 2, code, stderr)
		require.Contains(t, stderr, "UNKNOWN the dispatch family's store")
		require.Equal(t, []string{"s-u1"}, rig.fixedNames(), "the UNKNOWN convert changed a fixed name")
		return rig.lastEntry(id, actionConvert)
	}

	leftWith(t, leftRuleSet{unknownNoStop: true})
	require.Equal(t, "intent", unknown().State, "control: under the earlier rule the UNKNOWN leaves convert's intent pending")
	require.ErrorIs(t, holdrecord.Decide(rig.journal(id), release), holdrecord.ErrBusy, "control: hold release over the pending intent")
	require.ErrorIs(t, holdrecord.Decide(rig.journal(id), abort), holdrecord.ErrBusy, "control: hold abort over the pending intent")

	leftWith(t, leftRuleSet{})
	stop := unknown()
	require.Equal(t, "stop", stop.State, "the UNKNOWN after convert's intent ends in its stop entry")
	left := stop.Outputs[stopLeftKey]
	require.True(t, strings.HasPrefix(left, leftUnknownPrefix+"read hold "+id+"'s cohort record, "), "the stop records %q", left)
	require.Contains(t, left, "; unread the dispatch family's store: ", "the stop names the store convert could not read")
	require.Contains(t, left, "no run of convert in this hold saved its set")
	require.Contains(t, left, "earlier runs of "+actionConvert+" in this hold: run 1 ended with no entry", "the stop names the run the control left pending")
	require.ErrorIs(t, holdrecord.Decide(rig.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over the UNKNOWN stop")
	inputs := rig.abortHold(id)
	require.Contains(t, slices.Collect(maps.Values(inputs)), actionConvert+": "+left, "hold abort's intent carries what the stop left")
	require.Equal(t, "released", string(rig.row().Phase))
}

// A Dispatch store with no intake_consumers table, its database never migrated by Dispatch's
// migration that makes it, stops convert before it changes anything, and the stop names that
// migration when --dispatch-database-file names the store: the flag the operator gave is no remedy.
// Control, run first: the same store with no flag, which convert reaches because its set holds
// Dispatch's durables, names the flag.
func TestConvertNamesDispatchsMigrationForAStoreWithNoTable(t *testing.T) {
	rig := newConvertRig(t, 0)
	durable := dispatchDurables()[0]
	rig.publish("notifications.dispatch.one", 2)
	rig.fixed(durable, "notifications.dispatch.one", 0)
	rig.database(familyDispatch, dispatchMigrations[0])
	id, _ := rig.heldForConvert()
	migration := strings.TrimSuffix(filepath.Base(dispatchMigrations[len(dispatchMigrations)-1]), ".up.sql")
	stopped := func(args ...string) string {
		t.Helper()
		code, _, stderr := rig.run(append([]string{actionConvert, "--hold", id}, args...)...)
		require.Equal(t, 1, code, stderr)
		stop := rig.lastEntry(id, actionConvert)
		require.Equal(t, "stop", stop.State)
		require.Equal(t, []string{durable}, rig.fixedNames(), "convert changed a fixed name before it stopped")
		return stop.Outputs["reason"]
	}

	require.Contains(t, stopped(), "give --dispatch-database-file <file>", "control: with no flag the refusal names the flag")
	reason := stopped("--dispatch-database-file", "dispatch.url")
	require.Contains(t, reason, migration, "the refusal does not name Dispatch's migration")
	require.NotContains(t, reason, "give --dispatch-database-file", "the refusal names a flag the operator gave")
}

// convertConsumerSubject is the JetStream API subject of verb (CREATE or DELETE) for consumer name
// of the notification stream, in the one form convert sends (writeConsumer, deleteConsumer).
func convertConsumerSubject(verb, name string) string {
	return "$JS.API.CONSUMER." + verb + "." + notifications + "." + name
}

// convertWire dials a run's connections as nats.go does and reads every JetStream API subject the
// run sends on them, in the order it sends them (sent). When cutAt names a subject, the first write
// that sends it ends the run (cut, the run's ctx's cancel) and closes its connection before the
// subject is sent, and every later dial is refused: the run killed right before that request, so
// no copy of it reaches the hub on a reconnect.
type convertWire struct {
	cutAt string
	cut   func()
	done  atomic.Bool

	mu       sync.Mutex
	subjects []string
}

// convertWireAPI is a JetStream API request's PUB line, or a headered HPUB's, by its subject.
var convertWireAPI = regexp.MustCompile(`H?PUB (\$JS\.API\.\S+) `)

func (w *convertWire) Dial(network, address string) (net.Conn, error) {
	if w.done.Load() {
		return nil, errors.New("the killed run dials no more")
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &convertWireConn{Conn: conn, w: w}, nil
}

// sent is every JetStream API subject the run sent, in its order.
func (w *convertWire) sent() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.subjects)
}

type convertWireConn struct {
	net.Conn
	w *convertWire
}

func (c *convertWireConn) Write(p []byte) (int, error) {
	if c.w.done.Load() {
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	}
	var subjects []string
	for _, match := range convertWireAPI.FindAllSubmatch(p, -1) {
		subject := string(match[1])
		if c.w.cutAt != "" && subject == c.w.cutAt && c.w.done.CompareAndSwap(false, true) {
			c.w.cut()
			_ = c.Conn.Close()
			return 0, net.ErrClosed
		}
		subjects = append(subjects, subject)
	}
	c.w.mu.Lock()
	c.w.subjects = append(c.w.subjects, subjects...)
	c.w.mu.Unlock()
	return c.Conn.Write(p)
}

// convertOnWire runs convert --hold id as rig.run does, on connections of w's, the run ended at
// w's cut.
func (rig *convertRig) convertOnWire(id string, w *convertWire) (int, string, string) {
	rig.t.Helper()
	run, kill := context.WithCancel(context.Background())
	defer kill()
	w.cut = kill
	return rig.runDialed(run, w, actionConvert, "--hold", id)
}

// convertKilledBeforeTheDelete runs convert --hold id killed right before it sends the delete of
// fixed (convertWire), requires the run cut there with nothing written after its intent, and
// answers logical's record as the kill left it.
func (rig *convertRig) convertKilledBeforeTheDelete(id, fixed, logical string) selection.Record {
	rig.t.Helper()
	wire := &convertWire{cutAt: convertConsumerSubject("DELETE", fixed)}
	code, _, stderr := rig.convertOnWire(id, wire)
	require.True(rig.t, wire.done.Load(), "the run was never cut at its delete of %s\n%s", fixed, stderr)
	require.NotZero(rig.t, code, stderr)
	require.Equal(rig.t, "intent", rig.lastEntry(id, actionConvert).State, "the killed run wrote an entry after its intent")
	require.NotNil(rig.t, rig.consumer(fixed), "the killed run deleted %s", fixed)
	return rig.record(familySession, logical)
}

// A convert killed after its record write and before its delete of F (its connection cut right
// before it sends F's delete and its run ended, convertWire) leaves K's record naming F only as the
// open replace's old name, with no current: plan §3 step 2 writes op = {replace, old F, candidate
// N} and start = S alone, and the selection package refuses a record whose current is the fixed
// name (consumernames Names.Check). The record passes the fence every reader of a record applies
// (checkRecordFence), F is still on the stream, and convert's rerun settles the open replace (Step
// 0) and completes: K's record selects a fresh name at F's floor + 1 with the killed run's
// candidate retired, and no fixed name is left. Control: the convert that wrote F as the record's
// current while its conversion was open (the convert.go before this rule), whose record the fence
// refuses, naming its current. It fails when an open conversion's record names F anywhere but as
// the replace's old name, or the rerun does not complete the conversion.
func TestAConvertKilledBeforeItsDeleteOfFLeavesARecordTheFencePasses(t *testing.T) {
	rig := newConvertRig(t, 0)
	ctx := context.Background()
	const fixed, subject = "s-ses_kill", "notifications.agent.ses_kill"
	seqs := rig.publish(subject, 4)
	rig.fixed(fixed, subject, 0)
	consumer, err := rig.writer.Consumer(ctx, notifications, fixed)
	require.NoError(t, err)
	require.NoError(t, rig.ackUpTo(consumer, seqs[1]))
	id, _ := rig.heldForConvert()
	m, ok := classify(fixed)
	require.True(t, ok)

	record := rig.convertKilledBeforeTheDelete(id, fixed, m.Logical)
	require.NoError(t, checkRecordFence(familySession, m.Logical, record), "the open conversion's record fails the fence")
	require.NotNil(t, record.Op, "the killed run left no open operation")
	require.Equal(t, selection.OpReplace, record.Op.Kind)
	require.Equal(t, fixed, record.Op.Old, "the open replace's old name")
	require.Empty(t, record.Current, "the open conversion's record has a current")
	require.Equal(t, seqs[1]+1, record.Start, "the open conversion's start is not F's floor + 1")
	killed := record.Op.Candidate

	code, _, stderr := rig.run(actionConvert, "--hold", id)
	require.Zero(t, code, stderr)
	record = rig.record(familySession, m.Logical)
	rig.requireFresh(m, record, seqs[1]+1)
	require.NotEqual(t, killed, record.Current, "the rerun reused the killed run's candidate")
	require.Equal(t, []string{killed}, record.Retired, "the rerun's record retires the killed run's candidate alone")
	require.Empty(t, rig.fixedNames(), "a fixed name is left after the rerun")
}

// An open replace {old F, candidate N} with N absent and F present, as a convert killed after its
// record write and before its delete of F leaves it (convertKilledBeforeTheDelete), on
// ENVOY_NOTIFICATIONS at a full max_consumers that F's slot fills: convert's rerun settles it (Step
// 0, convertRule.settle) by deleting F before it reserves and creates a fresh N′ (plan §3 step 2:
// F goes first, so no logical name ever holds two slots and no cap update is needed). The rerun's
// own API subjects, read on its connection (convertWire), send F's delete before any create, and
// one create, N′'s, which the server takes: no create is refused at the cap, no stream update is
// sent, and max_consumers is unchanged. Control: the settle that created N′ first and deleted F
// only at publish (the convert.go before this rule), whose first create at the full cap the server
// refuses (err_code 10026), so its launch retires each refused candidate and deletes F only
// then. It fails when a candidate's create comes before F's delete, a create is refused, or the cap
// changes.
func TestConvertsSettleDeletesTheFixedNameBeforeItCreatesTheCandidateAtAFullCap(t *testing.T) {
	rig := newConvertRig(t, 1)
	ctx := context.Background()
	const fixed, subject = "s-ses_cap", "notifications.agent.ses_cap"
	seqs := rig.publish(subject, 4)
	rig.fixed(fixed, subject, 0)
	consumer, err := rig.writer.Consumer(ctx, notifications, fixed)
	require.NoError(t, err)
	require.NoError(t, rig.ackUpTo(consumer, seqs[1]))
	id, _ := rig.heldForConvert()
	m, ok := classify(fixed)
	require.True(t, ok)

	record := rig.convertKilledBeforeTheDelete(id, fixed, m.Logical)
	require.NotNil(t, record.Op, "the killed run left no open operation")
	require.Equal(t, fixed, record.Op.Old)
	absent := record.Op.Candidate
	require.Nil(t, rig.consumer(absent), "the killed run created its candidate")

	spy := &convertWire{}
	code, _, stderr := rig.convertOnWire(id, spy)
	require.Zero(t, code, stderr)
	record = rig.record(familySession, m.Logical)
	rig.requireFresh(m, record, seqs[1]+1)
	var order []string
	for _, s := range spy.sent() {
		require.NotEqual(t, "$JS.API.STREAM.UPDATE."+notifications, s, "the rerun updated the stream")
		if strings.HasPrefix(s, "$JS.API.CONSUMER.CREATE."+notifications+".") || strings.HasPrefix(s, "$JS.API.CONSUMER.DELETE."+notifications+".") {
			order = append(order, s)
		}
	}
	require.Equal(t, []string{convertConsumerSubject("DELETE", fixed), convertConsumerSubject("CREATE", record.Current)}, order,
		"the rerun's creates and deletes on %s, in the order it sent them", notifications)
	require.Equal(t, []string{absent}, record.Retired, "the rerun retired a candidate besides the killed run's")
	nc, _ := rig.operator()
	stream, err := streamInfo(ctx, nc, notifications)
	require.NoError(t, err)
	require.Equal(t, 1, stream.Config.MaxConsumers, "the stream's max_consumers changed")
	require.Empty(t, rig.fixedNames(), "a fixed name is left after the rerun")
}

// convert stops for K on a failed delete of K's fixed name F, as it stops on a failed read of F's
// info: its stop entry names F, K's record and the refused subject, and records what convert's
// runs left (its saved set, never unchanged); F stays on the stream, and K's record never retires
// F (the selection package refuses a record that names F anywhere but as an open replace's old
// name, consumernames Names.Check). The failed delete is the server's own: on a hub of the test's
// own (grantsNewRig), the operator user holds the held grant with a deny of F's delete subject
// alone (PublishDeny), so the server refuses that delete, and no other subject of the run, with a
// permissions violation. Two states: convert's own conversion of F, its record written before the
// delete, and a record converted already whose F is still listed. Control: the convert that
// retired F before deleting it (the convert.go before the selection library owned the record),
// whose stop leaves F among the retired names of the record converted already. It fails when a
// failed delete of F leaves F retired, a record the fence refuses, or no stop.
func TestConvertStopsOnAFailedDeleteOfTheFixedNameAndNeverRetiresIt(t *testing.T) {
	fixed := sessionFixedPrefix + "ses_grant"
	refused := convertConsumerSubject("DELETE", fixed)
	grant := natsgrant.Operator(natsgrant.OperatorPhaseHeld)
	grant.PublishDeny = append(slices.Clone(grant.PublishDeny), refused)
	m, ok := classify(fixed)
	require.True(t, ok)
	for _, c := range []struct {
		name      string
		converted bool
	}{
		{"convert's own conversion of F", false},
		{"a record converted already", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			g := grantsNewRig(t, 3, grant)
			id := g.hold(holdrecord.WorkflowCutover, natsgrant.OperatorPhaseHeld)
			g.notifications(3)
			g.fixed()
			value, err := json.Marshal(prepareCohort{Boundary: 1, Members: []prepareCohortMember{{Family: familySession, Logical: m.Logical, Fixed: fixed}}})
			require.NoError(t, err)
			_, err = g.records.PutRecord(ctx, id, prepareCohortKind, prepareCohortKey, 0, value)
			require.NoError(t, err)
			grantsSessionBucket(g, id)
			store := selection.NewKVStore(selection.ConnRequester{Conn: g.hub.admin})
			if c.converted {
				// The fresh name as convert creates it from F, and the record convert's publish
				// writes, with F still listed: a convert ended before its delete of F.
				info, err := consumerInfo(ctx, g.hub.admin, notifications, fixed)
				require.NoError(t, err)
				fresh := selection.FreshName(m.Prefix)
				_, err = writeConsumer(ctx, g.hub.admin, notifications, convertConsumerConfig(info.Config, fresh, 1, m), actionCreateConsumer)
				require.NoError(t, err)
				_, err = store.Write(ctx, m.Logical, selection.Record{Current: fresh, Start: 1}, 0)
				require.NoError(t, err)
			}

			code, _, stderr := createBucketRun(t, ctx, g.env(), actionConvert, "--hold", id)
			require.Equal(t, 1, code, stderr)
			require.Contains(t, stderr, `Permissions Violation for Publish to "`+refused+`"`, "the run's error names no refused delete of F")
			stop := lastOf(readJournal(t, g.records, id), actionConvert)
			require.Equal(t, "stop", stop.State, "convert's last entry after the failed delete")
			require.Contains(t, stop.Outputs["reason"], "delete "+fixed+", ", "the stop names no delete of F")
			require.Contains(t, stop.Outputs["reason"], m.Logical+"'s record", "the stop names no record of K")
			require.Contains(t, stop.Outputs["reason"], refused, "the stop names no refused subject")
			require.Contains(t, stop.Outputs[stopLeftKey], "convert's saved set of 1 fixed names under hold "+id, "the stop records no saved set")
			info, err := consumerInfo(ctx, g.hub.admin, notifications, fixed)
			require.NoError(t, err)
			require.NotNil(t, info, "F is gone after its refused delete")
			record, _, err := store.Read(ctx, m.Logical)
			require.NoError(t, err, "K's record")
			require.NotContains(t, record.Retired, fixed, "K's record retires F")
			require.NoError(t, checkRecordFence(familySession, m.Logical, record), "K's record fails the fence")
		})
	}
}
