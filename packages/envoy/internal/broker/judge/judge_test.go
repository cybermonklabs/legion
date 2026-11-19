// packages/envoy/internal/broker/judge/judge_test.go
// What a run leaves behind: which requests never reach the model and how each ends, what a grant
// and a deny write, what a model that gives no verdict leaves, what the model is shown, and what
// the judged row holds. Every run here goes through stand-in HTTP servers and a fake request
// machine.
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

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// finalWords is how every refusal ends: that the request will not be granted, and whom to tell.
const finalWords = ". This request will not be granted. Tell whoever gave you this work."

// noRetry fails when a sentence an agent reads tells it to try again.
func noRetry(t *testing.T, sentence string) {
	t.Helper()
	for _, said := range []string{"again", "retry", "without citing", "broker page"} {
		if strings.Contains(sentence, said) {
			t.Errorf("the refusal reads %q; want nothing in it that says %q", sentence, said)
		}
	}
}

// TestTheChecksBeforeTheModel is each check in turn: the model is never asked, and the request
// ends one of two ways. A key the policy now denies, and a key withheld from the session, refuse it
// for good, since no decision of anyone's could grant it: one refusal, naming the check, ending in
// the final words. Every other failure hands it to its person: one row, naming the check, and no
// refusal. The checks before the Dispatch reads stop the run before Dispatch is called at all.
func TestTheChecksBeforeTheModel(t *testing.T) {
	for _, c := range []struct {
		name          string
		want          string
		final         bool
		wantDetail    string
		readsDispatch bool
		request       func(*requests.PendingRequest)
		machine       func(*fakeRequests)
		dispatch      func(*dispatchStandIn)
	}{
		{
			name: "a key the rules give the judge nothing of", want: checkRulesJudge,
			wantDetail: "decide nothing for NO_JUDGE_KEY",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided = []string{"NO_JUDGE_KEY"}, []string{"NO_JUDGE_KEY"}
			},
		},
		{
			name: "a judge key beside a key the rules give the judge nothing of", want: checkRulesJudge,
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided = []string{testSecret, "NO_JUDGE_KEY"}, []string{testSecret, "NO_JUDGE_KEY"}
			},
		},
		{
			name: "a lifetime past the fifteen minutes of the class", want: checkLifetime,
			wantDetail: "past the 15m0s the judge decides",
			request:    func(p *requests.PendingRequest) { p.LifetimeSeconds = new(3600) },
		},
		{
			name: "a lifetime past a key's own judge seconds", want: checkLifetime,
			wantDetail: fmt.Sprintf("600 seconds of %s, past the %d", shortJudgeSecret, shortJudgeSeconds),
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided, p.LifetimeSeconds = []string{shortJudgeSecret}, []string{shortJudgeSecret}, new(600)
			},
		},
		{
			name: "two judge keys past the shorter one's judge seconds", want: checkLifetime,
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided, p.LifetimeSeconds = []string{testSecret, shortJudgeSecret}, []string{testSecret, shortJudgeSecret}, new(600)
			},
		},
		{
			name: "no signed lifetime at all", want: checkLifetime,
			request: func(p *requests.PendingRequest) { p.LifetimeSeconds = nil },
		},
		{
			name: "the policy denying the key now", want: checkPolicy, final: true,
			wantDetail: "the policy denies " + testSecret + " to this session now",
			machine:    func(f *fakeRequests) { f.decision.Outcome = policy.Deny },
		},
		{
			name: "the policy answering automatic now", want: checkPolicy,
			machine: func(f *fakeRequests) { f.decision.Outcome = policy.Automatic },
		},
		{
			name: "the policy that could not be re-read", want: checkPolicy,
			machine: func(f *fakeRequests) { f.policyErr = errors.New("the store did not answer") },
		},
		{
			name: "an administrator's key", want: checkAdmin,
			wantDetail: decidedByAPerson(testSecret),
			machine:    func(f *fakeRequests) { f.admin = true },
		},
		{
			name: "a key withheld from the session", want: checkWithheld, final: true,
			wantDetail: testSecret + " is withheld from this session",
			machine:    func(f *fakeRequests) { f.withheld = true },
		},
		{
			name: "a key its session got at once that needs a decision now", want: checkPolicy,
			wantDetail: agentKey + " needs a decision now",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided = []string{testSecret, agentKey}, []string{testSecret}
			},
		},
		{
			name: "a key its session got at once that the policy now denies", want: checkPolicy, final: true,
			wantDetail: "the policy denies " + agentKey + " to this session now",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided = []string{testSecret, agentKey}, []string{testSecret}
			},
			machine: func(f *fakeRequests) { f.deniedName = agentKey },
		},
		{
			name: "one key of two withheld from the session", want: checkWithheld, final: true,
			wantDetail: shortJudgeSecret + " is withheld from this session",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided, p.LifetimeSeconds = []string{testSecret, shortJudgeSecret}, []string{testSecret, shortJudgeSecret}, new(300)
			},
			machine: func(f *fakeRequests) { f.withheldName = shortJudgeSecret },
		},
		{
			name: "a key its session got at once, withheld from the session", want: checkWithheld, final: true,
			wantDetail: agentKey + " is withheld from this session",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided = []string{testSecret, agentKey}, []string{testSecret}
			},
			machine: func(f *fakeRequests) { f.automaticName, f.withheldName = agentKey, agentKey },
		},
		{
			name: "a key the policy denies after a key that failed it for a while", want: checkPolicy, final: true,
			wantDetail: "the policy denies " + shortJudgeSecret + " to this session now",
			request: func(p *requests.PendingRequest) {
				p.Secrets, p.Decided, p.LifetimeSeconds = []string{testSecret, shortJudgeSecret}, []string{testSecret, shortJudgeSecret}, new(300)
			},
			machine: func(f *fakeRequests) { f.automaticName, f.deniedName = testSecret, shortJudgeSecret },
		},
		{
			name: "an administrator's key withheld from the session", want: checkWithheld, final: true,
			wantDetail: testSecret + " is withheld from this session",
			machine:    func(f *fakeRequests) { f.admin, f.withheld = true, true },
		},
		{
			name: "an issue read that failed", want: checkDispatch, readsDispatch: true,
			wantDetail: "issue " + testIssue + " could not be read",
			dispatch:   func(d *dispatchStandIn) { d.issueStatus = http.StatusInternalServerError },
		},
		{
			name: "an ask read that failed", want: checkDispatch, readsDispatch: true,
			wantDetail: "ask " + testAsk + " could not be read",
			request:    func(p *requests.PendingRequest) { *p = citedPending() },
			dispatch:   func(d *dispatchStandIn) { d.askStatus = http.StatusInternalServerError },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
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

			var row map[string]any
			if c.final {
				var why string
				row, why = theOneRefusal(t, machine)
				if !strings.HasPrefix(why, "the model judge refused this request: ") || !strings.HasSuffix(why, finalWords) {
					t.Errorf("the refusal reads %q; want the judge's final words", why)
				}
				if c.wantDetail != "" && !strings.Contains(why, c.wantDetail) {
					t.Errorf("the refusal reads %q; want it to say %q", why, c.wantDetail)
				}
				noRetry(t, why)
			} else {
				row = theOneRow(t, machine)
			}
			if got := failedCheck(t, row); got != c.want {
				t.Errorf("the row names %q as the check that failed; want %q", got, c.want)
			}
			if c.wantDetail != "" && !strings.Contains(failedDetail(t, row), c.wantDetail) {
				t.Errorf("the failed check reads %q; want it to say %q", failedDetail(t, row), c.wantDetail)
			}
			if model.asked() != 0 || row["asked"] != false {
				t.Errorf("the model got %d requests and the row reads asked %v; want neither", model.asked(), row["asked"])
			}
			if read := len(dispatch.got()) > 0; read != c.readsDispatch {
				t.Errorf("Dispatch got %d requests; want any: %v", len(dispatch.got()), c.readsDispatch)
			}
		})
	}
}

