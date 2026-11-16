// Package dtx holds what the durabletask-go experiments of the evaluation share: how
// a process opens the library's PostgreSQL backend on the run's own schema, how it
// runs the engine in process, and how a driver reads the library's own tables with
// SQL.
//
// durabletask-go is a library, not a server: the engine runs inside the application's
// own process, against four tables it creates for itself in the application's
// database. The experiments read those tables directly, so a fact about a lock, a
// dequeue count or a history is a row rather than an inference.
package dtx

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/backend"
	"github.com/dapr/durabletask-go/backend/postgres"
	"github.com/dapr/durabletask-go/task"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/libx"
)

// Tables are the tables durabletask-go's PostgreSQL backend creates for itself. The
// library has no migration table: its CreateTaskHub executes one schema script of
// CREATE TABLE IF NOT EXISTS statements every time a worker starts.
var Tables = []string{"instances", "history", "newevents", "newtasks"}

// maxConnections is how many connections one process's pool may open. The library's
// own default is one, which serializes the workflow poller, the activity poller and
// the engine's writes onto a single connection.
const maxConnections = 5

// Engine is durabletask-go running in one process: the backend, the worker that polls
// it and the client that starts and signals instances.
type Engine struct {
	Backend backend.Backend
	Worker  backend.TaskHubWorker
	Client  backend.TaskHubClient
}

// Logger is the logger the experiments give durabletask-go: warnings and errors on
// stderr and nothing else. The library's own default logger prints a debug line per
// poll and per work item, which would bury the measured lines the experiments print.
type Logger struct{ out *log.Logger }

// NewLogger is that logger.
func NewLogger() *Logger {
	return &Logger{out: log.New(os.Stderr, "durabletask: ", log.LstdFlags)}
}

func (l *Logger) Debug(...any)             {}
func (l *Logger) Debugf(string, ...any)    {}
func (l *Logger) Info(...any)              {}
func (l *Logger) Infof(string, ...any)     {}
func (l *Logger) Warn(v ...any)            { l.out.Print(v...) }
func (l *Logger) Warnf(f string, v ...any) { l.out.Printf(f, v...) }
func (l *Logger) Error(v ...any)           { l.out.Print(v...) }
func (l *Logger) Errorf(f string, v ...any) {
	l.out.Printf(f, v...)
}

// Options is one process's view of the library.
type Options struct {
	// DSN is the PostgreSQL the command line named, and Schema the schema this run
	// owns inside the evaluation database.
	DSN    string
	Schema string
	// WorkflowLockTimeout is how long a locked workflow work item stays the
	// locker's. The library's default is two minutes, and the experiments shorten
	// it and say so. The PostgreSQL backend uses this same number for an activity
	// work item's lock expiration as well.
	WorkflowLockTimeout time.Duration
	// ActivityLockTimeout is the library's own activity lock setting, which its
	// PostgreSQL backend reads for nothing: its getActivityWorkItem computes the
	// expiration from WorkflowLockTimeout. It is set anyway, so a fact can say what
	// the process asked for as well as what the library did.
	ActivityLockTimeout time.Duration
}

// Start builds the engine, registers r and starts the worker. The caller must call
// Shutdown, or kill the process, which is what experiment 16 does.
func Start(ctx context.Context, r *task.TaskRegistry, o Options) (*Engine, error) {
	opts, err := options(o)
	if err != nil {
		return nil, err
	}
	logger := NewLogger()
	be := postgres.NewPostgresBackend(opts, logger)
	executor := task.NewTaskExecutorWithLogger(r, logger)
	workflowWorker := backend.NewWorkflowWorker(backend.WorkflowWorkerOptions{
		Backend:  be,
		Executor: executor,
		Logger:   logger,
	})
	activityWorker := backend.NewActivityTaskWorker(be, executor, logger)
	worker := backend.NewTaskHubWorker(be, workflowWorker, activityWorker, logger)
	if err := worker.Start(ctx); err != nil {
		return nil, fmt.Errorf("start the durabletask worker: %w", err)
	}
	return &Engine{Backend: be, Worker: worker, Client: backend.NewTaskHubClient(be)}, nil
}

// StartClient opens the backend without a worker, for a process that only starts or
// signals instances and must not execute any of them itself. The library creates its
// connection pool in CreateTaskHub, so that is what opens it; the script it runs is
// CREATE TABLE IF NOT EXISTS and changes nothing that is already there.
func StartClient(ctx context.Context, o Options) (*Engine, error) {
	opts, err := options(o)
	if err != nil {
		return nil, err
	}
	be := postgres.NewPostgresBackend(opts, NewLogger())
	if err := be.CreateTaskHub(ctx); err != nil {
		return nil, fmt.Errorf("open the durabletask backend: %w", err)
	}
	return &Engine{Backend: be, Client: backend.NewTaskHubClient(be)}, nil
}

