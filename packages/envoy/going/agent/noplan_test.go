package agent

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/logtest"
)

// verdict is the check's answer, as the chat model writes the structured reply it asks for.
func verdict(answer string) agenttest.Reply {
	return agenttest.Reply{Text: `{"verdict":"` + answer + `"}`}
}

// checkRequests is the guard's checks of a reply in prose among fake's requests, in order.
func checkRequests(fake *agenttest.Model) []*agenttest.Request {
	var checks []*agenttest.Request
	for _, request := range fake.Requests() {
		if isCheck(request) {
			checks = append(checks, request)
		}
	}
	return checks
}

// reopenPlan is a propose_plan call of one change, the reopen of ACME-7.
var reopenPlan = agenttest.Call{Name: toolProposePlan, Arguments: map[string]any{
	"summary": "Reopen ACME-7.",
	"changes": []any{map[string]any{"tool": dispatchIssueUpdate, "arguments": map[string]any{"issue": "ACME-7", "status": "todo"}}},
}}

// keptPlan answers every call of the run as the server keeps a plan: plan 1, its rows waiting.
func keptPlan(agentwire.CallRequest) agentwire.CallAnswer {
	return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(`{"plan":1,"rows":[]}`)}
}

// workClaimed works one turn of run on model, at the server client calls.
func workClaimed(t *testing.T, model agentloop.Model, client agentwire.Client, run *agentwire.ClaimedRun) {
	t.Helper()
	a := &agent{client: client, config: Config{Model: model}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
	newWorker(t.Context(), a, run, func() {}).work()
}

// replyFrames is the reply frame of each saved step of the run, in order.
func (s *fakeServer) replyFrames(t *testing.T) []replyFrame {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var replies []replyFrame
	for _, step := range s.steps {
		for _, frame := range step.Frames {
			if frame.Name != frameReply {
				continue
			}
			var reply replyFrame
			if err := json.Unmarshal(frame.Data, &reply); err != nil {
				t.Fatalf("step %d's reply frame does not read: %v", step.Step, err)
			}
			replies = append(replies, reply)
		}
	}
	return replies
}

// messageText is the text of one message of a request (agenttest.MessageText), "" where its
// content is neither a string nor a list of parts.
func messageText(message map[string]any) string {
	text, _ := agenttest.MessageText(message)
	return text
}

// toldAfter reports whether request gives the model prose as its own reply, and the person's
// message right after it holds note.
func toldAfter(request *agenttest.Request, prose, note string) bool {
	messages := request.Messages()
	for at, message := range messages {
		if message["role"] == "assistant" && messageText(message) == prose {
			return at+1 < len(messages) && messages[at+1]["role"] == "user" && strings.Contains(messageText(messages[at+1]), note)
		}
	}
	return false
}

// TestAChangeAnsweredInProseIsHeldBackAndThenProposed: change-16 of the eval, asked to reopen
// OPS-7 as todo and set it to P0, answered on its first model call "Reopening OPS-7 as todo and
// setting it to P0." and proposed no plan, and the run ended done with nothing changed. Such a
// reply is checked before it ends the turn: one call of the agent's chat model, at low effort,
// offered no tool and asked for a structured answer, is given the person's words and the reply,
// written as no step writes the words. The reply it flags is held back: saved, so the model reads
// what it wrote, with the note that nothing changed after it, and its frame carries no prose, so
// the person never reads it. The next step proposes the plan, and the run waits.
func TestAChangeAnsweredInProseIsHeldBackAndThenProposed(t *testing.T) {
	const words = "reopen ACME-7 as todo and set it to P0"
	const prose = "Reopening ACME-7 as todo and setting it to P0."
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			return verdict(noPlanFlagged)
		case strings.Contains(request.Text(), noPlanNote):
			return agenttest.Reply{Calls: []agenttest.Call{reopenPlan}}
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, words)
	end, bodies := server.end(t)
	if end.State != agentwire.StateWaiting {
		t.Fatalf("the run ended %s with %q, want waiting for the plan it proposed once its prose was held back", end.State, end.Reason)
	}
	steps, checks := stepRequests(fake), checkRequests(fake)
	if len(steps) != 2 || len(checks) != 1 {
		t.Fatalf("the run made %d steps' calls and %d checks, want the prose, its one check and the plan", len(steps), len(checks))
	}
	check := checks[0]
	if text := check.Text(); !strings.Contains(text, words) || !strings.Contains(text, prose) || strings.Contains(text, agenttest.Asked) {
		t.Errorf("the check is given %q, want the words and the reply, and not as a step gives the words", text)
	}
	if effort, tools := check.Field("reasoning_effort"), check.Field("tools"); effort != noPlanEffort || tools != nil {
		t.Errorf("the check asks for effort %v with the tools %v, want %s and none", effort, tools, noPlanEffort)
	}
	if tokens := check.Field("max_tokens"); tokens != float64(noPlanOutputTokens) {
		t.Errorf("the check asks for max_tokens %v, want %d", tokens, noPlanOutputTokens)
	}
	if !toldAfter(steps[1], prose, noPlanNote) {
		t.Errorf("the step after the held reply is given %v, want the reply as the model's own and the note that nothing changed after it", steps[1].Messages())
	}
	if len(bodies) != 2 || !bodies[0].NoPlan || bodies[0].Text != prose || bodies[1].NoPlan {
		t.Errorf("the run saved the model steps %+v, want the prose marked held back and then the plan's", bodies)
	}
	replies := server.replyFrames(t)
	if len(replies) != 2 || !replies[0].NoPlan || replies[0].Text != "" {
		t.Fatalf("the reply frames are %+v, want the held reply's first, marked and with no prose", replies)
	}
	for _, reply := range replies {
		if strings.Contains(reply.Text, prose) {
			t.Errorf("the reply frame %+v carries the held prose, which the person would read", reply)
		}
	}
}

