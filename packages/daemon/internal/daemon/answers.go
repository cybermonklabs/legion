package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// answering is the daemon's answer loop, where it is on (config.Answers settled): the Legion
// project token it answers for, the hand-off consumer connect opens, and the loop attachAnswers
// builds once supervision runs (the gate is supervision's).
type answering struct {
	project  string
	handoffs *intake.Handoffs
	loop     *answerloop.Loop
}

// attachAnswers builds the answer loop over its configuration (answersLoop), and under kubernetes
// first installs the shared public leak check its gh tool's guard runs (answersTools), refusing
// the boot only over a cause that is not GitHub's trouble (appTransient). stop ends the install
// where it still runs in the background and waits for it to end, which the daemon does as it
// stops. Without `answers` it builds nothing, and stop does nothing.
func (w *workflowRuntime) attachAnswers(ctx context.Context, cfg config.Config, p plan, s *supervision, st *store.Store) (stop func(), err error) {
	if w.answers == nil {
		return func() {}, nil
	}
	client, ok := w.dispatch.(answerloop.Dispatch)
	if !ok {
		return nil, fmt.Errorf("the answer loop: the daemon's Dispatch client (%T) cannot post in an agent's name", w.dispatch)
	}
	tools, stop, err := w.answersTools(ctx, cfg, p, s.runtime, st)
	if err != nil {
		return nil, fmt.Errorf("the answer loop: %w", err)
	}
	w.answers.loop = answerloop.New(w.answersLoop(cfg, p, s, st, client, tools))
	return stop, nil
}

