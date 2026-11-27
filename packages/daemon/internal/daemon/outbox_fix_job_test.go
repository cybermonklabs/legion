package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// commitsPut is one write of a step record's commits route as Dispatch received it: the record's
// write, out of which the client sends the run's identity, and the commits.
type commitsPut struct {
	Issue   string
	ID      string
	Write   dispatch.StepWrite
	Commits dispatch.StepCommits
}

// commitsDispatch is Dispatch as the record lane meets it (stepRecordDispatch) with the record's
// commits route beside its step and rating routes: it keeps each write of the commits route, and
// the order the three routes were written in.
type commitsDispatch struct {
	*stepRecordDispatch
	mu      sync.Mutex
	routes  []string
	commits []commitsPut
}

func (d *commitsDispatch) wrote(route string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes = append(d.routes, route)
}

func (d *commitsDispatch) PutStep(ctx context.Context, key, id string, write dispatch.StepWrite) error {
	d.wrote("step")
	return d.stepRecordDispatch.PutStep(ctx, key, id, write)
}

func (d *commitsDispatch) PutStepRating(ctx context.Context, key, id string, write dispatch.StepWrite, rating dispatch.PlanRating) error {
	d.wrote("rating")
	return d.stepRecordDispatch.PutStepRating(ctx, key, id, write, rating)
}

func (d *commitsDispatch) PutStepCommits(_ context.Context, key, id string, write dispatch.StepWrite, commits dispatch.StepCommits) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes = append(d.routes, "commits")
	d.commits = append(d.commits, commitsPut{Issue: key, ID: id, Write: write, Commits: commits})
	return nil
}

func (d *commitsDispatch) written() ([]string, []commitsPut) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.routes), slices.Clone(d.commits)
}

