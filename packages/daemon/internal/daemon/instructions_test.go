package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	instructionsEditedLine = "legion daemon read an edited instructions file"
	instructionsKeptLine   = "legion daemon launches with the instructions it last read"
)

// writeInstructions replaces path with text, as an operator's edit or a kubelet's update of a
// mounted ConfigMap does.
func writeInstructions(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// launchPrompt is the system prompt the claim's latest launch was composed with: the runtime
// composes it from the spec's files as it launches (runtime.ComposePrompt), and the daemon writes
// those files before it hands the spec over, so composing them again before the next launch reads
// what that launch read.
func launchPrompt(t *testing.T, rt *fake.Runtime, token claim.Token) string {
	t.Helper()
	prompt, err := runtime.ComposePrompt(lastLaunch(t, rt, token).Prompt)
	if err != nil {
		t.Fatalf("compose the prompt of %s's launch: %v", token, err)
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

// A running daemon reads each instructions file its configuration names again at every launch, so
// an edit of one reaches the next launch with no restart: the deployment's own `instructions` and
// an arm's, here the `memory` block's, alike. That is what lets a deployment change its
// instructions in place, as a Kubernetes daemon's ConfigMap mounted whole is changed, without
// replacing the daemon. The daemon logs each edit it reads, once, naming the file.
func TestAnEditedInstructionsFileReachesTheNextLaunchWithNoRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := testConfig(t)
	cfg.InstructionsPath = filepath.Join(dir, "instructions.md")
	memory, overlay := filepath.Join(dir, "memory.md"), filepath.Join(dir, "memory.yml")
	writeInstructions(t, cfg.InstructionsPath, "Never push to main.\n")
	writeInstructions(t, memory, "Read the tree's memory first.\n")
	writeInstructions(t, overlay, "{}\n")
	cfg.Projects = map[string]config.Project{cfg.Project: {
		Memory: config.Memory{Mode: config.ExperimentOn, ConfigFiles: []string{overlay}, InstructionsPath: memory},
	}}
	recordTree(t, cfg, "LEGION-1")
	rt := fake.NewRuntime()
	logs := &testlog.Buffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))

	before := launchPrompt(t, rt, d.spawn(architect()))
	for _, want := range []string{"Never push to main.", "Read the tree's memory first."} {
		if !strings.Contains(before, want) {
			t.Fatalf("the first launch's prompt holds no %q:\n%s", want, before)
		}
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 0 {
		t.Fatalf("lines saying %q before any edit = %v, want none: boot's own read is no edit", instructionsEditedLine, lines)
	}

	writeInstructions(t, cfg.InstructionsPath, "Open a pull request for every change.\n")
	writeInstructions(t, memory, "Write what you learned to the tree's memory.\n")
	after := launchPrompt(t, rt, d.spawn(worker("LEGION-2", claim.RoleImplementer)))
	for _, want := range []string{"Open a pull request for every change.", "Write what you learned to the tree's memory."} {
		if !strings.Contains(after, want) {
			t.Errorf("the launch after the edit holds no %q:\n%s", want, after)
		}
	}
	for _, gone := range []string{"Never push to main.", "Read the tree's memory first."} {
		if strings.Contains(after, gone) {
			t.Errorf("the launch after the edit still holds %q, the text boot read:\n%s", gone, after)
		}
	}
	if count, _ := boots(t, cfg); count != 1 {
		t.Errorf("boots = %d, want 1: the edit reached the launch with no restart", count)
	}

	lines := loggedLines(t, logs.String(), instructionsEditedLine)
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if line["level"] != "INFO" {
			t.Errorf("a line saying %q is at %v, want INFO: %v", instructionsEditedLine, line["level"], line)
		}
		file, _ := line["file"].(string)
		files = append(files, file)
	}
	if want := []string{cfg.InstructionsPath, memory}; strings.Join(files, " ") != strings.Join(want, " ") {
		t.Errorf("lines saying %q name the files %v, want %v, each once", instructionsEditedLine, files, want)
	}

	// A launch with no edit since the last one reads the files and logs nothing.
	launchPrompt(t, rt, d.spawn(worker("LEGION-3", claim.RoleTester)))
	if again := loggedLines(t, logs.String(), instructionsEditedLine); len(again) != len(lines) {
		t.Errorf("lines saying %q after a launch with no edit = %d, want the %d the edit logged", instructionsEditedLine, len(again), len(lines))
	}
}

// An edit that leaves an instructions file unreadable or blank, which boot would refuse, stops no
// launch of a running daemon: each launch carries the text the daemon last read, and the daemon
// says so once for each refusal, at WARN, naming the file and the refusal. A later edit that
// makes the file readable again reaches the next launch.
func TestAnInstructionsFileThatNoLongerReadsLaunchesWithTheTextLastRead(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.InstructionsPath = filepath.Join(t.TempDir(), "instructions.md")
	writeInstructions(t, cfg.InstructionsPath, "Never push to main.\n")
	rt := fake.NewRuntime()
	logs := &testlog.Buffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))
	if prompt := launchPrompt(t, rt, d.spawn(architect())); !strings.Contains(prompt, "Never push to main.") {
		t.Fatalf("the first launch's prompt holds no instructions:\n%s", prompt)
	}

	kept := func(issue string) {
		t.Helper()
		if prompt := launchPrompt(t, rt, d.spawn(worker(issue, claim.RoleImplementer))); !strings.Contains(prompt, "Never push to main.") {
			t.Errorf("the launch of %s holds no instructions, want the text last read:\n%s", issue, prompt)
		}
	}
	warned := func(want int, refusal string) {
		t.Helper()
		lines := warnLinesNaming(logs.String(), instructionsKeptLine)
		if len(lines) != want {
			t.Fatalf("WARN lines saying %q = %d, want %d:\n%s", instructionsKeptLine, len(lines), want, strings.Join(lines, "\n"))
		}
		if last := lines[len(lines)-1]; !strings.Contains(last, `"file":"`+cfg.InstructionsPath+`"`) || !strings.Contains(last, refusal) {
			t.Errorf("the last WARN line %s names no file %s and no refusal %q", last, cfg.InstructionsPath, refusal)
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

	writeInstructions(t, cfg.InstructionsPath, "Open a pull request for every change.\n")
	if prompt := launchPrompt(t, rt, d.spawn(worker("LEGION-5", claim.RoleTester))); !strings.Contains(prompt, "Open a pull request for every change.") {
		t.Errorf("the launch after the file reads again holds no new text:\n%s", prompt)
	}
	if lines := loggedLines(t, logs.String(), instructionsEditedLine); len(lines) != 1 {
		t.Errorf("lines saying %q = %v, want one, for the file that reads again", instructionsEditedLine, lines)
	}
}
