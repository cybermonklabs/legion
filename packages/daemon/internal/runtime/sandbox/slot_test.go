package sandbox

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An operator's pod volume may not take the name of a volume Legion gives the slot of a role that
// exists now: one of Legion's six, the controller's, or a role the deployment defines. A name of
// the form of a slot's volume, `<launcher, private, state, gh or agent-secrets-key>-<agent>`, where
// the agent is a name an issue's plan can give beside its role's own, such as `implementer-2`,
// collides with the slot of that agent wherever a plan names it, which no check at the daemon's
// boot can see, since no plan is known then. A plan names an agent `<role>-<n>`, from 2, the role
// cut so the name holds at most 32 bytes (claim.NumberedAgentName), for a role that exists now. A
// configuration saved before such agents had slots may hold such a name, so this release takes it
// with a warning that names the volume and the form and says a later release refuses it
// (PodVolumeWarnings). A name no agent can take, such as `gh-cache`, stays the operator's, with no
// warning.
func TestAnOperatorVolumeOfASlotsFormIsTakenAndOneOfARolesSlotRefused(t *testing.T) {
	// long is a role the deployment defines, of the grammar's full 32 bytes: its numbered agents
	// take the role cut to fit.
	long := strings.Repeat("r", 32)
	roles := append(slices.Clone(claim.Roles), claim.Role(long), claim.RoleController)
	podOf := func(name string) Pod {
		return Pod{Volumes: []corev1.Volume{{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}
	}
	check := func(name string) error {
		return CheckPod(podOf(name), roles, nil, testOptions().Tools, nil, nil, ompsessions.Store{}, NoExchange)
	}
	for _, name := range []string{"launcher-implementer", "private-tester", "state-controller", "gh-architect", "agent-secrets-key-merger", "state-" + long} {
		want := "runtime.kubernetes.pod.volumes[0].name " + name + " is a volume Legion puts in every pod"
		if err := check(name); err == nil || err.Error() != want {
			t.Errorf("CheckPod of an operator volume %s = %v, want %q", name, err, want)
		}
	}
	for name, slot := range map[string][2]string{
		"state-implementer-2": {"state", "implementer-2"}, "private-tester-10": {"private", "tester-10"},
		"agent-secrets-key-merger-3": {"agent-secrets-key", "merger-3"},
		"gh-" + long[:30] + "-2":     {"gh", long[:30] + "-2"}, "launcher-" + long[:29] + "-10": {"launcher", long[:29] + "-10"},
	} {
		if err := check(name); err != nil {
			t.Errorf("CheckPod of an operator volume %s = %v, want it taken: it collides with no slot of a role", name, err)
		}
		want := []string{"runtime.kubernetes.pod.volumes[0].name " + name + " has the form " + slot[0] + "-<slot> of a volume Legion gives each slot of a pod, and collides with the " +
			slot[0] + " volume of the slot of an agent named " + slot[1] + " wherever an issue's plan names one; a later release refuses this name, so rename the volume"}
		if got := PodVolumeWarnings(podOf(name), roles); !slices.Equal(got, want) {
			t.Errorf("PodVolumeWarnings of an operator volume %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{
		"gh-cache", "private-a", "launcher-security-reviewer", "agent-secrets-key-checker", "state-cache-2",
		"state-implementer-1", "state-implementer-02", "state-controller-2", "gh-" + long[:31] + "-2",
		"operator-extra", "state", "statefile", "state-2", "gh-", "launchers-a", "launcher-implementer",
	} {
		if got := PodVolumeWarnings(podOf(name), roles); len(got) != 0 {
			t.Errorf("PodVolumeWarnings of an operator volume %s = %q, want none: no agent of a plan takes its name, or CheckPod refuses it", name, got)
		}
		if name == "launcher-implementer" {
			continue
		}
		if err := check(name); err != nil {
			t.Errorf("CheckPod of an operator volume %s = %v, want it taken: it collides with no slot of a role", name, err)
		}
	}
}

// secondImplementer is the agent of a plan's second implementer step on the rig's issue: a name
// that is no role of the runtime.
var secondImplementer = claim.Token("legion-legion-legion-208-implementer-2")

// withPlanAgents is a store whose plan in force of the rig's issue names agents, each with the role
// of its first step (Store.PlanAgents).
func withPlanAgents(agents map[claim.Agent]claim.Role) rigOption {
	return func(_ *rig, o *Options) { o.Store.(*fakeStore).plan(testTree, agents) }
}

// An issue pod has a slot for each role of the runtime and for each agent name of the issue's
// plan that no role has: the first launch builds `--role implementer-2` with that slot's launcher,
// gh, private and state volumes and its Secret, which holds the App of the agent's first step, and a
// hello of that slot is taken as the claim `…-implementer-2`. A plan whose names are all roles
// builds the pod a plan of no agents builds, byte for byte.
func TestAnIssuePodHasASlotForEachAgentNameOfItsPlan(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{
		"implementer": claim.RoleImplementer, "reviewer": claim.RoleReviewer, "implementer-2": claim.RoleImplementer,
	}))
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	name := loc.Sandbox.Name
	pod := g.pod(name)
	if pod == nil {
		t.Fatalf("the launch built no pod %s", name)
	}
	command := containerNamed(t, pod.Spec, runtime.IssuePodContainer).Command
	if got, want := builtRoles(pod.Spec), append(slices.Clone(claim.Roles), "implementer-2"); !slices.Equal(got, want) {
		t.Errorf("the pod's slots = %v, want %v (the launcher's command %q)", got, want, command)
	}
	for _, volume := range []string{roleVolume("launcher", "implementer-2"), roleVolume("private", "implementer-2"),
		roleVolume(stateVolume, "implementer-2"), roleVolume(ghVolume, "implementer-2")} {
		if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == volume }) {
			t.Errorf("the pod has no volume %s", volume)
		}
	}
	secret := g.secret(roleSecretName(name, "implementer-2"))
	if secret == nil {
		t.Fatalf("the Sandbox has no Secret %s", roleSecretName(name, "implementer-2"))
	}
	if !slices.ContainsFunc(secret.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == g.sandbox(name).UID }) {
		t.Errorf("the Secret %s is not the Sandbox's", secret.Name)
	}
	owner := string(secret.Data[GitHubDefaultKey])
	if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(owner)]); err != nil || token != staticCredentialToken(claim.RoleImplementer) {
		t.Errorf("the implementer-2 Secret's %s hosts.yml holds %q (%v), want the implement App's %q", owner, token, err, staticCredentialToken(claim.RoleImplementer))
	}
	hello := shimwire.LauncherHello{Token: string(secret.Data[LauncherTokenFile]), Sandbox: name, Role: "implementer-2", PodUID: loc.Sandbox.PodUID, LauncherID: "l-implementer-2"}
	if handler, _, reason := g.r.LauncherResolver()(hello); handler == nil {
		t.Errorf("the launcher of the slot implementer-2 was refused: %s", reason)
	}
	g.r.launchers.mu.Lock()
	_, accepted := g.r.launchers.sessions[secondImplementer]
	g.r.launchers.mu.Unlock()
	if !accepted {
		t.Errorf("the launcher of the slot implementer-2 is not held as %s", secondImplementer)
	}

	plain := newRig(t, nil)
	plainPod := plain.pod(plain.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree)).Sandbox.Name)
	roles := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"implementer": claim.RoleImplementer, "reviewer": claim.RoleReviewer}))
	rolesPod := roles.pod(roles.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree)).Sandbox.Name)
	if !reflect.DeepEqual(plainPod.Spec, rolesPod.Spec) {
		t.Errorf("a plan whose agents are all roles built the pod\n%+v\nwant the pod a plan of no agents builds\n%+v", rolesPod.Spec, plainPod.Spec)
	}
}

