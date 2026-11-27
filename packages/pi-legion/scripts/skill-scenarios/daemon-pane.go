// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane (and the daemon-launched controller's), computed by the daemon's own functions,
// for worker-pane.ts and daemon-standin.ts to run Oh My Pi and the stand-in daemon under, and the
// prompt a launch of any role starts with, for a reader of that prompt. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from.
//
//	phase <role>             prints the phase the role's worker runs in, the first phase
//	                         workflow.RoleFor gives to the role, then on a second line the word of
//	                         the handoff record that phase ends with (handoffWords), empty for a
//	                         phase that ends with none
//	handoff-words            prints the word of the handoff record each phase ends with
//	                         (handoffWords) as one JSON object, phase to word
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh bundle
//	                         under its LEGION_GH_OWNERS_DIR, and prints its paneResponse (JSON);
//	                         it refuses the interactive controller, whose environment is `legion
//	                         controller start`'s and not a pane's
//	prompt <role> <position> [<backend>]
//	                         prints the system prompt a launch of the role starts with
//	                         (rolePrompt), byte for byte as the daemon writes its prompt file, for
//	                         the rig's claim: <position> is root or child for every role but the
//	                         controller (the claim on the root issue of the rig's tree, or on a
//	                         child under it), and headless or interactive for the controller (the
//	                         daemon's own launch under `controller: daemon`, or `legion controller
//	                         start`'s); <backend> is omp (the default) or claude, the backend the
//	                         role runs on, whose own prompt part follows the role's parts. A tree
//	                         architect's prompt ends with the design gate steps of a deployment
//	                         that sets no `gates.design`, and every architect's with the
//	                         `production_check` sentence of a project that sets none, as such a
//	                         launch's does, where the module's daemon tells an architect that
//	                         sentence at all (productionCheckFragment)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

const usage = "usage: daemon-pane phase <role> | handoff-words | pane < request.json | prompt <role> <root|child|headless|interactive> [omp|claude]"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case len(args) == 1 && args[0] == "handoff-words":
		err = json.NewEncoder(os.Stdout).Encode(handoffWords)
	case len(args) == 1 && args[0] == "pane":
		err = pane()
	case (len(args) == 3 || len(args) == 4) && args[0] == "prompt":
		backend := ""
		if len(args) == 4 {
			backend = args[3]
		}
		err = printPrompt(claim.Role(args[1]), args[2], backend)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-pane %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

