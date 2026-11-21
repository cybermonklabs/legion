package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// bootReads is what boot reads from the configuration, the daemon's environment and the files they
// name before it writes anything (readBoot).
type bootReads struct {
	project, operatorToken, dispatchToken string
	secrets                               map[string]string
	paneNatsUser                          string         // the pane seed's user's public key, "" with no pane seed
	nats                                  natsConnection // the user the daemon's own NATS connection authenticates as
	instructions                          []byte         // nil when the configuration names none
	variantInstructions                   []variantInstructions
	hookPrompts                           map[string]string // the words of each hook whose prompt_file names a file, by hook name; nil while no hook names one
	tmux                                  tmuxReads         // runtime: tmux
	sandbox                               sandboxReads      // runtime: kubernetes
}

// variantInstructions is one declared variant's instructions file as boot read it: the experiment
// and the arm that name it, and the operator's file verbatim. Boot writes one copy per entry
// under the state directory (config.WriteVariantInstructions), named and headed as the arm's
// declaration says (config.Variant.MemoryBlock), and a launch's prompt ends with the copies of the
// arms its unit was drawn into.
type variantInstructions struct {
	experiment string
	variant    config.Variant
	contents   []byte
}

// readBoot is every refusal boot makes from the configuration, the daemon's environment (lookup,
// and getenv for the OMP invocation) and the files they name, writing nothing: the project token,
// the operator bearer's file, operator configuration colliding with Legion's own
// (checkOperatorConfig), the launch secrets (readLaunchSecrets), the daemon's own NATS nkey seed
// (natsauth.DaemonSeed), the instructions file, every declared variant's instructions file, the
// file of every hook that reads its words from one (`hooks[].prompt_file`), the Dispatch bearer's
// file, and the runtime's own reads (readTmux, readSandbox).
// hostOMP is whether boot runs the host's Oh My Pi, false only for a test that replaced the
// runtime. prepare runs it first; CheckStart is it for `legion config check`, so the check
// refuses whatever boot refuses before its first write. What boot does after it is not a check's to
// do: create the state directory, write the instructions copies and the Dispatch token file, read
// provider keys from secretsd, and run the plugin gate or the image probe.
func readBoot(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool, log *slog.Logger) (bootReads, error) {
	var r bootReads
	var err error
	if r.project, err = claim.ProjectToken(cfg.Project); err != nil {
		return bootReads{}, err
	}
	if cfg.OperatorTokenFile == "" {
		return bootReads{}, errors.New("operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names")
	}
	if r.operatorToken, err = config.ReadSecretPointer("operator_token_file", cfg.OperatorTokenFile); err != nil {
		return bootReads{}, err
	}
	if err := checkOperatorConfig(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	if r.secrets, err = readLaunchSecrets(cfg, lookup); err != nil {
		return bootReads{}, err
	}
	paneSeed := r.secrets[natsauth.SeedVariable]
	if paneSeed != "" {
		if r.paneNatsUser, err = natsauth.PublicKey(paneSeed); err != nil {
			return bootReads{}, err
		}
	}
	daemonSeed, err := natsauth.DaemonSeed(cfg.NatsDaemonNkeySeedFile, lookup)
	if err != nil {
		return bootReads{}, err
	}
	if r.nats, err = chooseNATSConnection(cfg.NatsDaemonServiceTokenFile, daemonSeed, paneSeed, r.paneNatsUser); err != nil {
		return bootReads{}, err
	}
	if cfg.InstructionsPath != "" {
		if r.instructions, err = config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return bootReads{}, err
		}
	}
	// Every hook that reads its words from a file rather than writing them in the key, read as
	// the deployment's instructions are and refused the same way. The engine is given words and
	// never a path, so a file a hook names is as much a part of boot as the instructions are: a
	// missing one refuses boot, and `legion config check` with it.
	for _, hook := range cfg.Hooks {
		if hook.PromptFile == "" {
			continue
		}
		words, err := config.ReadHookPrompt(hook.Name, hook.PromptFile)
		if err != nil {
			return bootReads{}, err
		}
		if r.hookPrompts == nil {
			r.hookPrompts = map[string]string{}
		}
		r.hookPrompts[hook.Name] = words
	}
	// Every declared variant's instructions, read as the deployment's are and refused the same
	// way, in the order the project's file declares the experiments
	// (config.Config.ExperimentsDeclared, the list that maps the `memory` block in as the
	// experiment named memory). No mode decides it: a unit already drawn into an arm is given
	// that arm's instructions whatever the mode is now, and only removing the key takes them away.
	// The block's own arm (config.Variant.MemoryBlock) is refused in the block's own words
	// (config.ReadMemoryInstructions), which the documentation quotes, and every arm of the
	// `experiments` key, `experiments.memory` included, in the experiments' words
	// (config.ReadVariantInstructions).
	for _, experiment := range cfg.ExperimentsDeclared() {
		for _, variant := range experiment.Variants {
			if variant.InstructionsPath == "" {
				continue
			}
			var contents []byte
			if variant.MemoryBlock {
				contents, err = config.ReadMemoryInstructions(variant.InstructionsPath)
			} else {
				contents, err = config.ReadVariantInstructions(experiment.Name, variant.Name, variant.InstructionsPath)
			}
			if err != nil {
				return bootReads{}, err
			}
			r.variantInstructions = append(r.variantInstructions, variantInstructions{experiment: experiment.Name, variant: variant, contents: contents})
		}
	}
	if cfg.DispatchURL != "" {
		if r.dispatchToken, err = config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return bootReads{}, err
		}
	}
	switch cfg.Runtime.Name {
	case "tmux":
		r.tmux, err = readTmux(cfg, lookup, getenv, hostOMP)
	case "kubernetes":
		r.sandbox, err = readSandbox(cfg, r.project, r.dispatchToken, r.paneNatsUser, lookup, log)
	default:
		err = fmt.Errorf("runtime %q is neither tmux nor kubernetes", cfg.Runtime.Name)
	}
	if err != nil {
		return bootReads{}, err
	}
	return r, nil
}

