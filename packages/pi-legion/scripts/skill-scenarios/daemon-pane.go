// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane, computed by the daemon's own functions, for worker-pane.ts and
// daemon-standin.ts to run Oh My Pi and the stand-in daemon under, and the prompt a launch of any
// role starts with, for a reader of that prompt. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from, with daemon-pane-architect.go
// beside it where that checkout's daemon declares the function that file calls.
//
//	phase <role>             prints the phase the role's worker runs in, the first phase
//	                         workflow.RoleFor gives to the role, then on a second line the word of
//	                         the handoff record that phase ends with (handoffWords), empty for a
//	                         phase that ends with none
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh bundle
//	                         under its LEGION_GH_OWNERS_DIR, and prints its paneResponse (JSON);
//	                         it refuses the system prompt of a tree architect (systemPromptArgument)
//	prompt <role> <root|child>
//	                         prints the system prompt a launch of the role starts with
//	                         (rolePrompt), byte for byte as the daemon writes its prompt file, for
//	                         the rig's claim on the root issue of its tree or on a child under it;
//	                         an architect's prompt ends with what the daemon tells every architect
//	                         after its addressing (daemon.ArchitectAddressing) under the rig's
//	                         deployment (rigRoles), which lists no model and sets no `gates.design`
//	                         or `production_check`, as such a launch's does: the deployment's roles
//	                         and models, then, for the root's architect, the design gate steps,
//	                         then the production check; it fails, naming that function, in a
//	                         checkout whose daemon lacks it (architectAddressing)
//	prompt step <prompt_file> [<role>]
//	                         prints the system prompt a launch of a step of a role of the rig's
//	                         deployment starts with (stepPrompt), the checker's or the role named.
//	                         That role's prompt_file is the file given. The prompt is for the
//	                         rig's claim on a child issue, byte for byte as the daemon writes its
//	                         prompt file. Where the checkout's daemon has no key `roles`, it
//	                         exits 1 and prints no prompt
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

const usage = "usage: daemon-pane phase <role> | pane < request.json | prompt <role> <root|child> | prompt step <prompt_file> [checker|critic]"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case len(args) == 1 && args[0] == "pane":
		err = pane()
	case (len(args) == 3 || len(args) == 4) && args[0] == "prompt" && args[1] == "step":
		role := rigRoles[0].name
		if len(args) == 4 {
			role = args[3]
		}
		err = printStepPrompt(args[2], claim.Role(role))
	case len(args) == 3 && args[0] == "prompt" && (args[2] == "root" || args[2] == "child"):
		err = printPrompt(claim.Role(args[1]), args[2] == "root")
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon-pane %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

// handoffWords is the word of the handoff each handoff phase ends with: one of this program's
// restatements of the daemon (daemon-pane.ts names each), and its own copy rather than the
// daemon's phase table because one program runs against each label's checkout of the daemon
// module, and that table has had more than one name (phase.HandoffFile, then phase.Handoff).
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

// rigRole is a role of the rig's deployment, as a deployment's settings define one
// (`roles.<name>`): its name, the type of its steps, and its model, "" where the settings name
// none. It is this program's own type rather than the daemon's config.Role, which an older
// checkout of the daemon module lacks.
type rigRole struct{ name, typ, model string }

// rigRoles is the rig's deployment: a checker, whose steps are of the type testing and who works
// as the implement App (`github_app: implement`), then a critic, of the type review, who works as
// the review App, the App of a role whose settings name none. Neither names a model. No prompt
// names a role's App: every claim of the rig is told the one owner of the rig's project.
var rigRoles = []rigRole{{name: "checker", typ: "testing"}, {name: "critic", typ: "review"}}

// printPrompt prints the system prompt a launch of role starts with (rolePrompt), for the rig's
// claim on the root issue of its tree (root) or on a child issue under it, and nothing else: no
// newline is added, so the output is the text of the claim's prompt file.
func printPrompt(role claim.Role, root bool) error {
	issue := rigChild
	if root {
		issue = rigRoot
	}
	token, err := claim.NewToken(rigProject, issue, role)
	if err != nil {
		return err
	}
	stateDir, err := os.MkdirTemp("", "daemon-pane-prompt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	prompt, err := rolePrompt(stateDir, rigProject, supervise.Claim{
		Token: token, Project: rigProject, Tree: rigRoot, Issue: issue, Role: role, Generation: 1,
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, prompt)
	return err
}

// printStepPrompt prints the system prompt a launch of a step of role, a role of the rig's
// deployment, starts with (stepPrompt), whose prompt_file is promptFile, for the rig's claim on a
// child issue, and nothing else, as printPrompt prints a role's. A checkout whose daemon has no key
// `roles` starts no role of a deployment, so the mode refuses there and prints no prompt.
func printStepPrompt(promptFile string, role claim.Role) error {
	if !definesRoles() {
		return errors.New("this checkout's daemon has no key roles, so no launch of it starts a role of a deployment")
	}
	names := make([]string, len(rigRoles))
	for i, defined := range rigRoles {
		names[i] = defined.name
	}
	if !slices.Contains(names, string(role)) {
		return fmt.Errorf("%q is no role of the rig's deployment (%s)", role, strings.Join(names, ", "))
	}
	file, err := filepath.Abs(promptFile)
	if err != nil {
		return err
	}
	token, err := claim.NewToken(rigProject, rigChild, role)
	if err != nil {
		return err
	}
	stateDir, err := os.MkdirTemp("", "daemon-pane-prompt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)
	prompt, err := stepPrompt(stateDir, rigProject, supervise.Claim{
		Token: token, Project: rigProject, Tree: rigRoot, Issue: rigChild, Role: role, Generation: 1,
	}, file)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, prompt)
	return err
}