// answersLoop is the answer loop's configuration over the daemon's store, its Dispatch client
// (client, posting in an agent's name), its Envoy listener, supervision's answer gate, the issue
// record, the gh tool (tools, answersTools'), the conversation store and the project's pause flag.
func (w *workflowRuntime) answersLoop(cfg config.Config, p plan, s *supervision, st *store.Store, client answerloop.Dispatch, tools ghTools) answerloop.Config {
	// The loop reads the flag the machines read (supervise.Deps.Paused), and the resume pokes it
	// (projectPause.Resumed). A daemon that holds no pause leaves it never paused.
	var paused func() bool
	if w.pauses != nil {
		paused = func() bool {
			_, on := w.pauses.service.Paused()
			return on
		}
	}
	return answerloop.Config{
		Project: p.project, Answers: *cfg.Answers, Store: st, Dispatch: client,
		Envoy: notify.New(cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"]), Gate: s.answerGate,
		Issues: recordFacts{w}, Tools: tools,
		Conversations: answerloop.Harnesses{OpenSQL: conversationStore(*cfg.Answers, p.answers)},
		PassOn:        controllerPassOn{pool: w.pool, records: w.records, now: time.Now, log: w.log},
		Decisions:     routedDecisions{outbox: w.outbox, now: time.Now, log: w.log},
		AppID: func(role claim.Role) string {
			if appauth.AppRoleFor(role) == appauth.Implement {
				return cfg.GitHubApps.Implement.AppID
			}
			return cfg.GitHubApps.Review.AppID
		},
		Answer: answer.Answer, Paused: paused, Now: time.Now, Log: w.log,
	}
}

// answersTools is the answer loop's gh tool (ghTools): the daemon's host every answer opens under,
// with the bounds of cfg's answers, and stop, which ends the check's install where it still runs
// (installAnswersLeakCheck). The shared public leak check the host names, which the tool hands
// the guard at every call (ghtool.Host.LeakCheck), comes from one place per runtime and from
// nowhere else:
//
//   - Under kubernetes, where boot read the deployment's setting and the pin (answersLeakCheck), it
//     is the copy the daemon installs at that pin from the setting's repository through rt's
//     reader, which only the Agent Sandbox runtime holds and which every pod's launcher is pushed
//     the check through, held to the setting's manifest digest (installAnswersLeakCheck). A
//     Kubernetes daemon's environment holds none of the three places a check is resolved from.
//   - Under tmux it is the machine's own check, resolved at each call in the daemon's own
//     environment, the one the host names (ghtool.MachineLeakCheck), as the tmux runtime
//     resolves a pane's at each launch (runtime/tmux's resolveLeakCheck): an operator's machine
//     installs, moves and republishes the check while the daemon runs. Boot refused a machine
//     that carried none (answersLeakCheck); a call made once it carries none is refused with the
//     setup failure naming the missing check.
//
// Under either runtime the host carries the setting's manifest digest, which the guard is told as
// leakguard.CheckerVar and holds the check to, as a pane's and a pod's guard do: a check of another
// digest refuses the call (ghtool.Host.LeakCheckManifestSHA256). A tmux deployment that names no
// setting tells the guard none, and its machine's check runs as it is.
func (w *workflowRuntime) answersTools(ctx context.Context, cfg config.Config, p plan, rt runtime.Runtime, st *store.Store) (ghTools, func(), error) {
	legion, err := os.Executable()
	if err != nil {
		return ghTools{}, nil, fmt.Errorf("resolve this daemon's executable for the gh tool's wrapper: %w", err)
	}
	// environ is the daemon's own environment, which the guard runs the check and its own
	// visibility read in (ghtool.Host), and which a tmux daemon's check is resolved in.
	environ := os.Environ()
	var check ghtool.LeakChecker = ghtool.MachineLeakCheck{Environ: environ}
	stop := func() {}
	if tag := p.answers.leakCheckTag; tag != "" {
		sandboxed, ok := rt.(*sandbox.Runtime)
		if !ok {
			return ghTools{}, nil, fmt.Errorf("a Kubernetes daemon installs the gh tool's shared public leak check through the Agent Sandbox runtime's reader, not %T's", rt)
		}
		installed, stopInstall, err := installAnswersLeakCheck(ctx, cfg.StateDir, tag, cfg.LeakCheck, sandboxed.LeakCheck(), w.log)
		if err != nil {
			return ghTools{}, nil, err
		}
		check, stop = installed, stopInstall
	} else {
		w.log.Info("answers: the gh tool's leak guard runs the machine's shared public leak check, resolved at each call", "check", p.answers.leakCheck)
	}
	// Credential builds a role's bundle as a pane's is built, over the daemon's token source
	// whole (w.apps), since a bundle reads the owners each App is installed on. It records what it
	// read in no record of the daemon's (plan.installed, plan.left): those are what the daemon's
	// launches and its drift check read, and an answer's bundle is neither.
	host := ghtool.Host{
		GH: p.answers.gh, Git: p.tools["git"], Legion: legion, StateDir: cfg.StateDir,
		LeakCheck: check, LeakCheckManifestSHA256: cfg.LeakCheck.ManifestSHA256, Environ: environ,
		Credential: appauth.GitHubCredential(w.apps, p.repositories.servesRepository, cfg.RepositoryRenames, nil, nil, w.log), Calls: st, Log: w.log, Now: time.Now,
	}
	return ghTools{host: host, bounds: cfg.Answers.Bounds}, stop, nil
}

// installAnswersLeakCheck is the shared public leak check a Kubernetes daemon's gh tool runs before
// every call: the copy reader serves at tag, the pin boot read (answersLeakCheck), from the
// repository setting names (leakguard.Setting.Repository), held to setting's manifest digest and
// installed under stateDir (ghtool.LeakCheck.Install). reader is the Sandbox runtime's own
// (sandbox.Runtime.LeakCheck), the one reader of setting every pod's launcher is pushed the check
// through (leakCheckSource), so the daemon reads a tag once for both. nil is a daemon with no
// GitHub App to read the check with, refused as the runtime refuses the start of such a daemon's
// pods.
//
// The boot makes one try of the install (ghtool.LeakCheck.Install). A cause that is not GitHub's
// trouble refuses the boot, read by the boot App mint's own predicate (appTransient) over the
// reader's failures as transientReads reports them: a tarball read answered 404, a 403 that is no
// rate limit, or a 401 where the reader cannot forget its token, a tarball that does not pack or
// misses a file of the manifest, a tag the reader refuses, GitHub refusing the narrowing of the
// read's token, an App with no installation on the check's owner, any other mint failure GitHub
// did not report as its own trouble, a served check whose manifest digest is not setting's, which
// a pod built under setting is pushed none of either (runtime/sandbox's serveLeakCheck), and a
// failed install of what was read, the `--help` run of a check that does not answer among them.
// GitHub's trouble is waited out, as the boot waits it out (run's boot rule): the transport
// failing, the read's bound passing, an HTTP 5xx answer, GitHub's rate limit (a 429 or a
// rate-limited 403), a 401 to a token the reader then forgets, so the next try mints a fresh one,
// and a mint GitHub failed so. The check is then answered not installed, so the gh tool refuses
// each call with the setup failure naming the pin and the last try's cause (ghtool.LeakCheck), and
// the install is tried again in the background on appMintRetry's backoff, 5 s doubling to 30 s,
// each failed try logged with its cause and the wait before the next, until one lands, one meets a
// cause that is not GitHub's trouble, or ctx ends. Such a cause, a manifest digest that is not
// setting's among them, ends the install: it is logged once at Error with the tag, and the gh tool
// refuses every call with it until the daemon's next boot (ghtool.LeakCheck.Stop). stop ends the
// background install and waits for it.
//
// Every boot installs again, over what an earlier boot installed there (leakguard.InstallServed),
// as each new launcher process installs over what its pod's directory held. So a re-pin, which
// moves the worker image's pin and with it the environment of a daemon run from that image, is
// installed by the daemon's next boot, over the copy of the old tag, logging the one line that
// says so.
func installAnswersLeakCheck(ctx context.Context, stateDir, tag string, setting leakguard.Setting, reader sandbox.LeakCheckReader, log *slog.Logger) (*ghtool.LeakCheck, func(), error) {
	if reader == nil {
		return nil, nil, errors.New(leakguard.Cause("the daemon runs with no GitHub App, so it cannot read "+setting.Repository.String(), nil))
	}
	source := transientReads{reader: reader}
	check := ghtool.NewLeakCheck(tag, setting.ManifestSHA256)
	cause := check.Install(ctx, source, stateDir, log)
	switch {
	case cause == nil:
		return check, func() {}, nil
	case ctx.Err() != nil || !appTransient(cause):
		return nil, nil, cause
	}
	installing, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for retry := appMintRetry.Initial; ; retry = min(2*retry, appMintRetry.Max) {
			log.Warn("answers: the shared public leak check is not installed, so the gh tool refuses every call until it is; installing it again",
				"tag", tag, "retryIn", retry.String(), "error", cause)
			wait := time.NewTimer(retry)
			select {
			case <-installing.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
			cause = check.Install(installing, source, stateDir, log)
			switch {
			case cause == nil || installing.Err() != nil:
				return
			case !appTransient(cause):
				check.Stop(cause)
				log.Error("answers: the shared public leak check cannot be installed, so the gh tool refuses every call until the daemon's next boot; installing it no more",
					"tag", tag, "error", cause)
				return
			}
		}
	}()
	return check, func() { cancel(); <-done }, nil
}

// transientReads is reader as the daemon's own install reads it (installAnswersLeakCheck): a read
// GitHub's trouble failed (leakguard.TransientRead) is reported as an *appauth.TransientError,
// whose text is the read's own cause sentence, so the one predicate the daemon tells GitHub's
// trouble by (appTransient) reads the tarball read as it reads the read's mint, whose own error
// the refusal wraps. leakguard imports nothing of the App's, and the pods' launchers, which the
// same reader serves, read no such line.
type transientReads struct{ reader sandbox.LeakCheckReader }

func (r transientReads) Get(ctx context.Context, tag string) (leakguard.Served, error) {
	served, err := r.reader.Get(ctx, tag)
	if leakguard.TransientRead(err) {
		return served, &appauth.TransientError{Err: err}
	}
	return served, err
}

// recordFacts reads an agent's issue from the daemon's record for the change note: the status
// Dispatch showed, the phase, its pull request as the daemon's GitHub reads last observed it, and
// the repository it works; and, for the gh tool, the commit each role's phases row records it
// completed its round against, which a review the agent submits names as the head it read
// (ghtool.Agent.Head). The issue, its pull request and its phases rows are read in one
// transaction. The repository is the record's, else the one kept for the issue at its launch,
// else the one Dispatch resolves the issue to (questionRepository), and its read prepares and
// launches nothing: a question starts no repository cache and adds nothing to what intake hears.
type recordFacts struct{ w *workflowRuntime }

func (r recordFacts) Facts(ctx context.Context, key string) (answerloop.IssueFacts, error) {
	var issue *record.Issue
	var pr *record.PullRequest
	var phases []record.PhaseRow
	if err := pgx.BeginFunc(ctx, r.w.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = r.w.records.Issue(ctx, tx, key); err != nil {
			return err
		}
		if pr, err = r.w.records.PullRequest(ctx, tx, key); err != nil {
			return err
		}
		phases, err = r.w.records.Phases(ctx, tx, key)
		return err
	}); err != nil {
		return answerloop.IssueFacts{}, err
	}
	repo, err := r.w.questionRepository(ctx, key, issue)
	if err != nil {
		return answerloop.IssueFacts{}, err
	}
	facts := answerloop.IssueFacts{Recorded: issue != nil, Repository: repo}
	if issue != nil {
		facts.DispatchStatus, facts.Phase = issue.DispatchStatus, string(issue.Phase)
	}
	if pr != nil {
		facts.PullRequest = &answerloop.PullRequestFacts{Number: pr.Number, State: string(pr.State), Head: pr.HeadSHA}
	}
	for _, row := range phases {
		if row.HandoffCommit == "" {
			continue
		}
		if facts.HandoffCommits == nil {
			facts.HandoffCommits = map[claim.Role]string{}
		}
		facts.HandoffCommits[row.Role] = row.HandoffCommit
	}
	return facts, nil
}

