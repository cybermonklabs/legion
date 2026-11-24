package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// The lines the daemon logs when a launch reads an instructions file again (instructionsCopies).
const (
	instructionsEditedLine    = "legion daemon read an edited instructions file"
	instructionsReadAgainLine = "legion daemon reads an instructions file again"
	instructionsKeptLine      = "legion daemon launches with the instructions it last read"
)

// writeInstructions replaces path with text in place, as an operator's editor on the daemon's host
// does.
func writeInstructions(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// configMapDir is a directory laid out as the kubelet lays out a ConfigMap mounted whole: the
// files of one generation in a directory of their own, `..data` a link to that generation, and
// each key a link to `..data/<key>`. An update writes a new generation and moves `..data` to it by
// rename, so a reader of a key's path reads the old generation or the new one, and then removes
// the old generation, as the kubelet does.
type configMapDir struct {
	t          *testing.T
	dir        string
	generation int
}

func newConfigMapDir(t *testing.T, files map[string]string) *configMapDir {
	t.Helper()
	d := &configMapDir{t: t, dir: t.TempDir()}
	d.update(files)
	for key := range files {
		if err := os.Symlink(filepath.Join("..data", key), d.path(key)); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// path is where the daemon reads key.
func (d *configMapDir) path(key string) string { return filepath.Join(d.dir, key) }

// update makes files the ConfigMap's next generation.
func (d *configMapDir) update(files map[string]string) {
	d.t.Helper()
	d.generation++
	generation := fmt.Sprintf("..generation-%d", d.generation)
	if err := os.Mkdir(filepath.Join(d.dir, generation), 0o700); err != nil {
		d.t.Fatal(err)
	}
	for key, text := range files {
		writeInstructions(d.t, filepath.Join(d.dir, generation, key), text)
	}
	link := filepath.Join(d.dir, "..data_tmp")
	if err := os.Symlink(generation, link); err != nil {
		d.t.Fatal(err)
	}
	if err := os.Rename(link, filepath.Join(d.dir, "..data")); err != nil {
		d.t.Fatal(err)
	}
	if d.generation > 1 {
		if err := os.RemoveAll(filepath.Join(d.dir, fmt.Sprintf("..generation-%d", d.generation-1))); err != nil {
			d.t.Fatal(err)
		}
	}
}

// composingRuntime is the fake runtime composing each launch's system prompt where both real
// runtimes compose it, inside Spawn and Resume (runtime.ComposePrompt), and keeping the latest by
// claim: the prompt that launch's agent would have been started with.
type composingRuntime struct {
	*fake.Runtime
	t       *testing.T
	mu      sync.Mutex
	prompts map[claim.Token]string
}

func newComposingRuntime(t *testing.T) *composingRuntime {
	return &composingRuntime{Runtime: fake.NewRuntime(), t: t, prompts: map[claim.Token]string{}}
}

func (r *composingRuntime) compose(spec runtime.SpawnSpec) {
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		r.t.Errorf("compose the prompt of %s's launch: %v", spec.Claim, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts[spec.Claim] = prompt
}

func (r *composingRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.compose(spec)
	return r.Runtime.Spawn(ctx, spec)
}

func (r *composingRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.compose(spec)
	return r.Runtime.Resume(ctx, prev, spec)
}

// prompt is the system prompt of token's latest launch.
func (r *composingRuntime) prompt(token claim.Token) string {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt, launched := r.prompts[token]
	if !launched {
		r.t.Fatalf("no launch of %s was made; the runtime saw %v", token, r.Methods())
	}
	return prompt
}

// worker is a spawn of role on issue, a child of the tree architect() opens.
func worker(issue string, role claim.Role) api.SpawnRequest {
	return api.SpawnRequest{Tree: "LEGION-1", Issue: issue, Role: role, Prompt: "Reply ready and wait."}
}

// recordTree records key as an issue of cfg's project before its daemon boots, so the operator's
// spawn of key's architect draws the tree's experiments onto it (api's drawExperiments): a tree the
// daemon holds no record of is drawn into no arm. No workflow runs in such a daemon, so nothing
// else reads the record.
func recordTree(t *testing.T, cfg config.Config, key string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: key, Project: cfg.Project, Tree: key, Title: "a tree drawn into memory",
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Phase: phase.Implementing, Generation: 1,
			Status: "in_progress", Rank: "A"})
	}); err != nil {
		t.Fatalf("record %s: %v", key, err)
	}
}

// startLogging boots cfg's daemon over rt, logging to a buffer the test reads.
func startLogging(t *testing.T, cfg config.Config, rt *composingRuntime) (*daemon, *testlog.Buffer) {
	t.Helper()
	logs := &testlog.Buffer{}
	return startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil))), logs
}

// filesNamed is the `file` of each of lines, in order.
func filesNamed(lines []map[string]any) []string {
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		file, _ := line["file"].(string)
		files = append(files, file)
	}
	return files
}

