package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tree of the test below (dispatch://INFRA-3178, the plan's list L2): a root and one child. The
// root's architect owns the child, and the child's implementer is a phase worker of it. A write in
// Dispatch names its session, and the daemon finds the writer's claim by that session alone.
const (
	ownerMoveRoot      = "LEGION-1"
	ownerMoveChild     = "LEGION-2"
	ownerMoveArchitect = "ses-root-architect"
	ownerMoveWorker    = "ses-child-implementer"
)

// setBackYieldedLine is the line the outbox logs when a set-back finishes without writing, because
// a status change came after the write it undoes.
const setBackYieldedLine = "outbox set-back finished without writing: the issue's status changed after the write it undoes"

// issueLogEntry is one event of the child's Dispatch event log: at seq, of type typ, by actor. status
// is the issue's status after the event, and changed the fields the write moved. changed is nil for
// a write that moved no field, a write of the status the issue already shows among them, since
// Dispatch then leaves the list out (its IssueEventPayload carries it with omitzero).
type issueLogEntry struct {
	seq     int64
	typ     string
	actor   dispatch.Actor
	status  string
	changed []string
}

// issueLogDispatch is the child as Dispatch shows it, from one event log. GetIssue answers the
// status of the last event and its sequence. SetStatusAt writes only at the sequence of the read it
// is given, and refuses a write at any other sequence with ISSUE_CHANGED, as Dispatch does.
// EventsAfter serves the log after a sequence in Dispatch's own shape. Each write the daemon makes
// is an event of its own session. onRead runs once, before the first GetIssue answers, and onWrite
// once, before the first SetStatusAt reads its sequence. A row sets one of them to add the owner's
// events while a set-back is in flight, its lease taken: before its read, or between its read and
// its write.
type issueLogDispatch struct {
	outboxDispatch
	log             []issueLogEntry
	onRead, onWrite func()
}

func (d *issueLogDispatch) last() issueLogEntry { return d.log[len(d.log)-1] }

// append adds the event of actor's write of status to the log, as Dispatch records it: a close of
// an open issue is issue.closed, every other write is issue.updated, and changed names the status,
// and closed_at across a close or a reopen, only when the status moved.
func (d *issueLogDispatch) append(actor dispatch.Actor, status string) issueLogEntry {
	before := d.last().status
	entry := issueLogEntry{seq: d.last().seq + 1, typ: "issue.updated", actor: actor, status: status}
	if status != before {
		entry.changed = []string{"status"}
		if status == record.DoneStatus || before == record.DoneStatus {
			entry.changed = append(entry.changed, "closed_at")
		}
	}
	if status == record.DoneStatus && before != record.DoneStatus {
		entry.typ = "issue.closed"
	}
	d.log = append(d.log, entry)
	return entry
}

// raw is entry as Dispatch's event log serves it, the shape its durable stream carries.
func (d *issueLogDispatch) raw(entry issueLogEntry) json.RawMessage {
	parent := ownerMoveRoot
	event := map[string]any{
		"id": entry.seq, "issue_key": ownerMoveChild, "project": "LEGION", "seq": entry.seq, "type": entry.typ,
		"actor": map[string]string{"kind": entry.actor.Kind, "id": entry.actor.ID}, "notify": false,
		"created_at": time.Date(2026, 11, 20, 0, 0, int(entry.seq), 0, time.UTC),
		"payload": map[string]any{
			"key": ownerMoveChild, "status": entry.status, "title": ownerMoveChild, "parent": parent, "rank": "B",
			"labels": []string{}, "resolved_repo": "acme/widgets", "blocked_by": []string{}, "claim": nil,
			"last_seq": entry.seq, "changed": entry.changed,
		},
	}
	if entry.changed == nil {
		delete(event["payload"].(map[string]any), "changed")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return raw
}

func (d *issueLogDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	if read := d.onRead; read != nil {
		d.onRead = nil
		read()
	}
	parent := ownerMoveRoot
	last := d.last()
	return dispatch.Issue{Key: ownerMoveChild, Status: last.status, Title: ownerMoveChild, Rank: "B", Parent: &parent,
		ResolvedRepo: "acme/widgets", LastSeq: last.seq}, nil
}

func (d *issueLogDispatch) SetStatusAt(_ context.Context, read dispatch.Issue, status string) (dispatch.Issue, error) {
	if write := d.onWrite; write != nil {
		d.onWrite = nil
		write()
	}
	if read.LastSeq != d.last().seq {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusConflict, Code: "ISSUE_CHANGED", Message: "issue changed"}
	}
	d.append(dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(ownerMoveChild)}, status)
	return d.GetIssue(context.Background(), ownerMoveChild)
}

