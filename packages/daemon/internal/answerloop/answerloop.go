// Package answerloop is the daemon's question pipeline: it takes every message the Envoy listener
// hands off for one of the daemon's agents that is not running (intake.Handoff), decides before any
// model call whether it is a question the agent answers (Classify), and answers each question from
// the agent's own conversation (internal/answer), adds the exchange to that conversation in its
// harness's format, and posts the reply in the agent's name where it was asked.
//
// A question is recorded once (answer_questions), by the message's identity and the agent it was
// handed to, which are stable across retries, restarts and a newer conversation of that agent,
// and moves received -> generating -> appended -> posted, or passed_on when the
// controller must hear of it (PassOn), forwarded when the agent runs its conversation, woken when
// its released agent is given it instead (Wakes), or dropped.
// Recovery follows the state and the conversation: a question not yet appended is answered again,
// the model handed the tool calls the earlier run made (a gh comment or review call of unknown
// outcome looked up on GitHub first, ghtool.Lookup; a dispatch call or a web search of unknown
// outcome told the model as such, dispatchtool.UnknownOutcome and websearch.UnknownOutcome, and
// neither looked up nor made again), unless the conversation already holds the exchange
// an earlier pass wrote, which its legion-answer marker names: that reply, recorded before the
// append, is posted and the model is not called again. One appended is posted without answering
// again, since its answer is in the conversation already.
//
// One writer: the questions to one conversation run one after another, at most
// answers.bounds.concurrent conversations at once. A conversation its claim runs, or is starting,
// is never written: the question goes to the running agent on its role topic, held until the role
// is held (Gate). The day's spend is kept per project and UTC day; over answers.bounds.daily_usd a
// question gets the daemon's reply in the agent's name and is passed on. So does a question to an
// agent whose claim is held for OpenAI's cyber-policy refusal of its session's content
// (Gate.HeldForCyberPolicy), and no model is called for it.
//
// An agent whose claim the daemon released (an idle release of its issue pod, or a project's
// pause: Gate.Released) is answered for as any stopped agent, and given everything else that
// reached it through one outbox row that wakes it (Wakes): a person's decision on its ask, every
// other hand-off that asks it nothing and is its own (Verdict.ForAgent: a Dispatch message,
// comment or reply, an edit or resolution of one, the answer to, a reply on, or the resolution or
// edit of an ask it follows, and a message of a source no agent answers; no act of its own
// session), a question whose reply the loop does not post (one the controller would hear of, or
// one that asks nothing), which is then neither appended nor posted, and the loop's own answer to
// it as an asker. The controller, the architect and the conversation are told none of these. A
// question the loop answered wakes nothing; one that came with a decision is answered, appended
// and posted first, and the decision's row then says where. Once the claim is held for OpenAI's
// cyber-policy refusal such a question wakes nothing, and its decision goes to Decisions as an
// unreleased agent's does (passHeld).
//
// While the project is paused (Config.Paused) the loop answers, posts and passes on as it does
// while the project runs, and the answer's tools run their reads and refuse every write
// (internal/answer). A model call the provider fails while the project is paused keeps its
// question for the resume (keepForResume): the question is received again, nothing is appended,
// posted or passed on, and no question of its conversation is taken until the project resumes.
package answerloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatchtool"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Store is the loop's record (*store.Store): the conversations the daemon's history holds, the
// questions, their gh calls, and the day's spend.
type Store interface {
	AgentConversation(ctx context.Context, session string) (store.AgentConversation, bool, error)
	RoleConversations(ctx context.Context, token claim.Token) ([]store.AgentConversation, error)
	ReceiveQuestion(ctx context.Context, q store.AnswerQuestion) (store.AnswerQuestion, bool, error)
	OpenQuestions(ctx context.Context, project string) ([]store.AnswerQuestion, error)
	UpdateQuestion(ctx context.Context, q store.AnswerQuestion, from ...store.QuestionState) error
	AnswerToolCalls(ctx context.Context, question string) ([]store.AnswerToolCall, error)
	FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error
	AnswerSpend(ctx context.Context, project string, at time.Time) (float64, error)
	AddAnswerSpend(ctx context.Context, project string, at time.Time, usd float64) error
}

