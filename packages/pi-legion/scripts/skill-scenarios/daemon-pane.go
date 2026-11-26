// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane, computed by the daemon's own functions, for worker-pane.ts and
// daemon-standin.ts to run Oh My Pi and the stand-in daemon under, and the prompt a launch of any
// role starts with, for a reader of that prompt. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from.
//
//	phase <role>             prints the phase the role's worker runs in, the first phase
//	                         workflow.RoleFor gives to the role, then on a second line the word of
//	                         the handoff record that phase ends with (handoffWords), empty for a
//	                         phase that ends with none
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh bundle
//	                         under its LEGION_GH_OWNERS_DIR, and prints its paneResponse (JSON);
//	                         it refuses the system prompt of a tree architect (systemPromptArgument)
//	prompt <role> <root|child> [--plan-guidance <file>]
//	                         prints the system prompt a launch of the role starts with
//	                         (rolePrompt), byte for byte as the daemon writes its prompt file, for
//	                         the rig's claim on the root issue of its tree or on a child under it;
//	                         a root architect's prompt ends with the design gate steps of a
//	                         deployment that sets no `gates.design`, as such a launch's does; with
//	                         --plan-guidance, an owner's prompt (ownsPlan) then ends with the
//	                         daemon's copy of that file (planGuidanceCopy), as the prompt of a
//	                         deployment whose `plan_guidance` names it, and a checkout whose daemon
//	                         has no key plan_guidance refuses the flag
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const usage = "usage: daemon-pane phase <role> | pane < request.json | prompt <role> <root|child> [--plan-guidance <file>]"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case len(args) == 1 && args[0] == "pane":
		err = pane()
	case len(args) == 3 && args[0] == "prompt" && (args[2] == "root" || args[2] == "child"):
		err = printPrompt(claim.Role(args[1]), args[2] == "root", "")
	case len(args) == 5 && args[0] == "prompt" && (args[2] == "root" || args[2] == "child") && args[3] == "--plan-guidance" && args[4] != "":
		err = printPrompt(claim.Role(args[1]), args[2] == "root", args[4])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-pane %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

// handoffWords is the word of the handoff each handoff phase ends with. It is this program's own
// copy rather than the daemon's phase table because one program runs against each label's
// checkout of the daemon module, and that table has had more than one name (phase.HandoffFile,
// then phase.Handoff). The program's other copies of the daemon, each carried for the same reason
// and each saying so, are realGH, the join of rolePrompt, the write of systemPromptArgument,
// ownsPlan, readPlanGuidance and planGuidanceCopy; every other answer here is the daemon's own
// function (daemon-pane.ts).
var handoffWords = map[phase.Phase]string{
	phase.Planning: "plan", phase.Implementing: "implement", phase.Testing: "test", phase.Reviewing: "review",
}

// ownsPlan is whether c's launch is an owner's, the launch whose prompt carries the deployment's
// plan guidance: this program's own copy of daemon.OwnsPlan, which a checkout before the plan
// guidance lacks. daemon-pane_test.go holds it to the daemon's at this checkout.
func ownsPlan(c supervise.Claim) bool {
	return c.Role == claim.RoleArchitect
}

// planGuidanceCopy is the text of the daemon's copy of a deployment's plan guidance, the file an
// owner's prompt reads: this program's own copy of what config.WritePlanGuidance writes, which a
// checkout before the plan guidance lacks. daemon-pane_test.go holds it to the daemon's at this
// checkout.
func planGuidanceCopy(project string, guidance []byte) string {
	return "# Plan guidance of this deployment (" + project + ")\n\nLegion requires none of this guidance and refuses no plan for a step it lacks.\n\n" + string(guidance)
}

// planGuidanceMaxBytes is this program's copy of config.PlanGuidanceMaxBytes, which a checkout
// before the bound lacks.
const planGuidanceMaxBytes = 64 << 10

