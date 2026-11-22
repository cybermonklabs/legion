package consumernames

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// enrollDispatchFilter is the filter of a daemon's Dispatch durable
// (packages/daemon/internal/intake/consume.go, the Dispatch consumer's FilterSubjects).
const enrollDispatchFilter = "notifications.dispatch.issue.>"

// enrollDaemon is a Legion daemon in the fresh mode, as far as its consumer calls go: the
// selection protocol over its store with the daemon's own configuration
// (packages/daemon/internal/intake/durable_names.go, newDurableNames): the logical name as each
// fresh name's prefix, the ack floor as its floor, a logical name with a checkpoint and no record
// refused (missingRecord), a new project's first create at DeliverNew with the stream's end saved
// (first), its create (create), and every call on a physical name on a connection of that name's
// own, named as natsgrant.DaemonGenerationConnection names it, opened once the protocol reserved the
// name (Config.Reserved), as the stack's auth service grants a daemon entry's generation. Its
// stream identity, which it passes as Config.StreamID and tags each last floor with, is the store
// sentinel's StoreState.StreamID, read through the store, never the stream's info. The daemon's own
// durableNames.streamID in this tree reads the stream's info (consumernames.StreamIdentity) instead,
// so this part is the row's model, not the daemon's code. The daemon's module is its own, and its
// intake package is internal to it, so the row cannot run the daemon's code itself.
type enrollDaemon struct {
	project string
	connect func(name string) (*nats.Conn, error)
	// js is the daemon's own connection, which reads the stream alone.
	js    jetstream.JetStream
	store *selection.PGStore
	names *selection.Names

	mu          sync.Mutex
	generations map[string]jetstream.JetStream
}

// streamID is the stream identity the store's sentinel records.
func (d *enrollDaemon) streamID(ctx context.Context) (string, error) {
	state, err := d.store.State(ctx)
	if err != nil {
		return "", err
	}
	return state.StreamID, nil
}

// recordLastFloor writes the ack floor of kind's current, name, read on js, as its record's last
// floor, tagged with the sentinel's stream identity (durableNames.recordLastFloor), once the floor
// reads want.
func (d *enrollDaemon) recordLastFloor(ctx context.Context, kind string, js jetstream.JetStream, name string, want uint64) error {
	var floor uint64
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		consumer, err := js.Consumer(ctx, notifications, name)
		if err != nil {
			return err
		}
		if floor = consumer.CachedInfo().AckFloor.Stream; floor == want || time.Now().After(deadline) {
			break
		}
	}
	if floor != want {
		return fmt.Errorf("%s's ack floor is %d, want %d", name, floor, want)
	}
	id, err := d.streamID(ctx)
	if err != nil {
		return err
	}
	return d.names.RecordLastFloor(ctx, daemonFixedPrefix+d.project+"-"+kind, name, floor, id)
}

// enrollNewDaemon is project's daemon over its store db, its connections made by connect, the
// stack's daemon entry under the name it is given.
func enrollNewDaemon(db *pgxpool.Pool, connect func(name string) (*nats.Conn, error), project string) (*enrollDaemon, error) {
	own, err := connect("legion daemon " + project)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(own)
	if err != nil {
		return nil, err
	}
	store, err := pgStore(daemonFamily(project), db)
	if err != nil {
		return nil, err
	}
	d := &enrollDaemon{project: project, connect: connect, js: js, store: store, generations: map[string]jetstream.JetStream{}}
	d.names = selection.New(selection.Config{
		Store:    d.store,
		Server:   enrollDaemonServer{d: d},
		Prefix:   func(logical string) string { return logical },
		Fixed:    selection.LogicalNaming().Fixed,
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: d.streamID,
		MissingRecord: func(ctx context.Context, logical string) error {
			var seq int64
			err := db.QueryRow(ctx, `select stream_seq from consumer_positions where consumer = $1`, logical).Scan(&seq)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("%s has a checkpoint at %d and no selection record", logical, seq)
		},
		Owner: "legion daemon " + project + " (test)",
		Reserved: func(_ context.Context, _ string, op selection.Op) error {
			_, err := d.on(op.Candidate)
			return err
		},
	})
	return d, nil
}

// on is JetStream on physical's own connection, opened once.
func (d *enrollDaemon) on(physical string) (jetstream.JetStream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if js, ok := d.generations[physical]; ok {
		return js, nil
	}
	nc, err := d.connect(natsgrant.DaemonGenerationConnection(physical))
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	d.generations[physical] = js
	return js, nil
}

// enrollDaemonServer is the daemon's consumers, each on its own connection: info, and no delete,
// which the daemon's grant does not hold.
type enrollDaemonServer struct{ d *enrollDaemon }

func (s enrollDaemonServer) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	js, err := s.d.on(name)
	if err != nil {
		return nil, err
	}
	consumer, err := js.Consumer(ctx, notifications, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("%w: %s", selection.ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	return consumer.CachedInfo(), nil
}

func (s enrollDaemonServer) Delete(context.Context, string) error {
	return errors.New("the daemon's grant has no consumer delete")
}

// open is the daemon's open of its durable of kind filtering on filter (durableNames.open): the
// current its record selects, or a fresh one the protocol creates, and JetStream on the current's
// own connection, which the daemon consumes on.
func (d *enrollDaemon) open(ctx context.Context, kind, filter string) (selection.Selection, jetstream.JetStream, error) {
	logical := daemonFixedPrefix + d.project + "-" + kind
	sel, _, err := d.names.Ensure(ctx, logical, selection.CreateRequest{
		Create: func(ctx context.Context, name string, start selection.Start) (*jetstream.ConsumerInfo, error) {
			js, err := d.on(name)
			if err != nil {
				return nil, err
			}
			config := jetstream.ConsumerConfig{
				Durable:        name,
				FilterSubjects: []string{filter},
				AckPolicy:      jetstream.AckExplicitPolicy,
				AckWait:        30 * time.Second,
				Metadata:       map[string]string{selection.LogicalMetadataKey: logical},
			}
			switch start.Native {
			case selection.NativeNew:
				config.DeliverPolicy = jetstream.DeliverNewPolicy
			case selection.NativeAll:
				config.DeliverPolicy = jetstream.DeliverAllPolicy
			default:
				config.DeliverPolicy, config.OptStartSeq = jetstream.DeliverByStartSequencePolicy, start.Seq
			}
			consumer, err := js.CreateConsumer(ctx, notifications, config)
			if err != nil {
				return nil, err
			}
			return consumer.CachedInfo(), nil
		},
		First: func(ctx context.Context) (selection.Start, error) {
			stream, err := d.js.Stream(ctx, notifications)
			if err != nil {
				return selection.Start{}, err
			}
			return selection.Start{Seq: stream.CachedInfo().State.LastSeq + 1, Native: selection.NativeNew}, nil
		},
	})
	if err != nil {
		return sel, nil, err
	}
	js, err := d.on(sel.Record.Current)
	return sel, js, err
}

// enrollDaemonConnect connects as the stack's daemon entry under name, decided under the row's
// phase now.
func (h *stack) enrollDaemonConnect(name string) (*nats.Conn, error) {
	nc, err := h.s.Connect(h.s.Cluster.URL(), h.s.DaemonCredential(), nats.Name(name))
	if err == nil {
		// A writer: h.run closes it before a held command, as the held re-read admits none.
		h.track(nc)
	}
	return nc, err
}

// enrollWindow stands in for the window's cutover, which a store made after it missed: under a
// cutover hold, the selection bucket made with its sentinel cutover done (create-bucket's and
// convert's writes), and the session and Dispatch families' broker cutover records done, as convert
// writes them, straight through the hold routes; the hold, whose journal holds hold set's entries
// alone, is then released. Every later hold's held re-read finds the selection bucket present, as
// it is after the window.
func (h *stack) enrollWindow() {
	h.t.Helper()
	ctx := context.Background()
	window := h.heldHold(holdrecord.WorkflowCutover)
	nc, _ := h.operator()
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(h.t, err)
	for _, family := range []string{familySession, familyDispatch} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, window)
		require.NoError(h.t, err)
	}
	require.NoError(h.t, h.release(window))
}

