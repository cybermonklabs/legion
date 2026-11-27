package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The writers of the child's Dispatch event log in the tests below: a person, the parent's
// implementer, whose session holds its claim on the parent (transitionAgent), and the daemon's own
// session, whose release_children and rerun_child write a child's todo.
var (
	transitionPerson = dispatch.Actor{Kind: "user", ID: "a-person"}
	transitionAgent  = dispatch.Actor{Kind: "session", ID: "ses-parent-implementer"}
	transitionDaemon = dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(ownerMoveChild)}
)

// The lines the workflow logs as it judges an unrecorded child's todo (workflow's
// sessionTodoOnChild).
const (
	childSetBackLine   = "workflow: a session of the tree wrote todo on a child the daemon does not record; it starts nothing, and the daemon sets it back"
	childNoBeforeLine  = "workflow: a session of the tree wrote todo on a child the daemon does not record; it starts nothing, and Dispatch shows no status before it"
	childNoWriterLine  = "workflow: Dispatch's event log proves no writer of the todo on a child the daemon does not record; it starts nothing"
	unreadableChildKey = "LEGION-3"
)

// childLogDispatch is Dispatch as the rig's daemon reads it: the child, LEGION-2, from its one event
// log (issueLogDispatch, whose status writes are the daemon's own), the parent, LEGION-1, as a root
// at work carrying the legion label, and the project's listing of the two, the child as its log
// leaves it, with listed after them. A read of the log of a key failedLogs names fails with that
// error, and logReads counts the reads of the child's log.
type childLogDispatch struct {
	issueLogDispatch
	parent     dispatch.Issue
	listed     []dispatch.IssueSummary
	failedLogs map[string]error
	logReads   int
}

func (d *childLogDispatch) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	if key == d.parent.Key {
		return d.parent, nil
	}
	child, err := d.issueLogDispatch.GetIssue(ctx, key)
	child.Project = "LEGION"
	return child, err
}

func (d *childLogDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	last := d.last()
	parent := d.parent.Key
	return append([]dispatch.IssueSummary{
		{Key: d.parent.Key, Title: d.parent.Title, Status: d.parent.Status, Rank: d.parent.Rank, HandedOver: true, Labels: d.parent.Labels,
			LastSeq: d.parent.LastSeq, ResolvedRepo: d.parent.ResolvedRepo},
		{Key: ownerMoveChild, Title: ownerMoveChild, Status: last.status, Parent: &parent, Rank: "B", Labels: []string{}, LastSeq: last.seq,
			ResolvedRepo: "acme/widgets"},
	}, d.listed...), nil
}

func (d *childLogDispatch) EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error) {
	if err := d.failedLogs[key]; err != nil {
		return nil, err
	}
	if key != ownerMoveChild {
		return nil, nil
	}
	d.logReads++
	return d.issueLogDispatch.EventsAfter(ctx, key, after)
}

// childEvent is the issue event at seq by which actor left the child at status over before (""
// for its creation), as Dispatch records it: its changed list names the status where it moved.
func childEvent(seq int64, actor dispatch.Actor, before, status string) issueLogEntry {
	entry := issueLogEntry{seq: seq, typ: "issue.updated", actor: actor, status: status}
	if before == "" {
		entry.typ = "issue.created"
	}
	if before != status {
		entry.changed = []string{"status"}
	}
	return entry
}

// childRename is actor's rename of the child at seq while it stands at status: an issue event that
// moves no status.
func childRename(seq int64, actor dispatch.Actor, status string) issueLogEntry {
	return issueLogEntry{seq: seq, typ: "issue.updated", actor: actor, status: status, changed: []string{"title"}}
}

// childComment is a person's comment on the child at seq while it stands at status: no issue event.
func childComment(seq int64, status string) issueLogEntry {
	return issueLogEntry{seq: seq, typ: "comment.created", actor: transitionPerson, status: status}
}

// childTransitionRig is a daemon over one parent at work, LEGION-1, and one child under it,
// LEGION-2, which it records nothing of (childLogDispatch). The parent is recorded at implementing
// with its slot held and its tree open, the copy's row of it carries the legion label, and its
// implementer's claim holds transitionAgent's session, as a registration holds it. The handlers
// are the daemon's own, in its order: the copy of Dispatch's issues, the engine and admission; the
// outbox runs the child's status rows against the same Dispatch.
type childTransitionRig struct {
	t        *testing.T
	w        *workflowRuntime
	runner   *outbox
	dispatch *childLogDispatch
	logs     *testlog.Buffer
}