// A running daemon reads each instructions file its configuration names again before every launch,
// so an edit reaches the next launch with no restart: the deployment's own `instructions`, the
// `memory` block's and each arm of an experiment, the arm no unit was drawn into among them. The
// files here are a ConfigMap mounted whole, updated as the kubelet updates it, which is how a
// Kubernetes deployment changes its instructions without replacing the daemon. The daemon logs
// each edit it reads once, naming the file, and a launch with no edit since the last logs nothing.
func TestAnEditedInstructionsFileReachesTheNextLaunchWithNoRestart(t *testing.T) {
	t.Parallel()
	texts := map[string]string{
		"instructions.md": "Never push to main.\n",
		"memory.md":       "Read the tree's memory first.\n",
		"long.md":         "Write a long plan.\n",
		"short.md":        "Write a short plan.\n",
	}
	mount := newConfigMapDir(t, texts)
	overlay := filepath.Join(t.TempDir(), "memory.yml")
	writeInstructions(t, overlay, "{}\n")
	cfg := testConfig(t)
	cfg.InstructionsPath = mount.path("instructions.md")
	cfg.Projects = map[string]config.Project{cfg.Project: {
		Memory: config.Memory{Mode: config.ExperimentOn, ConfigFiles: []string{overlay}, InstructionsPath: mount.path("memory.md")},
		Experiments: []config.Experiment{{Name: "plan-length", Mode: config.ExperimentOn, Unit: config.ExperimentTree, OnVariant: "long",
			Variants: []config.Variant{
				{Name: "long", Weight: 1, InstructionsPath: mount.path("long.md")},
				{Name: "short", Weight: 1, InstructionsPath: mount.path("short.md")},
			}}},
	}}
	recordTree(t, cfg, "LEGION-1")
	rt := newComposingRuntime(t)
	d, logs := startLogging(t, cfg, rt)

	before := rt.prompt(d.spawn(architect()))
	for _, want := range []string{"Never push to main.", "Read the tree's memory first.", "Write a long plan."} {
		if !strings.Contains(before, want) {
			t.Fatalf("the first launch's prompt holds no %q:\n%s", want, before)
		}
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 0 {
		t.Fatalf("lines saying %q before any edit = %v, want none: boot's own read is no edit", instructionsEditedLine, lines)
	}

	mount.update(map[string]string{
		"instructions.md": "Open a pull request for every change.\n",
		"memory.md":       "Write what you learned to the tree's memory.\n",
		"long.md":         "Write a long plan with a test for each step.\n",
		"short.md":        "Write a short plan with one test.\n",
	})
	after := rt.prompt(d.spawn(worker("LEGION-2", claim.RoleImplementer)))
	for _, want := range []string{"Open a pull request for every change.", "Write what you learned to the tree's memory.", "Write a long plan with a test for each step."} {
		if !strings.Contains(after, want) {
			t.Errorf("the launch after the edit holds no %q:\n%s", want, after)
		}
	}
	for _, gone := range []string{"Never push to main.", "Read the tree's memory first.", "Write a long plan."} {
		if strings.Contains(after, gone) {
			t.Errorf("the launch after the edit still holds %q, the text boot read:\n%s", gone, after)
		}
	}
	if count, _ := boots(t, cfg); count != 1 {
		t.Errorf("boots = %d, want 1: the edit reached the launch with no restart", count)
	}

	lines := loggedLines(t, logs.String(), instructionsEditedLine)
	for _, line := range lines {
		if line["level"] != "INFO" {
			t.Errorf("a line saying %q is at %v, want INFO: %v", instructionsEditedLine, line["level"], line)
		}
	}
	want := []string{mount.path("instructions.md"), mount.path("memory.md"), mount.path("long.md"), mount.path("short.md")}
	if got := filesNamed(lines); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("lines saying %q name the files %v, want %v, each once: the arm no unit drew is read too", instructionsEditedLine, got, want)
	}

	rt.prompt(d.spawn(worker("LEGION-3", claim.RoleTester)))
	if again := loggedLines(t, logs.String(), instructionsEditedLine); len(again) != len(lines) {
		t.Errorf("lines saying %q after a launch with no edit = %d, want the %d the edit logged", instructionsEditedLine, len(again), len(lines))
	}
}

