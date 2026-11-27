package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// deploymentImplementer is the implementer of the rig's issue, LEGION-208.
var deploymentImplementer = claim.Token("legion-legion-legion-208-implementer")

// deploymentRig is the sandbox rig of a deployment that defines two roles of its own, the
// checker acting as the implement App and the critic as the review App, with the daemon's own gh
// answers built over that map (withProjectTokens reads Options.DeploymentApps, so the rig sets the
// map first).
func deploymentRig(t *testing.T) *rig {
	t.Helper()
	return newRig(t, nil,
		withOptions(func(o *Options) {
			o.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Implement, "critic": appauth.Review}
		}),
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)}))
}

// The runtime holds the roles of its deployment (Options.DeploymentApps) as a field of its own:
// an overlay of a role the deployment defines is taken, the image probe is told it, and two
// runtimes built in one process each keep their own list, so no package variable holds it.
func TestTheRuntimeTakesADeploymentRolesOverlays(t *testing.T) {
	withChecker := testOptions()
	withChecker.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Review}
	withChecker.RoleConfigFiles = map[claim.Role][]string{"checker": {"/etc/checker.yml"}}
	a, err := configure(withChecker)
	if err != nil {
		t.Fatalf("configure with the checker's overlays = %v, want no error", err)
	}
	if want := append(slices.Clone(claim.Roles), "checker"); !slices.Equal(a.issueRoles, want) {
		t.Errorf("the runtime's issue roles = %v, want %v", a.issueRoles, want)
	}
	command := containerNamed(t, a.probeManifest("legion-probe", ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Time{}).Spec.PodTemplate.Spec, probeContainer).Command
	at := slices.Index(command, "checker=/etc/checker.yml")
	if at < 1 || command[at-1] != bootprobe.RoleConfigFilesFlag {
		t.Errorf("the probe's command = %q, want %s checker=/etc/checker.yml", command, bootprobe.RoleConfigFilesFlag)
	}

	b, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(a.roleConfigRoles(), "checker") {
		t.Errorf("after a second runtime without the checker was built, the first runtime's roles = %v, want the checker among them", a.roleConfigRoles())
	}
	if slices.Contains(b.roleConfigRoles(), "checker") {
		t.Errorf("a runtime whose deployment defines no role has the roles %v, want no checker", b.roleConfigRoles())
	}
}

// The first launch of a role of an issue makes a pod that runs every role of the runtime: its
// launcher runs one --role for each role of the deployment too, and the Sandbox owns a role Secret
// for each of the eight, each holding the gh files of the App its role acts as (the render path
// writeLauncherSecrets, gitHubBundles, GitHubCredential, AppRoleFor).
func TestANewIssuePodBindsEachRoleOfTheDeploymentWithItsApp(t *testing.T) {
	g := deploymentRig(t)
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	name := loc.Sandbox.Name

	command := containerNamed(t, g.pod(name).Spec, runtime.IssuePodContainer).Command
	for _, role := range []string{"checker", "critic"} {
		at := slices.Index(command, role)
		if at < 1 || command[at-1] != "--role" {
			t.Errorf("the launcher's command = %q, want --role %s", command, role)
		}
	}

	every := append(slices.Clone(claim.Roles), "checker", "critic")
	var want, owned []string
	for _, role := range every {
		want = append(want, roleSecretName(name, role))
	}
	secrets, err := g.kube.CoreV1().Secrets(testNamespace).List(g.ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uid := g.sandbox(name).UID
	for _, secret := range secrets.Items {
		if _, launcher := secret.Data[LauncherTokenFile]; launcher && slices.ContainsFunc(secret.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == uid }) {
			owned = append(owned, secret.Name)
		}
	}
	slices.Sort(want)
	slices.Sort(owned)
	if !slices.Equal(owned, want) {
		t.Errorf("the Sandbox owns the role Secrets %q, want the eight %q", owned, want)
	}

	for role, prefix := range map[claim.Role]string{"checker": "ghs_implement_", "critic": "ghs_review_"} {
		secret := g.secret(roleSecretName(name, role))
		if secret == nil {
			continue
		}
		owner := string(secret.Data[GitHubDefaultKey])
		token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(owner)])
		if err != nil || !strings.HasPrefix(token, prefix) {
			t.Errorf("the %s Secret's %s hosts.yml holds %q (%v), want a token that begins %s", role, owner, token, err, prefix)
		}
	}
}

