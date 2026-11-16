package oauthstore_test

import (
	"context"
	"errors"
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
	if _, bound, err := s.RegisterPodSession(ctx, pod, session, kB); err != nil || bound != kB {
		t.Fatalf("RegisterPodSession with %s = %q, %v; want the registration bound to %s", kB, bound, err, kB)
	}
	if _, bound, err := s.RegisterPodSession(ctx, pod, session, ""); err != nil || bound != kB {
		t.Fatalf("RegisterPodSession with no thumbprint = %q, %v; want the binding to %s kept", bound, err, kB)
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
