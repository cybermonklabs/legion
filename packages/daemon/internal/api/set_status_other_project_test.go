package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// The controller's set_status names an issue by its key, and a key of another Dispatch project is
// no issue this daemon runs: only that project's daemon writes its status, as the outbox's own
// rule says of every row (claim.KeyOfProject), and the call reads and writes nothing of another
// project (dispatchReads.readings). So a todo or a backlog on such an issue, which Dispatch
// serves, is refused before anything is queued: a 4xx, not the 500 of a call that failed, whose
// sentence names the other project or says the issue is not of this daemon's project, and no
// outbox row is keyed by that issue.
func TestSetStatusOnAnIssueOfAnotherProjectIsRefusedByName(t *testing.T) {
	const other, foreign = "ACME", "ACME-5"
	for _, tc := range []struct{ status, shown string }{{"todo", "backlog"}, {"backlog", "in_progress"}} {
		t.Run(tc.status, func(t *testing.T) {
			h, _, statuses := statusHarness(t)
			statuses.issues[foreign] = dispatch.Issue{Key: foreign, Project: other, ResolvedRepo: "acme/widgets", Title: "another project's issue",
				Status: tc.shown, Rank: "C", Labels: []string{dispatch.LegionLabel}, HandedOver: true, LastSeq: 2}
			grant := controllerGrant(t, h)
			recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: foreign, Status: tc.status}, nil)
			t.Logf("set_status %s on %s (project %s) = %d %s", tc.status, foreign, other, recorder.Code, strings.TrimSpace(recorder.Body.String()))
			var failure Failure
			_ = json.Unmarshal(recorder.Body.Bytes(), &failure)
			if recorder.Code < 400 || recorder.Code >= 500 || !strings.Contains(failure.Error, "project") ||
				(!strings.Contains(failure.Error, "project "+other) && !strings.Contains(failure.Error, testDispatchProject)) {
				t.Errorf("set_status %s on %s, an issue of project %s, answered %d %s; want a 4xx refusal whose sentence names project %s or says the issue is not of project %s",
					tc.status, foreign, other, recorder.Code, strings.TrimSpace(recorder.Body.String()), other, testDispatchProject)
			}
			var rows int
			if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1`, foreign).Scan(&rows); err != nil {
				t.Fatalf("count the outbox rows of %s: %v", foreign, err)
			}
			if rows != 0 || len(statuses.writes) != 0 {
				t.Errorf("set_status %s on %s queued %d outbox rows keyed by it and wrote %v to Dispatch, want none", tc.status, foreign, rows, statuses.writes)
			}
		})
	}
}
