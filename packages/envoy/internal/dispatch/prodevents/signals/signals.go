// Package signals writes the record's rows that no person and no system posts
// (dispatch://INFRA-978, R6 and R7): the GitHub adapter's deploy rows, one per deploy run of a
// deploy repository that the repository's rule calls a deploy, and the failures of the three
// automatic signals: a failed after-merge check (check_fail), a merged revert (revert), and an issue
// labelled incident that links the pull request or the run it blames (incident_issue).
//
// One woken loop under one lease (Lease) reads three feeds, each from a cursor of its own
// (production_signal_cursors, migration 20261123041501), as legionfacts reads its three:
//
//   - deploys: the delivery change log (delivery_changes), whose run, job and pull request changes
//     a run write, a job write or a pull request write of the intake, the job intake and the
//     reconcile log once it commits. Each moves the deploy rows of its repository (passDeploys).
//     After the rows land it writes the failures a landed deploy row completes: a revert's, once the
//     deploy that ships the revert has its row, and a check_fail whose result came before its
//     deploy.
//   - checks: the event log (events), whose result.recorded of a fail on an after_merge check makes
//     a check_fail, and whose result.retracted rejects the row its result made (passChecks).
//   - incidents: the event log, whose issue.created and issue.updated of an issue labelled incident
//     make an incident_issue from the issue's own links (passIncidents).
//
// Every write is a transaction of its own that takes the delivery change log's lock nowhere
// (prodevents), after the write that caused it has committed, so no transaction that takes that
// lock locks a production_events row. A feed's cursor moves after the rows of its page are written,
// and every write is idempotent by its source_ref, so a page read again after a crash writes
// nothing twice. The record route's handlers write check_fail and its retraction's reject at once
// as well (api's recordCheckResult and retractCheckResult); the loop is what a write lost to a
// crash, or a result that came before its deploy, waits on.
package signals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Actor writes every row the loop writes but a check_fail and an incident_issue, which carry the
// actor of their result and of their event: the deploy rows, a revert's failure and its restore,
// and the reject of a failure on a deploy a rule change dropped.
var Actor = model.Actor{Kind: "system", ID: "production-signals"}

// Lease names the lease every pass takes (duty.Exclusive with no period): two processes woken by
// one commit would otherwise derive the same repository's rows at once.
const Lease = "production-signals"

// PassRetry is how long a process that found the lease held waits, plus up to a tenth again,
// before it tries once more (duty.WokenLoop.Retry), and how far on a pass that failed asks for the
// next, its cursors standing where they were. It is also the longest a pass whose safe horizon held
// an event back waits for the next (horizonRetry doubling up to it).
const PassRetry = 10 * time.Second

// horizonRetry is how far on a pass whose safe horizon held an event back asks for the next, the
// first time in a row.
const horizonRetry = 250 * time.Millisecond

// RevertsChannel is the Postgres notification channel a write of delivery_reverts announces itself
// on at commit (migration 20261123041500): a revert the reconcile records after its deploy row
// landed makes its failure on the pass it wakes.
const RevertsChannel = "dispatch_delivery_reverts"

// page is the most rows a page of one feed reads.
const page = 500

// The names of the three cursors (production_signal_cursors.name).
const (
	deploysCursor   = "deploys"
	checksCursor    = "checks"
	incidentsCursor = "incidents"
)

// Deps is what Run needs of the server.
type Deps struct {
	Pool *store.Pool
	// Horizon is the process's one events.Horizon: the event feeds are paged by the events' global
	// id, so they stop at its safe horizon.
	Horizon *events.Horizon
	// Leases is the pool a pass takes its lease connection from (duty.Exclusive).
	Leases duty.Leases
	// Events is signalled when an event commits (api.EventsChannel): a result, a retraction, an
	// issue's labels and links, and a project.updated whose default repository moved a source.
	Events <-chan struct{}
	// Deliveries is signalled when the delivery change log moved (delivery.ChangesChannel).
	Deliveries <-chan struct{}
	// Settings is signalled when a project's delivery settings moved (delivery.SettingsChannel).
	Settings <-chan struct{}
	// Reverts is signalled when a revert is recorded (RevertsChannel).
	Reverts <-chan struct{}
}

// Run keeps the record's GitHub deploy rows and its automatic failures in step with their inputs
// until ctx ends: a woken loop (duty.RunWoken) under Lease, one pass at start, a pass on each wake,
// which starts as soon as the pass running when it came ends, a pass PassRetry after one that
// failed, a pass soon after one whose safe horizon held an event back, and no timer otherwise.
func Run(ctx context.Context, deps Deps) {
	ctx = store.WithTransactionTracking(ctx)
	held := &duty.Backoff{First: horizonRetry, Max: PassRetry}
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  Lease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Deliveries, deps.Settings, deps.Reverts),
		Retry:  PassRetry,
		Pass: func(ctx context.Context) time.Time {
			heldBack, err := pass(ctx, deps)
			switch {
			case ctx.Err() != nil:
				return time.Time{}
			case err != nil:
				slog.Warn("dispatch: the production signals' pass failed", "error", err)
				return time.Now().Add(PassRetry)
			case heldBack:
				return time.Now().Add(held.Next())
			}
			held.Reset()
			return time.Time{}
		},
	})
}

// pass is one pass of the loop: the deploys feed first, so a deploy row a page of the event feeds
// blames is there when it is read, then the two event feeds up to the safe horizon the pass reads
// once. It answers whether that horizon held an event back.
func pass(ctx context.Context, deps Deps) (bool, error) {
	if err := passDeploys(ctx, deps.Pool, time.Now); err != nil {
		return false, fmt.Errorf("the deploys feed: %w", err)
	}
	safe, err := deps.Horizon.SafeOrAwait(ctx)
	if err != nil {
		return false, err
	}
	if err := passChecks(ctx, deps.Pool, safe); err != nil {
		return false, fmt.Errorf("the checks feed: %w", err)
	}
	if err := passIncidents(ctx, deps.Pool, safe); err != nil {
		return false, fmt.Errorf("the incidents feed: %w", err)
	}
	return deps.Horizon.HeldBack(safe), nil
}

// cursor is one row of production_signal_cursors: the position, the time of the change at it (the
// deploys feed's), and its stamp. held is false where no row exists, a feed this database has read
// nothing of.
type cursor struct {
	position int64
	at       *time.Time
	stamped  time.Time
	held     bool
}

// readCursor reads the cursor name.
func readCursor(ctx context.Context, q delivery.Querier, name string) (cursor, error) {
	var c cursor
	err := q.QueryRow(ctx, `select position, at, updated_at from production_signal_cursors where name = $1`, name).
		Scan(&c.position, &c.at, &c.stamped)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor{}, nil
	}
	if err != nil {
		return cursor{}, fmt.Errorf("read the %s cursor: %w", name, err)
	}
	c.held = true
	return c, nil
}

// moveCursor writes the cursor name at position, with the time of the change there and stamped.
func moveCursor(ctx context.Context, pool *store.Pool, name string, position int64, at *time.Time, stamped time.Time) error {
	if _, err := pool.Exec(ctx, `
		insert into production_signal_cursors (name, position, at, updated_at) values ($1, $2, $3, $4)
		on conflict (name) do update set position = excluded.position, at = excluded.at, updated_at = excluded.updated_at
	`, name, position, at, stamped); err != nil {
		return fmt.Errorf("move the %s cursor: %w", name, err)
	}
	return nil
}
