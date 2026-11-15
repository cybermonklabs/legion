package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's half of a project's pause (dispatch://INFRA-3759). The pause's records and its two
// transactions are the store's (internal/store/pause.go), its flag and its acts the pause
// service's (internal/pause), and what a machine does while the project is paused the
// supervisor's (internal/supervise/pause.go). What is the daemon's is what joins them:
//
//   - the stop transaction a machine's pause suspension ends in (projectPause.stopped,
//     supervise.Deps.PauseStopped, decision 6), and the let-out moment of a paced launch
//     (projectPause.wakeLetOut, Deps.WakeLetOut, decision 14);
//   - what runs after a pause or a resume commits (projectPause.Paused and projectPause.Resumed,
//     the service's Effects, decisions 3 and 22);
//   - the boot's read of the flag before any claim is restored, and its pass over the claims a
//     restart finds live while paused (supervision.start, decision 5);
//   - the outbox's `pause_stop` executor (outbox.pauseStop, decision 4).

// pauseStopWait is how long a `pause_stop` row waits before it asks its claim again while the
// claim holds the stop for its turn or its launch (ErrSuspendHeld), or a launch whose outcome is
// uncertain stands. The wait is bounded rather than the backoff's minute, so a turn of an hour
// asks six times and logs no warning a minute (decision 4); what ends it sooner is the stop
// transaction, which makes the row due at once when it records the claim stopped
// (store.ExpeditePauseStopRowsTx) and wakes its lane.
const pauseStopWait = 10 * time.Minute

// pauseHeldRowReleased is the line the pause service writes for each row a resume released
// (decision 23, internal/pause's Service.Resume); the stop transaction writes it too for the start
// it owes once its pause has ended with no pause open after it, so the log names that row as well.
const pauseHeldRowReleased = "pause: a held row released"

// projectPause is the daemon's half of its project's pause: the service, which holds the flag and
// makes each act, and what the service hands the daemon to do (pause.Effects). It is built only
// with the workflow (run): a daemon with no workflow has no outbox to run a pause's stops, so it
// neither pauses nor reads a pause, and its machines are handed no flag (nil Deps.Paused, never
// paused).
type projectPause struct {
	service *pause.Service
	store   *store.Store
	records *record.Postgres
	pods    *store.PodRecords
	// project is the project's Dispatch key ("LEGION"), the spelling of project_pauses; token is
	// its claim token ("legion", claim.ProjectToken), the spelling of the claim-keyed tables.
	project string
	token   string
	// backstop is the idle release's backstop (`backstop_minutes`), which the stop of an open
	// phase's worker owed no launch arms (decision 6); zero where the deployment releases no pod,
	// and then the stop arms none, as a release then arms none.
	backstop time.Duration
	now      func() time.Time
	log      *slog.Logger
	// workflow is the runtime this half belongs to, whose outbox lanes and admission pass the
	// resume wakes and applies. supervisor is set by openSupervision before any machine exists:
	// the catch-up reads its machines and the resume posts to them. The API, the only caller of
	// the service's acts and so of the Effects, is served after both are set.
	workflow   *workflowRuntime
	supervisor *supervisor
}

// newProjectPause builds the daemon's pause over st for cfg's project. Its flag reads no pause
// until the boot loads it (supervision.start).
func newProjectPause(cfg config.Config, st *store.Store, token string, w *workflowRuntime, clock supervise.Clock, log *slog.Logger) (*projectPause, error) {
	pods, err := st.PodRecordsOf(cfg.Project)
	if err != nil {
		return nil, err
	}
	p := &projectPause{store: st, records: record.NewStore(), pods: pods, project: cfg.Project, token: token,
		backstop: releaseBackstop(cfg), now: clock.Now, log: log, workflow: w}
	if p.service, err = pause.New(pause.Options{Store: st, Records: p.records, Project: cfg.Project, Config: cfg,
		Effects: p, Log: log, Now: clock.Now}); err != nil {
		return nil, err
	}
	return p, nil
}

// releaseBackstop is the idle release's backstop where the deployment releases pods
// (attachIdleRelease reads the same rule), and zero where it releases none.
func releaseBackstop(cfg config.Config) time.Duration {
	k := cfg.Runtime.Kubernetes
	if cfg.Runtime.Name != "kubernetes" || k == nil || !k.IdleRelease.Releases() {
		return 0
	}
	return k.IdleRelease.Backstop()
}

// stopped is the stop transaction (supervise.Deps.PauseStopped, decision 6): the claim write a
// pause's suspension ends in, with the pause's records, in one store transaction, whose context
// carries the machine's claim-write deadline. In order:
//
//   - the issue's lock (store.LockIssueForPause), so two stops of one pod's roles, the resume's
//     end of the pod's hold and a role's ready run one after another; then the row of the
//     suspension's pause for share (store.LockPauseTx), open or ended, so a resume of it waits;
//     and where that pause has ended, the project's open pause for share too
//     (store.LockOpenPauseTx), taken before anything is written so a later pause's resume waits
//     for this transaction rather than meeting it halfway;
//   - the claim, and its pending delivery where it holds one, as the plain write writes them
//     (store.PutClaimTx, store.PutClaimAndDeliveryTx);
//   - the stop (store.RecordPauseStoppedTx), and the claim's `pause_stop` rows made due at once
//     (store.ExpeditePauseStopRowsTx), so a stop that waited for this turn's end ends now.
//
// A claim no longer suspended — a later write of a claim whose stop is still owed (the machine's
// putStops) — stops there: its stop is recorded, and a claim that runs now needs no pod hold, no
// owed launch and no backstop. A suspended claim then gets, on the first record of its stop:
// the pod's hold where no role of the pod is live (store.IssueHasLiveRoleTx,
// store.OpenPausePodHoldTx), and its role's open resume closed (store.CloseRoleOpenResumeTx); and
// either the launch it is owed — a held wake of the pause (store.PodRecords.TakeHeldPodWake with
// pods.WakePause, which cancels the role's own backstop) with its start row
// (workflow.PauseWakeRow) — or, owed none as the worker of the issue's open phase serving its
// generation, its backstop (store.ArmPodBackstopTx). A start owed once the pause has ended, with
// no pause open after it, is logged as a released held row (decision 23); a later open pause
// holds it, and that pause's resume logs it.
//
// It is safe to make again: the machine owes a stop whose commit's answer was lost and makes it
// again with its next claim write. RecordPauseStoppedTx answers false for a stop already recorded,
// and then nothing of the first run is written twice: no second hold, no second backstop, and the
// role's open resume — the first run's held wake — is left open, so the held wake taken again
// conflicts with it (one open resume per role) and queues no second start. It writes no log line
// of the stop itself: the machine writes supervise.PauseRoleStoppedMessage once it lands.
func (p *projectPause) stopped(ctx context.Context, c supervise.Claim, stop supervise.PauseStop) error {
	at := p.now().UTC()
	var made pauseStopMade
	if err := p.store.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		made, err = p.stopTx(ctx, tx, c, stop, at)
		return err
	}); err != nil {
		return fmt.Errorf("write %s stopped for pause %d: %w", c.Token, stop.Pause, err)
	}
	if made.started || made.expedited > 0 {
		p.workflow.wakeOutbox(c.Issue, record.OutboxKindSupervise)
	}
	if made.released != 0 {
		p.log.Info(pauseHeldRowReleased, "pause", stop.Pause, "row", made.released, "kind", string(record.OutboxKindSupervise),
			"issue", c.Issue, "claim", string(c.Token))
	}
	return nil
}

