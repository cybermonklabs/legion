// Package judge is the secrets broker's model judge: the background run that decides a pending
// request for keys whose tags let it decide — every key that needs a decision carries a judge tag
// (policy.TagJudge), and none an administrator's tag (requests.Machine.judgeDecides). A key the
// session gets at once may stand beside them; it needs no decision, so the judge decides nothing
// of it and its grant releases it with the rest. The broker holds such a request for the judge
// (requests.judge_until): it is on no person's list and no person may decide it while the run
// lasts. The judge grants it or refuses it, and a refusal is final; a run that cannot decide
// hands the request to its person, once.
//
// One run is one request. It makes six checks of its own before any model is asked — the judge
// tag of every key it decides, the request's lifetime against each of those keys' judge
// seconds, the tag policy re-read now (a key it decides must still need approval, and a key the
// session got at once must still need none), whether a key has become an administrator's, what
// the session has had withheld from it, and whether the request belongs to a private project —
// and stops at the first that fails. Two of them refuse the request outright, since no grant could
// follow: the policy now denies a key, or a key is withheld from the session. Every other failure
// hands the request over. The judge never grants a private project's request, since its own token
// names no person and may see no private project: the project check hands such a request to its
// person, and the grant asks again (requests.Machine.decisionFacts). The refusal's own write asks
// Dispatch nothing, so it stands whatever project the request belongs to; the checks that lead to
// a refusal read Dispatch as every check does. A run whose checks pass reads what the reason
// points at in Dispatch — the issue it names and the ask it cites — as data, and, if the machine
// still shows the request pending (Runner.stillPending), puts each key it decides to one model
// through packages/envoy/agentloop, in a call of its own (Runner.askEachKey): the call holds that
// key's guidance as the instruction to follow, no other key's, and everything an agent wrote as
// data to judge (D15). A request is granted only when every key's call grants; one that denies
// refuses it. Grant is written with the approval itself, in one transaction, and deny with the
// refusal itself; a model that gives no verdict for a key hands the request over.
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
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// runBudget is how long one run has, taken when the run starts (D9): the Dispatch phase, bounded
// at dispatchBound; then the ask read, made only when the reason cites an ask and bounded at a
// dispatchBound of its own; and the model calls, one for each key the run decides, made side by
// side and each bounded at modelBound (model.go), so they take one modelBound however many keys a
// request names; with room left over for everything else a run asks of the request machine.
const runBudget = 35 * time.Second

// dispatchBound is how long one read of Dispatch has. The Dispatch phase is one: every read of
// Dispatch before the ask — the policy check's (Requests.JudgePolicy reads what Dispatch holds of
// a tagged name), the project check's (Requests.JudgeProject) and both of Runner.issue's GETs —
// runs under one requests.JudgeDispatch, so they share one bound. It is
// requests.JudgeDispatchBound, the bound the decision's own reads of Dispatch run under too.
// Runner.ask's own GET gets a bound of its own, because it is a second read and not part of the
// first.
//
// The bounds sit together because they have to add up: two of 5 seconds and modelBound's 20 of
// the 35 runBudget gives leave the run 5 seconds after the worst case, a Dispatch phase and an ask
// read that each spend their whole bound and a model call that then times out. In those 5 seconds
// the run reads the request's state (JudgeRequestState), and an approval makes its reads of
// Dispatch before its transaction (ApplyJudgeDecision) under requests.JudgeDispatch, which ends
// them requests.JudgeRowReserve before the run does, then its transaction. The request.judged row
// is written on a context of its own (Runner.write), so neither a slow Dispatch at the decision
// nor the run's own deadline can leave a run that asked the model without its row: the one
// outcome the audit trail exists to prevent.
//
// The per-call bounds of the two clients are backstops under this and not the phase's bound:
// cmd/cipherd's judgeDispatchTimeout is http.Client.Timeout, which is per Do, and Issue makes two
// calls; the project-access client bounds each of its calls at 10 s, and a project read can make
// one per project. Either client alone would let the phase run far past the arithmetic above, so
// this deadline is the one that ends a stalled read of a run and the clients' are the backstops
// behind it.
const dispatchBound = requests.JudgeDispatchBound

