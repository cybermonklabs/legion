package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/logtest"
)

// plainWords is n words of stem, stem0 to stem<n-1>, apart by spaces: a reply the fake server
// counts as n words.
func plainWords(stem string, n int) string {
	words := make([]string, n)
	for at := range words {
		words[at] = fmt.Sprintf("%s%d", stem, at)
	}
	return strings.Join(words, " ")
}

// toldLong is the start of what the model is told after a reply held back as long (longTold).
const toldLong = "The person did not see your reply above"

// lengthScript answers each step's call of a run with first until the model is told a reply of
// the turn was held back as long, and with second from then on; it passes every check.
func lengthScript(first, second string) func(*agenttest.Request) agenttest.Reply {
	return func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			return verdict(noPlanPasses)
		case strings.Contains(request.Text(), toldLong):
			return agenttest.Reply{Text: second}
		}
		return agenttest.Reply{Text: first}
	}
}

// madeWordsCalls is the words calls the run made of the server.
func (s *fakeServer) madeWordsCalls() []agentwire.CallRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentwire.CallRequest(nil), s.wordsCalls...)
}

// savedNotes is the note of each model step the run saved, in order.
func (s *fakeServer) savedNotes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var notes []string
	for _, step := range s.steps {
		if step.Kind == agentwire.StepModel {
			notes = append(notes, step.Note)
		}
	}
	return notes
}

// drawn is a reply frame's count of the words the bar draws, -1 where it carries none.
func drawn(reply replyFrame) int {
	if reply.DrawnWords == nil {
		return -1
	}
	return *reply.DrawnWords
}

// TestAnAnswerUnderTheBoundEndsTheTurnAsItIs: an answer of the command bar's agent has fewer than
// wordsBound words as the bar draws them, its chips' words counted. Before a reply in
// prose ends the turn done, the agent asks the server how many words the bar draws of it, with
// one words call of the run under its lease that records nothing (step and call 0); a reply under
// the bound ends the turn done as it is, with no other model call than its step's and the
// guard's check, and its frame says the count.
func TestAnAnswerUnderTheBoundEndsTheTurnAsItIs(t *testing.T) {
	prose := "[ACME-7](dispatch://ACME-7) waits on " + plainWords("word", 10)
	fake := agenttest.New(t, lengthScript(prose, "a second turn"))
	server, client := newFakeServer(t, keptPlan)
	server.words = func(agentwire.CallRequest) agentwire.CallAnswer { return countedWords(wordsBound - 1) }
	workRun(t, fake.Model(), client, "what does ACME-7 wait on")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose || bodies[0].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %+v; want done with the answer as written", end.State, end.Reason, bodies)
	}
	if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 1 || checks != 1 || fake.Count() != 2 {
		t.Errorf("the run made %d steps' calls and %d checks of %d model calls, want the answer and the guard's check alone", steps, checks, fake.Count())
	}
	if frame := server.lastReply(t); frame.Text != prose || frame.HeldLong || drawn(frame) != wordsBound-1 {
		t.Errorf("the answer's frame carries %q (held long %t) and %d drawn words, want the answer and %d", frame.Text, frame.HeldLong, drawn(frame), wordsBound-1)
	}
	calls := server.madeWordsCalls()
	if len(calls) != 1 || calls[0].Text != prose || calls[0].Run != "run-1" || calls[0].LeaseToken != "lease-1" || calls[0].Step != 0 || calls[0].Call != 0 {
		t.Errorf("the run made the words calls %+v, want one of the answer, of run-1 under its lease, at step 0 and call 0", calls)
	}
}

// TestAnAnswerOfJustTheBoundIsHeldBack: an answer has fewer than wordsBound words as the bar
// draws them, so a reply the bar draws in wordsBound words exactly is long, and is held back as one
// of more words is: saved with its count, its frame marked and with no prose, and the next step's
// model told how many words the person sees. The next, shorter reply ends the turn done.
func TestAnAnswerOfJustTheBoundIsHeldBack(t *testing.T) {
	bound, short := plainWords("bound", wordsBound), plainWords("short", 30)
	fake := agenttest.New(t, lengthScript(bound, short))
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, "what is the state of ACME-7's open children")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].Text != bound || bodies[0].LongWords != wordsBound ||
		bodies[1].Text != short || bodies[1].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %+v; want done, the reply of %d words held back with its count and then the short one", end.State, end.Reason, bodies, wordsBound)
	}
	if steps := stepRequests(fake); len(steps) != 2 || !toldAfter(steps[1], bound, longTold(wordsBound)) {
		t.Errorf("the run made %d steps' calls, want the second given the held reply and, after it, the note of its %d words", len(steps), wordsBound)
	}
	replies := server.replyFrames(t)
	if len(replies) != 2 || !replies[0].HeldLong || replies[0].Text != "" || drawn(replies[0]) != wordsBound {
		t.Fatalf("the reply frames are %+v, want the held reply's first, marked, with no prose and its %d words", replies, wordsBound)
	}
	if replies[1].Text != short || replies[1].HeldLong || drawn(replies[1]) != 30 {
		t.Errorf("the answer's frame carries %q (held long %t) and %d drawn words, want the short reply and 30", replies[1].Text, replies[1].HeldLong, drawn(replies[1]))
	}
}

