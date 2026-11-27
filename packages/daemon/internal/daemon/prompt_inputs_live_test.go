//go:build e2e

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	promptInputsScript          = "scripts/e2e/prompt-inputs.sh"
	promptInputsFixtureRev      = "ecd967b17a4e"
	promptInputsTask            = "Prompt inputs proof: gather the public context required before work. Read the role's entry skill, any other mandatory file reads, and README.md. Do not change files or send messages. Then report the repository's name in one line."
	promptInputsReadsTask       = promptInputsTask + " After the reads, run `pwd` with the shell tool before you answer."
	promptInputsControlRead     = "CONTRIBUTING.md"
	promptInputsStandinKeyName  = "ANTHROPIC_API_KEY"
	promptInputsStandinKeyValue = "prompt-inputs-local-provider"
)

type promptInputsRefs struct {
	Retained string `json:"retained"`
	Removed  string `json:"removed"`
}

type promptInputsRefTable struct {
	Description string                      `json:"description"`
	Withheld    string                      `json:"withheld_control_launch"`
	Launches    map[string]promptInputsRefs `json:"launches"`
}

type promptInputsRun struct {
	t                                            *testing.T
	root, subject, base, evidence, work, fixture string
	omp, legion, claude, claudePlugin, bun       string
	home, withheldHome, profile                  string
	dispatch                                     *handoffDispatch
	dispatchFile                                 string
	refs                                         promptInputsRefTable
	skills                                       []string
	rows                                         map[string]promptInputsRow
	isBase                                       bool
	spend                                        *promptInputsSpend
	gatewayAuth                                  string
	socketDir                                    string
}

type promptInputsPane struct {
	launch                  promptInputsLaunch
	name, issue, tree, task string
	token                   claim.Token
	at                      phase.Phase
	model                   *promptInputsModel
	reads, withheld, extra  bool
}

func TestPromptInputsLive(t *testing.T) {
	fixtureenv.Live(t)
	subject := os.Getenv("LEGION_PROMPT_INPUTS_SUBJECT")
	if subject == "" {
		t.Skipf("run %s with --subject", promptInputsScript)
	}
	r := &promptInputsRun{t: t, root: proofCheckoutRoot(t), subject: subject, base: os.Getenv("LEGION_PROMPT_INPUTS_BASE"), work: t.TempDir(), evidence: filepath.Dir(os.Getenv(liveDaemonLogEnv)), omp: os.Getenv(liveOmpPathEnv), profile: "prompt-inputs", rows: map[string]promptInputsRow{}}
	if os.Getenv(liveDaemonLogEnv) == "" {
		t.Fatal("the proof needs an evidence directory")
	}
	if os.Getenv("LEGION_PROMPT_INPUTS_READS") == "1" {
		auth := os.Getenv("ANTHROPIC_AUTH_TOKEN")
		if auth == "" {
			t.Fatal("--reads requires a gateway identity in ANTHROPIC_AUTH_TOKEN")
		}
		cap := promptInputsDefaultTokenCap
		if value := os.Getenv("LEGION_PROMPT_INPUTS_TOKEN_CAP"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 || n > promptInputsMaxTokenCap {
				t.Fatalf("LEGION_PROMPT_INPUTS_TOKEN_CAP must be in 1..%d", promptInputsMaxTokenCap)
			}
			cap = n
		}
		r.gatewayAuth = auth
		r.spend = &promptInputsSpend{cap: cap}
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	}
	r.checkInventory()
	r.setup()
	r.runWave(t, "omp", runtime.BackendOmp, false, false, false)
	r.runWave(t, "interactive", runtime.BackendOmp, true, false, false)
	r.runWave(t, "claude", runtime.BackendClaude, false, false, false)
	if os.Getenv("LEGION_PROMPT_INPUTS_READS") == "1" {
		if t.Failed() {
			t.Fatal("the stand-in proof failed; no real-model sessions started")
		}
		defer r.writeSpend()
		r.runWave(t, "reads-omp", runtime.BackendOmp, false, true, false)
		if os.Getenv("LEGION_PROMPT_INPUTS_READS_SUPERVISED_ONLY") != "1" {
			r.runWave(t, "reads-interactive", runtime.BackendOmp, true, true, false)
		}
		r.runWave(t, "reads-extra", runtime.BackendOmp, false, true, true)
		// The supervised-only continuation excludes already-measured interactive OMP
		// and the optional Claude probe. Their stand-in measurements still run.
		if os.Getenv("LEGION_PROMPT_INPUTS_READS_SUPERVISED_ONLY") != "1" {
			r.runWave(t, "reads-claude", runtime.BackendClaude, false, true, false)
		}
		r.finishReads()
	} else {
		r.writeJSON("control-extra-read.json", map[string]string{"status": "not observed", "reason": "requires --reads"})
	}
	var rows bytes.Buffer
	enc := json.NewEncoder(&rows)
	for _, launch := range promptInputsLaunches {
		row, ok := r.rows[launch.Name]
		if !ok {
			t.Errorf("no evidence row for %s", launch.Name)
			continue
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("total %s %d bytes (%s required reads)\n", row.Launch, row.TotalBytes, row.ReadsObserved)
	}
	r.writeEvidence("rows.jsonl", rows.Bytes())
}

func promptInputsCommand(t *testing.T, dir string, argv ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s\n%s", strings.Join(argv, " "), err, out, stderr.Bytes())
	}
	return out
}

func promptInputsReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *promptInputsRun) writeEvidence(name string, data []byte) {
	r.t.Helper()
	path := filepath.Join(r.evidence, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		r.t.Fatal(err)
	}
	if err := runtime.ReplaceFile(path, string(data)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *promptInputsRun) writeJSON(name string, v any) {
	r.t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		r.t.Fatal(err)
	}
	r.writeEvidence(name, append(data, '\n'))
}

func (r *promptInputsRun) checkInventory() {
	t := r.t
	fmt.Println("== composition-inventory")
	base := strings.TrimSpace(string(promptInputsCommand(t, r.subject, "git", "rev-parse", "--verify", r.base+"^{commit}")))
	head := strings.TrimSpace(string(promptInputsCommand(t, r.subject, "git", "rev-parse", "HEAD")))
	if diff := promptInputsCommand(t, r.subject, "git", "diff", base, "--", ".github/CODEOWNERS"); len(diff) != 0 {
		t.Fatalf("CODEOWNERS differs from %s:\n%s", base, diff)
	}
	r.isBase = head == base
	baseDir := filepath.Join(r.work, "base")
	promptInputsCommand(t, r.subject, "git", "worktree", "add", "--detach", baseDir, base)
	t.Cleanup(func() {
		if out, err := exec.Command("git", "-C", r.subject, "worktree", "remove", baseDir).CombinedOutput(); err != nil {
			t.Errorf("remove base worktree: %v: %s", err, out)
		}
	})
	baseLaunches := r.composerInventory(baseDir)
	subjectLaunches := r.composerInventory(r.subject)
	if len(baseLaunches) != len(promptInputsLaunches) || len(subjectLaunches) != len(promptInputsLaunches) {
		t.Fatalf("launch inventories: base %d, subject %d, table %d", len(baseLaunches), len(subjectLaunches), len(promptInputsLaunches))
	}
	for _, want := range promptInputsLaunches {
		for _, inventory := range []map[string]promptInputsLaunch{baseLaunches, subjectLaunches} {
			got := inventory[want.Name]
			if want.Role != got.Role || want.Backend != got.Backend || !slices.Equal(want.Parts, got.Parts) {
				t.Fatalf("%s: table %+v, composed %+v", want.Name, want, got)
			}
		}
	}
	r.writeJSON("composition.json", map[string]any{"base": base, "subject_head": head, "base_launches": baseLaunches, "subject_launches": subjectLaunches, "codeowners_diff": ""})
	if err := json.Unmarshal(promptInputsReadFile(t, os.Getenv("LEGION_PROMPT_INPUTS_REFS")), &r.refs); err != nil {
		t.Fatal(err)
	}
	if len(r.refs.Launches) != len(promptInputsLaunches) || r.refs.Withheld != "planner/omp" {
		t.Fatal("reference table must name every launch and planner/omp for the withheld-file control")
	}
	for _, launch := range promptInputsLaunches {
		refs := r.refs.Launches[launch.Name]
		if refs.Retained == refs.Removed {
			t.Fatalf("%s: retained and removed references must differ", launch.Name)
		}
		for _, ref := range []string{refs.Retained, refs.Removed} {
			if _, _, err := promptInputsSkillTarget(ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	fmt.Printf("ok composition-inventory (%d launches; no CODEOWNERS diff)\n", len(subjectLaunches))
}

func (r *promptInputsRun) composerInventory(checkout string) map[string]promptInputsLaunch {
	t := r.t
	dir := filepath.Join(checkout, "packages", "daemon")
	work := t.TempDir()
	probe := filepath.Join(work, "probe.go")
	writeProofFile(t, probe, string(promptInputsReadFile(t, filepath.Join(r.root, "packages/daemon/internal/daemon/testdata/prompt-inputs/dump_launches.go.tmpl"))))
	overlay := filepath.Join(work, "overlay.json")
	data, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(dir, "internal/prompts/prompt_inputs_dump_test.go"): probe}})
	if err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, overlay, string(data))
	output := filepath.Join(work, "launches.json")
	cmd := exec.CommandContext(t.Context(), "go", "test", "-overlay", overlay, "-run", "^TestPromptInputsDumpComposedLaunches$", "-count=1", "./internal/prompts")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEGION_PROMPT_INPUTS_DUMP_OUT="+output)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compose %s: %v\n%s", checkout, err, out)
	}
	var launches []promptInputsLaunch
	if err := json.Unmarshal(promptInputsReadFile(t, output), &launches); err != nil {
		t.Fatal(err)
	}
	byName := map[string]promptInputsLaunch{}
	for _, l := range launches {
		if _, ok := byName[l.Name]; ok {
			t.Fatalf("duplicate launch %s in %s", l.Name, checkout)
		}
		byName[l.Name] = l
	}
	return byName
}

