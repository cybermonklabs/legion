// expired_requests_test.go pins the two answers that keep an expired credential request in its
// approver's list until somebody dismisses it (dispatch://INFRA-3921): GET /v1/pending's ?state=
// and each row's state and deadline, and POST /v1/credential-requests/{record}/dismiss. A record
// is made expired the way time makes it: a secret request's pending_expires_at moved into the
// past with no sweep run, and a machine login written with a deadline already gone, its record
// being append-only.
package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// wireDismissal mirrors the dismiss route's answer.
type wireDismissal struct {
	RecordID    string    `json:"record_id"`
	DismissedBy string    `json:"dismissed_by"`
	DismissedAt time.Time `json:"dismissed_at"`
}

// secretRequestOnBox asks testApprover for DEEL_API_KEY from a session enrolled on a box of its
// own, so each call opens a record of its own rather than joining another box's request, and
// answers the request's id, its record's id and its deadline.
func (ts *testServer) secretRequestOnBox(t *testing.T, box string) (requestID, recordID string, deadline time.Time) {
	t.Helper()
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name()+"-"+box, testApprover)
	created := ts.requestAs(t, key, enrollmentID, "DEEL_API_KEY")
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests on %s = %+v, want a pending request on a record", box, created)
	}
	if err := ts.Store.Pool.QueryRow(context.Background(), `select pending_expires_at from requests where id=$1`, created.RequestID).
		Scan(&deadline); err != nil {
		t.Fatalf("read the deadline of the request on %s: %v", box, err)
	}
	return created.RequestID, *created.RecordID, deadline
}

// overdue moves request requestID's deadline an hour into the past and runs no sweep, so its
// request row still reads pending while every decision of it already answers that it expired.
// It answers the new deadline.
func (ts *testServer) overdue(t *testing.T, requestID string) time.Time {
	t.Helper()
	var deadline time.Time
	if err := ts.Store.Pool.QueryRow(context.Background(), `update requests set pending_expires_at = now() - interval '1 hour'
		where id=$1 returning pending_expires_at`, requestID).Scan(&deadline); err != nil {
		t.Fatalf("move request %s past its deadline: %v", requestID, err)
	}
	return deadline
}

