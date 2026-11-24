package oauthstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrResultGrantNotLive is the answer of both reads of a result grant for a grant no row holds,
// and for one whose end the database's clock has passed.
var ErrResultGrantNotLive = errors.New("oauthstore: no live result grant")

// ResultGrant is a CI run's result grant (result_grants, migration 0042): the session the token
// exchange decided for a result entry's run, kept by the broker in place of the access token the
// run would otherwise hold. Every broker pod reads it, by its placeholder's SHA-256 when a proxy
// connection presents the placeholder and by its id when the proxy mints the run's results token,
// so the grant outlives the pod that made it.
type ResultGrant struct {
	// ID is the grant's uuid; on the wire and in the proxy's lines it is named
	// oauthwire.ResultGrantIDPrefix + ID.
	ID string
	// PlaceholderSHA256 is placeholder.Hash of the placeholder the run holds, the only form of it
	// the broker keeps.
	PlaceholderSHA256 string
	// Subject, Session and Principal are the exchange's decision: the session's subject, the
	// session name its writes are recorded under, and the principal that name is claimed for.
	Subject, Session, Principal string
	// Scopes are the OAuth scopes the exchange granted, oauthwire.ScopeResults alone.
	Scopes []string
	// Extra is the session's extra claims: the run, the person, the project scope and the
	// projects, which a token minted for the grant carries.
	Extra map[string]any
	// EntryDigest is the results entry the grant was made under: the hex SHA-256 of the profile
	// DISPATCH_RESULTS_TOKEN's fingerprint and the private destination's scheme and host, as the
	// granting broker computed it at boot (cmd/cipherd's resultEntryDigest). A broker pod serves
	// the grant only when its own digest is this one (requests.Machine.ResultEntryDigest); the
	// column refuses a digest that is not 64 hex digits, so no grant holds an empty one.
	EntryDigest string
	// ExpiresAt is the grant's end: the database's now() past it, the grant reads as not live and
	// the sweep deletes it.
	ExpiresAt time.Time
}

// CreateResultGrant writes grant on the transaction ctx carries (the exchange's, which also claims
// the session name), or on the pool when it carries none.
func (s *Store) CreateResultGrant(ctx context.Context, grant ResultGrant) error {
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

// ResultGrantByPlaceholder is the live grant whose placeholder hashes to sha256Hex, or
// ErrResultGrantNotLive. The database's clock decides whether it is live, in the query.
func (s *Store) ResultGrantByPlaceholder(ctx context.Context, sha256Hex string) (ResultGrant, error) {
	return s.liveResultGrant(ctx, `placeholder_sha256 = $1`, sha256Hex)
}

// ResultGrantByID is the live grant id names, or ErrResultGrantNotLive, an id that is no uuid
// included. The database's clock decides whether it is live, in the query.
func (s *Store) ResultGrantByID(ctx context.Context, id string) (ResultGrant, error) {
	if _, err := uuid.Parse(id); err != nil {
		return ResultGrant{}, ErrResultGrantNotLive
	}
	return s.liveResultGrant(ctx, `id = $1`, id)
}

// liveResultGrant reads the one grant where names, if its end is after the database's now().
func (s *Store) liveResultGrant(ctx context.Context, where string, arg string) (ResultGrant, error) {
	var grant ResultGrant
	var extra []byte
	err := s.db(ctx).QueryRow(ctx, `select id::text, placeholder_sha256, subject, session, principal, scopes, extra, entry_digest, expires_at
		from result_grants where `+where+` and expires_at > now()`, arg).
		Scan(&grant.ID, &grant.PlaceholderSHA256, &grant.Subject, &grant.Session, &grant.Principal, &grant.Scopes, &extra, &grant.EntryDigest, &grant.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultGrant{}, ErrResultGrantNotLive
	}
	if err != nil {
		return ResultGrant{}, fmt.Errorf("oauthstore: read the result grant: %w", err)
	}
	if err := json.Unmarshal(extra, &grant.Extra); err != nil {
		return ResultGrant{}, fmt.Errorf("oauthstore: decode the result grant's claims: %w", err)
	}
	return grant, nil
}
