package access

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrLockTimeout is an access write that gave up waiting for one of its locks within its
// transaction's lock_timeout (PostgreSQL's 55P03, lock_not_available), with nothing changed. Its
// error names the lock (LockOwnerRow or LockSlackChannel), since each is taken in a statement of its
// own.
var ErrLockTimeout = errors.New("nothing was changed; try again once the write that holds it ends")

// The locks an access write names when it does not take one in time: the project's owner lock,
// which orders every write of the project's row (lockProject), and the one lock every write of a
// project's Slack channels takes before it reads any project's list (lockSlackChannels).
const (
	LockOwnerRow     = "owner-row"
	LockSlackChannel = "slack-channel"
)

// lockNotTaken is ErrLockTimeout for one lock, wrapping the database's own error.
type lockNotTaken struct {
	lock string
	err  error
}

func (l lockNotTaken) Error() string {
	return "the " + l.lock + " lock was not taken in time: another write holds it; " + ErrLockTimeout.Error()
}

func (l lockNotTaken) Unwrap() []error { return []error{ErrLockTimeout, l.err} }

// lockTimedOut answers err as ErrLockTimeout naming lock when it is PostgreSQL's lock_not_available,
// the error a statement that waited past the transaction's lock_timeout fails with, and err as it is
// otherwise.
func lockTimedOut(lock string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return lockNotTaken{lock: lock, err: err}
	}
	return err
}
