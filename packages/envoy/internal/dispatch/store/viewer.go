package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// RequestRole is the role the request pool runs as (OpenRequestPool): NOLOGIN, NOBYPASSRLS, no
// superuser, owning nothing, granted to the migrating login with SET, so every policy holds it
// (plan D1, D7).
const RequestRole = "dispatch_app"

// ErrNoViewer is the request pool refusing a statement, before any SQL, under a context that
// carries neither a viewer (WithViewer) nor the system-work mark (WithSystemWork), and
// RequireViewer refusing a context with no viewer whatever its mark: a request path that lost its
// viewer fails loudly rather than reading no row, or reading as the owner (plan D2, D3).
var ErrNoViewer = errors.New("store: the request pool runs a statement only under a viewer (store.WithViewer) or, for the server's own work, the system-work mark (store.WithSystemWork), and this context carries neither")

// ErrRequestPoolRefuses is the request pool refusing a method that would send a statement outside
// the viewer's settings: a connection it hands out (Acquire, AcquireFunc, AcquireAllIdle,
// Listener, Dedicated), a copy (CopyFrom) and every side pool (plan D1, D2).
var ErrRequestPoolRefuses = errors.New("store: the request pool hands out no connection, copy or side pool, since a statement sent on one would run outside the viewer's settings; take it from the system pool")

// viewerKey is the context key WithViewer stores the viewer under.
type viewerKey struct{}

// systemWorkKey is the context key WithSystemWork marks the server's own work with.
type systemWorkKey struct{}

// WithViewer returns ctx carrying viewer, as access.Resolve answers it for the request's caller:
// each statement the request pool runs under ctx runs as dispatch_app with the viewer's settings
// (plan D2).
func WithViewer(ctx context.Context, viewer model.Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, viewer)
}

// ViewerFrom is the viewer ctx carries (WithViewer), and false when it carries none.
func ViewerFrom(ctx context.Context) (model.Viewer, bool) {
	viewer, ok := ctx.Value(viewerKey{}).(model.Viewer)
	return viewer, ok
}

// WithSystemWork marks ctx as the server's own work: under it, with no viewer, the request store's
// pool runs a statement on the system pool it holds; a viewer wins over the mark (plan D3).
func WithSystemWork(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemWorkKey{}, true)
}

// isSystemWork reports whether ctx carries the system-work mark (WithSystemWork).
func isSystemWork(ctx context.Context) bool {
	marked, _ := ctx.Value(systemWorkKey{}).(bool)
	return marked
}

// RequireViewer refuses (ErrNoViewer) a context that carries no viewer, whatever its mark: a read
// that must run under a viewer on a lifetime context, which the system-work mark would otherwise
// send to the owner (plan D3).
func RequireViewer(ctx context.Context) error {
	if _, ok := ViewerFrom(ctx); !ok {
		return ErrNoViewer
	}
	return nil
}

// viewerSettingsSelect is the viewer's part of the settings statement each transaction of a
// request pool begins with (plan D2), each setting local to the transaction: the projects the
// viewer sees, joined with commas; its person; whether it is open; its session; and whether the
// deployment holds a private project. The D2 reader functions (dispatch_viewer_sees and the rest)
// are the only readers of these names, and a transaction that set none of them reads each one's
// fail-closed default.
const viewerSettingsSelect = `set_config('dispatch.viewer_sees', $1, true), set_config('dispatch.viewer_person', $2, true), ` +
	`set_config('dispatch.viewer_open', $3, true), set_config('dispatch.viewer_session', $4, true), ` +
	`set_config('dispatch.viewer_private', $5, true)`

// requestSettingsSQL is the settings statement of a request pool (OpenRequestPool). It sets the
// role first, local to the transaction, so the transaction runs as RequestRole even on a connection
// whose session role something reset.
const requestSettingsSQL = `select set_config('role', '` + RequestRole + `', true), ` + viewerSettingsSelect

// ownerSettingsSQL is the settings statement of the test-only request pool that runs as the owner
// (OpenRequestPoolAsOwnerForTest): the same viewer settings, and no role.
const ownerSettingsSQL = `select ` + viewerSettingsSelect

// viewerSettings is one viewer's settings statement, as a request pool sends it first in each
// transaction it opens under that viewer.
type viewerSettings struct {
	sql  string
	args []any
}

// settingsFor is viewer's settings statement in the form sql, the request pool's (requestSettingsSQL
// or ownerSettingsSQL). It refuses a project key holding a comma, which viewer_sees, a list joined
// with commas, cannot carry: a project key is [A-Z][A-Z0-9]{1,9}, so no stored key holds one.
func settingsFor(sql string, viewer model.Viewer) (*viewerSettings, error) {
	for _, key := range viewer.Sees {
		if strings.Contains(key, ",") {
			return nil, fmt.Errorf("store: the viewer sees a project key %q holding a comma, which the viewer_sees setting cannot carry", key)
		}
	}
	return &viewerSettings{sql: sql, args: []any{
		strings.Join(viewer.Sees, ","),
		viewer.Person,
		strconv.FormatBool(viewer.Open),
		viewer.Session,
		strconv.FormatBool(viewer.AnyPrivate),
	}}, nil
}