// natsConnection is the credential the daemon's own NATS connection presents: its platform identity
// token (config's NatsDaemonServiceTokenFile) when it names one, else its own seed
// (natsauth.DaemonSeed) when it has one, else the pane seed every pane receives (launchSecrets),
// else no credential. No launch carries it.
type natsConnection struct {
	seed      string         // "" for no seed
	tokenFile string         // the platform identity token's file, "" for none
	user      string         // seed's user's public key, or the account the token names; "" with neither
	source    natsSeedSource // where the credential came from
	paneUser  bool           // whether user is the pane seed's user, as when the daemon seed is the pane seed
}

// natsSeedSource is where the daemon's own connection's credential came from, as its boot line
// names it.
type natsSeedSource string

const (
	natsServiceToken natsSeedSource = "service-token" // nats_daemon_service_token_file
	natsSeedDaemon   natsSeedSource = "daemon"        // natsauth.DaemonSeed
	natsSeedPane     natsSeedSource = "pane"          // the pane seed every pane receives
	natsSeedNone     natsSeedSource = "none"          // no seed: no credential
)

// chooseNATSConnection is the connection over tokenFile, daemonSeed and paneSeed, each "" for none,
// where paneUser is paneSeed's user's public key, already read. A token file and a daemon seed
// together are refused naming both, since one connection presents one credential; the token file
// is read here (natsauth.ServiceIdentity), so a missing or unreadable token refuses boot.
func chooseNATSConnection(tokenFile, daemonSeed, paneSeed, paneUser string) (natsConnection, error) {
	switch {
	case tokenFile != "" && daemonSeed != "":
		return natsConnection{}, fmt.Errorf("%s and a daemon seed (%s, %s or %s) are both set: the daemon's own NATS connection presents one credential, so name one of them",
			natsauth.ServiceTokenFileKey, natsauth.DaemonSeedFileKey, natsauth.DaemonSeedFileVariable, natsauth.DaemonSeedVariable)
	case tokenFile != "":
		account, err := natsauth.ServiceIdentity(tokenFile)
		if err != nil {
			return natsConnection{}, err
		}
		return natsConnection{tokenFile: tokenFile, user: account, source: natsServiceToken}, nil
	case daemonSeed != "":
		user, err := natsauth.PublicKey(daemonSeed)
		if err != nil {
			return natsConnection{}, err
		}
		return natsConnection{seed: daemonSeed, user: user, source: natsSeedDaemon, paneUser: user == paneUser}, nil
	case paneSeed != "":
		return natsConnection{seed: paneSeed, user: paneUser, source: natsSeedPane, paneUser: true}, nil
	default:
		return natsConnection{source: natsSeedNone}, nil
	}
}