// The gh refresh passes over the slot of an agent of the plan in which no stored claim runs yet: its
// name is no role's, so no App names it, and nothing in it reads its files. The slot keeps the App
// the pod's build wrote into it, that of the agent's first step, through a tick that re-mints every
// other slot's lease. Once a claim of that agent is stored, the refresh renders its slot for that
// claim's role as it does every other slot. The daemon's own function refuses a role that names no
// App (appauth.AppRoleFor), so a refresh that asked for the slot's own name would write nothing
// there and log a failure at each tick; the stand-in here renders a bundle for any role it is asked
// for, named by that role, so such a refresh would show in the slot's files.
func TestTheRefreshOfAPlanSlotWaitsForAClaimOfItsAgent(t *testing.T) {
	var lease atomic.Pointer[string]
	first, second := "first", "second"
	lease.Store(&first)
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer}), withOptions(func(o *Options) {
		o.GitHubCredential = appauth.CredentialOf(func(_ context.Context, _ string, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			app := string(role)
			if acting, err := appauth.AppRoleFor(role, nil, appauth.Planned{}); err == nil {
				app = string(acting)
			}
			owner := strings.ToLower(repo.Owner())
			if owner == "" {
				owner = "example-org"
			}
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
				"acme": ghconfig.Render("ghs_"+app+"_"+*lease.Load()+"_read", app, staticCredentialExpiry),
				owner:  ghconfig.Render("ghs_"+app+"_"+*lease.Load(), app, staticCredentialExpiry),
			}}, nil
		})
	}))
	loc := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	slot := roleSecretName(loc.Sandbox.Name, "implementer-2")
	implementer := roleSecretName(loc.Sandbox.Name, claim.RoleImplementer)
	implement := func(lease string) string { return "ghs_" + string(appauth.Implement) + "_" + lease }
	holds := func(name string) string {
		t.Helper()
		secret := g.secret(name)
		if secret == nil {
			t.Fatalf("the Sandbox has no Secret %s", name)
		}
		token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(string(secret.Data[GitHubDefaultKey]))])
		if err != nil {
			t.Fatalf("the Secret %s's default hosts.yml: %v", name, err)
		}
		return token
	}
	readSecrets := func() map[string]bool {
		read := map[string]bool{}
		for _, a := range g.requests() {
			if a.verb == "get" && a.resource == "secrets" {
				read[a.name] = true
			}
		}
		return read
	}
	if got := holds(slot); got != implement(first) {
		t.Fatalf("the build wrote %q into %s, want the implement App's %q, the App of the agent's first step", got, slot, implement(first))
	}

	lease.Store(&second)
	g.clearActions()
	g.r.refreshGitHubCredentials(g.ctx)
	read := readSecrets()
	if !read[implementer] {
		t.Fatalf("the refresh read the Secrets %v, want the implementer's among them", read)
	}
	if got := holds(implementer); got != implement(second) {
		t.Fatalf("the refresh left %q in %s, want the re-minted %q: the tick wrote no slot", got, implementer, implement(second))
	}
	if read[slot] {
		t.Errorf("the refresh read %s, want no read of the slot of an agent no stored claim is of", slot)
	}
	if got := holds(slot); got != implement(first) {
		t.Errorf("the refresh left %q in %s, want the implement App's %q the build wrote: no stored claim runs in the slot", got, slot, implement(first))
	}

	g.store.mu.Lock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: secondImplementer, Issue: testTree, Role: claim.RoleImplementer, Generation: 1})
	g.store.mu.Unlock()
	g.clearActions()
	g.r.refreshGitHubCredentials(g.ctx)
	if !readSecrets()[slot] {
		t.Errorf("the refresh after a claim of implementer-2 was stored read %v, want %s among them", readSecrets(), slot)
	}
	if got := holds(slot); got != implement(second) {
		t.Errorf("the refresh after a claim of implementer-2 was stored left %q in %s, want the implement App's re-minted %q", got, slot, implement(second))
	}
}

