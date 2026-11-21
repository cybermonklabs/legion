package answerloop

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// countingTool is the rig's gh tool, counting the calls an answer runs through it and the loop's
// lookups of an earlier call.
type countingTool struct {
	*fakeTool
	runs, lookups atomic.Int64
}

func (c *countingTool) Run(ctx context.Context, seq int, args string) ghtool.Result {
	c.runs.Add(1)
	return c.fakeTool.Run(ctx, seq, args)
}

func (c *countingTool) Lookup(ctx context.Context, seq int) (ghtool.Found, error) {
	c.lookups.Add(1)
	return c.fakeTool.Lookup(ctx, seq)
}

// pauseRig is the rig with the project's pause flag, which reads counts the loop's reads of, a
// counted gh tool and the loop's log.
type pauseRig struct {
	*rig
	paused atomic.Bool
	reads  atomic.Int64
	gh     *countingTool
	log    *testlog.Buffer
}

func newPauseRig(t *testing.T) *pauseRig {
	p := &pauseRig{rig: newRig(t), log: &testlog.Buffer{}}
	p.gh = &countingTool{fakeTool: p.tool}
	p.cfg.Paused = func() bool {
		p.reads.Add(1)
		return p.paused.Load()
	}
	p.cfg.Tools = toolsFunc(func(ghtool.Agent) Tool { return p.gh })
	p.cfg.Log = slog.New(slog.NewTextHandler(p.log, nil))
	p.restart()
	return p
}

// question is the question id's row as the store holds it now.
func (p *pauseRig) question(id string) store.AnswerQuestion {
	p.t.Helper()
	q, _, err := p.st.Question(p.ctx, id)
	if err != nil {
		p.t.Fatal(err)
	}
	return q
}

// spent is the day's spend on answers.
func (p *pauseRig) spent() float64 {
	p.t.Helper()
	spent, err := p.st.AnswerSpend(p.ctx, project, testNow)
	if err != nil {
		p.t.Fatal(err)
	}
	return spent
}

// A question received while the project is paused is not answered: no model call, no gh call,
// nothing posted, nothing passed to the controller, and it stays received. Once the pause clears
// and the resume pokes the loop, it is answered and posted, with no wait for the sweep: the loop
// runs with an hour's.
func TestAQuestionReceivedWhilePausedIsAnsweredOnceTheResumePokesTheLoop(t *testing.T) {
	p := newPauseRig(t)
	p.paused.Store(true)
	p.cfg.Sweep = time.Hour
	p.restart()
	p.answer = func(req answer.Request) (answer.Reply, error) {
		req.GH.Run(context.Background(), 1, `["pr","view","7"]`)
		return answered(req, "Plum, from "+req.Question.Text), nil
	}
	p.conversation("ses_tester", testerToken, 0)
	p.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	p.drain()
	q := p.only()
	switch {
	case q.State != store.QuestionReceived:
		t.Fatalf("question = %s while paused; want received", q.State)
	case p.answerCount() != 0 || p.gh.runs.Load() != 0 || p.gh.lookups.Load() != 0:
		t.Fatalf("%d answers, %d gh calls and %d lookups while paused; want none", p.answerCount(), p.gh.runs.Load(), p.gh.lookups.Load())
	case len(p.dispatch.posts()) != 0 || len(p.listener.posts()) != 0 || len(p.rec.passed) != 0:
		t.Fatalf("while paused: Dispatch posts %s, listener posts %s, passed on %+v; want none", describe(p.dispatch.posts()),
			describe(p.listener.posts()), p.rec.passed)
	}

	// The wake Receive left is taken first, so once the loop's first schedule has read the pause
	// only the resume's poke wakes it before its sweep.
	select {
	case <-p.loop.wake:
	default:
	}
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan struct{})
	reads := p.reads.Load()
	go func() {
		defer close(done)
		p.loop.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	testwait.Eventually(t, "the loop's first schedule to read the pause", func() bool { return p.reads.Load() > reads })
	p.paused.Store(false)
	p.loop.Poke()
	testwait.Eventually(t, "the question to be posted after the resume", func() bool { return p.question(q.ID).State == store.QuestionPosted })
	posts := p.dispatch.posts()
	switch {
	case p.answerCount() != 1 || p.gh.runs.Load() != 1:
		t.Fatalf("%d answers and %d gh calls after the resume; want the question answered once", p.answerCount(), p.gh.runs.Load())
	case len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), "Plum, from Why plum?"):
		t.Fatalf("Dispatch posts = %s; want the one answer", describe(posts))
	case len(p.rec.passed) != 0:
		t.Fatalf("passed on %+v; want nothing", p.rec.passed)
	}
}

