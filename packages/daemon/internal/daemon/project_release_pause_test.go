package daemon

// The two refusals between a project's pause and its release (dispatch://INFRA-3583 R13, checks
// E17 and E18), and the answer of a release that finished before a pause, run on the idle rig
// through the `legion` an operator runs: the whole daemon on a scratch Postgres, the Sandbox
// runtime over the rig's stand-in cluster, a real NATS and Envoy listener, and a scripted role
// process in each pod. Each test logs every answer the checks print, each line beginning with its
// check's name.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pauseReleaseConfiguration is the configuration each test here boots on: the move test's wake
// pacing (moveConfiguration), one launch a second, with the idle release on where the move test
// turns it off, because a daemon whose idle release is off cannot hold a pause and answers
// PAUSE_UNAVAILABLE (pause.Unavailable). Its idle period and backstop are longer than any test here
// runs, so no pod is released and no stopped role is woken behind the test's back. Each test boots
// on the wall clock (idleRigOptions.wallClock), as the move test does: the pacing lets a launch out
// on a timer, which the rig's still clock never fires, so on it every launch after the first token
// waits for ever.
func pauseReleaseConfiguration(cfg *config.Config) {
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = 60, 1
	cfg.Runtime.Kubernetes.IdleRelease = config.IdleReleaseConfig{IdleMinutes: 60, BackstopMinutes: 60}
}

// operatorCommand runs `legion <args>` with the operator's flags against the rig's daemon, and logs
// what it printed and how it exited under check.
func (r *idleWakeRig) operatorCommand(check, binary string, args ...string) idleRigCommand {
	r.t.Helper()
	ran := r.legion(binary, append(args, r.operatorFlags()...)...)
	r.t.Logf("%s: legion %s exited %d; stdout %q; stderr %q", check, strings.Join(args, " "), ran.code, ran.stdout, ran.stderr)
	return ran
}

// operatorPost posts body to the operator's route at target, logs the daemon's answer under check,
// and answers its status and its refusal's body, decoded.
func (r *idleWakeRig) operatorPost(check, target string, body any) (int, api.PauseFailure) {
	r.t.Helper()
	status, answer := r.api.request(http.MethodPost, target, body, true)
	r.t.Logf("%s: POST %s answered %d %s", check, target, status, answer)
	var refused api.PauseFailure
	if status/100 != 2 {
		if err := json.Unmarshal(answer, &refused); err != nil {
			r.t.Fatalf("decode the refusal %s: %v", answer, err)
		}
	}
	return status, refused
}

