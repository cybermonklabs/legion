package api

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// The controller's set_status writes nothing to Dispatch in the request (decision 5 of
// dispatch://INFRA-4730): it reads the issue from Dispatch, and applies one status call with the
// issue, the status and the arguments it was given, and with that read as a reading of the copy,
// which the workflow decides and queues (its statusCall). The answer names what the call queued.
func TestIssueStatusAppliesTheControllersCallAndWritesNothingInTheRequest(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	grant := controllerGrant(t, h)
	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Status: "backlog", Children: intake.UnlinkArgument,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d: %s", recorder.Code, recorder.Body)
	}
	var response StatusCallResponse
	decodeInto(t, recorder, &response)
	if response.Queued == nil || len(response.Queued) != 0 {
		t.Fatalf("answer = %+v, want an empty queued list: the fact recorder queues nothing", response)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none in the request: the write is queued", statuses.writes)
	}
	recorded := facts.recorded()
	if len(recorded) != 1 {
		t.Fatalf("facts = %#v, want the one status call", recorded)
	}
	call, ok := recorded[0].(intake.StatusCall)
	want := []intake.StatusCallIssue{{Key: "LEGION-208", Status: "backlog", Children: intake.UnlinkArgument}}
	if !ok || call.Op != intake.SetStatusCall || call.Architect != "" || !reflect.DeepEqual(call.Issues, want) {
		t.Fatalf("fact = %#v, want the controller's set_status of %+v", recorded[0], want)
	}
	if len(call.Readings) != 1 || call.Readings[0].Key != "LEGION-208" || call.Readings[0].Seq != 7 {
		t.Fatalf("readings = %+v, want Dispatch's read of LEGION-208 at its sequence 7", call.Readings)
	}
}

// The arguments go with the moves they serve: children with a backlog or icebox, parent with a
// todo, each with its one or two values. A call that pairs them otherwise is refused before
// anything is read or applied.
func TestIssueStatusRefusesAnArgumentOfTheOtherMove(t *testing.T) {
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	grant := controllerGrant(t, h)
	for _, tc := range []struct {
		request IssueStatusRequest
		code    string
	}{
		{IssueStatusRequest{Status: "todo", Children: intake.IncludeArgument}, "INVALID_CHILDREN"},
		{IssueStatusRequest{Status: "backlog", Children: "move"}, "INVALID_CHILDREN"},
		{IssueStatusRequest{Status: "icebox", Parent: intake.IncludeArgument}, "INVALID_PARENT"},
		{IssueStatusRequest{Status: "todo", Parent: "unlink"}, "INVALID_PARENT"},
	} {
		tc.request.GrantID, tc.request.Issue = grant.GrantID, "LEGION-208"
		refused(t, h.request(http.MethodPost, "/legion/v1/issues/status", tc.request, nil), http.StatusBadRequest, tc.code)
	}
	if len(facts.recorded()) != 0 || len(statuses.reads) != 0 {
		t.Fatalf("refused calls applied %#v and read %v", facts.recorded(), statuses.reads)
	}
}

func TestIssueStatusReportsAnExpiredControllerGrant(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, _, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }), nil)
	grant := controllerGrant(t, h)
	now = now.Add(time.Minute)
	refused(t, h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Status: "todo",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")
}
