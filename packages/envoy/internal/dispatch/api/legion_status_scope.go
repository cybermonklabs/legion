package api

// legion_status_scope.go is R26 of dispatch://INFRA-3320: the refusal of three writes of a Legion
// issue. A Legion issue carries the legion label, in any letter case, or is under an issue of its
// own project that carries it (legionManagedStatus). Where Deps.LegionStatusScope is on
// (DISPATCH_LEGION_STATUS_SCOPE), a credential that holds neither model.ScopePerson nor
// model.ScopeLegionStatus cannot make these writes:
//
//   - (a) A write of the issue's status, when the issue is a Legion issue before the call or
//     after it. A write of the status that the issue holds already is a status write too.
//   - (b) The removal of the legion label: the labels before the call hold it, in any letter
//     case, and the labels after the call do not.
//   - (c) A change of the parent that makes a Legion issue stop being a Legion issue.
//
// The Legion daemon owns a Legion issue's status. Its own broker token holds legion-status, and a
// person's own credential holds person. Every other credential holds neither: the shared token, a
// personal token without person, a project's broker token and a pod's Kubernetes token. R26
// refuses each of them, but for one exception.
//
// The exception is the owning architect's write (writtenByOwningArchitect). For a credential that
// holds neither scope, it lifts the refusal of a step's status write, rule (a), and nothing else,
// when all three of these hold:
//
//   - The request's credential proves its session: model.Credential.Session, which only
//     brokerTokenActor sets, from a broker-issued token. A session that the body's actor or
//     ?session= names proves nothing.
//   - The step writes done, backlog, icebox or triage (architectWrites). With include_children
//     it writes backlog, icebox or triage only, because R24 takes include_children with done from
//     a person at a browser alone. The call passes neither unlink_children nor include_parent.
//   - Before the call and after it, the chain above the step's issue holds an issue whose marked
//     role holders name that session as architect (owningArchitectSQL). A pair is marked when the
//     daemon's own broker token published it (issue_role_holders.daemon_proven,
//     putIssueRoleHolders). The chain is the one that the legion walk reads (issueChainSQL): the
//     parent and each issue above it of the step's issue's own project, up to parentDepthCap. The
//     step's own issue's role holders do not count, so an issue that has no stored set of its own
//     is written alike.
//
// The same step's other parts are judged as for every other writer: (b) the removal of the label,
// and (c) a parent change that makes the issue stop being a Legion issue. So the architect cannot
// take the label off or move the issue out from under every labelled issue, with a status write or
// without one.
//
// The refusal reads the request's credential, as optionalActor resolved it
// (requestWriter), and never the actor that a body names. A bearer that proves no
// session names its session in the body, and that body can name any session
// (bearerSessionActor). The shared token whose body names the daemon's session is still the
// shared token, so R26 refuses it.
//
// The refusal is the first statement of refuseIssueWritePlan (parent_status_rule.go). Every write
// that R26 judges runs that check before its first write.
// TestOnlyTheJudgedWritersWriteAnIssuesStatusLabelsOrParent names each writer of an issue's
// status, labels or parent, and why a writer that runs no such check needs none. So R26 judges
// every step of every plan, and it judges a call before R24 does.
//
// The refusal takes no lock on an ancestor, and needs none. The walk is one statement, so it
// reads one snapshot of the chain. It runs after the locks that its caller holds, so, under READ
// COMMITTED, it reads every write that those locks waited for. The callers of
// refuseIssueWritePlan hold these locks:
//
//   - The PATCH's write core, writeIssue, holds the owner lock of the issue, and for a status or
//     parent write the owner locks of its current and new parent (lockPatchOwners,
//     lockPatchOwnersAndParent). For every parent write, it holds the deployment-wide dependency
//     lock too (lockIssueDependencyWrite).
//   - The planning check of the status-change route (planStatusChangeUnderLocks) and that of
//     R25's fix (planParentStatusFixUnderLocks) hold the owner lock of each step's issue and of
//     its current and new parent (lockIssueWriteOwners), and no dependency lock. Then each step
//     writes through writeIssue, which takes the locks of the item above and runs R26 again
//     before it writes, with the call's options (issueWriteCallStep.options).
//
// An ancestor that the write does not lock cannot break the rule, for this reason. With the
// refusal on, a caller with neither scope can make no issue stop being a Legion issue, because
// (b) and (c) refuse it, to the owning architect too. So a judgement read from a chain that moves
// under it can go stale in two ways only:
//
//   - A concurrent write made the issue a Legion issue (a label added, a move under a labelled
//     issue). Then the write that the refusal let through comes first in a serial order, and
//     nothing broke.
//   - A concurrent write of a person, the daemon or the system writer made the issue stop being a
//     Legion issue. Then the refusal comes first in a serial order, and the refusal was right.
//
// The owning architect's walk reads the role holders of issues that the caller does not lock
// either. A role holders publication takes the owner lock of its own issue alone
// (putIssueRoleHolders), and of the issues above a step the caller locks only the parent before
// and after the call. Each walk of the architect is one statement too, so it reads one snapshot of
// the chain and of its role holders. A judgement read from a set or a chain that changes under it
// can go stale in two ways only:
//
//   - A concurrent publication named the session as the architect of an issue on the chain, or a
//     concurrent move put such an issue on it. Then the refusal comes first in a serial order, and
//     the refusal was right.
//   - A concurrent publication dropped the session, or a concurrent move took its issue off the
//     chain. Then the write that the exception let through comes first in a serial order, before
//     that publication or move. A publication writes issue_role_holders and the project's mark
//     (project_role_holder_marks), and it reads its own issue's key and its project's
//     proven_sessions, none of which a status write writes. A move of an issue above the parent
//     writes that issue alone, and R26 and R24 judge it by its own chain and its new parent. So
//     neither reads what the write writes,
//     and that order is a serial one.
//
// This holds for each judgement under its own locks: the planning check's, and each step's in
// writeIssue.
//
// A loop made of moves (X under C while C goes under X, or a longer ring) cannot commit. Every
// parent write takes the deployment-wide dependency lock, and validateIssueParent refuses the
// cycle under that lock (TestConcurrentThreeWayDependenciesDoNotCommitCycle,
// issue_dependencies_test.go). The PATCH answers PARENT_INPUT before this refusal, so this
// refusal never judges a chain that the call would close into a loop.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// apicode.LegionStatusScopeRequired refuses one of R26's three writes of a Legion issue to a
// credential that holds neither the person scope nor the legion-status scope. It is not
// apicode.OAuthScopeRequired, which refuses a credential that lacks the one scope a route or a
// field needs, whatever the write changes. This refusal depends on the issue: the same write of an
// issue that is not a Legion issue is taken, and either of two scopes lets the write through. Its
// challenge names legion-status, the scope that a writer which is not a person needs. No client
// acts on this code, so @legion/contracts holds no copy of it. Both versions answer it, /api/v2 on
// PATCH /api/v2/issues/{key}, which declares it.

