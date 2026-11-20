// Package judge is the secrets broker's model judge: the background run that decides a pending
// request for keys the airlock rules let it decide — every key that needs a decision has a judge
// entry, and none carries an administrator's tag (requests.Machine.judgeDecides). A key the
// session gets at once may stand beside them; it needs no decision, so the judge decides nothing
// of it and its grant releases it with the rest. The broker holds such a request for the judge
// (requests.judge_until): it is on no person's list and no person may decide it while the run
// lasts. The judge grants it or refuses it, and a refusal is final; a run that cannot decide
// hands the request to its person, once.
//
// One run is one request. It makes five checks of its own before any model is asked — the rules
// entry of every key it decides, the request's lifetime against each of those keys' judge
// seconds, the tag policy re-read now (a key it decides must still need approval, and a key the
// session got at once must still need none), whether a key has become an administrator's, what
// the session has had withheld from it — and stops at the first that fails. Two of them refuse
// the request outright, since no grant could follow: the policy now denies a key, or a key is
// withheld from the session. Every other failure hands the request over. A run whose checks pass
// reads what the reason points at in Dispatch — the issue it names and the ask it cites — as
// data, and, if the machine still shows the request pending (Runner.stillPending), puts it to one
// model through packages/envoy/agentloop, with the guidance of each key it decides as the
// instruction to follow and everything an agent wrote as data to judge (D15). Grant is written
// with the approval itself, in one transaction, and deny with the refusal itself; a model that
// gives no verdict hands the request over.
//
// Nothing a run does is on a caller's path. A run that goes past its budget, and a broker that
// stops serving under one, write nothing at all, and the hold then runs out, which hands the
// request to its person (requests.JudgeHold).
package judge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// runBudget is how long one run has, taken when the run starts (D9): the Dispatch phase, which
// is the issue read and then the ask read, each bounded at dispatchBound and each made only when
// the reason points at one, and the model call, bounded at modelBound (model.go), with room left
// over for everything a run asks of the request machine.
const runBudget = 35 * time.Second

// dispatchBound is how long one read of Dispatch has: both of Runner.issue's GETs together, and
// then Runner.ask's own GET, which gets a bound of its own because it is a second read and not
// part of the first. The three bounds sit together because they have to add up, and they add up
// with time to spare: two of 5 seconds and 20 of the 35 runBudget gives, so the worst case — a
// run whose two Dispatch reads each spend their whole bound and whose model call then times out
// — reaches Runner.write with 5 seconds of the budget left. That is what the arithmetic is for.
// A run that reaches runBudget writes nothing at all — Runner.write returns on ctx.Err() — so a
// broker that paid for three HTTP reads and a model call would record no request.judged row,
// which is the one outcome the audit trail exists to prevent. The 5 seconds left over are the
// run's own reads of the request machine — JudgePolicy before the Dispatch phase and
// JudgeRequestState after it — and the short transaction that writes the row, each of them a
// local database call.
//
// cmd/broker's judgeDispatchTimeout is the per-call backstop under this and not the phase's
// bound: http.Client.Timeout is per Do, and Issue makes two calls, so the client alone would
// let the reads take twice as long as the arithmetic above allows. Five seconds for the whole
// phase is inside that per-call bound, so this deadline is the one that ends a stalled read of
// a run and the client's is the backstop behind it.
const dispatchBound = 5 * time.Second

// maxJudgedLifetime is the longest grant lifetime the judge decides, whatever judge seconds a
// rules entry allows: the bound of the class itself.
const maxJudgedLifetime = 15 * time.Minute

// notFound is the state an issue or an ask is shown to the model in when Dispatch holds none
// under the key or id the reason gave.
const notFound = "not found"