// send sends the settings statement on tx, which has just begun.
func (s *viewerSettings) send(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
		return fmt.Errorf("store: send the viewer's settings: %w", err)
	}
	return nil
}

// beginOwn opens a transaction on p's own connections with options and, where settings are given,
// sends them before anything else, rolling the transaction back when they fail: the begin of a
// transaction Begin or BeginTx hands on, and of the transaction the pool opens of its own for one
// statement or batch, under a viewer or at a cut.
func (p *Pool) beginOwn(ctx context.Context, options pgx.TxOptions, settings *viewerSettings) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	if settings != nil {
		if err := settings.send(ctx, tx); err != nil {
			rollbackBounded(ctx, tx)
			return nil, err
		}
	}
	return tx, nil
}

// A request pool runs each statement of Query, QueryRow and Exec, and each batch of SendBatch,
// under a viewer in a transaction of its own: it opens the transaction, sends the viewer's
// settings, runs the statement or the batch, and commits once the rows end, the row is scanned,
// the statement returns or the batch closes; an error of the statement rolls it back
// (endStatement). It is the shape a read at a cut takes (pool_cut.go), with a commit for a
// rollback, since a statement under a viewer may write. The transaction is not one Begin opens, so
// it takes no BeforeCommit step: a keyed write's step commits with the transaction its writer
// opened, not with each statement the request sends beside it. A statement takes three round
// trips more than on the system pool: the begin, the settings and the commit.

// statementCommitTimeout bounds the commit that ends a statement's own transaction under a viewer
// (endStatement). The commit runs past the end of the caller's context, as a statement sent
// outside a transaction commits with the statement whatever the caller does next, so this bound
// is what keeps a connection that stopped answering from holding the caller for good.
const statementCommitTimeout = 5 * time.Second

// endStatement ends tx, the transaction of one statement or batch under a viewer: it commits where
// the statement succeeded (failed is nil) and answers the commit's error, and rolls back where it
// failed (rollbackBounded). The commit runs on a context that the end of ctx does not end, bounded
// by statementCommitTimeout: pgx closes the connection of a commit whose context is cancelled,
// which would cost the pool that connection and leave the statement's write unwritten after it
// returned.
func endStatement(ctx context.Context, tx pgx.Tx, failed error) error {
	if failed != nil {
		rollbackBounded(ctx, tx)
		return nil
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), statementCommitTimeout)
	defer cancel()
	return tx.Commit(commit)
}

// queryViewed is Query under a viewer: sql in a transaction of its own that sends settings first.
// The rows hold the transaction's connection, and the caller counts as holding it, until they
// end, as a cursor outside a transaction does (trackedRows); their end ends the transaction
// (statementRows).
func (p *Pool) queryViewed(ctx context.Context, settings *viewerSettings, sql string, args []any) (pgx.Rows, error) {
	tx, err := p.beginOwn(ctx, pgx.TxOptions{}, settings)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		rollbackBounded(ctx, tx)
		return nil, err
	}
	return &statementRows{Rows: rows, ctx: ctx, tx: tx, release: hold(ctx)}, nil
}

// statementRows is the rows of Query under a viewer. Their end - an explicit Close, a Next that
// runs out of rows, or a Scan or Values error, which pgx treats as fatal and closes the rows on -
// commits the statement's own transaction, or rolls it back where the rows end in an error, and
// then clears the caller's mark. A commit's error is the rows' error (Err), so a caller that
// reads every row and then checks Err learns that its statement's write did not land.
type statementRows struct {
	pgx.Rows
	ctx     context.Context
	tx      pgx.Tx
	release func()
	ended   bool
	err     error
}

func (r *statementRows) end() {
	if r.ended {
		return
	}
	r.ended = true
	r.err = endStatement(r.ctx, r.tx, r.Rows.Err())
	if r.release != nil {
		r.release()
	}
}

func (r *statementRows) Close() {
	r.Rows.Close()
	r.end()
}

func (r *statementRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.end()
	return false
}

func (r *statementRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.end()
	}
	return err
}

func (r *statementRows) Values() ([]any, error) {
	values, err := r.Rows.Values()
	if err != nil {
		r.end()
	}
	return values, err
}

func (r *statementRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.err
}