// heldPods is the issue of each pod a pause still holds, in issue order (project_pause_pod_holds).
func (r *idleWakeRig) heldPods() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select issue from project_pause_pod_holds where ended_at is null order by issue`)
	if err != nil {
		r.t.Fatalf("read the pods a pause holds: %v", err)
	}
	defer rows.Close()
	held := []string{}
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			r.t.Fatalf("read the pods a pause holds: %v", err)
		}
		held = append(held, issue)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the pods a pause holds: %v", err)
	}
	return held
}

// count is the one number sql counts in the rig's database.
func (r *idleWakeRig) count(sql string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		r.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// roleStates is each claim as the operator's listing answers it, by token: its state, and the
// source of its suspension where it is suspended.
func (r *idleWakeRig) roleStates() map[claim.Token]string {
	r.t.Helper()
	states := map[claim.Token]string{}
	for _, c := range r.api.claims() {
		states[c.Token] = c.State
		if c.SuspendedBy != "" {
			states[c.Token] += ":" + c.SuspendedBy
		}
	}
	return states
}

// heldIssues is the issue of each pod a refusal names, in its order.
func heldIssues(refused api.PauseFailure) []string {
	issues := []string{}
	for _, pod := range refused.HeldPods {
		issues = append(issues, pod.Issue)
	}
	return issues
}

// stoppedByAPause is the state the operator's listing gives a claim a pause stopped.
var stoppedByAPause = string(supervise.StateSuspended) + ":" + string(supervise.SuspendedByReleased)

// E17 (dispatch://INFRA-3583 R13): `legion project release` answers 409 PROJECT_PAUSED while the
// project is paused, its dry run included, and deletes and stops no Sandbox; it answers so too
// after the resume while a pod the pause stopped is still held, naming that pod alone once every
// other pod's role was resumed; and once the last held pod's role is resumed the release runs.
func TestAReleaseIsRefusedWhileItsProjectIsPaused(t *testing.T) {
	t.Parallel()
	const check = "E17"
	turn := &idleRigPlan{TakeTurn: true}
	implementer := []claim.Role{claim.RoleImplementer}
	rig := newIdleRigWith(t, idleRigOptions{wallClock: true, configure: pauseReleaseConfiguration},
		idleRigSeed{name: "alpha", roles: implementer, plan: turn},
		idleRigSeed{name: "beta", roles: implementer, plan: turn},
		idleRigSeed{name: "gamma", roles: implementer, plan: turn})
	legion := buildLegion(t)
	rig.launchAll()
	issues := []idleRigIssue{rig.issue("alpha"), rig.issue("beta"), rig.issue("gamma")}
	keys := []string{issues[0].key, issues[1].key, issues[2].key}
	idleRigEventually(t, "each role up with no task it has not taken", func() bool {
		claims := rig.claimsByToken()
		for _, issue := range issues {
			if c := claims[rig.token(issue)]; !idleRigWorking(c.State) || c.Pending != nil {
				return false
			}
		}
		return true
	})
	sandboxes := rig.kube.sandboxNames()
	t.Logf("%s: the project's Sandboxes before the pause: %v", check, sandboxes)

	if paused := rig.operatorCommand(check, legion, "project", "pause", "--reason", "E17: the project is paused before its release"); paused.code != 0 {
		t.Fatalf("legion project pause exited %d, want 0", paused.code)
	}
	idleRigEventually(t, "each role stopped by the pause and each pod held", func() bool {
		states := rig.roleStates()
		for _, issue := range issues {
			if states[rig.token(issue)] != stoppedByAPause {
				return false
			}
		}
		return slices.Equal(rig.heldPods(), keys)
	})
	t.Logf("%s: each role's state while paused: %v; the pods a pause holds: %v", check, rig.roleStates(), rig.heldPods())
	if owed := rig.count(`select count(*) from project_pause_stops where launch_owed`); owed != 0 {
		t.Fatalf("the pause owes %d roles a launch, want none: every role had taken its task, so the resume launches none", owed)
	}

	// A release during the pause, its dry run and then the release itself: each refused, and
	// nothing of the project stopped or deleted. Both run before the test stops on a failure, so a
	// release that the pause did not refuse shows what it did.
	refusedWhilePaused := func(flags ...string) {
		t.Helper()
		ran := rig.operatorCommand(check, legion, append([]string{"project", "release"}, flags...)...)
		status, refused := rig.operatorPost(check, api.ProjectReleaseRoute, api.ProjectReleaseRequest{DryRun: slices.Contains(flags, "--dry-run")})
		t.Logf("%s: the project's Sandboxes after legion project release %v: %v", check, flags, rig.kube.sandboxNames())
		if ran.code != 1 || !strings.Contains(ran.stderr, "409") || !strings.Contains(ran.stderr, "the project is paused") {
			t.Errorf("legion project release %v while paused exited %d with stderr %q, want 1 and the daemon's 409", flags, ran.code, ran.stderr)
		}
		if status != http.StatusConflict || refused.Code != "PROJECT_PAUSED" || refused.Pause == nil || !slices.Equal(heldIssues(refused), keys) {
			t.Errorf("a release %v while paused answered %d %+v, want 409 PROJECT_PAUSED naming the pause and the pods of %v", flags, status, refused, keys)
		}
		if now := rig.kube.sandboxNames(); !slices.Equal(now, sandboxes) {
			t.Errorf("the project's Sandboxes after the release %v are %v, want every one of %v", flags, now, sandboxes)
		}
		states := rig.roleStates()
		for _, issue := range issues {
			if states[rig.token(issue)] != stoppedByAPause {
				t.Errorf("the claim of %s after the release %v is %s, want it as the pause left it, %s", issue.key, flags, states[rig.token(issue)], stoppedByAPause)
			}
		}
		if n := rig.count(`select count(*) from project_releases`); n != 0 {
			t.Errorf("the release %v while paused left %d records, want none", flags, n)
		}
	}
	refusedWhilePaused("--dry-run")
	refusedWhilePaused()
	if t.Failed() {
		t.FailNow()
	}

	// The resume ends no hold: no role of a held pod runs, and the pause owed none a launch.
	if resumed := rig.operatorCommand(check, legion, "project", "resume"); resumed.code != 0 {
		t.Fatalf("legion project resume exited %d, want 0", resumed.code)
	}
	if held := rig.heldPods(); !slices.Equal(held, keys) {
		t.Fatalf("the pods a pause holds after the resume are %v, want %v", held, keys)
	}

	// Each role but gamma's resumed, which ends its pod's hold at its ready.
	for _, issue := range issues[:2] {
		if resumed := rig.operatorCommand(check, legion, "claims", "resume", "--claim", string(rig.token(issue))); resumed.code != 0 {
			t.Fatalf("legion claims resume of %s exited %d, want 0", issue.key, resumed.code)
		}
	}
	idleRigEventually(t, "the holds of the resumed roles' pods ended", func() bool {
		return slices.Equal(rig.heldPods(), keys[2:])
	})
	t.Logf("%s: each role's state with one pod still held: %v; the pods a pause holds: %v", check, rig.roleStates(), rig.heldPods())
	ran := rig.operatorCommand(check, legion, "project", "release")
	status, refused := rig.operatorPost(check, api.ProjectReleaseRoute, api.ProjectReleaseRequest{})
	t.Logf("%s: the project's Sandboxes after it: %v", check, rig.kube.sandboxNames())
	gamma := rig.token(issues[2])
	if ran.code != 1 || !strings.Contains(ran.stderr, "409") || !strings.Contains(ran.stderr, keys[2]+" (resume one of "+string(gamma)+")") ||
		strings.Contains(ran.stderr, keys[0]) || strings.Contains(ran.stderr, keys[1]) {
		t.Errorf("legion project release with %s still held exited %d with stderr %q, want 1 and the daemon's 409 naming %s and its claim alone",
			keys[2], ran.code, ran.stderr, keys[2])
	}
	if status != http.StatusConflict || refused.Code != "PROJECT_PAUSED" || refused.Pause != nil || !slices.Equal(heldIssues(refused), keys[2:]) ||
		!slices.Equal(refused.HeldPods[0].Claims, []claim.Token{gamma}) {
		t.Errorf("a release with %s still held answered %d %+v, want 409 PROJECT_PAUSED naming no pause and %s's pod with %s", keys[2], status, refused, keys[2], gamma)
	}
	if now := rig.kube.sandboxNames(); !slices.Equal(now, sandboxes) {
		t.Errorf("the project's Sandboxes after the refused release are %v, want every one of %v", now, sandboxes)
	}
	if n := rig.count(`select count(*) from project_releases`); n != 0 {
		t.Errorf("the refused release left %d records, want none", n)
	}
	if t.Failed() {
		t.FailNow()
	}

	// The last held pod's role resumed: the release runs, and takes every pod.
	if resumed := rig.operatorCommand(check, legion, "claims", "resume", "--claim", string(gamma)); resumed.code != 0 {
		t.Fatalf("legion claims resume of %s exited %d, want 0", keys[2], resumed.code)
	}
	idleRigEventually(t, "the last hold ended", func() bool { return len(rig.heldPods()) == 0 })
	released, release := rig.projectRelease(legion, "--bound", "20s")
	t.Logf("%s: legion project release once the last hold ended exited %d; stdout %s; stderr %q", check, released.code, released.stdout, released.stderr)
	t.Logf("%s: the project's Sandboxes after it: %v", check, rig.kube.sandboxNames())
	if released.code != 0 || release.FinishedAt == nil || release.ID == 0 {
		t.Fatalf("legion project release once the last hold ended exited %d with %+v, want 0 and a finished release", released.code, release)
	}
	for _, issue := range issues {
		if line := releaseLine(t, release, issue.pod); !line.Deleted() {
			t.Errorf("the release's line of %s is %+v, want it deleted", issue.pod, line)
		}
	}
}

// E18 (dispatch://INFRA-3583 R13): while a project release runs — here held in the push wait of a
// pod whose role was asked to push and does not answer — `legion project pause` answers 409
// PROJECT_RELEASING, and nothing is paused and no role is stopped; once the release has finished,
// the pause is taken. "Finished" is the release's finish of its acts (its record's finishedAt,
// store.ProjectRelease.Runs), not the next boot: the daemon is not restarted between the two
// pauses, and the release's mark still holds every start when the second is taken.
func TestAPauseIsRefusedWhileAReleaseRuns(t *testing.T) {
	t.Parallel()
	const check = "E18"
	implementer := []claim.Role{claim.RoleImplementer}
	rig := newIdleRigWith(t, idleRigOptions{wallClock: true, configure: pauseReleaseConfiguration},
		idleRigSeed{name: "waiting", roles: implementer, plan: &idleRigPlan{TakeTurn: true, Unpushed: &idleRigReadUnpushed{Commits: 2}}},
		idleRigSeed{name: "clean", roles: implementer, plan: &idleRigPlan{TakeTurn: true}})
	legion := buildLegion(t)
	rig.launchAll()
	waiting := rig.issue("waiting")

	rig.awaitReportsSinceLastTurn()
	args := rig.releaseArgs("--bound", "40s")
	done := make(chan idleRigCommand, 1)
	go func() { done <- legionRun(legion, args...) }()
	idleRigEventually(t, "the release holding "+waiting.pod+" in its push wait", func() bool {
		running, ok := rig.releaseNow()
		if !ok || running.FinishedAt != nil {
			return false
		}
		line, ok := releaseLineNow(running, waiting.pod)
		return ok && line.Outcome == api.ReleaseWaiting && len(line.Asked) > 0
	})
	running, _ := rig.readRelease()
	before := rig.roleStates()
	t.Logf("%s: release %d holds %s in its push wait; each role's state before the pause: %v", check, running.ID, waiting.pod, before)

	const reason = "E18: a pause while the release runs"
	refusedPause := rig.operatorCommand(check, legion, "project", "pause", "--reason", reason)
	status, refused := rig.operatorPost(check, "/legion/v1/operator/project/pause", api.OperatorPauseRequest{Reason: reason})
	after := rig.roleStates()
	t.Logf("%s: each role's state after the refused pause: %v", check, after)
	if refusedPause.code != 1 || !strings.Contains(refusedPause.stderr, "409") ||
		!strings.Contains(refusedPause.stderr, fmt.Sprintf("project release %d runs", running.ID)) {
		t.Errorf("legion project pause while release %d runs exited %d with stderr %q, want 1 and the daemon's 409 naming the release",
			running.ID, refusedPause.code, refusedPause.stderr)
	}
	if status != http.StatusConflict || refused.Code != pause.CodeReleasing {
		t.Errorf("a pause while release %d runs answered %d %+v, want 409 %s", running.ID, status, refused, pause.CodeReleasing)
	}
	for token, state := range after {
		if state == stoppedByAPause {
			t.Errorf("the claim %s is %s after the refused pause (it was %s), want no role stopped by a pause", token, state, before[token])
		}
	}
	if !idleRigWorking(strings.Split(after[rig.token(waiting)], ":")[0]) {
		t.Errorf("the role the release waits on is %s after the refused pause, want it running", after[rig.token(waiting)])
	}
	if n := rig.count(`select count(*) from project_pauses`) + rig.count(`select count(*) from project_pause_stops`); n != 0 {
		t.Errorf("the refused pause left %d pause records, want none", n)
	}
	if t.Failed() {
		t.FailNow()
	}

	ran, finished := awaitReleaseCommand(t, done)
	if finished.FinishedAt == nil || finished.ID != running.ID {
		t.Fatalf("legion project release exited %d printing %q (stderr %q), want release %d finished", ran.code, ran.stdout, ran.stderr, running.ID)
	}
	released := rig.roleStates()
	t.Logf("%s: legion project release exited %d; release %d finished at %s; each role's state: %v", check, ran.code, finished.ID,
		finished.FinishedAt.UTC().Format(time.RFC3339), released)

	taken := rig.operatorCommand(check, legion, "project", "pause", "--reason", "E18: a pause once the release has finished")
	paused := rig.roleStates()
	t.Logf("%s: each role's state after the pause: %v", check, paused)
	if taken.code != 0 || !strings.HasPrefix(taken.stdout, "project paused: ") {
		t.Fatalf("legion project pause once release %d finished exited %d (stdout %q, stderr %q), want the pause taken", finished.ID, taken.code, taken.stdout, taken.stderr)
	}
	if n := rig.count(`select count(*) from project_pauses where ended_at is null`); n != 1 {
		t.Fatalf("the store holds %d open pauses after the pause was taken, want one", n)
	}
	// The pause after the release stops no role: the release stopped every role, so the pause asks
	// no stop, and each claim reads as the release left it.
	if n := rig.count(`select count(*) from project_pause_stops`); n != 0 {
		t.Errorf("the pause after the release asked %d stops, want none", n)
	}
	for token, state := range released {
		if paused[token] != state {
			t.Errorf("the claim %s is %s after the pause, want it as the release left it, %s", token, paused[token], state)
		}
	}
}

// dispatch://INFRA-3583 R13, after E18: once this process's release has finished and a pause is
// taken, `legion project release` run again, its dry run too, prints that release and exits 0, as
// it does with no pause, and the route answers it 200. The daemon answers its own release before
// it reads the pause, and that answer starts nothing: the release is not refused 409
// PROJECT_PAUSED, whose sentence says that nothing was recorded, stopped or deleted, after it
// stopped and deleted, and no second release is recorded.
func TestAFinishedReleaseIsAnsweredWhileItsProjectIsPaused(t *testing.T) {
	t.Parallel()
	const check = "R13 after the release"
	rig := newIdleRigWith(t, idleRigOptions{wallClock: true, configure: pauseReleaseConfiguration},
		idleRigSeed{name: "alpha", roles: []claim.Role{claim.RoleImplementer}, plan: &idleRigPlan{TakeTurn: true}})
	legion := buildLegion(t)
	rig.launchAll()
	alpha := rig.issue("alpha")
	idleRigEventually(t, "the role up with no task it has not taken", func() bool {
		c := rig.claimsByToken()[rig.token(alpha)]
		return idleRigWorking(c.State) && c.Pending == nil
	})

	released, release := rig.projectRelease(legion, "--bound", "20s")
	t.Logf("%s: legion project release exited %d; stdout %s; stderr %q", check, released.code, released.stdout, released.stderr)
	if released.code != 0 || release.FinishedAt == nil || release.ID == 0 {
		t.Fatalf("legion project release exited %d with %+v, want 0 and a finished release", released.code, release)
	}
	if line := releaseLine(t, release, alpha.pod); !line.Deleted() {
		t.Fatalf("the release's line of %s is %+v, want it deleted", alpha.pod, line)
	}
	taken := rig.operatorCommand(check, legion, "project", "pause", "--reason", check+": a pause once the release has finished")
	if taken.code != 0 || !strings.HasPrefix(taken.stdout, "project paused: ") {
		t.Fatalf("legion project pause once release %d finished exited %d (stdout %q, stderr %q), want the pause taken",
			release.ID, taken.code, taken.stdout, taken.stderr)
	}

	for _, flags := range [][]string{nil, {"--dry-run"}} {
		ran, again := rig.projectRelease(legion, flags...)
		t.Logf("%s: legion project release %v while paused exited %d; stdout %s; stderr %q", check, flags, ran.code, ran.stdout, ran.stderr)
		if ran.code != 0 || again.ID != release.ID || again.DryRun || again.FinishedAt == nil || !again.FinishedAt.Equal(*release.FinishedAt) {
			t.Errorf("legion project release %v while paused exited %d with %+v, want 0 and release %d as it finished", flags, ran.code, again, release.ID)
		}
	}
	status, body := rig.api.request(http.MethodPost, api.ProjectReleaseRoute, api.ProjectReleaseRequest{}, true)
	t.Logf("%s: POST %s while paused answered %d %s", check, api.ProjectReleaseRoute, status, body)
	var answered api.ProjectReleaseResponse
	if err := json.Unmarshal(body, &answered); status != http.StatusOK || err != nil || answered.ID != release.ID || answered.FinishedAt == nil {
		t.Errorf("POST %s while paused answered %d %s, want 200 with release %d as it finished", api.ProjectReleaseRoute, status, body, release.ID)
	}
	if n := rig.count(`select count(*) from project_releases`); n != 1 {
		t.Errorf("the store holds %d project releases, want only the one that finished", n)
	}
	if n := rig.count(`select count(*) from project_pauses where ended_at is null`); n != 1 {
		t.Errorf("the store holds %d open pauses, want the one taken after the release", n)
	}
}
