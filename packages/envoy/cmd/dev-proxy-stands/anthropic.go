// packages/envoy/cmd/dev-proxy-stands/anthropic.go
//
// The stand-ins an anthropic-federation secret is minted at, which the anthropic kind's check
// (scripts/airlock-check.sh k47) reads back: IAM Service Account Credentials' generateIdToken,
// where the broker reads the Google identity token of the account BROKER_SERVICE_ACCOUNT_SECRETS
// binds to the secret (BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL); Anthropic's token exchange,
// POST /v1/oauth/token, where it trades that token for an sk-ant-oat01- token
// (BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL); and the metadata server's access-token path, which
// the broker spends at generateIdToken as a pod spends its own. The Admin API the minted token is
// spent at is the anthropic stand, a recording service like S whose certificate names
// api.anthropic.com, so it is no code of this file's.
//
// The broker's minter reaches the two plain stands with a client that trusts the host's own roots
// alone (secrets.AnthropicFederation, ciphertoken.GoogleAccount), which is why both are served
// over http, as the GitHub and Google minter stands are.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// The paths these stands answer: generateIdToken under one account's name, the metadata server's
// access token of the machine's own account, and Anthropic's token exchange.
const (
	iamAccountPrefix  = "/v1/projects/-/serviceAccounts/"
	iamAccountSuffix  = ":generateIdToken"
	metadataTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
	anthropicExchange = "/v1/oauth/token"
)

// anthropicJWTBearer is the one grant type Anthropic's exchange takes (RFC 7523), and
// anthropicTokenPrefix what every token it answers opens with: secrets.AnthropicFederation refuses
// an answer that holds anything else.
const (
	anthropicJWTBearer   = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	anthropicTokenPrefix = "sk-ant-oat01-"
)

// defaultAnthropicLifetime is the expires_in the exchange stand answers until a check sets
// another: an hour, the longest the broker takes (federationLifetimeMax), so a stack driven with
// no call to the answer route mints as a federation rule of an hour does.
const defaultAnthropicLifetime = 3600

// randomText is n random bytes as unpadded base64url, which is the alphabet both a JWT's parts
// and Anthropic's tokens are written in.
func randomText(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// metadataAccessAnswer is the metadata server's answer on its access-token path: this run's one
// access token, for an hour, with the Metadata-Flavor header internal/googleidentity requires of
// what answers there. A request without Metadata-Flavor: Google is refused 403, as the server
// refuses one. Every other path is left to the metadata stand of proxyrulestest beside it.
func metadataAccessAnswer(access string) proxyrulestest.Answer {
	return func(w http.ResponseWriter, _ *http.Request, seen proxyrulestest.Request) bool {
		if seen.Path != metadataTokenPath {
			return false
		}
		w.Header().Set("Metadata-Flavor", "Google")
		if seen.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "expires_in": 3600, "token_type": "Bearer"})
		return true
	}
}

// identityToken is one Google identity token the IAM stand minted: the account it was minted
// for, the audience it was asked for, and the token. GET <records URL>/tokens/iam answers the
// list, which is how a check pairs an exchange's assertion with the account it names.
type identityToken struct {
	Account  string `json:"account"`
	Audience string `json:"audience"`
	Token    string `json:"token"`
}

// iamStand is IAM Service Account Credentials' generateIdToken: POST
// /v1/projects/-/serviceAccounts/{account}:generateIdToken, whose JSON body names the audience.
// It answers an identity token of its own for that account and audience, a JWT whose aud and
// email claims say both, and keeps each one it minted. A call that does not carry this run's
// metadata access token as its bearer is refused 401, as Google refuses a caller it cannot
// authenticate: the broker spends the pod's own access token there, and a stand that took any
// bearer would hide a broker that sent none.
type iamStand struct {
	access string

	mu     sync.Mutex
	minted []identityToken
}

// Answer is generateIdToken's answer, and false for every other request.
func (s *iamStand) Answer(w http.ResponseWriter, _ *http.Request, seen proxyrulestest.Request) bool {
	rest, ok := strings.CutPrefix(seen.Path, iamAccountPrefix)
	if !ok {
		return false
	}
	account, ok := strings.CutSuffix(rest, iamAccountSuffix)
	if !ok || account == "" || strings.Contains(account, "/") {
		return false
	}
	if seen.Method != http.MethodPost {
		googleError(w, http.StatusMethodNotAllowed, "generateIdToken takes POST, not "+seen.Method)
		return true
	}
	if seen.Header.Get("Authorization") != "Bearer "+s.access {
		googleError(w, http.StatusUnauthorized, "Request had invalid authentication credentials: the bearer is not this machine's access token")
		return true
	}
	var body struct {
		Audience     string `json:"audience"`
		IncludeEmail bool   `json:"includeEmail"`
	}
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil || body.Audience == "" {
		googleError(w, http.StatusBadRequest, "the body names no audience")
		return true
	}
	token := identityJWT(account, body.Audience)
	s.mu.Lock()
	s.minted = append(s.minted, identityToken{Account: account, Audience: body.Audience, Token: token})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	return true
}

// Minted is every identity token this stand answered, in order, as a copy.
func (s *iamStand) Minted() []identityToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.minted)
}

