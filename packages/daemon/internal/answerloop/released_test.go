package answerloop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// releasedRig is the loop's rig for a tester whose claim the daemon released, with its
// conversation, whose path the recorder reads at each wake.
func releasedRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t)
	path := r.conversation("ses_tester", testerToken, 0)
	r.rec.path = path
	r.gate.set(r.gate.released, "ses_tester", true)
	return r, path
}

// A question for an agent whose claim the daemon released is answered from its conversation and
// posted as any stopped agent's: nothing is forwarded to its role topic, and nothing wakes it.
func TestIdleReleasedAgentsQuestionIsAnsweredAndWakesNothing(t *testing.T) {
	r, path := releasedRig(t)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	if q := r.only(); q.State != store.QuestionPosted || q.Wake || r.answerCount() != 1 {
		t.Fatalf("question = %s, wake %t, after %d answers; want posted once with no wake", q.State, q.Wake, r.answerCount())
	}
	if posts := r.dispatch.posts(); len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), "Plum, from Why plum?") {
		t.Fatalf("Dispatch posts = %s; want the answer", describe(posts))
	}
	if !strings.Contains(fileContent(t, path), "Plum, from Why plum?") {
		t.Fatal("the conversation holds no exchange of the answer")
	}
	if sends := r.listener.posts(); len(sends) != 0 || len(r.rec.wakes) != 0 || len(r.rec.passed) != 0 {
		t.Fatalf("listener posts = %s, wakes %+v, passed %+v; want none", describe(sends), r.rec.wakes, r.rec.passed)
	}
}

// A person's answer to the released agent's ask that also asks is held: Decisions is told
// nothing, the question is answered, appended and posted on the ask's thread, and only then is
// the agent given the answer through one wake, which says where the loop posted its answer. The
// wake is asked for once its question is done, however often the answer is handed off, and the
// conversation held the loop's answer when it was.
func TestIdleReleasedAgentsDecisionThatAlsoAsksIsGivenOnceItsAnswerIsIn(t *testing.T) {
	r, _ := releasedRig(t)
	decided := dispatchHandoff("ses_tester", answeredEvent(34, person, askID, "question", []string{"B"}, new("Use B. Why did you test A?")))
	r.receive(decided)
	if q := r.only(); !q.Wake || len(r.rec.wakes) != 0 {
		t.Fatalf("question = %+v and wakes %+v at the hand-off; want the decision held on its question, nothing given yet", q, r.rec.wakes)
	}
	r.receive(decided)
	r.drain()
	r.restart()
	r.receive(decided)
	r.drain()
	q := r.only()
	if q.State != store.QuestionPosted || r.answerCount() != 1 || len(r.dispatch.posts()) != 1 {
		t.Fatalf("question = %s after %d answers and posts %s; want it posted once", q.State, r.answerCount(), describe(r.dispatch.posts()))
	}
	if len(r.rec.decisions) != 0 || len(r.rec.passed) != 0 {
		t.Fatalf("decisions %+v, passed %+v; want nothing told to the architect or the controller", r.rec.decisions, r.rec.passed)
	}
	if len(r.rec.wakes) != 1 {
		t.Fatalf("wakes = %+v; want one", r.rec.wakes)
	}
	wake := r.rec.wakes[0]
	if wake.Key != q.ID || wake.Session != "ses_tester" || wake.Issue != "LEGION-9" {
		t.Fatalf("wake = %+v; want the question's own, for ses_tester on LEGION-9", wake)
	}
	for _, want := range []string{"Selected: B\n\nUse B. Why did you test A?", "already answered the question in this message",
		"dispatch://LEGION-9/ask/" + askID, "do not answer it again"} {
		if !strings.Contains(wake.Text, want) {
			t.Errorf("the wake tells the agent %q; want it to hold %q", wake.Text, want)
		}
	}
	if !strings.Contains(r.rec.atWake[0], "Plum, from Selected: B") {
		t.Fatal("the wake was asked for before the loop's answer was in the conversation")
	}
}