func (r *promptInputsRun) setup() {
	t := r.t
	fmt.Println("== pack-subject-and-prepare-fixture")
	r.legion = buildProofLegion(t, r.root, r.work)
	r.fixture = filepath.Join(r.work, "fixture")
	promptInputsCommand(t, r.subject, "git", "worktree", "add", "--quiet", "--detach", r.fixture, promptInputsFixtureRev)
	t.Cleanup(func() {
		if out, err := exec.Command("git", "-C", r.subject, "worktree", "remove", r.fixture).CombinedOutput(); err != nil {
			status, _ := exec.Command("git", "-C", r.fixture, "status", "--porcelain=v1", "--untracked-files=all").Output()
			r.writeEvidence("fixture-status.txt", status)
			t.Errorf("remove fixture worktree: %v: %s", err, out)
		}
	})
	r.home = filepath.Join(r.work, "omp-home")
	r.withheldHome = filepath.Join(r.work, "withheld-home")
	installProofPlugins(t, r.subject, r.work, r.home, r.profile, r.omp)
	controlWork := filepath.Join(r.work, "withheld")
	if err := os.Mkdir(controlWork, 0700); err != nil {
		t.Fatal(err)
	}
	installProofPlugins(t, r.subject, controlWork, r.withheldHome, r.profile, r.omp)
	r.withhold(controlWork)
	for _, plugin := range []string{"pi-envoy", "pi-legion"} {
		staged := filepath.Join(t.TempDir(), "skills")
		promptInputsCommand(t, r.subject, "bash", "scripts/pi-plugin-prepack.sh", "--stage-skills", "@sjawhar/"+plugin, staged)
		want := promptInputsSkillDirs(t, staged)
		got := promptInputsSkillDirs(t, filepath.Join(r.work, plugin, "dist", "skills"))
		if !slices.Equal(want, got) {
			t.Fatalf("%s packed skills %v; prepack lists %v", plugin, got, want)
		}
		for _, skill := range want {
			body := string(promptInputsReadFile(t, filepath.Join(staged, skill, "SKILL.md")))
			header, _, _ := strings.Cut(strings.TrimPrefix(body, "---\n"), "\n---\n")
			if !slices.Contains(strings.Split(header, "\n"), "disable-model-invocation: true") {
				r.skills = append(r.skills, skill)
			}
		}
	}
	slices.Sort(r.skills)
	r.claudePlugin = filepath.Join(r.work, "claude-envoy")
	var err error
	r.socketDir, err = os.MkdirTemp(os.Getenv("LEGION_PROMPT_INPUTS_SOCKET_DIR"), "pi-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(r.socketDir); err != nil {
			t.Error(err)
		}
	})
	r.bun, err = exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	promptInputsCommand(t, filepath.Join(r.subject, "packages/claude-envoy"), r.bun, "scripts/build.ts", "--out", r.claudePlugin)
	r.installClaude()
	r.dispatch = startHandoffDispatch(t, r.root)
	r.dispatchFile = filepath.Join(r.work, "dispatch-token")
	writeProofFile(t, r.dispatchFile, r.dispatch.token+"\n")
	fmt.Printf("ok pack-subject-and-prepare-fixture (%s, %d model-visible skills; %s)\n", strings.TrimSpace(string(promptInputsCommand(t, r.work, r.omp, "--version"))), len(r.skills), promptInputsFixtureRev)
}

func promptInputsSkillDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "SKILL.md")); err != nil {
				t.Fatal(err)
			}
			names = append(names, e.Name())
		}
	}
	return names
}

