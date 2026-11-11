package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// under the state directory (config.WriteVariantInstructions), and a launch's prompt ends with
// the copies of the arms its unit was drawn into.
type variantInstructions struct {
	experiment, variant string
	contents            []byte
}

// readBoot is every refusal boot makes from the configuration, the daemon's environment (lookup,
// and getenv for the OMP invocation) and the files they name, writing nothing: the project token,
// the operator bearer's file, operator configuration colliding with Legion's own
// (checkOperatorConfig), the launch secrets (readLaunchSecrets), the daemon's own NATS nkey seed
// (natsauth.DaemonSeed), the instructions file, every declared variant's instructions file, the
// file of every hook that reads its words from one (`hooks[].prompt_file`), the Dispatch bearer's
// file, and the runtime's own reads (readTmux, readSandbox).
// hostOMP is whether boot runs the host's Oh My Pi, false only for a test that replaced the
// runtime. prepare runs it first; CheckStart is it for `legion start --check-config`, so the check
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
	if r.nats, err = chooseNATSConnection(daemonSeed, paneSeed, r.paneNatsUser); err != nil {
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
	// missing one refuses boot, and `legion start --check-config` with it.
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
	// (config.Project.ExperimentsDeclared, which maps the `memory` block to the experiment named
	// memory). No mode decides it: a unit already drawn into an arm is given that arm's
	// instructions whatever the mode is now, and only removing the key takes them away.
	for _, experiment := range cfg.Projects[cfg.Project].ExperimentsDeclared() {
		for _, variant := range experiment.Variants {
			if variant.InstructionsPath == "" {
				continue
			}
			contents, err := config.ReadVariantInstructions(experiment.Name, variant.Name, variant.InstructionsPath)
			if err != nil {
				return bootReads{}, err
			}
			r.variantInstructions = append(r.variantInstructions, variantInstructions{experiment: experiment.Name, variant: variant.Name, contents: contents})
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

// natsConnection is the NATS user the daemon's own connection authenticates as: its own seed
// (natsauth.DaemonSeed) when it has one, else the pane seed every pane receives (launchSecrets),
// else no credential. No launch carries it.
type natsConnection struct {
	seed     string         // "" for no credential
	user     string         // seed's user's public key, "" with no seed
	source   natsSeedSource // where seed came from
	paneUser bool           // whether user is the pane seed's user, as when the daemon seed is the pane seed
}

// natsSeedSource is where the daemon's own connection's seed came from, as its boot line names it.
type natsSeedSource string

const (
	natsSeedDaemon natsSeedSource = "daemon" // natsauth.DaemonSeed
	natsSeedPane   natsSeedSource = "pane"   // the pane seed every pane receives
	natsSeedNone   natsSeedSource = "none"   // no seed: no credential
)

// chooseNATSConnection is the connection over daemonSeed and paneSeed, each "" for none, where
// paneUser is paneSeed's user's public key, already read.
func chooseNATSConnection(daemonSeed, paneSeed, paneUser string) (natsConnection, error) {
	switch {
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

// log logs, once, the user the connection authenticates as, by public key, whether that is the pane
// user, and where its seed came from; never the seed.
func (c natsConnection) log(log *slog.Logger) {
	log.Info("legion daemon connects to NATS", "user", c.user, "paneUser", c.paneUser, "seed", string(c.source))
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
// and only taking the arm out of the file takes them away.
func checkRoleConfigFiles(cfg config.Config) error {
	for _, role := range config.RoleConfigRoles {
		for _, file := range cfg.RoleConfigFiles[role] {
			if err := podsafety.CheckOverlay(file); err != nil {
				return fmt.Errorf("role_config_files.%s names %s, which the %s's Oh My Pi could not read: %w", role, file, role, err)
			}
		}
	}
	block := memoryBlockWritten(cfg)
	for _, variant := range declaredVariants(cfg) {
		field := declaredOverlayField(cfg.Project, block, variant)
		for _, file := range variant.ConfigFiles {
			if err := podsafety.CheckOverlay(file); err != nil {
				return fmt.Errorf("%s names %s, which a role drawn into that arm could not read: %w", field, file, err)
			}
		}
	}
	return nil
}

// CheckStart is `legion start --check-config`'s reading of what boot reads before it writes
// anything (readBoot), over the daemon's environment (lookup, and the process's for the OMP
// invocation): nil when boot would get past every such refusal, and the public keys of the pane
// seed's user and the daemon seed's user, each "" when the daemon has no such seed, never a seed.
func CheckStart(cfg config.Config, lookup func(string) (string, bool)) (paneNatsUser, daemonNatsUser string, err error) {
	r, err := readBoot(cfg, lookup, os.Getenv, true, slog.New(slog.DiscardHandler))
	if err != nil {
		return "", "", err
	}
	if r.nats.source == natsSeedDaemon {
		daemonNatsUser = r.nats.user
	}
	return r.paneNatsUser, daemonNatsUser, nil
}
