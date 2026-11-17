package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The issue every step record here belongs to, and the record's own id, a version 5 id of the form
// the workflow names a record by.
const (
	stepIssue    = "LEGION-1"
	stepRecordID = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"
)

// stepRefusedLine is the one line that says a step record ended unwritten.
const stepRefusedLine = "outbox step record refused by Dispatch and dropped"

// stepPut is one step write as Dispatch received it.
type stepPut struct {
	Issue string
	ID    string
	Write dispatch.StepWrite
}

// stepRecordDispatch is Dispatch as a step record meets it: what each PUT of a record gets back,
// each PUT received, and the status writes the issue's board lane made. A hold that is not nil
// keeps every PUT of a record waiting until it closes, as a Dispatch that hangs on the route. It is
// read while a lane runs, which outboxDispatch, whose other methods it keeps, is not made for.
type stepRecordDispatch struct {
	*outboxDispatch
	mu sync.Mutex
	// answer is what the PUT numbered n, counting from zero, gets back; nil is the record Dispatch
	// stored.
	answer   func(n int) error
	hold     chan struct{}
	puts     []stepPut
	statuses []string
}

func (d *stepRecordDispatch) PutStep(ctx context.Context, key, id string, write dispatch.StepWrite) error {
	d.mu.Lock()
	d.puts = append(d.puts, stepPut{Issue: key, ID: id, Write: write})
	n, answer, hold := len(d.puts)-1, d.answer, d.hold
	d.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if answer == nil {
		return nil
	}
	return answer(n)
}

func (d *stepRecordDispatch) SetStatus(_ context.Context, _ string, status string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statuses = append(d.statuses, status)
	return dispatch.Issue{}, nil
}

// SetStatusAt is the write the outbox's status row makes (record.StatusWrite's fence). Without it
// the embedded stand-in's own SetStatus would take the write, and statusWrites would show none.
func (d *stepRecordDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	return d.SetStatus(ctx, issue.Key, status)
}

func (d *stepRecordDispatch) received() []stepPut {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.puts)
}

func (d *stepRecordDispatch) statusWrites() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.statuses)
}

// stepRecordOf is the end of a review that asked for changes, the record with every field set.
func stepRecordOf() record.StepRecord {
	started := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	session, model, result, report := "ses-reviewer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	returnedTo := 2
	return record.StepRecord{ID: stepRecordID, Task: "default", Step: 4, Kind: "step", Role: "reviewer", Type: "review",
		Agent: "legion-legion-legion-1-reviewer", Generation: 1, Session: &session, Model: &model, StartedAt: &started,
		EndedAt: &ended, Result: &result, ReturnedTo: &returnedTo, Report: &report}
}

// stepRecordRig is one issue's step record row, drained by hand (RunOnce) on a clock the test
// holds, with the runner's log captured.
type stepRecordRig struct {
	t      *testing.T
	pool   *store.Pool
	runner *outbox
	client *stepRecordDispatch
	clock  *heldClock
	logs   *syncBuffer
}

func newStepRecordRig(t *testing.T, answer func(int) error) *stepRecordRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	logs := &syncBuffer{}
	client := &stepRecordDispatch{outboxDispatch: &outboxDispatch{}, answer: answer}
	rig := &stepRecordRig{t: t, pool: pool, client: client, clock: microClock(), logs: logs}
	rig.runner = &outbox{pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: client,
		log: slog.New(slog.NewJSONHandler(logs, nil)), now: rig.clock.Now}
	enqueueOutbox(t, pool, record.NewStore(), mustOutboxRow(t, stepIssue, stepRecordOf(), rig.clock.Now()))
	return rig
}

// run drains every row due now, then moves the clock past the longest backoff, so a row the run
// retried is due at the next run.
func (r *stepRecordRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
	r.clock.advance(2 * retryBackoff(6))
}

// left is how many rows the issue has left.
func (r *stepRecordRig) left() int { return outboxRowsOf(r.t, r.pool, stepIssue) }

