package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A server process's identity is one uuid, minted when Open opens its pool, which every
// connection the process opens to Postgres carries at the end of its application_name, after one
// space: the shared pool's, every separate pool's (Pool.separate), the registry's (it copies the
// shared pool's configuration), and the dedicated connections (Pool.Dedicated), whose names end
// in the process id the document service takes from this identity (docs.Service.ProcessID). The
// events outbox holds the publisher hold under the same uuid (outbox.New), so the hold's row names
// the process whose sessions a standby that takes the hold over ends (EndSessionsOf).

// defaultApplicationName is the shared pool's application_name, before the identity, when the
// connection string names none.
const defaultApplicationName = "dispatch"

// maxApplicationName is the longest application_name Postgres keeps: it truncates a longer one
// to NAMEDATALEN - 1 bytes, which would cut the identity off its end.
const maxApplicationName = 63

// applicationName is base followed by one space and identity, with base written in printable
// ASCII and cut so the whole fits in maxApplicationName and the identity stays whole at its end.
// A nil identity (a pool NewPool wrapped) leaves base as it is.
//
// Postgres keeps an application_name in printable ASCII alone: it writes every other byte as \x
// and two hex digits (pg_clean_ascii, Postgres 16 and later) and then cuts the name to 63 bytes,
// so a base of non-ASCII letters taken as it is would grow past what this cut left room for and
// lose the identity's end, and EndSessionsOf would not find the session. The base is written in
// that same form here, before the cut, so the name Postgres shows is the name this builds, and the
// cut falls between two bytes' forms, never inside one.
func applicationName(base string, identity uuid.UUID) string {
	if identity == uuid.Nil {
		return base
	}
	suffix := " " + identity.String()
	room := maxApplicationName - len(suffix)
	var name strings.Builder
	for i := range len(base) {
		form := base[i : i+1]
		if base[i] < ' ' || base[i] > '~' {
			form = fmt.Sprintf(`\x%02x`, base[i])
		}
		if name.Len()+len(form) > room {
			break
		}
		name.WriteString(form)
	}
	return name.String() + suffix
}

// Identity is the identity every connection of this pool names at the end of its
// application_name: the one Open minted for it, shared by every separate pool opened from it
// (Outbox, TextTrees and the rest), or the one a pool NewPool wrapped names (identityNamedBy),
// and uuid.Nil for a pool whose connections name none.
func (p *Pool) Identity() uuid.UUID { return p.identity }

// identityNamedBy is the identity an application_name ends with, after one space, as
// applicationName writes it, or uuid.Nil when the name ends with none.
func identityNamedBy(name string) uuid.UUID {
	const width = 36
	if len(name) < width+1 || name[len(name)-width-1] != ' ' {
		return uuid.Nil
	}
	identity, err := uuid.Parse(name[len(name)-width:])
	if err != nil || identity.String() != name[len(name)-width:] {
		return uuid.Nil
	}
	return identity
}

// EndedSession is one database session EndSessionsOf ended: the backend's process id, its
// application_name, and whether pg_terminate_backend signalled it (false when the backend had
// already gone).
type EndedSession struct {
	PID             int32
	ApplicationName string
	Signalled       bool
}

// endSessionsOf ends every other session of this database whose application_name ends with one
// space and the identity $1. The identity is a whole uuid at the very end of the name, so neither
// another process's identity nor a name that merely begins with this one's matches. Every
// Dispatch process connects as one database role, which may signal its own backends.
const endSessionsOf = `
	select pid, coalesce(application_name, ''), pg_terminate_backend(pid)
	from pg_stat_activity
	where datname = current_database() and pid <> pg_backend_pid()
	  and right(application_name, 37) = ' ' || $1::text
	order by pid`

