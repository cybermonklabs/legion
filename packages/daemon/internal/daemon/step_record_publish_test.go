package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/steprecord"

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

// stepRefusedLine is the one line that says Dispatch refused a write of a step record for good:
// the record's own, so the record ended unwritten, or its variant's, so the record stays as
// Dispatch took it and names no variant.
const stepRefusedLine = "outbox step record refused by Dispatch and dropped"

// stepPut is one step write as Dispatch received it, the rating it carried on the record's rating
// route (nil for a write of any other route), and the experiment and the variant it carried on the
// record's experiment route (nil for a write of any other route).
type stepPut struct {
	Issue      string
	ID         string
	Write      dispatch.StepWrite
	Rating     *dispatch.PlanRating
	Experiment *dispatch.StepExperiment
}

// stepRecordDispatch is Dispatch as the record lane meets it (record.LaneRecord): what each PUT of
// a step record or a plan gets back, each PUT received, and the status writes the issue's board
// lane made. A hold that is not nil keeps every PUT of a record waiting until it closes, as a
// Dispatch that hangs on the route. It is read while a lane runs, which outboxDispatch, whose other
// methods it keeps, is not made for.
type stepRecordDispatch struct {
	*outboxDispatch
	mu sync.Mutex
	// answer is what the PUT numbered n, counting from zero over every route, gets back; nil is
	// the record Dispatch stored.
	answer   func(n int) error
	hold     chan struct{}
	puts     []stepPut
	plans    []planPut
	statuses []string
}

func (d *stepRecordDispatch) PutStep(ctx context.Context, key, id string, write dispatch.StepWrite) error {
	return d.putStep(ctx, stepPut{Issue: key, ID: id, Write: write})
}

func (d *stepRecordDispatch) PutStepRating(ctx context.Context, key, id string, write dispatch.StepWrite, rating dispatch.PlanRating) error {
	return d.putStep(ctx, stepPut{Issue: key, ID: id, Write: write, Rating: &rating})
}

func (d *stepRecordDispatch) PutStepExperiment(ctx context.Context, key, id string, write dispatch.StepWrite, experiment dispatch.StepExperiment) error {
	return d.putStep(ctx, stepPut{Issue: key, ID: id, Write: write, Experiment: &experiment})
}

// putStep records put, a write of any of the step's routes, and answers it.
func (d *stepRecordDispatch) putStep(ctx context.Context, put stepPut) error {
	d.mu.Lock()
	d.puts = append(d.puts, put)
	n, answer, hold := len(d.puts)+len(d.plans)-1, d.answer, d.hold
	d.mu.Unlock()
	return recordPutAnswer(ctx, n, answer, hold)
}

func (d *stepRecordDispatch) PutPlan(ctx context.Context, key, id string, plan dispatch.PlanWrite) error {
	d.mu.Lock()
	d.plans = append(d.plans, planPut{Issue: key, ID: id, Plan: plan})
	n, answer, hold := len(d.puts)+len(d.plans)-1, d.answer, d.hold
	d.mu.Unlock()
	return recordPutAnswer(ctx, n, answer, hold)
}

// recordPutAnswer is what the PUT numbered n gets back from answer, once hold, if not nil, closes.
func recordPutAnswer(ctx context.Context, n int, answer func(int) error, hold chan struct{}) error {
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

func (d *stepRecordDispatch) receivedPlans() []planPut {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.plans)
}

func (d *stepRecordDispatch) statusWrites() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.statuses)
}

// stepRecordOf is the end of a review that asked for changes, with every field the end of a step
// carries set: all but the rating and its reason, which only the retro's rating carries
// (ratingRecordOf). It names a fix's check too, which only a fix's record carries, so that one
// fixture sets every field of the record's commits.
func stepRecordOf() record.StepRecord {
	started := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	session, model, result, report := "ses-reviewer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	startCommit, endCommit, fixCheck := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "k-ci"
	returnedTo := 2
	return record.StepRecord{ID: stepRecordID, Task: "default", Step: 4, Kind: "step", Role: "reviewer", Type: "review",
		Agent: "legion-legion-legion-1-reviewer", Generation: 1, Session: &session, Model: &model, StartedAt: &started,
		EndedAt: &ended, Result: &result, ReturnedTo: &returnedTo, Report: &report,
		StartCommit: &startCommit, EndCommit: &endCommit, FixCheck: &fixCheck}
}

