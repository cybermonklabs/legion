package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The first boot's conversion of what the release before stored (R22 of dispatch://INFRA-3320,
// decision 21 of dispatch://INFRA-4730). Some of its steps read Dispatch's listing, so it is not
// one transaction: each numbered step runs in a transaction of its own, under the lock every fact
// applies under, and records itself in tree_conversions in that same transaction
// (record.RecordTreeConversionStep), so a boot after a crash starts at the next step, and only the
// last step writes when the conversion ended (converted_at), which the state reports. The record is
// the project's own, since several projects' daemons can share one database.
//
// The order is the decision's, with the claims' marks (7) after its steps that read only the
// store. The steps that read only the store (1 to 7) run right after the store migrates and before
// anything decodes a queued row (convertStore, before openUncountedRuns), and before supervision
// reads the claims: the release before queued its rows with no fence, which step 1 gives them.
// Then the boot's reconcile reads the listing and fills the copy of Dispatch's issues in a
// transaction of its own, runs the steps that read the listing or the copy (8 to 11) and the record
// (12), and only then the first admission pass (convertListed). A boot whose conversion is finished
// runs the reconcile's one transaction, as every later boot does.
const (
	// convertFences stamps the fence of each row the release before queued without one
	// (record.StampReleaseBeforeFences).
	convertFences = iota + 1
	// convertCarriedRoots copies each root a hand-over made onto its record
	// (record.CarryHandedOverRoots).
	convertCarriedRoots
	// convertLingers makes each linger the release before left the retention of each issue it
	// covered (record.RetainLingers).
	convertLingers
	// convertOperatorHolds makes the operator's hold of a tree a hold of each issue of it in work
	// (record.HoldIssuesOfOperatorTrees).
	convertOperatorHolds
	// convertReservations makes each tree cleanup still reserved the reservation of each issue of
	// it whose hand-over is not still to decide, and queues what finishes each
	// (reserveIssuesOfTreeCleanups).
	convertReservations
	// convertTreeDraws copies each tree's tree-unit draws onto its started issues
	// (record.CarryTreeDraws).
	convertTreeDraws
	// convertClaimGates marks each claim whose issue holds its own gate as holding it
	// (markClaimsOfGateHolders).
	convertClaimGates
	// convertMembersBack brings back each member that its root's departure suspended under the
	// release before (bringMembersBack).
	convertMembersBack
	// convertHandOvers finishes each hand-over whose write landed and cancels the rest
	// (convertHandOvers).
	convertHandOvers
	// convertOldRows deletes the rows only the release before runs (record.DeleteReleaseBeforeRows).
	convertOldRows
	// convertRootsUnderParents tells R18's notice of each root the release before ran under a
	// parent in Legion's work (tellRootsUnderParents).
	convertRootsUnderParents
	// convertRecord writes when the conversion ended.
	convertRecord
)

// conversionStep is one numbered step of the conversion: what it is called in the log and in an
// error, and what it writes, in the transaction the step runs in.
type conversionStep struct {
	n    int
	name string
	run  func(ctx context.Context, tx pgx.Tx) error
}

// storeConversionSteps are the steps that read only the store, 1 to 7, in order.
func (w *workflowRuntime) storeConversionSteps() []conversionStep {
	return []conversionStep{
		{convertFences, "fence the rows the release before queued", func(ctx context.Context, tx pgx.Tx) error {
			stamped, err := record.StampReleaseBeforeFences(ctx, tx, w.records, w.dispatchProject)
			w.logConverted(ctx, "rows fenced", stamped)
			return err
		}},
		{convertCarriedRoots, "carry each handed-over root onto its record", func(ctx context.Context, tx pgx.Tx) error {
			carried, err := record.CarryHandedOverRoots(ctx, tx, w.dispatchProject)
			w.logConverted(ctx, "roots carried", carried)
			return err
		}},
		{convertLingers, "retain each issue a linger covered", func(ctx context.Context, tx pgx.Tx) error {
			retained, err := record.RetainLingers(ctx, tx, w.records, w.dispatchProject, w.clockNow())
			w.logConverted(ctx, "issues retained", int64(len(retained)))
			return err
		}},
		{convertOperatorHolds, "hold each issue of a tree the operator holds", func(ctx context.Context, tx pgx.Tx) error {
			held, err := record.HoldIssuesOfOperatorTrees(ctx, tx, w.dispatchProject)
			w.logConverted(ctx, "issues the operator holds", held)
			return err
		}},
		{convertReservations, "reserve each issue of a tree cleanup", w.reserveIssuesOfTreeCleanups},
		{convertTreeDraws, "carry each tree's draws onto its started issues", func(ctx context.Context, tx pgx.Tx) error {
			carried, err := record.CarryTreeDraws(ctx, tx, w.dispatchProject)
			w.logConverted(ctx, "draws carried", carried)
			return err
		}},
		{convertClaimGates, "mark each claim whose issue holds its own gate", w.markClaimsOfGateHolders},
	}
}

