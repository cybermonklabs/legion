package harness

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// changes.go makes R3's changes 2 to 7 of a case's text, alone and in the combinations of
// combinations.yaml. Each change is a step the text meets, in the order a text meets them: the
// written rules (2) and the writing check alone (7a) when the agent writes, the reviewer (3 or 4)
// before the text posts, a person's Rewrite (5) and Define (6) when the person reads it. 7b is the
// check's findings as the reviewer's inputs, which the local Dispatch's DISPATCH_REVIEWER_RULES
// decides. Every model call of the reviewer, Rewrite and Define goes through the local Dispatch
// (`harness local`) with the candidate named in the Dispatch-Test-Model header, so the score is a
// score of the server's own code; a comment's, a message's and a document's Rewrite, which the
// server has no route for, and a Define of words in a comment or a message, call the same
// component (internal/dispatch/reviewer) from here.

// The changes --change names, besides 1 and the combinations.
const (
	changeRules         = "2"
	changeFindings      = "3"
	changeReviewRewrite = "4"
	changeRewrite       = "5"
	changeDefine        = "6"
	changeCheck         = "7a"
	changeCheckInputs   = "7b"
)

// stepOrder is the order in which a text meets the changes that are steps.
var stepOrder = []string{changeRules, changeCheck, changeFindings, changeReviewRewrite, changeRewrite, changeDefine}

// combinationsFile is combinations.yaml.
//
//go:embed combinations.yaml
var combinationsFile []byte

// Combinations reads combinations.yaml: each combination's changes, by its name.
func Combinations() (map[string][]string, error) {
	var file struct {
		Combinations map[string][]string `yaml:"combinations"`
	}
	if err := yaml.Unmarshal(combinationsFile, &file); err != nil {
		return nil, fmt.Errorf("combinations.yaml: %w", err)
	}
	for name, changes := range file.Combinations {
		if _, err := planOf(name, changes); err != nil {
			return nil, fmt.Errorf("combinations.yaml: %w", err)
		}
	}
	return file.Combinations, nil
}

// changePlan is what one --change makes: its steps in the order a text meets them, and whether
// the reviewer reads the check's findings (7b).
type changePlan struct {
	name   string
	steps  []string
	inputs bool
}

// planOf is the plan of the change name, which holds changes: one change, or a combination.
func planOf(name string, changes []string) (changePlan, error) {
	p := changePlan{name: name}
	for _, c := range changes {
		switch {
		case c == changeCheckInputs:
			p.inputs = true
		case slices.Contains(stepOrder, c):
			if slices.Contains(p.steps, c) {
				return changePlan{}, fmt.Errorf("%s names change %s twice", name, c)
			}
			p.steps = append(p.steps, c)
		default:
			return changePlan{}, fmt.Errorf("%s names %q, which is not one of the changes 2, 3, 4, 5, 6, 7a and 7b", name, c)
		}
	}
	slices.SortFunc(p.steps, func(a, b string) int { return slices.Index(stepOrder, a) - slices.Index(stepOrder, b) })
	switch {
	case len(p.steps) == 0:
		return changePlan{}, fmt.Errorf("%s holds no change that makes a text: 7b is the check's findings as the reviewer's inputs, and needs 3 or 4", name)
	case p.has(changeFindings) && p.has(changeReviewRewrite):
		return changePlan{}, fmt.Errorf("%s holds 3 and 4: a reviewer's findings go to the writer, or its rewrite posts, not both", name)
	case p.inputs && !p.has(changeFindings) && !p.has(changeReviewRewrite):
		return changePlan{}, fmt.Errorf("%s holds 7b with no reviewer: 7b needs 3 or 4", name)
	}
	return p, nil
}

func (p changePlan) has(step string) bool { return slices.Contains(p.steps, step) }

// candidate is whether a step calls the candidate model: the reviewer, Rewrite, Define and the
// detector's reader.
func (p changePlan) candidate() bool {
	return slices.ContainsFunc(p.steps, func(s string) bool { return s != changeRules })
}

// local is whether a step reads the local Dispatch: every step but the check alone.
func (p changePlan) local() bool {
	return slices.ContainsFunc(p.steps, func(s string) bool { return s != changeCheck })
}

// writer is whether a step plays the writer: every step but the reviewer's rewrite.
func (p changePlan) writer() bool {
	return slices.ContainsFunc(p.steps, func(s string) bool { return s != changeReviewRewrite })
}

// asksWriter is whether a step asks the writer a side question: Rewrite (5) and Define (6).
func (p changePlan) asksWriter() bool { return p.has(changeRewrite) || p.has(changeDefine) }

// changes are the changes the plan holds, for the exposure log.
func (p changePlan) changes() []string {
	out := slices.Clone(p.steps)
	if p.inputs {
		out = append(out, changeCheckInputs)
	}
	return out
}

// What posts when the writer does not act on a held text (DISPATCH_REVIEWER_UNEDITED): change 3's
// choice for a case with no session (plan Decision 18).
const (
	uneditedRewrite   = "rewrite"
	uneditedAsWritten = "as_written"
)

// writerWindow is the time the findings' frame tells the writer it has (the default of
// DISPATCH_REVIEWER_WRITER_WINDOW); the played writer answers at once.
const writerWindow = 5 * time.Minute

// waitLimit bounds the harness's wait for a Rewrite or a side question's answer: past it the case
// ends in an error, which the next run scores again.
const waitLimit = 10 * time.Minute

// rewriteRead is what the local Dispatch's database holds of an ask's Rewrite that no route
// answers: its run's model calls, and whether the rewrite read the asking agent's answer.
type rewriteRead struct {
	Calls       []serverCall
	AgentAnswer bool
}