// Dispatch is what the loop reads of Dispatch and writes there in the agent's name
// (*dispatch.HTTPClient): what Classify reads (Reader), and the replies.
type Dispatch interface {
	Reader
	ReplyToMessage(ctx context.Context, messageID string, attempt int, session, body string) error
	ReplyToComment(ctx context.Context, commentID, target string, attempt int, session, body string) error
	ReplyOnAsk(ctx context.Context, issue, askID, session, body string) error
	ReplyInThread(ctx context.Context, issue, commentID, session, body string) error
}

// Envoy sends the loop's messages through the listener (*notify.HTTPPublisher): an answer to an
// agent's message in the answering agent's name, and a question forwarded to the running agent on
// its role topic, which a publisher marks never to be handed off.
type Envoy interface {
	Send(ctx context.Context, m notify.Message) error
	Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error
}

// Gate keeps the loop and a claim's start off one conversation at once (the daemon's answerGate).
// Running is whether session's conversation is its claim's current one and the claim runs it, is
// starting or has been started. Append runs write while the claim cannot start, unless Running:
// then it runs nothing and answers false. Released is whether session's conversation is its
// claim's current one and the daemon released the claim (supervise.Claim.Released: an idle
// release of its issue pod, or a project's pause), which runs nothing and is not starting: the
// loop answers such an agent's questions as any stopped agent's, and gives it everything else it
// does not answer through Wakes. HeldForCyberPolicy is whether token's claim is held for OpenAI's
// cyber-policy refusal of its session's content (supervise.Claim.HeldForCyberPolicy): no action
// the daemon starts by itself may send that content to OpenAI again, so the loop calls no model
// for any conversation of such a claim (generate).
type Gate interface {
	Running(token claim.Token, session string) bool
	Released(token claim.Token, session string) bool
	HeldForCyberPolicy(token claim.Token) bool
	Append(token claim.Token, session string, write func() error) (bool, error)
}

// IssueFacts is what has changed on an agent's issue since it stopped, for the change note: the
// status Dispatch shows, the daemon's phase, the repository the issue works, and its pull request,
// whose repository and number the gh tool's description and its refusals name too (ghtool.Usage).
// Recorded is false for an issue the daemon holds no record of. HandoffCommits is, by role, the
// commit each role's phases row records it completed its round against
// (record.PhaseRow.HandoffCommit), a role whose row records none absent: the head the agent's
// recorded round read, which the gh tool names in a review's footer and submits a review at only
// while the pull request stands there (ghtool.Agent.Head).
type IssueFacts struct {
	Recorded       bool
	DispatchStatus string
	Phase          string
	Repository     ghrepo.Repository
	PullRequest    *PullRequestFacts
	HandoffCommits map[claim.Role]string
}

// PullRequestFacts is the daemon's latest observation of a pull request: its number, its state
// (open, merged, closed) and its head commit.
type PullRequestFacts struct {
	Number int
	State  string
	Head   string
}

// Issues reads an issue's facts from the daemon's record.
type Issues interface {
	Facts(ctx context.Context, issue string) (IssueFacts, error)
}

// Tool is one answer's gh tool (*ghtool.Answer, with ghtool.Lookup and its Close).
type Tool interface {
	answer.GH
	Lookup(ctx context.Context, seq int) (ghtool.Found, error)
	Close() error
}

// Tools opens the gh tool an answer runs with, as agent, for question.
type Tools interface {
	Open(ctx context.Context, agent ghtool.Agent, question string) (Tool, error)
}

// DispatchTool is one answer's dispatch tool (*dispatchtool.Answer, with its Close).
type DispatchTool interface {
	answer.Dispatch
	Close() error
}

// DispatchTools opens the dispatch tool an answer runs with, as the agent's session, for
// question, handed place, where the loop posts that question's reply (the loop's toolPlace), at
// which the tool refuses every write the model aims, a second reply there
// (dispatchtool.Place).
type DispatchTools interface {
	Open(session, question string, place dispatchtool.Place) (DispatchTool, error)
}

// WebSearchTools opens the web_search tool an answer runs with, for question
// (*websearch.Answer).
type WebSearchTools interface {
	Open(question string) (answer.WebSearch, error)
}