// enrollRun runs enroll of project under hold id, with the store flag the command requires: the
// file of project's store, rendered with its Deployment (enrollDeployed).
func (h *stack) enrollRun(id, project string) (int, string, string) {
	h.t.Helper()
	return h.run(actionEnroll, "--hold", id, "--daemon", project, "--daemon-database", project+"="+h.enrollDeployed(project))
}

// enrollFile writes the file of the store on pool as --daemon-database names one: its URL, and
// censusInstance on its second line.
func (h *stack) enrollFile(pool *pgxpool.Pool) string {
	h.t.Helper()
	file := filepath.Join(h.t.TempDir(), "store.url")
	require.NoError(h.t, os.WriteFile(file, []byte(pool.Config().ConnString()+"\n"+censusInstance+"\n"), 0o600))
	return file
}

// enrollDeployed answers the file of project's store, the test's database of daemon/<project>
// (enrollFile), and renders project's Deployment naming that store (censusDeploymentOf) unless one
// of its name is there already: enroll reads the store only at the address its one source names.
func (h *stack) enrollDeployed(project string) string {
	h.t.Helper()
	h.mu.Lock()
	pool := h.dbs[daemonFamily(project)]
	h.mu.Unlock()
	require.NotNil(h.t, pool, "the test made no database for %s", daemonFamily(project))
	d := censusDeploymentOf(project, pool)
	_, err := h.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace).Get(h.t.Context(), d.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		h.censusDeploy(d)
	} else {
		require.NoError(h.t, err)
	}
	return h.enrollFile(pool)
}

// enrollRequireStopped asserts that enroll of project under a hold of its own wrote a stop entry
// naming want and recording holdrecord.LeftUnchanged, wrote neither the store's marker nor the
// broker's cutover record, and left the hold releasable: its release is admitted, since every stop
// recorded unchanged, and moves the row to released.
func (h *stack) enrollRequireStopped(project string, store *selection.PGStore, want ...string) {
	h.t.Helper()
	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, _, stderr := h.enrollRun(id, project)
	require.Equal(h.t, 1, code, "enroll of %s refuses", project)
	last := h.lastEntry(id, "")
	require.Equal(h.t, enrollCommand, last.Command)
	require.Equal(h.t, "stop", last.State, "enroll of %s ends in its stop entry", project)
	require.Equal(h.t, holdrecord.LeftUnchanged, last.Outputs["left"], "the stop records that no run of enroll changed anything")
	for _, w := range want {
		require.Contains(h.t, stderr, w)
		require.Contains(h.t, last.Outputs["reason"], w, "the stop entry names what enroll found")
	}
	state, err := store.State(context.Background())
	if err == nil {
		require.NotEqual(h.t, id, state.Hold, "the stopped enroll wrote no marker")
	}
	if row, ok := h.cutoverRow(daemonFamily(project)); ok {
		require.NotEqual(h.t, id, row.HoldID, "the stopped enroll wrote no cutover record")
	}
	require.NoError(h.t, h.release(id), "a hold whose enroll stopped is released")
	require.Equal(h.t, "released", string(h.row().Phase))
}

