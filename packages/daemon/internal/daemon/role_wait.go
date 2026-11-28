package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// roleNameBound is how long after its claim's ready a launched session may still be without its
// role's Envoy name. A pane takes the name after its ready answers: at its first turn, or, when no
// turn comes, once the plugin's first-turn bound has passed, DEFAULT_CLAIM_FIRST_TURN_BOUND_MS, 30
// seconds (packages/pi-shared/src/claim-first-turn-bound.ts; the comment of ClaimSession,
// packages/pi-legion/src/claim-session.ts). So a publish the listener refuses because no session
// holds the role (notify.ErrNoHolder), while the claim has said it is ready and has started no turn
// since, is the name not taken yet rather than the role taking nothing: the row runs again at the
// end of this bound and does not climb its backoff (roleWaiting.awaitsName,
// dispatch://INFRA-4131/spec#b-r-wait-ready). From the bound on, even with no turn, a refusal is
// the role taking nothing, which climbs the backoff and is logged at Error after 30 minutes
// (roleWaiting.tookNothing).
const roleNameBound = 30 * time.Second

// waitLoggedAfter is how long a row waits for its role, or for an event of the daemon's own,
// before its wait is logged a second time: the one line above Info the wait writes between its
// start and its end (settle's logWait, dispatch://INFRA-4131/spec#b-r-logged-once).
const waitLoggedAfter = 30 * time.Minute

// The lines of a wait (settle's logWait, and the ends of a wait: waitEnded, waitsEndedOnCommit):
// the 30-minute line of a role that said it was ready and took nothing, a fault nothing else
// shows, at Error; the 30-minute line of every other wait, at WARN; and the end of the wait, at
// Info, whether the row's run or a delete of the row ended it.
const (
	roleTookNothingLine = "outbox row's role is ready and took nothing for 30 minutes"
	waitedLongLine      = "outbox row has waited 30 minutes"
	waitEndedLine       = "outbox row's wait ended"
)

// roleWaiting is the wait of a row that publishes to one role's topic, the controller's among
// them, and waits for that role to take it (dispatch://INFRA-4131/spec#b-r-wait-ready and
// #b-r-capped): what the row's run read of the role's claim and what the listener answered, which
// decide when the row runs again (roleWaitNext), what it waits for (record.OutboxWait.WaitFor) and
// how its wait is logged (logWait). Every executor of a kind that waits for its role
// (record.OutboxKind.WaitsForRole) returns one, through roleWait, for each answer of the listener
// that keeps its row, and controllerNotice for each wait of its read of the role's holder
// (controllerHolds), which publishes nothing.
type roleWaiting struct {
	// topic is the topic the row publishes to, and token the claim it is held to
	// (outbox.heldTopic): the claim the role topic names, the project's controller claim for the
	// controller's role topic under `controller: daemon` among them.
	topic string
	token claim.Token
	// held is whether topic is held to a claim of this daemon at all. It is false for the
	// controller's role topic under `controller: operator`, the operator's own controller's, which
	// no claim this daemon records can say is ready.
	held bool
	// view is the role's claim as the run read it (outbox.supervisedClaim), the zero view where
	// this daemon supervises none.
	view supervise.ClaimView
	// published is whether this run's publish reached the listener, and answer the listener's
	// refusal of it, nil where it did not refuse or the run did not publish. The gate's own
	// refusal of a claim that records a session it does not hold (unheldSession) reached no
	// listener, so it is neither. launch is the claim's Generation when this run made the one
	// publish a not-ready launch or a non-release suspension allows (roleWaitGate), stored into
	// record.OutboxWait.WaitLaunch by roleWaitNext so a later run of the same launch publishes
	// nothing; 0 keeps the row's stored WaitLaunch as it is.
	published bool
	answer    error
	launch    uint64
	// err is the wait itself, a wait.ErrWaiting, as the executor words it.
	err error
}

func (w roleWaiting) Error() string { return w.err.Error() }

func (w roleWaiting) Unwrap() error { return w.err }

