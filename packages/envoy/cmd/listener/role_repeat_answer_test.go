// What the role publish route answers a caller's repeat under the same idempotency_key: the
// stream keeps the first forward on the holder's agent subject and stores no second one, so the
// 200 is that stored forward, with duplicate and without the forward mark on its dedupe key, and
// never the new envelope the route built for the repeat.

package main

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// TestARolePublishRepeatedUnderOneIdempotencyKeyAnswersTheStoredCopy is check
// c-role-publish-repeat-stored-copy. A session publishes one message to a role whose holder is
// live under idempotency_key K, then publishes it again under K with another body. The holder's
// agent subject holds one forward, and the repeat's 200 carries duplicate and that forward's
// envelope, its body the first one and its dedupe key without roleForwardDedupePrefix, both where
// the repeat is forwarded again (the route's confirm record cleared, as another listener has none)
// and the stream answers it a duplicate, and where the route's confirm record answers it with no
// forward at all. A key that is no UUID (retry-1) is a repeat to the role route too, since the
// forward's Nats-Msg-Id is made of the key whatever its form (bus.ForwardMsgID). The charge is the
// role send's own, unchanged: a forward the stream confirmed costs one, a duplicate among them,
// and an answer from the confirm record forwards nothing and costs nothing.
//
// A role forward the stream refuses while an earlier forward under its id is still in process
// (nats-server's 10158) is sent again inside the forward's own wait (startRoleForward, through
// publishPastInProcess), as publish_in_process_test.go shows. This test's forward seam replaces
// the whole forward and fails the repeat's first one with the 10158 answer as a plain failed
// forward (ErrForwardNotStored wrapping it, not bus.ErrDuplicateInProcess), so the route forwards
// again, and the stream's next answer, the duplicate naming the first forward, is the copy the 200
// carries.
//
// The control is the same repeat to a role nobody claims: its 202 kept answer is the copy the
// role's kept lane holds, which the route already answered before this change (roleSend.kept).
func TestARolePublishRepeatedUnderOneIdempotencyKeyAnswersTheStoredCopy(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)
	rig.limit(600, 20)

	forwards := rig.interceptForwards(func(_ int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
		return forward()
	})

	for _, row := range []struct {
		name    string
		key     string
		forward bool
	}{
		{"case: a repeat forwarded again answers the stored forward with duplicate", uuid.NewString(), true},
		{"case: a repeat the route's confirm record answers gives the stored forward with duplicate, forwarding nothing", uuid.NewString(), false},
		{"case: a repeat under a key of another form, forwarded again, answers the stored forward with duplicate", "retry-1", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			role := "role-repeat-" + id.New()
			holder := "ses_role_repeat_holder_" + id.New()
			sender := "ses_role_repeat_sender_" + id.New()
			claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
			first := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": row.key}))
			first.requireHeld(t, "the first role publish", heldForward(t, rig.harness.client, holder), false)
			first.requireNamed(t, "the first role publish", sender)
			if row.forward {
				rig.deps.confirmed.Clear(bus.PublishDedupePrefix+row.key, holder)
			}
			before := *forwards
			repeat := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role,
				map[string]any{"idempotency_key": row.key, "message": "the repeat's own body"}))
			repeat.requireHeld(t, "the repeated role publish", heldForward(t, rig.harness.client, holder), true)
			if repeat.Holder != holder {
				t.Fatalf("the repeated role publish named holder %q, want %s; body %s", repeat.Holder, holder, repeat.raw)
			}
			made := *forwards - before
			if row.forward {
				if made != 1 {
					t.Fatalf("the repeat made %d forwards, want the one the stream answered a duplicate", made)
				}
				repeat.requireNamed(t, "the repeat forwarded again, which the stream confirmed", sender)
			} else {
				if made != 0 {
					t.Fatalf("the repeat the confirm record answers made %d forwards, want none", made)
				}
				repeat.requireUnnamed(t, "the repeat the confirm record answers")
			}
		})
	}

	t.Run("case: a send whose own first forward stored the copy, its acknowledgement lost, answers that copy with no duplicate", func(t *testing.T) {
		own := newSenderLimitRig(t)
		role := "role-own-forward-" + id.New()
		holder := "ses_role_own_forward_holder_" + id.New()
		claimRoleFor(t, own.deps.registry, own.deps.sessions, holder, role, nil)
		made := own.loseFirstForwardAck(func() {})
		answer := held(t, own.publishFrom(t, "ses_role_own_forward_sender", contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": uuid.NewString()}))
		if *made != 2 {
			t.Fatalf("the send made %d forwards, want 2: the first stored with its acknowledgement lost, the second a duplicate of it", *made)
		}
		answer.requireHeld(t, "a role send whose own first forward stored the copy", heldForward(t, own.harness.client, holder), false)
	})

	t.Run("case: a repeat asking for another reply answers the stored forward's expects_reply", func(t *testing.T) {
		role := "role-repeat-expects-" + id.New()
		holder := "ses_role_repeat_expects_" + id.New()
		sender := "ses_role_repeat_expects_sender_" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		key := uuid.NewString()
		if got := expectsReplyOf(t, "the first role publish", rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": key, "expects_reply": "none"})); got != `"none"` {
			t.Fatalf("the first role publish answered expects_reply %s, want the request's own \"none\"", got)
		}
		repeat := rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": key, "expects_reply": "required"})
		if got := expectsReplyOf(t, "the repeat asking for a required reply", repeat); !repeat.Duplicate || got != "false" {
			t.Fatalf("the role repeat asking for a required reply answered expects_reply %s and duplicate %t, want the stored forward's false and duplicate", got, repeat.Duplicate)
		}
	})

	t.Run("case: a repeat whose forward is refused while the first is in process is tried again and answers the stored forward with duplicate", func(t *testing.T) {
		role := "role-repeat-in-process-" + id.New()
		holder := "ses_role_repeat_in_process_" + id.New()
		sender := "ses_role_repeat_in_process_sender_" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		key := uuid.NewString()
		first := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": key}))
		first.requireHeld(t, "the first role publish", heldForward(t, rig.harness.client, holder), false)
		rig.deps.confirmed.Clear(bus.PublishDedupePrefix+key, holder)
		saved := rig.deps.delivery
		t.Cleanup(func() { rig.deps.delivery = saved })
		inProcess := fmt.Errorf("%w: %w", bus.ErrForwardNotStored,
			&natsgo.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})
		made := rig.interceptForwards(func(n int, forward func() (func() (bus.StoredAck, error), error)) (func() (bus.StoredAck, error), error) {
			if n == 1 {
				return func() (bus.StoredAck, error) { return bus.StoredAck{}, inProcess }, nil
			}
			return forward()
		})
		repeat := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role,
			map[string]any{"idempotency_key": key, "message": "the repeat's own body"}))
		repeat.requireHeld(t, "the repeat whose first forward was refused in process", heldForward(t, rig.harness.client, holder), true)
		if *made != 2 {
			t.Fatalf("the repeat made %d forwards, want 2: the one refused in process and the one the stream answered a duplicate", *made)
		}
	})

	// keptRole publishes twice under one key to a role nobody claims, the second time with a body of
	// its own, and returns both 202 answers and the copy the role's kept lane holds.
	keptRole := func(t *testing.T) (first, repeat heldAnswer, kept contracts.Envelope) {
		t.Helper()
		role := "role-repeat-unclaimed-" + id.New()
		sender := "ses_role_repeat_unclaimed_sender_" + id.New()
		key := map[string]any{"idempotency_key": uuid.NewString()}
		first = held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key))
		repeat = held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role,
			map[string]any{"idempotency_key": key["idempotency_key"], "message": "the repeat's own body"}))
		return first, repeat, heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
	}
	// requireKept fails unless answer is a 202 with kept: true whose envelope is kept, field for
	// field, and whose duplicate is duplicate.
	requireKept := func(t *testing.T, what string, answer heldAnswer, kept contracts.Envelope, duplicate bool) {
		t.Helper()
		answer.requireStatus(t, what, http.StatusAccepted)
		if !reflect.DeepEqual(answer.envelope, kept) {
			t.Fatalf("%s answered %+v, want the kept copy field for field: %+v", what, answer.envelope, kept)
		}
		if !answer.Kept || answer.Duplicate != duplicate {
			t.Fatalf("%s answered %s, want kept: true and duplicate %t", what, answer.raw, duplicate)
		}
	}

	// The keep already answered a repeat with its kept copy before this change (roleSend.kept), so
	// this row holds at main and with the change.
	t.Run("control: a publish and its repeat to a role nobody claims answer 202 with the kept copy", func(t *testing.T) {
		first, repeat, kept := keptRole(t)
		requireKept(t, "the first publish to the unclaimed role", first, kept, false)
		repeat.requireStatus(t, "the repeated publish to the unclaimed role", http.StatusAccepted)
		if repeat.envelope.EventID != kept.EventID || repeat.envelope.PayloadSummary != kept.PayloadSummary {
			t.Fatalf("the repeated publish to the unclaimed role answered %s, want the kept copy %+v", repeat.raw, kept)
		}
	})

	t.Run("case: a repeat to a role nobody claims answers 202 with the kept copy and duplicate", func(t *testing.T) {
		_, repeat, kept := keptRole(t)
		requireKept(t, "the repeated publish to the unclaimed role", repeat, kept, true)
	})
}

