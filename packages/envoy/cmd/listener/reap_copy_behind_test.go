package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// absentSessionCopyKV is the session bucket as a direct get of a replicated bucket can answer it: a
// Get of key answers that the bucket holds no record, as a copy that has not applied the session's
// registration answers; every other call is the bucket's own, and a read from the stream's leader
// does not go through this handle.
type absentSessionCopyKV struct {
	nats.KeyValue
	key string
}

func (kv absentSessionCopyKV) Get(key string) (nats.KeyValueEntry, error) {
	if key == kv.key {
		return nil, nats.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

// behindForSession makes the copy of the session bucket observer's direct gets reach answer
// sessionID absent, after checking that observer's session cache does not hold sessionID, so the
// reaper's read of the bucket is what the row tests.
func behindForSession(t *testing.T, observer *listenerDeps, sessionID string) {
	t.Helper()
	if _, err := observer.sessions.Get(sessionID); err == nil {
		t.Fatalf("the observer's session cache holds %s, so the reaper's bucket read is not under test", sessionID)
	}
	swapSessionBucket(t, observer.sessions, func(kv nats.KeyValue) nats.KeyValue {
		return absentSessionCopyKV{KeyValue: kv, key: sessionID}
	})
	if _, err := observer.sessions.Refresh(context.Background(), sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("a direct get of %s through the copy behind = %v, want %v: the copy is not as the row needs it", sessionID, err, nats.ErrKeyNotFound)
	}
}

// The interest reaper keeps the interest of a session the session bucket holds, even where the
// copy of the bucket a direct get reaches has not applied the session's registration: the reaper
// asks the stream's leader whether the session is gone (sessionMayBeLive). Before, the copy's
// "no record" made the reaper delete a live session's interest, and messages for its topics went
// nowhere until it subscribed again. Control: the interest of a session the bucket has no record
// of is reaped through the same copy.
func TestTheInterestReaperKeepsTheInterestOfASessionACopyBehindAnswersAbsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		registered bool
	}{
		{"a registered session the copy answers absent", true},
		{"control: a session the bucket has no record of", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimant, observer := trailingListeners(t)
			const sessionID = "ses_reap_interest_copy_behind"
			interests, err := claimant.client.JS().KeyValue(bus.InterestBucket)
			if err != nil {
				t.Fatalf("open the interest bucket: %v", err)
			}
			if tc.registered {
				if err := claimant.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
					t.Fatalf("register %s: %v", sessionID, err)
				}
			}
			stored, err := json.Marshal(store.Interest{SessionID: sessionID, MachineID: "test-machine", Topics: []string{"notifications.agent." + sessionID}, UpdatedAt: time.Now().Add(-20 * time.Minute).UnixMilli()})
			if err != nil {
				t.Fatalf("encode %s's interest: %v", sessionID, err)
			}
			if _, err := interests.Put(sessionID, stored); err != nil {
				t.Fatalf("store %s's interest: %v", sessionID, err)
			}
			testwait.Eventually(t, "the observer's interest cache to hold "+sessionID, func() bool {
				_, cached := observer.registry.Cached(sessionID)
				return cached
			})
			behindForSession(t, observer, sessionID)

			reaped, err := observer.registry.Reap(func(id string) (bool, error) { return sessionMayBeLive(observer.sessions, id) }, 10*time.Minute)
			want := map[bool]int{true: 0, false: 1}[tc.registered]
			if err != nil || reaped != want {
				t.Errorf("reap interests = %d, %v; want %d", reaped, err, want)
			}
			_, err = interests.Get(sessionID)
			switch {
			case tc.registered && err != nil:
				t.Fatalf("%s's interest after the reaper: %v, want it kept: the session is registered and only the copy behind answers it absent", sessionID, err)
			case !tc.registered && !errors.Is(err, nats.ErrKeyNotFound):
				t.Fatalf("%s's interest after the reaper: %v, want it deleted", sessionID, err)
			}
		})
	}
}

// The role claim reaper keeps a claim whose holder's session the session bucket holds, even where
// the copy of the bucket a direct get reaches has not applied that session's registration: the
// reaper asks the stream's leader whether the holder is gone (roleHolderMayBeLive,
// sessionMayBeLive). Before, the copy's "no record" made the reaper release a live holder's role.
// The claim is aged past the live window and the restart grace, so the holder's session is all
// that keeps it. Control: the claim of a holder whose session the bucket has no record of is
// released through the same copy.
func TestTheRoleReaperKeepsTheClaimOfAHolderACopyBehindAnswersAbsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		registered bool
	}{
		{"a registered holder the copy answers absent", true},
		{"control: a holder whose session the bucket has no record of", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimant, observer := trailingListeners(t)
			const (
				holder = "ses_reap_role_copy_behind"
				role   = "reap-copy-behind"
			)
			claimRole(t, claimant, holder, role)
			if !tc.registered {
				if err := claimant.sessions.Delete(context.Background(), holder); err != nil {
					t.Fatalf("end %s's session: %v", holder, err)
				}
			}
			ageRoleClaim(t, claimant, role, 2*observer.sessions.TTL())
			behindForSession(t, observer, holder)

			live := func(sessionID string) bool {
				return roleHolderMayBeLive(logging.New("test"), observer.sessions, sessionID)
			}
			sweep, err := observer.registry.ReapRoleClaims(context.Background(), live, observer.sessions.TTL(), observer.sessions.TTL())
			want := map[bool]int{true: 0, false: 1}[tc.registered]
			if err != nil || sweep.Reaped != want {
				t.Errorf("reap role claims = %+v, %v; want %d released", sweep, err, want)
			}
			wantHolder := map[bool]string{true: holder, false: ""}[tc.registered]
			if got, err := claimant.registry.RoleHolder(role); err != nil || got != wantHolder {
				t.Fatalf("%s's holder after the reaper = %q, %v; want %q", role, got, err, wantHolder)
			}
		})
	}
}
