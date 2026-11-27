// What the send route's 202 answers for a send marked with a role whose claim names another
// session: the copy the role's kept lane holds, as the publish route's kept answer gives it.

package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// Dispatch's send marked with a role nobody claims is kept for the role's next holder and
// answered 202 with the copy the role's kept lane holds (keepRoleSend). Dispatch names the
// key-taken answer as one it reads (listenerwire.ReadsHeader), and its sends name no session, so
// no later request can prove a copy its own: its repeat under the same idempotency_key, asking
// for another reply, is answered with the first send's kept copy's ids, times and expects_reply
// alone, with duplicate, as the publish route's kept answer gives another sender's copy
// (roleSend.kept). A copy an agent kept on the lane under the same dedupe key is answered the
// same way, with none of what the agent wrote. A marked send that does not name the header gets
// the envelope its own request built, kept, with duplicate. The control is the first send,
// answered its own kept copy whole with no duplicate and its own expects_reply.
func TestARoleMarkedSendKeptForTheRoleAnswersTheKeptCopy(t *testing.T) {
	t.Parallel()
	rig := newSenderLimitRig(t)
	const principal = "system:serviceaccount:dispatch:dispatch"
	useDispatchPrincipal(rig.deps, principal)
	target := "ses_marked_send_target_" + id.New()
	rig.live(t, target, nil)
	// sendAs is Dispatch's send to target marked with role, under key, asking for reply, naming
	// the key-taken answer as one it reads where reads is set; send is Dispatch's own, which does.
	sendAs := func(t *testing.T, reads bool, role, key, words, reply string) heldAnswer {
		t.Helper()
		body := messageJSON(t, map[string]any{
			"target_session": target, "source": "dispatch", "message": words, "idempotency_key": key,
			"role": role, "expects_reply": reply,
		}, nil)
		request := postJSON("/v1/messages/send", body)
		request = request.WithContext(withVerifiedPrincipal(request.Context(), principal))
		if reads {
			request = readingKeyTaken(request)
		}
		return held(t, serveLimited(t, rig.mux, rig.harness.logs.String, request))
	}
	send := func(t *testing.T, role, key, words, reply string) heldAnswer {
		t.Helper()
		return sendAs(t, true, role, key, words, reply)
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

	t.Run("case: a repeat asking for another reply answers the first send's kept copy's ids, times and expects_reply alone, with duplicate", func(t *testing.T) {
		role, key := "marked-send-repeat-"+id.New(), uuid.NewString()
		send(t, role, key, "dispatch's words", "optional").requireStatus(t, "the first marked send", http.StatusAccepted)
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		repeat := send(t, role, key, "dispatch's words again", "required")
		repeat.requireKeyTaken(t, "the repeated marked send", http.StatusAccepted, kept, "dispatch's words")
		if got := expectsReply(t, repeat); got != "optional" {
			t.Fatalf("the repeated marked send answered expects_reply %q, want the kept copy's %q", got, "optional")
		}
		if repeat.line["event_id"] != kept.EventID {
			t.Fatalf("the repeated marked send wrote the line %v, want the kept copy's event_id %s", repeat.line, kept.EventID)
		}
	})

	t.Run("case: a repeat whose client does not read the key-taken answer gets its own envelope, kept, with duplicate", func(t *testing.T) {
		role, key := "marked-send-unread-"+id.New(), uuid.NewString()
		words := "dispatch's first words " + id.New()
		send(t, role, key, words, "optional").requireStatus(t, "the first marked send", http.StatusAccepted)
		kept := heldCopy(t, rig.harness.client, bus.KeptRoleSubject(role))
		repeat := sendAs(t, false, role, key, "the repeat's own words", "required")
		repeat.requireOwnEnvelope(t, "the repeated marked send that does not read the key-taken answer", http.StatusAccepted, kept, words, "", "the repeat's own words")
		if !repeat.Kept {
			t.Fatalf("the repeated marked send that does not read the key-taken answer answered %s, want kept", repeat.raw)
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
		answer.requireKeyTaken(t, "the marked send under the agent's key", http.StatusAccepted, kept, words)
		if got := expectsReply(t, answer); got != "optional" {
			t.Fatalf("the marked send under the agent's key answered expects_reply %q, want the kept copy's %q", got, "optional")
		}
		if answer.line["event_id"] != kept.EventID || answer.line["source"] != "dispatch" {
			t.Fatalf("the marked send under the agent's key wrote the line %v, want the copy's event_id %s and Dispatch as the asker", answer.line, kept.EventID)
		}
	})
}