// EndSessionsOf ends (pg_terminate_backend) every session of this database that names identity at
// the end of its application_name, and returns each it ended. The backend of each ends at once,
// whatever its client is doing: a transaction it held open is rolled back and its locks are
// released, and its client's next read answers SQLSTATE 57P01 (SessionEnded). It refuses the nil
// identity, which names no process.
func (p *Pool) EndSessionsOf(ctx context.Context, identity uuid.UUID) ([]EndedSession, error) {
	if identity == uuid.Nil {
		return nil, errors.New("end the sessions of the nil identity: it names no process")
	}
	rows, err := p.Query(ctx, endSessionsOf, identity.String())
	if err != nil {
		return nil, fmt.Errorf("end the sessions of %s: %w", identity, err)
	}
	ended, err := pgx.CollectRows(rows, pgx.RowToStructByPos[EndedSession])
	if err != nil {
		return nil, fmt.Errorf("end the sessions of %s: %w", identity, err)
	}
	return ended, nil
}

// sessionEndedCode is SQLSTATE 57P01, admin_shutdown: what a backend answers its client when
// pg_terminate_backend, or a server shutdown, ended its session.
const sessionEndedCode = "57P01"

// SessionEndedCode and SessionEndedMessage are what a request whose database session was ended
// under it (SessionEnded) answers, from the API and the document socket alike: 503 with this code
// and message, and Retry-After: 1.
const (
	SessionEndedCode    = "DATABASE_SESSION_ENDED"
	SessionEndedMessage = "the database ended a session this request was using before the request finished, " +
		"and rolled back what that session had not committed: another server process took over this one's work, " +
		"or the database is shutting down. Read back what the request was to change, and make the request again if it is not there"
)

// SessionEnded reports whether err is a statement whose database session was ended under it
// (SQLSTATE 57P01): by a standby that took the publisher hold over from this process
// (EndSessionsOf), by an operator, or by the server shutting down. What the session had not
// committed was rolled back; the pools dial fresh connections for what comes next
// (onSessionEnded).
func SessionEnded(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sessionEndedCode
}

// onSessionEnded wraps next, a connection's handler of the errors Postgres sends it, so that the
// first connection of this process that meets an ended session (SessionEnded) has every pool of p
// drop the connections it holds (resetAll), and then does what next does: pgconn's default closes
// a connection on any FATAL error, as 57P01 is. A standby that takes the publisher hold over ends
// every session of this process at once, those idle in a pool too, and Postgres's answer to an
// idle one waits unread on its socket: without this, each of them would fail the next statement
// handed it, one request apiece, unless it had sat idle the second after which pgxpool pings a
// connection before handing it out. Every connection of p's pools carries the handler, since
// each separate pool is opened from a copy of the shared pool's configuration.
func (p *Pool) onSessionEnded(next pgconn.PgErrorHandler) pgconn.PgErrorHandler {
	return func(conn *pgconn.PgConn, pgErr *pgconn.PgError) bool {
		if pgErr.Code == sessionEndedCode {
			p.resetAll()
		}
		if next != nil {
			return next(conn, pgErr)
		}
		return !strings.EqualFold(pgErr.Severity, "FATAL")
	}
}

// resetAll has the shared pool and every separate pool opened from it destroy their idle
// connections at once and each connection in use when it is released (pgxpool.Pool.Reset), so
// the next caller of each is handed a connection dialled afresh.
func (p *Pool) resetAll() {
	p.mu.Lock()
	pools := p.pools()
	p.mu.Unlock()
	for _, pool := range pools {
		if pool != nil {
			pool.Reset()
		}
	}
}

// pools is the shared pool and every separate pool opened from it, each nil until it is opened.
// Its caller holds mu. Every pgx pool field of Pool is listed here: resetAll drops each one's
// connections, and TestAStoreWhoseSessionsWereEndedFailsOneStatementAndDialsAfresh fails for a
// field this leaves out.
func (p *Pool) pools() []*pgxpool.Pool {
	return []*pgxpool.Pool{p.pool, p.rooms, p.roomAppends, p.health, p.listen, p.leases, p.outbox, p.compaction, p.textTrees, p.horizon}
}
