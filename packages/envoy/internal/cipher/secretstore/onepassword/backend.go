package onepassword

import (
	"context"
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
// through the 1Password SDK signed in with it.
func (Backend) OpenStore(ctx context.Context, cfg secretstore.Config, _ secretstore.OpenEnv) (secretstore.Opened, error) {
	c := cfg.(config)
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return secretstore.Opened{}, fmt.Errorf("read %s: %w", tokenFileVariable, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return secretstore.Opened{}, fmt.Errorf("%s names %s, which holds no token", tokenFileVariable, c.tokenFile)
	}
	client, err := NewSDKClient(ctx, token, c.vault)
	if err != nil {
		return secretstore.Opened{}, err
	}
	return secretstore.Opened{Store: New(client, c.vault, Unlimited, nil), Credentials: "service account token file"}, nil
}