// PassOn tells the controller of a question the agent's reply does not settle: one not answered, a
// request for work, one past a bound or a fault, or over the daily limit. reason is the Reason,
// origin the question as its hand-off carries it: its text, who asked (Asker) and where (Ref, a
// dispatch:// reference or the Envoy envelope). It is called once the reply is posted, and again
// only when a restart finds the question not yet marked passed on, so an implementation passes a
// question once by its id. A question of an agent whose claim is released is never passed on: the
// loop posts nothing for it and gives it to the agent itself through Wakes.
type PassOn interface {
	PassOn(ctx context.Context, question store.AnswerQuestion, reason Reason, origin Origin) error
}

// Decision is a person's decision on an ask the stopped agent opened: their answer to the ask
// (ask.answered), or their reply that hands the ask back to the agent (intake.DispatchEvent's
// HandsTurnBack). ID is the decision's own id, the same however often it is handed off:
// `dispatch:<event id>` of the answer, `dispatch:<comment id>` of the reply. Asker is the person
// (`user:<login>`), Ref where they decided (`dispatch://<KEY>/ask/<id>` for an answer,
// `dispatch://<KEY>/comment/<id>` for a reply), Text what they decided: the options they selected
// and their text. Conversation is the agent's.
//
// Answered says which of the two it is — the person's answer of the ask, against a reply on its
// thread — and AskKind is that ask's kind as the answer's event names it
// (intake.DispatchPayload.Kind: `question`, or one the daemon does not know, a legacy `action`
// ask's among them); a reply's event names none, so a reply carries "". The two together say
// whether the parked-decision relay sends a decision of that kind at all (record.RelaysAskAnswer).
//
// AskIssue is the issue the ask stands on, which the event names and Ref is built from. It is the
// agent's own issue for an ordinary ask and another issue when the agent opened its ask
// elsewhere — a phase worker's finding on the controller's report issue
// (skill://legion-worker/references/findings.md) — and it is the issue the parked-decision relay
// reads, so it, and not the agent's conversation issue, says whether that relay carries this
// decision (workflow.RelaysDecision, the daemon's routedDecisions). A decision made where no
// issue is named carries "".
type Decision struct {
	ID           string
	Asker        string
	Ref          string
	Text         string
	Answered     bool
	AskKind      string
	AskIssue     string
	Conversation store.AgentConversation
}

// Decisions passes on the decisions the hand-offs carry (plan 2.9): to the architect that owns the
// agent's issue while it holds its role, else to the controller. Receive calls it for every
// hand-off carrying one, a redelivery and a restart's included, so an implementation passes a
// decision once by its ID. A decision for an agent whose claim is released goes to the agent
// itself through Wakes instead, and the architect and the controller are told nothing of it.
type Decisions interface {
	Decide(ctx context.Context, d Decision) error
}

// Wakes gives an agent whose claim the daemon released what reaches it that the loop does not
// answer for it (the daemon's releasedWakes): one outbox row per message, which wakes the claim
// and gives the message to the woken session once it holds its role, written once by the
// message's Key however often the loop asks, so an implementation writes nothing for a key it
// wrote before. The loop asks for a decision on the agent's ask in place of passing it to
// Decisions, for every other hand-off that asks the agent nothing and is its own
// (Verdict.ForAgent), for a question whose reply it does not post in place of posting it and
// passing it on (PassOn), and for its own answer to the agent as an asker in place of writing it
// into the conversation. A question that also carries a decision is asked for once its answer is
// appended and posted, so the woken agent finds the answer in its conversation.
type Wakes interface {
	Wake(ctx context.Context, w Wake) error
}

// Wake is one message for a released agent: Key its own id (QuestionID of the message's identity
// and the agent's role token, which for a question is the question's id), Issue, Role and Session
// the agent's conversation's, and Text what the woken agent is told (wakeMessage).
type Wake struct {
	Key     string
	Issue   string
	Role    claim.Role
	Session string
	Text    string
}

// Reason is why a question is passed on: the kind of reply the agent gave, a bound or fault the
// answer met (answer.Fault), or what the daemon itself found.
type Reason string

