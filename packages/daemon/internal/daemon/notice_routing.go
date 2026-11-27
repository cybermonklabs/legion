package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// noticeSummary is the one-line message a notice is published with, "<kind> on <issue>": the
// summary the listener shows and echoes back in a role-lane exception, which parseNoticeSummary
// reads.
func noticeSummary(kind record.NoticeKind, issue string) string {
	return string(kind) + noticeSummarySeparator + issue
}

// parseNoticeSummary is the kind and issue noticeSummary wrote into summary, and whether it did.
func parseNoticeSummary(summary string) (record.NoticeKind, string, bool) {
	kind, issue, found := strings.Cut(summary, noticeSummarySeparator)
	return record.NoticeKind(kind), issue, found && kind != "" && issue != ""
}

const noticeSummarySeparator = " on "

// errNoticeWaits is a notice held for its architect: the architect holds no role while its claim
// can hold it again, or an earlier notice held for the same architect has not gone yet, and a later
// one must not overtake it. The row is tried again on the outbox's backoff, logged once, or,
// while the wake pacing holds its architect's launch, at that launch's turn or parkedWakeBound
// on, whichever comes first (wakeForDelivery's turn, parkedWait).
var errNoticeWaits = wait.New("the notice waits for its architect")

// errNoticeUnroutable is a notice whose architect cannot be resolved from the record: its issue is
// not recorded, has no tree or no recorded root, or its keys make no claim token. readNoticeRoute
// decides it, once, when it reads the tree. Retrying cannot change the record and the outbox has
// no dead-letter path, so the row finishes undelivered with one log line.
var errNoticeUnroutable = errors.New("the notice has no architect to go to")

// errNoticeRowGone is a notice row, or a controller notice row, deleted while the runner held its
// lease: a catch-up a newer ready dropped (record.Store.DropCatchUps), or a changes request, or
// its relay to the controller, the person's approval followed that a clear of its generation
// dropped (record.Store.DropQueuedChanges).
var errNoticeRowGone = errors.New("the notice row was deleted under its lease")

// changesSuperseded is a changes request the person's approval followed: the gates stillOwed read
// record an approval of the document reviewed, at the version the request reviewed or a later
// one, given after the review (workflow.NoticeSuperseded). approved is the version approved,
// which the line that finishes the row names. Only an approval replaces a changes request, so
// the predicate's answer for one always names it (workflow.Supersession.By).
type changesSuperseded struct{ approved int }

func (s changesSuperseded) Error() string {
	return fmt.Sprintf("the person approved version %d of the document after the changes request", s.approved)
}

// stillOwed reads in tx, the caller's repeatable-read snapshot, whether row's publish is still
// owed: the row is still in the outbox under its lease (errNoticeRowGone when it is not), and,
// when its payload, a notice or a controller notice, holds a person's changes request
// (record.ChangesRequest), that request is still the person's word (changesSuperseded when it is
// not), asked of the gates of row's issue as tx holds them (workflow.NoticeSuperseded). The two
// are read in one snapshot because a clear of the row's generation asks each changes request
// queued on the issues it clears while their gates stand, deletes each row the person's approval
// followed, and then deletes the gates, those of a request it found standing included, whose row
// it keeps (workflow.Engine.ClearGeneration). A snapshot taken before the clear finds the row and
// the approval both; one taken after it finds a row the approval followed gone, and asks a row
// still there of the gates as they stand then. Read apart, the row could be read before the clear
// and the gates after it, and a request the approval followed would read as the person's word. It
// is the one read the delivery of a notice (readNoticeTree) and the publish of a relay to the
// controller (controllerNotice) make before they publish.
func (r *outbox) stillOwed(ctx context.Context, tx pgx.Tx, row record.OutboxRow, payload record.OutboxPayload) error {
	leased, err := r.records.OutboxLeased(ctx, tx, row.ID, row.LeaseToken)
	if err != nil {
		return err
	}
	if !leased {
		return errNoticeRowGone
	}
	request, asks := record.ChangesRequest(payload)
	if !asks {
		return nil
	}
	gates, err := r.records.GatesOf(ctx, tx, row.Issue)
	if err != nil {
		return err
	}
	if superseded := workflow.NoticeSuperseded(gates, r.gatedSteps, row.Issue, request, record.DecisionDesignChangesRequested); superseded != nil {
		return changesSuperseded{approved: superseded.By.Version}
	}
	return nil
}

// noticeRoute is what routes a notice of a tree: the tree's project token, its root, and every
// issue of the tree whose key makes a claim token, by key. An issue whose key makes no claim token
// owns nothing, so it is left out: a walk that meets it ends at the root, and an earlier notice of
// it holds nothing back.
type noticeRoute struct {
	project string
	root    record.Issue
	issues  map[string]record.Issue
}

