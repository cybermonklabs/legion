package storesmove

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// RehearsalBound is how long the rehearsal runs before it stops wherever it is, prints its report
// so far and refuses: half of the activeDeadlineSeconds of 600 that production's census Job runs
// under, so a rehearsal that would outlast the Job still reports.
const RehearsalBound = 300 * time.Second

// rehearsalIdleBound is the rehearsal session's idle_in_transaction_session_timeout. No statement
// of the rehearsal leaves its session idle in its transaction for more than a moment, so a census
// pod that dies without closing its connection releases the rehearsal's transaction id within a
// minute.
const rehearsalIdleBound = 60 * time.Second

// rehearsalApplicationName is how the rehearsal's connection appears in pg_stat_activity.
const rehearsalApplicationName = "envoy-dispatch census rehearsal"

// rehearsalTables are the six tables the rehearsal's copy and census read and write. A pending
// migration of another tree that touches one of them is refused (pgmigrate.RehearseOther).
var rehearsalTables = []string{"priority_nodes", "priority_tuples", "workflow_nodes", "workflow_tuples", "graph_nodes", "graph_tuples"}

// ownMigrations are the names of dispatch://INFRA-1098's own migrations, which only its pull
// request adds, the one list the code holds of them: pending where the census rehearses the move
// before its deploy, since production then holds every migration of origin/main that touches one
// of rehearsalTables (c-moved-recorded). Each is held to pgmigrate.RehearseOwn's rule; every other
// pending migration is another tree's, held to pgmigrate.RehearseOther's. A migration this issue
// adds joins the list in the change that adds it, or the rehearsal reads it as another tree's.
var ownMigrations = []string{
	"20261122200000_graph_edges_retirements.up.sql",
	"20261123120000_graph_edges_one_tuple_arm.up.sql",
	"20261123134318_stores_moved.up.sql",
	"20261123185607_graph_nodes_artifact_id_index.up.sql",
	"20261125093000_graph_tuples_workflow_inbound_index.up.sql",
}

// OwnMigrations is ownMigrations, a fresh slice for each caller: store's
// TestStoresMovedPassesTheCensusAtAOneByteLimit applies every migration of the store but these.
func OwnMigrations() []string { return slices.Clone(ownMigrations) }

// RehearsalOptions are the rehearsal's test settings, each zero in a deployment (cmd/dispatch's
// DISPATCH_TEST_STORES_REHEARSAL_* and DISPATCH_TEST_STORES_MOVE_EXPLAIN settings, read only under
// DISPATCH_TEST_HOOKS=1).
type RehearsalOptions struct {
	// Stop is the time after which the rehearsal stops and refuses; 0 is RehearsalBound.
	Stop time.Duration
	// HoldFile, where set, is a file the rehearsal waits for after its copy, its transaction held
	// open with `select pg_sleep(1)` each second, so a test can write through the serving release
	// while the rehearsal stands.
	HoldFile string
	// Explain prints the plan of each statement of the census before the drop.
	Explain bool
	// ExtraMigration, where set, is a file the rehearsal reads as one more pending migration of
	// another tree, after the others.
	ExtraMigration string
}

// Rehearsal is what the rehearsal found (Rehearse).
type Rehearsal struct {
	// Ran is whether a rehearsal ran: priority_nodes was a table.
	Ran bool
	// Migrations are the pending migrations, each with what the rehearsal does with it.
	Migrations []RehearsedMigration
	// Copied is how many tuples its copy moved, in CopyTime.
	Copied   int
	CopyTime time.Duration
	// Census is its census before the drop; nil where it took none.
	Census *Census
	// Stopped is whether it stopped at its bound (RehearsalOptions.Stop) before its census ended.
	Stopped bool

	out io.Writer
}

// RehearsedMigration is one pending migration and what the rehearsal does with it.
type RehearsedMigration struct {
	Name string
	Own  bool
	Step pgmigrate.RehearsalStep
	Why  string
}

// Refused reports whether a pending migration refuses the rehearsal, so the census could not be
// taken: `envoy-dispatch census` exits 2.
func (r *Rehearsal) Refused() bool {
	return slices.ContainsFunc(r.Migrations, func(m RehearsedMigration) bool { return m.Step == pgmigrate.RehearsalRefuses })
}

// Differs reports whether the rehearsal refuses on a line of its own: its census read a difference,
// or it stopped at its bound. `envoy-dispatch census` exits 1.
func (r *Rehearsal) Differs() bool {
	return r.Stopped || r.Census != nil && r.Census.Differs()
}