// TestAReplyFlaggedTwiceEndsSayingNothingChanged: the guard holds back one reply of a turn. A
// reply flagged again ends the turn done, with the sentence that nothing changed before its text,
// in the saved reply and in the frame the person reads, so the answer never says a change was
// made.
func TestAReplyFlaggedTwiceEndsSayingNothingChanged(t *testing.T) {
	const prose = "ACME-17 and ACME-18 each wait on nothing now. This sets both to wait on ACME-14."
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if isCheck(request) {
			return verdict(noPlanFlagged)
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, "make ACME-17 and ACME-18 wait on ACME-14")
	end, bodies := server.end(t)
	want := noPlanSaid + "\n\n" + prose
	if end.State != agentwire.StateDone || len(bodies) != 2 || !bodies[0].NoPlan || bodies[1].NoPlan || bodies[1].Text != want {
		t.Fatalf("the run ended %s with %q, saving %+v; want done, the first reply held back and the second saved as %q", end.State, end.Reason, bodies, want)
	}
	if frame := server.lastReply(t); frame.Text != want || frame.NoPlan {
		t.Errorf("the answer's frame carries %q (held back %t), want %q", frame.Text, frame.NoPlan, want)
	}
	if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 2 || checks != 2 {
		t.Errorf("the run made %d steps' calls and %d checks, want 2 of each", steps, checks)
	}
}