// changeRun makes one --change of each case's text.
type changeRun struct {
	plan      changePlan
	ds        *Dataset
	cfg       Config
	candidate string
	// candModel is the candidate as the component and the detector's reader call it.
	candModel agentloop.Model
	runner    *llm.Runner
	writer    *Writer
	local     *localClient
	loaded    map[string]Loaded
	envoy     *fakeEnvoy
	// rewriteRead reads an ask's Rewrite in the local Dispatch's database.
	rewriteRead func(ctx context.Context, askID string) (rewriteRead, error)
	// rules are the reviewer's rule sets the local Dispatch runs (DISPATCH_REVIEWER_RULES), which
	// the component's calls from here read too.
	rules    []reviewer.RuleSet
	rounds   int
	unedited string
	sessions map[string]claude.SessionText
	// writerOrder is the order of the writer's user message (score --writer-order).
	writerOrder string
	// answers are the writer's answers that the lanes of a lockstep run share, nil outside one.
	answers *sharedAnswers
	// tries and backoff bound the retries of a call that met a provider's failure; poll is how
	// often a wait looks again.
	tries   int
	backoff time.Duration
	poll    time.Duration
}

func (r *changeRun) Name() string { return r.plan.name }

// TraceStep is one thing a change did to a case's text, as cases.jsonl keeps it: the change, what
// came of it, and its detail. A review's step holds what the draft route answered: its findings,
// each finding it dropped because its quote is not in the text (its rule set and its quote as the
// model wrote it), and a rewrite that was not applied with the reason. Change 7a's step where the
// writer writes again holds the writing check's findings.
type TraceStep struct {
	Change   string                    `json:"change"`
	Outcome  string                    `json:"outcome"`
	Detail   string                    `json:"detail,omitempty"`
	Findings []TraceFinding            `json:"findings,omitempty"`
	Dropped  []reviewer.DroppedFinding `json:"dropped,omitempty"`
	// Rewrite is the review's rewrite where it was not applied, as the harness holds a text
	// (rewritten), and NotApplied the reason the draft route gave.
	Rewrite    string `json:"rewrite,omitempty"`
	NotApplied string `json:"not_applied,omitempty"`
}

// TraceFinding is one finding a step read. A review's is the reviewer's finding as the draft
// route answers it (reviewer.Finding): its rule set, its quote, why the quote breaks the rule,
// and the fix. The writing check's (7a) has the rule set the reviewer names the check's part by
// (structure or detector), the check's rule, the sentence as its quote, the check's fix, and no
// why.
type TraceFinding struct {
	RuleSet string `json:"rule_set"`
	Rule    string `json:"rule,omitempty"`
	Quote   string `json:"quote"`
	Why     string `json:"why,omitempty"`
	Fix     string `json:"fix"`
}

// reviewFindings are a review's findings as the trace keeps them.
func reviewFindings(findings []reviewer.Finding) []TraceFinding {
	out := make([]TraceFinding, len(findings))
	for i, f := range findings {
		out[i] = TraceFinding{RuleSet: string(f.RuleSet), Quote: f.Quote, Why: f.Why, Fix: f.Fix}
	}
	return out
}

// checkFindings are the writing check's findings as the trace keeps them.
func checkFindings(findings []writingcheck.Finding) []TraceFinding {
	out := make([]TraceFinding, len(findings))
	for i, f := range findings {
		set := reviewer.RuleStructure
		if f.Rule == writingcheck.DetectorRule {
			set = reviewer.RuleDetector
		}
		out[i] = TraceFinding{RuleSet: string(set), Rule: f.Rule, Quote: f.Text, Fix: f.Rewrite}
	}
	return out
}

// textRun is one case's text as the steps make it.
type textRun struct {
	c       *Case
	kind    reviewer.Kind
	session claude.SessionText
	loaded  Loaded
	// writer is the session the local Dispatch knows the writer as (harness load).
	writer string
	record string
	text   string
	out    Applied
}

func (t *textRun) trace(change, outcome, detail string) {
	t.out.Trace = append(t.out.Trace, TraceStep{Change: change, Outcome: outcome, Detail: detail})
}

func (t *textRun) add(calls ...Call) {
	for _, c := range calls {
		if c.Model != "" {
			t.out.Calls = append(t.out.Calls, c)
		}
	}
}

// turn is a writer's turn about the text as it stands, with message.
func (t *textRun) turn(message string) writerTurn {
	return writerTurn{kind: string(t.kind), text: t.text, record: t.record, session: t.session, message: message}
}

// kinds are the reviewer's kinds of the dataset's text kinds.
var kinds = map[string]reviewer.Kind{
	"ask": reviewer.KindAsk, "comment": reviewer.KindComment, "message": reviewer.KindMessage,
	"document": reviewer.KindDocument, "issue": reviewer.KindIssue,
}

// Apply makes the change of the case's text: each step in turn, each from the text the one before
// made. The Applied it answers on an error holds the calls made so far.
func (r *changeRun) Apply(ctx context.Context, c *Case) (Applied, error) {
	kind, ok := kinds[c.Kind]
	if !ok {
		return Applied{}, fmt.Errorf("a text of kind %q is none the reviewer reads", c.Kind)
	}
	t := &textRun{c: c, kind: kind, session: r.sessions[c.ID], text: c.Agent.Text, record: r.ds.RecordPart(c),
		writer: cmp.Or(c.WriterSession, localWriter), out: Applied{AsksWriter: r.plan.asksWriter()}}
	if r.plan.local() {
		loaded, ok := r.loaded[c.ID]
		if !ok {
			return Applied{}, errors.New("the local Dispatch holds no record of the case: run `harness load` on it")
		}
		t.loaded = loaded
	}
	for _, step := range r.plan.steps {
		var err error
		switch step {
		case changeRules:
			err = r.rulesStep(ctx, t)
		case changeCheck:
			err = r.checkStep(ctx, t)
		case changeFindings:
			err = r.findingsStep(ctx, t)
		case changeReviewRewrite:
			err = r.reviewRewriteStep(ctx, t)
		case changeRewrite:
			err = r.rewriteStep(ctx, t)
		case changeDefine:
			err = r.defineStep(ctx, t)
		}
		if err != nil && len(r.plan.steps) > 1 {
			return t.out, fmt.Errorf("step %s: %w", step, err)
		}
		if err != nil {
			return t.out, err
		}
	}
	t.out.Text, t.out.Same = t.text, t.text == c.Agent.Text
	return t.out, nil
}

