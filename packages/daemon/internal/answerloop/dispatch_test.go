package answerloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// fakeDispatchTools opens the rig's dispatch tool, recording the session and question each answer
// opens it for, the place it hands it, and how many it closed; its calls answer with output
// naming them.
type fakeDispatchTools struct {
	mu     sync.Mutex
	opened []string
	places map[string]dispatchtool.Place
	closed int
	calls  []dispatchtool.Call
}

func (f *fakeDispatchTools) Open(session, question string, place dispatchtool.Place) (DispatchTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, session+" "+question)
	if f.places == nil {
		f.places = map[string]dispatchtool.Place{}
	}
	f.places[question] = place
	return fakeDispatchTool{f}, nil
}

type fakeDispatchTool struct{ tools *fakeDispatchTools }

func (t fakeDispatchTool) Run(_ context.Context, call dispatchtool.Call) dispatchtool.Result {
	t.tools.mu.Lock()
	defer t.tools.mu.Unlock()
	t.tools.calls = append(t.tools.calls, call)
	return dispatchtool.Result{Output: "LEGION-9: a comment since you stopped says plum", Ran: true}
}

func (t fakeDispatchTool) Close() error {
	t.tools.mu.Lock()
	defer t.tools.mu.Unlock()
	t.tools.closed++
	return nil
}

// An answer whose model calls dispatch posts its reply as any answer does: the loop opens the
// answer's dispatch tool as the agent's session for the question, hands it to the model loop
// beside gh, and closes it once the answer is done. A loop with no dispatch tool, a daemon that
// found no dispatch command, hands the model loop none.
func TestAnAnswerThatCallsDispatchPostsItsReply(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	tools := &fakeDispatchTools{}
	r.cfg.DispatchTools = tools
	r.restart()
	r.answer = func(req answer.Request) (answer.Reply, error) {
		if req.Dispatch == nil || req.GH == nil {
			t.Errorf("the request carries gh %v and dispatch %v; want both", req.GH, req.Dispatch)
			return answer.Reply{}, nil
		}
		read := req.Dispatch.Run(context.Background(), dispatchtool.Call{Seq: 1, N: 1, Args: `["read","LEGION-9"]`})
		return answered(req, "Plum, from what "+read.Output), nil
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "What does the new comment say?"))
	r.drain()
	q := r.only()
	posts := r.dispatch.posts()
	if q.State != store.QuestionPosted || len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), "Plum, from what LEGION-9: a comment since you stopped says plum\n\n"+AnswerMarker+q.ID+" -->") {
		t.Fatalf("question %s, posts %s; want the reply from the dispatch read posted", q.State, describe(posts))
	}
	tools.mu.Lock()
	defer tools.mu.Unlock()
	if !slices.Equal(tools.opened, []string{"ses_tester " + q.ID}) || tools.closed != 1 || len(tools.calls) != 1 {
		t.Errorf("the dispatch tool was opened %v, closed %d times, called %d times; want once as ses_tester for %s, closed, one call", tools.opened, tools.closed, len(tools.calls), q.ID)
	}

	plain := newRig(t)
	plain.conversation("ses_tester", testerToken, 0)
	plain.answer = func(req answer.Request) (answer.Reply, error) {
		if req.Dispatch != nil {
			t.Errorf("a loop with no dispatch tool handed the model loop %v", req.Dispatch)
		}
		return answered(req, "Plum."), nil
	}
	plain.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	plain.drain()
	if q := plain.only(); q.State != store.QuestionPosted {
		t.Fatalf("question = %s; want posted with gh alone", q.State)
	}
}

