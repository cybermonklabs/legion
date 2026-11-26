package answerloop

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The A25 surface of dispatch://INFRA-204 (spec v26) in the loop: while the project is paused the
// loop answers as it does while the project runs, posts the reply and counts its cost in the day's
// spend; the answer's tools run their reads and refuse every write; and a model call the provider
// fails keeps its question for the resume. Each test runs the loop with the project paused, so
// each fails with answerloop's pause gate of 043f9c67 restored, which started no answer, stopped
// one in flight and posted nothing until the resume.

// countingTool is the rig's gh tool, recording the arguments of each call an answer runs through
// it, each answered with PR 7's state as a read gives it; its lookups of an earlier call are the
// rig's fakeTool's (lookedUp).
type countingTool struct {
	*fakeTool
	mu   sync.Mutex
	args []string
}

func (c *countingTool) Run(_ context.Context, call answer.GHCall) ghtool.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args = append(c.args, call.Args)
	return ghtool.Result{Output: "PR 7 is open.", Ran: true}
}

// ran is the arguments of each call the tool ran, in order.
func (c *countingTool) ran() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.args)
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

// pausingEnvoy is the listener, the project's pause coming as it takes the loop's send of a reply.
type pausingEnvoy struct {
	Envoy
	paused *atomic.Bool
}

func (e pausingEnvoy) Send(ctx context.Context, m notify.Message) error {
	e.paused.Store(true)
	return e.Envoy.Send(ctx, m)
}

// While the project is paused the loop answers each question as it does while the project runs,
// posts its reply where it was asked, counts its cost in the day's spend and passes nothing to the
// controller: a question received during the pause; one whose answer the pause comes during, its
// gh call run; one whose answer the pause comes as it is prepared, the lookup of a gh comment an
// earlier run left of unknown outcome made; and two questions of one session, the pause coming as
// the first reply is sent. The control is answerloop's pause gate of 043f9c67, which answered none
// of them, or posted none of their replies, until the resume.
func TestDuringAPauseEachQuestionIsAnsweredPostedAndCounted(t *testing.T) {
	for _, c := range []struct {
		name string
		// pause makes the project's pause come where the case says.
		pause func(p *pauseRig)
		// envoy hands off two Envoy messages of one session rather than one Dispatch message;
		// earlier records a gh comment of unknown outcome an earlier run of the question left.
		envoy, earlier bool
	}{
		{name: "a question received while paused", pause: func(p *pauseRig) { p.paused.Store(true) }},
		{name: "the pause comes during the answer", pause: func(p *pauseRig) {
			p.answer = func(req answer.Request) (answer.Reply, error) {
				p.paused.Store(true)
				req.GH.Run(context.Background(), answer.GHCall{Seq: 1, N: 1, Args: `["pr","view","7"]`})
				return answered(req, "Plum, from "+req.Question.Text), nil
			}
		}},
		{name: "the pause comes as the answer is prepared", earlier: true, pause: func(p *pauseRig) {
			p.cfg.Issues = issuesFunc(func(string) IssueFacts {
				p.paused.Store(true)
				return IssueFacts{Recorded: true, DispatchStatus: "done", Phase: "done"}
			})
		}},
		{name: "two questions of one session, the pause coming as the first reply is sent", envoy: true, pause: func(p *pauseRig) {
			p.cfg.Envoy = pausingEnvoy{Envoy: p.cfg.Envoy, paused: &p.paused}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPauseRig(t)
			c.pause(p)
			p.restart()
			p.conversation("ses_tester", testerToken, 0)
			if c.envoy {
				p.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Run the suite?"))
				p.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-2", "agent.ses_tester.k2", "And the linter?"))
			} else {
				p.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
			}
			if c.earlier {
				if err := p.st.StartAnswerToolCall(p.ctx, store.AnswerToolCall{Question: p.only().ID, Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "comment", "7", "--body", "Done."},
					App: "review", Owner: "o", StartedAt: testNow}); err != nil {
					t.Fatal(err)
				}
			}
			p.drain()
			questions := p.questions()
			for _, q := range questions {
				if q.State != store.QuestionPosted {
					t.Fatalf("question %s = %s with the project paused; want posted", q.ID, q.State)
				}
			}
			posts := len(p.dispatch.posts()) + len(p.listener.posts())
			switch {
			case !p.paused.Load():
				t.Fatal("the project is not paused; the case paused nothing")
			case p.answerCount() != len(questions) || posts != len(questions):
				t.Fatalf("%d answers and %d replies posted for %d questions; want each answered and posted once", p.answerCount(), posts, len(questions))
			case p.spent() != 0.25*float64(len(questions)):
				t.Fatalf("spend = %v; want each answer's 0.25 counted", p.spent())
			case len(p.rec.passed) != 0:
				t.Fatalf("passed on %+v; want nothing", p.rec.passed)
			case c.earlier && len(p.gh.lookedUp()) != 1:
				t.Fatalf("%d lookups of the earlier comment; want it looked up once", len(p.gh.lookedUp()))
			}
		})
	}
}