// TestEnrollANewProjectAfterTheWindow: after the cutover, a Legion project's new daemon store,
// which no cutover converted, is enrolled in a hold of the enroll workflow; the hold's record is
// complete, its release is admitted, the daemon in the fresh mode then serves every message
// published to its durable, and a later streams census reads the project's family done. Controls:
// with no enroll, that daemon refuses every consumer call and the census refuses, naming the
// family; a store holding a checkpoint, and a stream holding legion-go-<project>-dispatch, each make
// enroll write a stop entry naming it, and hold release then releases. (The release follows
// holdrecord's rule for stops that recorded LeftUnchanged, the table's WayOut, whose control is
// internal/holdrecord's model without WayOut.)
func TestEnrollANewProjectAfterTheWindow(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	// Project keys of the shape a daemon entry's generations are granted for (natsgrant's
	// daemonGeneration: an upper-case letter and up to nine upper-case letters or digits).
	const project, checkpointed, listed = "ENRNEW", "ENRCKPT", "ENRLIST"
	db := h.database(daemonFamily(project), daemonMigrations...)
	checkpointDB := h.database(daemonFamily(checkpointed), daemonMigrations...)
	listedDB := h.database(daemonFamily(listed), daemonMigrations...)
	h.replaceNotificationStream()
	h.enrollWindow()
	// The census's readers open each declared family's store from the file its flag names:
	// Dispatch's, which the window converted and which holds no record, and the new project's, at
	// the address its daemon's Deployment names (censusDaemonDeployed).
	dispatchFile, _ := censusStore(t, familyDispatch, dispatchMigrations...)
	censusArgs := []string{"--family", familySession, "--family", familyDispatch, "--family", daemonFamily(project),
		"--daemon-consumer", daemonFixedPrefix + project + "-dispatch=" + enrollDispatchFilter,
		"--dispatch-database-file", dispatchFile, "--daemon-database", project + "=" + h.censusDaemonDeployed(project, db)}

	// Control: the census, before any enroll, refuses naming the new project's family alone,
	// changes nothing, and prints no report it did not make; its stop entry records it unchanged,
	// so the hold is released.
	census := h.heldHold(holdrecord.WorkflowStreamsCensus)
	code, censusOut, stderr := h.censusCommand(census, censusArgs...)
	require.NotEqual(t, 0, code, "control: the census refuses before the family is enrolled")
	require.Contains(t, stderr, "not done: "+daemonFamily(project)+" (no row)")
	require.NotContains(t, censusOut, "CLEAN", "control: the refused census printed a report it did not make")
	censusLast := h.lastEntry(census, "")
	require.Equal(t, holdrecord.Operation{Command: censusCommandName, State: "stop"},
		holdrecord.Operation{Command: censusLast.Command, State: censusLast.State})
	require.Equal(t, holdrecord.LeftUnchanged, censusLast.Outputs["left"])
	require.NoError(t, h.release(census))

	// Control: a store holding a checkpoint stops enroll, naming it.
	_, err := checkpointDB.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 42, now())`, daemonFixedPrefix+checkpointed+"-dispatch")
	require.NoError(t, err)
	h.enrollRequireStopped(checkpointed, enrollStore(t, checkpointed, checkpointDB), "consumer_positions holds a checkpoint of "+daemonFixedPrefix+checkpointed+"-dispatch")

	// Control: a stream holding the project's fixed Dispatch durable stops enroll, naming it.
	wc, _ := h.replaceWriter()
	fixed := daemonFixedPrefix + listed + "-dispatch"
	makeConsumer(t, wc, jetstream.ConsumerConfig{Durable: fixed, FilterSubjects: []string{enrollDispatchFilter}, AckPolicy: jetstream.AckExplicitPolicy})
	h.enrollRequireStopped(listed, enrollStore(t, listed, listedDB), notifications+" lists consumer "+fixed)
	wc, _ = h.replaceWriter()
	require.NoError(t, deleteConsumer(ctx, wc, notifications, fixed))

	// Control: with no enroll, the daemon in the fresh mode refuses every consumer call, and no
	// consumer of the project is created.
	daemon, err := enrollNewDaemon(db, h.enrollDaemonConnect, project)
	require.NoError(t, err)
	for _, kind := range daemonKinds {
		_, _, err := daemon.open(ctx, kind, enrollDispatchFilter)
		require.True(t, selection.IsSelectionStateLost(err) || errors.Is(err, selection.ErrCutoverNotRun), "control: the unenrolled daemon's %s call is refused, got %v", kind, err)
	}
	released, _ := h.operator()
	require.Empty(t, consumersOf(t, released, enrollLogicalNames(project)...), "control: no consumer of the unenrolled project exists")

	// The enrolment, with messages on the stream before it, so its boundary is past 1.
	_, wjs := h.replaceWriter()
	replacePublish(t, wjs, 2)
	identity, last := h.enrollStream()
	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, stdout, _ := h.enrollRun(id, project)
	require.Equal(t, 0, code)
	require.Contains(t, stdout, daemonFamily(project)+": the store's marker and the broker's cutover record read done under hold "+id)
	state, err := enrollStore(t, project, db).State(ctx)
	require.NoError(t, err)
	require.Equal(t, ownSentinel(daemonFamily(project), selection.StoreState{Cutover: selection.CutoverDone, Hold: id, Boundary: last + 1, StreamID: identity}), state,
		"the marker records the boundary, the stream's last sequence + 1 at the enrolment, and the stream's identity")
	row, ok := h.cutoverRow(daemonFamily(project))
	require.True(t, ok)
	require.Equal(t, selection.CutoverDone, row.State)
	require.Equal(t, id, row.HoldID)
	journal := h.journal(id)
	require.True(t, journal.Complete, "enroll's done entry completes the hold's record")
	wantOutputs := map[string]string{"family": daemonFamily(project), "cutover": selection.CutoverDone, "boundary": formatSeq(last + 1), "stream_id": identity}
	enrolled := journal.Ops[len(journal.Ops)-1]
	require.Equal(t, holdrecord.Operation{Command: enrollCommand, State: "done", Outputs: wantOutputs},
		holdrecord.Operation{Command: enrolled.Command, State: enrolled.State, Outputs: enrolled.Outputs})
	require.NoError(t, h.release(id))

	// The daemon in the fresh mode now serves: its Dispatch durable is created, and every message
	// published to it arrives, in order.
	_, wjs = h.replaceWriter()
	daemon, err = enrollNewDaemon(db, h.enrollDaemonConnect, project)
	require.NoError(t, err)
	sel, djs, err := daemon.open(ctx, "dispatch", enrollDispatchFilter)
	require.NoError(t, err)
	current := sel.Record.Current
	require.True(t, strings.HasPrefix(current, daemonFixedPrefix+project+"-dispatch~"), "the daemon's durable is a fresh name, got %q", current)
	var published []uint64
	for i := range 5 {
		ack, err := wjs.Publish(ctx, fmt.Sprintf("notifications.dispatch.issue.enroll-%d", i), []byte("issue"))
		require.NoError(t, err)
		published = append(published, ack.Sequence)
	}
	consumer, err := djs.Consumer(ctx, notifications, current)
	require.NoError(t, err)
	batch, err := consumer.Fetch(len(published), jetstream.FetchMaxWait(10*time.Second))
	require.NoError(t, err)
	var delivered []uint64
	for msg := range batch.Messages() {
		meta, err := msg.Metadata()
		require.NoError(t, err)
		delivered = append(delivered, meta.Sequence.Stream)
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, batch.Error())
	require.Equal(t, published, delivered, "the enrolled daemon serves every message published to its durable")

	// A later census reads the family done, keeps the daemon's durable, which the project's store
	// records (RecordedFreshNames), and its done entry completes the streams-census hold's record.
	census = h.heldHold(holdrecord.WorkflowStreamsCensus)
	code, stdout, stderr = h.censusCommand(census, censusArgs...)
	require.Equal(t, 0, code, "the census reads every family done, %s with them, and runs clean: %s%s", daemonFamily(project), stdout, stderr)
	require.True(t, h.journal(census).Complete, "the clean census's done entry completes the record")
	nc, _ := h.operator()
	require.Equal(t, []string{current}, consumersOf(t, nc, enrollLogicalNames(project)...), "the census keeps the enrolled daemon's durable")
}

// TestAnEnrollRerunAfterACrashCompletes: enroll killed after its store marker write, or after its
// broker cutover record write too, is admitted again under the same hold, finds this hold's own
// writes, keeps them and completes the record, and the hold is released. A rerun that finds this
// hold's own writes beside something that stops it (a checkpoint, a marker naming another stream)
// writes a stop recording what every run left, this hold's marker and cutover record, which counts
// as changed: hold release is refused, and the way out is enroll's rerun, which completes once the
// finding is gone, or hold abort, whose intent carries the stop's record. Control: the same
// checkpoint without any write of this hold makes enroll stop recording unchanged, and that hold
// is released (TestEnrollANewProjectAfterTheWindow).
func TestAnEnrollRerunAfterACrashCompletes(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	h.replaceNotificationStream()
	h.enrollWindow()
	_, wjs := h.replaceWriter()
	replacePublish(t, wjs, 3)
	identity, last := h.enrollStream()
	// crashedBoundary is the boundary a crashed run of this build recorded, below the stream's last
	// sequence + 1 now, so a rerun that kept it is told from one that wrote it again.
	const crashedBoundary = 2
	require.Greater(t, last+1, uint64(crashedBoundary))
	crashed := func(project string, marker selection.StoreState, cutoverToo bool) (string, *pgxpool.Pool) {
		t.Helper()
		db := h.database(daemonFamily(project), daemonMigrations...)
		id := h.heldHold(holdrecord.WorkflowEnroll)
		_, err := h.records().Admit(ctx, id, holdrecord.Request{Command: enrollCommand}, map[string]string{"daemon": project})
		require.NoError(t, err)
		marker.Cutover, marker.Hold = selection.CutoverDone, id
		_, err = enrollStore(t, project, db).WriteSentinel(ctx, marker, 0)
		require.NoError(t, err)
		if cutoverToo {
			_, err = h.records().PutCutover(ctx, daemonFamily(project), selection.CutoverDone, id)
			require.NoError(t, err)
		}
		return id, db
	}
	whole := selection.StoreState{Boundary: crashedBoundary, StreamID: identity}
	requireCompleted := func(id, project string, db *pgxpool.Pool, boundary uint64) {
		t.Helper()
		code, _, _ := h.enrollRun(id, project)
		require.Equal(t, 0, code)
		state, err := enrollStore(t, project, db).State(ctx)
		require.NoError(t, err)
		require.Equal(t, ownSentinel(daemonFamily(project), selection.StoreState{Cutover: selection.CutoverDone, Hold: id, Boundary: boundary, StreamID: identity}), state)
		row, ok := h.cutoverRow(daemonFamily(project))
		require.True(t, ok)
		require.Equal(t, holdrecord.CutoverRow{Family: daemonFamily(project), State: selection.CutoverDone, HoldID: id, HoldGen: row.HoldGen}, row)
		require.True(t, h.journal(id).Complete)
		require.NoError(t, h.release(id))
	}

	t.Run("killed after the store's marker", func(t *testing.T) {
		id, db := crashed("ENRMARK", whole, false)
		requireCompleted(id, "ENRMARK", db, crashedBoundary)
	})
	t.Run("killed after the broker's cutover record", func(t *testing.T) {
		id, db := crashed("ENRBROK", whole, true)
		requireCompleted(id, "ENRBROK", db, crashedBoundary)
	})
	t.Run("a marker of this hold without the boundary and identity is written whole", func(t *testing.T) {
		id, db := crashed("ENRPART", selection.StoreState{}, false)
		requireCompleted(id, "ENRPART", db, last+1)
	})
	t.Run("a finding beside this hold's own writes stops recording them", func(t *testing.T) {
		const project = "ENRHALF"
		id, db := crashed(project, whole, false)
		_, err := db.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 7, now())`, daemonFixedPrefix+project+"-wake")
		require.NoError(t, err)
		code, _, stderr := h.enrollRun(id, project)
		require.Equal(t, 1, code)
		require.Contains(t, stderr, "consumer_positions holds a checkpoint of "+daemonFixedPrefix+project+"-wake")
		last := h.lastEntry(id, "")
		require.Equal(t, enrollCommand, last.Command)
		require.Equal(t, "stop", last.State)
		require.Equal(t, daemonFamily(project)+": store marker done, hold "+id+"'s; "+enrollCutRun, last.Outputs["left"], "the stop records the marker an earlier run of enroll wrote, and that run")
		require.ErrorIs(t, h.release(id), holdrecord.ErrOutOfOrder, "hold release is refused over a stop that changed something")
		_, err = db.Exec(ctx, `delete from consumer_positions`)
		require.NoError(t, err)
		requireCompleted(id, project, db, crashedBoundary)
	})
	t.Run("this hold's marker naming another stream identity stops, and hold abort ends the hold", func(t *testing.T) {
		const project = "ENRWRONG"
		id, db := crashed(project, selection.StoreState{Boundary: crashedBoundary, StreamID: "another-stream"}, true)
		code, _, stderr := h.enrollRun(id, project)
		require.Equal(t, 1, code)
		require.Contains(t, stderr, "this hold's marker records stream identity another-stream, and "+notifications+" carries "+identity)
		stop := h.lastEntry(id, "")
		require.Equal(t, "stop", stop.State)
		left := daemonFamily(project) + ": store marker done and broker cutover record done, hold " + id + "'s; " + enrollCutRun
		require.Equal(t, left, stop.Outputs["left"])
		state, err := enrollStore(t, project, db).State(ctx)
		require.NoError(t, err)
		require.Equal(t, "another-stream", state.StreamID, "the refused rerun wrote nothing")
		require.ErrorIs(t, h.release(id), holdrecord.ErrOutOfOrder, "hold release is refused over a stop that changed something")
		inputs := h.abortHold(id)
		require.Contains(t, slices.Collect(maps.Values(inputs)), enrollCommand+": "+left, "hold abort's intent carries what the stop left")
		require.Equal(t, "released", string(h.row().Phase))
	})
}

