package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ControllerStartMessage is the controller's first prompt at every start, so every start and
// restart runs the skill's start procedure with nothing typed, where the plugin alone would leave
// the session idle until a wake. `legion controller start` carries it to the operator's Oh My Pi
// as LEGION_CONTROLLER_START_MESSAGE, and pi-legion's ControllerStartCommand claims the controller
// and sends it as the session's first turn, ahead of any Envoy card or Legion notice (LEGION-392:
// that command, which runs after Oh My Pi's startup, sends it once the controller is registered,
// right before the controller role's Envoy claim and the live Envoy subscription a wake could
// arrive on). The daemon delivers it to its own controller (`controller: daemon`) as an RPC prompt
// each time that controller reports ready, after its claims.
const ControllerStartMessage = "Legion controller start: follow skill://legion-controller's start procedure now (\"What happened before you started\"), then end the turn."

// ControllerStartCommand is the positional word `legion controller start` passes to the operator's
// Oh My Pi, which runs command-line words as prompts once its startup is over and its TUI listens.
// pi-legion's extension command of that name claims the controller and sends
// ControllerStartMessage as the session's first turn. A claim from pi-legion's session_start
// handler instead would run inside Oh My Pi's startup, which drains every send made there at its
// end: the start turn would run inside the startup wait, with no TUI and no input, and a card on
// the controller role's lane could start its own turn first.
const ControllerStartCommand = "/legion-controller-start"

// The waits before a failed or retired controller is retried: the first (plan.controllerRetry,
// controllerRetryFirst unless a test replaces it), then doubled at each retry that fails again, up
// to the last. A controller that reaches ready resets them.
const (
	controllerRetryFirst = time.Minute
	controllerRetryMax   = 30 * time.Minute
)

// controllerKeeper keeps the daemon's own controller running (`controller: daemon`). The controller
// is one claim, on the controller role with no issue, which its machine supervises as any claim's:
// a death relaunches the same session, within the launch failure budget, the launch after the
// session's second end in a row with no finished turn between starts a fresh session (supervise's
// SessionRun, dispatch://INFRA-2045 R1), and a daemon-driven relaunch of a session older than
// `controller_session_max_age_hours` starts a fresh one too (supervise's replaceAgedSession, R3).
// What the machine leaves to its caller is the keeper's: the claim is created when the store holds
// none, launched while queued, resumed when suspended, and, when its budget ran out (failed) or its
// agent ended it (retired), retried with fresh budgets after a wait that doubles while it keeps
// failing. A controller held for OpenAI's cyber-policy refusal is the one failed controller it never
// retries (supervise.Claim.KeeperRetries): an operator's `legion claims retry` brings it back. At
// each ready it hands the controller the start message. At each look of a working controller it
// ends a turn open longer than turnBound (TurnPastBound, R2). It stops no live controller for any
// other reason. The liveness sweep (watchController) says when the controller is not registered,
// from a loop of its own.
type controllerKeeper struct {
	supervisor *supervisor
	// project is the project token the claim is filed under.
	project string
	// ctx bounds each decision the keeper hands a machine: supervision's lifetime, as a machine's
	// own work is bounded. It carries no hold mark: the keeper is reached from its own loop (keep)
	// and from the ready route (ready), so each call that reaches the pool takes a fresh one.
	ctx context.Context
	log *slog.Logger
	now func() time.Time
	// retryFirst is the first wait before a failed or retired controller is retried.
	retryFirst time.Duration
	// retryAt is when a failed or retired controller is next retried, zero while none is waiting,
	// and backoff the wait that set it.
	retryAt time.Time
	backoff time.Duration
	// heldLogged is the line keep last wrote for a retry the hold refused, and heldReason that
	// refusal, for the episode the refused retry holds the claim in (keep, held): the line is
	// written once per episode and cause, and the reason rides each later retry's Warn. Both are
	// cleared at one point, leaveHeld, at each way out of the episode.
	heldLogged, heldReason string
	// turnBound is `controller_turn_bound_minutes`, 0 for the rule off
	// (config.Config.ControllerTurnBound).
	turnBound time.Duration
}

// newControllerKeeper is the keeper of project's controller, with the wait before its first retry
// and the bound of its turn rule (turnBound, 0 for the rule off), so no keeper is built with the
// rule silently off.
func newControllerKeeper(ctx context.Context, sup *supervisor, project string, retryFirst, turnBound time.Duration, log *slog.Logger) *controllerKeeper {
	return &controllerKeeper{supervisor: sup, project: project, ctx: ctx, log: log, now: time.Now, retryFirst: retryFirst,
		turnBound: turnBound}
}