// A slot in which no stored claim runs holds the App of its own name only where that name is a
// role the runtime writes a slot for with no claim: one of its issue roles, such as `implementer`,
// or the controller. The slot of an agent of the plan whose name is no role, such as
// `implementer-2`, names no App, so its role is refused, and the error names the slot. Once a
// claim of that agent is stored, the slot holds the App of the claim's role, as runtime.SlotRole
// gives it: the implement App's for a claim of the implementer.
func TestASlotWithNoClaimHoldsTheAppOfItsNameOnlyWhereItsNameIsARole(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{"implementer-2": claim.RoleImplementer}))
	sandbox := SandboxName(secondImplementer)
	held := map[slotKey]slotHolder{}
	if role, err := g.r.slotAppRole(g.ctx, held, sandbox, "implementer-2", ""); err == nil || !strings.Contains(err.Error(), "implementer-2") {
		t.Errorf("the slot implementer-2, in which no stored claim runs, holds the App of %q (err %v), want it refused with an error that names the slot", role, err)
	}
	for _, own := range []claim.Role{claim.RoleImplementer, claim.RoleController} {
		if role, err := g.r.slotAppRole(g.ctx, held, sandbox, claim.Agent(own), ""); err != nil || role != own {
			t.Errorf("the slot %s, in which no stored claim runs, holds the App of %q (err %v), want its own name's", own, role, err)
		}
	}

	held[slotKey{sandbox: sandbox, slot: "implementer-2"}] = slotHolder{token: secondImplementer, role: claim.RoleImplementer, generation: 1}
	want, err := runtime.SlotRole(g.ctx, g.r.actsAs, secondImplementer, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("runtime.SlotRole of %s: %v", secondImplementer, err)
	}
	role, err := g.r.slotAppRole(g.ctx, held, sandbox, "implementer-2", "")
	if err != nil || role != want {
		t.Fatalf("the slot implementer-2, in which a claim of the implementer runs, holds the App of %q (err %v), want %q", role, err, want)
	}
	if app, err := g.r.roleApps.Of(g.ctx, childIssue, role); err != nil || app != appauth.Implement {
		t.Errorf("the role %q of the slot implementer-2 acts as the App %q (err %v), want the implement App", role, app, err)
	}
}

