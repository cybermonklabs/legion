// Package judge is the secrets broker's model judge: the background run that decides a pending
// human-tier credential request the airlock rules allow it to decide, and leaves every other
// request for the person who decides it today. It approves or refers, and it never denies.
//
// One run is one request. It makes five checks of its own before any model is asked — the rules
// entry's judge bound, the request's shape and lifetime, the tag policy re-read now, what the
// session has had withheld from it, and the Dispatch issue the signed reason names — and stops
// at the first that fails, leaving the request pending with a request.judged row that says which
// check it was and asking the model nothing. A request that passes all five, and that the
// machine still shows pending when the run reads its state once more (Runner.stillPending), is
// put to one model through packages/envoy/agentloop, as data to judge rather than as
// instructions (D15): approve is written with the approval itself, in one transaction, and
// refer writes the row alone.
//
// Nothing a run does is on a caller's path. A run that goes past its budget, and a broker that
// stops serving under one, write nothing at all, which leaves the request exactly as the hook
// handed it over: pending, for its person.
package judge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// runBudget is how long one run has, taken when the run starts (D9): the Dispatch phase,
// bounded at dispatchBound, and the model call, bounded at modelBound (model.go), with room
// left over for everything a run asks of the request machine.
const runBudget = 30 * time.Second

// dispatchBound is how long the whole Dispatch phase of one run has: both of Runner.issue's
// GETs together. The three bounds sit together because they have to add up, and they add up
// with time to spare: 5 seconds and 20 of the 30 runBudget gives, so the worst case — a
// Dispatch phase that spends its whole bound and a model call that then times out — reaches
// Runner.write with 5 seconds of the budget left. That is what the arithmetic is for. A run
// that reaches runBudget writes nothing at all — Runner.write returns on ctx.Err() — so a
// broker that paid for two HTTP reads and a model call would record no request.judged row,
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

// doneStatus is the one Dispatch issue status the judge refuses to decide against. The design
// says "not done or canceled"; Dispatch has no canceled status at all (model.IssueStatuses,
// internal/dispatch/model/model.go), so this is the whole of that rule. The word is spelled here
// rather than imported, because the broker reads Dispatch over its API and shares no types
// with it.
const doneStatus = "done"

// The five checks a run makes before the model, in the order it makes them. These are the names
// a request.judged row calls them by, so a reader of the row and a reader of this package see
// one set of words.
const (
	checkRulesJudge = "rules-judge"
	checkLifetime   = "lifetime"
	checkPolicy     = "policy"
	checkWithheld   = "withheld"
	checkIssue      = "issue"
)

// check is one check and what it found. Detail says what the check read, and is written for a
// check that failed; a check that passed is its name and its result.
type check struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Requests is what a run asks of the broker's request machine: the two questions it must ask
// again before it decides, and the two writes a verdict makes. It is declared here, in the
// consumer, so that this package depends on no concrete machine; *requests.Machine answers all
// four, and requests imports nothing of this package.
type Requests interface {
	// JudgePolicy answers, for the enrollment that made a pending request, what the live
	// policy says about one of its names at this moment: the decision policy.Set.Evaluate
	// makes for that enrollment's requester, and whether the name is withheld from the
	// session. The judge asks the machine one question instead of reading the policy
	// itself, because what a requester is — the person it acts for, the service its
	// account still proves, and the names its operator took back from it — is read from the
	// database and is the machine's to answer (requests.Machine.requester).
	JudgePolicy(ctx context.Context, enrollmentID, name string) (now policy.Decision, withheld bool, err error)

	// JudgeRequestState answers the state a decision would meet now for the record a run
	// is judging: "pending" for a request still waiting on its person, "expired" for one
	// whose pending window has passed though the sweep has not written that yet, and the
	// row's own word for one a person decided or a session cancelled. The run reads it
	// immediately before the model call (Runner.stillPending), so a request that went away
	// while the run was reading Dispatch costs no model call.
	JudgeRequestState(ctx context.Context, recordID string) (state string, err error)

	// ApplyJudgeDecision writes the approval and its request.judged row in one
	// transaction, so a grant and the record of why it exists commit together or not at all.
	ApplyJudgeDecision(ctx context.Context, recordID, modelID string, judged map[string]any) (requests.Decision, error)

	// LogJudged writes the request.judged row alone, which is every verdict that decides
	// nothing: a referral, a check that failed, and an approval the state machine then
	// refused.
	LogJudged(ctx context.Context, recordID, modelID string, judged map[string]any) error
}