func (d *issueLogDispatch) EventsAfter(_ context.Context, _ string, after int64) ([]json.RawMessage, error) {
	var events []json.RawMessage
	for _, entry := range d.log {
		if entry.seq > after {
			events = append(events, d.raw(entry))
		}
	}
	return events, nil
}

// ownerMoveRig is the tree the test runs, the daemon's two handlers that a Dispatch event goes
// through, and the outbox that runs the child's status rows against the fake Dispatch.
type ownerMoveRig struct {
	t         *testing.T
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	dispatch  *issueLogDispatch
	runner    *outbox
	logs      *testlog.Buffer
	// applied is the sequence of the last event of the log the handlers took.
	applied int64
}

// newOwnerMoveRig records the root and the child in implementing at in_progress, each holding its
// slot and the root's tree running, with the root's architect and the child's implementer claimed.
// Dispatch shows the child at in_progress, the daemon's own write at sequence 1, which the record
// took.
func newOwnerMoveRig(t *testing.T) *ownerMoveRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	admission := admit.New(records, engine, 10, "LEGION", quietLogger())
	parent := ownerMoveRoot
	for _, issue := range []record.Issue{
		{Key: ownerMoveRoot, Tree: ownerMoveRoot, HandedOver: true, Rank: "A"},
		{Key: ownerMoveChild, Tree: ownerMoveRoot, Parent: &parent, Rank: "B"},
	} {
		issue.Repo, issue.DefaultBranch, issue.GitHubRepo, issue.Project, issue.Title = "acme/widgets", "main", "acme/widgets", "LEGION", issue.Key
		issue.Phase, issue.Generation, issue.Status, issue.DispatchStatus, issue.LastDispatchSeq = phase.Implementing, 1, "in_progress", "in_progress", 1
		putOutboxIssue(t, pool, records, issue)
	}
	holdSlot(t, pool, ownerMoveRoot, ownerMoveChild)
	openTree(t, pool, "LEGION", ownerMoveRoot)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	for _, c := range []struct {
		issue   string
		role    claim.Role
		session string
	}{{ownerMoveRoot, claim.RoleArchitect, ownerMoveArchitect}, {ownerMoveChild, claim.RoleImplementer, ownerMoveWorker}} {
		token, err := claim.NewToken(project, c.issue, c.role)
		if err != nil {
			t.Fatalf("the %s claim token of %s: %v", c.role, c.issue, err)
		}
		if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, 1, $6, '', 'working', 0, 0, 0, 0)`,
			string(token), project, ownerMoveRoot, c.issue, string(c.role), c.session); err != nil {
			t.Fatalf("record the %s claim of %s: %v", c.role, c.issue, err)
		}
	}
	fake := &issueLogDispatch{log: []issueLogEntry{{seq: 1, typ: "issue.updated",
		actor: dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(ownerMoveChild)}, status: "in_progress", changed: []string{"status"}}}}
	logs := &testlog.Buffer{}
	return &ownerMoveRig{t: t, pool: pool, records: records, engine: engine, admission: admission, dispatch: fake, logs: logs, applied: 1,
		runner: &outbox{pool: pool, records: records, dispatch: fake, handlers: []intake.Handler{engine, admission},
			dispatchProject: "LEGION", log: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now}}
}

// write is session's write of status on the child: its event joins the log, and every event the
// handlers have not taken goes through them. It answers the event's sequence.
func (r *ownerMoveRig) write(session, status string) int64 {
	r.t.Helper()
	entry := r.dispatch.append(dispatch.Actor{Kind: "session", ID: session}, status)
	r.catchUp()
	return entry.seq
}

// catchUp applies each event of the log after the last one the handlers took, oldest first,
// decoded as the daemon's consumer decodes it, so the daemon's own writes come back with its actor.
func (r *ownerMoveRig) catchUp() {
	r.t.Helper()
	for _, entry := range r.dispatch.log {
		if entry.seq <= r.applied {
			continue
		}
		fact, _, err := intake.DecodeDispatchEvent("LEGION", ownerMoveChild, r.dispatch.raw(entry))
		if err != nil {
			r.t.Fatalf("decode event %d: %v", entry.seq, err)
		}
		result, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", fmt.Sprintf("%s-%d", ownerMoveChild, entry.seq), fact, r.engine, r.admission)
		if err != nil || result.Refusal != nil {
			r.t.Fatalf("apply event %d (%s of %s by %s) = %+v, %v", entry.seq, entry.typ, entry.status, entry.actor.Name(), result.Refusal, err)
		}
		r.applied = entry.seq
	}
}

// inFlight makes the child's next status row take its lease and then meet the owner's moves:
// before its first read when beforeRead holds, else between its read and its first write. When the
// moves run, first takes the sequence of the owner's first move.
func (r *ownerMoveRig) inFlight(beforeRead bool, first *int64, moves ...string) {
	r.t.Helper()
	owner := func() {
		for i, status := range moves {
			seq := r.write(ownerMoveArchitect, status)
			if i == 0 {
				*first = seq
			}
		}
	}
	if beforeRead {
		r.dispatch.onRead = owner
		return
	}
	r.dispatch.onWrite = owner
}

// run drains the child's status lane as the outbox runs it, then lets the handlers take the events
// the daemon's writes added. A row that set an in-flight order must have met it, or the row ran in
// another order than the one it names.
func (r *ownerMoveRig) run() {
	r.t.Helper()
	if drained, _ := r.runner.drain(context.Background(), ownerMoveChild, []record.OutboxKind{record.OutboxKindDispatchStatus}); !drained {
		r.t.Fatalf("the child's status lane did not drain: %s", r.logs.String())
	}
	if r.dispatch.onRead != nil || r.dispatch.onWrite != nil {
		r.t.Fatalf("no status row of the child made the read or the write this row's order needs the owner's moves at")
	}
	r.catchUp()
}

// statusWritesLeft is each status write the outbox still holds for the child, due or not, leased or
// not.
func (r *ownerMoveRig) statusWritesLeft() []record.StatusWrite {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select payload from outbox where issue = $1 and kind = $2 order by id`,
		ownerMoveChild, string(record.OutboxKindDispatchStatus))
	if err != nil {
		r.t.Fatalf("read the child's status rows: %v", err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		r.t.Fatalf("read the child's status rows: %v", err)
	}
	var writes []record.StatusWrite
	for _, payload := range payloads {
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindDispatchStatus, Issue: ownerMoveChild, Payload: payload})
		if err != nil {
			r.t.Fatalf("decode a status row of the child: %v", err)
		}
		writes = append(writes, decoded.(record.StatusWrite))
	}
	return writes
}

