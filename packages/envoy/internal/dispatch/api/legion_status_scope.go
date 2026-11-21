package api

// legion_status_scope.go is R26 of dispatch://INFRA-3320: the refusal of three writes of a Legion
// issue. A Legion issue carries the legion label, in any letter case, or is under an issue that
// carries it (legionManagedStatus). Where Deps.LegionStatusScope is on
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
// refuses each of them.
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
//     before it writes.
//
// An ancestor that the write does not lock cannot break the rule, for this reason. With the
// refusal on, a caller with neither scope can make no issue stop being a Legion issue, because
// (b) and (c) refuse it. So a judgement read from a chain that moves under it can go stale in two
// ways only:
//
//   - A concurrent write made the issue a Legion issue (a label added, a move under a labelled
//     issue). Then the write that the refusal let through comes first in a serial order, and
//     nothing broke.
//   - A concurrent write of a person, the daemon or the system writer made the issue stop being a
//     Legion issue. Then the refusal comes first in a serial order, and the refusal was right.
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
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// codeLegionStatusScopeRequired refuses one of R26's three writes of a Legion issue to a
// credential that holds neither the person scope nor the legion-status scope. It is not
// codeScopeRequired, which refuses a credential that lacks the one scope a route or a field
// needs, whatever the write changes. This refusal depends on the issue: the same write of an
// issue that is not a Legion issue is taken, and either of two scopes lets the write through.
// Its challenge names legion-status, the scope that a writer which is not a person needs. No
// client acts on this code, so @legion/contracts holds no copy of it.
const codeLegionStatusScopeRequired = "LEGION_STATUS_SCOPE_REQUIRED"

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
// no scope.
//
// It reads the issue's labels from its stored row, and walks the chain from that row to say
// whether the issue is a Legion issue before the call. The caller holds the owner lock of each
// step's issue, so the stored row is the issue's state before the call. The rest comes from the
// step: what the step writes, and the parent that the plan read under the same lock
// (parentBefore), which changesParent and parentLeft read.
//
// A refused step answers 403 codeLegionStatusScopeRequired. A read that fails is an error, which
// the caller answers 500: the read gates a write, so a failed read never lets the write through.
func refuseLegionStatusWrite(ctx context.Context, q pgx.Tx, on bool, writer issueWriter, plan issueWritePlan) error {
	if !on || writer.system || writer.credential.Holds(model.ScopePerson) || writer.credential.Holds(model.ScopeLegionStatus) {
		return nil
	}
	for _, step := range plan.steps {
		if err := refuseLegionStatusStep(ctx, q, step); err != nil {
			return err
		}
	}
	return nil
}

// refuseLegionStatusStep judges one step by R26's three rules. A step writes the status when it
// names a status (statusAfter), with any value, the value the issue holds already included. Only
// a step that writes labels (labelsAfter) can remove the label. So the label rule reads nothing
// when the step writes no labels, or writes labels that hold the label. Otherwise it reads the
// stored labels, and it needs no walk: the read walks the chain only when another rule needs the
// walk. The other two rules read whether the issue is a Legion issue: before the call in the same
// statement as the stored labels (legionIssueBefore), and after the call only when a rule needs it.
func refuseLegionStatusStep(ctx context.Context, q pgx.Tx, step issueWriteStep) error {
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
	if leavesNoLegionLabel && holdsLegionLabel(stored) {
		return legionStatusScopeRefusal(step.key, "the removal of the legion label")
	}
	if !walks {
		return nil
	}
	if writesStatus {
		// (a) A status write of an issue that is a Legion issue before the call or after it.
		if before {
			return legionStatusScopeRefusal(step.key, "a status write")
		}
		after, err := step.legionIssueAfter(ctx, q, stored, before)
		if err != nil {
			return err
		}
		if after {
			return legionStatusScopeRefusal(step.key, "a status write")
		}
		return nil
	}
	// (c) A parent write that makes a Legion issue stop being a Legion issue.
	if !before {
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

// legionIssueBefore reads key's stored row: its labels and, when walk is set, whether it is a
// Legion issue (legionManagedStatusWithLabelsSQL), in one statement. Without walk it reads the
// labels alone.
func legionIssueBefore(ctx context.Context, q pgx.Tx, key string, walk bool) (labels []string, legion bool, err error) {
	if walk {
		err = q.QueryRow(ctx, legionManagedStatusWithLabelsSQL, key).Scan(&labels, &legion)
	} else {
		err = q.QueryRow(ctx, `select labels from issues where key = $1`, key).Scan(&labels)
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s as it is stored, for R26's refusal: %w", key, err)
	}
	return labels, legion, nil
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
// issue's stored labels, and before is the answer before the call. The walk starts from the
// parent and the labels that the step leaves: the labels it writes, or the stored labels when it
// keeps them. A step that changes neither the parent nor the labels keeps the answer before the
// call, so it is answered without a second walk.
func (step issueWriteStep) legionIssueAfter(ctx context.Context, q pgx.Tx, stored []string, before bool) (bool, error) {
	labels := stored
	if step.labelsAfter != nil {
		labels = *step.labelsAfter
	}
	if !step.changesParent() && slices.Equal(stored, labels) {
		return before, nil
	}
	return legionManagedStatusOfRow(ctx, q, step.key, step.parentLeft(), labels)
}

// holdsLegionLabel says whether labels hold the legion label in any letter case. Labels keep the
// case they were typed in, and are de-duplicated where strings.EqualFold matches them (labelKey).
func holdsLegionLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(label string) bool { return strings.EqualFold(label, legionLabel) })
}

// legionStatusScopeRefusal is R26's refusal of write on key, a Legion issue. It is
// missingScopeError's refusal with R26's own code, so writeHandlerError writes the same challenge
// that RFC 6750 gives a credential without the legion-status scope (insufficientScopeChallenge).
func legionStatusScopeRefusal(key, write string) *apiError {
	refusal := missingScopeError(model.ScopeLegionStatus, fmt.Sprintf(
		"%s is a Legion issue (it, or an issue above it, carries the legion label): "+
			"%s needs a credential that holds the %s scope or the %s scope, and this request's holds neither",
		key, write, model.ScopePerson, model.ScopeLegionStatus))
	refusal.code = codeLegionStatusScopeRequired
	return refusal
}