// pauseStopMade is what one stop transaction wrote that its caller acts on once it commits: a
// start row queued (started), the start row's id where it is a released held row (released), and
// how many of the claim's `pause_stop` rows it made due (expedited).
type pauseStopMade struct {
	started   bool
	released  int64
	expedited int64
}

// stopTx is stopped's transaction.
func (p *projectPause) stopTx(ctx context.Context, tx pgx.Tx, c supervise.Claim, stop supervise.PauseStop, at time.Time) (pauseStopMade, error) {
	var made pauseStopMade
	if err := store.LockIssueForPause(ctx, tx, c.Issue); err != nil {
		return made, err
	}
	held, err := store.LockPauseTx(ctx, tx, stop.Pause)
	if err != nil {
		return made, err
	}
	later := false
	if held.Ended() {
		if _, later, err = store.LockOpenPauseTx(ctx, tx, held.Project); err != nil {
			return made, err
		}
	}
	if c.Pending != nil {
		err = store.PutClaimAndDeliveryTx(ctx, tx, supervise.StoredClaim(c), *c.Pending)
	} else {
		err = store.PutClaimTx(ctx, tx, c)
	}
	if err != nil {
		return made, err
	}
	first, err := store.RecordPauseStoppedTx(ctx, tx, held.ID, store.PauseStopClaim{Claim: c.Token, Tree: c.Tree, Issue: c.Issue,
		Role: c.Role, Generation: c.Generation}, at, stop.LaunchOwed)
	if err != nil {
		return made, err
	}
	if made.expedited, err = store.ExpeditePauseStopRowsTx(ctx, tx, c.Issue, c.Role, at); err != nil {
		return made, err
	}
	if c.State != supervise.StateSuspended {
		return made, nil
	}
	if first {
		live, err := store.IssueHasLiveRoleTx(ctx, tx, p.token, c.Issue)
		if err != nil {
			return made, err
		}
		if !live {
			if _, err := store.OpenPausePodHoldTx(ctx, tx, held.ID, c.Issue, at); err != nil {
				return made, err
			}
		}
		if _, err := store.CloseRoleOpenResumeTx(ctx, tx, p.token, c.Issue, c.Role, at); err != nil {
			return made, err
		}
	}
	issue, err := p.records.Issue(ctx, tx, c.Issue)
	if err != nil || issue == nil {
		// A claim of an issue the daemon keeps no record of — an operator's own spawn — has no
		// workflow to queue a start or arm a backstop in.
		return made, err
	}
	if stop.LaunchOwed {
		taken, err := p.pods.TakeHeldPodWake(ctx, tx, c.Issue, c.Role, pods.WakePause, at, held.ID)
		if err != nil || !taken {
			return made, err
		}
		row, err := record.NewOutboxRow(c.Issue, workflow.PauseWakeRow(*issue, c.Role, held.ID), at)
		if err != nil {
			return made, err
		}
		id, err := p.records.EnqueueID(ctx, tx, row)
		if err != nil {
			return made, err
		}
		made.started = true
		if held.Ended() && !later {
			made.released = id
		}
		return made, nil
	}
	if first && p.backstop > 0 && c.Role == workflow.OpenPhaseRole(issue.Phase) && c.ServingGeneration == issue.Generation {
		return made, store.ArmPodBackstopTx(ctx, tx, store.PodWakeBackstop{Project: p.token, Issue: c.Issue, Role: c.Role,
			DueAt: at.Add(p.backstop)})
	}
	return made, nil
}