// Options are what a Runner judges with.
type Options struct {
	// Rules is the airlock rules as the broker last read them, which say which secrets the
	// judge may decide at all and for how long.
	Rules airlock.Rules
	// Dispatch is the read of the issue a request's reason names, and Model the one model
	// the run asks.
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
// waiting for a person while another request's model call ran, which is the wait the judge
// exists to take away, and no load the broker has met needs one: a run is one HTTP read, one
// model call and one short transaction.
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
	row := verdictRow{checks: checks, verdict: Verdict{Word: Refer}}
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
// session can cancel the request, the person who decides it can decide it, or its pending
// window can pass. Consider caps nothing and no limit covers a session's creates, so a session
// looping create, cancel, create would otherwise start one model call per create with nothing
// to stop it; with this read a live request costs at most one model call and a request that is
// gone costs none. A cancelled or decided request must not be judged in any case: the state
// machine refuses a decision on it, so the model call would buy nothing at all.
//
// A run stopped here still writes its row. No request row is ever deleted — a cancel and an
// expiry are state changes — so LogJudged finds the request and the enrollment it hangs off,
// and the trail says why a run that cost a Dispatch read decided nothing.
//
// It is a gate and not a sixth entry in the checks array. The five checks answer one question —
// may the judge decide this request? — and a reader of a row reads them as that one list, which
// the verification document's check k10 and scripts/judge-check.sh pin by name and in order.
// This answers another question altogether: is there still a request to decide? Nothing is
// wrong with a request that was cancelled, so no check of it failed, and the row says what
// happened in a key of its own.
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

// beforeTheModel makes the five checks, in order, and builds the data the model would be shown.
// It stops at the first failure, so a request the rules do not let the judge decide costs no
// Dispatch read and no model call. Under Options.SkipChecks it makes every check all the same,
// recording each, so the negative control shows the model a request a check would have stopped.
func (r *Runner) beforeTheModel(ctx context.Context, p requests.PendingRequest) ([]check, Data) {
	var checks []check
	// add records one check and answers whether the run goes on: it stops at the first
	// failure, unless the checks have been turned off as a bar.
	add := func(name string, ok bool, detail string) bool {
		c := check{Check: name, Passed: ok}
		if !ok {
			c.Detail = detail
		}
		checks = append(checks, c)
		return ok || r.opts.SkipChecks
	}

	// The name is the request's first, so that the data in front of the model says what
	// was asked for even under SkipChecks, where a request for two secrets still reaches
	// it. Both of the first two checks refuse a request that names other than one.
	name := ""
	if len(p.Secrets) > 0 {
		name = p.Secrets[0]
	}
	data := Data{Secret: name, Rules: r.opts.Rules.Lookup(name), Reason: p.Reason}

	seconds, judged := r.opts.Rules.JudgeSeconds(name)
	switch {
	case len(p.Secrets) != 1:
		if !add(checkRulesJudge, false, decidesOneSecret(len(p.Secrets))) {
			return checks, data
		}
	case !judged:
		if !add(checkRulesJudge, false, decidesNothingFor(name)) {
			return checks, data
		}
	default:
		add(checkRulesJudge, true, "")
	}

	if ok, detail := lifetimeAllowed(p, seconds, judged); !add(checkLifetime, ok, detail) {
		return checks, data
	}

	now, withheld, err := r.opts.Requests.JudgePolicy(ctx, p.EnrollmentID, name)
	switch {
	case err != nil:
		if !add(checkPolicy, false, "the policy could not be re-read: "+err.Error()) {
			return checks, data
		}
	case now.Outcome != policy.Approval:
		if !add(checkPolicy, false, fmt.Sprintf("the policy answers %s for %s now, not approval", now.Outcome, name)) {
			return checks, data
		}
	default:
		add(checkPolicy, true, "")
	}

	if !add(checkWithheld, !withheld, fmt.Sprintf("%s is withheld from this session", name)) {
		return checks, data
	}

	ok, detail := r.issue(ctx, p, &data)
	add(checkIssue, ok, detail)
	return checks, data
}

// lifetimeAllowed is the second check: the request names one secret, its grant has a lifetime —
// the one it signed, else the broker's default (requests.PendingRequest.LifetimeSeconds) — and
// that lifetime is within both bounds: the class's own 15 minutes and the seconds the rules give
// the judge for that secret.
func lifetimeAllowed(p requests.PendingRequest, seconds uint32, judged bool) (bool, string) {
	switch {
	case len(p.Secrets) != 1:
		return false, decidesOneSecret(len(p.Secrets))
	case p.LifetimeSeconds == nil:
		return false, "the request signed no lifetime, and the judge decides none that lasts as long as the session"
	case time.Duration(*p.LifetimeSeconds)*time.Second > maxJudgedLifetime:
		return false, fmt.Sprintf("the request asks for %d seconds, past the %s the judge decides", *p.LifetimeSeconds, maxJudgedLifetime)
	case !judged:
		return false, decidesNothingFor(p.Secrets[0])
	case !record.JudgeAllowsLifetime(seconds, *p.LifetimeSeconds):
		return false, fmt.Sprintf("the request asks for %d seconds of %s, past the %d the rules let the judge decide", *p.LifetimeSeconds, p.Secrets[0], seconds)
	}
	return true, ""
}

// decidesOneSecret and decidesNothingFor are the two sentences the first check and the second
// both write. The two checks are deliberately made twice — under Options.SkipChecks the first
// no longer stops the run, so the second meets a request the first refused — and what a reader
// of the row sees for one request's two rows is spelled once.
func decidesOneSecret(secrets int) string {
	return fmt.Sprintf("the request names %d secrets, and the judge decides a request for one", secrets)
}

func decidesNothingFor(name string) string {
	return fmt.Sprintf("the airlock rules let the judge decide nothing for %s", name)
}

// issue is the fifth check, and the only one that reads anything outside the broker: the signed
// reason names a Dispatch issue key, and Dispatch shows that issue at a status that is not done.
// What it read goes into data, so the model sees the issue the check passed on; a read that
// failed still leaves the key there, which is what the negative control shows the model.
//
// The two GETs share one dispatchBound between them, so the phase costs the run what runBudget
// gave it however the reads go. They stay one after the other: a refused issue read means the
// spec is never asked for.
func (r *Runner) issue(ctx context.Context, p requests.PendingRequest, data *Data) (bool, string) {
	key := record.IssueKeyInReason(p.Reason)
	if key == "" {
		return false, "the request's signed reason names no Dispatch issue"
	}
	data.Issue.Key = key
	readCtx, cancel := context.WithTimeout(ctx, dispatchBound)
	defer cancel()
	issue, err := r.opts.Dispatch.Issue(readCtx, key)
	switch {
	case errors.Is(err, ErrNoIssue):
		return false, fmt.Sprintf("dispatch shows no issue %s", key)
	case err != nil:
		return false, fmt.Sprintf("issue %s could not be read: %v", key, err)
	}
	data.Issue = IssueData{
		Key: issue.Key, Title: issue.Title, Status: issue.Status,
		Spec: specExtract(issue.Spec), SpecPresent: issue.HasSpec,
	}
	if issue.Status == doneStatus {
		return false, fmt.Sprintf("issue %s is %s", key, issue.Status)
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
	// notApplied is why an approve verdict's approval wrote nothing at all, and is what
	// the row's applied is read off: a row this package writes on its own says applied
	// false exactly when the machine refused the attempt. The row written inside the
	// decision's own transaction carries an applied of that transaction's making, since
	// only the transaction knows what it did (requests' decider.judgedRow).
	notApplied error
	// decided is the state that transaction left the request in — "granted", or "denied"
	// where the approval committed and granted nothing — for the log line alone, and is
	// empty for every run that made no decision.
	decided string
}

// detail is the row's detail object, the one place its keys are spelled.
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
		detail["error"] = sanitize(v.err.Error(), maxRawAnswer)
	}
	if v.notPending != "" {
		detail["not_pending"] = v.notPending
	}
	if v.notApplied != nil {
		detail["applied"] = false
		detail["not_applied"] = sanitize(v.notApplied.Error(), maxRawAnswer)
	}
	return detail
}

