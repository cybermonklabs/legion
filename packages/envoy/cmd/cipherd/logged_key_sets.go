package main

import (
	"context"
	"log/slog"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/oidc"
)

// The lines the broker writes about every key set it caches for its callers' tokens: each
// cluster issuer's (loggedIssuers), Google's, and the token exchange's GitHub issuer's (logKeySet).
// Each says that a read found the key set other than the read before it found it
// (oidc.Keys.OnChange), so a key set that cannot be read for a day writes one line, however many
// tokens and background refreshes read it meanwhile, and a key the cached set still answers
// changes nothing. Each names the issuer as issuerAttrs does (name= for a named cluster issuer,
// and url=) and the key set's address (keys=).
const (
	// keySetUnreadableMessage is logged, at WARN, when a read of a key set fails and the read
	// before it succeeded, or there was none, followed by error=, the read's error, which names
	// the key set's address and never a token.
	keySetUnreadableMessage = "broker: key set cannot be read"
	// keySetReadableMessage is logged, at INFO, when a read of a key set succeeds after one that
	// failed.
	keySetReadableMessage = "broker: key set can be read again"
)

// logKeySetChange logs the change a read found in keys, the key set of issuer: it cannot be read
// where err is a read's error, and can be read again where err is nil (oidc.Keys.OnChange).
func logKeySetChange(issuer oidc.NamedIssuer, keys string, err error) {
	attrs := append(issuerAttrs(issuer), "keys", keys)
	if err != nil {
		slog.Warn(keySetUnreadableMessage, append(attrs, "error", err)...)
		return
	}
	slog.Info(keySetReadableMessage, attrs...)
}

// logKeySet has keys, the key set of the issuer at issuer, log each change its reads find
// (logKeySetChange), and answers it. The broker reads Google's key set and the GitHub issuer's
// through it; the clusters' issuers log theirs through their set (newLoggedIssuers).
func logKeySet(keys *oidc.Keys, issuer string) *oidc.Keys {
	keys.OnChange = func(err error) { logKeySetChange(oidc.NamedIssuer{URL: issuer}, keys.URL(), err) }
	return keys
}

// githubExchangeIssuer is the token exchange's GitHub issuer: BROKER_EXCHANGE_GITHUB_ISSUER, for
// BROKER_EXCHANGE_GITHUB_AUDIENCE, with its key set read through keysFor (issuerKeys in
// production) and logged (logKeySet).
func githubExchangeIssuer(ctx context.Context, cfg config.Config, keysFor keysFunc) (oauth.IssuerConfig, error) {
	keys, err := keysFor(ctx, cfg.ExchangeGitHubIssuer)
	if err != nil {
		return oauth.IssuerConfig{}, err
	}
	return oauth.IssuerConfig{
		Issuer: cfg.ExchangeGitHubIssuer, Audience: cfg.ExchangeGitHubAudience,
		Keys: logKeySet(keys, cfg.ExchangeGitHubIssuer),
	}, nil
}
