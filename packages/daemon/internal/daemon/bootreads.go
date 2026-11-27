package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/sjawhar/envoy/pgdsn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// bootReads is what boot reads from the configuration, the daemon's environment and the files they
// name before it writes anything (readBoot).
type bootReads struct {
	project, operatorToken, dispatchToken string
	secrets                               map[string]string
	paneNatsUser                          string             // the pane seed's user's public key, "" with no pane seed
	nats                                  natsConnection     // the user the daemon's own NATS connection authenticates as
	privateNameRules                      *leakguard.RuleSet // compiled from the launch secret leakguard.RulesSecret; nil when the deployment names no rule file
	instructions                          []byte             // nil when the configuration names none
	variantInstructions                   []variantInstructions
	hookPrompts                           map[string]string // the words of each hook whose prompt_file names a file, by hook name; nil while no hook names one
	tmux                                  tmuxReads         // runtime: tmux
	sandbox                               sandboxReads      // runtime: kubernetes
	answers                               answersReads      // with the answer loop on, under either runtime
}

// variantInstructions is one declared variant's instructions file as boot read it: the experiment
// and the arm that name it, and the operator's file verbatim. Boot writes one copy per entry
// under the state directory (writeInstructionsCopies), named and headed as the arm's declaration
// says (config.Variant.MemoryBlock), each launch reads the file again into that copy
// (instructionsCopies.refresh), and a launch's prompt ends with the copies of the arms its unit was
// drawn into.
type variantInstructions struct {
	experiment string
	variant    config.Variant
	contents   []byte
}

// readBoot is every refusal boot makes from the configuration, the daemon's environment (lookup,
// and getenv for the OMP invocation) and the files they name, writing nothing: the project token,
// the operator bearer's file, operator configuration colliding with Legion's own
// (checkOperatorConfig), the launch secrets (readLaunchSecrets), the deployment's private-name
// rule file among them, which it compiles (compilePrivateNameRules), the daemon's own NATS nkey
// seed (natsauth.DaemonSeed), the instructions file, every declared variant's instructions file,
// the file of every hook that reads its words from one (`hooks[].prompt_file`), the Dispatch
// bearer's file, or under dispatch_token_broker the transition token's file where the
// configuration names one, the runtime's own reads (readTmux, readSandbox), and, with the answer
// loop on, the answer loop's (readAnswersBoot).
// hostOMP is whether boot runs the host's Oh My Pi, false only for a test that replaced the
// runtime. prepare runs it first; CheckStart is it for `legion config check`, so the check refuses
// whatever boot refuses before its first write, with one exception: under dispatch_token_broker,
// the identity variables boot trades with and its first trade at the secrets broker are boot's
// alone (dispatchCredential), so a check reads the file and needs no identity
// (dispatch://INFRA-4425 R1). What boot does after it is not a check's to do: create the state
// directory, write the instructions copies and the Dispatch token file, read provider keys from
// secretsd, and run the plugin gate or the image probe.
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
	// The rule file is compiled from the bytes every launch carries, read once (launchSecrets), so
	// the rules the daemon's branch create runs and the rules each role runs are one file.
	if content, named := r.secrets[leakguard.RulesSecret]; named {
		if r.privateNameRules, err = compilePrivateNameRules(cfg.PrivateNameRulesNamedBy, cfg.PrivateNameRulesFile, content); err != nil {
			return bootReads{}, err
		}
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
	for _, experiment := range cfg.ExperimentsDeclared() {
		for _, variant := range experiment.Variants {
			if variant.InstructionsPath == "" {
				continue
			}
			contents, err := readArmInstructions(experiment.Name, variant)
			if err != nil {
				return bootReads{}, err
			}
			r.variantInstructions = append(r.variantInstructions, variantInstructions{experiment: experiment.Name, variant: variant, contents: contents})
		}
	}
	// The loader takes dispatch_token_broker only beside dispatch_url, and the transition file only
	// beside the broker, so the broker reads no bearer file. A launch's Dispatch credential is the
	// bearer, or under the broker the role's own trade and the shared token
	// dispatch_token_transition_file holds ("" with no such file), which a launch into a pod built
	// before the setting carries and the daemon's own calls never present.
	credential := sandbox.DispatchCredential{Broker: cfg.DispatchTokenBroker}
	switch {
	case cfg.DispatchTokenBroker && cfg.DispatchTokenTransitionFile != "":
		if credential.Transition, err = config.ReadSecretPointer("dispatch_token_transition_file", cfg.DispatchTokenTransitionFile); err != nil {
			return bootReads{}, err
		}
	case cfg.DispatchURL != "" && !cfg.DispatchTokenBroker:
		if r.dispatchToken, err = config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return bootReads{}, err
		}
		credential.Bearer = r.dispatchToken
	}
	switch cfg.Runtime.Name {
	case "tmux":
		r.tmux, err = readTmux(cfg, lookup, getenv, hostOMP)
	case "kubernetes":
		r.sandbox, err = readSandbox(cfg, r.project, credential, r.paneNatsUser, lookup, log)
	default:
		err = fmt.Errorf("runtime %q is neither tmux nor kubernetes", cfg.Runtime.Name)
	}
	if err != nil {
		return bootReads{}, err
	}
	if cfg.Answers != nil {
		if r.answers, err = readAnswersBoot(*cfg.Answers, cfg.Runtime.Name, cfg.LeakCheck, r.tmux, lookup); err != nil {
			return bootReads{}, err
		}
		// The dispatch tool refuses no boot: a daemon that finds no dispatch command offers none.
		r.answers.dispatch = answersDispatch(lookup, cfg.DispatchURL, cfg.DispatchTokenFile, dispatchInstall)
	}
	return r, nil
}