// The five checks a run makes before the model, in the order it makes them. These are the names
// a request.judged row calls them by, so a reader of the row and a reader of this package see
// one set of words.
//
// They read from the request outwards: what the rules say of each key, what the request itself
// asks for, what the policy answers for each key now, what each key is, and what this session
// has had taken back. checkAdmin sits directly after checkPolicy because the two are one read of
// the machine per key (Requests.JudgePolicy): the policy's outcome and the key's admin tag come
// back together. Two of them refuse the request for good when they fail — the policy denying a
// key, and a key withheld from the session — since no decision of anyone's could grant it; the
// rest hand the request to its person, who may.
const (
	checkRulesJudge = "rules-judge"
	checkLifetime   = "lifetime"
	checkPolicy     = "policy"
	checkAdmin      = "admin"
	checkWithheld   = "withheld"
	// checkDispatch is no check of the request: it is the run's read of what the reason points
	// at, recorded as a failed check when a read fails, so the row says why the request went to
	// its person.
	checkDispatch = "dispatch"
)

// check is one check and what it found. Detail says what the check read, and is written for a
// check that failed; a check that passed is its name and its result. Final is set on a failed
// check that refuses the request for good, rather than handing it to its person.
type check struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
	Final  bool   `json:"final,omitempty"`
}

// Requests is what a run asks of the broker's request machine: the two questions it must ask
// again before it decides, and the three writes a verdict makes. It is declared here, in the
// consumer, so that this package depends on no concrete machine; *requests.Machine answers all
// five, and requests imports nothing of this package.
type Requests interface {
	// JudgePolicy answers, for the enrollment that made a pending request, what the live
	// policy says about one of its names at this moment: the decision policy.Set.Evaluate
	// makes for that enrollment's requester, whether the name is withheld from the
	// session, and whether the secret is an administrator's (policy.Secret.Admin, the
	// admin: "true" tag). The judge asks the machine one question instead of reading the
	// policy itself, because what a requester is — the person it acts for, the service its
	// account still proves, and the names its operator took back from it — is read from the
	// database and is the machine's to answer (requests.Machine.requester).
	//
	// The tag comes back beside the decision rather than from a read of the judge's own,
	// because it is read off the very snapshot the decision was made against: a judge
	// holding a policy.Set of its own could answer for one snapshot while deciding on
	// another, and the promise an admin tag makes — that a person approves every release —
	// is not one to leave to two reads that may disagree.
	JudgePolicy(ctx context.Context, enrollmentID, name string) (now policy.Decision, withheld, admin bool, err error)

	// JudgeRequestState answers the state a decision would meet now for the record a run
	// is judging: "pending" for a request still waiting, "expired" for one whose pending
	// window has passed though the sweep has not written that yet, and the row's own word
	// for one decided or cancelled. The run reads it immediately before the model call
	// (Runner.stillPending), so a request that went away while the run was reading Dispatch
	// costs no model call.
	JudgeRequestState(ctx context.Context, recordID string) (state string, err error)

	// ApplyJudgeDecision writes the approval and its request.judged row in one
	// transaction, so a grant and the record of why it exists commit together or not at all.
	ApplyJudgeDecision(ctx context.Context, recordID, modelID string, judged map[string]any) (requests.Decision, error)

	// LogJudged writes the request.judged row of a run that decided nothing and hands the
	// request to its person: it ends the judge's hold in the same transaction. That is a
	// check that failed other than finally, a read of Dispatch that failed, a model that gave
	// no verdict, and an approval the state machine then refused.
	LogJudged(ctx context.Context, recordID, modelID string, judged map[string]any) error

	// DenyJudged ends a request the judge holds as the judge's refusal: the denial, why as
	// its detail for the agent to read, and the request.judged row, in one transaction. It
	// refuses a request the judge does not hold (requests.ErrJudgeHoldEnded).
	DenyJudged(ctx context.Context, recordID, modelID string, judged map[string]any, why string) (requests.Decision, error)
}

