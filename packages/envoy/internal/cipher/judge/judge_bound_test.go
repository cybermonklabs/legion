// packages/envoy/internal/cipher/judge/judge_bound_test.go
// Round 4's B8 on cybermonklabs/legion#4135: every read of Dispatch a run makes is under the one
// bound, requests.JudgeDispatch, and the run's request.judged row is written on a context of its
// own, so neither a slow Dispatch nor the run's own deadline can leave a run that asked the model
// without its row.
package judge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logtest"
)

// boundTransport records, for each HTTP request the judge's Dispatch client sends, whether its
// context was one requests.JudgeDispatch made.
type boundTransport struct {
	inner   http.RoundTripper
	mu      sync.Mutex
	bounded []bool
}

func (b *boundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	b.mu.Lock()
	b.bounded = append(b.bounded, requests.UnderJudgeDispatch(r.Context()))
	b.mu.Unlock()
	return b.inner.RoundTrip(r)
}

func (b *boundTransport) calls() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.bounded...)
}

// TestEveryDispatchReadOfARunIsUnderTheOneBound is the judge's half of round 4's class test: the
// policy check (JudgePolicy, which reads what Dispatch holds of a tagged name), the project check
// (JudgeProject) and both of the issue read's GETs run under requests.JudgeDispatch, the one bound
// the run's arithmetic gives its Dispatch phase. A run that made none of them, or no GET, is a
// failure too: a list without them cannot say whether they are bounded. It runs on the broker's
// own phase (New's requests.JudgeDispatch), so it also holds that phase's length: the deadline the
// phase's first read saw falls dispatchBound after a moment inside the run, as JudgeDispatch gives
// it, and a judge that cut the phase short before that read fails here.
func TestEveryDispatchReadOfARunIsUnderTheOneBound(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	opts := testOptions(t, dispatch, model, machine)
	client := *dispatch.server.Client()
	transport := &boundTransport{inner: client.Transport}
	client.Transport = transport
	opts.Dispatch = NewDispatch(dispatch.server.URL, ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, "projected-token")), &client)
	before := time.Now()
	New(t.Context(), opts).consider(t.Context(), pending())
	after := time.Now()
	switch seen := machine.phaseTimes().seen; {
	case seen.IsZero():
		t.Error("no read came under the run's Dispatch phase with a deadline on its context")
	case seen.Before(before.Add(dispatchBound)) || seen.After(after.Add(dispatchBound)):
		t.Errorf("the phase's first read saw a deadline %s after the run began (the run took %s); want dispatchBound, %s, after a moment inside the run", seen.Sub(before), after.Sub(before), dispatchBound)
	}

	if applied, _ := machine.rows(); len(applied) != 1 {
		t.Fatalf("the run applied %d approvals; want 1, so every read was made", len(applied))
	}
	calls := machine.boundedCalls()
	for _, want := range []string{"JudgePolicy", "JudgeProject"} {
		found := false
		for _, c := range calls {
			if c.method == want {
				found = true
				if !c.bounded {
					t.Errorf("%s was asked outside requests.JudgeDispatch", want)
				}
			}
		}
		if !found {
			t.Errorf("the run never called %s; want it called, under the bound", want)
		}
	}
	gets := transport.calls()
	if len(gets) != 2 {
		t.Errorf("the issue check sent %d requests; want its 2 GETs", len(gets))
	}
	for i, bounded := range gets {
		if !bounded {
			t.Errorf("the issue check's GET %d was sent outside requests.JudgeDispatch", i+1)
		}
	}
}

// TestTheJudgedRowIsWrittenOnAContextOfItsOwn: an approval that fails because the run's time ran
// out still leaves its row. Here the approval's reads take 3 seconds and the run has 2 left, so
// the approval fails at the run's deadline; the row is written on a context of its own, which the
// run's deadline does not cancel, with JudgeRowReserve to write in.
func TestTheJudgedRowIsWrittenOnAContextOfItsOwn(t *testing.T) {
	t.Parallel()
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.applyTakes = 3 * time.Second
	runner := New(t.Context(), testOptions(t, dispatch, model, machine))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	runner.write(ctx, pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})

	applied, logged := machine.rows()
	if len(applied) != 0 || len(logged) != 1 {
		t.Fatalf("the run applied %d approvals and logged %d rows; want the failed approval's row logged on its own", len(applied), len(logged))
	}
	if logged[0]["applied"] != false {
		t.Errorf("the row reads applied %v; want false: the approval wrote nothing", logged[0]["applied"])
	}
}

// TestADispatchFailureAtTheApprovalIsNoRefusal is round 4's m6: an approval that fails because
// Dispatch could not be asked whether the request is still the judge's, or because the broker
// could not learn whether Dispatch calls the session's person a restricted member
// (*projectaccess.MemberKindUncheckable, requests.Machine.restrictedOperator), is logged as that,
// at WARN, and never as the ERROR saying the judge's grant was refused, which is for a refusal the
// judge should never have reached. A member-kind read that failed because the run itself gave up
// (context.Canceled) says nothing of Dispatch: it is logged at INFO, on notApplied's line for the
// broker stopping under the approval. The request waits for its person in each case.
func TestADispatchFailureAtTheApprovalIsNoRefusal(t *testing.T) {
	for _, c := range []struct {
		name    string
		failure error
		level   slog.Level
	}{
		{"Dispatch could not say whether the request is the judge's", fmt.Errorf("%w: dispatch answered 503", requests.ErrProjectAccessUnavailable), slog.LevelWarn},
		{"Dispatch could not say the person's member kind", &projectaccess.MemberKindUncheckable{Person: "sami@example.com", Err: errors.New("projectaccess: dispatch answered /api/v1/access/person 503")}, slog.LevelWarn},
		{"the run gave up on the member-kind read", &projectaccess.MemberKindUncheckable{Person: "sami@example.com", Err: fmt.Errorf("projectaccess: ask dispatch: %w", context.Canceled)}, slog.LevelInfo},
	} {
		t.Run(c.name, func(t *testing.T) {
			var records logtest.Recorder
			logtest.SetDefault(t, &records)
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
			machine.applyErr = c.failure
			New(t.Context(), testOptions(t, dispatch, model, machine)).write(t.Context(), pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
			if _, logged := machine.rows(); len(logged) != 1 {
				t.Fatalf("the run logged %d rows; want the failed approval's row", len(logged))
			}
			var found bool
			for _, r := range records.Records() {
				if r.Level >= slog.LevelError {
					t.Errorf("the run wrote the ERROR line %q; want none: the approval was not refused", r.Message)
				}
				var namesRecord bool
				r.Attrs(func(a slog.Attr) bool {
					namesRecord = namesRecord || (a.Key == "record" && a.Value.String() == pending().RecordID)
					return true
				})
				if namesRecord && strings.Contains(r.Message, "approval") && r.Level == c.level && !strings.Contains(r.Message, "refused") {
					found = true
				}
			}
			if !found {
				t.Errorf("the run wrote no %v line naming the record's approval without calling it refused; want one", c.level)
			}
		})
	}
}
