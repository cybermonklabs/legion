package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// anthropicTokenURL is Anthropic's token exchange, which takes a jwt-bearer grant of a federation
// rule and answers a short-lived Anthropic token (Anthropic's WIF reference, "Token exchange
// request").
const anthropicTokenURL = "https://api.anthropic.com/v1/oauth/token"

// anthropicAudience is the audience of the Google identity token the exchange presents: the
// audience an Anthropic federation rule's match block names for a Google issuer.
const anthropicAudience = "https://api.anthropic.com"

// anthropicJWTBearer is the exchange's one grant type (RFC 7523).
const anthropicJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// anthropicTokenPrefix opens every token the exchange answers: an Anthropic OAuth access token. An
// answer that holds anything else, an API key among them, is no token of this exchange and is
// refused.
const anthropicTokenPrefix = "sk-ant-oat01-"

// AnthropicFederation mints an Anthropic organization's token by Workload Identity Federation: it
// reads the Google identity token of the one service account the deployment binds to an
// anthropic-federation secret (Accounts, the deployment's BROKER_SERVICE_ACCOUNT_SECRETS, as for
// google-service-account), minted at IAM's generateIdToken for the audience
// https://api.anthropic.com with the broker's own credential (ciphertoken.GoogleAccount), and
// trades it at Anthropic's POST /v1/oauth/token for the token the secret's federation rule
// grants. The secret's stored value names the account as well, and a mint happens only when the
// two are the same account, for the reason GoogleServiceAccount gives: the broker may mint the
// identity token of every account it is bound to, so the stored value alone choosing the account
// would let whoever may create a secret mint for someone else's.
//
// TokenURL is Anthropic's exchange, anthropicTokenURL when empty; IAMCredentialsURL is IAM's,
// Google's when empty; HTTP is the client both are called on; Now is the clock the minted token's
// expiry is read against, time.Now when nil. A test names its stand-ins in all four.
//
// The minted token expires when Anthropic says it does, now plus expires_in, and the airlock's
// proxy mints again five minutes before that (Minted). The policy serves the type at human tier
// alone, to the requesters its tag names, for grants of an hour at most (policy.TagRules).
type AnthropicFederation struct {
	Accounts          map[string]string
	TokenURL          string
	HTTP              *http.Client
	IAMCredentialsURL string
	Now               func() time.Time

	// mu guards readers: two releases can mint at once.
	mu sync.Mutex
	// readers is one identity-token reader for each bound account, so its short cache and its
	// own access-token source serve every mint of that account rather than one apiece.
	readers map[string]*ciphertoken.GoogleAccount
}

// anthropicExchange is one exchange's JSON body. WorkspaceID is left out when empty: Anthropic
// then scopes the token to the rule's sole workspace.
type anthropicExchange struct {
	GrantType        string `json:"grant_type"`
	Assertion        string `json:"assertion"`
	FederationRuleID string `json:"federation_rule_id"`
	OrganizationID   string `json:"organization_id"`
	ServiceAccountID string `json:"service_account_id"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
}

// anthropicToken is the exchange's answer (RFC 6749 §5.1).
type anthropicToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// reader is the identity-token reader of account, made on its first mint.
func (m *AnthropicFederation) reader(account string) *ciphertoken.GoogleAccount {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.readers[account]; ok {
		return r
	}
	if m.readers == nil {
		m.readers = map[string]*ciphertoken.GoogleAccount{}
	}
	r := ciphertoken.NewGoogleAccount(anthropicAudience)
	r.ServiceAccount, r.IAMCredentialsURL, r.HTTP = account, m.IAMCredentialsURL, m.HTTP
	m.readers[account] = r
	return r
}

// Mint returns the Anthropic token the federation rule of the secret req names grants, for the
// identity of the account the deployment binds to it. A stored value ParseValue refuses, and a
// secret whose binding is missing or names another account than the stored value, are refused
// before any call is made. An answer whose access_token is no sk-ant-oat01- token, or that names
// no lifetime, is refused. No error holds the identity token, the minted token or the stored
// value, and every error wraps one of ErrMintStoredValue, ErrMintNotBound, ErrMintRefused or
// ErrMintUpstream.
func (m *AnthropicFederation) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	const what = "exchange an Anthropic federation token"
	parsed, err := ParseValue(typeAnthropicFederation, req.Value)
	if err != nil {
		return Minted{}, err
	}
	secret := parsed.AnthropicFederation
	bound, ok := m.Accounts[req.Name]
	if !ok {
		return Minted{}, fmt.Errorf("%w: secret %s: the deployment binds no Google service account to it", ErrMintNotBound, req.Name)
	}
	if bound != secret.ServiceAccount {
		return Minted{}, fmt.Errorf("%w: secret %s: the deployment binds it to %s, which its stored value does not name", ErrMintNotBound, req.Name, bound)
	}
	// The reader's error names the account, the audience and what failed, Google's refusal cut
	// short among them, and never a token, so it is the operator's to read.
	assertion, err := m.reader(bound).Read()
	if err != nil {
		return Minted{}, fmt.Errorf("%w: %s: %s", ErrMintUpstream, what, err)
	}
	body, _ := json.Marshal(anthropicExchange{
		GrantType: anthropicJWTBearer, Assertion: assertion, FederationRuleID: secret.FederationRuleID,
		OrganizationID: secret.OrganizationID, ServiceAccountID: secret.ServiceAccountID, WorkspaceID: secret.WorkspaceID,
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(m.TokenURL, anthropicTokenURL), bytes.NewReader(body))
	if err != nil {
		return Minted{}, fmt.Errorf("%w: create the Anthropic token exchange request: %w", ErrMintUpstream, err)
	}
	request.Header.Set("Content-Type", "application/json")
	// One attempt, and an exchange that may have reached Anthropic is not sent again: the
	// identity token is the assertion, which Anthropic may hold to one use.
	payload, err := mintCall((*Retry)(nil).start(req.Type), m.HTTP, request, what, "Anthropic", http.StatusOK, mintResponseLimit, false, nil)
	if err != nil {
		return Minted{}, err
	}
	var token anthropicToken
	if json.Unmarshal(payload, &token) != nil {
		return Minted{}, fmt.Errorf("%w: decode the Anthropic token exchange's answer: not a JSON object of its fields", ErrMintUpstream)
	}
	if !strings.HasPrefix(token.AccessToken, anthropicTokenPrefix) || len(token.AccessToken) == len(anthropicTokenPrefix) {
		return Minted{}, fmt.Errorf("%w: Anthropic answered no %s token", ErrMintUpstream, anthropicTokenPrefix)
	}
	if token.ExpiresIn <= 0 {
		return Minted{}, fmt.Errorf("%w: Anthropic answered a token with no lifetime", ErrMintUpstream)
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	return Minted{Value: token.AccessToken, Expires: now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}
