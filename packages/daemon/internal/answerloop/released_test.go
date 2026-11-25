package answerloop

import (
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
