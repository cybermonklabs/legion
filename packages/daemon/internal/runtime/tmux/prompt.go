package tmux

import (
	"strings"

	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// innerCommand is the OMP command the shim runs: the launch prefix and invocation, one `--extension
// <root>` for each of extensionRoots (an experiment arm's extension root, which Oh My Pi loads
// ahead of the plugins a pane has installed, so it comes before `--model`), `--model @default`,
// `--resume` on the recorded session file when resuming, RPC mode (runtime-tmux.ts:414-431), and
// `--append-system-prompt` on promptFile, which Oh My Pi reads and appends to its own system
// prompt. A launch of no arm's folder names no root, and its command is what it always was.
//
// `--model @default` rides every launch, not the resuming one alone: @default is Oh My Pi's name
// for the `default` model role, which the settings of the profile the pane launches under set -
// the launch prefix picks that profile (`omp_launch_prefix`, `env OMP_PROFILE=… --`) - the
// operator's current role is resolved per launch, and a resumed session without the flag runs its
// recorded model. From Oh My Pi 18.18.6 of the fork (upstream oh-my-pi #18534), a resumed launch
// under this role alias appends a replacement `model_change` for its resolved model, which later
// launches without the flag read; that replacement is the role as it stood at the launch that
// wrote it, so it stands in for no later launch's flag. A settings set with no `default` role is
// not refused here: Oh My Pi launches silently on a built-in model instead
// (deploy/kubernetes/operator-route/README.md, "Changing a role's model").
func innerCommand(prefix []string, invocation, resumeSessionFile, promptFile string, extensionRoots ...string) string {
	var extensions strings.Builder
	for _, root := range extensionRoots {
		extensions.WriteString(" --extension " + shellprefix.Word(root))
	}
	resume := ""
	if resumeSessionFile != "" {
		resume = " --resume=" + shellprefix.Word(resumeSessionFile)
	}
	return omplaunch.WithPrefix(prefix, invocation) + extensions.String() + " --model @default" + resume + " --mode rpc " + omplaunch.SystemPromptArgument(promptFile)
}

// shimShellCommand is the pane's shell command: PATH exported first, then the workspace, then
// `legion worker-shim` dialing the daemon's stream listener with the pane's boot token file, around
// the inner command (runtime-tmux.ts:589-601). With provider keys, the shim is also pointed at the
// daemon-held directory whose files it exports into OMP's environment alone
// (`--provider-env-dir`); "" passes no such flag.
//
// PATH is exported here because it cannot ride a -e pair: tmux copies the -e pairs into a new
// pane's environment and then replaces PATH from the spawning client's own (spawn.c, "The session
// one is replaced from the client if there is one"), so a `-e PATH=…` never reaches a pane
// (LEGION-91, runtime-tmux.ts:603-618). An empty path exports nothing.
//
// The inner command is spliced in unquoted on purpose: its words are already quoted for the pane's
// shell, which word-splits them, so the shim receives OMP's argv, not one string. The pane
// marks its own window before it starts the shim: a crash after new-window created the pane but
// before the daemon received its report leaves an ownership marker the next daemon can reap.
func shimShellCommand(socket, path, workspace, legion, streamAddress, bootTokenFile, providerEnvDir, inner string) string {
	marker := "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner " + shellprefix.Word(socket) + " && "
	export := ""
	if path != "" {
		export = "export PATH=" + shellprefix.Word(path) + " && "
	}
	providerEnv := ""
	if providerEnvDir != "" {
		providerEnv = " --provider-env-dir " + shellprefix.Word(providerEnvDir)
	}
	return marker + export + "cd " + shellprefix.Word(workspace) + " && " + shellprefix.Word(legion) +
		" worker-shim --connect " + shellprefix.Word(streamAddress) +
		" --boot-token-file " + shellprefix.Word(bootTokenFile) + providerEnv +
		" -- " + inner
}