// For a released agent, every reply the loop would not post as its answer, and so would pass on
// or end with nothing, goes to the agent itself: nothing is appended or posted, the controller is
// told nothing, and the agent is given the message through one wake and the question ends woken.
// A post refused for good is given the same way in place of its pass-on. The control is
// TestTheReplyKindDecidesWhatIsPostedAndPassedOn, the same replies for an agent that is not
// released.
func TestIdleReleasedAgentsRepliesTheLoopDoesNotPostWakeIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(req answer.Request) answer.Reply
	}{
		{"not answered", func(req answer.Request) answer.Reply {
			reply := answered(req, "I would need my working copy.")
			reply.Kind = answer.NotAnswered
			return reply
		}},
		{"needs work", func(req answer.Request) answer.Reply {
			reply := answered(req, "That needs a run of my phase; nothing starts.")
			reply.Kind = answer.NeedsWork
			return reply
		}},
		{"asks nothing", func(req answer.Request) answer.Reply {
			reply := answered(req, "Noted.")
			reply.Kind = answer.None
			return reply
		}},
		{"a fault with no turns", func(answer.Request) answer.Reply {
			return answer.Reply{Kind: answer.NotAnswered, Fault: answer.AnswerSeconds}
		}},
		{"a fault with its turns", func(req answer.Request) answer.Reply {
			reply := answered(req, "Half an answer.")
			reply.Kind, reply.Fault = answer.NotAnswered, answer.ModelCalls
			return reply
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path := releasedRig(t)
			before := fileContent(t, path)
			r.answer = func(req answer.Request) (answer.Reply, error) { return tc.reply(req), nil }
			r.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Run the suite?"))
			r.drain()
			assertWoken(t, r, path, before, "Run the suite?")
		})
	}
	t.Run("over the daily limit", func(t *testing.T) {
		r, path := releasedRig(t)
		before := fileContent(t, path)
		if err := r.st.AddAnswerSpend(r.ctx, project, testNow, 50); err != nil {
			t.Fatal(err)
		}
		r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
		r.drain()
		if r.answerCount() != 0 {
			t.Fatalf("answers = %d; want none over the limit", r.answerCount())
		}
		assertWoken(t, r, path, before, "Why plum?")
	})
}

// assertWoken holds a released agent's one question to the wake in place of a reply: woken, nothing
// posted or passed on, path's conversation as it was before, and one wake carrying asked.
func assertWoken(t *testing.T, r *rig, path, before, asked string) {
	t.Helper()
	q := r.only()
	switch {
	case q.State != store.QuestionWoken || !q.Wake:
		t.Fatalf("question = %s, wake %t; want woken", q.State, q.Wake)
	case len(r.dispatch.posts()) != 0 || len(r.listener.posts()) != 0:
		t.Fatalf("posts = %s %s; want none", describe(r.dispatch.posts()), describe(r.listener.posts()))
	case fileContent(t, path) != before:
		t.Fatalf("the conversation changed:\n%s", fileContent(t, path)[len(before):])
	case len(r.rec.passed) != 0:
		t.Fatalf("passed on = %+v; want nothing told to the controller", r.rec.passed)
	case len(r.rec.wakes) != 1 || r.rec.wakes[0].Key != q.ID || !strings.Contains(r.rec.wakes[0].Text, asked):
		t.Fatalf("wakes = %+v; want one, keyed by the question, carrying %q", r.rec.wakes, asked)
	}
}

// leaked is a word that every write recorded by recordWrites carries in its arguments or its
// result: a wake names a write by its command, never by what it carried.
const leaked = "sk-leak-7731"

// recordedCalls is how many calls recordWrites records.
const recordedCalls = 6

// recordWrites records, as an answer's tools do before it ends, the calls it made for question: a
// gh merge that exited 0, a gh read, a gh review whose outcome is unknown (no exit status), a
// dispatch comment that exited 0, a dispatch read and a dispatch message whose outcome is unknown.
// The four writes are the calls a wake names, and the two reads none.
func recordWrites(r *rig, question string) error {
	zero := 0
	for _, call := range []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "merge", "7", "-R", "acme/widgets", "--squash", "--body", leaked}, Owner: "acme", Exit: &zero},
		{Seq: 2, Tool: store.ToolGH, Args: []string{"pr", "view", "7", "-R", "acme/widgets"}, Owner: "acme", Exit: &zero},
		{Seq: 3, Tool: store.ToolGH, Args: []string{"pr", "review", "7", "-R", "acme/widgets", "--approve"}, Owner: "acme"},
		{Seq: 4, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "LEGION-12", "--body", leaked}, Exit: &zero},
		{Seq: 5, Tool: store.ToolDispatch, Args: []string{"read", "LEGION-12"}, Exit: &zero},
		{Seq: 6, Tool: store.ToolDispatch, Args: []string{"message", "--issue", "LEGION-12", "--body", leaked}},
	} {
		call.Question, call.StartedAt = question, testNow
		if call.Tool == store.ToolGH {
			call.App = "implement"
		}
		if err := r.st.StartAnswerToolCall(r.ctx, call); err != nil {
			return err
		}
		result := leaked + " output"
		if call.Exit == nil {
			result = "outcome unknown, " + leaked
		}
		if err := r.st.FinishAnswerToolCall(r.ctx, question, call.Seq, result, call.Exit, testNow); err != nil {
			return err
		}
	}
	return nil
}

