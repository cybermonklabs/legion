package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

const (
	replacingSession     = "ses_fresh"
	replacingSessionFile = "/state/sessions/fresh.jsonl"
)

// registerFresh brings the agent that replaced an abandoned session up to ready under a session id
// of its own, so a test can tell the session the claim recorded from the one it left behind.
func (h *harness) registerFresh() {
	h.t.Helper()
	h.connect()
	h.must(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        replacingSession,
		SessionFile:    replacingSessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: h.deps.PluginContract,
	})
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: replacingSession})
}

// wantAbandonedToldBy is whether the claim's abandoned session has been told, and by the
// confirmation of which delivery (AbandonedSession.ToldBy), as the machine holds it and as the
// store holds it: "" is a session still untold, which names no delivery.
func (h *harness) wantAbandonedToldBy(by, when string) {
	h.t.Helper()
	for where, got := range map[string]AbandonedSession{"the machine": h.claim().Abandoned, "the store": h.store.load(h.token).Abandoned} {
		if got.Session == "" {
			h.t.Fatalf("%s: %s records no abandoned session, want one", when, where)
		}
		if got.Told != (by != "") || got.ToldBy != by {
			h.t.Fatalf("%s: %s records the abandonment told=%v by %q, want told=%v by %q", when, where, got.Told, got.ToldBy, by != "", by)
		}
	}
}

// abandonSession walks the claim to where the provider refused every turn of it out of its launch
// budget and an operator retried it, which leaves that session behind: from here the claim records
// an abandoned session nothing has told its replacement of yet.
func (h *harness) abandonSession(refusal string) {
	h.t.Helper()
	h.reach(StateIdle)
	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, refusal)
	}
	h.wantState(StateFailed)
	h.must(RequestRetry{Claim: h.token})
}

// A claim the provider refused out of its budget fails, and the retry of it leaves that session
// behind. The claim records which session it abandoned and why, since nothing else does — the
// session itself stays in its store — and the first task the agent that replaces it is given names
// that session, the cause in the provider's own words (the context size it refused at among them),
// the issue's branch and the handoffs committed on it. Nothing in a new session says that an older
// one existed, so a worker given the bare task would read an empty transcript as a fresh start and
// do again what the abandoned session already pushed (dispatch://INFRA-714, item 4 of the refusal
// section).
func TestTheFreshSessionsFirstTaskNamesTheSessionItReplacedAndWhatOutlivedIt(t *testing.T) {
	h := newHarness(t)
	refusal := "the provider ended the turn with an error at 226k tokens of context"

	h.abandonSession(refusal)

	if got := h.claim().Abandoned; got.Session != session || got.SessionFile != sessionFile || !strings.Contains(got.Why, refusal) {
		t.Fatalf("the claim records %+v, want the session %q it left behind and the provider's own words for why", got, session)
	}
	h.registerFresh()
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	first := h.wantPrompts(base + 1)[base].Message
	for _, want := range []string{
		session, "abandoned", refusal,
		"legion/LEGION-209", "dispatch handoff-read --issue LEGION-209 --phase", handoffWordsClause(),
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the first task of the fresh session does not name %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, "Continue Workflow. Issue: LEGION-209. Phase: implementing.") {
		t.Errorf("the first task of the fresh session is %q, want it to end with the task itself", first)
	}

	// The fresh agent has ended a turn, so it has read that first task: the next one is the task
	// alone, not the abandonment told again.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if second := h.wantPrompts(base + 2)[base+1].Message; second != "the next task" {
		t.Errorf("the next task of the fresh session is %q, want the task alone", second)
	}
}

// Whoever abandons a session, the claim records which one and why in that one's own words: an
// operator's `legion claims forget-session`, and an operator's retry in a new session, which fails
// nothing and so has no failure to name.
func TestEveryAbandonmentIsRecordedWithItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
		why   string
	}{
		{"the operator forgetting the session", RequestForgetSession{Claim: testToken, Session: session}, "forget-session"},
		{"a retry asking for a new session", RequestRetry{Claim: testToken, FreshSession: true}, "the retry asked for a new session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestStop{Claim: h.token})
			h.wantState(StateRetired)

			h.must(tc.event)

			got := h.claim()
			if got.Session != "" || got.SessionFile != "" {
				t.Fatalf("the claim still records session %q (%s), want it abandoned", got.Session, got.SessionFile)
			}
			if got.Abandoned.Session != session || !strings.Contains(got.Abandoned.Why, tc.why) {
				t.Fatalf("the claim records %+v, want the abandoned %q and a reason naming %q", got.Abandoned, session, tc.why)
			}
		})
	}
}