// enrollCutRun is how a stop of enroll names an earlier run of enroll in its hold cut after its
// intent (journalLeft).
const enrollCutRun = "earlier runs of " + enrollCommand + " in this hold: run 1 ended with no entry (cut after its intent), and the records hold what it wrote"

// enrollCut stands in for a run of enroll of project cut after it wrote its store's marker: in an
// enroll hold of its own, enroll's intent naming project, then the store's marker done under that
// hold with boundary and identity. It answers the hold and the store's pool.
func (h *stack) enrollCut(project string, boundary uint64, identity string) (string, *pgxpool.Pool) {
	h.t.Helper()
	ctx := context.Background()
	db := h.database(daemonFamily(project), daemonMigrations...)
	id := h.heldHold(holdrecord.WorkflowEnroll)
	_, err := h.records().Admit(ctx, id, holdrecord.Request{Command: enrollCommand}, map[string]string{"daemon": project})
	require.NoError(h.t, err)
	_, err = enrollStore(h.t, project, db).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Hold: id, Boundary: boundary, StreamID: identity}, 0)
	require.NoError(h.t, err)
	return id, db
}

// Every stop of enroll records what every run of enroll in this hold left (admission.left), as the
// stop of every other held command does, so a run cut after its intent counts as a change: a rerun
// of a daemon whose run was cut after it wrote its store's marker, and whose Deployment came to name
// another database, stops at the address rule before it reads the marker, recording that run 1
// ended with no entry and the records hold what it wrote; hold release is refused, and hold abort
// ends the hold. Control: feb6bdcd73's enroll, whose stop recorded unchanged from this run's reading
// alone, so hold release moved the row over the cut run's marker. (The review's case, a run of
// another daemon admitted as the cut run's rerun, which stopped recording unchanged at feb6bdcd73,
// is refused before its intent since: TestEnrollRefusesAnotherDaemonInAHoldWithAnEarlierEnroll.)
func TestAnEnrollStopCountsAnEarlierRunCutAfterItsIntent(t *testing.T) {
	h := newStack(t)
	h.replaceNotificationStream()
	h.enrollWindow()
	identity, last := h.enrollStream()
	const project = "ENRCUTM"
	id, db := h.enrollCut(project, last+1, identity)
	h.censusDeploy(censusDeployment(project, "daemon_elsewhere", censusInstance))
	code, _, stderr := h.run(actionEnroll, "--hold", id, "--daemon", project, "--daemon-database", project+"="+h.enrollFile(db))
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, daemonFamily(project)+"'s store is unreconciled, so enroll marks nothing")
	stop := h.lastEntry(id, "")
	left := stop.Outputs[stopLeftKey]
	require.Equal(t, enrollCommand+" stop", stop.Command+" "+stop.State)
	require.Contains(t, left, enrollCutRun)
	require.ErrorIs(t, h.release(id), holdrecord.ErrOutOfOrder, "hold release over a stop that counts the cut run")
	inputs := h.abortHold(id)
	require.Contains(t, slices.Collect(maps.Values(inputs)), enrollCommand+": "+left, "hold abort's intent carries what the stop left")
	require.Equal(t, "released", string(h.row().Phase))
}

