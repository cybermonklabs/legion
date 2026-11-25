package daemon

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// refreshStalePlugins posts StalePlugin to every live claim whose recorded plugin contract is not
// this daemon's — an earlier daemon's, or none at all, at the first boot on this release, or one a
// prior relaunch attempt the tree lifecycle or the store refused left stale (launch returns such a
// refusal before the claim's state, locator, or contract change). Trigger says which pass is
// posting: "boot", once over the restored claims before any of them is relaunched
// (relaunchUnfinished runs after this); "sweep", at every orphan sweep's own cadence, over the
// claims it already reads each tick, so a relaunch the boot pass could not complete is retried
// without waiting for the next restart. refreshPlugin's own re-check makes a re-post for a claim a
// prior attempt already moved on — relaunched, or registered current — a no-op. A claim still
// launching is left to its own registration, which decides the comparison the moment it arrives,
// so only the four states whose process has registered are read.
//
// A claim whose launch installs no plugin is never posted, whatever contract it records and
// round or no round (noPluginInstall). A relaunch is the lane's whole act, and for such a claim
// the relaunch cannot work: a Claude Code role's plugin under Kubernetes is the worker image's
// own claude-envoy, which no install reaches, so the launch keeps the pod the claim already runs
// in, the process comes back on that same plugin, registers the same contract again, and the next
// registration is the second relaunch in a row — which staleRegistration would charge, until the
// launch budget fails the claim. The contract decides nothing: a claim at this release's own
// contract is as unreachable by an install as one below it, and the next bump would walk exactly
// that claim to failed. Nothing here can move such a worker's plugin; a new image can, which is
// what the pod round replaces the pod for. So it is left running and is asked its round like any
// other worker, on the Envoy topic of its claim (supervision.openPodUpdate). staleRegistration
// accepts such a registration and charges it nothing, for the same reason
// (supervise.Deps.NoPluginInstall).
//
// A claim in a pod round is passed over and a claim in a plugin round is not. That term is the
// lane's own rather than this pass's: staleForTheLane holds it and says what each kind of round
// reads as and why, because the count of what the lane has left must name the claims this pass
// posts and no others (claimsToRefresh).
//
// A daemon that cannot relaunch onto a plugin (PluginRelaunchable false, LEGION-164's degraded
// Kubernetes mode) posts nothing; it logs one Error line per stale claim at boot only — a sweep's
// finding would only repeat a line the boot already wrote, since nothing relaunchable has changed
// in between.
//
// Every claim it does post takes a credit of the refresh gate but the two named below, and a
// claim the gate refuses is not posted at all (supervise.RefreshGate, dispatch://INFRA-2712
// R1). That is what holds the lane to one minute of the daemon's wake pacing: the gate grants K
// credits, K the rate, and the next sweep — a minute later, by which time this sweep's
// admissions have had their turns — posts the next K. Nothing of the refresh is lost by a
// refusal, since this pass re-reads every stale claim at every sweep, which is why the refused
// claim is passed over in silence rather than remembered anywhere. The admission is taken
// before the post, never after the machine has decided, so a refresh in flight is counted: the
// gate counts every credit it has granted and not had back (supervise.RefreshGate, its rule of
// return). The pass gives back the credit of a post the supervisor drops, because no machine ever
// decides that post (supervisor.post). Before any of this the gate is corrected: it takes back
// each credit, granted before its previous correction, that the machine holding it no longer
// holds for a refresh wake of its own, which is the backstop for a credit lost to a path
// that gives none back, and then sets its count from the refresh wakes the machines really hold
// — the lane's own kind, the one kind only this lane parks (supervise.WakePluginRefresh,
// parkedWakes) — which raises the count when the lane holds wakes no admission is behind, and
// otherwise leaves it at the credits out. The boot pass corrects an empty gate, which is what a
// restart leaves: no machine of this daemon holds a wake yet, so the reading is zero and the
// fleet's refresh starts with every credit free (supervise.RefreshGate.Correct).
//
// A claim whose machine already holds a waiting wake is passed over before the gate is asked at
// all, read from the same snapshot the correction counts (parkedWakes). The machine decides such
// a claim nothing — refreshPlugin returns over a claim that already holds a wake of any kind, and
// gives back whatever credit the post carried — so asking for one here would spend an admission
// on a claim that raises no relaunch, and spend it for most of the pass, since the machine's
// release lands only when its own goroutine decides the post. At a fleet's worth of stale claims that is
// the lane admitting the K claims it already has waiting and none of the ones it has not, which
// is the drain this change exists to shorten standing still for a sweep.
//
// A claim in a turn is posted with no credit at all, for that same reason read at the other
// end: its relaunch takes no wake now. The machine holds it for the turn's end
// (holdStaleRelaunch), and the relaunch that end runs asks the gate for a credit of its own
// (supervise's refreshPlugin), which is the design requirement R2 states — "A claim in a turn
// takes no admission; its turn's end asks the gate, so it is relaunched then if a credit is
// free and at the next sweep otherwise" (afterTurn).
// Admitted here instead, such a claim would spend one of the K on a relaunch that raises no
// wake, and spend it until the machine's release landed, which is after this pass has walked
// every claim; and a turn is minutes while the sweep is one, so the same claim would spend one
// at every tick of its turn. The tail is where that bites hardest: a refreshed idle claim
// leaves the stale set while a working one stays in it, so the set grows richer in claims in a
// turn as the wave drains, and the last idle stragglers would wait sweeps behind credits that
// raised nothing.
//
// Every term of the lane is read of each claim as its machine last published it
// (supervise.Machine.Published), not as claims holds it: claims says which claims to walk. A
// sweep reads them before its orphan reconciliation, which under Kubernetes lists every Sandbox,
// so by the time this pass walks them a claim can have been suspended, failed or retired, had its
// relaunch start, started a turn, or registered this daemon's contract. A credit posted to such a
// claim comes back only as its machine decides the post, which a machine deciding something else
// first holds until that decision ends — a relaunch waits minutes for its pod — and until then the
// gate counts it out and this pass refuses every claim after it: the lane spends the credit on
// nothing for a sweep or more (dispatch://INFRA-3904). The published claim is the one the machine
// decides the post against unless a decision lands in between, so what is left of that window is
// one decision long rather than a reconciliation long.
//
// The skip of a claim in a turn is as imperfect as the parked skip above, and costs what that one
// costs. The machine decides the post as the claim stands when the post reaches it: a claim read
// idle can have started a turn by then, and one read working can have ended it. The first is
// posted a credit the machine gives straight back (refreshPlugin, endRefreshLane) and is
// relaunched at that turn's end or at a later sweep; the second is posted none, and its machine
// asks the gate for a credit of its own as it decides the post (refreshPlugin's guard), so it is
// relaunched then if a credit is free and at the next sweep otherwise.
//
// One line is written for the refusals of a pass, naming how many claims are left for a later
// sweep, rather than one per claim: a boot over a fleet of several hundred refuses hundreds at
// every tick of the sweep, and a line each would bury the log the operator reads the wave from.
// A claim in a turn is in none of that count: nothing refused it, and it is left to no later
// sweep of this pass but to the end of its own turn.
//
// A claim whose read of whether its launch installs a plugin fails is decided nothing by this
// pass: no relaunch is posted and no line says it is stale, since nothing established either.
// One Warn line names the claim and the read's error, and the next sweep reads it again. Each
// read is bounded on its own (stalePluginReadBound), whatever the pass's context allows. A read
// that fails because ctx ended, the daemon stopping, says nothing of the store: the pass writes
// no line for it and ends there, since no sweep follows a stop.
//
// The project controller's claim is posted whatever the gate holds, and takes no credit: its
// relaunch is exempt from the pacing altogether (supervise's reserveWake, claim.IsController), so
// it holds none of what the gate counts, and it is the one session an operator's own reading of a
// running wave relies on. It is posted mid-turn as well, since its turn is never waited out
// (refreshPlugin).
func (s *supervision) refreshStalePlugins(ctx context.Context, claims []supervise.Claim, trigger string) {
	noInstall := func(ctx context.Context, c supervise.Claim) (bool, error) {
		read, end := context.WithTimeout(ctx, stalePluginReadBound)
		defer end()
		return noPluginInstall(read, s.cfg, s.backends, c)
	}
	parked, refreshes := s.parkedWakes()
	s.refreshGate.Correct(refreshes)
	refused := 0
	left := 0
	for i, c := range claims {
		if ctx.Err() != nil {
			left = len(claims) - i
			break
		}
		if m, supervised := s.supervisor.Machine(c.Token); supervised {
			c = m.Published()
		}
		stale, err := staleForTheLane(ctx, c, noInstall)
		if err != nil {
			if ctx.Err() != nil {
				left = len(claims) - i
				break
			}
			s.log.Warn("supervise: the stale-plugin pass could not read whether this claim's launch installs a plugin, so it decides nothing for the claim; the next sweep reads it again",
				"claim", c.Token, "trigger", trigger, "pluginContract", c.PluginContract, "daemonApiVersion", api.DaemonAPIVersion, "error", err)
			continue
		}
		if !stale {
			continue
		}
		if !s.plan.pluginRelaunchable {
			if trigger == "boot" {
				s.log.Error(supervise.NoPluginSourceMessage,
					"claim", c.Token, "incarnation", c.Locator.Incarnation, "pluginContract", c.PluginContract, "daemonApiVersion", api.DaemonAPIVersion)
			}
			continue
		}
		if parked[c.Token] {
			continue
		}
		// The two posts that take no credit, each for the reason above it: the controller's
		// claim, whose relaunch is outside the pacing the gate bounds, and a claim in a turn,
		// whose relaunch takes no wake until that turn's end, where it asks the gate itself.
		if claim.IsController(c.Role, c.Issue, c.Tree) || c.State == supervise.StateWorking {
			s.supervisor.post(c.Token, supervise.StalePlugin{Claim: c.Token, Trigger: trigger})
			continue
		}
		credit := s.refreshGate.Admit()
		if credit == nil {
			refused++
			continue
		}
		if s.afterRefreshAdmit != nil {
			s.afterRefreshAdmit(c.Token, credit)
		}
		if !s.supervisor.post(c.Token, supervise.StalePlugin{Claim: c.Token, Trigger: trigger, Credit: credit}) {
			credit.Release()
		}
	}
	// A pass whose own bound ended (bootStalePlugins) says how many claims it left unread; a pass
	// the daemon's stop ended says nothing, since no sweep follows a stop.
	if left > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.log.Warn("supervise: the stale-plugin pass's bound ended before it read every claim; the next sweep reads the rest",
			"claims", left, "trigger", trigger)
	}
	if refused > 0 {
		s.log.Info("supervise: the stale-plugin refresh gate is full; these claims are left to a later sweep",
			"claims", refused, "trigger", trigger)
	}
}

