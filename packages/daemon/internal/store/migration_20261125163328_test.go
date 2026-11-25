package store

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// The migration marks no claim and stops no clock it finds. Its way back, which an operator runs
// before a release from before it starts, makes every held catch-up due as one that was never
// paced and takes the turns out of every catch-up that carries them: that release publishes a
// notice only once it is due, and refuses a notice with a field it does not know. A held notice of
// another kind is none of this release's, and stays as it is.
func TestMessageWakePacingUpgradeAndWayBack(t *testing.T) {
	const version = 20261125163328
	ctx := t.Context()
	st := emptyStore(t)
	migrateThrough(t, st, version-1)
	exec := func(sql string) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-legion-legion-208-implementer', 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1,
		'session', '/session.jsonl', 'suspended', 0, 0, 0, 0)`)
	exec(`insert into pod_resumes (project, issue, role, kind, at)
		values ('legion', 'LEGION-208', 'implementer', 'delivery', now())`)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(query string) bool {
		t.Helper()
		var got bool
		if err := st.Pool().QueryRow(ctx, query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !read(`select message_mark_at is null from claims`) || read(`select queued from pod_resumes`) {
		t.Fatal("the migration marked a claim it found, or stopped the clock of a resume it found")
	}
	exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values
		('notice', 'LEGION-208', '{"kind":"catch-up", "paced_at":"2100-01-01T00:00:00Z", "paced_from":"2026-11-25T09:00:00Z"}', 0, '2100-01-01T00:00:00Z', ''),
		('notice', 'LEGION-208', '{"kind":"catch-up", "paced_at":"2026-11-25T09:05:00Z", "paced_from":"2026-11-25T09:00:00Z", "paced_turns":3}', 0, now(), ''),
		('notice', 'LEGION-208', '{"kind":"other"}', 0, '2100-01-01T00:00:00Z', '')`)
	wayBack, err := migrations.WayBack(version)
	if err != nil {
		t.Fatal(err)
	}
	exec(wayBack)
	if !read(`select bool_and(next_at <= now() and not (payload ? 'paced_turns')) from outbox where payload->>'kind' = 'catch-up'`) {
		t.Fatal("the way back left a catch-up held, or left one carrying its turns")
	}
	if !read(`select count(*) = 1 from outbox where payload->>'kind' = 'catch-up' and payload ? 'paced_at' and payload ? 'paced_from'`) {
		t.Fatal("the way back left the held moment on the catch-up it made due, or took the let-out off the one the pacer had let out")
	}
	if !read(`select next_at = '2100-01-01T00:00:00Z' from outbox where payload->>'kind' = 'other'`) {
		t.Fatal("the way back made a notice of another kind due")
	}
}
