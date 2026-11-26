package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sjawhar/legion/daemon/internal/registry"
)

var updateTree = flag.Bool("update", false, "rewrite testdata/tree.golden from newRoot")

// treeGolden is `legion`'s whole command tree, one line per command (treeLines): a command that
// moves, appears or goes, or a flag that changes, is a change to this file a reviewer reads.
const treeGolden = "testdata/tree.golden"

// treeLines is the tree under cmd, one line per command in the order cobra lists them: its path,
// `hidden` for a command only a program runs, `words unread` for one that hands every word to the
// program it wraps, then each flag it declares as `--name type`. A flag's default is left out: some
// are read from the environment or from this build's contract, which are no part of the tree.
func treeLines(cmd *cobra.Command) []string {
	line := cmd.CommandPath()
	if cmd.Hidden {
		line += "\thidden"
	}
	if cmd.DisableFlagParsing {
		line += "\twords unread"
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) { line += "\t--" + f.Name + " " + f.Value.Type() })
	lines := []string{line}
	for _, child := range cmd.Commands() {
		lines = append(lines, treeLines(child)...)
	}
	return lines
}

// treeOf is the whole of treeLines for root, as the golden file holds it.
func treeOf(root *cobra.Command) string {
	return strings.Join(treeLines(root), "\n") + "\n"
}

// `legion`'s tree is the golden one: `config check`, `config reference` and `issue status` where
// they are, `start` without --check-config, and the commands a program runs hidden. Each case
// after the walk is a full invocation against a test double, with its exit code and effect.
func TestLegionTreeGolden(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (legionState).
	got := treeOf(newRoot(io.Discard, io.Discard))
	if *updateTree {
		if err := os.WriteFile(treeGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(treeGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("legion's tree is not %s:\n--- got\n%s--- want\n%s", treeGolden, got, want)
	}

	t.Run("config check checks and starts nothing", func(t *testing.T) {
		legions := legionState(t)
		config, _ := workflowConfig(t, 13370, "")
		code, stdout, stderr := legionRun(t, "config", "check", "--config", config)
		if want := demoCheckConfigOK + tmuxCapabilityGaps + tmuxPauseUnavailable; code != 0 || stdout != want || stderr != "" {
			t.Fatalf("legion config check = %d, stdout %q, stderr %q; want 0 and %q", code, stdout, stderr, want)
		}
		if _, found, err := registry.Find(legions, "DEMO"); err != nil || found {
			t.Fatalf("registry.Find = %v, %v, want no entry: a check takes no team", found, err)
		}
	})
	t.Run("start --check-config names config check", func(t *testing.T) {
		legions := legionState(t)
		config, _ := workflowConfig(t, 13370, "")
		code, stdout, stderr := legionRun(t, "start", "--check-config", "--config", config)
		if code != 2 || stdout != "" || stderr != "legion start: --check-config is now \"legion config check\"\n" {
			t.Fatalf("legion start --check-config = %d, stdout %q, stderr %q; want 2 naming legion config check", code, stdout, stderr)
		}
		if _, found, err := registry.Find(legions, "DEMO"); err != nil || found {
			t.Fatalf("registry.Find = %v, %v, want no entry: nothing started", found, err)
		}
	})
	t.Run("config reference prints the key table", func(t *testing.T) {
		code, stdout, stderr := legionRun(t, "config", "reference", "--json")
		var keys []map[string]any
		if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &keys) != nil || len(keys) == 0 {
			t.Fatalf("legion config reference --json = %d, stderr %q, stdout %.200q; want 0 and the key table", code, stderr, stdout)
		}
	})
	t.Run("config-reference names config reference", func(t *testing.T) {
		code, stdout, stderr := legionRun(t, "config-reference", "--json")
		if code != 2 || stdout != "" || !strings.Contains(stderr, `"legion config reference"`) {
			t.Fatalf("legion config-reference --json = %d, stdout %q, stderr %q; want 2 naming legion config reference", code, stdout, stderr)
		}
	})
	t.Run("issue status sets the status through the daemon", func(t *testing.T) {
		daemon := newControllerDaemon(t)
		tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
		code, stdout, stderr := legionRun(t, "issue", "status", "DEMO-1", "todo", "--operator-token-file", tokenFile, "--port", strconv.Itoa(daemon.port))
		if code != 0 {
			t.Fatalf("legion issue status DEMO-1 todo = %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
		}
		if got := daemon.dispatch.writes; !slices.Equal(got, []string{"DEMO-1=todo"}) {
			t.Fatalf("Dispatch was asked for %v, want one write of DEMO-1=todo", got)
		}
	})
	t.Run("status <issue> <status> names issue status", func(t *testing.T) {
		daemon := newControllerDaemon(t)
		tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
		code, stdout, stderr := legionRun(t, "status", "DEMO-1", "todo", "--operator-token-file", tokenFile, "--port", strconv.Itoa(daemon.port))
		if code != 2 || stdout != "" || !strings.Contains(stderr, `"legion issue status DEMO-1 todo"`) {
			t.Fatalf("legion status DEMO-1 todo = %d, stdout %q, stderr %q; want 2 naming legion issue status", code, stdout, stderr)
		}
		if requests := daemon.requests(); len(requests) != 0 {
			t.Fatalf("%d requests reached the daemon, want none", len(requests))
		}
	})
	t.Run("a mutant that keeps config-reference at the top fails the walk", func(t *testing.T) {
		mutant := newRoot(io.Discard, io.Discard)
		moved := newConfigReferenceCommand(io.Discard, io.Discard)
		moved.Use = "config-reference --json"
		mutant.AddCommand(moved)
		if treeOf(mutant) == string(want) {
			t.Fatal("a tree with config-reference at the top level reads as the golden tree")
		}
	})
}

// legionRun runs `legion args…` in this process and answers what it exited with and printed.
func legionRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}