// listedConversionSteps are the steps that read the listing and the copy it filled, 8 to 11, and
// the record, 12, in order. listing is the boot's listing of the project, by key.
func (w *workflowRuntime) listedConversionSteps(listing map[string]dispatch.IssueSummary) []conversionStep {
	return []conversionStep{
		{convertMembersBack, "bring back each member its root's departure suspended", w.bringMembersBack},
		{convertHandOvers, "finish or cancel each hand-over", func(ctx context.Context, tx pgx.Tx) error {
			return w.convertHandOvers(ctx, tx, listing)
		}},
		{convertOldRows, "delete the rows only the release before runs", func(ctx context.Context, tx pgx.Tx) error {
			deleted, err := record.DeleteReleaseBeforeRows(ctx, tx, w.dispatchProject)
			w.logConverted(ctx, "rows deleted", deleted)
			return err
		}},
		{convertRootsUnderParents, "tell each root under a parent in Legion's work", w.tellRootsUnderParents},
		{convertRecord, "record the conversion", func(context.Context, pgx.Tx) error { return nil }},
	}
}

// logConverted writes, once the step commits, how much of what the step converts it found.
func (w *workflowRuntime) logConverted(ctx context.Context, what string, count int64) {
	if count == 0 {
		return
	}
	store.OnCommit(ctx, func() {
		w.log.Info("the first boot's conversion: "+what, "project", w.dispatchProject, "count", count)
	})
}

// readConversion is the project's record of its conversion (record.ReadTreeConversion), in a
// transaction of its own.
func (w *workflowRuntime) readConversion(ctx context.Context) (record.TreeConversion, error) {
	var conversion record.TreeConversion
	err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		conversion, err = record.ReadTreeConversion(ctx, tx, w.dispatchProject)
		return err
	})
	return conversion, err
}

// convertStore runs the steps of the conversion that read only the store (1 to 7) that the record
// does not hold yet, and leaves the runtime converting (w.converting) until the boot's reconcile
// has run the rest (convertListed). A project whose conversion is finished runs nothing. run calls
// it once the store has migrated and the workflow is open, before anything decodes a queued row.
func (w *workflowRuntime) convertStore(ctx context.Context) error {
	conversion, err := w.readConversion(ctx)
	if err != nil {
		return fmt.Errorf("read the conversion of %s: %w", w.dispatchProject, err)
	}
	if conversion.Converted() {
		return nil
	}
	w.converting = true
	w.log.Info("the first boot on this release converts what the release before stored", "project", w.dispatchProject,
		"steps", convertRecord, "done", conversion.Step)
	return w.runConversionSteps(ctx, conversion.Step, w.storeConversionSteps())
}

