package store

import (
	"context"
	"testing"
)

// dropControllerWakesVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const dropControllerWakesVersion = 20261115121838

// A queued `awaiting-person` or `unslotted` controller notice is a row this release cannot run:
// the daemon tells the controller no notice its skill says to do nothing on
// (#4065), so both kinds left record's controllerOnlyNoticeKind, and
// record.DecodeOutboxPayload refuses a controller notice of a kind it does not know on every
// attempt. A row the release before this one wrote would fail for ever on its backoff.
//
// The negative controls are the rows queued beside them: a controller notice of a kind this
// release still writes, and an architect's notice, stand untouched after the migration, so the
// delete takes the two unrunnable kinds and nothing else.
func TestTheControllerNoticesNoReleaseWritesAreGoneAfter20261115121838(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, dropControllerWakesVersion-1)
	queue := func(kind, issue, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '')`, kind, issue, payload); err != nil {
			t.Fatalf("queue the %s row of %s as the previous release wrote it: %v", kind, issue, err)
		}
	}
	// The rows the previous release wrote: workflow's enterAwaiting and admit's releaseUnslotted,
	// each through record.NewOutboxRow.
	queue("controller_notice", "LEGION-208", `{"kind":"awaiting-person","reason":"LEGION-208 waits on a person (a question): it keeps its admission slot until they answer"}`)
	queue("controller_notice", "LEGION-209", `{"kind":"unslotted","reason":"LEGION-209 was in_progress, a status only a running tree has, while no tree of it runs"}`)
	// The negative controls: a controller notice this release still writes, and an architect's
	// notice of a kind the controller no longer reads.
	queue("controller_notice", "LEGION-210", `{"kind":"tree-stalled","reason":"LEGION-210 has stopped. The tree has been quiet for 7.0 hours."}`)
	queue("notice", "LEGION-211", `{"kind":"session-lost","role":"implementer","reason":"the session store holds no session for this claim"}`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the drop of the controller notices: %v", err)
	}

	var stranded int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = 'controller_notice'
		and payload->>'kind' in ('awaiting-person', 'unslotted')`).Scan(&stranded); err != nil {
		t.Fatalf("count the queued awaiting-person and unslotted notices after the migration: %v", err)
	}
	if stranded != 0 {
		t.Errorf("%d awaiting-person or unslotted controller notice(s) survive the migration, want none: this release decodes neither kind, so each one fails for ever", stranded)
	}

	var kept int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where (kind = 'controller_notice' and issue = 'LEGION-210')
		or (kind = 'notice' and issue = 'LEGION-211')`).Scan(&kept); err != nil {
		t.Fatalf("count the negative controls after the migration: %v", err)
	}
	if kept != 2 {
		t.Errorf("%d of the two negative controls stand after the migration, want both: the delete takes the rows no daemon of this release can run and no others", kept)
	}
}
