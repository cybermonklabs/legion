package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// TxPool is what Transact opens its transaction on and reads a lost commit's outcome through.
// The daemon's guarded pool (*Pool), a pgx pool and intake's Pool all satisfy it.
type TxPool interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrCommitOutcomeUnknown is Transact's answer for a commit whose reply was lost while the store
// could not say whether the transaction committed. A warning names the transaction, and its
// hooks wait for the store's answer (watchOutcome).
var ErrCommitOutcomeUnknown = errors.New("the store did not say whether the transaction committed")

const (
	// commitReplyBound is how long a commit waits for its reply after its caller's context ends.
	commitReplyBound = 1 * time.Second
	// outcomeReadBound is how long one read of a transaction's outcome may take.
	outcomeReadBound = 1 * time.Second
	// outcomePollStep is how often a read asks again while the store says the transaction is in
	// progress.
	outcomePollStep = 50 * time.Millisecond
	// outcomeRetryFirst is how long the watcher waits before its first read; each wait after it
	// doubles, up to outcomeRetryMax.
	outcomeRetryFirst = 1 * time.Second
	outcomeRetryMax   = 30 * time.Second
)

// CommitSettleBound is the longest Transact waits, after its caller's context has ended, to learn
// a sent commit's outcome: commitReplyBound for the reply, then outcomeReadBound for the store's
// answer. It is counted from the end of the caller's context, not from the commit's start, and it
// holds no time for the hooks themselves. A caller whose context never ends has no deadline on the
// reply.
const CommitSettleBound = commitReplyBound + outcomeReadBound

// commitHooksKey is the context key a Transact scope's hook list lives under.
type commitHooksKey struct{}

// withCommitHooks scopes OnCommit to one transaction: the hooks registered on the returned
// context are appended to the returned list. Transact is its only caller, so a hook waits only on
// a commit whose outcome Transact reads (TestOnlyTransactOpensACommitScope).
func withCommitHooks(ctx context.Context) (context.Context, *[]func()) {
	hooks := &[]func(){}
	return context.WithValue(ctx, commitHooksKey{}, hooks), hooks
}

// OnCommit runs fn once the transaction of ctx's Transact has committed. Handler state kept in
// memory, and the journal line saying what a write did, must never claim more than what
// committed: a later step's failure rolls the transaction back, and a write retried after a
// failed commit would say it twice.
//
// A hook runs once, and only if its transaction committed. It usually runs on the owner's
// goroutine before Transact returns. When the commit's outcome was unknown, it runs later, on the
// watcher's goroutine: by then later transactions may have run their own hooks, and the daemon may
// be stopping. So a hook must be safe to run late: it states what committed, or applies an effect
// that no later state makes wrong. Work that is not safe to run late stays after Transact
// returns, where its own retry derives it again.
//
// The hook list is not goroutine-safe: OnCommit is called only on the goroutine that runs
// Transact's write. It panics on a context no Transact scopes, since a write made outside
// Transact has no commit to wait for.
func OnCommit(ctx context.Context, fn func()) {
	hooks, ok := ctx.Value(commitHooksKey{}).(*[]func())
	if !ok {
		panic("store.OnCommit outside a transaction's commit scope (store.Transact)")
	}
	*hooks = append(*hooks, fn)
}

// CommitScoped reports whether ctx is within a transaction's commit scope (Transact): a caller
// that wants a hook to run once the write it just made has committed can tell a transaction that
// will commit from a write made outside any scope, which has no commit to wait for.
func CommitScoped(ctx context.Context) bool {
	_, ok := ctx.Value(commitHooksKey{}).(*[]func())
	return ok
}