// convertListed is the boot's reconcile of a project whose conversion is not finished: the copy of
// Dispatch's issues is filled from issues, the boot's listing, in a transaction of its own; the
// steps that read the listing or the copy and the record run (8 to 12) where the record does not
// hold them yet; and then the first admission pass, in a transaction of its own. A retry of the
// reconcile reads the record again, so it runs no step twice. It answers the keys that pass
// refused (admitListing).
func (w *workflowRuntime) convertListed(ctx context.Context, issues []dispatch.IssueSummary) ([]string, error) {
	listed, err := w.prepareListing(ctx, issues, nil, true)
	if err != nil {
		return nil, err
	}
	if err := w.listingTx(ctx, func(scoped context.Context, tx pgx.Tx) error {
		return w.fillCopy(scoped, tx, listed.scope, true)
	}); err != nil {
		return nil, fmt.Errorf("fill the copy of Dispatch's issues: %w", err)
	}
	conversion, err := w.readConversion(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the conversion of %s: %w", w.dispatchProject, err)
	}
	byKey := make(map[string]dispatch.IssueSummary, len(issues))
	for _, summary := range issues {
		byKey[summary.Key] = summary
	}
	if err := w.runConversionSteps(ctx, conversion.Step, w.listedConversionSteps(byKey)); err != nil {
		return nil, err
	}
	w.converting = false
	w.log.Info("the first boot's conversion of what the release before stored is finished", "project", w.dispatchProject)
	return w.admitListing(ctx, listed, true)
}

// runConversionSteps runs each of steps the record does not hold yet (its number above done), in
// order, each in its own transaction (runConversionStep), and after each that commits tells the
// test's hook (afterConversionStep), whose error stops the boot there.
func (w *workflowRuntime) runConversionSteps(ctx context.Context, done int, steps []conversionStep) error {
	for _, step := range steps {
		if step.n <= done {
			continue
		}
		if err := w.runConversionStep(ctx, step); err != nil {
			return err
		}
		if w.afterConversionStep != nil {
			if err := w.afterConversionStep(step.n); err != nil {
				return fmt.Errorf("stop after step %d of the conversion of %s: %w", step.n, w.dispatchProject, err)
			}
		}
	}
	return nil
}

// runConversionStep runs step in a transaction of its own (store.Transact), under the lock every
// fact applies under, which records the step and settles what its writes end as a fact does
// (intake.OpenSettlers); the last step writes when the conversion ended. The line that says a step
// ran is a hook of the step's commit (store.OnCommit), as each step's own lines are
// (logConverted).
func (w *workflowRuntime) runConversionStep(ctx context.Context, step conversionStep) error {
	var convertedAt *time.Time
	if step.n == convertRecord {
		now := w.clockNow().UTC()
		convertedAt = &now
	}
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, w.handlers...)
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the step with every fact: %w", err)
		}
		if err := step.run(ctx, tx); err != nil {
			return err
		}
		if err := record.RecordTreeConversionStep(ctx, tx, w.dispatchProject, step.n, convertedAt); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			w.log.Info("the first boot's conversion ran a step", "project", w.dispatchProject, "step", step.n, "what", step.name)
		})
		return settle(ctx, tx)
	}); err != nil {
		return fmt.Errorf("step %d of the first boot's conversion of %s (%s): %w", step.n, w.dispatchProject, step.name, err)
	}
	return nil
}

// reserveIssuesOfTreeCleanups is step 5: each tree cleanup the release before reserved and has not
// confirmed becomes the reservation of each issue of that tree's lifecycle
// (record.ReserveIssuesOfTreeCleanups), and what finishes each is queued
// (queueReservedCleanups). An issue whose hand-over the release before would still apply once its
// write landed (handOverStands) is left out, with its lifecycle opened: whether it carries on as a
// root of its own is step 9's, which reads the listing, and that step reserves it where it does
// not (convertHandOvers). A reservation would hold such an issue's start and every launch of its
// claims until a cleanup that its next run fences out confirms, which none ever does.
func (w *workflowRuntime) reserveIssuesOfTreeCleanups(ctx context.Context, tx pgx.Tx) error {
	handovers, err := record.ReleaseBeforeHandovers(ctx, tx, w.dispatchProject)
	if err != nil {
		return err
	}
	var undecided []string
	for _, handover := range handovers {
		issue, err := w.records.Issue(ctx, tx, handover.Issue)
		if err != nil {
			return err
		}
		if issue == nil {
			continue
		}
		stands, err := w.handOverStands(ctx, tx, *issue, handover)
		if err != nil {
			return err
		}
		if stands {
			undecided = append(undecided, issue.Key)
		}
	}
	reserved, err := record.ReserveIssuesOfTreeCleanups(ctx, tx, w.dispatchProject, undecided)
	if err != nil {
		return err
	}
	w.logConverted(ctx, "issues reserved", int64(len(reserved)))
	return w.queueReservedCleanups(ctx, tx, reserved)
}

