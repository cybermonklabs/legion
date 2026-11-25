package store

import (
	"context"
	"testing"
)

// TestADeletedProjectTakesItsSessionRowsWithIt: session_projects names its project by key with no
// foreign key (so a session's write takes no lock on projects), and a project's delete sweeps the
// rows that name it instead, leaving every other project's rows as they were.
func TestADeletedProjectTakesItsSessionRowsWithIt(t *testing.T) {
	ctx := context.Background()
	store := openMigratedProjectStore(t)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('GONE', 'Gone', 'carol@example.com'), ('KEPT', 'Kept', 'carol@example.com')
	`); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into session_projects (session_id, project_key) values ('ses_a', 'GONE'), ('ses_a', 'KEPT'), ('ses_b', 'GONE')
	`); err != nil {
		t.Fatalf("seed session_projects: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `delete from projects where key = 'GONE'`); err != nil {
		t.Fatalf("delete GONE: %v", err)
	}
	var gone, kept int
	if err := store.Pool.QueryRow(ctx, `
		select count(*) filter (where project_key = 'GONE'), count(*) filter (where project_key = 'KEPT') from session_projects
	`).Scan(&gone, &kept); err != nil {
		t.Fatalf("count session_projects: %v", err)
	}
	if gone != 0 || kept != 1 {
		t.Fatalf("after GONE's delete, session_projects holds %d rows of GONE and %d of KEPT, want 0 and 1", gone, kept)
	}
}
