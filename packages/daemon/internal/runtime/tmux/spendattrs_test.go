package tmux

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// operatorSpendAttributes is the live deployment's own OTEL_RESOURCE_ATTRIBUTES, as an operator
// sets it in `runtime.tmux.pane_env`: the project, the lane, the credential the gateway bills and
// the person who owns the spend. The tests of the composition start from this value, so what they
// read is the shape an operator writes rather than one invented here.
const operatorSpendAttributes = "theorem.project=legion,theorem.lane=legion-infra,theorem.credential=gateway,theorem.person=sami@theorem.dev"

// Every pane carries the operator's own spend attributes with its own issue and role written
// after them (runtime.SpendResourceAttributes, panePairs), so a pane's spend splits by the issue
// it was spent on. The controller serves no issue, so it carries the role alone: an empty
// legion.issue on every controller pane would split a spend report by nothing.
//
// The assertion reads every pair of the name and its whole value, so a pane told nothing is told
// from a pane told the empty value, and a pane told the name twice is told from one told it once.
// Two pairs would leave which value the agent's process reads to the C library's lookup, which
// answers with the first, so the operator's own value would be the one read and the issue would
// never reach the spend report.
//
// The order inside the value is the control: the daemon's pairs stand after the operator's, where
// the detector's last-wins assignment gives them the key. A pane that carried the operator's value
// after the daemon's — the order the operator's own pairs are told in — fails here.
func TestEveryPaneCarriesTheOperatorsSpendAttributesWithItsOwnIssueAndRole(t *testing.T) {
	tree := testSpec()
	controller := controllerSpec(t, "omp")
	controller.Env = nil
	for name, tc := range map[string]struct {
		spec runtime.SpawnSpec
		want string
	}{
		"a tree pane":       {tree, operatorSpendAttributes + ",legion.issue=LEGION-43,legion.role=tester"},
		"a controller pane": {controller, operatorSpendAttributes + ",legion.role=controller"},
	} {
		t.Run(name, func(t *testing.T) {
			in := PaneInputs{
				StateDir: "/state", Workspace: "/state/workspaces/LEGION-43", DaemonURL: "http://127.0.0.1:13370",
				EnvoyURL:    "http://127.0.0.1:9020",
				OperatorEnv: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes},
			}
			var carried []string
			pairs := panePairs(tc.spec, in, secretFiles("/state", tc.spec))
			for i := 1; i < len(pairs); i += 2 {
				if value, ok := strings.CutPrefix(pairs[i], runtime.SpendAttributesVariable+"="); ok {
					carried = append(carried, value)
				}
			}
			if len(carried) != 1 || carried[0] != tc.want {
				t.Errorf("%s carries %s %q, want exactly one pair, %q", name, runtime.SpendAttributesVariable, carried, tc.want)
			}
		})
	}
	// The controller serves no issue, so no pane of it names one: an attribute of some other
	// launch's issue would bill the controller's minutes to that issue.
	in := PaneInputs{StateDir: "/state", OperatorEnv: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}}
	for _, pair := range panePairs(controller, in, secretFiles("/state", controller)) {
		if strings.HasPrefix(pair, runtime.SpendAttributesVariable+"=") && strings.Contains(pair, runtime.SpendIssueAttribute) {
			t.Errorf("a controller pane is told %q, want no %s", pair, runtime.SpendIssueAttribute)
		}
	}
}

// The spend attributes are the operator's to set in `runtime.tmux.pane_env`, which is how the
// deployment states what its whole machine is billed under: the runtime writes each launch's issue
// and role into the value the operator wrote rather than over it (composed, panePairs), so
// checkPaneEnv accepts the name where it refuses every name the runtime sets alone.
func TestCheckPaneEnvAcceptsTheOperatorsSpendAttributes(t *testing.T) {
	if err := checkPaneEnv(map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes}, nil); err != nil {
		t.Errorf("checkPaneEnv with the operator's own spend attributes: %v", err)
	}
}

// The composed value reaches a real process under this runtime too: a child is run with exactly
// the variables a pane is told (PaneVariables, which answers panePairs' own pairs in the pane's
// own order), and what that child prints is what an agent in such a pane reads. A pane of its
// own would prove nothing more here and needs a tmux this machine need not carry, while the
// variables are the whole of what a pane hands its process.
//
// The order inside the value is the control: the operator's attributes stand first and the
// daemon's issue and role after them, where the detector's last-wins assignment gives the launch
// a key the operator's value already holds. A pane that told the operator's own value last — as
// the operator's loop told it before this composition — fails here. The child prints the whole
// value, and the word `unset` for a variable its environment does not hold, so a value that is
// absent reads apart from one that is empty.
func TestAPanesChildProcessReadsTheComposedSpendAttributes(t *testing.T) {
	spec := testSpec()
	in := PaneInputs{
		StateDir: t.TempDir(), Workspace: "/state/workspaces/LEGION-43", DaemonURL: "http://127.0.0.1:13370",
		EnvoyURL:    "http://127.0.0.1:9020",
		OperatorEnv: map[string]string{runtime.SpendAttributesVariable: operatorSpendAttributes},
	}
	variables, err := PaneVariables(spec, in, secretFiles(in.StateDir, spec))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sh", "-c", `printf '%s' "${`+runtime.SpendAttributesVariable+`-unset}"`)
	child.Env = variables
	read, err := child.Output()
	if err != nil {
		t.Fatalf("the child of a pane's variables: %v", err)
	}
	t.Logf("a pane's child process read %s=%s", runtime.SpendAttributesVariable, read)
	want := operatorSpendAttributes + ",legion.issue=LEGION-43,legion.role=tester"
	if string(read) != want {
		t.Errorf("a pane's child read %s=%q, want %q", runtime.SpendAttributesVariable, read, want)
	}
	if !strings.HasPrefix(string(read), operatorSpendAttributes+",legion.") {
		t.Errorf("a pane's child read %s=%q, want the operator's own %q first and the daemon's pairs after it: the detector assigns last-wins",
			runtime.SpendAttributesVariable, read, operatorSpendAttributes)
	}
}