// daemonWritesAfter is each status the daemon wrote on the child after sequence seq.
func (r *ownerMoveRig) daemonWritesAfter(seq int64) []string {
	var writes []string
	for _, entry := range r.dispatch.log {
		if entry.seq > seq && entry.actor.ID == dispatch.DaemonSession(ownerMoveChild) {
			writes = append(writes, entry.status)
		}
	}
	return writes
}

// holds is the owner's status standing over its move at seq: the daemon wrote no status on the
// child after the move, Dispatch ends at the owner's status, the record reads it at phase done, and
// the outbox holds no status row that writes another status. Dispatch is read first, so a write
// over the move is named as the write, before the record that took its event back.
func (r *ownerMoveRig) holds(status string, move int64) {
	r.t.Helper()
	if writes := r.daemonWritesAfter(move); len(writes) != 0 {
		r.t.Fatalf("the daemon wrote %v on the child after the owner's move, want nothing: Dispatch ends at %s, and the owner's moves end at %s",
			writes, r.dispatch.last().status, status)
	}
	if shown := r.dispatch.last().status; shown != status {
		r.t.Fatalf("Dispatch ends with the child at %s, want the owner's %s", shown, status)
	}
	child := storedIssueOf(r.t, context.Background(), r.pool, r.records, ownerMoveChild)
	if child.Phase != phase.Done || child.Status != status {
		r.t.Fatalf("the child's record = phase %s, status %s, want the owner's %s at phase done", child.Phase, child.Status, status)
	}
	for _, write := range r.statusWritesLeft() {
		if write.Status != status {
			r.t.Fatalf("the outbox still holds %+v for the child, a write over the owner's %s", write, status)
		}
	}
}