// maxJudgedLifetime is the longest grant lifetime the judge decides, whatever a key's judge tag
// says: the bound of the class itself, which the policy load holds every judge tag to.
const maxJudgedLifetime = policy.MaxJudgeSeconds * time.Second

// notFound is the state an issue or an ask is shown to the model in when Dispatch holds none
// under the key or id the reason gave.
const notFound = "not found"

// The six checks a run makes before the model, in the order it makes them. These are the names
// a request.judged row calls them by, so a reader of the row and a reader of this package see
// one set of words.
//
// They read from the request outwards: what each key's tags give the judge, what the request
// itself asks for, what the policy answers for each key now, what each key is, what this session has had
// taken back, and the project each key belongs to. checkAdmin sits directly after checkPolicy
// because the two are one read of the machine per key (Requests.JudgePolicy): the policy's
// outcome and the key's admin tag come back together. Two of them refuse the request for good when
// they fail — the policy denying a key, and a key withheld from the session — since no decision of
// anyone's could grant it; the rest hand the request to its person, who may. checkProject comes
// last, before the reads of what the reason points at, so that a private project's request costs
// no Dispatch read of its issue or its ask: the judge leaves every such request to a person.
const (
	checkJudgeTag = "judge-tag"
	checkLifetime = "lifetime"
	checkPolicy   = "policy"
	checkAdmin    = "admin"
	checkWithheld = "withheld"
	checkProject  = "project"
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

// Requests is what a run asks of the broker's request machine: the three questions it must ask
// again before it decides, and the three writes a verdict makes. It is declared here, in the
// consumer, so that this package depends on no concrete machine; *requests.Machine answers all
// six, and requests imports nothing of this package.
type Requests interface {
	// JudgePolicy answers, for the enrollment that made a pending request, what the live
	// policy says about one of its names at this moment: the decision policy.Set.Evaluate
	// makes for that enrollment's requester, whether the name is withheld from the
	// session, and whether a person approves every release of the secret
	// (policy.Secret.PersonApprovesEveryRelease: true for a secret tagged admin: "true" and for an
	// anthropic-federation secret). The judge asks the machine one question instead of reading the
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

	// JudgeProject answers whether the request of enrollmentID for name, signed with reason, is a
	// private project's, which the judge must leave to a person: the secret's dispatch-project tag
	// (policy.TagProject) names a private project or a key Dispatch holds no project for, or the
	// credential that asked names a private project (a session's list, or the project a pod's
	// operator entry maps it to), or any issue key the reason names is a private project's. Those
	// are the private projects the person rule reads too (requests.Machine.HideUnseen), so the judge
	// decides no request that rule hides from a full member. The rule is about private projects
	// alone: a restricted member, who sees only the projects that list them, may still be unable to
	// see a public project's request the judge decided. A request whose secret carries no tag,
	// whose credential names no project and whose reason names no issue key answers false with no
	// call to Dispatch, and a broker with no project-access client answers false for an untagged
	// secret. The judge's own token names no person, so it may see no private project. ctx is the
	// run's Dispatch phase, which dispatchBound bounds.
	JudgeProject(ctx context.Context, enrollmentID, name, reason string) (private bool, err error)

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
	// no verdict, and an approval the state machine then refused or could not make. It answers
	// what it did: for a row saying an approval was not applied, nothing at all when that
	// approval's own row is there (its COMMIT applied and only the answer was lost), and applied
	// requests.AppliedUnknown when that could not be read. That last row ends no hold: the
	// approval it could not read may still hold the request's row, so the hold runs out by
	// itself (requests.JudgeHold) and only then is the request its person's. For a row it wrote,
	// it answers the verdict it wrote, which it reads off the request's state as it writes.
	LogJudged(ctx context.Context, recordID, modelID string, judged map[string]any) (requests.JudgedLog, error)

	// DenyJudged ends a request the judge holds as the judge's refusal: the denial, why as
	// its detail for the agent to read, and the request.judged row, in one transaction. It
	// refuses a request the judge does not hold (requests.ErrJudgeHoldEnded).
	DenyJudged(ctx context.Context, recordID, modelID string, judged map[string]any, why string) (requests.Decision, error)
}

// Options are what a Runner judges with.
type Options struct {
	// Policy is the broker's live policy, whose tags say which keys the judge may decide at all,
	// for how long, and on what guidance (policy.Set.JudgeSeconds, policy.Secret.Guidance). A run
	// reads it once, before the model (beforeTheModel), so the judge-tag and lifetime checks and
	// the data the model is shown are of one snapshot.
	Policy *policy.Current
	// Dispatch is the read of what a request's reason points at, and Model the one model the
	// run asks.
	Dispatch Dispatch
	Model    Model
	// Requests is the machine a verdict is written through.
	Requests Requests
	// SkipChecks turns the six checks off as a bar: each is still made and still recorded,
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
	// dispatchPhase makes the run's Dispatch phase: requests.JudgeDispatch, which only this
	// package's tests replace (holdPhase).
	dispatchPhase func(context.Context) (context.Context, context.CancelFunc)
}

// New is the runner for one broker. ctx is that broker's serving lifetime.
func New(ctx context.Context, o Options) *Runner {
	return &Runner{ctx: ctx, opts: o, dispatchPhase: requests.JudgeDispatch}
}

// Consider starts one run for one pending request, on a goroutine of its own with runBudget off
// the runner's context, and returns at once: the request that created p is already answered, and
// nothing a run does belongs on its path.
//
// There is no bound on the runs in flight and no queue. A queue would leave a legitimate request
// waiting while another request's model call ran, which is the wait the judge exists to take
// away, and no load the broker has met needs one: a run is its Dispatch reads under two bounds,
// one model call for each key it decides, made side by side, and one short transaction.
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
			row.calls = r.askEachKey(ctx, data)
			row.verdict, row.usage, row.raw, row.err = combine(row.calls)
		}
	}
	r.write(ctx, p, row)
}

