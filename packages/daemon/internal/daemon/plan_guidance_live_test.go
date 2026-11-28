//go:build e2e

// The live proof's driver for the plan guidance: a deployment whose configuration sets
// plan_guidance gives that text to the launch of the owner of a person's issue and to no other
// launch, serves the file's path in its settings, and `legion config check` names it.
// scripts/e2e/plan-guidance.sh builds this driver with `-tags e2e` and owns the nats-server it runs
// against; it starts no database, and the PostgreSQL is the server LEGION_TEST_PG_DSN names. It
// runs TestPlanGuidanceControlLive, a deployment that sets no plan_guidance, before
// TestPlanGuidanceLive.
//
// Dispatch is the real server this checkout builds (startHandoffDispatch), on a database of its
// own, where a person creates the issue in a project of the run's own. The daemon is the whole
// daemon (run, through the step records rig, newStepRecordsRigWith) with dispatch_url at that
// server, its store seeded with the issue admitted and its design gate approved, so its boot
// launches the issue's architect and its admission pass the planner, the first step's agent.
// The runtime is the package's fake one, which records each launch's spec; each check composes
// the launch's prompt from that spec with runtime.ComposePrompt, the function every runtime
// writes an agent's prompt file with. `legion config check` is the binary this checkout
// builds, run on a legion.yaml that names the same guidance file. GitHub's REST API, the Apps'
// tokens and Envoy's listener are stood in for, as for the step records' driver.
package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// planGuidancePlanted is the text of the run's guidance file, which no prompt part holds.
const planGuidancePlanted = "Plant 4c1e: give each review to an agent of another model family."

// planGuidanceLive is one run of the proof: the rig over one issue of one project, the checkout
// the legion binary is built from, and the guidance file the run wrote, which the deployment
// names or not.
type planGuidanceLive struct {
	*stepRecordsRig
	root     string
	guidance string
}

// planGuidanceCheck is one check of the proof: what it observed, and why it does not hold, nil
// when it holds.
type planGuidanceCheck struct {
	name string
	run  func() (string, error)
}

// TestPlanGuidanceLive is the proof: a deployment that sets plan_guidance, and each check holds.
func TestPlanGuidanceLive(t *testing.T) {
	g := newPlanGuidanceLive(t, true)
	for _, check := range g.checks() {
		g.step(check.name, func() {
			observed, err := check.run()
			g.note("%s", observed)
			if err != nil {
				t.Fatalf("%s: %v", check.name, err)
			}
		})
	}
}

// TestPlanGuidanceControlLive is the proof's control, run first: the same run with plan_guidance
// unset. The owner's launch then holds no guidance and the settings serve no path, so those two
// checks must fail; the worker's launch holds none either way, so that check must hold; and
// `legion config check` prints no plan_guidance line, so that check must fail too.
func TestPlanGuidanceControlLive(t *testing.T) {
	g := newPlanGuidanceLive(t, false)
	holds := map[string]bool{"owner-has-guidance": false, "worker-has-none": true, "settings-serve-the-key": false, "check-config-names-it": false}
	for _, check := range g.checks() {
		g.step("control-"+check.name, func() {
			observed, err := check.run()
			g.note("%s", observed)
			if held := err == nil; held != holds[check.name] {
				t.Fatalf("%s held %t on a deployment that sets no plan_guidance, want %t (%v)", check.name, held, holds[check.name], err)
			}
			if err != nil {
				g.note("fails, as the control must: %v", err)
			}
		})
	}
}

// newPlanGuidanceLive writes the run's guidance file, names it in the deployment's configuration
// when guided, and boots the rig over one issue a person created in Dispatch.
func newPlanGuidanceLive(t *testing.T, guided bool) *planGuidanceLive {
	t.Helper()
	fixtureenv.Live(t)
	root := liveCheckout(t)
	proof := newLiveProof(t, "scripts/e2e/plan-guidance.sh")
	guidance := filepath.Join(t.TempDir(), "plan-guidance.md")
	if err := os.WriteFile(guidance, []byte(planGuidancePlanted+"\n"), 0o600); err != nil {
		t.Fatalf("write the guidance file: %v", err)
	}
	if guided {
		proof.cfg.PlanGuidancePath = guidance
	}
	g := &planGuidanceLive{stepRecordsRig: newStepRecordsRigWith(t, proof, startHandoffDispatch(t, root).scratchDispatch, 1), root: root, guidance: guidance}
	g.note("Dispatch %s (database of its own, NATS off), project %s, issue %s; the daemon at dispatch_url %s; plan_guidance %q",
		g.dispatch.base, g.cfg.Project, g.key, g.cfg.DispatchURL, g.cfg.PlanGuidancePath)
	return g
}

// checks is the proof's four checks, in the order they run.
func (g *planGuidanceLive) checks() []planGuidanceCheck {
	return []planGuidanceCheck{
		{"owner-has-guidance", g.ownerHasGuidance},
		{"worker-has-none", g.workerHasNone},
		{"settings-serve-the-key", g.settingsServeTheKey},
		{"check-config-names-it", g.checkConfigNamesIt},
	}
}

// header is the first line of boot's copy of the guidance, which only the copy holds.
func (g *planGuidanceLive) header() string {
	return "# Plan guidance of this deployment (" + g.cfg.Project + ")"
}

