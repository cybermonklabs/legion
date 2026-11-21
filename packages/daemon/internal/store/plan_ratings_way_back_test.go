package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// planRatingsRelease names the release whose way back the header of planRecordKindVersion
// (20261117023932_outbox_kinds_with_the_plan_record) carries, as a failure to read that block
// names it. docs/kubernetes.md carries the same block under "Rolling back the release that rates
// each plan", and internal/textcheck's TestThePlanRatingsWayBackIsOneTextInTheHeaderAndTheGuide
// holds the two to one text. This file reads the header alone, so the guide stays out of this
// package's CI filter: a test that read the guide would start the whole daemon Go lane on an edit
// of the guide alone.
const planRatingsRelease = "the release that rates each plan"

// rollingBackProject is what the way back's two deletes name the project that rolls back by, and
// what an operator replaces with that project's key: legion.yaml's project, the part of each of
// its issue keys before the hyphen. projectScope is the predicate each delete carries with it.
const (
	rollingBackProject = "<PROJECT>"
	projectScope       = " and split_part(issue, '-', 1) = '" + rollingBackProject + "'"
)

// planRatingsWayBackFor is the block the header of planRecordKindVersion carries, as an operator
// who rolls project back runs it: with project's key in place of rollingBackProject.
func planRatingsWayBackFor(t *testing.T, project string) string {
	t.Helper()
	return strings.ReplaceAll(wayBackOf(t, planRecordKindVersion, planRatingsRelease), rollingBackProject, project)
}