const (
	ReasonNotAnswered    Reason = "not_answered"
	ReasonNeedsWork      Reason = "needs_work"
	ReasonOverLimit      Reason = "over_limit"
	ReasonNoConversation Reason = "no_conversation"
	ReasonUnknownFormat  Reason = Reason(answer.UnknownFormat)
	ReasonNoProvider     Reason = Reason(answer.NoProvider)
	ReasonNoPrice        Reason = Reason(answer.NoPrice)
	ReasonUnmetered      Reason = Reason(answer.Unmetered)
	// ReasonPostRefused is a reply Dispatch or the listener refused for good.
	ReasonPostRefused Reason = "post_refused"
	// ReasonHeldForCyberPolicy is a question to an agent whose claim is held for OpenAI's
	// cyber-policy refusal of its session's content (Gate.HeldForCyberPolicy), for which the loop
	// calls no model.
	ReasonHeldForCyberPolicy Reason = "held_for_cyber_policy"
)

// Config is the loop's configuration and collaborators.
type Config struct {
	// Project is the Legion project token (claim.ProjectToken) the loop answers for.
	Project string
	// Answers is `answers`: providers, prices and bounds.
	Answers       config.Answers
	Store         Store
	Dispatch      Dispatch
	Envoy         Envoy
	Gate          Gate
	Issues        Issues
	Tools         Tools
	Conversations Conversations
	PassOn        PassOn
	Decisions     Decisions
	Wakes         Wakes
	// DispatchTools opens each answer's dispatch tool; nil is a daemon that found no dispatch
	// command (or no bun for it), whose answers are offered gh alone.
	DispatchTools DispatchTools
	// WebSearchTools opens each answer's web_search tool; nil is a daemon whose answers configures
	// none (config.Answers.WebSearchOff), whose answers are offered no web_search tool.
	WebSearchTools WebSearchTools
	// AppID is the configured id of a role's GitHub App (config.GitHubApps), which the gh tool's
	// log line names.
	AppID func(claim.Role) string
	// Answer generates an answer: answer.Answer.
	Answer func(ctx context.Context, req answer.Request) (answer.Reply, error)
	// Paused says whether the project is paused (the daemon's pause flag, pause.Service.Paused);
	// nil is never paused. While it is, each answer is handed the flag, so its tools refuse every
	// write (answer.Request.Paused), and a model call the provider fails keeps its question for
	// the resume (keepForResume), whose Poke answers it.
	Paused func() bool
	Now    func() time.Time
	Log    *slog.Logger
	// Sweep is how often the loop looks again at the questions it holds: a held forward, and a
	// question an error left (30 s when zero).
	Sweep time.Duration
}

// Loop is the daemon's question pipeline.
type Loop struct {
	cfg  Config
	wake chan struct{}

	mu      sync.Mutex
	running map[string]bool
	// waiting is whether the last schedule left a conversation for want of a free worker.
	waiting bool
	// kept are the sessions whose question a model call the provider failed while the project was
	// paused left for the resume (keepForResume): schedule starts no worker for one while the
	// project is paused, and forgets them all once it reads the project running. It is the loop's
	// memory alone, so a daemon restarted during the pause answers each such question once more.
	kept    map[string]bool
	workers sync.WaitGroup

	// leftForSlot is a test hook, nil outside tests: schedule calls it, under mu, with the session
	// it leaves waiting for want of a free worker.
	leftForSlot func(session string)
}

// New is the loop over cfg.
func New(cfg Config) *Loop {
	if cfg.Sweep <= 0 {
		cfg.Sweep = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Loop{cfg: cfg, wake: make(chan struct{}, 1), running: map[string]bool{}, kept: map[string]bool{}}
}

// Run works the questions until ctx ends, then waits for the answers in flight to end.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Sweep)
	defer ticker.Stop()
	defer l.workers.Wait()
	for {
		l.schedule(ctx)
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-ticker.C:
		}
	}
}

// Poke wakes the loop to schedule now rather than at its next sweep: a question received, a
// worker that leaves a conversation waiting for its slot, and the project's resume, which takes
// the questions a provider fault during the pause kept (keepForResume).
func (l *Loop) Poke() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// paused is whether the project is paused (Config.Paused).
func (l *Loop) paused() bool { return l.cfg.Paused != nil && l.cfg.Paused() }

