package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestARunItsReaderIsNotShownCostsWhatAMissingRunCosts: GET /api/v1/delivery/runs/{id} decides
// whether its reader is shown the run in the statement that reads the run, so a run the reader is
// not shown issues, statement for statement and with the same row counts, what run 999, which no
// run holds, issues, and answers what that id answers with the id substituted. Over
// seedDeliveryReads' whole seed, alice, who does not see PRIV, reads through the route R2, a
// deploy run that shipped PRIV's P5, R3, one that shipped PRIV's P4, and C2, a PR-checks run of
// PRIV's P2; bob, a restricted member of PRIV whom the route's read gate refuses every run, reads
// below that gate C9, a PR-checks run of a pull request no row holds, which is shown to an open
// viewer alone. R1, which alice is shown, issues more statements than run 999 and answers its jobs,
// so the record can see a run read past its first statement.
//
// With every project public, alice is shown every run with its jobs, as before any project could
// be private (R9).
func TestARunItsReaderIsNotShownCostsWhatAMissingRunCosts(t *testing.T) {
	trace := &existenceTrace{}
	seed := seedDeliveryReadsWith(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), trace)}, true, true)
	const missingRun int64 = 999
	read := func(caller privateCaller, id int64, belowTheGate bool) (string, []string) {
		t.Helper()
		target := "/api/v1/delivery/runs/" + strconv.FormatInt(id, 10)
		header := seed.readerRequest(t, caller).Header
		ctx := context.WithValue(t.Context(), recordingKey{}, true)
		trace.take()
		response := httptest.NewRecorder()
		if belowTheGate {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			request.Header = header
			request.SetPathValue("id", strconv.FormatInt(id, 10))
			seed.server.getDeliveryRun(response, request)
		} else {
			response = seed.requestIn(t, ctx, caller, http.MethodGet, target, nil)
		}
		statements := slices.DeleteFunc(trace.take(), volatileStatement)
		body := strings.ReplaceAll(strings.TrimSpace(response.Body.String()), strconv.FormatInt(id, 10), "<id>")
		return fmt.Sprintf("%d %s", response.Code, body), statements
	}

	for _, row := range []struct {
		caller       privateCaller
		run          int64
		belowTheGate bool
	}{
		{callerAlice, deliveryRunR2, false},
		{callerAlice, deliveryRunR3, false},
		{callerAlice, deliveryRunC2, false},
		{callerBob, deliveryRunC9, true},
	} {
		missingAnswer, missing := read(row.caller, missingRun, row.belowTheGate)
		if !strings.HasPrefix(missingAnswer, "404 ") || len(missing) == 0 {
			t.Fatalf("%s reads run %d, which no run holds, as %s with %d statements: the comparison proves nothing", row.caller, missingRun, missingAnswer, len(missing))
		}
		hiddenAnswer, hidden := read(row.caller, row.run, row.belowTheGate)
		if hiddenAnswer != missingAnswer {
			t.Errorf("%s reads run %d, which it is not shown, as %s, want what run %d answers: %s", row.caller, row.run, hiddenAnswer, missingRun, missingAnswer)
		}
		if apart := firstDifference(hidden, missing); apart != "" {
			t.Errorf("%s reads run %d, which it is not shown, with statements apart from run %d's: %s", row.caller, row.run, missingRun, apart)
		}
	}
	_, missing := read(callerAlice, missingRun, false)
	shownAnswer, shown := read(callerAlice, deliveryRunR1, false)
	if !strings.HasPrefix(shownAnswer, "200 ") || len(shown) <= len(missing) {
		t.Errorf("alice reads R1, which she is shown, as %s with %d statements against run %d's %d: want 200 and more statements, or the record cannot see a read past the run's",
			shownAnswer, len(shown), missingRun, len(missing))
	}

	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	for _, run := range []int64{deliveryRunR1, deliveryRunR2, deliveryRunR3, deliveryRunC2, deliveryRunC9} {
		response := seed.request(t, callerAlice, http.MethodGet, "/api/v1/delivery/runs/"+strconv.FormatInt(run, 10), nil)
		if response.Code != http.StatusOK {
			t.Errorf("with every project public, alice reads run %d as %d %s, want 200", run, response.Code, response.Body.String())
			continue
		}
		if jobs := decodeBody[delivery.DeliveryRunDetailView](t, response).Jobs; len(jobs) != 1 {
			t.Errorf("with every project public, alice reads run %d with jobs %+v, want its one job", run, jobs)
		}
	}
}
