// packages/envoy/internal/cipher/judge/judge_bound_test.go
// Round 4's B8 on cybermonklabs/legion#4135: every read of Dispatch a run makes is under the one
// bound, requests.JudgeDispatch, and the run's request.judged row is written on a context of its
// own, so neither a slow Dispatch nor the run's own deadline can leave a run that asked the model
// without its row.
package judge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/requests"
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
// failure too: a list without them cannot say whether they are bounded.
func TestEveryDispatchReadOfARunIsUnderTheOneBound(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	opts := testOptions(t, dispatch, model, machine)
	transport := &boundTransport{inner: opts.Dispatch.HTTP.Transport}
	client := *opts.Dispatch.HTTP
	client.Transport = transport
	opts.Dispatch.HTTP = &client
	New(t.Context(), opts).consider(t.Context(), pending())

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
// Dispatch could not be asked whether the request is still the judge's is logged as that, at warn,
// and not as a refusal the judge should never have reached, which is an error.
func TestADispatchFailureAtTheApprovalIsNoRefusal(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.applyErr = fmt.Errorf("%w: dispatch answered 503", requests.ErrProjectAccessUnavailable)
	New(t.Context(), testOptions(t, dispatch, model, machine)).write(t.Context(), pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})

	if _, logged := machine.rows(); len(logged) != 1 {
		t.Fatalf("the run logged %d rows; want the failed approval's row", len(logged))
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "record=record-1") && strings.Contains(l, "approval") {
			line = l
		}
	}
	if !strings.Contains(line, "level=WARN") || strings.Contains(line, "refused") || !errors.Is(machine.applyErr, requests.ErrProjectAccessUnavailable) {
		t.Errorf("the approval's log line is %q; want a WARN that says Dispatch could not be asked, not that the approval was refused", line)
	}
}