// queueReservedCleanups queues what finishes each reservation the conversion made of an issue a
// tree cleanup of the release before covered. That release finished such a cleanup through its
// tree's close rows, which step 10 deletes, so for each issue the workflow reserved and records,
// each claim of it that has not retired is closed (an issue_close row, which retires it with its
// session kept, as the tree's close did) and its own cleanup is queued (an issue_suspend row, whose
// executor finishes a reserved cleanup once every claim of the issue has retired: it deletes the
// issue's pod and confirms the reservation, the outbox's cleanupClosedIssue). Each row is fenced at
// the run and the phase entry the issue stands at. A reservation the operator took is finished by
// the operator's own close, as the release before's was, so nothing is queued for it.
func (w *workflowRuntime) queueReservedCleanups(ctx context.Context, tx pgx.Tx, reserved []record.ReservedIssue) error {
	for _, r := range reserved {
		if r.Authority != treelifecycle.AuthorityWorkflow {
			continue
		}
		issue, err := w.records.Issue(ctx, tx, r.Issue)
		if err != nil {
			return err
		}
		if issue == nil {
			continue
		}
		live, err := w.liveRoles(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		for _, role := range live {
			if err := record.EnqueueFenced(ctx, tx, w.records, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree,
				Role: role, Generation: max(issue.Generation, 1)}, w.clockNow()); err != nil {
				return err
			}
		}
		treeGeneration := issue.Generation
		root, err := w.records.Issue(ctx, tx, issue.Tree)
		if err != nil {
			return err
		}
		if root != nil {
			treeGeneration = root.Generation
		}
		if err := record.EnqueueFenced(ctx, tx, w.records, issue.Key, record.IssueSuspend{Tree: issue.Tree,
			Generation: max(issue.Generation, 1), TreeGeneration: max(treeGeneration, 1)}, w.clockNow()); err != nil {
			return err
		}
	}
	return nil
}

