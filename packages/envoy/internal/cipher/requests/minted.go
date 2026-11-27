// packages/envoy/internal/cipher/requests/minted.go
// minted.go is the airlock's side of a capability the broker mints and holds: a grant the token
// exchange writes in place of an access token, which the caller holds only as a placeholder, and
// which the broker's proxy turns into a token of its own pod's key for each request the kind's dpop
// entry allows (internal/cipher/proxy's mintedTransport). Each kind keeps its grants in a table of
// its own (MintedGrants); what is here is how a proxy connection's placeholder and the proxy's read
// of a grant's values reach that table, and the guards every kind's grant is held to before any
// token is minted for it.
package requests

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// MintedGrants reads one minted kind's grants from the table the kind keeps them in: by the
// placeholder's hash, for a proxy connection that presents it, and by id, for the proxy's read of
// its values. Each answers oauthstore.ErrMintedGrantNotLive for a grant no row holds and for one
// whose end the database's clock has passed.
type MintedGrants interface {
	MintedByPlaceholder(ctx context.Context, sha256Hex string) (oauthstore.MintedGrant, error)
	MintedByID(ctx context.Context, id string) (oauthstore.MintedGrant, error)
}

// MintedTokens mints a minted grant's access token for this broker pod, bound to the pod key whose
// RFC 7638 thumbprint jkt is, and ending with the grant. *oauth.Provider is one.
type MintedTokens interface {
	MintGrantToken(ctx context.Context, grant oauthstore.MintedGrant, jkt string) (string, error)
}

// MintedKind is one capability the broker mints and holds behind a rules entry of kind dpop.
type MintedKind struct {
	// Name is the airlock name its grants hold: the profile whose dpop proxy block binds the
	// kind's host, and the variable a command finds the placeholder in.
	Name string
	// Word is the kind's word: its grants' placeholders open with placeholder.MintedPrefix(Word),
	// and their ids on the wire and in the proxy's lines with Word and a colon.
	Word string
	// Grants is the table the kind keeps its grants in.
	Grants MintedGrants
	// EntryDigest is this broker's digest of the kind's dpop entry and the private destination
	// (MintedEntryDigest). A grant made under another digest, by a broker with another entry or
	// destination on the same database, is no grant of this broker's, and "" serves no grant.
	EntryDigest string
}

// errMintedEntryMoved is the cause beside ErrGrantNotLive of a grant made under another entry or
// private destination than this broker's.
var errMintedEntryMoved = errors.New("the grant was made under another dpop entry or private destination than this broker's")

// mintedKindOfPlaceholder is the kind whose placeholder presented is, and whether it is a minted
// grant's placeholder at all: one whose text after placeholder.Prefix holds a hyphen, which no
// placeholder of a secret's release does. A minted placeholder whose word no kind of this broker
// has answers ok with a nil kind, so it is refused rather than read as a release's.
func (m *Machine) mintedKindOfPlaceholder(presented string) (kind *MintedKind, ok bool) {
	rest, prefixed := strings.CutPrefix(presented, placeholder.Prefix)
	if !prefixed {
		return nil, false
	}
	word, _, minted := strings.Cut(rest, "-")
	if !minted {
		return nil, false
	}
	return m.mintedKind(word), true
}

// mintedKindOfGrant is the kind whose grant grantID names and the grant's own id, and whether it
// is a minted grant's id at all: Word, a colon and the id. A secret grant's id is a uuid, which
// holds no colon. A minted id whose word no kind of this broker has answers ok with a nil kind.
func (m *Machine) mintedKindOfGrant(grantID string) (kind *MintedKind, id string, ok bool) {
	word, id, minted := strings.Cut(grantID, ":")
	if !minted {
		return nil, "", false
	}
	return m.mintedKind(word), id, true
}

func (m *Machine) mintedKind(word string) *MintedKind {
	for i := range m.Minted {
		if m.Minted[i].Word == word {
			return &m.Minted[i]
		}
	}
	return nil
}

// liveMinted is the grant read answered as AirlockConnection and AirlockValues answer it: a grant
// no row holds, or one whose end has passed, is ErrGrantNotLive; and so is one made under another
// entry digest than the kind's on this broker, or any grant of a kind this broker serves under no
// digest, before anything is minted for it.
func (kind *MintedKind) liveMinted(grant oauthstore.MintedGrant, err error) (oauthstore.MintedGrant, error) {
	if errors.Is(err, oauthstore.ErrMintedGrantNotLive) {
		return oauthstore.MintedGrant{}, ErrGrantNotLive
	}
	if err != nil {
		return oauthstore.MintedGrant{}, err
	}
	if kind.EntryDigest == "" || grant.EntryDigest != kind.EntryDigest {
		return oauthstore.MintedGrant{}, fmt.Errorf("%w: %w", ErrGrantNotLive, errMintedEntryMoved)
	}
	return grant, nil
}

// mintedConnection is AirlockConnection for a minted grant's placeholder: the grant read from its
// kind's table by the placeholder's hash and from nowhere else, so an ended grant's placeholder
// never falls through to a secret grant's placeholders. The connection is the grant's: its id as
// the kind's word, a colon and the uuid, its session, its end, the Minted mark, and the kind's one
// name with its one slot, whose placeholder is the one presented. It belongs to no enrollment, so
// the enrollment id is empty, and the proxy names its calls by the grant alone.
func (m *Machine) mintedConnection(ctx context.Context, kind *MintedKind, presented string) (AirlockConnection, error) {
	if kind == nil || kind.Grants == nil {
		return AirlockConnection{}, ErrGrantNotLive
	}
	grant, err := kind.liveMinted(kind.Grants.MintedByPlaceholder(ctx, placeholderHash(presented)))
	if err != nil {
		return AirlockConnection{}, err
	}
	expires := grant.ExpiresAt
	return AirlockConnection{
		GrantID: kind.Word + ":" + grant.ID, SessionID: grant.Session, Minted: true, Expires: &expires,
		Names: map[string]AirlockGrantName{kind.Name: {Slots: map[string]string{SlotValue: presented}}},
	}, nil
}

// mintedValues is AirlockValues for a minted grant, id its uuid: the grant's token, which this pod
// mints for the grant now (MintedTokens), bound to the pod's own key (MintedKey), and which no
// table and no other pod holds. Its Expires is zero: the token ends with the grant, and the grant's
// end is what the proxy's read of the connection checks on every request, so a value the proxy
// holds needs no clock of its own. It answers beside it the policy version the proxy holds values
// against, which a minted grant does not read: a policy change makes the pod mint again, which
// costs one token and changes nothing of the grant.
func (m *Machine) mintedValues(ctx context.Context, kind *MintedKind, id string) (map[string]AirlockSecret, string, error) {
	if kind == nil || kind.Grants == nil || m.MintedTokens == nil || m.MintedKey == "" {
		return nil, "", ErrGrantNotLive
	}
	grant, err := kind.liveMinted(kind.Grants.MintedByID(ctx, id))
	if err != nil {
		return nil, "", err
	}
	token, err := m.MintedTokens.MintGrantToken(ctx, grant, m.MintedKey)
	if err != nil {
		return nil, "", err
	}
	return map[string]AirlockSecret{kind.Name: {Slots: map[string]string{SlotValue: token}}}, m.PolicyVersion(), nil
}
