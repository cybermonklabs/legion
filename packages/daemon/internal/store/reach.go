package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

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
// machine state — the daemon's minute sweep and the `relaunch` route write it between a machine's
// writes — so a machine's write never carries it (putClaim names no such column), as it carries no
// step and no activity report. Unlike those it is not of one launch: a relaunch keeps the claim's
// session, and the record is of that session, so a launch's write leaves it as it stands. The
// claim's last relaunch is the one fact of it the claim's own write carries instead
// (supervise.Claim.LastRelaunch), which this write never touches. It answers whether the store
// holds the claim.
func (s *Store) PutClaimReach(ctx context.Context, token claim.Token, reach supervise.Reach) (bool, error) {
	return putClaimReach(ctx, s.pool, token, reach)
}

// PutClaimReachTx is PutClaimReach through the caller's transaction: the transaction of a notice
// the record marks told, so the notice and the mark commit together or not at all, and a daemon
// that ends after the commit never tells it again (internal/daemon's reach keeper).
func PutClaimReachTx(ctx context.Context, tx pgx.Tx, token claim.Token, reach supervise.Reach) (bool, error) {
	return putClaimReach(ctx, tx, token, reach)
}

func putClaimReach(ctx context.Context, db execer, token claim.Token, reach supervise.Reach) (bool, error) {
	encoded, err := json.Marshal(reach)
	if err != nil {
		return false, fmt.Errorf("put the reach record of claim %s: encode it: %w", token, err)
	}
	tag, err := db.Exec(ctx, `update claims set reach = $2, updated_at = now() where token = $1`, string(token), encoded)
	if err != nil {
		return false, fmt.Errorf("put the reach record of claim %s: %w", token, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReachProjectNotice is the project's own record of a controller notice that speaks for no claim's
// reach record (R3, R5): the reach keeper's notice that Envoy lists no live session, one for the
// project, and the lock-out notices' notice of a claim's newer process locked out by an older one,
// one for each claim (the table reach_project_notices). A claim's own reach notice has its told
// mark in the claim's reach record (PutClaimReachTx); these have no such record, so they have this
// one.
//
// Told is whether the controller's notice was queued, written in that notice's own transaction
// (PutReachProjectNoticeTx), so a daemon that ends after it never tells it again. A lock-out
// notice the controller's lane did not queue is written with Told false: it is kept, for the next
// daemon to tell while its worker stream still refuses the same newer process. Kind is the
// notice's kind, Claim the claim a lock-out is of ("" for the no-live notice), Newer and Older
// the two generations a lock-out names, Issue the issue the notice is queued on, and Notice the
// notice as the controller is told it, encoded.
type ReachProjectNotice struct {
	Project string
	Kind    string
	Claim   claim.Token
	Newer   uint64
	Older   uint64
	Issue   string
	Notice  []byte
	Told    bool
}

// ReachProjectNotices is every record of project's own reach notices.
func (s *Store) ReachProjectNotices(ctx context.Context, project string) ([]ReachProjectNotice, error) {
	rows, err := s.pool.Query(ctx, `select kind, claim_token, newer, older, issue, notice, told_at is not null
		from reach_project_notices where project = $1 order by kind, claim_token`, project)
	if err != nil {
		return nil, fmt.Errorf("read the reach notices of %s: %w", project, err)
	}
	defer rows.Close()
	var notices []ReachProjectNotice
	for rows.Next() {
		n := ReachProjectNotice{Project: project}
		var token string
		var newer, older int64
		if err := rows.Scan(&n.Kind, &token, &newer, &older, &n.Issue, &n.Notice, &n.Told); err != nil {
			return nil, fmt.Errorf("read the reach notices of %s: %w", project, err)
		}
		n.Claim, n.Newer, n.Older = claim.Token(token), uint64(newer), uint64(older)
		notices = append(notices, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the reach notices of %s: %w", project, err)
	}
	return notices, nil
}

// PutReachProjectNotice writes n in a statement of its own: a lock-out notice kept, untold.
func (s *Store) PutReachProjectNotice(ctx context.Context, n ReachProjectNotice) error {
	return putReachProjectNotice(ctx, s.pool, n)
}

// PutReachProjectNoticeTx writes n through the caller's transaction: the transaction of the
// notice it marks told, so the notice and its record commit together or not at all.
func PutReachProjectNoticeTx(ctx context.Context, tx pgx.Tx, n ReachProjectNotice) error {
	return putReachProjectNotice(ctx, tx, n)
}

// DeleteReachProjectNotice deletes the record of project's notice of kind, of claim token ("" for
// the no-live notice): a read that found a live session ends the no-live run it was told of, and
// a lock-out that ended untold leaves nothing to keep.
func (s *Store) DeleteReachProjectNotice(ctx context.Context, project, kind string, token claim.Token) error {
	if _, err := s.pool.Exec(ctx, `delete from reach_project_notices where project = $1 and kind = $2 and claim_token = $3`,
		project, kind, string(token)); err != nil {
		return fmt.Errorf("delete the %s notice record of %s: %w", kind, project, err)
	}
	return nil
}

func putReachProjectNotice(ctx context.Context, db execer, n ReachProjectNotice) error {
	if n.Newer > math.MaxInt64 || n.Older > math.MaxInt64 {
		return fmt.Errorf("put the %s notice record of %s: a generation does not fit a bigint", n.Kind, n.Project)
	}
	if _, err := db.Exec(ctx, `insert into reach_project_notices (project, kind, claim_token, newer, older, issue, notice, told_at)
		values ($1, $2, $3, $4, $5, $6, $7, case when $8::boolean then now() end)
		on conflict (project, kind, claim_token) do update set newer = excluded.newer, older = excluded.older,
			issue = excluded.issue, notice = excluded.notice, told_at = excluded.told_at`,
		n.Project, n.Kind, string(n.Claim), int64(n.Newer), int64(n.Older), n.Issue, n.Notice, n.Told); err != nil {
		return fmt.Errorf("put the %s notice record of %s: %w", n.Kind, n.Project, err)
	}
	return nil
}
