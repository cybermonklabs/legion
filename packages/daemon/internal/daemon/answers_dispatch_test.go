package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// dispatchTokenFileOf is a Dispatch token file of the test's own, as dispatch_token_file names
// one.
func dispatchTokenFileOf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(path, []byte("dsp-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noSearch is the end of the `answers: tools` line of answers that configure no web_search tool,
// which names it with why.
const noSearch = "; web_search: no answers.web_search block (set its model and price_per_search)"

// The answer loop's dispatch tool runs the dispatch command a pane runs, which boot finds as it
// finds the loop's gh (beside TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool):
// LEGION_DISPATCH_PATH first, which must be an absolute executable, else the first dispatch on
// PATH, a script among them, since the Envoy plugin's dispatch is a /bin/sh shim; and bun, which
// that shim starts, by LEGION_BUN_PATH or else on PATH. Where it finds no dispatch, no bun for it,
// or the daemon names no dispatch_url, the loop offers no dispatch tool and boot says why. The
// token file is the daemon's dispatch_token_file, made absolute.
func TestTheDispatchToolsCommandIsFoundAsTheLoopsGhIs(t *testing.T) {
	t.Parallel()
	onPath, elsewhere, empty := t.TempDir(), t.TempDir(), t.TempDir()
	shim := executable(t, onPath, "dispatch")
	pinned := executable(t, elsewhere, "dispatch")
	binary(t, onPath, "bun")
	ownBun := binary(t, elsewhere, "bun")
	missing := filepath.Join(t.TempDir(), "dispatch")
	token := dispatchTokenFileOf(t)
	const url = "https://dispatch.internal.example"
	for _, c := range []struct {
		name          string
		env           []string
		url           string
		dispatch, bun string
		absent        string
	}{
		{name: "the first dispatch on PATH, a script", env: []string{"PATH=" + empty + ":" + onPath}, url: url, dispatch: shim, bun: onPath},
		{name: "LEGION_DISPATCH_PATH before PATH", env: []string{"PATH=" + onPath, "LEGION_DISPATCH_PATH=" + pinned}, url: url, dispatch: pinned, bun: onPath},
		{name: "LEGION_BUN_PATH before PATH", env: []string{"PATH=" + onPath, "LEGION_BUN_PATH=" + ownBun}, url: url, dispatch: shim, bun: elsewhere},
		{name: "an override that is no absolute path", env: []string{"PATH=" + onPath, "LEGION_DISPATCH_PATH=dispatch"}, url: url,
			absent: `LEGION_DISPATCH_PATH is not an absolute executable path: "dispatch"`},
		{name: "no dispatch anywhere", env: []string{"PATH=" + empty}, url: url,
			absent: "no dispatch command on PATH or at " + missing + " (set LEGION_DISPATCH_PATH)"},
		{name: "no bun for it", env: []string{"PATH=" + empty, "LEGION_DISPATCH_PATH=" + pinned}, url: url,
			absent: "no bun for " + pinned + ", which runs on it (set LEGION_BUN_PATH, or put bun on PATH)"},
		{name: "no dispatch_url", env: []string{"PATH=" + onPath}, absent: "no dispatch_url, so no Dispatch to run it against"},
	} {
		got := answersDispatch(environLookup(c.env), c.url, token, missing)
		want := dispatchFound{dispatch: c.dispatch, bunDir: c.bun, absent: c.absent}
		if c.absent == "" {
			want.tokenFile = token
		}
		if got != want {
			t.Errorf("%s: found %+v, want %+v", c.name, got, want)
		}
	}
}

// Under dispatch_token_broker the daemon names no dispatch_token_file (config's
// resolveDispatchTokenBroker), and its broker token acts only as its own session, so no Dispatch
// token exists for an agent's session. With dispatch and bun both found, the loop still offers no
// dispatch tool, and boot says why; the empty token file never becomes the daemon's working
// directory, which the dispatch command cannot read as DISPATCH_TOKEN_FILE.
func TestTheDispatchToolIsAbsentWithNoDispatchTokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	executable(t, dir, "dispatch")
	binary(t, dir, "bun")
	got := answersDispatch(environLookup([]string{"PATH=" + dir}), "https://dispatch.internal.example", "", filepath.Join(t.TempDir(), "dispatch"))
	if want := (dispatchFound{absent: "no dispatch_token_file, so no Dispatch token for an agent's session"}); got != want {
		t.Fatalf("with no dispatch_token_file, found %+v; want %+v", got, want)
	}
}

// On Kubernetes the daemon runs from the worker image, whose PATH does not hold the Envoy plugin's
// bin directory, so a daemon that finds no dispatch on PATH runs the one the image keeps there,
// /opt/legion/pi-envoy/bin/dispatch (the image build's last step runs its --help); and boot reads
// it beside the loop's gh, so the daemon the boot starts carries it.
func TestAKubernetesDaemonFindsDispatchWhereTheWorkerImageKeepsIt(t *testing.T) {
	t.Parallel()
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dockerfile, []byte(dispatchInstall+" --help")) {
		t.Fatalf("the daemon looks for the worker image's dispatch at %s, and worker.Dockerfile runs no %s --help", dispatchInstall, dispatchInstall)
	}
	image, bunDir := t.TempDir(), t.TempDir()
	installed := executable(t, image, "dispatch")
	binary(t, bunDir, "bun")
	token := dispatchTokenFileOf(t)
	got := answersDispatch(environLookup([]string{"PATH=" + bunDir}), "https://dispatch.internal.example", token, installed)
	if want := (dispatchFound{dispatch: installed, bunDir: bunDir, tokenFile: token}); got != want {
		t.Fatalf("with dispatch only where the image keeps it, found %+v; want %+v", got, want)
	}

	cfg := kubernetesConfig(t, "https://127.0.0.1:1")
	cfg.LeakCheck = leakCheckSetting
	cfg.DispatchURL, cfg.DispatchTokenFile = "https://dispatch.internal.example", token
	cfg.Answers = answersConfig(filepath.Join(t.TempDir(), "ran"), dsnFileOf(t, conversationsDSN+"\n"))
	gh := binary(t, t.TempDir(), "gh")
	reads, err := readBoot(cfg, environLookup([]string{"PATH=" + bunDir, "LEGION_GH_PATH=" + gh, "LEGION_DISPATCH_PATH=" + installed,
		leakguard.TagVariable + "=" + leakCheckTag}), os.Getenv, true, quietLogger())
	if err != nil {
		t.Fatalf("readBoot with answers and dispatch: %v", err)
	}
	if want := (dispatchFound{dispatch: installed, bunDir: bunDir, tokenFile: token}); reads.answers.dispatch != want {
		t.Fatalf("boot's answer reads hold dispatch %+v; want %+v", reads.answers.dispatch, want)
	}
}

