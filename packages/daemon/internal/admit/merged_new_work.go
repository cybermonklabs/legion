package admit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// readmitRequest is admission's door of a request for new work (D17) on an issue a re-admission
// would take: a root at done or lingering, a labelled child at done of a tree that does not run
// (an orphan), and a child at done that a hand-over makes a root of its own. The record decides
// (workflow's RouteRequest). Where the work merged, the issue's next change starts in readmit's
// continuation mode (continueChange). Where the request waits for its merge read, the observation
// is recorded with the status todo and nothing else moves. Otherwise the re-admission runs as
// today, its whole clear included (readmit).
func (a *Admission) readmitRequest(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed, request workflow.Request) error {
	route, err := a.engine.RouteRequest(ctx, tx, stored, request)
	if err != nil {
		return fmt.Errorf("route the request on %s: %w", stored.Key, err)
	}
	switch route {
	case workflow.RequestStartsChange:
		return a.continueChange(ctx, tx, stored, o, request)
	case workflow.RequestWaits:
		o.Status = "todo"
		return a.recordObservation(ctx, tx, stored, o)
	}
	return a.readmit(ctx, tx, stored, o, false)
}

// continueChange is readmit's continuation mode (D17, "The path of a request"): the issue stays the
// root of its tree, or becomes a root of its own where it is a child that leaves its tree, and its
// next change starts there (workflow's StartRequestedChange), which raises its own generation once
// and keeps its design gate.
//
// A root ends its linger, cancels the pending hand-overs of the generation that ended and frees
// its tree's slots under a pause (releaseTreeSlots). A child already handed stays its own root.
// The members are owed their return (OweMembers), which the promotion or the pass that finds the
// tree running makes. It writes no generation of its own and keeps no stale approval, since the
// gate stays, and it calls no StartHandout, no RestartCaughtUp and no ClearTreeGeneration. Where
// the tree's runtime still stands (its lifecycle is open under the workflow, a linger that has not
// ended), no promotion will come, so the root's architect, which the linger suspended, is started
// again here with no task: its pending task stands, and the next-change notice tells it of the
// request. Where the runtime has gone, the promotion starts the architect with the request's task.
//
// A child that leaves its tree keeps its Dispatch parent and records the tree it left, as an
// orphan's re-admission does: StopLeftTree, HandedOverFrom, DeleteHandover and CarryAssignments.
// Its new tree takes the old root's design gate by the carry's copy (carriedGate), its tree's run
// starts at the change's generation, and its hand-over row names that generation. The old tree,
// its root and its other members are untouched.
func (a *Admission) continueChange(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed, request workflow.Request) error {
	oldTree, oldGeneration := stored.Tree, stored.Generation
	orphan := !claim.IsTreeRoot(stored.Key, oldTree)
	if orphan {
		if err := a.engine.StopLeftTree(ctx, tx, stored); err != nil {
			return err
		}
	}
	stored.Project, stored.Title, stored.HandedOver = a.project, o.Title, true
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank, stored.Priority, stored.Resolved = o.Rank, o.Priority, o.Resolved
	stored.Status, stored.DispatchStatus = "todo", "todo"
	stored.LingerUntil = nil
	stored.LastDispatchSeq = o.Seq
	stored.Ancestry = nil
	if orphan {
		stored.HandedOverFrom, stored.TakenBack = oldTree, false
	}
	// The tree does not move here: the change's own write moves it (ChangePlace.Tree), so an
	// orphan's tree run starts at the change's generation (record.Postgres.PutIssue).
	if err := a.engine.WriteIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record the request on %s: %w", stored.Key, err)
	}
	if err := a.store.OweMembers(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record the request on %s: %w", stored.Key, err)
	}
	var gates map[record.DesignStep]record.DesignGate
	if orphan {
		if err := a.store.DeleteHandover(ctx, tx, stored.Key); err != nil {
			return err
		}
		project, err := claim.ProjectToken(a.project)
		if err != nil {
			return fmt.Errorf("carry the experiments of %s: %w", stored.Key, err)
		}
		if _, err := a.store.CarryAssignments(ctx, tx, project, stored.Key, oldTree, a.declaredAssignments()); err != nil {
			return err
		}
		if gates, err = a.store.GatesOf(ctx, tx, oldTree); err != nil {
			return fmt.Errorf("read the gate %s leaves: %w", stored.Key, err)
		}
	} else {
		if err := a.store.CancelTreeHandovers(ctx, tx, oldTree, oldGeneration); err != nil {
			return err
		}
		if err := a.releaseTreeSlots(ctx, tx, stored.Key); err != nil {
			return fmt.Errorf("record the request on %s: %w", stored.Key, err)
		}
	}
	if err := a.engine.StartRequestedChange(ctx, tx, stored.Key, request, workflow.ChangePlace{Tree: stored.Key, Parent: stored.Parent}); err != nil {
		return fmt.Errorf("start the next change of %s: %w", stored.Key, err)
	}
	started, err := a.store.Issue(ctx, tx, stored.Key)
	if err != nil || started == nil {
		return err
	}
	store.OnCommit(ctx, func() {
		a.log.Info("admission: a request on a merged issue starts its next change", "issue", started.Key, "tree", started.Tree,
			"generation", started.Generation, "from_tree", oldTree)
	})
	if orphan {
		return a.carryGateTo(ctx, tx, *started, gates)
	}
	lifecycle, _, err := record.RootFacts(ctx, a.store, tx, *started, a.engine.GatedSteps())
	if err != nil || !lifecycle.OpenUnderWorkflow() {
		return err
	}
	return a.enqueue(ctx, tx, started.Key, record.SuperviseRequest{Op: "start", Tree: started.Tree, Role: claim.RoleArchitect,
		Generation: started.GenerationOf("start", claim.RoleArchitect)}, a.now())
}

