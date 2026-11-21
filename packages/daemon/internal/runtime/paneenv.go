package runtime

import (
	"regexp"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
)

// sessionPlacing are the variables that decide where Oh My Pi keeps the session a resume reads:
// its config root and agent directory, under which a file session lives (PI_CONFIG_DIR,
// PI_CODING_AGENT_DIR, and OMP_CONFIG_ROOT, which from the pinned Oh My Pi on replaces the config
// root the agent directory and file sessions live under), its session store (OMP_SESSION_STORAGE),
// and the session database's URL, by pointer or by value (OMP_SESSION_SQL_DSN_FILE,
// OMP_SESSION_SQL_DSN). The Sandbox runtime refuses the same names in an operator's pod, from
// ompdirs', ompsessions' and podsafety's constants (sandbox.CheckPod), and
// TestPaneVariableRefusalCoversTheSandboxRuntimesSessionNames holds this list to those constants:
// ompsessions imports this package, so its names are spelled out here.
var sessionPlacing = []string{ompdirs.ConfigRootVariable, "OMP_SESSION_SQL_DSN", "OMP_SESSION_SQL_DSN_FILE", "OMP_SESSION_STORAGE", "PI_CODING_AGENT_DIR", "PI_CONFIG_DIR"}

// ClaudeConfigDirVariable is the variable a Claude Code worker's own configuration directory is
// named in, under either runtime (claudelaunch.Environment): the claim's session transcripts, MCP
// configuration and bridge state (tmux.ClaudeConfigDir).
const ClaudeConfigDirVariable = "CLAUDE_CONFIG_DIR"

// ClaudeDisabledFeatures are the variables every Claude Code worker carries set to 1, under either
// runtime (claudelaunch.Environment), to turn off what would start a turn no task of the daemon's
// asked for and nothing would read: scheduled prompts. Background tasks are on, and a task that
// finishes while no turn runs wakes Claude Code into a turn of its own, which the bridge tells the
// daemon of like any other (internal/claudebridge).
var ClaudeDisabledFeatures = []string{"CLAUDE_CODE_DISABLE_CRON"}

// SessionExclusiveVariable is the variable that holds a resumed Oh My Pi to the session it was
// told to resume. From the pinned Oh My Pi on, a session another live process still holds moves
// the resuming process onto a new session id, which the daemon never recorded: under the SQL
// store, which holds a lease per session, the resume first waits up to 35 s for the holder's lease;
// under the file store a tmux pane keeps, it moves at its first write. Set to
// SessionExclusiveValue, nothing moves: that resume ends the process non-zero, and so do a lease
// lost and a session file another writer replaced. Every runtime sets it on each launch that
// resumes a recorded Oh My Pi session, after the operator's variables, and on no other launch: the
// tmux runtime as a resumed pane's -e pair (tmux's ompLauncher), the Sandbox runtime in the resumed
// generation's start command (sandbox's mainEnvironment).
const SessionExclusiveVariable = "OMP_SESSION_EXCLUSIVE"

// SessionExclusiveValue is the one value of SessionExclusiveVariable Oh My Pi reads as on
// (`process.env.OMP_SESSION_EXCLUSIVE === "1"`, session-manager.ts at the pinned tag), which every
// runtime sets.
const SessionExclusiveValue = "1"

// launchSecrets are every secret the daemon hands a launch in its spec (daemon.launchSecrets: the
// Envoy bearer, the deployment's private-name rule file and the pane NATS nkey seed, each when the
// daemon has one), which a runtime gives the agent as a file of the launch's own behind a
// `<NAME>_FILE` pointer it sets per launch. Each is refused here whether or not this daemon has
// it: a pane_env value would otherwise stand in for the secret of a daemon that named none, and
// be refused at every launch the day the daemon names one. LEGION_PRIVATE_NAME_RULES is
// leakguard.RulesSecret, written out so that this package does not import the leak guard and the
// rule engine behind it for one name; internal/daemon's
// TestEveryLaunchSecretsPointerIsRefusedAsAPaneVariable holds the two spellings together.
var launchSecrets = []string{"ENVOY_TOKEN", "LEGION_PRIVATE_NAME_RULES", "NATS_NKEY_SEED"}

// urlPassword is a URL whose userinfo carries a password, `scheme://user:password@`, anywhere in
// a value: a database URL, a NATS URL with credentials, one of a comma-separated list of either.
var urlPassword = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/?#@:]*:[^\s/?#@]+@`)

// PaneVariableRefusal is why the operator may not add name, set to value, to every pane of the tmux
// runtime (`runtime.tmux.pane_env`), or "" when nothing here refuses it. The loader asks it at load
// (config) and the tmux runtime again at boot (tmux.New), each beside its own checks of what only
// it can see. The tmux runtime also asks it, with no value, of each provider key's name at boot
// (tmux's readProviderKeyNames), since the shim exports a provider key into Oh My Pi's
// environment. A variable that places the session a resume reads is refused, since the runtime
// places a pane's sessions itself and a resume must find the session where the agent left it; so
// is one the runtime sets after the operator's variables on every Claude Code pane, or on every
// resumed Oh My Pi pane (SessionExclusiveVariable), where the operator's value would be overridden
// without a word, and a launch secret's pointer, which every launch sets to a file of its own and
// the runtime then refuses at every launch. A value that is a URL carrying a password is refused
// too: a pane variable is a value on tmux's command line, which every process on the host can
// read, and a secret reaches a pane as a file. The reason names the variable's role and never the
// value.
func PaneVariableRefusal(name, value string) string {
	switch {
	case slices.Contains(sessionPlacing, name):
		return "it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"
	case name == ClaudeConfigDirVariable || slices.Contains(ClaudeDisabledFeatures, name):
		return "the runtime sets it on every Claude Code pane itself, after the operator's variables"
	case name == SessionExclusiveVariable:
		return "the runtime sets it on every resumed Oh My Pi pane itself, after the operator's variables"
	}
	for _, secret := range launchSecrets {
		if name == secret+"_FILE" {
			return "it points at the launch secret " + secret + ", which the runtime hands every launch as a file of the launch's own"
		}
	}
	if urlPassword.MatchString(value) {
		return "its value is a URL carrying a password: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line"
	}
	return ""
}