// TestALongAnswerIsHeldBackAndToldHowManyWordsThePersonSees: at 8a3e562b, 6 of 11 answers to "What
// is the state of this issue's open children?" drew 120 words or more. A reply the bar draws in
// wordsBound words or more, whose words ask for no more, is held back: saved with its
// count, so the model reads what it wrote, its note saying the person did not see it, and its
// frame marked and with no prose, so the bar adds nothing of it. The next step's model is told,
// after it, how many words the person sees and to give the same facts in at most 90 words as the
// person sees them, with no repeat of what each link already shows, and its shorter reply ends the
// turn done as the answer. A turn whose words Jev reads as Ask, which makes no check, is held to
// the bound too.
func TestALongAnswerIsHeldBackAndToldHowManyWordsThePersonSees(t *testing.T) {
	const words = "what is the state of ACME-7's open children"
	long, short := plainWords("long", 130), plainWords("short", 30)
	for name, work := range map[string]func(*testing.T, agentwire.Client) *agenttest.Model{
		"a checked reply": func(t *testing.T, client agentwire.Client) *agenttest.Model {
			fake := agenttest.New(t, lengthScript(long, short))
			workRun(t, fake.Model(), client, words)
			return fake
		},
		"a turn Jev reads as Ask": func(t *testing.T, client agentwire.Client) *agenttest.Model {
			return workAfterJev(t, client, jevRun(agentwire.LineAsk, words), lengthScript(long, short))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, client := newFakeServer(t, answeredLine(agentwire.LineAsk))
			fake := work(t, client)
			end, bodies := server.end(t)
			if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].Text != long || bodies[0].LongWords != 130 || bodies[0].NoPlan ||
				bodies[1].Text != short || bodies[1].LongWords != 0 {
				t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the long reply held back with its 130 words and then the short one", end.State, end.Reason, len(bodies))
			}
			steps := stepRequests(fake)
			if len(steps) != 2 || !toldAfter(steps[1], long, longTold(130)) {
				t.Fatalf("the run made %d steps' calls, want the second given the long reply and, after it, the note of its 130 words", len(steps))
			}
			// What the second step's model reads: the count the bar drew, the target of the retry, which
			// stands below the bound of 120, and the links' key, status and title the prose leaves out.
			for _, want := range []string{
				"the bar draws it in 130 words",
				"give the same facts in at most 90 words as the person sees them",
				"each link already shows its item's key, status and title, so do not repeat them",
			} {
				if !toldAfter(steps[1], long, want) {
					t.Errorf("the second step's model is told %v after the long reply, want %q", steps[1].Messages(), want)
				}
			}
			replies := server.replyFrames(t)
			if len(replies) != 2 || !replies[0].HeldLong || replies[0].Text != "" || drawn(replies[0]) != 130 {
				t.Fatalf("the reply frames are %+v, want the held reply's first, marked, with no prose and its 130 words", replies)
			}
			if replies[1].Text != short || replies[1].HeldLong || drawn(replies[1]) != 30 {
				t.Errorf("the answer's frame carries %q (held long %t) and %d drawn words, want the short reply and 30", replies[1].Text, replies[1].HeldLong, drawn(replies[1]))
			}
			if notes := server.savedNotes(); len(notes) != 2 || !strings.HasPrefix(notes[0], longSaid+" long0 long1") {
				t.Errorf("the run saved the notes %q, want the held reply's to say the person did not see it", notes)
			}
		})
	}
}

// TestAnAnswerStillLongAfterItsTurnEndsWhole: one reply of a turn at most is held back as long. A
// reply still of wordsBound words or more after that turn ends the turn done, shown
// whole, never cut, and no third step is made.
func TestAnAnswerStillLongAfterItsTurnEndsWhole(t *testing.T) {
	first, still := plainWords("first", 150), plainWords("still", 140)
	fake := agenttest.New(t, lengthScript(first, still))
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, "what did the southern team write about the comet")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].LongWords != 150 || bodies[1].Text != still || bodies[1].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the first reply held back and the second saved whole", end.State, end.Reason, len(bodies))
	}
	if frame := server.lastReply(t); frame.Text != still || frame.HeldLong || drawn(frame) != 140 {
		t.Errorf("the answer's frame carries %d bytes (held long %t) and %d drawn words, want the 140 words whole", len(frame.Text), frame.HeldLong, drawn(frame))
	}
	if steps := stepRequests(fake); len(steps) != 2 {
		t.Errorf("the run made %d steps' calls, want 2: no third turn", len(steps))
	}
}