// failCallsAfterAppended is the loop's store with one failure in it: the first read of a
// question's recorded calls after the question is marked appended fails, as a connection reset or
// a statement timeout fails it. Everything else is the real store.
type failCallsAfterAppended struct {
	Store
	appended, failed bool
}

func (f *failCallsAfterAppended) UpdateQuestion(ctx context.Context, q store.AnswerQuestion, from ...store.QuestionState) error {
	err := f.Store.UpdateQuestion(ctx, q, from...)
	if err == nil && q.State == store.QuestionAppended {
		f.appended = true
	}
	return err
}

func (f *failCallsAfterAppended) AnswerToolCalls(ctx context.Context, question string) ([]store.AnswerToolCall, error) {
	if f.appended && !f.failed {
		f.failed = true
		return nil, errors.New("the store is unreachable")
	}
	return f.Store.AnswerToolCalls(ctx, question)
}

// assertWakeNamesWrites holds r's one question to its one wake, want, and to nothing run again: the
// model was called once, and the question's calls are the recordedCalls recordWrites recorded.
func assertWakeNamesWrites(t *testing.T, r *rig, want string) {
	t.Helper()
	calls, err := r.st.AnswerToolCalls(r.ctx, r.only().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.rec.wakes) != 1 || r.rec.wakes[0].Text != want {
		t.Errorf("wakes = %+v; want one telling the agent %q", r.rec.wakes, want)
	}
	if r.answerCount() != 1 || len(calls) != recordedCalls {
		t.Errorf("the model was called %d times and the question holds %d calls; want once and the %d recorded: nothing ran again", r.answerCount(), len(calls), recordedCalls)
	}
}

// A released agent whose question the loop gives it unanswered, after the question's calls ran
// writes, is told those writes in the one wake: its message word for word, and after it, apart,
// each gh write by its command, its repository and gh's exit, and each Dispatch write by its
// command and dispatch's exit, `no exit status` where the program answered none, and nothing of a
// call's arguments or output; a read is none. The wake does not say that the controller was told.
// Nothing runs again (the model is called once and the question holds the six calls it had), and
// the question is woken with nothing posted, appended or passed on (assertWoken). A fault reaches
// the wake two ways, a run that returned no turns (decideFault) and one that returned turns, and a
// call list the store cannot read when the wake is made ends the pass with the question still
// appended and the agent given nothing: the next pass gives the same wake, so a failed read is
// never taken for no writes. The control is TestIdleReleasedAgentsRepliesTheLoopDoesNotPostWakeIt,
// whose questions ran no write and whose wake is the message alone.
func TestIdleReleasedAgentsWakeNamesTheWritesAFaultedAnswerRan(t *testing.T) {
	const asked = "Merge PR 7 and say so on LEGION-12?"
	const want = "user:person@example.com sent you, in a Dispatch message on LEGION-9 (dispatch://LEGION-9/message/m-1), while you were stopped:\n" + asked +
		"\n\nThe Legion daemon's answer loop had already run these writes in your name for this message." +
		" gh writes: `gh pr merge` on acme/widgets (exit 0); `gh pr review` on acme/widgets (no exit status)." +
		" Dispatch writes: `dispatch comment` (exit 0); `dispatch message` (no exit status)." +
		" Do not repeat them: for one with no exit status, read what it did first."
	withNoTurns := func(answer.Request) answer.Reply {
		return answer.Reply{Kind: answer.NotAnswered, Fault: answer.ModelCalls, CostUSD: 0.25}
	}
	withTurns := func(req answer.Request) answer.Reply {
		reply := answered(req, "Half an answer.")
		reply.Kind, reply.Fault = answer.NotAnswered, answer.ModelCalls
		return reply
	}
	faulting := func(r *rig, reply func(answer.Request) answer.Reply) {
		r.answer = func(req answer.Request) (answer.Reply, error) {
			if err := recordWrites(r, req.Question.ID); err != nil {
				return answer.Reply{}, err
			}
			return reply(req), nil
		}
	}
	for _, tc := range []struct {
		name  string
		reply func(answer.Request) answer.Reply
	}{
		{"a fault with no turns", withNoTurns},
		{"a fault with its turns", withTurns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path := releasedRig(t)
			before := fileContent(t, path)
			faulting(r, tc.reply)
			r.receive(messageHandoff("ses_tester", "m-1", 1, asked))
			r.drain()
			assertWoken(t, r, path, before, asked)
			assertWakeNamesWrites(t, r, want)
		})
	}
	t.Run("a call list the store cannot read at the wake", func(t *testing.T) {
		r, path := releasedRig(t)
		before := fileContent(t, path)
		r.cfg.Store = &failCallsAfterAppended{Store: r.cfg.Store}
		r.restart()
		faulting(r, withNoTurns)
		r.receive(messageHandoff("ses_tester", "m-1", 1, asked))
		r.loop.schedule(r.ctx)
		r.loop.workers.Wait()
		if q := r.only(); q.State != store.QuestionAppended || len(r.rec.wakes) != 0 {
			t.Fatalf("question = %s, wakes %+v after the failed read; want it appended and the agent given nothing", q.State, r.rec.wakes)
		}
		r.drain()
		assertWoken(t, r, path, before, asked)
		assertWakeNamesWrites(t, r, want)
	})
}