// roleGate is what roleWaitGate decides before an executor of a kind that waits for its role
// (record.OutboxKind.WaitsForRole) publishes to its topic:
//   - view is the role's claim as this run read it (the zero view where this daemon supervises no
//     machine for it or topic is held to no claim of this daemon);
//   - ended is non-empty when the claim's state ends this row undelivered without publishing
//     (no machine for an ending kind, or a failed or retired claim for any kind that ends on one);
//   - wait is a roleWaiting when this run must publish nothing and wait instead (no machine for a
//     waiting kind, a failed or retired controller claim under `controller: daemon`, an
//     unregistered launch, a claim suspended before any launch, or a not-ready registered launch
//     or non-release suspension that this row has already published to in the claim's current
//     Generation).
//
// When ended is empty and wait is nil, the executor may publish.
type roleGate struct {
	view  supervise.ClaimView
	ended string
	wait  error
}

// endsWithoutClaim is whether kind ends undelivered (without publishing) when this daemon holds
// no supervising machine for its role's claim (dispatch://INFRA-4131/spec#b-r-wait-ready, P4):
// notice (routed to a root architect with no machine), role_message (no pause holds a claim with
// no machine), and update_ask. Every other role-waiting kind waits for the owed start.
func endsWithoutClaim(kind record.OutboxKind) bool {
	return kind == record.OutboxKindNotice || kind == record.OutboxKindRoleMessage || kind == record.OutboxKindUpdateAsk
}

// publishesOncePerLaunch is whether a claim in view's state allows at most one publish of a row
// per Claim.Generation: a registered launch that has not said it is ready, or a claim suspended by
// any source other than the idle release (operator, controller, daemon, exit, unknown). A claim in
// such a state at Generation 0 has had no launch (supervise's writeTry raises the generation at
// each launch try), so no session of it can take a publish, and the gate allows none.
func publishesOncePerLaunch(view supervise.ClaimView) bool {
	return view.State == supervise.StateRegistered || (view.State == supervise.StateSuspended && !view.Released())
}

// roleWaitGate decides whether row, of a kind that waits for its role, may publish to topic at
// this run from the daemon's own claim record (dispatch://INFRA-4131/spec#b-r-wait-ready). Every
// one of the nine executors calls it before publishing, after its own non-claim ends and waits
// (a notice's fence, a merge forward that no longer stands, a kept message's hour, a released
// role's deliveryWake, an update round check).
//
// By the claim's state:
//   - topic held to no claim of this daemon (`controller: operator`), or an outbox built without
//     a supervisor in a unit test: publish;
//   - no machine for the token: ending kinds (notice, role_message, update_ask) end undelivered
//     without publishing; the six waiting kinds wait (key claim:<token>);
//   - failed or retired: every kind but controller_notice ends undelivered without publishing;
//     controller_notice under `controller: daemon` waits (controllerKeeper retries a failed or
//     retired controller);
//   - unregistered launch (queued, launch_uncertain, launching, shim_connected): wait, publish
//     nothing (key claim:<token>);
//   - registered not ready, or suspended by any source other than the release: publish once for
//     the claim's Generation (when row.Wait.WaitLaunch != view.Generation), otherwise wait and
//     publish nothing (key claim:<token>); a claim suspended before its first launch (Generation
//     0) has no session to take the publish, so it publishes nothing and waits for the launch;
//   - released (no launch in progress): publish — callers that wake a released role themselves
//     already ran wakeForDelivery / deliveryWake before reaching the gate, so only a role the
//     daemon will not wake, a pause-held role_message, or a controller_notice reaches here;
//   - said ready (ready, working, idle): publish.
func (r *outbox) roleWaitGate(row record.OutboxRow, topic string) roleGate {
	token, held := r.heldTopic(topic)
	if !held || r.supervisor == nil {
		return roleGate{}
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		if endsWithoutClaim(row.Kind) {
			return roleGate{ended: "its role has no claim"}
		}
		return roleGate{wait: r.roleWait(topic, supervise.ClaimView{}, nil,
			wait.Errorf("outbox row %d (%s) for %s waits for %s's claim, whose start is owed", row.ID, row.Kind, row.Issue, token))}
	}
	view := machine.View()
	switch {
	case supervise.FailedOrRetired(view.State):
		if row.Kind == record.OutboxKindControllerNotice {
			return roleGate{view: view, wait: r.roleWait(topic, view, nil,
				wait.Errorf("outbox row %d (%s) for %s waits for %s, whose claim is %s, to start again",
					row.ID, row.Kind, row.Issue, token, view.State))}
		}
		return roleGate{view: view, ended: fmt.Sprintf("its role's claim is %s", view.State)}
	case supervise.Unregistered(view.State):
		return roleGate{view: view, wait: r.roleWait(topic, view, nil,
			wait.Errorf("outbox row %d (%s) for %s waits for %s's launch %d, which is %s, to register its session",
				row.ID, row.Kind, row.Issue, token, view.Generation, view.State))}
	case publishesOncePerLaunch(view) && view.Generation == 0:
		return roleGate{view: view, wait: r.roleWait(topic, view, nil,
			wait.Errorf("outbox row %d (%s) for %s waits for %s, which is %s and has had no launch, to launch",
				row.ID, row.Kind, row.Issue, token, view.State))}
	case publishesOncePerLaunch(view) && row.Wait.WaitLaunch == view.Generation:
		return roleGate{view: view, wait: r.roleWait(topic, view, nil,
			wait.Errorf("outbox row %d (%s) for %s already published to %s's launch %d (%s) and waits for it to say it is ready",
				row.ID, row.Kind, row.Issue, token, view.Generation, view.State))}
	default:
		return roleGate{view: view}
	}
}