// standardRun is a Standard run of words, whose steps ask for the level's medium effort and are
// offered its tools, unlike its rewrite, and whose 5 minutes of working time stand far past the
// rewrite's bound.
func standardRun(words string) *agentwire.ClaimedRun {
	return &agentwire.ClaimedRun{ID: "run-1", Words: words, Level: "standard", Mode: "ask", LeaseToken: "lease-1"}
}

// messageChars is the characters of the text of each message request sends, its system prompt's
// among them: what the pacer's estimate counts of a call whose messages hold no tool call and
// which sends no tool (going.PromptChars).
func messageChars(request *agenttest.Request) int {
	chars := 0
	for _, message := range request.Messages() {
		switch content := message["content"].(type) {
		case string:
			chars += len(content)
		case []any:
			for _, part := range content {
				if part, ok := part.(map[string]any); ok {
					text, _ := part["text"].(string)
					chars += len(text)
				}
			}
		}
	}
	return chars
}

// requireHeldShown fails t unless the run ended its turn done with held, a reply the bar draws in
// words words, held back as long and then, its rewrite drawing no reply it shows, shown whole:
// saved held back with its count and again whole as the answer, whose frame carries its prose and
// its count, is not marked held and names no call, after the steps' calls of the held reply and
// its rewrite alone and the held reply's one check.
func requireHeldShown(t *testing.T, server *fakeServer, fake *agenttest.Model, held string, words int) {
	t.Helper()
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || end.Reason != "" || len(bodies) != 2 || bodies[0].Text != held || bodies[0].LongWords != words ||
		bodies[1].Text != held || bodies[1].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the reply held back with its %d words and then shown whole", end.State, end.Reason, len(bodies), words)
	}
	replies := server.replyFrames(t)
	if len(replies) != 2 || !replies[0].HeldLong || replies[0].Text != "" {
		t.Fatalf("the reply frames are %+v, want the held reply's first, marked and with no prose", replies)
	}
	if shown := replies[1]; shown.Text != held || shown.HeldLong || drawn(shown) != words || len(shown.Tools) != 0 || shown.WaitsForPerson {
		t.Errorf("the answer's frame carries %d bytes (held long %t) and %d drawn words, with the calls %q; want the held reply whole, its %d words and no call",
			len(shown.Text), shown.HeldLong, drawn(shown), shown.Tools, words)
	}
	if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 2 || checks != 1 {
		t.Errorf("the run made %d steps' calls and %d checks, want the held reply's and its rewrite's, and the held reply's one check", steps, checks)
	}
}

// TestTheRewriteIsOfferedNoToolAtTheLowEffort: the turn after a reply held back as long is a
// rewrite, not a new run, as the architect ruled
// (dispatch://INFRA-1588/comment/7c38ba3b-bde1-4bb4-bcc7-f966b0968b8c) once such a turn at the
// run's level wrote 5,526 to 17,565 reasoning tokens to shorten a reply. A Standard run's first
// step is offered its tools at the level's effort; its rewrite's request sends no tool and no
// tool choice and asks for rewriteEffort, the lowest effort the chat model's calls ask for, at the
// level's output tokens still; and the pacer reserves for it its prompt, the level's output
// tokens and rewriteReasoningTokens, no tool and not the level's reasoning tokens. Its reply ends
// the turn done as the answer.
func TestTheRewriteIsOfferedNoToolAtTheLowEffort(t *testing.T) {
	long, short := plainWords("long", 130), plainWords("short", 30)
	fake := agenttest.New(t, lengthScript(long, short))
	server, client := newFakeServer(t, keptPlan)
	workClaimed(t, fake.Model(), client, standardRun("what is the state of ACME-7's open children"))
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].LongWords != 130 || bodies[1].Text != short || bodies[1].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the long reply held back and then the rewrite's", end.State, end.Reason, len(bodies))
	}
	steps := stepRequests(fake)
	if len(steps) != 2 || !toldAfter(steps[1], long, longTold(130)) {
		t.Fatalf("the run made %d steps' calls, want the second, the rewrite, given the long reply and the note of its 130 words", len(steps))
	}
	standard := Levels["standard"]
	if effort, tools := steps[0].Field("reasoning_effort"), steps[0].Field("tools"); effort != standard.Effort || tools == nil {
		t.Errorf("the first step asks for effort %v with the tools %v, want the Standard level's %s and its tools", effort, tools, standard.Effort)
	}
	rewrite := steps[1]
	if effort, tools, choice := rewrite.Field("reasoning_effort"), rewrite.Field("tools"), rewrite.Field("tool_choice"); effort != rewriteEffort || tools != nil || choice != nil {
		t.Errorf("the rewrite asks for effort %v with the tools %v and the tool choice %v, want %s and no tool", effort, tools, choice, rewriteEffort)
	}
	if tokens := rewrite.Field("max_tokens"); tokens != float64(standard.OutputTokens) {
		t.Errorf("the rewrite asks for max_tokens %v, want the level's %d", tokens, standard.OutputTokens)
	}
	chars := messageChars(rewrite)
	want := chars/charsPerToken + standard.OutputTokens + rewriteReasoningTokens
	if reserved := server.lastReply(t).ReservedTokens; reserved != want {
		t.Errorf("the pacer reserved %d tokens for the rewrite, want %d: a quarter of the %d characters of its messages, no tool, the level's %d output tokens and the %d reasoning tokens of %s effort",
			reserved, want, chars, standard.OutputTokens, rewriteReasoningTokens, rewriteEffort)
	}
}