// refuseLegionStatusWrite is R26's refusal (this file's comment). refuseIssueWritePlan runs it as
// its first statement, before the plan's first write. It judges every step of the plan. on is
// Deps.LegionStatusScope: off refuses nothing.
//
// A refused PATCH writes no field and no event of the issue, because the PATCH writes in one
// transaction. A close (status done) settles the issue's documents before that transaction
// (settleIssueBeforeClose), so a refused close can still have settled them. A refused
// POST /api/v1/issues/{key}/status-change can change some issues. Its plan is checked once before
// the first write (planStatusChangeUnderLocks). Then each step commits in a transaction of its
// own through writeIssue, which runs this check again. A step refused there stops the call after
// the earlier steps committed, and the answer lists those steps in changed, as for any refused
// step.
//
// It lets two writers through. One is Dispatch's system writer (writer.system): only
// parentStatusFixWriter sets it, for R25's boot fix, and no request can produce it. The other is
// a writer whose credential holds the person scope or the legion-status scope. That credential is
// the one that optionalActor resolved for the request (requestWriter), and a nil credential holds
// no scope. Of every other writer it lets one write through: the owning architect's status write
// (this file's comment), which reads the session that the credential proved
// (model.Credential.Session, empty for a nil credential and for every credential but a
// broker-issued token's) and the plan's options.
//
// It reads the issue's labels from its stored row, and walks the chain from that row to say
// whether the issue is a Legion issue before the call. The caller holds the owner lock of each
// step's issue, so the stored row is the issue's state before the call. The rest comes from the
// step: what the step writes, and the parent that the plan read under the same lock
// (parentBefore), which changesParent and parentLeft read.
//
// A refused step answers 403 LEGION_STATUS_SCOPE_REQUIRED (apicode.LegionStatusScopeRequired). A read that fails is an error, which
// the caller answers 500: the read gates a write, so a failed read never lets the write through.
func refuseLegionStatusWrite(ctx context.Context, q pgx.Tx, on bool, writer issueWriter, plan issueWritePlan) error {
	if !on || writer.system || writer.credential.Holds(model.ScopePerson) || writer.credential.Holds(model.ScopeLegionStatus) {
		return nil
	}
	call := architectCall{options: plan.options}
	if writer.credential != nil {
		call.session = writer.credential.Session
	}
	for _, step := range plan.steps {
		if err := refuseLegionStatusStep(ctx, q, call, step); err != nil {
			return err
		}
	}
	return nil
}

