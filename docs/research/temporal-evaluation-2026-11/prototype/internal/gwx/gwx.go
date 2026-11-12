// Package gwx holds what the go-workflows experiments of the evaluation share:
// how a process opens the library's PostgreSQL backend on the run's own schema, and
// how a driver reads the library's own tables with SQL.
//
// go-workflows is a library, not a server: workflows and activities run inside the
// application's own process, against five tables the library migrates for itself in
// the application's database. The experiments read those tables directly, so a fact
// about a lock, a worker or a history is a row rather than an inference.
package gwx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/history"
	"github.com/cschleiden/go-workflows/backend/postgres"
	"github.com/cschleiden/go-workflows/core"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// Tables are the tables go-workflows' PostgreSQL backend creates for itself, besides
// the schema_migrations table golang-migrate keeps its own version in.
var Tables = []string{"instances", "pending_events", "history", "activities", "attributes"}

// Options is one process's view of the library: the schema its state lives in, the
// name it locks rows under, and the two lock timeouts the experiments shorten.
type Options struct {
	// WorkerName is what the library writes into instances.worker and
	// activities.worker when this process locks a task. Pinned by the experiment so
	// a fact can name the process a lock is held by.
	WorkerName string
	// ActivityLockTimeout is how long a locked activity stays the locker's. The
	// library's default is two minutes; the experiments shorten it and say so.
	ActivityLockTimeout time.Duration
	// WorkflowLockTimeout is the same for a workflow task.
	WorkflowLockTimeout time.Duration
	// StickyTimeout is how long a workflow instance stays bound to the worker that
	// last ran a task for it.
	StickyTimeout time.Duration
	// Logger receives the library's own warnings and errors. Experiment 15 passes
	// one that keeps them, because the error a changed workflow produces is one of
	// its measurements.
	Logger *slog.Logger
}

// Migrate creates the library's own tables in the schema db is connected to. The
// library applies its migrations itself, with golang-migrate, which records what it
// did in a schema_migrations table beside them. Only the process that owns the schema
// calls this; every other process finds the tables already there.
func Migrate(db *sql.DB) error {
	b := postgres.NewPostgresBackendWithDB(db)
	if err := b.Migrate(); err != nil {
		return fmt.Errorf("apply go-workflows migrations: %w", err)
	}
	return nil
}

// Backend opens go-workflows' PostgreSQL backend on db, which must already be
// connected to the evaluation database with the run's schema as its search path.
func Backend(db *sql.DB, o Options) backend.Backend {
	logger := o.Logger
	if logger == nil {
		logger = libx.Logger()
	}
	return postgres.NewPostgresBackendWithDB(db,
		postgres.WithApplyMigrations(false),
		postgres.WithBackendOptions(
			backend.WithLogger(logger),
			backend.WithWorkerName(o.WorkerName),
			backend.WithActivityLockTimeout(o.ActivityLockTimeout),
			backend.WithWorkflowLockTimeout(o.WorkflowLockTimeout),
			backend.WithStickyTimeout(o.StickyTimeout),
		),
	)
}

// Instance is one row of the instances table: where a workflow instance stands, who
// holds it and until when.
type Instance struct {
	// Found is false when no row exists for the instance at all.
	Found bool
	// InstanceID and ExecutionID are the instance's identity.
	InstanceID  string
	ExecutionID string
	// State is core.WorkflowInstanceState: 0 active, 1 continued-as-new, 2 finished.
	State int
	Queue string
	// LockedUntil is set while a worker holds a workflow task for this instance, and
	// StickyUntil while the instance is bound to the worker that last ran one.
	LockedUntil *time.Time
	StickyUntil *time.Time
	// Worker is the name of that worker, which stays on the row after the lock
	// lapses.
	Worker      *string
	CompletedAt *time.Time
}

// String is the one-line form a fact prints.
func (i Instance) String() string {
	if !i.Found {
		return "no row"
	}
	return fmt.Sprintf("state=%s queue=%s locked_until=%s sticky_until=%s worker=%s completed_at=%s",
		StateName(i.State), i.Queue, stamp(i.LockedUntil), stamp(i.StickyUntil), orNone(i.Worker), stamp(i.CompletedAt))
}

// StateName is what an instances.state integer means.
func StateName(state int) string {
	switch core.WorkflowInstanceState(state) {
	case core.WorkflowInstanceStateActive:
		return "Active(0)"
	case core.WorkflowInstanceStateContinuedAsNew:
		return "ContinuedAsNew(1)"
	case core.WorkflowInstanceStateFinished:
		return "Finished(2)"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

// Finished reports whether the instance reached a state it cannot leave on its own.
func (i Instance) Finished() bool {
	return i.Found && core.WorkflowInstanceState(i.State) == core.WorkflowInstanceStateFinished
}

// ReadInstance reads one instances row with SQL.
func ReadInstance(ctx context.Context, db *sql.DB, instanceID string) (Instance, error) {
	var i Instance
	err := db.QueryRowContext(ctx, `SELECT instance_id, execution_id, state, queue,
		locked_until, sticky_until, worker, completed_at
		FROM instances WHERE instance_id = $1`, instanceID).
		Scan(&i.InstanceID, &i.ExecutionID, &i.State, &i.Queue,
			&i.LockedUntil, &i.StickyUntil, &i.Worker, &i.CompletedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Instance{}, nil
		}
		return Instance{}, fmt.Errorf("read instances row for %s: %w", instanceID, err)
	}
	i.Found = true
	return i, nil
}

// Activity is one row of the activities table: an activity that has been scheduled
// and not yet completed. The row is deleted when the activity completes, so its
// presence means "scheduled, maybe running, not finished".
type Activity struct {
	ActivityID      string
	EventType       int
	Queue           string
	ScheduleEventID int64
	VisibleAt       *time.Time
	// LockedUntil is when the current holder's claim lapses, and Worker is that
	// holder. A worker that dies leaves both exactly as they were.
	LockedUntil *time.Time
	Worker      *string
}

// String is the one-line form a fact prints.
func (a Activity) String() string {
	return fmt.Sprintf("activity_id=%s event_type=%s queue=%s schedule_event_id=%d locked_until=%s worker=%s visible_at=%s",
		a.ActivityID, history.EventType(a.EventType), a.Queue, a.ScheduleEventID,
		stamp(a.LockedUntil), orNone(a.Worker), stamp(a.VisibleAt))
}

// ReadActivities reads every activities row of one instance with SQL.
func ReadActivities(ctx context.Context, db *sql.DB, instanceID string) ([]Activity, error) {
	rows, err := db.QueryContext(ctx, `SELECT activity_id, event_type, queue, schedule_event_id,
		visible_at, locked_until, worker FROM activities WHERE instance_id = $1 ORDER BY id`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read activities rows for %s: %w", instanceID, err)
	}
	defer rows.Close()
	var activities []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ActivityID, &a.EventType, &a.Queue, &a.ScheduleEventID,
			&a.VisibleAt, &a.LockedUntil, &a.Worker); err != nil {
			return nil, fmt.Errorf("scan activities row for %s: %w", instanceID, err)
		}
		activities = append(activities, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read activities rows for %s: %w", instanceID, err)
	}
	return activities, nil
}

