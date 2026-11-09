// packages/envoy/internal/broker/judge/judge_test.go
// What a run leaves behind: which requests never reach the model, what an approve and a refer
// write, what a failure of Dispatch or of the model endpoint leaves, and what the judged row
// holds. Every run here goes through stand-in HTTP servers and a fake request machine.
package judge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestACheckThatFailsLeavesTheRequestPendingAndAsksTheModelNothing is each of the six checks in
// turn: the request is left pending, one request.judged row names the check that failed, and the
// model is never asked. The checks before the issue read stop the run before Dispatch is called
// at all, which is what keeps a request the rules do not allow from costing a round trip.
func TestACheckThatFailsLeavesTheRequestPendingAndAsksTheModelNothing(t *testing.T) {
	for _, c := range []struct {
		name          string
		want          string
		wantDetail    string
		readsDispatch bool
		request       func(*requests.PendingRequest)
		machine       func(*fakeRequests)
		dispatch      func(*dispatchStandIn)
	}{
		{
			name: "a secret whose entry has no judge", want: checkRulesJudge,
			request: func(p *requests.PendingRequest) { p.Secrets = []string{"NO_JUDGE_KEY"} },
		},
		{
			name: "two secrets", want: checkRulesJudge,
			request: func(p *requests.PendingRequest) { p.Secrets = []string{testSecret, "NO_JUDGE_KEY"} },
		},
		{
			name: "a lifetime past the fifteen minutes of the class", want: checkLifetime,
			wantDetail: "past the 15m0s the judge decides",
			request:    func(p *requests.PendingRequest) { p.LifetimeSeconds = new(3600) },
		},
		{
			// The one request this check holds back that the class's own bound would
			// let through: ten minutes is well inside the 15 the judge ever decides,
			// and past the five this name's own entry gives it. A judge that read only
			// the class would decide it, which is a grant longer than the rules file
			// says anyone may have of this secret without a person.
			name: "a lifetime inside the class but past the entry's own judge", want: checkLifetime,
			wantDetail: "past the 300 the rules let the judge decide",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.LifetimeSeconds = []string{shortJudgeSecret}, new(600)
			},
		},
		{
			name: "no signed lifetime at all", want: checkLifetime,
			wantDetail: "signed no lifetime",
			request:    func(p *requests.PendingRequest) { p.LifetimeSeconds = nil },
		},
		{
			name: "the tag policy refusing the name now", want: checkPolicy,
			machine: func(f *fakeRequests) {
				f.decision = policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonRequesterNotListed}
			},
		},
		{
			// An administrator's secret: the tag promises that a person approves
			// every release of one, and the judge is no person. The promise holds
			// whatever seconds a rules file gives the judge for the name, which is
			// why this request reaches the admin check at all — its entry has a
			// judge, its lifetime is inside the bound and the policy approves it.
			name: "an administrator's secret", want: checkAdmin,
			wantDetail: "is an administrator's secret, and a person approves every release of one",
			machine:    func(f *fakeRequests) { f.admin = true },
		},
		{
			name: "a name withheld from the session", want: checkWithheld,
			machine: func(f *fakeRequests) { f.withheld = true },
		},
		{
			name: "a reason naming no issue", want: checkIssue,
			request: func(p *requests.PendingRequest) { p.Reason = "reading the broker's own tests" },
		},
		{
			name: "an issue Dispatch does not hold", want: checkIssue, readsDispatch: true,
			dispatch: func(d *dispatchStandIn) { d.issueStatus = http.StatusNotFound },
		},
		{
			name: "an issue that is done", want: checkIssue, readsDispatch: true,
			dispatch: func(d *dispatchStandIn) { d.issue["status"] = "done" },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
			request := pending()
			if c.request != nil {
				c.request(&request)
			}
			if c.machine != nil {
				c.machine(machine)
			}
			if c.dispatch != nil {
				c.dispatch(dispatch)
			}
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), request)

			row := theOneRow(t, machine)
			if got := failedCheck(t, row); got != c.want {
				t.Errorf("the row names %q as the check that failed; want %q", got, c.want)
			}
			if detail := failedDetail(t, row); c.wantDetail != "" && !strings.Contains(detail, c.wantDetail) {
				t.Errorf("the failed check reads %q; want it to say %q, the bound it was held to", detail, c.wantDetail)
			}
			if row["verdict"] != Refer || row["asked"] != false {
				t.Errorf("the row reads verdict %v, asked %v; want a referral the model was never asked for", row["verdict"], row["asked"])
			}
			if model.asked() != 0 {
				t.Errorf("the model got %d requests; want none, since a check stopped the run", model.asked())
			}
			if called := len(dispatch.got()) > 0; called != c.readsDispatch {
				t.Errorf("Dispatch got %d requests; want the run to read it: %v", len(dispatch.got()), c.readsDispatch)
			}
		})
	}
}

