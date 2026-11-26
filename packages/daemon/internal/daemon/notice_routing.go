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

// errNoticeWaits is a notice held for its architect: the architect holds no role while its claim
// can hold it again, an earlier notice held for the same architect has not gone yet, and a later
// one must not overtake it, or the owner walk met a Dispatch chain the copy cannot show to its top
// (record.ErrChainUnknown), whose architect is known once the daemon has read Dispatch again. The
// row is tried again on the outbox's backoff, logged once, or, while the wake pacing holds its
// architect's launch, at that launch's turn or parkedWakeBound on, whichever comes first
// (wakeForDelivery's turn, parkedWait).
var errNoticeWaits = wait.New("the notice waits for its architect")

// errNoticeUnroutable is a notice whose architect cannot be resolved from the record: its issue is
// not recorded, or its keys make no claim token. readNoticeTree decides it, once, when it reads
// the route. Retrying cannot change the record and the outbox has no dead-letter path, so the row
// finishes undelivered with one log line.
var errNoticeUnroutable = errors.New("the notice has no architect to go to")

// errNoticeRowGone is a notice row deleted while the runner held its lease: a catch-up a newer
// ready dropped (record.Store.DropCatchUps).
var errNoticeRowGone = errors.New("the notice row was deleted under its lease")

// noticeRoute is what routes the notices of one read: the project's claim token, and the Dispatch
// chains of their issues as one read of the copy holds them (record.Chains), over which the owner
// walk runs (R18 of dispatch://INFRA-3320). Nothing of the stored tree is read, so a notice of an
// issue whose parent moved goes to the architects of the chain it stands on now.
type noticeRoute struct {
	project string
	chains  *record.Chains
}

// noticeSnapshot is one read of a notice's route: the route, the gate root of the notice's issue
// (record.Chains.StateTree), for which a person's spec decision nobody will hear is kept
// (parkDecision), and the fence, the project's unfinished notices written before the row, oldest
// first. The owner walk and the fence see this one state, so a change of parent committing
// meanwhile cannot show them half of itself.
type noticeSnapshot struct {
	noticeRoute
	root    string
	earlier []record.OutboxRow
}

// readNoticeTree reads row's route in one repeatable-read transaction: that the row is still in
// the outbox under its lease, the record of row's issue, the fence, and the chains of row's issue
// and of every issue the fence names, in one read (readNoticeRoute). A row deleted under its lease
// is errNoticeRowGone, and a notice it cannot route is errNoticeUnroutable.
func (r *outbox) readNoticeTree(ctx context.Context, row record.OutboxRow) (record.Issue, noticeSnapshot, error) {
	var issue record.Issue
	var snapshot noticeSnapshot
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		leased, err := r.records.OutboxLeased(ctx, tx, row.ID, row.LeaseToken)
		if err != nil {
			return err
		}
		if !leased {
			return errNoticeRowGone
		}
		if !claim.IsIssueKey(row.Issue) {
			return fmt.Errorf("%w: %q is not an issue key", errNoticeUnroutable, row.Issue)
		}
		recorded, err := r.records.Issue(ctx, tx, row.Issue)
		if err != nil {
			return err
		}
		if recorded == nil {
			return fmt.Errorf("%w: workflow issue %s is not recorded", errNoticeUnroutable, row.Issue)
		}
		if snapshot.earlier, err = r.records.EarlierNotices(ctx, tx, recorded.Project, row.ID); err != nil {
			return err
		}
		keys := make([]string, 0, len(snapshot.earlier)+1)
		keys = append(keys, row.Issue)
		for _, other := range snapshot.earlier {
			keys = append(keys, other.Issue)
		}
		if snapshot.noticeRoute, err = r.readNoticeRoute(ctx, tx, recorded.Project, keys); err != nil {
			return err
		}
		if issue, err = snapshot.issue(row.Issue); err != nil {
			return err
		}
		snapshot.root = snapshot.chains.StateTree(row.Issue)
		return nil
	}); err != nil {
		return record.Issue{}, noticeSnapshot{}, fmt.Errorf("read the route of notice row %d: %w", row.ID, err)
	}
	return issue, snapshot, nil
}

// readNoticeRoute reads in tx the route of the notices of the issues keys names, all of
// dispatchProject: the project's claim token and the Dispatch chains of every key, in one read
// (record.ReadChainsOf, which walks the keys' own chains), so a caller routing many notices reads
// the copy a fixed number of times however many there are. A project that makes no claim token
// routes nothing (errNoticeUnroutable).
func (r *outbox) readNoticeRoute(ctx context.Context, tx pgx.Tx, dispatchProject string, keys []string) (noticeRoute, error) {
	project, err := claim.ProjectToken(dispatchProject)
	if err != nil {
		return noticeRoute{}, fmt.Errorf("%w: %w", errNoticeUnroutable, err)
	}
	chains, err := record.ReadChainsOf(ctx, tx, r.records, dispatchProject, keys)
	if err != nil {
		return noticeRoute{}, err
	}
	return noticeRoute{project: project, chains: chains}, nil
}

