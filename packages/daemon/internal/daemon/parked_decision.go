package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// parkedDecisionSource is the processed-event source of the parked-decision notices the outbox
// writes. Its event id is the dedupe key the undelivered notice went under (record.Notice.
// Published: a re-held copy's is its original row's), so one decision writes one notice however
// many rows and reports carry it, and however often a row whose finish did not commit runs again.
// Nothing downstream can: each relay is a controller row of its own, published under its own
// row's key, so neither the listener's delivery caches nor a host's dedupe
// (dedupeKeyNamesItsEvent, packages/contracts/src/envelope.ts) can tell that two rows relay one
// decision.
const parkedDecisionSource = "parked-decision"

// keptJSON encodes what a start row's task names of what its root keeps, for the delivery to carry
// and the claim store to persist (supervise.Delivery.Kept). A task that names nothing carries
// nothing.
func keptJSON(kept *record.KeptDecisions) ([]byte, error) {
	if kept == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("encode the decisions a start's task names: %w", err)
	}
	return encoded, nil
}

// parkDecision hands the person's spec decision a notice reports to the one rule for a decision
// nobody in its tree hears (record.RelayParkedDecision): the project's controller is told, in a
// parked-decision notice of its own, and the decision is kept for the root's next architect start.
// It runs in a transaction of its own that marks key processed and owns the commit hook the write
// registers (projectRecords, store.OnCommit), so the notice's lane wakes as it commits. It does
// nothing for a kind that is no spec decision, nor for a changes request the person has since
// replaced with a newer one of the same document (workflow.ChangesSuperseded): the workflow tells
// such a request when it arrives after the newer one — late, or replayed from a message the intake
// gave up on, however late — and kept now it would take the newer decision's place, and its
// claim, under the document's ref. Every outcome logs one line that pairs the
// undelivered row with what became of its decision: sent, kept for a root that waits for its slot,
// not relayed (by no person, on a root that is no parked tree, on a tree that hears it, on a root
// with no record, or replaced since), or handled before under the same key, by an earlier run or
// another row carrying the same notice, whose own line said what became of it. The line is a hook
// of the transaction, so it is logged once the transaction has committed.
//
// A decision that is no longer the person's word is not relayed: the gates of the issue it was
// made on, as the record holds them now, are asked (workflow.NoticeSuperseded), and the
// controller is not told to act on a review the person's own approval took the place of, nor on
// an approval the gate no longer holds. The same read of the gates says whether a newer changes
// request replaced it (workflow.ChangesSuperseded). The relay carries the notice's facts of the
// review (record.RelayParkedDecision), so the publish of a relayed changes request asks again
// of the same decision (controllerNotice).
func (r *outbox) parkDecision(ctx context.Context, issue, root string, notice record.Notice, key string, row int64) error {
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		return nil
	}
	args := []any{"row", row, "kind", notice.Kind, "issue", issue, "root", root, "key", key}
	now := r.now().UTC()
	outcome, fresh, recorded, replaced := record.RelayNone, false, false, false
	var superseded *workflow.Supersession
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		store.OnCommit(ctx, func() {
			switch {
			case !fresh:
				r.log.Info("outbox parked decision handled before under this key", args...)
			case !recorded:
				r.log.Info("outbox parked decision not relayed: its root has no record", args...)
			case superseded != nil && superseded.By == nil:
				r.log.Info("outbox parked decision not relayed: the gate no longer holds the approval it reports",
					append(args, "version", notice.Version)...)
			case superseded != nil:
				r.log.Info("outbox parked decision not relayed: the person's approval followed it",
					append(args, "version", notice.Version, "approved", superseded.By.Version)...)
			case replaced:
				r.log.Info("outbox parked decision not relayed: a newer changes request of its document replaced it", append(args, "ref", notice.Ref, "version", notice.Version)...)
			case outcome == record.RelaySent:
				r.log.Info("outbox parked decision sent to the controller", args...)
			case outcome == record.RelayKept:
				r.log.Info("outbox parked decision kept for its root's start, which waits for its slot", args...)
			case outcome == record.RelayNoPerson, outcome == record.RelayNoPersonWaiting:
				r.log.Info("outbox parked decision not relayed: it names no person", append(args, "ref", notice.Ref)...)
			case outcome == record.RelayNotParked:
				r.log.Info("outbox parked decision not relayed: its root is not parked", args...)
			default:
				r.log.Info("outbox parked decision not relayed: its tree hears it", args...)
			}
		})
		marked, err := r.records.MarkProcessed(ctx, tx, parkedDecisionSource, key)
		if err != nil || !marked {
			return err
		}
		fresh = true
		stored, err := r.records.Issue(ctx, tx, root)
		if err != nil || stored == nil {
			return err
		}
		recorded = true
		gates, err := r.records.GatesOf(ctx, tx, issue)
		if err != nil {
			return err
		}
		if superseded = workflow.NoticeSuperseded(gates, r.gatedSteps, issue, notice, decision); superseded != nil {
			return nil
		}
		if decision == record.DecisionDesignChangesRequested {
			if replaced = workflow.ChangesSuperseded(gates, issue, notice); replaced {
				return nil
			}
		}
		// The decision is kept at Dispatch's mark of the review it reports (record.Notice.Review),
		// so a later approval is weighed against it by Dispatch's order and not by this clock;
		// this clock places it only where the notice carries no time of the review (one an
		// earlier release queued, or a Dispatch event that gave none). It is kept with the
		// document reviewed and its version (record.Notice.Artifact and Version), as its relay
		// carries them, so the kept decision and the relay are asked the same question of the
		// same review.
		at := notice.Review.At
		if at.IsZero() {
			at = now
		}
		outcome, err = record.RelayParkedDecision(ctx, tx, r.records, issue, *stored, true,
			record.ParkedDecision{Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: at, Seq: notice.Review.Seq,
				Artifact: notice.Artifact, Version: notice.Version},
			notice, r.gatedSteps)
		return err
	}); err != nil {
		return fmt.Errorf("send the controller the decision %s carried on %s: %w", key, issue, err)
	}
	return nil
}