// wakeLetOut is supervise.Deps.WakeLetOut (decision 14): the moment the wake pacing lets a launch
// of c out becomes the stuck clock of its role's open resume (store.Store.SetResumeClock, which
// never moves a clock back), so a launch that waited its turn in a long queue is not warned stuck
// for the wait. The machine calls it inside its decision, so the write runs on a goroutine of its
// own, as podResume's stamp does, under a hold mark of its own; a failed write is logged and the
// clock keeps the moment it had.
func (p *projectPause) wakeLetOut(c supervise.Claim, at time.Time) {
	if c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		if _, err := p.store.SetResumeClock(ctx, p.token, c.Issue, c.Role, at); err != nil {
			p.log.Warn("wake: could not start a resume's stuck clock at its launch's let-out moment",
				"claim", c.Token, "at", at, "error", err)
		}
	}()
}

// Paused is the catch-up once a pause has committed and the flag is set (pause.Effects, decision
// 3): every live role of the project's machines but the controller's that the pause's
// transaction did not name — a launch that passed the gate before the flag was set, a claim the
// store held suspended while its machine was already bringing it back — is asked to stop through
// the store, a stop row and a `pause_stop` row each (store.Store.AskPauseStops), so a restart
// keeps the ask. Then the lifecycle lane of every issue a stop was asked of is woken, so the stops
// run now rather than at the next sweep. A failed ask is logged: the gate still holds every
// launch, and the next boot's pass asks what this one could not.
func (p *projectPause) Paused(ctx context.Context, held store.Pause, asked []store.PauseStopClaim) {
	named := make(map[claim.Token]bool, len(asked))
	issues := map[string]bool{}
	for _, stop := range asked {
		named[stop.Claim], issues[stop.Issue] = true, true
	}
	var missed []store.PauseStopClaim
	for _, m := range p.supervisor.supervised() {
		c := m.View().Claim
		if named[c.Token] || !pausable(c) {
			continue
		}
		missed = append(missed, store.PauseStopClaim{Claim: c.Token, Tree: c.Tree, Issue: c.Issue, Role: c.Role, Generation: c.Generation})
	}
	if len(missed) > 0 {
		caught, err := p.store.AskPauseStops(ctx, held.ID, missed, p.now().UTC(), pause.StopRow)
		if err != nil {
			p.log.Error("pause: could not ask the stop of the live roles the pause's transaction did not name; the gate holds their launches, and the next boot asks them",
				"pause", held.ID, "claims", stopTokens(missed), "error", err)
		}
		for _, stop := range caught {
			issues[stop.Issue] = true
		}
		if len(caught) > 0 {
			p.log.Info("pause: the catch-up asked the stop of live roles the pause's transaction did not name",
				"pause", held.ID, "claims", stopTokens(caught))
		}
	}
	for issue := range issues {
		p.workflow.wakeOutbox(issue, record.OutboxKindSupervise)
	}
}

