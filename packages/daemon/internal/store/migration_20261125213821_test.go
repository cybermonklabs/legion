package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// runRetrosVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const runRetrosVersion = 20261125213821

// runRetrosRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const runRetrosRelease = "the release that runs no retro after an issue's close"

// runRetroSeed is a database at the release before the run retros migration, on which a test
// seeds the issues, the step starts and the phases rows the migration fills run_retros from.
type runRetroSeed struct {
	t     *testing.T
	store *Store
}

func (s runRetroSeed) exec(statement string, args ...any) {
	s.t.Helper()
	if _, err := s.store.Pool().Exec(s.t.Context(), statement, args...); err != nil {
		s.t.Fatalf("%s: %v", statement, err)
	}
}

// issue is the record of key in tree, standing in phase (held from heldFrom for holdReason where
// phase is held) at generation, whose last move was at runRetroMoved.
func (s runRetroSeed) issue(key, tree, parent, phase, heldFrom, holdReason string, generation int, takenBack bool) {
	s.t.Helper()
	s.exec(`insert into issues (key, tree, project, title, parent, phase, held_from, hold_reason, generation, status, rank,
			taken_back, moved_at, last_dispatch_seq)
		values ($1, $2, 'LEGION', $1, nullif($3, ''), $4, nullif($5, ''), nullif($6, ''), $7, 'in_progress', 'A', $8, $9, 1)`,
		key, tree, parent, phase, heldFrom, holdReason, generation, takenBack, runRetroMoved)
}

// stepStart is key's step_starts row of kind at place, whose latest start was of run generation at
// at, ended: what record.RaiseStepStarts writes, whose record names the generation of the start.
func (s runRetroSeed) stepStart(key, kind string, place, generation int, at time.Time) {
	s.t.Helper()
	s.exec(`insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
		values ($1, 'default', $2, $3, 1, 'agent of ' || $1, $4::timestamptz, 'ended',
		jsonb_build_object('task', 'default', 'kind', $2::text, 'step', $3::int, 'generation', $5::int, 'agent', 'agent of ' || $1))`,
		key, kind, place, at, generation)
}

// merger is key's phases row of the merger, with the task its step carried and the entry raised on
// it: the row the merger's start in a run leaves, which a re-admission clears the task of
// (record.Store.ClearGeneration) and keeps the entry of.
func (s runRetroSeed) merger(key, stepTask string, entry int, raisedAt *time.Time) {
	s.t.Helper()
	s.exec(`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, step_task, entry_raised_at)
		values ($1, 'merger', '', '', 0, '', $2, $3, $4)`, key, entry, stepTask, raisedAt)
}

// runRetroMoved is when every seeded issue last moved.
var runRetroMoved = time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

