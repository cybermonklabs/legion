package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var testClaude = ClaudeLaunch{Binary: "/opt/claude/bin/claude", PluginDir: "/opt/legion/claude-envoy", Bun: "/opt/bun/bin/bun"}

// The inner command of a Claude Code role: the bridge around `claude -p` in stream-json both ways
// (--verbose, which stream-json output requires) re-emitting each user message it takes, its
// thinking summarized so the pane is shown it, every permission granted, only the configured MCP
// server, both plugin directories, the session, and last `--append-system-prompt-file` on the
// claim's prompt file, a path, never the prompt's text.
func TestClaudeInnerCommand(t *testing.T) {
	const promptFile = "/state/secrets/c-system-prompt.md"
	got := claudeInnerCommand("/opt/legion/legion", testClaude, "/state/claude/c/mcp.json", "11111111-2222-4333-8444-555555555555", false, promptFile)
	want := "/opt/legion/legion claude-bridge -- /opt/claude/bin/claude -p --input-format stream-json --output-format stream-json --verbose" +
		" --replay-user-messages" +
		" --thinking-display summarized" +
		" --permission-mode bypassPermissions --strict-mcp-config --mcp-config /state/claude/c/mcp.json" +
		" --plugin-dir /opt/legion/claude-envoy --plugin-dir /opt/legion/claude-envoy/legion" +
		" --session-id 11111111-2222-4333-8444-555555555555" +
		" --append-system-prompt-file /state/secrets/c-system-prompt.md"
	if got != want {
		t.Errorf("fresh:\n%s\nwant\n%s", got, want)
	}
	resumed := claudeInnerCommand("/opt/legion/legion", testClaude, "/m.json", "11111111-2222-4333-8444-555555555555", true, "/state dir/p.md")
	if !strings.Contains(resumed, " --resume 11111111-2222-4333-8444-555555555555 ") || strings.Contains(resumed, "--session-id") ||
		!strings.HasSuffix(resumed, " --append-system-prompt-file '/state dir/p.md'") {
		t.Errorf("resumed: %s", resumed)
	}
}

// A resumed claim continues the session its recorded transcript names, and only a transcript of the
// claim's own Claude Code directory: a recorded Oh My Pi session is refused by name.
func TestClaudeResumeSession(t *testing.T) {
	dir := "/state/claude/legion-acme-acme-7-implementer"
	id := "11111111-2222-4333-8444-555555555555"
	got, err := claudelaunch.ResumeSession(dir, filepath.Join(dir, "projects", "-state-workspaces-acme-7", id+".jsonl"))
	if err != nil || got != id {
		t.Errorf("claudelaunch.ResumeSession = %q, %v", got, err)
	}
	for file, want := range map[string]string{
		"/state/trees/x/.omp/2026-10-18T00-00-00Z_" + id + ".jsonl":   "is not a Claude Code transcript",
		filepath.Join(dir, "projects", "p", "sub", id+".jsonl"):       "is not a Claude Code transcript",
		filepath.Join(dir, "projects", "p", "not-a-session.jsonl"):    "is not named for a Claude Code session id",
		filepath.Join(dir, "projects", "p", id+".jsonl.bak"):          "is not named for a Claude Code session id",
		filepath.Join(dir, "projects", "..", "projects", id+".jsonl"): "is not a Claude Code transcript",
	} {
		if _, err := claudelaunch.ResumeSession(dir, file); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("claudelaunch.ResumeSession(%s) = %v, want %q", file, err, want)
		}
	}
}

func TestNewSessionIDIsAVersion4UUID(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := claudelaunch.NewSessionID()
		if !uuid.MatchString(id) || seen[id] {
			t.Fatalf("newSessionID = %q", id)
		}
		seen[id] = true
	}
}

// The MCP configuration names the one server, on bun, with its plugin data in the claim's directory.
func TestWriteClaudeMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := writeClaudeMCPConfig(dir, testClaude)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the configuration's mode: %v, %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	var config struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.MCPServers[ClaudeMCPServer]
	if len(config.MCPServers) != 1 || !ok || server.Command != testClaude.Bun ||
		len(server.Args) != 1 || server.Args[0] != "/opt/legion/claude-envoy/dist/envoy-channel.js" ||
		server.Env["CLAUDE_PLUGIN_DATA"] != filepath.Join(dir, "envoy-plugin-data") {
		t.Errorf("the MCP configuration is %s", raw)
	}
}