// composed is the prompt the latest launch of role's claim on the run's issue was started with.
func (g *planGuidanceLive) composed(role claim.Role) (runtime.SpawnSpec, string, error) {
	spec := lastLaunch(g.t, g.rt, g.token(role))
	text, err := runtime.ComposePrompt(spec.Prompt)
	return spec, text, err
}

// ownerHasGuidance: the architect's launch reads boot's copy of the guidance, and its composed
// prompt holds the copy's header and the planted text once each, after the launch's own
// addressing, which no role part holds.
func (g *planGuidanceLive) ownerHasGuidance() (string, error) {
	spec, text, err := g.composed(claim.RoleArchitect)
	if err != nil {
		return "", fmt.Errorf("compose the architect's prompt: %w", err)
	}
	header, addressing := g.header(), spec.Prompt.Addressing
	observed := fmt.Sprintf("the architect's launch reads the plan guidance from %q; its composed prompt (%d bytes) holds the copy's header %d times and the planted text %d times, its addressing %d times",
		spec.Prompt.PlanGuidancePath, len(text), strings.Count(text, header), strings.Count(text, planGuidancePlanted), strings.Count(text, addressing))
	if want := filepath.Join(g.cfg.StateDir, config.PlanGuidanceFile); spec.Prompt.PlanGuidancePath != want {
		return observed, fmt.Errorf("the architect's launch reads the plan guidance from %q, want boot's copy %q", spec.Prompt.PlanGuidancePath, want)
	}
	if strings.Count(text, header) != 1 || strings.Count(text, planGuidancePlanted) != 1 {
		return observed, errors.New("the architect's prompt does not hold the copy's header and the planted text once each")
	}
	if addressing == "" || strings.Count(text, addressing) != 1 || strings.Index(text, header) < strings.Index(text, addressing)+len(addressing) {
		return observed, errors.New("the guidance does not stand after the launch's addressing")
	}
	return observed, nil
}

// workerHasNone: the planner, the agent of the issue's first step, is launched with no guidance,
// and its composed prompt holds neither the copy's header nor the planted text.
func (g *planGuidanceLive) workerHasNone() (string, error) {
	g.worker(claim.RolePlanner)
	spec, text, err := g.composed(claim.RolePlanner)
	if err != nil {
		return "", fmt.Errorf("compose the planner's prompt: %w", err)
	}
	observed := fmt.Sprintf("the planner's launch reads the plan guidance from %q; its composed prompt (%d bytes) holds the copy's header %d times and the planted text %d times",
		spec.Prompt.PlanGuidancePath, len(text), strings.Count(text, g.header()), strings.Count(text, planGuidancePlanted))
	if spec.Prompt.PlanGuidancePath != "" || strings.Contains(text, g.header()) || strings.Contains(text, planGuidancePlanted) {
		return observed, errors.New("the planner's launch carries the plan guidance")
	}
	return observed, nil
}

// settingsServeTheKey: GET /legion/v1/state, the route `legion state --json` prints, serves
// settings.plan_guidance, the path of the run's guidance file.
func (g *planGuidanceLive) settingsServeTheKey() (string, error) {
	got := g.d.state().Settings.PlanGuidance
	observed := fmt.Sprintf("GET /legion/v1/state serves settings.plan_guidance %q", got)
	if got != g.guidance {
		return observed, fmt.Errorf("settings.plan_guidance is %q, want the guidance file %q", got, g.guidance)
	}
	return observed, nil
}

// checkConfigNamesIt: `legion config check`, built from this checkout, on a legion.yaml
// that names the run's guidance file as the deployment does, exits 0 and prints the line that
// names the file and who carries it.
func (g *planGuidanceLive) checkConfigNamesIt() (string, error) {
	t := g.t
	t.Helper()
	dir := t.TempDir()
	legion := buildProofLegion(t, g.root, dir)
	body := fmt.Sprintf(`project: DEMO
port: 13370
postgres_dsn: postgres://legion@127.0.0.1:1/legion
state_dir: %s
operator_token_file: ./operator-token
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
projects:
  DEMO: {}
`, filepath.Join(dir, "state"))
	if g.cfg.PlanGuidancePath != "" {
		body += "plan_guidance: " + g.cfg.PlanGuidancePath + "\n"
	}
	file := filepath.Join(dir, "legion.yaml")
	for name, contents := range map[string]string{file: body, filepath.Join(dir, "operator-token"): "operator-token\n"} {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	check := exec.Command(legion, "config", "check", "--config", file)
	check.Env = append(os.Environ(), "LEGION_OMP_PATH="+legion, "LEGION_GH_PATH="+legion, "XDG_STATE_HOME="+t.TempDir())
	var stdout, stderr bytes.Buffer
	check.Stdout, check.Stderr = &stdout, &stderr
	code := 0
	if err := check.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run `legion config check`: %v", err)
		}
		code = exit.ExitCode()
	}
	want := "plan_guidance: " + g.guidance + " — each owner's system prompt carries this guidance after its addressing; Legion requires none of it"
	printed := slices.ContainsFunc(strings.Split(stdout.String(), "\n"), func(line string) bool { return strings.HasPrefix(line, "plan_guidance: ") })
	observed := fmt.Sprintf("`legion config check` exited %d and printed a plan_guidance line: %t", code, printed)
	if code != 0 {
		return observed, fmt.Errorf("`legion config check` exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	if !slices.Contains(strings.Split(stdout.String(), "\n"), want) {
		return observed, fmt.Errorf("`legion config check` printed no line %q:\n%s", want, stdout.String())
	}
	return observed + ": " + want, nil
}
