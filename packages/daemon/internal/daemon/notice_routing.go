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

// errNoticeWaits is a notice held for its recipient: the recipient holds no role while its claim
// can hold it again, or an earlier notice held for the same recipient has not gone yet, and a later
// one must not overtake it. The row is tried again on the outbox's backoff, logged once, or,
// while the wake pacing holds its recipient's launch, at that launch's turn or parkedWakeBound
// on, whichever comes first (wakeForDelivery's turn, parkedWait).
var errNoticeWaits = wait.New("the notice waits for its recipient")

// errNoticeUnroutable is a notice whose recipient cannot be resolved from the record: its issue is
// not recorded, has no tree or no recorded root, or its keys make no claim token. readNoticeRoute
// decides it, once, when it reads the tree. Retrying cannot change the record and the outbox has
// no dead-letter path, so the row finishes undelivered with one log line.
var errNoticeUnroutable = errors.New("the notice has no recipient to go to")

// errNoticeRowGone is a notice row deleted while the runner held its lease: a catch-up a newer
// ready dropped (record.Store.DropCatchUps).
var errNoticeRowGone = errors.New("the notice row was deleted under its lease")

// noticeRoute is what routes a notice of a tree: the tree's project token, its root, and every
// issue of the tree whose key makes a claim token, by key. An issue whose key makes no claim token
// owns nothing, so it is left out: a walk that meets it ends at the root, and an earlier notice of
// it holds nothing back.
type noticeRoute struct {
	project string
	root    record.Issue
	issues  map[string]record.Issue
}

// treeSnapshot is one read of a notice's tree: its route, and the fence, the tree's unfinished
// notices written before the row, oldest first. The owner walk, the fence, and the linger check
// all see this one state, so a reparent committing meanwhile cannot show them half of itself.
type treeSnapshot struct {
	noticeRoute
	earlier []record.OutboxRow
}

// readNoticeTree reads row's tree in one repeatable-read transaction: that the row is still in the
// outbox under its lease, row's issue and the tree's route (readNoticeRoute), then the fence. A row
// deleted under its lease is errNoticeRowGone, and a notice it cannot route is errNoticeUnroutable.
func (r *outbox) readNoticeTree(ctx context.Context, row record.OutboxRow) (record.Issue, treeSnapshot, error) {
	var issue record.Issue
	var tree treeSnapshot
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		leased, err := r.records.OutboxLeased(ctx, tx, row.ID, row.LeaseToken)
		if err != nil {
			return err
		}
		if !leased {
			return errNoticeRowGone
		}
		if issue, tree.noticeRoute, err = r.readNoticeRoute(ctx, tx, row.Issue, map[string]noticeRoute{}); err != nil {
			return err
		}
		tree.earlier, err = r.records.EarlierNotices(ctx, tx, tree.root.Key, row.ID)
		return err
	}); err != nil {
		return record.Issue{}, treeSnapshot{}, fmt.Errorf("read the tree of notice row %d: %w", row.ID, err)
	}
	return issue, tree, nil
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

// earlierNoticeFor is the oldest of tree's earlier notices that goes to recipient as the claims
// run now — its id, the moment it is due, and whether it is a catch-up the wake pacing still
// holds in its queue — or 0, the zero moment and false when there is none. Each earlier notice's
// recipient is resolved again rather than recorded when it was held, since a sub-architect can
// start or end while it waits, and by the rule its own publish follows (noticeRecipient), so a
// notice held for the claim it names holds back no notice of the architect, nor the other way
// round. An earlier row whose recipient cannot be resolved is no claim's, so it holds back no
// later notice; it finishes on its own attempt (notice).
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
func earlierNoticeFor(tree treeSnapshot, recipient claim.Token, runs func(claim.Token) bool) (earlier int64, due time.Time, held bool) {
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
		if !ok {
			continue
		}
		if to, err := noticeRecipient(tree.project, tree.issues, issue, notice, runs); err == nil && to == recipient {
			if record.CatchUpIsHeld(other.NextAt) {
				return other.ID, time.Time{}, true
			}
			return other.ID, other.NextAt, false
		}
	}
	return 0, time.Time{}, false
}