// Resumed is what follows a resume's commit once the flag is cleared (pause.Effects, decision 22),
// in its order: every machine of the project is posted the end of the pause
// (supervise.ResumePause), which drops a hold of that pause, lets a launch its gate held into the
// wake pacing and sends the task the claim holds where it may now go; the hold of each pod with a
// live role ends (endLiveHolds); every outbox lane of the project with a row is woken, so the rows
// the pause held run now (outbox.wakeUnfinished); and one admission pass is applied
// (applyResumePass). The resume launches no role itself: each held row launches its role through
// the pacing when it runs.
func (p *projectPause) Resumed(ctx context.Context, ended store.Pause) {
	for _, m := range p.supervisor.supervised() {
		token := m.View().Claim.Token
		p.supervisor.post(token, supervise.ResumePause{Claim: token, Pause: ended.ID})
	}
	p.endLiveHolds(ctx, ended)
	if r := p.workflow.outboxWake.Load(); r != nil {
		r.wakeUnfinished(ctx)
	}
	p.applyResumePass(ctx, ended)
}

// applyResumePass applies the resume's one admission pass as a fact of its own
// (intake.ProjectResumed), keyed on the ended pause, through the workflow's handlers: admission
// promotes on it as on every fact that is not a Dispatch issue, and nothing else acts on it. It is
// not the controller's tick, which would also queue the controller a tick notice: the resume
// wakes nobody by itself. A failed pass is logged; the next fact or tick promotes the line.
func (p *projectPause) applyResumePass(ctx context.Context, ended store.Pause) {
	w := p.workflow
	eventID := fmt.Sprintf("project-resumed:%s:%d", p.project, ended.ID)
	if _, err := intake.ApplyFact(ctx, w.pool, "pause", eventID, intake.ProjectResumed{Pause: ended.ID}, w.handlers...); err != nil {
		p.log.Error("pause: the resume's admission pass failed; the next fact or tick promotes the line",
			"pause", ended.ID, "error", err)
	}
}