// roleWait is the role wait (roleWaiting) of a row that publishes to topic, whose role's claim the
// run read as view, and whose publish answered answer: the publish's error, nil where the run made
// no publish. waiting is the wait itself (a wait.ErrWaiting) as the executor words it, which the
// role wait unwraps to, so settle and every reader of the error still read it as that wait. When
// the run published to a not-ready registered launch or non-release suspension, launch records
// view.Generation so roleWaitNext stores it into record.OutboxWait.WaitLaunch (the gate allows
// no such publish at Generation 0, so launch is never 0 for one).
func (r *outbox) roleWait(topic string, view supervise.ClaimView, answer, waiting error) error {
	token, held := r.heldTopic(topic)
	w := roleWaiting{topic: topic, token: token, held: held, view: view, err: waiting}
	var unheld unheldSession
	if answer != nil && !errors.As(answer, &unheld) {
		w.published, w.answer = true, answer
		if held && publishesOncePerLaunch(view) {
			w.launch = view.Generation
		}
	}
	return w
}

// roleWaitPublished is roleWait for a run whose publish reached the listener and was not refused
// (for example, deliverHeld finding the listener stored the message for an earlier launch's
// session rather than the current launch's): it records no refusal answer, and still marks
// view.Generation when view is a once-per-launch state so a later run of the same launch does not
// publish again.
func (r *outbox) roleWaitPublished(topic string, view supervise.ClaimView, waiting error) error {
	token, held := r.heldTopic(topic)
	w := roleWaiting{topic: topic, token: token, held: held, view: view, published: true, err: waiting}
	if held && publishesOncePerLaunch(view) {
		w.launch = view.Generation
	}
	return w
}

// refusedReady is whether this run's publish was refused while the role's claim said it was ready
// (supervise.SaidReady), the claim being one of this daemon's.
func (w roleWaiting) refusedReady() bool {
	return w.held && w.answer != nil && supervise.SaidReady(w.view.State)
}

// turnSinceReady is whether the claim has started a turn since its last ready: a turn start that
// is not before the ready (supervise.Claim.LastTurnStart, LastReady).
func (w roleWaiting) turnSinceReady() bool {
	return !w.view.LastTurnStart.Before(w.view.LastReady)
}

// awaitsName is whether the refusal is the ready session not holding its role's name yet
// (roleNameBound): the listener answered that no session holds the role, while the claim said it
// was ready, has started no turn since, and now is still inside the bound after that ready. Such a
// row runs again at the bound's end, its step not raised, and waits for any change of the claim, a
// turn start among them (dispatch://INFRA-4131/spec#b-r-wait-ready).
func (w roleWaiting) awaitsName(now time.Time) bool {
	return w.refusedReady() && errors.Is(w.answer, notify.ErrNoHolder) && !w.turnSinceReady() &&
		now.Before(w.view.LastReady.Add(roleNameBound))
}