// seedRunRetroIssues is the database of a release with no run retros: one issue for each way the
// migration decides that a run started and that it had its retro. The keys are LEGION-50nn, a
// group of tens for each rule.
func seedRunRetroIssues(t *testing.T, store *Store) {
	t.Helper()
	seed := runRetroSeed{t: t, store: store}
	first := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	retro := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	// A root a person put back to todo after its tree finished (readmit): it is at admitted again
	// in generation 2 with TakenBack still set, and its only step start is of generation 1. A root
	// whose TakenBack is set and that never had a step start is the same.
	seed.issue("LEGION-5001", "LEGION-5001", "", "admitted", "", "", 2, true)
	seed.stepStart("LEGION-5001", "step", 1, 1, first)
	seed.issue("LEGION-5002", "LEGION-5002", "", "admitted", "", "", 1, true)

	// A parent held at admitted for its children (admit's waitsForChildren) above a child that
	// started, and a grandchild that started beneath that child: the run started in each issue
	// above one that started. The child's sibling never started, and a start does not go down.
	seed.issue("LEGION-5010", "LEGION-5010", "", "admitted", "", "", 1, false)
	seed.issue("LEGION-5011", "LEGION-5010", "LEGION-5010", "implementing", "", "", 1, false)
	seed.stepStart("LEGION-5011", "step", 2, 1, first)
	seed.issue("LEGION-5012", "LEGION-5010", "LEGION-5011", "planning", "", "", 1, false)
	seed.issue("LEGION-5013", "LEGION-5010", "LEGION-5010", "admitted", "", "", 1, false)

	// A closed child that started, beneath a parent that has not yet: a closed run is a run that
	// started, so the parent above it is started too.
	seed.issue("LEGION-5040", "LEGION-5040", "", "admitted", "", "", 1, false)
	seed.issue("LEGION-5041", "LEGION-5040", "LEGION-5040", "done", "", "", 1, false)
	seed.stepStart("LEGION-5041", "step", 1, 1, first)

	// A retro step start in the run is a retro the run had, whatever its result (the row keeps
	// none): this child is done in generation 3, and its retro started in generation 3. Another
	// child is in generation 2 and its only retro start is of generation 1, which is another run's.
	seed.issue("LEGION-5020", "LEGION-5020", "", "implementing", "", "", 1, false)
	seed.issue("LEGION-5021", "LEGION-5020", "LEGION-5020", "done", "", "", 3, false)
	seed.stepStart("LEGION-5021", "step", 1, 3, first)
	seed.stepStart("LEGION-5021", "retro", 0, 3, retro)
	seed.issue("LEGION-5022", "LEGION-5020", "LEGION-5020", "implementing", "", "", 2, false)
	seed.stepStart("LEGION-5022", "retro", 0, 1, first)
	seed.stepStart("LEGION-5022", "step", 1, 2, retro)

	// The phases that say the retro ran or passed with no step start of it: retro, merging, a hold
	// from either, but a hold from retro for its children stands in place of the retro.
	seed.issue("LEGION-5030", "LEGION-5030", "", "retro", "", "", 1, false)
	seed.issue("LEGION-5031", "LEGION-5031", "", "merging", "", "", 1, false)
	seed.issue("LEGION-5032", "LEGION-5032", "", "held", "retro", "children", 1, false)
	seed.issue("LEGION-5033", "LEGION-5033", "", "held", "retro", "escalated", 1, false)
	seed.issue("LEGION-5034", "LEGION-5034", "", "held", "merging", "", 1, false)
	seed.issue("LEGION-5037", "LEGION-5037", "", "held", "implementing", "escalated", 1, false)

	// The merger's phases row: one whose step carries a task was entered in this run, so the merger
	// started and the retro passed before it and the work came back; one whose task a re-admission
	// cleared keeps its entry of an earlier run, which says nothing of this one.
	seed.issue("LEGION-5035", "LEGION-5035", "", "implementing", "", "", 2, false)
	seed.merger("LEGION-5035", "default", 1, &retro)
	seed.issue("LEGION-5036", "LEGION-5036", "", "implementing", "", "", 2, false)
	seed.merger("LEGION-5036", "", 3, &first)

	// A child set back to todo (its second run, at admitted) whose second run has not started: its
	// newest run that started is the first, which a close of it owes the retro of unless that run
	// had one (the workflow's retroAtClose reads the newest run of any generation). One child's
	// first run had a retro step start; the other's had none.
	seed.issue("LEGION-5050", "LEGION-5050", "", "implementing", "", "", 1, false)
	seed.issue("LEGION-5051", "LEGION-5050", "LEGION-5050", "admitted", "", "", 2, false)
	seed.stepStart("LEGION-5051", "step", 1, 1, first)
	seed.stepStart("LEGION-5051", "retro", 0, 1, retro)
	seed.issue("LEGION-5052", "LEGION-5050", "LEGION-5050", "admitted", "", "", 2, false)
	seed.stepStart("LEGION-5052", "step", 1, 1, first)
}