// stepRecordBeforeTheRating is record.StepRecord as the release that records each step declares
// it (commit aa36bcbe4e), the release this rollback goes back to: every field of this release's
// shape but PlanRating. It is a copy because that release's type is not in the tree.
// That release decodes a step_record row strictly (record.DecodeOutboxPayload,
// DisallowUnknownFields), so a row this shape refuses is a row it fails at every attempt.
type stepRecordBeforeTheRating struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Generation uint64     `json:"generation"`
	Session    *string    `json:"session,omitempty"`
	Model      *string    `json:"model,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Result     *string    `json:"result,omitempty"`
	ReturnedTo *int       `json:"returned_to,omitempty"`
	Report     *string    `json:"report,omitempty"`
}

// decodedBeforeTheRating is the error the release before's strict decode of a step_record payload
// gives, nil where it takes the payload.
func decodedBeforeTheRating(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value stepRecordBeforeTheRating
	return decoder.Decode(&value)
}

// writeOf is how the daemon names a write of payload in the error of a run that failed (its
// executors: "write the plan record %s of %s", "write the step record %s of %s" and "write the
// rating of the step record %s of %s"), and the Dispatch route the write goes to, under
// /api/v1/issues/{key}/, as the daemon's client sends it (dispatch's PutPlan, PutStep and
// PutStepRating): a plan on its own id, a step record on its own, and a step record carrying a
// rating on that record's rating route.
func writeOf(payload record.OutboxPayload) (what, route string) {
	switch write := payload.(type) {
	case record.PlanRecord:
		return "plan record " + write.ID, "plans/" + write.ID
	case record.StepRecord:
		if write.PlanRating != nil {
			return "rating of the step record " + write.ID, "steps/" + write.ID + "/rating"
		}
		return "step record " + write.ID, "steps/" + write.ID
	default:
		return "", ""
	}
}

// The way back of the release that rates each plan deletes the two kinds of row an earlier image
// cannot take, and nothing else: the plan records, and the step records that carry a retro's
// rating. The rows here are two issues' pending writes as this release queues them
// (record.NewOutboxRow), each at the moment its write is made, under a Dispatch older than the
// release that records each step, which serves no step's, plan's or rating's route (the daemon
// and Dispatch deploy in either order). Of the first issue: an earlier run's plan, the start and
// the end of one of its steps, and its retro's rating; this run's plan and the start and the end
// of one of its steps; and, at the moment the daemon stops for the rollback, this run's retro's
// end and its rating, whose record id is the end's. Of a second issue, whose run starts at that
// moment: its plan and a status write. Each row queued before the stop waits to retry, as that
// Dispatch's NOT_FOUND leaves a row (the daemon's RetryOutbox): attempts counted, the last error
// kept and the retry time its backoff (record.RetryBackoff) set, at most 64 seconds after the
// stop. The rows of the stop's own moment have not run. The block runs as the migration's header
// carries it, as one exec (the simple protocol a psql run uses). Then the plans and the ratings
// are gone, waiting or not, and every other row is still there with its payload and its retry
// state: none of them is a write the earlier image is owed less than this one.
//
// The control comes first, on the same rows: the strict decode of the release before refuses the
// rating rows and takes the ordinary step records. After the block every step record left takes
// that decode, and every row left takes this release's decoder too, which is what the roll forward
// reads. The other kinds left carry payloads this release did not change, so the earlier image
// decodes them with the types it shares with this one.
//
// What this cannot show is the earlier daemon running: its binary is not here, so its decode is
// the copy above (stepRecordBeforeTheRating).
func TestThePlanRatingsWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// issue is the issue whose retro ends as the daemon stops, and next the issue whose run starts
	// then.
	const issue, next = "LEGION-208", "LEGION-210"
	// The moments of issue's two runs: the earlier run's start, the start and the end of its
	// implementer step, and its retro's end; this run's start, the start and the end of its
	// implementer step, and its retro's start; and stopped, when this run's retro ends and the
	// daemon stops for the rollback.
	earlierRun := time.Date(2026, 11, 17, 0, 10, 0, 0, time.UTC)
	earlierStepStarted := earlierRun.Add(6 * time.Minute)
	earlierStepEnded := earlierStepStarted.Add(41*time.Minute + 8*time.Second)
	earlierRetroEnded := earlierStepEnded.Add(12 * time.Minute)
	run := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	stepStarted := run.Add(8 * time.Minute)
	stepEnded := stepStarted.Add(97*time.Minute + 13*time.Second)
	retroStarted := stepEnded.Add(4 * time.Minute)
	stopped := retroStarted.Add(9*time.Minute + 31*time.Second)
	agent := "legion-legion-legion-208-implementer"
	retro := "7a1b2c3d-4e5f-5a6b-8c7d-9e0f1a2b3c4d"
	step := "1f2e3d4c-5b6a-5978-8a9b-0c1d2e3f4a5b"
	earlierRetro := "2b3c4d5e-6f70-5a8b-9c0d-1e2f3a4b5c6d"
	earlierStep := "3c4d5e6f-7081-5b9c-8d0e-2f3a4b5c6d7e"
	defaultSteps := []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}
	// retried is the attempts and the next due time the daemon's outbox leaves a row queued at
	// queued with, once the daemon stops at stopped: the row ran each time it came due before then,
	// each run met the wait and added its backoff (record.RetryBackoff, which the daemon's outbox
	// reads), and a row queued as the daemon stops has not run.
	retried := func(queued time.Time) (attempts int, nextAt time.Time) {
		for nextAt = queued; nextAt.Before(stopped); attempts++ {
			nextAt = nextAt.Add(record.RetryBackoff(attempts))
		}
		return attempts, nextAt
	}
	// waitingError is the error each of those runs stores, as the daemon writes it of the row that
	// met it: the wait its outbox raises (its execute, "outbox row %d (%s) of %s waits for a
	// Dispatch that serves the route: %v") over its executor's error ("write the plan record %s of
	// %s: %w" or "write the step record %s of %s: %w", as writeOf names the write), which wraps
	// Dispatch's own refusal of a route it does not serve.
	waitingError := func(id int64, kind record.OutboxKind, key string, payload record.OutboxPayload) string {
		what, route := writeOf(payload)
		return fmt.Sprintf("outbox row %d (%s) of %s waits for a Dispatch that serves the route: write the %s of %s: NOT_FOUND: no route for PUT /api/v1/issues/%s/%s",
			id, kind, key, what, key, key, route)
	}
	// queued is the rows in the order the daemon queues them, each with its issue and the moment
	// its write is made.
	queued := []struct {
		name    string
		issue   string
		at      time.Time
		payload record.OutboxPayload
		kept    bool
	}{
		{name: "an earlier run's plan", issue: issue, at: earlierRun, payload: record.PlanRecord{ID: "4d5e6f70-8192-5cad-9e0f-3a4b5c6d7e8f",
			Generation: 1, Source: "default", Tasks: []record.PlanRecordTask{{ID: "default", Steps: defaultSteps}}}},
		{name: "an earlier run's start of a step", kept: true, issue: issue, at: earlierStepStarted, payload: record.StepRecord{ID: earlierStep,
			Task: "default", Step: 2, Kind: "step", Role: "implementer", Type: "implementation", Agent: agent, Generation: 1, StartedAt: &earlierStepStarted}},
		{name: "an earlier run's end of a step", kept: true, issue: issue, at: earlierStepEnded, payload: record.StepRecord{ID: earlierStep,
			Task: "default", Step: 2, Kind: "step", Role: "implementer", Type: "implementation", Agent: agent, Generation: 1,
			StartedAt: &earlierStepStarted, EndedAt: &earlierStepEnded, Result: new("failed"), ReturnedTo: new(1)}},
		{name: "an earlier run's rating", issue: issue, at: earlierRetroEnded, payload: record.StepRecord{ID: earlierRetro, Task: "default", Step: 0,
			Kind: "retro", Role: "implementer", Agent: agent, Generation: 1, PlanRating: &record.PlanRating{Rating: 2, Reason: "The plan was late."}}},
		{name: "the plan", issue: issue, at: run, payload: record.PlanRecord{ID: "5d6e7f80-91a2-5b3c-8d4e-5f6a7b8c9d0e", Generation: 2, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: defaultSteps}}}},
		{name: "the start of a step", kept: true, issue: issue, at: stepStarted, payload: record.StepRecord{ID: step, Task: "default", Step: 2, Kind: "step",
			Role: "implementer", Type: "implementation", Agent: agent, Generation: 2, StartedAt: &stepStarted}},
		{name: "the end of a step", kept: true, issue: issue, at: stepEnded, payload: record.StepRecord{ID: step, Task: "default", Step: 2, Kind: "step",
			Role: "implementer", Type: "implementation", Agent: agent, Generation: 2, Session: new("s1"), Model: new("first/model"),
			StartedAt: &stepStarted, EndedAt: &stepEnded, Result: new("passed")}},
		{name: "the retro's end", kept: true, issue: issue, at: stopped, payload: record.StepRecord{ID: retro, Task: "default", Step: 0, Kind: "retro",
			Role: "implementer", Agent: agent, Generation: 2, StartedAt: &retroStarted, EndedAt: &stopped, Result: new("passed")}},
		{name: "the retro's rating", issue: issue, at: stopped, payload: record.StepRecord{ID: retro, Task: "default", Step: 0, Kind: "retro", Role: "implementer",
			Agent: agent, Generation: 2, PlanRating: &record.PlanRating{Rating: 4, Reason: "The plan put the tests after the review."}}},
		{name: "the next issue's plan", issue: next, at: stopped, payload: record.PlanRecord{ID: "6e7f8091-a2b3-5c4d-9e5f-6a7b8c9d0e1f", Generation: 1,
			Source: "default", Tasks: []record.PlanRecordTask{{ID: "default", Steps: defaultSteps}}}},
		{name: "a status write of the next issue", kept: true, issue: next, at: stopped, payload: record.OrdinaryStatusWrite("in_progress", "todo", "")},
	}
	ids := make([]int64, len(queued))
	// retryState is a row's attempts, the time it is next due and its last error.
	type retryState struct {
		attempts  int
		nextAt    time.Time
		lastError string
	}
	// inserted is the retry state each row of queued is inserted with, at its place in queued.
	inserted := make([]retryState, len(queued))
	for i, q := range queued {
		row, err := record.NewOutboxRow(q.issue, record.Fenced(q.payload, record.FirstFence), q.at)
		if err != nil {
			t.Fatalf("queue %s: %v", q.name, err)
		}
		var state retryState
		state.attempts, state.nextAt = retried(row.NextAt)
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3, $4, $5, '') returning id`, string(row.Kind), row.Issue, row.Payload, state.attempts, state.nextAt).Scan(&ids[i]); err != nil {
			t.Fatalf("insert %s: %v", q.name, err)
		}
		if state.attempts > 0 {
			state.lastError = waitingError(ids[i], row.Kind, row.Issue, q.payload)
			if _, err := store.Pool().Exec(ctx, `update outbox set last_error = $2 where id = $1`, ids[i], state.lastError); err != nil {
				t.Fatalf("stamp the error of %s: %v", q.name, err)
			}
		}
		inserted[i] = state
		if row.Kind != record.OutboxKindStepRecord {
			continue
		}
		write, _ := q.payload.(record.StepRecord)
		if err := decodedBeforeTheRating(row.Payload); (write.PlanRating != nil) != (err != nil) {
			t.Fatalf("the release before's decode of %s = %v; the control wants it to refuse the rating rows alone, or this test cannot see the rows the way back exists for",
				q.name, err)
		}
	}

	if _, err := store.Pool().Exec(ctx, planRatingsWayBackFor(t, "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back the header of migration %d carries: %v", planRecordKindVersion, err)
	}

	rows, err := store.Pool().Query(ctx, `select id, kind, issue, payload, attempts, next_at, last_error from outbox order by id`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	// stood is a row left after the way back, with its retry state.
	type stood struct {
		row record.OutboxRow
		retryState
	}
	left := map[int64]stood{}
	for rows.Next() {
		var s stood
		var kind string
		if err := rows.Scan(&s.row.ID, &kind, &s.row.Issue, &s.row.Payload, &s.attempts, &s.nextAt, &s.lastError); err != nil {
			t.Fatalf("read an outbox row: %v", err)
		}
		s.row.Kind = record.OutboxKind(kind)
		left[s.row.ID] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	var wantLeft []int64
	for i, q := range queued {
		if q.kept {
			wantLeft = append(wantLeft, ids[i])
		}
		s, stands := left[ids[i]]
		row := s.row
		switch {
		case q.kept && !stands:
			t.Errorf("the way back deleted %s (row %d), which the release before reads and is owed", q.name, ids[i])
			continue
		case !q.kept && stands:
			t.Errorf("the way back left %s (row %d): %s", q.name, ids[i], row.Payload)
		case !stands:
			continue
		}
		decoded, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Errorf("this release's decode of %s after the way back: %v", q.name, err)
		} else if !reflect.DeepEqual(decoded, q.payload) {
			t.Errorf("%s reads %#v after the way back, want %#v as it was queued", q.name, decoded, q.payload)
		}
		if want := inserted[i]; s.attempts != want.attempts || !s.nextAt.Equal(want.nextAt) || s.lastError != want.lastError {
			t.Errorf("%s stands after the way back with %d attempts, due at %s, after %q; want its %d, %s and %q as they were",
				q.name, s.attempts, s.nextAt.UTC(), s.lastError, want.attempts, want.nextAt, want.lastError)
		}
		if row.Kind != record.OutboxKindStepRecord {
			continue
		}
		if err := decodedBeforeTheRating(row.Payload); err != nil {
			t.Errorf("the release before refuses %s after the way back, and retries it at every attempt: %v", q.name, err)
		}
	}
	if len(left) != len(wantLeft) {
		t.Errorf("the outbox holds %d rows after the way back, want the %d rows %v", len(left), len(wantLeft), wantLeft)
	}
	// The way back restores no kind check: the rollback keeps this release's, which must take every
	// kind the release before writes, its list (20261117000528's) whole, or that release's first
	// write of a kind left out fails its transaction.
	for _, kind := range stepRecordReleaseKinds {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-209', '{}', 0, now(), '')`, kind); err != nil {
			t.Errorf("the release before queues a %s row after the way back: %v", kind, err)
		}
	}
}