// TestTheModelsTwoVerdicts: grant is written with the approval, in the one transaction
// ApplyJudgeDecision makes, and deny is the request's final refusal, carrying the model's own
// reason in the sentence its agent reads, which tells it nothing to try again.
func TestTheModelsTwoVerdicts(t *testing.T) {
	t.Run(Grant, func(t *testing.T) {
		machine := runAgainstTheModel(t, verdictReply(Grant, "The reason meets the guidance."))
		applied, logged := machine.rows()
		if denied, _ := machine.refused(); len(applied) != 1 || len(logged) != 0 || len(denied) != 0 {
			t.Fatalf("the run applied %d, logged %d and refused %d; want the one grant", len(applied), len(logged), len(denied))
		}
		if applied[0]["verdict"] != Grant || applied[0]["reason"] != "The reason meets the guidance." || applied[0]["asked"] != true {
			t.Errorf("the row reads %v; want the grant, its reason, and the model asked", applied[0])
		}
	})
	t.Run(Deny, func(t *testing.T) {
		machine := runAgainstTheModel(t, verdictReply(Deny, "The reason cites no approved post"))
		row, why := theOneRefusal(t, machine)
		if want := "the model judge refused this request: The reason cites no approved post" + finalWords; why != want {
			t.Errorf("the refusal reads %q; want %q", why, want)
		}
		noRetry(t, why)
		if row["verdict"] != Deny || row["asked"] != true {
			t.Errorf("the row reads verdict %v, asked %v; want the model's deny", row["verdict"], row["asked"])
		}
	})
}