// What one key's call did, as the row's calls name it: the model granted the key, denied it, or
// gave no verdict (a fault, a time-out, an answer outside the two words), which is no deny.
const (
	callGranted = "granted"
	callDenied  = "denied"
	callFailed  = "failed"
)

// keyCall is one model call of a run: the one key it decided, and what the model answered for it.
type keyCall struct {
	key     string
	verdict Verdict
	usage   agentloop.Usage
	raw     string
	err     error
}

func (c keyCall) did() string {
	switch {
	case c.err != nil:
		return callFailed
	case c.verdict.Word == Grant:
		return callGranted
	default:
		return callDenied
	}
}

// askEachKey decides each key of data in a model call of its own, all at once, each shown that
// one key, its seconds and its own owner's guidance beside the request's reason, issue and ask, and
// nothing of any other key. One owner's guidance is an instruction only for that owner's key: a
// guidance that tries to steer another key cannot reach that key's call, which never holds it.
// The calls run side by side, each under modelBound, so a request of N judge keys makes N calls
// and waits as long as the slowest of them, not their sum. The answers come back in data's key
// order.
func (r *Runner) askEachKey(ctx context.Context, data Data) []keyCall {
	calls := make([]keyCall, len(data.Keys))
	var wg sync.WaitGroup
	for i, key := range data.Keys {
		one := data
		one.Keys = []KeyData{key}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := keyCall{key: key.Name}
			c.verdict, c.usage, c.raw, c.err = r.opts.Model.Ask(ctx, one)
			calls[i] = c
		}()
	}
	wg.Wait()
	return calls
}

