package oauthstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ApproveGrants is the approve capability's grants (approve_grants, migration 0046), read as
// Cipher's proxy reads every minted kind's grants (requests.MintedGrants): a grant is live while its
// end is after the database's now() and the person's sign-in it was made from - its launcher
// credential - is neither revoked nor past its expiry, so revoking the sign-in ends the grant.
type ApproveGrants struct {
	Store *Store
}

// placeholderSHA256 is the SHA-256, in hex, of the placeholder an approve grant's session holds,
// the only form of it the broker keeps, and the one requests reads a presented placeholder by.
func placeholderSHA256(placeholder string) string {
	sum := sha256.Sum256([]byte(placeholder))
	return hex.EncodeToString(sum[:])
}

// CreateApproveGrant writes grant, made from the sign-in whose launcher credential credentialID is,
// with the placeholder its session holds, on the transaction ctx carries (the exchange's, which
// also claims the session name), or on the pool when it carries none. grant.PlaceholderSHA256 is
// set from placeholder here.
func (s *Store) CreateApproveGrant(ctx context.Context, grant MintedGrant, credentialID, placeholder string) (MintedGrant, error) {
	grant.PlaceholderSHA256 = placeholderSHA256(placeholder)
	extra, err := json.Marshal(grant.Extra)
	if err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: encode the approve grant's claims: %w", err)
	}
	if _, err := s.db(ctx).Exec(ctx, `insert into approve_grants
		(id, placeholder_sha256, credential_id, subject, session, principal, scopes, extra, entry_digest, expires_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		grant.ID, grant.PlaceholderSHA256, credentialID, grant.Subject, grant.Session, grant.Principal, grant.Scopes, extra, grant.EntryDigest, grant.ExpiresAt); err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: write the approve grant: %w", err)
	}
	return grant, nil
}

// MintedByPlaceholder is the live approve grant whose placeholder hashes to sha256Hex, or
// ErrMintedGrantNotLive.
func (g ApproveGrants) MintedByPlaceholder(ctx context.Context, sha256Hex string) (MintedGrant, error) {
	return g.live(ctx, `a.placeholder_sha256 = $1`, sha256Hex)
}

// MintedByID is the live approve grant id names, or ErrMintedGrantNotLive, an id that is no uuid
// included.
func (g ApproveGrants) MintedByID(ctx context.Context, id string) (MintedGrant, error) {
	if _, err := uuid.Parse(id); err != nil {
		return MintedGrant{}, ErrMintedGrantNotLive
	}
	return g.live(ctx, `a.id = $1`, id)
}

// live reads the one grant where names, if its end is after the database's now() and its sign-in's
// credential is live.
func (g ApproveGrants) live(ctx context.Context, where, arg string) (MintedGrant, error) {
	var grant MintedGrant
	var extra []byte
	err := g.Store.db(ctx).QueryRow(ctx, `select a.id::text, a.placeholder_sha256, a.subject, a.session, a.principal, a.scopes, a.extra, a.expires_at, a.entry_digest
		from approve_grants a join launcher_credentials c on c.id = a.credential_id
		where `+where+` and a.expires_at > now() and c.revoked_at is null and c.expires_at > now()`, arg).
		Scan(&grant.ID, &grant.PlaceholderSHA256, &grant.Subject, &grant.Session, &grant.Principal, &grant.Scopes, &extra, &grant.ExpiresAt, &grant.EntryDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return MintedGrant{}, ErrMintedGrantNotLive
	}
	if err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: read the approve grant: %w", err)
	}
	if err := json.Unmarshal(extra, &grant.Extra); err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: decode the approve grant's claims: %w", err)
	}
	return grant, nil
}