// say prints one line of the rehearsal's report.
func (r *Rehearsal) say(format string, args ...any) {
	fmt.Fprintf(r.out, "census: rehearsal: "+format+"\n", args...)
}

// refuse prints one refusal of the rehearsal itself, a line that starts `census: REFUSED
// rehearsal:`.
func (r *Rehearsal) refuse(format string, args ...any) {
	fmt.Fprintf(r.out, "census: REFUSED rehearsal: "+format+"\n", args...)
}

// Rehearse is the pre-deploy census's rehearsal of the move, on a connection of its own to the
// database databaseURL names, its report printed to out as it goes: the copy and the census before
// the drop in one repeatable read transaction that it rolls back, so a difference stops a deploy
// before the image moves, and the boot's census under the swap's locks stays the second guard. It
// runs only where priority_nodes is a table, and prints nothing where it is a view, after the swap,
// or gone, after the drop.
//
// The transaction reads one snapshot while the serving release goes on writing the old tables. It
// reads the old tables, takes row exclusive on the two graph tables and no lock the serving
// release's writes meet, takes no owner's row and appends no event, so it holds up no write. It
// sets idle_in_transaction_session_timeout (rehearsalIdleBound), and it stops at
// RehearsalOptions.Stop wherever it is and refuses on a line of its own. Its nextval draws stay
// after the rollback, as PostgreSQL's sequences do, and no value a read answers depends on that gap.
//
// It reads the pending migrations, those schema_migrations does not record, in name order. Of
// this issue's own (ownMigrations), it runs in its transaction each pgmigrate.RehearseOwn applies
// and leaves out each it skips; of another tree's it runs none, and leaves out each
// pgmigrate.RehearseOther skips, which reads the triggers of the six tables in the catalog. The
// migrations it applies, this issue's own, create no trigger (RehearseOwn refuses one). A pending
// migration either refuses ends the rehearsal before it runs anything, on the line `census: the
// rehearsal refuses the pending migration <name>` (Rehearsal.Refused). The copy then moves every
// tuple of the four tables in batches of DefaultBatch, and the census before the drop reads them.
// A difference is named by its address alone, its store, kind, id and old seq, since the census
// prints no row. It answers an error only where the rehearsal could not be taken.
func Rehearse(ctx context.Context, databaseURL string, out io.Writer, options RehearsalOptions) (*Rehearsal, error) {
	stop := options.Stop
	if stop <= 0 {
		stop = RehearsalBound
	}
	bounded, cancel := context.WithTimeout(ctx, stop)
	defer cancel()
	rehearsal := &Rehearsal{out: out}
	err := rehearse(bounded, databaseURL, rehearsal, options)
	if err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		rehearsal.Stopped = true
		rehearsal.refuse("it stopped after %s, its bound, before its census before the drop ended; it rolled back, so it changed nothing", stop)
		return rehearsal, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the stores' rehearsal: %w", err)
	}
	return rehearsal, nil
}

// rehearse runs the rehearsal (Rehearse) on a connection of its own, filling rehearsal.
func rehearse(ctx context.Context, databaseURL string, rehearsal *Rehearsal, options RehearsalOptions) error {
	conn, err := store.Connect(ctx, databaseURL, rehearsalApplicationName)
	if err != nil {
		return err
	}
	defer closeBounded(ctx, conn)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `set local idle_in_transaction_session_timeout = '`+
		strconv.FormatInt(rehearsalIdleBound.Milliseconds(), 10)+`ms'`); err != nil {
		return fmt.Errorf("bound its idle time: %w", err)
	}
	relkind, err := priorityNodesKind(ctx, tx)
	if err != nil {
		return err
	}
	if relkind != tableKind {
		return nil
	}
	rehearsal.Ran = true

	applied, err := rehearsal.readPending(ctx, tx, options.ExtraMigration)
	if err != nil {
		return err
	}
	if rehearsal.Refused() {
		return nil
	}
	for _, migration := range applied {
		if err := pgmigrate.Exec(ctx, tx, migration, nil); err != nil {
			return fmt.Errorf("run the pending migration %s: %w", migration.Name, err)
		}
	}

	started := time.Now()
	rehearsal.Copied, err = newMover(Options{}).copyAll(ctx, tx)
	if err != nil {
		return err
	}
	rehearsal.CopyTime = time.Since(started).Round(time.Millisecond)
	rehearsal.say("its copy ended: %d tuples of the four tables in %s, in batches of %d", rehearsal.Copied, rehearsal.CopyTime, DefaultBatch)
	if options.HoldFile != "" {
		if err := holdUntil(ctx, tx, options.HoldFile, rehearsal); err != nil {
			return err
		}
	}

	var plan planner
	if options.Explain {
		plan = rehearsal.explain(tx)
	}
	rehearsal.Census, err = takeCensus(ctx, tx, plan)
	if err != nil {
		return err
	}
	for _, line := range rehearsal.Census.Lines() {
		rehearsal.say("%s", line)
	}
	if first := rehearsal.Census.First(); first != nil {
		rehearsal.refuse("the census before the drop read a difference, which the boot's swap would refuse too: %s; it rolled back, so it changed nothing", first)
		return nil
	}
	rehearsal.say("no difference; it rolled back, so it changed nothing")
	return nil
}