// A released agent is given what reaches it and asks it nothing, once each by its own identity:
// a root comment that mentions nobody, a person's decision that asks nothing besides (in place of
// Decisions), and the loop's own answer to it as an asker (in place of its conversation). The
// daemon's own post, a comment carrying the loop's own mark, the daemon's own notice and the
// broker's message are none of the agent's and give it nothing. For an agent that is not
// released, the same hand-offs give nothing, and the decision goes to Decisions.
func TestIdleReleasedAgentIsGivenWhatAsksItNothingAndNothingThatIsNotItsOwn(t *testing.T) {
	daemon := intake.DispatchActor{Kind: "session", ID: dispatch.DaemonSessionPrefix + "legion"}
	comment := dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-root", "issue_key": "LEGION-9", "body": "Thanks, that settles it.", "ask_id": nil}))
	decided := dispatchHandoff("ses_tester", answeredEvent(35, person, askID, "question", []string{"B"}, nil))
	ownAnswer := answerHandoff("ses_tester", "ses_asker", "evt-answer", "agent.ses_tester.kans", "Plum.\n\n"+AnswerMarker+"0123 -->")
	notYours := []intake.Handoff{
		dispatchHandoff("ses_tester", bare("comment.created", daemon, new("LEGION-9"),
			map[string]any{"id": "c-daemon", "issue_key": "LEGION-9", "body": "LEGION-9 moved to testing."})),
		dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
			map[string]any{"id": "c-marked", "issue_key": "LEGION-9", "body": "Plum.\n\n" + AnswerMarker + "0123 -->"})),
		envoyHandoff("notifications.role."+string(testerToken), "ses_tester", "evt-notice", "legion-outbox:7", "phase-finished on LEGION-9"),
	}
	broker := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-broker", "envoy.k1", "your session was reaped")
	broker.Envelope.Source = "envoy"
	notYours = append(notYours, broker)

	r, path := releasedRig(t)
	before := fileContent(t, path)
	for _, h := range append([]intake.Handoff{comment, decided, ownAnswer, comment, decided, ownAnswer}, notYours...) {
		r.receive(h)
	}
	if all := r.questions(); len(all) != 0 {
		t.Fatalf("questions = %+v; want none recorded", all)
	}
	if len(r.rec.decisions) != 0 || fileContent(t, path) != before {
		t.Fatalf("decisions = %+v, conversation changed %t; want neither", r.rec.decisions, fileContent(t, path) != before)
	}
	if len(r.rec.wakes) != 3 {
		t.Fatalf("wakes = %+v; want one each for the comment, the decision and the loop's answer", r.rec.wakes)
	}
	for i, want := range []string{"Thanks, that settles it.", "Selected: B", "Plum."} {
		if !strings.Contains(r.rec.wakes[i].Text, want) {
			t.Errorf("wake %d tells the agent %q; want it to hold %q", i, r.rec.wakes[i].Text, want)
		}
	}

	notReleased := newRig(t)
	notReleased.conversation("ses_tester", testerToken, 0)
	for _, h := range append([]intake.Handoff{comment, decided}, notYours...) {
		notReleased.receive(h)
	}
	if len(notReleased.rec.wakes) != 0 || len(notReleased.rec.decisions) != 1 {
		t.Fatalf("for an agent that is not released: wakes %+v, decisions %+v; want no wake and the decision passed on",
			notReleased.rec.wakes, notReleased.rec.decisions)
	}
}