// Shutdown stops the worker and lets go of the database. It leaves the tables alone:
// the run's whole schema is dropped by the driver at the end.
func (e *Engine) Shutdown(ctx context.Context) {
	if e == nil || e.Worker == nil {
		return
	}
	_ = e.Worker.Shutdown(ctx)
}

// options turns the experiment's DSN into the library's own options struct, pointed
// at the evaluation database with the run's schema as its search path.
func options(o Options) (*postgres.PostgresOptions, error) {
	u, err := url.Parse(o.DSN)
	if err != nil {
		return nil, fmt.Errorf("-postgres-dsn %q is not a URL: %w", o.DSN, err)
	}
	host := u.Hostname()
	port, err := strconv.ParseUint(portOf(u), 10, 16)
	if err != nil {
		return nil, fmt.Errorf("-postgres-dsn %q has no usable port: %w", o.DSN, err)
	}
	password, _ := u.User.Password()
	opts := postgres.NewPostgresOptions(host, uint16(port), libx.EvalDatabase, u.User.Username(), password)
	opts.WorkflowLockTimeout = o.WorkflowLockTimeout
	opts.ActivityLockTimeout = o.ActivityLockTimeout
	opts.PgOptions.MaxConns = maxConnections
	// Every table the library creates and reads is unqualified, so the search path
	// is what puts this run's state in this run's schema.
	opts.PgOptions.ConnConfig.RuntimeParams["search_path"] = o.Schema
	return opts, nil
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "5432"
}

// Instance is one row of the Instances table: where an instance stands, who holds it
// and until when.
type Instance struct {
	// Found is false when no row exists for the instance at all.
	Found bool
	// InstanceID, ExecutionID and Name are the instance's identity.
	InstanceID  string
	ExecutionID string
	Name        string
	// RuntimeStatus is PENDING, RUNNING, COMPLETED, FAILED, TERMINATED or SUSPENDED.
	RuntimeStatus string
	// LockedBy is the worker holding the instance, written as hostname,pid,uuid, and
	// LockExpiration is when that hold lapses. A worker that dies leaves both as
	// they were.
	LockedBy       *string
	LockExpiration *time.Time
	CompletedTime  *time.Time
	// Version is the workflow version the instance is pinned to, which is how the
	// library keeps a recorded run on the code it started under.
	Version *string
	Output  *string
	Failure []byte
}

// String is the one-line form a fact prints.
func (i Instance) String() string {
	if !i.Found {
		return "no row"
	}
	return fmt.Sprintf("RuntimeStatus=%s Version=%s LockedBy=%s LockExpiration=%s CompletedTime=%s Output=%s FailureDetails=%s",
		i.RuntimeStatus, orNone(i.Version), orNone(i.LockedBy), stamp(i.LockExpiration), stamp(i.CompletedTime),
		orNone(i.Output), failure(i.Failure))
}

// Completed reports whether the instance reached a status it cannot leave on its own.
func (i Instance) Completed() bool {
	switch i.RuntimeStatus {
	case "COMPLETED", "FAILED", "TERMINATED":
		return true
	}
	return false
}

// ReadInstance reads one Instances row with SQL.
func ReadInstance(ctx context.Context, db *sql.DB, instanceID string) (Instance, error) {
	var i Instance
	err := db.QueryRowContext(ctx, `SELECT instanceid, executionid, name, runtimestatus,
		lockedby, lockexpiration, completedtime, version, output, failuredetails
		FROM instances WHERE instanceid = $1`, instanceID).
		Scan(&i.InstanceID, &i.ExecutionID, &i.Name, &i.RuntimeStatus,
			&i.LockedBy, &i.LockExpiration, &i.CompletedTime, &i.Version, &i.Output, &i.Failure)
	if err != nil {
		if err == sql.ErrNoRows {
			return Instance{}, nil
		}
		return Instance{}, fmt.Errorf("read Instances row for %s: %w", instanceID, err)
	}
	i.Found = true
	return i, nil
}

// WaitForInstance polls the Instances row until pass accepts it, and fails with the
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

