package harness

import (
	"bytes"
	"flag"
	"fmt"
	"net/url"
	"os"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// CredentialURL refuses raw, a URL that a key, a token or a bearer is sent to, unless it is https,
// or http to a host inside the machine or the cluster (embed.ClusterLocalHost, the rule
// cmd/dispatch refuses a model group's endpoint by): localhost, a loopback address, or a
// Kubernetes Service's name (ending .svc or .cluster.local). Over http to any other host the
// credential would cross the network in clear text. name is the flag or the setting raw came
// from. An empty raw is no URL and sends nothing, so it passes; the caller that needs one says so.
func CredentialURL(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q is not an absolute http or https URL with a host", name, raw)
	}
	if u.Scheme == "https" || embed.ClusterLocalHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s %q is http to a host outside the cluster, which would carry the credential in clear text: use https, or http to localhost, a loopback address or a cluster Service (*.svc, *.cluster.local)", name, raw)
}

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
	// claudeBuilt and foundryBuilt say a client of the family was built (claudeClient,
	// foundryClient), whose calls read the gateway's key or the federated token (readCredentials).
	claudeBuilt, foundryBuilt bool
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

// files are the files the flags name that a command reads: the gateway's key and the federated
// token.
func (m *modelFlags) files() []File {
	return append(Files("--gateway-key-file", m.keyFile), Files("--federated-token-file", m.tokenFile)...)
}

// ReadCredential reads the credential file path, which name names, and refuses one it cannot read
// or that holds nothing but white space, which no call signs in with. A run that must not start
// on a credential its calls could not send, a held-out run or a final test before its claim,
// reads each of its credential files so, rather than meet the refusal on its first call.
func ReadCredential(name, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%s %s holds no credential", name, path)
	}
	return nil
}

// readCredentials reads each credential file a client built so far reads on its calls
// (ReadCredential): the gateway's key, which a Claude model's client reads on each call, and the
// federated token, which a Foundry model's Entra sign-in reads on its first call alone. A held-out
// run reads them before its claim, so a run refused on either has read no case.
func (m *modelFlags) readCredentials() error {
	if m.claudeBuilt {
		if err := ReadCredential("the gateway key file", m.keyFile); err != nil {
			return err
		}
	}
	if m.foundryBuilt {
		return ReadCredential("the federated token file", m.tokenFile)
	}
	return nil
}

// claudeClient is the client of a Claude model of the config, at a gateway the key may be sent to
// (CredentialURL).
func (m *modelFlags) claudeClient(cfg Config, model ModelEntry, runner *llm.Runner) (*claude.Client, error) {
	price, ok := cfg.Prices.Prices[model.ID]
	if !ok {
		return nil, fmt.Errorf("prices.yaml has no price for %q", model.ID)
	}
	if err := CredentialURL("--gateway", m.gateway); err != nil {
		return nil, err
	}
	client, err := claude.New(m.gateway, m.keyFile, model.ID, model.Effort, price.agentloop(), runner)
	m.claudeBuilt = m.claudeBuilt || err == nil
	return client, err
}

// foundryTokens is the Entra token source of a Foundry account; a test replaces it with a
// stand-in's, which signs in nowhere.
var foundryTokens = llm.FoundryConfig.Tokens

// foundryClient is the client of a Foundry deployment of the config, at its effort and the price
// of its id, at an endpoint the Entra token may be sent to (CredentialURL).
func (m *modelFlags) foundryClient(cfg Config, model ModelEntry, runner *llm.Runner) (*llm.Foundry, error) {
	account := m.foundryConfig()
	if err := CredentialURL("--foundry-endpoint", account.Endpoint); err != nil {
		return nil, err
	}
	tokens, err := foundryTokens(account)
	if err != nil {
		return nil, err
	}
	m.foundryBuilt = true
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