// Options are what a Runner judges with.
type Options struct {
	// Rules is the airlock rules as the broker last read them, which say which keys the judge
	// may decide at all, for how long, and on what guidance.
	Rules airlock.Rules
	// Dispatch is the read of what a request's reason points at, and Model the one model the
	// run asks.
	Dispatch Dispatch
	Model    Model
	// Requests is the machine a verdict is written through.
	Requests Requests
	// SkipChecks turns the five checks off as a bar: each is still made and still recorded,
	// and none of them holds a request back, so the model is asked about a request a check
	// would have stopped. It is the negative control of the judge's own checks and is for
	// the broker's local stack alone (BROKER_JUDGE_TEST_SKIP_CHECKS).
	SkipChecks bool
}

// Runner judges the pending requests a broker hands it. New takes the broker's serving
// lifetime, which every run hangs off, so the runs end when the broker does.
type Runner struct {
	ctx  context.Context
	opts Options
}

// New is the runner for one broker. ctx is that broker's serving lifetime.
func New(ctx context.Context, o Options) *Runner {
	return &Runner{ctx: ctx, opts: o}
}

// Consider starts one run for one pending request, on a goroutine of its own with runBudget off
// the runner's context, and returns at once: the request that created p is already answered, and
// nothing a run does belongs on its path.
//
// There is no bound on the runs in flight and no queue. A queue would leave a legitimate request
// waiting while another request's model call ran, which is the wait the judge exists to take
// away, and no load the broker has met needs one: a run is a Dispatch read or two, one model call
// and one short transaction.
func (r *Runner) Consider(p requests.PendingRequest) {
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, runBudget)
		defer cancel()
		r.consider(ctx, p)
	}()
}

// consider is one run, from the first check to the row it writes.
func (r *Runner) consider(ctx context.Context, p requests.PendingRequest) {
	if ctx.Err() != nil {
		return
	}
	checks, data := r.beforeTheModel(ctx, p)
	row := verdictRow{checks: checks, verdict: Verdict{Word: Deny}}
	if firstFailure(checks) == nil || r.opts.SkipChecks {
		if row.notPending = r.stillPending(ctx, p); row.notPending == "" {
			row.asked = true
			row.verdict, row.usage, row.raw, row.err = r.opts.Model.Ask(ctx, data)
		}
	}
	r.write(ctx, p, row)
}

// stillPending is the one read a run makes between its checks and the model call, and the last
// thing standing between a request and a model call somebody pays for. It answers "" for a
// request the machine still shows pending, and otherwise the sentence the row records for
// having asked nothing.
//
// It is read again because nothing has said the request is still there since the hook handed it
// over: the Dispatch phase above may spend the whole of dispatchBound, and in those seconds the
// session can cancel the request or its pending window can pass. A cancelled request must not be
// judged: the state machine refuses a decision on it, so the model call would buy nothing at all,
// and the requester is told nothing by it either.
//
// What it does not do is bound how many model calls one session can start: each create makes a
// request of its own, live for the milliseconds this run spends before the model call, so a
// session that loops create, cancel, create pays for one model call per round. D8 decides not
// to cap the runs in flight or limit a session's creates.
//
// A run stopped here still writes its row. No request row is ever deleted — a cancel and an
// expiry are state changes — so LogJudged finds the request and the enrollment it hangs off,
// and the trail says why a run that cost a Dispatch read decided nothing.
//
// It is a gate and not an entry in the checks array. The checks answer one question — may the
// judge decide this request? — and a reader of a row reads them as that one list, which the
// verification document's check k10 names in order and TestTheChecksBeforeTheModel holds by name. This
// answers another question altogether: is there still a request to decide? Nothing is wrong
// with a request that was cancelled, so no check of it failed, and the row says what happened in
// a key of its own.
func (r *Runner) stillPending(ctx context.Context, p requests.PendingRequest) string {
	state, err := r.opts.Requests.JudgeRequestState(ctx, p.RecordID)
	switch {
	case err != nil:
		// A state that could not be read is one that may have moved, and the model call
		// is the expensive half of a run: the run fails closed and asks nothing.
		return "the request's state could not be read: " + err.Error()
	case state != "pending":
		return "the request is no longer pending: it is " + state
	}
	return ""
}

