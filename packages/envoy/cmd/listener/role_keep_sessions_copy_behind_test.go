package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The kept lane's handover resolves the role's holder with the holder's session record read from
// the session bucket stream's leader (handOverKeptRole, leaderHolderSessions), so a copy kept for
// the role reaches a holder this listener's cache has not caught up with, even where the copy of
// the session bucket a direct get reaches has not applied that holder's registration either. The
// sweep is the one start of a handover in this row (sweepOnce): the holder registered and claimed
// the role through another listener's stores, which starts no handover on this one, and the copy
// was kept with no handover started behind it. When the handover read the record with a direct
// get, the copy answered it absent, the holder read lapsed, and the walk with no holder left the
// copy on the lane for the next sweep, minutes later, which would read the record the same way.
// Control: with a current copy the same sweep forwards the kept copy to the holder.
func TestTheKeptLaneSweepHandsACopyToAHolderWhileTheSessionsCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the session bucket's copy is behind the holder's registration", true},
		{"control: the session bucket's copy is current", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const role, holder = "kept-sweep-sessions-behind", "ses_kept_sweep_sessions_behind"
			claimant, observer := trailingListeners(t)
			claimRoleFor(t, claimant.registry, claimant.sessions, holder, role, nil)
			kept, err := keepRoleMessage(*observer.delivery, role, listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-sweep-sessions-behind-00"))
			if err != nil {
				t.Fatalf("keep a copy on role %s's lane: %v", role, err)
			}
			if tc.behind {
				behindForSession(t, observer, holder)
			} else if _, err := observer.sessions.Get(holder); err == nil {
				t.Fatalf("the observer's session cache holds %s, so the handover's read of the bucket is not under test", holder)
			}

			sweepOnce(testwait.Context(t), *observer.delivery)
			held := storedEnvelopes(t, observer.client, contracts.LegacyAgentSubject(holder))
			if n := copiesOf(held, kept.item.EventID); n != 1 {
				t.Fatalf("after one sweep the stream holds %d forwards of the kept copy %s on %s's agent subject, want 1: %s holds %s live",
					n, kept.item.EventID, holder, holder, role)
			}
		})
	}
}