// The launcher credential a runtime holds for the slot of an agent of the plan goes with its
// Sandbox, as each role's does, and the Secret of another Sandbox whose name begins with the
// forgotten one's stays held.
func TestTheLauncherCredentialOfAPlanSlotIsForgottenWithItsSandbox(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	name := SandboxName(secondImplementer)
	s := &sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-sandbox"}}
	other := &sandbox{ObjectMeta: metav1.ObjectMeta{Name: name + "1", UID: "uid-other"}}
	held := map[*sandbox]*corev1.Secret{
		s:     {ObjectMeta: metav1.ObjectMeta{Name: roleSecretName(name, "implementer-2")}, Data: map[string][]byte{LauncherTokenFile: []byte("token")}},
		other: {ObjectMeta: metav1.ObjectMeta{Name: roleSecretName(other.Name, claim.RoleTester)}, Data: map[string][]byte{LauncherTokenFile: []byte("other")}},
	}
	for sandbox, secret := range held {
		r.cacheLauncherCredential(sandbox, secret)
	}
	r.launcherAuth.forgetSandbox(name)
	if _, kept := r.launcherAuth.lookup(held[s].Name, s.UID); kept {
		t.Errorf("the implementer-2 launcher credential is still held after its Sandbox %s was forgotten", name)
	}
	if _, kept := r.launcherAuth.lookup(held[other].Name, other.UID); !kept {
		t.Errorf("the credential of %s was dropped with the Sandbox %s", held[other].Name, name)
	}
}

