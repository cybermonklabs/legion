package sandbox

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// checkerToken is the claim of a role the deployment defines, checker, on the rig's issue.
var checkerToken = claim.Token("legion-legion-legion-208-checker")

// withChecker is a deployment that defines the role checker, acting as the review App.
func withChecker(o *Options) {
	o.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Review}
}

// podBuiltBeforeChecker is the rig's issue pod built by a runtime whose deployment defines no
// role, with the implementer launched in it and then suspended, so no claim records a process
// in the pod; then the daemon restarts with the role checker defined (secondRuntime, which makes
// the restarted runtime the rig's). It answers the implementer's locator.
func podBuiltBeforeChecker(t *testing.T, g *rig) runtime.Locator {
	t.Helper()
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	secondRuntime(t, g, withChecker)()
	return loc
}

// A launch into a live pod that lacks the slot of its claim or the role of its step, both
// `checker` here (a claim named for its role), writes nothing and waits: it neither replaces the
// pod nor suspends its Sandbox, since that would delete the issue's disk with everything a role
// did not push, and only the pod's update round or its release replaces it (ruling 18). The
// runtime answers which issue's pod lacks which name.
func TestALaunchOfARoleItsPodWasBuiltWithoutWritesNothingAndWaits(t *testing.T) {
	g := newRig(t, nil)
	loc := podBuiltBeforeChecker(t, g)
	g.clearActions()

	_, err := g.r.Spawn(g.ctx, testSpec(t, checkerToken, "checker", testTree))
	if !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Errorf("the checker's launch = %v, want an error that is runtime.ErrAwaitsPodUpdate", err)
	}
	if writes := g.writes(); len(writes) > 0 {
		t.Errorf("the checker's launch wrote %v, want nothing written", writes)
	}
	pod := g.pod(loc.Sandbox.Name)
	if pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
		t.Errorf("the issue's pod is %v, want the pod %s kept", pod, loc.Sandbox.PodUID)
	}
	if s := g.sandbox(loc.Sandbox.Name); s == nil || s.mode() == modeSuspended {
		t.Errorf("the issue's Sandbox is %v, want it kept running", s)
	}
	if uid, lacks := g.r.PodLacksRole(testTree, "checker"); !lacks || uid != loc.Sandbox.PodUID {
		t.Errorf("PodLacksRole(checker) = %q, %v; want %q, true", uid, lacks, loc.Sandbox.PodUID)
	}
	if uid, lacks := g.r.PodLacksRole(testTree, claim.RoleTester); lacks {
		t.Errorf("PodLacksRole(tester) = %q, true; want false: the pod was built with the tester", uid)
	}
}

// One of Legion's roles, in its own claim's slot, still launches into a pod built before a role of
// the deployment: the pod is judged by the slots and roles it was built with, so the missing
// Secret of the deployment's role is no reason to replace it.
func TestABuiltinRoleLaunchesIntoAPodBuiltBeforeADeploymentRole(t *testing.T) {
	g := newRig(t, nil)
	loc := podBuiltBeforeChecker(t, g)

	tester := g.spawn(workerSpec(t))
	if tester.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Errorf("the tester was launched into the pod %s, want the kept pod %s", tester.Sandbox.PodUID, loc.Sandbox.PodUID)
	}
}

// A pod built before a role of the deployment drifts on its roles, whether or not its template
// records the configuration it was built from, so the update round opens over it; a pod built
// with that role names no such reason.
func TestAPodBuiltBeforeARoleDriftsOnItsRoles(t *testing.T) {
	want := []string{"roles architect, planner, implementer, tester, reviewer, merger, now architect, planner, implementer, tester, reviewer, merger, checker"}
	g := newRig(t, nil)
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	restarted := secondRuntime(t, g, withChecker)()
	if drifted := driftOf(t, g, restarted, loc); !slices.Equal(drifted, want) {
		t.Errorf("PodDrift = %q, want %q", drifted, want)
	}
	forgetPodShape(t, g, restarted, loc.Sandbox.Name)
	if drifted := driftOf(t, g, restarted, loc); !slices.Equal(drifted, want) {
		t.Errorf("PodDrift of a pod written before the shape record = %q, want %q", drifted, want)
	}

	built := deploymentRig(t)
	withRole := built.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	for _, reason := range driftOf(t, built, built.r, withRole) {
		if strings.HasPrefix(reason, "roles ") {
			t.Errorf("PodDrift of a pod built with every role names %q, want no roles reason", reason)
		}
	}
}