// handoffWords is the word of the handoff each handoff phase ends with: this program's one
// restatement of the daemon, and its own copy rather than the daemon's phase table because one
// program runs against each label's checkout of the daemon module, and that table has had more
// than one name (phase.HandoffFile, then phase.Handoff). Every other answer here is the daemon's
// own function (daemon-pane.ts).
var handoffWords = map[phase.Phase]string{
	phase.Planning: "plan", phase.Implementing: "implement", phase.Testing: "test", phase.Reviewing: "review",
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

// The two launches of the controller: the daemon's own, under `controller: daemon`, and the
// operator's, `legion controller start`.
const (
	controllerHeadless    = "headless"
	controllerInteractive = "interactive"
)

// launch is one launch of the daemon, which every prompt of this program is composed for: the
// claim it starts, how the controller's launch starts (controllerHeadless or
// controllerInteractive, "" for every other role) and the backend the role runs on.
type launch struct {
	claim      supervise.Claim
	controller string
	backend    runtime.Backend
	// repository and defaultBranch are the issue's repository and its default branch, which the
	// pane's addressing and gh bundle name; "" is the rig's own (rigRepository, rigDefaultBranch).
	repository    string
	defaultBranch string
}

// where answers the launch's repository, its default branch and the repository's owner, the
// rig's own where the launch names none.
func (l launch) where() (repository, branch, owner string) {
	repository, branch = l.repository, l.defaultBranch
	if repository == "" {
		repository = rigRepository
	}
	if branch == "" {
		branch = rigDefaultBranch
	}
	owner, _, _ = strings.Cut(repository, "/")
	return repository, branch, owner
}

// newLaunch is the launch of role on the issue of project whose tree's root is tree ("" is the
// issue itself), on backend ("" is Oh My Pi), refused as the daemon refuses it: a backend that is
// none, a backend that does not run the role (`worker_backends`), and a controller that names an
// issue or a tree, or no way it starts. The controller's claim is the project's, with no issue and
// no tree.
func newLaunch(project, issue, tree, role, controller, backend string) (launch, error) {
	r := claim.Role(role)
	l := launch{controller: controller, backend: runtime.Backend(backend)}
	info, err := runtime.LookupBackend(l.backend)
	if err != nil {
		return launch{}, err
	}
	if !info.Runs(r) {
		return launch{}, fmt.Errorf("the %s backend does not run the %s: it runs %s alone, since %s", info.Name, r, runtime.JoinRoles(info.Roles), info.Why)
	}
	if r == claim.RoleController {
		if issue != "" || tree != "" {
			return launch{}, fmt.Errorf("the controller has no issue and no tree, and this launch names %q under %q", issue, tree)
		}
		if controller != controllerHeadless && controller != controllerInteractive {
			return launch{}, fmt.Errorf("the controller starts %s or %s, not %q", controllerHeadless, controllerInteractive, controller)
		}
		l.claim = supervise.Claim{Token: claim.ControllerToken(project), Project: project, Role: r, Generation: 1}
		return l, nil
	}
	if controller != "" {
		return launch{}, fmt.Errorf("only the controller starts %s or %s, and this launch is of the %s", controllerHeadless, controllerInteractive, r)
	}
	if tree == "" {
		tree = issue
	}
	token, err := claim.NewToken(project, issue, r)
	if err != nil {
		return launch{}, err
	}
	l.claim = supervise.Claim{Token: token, Project: project, Tree: tree, Issue: issue, Role: r, Generation: 1}
	return l, nil
}

// printPrompt prints the system prompt a launch of role starts with (rolePrompt), for the rig's
// claim on the root issue of its tree (position root) or on a child issue under it (child), or,
// for the controller, its headless or its interactive launch, on backend ("" is Oh My Pi), and
// nothing else: no newline is added, so the output is the text of the claim's prompt file.
func printPrompt(role claim.Role, position, backend string) error {
	var l launch
	var err error
	switch {
	case role == claim.RoleController:
		l, err = newLaunch(rigProject, "", "", string(role), position, backend)
	case position == "root":
		l, err = newLaunch(rigProject, rigRoot, rigRoot, string(role), "", backend)
	case position == "child":
		l, err = newLaunch(rigProject, rigChild, rigRoot, string(role), "", backend)
	default:
		err = fmt.Errorf("the %s's claim is on the root or on a child, not %q", role, position)
	}
	if err != nil {
		return err
	}
	stateDir, err := os.MkdirTemp("", "daemon-pane-prompt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	prompt, err := rolePrompt(stateDir, l)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, prompt)
	return err
}

// paneRequest is the claim a pane is launched for, and the rig's stand-ins for what the daemon
// knows at boot. The pane's tree is Tree, its issue when none is named. The controller's pane
// names no issue and no tree.
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
	// Tree is the root of the tree the pane's issue is in: "" for a claim on a root, the root's
	// key for a child's.
	Tree string `json:"tree"`
	// Controller is how the controller's launch starts, controllerHeadless (the pane of the
	// daemon's own controller) or controllerInteractive, which has no pane here; "" for every
	// other role.
	Controller string `json:"controller"`
	// Backend is the backend the role runs on, "omp" or "claude"; "" is Oh My Pi.
	Backend string `json:"backend"`
	// Repository and DefaultBranch are the issue's repository (owner/name) and its default
	// branch, as the daemon's record of the issue holds them: the case world's, for a case run.
	// "" is the tester-proof world's (rigRepository, rigDefaultBranch).
	Repository    string `json:"repository"`
	DefaultBranch string `json:"defaultBranch"`
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
	l, err := newLaunch(req.Project, req.Issue, req.Tree, req.Role, req.Controller, req.Backend)
	if err != nil {
		return err
	}
	l.repository, l.defaultBranch = req.Repository, req.DefaultBranch
	_, _, owner := l.where()
	if l.controller == controllerInteractive {
		return errors.New("the interactive controller runs in the operator's terminal under `legion controller start`, whose environment is its own (controllerEnvironment), not a pane's, and the rig does not stand in for it; `prompt controller interactive` composes its prompt")
	}
	role, token := l.claim.Role, l.claim.Token
	ghRoot, gh := "", ""
	if req.GHToken != "" {
		// ghconfig's one writer of a role's gh bundle, so the rig's directory is the daemon's layout;
		// the App label and expiry only feed the runtime's log lines, which the rig does not write. The
		// rig's project serves one repository of one owner, so its bundle holds that owner's directory
		// alone, where a daemon serving more holds a `<owner>/<repo>` directory beside it per sibling
		// repository.
		ghRoot = runtime.GHConfigDir(req.StateDir, token)
		bundle := ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: ghconfig.Render(req.GHToken, "", time.Time{})}}
		if _, err := ghconfig.WriteBundle(ghRoot, bundle); err != nil {
			return fmt.Errorf("write the claim's gh files: %w", err)
		}
		if gh, err = realGH(req.GHPath, req.Path); err != nil {
			return err
		}
	}
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: l.claim.Tree, Issue: l.claim.Issue, Role: role, Generation: 1}
	variables, err := tmux.PaneVariables(spec, tmux.PaneInputs{
		StateDir: req.StateDir, Workspace: req.Workspace, DaemonURL: req.DaemonURL, EnvoyURL: req.EnvoyURL,
		NATSURLs: req.NATSURLs, GHOwnersDir: ghRoot, GHDefault: owner, GH: gh,
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
		if res.SystemPromptArgument, err = systemPromptArgument(req, l); err != nil {
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

// productionCheckFragment is the module's own daemon.ProductionCheckFragment, the sentence the
// daemon appends to every architect's addressing (workerSpawnSpec), or nil where the module's
// package daemon declares no such function: that daemon tells no architect the sentence. The
// program is built against each label's checkout of the daemon module, and one older than the
// function would not build with a call of it here, so daemon-pane.ts sets it from an overlay file
// it adds only where the module declares the function (PRODUCTION_CHECK_HOOK,
// declaresProductionCheck).
var productionCheckFragment func(claim.Role) string

// rigProductionCheck is the `production_check` of the rig's project, which sets none: the config
// reads that as the tester (config.Project.ProductionCheck).
const rigProductionCheck = claim.RoleTester

// rolePrompt is the system prompt a launch l starts with, composed as the daemon composes it: for
// the controller (controllerSpawnSpec, and `legion controller start` for the interactive launch)
// its role part and its launch's part (prompts.ControllerPromptPaths), then its addressing, the
// design gate steps of a deployment that leaves `gates.design` unset (config.Gates.Steps) and the
// reach of its App (daemon.RepositoriesSentence); for every other role (specs.SpawnSpec) the
// role's parts (prompts.Compose), then its addressing for the rig's repository
// (daemon.AddressingFragment), which for the architect of a tree's root ends with the design gate
// steps, as workerSpawnSpec appends them, and for every architect with the `production_check`
// sentence of the rig's project, where the module's daemon has one (productionCheckFragment).
// After the parts of either comes the prompt part of the backend the role runs on
// (runtime.Backends, prompts.Composer.Part), none for Oh My Pi. It is the prompt of a deployment
// with no instructions file and no experiment, the role prompts snapshotted under stateDir, the
// parts concatenated with the newlines that end them dropped, a blank line, then the addressing.
// The join is this program's own copy of the daemon's (runtime.ComposePrompt), since the rig runs
// it against each label's checkout of the daemon module, an older one among them, which lacks it.
func rolePrompt(stateDir string, l launch) (string, error) {
	c := l.claim
	composer, err := prompts.New(stateDir)
	if err != nil {
		return "", err
	}
	var paths []string
	if c.Role == claim.RoleController {
		if paths, err = composer.ControllerPromptPaths(l.controller == controllerHeadless); err != nil {
			return "", err
		}
	} else {
		parts, err := composer.Compose(c.Role, claim.IsTreeRoot(c.Issue, c.Tree))
		if err != nil {
			return "", err
		}
		paths = parts.RolePromptPaths
	}
	info, err := runtime.LookupBackend(l.backend)
	if err != nil {
		return "", err
	}
	if info.PromptPart != "" {
		paths = append(paths, composer.Part(info.PromptPart))
	}
	var body strings.Builder
	for _, path := range paths {
		text, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("prompt file: %w", err)
		}
		body.Write(text)
	}
	steps := config.Gates{}.Steps()
	name, branch, owner := l.where()
	var addressing string
	if c.Role == claim.RoleController {
		addressing = daemon.DesignGateFragment(steps) + " " + daemon.RepositoriesSentence([]string{owner})
	} else {
		repository, err := ghrepo.Parse("the rig's repository", name)
		if err != nil {
			return "", err
		}
		if addressing, err = daemon.AddressingFragment(c.Project, c, repository, branch, []string{owner}); err != nil {
			return "", err
		}
		if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
			addressing += " " + daemon.DesignGateFragment(steps)
		}
		if c.Role == claim.RoleArchitect && productionCheckFragment != nil {
			addressing += " " + productionCheckFragment(rigProductionCheck)
		}
	}
	return strings.TrimRight(body.String(), "\n") + "\n\n" + addressing, nil
}

// systemPromptArgument writes the pane's system prompt (rolePrompt, for the launch l) to the
// claim's prompt file, as a pane's launch does, and answers the argument that names that file. The
// argument is Oh My Pi's, `--append-system-prompt <file>`, whatever backend the prompt is composed
// for: the rig's agents run on Oh My Pi, and a pane that runs the claude backend's prompt under it
// is the rig's choice, not the daemon's. The write is this program's own copy of the daemon's
// (runtime.WriteSystemPrompt), since the rig runs it against each label's checkout of the daemon
// module, an older one among them, which lacks it.
func systemPromptArgument(req paneRequest, l launch) (string, error) {
	prompt, err := rolePrompt(req.StateDir, l)
	if err != nil {
		return "", err
	}
	promptFile := filepath.Join(req.StateDir, "secrets", string(l.claim.Token)+"-system-prompt.md")
	if err := os.MkdirAll(filepath.Dir(promptFile), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return "--append-system-prompt " + shellprefix.Word(promptFile), nil
}