// combine is the run's verdict from its calls: grant only when every call granted; deny, final,
// when any call denied, with the first denying key's reason; and otherwise no verdict, handed to
// the request's person, when a call failed to give one. A deny beats a failed call: a key's owner
// said no, and asking the person would only ask past that. Usage is the sum of every call's. Where
// the run decided several keys, each reason and error it carries names its key; one key's reason
// is the model's own words, as it always was.
func combine(calls []keyCall) (Verdict, agentloop.Usage, string, error) {
	var usage agentloop.Usage
	for _, c := range calls {
		usage = agentloop.SumUsage(usage, c.usage)
	}
	if len(calls) == 0 {
		return Verdict{Word: Deny}, usage, "", errors.New("the request named no key for the model to decide")
	}
	said := func(c keyCall) string {
		if len(calls) == 1 {
			return c.verdict.Reason
		}
		return c.key + ": " + c.verdict.Reason
	}
	for _, c := range calls {
		if c.err == nil && c.verdict.Word == Deny {
			return Verdict{Word: Deny, Reason: record.Sanitize(said(c), maxReasonRunes)}, usage, "", nil
		}
	}
	for _, c := range calls {
		if c.err != nil {
			if len(calls) == 1 {
				return Verdict{Word: Deny}, usage, c.raw, c.err
			}
			return Verdict{Word: Deny}, usage, c.raw, fmt.Errorf("the call deciding %s: %w", c.key, c.err)
		}
	}
	reasons := make([]string, 0, len(calls))
	for _, c := range calls {
		reasons = append(reasons, said(c))
	}
	return Verdict{Word: Grant, Reason: record.Sanitize(strings.Join(reasons, "; "), maxReasonRunes)}, usage, "", nil
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
// request of its own, live for the milliseconds this run spends before the model calls, so a
// session that loops create, cancel, create pays for one model call per judge key per round. D8
// decides not to cap the runs in flight or limit a session's creates.
//
// A run stopped here still writes its row. No request row is ever deleted — a cancel and an
// expiry are state changes — so LogJudged finds the request and the enrollment it hangs off,
// and the trail says why a run that cost a Dispatch read decided nothing.
//
// It is a gate and not an entry in the checks array. The checks answer one question — may the
// judge decide this request? — and a reader of a row reads them as that one list, which the
// verification document's check k10 and scripts/judge-check.sh pin by name and in order. This
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

// beforeTheModel makes the six checks, in order, then the reads of what the reason points at,
// and builds the data the model would be shown. It stops at the first failure, so a request whose
// keys' tags do not let the judge decide it costs no Dispatch read and no model call. Under
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

	// The judge's own two checks and the data are read off one snapshot of the policy, so the
	// seconds a check held the request to and the guidance the model is shown are the same tags.
	// The model is shown the keys the judge decides and nothing of a key the session gets at
	// once beside them, which no guidance is about.
	set := r.opts.Policy.Get()
	data := Data{Reason: p.Reason}
	data.Keys = slices.Grow(data.Keys, len(p.Decided))
	for _, name := range p.Decided {
		seconds, _ := set.JudgeSeconds(name)
		data.Keys = append(data.Keys, KeyData{Name: name, JudgeSeconds: seconds, Guidance: set.Secrets[name].Guidance})
	}

	if ok, detail := judgeTags(set, p); !add(checkJudgeTag, ok, detail, false) {
		return checks, data
	}
	if ok, detail := lifetimeAllowed(set, p); !add(checkLifetime, ok, detail, false) {
		return checks, data
	}

	// The Dispatch phase: from here to the issue read, every read of Dispatch — the policy
	// check's, the project check's and the issue's two GETs — runs under one
	// requests.JudgeDispatch, so however slowly Dispatch answers, the ask read, the model call and
	// the row's write keep the parts of runBudget the arithmetic gives them.
	phase, cancel := r.dispatchPhase(ctx)
	defer cancel()

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
		now, withheld, admin, err := r.opts.Requests.JudgePolicy(phase, p.EnrollmentID, name)
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

	// The project check, for every key of the request, since the grant releases them all. A
	// private project's request is its person's: the judge's own token names no person, so it
	// may see no private project. A failed read hands the request over too; neither is a
	// refusal, since a person may decide it.
	projectOK, projectDetail := true, ""
	for _, name := range p.Secrets {
		private, err := r.opts.Requests.JudgeProject(phase, p.EnrollmentID, name, p.Reason)
		switch {
		case err != nil:
			projectOK, projectDetail = false, fmt.Sprintf("whether the request for %s belongs to a private project (by the secret, the credential that asked, or an issue its reason names) could not be read: %v", name, err)
		case private:
			projectOK, projectDetail = false, fmt.Sprintf("the request for %s belongs to a private project (by the secret, the credential that asked, or an issue its reason names), or the secret to one Dispatch does not hold, which a person decides", name)
		}
		if !projectOK {
			break
		}
	}
	if !add(checkProject, projectOK, projectDetail, false) {
		return checks, data
	}

	// What the reason points at, read as data. A key or an id Dispatch holds nothing under is
	// shown to the model as that; a read that failed hands the request to its person, since
	// the guidance may need what the read would have shown. The issue is read on the phase; the
	// ask has a bound of its own.
	if ok, detail := r.issue(phase, p, &data); !ok {
		add(checkDispatch, false, detail, false)
		return checks, data
	}
	if ok, detail := r.ask(ctx, p, &data); !ok {
		add(checkDispatch, false, detail, false)
	}
	return checks, data
}

