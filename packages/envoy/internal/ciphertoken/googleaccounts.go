package ciphertoken

import (
	"errors"
	"net/http"
	"sync"
)

// GoogleAccounts mints the Google identity token of each service account it is asked for, for one
// Audience, at IAM's generateIdToken with includeEmail and this pod's own access token, through a
// GoogleAccount of that account (ServiceAccount set). Every Read mints a token of its own: nothing
// is cached, and concurrent reads of one account are not joined into one mint, as GoogleAccount's
// own Read joins them. The broker's anthropic-federation minter reads through it
// (secrets.AnthropicFederation.IdentityToken), and each of its exchanges presents the identity
// token it read once and never again, so no two exchanges may hold the same one. It keeps one
// GoogleAccount per account, made on that account's first Read, so that account's access-token
// source serves every later mint. IAMCredentialsURL and HTTP are each GoogleAccount's.
type GoogleAccounts struct {
	Audience          string
	IAMCredentialsURL string
	HTTP              *http.Client

	mu      sync.Mutex
	readers map[string]*GoogleAccount
}

// errNoAccount is a Read that names no account: a GoogleAccount with no ServiceAccount reads this
// machine's own identity, which no caller of GoogleAccounts asks for.
var errNoAccount = errors.New("no service account to read the identity token of")

// Read is a new identity token of account for Audience, minted for this call alone
// (GoogleAccount.mint, whose errors it answers). An empty account is refused, and mints nothing.
func (g *GoogleAccounts) Read(account string) (string, error) {
	if account == "" {
		return "", errNoAccount
	}
	g.mu.Lock()
	reader, ok := g.readers[account]
	if !ok {
		if g.readers == nil {
			g.readers = map[string]*GoogleAccount{}
		}
		reader = NewGoogleAccount(g.Audience)
		reader.ServiceAccount, reader.IAMCredentialsURL, reader.HTTP = account, g.IAMCredentialsURL, g.HTTP
		g.readers[account] = reader
	}
	g.mu.Unlock()
	return reader.mint()
}