// Message is one row of NewEvents or NewTasks: a piece of work waiting to be picked
// up, or picked up and not finished. NewEvents is what wakes an instance; NewTasks is
// an activity to run.
type Message struct {
	// Table is which of the two tables the row is in.
	Table string
	// SequenceNumber is the row's identity, and Event names what it carries.
	SequenceNumber int64
	Event          string
	// DequeueCount is how often the row has been handed to a worker. It is the
	// nearest thing in this schema to an attempt counter.
	DequeueCount int
	// LockedBy is the worker that holds it, written as hostname,pid,uuid.
	LockedBy *string
	// LockExpiration is when that hold lapses. NewEvents has no such column: a
	// workflow work item's lock lives on the Instances row instead.
	LockExpiration *time.Time
	VisibleTime    *time.Time
}

// String is the one-line form a fact prints.
func (m Message) String() string {
	return fmt.Sprintf("%s seq=%d event=%s DequeueCount=%d LockedBy=%s LockExpiration=%s VisibleTime=%s",
		m.Table, m.SequenceNumber, m.Event, m.DequeueCount, orNone(m.LockedBy),
		stamp(m.LockExpiration), stamp(m.VisibleTime))
}

// ReadNewEvents reads the NewEvents rows of one instance with SQL.
func ReadNewEvents(ctx context.Context, db *sql.DB, instanceID string) ([]Message, error) {
	return readMessages(ctx, db, instanceID, "newevents",
		`SELECT sequencenumber, dequeuecount, lockedby, NULL::timestamp, visibletime, eventpayload
		 FROM newevents WHERE instanceid = $1 ORDER BY sequencenumber`)
}

// ReadNewTasks reads the NewTasks rows of one instance with SQL: the activities that
// have been scheduled and not yet completed.
func ReadNewTasks(ctx context.Context, db *sql.DB, instanceID string) ([]Message, error) {
	return readMessages(ctx, db, instanceID, "newtasks",
		`SELECT sequencenumber, dequeuecount, lockedby, lockexpiration, NULL::timestamp, eventpayload
		 FROM newtasks WHERE instanceid = $1 ORDER BY sequencenumber`)
}

func readMessages(ctx context.Context, db *sql.DB, instanceID, table, query string) ([]Message, error) {
	rows, err := db.QueryContext(ctx, query, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read %s rows for %s: %w", table, instanceID, err)
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		m := Message{Table: table}
		var payload []byte
		if err := rows.Scan(&m.SequenceNumber, &m.DequeueCount, &m.LockedBy,
			&m.LockExpiration, &m.VisibleTime, &payload); err != nil {
			return nil, fmt.Errorf("scan %s row for %s: %w", table, instanceID, err)
		}
		m.Event = eventNameOf(payload)
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s rows for %s: %w", table, instanceID, err)
	}
	return messages, nil
}

// ReadHistory is the event names of one instance's History rows, in order. The
// history is what the engine replays, so its shape is what a changed workflow is
// measured against.
func ReadHistory(ctx context.Context, db *sql.DB, instanceID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT eventpayload FROM history
		WHERE instanceid = $1 ORDER BY sequencenumber`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read History rows for %s: %w", instanceID, err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan History row for %s: %w", instanceID, err)
		}
		events = append(events, eventNameOf(payload))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read History rows for %s: %w", instanceID, err)
	}
	return events, nil
}

// Events is a list of event names as one line, for a fact.
func Events(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d: %s", len(names), strings.Join(names, " "))
}

// CountEvents is how many of one event name the list holds.
func CountEvents(names []string, name string) int {
	n := 0
	for _, e := range names {
		if e == name {
			n++
		}
	}
	return n
}

// Messages is a list of rows as one line, for a fact.
func Messages(messages []Message) string {
	if len(messages) == 0 {
		return "no rows"
	}
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, " | ")
}

// eventNameOf is what a stored event payload is: the name of the protobuf oneof the
// library put in it, which is the library's own name for the event.
func eventNameOf(payload []byte) string {
	e, err := backend.UnmarshalHistoryEvent(payload)
	if err != nil {
		return fmt.Sprintf("undecodable(%d bytes)", len(payload))
	}
	name := strings.TrimPrefix(fmt.Sprintf("%T", e.GetEventType()), "*protos.HistoryEvent_")
	if ts := e.GetTaskScheduled(); ts != nil {
		return name + "(" + ts.GetName() + ")"
	}
	if er := e.GetEventRaised(); er != nil {
		return name + "(" + er.GetName() + ")"
	}
	return name
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
	if len(*s) > 200 {
		return (*s)[:200] + fmt.Sprintf("... (%d bytes)", len(*s))
	}
	return *s
}

func failure(b []byte) string {
	if len(b) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d bytes recorded", len(b))
}