// judgeTags is the first check: every key the judge decides carries a judge tag in the policy the
// broker holds now. Create held the request for the judge on the policy it read then; a retag
// since may have taken the tag away, and such a request is its person's to decide. A key the
// session gets at once needs no decision, so no tag of it is asked for.
func judgeTags(set *policy.Set, p requests.PendingRequest) (bool, string) {
	if len(p.Decided) == 0 {
		return false, "the request names no key that needs a decision"
	}
	for _, name := range p.Decided {
		if _, judged := set.JudgeSeconds(name); !judged {
			return false, decidesNothingFor(name)
		}
	}
	return true, ""
}

// lifetimeAllowed is the second check: the grant has a lifetime — the one the request signed,
// else the broker's default, which for a judge key is its judge seconds — within the class's 15
// minutes and within the judge seconds of every key the judge decides. Create refuses a request
// that asks for longer, so a failure here is a judge tag that moved under the request.
func lifetimeAllowed(set *policy.Set, p requests.PendingRequest) (bool, string) {
	switch {
	case p.LifetimeSeconds == nil:
		return false, "the request signed no lifetime, and the judge decides none that lasts as long as the session"
	case time.Duration(*p.LifetimeSeconds)*time.Second > maxJudgedLifetime:
		return false, fmt.Sprintf("the request asks for %d seconds, past the %s the judge decides", *p.LifetimeSeconds, maxJudgedLifetime)
	}
	for _, name := range p.Decided {
		seconds, judged := set.JudgeSeconds(name)
		if !judged {
			return false, decidesNothingFor(name)
		}
		if !record.JudgeAllowsLifetime(seconds, *p.LifetimeSeconds) {
			return false, fmt.Sprintf("the request asks for %d seconds of %s, past the %d its judge tag lets the judge decide", *p.LifetimeSeconds, name, seconds)
		}
	}
	return true, ""
}

// decidesNothingFor is the sentence the first two checks write for a key whose tags give the
// judge nothing of it.
func decidesNothingFor(name string) string {
	return fmt.Sprintf("%s carries no judge tag, so the judge decides nothing of it", name)
}

// decidedByAPerson is the sentence the admin check writes, and the one a person reads in the
// request.judged row of a request the judge handed over for naming a secret a person approves
// every release of (policy.Secret.PersonApprovesEveryRelease). It says both halves of why: what
// the key is, and that the promise is unconditional — every release of such a credential is
// approved by a person, and no judge tag on the key can undo that
// (policy.TagAdmin, the broker's concepts document).
func decidedByAPerson(name string) string {
	return fmt.Sprintf("%s is an administrator's secret or an Anthropic federation secret, and a person approves every release of one", name)
}