// TestAReplyFlaggedOnTheLastCallEndsSayingNothingChanged: the run's last model call is offered no
// tool, so no call is left to mend a reply flagged there: it ends the turn done with the sentence
// that nothing changed before it. The check is no model call of the level: the turn spent one.
func TestAReplyFlaggedOnTheLastCallEndsSayingNothingChanged(t *testing.T) {
	const prose = "The planner's ask is answered: column order is saved for each person."
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if isCheck(request) {
			return verdict(noPlanFlagged)
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workClaimed(t, fake.Model(), client, &agentwire.ClaimedRun{ID: "run-1", Words: "answer the planner's ask on ACME-3", Level: "fast", Mode: "ask",
		LeaseToken: "lease-1", BudgetUsed: agentwire.Budget{Calls: Levels["fast"].Calls - 1}})
	end, bodies := server.end(t)
	want := noPlanSaid + "\n\n" + prose
	if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != want || bodies[0].NoPlan {
		t.Fatalf("the run ended %s with %q, saving %+v; want done with the one reply %q", end.State, end.Reason, bodies, want)
	}
	if frame := server.lastReply(t); frame.Text != want {
		t.Errorf("the answer's frame carries %q, want %q", frame.Text, want)
	}
	steps, checks := stepRequests(fake), checkRequests(fake)
	if len(steps) != 1 || steps[0].Field("tool_choice") != "none" || len(checks) != 1 {
		t.Errorf("the run made %d steps' calls, the last with tool_choice %v, and %d checks; want the last call, offered no tool, and its check",
			len(steps), steps[0].Field("tool_choice"), len(checks))
	}
	if end.Used.Calls != 1 {
		t.Errorf("the turn spent %d calls, want 1: the check is no call of the level", end.Used.Calls)
	}
}

// TestAnAnswerTheCheckPassesEndsTheTurnAsItIs: a plain answer to a question is checked, passes,
// and ends the turn done as the model wrote it, with no step after it.
func TestAnAnswerTheCheckPassesEndsTheTurnAsItIs(t *testing.T) {
	const prose = "[ACME-7](dispatch://ACME-7) is assigned to kim@example.com."
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if isCheck(request) {
			return verdict(noPlanPasses)
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, "who is ACME-7 assigned to")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose || bodies[0].NoPlan {
		t.Fatalf("the run ended %s with %q, saving %+v; want done with the answer as written", end.State, end.Reason, bodies)
	}
	if frame := server.lastReply(t); frame.Text != prose || frame.NoPlan {
		t.Errorf("the answer's frame carries %q (held back %t), want %q", frame.Text, frame.NoPlan, prose)
	}
	steps, checks := stepRequests(fake), checkRequests(fake)
	if len(steps) != 1 || len(checks) != 1 || !strings.Contains(checks[0].Text(), prose) {
		t.Errorf("the run made %d steps' calls and %d checks, want the answer and its one check", len(steps), len(checks))
	}
}

// TestACheckedReplysFrameSaysWhatItsCheckTook: the check is one more model call on every turn that
// ends in prose with no plan, so the reply it judged says what it took: its milliseconds, which
// the run's working time counts, and the tokens it spent, its prompt's and its reply's. A reply
// the check holds back says it as a reply the check passes does. The fake takes checkWait over
// each check, and counts each at 120 prompt tokens and 9 reply tokens.
func TestACheckedReplysFrameSaysWhatItsCheckTook(t *testing.T) {
	const checkWait = 20 * time.Millisecond
	const words = "reopen ACME-7 as todo and set it to P0"
	const prose = "Reopening ACME-7 as todo and setting it to P0."
	const answer = "ACME-7 is reopened as todo and set to P0 once you approve a plan for it."
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		switch {
		case isCheck(request):
			// The check's time is what the frame says, so the fake takes a known time over it.
			time.Sleep(checkWait)
			reply := verdict(noPlanPasses)
			if strings.Contains(request.Text(), noPlanText(words, prose)) {
				reply = verdict(noPlanFlagged)
			}
			reply.Prompt, reply.Output = 120, 9
			return reply
		case strings.Contains(request.Text(), noPlanNote):
			return agenttest.Reply{Text: answer}
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workRun(t, fake.Model(), client, words)
	end, _ := server.end(t)
	replies := server.replyFrames(t)
	if end.State != agentwire.StateDone || len(replies) != 2 || !replies[0].NoPlan || replies[1].Text != answer {
		t.Fatalf("the run ended %s with %q and the reply frames %+v, want the prose held back and the answer the check passed", end.State, end.Reason, replies)
	}
	var checked int64
	for i, reply := range replies {
		if reply.CheckMS == nil || reply.CheckTokens == nil {
			t.Fatalf("reply %d's frame says its check took %v ms and %v tokens, want what the check took", i+1, reply.CheckMS, reply.CheckTokens)
		}
		if *reply.CheckMS < checkWait.Milliseconds() || *reply.CheckTokens != 129 {
			t.Errorf("reply %d's frame says its check took %d ms and %d tokens, want at least the fake's %d ms and its 120 prompt and 9 reply tokens",
				i+1, *reply.CheckMS, *reply.CheckTokens, checkWait.Milliseconds())
		}
		checked += *reply.CheckMS
	}
	if end.Used.WorkingMS < checked {
		t.Errorf("the turn worked %d ms, want at least the %d ms its checks took", end.Used.WorkingMS, checked)
	}
}

// TestARunRestartedAfterAHeldReplyGoesOn: a worker that starts from saved steps whose last is a
// reply the guard held back does not read it as the end of the turn: it makes the next step,
// whose model is given the reply and the note that nothing changed. The turn already held a reply
// back, so a reply flagged after the restart ends the turn saying nothing changed.
func TestARunRestartedAfterAHeldReplyGoesOn(t *testing.T) {
	const prose = "Reopening ACME-7 as todo and setting it to P0."
	held, err := json.Marshal(modelBody{Text: prose, Calls: []toolCall{}, NoPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	restarted := func() *agentwire.ClaimedRun {
		return &agentwire.ClaimedRun{ID: "run-1", Words: "reopen ACME-7 as todo and set it to P0", Level: "fast", Mode: "ask", LeaseToken: "lease-1",
			BudgetUsed: agentwire.Budget{Calls: 1},
			History:    []agentwire.SavedStep{{Step: 1, Kind: agentwire.StepModel, Body: held, Note: note(noPlanSaid + " " + prose)}}}
	}

	t.Run("the next step proposes the plan", func(t *testing.T) {
		fake := agenttest.New(t, func(*agenttest.Request) agenttest.Reply { return agenttest.Reply{Calls: []agenttest.Call{reopenPlan}} })
		server, client := newFakeServer(t, keptPlan)
		workClaimed(t, fake.Model(), client, restarted())
		if end, _ := server.end(t); end.State != agentwire.StateWaiting {
			t.Fatalf("the restarted run ended %s with %q, want waiting for the plan its next step proposed", end.State, end.Reason)
		}
		if steps := stepRequests(fake); len(steps) != 1 || !toldAfter(steps[0], prose, noPlanNote) {
			t.Errorf("the restarted run made %d steps' calls, want one given the held reply and the note after it", len(steps))
		}
	})

	t.Run("a reply flagged again ends saying nothing changed", func(t *testing.T) {
		fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
			if isCheck(request) {
				return verdict(noPlanFlagged)
			}
			return agenttest.Reply{Text: prose}
		})
		server, client := newFakeServer(t, keptPlan)
		workClaimed(t, fake.Model(), client, restarted())
		end, bodies := server.end(t)
		if want := noPlanSaid + "\n\n" + prose; end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != want {
			t.Fatalf("the restarted run ended %s with %q, saving %+v; want done with %q: its turn held a reply back before", end.State, end.Reason, bodies, want)
		}
	})
}

// TestANewIssueRunsProseIsHeldBackWithNoCheck: new_issue-06 of the eval answered "OPS-21 already
// tracks the noisy CPU alert at night, so I will not create a second issue." and filed nothing. A
// new-issue run's words always ask to file, so its prose is held back with no check, and its model
// is told to file the issue now; its next step proposes the issue. No check was made, so the held
// reply's frame says no check's time or tokens.
func TestANewIssueRunsProseIsHeldBackWithNoCheck(t *testing.T) {
	const prose = "ACME-21 already tracks the noisy CPU alert at night, so I will not create a second issue."
	file := agenttest.Call{Name: toolProposePlan, Arguments: map[string]any{
		"summary": "File the noisy CPU alert.",
		"changes": []any{map[string]any{"tool": "dispatch_issue", "arguments": map[string]any{"project": "ACME", "title": "The CPU alert is noisy at night"}}},
	}}
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if strings.Contains(request.Text(), newIssueNoPlanNote) {
			return agenttest.Reply{Calls: []agenttest.Call{file}}
		}
		return agenttest.Reply{Text: prose}
	})
	server, client := newFakeServer(t, keptPlan)
	workClaimed(t, fake.Model(), client, &agentwire.ClaimedRun{ID: "run-1", Words: "the CPU alert is noisy at night", Kind: agentwire.RunNewIssue,
		Level: "fast", Mode: "ask", LeaseToken: "lease-1"})
	end, bodies := server.end(t)
	if end.State != agentwire.StateWaiting || len(bodies) != 2 || !bodies[0].NoPlan {
		t.Fatalf("the run ended %s with %q, saving %+v; want the prose held back and then the issue proposed", end.State, end.Reason, bodies)
	}
	if checks := checkRequests(fake); len(checks) != 0 {
		t.Errorf("the new-issue run made %d checks, want none", len(checks))
	}
	if steps := stepRequests(fake); len(steps) != 2 || !toldAfter(steps[1], prose, newIssueNoPlanNote) {
		t.Errorf("the run made %d steps' calls, want the second given the prose and the note to file the issue now", len(steps))
	}
	replies := server.replyFrames(t)
	if len(replies) != 2 || !replies[0].NoPlan {
		t.Fatalf("the reply frames are %+v, want the held prose's and then the proposal's", replies)
	}
	for i, reply := range replies {
		if reply.CheckMS != nil || reply.CheckTokens != nil {
			t.Errorf("reply %d's frame carries a check's time (%t) or tokens (%t), want neither", i+1, reply.CheckMS != nil, reply.CheckTokens != nil)
		}
	}
}

