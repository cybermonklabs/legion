package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimHandshakeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimHandshakeVersion = 20261114020537

// 20261114020537 adds the four columns a claim keeps its handshake in (supervise.Claim.Handshake):
// the last entry it was queued a task of, that task's run, when a task of that entry was first
// queued and when a turn first confirmed one. Every claim the table holds before it reads back
// with no handshake, which is what lets the state read a root of the release before this one with
// both times absent: here the architect claim of a root the architect upgrade settled at entry 1,
// queued and confirming it with no task and no times. A handshake written after it reads back as
// it was written, first with no confirmation and then with one, and through a store reopened as a
// daemon restart reopens it, since a restarted daemon builds every claim from this table.
func TestTheClaimHandshakeMigrationLeavesEveryClaimWithNoneAndTheHandshakeRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimHandshakeVersion-1)
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'admitted', 3, 'in_progress', 'A', 1)`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, serving_generation, given_entry, served_entry)
			values ('legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 3, 'ses_a', '',
				'idle', 0, 0, 0, 0, 3, 1, 1)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before the migration: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim handshake: %v", err)
	}
	upgraded := onlyClaim(t, store)
	if upgraded.GivenEntry != 1 || upgraded.ServedEntry != 1 || upgraded.Handshake != (supervise.EntryHandshake{}) {
		t.Fatalf("the architect claim after the migration was given entry %d, served entry %d and holds the handshake %+v; "+
			"want 1, 1 and none: a claim of the release before this one names no handshake, so both times read as absent",
			upgraded.GivenEntry, upgraded.ServedEntry, upgraded.Handshake)
	}

	// The architect's entry 2, queued and taken by no turn yet.
	want := upgraded
	want.GivenEntry = 2
	want.Handshake = supervise.EntryHandshake{Entry: 2, Generation: 3, QueuedAt: at(3)}
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put the claim queued entry 2: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)

	// A turn confirmed it, and the turn's end retired the task: the handshake is the claim's row.
	want.ServedEntry = 2
	want.Handshake.ConfirmedAt = at(4)
	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put the claim whose turn confirmed entry 2: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), want)
	sameClaim(t, onlyClaim(t, reopenStore(t, store)), want)
}
