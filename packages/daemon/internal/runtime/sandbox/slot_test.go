package sandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

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
	for token, want := range map[claim.Token]claim.Role{
		agent:           "implementer-2",
		workerToken:     claim.RoleTester,
		controllerToken: claim.RoleController,
	} {
		if got := slotOf(token); got != want {
			t.Errorf("slotOf(%s) = %q, want %q", token, got, want)
		}
	}
	if _, issuePod := podKindFor(slotOf(agent)).(issuePod); !issuePod {
		t.Errorf("podKindFor(slotOf(%s)) = %T, want the issue pod", agent, podKindFor(slotOf(agent)))
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
