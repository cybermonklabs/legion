package main

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// keptRoleLeftLine is the WARN a handover writes where a read failed and no claim record names a
// holder to read again for (handOverKeptRole's retry).
const keptRoleLeftLine = "listener kept role drain left the lane to the role's next claim"

// requireLeftLines requires logs to hold want lines keptRoleLeftLine for role and, where it wants
// one, that it is at WARN, carries fields and has an error carrying cause.
func requireLeftLines(t *testing.T, logs, role string, want int, fields map[string]any, cause string) {
	t.Helper()
	lines := linesIn(logs, keptRoleLeftLine, map[string]any{"role": role})
	if len(lines) != want {
		t.Fatalf("the handover logged %d lines %q for role %s, want %d:\n%s", len(lines), keptRoleLeftLine, role, want, logs)
	}
	if want == 0 {
		return
	}
	assertRecordFields(t, lines[0], map[string]any{"level": "WARN"})
	assertRecordFields(t, lines[0], fields)
	assertRecordErrorCarries(t, lines[0], cause)
}

// A resolution of the role's holder that failed still walks the lane with no holder, and where
// that walk drains the lane the handover ends there, with no read of the role's claim record
// (handOverKeptRole). Here the copy's hour has ended, so the walk ends it with a dead letter that
// says no session held the role, while the claim names a live holder. The handover used to end
// with nothing logged above Debug, so a test that read that dead letter could print no cause; it
// now says at WARN that it walked with no holder, naming the resolution that failed, the window it
// was given and its error. The control drains the same lane with the resolution answering the
// live holder, and writes no such line.
func TestAHandoverWhoseHolderLookupFailedSaysSoWhereItsWalkDrainedTheLane(t *testing.T) {
	t.Parallel()
	const walked = "listener kept role drain walked the role's copies with no holder because the holder lookup failed"
	for _, test := range []struct {
		name       string
		fail       bool
		wantWalked int
	}{
		{name: "the holder lookup fails", fail: true, wantWalked: 1},
		{name: "control: the holder lookup answers the live holder"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newListenerDeliveryHarness(t)
			const (
				role   = "kept-holder-unresolved-drained"
				holder = "ses_kept_holder_unresolved_drained"
				sender = "ses_kept_holder_unresolved_sender"
			)
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-00")
			item.SourceSession = sender
			held, err := keepRoleMessage(harness.config, role, item)
			if err != nil {
				t.Fatalf("keep the copy: %v; the listener log says why:\n%s", err, harness.logs.String())
			}
			claimRoleBeyondTheSessionCache(t, harness, holder, role)
			// Every read of the session bucket fails from here, so the handover's own resolution
			// of the holder fails whichever other reader of the registry reads the bucket first.
			failure := errors.New("the session bucket's read did not answer")
			if test.fail {
				swapSessionBucket(t, harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
					return bucketThatFails{KeyValue: kv, err: failure}
				})
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			// The copy's hour ended a minute ago and its sender is owed a dead letter for it.
			pastItsHour := func() time.Time { return time.Now().Add(roleKeepWindow + time.Minute) }
			handover, err := handOverKeptRole(ctx, harness.config, role, pastItsHour)
			if err != nil {
				t.Fatalf("hand over role %s's kept lane: %v\n%s", role, err, harness.logs.String())
			}
			if !test.fail && handover.resolved != roles.Live {
				t.Fatalf("the control's handover resolved role %s's holder as %v, want %v: its walk did not have the holder:\n%s",
					role, handover.resolved, roles.Live, harness.logs.String())
			}
			stored, err := harness.client.ReadKeptRoleCursor(ctx, role)
			if err != nil {
				t.Fatalf("read role %s's drain cursor: %v", role, err)
			}
			if stored.Cursor.Kept < held.sequence {
				t.Fatalf("role %s's drain cursor is at %d, want it past the copy at %d: the walk did not drain the lane:\n%s",
					role, stored.Cursor.Kept, held.sequence, harness.logs.String())
			}

			lines := linesIn(harness.logs.String(), walked, map[string]any{"role": role})
			if len(lines) != test.wantWalked {
				t.Fatalf("the handover logged %d lines %q for role %s, want %d:\n%s", len(lines), walked, role, test.wantWalked, harness.logs.String())
			}
			if test.wantWalked == 0 {
				return
			}
			assertRecordFields(t, lines[0], map[string]any{
				"level": "WARN",
				"read":  "listener kept role message could not resolve the role's holder",
				"bound": float64(roleKeepHolderWindow),
			})
			assertRecordErrorCarries(t, lines[0], failure.Error())
		})
	}
}