// localWriter is the session harness load writes a case's texts as where the case names none.
const localWriter = "harness-writer"

// --- Change 2: the written rules before the write.

// rulesTools are the dispatch commands the written rules refuse a first write of
// (writing-rules.ts, WRITING_RULES_COMMANDS), by the kind of text each writes.
var rulesTools = map[reviewer.Kind]string{
	reviewer.KindAsk: "dispatch_ask", reviewer.KindIssue: "dispatch_issue",
	reviewer.KindComment: "dispatch_comment", reviewer.KindMessage: "dispatch_message",
}

// rulesStep is change 2: the dispatch command refuses the write with the rules for its reader,
// which the local Dispatch answers for the write (POST /api/v1/writing-rules), and the writer
// writes its text again. A document is no write the command refuses.
func (r *changeRun) rulesStep(ctx context.Context, t *textRun) error {
	tool, ok := rulesTools[t.kind]
	if !ok {
		t.trace(changeRules, "no_refusal", "the dispatch command refuses no write of a "+string(t.kind))
		return nil
	}
	route, body := t.write()
	body["actor"] = map[string]string{"kind": "session", "id": t.writer}
	var rules struct {
		Reader string `json:"reader"`
		Text   string `json:"text"`
	}
	if err := r.local.call(ctx, actor{session: t.writer}, http.MethodPost, "/api/v1/writing-rules", map[string]any{"route": route, "body": body}, nil, &rules); err != nil {
		return fmt.Errorf("the rules for the write: %w", err)
	}
	text, call, err := r.writer.Rewrite(ctx, "writer:rules", t.turn(writingRulesRefusal(tool, rules.Text)))
	t.add(call)
	t.out.WaitSeconds += call.Seconds
	if err != nil {
		return err
	}
	t.trace(changeRules, "written_again", "the rules for "+cmp.Or(rules.Reader, "no reader"))
	t.text = text
	return nil
}

// write is the route and the body of the case's write as its text stands, in the local Dispatch.
func (t *textRun) write() (string, map[string]any) {
	issue, reply := t.loaded.Issue, t.loaded.ReplyTo
	switch {
	case t.kind == reviewer.KindAsk && issue != "":
		question, options := splitAsk(t.text)
		return "POST /api/v1/issues/" + issue + "/asks", map[string]any{"question": question, "options": options}
	case t.kind == reviewer.KindComment && issue != "":
		body := map[string]any{"body": t.text}
		if reply != "" {
			body["reply_to"] = reply
		}
		return "POST /api/v1/issues/" + issue + "/comments", body
	case reply != "" && issue == "":
		return "POST /api/v1/messages/" + reply + "/reply", map[string]any{"body": t.text, "attempt": 1}
	case issue != "":
		return "POST /api/v1/issues/" + issue + "/messages", map[string]any{"body": t.text}
	}
	return "POST /api/v1/agents/" + url.PathEscape(t.writer) + "/messages", map[string]any{"body": t.text}
}

// --- Changes 3 and 4: the reviewer, through the local Dispatch's draft route.