// 20261125213821 adds run_retros, one row for each issue and run, which is the one source of
// whether a run started and whether it had its retro (INFRA-5602's P1 and P2), and fills it from
// what the database holds for each recorded issue's current run. The rows are the rules of that
// fill: a run started where an issue stands in a phase that carries a run or has a step start of
// its generation, and each issue above such an issue in its tree started the run too, so a parent
// held at admitted above a child that started is started; TakenBack alone does not start a run,
// since it survives a root's re-admission; a run had its retro where a retro step start of its
// generation, of any result, stands, or its issue stands in retro (not held for its children) or
// merging, or held from either, or its merger's phases row was entered in the run. An issue whose
// current run has not started gets a row for its newest run that did, from its step starts, since
// a close reads the newest run of any generation; every other issue gets one for its current run
// alone.
func TestTheRunRetroMigrationFillsTheCurrentRuns(t *testing.T) {
	store := emptyStore(t)
	migrateThrough(t, store, runRetrosVersion-1)
	seedRunRetroIssues(t, store)

	migrateThrough(t, store, runRetrosVersion)

	rowOf := func(t *testing.T, issue string) string {
		t.Helper()
		var row string
		if err := store.Pool().QueryRow(t.Context(), `select coalesce((select row(generation, tree, retro_started_at is not null)::text
			from run_retros where issue = $1), 'none')`, issue).Scan(&row); err != nil {
			t.Fatalf("read the run retro of %s: %v", issue, err)
		}
		return row
	}
	for _, tc := range []struct{ name, issue, want string }{
		{"a root re-admitted with TakenBack set and no step start in this run has its newest started run, its first", "LEGION-5001", "(1,LEGION-5001,f)"},
		{"a root with TakenBack set and no step start at all did not start", "LEGION-5002", "none"},
		{"a parent held at admitted above a child that started is started", "LEGION-5010", "(1,LEGION-5010,f)"},
		{"the child that started is started", "LEGION-5011", "(1,LEGION-5010,f)"},
		{"a grandchild in a phase that carries a run is started", "LEGION-5012", "(1,LEGION-5010,f)"},
		{"a sibling that never started did not start with its parent", "LEGION-5013", "none"},
		{"a parent above a closed child that started is started", "LEGION-5040", "(1,LEGION-5040,f)"},
		{"a closed child with a step start in this run is started", "LEGION-5041", "(1,LEGION-5040,f)"},
		{"a root in a phase of its own is started with no retro", "LEGION-5020", "(1,LEGION-5020,f)"},
		{"a run with a retro step start, of any result, has its retro", "LEGION-5021", "(3,LEGION-5020,t)"},
		{"a retro step start of an earlier run is no retro of this one", "LEGION-5022", "(2,LEGION-5020,f)"},
		{"an issue in retro had its retro", "LEGION-5030", "(1,LEGION-5030,t)"},
		{"an issue in merging had its retro", "LEGION-5031", "(1,LEGION-5031,t)"},
		{"an issue held in place of its retro for its children had none", "LEGION-5032", "(1,LEGION-5032,f)"},
		{"an issue held from retro for another reason had its retro", "LEGION-5033", "(1,LEGION-5033,t)"},
		{"an issue held from merging had its retro", "LEGION-5034", "(1,LEGION-5034,t)"},
		{"an issue held from implementing had none", "LEGION-5037", "(1,LEGION-5037,f)"},
		{"a merger entered in this run says the retro passed", "LEGION-5035", "(2,LEGION-5035,t)"},
		{"a merger row of an earlier run, its task cleared, says nothing of this one", "LEGION-5036", "(2,LEGION-5036,f)"},
		{"a child set back to todo, not started again, has its first run, which had its retro", "LEGION-5051", "(1,LEGION-5050,t)"},
		{"a child set back to todo, not started again, has its first run, which had no retro", "LEGION-5052", "(1,LEGION-5050,f)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowOf(t, tc.issue); got != tc.want {
				t.Fatalf("run_retros of %s (generation, tree, retro started) = %s, want %s", tc.issue, got, tc.want)
			}
		})
	}

	t.Run("the retro's start is the retro step's own moment", func(t *testing.T) {
		const query = `select to_char(retro_started_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') from run_retros where issue = 'LEGION-5021'`
		if got, want := rowsAsText(t, store, query), []string{"2026-11-24T12:00:00Z"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the retro of LEGION-5021 started at %v, want %v", got, want)
		}
	})
	t.Run("each issue has one row, its current run's or its newest started run's, and no retro after a close is open", func(t *testing.T) {
		const opened = `select count(*)::text from run_retros r
			where r.opened_at is not null or r.bound_at is not null or r.agent <> '' or r.entry <> 0
			or r.ended_at is not null or r.outcome <> '' or r.ended_by <> ''`
		if got, want := rowsAsText(t, store, opened), []string{"0"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows with a retro after a close = %v, want none", got)
		}
		const older = `select coalesce(string_agg(r.issue || ':' || r.generation, ',' order by r.issue), '') from run_retros r
			join issues i on i.key = r.issue where r.generation <> i.generation`
		if got, want := rowsAsText(t, store, older), []string{"LEGION-5001:1,LEGION-5051:1,LEGION-5052:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows of a run other than the current one = %v, want %v: the issues whose current run has not started", got, want)
		}
		if got, want := rowsAsText(t, store, `select count(*)::text from run_retros`), []string{"20"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("run_retros holds %v rows, want %v: one for each of the 20 issues a run of which started", got, want)
		}
		if got, want := rowsAsText(t, store, `select count(*)::text from (select issue from run_retros group by issue having count(*) > 1) twice`), []string{"0"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("issues with more than one run_retros row = %v, want none", got)
		}
	})
}