// TestApproveAppliesTheDecisionAndReferWritesTheRowAlone is the two verdicts a model may answer:
// approve is written with the approval, in the one transaction ApplyJudgeDecision makes, and
// refer writes its row and nothing else, which leaves the request for its person.
func TestApproveAppliesTheDecisionAndReferWritesTheRowAlone(t *testing.T) {
	for _, c := range []struct{ verdict, reason string }{
		{Approve, "The reason names INFRA-1353 and the issue is the broker's judge."},
		{Refer, "The reason says nothing about this secret."},
	} {
		t.Run(c.verdict, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(c.verdict, c.reason)), newRequests()
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			applied, logged := machine.rows()
			rows := append(applied, logged...)
			if len(rows) != 1 {
				t.Fatalf("the run wrote %d decisions and %d rows; want exactly one write", len(applied), len(logged))
			}
			if c.verdict == Approve && len(applied) != 1 || c.verdict == Refer && len(logged) != 1 {
				t.Fatalf("a %s verdict applied %d decisions and logged %d rows", c.verdict, len(applied), len(logged))
			}
			row := rows[0]
			if row["verdict"] != c.verdict || row["reason"] != c.reason || row["asked"] != true {
				t.Errorf("the row reads %v / %v / asked %v; want the model's own verdict and reason", row["verdict"], row["reason"], row["asked"])
			}
			if failed := failedCheck(t, row); failed != "" {
				t.Errorf("the row names %q as a failed check; want every check passed", failed)
			}
			if row["priced"] != false {
				t.Errorf("the row reads priced %v; want false, since the judge sets no price", row["priced"])
			}
			tokens, ok := row["tokens"].(map[string]any)
			if !ok || tokens["input"] != 1200 || tokens["output"] != 48 {
				t.Errorf("the row's tokens are %v; want the counts the model reported", row["tokens"])
			}
			if model.asked() != 1 {
				t.Errorf("the model got %d requests; want exactly one", model.asked())
			}
		})
	}
}

// TestAJudgeFailureLeavesTheRequestForItsPerson: Dispatch failing, Dispatch refusing the
// broker's token, and the model endpoint failing each end the run as a referral with a row, and
// never as an approval. A judge that cannot read what it needs leaves the request where it was.
func TestAJudgeFailureLeavesTheRequestForItsPerson(t *testing.T) {
	for _, c := range []struct {
		name  string
		setUp func(*dispatchStandIn, *modelStandIn)
		asked bool
	}{
		{
			name:  "Dispatch answering 500",
			setUp: func(d *dispatchStandIn, _ *modelStandIn) { d.issueStatus = http.StatusInternalServerError },
		},
		{
			name:  "Dispatch answering 401",
			setUp: func(d *dispatchStandIn, _ *modelStandIn) { d.issueStatus = http.StatusUnauthorized },
		},
		{
			name:  "the model endpoint answering 503",
			setUp: func(_ *dispatchStandIn, m *modelStandIn) { m.status = http.StatusServiceUnavailable },
			asked: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
			c.setUp(dispatch, model)
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			row := theOneRow(t, machine)
			if row["verdict"] != Refer {
				t.Errorf("the row reads verdict %v; want a referral", row["verdict"])
			}
			if row["asked"] != c.asked {
				t.Errorf("the row reads asked %v; want %v", row["asked"], c.asked)
			}
			if c.asked {
				if _, ok := row["error"].(string); !ok {
					t.Errorf("the row carries error %v; want the model failure's own words", row["error"])
				}
			} else if failed := failedCheck(t, row); failed != checkIssue {
				t.Errorf("the row names %q as the check that failed; want the issue read", failed)
			}
		})
	}
}