// enroll refuses, before its intent, a run whose --daemon is not the daemon of an earlier run of
// enroll in its hold, naming both, so the journal's rerun by command name alone never gives the
// hold's one enroll to another store: enroll --daemon ENRFIRST is cut after it wrote its store's
// marker, and enroll --daemon ENRSECOND, a new and clean store, in that hold is refused and writes
// no entry, no marker and no cutover record; ENRFIRST's rerun then completes, and the hold is
// released. Control: ed76749bf3's enroll admits ENRSECOND as the cut run's rerun and completes the
// hold, so hold release leaves ENRFIRST's store marked done under it with no cutover record, which
// refuses every later enroll of ENRFIRST.
func TestEnrollRefusesAnotherDaemonInAHoldWithAnEarlierEnroll(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	h.replaceNotificationStream()
	h.enrollWindow()
	identity, last := h.enrollStream()
	id, firstDB := h.enrollCut("ENRFIRST", last+1, identity)
	secondDB := h.database(daemonFamily("ENRSECOND"), daemonMigrations...)
	before := len(h.journal(id).Ops)
	code, stdout, stderr := h.enrollRun(id, "ENRSECOND")
	require.Equal(t, 1, code, "enroll --daemon ENRSECOND after ENRFIRST's cut run: %s%s", stdout, stderr)
	require.Contains(t, stderr, "enroll --daemon ENRSECOND: this hold's earlier run of enroll was --daemon ENRFIRST")
	require.Len(t, h.journal(id).Ops, before, "the refused enroll wrote a journal entry")
	_, err := enrollStore(t, "ENRSECOND", secondDB).State(ctx)
	require.ErrorIs(t, err, selection.ErrNoSentinel, "the refused enroll marked ENRSECOND's store")
	_, ok := h.cutoverRow(daemonFamily("ENRSECOND"))
	require.False(t, ok, "the refused enroll wrote ENRSECOND's cutover record")

	code, stdout, stderr = h.enrollRun(id, "ENRFIRST")
	require.Equal(t, 0, code, "ENRFIRST's rerun: %s%s", stdout, stderr)
	row, ok := h.cutoverRow(daemonFamily("ENRFIRST"))
	require.True(t, ok)
	require.Equal(t, id, row.HoldID)
	state, err := enrollStore(t, "ENRFIRST", firstDB).State(ctx)
	require.NoError(t, err)
	require.Equal(t, id, state.Hold)
	require.NoError(t, h.release(id))
}

// enrollStream is the notification stream's identity and last sequence, read as the operator.
func (h *stack) enrollStream() (string, uint64) {
	h.t.Helper()
	nc, _ := h.operator()
	info, err := streamInfo(context.Background(), nc, notifications)
	require.NoError(h.t, err)
	identity := selection.StreamIdentity(info)
	require.NotEmpty(h.t, identity, "%s carries a stream identity", notifications)
	return identity, info.State.LastSeq
}