// The way back deletes the plans and the ratings of the project that rolls back, and no other
// project's. Several projects' daemons can share one database, and the outbox is one table for all
// of them: each daemon runs only its own project's rows (DueRows' split_part scope), so one project
// can roll back while another stays on this release, whose daemon still owes its own rows to
// Dispatch. Each project here has a pending plan and a pending rating, as this release queues them
// (record.NewOutboxRow). The block runs for LEGION as an operator runs it: LEGION's two rows go,
// and OTHER's two stand.
//
// The control runs the same block with the project's predicate cut out of both deletes, over the
// same rows queued again: it leaves none of OTHER's rows, so the predicate is what keeps them.
func TestThePlanRatingsWayBackDeletesOnlyTheRowsOfTheProjectThatRollsBack(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	steps := []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}
	queued := []struct {
		issue   string
		payload record.OutboxPayload
	}{
		{issue: "LEGION-1", payload: record.PlanRecord{ID: "4d5e6f70-8192-5cad-9e0f-3a4b5c6d7e8f", Generation: 1, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: steps}}}},
		{issue: "LEGION-1", payload: record.StepRecord{ID: "7a1b2c3d-4e5f-5a6b-8c7d-9e0f1a2b3c4d", Task: "default", Kind: "retro",
			Role: "implementer", Agent: "legion-legion-legion-1-implementer", Generation: 1,
			PlanRating: &record.PlanRating{Rating: 4, Reason: "The plan put the tests after the review."}}},
		{issue: "OTHER-7", payload: record.PlanRecord{ID: "6e7f8091-a2b3-5c4d-9e5f-6a7b8c9d0e1f", Generation: 1, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: steps}}}},
		{issue: "OTHER-7", payload: record.StepRecord{ID: "2b3c4d5e-6f70-5a8b-9c0d-1e2f3a4b5c6d", Task: "default", Kind: "retro",
			Role: "implementer", Agent: "legion-other-other-7-implementer", Generation: 1,
			PlanRating: &record.PlanRating{Rating: 2, Reason: "The plan was late."}}},
	}
	// leftBy is the issue of each row a fresh store's outbox holds after block ran over queued, in
	// the order the rows were queued.
	leftBy := func(block string) []string {
		store := emptyStore(t)
		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		for _, q := range queued {
			row, err := record.NewOutboxRow(q.issue, record.Fenced(q.payload, record.FirstFence), at)
			if err != nil {
				t.Fatalf("queue a %T of %s: %v", q.payload, q.issue, err)
			}
			if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ($1, $2, $3, 0, $4, '')`, string(row.Kind), row.Issue, row.Payload, row.NextAt); err != nil {
				t.Fatalf("insert a %T of %s: %v", q.payload, q.issue, err)
			}
		}
		if _, err := store.Pool().Exec(ctx, block, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run the way back:\n%s\n%v", block, err)
		}
		return rowsAsText(t, store, `select issue from outbox order by id`)
	}

	if left, want := leftBy(planRatingsWayBackFor(t, "LEGION")), []string{"OTHER-7", "OTHER-7"}; !reflect.DeepEqual(left, want) {
		t.Errorf("the way back for LEGION leaves the rows of %v, want OTHER's plan and rating %v: another project's daemon, still on this release, never sends a row the block deleted",
			left, want)
	}

	block := wayBackOf(t, planRecordKindVersion, planRatingsRelease)
	if scoped := strings.Count(block, projectScope); scoped != 2 {
		t.Errorf("the way back scopes %d of its two deletes to the project that rolls back (%q), want both:\n%s", scoped, projectScope, block)
	}
	if left := leftBy(strings.ReplaceAll(block, projectScope, "")); len(left) != 0 {
		t.Errorf("the control, the way back with no project predicate, leaves the rows of %v; want none, or this test cannot see a block that deletes another project's rows", left)
	}
}