// stepRecordRig is one issue's row on the record lane, a step record or a plan, drained by hand
// (RunOnce) on a clock the test holds, with the runner's log captured.
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
	return newStepRecordRigOf(t, stepRecordOf(), answer)
}

// newStepRecordRigOf is the rig with rec queued as the issue's row on the record lane.
func newStepRecordRigOf(t *testing.T, rec record.OutboxPayload, answer func(int) error) *stepRecordRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	logs := &syncBuffer{}
	client := &stepRecordDispatch{outboxDispatch: &outboxDispatch{}, answer: answer}
	rig := &stepRecordRig{t: t, pool: pool, client: client, clock: microClock(), logs: logs}
	rig.runner = &outbox{pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: client,
		log: slog.New(slog.NewJSONHandler(logs, nil)), now: rig.clock.Now}
	enqueueOutbox(t, pool, record.NewStore(), mustOutboxRow(t, stepIssue, rec, rig.clock.Now()))
	return rig
}

// run drains every row due now, then moves the clock past the longest backoff, so a row the run
// retried is due at the next run.
func (r *stepRecordRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
	r.clock.advance(2 * record.RetryBackoffMost)
}

// left is how many rows the issue has left.
func (r *stepRecordRig) left() int { return outboxRowsOf(r.t, r.pool, stepIssue) }

// endedWithOneWarning holds that the rig's row ended with one Warn line, line, naming its issue,
// its record under idKey (step for a step record, plan for a plan) and the status and code
// Dispatch refused it with, and that its failure is not logged as a failed attempt besides.
func (r *stepRecordRig) endedWithOneWarning(line, idKey, id string, refusal *dispatch.Error) {
	t := r.t
	t.Helper()
	logs := r.logs.String()
	warned := loggedLines(t, logs, `"level":"WARN"`)
	if len(warned) != 1 || warned[0]["msg"] != line {
		t.Fatalf("warnings = %v, want the one %q: %s", warned, line, logs)
	}
	for key, want := range map[string]any{"issue": stepIssue, idKey: id, "code": refusal.Code, "status": float64(refusal.Status)} {
		if warned[0][key] != want {
			t.Errorf("%q names %s %v, want %v", line, key, warned[0][key], want)
		}
	}
	if failed := loggedLines(t, logs, "outbox row failed"); len(failed) != 0 {
		t.Fatalf("logged %d failed attempts, want none for a refusal no retry changes: %s", len(failed), logs)
	}
}

// waitedNeverFailed holds that the rig's runs logged no ERROR line and never dropped, the line
// that says the write was dropped, and that each wait was logged at levels, in order.
func (r *stepRecordRig) waitedNeverFailed(dropped string, levels []string) {
	t := r.t
	t.Helper()
	logs := r.logs.String()
	if errored := loggedLines(t, logs, `"level":"ERROR"`); len(errored) != 0 {
		t.Errorf("logged %d ERROR lines while Dispatch had no route, want none: %s", len(errored), logs)
	}
	if said := loggedLines(t, logs, dropped); len(said) != 0 {
		t.Errorf("logged the write dropped %d times, want never: %s", len(said), logs)
	}
	var logged []string
	for _, line := range loggedLines(t, logs, `"msg":"outbox row waits"`) {
		level, _ := line["level"].(string)
		logged = append(logged, level)
	}
	if !slices.Equal(logged, levels) {
		t.Fatalf("the waits logged at %v, want %v:\n%s", logged, levels, logs)
	}
}

