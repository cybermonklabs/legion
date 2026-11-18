// What the role publish route answers a caller's repeat under the same idempotency_key: the
// stream keeps the first forward on the holder's agent subject and stores no second one, so the
// 200 is that stored forward, with duplicate and without the forward mark on its dedupe key, and
// never the new envelope the route built for the repeat.

package main

import (
	"encoding/json"
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
// A repeat's forward the stream refuses while the first one under its id is in process (as
// nats-server's 10158 refusal, which a forward's wait returns as ErrForwardNotStored wrapping it)
// is tried again, as any forward that failed is, and the stream's next answer, the duplicate
// naming the first forward, is the copy the 200 carries.
//
// The control is the same repeat to a role nobody claims: its 202 kept answer is the copy the
// role's kept lane holds, which the route already answered before this change (roleSend.kept).
func TestARolePublishRepeatedUnderOneIdempotencyKeyAnswersTheStoredCopy(t *testing.T) {
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
				rig.deps.confirmed.Clear(publishDedupePrefix+row.key, holder)
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
		rig.deps.confirmed.Clear(publishDedupePrefix+key, holder)
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

	t.Run("control: a repeat to a role nobody claims answers 202 with the kept copy", func(t *testing.T) {
		role := "role-repeat-unclaimed-" + id.New()
		sender := "ses_role_repeat_unclaimed_sender_" + id.New()
		key := map[string]any{"idempotency_key": uuid.NewString()}
		first := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role, key))
		repeat := held(t, rig.publishFrom(t, sender, contracts.RoleTopicPrefix+role,
			map[string]any{"idempotency_key": key["idempotency_key"], "message": "the repeat's own body"}))
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		for _, answer := range []struct {
			what      string
			duplicate bool
			heldAnswer
		}{{"the first publish to the unclaimed role", false, first}, {"the repeated publish to the unclaimed role", true, repeat}} {
			answer.requireStatus(t, answer.what, http.StatusAccepted)
			if !reflect.DeepEqual(answer.envelope, kept) {
				t.Fatalf("%s answered %+v, want the kept copy field for field: %+v", answer.what, answer.envelope, kept)
			}
			var fields struct {
				Kept bool `json:"kept"`
			}
			if err := json.Unmarshal([]byte(answer.raw), &fields); err != nil || !fields.Kept || answer.Duplicate != answer.duplicate {
				t.Fatalf("%s answered %s (%v), want kept: true and duplicate %t", answer.what, answer.raw, err, answer.duplicate)
			}
		}
	})
}
