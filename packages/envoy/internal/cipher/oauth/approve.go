package oauth

import (
	"context"
	"maps"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// formRequestedTokenType is the request's requested_token_type (RFC 8693 Section 2.1), which the
// library reads too: absent, it is the access token.
const formRequestedTokenType = "requested_token_type"

// ApproveGrantMaxLifetime is the longest an approve grant lasts: long enough for a session to make
// the approvals one run of an agent needs, short enough that a placeholder a session holds opens
// the proxy for no longer than that.
const ApproveGrantMaxLifetime = 15 * time.Minute

// ApproveName and ApproveWord are the approve capability's name and word: the profile whose dpop
// block declares it and binds Dispatch's host, which is also the variable a session finds the
// placeholder in, and the word its placeholders open with (placeholder.MintedPrefix) and its grant
// ids carry before a colon on the wire.
const (
	ApproveName = "DISPATCH_APPROVE_TOKEN"
	ApproveWord = "approve"
)

// ApproveSettings is the approve capability (requests.MintedKind ApproveName) as the token exchange
// grants it: the digest of the dpop block and private destination every grant is made under
// (config.MintedEntryDigest), and what the session is handed beside its placeholder - where
// Cipher's proxy is, the certificate authority to trust it by, and the one host the capability's
// dpop block binds.
type ApproveSettings struct {
	EntryDigest   string
	ProxyURL      string
	CACertificate string
	Host          string
}

// approveGrantLifetime is how long an approve grant lasts: ApproveGrantMaxLifetime, or the access
// lifetime when that is shorter, so a grant never outlives the tokens this broker mints.
func (p *Provider) approveGrantLifetime() time.Duration {
	if p.accessLifetime > 0 && p.accessLifetime < ApproveGrantMaxLifetime {
		return p.accessLifetime
	}
	return ApproveGrantMaxLifetime
}

// grantApprove writes the approve grant of a trade that asked for the proxy placeholder, in the
// exchange's open transaction, which ctx carries, and answers the body exchangeGrant writes once
// it has claimed the session name and committed. Only a person's sign-in is granted it, and only
// by a broker that serves the approve capability: every other trade is refused invalid_request,
// the subject token being sound and the request asking for what its identity is not given. The
// grant holds the session the exchange decided for the sign-in - its subject, principal and extra
// claims, the person and the project scope person - less any projects, with the scope approve
// alone, the sign-in's credential, which ends it when revoked, and the capability's entry digest.
// Nobody approves it: the person's sign-in is its authority. It sets the session's access-token
// expiry to the grant's end, which the claim holds the session name oauthstore.SessionRetention
// past (claimSession). The body is the
// placeholder as access_token, its type as issued_token_type, the token_type N_A of a token that
// is no access token (RFC 8693 Section 2.2.1), the seconds the grant lasts, the grant's id and the
// proxy release a session runs with. The library mints nothing, and no approve-capable token ever
// leaves the broker.
func (p *Provider) grantApprove(ctx context.Context, session *oauthstore.ExchangeSession, subjectTokenType, credentialID string) (map[string]any, error) {
	if subjectTokenType != oauthwire.SubjectTokenTypePerson || p.approve == nil || credentialID == "" {
		return nil, oauth2.ErrInvalidRequest.WithHint("Only a person's sign-in is traded for the approve capability's proxy placeholder, on a broker that serves it.")
	}
	held, err := placeholder.Mint(placeholder.MintedPrefix(ApproveWord))
	if err != nil {
		return nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	lifetime := p.approveGrantLifetime()
	expires := time.Now().UTC().Add(lifetime).Truncate(time.Microsecond)
	extra := maps.Clone(session.Extra)
	delete(extra, ClaimProjects)
	sessionName, _ := extra[ClaimSession].(string)
	grant, err := p.store.CreateApproveGrant(ctx, oauthstore.MintedGrant{
		ID: uuid.NewString(), Subject: session.Subject, Session: sessionName, Principal: session.Principal,
		Scopes: []string{oauthwire.ScopeApprove}, Extra: extra, ExpiresAt: expires, EntryDigest: p.approve.EntryDigest,
	}, credentialID, held)
	if err != nil {
		return nil, oauth2.ErrServerError.WithWrap(err).WithDebugError(err)
	}
	// The session name is held oauthstore.SessionRetention past the grant's end, as a token's
	// claim holds it past the token's end (claimSession reads the session's access-token expiry).
	session.SetExpiresAt(oauth2.AccessToken, expires)
	return map[string]any{
		"access_token":         held,
		"issued_token_type":    oauthwire.TokenTypeProxyPlaceholder,
		"token_type":           "N_A",
		"expires_in":           int64(lifetime / time.Second),
		oauthwire.ExtraGrantID: ApproveWord + ":" + grant.ID,
		oauthwire.ExtraProxy: oauthwire.AirlockRelease{
			ProxyURL: p.approve.ProxyURL, CACertificate: p.approve.CACertificate, ProxyPassword: held,
			Names: map[string]oauthwire.AirlockName{ApproveName: {Value: held, Slots: map[string]string{"value": held}, Hosts: []string{p.approve.Host}}},
		},
	}, nil
}