// queryRowViewed is QueryRow under a viewer: sql in a transaction of its own that sends settings
// first, which the row's Scan ends (statementRow).
func (p *Pool) queryRowViewed(ctx context.Context, settings *viewerSettings, sql string, args []any) pgx.Row {
	tx, err := p.beginOwn(ctx, pgx.TxOptions{}, settings)
	if err != nil {
		return errRow{err: err}
	}
	return statementRow{ctx: ctx, row: tx.QueryRow(ctx, sql, args...), tx: tx}
}

// statementRow is the row of QueryRow under a viewer. Its Scan ends the statement's own
// transaction once pgx's row has given the connection back to it: it commits where the statement
// ran, pgx.ErrNoRows included, since a statement that matched no row still ran, and answers the
// commit's error before ErrNoRows; it rolls back on any other error, and answers that error.
type statementRow struct {
	ctx context.Context
	row pgx.Row
	tx  pgx.Tx
}

func (r statementRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		rollbackBounded(r.ctx, r.tx)
		return err
	}
	if commitErr := endStatement(r.ctx, r.tx, nil); commitErr != nil {
		return commitErr
	}
	return err
}

// execViewed is Exec under a viewer: sql in a transaction of its own that sends settings first,
// committed once the statement returns.
func (p *Pool) execViewed(ctx context.Context, settings *viewerSettings, sql string, args []any) (pgconn.CommandTag, error) {
	tx, err := p.beginOwn(ctx, pgx.TxOptions{}, settings)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if commitErr := endStatement(ctx, tx, err); err == nil && commitErr != nil {
		return pgconn.CommandTag{}, commitErr
	}
	return tag, err
}

// sendBatchViewed is SendBatch under a viewer: batch in a transaction of its own that sends
// settings first, which the batch's Close ends (statementBatch). An empty batch sends nothing, so
// it goes to pgx as it is and opens no transaction. A batch that begins a transaction of its own
// (pgx.Batch.BeginTx) is refused by pgx inside this one.
func (p *Pool) sendBatchViewed(ctx context.Context, settings *viewerSettings, batch *pgx.Batch) pgx.BatchResults {
	if batch.Len() == 0 {
		return p.pool.SendBatch(ctx, batch)
	}
	tx, err := p.beginOwn(ctx, pgx.TxOptions{}, settings)
	if err != nil {
		return errBatchResults{err: err}
	}
	return &statementBatch{BatchResults: tx.SendBatch(ctx, batch), ctx: ctx, tx: tx}
}

// statementBatch is the results of SendBatch under a viewer. Its Close commits the batch's own
// transaction, or rolls it back where the batch failed, and answers the batch's first error or the
// commit's.
type statementBatch struct {
	pgx.BatchResults
	ctx    context.Context
	tx     pgx.Tx
	closed bool
	err    error
}

func (b *statementBatch) Close() error {
	if b.closed {
		return b.err
	}
	b.closed = true
	b.err = b.BatchResults.Close()
	if commitErr := endStatement(b.ctx, b.tx, b.err); b.err == nil {
		b.err = commitErr
	}
	return b.err
}

// AsOwner runs fn on tx as the table owner where tx runs as dispatch_app, and puts dispatch_app
// back before it returns, also when fn fails; on a transaction of any other role it runs fn and
// changes no role (plan D5).
//
// It reads current_user on tx. Where that is RequestRole (a request's transaction), it sets the
// role to none, local to the transaction, so the role falls back to the session user, the login
// that owns the tables; runs fn; sets RequestRole back, whatever fn answered; and reads
// current_user again. A switch back that does not leave tx as RequestRole fails the transaction:
// AsOwner rolls it back and answers the failure, so no later statement of the caller's runs as the
// owner. fn sees the transaction's own uncommitted writes and takes no second connection. A switch
// back that fails because fn left the transaction aborted answers fn's error with it: nothing
// runs in an aborted transaction but its rollback.
func AsOwner(ctx context.Context, tx pgx.Tx, fn func(tx pgx.Tx) error) error {
	var user string
	if err := tx.QueryRow(ctx, `select current_user::text`).Scan(&user); err != nil {
		return fmt.Errorf("store: read the role a rule's transaction runs as: %w", err)
	}
	if user != RequestRole {
		return fn(tx)
	}
	if _, err := tx.Exec(ctx, `select set_config('role', 'none', true)`); err != nil {
		return fmt.Errorf("store: run a rule as the owner: %w", err)
	}
	ruleErr := fn(tx)
	var back string
	if err := tx.QueryRow(ctx, `select set_config('role', $1, true), current_user::text`, RequestRole).Scan(nil, &back); err != nil {
		return errors.Join(ruleErr, fmt.Errorf("store: put %s back after a rule run as the owner: %w", RequestRole, err))
	}
	if back != RequestRole {
		rollbackBounded(ctx, tx)
		return errors.Join(ruleErr, fmt.Errorf("store: after a rule run as the owner the transaction runs as %q, not %s; it is rolled back", back, RequestRole))
	}
	return ruleErr
}