// The launcher of a role of the deployment is accepted on its issue's pod as one of Legion's is,
// and a role neither Legion nor the deployment defines is refused.
func TestALauncherOfADeploymentRoleIsAccepted(t *testing.T) {
	g := deploymentRig(t)
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	name := loc.Sandbox.Name
	secret := g.secret(roleSecretName(name, "checker"))
	if secret == nil {
		t.Fatalf("the pod has no checker Secret")
	}
	accept := g.r.LauncherResolver()
	hello := shimwire.LauncherHello{Token: string(secret.Data[LauncherTokenFile]), Sandbox: name, Role: "checker", PodUID: loc.Sandbox.PodUID, LauncherID: "l-checker"}
	if handler, _, reason := accept(hello); handler == nil {
		t.Errorf("the checker's launcher was refused: %s", reason)
	}
	ghost := hello
	ghost.Role = "ghost"
	if handler, _, reason := accept(ghost); handler != nil || reason != "launcher role is not one of its pod's roles" {
		t.Errorf("a launcher of ghost = %v, %q; want the refusal %q", handler != nil, reason, "launcher role is not one of its pod's roles")
	}
}

func TestALauncherOfADroppedRoleCanReconnectAndStop(t *testing.T) {
	g := deploymentRig(t)
	const checker claim.Role = "checker"
	token, err := claim.NewToken(testProject, testTree, checker)
	if err != nil {
		t.Fatal(err)
	}
	loc := g.spawn(testSpec(t, token, checker, testTree))
	logs := &lockedLog{}
	restarted := secondRuntime(t, g, func(o *Options) {
		*o = g.opts
		o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review}
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)})(g, o)
	})()
	if slices.Contains(restarted.issueRoles, checker) {
		t.Fatal("checker is still configured after restart")
	}

	hello := rigLauncherHello(g, loc.Sandbox.Name, loc.Sandbox.PodUID, checker)
	hello.Token += "-wrong"
	if handler, _, reason := restarted.LauncherResolver()(hello); handler != nil || reason != "launcher token is not current" {
		t.Errorf("dropped role with a wrong token: accepted=%t, refusal=%q", handler != nil, reason)
	}

	restarted.refreshGitHubCredentials(g.ctx)
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "github credential refresh failed") && strings.Contains(line, "role=checker") {
			t.Errorf("refresh tried the dropped role: %s", line)
		}
	}

	launcher := redial(t, g, token, loc.Sandbox.Generation, false)
	if err := restarted.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	if got, want := launcher.commands(), fmt.Sprintf("stop %d", loc.Sandbox.Generation); got != want {
		t.Errorf("dropped role's launcher received %q, want %q", got, want)
	}
}

// generationTokens is the daemon's lease source (appauth.Tokens) with a lease generation: each
// token names its App, the generation lease holds when it is minted, and its owner, so a slot the
// refresh re-minted holds the newer generation and a slot it passed over keeps the older.
type generationTokens struct{ lease *atomic.Pointer[string] }

func (t generationTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_" + *t.lease.Load() + "_" + strings.ToLower(owner), ExpiresAt: staticCredentialExpiry}, nil
}

func (t generationTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: "ghs_" + string(role) + "_" + *t.lease.Load() + "_read_" + strings.ToLower(owner), ExpiresAt: staticCredentialExpiry}, nil
}