// TestTheModelsWordsAreClampedAndFiltered: an answer of 50 kB is stored as 2,000 runes of raw
// answer, and a reason carrying control and zero-width characters is stored without them. Both
// strings reach the audit row and the broker's log, so neither may carry what a reason of a
// request's own may not (D12).
func TestTheModelsWordsAreClampedAndFiltered(t *testing.T) {
	t.Run("an answer of 50 kB", func(t *testing.T) {
		machine := runAgainstTheModel(t, strings.Repeat("not an object at all. ", 50_000/22))

		row := theOneRow(t, machine)
		raw, ok := row["raw_answer"].(string)
		if !ok {
			t.Fatalf("the row carries raw_answer %v; want the answer that did not parse", row["raw_answer"])
		}
		if runes := len([]rune(raw)); runes != maxRawAnswer {
			t.Errorf("the raw answer is %d runes; want it clamped to %d", runes, maxRawAnswer)
		}
		if row["verdict"] != Refer {
			t.Errorf("the row reads verdict %v; want an answer that did not parse to be a referral", row["verdict"])
		}
	})

	t.Run("a reason carrying control characters", func(t *testing.T) {
		machine := runAgainstTheModel(t, verdictReply(Approve, "it \u0000is\u200b fine\u2028now\nthen"))

		applied, _ := machine.rows()
		if len(applied) != 1 {
			t.Fatalf("the run applied %d decisions; want one", len(applied))
		}
		if reason := applied[0]["reason"]; reason != "it is finenowthen" {
			t.Errorf("the row's reason is %q; want the refused runes dropped and the rest kept", reason)
		}
	})
}

// TestARawAnswerAndAReasonCarryNoRefusedRune pins the filter itself over the two bounds, as the
// row and the log line need it: every rune record.validateReason refuses is gone, and what is
// left is no longer than the bound.
func TestARawAnswerAndAReasonCarryNoRefusedRune(t *testing.T) {
	refused := "\u0000\u0001\u200b\u202e\u2028\u2029\ufeff\n\r\t"
	for _, c := range []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"a reason of refused runes alone", refused, maxReasonRunes, ""},
		{"a reason around them", "a" + refused + "b", maxReasonRunes, "ab"},
		{"a reason at its bound", strings.Repeat("é", 500), maxReasonRunes, strings.Repeat("é", maxReasonRunes)},
		{"a raw answer at its bound", strings.Repeat("x", 50_000), maxRawAnswer, strings.Repeat("x", maxRawAnswer)},
		{"a string inside both bounds", "plain words", maxReasonRunes, "plain words"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := sanitize(c.in, c.limit)
			if got != c.want {
				t.Errorf("sanitize(%q, %d) = %q; want %q", c.in, c.limit, got, c.want)
			}
			for _, r := range got {
				if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp) {
					t.Errorf("sanitize kept %q, a rune a reason may not carry", r)
				}
			}
		})
	}
}

// TestAnApprovalTheRequestMachineRefusedStillRecordsItsVerdict: a person decided the request
// while the judge was reading, so ApplyJudgeDecision answers ErrTerminal and writes nothing. The
// verdict is still worth keeping, so it is written on its own, saying the approval did not land
// (D10, D11).
func TestAnApprovalTheRequestMachineRefusedStillRecordsItsVerdict(t *testing.T) {
	for _, refusal := range []error{requests.ErrTerminal, requests.ErrExpired} {
		t.Run(refusal.Error(), func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
			machine.applyErr = refusal
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			applied, logged := machine.rows()
			if len(applied) != 1 || len(logged) != 1 {
				t.Fatalf("the run applied %d decisions and logged %d rows; want one attempt and one row", len(applied), len(logged))
			}
			if _, carries := applied[0]["applied"]; carries {
				t.Errorf("the attempt's row reads applied %v; want the key left to the transaction, which is the only thing that knows what it did", applied[0]["applied"])
			}
			row := logged[0]
			if row["verdict"] != Approve || row["applied"] != false {
				t.Errorf("the row reads verdict %v, applied %v; want the approval recorded as one that did not land", row["verdict"], row["applied"])
			}
			if notApplied, _ := row["not_applied"].(string); !strings.Contains(notApplied, refusal.Error()) {
				t.Errorf("the row reads not_applied %q; want the machine's own refusal", row["not_applied"])
			}
		})
	}
}