// keptPodControllerSource is the processed-event source of the copies of workspace-kept and
// workspace-taken notices that the outbox queues for the controller (tellControllerOfKeptPod). Its
// event id is the dedupe key the undelivered notice went under (record.Notice.Published: a re-held
// copy's is its original row's), so one notice queues one controller row however often its row
// runs and however many copies carry it.
const keptPodControllerSource = "workspace-kept-controller"

// diskNotice is whether kind is a notice of an issue pod's disk that must not finish with nobody
// told: a pod the daemon keeps for its disk (record.WorkspaceKeptNotice), or a replacement that
// still took a disk that may hold work (record.WorkspaceTakenNotice, R7 of #5285).
// Where its owning architect will not hold its role again, it goes to the controller
// (tellControllerOfKeptPod).
func diskNotice(kind record.NoticeKind) bool {
	return kind == record.WorkspaceKeptNotice || kind == record.WorkspaceTakenNotice
}

// tellControllerOfKeptPod hands a notice of an issue pod's disk (diskNotice) whose owning
// architect will not hold its role again, for why, to the project's controller (the Legion
// reviewer's point 1 on R6 of #5285). The daemon counts a kept pod told when it
// queues the notice (store's recordDecline), and keeps the pod until a read finds it clean, and a
// workspace-taken notice is the one word of a disk that is gone, so neither must finish with
// nobody told: the controller gets a controller notice of its own with the notice's words
// (record.ControllerNotice, published on the controller topic by its own row, controllerNotice),
// queued once under key in a transaction that marks key processed, so a row whose finish did not
// commit runs again and queues nothing more. The undelivered row then finishes.
//
// It does nothing for a kind that is no disk notice, as parkDecision does nothing for a kind that
// is no spec decision, so every place a notice finishes undelivered because nobody will hold its
// architect's role calls both, before its own line: an architect that will not hold its role
// again (noticeAnswered), and one that stopped with its finished tree, found before a publish
// (deliverNotice, the Legion reviewer's point 3 on R6's review fixes) or before a re-hold
// (reholdNotice). Every outcome logs one line that pairs the undelivered row with what became of
// its notice, sent or handed over before under the same key, once the transaction has committed.
func (r *outbox) tellControllerOfKeptPod(ctx context.Context, issue string, row int64, notice record.Notice, key string,
	architect claim.Token, why string) error {
	if !diskNotice(notice.Kind) {
		return nil
	}
	copied, err := record.NewOutboxRow(issue, record.ControllerNotice(notice), r.now().UTC())
	if err != nil {
		return fmt.Errorf("the controller's copy of the %s notice of %s is a row the outbox refuses: %w", notice.Kind, issue, err)
	}
	args := []any{"row", row, "kind", notice.Kind, "issue", issue, "architect", architect, "because", why, "key", key}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		fresh, err := r.records.MarkProcessed(ctx, tx, keptPodControllerSource, key)
		if err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			if !fresh {
				r.log.Info("outbox disk notice handed to the controller before under this key", args...)
				return
			}
			r.log.Info("outbox disk notice sent to the controller: no architect will hear it", args...)
		})
		if !fresh {
			return nil
		}
		return r.records.Enqueue(ctx, tx, copied)
	}); err != nil {
		return fmt.Errorf("send the controller the %s notice of %s: %w", notice.Kind, issue, err)
	}
	return nil
}

