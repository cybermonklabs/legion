package roles_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/roles"
)

// A path checks the fence before it publishes a forward, which bounds when the publish starts and
// not when the stream stores it. ForwardStoredTooLate is the second question, asked of the
// acknowledgement: a forward stored later than the fence and the path's own acknowledgement window
// after its resolution is too late to count as a delivery, since the takeover record that would
// copy the message to the role's next holder may have ended (cmd/listener takeoverRecordLife).
func TestForwardStoredTooLate(t *testing.T) {
	resolved := time.Unix(1_800_000_000, 0)
	const window = 2 * time.Second
	for _, row := range []struct {
		name     string
		resolved time.Time
		stored   time.Time
		window   time.Duration
		tooLate  bool
	}{
		{"stored at once", resolved, resolved, window, false},
		{"stored inside the fence", resolved, resolved.Add(roles.ForwardFenceAge - time.Second), window, false},
		{"stored past the fence, inside the window", resolved, resolved.Add(roles.ForwardFenceAge + window - time.Millisecond), window, false},
		{"stored at the fence and the window", resolved, resolved.Add(roles.ForwardFenceAge + window), window, true},
		{"stored long after", resolved, resolved.Add(2 * roles.ForwardFenceAge), window, true},
		{"a zero resolution is never too late", time.Time{}, resolved.Add(72 * time.Hour), window, false},
		{"a zero window is the fence itself", resolved, resolved.Add(roles.ForwardFenceAge), 0, true},
		{"stored before its resolution", resolved, resolved.Add(-time.Hour), window, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := roles.ForwardStoredTooLate(row.resolved, row.stored, row.window); got != row.tooLate {
				t.Errorf("ForwardStoredTooLate(%s, %s, %s) = %t, want %t",
					row.resolved.UTC().Format(time.RFC3339), row.stored.UTC().Format(time.RFC3339), row.window, got, row.tooLate)
			}
		})
	}
}

// ResolutionTooOld and ForwardStoredTooLate ask the two halves of one rule, and a forward that is
// too old to start is not yet too late to have been stored: a path that resolves again at the
// fence (refreshStaleRoleForward) is doing so inside the window this leaves it.
func TestAForwardTooOldToStartIsNotYetTooLateToStore(t *testing.T) {
	resolved := time.Unix(1_800_000_000, 0)
	at := resolved.Add(roles.ForwardFenceAge)
	if !roles.ResolutionTooOld(resolved, at) {
		t.Fatalf("a resolution %s old is not too old to start a forward on", roles.ForwardFenceAge)
	}
	if roles.ForwardStoredTooLate(resolved, at, time.Second) {
		t.Fatalf("a forward stored at the fence, inside a 1s window, is too late; want the window to count")
	}
}