// The slot of a claim's process is its agent's name, which can be no role (`implementer-2`), and
// the controller's for the controller's token. Such a claim's locator is an issue pod's like any
// other claim of the issue, and its observation reads its own slot: the address record of its
// generation is under its name, never under a role read off the token, so an address that moved
// since that generation started is observed.
func TestALocatorOfANameThatIsNoRoleIsAnIssuePodsAndIsObserved(t *testing.T) {
	issue, _, ok := workerToken.Cut()
	if !ok {
		t.Fatalf("Cut(%s) names no issue", workerToken)
	}
	agent := claim.Token(issue + "-implementer-2")
	for token, want := range map[claim.Token]claim.Agent{
		agent:           "implementer-2",
		workerToken:     claim.Agent(claim.RoleTester),
		controllerToken: claim.Agent(claim.RoleController),
	} {
		if got, ok := token.Slot(); !ok || got != want {
			t.Errorf("%s.Slot() = %q, %t; want %q", token, got, ok, want)
		}
	}
	slot, _ := agent.Slot()
	if _, issuePod := podKindFor(launcherOf(slot)).(issuePod); !issuePod {
		t.Errorf("podKindFor(launcherOf(%s)) = %T, want the issue pod", slot, podKindFor(launcherOf(slot)))
	}
	loc := sandboxLocator(agent, recorded)
	if err := loc.Validate(); err != nil {
		t.Fatalf("validate the locator of %s: %v", agent, err)
	}

	name := SandboxName(agent)
	labels := claimLabels(claim.RoleImplementer)
	opts := testOptions()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	containers := podOf(t, r, workerSpec(t)).Containers
	moved := testOptions()
	moved.DaemonURL = movedDaemonURL
	earlier, err := configure(moved)
	if err != nil {
		t.Fatal(err)
	}
	// recordOf is slot's record of generation 1 as a runtime under the moved daemon address wrote
	// it when that generation started.
	recordOf := func(slot claim.Role) string {
		record, err := earlier.recordFor(slot, 1, earlier.enrollsIn(nil, slot), earlier.dispatch.Bearer != "")
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	running := corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: workerContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
	objectsWith := func(records map[string]string) []k8sruntime.Object {
		s := sandboxObject(t, name, sandboxUID, modeRunning, labels)
		s.SetAnnotations(records)
		pod := podObject(name, recorded, sandboxUID, labels, running)
		pod.Spec.Containers = containers
		return []k8sruntime.Object{s, pod}
	}
	for _, tc := range []struct {
		row     string
		objects []k8sruntime.Object
		want    runtime.ObservationKind
		detail  string
	}{
		{
			row:     "the record under its own slot names the moved address",
			objects: objectsWith(map[string]string{addressesAnnotation("implementer-2"): recordOf("implementer-2")}),
			want:    runtime.StaleAddress,
			detail:  movedAddress{"LEGION_DAEMON_URL", movedDaemonURL, opts.DaemonURL}.String(),
		},
		{
			row:     "another slot's record is not its own",
			objects: objectsWith(map[string]string{addressesAnnotation(claim.RoleTester): recordOf(claim.RoleTester)}),
			want:    runtime.Alive,
			detail:  "runs generation 1",
		},
	} {
		t.Run(tc.row, func(t *testing.T) {
			g := newRig(t, tc.objects, withoutController())
			g.reportLauncher(agent, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
			obs, err := g.r.Probe(g.ctx, loc)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != tc.want {
				t.Fatalf("kind %s, want %s (detail: %s)", obs.Kind, tc.want, obs.Detail)
			}
			if !strings.Contains(obs.Detail, tc.detail) {
				t.Errorf("detail %q lacks %q", obs.Detail, tc.detail)
			}
		})
	}
}

// Two agents of one role run in one issue pod at one time, each in the slot of its own name: a
// launch of `…-implementer` while `…-implementer-2` runs keeps the pod, starts in the slot
// `implementer` with that slot's state home and address record, and sends no stop to the
// launcher of `…-implementer-2`, whose generation goes on; each slot's Secret holds the implement
// App's token. A slot picked by the role would put both agents in one slot, where the second's
// start takes the first's state home and address record from under it.
func TestTwoAgentsOfOneRoleRunAtOneTimeInTwoSlots(t *testing.T) {
	g := newRig(t, nil, withPlanAgents(map[claim.Agent]claim.Role{
		"implementer": claim.RoleImplementer, "implementer-2": claim.RoleImplementer,
	}))
	first := g.spawn(testSpec(t, secondImplementer, claim.RoleImplementer, testTree))
	second := g.spawn(testSpec(t, deploymentImplementer, claim.RoleImplementer, testTree))
	if second.Sandbox.PodUID != first.Sandbox.PodUID {
		t.Fatalf("the launch of %s ran in pod %s, want the pod %s in which %s runs", deploymentImplementer, second.Sandbox.PodUID, first.Sandbox.PodUID, secondImplementer)
	}
	name := first.Sandbox.Name
	for _, frame := range g.sent(secondImplementer) {
		if stop, ok := frame.(shimwire.LauncherStop); ok {
			t.Errorf("the launcher of %s was sent a stop of generation %d, want its generation left running", secondImplementer, stop.Generation)
		}
	}
	s := g.sandbox(name)
	if s == nil {
		t.Fatalf("the Sandbox %s is gone", name)
	}
	for token, slot := range map[claim.Token]claim.Role{secondImplementer: "implementer-2", deploymentImplementer: claim.RoleImplementer} {
		if got, want := startEnv(lastStart(t, g, token))["XDG_STATE_HOME"], roleStateHome(claim.Agent(slot)); got != want {
			t.Errorf("the start of %s names the state home %q, want the slot %s's %q", token, got, slot, want)
		}
		if _, recorded := s.Annotations[addressesAnnotation(slot)]; !recorded {
			t.Errorf("the Sandbox holds no address record of the slot %s for %s (annotations %v)", slot, token, s.Annotations)
		}
		secret := g.secret(roleSecretName(name, slot))
		if secret == nil {
			t.Errorf("the Sandbox has no Secret %s", roleSecretName(name, slot))
			continue
		}
		owner := string(secret.Data[GitHubDefaultKey])
		if token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(owner)]); err != nil || token != staticCredentialToken(claim.RoleImplementer) {
			t.Errorf("the %s Secret's %s hosts.yml holds %q (%v), want the implement App's %q", slot, owner, token, err, staticCredentialToken(claim.RoleImplementer))
		}
	}
	for _, loc := range []runtime.Locator{first, second} {
		if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
			t.Errorf("%s after both launches: %s (%v), want it alive", loc.Claim, obs.Kind, err)
		}
	}
}
