package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimByPriorBootTokenHash finds the claim one of whose previous launches minted the boot token
// with this hash, and that launch's generation: a shim of a generation the claim has left, which
// the worker stream tells to stop rather than reading as a token no launch minted (R3). The
// database keeps the hash in the statement whose write replaced it (putClaim's doc), whatever wrote
// it and however many launches ago, so it is known to a daemon that restarted after that write as
// well. A hash of the claim's current launch is ClaimByBootTokenHash's, and is not answered here.
func (s *Store) ClaimByPriorBootTokenHash(ctx context.Context, hash []byte) (supervise.Claim, uint64, bool, error) {
	var (
		token      string
		generation int64
	)
	err := s.pool.QueryRow(ctx, `select claim_token, generation from claim_boot_tokens where boot_token_hash = $1`, hash).
		Scan(&token, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, 0, false, nil
	}
	if err != nil {
		return supervise.Claim{}, 0, false, fmt.Errorf("read the launch of a previous boot token: %w", err)
	}
	c, err := scanClaim(s.pool.QueryRow(ctx, claimSelect+" where c.token = $1", token))
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, 0, false, nil
	}
	if err != nil {
		return supervise.Claim{}, 0, false, err
	}
	return c, uint64(generation), true, nil
}
