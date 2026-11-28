package workflow

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// refusedRoot is LEGION-208 walking the plan `implementer tester`, in implementing at step 1 with
// its task running, Dispatch showing it in_progress, and the claims of its architect and its
// implementer, which have both run.
func refusedRoot(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Admitted)
	root.DispatchStatus = "in_progress"
	seedIssue(t, pool, root)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	seedPhaseWorker(t, pool, root.Key, claim.RoleArchitect, "")
	seedPhaseWorker(t, pool, root.Key, claim.RoleImplementer, "")
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	return issueOf(t, pool, engine, root.Key)
}

// setStatus is the controller's set_status of key to status.
func setStatus(key, status string) intake.StatusCall {
	return intake.StatusCall{Op: intake.SetStatusCall, Issues: []intake.StatusCallIssue{{Key: key, Status: status}}}
}

// refusalOf is the outbox's fact of Dispatch's PARENT_STATUS_CONFLICT on the last status row of
// issue that writes status, the controller's: the run and the phase entry the row is fenced at, as
// the outbox reads them from the row (parentRuleRefused).
func refusalOf(t *testing.T, pool *pgxpool.Pool, issue, status string) intake.StatusRefused {
	t.Helper()
	var generation, entry int64
	if err := pool.QueryRow(t.Context(), `select coalesce((payload->>'generation')::bigint, 0), coalesce((payload->>'entry')::bigint, 0)
		from outbox where kind = 'dispatch_status' and issue = $1 and payload->>'status' = $2 order by id desc limit 1`,
		issue, status).Scan(&generation, &entry); err != nil {
		t.Fatalf("read the queued write of %s on %s: %v", status, issue, err)
	}
	return intake.StatusRefused{Issue: issue, Generation: uint64(generation), Entry: entry, Status: status,
		Issues: []string{"LEGION-209 (in_progress)"}, Controller: true}
}

// rolesOf is the role of each supervise row of op that key's outbox holds, oldest first.
func rolesOf(t *testing.T, pool *pgxpool.Pool, key, op string) []claim.Role {
	t.Helper()
	var roles []claim.Role
	for _, request := range superviseRowsOf(t, pool, key, op) {
		roles = append(roles, request.Role)
	}
	return roles
}

