// Package ratelimit is the GitHub rate limits every Dispatch process obeys together: one row of
// github_rate_limits (migration 0160) per scope, judged by the database's clock.
//
// GitHub budgets the App and each installation, not a process, and warns that requests sent while
// limited may get the integration banned. So a limit one process meets is recorded here, and every
// process reads it before it sends anything under that scope: githubapp.Client through the Limits
// it is given (Wait and Record, and Strike for a secondary limit, which GitHub shares between an
// installation's REST calls and its GraphQL queries), and the webhook redelivery sweep for its own
// doubling backoff (Strike, InForce, Get and Clear). The database's now() is the one clock both
// processes read, as embeddings_rate_limit's is (migration 0078), so they agree on when a limit
// ends whatever their hosts' clocks say.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Store reads and writes github_rate_limits on a pool.
type Store struct {
	pool *store.Pool
}

// New returns the store on pool.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// Limit is one scope's row as it was read or written.
type Limit struct {
	Scope string
	// Until is when a request under Scope may be sent again, by the database's clock.
	Until time.Time
	// Wait is Until less the database's now() when the row was read or written: how long the limit
	// had left then, zero or less once it has passed.
	Wait time.Duration
	// Strikes counts the limits Strike recorded with no Clear and no StrikeReset of quiet in
	// between: the run the doubling wait is taken from.
	Strikes int
	// updatedAt is the database's now() at the row's last write (every Record and Strike), which
	// Clear matches so it deletes only the row as read, and which Wait answers as the limit's
	// identity.
	updatedAt time.Time
}

// columns is what every read and write returns, in Limit's order.
const columns = `scope, until, extract(epoch from until - now()), strikes, updated_at`

func scan(row pgx.Row) (Limit, error) {
	var limit Limit
	var seconds float64
	err := row.Scan(&limit.Scope, &limit.Until, &seconds, &limit.Strikes, &limit.updatedAt)
	limit.Wait = time.Duration(seconds * float64(time.Second))
	return limit, err
}

// Wait is how long the limit in force on scope has left, zero when none is, and when it was last
// recorded. It is the read githubapp.Client makes before every request it sends. recorded is the
// row's last write by any process, so it names one limit: time passing leaves it alone, while a
// limit recorded again (a new one, or one extended) changes it.
func (s *Store) Wait(ctx context.Context, scope string) (wait time.Duration, recorded time.Time, err error) {
	limit, found, err := s.InForce(ctx, scope)
	if err != nil || !found {
		return 0, time.Time{}, err
	}
	return limit.Wait, limit.updatedAt, nil
}

