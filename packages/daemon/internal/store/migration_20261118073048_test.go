package store

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// phaseEntryAwaitingVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const phaseEntryAwaitingVersion = 20261118073048

// 20261118073048 keeps on a resume's entry the wait it answers (record.PhaseRow.EntryAwaiting)
// where 20261114061900 kept the mark that the entry's task is sent as written. An entry the
// release before marked is a resume's whose wait that release never kept: it reads back holding
// the empty wait, so the sweep's reach keeps its root and a send of it again renders the resume
// from the record (workflow.Engine.SendArchitectStart). Every other row reads back holding none,
// and the mark's column is gone. A wait written after it reads back as it was written, through a
// store reopened as a daemon restart reopens it.
//
// The way back the header carries puts the mark back as the release before reads it, set on the
// entries that hold a wait and on no other, and run a second time it changes nothing. Migrate then
// runs this file again, and each entry the way back marked holds the empty wait: the wait itself
// is what a rollback costs.
func TestThePhaseEntryAwaitingMigrationKeepsAResumesEntryAndItsWayBackPutsTheMarkBack(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, phaseEntryAwaitingVersion-1)
	const resumed = "A person answered your ask ask-1 on LEGION-208; read it. Continue."
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'planning', 3, 'in_progress', 'A', 1)`,
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-209', 'LEGION-209', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'B', 1)`,
		// The architect's entry 2, which the resume of a person's answer raised and marked.
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, entry_verbatim)
			values ('LEGION-208', 'architect', '', '', 0, '', 2, '` + resumed + `', true)`,
		// A phase worker's entry and an architect entry the derivation wrote, neither marked.
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
			values ('LEGION-208', 'planner', 'legion-LEGION-208-planner', '', 0, '', 1, 'Plan the change.')`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
			values ('LEGION-209', 'architect', '', '', 0, '', 1, 'LEGION-209 is yours.')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the wait: %v", err)
	}
	rowOf := func(st *Store, issue string, role claim.Role) record.PhaseRow {
		t.Helper()
		var rows []record.PhaseRow
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			var err error
			rows, err = record.NewStore().Phases(ctx, tx, issue)
			return err
		}); err != nil {
			t.Fatalf("read the phases rows of %s: %v", issue, err)
		}
		return record.PhaseRowOf(rows, role)
	}
	if row := rowOf(store, "LEGION-208", claim.RoleArchitect); row.Entry != 2 || row.EntryTask != resumed ||
		row.EntryAwaiting == nil || *row.EntryAwaiting != (record.Awaiting{}) {
		t.Fatalf("the resumed architect's row after the migration = entry %d, task %q, wait %+v; want entry 2 and its task holding the empty wait: "+
			"the release before kept no wait, and the entry is still the resume's", row.Entry, row.EntryTask, row.EntryAwaiting)
	}
	for _, other := range []struct {
		issue string
		role  claim.Role
	}{{"LEGION-208", claim.RolePlanner}, {"LEGION-209", claim.RoleArchitect}} {
		if row := rowOf(store, other.issue, other.role); row.Entry != 1 || row.EntryAwaiting != nil {
			t.Fatalf("the %s's row of %s after the migration = entry %d, wait %+v; want entry 1 holding none: no resume raised it",
				other.role, other.issue, row.Entry, row.EntryAwaiting)
		}
	}
	entries := func(st *Store) map[record.PhaseRole]record.PhaseEntry {
		t.Helper()
		var read map[record.PhaseRole]record.PhaseEntry
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			var err error
			read, err = record.NewStore().PhaseEntries(ctx, tx, []string{"LEGION-208", "LEGION-209"})
			return err
		}); err != nil {
			t.Fatalf("read the phase entries: %v", err)
		}
		return read
	}
	if read := entries(store); !read[record.PhaseRole{Issue: "LEGION-208", Role: claim.RoleArchitect}].Resumed ||
		read[record.PhaseRole{Issue: "LEGION-208", Role: claim.RolePlanner}].Resumed ||
		read[record.PhaseRole{Issue: "LEGION-209", Role: claim.RoleArchitect}].Resumed {
		t.Fatalf("the phase entries after the migration = %+v, want the resumed architect's alone read as a resume's", read)
	}
	if columnStands(t, store, "entry_verbatim") {
		t.Fatal("phases.entry_verbatim stands after the migration, want it dropped: nothing reads it, and the wait replaced it")
	}

	// The resume of an answered root raises LEGION-209's architect entry 2 on a changes request.
	answeredAt := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	wait := record.Awaiting{GateVersion: 4, Artifact: "artifact-209", AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByChanges,
		AnsweredDetail: `version 4: "name the rollback path"`, AnsweredVersion: 4,
		Since: record.EventMark{Seq: 10, At: answeredAt.Add(-time.Hour)}}
	want := rowOf(store, "LEGION-209", claim.RoleArchitect)
	want.Entry, want.EntryTask, want.EntryAwaiting = 2, "A person requested changes to your spec on LEGION-209.", &wait
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, want)
	}); err != nil {
		t.Fatalf("put the architect's row of the resume's entry: %v", err)
	}
	for _, st := range []*Store{store, reopenStore(t, store)} {
		got := rowOf(st, "LEGION-209", claim.RoleArchitect)
		if got.Entry != 2 || got.EntryAwaiting == nil || !sameWait(*got.EntryAwaiting, wait) {
			t.Fatalf("the architect's row = entry %d, wait %+v; want entry 2 holding %+v, as it was put", got.Entry, got.EntryAwaiting, wait)
		}
		if !entries(st)[record.PhaseRole{Issue: "LEGION-209", Role: claim.RoleArchitect}].Resumed {
			t.Fatal("the phase entry of the architect a resume raised is not read as a resume's, want it read so")
		}
	}

	wayBack := headerWayBack(t, phaseEntryAwaitingVersion)
	for run := 1; run <= 2; run++ {
		if _, err := store.Pool().Exec(ctx, wayBack, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run the way back the header carries, run %d: %v", run, err)
		}
		if columnStands(t, store, "entry_awaiting") || !columnStands(t, store, "entry_verbatim") {
			t.Fatalf("after run %d of the way back entry_awaiting stands: %t, entry_verbatim stands: %t; want the release before's column alone",
				run, columnStands(t, store, "entry_awaiting"), columnStands(t, store, "entry_verbatim"))
		}
		// The release before reads its rows with this column named (record's Phases at that release).
		marked := map[string]bool{}
		rows, err := store.Pool().Query(ctx, `select issue || '/' || role, entry_verbatim from phases`)
		if err != nil {
			t.Fatalf("read the phases rows as the release before reads them: %v", err)
		}
		for rows.Next() {
			var key string
			var verbatim bool
			if err := rows.Scan(&key, &verbatim); err != nil {
				t.Fatalf("read a phases row as the release before reads it: %v", err)
			}
			marked[key] = verbatim
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the phases rows as the release before reads them: %v", err)
		}
		if wantMarked := map[string]bool{"LEGION-208/architect": true, "LEGION-208/planner": false, "LEGION-209/architect": true}; !maps.Equal(marked, wantMarked) {
			t.Fatalf("after run %d of the way back the marks = %v, want %v: each entry that held a wait is a resume's, which that release sends as written",
				run, marked, wantMarked)
		}
		if recorded := timesRecorded(t, store, phaseEntryAwaitingVersion); recorded != 0 {
			t.Fatalf("schema_version holds %d rows of this migration after run %d of the way back, want none: the roll-forward has to run it again", recorded, run)
		}
	}

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the roll-forward applied %d migrations, want this one run a second time", applied)
	}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		if row := rowOf(store, issue, claim.RoleArchitect); row.EntryAwaiting == nil || *row.EntryAwaiting != (record.Awaiting{}) {
			t.Fatalf("the architect's row of %s after the roll-forward holds the wait %+v, want the empty wait: the way back kept the mark and not the wait",
				issue, row.EntryAwaiting)
		}
	}
	if row := rowOf(store, "LEGION-208", claim.RolePlanner); row.EntryAwaiting != nil {
		t.Fatalf("the planner's row after the roll-forward holds the wait %+v, want none", row.EntryAwaiting)
	}
}

