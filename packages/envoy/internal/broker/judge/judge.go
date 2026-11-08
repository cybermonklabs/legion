// Package judge is the secrets broker's model judge: the background run that decides a pending
// human-tier credential request the airlock rules allow it to decide, and leaves every other
// request for the person who decides it today. It approves or refers, and it never denies.
//
// One run is one request. It makes five checks of its own before any model is asked — the rules
// entry's judge bound, the request's shape and lifetime, the tag policy re-read now, what the
// session has had withheld from it, and the Dispatch issue the signed reason names — and stops
// at the first that fails, leaving the request pending with a request.judged row that says which
// check it was and asking the model nothing. A request that passes all five is put to one model
// through packages/envoy/agentloop, as data to judge rather than as instructions (D15): approve
// is written with the approval itself, in one transaction, and refer writes the row alone.
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

// runBudget is how long one run has, taken when the run starts (D9): the Dispatch read and the
// 20-second model call, with room.
const runBudget = 30 * time.Second

// maxJudgedLifetime is the longest signed lifetime the judge decides, whatever judge seconds a
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

// PendingRequest is one pending credential request as the hook hands it to the judge: what the
// run reads of the request, and nothing else.
//
// It is declared here rather than taken from internal/broker/requests so that this package names
// no concrete machine and that machine names no judge. A later task adds the same fields to
// requests as requests.PendingRequest, and the Machine.Judge hook converts one to the other,
// which is what keeps requests from importing this package. The cleaner seam, if the two types
// ever drift, is for requests to export the type and for this package to take it: the import
// would still run one way, since the hook is a func field the broker's main wires up and not a
// call requests makes into the judge.
type PendingRequest struct {
	// RecordID is the credential-request record a decision is written against, and
	// RequestID the request row it belongs to.
	RecordID, RequestID string
	// EnrollmentID is the enrollment that asked, which the policy is re-read for, and
	// SessionID the Envoy session it recorded, empty for an enrollment that names none.
	EnrollmentID, SessionID string
	// Secrets are the names the request asks for. The judge decides a request for one.
	Secrets []string
	// LifetimeSeconds is the lifetime the requester signed, nil for a grant that lasts
	// until the session ends. An unbounded grant is what the judge's seconds are there to
	// bound, so the judge decides none.
	LifetimeSeconds *int
	// Reason is the requester's own signed reason, which names the Dispatch issue. It is
	// part of the signed request, so it cannot change after the request was made.
	Reason string
	// Approver is the record's approver: the secret's owner, or record.AnyoneApprover for a
	// shared one. The judge decides in that approver's place and names it in its log line.
	Approver string
}

// Requests is what a run asks of the broker's request machine: the policy question it must ask
// again at decision time, and the two writes a verdict makes. It is declared here, in the
// consumer, so that this package depends on no concrete machine; *requests.Machine answers all
// three, and requests imports nothing of this package.
type Requests interface {
	// JudgePolicy answers, for the enrollment that made a pending request, what the live
	// policy says about one of its names at this moment: the decision policy.Set.Evaluate
	// makes for that enrollment's requester, and whether the name is withheld from the
	// session. The judge asks the machine one question instead of reading the policy
	// itself, because what a requester is — the person it acts for, the service its
	// account still proves, and the names its operator took back from it — is read from the
	// database and is the machine's to answer (requests.Machine.requester).
	JudgePolicy(ctx context.Context, enrollmentID, name string) (now policy.Decision, withheld bool, err error)

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
func (r *Runner) Consider(p PendingRequest) {
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, runBudget)
		defer cancel()
		r.consider(ctx, p)
	}()
}

// consider is one run, from the first check to the row it writes.
func (r *Runner) consider(ctx context.Context, p PendingRequest) {
	if ctx.Err() != nil {
		return
	}
	checks, data, passed := r.beforeTheModel(ctx, p)
	row := verdictRow{checks: checks, verdict: Verdict{Word: Refer}}
	if passed || r.opts.SkipChecks {
		row.asked = true
		row.verdict, row.usage, row.raw, row.err = r.opts.Model.Ask(ctx, data)
	}
	r.write(ctx, p, row)
}