// A fifo or a link at the MCP configuration's path, which the pane can leave there since the
// claim's directory is its own, is replaced by a 0600 regular file holding the configuration, and
// the file a link named is left as it was. Written with os.WriteFile, as it was, a fifo holds the
// spawn waiting for a reader for as long as none comes, and a link has the file it names truncated
// and written.
func TestWriteClaudeMCPConfigReplacesAFifoOrALinkAtItsPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		// plant puts the shape at path and answers the check of what it must leave as it was,
		// or nil.
		plant func(t *testing.T, path string) (kept func(t *testing.T))
	}{
		{"a fifo", func(t *testing.T, path string) func(t *testing.T) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"a link to a file outside the directory", func(t *testing.T, path string) func(t *testing.T) {
			precious := filepath.Join(t.TempDir(), "precious")
			if err := os.WriteFile(precious, []byte("precious\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(precious, path); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T) {
				t.Helper()
				if body, err := os.ReadFile(precious); err != nil || string(body) != "precious\n" {
					t.Errorf("%s, the file the link named, is %q (%v), want it as it was", precious, body, err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mcp.json")
			kept := tc.plant(t, path)
			type answer struct {
				path string
				err  error
			}
			answered := make(chan answer, 1)
			go func() {
				path, err := writeClaudeMCPConfig(dir, testClaude)
				answered <- answer{path, err}
			}()
			var got answer
			select {
			case got = <-answered:
			case <-time.After(time.Second):
				// A second is long for a write and a rename, but a loaded machine can take longer
				// without the write ever touching the fifo, so a late answer alone says nothing. A
				// write that waits in an open of the fifo waits for a reader: an end opened here for
				// reading releases it, and what it writes then comes through that end, while a
				// write by rename puts nothing in the fifo. The end stays open until the write
				// answers, so a writer's bytes are still in the fifo when it is read; the end opened
				// is read only while it is the fifo, never the regular file a rename put at the
				// path.
				got = readFifoUntilAnswered(t, path, answered, func(through []byte) {
					t.Fatalf("writeClaudeMCPConfig over %s wrote %q into the fifo, after waiting in its open for a reader", path, through)
				})
			}
			if got.err != nil || got.path != path {
				t.Fatalf("writeClaudeMCPConfig over %s = %s, %v, want it replaced", path, got.path, got.err)
			}
			if kept != nil {
				kept(t)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s is %v after writeClaudeMCPConfig, want a regular file of mode 0600", path, info.Mode())
			}
			want, err := claudelaunch.MCPConfig(testClaude.Bun, testClaude.PluginDir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if body, err := os.ReadFile(path); err != nil || string(body) != string(want) {
				t.Fatalf("%s holds %q (%v) after writeClaudeMCPConfig, want %q", path, body, err, want)
			}
		})
	}
}

// readFifoUntilAnswered holds the fifo at path open for reading until answered gives the answer it
// returns, reading what any writer puts in it, and calls wrote with those bytes when there are
// any. It gives up after 10 seconds. A path that is no longer a fifo is not read.
func readFifoUntilAnswered[A any](t *testing.T, path string, answered <-chan A, wrote func(through []byte)) A {
	t.Helper()
	var end *os.File
	if opened, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0); err == nil {
		if info, err := opened.Stat(); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
			end = opened
		} else {
			_ = opened.Close()
		}
	}
	var through []byte
	read := func() {
		if end == nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			n, err := end.Read(buf)
			through = append(through, buf[:n]...)
			if n == 0 || err != nil {
				return
			}
		}
	}
	give := time.After(10 * time.Second)
	for {
		read()
		select {
		case got := <-answered:
			read()
			if end != nil {
				_ = end.Close()
			}
			if len(through) > 0 {
				wrote(through)
			}
			return got
		case <-give:
			t.Fatalf("no answer for %s in 11 seconds", path)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A Claude Code pane carries its claim's directory and Claude Code's scheduled prompts off, beside
// the pairs every pane carries. Nothing on the pane turns its background tasks off: a task that
// finishes wakes a turn of Claude Code's own, which the bridge reports to the daemon.
func TestClaudeEnvironment(t *testing.T) {
	got := strings.Join(claudeEnvironment("/state/claude/c"), " ")
	if got != "-e CLAUDE_CONFIG_DIR=/state/claude/c -e CLAUDE_CODE_DISABLE_CRON=1" {
		t.Errorf("claudeEnvironment = %s", got)
	}
	if strings.Contains(got, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS") {
		t.Errorf("claudeEnvironment = %s, want no pane pair turning Claude Code's background tasks off", got)
	}
}

// A Claude Code launch resuming a recorded Oh My Pi session is refused before anything is written;
// a fresh one writes its claim's directory and runs the bridge, its prompt flag naming the prompt
// file the launch hands it.
func TestClaudeLauncherLaunch(t *testing.T) {
	spec := testSpec()
	spec.Backend = runtime.BackendClaude
	host := Host{StateDir: t.TempDir(), Legion: "/opt/legion/legion"}
	launcher := NewClaudeLauncher(testClaude)
	spec.ResumeSessionFile = "/state/trees/x/.omp/session.jsonl"
	if _, _, err := launcher.Launch(spec, host, "/state/p.md"); err == nil || !strings.Contains(err.Error(), "the role's backend changed") {
		t.Errorf("an Oh My Pi session: %v", err)
	}
	spec.ResumeSessionFile = ""
	inner, pairs, err := launcher.Launch(spec, host, "/state/secrets/c-system-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	configDir := ClaudeConfigDir(host.StateDir, spec.Claim)
	if configDir != filepath.Join(host.StateDir, "claude", string(spec.Claim)) {
		t.Errorf("ClaudeConfigDir = %s", configDir)
	}
	if !strings.Contains(inner, " claude-bridge -- ") || !strings.Contains(inner, "--mcp-config "+filepath.Join(configDir, "mcp.json")) ||
		!strings.Contains(strings.Join(pairs, " "), "CLAUDE_CONFIG_DIR="+configDir) {
		t.Errorf("Launch = %s, %q", inner, pairs)
	}
	if want := " --append-system-prompt-file /state/secrets/c-system-prompt.md"; !strings.HasSuffix(inner, want) {
		t.Errorf("Launch = %s, want it to end %s", inner, want)
	}
}
