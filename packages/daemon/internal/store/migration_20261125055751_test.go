package store

import (
	"context"
	"testing"
)

func TestTaskAgentPhaseKeysPreserveEachExistingEntry(t *testing.T) {
	st := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, st, 20261125055750)
	for _, query := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		 values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'testing', 'A', 1)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, step_task, step_place)
		 values ('LEGION-208', 'tester', 'legion-legion-legion-208-checker-2', 'checked-head', 3, 'PASS', 7, 'check a', 'a', 2),
		        ('LEGION-208', 'architect', '', '', 0, '', 4, 'own the root', '', 0)`,
	} {
		if _, err := st.Pool().Exec(ctx, query); err != nil {
			t.Fatalf("seed an earlier phase row: %v", err)
		}
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the phase key: %v", err)
	}
	var task, agent, head, entryTask string
	var entry, place, rounds int
	if err := st.Pool().QueryRow(ctx, `select task, agent, handoff_commit, entry, entry_task, step_place, rounds
		from phases where issue = 'LEGION-208' and role = 'tester'`).Scan(&task, &agent, &head, &entry, &entryTask, &place, &rounds); err != nil {
		t.Fatal(err)
	}
	if task != "a" || agent != "checker-2" || head != "checked-head" || entry != 7 || entryTask != "check a" || place != 2 || rounds != 3 {
		t.Fatalf("the task's named agent and entry were not preserved: task %q agent %q head %q entry %d task text %q place %d rounds %d",
			task, agent, head, entry, entryTask, place, rounds)
	}
	if err := st.Pool().QueryRow(ctx, `select task, agent, entry from phases
		where issue = 'LEGION-208' and role = 'architect'`).Scan(&task, &agent, &entry); err != nil {
		t.Fatal(err)
	}
	if task != "" || agent != "architect" || entry != 4 {
		t.Fatalf("the architect's entry changed: task %q agent %q entry %d", task, agent, entry)
	}
	// Exercise the primary key itself, not a second copy of the Go selector. These both
	// conflict with the earlier (issue, role) key, but differ in separate new key components.
	for _, query := range []string{
		`insert into phases (issue, task, role, agent, claim, handoff_commit, rounds, verdict)
		 values ('LEGION-208', 'b', 'tester', 'checker-2', 'legion-legion-legion-208-checker-2', '', 0, '')`,
		`insert into phases (issue, task, role, agent, claim, handoff_commit, rounds, verdict)
		 values ('LEGION-208', 'a', 'tester', 'checker-3', 'legion-legion-legion-208-checker-3', '', 0, '')`,
	} {
		if _, err := st.Pool().Exec(ctx, query); err != nil {
			t.Fatalf("a distinct phase identity was refused: %v", err)
		}
	}
	if _, err := st.Pool().Exec(ctx, `insert into phases (issue, task, role, agent, claim, handoff_commit, rounds, verdict)
		values ('LEGION-208', 'a', 'tester', 'checker-2', 'legion-legion-legion-208-checker-2', '', 0, '')`); err == nil {
		t.Fatal("the primary key accepted the same task, role and agent twice")
	}
}
