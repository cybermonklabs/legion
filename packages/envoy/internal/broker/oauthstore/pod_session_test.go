package oauthstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestAWriteWithoutAThumbprintKeepsTheBinding: a registration bound to a role's key stays bound
// while the row lives. A later write naming no key, which is how a session watch writes,
// extends the row and leaves the key required: an exchange proved with another key is refused as
// bound to another key, and one proved with the bound key is admitted.
func TestAWriteWithoutAThumbprintKeepsTheBinding(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, session, kA, kB = "pod-uid-issue", "reviewer-g1", "thumbprint-of-role-a", "thumbprint-of-role-b"
	if stored, err := s.RegisterPodSession(ctx, pod, session, kB, 0); err != nil || stored.JKT != kB {
		t.Fatalf("RegisterPodSession with %s = %+v, %v; want the registration bound to %s", kB, stored, err, kB)
	}
	if stored, err := s.RegisterPodSession(ctx, pod, session, "", 0); err != nil || stored.JKT != kB {
		t.Fatalf("RegisterPodSession with no thumbprint = %+v, %v; want the binding to %s kept", stored, err, kB)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kA); registered || !errors.Is(err, oauthstore.ErrPodSessionBoundToAnotherKey) {
		t.Fatalf("PodSessionRegistered with %s after a write naming no key = %t, %v; want false, ErrPodSessionBoundToAnotherKey: the write removed the binding to %s", kA, registered, err, kB)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kB); !registered || err != nil {
		t.Fatalf("PodSessionRegistered with %s = %t, %v; want true", kB, registered, err)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, "another-session", kB); registered || err != nil {
		t.Fatalf("PodSessionRegistered for a session never registered = %t, %v; want false and no error", registered, err)
	}
}

// TestALateOlderGenerationLeavesTheNewerKey: a role's new incarnation writes (P, S, kB, gen 2),
// and its old incarnation's write (P, S, kA, gen 1), which the broker commits only afterwards,
// changes nothing: the row stays bound to kB at generation 2 with the expiry the newer write
// left, the late write is answered that row, and the exchange keeps admitting kB and refusing kA.
func TestALateOlderGenerationLeavesTheNewerKey(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, session, kA, kB = "pod-uid-issue", "implementer-g1", "thumbprint-of-the-old-incarnation", "thumbprint-of-the-new-incarnation"
	newer, err := s.RegisterPodSession(ctx, pod, session, kB, 2)
	if err != nil || newer.JKT != kB || newer.Generation != 2 {
		t.Fatalf("RegisterPodSession with %s at generation 2 = %+v, %v; want the registration bound to %s at generation 2", kB, newer, err, kB)
	}
	late, err := s.RegisterPodSession(ctx, pod, session, kA, 1)
	if err != nil || late.JKT != kB || late.Generation != 2 || !late.ExpiresAt.Equal(newer.ExpiresAt) {
		t.Fatalf("a late write with %s at generation 1 after %s at generation 2 = %+v, %v; want it answered the unchanged row %+v: the old incarnation's write put its key back", kA, kB, late, err, newer)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kA); registered || !errors.Is(err, oauthstore.ErrPodSessionBoundToAnotherKey) {
		t.Fatalf("PodSessionRegistered with the old key %s = %t, %v; want false, ErrPodSessionBoundToAnotherKey", kA, registered, err)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kB); !registered || err != nil {
		t.Fatalf("PodSessionRegistered with the new key %s = %t, %v; want true", kB, registered, err)
	}
}

// TestAWriteOfTheSameGenerationIsApplied: a write of the row's own generation is applied, as a
// renewal of one incarnation and every write of a session watch, which sends none, are: a key it
// names replaces the row's, and its expiry is never earlier than the row's.
func TestAWriteOfTheSameGenerationIsApplied(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, kA, kB = "pod-uid-issue", "thumbprint-of-role-a", "thumbprint-of-role-b"
	for _, generation := range []int64{0, 3} {
		session := fmt.Sprintf("session-at-generation-%d", generation)
		first, err := s.RegisterPodSession(ctx, pod, session, kA, generation)
		if err != nil {
			t.Fatalf("RegisterPodSession at generation %d: %v", generation, err)
		}
		again, err := s.RegisterPodSession(ctx, pod, session, kB, generation)
		if err != nil || again.JKT != kB || again.Generation != generation || again.ExpiresAt.Before(first.ExpiresAt) {
			t.Fatalf("a second write at generation %d with %s after %+v = %+v, %v; want it applied: bound to %s at generation %d", generation, kB, first, again, err, kB, generation)
		}
	}
}

