package sandbox

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
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
// pod would collide with it.
func TestAnOperatorVolumeMayNotTakeADeploymentRolesVolumeName(t *testing.T) {
	pod := Pod{Volumes: []corev1.Volume{{Name: "launcher-checker", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}
	legion := append(slices.Clone(claim.Roles), claim.RoleController)
	want := "runtime.kubernetes.pod.volumes[0].name launcher-checker is a volume Legion puts in every pod"
	if err := CheckPod(pod, append(slices.Clone(legion), "checker"), nil, testOptions().Tools, nil, nil, ompsessions.Store{}, NoExchange); err == nil || err.Error() != want {
		t.Errorf("CheckPod with the checker among the roles = %v, want %q", err, want)
	}
	if err := CheckPod(pod, legion, nil, testOptions().Tools, nil, nil, ompsessions.Store{}, NoExchange); err != nil {
		t.Errorf("CheckPod with no checker among the roles = %v, want no refusal", err)
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
	r.forgetLauncherCredentials(name)
	if _, held := r.launcherAuth.lookup(secret.Name, s.UID); held {
		t.Errorf("the checker's launcher credential is still held after its Sandbox %s was forgotten", name)
	}
}
