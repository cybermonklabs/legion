package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/registry"
)

var updateTree = flag.Bool("update", false, "rewrite testdata/tree.golden from newRoot")

// treeGolden is `legion`'s command tree as the spec's "Command trees" gives it, one line per
// command (treeLines).
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

// `legion`'s tree is the spec's: the three moves (config check, config reference, issue status),
// every other command where it was, and the commands a program runs hidden. Each case after the
// walk is a full invocation against a test double, with its exit code and effect.
func TestLegionTreeGolden(t *testing.T) {
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
		if want := demoCheckConfigOK + tmuxCapabilityGaps; code != 0 || stdout != want || stderr != "" {
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
	t.Run("issue status reaches the daemon's status route once", func(t *testing.T) {
		daemon := newStatusDaemon(t)
		code, stdout, stderr := legionRun(t, "issue", "status", "DEMO-1", "todo", "--operator-token-file", operatorTokenFile(t), "--port", daemon.port)
		if code != 0 {
			t.Fatalf("legion issue status DEMO-1 todo = %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
		}
		if got := daemon.statusWrites(); !slices.Equal(got, []api.IssueStatusRequest{{GrantID: "grant-1", Issue: "DEMO-1", Status: "todo"}}) {
			t.Fatalf("the status route got %+v, want one write of DEMO-1 todo under the minted grant", got)
		}
	})
	t.Run("status <issue> <status> names issue status", func(t *testing.T) {
		daemon := newStatusDaemon(t)
		code, stdout, stderr := legionRun(t, "status", "DEMO-1", "todo", "--operator-token-file", operatorTokenFile(t), "--port", daemon.port)
		if code != 2 || stdout != "" || !strings.Contains(stderr, `"legion issue status DEMO-1 todo"`) {
			t.Fatalf("legion status DEMO-1 todo = %d, stdout %q, stderr %q; want 2 naming legion issue status", code, stdout, stderr)
		}
		if requests := daemon.requests(); len(requests) != 0 {
			t.Fatalf("the daemon got %q, want nothing", requests)
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

// statusDaemon is a daemon's two routes an issue status call reaches: the grants route, which
// mints grant-1 for the operator bearer, and the issue status route, which records each write.
type statusDaemon struct {
	port   string
	mu     sync.Mutex
	seen   []string
	writes []api.IssueStatusRequest
}

func newStatusDaemon(t *testing.T) *statusDaemon {
	t.Helper()
	d := &statusDaemon{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.seen = append(d.seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/legion/v1/grants":
			if r.Header.Get("Authorization") != "Bearer "+claimsOperatorToken {
				http.Error(w, `{"error":"not the operator bearer"}`, http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(api.GrantResponse{GrantID: "grant-1"})
		case "/legion/v1/issues/status":
			var write api.IssueStatusRequest
			if err := json.NewDecoder(r.Body).Decode(&write); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			d.writes = append(d.writes, write)
			_, _ = io.WriteString(w, "{}\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, d.port, err = net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *statusDaemon) statusWrites() []api.IssueStatusRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.writes)
}

func (d *statusDaemon) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}