// A slot in which a stored claim runs is refreshed only where the role the refresh would mint it
// for names an App (appauth.AppRoleFor), and no slot's name tells which. The daemon restarts with
// the checker taken out of its settings while a stored claim of the checker still runs in the
// issue's pod, beside a stored claim of the implementer in the slot of `implementer-2`, an agent
// of the plan whose name is no role. The tick asks no App for the checker, logs no refused
// refresh, and leaves the checker's slot holding the lease its build wrote, which expires without
// another mint, as a dropped role's slot in which no claim runs does. In the same tick the slot of
// implementer-2 is re-minted with the implement App's newer lease, as the slot `implementer` is.
// The two faults: a refresh of every slot a stored claim runs in asks the credential for the
// checker at every tick, which refuses it and logs a failure each time, for the pod's life; a
// refresh of the configured roles' slots alone leaves implementer-2 on the lease its build wrote,
// which expires within the hour while its claim works.
func TestTheRefreshPassesOverADroppedRolesClaimAndRefreshesAPlanAgentsClaim(t *testing.T) {
	const checker claim.Role = "checker"
	first, second := "first", "second"
	tokens := generationTokens{lease: new(atomic.Pointer[string])}
	tokens.lease.Store(&first)
	g := newRig(t, nil,
		withOptions(func(o *Options) {
			o.DeploymentApps = appauth.DeploymentApps{checker: appauth.Implement, "critic": appauth.Review}
		}),
		withProjectTokens(tokens),
		withPlanAgents(map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer}))
	checkerToken, err := claim.NewToken(testProject, testTree, checker)
	if err != nil {
		t.Fatal(err)
	}
	loc := g.spawn(testSpec(t, checkerToken, checker, testTree))
	name := loc.Sandbox.Name
	if planned := SandboxName(secondImplementer); planned != name {
		t.Fatalf("implementer-2's claim names the Sandbox %s, want the checker's %s: the case's premise", planned, name)
	}
	g.store.mu.Lock()
	g.store.claims = append(g.store.claims,
		supervise.Claim{Token: checkerToken, Issue: testTree, Role: checker, Generation: 1},
		supervise.Claim{Token: secondImplementer, Issue: testTree, Role: claim.RoleImplementer, Generation: 1})
	g.store.mu.Unlock()

	holds := func(slot claim.Role) string {
		t.Helper()
		return slotHostsToken(t, g, name, slot)
	}
	implement := func(lease string) string { return "ghs_" + string(appauth.Implement) + "_" + lease + "_sjawhar" }
	for _, slot := range []claim.Role{checker, "implementer-2", claim.RoleImplementer} {
		if got := holds(slot); got != implement(first) {
			t.Fatalf("the build wrote %q into the %s slot, want the implement App's %q: the case's premise", got, slot, implement(first))
		}
	}

	logs := &lockedLog{}
	asked := &askedRoles{}
	restarted := secondRuntime(t, g, func(o *Options) {
		*o = g.opts
		o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review}
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
		withProjectTokens(tokens)(g, o)
		asked.wrap(o)
	})()
	if slices.Contains(restarted.issueRoles, checker) {
		t.Fatal("the checker is still configured after the restart: the case's premise")
	}

	tokens.lease.Store(&second)
	restarted.refreshGitHubCredentials(g.ctx)

	if roles := asked.take(); slices.Contains(roles, checker) {
		t.Errorf("the tick asked the credential for the roles %v, want no ask for the checker, which the deployment no longer defines", roles)
	}
	if failed := linesHolding(logs.String(), "github credential refresh failed"); len(failed) > 0 {
		t.Errorf("the tick logged %q, want no refresh refused", failed)
	}
	if got := holds(checker); got != implement(first) {
		t.Errorf("the tick left %q in the checker's slot, want the lease its build wrote, %q", got, implement(first))
	}
	for _, slot := range []claim.Role{"implementer-2", claim.RoleImplementer} {
		if got := holds(slot); got != implement(second) {
			t.Errorf("the tick left %q in the %s slot, want the implement App's re-minted %q", got, slot, implement(second))
		}
	}
}

// slotHostsToken is the token the hosts.yml of the default owner of slot's Secret in the pod of
// sandbox holds; a slot with no Secret fails the test.
func slotHostsToken(t *testing.T, g *rig, sandbox string, slot claim.Role) string {
	t.Helper()
	secret := g.secret(roleSecretName(sandbox, slot))
	if secret == nil {
		t.Fatalf("the Sandbox has no Secret for the slot %s", slot)
	}
	token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(string(secret.Data[GitHubDefaultKey]))])
	if err != nil {
		t.Fatalf("the %s slot's default hosts.yml: %v", slot, err)
	}
	return token
}

// askedRoles records each role a runtime asks its credential function for (wrap).
type askedRoles struct {
	mu    sync.Mutex
	roles []claim.Role
}