// schedule starts a worker for each conversation with open questions that has none, oldest
// question first, while fewer than the bound run. A conversation the bound leaves waiting is
// scheduled as soon as a worker ends. While the project is paused it starts none for a
// conversation whose question a provider fault kept for the resume (kept), and once the project
// runs it forgets every such conversation, which it schedules as any other.
func (l *Loop) schedule(ctx context.Context) {
	open, err := l.cfg.Store.OpenQuestions(ctx, l.cfg.Project)
	if err != nil {
		if ctx.Err() == nil {
			l.cfg.Log.Error("answers: could not read the open questions; trying again at the next sweep", "error", err)
		}
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.kept) > 0 && !l.paused() {
		clear(l.kept)
	}
	l.waiting = false
	for _, q := range open {
		if l.running[q.Session] || l.kept[q.Session] {
			continue
		}
		if len(l.running) >= max(l.cfg.Answers.Bounds.Concurrent, 1) {
			l.waiting = true
			if l.leftForSlot != nil {
				l.leftForSlot(q.Session)
			}
			return
		}
		l.running[q.Session] = true
		l.workers.Go(func() {
			defer func() {
				l.mu.Lock()
				delete(l.running, q.Session)
				waiting := l.waiting
				l.mu.Unlock()
				if waiting {
					l.Poke()
				}
			}()
			l.work(ctx, q.Session)
		})
	}
}

// work runs session's open questions one after another, in the order they were received, until
// none is left to work now: a held forward is tried once a pass, and an error ends the pass, which
// the next sweep tries again. A question a provider fault kept for the resume ends the pass too
// (errKeptForResume), and the questions after it wait with it, so its conversation is still
// answered in the order it was asked.
func (l *Loop) work(ctx context.Context, session string) {
	tried := map[string]store.QuestionState{}
	for ctx.Err() == nil {
		open, err := l.cfg.Store.OpenQuestions(ctx, l.cfg.Project)
		if err != nil {
			l.cfg.Log.Error("answers: could not read the open questions; trying again at the next sweep", "session", session, "error", err)
			return
		}
		next := slices.IndexFunc(open, func(q store.AnswerQuestion) bool {
			state, seen := tried[q.ID]
			return q.Session == session && (!seen || state != q.State)
		})
		if next < 0 {
			return
		}
		q := open[next]
		tried[q.ID] = q.State
		if err := l.process(ctx, q); err != nil {
			if !errors.Is(err, errKeptForResume) && ctx.Err() == nil {
				l.cfg.Log.Error("answers: question not finished; trying again at the next sweep", "question", q.ID, "issue", q.Issue,
					"role", q.Role, "session", q.Session, "state", q.State, "error", err)
			}
			return
		}
	}
}