// ghTools opens the gh tool of one answer (ghtool.Open) under the daemon's host, whose shared
// public leak check is its runtime's (answersTools).
type ghTools struct {
	host   ghtool.Host
	bounds config.AnswerBounds
}

func (g ghTools) Open(ctx context.Context, agent ghtool.Agent, question string) (answerloop.Tool, error) {
	a, err := ghtool.Open(ctx, g.host, agent, question)
	if err != nil {
		return nil, err
	}
	return ghTool{answer: a, bounds: g.bounds}, nil
}

// ghTool is one answer's gh tool: its calls (ghtool.Run) and its restart lookup (ghtool.Lookup).
type ghTool struct {
	answer *ghtool.Answer
	bounds config.AnswerBounds
}

func (t ghTool) Run(ctx context.Context, seq int, args string) ghtool.Result {
	return ghtool.Run(ctx, ghtool.Call{Answer: t.answer, Seq: seq, Args: args, Bounds: t.bounds})
}

func (t ghTool) Lookup(ctx context.Context, seq int) (ghtool.Found, error) {
	return ghtool.Lookup(ctx, t.answer, seq, t.bounds)
}

func (t ghTool) Close() error { return t.answer.Close() }

// controllerPassOn passes a question to the controller: in one transaction it marks the question
// passed by its id (processed_events, store.QuestionPassedSource) and queues its agent-question
// notice, naming every gh write the question's recorded calls ran (ghtool.Writes), a controller
// notice row the outbox publishes to the controller topic as it does every controller notice, so
// the notice survives a restart, and a restart that finds the question not yet marked passed on
// and calls this again queues nothing more. records wakes the outbox once the transaction commits
// (projectRecords). `legion state` lists the question from the mark (questionsForController).
type controllerPassOn struct {
	pool    *store.Pool
	records record.Store
	now     func() time.Time
	log     *slog.Logger
}