// carryGateTo copies gates, the design gate of the tree root a child leaves, onto root, the child
// as the root of its own that its next change made, as an orphan's re-admission copies it (readmit,
// carriedGate), and records the hand-over of that generation (record.HandedOverRoot): its
// architect then reads that a hand-over carried it in, with the gate where one came.
func (a *Admission) carryGateTo(ctx context.Context, tx pgx.Tx, root record.Issue, gates map[record.DesignStep]record.DesignGate) error {
	if len(gates) > 0 {
		held, err := a.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return fmt.Errorf("read the gates of %s: %w", root.Key, err)
		}
		for _, gate := range gates {
			carried, err := a.carriedGate(ctx, tx, root.Key, gate)
			if err != nil {
				return fmt.Errorf("carry gate to %s: %w", root.Key, err)
			}
			if carried == nil {
				continue
			}
			if err := a.store.PutGate(ctx, tx, *carried); err != nil {
				return fmt.Errorf("carry gate to %s: %w", root.Key, err)
			}
		}
		if err := a.store.BaselineTold(ctx, tx, root.Key, record.AudienceArchitect, root.Key, a.now()); err != nil {
			return fmt.Errorf("take the baseline of %s's architect: %w", root.Key, err)
		}
		if err := record.PublishChangedGates(ctx, a.store, tx, a.engine.GatedSteps(), root.Key, held, a.now()); err != nil {
			return fmt.Errorf("publish the gates carried to %s: %w", root.Key, err)
		}
	}
	return a.store.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: root.Key, Generation: root.Generation, GateCarried: len(gates) > 0})
}

// strandedRequest is a todo observation of a child at done whose tree is not live and which no
// label hands over (D17, "A stranded child at done with todo"): the request is accepted, and the
// record decides it now (workflow's RouteRequest), so a merge it knows is saved, a request on a
// closed issue with no proof of its merge queues its merge read, and a person's confirmation is
// taken. The child stays where it stands, recorded todo, until its tree's run places it
// (reenterStrandedChildren). An edit of a child already todo is no request.
func (a *Admission) strandedRequest(ctx context.Context, tx pgx.Tx, stored record.Issue, status string, request workflow.Request) error {
	if status != "todo" || stored.Status == "todo" || stored.Phase != phase.Done || claim.IsTreeRoot(stored.Key, stored.Tree) {
		return nil
	}
	root, err := a.store.Issue(ctx, tx, stored.Tree)
	if err != nil || root == nil {
		return err
	}
	live, err := record.TreeLive(ctx, a.store, tx, *root, a.engine.GatedSteps())
	if err != nil || live {
		return err
	}
	_, err = a.engine.RouteRequest(ctx, tx, stored, request)
	return err
}

// applyMergeRead is admission's half of the merge read's answer (intake.MergeRead), after the
// workflow's, which saved the merge and placed a child of a live tree: where the request still
// stands (workflow's MergeReadRequest), a root at done or lingering, and a labelled child of a
// tree that does not run, start their next change in readmit's continuation mode with the request
// as the row stands now. An unlabelled child of such a tree stays where it stands, and its tree's
// run places it (reenterStrandedChildren). The row ends either way: the record now holds the merge.
func (a *Admission) applyMergeRead(ctx context.Context, tx pgx.Tx, fact intake.MergeRead) error {
	stored, request, standing, err := a.engine.MergeReadRequest(ctx, tx, fact)
	if err != nil || !standing {
		return err
	}
	if _, err := a.store.CancelMergeRead(ctx, tx, stored.Key); err != nil {
		return err
	}
	o := observed{Title: stored.Title, Parent: deref(stored.Parent), Rank: stored.Rank, Priority: stored.Priority, Resolved: stored.Resolved,
		Status: "todo", Shown: stored.DispatchStatus, Ancestry: stored.Ancestry, HandedOver: stored.HandedOver, Seq: stored.LastDispatchSeq}
	if stored.Readmittable() {
		return a.continueChange(ctx, tx, stored, o, request)
	}
	if claim.IsTreeRoot(stored.Key, stored.Tree) || !stored.HandedOver {
		return nil
	}
	root, err := a.store.Issue(ctx, tx, stored.Tree)
	if err != nil {
		return err
	}
	if root != nil {
		live, err := record.TreeLive(ctx, a.store, tx, *root, a.engine.GatedSteps())
		if err != nil || live {
			return err
		}
	}
	return a.continueChange(ctx, tx, stored, o, request)
}