// definesRoles says whether this checkout's daemon has the top-level key `roles`, the key a
// deployment defines roles of its own with: an entry of its key table (config.ConfigReference)
// named so. A nested key of that name, such as a hook's `roles`, stands under an entry's Nested
// and does not count.
func definesRoles() bool {
	for _, key := range config.ConfigReference() {
		if key.Name == "roles" {
			return true
		}
	}
	return false
}

// rigProductionCheck is the production check of the rig's deployment, which sets no
// `production_check`: the tester, the role config's readProject gives a project that names no
// such key. daemon-pane_test.go holds it to the key table's default.
const rigProductionCheck = claim.RoleTester

// stepPromptPaths is the prompt of a step of a role of the deployment, as the paths of its parts:
// the headless part every phase worker is told, as prompts.New(stateDir) snapshots it, then
// promptFile, the role's own prompt (`roles.<name>.prompt_file`), then the daemon's part every
// phase worker ends with (worker-common.md). It restates prompts.Composer.ComposeStep, since one
// program runs against each label's checkout of the daemon module, and an older one lacks that
// method. It is that method's paths at the commit this file is part of, which daemon-pane_test.go
// holds it equal to. A checkout whose daemon composes a step otherwise is composed with that
// checkout's own copy of this program.
func stepPromptPaths(stateDir string, composer *prompts.Composer, promptFile string) []string {
	return []string{
		filepath.Join(stateDir, "prompts", "shared", "mechanics", "headless.md"),
		promptFile,
		composer.Part("worker-common.md"),
	}
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

// architectAddressing is what the architect of c is told after the sentence every launch is told,
// as the daemon composes it for the rig's deployment (daemon.ArchitectAddressing, through
// daemon-pane-architect.go, which sets it). It lives in a file of its own because a checkout of the
// daemon module older than that function does not declare it, and a call to it would keep this
// program from building there in every mode: daemon-pane.ts builds that file in only where the
// checkout's daemon declares the function. It is nil in a build without it, and joinedPrompt then
// refuses an architect rather than compose a prompt that lacks what its daemon tells one.
var architectAddressing func(c supervise.Claim) string

// rolePrompt is the system prompt a launch of c starts with, composed as the daemon composes it
// (specs.SpawnSpec): the role prompts snapshotted under stateDir, then the role's parts and its
// addressing joined as joinedPrompt joins them. It is the prompt of a deployment with no
// instructions file and no experiment, whose roles run on Oh My Pi, a backend whose launch is told
// no prompt part of its own.
func rolePrompt(stateDir, project string, c supervise.Claim) (string, error) {
	composer, err := prompts.New(stateDir)
	if err != nil {
		return "", err
	}
	parts, err := composer.Compose(c.Role, claim.IsTreeRoot(c.Issue, c.Tree))
	if err != nil {
		return "", err
	}
	return joinedPrompt(parts.RolePromptPaths, project, c)
}

// stepPrompt is the system prompt a launch of a step of c's role, a role of the deployment whose
// prompt_file is promptFile, starts with, composed as the daemon composes it: the role prompts
// snapshotted under stateDir, then the step's parts (stepPromptPaths) and its addressing joined as
// joinedPrompt joins them, for the same deployment as rolePrompt's.
func stepPrompt(stateDir, project string, c supervise.Claim, promptFile string) (string, error) {
	composer, err := prompts.New(stateDir)
	if err != nil {
		return "", err
	}
	return joinedPrompt(stepPromptPaths(stateDir, composer, promptFile), project, c)
}

// joinedPrompt is the text of the files at paths concatenated, with the newlines that end them
// dropped, a blank line, then c's addressing for the rig's repository, which for an architect ends
// with what the daemon tells every architect after it (architectAddressing), as workerSpawnSpec
// appends it. The join is this program's own copy of the daemon's (runtime.ComposePrompt), since
// the rig runs it against each label's checkout of the daemon module, an older one among them,
// which lacks it.
func joinedPrompt(paths []string, project string, c supervise.Claim) (string, error) {
	repository, err := ghrepo.Parse("the rig's repository", rigRepository)
	if err != nil {
		return "", err
	}
	addressing, err := daemon.AddressingFragment(project, c, repository, rigDefaultBranch, []string{rigOwner})
	if err != nil {
		return "", err
	}
	if c.Role == claim.RoleArchitect {
		if architectAddressing == nil {
			return "", errors.New("this checkout has no daemon.ArchitectAddressing, so the rig cannot compose an architect's prompt; " +
				"run the rig from a checkout that carries it")
		}
		addressing += architectAddressing(c)
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
