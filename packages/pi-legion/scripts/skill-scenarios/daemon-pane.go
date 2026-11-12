// Daemon-pane is the skill-scenario rig's one reading of the Legion daemon: what the daemon gives a
// phase-worker pane, computed by the daemon's own functions, for worker-pane.ts and
// daemon-standin.ts to run Oh My Pi and the stand-in daemon under. It belongs to no module:
// daemon-pane.ts runs it with `go run -overlay` as a main inside a checkout's packages/daemon, the
// one place the daemon's internal packages can be imported from.
//
//	phase <role>             prints the phase the role's worker runs in, then on a second line the
//	                         word of the handoff record that phase ends with (handoffWords), empty
//	                         for a phase that ends with none: the first phase workflow.RoleFor
//	                         gives to the role
//	refusal not-current-phase <role> <phase> <issue>
//	                         prints the code of the daemon's refusal of a completion of a phase
//	                         the role does not run, then on a second line the daemon's own
//	                         message for it, read out of the daemon's source (notCurrentPhase)
//	pane                     reads a paneRequest (JSON) on stdin, writes the claim's gh bundle
//	                         under its LEGION_GH_OWNERS_DIR, and prints its paneResponse (JSON)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const usage = "usage: daemon-pane phase <role> | refusal not-current-phase <role> <phase> <issue> [<engine source>] | pane < request.json"

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "phase":
		err = printPhase(claim.Role(args[1]))
	case (len(args) == 5 || len(args) == 6) && args[0] == "refusal" && args[1] == "not-current-phase":
		// The engine source the refusal is read out of: the daemon's own, and the one a caller
		// names otherwise. A caller names one so that a test can hold this reading to a
		// substitute engine.go and watch each guard below fire — the guards answer a daemon
		// that writes the refusal in some other shape, which today's daemon does not, so
		// without a source of its own not one of them can be reached (daemon-pane.test.ts).
		// The rig itself names none and reads the daemon's file.
		engine := defaultEngineFile
		if len(args) == 6 {
			engine = args[5]
		}
		err = printNotCurrentPhase(args[2], args[3], args[4], engine)
	case len(args) == 1 && args[0] == "pane":
		err = pane()
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

// printPhase prints the first phase of the transition table `role` works and that phase's handoff
// word.
func printPhase(role claim.Role) error {
	for _, p := range phase.All {
		if workflow.RoleFor(p) != role {
			continue
		}
		fmt.Println(p)
		fmt.Println(handoffWords[p])
		return nil
	}
	return fmt.Errorf("%q works no phase", role)
}

// defaultEngineFile is where the daemon builds the workflow's refusals, read relative to the
// module this program is run inside (daemon-pane.ts). Every reader below takes the file as an
// argument rather than this constant, so a caller — a test — can hold the reading to a
// substitute of its own.
const defaultEngineFile = "internal/workflow/engine.go"

// notCurrentPhaseCode is the refusal the daemon answers a completion of a phase the completing
// role does not run with.
const notCurrentPhaseCode = "HANDOFF_NOT_CURRENT_PHASE"

// printNotCurrentPhase prints that refusal's code and the daemon's own message for it, for the
// stand-in to answer such a completion with (daemon-standin.ts).
func printNotCurrentPhase(role, issuePhase, issue, engineFile string) error {
	message, err := notCurrentPhase(role, issuePhase, issue, engineFile)
	if err != nil {
		return err
	}
	fmt.Println(notCurrentPhaseCode)
	fmt.Println(message)
	return nil
}

// notCurrentPhase is the sentence the daemon sends a worker whose completion names a phase its
// role does not run, read out of the daemon's own code rather than restated here: the skill has
// the worker quote that sentence to its architect, so a stand-in answering a sentence of its own
// would teach every agent a message the daemon never sends, and a copy goes stale the day the
// daemon's wording moves with nothing to say so. The engine builds it in one place,
// `refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf(<format>, <arguments>...))`, so this reads
// that call's format and fills it with the arguments the engine itself names. A daemon that
// writes the refusal in some other shape is an error here, never a sentence this invents.
func notCurrentPhase(role, issuePhase, issue, engineFile string) (string, error) {
	format, arguments, err := refusalMessage(notCurrentPhaseCode, engineFile)
	if err != nil {
		return "", err
	}
	named := map[string]string{"fact.Role": role, "issue.Phase": issuePhase, "issue.Key": issue}
	values := make([]any, 0, len(arguments))
	for _, argument := range arguments {
		value, ok := named[argument]
		if !ok {
			return "", fmt.Errorf("%s fills the %s message with %s, which this program has no value for", engineFile, notCurrentPhaseCode, argument)
		}
		values = append(values, value)
	}
	message := fmt.Sprintf(format, values...)
	if strings.Contains(message, "%!") {
		return "", fmt.Errorf("%s's %s message %q takes other arguments than the %d it names", engineFile, notCurrentPhaseCode, format, len(arguments))
	}
	return message, nil
}

