package duty

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock"
)

// ErrSessionClosed is FreeSessionLock's answer for a connection that was closed before its unlock
// was sent. The statement that closed it has reported its own failure to its caller. A closed
// connection cannot run an unlock, and Postgres drops the session's locks when the session ends,
// which can come a moment after the close, once pgx's own cleanup of the connection reaches it.
var ErrSessionClosed = errors.New("the connection was closed before the unlock; its locks go with its session")

// errLockNotHeld is pg_advisory_unlock answering false: the session did not hold the lock it was
// about to give back, so what that connection holds is unknown.
var errLockNotHeld = errors.New("the unlock reports this session did not hold the lock")

// FreeSessionLock gives back a session-level advisory lock taken on conn, and then conn itself.
// unlock is a statement that answers pg_advisory_unlock's boolean, run with args. conn goes back
// to its pool once Postgres answers true. Every session-level advisory lock Dispatch takes on a
// pooled connection is given back this way: a loop's lease (freeLeaseOn), a document room's lock
// (internal/dispatch/docs, lockRoom and a room's batch of browser updates, appendRoomTx), the
// files backfill's lock (internal/dispatch/files) and the migration runner's key
// (internal/dispatch/store, unlockMigrations). The migration runner gives it back through
// FreeSessionLockThenReset, which resets the connection's session between the unlock and the
// release so a migration's leftover setting does not reach the pool's next caller.
//
// The unlock runs under a LeaseCall of its own, never the holder's context, for two reasons. A
// lock must be given back after its holder's context has ended too. And an unbounded unlock on a
// half-open connection would hold its caller here, and everything queued behind that caller,
// until TCP gives the connection up.
//
// An unlock that fails, reaches that bound or answers false leaves what the session holds
// unknown. The pool would hand such a connection to its next caller, which could take a lock the
// session already holds and unlock it twice over. So the connection is destroyed instead of
// returned: it is hijacked out of the pool and closed under a LeaseCall of its own. Closing ends
// the session, and Postgres drops every lock the session held.
//
// The answer is nil once the unlock answered true, ErrSessionClosed when conn was closed before
// the unlock, and otherwise what failed, for the caller to log beside what the lock was for. conn
// is released or closed whatever the answer, so the caller does not use it again. Each bound runs
// on c (LeaseCall).
func FreeSessionLock(c clock.Clock, conn *pgxpool.Conn, unlock string, args ...any) error {
	return FreeSessionLockThenReset(c, conn, nil, unlock, args...)
}

// FreeSessionLockThenReset is FreeSessionLock with a reset step. reset runs on conn after the
// unlock has answered true and before conn goes back to its pool, under a LeaseCall of its own and
// the bound the unlock gets, on c as well. A caller gives conn back through this when the
// connection carries session state the pool's next caller must not meet: the migration runner's
// connection does, because a migration's own text can run a SET without LOCAL and its stored code
// can call set_config(…, false) (internal/pgmigrate, ResetSession).
//
// The order is fixed. The unlock runs first, then the reset. The migration runner's reset runs
// DISCARD ALL, which releases the session's advisory locks, so a reset before the unlock would
// leave pg_advisory_unlock nothing to free and it would answer false. A reset that fails leaves
// what the session carries unknown, exactly as a failed unlock does, so the connection is hijacked
// out of the pool and closed rather than returned, and the error comes back for the caller to log.
//
// reset is nil for a caller that needs no reset, which is how FreeSessionLock calls this.
func FreeSessionLockThenReset(c clock.Clock, conn *pgxpool.Conn, reset func(context.Context, *pgx.Conn) error, unlock string, args ...any) error {
	if conn.Conn().IsClosed() {
		conn.Release()
		return ErrSessionClosed
	}
	ctx, cancel := LeaseCall(c)
	defer cancel()
	freed := false
	err := conn.QueryRow(ctx, unlock, args...).Scan(&freed)
	if err == nil && !freed {
		err = errLockNotHeld
	}
	if err == nil && reset != nil {
		resetCtx, resetCancel := LeaseCall(c)
		defer resetCancel()
		err = reset(resetCtx, conn.Conn())
	}
	if err == nil {
		conn.Release()
		return nil
	}
	dead := conn.Hijack()
	closeCtx, closeCancel := LeaseCall(c)
	defer closeCancel()
	if closeErr := dead.Close(closeCtx); closeErr != nil {
		return fmt.Errorf("%w; close its connection: %v", err, closeErr)
	}
	return err
}