// issue is key's record as the route's read holds it: errNoticeUnroutable where key is no issue
// key or the daemon records no such issue, which has no architect to hear its notices.
func (n noticeRoute) issue(key string) (record.Issue, error) {
	if !claim.IsIssueKey(key) {
		return record.Issue{}, fmt.Errorf("%w: %q is not an issue key", errNoticeUnroutable, key)
	}
	issue, recorded := n.chains.Record(key)
	if !recorded {
		return record.Issue{}, fmt.Errorf("%w: workflow issue %s is not recorded", errNoticeUnroutable, key)
	}
	return issue, nil
}

// earlierNoticeFor is the oldest of the snapshot's earlier notices that goes to architect as the
// claims run now — its id, the moment it is due, and whether it is a catch-up the wake pacing
// still holds in its queue — or 0, the zero moment and false when there is none. Each earlier
// notice's architect is resolved again rather than recorded when it was held, since a
// sub-architect can start or end, and a parent change, while it waits, and by the rule its own
// publish follows (noticeArchitect). An earlier row whose architect cannot be resolved, or whose
// walk meets an unknown chain, is no architect's yet, so it holds back no later notice; it
// finishes, or waits, on its own attempt (notice).
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
func earlierNoticeFor(snapshot noticeSnapshot, architect claim.Token, runs func(claim.Token) bool) (earlier int64, due time.Time, held bool) {
	for _, other := range snapshot.earlier {
		payload, err := record.DecodeOutboxPayload(other)
		if err != nil {
			continue
		}
		notice, ok := payload.(record.Notice)
		if !ok {
			continue
		}
		issue, err := snapshot.issue(other.Issue)
		if err != nil {
			continue
		}
		if owner, err := noticeArchitect(snapshot.noticeRoute, issue, notice, runs); err == nil && owner == architect {
			if record.CatchUpIsHeld(other.NextAt) {
				return other.ID, time.Time{}, true
			}
			return other.ID, other.NextAt, false
		}
	}
	return 0, time.Time{}, false
}

// noticeArchitect is the architect claim notice, of issue, goes to. A catch-up goes to the
// architect of its own issue and to no other: it states what that architect acts on
// (workflow.QueueCatchUp, which queues it on that issue), so a child's catch-up that walked up to
// the architect above it while the child's own architect was stopped would be read there as that
// architect's own state. A notice of a change of parent names the architect it tells
// (record.Notice.To, R18 of dispatch://INFRA-3320's three architects), and goes to that one alone.
// Each of those waits for its architect, and finishes undelivered once that claim will not hold
// its role again or, for a catch-up, once its issue is out of the workflow by its status or its
// phase (deliverNotice's record.InWorkflow), since a stop leaves that claim suspended rather than
// ended. Every other notice goes to the architect that owns its issue on the Dispatch chain
// (record.OwningArchitect), by runs, from the issue above it for a notice the architect above it
// must hear (record.NoticeStartsAtParent), and waits (errNoticeWaits) while that walk meets a
// chain the copy cannot show to its top. The publish (deliverNotice), the fence (earlierNoticeFor)
// and a ready's release (releaseWaiting) all ask this, so each finds a notice the same architect.
func noticeArchitect(route noticeRoute, issue record.Issue, notice record.Notice, runs func(claim.Token) bool) (claim.Token, error) {
	switch {
	case notice.CatchUp != nil:
		return claim.NewToken(route.project, issue.Key, claim.RoleArchitect)
	case notice.To != "":
		return claim.NewToken(route.project, notice.To, claim.RoleArchitect)
	}
	architect, err := record.OwningArchitect(route.project, route.chains, issue.Key, record.NoticeStartsAtParent(notice.Kind, notice.Role), runs)
	if errors.Is(err, record.ErrChainUnknown) {
		return "", fmt.Errorf("%w: %w", errNoticeWaits, err)
	}
	return architect, err
}

// holdsRole is whether held's claim keeps its role through the decision in flight: the record's
// rule holds (record.HoldsRole: it has not ended, and it is not suspended unless the idle release
// suspended it), and that decision is not stopping its process for good (supervise.ClaimView's
// Stopping) — a suspension, a failure, a release. The owner walk reads it, so no notice but a
// catch-up is routed to a sub-architect whose session is being stopped: such a notice goes on up
// the chain while the stop runs. It goes on up after the stop too, unless the stop is the idle
// release's, whose released claim holds its role again (record.HoldsRole). A catch-up waits for
// the architect of its own issue (noticeArchitect).
func holdsRole(held supervise.ClaimView) bool {
	return record.HoldsRole(held.State, held.SuspendedBy) && !held.Stopping
}

// claimEnded is whether a claim in state will not hold its role again: this daemon does not
// supervise it (""), or it failed or retired. A child's own architect that failed does start again,
// at its child's next phase start (workflow's reviveArchitect), but that launch's ready queues a
// catch-up of its own, so nothing held for the failed claim waits for it. A suspended claim has
// not ended: a resume starts its agent again — the operator's resume or deliver whoever suspended
// it (`legion claims resume`), and the controller's any suspension but the operator's
// (supervise.Claim.HeldAgainst) — and so does the workflow's next start of its role for a claim
// the workflow suspended, which for an issue Dispatch moved out of Legion's work is its return (R20
// of dispatch://INFRA-3320). So a notice for a suspended or released architect waits for it, and
// one for a failed or retired architect ends undelivered (R18).
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