// beforeTheModel makes the five checks, in order, and builds the data the model would be shown.
// It stops at the first failure, so a request the rules do not let the judge decide costs no
// Dispatch read and no model call. Under Options.SkipChecks it makes every check all the same,
// recording each, so the negative control shows the model a request a check would have stopped.
func (r *Runner) beforeTheModel(ctx context.Context, p PendingRequest) ([]check, Data, bool) {
	var checks []check
	passed := true
	// add records one check and answers whether the run goes on: it stops at the first
	// failure, unless the checks have been turned off as a bar.
	add := func(name string, ok bool, detail string) bool {
		c := check{Check: name, Passed: ok}
		if !ok {
			c.Detail, passed = detail, false
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
		if !add(checkRulesJudge, false, fmt.Sprintf("the request names %d secrets, and the judge decides a request for one", len(p.Secrets))) {
			return checks, data, passed
		}
	case !judged:
		if !add(checkRulesJudge, false, fmt.Sprintf("the airlock rules let the judge decide nothing for %s", name)) {
			return checks, data, passed
		}
	default:
		add(checkRulesJudge, true, "")
	}

	if ok, detail := lifetimeAllowed(p, seconds, judged); !add(checkLifetime, ok, detail) {
		return checks, data, passed
	}

	now, withheld, err := r.opts.Requests.JudgePolicy(ctx, p.EnrollmentID, name)
	switch {
	case err != nil:
		if !add(checkPolicy, false, "the policy could not be re-read: "+err.Error()) {
			return checks, data, passed
		}
	case now.Outcome != policy.Approval:
		if !add(checkPolicy, false, fmt.Sprintf("the policy answers %s for %s now, not approval", now.Outcome, name)) {
			return checks, data, passed
		}
	default:
		add(checkPolicy, true, "")
	}

	if !add(checkWithheld, !withheld, fmt.Sprintf("%s is withheld from this session", name)) {
		return checks, data, passed
	}

	ok, detail := r.issue(ctx, p, &data)
	add(checkIssue, ok, detail)
	return checks, data, passed
}

// lifetimeAllowed is the second check: the request names one secret, it signed a lifetime, and
// that lifetime is within both bounds — the class's own 15 minutes and the seconds the rules
// give the judge for that secret.
func lifetimeAllowed(p PendingRequest, seconds uint32, judged bool) (bool, string) {
	switch {
	case len(p.Secrets) != 1:
		return false, fmt.Sprintf("the request names %d secrets, and the judge decides a request for one", len(p.Secrets))
	case p.LifetimeSeconds == nil:
		return false, "the request signed no lifetime, and the judge decides none that lasts as long as the session"
	case time.Duration(*p.LifetimeSeconds)*time.Second > maxJudgedLifetime:
		return false, fmt.Sprintf("the request asks for %d seconds, past the %s the judge decides", *p.LifetimeSeconds, maxJudgedLifetime)
	case !judged:
		return false, fmt.Sprintf("the airlock rules let the judge decide nothing for %s", p.Secrets[0])
	case int64(*p.LifetimeSeconds) > int64(seconds):
		return false, fmt.Sprintf("the request asks for %d seconds of %s, past the %d the rules let the judge decide", *p.LifetimeSeconds, p.Secrets[0], seconds)
	}
	return true, ""
}

// issue is the fifth check, and the only one that reads anything outside the broker: the signed
// reason names a Dispatch issue key, and Dispatch shows that issue at a status that is not done.
// What it read goes into data, so the model sees the issue the check passed on; a read that
// failed still leaves the key there, which is what the negative control shows the model.
func (r *Runner) issue(ctx context.Context, p PendingRequest, data *Data) (bool, string) {
	key := record.IssueKeyInReason(p.Reason)
	if key == "" {
		return false, "the request's signed reason names no Dispatch issue"
	}
	data.Issue.Key = key
	issue, err := r.opts.Dispatch.Issue(ctx, key)
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
	// applied is set on an approve verdict alone: whether the approval landed. notApplied
	// is why it did not.
	applied    *bool
	notApplied error
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
	if v.applied != nil {
		detail["applied"] = *v.applied
		if v.notApplied != nil {
			detail["not_applied"] = sanitize(v.notApplied.Error(), maxRawAnswer)
		}
	}
	return detail
}

// write is the one place a run writes anything. An approve verdict goes through
// ApplyJudgeDecision, which commits the approval and the row together; every other verdict, and
// an approval the state machine refused, writes the row alone.
func (r *Runner) write(ctx context.Context, p PendingRequest, row verdictRow) {
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
		row.applied = new(true)
		_, err := r.opts.Requests.ApplyJudgeDecision(ctx, p.RecordID, modelID, row.detail(modelID))
		if err == nil {
			r.logged(p, row)
			return
		}
		// A person decided the request first, or it expired, while the judge was
		// reading: the ordinary outcome of losing a race and no fault of the judge's
		// (D10). Any other refusal is one the judge should not have reached at all.
		// Either way the approval wrote nothing, and the verdict is still the record of
		// what the judge was asked and what it answered, so it is written on its own.
		row.applied, row.notApplied = new(false), err
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
func (r *Runner) logged(p PendingRequest, row verdictRow) {
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
	if row.applied != nil {
		attrs = append(attrs, "applied", *row.applied)
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
