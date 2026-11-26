package api

// parent_status_fix.go is R25's fix: the one change, at boot with DISPATCH_PARENT_STATUS_RULE on,
// of the pairs of a parent and a child that break R24 (parent_status_rule.go). It unlinks each
// open issue from its done parent, then moves each not-started ancestor of an issue in work to
// in_progress, the highest first. Each change is one write through the PATCH's write core
// (writeIssue), as Dispatch's system actor, with a comment on the issue it changes. One process
// runs the fix under the lease parent-status-fix and then records that it ran (the table
// parent_status_fix), so the fix runs once per database, and every process serves only once the
// record is there.

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// parentStatusFixLease names the lease that one process runs R25's fix under, so that two
// processes that boot together never both run it.
const parentStatusFixLease = "parent-status-fix"

// parentStatusFixWait is how long a boot waits before it reads the fix's record again while
// another process holds the lease, and parentStatusFixLogEvery is how often it says in its log
// which lease it waits on.
const (
	parentStatusFixWait     = time.Second
	parentStatusFixLogEvery = 15 * time.Second
)

// parentStatusRuleOn is the rule turned on. Only RunParentStatusFixAtBoot makes it, and only once
// the fix's record exists.
func parentStatusRuleOn() ParentStatusRule { return ParentStatusRule{on: true} }

// RunParentStatusFixAtBoot runs R25's fix once per database, before the server takes any write,
// and answers the rule that the server is built with. deps needs Store, Docs and Events, as
// RunIssueAskRepair's do, and leases is the lease pool's connection that the boot reserved for the
// fix.
//
// With on false it answers the rule off at once, and reads and writes nothing. With on true it
// answers the rule on only once the fix's record exists. It reads the record. While another
// process holds the lease, it waits a second and reads the record again, and says in its log
// every 15 seconds which lease it waits on: a try of a lease that another process holds would log
// a skip at each try (duty.Exclusive). Else it takes the lease, reads the record again under it,
// and when the record is still absent, runs the fix and then writes the record.
//
// A step that fails answers its error, and no record is written. The next boot plans again from
// what stands, and since each step and its comment commit together, nothing is changed or
// commented twice. A boot that stopped after its last step and before the record plans no step
// the next time and writes only the record. A context that ends answers its error.
func RunParentStatusFixAtBoot(ctx context.Context, deps Deps, leases duty.Leases, on bool) (ParentStatusRule, error) {
	if !on {
		return ParentStatusRule{}, nil
	}
	// The fix writes with the rule on, so a step that would break the rule is refused.
	deps.ParentStatusRule = parentStatusRuleOn()
	s := &server{deps: deps}
	pool, err := s.readyPool()
	if err != nil {
		return ParentStatusRule{}, err
	}
	pass := &parentStatusFixPass{server: s}
	var logged time.Time
	for {
		recorded, err := parentStatusFixRecorded(ctx, pool)
		if err != nil {
			return ParentStatusRule{}, err
		}
		if recorded {
			return deps.ParentStatusRule, nil
		}
		held, err := duty.LeaseHeld(ctx, pool, parentStatusFixLease)
		if err != nil {
			return ParentStatusRule{}, err
		}
		if !held {
			ran := duty.Exclusive(ctx, leases, parentStatusFixLease, time.Time{}, 0, pass.run)
			if pass.err != nil {
				if ctx.Err() != nil {
					return ParentStatusRule{}, ctx.Err()
				}
				return ParentStatusRule{}, pass.err
			}
			if ran {
				// The pass wrote the record or found it written, so the next read finds it.
				continue
			}
			// Another process took the lease between the read and the try, or the try failed and
			// logged why: wait as for a lease that another process holds.
		}
		if now := time.Now(); now.Sub(logged) >= parentStatusFixLogEvery {
			slog.Info("dispatch: the boot waits for the parent status fix, which another process runs under its lease", "lease", parentStatusFixLease)
			logged = now
		}
		if !waitFor(ctx, parentStatusFixWait) {
			return ParentStatusRule{}, ctx.Err()
		}
	}
}

// parentStatusFixRecorded reports whether the fix's record exists: whether a process ran the fix
// on this database.
func parentStatusFixRecorded(ctx context.Context, q queryer) (bool, error) {
	var recorded bool
	if err := q.QueryRow(ctx, `select exists (select 1 from parent_status_fix)`).Scan(&recorded); err != nil {
		return false, fmt.Errorf("read the record of the parent status fix: %w", err)
	}
	return recorded, nil
}

// parentStatusFixPass is the fix's pass under its lease, which duty.Exclusive runs, and the error
// that the pass ended with. Each step of the pass commits a transaction of its own (writeIssue),
// and the record is a statement of its own, so the pass holds nothing once it returns.
type parentStatusFixPass struct {
	server *server
	err    error
}

