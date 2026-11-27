package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestAFailedAccessListWriteLogsNoPerson: when the database fails the write of a person onto or
// off a project's access list, the 500 is logged ("dispatch: API handler failed") with its error,
// and no line the server logs names any part of that person's email: the log is the operator's,
// and the email is the listed person's. A trigger fails both statements, so the request reaches
// the list write itself, past the owner check, and the failure is the database's, as a timeout
// or a lost connection would be.
//
// The access-log line ("dispatch: request") records the request's path, and this route's path
// names the person, as `/api/v1/people/{login}/nodes` names its login; that one field is the
// access log's record of the request, not an error's text, and is the one part not read here.
func TestAFailedAccessListWriteLogsNoPerson(t *testing.T) {
	const local, domain = "zq.hidden.person", "zq-secret.example"
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	handler, database := newAccessServer(t)
	if _, err := database.Pool.Exec(context.Background(), `
		create function refuse_access_list_writes() returns trigger language plpgsql as $$
		begin
			raise exception 'the database is not answering';
		end
		$$;
		create trigger refuse_access_list_writes before insert or delete on project_access
			for each statement execute function refuse_access_list_writes();
	`); err != nil {
		t.Fatalf("make the access list's writes fail: %v", err)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		call := method + " /api/v1/projects/ACC/access/people/" + local + "@" + domain
		response := dispatchRequest(t, handler, method, "/api/v1/projects/ACC/access/people/"+local+"@"+domain, nil, "alice")
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("alice: %s with the list's writes failing = %d %s, want 500", call, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), local) {
			t.Errorf("alice: %s answered %s, which names the person", call, response.Body.String())
		}
	}
	if failed := strings.Count(logged.Lines(`"msg":"dispatch: API handler failed"`), "\n"); failed != 2 {
		t.Fatalf("the server logged %d failed-handler lines for the two failed writes, want 2, so this test reads nothing; the log:\n%s", failed, logged.String())
	}
	if !strings.Contains(logged.Lines(`"msg":"dispatch: API handler failed"`), "the database is not answering") {
		t.Fatalf("the failed-handler lines do not carry the database's failure, so they are not this test's writes:\n%s", logged.String())
	}
	for line := range strings.Lines(logged.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %v: %q", err, line)
		}
		if record["msg"] == accesslog.Message {
			delete(record, "path")
		}
		text, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("encode a log line again: %v", err)
		}
		for _, part := range []string{local, domain} {
			if strings.Contains(strings.ToLower(string(text)), part) {
				t.Errorf("a log line names %q of the listed person:\n%s", part, line)
			}
		}
	}
}
