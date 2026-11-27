package prompts

//ci:reads packages/daemon/docker/omp-plugins/package.json TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost reads the plugin version codeGraphActions copies
//ci:reads packages/daemon/docker/codegraph/package.json TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost reads the CLI version codeGraphCLIForms copies

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A broken role sequence drops the shared core, uses a root prompt for a child, or makes the Go
// daemon's instructions win before the shared role instructions. The prompt paths are what every
// launch concatenates into the one prompt file its agent reads, so their order is the contract. The
// Go daemon's text is the role's own part, then the part every architect or every phase worker
// shares.
func TestComposeOrdersSharedRolePartsBeforeTheGoDaemonParts(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name    string
		role    claim.Role
		isRoot  bool
		shared  []string
		goParts []string
	}{
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, []string{"architect-root.md", "architect-common.md"}},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, []string{"architect.md", "architect-common.md"}},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, []string{"planner.md", "worker-common.md"}},
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, []string{"implementer.md", "worker-common.md"}},
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, []string{"tester.md", "worker-common.md"}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, []string{"reviewer.md", "worker-common.md"}},
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := composer.Compose(tc.role, tc.isRoot)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			want := make([]string, 0, len(tc.shared)+len(tc.goParts))
			for _, part := range tc.shared {
				want = append(want, filepath.Join(stateDir, "prompts", "shared", part))
			}
			for _, part := range tc.goParts {
				want = append(want, filepath.Join(stateDir, "prompts", "go", part))
			}
			if !reflect.DeepEqual(parts.RolePromptPaths, want) {
				t.Fatalf("RolePromptPaths = %q, want %q", parts.RolePromptPaths, want)
			}
			// A pane reads each shared part from the snapshot by path, so the snapshot holds the
			// embedded part byte for byte.
			for _, part := range tc.shared {
				got, err := os.ReadFile(filepath.Join(stateDir, "prompts", "shared", part))
				if err != nil {
					t.Fatalf("read the snapshot's %s: %v", part, err)
				}
				if string(got) != rolePart(t, part) {
					t.Errorf("the snapshot's %s is not the embedded part", part)
				}
			}
			// Every launch joins the parts byte for byte (runtime.ComposePrompt), so the role's Go
			// daemon part and the shared one must meet at one blank line, as the paragraphs within a
			// part do.
			var text strings.Builder
			for i, path := range parts.RolePromptPaths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read part: %v", err)
				}
				if joined := text.String(); i > len(tc.shared) {
					gap := len(joined) - len(strings.TrimRight(joined, "\n")) + len(body) - len(strings.TrimLeft(string(body), "\n"))
					if gap != 2 {
						t.Errorf("Go daemon parts %q join %s with %d newlines, want one blank line", tc.goParts, path, gap)
					}
				}
				text.Write(body)
			}
		})
	}
}

// composedText is the whole text a role's composed prompt gives its pane: the parts Compose names,
// in the order the tmux runtime appends them (Parts.RolePromptPaths), read back from the snapshot
// and joined. A compose or a read that fails ends the test (t.Fatalf).
func composedText(t *testing.T, composer *Composer, role claim.Role, isRoot bool) string {
	t.Helper()
	parts, err := composer.Compose(role, isRoot)
	if err != nil {
		t.Fatalf("Compose %s (root %t): %v", role, isRoot, err)
	}
	return readJoined(t, parts.RolePromptPaths...)
}

// readJoined is the bytes of each of paths, read in order and joined; a read that fails ends the
// test (t.Fatalf), naming the path.
func readJoined(t *testing.T, paths ...string) string {
	t.Helper()
	var text strings.Builder
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
	}
	return text.String()
}