// TestEnrollStopsOnWhatItFinds: enroll stops, naming it, on each other sign that a store is not
// new: a selection row, a fresh-form consumer of the project on the stream, a marker done or
// rolled_back from another hold, a broker cutover record from another hold, and a store with no
// intake_consumers table; and on a stream that carries no identity, which the daemon could never
// count a last floor on. Each such hold is released. Control: a store with none of them, on a
// stream with an identity, is enrolled (TestEnrollANewProjectAfterTheWindow).
func TestEnrollStopsOnWhatItFinds(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	h.replaceNotificationStream()
	h.enrollWindow()

	t.Run("a selection row", func(t *testing.T) {
		const project = "ENRROW"
		db := h.database(daemonFamily(project), daemonMigrations...)
		store := enrollStore(t, project, db)
		_, err := store.Write(ctx, daemonFixedPrefix+project+"-github", selection.Record{Start: 3}, 0)
		require.NoError(t, err)
		h.enrollRequireStopped(project, store, "intake_consumers holds the selection row of "+daemonFixedPrefix+project+"-github")
	})
	t.Run("a fresh-form consumer on the stream", func(t *testing.T) {
		const project = "ENRFRESH"
		db := h.database(daemonFamily(project), daemonMigrations...)
		wc, _ := h.replaceWriter()
		fresh := selection.FreshName(daemonFixedPrefix + project + "-github")
		makeConsumer(t, wc, jetstream.ConsumerConfig{Durable: fresh, FilterSubjects: []string{"notifications.github.>"}, AckPolicy: jetstream.AckExplicitPolicy})
		h.enrollRequireStopped(project, enrollStore(t, project, db), notifications+" lists consumer "+fresh)
		wc, _ = h.replaceWriter()
		require.NoError(t, deleteConsumer(ctx, wc, notifications, fresh))
	})
	for marker, project := range map[string]string{selection.CutoverDone: "ENRMDONE", selection.CutoverRolledBack: "ENRMROLL"} {
		t.Run("a marker "+marker+" from another hold", func(t *testing.T) {
			db := h.database(daemonFamily(project), daemonMigrations...)
			store := enrollStore(t, project, db)
			_, err := store.WriteSentinel(ctx, selection.StoreState{Cutover: marker, Hold: "another-hold"}, 0)
			require.NoError(t, err)
			h.enrollRequireStopped(project, store, "the store's marker reads "+marker+" from hold another-hold")
		})
	}
	t.Run("a broker cutover record from another hold", func(t *testing.T) {
		const project = "ENROTHER"
		db := h.database(daemonFamily(project), daemonMigrations...)
		other := h.heldHold(holdrecord.WorkflowEnroll)
		_, err := h.records().PutCutover(ctx, daemonFamily(project), selection.CutoverRolledBack, other)
		require.NoError(t, err)
		require.NoError(t, h.release(other))
		h.enrollRequireStopped(project, enrollStore(t, project, db), "the broker holds the cutover record "+daemonFamily(project)+" rolled_back from hold "+other)
	})
	t.Run("a store with no intake_consumers table", func(t *testing.T) {
		const project = "ENRNOTAB"
		db := h.database(daemonFamily(project), daemonMigrations[0])
		h.enrollRequireStopped(project, enrollStore(t, project, db), daemonFamily(project)+"'s store is unreconciled, so enroll marks nothing: it has no intake_consumers table")
	})
	t.Run("a stream that carries no identity", func(t *testing.T) {
		const project = "ENRNOID"
		db := h.database(daemonFamily(project), daemonMigrations...)
		update := h.heldHold(holdrecord.WorkflowStreamsUpdate)
		nc, _ := h.operator()
		info, err := streamInfo(ctx, nc, notifications)
		require.NoError(t, err)
		config := info.Config
		config.Metadata = maps.Clone(config.Metadata)
		delete(config.Metadata, selection.StreamIdentityMetadataKey)
		_, err = apiRequest(ctx, nc, "$JS.API.STREAM.UPDATE."+notifications, config)
		require.NoError(t, err)
		require.NoError(t, h.release(update))
		h.enrollRequireStopped(project, enrollStore(t, project, db), notifications+" carries no stream identity ("+selection.StreamIdentityMetadataKey+")")
	})
}

// enrollFetch takes want messages from consumer name on js, acknowledging each once the server
// confirms it, then checks no more is waiting, and answers their stream sequences in order.
func enrollFetch(t *testing.T, js jetstream.JetStream, name string, want int) []uint64 {
	t.Helper()
	ctx := context.Background()
	consumer, err := js.Consumer(ctx, notifications, name)
	require.NoError(t, err)
	var got []uint64
	for deadline := time.Now().Add(20 * time.Second); len(got) < want && time.Now().Before(deadline); {
		batch, err := consumer.Fetch(want-len(got), jetstream.FetchMaxWait(2*time.Second))
		require.NoError(t, err)
		for msg := range batch.Messages() {
			meta, err := msg.Metadata()
			require.NoError(t, err)
			got = append(got, meta.Sequence.Stream)
			require.NoError(t, msg.DoubleAck(ctx))
		}
	}
	extra, err := consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	require.NoError(t, err)
	for msg := range extra.Messages() {
		meta, _ := msg.Metadata()
		t.Fatalf("%s handed over %v and then %d, past the %d messages wanted", name, got, meta.Sequence.Stream, want)
	}
	return got
}