// liveRoles is each role of key whose stored claim has not retired, in claim.Roles' order: the
// claims a cleanup of the issue waits for.
func (w *workflowRuntime) liveRoles(ctx context.Context, tx pgx.Tx, key string) ([]claim.Role, error) {
	tokens := make([]claim.Token, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		token, err := claim.NewToken(w.projectID, key, role)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	claims, err := w.records.RoleClaims(ctx, tx, tokens)
	if err != nil {
		return nil, err
	}
	var live []claim.Role
	for i, role := range claim.Roles {
		if held := claims[tokens[i]]; held != nil && held.State != supervise.StateRetired {
			live = append(live, role)
		}
	}
	return live, nil
}

// markClaimsOfGateHolders is step 7: each claim no start of this release has marked takes the mark
// that its issue holds its own gate (record.MarkClaimsOfGateHolders), by the issue's record or by
// the release before's rule for a root, a claim whose issue is the tree it stores. supervise's
// treeRoot reads the mark, so an architect left unmarked is no tree's root: an operator's close of
// its tree is refused, and its stop and close act on it as on a member. The migration of
// 20261122154645 marks each claim it finds by the release before's rule, and the workflow's start
// writes the mark again from the Dispatch chain at each launch (supervise.Machine.MarkLaunch). But
// a claim the release before wrote while a rollback held takes the mark's default, false, and the
// workflow never starts an operator's claim, so nothing else would ever mark such a claim; this
// step runs again on the first boot after a rollback whose way back deleted the record.
//
// It reads only the store because supervision reads every claim before the boot's reconcile runs
// the steps that read the listing (openSupervision), and builds its machines from that reading: a
// claim a later step marked would be built unmarked, and its first write would store it unmarked
// again. So no claim of a root a landed hand-over makes, which step 9 marks as holding its own
// gate, is marked here: its architect's claim is made by the start the first admission pass
// queues for it (workflow.Engine.StartArchitect, from admit's promotion of a queued root), which
// reads the gate on the Dispatch chain and writes the mark (the outbox's start, launchOf), and
// each start of a claim it carried marks that claim the same way.
func (w *workflowRuntime) markClaimsOfGateHolders(ctx context.Context, tx pgx.Tx) error {
	marked, err := record.MarkClaimsOfGateHolders(ctx, tx, w.dispatchProject)
	w.logConverted(ctx, "claims marked as holding their issue's gate", marked)
	return err
}

// bringMembersBack is step 8 (item 46 of dispatch://INFRA-4730; R20 and R21 of
// dispatch://INFRA-3320). Each member that its root's departure left suspended under the release
// before (record.ReadMembersBack) comes back into Legion's work as a return brings back an issue a
// stop left in its phase (workflow.Engine.ReturnToWork): it resumes the phase it stands in at a new
// phase entry, with its generation and its sessions, and every other claim of it that has run
// comes back too. No event brings the member back, so the return reads Dispatch as the record last
// read it (recordedReading). The return judges R9 before it starts anything
// (workflow.Engine.ReturnMemberToWork): where an open Dispatch child of the member holds the start
// of the phase it stands in, planning through accepting, it waits in awaiting_children holding
// that start instead, and no claim of it starts, nor its step.
//
// The step runs before the hand-overs (step 9), so it reads the store as the release before left
// it, as `legion members-back` reads it before the upgrade (ListMembersBack): the members it brings
// back are the members that listing prints. It logs one line for each member and one count.
func (w *workflowRuntime) bringMembersBack(ctx context.Context, tx pgx.Tx) error {
	members, err := record.ReadMembersBack(ctx, tx, w.dispatchProject)
	if err != nil {
		return err
	}
	for _, member := range members {
		issue, err := w.records.Issue(ctx, tx, member.Key)
		if err != nil {
			return err
		}
		if issue == nil {
			return fmt.Errorf("bring back %s: the daemon records no such issue", member.Key)
		}
		waits, err := w.engine.ReturnMemberToWork(ctx, tx, *issue, recordedReading(*issue))
		if err != nil {
			return fmt.Errorf("bring back %s: %w", member.Key, err)
		}
		store.OnCommit(ctx, func() {
			w.log.Info("the first boot's conversion brought back a member its root's departure suspended", "project", w.dispatchProject,
				"issue", member.Key, "phase", member.Phase, "root", member.Root, "left", member.Left, "awaitingChildren", waits)
		})
	}
	w.logConverted(ctx, "members brought back", int64(len(members)))
	return nil
}

// recordedReading is issue's own record of Dispatch as an observation of it: the reading that a
// return is handed where no event brings the issue back (bringMembersBack). It holds the status
// that Dispatch last showed where that is a status in work, else the status the record holds, and
// the sequence the record last read, so it moves no fence.
func recordedReading(issue record.Issue) intake.DispatchIssue {
	status := issue.DispatchStatus
	if !record.InWorkStatus(status) {
		status = issue.Status
	}
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return intake.DispatchIssue{Key: issue.Key, Seq: issue.LastDispatchSeq, Status: status, Title: issue.Title, Parent: parent,
		Rank: issue.Rank, Priority: issue.Priority, HandedOver: issue.HandedOver, ResolvedRepo: issue.Repo}
}

// MembersBackListing is what `legion members-back` prints of a project (ListMembersBack): each
// member that the project's first boot on this release brings back, or nothing where the
// conversion has brought them back already. BroughtBack says that the conversion's step that
// brings them back has committed (bringMembersBack), and ConvertedAt is when the whole conversion
// ended, nil until it has.
type MembersBackListing struct {
	Project     string
	BroughtBack bool
	ConvertedAt *time.Time
	Members     []record.MemberBack
}

// ListMembersBack is the listing of project in tx, a read-only transaction on the store of the
// release before or of this release: the members that the conversion's step brings back, through
// the one reading that the step makes (record.ReadMembersBack), or none where the project's record
// of its conversion holds that step already.
func ListMembersBack(ctx context.Context, tx pgx.Tx, project string) (MembersBackListing, error) {
	conversion, err := record.ReadTreeConversionOfAnyRelease(ctx, tx, project)
	if err != nil {
		return MembersBackListing{}, err
	}
	listing := MembersBackListing{Project: project, BroughtBack: conversion.Step >= convertMembersBack, ConvertedAt: conversion.ConvertedAt}
	if listing.BroughtBack {
		return listing, nil
	}
	listing.Members, err = record.ReadMembersBack(ctx, tx, project)
	return listing, err
}

// convertHandOvers is step 9: the listing's reading of each hand-over's issue decides it. Where
// the hand-over's write landed — Dispatch shows the issue todo with the label, which is what the
// release before's own outbox read to apply it, and the release before would still have applied
// it (handOverStands) — the issue is made the root of its own the release before's
// consumeHandover would have made it (finishHandOver), and carries on as a run of its own, its
// cleanup not reserved (step 5 left it out). Every other hand-over is cancelled: the issue is left
// to R19 of dispatch://INFRA-3320 and waits for a person's handover, and where a tree cleanup the
// release before reserved covers it, its own cleanup is reserved and what finishes it queued, as
// step 5 does for the tree's other issues (record.ReserveIssueOfTreeCleanup, queueReservedCleanups).
// Either way the release before's row goes. No hand-over is made after this step, so the later
// echo of a landed one's write is the daemon's own write, which finds the record already at the
// status it carries and changes nothing.
func (w *workflowRuntime) convertHandOvers(ctx context.Context, tx pgx.Tx, listing map[string]dispatch.IssueSummary) error {
	handovers, err := record.ReleaseBeforeHandovers(ctx, tx, w.dispatchProject)
	if err != nil {
		return err
	}
	for _, handover := range handovers {
		issue, err := w.records.Issue(ctx, tx, handover.Issue)
		if err != nil {
			return err
		}
		landed := false
		if issue != nil {
			if landed, err = w.handOverLanded(ctx, tx, *issue, handover, listing); err != nil {
				return err
			}
		}
		if landed {
			if err := w.finishHandOver(ctx, tx, *issue, handover); err != nil {
				return err
			}
		} else {
			reserved, err := record.ReserveIssueOfTreeCleanup(ctx, tx, w.dispatchProject, handover.Issue)
			if err != nil {
				return err
			}
			if err := w.queueReservedCleanups(ctx, tx, reserved); err != nil {
				return err
			}
		}
		if err := record.DeleteReleaseBeforeHandover(ctx, tx, handover.Issue); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			w.log.Info("the first boot's conversion decided a hand-over of the release before", "project", w.dispatchProject,
				"issue", handover.Issue, "tree", handover.Tree, "linger", handover.Generation, "landed", landed)
		})
	}
	return nil
}

