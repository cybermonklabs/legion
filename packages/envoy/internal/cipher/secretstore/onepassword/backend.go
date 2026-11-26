package onepassword

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// RereadBudget is the whole fleet's budget of POST /v1/secrets/{name}/reread on the 1Password store:
// one reread every 10 seconds, so rereads alone cannot spend more than 8,640 of a day's requests,
// and the daily budget's hold stops them well before that (BROKER_ONEPASSWORD_DAILY_REQUESTS). Its
// burst is four, the measured floor of one person's write sequence, which internal/cipher/api's
// RereadOverallFor never divides (its limits.go says how that floor was measured).
var RereadBudget = ratelimit.Limit{Every: 10 * time.Second, Burst: 4}

// Backend is the 1Password store's registry entry.
type Backend struct{}

func (Backend) Name() string { return Name }
func (Backend) ID() string   { return "onepassword" }

// ServiceSDKs is the 1Password Go SDK, which agent-secrets and cipherd import for this store alone.
func (Backend) ServiceSDKs() []string {
	return []string{"github.com/1password/onepassword-sdk-go"}
}

func (Backend) Development() bool             { return false }
func (Backend) RereadBudget() ratelimit.Limit { return RereadBudget }
func (Backend) Variables() []string {
	return []string{vaultVariable, tokenFileVariable, dailyRequestsVariable}
}

// OpenStore reads the service account token from the file cfg names, once, and opens the vault
// through the 1Password SDK signed in with it, spending from the deployment's daily budget of
// requests, which env's database counts for every replica (NewBudget).
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config, env secretstore.OpenEnv) (secretstore.Opened, error) {
	c := cfg.(config)
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return secretstore.Opened{}, fmt.Errorf("read %s: %w", tokenFileVariable, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return secretstore.Opened{}, fmt.Errorf("%s names %s, which holds no token", tokenFileVariable, c.tokenFile)
	}
	if env.Database == nil {
		return secretstore.Opened{}, errors.New("the 1Password store counts its requests in the broker's database, and the broker gave it none")
	}
	client, err := NewSDKClient(ctx, token, c.vault)
	if err != nil {
		return secretstore.Opened{}, err
	}
	budget := NewBudget(env.Database, c.dailyRequests, env.Now, env.Log)
	return secretstore.Opened{Store: New(client, c.vault, budget, env.Now), Credentials: "service account token file"}, nil
}

// settingsVault is the store's settings field naming its vault (SettingsFields.Vault).
const settingsVault = "onepassword_vault"

// Kind is the store's kind as the CLI's lines name it.
func (Backend) Kind() string { return "1Password" }

// Place is the vault the broker's settings put the agent secrets in, as a line names it after
// Kind: "vault <id>", "" where they name none; as the broker's address lets its answer print
// (Settings.Words).
func (Backend) Place(s secretstore.Settings) string {
	if vault := s.Fields[settingsVault]; vault != "" {
		return "vault " + s.Words(vault)
	}
	return ""
}

// OpenWriter refuses settings that name no vault id or no title prefix, then signs in as
// 1Password's own tools do: through the SDK with OP_SERVICE_ACCOUNT_TOKEN where it is set and the
// SDK builds for this system (sdkSupported), and otherwise through the person's op, which signs in
// as op does, OP_SERVICE_ACCOUNT_TOKEN and OP_ACCOUNT included, every op through opts.Run. The
// vault's permissions decide each write.
func (Backend) OpenWriter(ctx context.Context, s secretstore.Settings, opts secretstore.WriterOptions) (secretstore.Writer, error) {
	vault := s.Fields[settingsVault]
	if !vaultPattern.MatchString(vault) {
		return nil, fmt.Errorf("the broker's settings name no 1Password vault id in %s (got %s), so there is no vault to write", settingsVault, s.Words(vault))
	}
	if !prefixPattern.MatchString(s.Prefix) || len(s.Prefix) > maxPrefixLength {
		return nil, fmt.Errorf("the broker's secrets prefix %s is no 1Password title prefix of lowercase letters, digits and hyphens ending in -", s.Words(s.Prefix))
	}
	h := CurrentHooks()
	var client Client
	var signIn secretstore.SignIn
	if token := os.Getenv(tokenVariable); token != "" && sdkSupported {
		var err error
		if client, err = h.SDKClient(ctx, token, vault); err != nil {
			return nil, failure("sign in to 1Password with "+tokenVariable, err)
		}
		signIn = sdkSignIn{}
	} else {
		account, err := h.Whoami(ctx, opts.Run)
		if err != nil {
			return nil, err
		}
		client = h.OpClient(vault, opts.Run)
		signIn = opSignIn{account: account, opAccount: os.Getenv(accountVariable)}
	}
	return &Writer{Store: New(client, vault, Unlimited, nil), SignIn: signIn, prefix: s.Prefix, read: h.Read}, nil
}

// HelpRows are the 1Password variables of the CLI's environment table.
func (Backend) HelpRows() string {
	return `  OP_SERVICE_ACCOUNT_TOKEN   a 1Password service account token the secret forms sign in with
                             against a broker on a 1Password vault; without it they run op, which
                             signs in as op does
  OP_ACCOUNT                 the op account the secret forms use where op holds more than one
`
}

// ProfileHint is the 1Password clause of the CLI's --profile refusal.
func (Backend) ProfileHint() (secretstore.ProfileHint, bool) {
	return secretstore.ProfileHint{Rank: 30, Clause: "on 1Password set OP_SERVICE_ACCOUNT_TOKEN, or sign in with op"}, true
}
