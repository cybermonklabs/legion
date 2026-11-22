package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimReach is one claim's reach record as the store holds it (PutClaimReach's column): what the
// operator routes that answer one claim show (internal/api's operatorAnswer). A machine's own copy
// is the record as it stood when the machine was built, since the reach keeper writes the record
// through this narrow column and never into a machine's claim. A token no claim holds answers the
// zero record, which is a claim not marked.
func (s *Store) ClaimReach(ctx context.Context, token claim.Token) (supervise.Reach, error) {
	var (
		reach   supervise.Reach
		encoded []byte
	)
	err := s.pool.QueryRow(ctx, `select reach from claims where token = $1`, string(token)).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Reach{}, nil
	}
	if err != nil {
		return supervise.Reach{}, fmt.Errorf("read the reach record of claim %s: %w", token, err)
	}
	if err := json.Unmarshal(encoded, &reach); err != nil {
		return supervise.Reach{}, fmt.Errorf("read the reach record of claim %s: decode it: %w", token, err)
	}
	return reach, nil
}

// PutClaimReach writes the reach column of token's claim row and nothing else: the daemon's record
// of whether Envoy can reach that claim's session (supervise.Reach, R5). It is no part of a claim's
// machine state — the daemon's minute sweep and the relaunches it starts write it between a machine's
// writes — so a machine's write never carries it (putClaim names no such column), as it carries no
// step and no activity report. Unlike those it is not of one launch: a relaunch keeps the claim's
// session, and the record is of that session, so a launch's write leaves it as it stands. The
// claim's last relaunch is the one fact of it the claim's own write carries instead
// (supervise.Claim.LastRelaunch), which this write never touches. It answers whether the store
// holds the claim.
func (s *Store) PutClaimReach(ctx context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
	encoded, err := json.Marshal(reach)
	if err != nil {
		return false, fmt.Errorf("put the reach record of claim %s: encode it: %w", token, err)
	}
	tag, err := s.pool.Exec(ctx, `update claims set reach = $2, updated_at = now() where token = $1`, string(token), encoded)
	if err != nil {
		return false, fmt.Errorf("put the reach record of claim %s: %w", token, err)
	}
	return tag.RowsAffected() == 1, nil
}