// beforeTheModel makes the five checks, in order, then the reads of what the reason points at,
// and builds the data the model would be shown. It stops at the first failure, so a request the
// rules do not let the judge decide costs no Dispatch read and no model call. Under
// Options.SkipChecks it makes every check all the same, recording each, so the negative control
// shows the model a request a check would have stopped.
func (r *Runner) beforeTheModel(ctx context.Context, p requests.PendingRequest) ([]check, Data) {
	var checks []check
	// add records one check and answers whether the run goes on: it stops at the first
	// failure, unless the checks have been turned off as a bar. final marks a failure that
	// refuses the request for good.
	add := func(name string, ok bool, detail string, final bool) bool {
		c := check{Check: name, Passed: ok}
		if !ok {
			c.Detail, c.Final = detail, final
		}
		checks = append(checks, c)
		return ok || r.opts.SkipChecks
	}

	// The model is shown the keys the judge decides and nothing of a key the session gets at
	// once beside them, which no guidance is about.
	data := Data{Reason: p.Reason}
	for _, name := range p.Decided {
		data.Keys = append(data.Keys, KeyData{Name: name, Rules: r.opts.Rules.Lookup(name), Guidance: r.opts.Rules.Guidance(name)})
	}

	if ok, detail := r.rulesJudge(p); !add(checkRulesJudge, ok, detail, false) {
		return checks, data
	}
	if ok, detail := r.lifetimeAllowed(p); !add(checkLifetime, ok, detail, false) {
		return checks, data
	}

	// One read of the machine per key answers the next three checks: the policy's outcome,
	// the admin tag and the withheld names all come back together, off one snapshot. Each
	// check names the first key it failed for. Every key of the request is read, the ones
	// granted at once too, since the grant releases them all: a key the judge decides must
	// still need approval, and a key the session got at once must still need none.
	//
	// A key the policy now denies, and a key withheld from the session, refuse the request for
	// good whatever else the snapshot shows (R2: a refusal is final). So a deny outranks another
	// key's failure of the policy check, a withheld key is judged by the withheld check alone —
	// the policy folds a withheld name into an approval, which would otherwise read as a key
	// that needs a decision now — and a failure of the policy or admin check that would hand the
	// request over does not stop the run before the withheld check is recorded.
	policyOK, policyDetail, policyFinal := true, "", false
	adminOK, adminDetail := true, ""
	withheldOK, withheldDetail := true, ""
	for _, name := range p.Secrets {
		now, withheld, admin, err := r.opts.Requests.JudgePolicy(ctx, p.EnrollmentID, name)
		if err != nil {
			if policyOK {
				policyOK, policyDetail = false, "the policy could not be re-read: "+err.Error()
			}
			continue
		}
		if admin && adminOK {
			adminOK, adminDetail = false, decidedByAPerson(name)
		}
		if withheld && withheldOK {
			withheldOK, withheldDetail = false, fmt.Sprintf("%s is withheld from this session", name)
		}
		decides := slices.Contains(p.Decided, name)
		switch {
		case policyFinal:
		case now.Outcome == policy.Deny:
			policyOK, policyDetail, policyFinal = false, fmt.Sprintf("the policy denies %s to this session now", name), true
		case withheld, !policyOK:
		case decides && now.Outcome != policy.Approval:
			policyOK, policyDetail = false, fmt.Sprintf("the policy answers %s for %s now, not approval", now.Outcome, name)
		case !decides && now.Outcome != policy.Automatic:
			policyOK, policyDetail = false, fmt.Sprintf("%s needs a decision now, where the session got it at once when it asked", name)
		}
	}
	refused := policyFinal || !withheldOK
	if !add(checkPolicy, policyOK, policyDetail, policyFinal) && (policyFinal || !refused) {
		return checks, data
	}
	if !add(checkAdmin, adminOK, adminDetail, false) && !refused {
		return checks, data
	}
	if !add(checkWithheld, withheldOK, withheldDetail, true) {
		return checks, data
	}

	// What the reason points at, read as data. A key or an id Dispatch holds nothing under is
	// shown to the model as that; a read that failed hands the request to its person, since
	// the guidance may need what the read would have shown.
	if ok, detail := r.issue(ctx, p, &data); !ok {
		add(checkDispatch, false, detail, false)
		return checks, data
	}
	if ok, detail := r.ask(ctx, p, &data); !ok {
		add(checkDispatch, false, detail, false)
	}
	return checks, data
}

