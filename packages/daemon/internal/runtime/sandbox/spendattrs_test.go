package sandbox

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// operatorSpendAttributes is the live deployment's own OTEL_RESOURCE_ATTRIBUTES, the attributes
// every pod of it is billed under before any launch adds its own: the project, the lane, the
// credential the gateway bills and the person who owns the spend. Every test of the composition
// starts from this value, so what the tests read is the shape an operator writes rather than one
// invented here.
const operatorSpendAttributes = "theorem.project=legion,theorem.lane=legion-infra,theorem.credential=gateway,theorem.person=sami@theorem.dev"

// Every launch carries the operator's own spend attributes with its own issue and role written
// after them (runtime.SpendResourceAttributes, roleOperatorEnv), whatever the launch runs: an Oh
// My Pi worker, a Claude Code worker, or the controller, which serves no issue and so carries the
// role alone with no empty legion.issue to split a spend report by. The daemon's pairs come last
// because the detector that reads the variable assigns last-wins.
//
// The environment read here is the launch's whole one, the container's and the generation's start
// command together (workerOf), and the assertion reads every pair of the name in it: exactly one,
// whose whole value is the composition. A second pair would leave which value the agent's process
// reads to the C library's lookup, which answers with the first, so the operator's own value
// would be the one read and the issue would never reach the spend report.
func TestEveryLaunchCarriesTheOperatorsSpendAttributesWithItsOwnIssueAndRole(t *testing.T) {
	opts := testOptions()
	opts.Pod = Pod{Env: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	// testTree is the issue these specs name as well as the tree (testSpec), and the tester and
	// the controller are the roles they run.
	for name, tc := range map[string]struct {
		spec runtime.SpawnSpec
		want string
	}{
		"a worker":             {workerSpec(t), operatorSpendAttributes + ",legion.issue=" + testTree + ",legion.role=tester"},
		"a Claude Code worker": {claudeWorkerSpec(t), operatorSpendAttributes + ",legion.issue=" + testTree + ",legion.role=tester"},
		"the controller":       {controllerSpec(t), operatorSpendAttributes + ",legion.role=controller"},
	} {
		t.Run(name, func(t *testing.T) {
			var carried []string
			for _, entry := range workerOf(t, r, tc.spec).Env {
				if entry.Name == runtime.SpendAttributesVariable {
					carried = append(carried, entry.Value)
				}
			}
			if len(carried) != 1 || carried[0] != tc.want {
				t.Errorf("%s carries %s %q, want exactly one pair, %q", name, runtime.SpendAttributesVariable, carried, tc.want)
			}
		})
	}
}

// The image probe's container is told the operator's spend attributes exactly as the operator
// wrote them (probe.go builds its environment from operatorEnv, not roleOperatorEnv): the probe
// runs no role and serves no issue, so there is no issue to split its spend by and no role to
// name, and a legion.issue of some launch's would bill the probe's own minutes to that issue.
func TestTheImageProbeIsToldTheOperatorsSpendAttributesAlone(t *testing.T) {
	opts := goldenOptions()
	opts.Pod = Pod{Env: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	var carried []string
	for _, entry := range r.probeManifest(probeSandboxName, ImageProbe{Contract: 3}, time.Now()).Spec.PodTemplate.Spec.Containers[0].Env {
		if entry.Name == runtime.SpendAttributesVariable {
			carried = append(carried, entry.Value)
		}
	}
	if len(carried) != 1 || carried[0] != operatorSpendAttributes {
		t.Fatalf("the probe container carries %s %q, want exactly one pair, the operator's own %q",
			runtime.SpendAttributesVariable, carried, operatorSpendAttributes)
	}
	if strings.Contains(carried[0], runtime.SpendIssueAttribute) || strings.Contains(carried[0], runtime.SpendRoleAttribute) {
		t.Errorf("the probe container carries %s = %q, want no %s and no %s: it runs no role and serves no issue",
			runtime.SpendAttributesVariable, carried[0], runtime.SpendIssueAttribute, runtime.SpendRoleAttribute)
	}
}

// The composed value reaches a real process, not only the launch's own data: the plain pairs
// launchEnvironment hands the generation's start command are run as one child's whole
// environment, and what that child prints is what the agent of the generation reads. The
// launcher hands LauncherStart.Env to its child verbatim, which internal/launcher's
// TestAGenerationsCredentialsLiveExactlyAsLongAsItsChild proves — its child read the credential
// through the pointer the start command's own ENVOY_TOKEN_FILE pair named — so a child run under
// exactly those pairs is the generation's own environment, and that proof is cited here rather
// than written again.
//
// The controller's launch is the control: it serves no issue, so its child prints the operator's
// attributes with legion.role=controller and no legion.issue at all. The child prints the whole
// value, and the word `unset` for a variable its environment does not hold, so a value that is
// absent reads apart from one that is empty and from the operator's own alone.
func TestAGenerationsChildProcessReadsTheComposedSpendAttributes(t *testing.T) {
	opts := testOptions()
	opts.Pod = Pod{Env: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		spec runtime.SpawnSpec
		want string
	}{
		"a worker":       {workerSpec(t), operatorSpendAttributes + ",legion.issue=" + testTree + ",legion.role=tester"},
		"the controller": {controllerSpec(t), operatorSpendAttributes + ",legion.role=controller"},
	} {
		t.Run(name, func(t *testing.T) {
			_, plain := r.launchEnvironment(launchOf(t, r, tc.spec))
			child := exec.Command("/bin/sh", "-c", `printf '%s' "${`+runtime.SpendAttributesVariable+`-unset}"`)
			child.Env = plain
			read, err := child.Output()
			if err != nil {
				t.Fatalf("the child of %s's start command: %v", name, err)
			}
			t.Logf("%s: its child process read %s=%s", name, runtime.SpendAttributesVariable, read)
			if string(read) != tc.want {
				t.Errorf("the child of %s read %s=%q, want %q", name, runtime.SpendAttributesVariable, read, tc.want)
			}
		})
	}
}

// The composed value travels in the generation's start command and in no pod specification,
// which is why a role already running is not restarted for it: a pod is replaced for what its own
// specification holds and for the configuration its template records (podShapeAnnotation,
// podShapeDrift), and the whole template — every container and that annotation with them — names
// this variable nowhere.
//
// POD_UID is the control that tells the two carriers apart: it is the one variable of a role's
// environment that comes from the pod specification, through the downward API, and it is printed
// beside the start command's pairs from the same launch. So a read that finds nothing of the
// spend attributes in the pod specification is told from a read that could see nothing there at
// all. Both printed halves are the evidence, and both are asserted.
func TestTheComposedSpendAttributesTravelInTheStartCommandAndNoPodSpecification(t *testing.T) {
	opts := testOptions()
	opts.Pod = Pod{Env: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	l := launchOf(t, r, workerSpec(t))
	_, plain := r.launchEnvironment(l)
	var carried []string
	for _, pair := range plain {
		if strings.HasPrefix(pair, runtime.SpendAttributesVariable+"=") {
			carried = append(carried, pair)
		}
	}
	t.Logf("the generation's start command carries: %s", strings.Join(carried, " "))
	want := runtime.SpendAttributesVariable + "=" + operatorSpendAttributes + ",legion.issue=" + testTree + ",legion.role=tester"
	if len(carried) != 1 || carried[0] != want {
		t.Errorf("the start command carries %q, want exactly one pair, %q", carried, want)
	}
	template := r.podTemplate(l)
	container := containerNamed(t, template.Spec, l.kind.container())
	var sources []string
	podUID := "no POD_UID at all"
	for _, entry := range container.Env {
		switch {
		case entry.ValueFrom != nil && entry.ValueFrom.FieldRef != nil:
			sources = append(sources, entry.Name+" from the downward API's "+entry.ValueFrom.FieldRef.FieldPath)
			if entry.Name == "POD_UID" {
				podUID = entry.ValueFrom.FieldRef.FieldPath
			}
		case entry.ValueFrom != nil:
			sources = append(sources, entry.Name+" from "+entry.ValueFrom.String())
		default:
			sources = append(sources, entry.Name+"="+entry.Value)
		}
	}
	t.Logf("the pod specification's %s container carries: %s", container.Name, strings.Join(sources, ", "))
	if podUID != "metadata.uid" {
		t.Errorf("the %s container carries POD_UID from %q, want the downward API's metadata.uid: it is the control this read tells the pod specification by",
			container.Name, podUID)
	}
	encoded, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{runtime.SpendAttributesVariable, operatorSpendAttributes} {
		if strings.Contains(string(encoded), text) {
			t.Errorf("the pod template names %q: the composed value travels in the generation's start command alone, so that no role already running is restarted for it", text)
		}
	}
}