// TestAnEnrolledStoreLetsTheDaemonCountItsFloor: enroll records in the store's marker the
// boundary, the stream's last sequence + 1 at the enrolment, and the stream's identity, which the
// daemon tags each last floor with (the row's daemon, enrollDaemon, reads it from the sentinel;
// durableNames.streamID in this tree reads the stream's info). So after its current is removed (an
// inactivity delete, done here by a listener's connection), the
// enrolled daemon's next open creates its fresh name at the sequence after its last floor and hands
// over nothing it acknowledged, and its first create starts at the boundary, so a message published
// between the enrolment and the daemon's first open arrives. Control: a store whose marker was
// written without either, as enroll wrote it before it recorded them, leaves the daemon unable to
// count its floor: its last floor carries no identity, so its next open falls back to the record's
// saved start and hands over again the messages it acknowledged; and its first create, at
// DeliverNew, misses the message published before it.
func TestAnEnrolledStoreLetsTheDaemonCountItsFloor(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	const enrolled, bare = "ENRFLOOR", "ENRBARE"
	enrolledDB := h.database(daemonFamily(enrolled), daemonMigrations...)
	bareDB := h.database(daemonFamily(bare), daemonMigrations...)
	h.replaceNotificationStream()
	h.enrollWindow()
	publish := func(n int) []uint64 {
		t.Helper()
		_, wjs := h.replaceWriter()
		var seqs []uint64
		for range n {
			ack, err := wjs.Publish(ctx, "notifications.dispatch.issue.floor", []byte("issue"))
			require.NoError(t, err)
			seqs = append(seqs, ack.Sequence)
		}
		return seqs
	}
	before := publish(3)
	identity, last := h.enrollStream()
	require.Equal(t, before[2], last)

	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, _, _ := h.enrollRun(id, enrolled)
	require.Equal(t, 0, code)
	require.NoError(t, h.release(id))
	state, err := enrollStore(t, enrolled, enrolledDB).State(ctx)
	require.NoError(t, err)
	require.Equal(t, ownSentinel(daemonFamily(enrolled), selection.StoreState{Cutover: selection.CutoverDone, Hold: id, Boundary: last + 1, StreamID: identity}), state)
	// Control: a store enrolled without the boundary and the identity.
	_, err = enrollStore(t, bare, bareDB).WriteSentinel(ctx, selection.StoreState{Cutover: selection.CutoverDone, Hold: "an-enrolment-without-them"}, 0)
	require.NoError(t, err)

	between := publish(1)[0]
	type served struct {
		daemon  *enrollDaemon
		db      *pgxpool.Pool
		js      jetstream.JetStream
		current string
	}
	open := func(project string, db *pgxpool.Pool) served {
		t.Helper()
		daemon, err := enrollNewDaemon(db, h.enrollDaemonConnect, project)
		require.NoError(t, err)
		sel, js, err := daemon.open(ctx, "dispatch", enrollDispatchFilter)
		require.NoError(t, err)
		return served{daemon: daemon, db: db, js: js, current: sel.Record.Current}
	}
	e, b := open(enrolled, enrolledDB), open(bare, bareDB)
	after := publish(3)
	require.Equal(t, append([]uint64{between}, after...), enrollFetch(t, e.js, e.current, 4), "the enrolled daemon starts at the boundary and hands over the message published before its first create")
	require.Equal(t, after, enrollFetch(t, b.js, b.current, 3), "control: the bare store's daemon starts at DeliverNew and misses the message published before its first create")

	floor := after[2]
	for project, s := range map[string]served{enrolled: e, bare: b} {
		require.NoError(t, s.daemon.recordLastFloor(ctx, "dispatch", s.js, s.current, floor), project)
		record, _, err := enrollStore(t, project, s.db).Read(ctx, daemonFixedPrefix+project+"-dispatch")
		require.NoError(t, err)
		want := ""
		if project == enrolled {
			want = identity
		}
		require.Equal(t, &selection.Floor{Floor: floor, Name: s.current, StreamID: want}, record.LastFloor, "%s's last floor carries the sentinel's identity", project)
		wc, _ := h.replaceWriter()
		require.NoError(t, deleteConsumer(ctx, wc, notifications, s.current), "the current's removal")
	}

	again := func(project string, db *pgxpool.Pool) served {
		t.Helper()
		s := open(project, db)
		record, _, err := enrollStore(t, project, db).Read(ctx, daemonFixedPrefix+project+"-dispatch")
		require.NoError(t, err)
		require.Equal(t, s.current, record.Current)
		return s
	}
	e2, b2 := again(enrolled, enrolledDB), again(bare, bareDB)
	require.NotEqual(t, e.current, e2.current)
	require.NotEqual(t, b.current, b2.current)
	next := publish(1)[0]
	require.Equal(t, []uint64{next}, enrollFetch(t, e2.js, e2.current, 1), "the enrolled daemon counts its last floor: its new consumer starts after it and hands over nothing it acknowledged")
	require.Equal(t, append(append([]uint64{}, after...), next), enrollFetch(t, b2.js, b2.current, 4),
		"control: the bare store's daemon cannot count its floor: its new consumer starts at the record's saved start and hands over again what it acknowledged")
}

// enroll refuses a store whose address, a database on an instance, another daemon's Deployment
// names too, as the census's readers of the daemon stores refuse it: with the project's Deployment
// and another project's naming its store's one database, enroll writes a stop entry naming both
// Deployments and both projects and recording the run unchanged, writes neither the store's marker
// nor the broker's cutover record, and the hold is released. Control: with the other Deployment
// naming another database, the same enroll is done and marks the store.
func TestEnrollRefusesAStoreAddressTwoDeploymentsName(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	const project, other = "ENRSHARE", "ENROTHER"
	db := h.database(daemonFamily(project), daemonMigrations...)
	h.replaceNotificationStream()
	h.enrollWindow()
	database := db.Config().ConnConfig.Database
	h.censusDeploy(censusDeploymentOf(project, db))
	sharer := censusDeployment(other, database, censusInstance)
	h.censusDeploy(sharer)
	h.enrollRequireStopped(project, enrollStore(t, project, db), daemonFamily(project)+"'s store is unreconciled, so enroll marks nothing",
		"legion/legion-daemon-enrshare (project ENRSHARE)", "legion/legion-daemon-enrother (project ENROTHER)",
		fmt.Sprintf("each name database %q on instance %q", database, censusInstance))

	// Control: the other Deployment names another database.
	deployments := h.s.Kube.Client.AppsV1().Deployments(writerhold.DaemonNamespace)
	rendered, err := deployments.Get(ctx, sharer.Name, metav1.GetOptions{})
	require.NoError(t, err)
	rendered.Annotations[writerhold.DaemonStoreDatabaseAnnotation] = "daemon_enrother"
	_, err = deployments.Update(ctx, rendered, metav1.UpdateOptions{})
	require.NoError(t, err)
	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, stdout, stderr := h.enrollRun(id, project)
	require.Equal(t, 0, code, "control: enroll beside a Deployment at another address: %s%s", stdout, stderr)
	state, err := enrollStore(t, project, db).State(ctx)
	require.NoError(t, err)
	require.Equal(t, id, state.Hold, "control: the enroll marked the store")
	require.NoError(t, h.release(id))
}

