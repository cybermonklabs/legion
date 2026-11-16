package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// TestAReleaseBodyNamesTheCommandItStarts: POST /v1/grants/{id}/values takes the command the
// release starts as "command", beside a github_app narrowing or alone, and the reason the
// session's own call gave as "reason", and the broker logs both with the reason the grant's
// request gave, every value the release handed out replaced by "[redacted]". A command that is
// not a list of strings refuses the whole body, as any field of the wrong shape does, and
// releases nothing.
func TestAReleaseBodyNamesTheCommandItStarts(t *testing.T) {
	logged := policytest.CaptureLog(t)
	s := newNarrowingServer(t)
	plain := s.grant(t, "PLAIN_KEY")

	for _, row := range []struct {
		name    string
		grantID string
		body    map[string]any
		logs    string
	}{
		{"alone", plain, map[string]any{"command": []string{"sh", "-c", "echo plain-v1"}}, `command="[sh -c echo [redacted]]"`},
		{"beside a narrowing", s.grantID, map[string]any{"github_app": map[string]any{"owner": "ada"}, "command": []string{"gh", "pr", "list"}}, `command="[gh pr list]"`},
		{"with its call's own reason", plain, map[string]any{"command": []string{"sh", "-c", "true"}, "reason": "renew the deploy key"}, `call_reason="renew the deploy key" command="[sh -c true]"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if status, body := s.values(t, row.grantID, row.body); status != http.StatusOK {
				t.Fatalf("POST values = %d %s, want 200", status, body)
			}
			want := 0
			for line := range strings.Lines(logged.String()) {
				if strings.Contains(line, "INFO "+requests.ReleasedMessage+" grant="+row.grantID) && strings.Contains(line, `reason="need it"`) && strings.Contains(line, row.logs) {
					want++
				}
			}
			if want != 1 {
				t.Fatalf("logged %d %q lines for the grant naming its reason and %s, want 1:\n%s", want, requests.ReleasedMessage, row.logs, logged.String())
			}
		})
	}

	status, body := s.values(t, plain, map[string]any{"command": "sh -c true"})
	if reply := decode[wireError](t, body); status != http.StatusBadRequest || reply.Code != "INVALID_NARROWING" || !strings.Contains(reply.Error, "command") {
		t.Fatalf("POST values with a command that is no list = %d %s, want 400 INVALID_NARROWING naming command", status, body)
	}
	if strings.Contains(string(body), "plain-v1") {
		t.Fatalf("the refused body answered the stored value: %s", body)
	}
}