// TestARewritePastItsBoundShowsTheHeldReplyWhole: the rewrite has a bound of its own, the 20 s the
// architect ruled, within what is left of the run's working time. A rewrite that draws no reply
// within it, from a chat model that answers nothing, ends the turn done with the held reply shown
// whole, cut nowhere, and the log says what happened. A Standard run works up to 5 minutes, so the
// test's deadline, far under that, stops a rewrite that waits on past its own bound, and the test
// fails rather than hangs.
func TestARewritePastItsBoundShowsTheHeldReplyWhole(t *testing.T) {
	if rewriteCallTime != 20*time.Second {
		t.Errorf("the rewrite is bounded at %s, want the 20s the architect ruled", rewriteCallTime)
	}
	const bound, deadline = 200 * time.Millisecond, 5 * time.Second
	was := rewriteCallTime
	rewriteCallTime = bound
	t.Cleanup(func() { rewriteCallTime = was })
	var log logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
	long := plainWords("long", 150)
	// silent is never closed: the rewrite's call is answered nothing until its client leaves it.
	silent := make(chan struct{})
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			return verdict(noPlanPasses)
		case strings.Contains(request.Text(), toldLong):
			return agenttest.Reply{Hold: silent}
		}
		return agenttest.Reply{Text: long}
	})
	server, client := newFakeServer(t, keptPlan)
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	a := &agent{client: client, config: Config{Model: fake.Model()}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
	newWorker(ctx, a, standardRun("what did the southern team write about the comet"), func() {}).work()
	if ctx.Err() != nil {
		t.Fatalf("the run still waited on its rewrite at the test's deadline of %s, want its turn ended within the rewrite's bound of %s", deadline, bound)
	}
	requireHeldShown(t, server, fake, long, 150)
	warned := log.Lines("the rewrite of a reply held back as long drew no reply")
	if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, bound.String()) {
		t.Errorf("the log holds %q, want a warning naming run-1 and the rewrite's bound", warned)
	}
}

// TestARewriteThatFailsShowsTheHeldReplyWhole: a rewrite whose call fails, the chat model
// answering 400, which is not tried again, ends the turn done with the held reply shown whole, as
// one past its bound does, not the run failed, and the log says what the chat model answered.
func TestARewriteThatFailsShowsTheHeldReplyWhole(t *testing.T) {
	var log logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
	long := plainWords("long", 140)
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			return verdict(noPlanPasses)
		case strings.Contains(request.Text(), toldLong):
			return agenttest.Reply{Status: http.StatusBadRequest}
		}
		return agenttest.Reply{Text: long}
	})
	server, client := newFakeServer(t, keptPlan)
	workClaimed(t, fake.Model(), client, standardRun("what did the southern team write about the comet"))
	requireHeldShown(t, server, fake, long, 140)
	warned := log.Lines("the rewrite of a reply held back as long drew no reply")
	if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, "answered 400") {
		t.Errorf("the log holds %q, want a warning naming run-1 and the status the chat model answered", warned)
	}
}

