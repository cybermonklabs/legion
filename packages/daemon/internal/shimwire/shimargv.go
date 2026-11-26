package shimwire

// The names of `legion worker-shim`'s command line that a process other than the shim writes or
// reads. The daemon writes AgentSecretsBinFlag into the shim's argv (internal/runtime/sandbox's
// launcher command, LauncherStart.Argv). The controller pod's launcher adds ReapOrphansFlag to the
// shim's argv (internal/launcher's reapingShim), and its memory guard tells the shim's own secrets
// keygen and renewer from the orphans the shim adopts by the executable AgentSecretsBinFlag names,
// run as AgentSecretsKeygen or AgentSecretsRenew (internal/launcher's shimOwnChildren). The shim
// declares its flags and runs those two tasks by these same constants (cmd/legion's
// worker_shim.go, internal/shim's agentsecrets.go and config.go), so a rename on one side is a
// rename on the other.
const (
	// ReapOrphansFlag is the flag, without its dashes, that makes the shim the child subreaper of
	// everything it starts (internal/shim's Config.ReapOrphans).
	ReapOrphansFlag = "reap-orphans"
	// AgentSecretsBinFlag is the flag, without its dashes, that names the agent-secrets executable.
	AgentSecretsBinFlag = "agent-secrets-bin"
	// AgentSecretsKeygen and AgentSecretsRenew are the agent-secrets tasks the shim runs as its own
	// children: the key's generation and the lease's renewer.
	AgentSecretsKeygen = "keygen"
	AgentSecretsRenew  = "renew"
)