// refusalMessage is the format string and the argument expressions, as the daemon writes them,
// of the engine's `refused(code, fmt.Sprintf(…))` call for `code`.
func refusalMessage(code, engineFile string) (string, []string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), engineFile, nil, 0)
	if err != nil {
		return "", nil, fmt.Errorf("read the daemon's %s: %w", engineFile, err)
	}
	format, arguments, found := "", []string(nil), false
	ast.Inspect(file, func(node ast.Node) bool {
		if found {
			return false
		}
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		name, isName := call.Fun.(*ast.Ident)
		if !isName || name.Name != "refused" || len(call.Args) != 2 {
			return true
		}
		if refused, isText := stringLiteral(call.Args[0]); !isText || refused != code {
			return true
		}
		sprintf, isSprintf := call.Args[1].(*ast.CallExpr)
		if !isSprintf {
			return true
		}
		printer, isSelector := sprintf.Fun.(*ast.SelectorExpr)
		if !isSelector || printer.Sel.Name != "Sprintf" || len(sprintf.Args) == 0 {
			return true
		}
		text, isText := stringLiteral(sprintf.Args[0])
		if !isText {
			return true
		}
		format, found = text, true
		for _, argument := range sprintf.Args[1:] {
			arguments = append(arguments, types.ExprString(argument))
		}
		return false
	})
	if !found {
		return "", nil, fmt.Errorf("%s holds no refused(%q, fmt.Sprintf(…)) call: the daemon writes that refusal some other way now", engineFile, code)
	}
	return format, arguments, nil
}

// stringLiteral is the text of an interpreted string literal, and false for any other expression.
func stringLiteral(expression ast.Expr) (string, bool) {
	literal, isLiteral := expression.(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(literal.Value)
	return text, err == nil
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
	// into the claim's gh bundle (runtime.GHConfigDir), the one directory of Repository's owner,
	// which the pane's GH_CONFIG_DIR names, as the daemon's tmux runtime writes it at spawn; the
	// pane's LEGION_GH_PATH is realGH's: GHPath when the rig names one, else the first gh on
	// Path, refused when that is a script, as the daemon resolves it at boot. "" is a daemon
	// with no GitHub credential: no files, and no gh variable on the pane.
	GHToken string `json:"ghToken"`
	// Path is the PATH the daemon would start under; the pane's PATH is it behind the pane's
	// launcher directory (workerbin.Path).
	Path string `json:"path"`
	// Repository is the GitHub repository of the run's own world and DefaultBranch the branch
	// that world opens its pull request against. The pane's addressing names them, as a real
	// pane's names its issue's, and its gh bundle is the directory of this repository's owner.
	// Why they are the world's own and no name of this program's: the worker world comment of
	// rig.sh, which holds that story once.
	Repository    string `json:"repository"`
	DefaultBranch string `json:"defaultBranch"`
	// GHPath is the real gh the rig names for the pane, as an operator names one with
	// LEGION_GH_PATH: its recording stand-in. "" is the daemon's own resolution, the first gh on
	// Path, refused when that is a script.
	GHPath string `json:"ghPath"`
	// SystemPrompt asks for the pane's system prompt, composed from the role prompts the daemon
	// module embeds.
	SystemPrompt bool `json:"systemPrompt"`
}

type paneResponse struct {
	// Env is every variable the daemon tells the pane, PATH included.
	Env map[string]string `json:"env"`
	// SystemPromptArgument is the pane's one --append-system-prompt argument, as shell text; ""
	// without SystemPrompt.
	SystemPromptArgument string `json:"systemPromptArgument,omitempty"`
}

func pane() error {
	var req paneRequest
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	repository, err := ghrepo.Parse("the rig's repository", req.Repository)
	if err != nil {
		return err
	}
	if req.DefaultBranch == "" {
		return errors.New("the rig's world names no default branch, which the pane's addressing names")
	}
	role := claim.Role(req.Role)
	token, err := claim.NewToken(req.Project, req.Issue, role)
	if err != nil {
		return err
	}
	owner := repository.Owner()
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
	spec := runtime.SpawnSpec{Claim: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1}
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
		if res.SystemPromptArgument, err = systemPromptArgument(req, token, repository); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

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

// systemPromptArgument composes the pane's role prompt as the daemon does (specs.SpawnSpec): the
// role prompts snapshotted under the state directory, the role's parts, then its addressing. The
// addressing names `repository`, the world's own, and the world's default branch, so the
// repository a worker is told to work in is the one its gh calls reach and the one the
// architect's note in its prompt names.
func systemPromptArgument(req paneRequest, token claim.Token, repository ghrepo.Repository) (string, error) {
	role := claim.Role(req.Role)
	if claim.IsTreeArchitect(role, req.Issue, req.Issue) {
		return "", errors.New("a tree architect is also told its design gate policy, which the rig does not stand in for")
	}
	composer, err := prompts.New(req.StateDir)
	if err != nil {
		return "", err
	}
	parts, err := composer.Compose(role, claim.IsTreeRoot(req.Issue, req.Issue))
	if err != nil {
		return "", err
	}
	addressing, err := daemon.AddressingFragment(req.Project, supervise.Claim{
		Token: token, Project: req.Project, Tree: req.Issue, Issue: req.Issue, Role: role, Generation: 1,
	}, repository, req.DefaultBranch, api.RepositoriesView{
		Mapped: []api.MappedRepositoryView{{Repo: repository.String(), Default: true}},
	})
	if err != nil {
		return "", err
	}
	return omplaunch.SystemPromptArgument(runtime.PromptParts{RolePromptPaths: parts.RolePromptPaths, Addressing: addressing}), nil
}