func (r *promptInputsRun) installClaude() {
	t := r.t
	dir := t.TempDir()
	pinDir := filepath.Join(r.root, "packages/daemon/docker/claude-code")
	for _, name := range []string{"package.json", "package-lock.json"} {
		writeProofFile(t, filepath.Join(dir, name), string(promptInputsReadFile(t, filepath.Join(pinDir, name))))
	}
	var pin struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(promptInputsReadFile(t, filepath.Join(dir, "package.json")), &pin); err != nil {
		t.Fatal(err)
	}
	want := pin.Dependencies["@anthropic-ai/claude-code"]
	if want == "" {
		t.Fatal("no Claude Code release pin")
	}
	promptInputsCommand(t, dir, "npm", "ci", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", "--cache", filepath.Join(dir, "cache"))
	r.claude = filepath.Join(dir, "node_modules/@anthropic-ai/claude-code-linux-x64/claude")
	version := strings.Fields(string(promptInputsCommand(t, dir, r.claude, "--version")))
	if len(version) == 0 || version[0] != want {
		t.Fatalf("Claude Code version %q, want %s", version, want)
	}
}

func promptInputsSkillTarget(ref string) (string, string, error) {
	uri, ok := strings.CutPrefix(ref, "skill://")
	if !ok || strings.ContainsAny(uri, "?#\\\x00") {
		return "", "", fmt.Errorf("invalid skill reference %q", ref)
	}
	name, rest, hasPath := strings.Cut(uri, "/")
	if !hasPath {
		rest = "SKILL.md"
	}
	if !filepath.IsLocal(name) || !filepath.IsLocal(rest) || filepath.Clean(rest) != rest {
		return "", "", fmt.Errorf("invalid skill reference %q", ref)
	}
	return name, rest, nil
}

func (r *promptInputsRun) withhold(work string) {
	t := r.t
	ref := r.refs.Launches[r.refs.Withheld].Retained
	name, part, err := promptInputsSkillTarget(ref)
	if err != nil {
		t.Fatal(err)
	}
	if part == "SKILL.md" {
		t.Fatal("withhold a reference file, not the skill entry point")
	}
	var removed int
	for _, root := range []string{filepath.Join(work, "pi-envoy"), filepath.Join(work, "pi-legion"), r.withheldHome} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, filepath.Join("skills", name, part)) {
				removed++
				return os.Remove(path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if removed == 0 {
		t.Fatalf("no packaged file to withhold for %s", ref)
	}
}

func (r *promptInputsRun) runWave(t *testing.T, name string, backend runtime.Backend, interactive, reads, extra bool) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		wave := *r
		wave.t = t
		r := &wave
		p := newLiveProof(t, promptInputsScript)
		// Each wave boots a different project on the run's broker. Once the daemon and
		// panes stop, remove their consumers and messages before creating the next wave.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := p.js.Stream(ctx, "ENVOY_NOTIFICATIONS")
			if err != nil {
				t.Error(err)
				return
			}
			consumers := stream.ConsumerNames(ctx)
			for name := range consumers.Name() {
				if err := stream.DeleteConsumer(ctx, name); err != nil {
					t.Error(err)
				}
			}
			if err := consumers.Err(); err != nil {
				t.Error(err)
			}
			if err := stream.Purge(ctx); err != nil {
				t.Error(err)
			}
		})
		a := liveAgentProof{liveProof: p, omp: r.omp, legion: r.legion}
		if backend == runtime.BackendClaude {
			t.Setenv("LEGION_CLAUDE_PATH", r.claude)
			t.Setenv("LEGION_BUN_PATH", r.bun)
			t.Setenv("LEGION_CLAUDE_PLUGIN_DIR", r.claudePlugin)
			a.cfg.WorkerBackends = map[claim.Role]runtime.Backend{}
		}
		panes := map[claim.Token]*promptInputsPane{}
		var ordered []*promptInputsPane
		overlays := map[claim.Token][]string{}
		work := t.TempDir()
		op := filepath.Join(work, "operator.yml")
		writeProofFile(t, op, "retry:\n  enabled: false\n")
		var models, enabled, withheldModels, withheldEnabled strings.Builder
		models.WriteString("providers:\n")
		withheldModels.WriteString("providers:\n")
		add := func(l promptInputsLaunch, withheld bool) {
			pane := &promptInputsPane{launch: l, name: l.Name, task: promptInputsTask, reads: reads, extra: extra, withheld: withheld}
			if reads {
				pane.task = promptInputsReadsTask
			}
			if withheld {
				pane.name = "control-withheld/omp"
			}
			if extra {
				pane.name = "control-extra-read/omp"
			}
			if reads && !extra {
				pane.name = "reads-" + l.Name
			}
			if l.Role == claim.RoleController {
				pane.token = claim.ControllerToken(a.project)
				pane.task = ControllerStartMessage
				if reads {
					pane.task = promptInputsReadsTask
				}
			} else {
				n := len(ordered) + 1
				pane.issue = fmt.Sprintf("%s-%02d", a.cfg.Project, n)
				pane.tree = pane.issue
				pane.at = promptInputsPhase(l.Role)
				if l.Name == "architect-child/omp" {
					pane.tree = ordered[0].issue
				}
				var err error
				pane.token, err = claim.NewToken(a.project, pane.issue, l.Role)
				if err != nil {
					t.Fatal(err)
				}
			}
			pane.model = r.startModel(t, pane, namedPromptInputsProvider(len(ordered)+1))
			if backend == runtime.BackendOmp {
				m, e := &models, &enabled
				if withheld {
					m, e = &withheldModels, &withheldEnabled
				}
				selector := pane.model.writeProvider(m, e)
				overlay := filepath.Join(work, fmt.Sprintf("model-%d.yml", len(ordered)))
				writeRoleOverlay(t, overlay, selector)
				if reads {
					writeProofFile(t, overlay, string(promptInputsReadFile(t, overlay))+"defaultThinkingLevel: xhigh\n")
				}
				overlays[pane.token] = []string{op, overlay}
			} else {
				a.cfg.WorkerBackends[l.Role] = backend
			}
			panes[pane.token] = pane
			ordered = append(ordered, pane)
		}
		for _, l := range promptInputsLaunches {
			if l.Backend != backend || (l.Name == "controller-interactive/omp") != interactive || (extra && l.Name != r.refs.Withheld) {
				continue
			}
			if reads && backend == runtime.BackendClaude && l.Name != "planner/claude" {
				continue
			}
			add(l, false)
		}
		if !reads && !interactive && backend == runtime.BackendOmp {
			for _, l := range promptInputsLaunches {
				if l.Name == r.refs.Withheld {
					add(l, true)
				}
			}
		}
		if backend == runtime.BackendOmp {
			r.writeProfile(t, r.home, models.String(), enabled.String())
			if withheldEnabled.Len() > 0 {
				r.writeProfile(t, r.withheldHome, withheldModels.String(), withheldEnabled.String())
			}
		}
		a.rt = newLocalRuntime(t, a.cfg.StateDir, r.legion, r.omp, r.home, r.profile)
		a.rt.workDirOf = func(runtime.SpawnSpec) string { return r.fixture }
		a.rt.daemonURL, a.rt.envoyURL, a.rt.natsURLs = a.cfg.DaemonURL, newPaneListener(t), a.cfg.NatsURLs
		a.rt.dispatchURL, a.rt.dispatchTokenFile = r.dispatch.base, r.dispatchFile
		a.rt.overlays = func(token claim.Token) []string { return overlays[token] }
		// Oh My Pi writes shell snapshots under TMPDIR. Keep them in the run's scratch
		// directory, not in the pinned worktree the agents read.
		a.rt.environment = []string{"BUN_RUNTIME_TRANSPILER_CACHE_PATH=0", "OMP_SESSION_STORAGE=file", "TMPDIR=" + r.socketDir}
		a.rt.agent = func(spec runtime.SpawnSpec, promptFile string) ([]string, []string, error) {
			pane := panes[spec.Claim]
			if pane == nil {
				return nil, nil, fmt.Errorf("unexpected claim %s", spec.Claim)
			}
			if backend == runtime.BackendClaude {
				return r.claudeAgent(a.cfg.StateDir, spec, promptFile, pane.model)
			}
			if pane.withheld {
				return nil, []string{"HOME=" + r.withheldHome}, nil
			}
			return nil, nil, nil
		}
		if !interactive {
			for _, pane := range ordered {
				if pane.launch.Role == claim.RoleController {
					a.cfg.ControllerLaunch = config.ControllerLaunchDaemon
				}
			}
			r.seed(&a, ordered)
		}
		rt := &promptInputsRuntime{localRuntime: a.rt, run: r, panes: panes}
		o := a.rt.daemonOverrides(rt)
		o.workflowTokens = &workflowTokenRecorder{}
		a.d = startDaemonLogging(t, a.cfg, o, a.log())
		if a.cfg.ControllerLaunch == config.ControllerLaunchDaemon {
			// The keeper creates the controller independently of HTTP startup.
			a.await("the daemon controller launch", paneReady, func() bool { return len(a.rt.LaunchesOf(claim.ControllerToken(a.project))) != 0 })
		}
		for _, pane := range ordered {
			t.Run(promptInputsSlug(pane.name), func(t *testing.T) {
				a := a
				a.t = t
				a.step("capture-"+promptInputsSlug(pane.name), func() {
					if interactive {
						r.captureInteractive(&a, rt, pane, overlays[pane.token])
						return
					}
					a.awaitReady(pane.token)
					held := a.claim(pane.token)
					if held.Locator == nil {
						t.Fatalf("%s has no locator", pane.name)
					}
					pid, _, err := parseLocalIncarnation(held.Locator.Incarnation)
					if err != nil {
						t.Fatal(err)
					}
					binary, flag := r.omp, promptWord
					if backend == runtime.BackendClaude {
						binary, flag = r.claude, "--append-system-prompt-file"
					}
					specs := a.rt.LaunchesOf(pane.token)
					if len(specs) != 1 {
						t.Fatalf("%s launched %d times", pane.name, len(specs))
					}
					r.capture(&a, pane, pid, binary, flag, specs[0].Prompt, held.SessionFile)
				})
			})
		}
	})
}