// A step record's commits and a fix's check go to Dispatch on the record's commits route, after
// the record's own write: a record naming any of them is written on the step route, then on the
// commits route, with the record's identity and its value of each, and a record naming none is
// written on the step route alone. Every field of the record that is no field of the step write,
// its id and its rating aside, is a field of the commits write under the same name, so a commit
// added to the record and left out of the commits write fails here.
func TestARecordWithCommitsWritesThemAfterTheRecord(t *testing.T) {
	t.Parallel()
	recordType := reflect.TypeFor[record.StepRecord]()
	writeType, commitsType := reflect.TypeFor[dispatch.StepWrite](), reflect.TypeFor[dispatch.StepCommits]()
	for i := range recordType.NumField() {
		field := recordType.Field(i)
		if _, written := writeType.FieldByName(field.Name); written || field.Name == "ID" || field.Name == "PlanRating" {
			continue
		}
		mirror, ok := commitsType.FieldByName(field.Name)
		if !ok || field.Type != reflect.TypeFor[*string]() || mirror.Type != reflect.TypeFor[string]() {
			t.Errorf("record.StepRecord's %s (%s) is no field of the step write, and dispatch.StepCommits holds it as %s (present: %t), want a string of the same name",
				field.Name, field.Type, mirror.Type, ok)
		}
	}
	for i := range commitsType.NumField() {
		if _, ok := recordType.FieldByName(commitsType.Field(i).Name); !ok {
			t.Errorf("dispatch.StepCommits' %s is no field of record.StepRecord", commitsType.Field(i).Name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	t.Run("the end of a review that names its commits and a fix's check", func(t *testing.T) {
		t.Parallel()
		rec := stepRecordOf()
		rig := newStepRecordRigOf(t, rec, nil)
		client := &commitsDispatch{stepRecordDispatch: rig.client}
		rig.runner.dispatch = client
		rig.run()
		if left := rig.left(); left != 0 {
			t.Fatalf("rows left after Dispatch took the record and its commits = %d, want none", left)
		}
		routes, commits := client.written()
		if !slices.Equal(routes, []string{"step", "commits"}) {
			t.Fatalf("the routes written = %v, want the step route, then the commits route", routes)
		}
		puts := rig.client.received()
		if len(commits) != 1 || commits[0].Issue != stepIssue || commits[0].ID != rec.ID || !reflect.DeepEqual(commits[0].Write, puts[0].Write) {
			t.Fatalf("the commits writes = %+v, want one of record %s under %s carrying the record's write %+v", commits, rec.ID, stepIssue, puts[0].Write)
		}
		written, value := reflect.ValueOf(commits[0].Commits), reflect.ValueOf(rec)
		for i := range commitsType.NumField() {
			name := commitsType.Field(i).Name
			want := value.FieldByName(name)
			if want.IsNil() {
				t.Fatalf("the fixture names no %s, so a write that dropped it would pass", name)
			}
			if got := written.Field(i).String(); got != want.Elem().String() {
				t.Errorf("the commits write's %s = %q, want the record's %q", name, got, want.Elem().String())
			}
		}
	})

	t.Run("control: a record that names no commit is written on the step route alone", func(t *testing.T) {
		t.Parallel()
		rec := stepRecordOf()
		rec.StartCommit, rec.EndCommit, rec.FixCheck = nil, nil, nil
		rig := newStepRecordRigOf(t, rec, nil)
		client := &commitsDispatch{stepRecordDispatch: rig.client}
		rig.runner.dispatch = client
		rig.run()
		if left := rig.left(); left != 0 {
			t.Fatalf("rows left after Dispatch took the record = %d, want none", left)
		}
		if routes, _ := client.written(); !slices.Equal(routes, []string{"step"}) {
			t.Fatalf("the routes written = %v, want the step route alone", routes)
		}
	})
}

// A Dispatch older than the commits route takes the record's own write and answers the commits
// write with its router's 404 NOT_FOUND "no route for …". The row waits rather than fails or
// drops: each run sends the record again, which changes nothing Dispatch holds, and then the
// commits, and keeps the row, logged as a wait and never as an error or a drop. Once Dispatch
// serves the route, the next run writes the commits and ends the row. The stand-in answers as the
// router does, through the daemon's real HTTP client, so the client's reading of that body is what
// decides.
func TestAnOlderDispatchKeepsTheRowWaiting(t *testing.T) {
	t.Parallel()
	rig := newStepRecordRig(t, nil)
	rig.runner.log = slog.New(slog.NewJSONHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stepRoute := "/api/v1/issues/" + stepIssue + "/steps/" + stepRecordID
	commitsRoute := stepRoute + "/commits"

	var mu sync.Mutex
	routed, steps, asked := false, 0, 0
	var landed map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPut && req.URL.Path == stepRoute:
			steps++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": stepRecordID})
		case req.Method == http.MethodPut && req.URL.Path == commitsRoute && routed:
			asked++
			if err := json.NewDecoder(req.Body).Decode(&landed); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": stepRecordID})
		default:
			if req.URL.Path == commitsRoute {
				asked++
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "NOT_FOUND", "error": "no route for " + req.Method + " " + req.URL.Path, "hint": "GET /api/v1 lists every route",
			})
		}
	}))
	t.Cleanup(server.Close)
	rig.runner.dispatch = dispatch.New(server.URL, dispatch.BearerTransport("agent-token", nil))

	const tries = 3
	for try := 1; try <= tries; try++ {
		rig.run()
		var attempts int
		if err := rig.pool.QueryRow(context.Background(), `select attempts from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindStepRecord), stepIssue).Scan(&attempts); err != nil {
			t.Fatalf("after try %d, read the row: %v (the row is gone, though Dispatch has no commits route)", try, err)
		}
		if attempts != try {
			t.Fatalf("after try %d the row has %d attempts, want %d: the row kept for its next run", try, attempts, try)
		}
		mu.Lock()
		gotSteps, gotAsked := steps, asked
		mu.Unlock()
		if gotSteps != try || gotAsked != try {
			t.Fatalf("after try %d the stand-in took %d step writes and was asked for the commits %d times, want %d of each",
				try, gotSteps, gotAsked, try)
		}
	}

	mu.Lock()
	routed = true
	mu.Unlock()
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("after Dispatch served the commits route the issue has %d rows left, want none", left)
	}
	mu.Lock()
	body := landed
	mu.Unlock()
	for name, want := range map[string]string{
		"start_commit": `"1111111111111111111111111111111111111111"`, "end_commit": `"2222222222222222222222222222222222222222"`,
		"fix_check": `"k-ci"`, "task": `"default"`, "step": "4", "kind": `"step"`, "role": `"reviewer"`, "generation": "1",
	} {
		if got := string(body[name]); got != want {
			t.Errorf("the commits write names %s as %q, want %s", name, got, want)
		}
	}
	rig.waitedNeverFailed(stepRefusedLine, []string{"INFO", "DEBUG", "DEBUG"})
}

// The CI job a fix starts for, as the fix's job read (record.FixJobRead) asks Dispatch for it.
const (
	fixJobHead    = "c0de000000000000000000000000000000000001"
	fixJobAnother = "c0de000000000000000000000000000000000002"
	fixJobOldest  = "c0de000000000000000000000000000000000003"
	fixJobPage    = "https://github.com/o/r/actions/runs/7/job/9"
)

// fixJobReadOf is the read of fix 1 of task default at run 1, of check k-ci at fixJobHead.
func fixJobReadOf(check string) record.FixJobRead {
	return record.FixJobRead{Generation: 1, Task: "default", Fix: 1, Check: check, Head: fixJobHead}
}

// resultsAnswer is Dispatch's answer to GET /api/v1/issues/{key}/results for stepIssue: the check
// k-ci, which CI proves, with its two results of role ci, newest first, the newer at newer with the
// page of its run as its observed text's last https:// line, and the older at fixJobOldest with a
// page of its own; beside it the check k-unit, whose result at fixJobHead names another page.
func resultsAnswer(t *testing.T, newer string) string {
	t.Helper()
	result := func(id, commit, observed string, at time.Time) map[string]any {
		return map[string]any{"id": id, "role": "ci", "verdict": "fail", "commit_sha": commit, "observed": observed,
			"created_at": at, "actor": map[string]string{"kind": "github_app", "id": "legion-ci"}, "session": ""}
	}
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	answer := map[string]any{"issue": stepIssue, "verification_artifact_id": "a-verification", "checks": []any{
		map[string]any{"block_id": "k-unit", "ci_check": "unit", "ci": map[string]any{"run": "unit", "state": "fail", "case": "failure"},
			"results": []any{result("ci-run:3", fixJobHead, "failure\nhttps://github.com/o/r/actions/runs/7/job/3", at)}},
		map[string]any{"block_id": "k-ci", "ci_check": "daemon-go", "ci": map[string]any{"run": "daemon-go (race)", "state": "fail", "case": "failure"},
			"results": []any{
				result("ci-run:9", newer, "failure\nThe race step failed\nhttps://docs.example/flakes\n2 tests failed\n"+fixJobPage, at),
				result("ci-run:5", fixJobOldest, "failure\nhttps://github.com/o/r/actions/runs/4/job/5", at.Add(-time.Hour)),
			}},
	}}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// fixJobRig is one fix's job read of stepIssue (record.FixJobRead), drained by hand (RunOnce) on
// a clock the test holds, against a stand-in of Dispatch's results route reached through the
// daemon's real client, with each fact the row applies kept.
type fixJobRig struct {
	t      *testing.T
	pool   *store.Pool
	runner *outbox
	clock  *heldClock
	facts  *outboxFactHandler
	logs   *syncBuffer
	mu     sync.Mutex
	reads  int
}

// newFixJobRig queues read, first due now, against a Dispatch that answers its results read
// numbered n, from 0, with answer(n): the status and the body.
func newFixJobRig(t *testing.T, read record.FixJobRead, answer func(n int) (int, string)) *fixJobRig {
	t.Helper()
	rig := &fixJobRig{t: t, pool: isolatedOutboxPool(t), clock: microClock(), facts: &outboxFactHandler{}, logs: &syncBuffer{}}
	results := "/api/v1/issues/" + stepIssue + "/results"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != results {
			t.Errorf("Dispatch was asked %s %s, want GET %s alone", req.Method, req.URL.Path, results)
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		rig.mu.Lock()
		n := rig.reads
		rig.reads++
		rig.mu.Unlock()
		status, body := answer(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	rig.runner = &outbox{pool: rig.pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: dispatch.New(server.URL, "agent-token"),
		handlers: []intake.Handler{rig.facts}, log: slog.New(slog.NewJSONHandler(rig.logs, nil)), now: rig.clock.Now}
	row := mustOutboxRow(t, stepIssue, read, rig.clock.Now())
	row.CreatedAt = rig.clock.Now()
	enqueueOutbox(t, rig.pool, record.NewStore(), row)
	return rig
}

// drain runs every row due now.
func (r *fixJobRig) drain() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
}

func (r *fixJobRig) readsMade() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// applied is each fix job read the row applied.
func (r *fixJobRig) applied() []intake.FixJobRead {
	var reads []intake.FixJobRead
	for _, fact := range r.facts.facts() {
		if read, ok := fact.(intake.FixJobRead); ok {
			reads = append(reads, read)
		}
	}
	return reads
}

// One read of Dispatch's results gives the fix its job: the name of the check run the check's CI
// job makes, and the page of the newest result of role ci at the fix's head, the last line of its
// observed text that starts with https://. The read takes neither from another check, nor from a
// result at another commit. The control is a newest result at another commit, beside an older one
// at another commit still: the read answers the name and no page, where a read that took the
// newest result whatever its commit would answer a page. A check Dispatch does not list answers
// neither. Each starts the fix: the row applies the read once and ends.
func TestTheFixJobReadTakesTheNameAndTheAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		check string
		newer string
		want  intake.FixJobRead
	}{
		{name: "the newer result at the head", check: "k-ci", newer: fixJobHead,
			want: intake.FixJobRead{Key: stepIssue, Generation: 1, Task: "default", Fix: 1, Name: "daemon-go (race)", URL: fixJobPage}},
		{name: "control: the newest result at another commit", check: "k-ci", newer: fixJobAnother,
			want: intake.FixJobRead{Key: stepIssue, Generation: 1, Task: "default", Fix: 1, Name: "daemon-go (race)"}},
		{name: "a check Dispatch does not list", check: "k-gone", newer: fixJobHead,
			want: intake.FixJobRead{Key: stepIssue, Generation: 1, Task: "default", Fix: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := resultsAnswer(t, tc.newer)
			rig := newFixJobRig(t, fixJobReadOf(tc.check), func(int) (int, string) { return http.StatusOK, body })
			rig.drain()
			if reads := rig.readsMade(); reads != 1 {
				t.Fatalf("Dispatch's results were read %d times, want once", reads)
			}
			if got := rig.applied(); len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("the row applied %+v, want the one read %+v", got, tc.want)
			}
			if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
				t.Fatalf("rows left after the read applied = %d, want none", left)
			}
			if errored := loggedLines(t, rig.logs.String(), `"level":"ERROR"`); len(errored) != 0 {
				t.Fatalf("logged %d ERROR lines for a read Dispatch answered: %s", len(errored), rig.logs.String())
			}
		})
	}
}

// A read Dispatch does not answer is made again on the row's backoff, and applies nothing while
// the row is younger than fixJobReadBound: the fix waits for its job. The first read that fails at
// or after that bound from the row's first try starts the fix without its job: the row applies the
// read with the failure's text and no name and no page, and ends. The control is a Dispatch that
// answers after two failures, well inside the bound: the fix starts with its job, and the failures
// leave no trace on the fact.
func TestAFixJobReadThatNeverAnswersStartsTheFixAfterItsBound(t *testing.T) {
	t.Parallel()
	const unavailable = `{"code":"DATABASE_UNAVAILABLE","error":"database unavailable"}`

	t.Run("a Dispatch that never answers", func(t *testing.T) {
		t.Parallel()
		rig := newFixJobRig(t, fixJobReadOf("k-ci"), func(int) (int, string) { return http.StatusServiceUnavailable, unavailable })
		first := rig.clock.Now()
		for attempt := 0; ; attempt++ {
			waited := rig.clock.Now().Sub(first)
			rig.drain()
			if reads := rig.readsMade(); reads != attempt+1 {
				t.Fatalf("after attempt %d, %s after the first try, Dispatch was read %d times, want one read an attempt",
					attempt+1, waited, reads)
			}
			applied := rig.applied()
			if waited < fixJobReadBound {
				if len(applied) != 0 {
					t.Fatalf("attempt %d, %s after the first try and inside the bound of %s, applied %+v, want nothing: the fix waits for its job",
						attempt+1, waited, fixJobReadBound, applied)
				}
				if left := outboxRowsOf(t, rig.pool, stepIssue); left != 1 {
					t.Fatalf("rows left after attempt %d = %d, want the read kept for its retry", attempt+1, left)
				}
				rig.clock.advance(record.RetryBackoff(attempt))
				continue
			}
			if len(applied) != 1 {
				t.Fatalf("attempt %d, %s after the first try, past the bound of %s, applied %+v, want the one read that starts the fix",
					attempt+1, waited, fixJobReadBound, applied)
			}
			got := applied[0]
			if got.Key != stepIssue || got.Generation != 1 || got.Task != "default" || got.Fix != 1 || got.Name != "" || got.URL != "" {
				t.Fatalf("the read applied = %+v, want fix 1 of task default of %s at run 1 with no name and no page", got, stepIssue)
			}
			if !strings.Contains(got.ReadFailed, "DATABASE_UNAVAILABLE: database unavailable") {
				t.Fatalf("the read's failure = %q, want the text of Dispatch's last answer, DATABASE_UNAVAILABLE: database unavailable", got.ReadFailed)
			}
			if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
				t.Fatalf("rows left after the read applied = %d, want none", left)
			}
			return
		}
	})

	t.Run("control: a Dispatch that answers inside the bound", func(t *testing.T) {
		t.Parallel()
		body := resultsAnswer(t, fixJobHead)
		rig := newFixJobRig(t, fixJobReadOf("k-ci"), func(n int) (int, string) {
			if n < 2 {
				return http.StatusServiceUnavailable, unavailable
			}
			return http.StatusOK, body
		})
		for attempt := range 3 {
			rig.drain()
			rig.clock.advance(record.RetryBackoff(attempt))
		}
		want := intake.FixJobRead{Key: stepIssue, Generation: 1, Task: "default", Fix: 1, Name: "daemon-go (race)", URL: fixJobPage}
		if got := rig.applied(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("the row applied %+v after two failed reads and one answered, want the one read %+v", got, want)
		}
		if reads := rig.readsMade(); reads != 3 {
			t.Fatalf("Dispatch's results were read %d times, want three", reads)
		}
		if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
			t.Fatalf("rows left after the read applied = %d, want none", left)
		}
	})
}