// treeSnapshot is one read of a notice's tree: its route, the fence, the tree's unfinished notices
// written before the row, oldest first, and whether the tree holds its work (holdsWork: the
// deployment's hold is `work` and the tree's standing design gate stands closed,
// record.TreeGate.Standing). The owner walk, the fence, the linger check and the work hold's wait
// of a notice of the owner's acceptance all see this one state, so a reparent or a move of the
// issue committing meanwhile cannot show them half of itself.
type treeSnapshot struct {
	noticeRoute
	earlier   []record.OutboxRow
	holdsWork bool
}

// heldForTheOwner is whether the work hold holds notice, of issue: the tree holds its work, and the
// notice tells issue's architect that issue waits in its own acceptance
// (workflow.TellsOwnersAcceptance). No architect is told to start its own acceptance, the owner's
// place, while the hold stands: the notice waits, and every later notice of the architect passes
// it (earlierNoticeFor).
func (tree treeSnapshot) heldForTheOwner(issue record.Issue, notice record.Notice) bool {
	return tree.holdsWork && workflow.TellsOwnersAcceptance(issue, notice)
}

// recomposedCatchUp is what the delivery's own read states of a catch-up in place of what the row
// was written with: the record half (workflow.TreeCatchUp), the notice's reason, and Decided.
type recomposedCatchUp struct {
	tree    record.CatchUpTree
	reason  string
	decided string
}

// readNoticeTree reads row's tree in one repeatable-read transaction: whether the row's publish is
// still owed (stillOwed: the row is still in the outbox under its lease, and a changes request is
// still the person's word), row's issue and the tree's route (readNoticeRoute), the fence, and,
// under `work`, the tree's standing design gate, the live gate every fence of the hold reads
// (treeGate), read here in the same snapshot as the issue whose phase the hold's wait reads. A
// row deleted under its lease is errNoticeRowGone, a changes request the person's approval
// followed is changesSuperseded, and a notice it cannot route is errNoticeUnroutable.
// For a catch-up (payload.CatchUp) the same transaction composes again what the architect of
// row's issue owns (workflow.TreeCatchUp, a pure read, for the issue the row is queued on, as
// workflow.QueueCatchUp composed it: a root's whole tree, a child's own issue and every issue
// under it, so a child's architect is never told the whole tree), and what the architect is told
// is the record at this read, the one the owner walk and the fence see, rather than at the moment
// the row was queued: a gate registered between the two reached the architect at its old version
// (dispatch://INFRA-2819). A child's catch-up claims no kept decision, so it carries no Kept and
// Decided stays empty. The reason it composes keeps the sentence of what the architect's last
// process ended that the row was queued with (record.CatchUp.Relaunch,
// workflow.CatchUpReason): that sentence is of what ended before the launch, which this read does
// not restate. It writes Decided again from the decisions the row claimed (Kept), each asked of the
// gates this read holds (workflow.ReadKeptGates, workflow.KeptDecisionsTask), so a kept decision
// that stopped being the person's word while the row waited (workflow.SupersedingApproval) drops
// from the text. It deletes nothing: the row's queueing claimed those decisions, and its settling
// stays keyed to the whole of Kept, so the publish forgets a dropped decision with the rest. A
// catch-up an earlier release queued with Decided and no Kept is told as its row holds it. It is
// nil for every other notice. The transaction stays read-only.
func (r *outbox) readNoticeTree(ctx context.Context, row record.OutboxRow, payload record.Notice) (record.Issue, treeSnapshot, *recomposedCatchUp, error) {
	var issue record.Issue
	var tree treeSnapshot
	var recomposed *recomposedCatchUp
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.stillOwed(ctx, tx, row, payload); err != nil {
			return err
		}
		var err error
		if issue, tree.noticeRoute, err = r.readNoticeRoute(ctx, tx, row.Issue, map[string]noticeRoute{}); err != nil {
			return err
		}
		if tree.earlier, err = r.records.EarlierNotices(ctx, tx, tree.root.Key, row.ID); err != nil {
			return err
		}
		if r.holdIsWork() {
			gate, err := r.treeGateTx(ctx, tx, tree.root.Key)
			if err != nil {
				return err
			}
			_, tree.holdsWork = gate.Standing()
		}
		if payload.CatchUp == nil {
			return nil
		}
		state, reason, err := workflow.TreeCatchUp(ctx, tx, r.records, r.gatedSteps, issue)
		if err != nil {
			return err
		}
		recomposed = &recomposedCatchUp{tree: state, reason: workflow.CatchUpReason(payload.CatchUp.Relaunch, reason),
			decided: payload.CatchUp.Decided}
		if kept := payload.CatchUp.Kept; kept != nil {
			gates, err := workflow.ReadKeptGates(ctx, tx, r.records, tree.root.Key, *kept)
			if err != nil {
				return err
			}
			recomposed.decided, _ = workflow.KeptDecisionsTask(gates, r.gatedSteps, tree.root.Key, *kept)
		}
		return nil
	}); err != nil {
		return record.Issue{}, treeSnapshot{}, nil, fmt.Errorf("read the tree of notice row %d: %w", row.ID, err)
	}
	return issue, tree, recomposed, nil
}