// TestACheckThatFailsEndsTheTurnAsBefore: a check that draws no answer - the chat model refuses
// it, or answers what does not read - does not stand between the person and the answer: the reply
// ends the turn done as the model wrote it, and a warning names the run and the cause.
func TestACheckThatFailsEndsTheTurnAsBefore(t *testing.T) {
	const prose = "[ACME-7](dispatch://ACME-7) is closed."
	for _, c := range []struct {
		name  string
		reply agenttest.Reply
		cause string
	}{
		{name: "a check the model refuses", reply: agenttest.Reply{Status: http.StatusBadRequest}, cause: string(agentloop.FaultProviderError)},
		{name: "a check whose answer does not read", reply: agenttest.Reply{Text: "It passes."}, cause: string(agentloop.FaultMalformed)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var log logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
			fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
				if isCheck(request) {
					return c.reply
				}
				return agenttest.Reply{Text: prose}
			})
			server, client := newFakeServer(t, keptPlan)
			workRun(t, fake.Model(), client, "close ACME-7")
			end, bodies := server.end(t)
			if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose || bodies[0].NoPlan {
				t.Fatalf("the run ended %s with %q, saving %+v; want done with the reply as written", end.State, end.Reason, bodies)
			}
			if frame := server.lastReply(t); frame.Text != prose {
				t.Errorf("the answer's frame carries %q, want %q", frame.Text, prose)
			}
			if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 1 || checks == 0 {
				t.Errorf("the run made %d steps' calls and %d checks, want the reply and its check", steps, checks)
			}
			warned := log.Lines("the check of a reply in prose failed")
			if !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, c.cause) {
				t.Errorf("the log holds %q, want a warning naming run-1 and the cause %s", warned, c.cause)
			}
		})
	}
}