// A Dispatch older than the route answers its router's 404 under the code NOT_FOUND, which is no
// refusal of the record: the row retries on its backoff until the upgraded Dispatch takes it, which
// is what lets the daemon and Dispatch deploy in either order. Each failed attempt is the outbox's
// own Error line, and nothing says the record was dropped, because it was not. The write that lands
// is the whole record, under its issue and its id.
func TestAStepRecordRetriesUntilDispatchHasTheRoute(t *testing.T) {
	notFound := &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route"}
	rig := newStepRecordRig(t, answersInTurn(notFound, notFound))
	for attempt := 1; attempt <= 2; attempt++ {
		rig.run()
		if left := rig.left(); left != 1 {
			t.Fatalf("rows left after attempt %d = %d, want the record retried", attempt, left)
		}
	}
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("rows left after Dispatch took the record = %d, want none", left)
	}
	puts := rig.client.received()
	if len(puts) != 3 {
		t.Fatalf("PUTs of the record = %d, want three: two the old router refused, one stored", len(puts))
	}
	started, ended := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	session, model, result, report := "ses-reviewer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	returnedTo := 2
	want := stepPut{Issue: stepIssue, ID: stepRecordID, Write: dispatch.StepWrite{Task: "default", Step: 4, Kind: "step",
		Role: "reviewer", Type: "review", Agent: "legion-legion-legion-1-reviewer", Generation: 1, Session: &session,
		Model: &model, StartedAt: &started, EndedAt: &ended, Result: &result, ReturnedTo: &returnedTo, Report: &report}}
	for i, put := range puts {
		if !reflect.DeepEqual(put, want) {
			t.Fatalf("PUT %d = %+v, want %+v", i, put, want)
		}
	}
	logs := rig.logs.String()
	if failed := loggedLines(t, logs, "outbox row failed"); len(failed) != 2 {
		t.Fatalf("logged %d failed attempts, want the 2 the old router answered: %s", len(failed), logs)
	}
	if warned := loggedLines(t, logs, `"level":"WARN"`); len(warned) != 0 {
		t.Fatalf("logged %d warnings, want none for a record that landed: %s", len(warned), logs)
	}
}

