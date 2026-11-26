package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// podWaitRigRole is the role the rig's deployment defines after its pod was built: no pod of the
// rig holds a launcher, a Secret or a volume of it until a pod is built again.
const podWaitRigRole = claim.Role("checker")

// defineChecker gives the rig's configuration the role checker and restarts the daemon on it, as a
// deployment that adds a role to its settings restarts: the restarted daemon's runtime runs the
// role in every issue pod it builds from then on (sandboxOptions' DeploymentApps), and none it
// built before. The checker launches with its own prompt file (prompt_file), as every role of the
// deployment does (prompts.Composer.ComposeStep).
func (r *idleWakeRig) defineChecker(issue idleRigIssue) {
	r.t.Helper()
	prompt := filepath.Join(r.t.TempDir(), "checker.md")
	if err := os.WriteFile(prompt, []byte("You are the checker of "+issue.key+". Check the work, then report pass or fail.\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.cfg.Roles = append(r.cfg.Roles, config.Role{Name: podWaitRigRole, Type: "testing", PromptFile: prompt})
	r.restart()
	r.awaitListenerReady()
}

// roleToken is the claim token of role on the issue.
func (r *idleWakeRig) roleToken(issue idleRigIssue, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// startChecker queues the start of the checker on the issue, as the workflow queues a start of the
// role whose step the issue's task stands at.
func (r *idleWakeRig) startChecker(issue idleRigIssue) {
	r.t.Helper()
	enqueueOutbox(r.t, r.pool, r.records, mustOutboxRow(r.t, issue.key, record.SuperviseRequest{
		Op: "start", Tree: issue.key, Role: podWaitRigRole, Generation: 1, Task: "Check " + issue.key + ".",
	}, time.Now().UTC()))
}

// startWaitsForPod is the roles the served state names as waiting for the issue's pod.
func (r *idleWakeRig) startWaitsForPod(issue idleRigIssue) []string {
	r.t.Helper()
	return r.api.state().Issues[issue.key].StartWaitsForPod
}

// claimOf is whether the operator's listing holds a claim of role on the issue.
func (r *idleWakeRig) claimOf(issue idleRigIssue, role claim.Role) bool {
	r.t.Helper()
	token := r.roleToken(issue, role)
	return slices.ContainsFunc(r.api.claims(), func(c api.OperatorClaim) bool { return string(c.Token) == string(token) })
}

// releaseMarked is whether a release of the issue's pod is marked.
func (r *idleWakeRig) releaseMarked(issue idleRigIssue) bool {
	r.t.Helper()
	var marked bool
	if err := r.pool.QueryRow(context.Background(), `select exists (select 1 from issue_releases where issue = $1)`, issue.key).Scan(&marked); err != nil {
		r.t.Fatal(err)
	}
	return marked
}

// podUID is the uid of the issue's pod as the fake API holds it, "" while it holds no pod of the
// issue. It reads the pod as main's rigs read a pod's uid (controllerSandboxPodOf).
func (r *idleWakeRig) podUID(issue idleRigIssue) string {
	pod, _ := r.kube.controllerSandboxPodOf(issue.pod)
	return pod.uid
}

// livePodUID is podUID of a pod that must exist now. A read of no uid fails the test, so a later
// read is never compared with an empty one.
func (r *idleWakeRig) livePodUID(issue idleRigIssue) string {
	r.t.Helper()
	uid := r.podUID(issue)
	if uid == "" {
		r.t.Fatalf("the pod of %s reads no uid, want the live pod that later reads are compared with", issue.key)
	}
	return uid
}

// launcherConnections is how many times the launcher of the issue's role in the pod uid has been
// accepted by a daemon: the line the launcher logs once a daemon acknowledges its hello
// (launcher's serve), which names the pod's Sandbox, the role and the pod.
func (r *idleWakeRig) launcherConnections(issue idleRigIssue, uid string) int {
	r.t.Helper()
	accepted := fmt.Sprintf("(sandbox %s, role %s, pod %s)", issue.pod, issue.role, uid)
	connections := 0
	for _, line := range strings.Split(r.pods.log(), "\n") {
		if strings.Contains(line, "launcher: connected to the daemon at ") && strings.Contains(line, accepted) {
			connections++
		}
	}
	return connections
}

// checkerStartTried is whether the outbox has tried the checker's start at least once: the row is
// gone (a try that started the role) or it carries a try (a try that waits).
func (r *idleWakeRig) checkerStartTried(issue idleRigIssue) bool {
	r.t.Helper()
	var untried bool
	if err := r.pool.QueryRow(context.Background(), `select exists (select 1 from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $2 and attempts = 0)`, issue.key, string(podWaitRigRole)).Scan(&untried); err != nil {
		r.t.Fatal(err)
	}
	return !untried
}

// countPodWaitHeld records every pod-wait-held controller notice the daemon queues from now on, as
// a row of a table of the test's own written by a trigger on the outbox: the board lane publishes
// and deletes such a row as it comes due, so the outbox itself cannot be counted. It answers how
// many it has recorded for the issue.
func (r *idleWakeRig) countPodWaitHeld(issue idleRigIssue) func() int {
	r.t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`create table if not exists pod_wait_held_told (issue text not null, reason text not null)`,
		`create or replace function pod_wait_held_told() returns trigger language plpgsql as $$
		begin
			if new.kind = 'controller_notice' and new.payload->>'kind' = 'pod-wait-held' then
				insert into pod_wait_held_told (issue, reason) values (new.issue, coalesce(new.payload->>'reason', ''));
			end if;
			return new;
		end $$`,
		`drop trigger if exists pod_wait_held_told on outbox`,
		`create trigger pod_wait_held_told after insert on outbox for each row execute function pod_wait_held_told()`,
	} {
		if _, err := r.pool.Exec(ctx, statement); err != nil {
			r.t.Fatalf("record the pod-wait-held notices: %v", err)
		}
	}
	return func() int {
		var told int
		if err := r.pool.QueryRow(ctx, `select count(*) from pod_wait_held_told where issue = $1`, issue.key).Scan(&told); err != nil {
			r.t.Fatal(err)
		}
		return told
	}
}

// awaitUnownedReleasedThenStarted waits for the issue's pod, its Sandbox and its disk to be gone,
// or for a pod other than before to stand in its place,
// while every claim of the issue but role's stays as it was — as awaitUnownedReleased waits, with
// role's line left out, since the start that waits for the pod makes role's claim the moment the
// Sandbox has left the store — then for role's claim to register in a new pod of the issue, other
// than the pod before.
//
// The first wait keeps the pod quiet, since the release measures the idle period from the claim
// rows. It back-dates every claim of the project but role's (quietExcept). The start of role runs
// in the moment the release takes the pod. A back-date of the new claim made before the wait reads
// the pod gone would let the idle rule release the new pod as idle at once, so the role would be
// suspended and never registered in it. A slow poll, as when the package's other tests load the
// machine, makes that moment wide.
func (r *idleWakeRig) awaitUnownedReleasedThenStarted(issue idleRigIssue, role claim.Role, before string) {
	r.t.Helper()
	token := string(r.roleToken(issue, role))
	others := func() []string {
		return slices.DeleteFunc(r.claimLines(issue), func(line string) bool { return strings.HasPrefix(line, token+":") })
	}
	held := others()
	seen := "never read"
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the pod of %s, as the wait last read it: %s", issue.key, seen)
			r.t.Log(r.diagnose())
		}
	}()
	var quieted time.Time
	idleRigEventually(r.t, idleRigReleaseWait, "the pod of "+issue.key+" released with every claim of it but "+string(role)+"'s left as it was", func() bool {
		if time.Since(quieted) > 2*time.Second {
			quieted = time.Now()
			r.quietExcept(claim.Token(token))
		}
		if got := others(); !slices.Equal(got, held) {
			r.t.Fatalf("the claims of %s read\n  %s\nwant the ones it stood at\n  %s\nthis release stops no role and changes no claim",
				issue.key, strings.Join(got, "\n  "), strings.Join(held, "\n  "))
		}
		// The release takes the pod, its Sandbox and its disk, and the start that waited builds the
		// next pod at once: a poll that comes after both reads a pod standing, so a pod other than
		// before, the one the release took, is that release too.
		pod, sandbox, disk, uid := r.kube.pod(issue.pod), r.kube.sandbox(issue.pod) != nil, r.kube.disk(issue.pod), r.podUID(issue)
		seen = fmt.Sprintf("pod %t, sandbox %t, disk %t, uid %q", pod, sandbox, disk, uid)
		return !pod && !sandbox && !disk || uid != "" && uid != before
	})
	idleRigEventually(r.t, idleRigPodWait, "the "+string(role)+" of "+issue.key+" registered in a new pod", func() bool {
		uid := r.podUID(issue)
		return r.agents.role(issue.key, string(role)).Registered >= 1 && uid != "" && uid != before &&
			r.claimOf(issue, role) && idleRigWorking(r.api.claim(claim.Token(token)).State)
	})
}