// run is the pass: fixParentStatuses, whose error it keeps for RunParentStatusFixAtBoot.
func (pass *parentStatusFixPass) run(ctx context.Context) {
	pass.err = pass.server.fixParentStatuses(ctx)
}

// fixParentStatuses is the fix's pass under its lease. It reads the record again, since another
// process can have run the fix before this one took the lease. When the record is still absent,
// it plans the fix (planParentStatusFix), runs each step in a transaction of its own, and then
// writes the record with the number of issues it unlinked and moved, in a statement of its own.
func (s *server) fixParentStatuses(ctx context.Context) error {
	pool, err := s.readyPool()
	if err != nil {
		return err
	}
	recorded, err := parentStatusFixRecorded(ctx, pool)
	if err != nil || recorded {
		return err
	}
	steps, err := s.planParentStatusFix(ctx)
	if err != nil {
		return err
	}
	unlinked, moved := 0, 0
	for _, step := range steps {
		written, err := s.writeIssue(ctx, step.call(ctx))
		if err != nil {
			return fmt.Errorf("the parent status fix's write of %s: %w", step.key, err)
		}
		switch {
		case !written.wrote:
		case step.reparent:
			unlinked++
		default:
			moved++
		}
	}
	if _, err := pool.Exec(ctx, `insert into parent_status_fix (unlinked, moved) values ($1, $2)`, unlinked, moved); err != nil {
		return fmt.Errorf("record the parent status fix: %w", err)
	}
	slog.Info("dispatch: the parent status fix ran", "unlinked", unlinked, "moved", moved)
	return nil
}

// readParentStatusFixIssues reads every issue of the database, of every project, in key order:
// by project, then by number.
func readParentStatusFixIssues(ctx context.Context, q queryer) ([]ruleIssue, error) {
	rows, err := q.Query(ctx, `select key, status, parent_key from issues order by project_key, number`)
	if err != nil {
		return nil, fmt.Errorf("read the issues of the parent status fix: %w", err)
	}
	issues, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ruleIssue, error) {
		var issue ruleIssue
		err := row.Scan(&issue.key, &issue.status, &issue.parent)
		return issue, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the issues of the parent status fix: %w", err)
	}
	return issues, nil
}

// parentStatusFixStep is one step of R25's fix: the unlink of an open issue from its done parent,
// or the move of a not-started issue to in_progress. names is the issue that the step's comment
// names: the done parent of an unlink, and, for a move, the issue in work under it that the move
// is for.
type parentStatusFixStep struct {
	issueWriteStep
	names string
}

// parentStatusFixSteps is R25's two steps over issues, every issue of the database in key order.
// First each open issue under a done parent is unlinked, in key order, and keeps its status. Then,
// over the links left, each not-started ancestor of an issue in work is moved to in_progress: the
// highest first (the fewest links above it), then in key order. A move's comment names the issue
// in work under it that comes first in key order. Over the links left, no done issue has an open
// issue under it, so each ancestor of an issue in work is in work or not started.
func parentStatusFixSteps(issues []ruleIssue) []parentStatusFixStep {
	statuses := make(map[string]string, len(issues))
	order := make(map[string]int, len(issues))
	for index, issue := range issues {
		statuses[issue.key], order[issue.key] = issue.status, index
	}
	var steps []parentStatusFixStep
	// left is the parent of each issue over the links that the unlinks leave.
	left := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue.parent == nil {
			continue
		}
		if statusGroup(issue.status) != groupDone && statusGroup(statuses[*issue.parent]) == groupDone {
			steps = append(steps, parentStatusFixStep{names: *issue.parent, issueWriteStep: issueWriteStep{
				key: issue.key, statusBefore: issue.status, parentBefore: issue.parent, reparent: true,
			}})
			continue
		}
		left[issue.key] = *issue.parent
	}
	parentLeft := func(key string) *string {
		if parent, ok := left[key]; ok {
			return &parent
		}
		return nil
	}
	// Each issue in work, in key order, walks up the links left. An ancestor that an earlier walk
	// reached was moved, or left, with everything above it, so a walk stops there. That also ends
	// a walk round a cycle of links.
	inProgress := "in_progress"
	walked := map[string]bool{}
	var moves []parentStatusFixStep
	for _, issue := range issues {
		if statusGroup(issue.status) != groupInWork {
			continue
		}
		for above, ok := left[issue.key]; ok && !walked[above]; above, ok = left[above] {
			walked[above] = true
			if statusGroup(statuses[above]) == groupNotStarted {
				moves = append(moves, parentStatusFixStep{names: issue.key, issueWriteStep: issueWriteStep{
					key: above, statusBefore: statuses[above], statusAfter: &inProgress, parentBefore: parentLeft(above),
				}})
			}
		}
	}
	depths := make(map[string]int, len(moves))
	for _, move := range moves {
		links := 0
		for above, ok := left[move.key]; ok && links < len(issues); above, ok = left[above] {
			links++
		}
		depths[move.key] = links
	}
	slices.SortFunc(moves, func(a, b parentStatusFixStep) int {
		return cmp.Or(cmp.Compare(depths[a.key], depths[b.key]), cmp.Compare(order[a.key], order[b.key]))
	})
	return append(steps, moves...)
}