func (p controllerPassOn) PassOn(ctx context.Context, q store.AnswerQuestion, reason answerloop.Reason, origin answerloop.Origin) error {
	queued := false
	// The pass is its own transaction, so it owns the commit hook the row's write registers.
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, p.pool, func(tx pgx.Tx) error {
		fresh, err := p.records.MarkProcessed(scoped, tx, store.QuestionPassedSource, q.ID)
		if err != nil || !fresh {
			return err
		}
		calls, err := store.AnswerToolCallArgsTx(scoped, tx, []string{q.ID})
		if err != nil {
			return err
		}
		question := &record.AgentQuestion{
			ID: q.ID, Issue: q.Issue, Session: q.Session, Asker: origin.Asker, Ref: origin.Ref,
			Text: record.AgentQuestionText(origin.Text, origin.Ref), Writes: ghtool.Writes(calls[q.ID]),
		}
		row, err := record.NewOutboxRow(q.Issue, record.ControllerNotice{
			Kind: record.AgentQuestionNotice, Role: q.Role, Reason: string(reason), Question: question,
		}, p.now())
		if err != nil {
			return fmt.Errorf("the agent-question notice of %s: %w", q.ID, err)
		}
		queued = true
		return p.records.Enqueue(scoped, tx, row)
	}); err != nil {
		return fmt.Errorf("pass the question %s to the controller: %w", q.ID, err)
	}
	committed()
	if queued {
		p.log.Info("answers: question passed to the controller", "question", q.ID, "issue", q.Issue, "role", q.Role, "session", q.Session,
			"reason", reason, "asker", origin.Asker, "ref", origin.Ref)
	}
	return nil
}