// TestAModelThatGivesNoVerdictHandsTheRequestOver: a model endpoint that fails, an answer that is
// no JSON object, and an answer outside the two words are no verdict at all. Each hands the
// request to its person, once — one row carrying the error, and no refusal — and never grants.
func TestAModelThatGivesNoVerdictHandsTheRequestOver(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply string
		setUp func(*modelStandIn)
	}{
		{name: "the model endpoint failing", reply: verdictReply(Grant, "yes"), setUp: func(m *modelStandIn) { m.status = http.StatusServiceUnavailable }},
		{name: "an answer that is no JSON", reply: "I think this is fine, but not in JSON."},
		{name: "an answer outside the two words", reply: verdictReply("refer", "for a person")},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, c.reply), newRequests()
			if c.setUp != nil {
				c.setUp(model)
			}
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())
			row := theOneRow(t, machine)
			if _, carries := row["error"].(string); !carries {
				t.Errorf("the row carries error %v; want what the model call failed with", row["error"])
			}
			if row["verdict"] != Deny {
				t.Errorf("the row reads verdict %v; want the no-verdict default, deny, beside its error", row["verdict"])
			}
		})
	}
}

// TestTheModelReadsEachKeysGuidanceAndWhatTheReasonPointsAt: the whole of what the model is shown,
// for a request for two judge keys whose reason names an issue and cites an ask: each key with its
// rules entry and its guidance, the signed reason, the issue with the extract of its spec, and the
// ask with its question and the answer and who gave it. A reason that points at nothing costs no
// Dispatch read and shows no issue and no ask.
func TestTheModelReadsEachKeysGuidanceAndWhatTheReasonPointsAt(t *testing.T) {
	t.Run("a reason naming an issue and citing an ask", func(t *testing.T) {
		dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
		request := citedPending()
		request.Secrets, request.Decided, request.LifetimeSeconds = []string{testSecret, shortJudgeSecret}, []string{testSecret, shortJudgeSecret}, new(300)
		New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), request)

		data := model.data(t)
		if len(data.Keys) != 2 || data.Keys[0].Name != testSecret || data.Keys[0].Guidance != testGuidance ||
			data.Keys[1].Name != shortJudgeSecret || data.Keys[1].Guidance != shortGuidance {
			t.Errorf("the keys in front of the model are %+v; want both, each with its own guidance", data.Keys)
		}
		if data.Keys[0].Rules == nil || data.Keys[0].Rules.Judge == nil || *data.Keys[0].Rules.Judge != 900 {
			t.Errorf("the first key's rules entry is %+v; want its own entry with its judge seconds", data.Keys[0].Rules)
		}
		if data.Reason != request.Reason {
			t.Errorf("the data's reason is %q; want the request's signed reason", data.Reason)
		}
		switch {
		case data.Issue == nil || data.Issue.Key != testIssue || !data.Issue.Found || !strings.Contains(data.Issue.Spec, "**R1.**"):
			t.Errorf("the issue in front of the model is %+v; want the issue the reason names, with its spec's Requirements", data.Issue)
		case data.Ask == nil || data.Ask.ID != testAsk || data.Ask.Question != testQuestion || data.Ask.AnsweredBy != testApprover ||
			data.Ask.Text != testAnswerText || len(data.Ask.Selected) != 1:
			t.Errorf("the ask in front of the model is %+v; want the cited ask, its question and its answer", data.Ask)
		}
		if asked := machine.policyAsked; len(asked) != 2 {
			t.Errorf("the policy was re-read for %v; want each of the two keys", asked)
		}
	})

	t.Run("a reason pointing at nothing", func(t *testing.T) {
		dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
		request := pending()
		request.Reason = "export the payroll for the month"
		New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), request)

		data := model.data(t)
		if data.Issue != nil || data.Ask != nil {
			t.Errorf("the data carries issue %+v and ask %+v; want neither for a reason that points at nothing", data.Issue, data.Ask)
		}
		if calls := dispatch.got(); len(calls) != 0 {
			t.Errorf("Dispatch got %d calls; want none", len(calls))
		}
		if applied, _ := machine.rows(); len(applied) != 1 {
			t.Errorf("the run applied %d decisions; want the request judged on its guidance alone", len(applied))
		}
	})
}