// The daemon's boot names the answer loop's tools in one line, `answers: tools`, whose tools key
// lists each tool the loop offers and which names each tool it does not offer by its own key, with
// why; an after_merge check reads it on INFRA. `legion config check` says the same in one line. A
// daemon that found dispatch hands the loop a dispatch tool, which opens each answer's directory
// under the state directory; one that found none hands it none. Its answers configure no
// web_search tool, which the line names with why (TestTheBootLineNamesWebSearchOfferedOrWhyNot).
func TestTheBootLineNamesTheLoopsToolsAndADaemonWithNoDispatchOffersNone(t *testing.T) {
	t.Parallel()
	stateDir, token := t.TempDir(), dispatchTokenFileOf(t)
	shim := executable(t, t.TempDir(), "dispatch")
	bunDir := t.TempDir()
	machine := filepath.Join(t.TempDir(), "public-leak-check.sh")
	cfg := config.Config{StateDir: stateDir, Answers: &config.Answers{Bounds: config.AnswerBounds{GHCall: 30 * time.Second, GHOutputChars: 60000, GHCalls: 20}},
		DispatchURL: "https://dispatch.internal.example", DispatchTokenFile: token}
	const why = "no dispatch command on PATH or at /opt/legion/pi-envoy/bin/dispatch (set LEGION_DISPATCH_PATH)"
	for _, c := range []struct {
		name  string
		found dispatchFound
		tools []any
		line  string
	}{
		{name: "dispatch found", found: dispatchFound{dispatch: shim, bunDir: bunDir, tokenFile: token}, tools: []any{"gh", "dispatch"}, line: "answers: tools [gh dispatch]" + noSearch},
		{name: "no dispatch", found: dispatchFound{absent: why}, tools: []any{"gh"}, line: "answers: tools [gh]; dispatch: " + why + noSearch},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			w := &workflowRuntime{log: slog.New(slog.NewJSONHandler(&logs, nil)), answers: &answering{}, dispatch: dispatch.New(cfg.DispatchURL, dispatch.BearerTransport("t", nil))}
			p := plan{answers: answersReads{leakCheck: machine, dispatch: c.found}}
			stop, err := w.attachAnswers(context.Background(), cfg, p, &supervision{}, nil)
			if err != nil {
				t.Fatalf("attachAnswers: %v", err)
			}
			defer stop()
			var lines []map[string]any
			for _, raw := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
				var line map[string]any
				if err := json.Unmarshal(raw, &line); err != nil {
					t.Fatal(err)
				}
				if line["msg"] == "answers: tools" {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("boot logged %d answers: tools lines, want 1: %s", len(lines), logs.String())
			}
			if tools, _ := lines[0]["tools"].([]any); !slices.Equal(tools, c.tools) {
				t.Errorf("the line's tools are %v, want %v", lines[0]["tools"], c.tools)
			}
			if reason, named := lines[0]["dispatch"]; named != (c.found.absent != "") || named && reason != c.found.absent {
				t.Errorf("the line names dispatch as %v (%t), want %q", reason, named, c.found.absent)
			}
			if got := loopToolsOf(c.found, *cfg.Answers).String(); got != c.line {
				t.Errorf("legion config check's line is %q, want %q", got, c.line)
			}

			tools := w.answersLoop(cfg, p, &supervision{}, nil, nil, ghTools{}).DispatchTools
			if c.found.dispatch == "" {
				if tools != nil {
					t.Fatalf("a daemon with no dispatch hands the loop the dispatch tools %v; want none", tools)
				}
				return
			}
			if tools == nil {
				t.Fatal("a daemon that found dispatch hands the loop no dispatch tool")
			}
			opened, err := tools.Open("ses_tester", "dispatch:m-1", dispatchtool.Place{})
			if err != nil {
				t.Fatalf("open the answer's dispatch tool: %v", err)
			}
			home := filepath.Join(stateDir, "answers", "dispatch:m-1", "dispatch", "home")
			if _, err := os.Stat(home); err != nil {
				t.Errorf("the answer's HOME: %v", err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// `legion config check` reads the line from the daemon's environment as boot does: with answers
// it names the loop's tools, and without answers it names none.
func TestConfigCheckNamesTheLoopsToolsAsBootDoes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shim := executable(t, dir, "dispatch")
	binary(t, dir, "bun")
	cfg := config.Config{Answers: &config.Answers{}, DispatchURL: "https://dispatch.internal.example", DispatchTokenFile: dispatchTokenFileOf(t)}
	if line, ok := AnswersToolsLine(cfg, environLookup([]string{"PATH=" + dir, "LEGION_DISPATCH_PATH=" + shim})); !ok || line != "answers: tools [gh dispatch]"+noSearch {
		t.Errorf("AnswersToolsLine = %q, %t; want the line naming gh and dispatch", line, ok)
	}
	if line, ok := AnswersToolsLine(cfg, environLookup([]string{"PATH=" + t.TempDir(), "LEGION_DISPATCH_PATH=" + shim})); !ok ||
		line != "answers: tools [gh]; dispatch: no bun for "+shim+", which runs on it (set LEGION_BUN_PATH, or put bun on PATH)"+noSearch {
		t.Errorf("AnswersToolsLine with no bun = %q, %t; want the line naming why dispatch is not offered", line, ok)
	}
	cfg.Answers = nil
	if line, ok := AnswersToolsLine(cfg, environLookup(nil)); ok {
		t.Errorf("AnswersToolsLine with no answers = %q; want none", line)
	}
}
