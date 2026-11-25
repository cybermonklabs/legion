package oauth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// formRequestedTokenType is the request's requested_token_type (RFC 8693 Section 2.1), which the
// library reads too: absent, it is the access token.
const formRequestedTokenType = "requested_token_type"

// resultGrantMaxLifetime is the longest a result grant lasts: a report writes its checks one after
// another, each bounded at a minute, so a grant shorter than the run's report fails more reports,
// and a longer one leaves a placeholder that opens the proxy longer than any report needs.
const resultGrantMaxLifetime = 15 * time.Minute

// resultValueSlot is the one slot of DISPATCH_RESULTS_TOKEN, the wire word a stored secret's own
// value is released under (requests.SlotValue): the placeholder stands for the whole token.
const resultValueSlot = "value"

// AirlockSettings is what a result entry's run is handed beside its placeholder: where the
// broker's airlock proxy is, the certificate authority to trust it by, and the hosts the proxy
// block of the results profile DISPATCH_RESULTS_TOKEN binds, which the command keeps off its
// NO_PROXY; and the results entry the run's grant is made under, which the grant records.
type AirlockSettings struct {
	// ProxyURL is BROKER_AIRLOCK_PROXY_URL.
	ProxyURL string
	// CACertificatePEM is the airlock authority's own certificate, as every release hands it over.
	CACertificatePEM string
	// ResultHosts are the hosts DISPATCH_RESULTS_TOKEN's entry binds.
	ResultHosts []string
	// EntryDigest is this broker's results entry: the hex SHA-256 of the profile
	// DISPATCH_RESULTS_TOKEN's fingerprint and the private destination's scheme and host, which
	// cmd/cipherd computes once at boot (resultEntryDigest) and hands the request machine too
	// (requests.Machine.ResultEntryDigest). Every grant records it (oauthstore.ResultGrant), and a
	// broker pod whose own digest differs refuses the grant, so a run's token goes only where the
	// broker that granted it would have sent it.
	EntryDigest string
	// GrantLifetime is BROKER_RESULT_GRANT_TEST_SECONDS, a local stack's own grant lifetime in
	// place of resultGrantLifetime's; zero for every production start, which refuses the setting.
	GrantLifetime time.Duration
}

// asksForPlaceholder reports whether form asks for the airlock placeholder.
func asksForPlaceholder(form url.Values) bool {
	return form.Get(formRequestedTokenType) == oauthwire.TokenTypeAirlockPlaceholder
}

// placeholderDecision holds what a request asks for to what decide decided: a result entry's run
// is granted the airlock placeholder and never an access token, since any process of a CI job
// can ask GitHub for a fresh run token, and a results token it was answered would be a Dispatch
// credential in the job; and no other decision is granted the placeholder. Both refusals are
// invalid_request, the subject token being sound and the request asking for what its identity is
// not given.
func placeholderDecision(form url.Values, claims exchange.Claims) error {
	results := claims.Scope == exchange.ProjectScopeResults
	switch asked := asksForPlaceholder(form); {
	case results && !asked:
		return oauth2.ErrInvalidRequest.WithHintf("A result entry's run gets no access token: it asks for %s=%s, and the broker's airlock holds its capability; run the write under `agent-secrets result-grant`.",
			formRequestedTokenType, oauthwire.TokenTypeAirlockPlaceholder)
	case asked && !results:
		return oauth2.ErrInvalidRequest.WithHint("Only a result entry's run is granted through the airlock.")
	}
	return nil
}

// resultGrantLifetime is how long a result grant lasts: resultGrantMaxLifetime, or the access
// lifetime when that is shorter, so a grant never outlives the tokens this broker mints, or the
// local stack's BROKER_RESULT_GRANT_TEST_SECONDS when it names one.
func (p *Provider) resultGrantLifetime() time.Duration {
	if p.airlock != nil && p.airlock.GrantLifetime > 0 {
		return p.airlock.GrantLifetime
	}
	return min(resultGrantMaxLifetime, p.accessLifetime)
}