// jevRun is a fast run of words started from line, "" for none, at a server that names a
// decision model.
func jevRun(line, words string) *agentwire.ClaimedRun {
	return &agentwire.ClaimedRun{ID: "run-1", Words: words, Line: line, Decider: true, Level: "fast", Mode: "ask", LeaseToken: "lease-1"}
}

// answeredLine answers a line call with line, and every other call as keptPlan does.
func answeredLine(line string) func(agentwire.CallRequest) agentwire.CallAnswer {
	return func(call agentwire.CallRequest) agentwire.CallAnswer {
		if call.Kind == agentwire.KindLine {
			return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(`{"line":"` + line + `"}`)}
		}
		return keptPlan(call)
	}
}

// lineCalls is the line calls the run made of the server.
func (s *fakeServer) lineCalls() []agentwire.CallRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []agentwire.CallRequest
	for _, call := range s.calls {
		if call.Kind == agentwire.KindLine {
			lines = append(lines, call)
		}
	}
	return lines
}

// workAfterJev works one turn of run at the server client calls, on a chat model whose every
// reply, as script writes it, waits until Jev's reading of the turn's words is in: so the guard
// decides with the reading in hand, as it does wherever Jev answers within the turn's first model
// call. A turn the guard asks Jev nothing of waits for nothing.
func workAfterJev(t *testing.T, client agentwire.Client, run *agentwire.ClaimedRun, script func(*agenttest.Request) agenttest.Reply) *agenttest.Model {
	t.Helper()
	var working atomic.Pointer[worker]
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		reply := script(request)
		if read := working.Load().jev.Load(); read != nil {
			reply.Hold = read.answered
		}
		return reply
	})
	a := &agent{client: client, config: Config{Model: fake.Model()}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
	w := newWorker(t.Context(), a, run, func() {})
	working.Store(w)
	w.work()
	return fake
}

