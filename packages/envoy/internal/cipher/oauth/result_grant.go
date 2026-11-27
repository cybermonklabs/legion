package oauth

import (
	"context"
	"maps"
	"net/url"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// resultGrantMaxLifetime is the longest a result grant lasts: a report writes its checks one after
// another, each bounded at a minute, so a grant shorter than the run's report fails more reports,
// and a longer one leaves a placeholder that opens the proxy longer than any report needs.
const resultGrantMaxLifetime = 15 * time.Minute

// resultValueSlot is the one slot of DISPATCH_RESULTS_TOKEN, the wire word a stored secret's own
// value is released under (requests.SlotValue): the placeholder stands for the whole token.
const resultValueSlot = "value"

// AirlockSettings is what a result entry's run is handed beside its placeholder: where the
// broker's airlock proxy is, the roots to trust it by, and the hosts the proxy block of the
// results profile DISPATCH_RESULTS_TOKEN binds, which the command keeps off its NO_PROXY; and the
// results entry the run's grant is made under, which the grant records.
type AirlockSettings struct {
	// ProxyURL is BROKER_AIRLOCK_PROXY_URL.
	ProxyURL string
	// ProxyRoots answers the roots the proxy's certificates chain to, PEM blocks one after the
	// other, read at each trade as a release of names reads them (requests.Machine.ProxyRoots): a
	// pod on a pool learns its root from its first intermediate, after the start, and a rollout
	// carries two roots. nil, or an answer of "", hands the run no root.
	ProxyRoots func() string
	// ResultHosts are the hosts DISPATCH_RESULTS_TOKEN's entry binds.
	ResultHosts []string
	// EntryDigest is this broker's results entry: config.MintedEntryDigest of the profile
	// DISPATCH_RESULTS_TOKEN's fingerprint and the private destination of its host's scheme, host
	// name and port, which cmd/cipherd computes once at boot (resultEntryDigest) and hands the
	// request machine too (the results kind's requests.MintedKind.EntryDigest). Every grant records
	// it (oauthstore.MintedGrant.EntryDigest), and a broker pod whose own digest differs refuses
	// the grant, so a run's token goes only to a destination of the same text as the granting
	// broker's. The digest binds the destination's name, not an address: where the URL's host is a
	// name, the address each pod's resolver gives that name is each pod's own, and the proxy holds
	// it to the destination's address rule (netaddr.DestinationRefusal) at every dial.
	EntryDigest string
	// GrantLifetime is BROKER_RESULT_GRANT_TEST_SECONDS, a local stack's own grant lifetime in
	// place of resultGrantLifetime's; zero for every production start, which refuses the setting.
	GrantLifetime time.Duration
}

// proxyRoots is the roots a run's release hands over, and "" where the settings answer none, as
// a deployment whose pod holds no intermediate yet does (ApproveSettings.proxyRoots).
func (s *AirlockSettings) proxyRoots() string {
	if s.ProxyRoots == nil {
		return ""
	}
	return s.ProxyRoots()
}

// asksForPlaceholder reports whether form asks for the proxy placeholder, the one type a trade
// for a capability the broker mints and holds asks for: a result entry's run's and a person's
// approve trade alike (oauthwire.TokenTypeProxyPlaceholder).
func asksForPlaceholder(form url.Values) bool {
	return form.Get(formRequestedTokenType) == oauthwire.TokenTypeProxyPlaceholder
}

// placeholderDecision holds a result entry's run to the proxy placeholder: it is granted the
// placeholder of a result grant and never an access token, since any process of a CI job can ask
// GitHub for a fresh run token, and a results token it was answered would be a Dispatch
// credential in the job. The refusal is invalid_request, the subject token being sound and the
// request asking for what its identity is not given. A placeholder trade of any other decision is
// the approve capability's, which grantApprove holds to a person's sign-in.
func placeholderDecision(form url.Values, claims exchange.Claims) error {
	if claims.Scope == exchange.ProjectScopeResults && !asksForPlaceholder(form) {
		return oauth2.ErrInvalidRequest.WithHintf("A result entry's run gets no access token: it asks for %s=%s, and the broker's proxy holds its capability; run the write under `agent-secrets result-grant`.",
			formRequestedTokenType, oauthwire.TokenTypeProxyPlaceholder)
	}
	return nil
}

// resultTrade reports whether session is a result entry's run: the project scope decide wrote for
// it is results.
func resultTrade(session *oauthstore.ExchangeSession) bool {
	word, _ := session.Extra[ClaimProjectScope].(string)
	return word == oauthwire.ProjectScopeResults
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
func (p *Provider) writeResultGrant(ctx context.Context, request oauth2.Requester, session *oauthstore.ExchangeSession) (string, oauthstore.MintedGrant, error) {
	held, err := placeholder.Mint(placeholder.MintedPrefix(oauthwire.AirlockResultsWord))
	if err != nil {
		return "", oauthstore.MintedGrant{}, err
	}
	name, _ := session.Extra[ClaimSession].(string)
	expires := time.Now().Add(p.resultGrantLifetime()).UTC().Truncate(time.Microsecond)
	session.SetExpiresAt(oauth2.AccessToken, expires)
	grant := oauthstore.MintedGrant{
		ID: uuid.NewString(), PlaceholderSHA256: placeholder.Hash(held),
		Subject: session.Subject, Session: name, Principal: session.Principal,
		Scopes: []string(request.GetGrantedScopes()), Extra: maps.Clone(session.Extra),
		EntryDigest: p.airlock.EntryDigest, ExpiresAt: expires,
	}
	if err := p.store.CreateResultGrant(ctx, grant); err != nil {
		return "", oauthstore.MintedGrant{}, err
	}
	return held, grant, nil
}

// grantResult writes the result grant of a result entry's run that asked for the proxy
// placeholder, in the exchange's open transaction, which ctx carries (writeResultGrant), and
// answers the body exchangeGrant writes once it has claimed the session name and committed, as
// grantApprove answers a person's approve trade: the placeholder as access_token, its type as
// issued_token_type, the token_type N_A of a token that is no access token (RFC 8693 Section
// 2.2.1), the seconds the grant lasts, the grant's id and the proxy release a command runs with
// (oauthwire.ExtraProxy). The library mints nothing for it, and the broker never calls its own
// public URL.
func (p *Provider) grantResult(ctx context.Context, request oauth2.Requester, session *oauthstore.ExchangeSession) (map[string]any, error) {
	if p.airlock == nil {
		return nil, oauth2.ErrServerError.WithHint("This broker's proxy binds no " + oauthwire.AirlockResultsName + ", so it grants no result entry's run a placeholder.")
	}
	held, grant, err := p.writeResultGrant(ctx, request, session)
	if err != nil {
		return nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	return map[string]any{
		"access_token":         held,
		"issued_token_type":    oauthwire.TokenTypeProxyPlaceholder,
		"token_type":           "N_A",
		"expires_in":           int64(time.Until(grant.ExpiresAt).Round(time.Second) / time.Second),
		oauthwire.ExtraGrantID: oauthwire.AirlockResultsWord + ":" + grant.ID,
		oauthwire.ExtraProxy: oauthwire.AirlockRelease{
			ProxyURL: p.airlock.ProxyURL, CACertificate: p.airlock.proxyRoots(), ProxyPassword: held,
			Names: map[string]oauthwire.AirlockName{oauthwire.AirlockResultsName: {
				Value: held, Slots: map[string]string{resultValueSlot: held}, Hosts: p.airlock.ResultHosts,
			}},
		},
	}, nil
}