// Receive is the hand-off consumer's handler (intake.HandoffHandler): it finds the conversation the
// hand-off is for in the daemon's history, classifies it, delivers the loop's own answer into the
// asking agent's conversation (deliver), passes on the decision it carries (Decisions), records
// the question it asks, and wakes the loop. A hand-off for a session the history does not hold,
// one that is no question and carries no decision it passes on, and a delivery whose
// conversation cannot be read are logged and dropped, and Receive answers dropped: nothing of
// them is recorded, so a replay of such a hand-off closes it as no fact (intake's
// replayHandoff). A nil error acknowledges it. An error leaves it to be delivered again, and
// given up and kept as an intake message is once the intake's bound is reached
// (intake.OpenHandoffs).
//
// An agent whose claim the daemon released (Gate.Released, read here) has its questions answered
// as any stopped agent's, and is given through Wakes, once each, everything else that reached it
// and that is the agent's own: the loop's answer to it as an asker, in place of writing it into
// the conversation; a person's decision on its ask that asks nothing besides, in place of
// Decisions; and every other hand-off that is no question and no decision (Verdict.ForAgent):
// each Dispatch event that reached it, such as a message, comment or reply that asks nothing or
// the resolution, answer or edit of an ask it follows, and a message of a source no agent
// answers. A reply on, or an answer to, an ask the agent follows and did not open is told as made
// on an ask it follows (Verdict.Followed), never on its own. A decision that also asks is held on
// its question (store.AnswerQuestion.Wake) and given once the answer is appended and posted, so
// the woken agent finds the answer in its conversation. The daemon's own notices and posts, the
// broker's messages, anything carrying the loop's own mark, and a person's review of the agent's
// own approval request, which the design gate relays to its architect (Verdict.ForArchitect),
// are none of the agent's, and reach it neither way; nor does an act of the agent's own session
// (actedByAgent), such as its own reply or its own hand-back of its approval request. A claim
// that is not released is passed its decision as before.
func (l *Loop) Receive(ctx context.Context, h intake.Handoff, raw []byte) (dropped bool, err error) {
	conv, ok, err := l.conversationFor(ctx, h)
	if err != nil {
		return false, err
	}
	if !ok {
		l.cfg.Log.Warn("answers: hand-off dropped: the daemon's history holds no such conversation", "event", h.Envelope.EventID,
			"targetSession", h.TargetSession, "roleToken", h.RoleToken, "topic", h.OriginalTopic)
		return true, nil
	}
	verdict, err := Classify(ctx, h, conv, l.cfg.Dispatch)
	if err != nil {
		return false, err
	}
	released := l.cfg.Gate.Released(conv.Token, conv.Session)
	switch {
	case verdict.Delivery != nil && released:
		return false, l.wakeAgent(ctx, conv, verdict.Delivery.Identity, wakeMessage(*verdict.Delivery), "the loop's own answer to the agent")
	case verdict.Delivery != nil:
		return l.deliver(ctx, h, conv, *verdict.Delivery)
	case verdict.Decision != nil && !released:
		if err := l.cfg.Decisions.Decide(ctx, *verdict.Decision); err != nil {
			return false, fmt.Errorf("pass on the decision %s of %s: %w", verdict.Decision.ID, h.Envelope.EventID, err)
		}
	}
	if verdict.Question == nil {
		if released && (verdict.Decision != nil || verdict.ForAgent) {
			identity, text := handoffMessage(h, verdict.Followed)
			return false, l.wakeAgent(ctx, conv, identity, text, verdict.Because)
		}
		l.cfg.Log.Info("answers: hand-off is no question; not answered", "event", h.Envelope.EventID, "issue", conv.Issue,
			"role", conv.Role, "session", conv.Session, "because", verdict.Because)
		// A decision passed on above is the hand-off's record; with none, nothing of it is kept.
		return verdict.Decision == nil, nil
	}
	origin := verdict.Question
	now := l.cfg.Now().UTC()
	q, inserted, err := l.cfg.Store.ReceiveQuestion(ctx, store.AnswerQuestion{
		ID: QuestionID(l.cfg.Project, origin.Identity, string(conv.Token)), Project: l.cfg.Project, Identity: origin.Identity,
		Session: conv.Session, Token: conv.Token, Issue: conv.Issue, Role: conv.Role, Kind: string(origin.Kind),
		MayAskNothing: origin.MayAskNothing, Handoff: raw, Wake: released && verdict.Decision != nil, ReceivedAt: now,
	})
	if err != nil {
		return false, err
	}
	if !inserted {
		l.cfg.Log.Info("answers: question handed off again; not answered again", "question", q.ID, "issue", q.Issue, "role", q.Role,
			"session", q.Session, "state", q.State, "deliveries", q.Deliveries)
		return false, nil
	}
	l.logState(q, "")
	l.Poke()
	return false, nil
}

// wakeAgent gives text to conv's agent, whose claim the daemon released, through Wakes, keyed by
// the message's identity and the agent's role token (QuestionID), so the message is given once
// however often it is handed off. because says what the message is, for the log.
func (l *Loop) wakeAgent(ctx context.Context, conv store.AgentConversation, identity, text, because string) error {
	key := QuestionID(l.cfg.Project, identity, string(conv.Token))
	if err := l.cfg.Wakes.Wake(ctx, Wake{Key: key, Issue: conv.Issue, Role: conv.Role, Session: conv.Session, Text: text}); err != nil {
		return fmt.Errorf("give %s to the released %s: %w", identity, conv.Session, err)
	}
	l.cfg.Log.Info("answers: the message goes to the released agent through its wake", "message", identity, "key", key,
		"issue", conv.Issue, "role", conv.Role, "session", conv.Session, "because", because)
	return nil
}

// wakeMessage is a message as its released agent is told it through Wakes: who sent it, where and
// under which reference, then the message word for word.
func wakeMessage(o Origin) string {
	return fmt.Sprintf("%s sent you, in %s (%s), while you were stopped:\n%s", o.Asker, o.Where, o.Ref, o.Text)
}

