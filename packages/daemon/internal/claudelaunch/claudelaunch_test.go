package claudelaunch

import (
	"strings"
	"testing"
)

var testPaths = Paths{Legion: "/opt/legion/bin/legion", Claude: "/usr/local/bin/claude", PluginDir: "/opt/legion/claude-envoy"}

// The agent command as words, which is what a pod's argv carries and what a pane's shell renders:
// the bridge around `claude -p` in stream-json both ways (--verbose, which stream-json output
// requires) re-emitting each user message it takes (--replay-user-messages), its thinking
// summarized so the pane and the Agents page are shown it (--thinking-display summarized), every
// permission granted, only the configured MCP server, both plugin directories, the session, and
// the system prompt as the path of the file the runtime wrote it to, never its text.
func TestArgv(t *testing.T) {
	session := "11111111-2222-4333-8444-555555555555"
	got := Argv(testPaths, "/state/claude-mcp.json", session, false, "/state/system-prompt.md")
	want := []string{
		"/opt/legion/bin/legion", "claude-bridge", "--", "/usr/local/bin/claude",
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--replay-user-messages",
		"--thinking-display", "summarized",
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config", "--mcp-config", "/state/claude-mcp.json",
		"--plugin-dir", "/opt/legion/claude-envoy",
		"--plugin-dir", "/opt/legion/claude-envoy/legion",
		"--session-id", session,
		"--append-system-prompt-file", "/state/system-prompt.md",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("fresh:\n%q\nwant\n%q", got, want)
	}
	resumed := Argv(testPaths, "/state/claude-mcp.json", session, true, "/state/system-prompt.md")
	if resume := resumed[len(resumed)-4 : len(resumed)-2]; resume[0] != "--resume" || resume[1] != session {
		t.Errorf("resumed carries %q before its prompt file, want --resume %s", resume, session)
	}
}

// A Claude Code worker's own environment, in order: its configuration directory, and Claude Code's
// scheduled prompts off. Its background tasks are not turned off, under any runtime: a task that
// finishes wakes a turn the bridge reports to the daemon.
func TestEnvironment(t *testing.T) {
	got := strings.Join(Environment("/state/claude/c"), " ")
	if got != "CLAUDE_CONFIG_DIR=/state/claude/c CLAUDE_CODE_DISABLE_CRON=1" {
		t.Errorf("Environment = %s", got)
	}
	if strings.Contains(got, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS") {
		t.Errorf("Environment = %s, want nothing turning Claude Code's background tasks off", got)
	}
}

// A transcript is recognized by its shape alone, wherever its configuration directory is — a pod
// role's on the issue's volume and a tmux claim's under the daemon's state directory alike — and
// its session id is its name's. An Oh My Pi session file, a transcript name below another
// directory than a project's, and a project file named for no session id are none.
func TestTranscript(t *testing.T) {
	const id = "9f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	for _, file := range []string{
		"/legion/state/legion-legion-208-tester/claude/projects/--legion-workspaces-sjawhar-legion-legion-208--/" + id + ".jsonl",
		"/var/lib/legion/claude/legion-legion-legion-208-tester/projects/--cwd--/" + id + ".jsonl",
	} {
		if got, ok := Transcript(file); !ok || got != id {
			t.Errorf("Transcript(%s) = %q, %t; want %s", file, got, ok, id)
		}
	}
	for _, file := range []string{
		"/home/legion/.omp/agent/sessions/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_0198.jsonl",
		"/state/claude/c/projects/--cwd--/2026-09-23T12-00-00-000Z_0198.jsonl",
		"/state/claude/c/projects/--cwd--/sub/" + id + ".jsonl",
		"/state/claude/c/projects/" + id + ".jsonl",
		"/state/claude/c/projects/--cwd--/" + id + ".jsonl.bak",
		"",
	} {
		if got, ok := Transcript(file); ok {
			t.Errorf("Transcript(%s) = %q, true; want no transcript", file, got)
		}
	}
}