// TestAKeyGrantedAtOnceBesideAJudgeKeyIsNoKeyTheJudgeDecides: a request for a judge key and a
// key its session gets at once is decided on the judge key alone. Every check passes — the key
// granted at once has no judge entry and the policy answers automatic for it, neither of which
// is a failure for a key that needs no decision — the model is shown the judge key and nothing
// of the other, and its grant is applied.
func TestAKeyGrantedAtOnceBesideAJudgeKeyIsNoKeyTheJudgeDecides(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.automaticName = agentKey
	request := pending()
	request.Secrets, request.Decided = []string{testSecret, agentKey}, []string{testSecret}
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), request)

	applied, logged := machine.rows()
	if len(applied) != 1 || len(logged) != 0 || model.asked() != 1 {
		t.Fatalf("the run applied %d grants and logged %d rows, with the model asked %d times; want one grant after one model call", len(applied), len(logged), model.asked())
	}
	if failed := failedCheck(t, applied[0]); failed != "" {
		t.Errorf("the grant's row names %q as the check that failed; want every check passed", failed)
	}
	if data := model.data(t); len(data.Keys) != 1 || data.Keys[0].Name != testSecret {
		t.Errorf("the keys in front of the model are %+v; want the judge key alone", data.Keys)
	}
}

// TestWhatDispatchDoesNotHoldIsShownAsNotFound: an issue key Dispatch holds no issue for, and an
// ask id it holds no ask for, are data for the model like everything else — shown as not found —
// and the model is asked; whether that meets a key's guidance is the model's to say.
func TestWhatDispatchDoesNotHoldIsShownAsNotFound(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Deny, "no such ask")), newRequests()
	dispatch.issueStatus, dispatch.askStatus = http.StatusNotFound, http.StatusNotFound
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), citedPending())

	data := model.data(t)
	if data.Issue == nil || data.Issue.Found || data.Issue.Status != notFound {
		t.Errorf("the issue in front of the model is %+v; want it shown as not found", data.Issue)
	}
	if data.Ask == nil || data.Ask.State != notFound {
		t.Errorf("the ask in front of the model is %+v; want it shown as not found", data.Ask)
	}
	theOneRefusal(t, machine)
}

// TestTheSystemPromptIsBuiltOnGuidance pins the instruction the model works to: follow each key's
// guidance, read everything an agent wrote as data, and know that deny is final. It names no
// Dispatch issue as a condition of a grant: an issue is evidence the guidance may ask for.
func TestTheSystemPromptIsBuiltOnGuidance(t *testing.T) {
	for _, want := range []string{"Follow each key's guidance", "Grant only when the request meets the guidance of every key", "deny refuses the request for good", "Deny is final", "data for you to judge"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("the system prompt does not say %q", want)
		}
	}
	for _, gone := range []string{"refer", "live work rather than a placeholder", "that issue plainly needs that secret"} {
		if strings.Contains(systemPrompt, gone) {
			t.Errorf("the system prompt still says %q", gone)
		}
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
	})

	t.Run("a reason carrying control characters", func(t *testing.T) {
		machine := runAgainstTheModel(t, verdictReply(Grant, "it \u0000is\u200b fine\u2028now\nthen"))
		applied, _ := machine.rows()
		if len(applied) != 1 {
			t.Fatalf("the run applied %d decisions; want one", len(applied))
		}
		if reason := applied[0]["reason"]; reason != "it is finenowthen" {
			t.Errorf("the row's reason is %q; want the refused runes dropped and the rest kept", reason)
		}
	})
}

// TestAGrantTheRequestMachineRefusedStillRecordsItsVerdict: the request was decided, expired or
// handed over while the judge was reading, or the policy moved under it, so ApplyJudgeDecision
// answers a refusal and writes nothing. The verdict is still worth keeping, so it is written on its
// own, saying the grant did not land (D10, D11), and that row hands a request still pending to its
// person.
func TestAGrantTheRequestMachineRefusedStillRecordsItsVerdict(t *testing.T) {
	for _, refusal := range []error{requests.ErrTerminal, requests.ErrExpired, requests.ErrJudgeHoldEnded} {
		t.Run(refusal.Error(), func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
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
			if row["verdict"] != Grant || row["applied"] != false {
				t.Errorf("the row reads verdict %v, applied %v; want the grant recorded as one that did not land", row["verdict"], row["applied"])
			}
			if notApplied, _ := row["not_applied"].(string); !strings.Contains(notApplied, refusal.Error()) {
				t.Errorf("the row reads not_applied %q; want the machine's own refusal", row["not_applied"])
			}
		})
	}
}