// What a retry can change is retried: an outage, "come back later", a codeless answer from a
// proxy, and a credential an operator restores, as the shared rule rides each of them out
// (dispatch.PermanentRefusal). So is the old router's NOT_FOUND, and an older Dispatch's refusal of
// a field this daemon added. None of them ends the row.
func TestAStepRecordRetriesAnOutageAndACredential(t *testing.T) {
	row := mustOutboxRow(t, stepIssue, stepRecordOf(), time.Now())
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "503", err: &dispatch.Error{Status: http.StatusServiceUnavailable, Message: "maintenance"}},
		{name: "a transport failure", err: &dispatch.TransientError{Err: errors.New("dial tcp: connection refused")}},
		{name: "401", err: &dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "token expired"}},
		{name: "403 ACTOR_SESSION_MISMATCH", err: &dispatch.Error{Status: http.StatusForbidden, Code: "ACTOR_SESSION_MISMATCH", Message: "the bearer names another session"}},
		{name: "404 NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route"}},
		{name: "408", err: &dispatch.Error{Status: http.StatusRequestTimeout, Code: "TIMEOUT", Message: "slow"}},
		{name: "429", err: &dispatch.Error{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "later"}},
		{name: "a codeless 400", err: &dispatch.Error{Status: http.StatusBadRequest, Message: "Bad Request"}},
		{name: "an older Dispatch's refusal of a newer field", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON",
			Message: `invalid JSON body: json: unknown field "labels_add"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("write the step record %s of %s: %w", stepRecordID, stepIssue, tc.err)
			if permanentRefusal(row, wrapped) {
				t.Fatalf("permanentRefusal(%v) = true, want the row retried", tc.err)
			}
		})
	}
}

// What no retry changes ends the row after its one run, and the one Warn line says the record was
// dropped, naming the issue, the record's id and Dispatch's code, since nothing else would: an
// issue Dispatch does not have, a body Dispatch will never take, a writer the route refuses, and an
// id of another issue. The row's failure is not logged as a failed attempt besides.
func TestAStepRecordThatDispatchRefusesEndsWithAWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *dispatch.Error
	}{
		{name: "404 ISSUE_NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no issue LEGION-1"}},
		{name: "400 STEP_INPUT", err: &dispatch.Error{Status: http.StatusBadRequest, Code: dispatch.StepInput, Message: "result must be one of passed failed stopped"}},
		{name: "400 INVALID_JSON", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON", Message: "invalid JSON body: unexpected EOF"}},
		{name: "403 STEP_WRITER", err: &dispatch.Error{Status: http.StatusForbidden, Code: dispatch.StepWriter, Message: "only the session legion-daemon:LEGION writes a step record of LEGION-1"}},
		{name: "409 STEP_OTHER_ISSUE", err: &dispatch.Error{Status: http.StatusConflict, Code: dispatch.StepOtherIssue, Message: "the step belongs to LEGION-9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStepRecordRig(t, answersInTurn(tc.err))
			rig.run()
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left after Dispatch refused the record = %d, want none", left)
			}
			if puts := rig.client.received(); len(puts) != 1 {
				t.Fatalf("PUTs of the record = %d, want the one Dispatch refused", len(puts))
			}
			logs := rig.logs.String()
			warned := loggedLines(t, logs, `"level":"WARN"`)
			if len(warned) != 1 || warned[0]["msg"] != stepRefusedLine {
				t.Fatalf("warnings = %v, want the one %q: %s", warned, stepRefusedLine, logs)
			}
			line := warned[0]
			for key, want := range map[string]any{"issue": stepIssue, "step": stepRecordID, "code": tc.err.Code,
				"status": float64(tc.err.Status)} {
				if line[key] != want {
					t.Errorf("%q names %s %v, want %v", stepRefusedLine, key, line[key], want)
				}
			}
			if failed := loggedLines(t, logs, "outbox row failed"); len(failed) != 0 {
				t.Fatalf("logged %d failed attempts, want none for a refusal no retry changes: %s", len(failed), logs)
			}
		})
	}
}

// A step record runs on a lane of its own (record.LaneRecord), so a Dispatch that hangs on the
// route holds nothing else of the issue: with the record's PUT waiting, the same issue's status
// write runs on the board lane and lands.
func TestAStepRecordHoldsNoStatusWrite(t *testing.T) {
	pool := isolatedOutboxPool(t)
	putOutboxIssue(t, pool, record.NewStore(), claimReleaseRoot())
	putOutboxIssue(t, pool, record.NewStore(), parkedIssue())
	clock := microClock()
	hold := make(chan struct{})
	client := &stepRecordDispatch{outboxDispatch: &outboxDispatch{issue: dispatch.Issue{Key: claimIssue, Status: "backlog"}}, hold: hold}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", dispatch: client, project: "legion"}
	runner.records = wakingRecords(runner)
	runLanes(t, runner, clock, func() { close(hold) })

	commitRows(t, pool, runner.records, "record", mustOutboxRow(t, claimIssue, stepRecordOf(), clock.Now()))
	testwait.Eventually(t, "the record's PUT reached Dispatch and waits there", func() bool {
		return len(client.received()) == 1
	})
	commitRows(t, pool, runner.records, "board",
		mustOutboxRow(t, claimIssue, record.OrdinaryStatusWrite("icebox", "backlog", ""), clock.Now()))
	testwait.Eventually(t, "the status write ran while the record's PUT waited", func() bool {
		return slices.Equal(client.statusWrites(), []string{"icebox"})
	})
	var left int
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
		left = outboxRowsOf(t, pool, claimIssue)
		return left == 1
	}) {
		t.Fatalf("rows left on %s = %d, want the waiting record alone", claimIssue, left)
	}
}