// retriedThenLanded drains the rig's one row refused times, each run keeping it for its retry,
// then once more, which lands it: refused+1 PUTs of the same record under the same id, one failed
// attempt logged for each refusal and no warning, since nothing was dropped. It answers the PUTs.
func (r *stepRecordRig) retriedThenLanded(refused int) []stepPut {
	t := r.t
	t.Helper()
	for attempt := 1; attempt <= refused; attempt++ {
		r.run()
		if left := r.left(); left != 1 {
			t.Fatalf("rows left after attempt %d = %d, want the record kept for its retry", attempt, left)
		}
		if puts := r.client.received(); len(puts) != attempt {
			t.Fatalf("PUTs of the record after attempt %d = %d, want one for each attempt", attempt, len(puts))
		}
	}
	r.run()
	if left := r.left(); left != 0 {
		t.Fatalf("rows left after Dispatch took the record = %d, want none", left)
	}
	puts := r.client.received()
	if len(puts) != refused+1 {
		t.Fatalf("PUTs of the record = %d, want %d: %d Dispatch refused and one it stored", len(puts), refused+1, refused)
	}
	for i, put := range puts[1:] {
		if !reflect.DeepEqual(put, puts[0]) {
			t.Fatalf("PUT %d wrote %+v, want the same record under the same id as the first, %+v", i+1, put, puts[0])
		}
	}
	logs := r.logs.String()
	if failed := loggedLines(t, logs, "outbox row failed"); len(failed) != refused {
		t.Fatalf("logged %d failed attempts, want the %d Dispatch refused: %s", len(failed), refused, logs)
	}
	if warned := loggedLines(t, logs, `"level":"WARN"`); len(warned) != 0 {
		t.Fatalf("logged %d warnings, want none for a record that landed: %s", len(warned), logs)
	}
	return puts
}

