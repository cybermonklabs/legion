// What the send route's 202 answers for a send marked with a role whose claim names another
// session: the copy the role's kept lane holds, as the publish route's kept answer gives it.

package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// Dispatch's send marked with a role nobody claims is kept for the role's next holder and
// answered 202 with the copy the role's kept lane holds (keepRoleSend). Its repeat under the same
// idempotency_key, asking for another reply, is answered with the first send's kept copy, with
// duplicate and that copy's expects_reply, as the publish route's kept answer is (roleSend.kept).
// A copy an agent kept on the lane under the same dedupe key is another sender's: the 202 gives
// its event_id and times alone, with duplicate, and none of what the agent wrote. The control is
// the first send, answered its own kept copy whole with no duplicate and its own expects_reply.
func TestARoleMarkedSendKeptForTheRoleAnswersTheKeptCopy(t *testing.T) {
	rig := newSenderLimitRig(t)
	const principal = "system:serviceaccount:dispatch:dispatch"
	useDispatchPrincipal(rig.deps, principal)
	target := "ses_marked_send_target_" + id.New()
	rig.live(t, target, nil)
	// send is Dispatch's send to target marked with role, under key, asking for reply.
	send := func(t *testing.T, role, key, words, reply string) heldAnswer {
		t.Helper()
		body := messageJSON(t, map[string]any{
			"target_session": target, "source": "dispatch", "message": words, "idempotency_key": key,
			"role": role, "expects_reply": reply,
		}, nil)
		request := postJSON("/v1/messages/send", body)
		request = request.WithContext(withVerifiedPrincipal(request.Context(), principal))
		return held(t, serveLimited(t, rig.mux, rig.harness.logs.String, request))
	}
	expectsReply := func(t *testing.T, answer heldAnswer) string {
		t.Helper()
		answer.requireStatus(t, "the marked send", http.StatusAccepted)
		var fields map[string]any
		if err := json.Unmarshal([]byte(answer.raw), &fields); err != nil {
			t.Fatalf("decode %s: %v", answer.raw, err)
		}
		reply, _ := fields["expects_reply"].(string)
		return reply
	}

	t.Run("control: the first marked send answers its own kept copy whole with no duplicate", func(t *testing.T) {
		role := "marked-send-first-" + id.New()
		first := send(t, role, uuid.NewString(), "dispatch's first words", "optional")
		first.requireStatus(t, "the first marked send", http.StatusAccepted)
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		if !reflect.DeepEqual(first.envelope, kept) || first.Duplicate || !first.Kept {
			t.Fatalf("the first marked send answered %s, want its own kept copy %+v whole, kept, with no duplicate", first.raw, kept)
		}
		if got := expectsReply(t, first); got != "optional" {
			t.Fatalf("the first marked send answered expects_reply %q, want its own %q", got, "optional")
		}
	})

	t.Run("case: a repeat asking for another reply answers the first send's kept copy with duplicate and its expects_reply", func(t *testing.T) {
		role, key := "marked-send-repeat-"+id.New(), uuid.NewString()
		send(t, role, key, "dispatch's words", "optional").requireStatus(t, "the first marked send", http.StatusAccepted)
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		repeat := send(t, role, key, "dispatch's words again", "required")
		repeat.requireStatus(t, "the repeated marked send", http.StatusAccepted)
		if !reflect.DeepEqual(repeat.envelope, kept) || !repeat.Duplicate {
			t.Fatalf("the repeated marked send answered %s, want the first send's kept copy %+v whole with duplicate", repeat.raw, kept)
		}
		if got := expectsReply(t, repeat); got != "optional" {
			t.Fatalf("the repeated marked send answered expects_reply %q, want the kept copy's %q", got, "optional")
		}
		if repeat.line["event_id"] != kept.EventID {
			t.Fatalf("the repeated marked send wrote the line %v, want the kept copy's event_id %s", repeat.line, kept.EventID)
		}
	})

	t.Run("case: an agent's copy kept under the send's key is answered with its ids, times and expects_reply alone, with duplicate", func(t *testing.T) {
		role, key := "marked-send-other-"+id.New(), uuid.NewString()
		words := "alice's own words " + id.New()
		planted := held(t, serveLimited(t, rig.mux, rig.harness.logs.String, postJSON("/v1/messages/publish", messageJSON(t, map[string]any{
			"topic": contracts.RoleTopicPrefix + role, "source_session": "ses_marked_send_alice", "message": words, "expects_reply": "optional",
			"dedupe_key": contracts.AgentSendDedupeKey(target, key),
		}, nil))))
		planted.requireStatus(t, "the agent's publish to the role nobody claims", http.StatusAccepted)
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		answer := send(t, role, key, "dispatch's words", "required")
		answer.requireStatus(t, "the marked send under the agent's key", http.StatusAccepted)
		got := answer.envelope
		if !answer.Duplicate || got.EventID != kept.EventID || got.IssuedAt != kept.IssuedAt ||
			!reflect.DeepEqual(got.KeptAt, kept.KeptAt) {
			t.Fatalf("the marked send under the agent's key answered %s, want duplicate and the kept copy's event_id and times: %+v", answer.raw, kept)
		}
		if got.Payload != "" || got.PayloadSummary != "" || got.Source != "" || got.SourceSession != "" ||
			got.Sender != nil || strings.Contains(answer.raw, words) {
			t.Fatalf("the marked send under the agent's key answered %s, which carries what the agent wrote or who it is", answer.raw)
		}
		if got := expectsReply(t, answer); got != "optional" {
			t.Fatalf("the marked send under the agent's key answered expects_reply %q, want the kept copy's %q", got, "optional")
		}
		if answer.line["event_id"] != kept.EventID || answer.line["source"] != "dispatch" {
			t.Fatalf("the marked send under the agent's key wrote the line %v, want the copy's event_id %s and Dispatch as the asker", answer.line, kept.EventID)
		}
	})
}