// handoffMessage is a hand-off that asks nothing as its released agent is told it (wakeMessage),
// and the message's identity, the one a question of it would have (Origin.Identity), which the
// same message carries at every delivery attempt. Only a Dispatch event and an agent session's
// message are read by ReadOrigin, as Classify reads them, and a reply on, or an answer to, an ask
// the agent follows and did not open (followed, Verdict.Followed) is told as made on an ask it
// follows (readOrigin), so the agent does not take another agent's ask, or a decision made on it,
// for its own. A hand-off of any other source, and one ReadOrigin reads nothing of, is told as
// its sender and where it was sent, then its words: a Dispatch event's actor, type and issue,
// then the item's body, else the envelope's source and topic, then its payload; its identity is
// the envelope's dedupe key.
func handoffMessage(h intake.Handoff, followed bool) (identity, text string) {
	env := h.Envelope
	if env.Source == "dispatch" || env.Source == "agent" && env.SourceSession != "" {
		if origin, err := readOrigin(h, followed); err == nil {
			return origin.Identity, wakeMessage(origin)
		}
	}
	words := env.Payload
	if words == "" {
		words = env.PayloadSummary
	}
	sender, where := env.Source, "a message on "+env.Topic
	if env.SourceSession != "" {
		sender += " session " + env.SourceSession
	}
	if event, err := intake.DispatchOf(env); err == nil {
		sender, where = event.Actor.Kind+":"+event.Actor.ID, "a Dispatch "+event.Type
		if issue := eventIssue(event); issue != "" {
			where += " on " + issue
		}
		if event.Payload.Body != "" {
			words = event.Payload.Body
		}
	}
	return env.DedupeKey, fmt.Sprintf("%s sent you, in %s, while you were stopped:\n%s", sender, where, words)
}

// answeredNote is the sentence a released agent is told after a message whose question the loop
// answered (wakeMessage): where the answer was posted, so it does not answer it again.
func answeredNote(o Origin) string {
	return fmt.Sprintf("\n\nThe Legion daemon already answered the question in this message for you, from your conversation, which holds the answer, "+
		"and posted that answer in your name in %s (%s): do not answer it again.", o.Where, o.Ref)
}

// deliver gives the loop's own answer, handed back because the agent it was sent to was stopped,
// to that agent: written into its conversation as a message it received, so it holds the answer
// when it runs again, word for word as the listener would have delivered it live. Nothing is
// answered, billed or posted for it. The delivery's identity is the envelope's dedupe key, which
// the append records beside the message as it records a question's id, so a hand-off delivered
// again and one the daemon is handed after a restart write nothing more (Session.Answered). An
// asker whose claim the daemon released is given the answer through Wakes instead, and its
// conversation is not written (Receive).
//
// A claim that runs the conversation takes the answer live instead, as a forwarded question goes
// to a running agent: the loop sends it to the agent's own session through the listener, for a
// live session and nobody else (notify.Message's LiveOnly), and the hand-off is done with. A
// send the listener refuses is an agent that stopped again in that race, and its answer is
// written into the conversation after all, which leaves no message going round between the two.
//
// A conversation the reader refuses, or its store no longer holds, is logged and the message
// dropped, which deliver answers (dropped): no later delivery of it would read any better. A
// conversation that moved since it was read is an error, so the hand-off comes again and is
// written after a fresh read.
func (l *Loop) deliver(ctx context.Context, h intake.Handoff, conv store.AgentConversation, d Origin) (dropped bool, err error) {
	session, err := l.cfg.Conversations.Open(ctx, conv)
	switch {
	case errors.Is(err, conversation.ErrUnknownFormat), errors.Is(err, conversationMissing):
		l.cfg.Log.Warn("answers: the loop's own answer is not delivered: the agent's conversation cannot be read",
			"delivery", d.Identity, "session", conv.Session, "error", err)
		return true, nil
	case err != nil:
		return false, err
	}
	defer session.Close()
	if session.Answered(d.Identity) {
		l.cfg.Log.Info("answers: the asking agent's conversation already holds this answer; it is not delivered again",
			"delivery", d.Identity, "issue", conv.Issue, "role", conv.Role, "session", conv.Session)
		return false, nil
	}
	written, err := l.writeDelivery(ctx, session, conv, d)
	if err != nil {
		return false, err
	}
	if written {
		return false, nil
	}
	// The agent is running: it takes the message the way the listener would have delivered it,
	// its sender's own words. The idempotency key is the envelope's dedupe key, so this send is
	// one message however often it is tried, and not the key the refused send to the stopped
	// agent already used.
	err = l.cfg.Envoy.Send(ctx, notify.Message{
		Target: conv.Session, SourceSession: d.Sender, Text: d.Text, InReplyTo: h.Envelope.InReplyTo,
		IdempotencyKey: d.Identity, LiveOnly: true,
	})
	if err == nil {
		l.cfg.Log.Info("answers: the loop's own answer is given to the asking agent live; its claim runs the conversation",
			"delivery", d.Identity, "issue", conv.Issue, "role", conv.Role, "session", conv.Session)
		return false, nil
	}
	if !errors.Is(err, notify.ErrRefused) {
		return false, fmt.Errorf("give the answer %s to the running %s: %w", d.Identity, conv.Session, err)
	}
	l.cfg.Log.Info("answers: the asking agent stopped before the answer reached it live; it is written into its conversation",
		"delivery", d.Identity, "session", conv.Session, "error", err)
	written, err = l.writeDelivery(ctx, session, conv, d)
	switch {
	case err != nil:
		return false, err
	case !written:
		return false, fmt.Errorf("deliver the answer %s to %s: its claim runs the conversation again", d.Identity, conv.Session)
	}
	return false, nil
}