// wrap makes o's credential function record each role it is asked for, then answer as it did.
func (a *askedRoles) wrap(o *Options) {
	credential := o.GitHubCredential
	o.GitHubCredential = func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		a.mu.Lock()
		a.roles = append(a.roles, role)
		a.mu.Unlock()
		return credential(ctx, role, repo)
	}
}

// take is every role asked for since the last take.
func (a *askedRoles) take() []claim.Role {
	a.mu.Lock()
	defer a.mu.Unlock()
	roles := a.roles
	a.roles = nil
	return roles
}

// An agent of the plan whose first step's role names no App — here the checker, which the
// deployment no longer defines, in a plan made while it did — gets no slot: a new pod of the issue
// builds the slots of the runtime's roles and no `checker` or `checker-2`, owns no Secret for
// either, and the implementer's launch into it starts. The update sweep reads the slots from the
// same rule (agentSlots), so the pod does not drift for the two it lacks, sweep after sweep. Once
// the deployment defines the checker again, the sweep does read `checker-2` as a slot the pod
// lacks (`checker` is then a role's slot, which the roles' own drift answers for), so the rule is
// of the role and the sweep is not blind to the slot. The fault: the build asks the App of the
// agent's first step, which the credential refuses, and every launch into a new pod of the issue
// fails while the plan names the agent.
func TestAnAgentWhoseFirstStepsRoleNamesNoAppGetsNoSlotInTheBuildOrTheSweep(t *testing.T) {
	const checker claim.Role = "checker"
	g := newRig(t, nil,
		withOptions(func(o *Options) { o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review} }),
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)}),
		withPlanAgents(map[claim.Agent]claim.Role{"implementer": claim.RoleImplementer, "checker": checker, "checker-2": checker}))
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	name := loc.Sandbox.Name
	built := templateRoles(g.sandbox(name))
	if want := append(slices.Clone(claim.Roles), "critic"); !slices.Equal(built, want) {
		t.Errorf("the pod's template binds the slots %v, want %v: no slot of an agent whose first step's role names no App", built, want)
	}
	for _, slot := range []claim.Role{checker, "checker-2"} {
		if g.secret(roleSecretName(name, slot)) != nil {
			t.Errorf("the Sandbox owns a Secret for the slot %s, whose agent's first step's role names no App", slot)
		}
	}
	for sweep := 1; sweep <= 2; sweep++ {
		if drifted := slotsDriftOf(t, g.ctx, g.r, loc); len(drifted) > 0 {
			t.Errorf("PodSlotsDrift at sweep %d = %q, want nothing: the sweep reads the slots the build makes", sweep, drifted)
		}
	}

	restarted := secondRuntime(t, g, func(o *Options) {
		*o = g.opts
		o.DeploymentApps = appauth.DeploymentApps{checker: appauth.Implement, "critic": appauth.Review}
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)})(g, o)
	})()
	want := "slots " + runtime.JoinRoles(built) + ", now " + runtime.JoinRoles(append(slices.Clone(restarted.issueRoles), "checker-2"))
	if drifted := slotsDriftOf(t, g.ctx, restarted, loc); !slices.Equal(drifted, []string{want}) {
		t.Errorf("PodSlotsDrift once the deployment defines the checker again = %q, want %q", drifted, []string{want})
	}
}

