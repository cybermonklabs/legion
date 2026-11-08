package answerloop

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// answerHandoff is the loop's own answer handed back to this daemon: the message the answer one
// stopped agent gave was sent to the asker as, carrying the marker every reply the loop posts
// ends with, and handed off because the asker is stopped too.
func answerHandoff(target, sender, eventID, dedupeKey, text string) intake.Handoff {
	topic := "notifications.agent." + target
	return intake.Handoff{
		OriginalTopic: topic, TargetSession: target, RoleToken: string(testerToken), Project: project,
		Envelope: intake.HandoffEnvelope{EventID: eventID, Source: "agent", SourceEventID: "s", SourceSession: sender,
			Topic: topic, DedupeKey: dedupeKey, PayloadSummary: text, InReplyTo: "evt-asked", TraceID: "t"},
	}
}

// claudeConversation records a Claude Code conversation of session for token in a transcript of
// its own, and returns its path.
func (r *rig) claudeConversation(session string, token claim.Token) string {
	r.t.Helper()
	path := filepath.Join(r.dir, session+".jsonl")
	common := `,"userType":"external","entrypoint":"sdk-cli","cwd":"/work","sessionId":"` + session + `","version":"2.1.311","gitBranch":"main"}`
	content := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"Test the fruit."},` +
		`"uuid":"00000000-0000-4000-8000-000000000001","timestamp":"2026-10-26T00:00:00.000Z"` + common + "\n" +
		`{"parentUuid":"00000000-0000-4000-8000-000000000001","isSidechain":false,"type":"assistant","effort":"high",` +
		`"message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Tested: plum."}],` +
		`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}},` +
		`"uuid":"00000000-0000-4000-8000-000000000002","timestamp":"2026-10-26T00:00:01.000Z"` + common + "\n" +
		`{"type":"last-prompt","lastPrompt":"Test the fruit.","leafUuid":"00000000-0000-4000-8000-000000000002","sessionId":"` + session + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.st.RegisterConversation(r.ctx, store.AgentConversation{
		Session: session, Project: project, Issue: "LEGION-9", Role: claim.RoleTester, Token: token,
		Harness: runtime.BackendClaude, Location: path, StartedAt: testNow, RecordedAt: testNow,
	}); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// heldMessages are the messages of the asker's conversation, as its harness rebuilds it on
// resume, that hold needle.
func heldMessages(t *testing.T, r *rig, needle string) []conversation.Message {
	t.Helper()
	conv, ok, err := r.st.AgentConversation(r.ctx, "ses_asker")
	if err != nil || !ok {
		t.Fatalf("the asker's conversation: %v, held %t", err, ok)
	}
	session, err := (Harnesses{}).Open(r.ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var held []conversation.Message
	for _, message := range session.Conversation().Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, needle) {
				held = append(held, message)
				break
			}
		}
	}
	return held
}

// The loop's own answer, sent in one stopped agent's name to an asker that is stopped too, is
// handed back to this daemon by the listener. It is never answered — no model call, no question
// recorded, nothing posted — and is written into the asker's conversation as a message it
// received, so the asker holds the answer when it runs again. It is written in once: the same
// hand-off delivered again, and one the daemon is handed after a restart, add nothing, because
// the append records the envelope's dedupe key as the delivery's own.
func TestTheLoopsOwnAnswerIsDeliveredIntoAStoppedAskersConversation(t *testing.T) {
	const key = "agent.ses_asker.0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		open func(r *rig) string
	}{
		{"an Oh My Pi session", func(r *rig) string { return r.conversation("ses_asker", testerToken, 0) }},
		{"a Claude Code transcript", func(r *rig) string { return r.claudeConversation("ses_asker", testerToken) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			path := tc.open(r)
			before := fileContent(t, path)
			answer := "Plum, from Which?\n\n" + AnswerMarker + "0123456789abcdef0123456789abcdef -->"
			handoff := answerHandoff("ses_asker", "ses_tester", "evt-9", key, answer)
			r.receive(handoff)

			after := fileContent(t, path)
			if !strings.HasPrefix(after, before) || after == before {
				t.Fatalf("the asker's conversation after the hand-off:\n%s", after)
			}
			delivered := heldMessages(t, r, answer)
			if len(delivered) != 1 || delivered[0].Role != conversation.User ||
				!strings.HasPrefix(delivered[0].Content[0].Text, "session:ses_tester sent you, in an Envoy message from session ses_tester:\n") {
				t.Errorf("the asker's conversation holds the answer as %+v; want one message it received, naming who sent it and where", delivered)
			}
			if got := strings.Count(after, key); got != 1 {
				t.Errorf("the delivery %s is recorded %d times; want once, by the marker the append writes", key, got)
			}
			if got := r.answerCount(); got != 0 {
				t.Errorf("model calls = %d; want none for the loop's own answer", got)
			}
			if got := r.questions(); len(got) != 0 {
				t.Errorf("questions recorded = %v; want none", got)
			}
			if posts, sends := r.dispatch.posts(), r.listener.posts(); len(posts) != 0 || len(sends) != 0 {
				t.Errorf("posts = %s; listener posts = %s; want nothing posted", describe(posts), describe(sends))
			}

			r.receive(handoff)
			if again := fileContent(t, path); again != after {
				t.Errorf("the hand-off delivered again wrote:\n%s", strings.TrimPrefix(again, after))
			}
			r.restart()
			r.receive(handoff)
			if again := fileContent(t, path); again != after {
				t.Errorf("the hand-off after a restart wrote:\n%s", strings.TrimPrefix(again, after))
			}
			if got := r.answerCount(); got != 0 {
				t.Errorf("model calls after the redeliveries = %d; want none", got)
			}
		})
	}
}