// tookNothing is whether the refusal is a ready role taking nothing, the fault the 30-minute line
// logs at Error (dispatch://INFRA-4131/spec#b-r-logged-once): this run's publish was refused while
// the claim said it was ready, and either a turn has started since that ready, or now is at or past
// roleNameBound after it, when even a session given no turn has taken its role's name.
func (w roleWaiting) tookNothing(now time.Time) bool {
	return w.refusedReady() && (w.turnSinceReady() || !now.Before(w.view.LastReady.Add(roleNameBound)))
}

// waitsFor is the key of what the row waits for (record.OutboxWait.WaitFor), whose event makes it
// due at once (record.Store.ExpediteWaits): the claim's ready alone for a refusal while the claim
// said it was ready, so a turn start or another change of a busy session does not cut the row's
// backoff short (record.WaitForReady); any change of the claim, its ready among them, while the
// claim has not said it is ready, the run published nothing, or the ready session has not taken its
// role's name yet (record.WaitForClaim); and nothing for a topic held to no claim of this daemon,
// whose wait no event of this daemon ends.
func (w roleWaiting) waitsFor(now time.Time) string {
	switch {
	case !w.held:
		return ""
	case w.refusedReady() && !w.awaitsName(now):
		return record.WaitForReady(w.token)
	default:
		return record.WaitForClaim(w.token)
	}
}

// eventWaiting is the wait of a row for an event this daemon records itself
// (dispatch://INFRA-4131/spec#b-r-wait-event): key names the event (record.WaitForClaim and the
// other builders of a key), the write that records it makes the row due at once
// (record.Store.ExpediteWaits), and the row waits on the capped backoff until then (roleWaitNext).
type eventWaiting struct {
	key string
	// err is the wait itself, a wait.ErrWaiting, as the executor words it.
	err error
}

func (w eventWaiting) Error() string { return w.err.Error() }

func (w eventWaiting) Unwrap() error { return w.err }

// eventWait is the event wait (eventWaiting) of a row that waits for the event key names. waiting
// is the wait itself (a wait.ErrWaiting) as the executor words it, which the event wait unwraps to,
// so settle and every reader of the error still read it as that wait.
func eventWait(key string, waiting error) error {
	return eventWaiting{key: key, err: waiting}
}

// claimWait is err, the answer of token's claim's machine to what an outbox row asked of it, as
// the row's event wait where that answer is a wait whose end the daemon records itself
// (dispatch://INFRA-4131/spec#b-r-wait-event). Each of these the machine ends with a change of the
// claim it tells (supervise.Deps.ClaimChanged), so the row waits for any change of the claim
// (record.WaitForClaim): a task on its way to the agent (supervise.ErrTaskSending), which the
// send's end settles; a delivery behind the claim's pending one (supervise.ErrDeliveryPending),
// which a turn's confirmation of that one, its retirement or its drop ends; a suspension held for
// a turn, a launch or a compaction (supervise.ErrSuspendHeld), which lands at its end; and a
// suspension that holds (supervise.ErrHeldByOperator, supervise.ErrHeldByController), which a
// resume lifts. A tree's reserved cleanup that the claim's admission or launch met
// (treelifecycle.ErrCleanupReserved) ends only at the next epoch of tree, the claim's tree, which
// admission opens (record.WaitForEpoch); so does the tree's confirmed cleanup
// (treelifecycle.ErrLifecycleClosed), which the admission or launch of a start meets between the
// confirmation and the promotion that opens that next epoch, and which is no failure of the row
// but the same wait, made one here. Every other answer is err as it is: a failure, or a wait whose
// end the daemon does not record, such as the Envoy listener's freeing of a role
// (supervise.ErrForgetWaitsForRole), which keeps the outbox's backoff.
func claimWait(token claim.Token, tree string, err error) error {
	switch {
	case errors.Is(err, treelifecycle.ErrCleanupReserved):
		return eventWait(record.WaitForEpoch(tree), err)
	case errors.Is(err, treelifecycle.ErrLifecycleClosed):
		return eventWait(record.WaitForEpoch(tree), wait.Errorf("%s waits for the next epoch of tree %s: %w", token, tree, err))
	case errors.Is(err, supervise.ErrTaskSending), errors.Is(err, supervise.ErrDeliveryPending),
		errors.Is(err, supervise.ErrSuspendHeld), errors.Is(err, supervise.ErrHeldByOperator),
		errors.Is(err, supervise.ErrHeldByController):
		return eventWait(record.WaitForClaim(token), err)
	}
	return err
}