// architectCall is what the owning architect's write reads of a call beside its step
// (writtenByOwningArchitect): session, the session that the request's credential proved, empty
// for a credential that proves none, and the call's options.
type architectCall struct {
	session string
	options statusChangeOptions
}

// refuseLegionStatusStep judges one step by R26's three rules. A step writes the status when it
// names a status (statusAfter), with any value, the value the issue holds already included. Only
// a step that writes labels (labelsAfter) can remove the label. So the label rule reads nothing
// when the step writes no labels, or writes labels that hold the label. Otherwise it reads the
// stored labels, and it needs no walk: the read walks the chain only when another rule needs the
// walk. The other two rules read whether the issue is a Legion issue: before the call in the same
// statement as the stored labels (legionIssueBefore), and after the call only when a rule needs it.
//
// A status write of a Legion issue is refused unless call is the owning architect's
// (writtenByOwningArchitect), which reads its chains only for such a write. A status write that
// the exception lets through does not end the judgement: rule (c) judges the same step's parent
// change after it.
func refuseLegionStatusStep(ctx context.Context, q pgx.Tx, call architectCall, step issueWriteStep) error {
	writesStatus := step.statusAfter != nil
	walks := writesStatus || step.changesParent()
	leavesNoLegionLabel := step.labelsAfter != nil && !holdsLegionLabel(*step.labelsAfter)
	if !walks && !leavesNoLegionLabel {
		return nil
	}
	stored, before, err := legionIssueBefore(ctx, q, step.key, walks)
	if err != nil {
		return err
	}
	// (b) The removal of the legion label.
	if leavesNoLegionLabel && holdsLegionLabel(stored.labels) {
		return legionStatusScopeRefusal(step.key, "the removal of the legion label")
	}
	if !walks {
		return nil
	}
	if writesStatus {
		// (a) A status write of an issue that is a Legion issue before the call or after it.
		legion := before
		if !legion {
			if legion, err = step.legionIssueAfter(ctx, q, stored, before); err != nil {
				return err
			}
		}
		if !legion {
			return nil
		}
		owned, err := step.writtenByOwningArchitect(ctx, q, stored.project, *step.statusAfter, call)
		if err != nil {
			return err
		}
		if !owned {
			return legionStatusScopeRefusal(step.key, "a status write")
		}
	}
	// (c) A parent write that makes a Legion issue stop being a Legion issue.
	if !before || !step.changesParent() {
		return nil
	}
	after, err := step.legionIssueAfter(ctx, q, stored, before)
	if err != nil {
		return err
	}
	if !after {
		return legionStatusScopeRefusal(step.key, "a move out from under every labelled issue")
	}
	return nil
}

// writtenByOwningArchitect says whether R26 lets the step's status write through as the owning
// architect's write (this file's comment). project is the step's issue's stored project. The
// write is the architect's when call.session is a proven session, the step's status is one that
// call.options lets the architect write (architectWrites), and the chain above the issue names
// that session as the architect of an issue both before the call (from parentBefore) and after it
// (from parentLeft). The issue's own role holders do not count. It reads nothing when the session
// or the status rules the write out, and one walk when the step keeps its parent. status is the
// status the step writes (step.statusAfter, which a status write sets).
func (step issueWriteStep) writtenByOwningArchitect(ctx context.Context, q pgx.Tx, project, status string, call architectCall) (bool, error) {
	if call.session == "" || !call.options.architectWrites(status) {
		return false, nil
	}
	before, err := architectAbove(ctx, q, step.key, project, step.parentBefore, call.session)
	if err != nil || !before {
		return false, err
	}
	if !step.changesParent() {
		return true, nil
	}
	return architectAbove(ctx, q, step.key, project, step.parentLeft(), call.session)
}

// architectWrites says whether the owning architect may write status in a call with options: a
// status out of the workflow (model.IssueStatusesOutOfWorkflow, the daemon's
// record.OutOfWorkflowStatuses), in a call with neither unlink_children nor include_parent. With
// include_children it may not write done, because R24 takes include_children with done from a
// person at a browser alone (refuseIssueWritePlan). Every status in work stays the daemon's.
func (options statusChangeOptions) architectWrites(status string) bool {
	if options.unlinkChildren || options.includeParent || !slices.Contains(model.IssueStatusesOutOfWorkflow, status) {
		return false
	}
	return status != "done" || !options.includeChildren
}

