package oauthstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ResultGrants is the table the results capability keeps its grants in, result_grants (migration
// 0047): each a CI run's result grant, the session the token exchange decided for a result entry's
// run, kept by the broker in place of the access token the run would otherwise hold. It answers
// each grant as a MintedGrant: Scopes are oauthwire.ScopeResults alone, Extra holds the run, the
// person, the project scope and the projects, and EntryDigest is the digest of the profile
// DISPATCH_RESULTS_TOKEN and the private destination of its host (config.MintedEntryDigest), which
// the column refuses unless it is 64 hex digits, so no grant holds an empty one. It is the results
// kind's requests.MintedGrants: every broker pod reads a grant, by its placeholder's SHA-256 when a
// proxy connection presents the placeholder and by its id when the proxy mints the run's results
// token, so the grant outlives the pod that made it.
type ResultGrants struct {
	store *Store
}

// ResultGrants is the results capability's table on this store.
func (s *Store) ResultGrants() ResultGrants { return ResultGrants{store: s} }

// CreateResultGrant writes grant into result_grants on the transaction ctx carries (the
// exchange's, which also claims the session name), or on the pool when it carries none.
func (s *Store) CreateResultGrant(ctx context.Context, grant MintedGrant) error {
	extra, err := json.Marshal(grant.Extra)
	if err != nil {
		return fmt.Errorf("oauthstore: encode the result grant's claims: %w", err)
	}
	if _, err := s.db(ctx).Exec(ctx, `insert into result_grants (id, placeholder_sha256, subject, session, principal, scopes, extra, entry_digest, expires_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		grant.ID, grant.PlaceholderSHA256, grant.Subject, grant.Session, grant.Principal, grant.Scopes, extra, grant.EntryDigest, grant.ExpiresAt); err != nil {
		return fmt.Errorf("oauthstore: write the result grant: %w", err)
	}
	return nil
}

// MintedByPlaceholder is the live result grant whose placeholder hashes to sha256Hex, or
// ErrMintedGrantNotLive. The database's clock decides whether it is live, in the query.
func (g ResultGrants) MintedByPlaceholder(ctx context.Context, sha256Hex string) (MintedGrant, error) {
	return g.live(ctx, `placeholder_sha256 = $1`, sha256Hex)
}

// MintedByID is the live result grant id names, or ErrMintedGrantNotLive, an id that is no uuid
// included. The database's clock decides whether it is live, in the query.
func (g ResultGrants) MintedByID(ctx context.Context, id string) (MintedGrant, error) {
	if _, err := uuid.Parse(id); err != nil {
		return MintedGrant{}, ErrMintedGrantNotLive
	}
	return g.live(ctx, `id = $1`, id)
}

// live reads the one result grant where names, if its end is after the database's now().
func (g ResultGrants) live(ctx context.Context, where string, arg string) (MintedGrant, error) {
	var grant MintedGrant
	var extra []byte
	err := g.store.db(ctx).QueryRow(ctx, `select id::text, placeholder_sha256, subject, session, principal, scopes, extra, entry_digest, expires_at
		from result_grants where `+where+` and expires_at > now()`, arg).
		Scan(&grant.ID, &grant.PlaceholderSHA256, &grant.Subject, &grant.Session, &grant.Principal, &grant.Scopes, &extra, &grant.EntryDigest, &grant.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MintedGrant{}, ErrMintedGrantNotLive
	}
	if err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: read the result grant: %w", err)
	}
	if err := json.Unmarshal(extra, &grant.Extra); err != nil {
		return MintedGrant{}, fmt.Errorf("oauthstore: decode the result grant's claims: %w", err)
	}
	return grant, nil
}