// launchWait is waiting, the wait of a row of a kind that waits for its role
// (record.OutboxKind.WaitsForRole) for a launch of token's claim still to come, as the row's event
// wait for any change of that claim (record.WaitForClaim): the launch its own run's wake asked of
// the released role (wakeForDelivery), the start queued that brings the role back, or the launch the
// wake pacing holds over no process (parkedWait). That launch, its registration and its ready are
// each a change of the claim the machine tells (supervise.Deps.ClaimChanged), so each makes the row
// due at once, and its ready finds the row however far its backoff has climbed
// (dispatch://INFRA-4131/spec#b-r-wait-ready). Such a wait that names its moment (wait.Until) keeps
// it, so the row runs then at the latest.
func launchWait(token claim.Token, waiting error) error {
	return eventWait(record.WaitForClaim(token), waiting)
}

// issueStopWait is err, the store's answer to the authorization of a close or a release of
// issue's pod (store.Store.IssueSuspension, store.Store.IssueRelease), as the row's event wait
// where that answer is one of the store's waits (dispatch://INFRA-4131/spec#b-r-wait-event): the
// issue's role stops still queued (store.ErrRoleStopsUnfinished), which their finishes end
// (record.WaitForRoleStops); a stored claim of the issue that may still hold a process
// (store.StoredClaimWait), which the claim's stop ends (record.WaitForClaim); and the release's
// workspace check still queued (store.ErrWorkspaceCheckUnfinished), which its finish ends
// (record.WaitForWorkspaceCheck). Every other answer is err as it is.
func issueStopWait(issue string, err error) error {
	var stored store.StoredClaimWait
	switch {
	case errors.Is(err, store.ErrRoleStopsUnfinished):
		return eventWait(record.WaitForRoleStops(issue), err)
	case errors.Is(err, store.ErrWorkspaceCheckUnfinished):
		return eventWait(record.WaitForWorkspaceCheck(issue), err)
	case errors.As(err, &stored):
		return eventWait(record.WaitForClaim(stored.Token), err)
	}
	return err
}

// finishedWaits is the keys of the waits row's finish ends (record.Store.ExpediteWaits,
// dispatch://INFRA-4131/spec#b-r-wait-event), which finish makes due in the transaction that
// deletes the row: a role stop of the issue (a supervise row's suspend, step_stop, issue_close or
// release), which a close or a release of its pod waits for (record.WaitForRoleStops); the issue's
// workspace check, its issue_bundle row, which a release's teardown waits for
// (record.WaitForWorkspaceCheck); its branch, the issue_branch row, which the first start of a
// child's own architect waits for (record.WaitForBranch); and a notice, which every later notice
// to the same architect waits behind (record.WaitForRow). Every other row ends no wait.
func finishedWaits(row record.OutboxRow) []string {
	switch row.Kind {
	case record.OutboxKindNotice:
		return []string{record.WaitForRow(row.ID)}
	case record.OutboxKindIssueBundle:
		return []string{record.WaitForWorkspaceCheck(row.Issue)}
	case record.OutboxKindIssueBranch:
		return []string{record.WaitForBranch(row.Issue)}
	case record.OutboxKindSupervise:
		payload, err := record.DecodeOutboxPayload(row)
		if request, ok := payload.(record.SuperviseRequest); err == nil && ok && roleStop(request.Op) {
			return []string{record.WaitForRoleStops(row.Issue)}
		}
	}
	return nil
}

// roleStop is whether op is one of the role stops a close and a release of an issue's pod wait for
// (store's issueStopFences): a suspend, a step stop, an issue close and a role stop of an idle
// release.
func roleStop(op record.SuperviseOp) bool {
	return op == "suspend" || op == "step_stop" || op == "issue_close" || op == "release"
}

// readyReleasedLine is the one line a ready's release writes once its transaction commits
// (releaseReady), naming the claims that said they are ready and the rows it made due.
const readyReleasedLine = "outbox rows released: their role is ready"