// rulesJudge is the first check: every key the judge decides has a judge entry in the rules the
// broker holds now. Create held the request for the judge on the rules it read then; a reload
// since may have taken an entry away, and such a request is its person's to decide. A key the
// session gets at once needs no decision, so no entry of it is asked for.
func (r *Runner) rulesJudge(p requests.PendingRequest) (bool, string) {
	if len(p.Decided) == 0 {
		return false, "the request names no key that needs a decision"
	}
	for _, name := range p.Decided {
		if _, judged := r.opts.Rules.JudgeSeconds(name); !judged {
			return false, decidesNothingFor(name)
		}
	}
	return true, ""
}

// lifetimeAllowed is the second check: the grant has a lifetime — the one the request signed,
// else the broker's default, which for a judge key is its judge seconds — within the class's 15
// minutes and within the judge seconds of every key the judge decides. Create refuses a request
// that asks for longer, so a failure here is rules that moved under the request.
func (r *Runner) lifetimeAllowed(p requests.PendingRequest) (bool, string) {
	switch {
	case p.LifetimeSeconds == nil:
		return false, "the request signed no lifetime, and the judge decides none that lasts as long as the session"
	case time.Duration(*p.LifetimeSeconds)*time.Second > maxJudgedLifetime:
		return false, fmt.Sprintf("the request asks for %d seconds, past the %s the judge decides", *p.LifetimeSeconds, maxJudgedLifetime)
	}
	for _, name := range p.Decided {
		seconds, judged := r.opts.Rules.JudgeSeconds(name)
		if !judged {
			return false, decidesNothingFor(name)
		}
		if !record.JudgeAllowsLifetime(seconds, *p.LifetimeSeconds) {
			return false, fmt.Sprintf("the request asks for %d seconds of %s, past the %d the rules let the judge decide", *p.LifetimeSeconds, name, seconds)
		}
	}
	return true, ""
}

// decidesNothingFor is the sentence the first two checks write for a key the rules give the judge
// nothing of.
func decidesNothingFor(name string) string {
	return fmt.Sprintf("the airlock rules let the judge decide nothing for %s", name)
}

// decidedByAPerson is the sentence the admin check writes, and the one a person reads in the
// request.judged row of a request the judge handed over for being an administrator's. It says
// both halves of why: what the key is, and that the promise its tag makes is unconditional —
// every release of such a credential is approved by a person, and no rules file that gives the
// judge seconds of the name can undo that (policy.TagAdmin, the broker's concepts document).
func decidedByAPerson(name string) string {
	return fmt.Sprintf("%s is an administrator's secret, and a person approves every release of one", name)
}

// issue reads the Dispatch issue the signed reason names, when it names one, into data, so the
// model can read it against the keys' guidance. A key Dispatch holds no issue for is shown as not
// found; a read that failed answers false, and the run hands the request over. The two GETs share
// one dispatchBound between them, so the phase costs the run what runBudget gave it however the
// reads go.
func (r *Runner) issue(ctx context.Context, p requests.PendingRequest, data *Data) (bool, string) {
	key := record.IssueKeyInReason(p.Reason)
	if key == "" {
		return true, ""
	}
	readCtx, cancel := context.WithTimeout(ctx, dispatchBound)
	defer cancel()
	issue, err := r.opts.Dispatch.Issue(readCtx, key)
	switch {
	case errors.Is(err, ErrNoIssue):
		data.Issue = &IssueData{Key: key, Status: notFound}
		return true, ""
	case err != nil:
		data.Issue = &IssueData{Key: key}
		return false, fmt.Sprintf("issue %s could not be read: %v", key, err)
	}
	data.Issue = &IssueData{
		Key: issue.Key, Title: issue.Title, Status: issue.Status,
		Spec: specExtract(issue.Spec), SpecPresent: issue.HasSpec, Found: true,
	}
	return true, ""
}