// writeResultGrant writes the grant of the run decide decided onto session, on the transaction
// ctx carries, and answers the placeholder the run is handed and the grant. The grant holds the
// session's decision whole (its subject, session name, principal, granted scopes and extra claims),
// the placeholder's hash alone, and the results entry it is made under (AirlockSettings.EntryDigest).
// The session's access-token expiry is set to the grant's end, so claimSession holds the name until
// then.
func (p *Provider) writeResultGrant(ctx context.Context, request oauth2.Requester, session *oauthstore.ExchangeSession) (string, oauthstore.ResultGrant, error) {
	held, err := placeholder.Mint(placeholder.ResultPrefix)
	if err != nil {
		return "", oauthstore.ResultGrant{}, err
	}
	name, _ := session.Extra[ClaimSession].(string)
	expires := time.Now().Add(p.resultGrantLifetime()).UTC().Truncate(time.Microsecond)
	session.SetExpiresAt(oauth2.AccessToken, expires)
	grant := oauthstore.ResultGrant{
		ID: uuid.NewString(), PlaceholderSHA256: placeholder.Hash(held),
		Subject: session.Subject, Session: name, Principal: session.Principal,
		Scopes: []string(request.GetGrantedScopes()), Extra: maps.Clone(session.Extra),
		EntryDigest: p.airlock.EntryDigest, ExpiresAt: expires,
	}
	if err := p.store.CreateResultGrant(ctx, grant); err != nil {
		return "", oauthstore.ResultGrant{}, err
	}
	return held, grant, nil
}

// grantPlaceholder answers a result entry's run that asked for the airlock placeholder, on the
// exchange's open transaction txCtx: it writes the grant (writeResultGrant), claims the session
// name until the grant's end, commits, and answers C8 with no-store: the placeholder as
// access_token, its type as issued_token_type, the token_type N_A of a token that is no access
// token (RFC 8693 Section 2.2.1), the seconds the grant lasts, the grant's id and the airlock
// release a command runs with. The library mints nothing for it, and the broker never calls its
// own public URL.
func (p *Provider) grantPlaceholder(ctx, txCtx context.Context, w http.ResponseWriter, request oauth2.AccessRequester, session *oauthstore.ExchangeSession, committed *bool) {
	if p.airlock == nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithHint("This broker's airlock binds no "+oauthwire.AirlockResultsName+", so it grants no result entry's run a placeholder."))
		return
	}
	held, grant, err := p.writeResultGrant(txCtx, request, session)
	if err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	if _, _, err := p.claimSession(txCtx, session); err != nil {
		p.writeError(ctx, w, request, err)
		return
	}
	if err := p.store.Commit(txCtx); err != nil {
		p.writeError(ctx, w, request, oauth2.ErrServerError.WithWrap(err).WithDebugError(err))
		return
	}
	*committed = true
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":         held,
		"issued_token_type":    oauthwire.TokenTypeAirlockPlaceholder,
		"token_type":           "N_A",
		"expires_in":           int64(time.Until(grant.ExpiresAt).Round(time.Second) / time.Second),
		oauthwire.ExtraGrantID: oauthwire.ResultGrantIDPrefix + grant.ID,
		oauthwire.ExtraAirlock: oauthwire.AirlockRelease{
			ProxyURL: p.airlock.ProxyURL, CACertificate: p.airlock.CACertificatePEM, ProxyPassword: held,
			Names: map[string]oauthwire.AirlockName{oauthwire.AirlockResultsName: {
				Value: held, Slots: map[string]string{resultValueSlot: held}, Hosts: p.airlock.ResultHosts,
			}},
		},
	})
}

// MintResultToken mints the results token this broker pod puts into a result grant's writes
// (the airlock proxy's resultTransport): an access token of the exchange client holding the
// grant's decision whole - its principal as subject and username, its granted scopes and its
// extra claims, the run among them - bound to the pod key whose RFC 7638 thumbprint jkt is, and
// ending when the grant ends, so a token the broker no longer serves is no longer live at
// introspection. It is stored as every access token is, by its signature, so the token's text is
// in no table, and it is minted on the pod that serves the write: no other pod holds the key it is
// bound to, so a token minted elsewhere could not be used there.
func (p *Provider) MintResultToken(ctx context.Context, grant oauthstore.ResultGrant, jkt string) (string, error) {
	if jkt == "" {
		return "", errors.New("oauth: a result grant's token is minted only bound to the pod's DPoP key")
	}
	client, err := p.store.GetClient(ctx, oauthwire.ExchangeClientID)
	if err != nil {
		return "", err
	}
	session := oauthstore.NewExchangeSession()
	session.Subject, session.Username = grant.Subject, grant.Principal
	session.Extra = maps.Clone(grant.Extra)
	if session.Extra == nil {
		session.Extra = map[string]any{}
	}
	session.SetDPoPJWKThumbprint(jkt)
	session.SetExpiresAt(oauth2.AccessToken, grant.ExpiresAt)
	request := oauth2.NewRequest()
	request.Client = client
	request.Session = session
	request.RequestedAt = time.Now().UTC().Truncate(time.Second)
	request.SetRequestedScopes(grant.Scopes)
	for _, scope := range grant.Scopes {
		request.GrantScope(scope)
	}
	token, signature, err := p.strategy.GenerateAccessToken(ctx, request)
	if err != nil {
		return "", err
	}
	if err := p.store.CreateAccessTokenSession(ctx, signature, request); err != nil {
		return "", err
	}
	return token, nil
}
