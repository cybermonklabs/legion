package daemon

import (
	"fmt"
	"os"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// launchSecret is one secret every launch's spec carries (specs.SpawnSpec), by its name, and how
// the daemon reads it.
type launchSecret struct {
	name string
	read func() (string, error)
}

// launchSecrets are the secrets every launch's spec carries, in name order: the Envoy bearer, when
// the daemon has one; the deployment's private-name rule file (leakguard.RulesSecret), when the
// configuration or the daemon's environment names one (config.Config.PrivateNameRulesFile), as
// its bytes verbatim, which every role's leak guard runs behind the secret's `_FILE` pointer,
// leakguard.RulesFileVar; and the pane NATS nkey seed, when the configuration or the daemon's
// environment (lookup) names one (natsauth.Seed). The rule file is read here and nowhere else:
// readBoot compiles these same bytes, so the rules a role runs are the rules boot accepted, and a
// file named or changed later reaches no launch before the daemon restarts. The daemon's own NATS
// connection authenticates with the pane seed only when the daemon has no seed of its own
// (natsauth.DaemonSeed, chooseNATSConnection), which no launch carries. Which of them there are is
// known from the configuration and the environment alone, so checkOperatorConfig names them
// without reading a file.
func launchSecrets(cfg config.Config, lookup func(string) (string, bool)) []launchSecret {
	var secrets []launchSecret
	if cfg.EnvoyTokenFile != "" {
		secrets = append(secrets, launchSecret{"ENVOY_TOKEN", func() (string, error) {
			return config.ReadSecretPointer("envoy_token_file", cfg.EnvoyTokenFile)
		}})
	}
	if cfg.PrivateNameRulesFile != "" {
		secrets = append(secrets, launchSecret{leakguard.RulesSecret, func() (string, error) {
			return readPrivateNameRulesFile(cfg.PrivateNameRulesNamedBy, cfg.PrivateNameRulesFile)
		}})
	}
	if natsauth.Configured(cfg.NatsNkeySeedFile, lookup) {
		secrets = append(secrets, launchSecret{natsauth.SeedVariable, func() (string, error) {
			return natsauth.Seed(cfg.NatsNkeySeedFile, lookup)
		}})
	}
	return secrets
}

// readLaunchSecrets reads every launch secret (launchSecrets), by name, refusing the first that
// cannot be read (readBoot).
func readLaunchSecrets(cfg config.Config, lookup func(string) (string, bool)) (map[string]string, error) {
	secrets := map[string]string{}
	for _, secret := range launchSecrets(cfg, lookup) {
		value, err := secret.read()
		if err != nil {
			return nil, err
		}
		secrets[secret.name] = value
	}
	return secrets, nil
}

// launchSecretNames are the names of the secrets every launch's spec carries (launchSecrets), which
// a provider key and the operator's pod may not collide with.
func launchSecretNames(cfg config.Config, lookup func(string) (string, bool)) []string {
	var names []string
	for _, secret := range launchSecrets(cfg, lookup) {
		names = append(names, secret.name)
	}
	return names
}

// checkOperatorConfig refuses, over the configuration and the daemon's environment (lookup), what
// the operator configured that collides with Legion's own, before anything is opened (readBoot). On
// either runtime, a provider key may not name a launch secret, which every launch hands the agent
// behind its `<NAME>_FILE` pointer alone (a tmux pane would refuse every launch; a pod's shim skips
// the key). Under runtime: kubernetes, the operator's pod and provider keys are then held to the
// Sandbox runtime's own refusals (sandbox.CheckPod), the exchange identity's names among them only
// where runtime.kubernetes.agent_secrets names an exchange_audience.
func checkOperatorConfig(cfg config.Config, lookup func(string) (string, bool)) error {
	names := launchSecretNames(cfg, lookup)
	for _, key := range cfg.ProviderKeys {
		if slices.Contains(names, key.Env) {
			return fmt.Errorf("provider_keys names %s, the launch secret every launch carries behind its %s_FILE pointer: a provider key may not name a launch secret", key.Env, key.Env)
		}
	}
	if cfg.Runtime.Kubernetes == nil {
		return nil
	}
	broker := cfg.Runtime.Kubernetes.AgentSecrets
	return sandbox.CheckPod(sandbox.Pod(cfg.Runtime.Kubernetes.Pod), providerSecretKeys(cfg.ProviderKeys),
		workerImageTools, names, providersSecrets(cfg, lookup), cfg.Runtime.Kubernetes.SessionStore, broker != nil && broker.ExchangeAudience != "")
}

// environLookup is os.LookupEnv over environ, the daemon's environment or a test's.
func environLookup(environ []string) func(string) (string, bool) {
	return func(name string) (string, bool) { return envValue(environ, name) }
}

// environment is o.environ, or the process's when a test replaced none.
func (o overrides) environment() []string {
	if o.environ == nil {
		return os.Environ()
	}
	return o.environ
}