// staleForTheLane is the stale-plugin lane's own stale claim, in the five terms its two readers
// read alike: a recorded session, a process that holds a capability, a recorded plugin contract
// that is not this daemon's, no pod update round, and a launch that installs a plugin at all.
// The readers are the pass that posts the lane's relaunches (refreshStalePlugins) and the count
// of what the lane still has to do (claimsToRefresh), and they are one predicate because a term
// added to one of them alone makes the count name work the lane never does or hide work it will
// — which is the one thing the count exists not to do (dispatch://INFRA-2712 R4).
//
// A claim in a pod round is out of the set, whatever contract it records (InPodRound). That
// round is carrying it, and the process is replaced when the round ends (reconcilePodUpdates),
// so a StalePlugin post would relaunch mid-turn a claim the round is waiting on and take the
// pod's replacement its answer; the count says the same, because a claim the pass will not
// post is not work the lane has left.
//
// A claim in a plugin round is in the set, and the term is by round kind for that reason. The
// boot's own de-round pass has posted such a claim its end a moment before the refresh pass
// runs (endPluginRounds), and one machine's inbox is FIFO, so that end is decided ahead of the
// post and the machine sees a claim in no round at all. What that pass reads is each claim as its
// machine last published it, which still shows the round until the machine has decided that end,
// and the count reads the claims the store holds, where the round stands until that end's own
// write. So a term of "in any round" would pass over exactly the claims the de-round pass has just
// freed and leave each of them a sweep — a whole minute, and a credit of the gate each — behind
// the rest of the fleet, and would have the count hide that work at the one moment it exists to
// be read: a boot over a fleet an earlier daemon left mid-round. A claim whose plugin round is
// somehow still standing when its machine takes the post is decided nothing wrong by it either:
// refreshPlugin relaunches it, and no plugin round holds a relaunch back, since the ask that did
// was deleted with the round. A claim that left its round at a registration speaking this
// daemon's contract (supervise's endCurrentPluginRound) is no longer stale at all, and the
// contract term above passes over it.
//
// noPluginInstall nil is every launch installing a plugin, which only a source a test builds by
// hand holds: serve gives the state's source the rule over this deployment's configuration and
// the lane's own pass builds a closure of it at every sweep, so no daemon reads this term as
// nil, a tmux daemon included (noPluginInstall, serve, refreshStalePlugins).
//
// A read of whether c's launch installs a plugin that fails is the error, and the claim is then
// neither in the set nor out of it: each reader decides nothing of it and says why, the pass in a
// line of its own and the count by ending the state's read with the error.
func staleForTheLane(ctx context.Context, c supervise.Claim, noPluginInstall func(context.Context, supervise.Claim) (bool, error)) (bool, error) {
	if c.Session == "" || c.PluginContract == api.DaemonAPIVersion || !supervise.HoldsCapability(c.State) || c.InPodRound() {
		return false, nil
	}
	if noPluginInstall == nil {
		return true, nil
	}
	none, err := noPluginInstall(ctx, c)
	if err != nil {
		return false, err
	}
	return !none, nil
}