// handOverLanded says whether handover, of issue, is one whose write landed and which the release
// before would still have applied (convertHandOvers).
func (w *workflowRuntime) handOverLanded(ctx context.Context, tx pgx.Tx, issue record.Issue, handover record.ReleaseBeforeHandover,
	listing map[string]dispatch.IssueSummary) (bool, error) {
	summary, listed := listing[issue.Key]
	if !listed || summary.Status != "todo" || !summary.HandedOver {
		return false, nil
	}
	return w.handOverStands(ctx, tx, issue, handover)
}

// handOverStands says whether handover, of issue, is one the release before would still have
// applied once its write landed: its fence is not cancelled, the issue still stands in the tree it
// names, and that tree's root still lingers at the generation it names. Only the listing decides
// such a hand-over (handOverLanded), so step 5 leaves its issue to step 9
// (reserveIssuesOfTreeCleanups).
func (w *workflowRuntime) handOverStands(ctx context.Context, tx pgx.Tx, issue record.Issue, handover record.ReleaseBeforeHandover) (bool, error) {
	if handover.Cancelled || issue.Tree != handover.Tree {
		return false, nil
	}
	root, err := w.records.Issue(ctx, tx, handover.Tree)
	if err != nil {
		return false, err
	}
	return root != nil && root.LingerUntil != nil && root.Generation == handover.Generation, nil
}