// A session publishes under K to a role another session holds, and the stream stores the forward
// on that holder's agent subject. The sender then takes the role and repeats under K: the repeat
// is an echo (the sender holds the role), which forwards nothing, and its 200 is the forward the
// stream holds, read back at the sequence the route recorded for it, with duplicate and the
// holder the sender is now: never the new envelope the route built for the repeat, whose event_id
// no stored message carries. The control is the holder's first publish to its own role, which no
// forward stored: it answers its own envelope with no duplicate, as an echo always has.
//
// A second request under K that the route's record answers as a repeat of that forward, with no
// forward of its own (another sender's to the same role, or the same sender's to another role the
// same holder holds), is a request of its own too: once its sender takes that role and repeats,
// the echo answers the same stored forward, its key-taken answer for another sender whose client
// reads it and whole with duplicate for the forward's own. Two more cases clear the route's record
// of K before the second request, which is then forwarded again, not answered from the record,
// and recorded through the stream's confirmation, as a forward always was: the echo answers the
// same stored forward.
func TestARoleRepeatFromTheSenderThatTookTheRoleAnswersTheStoredForward(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)

	t.Run("control: a holder's first publish to its own role answers its own envelope with no duplicate", func(t *testing.T) {
		role := "role-echo-first-" + id.New()
		holder := "ses_role_echo_first_" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, holder, role, nil)
		echo := held(t, rig.publishFrom(t, holder, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": uuid.NewString()}))
		echo.requireStatus(t, "the holder's publish to its own role", http.StatusOK)
		if echo.Duplicate || echo.envelope.SourceSession != holder || echo.Holder != holder || echo.line["event_id"] != echo.envelope.EventID {
			t.Fatalf("the holder's publish to its own role answered %s, want its own envelope, holder %s, no duplicate", echo.raw, holder)
		}
	})

	t.Run("case: the sender's repeat after it took the role answers the stored forward with duplicate", func(t *testing.T) {
		role := "role-echo-repeat-" + id.New()
		bob := "ses_role_echo_bob_" + id.New()
		alice := "ses_role_echo_alice_" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, bob, role, nil)
		key := uuid.NewString()
		first := held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+role, map[string]any{"idempotency_key": key}))
		stored := heldForward(t, rig.harness.client, bob)
		first.requireHeld(t, "Alice's publish to Bob's role", stored, false)
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, alice, role, nil)
		repeat := held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+role,
			map[string]any{"idempotency_key": key, "message": "the repeat's own body"}))
		repeat.requireHeld(t, "Alice's repeat after she took the role", stored, true)
		if repeat.Holder != alice {
			t.Fatalf("Alice's repeat after she took the role named holder %q, want %s; body %s", repeat.Holder, alice, repeat.raw)
		}
		heldForward(t, rig.harness.client, bob)
	})

	// The record is of the request, not the key alone: the same sender's forward of the same key to
	// another role is another request, and the repeat to the first role answers that role's forward.
	t.Run("case: the sender's repeat to a role it took answers that role's forward, not another role's under the same key", func(t *testing.T) {
		roleA, roleB := "role-echo-cross-a-"+id.New(), "role-echo-cross-b-"+id.New()
		bob, carol := "ses_role_echo_cross_bob_"+id.New(), "ses_role_echo_cross_carol_"+id.New()
		alice := "ses_role_echo_cross_alice_" + id.New()
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, bob, roleA, nil)
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, carol, roleB, nil)
		key := uuid.NewString()
		held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+roleA, map[string]any{"idempotency_key": key})).
			requireHeld(t, "Alice's publish to role A", heldForward(t, rig.harness.client, bob), false)
		held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+roleB, map[string]any{"idempotency_key": key, "message": "role B's different message"})).
			requireHeld(t, "Alice's publish to role B under the same key", heldForward(t, rig.harness.client, carol), false)
		claimRoleFor(t, rig.deps.registry, rig.deps.sessions, alice, roleA, nil)
		repeat := held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+roleA, map[string]any{"idempotency_key": key}))
		repeat.requireHeld(t, "Alice's repeat to role A after she took it", heldForward(t, rig.harness.client, bob), true)
	})

	for _, row := range []struct {
		name string
		// other is whether the second request is another sender's to role A, rather than the
		// first sender's to role B; forward is whether the route's record of the key is cleared
		// first, so the second request is forwarded again rather than answered from the record.
		other, forward bool
	}{
		{"case: another sender's repeat the record answered, repeated after it took the role, learns only that the key is taken", true, false},
		{"case: the sender's request to a second role the record answered, repeated after it took that role, answers the stored forward with duplicate", false, false},
		{"case: another sender's repeat forwarded again, not answered from the record, repeated after it took the role, learns only that the key is taken", true, true},
		{"case: the sender's request to a second role forwarded again, not answered from the record, repeated after it took that role, answers the stored forward with duplicate", false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			roleA, roleB := "role-echo-recorded-a-"+id.New(), "role-echo-recorded-b-"+id.New()
			bob := "ses_role_echo_recorded_bob_" + id.New()
			alice := "ses_role_echo_recorded_alice_" + id.New()
			claimRoleFor(t, rig.deps.registry, rig.deps.sessions, bob, roleA, nil)
			claimRoleFor(t, rig.deps.registry, rig.deps.sessions, bob, roleB, nil)
			key := uuid.NewString()
			words := "alice's own words " + id.New()
			held(t, rig.publishFrom(t, alice, contracts.RoleTopicPrefix+roleA, map[string]any{"idempotency_key": key, "message": words})).
				requireHeld(t, "Alice's publish to role A", heldForward(t, rig.harness.client, bob), false)
			stored := heldForward(t, rig.harness.client, bob)
			second, role, who := alice, roleB, "Alice's request to role B"
			if row.other {
				second, role, who = "ses_role_echo_recorded_mallory_"+id.New(), roleA, "Mallory's request to role A"
			}
			// answers fails unless answer is what second, whose client reads the key-taken
			// answer, gets for the stored forward: the key taken for another sender, the whole
			// forward with duplicate for its own.
			answers := func(what string, answer heldAnswer) {
				t.Helper()
				if row.other {
					answer.requireKeyTaken(t, what, http.StatusOK, stored, words)
					return
				}
				answer.requireHeld(t, what, stored, true)
			}
			if row.forward {
				rig.deps.confirmed.Clear(bus.PublishDedupePrefix+key, bob)
			}
			answers(who, held(t, rig.publishFromReading(t, second, contracts.RoleTopicPrefix+role,
				map[string]any{"idempotency_key": key, "message": "the second request's own body"})))
			claimRoleFor(t, rig.deps.registry, rig.deps.sessions, second, role, nil)
			answers(who+", repeated after it took the role", held(t, rig.publishFromReading(t, second, contracts.RoleTopicPrefix+role,
				map[string]any{"idempotency_key": key, "message": "the repeat's own body"})))
			heldForward(t, rig.harness.client, bob)
		})
	}
}