// settleKept settles what an architect start row's task named of what its tree is owed, once the
// row has run and whatever its generation or phase made of it. Two things say the task reached
// the claim: gave, which the run that handed it to the claim knows, and the claim's own record of
// every start row whose task it queued (supervise.Claim.DeliveredRows), which outlives the
// delivery and answers for a later run of the same row. Either way this row has nothing left to
// settle: the claim's write of that delivery took the rows the row claimed and holds them, and
// the write that records the delivery's confirmation tells them — or, if the delivery was retired
// first, left them KEPT for the next start or catch-up of each one's own tree to name.
//
// A row neither says reached no claim — an earlier generation, a phase left, a lingering tree, a
// claim that never took the task — and hands what it claimed over (workflow.HandOverKept), so no
// release leaves a decision unclaimed while the tree it is owed to has an architect to tell.
//
// Every supervise row is settled after its work (the outbox's execute), and the one step that
// names decisions inside a start's own run — the ClaimReady a start applies when it finds the root
// architect already running — settles first, so the catch-up that fact writes never names a
// decision the start's task carries. A failure returns an error, so the row retries: a retried
// start queues its task no second time (the claim holds the row), so it settles the same way
// again, and a settlement that committed finds nothing left to do.
func (r *outbox) settleKept(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, gave bool) error {
	if payload.Kept == nil {
		return nil
	}
	root := payload.Tree
	token, err := claim.NewToken(r.project, root, claim.RoleArchitect)
	if err != nil {
		return fmt.Errorf("derive the architect of %s: %w", root, err)
	}
	held := r.supervisedClaim(token).Claim
	if gave || slices.Contains(held.DeliveredRows, row.ID) {
		return nil
	}
	r.log.Info("outbox start finished without giving its claim the task; the decisions it named are handed over", "row", row.ID,
		"root", root, "generation", payload.Generation, "launch", held.Generation)
	return r.handOverKept(ctx, root, row.ID)
}

// handOverKept hands the decisions outbox row claims to a fresh catch-up of the architect of each
// tree they are owed to now, and leaves KEPT what is owed to a tree with no architect to tell
// (workflow.HandOverKept). root names the row's own tree in the error the settlement reports.
func (r *outbox) handOverKept(ctx context.Context, root string, row int64) error {
	return r.writeSettlement(ctx, root, row, func(ctx context.Context, tx pgx.Tx) error {
		return workflow.HandOverKept(ctx, tx, r.records, r.gatedSteps, row, r.now().UTC())
	})
}

// writeSettlement runs one settlement of what outbox row named of what root keeps
// (settleUnderFacts): a hand-over reads the root and writes the catch-up a ready would write, so a
// queued catch-up's lane wakes as it commits. Its failure names the row and the root.
func (r *outbox) writeSettlement(ctx context.Context, root string, row int64, write func(context.Context, pgx.Tx) error) error {
	if err := r.settleUnderFacts(ctx, write); err != nil {
		return fmt.Errorf("settle the decisions outbox row %d named to %s: %w", row, root, err)
	}
	return nil
}

// settleUnderFacts runs write in a transaction of its own that serializes with every fact
// (intake.SerializeFacts) and owns the commit hooks its writes register (projectRecords,
// store.OnCommit), so the lane of each row its writes queue wakes as it commits. Every settlement
// the outbox writes under the fact lock runs here: a hand-over of what a root keeps
// (writeSettlement), and the settlement of the news a tree is owed (settleNews). The caller names
// what was settled in the error.
func (r *outbox) settleUnderFacts(ctx context.Context, write func(context.Context, pgx.Tx) error) error {
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the settlement with every fact: %w", err)
		}
		return write(ctx, tx)
	}); err != nil {
		return err
	}
	return nil
}

// toldKept records that a catch-up published under key told the architect the decisions it named,
// in a transaction of its own and before anything forgets them (settleTold). The forget can fail —
// a database error, or the daemon dying between the publish and its commit — and this mark is then
// the only thing that says the architect was told: without it the row's ending would release what
// it named and the next start or catch-up would tell it a second time. The mark goes under the
// telling's dedupe key, which a re-held copy shares with the row it copies
// (record.Notice.Published), since one key is one telling however many rows publish it. A start
// needs none: its claim records the row with the delivery (supervise.Claim.DeliveredRows).
func (r *outbox) toldKept(ctx context.Context, root, key string) error {
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.records.MarkProcessed(ctx, tx, record.KeptToldSource, key)
		return err
	}); err != nil {
		return fmt.Errorf("record that the catch-up of %s told the decisions it named: %w", root, err)
	}
	return nil
}

