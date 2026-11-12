package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// upgradeRecoveryReadsAnswer is the way back the migration's header names, as far as a test
// without the old binary can take it: the eleven recovery statements run with the daemon stopped,
// and then the record is read as the release before this one reads it, and as the roll-forward
// after that rollback reads it.
//
// Each statement is the header's own, run here by itself: an operator runs the eleven as one
// transaction under psql's ON_ERROR_STOP, as the header and docs/kubernetes.md give them, and a
// test holding a pool runs them in that order.
//
// The three reads are that release's own, as literal SQL at commit e90c078559: its claim read
// (internal/store/claims.go, claimSelect), which joins the pending delivery and is the one read
// that names both dropped columns of the claim side; its phases read (internal/record/store.go,
// Postgres.Phases); and the delivery read its retirement makes (RetireDelivery). Each must answer
// a row, and the columns the recovery brought back must read as the empty string their default
// leaves, which is the whole reason the header can say the older daemon's fence compares nothing
// and refuses none of the completions in flight.
//
// The fourth statement is the strip of this release's own payload keys, and what it leaves is
// read the same way: every supervise payload still in the outbox is decoded as that release
// decodes one — strictly, with DisallowUnknownFields, which is the decoder
// record.DecodeOutboxPayload builds — against preEntrySuperviseRequest, that release's own
// request shape at the same commit. A key it does not know refuses the row, and its
// record.Postgres.RoleRuns decodes every supervise row of the issues an admission pass reads, so
// one such row would retry for ever and fail every pass of its issue besides.
//
// The last seven statements are the re-arm: the six entry columns dropped and this migration's
// version row deleted. Neither is owed to the older daemon — it reads none of those columns, and
// its own embedded set holds no migration of this version — and both are owed to the
// roll-forward, which has to run this file again. What this reads back is what makes that run
// possible: none of the six columns standing on the three tables the migration adds them to, and
// no row of 20261111225552 in schema_version with every other version row left where it was,
// since store.apply decides each migration on its own row. A way back that put the six columns
// back to zero instead would leave this migration recorded, so it would never run again, and a
// role whose task that daemon lost inside the rollback window would read 0 against 0: level,
// owed nothing and swept nothing. What the re-armed migration makes of the token that daemon kept
// current is the next helper's
// (upgradeRollForwardSweepsTheLostTask).
//
// What this cannot show is that daemon running: no binary of it is here to boot against the
// recovered schema, so that it then mints a fresh token at each role's next start is not
// asserted, and neither is the decode this copies — what the copy shows is that the payloads the
// strip leaves carry no key outside that release's shape.
func upgradeRecoveryReadsAnswer(t *testing.T, pool *pgxpool.Pool, token claim.Token, issue string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		"alter table phases add column if not exists assignment text not null default '';",
		"alter table claims add column if not exists serving_assignment text not null default '';",
		"alter table pending_task_deliveries add column if not exists assignment text not null default '';",
		`update outbox set payload = payload - 'entry' - 'resendEntry'
			where kind = 'supervise' and (payload ? 'entry' or payload ? 'resendEntry');`,
		"alter table phases drop column if exists entry;",
		"alter table phases drop column if exists entry_task;",
		"alter table phases drop column if exists entry_raised_at;",
		"alter table claims drop column if exists given_entry;",
		"alter table claims drop column if exists served_entry;",
		"alter table pending_task_deliveries drop column if exists entry;",
		"delete from schema_version where version = 20261111225552;",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("run the recovery statement %q: %v", statement, err)
		}
	}

	const claimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.tree_epoch, c.session,
	c.session_file, c.locator, c.state, c.failure, c.launch_failures, c.deaths, c.prompt_failures, c.prompt_retires,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.last_start_row, c.serving_generation,
	c.serving_assignment, c.serving_check_look, c.serving_check_opened_at,
	c.agent_secrets_enrollment, c.agent_secrets_enrollment_incarnation, c.suspended_by,
	c.plugin_contract, c.plugin_relaunch_for, c.delivered_rows,
	c.update_round, c.update_reasons, c.update_opened_at, c.update_tasked_at, c.update_signalled_at,
	c.update_head, c.update_told_at, c.update_answered_by_turn_end, c.update_reasked_for_push_check,
	c.update_left_round, c.provider_error_run, c.death_charged,
	c.step_at, c.turn_started_at, c.step_tool, c.step_call_id, c.step_args_bytes,
	c.step_args_hash, c.step_started_at, c.step_ended_at,
	d.delivery_id, d.task, d.phase, d.generation, d.assignment, d.check_look, d.check_opened_at,
	d.queued_at, d.delivered_at, d.marked_by, d.confirmed_at, d.interrupted, d.out_of_memory,
	d.send_failures, d.last_send_failure_at, d.last_send_error,
	d.kept_decisions, d.provider_error_retries, d.provider_error_note
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`
	const phasesSelect = `select issue, role, claim, handoff_commit, handoff_record, handoff_version, rounds, conflict_rounds, backward_moves, cap_told, verdict, summary, last_handoff, decision, completed_at, merge_note, assignment from phases
		where issue = $1 order by role`
	const deliverySelect = "select delivery_id from pending_task_deliveries where claim_token = $1"

	for _, read := range []struct {
		what  string
		query string
		arg   any
		// empty names the columns the recovery brought back, which every row of the recovered
		// schema carries at the default's empty string.
		empty []string
	}{
		{what: "the claim read", query: claimSelect + " where c.token = $1", arg: string(token),
			empty: []string{"serving_assignment", "assignment"}},
		{what: "the phases read", query: phasesSelect, arg: issue, empty: []string{"assignment"}},
		{what: "the pending delivery read", query: deliverySelect, arg: string(token)},
	} {
		rows, err := pool.Query(ctx, read.query, read.arg)
		if err != nil {
			t.Fatalf("%s of the release before this one, after the recovery: %v", read.what, err)
		}
		// The field names are taken before the rows are read: pgx hands them out on the open
		// Rows, and the collect below closes it.
		fields := append([]pgconn.FieldDescription(nil), rows.FieldDescriptions()...)
		values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
		if err != nil {
			t.Fatalf("%s of the release before this one, after the recovery: %v", read.what, err)
		}
		if len(values) != 1 {
			t.Fatalf("%s of the release before this one answered %d rows, want 1: the recovery is what lets that daemon read this record at all",
				read.what, len(values))
		}
		for _, name := range read.empty {
			held, ok := columnOf(fields, values[0], name)
			if !ok {
				t.Fatalf("%s names no column %s: the recovery statements are what bring it back", read.what, name)
			}
			if held != "" {
				t.Fatalf("%s reads %s = %q, want the empty string: the recovery adds the column with that default on every row, which is why the older daemon's fence compares nothing",
					read.what, name, held)
			}
		}
	}

	// Every supervise payload the strip left, decoded as the release before this one decodes one.
	// A row it refuses is a row that retries for ever on that release, and an admission pass of
	// its issue that fails with it, so each must decode and the outbox must hold rows to decode:
	// the starts the sweep wrote and the start UPGRADE-807 owed, which ran but was never finished.
	rows, err := pool.Query(ctx, "select id, payload from outbox where kind = 'supervise' order by id")
	if err != nil {
		t.Fatalf("read the supervise rows the recovery left: %v", err)
	}
	payloads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		id      int64
		payload []byte
	}, error) {
		var held struct {
			id      int64
			payload []byte
		}
		err := row.Scan(&held.id, &held.payload)
		return held, err
	})
	if err != nil {
		t.Fatalf("read the supervise rows the recovery left: %v", err)
	}
	if len(payloads) == 0 {
		t.Fatal("the record holds no supervise row after the recovery, want the starts of the upgrade's own shapes: a decode of nothing shows nothing")
	}
	for _, held := range payloads {
		decoder := json.NewDecoder(bytes.NewReader(held.payload))
		decoder.DisallowUnknownFields()
		var request preEntrySuperviseRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatalf("the release before this one decodes outbox row %d, %s, as %v; want it decoded: a payload it refuses is a row that retries for ever, and every admission pass of its issue fails with it (record.Postgres.RoleRuns)",
				held.id, held.payload, err)
		}
	}

	// What the re-arm leaves, which is the whole of what lets the roll-forward run this file
	// again: none of the six columns the migration's first statements add, since `add column` on a
	// column that already stands fails, and no version row of its own.
	for _, gone := range []struct{ table, column string }{
		{table: "phases", column: "entry"},
		{table: "phases", column: "entry_task"},
		{table: "phases", column: "entry_raised_at"},
		{table: "claims", column: "given_entry"},
		{table: "claims", column: "served_entry"},
		{table: "pending_task_deliveries", column: "entry"},
	} {
		var standing bool
		if err := pool.QueryRow(ctx, `select exists (select 1 from information_schema.columns
			where table_schema = current_schema() and table_name = $1 and column_name = $2)`,
			gone.table, gone.column).Scan(&standing); err != nil {
			t.Fatalf("read whether %s.%s stands after the recovery: %v", gone.table, gone.column, err)
		}
		if standing {
			t.Fatalf("the recovery left %s.%s standing, want it dropped: the roll-forward runs this migration again from its first statement, which adds that column",
				gone.table, gone.column)
		}
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var armed, recorded int
	if err := pool.QueryRow(ctx, "select count(*) from schema_version where version = $1::bigint", phaseEntryMigration).Scan(&armed); err != nil {
		t.Fatalf("read whether the phase entry migration is still recorded: %v", err)
	}
	if armed != 0 {
		t.Fatalf("schema_version holds %d rows of the phase entry migration after the recovery, want none: its row is what store.apply reads to decide the migration is taken, and the roll-forward has to run it again", armed)
	}
	if err := pool.QueryRow(ctx, "select count(*) from schema_version").Scan(&recorded); err != nil {
		t.Fatalf("count the versions recorded after the recovery: %v", err)
	}
	if recorded != len(all)-1 {
		t.Fatalf("schema_version holds %d rows after the recovery, want the %d every other migration recorded: the delete names one version, and store.apply decides each migration on its own row",
			recorded, len(all)-1)
	}
}

// upgradeRollForwardSweepsTheLostTask is the roll-forward that follows the way back: the newer
// image comes back, Migrate runs this migration a second time because the way back deleted its
// version row, and the daemon's ordinary sweep runs over what the second fill left.
//
// The role it is read on is one whose task the older daemon lost while it ran, which is the
// window the re-arm exists for. The shape written here is a start finished without acting: that
// daemon handed the role its phase's work and minted the token onto the phases row, and the row
// that carried the task ended without delivering it, leaving no start row behind and a claim that
// was never given the work. That is the one of the two shapes this test can write faithfully —
// the other, a later transition's start written over the row an issue already owes, leaves a row
// the fill stamps, and the stamped row is what starts the role, so the sweep is not what sends
// it (the first half's seventh and tenth shapes read that path). It is written in that release's
// own SQL, a phases row carrying an assignment and a claim serving none, because this release's
// writers name the entry columns the way back has dropped.
//
// What the second run reads is the record that daemon kept current: the token on the phases row.
// The fill numbers the role entry 1 and finds no start row to take a task from, so the record
// holds no task of that entry and the sweep sends the line the daemon composes for a worker
// already in its phase (workflow.ResumePhaseTask, through Engine.SendOwedEntry), at entry 1.
//
// The control is this same half under the zeroing way back: the six entry columns left standing
// at zero and this migration's version row left where it is. Migrate then applies nothing, the
// role's entry and its claim both read 0, and the sweep sends it nothing at all, because a role
// level at zero is owed nothing (workflow.OwedEntry) and admit's owedEntryRoles selects by that
// same answer. Leaving the delete alone out of the way back is no control at all: the six drops
// still run, so the read of entry and entry_task below fails for a column that does not exist
// before anything is swept. That is why the way back drops the six columns and deletes the row
// together, rather than zeroing the columns.
//
// The ten shapes of the first half are left level by the same run: the recovery brought their
// assignment columns back empty on every row, so every statement of the second fill passes over
// them and the sweep writes no start for any of them, which is what the count of every other
// issue's start rows reads.
func upgradeRollForwardSweepsTheLostTask(t *testing.T, st *store.Store, w *workflowRuntime, root record.Issue) {
	t.Helper()
	ctx := context.Background()
	pool := st.Pool()
	const key = "UPGRADE-811"
	project, err := claim.ProjectToken(upgradeProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	parent := root.Key
	issue := record.Issue{Key: key, Tree: root.Key, Parent: &parent, Project: upgradeProject,
		Title: "the role whose start finished without acting while the older image ran", Phase: phase.Testing,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: key,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 10, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("record the issue the older daemon admitted after the rollback: %v", err)
	}
	// The phases row and the claim as that release writes them: the token it minted at the
	// transition stands on the row, and the claim serves none, because the start that would have
	// given it the work finished without acting. No outbox row stands beside them for the same
	// reason, so the second fill finds the role a task nowhere.
	if _, err := pool.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, assignment)
		values ($1, 'tester', $2, '', 0, '', $3)`, key, string(token), "a-811-minted-after-the-rollback"); err != nil {
		t.Fatalf("record the phases row the older daemon left on %s: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, capability_hash,
		serving_generation, serving_assignment, suspended_by)
		values ($1, $2, $3, $4, 'tester', 1, 'ses-811', '', $5, 0, 0, 0, 0, $6, 1, '', '')`,
		string(token), project, root.Key, key, string(supervise.StateReady), []byte("capability-"+key)); err != nil {
		t.Fatalf("record the claim the older daemon left on %s: %v", key, err)
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward onto the record the older daemon left: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the roll-forward applied %d migrations, want the phase entry migration run a second time: the way back deletes its version row, and store.apply decides each migration on its own row",
			applied)
	}
	var entry int64
	var entryTask string
	if err := pool.QueryRow(ctx, "select entry, entry_task from phases where issue = $1 and role = 'tester'",
		key).Scan(&entry, &entryTask); err != nil {
		t.Fatalf("read the phases row of %s after the roll-forward: %v", key, err)
	}
	if entry != 1 || entryTask != "" {
		t.Fatalf("the phases row of %s = entry %d carrying %q, want entry 1 carrying no task: the second fill reads the token that daemon minted, and the role owes no start to take a task from",
			key, entry, entryTask)
	}
	var given, served int64
	if err := pool.QueryRow(ctx, "select given_entry, served_entry from claims where token = $1",
		string(token)).Scan(&given, &served); err != nil {
		t.Fatalf("read the claim of %s after the roll-forward: %v", key, err)
	}
	if given != 0 || served != 0 {
		t.Fatalf("the claim of %s was given entry %d and served entry %d, want 0 and 0: it serves no token, so the fill counts it as holding none of that entry's work",
			key, given, served)
	}

	elsewhere := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise'
			and payload->>'op' = 'start' and issue <> $1`, key).Scan(&n); err != nil {
			t.Fatalf("count the start rows of every other issue: %v", err)
		}
		return n
	}
	before := elsewhere()
	w.applyEntrySweep(ctx, time.Now().Add(time.Hour))
	starts := sweptStarts(t, pool, key)
	if len(starts) != 1 {
		t.Fatalf("the sweep after the roll-forward sent %s %+v, want the one start of entry 1: a role whose task that daemon lost is what the re-arm exists to send again",
			key, starts)
	}
	want := sweptStart{id: starts[0].id, role: claim.RoleTester, task: workflow.ResumePhaseTask(issue), entry: 1}
	if starts[0] != want {
		t.Fatalf("the start the sweep sent %s = %+v, want %+v: the record holds no task of that entry, so what is sent is the line the daemon composes for a worker already in its phase",
			key, starts[0], want)
	}
	if after := elsewhere(); after != before {
		t.Fatalf("the sweep after the roll-forward wrote %d start rows elsewhere, want none beyond the %d standing: the recovery brought the assignment columns back empty, so the second fill passes over every shape of the first half and each stays level",
			after-before, before)
	}
}

