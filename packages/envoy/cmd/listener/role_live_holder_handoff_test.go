package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/session"
)

// A send Dispatch marks with a finished Legion role, aimed at the role's stopped newest session,
// is the role's message, and while another session holds that role live the message is that
// holder's: a person's resumed or forked session that took the role over reads it. So the send
// route keeps it on the role's kept lane for the live holder (keepRoleSend, #4387's keep, which
// hands the lane to whoever holds the role now) and hands nothing to the stopped session's daemon:
// 202 kept, and no hand-off. The hand-off is for a role that no one holds live
// (TestADispatchSendMarkedWithAStoppedRoleIsHandedToItsDaemon); a live holder reads the message
// itself.
func TestAMarkedSendToAFinishedRoleThatAnotherSessionHoldsLiveIsKeptForThatHolder(t *testing.T) {
	rig := newKnownSessionRig(t)
	const principal = "system:serviceaccount:dispatch:example-dispatch"
	useDispatchPrincipal(rig.deps, principal)
	const agent, holder = "ses_finished_role_stopped", "ses_finished_role_taken_over"
	finished := legionAgentEntry(agent, "INFRA-206", "reviewer")
	role := finished.RoleToken
	withLegionAgent(t, rig.deps, finished)
	rig.register(t, agent, nil)
	rig.stop(t, agent)
	rig.record(t, agent)
	rig.register(t, holder, nil)
	claimRole(t, rig.deps, holder, role)
	client := rig.harness.client

	got := sendMarkedByDispatch(t, rig.mux, principal, agent, role, "what did the review find?")
	if count, _ := handoffs(t, client, agent); count != 0 {
		t.Errorf("the stream holds %d hand-offs for %s, want none: %s holds %s live, so the message is its", count, agent, holder, role)
	}
	if got.status != http.StatusAccepted || !got.Kept {
		t.Fatalf("a send marked with %s to its stopped session %s while %s holds the role live: status %d, body %s; want 202 kept for the holder",
			role, agent, holder, got.status, got.raw)
	}
	waitForKeptRoleMessage(t, client, role, got.EventID)
}

// The hand-off waits only on a live holder: a session whose claim of the finished role stands
// while its own registration is gone reads Lapsed (roles.ResolveHolder), and a lapsed holder is
// not answering, so the marked send is handed to the stopped session's daemon exactly as for a
// role no one holds - 200 to that session, one hand-off on the role token, nothing kept.
func TestAMarkedSendToAFinishedRoleWhoseOtherHolderLapsedIsHandedToItsDaemon(t *testing.T) {
	rig := newKnownSessionRig(t)
	const principal = "system:serviceaccount:dispatch:example-dispatch"
	useDispatchPrincipal(rig.deps, principal)
	const agent, holder = "ses_finished_role_stopped_lapsed", "ses_finished_role_lapsed_holder"
	finished := legionAgentEntry(agent, "INFRA-206", "reviewer")
	role := finished.RoleToken
	withLegionAgent(t, rig.deps, finished)
	rig.register(t, agent, nil)
	rig.stop(t, agent)
	rig.record(t, agent)
	rig.register(t, holder, nil)
	claimRole(t, rig.deps, holder, role)
	if err := rig.deps.sessions.Delete(context.Background(), holder); err != nil {
		t.Fatalf("deregister %s: %v", holder, err)
	}
	client := rig.harness.client

	got := sendMarkedByDispatch(t, rig.mux, principal, agent, role, "what did the review find?")
	if got.status != http.StatusOK || got.Recipient != agent || got.Kept {
		t.Fatalf("a send marked with %s to its stopped session %s while %s's claim of the role has lapsed: status %d, body %s; want 200 handed off to %s, nothing kept",
			role, agent, holder, got.status, got.raw, agent)
	}
	if count, _ := handoffs(t, client, agent); count != 1 {
		t.Fatalf("the stream holds %d hand-offs for %s, want the one the marked send made: %s is not live", count, agent, holder)
	}
	assertNothingKept(t, client, role)
}

// A lookup of the finished role's other holder that does not answer decides nothing about that
// holder: it may be live, so the marked send is neither handed to the stopped session's daemon
// nor kept. It is answered as a failed read of the role's claim is (refuseRoleClaimRead): the
// bus-unavailable 503, with no hand-off and nothing on the role's kept lane, so the sender sends
// again once the read answers.
func TestAMarkedSendToAFinishedRoleWhoseHolderLookupFailsIsRefusedAndNotHandedOff(t *testing.T) {
	rig := newKnownSessionRig(t)
	const principal = "system:serviceaccount:dispatch:example-dispatch"
	useDispatchPrincipal(rig.deps, principal)
	const agent, holder = "ses_finished_role_stopped_unread", "ses_finished_role_unread_holder"
	finished := legionAgentEntry(agent, "INFRA-206", "reviewer")
	role := finished.RoleToken
	withLegionAgent(t, rig.deps, finished)
	rig.register(t, agent, nil)
	rig.stop(t, agent)
	rig.record(t, agent)
	// The holder's record stands in the bucket alone, so the resolution reads the bucket for it,
	// and that read fails. Only the holder's read fails: a read of another session the listener
	// makes meanwhile (a consumer repair's read of the stopped agent) answers, and cannot take the
	// failure meant for the holder (a loaded run of the package on 2026-11-23: that repair read
	// took it, and the send was answered 202).
	claimRoleBeyondTheSessionCache(t, rig.harness, holder, role)
	failed := failTheNextSessionReadOf(t, rig.harness.sessions, holder)
	client := rig.harness.client

	got := sendMarkedByDispatch(t, rig.mux, principal, agent, role, "what did the review find?")
	if !failed.Load() {
		t.Fatalf("no read of %s in the session bucket failed, so this send did not meet a failed lookup of it", holder)
	}
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("a send marked with %s to its stopped session %s while the lookup of %s fails: status %d, body %s; want 503",
			role, agent, holder, got.status, got.raw)
	}
	if count, _ := handoffs(t, client, agent); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: the lookup of %s did not answer", count, agent, holder)
	}
	assertNothingKept(t, client, role)
}

// failFirstGetOfKV is failFirstGetKV for one key: its first read of key does not answer, and every
// other read answers.
type failFirstGetOfKV struct {
	natsgo.KeyValue
	key    string
	failed *atomic.Bool
}

func (kv failFirstGetOfKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key && kv.failed.CompareAndSwap(false, true) {
		return nil, errors.New("the session bucket's read did not answer")
	}
	return kv.KeyValue.Get(key)
}

// failTheNextSessionReadOf is failTheNextSessionRead for the one session key: the next bucket read
// of key fails, and the flag it returns reports whether a read of key took the failure.
func failTheNextSessionReadOf(t *testing.T, sessions *session.SessionRegistry, key string) *atomic.Bool {
	t.Helper()
	failed := &atomic.Bool{}
	swapSessionBucket(t, sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
		return failFirstGetOfKV{KeyValue: kv, key: key, failed: failed}
	})
	return failed
}
