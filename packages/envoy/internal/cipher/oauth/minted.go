package oauth

import (
	"context"
	"errors"
	"maps"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// MintGrantToken mints the token this broker pod puts into each allowed request of a grant of a
// capability the broker mints and holds (requests.MintedKind, the airlock proxy's mintedTransport):
// an access token of the exchange client holding the grant's decision whole - its principal as
// subject and username, its granted scopes and its extra claims - bound to the pod key whose RFC
// 7638 thumbprint jkt is, and ending when the grant ends, so a token the broker no longer serves is
// no longer live at introspection. It is stored as every access token is, by its signature, so the
// token's text is in no table, and it is minted on the pod that serves the request: no other pod
// holds the key it is bound to, so a token minted elsewhere could not be used there.
func (p *Provider) MintGrantToken(ctx context.Context, grant oauthstore.MintedGrant, jkt string) (string, error) {
	if jkt == "" {
		return "", errors.New("oauth: a minted grant's token is minted only bound to the pod's DPoP key")
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
