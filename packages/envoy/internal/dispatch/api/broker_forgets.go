// broker_forgets.go: the sender of the forgets Dispatch owes the secrets broker (broker_forgets).
//
// A switch to private asks the broker to count a project's live developer-box tokens, and the broker
// first records the project and its Slack channels in its store of private projects, answering the
// record's number. A switch that does not make the project private owes the broker the forget of
// that record, and a switch of a private or closing project to public owes it the forget of the
// project's record whatever its number (access.OweBrokerForget). Dispatch's event outbox publishes
// to NATS only (internal/dispatch/outbox), so these forgets have this queue of their own.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// brokerForgetPoll is how often the sender looks for a forget that has come due.
	brokerForgetPoll = 5 * time.Second
	// brokerForgetFirstRetry and brokerForgetLastRetry bound the wait after a forget the broker did
	// not take: the first wait, doubled at each failure, and its ceiling.
	brokerForgetFirstRetry = 5 * time.Second
	brokerForgetLastRetry  = 5 * time.Minute
)

// brokerForgetter is the one call of the secrets broker the sender makes
// (agentsecrets.Client.ForgetPrivateProject).
type brokerForgetter interface {
	ForgetPrivateProject(ctx context.Context, project string, record *int64) (time.Duration, error)
}

// RunBrokerForgets sends the forgets Dispatch owes broker until ctx ends: at once, and then every
// brokerForgetPoll, every forget that has come due (sendBrokerForgets). cmd/dispatch starts it
// when DISPATCH_AGENT_SECRETS_URL names a broker. Every process runs it: each claims a row with
// `for update skip locked`, so two processes never send one forget at once.
func RunBrokerForgets(ctx context.Context, pool *store.Pool, broker brokerForgetter) {
	ticker := time.NewTicker(brokerForgetPoll)
	defer ticker.Stop()
	for {
		if err := sendBrokerForgets(ctx, pool, broker); err != nil && ctx.Err() == nil {
			slog.Warn("dispatch: the forgets owed the secrets broker could not be read", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sendBrokerForgets sends each forget that has come due, one transaction at a time
// (sendOneBrokerForget), until none is due.
func sendBrokerForgets(ctx context.Context, pool *store.Pool, broker brokerForgetter) error {
	for {
		sent, err := sendOneBrokerForget(ctx, pool, broker)
		if err != nil || !sent {
			return err
		}
	}
}

// sendOneBrokerForget claims the oldest forget that has come due, `for update skip locked`, and
// answers false when none is. It deletes the row unsent when its project is private as it reads it:
// the switch committed, or the project was made private another way, and its record must stay. It
// sends every other row, and deletes it once the broker takes it. A forget the broker asks to send
// later (409 RECORD_IN_FLIGHT) waits as long as it asked; any other failure, a broker without the
// route included, waits brokerForgetFirstRetry, doubled at each failure up to brokerForgetLastRetry.
func sendOneBrokerForget(ctx context.Context, pool *store.Pool, broker brokerForgetter) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var (
		id       int64
		project  string
		record   *int64
		attempts int
	)
	err = tx.QueryRow(ctx, `select id, project, record, attempts from broker_forgets
		where next_attempt_at <= now() order by next_attempt_at, id limit 1 for update skip locked`).Scan(&id, &project, &record, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim a forget owed the secrets broker: %w", err)
	}
	var private bool
	if err := tx.QueryRow(ctx, `select coalesce((select visibility = 'private' from projects where key = $1), false)`, project).Scan(&private); err != nil {
		return false, fmt.Errorf("read the visibility of project %s: %w", project, err)
	}
	if private {
		if _, err := tx.Exec(ctx, `delete from broker_forgets where id = $1`, id); err != nil {
			return false, fmt.Errorf("drop the forget of private project %s: %w", project, err)
		}
		return true, commitForget(ctx, tx)
	}
	retryAfter, sendErr := broker.ForgetPrivateProject(ctx, project, record)
	if sendErr == nil {
		if _, err := tx.Exec(ctx, `delete from broker_forgets where id = $1`, id); err != nil {
			return false, fmt.Errorf("drop the delivered forget of project %s: %w", project, err)
		}
		return true, commitForget(ctx, tx)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	wait := retryAfter
	if wait <= 0 {
		wait = brokerForgetFirstRetry << min(attempts, 6)
		wait = min(wait, brokerForgetLastRetry)
	}
	slog.Warn("dispatch: the secrets broker did not take a forget; it is sent again later",
		"project", project, "record", record, "attempts", attempts+1, "retry_in", wait, "error", sendErr)
	if _, err := tx.Exec(ctx, `update broker_forgets set attempts = attempts + 1, last_error = $2,
		next_attempt_at = now() + make_interval(secs => $3) where id = $1`, id, sendErr.Error(), wait.Seconds()); err != nil {
		return false, fmt.Errorf("put off the forget of project %s: %w", project, err)
	}
	return true, commitForget(ctx, tx)
}

func commitForget(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit a forget owed the secrets broker: %w", err)
	}
	return nil
}