// A state directory outlives the daemon binary that wrote its prompt snapshot, so a boot of a newer
// daemon finds the older daemon's words there. A part whose content is not the running binary's is
// rewritten, shared or the daemon's own: a prompt that contradicts the daemon's own behaviour (who
// posts the merge) is the defect, and nothing treats these files as the operator's to edit (the
// operator's text is `instructions`). A part already holding the running binary's content is left
// as it is, so an ordinary restart writes nothing.
func TestNewRewritesAPartTheRunningDaemonDidNotWrite(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := New(stateDir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	snapshot := func(name string) string { return filepath.Join(stateDir, "prompts", name) }
	embedded := map[string]string{
		"go/merger.md":          "go/merger.md",
		"go/tester.md":          "go/tester.md",
		"shared/merger.md":      "roles/merger.md",
		"shared/core/tester.md": "roles/core/tester.md",
	}
	stale := []string{"go/merger.md", "shared/merger.md"}
	current := []string{"go/tester.md", "shared/core/tester.md"}
	for _, name := range stale {
		if err := os.WriteFile(snapshot(name), []byte("an older daemon's "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write the older part: %v", err)
		}
	}
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range current {
		if err := os.Chtimes(snapshot(name), written, written); err != nil {
			t.Fatalf("date the current part: %v", err)
		}
	}
	if _, err := New(stateDir); err != nil {
		t.Fatalf("second New: %v", err)
	}
	for name, source := range embedded {
		parts := goParts
		if strings.HasPrefix(source, "roles/") {
			parts = roleParts
		}
		want, err := parts.ReadFile(source)
		if err != nil {
			t.Fatalf("read embedded %s: %v", source, err)
		}
		got, err := os.ReadFile(snapshot(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s after the second New = %q, want the running daemon's part", name, got)
		}
	}
	for _, name := range current {
		if info, err := os.Stat(snapshot(name)); err != nil || !info.ModTime().Equal(written) {
			t.Errorf("%s, already holding the running daemon's content, was written again: %v", name, err)
		}
	}
}

// foldWhitespace is one line of text, so a phrase a hard-wrapped prompt breaks over two lines is
// still one phrase to match.
func foldWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// codeGraphActions are the actions the `codegraph` tool takes: the action table of
// @bopstack/pi-codegraph (its src/descriptors.ts), at the version packages/daemon/docker/omp-plugins
// pins for the worker image. A prompt that names another action sends its role to a call the tool
// refuses.
var codeGraphActions = []string{
	"help", "search", "explore", "node", "callers", "callees", "files", "status",
	"impact", "affected", "init", "index", "sync", "unlock",
}

// codeGraphPluginVersion is the version of @bopstack/pi-codegraph that codeGraphActions copies, and
// codeGraphCLIVersion the version of the CodeGraph CLI (@colbymchenry/codegraph) whose commands
// codeGraphCLIForms and go/claude-mechanics.md copy and whose answers the prompts' CodeGraph rule
// describes. Each is the pin of the worker image.
const (
	codeGraphPluginVersion = "0.1.1"
	codeGraphCLIVersion    = "1.6.4"
)

// codeGraphRoles classifies every role a claim can be on by what its prompt must say about the
// `codegraph` tool: the actions of the step it carries at the point where it decides, besides
// `status`, which the rule for an empty or a failed answer names for every role that reads code. A
// role that is not here fails TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost until its
// actions are listed. Every claim role reads code today. A claim role added later that reads no
// code needs this test to hold its prompt to no `xd://codegraph` instead. The controller has no
// claim role: its spawn spec carries no repository or workspace, so its prompt files are checked
// apart.
var codeGraphRoles = map[claim.Role][]string{
	claim.RoleArchitect:   {"impact", "callers", "affected", "explore"},
	claim.RolePlanner:     {"impact", "callers", "affected", "explore"},
	claim.RoleImplementer: {"impact", "callers", "affected"},
	claim.RoleTester:      {"impact", "callers", "affected"},
	claim.RoleReviewer:    {"impact", "callers"},
	claim.RoleMerger:      {"callers", "impact"},
}

// codeGraphByCommand is whether each backend's roles reach CodeGraph through the `codegraph`
// command, which the backend's prompt part maps action by action, rather than through the tool that
// every role's own text names. Claude Code has no such tool. A backend that is not here fails the
// test until someone says how its roles reach CodeGraph.
var codeGraphByCommand = map[runtime.Backend]bool{
	runtime.BackendOmp:    false,
	runtime.BackendClaude: true,
}

// codeGraphCLIForms is the command a role that reaches CodeGraph by its command runs for each
// action a prompt names.
var codeGraphCLIForms = map[string]string{
	"impact":   "codegraph impact <symbol>",
	"callers":  "codegraph callers <symbol>",
	"affected": "codegraph affected <path> --depth 1",
	"explore":  "codegraph explore <words>",
	"status":   "codegraph status",
}

// dockerPin is the version the worker image pins for package, read from the package.json under
// packages/daemon/docker/dir.
func dockerPin(t *testing.T, dir, pkg string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docker", dir, "package.json"))
	if err != nil {
		t.Fatalf("read the pin of %s: %v", pkg, err)
	}
	var pinned struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(body, &pinned); err != nil {
		t.Fatalf("decode the pin of %s: %v", pkg, err)
	}
	return pinned.Dependencies[pkg]
}

// Every role that reads or changes code is told when to ask the `codegraph` tool, for the host it
// runs on: the architects before the spec names what a change touches, the planner before a task
// names its files and tests, the implementer before it changes a symbol, the tester before it
// judges the implementer's tests, the reviewer for each changed exported symbol and the merger
// where the base changed code the change uses. The inventory is every claim role on every backend
// that runs it, each row the prompt that backend launches the role with, so a role or a backend
// added later fails here until it is classified, and the controller, which works no checkout, is
// told nothing.
//
// Each prompt gives the call's shape once and the rule for a missing tool and for an empty, a stale
// or a partial answer once. A role that took `status` or the tool's own advice would start a second
// build beside the warm-up's, which corrupts the index. Each prompt has the role check the
// warm-up's lease file with no CodeGraph call before its first query and before each `status`,
// because a `status` during the warm-up's build made that build fail. No prompt lets a role run
// `init`, since its build takes no lease and other roles would call `status` during it; only the
// warm-up builds the index. Every action a prompt names is one the tool takes and one the role's
// row lists, `affected` always with `"depth":1`, since its default depth fans out across unrelated
// tests (3,101 test files for one Go file at depth 5 and 17 at depth 1, measured on CodeGraph
// 1.6.4). This shows the text is loaded, never that a role follows it.
func TestEveryCodeReadingRoleIsToldWhenToAskCodeGraphOnEachHost(t *testing.T) {
	if got := dockerPin(t, "omp-plugins", "@bopstack/pi-codegraph"); got != codeGraphPluginVersion {
		t.Fatalf("@bopstack/pi-codegraph is pinned at %q and codeGraphActions copies %s: re-read src/descriptors.ts of the pinned version and update codeGraphActions", got, codeGraphPluginVersion)
	}
	if got := dockerPin(t, "codegraph", "@colbymchenry/codegraph"); got != codeGraphCLIVersion {
		t.Fatalf("@colbymchenry/codegraph is pinned at %q and codeGraphCLIForms copies %s: run `codegraph <subcommand> --help` of each form in the pinned version and update codeGraphCLIForms and go/claude-mechanics.md", got, codeGraphCLIVersion)
	}

	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const (
		callShape          = "`write` to `xd://codegraph` the content `{\"action\":"
		staleAnswer        = "It does not see an edit made since the build, by you, by another role or by the base"
		pendingChange      = "`status` counts each changed file as a pending change"
		buildRunning       = "While a build runs, `status` says the index is truncated or that an index rebuild is running in another process"
		leaseCheck         = "run `find <the workspace's absolute path>/.codegraph/legion-warm.lock -mmin -1 2>/dev/null`"
		leaseFresh         = "When it prints the path, read the source, make no `codegraph` call"
		statusOnFailure    = "Ask `{\"action\":\"status\"}` when an answer is empty or failed"
		readsSource        = "Read the source with grep and the read tool in three cases"
		missingTool        = "The tool is missing: it says it is unavailable on `PATH`, or the workspace is not initialized"
		emptyAnswer        = "An empty answer never means \"none\""
		partialAnswer      = "The answer may be stale or partial"
		anyEditor          = "whoever changed it"
		nameMatching       = "`impact` and `callers` match by name"
		blockPerDefinition = "give one block for each definition of a name"
		noIndexAction      = "Run no index action, whatever `status` or the tool's own guidance advises (no `init`, `index`, `sync` or `unlock`)"
		warmUpOnly         = "Only the warm-up builds the index, under its lease, when a role starts"
		// Held only by a prompt of a role that reaches CodeGraph by its command, and the retired
		// call, the retired sentence and the retired command, which no prompt holds.
		claudeMapping      = "is the `codegraph` command"
		retiredCall        = "codegraph({"
		retiredStatusFirst = "Ask `{\"action\":\"status\"}` before your first query"
		retiredInitForm    = "codegraph init"
	)
	onceEach := []string{callShape, staleAnswer, pendingChange, leaseCheck, leaseFresh, buildRunning, statusOnFailure, readsSource, missingTool, emptyAnswer, partialAnswer, anyEditor, nameMatching, blockPerDefinition, noIndexAction, warmUpOnly}
	action := regexp.MustCompile(`"action":"([^"]*)"`)
	affectedCall := regexp.MustCompile(`\{"action":"affected"[^}]*\}`)

	for role := range codeGraphRoles {
		if !claim.IsRole(role) {
			t.Errorf("codeGraphRoles classifies %q, which is not a claim role", role)
		}
	}
	backends := slices.Sorted(maps.Keys(runtime.Backends))
	for _, backend := range backends {
		byCommand, ok := codeGraphByCommand[backend]
		if !ok {
			t.Errorf("the backend %q has no row in codeGraphByCommand: say how its roles reach the `codegraph` tool", backend)
		} else if byCommand && runtime.Backends[backend].PromptPart == "" {
			t.Errorf("the backend %q reaches CodeGraph by its command, and names no prompt part that maps the actions to it", backend)
		}
	}

	for _, role := range claim.Roles {
		listed, ok := codeGraphRoles[role]
		if !ok {
			t.Errorf("the role %q has no row in codeGraphRoles: say whether its prompt must tell it when to ask the `codegraph` tool", role)
			continue
		}
		named := append([]string{"status"}, listed...)
		for _, isRoot := range []bool{true, false} {
			if isRoot && role != claim.RoleArchitect {
				continue
			}
			for _, backend := range backends {
				info := runtime.Backends[backend]
				if !info.Runs(role) {
					continue
				}
				row := fmt.Sprintf("%s (root %t) on %s", role, isRoot, backend)
				t.Run(row, func(t *testing.T) {
					launched := composedText(t, composer, role, isRoot)
					if info.PromptPart != "" {
						launched += readJoined(t, composer.Part(info.PromptPart))
					}
					text := foldWhitespace(launched)
					for _, needle := range onceEach {
						if n := strings.Count(text, needle); n != 1 {
							t.Errorf("the prompt of %s holds %q %d times, want once", row, needle, n)
						}
					}
					if strings.Contains(text, retiredCall) {
						t.Errorf("the prompt of %s holds %q, a call no host can make", row, retiredCall)
					}
					if n := strings.Count(text, retiredStatusFirst); n != 0 {
						t.Errorf("the prompt of %s holds %q %d times, want 0: a `status` while the warm-up builds the index makes that build fail", row, retiredStatusFirst, n)
					}
					if n := strings.Count(text, retiredInitForm); n != 0 {
						t.Errorf("the prompt of %s holds %q %d times, want 0: a role's own `init` takes no lease, so other roles call `status` during that build", row, retiredInitForm, n)
					}
					found := map[string]bool{}
					for _, match := range action.FindAllStringSubmatch(text, -1) {
						found[match[1]] = true
						switch {
						case !slices.Contains(codeGraphActions, match[1]):
							t.Errorf("the prompt of %s names the action %q, which the `codegraph` tool does not take (%q)", row, match[1], codeGraphActions)
						case !slices.Contains(named, match[1]):
							t.Errorf("the prompt of %s names the action %q, which is not one of the actions codeGraphRoles lists for it (%q, and status)", row, match[1], listed)
						}
					}
					for _, want := range named {
						if !found[want] {
							t.Errorf("the prompt of %s never names the action %q", row, want)
						}
					}
					for _, call := range affectedCall.FindAllString(text, -1) {
						if !strings.Contains(call, `"depth":1`) {
							t.Errorf("the prompt of %s names the call %s without \"depth\":1: the default depth fans out across unrelated tests", row, call)
						}
					}
					wantMapping := 0
					if codeGraphByCommand[backend] {
						wantMapping = 1
						forms := map[string]string{}
						for _, want := range named {
							form, ok := codeGraphCLIForms[want]
							if !ok {
								t.Errorf("codeGraphCLIForms has no command for the action %q, which the prompt of %s names", want, row)
								continue
							}
							forms[want] = form
						}
						for what, form := range forms {
							if !strings.Contains(text, form) {
								t.Errorf("the prompt of %s does not say that %s is %q", row, what, form)
							}
						}
					}
					if n := strings.Count(text, claudeMapping); n != wantMapping {
						t.Errorf("the prompt of %s holds %q %d times, want %d: only a backend that reaches CodeGraph by its command is told the command for each action", row, claudeMapping, n, wantMapping)
					}
				})
			}
		}
	}

	for _, headless := range []bool{true, false} {
		paths, err := composer.ControllerPromptPaths(headless)
		if err != nil {
			t.Fatalf("ControllerPromptPaths(%t): %v", headless, err)
		}
		for _, path := range paths {
			body := readJoined(t, path)
			if strings.Contains(body, "xd://codegraph") {
				t.Errorf("the controller's prompt part %s names `xd://codegraph`: the controller works no checkout", path)
			}
			if strings.Contains(body, retiredCall) {
				t.Errorf("the controller's prompt part %s holds %q, a call no host can make", path, retiredCall)
			}
		}
	}
}