// log logs, once, who the connection authenticates as, never the credential: for a seed, the user
// by public key, whether that is the pane user, and where its seed came from; for the service
// token, the account the token names, and seed=service-token.
func (c natsConnection) log(log *slog.Logger) {
	log.Info("legion daemon connects to NATS", "user", c.user, "paneUser", c.paneUser, "seed", string(c.source))
}

// waitsOut reports whether the boot waits a failed connect on this connection out, retrying it on
// readinessRetry and logging each failure with its reason (bootprobe.Run), rather than refusing
// the boot: an unreachable NATS for every credential (natsauth.Unreachable), and, for the service
// token alone, a refusal (natsauth.Refused). The NATS auth service refuses a token while it is
// away or holds no answer a decision needs yet, so the token's connection waits that out at boot as
// it does while running (natsauth.ConnectService, and options' reconnect delay), and the daemon,
// which serves nothing while its boot runs, reads as not ready meanwhile rather than exiting into a
// restart. A seed's refusal, or a refusal of no credential, is a misconfiguration no wait fixes,
// and refuses the boot as before.
func (c natsConnection) waitsOut(err error) bool {
	return natsauth.Unreachable(err) || c.tokenFile != "" && natsauth.Refused(err)
}

// options are the connection options the daemon's own connection dials with, after its
// credential: what the server reports, logged (natsauth.LogEvents), and reconnects that never give
// up (natsauth.ReconnectForever). The service token's connection also waits its reconnects out on
// the boot's own schedule, readinessRetry, which waitsOut holds the boot's connect to: one rule,
// bootprobe.Retry.Delay, waits a refusal of the auth service out at boot and while running. nats.go
// counts a reconnect's attempts from one, and Delay its retries from none. A seed's keeps nats.go's
// own reconnect wait.
func (c natsConnection) options(log *slog.Logger) []nats.Option {
	options := []nats.Option{natsauth.LogEvents(log), natsauth.ReconnectForever()}
	if c.tokenFile != "" {
		options = append(options, nats.CustomReconnectDelay(func(attempts int) time.Duration { return readinessRetry.Delay(attempts - 1) }))
	}
	return options
}

// tmuxReads is what a daemon on this host needs that readBoot reads: a launch for each harness its
// roles run on (resolveLaunchers), and the host's git and jj its own provisioning runs
// (daemonTools), which no pane is told.
type tmuxReads struct {
	launches map[runtime.Backend]launchReads // Oh My Pi's absent when boot does not run the host's Oh My Pi
	tools    map[string]string               // nil without a repository
}

// readTmux is tmux's share of readBoot: each backend's launch (resolveLaunchers), Oh My Pi's only
// when boot runs the host's Oh My Pi (hostOMP), every overlay role_config_files names, which must
// be a file a pane can read (checkRoleConfigFiles), and, for a configuration with a repository, the
// host's git and jj Legion runs itself.
func readTmux(cfg config.Config, lookup func(string) (string, bool), getenv func(string) string, hostOMP bool) (tmuxReads, error) {
	var r tmuxReads
	var err error
	if r.launches, err = resolveLaunchers(cfg, lookup, getenv, hostOMP); err != nil {
		return tmuxReads{}, err
	}
	if err := checkRoleConfigFiles(cfg); err != nil {
		return tmuxReads{}, err
	}
	if _, ok := cfg.Projects[cfg.Project]; ok {
		if r.tools, err = resolveTools(lookup); err != nil {
			return tmuxReads{}, err
		}
	}
	return r, nil
}