// A wake that gives a released agent a person's decision on its ask names that decision
// (Wake.Decision), so the daemon can pass it on if the claim fails or retires before the wake
// gives it: a decision that asks nothing besides, at the hand-off; one that also asks, once its
// answer is posted; and one that also asks whose reply the loop does not post, given unanswered.
// The question of the last two records that it holds the decision
// (store.AnswerQuestion.HoldsDecision). The control is a comment that asks nothing and a question
// given unanswered with no decision: their wakes name none.
func TestIdleReleasedAgentsWakeNamesTheDecisionItGives(t *testing.T) {
	notAnswered := func(req answer.Request) (answer.Reply, error) {
		reply := answered(req, "I would need my working copy.")
		reply.Kind = answer.NotAnswered
		return reply, nil
	}
	for _, tc := range []struct {
		name     string
		handoff  intake.Handoff
		answer   func(answer.Request) (answer.Reply, error)
		decision string // the decision's id the wake names, "" for none
		question bool   // whether a question is recorded, and holds the decision
	}{
		{name: "a decision that asks nothing", decision: "dispatch:35",
			handoff: dispatchHandoff("ses_tester", answeredEvent(35, person, askID, "question", []string{"B"}, nil))},
		{name: "a decision that also asks, answered", decision: "dispatch:34", question: true,
			handoff: dispatchHandoff("ses_tester", answeredEvent(34, person, askID, "question", []string{"B"}, new("Use B. Why did you test A?")))},
		{name: "a decision that also asks, given unanswered", decision: "dispatch:34", question: true, answer: notAnswered,
			handoff: dispatchHandoff("ses_tester", answeredEvent(34, person, askID, "question", []string{"B"}, new("Use B. Why did you test A?")))},
		{name: "a comment that asks nothing", handoff: dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
			map[string]any{"id": "c-root", "issue_key": "LEGION-9", "body": "Thanks, that settles it.", "ask_id": nil}))},
		{name: "a question given unanswered", question: true, answer: notAnswered,
			handoff: envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Run the suite?")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := releasedRig(t)
			if tc.answer != nil {
				r.answer = tc.answer
			}
			r.receive(tc.handoff)
			r.drain()
			if len(r.rec.wakes) != 1 {
				t.Fatalf("wakes = %+v; want one", r.rec.wakes)
			}
			got := ""
			if d := r.rec.wakes[0].Decision; d != nil {
				got = d.ID
				if d.Asker != "user:person@example.com" || d.Ref != "dispatch://LEGION-9/ask/"+askID || !strings.Contains(d.Text, "Selected: B") || !d.Answered {
					t.Errorf("the wake's decision = %+v; want the person's answer to the agent's ask", *d)
				}
			}
			if got != tc.decision {
				t.Fatalf("the wake names the decision %q; want %q", got, tc.decision)
			}
			if questions := r.questions(); tc.question != (len(questions) == 1) {
				t.Fatalf("questions = %+v; want one recorded %t", questions, tc.question)
			} else if tc.question && r.only().HoldsDecision != (tc.decision != "") {
				t.Fatalf("the question holds a decision %t; want %t", r.only().HoldsDecision, tc.decision != "")
			}
			if len(r.rec.decisions) != 0 {
				t.Fatalf("decisions = %+v; want none passed on while the claim is released", r.rec.decisions)
			}
		})
	}
}