// releaseReady makes due at once every row that waits for one of tokens' claims, each of which has
// said it is ready (workflow's claimReady, dispatch://INFRA-4131/spec#b-r-wait-ready): a row the
// gate holds, that waits for the role's launch (launchWait) or that waits for the role's name
// (record.WaitForClaim), and a row on the capped waits of a refusal while the claim said it was
// ready (record.WaitForReady), which only a ready makes due. Each row's step goes back to 0, a row
// a lane holds is marked woken so that its retry makes it due at once, and a catch-up the wake
// pacing holds to a moment still ahead of now keeps that moment, since its wait is the turn the
// operator's rate gave it and not the architect being away (record.Store.ExpediteWaits). Once the
// release commits it writes one line naming the claims and the rows it made due.
//
// A row waits for the claim its last run read: a notice for the architect it was routed to then
// (noticeArchitect), so a notice whose routing moved to another architect since runs at its next
// step, at most RoleWaitBackoffMost later. A launch says it is ready before its session takes its
// role's Envoy name, so a row this releases can be refused because no session holds the role; it
// then waits for that name (roleWaiting.awaitsName), not for another ready.
func (r *outbox) releaseReady(ctx context.Context, tokens ...claim.Token) error {
	keys := make([]string, 0, 2*len(tokens))
	for _, token := range tokens {
		keys = append(keys, record.WaitForClaim(token), record.WaitForReady(token))
	}
	return r.expediteWaits(ctx, keys, func(rows []int64) {
		r.log.Info(readyReleasedLine, "claims", tokens, "rows", rows)
	})
}

// releaseChanged makes due at once every row that waits for one of keys, which a change of one of
// this daemon's claims made happen (workflow's claimChanged): the claim's own key, its tree's, and
// its ready's where the change was a ready (dispatch://INFRA-4131/spec#b-r-wait-event), on the same
// rules as a ready's release (releaseReady). A claim changes at every turn, so this release writes
// no line of its own: each row it makes due writes its wait's lines as it runs (logWait).
func (r *outbox) releaseChanged(ctx context.Context, keys []string) error {
	return r.expediteWaits(ctx, keys, nil)
}

// expediteWaits makes due, in a transaction of its own, every row that waits for one of keys
// (record.Store.ExpediteWaits), and calls released with the rows it made due once that transaction
// has committed, where it made any and released is set. The transaction owns the commit hooks the
// expedite registers (projectRecords), so each row's lane is woken once the rows are due.
func (r *outbox) expediteWaits(ctx context.Context, keys []string, released func(rows []int64)) error {
	now := r.now()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		due, err := r.records.ExpediteWaits(ctx, tx, keys, now)
		if err != nil || len(due) == 0 || released == nil {
			return err
		}
		ids := make([]int64, len(due))
		for i, row := range due {
			ids[i] = row.ID
		}
		store.OnCommit(ctx, func() { released(ids) })
		return nil
	}); err != nil {
		return fmt.Errorf("release the outbox rows that wait for %v: %w", keys, err)
	}
	return nil
}

// listenerRefused is whether err is the listener's refusal of a role publish that can keep its row:
// no session holds the role (notify.ErrNoHolder) or its holder is unresponsive
// (notify.ErrHolderUnresponsive), the holder did not confirm it (notify.ErrNotDelivered), does not
// advertise its delivery mode (notify.ErrRefusedByHolder), or the listener's own NATS grant denied
// the forward (notify.ErrPublishDenied). The gate's own refusal (unheldSession) is no answer of the
// listener's, and nor is a failure to reach the listener or to read its answer, a store read or a
// Dispatch read: those stay the row's failures.
func listenerRefused(err error) bool {
	var unheld unheldSession
	if err == nil || errors.As(err, &unheld) {
		return false
	}
	var notDelivered notify.ErrNotDelivered
	var refused notify.ErrRefusedByHolder
	var denied notify.ErrPublishDenied
	return errors.Is(err, notify.ErrNoHolder) || errors.Is(err, notify.ErrHolderUnresponsive) ||
		errors.As(err, &notDelivered) || errors.As(err, &refused) || errors.As(err, &denied)
}