// finishHandOver makes issue, whose hand-over landed, what the release before's consumeHandover
// made of it, on this release's own columns alone: a root of its own carrying its run and its gate
// into a new generation of its run, queued todo with the label for its first start under this
// release, which starts its architect and, where it carries a run, resumes that run in its phase
// (admit's promotion, workflow.Engine.StartCarriedRun). Its record holds that it was carried in at
// that generation and whether its old root's gate came with it (Issue.CarriedGeneration,
// CarriedGate), that it holds its own gate, and, where it carries a run, that its run has stood in
// a phase of its own (Issue.TakenBack). It writes neither issues.tree, which is written once, nor
// handed_over_from, nor a handed_over_roots row: those are the release before's, and its rollback's
// way back writes them (R22 of dispatch://INFRA-3320).
//
// What carries is the release before's: an issue that ran a phase (Issue.CarriesRun) keeps that
// phase, its pull request, moved to the new generation with its head run gone, its old root's
// gate rows as that release carried them (carriedGate), and its plan, copied to the new
// generation with each task where it stood (record.Store's CarryPlan with carried), as that
// release's readmit of a carried orphan copies it. So a new run whose owner gave a plan walks
// that plan, and not the default steps. The new run's plan node is written after the copy, so it
// names the plan the run carries (workflow.Engine.WritePlan). One that ran none starts at
// admitted with nothing held, and its old generation's facts are cleared. Either way it takes the
// tree-unit draws of the tree it leaves, its handout and its catch-up cursor start again, and the
// record marks it moved.
func (w *workflowRuntime) finishHandOver(ctx context.Context, tx pgx.Tx, issue record.Issue, handover record.ReleaseBeforeHandover) error {
	if issue.Generation == math.MaxUint64 {
		return fmt.Errorf("finish the hand-over of %s: generation overflows", issue.Key)
	}
	now := w.clockNow()
	carry := issue.CarriesRun()
	var gates map[record.DesignStep]record.DesignGate
	var pr *record.PullRequest
	if carry {
		var err error
		if gates, err = w.records.GatesOf(ctx, tx, handover.Tree); err != nil {
			return fmt.Errorf("read the gate %s's hand-over carries: %w", issue.Key, err)
		}
		if pr, err = w.records.PullRequest(ctx, tx, issue.Key); err != nil {
			return fmt.Errorf("read the pull request %s's hand-over carries: %w", issue.Key, err)
		}
	} else {
		issue.Phase, issue.Hold, issue.Awaiting, issue.ChildrenWait = phase.Admitted, nil, nil, nil
	}
	oldGeneration := issue.Generation
	issue.Generation++
	issue.Status, issue.DispatchStatus, issue.HandedOver = "todo", "todo", true
	issue.TakenBack, issue.HoldsGate = carry, true
	issue.CarriedGeneration, issue.CarriedGate = issue.Generation, len(gates) > 0
	if err := w.records.PutIssue(ctx, tx, issue); err != nil {
		return fmt.Errorf("finish the hand-over of %s: %w", issue.Key, err)
	}
	if err := w.records.MarkIssueMoved(ctx, tx, issue.Key, now); err != nil {
		return err
	}
	if err := w.records.StartHandout(ctx, tx, issue.Key); err != nil {
		return fmt.Errorf("finish the hand-over of %s: %w", issue.Key, err)
	}
	if err := w.records.RestartCaughtUp(ctx, tx, issue.Key); err != nil {
		return fmt.Errorf("finish the hand-over of %s: %w", issue.Key, err)
	}
	if _, err := w.records.CarryAssignments(ctx, tx, issue.Key, []string{handover.Tree}); err != nil {
		return err
	}
	if !carry {
		return w.records.ClearGeneration(ctx, tx, issue.Key, now)
	}
	if err := w.records.CarryPlan(ctx, tx, issue.Key, oldGeneration, issue.Generation, true); err != nil {
		return fmt.Errorf("carry the plan of %s into its new generation: %w", issue.Key, err)
	}
	if err := w.engine.WritePlan(ctx, tx, issue); err != nil {
		return fmt.Errorf("write the plan of %s's carried run: %w", issue.Key, err)
	}
	if pr != nil {
		// The carried pull request keeps its CI settlement fence: its Generation is the listener's
		// generation of the settlement that stands, not the issue's, so a settlement of its head
		// from before the carry, delivered late or replayed, is as stale after it as it was before.
		// Its head run goes, as with a new generation (record.ClearGeneration), since its replay
		// would put the old fence back; the write deletes the run's own memory.
		pr.Run = nil
		if err := w.records.PutPullRequest(ctx, tx, *pr); err != nil {
			return fmt.Errorf("carry the pull request of %s into its new generation: %w", issue.Key, err)
		}
	}
	if len(gates) == 0 {
		return nil
	}
	held, err := w.records.GatesOf(ctx, tx, issue.Key)
	if err != nil {
		return fmt.Errorf("read the gates of %s before its carry: %w", issue.Key, err)
	}
	for _, step := range slices.Sorted(func(yield func(record.DesignStep) bool) {
		for step := range gates {
			if !yield(step) {
				return
			}
		}
	}) {
		carried, err := w.carriedGate(ctx, tx, issue.Key, gates[step])
		if err != nil {
			return fmt.Errorf("carry the gate of %s: %w", issue.Key, err)
		}
		if carried == nil {
			continue
		}
		if err := w.records.PutGate(ctx, tx, *carried); err != nil {
			return fmt.Errorf("carry the gate of %s: %w", issue.Key, err)
		}
	}
	return record.PublishChangedGates(ctx, w.records, tx, w.gatedSteps, issue.Key, held, now)
}