// A Dispatch older than the route answers each write with its router's 404 NOT_FOUND "no route
// for …" (routes/router.go, staticHandler), and the daemon and Dispatch deploy in either order.
// So the row waits rather than fails, as the role holders' publication does
// (TestARoleHoldersPublicationWaitsForADispatchThatHasNoRouteForIt): it logs `outbox row waits` and
// no ERROR line, at INFO on its first run, DEBUG on the next six, and WARN from its seventh retry,
// when its backoff has reached its cap of about a minute (waitWarnAttempts); each run keeps the row,
// and nothing says the record was dropped, because it was not. Once Dispatch serves the route, the
// next run writes the whole record, under its issue and its id, and ends the row. The stand-in
// answers as the router does, through the daemon's real HTTP client, so the client's reading of
// that body is what decides.
func TestAStepRecordWaitsForADispatchThatHasNoRouteForIt(t *testing.T) {
	t.Parallel()
	rig := newStepRecordRig(t, nil)
	rig.runner.log = slog.New(slog.NewJSONHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	route := "/api/v1/issues/" + stepIssue + "/steps/" + stepRecordID

	var mu sync.Mutex
	routed, asked := false, 0
	var landed []stepPut
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		w.Header().Set("Content-Type", "application/json")
		if !routed || req.Method != http.MethodPut || (req.URL.Path != route && req.URL.Path != route+"/commits") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "NOT_FOUND", "error": "no route for " + req.Method + " " + req.URL.Path, "hint": "GET /api/v1 lists every route",
			})
			return
		}
		// The record names its commits, which the record's commits route takes after the record
		// (TestAnOlderDispatchKeepsTheRowWaiting holds that route's own wait); a Dispatch that
		// serves the step route here serves that one too.
		if req.URL.Path == route+"/commits" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": stepRecordID})
			return
		}
		var write dispatch.StepWrite
		if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		landed = append(landed, stepPut{Issue: stepIssue, ID: stepRecordID, Write: write})
		_ = json.NewEncoder(w).Encode(map[string]string{"id": stepRecordID})
	}))
	t.Cleanup(server.Close)
	rig.runner.dispatch = dispatch.New(server.URL, dispatch.BearerTransport("agent-token", nil))

	const tries = waitWarnAttempts + 1
	for try := 1; try <= tries; try++ {
		rig.run()
		var attempts int
		if err := rig.pool.QueryRow(context.Background(), `select attempts from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindStepRecord), stepIssue).Scan(&attempts); err != nil {
			t.Fatalf("after try %d, read the row: %v", try, err)
		}
		if attempts != try {
			t.Fatalf("after try %d the row has %d attempts, want %d: the record kept for its next run", try, attempts, try)
		}
		mu.Lock()
		reached := asked
		mu.Unlock()
		if reached != try {
			t.Fatalf("after try %d the stand-in was asked %d times, want once a try", try, reached)
		}
	}

	mu.Lock()
	routed = true
	mu.Unlock()
	rig.run()
	started, ended := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	session, model, result, report := "ses-reviewer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	returnedTo := 2
	want := []stepPut{{Issue: stepIssue, ID: stepRecordID, Write: dispatch.StepWrite{Task: "default", Step: 4, Kind: "step",
		Role: "reviewer", Type: "review", Agent: "legion-legion-legion-1-reviewer", Generation: 1, Session: &session,
		Model: &model, StartedAt: &started, EndedAt: &ended, Result: &result, ReturnedTo: &returnedTo, Report: &report}}}
	if !reflect.DeepEqual(landed, want) {
		t.Fatalf("once the route was served the stand-in took %+v, want %+v", landed, want)
	}
	if left := rig.left(); left != 0 {
		t.Fatalf("after the record landed the issue has %d rows left, want none", left)
	}

	rig.waitedNeverFailed(stepRefusedLine, []string{"INFO", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "WARN"})
}

// The experiment and the variant of a record's issue reach Dispatch after the record's own write,
// and only once that write was taken (outbox.run): a Dispatch before the experiment route then
// takes the record and makes only the pair wait, and no pair is ever sent ahead of its record. A
// start that names the pair is two writes, the start's on the step route and then the pair's on
// the experiment route; a retro's rating that names it is the rating's on the rating route and
// then the pair's; a start that names none is the one write it was. Where Dispatch does not take
// the record's write (a 503 here), the run ends there, with no write of the pair, and the next
// run sends the record again and then the pair.
func TestAStepRecordSendsItsVariantAfterItsRecord(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)
	session := "ses-implementer"
	pair := &record.StepExperiment{Experiment: "planning", Variant: "b"}
	start := record.StepRecord{ID: stepRecordID, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-legion-legion-1-implementer", Generation: 1, Session: &session, StartedAt: &started}
	named := start
	named.StepExperiment = pair
	rated := ratingRecordOf()
	rated.StepExperiment = pair
	// routeOf names the route a write was sent to.
	routeOf := func(put stepPut) string {
		switch {
		case put.Rating != nil:
			return "rating"
		case put.Experiment != nil:
			return "experiment"
		}
		return "step"
	}
	outage := &dispatch.Error{Status: http.StatusServiceUnavailable, Message: "maintenance"}
	for _, tc := range []struct {
		name string
		rec  record.StepRecord
		// answers is what Dispatch answers each write in turn, nil past its end.
		answers []error
		// first and all are the routes written after the first run and after the last.
		first, all []string
	}{
		{name: "a start that names the pair", rec: named, first: []string{"step", "experiment"}, all: []string{"step", "experiment"}},
		{name: "a retro's rating that names the pair", rec: rated, first: []string{"rating", "experiment"}, all: []string{"rating", "experiment"}},
		{name: "a start that names no pair", rec: start, first: []string{"step"}, all: []string{"step"}},
		{name: "a start that names the pair, whose record Dispatch does not take at first", rec: named, answers: []error{outage},
			first: []string{"step"}, all: []string{"step", "step", "experiment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStepRecordRigOf(t, tc.rec, answersInTurn(tc.answers...))
			routes := func() []string {
				var routes []string
				for _, put := range rig.client.received() {
					if put.Issue != stepIssue || put.ID != tc.rec.ID {
						t.Errorf("a write went to record %s under %s, want record %s under %s", put.ID, put.Issue, tc.rec.ID, stepIssue)
					}
					routes = append(routes, routeOf(put))
				}
				return routes
			}
			rig.run()
			if got := routes(); !slices.Equal(got, tc.first) {
				t.Fatalf("after the first run the writes went to %q, want %q", got, tc.first)
			}
			for range len(tc.answers) {
				rig.run()
			}
			if got := routes(); !slices.Equal(got, tc.all) {
				t.Fatalf("the writes went to %q, want %q", got, tc.all)
			}
			if left := rig.left(); left != 0 {
				t.Fatalf("after Dispatch took every write the issue has %d rows left, want none", left)
			}
		})
	}
}

// A Dispatch from before the experiment route serves the step route and answers the experiment
// route its router's 404 NOT_FOUND "no route for …". A step record that names its issue's variant
// still lands there, and only the variant waits: the run writes the record first and the variant
// second, and execute makes the experiment route's NOT_FOUND a wait
// (TestAStepRecordWaitsForADispatchThatHasNoRouteForIt). So after the first run of the start and
// of the end of one step, two rows, Dispatch holds the record with every field of both and no
// experiment or variant, both rows are still queued, and each logged `outbox row waits` at INFO and
// no ERROR line. A later run sends each record again ahead of its variant, which changes nothing
// Dispatch holds, since Dispatch keeps each field a record already holds. Once Dispatch serves the
// route, the next run writes the variant and ends both rows. The stand-in is the live proofs'
// board (liveDispatch), which takes each route as Dispatch does, behind a router that answers the
// experiment route as an older Dispatch does until the test serves it; the runner's client is the
// daemon's real HTTP client, so its requests and their order decide.
func TestAStepRecordLandsWhileItsVariantWaitsForItsRoute(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	session, model, result, report := "ses-implementer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	start := record.StepRecord{ID: stepRecordID, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-legion-legion-1-implementer", Generation: 1,
		StepExperiment: &record.StepExperiment{Experiment: "planning", Variant: "b"}}
	end := start
	start.Session, start.StartedAt = &session, &started
	end.Session, end.Model, end.EndedAt, end.Result, end.Report = &session, &model, &ended, &result, &report
	rig := newStepRecordRigOf(t, start, nil)
	rig.runner.log = slog.New(slog.NewJSONHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	enqueueOutbox(t, rig.pool, record.NewStore(), mustOutboxRow(t, stepIssue, end, rig.clock.Now()))

	board := newLiveDispatch(t, "LEGION")
	stepRoute := "/api/v1/issues/" + stepIssue + "/steps/" + stepRecordID
	experimentRoute := stepRoute + "/experiment"
	var mu sync.Mutex
	served := false
	taken := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read the write to %s: %v", req.URL.Path, err)
			return
		}
		mu.Lock()
		taken[req.URL.Path] = append(taken[req.URL.Path], string(body))
		routed := served
		mu.Unlock()
		if req.URL.Path == experimentRoute && !routed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "NOT_FOUND", "error": "no route for " + req.Method + " " + req.URL.Path, "hint": "GET /api/v1 lists every route",
			})
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		board.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	rig.runner.dispatch = dispatch.New(server.URL, dispatch.BearerTransport("agent-token", nil))
	held := func() map[string]any {
		board.mu.Lock()
		defer board.mu.Unlock()
		return maps.Clone(board.steps[stepRecordID])
	}
	stepBodies := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(taken[stepRoute])
	}
	want := map[string]any{"id": stepRecordID, "issue": stepIssue, "task": "default", "step": float64(2), "kind": "step",
		"role": "implementer", "type": "implementation", "agent": "legion-legion-legion-1-implementer", "generation": float64(1),
		"session": session, "model": model, "started_at": "2026-11-16T22:00:00Z", "ended_at": "2026-11-16T23:00:00Z",
		"result": result, "report": report}

	rig.run()
	if got := held(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the first run Dispatch holds the record %v, want every field of the start and the end and no variant, %v", got, want)
	}
	if left := rig.left(); left != 2 {
		t.Fatalf("after the first run the issue has %d rows left, want the start and the end kept for their variant", left)
	}
	logs := rig.logs.String()
	var levels []string
	for _, line := range loggedLines(t, logs, `"msg":"outbox row waits"`) {
		level, _ := line["level"].(string)
		levels = append(levels, level)
	}
	if !slices.Equal(levels, []string{"INFO", "INFO"}) {
		t.Fatalf("the first run logged its waits at %v, want one INFO wait for each row:\n%s", levels, logs)
	}
	if errored := loggedLines(t, logs, `"level":"ERROR"`); len(errored) != 0 {
		t.Fatalf("logged %d ERROR lines while the variant waited for its route, want none: %s", len(errored), logs)
	}

	before := held()
	rig.run()
	if left := rig.left(); left != 2 {
		t.Fatalf("after the second run the issue has %d rows left, want both still waiting for their variant", left)
	}
	counts := map[string]int{}
	for _, body := range stepBodies() {
		counts[body]++
	}
	twice := len(counts) == 2
	for _, n := range counts {
		twice = twice && n == 2
	}
	if !twice {
		t.Fatalf("the step route took %q, want each of the two writes twice with the same body", stepBodies())
	}
	if got := held(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the writes sent again changed the record Dispatch holds from %v to %v", before, got)
	}

	mu.Lock()
	served = true
	mu.Unlock()
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("once Dispatch served the route the issue has %d rows left, want none", left)
	}
	want["experiment"], want["variant"] = "planning", "b"
	if got := held(); !reflect.DeepEqual(got, want) {
		t.Fatalf("once Dispatch served the route it holds the record %v, want %v", got, want)
	}
	if dropped := loggedLines(t, rig.logs.String(), stepRefusedLine); len(dropped) != 0 {
		t.Errorf("logged a record dropped %d times, want never: %s", len(dropped), rig.logs.String())
	}
}

// A step record and the writes Dispatch gets are the same fields, the record's id aside, and the
// publication copies them one by one (stepWriteOf), since the dispatch package imports no record
// of the daemon's. A field added to both types and left out of the copy compiles and drops that
// field from every write. So the record's fields but its rating, its commits and its experiment
// are held to the step write's, each of the same type under the same JSON name, and its rating
// (record.PlanRating) and its experiment (record.StepExperiment) to the write's mirrors of them in
// dispatch (dispatch.PlanRating and dispatch.StepExperiment), the rating route's and the
// experiment route's own arguments (Client.PutStepRating, Client.PutStepExperiment), which the copy
// converts: each pair of structs has the same fields, each of the same type under the same JSON
// name, so a field added to one alone fails here or stops the build. The record's two commits and
// a fix's check (StartCommit, EndCommit, FixCheck) are no field of the step write either: they go
// to Dispatch on a route of their own. The fixtures are the two writes production makes, to a
// value in every field between them, which a new field must get in the write that carries it: the
// end of a step carries every field but the rating, the retro's rating (workflow's rateRetro)
// carries the step's identity and the rating, and each, as the write of an issue a draw put in an
// experiment, carries the experiment and the variant. The run (outbox.run) sends each to its own
// route, the commits a record names to the commits route, and then its pair to the experiment
// route, and each write carries its own record's value of each field.
func TestAStepRecordReachesDispatchWithEveryField(t *testing.T) {
	t.Parallel()
	recordType, writeType := reflect.TypeFor[record.StepRecord](), reflect.TypeFor[dispatch.StepWrite]()
	recordFields := map[string]reflect.StructField{}
	for i := range recordType.NumField() {
		field := recordType.Field(i)
		if name := field.Name; name != "ID" && name != "PlanRating" && name != "StepExperiment" &&
			name != "StartCommit" && name != "EndCommit" && name != "FixCheck" {
			recordFields[field.Name] = field
		}
	}
	if len(recordFields) != writeType.NumField() {
		t.Errorf("record.StepRecord has %d fields beside its ID, its rating, its commits and its experiment, dispatch.StepWrite %d: a field of one is not a field of the other",
			len(recordFields), writeType.NumField())
	}
	for i := range writeType.NumField() {
		field := writeType.Field(i)
		mirror, ok := recordFields[field.Name]
		if !ok || mirror.Type != field.Type || mirror.Tag.Get("json") != field.Tag.Get("json") {
			t.Errorf("dispatch.StepWrite's %s is %s %q; record.StepRecord's is %s %q (present: %t)", field.Name,
				field.Type, field.Tag.Get("json"), mirror.Type, mirror.Tag.Get("json"), ok)
		}
	}
	for _, mirrored := range [][2]reflect.Type{
		{reflect.TypeFor[record.PlanRating](), reflect.TypeFor[dispatch.PlanRating]()},
		{reflect.TypeFor[record.StepExperiment](), reflect.TypeFor[dispatch.StepExperiment]()},
	} {
		recordMirror, writeMirror := mirrored[0], mirrored[1]
		if recordMirror.NumField() != writeMirror.NumField() {
			t.Errorf("%s has %d fields, %s %d", recordMirror, recordMirror.NumField(), writeMirror, writeMirror.NumField())
			continue
		}
		for i := range writeMirror.NumField() {
			w, r := writeMirror.Field(i), recordMirror.Field(i)
			if w.Name != r.Name || w.Type != r.Type || w.Tag.Get("json") != r.Tag.Get("json") {
				t.Errorf("%s's field %d is %s %s %q; %s's is %s %s %q", writeMirror, i,
					w.Name, w.Type, w.Tag.Get("json"), recordMirror, r.Name, r.Type, r.Tag.Get("json"))
			}
		}
	}
	end, rating := stepRecordOf(), ratingRecordOf()
	end.StepExperiment = &record.StepExperiment{Experiment: "planning", Variant: "b"}
	rating.StepExperiment = &record.StepExperiment{Experiment: "planning", Variant: "a"}
	fixtures := []struct {
		name string
		rec  record.StepRecord
	}{
		{"the end of a review that asked for changes", end},
		{"the rating of a retro", rating},
	}
	for i := range recordType.NumField() {
		set := false
		for _, fixture := range fixtures {
			set = set || !reflect.ValueOf(fixture.rec).Field(i).IsZero()
		}
		if !set {
			t.Errorf("no fixture sets %s, so a write that dropped it would pass", recordType.Field(i).Name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// shown reads a pointer through, so a failure prints the value rather than an address.
	shown := func(v reflect.Value) any {
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			return v.Elem().Interface()
		}
		return v.Interface()
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			rec := fixture.rec
			client := &stepRecordDispatch{outboxDispatch: &outboxDispatch{}}
			runner := &outbox{dispatch: client, log: quietLogger()}
			if err := runner.run(context.Background(), mustOutboxRow(t, stepIssue, rec, time.Now())); err != nil {
				t.Fatalf("publish the record: %v", err)
			}
			puts := client.received()
			if len(puts) != 2 {
				t.Fatalf("PUTs = %+v, want the record's own write and then its variant's", puts)
			}
			var rating *dispatch.PlanRating
			if rec.PlanRating != nil {
				rating = new(dispatch.PlanRating(*rec.PlanRating))
			}
			for n, sent := range []struct {
				route      string
				rating     *dispatch.PlanRating
				experiment *dispatch.StepExperiment
			}{
				{"the record's own route", rating, nil},
				{"the experiment route", nil, new(dispatch.StepExperiment(*rec.StepExperiment))},
			} {
				put := puts[n]
				if put.Issue != stepIssue || put.ID != rec.ID {
					t.Errorf("the write to %s is of record %s under %s, want record %s under %s", sent.route, put.ID, put.Issue, rec.ID, stepIssue)
				}
				written, value := reflect.ValueOf(put.Write), reflect.ValueOf(rec)
				for i := range writeType.NumField() {
					field := writeType.Field(i)
					if got, want := written.Field(i), value.FieldByName(field.Name); !reflect.DeepEqual(got.Interface(), want.Interface()) {
						t.Errorf("the write to %s has %s = %v, want the record's %v", sent.route, field.Name, shown(got), shown(want))
					}
				}
				if !reflect.DeepEqual(put.Rating, sent.rating) {
					t.Errorf("the write to %s carries the rating %+v, want %+v", sent.route, put.Rating, sent.rating)
				}
				if !reflect.DeepEqual(put.Experiment, sent.experiment) {
					t.Errorf("the write to %s carries the experiment %+v, want %+v", sent.route, put.Experiment, sent.experiment)
				}
			}
		})
	}
}

// A merge that moves back to the retro returns its task to place 0, the retro's: the row carries
// that 0 through the store, and the write Dispatch gets names it, where a nil would read as a
// failed merge that returned nowhere.
func TestAReturnToTheRetroReachesDispatchAsPlaceZero(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	session, model, result, retro := "ses-merger", "anthropic/claude-opus-5", "failed", 0
	rig := newStepRecordRigOf(t, record.StepRecord{ID: stepRecordID, Task: "default", Kind: "merge", Role: "merger",
		Agent: "legion-legion-legion-1-merger", Generation: 1, Session: &session, Model: &model, EndedAt: &ended,
		Result: &result, ReturnedTo: &retro}, nil)
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("rows left after Dispatch took the record = %d, want none", left)
	}
	puts := rig.client.received()
	if len(puts) != 1 {
		t.Fatalf("PUTs of the record = %d, want one", len(puts))
	}
	if got := puts[0].Write.ReturnedTo; got == nil || *got != 0 {
		t.Fatalf("the write's returned_to = %v, want a pointer to 0, the retro's place", got)
	}
}

// What a retry can change is retried: an outage, "come back later", a codeless answer from a
// proxy, and a credential an operator restores, as the shared rule rides each of them out
// (dispatch.PermanentRefusal). So is a handler's 404 NOT_FOUND "not found", which is not the
// router's "no route for …" that a Dispatch older than the route answers (a wait, as
// TestAStepRecordWaitsForADispatchThatHasNoRouteForIt shows), and an older Dispatch's refusal of
// a field this daemon added. Each is the answer to a real PUT of the queued row: the row stays,
// its failed attempt is the outbox's own Error line and nothing says the record was dropped, and
// once Dispatch answers again the next run writes the same record under the same id and ends it.
func TestAStepRecordRetriesAnOutageAndACredential(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "503", err: &dispatch.Error{Status: http.StatusServiceUnavailable, Message: "maintenance"}},
		{name: "a transport failure", err: &dispatch.TransientError{Err: errors.New("dial tcp: connection refused")}},
		{name: "401", err: &dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "token expired"}},
		{name: "403 ACTOR_SESSION_MISMATCH", err: &dispatch.Error{Status: http.StatusForbidden, Code: "ACTOR_SESSION_MISMATCH", Message: "the bearer names another session"}},
		{name: "a handler's 404 NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"}},
		{name: "408", err: &dispatch.Error{Status: http.StatusRequestTimeout, Code: "TIMEOUT", Message: "slow"}},
		{name: "429", err: &dispatch.Error{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "later"}},
		{name: "a codeless 400", err: &dispatch.Error{Status: http.StatusBadRequest, Message: "Bad Request"}},
		{name: "an older Dispatch's refusal of a newer field", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON",
			Message: `invalid JSON body: json: unknown field "labels_add"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newStepRecordRig(t, answersInTurn(tc.err)).retriedThenLanded(1)
		})
	}
}