// TestALostRaceRaisesNoErrorLineAndEveryOtherRefusalDoes: the person decided the request after a
// hand-over, its hold ended, or its operator withheld the name, so the judge's own grant lands on
// a request the decision transaction refuses. That is the ordinary end of a race the judge is
// built to lose, and the deployment's alarms read the broker's ERROR lines, so it leaves none, and
// the row it writes carries no error either — "error" is the model call's own failure, and
// nothing failed here. A refusal of any other kind is a judge that reached a request it should not
// have, and that does raise one, naming the record a person has to look at.
func TestALostRaceRaisesNoErrorLineAndEveryOtherRefusalDoes(t *testing.T) {
	for _, c := range []struct {
		refusal   error
		wantError bool
	}{
		{requests.ErrTerminal, false},
		{requests.ErrExpired, false},
		{requests.ErrJudgeHoldEnded, false},
		// The withheld guard currentPolicyAdmits makes for the judge alone: the
		// operator took the name back from the session while the model was answering.
		{fmt.Errorf("%w: AIRLOCK_JUDGED_TOKEN is withheld from this session", record.ErrNotApprover), false},
		{errors.New("the grant could not be written"), true},
	} {
		t.Run(c.refusal.Error(), func(t *testing.T) {
			var records logtest.Recorder
			logtest.SetDefault(t, &records)
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
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
				t.Fatalf("the run logged %d rows; want the verdict kept once", len(logged))
			}
			if _, carries := logged[0]["error"]; carries {
				t.Errorf("the row carries error %v; want none: the model answered, and it was the apply that was refused", logged[0]["error"])
			}
		})
	}
}

// TestARunWhoseContextEndsWritesNothing: the broker stopped serving, or the run's budget went,
// before anything was written. Nothing is read and nothing is written; the hold then runs out,
// and the request is its person's.
func TestARunWhoseContextEndsWritesNothing(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
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

// TestAnIssueWithNoSpecIsShownOnItsKeyTitleAndStatus: Dispatch holds the issue but no spec
// document, so the model is asked all the same, and the data object says the spec is absent
// rather than showing it an empty one (D14).
func TestAnIssueWithNoSpecIsShownOnItsKeyTitleAndStatus(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	dispatch.specStatus = http.StatusNotFound
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

	data := model.data(t)
	if data.Issue == nil || data.Issue.Key != testIssue || data.Issue.Title != "The broker's model judge" || data.Issue.Status != "doing" {
		t.Errorf("the issue in front of the model is %+v; want its key, title and status", data.Issue)
	}
	if data.Issue != nil && (data.Issue.SpecPresent || data.Issue.Spec != "") {
		t.Errorf("the issue reads spec_present %v with spec %q; want the absence said plainly", data.Issue.SpecPresent, data.Issue.Spec)
	}
	if applied, _ := machine.rows(); len(applied) != 1 {
		t.Errorf("the run applied %d decisions; want the request judged on what Dispatch does hold", len(applied))
	}
}

// TestSkipChecksAsksTheModelAboutARequestACheckWouldHaveStopped is the negative control of the
// five checks: with the switch on, a request that fails one still reaches the model, and the row
// still records which check failed.
func TestSkipChecksAsksTheModelAboutARequestACheckWouldHaveStopped(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
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
		t.Fatalf("the run applied %d decisions and logged %d rows; want the model's grant applied", len(applied), len(logged))
	}
	if failed := failedCheck(t, applied[0]); failed != checkLifetime {
		t.Errorf("the row names %q as the check that failed; want the lifetime check recorded all the same", failed)
	}
}

// TestConsiderRunsTheJudgeOffTheRunnersContext: Consider answers at once and the run happens on
// a goroutine of its own, which is how the request that created the record is never held up by
// a model call.
func TestConsiderRunsTheJudgeOffTheRunnersContext(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	New(t.Context(), testOptions(t, dispatch, model, machine)).Consider(pending())

	waitFor(t, "the run Consider started to apply its decision", 10*time.Second, func() bool {
		applied, _ := machine.rows()
		return len(applied) == 1
	})
}