// TestALostRaceRaisesNoErrorLineAndEveryOtherRefusalDoes: the person who decides the request
// today answered while the judge was reading, or its operator withheld the name, so the judge's
// own approval lands on a request the decision transaction refuses. That is the ordinary end of
// a race the judge is built to lose, and the deployment's alarms read the broker's ERROR lines,
// so it leaves none, and the row it writes carries no error either — "error" is the model call's
// own failure, and nothing failed here. A refusal of any other kind is a judge that reached a
// request it should not have, and that does raise one, naming the record a person has to look
// at.
func TestALostRaceRaisesNoErrorLineAndEveryOtherRefusalDoes(t *testing.T) {
	for _, c := range []struct {
		refusal   error
		wantError bool
	}{
		{requests.ErrTerminal, false},
		{requests.ErrExpired, false},
		// The withheld guard currentPolicyAdmits makes for the judge alone: the
		// operator took the name back from the session while the model was answering.
		{fmt.Errorf("%w: AIRLOCK_JUDGED_TOKEN is withheld from this session", record.ErrNotApprover), false},
		{errors.New("the grant could not be written"), true},
	} {
		t.Run(c.refusal.Error(), func(t *testing.T) {
			var records logtest.Recorder
			logtest.SetDefault(t, &records)
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
			machine.applyErr = c.refusal
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			var errorLines []string
			for _, r := range records.Records() {
				if r.Level >= slog.LevelError {
					errorLines = append(errorLines, r.Message)
				}
			}
			if got := len(errorLines) > 0; got != c.wantError {
				t.Errorf("the run wrote the ERROR lines %q; want any: %v", errorLines, c.wantError)
			}
			_, logged := machine.rows()
			if len(logged) != 1 {
				t.Fatalf("the run logged %d rows; want the verdict kept whatever the refusal", len(logged))
			}
			if _, carries := logged[0]["error"]; carries {
				t.Errorf("the row carries error %v; want none: the model answered, and it was the apply that was refused", logged[0]["error"])
			}
		})
	}
}

// TestARunWhoseContextEndsWritesNothing: the broker stopped serving, or the run's thirty seconds
// went, before anything was written. Nothing is read and nothing is written, which leaves the
// request pending for the person who decides it today.
func TestARunWhoseContextEndsWritesNothing(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(ended, pending())

	applied, logged := machine.rows()
	if len(applied) != 0 || len(logged) != 0 {
		t.Errorf("the run applied %d decisions and logged %d rows; want neither", len(applied), len(logged))
	}
	if model.asked() != 0 || len(dispatch.got()) != 0 {
		t.Errorf("the model got %d requests and Dispatch %d; want none of either", model.asked(), len(dispatch.got()))
	}
}

// TestAnIssueWithNoSpecIsJudgedOnItsKeyTitleAndStatus: Dispatch holds the issue but no spec
// document, so the model is asked all the same, and the data object says the spec is absent
// rather than showing it an empty one (D14).
func TestAnIssueWithNoSpecIsJudgedOnItsKeyTitleAndStatus(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	dispatch.specStatus = http.StatusNotFound
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

	data := model.data(t)
	if data.Issue.Key != testIssue || data.Issue.Title != "The broker's model judge" || data.Issue.Status != "doing" {
		t.Errorf("the issue in front of the model is %+v; want its key, title and status", data.Issue)
	}
	if data.Issue.SpecPresent || data.Issue.Spec != "" {
		t.Errorf("the issue reads spec_present %v with spec %q; want the absence said plainly", data.Issue.SpecPresent, data.Issue.Spec)
	}
	if applied, _ := machine.rows(); len(applied) != 1 {
		t.Errorf("the run applied %d decisions; want the issue judged on what Dispatch does hold", len(applied))
	}
}

