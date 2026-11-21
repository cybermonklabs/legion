package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// ErrUnstorableProof is a DPoP proof the replay store records nothing of, because the row it would
// write cannot be stored: its jti holds a control character (unicode.IsControl, U+0000 among them),
// or its jti and htu together are longer than maxStoredProofKey. PostgreSQL writes U+0000 in no
// text, and refuses a key too long for the primary key's btree (SQLSTATE 54000); either way the
// insert failed, and the library reported the failure as its server_error, which the broker-token
// check answered as an unreachable broker (503 BROKER_UNAVAILABLE) for a request whose only fault
// is the caller's own proof or the length of its own path. The secrets broker's own replay store
// refuses the same proofs (internal/cipher/oauthstore's ErrUnstorableText). The API answers it 401
// BROKER_TOKEN_PROOF_INVALID, the caller's refusal (api.brokerTokenActor).
var ErrUnstorableProof = errors.New("store: the DPoP proof's jti holds a control character, or its jti and target URI are too long to record")

// maxStoredProofKey bounds the bytes of a recorded proof's jti and htu together. The primary key
// (jti, htu) is a btree, whose version 4 takes an index tuple of at most 2,704 bytes on an 8 kB page
// (the refusal names that limit), and the tuple adds to the two texts at most 26 bytes: its 8-byte
// header, a 4-byte length header for each column, up to 3 bytes aligning the second column and up
// to 7 rounding the tuple up to 8. 2,048 leaves that and more, and it is far above what a client's
// request needs: the library caps jti at 255 characters, and htu is the request's own URL without
// its query.
const maxStoredProofKey = 2048

// jtiRetentionMargin is how long past a DPoP proof's acceptance window its record is kept. The
// window's end is dated on the clock of whoever checked the proof and compared with this
// database's, so the margin covers the skew between the two; it is the broker's own margin for
// the same record (internal/cipher/oauthstore).
const jtiRetentionMargin = time.Minute

// DPoPProofs is Dispatch's rfc9449.DPoPReplayStorage: the record of every RFC 9449 proof a
// request has already spent, so a captured proof replayed inside its acceptance window is refused
// (migration 0150). internal/ciphertoken hands it to the library's resource check, and nothing
// else writes the table.
type DPoPProofs struct {
	pool *Pool
}

// NewDPoPProofs is the replay store on pool.
func NewDPoPProofs(pool *Pool) *DPoPProofs { return &DPoPProofs{pool: pool} }

// CheckAndSetDPoPProofUsed records a proof as used until exp plus jtiRetentionMargin, keyed on its
// jti and normalized target URI (htu), and reports whether it already was. The insert, or the
// takeover of an expired record of the same key, is one statement, so two requests presenting one
// proof at the same moment cannot both see it unused. jkt, nonce and htm are left out of the key:
// widening it only ever admits presentations that are no replay, and this store serves one
// resource server whose proofs all carry an 'ath' the library has already checked, so the narrow
// key the interface requires is the key. Sweep deletes expired records. A proof whose jti holds a
// control character, or whose jti and htu are over maxStoredProofKey bytes together, is
// ErrUnstorableProof, and records nothing.
func (d *DPoPProofs) CheckAndSetDPoPProofUsed(ctx context.Context, jti, _, _, _, htu string, exp time.Time) (bool, error) {
	if strings.IndexFunc(jti, unicode.IsControl) >= 0 || len(jti)+len(htu) > maxStoredProofKey {
		return false, ErrUnstorableProof
	}
	var recorded bool
	err := d.pool.QueryRow(ctx, `insert into dpop_proofs (jti, htu, expires_at) values ($1, $2, $3)
		on conflict (jti, htu) do update set expires_at = excluded.expires_at
		where dpop_proofs.expires_at <= now()
		returning true`, jti, htu, exp.Add(jtiRetentionMargin)).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("record DPoP proof: %w", err)
	}
	return false, nil
}

// Sweep deletes every record whose proof can no longer be presented, on the expiry index, and
// answers how many it deleted. A record lives for the proof's acceptance window and a minute, so
// what one pass finds is the proofs of the last few minutes rather than a day's worth; the
// maintenance pass runs it beside the idempotency-key sweep (api.RunMaintenanceSweep).
func (d *DPoPProofs) Sweep(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `delete from dpop_proofs where expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("sweep DPoP proofs: %w", err)
	}
	return tag.RowsAffected(), nil
}