// A run in flight when the pause comes stops there. The stub answer runs one gh call, recorded
// as ghtool.Run records it, the project is paused, and the stub asks the request's pause check, as
// answer.Answer reads it before its next model call or gh call. The question stays generating:
// nothing is appended, posted or passed on, the run's cost is the day's, and the loop logs once
// that the question waits. After the resume it is answered again from the conversation as it was,
// the new run handed the gh call the stopped one made with its result, so a write that call made
// is not made again, and posted.
func TestARunThePauseStopsInFlightIsAnsweredAgainAfterTheResume(t *testing.T) {
	p := newPauseRig(t)
	path := p.conversation("ses_tester", testerToken, 0)
	before := fileContent(t, path)
	comment := []string{"pr", "comment", "7", "--body", "Done."}
	stopped := false
	p.answer = func(req answer.Request) (answer.Reply, error) {
		if stopped {
			return answered(req, "Commented already."), nil
		}
		stopped = true
		exit := 0
		if err := p.st.StartAnswerToolCall(p.ctx, store.AnswerToolCall{Question: req.Question.ID, Seq: 1, Args: comment, App: "review", Owner: "o",
			StartedAt: testNow}); err != nil {
			t.Error(err)
		}
		req.GH.Run(context.Background(), 1, `["pr","comment","7","--body","Done."]`)
		if err := p.st.FinishAnswerToolCall(p.ctx, req.Question.ID, 1, "https://github.com/o/r/pull/7#issuecomment-1", &exit, testNow); err != nil {
			t.Error(err)
		}
		p.paused.Store(true)
		if req.Paused == nil || !req.Paused() {
			return answered(req, "Commented."), nil
		}
		return answer.Reply{Calls: 1, CostUSD: 0.25}, answer.ErrPaused
	}
	p.receive(messageHandoff("ses_tester", "m-1", 1, "Please comment."))
	p.drain()
	q := p.only()
	switch {
	case q.State != store.QuestionGenerating:
		t.Fatalf("question = %s after the pause stopped its run; want generating", q.State)
	case p.answerCount() != 1 || p.gh.runs.Load() != 1:
		t.Fatalf("%d answers and %d gh calls; want the one stopped run and its one call", p.answerCount(), p.gh.runs.Load())
	case len(p.dispatch.posts()) != 0 || len(p.rec.passed) != 0:
		t.Fatalf("while paused: Dispatch posts %s, passed on %+v; want none", describe(p.dispatch.posts()), p.rec.passed)
	case fileContent(t, path) != before:
		t.Fatalf("the conversation changed while paused:\n%s", fileContent(t, path)[len(before):])
	case p.spent() != 0.25:
		t.Fatalf("spend = %v; want the stopped run's 0.25", p.spent())
	case strings.Count(p.log.String(), "answers: the project is paused; the question waits for the resume") != 1 ||
		!strings.Contains(p.log.String(), "question="+q.ID):
		t.Fatalf("log:\n%s\nwant one line saying the question waits for the resume", p.log.String())
	}

	p.paused.Store(false)
	p.drain()
	posts := p.dispatch.posts()
	switch {
	case p.answerCount() != 2 || p.gh.runs.Load() != 1:
		t.Fatalf("%d answers and %d gh calls after the resume; want one more answer and no more gh calls", p.answerCount(), p.gh.runs.Load())
	case p.question(q.ID).State != store.QuestionPosted:
		t.Fatalf("question = %s after the resume; want posted", p.question(q.ID).State)
	case len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), "Commented already."):
		t.Fatalf("Dispatch posts = %s; want the one answer", describe(posts))
	case len(p.rec.passed) != 0:
		t.Fatalf("passed on %+v; want nothing", p.rec.passed)
	case p.spent() != 0.5:
		t.Fatalf("spend = %v; want both runs' 0.25", p.spent())
	}
	if earlier := p.requests[1].Earlier; len(earlier) != 1 || strings.Join(earlier[0].Args, " ") != strings.Join(comment, " ") ||
		earlier[0].Result == nil || !strings.Contains(*earlier[0].Result, "issuecomment-1") {
		t.Fatalf("the run after the resume was handed %+v; want the stopped run's comment with its result", earlier)
	}
	if strings.Count(fileContent(t, path), "Please comment.") != 1 {
		t.Fatalf("the conversation holds the question %d times; want the one exchange", strings.Count(fileContent(t, path), "Please comment."))
	}
}

