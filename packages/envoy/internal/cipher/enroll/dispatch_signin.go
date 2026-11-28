package enroll

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DispatchSignInsActor is the actor RevokeDispatchSignIns records: the broker itself, ending every
// Dispatch sign-in for step 3 of rolling BROKER_DISPATCH_SIGNIN back, which no person decided, as
// ReuseActor is the broker's on finding a reused refresh token.
const DispatchSignInsActor = "broker:revoke-dispatch-signins"

// RevokeDispatchSignIns ends every live Dispatch sign-in, each launcher credential whose
// dispatch_scopes is not null and that no revoke has ended, expired or not, and answers how many it
// ended. It is the broker's own revoke, as RevokeReusedCredential is, and asks no person: each one
// is ended through endLiveCredential under DispatchSignInsActor, which revokes the credential and
// ends its OAuth tokens, so no token traded from the sign-in is live afterwards and no refresh of it
// is answered. A sign-in's credential enrolls nothing (AuthenticateLauncher), so it has no
// enrollment to end. It is step 3 of rolling BROKER_DISPATCH_SIGNIN back (cmd/cipherd's
// revoke-dispatch-signins), taken once no replica approves, refreshes or trades a sign-in, so
// nothing it ends is minted again behind it.
//
// Each sign-in is ended in a transaction of its own, taking its credential's row as a revoke does
// and reading it live again under that lock, so one a concurrent revoke ended first is left out of
// the count, and a failure part way leaves every sign-in ended before it ended: the count it answers
// beside an error is how many it ended. The ids are chosen on dispatch_scopes, which migration 0053
// makes write-once, so a credential chosen as a sign-in is one under the lock too.
func (s *Service) RevokeDispatchSignIns(ctx context.Context) (int, error) {
	rows, err := s.Store.Pool.Query(ctx, `select id::text from launcher_credentials
		where dispatch_scopes is not null and revoked_at is null order by id`)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	ended := 0
	for _, id := range ids {
		done, err := s.revokeDispatchSignIn(ctx, id)
		if err != nil {
			return ended, err
		}
		if done {
			ended++
		}
	}
	return ended, nil
}

// revokeDispatchSignIn ends Dispatch sign-in id in a transaction of its own (RevokeDispatchSignIns),
// answering false, and ending nothing, for one a revoke ended before its row's lock was taken.
func (s *Service) revokeDispatchSignIn(ctx context.Context, id string) (bool, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ended, err := endLiveCredential(ctx, tx, id, DispatchSignInsActor, "the broker revoked every Dispatch sign-in")
	if err != nil || !ended {
		return false, err
	}
	return true, tx.Commit(ctx)
}