func (k *controllerKeeper) token() claim.Token { return claim.ControllerToken(k.project) }

// run keeps the controller at once and then every interval until ctx is done.
func (k *controllerKeeper) run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := k.keep(); err != nil && ctx.Err() == nil {
			k.log.Error("controller: keep the daemon's controller running", "claim", k.token(), "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// keep is one look at the controller's claim, and the one request its state calls for.
func (k *controllerKeeper) keep() error {
	// Each look is a fresh call on k.ctx, beside the ready route, so it takes a hold mark of its
	// own, which its create and its request share as they run one after another.
	ctx := store.ForConcurrentWorker(k.ctx)
	token := k.token()
	m, ok := k.supervisor.Machine(token)
	if !ok {
		created, _, err := k.supervisor.Create(ctx, supervise.Claim{
			Token: token, Project: k.project, Role: claim.RoleController, State: supervise.StateQueued,
		}, "")
		if err != nil {
			return fmt.Errorf("create the controller's claim: %w", err)
		}
		k.log.Info("controller: the daemon launches this project's controller", "claim", token)
		m = created
	}
	var request supervise.Event
	c := m.Claim()
	state := c.State
	if state != supervise.StateFailed && state != supervise.StateRetired {
		// The claim left the state a refused retry holds it in, whatever it moved to.
		k.leaveHeld()
	}
	switch state {
	case supervise.StateQueued:
		// No request of this keeper marks itself exempt from the wake pacing, this spawn
		// included: the machine exempts the controller's claim by identity, which covers the
		// launches no request raises as well (supervise's reserveWake, claim.IsController). The
		// controller is the one session the operator's own read of a running wave relies on, so
		// it comes up ahead of the queue it is reading: holding it behind a wave of workers
		// would leave nobody able to say what that wave is doing.
		request = supervise.RequestSpawn{Claim: token}
	case supervise.StateSuspended:
		// The daemon keeps its controller running by rule, which holds an operator's suspension:
		// only the operator's resume runs a controller the operator suspended.
		request = supervise.RequestRuleResume{Claim: token, Reason: "the daemon keeps its controller running"}
	case supervise.StateFailed, supervise.StateRetired:
		if !c.KeeperRetries() {
			// No retry, at any wait, and the wait starts over once the controller runs again
			// (cyberPolicyHeldLine).
			k.retryAt, k.backoff = time.Time{}, 0
			k.held(slog.LevelError, cyberPolicyHeldLine, token, c.CyberPolicyRefusal(),
				"state", state, "cause", string(c.FailureCause), "failure", c.Failure)
			return nil
		}
		now := k.now()
		if k.retryAt.IsZero() {
			k.backoff = k.retryFirst
			k.retryAt = now.Add(k.backoff)
			k.log.Error("controller: the daemon's controller stopped; retrying it with fresh budgets", "claim", token,
				"state", state, "retryAt", k.retryAt)
			return nil
		}
		if now.Before(k.retryAt) {
			return nil
		}
		k.backoff = min(2*k.backoff, controllerRetryMax)
		k.retryAt = now.Add(k.backoff)
		retrying := []any{"claim", token, "state", state, "nextRetryAt", k.retryAt}
		if k.heldReason != "" {
			retrying = append(retrying, "reason", k.heldReason)
		}
		k.log.Warn("controller: retrying the daemon's controller", retrying...)
		request = supervise.RequestRetry{Claim: token}
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		k.retryAt, k.backoff = time.Time{}, 0
		return k.mind(ctx, m)
	default:
		// Launching, connected, registered, or a launch the previous daemon left uncertain: the
		// machine and boot's reconciliation own those.
		return nil
	}
	err := m.Handle(ctx, request)
	// A retry resumes the recorded session (R5 of dispatch://INFRA-4508): the run of refused turns
	// no longer starts a new one, and the machine starts one only after the session's launches keep
	// ending (supervise's SessionRun, dispatch://INFRA-2045 R1) or when the session is older than
	// its age (supervise's replaceAgedSession, R3: the keeper's retry is no operator's, so it is not
	// exempt from that rule). The keeper asks for a new session in a retry in one case alone: the
	// resume is refused because one of the recorded session's names is held for another
	// claim, or for none (supervise.IsSessionHeldElsewhere, claim.SessionHeldRefusal). Such a resume
	// would be refused at every retry, and nobody else retries this claim, so the keeper asks the
	// retry again in a new session, as an operator's `legion claims retry --fresh-session` does,
	// and the answer to that is the one judged below: the hold's wait on the role, a lookup the
	// listener refuses, or the retry admitted. The resume's other refusals — a session name the
	// daemon cannot hold, a claim at the cap of held names — are answered as before: no new session
	// is asked for.
	if retry, ok := request.(supervise.RequestRetry); ok && !retry.FreshSession && supervise.IsSessionHeldElsewhere(err) {
		request = supervise.RequestRetry{Claim: token, FreshSession: true}
		err = m.Handle(ctx, request)
	}
	// A retry in a new session the hold refuses is no fault of the keeper's, and its own backoff
	// asks again, so it returns nothing its loop logs at Error; the keeper says why once per
	// episode, at the level the cause asks for. A role held under the recorded session by another
	// claim's live process waits for an operator's suspend of that claim, which the reason names,
	// with the bound: Warn (supervise.ErrForgetWaitsForRole). A lookup the listener refuses the
	// same way every time is a fault no suspend fixes: Error, with the listener's status
	// (notify.RoleLookupRefused). Each later retry's Warn carries the reason. Any other answer, an
	// admitted request among them, ends the episode.
	var refusedLookup *notify.RoleLookupRefused
	switch {
	case err != nil && errors.Is(err, supervise.ErrForgetWaitsForRole):
		// The wait covers both of forgetWait's causes, a role held under the recorded session and
		// a lookup the listener did not answer; the reason says which (review round 16 of #4220,
		// #63).
		k.held(slog.LevelWarn, "controller: the retry of the daemon's controller waits for the Envoy listener to free its role, or to answer who holds it", token, err)
		return nil
	case err != nil && errors.As(err, &refusedLookup):
		k.held(slog.LevelError, "controller: the retry of the daemon's controller is refused: the Envoy listener refuses the lookup of its role's holder", token, err,
			"status", refusedLookup.Status)
		return nil
	}
	k.leaveHeld()
	if err != nil {
		return fmt.Errorf("%T for the controller's claim: %w", request, err)
	}
	return nil
}

// cyberPolicyHeldLine is the keeper's one line for a controller held for OpenAI's cyber-policy
// refusal (supervise.Claim.KeeperRetries), which it retries at no wait: its retry would be one the
// daemon makes by itself, which the machine refuses (supervise.ErrHeldForCyberPolicy), since the
// start message the keeper sends at the relaunch's ready is input from outside the session, which
// ends Oh My Pi's own hold, so the refused content would go to OpenAI again and add a refusal to
// the organization's record. The line is written once per episode (held, leaveHeld) at Error, and
// its reason is the machine's refusal (supervise.Claim.CyberPolicyRefusal), which names the
// operator's `legion claims retry`: the machine admits that retry for such a controller, in a new
// session.
const cyberPolicyHeldLine = "controller: the daemon's controller is held for OpenAI's cyber_policy refusal of its session's content; the keeper does not retry it"

// held records a retry the hold refused (keep): its reason, for each later retry's Warn, and one
// line at level, written once per episode and cause.
func (k *controllerKeeper) held(level slog.Level, msg string, token claim.Token, err error, args ...any) {
	k.heldReason = err.Error()
	if k.heldLogged == msg {
		return
	}
	k.heldLogged = msg
	k.log.Log(k.ctx, level, msg, append([]any{"claim", token, "reason", err}, args...)...)
}

// leaveHeld ends a held episode (keep, held): the one reset of what a refused retry recorded,
// called at each way out of it — a request admitted, a refusal of another cause, and the claim in
// any state but failed or retired, running or not — so the next refused retry is written again,
// and no later retry's Warn names a reason that is no longer the claim's.
func (k *controllerKeeper) leaveHeld() { k.heldLogged, k.heldReason = "", "" }

// ready hands the controller the start message once its agent says it is ready, at every launch, so
// each one runs the skill's start procedure before any wake. A start message still pending from an
// earlier launch — its turn interrupted by the death this launch replaced — is sent on this ready
// instead, so none is queued behind it. The message is the daemon's own and resumes nothing
// (RequestDeliver.ResumesNothing): a claim an operator suspended at or after this ready — between
// the ready and the message, or while it was launching, the suspension held until this ready and
// ended there — refuses it and stays suspended, and the next launch's ready sends it again.
func (k *controllerKeeper) ready(c supervise.Claim) {
	m, ok := k.supervisor.Machine(c.Token)
	if !ok {
		return
	}
	// The ready route calls this on its own goroutine, beside the keeper's loop, so the decision
	// takes a fresh hold mark at the call.
	err := m.Handle(store.ForConcurrentWorker(k.ctx), supervise.RequestDeliver{Claim: c.Token, Task: ControllerStartMessage, ResumesNothing: true})
	var refused *supervise.RefusedError
	switch {
	case err == nil:
	case errors.Is(err, supervise.ErrDeliveryPending):
		k.log.Info("controller: the start message waits behind the controller's pending delivery", "claim", c.Token)
	case errors.As(err, &refused):
		k.log.Info(startMessageRefusedLine, "claim", c.Token, "reason", err)
	default:
		k.log.Error("controller: hand the daemon's controller its start message", "claim", c.Token, "error", err)
	}
}

// startMessageRefusedLine is the keeper's Info line for a start message the machine refused with a
// *RefusedError (ready): a controller that left its ready before the message — suspended, by an
// operator at or after this ready or by anyone else (supervise.RequestDeliver.ResumesNothing), or
// failed, retired or launch-uncertain, each of which ignores a delivery. The next launch's ready
// sends the message again.
const startMessageRefusedLine = "controller: the start message is not sent: the controller left its ready before it"

// stopLaunchedController ends the daemon's own controller on a daemon that leaves the controller to
// its operator (`controller: operator`). A claim an earlier boot under `controller: daemon` stored
// would otherwise be re-adopted and relaunched at every death, its pod contending with the
// operator's `legion controller start` for the one controller record, which the register route
// refuses it on this daemon. First it stops the claim, unless it is retired already: the stop
// releases its process (its Sandbox, under the Sandbox runtime) and retires it before anything
// relaunches it. Then it ends the claim's registration (endLaunchedRegistration), for a claim
// retired before this boot too. The stop comes first: a stop that fails refuses the boot with the
// record untouched, so a daemon switched to `controller: daemon` again re-adopts a controller whose
// registration, grants and wakes still work, where a record ended ahead of a pod that kept running
// would leave that pod unable to act until it died. Each step reads the state it repairs at every
// boot, so a boot that fails partway leaves nothing the next one does not finish: a registration
// ending that fails after the stop is retried, since the retired claim still names its session.
// Either failure refuses the boot, naming the claim, rather than leave this daemon supervising a
// controller it does not run. When it returns nil the controller's claim is absent or retired.
func (s *supervision) stopLaunchedController(ctx context.Context) error {
	token := claim.ControllerToken(s.plan.project)
	m, ok := s.supervisor.Machine(token)
	if !ok {
		return nil
	}
	if state := m.Claim().State; state != supervise.StateRetired {
		s.log.Warn("controller: stopping the controller an earlier boot under controller: daemon launched; this daemon leaves the controller to its operator",
			"claim", token, "state", state)
		if err := m.Handle(ctx, supervise.RequestStop{Claim: token, By: supervise.SuspendedByOperator}); err != nil {
			return fmt.Errorf("stop %s, the controller an earlier boot under controller: daemon launched, since this daemon leaves the controller to its operator (controller: operator): %w", token, err)
		}
	}
	return s.endLaunchedRegistration(ctx, m.Claim())
}

// endLaunchedRegistration mints the controller record controller.UnheldCapability while the record
// still names c's session, the stopped controller's registration: the state's controllerLocator
// then names no dead pod's session, no grant mints for it, and no capability a launch registered
// under survives the switch. A record naming any other session, the operator's controller's after a
// `legion controller start`, or none, is left as it is. A record with no registration is left as it
// is even though a claim whose agent never registered has no session either: that record can hold
// the capability `legion controller start` minted for an
// operator's controller that has not registered yet, and matching the two empty sessions would
// replace it, so that controller's registration would then be refused. The guard on the record's
// registration is correct only beside the session comparison, in one condition: alone it would let
// a claim with no session through against a record naming one, which the comparison refuses, and
// alone the comparison would let the two empty sessions through, which the guard refuses. It runs
// at boot, before the API serves, so no registration can come between the read and the mint.
func (s *supervision) endLaunchedRegistration(ctx context.Context, c supervise.Claim) error {
	record, found, err := s.supervisor.store.Controller(ctx, s.plan.project)
	if err != nil {
		return fmt.Errorf("read the controller record to end the registration of %s, the controller an earlier boot under controller: daemon launched: %w", c.Token, err)
	}
	if !found || !record.Registered() || record.Session != c.Session {
		return nil
	}
	s.log.Warn("controller: ending the registration of the controller an earlier boot under controller: daemon launched; this daemon leaves the controller to its operator",
		"claim", c.Token, "session", c.Session)
	if _, err := s.supervisor.store.MintController(ctx, s.plan.project, controller.UnheldCapability()); err != nil {
		return fmt.Errorf("end the registration of %s, the controller an earlier boot under controller: daemon launched, since this daemon leaves the controller to its operator (controller: operator): %w", c.Token, err)
	}
	return nil
}

// controllerNotRegistered is the liveness sweep's line while no live controller holds the record:
// the whole line under `controller: operator`, and its leading phrase under `controller: daemon`,
// so one log query on it counts either mode.
const controllerNotRegistered = "controller not registered; run legion controller start"

// watchController is the daemon's liveness sweep over the project's controller, under either
// runtime and either `controller` mode: every sweep interval it reads the project's controller
// record, and when no session holds it, or the Envoy role registry says the session is gone, it says
// so at most once per worker boot timeout, naming the remedy for its mode: `legion controller
// start` for the operator's controller, the keeper's relaunch for the daemon's, or the operator's
// retry for a daemon's controller its keeper does not retry (launchedControllerLine), with the
// state of its claim as the store holds it, so a launch in flight or a backoff reads apart from a
// death. It reads only the store and the role registry, never the controller's machine, whose lock
// a launch holds for as long as the launch takes, so the keeper's loop and the controller's
// launches never hold it up. The Prober logs why each Gone or Unknown verdict was reached; Unknown
// is never a death verdict, so it says nothing more. Every line the sweep and its Prober write
// names the mode, and both modes write the same lines on the same cadence.
func watchController(ctx context.Context, st *store.Store, cfg config.Config, p plan, log *slog.Logger) {
	launched := cfg.ControllerLaunch == config.ControllerLaunchDaemon
	log = log.With("mode", string(cfg.ControllerLaunch))
	prober := controller.NewProber(controller.ProberOptions{
		EnvoyURL: cfg.EnvoyURL, EnvoyToken: p.secrets["ENVOY_TOKEN"], Project: p.project, Log: log,
	})
	ticker := time.NewTicker(p.orphanSweep)
	defer ticker.Stop()
	var logged time.Time
	for {
		record, _, err := st.Controller(ctx, p.project)
		liveness := controller.Gone
		switch {
		case err != nil:
			liveness = controller.Unknown
			if ctx.Err() == nil {
				log.Warn("controller: read its record", "error", err)
			}
		case record.Registered():
			liveness = prober.Probe(ctx, record.Session)
		}
		switch liveness {
		case controller.Live:
			logged = time.Time{}
		case controller.Gone:
			if logged.IsZero() || time.Since(logged) >= cfg.WorkerBootTimeout {
				if launched {
					held, err := launchedController(ctx, st, p.project)
					attrs := []any{"project", cfg.Project, "registered", record.Registered(), "claimState", held.State}
					if err != nil {
						attrs = append(attrs, "claimError", err.Error())
					}
					log.Warn(launchedControllerLine(held), attrs...)
				} else {
					log.Warn(controllerNotRegistered, "project", cfg.Project, "registered", record.Registered())
				}
				logged = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// launchedControllerLine is the liveness sweep's line for the daemon's own controller while no
// live session holds the record (watchController), given its claim as the store holds it, the
// zero claim before the daemon has created one. It names the remedy: the keeper's relaunch of the
// controller, which the sweep's reader waits for, or, for a controller the keeper retries no more
// (supervise.Claim.KeeperRetries, a controller held for OpenAI's cyber-policy refusal), the
// operator's retry (supervise.RetryCommand), since the keeper writes its own line once per
// process and this one recurs.
func launchedControllerLine(c supervise.Claim) string {
	if c.Role == claim.RoleController && !c.KeeperRetries() {
		return controllerNotRegistered + " only under controller: operator; this daemon's controller is held for OpenAI's cyber_policy refusal of its session's content, which its keeper does not retry: run " +
			supervise.RetryCommand(c.Token)
	}
	return controllerNotRegistered + " only under controller: operator; this daemon launches its own controller and relaunches it"
}

// launchedController is the daemon's own controller's claim as the store holds it, the zero claim
// before the daemon has created one.
func launchedController(ctx context.Context, st *store.Store, project string) (supervise.Claim, error) {
	claims, err := st.Claims(ctx)
	if err != nil {
		return supervise.Claim{}, err
	}
	token := claim.ControllerToken(project)
	for _, c := range claims {
		if c.Token == token {
			return c, nil
		}
	}
	return supervise.Claim{}, nil
}

// claimReadyHook is the ready route's hook: the daemon's controller's ready goes to its keeper, and
// every other claim's to the workflow's (nil for a daemon with no workflow).
func claimReadyHook(keeper *controllerKeeper, workflow func(supervise.Claim)) func(supervise.Claim) {
	if keeper == nil {
		return workflow
	}
	return func(c supervise.Claim) {
		if c.Role == claim.RoleController {
			keeper.ready(c)
			return
		}
		if workflow != nil {
			workflow(c)
		}
	}
}
