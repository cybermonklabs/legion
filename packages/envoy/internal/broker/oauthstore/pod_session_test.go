package oauthstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
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