// A launch the supervisor makes itself — an operator's resume of a deployment role's claim, which
// reaches the runtime with no outbox row to wait on — meets the same refusal through
// Resume, and its text names the pod and the role, so the operator who reads the claim's last
// start failure can tell which pod holds the role back.
func TestALaunchTheSupervisorMakesIntoAPodBuiltWithoutItsRoleIsRefusedNamingThePodAndTheRole(t *testing.T) {
	g := newRig(t, nil)
	loc := podBuiltBeforeChecker(t, g)
	g.clearActions()

	spec := testSpec(t, checkerToken, "checker", testTree)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-checker-g2", resumeSession
	_, err := g.r.Resume(g.ctx, nil, spec)
	if !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Errorf("the checker's resume = %v, want an error that is runtime.ErrAwaitsPodUpdate", err)
	}
	if err == nil || !strings.Contains(err.Error(), loc.Sandbox.PodUID) || !strings.Contains(err.Error(), "checker") {
		t.Errorf("the checker's resume = %v, want its text to name the pod %s and the checker", err, loc.Sandbox.PodUID)
	}
	if writes := g.writes(); len(writes) > 0 {
		t.Errorf("the checker's resume wrote %v, want nothing written", writes)
	}
	if pod := g.pod(loc.Sandbox.Name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
		t.Errorf("the issue's pod is %v, want the pod %s kept", pod, loc.Sandbox.PodUID)
	}
}

// A claim runs in the slot of its agent (slotOf) and on the role of its step, and a launch into a
// pod the runtime keeps needs both of that pod: the slot's launcher, Secret and volumes, and the
// role's own resources, its role_config_files overlays among them. A keyed agent's slot and its
// step's role can be two names, so a launch into a pod built before a role of the deployment waits
// where the pod lacks either, its text naming the one it lacks, and goes on where it holds both.
func TestALaunchIntoAKeptPodWaitsForTheSlotOfItsClaimAndTheRoleOfItsStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token claim.Token
		role  claim.Role
		// lacks is what the refusal names, "" for a launch that goes on.
		lacks string
	}{
		{"the pod lacks the slot of the claim's agent", checkerToken, claim.RoleTester, "the slot checker"},
		{"the pod lacks the role of the step", workerToken, "checker", "the role checker"},
		{"the pod holds the slot and the role", workerToken, claim.RoleReviewer, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := podBuiltBeforeChecker(t, g)
			g.clearActions()
			spec := testSpec(t, tc.token, tc.role, testTree)
			if tc.lacks == "" {
				if launched := g.spawn(spec); launched.Sandbox.PodUID != loc.Sandbox.PodUID {
					t.Errorf("the launch went into the pod %s, want the kept pod %s", launched.Sandbox.PodUID, loc.Sandbox.PodUID)
				}
				return
			}
			_, err := g.r.Spawn(g.ctx, spec)
			if !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
				t.Errorf("the launch = %v, want an error that is runtime.ErrAwaitsPodUpdate", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.lacks) || !strings.Contains(err.Error(), loc.Sandbox.PodUID) {
				t.Errorf("the launch = %v, want its text to name %s and the pod %s", err, tc.lacks, loc.Sandbox.PodUID)
			}
			if writes := g.writes(); len(writes) > 0 {
				t.Errorf("the launch wrote %v, want nothing written", writes)
			}
			if pod := g.pod(loc.Sandbox.Name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
				t.Errorf("the issue's pod is %v, want the pod %s kept", pod, loc.Sandbox.PodUID)
			}
		})
	}
}

// The gh refresh of a pod built before a role of the deployment reads the Secrets of the roles the
// pod was built with, its own --role values, and never the Secret the pod has no role of: that
// read would fail at every tick and log a failed refresh for a role no process of the pod runs.
func TestTheRefreshOfAPodBuiltBeforeARoleReadsNoSecretOfThatRole(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	restarted := secondRuntime(t, g, withChecker)()
	g.clearActions()
	restarted.refreshGitHubCredentials(g.ctx)
	read := map[string]bool{}
	for _, a := range g.requests() {
		if a.verb == "get" && a.resource == "secrets" {
			read[a.name] = true
		}
	}
	if !read[roleSecretName(loc.Sandbox.Name, claim.RoleImplementer)] {
		t.Fatalf("the refresh read the Secrets %v, want the implementer's among them: the pod was built with it", read)
	}
	if checker := roleSecretName(loc.Sandbox.Name, "checker"); read[checker] {
		t.Errorf("the refresh read %s, want no read of a Secret of a role the pod was built without", checker)
	}
}

// A pod the Agent Sandbox controller recreated from a template built before a role of the
// deployment is bound at its launchers' hello as any recreated pod is: the binding writes the
// Secret of each role the pod was built with (templateRoles) and asks for none of the role it was
// built without, whose Secret no launch wrote, so the hello is accepted and the next launch keeps
// the pod. The control is the binding that asked for every role of the runtime now, the checker's
// among them, which failed on that Secret at every hello.
func TestAPodRecreatedFromATemplateBuiltBeforeARoleIsBoundAtItsLaunchersHello(t *testing.T) {
	g := newRig(t, nil)
	first := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	secondRuntime(t, g, withChecker)()
	name := first.Sandbox.Name
	recreated := g.recreatePod(name, first.Sandbox.PodUID)

	if handler, _, reason := g.r.LauncherResolver()(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler == nil {
		t.Fatalf("the tester launcher of the recreated pod %s was refused: %s", recreated, reason)
	}
	wantBound(t, g, name, recreated, claim.Roles...)
	if checker := roleSecretName(name, "checker"); g.secret(checker) != nil {
		t.Errorf("the binding wrote %s, a Secret of a role the pod was built without", checker)
	}
}