// yielded is the set-back's one log line that it finished without writing, naming the owner's
// first move by its sequence and its session.
func (r *ownerMoveRig) yielded(move int64) {
	r.t.Helper()
	lines := loggedMessages(r.t, r.logs.String(), setBackYieldedLine)
	if len(lines) != 1 || lines[0]["issue"] != ownerMoveChild || lines[0]["seq"] != float64(move) || lines[0]["actor"] != "session:"+ownerMoveArchitect {
		r.t.Fatalf("the set-back logged %v, want one %q naming %s, the owner's move at %d and its session", lines, setBackYieldedLine, ownerMoveChild, move)
	}
}

// restored is the worker's status set back: Dispatch shows the record's in_progress again, the
// daemon wrote it once after seq, the record still runs the child, and the outbox holds no status
// row of it.
func (r *ownerMoveRig) restored(after int64) {
	r.t.Helper()
	if writes := r.daemonWritesAfter(after); !slices.Equal(writes, []string{"in_progress"}) {
		r.t.Fatalf("the daemon wrote %v on the child, want the record's in_progress once", writes)
	}
	if shown := r.dispatch.last().status; shown != "in_progress" {
		r.t.Fatalf("Dispatch ends with the child at %s, want the record's in_progress", shown)
	}
	child := storedIssueOf(r.t, context.Background(), r.pool, r.records, ownerMoveChild)
	if !workflow.InWorkflow(*child) || child.Phase != phase.Implementing || child.Status != "in_progress" {
		r.t.Fatalf("the child's record = phase %s, status %s, want it still implementing at in_progress", child.Phase, child.Status)
	}
	if left := r.statusWritesLeft(); len(left) != 0 {
		r.t.Fatalf("the outbox still holds %+v for the child, want nothing", left)
	}
}

