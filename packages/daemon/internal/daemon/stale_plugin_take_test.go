package daemon

// The workers an old plugin runs at the rollout that moves each project's daemon and worker image
// together (dispatch://INFRA-2811, decision 18 and departure 3). That rollout changes the image,
// so an update round opens over every issue pod, and the round's question is a task, which a
// worker whose plugin declared a contract below supervise.TaskTakeContract cannot take. Two
// rules keep such a worker from waiting on it for ever. A claim whose launch installs a plugin
// is relaunched onto the current one inside the round as outside it (staleForTheLane,
// supervise.StaleRefusesCalls). A Claude Code role in a pod, whose plugin is the image's own and
// which no relaunch moves, has its pod released onto the configured image at once when it is
// idle and at its turn's end when it is working, its unpushed work saved first, past the image,
// contract and round holds that keep every other such pod (workflow.IdleRelease's old plugin) —
// unless the pod already runs the configured image, which logs one error naming the claim.

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// oldContract is the plugin contract of a worker the rollout found running: the one just below
// the contract that takes tasks.
const oldContract = supervise.TaskTakeContract - 1

// configuredImage is the worker image the rig's configuration names, which the error a pod
// already on it logs for an old plugin names.
const configuredImage = "ghcr.io/sjawhar/legion-worker@sha256:2f6c0e1a"

// A claim whose plugin takes no task is relaunched onto the current plugin inside the pod round
// the rollout's image change opens over it, as it is outside one. The round asks it a task it
// cannot take, so it would wait on that claim for an answer it never gives. The count of what the
// lane has left names the claim, and the API's stale-caller rule refuses it, both reading the
// same reversal. The relaunch keeps the claim's pod and its round, and the process it starts
// registers on this daemon's contract and takes the round's ask. A claim the round still serves
// on its old contract — one ahead of this daemon, whose plugin takes tasks — is left to it.
func TestStalePluginBelowTheTakeContractIsRelaunchedInsideAPodRound(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true, contract: oldContract},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion + 1},
	)
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	if rig.tookAsk(claim.RoleImplementer) {
		t.Fatal("the claim whose plugin takes no task took its round's ask on its old contract")
	}
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}
	noInstall := func(c supervise.Claim) bool { return noPluginInstall(rig.s.cfg, c.Role) }
	if left := claimsToRefresh(claims, noInstall); left != 1 {
		t.Fatalf("claimsToRefresh = %d, want the one claim whose plugin takes no task", left)
	}
	for _, c := range claims {
		want := c.Role == claim.RoleImplementer
		if refused := supervise.StaleRefusesCalls(c, api.DaemonAPIVersion, true, noInstall); refused != want {
			t.Errorf("the %s at contract %d in round %q is refused its calls = %v, want %v",
				c.Role, c.PluginContract, c.UpdateRound, refused, want)
		}
	}
	token := rig.tokens[claim.RoleImplementer]
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})

	rig.s.refreshStalePlugins(claims, "sweep")

	relaunched := rig.awaitClaim(claim.RoleImplementer, "the claim whose plugin takes no task to be relaunched inside its round",
		func(c supervise.Claim) bool { return c.State == supervise.StateLaunching })
	if relaunched.UpdateRound != podUpdateRound {
		t.Errorf("the relaunched claim records round %q, want %q kept for its new process to answer", relaunched.UpdateRound, podUpdateRound)
	}
	rig.settle(claim.RoleTester)
	if c := rig.claimOf(claim.RoleTester); c.State != supervise.StateReady {
		t.Errorf("the tester, whose plugin takes tasks, is %s, want it left to its round", c.State)
	}
	resumes := rig.rt.CallsOf("Resume")
	if len(resumes) != 1 || resumes[0].Spec.Claim != token {
		t.Fatalf("the sweep started %+v, want the one process of the claim whose plugin takes no task", resumes)
	}

	// The relaunched process comes up in the same pod on the current plugin, and takes the ask
	// its old one could not.
	conn := fake.NewConn()
	rig.conns.Register(token, conn)
	rig.workers[claim.RoleImplementer] = conn
	rig.registerInto(claim.RoleImplementer, podUpdatePod)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || c.Pending == nil {
		t.Fatalf("the relaunched claim records round %q and task %+v, want the round's ask still owed", c.UpdateRound, c.Pending)
	}
	if !rig.tookAsk(claim.RoleImplementer) {
		t.Fatalf("the relaunched process holds %+v, want the round's ask taken on this daemon's contract", rig.claimOf(claim.RoleImplementer).Pending)
	}
}