// A person's resolution of an ask the released agent follows but did not open, which Dispatch
// sends to every follower and the listener hands off in place of the wake it raised before, is
// given to the agent once through one wake, however often it is handed off: no model call, no
// question recorded, nothing posted, passed on or decided, and the conversation as it was. A
// person's review of the agent's own approval request gives it nothing: the design gate relays it
// to the agent's architect as the document's review. For an agent that is not released, the
// resolution gives nothing either.
func TestIdleReleasedFollowerIsGivenAPersonsResolutionOfTheAskOnce(t *testing.T) {
	const followed = "a0000000-0000-4000-8000-000000000002"
	resolved := dispatchHandoff("ses_tester", bare("ask.resolved", person, new("LEGION-9"),
		map[string]any{"id": followed, "issue_key": "LEGION-9", "kind": "question", "state": "resolved"}))
	review := dispatchHandoff("ses_tester", answeredEvent(36, person, askID, "approval", []string{"Approve"}, new("Looks right.")))
	review.Envelope.EventID, review.Envelope.DedupeKey = "dispatch-2", "dispatch-2"

	r, path := releasedRig(t)
	before := fileContent(t, path)
	for _, h := range []intake.Handoff{resolved, review, resolved} {
		r.receive(h)
	}
	r.drain()
	if all := r.questions(); len(all) != 0 || r.answerCount() != 0 {
		t.Fatalf("questions = %+v after %d model calls; want none recorded and no model call", all, r.answerCount())
	}
	if posts, sends := r.dispatch.posts(), r.listener.posts(); len(posts) != 0 || len(sends) != 0 || len(r.rec.passed) != 0 || len(r.rec.decisions) != 0 {
		t.Fatalf("Dispatch posts %s, listener posts %s, passed %+v, decisions %+v; want none", describe(posts), describe(sends), r.rec.passed, r.rec.decisions)
	}
	if fileContent(t, path) != before {
		t.Fatalf("the conversation changed:\n%s", fileContent(t, path)[len(before):])
	}
	if len(r.rec.wakes) != 1 {
		t.Fatalf("wakes = %+v; want one, for the resolution", r.rec.wakes)
	}
	wake := r.rec.wakes[0]
	if wake.Session != "ses_tester" || wake.Issue != "LEGION-9" {
		t.Fatalf("wake = %+v; want it for ses_tester on LEGION-9", wake)
	}
	for _, want := range []string{"user:person@example.com sent you, in a Dispatch ask.resolved on LEGION-9, while you were stopped:", followed} {
		if !strings.Contains(wake.Text, want) {
			t.Errorf("the wake tells the agent %q; want it to hold %q", wake.Text, want)
		}
	}

	notReleased := newRig(t)
	notReleased.conversation("ses_tester", testerToken, 0)
	notReleased.receive(resolved)
	notReleased.drain()
	if len(notReleased.rec.wakes) != 0 || len(notReleased.questions()) != 0 || notReleased.answerCount() != 0 {
		t.Fatalf("for an agent that is not released: wakes %+v, questions %+v, %d model calls; want none",
			notReleased.rec.wakes, notReleased.questions(), notReleased.answerCount())
	}
}

// A released agent that follows an ask another agent opened is woken for a person's answer to
// that ask and for a person's reply on its thread, and each wake names the ask as one the agent
// follows and did not open, never as its own: what was decided there is the other agent's, and an
// agent told the ask was its own could act on a decision made for someone else. Nothing is
// answered, recorded or decided for either. The control is the same answer on the agent's own
// ask, a decision, which the wake names as the agent's ask.
func TestIdleReleasedFollowerIsToldAFollowedAsksAnswerAndReplyAsOnAnAskItFollows(t *testing.T) {
	const followed = "a0000000-0000-4000-8000-000000000003"
	answer := dispatchHandoff("ses_tester", answeredEvent(37, person, followed, "question", []string{"B"}, new("Use B.")))
	reply := dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-41", "issue_key": "LEGION-9", "body": "Why A?", "ask_id": followed}))

	r, _ := releasedRig(t)
	r.ask = map[string]any{"ask": map[string]any{"id": followed, "issue_key": "LEGION-9", "author": map[string]any{"kind": "session", "id": "ses_reviewer"}},
		"replies": []any{}}
	r.receive(answer)
	r.receive(reply)
	r.drain()
	if all := r.questions(); len(all) != 0 || r.answerCount() != 0 || len(r.rec.decisions) != 0 || len(r.rec.passed) != 0 {
		t.Fatalf("questions %+v after %d model calls, decisions %+v, passed %+v; want none", all, r.answerCount(), r.rec.decisions, r.rec.passed)
	}
	if len(r.rec.wakes) != 2 {
		t.Fatalf("wakes = %+v; want one for the answer and one for the reply", r.rec.wakes)
	}
	where := "ask " + followed + " on LEGION-9, which the agent follows but did not open"
	for i, want := range []string{
		"user:person@example.com sent you, in an answer to " + where + " (dispatch://LEGION-9/ask/" + followed + "), while you were stopped:\nSelected: B\n\nUse B.",
		"user:person@example.com sent you, in a reply on " + where + " (dispatch://LEGION-9/comment/c-41), while you were stopped:\nWhy A?",
	} {
		if got := r.rec.wakes[i].Text; got != want {
			t.Errorf("wake %d tells the follower %q; want %q", i, got, want)
		}
	}

	own, _ := releasedRig(t)
	own.receive(dispatchHandoff("ses_tester", answeredEvent(38, person, askID, "question", []string{"B"}, nil)))
	want := "user:person@example.com sent you, in an answer to the agent's ask " + askID + " on LEGION-9 (dispatch://LEGION-9/ask/" + askID +
		"), while you were stopped:\nSelected: B"
	if len(own.rec.wakes) != 1 || own.rec.wakes[0].Text != want {
		t.Fatalf("wakes for an answer on the agent's own ask = %+v; want one telling it %q", own.rec.wakes, want)
	}
}