// expiredMachineLogin writes a person's machine login for testApprover whose deadline passed an
// hour ago, as machine.Service.LoginDevice writes one, and answers its record's id and its
// deadline.
func (ts *testServer) expiredMachineLogin(t *testing.T, host string) (recordID string, deadline time.Time) {
	t.Helper()
	body := record.Body{
		Request:         signMachineLoginRequest(t, newSigningKey(t), ts.URL, testApprover, host, ""),
		Approver:        testApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(3600),
		RulesVersion:    "rules-v1",
		ExpiresAt:       time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		Code:            "EXPD-WXYZ",
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at)
		values ($1,$2,'launcher_credential',$3,$4,$5)`, body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatalf("insert the expired machine login for %s: %v", host, err)
	}
	return body.ID(), body.ExpiresAt
}

// pendingOf is GET /v1/pending for testApprover with query after its approver, which must answer
// 200.
func (ts *testServer) pendingOf(t *testing.T, query string) []wirePendingEntry {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover+query, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending?approver=%s%s = %d, want 200: %s", testApprover, query, status, body)
	}
	return decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending
}

// listed is each row's record id and state, in the list's order.
func listed(rows []wirePendingEntry) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.RecordID + "=" + row.State
	}
	return out
}

// TestPendingListsEachRowsStateAndTheStatesAskedFor: with no ?state=, GET /v1/pending lists what
// it listed before a record had a state, every record still pending by what is stored, newest
// first, a secret request past its deadline included until the sweep ends it, and each row now
// says its state and its deadline. ?state=pending and ?state=expired ask for those states, the
// pending rows first and each state newest first, and the expired rows stay listed after the
// sweep; any other value is 400 STATE_INPUT.
func TestPendingListsEachRowsStateAndTheStatesAskedFor(t *testing.T) {
	ts := newTestServer(t)
	lateRequest, late, _ := ts.secretRequestOnBox(t, "late-box")
	lateDeadline := ts.overdue(t, lateRequest)
	_, live, liveDeadline := ts.secretRequestOnBox(t, "live-box")
	login, loginDeadline := ts.expiredMachineLogin(t, "example-host-expired")
	deadlines := map[string]time.Time{late: lateDeadline, live: liveDeadline, login: loginDeadline}
	expired, pending := func(id string) string { return id + "=expired" }, func(id string) string { return id + "=pending" }

	check := func(t *testing.T, query string, want []string) {
		t.Helper()
		rows := ts.pendingOf(t, query)
		if got := listed(rows); !slices.Equal(got, want) {
			t.Fatalf("GET /v1/pending%s lists %v, want %v", query, got, want)
		}
		for _, row := range rows {
			if row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadlines[row.RecordID]) {
				t.Errorf("GET /v1/pending%s: row %s has expires_at %v, want its deadline %s", query, row.RecordID, row.ExpiresAt, deadlines[row.RecordID])
			}
			if wantPage := row.Kind == record.KindAgentSecret; (row.ApproveURL != nil) != wantPage {
				t.Errorf("GET /v1/pending%s: row %s (%s) has approve_url %v, want one for a secret request alone", query, row.RecordID, row.Kind, row.ApproveURL)
			}
		}
	}

	t.Run("no state lists every record still pending by what is stored, each with its state", func(t *testing.T) {
		check(t, "", []string{expired(login), pending(live), expired(late)})
	})
	t.Run("both states list the pending rows first, then the expired, each newest first", func(t *testing.T) {
		check(t, "&state=pending&state=expired", []string{pending(live), expired(login), expired(late)})
		check(t, "&state=expired&state=pending", []string{pending(live), expired(login), expired(late)})
	})
	t.Run("one state lists that state alone", func(t *testing.T) {
		check(t, "&state=pending", []string{pending(live)})
		check(t, "&state=expired", []string{expired(login), expired(late)})
	})
	t.Run("any other state is 400 STATE_INPUT", func(t *testing.T) {
		for _, query := range []string{"&state=dismissed", "&state=pending&state=Expired", "&state="} {
			status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover+query, nil)
			if refusal := decode[wireError](t, body); status != http.StatusBadRequest || refusal.Code != "STATE_INPUT" || !strings.Contains(refusal.Error, "pending") {
				t.Errorf("GET /v1/pending%s = %d %s, want 400 STATE_INPUT naming the states it takes", query, status, body)
			}
		}
	})

	if n, err := ts.machine.ExpirePending(context.Background(), time.Now()); err != nil || n != 1 {
		t.Fatalf("ExpirePending = %d, %v; want the overdue secret request alone expired", n, err)
	}
	t.Run("after the sweep, no state leaves the swept request out and the states list keeps it", func(t *testing.T) {
		check(t, "", []string{expired(login), pending(live)})
		check(t, "&state=pending&state=expired", []string{pending(live), expired(login), expired(late)})
	})
}

// TestDismissAnswersEachOutcomeAndTakesTheRecordOffTheList drives every answer of POST
// /v1/credential-requests/{record}/dismiss: a malformed id, no approver, an id no record has, a
// login whose list does not hold the record, a record that can still be decided and one that was
// decided are refused and write nothing; an expired secret request and an expired machine login
// are dismissed, a second dismissal answers the first one's values, and a list asking for both
// states holds neither of them afterwards, while the request that can still be decided stays.
func TestDismissAnswersEachOutcomeAndTakesTheRecordOffTheList(t *testing.T) {
	ts := newTestServer(t)
	lateRequest, late, _ := ts.secretRequestOnBox(t, "late-box")
	ts.overdue(t, lateRequest)
	_, live, _ := ts.secretRequestOnBox(t, "live-box")
	_, denied, _ := ts.secretRequestOnBox(t, "denied-box")
	ts.decide(t, testApprover, denied, false)
	login, _ := ts.expiredMachineLogin(t, "example-host-expired")
	dismiss := func(t *testing.T, recordID string, body any) (int, []byte) {
		t.Helper()
		return ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/dismiss", body)
	}
	dismissals := func(t *testing.T) []string {
		t.Helper()
		return ts.column(t, `select record_id || '=' || dismissed_by from credential_request_dismissals order by dismissed_at, record_id`)
	}
	asApprover := map[string]any{"approver": testApprover}

	for _, refused := range []struct {
		name, recordID string
		body           any
		status         int
		code           string
	}{
		{"a malformed record id", "not-a-record", asApprover, http.StatusBadRequest, "RECORD_ID_INPUT"},
		{"no approver", late, map[string]any{"approver": " "}, http.StatusBadRequest, "APPROVER_REQUIRED"},
		{"an id no record has", strings.Repeat("0", 64), asApprover, http.StatusNotFound, "NOT_FOUND"},
		{"a login whose list does not hold the record", late, map[string]any{"approver": "mallory@example.com"}, http.StatusForbidden, "NOT_APPROVER"},
		{"a request that can still be decided", live, asApprover, http.StatusConflict, "RECORD_PENDING"},
		{"a request that was denied", denied, asApprover, http.StatusConflict, "RECORD_TERMINAL"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			status, body := dismiss(t, refused.recordID, refused.body)
			if got := decode[wireError](t, body); status != refused.status || got.Code != refused.code {
				t.Fatalf("dismiss %s = %d %s, want %d %s", refused.name, status, body, refused.status, refused.code)
			}
		})
	}
	if got := dismissals(t); len(got) != 0 {
		t.Fatalf("dismissals after the refusals = %v, want none: a refused dismissal writes nothing", got)
	}

	dismissedBy := record.CanonicalLogin(testApprover)
	var first wireDismissal
	t.Run("an expired secret request is dismissed, and a second dismissal answers the first", func(t *testing.T) {
		status, body := dismiss(t, late, asApprover)
		first = decode[wireDismissal](t, body)
		if status != http.StatusOK || first.RecordID != late || first.DismissedBy != dismissedBy || first.DismissedAt.IsZero() {
			t.Fatalf("dismiss the expired secret request = %d %s, want 200 naming it, dismissed by %s, with its time", status, body, dismissedBy)
		}
		status, body = dismiss(t, late, asApprover)
		if again := decode[wireDismissal](t, body); status != http.StatusOK || again.RecordID != first.RecordID ||
			again.DismissedBy != first.DismissedBy || !again.DismissedAt.Equal(first.DismissedAt) {
			t.Fatalf("dismiss it again = %d %s, want 200 with the first dismissal's values %+v", status, body, first)
		}
	})
	t.Run("an expired machine login is dismissed", func(t *testing.T) {
		status, body := dismiss(t, login, asApprover)
		if got := decode[wireDismissal](t, body); status != http.StatusOK || got.RecordID != login || got.DismissedBy != dismissedBy || got.DismissedAt.IsZero() {
			t.Fatalf("dismiss the expired machine login = %d %s, want 200 naming it, dismissed by %s", status, body, dismissedBy)
		}
	})
	if got, want := dismissals(t), []string{late + "=" + dismissedBy, login + "=" + dismissedBy}; !slices.Equal(got, want) {
		t.Fatalf("dismissals = %v, want one row for each dismissed record, %v", got, want)
	}
	if got, want := listed(ts.pendingOf(t, "&state=pending&state=expired")), []string{live + "=pending"}; !slices.Equal(got, want) {
		t.Fatalf("GET /v1/pending with both states after the dismissals lists %v, want %v: a dismissed record leaves the list", got, want)
	}
}