// issued reports whether token is one this stand minted.
func (s *iamStand) issued(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.minted, func(t identityToken) bool { return t.Token == token })
}

// identityJWT is an identity token in the shape Google mints one: three base64url parts, whose
// claims name the account as email and the audience as aud, signed by nobody. Its signature part
// is random, so two mints of one account and audience are two tokens, as Google's are.
func identityJWT(account, audience string) string {
	now := time.Now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://accounts.google.com", "aud": audience, "azp": account, "email": account,
		"email_verified": true, "iat": now, "exp": now + 3600,
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "." + randomText(32)
}

// googleError answers a Google API error body: code, message and status, the shape a Google
// client reads.
func googleError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": message, "status": http.StatusText(status)}})
}

// anthropicToken is one token the exchange stand answered, with the lifetime it said the token
// has. GET <records URL>/tokens/anthropic answers the list: a check reads the token the stand-in
// API should have got in Authorization from it, and the SHA-256 the broker's mint line should
// name.
type anthropicToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	// ServiceAccountID is the svac_ id the exchange named, which tells two secrets' tokens apart.
	ServiceAccountID string `json:"service_account_id"`
}

// anthropicExchangeStand is Anthropic's token exchange: POST /v1/oauth/token with a jwt-bearer
// grant's JSON body. It answers an sk-ant-oat01- token of its own and the expires_in a check set
// (defaultAnthropicLifetime until one does), and keeps each token it answered. What each exchange
// was sent - its assertion, organization_id, federation_rule_id, service_account_id and
// workspace_id - is the service's own record. Anthropic verifies the assertion against Google's
// keys; this stand takes an assertion only when the IAM stand minted it, so the token the broker
// presents has to be the one it read there, and refuses any other 401 invalid_grant.
type anthropicExchangeStand struct {
	iam *iamStand

	mu        sync.Mutex
	expiresIn int64
	minted    []anthropicToken
}

// anthropicExchangeBody is the exchange's JSON body as secrets.AnthropicFederation sends it.
type anthropicExchangeBody struct {
	GrantType        string `json:"grant_type"`
	Assertion        string `json:"assertion"`
	FederationRuleID string `json:"federation_rule_id"`
	OrganizationID   string `json:"organization_id"`
	ServiceAccountID string `json:"service_account_id"`
	WorkspaceID      string `json:"workspace_id"`
}

// Answer is the exchange's answer, and false for every other request.
func (a *anthropicExchangeStand) Answer(w http.ResponseWriter, _ *http.Request, seen proxyrulestest.Request) bool {
	if seen.Path != anthropicExchange {
		return false
	}
	if seen.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "the token exchange takes POST, not "+seen.Method)
		return true
	}
	var body anthropicExchangeBody
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body is no JSON object")
		return true
	}
	switch {
	case body.GrantType != anthropicJWTBearer:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "the grant type is not "+anthropicJWTBearer)
		return true
	case body.Assertion == "" || body.FederationRuleID == "" || body.OrganizationID == "" || body.ServiceAccountID == "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body names no assertion, federation_rule_id, organization_id or service_account_id")
		return true
	case !a.iam.issued(body.Assertion):
		oauthError(w, http.StatusUnauthorized, "invalid_grant", "the assertion is no identity token the stand-in IAM minted")
		return true
	}
	a.mu.Lock()
	token := anthropicToken{AccessToken: anthropicTokenPrefix + randomText(48), ExpiresIn: a.expiresIn, ServiceAccountID: body.ServiceAccountID}
	a.minted = append(a.minted, token)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token.AccessToken, "expires_in": token.ExpiresIn, "token_type": "bearer"})
	return true
}

// Answers sets the expires_in every later exchange answers.
func (a *anthropicExchangeStand) Answers(expiresIn int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expiresIn = expiresIn
}

// Minted is every token this stand answered, in order, as a copy.
func (a *anthropicExchangeStand) Minted() []anthropicToken {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.minted)
}

// oauthError answers an OAuth 2.0 error body (RFC 6749 §5.2).
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

// anthropicAnswerRequest is the body POST <records URL>/answers/anthropic takes: the expires_in
// every later exchange answers, a whole number of seconds above zero. A check sets 3601 to drive
// the broker's refusal of a token that outlives its grant's hour.
type anthropicAnswerRequest struct {
	ExpiresIn int64 `json:"expires_in"`
}

// anthropicAnswerOf is one such request as the lifetime it sets, and what is wrong with a request
// that sets none: a lifetime of zero or less, which also is what a body that names none decodes
// to, is named back, since a check that misspelt the field must read that rather than drive its
// next case against the lifetime still in place.
func anthropicAnswerOf(body anthropicAnswerRequest) (int64, error) {
	if body.ExpiresIn <= 0 {
		return 0, errors.New(`the answer names no expires_in above zero: {"expires_in": <seconds>}`)
	}
	return body.ExpiresIn, nil
}

// tokensRoute answers the list a stand minted, as a JSON array.
func tokensRoute[T any](minted func() []T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		list := minted()
		if list == nil {
			list = []T{}
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(list); err != nil {
			http.Error(w, fmt.Sprintf("encode the list: %v", err), http.StatusInternalServerError)
		}
	}
}