// Transact runs write in one transaction of pool, opened with pgx's default options, commits it,
// and runs the hooks write registered with OnCommit if and only if the transaction committed.
// write runs once, on the scoped context, and is never retried. In order:
//
//   - A rollback is deferred as soon as the transaction begins, so a panic in write, or any
//     return below, gives the connection back to the pool and clears the caller's hold mark.
//   - write's error is returned. No hook runs.
//   - The transaction's id is read. No id means the transaction wrote nothing.
//   - A caller's context that has ended is returned as its error, and nothing commits: a context
//     that ended inside write already fails the id read, and this refuses one that ended after it.
//   - The commit runs on a context that the caller's cancel ends only commitReplyBound after that
//     cancel. A caller whose context never ends waits for the reply as long as the server takes.
//   - A commit that succeeded runs the hooks, in order, on this goroutine.
//   - A commit the server answered as not committed (an ERROR, or a ROLLBACK) returns its error,
//     and so does one that never reached the server (commitErrorKind). No hook runs.
//   - Any other failure lost the reply, and the server may have committed. A transaction that
//     wrote nothing holds the same rows either way, so its hooks run. Otherwise the store is asked
//     on a fresh connection, under a hold mark of its own, for at most outcomeReadBound
//     (readOutcome). Committed logs a warning and runs the hooks. Aborted returns an error, and
//     no hook runs. A store that cannot say logs a warning naming the transaction before Transact
//     returns ErrCommitOutcomeUnknown, and leaves the hooks to a watcher that runs them once the
//     store says the transaction committed (watchOutcome).
//
// So a caller whose context ends waits at most CommitSettleBound more for its commit's outcome.
func Transact(ctx context.Context, pool TxPool, write func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	// Once a commit has been tried, the transaction is closed and this does nothing.
	defer func() { _ = tx.Rollback(ctx) }()

	scoped, hooks := withCommitHooks(ctx)
	if err := write(scoped, tx); err != nil {
		return err
	}
	var assigned *string
	if err := tx.QueryRow(ctx, "select pg_current_xact_id_if_assigned()::text").Scan(&assigned); err != nil {
		return fmt.Errorf("read the transaction's id: %w", err)
	}
	xid := ""
	if assigned != nil {
		xid = *assigned
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	committing, stop := commitContext(ctx)
	err = tx.Commit(committing)
	stop()
	switch {
	case err == nil:
		runHooks(*hooks)
		return nil
	case commitErrorKind(err) != commitAmbiguous:
		return err
	default:
		return settleLostReply(ctx, pool, xid, *hooks, err)
	}
}

// settleLostReply is Transact's answer for a commit whose reply was lost with lost, for the
// transaction xid, "" when it wrote nothing, and the hooks its write registered. The read of the
// outcome takes a fresh connection under a mark of its own: pgx has already given the
// transaction's connection back, and the caller's mark is clear.
func settleLostReply(ctx context.Context, pool TxPool, xid string, hooks []func(), lost error) error {
	if xid == "" {
		runHooks(hooks)
		return nil
	}
	reading, cancel := context.WithTimeout(ForConcurrentWorker(context.WithoutCancel(ctx)), outcomeReadBound)
	defer cancel()
	switch result, _ := readOutcome(reading, pool, xid); result {
	case outcomeCommitted:
		slog.Warn("store: a commit's reply was lost; the store says the transaction committed",
			"transaction", xid, "error", lost)
		runHooks(hooks)
		return nil
	case outcomeAborted:
		return fmt.Errorf("transaction %s did not commit: %w", xid, lost)
	default:
		slog.Warn("store: a commit's reply was lost and the store cannot say whether it landed; its commit hooks wait until it can",
			"transaction", xid, "hooks", len(hooks), "error", lost)
		go watchOutcome(ForConcurrentWorker(context.WithoutCancel(ctx)), pool, xid, hooks)
		return fmt.Errorf("%w: transaction %s: %w", ErrCommitOutcomeUnknown, xid, lost)
	}
}

// commitContext is ctx without its cancel, ended commitReplyBound after ctx ends, for the commit
// alone: a ctx that never ends never ends it. stop ends it at once and releases what it holds.
func commitContext(ctx context.Context) (committing context.Context, stop func()) {
	committing, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopAfter := context.AfterFunc(ctx, func() {
		bound := time.NewTimer(commitReplyBound)
		defer bound.Stop()
		select {
		case <-bound.C:
		case <-committing.Done():
		}
		cancel()
	})
	return committing, func() {
		stopAfter()
		cancel()
	}
}

func runHooks(hooks []func()) {
	for _, hook := range hooks {
		hook()
	}
}

// commitError is what a failed commit says of whether the server committed.
type commitError int

const (
	commitAmbiguous commitError = iota // the server may or may not have committed: read the outcome
	commitAnswered                     // the server answered that the transaction did not commit
	commitUnsent                       // nothing reached the server
)

// commitErrorKind sorts a failed commit's error. The server's answer that the transaction did not
// commit is a ROLLBACK tag (pgx.ErrTxCommitRollback) or an ERROR, read from the unlocalized
// severity where the server sends one. A FATAL or a PANIC is also a *pgconn.PgError, which pgx
// returns for a server that ends the connection: it says that the connection ended, not whether
// the transaction committed, so it is ambiguous, as is every error but one pgx marks as sent
// nothing (pgconn.SafeToRetry).
func commitErrorKind(err error) commitError {
	switch {
	case errors.Is(err, pgx.ErrTxCommitRollback), answeredError(err):
		return commitAnswered
	case pgconn.SafeToRetry(err):
		return commitUnsent
	default:
		return commitAmbiguous
	}
}

// answeredError reports whether err is the server's ERROR answer.
func answeredError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return severity == "ERROR"
}