// TestAWriteSaysWhetherItWasAppliedAndWhatItReplaced: each write answers whether it set the row
// and the key it replaced with another. A first write replaces nothing; a renewal with the key
// the row holds, a write naming no key and a write binding a row bound to the pod alone replace
// nothing; a write naming another key replaces the row's; and a write of a lower generation is
// not applied and replaces nothing.
func TestAWriteSaysWhetherItWasAppliedAndWhatItReplaced(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, kA, kB = "pod-uid-issue", "thumbprint-of-role-a", "thumbprint-of-role-b"
	for _, w := range []struct {
		what, session, jkt string
		generation         int64
		applied            bool
		replaced           string
	}{
		{"a first write with kA", "implementer-g1", kA, 1, true, ""},
		{"a renewal with kA", "implementer-g1", kA, 1, true, ""},
		{"a write naming no key", "implementer-g1", "", 1, true, ""},
		{"a write with kB", "implementer-g1", kB, 2, true, kA},
		{"a late write with kA at a lower generation", "implementer-g1", kA, 1, false, ""},
		{"a first write naming no key", "watched-session", "", 0, true, ""},
		{"a write binding the pod's row to kA", "watched-session", kA, 0, true, ""},
	} {
		stored, err := s.RegisterPodSession(ctx, pod, w.session, w.jkt, w.generation)
		if err != nil || stored.Applied != w.applied || stored.Replaced != w.replaced {
			t.Fatalf("%s of %s at generation %d = %+v, %v; want applied %t, replacing %q", w.what, w.session, w.generation, stored, err, w.applied, w.replaced)
		}
	}
}

// TestAWriteNamesTheKeyTheRowHeldWhenItWrote: the key a write says it replaced is the one the row
// held when this write changed it, not one an earlier read of the row saw. A write that waits on
// another writer's change of the row's key names the key that writer committed; a write that
// finds no row and then waits on another writer's insert of it names the key that writer
// inserted, rather than reporting a first write.
func TestAWriteNamesTheKeyTheRowHeldWhenItWrote(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	const pod, kA, kB, kC = "pod-uid-issue", "thumbprint-a", "thumbprint-b", "thumbprint-c"
	// race runs other, another writer's statement, in a transaction of its own, writes kC at
	// generation 1 while that transaction is open, waits until the write waits on it, and commits
	// it: the write's answer is what it stored after the other writer committed.
	race := func(t *testing.T, session, other string) oauthstore.PodSessionRegistration {
		t.Helper()
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the other writer's transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, other, pod, session, kB); err != nil {
			t.Fatalf("the other writer's %q: %v", other, err)
		}
		type answer struct {
			stored oauthstore.PodSessionRegistration
			err    error
		}
		answered := make(chan answer, 1)
		go func() {
			stored, err := s.RegisterPodSession(ctx, pod, session, kC, 1)
			answered <- answer{stored, err}
		}()
		storetest.AwaitLockWaiters(t, pool, holder, 1)
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("commit the other writer's transaction: %v", err)
		}
		got := <-answered
		if got.err != nil {
			t.Fatalf("the write of %s with %s: %v", session, kC, got.err)
		}
		return got.stored
	}

	if _, err := s.RegisterPodSession(ctx, pod, "changed", kA, 1); err != nil {
		t.Fatalf("register %s with %s: %v", "changed", kA, err)
	}
	changed := race(t, "changed", `update oauth_pod_sessions set jkt = $3 where pod_uid = $1 and session = $2`)
	if !changed.Applied || changed.JKT != kC || changed.Replaced != kB {
		t.Errorf("a write with %s waiting on another writer's change of %s to %s = %+v; want it applied, bound to %s, replacing %s, the key the other writer committed",
			kC, kA, kB, changed, kC, kB)
	}

	inserted := race(t, "inserted", `insert into oauth_pod_sessions (pod_uid, session, expires_at, jkt, generation)
		values ($1, $2, now() + interval '1 day', $3, 1)`)
	if !inserted.Applied || inserted.JKT != kC || inserted.Replaced != kB {
		t.Errorf("a write with %s waiting on another writer's insert of the row with %s = %+v; want it applied, bound to %s, replacing %s, the key the other writer inserted",
			kC, kB, inserted, kC, kB)
	}
}