// readPlanGuidance reads a deployment's plan guidance file as the daemon's boot reads it, refusing
// a file that cannot be read, that is not a regular file, that holds more than
// planGuidanceMaxBytes or that holds only blank lines, so the program prints no prompt that no
// launch of the daemon is given: this program's own copy of config.ReadPlanGuidance, which a
// checkout before the plan guidance lacks. daemon-pane_test.go holds it to the daemon's at this
// checkout.
func readPlanGuidance(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("plan_guidance names %s, which is not a regular file", path)
	}
	contents, err := io.ReadAll(io.LimitReader(f, planGuidanceMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plan guidance file %s could not be read: %w", path, err)
	}
	if len(contents) > planGuidanceMaxBytes {
		return nil, fmt.Errorf("plan_guidance names %s, which holds more than %d bytes, the most an owner's prompt takes of it", path, planGuidanceMaxBytes)
	}
	if strings.TrimSpace(string(contents)) == "" {
		return nil, fmt.Errorf("plan guidance file %s is empty", path)
	}
	return contents, nil
}

// planGuidanceKeyed is whether this checkout's daemon reads the key plan_guidance
// (config.ConfigReference), without which no launch of it carries a plan guidance.
func planGuidanceKeyed() bool {
	for _, key := range config.ConfigReference() {
		if key.Name == "plan_guidance" {
			return true
		}
	}
	return false
}

func printPhase(role claim.Role) error {
	for _, p := range phase.All {
		if workflow.RoleFor(p) == role {
			fmt.Println(p)
			fmt.Println(handoffWords[p])
			return nil
		}
	}
	return fmt.Errorf("%q works no phase", role)
}

// rigProject, rigRoot and rigChild are the claim `prompt` composes for: the rig's project, the
// root issue of its tree, and a child issue under that root.
const (
	rigProject = "rig"
	rigRoot    = "RIG-1"
	rigChild   = "RIG-2"
)

// rigClaim is the claim `prompt` composes for: the rig's claim on the root issue of its tree
// (root) or on a child issue under that root.
func rigClaim(role claim.Role, root bool) (supervise.Claim, error) {
	issue := rigChild
	if root {
		issue = rigRoot
	}
	token, err := claim.NewToken(rigProject, issue, role)
	if err != nil {
		return supervise.Claim{}, err
	}
	return supervise.Claim{Token: token, Project: rigProject, Tree: rigRoot, Issue: issue, Role: role, Generation: 1}, nil
}

// guidedPrompt is the system prompt a launch of c starts with (rolePrompt), with the deployment's
// plan guidance where guidance is not nil and c's launch is an owner's (ownsPlan): a blank line
// and the daemon's copy of the file, its trailing newlines dropped, as runtime.ComposePrompt joins
// a part after the addressing. Any other role's prompt is the prompt without it, byte for byte.
func guidedPrompt(stateDir string, c supervise.Claim, guidance []byte) (string, error) {
	prompt, err := rolePrompt(stateDir, rigProject, c)
	if err != nil {
		return "", err
	}
	if guidance != nil && ownsPlan(c) {
		prompt += "\n\n" + strings.TrimRight(planGuidanceCopy(rigProject, guidance), "\n")
	}
	return prompt, nil
}

