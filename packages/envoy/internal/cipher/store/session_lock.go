package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LockSession takes the transaction lock of one session, until tx ends: the session name name
// under the principal carry, as enrollments.session_id and enrollments.carry_principal spell them.
// Two writers take it, and nothing else does:
//
//   - an enrollment's adoption (enroll.adopt), before it reads which enrollments of its session
//     it takes from, so an adoption reads its predecessors only once no operator's revoke of a
//     grant of the session is in flight;
//   - an operator's revoke (requests.Machine.RevokeByApprover), before it locks the session's
//     live and dormant enrollments and withholds the grant's names from each, so it waits for an
//     adoption in flight to commit, and then locks and withholds on the enrollment that adoption
//     made.
//
// So an enrollment of the session is either adopted after the revoke committed, and takes the
// withheld names off the boxes it adopts from (enroll.adopt's withheld clause), or committed before
// the revoke locked the session's enrollments, and is among them. Neither can miss the other.
//
// The key is the JSON array of "session", the name and the principal, hashed (hashtextextended), so
// no session's key is spelled as another's; two sessions whose hashes collide only wait for each
// other.
func LockSession(ctx context.Context, tx pgx.Tx, name, carry string) error {
	key, err := json.Marshal([]string{"session", name, carry})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, string(key)); err != nil {
		return fmt.Errorf("lock the session: %w", err)
	}
	return nil
}