// TestTheRewriteIsGivenTheWordsTheHeldReplyAndTheNoteAlone: at 52f1406a the rewrite was given the
// run's whole conversation, and ran past its 20 s bound in 2 of 2 tries of the real model, so the
// architect ruled (dispatch://INFRA-1588/comment/8a23ec38-289e-44c4-88a0-b332fd3f5453) that it is
// given a short system message of the answer's rules, the person's words, the held reply and the
// note of its count alone. A Standard run asked on an issue's page after an earlier turn of the
// conversation reads the page's issue, then reads the issue's children in a step of its own, and
// writes a long reply, given all of that. Its rewrite's request holds rewritePrompt as its system
// message, then the person's words as every step gives them, the held reply as the model's own and
// longTold after it, and nothing else - no earlier turn, no page, no read, no earlier step - and
// sends no tool and no tool choice, at rewriteEffort.
func TestTheRewriteIsGivenTheWordsTheHeldReplyAndTheNoteAlone(t *testing.T) {
	const words, earlier = "what is the state of ACME-7's open children", "who holds ACME-7"
	long, short := "[ACME-7](dispatch://ACME-7) waits on "+plainWords("long", 130), plainWords("short", 30)
	children := agenttest.Call{Name: toolRead, Arguments: map[string]any{"ref": "dispatch://ACME-7/children"}}
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			return verdict(noPlanPasses)
		case strings.Contains(request.Text(), toldLong):
			return agenttest.Reply{Text: short}
		case len(request.ToolResults()) == 0:
			return agenttest.Reply{Calls: []agenttest.Call{children}}
		}
		return agenttest.Reply{Text: long}
	})
	server, client := newFakeServer(t, func(agentwire.CallRequest) agentwire.CallAnswer {
		return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(`{"key":"ACME-7","title":"Comet survey","children":[{"key":"ACME-9"}]}`)}
	})
	run := standardRun(words)
	run.Context = json.RawMessage(`{"path":"/issues/ACME-7","project":"ACME","issue":"ACME-7","block":""}`)
	run.Turns = []agentwire.Turn{{Run: "run-0", Words: earlier, Notes: []string{"Read ACME-7"}}}
	workClaimed(t, fake.Model(), client, run)
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 3 || len(bodies[0].Calls) != 1 || bodies[1].Text != long || bodies[1].LongWords == 0 ||
		bodies[2].Text != short || bodies[2].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the read, the long reply held back and then the rewrite's", end.State, end.Reason, len(bodies))
	}
	steps := stepRequests(fake)
	if len(steps) != 3 {
		t.Fatalf("the run made %d steps' calls, want the read's, the long reply's and the rewrite's", len(steps))
	}
	if given := steps[1].Text(); !strings.Contains(given, earlier) || !strings.Contains(given, "Comet survey") || len(steps[1].ToolResults()) != 1 {
		t.Fatalf("the long reply's step is given %q, want the earlier turn, the page's issue and the read's result", given)
	}
	want := []struct{ role, text string }{
		{"system", rewritePrompt},
		{"user", agenttest.Asked + words + "\n"},
		{"assistant", long},
		{"user", longTold(bodies[1].LongWords)},
	}
	rewrite := steps[2]
	messages := rewrite.Messages()
	if len(messages) != len(want) {
		t.Fatalf("the rewrite is given %d messages, %v; want its system message, the person's words, the held reply and the note of its count alone", len(messages), messages)
	}
	for at, message := range messages {
		if message["role"] != want[at].role || messageText(message) != want[at].text {
			t.Errorf("the rewrite's message %d is %v %q, want %s %q", at, message["role"], messageText(message), want[at].role, want[at].text)
		}
	}
	if effort, tools, choice := rewrite.Field("reasoning_effort"), rewrite.Field("tools"), rewrite.Field("tool_choice"); effort != rewriteEffort || tools != nil || choice != nil {
		t.Errorf("the rewrite asks for effort %v with the tools %v and the tool choice %v, want %s and no tool", effort, tools, choice, rewriteEffort)
	}
}