// readNoticeRoute reads in tx the tree of the issue key names, and returns the issue as the tree's
// own issue set holds it with the tree's route. routes holds the routes already read by tree root
// and keeps the ones read here, so a caller routing several notices in one transaction reads each
// tree once. A notice is unroutable when its issue is not recorded, has no
// tree or no recorded root, or its keys make no claim token.
func (r *outbox) readNoticeRoute(ctx context.Context, tx pgx.Tx, key string, routes map[string]noticeRoute) (record.Issue, noticeRoute, error) {
	if !claim.IsIssueKey(key) {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: %q is not an issue key", errNoticeUnroutable, key)
	}
	recorded, err := r.records.Issue(ctx, tx, key)
	if err != nil {
		return record.Issue{}, noticeRoute{}, err
	}
	if recorded == nil {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: workflow issue %s is not recorded", errNoticeUnroutable, key)
	}
	if recorded.Tree == "" {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: issue %s has no tree root", errNoticeUnroutable, key)
	}
	if route, ok := routes[recorded.Tree]; ok {
		return route.issues[key], route, nil
	}
	var route noticeRoute
	if route.project, err = claim.ProjectToken(recorded.Project); err != nil {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: %w", errNoticeUnroutable, err)
	}
	members, err := r.records.TreeIssues(ctx, tx, recorded.Tree)
	if err != nil {
		return record.Issue{}, noticeRoute{}, err
	}
	route.issues = record.TreeByKey(members)
	var ok bool
	if route.root, ok = route.issues[recorded.Tree]; !ok {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: the root %s of %s is not recorded", errNoticeUnroutable, recorded.Tree, key)
	}
	routes[recorded.Tree] = route
	return route.issues[key], route, nil
}

// earlierNoticeFor is the oldest of tree's earlier notices that goes to architect as the claims
// run now — its id, the moment it is due, and whether it is a catch-up the wake pacing still
// holds in its queue — or 0, the zero moment and false when there is none. Each earlier notice's
// architect is resolved again rather than recorded when it was held, since a sub-architect can
// start or end while it waits, and by the rule its own publish follows (noticeArchitect). An
// earlier row whose architect cannot be resolved is no architect's, so it holds back no later
// notice; it finishes on its own attempt (notice). Nor does an earlier row the work hold holds
// (heldForTheOwner): it waits for the gate's opening, which can be days, and every other notice
// of its architect goes meanwhile.
//
// The moment is what the row behind it waits for: an earlier row the wake pacing holds is due
// minutes out — at the moment the pacing reserved for a catch-up (record.Notice.PacedAt), or, for
// a row waiting out its architect's parked launch, at that launch's turn or parkedWakeBound on,
// whichever comes first (parkedWait) — and the row behind it runs once, then (deliverNotice's
// wait.Until), rather than climbing its backoff to it. So a park that ends before its turn holds
// the row behind it for at most the bound as well. A catch-up the pacing still holds in its
// queue is due at no moment until the pacer lets its place out (record.CatchUpIsHeld): held is
// true for it and the moment is the zero time, and the row behind it waits for the pacer's
// estimate of that place instead (deliverNotice).
func earlierNoticeFor(tree treeSnapshot, architect claim.Token, runs func(claim.Token) bool) (earlier int64, due time.Time, held bool) {
	for _, other := range tree.earlier {
		payload, err := record.DecodeOutboxPayload(other)
		if err != nil {
			continue
		}
		notice, ok := payload.(record.Notice)
		if !ok {
			continue
		}
		issue, ok := tree.issues[other.Issue]
		if !ok || tree.heldForTheOwner(issue, notice) {
			continue
		}
		if owner, err := noticeArchitect(tree.project, tree.issues, issue, notice, runs); err == nil && owner == architect {
			if record.CatchUpIsHeld(other.NextAt) {
				return other.ID, time.Time{}, true
			}
			return other.ID, other.NextAt, false
		}
	}
	return 0, time.Time{}, false
}