// A released agent is woken by no act of its own session: its own reply in its comment thread and
// its own hand-back of its approval request, which Dispatch routes back to their author, raise no
// wake, record no question and leave the conversation as it was, however often they are handed
// off. The control is another agent's reply in the same thread, which wakes the agent once.
func TestIdleReleasedAgentIsWokenByNoActOfItsOwn(t *testing.T) {
	self := intake.DispatchActor{Kind: "session", ID: "ses_tester"}
	reviewer := intake.DispatchActor{Kind: "session", ID: "ses_reviewer"}
	reply := func(actor intake.DispatchActor, id string) intake.Handoff {
		return dispatchHandoff("ses_tester", bare("comment.created", actor, new("LEGION-9"),
			map[string]any{"id": id, "issue_key": "LEGION-9", "body": "Still plum.", "ask_id": nil, "reply_to": threadRoot}))
	}
	ownReply := reply(self, "c-51")
	ownHandBack := dispatchHandoff("ses_tester", bare("ask.handed_back", self, new("LEGION-9"), map[string]any{"id": askID}))

	r, path := releasedRig(t)
	before := fileContent(t, path)
	for _, h := range []intake.Handoff{ownReply, ownHandBack, ownReply, ownHandBack} {
		r.receive(h)
	}
	r.drain()
	if all := r.questions(); len(all) != 0 || len(r.rec.wakes) != 0 || r.answerCount() != 0 || len(r.rec.decisions) != 0 {
		t.Fatalf("questions %+v, wakes %+v, %d model calls, decisions %+v for the agent's own acts; want none", all, r.rec.wakes, r.answerCount(), r.rec.decisions)
	}
	if fileContent(t, path) != before {
		t.Fatalf("the conversation changed:\n%s", fileContent(t, path)[len(before):])
	}

	r.receive(reply(reviewer, "c-52"))
	want := "session:ses_reviewer sent you, in a reply in the comment thread of " + threadRoot + " on LEGION-9"
	if len(r.rec.wakes) != 1 || !strings.HasPrefix(r.rec.wakes[0].Text, want) {
		t.Fatalf("wakes after another agent's reply = %+v; want one telling the agent %q", r.rec.wakes, want)
	}
}

// A message of a source no agent answers, a GitHub notification here, is given to a released
// agent by its own source and topic: the wake does not tell it as an Envoy session's message.
func TestIdleReleasedAgentIsToldAMessageOfAnotherSourceByItsSourceAndTopic(t *testing.T) {
	const topic = "notifications.github.acme.widgets.pull_request_review.submitted"
	github := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-github", "github.k1", "acme/widgets#7 got a review")
	github.Envelope.Source, github.Envelope.SourceSession, github.Envelope.Topic = "github", "", topic

	r, _ := releasedRig(t)
	r.receive(github)
	if len(r.rec.wakes) != 1 {
		t.Fatalf("wakes = %+v; want one for the GitHub notification", r.rec.wakes)
	}
	want := "github sent you, in a message on " + topic + ", while you were stopped:\nacme/widgets#7 got a review"
	if got := r.rec.wakes[0].Text; got != want {
		t.Errorf("the wake tells the agent %q; want %q", got, want)
	}
}