// A worker that registers on a plugin contract below the one that takes tasks is relaunched onto
// the current plugin rather than left waiting for a prompt this daemon never sends: its
// registration is refused naming both contracts, the relaunch is charged nothing, and the
// process that relaunch starts registers on this daemon's contract and is accepted. It is the
// stale-plugin relaunch this daemon already makes for every other contract, read at the
// boundary the take moved the fleet across.
func TestContractBelowTheTakeContractIsRelaunchedOntoTheCurrentPlugin(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	token := rig.tokens[claim.RoleImplementer]
	m, ok := rig.sup.Machine(token)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	before := rig.claimOf(claim.RoleImplementer)
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})

	err := m.Handle(rig.ctx, supervise.RequestRegister{
		Claim: before.Token, Generation: before.Generation, Session: before.Session, SessionFile: before.SessionFile,
		CapabilityHash: []byte(claim.RoleImplementer), PluginContract: oldContract,
	})
	if !errors.Is(err, claim.StalePluginContract(oldContract, api.DaemonAPIVersion)) {
		t.Fatalf("a registration on contract %d = %v, want it refused as a stale plugin naming both contracts", oldContract, err)
	}
	relaunched := rig.awaitClaim(claim.RoleImplementer, "the worker below the take contract to be relaunched",
		func(c supervise.Claim) bool { return c.State == supervise.StateLaunching })
	if relaunched.Budgets != (supervise.Budgets{}) {
		t.Errorf("the relaunch charged %+v, want nothing: the process predates this daemon's plugin", relaunched.Budgets)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 1 {
		t.Fatalf("the stale registration started %d processes, want the one relaunch", len(resumes))
	}

	// The relaunched process comes up on the current plugin, registers on this daemon's
	// contract and is ready, on a contract that takes tasks.
	conn := fake.NewConn()
	rig.conns.Register(token, conn)
	rig.workers[claim.RoleImplementer] = conn
	rig.registerInto(claim.RoleImplementer, podUpdatePod)
	if after := rig.claimOf(claim.RoleImplementer); after.PluginContract != api.DaemonAPIVersion || supervise.TakesNoTask(after.PluginContract) {
		t.Fatalf("the claim after its relaunch records contract %d, want %d, which takes tasks", after.PluginContract, api.DaemonAPIVersion)
	}
}

// oldPluginRig is the idle rig's pod as the rollout leaves a Claude Code role's: its implementer
// runs on Claude Code, whose claude-envoy declared oldContract, the rollout's image change has
// opened an update round over the pod, and the pod still runs the image before that change.
// The pod's last write is a minute old, well inside the rig's ten-minute idle period, and its
// session reported after it that it runs no background work. logs is what the idle pass wrote.
func oldPluginRig(t *testing.T) (*idleRig, *syncBuffer) {
	t.Helper()
	rig := newIdleRig(t, func(c *supervise.Claim) {
		c.PluginContract = oldContract
		c.UpdateRound, c.UpdateReasons = supervise.PodRound(releasedPodUID), "image"
	})
	rig.rt.onImage = false
	rig.w.idleFixedPlugin = func(role claim.Role) bool { return role == claim.RoleImplementer }
	rig.w.idleImage = configuredImage
	logs := &syncBuffer{}
	rig.w.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := rig.pool.Exec(context.Background(), `update claims set updated_at = now() - interval '1 minute',
		activity_at = now() - interval '30 seconds' where issue = $1`, rig.issue.Key); err != nil {
		t.Fatalf("bring the pod's last write inside the idle period: %v", err)
	}
	return rig, logs
}

// wantReleasedOntoTheImage is the old-plugin release run to its end: the save of the
// workspace's unpushed work, the role's stop and the teardown queued in that order, the bundle
// asked of the role before its process stopped and stored, the claim suspended as released with
// its task kept and out of the round its pod was in, and the pod torn down. The round goes with
// the stop because the save holds what the round would have asked the worker to push: left on
// the claim, the round would read the claim's return in the new pod as a worker that lost its
// work with the pod it never answered for.
func wantReleasedOntoTheImage(t *testing.T, rig *idleRig) {
	t.Helper()
	if got := rig.rows(t); len(got) != 3 || got[0] != "issue_bundle" || got[1] != "supervise:release" || got[2] != "issue_release" {
		t.Fatalf("queued rows = %v, want the save, then the role stop, then the teardown", got)
	}
	rig.drain(t)
	got := rig.machine.Claim()
	if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
		t.Fatalf("the released claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
	}
	if got.Pending == nil || got.Pending.ID != "outbox:7" {
		t.Fatalf("the released claim's task = %+v, want it kept for the process the new pod starts", got.Pending)
	}
	if got.UpdateRound != "" {
		t.Fatalf("the released claim records round %q, want it out of the round its released pod was in", got.UpdateRound)
	}
	bundled, suspended := rig.rt.reads()
	if len(bundled) != 1 || bundled[0] != claim.RoleImplementer {
		t.Fatalf("bundle asked of %v, want the pod's one live role", bundled)
	}
	if !rig.bundled(t) {
		t.Fatal("the release deleted the pod with no bundle of its unpushed work saved")
	}
	if suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want one", suspended)
	}
}