// The dispatch tool an answer opens is handed where the loop posts that answer's reply
// (dispatchtool.Place), so the tool refuses a write the model aims there: for a reply in a comment
// thread, a person's reply on the agent's ask, their answer to the ask and a mention, the ask and
// the comments the hand-off names, and every comment the thread held when the answer started,
// read once, the agent's own earlier comment among them: a mention under an ask is handed that ask
// and its replies, and a mention that is a thread's root the thread read by its own id; for a
// message, the message; and for an Envoy message, the session that sent it. A thread read that
// fails costs the answer nothing: the tool is handed the hand-off's comments, and one warning names
// the question and the error.
func TestTheDispatchToolIsHandedWhereTheAnswerIsPosted(t *testing.T) {
	const unreadRoot = "c0000000-0000-4000-8000-000000000006"
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	tools := &fakeDispatchTools{}
	r.cfg.DispatchTools = tools
	var logs bytes.Buffer
	r.cfg.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	r.restart()
	agent, someone := map[string]any{"kind": "session", "id": "ses_tester"}, map[string]any{"kind": "user", "id": person.ID}
	r.thread["replies"] = []any{map[string]any{"id": "c-own", "author": agent, "body": "Tested B too."}, map[string]any{"id": "c-3", "author": someone, "body": "Why B?"}}
	r.ask["replies"] = []any{map[string]any{"id": "c-ask-own", "author": agent, "body": "A failed first."}, map[string]any{"id": "c-2", "author": someone, "body": "Why A?"}}
	// Dispatch holds no comment at an empty id: a thread read naming none reads nothing.
	r.unreadable = map[string]bool{"/api/v1/comments/" + unreadRoot: true, "/api/v1/comments/": true}
	// mention is a comment mentioning the agent: a reply in root's thread, a reply under ask, or,
	// with neither, a thread's root.
	mention := func(id, root, ask string) intake.Handoff {
		payload := map[string]any{"id": id, "issue_key": "LEGION-9", "body": "@tester why?"}
		if root != "" {
			payload["reply_to"] = root
		}
		if ask != "" {
			payload["ask_id"] = ask
		}
		return dispatchHandoff("ses_tester", frame("comment.created", person, new("LEGION-9"), payload,
			map[string]any{"attempt": 2, "mode": "btw", "comment_id": id, "target": "role:" + string(testerToken)}))
	}
	askReply := dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-2", "issue_key": "LEGION-9", "body": "Why A?", "ask_id": askID}))
	askAnswer := dispatchHandoff("ses_tester", answeredEvent(34, person, askID, "question", []string{"B"}, new("Use B. Why A?")))
	for _, h := range []intake.Handoff{threadReplyHandoff("ses_tester", "c-3", "Why B?"), askReply, askAnswer, mention("c-1", threadRoot, ""),
		mention("c-6", unreadRoot, ""), mention("c-7", "", askID), mention("c-8", "", ""), messageHandoff("ses_tester", "m-1", 1, "Why?"),
		envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-9", "agent.ses_tester.k9", "Which?")} {
		r.receive(h)
	}
	r.drain()
	// The rig answers every thread read with its one thread, threadRoot's, the root mention c-8's too.
	want := map[string]dispatchtool.Place{
		"dispatch:c-3":        {Comments: []string{"c-3", threadRoot, "c-own"}},
		"dispatch:c-2":        {Ask: askID, Comments: []string{"c-2", "c-ask-own"}},
		"dispatch:34":         {Ask: askID, Comments: []string{"c-ask-own", "c-2"}},
		"dispatch:c-1":        {Comments: []string{"c-1", threadRoot, "c-own", "c-3"}},
		"dispatch:c-6":        {Comments: []string{"c-6", unreadRoot}},
		"dispatch:c-7":        {Ask: askID, Comments: []string{"c-7", "c-ask-own", "c-2"}},
		"dispatch:c-8":        {Comments: []string{"c-8", threadRoot, "c-own", "c-3"}},
		"dispatch:m-1":        {Message: "m-1"},
		"agent.ses_tester.k9": {Session: "ses_asker"},
	}
	tools.mu.Lock()
	defer tools.mu.Unlock()
	questions := r.questions()
	if len(questions) != len(want) {
		t.Fatalf("%d questions; want %d, one of each kind", len(questions), len(want))
	}
	unread := ""
	for _, q := range questions {
		if q.State != store.QuestionPosted {
			t.Errorf("question %s is %s; want posted", q.Identity, q.State)
		}
		if got, ok := tools.places[q.ID]; !ok || !reflect.DeepEqual(got, want[q.Identity]) {
			t.Errorf("the dispatch tool for %s (%s) was handed %+v (opened %t); want %+v", q.Identity, q.Kind, got, ok, want[q.Identity])
		}
		if q.Identity == "dispatch:c-6" {
			unread = q.ID
		}
	}
	var warned []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("log line %s: %v", line, err)
		}
		if msg, _ := record["msg"].(string); strings.HasPrefix(msg, "answers: the thread the answer is posted in was not read") {
			warned = append(warned, record)
		}
	}
	if len(warned) != 1 || warned[0]["level"] != "WARN" || warned[0]["question"] != unread ||
		!strings.Contains(fmt.Sprint(warned[0]["error"]), unreadRoot) {
		t.Errorf("the failed thread read logged %v; want one warning naming question %s and the error reading %s", warned, unread, unreadRoot)
	}
}

