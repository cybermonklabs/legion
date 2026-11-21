package harness

import (
	"flag"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// Where the pod's model access lives where a flag names none.
const (
	// defaultKeyFile is the model gateway's key in a Legion pod.
	defaultKeyFile = "/var/run/model-gateway-key/key"
	// defaultTokenFile is the pod's ServiceAccount token, which the Foundry account's dev
	// identity admits.
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// defaultAPIVersion is the api-version the Foundry chat route takes.
	defaultAPIVersion = "2024-05-01-preview"
	// DefaultConcurrency is the model calls a run has in flight at once.
	DefaultConcurrency = 6
)

// modelFlags are the flags that say how a command reaches the models: the gateway for Claude,
// the Foundry account and its sign-in, the calls in flight, and, for a command that adds it
// (addPaceFlag), the pace of the calls' uncached input.
type modelFlags struct {
	gateway, keyFile                   string
	foundry, apiVersion                string
	tenant, clientID, tokenFile, scope string
	concurrency                        int
	uncachedPerMinute                  int
}

func addModelFlags(fs *flag.FlagSet) *modelFlags {
	m := &modelFlags{}
	fs.StringVar(&m.gateway, "gateway", os.Getenv("ANTHROPIC_BASE_URL"), "the model gateway's base URL, which serves Anthropic Messages (default $ANTHROPIC_BASE_URL)")
	fs.StringVar(&m.keyFile, "gateway-key-file", defaultKeyFile, "the file holding the gateway's key (read on each call, never printed)")
	fs.StringVar(&m.foundry, "foundry-endpoint", "", "the Foundry account's models inference endpoint, its base URL with /models on the end (for a foundry model)")
	fs.StringVar(&m.apiVersion, "foundry-api-version", defaultAPIVersion, "the api-version the Foundry chat route takes")
	fs.StringVar(&m.tenant, "azure-tenant", "", "the Entra tenant id of the Foundry account's keyless sign-in")
	fs.StringVar(&m.clientID, "azure-client-id", "", "the Entra client id the pod's token is exchanged for")
	fs.StringVar(&m.tokenFile, "federated-token-file", defaultTokenFile, "the federated token Entra trusts for the client id")
	fs.StringVar(&m.scope, "scope", embed.DefaultEntraScope, "the Entra token scope")
	fs.IntVar(&m.concurrency, "concurrency", DefaultConcurrency, "the model calls in flight at once")
	return m
}

// addPaceFlag adds --uncached-input-per-minute, the pace of the command's own model calls.
func (m *modelFlags) addPaceFlag(fs *flag.FlagSet) {
	fs.IntVar(&m.uncachedPerMinute, "uncached-input-per-minute", 0, "hold this process's own model calls, retries and fallbacks included, to this many uncached input tokens in any one minute: the prompt tokens not read from the provider's cache, cache writes included (0: no pacing; the calls are still counted)")
}

// runner is the one Runner of a command: every client it builds shares its bound and its pacer.
// The pacer counts the calls' uncached input even where it paces none.
func (m *modelFlags) runner() (*llm.Runner, error) {
	switch {
	case m.concurrency < 1:
		return nil, fmt.Errorf("--concurrency must be at least 1")
	case m.uncachedPerMinute < 0:
		return nil, fmt.Errorf("--uncached-input-per-minute must not be below 0")
	}
	return &llm.Runner{Limit: llm.NewLimit(m.concurrency), Pacer: llm.NewPacer(m.uncachedPerMinute)}, nil
}

// foundryConfig is the Foundry account the flags name.
func (m *modelFlags) foundryConfig() llm.FoundryConfig {
	return llm.FoundryConfig{
		Endpoint: m.foundry, APIVersion: m.apiVersion,
		Entra: embed.EntraConfig{TenantID: m.tenant, ClientID: m.clientID, TokenFile: m.tokenFile, Scope: m.scope},
	}
}

// claudeClient is the client of a Claude model of the config.
func (m *modelFlags) claudeClient(cfg Config, model ModelEntry, runner *llm.Runner) (*claude.Client, error) {
	price, ok := cfg.Prices.Prices[model.ID]
	if !ok {
		return nil, fmt.Errorf("prices.yaml has no price for %q", model.ID)
	}
	return claude.New(m.gateway, m.keyFile, model.ID, model.Effort, price.agentloop(), runner)
}

// foundryTokens is the Entra token source of a Foundry account; a test replaces it with a
// stand-in's, which signs in nowhere.
var foundryTokens = llm.FoundryConfig.Tokens

// foundryClient is the client of a Foundry deployment of the config, at its effort and the price
// of its id.
func (m *modelFlags) foundryClient(cfg Config, model ModelEntry, runner *llm.Runner) (*llm.Foundry, error) {
	account := m.foundryConfig()
	tokens, err := foundryTokens(account)
	if err != nil {
		return nil, err
	}
	return llm.NewFoundry(account, tokens, model.ID, model.Effort, cfg.Prices.Prices[model.ID].agentloop(), runner), nil
}

// grader is the grader of the config, with its fallbacks, reading the context inputs give. A
// Foundry grader (Config.WithNoSessionGrader) is a grader that reads no session part, with none.
func (m *modelFlags) grader(cfg Config, inputs GraderInputs, runner *llm.Runner) (*Grader, error) {
	if g := cfg.Models.Roles.Grader; g.Family == llm.FamilyFoundry {
		f, err := m.foundryClient(cfg, g, runner)
		if err != nil {
			return nil, err
		}
		return NewNoSessionGrader(f, inputs), nil
	}
	model, err := m.claudeClient(cfg, cfg.Models.Roles.Grader, runner)
	if err != nil {
		return nil, err
	}
	var fallbacks []*claude.Client
	for _, f := range cfg.Models.Roles.GraderFallbacks {
		c, err := m.claudeClient(cfg, f, runner)
		if err != nil {
			return nil, err
		}
		fallbacks = append(fallbacks, c)
	}
	return NewGrader(model, fallbacks, inputs), nil
}
