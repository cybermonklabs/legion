package dbosx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Workflow is one row of DBOS's workflow_status table, with the output and the error
// the run recorded. Those two moved to the workflow_output table in migration 109, so
// they are read the way DBOS reads them itself: from that table, falling back to the
// old columns.
type Workflow struct {
	// Found is false when no row exists for the workflow at all.
	Found bool
	// ID, Name and Status are the workflow's identity and where it stands: PENDING,
	// ENQUEUED, SUCCESS, ERROR, CANCELLED, MAX_RECOVERY_ATTEMPTS_EXCEEDED.
	ID     string
	Name   string
	Status string
	// ExecutorID is the process DBOS holds the workflow against. A pending workflow is
	// recovered by a process launching under this id, and by no other.
	ExecutorID string
	// ApplicationVersion is the version of the code the workflow was last dispatched
	// under. Recovery and dequeue both filter on it.
	ApplicationVersion string
	// QueueName is the queue the workflow sits on, empty when it is running directly.
	QueueName string
	// RecoveryAttempts counts how often the row was dispatched to an executor.
	RecoveryAttempts int64
	// OwnerXID is the token of the execution that owns the row. DBOS refuses a write
	// from an execution that no longer matches it.
	OwnerXID string
	// Output and Error are the recorded result, nil when there is none yet.
	Output *string
	Error  *string
	// StartedAt is when the current execution was dispatched, in epoch milliseconds.
	StartedAt *int64
}

// String is the one-line form a fact prints.
func (w Workflow) String() string {
	if !w.Found {
		return "no row"
	}
	return fmt.Sprintf("status=%s executor=%s version=%s queue=%s recovery_attempts=%d owner_xid=%s output=%s error=%s",
		w.Status, orNone(w.ExecutorID), orNone(w.ApplicationVersion), orNone(w.QueueName),
		w.RecoveryAttempts, orNone(w.OwnerXID), orNil(w.Output), orNil(w.Error))
}

// ReadWorkflow reads one workflow_status row with SQL.
func ReadWorkflow(ctx context.Context, pool dbos.Pool, schema, workflowID string) (Workflow, error) {
	query := fmt.Sprintf(`SELECT s.workflow_uuid, COALESCE(s.name, ''), COALESCE(s.status, ''),
		       COALESCE(s.executor_id, ''), COALESCE(s.application_version, ''),
		       COALESCE(s.queue_name, ''), COALESCE(s.recovery_attempts, 0),
		       COALESCE(s.owner_xid, ''),
		       COALESCE(o.output, s.output), COALESCE(o.error, s.error),
		       s.started_at_epoch_ms
		FROM %s.workflow_status s LEFT JOIN %s.workflow_output o USING (workflow_uuid)
		WHERE s.workflow_uuid = $1`, quote(schema), quote(schema))
	var w Workflow
	err := pool.QueryRow(ctx, query, workflowID).Scan(&w.ID, &w.Name, &w.Status,
		&w.ExecutorID, &w.ApplicationVersion, &w.QueueName, &w.RecoveryAttempts,
		&w.OwnerXID, &w.Output, &w.Error, &w.StartedAt)
	if err != nil {
		if errors.Is(err, dbos.ErrNoRows) {
			return Workflow{}, nil
		}
		return Workflow{}, fmt.Errorf("read workflow_status for %s: %w", workflowID, err)
	}
	w.Found = true
	return w, nil
}

// Step is one row of DBOS's operation_outputs table: the record that a step ran and
// what it returned. A step with no row here either never ran or ran without its outcome
// ever being written down -- the two are the same row, which is what experiment 7 is
// about.
type Step struct {
	// ID is the step's function_id, counted from 0 within the workflow.
	ID int
	// Name is the recorded function_name. A replay whose code presents a different
	// name at this id is refused.
	Name string
	// Output and Error are the recorded outcome in the form DBOS stored it.
	Output *string
	Error  *string
	// Serialization names the encoding of Output, so a reader can decode it.
	Serialization string
}

// String is the one-line form a fact prints.
func (s Step) String() string {
	return fmt.Sprintf("id=%d name=%s output=%s error=%s", s.ID, s.Name, orNil(s.Output), orNil(s.Error))
}

// ReadSteps reads every operation_outputs row of a workflow with SQL, in step order.
func ReadSteps(ctx context.Context, pool dbos.Pool, schema, workflowID string) ([]Step, error) {
	query := fmt.Sprintf(`SELECT function_id, function_name, output, error, COALESCE(serialization, '')
		FROM %s.operation_outputs WHERE workflow_uuid = $1 ORDER BY function_id`, quote(schema))
	rows, err := pool.Query(ctx, query, workflowID)
	if err != nil {
		return nil, fmt.Errorf("read operation_outputs for %s: %w", workflowID, err)
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var s Step
		if err := rows.Scan(&s.ID, &s.Name, &s.Output, &s.Error, &s.Serialization); err != nil {
			return nil, fmt.Errorf("scan operation_outputs for %s: %w", workflowID, err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read operation_outputs for %s: %w", workflowID, err)
	}
	return steps, nil
}

// StepNames is the recorded steps as one line, for a fact.
func StepNames(steps []Step) string {
	if len(steps) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, fmt.Sprintf("%d:%s", s.ID, s.Name))
	}
	return strings.Join(parts, " ")
}