// readPending reads the pending migrations in tx, with the file extra names, where set, as one
// more of another tree after them, and what the rehearsal does with each, printing each. It answers
// the ones it applies, in the runner's order.
func (r *Rehearsal) readPending(ctx context.Context, tx pgx.Tx, extra string) ([]pgmigrate.Migration, error) {
	pending, err := store.PendingMigrations(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the pending migrations: %w", err)
	}
	if extra != "" {
		text, err := os.ReadFile(extra)
		if err != nil {
			return nil, fmt.Errorf("read the extra pending migration %s: %w", extra, err)
		}
		pending = append(pending, pgmigrate.Migration{Name: extra, SQL: string(text)})
	}
	triggers, err := triggersOf(ctx, tx, rehearsalTables)
	if err != nil {
		return nil, err
	}
	var applied []pgmigrate.Migration
	for _, migration := range pending {
		rehearsed := RehearsedMigration{Name: migration.Name, Own: slices.Contains(ownMigrations, migration.Name)}
		whose := "another tree's"
		if rehearsed.Own {
			whose = "this issue's own"
			rehearsed.Step, rehearsed.Why = pgmigrate.RehearseOwn(migration, rehearsalTables)
		} else {
			rehearsed.Step, rehearsed.Why = pgmigrate.RehearseOther(migration, rehearsalTables, triggers)
		}
		r.Migrations = append(r.Migrations, rehearsed)
		if rehearsed.Step == pgmigrate.RehearsalRefuses {
			fmt.Fprintf(r.out, "census: the rehearsal refuses the pending migration %s\n", migration.Name)
			r.say("it refuses the pending migration %s, of %s: %s", migration.Name, whose, rehearsed.Why)
			continue
		}
		r.say("it %s the pending migration %s, of %s: %s", rehearsed.Step, migration.Name, whose, rehearsed.Why)
		if rehearsed.Step == pgmigrate.RehearsalApplies {
			applied = append(applied, migration)
		}
	}
	return applied, nil
}

// triggersOf reads, in the catalog tx sees, each trigger of tables that no constraint made and the
// name of the function it runs (pg_trigger.tgfoid).
func triggersOf(ctx context.Context, tx pgx.Tx, tables []string) ([]pgmigrate.TriggerFunction, error) {
	rows, err := tx.Query(ctx, `
		select c.relname::text, p.proname::text
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_proc p on p.oid = t.tgfoid
		where not t.tgisinternal and c.oid in (select to_regclass(name) from unnest($1::text[]) as name)
		order by 1, 2
	`, tables)
	if err != nil {
		return nil, fmt.Errorf("read the triggers of the tables the rehearsal reads: %w", err)
	}
	triggers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pgmigrate.TriggerFunction, error) {
		var trigger pgmigrate.TriggerFunction
		err := row.Scan(&trigger.Table, &trigger.Function)
		return trigger, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the triggers of the tables the rehearsal reads: %w", err)
	}
	return triggers, nil
}

// holdUntil holds tx open until a file stands at path, with `select pg_sleep(1)` each second, so
// no moment of the hold leaves the session idle in its transaction.
func holdUntil(ctx context.Context, tx pgx.Tx, path string, rehearsal *Rehearsal) error {
	rehearsal.say("it holds its transaction until %s exists", path)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `select pg_sleep(1)`); err != nil {
			return fmt.Errorf("hold its transaction until %s exists: %w", path, err)
		}
	}
}

// explain is the planner that prints the plan of each statement of the census before the drop,
// each line of it after the name of what the statement reads.
func (r *Rehearsal) explain(tx pgx.Tx) planner {
	return func(ctx context.Context, what, query string) error {
		rows, err := tx.Query(ctx, `explain `+query)
		if err != nil {
			return fmt.Errorf("explain the census of the %s: %w", what, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("explain the census of the %s: %w", what, err)
		}
		r.say("plan of the census of the %s:\n%s", what, "    "+strings.Join(lines, "\n    "))
		return nil
	}
}