// noticeArchitect is the architect claim notice, of issue in tree, goes to. A catch-up goes to the
// architect of its own issue and to no other: it states what that architect owns
// (workflow.QueueCatchUp, which queues it on that issue), so a child's catch-up that walked up to
// the architect above it while the child's own architect was stopped would be read there as that
// architect's own state. It waits for its architect, and finishes undelivered once that claim
// will not hold its role again or, for a child's catch-up, once the child is out of the workflow
// by its status or its phase (deliverNotice's record.InWorkflow), since a park leaves that claim
// suspended rather than ended. A root's catch-up gets no such check: besides the endings every
// notice has, catchUpSuperseded, stoppedWithTree and architectEnded end it. Every other notice
// goes to the architect that owns its issue (record.OwningArchitect), by runs, from the issue's
// parent for a notice the architect above it must hear (record.NoticeStartsAtParent). The
// publish (deliverNotice), the fence (earlierNoticeFor) and a ready's release (releaseWaiting)
// all ask this, so each finds a notice the same architect. project is the tree's claim project
// token.
func noticeArchitect(project string, tree map[string]record.Issue, issue record.Issue, notice record.Notice, runs func(claim.Token) bool) (claim.Token, error) {
	if notice.CatchUp != nil {
		return claim.NewToken(project, issue.Key, claim.RoleArchitect)
	}
	return record.OwningArchitect(project, tree, issue, record.NoticeStartsAtParent(notice.Kind, notice.Role), runs)
}

// holdsRole is whether held's claim keeps its role through the decision in flight: the record's
// rule holds (supervise.HoldsRole: it has not ended, and it is not suspended unless the idle
// release suspended it), and that decision is not stopping its process for good
// (supervise.ClaimView's Stopping) — a suspension, a failure, a release. The owner walk reads it,
// so no notice but a catch-up is routed to a sub-architect whose session is being stopped: such a
// notice goes on up the tree while the stop runs. It goes on up after the stop too, unless the
// stop is the idle release's, whose released claim holds its role again (supervise.HoldsRole). A
// catch-up waits for the architect of its own issue (noticeArchitect).
func holdsRole(held supervise.ClaimView) bool {
	return supervise.HoldsRole(held.State, held.SuspendedBy) && !held.Stopping
}

// claimEnded is whether a claim in state will not hold its role again: this daemon does not
// supervise it (""), or it failed or retired. A child's own architect that failed does start again,
// at its child's next phase start (workflow's reviveArchitect), but that launch's ready queues a
// catch-up of its own, so nothing held for the failed claim waits for it. A suspended claim has
// not ended: a resume starts its agent again — the operator's resume or deliver whoever suspended
// it (`legion claims resume`), and the controller's any suspension but the operator's
// (supervise.Claim.HeldAgainst) — and so does the workflow's
// next start of its role for a claim the workflow suspended, which for a child's own architect is
// the child's next first phase start (workflow's StartFirstPhase), or, after its tree's linger,
// the re-admission's start of a child past admitted (admit's startMidPhaseMembers).
func claimEnded(state supervise.ClaimState) bool {
	return state == "" || state == supervise.StateFailed || state == supervise.StateRetired
}

// supervisedClaim is token's claim as this daemon supervises it, or the zero view, whose state is
// "", when it does not supervise it. It is the machine's View, which never waits on a decision in
// flight: the outbox runs one row at a time, and an architect's relaunch can hold its machine for
// minutes while the runtime waits out its pods.
func (r *outbox) supervisedClaim(token claim.Token) supervise.ClaimView {
	machine, ok := r.supervisor.Machine(token)
	if !ok {
		return supervise.ClaimView{}
	}
	return machine.View()
}

// claimState is the state of token's claim, or "" when this daemon does not supervise it.
func (r *outbox) claimState(token claim.Token) supervise.ClaimState {
	return r.supervisedClaim(token).State
}

// holdsItsRole is holdsRole for token's claim: the outbox's runs for the owner walk
// (record.OwningArchitect).
func (r *outbox) holdsItsRole(token claim.Token) bool {
	return holdsRole(r.supervisedClaim(token))
}

// architectEnded says why nobody will hold the role of the owning architect, in state, for a
// notice of tree, or "" when its claim can hold the role again: a lingering tree's architects are
// suspended, and a closed tree's retired, until re-admission, which starts each with the tree's
// record rather than the notices of its close.
func architectEnded(tree treeSnapshot, state supervise.ClaimState) string {
	if tree.root.Lingers() {
		return "its tree lingers or has closed"
	}
	if claimEnded(state) {
		return "its claim has failed, retired, or is not supervised"
	}
	return ""
}

// stoppedWithTree says whether the owning architect of a notice, its claim held, has stopped with
// its finished tree: the tree lingers or has closed (lingers), and the claim holds its role no
// longer (holdsRole), as the close suspended it, or is suspending it now, or it failed or retired.
// Nothing more is sent to such an architect, not even through the Envoy registration its stopped
// session left behind: re-admission starts it with the tree's record rather than the notices of its
// close.
func stoppedWithTree(lingers bool, held supervise.ClaimView) bool {
	return lingers && !holdsRole(held)
}