// TestTheWorstCaseStillWritesItsJudgedRow is the run the three bounds are arithmetic for: the
// issue read spends what its dispatchBound gives it, the ask read its own, the model call then
// runs out its own modelBound, and the transaction that records the verdict takes two seconds of
// its own. That is the longest a run can be, and its row is still written, because the bounds
// leave the write its seconds. Were they to fill runBudget between them, this run would write
// nothing at all. A model that never answers gives no verdict, so the row hands the request to its
// person. It takes the wall-clock time it says it does and runs beside the rest of the package.
func TestTheWorstCaseStillWritesItsJudgedRow(t *testing.T) {
	t.Parallel()
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	// Each Dispatch answer takes half of a bound, less a twentieth of a second for its round
	// trip: the issue phase's two GETs spend the first bound together and the ask read half
	// of its own, and each still answers.
	dispatch.delay = dispatchBound/2 - 50*time.Millisecond
	// The model never answers, so the call ends on modelBound.
	model.delay = runBudget
	// The row's own transaction, which has to commit inside what is left of the budget.
	machine.writeTakes = 2 * time.Second
	started := time.Now()
	New(t.Context(), testOptions(t, dispatch, model, machine)).Consider(citedPending())

	waitFor(t, "the slowest run there is to write its row", runBudget+5*time.Second, func() bool {
		_, logged := machine.rows()
		return len(logged) == 1
	})
	if took := time.Since(started); took < dispatchBound+modelBound-time.Second {
		t.Errorf("the run wrote its row after %s; want it to have spent both bounds, %s", took, dispatchBound+modelBound)
	}
	row := theOneRow(t, machine)
	if row["asked"] != true {
		t.Errorf("the row reads asked %v; want the model asked", row["asked"])
	}
	if _, carries := row["error"].(string); !carries {
		t.Errorf("the row carries error %v; want the model call's own timeout", row["error"])
	}
	if failed := failedCheck(t, row); failed != "" {
		t.Errorf("the row names %q as a failed check; want every check passed before the model", failed)
	}
}

// TestARunEndsWellInsideTheJudgesHold: a request the judge holds is its person's once the hold
// runs out, so a run that could outlast the hold would see its request handed over under it. The
// hold is two whole runs long at least, so only a lost run is ever ended by it.
func TestARunEndsWellInsideTheJudgesHold(t *testing.T) {
	if 2*runBudget > requests.JudgeHold {
		t.Fatalf("runBudget is %s and requests.JudgeHold %s; want the hold to cover two runs", runBudget, requests.JudgeHold)
	}
}

// TestARequestThatIsNoLongerPendingIsNeverPutToTheModel: the run reads the request's state again
// immediately before the model call, and asks nothing about a request that has gone — cancelled
// by its session, decided, or expired while the run was reading Dispatch. The row says so and
// names no failed check: nothing was wrong with the request, there is simply no request left to
// decide. A state that could not be read at all is the same answer, since the model call is what
// a run that guessed wrong would be paying for.
func TestARequestThatIsNoLongerPendingIsNeverPutToTheModel(t *testing.T) {
	for _, c := range []struct {
		name, state string
		err         error
		want        string
	}{
		{name: "its session cancelled it", state: "cancelled", want: "the request is no longer pending: it is cancelled"},
		{name: "it was decided", state: "granted", want: "the request is no longer pending: it is granted"},
		{name: "its pending window passed", state: "expired", want: "the request is no longer pending: it is expired"},
		{
			name: "its state could not be read", err: errors.New("the request is gone"),
			want: "the request's state could not be read: the request is gone",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
			machine.state, machine.stateErr = c.state, c.err
			New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())

			if model.asked() != 0 {
				t.Errorf("the model got %d requests; want none for a request that is no longer pending", model.asked())
			}
			row := theOneRow(t, machine)
			if row["not_pending"] != c.want || row["asked"] != false {
				t.Errorf("the row reads not_pending %q, asked %v; want %q and the model never asked", row["not_pending"], row["asked"], c.want)
			}
			if failed := failedCheck(t, row); failed != "" {
				t.Errorf("the row names %q as a failed check; want every check passed: the request itself was fine", failed)
			}
		})
	}
}

// runAgainstTheModel is one run whose every check passes, against a model answering reply.
func runAgainstTheModel(t *testing.T, reply string) *fakeRequests {
	t.Helper()
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, reply), newRequests()
	New(t.Context(), testOptions(t, dispatch, model, machine)).consider(t.Context(), pending())
	return machine
}