// ask reads the Dispatch ask the signed reason cites, when it cites one, into data: its issue,
// its state, the question, the options, and the answer with who gave it. Nothing requires a
// reason to cite one; a key's guidance can ask for one, and this is what lets the model see that
// the ask is real. An id Dispatch holds no ask for is shown as not found; a read that failed
// answers false, and the run hands the request over. The read gets a dispatchBound of its own.
func (r *Runner) ask(ctx context.Context, p requests.PendingRequest, data *Data) (bool, string) {
	_, id := record.AskRefInReason(p.Reason)
	if id == "" {
		return true, ""
	}
	readCtx, cancel := context.WithTimeout(ctx, dispatchBound)
	defer cancel()
	ask, err := r.opts.Dispatch.Ask(readCtx, id)
	switch {
	case errors.Is(err, ErrNoAsk):
		data.Ask = &AskData{ID: id, State: notFound}
		return true, ""
	case err != nil:
		return false, fmt.Sprintf("ask %s could not be read: %v", id, err)
	}
	data.Ask = &AskData{
		ID: ask.ID, IssueKey: ask.IssueKey, State: ask.State, Question: ask.Question,
		Options: ask.Options, Selected: ask.Selected, Text: ask.Text, AnsweredBy: ask.AnsweredBy,
	}
	return true, ""
}

// verdictRow is one run's outcome, and what its request.judged row is written from.
type verdictRow struct {
	checks []check
	// asked is whether the model was asked at all: a run stopped by a check was not.
	asked   bool
	verdict Verdict
	usage   agentloop.Usage
	// raw is the model's reply when it did not parse, and err what failed when the call
	// did; both are already clamped and filtered.
	raw string
	err error
	// notPending is what stillPending answered: why a run whose checks all passed asked
	// the model nothing. Empty for every other run.
	notPending string
	// notApplied is why a grant verdict's approval wrote nothing at all, and is what the
	// row's applied is read off: a row this package writes on its own says applied false
	// exactly when the machine refused the attempt. The row written inside the decision's
	// own transaction carries an applied of that transaction's making, since only the
	// transaction knows what it did (requests' decider.judgedRow).
	notApplied error
	// decided is the state that transaction left the request in — "granted", or "denied"
	// where the approval committed and granted nothing, or where the judge refused it — for
	// the log line alone, and is empty for every run that made no decision.
	decided string
	// refusal is what a refused request's agent was told (refusalOf), for the log line; empty
	// for every run that refused nothing.
	refusal string
}

// detail is the row's detail object, the one place this package spells its keys. The row an
// approval's own transaction writes gains one more, applied, which requests' decider.judgedRow
// sets from what that transaction did.
func (v verdictRow) detail(modelID string) map[string]any {
	detail := map[string]any{
		"checks":  v.checks,
		"model":   modelID,
		"asked":   v.asked,
		"verdict": v.verdict.Word,
		"reason":  v.verdict.Reason,
		// The judge sets no agentloop.Price, which is where every Usage.CostUSD comes
		// from, so each of them is zero. A zero cost in the row would read as a call
		// that was free, so the row keeps the counts and says it priced nothing (D13).
		"priced": false,
	}
	if v.asked {
		detail["tokens"] = map[string]any{
			"input": v.usage.Input, "output": v.usage.Output,
			"cache_write": v.usage.CacheWrite, "cache_read": v.usage.CacheRead,
			"reasoning": v.usage.Reasoning, "metered": v.usage.Metered,
		}
	}
	if v.raw != "" {
		detail["raw_answer"] = v.raw
	}
	if v.err != nil {
		detail["error"] = record.Sanitize(v.err.Error(), maxRawAnswer)
	}
	if v.notPending != "" {
		detail["not_pending"] = v.notPending
	}
	if v.notApplied != nil {
		detail["applied"] = false
		detail["not_applied"] = record.Sanitize(v.notApplied.Error(), maxRawAnswer)
	}
	return detail
}