// The way back of 20261125213821, the last paragraph of its header (migrations.WayBack), is what
// an operator runs with every daemon stopped before the release before starts. That release
// decodes an outbox payload strictly (record.DecodeOutboxPayload, DisallowUnknownFields), so each
// queued row this release writes and it does not know is deleted: a retro_bound row, and a
// supervise start that carries retroRun or retroWake. Every other row stands, a start that
// carries a look among them. The way back drops the table and the two columns of the serving
// marker and the migration's version row, so the roll-forward's Migrate applies the file again,
// which fills run_retros from the issues as they stand then. The claim and the delivery the
// columns were on stay, as the release before wrote them.
func TestTheRunRetroMigrationsWayBackRemovesWhatTheReleaseBeforeCannotRead(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, runRetrosVersion-1)
	seed := runRetroSeed{t: t, store: store}
	seed.issue("LEGION-5101", "LEGION-5101", "", "implementing", "", "", 1, false)
	seed.stepStart("LEGION-5101", "step", 2, 1, time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	migrateThrough(t, store, runRetrosVersion)

	seed.exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_retro_run)
		values ('legion-LEGION-5101-implementer', 'LEGION', 'LEGION-5101', 'LEGION-5101', 'implementer', 1, '', '', 'idle', 0, 0, 0, 0, 3)`)
	seed.exec(`insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, confirmed_at, retro_run)
		values ('legion-LEGION-5101-implementer', 'delivery-1', 'the retro', now(), now(), 3)`)
	queue := func(kind, payload string) {
		t.Helper()
		seed.exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-5101', $2::jsonb, 0, now(), '')`, kind, payload)
	}
	queue("retro_bound", `{"generation": 1}`)
	queue("supervise", `{"op": "start", "tree": "LEGION-5101", "role": "implementer", "generation": 1, "retroRun": 1, "task": "the retro"}`)
	queue("supervise", `{"op": "start", "tree": "LEGION-5101", "role": "architect", "generation": 1, "retroWake": true, "wakeKind": "role_message"}`)
	queue("supervise", `{"op": "start", "tree": "LEGION-5101", "role": "implementer", "generation": 1, "checkLook": 1}`)
	queue("supervise", `{"op": "start", "tree": "LEGION-5101", "role": "implementer", "generation": 1, "task": "a phase"}`)
	queue("supervise", `{"op": "suspend", "tree": "LEGION-5101", "role": "implementer", "generation": 1}`)
	queue("retro_bound", `{"generation": 2}`)

	seed.exec(wayBackOf(t, runRetrosVersion, runRetrosRelease))

	const gone = `select (select count(*) from information_schema.tables where table_name = 'run_retros')::text
		|| ' ' || (select count(*) from information_schema.columns where table_name = 'claims' and column_name = 'serving_retro_run')::text
		|| ' ' || (select count(*) from information_schema.columns where table_name = 'pending_task_deliveries' and column_name = 'retro_run')::text
		|| ' ' || (select count(*) from schema_version where version = 20261125213821)::text`
	if got, want := rowsAsText(t, store, gone), []string{"0 0 0 0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back the table, the claim's column, the delivery's column and the version row (counts) = %v, want none of them", got)
	}
	const queued = `select kind || ' ' || payload::text from outbox order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`supervise {"op": "start", "role": "implementer", "tree": "LEGION-5101", "checkLook": 1, "generation": 1}`,
		`supervise {"op": "start", "role": "implementer", "task": "a phase", "tree": "LEGION-5101", "generation": 1}`,
		`supervise {"op": "suspend", "role": "implementer", "tree": "LEGION-5101", "generation": 1}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the queued rows after the way back = %q, want %q: each retro_bound row and each start that carries retroRun or retroWake deleted, every other row kept", got, want)
	}
	if got, want := rowsAsText(t, store, `select count(*)::text from claims where token = 'legion-LEGION-5101-implementer'`), []string{"1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claims of the retro's agent after the way back = %v, want the claim kept: the columns went, the row did not", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got, want := rowsAsText(t, store, `select row(issue, generation, retro_started_at is not null)::text from run_retros order by issue`), []string{"(LEGION-5101,1,f)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run_retros after the roll-forward = %v, want %v: the file applied again, filled from the issues as they stand", got, want)
	}
}