// owningArchitectSQL answers whether an issue above a step's issue, whose parent is $1 and whose
// project is $2, has marked role holders that name the session $3 as its architect. The
// chain is issueChainSQL's, the one the legion walk reads: it climbs only to a parent of the
// step's issue's own project, so it ends at the first issue of another project as at a root, and
// it stops at parentDepthCap links above the step's issue. The step's own row names no key, as
// legionManagedStatusOfRowSQL's does, so its role holders do not count. A pair is marked when the daemon's own broker token published it
// (daemon_proven, putIssueRoleHolders). The role holders key (issue_key, role) finds each issue's
// architect.
var owningArchitectSQL = `with recursive ` + issueChainSQL(`select null::text as key, $1::text as parent_key, '{}'::text[] as labels, $2::text as project_key, 0 as depth`) + `
	select exists (
		select 1 from chain c join issue_role_holders h on h.issue_key = c.key
		where h.role = 'architect' and h.session = $3 and h.daemon_proven
	)`

// architectAbove reads owningArchitectSQL for the step's issue key: whether the chain from
// parent, within project, names session as the architect of an issue. A nil parent is an empty
// chain, which names no architect, so it reads nothing.
func architectAbove(ctx context.Context, q pgx.Tx, key, project string, parent *string, session string) (bool, error) {
	if parent == nil {
		return false, nil
	}
	var named bool
	if err := q.QueryRow(ctx, owningArchitectSQL, *parent, project, session).Scan(&named); err != nil {
		return false, fmt.Errorf("read whether session %s is the architect of an issue above %s, for R26's refusal: %w", session, key, err)
	}
	return named, nil
}

// legionIssueBefore reads key's stored row: its labels, its project and, when walk is set,
// whether it is a Legion issue (legionManagedStatusWithLabelsSQL), in one statement. Without walk
// it reads the labels and the project alone.
func legionIssueBefore(ctx context.Context, q pgx.Tx, key string, walk bool) (stored storedLegionRow, legion bool, err error) {
	if walk {
		err = q.QueryRow(ctx, legionManagedStatusWithLabelsSQL, key).Scan(&stored.labels, &stored.project, &legion)
	} else {
		err = q.QueryRow(ctx, `select labels, project_key from issues where key = $1`, key).Scan(&stored.labels, &stored.project)
	}
	if err != nil {
		return storedLegionRow{}, false, fmt.Errorf("read %s as it is stored, for R26's refusal: %w", key, err)
	}
	return stored, legion, nil
}

// storedLegionRow is what R26's refusal reads of a step's issue as it is stored: its labels and
// its project, which the walk after the call starts from (legionIssueAfter).
type storedLegionRow struct {
	labels  []string
	project string
}

// parentLeft is the parent that the step leaves on its issue: the parent it writes, or the
// parent the issue has when the step writes none. Nil is no parent.
func (step issueWriteStep) parentLeft() *string {
	if step.reparent {
		return step.parentAfter
	}
	return step.parentBefore
}

// legionIssueAfter says whether the step's issue is a Legion issue after the call. stored is the
// issue's stored row, and before is the answer before the call. No write moves an issue to
// another project, so the walk climbs from the stored project. The walk starts from the
// parent and the labels that the step leaves: the labels it writes, or the stored labels when it
// keeps them. A step that changes neither the parent nor the labels keeps the answer before the
// call, so it is answered without a second walk.
func (step issueWriteStep) legionIssueAfter(ctx context.Context, q pgx.Tx, stored storedLegionRow, before bool) (bool, error) {
	labels := stored.labels
	if step.labelsAfter != nil {
		labels = *step.labelsAfter
	}
	if !step.changesParent() && slices.Equal(stored.labels, labels) {
		return before, nil
	}
	return legionManagedStatusOfRow(ctx, q, step.key, stored.project, step.parentLeft(), labels)
}

// holdsLegionLabel says whether labels hold the legion label in any letter case. Labels keep the
// case they were typed in, and are de-duplicated where strings.EqualFold matches them (labelKey).
func holdsLegionLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(label string) bool { return strings.EqualFold(label, legionLabel) })
}

// legionStatusScopeRefusal is R26's refusal of write on key, a Legion issue. It is
// missingScopeError's refusal with R26's own code, so writeHandlerError writes the same challenge
// that RFC 6750 gives a credential without the legion-status scope (insufficientScopeChallenge).
// Each refusal names the one write that R26 lets through without either scope: the owning
// architect's (this file's comment).
func legionStatusScopeRefusal(key, write string) *apiError {
	refusal := missingScopeError(model.ScopeLegionStatus, fmt.Sprintf(
		"%s is a Legion issue (it, or an issue above it, carries the legion label): "+
			"%s needs a credential that holds the %s scope or the %s scope, and this request's holds neither. "+
			"An architect of an issue above writes done, backlog, icebox or triage once its claim is ready: "+
			"its broker token proves its session, and the Legion daemon has published that session as the architect of that issue",
		key, write, model.ScopePerson, model.ScopeLegionStatus))
	refusal.code = apicode.LegionStatusScopeRequired
	return refusal
}