// noticeRecipient is the claim notice, of issue in tree, goes to: the claim the notice names
// (record.Notice.To, a task's sub-owner's), which the workflow decided from the task the notice is
// about, else an architect. A catch-up names none and goes to the architect of its own issue and to
// no other: it states what that architect owns (workflow.QueueCatchUp, which queues it on that
// issue), so a child's catch-up that walked up to the architect above it while the child's own
// architect was stopped would be read there as that architect's own state. It waits for its
// architect, and finishes undelivered once that claim will not hold its role again or, for a
// child's catch-up, once the child is out of the workflow by its status or its phase
// (deliverNotice's record.InWorkflow), since a park leaves that claim suspended rather than ended.
// A root's catch-up gets no such check: besides the endings every notice has, catchUpSuperseded,
// stoppedWithTree and recipientEnded end it. Every other notice goes to the architect that owns its
// issue (record.OwningArchitect), by runs, from the issue's parent for a notice the architect above
// it must hear (record.NoticeStartsAtParent). The publish (deliverNotice), the fence
// (earlierNoticeFor) and a ready's release (releaseWaiting) all ask this, so each finds a notice the
// same recipient, and each answer is a claim a notice goes to (record.NoticeGoesTo). project is the
// tree's claim project token.
func noticeRecipient(project string, tree map[string]record.Issue, issue record.Issue, notice record.Notice, runs func(claim.Token) bool) (claim.Token, error) {
	switch {
	case notice.To != "":
		return notice.To, nil
	case notice.CatchUp != nil:
		return claim.NewToken(project, issue.Key, claim.RoleArchitect)
	}
	return record.OwningArchitect(project, tree, issue, record.NoticeStartsAtParent(notice.Kind, notice.Role), runs)
}

// holdsRole is whether held's claim keeps its role through the decision in flight: the record's
// rule holds (record.HoldsRole: it has not ended, and it is not suspended unless the idle release
// suspended it), and that decision is not stopping its process for good (supervise.ClaimView's
// Stopping) — a suspension, a failure, a release. The owner walk reads it, so no notice but a
// catch-up is routed to a sub-architect whose session is being stopped: such a notice goes on up
// the tree while the stop runs. It goes on up after the stop too, unless the stop is the idle
// release's, whose released claim holds its role again (record.HoldsRole). A catch-up waits for
// the architect of its own issue (noticeRecipient).
func holdsRole(held supervise.ClaimView) bool {
	return record.HoldsRole(held.State, held.SuspendedBy) && !held.Stopping
}

// claimEnded is whether a claim in state will not hold its role again: this daemon does not
// supervise it (""), or it failed or retired. A child's own architect that failed does start again,
// at its child's next phase start (workflow's reviveArchitect), but that launch's ready queues a
// catch-up of its own, so nothing held for the failed claim waits for it. A suspended claim has
// not ended: the operator's resume or deliver starts it again (`legion claims resume`), and so
// does the workflow's next start of its role for a claim the workflow suspended, which for a
// child's own architect is the child's next first phase start (workflow's StartFirstPhase), or,
// after its tree's linger, the re-admission's start of a child past admitted (admit's
// startMidPhaseMembers).
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

// recipientEnded says why nobody will hold the role of a notice's recipient, in state, for a notice
// of tree, or "" when its claim can hold the role again: a lingering tree's claims are suspended,
// and a closed tree's retired, until re-admission, which starts each with the tree's record rather
// than the notices of its close.
func recipientEnded(tree treeSnapshot, state supervise.ClaimState) string {
	if tree.root.Lingers() {
		return "its tree lingers or has closed"
	}
	if claimEnded(state) {
		return "its claim has failed, retired, or is not supervised"
	}
	return ""
}

// stoppedWithTree says whether the recipient of a notice, its claim held, has stopped with its
// finished tree: the tree lingers or has closed (lingers), and the claim holds its role no longer
// (holdsRole), as the close suspended it, or is suspending it now, or it failed or retired.
// Nothing more is sent to such a recipient, not even through the Envoy registration its stopped
// session left behind: re-admission starts it with the tree's record rather than the notices of its
// close.
func stoppedWithTree(lingers bool, held supervise.ClaimView) bool {
	return lingers && !holdsRole(held)
}