// TestARewriteThatAddsALinkShowsTheHeldReplyWhole: the rewrite shortens the held reply and adds
// nothing to it, as the architect ruled
// (dispatch://INFRA-1588/comment/8a23ec38-289e-44c4-88a0-b332fd3f5453). A rewrite whose reply
// links an item the held reply does not - by a markdown link, by a bare dispatch:// reference, or
// by an issue key written bare, which the server may store as the issue's link and the bar draw
// as its chip (chips.LinkBareKeys) - ends the turn done with the held reply shown whole, as one
// past its bound does, its own reply neither checked nor counted, and the log names the link.
func TestARewriteThatAddsALinkShowsTheHeldReplyWhole(t *testing.T) {
	held := "[ACME-7](dispatch://ACME-7) waits on " + plainWords("long", 130)
	for name, rewritten := range map[string]string{
		"a markdown link":  "[ACME-7](dispatch://ACME-7) waits on [ACME-9](dispatch://ACME-9) and " + plainWords("short", 20),
		"a bare reference": "[ACME-7](dispatch://ACME-7) waits on dispatch://ACME-9 and " + plainWords("short", 20),
		"a bare key":       "[ACME-7](dispatch://ACME-7) waits on ACME-9 and " + plainWords("short", 20),
	} {
		t.Run(name, func(t *testing.T) {
			var log logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
			fake := agenttest.New(t, lengthScript(held, rewritten))
			server, client := newFakeServer(t, keptPlan)
			workRun(t, fake.Model(), client, "what does ACME-7 wait on")
			requireHeldShown(t, server, fake, held, len(strings.Fields(held)))
			warned := log.Lines("links what the held reply does not")
			if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, "dispatch://ACME-9") {
				t.Errorf("the log holds %q, want a warning naming run-1 and the link dispatch://ACME-9", warned)
			}
		})
	}
}

// TestARewriteThatKeepsItsLinksIsShown: a rewrite whose every link is a link of the held reply ends
// the turn done as the answer, checked as any reply in prose is, however it writes each link: an
// issue by its dashboard path where the held reply wrote its reference, another by an autolink
// where the held reply wrote a markdown link, and a time by the same address, with one of the held
// reply's links left out; an issue key both write bare; and a key the held reply wrote bare, which
// the server may link as it stores the reply, that the rewrite links.
func TestARewriteThatKeepsItsLinksIsShown(t *testing.T) {
	const at = "(dispatch-time:2026-11-18T11:35:00Z)"
	for _, c := range []struct{ name, held, kept string }{
		{
			name: "links written another way",
			held: "[ACME-7](dispatch://ACME-7) waits on [ACME-9](dispatch://ACME-9) and [ACME-12](dispatch://ACME-12), updated [5 days ago]" + at + ": " +
				plainWords("long", 130),
			kept: "[ACME-7](/issues/ACME-7) waits on <dispatch://ACME-9>, updated [5 days ago]" + at + ": " + plainWords("short", 20),
		},
		{
			name: "a key both write bare",
			held: "ACME-7 waits on its review: " + plainWords("long", 130),
			kept: "ACME-7 waits on its review: " + plainWords("short", 20),
		},
		{
			name: "a key the held reply wrote bare, linked",
			held: "ACME-7 waits on its review: " + plainWords("long", 130),
			kept: "[ACME-7](dispatch://ACME-7) waits on its review: " + plainWords("short", 20),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := agenttest.New(t, lengthScript(c.held, c.kept))
			server, client := newFakeServer(t, keptPlan)
			workRun(t, fake.Model(), client, "what does ACME-7 wait on")
			end, bodies := server.end(t)
			if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].Text != c.held || bodies[0].LongWords == 0 || bodies[1].Text != c.kept || bodies[1].LongWords != 0 {
				t.Fatalf("the run ended %s with %q, saving %+v; want done, the reply held back and then the rewrite's", end.State, end.Reason, bodies)
			}
			if frame := server.lastReply(t); frame.Text != c.kept || frame.HeldLong {
				t.Errorf("the answer's frame carries %q (held long %t), want the rewrite's reply", frame.Text, frame.HeldLong)
			}
			if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 2 || checks != 2 {
				t.Errorf("the run made %d steps' calls and %d checks, want the held reply's and its rewrite's, each checked", steps, checks)
			}
		})
	}
}

// TestWordsThatAskForMoreGetTheSameAnswerAgain: no other model call decides whether the person's
// words ask for more than a short answer: the rewrite asks it, in the architect's words
// (dispatch://INFRA-1588/comment/77ddfc7a-b65c-4d36-b78c-c00e703c7d31). Words that ask for all of
// a thing get the same long answer again, which ends the turn done, whole, with no third turn.
func TestWordsThatAskForMoreGetTheSameAnswerAgain(t *testing.T) {
	const ruling = "If the person's words ask for more than a short answer, give the same answer again."
	answer := plainWords("everything", 160)
	fake := agenttest.New(t, lengthScript(answer, answer))
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, "say everything the southern team wrote about the comet, in full")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 2 || bodies[0].LongWords != 160 || bodies[1].Text != answer || bodies[1].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %d model steps; want done, the answer held back once and then given again whole", end.State, end.Reason, len(bodies))
	}
	steps := stepRequests(fake)
	if len(steps) != 2 || !toldAfter(steps[1], answer, ruling) {
		t.Fatalf("the run made %d steps' calls, want the second told after the held answer %q", len(steps), ruling)
	}
	if frame := server.lastReply(t); frame.Text != answer || drawn(frame) != 160 {
		t.Errorf("the answer's frame carries %d bytes and %d drawn words, want the 160 words whole", len(frame.Text), drawn(frame))
	}
}