// answersReads is what the answer loop needs that readBoot reads: gh, the real gh on the daemon's
// own host its gh tool runs, under either runtime (answersGH); where the shared public leak check
// that tool's guard runs comes from (answersLeakCheck): leakCheck, the machine's check a tmux
// daemon resolved, or leakCheckTag, the pin a Kubernetes daemon installs the deployment's setting's
// check at (installAnswersLeakCheck), exactly one of them set; conversationDSN, the URL of the
// database the conversations live in: answers.conversation_store.dsn_file's under
// runtime.kubernetes.session_store postgres, runtime.tmux.session_dsn_file's under
// runtime.tmux.session_store postgres ("" under any other store), the one place the loop gets that
// URL from, whichever key named its file; and dispatch, the dispatch command its dispatch tool
// runs, or why the loop offers none (answersDispatch).
type answersReads struct {
	gh                      string
	leakCheck, leakCheckTag string
	conversationDSN         string
	dispatch                dispatchFound
}

// readAnswersBoot is the answer loop's share of readBoot under runtimeName and the deployment's
// leak check setting (config's leak_check), tmux what readTmux read (the zero value under
// kubernetes): its tools, the tmux daemon's own (nil under tmux with no repository), and its
// session database's URL, which under runtime.tmux.session_store postgres is the conversation
// store's (config.ConversationStore.DSNFile names the same file). No provider's key is read: the
// loop reads it for every model call (config.AnswerCredential), so neither boot nor `legion config
// check` runs an api_key_command.
func readAnswersBoot(answers config.Answers, runtimeName string, setting leakguard.Setting, tmux tmuxReads, lookup func(string) (string, bool)) (answersReads, error) {
	var r answersReads
	var err error
	if r.gh, err = answersGH(lookup, tmux.tools); err != nil {
		return answersReads{}, err
	}
	if r.leakCheck, r.leakCheckTag, err = answersLeakCheck(runtimeName, setting, lookup); err != nil {
		return answersReads{}, err
	}
	switch file := answers.ConversationStore.DSNFile; {
	case tmux.sessionDSN != "":
		r.conversationDSN = tmux.sessionDSN
	case file != "":
		if r.conversationDSN, err = readPostgresURL("answers.conversation_store.dsn_file", file); err != nil {
			return answersReads{}, err
		}
	}
	return r, nil
}

// readPostgresURL is the postgres:// URL the file a key names holds, refused, naming the key and
// the file and never what it holds, unless it is one.
func readPostgresURL(key, file string) (string, error) {
	dsn, err := config.ReadSecretPointer(key, file)
	if err != nil {
		return "", err
	}
	if parsed, err := url.Parse(dsn); err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", fmt.Errorf("%s names %s, which holds no postgres:// URL", key, file)
	}
	return dsn, nil
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
// roles run on (resolveLaunchers), the host's git and jj its own provisioning runs (daemonTools),
// which no pane is told, and, under runtime.tmux.session_store postgres, the URL of the session
// database every pane's Oh My Pi keeps its sessions in, which the boot gate probes (verifyPanes).
type tmuxReads struct {
	launches   map[runtime.Backend]launchReads // Oh My Pi's absent when boot does not run the host's Oh My Pi
	tools      map[string]string               // nil without a repository
	sessionDSN string                          // "" under the files store
}

// readTmux is tmux's share of readBoot: each backend's launch (resolveLaunchers), Oh My Pi's only
// when boot runs the host's Oh My Pi (hostOMP), every overlay role_config_files names, which must
// be a file a pane can read (checkRoleConfigFiles), for a configuration with a repository, the
// host's git and jj Legion runs itself, and under runtime.tmux.session_store postgres the session
// database's URL from runtime.tmux.session_dsn_file, which must be a postgres:// URL.
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
	if file := cfg.Runtime.Tmux.SessionDSN(); file != "" {
		if r.sessionDSN, err = readPostgresURL("runtime.tmux.session_dsn_file", file); err != nil {
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

// CheckStart is `legion config check`'s reading of what boot reads before it writes
// anything (readBoot), over the daemon's environment (lookup, and the process's for the OMP
// invocation), then of postgres_dsn as the store reads it before it opens a connection
// (store.PoolConfig, which parses the DSN and opens nothing): nil when boot would get past every
// such refusal, and the public keys of the pane seed's user and the daemon seed's user, each ""
// when the daemon has no such seed, never a seed. The store refuses a postgres_dsn pgx cannot
// parse, such as a malformed URL, a bad pool_* key or an sslrootcert, sslcert or sslkey file it
// cannot read, and one whose default_query_exec_mode the store's text rule cannot run in. pgx
// fills what the DSN leaves unset from the process's own PG* environment variables, whatever
// lookup holds, as it does at boot. Each such refusal is pgdsn.ConnectError, as store.Open wraps
// it: a DSN pgx cannot parse is named by its setting alone, with none of pgx's text, since that
// text quotes the DSN (a keyword DSN's password among it, which pgx masks only when written
// `password=<value>`); any other refusal names the DSN's address and the driver's detail, never
// the DSN's password.
func CheckStart(cfg config.Config, lookup func(string) (string, bool)) (paneNatsUser, daemonNatsUser string, err error) {
	r, err := readBoot(cfg, lookup, os.Getenv, true, slog.New(slog.DiscardHandler))
	if err != nil {
		return "", "", err
	}
	if _, err := store.PoolConfig(cfg.PostgresDSN); err != nil {
		return "", "", pgdsn.ConnectError(store.DSNSetting, cfg.PostgresDSN, err)
	}
	if r.nats.source == natsSeedDaemon {
		daemonNatsUser = r.nats.user
	}
	return r.paneNatsUser, daemonNatsUser, nil
}
