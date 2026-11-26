package supervise

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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

// abandonSession walks the claim to where an operator stopped it and a retry asking for a new
// session started it again, which leaves the recorded session behind (freshSession): from here the
// claim records an abandoned session nothing has told its replacement of yet. It answers why the
// claim records the session was left: the retry's own reason, since a stopped claim carries no
// failure to name.
func (h *harness) abandonSession() string {
	h.t.Helper()
	h.reach(StateIdle)
	h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
	h.wantState(StateRetired)
	h.must(RequestRetry{Claim: h.token, FreshSession: true})
	return "the retry asked for a new session"
}

// A retry that asks for a new session leaves the recorded one behind. The claim records which
// session it abandoned and why, since nothing else does — the session itself stays in its store —
// and the first task the agent that replaces it is given names that session, quoted, why it was
// left, the issue's branch and the handoffs committed on it. Nothing in a new session says that an
// older one existed, so a worker given the bare task would read an empty transcript as a fresh
// start and do again what the abandoned session already pushed (dispatch://INFRA-714, item 4 of
// the refusal section).
func TestTheFreshSessionsFirstTaskNamesTheSessionItReplacedAndWhatOutlivedIt(t *testing.T) {
	h := newHarness(t)

	refusal := h.abandonSession()

	if got := h.claim().Abandoned; got.Session != session || got.SessionFile != sessionFile || !strings.Contains(got.Why, refusal) {
		t.Fatalf("the claim records %+v, want the session %q it left behind and why", got, session)
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
	if quoted := "The session this claim ran before this one, " + strconv.Quote(session) + ", was abandoned"; !strings.Contains(first, quoted) {
		t.Errorf("the first task of the fresh session does not quote the session it replaced, %q:\n%s", quoted, first)
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
// operator's forget, from `legion claims forget-session` or its retry --fresh-session, the forget
// of the controller's fresh session, which no operator asked for, and an operator's retry in a new
// session, which fails nothing and so has no failure to name.
//
// The negative control is forgottenBy answering the operator's sentence for every asker: the
// controller's row then records that an operator cleared the session.
func TestEveryAbandonmentIsRecordedWithItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
		why   string
	}{
		{"the operator forgetting the session", RequestForgetSession{Claim: testToken, Session: session, By: SuspendedByOperator}, "forget-session"},
		{"the controller's fresh session", RequestForgetSession{Claim: testToken, Session: session, By: SuspendedByController},
			"the project's controller asked for a fresh session of this agent"},
		{"a retry asking for a new session", RequestRetry{Claim: testToken, FreshSession: true}, "the retry asked for a new session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
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

// A take a hold refused reached nobody, so the abandoned session is still owed to the fresh agent:
// the task waits for its next take, and the claim and the store both still record the sentence as
// untold. The take that is accepted is answered with it, and that acceptance is what marks it
// told, so the task after it is the task alone. A take of the same number sent again is answered
// the same text, sentence and all, and marks nothing again. Marking it where a take was refused
// instead would leave the first task the fresh session actually read without the branch, the
// handoffs and the sentence that says an older session existed at all (dispatch://INFRA-714).
func TestTakeRefusedLeavesTheAbandonedSessionStillToBeTold(t *testing.T) {
	h := newHarness(t)
	refusal := h.abandonSession()
	h.registerFresh()
	h.manualTakes()
	base := len(h.prompts())

	// A turn of the fresh agent's own is running when the task arrives, so its take is refused.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})
	h.wantRefused(HoldInTurn)
	if told, stored := h.claim().Abandoned.Told, h.store.load(h.token).Abandoned.Told; told || stored {
		t.Fatalf("after the refusal the claim records the abandonment told = %v and the store %v, want it untold in both: the take was refused", told, stored)
	}

	// The turn ends, the value is written again, and this time the take is accepted.
	h.must(StreamTurnEnd{Claim: h.token})
	taken := h.mustTake()
	for _, want := range []string{session, "abandoned", refusal, "legion/LEGION-209",
		"dispatch handoff-read --issue LEGION-209 --phase", handoffWordsClause()} {
		if !strings.Contains(taken, want) {
			t.Errorf("the accepted take's text does not name %q:\n%s", want, taken)
		}
	}
	h.wantAbandonedToldBy(h.pending().ID, "at the accepted take")
	if again := h.mustTake(); again != taken {
		t.Fatalf("the take sent again was answered %q, want the same text %q", again, taken)
	}
	h.wantPrompts(base + 2)
	h.wantAbandonedToldBy(h.pending().ID, "after the take sent again, which marks nothing again")

	// That task's turn is over, so the one after it is the task alone.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if next := h.mustTake(); next != "the next task" {
		t.Errorf("the task after the one the agent took is %q, want the task alone", next)
	}
}

// The abandoned session's sentence is told by the accepted take whose text carried it, by the rule
// the record of a let-go follows (accept), and the record names that take's delivery (ToldBy). A
// take a hold refused carried nothing: the fresh agent is in a turn of its own, so the abandonment
// is still untold, and the take after that turn's end carries it and tells it. A provider error
// then takes that take back: the error is a model call that failed, often the turn's first, so
// the telling goes back with the take (giveBackTake), and the take after the ladder's wait carries
// the session again and tells it again. Once that task's turn is over, the task after it is the
// task alone.
//
// The negative control is the give-back taken out of the provider error's take-back (takeBack's
// giveBackTake): the take after the wait is then answered with the task and the provider's note
// alone, and this test reads the missing session there.
func TestAnAbandonedSessionsSentenceIsToldAtTheAcceptedTake(t *testing.T) {
	h := newHarness(t)
	refusal := h.abandonSession()
	h.registerFresh()
	h.manualTakes()
	says := []string{session, "abandoned", refusal, "legion/LEGION-209"}

	// A turn of the fresh agent's own is running when the task arrives, so its take is refused.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})
	h.wantRefused(HoldInTurn)
	h.wantAbandonedToldBy("", "after the take a hold refused")

	h.must(StreamTurnEnd{Claim: h.token})
	first := h.mustTake()
	wantSays(t, first, says...)
	id := h.pending().ID
	h.wantAbandonedToldBy(id, "at the accepted take")

	// The take's turn ends in a provider error, which takes the take back.
	h.must(StreamTurnStart{Claim: h.token})
	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
	if p := h.pending(); p.ID != id || p.taken() {
		t.Fatalf("pending after the provider error = %+v, want the task %s taken back", p, id)
	}
	h.wantAbandonedToldBy("", "after the provider error took the take that told it back")

	h.advance(promptRetryDelay(1))
	again := h.mustTake()
	wantSays(t, again, says...)
	h.wantAbandonedToldBy(id, "at the take accepted after the ladder's wait")

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
	if next := h.mustTake(); next != "the next task" {
		t.Errorf("the task after the one whose take told the abandonment is %q, want the task alone", next)
	}
}

// The give-back of a take's telling is keyed on the take, not on the delivery, whose id stands
// across every take-back of its task. Take 1 carries the abandoned session's sentence; its turn
// starts and its process dies in it, so that turn ran the text and the telling stands. Take 2,
// made by the relaunched process, carries nothing of it, and is given back before its turn starts:
// its own process dies first. Take 3 must not tell the session again, since take 2 told nothing
// to give back (departure 11: told once).
//
// The negative control is the give-back keyed on the delivery's id alone (untell before this
// change): take 2's give-back then untells what take 1 told, and take 3 carries the sentence a
// second time.
func TestAbandonedSessionToldByACutOffTurnIsNotToldAgainWhenALaterTakeIsGivenBack(t *testing.T) {
	h := newHarness(t)
	refusal := h.abandonSession()
	h.registerFresh()
	h.manualTakes()
	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	first := h.mustTake()
	wantSays(t, first, refusal)
	id := h.pending().ID
	h.wantAbandonedToldBy(id, "at take 1")
	h.must(StreamTurnStart{Claim: h.token})
	h.observe(runtime.Gone)
	h.wantAbandonedToldBy(id, "once take 1's turn was cut off")

	h.registerFresh()
	second := h.mustTake()
	wantSaysNot(t, second, refusal)
	h.observe(runtime.Gone)
	h.wantAbandonedToldBy(id, "once take 2, which carried nothing of it, was given back before its turn")

	h.registerFresh()
	third := h.mustTake()
	wantSaysNot(t, third, refusal)
	h.wantAbandonedToldBy(id, "at take 3")
}

// A take's give-back untells what that take told, which the take itself records
// (Delivery.TakeTold) and the store keeps with it, so a daemon restarted between the take and its
// give-back gives back what one that never stopped would. Take 1 carries the abandoned session's
// sentence; its turn starts and its process dies in it, so that turn ran the text and the telling
// stands. Take 2, made by the relaunched process, carries nothing of it. The daemon restarts, and
// take 2 is given back before its turn starts: its own process dies first. Nothing goes back to
// untold, and take 3 does not tell the session again (departure 11: told once). The other way
// round, a take that told the session, accepted before the restart and given back before its
// turn, gives the telling back, so the next take tells the session rather than lose it.
//
// The negative control is the restored machine reading the take's telling from the records that
// name the pending delivery (ToldBy is its id): take 2's give-back then untells what take 1 told,
// and take 3 carries the sentence a second time.
func TestTakeBackAfterADaemonRestartGivesBackOnlyWhatThatTakeTold(t *testing.T) {
	const task = "Continue Workflow. Issue: LEGION-209. Phase: implementing."
	// Why the claim records the session was left (abandonSession): the retry's own reason.
	refusal := "the retry asked for a new session"

	t.Run("a take that told nothing", func(t *testing.T) {
		h := newHarness(t)
		h.abandonSession()
		h.registerFresh()
		h.manualTakes()
		h.must(RequestDeliver{Claim: h.token, Task: task})

		first := h.mustTake()
		wantSays(t, first, refusal)
		id := h.pending().ID
		h.must(StreamTurnStart{Claim: h.token})
		h.observe(runtime.Gone)
		h.wantAbandonedToldBy(id, "once take 1's turn was cut off")

		h.registerFresh()
		second := h.mustTake()
		wantSaysNot(t, second, refusal)

		h.restart()
		h.connect()
		h.observe(runtime.Gone)
		h.wantAbandonedToldBy(id, "once take 2, which carried nothing of it, was given back before its turn after the restart")

		h.registerFresh()
		third := h.mustTake()
		wantSaysNot(t, third, refusal)
		h.wantAbandonedToldBy(id, "at take 3")
	})

	t.Run("a take that told the session", func(t *testing.T) {
		h := newHarness(t)
		h.abandonSession()
		h.registerFresh()
		h.manualTakes()
		h.must(RequestDeliver{Claim: h.token, Task: task})

		first := h.mustTake()
		wantSays(t, first, refusal)
		id := h.pending().ID

		h.restart()
		h.connect()
		h.observe(runtime.Gone)
		h.wantAbandonedToldBy("", "once take 1, which told it, was given back before its turn after the restart")

		h.registerFresh()
		second := h.mustTake()
		wantSays(t, second, refusal)
		h.wantAbandonedToldBy(id, "at take 2")
	})
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
