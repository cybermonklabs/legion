package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The A25 surface of dispatch://INFRA-204 (spec v26): while the project is paused the answer loop
// answers for an agent the pause stopped, and the pause still starts no role. Each test drives a
// real supervise machine to the pause's stop and the loop through the daemon's answer gate, its
// outbox's rows and its controller's pass, the loop reading the pause flag the machines read
// (releasedRig), so each fails with answerloop's pause gate of 043f9c67 restored, which answered
// nothing until the resume.

// launched is how many launches the runtime took since the rig's stop.
func (r *releasedRig) launched() int {
	return len(r.rt.CallsOf("Spawn")) + len(r.rt.CallsOf("Resume")) - r.launches
}

// spent is the project's spend on answers today.
func (r *releasedRig) spent() float64 {
	r.t.Helper()
	spent, err := r.st.AnswerSpend(r.ctx, "legion", time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	return spent
}

// resume ends the pause that stopped the implementer as the operator's resume does: the store's
// pause ended, the flag the machines and the loop read cleared, and the machine told the pause has
// ended (projectPause.Resumed).
func (r *releasedRig) resume() {
	r.t.Helper()
	pause := r.paused.Load()
	if pause == 0 {
		r.t.Fatal("the rig's project is not paused")
	}
	endLegionPause(r.t, r.pool, pause, time.Now())
	r.paused.Store(0)
	if err := r.machine.Handle(r.ctx, supervise.ResumePause{Claim: r.token, Pause: pause}); err != nil {
		r.t.Fatalf("tell the implementer the pause ended: %v", err)
	}
	r.loop.Poke()
}

// While the project is paused a question to an agent the pause stopped is answered from its
// conversation and posted where it was asked, and its cost counts in the day's spend; the machine
// launches nothing, no wake row is written, and the controller is told nothing. The control is
// answerloop's pause gate of 043f9c67: nothing is answered until the resume.
func TestDuringAPauseAQuestionToAnAgentThePauseStoppedIsAnsweredPostedAndCounted(t *testing.T) {
	t.Parallel()
	r := newReleasedRig(t, stopByPause)
	r.answer = func(req answer.Request) answer.Reply {
		reply := releasedReply(req, answer.Answered, "Answered: "+req.Question.Text)
		reply.CostUSD = 0.25
		return reply
	}
	r.handOff(releasedMessage("m-1", "Why A?"))
	r.work()
	posts, sends := r.requests()
	switch {
	case !r.cfg.Paused():
		t.Fatal("the loop reads the project running; want it paused throughout")
	case len(r.states()) != 1 || r.states()[0] != string(store.QuestionPosted):
		t.Fatalf("questions = %v; want the one posted while paused", r.states())
	case len(posts) != 1 || !strings.HasPrefix(posts[0], "/api/v1/messages/m-1/reply ") || !strings.Contains(posts[0], "Answered: Why A?"):
		t.Fatalf("Dispatch posts = %q; want the answer to the message", posts)
	case len(sends) != 0:
		t.Fatalf("listener requests = %q; want none", sends)
	case !strings.Contains(r.conversation(), "Answered: Why A?"):
		t.Fatal("the implementer's conversation holds no exchange of the answer")
	case r.spent() != 0.25:
		t.Fatalf("the day's spend = %v; want the answer's 0.25", r.spent())
	case r.rows(record.OutboxKindAgentMessage) != 0 || r.rows(record.OutboxKindControllerNotice) != 0 || len(r.passedToController()) != 0:
		t.Fatalf("%d wake rows, %d controller notices, %+v passed to the controller; want none", r.rows(record.OutboxKindAgentMessage),
			r.rows(record.OutboxKindControllerNotice), r.passedToController())
	}
	r.assertStillReleased()
}

// While the project is paused a person's answer to the ask of an agent the pause stopped, which
// also asks it a question, is answered: the question is answered, appended to the conversation and
// posted on the ask's thread, and then the one wake row carrying the person's answer is written.
// The pause holds that row: the outbox publishes nothing of it and wakes nothing, the machine
// launches nothing and the claim stays released, and the architect and the controller are told
// nothing. Once the project resumes the row wakes the claim and the machine launches it. The
// control is the same answer to an agent idle release stopped with no pause: the row wakes the
// claim, and the machine launches it, at once. With answerloop's pause gate of 043f9c67 restored,
// nothing is answered and no row is written while the project is paused.
func TestDuringAPauseAPersonsAnswerToItsAskWakesTheAgentThePauseStoppedOnlyAfterTheResume(t *testing.T) {
	t.Parallel()
	for _, stop := range []stopBy{stopByPause, stopByIdleRelease} {
		t.Run(string(stop), func(t *testing.T) {
			r := newReleasedRig(t, stop)
			r.handOff(releasedAnswer("Use B. Why did you implement A?"))
			r.work()
			posts, _ := r.requests()
			switch {
			case len(r.states()) != 1 || r.states()[0] != string(store.QuestionPosted):
				t.Fatalf("questions = %v; want the one posted", r.states())
			case len(posts) != 1 || !strings.HasPrefix(posts[0], "/api/v1/issues/"+releasedIssue+"/comments ") || !strings.Contains(posts[0], releasedAsk):
				t.Fatalf("Dispatch posts = %q; want the answer on the ask's thread", posts)
			case !strings.Contains(r.conversation(), "Answered: Selected: B"):
				t.Fatal("the implementer's conversation holds no exchange of the answer")
			}
			if messages := r.agentMessages(); len(messages) != 1 || !strings.Contains(messages[0].Text, "Use B. Why did you implement A?") {
				t.Fatalf("agent_message rows = %+v; want one carrying the person's answer", messages)
			}
			if err := r.runner.RunOnce(r.ctx); err != nil {
				t.Fatalf("drain the outbox: %v", err)
			}
			if stop == stopByPause {
				if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.rows(record.OutboxKindAgentMessage) != 1 || podResumes(t, r.pool, releasedIssue) != 0 {
					t.Fatalf("while paused: %d publishes, %d wake rows, %d wakes; want the row held, nothing published or woken",
						attempts, r.rows(record.OutboxKindAgentMessage), podResumes(t, r.pool, releasedIssue))
				}
				r.assertStillReleased()
				if notices, toController := r.rows(record.OutboxKindNotice), r.rows(record.OutboxKindControllerNotice); notices != 0 || toController != 0 {
					t.Fatalf("%d notice and %d controller notice rows; want the architect and the controller told nothing", notices, toController)
				}
				r.resume()
				if err := r.runner.RunOnce(r.ctx); err != nil {
					t.Fatalf("drain the outbox after the resume: %v", err)
				}
			}
			if held := r.machine.Claim(); held.Released() || r.launched() != 1 || podResumes(t, r.pool, releasedIssue) != 1 {
				t.Fatalf("the implementer is %s by %q after %d launches and %d wakes; want it woken once and launched", held.State, held.SuspendedBy,
					r.launched(), podResumes(t, r.pool, releasedIssue))
			}
		})
	}
}