// settleTold tells what outbox row's catch-up named of what its tree is owed, once the row has run
// and told it (toldKept), however that run ended: the publish that told it, a newer catch-up that
// superseded the row since, a holder that refused it, a grant that denied it, an architect that
// will not hold its role again, or the row's own deletion under its lease. The telling is held to
// the one validity rule (record.Store.ForgetKeptDecisions): a row whose issue has left this tree
// since the catch-up was composed is not deleted but left KEPT, for the tree it stands in now to
// name. Only a row that told nothing is left to settle another way — the hand-over of a superseded
// catch-up (supersededKept), and otherwise the architect's next ready, which names every unclaimed
// decision. A failure returns an error, so the row retries and tells again; a telling of what is
// already gone deletes nothing.
func (r *outbox) settleTold(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if payload.CatchUp == nil || payload.CatchUp.Kept == nil {
		return nil
	}
	// A catch-up is enqueued on the issue whose architect it tells (workflow.QueueCatchUp): a
	// root's for its tree, a child's for its own subtree. Only a root's carries kept decisions, so
	// the row's issue here is that root; the key is the one its publish went out under, a re-held
	// copy's being the row it copies (record.Notice.Published).
	root := row.Issue
	_, key := payload.Published(row.ID)
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		told, err := r.records.Processed(ctx, tx, record.KeptToldSource, key)
		if err != nil || !told {
			return err
		}
		tree, err := r.treeOfIssue(ctx, tx, root)
		if err != nil {
			return err
		}
		return r.records.ForgetKeptDecisions(ctx, tx, tree, *payload.CatchUp.Kept, row.ID)
	}); err != nil {
		return fmt.Errorf("tell the decisions the catch-up of %s named: %w", root, err)
	}
	return nil
}

// treeOfIssue is the tree issue stands in now, which is the tree a row of that issue tells for: a
// root the record no longer holds, or one that is its own tree, answers itself.
func (r *outbox) treeOfIssue(ctx context.Context, tx pgx.Tx, issue string) (string, error) {
	stored, err := r.records.Issue(ctx, tx, issue)
	if err != nil || stored == nil {
		return issue, err
	}
	return stored.Tree, nil
}

// supersededKept hands what outbox row's catch-up named over to a fresh catch-up once a newer one
// has superseded it, so that it will never publish: the newer catch-up was composed from what
// nothing claimed and left this row's decisions out (workflow.HandOverKept). A row that published
// before — its telling failed, or the daemon died between the two — told what it named and hands
// nothing over: settleTold tells it when the row finishes here.
func (r *outbox) supersededKept(ctx context.Context, root string, row int64, key string) error {
	return r.writeSettlement(ctx, root, row, func(ctx context.Context, tx pgx.Tx) error {
		told, err := r.records.Processed(ctx, tx, record.KeptToldSource, key)
		if err != nil || told {
			return err
		}
		return workflow.HandOverKept(ctx, tx, r.records, r.gatedSteps, row, r.now().UTC())
	})
}

// spentKept hands over what a catch-up named once its re-holds are spent (noticeReholdDelays):
// no copy is queued to carry its claims on, and the architect it went to still holds its launch,
// so nothing else would name those decisions until that architect's next launch. claims is the
// row still running that claims them (the receipt timeout the notice executor answers), whose
// claims go to a fresh catch-up of the launch the architect stands at (workflow.HandOverKept); 0
// is an earlier listener's report after the row finished and its deletion released them, and then
// a fresh catch-up names what the tree is owed and nothing claims, when there is any. A telling
// one of the catch-up's rows already made hands nothing over: settleTold tells it.
func (r *outbox) spentKept(ctx context.Context, root string, claims int64, key string) error {
	return r.writeSettlement(ctx, root, claims, func(ctx context.Context, tx pgx.Tx) error {
		told, err := r.records.Processed(ctx, tx, record.KeptToldSource, key)
		if err != nil || told {
			return err
		}
		if claims != 0 {
			return workflow.HandOverKept(ctx, tx, r.records, r.gatedSteps, claims, r.now().UTC())
		}
		tree, err := r.treeOfIssue(ctx, tx, root)
		if err != nil {
			return err
		}
		decisions, approval, err := r.records.UnclaimedKept(ctx, tx, tree)
		if err != nil || len(decisions) == 0 && approval.Approved == 0 {
			return err
		}
		return workflow.CatchUpTheTreesOwed(ctx, tx, r.records, r.gatedSteps, []string{tree}, r.now().UTC())
	})
}
