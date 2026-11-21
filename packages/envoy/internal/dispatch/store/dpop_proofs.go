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

// ErrUnstorableProof is a DPoP proof the replay store records nothing of, because its jti holds a
// control character (unicode.IsControl, U+0000 among them). PostgreSQL writes U+0000 in no text, so
// such an insert failed, and the library reported the failure as its server_error, which the
// broker-token check answered as an unreachable broker (503 BROKER_UNAVAILABLE) for a request whose
// only fault is the caller's own proof. The other control characters are refused with it, as the
// secrets broker's own replay store refuses them (its oauthstore.ErrUnstorableText). The API
// answers it 401 BROKER_TOKEN_PROOF_INVALID, the caller's refusal (api.brokerTokenActor).
var ErrUnstorableProof = errors.New("store: the DPoP proof's jti holds a control character, which the replay store does not keep")

// jtiRetentionMargin is how long past a DPoP proof's acceptance window its record is kept. The
// window's end is dated on the clock of whoever checked the proof and compared with this
// database's, so the margin covers the skew between the two; it is the broker's own margin for
// the same record (internal/broker/oauthstore).
const jtiRetentionMargin = time.Minute

// DPoPProofs is Dispatch's rfc9449.DPoPReplayStorage: the record of every RFC 9449 proof a
// request has already spent, so a captured proof replayed inside its acceptance window is refused
// (migration 0150). internal/brokertoken hands it to the library's resource check, and nothing
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
// key the interface requires is the key. Sweep deletes expired records. A jti holding a control
// character is ErrUnstorableProof, and records nothing.
func (d *DPoPProofs) CheckAndSetDPoPProofUsed(ctx context.Context, jti, _, _, _, htu string, exp time.Time) (bool, error) {
	if strings.IndexFunc(jti, unicode.IsControl) >= 0 {
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