// write is the one place a run writes anything, and it chooses between the three ends a request
// the judge holds can come to. A grant verdict goes through ApplyJudgeDecision, which commits the
// approval and the row together. A deny verdict, and a check that fails finally, go through
// DenyJudged, which refuses the request for good. Everything else — a check that fails other than
// finally, a Dispatch read that failed, a model that gave no verdict, an approval the machine
// then refused, and a request that is no longer pending — writes the row through LogJudged, which
// hands the request to its person.
func (r *Runner) write(ctx context.Context, p requests.PendingRequest, row verdictRow) {
	if ctx.Err() != nil {
		// The run's budget went, or the broker stopped serving, so nothing is written. The
		// hold runs out at its own time and the request is then its person's
		// (requests.JudgeHold).
		slog.Info("broker judge: the run ended before it wrote anything", "record", p.RecordID,
			"request", p.RequestID, "secrets", p.Secrets, "reason", ctx.Err())
		return
	}
	modelID := r.opts.Model.ID
	failed := decidingFailure(row.checks)
	switch {
	case row.notPending != "":
		// Nothing is left to decide.
	case failed != nil && failed.Final && !r.opts.SkipChecks:
		if r.refuse(ctx, p, row) {
			return
		}
	case failed != nil && !r.opts.SkipChecks:
		// Handed over: the request is its person's.
	case row.asked && row.err != nil:
		// The model gave no verdict: handed over.
	case row.asked && row.verdict.Word == Grant:
		dec, err := r.opts.Requests.ApplyJudgeDecision(ctx, p.RecordID, modelID, row.detail(modelID))
		if err == nil {
			// The transaction wrote its own request.judged row and set that row's
			// applied from what it did, so what is left for this line is the state
			// it answered with: an approval the current policy turned into the
			// request's own denial, or one whose enrollment had gone, commits and
			// grants nothing.
			row.decided = dec.State
			r.logged(p, row)
			return
		}
		// The request was decided or expired first, the hold ended under the run, or the
		// current policy moved while the judge was reading: the session's operator withheld
		// a key or its owner tagged it an administrator's (the two guards
		// currentPolicyAdmits makes for the judge alone), the key's owner changed, or its
		// max-grant fell below the request's lifetime. Each is the ordinary outcome of losing
		// a race and no fault of the judge's (D10), and the policy's four all answer
		// record.ErrNotApprover. Any other refusal is one the judge should not have reached
		// at all. Either way the approval wrote nothing; the row below hands a request still
		// pending to its person, and keeps the verdict as the record of what was asked.
		row.notApplied = err
		if errors.Is(err, requests.ErrTerminal) || errors.Is(err, requests.ErrExpired) ||
			errors.Is(err, requests.ErrJudgeHoldEnded) || errors.Is(err, record.ErrNotApprover) {
			slog.Info("broker judge: the request was decided, expired, handed over or moved out of the judge's policy before the judge's grant landed",
				"record", p.RecordID, "request", p.RequestID, "model", modelID, "reason", err)
		} else {
			slog.Error("broker judge: the judge's grant was refused", "record", p.RecordID,
				"request", p.RequestID, "model", modelID, "err", err)
		}
	case row.asked:
		if r.refuse(ctx, p, row) {
			return
		}
	}
	if err := r.opts.Requests.LogJudged(ctx, p.RecordID, modelID, row.detail(modelID)); err != nil {
		slog.Error("broker judge: the verdict could not be written", "record", p.RecordID,
			"request", p.RequestID, "model", modelID, "err", err)
		return
	}
	r.logged(p, row)
}

