package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// Database is what a command runs its statements on in a family's Postgres store: a
// *pgxpool.Pool, or a test's.
type Database interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// env is what a run reaches outside the process. Command gives the real one (realEnv); a test
// gives its own.
type env struct {
	// records opens the operator's client of the broker's hold routes for o, with the signing key
	// when signing.
	records func(ctx context.Context, o options, signing bool) (*holdrecord.Client, error)
	// connect opens the operator's NATS connection to o's hub, its replies under
	// natsgrant.OperatorInbox.
	connect func(ctx context.Context, o options) (*nats.Conn, error)
	// database opens the Postgres store of family: "dispatch", or "daemon/<project key>", and
	// answers with it the close of what the open made, which the run calls when it ends: realEnv's
	// pool (openDatabase). nil is a store the caller owns and closes, as a test's.
	database func(ctx context.Context, o options, family string) (Database, func(), error)
	// hub opens the operator's reach of the clustered hub for o (writerhold.Operator): its
	// Kubernetes objects and each declared server's monitoring port, which the held re-read
	// (heldReread), convert's meta barrier (writerhold.MetaBarrier) and restore --finish's restart
	// read; and the daemons' Deployments and the retired projects' entries
	// (writerhold.KubeDaemonDeployments), which every reader of a daemon store reads by the address
	// rule (daemonStoresByRule), status's gone-store rule among them.
	hub func(ctx context.Context, o options) (*writerhold.Operator, error)
	// reread, when set, replaces the held re-read (heldReread): nil, as realEnv leaves it, is the
	// re-read. Only the grant rows, whose hubs of static users have no callout stack, set it.
	reread func(ctx context.Context, r *run) error
	now    func() time.Time
}

// run is one command's reach: its options, its env, and what it opened, each opened once, when
// first needed, and closed when the command ends (close).
type run struct {
	o              options
	e              env
	stdout, stderr io.Writer

	records  *holdrecord.Client
	nc       *nats.Conn
	js       jetstream.JetStream
	operator *writerhold.Operator
	// heldStreams are the protected streams the latest held re-read read, which a command's
	// postcheck compares against (writerhold.Postcheck's step 1).
	heldStreams []writerhold.StreamReading
	databases   map[string]Database
	// closeDatabases are the closes env.database answered for the stores it opened.
	closeDatabases []func()
	// reads name what a held command read since its intent, in order (read): what the stop of an
	// UNKNOWN exit after it records as read (leftUnknown).
	reads []string
}

// read records that a held command read what since its intent, for the stop of an UNKNOWN exit
// after it (leftUnknown).
func (r *run) read(what string) { r.reads = append(r.reads, what) }

// close closes the run's operator connection, the latest one reconnect made, and every store it
// opened (env.database).
func (r *run) close() {
	for _, closeDatabase := range r.closeDatabases {
		closeDatabase()
	}
	if r.nc != nil {
		r.nc.Close()
	}
}

func (r *run) now() time.Time {
	if r.e.now != nil {
		return r.e.now()
	}
	return time.Now()
}

// hold is the run's client of the broker's hold routes, opened with the signing key for every
// command but status.
func (r *run) hold(ctx context.Context) (*holdrecord.Client, error) {
	if r.records == nil {
		records, err := r.e.records(ctx, r.o, r.o.action != actionStatus)
		if err != nil {
			return nil, err
		}
		r.records = records
	}
	return r.records, nil
}

// readHoldRecord reads hold holdID's record of kind and key (holdrecord.Client.Records, which
// verifies each record) into a T, with its gen, which a write of it is conditional on, and false
// when the hold holds none. A value that does not decode is UNKNOWN, naming what.
func readHoldRecord[T any](ctx context.Context, records *holdrecord.Client, holdID, kind, key, what string) (T, uint64, bool, error) {
	var zero T
	list, err := records.Records(ctx, holdID, kind)
	if err != nil {
		return zero, 0, false, err
	}
	for _, record := range list {
		if record.Key != key {
			continue
		}
		var value T
		if err := json.Unmarshal(record.Value, &value); err != nil {
			return zero, 0, false, unknownf(err, "%s", what)
		}
		return value, record.Gen, true, nil
	}
	return zero, 0, false, nil
}