// TestALongAnswerEndsWholeWhereNoTurnIsLeft: a reply of wordsBound words or more on the
// run's last model call, which leaves no call to say it in fewer, ends the turn done whole; and so
// does one whose words call fails, which holds nothing back and is logged with the run and the
// cause, and one whose words call draws no count within wordsCallTime. The words call is made once
// the step's watchdog stopped, and serverCall waits out a sign-in the issuer cannot confirm until
// the call's context ends, so without a bound of its own a sign-in outage would hold the finished
// answer until the run stopped.
func TestALongAnswerEndsWholeWhereNoTurnIsLeft(t *testing.T) {
	long := plainWords("long", 150)
	requireWhole := func(t *testing.T, server *fakeServer, fake *agenttest.Model, words int) {
		t.Helper()
		end, bodies := server.end(t)
		if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != long || bodies[0].LongWords != 0 {
			t.Fatalf("the run ended %s with %q, saving %d model steps; want done with the long reply whole", end.State, end.Reason, len(bodies))
		}
		if frame := server.lastReply(t); frame.Text != long || frame.HeldLong || drawn(frame) != words {
			t.Errorf("the answer's frame carries %d bytes (held long %t) and %d drawn words, want the reply whole and %d", len(frame.Text), frame.HeldLong, drawn(frame), words)
		}
		if steps := stepRequests(fake); len(steps) != 1 {
			t.Errorf("the run made %d steps' calls, want the one", len(steps))
		}
	}

	t.Run("the level leaves no model call", func(t *testing.T) {
		fake := agenttest.New(t, lengthScript(long, "a shorter answer"))
		server, client := newFakeServer(t, keptPlan)
		workClaimed(t, fake.Model(), client, &agentwire.ClaimedRun{ID: "run-1", Words: "what did the team write", Level: "fast", Mode: "ask",
			LeaseToken: "lease-1", BudgetUsed: agentwire.Budget{Calls: Levels["fast"].Calls - 1}})
		requireWhole(t, server, fake, 150)
	})

	t.Run("the words call fails", func(t *testing.T) {
		var log logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
		fake := agenttest.New(t, lengthScript(long, "a shorter answer"))
		server, client := newFakeServer(t, keptPlan)
		server.words = func(agentwire.CallRequest) agentwire.CallAnswer {
			return agentwire.CallAnswer{Status: http.StatusInternalServerError, Body: []byte(`{"code":"INTERNAL"}`)}
		}
		workRun(t, fake.Model(), client, "what did the team write")
		requireWhole(t, server, fake, -1)
		warned := log.Lines("the words call of a reply in prose failed")
		if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, "status 500") {
			t.Errorf("the log holds %q, want a warning naming run-1 and the status the call answered", warned)
		}
	})

	t.Run("the words call draws no count within its bound", func(t *testing.T) {
		const bound, deadline = 200 * time.Millisecond, 5 * time.Second
		was := wordsCallTime
		wordsCallTime = bound
		t.Cleanup(func() { wordsCallTime = was })
		var log logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
		fake := agenttest.New(t, lengthScript(long, "a shorter answer"))
		server, client := newFakeServer(t, keptPlan)
		server.refuseWords = &agentwire.Error{Code: agentwire.CodeSignInUnavailable, Message: "the sign-in issuer did not answer"}
		// The test's own deadline stops a run whose words call waits on past its bound, so the
		// test fails rather than hangs.
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		defer cancel()
		a := &agent{client: client, config: Config{Model: fake.Model()}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
		newWorker(ctx, a, &agentwire.ClaimedRun{ID: "run-1", Words: "what did the team write", Level: "fast", Mode: "ask", LeaseToken: "lease-1"}, func() {}).work()
		if ctx.Err() != nil {
			t.Fatalf("the run still waited on its words call at the test's deadline of %s, want its turn ended within the call's bound of %s", deadline, bound)
		}
		requireWhole(t, server, fake, -1)
		if calls := server.madeWordsCalls(); len(calls) == 0 {
			t.Errorf("the run made no words call, want one refused %s", agentwire.CodeSignInUnavailable)
		}
		warned := log.Lines("the words call of a reply in prose failed")
		if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, "no count within "+bound.String()) {
			t.Errorf("the log holds %q, want a warning naming run-1 and the call's bound", warned)
		}
	})
}

