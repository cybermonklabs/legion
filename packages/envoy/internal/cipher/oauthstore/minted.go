package oauthstore

import (
	"errors"
	"time"
)

// ErrMintedGrantNotLive is a minted kind's answer, from either read of one of its grants, for a
// grant no row holds and for one whose end the database's clock has passed.
var ErrMintedGrantNotLive = errors.New("oauthstore: no live minted grant")

// MintedGrant is one grant of a capability the broker mints and holds behind a dpop entry
// (requests.MintedKind): the decision the token exchange made, kept by the broker in place of the
// access token the caller would otherwise hold. Each minted kind keeps its grants in a table of its
// own, typed by what that kind is decided for, and answers each of them in this one shape, which
// is what the airlock reads and what a token minted for the grant carries. Every broker pod reads
// it, by its placeholder's SHA-256 when a proxy connection presents the placeholder and by its id
// when the proxy mints the grant's token, so the grant outlives the pod that made it.
type MintedGrant struct {
	// ID is the grant's uuid; on the wire and in the proxy's lines it is named by its kind's
	// word, a colon and this.
	ID string
	// PlaceholderSHA256 is the SHA-256, in hex, of the placeholder the caller holds, the only form
	// of it the broker keeps.
	PlaceholderSHA256 string
	// Subject, Session and Principal are the exchange's decision: the session's subject, the
	// session name its requests are recorded under, and the principal that name is claimed for.
	Subject, Session, Principal string
	// Scopes are the OAuth scopes the exchange granted.
	Scopes []string
	// Extra is the session's extra claims, which a token minted for the grant carries.
	Extra map[string]any
	// ExpiresAt is the grant's end: the database's now() past it, the grant reads as not live.
	ExpiresAt time.Time
	// EntryDigest is the digest of the dpop entry and the private destination the grant was made
	// under, 64 hex digits, which a broker compares with its own before it mints any token for the
	// grant. It is a column of its own, never a member of Extra, since Extra reaches the minted
	// token's introspection.
	EntryDigest string
}