// conn is the run's operator connection to the hub, and its JetStream.
func (r *run) conn(ctx context.Context) (*nats.Conn, jetstream.JetStream, error) {
	if r.nc == nil {
		nc, err := r.e.connect(ctx, r.o)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to the hub as the operator: %w", err)
		}
		js, err := jetstream.New(nc)
		if err != nil {
			nc.Close()
			return nil, nil, err
		}
		r.nc, r.js = nc, js
	}
	return r.nc, r.js, nil
}

// reconnectWithin bounds how long reconnect connects again, while the hub's auth service, itself
// reconnecting after a server's restart, may refuse the operator.
const reconnectWithin = time.Minute

// reconnect replaces the run's operator connection with one made now, for a held command after it
// restarted the hub's servers (writerhold's restart, proof and barrier): nats.go closes a
// connection whose reconnects the hub refused twice in a row, and a restart refuses every
// reconnect until the auth service is back. It connects every 250 ms until one connect is
// admitted, and is UNKNOWN once reconnectWithin passes. What the caller made on the old connection
// (a store over it, a bucket barrier) it makes again.
func (r *run) reconnect(ctx context.Context) (*nats.Conn, jetstream.JetStream, error) {
	if r.nc != nil {
		r.nc.Close()
	}
	r.nc, r.js = nil, nil
	deadline := time.Now().Add(reconnectWithin)
	for {
		nc, js, err := r.conn(ctx)
		if err == nil {
			return nc, js, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, unknownf(err, "connect to the hub as the operator again, for %s", reconnectWithin)
		}
		if sleep(ctx, 250*time.Millisecond) != nil {
			return nil, nil, unknownf(ctx.Err(), "connect to the hub as the operator again")
		}
	}
}

// operatorHub is the run's reach of the clustered hub (env.hub), opened once.
func (r *run) operatorHub(ctx context.Context) (*writerhold.Operator, error) {
	if r.operator == nil {
		if r.e.hub == nil {
			return nil, errors.New("this run has no reach of the hub's Kubernetes objects")
		}
		op, err := r.e.hub(ctx, r.o)
		if err != nil {
			return nil, fmt.Errorf("the operator's reach of the hub: %w", err)
		}
		r.operator = op
	}
	return r.operator, nil
}

// database is family's Postgres store, opened once, and closed when the run ends (close).
func (r *run) database(ctx context.Context, family string) (Database, error) {
	if db, ok := r.databases[family]; ok {
		return db, nil
	}
	db, closeDatabase, err := r.e.database(ctx, r.o, family)
	if err != nil {
		return nil, err
	}
	if closeDatabase != nil {
		r.closeDatabases = append(r.closeDatabases, closeDatabase)
	}
	if r.databases == nil {
		r.databases = map[string]Database{}
	}
	r.databases[family] = db
	return db, nil
}

// requireRow reads the hold row and refuses, naming the phase it found, unless it is phase, and,
// for prepare and held, under the run's hold id: each operator command runs only in the one phase
// it needs, so a deploy during a hold changes nothing. A held command that ran the held re-read
// when it started and has only read since checks the row with it alone before its intent
// (admitRead).
func (r *run) requireRow(ctx context.Context, phase natsgrant.OperatorPhase) (holdrecord.Row, error) {
	records, err := r.hold(ctx)
	if err != nil {
		return holdrecord.Row{}, err
	}
	row, err := records.Row(ctx)
	if err != nil {
		return holdrecord.Row{}, err
	}
	if row.Phase != phase {
		return row, fmt.Errorf("%s runs while the hold row is %s, and it is %s (hold %q)", r.o.action, phase, row.Phase, row.HoldID)
	}
	if phase != natsgrant.OperatorPhaseReleased && row.HoldID != r.o.holdID {
		return row, fmt.Errorf("the hold row is %s under hold %q, not under --hold %q", row.Phase, row.HoldID, r.o.holdID)
	}
	return row, nil
}