// An idle Claude Code role whose plugin takes no task has no turn end to wait for, and the round
// over its pod asks it a task it cannot take, so its pod is released at once: inside the idle
// period, out of the round, off the image before the rollout, past the contract it records. The
// release saves the workspace's unpushed work before it stops the role, and the role comes back
// on the configured image, whose claude-envoy takes tasks. A pod whose roles' launches install a
// plugin is the control: it is kept for the relaunch that moves its plugin, behind its image as
// every such pod is.
func TestStalePluginClaudeRoleInAPodIsReleasedWhenIdle(t *testing.T) {
	t.Run("a Claude Code role", func(t *testing.T) {
		rig, logs := oldPluginRig(t)

		rig.w.releaseIdlePods(context.Background())

		wantReleasedOntoTheImage(t, rig)
		if line := logs.String(); !strings.Contains(line, "a plugin that takes no task, so the role comes back on the configured image") ||
			!strings.Contains(line, string(rig.token)) {
			t.Errorf("the release logged %s, want the line naming the claim it was made for", line)
		}
	})
	t.Run("a role whose launch installs a plugin", func(t *testing.T) {
		rig, _ := oldPluginRig(t)
		rig.w.idleFixedPlugin = func(claim.Role) bool { return false }

		rig.w.releaseIdlePods(context.Background())

		if got := rig.rows(t); len(got) != 0 {
			t.Fatalf("queued rows = %v, want none: the lane's relaunch moves this role's plugin", got)
		}
		if rig.marked(t) {
			t.Fatal("the pod of a role a relaunch moves was marked for release")
		}
	})
}

// A Claude Code role whose plugin takes no task and that is in a turn keeps its pod until the turn
// ends: the release that passes over the image, the contract and the round holds no turn in flight
// to cut. The turn's end is what releases it, at the next pass, with the idle period no part of
// the wait.
func TestStalePluginClaudeRoleInAPodIsReleasedAtItsTurnsEnd(t *testing.T) {
	rig, _ := oldPluginRig(t)
	ctx := context.Background()
	if err := rig.machine.Handle(ctx, supervise.StreamTurnStart{Claim: rig.token}); err != nil {
		t.Fatalf("start the worker's turn: %v", err)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateWorking {
		t.Fatalf("the claim is %s, want it in its turn", got.State)
	}

	rig.w.releaseIdlePods(ctx)

	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("queued rows = %v, want none while the role is in its turn", got)
	}
	if rig.marked(t) {
		t.Fatal("the pod was marked for release under a turn in flight")
	}

	if err := rig.machine.Handle(ctx, supervise.StreamTurnEnd{Claim: rig.token}); err != nil {
		t.Fatalf("end the worker's turn: %v", err)
	}
	if got := rig.machine.Claim(); got.State == supervise.StateWorking {
		t.Fatalf("the claim is %s after its turn's end", got.State)
	}

	rig.w.releaseIdlePods(ctx)

	wantReleasedOntoTheImage(t, rig)
}

// A Claude Code role whose plugin takes no task in a pod that already runs the configured image
// is not released: the pod that replaced it would come up on the same image with the same
// claude-envoy, so a deployment that has not moved its image would release that pod again at
// every pass. The pod is kept under the contract hold, and one error names the claim, its
// contract and the image; the next pass over the same pod logs nothing more.
func TestStalePluginClaudeRoleOnTheConfiguredImageIsNotReleased(t *testing.T) {
	rig, logs := oldPluginRig(t)
	rig.rt.onImage = true
	ctx := context.Background()

	rig.w.releaseIdlePods(ctx)
	rig.w.releaseIdlePods(ctx)

	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("queued rows = %v, want none: the pod already runs the configured image", got)
	}
	if rig.marked(t) {
		t.Fatal("a pod already on the configured image was marked for release")
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the claim = %s by %s, want it left running", got.State, got.SuspendedBy)
	}
	var errorsLogged []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			errorsLogged = append(errorsLogged, line)
		}
	}
	if len(errorsLogged) != 1 {
		t.Fatalf("the two passes logged %d errors, want one: %q", len(errorsLogged), errorsLogged)
	}
	for _, want := range []string{string(rig.token), `"pluginContract":` + fmtContract(oldContract), configuredImage} {
		if !strings.Contains(errorsLogged[0], want) {
			t.Errorf("the error %s does not name %s", errorsLogged[0], want)
		}
	}
	if hold := workflow.IdleHoldPluginContract.String(); !strings.Contains(logs.String(), `"hold":"`+hold+`"`) {
		t.Errorf("the pass logged %s, want the pod kept under %s", logs.String(), hold)
	}
}

// fmtContract is a plugin contract as the log and the refusal write it.
func fmtContract(contract int) string { return strconv.Itoa(contract) }
