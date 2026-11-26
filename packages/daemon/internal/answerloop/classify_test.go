package answerloop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// reads is what Classify reads of Dispatch: each ask's author, and each comment's thread by its
// root. A comment it holds no thread of is a read that fails.
type reads struct {
	asks    map[string]string
	threads map[string]dispatch.Thread
}

func (r reads) AskThread(_ context.Context, id string) (dispatch.AskThread, error) {
	return dispatch.AskThread{ID: id, IssueKey: new("LEGION-9"), Author: dispatch.Actor{Kind: "session", ID: r.asks[id]}}, nil
}

func (r reads) Thread(_ context.Context, id string) (dispatch.Thread, error) {
	thread, ok := r.threads[id]
	if !ok {
		return dispatch.Thread{}, fmt.Errorf("Dispatch answered 503 for the comment %s", id)
	}
	return thread, nil
}

// thread is the thread of the comment root, which author wrote, with replies in order.
func thread(root string, author intake.DispatchActor, replies ...dispatch.Comment) dispatch.Thread {
	return dispatch.Thread{Root: by(root, author), Replies: replies}
}

// by is the comment id, which author wrote.
func by(id string, author intake.DispatchActor) dispatch.Comment {
	return dispatch.Comment{ID: id, Author: dispatch.Actor{Kind: author.Kind, ID: author.ID}, Body: "a comment"}
}