// requirePhase is requireRow, and for held then the held re-read (heldReread), which reads the
// hold's journal, before the command does anything else.
func (r *run) requirePhase(ctx context.Context, phase natsgrant.OperatorPhase) (holdrecord.Row, error) {
	row, err := r.requireRow(ctx, phase)
	if err != nil || phase != natsgrant.OperatorPhaseHeld {
		return row, err
	}
	return row, r.heldReread(ctx)
}

// The held re-read's writer census: read again while its answer is not whole, at most
// heldCensusReadings times, heldCensusPause apart (writerhold's hold set, wholeCensus).
const (
	heldCensusReadings = 10
	heldCensusPause    = 3 * time.Second
)

// heldReread is the rule every held command keeps when it starts and before its mutation steps
// (plan §3a, "Every held command"): it reads again step 1's server and stream reads, step 2's
// writer census and step 6's report, as writerhold's hold set reads them (hold.go Set), on the
// command's own connection:
//
//   - the render's precondition, which names the auth service's user (writerhold.Precondition),
//     and every declared server, its identity and its routes, leafnodes and gateways
//     (writerhold.Discover);
//   - the restart: every declared pod must be one hold set restarted in this hold, by the server
//     ids its done entry records (server_id.<pod>). A server's id is new at each start, so an id
//     other than the recorded one is a start since, still inside the hold; a pod hold set did not
//     restart is UNKNOWN;
//   - every protected stream by name, absent only where the hold's workflow declares it
//     (writerhold.ReadProtectedStreams), and the writer census over the ones present
//     (writerhold.TakeCensus), read again while it is not whole;
//   - the report: every connection of every declared server is the command's own or the auth
//     service's (writerhold.Report). Its restart check compares step 1's ids, which the journal
//     does not keep; the restart is the check above, so the report is given ids no server
//     answers, and checks the connections, the routes, the leafnodes and the gateways.
//
// A refusal or an UNKNOWN of any of them stops the command before it writes anything.
func (r *run) heldReread(ctx context.Context) error {
	if r.e.reread != nil {
		return r.e.reread(ctx, r)
	}
	d, _, err := r.records.Journal(ctx, r.o.holdID)
	if err != nil {
		return err
	}
	op, err := r.operatorHub(ctx)
	if err != nil {
		return err
	}
	nc, _, err := r.conn(ctx)
	if err != nil {
		return unknownf(err, "the operator's connection")
	}
	render, err := writerhold.Precondition(ctx, op.Hub)
	if err != nil {
		return hubError(err, "the held re-read, the hub's render")
	}
	declared, err := writerhold.Discover(ctx, op.Hub)
	if err != nil {
		return hubError(err, "the held re-read, the declared servers")
	}
	restarted := heldRestarted(d)
	report := make(writerhold.Declared, len(declared))
	for i, server := range declared {
		if _, ok := restarted[server.Pod]; !ok {
			return unknownf(nil, "declared server %s (%s) is not one hold set restarted in hold %s: its done entry records no server_id.%s",
				server.Pod, server.Varz.ServerID, r.o.holdID, server.Pod)
		}
		report[i] = server
		report[i].Varz.ServerID = "the process " + server.Pod + " ran before hold set's restart"
	}
	streams, err := writerhold.ReadProtectedStreams(ctx, nc, heldExpectedAbsent(d.Workflow))
	if err != nil {
		return hubError(err, "the held re-read, the protected streams")
	}
	r.heldStreams = streams
	var present []string
	for _, stream := range streams {
		if !stream.Absent {
			present = append(present, stream.Name)
		}
	}
	for reading := 1; ; reading++ {
		_, err := writerhold.TakeCensus(ctx, nc, present)
		var unknown *writerhold.Unknown
		if err == nil {
			break
		}
		if !errors.As(err, &unknown) || reading == heldCensusReadings {
			return hubError(err, "the held re-read, the writer census")
		}
		if sleep(ctx, heldCensusPause) != nil {
			return unknownf(ctx.Err(), "the writer census")
		}
	}
	if _, err := (writerhold.Report{Hub: op.Hub, Declared: report, AuthUser: render.AuthUser, Wait: op.Wait}).Run(ctx, nc); err != nil {
		return hubError(err, "the held re-read, the report")
	}
	return nil
}

