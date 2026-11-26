package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/redirects"
)

// anthropicTokenURL is Anthropic's token exchange, which takes a jwt-bearer grant of a federation
// rule and answers a short-lived Anthropic token (Anthropic's WIF reference, "Token exchange
// request").
const anthropicTokenURL = "https://api.anthropic.com/v1/oauth/token"

// AnthropicAudience is the audience of the Google identity token the exchange presents: the
// audience an Anthropic federation rule's match block names for a Google issuer, and the one the
// reader AnthropicFederation.IdentityToken calls is built for.
const AnthropicAudience = "https://api.anthropic.com"

// anthropicJWTBearer is the exchange's one grant type (RFC 7523).
const anthropicJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// anthropicTokenPrefix opens every token the exchange answers: an Anthropic OAuth access token. An
// answer that holds anything else, an API key among them, is no token of this exchange and is
// refused.
const anthropicTokenPrefix = "sk-ant-oat01-"

// AnthropicFederation mints an Anthropic organization's token by Workload Identity Federation: it
// reads the Google identity token of the one service account the deployment binds to an
// anthropic-federation secret (Accounts, the deployment's BROKER_SERVICE_ACCOUNT_SECRETS, as for
// google-service-account), for the audience AnthropicAudience, and trades it at Anthropic's
// POST /v1/oauth/token for the token the secret's federation rule grants. The secret's stored value
// names the account as well, and a mint happens only when the two are the same account, for the
// reason GoogleServiceAccount gives: the broker may mint the identity token of every account it is
// bound to, so the stored value alone choosing the account would let whoever may create a secret
// mint for someone else's.
//
// IdentityToken reads that identity token for an account: cmd/cipherd's is
// ciphertoken.GoogleAccounts' Read, with no cache, which mints it at IAM's generateIdToken with
// includeEmail and the broker's own credential. It is a field rather than a call this package
// makes because agent-secrets links this package for ParseValue, and the token reader's package
// would bring modules into that binary that its list does not name. A nil IdentityToken mints
// nothing.
//
// TokenURL is Anthropic's exchange, anthropicTokenURL when empty; HTTP is the client it is called
// on, ownClient when nil, and follows no redirect either way, since the request carries the
// identity token (redirects.Refused); Retry counts each exchange's one attempt in
// broker_mint_attempts_total, and a nil Retry counts none; Now is the clock the minted token's
// expiry is read against, time.Now when nil. A test names its stand-ins in all of them.
//
// The minted token expires when Anthropic says it does, read from the moment the exchange was
// asked, and the airlock's proxy mints again five minutes before that (Minted). The policy serves
// the type at human tier alone, to the requesters its tag names, for grants of an hour at most
// (policy.TagRules), and no model judge decides it (requests' judgeKey).
type AnthropicFederation struct {
	Accounts      map[string]string
	IdentityToken func(account string) (string, error)
	TokenURL      string
	HTTP          *http.Client
	Retry         *Retry
	Now           func() time.Time
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

// errNoIdentityReader is a minter built with no IdentityToken.
var errNoIdentityReader = errors.New("the broker reads no Google identity token for this type")

// federationLifetimeMax bounds a federated token's lifetime, expires_in. A grant of an
// anthropic-federation secret lasts an hour at most (policy.TagRules), and the minted value lives
// for as long as its token does, so a token that outlived the hour would outlive its grant: a
// federation rule may set token_lifetime_seconds anywhere from 60 to 86400, and its owner sets it
// to 3600 or less. A rule that sets five minutes or less is taken, but every proxied call then
// mints again, since the airlock proxy mints again five minutes before a value ends.
const federationLifetimeMax = time.Hour

// Mint returns the Anthropic token the federation rule of the secret req names grants, for the
// identity of the account the deployment binds to the stored secret req's value was read from
// (req.Source), so a profile of a bound secret mints under its source's binding whatever its own
// name (boundAccount). A stored value ParseValue refuses, and a
// secret whose binding is missing or names another account than the stored value, are refused
// before any call is made. The exchange is sent once (Retry.startOnce): the identity token it
// carries is read for this exchange alone, and a retry would present it again, so a failed
// exchange is the release's error and the next release reads a new one. An answer whose
// access_token is no sk-ant-oat01- token, or that names no lifetime, is refused, and so is one
// whose lifetime is over federationLifetimeMax, the rule's fault. The minted value expires at the
// moment the exchange was asked plus that lifetime. No error holds the
// identity token, the minted token or the stored value, and every error wraps one of
// ErrMintStoredValue, ErrMintNotBound, ErrMintRefused or ErrMintUpstream.
func (m *AnthropicFederation) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	const what = "exchange an Anthropic federation token"
	parsed, err := ParseValue(typeAnthropicFederation, req.Value)
	if err != nil {
		return Minted{}, err
	}
	secret := parsed.AnthropicFederation
	bound, err := boundAccount(m.Accounts, req, secret.ServiceAccount)
	if err != nil {
		return Minted{}, err
	}
	if m.IdentityToken == nil {
		return Minted{}, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, errNoIdentityReader)
	}
	// The reader's error names the account, the audience and what failed, Google's refusal cut
	// short among them, and never a token, so it is the operator's to read.
	assertion, err := m.IdentityToken(bound)
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
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	// The token's life is read from before the exchange, so a slow answer never lengthens it.
	asked := now()
	payload, err := mintCall(m.Retry.startOnce(req.Type), redirects.Refused(cmp.Or(m.HTTP, ownClient)), request, what, "Anthropic", http.StatusOK, mintResponseLimit, false, nil)
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
	lifetime := time.Duration(token.ExpiresIn) * time.Second
	if lifetime > federationLifetimeMax {
		return Minted{}, fmt.Errorf("%w: secret %s: Anthropic answered a token that lives %d seconds, longer than the grant's hour: set the federation rule's token_lifetime_seconds to %d or less",
			ErrMintRefused, req.Name, token.ExpiresIn, int(federationLifetimeMax.Seconds()))
	}
	return Minted{Value: token.AccessToken, Expires: asked.Add(lifetime)}, nil
}