// A prompt the agent refused reached nobody, so the abandoned session is still owed to the fresh
// agent: the task goes back in the queue and the claim and the store both still record the
// sentence as untold. The prompt the agent does acknowledge carries it again, and the turn that
// takes that prompt is what marks it told — not the acknowledgement, which Oh My Pi gives at a
// prompt's admission and can still follow with a busy answer — so the task after it is the task
// alone. Marking it where the send started instead left the first task the fresh session actually
// read without the branch, the handoffs and the sentence that says an older session existed at all
// (dispatch://INFRA-714).
func TestARefusedPromptLeavesTheAbandonedSessionStillToBeTold(t *testing.T) {
	h := newHarness(t)
	refusal := "the provider ended the turn with an error at 226k tokens of context"
	h.abandonSession(refusal)
	h.registerFresh()
	base := len(h.prompts())
	h.conn.RefusePrompt("the agent will not take it")

	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	if first := h.wantPrompts(base + 1)[base].Message; !strings.Contains(first, session) {
		t.Fatalf("the refused prompt is %q, want it to name the abandoned session %q", first, session)
	}
	if told, stored := h.claim().Abandoned.Told, h.store.load(h.token).Abandoned.Told; told || stored {
		t.Fatalf("after the refusal the claim records the abandonment told = %v and the store %v, want it untold in both: the agent the prompt was refused by never read it", told, stored)
	}

	// The shim's next hello sends the task again, and this time the agent takes it.
	h.conn.FailPrompt(nil)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})

	acknowledged := h.wantPrompts(base + 2)[base+1]
	for _, want := range []string{session, "abandoned", refusal, "legion/LEGION-209",
		"dispatch handoff-read --issue LEGION-209 --phase", handoffWordsClause()} {
		if !strings.Contains(acknowledged.Message, want) {
			t.Errorf("the task the agent acknowledged does not name %q:\n%s", want, acknowledged.Message)
		}
	}
	h.wantAbandonedToldBy("", "at the acknowledgement")

	// That task's turn takes it, which tells the abandonment, and once it is over the one after it
	// is the task alone.
	h.must(StreamTurnStart{Claim: h.token})
	h.wantAbandonedToldBy(acknowledged.DeliveryID, "once the task's turn began")
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if next := h.wantPrompts(base + 3)[base+2].Message; next != "the next task" {
		t.Errorf("the task after the one the agent acknowledged is %q, want the task alone", next)
	}
}

// The abandoned session's sentence is told at the confirmation of the prompt that carried it, by
// the rule the record of a let-go follows (confirm). The pinned Oh My Pi acknowledges a prompt at
// its admission and can then answer it busy (lateRefused): the fresh agent is in a turn of its own
// and never read that prompt, so the abandonment is still untold and the task the turn's end sends
// names the session again. The turn that takes that prompt tells it, by that delivery. A late busy
// answer to the same prompt then says that turn was another's: the confirmation goes back, and the
// telling with it, so the next prompt names the session again, and the turn that takes it tells it.
func TestAnAbandonedSessionsSentenceIsToldAtConfirmation(t *testing.T) {
	h := newHarness(t)
	refusal := "the provider ended the turn with an error at 226k tokens of context"
	h.abandonSession(refusal)
	h.registerFresh()
	base := len(h.prompts())
	names := func(prompt fake.Prompt, which string) {
		t.Helper()
		for _, want := range []string{session, "abandoned", refusal, "legion/LEGION-209"} {
			if !strings.Contains(prompt.Message, want) {
				t.Fatalf("%s does not name %q:\n%s", which, want, prompt.Message)
			}
		}
	}

	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})
	first := h.wantPrompts(base + 1)[base]
	names(first, "the fresh session's first task")
	h.wantAbandonedToldBy("", "at the acknowledgement")

	h.must(StreamLateRefusal{Claim: h.token, DeliveryID: first.DeliveryID, Error: busyAnswer})
	h.wantAbandonedToldBy("", "after the busy answer")

	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	second := h.wantPrompts(base + 2)[base+1]
	names(second, "the task the agent's own turn end sent")
	h.must(StreamTurnStart{Claim: h.token})
	h.wantAbandonedToldBy(second.DeliveryID, "once the task's turn began")

	// Oh My Pi answers that prompt busy after all: the turn that confirmed it was another input's.
	h.must(StreamLateRefusal{Claim: h.token, DeliveryID: second.DeliveryID, Error: busyAnswer})
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the late busy answer = %+v, want the confirmation taken back", p)
	}
	h.wantAbandonedToldBy("", "after the confirmation that told it was taken back")

	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	third := h.wantPrompts(base + 3)[base+2]
	names(third, "the task sent after the other input's turn")
	h.must(StreamTurnStart{Claim: h.token})
	h.wantAbandonedToldBy(third.DeliveryID, "once the task's own turn began")
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
	if next := h.wantPrompts(base + 4)[base+3].Message; next != "the next task" {
		t.Errorf("the task after the one whose turn told the abandonment is %q, want the task alone", next)
	}
}

// handoffWordsClause is the list of phase words exactly as the prompt renders it, read from the
// phases themselves as delivery's handoffWords builds it: a phase that gains a handoff is
// asserted here without anyone remembering to come back. The whole clause is asserted rather
// than each word on its own, because the prompt carries the task behind it — "Phase:
// implementing" names a word all by itself — so a word dropped from the list would still be
// found somewhere in the text.
func handoffWordsClause() string {
	words := make([]string, 0, len(phase.All))
	for _, p := range phase.All {
		if word, _, ok := phase.Handoff(p); ok {
			words = append(words, word)
		}
	}
	if len(words) == 0 {
		panic("no phase has a handoff record: the prompt would name none")
	}
	return "(" + strings.Join(words, ", ") + ")"
}