// claimsToRefresh is how many of claims the stale-plugin lane still has to relaunch onto this
// daemon's plugin, which the state serves as `claimsToRefresh` (api.State.ClaimsToRefresh,
// contract 73): a reader of a running wave can then tell what the lane has left from what it
// has already admitted into the pacing, which `wakesWaiting` alone cannot say now that the gate
// holds no more than a minute of that pacing at a time (dispatch://INFRA-2712 R4).
//
// The set is the one refreshStalePlugins walks, read through that pass's own predicate and the
// whole of it (staleForTheLane): the round term is in there with the other four, so no claim
// the pass would post is left out of this count and none it passes over is named by it. It is
// computed here, where the lane's own rule lives, rather than in internal/api, which does not
// own that rule and which cuts the body to its caller only after it is built (api's stateFor).
//
// A claim in a turn is counted, for the same reason the pass posts it: the lane has it left to
// refresh, its relaunch is raised at that turn's end (supervise's holdStaleRelaunch, afterTurn),
// and a count that left it out would read as a drained lane over a fleet still working.
//
// Whether this daemon can relaunch onto a plugin at all is deliberately not a term of it
// (plan.pluginRelaunchable). On a daemon that cannot, the pass posts nothing and writes one
// Error line per stale claim at boot, and this count is what that gap costs, standing at the
// claims the lane cannot act on beside those lines. A count zeroed there would read as a lane
// with nothing left, which is the one thing such a daemon is not.
//
// A claim whose read of the plan fails is the error, and no count: State runs inside its
// snapshot's transaction, which a failure of the server has already ended, so the read ends there
// with the error that ended it rather than at the next read of the snapshot, which would fail on
// the aborted transaction and hide the cause.
func claimsToRefresh(ctx context.Context, claims []supervise.Claim, noPluginInstall func(context.Context, supervise.Claim) (bool, error)) (int, error) {
	left := 0
	for _, c := range claims {
		stale, err := staleForTheLane(ctx, c, noPluginInstall)
		if err != nil {
			return 0, err
		}
		if stale {
			left++
		}
	}
	return left, nil
}