// What no retry changes ends the row after its one run, and the one Warn line says the record was
// dropped, naming the issue, the record's id and Dispatch's code, since nothing else would: an
// issue Dispatch does not have, a body Dispatch will never take, a writer the route refuses, and an
// id of another issue. The row's failure is not logged as a failed attempt besides.
func TestAStepRecordThatDispatchRefusesEndsWithAWarning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  *dispatch.Error
	}{
		{name: "404 ISSUE_NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no issue LEGION-1"}},
		{name: "400 " + steprecord.CodeInput, err: &dispatch.Error{Status: http.StatusBadRequest, Code: steprecord.CodeInput, Message: "result must be one of passed failed stopped"}},
		{name: "400 INVALID_JSON", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON", Message: "invalid JSON body: unexpected EOF"}},
		{name: "403 " + steprecord.CodeWriter, err: &dispatch.Error{Status: http.StatusForbidden, Code: steprecord.CodeWriter, Message: "only the session legion-daemon:LEGION writes a step record of LEGION-1"}},
		{name: "409 " + steprecord.CodeOtherIssue, err: &dispatch.Error{Status: http.StatusConflict, Code: steprecord.CodeOtherIssue, Message: "the step belongs to LEGION-9"}},
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
			rig.endedWithOneWarning(stepRefusedLine, "step", stepRecordID, tc.err)
		})
	}
}

// A step record runs on a lane of its own (record.LaneRecord), so a Dispatch that hangs on the
// route holds nothing else of the issue: with the record's PUT waiting, the same issue's status
// write runs on the board lane and lands.
func TestAStepRecordHoldsNoStatusWrite(t *testing.T) {
	t.Parallel()
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
	if !testwait.Holds(testwait.Remaining(t), testwait.Poll, func() bool {
		left = outboxRowsOf(t, pool, claimIssue)
		return left == 1
	}) {
		t.Fatalf("rows left on %s = %d, want the waiting record alone", claimIssue, left)
	}
}