// checkRoleConfigFiles refuses an overlay of role_config_files, or of the project's `memory`
// block, that is not a file a pane that names it could read, naming the key and the file: a
// pane's Oh My Pi refuses to start on an overlay it cannot read, and every pane of the daemon
// runs on this host as this daemon's user. Under kubernetes the files are the pods' mounts',
// which only the image probe reads. Every declared arm's overlays are read whatever its
// experiment's mode is: a unit already drawn into an arm names that arm's files under any mode,
// and only taking the arm out of the file takes them away. The block's own arm
// (config.Variant.MemoryBlock) is refused in the block's own words, which the documentation
// quotes, and every arm of the `experiments` key, `experiments.memory` included, in the
// experiments' words.
func checkRoleConfigFiles(cfg config.Config) error {
	for _, role := range config.RoleConfigRoles {
		for _, file := range cfg.RoleConfigFiles[role] {
			if err := podsafety.CheckOverlay(file); err != nil {
				return fmt.Errorf("role_config_files.%s names %s, which the %s's Oh My Pi could not read: %w", role, file, role, err)
			}
		}
	}
	for _, declared := range cfg.ExperimentsDeclared() {
		for _, variant := range declared.Variants {
			reader := "a role drawn into that arm could not read"
			if variant.MemoryBlock {
				reader = "a memory-group role's Oh My Pi could not read"
			}
			for _, file := range variant.ConfigFiles {
				if err := podsafety.CheckOverlay(file); err != nil {
					return fmt.Errorf("%s names %s, which %s: %w", variant.ConfigFilesField, file, reader, err)
				}
			}
		}
	}
	return nil
}

// CheckedNATS is who the daemon's NATS credentials authenticate as, as CheckStart read them: the
// pane seed's user and the daemon seed's user by public key, and the account the daemon's service
// token names (nats_daemon_service_token_file), each "" when the daemon has no such credential;
// never a seed or a token.
type CheckedNATS struct {
	PaneUser, DaemonUser, DaemonServiceAccount string
}

// CheckStart is `legion config check`'s reading of what boot reads before it writes
// anything (readBoot), over the daemon's environment (lookup, and the process's for the OMP
// invocation), then of postgres_dsn as the store reads it before it opens a connection
// (store.PoolConfig, which parses the DSN and opens nothing): nil when boot would get past every
// such refusal, and who the daemon's NATS credentials authenticate as (CheckedNATS). The store
// refuses a postgres_dsn pgx cannot parse, such as a malformed URL, a bad pool_* key or an
// sslrootcert, sslcert or sslkey file it cannot read, and one whose default_query_exec_mode the
// store's text rule cannot run in. pgx fills what the DSN leaves unset from the process's own PG*
// environment variables, whatever lookup holds, as it does at boot. Each such refusal is
// store.ConnectError, as store.Open wraps it: the DSN's address and the driver's detail, never the
// DSN's password. The driver's detail of a DSN pgx cannot parse quotes the DSN with its user name
// and its query, which `legion config check` and `legion start` both remove when they write the
// refusal (printRefusal, the URL rule).
func CheckStart(cfg config.Config, lookup func(string) (string, bool)) (CheckedNATS, error) {
	r, err := readBoot(cfg, lookup, os.Getenv, true, slog.New(slog.DiscardHandler))
	if err != nil {
		return CheckedNATS{}, err
	}
	if _, err := store.PoolConfig(cfg.PostgresDSN); err != nil {
		return CheckedNATS{}, store.ConnectError(cfg.PostgresDSN, err)
	}
	checked := CheckedNATS{PaneUser: r.paneNatsUser}
	switch r.nats.source {
	case natsSeedDaemon:
		checked.DaemonUser = r.nats.user
	case natsServiceToken:
		checked.DaemonServiceAccount = r.nats.user
	}
	return checked, nil
}
