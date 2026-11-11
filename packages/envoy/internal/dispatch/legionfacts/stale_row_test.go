package legionfacts

import (
	"testing"
	"time"
)

// TestAnIssueThatLosesItsPhaseWorkLosesItsStoredRow: a stored row means Legion worked the issue
// (dispatch://INFRA-2915's spec, "Every issue with phase work has a row, zero hours included. A
// row then means 'Legion worked it'"), and R1 covers every issue the project has Legion phase
// work for. Phase work is not only gained: a session's newest title decides which issue its
// events work, so a role session that writes next on another issue under that issue's title
// takes the work with it (legion.PhaseWork, and api/legion_phase_work.go, which states the same
// rule for the board). The issue it left then carries no phase work, and its row must go.
//
// It does not. Write only ever upserts and no pass deletes, so the row stands for ever and the
// board serves it. Measured against a real Dispatch server of this head on 2026-11-11: one
// session wrote a comment on WIDGETS-1 titled for that issue, and the board answered one entry
// with legion_issues 1; the same session then wrote on WIDGETS-2 titled for WIDGETS-2, and the
// board answered two entries with legion_issues 1 — its own header counting one issue of Legion
// work while its body carried two — and over four sweeps the stale row did not go. The
// independent reading of R1 answered one issue against the two stored rows.
//
// The issue here is one the rig says no Legion role worked, given its work by one session of
// this test, so the session is the whole of its phase work and moving that session takes all of
// it. TestEveryIssueLegionWorkedGetsAStoredRowAndNoOtherIssueDoes states the same rule over a
// fresh history, where no issue has lost its work yet.
func TestAnIssueThatLosesItsPhaseWorkLosesItsStoredRow(t *testing.T) {
	seeded := seed(t, 3)
	project := seeded.fixture.Project
	pool := seeded.database.Pool

	if len(seeded.fixture.NoPhaseWork) == 0 || len(seeded.fixture.PhaseWork) == 0 {
		t.Fatalf("the rig wrote %d issues with no phase work and %d with it, so this test examined nothing",
			len(seeded.fixture.NoPhaseWork), len(seeded.fixture.PhaseWork))
	}
	worked := seeded.fixture.NoPhaseWork[0]
	other := seeded.fixture.PhaseWork[0]
	const session = "s-one-role"

	// The one session's first event, on the issue its own title names: the issue carries Legion
	// phase work from here on.
	writeRoleEvent(t, seeded, session, worked, seeded.now.Add(-2*time.Hour))
	rows, err := ComputeProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	if _, err := Write(t.Context(), pool, rows); err != nil {
		t.Fatalf("write the facts: %v", err)
	}
	stored, err := Read(t.Context(), pool, project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}
	if _, has := stored[worked]; !has {
		t.Fatalf("%s carries Legion phase work and has no stored row, so this test examined nothing", worked)
	}

	// The same session writes next on another issue, under that issue's title. Its newest title
	// is what the rule reads, so the work goes with it and the issue it left has none.
	writeRoleEvent(t, seeded, session, other, seeded.now.Add(-time.Hour))
	moved, err := ComputeProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project's facts again: %v", err)
	}
	for _, row := range moved {
		if row.Issue == worked {
			t.Fatalf("%s still carries Legion phase work after its session moved, so this test examined nothing", worked)
		}
	}
	if _, err := Write(t.Context(), pool, moved); err != nil {
		t.Fatalf("write the facts again: %v", err)
	}

	after, err := Read(t.Context(), pool, project)
	if err != nil {
		t.Fatalf("read the facts back: %v", err)
	}
	if _, has := after[worked]; has {
		t.Errorf("%s carries no Legion phase work and keeps its stored row: the computation answers %d issues and the table holds %d rows",
			worked, len(moved), len(after))
	}
	t.Logf("%d issues carried phase work with %s worked, %d after its session moved to %s, and the table holds %d rows",
		len(rows), worked, len(moved), other, len(after))
}

// writeRoleEvent writes one event of a Legion role's session on an issue, under a title that
// names that issue, which is what gives the issue Legion phase work. The moment matters: a
// session's newest title is its newest titled event by created_at (delivery.ResolveSessionTitles),
// so two events of one session at one moment leave which title is newest undecided.
func writeRoleEvent(t *testing.T, seeded history, session, issue string, at time.Time) {
	t.Helper()
	if _, err := seeded.database.Pool.Exec(t.Context(), `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
		        'comment.created',
		        jsonb_build_object('kind', 'session', 'id', $2::text,
		          'origin', jsonb_build_object('session_title', 'Legion implementer · ' || $1)),
		        '{}'::jsonb, false, $3)`, issue, session, at); err != nil {
		t.Fatalf("write session %s's event on %s: %v", session, issue, err)
	}
}