// issue reads the Dispatch issue the signed reason names, when it names one, into data, so the
// model can read it against the keys' guidance. A key Dispatch holds no issue for is shown as not
// found; a read that failed answers false, and the run hands the request over.
//
// ctx is the Dispatch phase's, which the policy and project checks' reads have already spent part
// of: its two GETs share what is left of one dispatchBound, so the phase costs the run what
// runBudget gave it however the reads go.
func (r *Runner) issue(ctx context.Context, p requests.PendingRequest, data *Data) (bool, string) {
	key := record.IssueKeyInReason(p.Reason)
	if key == "" {
		return true, ""
	}
	issue, err := r.opts.Dispatch.Issue(ctx, key)
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
	asked bool
	// calls is each key's own model call, in the request's key order (askEachKey); verdict,
	// usage, raw and err are what combine made of them.
	calls   []keyCall
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
	// appliedUnknown is a row LogJudged wrote with applied requests.AppliedUnknown: the
	// approval's answer was lost, and so was the read of whether it applied.
	appliedUnknown bool
	// written is the verdict LogJudged answered it wrote, for the log line; empty for every run
	// whose row LogJudged did not write.
	written string
	// decided is the state that transaction left the request in — "granted", or "denied"
	// where the approval committed and granted nothing, or where the judge refused it — for
	// the log line alone, and is empty for every run that made no decision.
	decided string
	// refusal is what a refused request's agent was told (refusalOf), for the log line; empty
	// for every run that refused nothing.
	refusal string
}

// detail is the row's detail object, the one place this package spells its keys. It carries no
// verdict: the row's verdict is what became of the request, which the row's own transaction reads
// and sets (requests' decider.judgedRow for a decision, LogJudged's read of the request for every
// other row), never the model's answer, which is each call's.
func (v verdictRow) detail(modelID string) map[string]any {
	detail := map[string]any{
		"checks": v.checks,
		"model":  modelID,
		"asked":  v.asked,
		"reason": v.verdict.Reason,
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
		// Each key's own call: the key, what the call did (callGranted, callDenied, or
		// callFailed with its error where the model gave no verdict, which is no deny), its
		// reason, so the row says which key's owner's guidance decided, and the raw answer of
		// a call whose answer did not parse, whatever the other calls did.
		calls := make([]map[string]any, 0, len(v.calls))
		for _, c := range v.calls {
			call := map[string]any{"key": c.key, "verdict": c.did(), "reason": c.verdict.Reason}
			if c.err != nil {
				call["error"] = record.Sanitize(c.err.Error(), maxRawAnswer)
			}
			if c.raw != "" {
				call["raw_answer"] = c.raw
			}
			calls = append(calls, call)
		}
		detail["calls"] = calls
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
		// An approval the state machine refused before its COMMIT wrote nothing, so applied is
		// false. One whose COMMIT failed may have applied, so applied is unknown, and
		// requests.Machine.LogJudged reads what that COMMIT left before it writes the row.
		detail["applied"] = false
		if errors.Is(v.notApplied, requests.ErrCommitUncertain) {
			detail["applied"] = requests.AppliedUnknown
		}
		detail["not_applied"] = record.Sanitize(v.notApplied.Error(), maxRawAnswer)
	}
	return detail
}