// ReadHistory is the event types of one instance's history table rows, in order, as
// one line. The history is what the library replays, so its shape is what a changed
// workflow is measured against.
func ReadHistory(ctx context.Context, db *sql.DB, instanceID string) ([]string, error) {
	return readEventTypes(ctx, db, "history", instanceID)
}

// ReadPendingEvents is the same for the pending_events table: what is waiting to be
// delivered to the instance.
func ReadPendingEvents(ctx context.Context, db *sql.DB, instanceID string) ([]string, error) {
	return readEventTypes(ctx, db, "pending_events", instanceID)
}

// ReadEventPayloads is what the attributes table holds for every history event of
// one type: the serialized attributes the library wrote, which is where a finished
// instance's result or error is. The payloads are returned as the JSON the library
// stored, cut to payloadLimit so one fact stays one line.
func ReadEventPayloads(ctx context.Context, db *sql.DB, instanceID, eventType string) ([]string, error) {
	t, err := eventTypeByName(eventType)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT a.data FROM history h
		JOIN attributes a ON a.event_id = h.event_id AND a.instance_id = h.instance_id
			AND a.execution_id = h.execution_id
		WHERE h.instance_id = $1 AND h.event_type = $2 ORDER BY h.id`, instanceID, t)
	if err != nil {
		return nil, fmt.Errorf("read %s payloads for %s: %w", eventType, instanceID, err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan %s payload for %s: %w", eventType, instanceID, err)
		}
		text := string(data)
		if len(text) > payloadLimit {
			text = fmt.Sprintf("%s... (%d bytes of stored attributes)", text[:payloadLimit], len(data))
		}
		payloads = append(payloads, text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s payloads for %s: %w", eventType, instanceID, err)
	}
	return payloads, nil
}

// payloadLimit is how much of a stored event payload a fact prints. A recorded error
// runs to kilobytes, and the whole of it would bury the line it belongs to.
const payloadLimit = 600

// eventTypeByName is the event type an experiment names, looked up through the
// library's own String method so the names a fact prints and the names it asks for
// are the same set.
func eventTypeByName(name string) (int, error) {
	for t := 1; t < 32; t++ {
		if history.EventType(t).String() == name {
			return t, nil
		}
	}
	return 0, fmt.Errorf("go-workflows has no history event type named %q", name)
}

func readEventTypes(ctx context.Context, db *sql.DB, table, instanceID string) ([]string, error) {
	query := fmt.Sprintf(`SELECT event_type FROM %s WHERE instance_id = $1 ORDER BY id`, libx.Quote(table))
	rows, err := db.QueryContext(ctx, query, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", table, instanceID, err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var t int
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan %s for %s: %w", table, instanceID, err)
		}
		types = append(types, history.EventType(t).String())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", table, instanceID, err)
	}
	return types, nil
}

// Events is a list of event type names as one line, for a fact.
func Events(types []string) string {
	if len(types) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d: %s", len(types), strings.Join(types, " "))
}

// CountEvents is how many of one event type the list holds.
func CountEvents(types []string, name string) int {
	n := 0
	for _, t := range types {
		if t == name {
			n++
		}
	}
	return n
}

// WaitForInstance polls the instances row until pass accepts it, and fails with the
// last row it saw when the timeout passes first.
func WaitForInstance(ctx context.Context, db *sql.DB, instanceID string, timeout time.Duration, pass func(Instance) bool) (Instance, error) {
	deadline := time.Now().Add(timeout)
	for {
		i, err := ReadInstance(ctx, db, instanceID)
		if err != nil {
			return Instance{}, err
		}
		if pass(i) {
			return i, nil
		}
		if !time.Now().Before(deadline) {
			return i, fmt.Errorf("instance %s was still %s after %s", instanceID, i, timeout)
		}
		if err := libx.Sleep(ctx, 250*time.Millisecond); err != nil {
			return i, err
		}
	}
}

// Result is what an experiment's activity reports: which performance of the side
// effect this was, and the process that performed it.
type Result struct {
	Attempt int `json:"attempt"`
	PID     int `json:"pid"`
}

func stamp(t *time.Time) string {
	if t == nil {
		return "null"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func orNone(s *string) string {
	if s == nil || *s == "" {
		return "none"
	}
	return *s
}
