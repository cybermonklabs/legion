package store

import (
	"context"
	"testing"
)

// pastReviewStartsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const pastReviewStartsVersion = 20261110233602

// 20261110233602 takes the admission-slot mark off every queued start of a phase that holds no
// slot any more. An issue keeps one of the cap's slots until its work is past its review, except
// while it waits on another issue (blockers.waitsOf), so accepting, retro and merging hold none
// (record.KeepsSlot), and a start the previous release
// marked for a slot in one of those phases would wait for a row its issue never takes again:
// record.Store's outboxInOrder holds a marked start unclaimable while the slots table names no
// row of its issue, so the row would stand in the outbox for ever and the tree would stop at its
// retro.
//
// The rows here are written as the previous release wrote them, through SQL rather than through
// workflow.StartOrOwe, which no longer marks a start of those phases at all. The negative
// controls are the three rows the statement must leave alone: a marked start in implementing,
// which still holds a slot and still waits for it; a supervise row that is no start; and a row
// of another kind whose payload carries the same field name.
func TestTheStartsPastReviewLoseTheSlotMarkAfter20261110233602(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, pastReviewStartsVersion-1)
	queue := func(kind, issue, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '')`, kind, issue, payload); err != nil {
			t.Fatalf("queue the %s row of %s as the previous release wrote it: %v", kind, issue, err)
		}
	}
	start := func(issue, phase, role string) string {
		return `{"op":"start","tree":"LEGION-1","role":"` + role + `","generation":1,"phase":"` + phase +
			`","task":"Continue ` + issue + `.","resumeTask":true,"needsSlot":true}`
	}
	// The three phases that hold no slot now, each with the start its transition owed, and a
	// fourth row naming awaiting_merge, which the statement names beside them: that phase went
	// with the release before this one, so no deployment holds a start for it, and the row is
	// here to say what the statement does where one stood.
	queue("supervise", "LEGION-201", start("LEGION-201", "accepting", "architect"))
	queue("supervise", "LEGION-202", start("LEGION-202", "retro", "implementer"))
	queue("supervise", "LEGION-203", start("LEGION-203", "merging", "merger"))
	queue("supervise", "LEGION-204", start("LEGION-204", "awaiting_merge", "merger"))
	// The negative controls.
	queue("supervise", "LEGION-205", start("LEGION-205", "implementing", "implementer"))
	queue("supervise", "LEGION-206", `{"op":"stop","tree":"LEGION-1","role":"implementer","needsSlot":true}`)
	queue("notice", "LEGION-207", `{"kind":"children-first","reason":"LEGION-207 waits for the children open under it"}`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the cutover of the starts past a review: %v", err)
	}

	marked := func(issue string) *bool {
		t.Helper()
		var mark *bool
		if err := store.Pool().QueryRow(ctx, `select (payload->>'needsSlot')::bool from outbox where issue = $1`, issue).Scan(&mark); err != nil {
			t.Fatalf("read the slot mark of the row of %s: %v", issue, err)
		}
		return mark
	}
	for _, issue := range []string{"LEGION-201", "LEGION-202", "LEGION-203", "LEGION-204"} {
		if mark := marked(issue); mark != nil {
			t.Errorf("the start of %s carries needsSlot = %t after the migration, want the field gone: its phase holds no slot now, so a row that waits for one waits for ever", issue, *mark)
		}
	}
	if mark := marked("LEGION-205"); mark == nil || !*mark {
		t.Errorf("the start of LEGION-205 in implementing carries needsSlot = %v after the migration, want it still marked: implementing holds a slot, and the row waits for the row its issue takes", mark)
	}
	if mark := marked("LEGION-206"); mark == nil || !*mark {
		t.Errorf("the supervise row of LEGION-206 that is no start carries needsSlot = %v after the migration, want it untouched: the statement names starts alone", mark)
	}

	var kept int
	if err := store.Pool().QueryRow(ctx,
		`select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'children-first' and issue = 'LEGION-207'`).Scan(&kept); err != nil {
		t.Fatalf("count the children-first notice after the migration: %v", err)
	}
	if kept != 1 {
		t.Errorf("the children-first notice of LEGION-207 stands %d time(s) after the migration, want 1: the statement reads supervise rows and no other kind", kept)
	}

	// The task and the phase of a row the statement rewrote are the transition's own still, so
	// what runs is the start that was queued and not a narrower one.
	var task, phase string
	if err := store.Pool().QueryRow(ctx,
		`select payload->>'task', payload->>'phase' from outbox where issue = 'LEGION-202'`).Scan(&task, &phase); err != nil {
		t.Fatalf("read the payload of the retro start after the migration: %v", err)
	}
	if task != "Continue LEGION-202." || phase != "retro" {
		t.Errorf("the retro start of LEGION-202 = task %q in phase %q after the migration, want the payload the transition wrote, the mark alone taken off", task, phase)
	}
}
