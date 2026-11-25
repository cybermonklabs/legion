package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestTheOutboxDecidesDispatchsCodeAsItCameBesideAWriteThatQuotesIt: Dispatch refuses a write with
// one of its codes, and the write quotes that code, as a message about an earlier refusal does. The
// write's body is a value the request carried, so the rule cuts the run of it the code holds from
// the code's reading. Each of the outbox's decisions on the code still reads it as it came
// (dispatch.Error's Code): the refusals that end a row for good - a message post's, a
// design-gates or role-holders publish's and a record's 404 ISSUE_NOT_FOUND, a role-holders
// publish's coded 400, a record's 403 STEP_WRITER and 409 STEP_OTHER_ISSUE or PLAN_OTHER_ISSUE -
// and a status write's 409 ISSUE_CHANGED, which reads the issue again rather than fail the row. In
// the control the write names no code.
func TestTheOutboxDecidesDispatchsCodeAsItCameBesideAWriteThatQuotesIt(t *testing.T) {
	refused := func(t *testing.T, status int, code, message string) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"` + code + `","error":"Dispatch refused the write"}`))
		}))
		t.Cleanup(server.Close)
		return dispatch.New(server.URL, dispatch.BearerTransport(githubtest.InstallationToken(t.Name()), nil)).PostMessage(context.Background(), "LEGION-9", message)
	}
	ends := func(kind record.OutboxKind) func(error) bool {
		return func(err error) bool { return permanentRefusal(record.OutboxRow{Kind: kind}, err) }
	}
	// rereads is whether a status write the issue changed under reads the issue again, and finds
	// it at the status the row writes, rather than fail the row on the refusal.
	rereads := func(err error) bool {
		client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-9", Status: "done"}, statusErr: err}
		runner := &outbox{dispatch: client, log: quietLogger()}
		read := dispatch.Issue{Key: "LEGION-9", Status: "in_progress"}
		return runner.writeStatus(context.Background(), record.OutboxRow{ID: 1, Issue: "LEGION-9"}, record.OrdinaryStatusWrite("done", "in_progress", ""), nil, read) == nil
	}
	for _, c := range []struct {
		name    string
		status  int
		code    string
		decided func(error) bool
	}{
		{"a message post's 404", http.StatusNotFound, dispatch.IssueNotFound, ends(record.OutboxKindDispatchMessage)},
		{"a design-gates publish's 404", http.StatusNotFound, dispatch.IssueNotFound, ends(record.OutboxKindDesignGatesPublish)},
		{"a role-holders publish's 404", http.StatusNotFound, dispatch.IssueNotFound, ends(record.OutboxKindRoleHoldersPublish)},
		{"a role-holders publish's coded 400", http.StatusBadRequest, "INVALID_ROLE_HOLDER", ends(record.OutboxKindRoleHoldersPublish)},
		{"a step record's 404", http.StatusNotFound, dispatch.IssueNotFound, recordRefused},
		{"a step record's 403", http.StatusForbidden, steprecord.CodeWriter, recordRefused},
		{"a step record's 409", http.StatusConflict, steprecord.CodeOtherIssue, recordRefused},
		{"a plan record's 409", http.StatusConflict, steprecord.CodePlanOtherIssue, recordRefused},
		{"a status write's 409", http.StatusConflict, "ISSUE_CHANGED", rereads},
	} {
		for _, row := range []struct{ name, message string }{
			{"control", "the step finished; nothing was refused"},
			{"quoted", "the last write was refused: " + c.code + ", so it is posted again"},
		} {
			t.Run(c.name+" "+c.code+"/"+row.name, func(t *testing.T) {
				err := refused(t, c.status, c.code, row.message)
				if !c.decided(err) {
					t.Errorf("Dispatch's %d %s to a write %q = %v, want it decided on Dispatch's code as it came", c.status, c.code, row.message, err)
				}
			})
		}
	}
}
