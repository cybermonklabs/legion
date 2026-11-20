package api_test

import (
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// lastRelease is the attributes of the last requests.ReleasedMessage record capture holds for
// grantID, as the broker's handler receives them.
func lastRelease(t *testing.T, capture *logtest.Recorder, grantID string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, r := range capture.Records() {
		if r.Message != requests.ReleasedMessage {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		if attrs["grant"] == grantID {
			last = attrs
		}
	}
	if last == nil {
		t.Fatalf("no %q record names grant %s", requests.ReleasedMessage, grantID)
	}
	return last
}

// TestAReleaseBodyNamesTheCommandItStarts: POST /v1/grants/{id}/values takes the command the
// release starts as "command", beside a github_app narrowing or alone, and the reason the
// session's own call gave as "reason", and the broker logs both with the reason the grant's
// request gave, every value of 8 bytes or more the release handed out replaced by "[redacted]".
// A command that is not a list of strings refuses the whole body, as any field of the wrong shape
// does, and releases nothing.
func TestAReleaseBodyNamesTheCommandItStarts(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	s := newNarrowingServer(t)
	plain := s.grant(t, "PLAIN_KEY")

	for _, row := range []struct {
		name       string
		grantID    string
		body       map[string]any
		command    []string
		callReason string
	}{
		{"alone", plain, map[string]any{"command": []string{"sh", "-c", "echo plain-v1"}}, []string{"sh", "-c", "echo [redacted]"}, ""},
		{"beside a narrowing", s.grantID, map[string]any{"github_app": map[string]any{"owner": "ada"}, "command": []string{"gh", "pr", "list"}}, []string{"gh", "pr", "list"}, ""},
		{"with its call's own reason", plain, map[string]any{"command": []string{"sh", "-c", "true"}, "reason": "renew the deploy key"}, []string{"sh", "-c", "true"}, "renew the deploy key"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if status, body := s.values(t, row.grantID, row.body); status != http.StatusOK {
				t.Fatalf("POST values = %d %s, want 200", status, body)
			}
			last := lastRelease(t, capture, row.grantID)
			callReason, named := last["call_reason"]
			if last["reason"] != "need it" || !reflect.DeepEqual(last["command"], row.command) || named != (row.callReason != "") || (named && callReason != row.callReason) {
				t.Fatalf("the release record = %v; want reason \"need it\", command %q and call_reason %q", last, row.command, row.callReason)
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