// requireChecked fails t unless the run made one step's call and its one check, ended done with
// prose as written, and the reply's frame says what the check took, no skip, and jev, what Jev
// read the words as.
func requireChecked(t *testing.T, server *fakeServer, fake *agenttest.Model, prose, jev string) {
	t.Helper()
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose {
		t.Fatalf("the run ended %s with %q, saving %+v; want done with the reply as written", end.State, end.Reason, bodies)
	}
	if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 1 || checks != 1 {
		t.Errorf("the run made %d steps' calls and %d checks, want the reply and its one check", steps, checks)
	}
	if frame := server.lastReply(t); frame.CheckMS == nil || frame.CheckSkipped != "" || frame.JevLine != jev {
		t.Errorf("the reply's frame says check_ms %v, check_skipped %q and jev_line %q; want the check's time, no skip and %q",
			frame.CheckMS, frame.CheckSkipped, frame.JevLine, jev)
	}
}

// TestATurnJevReadsAsAskEndsWithNoCheck: c13 checked 243 of 626 turns, each check adding a median
// 3.4 s to its turn, answers and statuses among them, whose words ask for no change. A run started
// from the line Ask, or from none (a follow-up typed in the box), asks Jev again what its words
// ask, with one line call beside its first model call: a turn Jev reads as Ask ends with its prose
// as written and no check, and its frame says the check was skipped because Jev read Ask, with no
// check's time.
func TestATurnJevReadsAsAskEndsWithNoCheck(t *testing.T) {
	const words = "who is ACME-7 assigned to"
	const prose = "[ACME-7](dispatch://ACME-7) is assigned to kim@example.com."
	for name, line := range map[string]string{"started from Ask": agentwire.LineAsk, "a follow-up of no line": ""} {
		t.Run(name, func(t *testing.T) {
			server, client := newFakeServer(t, answeredLine(agentwire.LineAsk))
			fake := workAfterJev(t, client, jevRun(line, words), func(request *agenttest.Request) agenttest.Reply {
				if isCheck(request) {
					return verdict(noPlanPasses)
				}
				return agenttest.Reply{Text: prose}
			})
			end, bodies := server.end(t)
			if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose || bodies[0].NoPlan {
				t.Fatalf("the run ended %s with %q, saving %+v; want done with the answer as written", end.State, end.Reason, bodies)
			}
			if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 1 || checks != 0 {
				t.Errorf("the run made %d steps' calls and %d checks, want the answer and no check", steps, checks)
			}
			frame := server.lastReply(t)
			if frame.Text != prose || frame.CheckSkipped != checkSkippedJevAsk || frame.JevLine != agentwire.LineAsk || frame.CheckMS != nil || frame.CheckTokens != nil {
				t.Errorf("the answer's frame is %+v, want its prose, check_skipped %q, jev_line ask and no check's time or tokens", frame, checkSkippedJevAsk)
			}
			if calls := server.lineCalls(); len(calls) != 1 || calls[0].Run != "run-1" || calls[0].LeaseToken != "lease-1" {
				t.Errorf("the run made the line calls %+v, want one of run-1 under its lease", calls)
			}
		})
	}
}

// TestATurnJevReadsAsAChangeIsChecked: a turn of an Ask run whose words Jev reads as Do or File
// asks for a change, so its prose is checked as every such reply was.
func TestATurnJevReadsAsAChangeIsChecked(t *testing.T) {
	const prose = "Reopening ACME-7 as todo."
	for _, line := range []string{agentwire.LineDo, agentwire.LineFile} {
		t.Run("Jev reads "+line, func(t *testing.T) {
			server, client := newFakeServer(t, answeredLine(line))
			fake := workAfterJev(t, client, jevRun(agentwire.LineAsk, "reopen ACME-7 as todo"), func(request *agenttest.Request) agenttest.Reply {
				if isCheck(request) {
					return verdict(noPlanPasses)
				}
				return agenttest.Reply{Text: prose}
			})
			requireChecked(t, server, fake, prose, line)
		})
	}
}