// carriedGate is gate, a step of the old root's gate, as the release before's carry wrote it onto
// key (R2 and R8 of dispatch://INFRA-3320), or nil where the carry brings no row for that step: a
// step an ancestor's approval opened arrives with its basis unless that ancestor is done or
// accepted (record.BasisAcceptance), where no row comes and key's architect registers its own; a
// step that waits on an ancestor's open approval request arrives closed with no basis
// (record.EndPendingBasis); and key rests on each ancestor the old root rested on that is neither
// done nor accepted.
func (w *workflowRuntime) carriedGate(ctx context.Context, tx pgx.Tx, key string, gate record.DesignGate) (*record.DesignGate, error) {
	gate.Issue = key
	standing := ""
	if gate.OnBasis() {
		standing = gate.Basis.Issue
		accepted, _, err := record.BasisAcceptance(ctx, w.records, tx, standing)
		if err != nil {
			return nil, err
		}
		if accepted != "" {
			return nil, nil
		}
	}
	gate = record.EndPendingBasis(gate)
	var rests []string
	for _, ancestor := range gate.RestsOn {
		if ancestor != standing {
			accepted, _, err := record.BasisAcceptance(ctx, w.records, tx, ancestor)
			if err != nil {
				return nil, err
			}
			if accepted != "" {
				continue
			}
		}
		rests = append(rests, ancestor)
	}
	gate.RestsOn = rests
	return &gate, nil
}

// tellRootsUnderParents is step 11 (R21 of dispatch://INFRA-3320): each recorded issue the release
// before ran as a root of its own (its stored tree is its own key) whose Dispatch parent, in the
// copy the boot's listing filled, is in Legion's work is told R18's notice through the path a
// change of parent takes (workflow.Engine.TellParentAtFirstBoot): the mark of the parent it was
// last told of is cleared first, since the release before's own notice already set it, and one
// re-parent seed names that parent as the new parent and no old parent, so its own architect and
// the architect above it are told once, and a later listing or event that repeats the parent tells
// nobody again.
func (w *workflowRuntime) tellRootsUnderParents(ctx context.Context, tx pgx.Tx) error {
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return err
	}
	byKey := make(map[string]record.Issue, len(issues))
	var roots []string
	for _, issue := range issues {
		byKey[issue.Key] = issue
		if issue.Key == issue.Tree {
			roots = append(roots, issue.Key)
		}
	}
	rows, err := w.records.DispatchIssuesOf(ctx, tx, roots)
	if err != nil {
		return err
	}
	parents := make(map[string]string, len(rows))
	for _, row := range rows {
		parents[row.Key] = row.Parent
	}
	slices.Sort(roots)
	var told int64
	for _, key := range roots {
		parent, recorded := byKey[parents[key]]
		if parents[key] == "" || !recorded || !record.InLegionsWork(parent) {
			continue
		}
		queued, err := w.engine.TellParentAtFirstBoot(ctx, tx, byKey[key], parent.Key)
		if err != nil {
			return err
		}
		if queued {
			told++
		}
	}
	w.logConverted(ctx, "roots under a parent in Legion's work told", told)
	return nil
}