// planParentStatusFix is R25's fix over every issue of the database, checked under its owner
// locks before any step writes. It reads the plan with plain reads, then, in a transaction
// of its own that writes nothing, takes the owner locks of every step's issue and of each step's
// current and new parent in one store.LockOwners call, reads the plan again under them, and checks
// the whole plan as Dispatch's system actor writes it (refuseIssueWritePlan), with the rule on: a
// plan that breaks the rule is refused before any step writes. A plan whose issues moved between
// the two reads, so that it needs a lock the transaction does not hold, starts over with what the
// second read found, as planStatusChange does, and a plan that moves at every attempt is refused
// with nothing applied. The transaction rolls back before the first step, and each step takes
// its own locks in its own transaction.
func (s *server) planParentStatusFix(ctx context.Context) ([]parentStatusFixStep, error) {
	pool, err := s.readyPool()
	if err != nil {
		return nil, err
	}
	issues, err := readParentStatusFixIssues(ctx, pool)
	if err != nil {
		return nil, err
	}
	steps := parentStatusFixSteps(issues)
	for range parentLockAttempts {
		planned, locked, err := s.planParentStatusFixUnderLocks(ctx, steps)
		if err != nil || locked {
			return planned, err
		}
		steps = planned
	}
	return nil, fmt.Errorf("the issues that the parent status fix writes changed at each of %d attempts while it waited to lock them, so nothing was applied",
		parentLockAttempts)
}

// planParentStatusFixUnderLocks is one attempt of planParentStatusFix, which locks the owners that
// steps need. It answers the plan read again under those locks and true, or, when that plan needs
// a lock the attempt does not hold, that plan and false, for the next attempt to lock.
func (s *server) planParentStatusFixUnderLocks(ctx context.Context, steps []parentStatusFixStep) ([]parentStatusFixStep, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	held := issueWriteLocks(steps)
	if err := lockIssueWriteOwners(ctx, tx, held); err != nil {
		return nil, false, err
	}
	issues, err := readParentStatusFixIssues(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	again := parentStatusFixSteps(issues)
	if len(again) == 0 {
		return again, true, nil
	}
	if !locksHeld(held, again) {
		return again, false, nil
	}
	keys := make([]string, 0, len(again))
	for _, step := range again {
		keys = append(keys, step.key)
	}
	rows, err := readIssueWriteRows(ctx, tx, keys)
	if err != nil {
		return nil, false, err
	}
	writes := issueWritePlan{steps: make([]issueWriteStep, 0, len(again))}
	for index := range again {
		row := rows[again[index].key]
		again[index].title, again[index].project = row.title, row.project
		writes.steps = append(writes.steps, again[index].issueWriteStep)
	}
	conflict, err := s.refuseIssueWritePlan(ctx, tx, parentStatusFixWriter(), writes)
	if err != nil {
		return nil, false, err
	}
	if conflict != nil {
		return nil, false, fmt.Errorf("the plan of the parent status fix breaks the rule it fixes, so nothing was applied: %w", conflict)
	}
	return again, true, nil
}

// call is the write of the step through the PATCH's write core, as Dispatch's system actor, with
// the comment that says what the step changed and why. The step decides under its own lock
// whether it writes: an unlink leaves an issue that is done or is no longer under the done parent
// the plan read, and a move leaves an issue that is no longer not started.
func (step parentStatusFixStep) call(ctx context.Context) issueWriteCall {
	call := issueWriteCall{
		writer: parentStatusFixWriter(),
		// The fix runs at boot for no caller, so it reads as the server's own reader.
		who: everyReader,
		key: step.key,
		// The fix closes no issue, so neither a settlement nor an ask sweep runs on it.
		outliving: ctx,
		// The fix has no route. The advice of a system actor reads nothing, so the lease names
		// the write instead.
		adviceRoute: parentStatusFixLease,
		comment:     &issueComment{author: parentStatusRuleActor},
		step:        &issueWriteCallStep{},
	}
	names := step.names
	if step.reparent {
		call.unlinkFrom(names)
		call.comment.words = func(model.Issue) string {
			return fmt.Sprintf("Dispatch's parent status rule (R24): %s is done and this issue is not, so Dispatch removed the link. "+
				"This issue keeps its status, its labels and the issues under it.", names)
		}
		return call
	}
	call.fields.Status = step.statusAfter
	call.step.leaves = func(before model.Issue) bool { return statusGroup(before.Status) != groupNotStarted }
	call.comment.words = func(before model.Issue) string {
		return fmt.Sprintf("Dispatch's parent status rule (R24): %s, in work, is under this issue, so Dispatch moved it from `%s` to `in_progress`.",
			names, before.Status)
	}
	return call
}