// toolResultOf is the tool_result block a Messages request body sends for the tool call id:
// whether it is an error, and its content as JSON.
func toolResultOf(t *testing.T, body, id string) (isError bool, content string) {
	t.Helper()
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	for _, m := range request.Messages {
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b["type"] == "tool_result" && b["tool_use_id"] == id {
				text, _ := json.Marshal(b["content"])
				isError, _ = b["is_error"].(bool)
				return isError, string(text)
			}
		}
	}
	t.Fatalf("the request sends no tool_result for %s:\n%s", id, body)
	return false, ""
}

// While the project is paused the answer's tools run their reads and refuse every write, through
// the real model loop against a stand-in Anthropic endpoint: the model asks, one call at a time,
// for a gh comment, a gh read, a dispatch comment and a dispatch read. Each write is refused as
// the tool's result, an error naming the pause, and neither tool is handed it; each read runs and
// its output reaches the model. The system prompt says the project is paused, and the reply that
// says the comment was not made is posted, its cost counted in the day's spend.
func TestDuringAPauseTheToolsRunEachReadAndRefuseEachWrite(t *testing.T) {
	const reply = "PR 7 is open. I did not comment: the project is paused, and Legion makes no write in my name until it resumes."
	ghWrite := []string{"pr", "comment", "7", "-R", "acme/widgets", "--body", "Done."}
	ghRead := []string{"pr", "view", "7", "-R", "acme/widgets"}
	dispatchWrite := []string{"comment", "--issue", "LEGION-9", "--body", "Done."}
	dispatchRead := []string{"read", "--issue", "LEGION-9"}
	call := func(id, tool string, args []string) string {
		return anthropicEvents("tool_use", 10, 5, map[string]any{"type": "tool_use", "id": id, "name": tool, "input": map[string]any{"args": args}})
	}
	endpoint := newAnthropicStandIn(t, call("toolu_1", store.ToolGH, ghWrite), call("toolu_2", store.ToolGH, ghRead),
		call("toolu_3", store.ToolDispatch, dispatchWrite), call("toolu_4", store.ToolDispatch, dispatchRead),
		anthropicEvents("end_turn", 10, 30, map[string]any{"type": "text", "text": `{"kind":"answered","text":"` + reply + `","needs":""}`}))
	keyFile := filepath.Join(t.TempDir(), "anthropic-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-standin\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newPauseRig(t)
	p.paused.Store(true)
	p.cfg.Answers.Providers = map[config.AnswerProvider]config.AnswerEndpoint{
		config.AnswerAnthropic: {BaseURL: endpoint.URL, Credential: config.AnswerCredential{File: keyFile}},
	}
	p.cfg.Answers.Prices = map[string]config.AnswerPrice{"anthropic/claude-opus-5-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}}
	dispatchTools := &fakeDispatchTools{}
	p.cfg.DispatchTools = dispatchTools
	// The real model loop needs the issue's repository, which its gh tool's description names.
	p.cfg.Issues = issuesFunc(func(string) IssueFacts {
		return IssueFacts{Recorded: true, DispatchStatus: "in_progress", Phase: "testing", Repository: ghrepo.MustParse("acme/widgets"),
			PullRequest: &PullRequestFacts{Number: 7, State: "open", Head: "abc123"}}
	})
	p.restart()
	p.answer = func(req answer.Request) (answer.Reply, error) {
		reply, err := answer.Answer(context.Background(), req)
		if err != nil || reply.Fault != "" {
			t.Logf("the answer ended with fault %q (%s), %v", reply.Fault, reply.Detail, err)
		}
		return reply, err
	}
	p.conversation("ses_tester", testerToken, 0)
	p.receive(messageHandoff("ses_tester", "m-1", 1, "Is PR 7 open? Comment Done. on it, and on LEGION-9."))
	p.drain()

	q := p.only()
	posts := p.dispatch.posts()
	if q.State != store.QuestionPosted || len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), reply+"\n\n"+AnswerMarker+q.ID+" -->") {
		t.Fatalf("question %s, posts %s with the project paused; want the reply posted", q.State, describe(posts))
	}
	encoded := func(args []string) string {
		raw, _ := json.Marshal(args)
		return string(raw)
	}
	if ran := p.gh.ran(); !slices.Equal(ran, []string{encoded(ghRead)}) {
		t.Errorf("the gh tool ran %q; want the read alone", ran)
	}
	dispatchTools.mu.Lock()
	calls := slices.Clone(dispatchTools.calls)
	dispatchTools.mu.Unlock()
	if len(calls) != 1 || calls[0].Args != encoded(dispatchRead) {
		t.Errorf("the dispatch tool ran %+v; want the read alone", calls)
	}
	endpoint.mu.Lock()
	bodies := slices.Clone(endpoint.bodies)
	endpoint.mu.Unlock()
	if len(bodies) != 5 {
		t.Fatalf("%d model calls; want five", len(bodies))
	}
	for i, want := range []struct {
		id, holds string
		refused   bool
	}{
		{id: "toolu_1", holds: "the Legion project is paused", refused: true},
		{id: "toolu_2", holds: "PR 7 is open."},
		{id: "toolu_3", holds: "the Legion project is paused", refused: true},
		{id: "toolu_4", holds: "LEGION-9: a comment since you stopped says plum"},
	} {
		isError, content := toolResultOf(t, bodies[i+1], want.id)
		if isError != want.refused || !strings.Contains(content, want.holds) {
			t.Errorf("call %s reached the model as %s (error %t); want it to hold %q, error %t", want.id, content, isError, want.holds, want.refused)
		}
	}
	if !strings.Contains(bodies[0], "Legion has paused your project") {
		t.Errorf("the system prompt does not say the project is paused:\n%s", bodies[0])
	}
	if p.spent() <= 0 {
		t.Errorf("spend = %v; want the answer's cost counted", p.spent())
	}
	if len(p.rec.passed) != 0 {
		t.Errorf("passed on %+v; want nothing", p.rec.passed)
	}
}

