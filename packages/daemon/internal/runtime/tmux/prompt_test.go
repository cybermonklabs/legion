package tmux

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// writeLaunchPrompt writes spec's composed system prompt to the claim's prompt file under host's
// state directory, as launch does before the pane opens, and returns its path, for a test that runs
// a launcher's command itself: Oh My Pi handed a path it cannot open would take the path's text for
// its prompt.
func writeLaunchPrompt(t *testing.T, spec runtime.SpawnSpec, host Host) string {
	t.Helper()
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	promptFile, err := runtime.WriteSystemPrompt(host.StateDir, string(spec.Claim), prompt)
	if err != nil {
		t.Fatal(err)
	}
	return promptFile
}

// The OMP command inside the shim, as runtime-tmux.ts:430 builds it: prefix and invocation,
// `--model @default`, `--resume=<file>` when resuming, `--mode rpc` (processes.test.ts:1126 fresh,
// :3950 resumed), then `--append-system-prompt` on the path of the claim's prompt file, one word
// quoted for the pane's shell when it needs to be, never the prompt's text. The model flag rides
// both launches, `--model <model>` for a role of `roles` that names one: a resumed session would
// otherwise run its recorded model rather than the one the pod's overlay puts on the `default` role. From Oh My Pi 18.18.6 of the fork
// (upstream oh-my-pi #18534), a resumed launch under the role alias appends a replacement
// `model_change` for its resolved model, which later launches without the flag read.
func TestInnerCommand(t *testing.T) {
	const promptFile = "/state/secrets/legion-omp-legion-42-architect-system-prompt.md"
	if got, want := innerCommand(nil, "/opt/oh-my-pi/18.0.3/omp", "", "", promptFile),
		"/opt/oh-my-pi/18.0.3/omp --model @default --mode rpc --append-system-prompt "+promptFile; got != want {
		t.Errorf("fresh: got %q, want %q", got, want)
	}
	if got, want := innerCommand([]string{"env", "K=v", "--"}, "/opt/oh-my-pi/18.0.3/omp", "", "/state/trees/x/.omp/s 1.jsonl", "/state dir/secrets/p-system-prompt.md"),
		"env 'K=v' -- /opt/oh-my-pi/18.0.3/omp --model @default --resume='/state/trees/x/.omp/s 1.jsonl' --mode rpc --append-system-prompt '/state dir/secrets/p-system-prompt.md'"; got != want {
		t.Errorf("resumed: got %q, want %q", got, want)
	}
}

// A launch whose spec names the model of its role names that model where every other launch names
// @default, quoted for the pane's shell as one word, so a role of the deployment runs the model
// its settings give it.
func TestALaunchNamesTheModelOfItsRole(t *testing.T) {
	const promptFile = "/state/secrets/legion-omp-legion-42-checker-system-prompt.md"
	got := innerCommand(nil, "/opt/omp", "checker-model", "", promptFile)
	if !strings.Contains(got, " --model checker-model --mode rpc") || strings.Contains(got, "@default") {
		t.Errorf("innerCommand with the model checker-model = %q, want it to hold %q and no @default", got, " --model checker-model --mode rpc")
	}
	if quoted := innerCommand(nil, "/opt/omp", "a b;c", "", promptFile); !strings.Contains(quoted, " --model 'a b;c'") {
		t.Errorf("innerCommand with the model a b;c = %q, want it to hold %q", quoted, " --model 'a b;c'")
	}
}

// The pane's shell command: PATH exported before anything runs (tmux replaces a pane's PATH from
// the spawning client after copying the -e pairs — LEGION-91, runtime-tmux.ts:609-625), then the
// workspace, then the shim around the inner command (runtime-tmux.ts:599; the shim now dials the
// daemon's stream listener with the pane's boot token rather than serving a socket). With provider
// keys, the shim is pointed at the daemon-held directory whose files it exports into OMP's
// environment alone; without, the flag is absent.
func TestShimShellCommand(t *testing.T) {
	got := shimShellCommand(
		"legion-omp",
		"/state dir/bin:/usr/bin",
		"/state/workspaces/LEGION-42",
		"/opt/legion/legion",
		"unix:///state/worker-stream.sock",
		"/state/secrets/legion-omp-LEGION-42-architect",
		"/state dir/secrets/provider-env",
		"omp --mode rpc",
	)
	want := "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-omp && export PATH='/state dir/bin:/usr/bin' && cd /state/workspaces/LEGION-42 && /opt/legion/legion worker-shim" +
		" --connect unix:///state/worker-stream.sock --boot-token-file /state/secrets/legion-omp-LEGION-42-architect" +
		" --provider-env-dir '/state dir/secrets/provider-env' -- omp --mode rpc"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := shimShellCommand("legion-omp", "", "/w", "/l", "unix:///s", "/t", "", "omp"); got != "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-omp && cd /w && /l worker-shim --connect unix:///s --boot-token-file /t -- omp" {
		t.Errorf("no PATH, no provider keys: got %q", got)
	}
}
