package oauthstore

import (
	"context"
	"slices"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc8693"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// The client type every registration here is, which the token exchange reads through
// rfc8693.Client: which subject, actor and requested token types a client may name, which client
// may exchange a token issued to it, and whether it may present an actor token for a subject
// token that carries no 'may_act' claim. A plain *oauth2.DefaultClient answers none of those. The
// exchange client also answers the scope strategy the exchange checks a requested scope with.
var (
	_ rfc8693.Client               = exchangeRegisteredClient{}
	_ oauth2.ScopeStrategyProvider = exchangeRegisteredClient{}
)

// exchangeRegisteredClient is the exchange client's registration with two answers of its own.
// The library refuses a delegation whose subject token carries no RFC 8693 Section 4.4
// 'may_act' claim unless its client says an authorization outside the token decides it, and
// Google puts no 'may_act' in a person's ID token, so without this every person's sign-in would
// be refused invalid_grant. What decides it instead is the broker's own: the machine's actor
// token is verified against Google and held to the entry rules BROKER_VM_SERVICES makes (the
// google identity provider's decision of a person's trade), and the sign-in that first bound the
// person to the box key is a row in the broker's own database. The other answer is the scope
// strategy (GetScopeStrategy).
type exchangeRegisteredClient struct {
	*oauth2.DefaultRegisteredClient
}

// GetAllowActorTokenWithoutMayAct reports true, for the reason exchangeRegisteredClient gives.
func (exchangeRegisteredClient) GetAllowActorTokenWithoutMayAct() bool { return true }

// GetScopeStrategy is readScopeStrategy: the library checks the scopes an exchange asks for with
// it, against this client's registered scopes and, for a machine login's access token as the
// subject, against the scopes that token was granted.
func (exchangeRegisteredClient) GetScopeStrategy(context.Context) oauth2.ScopeStrategy {
	return readScopeStrategy
}

// readScopeStrategy holds oauthwire.ScopeRead by every subject and reads every other scope as
// the library's default wildcard strategy does. A machine login is granted oauthwire.Scope alone,
// so the library's check that a trade asks only for scopes its subject was granted
// (validateExchangeTokenPolicy) would refuse a machine login's trade that asks for read until it
// logged in again. Read only narrows what a token may do, so every subject holds it; a scope
// that widens, such as approve, is still refused invalid_scope.
func readScopeStrategy(haystack []string, needle string) bool {
	return needle == oauthwire.ScopeRead || oauth2.WildcardScopeStrategy(haystack, needle)
}

// authMethodNone is the 'token_endpoint_auth_method' of a public client, which presents no
// credentials of its own. A registration leaving it unset is read as client_secret_basic, the
// default OpenID Connect Dynamic Client Registration states, which a public client could never
// satisfy. The library spells it in a package its users cannot import.
const authMethodNone = "none"

// GetClient returns one of the provider's two clients, and oauth2.ErrNotFound for any other id:
// the agent-secrets helper logging a machine in, and the caller exchanging a subject token for a
// Dispatch access token.
func (s *Store) GetClient(_ context.Context, id string) (oauth2.Client, error) {
	switch id {
	case oauthwire.ClientID:
		return machineClient(), nil
	case oauthwire.ExchangeClientID:
		return s.exchangeClient(), nil
	}
	return nil, oauth2.ErrNotFound
}

// machineClient is the agent-secrets helper logging a machine in. It is a public client, so it
// authenticates with no secret; the DPoP key its device request names binds every token it is
// issued.
//
// Its tokens are the one kind this provider issues that may itself be exchanged: a dev box
// session presents the access token its machine login holds, and the library asks the client the
// token was issued to whether that is permitted. It permits the exchange client and no other, so
// no caller holding one of these tokens can exchange it anywhere else.
func machineClient() *oauth2.DefaultRegisteredClient {
	return &oauth2.DefaultRegisteredClient{
		DefaultClient: &oauth2.DefaultClient{
			ID:         oauthwire.ClientID,
			Public:     true,
			GrantTypes: []string{string(oauth2.GrantTypeDeviceCode), string(oauth2.GrantTypeRefreshToken)},
			Scopes:     []string{oauthwire.Scope},
		},
		TokenEndpointAuthMethod:         authMethodNone,
		TokenExchangePermittedClientIDs: []string{oauthwire.ExchangeClientID},
	}
}

// ExchangeTypes sets the token types the exchange client may present: subject as a subject token
// beside the broker's own access token, and actor as an actor token. The provider calls it with
// every type its identity providers register (identity.Registry.TokenTypeNames) before it serves
// the token-exchange grant.
func (s *Store) ExchangeTypes(subject, actor []string) {
	s.exchangeSubjectTypes = append(slices.Clone(subject), oauthwire.SubjectTokenTypeAccessToken)
	s.exchangeActorTypes = slices.Clone(actor)
}

// noActorTokenType names no token type at all: it is the exchange client's one actor type when
// no identity provider registers one, because the library reads an empty actor list as every
// type, and a broker whose providers name no actor must take none.
const noActorTokenType = "urn:envoy:params:oauth:token-type:no-actor"

// exchangeClient is the client every exchanging caller presents: a pod, a workflow run, a machine
// holding its cloud identity token, a person signed in on such a machine, and a dev
// box session. It is registered confidential because the library refuses a public client the
// token-exchange grant, but it presents no client credential at all: the provider's own
// authentication strategy answers it for a credential-less exchange, and the subject token and
// the DPoP proof are what the request is judged on. A secret here would be one more shared
// secret in every pod, which is what this exchange exists to remove.
//
// It holds the token-exchange grant and no other, may present exactly the kinds of subject token
// this broker's identity providers register and its own access token (ExchangeTypes), and may ask
// for an access token or the proxy placeholder a person's sign-in is traded for
// (oauthwire.TokenTypeProxyPlaceholder), and nothing else: a caller that asked for a JSON Web
// Token would be asking the broker to sign a claim set of its own, which it does not do here. The
// actor tokens it may present are the actor types the providers register — the machine's Google
// identity token, which a person signing in on that machine carries beside their own: the library
// reads an empty actor list as no restriction at all, so naming these, or noActorTokenType where
// there are none, is what keeps every other kind of token out of the actor_token parameter, and a
// request that carries no actor token is not an actor request at all and reaches no such check.
//
// It is registered for oauthwire.Scope, which a request may name and which grants nothing, and
// for oauthwire.ScopeRead, which narrows the trade to reading whatever its subject
// (oauth.Provider.decide). The library validates a requested scope and grants none of them
// itself: the exchange grants what it decided.
func (s *Store) exchangeClient() oauth2.Client {
	subject := s.exchangeSubjectTypes
	if subject == nil {
		subject = []string{oauthwire.SubjectTokenTypeAccessToken}
	}
	actor := s.exchangeActorTypes
	if len(actor) == 0 {
		actor = []string{noActorTokenType}
	}
	return exchangeRegisteredClient{&oauth2.DefaultRegisteredClient{
		DefaultClient: &oauth2.DefaultClient{
			ID:         oauthwire.ExchangeClientID,
			GrantTypes: []string{string(oauth2.GrantTypeTokenExchange)},
			Scopes:     []string{oauthwire.Scope, oauthwire.ScopeRead},
		},
		TokenExchangeSubjectTokenTypes: slices.Clone(subject),
		// An access token, and the proxy placeholder a person's sign-in is traded for to reach
		// the approve capability (oauth.Provider.grantApprove), which is never an access token.
		TokenExchangeRequestTokenTypes: []string{oauthwire.SubjectTokenTypeAccessToken, oauthwire.TokenTypeProxyPlaceholder},
		TokenExchangeActorTokenTypes:   slices.Clone(actor),
	}}
}

// ClientAssertionJWTValid reports every jti as unknown: no registered client authenticates with a
// client assertion, the machine-login client being public and the exchange client presenting no
// credential of its own (the subject token and its DPoP proof are what the exchange judges).
func (s *Store) ClientAssertionJWTValid(context.Context, string) error { return nil }

// SetClientAssertionJWT is oauth2.ErrNotFound: no registered client authenticates with a client
// assertion, so there is never one to remember.
func (s *Store) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return oauth2.ErrNotFound
}

// CreateClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) CreateClient(context.Context, oauth2.Client) error { return oauth2.ErrNotFound }

// UpdateClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) UpdateClient(context.Context, string, oauth2.Client) error {
	return oauth2.ErrNotFound
}

// DeleteClient is oauth2.ErrNotFound: the provider registers no clients.
func (s *Store) DeleteClient(context.Context, string) error { return oauth2.ErrNotFound }