// An edit that leaves an instructions file unreadable or blank, which boot would refuse, stops no
// launch of a running daemon: each launch carries the text the daemon last read, and the daemon
// says so once for each refusal, at WARN, naming the file and the refusal. Once the file reads
// again the daemon says so once at INFO, even when it holds the very text the daemon kept, and a
// later edit reaches the next launch.
func TestAnInstructionsFileThatNoLongerReadsLaunchesWithTheTextLastRead(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.InstructionsPath = filepath.Join(t.TempDir(), "instructions.md")
	writeInstructions(t, cfg.InstructionsPath, "Never push to main.\n")
	rt := newComposingRuntime(t)
	d, logs := startLogging(t, cfg, rt)
	if prompt := rt.prompt(d.spawn(architect())); !strings.Contains(prompt, "Never push to main.") {
		t.Fatalf("the first launch's prompt holds no instructions:\n%s", prompt)
	}

	kept := func(issue string) {
		t.Helper()
		if prompt := rt.prompt(d.spawn(worker(issue, claim.RoleImplementer))); !strings.Contains(prompt, "Never push to main.") {
			t.Errorf("the launch of %s holds no instructions, want the text last read:\n%s", issue, prompt)
		}
	}
	warned := func(want int, refusal string) {
		t.Helper()
		lines := loggedLines(t, logs.String(), instructionsKeptLine)
		if len(lines) != want {
			t.Fatalf("lines saying %q = %d, want %d: %v", instructionsKeptLine, len(lines), want, lines)
		}
		last := lines[len(lines)-1]
		if cause, _ := last["error"].(string); last["level"] != "WARN" || last["file"] != cfg.InstructionsPath || !strings.Contains(cause, refusal) {
			t.Errorf("the last line saying %q is %v, want one at WARN naming %s and the refusal %q", instructionsKeptLine, last, cfg.InstructionsPath, refusal)
		}
	}

	writeInstructions(t, cfg.InstructionsPath, "  \n")
	kept("LEGION-2")
	kept("LEGION-3")
	warned(1, "is empty")

	if err := os.Remove(cfg.InstructionsPath); err != nil {
		t.Fatal(err)
	}
	kept("LEGION-4")
	warned(2, "could not be read")

	writeInstructions(t, cfg.InstructionsPath, "Never push to main.\n")
	kept("LEGION-5")
	kept("LEGION-6")
	if lines := loggedLines(t, logs.String(), instructionsReadAgainLine); len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["file"] != cfg.InstructionsPath {
		t.Errorf("lines saying %q = %v, want one at INFO naming %s: the file reads again with the text the daemon kept", instructionsReadAgainLine, lines, cfg.InstructionsPath)
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 0 {
		t.Errorf("lines saying %q = %v, want none: the file reads again with the text the daemon kept, which is no edit", instructionsEditedLine, lines)
	}

	writeInstructions(t, cfg.InstructionsPath, "Open a pull request for every change.\n")
	if prompt := rt.prompt(d.spawn(worker("LEGION-7", claim.RoleTester))); !strings.Contains(prompt, "Open a pull request for every change.") {
		t.Errorf("the launch after the edit holds no new text:\n%s", prompt)
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 1 {
		t.Errorf("lines saying %q = %v, want one, for the edit", instructionsEditedLine, lines)
	}
}

// A copy the daemon cannot replace stops no launch either, and is logged once at ERROR however many
// launches try it, although each try's error names a temporary file of a new name; once the copy
// can be replaced again, the next launch writes it and logs the edit. The arm here is one no unit
// was drawn into, so no launch reads its copy and every launch goes on.
func TestAnInstructionsCopyThatCannotBeReplacedIsLoggedOnce(t *testing.T) {
	t.Parallel()
	arm := filepath.Join(t.TempDir(), "long.md")
	writeInstructions(t, arm, "Write a long plan.\n")
	cfg := testConfig(t)
	cfg.Projects = map[string]config.Project{cfg.Project: {
		Experiments: []config.Experiment{{Name: "plan-length", Mode: config.ExperimentOff, Unit: config.ExperimentTree,
			Variants: []config.Variant{{Name: "long", Weight: 1, InstructionsPath: arm}, {Name: "control", Weight: 1}}}},
	}}
	rt := newComposingRuntime(t)
	d, logs := startLogging(t, cfg, rt)
	rt.prompt(d.spawn(architect()))

	// A directory that holds a file where the copy was: no rename of a file replaces it.
	copied := config.VariantInstructionsFile(cfg.StateDir, "plan-length", config.Variant{Name: "long"})
	if err := os.Remove(copied); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(copied, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeInstructions(t, arm, "Write a long plan with a test for each step.\n")
	rt.prompt(d.spawn(worker("LEGION-2", claim.RoleImplementer)))
	rt.prompt(d.spawn(worker("LEGION-3", claim.RoleTester)))
	refused := loggedLines(t, logs.String(), instructionsKeptLine)
	if len(refused) != 1 || refused[0]["level"] != "ERROR" || refused[0]["msg"] != instructionsKeptLine+": its copy could not be replaced" || refused[0]["file"] != arm {
		t.Fatalf("lines saying %q = %v, want one at ERROR naming %s and its copy", instructionsKeptLine, refused, arm)
	}

	if err := os.RemoveAll(copied); err != nil {
		t.Fatal(err)
	}
	rt.prompt(d.spawn(worker("LEGION-4", claim.RoleReviewer)))
	if got := filesNamed(loggedLines(t, logs.String(), instructionsEditedLine)); strings.Join(got, " ") != arm {
		t.Errorf("lines saying %q name the files %v, want %s once: the copy is replaced at the next launch", instructionsEditedLine, got, arm)
	}
}