// TestATurnJevGivesNoLineIsChecked: a reading that never came is no Ask. The line call refused
// (the failure logged once, naming the run, and never the run's), a server that names no decision
// model, which the agent asks nothing, and a line call still out when the guard decides, which the
// turn's end cuts off: each turn's prose is checked, and its frame says Jev gave no line, or none
// yet.
func TestATurnJevGivesNoLineIsChecked(t *testing.T) {
	const words = "who is ACME-7 assigned to"
	const prose = "[ACME-7](dispatch://ACME-7) is assigned to kim@example.com."
	script := func(request *agenttest.Request) agenttest.Reply {
		if isCheck(request) {
			return verdict(noPlanPasses)
		}
		return agenttest.Reply{Text: prose}
	}

	t.Run("the line call is refused", func(t *testing.T) {
		var log logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
		server, client := newFakeServer(t, keptPlan)
		server.refuse = func(call agentwire.CallRequest) *agentwire.Error {
			if call.Kind == agentwire.KindLine {
				return &agentwire.Error{Code: agentwire.CodeCallFailed, Message: "the decision slot is busy"}
			}
			return nil
		}
		fake := workAfterJev(t, client, jevRun(agentwire.LineAsk, words), script)
		requireChecked(t, server, fake, prose, jevNone)
		warned := log.Lines("the line call of the run's words failed")
		if strings.Count(warned, "\n") != 1 || !strings.Contains(warned, "level=WARN") || !strings.Contains(warned, "run=run-1") || !strings.Contains(warned, "the decision slot is busy") {
			t.Errorf("the log holds %q, want one warning naming run-1 and the refusal", warned)
		}
	})

	t.Run("the server names no decision model", func(t *testing.T) {
		server, client := newFakeServer(t, keptPlan)
		run := jevRun(agentwire.LineAsk, words)
		run.Decider = false
		fake := workAfterJev(t, client, run, script)
		requireChecked(t, server, fake, prose, jevNone)
		if calls := server.lineCalls(); len(calls) != 0 {
			t.Errorf("the run made the line calls %+v at a server with no decision model, want none", calls)
		}
	})

	t.Run("the line call is still out", func(t *testing.T) {
		var log logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&log, nil))
		// The line call is answered only once the run's turn has ended, and the step's reply waits
		// until the server holds the line call: so the guard decides while the call is out, and
		// the turn's answer waits for no reading.
		asked, release := make(chan struct{}), make(chan struct{})
		server, client := newFakeServer(t, func(call agentwire.CallRequest) agentwire.CallAnswer {
			if call.Kind == agentwire.KindLine {
				close(asked)
				<-release
			}
			return answeredLine(agentwire.LineAsk)(call)
		})
		fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
			reply := script(request)
			reply.Hold = asked
			return reply
		})
		workClaimed(t, fake.Model(), client, jevRun(agentwire.LineAsk, words))
		close(release)
		requireChecked(t, server, fake, prose, jevLate)
		if calls := server.lineCalls(); len(calls) != 1 {
			t.Errorf("the run made the line calls %+v, want the one cut off at the turn's end", calls)
		}
		if warned := log.Lines("the line call"); warned != "" {
			t.Errorf("the log holds %q, want nothing of a line call the turn's end cut off", warned)
		}
	})
}

// TestADoLineRunIsCheckedWithNoLineCall: a run started from the line Do keeps the line the person
// pressed: Jev is asked nothing, and its prose is checked as before, its frame naming no reading.
func TestADoLineRunIsCheckedWithNoLineCall(t *testing.T) {
	const prose = "Reopening ACME-7 as todo."
	server, client := newFakeServer(t, answeredLine(agentwire.LineAsk))
	fake := workAfterJev(t, client, jevRun(agentwire.LineDo, "reopen ACME-7 as todo"), func(request *agenttest.Request) agenttest.Reply {
		if isCheck(request) {
			return verdict(noPlanPasses)
		}
		return agenttest.Reply{Text: prose}
	})
	requireChecked(t, server, fake, prose, "")
	if calls := server.lineCalls(); len(calls) != 0 {
		t.Errorf("the Do run made the line calls %+v, want none", calls)
	}
}