// TestARunRestartedAfterALongReplyGoesOn: a worker that starts from saved steps whose last is a
// reply held back as long does not read it as the end of the turn: it makes the next step, whose
// model is given the reply and how many words the person would see. The turn already held a reply
// back as long, so a long reply after the restart ends the turn whole; a held reply older than
// the history's whole turns is read so by its note.
func TestARunRestartedAfterALongReplyGoesOn(t *testing.T) {
	held, again := plainWords("held", 140), plainWords("again", 150)
	body, err := json.Marshal(modelBody{Text: held, Calls: []toolCall{}, LongWords: 140})
	if err != nil {
		t.Fatal(err)
	}
	for name, step := range map[string]agentwire.SavedStep{
		"a held reply of the whole turns": {Step: 1, Kind: agentwire.StepModel, Body: body, Note: note(longSaid + " " + held)},
		"a held reply read by its note":   {Step: 1, Kind: agentwire.StepModel, Note: note(longSaid + " " + held)},
	} {
		t.Run(name, func(t *testing.T) {
			fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
				if isCheck(request) {
					return verdict(noPlanPasses)
				}
				return agenttest.Reply{Text: again}
			})
			server, client := newFakeServer(t, keptPlan)
			workClaimed(t, fake.Model(), client, &agentwire.ClaimedRun{ID: "run-1", Words: "what did the team write", Level: "fast", Mode: "ask",
				LeaseToken: "lease-1", BudgetUsed: agentwire.Budget{Calls: 1}, History: []agentwire.SavedStep{step}})
			end, bodies := server.end(t)
			if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != again || bodies[0].LongWords != 0 {
				t.Fatalf("the restarted run ended %s with %q, saving %d model steps; want done with the long reply after the restart whole", end.State, end.Reason, len(bodies))
			}
			steps := stepRequests(fake)
			if len(steps) != 1 {
				t.Fatalf("the restarted run made %d steps' calls, want one", len(steps))
			}
			if len(step.Body) > 0 && !toldAfter(steps[0], held, longTold(140)) {
				t.Errorf("the restarted run's step is given %v, want the held reply and the note of its 140 words after it", steps[0].Messages())
			}
		})
	}
}

// TestAReplyTheGuardFlagsIsNotCounted: the words the bar draws are counted only of a reply that
// would end the turn done as it is: a new-issue run's prose, which the guard holds back with no
// check, and a reply the check flags, held back or ended saying nothing changed, make no words
// call.
func TestAReplyTheGuardFlagsIsNotCounted(t *testing.T) {
	long := plainWords("long", 130)
	t.Run("a new-issue run's prose", func(t *testing.T) {
		file := agenttest.Call{Name: toolProposePlan, Arguments: map[string]any{
			"summary": "File the alert.",
			"changes": []any{map[string]any{"tool": "dispatch_issue", "arguments": map[string]any{"project": "ACME", "title": "The CPU alert is noisy"}}},
		}}
		fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
			if strings.Contains(request.Text(), newIssueNoPlanNote) {
				return agenttest.Reply{Calls: []agenttest.Call{file}}
			}
			return agenttest.Reply{Text: long}
		})
		server, client := newFakeServer(t, keptPlan)
		workClaimed(t, fake.Model(), client, &agentwire.ClaimedRun{ID: "run-1", Words: "the CPU alert is noisy", Kind: agentwire.RunNewIssue,
			Level: "fast", Mode: "ask", LeaseToken: "lease-1"})
		if end, _ := server.end(t); end.State != agentwire.StateWaiting {
			t.Fatalf("the run ended %s with %q, want waiting for the issue it proposed", end.State, end.Reason)
		}
		if calls := server.madeWordsCalls(); len(calls) != 0 {
			t.Errorf("the new-issue run made %d words calls, want none", len(calls))
		}
	})

	t.Run("a reply the check flags", func(t *testing.T) {
		fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
			if isCheck(request) {
				return verdict(noPlanFlagged)
			}
			return agenttest.Reply{Text: long}
		})
		server, client := newFakeServer(t, keptPlan)
		workRun(t, fake.Model(), client, "make ACME-17 wait on ACME-14")
		end, bodies := server.end(t)
		if end.State != agentwire.StateDone || len(bodies) != 2 || !bodies[0].NoPlan || bodies[1].Text != noPlanSaid+"\n\n"+long {
			t.Fatalf("the run ended %s with %q, saving %d model steps; want the reply held back and then ended saying nothing changed", end.State, end.Reason, len(bodies))
		}
		if calls := server.madeWordsCalls(); len(calls) != 0 {
			t.Errorf("the run made %d words calls of replies the check flagged, want none", len(calls))
		}
		if frame := server.lastReply(t); frame.DrawnWords != nil {
			t.Errorf("the flagged answer's frame says %d drawn words, want none", *frame.DrawnWords)
		}
	})
}
