package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The deployment's plan guidance reaches the composed prompt of each owner, a tree's root
// architect and a sub-architect, after its own addressing and before the deployment's
// instructions, and no other agent's. Each launch is built twice, by a daemon whose deployment
// sets the guidance and by one that sets none. A phase worker's prompt is the same text in both
// builds, byte for byte: a deployment that sets the guidance changes no other agent's prompt.
func TestOnlyAnOwnersPromptHoldsThePlanGuidance(t *testing.T) {
	t.Parallel()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	stateDir := t.TempDir()
	const (
		planted    = "Plant 7f3a: give each review to another model family."
		header     = "# Plan guidance of this deployment (s1)"
		deployment = "Deployment rule 7f3a."
	)
	if err := config.WritePlanGuidance([]byte(planted+"\n"), stateDir, "s1"); err != nil {
		t.Fatalf("WritePlanGuidance: %v", err)
	}
	guidance := config.PlanGuidanceCopy(stateDir)
	if err := config.WriteDeploymentInstructions([]byte(deployment+"\n"), stateDir, "s1"); err != nil {
		t.Fatalf("WriteDeploymentInstructions: %v", err)
	}
	instructions := config.DeploymentInstructionsCopy(stateDir)
	builds := []struct {
		name string
		s    specs
	}{
		{"with", specs{stateDir: stateDir, project: "s1", prompts: composer, instructions: instructions, planGuidance: guidance, appOwners: noAppOwners}},
		{"without", specs{stateDir: stateDir, project: "s1", prompts: composer, instructions: instructions, appOwners: noAppOwners}},
	}
	launches := []struct {
		name, issue string
		role        claim.Role
		owner       bool
	}{
		{"the root architect", "S1-1", claim.RoleArchitect, true},
		{"a sub-architect", "S1-2", claim.RoleArchitect, true},
		{"the planner", "S1-2", claim.RolePlanner, false},
		{"the implementer", "S1-2", claim.RoleImplementer, false},
		{"the tester", "S1-2", claim.RoleTester, false},
		{"the reviewer", "S1-2", claim.RoleReviewer, false},
		{"the merger", "S1-2", claim.RoleMerger, false},
	}
	composed := map[string]map[string]string{}
	for _, build := range builds {
		composed[build.name] = map[string]string{}
		for _, launch := range launches {
			t.Run(build.name+"/"+launch.name, func(t *testing.T) {
				token, err := claim.NewToken("s1", launch.issue, launch.role)
				if err != nil {
					t.Fatal(err)
				}
				spec, err := build.s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: launch.issue, Tree: "S1-1", Role: launch.role})
				if err != nil {
					t.Fatalf("SpawnSpec: %v", err)
				}
				text, err := runtime.ComposePrompt(spec.Prompt)
				if err != nil {
					t.Fatalf("ComposePrompt: %v", err)
				}
				composed[build.name][launch.name] = text
				guided := build.name == "with" && launch.owner

				if !launch.owner && !strings.Contains(text, deployment) {
					t.Errorf("the prompt omits the deployment's instructions %q", deployment)
				}
				want := 0
				if guided {
					want = 1
				}
				for _, phrase := range []string{header, planted} {
					if held := strings.Count(text, phrase); held != want {
						t.Errorf("the prompt holds %q %d times, want %d: once in an owner's prompt of a deployment that sets the guidance, and nowhere else", phrase, held, want)
					}
				}
				if !guided {
					return
				}
				// The order anchors on the launch's own addressing, which no role part holds, and on
				// text the guidance copy alone holds: the guidance starts after the addressing ends,
				// and the deployment's instructions start after the guidance ends.
				addressing := spec.Prompt.Addressing
				addressingEnds := strings.Index(text, addressing) + len(addressing)
				guidanceStarts, guidanceEnds := strings.Index(text, header), strings.Index(text, planted)+len(planted)
				deploymentStarts := strings.Index(text, deployment)
				if addressing == "" || strings.Count(text, addressing) != 1 || addressingEnds > guidanceStarts || guidanceEnds > deploymentStarts {
					t.Errorf("the guidance is not between the launch's addressing and the deployment's instructions: the addressing (held %d times) ends at byte %d, the guidance runs from %d to %d, the deployment's instructions start at %d",
						strings.Count(text, addressing), addressingEnds, guidanceStarts, guidanceEnds, deploymentStarts)
				}
			})
		}
	}
	for _, launch := range launches {
		if !launch.owner && composed["with"][launch.name] != composed["without"][launch.name] {
			t.Errorf("%s's prompt differs between a deployment that sets the plan guidance and one that sets none", launch.name)
		}
	}
}

// The plan guidance is an operator's file of the same kind as the instructions, so a running
// daemon reads it again before each launch, as it reads them (instructionsCopies): an edit reaches
// the next owner's launch with no restart, and an owner already running keeps the prompt it
// launched with. An edit that leaves the file blank, which boot would refuse, stops no launch: each
// owner launches with the text last read, and the daemon logs once at WARN that it launches with
// the instructions it last read, naming the file and the refusal.
func TestAnEditedPlanGuidanceReachesTheNextOwnersLaunchWithNoRestart(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.PlanGuidancePath = filepath.Join(t.TempDir(), "plan-guidance.md")
	writeInstructions(t, cfg.PlanGuidancePath, "Plant 1a2b: review on another family.\n")
	rt := newComposingRuntime(t)
	d, logs := startLogging(t, cfg, rt)

	first := d.spawn(architect())
	if prompt := rt.prompt(first); !strings.Contains(prompt, "Plant 1a2b") {
		t.Fatalf("the root architect's first launch holds no guidance:\n%s", prompt)
	}

	writeInstructions(t, cfg.PlanGuidancePath, "Plant 3c4d: review on the same family.\n")
	next := rt.prompt(d.spawn(worker("LEGION-2", claim.RoleArchitect)))
	if !strings.Contains(next, "Plant 3c4d") || strings.Contains(next, "Plant 1a2b") {
		t.Errorf("the sub-architect launched after the edit holds the guidance boot read, want the edited one:\n%s", next)
	}
	if running := rt.prompt(first); !strings.Contains(running, "Plant 1a2b") || strings.Contains(running, "Plant 3c4d") {
		t.Errorf("the root architect, already running, has a prompt other than the one it launched with:\n%s", running)
	}
	if count, _ := boots(t, cfg); count != 1 {
		t.Errorf("boots = %d, want 1: the edit reached the launch with no restart", count)
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["file"] != cfg.PlanGuidancePath {
		t.Errorf("lines saying %q = %v, want one at INFO naming %s", instructionsEditedLine, lines, cfg.PlanGuidancePath)
	}

	writeInstructions(t, cfg.PlanGuidancePath, "  \n")
	if blank := rt.prompt(d.spawn(worker("LEGION-3", claim.RoleArchitect))); !strings.Contains(blank, "Plant 3c4d") {
		t.Errorf("the owner launched after a blank edit holds no guidance, want the text last read:\n%s", blank)
	}
	lines := loggedLines(t, logs.String(), instructionsKeptLine)
	if len(lines) != 1 {
		t.Fatalf("lines saying %q = %v, want one", instructionsKeptLine, lines)
	}
	if cause, _ := lines[0]["error"].(string); lines[0]["level"] != "WARN" || lines[0]["file"] != cfg.PlanGuidancePath || !strings.Contains(cause, "is empty") {
		t.Errorf("the line saying %q is %v, want one at WARN naming %s and the refusal %q", instructionsKeptLine, lines[0], cfg.PlanGuidancePath, "is empty")
	}
}