// heldRestarted is each pod's server id after hold set's restart, from its latest done entry's
// outputs (server_id.<pod>).
func heldRestarted(d holdrecord.Done) map[string]string {
	ids := map[string]string{}
	for _, entry := range d.Ops {
		if entry.Command != holdrecord.CommandSet || entry.State != "done" {
			continue
		}
		ids = map[string]string{}
		for key, id := range entry.Outputs {
			if pod, ok := strings.CutPrefix(key, "server_id."); ok {
				ids[pod] = id
			}
		}
	}
	return ids
}

// heldExpectedAbsent are the protected streams w's commands may find absent, as hold set's
// preflight declares them (writerhold hold.go expectedAbsent): the selection bucket in the
// cutover, until create-bucket makes it, and in a selection recovery, until create-bucket
// --after-loss does; every protected stream in streams-update.
func heldExpectedAbsent(w holdrecord.Workflow) []string {
	switch w {
	case holdrecord.WorkflowCutover, holdrecord.WorkflowSelectionRecovery:
		return []string{selectionStreamName}
	case holdrecord.WorkflowStreamsUpdate:
		return writerhold.ProtectedStreams()
	}
	return nil
}

// hubError is err, a writerhold reading or step that failed, as this package answers it: its
// UNKNOWN (writerhold.Unknown) stays UNKNOWN here (exit 2), and its refusal a refusal, each naming
// what.
func hubError(err error, what string) error {
	if errors.As(err, new(*writerhold.Unknown)) {
		return unknownf(err, "%s", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// hubUnknown is hubError with a refusal answered as it is: a caller that names its refusal
// itself, or answers it to its operator in the hub's own words (restore, behindBarrier).
func hubUnknown(err error, what string) error {
	if errors.As(err, new(*writerhold.Unknown)) {
		return unknownf(err, "%s", what)
	}
	return err
}

// sleep waits d, and answers ctx's error when ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// admission is a held command admitted to its hold: its intent entry is written, and it ends
// with its done entry or its stop entry. Every exit after the intent writes one of them (end,
// stopped): a refusal, an UNKNOWN and a done entry whose write failed alike, so no run leaves its
// intent pending but one cut short before it can write (a kill, or a broker it can no longer
// reach).
type admission struct {
	r        *run
	command  string
	workflow holdrecord.Workflow
	// barrier is set by a command whose own account of what it left is plain text (cleanup,
	// replace) right before it puts this hold's barrier key (behindBarrier): from then on its stop
	// counts the key as a change (left, barrierLeft), since none of those commands deletes the
	// bucket.
	barrier bool
	// ended is set once the run wrote its done entry or tried its stop entry: end then writes no
	// other stop.
	ended bool
}

// admit reads the row, which must be held under the run's hold id, runs the held re-read
// (requirePhase, heldReread), and writes command's intent entry (intent).
func (r *run) admit(ctx context.Context, command string, inputs map[string]string) (*admission, error) {
	if _, err := r.requirePhase(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return nil, err
	}
	return r.intent(ctx, command, inputs)
}

// admitRead is admit for a command that ran the held re-read when it started (requirePhase) and
// has only read since what can refuse it before its intent (create-bucket, replay,
// repair-sentinel): it reads the row again, which must still be held under the run's hold id
// (requireRow), and writes the intent with no second re-read, which would read again what the
// first read with nothing changed between. The re-read still comes before the command's first
// change, the intent.
func (r *run) admitRead(ctx context.Context, command string, inputs map[string]string) (*admission, error) {
	if _, err := r.requireRow(ctx, natsgrant.OperatorPhaseHeld); err != nil {
		return nil, err
	}
	return r.intent(ctx, command, inputs)
}

// intent writes command's intent entry with inputs (holdrecord.Client.Admit): the decision and
// the entry are one write.
func (r *run) intent(ctx context.Context, command string, inputs map[string]string) (*admission, error) {
	w, err := r.records.Admit(ctx, r.o.holdID, holdrecord.Request{Command: command}, inputs)
	if err != nil {
		return nil, err
	}
	r.reads = nil
	return &admission{r: r, command: command, workflow: w}, nil
}

// reread is the held re-read again (heldReread), which an admitted command runs before each of
// its mutation steps. Its refusal or UNKNOWN is an exit after the intent like any other, which the
// command ends with its stop (end, stopped); the way out is then the rerun, once whatever it found
// is gone, or hold abort.
func (a *admission) reread(ctx context.Context) error {
	if err := a.r.heldReread(ctx); err != nil {
		return err
	}
	a.r.read("the held re-read")
	return nil
}

// done writes the command's done entry with outputs (holdrecord.Client.AppendDone). A write that
// fails is UNKNOWN, as it may have landed: its caller ends it with the command's stop like every
// other exit after the intent, and a done entry that did land refuses that stop
// (holdrecord.ErrNoIntent), which the stop's caller names after the error.
func (a *admission) done(ctx context.Context, outputs map[string]string) error {
	if err := a.r.records.AppendDone(ctx, a.r.o.holdID, a.command, outputs); err != nil {
		return unknownf(err, "whether %s's done entry was written", a.command)
	}
	a.ended = true
	return nil
}

// stop writes the command's stop entry with reason and left, what every run of the command in this
// hold changed outside the journal: holdrecord.LeftUnchanged when none changed anything, which is
// the only stop hold release is admitted over; anything else counts as changed, and the way out is
// the command's rerun or hold abort (holdrecord.Client.AppendStopLeft). It writes the entry when
// the command is Stoppable in its workflow, and answers reason as the command's refusal; where it
// is not Stoppable, the intent stays pending, so only its rerun is admitted (holdrecord.ErrBusy for
// any other).
func (a *admission) stop(ctx context.Context, reason, left string) error {
	a.ended = true
	refusal := errors.New(reason)
	if !holdrecord.Stoppable(a.workflow, a.command) {
		return refusal
	}
	if err := a.r.records.AppendStopLeft(ctx, a.r.o.holdID, a.command, reason, left); err != nil {
		return fmt.Errorf("%s; and its stop entry: %w", reason, err)
	}
	return refusal
}

// end ends err, how a's command's work after its intent ended: as it is when it is nil, or when
// the work wrote its done entry or tried its stop entry itself; else with a's stop (stopped), which
// records this run's (unchanged, account) among what every run of the command left. So an exit
// that no step of the command stops for itself, an UNKNOWN reading among them, still ends in the
// command's stop, and the hold keeps its way out.
func (a *admission) end(ctx context.Context, err error, unchanged bool, account string) error {
	if err == nil || a.ended {
		return err
	}
	return a.stopped(ctx, err, unchanged, account)
}

// stopped ends err, a refusal or an UNKNOWN after a's intent, with a's stop (stop), which records
// what every run of a's command in this hold left (leftAfter: unchanged when this run changed
// nothing outside the journal, and account what it did change), and answers err as it is, so an
// UNKNOWN stays UNKNOWN; a stop entry the broker refused is named after it.
func (a *admission) stopped(ctx context.Context, err error, unchanged bool, account string) error {
	if !stopsAfter(err) {
		return err
	}
	if serr := a.stop(ctx, err.Error(), a.leftAfter(ctx, err, unchanged, account)); errors.Unwrap(serr) != nil {
		return fmt.Errorf("%w; and its stop entry: %w", err, errors.Unwrap(serr))
	}
	return err
}

// stopsAfter reports whether err, an exit after a command's intent, writes the command's stop: it
// does, but under the earlier rule a row's control sets (leftRuleSet.unknownNoStop), where an
// UNKNOWN wrote none.
func stopsAfter(err error) bool { return !leftRules.unknownNoStop || !isUnknown(err) }

// leftAfter is what a's stop after err records as left (left): for an UNKNOWN (isUnknown), this
// run's account under leftUnknown, which is never holdrecord.LeftUnchanged; for any other, this
// run's (unchanged, account).
func (a *admission) leftAfter(ctx context.Context, err error, unchanged bool, account string) string {
	if isUnknown(err) && !leftRules.unknownAsAccount {
		return a.left(ctx, false, leftUnknown(a.r.reads, err, account))
	}
	return a.left(ctx, unchanged, account)
}

// leftUnknown is a stop's account of what its run left after err, an UNKNOWN exit:
// leftUnknownPrefix, then `read` and what the run read since its intent (reads, run.read), then
// `unread` and what it could not read (err), then account, what the run changed. A run that could
// not read part of what it acts on cannot say what it left there, so its stop counts as changed
// (holdrecord's rule admits hold release only over stops that recorded holdrecord.LeftUnchanged),
// and the way out is the command's rerun to its done entry, or hold abort.
func leftUnknown(reads []string, err error, account string) string {
	read := "nothing since its intent"
	if len(reads) > 0 {
		read = listNames(reads)
	}
	return leftUnknownPrefix + "read " + read + "; unread " + strings.TrimPrefix(err.Error(), "UNKNOWN ") + "; " + account
}

// left is what a's stop records as left: what every run of a's command in this hold left
// (journalLeft over the hold's journal), this run's (unchanged, account) among them, and this
// hold's barrier key when this run put it (barrier, barrierLeft), which then counts as a change
// named after account. A journal read that fails is never taken as unchanged.
func (a *admission) left(ctx context.Context, unchanged bool, account string) string {
	if a.barrier && !leftRules.barrierNoChange {
		unchanged, account = false, account+"; it put "+barrierLeft
	}
	d, _, err := a.r.records.Journal(ctx, a.r.o.holdID)
	if err != nil {
		return fmt.Sprintf("%s; what earlier runs of %s left is unknown: the hold's journal could not be read (%v)", account, a.command, err)
	}
	return journalLeft(d, a.command, unchanged, account)
}

// barrierLeft names, in a stop's account of what its run left, this hold's barrier key in the
// selection bucket, hold.barrier.<hold id>, which every held reader of the bucket puts before it
// reads (writerhold.ReadBucket, holdrecord.Barrier). The key is a change of the command whose run
// put it, as long as it is there: only a delete of the whole bucket takes it away (create-bucket
// --after-loss deletes a leftover bucket), and a key that another command of the hold put is no
// change of this one.
const barrierLeft = "this hold's barrier key in " + selectionStreamName

// leftRuleSet is the rule a stop's record of what its command left follows. The zero value is the
// hold's: a stop names every change that its command's runs made and did not undo, this hold's
// barrier key among them while a run of the command put it and the bucket that holds it stands.
// Each field is an earlier rule, which only a row's control sets, to show that the row fails under
// it.
type leftRuleSet struct {
	// barrierNoChange: the barrier key, the hold's own, is no change of the command that put it
	// (cleanup, replace and the restore commands before this rule).
	barrierNoChange bool
	// ownRunOnly: a create-bucket, replay or repair-sentinel stop records its own run alone, its
	// command's earlier runs in the hold aside (internal/holdrecord's model names this control
	// OwnRunOnly).
	ownRunOnly bool
	// barrierAfterDelete: the barrier key counts once the run put it, though the run deleted the
	// bucket that held it after (create-bucket --after-loss before this rule).
	barrierAfterDelete bool
	// unknownNoStop: an UNKNOWN exit after its command's intent writes no stop entry, so the intent
	// stays pending and the hold admits the command's rerun alone, hold release and hold abort
	// refused (convert's and enroll's UNKNOWN exits, and every command's lost done entry, before
	// this rule).
	unknownNoStop bool
	// unknownAsAccount: an UNKNOWN exit's stop records its run's own account as a refusal's stop
	// does, holdrecord.LeftUnchanged when the run changed nothing (cleanup's, replace's,
	// create-bucket's, replay's, repair-sentinel's and the restore commands' UNKNOWN exits before
	// this rule).
	unknownAsAccount bool
}

// leftUnknownPrefix begins what the stop of an UNKNOWN exit records as left (leftUnknown), the one
// mark every stop of this package's commands gives such a stop, which never reads
// holdrecord.LeftUnchanged.
const leftUnknownPrefix = "unknown: "

// leftRules is the rule every stop of this package follows; a row's control alone changes it.
var leftRules leftRuleSet

// stopLeftKey is the key of a stop entry's outputs that holds what its command left, as
// holdrecord's AppendStopLeft writes it (internal/holdrecord/journal.go, stopLeftKey, which
// holdrecord does not export).
const stopLeftKey = "left"

// journalLeft is what a stop of command records as left, from d, the hold's journal with this
// run's intent as command's latest entry, and this run's own account: unchanged when it changed
// nothing outside the journal, and account, which names what it changed or that it changed
// nothing. It is holdrecord.LeftUnchanged only when this run changed nothing and each earlier run
// of command in this hold ended in a stop that recorded unchanged. A done entry and a run cut after
// its intent (an intent the command's next intent follows) count as changes. Otherwise it is
// account and how each earlier run ended; an earlier stop's own account is in that stop entry, so
// it is named, not copied, and the text does not grow with each rerun.
func journalLeft(d holdrecord.Done, command string, unchanged bool, account string) string {
	var ops []holdrecord.Operation
	for _, op := range d.Ops {
		if op.Command == command {
			ops = append(ops, op)
		}
	}
	// The latest entry is this run's intent; every intent before it began an earlier run.
	if n := len(ops); n > 0 && ops[n-1].State == "intent" {
		ops = ops[:n-1]
	}
	var earlier []string
	for i, op := range ops {
		if op.State != "intent" {
			continue
		}
		end, leftNothing := "ended with no entry (cut after its intent), and the records hold what it wrote", false
		if i+1 < len(ops) {
			switch next := ops[i+1]; {
			case next.State == "stop" && next.Outputs[stopLeftKey] == holdrecord.LeftUnchanged:
				end, leftNothing = "stopped, and its stop recorded it left nothing changed", true
			case next.State == "stop" && next.Outputs[stopLeftKey] != "":
				end = "stopped, and its stop entry records what it left"
			case next.State == "stop":
				end = "stopped, and its stop recorded nothing of what it left"
			case next.State == "done":
				end = "is done"
			}
		}
		unchanged = unchanged && leftNothing
		earlier = append(earlier, fmt.Sprintf("run %d %s", len(earlier)+1, end))
	}
	switch {
	case unchanged:
		return holdrecord.LeftUnchanged
	case len(earlier) == 0:
		return account
	}
	return account + "; earlier runs of " + command + " in this hold: " + listNames(earlier)
}

// leftListed bounds how many names each list of a stop entry's account names; the rest are
// counted.
const leftListed = 32

// listNames is names joined, at most leftListed of them, and how many more.
func listNames(names []string) string {
	if len(names) <= leftListed {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:leftListed], ", "), len(names)-leftListed)
}

// realEnv is the env of a real run: the broker's routes with the operator's token and the hold
// key from Secret Manager, the hub as the operator, and each family's database from the file its
// flag names, all from the hold's flags as writerhold reads them (writerhold.HubFlags).
func realEnv() env { return envFromFlags(realHubOpeners) }

// hubOpeners are how envFromFlags reaches the broker's hold routes, the hub's NATS and the hub's
// Kubernetes objects from a run's writerhold.HubFlags.
type hubOpeners struct {
	records func(h writerhold.HubFlags, ctx context.Context, signing bool) (*holdrecord.Client, error)
	connect func(h writerhold.HubFlags) (*nats.Conn, error)
	hub     func(h writerhold.HubFlags, ctx context.Context) (*writerhold.Operator, error)
}

// realHubOpeners are writerhold's own: HubFlags.Records (Secret Manager's signing key when
// signing), ConnectOperator and HubFlags.Operator (the in-cluster configuration or KUBECONFIG's, and
// the Compute API for the hub's disks).
var realHubOpeners = hubOpeners{
	records: writerhold.HubFlags.Records,
	connect: func(h writerhold.HubFlags) (*nats.Conn, error) {
		return writerhold.ConnectOperator(h.NATS, h.TokenFile, "envoy-infra consumer-names")
	},
	hub: writerhold.HubFlags.Operator,
}

// envFromFlags is the env that reaches everything a run's flags name through open, and each
// family's database from the file its flag names (openDatabase), closed when the run ends.
func envFromFlags(open hubOpeners) env {
	return env{
		records: func(ctx context.Context, o options, signing bool) (*holdrecord.Client, error) {
			return open.records(o.HubFlags, ctx, signing)
		},
		connect: func(_ context.Context, o options) (*nats.Conn, error) { return open.connect(o.HubFlags) },
		database: func(ctx context.Context, o options, family string) (Database, func(), error) {
			pool, err := openDatabase(ctx, o, family)
			if err != nil {
				return nil, nil, err
			}
			return pool, pool.Close, nil
		},
		hub: func(ctx context.Context, o options) (*writerhold.Operator, error) {
			return open.hub(o.HubFlags, ctx)
		},
		now: time.Now,
	}
}

// openDatabase opens a pool on family's database from the URL its file holds
// (familyDatabaseFile), which its caller closes.
func openDatabase(ctx context.Context, o options, family string) (*pgxpool.Pool, error) {
	f, err := familyDatabaseFile(o, family)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, f.url)
	if err != nil {
		return nil, fmt.Errorf("open %s's database (%s; the URL is not echoed): %w", family, f.flag, err)
	}
	return pool, nil
}

