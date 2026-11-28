// check_states_fence_test.go holds the test handoff's wait for the states its results caused (the
// fence, awaitCheckStatesSettled) and the serial each state event and the handoff carry. The rig
// is check_states_test.go's.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestATestHandoffComesAfterTheStatesItsResultsCaused: with the loop running, the tester records a
// pass of k1 at H1 and at once writes its test handoff. The handoff answers after the settle: the
// event of k1 at H1 has a sequence below handoff.recorded's, and the handoff answers its serial as
// check_states_serial. A plan handoff waits for nothing and answers no serial. With the loop
// stopped and a fence of 1 s, a test handoff after a recorded result answers 503
// CHECK_STATES_UNSETTLED and records nothing, and a mark due 5 minutes on, as a failed read
// leaves, does not hold it.
func TestATestHandoffComesAfterTheStatesItsResultsCaused(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	settleOwed(t, rig)

	// The settle of the tester's result is held after it reads its marks until the handoff's
	// fence is waiting, so a fence that did not wait would answer the serial from before it.
	var gated atomic.Bool
	hold := newHoldOnce(t)
	rig.settler.hooks.afterMarks = func(issue string) {
		if gated.Load() {
			hold.hook(issue)
		}
	}
	fenceWaits := make(chan struct{}, 1)
	rig.served.checkStatesFenceWaits = func(string) {
		select {
		case fenceWaits <- struct{}{}:
		default:
		}
	}
	wake := listenForCheckStates(t, rig)
	stop := runCheckStatesLoop(t, rig.settler, wake)
	time.Sleep(4 * rig.settler.schedule.minGap)

	gated.Store(true)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	hold.await(t, "the settle of the tester's pass")
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- postHandoff(t, rig.handler, rig.issue, "test", `{"tested":"H1"}`) }()
	var response *httptest.ResponseRecorder
	select {
	case response = <-answered:
		hold.release()
	case <-fenceWaits:
		hold.release()
		select {
		case response = <-answered:
		case <-time.After(10 * time.Second):
			t.Fatal("the test handoff did not answer within 10s of the settle")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the test handoff neither answered nor waited within 10s")
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("the test handoff: status=%d body=%s", response.Code, response.Body.String())
	}
	handoff := decodeBody[model.HandoffSummary](t, response)
	events := awaitEvents(t, rig, 0, 2*time.Second, func(events []checkStatesEvent) bool {
		return len(eventsOfCheck(events, "k1", eachHeadH1)) == 2
	})
	passed := eventsOfCheck(events, "k1", eachHeadH1)
	if len(passed) != 2 || passed[1].state() != "yes" {
		t.Fatalf("the events of k1 at H1 are %v, want no and then yes", eventLines(passed))
	}
	var recordedSeq int64
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select seq from events where issue_key = $1 and type = 'handoff.recorded' and payload->>'handoff_id' = $2`,
		rig.issue, handoff.ID).Scan(&recordedSeq); err != nil {
		t.Fatalf("read handoff.recorded of %s: %v", handoff.ID, err)
	}
	read := readHandoffJSON(t, rig, handoff.ID)
	serial, held := read["check_states_serial"]
	if passed[1].Seq >= recordedSeq || !held || string(serial) != strconv.FormatInt(passed[1].Payload.Serial, 10) {
		t.Fatalf("the state event of the tester's pass has seq %d and serial %d, and handoff.recorded seq %d with check_states_serial %s; want the state event first and the serial its own",
			passed[1].Seq, passed[1].Payload.Serial, recordedSeq, serial)
	}

	planned := postHandoff(t, rig.handler, rig.issue, "plan", `{"plan":"one"}`)
	if planned.Code != http.StatusCreated {
		t.Fatalf("the plan handoff: status=%d body=%s", planned.Code, planned.Body.String())
	}
	if _, held := readHandoffJSON(t, rig, decodeBody[model.HandoffSummary](t, planned).ID)["check_states_serial"]; held {
		t.Fatal("the plan handoff answers a check_states_serial")
	}

	stop()
	rig.served.checkStatesSchedule.fenceWait = time.Second
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH1)
	refused := postHandoff(t, rig.handler, rig.issue, "test", `{"tested":"H1"}`)
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusServiceUnavailable || code != "CHECK_STATES_UNSETTLED" {
		t.Fatalf("a test handoff with the settler stopped: status=%d body=%s, want 503 CHECK_STATES_UNSETTLED", refused.Code, refused.Body.String())
	}
	var tests int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from handoffs where issue_key = $1 and phase = 'test'`, rig.issue).Scan(&tests); err != nil {
		t.Fatalf("count the test handoffs: %v", err)
	}
	if tests != 1 {
		t.Fatalf("%s holds %d test handoffs after the refusal, want the one written before it", rig.issue, tests)
	}

	settleOwed(t, rig)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into check_states_owed (issue_key, due_at, failures) values ($1, now() + interval '5 minutes', 1)`, rig.issue); err != nil {
		t.Fatalf("leave a retry mark: %v", err)
	}
	if again := postHandoff(t, rig.handler, rig.issue, "test", `{"tested":"H1"}`); again.Code != http.StatusCreated {
		t.Fatalf("a test handoff beside a mark due 5 minutes on: status=%d body=%s, want 201", again.Code, again.Body.String())
	}
}

// readHandoffJSON is GET /api/v1/handoffs/{id}'s answer, member by member.
func readHandoffJSON(t *testing.T, rig *checkStatesRig, id string) map[string]json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/handoffs/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read handoff %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[map[string]json.RawMessage](t, response)
}

// TestTheSerialCountsEachStateEventOfTheIssue: the events of two heads carry the serials 1, 2, 3
// and on, in the order the issue's log holds them, with no gap and no repeat.
func TestTheSerialCountsEachStateEventOfTheIssue(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH3, "open", rig.issue)
	settleOwed(t, rig)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	settleOwed(t, rig)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH3)
	settleOwed(t, rig)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH2, "open", rig.issue)
	settleOwed(t, rig)

	var serials []int64
	events := checkStateEventsAfter(t, rig.database, 0)
	slices.SortFunc(events, func(a, b checkStatesEvent) int { return int(a.Seq - b.Seq) })
	for _, event := range events {
		serials = append(serials, event.Payload.Serial)
	}
	if len(serials) < 8 {
		t.Fatalf("the two heads published %d events, want at least 8: %v", len(serials), eventLines(events))
	}
	for index, serial := range serials {
		if serial != int64(index+1) {
			t.Fatalf("the serials in the log's order are %v, want 1 to %d", serials, len(serials))
		}
	}
}