// endLiveHolds ends the hold of each pod of the project in which a role is live (decision 13): that
// pod is already in the state a released pod's wake leaves it in. Each hold is ended in a short
// transaction of its own under its issue's lock, and the live role is read under that lock, so a
// stop transaction of a role of the same pod commits first and the read sees it. A hold whose pod
// has no live role stays until a role of it is ready again or the pod is deleted. A failed end is
// logged and the hold stays.
func (p *projectPause) endLiveHolds(ctx context.Context, ended store.Pause) {
	holds, err := p.store.OpenPausePodHolds(ctx, p.project)
	if err != nil {
		p.log.Error("pause: the resume could not read the pods a pause holds; their holds stay until a role of each is ready",
			"pause", ended.ID, "error", err)
		return
	}
	for _, hold := range holds {
		released, err := p.endLiveHold(ctx, hold.Issue)
		switch {
		case err != nil:
			p.log.Error("pause: the resume could not end a pod's hold; it stays until a role of the pod is ready",
				"pause", ended.ID, "issue", hold.Issue, "held_by", hold.Pause, "error", err)
		case released:
			p.log.Info("pause: the resume ended a pod's hold; a role of the pod is live", "pause", ended.ID,
				"issue", hold.Issue, "held_by", hold.Pause)
		}
	}
}

// endLiveHold ends issue's pod hold where a role of the pod is live, and answers whether it did
// (store.Store.EndLivePausePodHold, under the issue's lock).
func (p *projectPause) endLiveHold(ctx context.Context, issue string) (bool, error) {
	return p.store.EndLivePausePodHold(ctx, p.token, issue, store.PausePodHoldResumed, p.now().UTC())
}

// askBootStops is the boot's pass while the project is paused (decision 5): once every claim is
// restored, each live claim of the project but the controller's that the open pause has asked no
// stop of — a role an earlier daemon launched before the pause reached it, or whose stop it never
// queued — is asked to stop (store.Store.AskLivePauseStops). Its rows run once the outbox's boot
// sweep starts the lanes, and the relaunches and the stale-plugin pass of this boot meet the
// gate meanwhile, so the boot launches nothing but the controller.
func (p *projectPause) askBootStops(ctx context.Context, open store.Pause) error {
	asked, err := p.store.AskLivePauseStops(ctx, open.ID, p.now().UTC(), pause.StopRow)
	if err != nil {
		return err
	}
	if len(asked) > 0 {
		p.log.Info("pause: the boot asked the stop of the live roles the project's pause has not stopped",
			"pause", open.ID, "claims", stopTokens(asked))
	}
	return nil
}

// pausable is whether a pause stops c: a live claim — not suspended, failed or retired — that is
// not the controller's, which the pause never stops (store's liveClaimsOf reads the same rule).
func pausable(c supervise.Claim) bool {
	switch c.State {
	case supervise.StateSuspended, supervise.StateFailed, supervise.StateRetired:
		return false
	}
	return !claim.IsController(c.Role, c.Issue, c.Tree)
}

// stopTokens is the claims of stops, for a log line.
func stopTokens(stops []store.PauseStopClaim) []string {
	tokens := make([]string, len(stops))
	for i, stop := range stops {
		tokens[i] = string(stop.Claim)
	}
	return tokens
}