// databaseFile is a family's database file as its flag names it. Its first line is the database's
// URL with the operator's credential, which every command opens; a daemon's file has a second, the
// connection name of the Cloud SQL instance the operator's own proxy reaches with that URL, which
// every reader of a daemon store compares with the daemon's one address source, its Deployment or
// its entry of the retired projects' ConfigMap (censusReadDaemonStores, daemonStoresByRule); status
// reads it only for a daemon store whose database answers that it does not exist (statusGone). A
// file of one line serves status otherwise.
type databaseFile struct {
	// flag names the flag and value that named the file, as an error names it.
	flag string
	// url is the first line; instance the second, "" when the file has none.
	url, instance string
}

// familyDatabaseFile reads family's database file from the flag that names it:
// --dispatch-database-file for Dispatch, --daemon-database <project>=<file> for a daemon. A file
// whose first line holds no URL is refused: a pool opened on an empty URL would reach whatever
// server the environment names.
func familyDatabaseFile(o options, family string) (databaseFile, error) {
	file, flagName := o.storeFile(family)
	if file == "" {
		return databaseFile{}, fmt.Errorf("%s's store needs %s", family, flagName)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return databaseFile{}, fmt.Errorf("read %s: %w", flagName, err)
	}
	first, rest, _ := strings.Cut(string(raw), "\n")
	second, _, _ := strings.Cut(rest, "\n")
	f := databaseFile{flag: flagName, url: strings.TrimSpace(first), instance: strings.TrimSpace(second)}
	if f.url == "" {
		return databaseFile{}, fmt.Errorf("%s: its file's first line holds no database URL", flagName)
	}
	return f, nil
}