// A reply the pause comes before is not posted: the answer ends and the project is paused before
// the loop posts it. The exchange is in the conversation and the question is appended; after the
// resume it is posted without being answered again.
func TestAReplyThePauseComesBeforeIsPostedAfterTheResume(t *testing.T) {
	p := newPauseRig(t)
	p.answer = func(req answer.Request) (answer.Reply, error) {
		p.paused.Store(true)
		return answered(req, "Plum, from "+req.Question.Text), nil
	}
	p.conversation("ses_tester", testerToken, 0)
	p.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	p.drain()
	q := p.only()
	switch {
	case q.State != store.QuestionAppended:
		t.Fatalf("question = %s once paused before its post; want appended", q.State)
	case len(p.dispatch.posts()) != 0 || len(p.rec.passed) != 0:
		t.Fatalf("while paused: Dispatch posts %s, passed on %+v; want none", describe(p.dispatch.posts()), p.rec.passed)
	}
	p.paused.Store(false)
	p.drain()
	if posts := p.dispatch.posts(); p.answerCount() != 1 || p.question(q.ID).State != store.QuestionPosted || len(posts) != 1 {
		t.Fatalf("after the resume: %d answers, question %s, Dispatch posts %s; want the one answer posted once", p.answerCount(),
			p.question(q.ID).State, describe(posts))
	}
}

// A pause that comes while the answer is prepared stops it before the question's first gh call:
// the lookup of a comment call an earlier run left of unknown outcome is not made, and no model is
// called. After the resume the lookup is made and the question answered and posted.
func TestAPauseAsTheAnswerIsPreparedStopsItBeforeItsFirstGhCall(t *testing.T) {
	p := newPauseRig(t)
	p.cfg.Issues = issuesFunc(func(string) IssueFacts {
		p.paused.Store(true)
		return IssueFacts{Recorded: true, DispatchStatus: "done", Phase: "done"}
	})
	p.restart()
	p.conversation("ses_tester", testerToken, 0)
	p.receive(messageHandoff("ses_tester", "m-1", 1, "Comment on the PR."))
	q := p.only()
	if err := p.st.StartAnswerToolCall(p.ctx, store.AnswerToolCall{Question: q.ID, Seq: 1, Args: []string{"pr", "comment", "7", "--body", "Done."},
		App: "review", Owner: "o", StartedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	p.drain()
	switch {
	case p.question(q.ID).State != store.QuestionGenerating:
		t.Fatalf("question = %s once paused as its answer was prepared; want generating", p.question(q.ID).State)
	case p.gh.lookups.Load() != 0 || p.answerCount() != 0:
		t.Fatalf("%d lookups and %d answers while paused; want none", p.gh.lookups.Load(), p.answerCount())
	}
	p.cfg.Issues = issuesFunc(func(string) IssueFacts { return IssueFacts{Recorded: true, DispatchStatus: "done", Phase: "done"} })
	p.restart()
	p.paused.Store(false)
	p.drain()
	if posts := p.dispatch.posts(); p.gh.lookups.Load() != 1 || p.answerCount() != 1 || p.question(q.ID).State != store.QuestionPosted || len(posts) != 1 {
		t.Fatalf("after the resume: %d lookups, %d answers, question %s, Dispatch posts %s; want the lookup, the answer and its post",
			p.gh.lookups.Load(), p.answerCount(), p.question(q.ID).State, describe(posts))
	}
}

// pausingEnvoy is the listener, the project's pause coming as it takes the loop's send of a reply.
type pausingEnvoy struct {
	Envoy
	paused *atomic.Bool
}

func (e pausingEnvoy) Send(ctx context.Context, m notify.Message) error {
	e.paused.Store(true)
	return e.Envoy.Send(ctx, m)
}

// countedConversations is the rig's conversations, counting each read of one.
type countedConversations struct {
	Conversations
	reads atomic.Int64
}

func (c *countedConversations) Open(ctx context.Context, conv store.AgentConversation) (Session, error) {
	c.reads.Add(1)
	return c.Conversations.Open(ctx, conv)
}

// toolsOpened is the rig's gh tool, recording the question each answer opens it for.
type toolsOpened struct {
	tool      Tool
	mu        sync.Mutex
	questions []string
}

func (o *toolsOpened) Open(_ context.Context, _ ghtool.Agent, question string) (Tool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.questions = append(o.questions, question)
	return o.tool, nil
}

func (o *toolsOpened) opened() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.questions)
}