// roleWaitRun is whether the run of a row of kind that ended in err is a wait of R1, R2 or R6
// (dispatch://INFRA-4131/spec#b-r-wait-ready, #b-r-capped and #b-r-wait-event): its run returned a
// role wait or an event wait, or its kind waits for its role (record.OutboxKind.WaitsForRole) and
// its run waited or met the listener's refusal. Such a row runs again on the capped backoff of its
// own wait (roleWaitNext), and is logged by its wait rather than by its attempts (logWait). Every
// other run keeps the outbox's backoff (record.RetryBackoff) and its lines.
func roleWaitRun(kind record.OutboxKind, err error) bool {
	var role roleWaiting
	var event eventWaiting
	if errors.As(err, &role) || errors.As(err, &event) {
		return true
	}
	return kind.WaitsForRole() && (errors.Is(err, wait.ErrWaiting) || listenerRefused(err))
}

// roleWaitNext is when row runs again after a run that ended in cause, a wait of roleWaitRun's, at
// now, and the wait it keeps from then on (dispatch://INFRA-4131/spec#b-r-capped): the row waits
// record.RoleWaitBackoff of its wait's step, and the step goes up by one. A role wait that read a
// ready other than the one the step counts from (record.OutboxWait.WaitReady) starts the step again
// from 0, so a relaunch's ready restarts the backoff even where the ready's own release of the row
// was lost to a restart; and a ready session that has not taken its role's name yet runs again at
// roleNameBound after its ready, its step not raised (roleWaiting.awaitsName). The key of what the
// row waits for is the wait's own (roleWaiting.waitsFor, eventWaiting's key), and none for any
// other wait. The moment the wait started and its 30-minute line are settle's (logWait), and the
// launch the row last published to (record.OutboxWait.WaitLaunch) is kept as the row holds it
// unless this run published to a not-ready launch or non-release suspension (roleWaiting.launch).
// A GitHub rate limit and a wait that names its moment still decide over this moment (retry).
func roleWaitNext(row record.OutboxRow, cause error, now time.Time) (time.Time, record.OutboxWait) {
	next := row.Wait
	next.WaitFor = ""
	var role roleWaiting
	var event eventWaiting
	switch {
	case errors.As(cause, &role):
		// The store keeps a moment to the microsecond, so the ready the step counts from is
		// compared at that grain.
		if !role.view.LastReady.Truncate(time.Microsecond).Equal(next.WaitReady.Truncate(time.Microsecond)) {
			next.WaitStep, next.WaitReady = 0, role.view.LastReady
		}
		next.WaitFor = role.waitsFor(now)
		if role.launch != 0 {
			next.WaitLaunch = role.launch
		}
		if role.awaitsName(now) {
			return role.view.LastReady.Add(roleNameBound), next
		}
	case errors.As(cause, &event):
		next.WaitFor = event.key
	}
	at := now.Add(record.RoleWaitBackoff(next.WaitStep))
	next.WaitStep++
	return at, next
}

// logWait logs one run of row that ended in err, a wait of roleWaitRun's, by the row's wait rather
// than by its attempts (dispatch://INFRA-4131/spec#b-r-logged-once), and answers the wait the row
// keeps from this run on. The first run that waits writes one Info line, `outbox row waits`, and
// starts the wait (record.OutboxWait.WaitSince). The first run at or after waitLoggedAfter from
// that start writes one line more and marks it written (WaitLogged): at Error, roleTookNothingLine,
// where this run's publish was refused while the claim said it was ready and the role has had its
// chance to take it (roleWaiting.tookNothing), and at WARN, waitedLongLine, for every other wait: a
// claim that has not said it is ready, a ready session that has not taken its role's name yet, a
// topic held to no claim of this daemon, an event. The level is read at that run, since the line is
// written once. Every other run writes `outbox row waits` at Debug. Each line names the row's
// issue, its kind, its attempts, the role or the event it waits for, and the listener's answer to
// this run's publish where there is one. Both marks are on the row, so a daemon restarted in the
// wait writes neither line again.
func (r *outbox) logWait(ctx context.Context, row record.OutboxRow, err error, kept string) record.OutboxWait {
	now := r.now().UTC()
	next := row.Wait
	fields := append([]any{"row", row.ID, "kind", row.Kind, "issue", row.Issue, "attempts", row.Attempts}, waitFields(err)...)
	fields = append(fields, "error", kept)
	var role roleWaiting
	switch {
	case next.WaitSince.IsZero():
		next.WaitSince = now
		r.log.Log(ctx, slog.LevelInfo, "outbox row waits", fields...)
	case !next.WaitLogged && !now.Before(next.WaitSince.Add(waitLoggedAfter)):
		next.WaitLogged = true
		fields = append(fields, "waited", now.Sub(next.WaitSince).String())
		if errors.As(err, &role) && role.tookNothing(now) {
			r.log.Log(ctx, slog.LevelError, roleTookNothingLine, fields...)
		} else {
			r.log.Log(ctx, slog.LevelWarn, waitedLongLine, fields...)
		}
	default:
		r.log.Log(ctx, slog.LevelDebug, "outbox row waits", fields...)
	}
	return next
}