func newChildTransitionRig(t *testing.T, log ...issueLogEntry) *childTransitionRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	logs := &testlog.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	parent := record.Issue{Key: ownerMoveRoot, Tree: ownerMoveRoot, Project: "LEGION", Title: ownerMoveRoot, HandedOver: true, Rank: "A",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", LastDispatchSeq: 1, SeenSeq: 1}
	putOutboxIssue(t, pool, records, parent)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: parent.Key, Project: "LEGION", Seq: 1,
			Status: record.Carry("in_progress"), Parent: record.Carry(""), Labels: record.Carry([]string{dispatch.LegionLabel})})
	}); err != nil {
		t.Fatalf("write the copy row of %s: %v", parent.Key, err)
	}
	holdSlot(t, pool, parent.Key)
	openTree(t, pool, "LEGION", parent.Key)
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, parent.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("the implementer claim token of %s: %v", parent.Key, err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, 'implementer', 1, $4, '', 'working', 0, 0, 0, 0)`,
		string(token), project, parent.Key, transitionAgent.ID); err != nil {
		t.Fatalf("record the implementer claim of %s: %v", parent.Key, err)
	}
	if _, err := pool.Exec(ctx, `insert into claim_sessions (project, kind, name, claim) values ($1, 'session', $2, $3)`,
		project, transitionAgent.ID, string(token)); err != nil {
		t.Fatalf("hold the session of the implementer claim of %s: %v", parent.Key, err)
	}
	fake := &childLogDispatch{issueLogDispatch: issueLogDispatch{log: log}, parent: dispatch.Issue{Key: parent.Key, Project: "LEGION",
		Title: parent.Key, Status: "in_progress", Rank: "A", HandedOver: true, Labels: []string{dispatch.LegionLabel},
		ResolvedRepo: "acme/widgets", LastSeq: 1}}
	repos := newRepositories("LEGION", ghrepo.Renames{})
	repos.fill(ghrepo.MustParse("acme/widgets"))
	branches := newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)
	branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, logger)
	admission := admit.New(records, engine, 10, "LEGION", logger)
	handlers := []intake.Handler{blockers.NewMirror(records, "LEGION"), engine, admission}
	w := &workflowRuntime{pool: pool, records: records, admission: admission, engine: engine, handlers: handlers, repositories: repos,
		defaultBranches: branches, dispatch: fake, dispatchProject: "LEGION", bootID: "child-transition-boot", log: logger}
	return &childTransitionRig{t: t, w: w, dispatch: fake, logs: logs,
		runner: &outbox{pool: pool, records: records, dispatch: fake, handlers: handlers, dispatchProject: "LEGION", log: logger, now: time.Now}}
}

// add appends entries to the child's log: Dispatch has run on, and no event of them is delivered.
func (r *childTransitionRig) add(entries ...issueLogEntry) {
	r.dispatch.log = append(r.dispatch.log, entries...)
}

// deliver is the Dispatch consumer's handling of the child's issue event at seq while Dispatch
// shows the log as it stands: the event decoded as the consumer decodes it, prepared as it
// prepares it (prepareRepository) and applied through the daemon's handlers under its id.
func (r *childTransitionRig) deliver(seq int64) {
	r.t.Helper()
	at := slices.IndexFunc(r.dispatch.log, func(entry issueLogEntry) bool { return entry.seq == seq })
	if at < 0 {
		r.t.Fatalf("the child's log holds no event at %d", seq)
	}
	decoded, _, err := intake.DecodeDispatchEvent("LEGION", ownerMoveChild, r.dispatch.raw(r.dispatch.log[at]))
	if err != nil {
		r.t.Fatalf("decode the child's event %d: %v", seq, err)
	}
	prepared, err := r.w.prepareRepository(context.Background(), decoded.(intake.DispatchIssue))
	if err != nil {
		r.t.Fatalf("prepare the child's event %d: %v", seq, err)
	}
	r.apply(fmt.Sprintf("dispatch-%d", seq), prepared)
}

// apply applies fact through the daemon's handlers under id, as the consumer does.
func (r *childTransitionRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), r.w.pool, "dispatch", id, fact, r.w.handlers...); err != nil {
		r.t.Fatalf("apply %s: %v", id, err)
	}
}

// reread is the chain re-read of the child (rereadChain): Dispatch's view of it and of its parent,
// applied as the re-read applies them.
func (r *childTransitionRig) reread() {
	r.t.Helper()
	known, err := r.w.rereadChain(context.Background(), ownerMoveChild, &chainReread{logged: map[string]bool{}})
	if err != nil || !known {
		r.t.Fatalf("the re-read of the child's chain = %t, %v; want it read whole", known, err)
	}
}

// boot is the boot's reconcile of Dispatch's listing of the project, over the stream positions of
// consumers of the test's own.
func (r *childTransitionRig) boot() {
	r.t.Helper()
	if r.w.consumers == nil {
		r.w.consumers = bootClaimConsumers(r.t, r.w.pool)
	}
	if err := r.w.reconcile(context.Background()); err != nil {
		r.t.Fatalf("the boot's reconcile: %v", err)
	}
}

// runStatusRows drains the child's status lane as the outbox runs it.
func (r *childTransitionRig) runStatusRows() {
	r.t.Helper()
	if drained, _ := r.runner.drain(context.Background(), ownerMoveChild, []record.OutboxKind{record.OutboxKindDispatchStatus}); !drained {
		r.t.Fatalf("the child's status lane did not drain: %s", r.logs.String())
	}
}

// statusWrites is each status write the outbox holds for key, with the run and the phase entry
// the queue fences each row at (record.NewFencedRow) left out: the judgement under test names the
// write, its status and the sequence it undoes, and the fence is the queue's.
func (r *childTransitionRig) statusWrites(key string) []record.StatusWrite {
	r.t.Helper()
	var writes []record.StatusWrite
	for _, row := range rowsOfKind(r.t, r.w.pool, record.OutboxKindDispatchStatus) {
		if row.Issue != key {
			continue
		}
		decoded, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode a status row of %s: %v", key, err)
		}
		write := decoded.(record.StatusWrite)
		write.Generation, write.Entry = 0, 0
		writes = append(writes, write)
	}
	return writes
}

// startsNothing checks the child after path: no record of it, the status rows the outbox holds for
// it are want, and the daemon has logged setBacks lines that it sets the child back.
func (r *childTransitionRig) startsNothing(path string, setBacks int, want ...record.StatusWrite) {
	r.t.Helper()
	if stored := readIssue(r.t, r.w.pool, ownerMoveChild); stored != nil {
		r.t.Fatalf("after %s the child is recorded at %s/%s, want nothing recorded", path, stored.Phase, stored.Status)
	}
	if got := r.statusWrites(ownerMoveChild); !slices.Equal(got, want) {
		r.t.Fatalf("after %s the outbox holds %+v for the child, want %+v", path, got, want)
	}
	if lines := loggedMessages(r.t, r.logs.String(), childSetBackLine); len(lines) != setBacks {
		r.t.Fatalf("after %s the daemon logged %d set-back lines, want %d:\n%s", path, len(lines), setBacks, r.logs.String())
	}
}

// started checks the child after path: recorded under the parent in Legion's work.
func (r *childTransitionRig) started(path string) {
	r.t.Helper()
	stored := readIssue(r.t, r.w.pool, ownerMoveChild)
	if stored == nil || !record.InLegionsWork(*stored) || stored.Tree != ownerMoveRoot {
		r.t.Fatalf("after %s the child is %+v, want it recorded in %s's tree in Legion's work", path, stored, ownerMoveRoot)
	}
}

// R19 of dispatch://INFRA-3320 and the architect's ruling
// (dispatch://INFRA-4730/comment/ada915ba-c94a-4876-8d6e-6cc1a1423f32) on each path by which
// Dispatch's todo on a child the daemon records nothing of reaches admission: the consumer's
// prepared event, the chain re-read and the boot's listing each judge the todo by the transition
// of the child's event log that wrote it (intake.StatusTransitionAt), never by the actor of the
// newest event. The parent's implementer's todo starts nothing however far a person's rename takes
// Dispatch past it, and each reading of it finds the one set-back its first queued, fenced at the
// todo's own sequence, which the outbox then writes over the rename; a person's later todo starts
// the child. The daemon's own todo starts it whoever renames it after. An agent's todo that created
// the child starts nothing and queues nothing on any path. A parent that enters Legion's work
// between a todo's preparation and its apply starts nothing on that todo, since nothing read who
// wrote it, and the next reading starts it. A listing leaves out of admission a child whose log it
// cannot read, and admits the other children.
func TestAnUnrecordedChildsTodoIsJudgedByTheTransitionThatWroteIt(t *testing.T) {
	t.Parallel()
	t.Run("an agent's todo, a person's rename after it", func(t *testing.T) {
		t.Parallel()
		r := newChildTransitionRig(t, childEvent(1, transitionPerson, "", "triage"), childComment(2, "triage"),
			childEvent(3, transitionPerson, "triage", "backlog"), childEvent(4, transitionAgent, "backlog", "todo"),
			childRename(5, transitionPerson, "todo"))
		setBack := record.ReassertStatusWrite("backlog", "todo", 4)
		r.deliver(4)
		r.startsNothing("the agent's own event, Dispatch past it at the rename", 1, setBack)
		r.deliver(5)
		r.startsNothing("the person's rename", 1, setBack)
		r.reread()
		r.startsNothing("the chain re-read", 1, setBack)
		r.boot()
		r.startsNothing("the boot's listing", 1, setBack)
		if reads := r.dispatch.logReads; reads != 4 {
			t.Fatalf("the child's event log was read %d times over four readings, want one each", reads)
		}

		r.runStatusRows()
		if last := r.dispatch.last(); last.actor != transitionDaemon || last.status != "backlog" {
			t.Fatalf("Dispatch ends with the child at %s by %s, want the daemon's set-back to backlog over the rename", last.status, last.actor.Name())
		}
		r.startsNothing("the set-back", 1)

		r.add(childEvent(7, transitionPerson, "backlog", "todo"))
		r.deliver(7)
		r.started("a person's todo after the set-back")
	})

	t.Run("the daemon's own todo, an agent's rename after it", func(t *testing.T) {
		t.Parallel()
		r := newChildTransitionRig(t, childEvent(1, transitionPerson, "", "backlog"), childEvent(2, transitionDaemon, "backlog", "todo"),
			childRename(3, transitionAgent, "todo"))
		r.deliver(3)
		r.started("the agent's rename of the daemon's todo")
		for _, write := range r.statusWrites(ownerMoveChild) {
			if write.Kind == record.ReassertStatusRow {
				t.Fatalf("the outbox holds the set-back %+v for the child, want none of the daemon's own todo", write)
			}
		}
	})

	t.Run("an agent's todo that created the child", func(t *testing.T) {
		t.Parallel()
		r := newChildTransitionRig(t, childEvent(1, transitionAgent, "", "todo"), childComment(2, "todo"))
		r.deliver(1)
		r.startsNothing("the agent's creation at todo", 0)
		if lines := loggedMessages(t, r.logs.String(), childNoBeforeLine); len(lines) != 1 || lines[0]["seq"] != float64(1) {
			t.Fatalf("the daemon logged %v, want one %q at the creation's sequence 1", lines, childNoBeforeLine)
		}
		r.reread()
		r.startsNothing("the chain re-read", 0)
		r.boot()
		r.startsNothing("the boot's listing", 0)
	})

	t.Run("a parent that enters Legion's work after the todo's preparation", func(t *testing.T) {
		t.Parallel()
		r := newChildTransitionRig(t, childEvent(1, transitionPerson, "", "backlog"), childEvent(2, transitionPerson, "backlog", "todo"))
		park := func(status string) {
			t.Helper()
			if _, err := r.w.pool.Exec(context.Background(), `update issues set status = $2 where key = $1`, ownerMoveRoot, status); err != nil {
				t.Fatalf("set the record of %s at %s: %v", ownerMoveRoot, status, err)
			}
		}
		park("backlog")
		decoded, _, err := intake.DecodeDispatchEvent("LEGION", ownerMoveChild, r.dispatch.raw(r.dispatch.last()))
		if err != nil {
			t.Fatalf("decode the person's todo: %v", err)
		}
		prepared, err := r.w.prepareRepository(context.Background(), decoded.(intake.DispatchIssue))
		if err != nil {
			t.Fatalf("prepare the person's todo: %v", err)
		}
		park("in_progress")
		r.apply("dispatch-2", prepared)
		r.startsNothing("a todo applied under a parent that was out of work at its preparation", 0)
		if r.dispatch.logReads != 0 {
			t.Fatalf("the child's event log was read %d times, want none while its parent stood out of Legion's work", r.dispatch.logReads)
		}
		if lines := loggedMessages(t, r.logs.String(), childNoWriterLine); len(lines) != 1 {
			t.Fatalf("the daemon logged %v, want one %q", lines, childNoWriterLine)
		}
		r.boot()
		r.started("the boot's listing, which reads who wrote the todo")
	})

	t.Run("a listing with a child whose log it cannot read", func(t *testing.T) {
		t.Parallel()
		r := newChildTransitionRig(t, childEvent(1, transitionPerson, "", "backlog"), childEvent(2, transitionPerson, "backlog", "todo"))
		parent := ownerMoveRoot
		r.dispatch.listed = []dispatch.IssueSummary{{Key: unreadableChildKey, Title: unreadableChildKey, Status: "todo", Parent: &parent, Rank: "C",
			Labels: []string{}, LastSeq: 2, ResolvedRepo: "acme/widgets"}}
		r.dispatch.failedLogs = map[string]error{unreadableChildKey: &dispatch.EventLogError{Event: 2, Seq: 2,
			Err: errors.New("page issue events for " + unreadableChildKey + ": non-ascending sequence 2 after 2")}}
		r.boot()
		r.started("the boot's listing")
		if stored := readIssue(t, r.w.pool, unreadableChildKey); stored != nil {
			t.Fatalf("%s is recorded as %+v after the listing, want it left out of admission", unreadableChildKey, stored)
		}
		lines := loggedMessages(t, r.logs.String(), leftOutLine)
		if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["issue"] != unreadableChildKey {
			t.Fatalf("the listing logged %v, want one ERROR %q naming %s", lines, leftOutLine, unreadableChildKey)
		}
	})
}