// questionsForController is the project's questions passed to the controller in the
// api.QuestionsForControllerWindow before now, newest first, at most api.QuestionsForControllerLimit,
// read through tx, each with who asked and where as its recorded hand-off says, and every gh write
// its recorded calls ran (ghtool.Writes). It reads what the state lists of a question and no more
// (store.PassedQuestionsTx, store.AnswerToolCallArgsTx): never a reply, and never a call's result.
// A hand-off that no longer reads names no asker and no ref; the question is listed all the same.
func questionsForController(ctx context.Context, tx pgx.Tx, st *store.Store, project string, now time.Time) ([]api.QuestionForController, error) {
	passed, err := st.PassedQuestionsTx(ctx, tx, project, now.Add(-api.QuestionsForControllerWindow), api.QuestionsForControllerLimit)
	if err != nil || len(passed) == 0 {
		return nil, err
	}
	ids := make([]string, len(passed))
	for i, p := range passed {
		ids[i] = p.ID
	}
	calls, err := store.AnswerToolCallArgsTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	questions := make([]api.QuestionForController, len(passed))
	for i, p := range passed {
		questions[i] = api.QuestionForController{
			ID: p.ID, Issue: p.Issue, Role: p.Role, Session: p.Session, Reason: p.PassReason, PassedAt: p.PassedAt.UTC(),
			Writes: ghtool.Writes(calls[p.ID]),
		}
		if h, err := intake.DecodeHandoff(p.Handoff); err == nil {
			if origin, err := answerloop.ReadOrigin(h); err == nil {
				questions[i].Asker, questions[i].Ref = origin.Asker, origin.Ref
			}
		}
	}
	return questions, nil
}

// decisionPassedSource is the processed_events source the daemon marks a decision passed on under,
// by the decision's id (answerloop.Decision.ID), in the transaction that queues its agent-decision
// notice: the mark is what passes a decision once, across redeliveries and restarts.
const decisionPassedSource = string(record.AgentDecisionNotice)

// routedDecisions passes on a person's decision on an ask a stopped agent opened (plan 2.9): in one
// transaction it marks the decision passed by its id and queues one agent-decision notice. The
// notice is an architect notice row when the architect that owns the agent's issue holds its role
// (decisionRoute), which the outbox routes to it as it routes every notice; otherwise a
// controller notice row, so the controller hears the decision nobody in the tree will. A redelivery,
// and a restart's, finds the mark and queues nothing more. outbox's records wake the outbox once the
// transaction commits (projectRecords).
//
// A decision #2163's relay carries itself is the one this queues nothing for (relayed): that
// relay tells the controller in its own parked-decision notice and keeps the decision for the
// root's next start, and this loop merges second, so it records the decision passed — a
// redelivery and a restart queue nothing later either — and logs the one line that says whose it
// is. What the same message asks the agent is still answered: only the decision is the relay's.
//
// Whether the relay carries it is read of the ask's own issue (Decision.AskIssue), the issue that
// relay reads, and never of the agent's conversation issue: a phase worker opens its findings as
// an ask on the controller's report issue (skill://legion-worker/references/findings.md), an
// issue in no tree the daemon records, and the relay sends nothing for an ask of one. Held back
// on the agent's parked tree alone, a person's answer to such an ask reached nobody.
type routedDecisions struct {
	outbox *outbox
	now    func() time.Time
	log    *slog.Logger
}