func namedPromptInputsProvider(index int) string { return fmt.Sprintf("promptinputs%d", index) }
func promptInputsSlug(name string) string        { return strings.ReplaceAll(name, "/", "-") }

func promptInputsPhase(role claim.Role) phase.Phase {
	switch role {
	case claim.RolePlanner:
		return phase.Planning
	case claim.RoleImplementer:
		return phase.Implementing
	case claim.RoleTester:
		return phase.Testing
	case claim.RoleReviewer:
		return phase.Reviewing
	case claim.RoleMerger:
		return phase.Merging
	default:
		return phase.Planning
	}
}

func (r *promptInputsRun) seed(a *liveAgentProof, panes []*promptInputsPane) {
	t := a.t
	ctx := t.Context()
	a.cfg.AdmissionCap = len(panes)
	var slots []string
	for _, p := range panes {
		if p.issue == "" {
			continue
		}
		if p.tree != p.issue {
			a.board.place(p.issue, "in_progress", p.tree, false)
		} else {
			a.board.show(p.issue, "in_progress")
		}
		if p.at != phase.Merging {
			slots = append(slots, p.issue)
		}
	}
	if err := store.Transact(ctx, a.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range panes {
			if p.issue == "" {
				continue
			}
			issue := record.Issue{Key: p.issue, Tree: p.tree, Repo: liveRepo, GitHubRepo: liveRepo, DefaultBranch: "main", Project: a.cfg.Project, Title: "Measure the prompt inputs of a public fixture", Phase: p.at, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: p.tree == p.issue}
			if p.tree != p.issue {
				issue.Parent = &p.tree
			}
			if err := a.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		return a.admit(ctx, tx, slots...)
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range panes {
		// The keeper normally delivers its start message. For the reads fixture, it
		// resumes the fixed public task as the pending delivery instead.
		if p.issue == "" && !p.reads {
			continue
		}
		if p.issue != "" && p.tree == p.issue {
			if _, err := a.st.OpenTreeLifecycle(ctx, a.project, p.tree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.st.AdmitClaim(ctx, supervise.Claim{Token: p.token, Project: a.project, Tree: p.tree, Issue: p.issue, Role: p.launch.Role, Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("seeded-" + randomSuffix(t)), ServingGeneration: 1}); err != nil {
			t.Fatal(err)
		}
		if err := a.st.PutDelivery(ctx, p.token, supervise.Delivery{ID: "prompt-inputs-" + randomSuffix(t), Task: p.task, Generation: 1, Phase: p.at, QueuedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *promptInputsRun) writeProfile(t *testing.T, home, models, enabled string) {
	t.Helper()
	dir := filepath.Join(home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, filepath.Join(dir, "models.yml"), models)
	writeProofFile(t, filepath.Join(dir, "config.yml"), "enabledModels:\n"+enabled+"startup:\n  setupWizard: false\n")
}

func (r *promptInputsRun) claudeAgent(state string, spec runtime.SpawnSpec, promptFile string, endpoint *promptInputsModel) ([]string, []string, error) {
	// claude-bridge puts a Unix socket below TMPDIR, so it needs the caller's short scratch
	// path rather than the deeper fixture or daemon state directories.
	dir := filepath.Join(state, "claude", string(spec.Claim))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	mcp, err := claudelaunch.MCPConfig(r.bun, r.claudePlugin, dir)
	if err != nil {
		return nil, nil, err
	}
	configPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(configPath, mcp, 0600); err != nil {
		return nil, nil, err
	}
	session, resume := claudelaunch.NewSessionID(), spec.ResumeSessionFile != ""
	if resume {
		session, err = claudelaunch.ResumeSession(dir, spec.ResumeSessionFile)
		if err != nil {
			return nil, nil, err
		}
	}
	argv := claudelaunch.Argv(claudelaunch.Paths{Legion: r.legion, Claude: r.claude, PluginDir: r.claudePlugin}, configPath, session, resume, promptFile)
	env := append(claudelaunch.Environment(dir), "TMPDIR="+r.socketDir, "ANTHROPIC_BASE_URL="+endpoint.url, promptInputsStandinKeyName+"="+promptInputsStandinKeyValue, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
	if endpoint.gateway != nil {
		env = append(env, "ANTHROPIC_MODEL="+promptInputsReadsModel, "CLAUDE_CODE_MAX_OUTPUT_TOKENS=8192")
	} else {
		env = append(env, "ANTHROPIC_MODEL="+endpoint.name+standinModelID)
	}
	return argv, env, nil
}

// Keep the daemon's path order and addressing. Substitute only the subject's contents in the
// SpawnSpec before localRuntime calls ComposePrompt. A path outside the composer is a failure.
type promptInputsRuntime struct {
	*localRuntime
	run   *promptInputsRun
	panes map[claim.Token]*promptInputsPane
}

func (rt *promptInputsRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	var err error
	spec.Prompt, err = rt.subjectPrompt(spec.Claim, spec.Prompt)
	if err != nil {
		return runtime.Locator{}, err
	}
	return rt.localRuntime.Spawn(ctx, spec)
}
func (rt *promptInputsRuntime) Resume(ctx context.Context, loc *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	var err error
	spec.Prompt, err = rt.subjectPrompt(spec.Claim, spec.Prompt)
	if err != nil {
		return runtime.Locator{}, err
	}
	return rt.localRuntime.Resume(ctx, loc, spec)
}
func (rt *promptInputsRuntime) subjectPrompt(token claim.Token, parts runtime.PromptParts) (runtime.PromptParts, error) {
	pane := rt.panes[token]
	if pane == nil {
		return parts, fmt.Errorf("unexpected claim %s", token)
	}
	paths := make([]string, 0, len(parts.RolePromptPaths))
	var names []string
	for _, p := range parts.RolePromptPaths {
		rel, err := filepath.Rel(filepath.Join(rt.stateDir, "prompts"), p)
		if err != nil {
			return parts, err
		}
		dir, file, ok := strings.Cut(filepath.ToSlash(rel), "/")
		if !ok || (dir != "shared" && dir != "go") {
			return parts, fmt.Errorf("not a composed prompt part: %s", p)
		}
		if dir == "shared" {
			dir = "roles"
		}
		name := dir + "/" + file
		names = append(names, name)
		path := filepath.Join(rt.run.subject, "packages/daemon/internal/prompts", name)
		if pane.extra && len(paths) == 0 {
			data, err := os.ReadFile(path)
			if err != nil {
				return parts, err
			}
			path = filepath.Join(rt.stateDir, "extra-read.md")
			instruction := "Read `" + promptInputsControlRead + "` before any other tool call. This is a mandatory read for this fixture.\n\n"
			if err := os.WriteFile(path, append([]byte(instruction), data...), 0600); err != nil {
				return parts, err
			}
		}
		paths = append(paths, path)
	}
	if !slices.Equal(names, pane.launch.Parts) {
		return parts, fmt.Errorf("%s daemon composed %v, want %v", pane.name, names, pane.launch.Parts)
	}
	parts.RolePromptPaths = paths
	return parts, nil
}

func (r *promptInputsRun) captureInteractive(a *liveAgentProof, rt *promptInputsRuntime, pane *promptInputsPane, overlays []string) {
	t := a.t
	composer, err := prompts.New(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := composer.ControllerPromptPaths(false)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture does not configure a design gate, a review App installation, or deployment
	// instructions. Compose as controller start does, with that reach and that gate.
	parts, err := rt.subjectPrompt(pane.token, runtime.PromptParts{RolePromptPaths: paths, Addressing: DesignGateFragment(nil) + " " + RepositoriesSentence(nil)})
	if err != nil {
		t.Fatal(err)
	}
	text, err := runtime.ComposePrompt(parts)
	if err != nil {
		t.Fatal(err)
	}
	file, err := runtime.WriteSystemPrompt(a.cfg.StateDir, "prompt-inputs-interactive", text)
	if err != nil {
		t.Fatal(err)
	}
	// The terminal command's first message is fixed. Feed that message to this one process
	// through RPC instead of allocating a terminal in the build pod.
	cmd := exec.Command(r.omp, "--mode", "rpc", promptWord, file)
	if pane.reads {
		dir := filepath.Join(r.work, "interactive-reads-session")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cmd.Args = append(cmd.Args, "--session-dir", dir)
	}
	cmd.Dir = r.fixture
	cmd.Env = a.rt.paneEnvironment([]string{"LEGION_CONTROLLER=1", "LEGION_ROLE=controller", "LEGION_PROJECT=" + a.cfg.Project, "LEGION_DAEMON_URL=" + a.cfg.DaemonURL, "DISPATCH_URL=" + r.dispatch.base, "DISPATCH_TOKEN_FILE=" + r.dispatchFile, "ENVOY_URL=" + a.rt.envoyURL, "PI_CONFIG_FILES=" + strings.Join(overlays, string(os.PathListSeparator))}, r.fixture)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log := evidenceFile(t, filepath.Join(r.evidence, promptInputsSlug(pane.name)+".log"))
	cmd.Stdout, cmd.Stderr = log, log
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	if err := json.NewEncoder(in).Encode(map[string]string{"type": "prompt", "message": pane.task}); err != nil {
		t.Fatal(err)
	}
	r.capture(a, pane, cmd.Process.Pid, r.omp, promptWord, parts, "")
}

func promptInputsFindArgv(root int, binary, flag string) (int, []string, string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, nil, "", err
	}
	parents := map[int]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err == nil {
			parents[pid] = parentOf(string(status))
		}
	}
	for pid := range parents {
		p := pid
		for p > 0 && p != root {
			p = parents[p]
		}
		if p != root {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if argv[0] != binary {
			continue
		}
		for i := len(argv) - 2; i >= 1; i-- {
			if argv[i] == flag {
				return pid, argv, argv[i+1], nil
			}
		}
	}
	return 0, nil, "", fmt.Errorf("process %d has no %s descendant with %s", root, binary, flag)
}

func (r *promptInputsRun) capture(a *liveAgentProof, p *promptInputsPane, root int, binary, flag string, parts runtime.PromptParts, session string) {
	t := a.t
	name := promptInputsSlug(p.name)
	var response promptInputsCapture
	select {
	case response = <-p.model.done:
	case <-time.After(paneReady):
		t.Fatalf("%s did not complete a recording; see pane logs in %s", p.name, r.evidence)
	}
	if len(response.raw) != 0 {
		r.writeEvidence("requests/"+name+".json", response.raw)
	}
	if response.err != nil {
		if p.reads && p.launch.Backend == runtime.BackendClaude && p.model.gateway.unreachable() {
			r.writeJSON("claude-gateway.json", map[string]string{"status": "not observed", "reason": response.err.Error()})
			t.Logf("Claude Code gateway probe: %v", response.err)
			return
		}
		t.Fatalf("%s: %v", p.name, response.err)
	}
	pid, argv, promptFile, err := promptInputsFindArgv(root, binary, flag)
	if err != nil {
		t.Fatal(err)
	}
	launched := promptInputsReadFile(t, promptFile)
	want, err := runtime.ComposePrompt(parts)
	if err != nil || string(launched) != want {
		t.Fatalf("%s pid %d: argv's prompt does not match composition: %v", p.name, pid, err)
	}
	r.writeEvidence("prompts/"+name+".md", launched)
	r.writeJSON("argv/"+name+".json", map[string]any{"pid": pid, "argv": argv})
	split, err := promptInputsSplitRequest(response.raw, string(launched))
	if err != nil {
		t.Fatal(err)
	}
	r.writeJSON("splits/"+name+".json", split)
	if !split.LegionPromptFound || split.DispatchFirstBytes == 0 || split.FirstMessageBytes == 0 || split.ToolDefinitionsBytes == 0 {
		t.Fatalf("%s: missing prompt, dispatch-first, first message or tools (see splits/%s.json)", p.name, name)
	}
	var actual []string
	for _, skill := range split.Skills {
		actual = append(actual, promptInputsNormalizeSkillName(skill))
	}
	for _, want := range r.skills {
		if !slices.Contains(actual, want) {
			t.Errorf("%s: request does not list packaged skill %s (lists %v)", p.name, want, actual)
		}
	}
	row := promptInputsRow{Launch: p.launch.Name, AddressingBytes: len(parts.Addressing), TaskBytes: len(p.task), FirstRequest: split.toRequest(), References: response.refs, ReadsObserved: "not observed"}
	for i, path := range parts.RolePromptPaths {
		b := promptInputsReadFile(t, path)
		row.Parts = append(row.Parts, promptInputsPart{Path: p.launch.Parts[i], Bytes: len(b), SHA256: fmt.Sprintf("%x", sha256.Sum256(b))})
	}
	if p.reads {
		row.RequiredReads = r.recordReads(a, p, session)
		row.ReadsObserved = "observed"
		row.TotalBytes = promptInputsTotal(row)
		if p.extra {
			before := r.rows[p.launch.Name]
			if slices.ContainsFunc(before.RequiredReads, func(read promptInputsRead) bool { return read.Path == promptInputsControlRead }) {
				t.Fatalf("normal %s already read %s", p.launch.Name, promptInputsControlRead)
			}
			if !slices.ContainsFunc(row.RequiredReads, func(read promptInputsRead) bool { return read.Path == promptInputsControlRead && read.Bytes > 0 }) || row.TotalBytes <= before.TotalBytes {
				t.Fatalf("extra-read control: %s did not add %s and increase the total", p.launch.Name, promptInputsControlRead)
			}
			r.writeJSON("control-extra-read.json", row)
			return
		}
		row.References = r.rows[p.launch.Name].References
	} else {
		for _, result := range response.refs {
			want := !p.withheld && result.Kind == "retained" || result.Kind == "removed" && r.isBase
			if result.Resolved != want {
				t.Errorf("%s %s %s: reachable=%t want %t (%s)", p.name, result.Kind, result.Path, result.Resolved, want, result.Detail)
			}
		}
		if p.withheld {
			r.writeJSON("control-withheld.json", response.refs)
			return
		}
		if session != "" {
			if data, err := os.ReadFile(session); err == nil {
				r.writeEvidence("standin-transcripts/"+name+".jsonl", data)
			}
		}
	}
	row.TotalBytes = promptInputsTotal(row)
	r.rows[row.Launch] = row
	r.writeJSON("rows/"+name+".json", row)
	fmt.Printf("   %s pid %d: request %d wire bytes, input total %d bytes\n", p.name, pid, split.TotalRequestBytes, row.TotalBytes)
}

func promptInputsTotal(row promptInputsRow) int {
	p := row.FirstRequest
	n := p.SystemTextBytes + p.ToolDefinitionsBytes + p.SkillListBytes + p.DispatchFirstBytes + p.FirstMessageBytes
	for _, read := range row.RequiredReads {
		n += read.Bytes
	}
	return n
}

func (r *promptInputsRun) finishReads() {
	for _, l := range promptInputsLaunches {
		if l.Backend != runtime.BackendClaude {
			continue
		}
		row := r.rows[l.Name]
		if row.ReadsObserved == "observed" {
			continue
		}
		row.RequiredReads = r.rows[string(l.Role)+"/omp"].RequiredReads
		row.ReadsObserved = "not observed"
		row.TotalBytes = promptInputsTotal(row)
		r.rows[l.Name] = row
		r.writeJSON("rows/"+promptInputsSlug(l.Name)+".json", row)
	}
}

// Each pane has a private HTTP endpoint, so a title-generation request or another pane's turn
// cannot advance the reference probe. A continuation must return the exact tool_use_id we sent.
type promptInputsModel struct {
	*modelStandin
	mu       sync.Mutex
	task     string
	refs     []promptInputsRefResult
	readArgs []map[string]any
	first    []byte
	next     int
	done     chan promptInputsCapture
	finished bool
	gateway  *promptInputsGateway
}

type promptInputsCapture struct {
	raw  []byte
	refs []promptInputsRefResult
	err  error
}

func (r *promptInputsRun) startModel(t *testing.T, p *promptInputsPane, name string) *promptInputsModel {
	t.Helper()
	m := &promptInputsModel{modelStandin: &modelStandin{name: name}, task: p.task, done: make(chan promptInputsCapture, 1)}
	if p.reads {
		m.gateway = promptInputsNewGateway(t, r.spend, r.gatewayAuth)
		t.Cleanup(func() { r.writePaneSpend(p) })
	} else {
		refs := r.refs.Launches[p.launch.Name]
		for i, ref := range []string{refs.Retained, refs.Removed} {
			m.refs = append(m.refs, promptInputsRefResult{Kind: []string{"retained", "removed"}[i], Path: ref})
			path := ref
			key := "path"
			if p.launch.Backend == runtime.BackendClaude {
				skill, part, err := promptInputsSkillTarget(ref)
				if err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(r.claudePlugin, "legion", "skills")
				if _, err := os.Stat(filepath.Join(root, skill)); os.IsNotExist(err) {
					root = filepath.Join(r.claudePlugin, "skills")
				}
				path = filepath.Join(root, skill, part)
				key = "file_path"
			}
			m.readArgs = append(m.readArgs, map[string]any{key: path})
		}
	}
	m.serve(t, m)
	return m
}

func (m *promptInputsModel) writeProvider(models, enabled *strings.Builder) string {
	id := m.name + standinModelID
	if m.gateway != nil {
		id = promptInputsReadsModel
	}
	fmt.Fprintf(models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: prompt-inputs-local-provider\n    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n        reasoning: true\n", m.name, m.url, id, id)
	selector := m.name + "/" + id
	fmt.Fprintf(enabled, "  - %s\n", selector)
	return selector
}

func (m *promptInputsModel) complete(err error) {
	if m.finished {
		return
	}
	m.finished = true
	m.done <- promptInputsCapture{raw: m.first, refs: slices.Clone(m.refs), err: err}
}

func (m *promptInputsModel) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"input_tokens":1}`)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 16<<20))
	if err != nil {
		http.Error(w, "cannot read model request", http.StatusBadRequest)
		return
	}
	if m.gateway != nil {
		m.mu.Lock()
		if m.first == nil && bytes.Contains(raw, []byte(m.task[:24])) {
			m.first = raw
		}
		m.mu.Unlock()
		m.gateway.serve(w, req, raw, m)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var call struct {
		Model string `json:"model"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		http.Error(w, "bad model JSON", http.StatusBadRequest)
		return
	}
	if !bytes.Contains(raw, []byte(m.task[:24])) || len(call.Tools) == 0 || m.finished {
		streamAnthropicText(w, m.name, "WAITING on the next fixture task.")
		return
	}
	if m.first == nil {
		m.first = raw
	}
	if m.next > 0 {
		id := fmt.Sprintf("toolu_prompt_inputs_%d", m.next)
		found := false
		for _, msg := range call.Messages {
			var blocks []struct {
				Type    string          `json:"type"`
				ID      string          `json:"tool_use_id"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(msg.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_result" || b.ID != id {
					continue
				}
				found = true
				result := &m.refs[m.next-1]
				result.Resolved = !b.IsError
				if b.IsError {
					result.Detail = "read tool returned an error"
				} else {
					result.Bytes = promptInputsContentTextBytes(b.Content)
				}
			}
		}
		if !found {
			m.complete(fmt.Errorf("missing tool result %s", id))
			streamAnthropicText(w, m.name, "WAITING on the next fixture task.")
			return
		}
	}
	if m.next == len(m.refs) {
		m.complete(nil)
		streamAnthropicText(w, m.name, "WAITING on the next fixture task.")
		return
	}
	for _, tool := range call.Tools {
		if strings.EqualFold(strings.TrimLeft(tool.Name, "_"), "read") {
			m.next++
			streamAnthropicToolUse(w, m.name, fmt.Sprintf("toolu_prompt_inputs_%d", m.next), tool.Name, m.readArgs[m.next-1])
			return
		}
	}
	m.complete(fmt.Errorf("request did not offer a read tool"))
	streamAnthropicText(w, m.name, "WAITING on the next fixture task.")
}
