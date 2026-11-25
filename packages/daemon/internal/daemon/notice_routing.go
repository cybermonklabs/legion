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
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// noticeSummary is the one-line message a notice or a role message of kind is published with,
// "<kind> on <issue>": the summary the listener shows and echoes back in a role-lane exception,
// which parseNoticeSummary reads. record.NoticeKind is texts.Kind, so a role message's kind is one
// too.
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

// earlierNoticeFor is the oldest of tree's earlier notices that goes to architect as the claims
// run now and holds a later notice back — its id and, where the wake pacing holds it, the moment
// it is due — or 0 and the zero moment when there is none. Each earlier notice's architect is
// resolved again rather than recorded when it was held, since a sub-architect can start or end
// while it waits, and by the rule its own publish follows (noticeArchitect). An earlier row whose
// architect cannot be resolved is no architect's, so it holds back no later notice; it finishes
// on its own attempt (notice).
//
// A notice that takes the low rule (lowRule) holds no later notice back while the architect has
// not said it is ready (supervise.SaidReady, which ready gives): a low notice to a released
// architect waits for that ready and wakes nothing (deliverNotice), so a med notice behind it goes
// past it and wakes the pod. Once the architect is ready, its ready makes both due
// (releaseWaiting), and the fence holds the later notice behind the low one again, so they arrive
// in the order they were written. A low notice a project's pause held wakes the architect as a med
// one does, so it holds a later notice back as a med one does.
//
// The moment is what the row behind it waits for where the wake pacing holds the earlier row
// (record.Notice.PacedAt, still ahead of now, which is read only then): such a row is due minutes
// out, and the row behind it runs once, then (deliverNotice's wait.Until), rather than climbing
// its backoff to it. Behind any other earlier row the moment is zero, and the row behind waits on
// the outbox's own short backoff (errNoticeWaits): that earlier row's next attempt can be an hour
// out (a low notice's wait), or sooner than its next_at says (a ready releases it). A row waiting
// out its architect's parked launch (parkedWait) is one of these: a med row behind it meets the
// same park before the fence (deliverNotice's wakeForDelivery), and so waits for that launch's
// turn or parkedWakeBound on, whichever comes first, as the earlier row does.
func earlierNoticeFor(tree treeSnapshot, architect claim.Token, runs func(claim.Token) bool, ready bool, now func() time.Time) (int64, time.Time) {
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
		if owner, err := noticeArchitect(tree.project, tree.issues, issue, notice, runs); err != nil || owner != architect {
			continue
		}
		if !ready && lowRule(other, notice) {
			continue
		}
		if !notice.PacedAt.IsZero() && notice.PacedAt.After(now()) {
			return other.ID, other.NextAt
		}
		return other.ID, time.Time{}
	}
	return 0, time.Time{}
}

// noticeUrgency is the urgency notice goes to reader with: the rule the list gives its kind
// (texts.UrgencyOf), read with the variant the notice's own payload carries (texts.Variant, whose
// fields are the notice's keys of the same names). A copy of a notice (reholdNotice) carries the
// payload of the row it copies, so it goes with that row's urgency. A row an earlier release
// queued carries only the keys that release wrote: a checks-red row of main names no head, and so
// goes med (texts' checksRedUrgency), as a phase-finished row of main names no to.
func noticeUrgency(notice record.Notice, reader claim.Role) texts.Urgency {
	return texts.UrgencyOf(notice.Kind, reader, texts.Variant{Role: notice.Role, Phase: notice.Phase, To: notice.To,
		Change: notice.Change, Hold: notice.Hold, Head: notice.Head})
}

// lowRule says whether the notice row takes the low rule (dispatch://INFRA-3686/spec R5): it is
// low for the architect (noticeUrgency), and no project's pause held it
// (record.OutboxRow.HeldByPause). Such a row calls no wakeForDelivery and holds no later notice
// back (deliverNotice, earlierNoticeFor). A row a pause held is the exception, as the spec's
// Design keeps R4 of dispatch://INFRA-3759: the resume launches each reader it held a text for,
// whatever the text's urgency, so a low row a pause held wakes its released architect with that
// pause's wake and holds a later notice back, as a med row does. It is still published at low.
func lowRule(row record.OutboxRow, notice record.Notice) bool {
	return row.HeldByPause == 0 && noticeUrgency(notice, claim.RoleArchitect) == texts.Low
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
// rule holds (record.HoldsRole: it has not ended, and it is not suspended unless the idle release
// suspended it), and that decision is not stopping its process for good (supervise.ClaimView's
// Stopping) — a suspension, a failure, a release. The owner walk reads it, so no notice but a
// catch-up is routed to a sub-architect whose session is being stopped: such a notice goes on up
// the tree while the stop runs. It goes on up after the stop too, unless the stop is the idle
// release's, whose released claim holds its role again (record.HoldsRole). A catch-up waits for
// the architect of its own issue (noticeArchitect).
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