// A worker already running when the pause comes reads it again before its session's next
// question, so that question is not begun. Two questions of one session are open, and the pause
// comes after the first one's last read of it: as the first reply is sent, which post read the
// pause before, or as the first answer returns a reply to a message that asks nothing, which
// nothing posts. The first question ends; the second stays received, its conversation is not read
// and no gh tool is opened for it. A pause the first answer reads itself ends the pass with
// answer.ErrPaused, before any read of the next question.
func TestAPauseAfterAQuestionsLastReadOfItStopsItsWorkerBeforeTheSessionsNextQuestion(t *testing.T) {
	for _, c := range []struct {
		name string
		// asksNothing is a first answer that replies none and returns as the pause comes;
		// otherwise the pause comes as the first reply is sent.
		asksNothing bool
		first       store.QuestionState
	}{
		{name: "as the first reply is sent", first: store.QuestionPosted},
		{name: "as the first answer, to a message that asks nothing, returns", asksNothing: true, first: store.QuestionDropped},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPauseRig(t)
			conversations := &countedConversations{Conversations: p.cfg.Conversations}
			tools := &toolsOpened{tool: p.gh}
			p.cfg.Conversations, p.cfg.Tools = conversations, tools
			if c.asksNothing {
				p.answer = func(req answer.Request) (answer.Reply, error) {
					reply := answered(req, "Noted.")
					reply.Kind = answer.None
					p.paused.Store(true)
					return reply, nil
				}
			} else {
				p.cfg.Envoy = pausingEnvoy{Envoy: p.cfg.Envoy, paused: &p.paused}
			}
			p.restart()
			p.conversation("ses_tester", testerToken, 0)
			p.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Run the suite?"))
			first := p.only().ID
			p.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-2", "agent.ses_tester.k2", "And the linter?"))
			second := ""
			for id := range p.questions() {
				if id != first {
					second = id
				}
			}
			p.drain()
			switch {
			case p.question(first).State != c.first:
				t.Fatalf("the first question = %s; want %s", p.question(first).State, c.first)
			case p.question(second).State != store.QuestionReceived:
				t.Fatalf("the second question = %s once the pause came; want received", p.question(second).State)
			case !slices.Equal(tools.opened(), []string{first}):
				t.Fatalf("the gh tool was opened for %v; want once, for the first question %s", tools.opened(), first)
			case conversations.reads.Load() != 1 || p.answerCount() != 1:
				t.Fatalf("%d conversation reads and %d answers; want the first question's alone", conversations.reads.Load(), p.answerCount())
			}
		})
	}
}