// A stored claim whose role names no App holds no slot in a pod's build, since it can never start:
// a keyed agent's claim held at a step of the checker, which the deployment no longer defines. Its
// slot is built as one no claim runs in, and the refresher passes over it as it passes over every
// slot whose claim's role names no App, so the lease the build wrote expires. Two places such a
// claim can stand, each with a fault of its own:
//   - B, the slot of an agent of the plan, `implementer-2`, whose first step is the implementer:
//     it holds the implement App, its first step's. This pull request's slots of a plan's agents
//     brought it.
//   - C, the slot of one of Legion's roles, `reviewer`: it holds the review App, its own name's.
//     The base built such a slot for its stored claim's role too.
//
// In each, the implementer's launch into a new pod starts, no role is asked of the credential that
// names no App, at the build or at the tick after it, and the tick logs no refusal, re-mints the
// implementer's slot and leaves this one on the lease its build wrote. The fault: the build asks
// the App of the stored claim's role, which the credential refuses, and every launch into a new
// pod of the issue fails while that claim is stored.
func TestABuildWritesTheSlotOfAClaimWhoseRoleNamesNoAppAsOneNoClaimRunsIn(t *testing.T) {
	const checker claim.Role = "checker"
	reviewer, err := claim.NewToken(testProject, testTree, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		agents map[claim.Agent]claim.Role
		stored claim.Token
		slot   claim.Role
		app    appauth.AppRole
	}{
		{name: "B, a plan agent's slot, holds its first step's App", agents: map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer},
			stored: secondImplementer, slot: "implementer-2", app: appauth.Implement},
		{name: "C, a role's slot, holds its own name's App", stored: reviewer, slot: claim.RoleReviewer, app: appauth.Review},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := "first", "second"
			tokens := generationTokens{lease: new(atomic.Pointer[string])}
			tokens.lease.Store(&first)
			logs := &lockedLog{}
			asked := &askedRoles{}
			g := newRig(t, nil,
				withOptions(func(o *Options) {
					o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review}
					o.Log = slog.New(slog.NewTextHandler(logs, nil))
				}),
				withProjectTokens(tokens),
				withOptions(asked.wrap),
				withPlanAgents(tc.agents))
			g.store.mu.Lock()
			g.store.claims = append(g.store.claims, supervise.Claim{Token: tc.stored, Issue: testTree, Role: checker, Generation: 1})
			g.store.mu.Unlock()
			loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
			name := loc.Sandbox.Name
			lease := func(app appauth.AppRole, generation string) string {
				return "ghs_" + string(app) + "_" + generation + "_sjawhar"
			}
			if roles := asked.take(); slices.Contains(roles, checker) {
				t.Errorf("the build asked the credential for the roles %v, want no ask for the checker", roles)
			}
			if got := slotHostsToken(t, g, name, tc.slot); got != lease(tc.app, first) {
				t.Fatalf("the build wrote %q into the %s slot, want %q", got, tc.slot, lease(tc.app, first))
			}

			tokens.lease.Store(&second)
			g.r.refreshGitHubCredentials(g.ctx)
			if roles := asked.take(); slices.Contains(roles, checker) {
				t.Errorf("the tick asked the credential for the roles %v, want no ask for the checker", roles)
			}
			if failed := linesHolding(logs.String(), "github credential refresh failed"); len(failed) > 0 {
				t.Errorf("the tick logged %q, want no refresh refused", failed)
			}
			if got := slotHostsToken(t, g, name, tc.slot); got != lease(tc.app, first) {
				t.Errorf("the tick left %q in the %s slot, want the lease its build wrote, %q", got, tc.slot, lease(tc.app, first))
			}
			if got := slotHostsToken(t, g, name, claim.RoleImplementer); got != lease(appauth.Implement, second) {
				t.Errorf("the tick left %q in the implementer's slot, want the re-minted %q: the tick wrote no slot", got, lease(appauth.Implement, second))
			}
		})
	}
}

// The launch of a claim whose role names no App is refused, naming that role, whatever the slot
// it runs in would hold were no claim in it: a keyed agent's claim at a step of the checker, which
// the deployment no longer defines, launched into the slot `reviewer` of a new pod. The build's
// rule for a stored claim that can never start (gitHubBundles) is never the launching claim's,
// whose process would otherwise start as the checker holding the review App's lease.
func TestALaunchOfAClaimWhoseRoleNamesNoAppIsRefusedNamingTheRole(t *testing.T) {
	const checker claim.Role = "checker"
	reviewer, err := claim.NewToken(testProject, testTree, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t, nil,
		withOptions(func(o *Options) { o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review} }),
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)}))
	g.launcher(reviewer)
	_, err = g.r.Spawn(g.ctx, testSpec(t, reviewer, checker, testTree))
	if err == nil || !strings.Contains(err.Error(), "the role checker is none of Legion's") {
		t.Fatalf("Spawn of %s as the checker = %v, want it refused naming the role checker", reviewer, err)
	}
	if secret := g.secret(roleSecretName(SandboxName(reviewer), claim.RoleReviewer)); secret != nil {
		t.Errorf("the refused launch wrote the Secret %s", secret.Name)
	}
}