// InForce returns, of the limits in force on scopes, the one that ends last; found is false when
// none is.
func (s *Store) InForce(ctx context.Context, scopes ...string) (limit Limit, found bool, err error) {
	limit, err = scan(s.pool.QueryRow(ctx, `
		select `+columns+` from github_rate_limits
		where scope = any($1) and until > now()
		order by until desc
		limit 1
	`, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("read the GitHub rate limit of %v: %w", scopes, err)
	}
	return limit, true, nil
}

// Get returns scope's row whether or not its limit is still in force; found is false when scope
// has none.
func (s *Store) Get(ctx context.Context, scope string) (limit Limit, found bool, err error) {
	limit, err = scan(s.pool.QueryRow(ctx, `select `+columns+` from github_rate_limits where scope = $1`, scope))
	if errors.Is(err, pgx.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("read the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, true, nil
}

// Record records that GitHub asked for wait before the next request under scope. The limit ends
// at the later of now() plus wait and a limit already recorded: two processes meeting one limit
// moments apart never shorten it. It writes in a transaction of its own, as every write a keyed
// request makes is: a request whose GitHub call meets a limit is one, and the transaction's
// commit step marks that request's Idempotency-Key committed with the write.
func (s *Store) Record(ctx context.Context, scope string, wait time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into github_rate_limits as l (scope, until, updated_at)
		values ($1, now() + make_interval(secs => $2), now())
		on conflict (scope) do update set until = greatest(l.until, excluded.until), updated_at = now()
	`, scope, wait.Seconds()); err != nil {
		return fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	return nil
}

// StrikeReset is how long a scope must go without a strike before its count starts again at one.
//
// The doubling is for a request that keeps failing on one secondary limit: GitHub asks a client
// that meets one to back off further each time, and the sweep's waits run one, two, four minutes
// and so on to a ceiling of 64. It is not a record of the scope's whole history. Without a reset
// the count only ever rose, since Clear is reached solely by a sweep that found GitHub willing
// again within the one pass that was already backing off, so a scope struck six times over a
// month met its next limit, unrelated to any of them, with a 64-minute wait: an hour of webhook
// redeliveries not made because of a limit six weeks old. An hour of quiet is far longer than
// any secondary limit GitHub hands out and far shorter than the gap between two unrelated ones,
// so a strike an hour after the last is treated as the first of a new run.
const StrikeReset = time.Hour

// strike is Strike's statement: one more strike, and the latest of now() plus GitHub's wait, one
// minute doubled for each strike before this one (up to 64 minutes), and a limit already recorded.
//
// A scope whose last strike is older than StrikeReset starts again: its count goes to one and
// its wait to a minute, exactly as the insert writes a scope that has no row at all. The clock
// is struck_at (migration 20261110132532), which Strike alone writes, rather than updated_at,
// which every write of the row moves. The two part on a scope that is both struck and recorded:
// an installation's scope holds the secondary limits its queries meet, which are struck because
// GitHub shares them with that installation's REST calls (githubapp.Client.recordGraphQLLimit),
// and every ordinary REST limit of that installation, which Record writes. Read from updated_at,
// a busy installation's REST limits would hold its run of strikes open for ever, and a secondary
// limit days after the one before it would wait the full 64 minutes for limits unrelated to it.
// The reset is an hour with no strike, which is what StrikeReset says it is.
//
// A null struck_at is a scope no strike has been recorded for: every row Record created and
// nothing has struck -- an installation's scope that has met the ordinary REST limits alone --
// and every row struck before the migration added the column, which nothing backfilled. The
// first strike of such a scope begins its run at one minute, which is what '-infinity' says in
// the reset condition below: a scope never struck is further back than any hour, so the one
// condition reads the same in both the places it appears. For a row struck by the release
// before the column, that costs one shorter wait, which the strike after it doubles again.
const strike = `
	insert into github_rate_limits as l (scope, until, strikes, struck_at, updated_at)
	values ($1, now() + greatest(make_interval(secs => $2), interval '1 minute'), 1, now(), now())
	on conflict (scope) do update set
		strikes = case when coalesce(l.struck_at, '-infinity') < now() - make_interval(secs => $3) then 1 else l.strikes + 1 end,
		until = greatest(l.until, now() + greatest(make_interval(secs => $2),
			interval '1 minute' * power(2, case when coalesce(l.struck_at, '-infinity') < now() - make_interval(secs => $3) then 0 else least(l.strikes, 6) end))),
		struck_at = now(),
		updated_at = now()
	returning ` + columns

// Strike records a limit GitHub answered with under scope, whose wait doubles with every limit met
// before a Clear or before StrikeReset of quiet, as GitHub asks of a request that keeps failing on
// a secondary limit. It returns the row it wrote.
func (s *Store) Strike(ctx context.Context, scope string, wait time.Duration) (Limit, error) {
	limit, err := scan(s.pool.QueryRow(ctx, strike, scope, wait.Seconds(), StrikeReset.Seconds()))
	if err != nil {
		return Limit{}, fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, nil
}

// PreviewStrike returns the row Strike would write, and writes nothing: Strike's own statement in
// a transaction it rolls back, so the two never differ.
func (s *Store) PreviewStrike(ctx context.Context, scope string, wait time.Duration) (Limit, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Limit{}, fmt.Errorf("preview the GitHub rate limit of %s: %w", scope, err)
	}
	defer tx.Rollback(ctx)
	limit, err := scan(tx.QueryRow(ctx, strike, scope, wait.Seconds(), StrikeReset.Seconds()))
	if err != nil {
		return Limit{}, fmt.Errorf("preview the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, nil
}

// Clear deletes limit's row only if nothing has written it since it was read: a limit another
// process recorded meanwhile stands.
func (s *Store) Clear(ctx context.Context, limit Limit) error {
	if _, err := s.pool.Exec(ctx, `delete from github_rate_limits where scope = $1 and updated_at = $2`, limit.Scope, limit.updatedAt); err != nil {
		return fmt.Errorf("clear the GitHub rate limit of %s: %w", limit.Scope, err)
	}
	return nil
}