// printPrompt prints the system prompt a launch of role starts with (guidedPrompt), for the rig's
// claim on the root issue of its tree (root) or on a child issue under it, and nothing else: no
// newline is added, so the output is the text of the claim's prompt file. A guidanceFile that is
// not "" is the deployment's plan guidance. It is refused, before the file is read, by a checkout
// whose daemon has no key plan_guidance, since no launch of that daemon carries one.
func printPrompt(role claim.Role, root bool, guidanceFile string) error {
	var guidance []byte
	if guidanceFile != "" {
		if !planGuidanceKeyed() {
			return errors.New("this checkout's daemon has no key plan_guidance, so no launch of it carries a plan guidance")
		}
		var err error
		if guidance, err = readPlanGuidance(guidanceFile); err != nil {
			return err
		}
	}
	c, err := rigClaim(role, root)
	if err != nil {
		return err
	}
	stateDir, err := os.MkdirTemp("", "daemon-pane-prompt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	prompt, err := guidedPrompt(stateDir, c, guidance)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, prompt)
	return err
}

// paneRequest is the claim a pane is launched for, and the rig's stand-ins for what the daemon
// knows at boot. The pane's tree is its issue.
type paneRequest struct {
	Project string `json:"project"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
	// StateDir stands for the daemon's state_dir, Workspace for the issue's workspace.
	StateDir  string   `json:"stateDir"`
	Workspace string   `json:"workspace"`
	DaemonURL string   `json:"daemonUrl"`
	EnvoyURL  string   `json:"envoyUrl"`
	NATSURLs  []string `json:"natsUrls"`
	// BootTokenFile is the rig's boot token file, the one its stand-in daemon checks.
	BootTokenFile string `json:"bootTokenFile"`
	// GHToken is the GitHub token the rig stands in for the claim's App token: it is rendered
	// into the claim's gh bundle (runtime.GHConfigDir), the one directory of rigOwner, which the
	// pane's GH_CONFIG_DIR names, as the daemon's tmux runtime writes it at spawn; the pane's
	// LEGION_GH_PATH is realGH's: GHPath when the rig names one, else the first gh on Path,
	// refused when that is a script, as the daemon resolves it at boot. "" is a daemon
	// with no GitHub credential: no files, and no gh variable on the pane.
	GHToken string `json:"ghToken"`
	// Path is the PATH the daemon would start under; the pane's PATH is it behind the pane's
	// launcher directory (workerbin.Path).
	Path string `json:"path"`
	// GHPath is the real gh the rig names for the pane, as an operator names one with
	// LEGION_GH_PATH: its recording stand-in. "" is the daemon's own resolution, the first gh on
	// Path, refused when that is a script.
	GHPath string `json:"ghPath"`
	// SystemPrompt asks for the pane's system prompt, composed from the role prompts the daemon
	// module embeds and written to the claim's prompt file under StateDir, as a pane's launch
	// writes it (runtime.WriteSystemPrompt).
	SystemPrompt bool `json:"systemPrompt"`
}

type paneResponse struct {
	// Env is every variable the daemon tells the pane, PATH included.
	Env map[string]string `json:"env"`
	// SystemPromptArgument is the pane's one --append-system-prompt argument, as shell text: the
	// flag and the path of the file the prompt was written to; "" without SystemPrompt.
	SystemPromptArgument string `json:"systemPromptArgument,omitempty"`
}

func pane() error {
	var req paneRequest
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	role := claim.Role(req.Role)
	token, err := claim.NewToken(req.Project, req.Issue, role)
	if err != nil {
		return err
	}
	ghRoot, gh := "", ""
	if req.GHToken != "" {
		// ghconfig's one writer of a role's gh bundle, so the rig's directory is the daemon's layout;
		// the App label and expiry only feed the runtime's log lines, which the rig does not write. The
		// rig's project serves one repository of one owner, so its bundle holds that owner's directory
		// alone, where a daemon serving more holds a `<owner>/<repo>` directory beside it per sibling
		// repository.
		ghRoot = runtime.GHConfigDir(req.StateDir, token)
		bundle := ghconfig.Bundle{Default: rigOwner, Dirs: map[string]ghconfig.Rendered{rigOwner: ghconfig.Render(req.GHToken, "", time.Time{})}}
		if _, err := ghconfig.WriteBundle(ghRoot, bundle); err != nil {
			return fmt.Errorf("write the claim's gh files: %w", err)
		}
		if gh, err = realGH(req.GHPath, req.Path); err != nil {
			return err
		}
	}
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL, EnvoyURL: req.EnvoyURL,
		NATSURLs: req.NATSURLs, GHOwnersDir: ghRoot, GHDefault: rigOwner, GH: gh,
	}, []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: req.BootTokenFile}})
	if err != nil {
		return err
	}
	res := paneResponse{Env: map[string]string{"PATH": workerbin.Path(req.Path, req.StateDir)}}
	for _, variable := range variables {
		name, value, _ := strings.Cut(variable, "=")
		res.Env[name] = value
	}
	if req.SystemPrompt {
		if res.SystemPromptArgument, err = systemPromptArgument(req, token); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

// rigOwner is the one GitHub owner of the rig's project, the owner of rigRepository, a placeholder
// whose directory of the gh bundle holds the rig's token.
const rigOwner = "example"

// rigRepository and rigDefaultBranch are the repository and default branch a rig worker's
// addressing names, as a real pane's does for its issue. They are the tester-proof world's, which
// rig.sh names again: rigRepository must equal rig.sh's `worker_repo`, and rigDefaultBranch
// rig.sh's `worker_default_branch`. The branch is not `main`, since the skills tell a worker never
// to assume one.
const (
	rigRepository    = rigOwner + "/widgets"
	rigDefaultBranch = "trunk"
)

// realGH is the real gh the pane is told as LEGION_GH_PATH: named, the gh the rig names (its
// stand-in, as an operator's LEGION_GH_PATH is taken as given), else the daemon's resolution at
// boot, the first executable gh on path, refused when it is a script (daemon.RefuseScriptGH's
// rule). It is this program's own copy, since the rig runs it against each label's checkout of the
// daemon module, an older one among them.
func realGH(named, path string) (string, error) {
	if named != "" {
		if info, err := os.Stat(named); err != nil || !filepath.IsAbs(named) || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("the rig's gh %q is not an absolute executable path", named)
		}
		return named, nil
	}
	for _, dir := range filepath.SplitList(path) {
		gh := filepath.Join(dir, "gh")
		if info, err := os.Stat(gh); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			head, err := firstBytes(gh, 2)
			if err != nil {
				return "", fmt.Errorf("read %s, the first gh on the daemon's PATH: %w", gh, err)
			}
			if string(head) == "#!" {
				return "", fmt.Errorf("the first gh on the daemon's PATH, %s, is a script, not the real gh, which the daemon refuses to boot on", gh)
			}
			return gh, nil
		}
	}
	return "", fmt.Errorf("no gh on the daemon's PATH %q: the daemon resolves the real gh every pane's `legion gh` runs at boot", path)
}

// firstBytes is up to n bytes from the start of path.
func firstBytes(path string, n int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, n)
	read, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:read], nil
}

// rolePromptInputs is what a launch of c composes its prompt from, each the daemon's own answer:
// the paths of the role's prompt files, snapshotted under stateDir, and its addressing for the
// rig's repository, which for the architect of a tree's root ends with the design gate steps of a
// deployment that leaves `gates.design` unset (config.Gates.Steps), as workerSpawnSpec appends
// them. They are the prompt of a deployment with no instructions file and no experiment, whose
// roles run on Oh My Pi, a backend whose launch is told no prompt part of its own.
func rolePromptInputs(stateDir, project string, c supervise.Claim) (paths []string, addressing string, err error) {
	composer, err := prompts.New(stateDir)
	if err != nil {
		return nil, "", err
	}
	parts, err := composer.Compose(c.Role, claim.IsTreeRoot(c.Issue, c.Tree))
	if err != nil {
		return nil, "", err
	}
	repository, err := ghrepo.Parse("the rig's repository", rigRepository)
	if err != nil {
		return nil, "", err
	}
	addressing, err = daemon.AddressingFragment(project, c, repository, rigDefaultBranch, []string{rigOwner})
	if err != nil {
		return nil, "", err
	}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		addressing += " " + daemon.DesignGateFragment(config.Gates{}.Steps())
	}
	return parts.RolePromptPaths, addressing, nil
}

// rolePrompt is the system prompt a launch of c starts with (rolePromptInputs): the role's parts
// concatenated with the newlines that end them dropped, a blank line, then its addressing. The
// join is this program's own copy of the daemon's (runtime.ComposePrompt), since the rig runs it
// against each label's checkout of the daemon module, an older one among them, which lacks it.
func rolePrompt(stateDir, project string, c supervise.Claim) (string, error) {
	paths, addressing, err := rolePromptInputs(stateDir, project, c)
	if err != nil {
		return "", err
	}
	var body strings.Builder
	for _, path := range paths {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		body.Write(text)
	}
	return strings.TrimRight(body.String(), "\n") + "\n\n" + addressing, nil
}

// systemPromptArgument writes the pane's system prompt (rolePrompt; the pane's tree is its issue)
// to the claim's prompt file, as a pane's launch does, and answers the argument that names that
// file. It refuses a tree architect: a pane runs against the rig's stand-in daemon, which serves a
// phase worker's routes alone and none of the tree architect's, its design gate's among them. The
// write is this program's own copy of the daemon's (runtime.WriteSystemPrompt), since the rig runs
// it against each label's checkout of the daemon module, an older one among them, which lacks it.
func systemPromptArgument(req paneRequest, token claim.Token) (string, error) {
	role := claim.Role(req.Role)
	if claim.IsTreeArchitect(role, req.Issue, req.Issue) {
		return "", errors.New("a tree architect is also told its design gate policy, which the rig does not stand in for")
	}
	prompt, err := rolePrompt(req.StateDir, req.Project, supervise.Claim{
		Token: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1,
	})
	if err != nil {
		return "", err
	}
	promptFile := filepath.Join(req.StateDir, "secrets", string(token)+"-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(promptFile), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return "--append-system-prompt " + shellprefix.Word(promptFile), nil
}