// parkedWakes is every claim of this daemon whose machine holds a waiting wake of any kind,
// with the refreshes of that same reading counted beside it: the two answers the refresh pass
// needs come from one walk of the published views, so the claims it passes over and the count
// it puts the gate right from are of one instant rather than of two (waitingWakes; the pass
// makes the gate's own correction with this count, supervise.RefreshGate.Correct).
//
// Every kind is in the set, not the lane's own: what the pass reads it for is whether the machine
// would decide this claim anything, and refreshPlugin decides a claim that holds a wake of any
// kind nothing at all. The count beside it is the lane's own kind alone, because that is what the
// gate counts.
//
// The reading is of the views each machine last published, so it lags a decision in flight in both
// directions (Machine.WaitingWake): a claim whose wake was parked a moment ago can be missing from
// it, and one whose wake has just run can still be in it. Neither costs anything here. The first
// is a post the machine answers by giving its credit straight back, and the second is a claim the
// pass leaves to the next sweep, a minute later.
func (s *supervision) parkedWakes() (map[claim.Token]bool, int) {
	parked := map[claim.Token]bool{}
	refreshes := 0
	for _, wake := range s.waitingWakes() {
		parked[wake.Claim] = true
		if wake.Kind == supervise.WakePluginRefresh {
			refreshes++
		}
	}
	return parked, refreshes
}