func (d routedDecisions) Decide(ctx context.Context, decision answerloop.Decision) error {
	conv := decision.Conversation
	notice := record.Notice{Kind: record.AgentDecisionNotice, Role: conv.Role, AgentDecision: &record.AgentDecision{
		ID: decision.ID, Issue: conv.Issue, Session: conv.Session, Asker: decision.Asker, Ref: decision.Ref,
		Text: record.AgentDecisionText(decision.Text, decision.Ref),
	}}
	// to is whom the decision was queued for, empty when an earlier hand-off passed it on or when
	// #2163's relay owns it (relayed).
	to, relayed := "", false
	// The pass is its own transaction, so it owns the commit hook the row's write registers.
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, d.outbox.pool, func(tx pgx.Tx) error {
		fresh, err := d.outbox.records.MarkProcessed(scoped, tx, decisionPassedSource, decision.ID)
		if err != nil || !fresh {
			return err
		}
		relayed, err = d.relayCarries(scoped, tx, decision)
		if err != nil {
			return err
		}
		if relayed {
			return nil
		}
		architect, err := d.decisionRoute(scoped, tx, conv.Issue)
		if err != nil {
			return err
		}
		var payload record.OutboxPayload = record.ControllerNotice(notice)
		to = "the controller"
		if architect != "" {
			payload, to = notice, string(architect)
		}
		row, err := record.NewOutboxRow(conv.Issue, payload, d.now())
		if err != nil {
			return fmt.Errorf("the agent-decision notice of %s: %w", decision.ID, err)
		}
		return d.outbox.records.Enqueue(scoped, tx, row)
	}); err != nil {
		return fmt.Errorf("pass the decision %s on: %w", decision.ID, err)
	}
	committed()
	switch {
	case relayed:
		d.log.Info("answers: decision left to the parked-decision relay of the tree its ask stands in", "decision", decision.ID,
			"issue", conv.Issue, "ask_issue", decision.AskIssue, "role", conv.Role, "session", conv.Session,
			"asker", decision.Asker, "ref", decision.Ref)
	case to != "":
		d.log.Info("answers: decision passed on", "decision", decision.ID, "to", to, "issue", conv.Issue, "role", conv.Role,
			"session", conv.Session, "asker", decision.Asker, "ref", decision.Ref)
	}
	return nil
}

// relayCarries says whether sjawhar/legion#2163's parked-decision relay carries decision itself,
// judged by that relay's own rules and on the issue the ask stands on, which is the issue it
// reads: the ask's kind, which the relay's two paths gate on (relayOwns), and the tree that issue
// stands in with who decided (workflow.RelaysDecision). Everything else is this loop's to pass on
// — an ask of an issue the daemon does not record, a tree that runs, a lingering root someone
// moved back into the workflow, a decision no person made, and an answer of a kind the relay
// leaves alone — so no decision reaches nobody and none reaches the controller twice.
func (d routedDecisions) relayCarries(ctx context.Context, tx pgx.Tx, decision answerloop.Decision) (bool, error) {
	if !relayOwns(decision) || decision.AskIssue == "" {
		return false, nil
	}
	return workflow.RelaysDecision(ctx, tx, d.outbox.records, decision.AskIssue, decision.Asker, d.outbox.gatedSteps)
}

// relayOwns says whether the parked-decision relay sends a decision of this ask's kind at all:
// every reply on an ask's thread, which the relay sends whatever the ask's kind (workflow's
// dispatchAskReply), and a person's answer only of an ask kind it answers for
// (record.RelaysAskAnswer, workflow's dispatchAskClosed). An answer the relay leaves alone — a
// legacy `action` ask's, or one whose event names no kind — is this loop's to pass on wherever
// its ask stands, so no decision reaches nobody.
func relayOwns(decision answerloop.Decision) bool {
	return !decision.Answered || record.RelaysAskAnswer(decision.AskKind)
}

// decisionRoute is where a decision on the agent's issue key goes once it is this loop's to pass
// on (relayCarries), read through tx as the outbox reads a notice's route (readNoticeRoute,
// noticeArchitect): the architect that owns the issue when it holds its role (holdsItsRole) and
// its tree neither lingers nor has closed. No architect is the controller's: the tree's architect
// is suspended, being stopped, failed, retired or not supervised, its root lingers — parked, or
// on a status someone moved back into the workflow — or the issue is not recorded in a tree the
// daemon routes notices of.
func (d routedDecisions) decisionRoute(ctx context.Context, tx pgx.Tx, key string) (claim.Token, error) {
	issue, route, err := d.outbox.readNoticeRoute(ctx, tx, key, map[string]noticeRoute{})
	switch {
	case errors.Is(err, errNoticeUnroutable):
		return "", nil
	case err != nil:
		return "", err
	case route.root.Lingers():
		return "", nil
	}
	architect, err := noticeArchitect(route.project, route.issues, issue, record.Notice{Kind: record.AgentDecisionNotice}, d.outbox.holdsItsRole)
	if err != nil || !d.outbox.holdsItsRole(architect) {
		return "", nil
	}
	return architect, nil
}