// Every kind of hand-off is classified before any model call: what the agent answers (a message
// sent to it, a comment mentioning it, a reply on an ask it opened, a reply in a comment thread
// whose newest earlier comment it wrote or, after a person's, whose root it wrote, another
// agent's Envoy message), the decisions (a person's answer to its ask, a person's reply that
// hands its ask back to it), each answered too when it asks something and allowed to ask
// nothing, and everything it never answers. Of those it never answers, each row says whether
// it is the agent's own all the same (Verdict.ForAgent), which a released agent is woken for:
// every Dispatch event but the loop's own answer, the daemon's own post, a person's review of the
// agent's approval request and the agent's own act, and a message of a source no agent answers. A
// reply on, or an answer to, an ask the agent follows and did not open says so (Verdict.Followed).
func TestEveryHandoffIsClassifiedBeforeAnyModelCall(t *testing.T) {
	const ownAsk, othersAsk = "a-own", "a-other"
	agent := intake.DispatchActor{Kind: "session", ID: "ses_asker"}
	self := intake.DispatchActor{Kind: "session", ID: "ses_tester"}
	reviewer := intake.DispatchActor{Kind: "session", ID: "ses_reviewer"}
	daemon := intake.DispatchActor{Kind: "session", ID: "legion-daemon:LEGION"}
	reader := reads{asks: map[string]string{ownAsk: "ses_tester", othersAsk: "ses_reviewer"}, threads: map[string]dispatch.Thread{
		// The agent's root, under it people's replies and one of the agent's own.
		"r-own": thread("r-own", self, by("c-5", person), by("c-16", person), by("c-17", self), by("c-18", person)),
		// A person's root, the agent's reply, and a person's reply after it.
		"r-person": thread("r-person", person, by("c-19", self), by("c-20", person)),
		// The agent's root, the reviewer's reply, and a person's reply after it.
		"r-reviewed": thread("r-reviewed", self, by("c-21", reviewer), by("c-22", person)),
		// A person's root, the loop's answer in the agent's session, and a person's reply after it.
		"r-answered": thread("r-answered", person, by("c-23", self), by("c-24", person)),
		// The agent's root, and two people's replies under it.
		"r-people": thread("r-people", self, by("c-25", person), by("c-26", person)),
		// A thread only people wrote in.
		"r-nobody": thread("r-nobody", person, by("c-27", person)),
	}}
	reader.threads["r-answered"].Replies[0].Body = "Because B.\n\n<!-- legion-answer:0123456789abcdef0123456789abcdef -->"
	comment := func(id string, ask *string, body string) map[string]any {
		return map[string]any{"id": id, "issue_key": "LEGION-9", "body": body, "ask_id": ask}
	}
	mention := map[string]any{"attempt": 1, "mode": "btw", "comment_id": "c-1", "target": "role:legion-legion-legion-9-tester"}
	reply := func(id, waitingOn, body string) map[string]any {
		return map[string]any{"id": id, "issue_key": "LEGION-9", "body": body, "ask_id": ownAsk, "reply_to": nil, "ask_waiting_on": waitingOn}
	}
	threadReply := func(id, root, body string) map[string]any {
		return map[string]any{"id": id, "issue_key": "LEGION-9", "body": body, "ask_id": nil, "reply_to": root}
	}
	for _, tc := range []struct {
		name     string
		handoff  intake.Handoff
		kind     Kind // "" when it is no question
		decision bool
		maySkip  bool
		fails    bool   // the read fails, so the hand-off is tried again
		because  string // part of why it is no question, where the row holds the reason too
		forAgent bool   // no question and no decision, and the agent's own all the same
		followed bool   // on an ask the agent follows and did not open
	}{
		{name: "a message sent to the agent", handoff: messageHandoff("ses_tester", "m-1", 1, "Why plum?"), kind: KindMessage},
		{name: "a message on the conversation page", kind: KindMessage, handoff: dispatchHandoff("ses_tester", frame("message.created", person, nil,
			map[string]any{"id": "m-2", "issue_key": nil, "body": "Hi?", "target": "session:ses_tester"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "a message not sent to the agent", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("message.created", person, new("LEGION-9"),
			map[string]any{"id": "m-3", "body": "status?"}))},
		{name: "a comment mentioning the agent", kind: KindMention,
			handoff: dispatchHandoff("ses_tester", frame("comment.created", agent, new("LEGION-9"), comment("c-1", nil, "@tester why?"), mention))},
		{name: "an agent's reply on the agent's ask", kind: KindAskReply,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", agent, new("LEGION-9"), comment("c-2", new(ownAsk), "Which run?")))},
		{name: "a person's reply on the agent's ask", kind: KindAskReply, decision: true, maySkip: true,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"), comment("c-3", new(ownAsk), "Use B. Why A?")))},
		{name: "a person's reply handing the agent's ask back to it", kind: KindAskReply, decision: true, maySkip: true,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"), reply("c-9", "agent", "Use B.")))},
		{name: "a person's reply leaving the agent's ask with the person", kind: KindAskReply,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"), reply("c-10", "human", "Which version?")))},
		{name: "an agent's reply handing the agent's ask to the person", kind: KindAskReply,
			handoff: dispatchHandoff("ses_tester", bare("comment.created", agent, new("LEGION-9"), reply("c-11", "human", "Over to you: B?")))},
		{name: "a person's reply on the agent's ask that mentions it", kind: KindMention, decision: true, maySkip: true,
			handoff: dispatchHandoff("ses_tester", frame("comment.created", person, new("LEGION-9"), reply("c-12", "agent", "@tester use B; why A?"), mention))},
		{name: "the loop's own answer on the agent's ask", handoff: dispatchHandoff("ses_tester", bare("comment.created", self, new("LEGION-9"),
			reply("c-13", "human", "Because B.\n\n<!-- legion-answer:0123456789abcdef0123456789abcdef -->")))},
		{name: "a reply on an ask the agent follows", forAgent: true, followed: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), comment("c-4", new(othersAsk), "Why?")))},
		{name: "a reply on the agent's comment thread", kind: KindThreadReply, maySkip: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-5", "r-own", "ok")))},
		{name: "a root comment that mentions nobody", because: "a root comment", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), comment("c-15", nil, "ok")))},
		{name: "a reply after the agent's own in a thread", kind: KindThreadReply, maySkip: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-20", "r-person", "Why B?")))},
		{name: "a reply after another agent's on the agent's thread", because: "session ses_reviewer answers", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-22", "r-reviewed", "Why B?")))},
		{name: "the same reply to the agent that wrote the comment before it", kind: KindThreadReply, maySkip: true, handoff: dispatchHandoff("ses_reviewer",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-22", "r-reviewed", "Why B?")))},
		{name: "a reply after the loop's own answer", kind: KindThreadReply, maySkip: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-24", "r-answered", "And C?")))},
		{name: "a reply after a person's on the agent's thread", kind: KindThreadReply, maySkip: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-26", "r-people", "Agreed?")))},
		{name: "a reply in a thread only people wrote in", because: "no agent wrote", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-27", "r-nobody", "Agreed?")))},
		{name: "the agent's own reply on its comment thread", because: "no person wrote", handoff: dispatchHandoff("ses_tester",
			bare("comment.created", self, new("LEGION-9"), threadReply("c-17", "r-own", "Still plum.")))},
		// Another agent's reply is no question either, though it follows the agent's root; the same
		// reply by a person is ("a reply on the agent's comment thread").
		{name: "another agent's reply on the agent's comment thread", because: "no person wrote", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", reviewer, new("LEGION-9"), threadReply("c-5", "r-own", "ok")))},
		{name: "a reply on the agent's thread that mentions someone else", because: "mentions someone", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), func() map[string]any {
				p := threadReply("c-16", "r-own", "@reviewer why?")
				p["mentions"] = []any{map[string]any{"target": "role:legion-legion-legion-9-reviewer", "session_id": "ses_reviewer"}}
				return p
			}()))},
		{name: "a reply the thread's read does not hold", because: "does not hold", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-28", "r-own", "Why?")))},
		{name: "a reply in a document's comment thread", because: "document's comment thread", forAgent: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, nil, map[string]any{"id": "c-29", "issue_key": nil, "artifact_id": "doc-1", "body": "Why?", "ask_id": nil, "reply_to": "r-own"}))},
		{name: "a reply in a thread whose read fails", fails: true, handoff: dispatchHandoff("ses_tester",
			bare("comment.created", person, new("LEGION-9"), threadReply("c-30", "r-unread", "Why?")))},
		{name: "a person's answer to the agent's ask that asks", kind: KindAskReply, decision: true, maySkip: true,
			handoff: dispatchHandoff("ses_tester", answeredEvent(34, person, ownAsk, "question", []string{"B"}, new("Use B. Why A?")))},
		{name: "a person's answer to the agent's ask with options alone", decision: true,
			handoff: dispatchHandoff("ses_tester", answeredEvent(35, person, ownAsk, "question", []string{"B"}, nil))},
		{name: "a person's review of the agent's approval request", handoff: dispatchHandoff("ses_tester",
			answeredEvent(36, person, ownAsk, "approval", []string{"Approve"}, new("Looks right.")))},
		{name: "the agent's own hand-back of its approval request", handoff: dispatchHandoff("ses_tester",
			bare("ask.handed_back", self, new("LEGION-9"), map[string]any{"id": ownAsk}))},
		{name: "an answer to another's ask", forAgent: true, followed: true, handoff: dispatchHandoff("ses_tester",
			answeredEvent(37, person, othersAsk, "question", []string{"B"}, new("Why A?")))},
		{name: "an edit", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("comment.edited", person, new("LEGION-9"), comment("c-6", new(ownAsk), "edited")))},
		{name: "a message edit", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("message.edited", person, new("LEGION-9"), map[string]any{"id": "m-4"}))},
		{name: "a resolution", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("comment.resolved", person, new("LEGION-9"), comment("c-7", new(ownAsk), "")))},
		{name: "an ask resolved", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("ask.resolved", person, new("LEGION-9"), map[string]any{"id": ownAsk}))},
		{name: "a reply already answered", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("message.answered", agent, new("LEGION-9"), map[string]any{"id": "m-5"}))},
		{name: "a delivery receipt", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("message.delivery", agent, new("LEGION-9"), map[string]any{"id": "m-6"}))},
		{name: "a claim", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("issue.claimed", agent, new("LEGION-9"), map[string]any{"key": "LEGION-9"}))},
		{name: "an issue event", forAgent: true, handoff: dispatchHandoff("ses_tester", bare("issue.updated", person, new("LEGION-9"), map[string]any{"key": "LEGION-9"}))},
		{name: "the daemon's own post", handoff: dispatchHandoff("ses_tester", frame("message.created", daemon, new("LEGION-9"),
			map[string]any{"id": "m-7", "body": "READY"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "the loop's own answer, by its body marker", handoff: dispatchHandoff("ses_tester", frame("message.created", agent, new("LEGION-9"),
			map[string]any{"id": "m-8", "body": "Plum.\n\n<!-- legion-answer:abc -->"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "the loop's gh comment, by its footer's answer key", handoff: dispatchHandoff("ses_tester", frame("comment.created", agent, new("LEGION-9"),
			comment("c-8", nil, `Done. <!-- legion: {"session":"ses_x","phase":"testing","answer":"q.1"} -->`), mention))},
		// What another stopped agent's dispatch tool posts carries the loop's mark (dispatchtool.Mark),
		// so the loop answers none of it: a comment mentioning the agent, a message sent to it, and
		// an agent's message to its session. The same comment and message without the mark are
		// questions ("a comment mentioning the agent", "a message sent to the agent").
		{name: "the loop's dispatch comment mentioning the agent, by its mark", handoff: dispatchHandoff("ses_tester", frame("comment.created", agent, new("LEGION-9"),
			comment("c-31", nil, "@tester why?"+dispatchtool.Mark("dispatch:m-1", 3)), mention))},
		{name: "the loop's dispatch message to the agent, by its mark", handoff: messageHandoff("ses_tester", "m-9", 1, "Why plum?"+dispatchtool.Mark("dispatch:m-1", 4))},
		{name: "the loop's dispatch message to the agent's session, by its mark", handoff: dispatchHandoff("ses_tester", frame("message.created", agent, nil,
			map[string]any{"id": "m-10", "issue_key": nil, "body": "Hi?" + dispatchtool.Mark("dispatch:m-1", 5), "target": "session:ses_tester"}, map[string]any{"attempt": 1, "mode": "btw"}))},
		{name: "another agent's Envoy message", kind: KindEnvoy, maySkip: true,
			handoff: envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.0123456789abcdef0123456789abcdef", "Which fruit?")},
		{name: "an Envoy message with the loop's dedupe key", handoff: envoyHandoff("notifications.role.x", "ses_tester", "evt-2", "legion-answer:q1", "Plum.")},
		{name: "the loop's answer sent by Envoy", handoff: envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-3",
			"agent.ses_tester.0123456789abcdef0123456789abcdef", "Plum.\n\n<!-- legion-answer:q2 -->")},
		{name: "the daemon's notice", handoff: envoyHandoff("notifications.role.x", "ses_tester", "evt-4", "legion-outbox:42", "worker-died")},
		{name: "the broker's message", handoff: func() intake.Handoff {
			h := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-5", "envoy.wake.1", "wake")
			h.Envelope.Source = "envoy"
			return h
		}()},
		{name: "a person's Envoy message", forAgent: true, handoff: func() intake.Handoff {
			h := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-6", "x", "hi")
			h.Envelope.Source, h.Envelope.SourceSession = "human", ""
			return h
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := Classify(context.Background(), tc.handoff, store.AgentConversation{Session: "ses_tester"}, reader)
			if tc.fails {
				if err == nil {
					t.Fatalf("classified as %+v; want the read's error, so the hand-off is tried again", verdict)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.kind == "" && verdict.Question != nil:
				t.Fatalf("answered as a %s question; want none (%s)", verdict.Question.Kind, verdict.Because)
			case tc.kind != "" && verdict.Question == nil:
				t.Fatalf("not answered (%s); want a %s question", verdict.Because, tc.kind)
			case tc.kind != "" && (verdict.Question.Kind != tc.kind || verdict.Question.MayAskNothing != tc.maySkip):
				t.Fatalf("question = %+v; want kind %s, may ask nothing %t", *verdict.Question, tc.kind, tc.maySkip)
			case tc.decision != (verdict.Decision != nil):
				t.Fatalf("decision = %t; want %t", verdict.Decision != nil, tc.decision)
			case !strings.Contains(verdict.Because, tc.because):
				t.Fatalf("not answered because %q; want %q among the reasons", verdict.Because, tc.because)
			case verdict.ForAgent != tc.forAgent || verdict.Followed != tc.followed:
				t.Fatalf("the agent's own %t, on an ask it follows %t (%s); want %t and %t", verdict.ForAgent, verdict.Followed, verdict.Because, tc.forAgent, tc.followed)
			}
		})
	}
}

// A decision names its own id, the same whichever envelope carries it (the Dispatch event's for an
// answer, the comment's for a reply), the person, where they decided and what: an answer's selected
// options and text, a reply's body. An answer that also asks is answered on the ask's thread. An
// answer also names the ask's kind, the one the event carries and not a kind the daemon knows
// alone, since that is what says whether the parked-decision relay sends it; a reply names none.
// Both name the issue the ask stands on, which is the issue that relay reads (Decision.AskIssue).
func TestADecisionNamesItsIDThePersonWhereAndWhat(t *testing.T) {
	asks := reads{asks: map[string]string{askID: "ses_tester"}}
	conv := store.AgentConversation{Session: "ses_tester", Issue: "LEGION-9"}
	classify := func(h intake.Handoff) Verdict {
		t.Helper()
		verdict, err := Classify(context.Background(), h, conv, asks)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Decision == nil {
			t.Fatalf("no decision (%s)", verdict.Because)
		}
		return verdict
	}
	answer := answeredEvent(34, person, askID, "question", []string{"B", "C"}, new("Use B. Why A?"))
	first := classify(dispatchHandoff("ses_tester", answer))
	redelivered := dispatchHandoff("ses_tester", answer)
	redelivered.Envelope.EventID, redelivered.Envelope.DedupeKey = "dispatch-2", "dispatch-2"
	again := classify(redelivered)
	want := Decision{ID: "dispatch:34", Asker: "user:person@example.com", Ref: "dispatch://LEGION-9/ask/" + askID,
		Text: "Selected: B; C\n\nUse B. Why A?", Answered: true, AskKind: "question", AskIssue: "LEGION-9", Conversation: conv}
	if *first.Decision != want || *again.Decision != want {
		t.Fatalf("decisions = %+v, %+v; want %+v for both", *first.Decision, *again.Decision, want)
	}
	if q := first.Question; q == nil || q.Identity != "dispatch:34" || q.AskID != askID || q.Issue != "LEGION-9" || q.Text != want.Text || q.Ref != want.Ref {
		t.Fatalf("question = %+v; want the answer's, on the ask's thread", q)
	}
	reply := classify(dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-9", "issue_key": "LEGION-9", "body": "Use B.", "ask_id": askID, "reply_to": nil, "ask_waiting_on": "agent"})))
	if want := (Decision{ID: "dispatch:c-9", Asker: "user:person@example.com", Ref: "dispatch://LEGION-9/comment/c-9", Text: "Use B.",
		AskIssue: "LEGION-9", Conversation: conv}); *reply.Decision != want {
		t.Fatalf("decision = %+v; want %+v", *reply.Decision, want)
	}
	legacy := classify(dispatchHandoff("ses_tester", answeredEvent(35, person, askID, "action", []string{"B"}, new("Use B."))))
	if got := *legacy.Decision; !got.Answered || got.AskKind != "action" {
		t.Fatalf("decision on a legacy action ask = %+v; want it answered, naming the kind the event carries", got)
	}
}

// A question's identity is the Dispatch message or comment it came from, whichever delivery
// attempt carried it, and an Envoy message's dedupe key.
func TestAQuestionsIdentityIsItsMessageOrDedupeKey(t *testing.T) {
	first, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 1, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	later, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 2, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != "dispatch:m-1" || later.Identity != first.Identity || later.Attempt != 2 {
		t.Fatalf("identities = %q, %q; want dispatch:m-1 for both attempts", first.Identity, later.Identity)
	}
	envoy, err := ReadOrigin(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Which?"))
	if err != nil {
		t.Fatal(err)
	}
	if envoy.Identity != "agent.ses_tester.k1" || envoy.Sender != "ses_asker" || envoy.EventID != "evt-1" {
		t.Fatalf("Envoy origin = %+v", envoy)
	}
}

// Who asked is named for the model and for the controller a question is passed to: a person by
// their Dispatch login, an agent by its session, with the session's title when the listener
// stamped one on the envelope; where is the dispatch:// reference or the Envoy event.
func TestAQuestionsAskerIsTheLoginOrTheSessionWithItsTitle(t *testing.T) {
	person, err := ReadOrigin(messageHandoff("ses_tester", "m-1", 1, "Why?"))
	if err != nil {
		t.Fatal(err)
	}
	if person.Asker != "user:person@example.com" || person.Ref != "dispatch://LEGION-9/message/m-1" {
		t.Fatalf("Dispatch origin asker %q, ref %q; want the login and the message's reference", person.Asker, person.Ref)
	}
	untitled := envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Which?")
	agent, err := ReadOrigin(untitled)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Asker != "session:ses_asker" || agent.Ref != "envoy:evt-1" {
		t.Fatalf("Envoy origin asker %q, ref %q; want the session and the event", agent.Asker, agent.Ref)
	}
	titled := untitled
	titled.Envelope.Sender = &intake.HandoffSender{SessionID: "ses_asker", Title: "LEGION-9 implementer"}
	if agent, err = ReadOrigin(titled); err != nil {
		t.Fatal(err)
	}
	if agent.Asker != "session:ses_asker (LEGION-9 implementer)" {
		t.Fatalf("Envoy origin with a stamped sender asker %q; want the session and its title", agent.Asker)
	}
}

// A reply in a comment thread is answered in that thread: its identity is the reply's, the model
// may hear it asks nothing, and its answer is a reply to it on its issue, in the thread of its
// root. A reply in a document's thread, which has no issue, is no question the loop answers.
func TestAReplyInACommentThreadIsAnsweredInThatThread(t *testing.T) {
	origin, err := ReadOrigin(dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-2", "issue_key": "LEGION-9", "body": "Why plum?", "ask_id": nil, "reply_to": "c-1"})))
	if err != nil {
		t.Fatal(err)
	}
	want := Origin{Kind: KindThreadReply, Identity: "dispatch:c-2", Text: "Why plum?", Asker: "user:person@example.com",
		Where: "a reply in the comment thread of c-1 on LEGION-9", Ref: "dispatch://LEGION-9/comment/c-2", MayAskNothing: true,
		CommentID: "c-2", Issue: "LEGION-9", Thread: "c-1"}
	if origin != want {
		t.Fatalf("origin = %+v\nwant %+v", origin, want)
	}
	if origin, err := ReadOrigin(dispatchHandoff("ses_tester", bare("comment.created", person, nil,
		map[string]any{"id": "c-3", "issue_key": nil, "artifact_id": "doc-1", "body": "Why?", "ask_id": nil, "reply_to": "c-1"}))); err == nil {
		t.Fatalf("origin = %+v; want a reply in a document's thread refused", origin)
	}
}