// writeDelivery writes d into the agent's conversation under the gate, and reports whether it
// was written: false, and no error, for a claim that runs the conversation, which the loop
// answers by giving the message to the running agent instead.
func (l *Loop) writeDelivery(ctx context.Context, session Session, conv store.AgentConversation, d Origin) (bool, error) {
	ran, err := l.cfg.Gate.Append(conv.Token, conv.Session, func() error {
		return session.Deliver(ctx, conversation.Delivery{ID: d.Identity, Text: deliveredMessage(d)}, l.cfg.Now())
	})
	switch {
	case !ran:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("deliver the answer %s to %s: %w", d.Identity, conv.Session, err)
	}
	l.cfg.Log.Info("answers: the loop's own answer is delivered into the asking agent's conversation",
		"delivery", d.Identity, "issue", conv.Issue, "role", conv.Role, "session", conv.Session)
	return true, nil
}

// deliveredMessage is a delivery as the agent reads it in its conversation: who sent it and
// where, then the message word for word, which is the shape the question side of an exchange is
// written in (internal/answer's userMessage).
func deliveredMessage(d Origin) string {
	return fmt.Sprintf("%s sent you, in %s:\n%s", d.Asker, d.Where, d.Text)
}

// conversationFor is the conversation a hand-off is answered from: for a message to a role, the
// role's newest conversation; for one to a session, that session's, an older session of a role a
// newer one replaced included. A conversation of another project is none of this daemon's.
func (l *Loop) conversationFor(ctx context.Context, h intake.Handoff) (store.AgentConversation, bool, error) {
	var conv store.AgentConversation
	if token, isRole := strings.CutPrefix(h.OriginalTopic, notify.RoleTopicPrefix); isRole {
		role, err := l.cfg.Store.RoleConversations(ctx, claim.Token(token))
		if err != nil {
			return store.AgentConversation{}, false, err
		}
		if len(role) == 0 {
			return store.AgentConversation{}, false, nil
		}
		conv = role[0]
	} else {
		found, ok, err := l.cfg.Store.AgentConversation(ctx, h.TargetSession)
		if err != nil || !ok {
			return store.AgentConversation{}, false, err
		}
		conv = found
	}
	return conv, conv.Project == l.cfg.Project, nil
}

// logState is a question's one line per transition.
func (l *Loop) logState(q store.AnswerQuestion, reason Reason) {
	attrs := []any{"question", q.ID, "issue", q.Issue, "role", q.Role, "session", q.Session, "state", q.State, "kind", q.Kind}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	l.cfg.Log.Info("answers: question", attrs...)
}

// move writes q in its new state, from one of from, and logs the transition.
func (l *Loop) move(ctx context.Context, q *store.AnswerQuestion, to store.QuestionState, from ...store.QuestionState) error {
	next := *q
	next.State, next.UpdatedAt = to, l.cfg.Now().UTC()
	if err := l.cfg.Store.UpdateQuestion(ctx, next, from...); err != nil {
		return err
	}
	*q = next
	l.logState(next, Reason(next.PassReason))
	return nil
}