// write is the one place a run writes anything, and it chooses between the three ends a request
// the judge holds can come to. A grant verdict goes through ApplyJudgeDecision, which commits the
// approval and the row together. A deny verdict, and a check that fails finally, go through
// DenyJudged, which refuses the request for good. Everything else — a check that fails other than
// finally, a Dispatch read that failed, a model that gave no verdict, an approval the machine
// then refused or could not make, and a request that is no longer pending — writes the row
// through LogJudged, which hands the request to its person. That row is written on a context of
// its own (rowContext), so the run's deadline cannot cancel the one write that records what the
// judge was asked. What a failed approval's line says is decided once that row is written, since
// only LogJudged can tell an approval that did not apply from one whose COMMIT applied and whose
// answer was lost.
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
		// current policy moved while the judge was reading; notApplied says which once the row
		// below is written. Either way the approval wrote nothing it is known to have written;
		// the row hands a request still pending to its person, and keeps the verdict as the
		// record of what was asked.
		row.notApplied = err
	case row.asked:
		if r.refuse(ctx, p, row) {
			return
		}
	}
	rowCtx, cancel := rowContext(ctx)
	defer cancel()
	wrote, err := r.opts.Requests.LogJudged(rowCtx, p.RecordID, modelID, row.detail(modelID))
	if err != nil {
		attrs := []any{"record", p.RecordID, "request", p.RequestID, "model", modelID, "err", err}
		if row.notApplied != nil {
			attrs = append(attrs, "not_applied", row.notApplied)
		}
		slog.Error("broker judge: the verdict could not be written", attrs...)
		return
	}
	if wrote.Write == requests.JudgedAlreadyApplied {
		// The approval's own row is there, so its transaction committed: it may have granted,
		// or recorded the request's own denial, and that row says which.
		slog.Info("broker judge: the judge's approval committed, and only its answer was lost; its own row records what it did, so no second row was written",
			"record", p.RecordID, "request", p.RequestID, "model", modelID, "err", row.notApplied)
		return
	}
	if row.notApplied != nil {
		r.notApplied(p, modelID, row.notApplied, wrote.Write)
	}
	row.appliedUnknown = wrote.Write == requests.JudgedAppliedUnknown
	row.written = wrote.Verdict
	r.logged(p, row)
}

// notApplied is the line for an approval that wrote nothing, err being what it answered and wrote
// what LogJudged then did with the verdict's row, which is written by then. A person decided the
// request first, it expired, the judge's hold ended under the run, or the current policy moved
// while the judge was reading: the session's operator withheld a key or its owner tagged it an
// administrator's (the two guards currentPolicyAdmits makes for the judge alone), the key's owner
// changed, its max-grant fell below the request's lifetime, or the request became a private
// project's, all of which answer record.ErrNotApprover; a profiles roll took the request's target
// out of its profile, which the approval's own target check answers requests.ErrTargetRefused; or
// the key's owner lowered its judge tag below the request's lifetime or took it off, which the
// decision's live read of the tags answers record.JudgeTagRefusal. Each is the ordinary outcome of
// losing a race and no fault of the judge's (D10). Only that typed refusal is one: a record the
// judge may never decide whatever its tags say, also record.ErrNotJudgeable, is a judge that
// reached a request it should not have. The broker stopping under the approval
// (context.Canceled in what the approval answered), a COMMIT that failed and, by the row
// LogJudged read, did not apply (requests.ErrCommitUncertain), Dispatch not answering the
// approval's check, Dispatch not saying what kind of member the session's person is
// (*projectaccess.MemberKindUncheckable), and the run's time running out before the approval
// landed are no refusal either. Any other refusal is one the judge should not have reached at all,
// and is an ERROR even while the broker stops.
func (r *Runner) notApplied(p requests.PendingRequest, modelID string, err error, wrote requests.JudgedWrite) {
	attrs := []any{"record", p.RecordID, "request", p.RequestID, "model", modelID, "err", err}
	switch {
	case wrote == requests.JudgedAppliedUnknown:
		slog.Warn("broker judge: whether the judge's approval applied could not be read: its COMMIT failed, and so did the read of its own row; the row it wrote says applied unknown",
			attrs...)
	case errors.Is(err, requests.ErrTerminal) || errors.Is(err, requests.ErrExpired) ||
		errors.Is(err, requests.ErrJudgeHoldEnded) || errors.Is(err, record.ErrNotApprover) ||
		errors.Is(err, requests.ErrTargetRefused) || errors.As(err, new(record.JudgeTagRefusal)):
		slog.Info("broker judge: the request was decided, expired, handed over or moved out of the judge's policy before the judge's grant landed",
			attrs...)
	case errors.Is(err, context.Canceled):
		slog.Info("broker judge: the broker stopped while the judge's approval was being written; the approval was not applied, and the request waits for its person",
			attrs...)
	case errors.Is(err, requests.ErrCommitUncertain):
		slog.Warn("broker judge: the judge's approval's COMMIT failed, and its own row shows it did not apply; the request waits for its person",
			attrs...)
	case errors.Is(err, requests.ErrProjectAccessUnavailable) || errors.Is(err, context.DeadlineExceeded):
		slog.Warn("broker judge: the judge's approval was not applied: Dispatch could not be asked whether the request is still the judge's, or the run's time ran out; the request waits for its person",
			attrs...)
	case errors.As(err, new(*projectaccess.MemberKindUncheckable)):
		slog.Warn("broker judge: the judge's approval was not applied: Dispatch could not say whether the session's person is a restricted member; the request waits for its person",
			attrs...)
	default:
		slog.Error("broker judge: the judge's grant was refused", attrs...)
	}
}