// A line of a pod's log that a role of the deployment wrote is that role's, never the launcher's
// own, and the bound of a launch counts the calls of every role of the runtime's issue pods: a
// runtime with one role more than Legion's six has a bound longer by that role's calls.
func TestAPodsLogAndBoundCountItsDeploymentsRoles(t *testing.T) {
	roles := append(slices.Clone(claim.Roles), "checker")
	if got := containerLines("checker: a\nlauncher: b\n", 10, roles); got != "launcher: b" {
		t.Errorf("the launcher's own lines = %q, want %q", got, "launcher: b")
	}
	plain, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	deployed := testOptions()
	deployed.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Review}
	withChecker, err := configure(deployed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := withChecker.LaunchBound()-plain.LaunchBound(), time.Duration(relaunchRoleCalls)*apiTimeout; got != want {
		t.Errorf("the checker's runtime's launch bound exceeds the plain runtime's by %s, want %s (one role's calls)", got, want)
	}
}

// An operator's pod volume may not take the name of a volume Legion gives a role of the
// deployment, as it may not take one of Legion's own roles': the role's launcher volume in the
// pod would collide with it. Where no role has that name it is taken with no warning, since no
// agent of a plan takes the name of a role that does not exist. The slot volume of a numbered
// agent of the role, `launcher-checker-2`, is warned of while the role exists and taken with no
// warning once it does not (TestAnOperatorVolumeOfASlotsFormIsTakenAndOneOfARolesSlotRefused).
func TestAnOperatorVolumeMayNotTakeADeploymentRolesVolumeName(t *testing.T) {
	podOf := func(name string) Pod {
		return Pod{Volumes: []corev1.Volume{{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}
	}
	pod := podOf("launcher-checker")
	legion := append(slices.Clone(claim.Roles), claim.RoleController)
	withChecker := append(slices.Clone(legion), "checker")
	want := "runtime.kubernetes.pod.volumes[0].name launcher-checker is a volume Legion puts in every pod"
	if err := CheckPod(pod, withChecker, nil, testOptions().Tools, nil, nil, ompsessions.Store{}, NoExchange); err == nil || err.Error() != want {
		t.Errorf("CheckPod with the checker among the roles = %v, want %q", err, want)
	}
	if err := CheckPod(pod, legion, nil, testOptions().Tools, nil, nil, ompsessions.Store{}, NoExchange); err != nil {
		t.Errorf("CheckPod with no checker among the roles = %v, want it taken", err)
	}
	if warnings := PodVolumeWarnings(pod, legion); len(warnings) != 0 {
		t.Errorf("PodVolumeWarnings of launcher-checker with no checker among the roles = %q, want none: no agent takes the name checker", warnings)
	}
	numbered := podOf("launcher-checker-2")
	if warnings := PodVolumeWarnings(numbered, withChecker); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "runtime.kubernetes.pod.volumes[0].name launcher-checker-2 has the form launcher-<slot>") {
		t.Errorf("PodVolumeWarnings of launcher-checker-2 with the checker among the roles = %q, want the one warning of launcher-checker-2", warnings)
	}
	if warnings := PodVolumeWarnings(numbered, legion); len(warnings) != 0 {
		t.Errorf("PodVolumeWarnings of launcher-checker-2 with no checker among the roles = %q, want none: no agent takes the name checker-2", warnings)
	}
}

// The launcher credential a runtime holds for a role of the deployment's Secret goes with its
// Sandbox, as each of Legion's roles' does: no deleted Sandbox's role keeps a credential cached.
func TestTheLauncherCredentialOfADeploymentRoleIsForgottenWithItsSandbox(t *testing.T) {
	opts := testOptions()
	opts.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Review}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	name := SandboxName(deploymentImplementer)
	s := &sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-sandbox"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: roleSecretName(name, "checker")}, Data: map[string][]byte{LauncherTokenFile: []byte("token")}}
	r.cacheLauncherCredential(s, secret)
	if _, held := r.launcherAuth.lookup(secret.Name, s.UID); !held {
		t.Fatalf("the checker's credential was not cached")
	}
	r.launcherAuth.forgetSandbox(name)
	if _, held := r.launcherAuth.lookup(secret.Name, s.UID); held {
		t.Errorf("the checker's launcher credential is still held after its Sandbox %s was forgotten", name)
	}
}