// enroll reads the daemon store by the census's rule for the daemon stores, at exactly the address
// the project's one source names, before it reads or writes anything else, and marks no other
// store: a project no source names (no Deployment and no retired entry), one whose Deployment names
// another database than its file reaches, and one whose store is gone at its Deployment's address
// each make enroll stop, naming the project and the source, with no marker and no broker cutover
// record written and the stop recording unchanged, and each hold is released. Control, in the same
// stack: a project whose Deployment names its store is enrolled. And 3f1eb2b20c's enroll, which
// opens the file's URL with no rule, marks the first two stores done and writes their cutover
// records, which no command removes.
func TestEnrollReadsItsStoreByTheAddressRule(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	h.replaceNotificationStream()
	h.enrollWindow()
	const ghost, wrong, gone, right = "ENRGHOST", "ENRWRONG", "ENRGONE", "ENRRIGHT"
	pools := map[string]*pgxpool.Pool{}
	for _, project := range []string{ghost, wrong, right} {
		pools[project] = h.database(daemonFamily(project), daemonMigrations...)
	}
	h.censusDeploy(censusDeployment(wrong, "daemon_elsewhere", censusInstance))
	dropped := censusDroppedDatabase()
	h.censusDeploy(censusDeployment(gone, dropped, censusInstance))
	files := map[string]string{
		ghost: ghost + "=" + h.enrollFile(pools[ghost]),
		wrong: wrong + "=" + h.enrollFile(pools[wrong]),
		gone:  censusStoreFileFor(t, gone, pools[right], dropped, censusInstance),
	}
	for _, c := range []struct{ project, why string }{
		{ghost, "0 Deployments of namespace legion name project ENRGHOST"},
		{wrong, fmt.Sprintf("reaches database %q, and Deployment legion/legion-daemon-enrwrong names database %q", pools[wrong].Config().ConnConfig.Database, "daemon_elsewhere")},
		{gone, daemonFamily(gone) + "'s store is gone at the address its Deployment names, so enroll marks nothing: database " + dropped},
	} {
		id := h.heldHold(holdrecord.WorkflowEnroll)
		code, stdout, stderr := h.run(actionEnroll, "--hold", id, "--daemon", c.project, "--daemon-database", files[c.project])
		require.Equal(t, 1, code, "enroll of %s: %s%s", c.project, stdout, stderr)
		require.Contains(t, stderr, c.why)
		stop := h.lastEntry(id, "")
		require.Equal(t, enrollCommand+" stop", stop.Command+" "+stop.State)
		require.Equal(t, holdrecord.LeftUnchanged, stop.Outputs[stopLeftKey], "the stop of enroll of %s", c.project)
		if pool, ok := pools[c.project]; ok {
			_, err := enrollStore(t, c.project, pool).State(ctx)
			require.ErrorIs(t, err, selection.ErrNoSentinel, "enroll of %s wrote the store's marker", c.project)
		}
		_, ok := h.cutoverRow(daemonFamily(c.project))
		require.False(t, ok, "enroll of %s wrote the broker's cutover record", c.project)
		require.NoError(t, h.release(id))
	}

	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, stdout, stderr := h.enrollRun(id, right)
	require.Equal(t, 0, code, "control: enroll of a store its Deployment names: %s%s", stdout, stderr)
	state, err := enrollStore(t, right, pools[right]).State(ctx)
	require.NoError(t, err)
	require.Equal(t, id, state.Hold, "control: the enroll marked the store")
	require.NoError(t, h.release(id))
}

// enroll reads the daemon store's checkpoints after its intent, and a consumer_positions table it
// cannot read (a stream_seq that is no number) is UNKNOWN, an exit after its intent: enroll writes
// its stop, recording what it left as unknown (leftUnknownPrefix), naming each reading it made and
// the one it could not make, never unchanged; hold release is refused over it, and hold abort is
// admitted and ends the hold, its intent carrying what the stop left. Control, run first: under the
// earlier rule that an UNKNOWN exit writes no stop (leftRuleSet.unknownNoStop), enroll's intent
// stays pending, and hold release and hold abort are both refused, so the UNKNOWN left the hold
// only enroll's rerun, which ended the same way.
func TestEnrollStopsOnAnUnknownRead(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	h.replaceNotificationStream()
	h.enrollWindow()
	const project = "ENRUNKN"
	family := daemonFamily(project)
	db := h.database(family, daemonMigrations[1])
	_, err := db.Exec(ctx, `create table consumer_positions (consumer text primary key, stream_seq text not null)`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `insert into consumer_positions (consumer, stream_seq) values ($1, 'no sequence')`, daemonFixedPrefix+project+"-wake")
	require.NoError(t, err)
	id := h.heldHold(holdrecord.WorkflowEnroll)
	release, abort := holdrecord.Request{Command: holdrecord.CommandRelease}, holdrecord.Request{Command: holdrecord.CommandAbort}
	unknown := func() holdrecord.Operation {
		t.Helper()
		code, _, stderr := h.enrollRun(id, project)
		require.Equal(t, 2, code, stderr)
		require.Contains(t, stderr, "UNKNOWN "+family+"'s checkpoints")
		return h.lastEntry(id, enrollCommand)
	}

	leftWith(t, leftRuleSet{unknownNoStop: true})
	require.Equal(t, "intent", unknown().State, "control: under the earlier rule the UNKNOWN leaves enroll's intent pending")
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrBusy, "control: hold release over the pending intent")
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), abort), holdrecord.ErrBusy, "control: hold abort over the pending intent")

	leftWith(t, leftRuleSet{})
	stop := unknown()
	require.Equal(t, "stop", stop.State, "the UNKNOWN after enroll's intent ends in its stop entry")
	left := stop.Outputs[stopLeftKey]
	require.True(t, strings.HasPrefix(left, leftUnknownPrefix), "the stop records %q", left)
	require.Contains(t, left, leftUnknownPrefix+"read "+family+"'s marker, at the address its Deployment names, the broker's cutover records, the consumers of "+notifications+
		"; unread "+family+"'s checkpoints: ", "the stop names what enroll read and what it could not read")
	require.Contains(t, left, "no run of enroll in this hold wrote this hold's store marker or the family's broker cutover record")
	require.Contains(t, left, "earlier runs of "+enrollCommand+" in this hold: run 1 ended with no entry", "the stop names the run the control left pending")
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), release), holdrecord.ErrOutOfOrder, "hold release over the UNKNOWN stop")
	inputs := h.abortHold(id)
	require.Contains(t, slices.Collect(maps.Values(inputs)), enrollCommand+": "+left, "hold abort's intent carries what the stop left")
	require.Equal(t, "released", string(h.row().Phase))
	_, err = enrollStore(t, project, db).State(ctx)
	require.ErrorIs(t, err, selection.ErrNoSentinel, "the UNKNOWN enroll wrote the store's marker")
}

// enrollStore is project's daemon store on db (pgFamilyOf of daemonFamily(project)), failing t when
// the library refuses to make it.
func enrollStore(t testing.TB, project string, db Database) *selection.PGStore {
	t.Helper()
	return pgFamilyOf(t, daemonFamily(project), db).PGStore
}