// rowContext is the context a run's request.judged row is written on when nothing else wrote it:
// ctx's values without its cancellation, so the run's deadline cannot cancel the write, and a
// deadline of its own, the later of ctx's and requests.JudgeRowReserve from now, so a write that
// cannot commit still ends.
func rowContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(requests.JudgeRowReserve)
	if end, ok := ctx.Deadline(); ok && end.After(deadline) {
		deadline = end
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
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
// same strings the row holds and GET /v1/grant-log then serves. Its verdict is the row's: what
// became of the request (outcome), never a deny the judge did not make; calls is each key's call
// as key:what-it-did.
func (r *Runner) logged(p requests.PendingRequest, row verdictRow) {
	attrs := []any{
		"record", p.RecordID, "request", p.RequestID, "session", p.SessionID,
		"approver", p.Approver, "secrets", p.Secrets, "model", r.opts.Model.ID,
		"asked", row.asked, "verdict", row.outcome(), "reason", row.verdict.Reason,
	}
	if len(row.calls) > 0 {
		calls := make([]string, 0, len(row.calls))
		for _, c := range row.calls {
			calls = append(calls, c.key+":"+c.did())
		}
		attrs = append(attrs, "calls", strings.Join(calls, ","))
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
	// false where the machine refused the attempt outright, and unknown where the attempt's
	// answer was lost and so was the read of whether it applied. A refusal is decided denied,
	// and carries what its agent was told. handed_over is the row's own word: a row LogJudged
	// wrote saying handed to the person.
	handedOver := row.written == requests.JudgedHandedOver
	switch {
	case row.refusal != "":
		attrs = append(attrs, "applied", false, "decided", row.decided, "refusal", row.refusal)
	case row.decided != "":
		attrs = append(attrs, "applied", row.decided == "granted", "decided", row.decided)
	case row.appliedUnknown:
		attrs = append(attrs, "applied", requests.AppliedUnknown)
	case row.notApplied != nil && handedOver:
		attrs = append(attrs, "applied", false, "handed_over", true)
	case row.notApplied != nil:
		attrs = append(attrs, "applied", false)
	case handedOver:
		attrs = append(attrs, "handed_over", true)
	}
	slog.Info("broker judge: wrote a verdict", attrs...)
}

// outcome is what became of the request, as the row this run wrote says it: for a row the
// decision's own transaction wrote, the state that transaction left the request in, mapped as
// decider.judgedRow maps the same state (requests.JudgedGranted, else JudgedRefused); for every
// other row, the verdict LogJudged answered it wrote (written).
func (v verdictRow) outcome() string {
	switch {
	case v.decided == "granted":
		return requests.JudgedGranted
	case v.decided != "":
		return requests.JudgedRefused
	default:
		return v.written
	}
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