// CountNotifications is how many message rows the notifications table holds for a
// workflow, and how many of those are still unconsumed: what a Send leaves behind, and
// what is still waiting to be received.
func CountNotifications(ctx context.Context, pool dbos.Pool, schema, workflowID string) (total, unconsumed int64, err error) {
	query := fmt.Sprintf(`SELECT count(*), count(*) FILTER (WHERE NOT consumed)
		FROM %s.notifications WHERE destination_uuid = $1`, quote(schema))
	if err := pool.QueryRow(ctx, query, workflowID).Scan(&total, &unconsumed); err != nil {
		return 0, 0, fmt.Errorf("count notifications for %s: %w", workflowID, err)
	}
	return total, unconsumed, nil
}

// RowCounts is how many rows of each of DBOS's own tables one workflow occupies, which
// is what a workflow standing still holds in the database.
func RowCounts(ctx context.Context, pool dbos.Pool, schema, workflowID string) (string, error) {
	tables := []string{"workflow_status", "workflow_input", "workflow_output", "operation_outputs", "notifications", "workflow_events", "streams"}
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		column := "workflow_uuid"
		if table == "notifications" {
			column = "destination_uuid"
		}
		var n int64
		query := fmt.Sprintf(`SELECT count(*) FROM %s.%s WHERE %s = $1`, quote(schema), quote(table), column)
		if err := pool.QueryRow(ctx, query, workflowID).Scan(&n); err != nil {
			return "", fmt.Errorf("count %s rows for %s: %w", table, workflowID, err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " "), nil
}

// RowBytes is how many bytes of PostgreSQL row the workflow occupies across the tables
// DBOS keeps it in. It is what one open wait costs in storage, measured rather than
// estimated.
func RowBytes(ctx context.Context, pool dbos.Pool, schema, workflowID string) (int64, error) {
	query := fmt.Sprintf(`SELECT
		  (SELECT COALESCE(sum(pg_column_size(s.*)), 0) FROM %s.workflow_status s WHERE s.workflow_uuid = $1)
		+ (SELECT COALESCE(sum(pg_column_size(o.*)), 0) FROM %s.operation_outputs o WHERE o.workflow_uuid = $1)
		+ (SELECT COALESCE(sum(pg_column_size(i.*)), 0) FROM %s.workflow_input i WHERE i.workflow_uuid = $1)
		+ (SELECT COALESCE(sum(pg_column_size(w.*)), 0) FROM %s.workflow_output w WHERE w.workflow_uuid = $1)`,
		quote(schema), quote(schema), quote(schema), quote(schema))
	var bytes int64
	if err := pool.QueryRow(ctx, query, workflowID).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("measure row bytes for %s: %w", workflowID, err)
	}
	return bytes, nil
}

// Backends is what pg_stat_activity says about the connections to the evaluation
// database: how many there are and what each one is doing. A wait that held a
// transaction open would show here as a backend idle in transaction.
type Backends struct {
	Total             int
	Active            int
	Idle              int
	IdleInTransaction int
}

// String is the one-line form a fact prints.
func (b Backends) String() string {
	return fmt.Sprintf("total=%d active=%d idle=%d idle-in-transaction=%d",
		b.Total, b.Active, b.Idle, b.IdleInTransaction)
}

// ReadBackends counts the connections to the evaluation database by state.
func ReadBackends(ctx context.Context, pool dbos.Pool) (Backends, error) {
	rows, err := pool.Query(ctx, `SELECT COALESCE(state, 'unknown'), count(*)
		FROM pg_stat_activity WHERE datname = current_database() GROUP BY 1`)
	if err != nil {
		return Backends{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	defer rows.Close()
	var b Backends
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return Backends{}, fmt.Errorf("scan pg_stat_activity: %w", err)
		}
		b.Total += n
		switch state {
		case "active":
			b.Active += n
		case "idle":
			b.Idle += n
		case "idle in transaction", "idle in transaction (aborted)":
			b.IdleInTransaction += n
		}
	}
	if err := rows.Err(); err != nil {
		return Backends{}, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	return b, nil
}

// CountHeldLocks counts the locks the connections to this database hold that a waiting
// workflow would hold if it waited inside a transaction or on a row: a transaction id,
// a tuple, or a row-exclusive lock on one of the schema's tables. Locks every live
// backend holds on itself (its own virtual transaction id) are not counted.
func CountHeldLocks(ctx context.Context, pool dbos.Pool, schema string) (int64, error) {
	var n int64
	query := `SELECT count(*)
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE a.datname = current_database()
		  AND (l.locktype IN ('transactionid', 'tuple')
		       OR (l.locktype = 'relation' AND l.mode <> 'AccessShareLock'
		           AND l.relation IN (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		                              WHERE n.nspname = $1)))`
	if err := pool.QueryRow(ctx, query, schema).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pg_locks: %w", err)
	}
	return n, nil
}

// DropSchema removes the run's schema and everything DBOS put in it, so the next run of
// the same experiment starts on an empty database.
func DropSchema(ctx context.Context, pool dbos.Pool, schema string) error {
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, quote(schema))); err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}
	return nil
}

// quote makes an identifier safe to interpolate. The experiments build their own schema
// names, so this guards against a name that cannot be an identifier rather than against
// an attacker.
func quote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// blobLimit is how much of a stored value a fact prints. DBOS stores an output or an
// error in its serialized form, and a serialized error runs to kilobytes; the whole of
// it would bury the line it belongs to. Anything longer is cut, and says how long it
// was, so a reader knows a cut happened.
const blobLimit = 120

func orNil(s *string) string {
	switch {
	case s == nil:
		return "null"
	case *s == "":
		return "empty"
	case len(*s) > blobLimit:
		return (*s)[:blobLimit] + fmt.Sprintf("... (%d bytes of serialized value)", len(*s))
	default:
		return *s
	}
}