// waitFields is what a wait's lines name of what err waits for: the claim of a role wait, or its
// topic where that is held to no claim of this daemon, with the listener's answer to the run's
// publish where there is one, kept as the row keeps a far side's text; the key of an event wait;
// and nothing for any other wait, whose error says what it waits for.
func waitFields(err error) []any {
	var role roleWaiting
	var event eventWaiting
	switch {
	case errors.As(err, &role):
		fields := []any{"topic", role.topic}
		if role.held {
			fields = []any{"role", string(role.token)}
		}
		if role.answer != nil {
			fields = append(fields, "answer", storetext.FarSideError(role.answer.Error()))
		}
		return fields
	case errors.As(err, &event):
		return []any{"event", event.key}
	}
	return nil
}

// waitEndedOnCommit logs, once the transaction of ctx commits, that the wait of row ended with
// the row (waitEnded), where the row was waiting: the run that finishes the row, delivered or not
// (outbox.finish, finishRefused).
func (r *outbox) waitEndedOnCommit(ctx context.Context, row record.OutboxRow) {
	if row.Wait.WaitSince.IsZero() {
		return
	}
	store.OnCommit(ctx, func() {
		waitEnded(r.log, record.WaitingRow{ID: row.ID, Kind: row.Kind, Issue: row.Issue, Attempts: row.Attempts,
			WaitSince: row.Wait.WaitSince}, r.now(), "its run")
	})
}

// waitsEndedOnCommit logs, once the transaction of ctx commits, that the wait of each of rows
// ended (waitEnded): rows a delete took out of the outbox without running them, by is what deleted
// them, and now is the moment of the delete. A row that was not waiting (record.WaitingRow's
// WaitSince zero) ended no wait and logs nothing. A delete made outside a commit scope has no
// commit to wait for, so its lines are written at once.
func waitsEndedOnCommit(ctx context.Context, log *slog.Logger, rows []record.WaitingRow, by string, now time.Time) {
	var waited []record.WaitingRow
	for _, row := range rows {
		if !row.WaitSince.IsZero() {
			waited = append(waited, row)
		}
	}
	if len(waited) == 0 {
		return
	}
	ended := func() {
		for _, row := range waited {
			waitEnded(log, row, now, by)
		}
	}
	if !store.CommitScoped(ctx) {
		ended()
		return
	}
	store.OnCommit(ctx, ended)
}

// waitEnded is the one line that a row's wait ended, at Info (dispatch://INFRA-4131/spec#b-r-logged-once):
// the row, its kind and issue, its attempts, how long it waited from the start of its wait to now,
// and by what it ended: its run, or the delete that took it out of the outbox.
func waitEnded(log *slog.Logger, row record.WaitingRow, now time.Time, by string) {
	if log == nil {
		log = slog.Default()
	}
	log.Info(waitEndedLine, "row", row.ID, "kind", row.Kind, "issue", row.Issue, "attempts", row.Attempts,
		"waited", now.Sub(row.WaitSince).String(), "endedBy", by)
}

// droppedStarts is the wake's starts the cancel or the decline of a release deleted
// (store.ReleaseCancelled.DroppedStarts), as the rows whose waits waitsEndedOnCommit ends.
func droppedStarts(starts []store.DroppedStart) []record.WaitingRow {
	rows := make([]record.WaitingRow, len(starts))
	for i, start := range starts {
		rows[i] = record.WaitingRow{ID: start.ID, Kind: record.OutboxKind(start.Kind), Issue: start.Issue,
			Attempts: start.Attempts, WaitSince: start.WaitSince}
	}
	return rows
}