// While the project is paused a model call the provider fails, as the model gateway does once it
// refuses the project's calls, keeps its question for the resume, whether or not a model call
// returned turns before it: the question is received again, nothing is appended, posted or passed
// to the controller, and the call's cost is the day's. While the pause stands the loop makes no
// model call for that question again, nor for a later question of its session, which waits behind
// it: a schedule meets the pause and starts no worker for the session. The resume's poke runs them
// at once, the loop's sweep an hour away, in the order they were received, and each is answered
// and posted. The control is the same fault while the project runs: the daemon's reply naming the
// fault is posted and the question is passed to the controller, as before.
func TestDuringAPauseAProviderFaultKeepsTheQuestionForTheResume(t *testing.T) {
	for _, c := range []struct {
		name string
		// turns is a fault after a model call returned turns; running is the control.
		turns, running bool
	}{
		{name: "a first model call the provider fails"},
		{name: "a model call the provider fails after one returned", turns: true},
		{name: "control: the same fault while the project runs", running: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPauseRig(t)
			p.paused.Store(!c.running)
			p.cfg.Sweep = time.Hour
			p.restart()
			path := p.conversation("ses_tester", testerToken, 0)
			before := fileContent(t, path)
			p.answer = func(req answer.Request) (answer.Reply, error) {
				if p.answerCount() > 1 {
					return answered(req, "Plum, from "+req.Question.Text), nil
				}
				failed := answer.Reply{Kind: answer.NotAnswered, Fault: answer.ProviderError, Detail: "429: the gateway refuses the project's calls",
					Calls: 1, CostUSD: 0.1}
				if c.turns {
					failed.Calls = 2
					failed.Append = conversation.Append{QuestionID: req.Question.ID, Question: req.Question.Text, Provider: "anthropic",
						API: "anthropic-messages", Model: "claude-opus-5-5",
						Turns: []conversation.Turn{{Content: []conversation.Block{{Type: conversation.Text, Text: "Checking PR 7."}}}}}
				}
				return failed, nil
			}
			p.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
			kept := p.only().ID
			p.drain()
			q := p.question(kept)
			if c.running {
				posts := p.dispatch.posts()
				if q.State != store.QuestionPassedOn || len(p.rec.passed) != 1 || p.rec.passed[0].Reason != string(answer.ProviderError) || len(posts) != 1 ||
					!strings.HasPrefix(posts[0].Body["body"].(string), "My model provider failed to answer.") {
					t.Fatalf("while the project runs: question %s, passed on %+v, posts %s; want the daemon's reply posted and the question passed on",
						q.State, p.rec.passed, describe(posts))
				}
				return
			}
			switch {
			case p.answerCount() != 1:
				t.Fatalf("%d model calls while paused; want the one the provider failed", p.answerCount())
			case q.State != store.QuestionReceived:
				t.Fatalf("question = %s once its provider failed while paused; want received", q.State)
			case len(p.dispatch.posts()) != 0 || len(p.listener.posts()) != 0 || len(p.rec.passed) != 0:
				t.Fatalf("while paused: Dispatch posts %s, listener posts %s, passed on %+v; want none", describe(p.dispatch.posts()),
					describe(p.listener.posts()), p.rec.passed)
			case fileContent(t, path) != before:
				t.Fatalf("the conversation changed while paused:\n%s", fileContent(t, path)[len(before):])
			case p.spent() != 0.1:
				t.Fatalf("spend = %v; want the failed run's 0.1", p.spent())
			case strings.Count(p.log.String(), "answers: the model provider failed while the project is paused; the question waits for the resume") != 1 ||
				!strings.Contains(p.log.String(), "question="+q.ID):
				t.Fatalf("log:\n%s\nwant one line saying the question waits for the resume", p.log.String())
			}

			p.receive(messageHandoff("ses_tester", "m-2", 1, "And the linter?"))
			p.drain()
			for _, q := range p.questions() {
				if q.State != store.QuestionReceived {
					t.Fatalf("question %s = %s behind the kept one while paused; want received", q.ID, q.State)
				}
			}
			if p.answerCount() != 1 {
				t.Fatalf("%d model calls while paused; want no more after the kept question's", p.answerCount())
			}

			// The wake Receive left is taken first, so once the loop's first schedule has read the
			// pause only the resume's poke wakes it before its sweep.
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
			testwait.Eventually(t, "both questions posted after the resume", func() bool {
				for _, q := range p.questions() {
					if q.State != store.QuestionPosted {
						return false
					}
				}
				return true
			})
			p.mu.Lock()
			asked := []string{p.requests[1].Question.Text, p.requests[2].Question.Text}
			p.mu.Unlock()
			switch {
			case p.answerCount() != 3 || !slices.Equal(asked, []string{"Why plum?", "And the linter?"}):
				t.Fatalf("%d model calls, the ones after the resume asked %q; want the kept question answered again first, then the one behind it", p.answerCount(), asked)
			case len(p.dispatch.posts()) != 2 || len(p.rec.passed) != 0:
				t.Fatalf("after the resume: Dispatch posts %s, passed on %+v; want both answers posted and nothing passed on", describe(p.dispatch.posts()), p.rec.passed)
			case p.spent() != 0.6:
				t.Fatalf("spend = %v; want the failed run's 0.1 and both answers' 0.25", p.spent())
			}
		})
	}
}