// serverCall is one model call a route of the local Dispatch answers (api reviewerCall): its
// attempts absent where it made one try.
type serverCall struct {
	Purpose          string `json:"purpose"`
	Deployment       string `json:"deployment"`
	Attempts         int    `json:"attempts,omitempty"`
	Served           string `json:"served"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	Fault            string `json:"fault,omitempty"`
}

// wording is a text's wording as a route answers it (model.TextWording).
type wording struct {
	Title    string            `json:"title,omitempty"`
	Body     string            `json:"body"`
	Options  []model.AskOption `json:"options,omitempty"`
	Multiple bool              `json:"multiple,omitempty"`
}

// draftAnswer is what POST /api/v1/reviewer-drafts answers.
type draftAnswer struct {
	Reader     string                    `json:"reader"`
	Findings   []reviewer.Finding        `json:"findings"`
	Dropped    []reviewer.DroppedFinding `json:"dropped,omitempty"`
	Rewrite    *wording                  `json:"rewrite"`
	NotApplied string                    `json:"not_applied,omitempty"`
	Calls      []serverCall              `json:"calls"`
}

// priced is the server's calls as the outputs record them, each priced at its model's price over
// all its tries; the last one is given the request's time, which no route answers call by call.
func (r *changeRun) priced(calls []serverCall, seconds float64) []Call {
	out := make([]Call, len(calls))
	for i, c := range calls {
		p := r.cfg.Prices.Prices[c.Deployment]
		out[i] = Call{
			Model: c.Deployment, Served: c.Served, Input: c.InputTokens, Output: c.OutputTokens,
			CacheWrite: c.CacheWriteTokens, CacheRead: c.CacheReadTokens, Attempts: cmp.Or(c.Attempts, 1),
			CostUSD: (float64(c.InputTokens)*p.Input + float64(c.OutputTokens)*p.Output +
				float64(c.CacheWriteTokens)*p.CacheWrite + float64(c.CacheReadTokens)*p.CacheRead) / 1e6,
		}
	}
	if len(out) > 0 {
		out[len(out)-1].Seconds = seconds
	}
	return out
}

// testModel is the header that names the candidate to the local Dispatch.
func (r *changeRun) testModel() http.Header {
	return http.Header{"Dispatch-Test-Model": {r.candidate}}
}

// retryable is whether a route's refusal of a model call is a provider's failure a retry can
// clear: the call failed or ended on a provider error, and not on the candidate's own fault (its
// time bound, a refusal, a reply cut short or out of its schema), which production meets too.
func retryable(err error) bool {
	var refused *localStatusError
	if !errors.As(err, &refused) || refused.status != http.StatusBadGateway {
		return false
	}
	return strings.Contains(refused.body, "provider_error") ||
		(strings.Contains(refused.body, "call failed") && !strings.Contains(refused.body, "deadline"))
}

// draft is the reviewer's review of the text as it stands, the candidate's, through the draft
// route. It traces a review step of change, with detail; the step holds what the route answered:
// the findings, the findings it dropped, and a rewrite that was not applied with the reason.
// reviewed is false where the model gave no usable answer after the retries, which posts the text
// as written in production (R5).
func (r *changeRun) draft(ctx context.Context, t *textRun, change, detail string) (answer draftAnswer, reviewed bool, err error) {
	t.trace(change, "review", detail)
	step := len(t.out.Trace) - 1
	body := map[string]any{"kind": string(t.kind), "text": t.text, "issue": t.loaded.Issue, "reader": t.loaded.Reader}
	if t.kind == reviewer.KindAsk {
		question, options := splitAsk(t.text)
		body["text"], body["options"] = question, options
	}
	for attempt := 0; ; attempt++ {
		started := time.Now()
		err = r.local.call(ctx, actor{person: t.loaded.Reader}, http.MethodPost, "/api/v1/reviewer-drafts", body, r.testModel(), &answer)
		seconds := time.Since(started).Seconds()
		t.out.WaitSeconds += seconds
		if err == nil {
			t.add(r.priced(answer.Calls, seconds)...)
			review := &t.out.Trace[step]
			review.Findings, review.Dropped = reviewFindings(answer.Findings), answer.Dropped
			if answer.Rewrite != nil && answer.NotApplied != "" {
				review.Rewrite, review.NotApplied = rewritten(*answer.Rewrite, t.kind), answer.NotApplied
			}
			return answer, true, nil
		}
		if !retryable(err) || attempt+1 >= r.tries {
			break
		}
		if !sleepFor(ctx, r.backoff<<attempt) {
			return draftAnswer{}, false, ctx.Err()
		}
	}
	var refused *localStatusError
	if errors.As(err, &refused) && refused.status == http.StatusBadGateway {
		t.trace(change, "review_failed", refused.body)
		return draftAnswer{}, false, nil
	}
	return draftAnswer{}, false, err
}

func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// rewritten is a route's wording as the harness holds a text: an ask's question and options, one
// line each, as the dataset writes them; any other kind's body.
func rewritten(w wording, kind reviewer.Kind) string {
	if kind == reviewer.KindAsk {
		return joinAsk(w.Body, w.Options)
	}
	return w.Body
}

// findingsStep is change 3: the reviewer's findings go to the writer, which edits its text, or
// posts it as written with a reason; an edit is reviewed again, up to rounds reviews, after which
// the last edit posts. With no session the writer is not running, and the setting's choice posts
// (plan Decision 18): the reviewer's rewrite under DISPATCH_REVIEWER_UNEDITED=rewrite, where it
// holds one that is applied, else the text as written.
func (r *changeRun) findingsStep(ctx context.Context, t *textRun) error {
	for round := 1; ; round++ {
		answer, reviewed, err := r.draft(ctx, t, changeFindings, fmt.Sprintf("round %d", round))
		switch {
		case err != nil:
			return err
		case !reviewed:
			return nil
		case len(answer.Findings) == 0:
			t.trace(changeFindings, "passed", "")
			return nil
		case t.session.Empty():
			if r.unedited == uneditedRewrite && answer.Rewrite != nil && answer.NotApplied == "" {
				t.text = rewritten(*answer.Rewrite, t.kind)
				t.trace(changeFindings, "no_writer_rewrite_posted", fmt.Sprintf("%d findings", len(answer.Findings)))
			} else {
				t.trace(changeFindings, "no_writer_posted_as_written", cmp.Or(answer.NotApplied, r.unedited))
			}
			return nil
		}
		frame := reviewer.FindingsFrame{
			ID: t.c.Agent.Key, Kind: t.kind, Findings: answer.Findings, Held: true, TimeLeft: writerWindow,
			PostsRewrite: r.unedited == uneditedRewrite,
		}
		if answer.Rewrite != nil {
			text := reviewer.Text{Kind: t.kind, Title: answer.Rewrite.Title, Body: answer.Rewrite.Body, Options: answer.Rewrite.Options, Multiple: answer.Rewrite.Multiple}
			frame.Rewrite = &text
		}
		if answer.NotApplied != "" {
			frame.NotApplied = &reviewer.NotApplied{Reason: answer.NotApplied}
		}
		body, err := frame.Body()
		if err != nil {
			return err
		}
		act, call, err := r.writer.ActOnFindings(ctx, t.turn(body))
		t.add(call)
		t.out.WaitSeconds += call.Seconds
		if err != nil {
			return err
		}
		if act.Action == actionPost {
			t.trace(changeFindings, "posted_as_written", act.Reason)
			return nil
		}
		t.text = act.Text
		t.trace(changeFindings, "edited", fmt.Sprintf("round %d", round))
		if round >= r.rounds {
			t.trace(changeFindings, "bound", fmt.Sprintf("the edit posts after %d reviews", round))
			return nil
		}
	}
}

// reviewRewriteStep is change 4: the reviewer's rewrite from Dispatch's record alone posts, where
// the review found something and the rewrite keeps what a rewrite keeps.
func (r *changeRun) reviewRewriteStep(ctx context.Context, t *textRun) error {
	answer, reviewed, err := r.draft(ctx, t, changeReviewRewrite, "")
	switch {
	case err != nil:
		return err
	case !reviewed:
	case len(answer.Findings) == 0:
		t.trace(changeReviewRewrite, "passed", "")
	case answer.Rewrite == nil || answer.NotApplied != "":
		t.trace(changeReviewRewrite, "posted_as_written", cmp.Or(answer.NotApplied, "the reviewer wrote no rewrite"))
	default:
		t.text = rewritten(*answer.Rewrite, t.kind)
		t.trace(changeReviewRewrite, "rewrite_posted", fmt.Sprintf("%d findings", len(answer.Findings)))
	}
	return nil
}

// --- Changes 5 and 6: a person's Rewrite and Define.

// openAsk opens the text as an ask of the writer's on the case's issue in the local Dispatch, and
// answers its id; "" with the refusal's text where the local Dispatch refuses it, so the step
// reads the component instead.
func (r *changeRun) openAsk(ctx context.Context, t *textRun) (string, string, error) {
	question, options := splitAsk(t.text)
	var made struct {
		ID string `json:"id"`
	}
	err := r.local.send(ctx, actor{session: t.writer}, http.MethodPost, "/api/v1/issues/"+url.PathEscape(t.loaded.Issue)+"/asks",
		map[string]any{"question": question, "options": options}, &made)
	var refused *localStatusError
	if errors.As(err, &refused) && refused.status < http.StatusInternalServerError {
		return "", refused.body, nil
	}
	return made.ID, "", err
}

// sideAnswer is the writer's answer to one side question the local Dispatch sent through the
// fake Envoy.
type sideAnswer struct {
	answer string
	call   Call
	err    error
}

// sharedAnswers are the writer's answers to side questions that the lanes of a lockstep run
// (score --models) share. The side question of changes 5 and 6 does not depend on the candidate:
// the local Dispatch writes it from the ask before the candidate is called
// (reviewer.RewriteQuestion in api/ask_rewrite.go, reviewer.DefineQuestion in
// api/definitions.go), and the component's path writes it from the case the same way. So the
// first lane of a case that asks a question makes the answer, and each other lane takes it and
// calls no model. The lanes of a case run one after another (scorer.scoreCase), so no two of them
// ask one question at once.
type sharedAnswers struct {
	mu sync.Mutex
	by map[string]sharedAnswer
}

// sharedAnswer is one answer of sharedAnswers: its text, the writer's call that made it, and the
// candidate of the lane that made it.
type sharedAnswer struct {
	text  string
	call  Call
	model string
}

// writerAnswer is the writer's answer to the side question about the text as it stands. In a
// lockstep run the answer is made once for the case, the text and the question, and shared
// (sharedAnswers). A lane that takes another lane's answer records the call that made it, with
// SharedFrom naming that lane's model, so the change's cost per text and its wait hold the call
// as a run of one model holds it. The local Dispatch's question names the ask by its address, and
// each lane opens an ask of its own, so the ask's id is the one part of the question that differs
// from lane to lane: askID, which the key leaves out ("" where the question names no ask).
func (r *changeRun) writerAnswer(ctx context.Context, t *textRun, question, askID string) (string, Call, error) {
	if r.answers == nil {
		return r.writer.Answer(ctx, t.turn(question))
	}
	asked := question
	if askID != "" {
		asked = strings.ReplaceAll(question, askID, "")
	}
	key := sha256Hex([]byte(t.c.ID + "\x00" + t.text + "\x00" + asked))
	r.answers.mu.Lock()
	made, ok := r.answers.by[key]
	r.answers.mu.Unlock()
	if ok {
		call := made.call
		call.SharedFrom = made.model
		return made.text, call, nil
	}
	text, call, err := r.writer.Answer(ctx, t.turn(question))
	if err == nil {
		r.answers.mu.Lock()
		r.answers.by[key] = sharedAnswer{text: text, call: call, model: r.candidate}
		r.answers.mu.Unlock()
	}
	return text, call, err
}

// answerSideQuestion waits for the side question match accepts, which the local Dispatch sends the
// writer through the fake Envoy about the ask askID, has the writer answer it, and replies as the
// writer session.
func (r *changeRun) answerSideQuestion(ctx context.Context, t *textRun, askID string, match func(fakeSend) bool) sideAnswer {
	send, err := r.envoy.await(ctx, r.poll, func(s fakeSend) bool { return s.Target == t.writer && match(s) })
	if err != nil {
		if ctx.Err() != nil {
			return sideAnswer{}
		}
		return sideAnswer{err: err}
	}
	answer, call, err := r.writerAnswer(ctx, t, send.Body, askID)
	if err != nil {
		return sideAnswer{call: call, err: err}
	}
	err = r.local.send(ctx, actor{session: t.writer}, http.MethodPost, "/api/v1/messages/"+url.PathEscape(send.ID)+"/reply",
		map[string]any{"body": answer, "attempt": send.Attempt}, nil)
	if ctx.Err() != nil {
		err = nil
	}
	return sideAnswer{answer: answer, call: call, err: err}
}

// rewriteStep is change 5. An ask on an issue is rewritten by the local Dispatch's Rewrite
// (POST /api/v1/asks/{id}/rewrites), which sends the writer one side question when the writer is
// running (the case has a session) and reads its answer; any other text by the component's
// Rewrite, after the same side question, which the writer answers when it is running.
func (r *changeRun) rewriteStep(ctx context.Context, t *textRun) error {
	if t.kind == reviewer.KindAsk && t.loaded.Issue != "" {
		id, refused, err := r.openAsk(ctx, t)
		if err != nil {
			return err
		}
		if id != "" {
			return r.rewriteAsk(ctx, t, id)
		}
		t.trace(changeRewrite, "ask_refused", refused)
	}
	var answer reviewer.Answer
	if !t.session.Empty() {
		question, err := reviewer.RewriteQuestion(t.c.Agent.Key, reviewer.Text{Kind: t.kind, Body: t.text})
		if err != nil {
			return err
		}
		text, call, err := r.writerAnswer(ctx, t, question, "")
		t.add(call)
		t.out.WaitSeconds += call.Seconds
		if err != nil {
			return err
		}
		answer = reviewer.Answer{Question: question, Text: text}
	}
	record, err := r.recordOf(ctx, t)
	if err != nil {
		return err
	}
	var done reviewer.Rewritten
	err = r.component(ctx, t, func(m reviewer.Model) ([]reviewer.Call, error) {
		var err error
		done, err = reviewer.Rewrite(ctx, m, r.rules, record, answer)
		return done.Calls, err
	})
	var failed *reviewer.ModelError
	switch {
	case errors.As(err, &failed):
		t.trace(changeRewrite, "rewrite_failed", failed.Error())
	case err != nil:
		return err
	case done.NotApplied != nil:
		t.trace(changeRewrite, "not_applied", done.NotApplied.Reason)
	default:
		t.text = rewritten(wording{Body: done.Text.Body, Options: done.Text.Options}, t.kind)
		t.out.WriterAnswer = answer.Text
		t.trace(changeRewrite, "rewritten", "the component's rewrite")
	}
	return nil
}

// askRead is the part of GET /api/v1/asks/{id} a Rewrite reads.
type askRead struct {
	Ask struct {
		Question string            `json:"question"`
		Options  []model.AskOption `json:"options"`
		Rewrite  *struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		} `json:"rewrite"`
	} `json:"ask"`
}

// rewriteAsk is a person's Rewrite of the ask id in the local Dispatch, the reader pressing it: the
// wait runs from the press to the rewrite's end. Where the writer's answer came from another lane
// of a lockstep run, at once, the wait also holds the time of the call that made it, which a
// person waits for as in that lane.
func (r *changeRun) rewriteAsk(ctx context.Context, t *textRun, id string) error {
	sideCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var side sideAnswer
	if !t.session.Empty() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			side = r.answerSideQuestion(sideCtx, t, id, func(s fakeSend) bool { return strings.Contains(s.Body, id) })
		}()
	}
	started := time.Now()
	err := r.local.call(ctx, actor{person: t.loaded.Reader}, http.MethodPost, "/api/v1/asks/"+id+"/rewrites", nil, r.testModel(), nil)
	var read askRead
	for err == nil {
		if err = r.local.call(ctx, actor{person: t.loaded.Reader}, http.MethodGet, "/api/v1/asks/"+id, nil, nil, &read); err != nil {
			break
		}
		if read.Ask.Rewrite != nil && read.Ask.Rewrite.State != "rewriting" {
			break
		}
		if time.Since(started) > waitLimit {
			err = fmt.Errorf("the rewrite of ask %s did not end in %s", id, waitLimit)
			break
		}
		if !sleepFor(ctx, r.poll) {
			err = ctx.Err()
		}
	}
	seconds := time.Since(started).Seconds()
	stop()
	wg.Wait()
	if side.call.SharedFrom != "" {
		seconds += side.call.Seconds
	}
	t.add(side.call)
	if err != nil {
		return err
	}
	if side.err != nil {
		return fmt.Errorf("the writer's answer to the side question: %w", side.err)
	}
	run, err := r.rewriteRead(ctx, id)
	if err != nil {
		return err
	}
	t.add(r.priced(run.Calls, seconds-side.call.Seconds)...)
	t.out.WaitSeconds += seconds
	if read.Ask.Rewrite.State != "rewritten" {
		t.trace(changeRewrite, "rewrite_failed", read.Ask.Rewrite.Reason)
		return nil
	}
	t.text = joinAsk(read.Ask.Question, read.Ask.Options)
	if run.AgentAnswer {
		t.out.WriterAnswer = side.answer
	}
	t.trace(changeRewrite, "rewritten", fmt.Sprintf("the server's Rewrite; the agent's answer read: %t", run.AgentAnswer))
	return nil
}

// definitionRead is the part of a definition (POST and GET /api/v1/definitions) Define reads.
type definitionRead struct {
	ID          string                `json:"id"`
	Definitions []reviewer.Definition `json:"definitions"`
	Failure     string                `json:"failure"`
	Agent       struct {
		State  string `json:"state"`
		Answer string `json:"answer"`
	} `json:"agent"`
	Calls []serverCall `json:"calls"`
}

// wordsOf are the words a question case's person asked about: those he selected, else those the
// selection prompt found in his question.
func wordsOf(c *Case) []string {
	if q := c.OwnerReply; q != nil {
		if len(q.Selected) > 0 {
			return q.Selected
		}
		if q.Verdict != nil {
			return q.Verdict.Words
		}
	}
	return nil
}

// definitionText is the definition a person reads: what the record says each item of the words
// means, or why it says nothing, and the asking agent's answer beside it, as Define shows them.
func definitionText(defs []reviewer.Definition, failure, answer string) string {
	var b strings.Builder
	for _, d := range defs {
		fmt.Fprintf(&b, "- %s: %s", d.Words, d.Meaning)
		if len(d.Links) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(d.Links, ", "))
		}
		b.WriteString("\n")
	}
	if failure != "" {
		fmt.Fprintf(&b, "Dispatch gave no definition: %s\n", failure)
	}
	if answer != "" {
		fmt.Fprintf(&b, "\nThe agent's answer:\n\n%s\n", answer)
	}
	return strings.TrimSpace(b.String())
}

// defineStep is change 6: Define of the words the person asked about. Words in an ask on an issue,
// and in a document, go to the local Dispatch's Define (POST /api/v1/definitions), which defines
// them from the record at once and sends the asking writer, when it runs, one side question whose
// answer the person reads beside the definition; words in a comment or a message go to the
// component's Define, with the same side question.
func (r *changeRun) defineStep(ctx context.Context, t *textRun) error {
	words := wordsOf(t.c)
	if len(words) == 0 {
		t.trace(changeDefine, "no_words", "the person's question names no words")
		return nil
	}
	body := map[string]any{"words": words}
	askID := ""
	switch doc := t.localDocument(); {
	case t.kind == reviewer.KindAsk && t.loaded.Issue != "":
		id, refused, err := r.openAsk(ctx, t)
		if err != nil {
			return err
		}
		if id == "" {
			t.trace(changeDefine, "ask_refused", refused)
			return r.defineComponent(ctx, t, words)
		}
		body["ask_id"], askID = id, id
	case t.kind == reviewer.KindDocument && doc != "":
		body["document"] = doc
	default:
		return r.defineComponent(ctx, t, words)
	}
	started := time.Now()
	var read definitionRead
	if err := r.local.call(ctx, actor{person: t.loaded.Reader}, http.MethodPost, "/api/v1/definitions", body, r.testModel(), &read); err != nil {
		return err
	}
	t.add(r.priced(read.Calls, time.Since(started).Seconds())...)
	// shared is the time of the writer's call that another lane of a lockstep run made, whose
	// answer this lane took at once: the person waits for it as in that lane.
	shared := 0.0
	if read.Agent.State == "waiting" && !t.session.Empty() {
		side := r.answerSideQuestion(ctx, t, askID, func(s fakeSend) bool { return s.ID == read.ID })
		t.add(side.call)
		if side.call.SharedFrom != "" {
			shared = side.call.Seconds
		}
		if side.err != nil {
			return fmt.Errorf("the writer's answer to the side question: %w", side.err)
		}
		for read.Agent.State == "waiting" {
			if time.Since(started) > waitLimit {
				return fmt.Errorf("definition %s: no answer came in %s", read.ID, waitLimit)
			}
			if !sleepFor(ctx, r.poll) {
				return ctx.Err()
			}
			if err := r.local.call(ctx, actor{person: t.loaded.Reader}, http.MethodGet, "/api/v1/definitions/"+read.ID, nil, nil, &read); err != nil {
				return err
			}
		}
	}
	t.out.WaitSeconds += time.Since(started).Seconds() + shared
	t.out.Definition = definitionText(read.Definitions, read.Failure, read.Agent.Answer)
	t.out.WriterAnswer = read.Agent.Answer
	t.trace(changeDefine, "defined", fmt.Sprintf("the server's Define; the agent's answer: %s", cmp.Or(read.Agent.State, "none")))
	return nil
}

// localDocument is the local id of the document the case's text is a version of, "" for none.
func (t *textRun) localDocument() string {
	own := t.c.textDocument()
	for _, d := range t.loaded.Documents {
		if own != "" && d.ArtifactID == own {
			return d.LocalID
		}
	}
	return ""
}

// defineComponent is the component's Define of words in the text from Dispatch's record, with the
// writer's answer to the side question beside it, as the server's Define shows an ask's.
func (r *changeRun) defineComponent(ctx context.Context, t *textRun, words []string) error {
	answer := ""
	if !t.session.Empty() {
		question, err := reviewer.DefineQuestion(t.c.Agent.Key, t.kind, words)
		if err != nil {
			return err
		}
		text, call, err := r.writerAnswer(ctx, t, question, "")
		t.add(call)
		t.out.WaitSeconds += call.Seconds
		if err != nil {
			return err
		}
		answer = text
	}
	record, err := r.recordOf(ctx, t)
	if err != nil {
		return err
	}
	var done reviewer.Defined
	err = r.component(ctx, t, func(m reviewer.Model) ([]reviewer.Call, error) {
		var err error
		done, err = reviewer.Define(ctx, m, r.rules, record, words, reviewer.Answer{})
		return done.Calls, err
	})
	failure := ""
	var failed *reviewer.ModelError
	switch {
	case errors.As(err, &failed):
		failure = failed.Error()
	case err != nil:
		return err
	}
	t.out.Definition = definitionText(done.Definitions, failure, answer)
	t.out.WriterAnswer = answer
	t.trace(changeDefine, "defined", "the component's Define")
	return nil
}

// component makes one call of the reviewer component with the candidate, in a slot of the run's
// bound and past its pacer, again after a provider's failure; the calls of every attempt are the
// text's.
func (r *changeRun) component(ctx context.Context, t *textRun, call func(reviewer.Model) ([]reviewer.Call, error)) error {
	m := reviewer.Model{Model: r.candModel}
	for attempt := 0; ; attempt++ {
		release, err := r.runner.Hold(ctx)
		if err != nil {
			return err
		}
		started := time.Now()
		calls, err := call(m)
		used := make([]agentloop.Usage, len(calls))
		for i, c := range calls {
			used[i] = c.Usage
		}
		release(used...)
		seconds := time.Since(started).Seconds()
		t.out.WaitSeconds += seconds
		for i, c := range calls {
			out := Call{Model: c.Deployment, Served: c.Served, Input: c.Usage.Input, Output: c.Usage.Output,
				CacheWrite: c.Usage.CacheWrite, CacheRead: c.Usage.CacheRead, CostUSD: c.Usage.CostUSD, Attempts: cmp.Or(c.Attempts, 1)}
			if i == len(calls)-1 {
				out.Seconds = seconds
			}
			t.add(out)
		}
		var failed *reviewer.ModelError
		retry := errors.As(err, &failed) && (failed.Fault == agentloop.FaultProviderError ||
			(failed.Err != nil && !errors.Is(failed.Err, context.DeadlineExceeded)))
		if !retry || attempt+1 >= r.tries {
			return err
		}
		if !sleepFor(ctx, r.backoff<<attempt) {
			return ctx.Err()
		}
	}
}

// readerWordsShown is how many of the reader's words recordOf asks the local Dispatch for: the 200
// most recently asked, as many as Dispatch's own record of a text holds for the reviewer
// (store.ReaderWords) and the most one page of the route answers.
const readerWordsShown = 200

// recordOf is Dispatch's record of the text as the component reads it, filled from the case as the
// dataset stored it (reviewer.Record): the text as it stands, its thread, the issue, its primary
// and decisions documents, the writer's texts, and the words the reader asked about before, which
// the local Dispatch holds (GET /api/v1/readers/{login}/words): the page of as many words as the
// reviewer reads in Dispatch's own record (readerWordsShown).
func (r *changeRun) recordOf(ctx context.Context, t *textRun) (reviewer.Record, error) {
	c := t.c
	text := reviewer.Text{Kind: t.kind, Body: t.text}
	if t.kind == reviewer.KindAsk {
		question, options := splitAsk(t.text)
		text.Body, text.Options = question, options
	}
	record := reviewer.Record{Text: text, Project: c.Record.Project, Reader: t.loaded.Reader}
	if is := c.Record.Issue; is != nil {
		record.IssueKey, record.IssueTitle = is.Key, is.Title
	}
	for _, e := range c.Record.Thread {
		at, _ := time.Parse(time.RFC3339Nano, e.Time)
		entry := reviewer.Entry{Kind: cmp.Or(kinds[e.Kind], reviewer.KindComment), Author: c.author(e), At: at, Text: e.Text}
		record.Thread = append(record.Thread, entry)
		if entry.Author == "the writer agent" {
			record.WriterTexts = append(record.WriterTexts, entry)
		}
	}
	own := c.textDocument()
	for _, d := range c.Record.Documents {
		markdown, ok := r.ds.docs[docKey{d.ArtifactID, d.Version}]
		switch {
		case !ok || d.ArtifactID == own:
		case d.Name == "decisions.md":
			record.Decisions = markdown
		case d.Name == "spec.md" && t.kind != reviewer.KindDocument:
			record.Document = &reviewer.Document{Name: d.Name, Markdown: markdown}
		}
	}
	if record.Reader != "" {
		var words struct {
			Items []struct {
				Word string `json:"word"`
			} `json:"items"`
		}
		path := "/api/v1/readers/" + url.PathEscape(record.Reader) + "/words?limit=" + strconv.Itoa(readerWordsShown)
		if err := r.local.call(ctx, actor{session: t.writer}, http.MethodGet, path, nil, nil, &words); err != nil {
			return reviewer.Record{}, fmt.Errorf("the reader's words: %w", err)
		}
		for _, w := range words.Items {
			record.ReaderWords = append(record.ReaderWords, w.Word)
		}
	}
	return record, nil
}

// --- Change 7a: the writing check alone.

// checkFields is the text's prose as the writing check reads a write's fields.
func checkFields(t *textRun) []writingcheck.Field {
	if t.kind != reviewer.KindAsk {
		return []writingcheck.Field{{Name: "body", Text: t.text}}
	}
	question, options := splitAsk(t.text)
	fields := []writingcheck.Field{{Name: "question", Text: question}}
	for i, o := range options {
		if o.Description != "" {
			fields = append(fields, writingcheck.Field{Name: fmt.Sprintf("options[%d].description", i), Text: o.Description})
		}
	}
	return fields
}

// checkStep is change 7a, the writing check of pull request #3981 alone: its structure rules and
// its detector, whose reader is the candidate, read the text; their findings go to the writer as
// #3981's refusal did, and the writer writes its text again. A text with no finding posts as
// written.
func (r *changeRun) checkStep(ctx context.Context, t *textRun) error {
	var result writingcheck.Result
	for attempt := 0; ; attempt++ {
		var observed reader.Call
		called := false
		detector := reader.New(reader.Config{
			Model:    r.candModel,
			NoEffort: writingcheck.ReaderNoEffort && r.candModel.Endpoint.Kind == agentloop.OpenAIChat,
			Timeout:  reviewer.DefaultTimeout,
			Observe: func(c reader.Call) {
				observed, called = c, true
			},
		})
		release, err := r.runner.Hold(ctx)
		if err != nil {
			return err
		}
		started := time.Now()
		result = writingcheck.New(writingcheck.Config{Rules: writingcheck.EnabledRules, Detector: true, Reader: detector}).Check(ctx, checkFields(t))
		release(observed.Usage)
		seconds := time.Since(started).Seconds()
		t.out.WaitSeconds += seconds
		if called {
			t.add(Call{Model: r.candidate, Served: observed.Model, Input: observed.Usage.Input, Output: observed.Usage.Output,
				CacheWrite: observed.Usage.CacheWrite, CacheRead: observed.Usage.CacheRead, CostUSD: observed.Usage.CostUSD,
				Attempts: 1, Seconds: seconds})
		}
		retry := result.Outcome == reader.OutcomeError || result.Outcome == reader.OutcomeTimeout
		if !retry || attempt+1 >= r.tries {
			break
		}
		if !sleepFor(ctx, r.backoff<<attempt) {
			return ctx.Err()
		}
	}
	if result.Outcome != writingcheck.OutcomeRefused && result.Outcome != writingcheck.OutcomeClean {
		t.trace(changeCheck, "detector_failed", result.Outcome)
	}
	if len(result.Findings) == 0 {
		t.trace(changeCheck, "passed", fmt.Sprintf("%d candidates", result.Candidates))
		return nil
	}
	text, call, err := r.writer.Rewrite(ctx, "writer:check", t.turn(checkRefusal(result.Findings)))
	t.add(call)
	t.out.WaitSeconds += call.Seconds
	if err != nil {
		return err
	}
	t.text = text
	t.out.Trace = append(t.out.Trace, TraceStep{Change: changeCheck, Outcome: "written_again",
		Detail: fmt.Sprintf("%d findings", len(result.Findings)), Findings: checkFindings(result.Findings)})
	return nil
}

// --- Asks as the dataset writes them.

// splitAsk is an ask's text as the dataset writes it (dsAskText: the question, then each option on
// a line "- label: description") split into the question and its options: the run of lines at
// the end that start with "- ". A question that itself ends in a list reads that list as options,
// which joinAsk writes back as it was, so the text a person sees is the same either way.
func splitAsk(text string) (string, []model.AskOption) {
	lines := strings.Split(text, "\n")
	start := len(lines)
	for start > 1 && strings.HasPrefix(lines[start-1], "- ") {
		start--
	}
	options := []model.AskOption{}
	for _, line := range lines[start:] {
		label, description, _ := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
		options = append(options, model.AskOption{Label: label, Description: description})
	}
	return strings.Join(lines[:start], "\n"), options
}

// joinAsk is an ask's question and options as the dataset writes them (dsAskText).
func joinAsk(question string, options []model.AskOption) string {
	var b strings.Builder
	b.WriteString(question)
	for _, o := range options {
		b.WriteString("\n- " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
	}
	return b.String()
}