// preEntrySuperviseRequest is record.SuperviseRequest as the release before this one declares it,
// at commit e90c078559: every field of this release's shape but the two it adds, Entry and
// ResendEntry. It is a copy because that release's own type is gone from the tree, and the
// recovery above is judged on exactly what that release would make of the payloads left behind.
type preEntrySuperviseRequest struct {
	Op            record.SuperviseOp    `json:"op"`
	Tree          string                `json:"tree"`
	Role          claim.Role            `json:"role"`
	Task          string                `json:"task,omitempty"`
	Kept          *record.KeptDecisions `json:"kept,omitempty"`
	Generation    uint64                `json:"generation"`
	Phase         phase.Phase           `json:"phase,omitempty"`
	Assignment    string                `json:"assignment,omitempty"`
	ResumeTask    bool                  `json:"resumeTask,omitempty"`
	RuleResume    bool                  `json:"ruleResume,omitempty"`
	NeedsSlot     bool                  `json:"needsSlot,omitempty"`
	Relaunch      bool                  `json:"relaunch,omitempty"`
	Linger        uint64                `json:"linger,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	CheckLook     int                   `json:"checkLook,omitempty"`
	CheckOpenedAt time.Time             `json:"checkOpenedAt,omitzero"`
}

// columnOf is the value of the named column of one read row, as a string, and false where the read
// names no such column.
func columnOf(fields []pgconn.FieldDescription, values []any, name string) (string, bool) {
	for i, field := range fields {
		if field.Name != name || i >= len(values) {
			continue
		}
		held, ok := values[i].(string)
		if !ok {
			return fmt.Sprintf("%v", values[i]), true
		}
		return held, true
	}
	return "", false
}