// TestTheDataInFrontOfTheModelIsTheRequestAndItsIssue: the whole of what the model is shown, for
// a request every check passed — the secret, its rules entry, the signed reason, and the issue
// with the extract of its spec.
func TestTheDataInFrontOfTheModelIsTheRequestAndItsIssue(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	request := pending()
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), request)

	data := model.data(t)
	switch {
	case data.Secret != testSecret:
		t.Errorf("the data names the secret %q; want %q", data.Secret, testSecret)
	case data.Rules == nil || data.Rules.Judge == nil || *data.Rules.Judge != 900:
		t.Errorf("the data's rules entry is %+v; want the secret's own entry with its judge seconds", data.Rules)
	case data.Reason != request.Reason:
		t.Errorf("the data's reason is %q; want the request's signed reason", data.Reason)
	case !data.Issue.SpecPresent || !strings.Contains(data.Issue.Spec, "**R1.** It never denies."):
		t.Errorf("the data's spec extract is %q; want the Requirements section in it", data.Issue.Spec)
	case strings.Contains(data.Issue.Spec, "People wait."):
		t.Errorf("the data's spec extract is %q; want the sections between Summary and Requirements left out", data.Issue.Spec)
	}
	if policyAsked := machine.policyAsked; len(policyAsked) != 1 || policyAsked[0] != testSecret {
		t.Errorf("the policy was re-read for %v; want the one name the request asks for", policyAsked)
	}
}

// TestSkipChecksAsksTheModelAboutARequestACheckWouldHaveStopped is the negative control of the
// six checks: with the switch on, a request that fails one still reaches the model, and the row
// still records which check failed.
func TestSkipChecksAsksTheModelAboutARequestACheckWouldHaveStopped(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	options := testOptions(t, dispatch, model, machine)
	options.SkipChecks = true
	request := pending()
	request.LifetimeSeconds = new(3600)
	New(t.Context(), options).consider(t.Context(), request)

	if model.asked() != 1 {
		t.Fatalf("the model got %d requests; want the switch to put the request to it anyway", model.asked())
	}
	applied, logged := machine.rows()
	if len(applied) != 1 || len(logged) != 0 {
		t.Fatalf("the run applied %d decisions and logged %d rows; want the model's approval applied", len(applied), len(logged))
	}
	if failed := failedCheck(t, applied[0]); failed != checkLifetime {
		t.Errorf("the row names %q as the check that failed; want the lifetime check recorded all the same", failed)
	}
}

// TestConsiderRunsTheJudgeOffTheRunnersContext: Consider answers at once and the run happens on
// a goroutine of its own, which is how the request that created the record is never held up by
// a model call.
func TestConsiderRunsTheJudgeOffTheRunnersContext(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	New(t.Context(), testOptions(t, dispatch, model, machine)).Consider(pending())

	waitFor(t, "the run Consider started to apply its decision", 10*time.Second, func() bool {
		applied, _ := machine.rows()
		return len(applied) == 1
	})
}

