package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

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
// agentwire.WordsBound words as the bar draws them, its chips' words counted. Before a reply in
// prose ends the turn done, the agent asks the server how many words the bar draws of it, with
// one words call of the run under its lease that records nothing (step and call 0); a reply under
// the bound ends the turn done as it is, with no other model call than its step's and the
// guard's check, and its frame says the count.
func TestAnAnswerUnderTheBoundEndsTheTurnAsItIs(t *testing.T) {
	prose := "[ACME-7](dispatch://ACME-7) waits on " + plainWords("word", 10)
	fake := agenttest.New(t, lengthScript(prose, "a second turn"))
	server, client := newFakeServer(t, keptPlan)
	server.words = func(agentwire.CallRequest) agentwire.CallAnswer { return countedWords(agentwire.WordsBound - 1) }
	workRun(t, fake.Model(), client, "what does ACME-7 wait on")
	end, bodies := server.end(t)
	if end.State != agentwire.StateDone || len(bodies) != 1 || bodies[0].Text != prose || bodies[0].LongWords != 0 {
		t.Fatalf("the run ended %s with %q, saving %+v; want done with the answer as written", end.State, end.Reason, bodies)
	}
	if steps, checks := len(stepRequests(fake)), len(checkRequests(fake)); steps != 1 || checks != 1 || fake.Count() != 2 {
		t.Errorf("the run made %d steps' calls and %d checks of %d model calls, want the answer and the guard's check alone", steps, checks, fake.Count())
	}
	if frame := server.lastReply(t); frame.Text != prose || frame.HeldLong || drawn(frame) != agentwire.WordsBound-1 {
		t.Errorf("the answer's frame carries %q (held long %t) and %d drawn words, want the answer and %d", frame.Text, frame.HeldLong, drawn(frame), agentwire.WordsBound-1)
	}
	calls := server.madeWordsCalls()
	if len(calls) != 1 || calls[0].Text != prose || calls[0].Run != "run-1" || calls[0].LeaseToken != "lease-1" || calls[0].Step != 0 || calls[0].Call != 0 {
		t.Errorf("the run made the words calls %+v, want one of the answer, of run-1 under its lease, at step 0 and call 0", calls)
	}
}

// TestALongAnswerIsHeldBackAndToldHowManyWordsThePersonSees: at 8a3e562b, 6 of 11 answers to "What
// is the state of this issue's open children?" drew 120 words or more. A reply the bar draws in
// agentwire.WordsBound words or more, whose words ask for no more, is held back: saved with its
// count, so the model reads what it wrote, its note saying the person did not see it, and its
// frame marked and with no prose, so the bar adds nothing of it. The next step's model is told,
// after it, how many words the person sees and to give the same facts in fewer than 120, and its
// shorter reply ends the turn done as the answer. A turn whose words Jev reads as Ask, which makes
// no check, is held to the bound too.
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
			if told := longTold(130); !strings.Contains(told, " 130 words") || !strings.Contains(told, "Otherwise give the same facts in fewer than 120 words.") {
				t.Errorf("the model is told %q, want the count the person sees and the bound", told)
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
// reply still of agentwire.WordsBound words or more after that turn ends the turn done, shown
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

// TestWordsThatAskForMoreGetTheSameAnswerAgain: no other model call decides whether the person's
// words ask for more than a short answer: the one more turn asks it, in the architect's words
// (dispatch://INFRA-1588/comment/77ddfc7a-b65c-4d36-b78c-c00e703c7d31). Words that ask for all of
// a thing get the same long answer again, which ends the turn done, whole, with no third turn.
func TestWordsThatAskForMoreGetTheSameAnswerAgain(t *testing.T) {
	const ruling = "If the person's words ask for more than a short answer, give the same answer again. Otherwise give the same facts in fewer than 120 words."
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

// TestALongAnswerEndsWholeWhereNoTurnIsLeft: a reply of agentwire.WordsBound words or more on the
// run's last model call, which leaves no call to say it in fewer, ends the turn done whole; and so
// does one whose words call fails, which holds nothing back and is logged with the run and the
// cause.
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