// The owning architect's move of a child stands over every status write of the child that the
// daemon queued or has in flight when the move arrives (dispatch://INFRA-3178 R3, the plan's list
// L2). Each row puts the writes in its order through the daemon's two handlers, as the consumer
// applies Dispatch's events, and runs the outbox's status executor against a Dispatch that answers
// from one event log. "In flight" is a set-back whose lease the lane took before the owner's
// events reached the log: its read is taken before them (rows 3, 4, 6 and 9) or after them (row
// 5). Rows 1 to 3 hold through the drop of the child's status writes and the executor's own stop at
// a third status. Rows 4 to 6 end with Dispatch back at the status the set-back observed, so only
// the set-back's read of the events after its write stops it. Rows 7 to 9 hold that read to its
// narrow meaning: only a status change after the set-back's write cancels it, so a worker's own
// writes are still set back.
func TestTheOwningArchitectsDoneStandsOverAStatusWriteTheDaemonQueuedOrHasInFlight(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		run  func(r *ownerMoveRig)
	}{
		{"row 1: a phase status row queued, then the owner's done", func(r *ownerMoveRig) {
			// The workflow moved the child to testing and queued the write, which no lane has taken.
			if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
				child, err := r.records.Issue(ctx, tx, ownerMoveChild)
				if err != nil {
					return err
				}
				child.Phase, child.Status = phase.Testing, "testing"
				if err := r.records.PutIssue(ctx, tx, *child); err != nil {
					return err
				}
				queued, err := record.NewOutboxRow(ownerMoveChild, record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now())
				if err != nil {
					return err
				}
				return r.records.Enqueue(ctx, tx, queued)
			}); err != nil {
				r.t.Fatalf("queue the child's move to testing: %v", err)
			}
			move := r.write(ownerMoveArchitect, record.DoneStatus)
			r.run()
			r.holds(record.DoneStatus, move)
		}},
		{"row 2: a worker's backlog with its set-back queued, then the owner's done", func(r *ownerMoveRig) {
			r.write(ownerMoveWorker, "backlog")
			move := r.write(ownerMoveArchitect, record.DoneStatus)
			r.run()
			r.holds(record.DoneStatus, move)
		}},
		{"row 3: a worker's backlog with its set-back leased and its read taken, then the owner's done", func(r *ownerMoveRig) {
			r.write(ownerMoveWorker, "backlog")
			var move int64
			r.inFlight(false, &move, record.DoneStatus)
			r.run()
			r.holds(record.DoneStatus, move)
		}},
		{"row 4: a worker's backlog with its set-back leased and its read taken, then the owner's icebox and backlog", func(r *ownerMoveRig) {
			r.write(ownerMoveWorker, "backlog")
			var move int64
			r.inFlight(false, &move, "icebox", "backlog")
			r.run()
			r.holds("backlog", move)
			r.yielded(move)
		}},
		{"row 5: a worker's backlog with its set-back leased, the owner's icebox and backlog, then its first read", func(r *ownerMoveRig) {
			r.write(ownerMoveWorker, "backlog")
			var move int64
			r.inFlight(true, &move, "icebox", "backlog")
			r.run()
			r.holds("backlog", move)
			r.yielded(move)
		}},
		{"row 6: a worker's backlog with its set-back leased and its read taken, then the owner's done and backlog", func(r *ownerMoveRig) {
			r.write(ownerMoveWorker, "backlog")
			var move int64
			r.inFlight(false, &move, record.DoneStatus, "backlog")
			r.run()
			r.holds("backlog", move)
			r.yielded(move)
		}},
		{"row 7: a worker's backlog, then the worker's done", func(r *ownerMoveRig) {
			first := r.write(ownerMoveWorker, "backlog")
			r.write(ownerMoveWorker, record.DoneStatus)
			r.run()
			r.restored(first)
		}},
		{"row 8: a worker's backlog, then the worker's backlog again", func(r *ownerMoveRig) {
			first := r.write(ownerMoveWorker, "backlog")
			r.write(ownerMoveWorker, "backlog")
			r.run()
			r.restored(first)
		}},
		{"row 9: a worker's backlog with its set-back in flight, the owner's backlog, then the owner's done", func(r *ownerMoveRig) {
			first := r.write(ownerMoveWorker, "backlog")
			var same int64
			r.inFlight(false, &same, "backlog")
			r.run()
			r.restored(first)
			move := r.write(ownerMoveArchitect, record.DoneStatus)
			r.run()
			r.holds(record.DoneStatus, move)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.run(newOwnerMoveRig(t))
		})
	}
}