// outcome is what the store says of a transaction whose commit's reply was lost.
type outcome int

const (
	outcomeUnknown outcome = iota
	outcomeCommitted
	outcomeAborted
)

// readOutcome asks `select pg_xact_status($1::xid8)` on a fresh connection, again every
// outcomePollStep while it answers `in progress`, until ctx ends. A failed read, a status the
// server does not keep (NULL), and a transaction still in progress when ctx ends are unknown,
// with the error that says which.
func readOutcome(ctx context.Context, pool TxPool, xid string) (outcome, error) {
	for {
		var status *string
		if err := pool.QueryRow(ctx, "select pg_xact_status($1::xid8)", xid).Scan(&status); err != nil {
			return outcomeUnknown, fmt.Errorf("read the status of transaction %s: %w", xid, err)
		}
		switch {
		case status == nil:
			return outcomeUnknown, fmt.Errorf("the store keeps no status of transaction %s", xid)
		case *status == "committed":
			return outcomeCommitted, nil
		case *status == "aborted":
			return outcomeAborted, nil
		}
		poll := time.NewTimer(outcomePollStep)
		select {
		case <-ctx.Done():
			poll.Stop()
			return outcomeUnknown, fmt.Errorf("transaction %s is still in progress: %w", xid, ctx.Err())
		case <-poll.C:
		}
	}
}

// watchOutcome runs hooks once readOutcome says xid committed, and drops them once it says it
// aborted. ctx has no cancel and a hold mark of its own, since the transaction's owner may hold
// its next transaction's connection on its old mark, and the watcher holds no connection between
// reads. It waits outcomeRetryFirst, then twice as long each time up to outcomeRetryMax, and each
// read takes at most outcomeReadBound. It ends at the first answer, or once the pool has closed,
// which says that the hooks did not run; so it runs its hooks at most once. It has no other time
// limit and is not joined at stop: a status the server does not keep leaves it reading every
// outcomeRetryMax until the pool closes.
func watchOutcome(ctx context.Context, pool TxPool, xid string, hooks []func()) {
	for wait := outcomeRetryFirst; ; wait = min(2*wait, outcomeRetryMax) {
		time.Sleep(wait)
		reading, cancel := context.WithTimeout(ctx, outcomeReadBound)
		result, err := readOutcome(reading, pool, xid)
		cancel()
		switch {
		case result == outcomeCommitted:
			slog.Warn("store: a transaction whose outcome was unknown committed; its commit hooks run now",
				"transaction", xid, "hooks", len(hooks))
			runHooks(hooks)
			return
		case result == outcomeAborted:
			slog.Info("store: a transaction whose outcome was unknown did not commit; its commit hooks are dropped",
				"transaction", xid, "hooks", len(hooks))
			return
		case errors.Is(err, puddle.ErrClosedPool):
			slog.Error("store: the pool closed before the store said whether a transaction committed; its commit hooks did not run",
				"transaction", xid, "hooks", len(hooks))
			return
		}
	}
}
