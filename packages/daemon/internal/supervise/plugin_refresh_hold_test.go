package supervise

// The task a stale claim is owed waits for the process the refresh lane's relaunch starts
// (sendPending's staleRefusesClaim, dispatch://INFRA-2712 R2). The window these tests drive is
// the one a plugin wave leaves the whole fleet in: every live claim records the contract of the
// daemon before this one, the gate lets the lane relaunch only K of them a minute, and until a
// claim's own relaunch has run the daemon answers that claim's every `legion` route 409
// (internal/api's staleClaim). A task sent into that window reaches an agent that can read it
// and do nothing with it, and the turn it starts is what postpones the relaunch the claim is
// waiting for (refreshPlugin's holdStaleRelaunch).
//
// The hold is exactly the set internal/api refuses, so the two claims that set leaves out hold
// nothing back and are sent their task where it arrives: a Claude Code role's claim in a pod,
// whose plugin is the image's own and which no relaunch of this deployment moves, and every
// claim of a daemon that can relaunch onto no plugin at all (LEGION-164's degraded Kubernetes
// mode), where no relaunch is coming for any of them and a held task would wait for a process
// nobody is going to start.
//
// The negative controls are the terms of that predicate, one test each: the hold reverted in
// sendPending for the first, and for the other two the term that excuses the claim dropped from
// staleRefusesClaim (its NoPluginInstall branch, its PluginRelaunchable term), which holds a
// task that must go now.

import "testing"

// A stale claim with no wake parked over it yet — the gate has admitted it nothing, which at a
// fleet's worth of claims is most of them for most of a wave — holds the task it is handed, and
// is sent it once the process its relaunch starts has registered on this daemon's contract and
// said it is ready. Nothing new carries it there: the ready of every launch sends the task the
// claim was holding (finishReady, sendPending), which is where a task held back for a waiting
// launch already goes.
//
// The task is held with no wake at all, which is what tells this hold from the launch's
// (launchWaits): the claim records its process, is idle, and holds a wake of no kind, so the
// only thing standing between the task and the prompt is the contract.
func TestAStaleClaimHoldsItsTaskUntilItsReplacementRegistersOnThisDaemonsContract(t *testing.T) {
	const next = "review the pull request"
	h := pacedHarness(t, wakeRate)
	h.reach(StateIdle)
	// The walk to idle delivers a phase task and works it, so one prompt is already sent.
	sent := len(h.prompts())
	// The daemon this claim's process registered with is gone; this one speaks the next
	// contract, and can relaunch onto its plugin. The harness's own register() walk records the
	// daemon's contract, so the replacement below comes up current.
	h.deps.PluginContract, h.m.deps.PluginContract = refreshContract, refreshContract
	h.m.deps.PluginRelaunchable = true

	h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})

	h.wantPrompts(sent)
	if h.m.View().WakeWaiting != nil {
		t.Fatalf("the claim holds the wake %+v, want none: this task is held by the contract, not by a launch",
			h.m.View().WakeWaiting)
	}
	if p := h.pending(); p.Task != next || !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task delivered to the stale claim is %+v, want %q queued unread: every `legion` call of the process it would reach is refused",
			p, next)
	}

	// The lane's credit comes, the relaunch waits its paced turn, and the turn replaces the
	// process. The task is still the claim's own through all of it.
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Admitted: true})
	waiting := h.wantWaitingWake(WakePluginRefresh)
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantPrompts(sent)
	if p := h.pending(); p.Task != next || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the task after the relaunch's turn is %+v, want %q still waiting: no turn of it ever ran", p, next)
	}

	h.relaunched()

	if got := h.claim(); got.PluginContract != refreshContract {
		t.Fatalf("the relaunched claim records contract %d, want this daemon's %d", got.PluginContract, refreshContract)
	}
	// The stale process left no report the daemon took (a stale plugin's report is refused), so
	// the relaunch recorded its background work as unknown, and the task carries that sentence in
	// front of it (dispatch://INFRA-3276 R3).
	ended := h.claim().EndedWork
	if ended.Cause != endStalePlugin || ended.Reported || !ended.Tells() {
		t.Fatalf("the relaunch recorded %+v, want an untold, unreported end of the stale plugin's process", ended)
	}
	if got, want := h.wantPrompts(sent + 1)[sent], relaunchSentence(ended, true)+"\n\n"+next; got.Message != want {
		t.Fatalf("the prompt after the relaunch is %+v, want the task %q behind the relaunch's sentence, to the process this daemon serves", got, want)
	}
}

// A Claude Code role's claim in a pod is sent its task where it arrives, whatever contract it
// records. Its plugin is the worker image's own claude-envoy, which no install of this
// deployment reaches and no relaunch moves (Deps.NoPluginInstall), so the lane posts it nothing
// and its stale registration is accepted and charged nothing — and internal/api serves it on
// the contract it records for the same reason. Held here, its task would wait for a replacement
// process the daemon is never going to start, for the rest of the deployment's life.
func TestAClaudeCodeRolesClaimInAPodIsSentItsTaskOnAnEarlierContract(t *testing.T) {
	const next = "review the pull request"
	h := newHarness(t)
	h.reach(StateIdle)
	sent := len(h.prompts())
	h.m.deps.PluginContract = refreshContract
	h.m.deps.PluginRelaunchable = true
	h.m.deps.NoPluginInstall = func(Claim) bool { return true }

	h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})

	if got := h.wantPrompts(sent + 1)[sent]; got.Message != next {
		t.Fatalf("the prompt to the claim no install reaches is %+v, want the task %q at once: nothing will ever relaunch it",
			got, next)
	}
	if got := h.claim(); got.PluginContract == refreshContract {
		t.Fatalf("the claim records contract %d, want the earlier one it registered on: this test is of a stale claim",
			got.PluginContract)
	}
}

// A stale claim of a daemon that can relaunch onto no plugin at all is sent its task where it
// arrives too (Deps.PluginRelaunchable false, LEGION-164's degraded Kubernetes mode). Such a
// daemon starts no relaunch for any stale claim, and internal/api refuses none of them, because
// refusing them would stall agents that work today. The same reading holds the task: there is no
// replacement process to wait for.
func TestAStaleClaimOnADaemonThatCanRelaunchOntoNoPluginIsSentItsTask(t *testing.T) {
	const next = "review the pull request"
	h := newHarness(t)
	h.reach(StateIdle)
	sent := len(h.prompts())
	h.m.deps.PluginContract = refreshContract
	// PluginRelaunchable is left as the harness leaves it, false: this daemon knows no plugin
	// versions to relaunch anything onto.

	h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})

	if got := h.wantPrompts(sent + 1)[sent]; got.Message != next {
		t.Fatalf("the prompt to the stale claim of a degraded daemon is %+v, want the task %q at once: no relaunch is coming for it",
			got, next)
	}
}