// noPluginInstall is c's launch installing no plugin at all, whatever contract the claim records:
// the runtime is Kubernetes and the claim's role runs on Claude Code, whose plugin there is the
// worker image's own claude-envoy. No install reaches it (specs.stalePluginInstall installs none
// for that backend, and no npm install reaches the image's own) and no relaunch moves it, because
// the launch keeps the pod the claim already runs in: only a new image carries another
// claude-envoy, which is what the pod round replaces the pod for.
//
// Under tmux a Claude Code role is exempt from nothing. Its pane loads the claude-envoy held at
// LEGION_CLAUDE_PLUGIN_DIR (resolveClaude; tmux's ClaudeLaunch, whose MCP configuration and
// plugin argument every launch writes afresh), so a relaunch once that pane's turn ends brings
// the worker up on whatever that directory holds now, exactly as an Oh My Pi pane comes back on
// the plugin installed on the daemon's host. No tmux launch installs a plugin for any backend
// (specs.stalePluginInstall), so a relaunch is the whole of the lane there — and for this role
// it is enough.
//
// It is one rule read in three places, each of which would otherwise act on a claim no install
// of this deployment can move: this package's own stale-plugin pass (refreshStalePlugins); the
// machine's stale registration, which supervise cannot decide for itself because a role's
// backend is the daemon's configuration (supervise.Deps.NoPluginInstall, handed this same
// function); and the API's stale-caller refusal, which would otherwise hold such a worker's
// grants, its handoff_complete and its ready on a relaunch that never comes
// (api.Options.NoPluginInstall, api's staleClaim). The harness is backends' for c's issue
// (roleBackends); nil reads worker_backends alone. backends reads the issue's plan only for a role
// worker_backends puts on Claude Code that is none of Legion's, the one case where a role of the
// plan, which runs on Oh My Pi, changes the answer, and it reads it under ctx, the context of the
// call that asks, so each reader asks this only of a claim the cheap terms of the stale-plugin rule
// find stale (supervise.StaleRefusesCalls, staleForTheLane). A caller that holds a transaction of
// the store hands in backends that read through it (stateNoPluginInstall): the pool refuses such a
// caller a second connection (store.ErrNestedAcquire).
//
// A read of the issue's plan that fails is the error, and the answer beside it says nothing of
// the claim: no reader takes it as a claim an install moves or as one no install moves. Each logs
// the error once, or returns it, and decides nothing on it — no call refused, no relaunch posted,
// no registration charged, no count made — and asks again at its next call or pass
// (staleForTheLane, supervise.StaleRefusesCalls, supervise's staleRegistration).
func noPluginInstall(ctx context.Context, cfg config.Config, backends roleBackend, c supervise.Claim) (bool, error) {
	if cfg.Runtime.Name != "kubernetes" {
		return false, nil
	}
	if backends == nil {
		backends = roleBackends(cfg.BackendFor, nil)
	}
	backend, err := backends(ctx, c.Issue, c.Role)
	if err != nil {
		return false, err
	}
	return runtime.Of(backend) == runtime.BackendClaude, nil
}

// stateNoPluginInstall is noPluginInstall as the state's count asks it (source.noPluginInstall):
// inside the state's snapshot, so the plan of a claim's issue is read through tx, the snapshot's
// own transaction, and never on a connection of its own (planRoleIn).
func stateNoPluginInstall(cfg config.Config) func(context.Context, pgx.Tx, supervise.Claim) (bool, error) {
	return func(ctx context.Context, tx pgx.Tx, c supervise.Claim) (bool, error) {
		return noPluginInstall(ctx, cfg, roleBackends(cfg.BackendFor, planRoleIn(tx)), c)
	}
}