// The release of an idle pod whose Sandbox keeps its volume deletes the Secrets the Sandbox owns,
// the role Secret of each role of the deployment among them, as it deletes each of Legion's: each
// holds a launcher token and the gh files of its App, which no pod reads once the pod is gone
// (deleteIssueSecrets over Runtime.issueRoles). The control is the same release with the loop over
// Legion's six alone, which leaves the checker's and the critic's Secrets behind.
func TestTheReleaseOfAKeptVolumeDeletesEachDeploymentRolesSecret(t *testing.T) {
	g := newRig(t, nil,
		withOptions(func(o *Options) {
			o.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Implement, "critic": appauth.Review}
			keptVolumes(SandboxMicroVM)(o)
		}),
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)}))
	g.spawn(childSpec(t))
	name := SandboxName(childToken)
	secrets := []string{secretName(name)}
	for _, role := range append(slices.Clone(claim.Roles), "checker", "critic") {
		secrets = append(secrets, roleSecretName(name, role))
	}
	for _, secret := range secrets {
		if g.secret(secret) == nil {
			t.Fatalf("the running pod's Sandbox has no Secret %s", secret)
		}
	}
	if released, err := g.r.ReleaseIssue(g.ctx, childIssue, testTree, authorized, nil); err != nil || !released {
		t.Fatalf("ReleaseIssue = %t, %v; want the running kept Sandbox released", released, err)
	}
	if s := g.sandbox(name); s == nil || !ownsKeptVolume(s) {
		t.Fatalf("after the release the issue's Sandbox is %+v, want it kept with its volume", s)
	}
	for _, secret := range secrets {
		if g.secret(secret) != nil {
			t.Errorf("the release left the Secret %s, want each Secret of the Sandbox deleted", secret)
		}
	}
}

// The release of a kept volume deletes the role Secret of each role the Sandbox's pod was built
// with, a role the deployment no longer defines among them: a daemon that restarted with settings
// that dropped the checker releases a pod built while the checker was defined, and the checker's
// Secret, which holds its launcher token and the gh files of its App, goes with the others
// (deleteIssueSecrets over the stored template's roles, builtRoles, as well as Runtime.issueRoles).
// The control is the loop over the runtime's roles alone, which leaves the checker's Secret behind.
func TestTheReleaseOfAKeptVolumeDeletesTheSecretOfARoleTheDeploymentLost(t *testing.T) {
	g := newRig(t, nil,
		withOptions(func(o *Options) {
			o.DeploymentApps = appauth.DeploymentApps{"checker": appauth.Implement, "critic": appauth.Review}
			keptVolumes(SandboxMicroVM)(o)
		}),
		withProjectTokens(refusingOwnerTokens{refuse: new(atomic.Bool)}))
	g.spawn(childSpec(t))
	name := SandboxName(childToken)
	checker := roleSecretName(name, "checker")
	if g.secret(checker) == nil {
		t.Fatalf("the running pod's Sandbox has no Secret %s", checker)
	}
	restarted := secondRuntime(t, g, func(o *Options) {
		*o = g.opts
		o.DeploymentApps = appauth.DeploymentApps{"critic": appauth.Review}
	})()
	if slices.Contains(restarted.issueRoles, "checker") {
		t.Fatalf("the restarted runtime's issue roles %v hold the checker, want it gone with the setting", restarted.issueRoles)
	}

	if released, err := restarted.ReleaseIssue(g.ctx, childIssue, testTree, authorized, nil); err != nil || !released {
		t.Fatalf("ReleaseIssue = %t, %v; want the running kept Sandbox released", released, err)
	}

	if s := g.sandbox(name); s == nil || !ownsKeptVolume(s) {
		t.Fatalf("after the release the issue's Sandbox is %+v, want it kept with its volume", s)
	}
	if g.secret(checker) != nil {
		t.Errorf("the release left the Secret %s of the checker, a role the pod was built with, want it deleted", checker)
	}
}