// pauseStop is the `pause_stop` executor (decision 4): one role's stop for one pause. A row whose
// pause has ended finishes without acting, since the resume ended what it asked. Otherwise the
// claim's machine is asked to suspend for the pause (RequestSuspend, By released, the pause's id,
// no deadline), and the row ends the pause's stop row with what the claim answers:
//
//   - suspended: the claim is stopped. A stop the pause's suspension made has ended its stop row
//     already, through the stop transaction (projectPause.stopped); a stop row still open is ended
//     `taken_over`, since something else stopped the claim first (pauseStopAnswered);
//   - held for the agent's turn or its launch (supervise.ErrSuspendHeld): the row asks again after
//     pauseStopWait, or as soon as the stop transaction records the claim stopped and makes the
//     row due;
//   - refused as not registered: a queued claim runs nothing and has no launch under way, and the
//     pause holds the start row that would launch it, so its stop row ends `stopped` with no
//     launch owed; a launch whose outcome is uncertain asks again after the same wait, since the
//     boot's relaunch of it meets the pause's gate, whose stop ends the stop row through the stop
//     transaction;
//   - failed or retired, or no machine at all: the stop row ends `failed`.
func (r *outbox) pauseStop(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) error {
	if r.trees == nil {
		return errors.New("pause stop executor has no store")
	}
	held, err := r.trees.PauseByID(ctx, payload.Pause)
	if err != nil {
		return err
	}
	if held.Ended() {
		r.log.Info("outbox pause stop finished without acting: its pause has ended", "row", row.ID, "issue", row.Issue,
			"role", payload.Role, "pause", held.ID)
		return nil
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		return r.endPauseStop(ctx, row, held, token, store.PauseStopFailed, "the daemon supervises no claim of the role")
	}
	switch machine.Claim().State {
	case supervise.StateFailed, supervise.StateRetired:
		return r.endPauseStop(ctx, row, held, token, store.PauseStopFailed, "the claim has failed or retired")
	}
	err = machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByReleased, Pause: held.ID, Reason: payload.Reason})
	var refused *supervise.RefusedError
	switch {
	case err == nil:
		return r.pauseStopAnswered(ctx, row, held, machine.Claim())
	case errors.Is(err, supervise.ErrSuspendHeld):
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s is held for the claim's turn or its launch", held.ID, token))
	case errors.As(err, &refused) && refused.State == supervise.StateQueued:
		return r.endPauseStop(ctx, row, held, token, store.PauseStopStopped,
			"the claim is queued: no process runs, no launch is under way, and the pause holds the start that would launch it")
	case errors.As(err, &refused) && refused.State == supervise.StateLaunchUncertain:
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s waits for the relaunch of its uncertain launch, which the pause's gate stops", held.ID, token))
	default:
		return fmt.Errorf("stop claim %s for pause %d: %w", token, held.ID, err)
	}
}

// pauseStopAnswered ends the pause's stop row of a claim that answered its stop with nil, by what
// the claim holds now. A suspended claim's stop row is ended `taken_over` where it is still open:
// one this pause's own suspension stopped was ended by its stop transaction, so what is left open
// was stopped first by a park, a close, the operator, an earlier pause's late stop or an idle
// release — or by this pause's suspension whose write the machine still owes, whose stop
// transaction then records the stop in a row of its own (store.RecordPauseStoppedTx), with its
// hold, its owed launch or its backstop, when it lands.
func (r *outbox) pauseStopAnswered(ctx context.Context, row record.OutboxRow, held store.Pause, c supervise.Claim) error {
	switch {
	case c.State == supervise.StateFailed || c.State == supervise.StateRetired:
		return r.endPauseStop(ctx, row, held, c.Token, store.PauseStopFailed, "the claim has failed or retired")
	case c.State != supervise.StateSuspended:
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s was answered while the claim is %s", held.ID, c.Token, c.State))
	default:
		return r.endPauseStop(ctx, row, held, c.Token, store.PauseStopTakenOver,
			fmt.Sprintf("a suspension by %s stopped the claim first", c.SuspendedBy))
	}
}

// endPauseStop ends held's open stop of token as end, logging why where one was open: none is
// open once the stop transaction recorded the stop, or once a resume cancelled it.
func (r *outbox) endPauseStop(ctx context.Context, row record.OutboxRow, held store.Pause, token claim.Token, end store.PauseStopEnd, why string) error {
	ended, err := r.trees.EndPauseStop(ctx, held.ID, token, end, r.now().UTC())
	if err != nil {
		return err
	}
	if ended {
		r.log.Info("outbox pause stop ended its stop row", "row", row.ID, "issue", row.Issue, "claim", string(token),
			"pause", held.ID, "end", string(end), "why", why)
	}
	return nil
}
