package sandbox

import (
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