// write is the one place a run writes anything. An approve verdict goes through
// ApplyJudgeDecision, which commits the approval and the row together; every other verdict, and
// an approval the state machine refused, writes the row alone.
func (r *Runner) write(ctx context.Context, p requests.PendingRequest, row verdictRow) {
	if ctx.Err() != nil {
		// The run's budget went, or the broker stopped serving, so nothing is written
		// and the request stays as it was: pending, for the person who decides it
		// today, which is where a judge that never ran would have left it.
		slog.Info("broker judge: the run ended before it wrote anything", "record", p.RecordID,
			"request", p.RequestID, "secrets", p.Secrets, "reason", ctx.Err())
		return
	}
	modelID := r.opts.Model.ID
	if row.verdict.Word == Approve {
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
		// A person decided the request first, or it expired, while the judge was
		// reading: the ordinary outcome of losing a race and no fault of the judge's
		// (D10). Any other refusal is one the judge should not have reached at all.
		// Either way the approval wrote nothing, and the verdict is still the record of
		// what the judge was asked and what it answered, so it is written on its own.
		row.notApplied = err
		if errors.Is(err, requests.ErrTerminal) || errors.Is(err, requests.ErrExpired) {
			slog.Info("broker judge: the request was decided before the judge's approval landed",
				"record", p.RecordID, "request", p.RequestID, "model", modelID, "reason", err)
		} else {
			slog.Error("broker judge: the judge's approval was refused", "record", p.RecordID,
				"request", p.RequestID, "model", modelID, "err", err)
		}
	}
	if err := r.opts.Requests.LogJudged(ctx, p.RecordID, modelID, row.detail(modelID)); err != nil {
		slog.Error("broker judge: the verdict could not be written", "record", p.RecordID,
			"request", p.RequestID, "model", modelID, "err", err)
		return
	}
	r.logged(p, row)
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
	if failed := firstFailure(row.checks); failed != nil {
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
	// applied is the approve verdict's alone: what the decision transaction did where it
	// ran, and false where the machine refused the attempt outright.
	switch {
	case row.decided != "":
		attrs = append(attrs, "applied", row.decided == "granted", "decided", row.decided)
	case row.notApplied != nil:
		attrs = append(attrs, "applied", false)
	}
	slog.Info("broker judge: wrote a verdict", attrs...)
}

// firstFailure is the check a run stopped at, and nil for a run every check passed.
func firstFailure(checks []check) *check {
	for i := range checks {
		if !checks[i].Passed {
			return &checks[i]
		}
	}
	return nil
}