// Decision 12 of dispatch://INFRA-4730: a status write the daemon counted before its echo (R16 of
// dispatch://INFRA-3320) that Dispatch refuses with PARENT_STATUS_CONFLICT is undone, and the
// undo is the whole move the count made (statusRefused, undoRefusedStatus). The controller's
// backlog took the root out of Legion's work (leave): every claim suspended and its task
// stopped. Refused, the root comes back into work as a person's in_progress brings it
// (ReturnToWork): its task runs again at its step, its architect's claim is started again, and
// its implementer is started at a new phase entry. The controller is told the code and the issue
// Dispatch named. Negative control, run when this was written and then reverted: with the
// refusal setting the record's status alone, the root read in_progress with its task stopped
// and no start queued.
func TestARefusedStopComesBackIntoWork(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := refusedRoot(t, pool, engine)
	apply := applyFacts(t, pool, engine)
	if result := apply("backlog", setStatus(root.Key, "backlog")); result.Refusal != nil {
		t.Fatalf("the controller's backlog was refused: %+v", result.Refusal)
	}
	if stopped := issueOf(t, pool, engine, root.Key); stopped.Status != "backlog" || stopped.DispatchStatus != "in_progress" {
		t.Fatalf("%s after the counted backlog has status %q with Dispatch at %q, want backlog counted while Dispatch shows in_progress",
			root.Key, stopped.Status, stopped.DispatchStatus)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped {
		t.Fatalf("task a after the counted backlog is %s, want stopped by the leave", task.State)
	}
	entry := roleRowOf(t, pool, engine, root.Key, claim.RoleImplementer).Entry
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleArchitect); len(starts) != 0 {
		t.Fatalf("the architect starts before the refusal = %+v, want none", starts)
	}
	notices := len(noticeRows(t, pool))

	if result := apply("refused", refusalOf(t, pool, root.Key, "backlog")); result.Refusal != nil {
		t.Fatalf("the refusal was refused: %+v", result.Refusal)
	}

	back := issueOf(t, pool, engine, root.Key)
	if !record.InLegionsWork(back) || back.Status != "in_progress" || back.DispatchStatus != "in_progress" {
		t.Fatalf("%s after the refusal has status %q with Dispatch at %q, want it back in Legion's work at in_progress", root.Key, back.Status, back.DispatchStatus)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	// A start of a phase worker takes the place of the one the issue already holds
	// (Store.EnqueueStart), so the implementer's start of the gate's opening is gone.
	architect, implementer := roleStartsOf(t, pool, root.Key, claim.RoleArchitect), roleStartsOf(t, pool, root.Key, claim.RoleImplementer)
	if len(architect) != 1 || architect[0].Task != "" {
		t.Fatalf("the architect starts after the refusal = %+v, want one plain start of its claim in its own session", architect)
	}
	if len(implementer) != 1 || implementer[0].Phase != phase.Implementing || implementer[0].Entry != entry+1 {
		t.Fatalf("the implementer starts after the refusal = %+v, want one of implementing at the new entry %d", implementer, entry+1)
	}
	told := noticeRows(t, pool)[notices:]
	if !slices.ContainsFunc(told, func(payload record.OutboxPayload) bool {
		notice, ok := payload.(record.ControllerNotice)
		return ok && notice.Kind == "message-refused"
	}) {
		t.Fatalf("the notices after the refusal = %+v, want the controller's message-refused", told)
	}
}

// The other way round: the controller's todo on the root a person moved to the backlog brought it
// back into work (ReturnToWork): its task running, its implementer started at a new phase entry,
// and the status of implementing queued over the todo. Refused, the root leaves again as a
// person's backlog takes it out (leave): its record reads backlog, the status writes it had queued
// are dropped, its task is stopped, and each of its claims is suspended, its worker's among them.
// Negative control, run when this was written and then reverted: with the refusal setting the
// record's status alone, the root stayed in Legion's work with its task running and no claim
// suspended, since the return had written in_progress over the todo.
func TestARefusedReturnLeavesWork(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	root := refusedRoot(t, pool, engine)
	apply := applyFacts(t, pool, engine)
	backlog := personsTodo(root.Key)
	backlog.Status = "backlog"
	if result := apply("person-backlog", backlog); result.Refusal != nil {
		t.Fatalf("the person's backlog was refused: %+v", result.Refusal)
	}
	// Admission records the event's observation after the engine (recordObservation), so the
	// record and the copy show Dispatch at the backlog.
	parked := issueOf(t, pool, engine, root.Key)
	parked.DispatchStatus, parked.LastDispatchSeq = "backlog", backlog.Seq
	seedIssue(t, pool, parked)
	if result := apply("todo", setStatus(root.Key, "todo")); result.Refusal != nil {
		t.Fatalf("the controller's todo was refused: %+v", result.Refusal)
	}
	returned := issueOf(t, pool, engine, root.Key)
	if !record.InLegionsWork(returned) || returned.DispatchStatus != "backlog" {
		t.Fatalf("%s after the counted todo has status %q with Dispatch at %q, want it back in Legion's work while Dispatch shows backlog",
			root.Key, returned.Status, returned.DispatchStatus)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Implementing, record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	if got := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(got) == 0 {
		t.Fatal("the counted todo started no implementer, want the return's start")
	}
	suspends := len(superviseRowsOf(t, pool, root.Key, "suspend"))

	if result := apply("refused", refusalOf(t, pool, root.Key, "todo")); result.Refusal != nil {
		t.Fatalf("the refusal was refused: %+v", result.Refusal)
	}

	left := issueOf(t, pool, engine, root.Key)
	if record.InLegionsWork(left) || left.Status != "backlog" || left.Phase != phase.Implementing {
		t.Fatalf("%s after the refusal stands in %s at status %q, want it out of Legion's work at backlog, its phase kept", root.Key, left.Phase, left.Status)
	}
	if writes := statusWritesOf(t, pool, root.Key); len(writes) != 0 {
		t.Fatalf("the status writes of %s after the refusal = %v, want none: the leave drops what the return queued", root.Key, writes)
	}
	if task := taskOf(t, pool, engine, root.Key); task.State != record.TaskStopped || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
		t.Fatalf("task a after the refusal is %s at %+v, want stopped at step 1", task.State, task.Place)
	}
	if got := rolesOf(t, pool, root.Key, "suspend")[suspends:]; !slices.Contains(got, claim.RoleImplementer) || !slices.Contains(got, claim.RoleArchitect) {
		t.Fatalf("the claims suspended after the refusal = %v, want each of them, the implementer's and the architect's among them", got)
	}
}
