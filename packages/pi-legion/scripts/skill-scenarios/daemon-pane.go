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
//	prompt <role> <root|child>
//	                         prints the system prompt a launch of the role starts with
//	                         (rolePrompt), byte for byte as the daemon writes its prompt file, for
//	                         the rig's claim on the root issue of its tree or on a child under it;
//	                         an architect's addressing goes on with the roles of the rig's
//	                         deployment (rigRoles, deploymentRolesLine) where the checkout's daemon
//	                         has the key `roles`, a root architect's then with the design gate
//	                         steps of a deployment that sets no `gates.design`, and every
//	                         architect's ends with the production check of one that sets no
//	                         `production_check` (productionCheckLine) where the daemon has that
//	                         key, as such a launch's does
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

// legionStepRoles are Legion's own five roles of a step, each with the type of its step, in the
// order the daemon's sentence of the deployment's roles lists them (deploymentRolesLine).
var legionStepRoles = []rigRole{
	{name: "planner", typ: "planning"}, {name: "implementer", typ: "implementation"}, {name: "tester", typ: "testing"},
	{name: "reviewer", typ: "review"}, {name: "acceptance", typ: "review"},
}

// deploymentRolesLine is the sentence every architect is told after its addressing, the roles a
// step of its plan may name: Legion's five (legionStepRoles), then each of roles in its order,
// `<name> (<type>)`, with ` with the model <model>` where the role names one. It restates the
// daemon's daemon.DeploymentRolesFragment, since one program runs against each label's checkout of
// the daemon module, and an older one lacks that function and config.Role. It is that function's
// sentence at the commit this file is part of, which daemon-pane_test.go holds it equal to. A
// checkout whose daemon tells another is composed with that checkout's own copy of this program.
func deploymentRolesLine(roles []rigRole) string {
	entries := make([]string, 0, len(legionStepRoles)+len(roles))
	for _, role := range slices.Concat(legionStepRoles, roles) {
		entry := fmt.Sprintf("%s (%s)", role.name, role.typ)
		if role.model != "" {
			entry += " with the model " + role.model
		}
		entries = append(entries, entry)
	}
	return "The roles of this deployment, each with the type of its step: " + strings.Join(entries, ", ") + "."
}

// rigProductionCheck is the production check of the rig's deployment, which sets no
// `production_check`: the tester, the role config's readProject gives a project that names no
// such key. daemon-pane_test.go holds it to the key table's default.
const rigProductionCheck = claim.RoleTester

// checksProduction says whether this checkout's daemon has the project key `production_check`: a
// nested key of its key table's entry `projects` (config.ConfigReference), as definesRoles reads
// `roles`. Where it has the key, every architect is told the production check last in its
// addressing (productionCheckLine). The key table holds no sign of that sentence itself, so a
// checkout whose daemon has the key and tells no such sentence, one from the key table
// (7ed02373162) to the sentence (b537812fb53, #3995), is composed here with the sentence all the
// same.
func checksProduction() bool {
	for _, key := range config.ConfigReference() {
		if key.Name != "projects" {
			continue
		}
		for _, nested := range key.Nested {
			if nested.Path == "projects.<KEY>" && nested.Name == "production_check" {
				return true
			}
		}
	}
	return false
}

// productionCheckLine is the sentence every architect is told last in its addressing, after the
// roles of its deployment and, for the architect of a tree's root, after the design gate steps:
// the project's `production_check` (check), the role that works the production check a merge
// opens, or `off` for the empty role. It restates the daemon's daemon.ProductionCheckFragment,
// since one program runs against each label's checkout of the daemon module, and an older one
// lacks that function. It is that function's sentence at the commit this file is part of, which
// daemon-pane_test.go holds it equal to.
func productionCheckLine(check claim.Role) string {
	word := string(check)
	if word == "" {
		word = "off"
	}
	return "Production check: " + word + "."
}

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
// dropped, a blank line, then c's addressing for the rig's repository. An architect's addressing
// goes on with the roles of the rig's deployment (deploymentRolesLine) where the checkout's daemon
// has the key `roles`, that of the architect of a tree's root then with the design gate steps of a
// deployment that leaves `gates.design` unset (config.Gates.Steps), and every architect's ends with
// the production check of the rig's deployment (productionCheckLine, rigProductionCheck) where the
// daemon has the key `production_check`, as workerSpawnSpec appends them. The join is this
// program's own copy of the daemon's (runtime.ComposePrompt), since the rig runs it against each
// label's checkout of the daemon module, an older one among them, which lacks it.
func joinedPrompt(paths []string, project string, c supervise.Claim) (string, error) {
	repository, err := ghrepo.Parse("the rig's repository", rigRepository)
	if err != nil {
		return "", err
	}
	addressing, err := daemon.AddressingFragment(project, c, repository, rigDefaultBranch, []string{rigOwner})
	if err != nil {
		return "", err
	}
	if c.Role == claim.RoleArchitect && definesRoles() {
		addressing += " " + deploymentRolesLine(rigRoles)
	}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		addressing += " " + daemon.DesignGateFragment(config.Gates{}.Steps())
	}
	if c.Role == claim.RoleArchitect && checksProduction() {
		addressing += " " + productionCheckLine(rigProductionCheck)
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