// A hand-off the loop's answer came back on while the asker's claim runs its conversation writes
// nothing into that conversation: the running agent takes the answer live, through the
// listener's direct-message route, in its sender's own words and for a live session alone
// (`handoff: false`), and the hand-off is done with — nothing is left to be delivered again.
func TestTheLoopsOwnAnswerReachesARunningAskerLive(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_asker", testerToken, 0)
	before := fileContent(t, path)
	r.gate.set(r.gate.running, "ses_asker", true)
	answer := "Plum.\n\n" + AnswerMarker + "k9 -->"
	if err := r.loop.Receive(r.ctx, answerHandoff("ses_asker", "ses_tester", "evt-9", "agent.ses_asker.k9", answer), nil); err != nil {
		t.Fatalf("Receive = %v; want the answer given to the running asker", err)
	}
	sends := r.listener.posts()
	want := request{"POST", "/v1/messages/send", map[string]any{"target_session": "ses_asker", "source": "agent",
		"source_session": "ses_tester", "message": answer, "in_reply_to": "evt-asked", "idempotency_key": "agent.ses_asker.k9",
		"handoff": false}}
	if len(sends) != 1 || !reflect.DeepEqual(sends[0], want) {
		t.Fatalf("listener posts = %s; want %s", describe(sends), describe([]request{want}))
	}
	if after := fileContent(t, path); after != before {
		t.Errorf("the running asker's conversation was written to:\n%s", strings.TrimPrefix(after, before))
	}
	if got := r.answerCount(); got != 0 {
		t.Errorf("model calls = %d; want none", got)
	}
}

// The asker stopped again between the hand-off and the live send: the listener refuses the send
// for a live session, and the answer is written into the conversation after all, so nothing is
// handed round between the two.
func TestAnAskerThatStoppedAgainGetsTheAnswerInItsConversation(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_asker", testerToken, 0)
	before := fileContent(t, path)
	// The claim stops as the send reaches the listener, which finds no live session for it.
	listener := newStandIn(t, func(w http.ResponseWriter, _ request) {
		r.gate.set(r.gate.running, "ses_asker", false)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session ses_asker"}`))
	})
	r.cfg.Envoy = notify.New(listener.URL, "envoy-token")
	r.loop = New(r.cfg)
	r.gate.set(r.gate.running, "ses_asker", true)
	answer := "Plum.\n\n" + AnswerMarker + "k9 -->"
	handoff := answerHandoff("ses_asker", "ses_tester", "evt-9", "agent.ses_asker.k9", answer)
	if err := r.loop.Receive(r.ctx, handoff, nil); err != nil {
		t.Fatalf("Receive = %v; want the refused answer written into the conversation", err)
	}
	after := fileContent(t, path)
	if !strings.HasPrefix(after, before) || after == before {
		t.Fatalf("the asker's conversation after the refused send:\n%s", after)
	}
	if got := len(listener.posts()); got != 1 {
		t.Errorf("listener posts = %d; want the one live send that was refused", got)
	}
	if delivered := heldMessages(t, r, answer); len(delivered) != 1 || delivered[0].Role != conversation.User {
		t.Errorf("the asker's conversation holds the answer as %+v; want one message it received", delivered)
	}
}