// A question the daemon stopped while a dispatch call ran is answered again on restart with that
// call recorded as of unknown outcome, in the dispatch tool's own words, and never looked up on
// GitHub, which holds nothing of Dispatch's: the gh call of unknown outcome beside it is looked up
// as before.
func TestARestartRecordsAnUnknownDispatchCallAndLooksNothingUp(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Comment on LEGION-12."))
	q := r.only()
	q.State, q.UpdatedAt = store.QuestionGenerating, testNow
	if err := r.st.UpdateQuestion(r.ctx, q, store.QuestionReceived); err != nil {
		t.Fatal(err)
	}
	for _, call := range []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "LEGION-12", "--body", "Plum." + dispatchtool.Mark(q.ID, 1)}},
		{Seq: 2, Tool: store.ToolGH, Args: []string{"pr", "comment", "7", "--body", "Done."}, App: "review", Owner: "o"},
	} {
		call.Question, call.StartedAt = q.ID, testNow
		if err := r.st.StartAnswerToolCall(r.ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	r.tool.found[2] = ghtool.Found{Exists: true, URL: "https://github.com/o/r/pull/7#issuecomment-5", ID: 5}
	r.restart()
	r.drain()
	if r.answerCount() != 1 {
		t.Fatalf("answers = %d; want the question answered again once", r.answerCount())
	}
	earlier := r.requests[0].Earlier
	if len(earlier) != 2 || earlier[0].Tool != store.ToolDispatch || earlier[0].Result == nil || *earlier[0].Result != dispatchtool.UnknownOutcome ||
		earlier[1].Result == nil || !strings.Contains(*earlier[1].Result, "issuecomment-5") {
		t.Fatalf("earlier calls = %+v; want the dispatch call of unknown outcome and the gh comment found on GitHub", earlier)
	}
	if call, _, _ := r.st.AnswerToolCall(r.ctx, q.ID, 1); call.Result == nil || *call.Result != dispatchtool.UnknownOutcome || call.Exit != nil {
		t.Fatalf("call 1 = %+v; want its unknown outcome recorded as its result, no exit", call)
	}
	if looked := r.tool.lookedUp(); !slices.Equal(looked, []int{2}) {
		t.Fatalf("the gh tool looked up calls %v; want the gh call alone", looked)
	}
}

// An answer that passed a bound after it ran writes is the daemon's reply naming each, the
// dispatch writes beside the gh ones: every dispatch call that ran a command but a read, and no
// read and no call the tool refused.
func TestAFaultedAnswerNamesItsDispatchWrites(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.answer = func(req answer.Request) (answer.Reply, error) {
		zero, one := 0, 1
		refused := "refused: `dispatch claim` is none of the commands this tool runs"
		for _, call := range []store.AnswerToolCall{
			{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "merge", "7", "-R", "acme/widgets", "--squash"}, App: "implement", Owner: "acme", Exit: &zero},
			{Seq: 2, Tool: store.ToolDispatch, Args: []string{"read", "LEGION-12"}, Exit: &zero},
			{Seq: 3, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "LEGION-12", "--body", "Merged."}, Exit: &zero},
			{Seq: 4, Tool: store.ToolDispatch, Args: []string{"claim", "--issue", "LEGION-12"}},
			{Seq: 5, Tool: store.ToolDispatch, Args: []string{"message", "--issue", "LEGION-12", "--body", "x"}, Exit: &one},
		} {
			call.Question, call.StartedAt = req.Question.ID, testNow
			if err := r.st.StartAnswerToolCall(r.ctx, call); err != nil {
				return answer.Reply{}, err
			}
			result := "done"
			if call.Seq == 4 {
				result = refused
			}
			if err := r.st.FinishAnswerToolCall(r.ctx, req.Question.ID, call.Seq, result, call.Exit, testNow); err != nil {
				return answer.Reply{}, err
			}
		}
		return answer.Reply{Kind: answer.NotAnswered, Fault: answer.ModelCalls, CostUSD: 0.25}, nil
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Merge PR 7 and say so on LEGION-12?"))
	r.drain()
	const reply = "I could not answer within the bound of 8 model calls. I had already run these gh writes: `gh pr merge` on acme/widgets (exit 0). " +
		"I had already run these Dispatch writes: `dispatch comment` (exit 0); `dispatch message` (exit 1). I passed your question to the Legion controller."
	if posts := r.dispatch.posts(); len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), reply+"\n\n"+AnswerMarker) {
		t.Fatalf("posts = %s; want one reply naming the merge, the comment and the message: %q", describe(posts), reply)
	}
}

// A question forwarded to the running agent names each call the loop already made with its tool,
// in its text and its payload, so a dispatch write is never read as a gh one.
func TestAForwardedQuestionNamesEachCallsTool(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.answer = func(req answer.Request) (answer.Reply, error) {
		zero := 0
		if err := r.st.StartAnswerToolCall(r.ctx, store.AnswerToolCall{Question: req.Question.ID, Seq: 1, Tool: store.ToolDispatch,
			Args: []string{"comment", "--issue", "LEGION-12", "--body", "Done."}, StartedAt: testNow}); err != nil {
			t.Error(err)
		}
		if err := r.st.FinishAnswerToolCall(r.ctx, req.Question.ID, 1, "comment c-9 posted", &zero, testNow); err != nil {
			t.Error(err)
		}
		r.gate.set(r.gate.started, "ses_tester", true)
		return answered(req, "Commented."), nil
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Please comment."))
	r.drain()
	publishes := r.listener.posts()
	if len(publishes) != 1 {
		t.Fatalf("listener posts = %s; want the forward", describe(publishes))
	}
	var payload forwardPayload
	if err := json.Unmarshal([]byte(publishes[0].Body["payload"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Calls) != 1 || payload.Calls[0].Tool != store.ToolDispatch ||
		!strings.Contains(payload.Text, "\n1. dispatch [\"comment\",\"--issue\",\"LEGION-12\",\"--body\",\"Done.\"]\ncomment c-9 posted\n") {
		t.Fatalf("forward payload = %+v; want the dispatch call named as dispatch's", payload)
	}
}