// refuse ends a request the judge holds with its refusal, the reason in its detail, and answers
// whether it did. A refusal the machine declines — the request was decided, expired or handed over
// in the meantime — leaves the verdict to be written alone; one that failed for another reason is
// logged as an error and does the same, so the trail keeps the verdict and the request goes to its
// person.
func (r *Runner) refuse(ctx context.Context, p requests.PendingRequest, row verdictRow) bool {
	modelID := r.opts.Model.ID
	why := refusalOf(row)
	if _, err := r.opts.Requests.DenyJudged(ctx, p.RecordID, modelID, row.detail(modelID), why); err != nil {
		if errors.Is(err, requests.ErrTerminal) || errors.Is(err, requests.ErrExpired) || errors.Is(err, requests.ErrJudgeHoldEnded) {
			slog.Info("broker judge: the request was decided, expired or handed over before the judge's refusal landed",
				"record", p.RecordID, "request", p.RequestID, "model", modelID, "reason", err)
		} else {
			slog.Error("broker judge: the judge's refusal was refused", "record", p.RecordID,
				"request", p.RequestID, "model", modelID, "err", err)
		}
		return false
	}
	row.decided = "denied"
	row.refusal = why
	r.logged(p, row)
	return true
}

// refusalOf is the sentence a refused request's agent reads in the request's decision detail
// (agent-secrets status, and the use command's own refusal): what the judge found, that this
// request will not be granted, and to tell whoever gave it the work. It tells the agent to try
// nothing again: a refusal is the end of the request. It holds the model's own words where the
// model's answer is the reason, clamped and filtered as the row holds them (D12).
func refusalOf(row verdictRow) string {
	const end = ". This request will not be granted. Tell whoever gave you this work."
	why := row.verdict.Reason
	if failed := decidingFailure(row.checks); failed != nil && failed.Final {
		why = failed.Detail
	}
	return "the model judge refused this request: " + record.Sanitize(why, maxRawAnswer-len(end)-40) + end
}

// logged is the one line a run that wrote a row leaves behind. It carries the model's own words
// — the reason, and the raw answer where there was one — already clamped and filtered (D12), the
// same strings the row holds and GET /v1/grant-log then serves.
func (r *Runner) logged(p requests.PendingRequest, row verdictRow) {
	attrs := []any{
		"record", p.RecordID, "request", p.RequestID, "session", p.SessionID,
		"approver", p.Approver, "secrets", p.Secrets, "model", r.opts.Model.ID,
		"asked", row.asked, "verdict", row.verdict.Word, "reason", row.verdict.Reason,
	}
	if failed := decidingFailure(row.checks); failed != nil {
		attrs = append(attrs, "failed_check", failed.Check, "failed_detail", failed.Detail)
	}
	if row.raw != "" {
		attrs = append(attrs, "raw_answer", row.raw)
	}
	if row.err != nil {
		attrs = append(attrs, "model_err", row.err)
	}
	if row.notPending != "" {
		attrs = append(attrs, "not_pending", row.notPending)
	}
	// applied is the grant verdict's alone: what the decision transaction did where it ran,
	// and false where the machine refused the attempt outright. A refusal is decided denied,
	// and carries what its agent was told. Every other row handed the request over.
	switch {
	case row.refusal != "":
		attrs = append(attrs, "applied", false, "decided", row.decided, "refusal", row.refusal)
	case row.decided != "":
		attrs = append(attrs, "applied", row.decided == "granted", "decided", row.decided)
	case row.notApplied != nil:
		attrs = append(attrs, "applied", false, "handed_over", true)
	case row.notPending == "":
		attrs = append(attrs, "handed_over", true)
	}
	slog.Info("broker judge: wrote a verdict", attrs...)
}

// firstFailure is the first check that failed, and nil for a run every check passed.
func firstFailure(checks []check) *check {
	for i := range checks {
		if !checks[i].Passed {
			return &checks[i]
		}
	}
	return nil
}

// decidingFailure is the check that decides how a run ends: the first that failed for good, which
// refuses the request whatever failed before it, else the first that failed, which hands it over;
// nil for a run every check passed.
func decidingFailure(checks []check) *check {
	for i := range checks {
		if !checks[i].Passed && checks[i].Final {
			return &checks[i]
		}
	}
	return firstFailure(checks)
}