// quietExcept is quiet for every claim of the project but token's: the claim a start makes while
// the test waits is left with the moment its own rows record.
func (r *idleWakeRig) quietExcept(token claim.Token) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update claims set updated_at = now() - interval '30 minutes'
		 where project = $1 and token <> $2`, r.project, string(token)); err != nil {
		r.t.Fatalf("back-date the project's claims but %s: %v", token, err)
	}
}

// A pod built before a role of the deployment is kept until main's rule for a pod with no live
// claim or the pod's update round ends it: the start of the role waits on its
// row and makes no claim, so no launch replaces the pod with the issue's disk under it.
//
//  1. A stopped role's work stays while the start waits: for three of the rig's ticks the pod
//     keeps its uid, no release is marked, the served issue names the checker in
//     startWaitsForPod, and no claim of the checker exists.
//  2. After the idle period, main's rule releases the pod, and the checker starts in a new pod.
//     That release has the pod's launcher read the disk first (R6 of #5285), so the pod is made
//     quiet only once its launcher is connected to the restarted daemon: a read made before then
//     fails, and a failed read keeps the pod for an hour (pods.DeclineUnreadRetry).
//  3. An operator's suspension keeps the pod past the idle period and is told once.
func TestAPodBuiltBeforeARoleIsKeptUntilMainsRuleOrItsRoundEndsIt(t *testing.T) {
	t.Parallel()
	waiting := func(t *testing.T, rig *idleWakeRig, issue idleRigIssue) {
		t.Helper()
		idleRigEventually(t, idleRigRecordWait, "the served issue "+issue.key+" naming the checker's start as waiting for its pod", func() bool {
			return slices.Contains(rig.startWaitsForPod(issue), string(podWaitRigRole))
		})
	}
	kept := func(t *testing.T, rig *idleWakeRig, issue idleRigIssue, uid string) {
		t.Helper()
		for tick := range 3 {
			time.Sleep(2 * idleRigTick)
			if now := rig.podUID(issue); now != uid {
				t.Errorf("tick %d: the pod of %s is %q, want the pod %q kept", tick, issue.key, now, uid)
			}
			if rig.releaseMarked(issue) {
				t.Errorf("tick %d: a release of %s's pod is marked, want none", tick, issue.key)
			}
			if names := rig.startWaitsForPod(issue); !slices.Contains(names, string(podWaitRigRole)) {
				t.Errorf("tick %d: the served issue %s names %v as waiting for its pod, want the checker", tick, issue.key, names)
			}
			if rig.claimOf(issue, podWaitRigRole) {
				t.Errorf("tick %d: a claim of the checker on %s exists, want none while its start waits", tick, issue.key)
			}
		}
	}

	t.Run("AStoppedRolesWorkStaysWhileTheStartWaits", func(t *testing.T) {
		rig := newIdleWakeRig(t, nil, "a")
		issue := rig.issue("a")
		rig.stopRole(issue)
		uid := rig.livePodUID(issue)
		rig.defineChecker(issue)
		rig.startChecker(issue)
		// The start's first try, whichever way it went, so the ticks below read what it did:
		// a try that waits leaves the row with its wait, one that starts the role takes it.
		idleRigEventually(t, idleRigRecordWait, "the first try of the checker's start on "+issue.key, func() bool {
			return rig.checkerStartTried(issue)
		})
		kept(t, rig, issue, uid)
	})

	t.Run("AfterTheIdlePeriodThePodIsReleasedAndTheRoleStartsInANewPod", func(t *testing.T) {
		rig := newIdleWakeRig(t, nil, "a")
		issue := rig.issue("a")
		rig.stopRole(issue)
		uid := rig.livePodUID(issue)
		connected := rig.launcherConnections(issue, uid)
		rig.defineChecker(issue)
		rig.startChecker(issue)
		waiting(t, rig, issue)
		idleRigEventually(t, idleRigRecordWait, "the launcher of "+issue.key+"'s pod connected to the restarted daemon", func() bool {
			return rig.launcherConnections(issue, uid) > connected
		})
		rig.quiet()
		rig.awaitUnownedReleasedThenStarted(issue, podWaitRigRole, uid)
		idleRigEventually(t, idleRigRecordWait, "the served issue "+issue.key+" naming no start waiting for its pod", func() bool {
			return len(rig.startWaitsForPod(issue)) == 0
		})
	})

	t.Run("AnOperatorsSuspensionKeepsThePodAndIsToldOnce", func(t *testing.T) {
		rig := newIdleWakeRig(t, nil, "a")
		issue := rig.issue("a")
		token := rig.token(issue)
		if status, body := rig.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil, true); status != http.StatusOK && status != http.StatusAccepted {
			t.Fatalf("the operator's suspension of %s = %d %s", token, status, body)
		}
		idleRigEventually(t, idleRigRecordWait, "the suspended implementer of "+issue.key+" recording no process", func() bool {
			suspended := rig.api.claim(token)
			return suspended.State == "suspended" && suspended.Locator == nil
		})
		uid := rig.livePodUID(issue)
		told := rig.countPodWaitHeld(issue)
		rig.defineChecker(issue)
		rig.startChecker(issue)
		waiting(t, rig, issue)
		rig.quiet()
		idleRigEventually(t, idleRigRecordWait, "the controller told of "+issue.key+"'s pod, which an operator's suspension holds", func() bool {
			rig.quiet()
			return told() >= 1
		})
		for tick := range 3 {
			rig.quiet()
			time.Sleep(2 * idleRigTick)
			if now := rig.podUID(issue); now != uid {
				t.Errorf("tick %d: the pod of %s is %q, want the pod %q the operator's suspension keeps", tick, issue.key, now, uid)
			}
			if rig.releaseMarked(issue) {
				t.Errorf("tick %d: a release of %s's pod is marked, want none while an operator holds a claim of it", tick, issue.key)
			}
		}
		if n := told(); n != 1 {
			t.Errorf("the controller was told %d times of %s's pod, want once", n, issue.key)
		}
	})
}