// TestTheWorstCaseStillWritesItsJudgedRow is the run the three bounds are arithmetic for: the
// Dispatch phase spends what dispatchBound gives it, the model call then runs out its own
// modelBound, and the transaction that records the verdict takes two seconds of its own. That
// is the longest a run can be, and its row is still written, because the two bounds leave the
// write its seconds. Were they to fill runBudget between them, this run would write nothing at
// all — the broker would have paid for two HTTP reads and a model call with no audit row to
// show for them, which is the one outcome the trail exists to prevent.
//
// The request is left pending either way, so the row is the whole of what this proves. It takes
// the wall-clock time it says it does — the bounds are real durations — and runs beside the
// rest of the package's tests rather than after them.
func TestTheWorstCaseStillWritesItsJudgedRow(t *testing.T) {
	t.Parallel()
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
	// Each of the phase's two GETs takes half of its bound, less a twentieth of a second
	// for the two round trips themselves: the phase has to spend its bound and still
	// answer, since a read the bound cut short would fail the issue check and the model
	// would never be asked at all.
	dispatch.delay = dispatchBound/2 - 50*time.Millisecond
	// The model never answers, so the call ends on modelBound.
	model.delay = runBudget
	// The row's own transaction, which has to commit inside what is left of the budget.
	machine.writeTakes = 2 * time.Second
	started := time.Now()
	New(t.Context(), testOptions(t, dispatch, model, machine)).Consider(pending())

	waitFor(t, "the slowest run there is to write its row", runBudget+5*time.Second, func() bool {
		_, logged := machine.rows()
		return len(logged) == 1
	})
	if took := time.Since(started); took < dispatchBound+modelBound-time.Second {
		t.Errorf("the run wrote its row after %s; want it to have spent both bounds, %s", took, dispatchBound+modelBound)
	}
	row := theOneRow(t, machine)
	if row["verdict"] != Refer || row["asked"] != true {
		t.Errorf("the row reads verdict %v, asked %v; want the model asked and the request referred", row["verdict"], row["asked"])
	}
	if _, carries := row["error"].(string); !carries {
		t.Errorf("the row carries error %v; want the model call's own timeout", row["error"])
	}
	if failed := failedCheck(t, row); failed != "" {
		t.Errorf("the row names %q as a failed check; want every check passed before the model", failed)
	}
}

// TestARequestThatIsNoLongerPendingIsNeverPutToTheModel: the run reads the request's state again
// immediately before the model call, and asks nothing about a request that has gone — cancelled
// by its session, decided by its person, or expired while the run was reading Dispatch. The row
// says so and names no failed check: nothing was wrong with the request, there is simply no
// request left to decide. A state that could not be read at all is the same answer, since the
// model call is what a run that guessed wrong would be paying for.
func TestARequestThatIsNoLongerPendingIsNeverPutToTheModel(t *testing.T) {
	for _, c := range []struct {
		name, state string
		err         error
		want        string
	}{
		{name: "its session cancelled it", state: "cancelled", want: "the request is no longer pending: it is cancelled"},
		{name: "its person decided it", state: "granted", want: "the request is no longer pending: it is granted"},
		{name: "its pending window passed", state: "expired", want: "the request is no longer pending: it is expired"},
		{
			name: "its state could not be read", err: errors.New("the request is gone"),
			want: "the request's state could not be read: the request is gone",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
			machine.state, machine.stateErr = c.state, c.err
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			if model.asked() != 0 {
				t.Errorf("the model got %d requests; want none for a request that is no longer pending", model.asked())
			}
			row := theOneRow(t, machine)
			if row["not_pending"] != c.want {
				t.Errorf("the row reads not_pending %q; want %q", row["not_pending"], c.want)
			}
			if row["verdict"] != Refer || row["asked"] != false {
				t.Errorf("the row reads verdict %v, asked %v; want a referral the model was never asked for", row["verdict"], row["asked"])
			}
			if failed := failedCheck(t, row); failed != "" {
				t.Errorf("the row names %q as a failed check; want every check passed: the request itself was fine", failed)
			}
		})
	}

	t.Run("a request still pending", func(t *testing.T) {
		dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Approve, "yes")), newRequests()
		New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

		if model.asked() != 1 || machine.stateAsked != 1 {
			t.Errorf("the run read the state %d times and asked the model %d times; want one read and the one model call it gates", machine.stateAsked, model.asked())
		}
		applied, _ := machine.rows()
		if len(applied) != 1 {
			t.Fatalf("the run applied %d decisions; want the pending request judged", len(applied))
		}
		if _, carries := applied[0]["not_pending"]; carries {
			t.Errorf("the row reads not_pending %v; want the key absent for a request that was pending", applied[0]["not_pending"])
		}
	})
}

// runAgainstTheModel is one run whose every check passes, against a model answering reply.
func runAgainstTheModel(t *testing.T, reply string) *fakeRequests {
	t.Helper()
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, reply), newRequests()
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())
	return machine
}