// columnStands is whether the phases table holds column.
func columnStands(t *testing.T, store *Store, column string) bool {
	t.Helper()
	var stands bool
	if err := store.Pool().QueryRow(context.Background(), `select exists (select 1 from information_schema.columns
		where table_schema = current_schema() and table_name = 'phases' and column_name = $1)`, column).Scan(&stands); err != nil {
		t.Fatalf("read whether phases.%s stands: %v", column, err)
	}
	return stands
}

// sameWait is whether got is want, each moment of it read as an instant: a wait read back from
// JSON keeps the instant of a moment and not always the location it was written in.
func sameWait(got, want record.Awaiting) bool {
	if !sameInstant(got.AnsweredAt, want.AnsweredAt) || !sameInstant(&got.Since.At, &want.Since.At) {
		return false
	}
	got.AnsweredAt, got.Since.At, want.AnsweredAt, want.Since.At = nil, time.Time{}, nil, time.Time{}
	return got == want
}

// headerWayBack is the one transaction the header of migration version carries for its way back:
// the lines of its psql heredoc, without the comment marks.
func headerWayBack(t *testing.T, version int) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != version {
			continue
		}
		var block []string
		inside := false
		for _, line := range strings.Split(migration.SQL, "\n") {
			text := strings.TrimPrefix(line, "--   ")
			switch {
			case !inside && strings.HasSuffix(text, "<<'SQL'"):
				inside = true
			case inside && text == "SQL":
				return strings.Join(block, "\n")
			case inside:
				block = append(block, text)
			}
		}
		t.Fatalf("the header of %s carries no psql heredoc closed by SQL", migration.Name)
	}
	t.Fatalf("no embedded migration has version %d", version)
	return ""
}
