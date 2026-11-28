package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// messageRefusedText is the listener's own error in the refusals messageRefusal makes.
const messageRefusedText = "envoy: the message is larger than the listener takes"

// messageRefusal is the error the daemon's own publisher (notify.HTTPPublisher) answers when the
// listener refuses the message for itself with status, a 400 or a 413 carrying the listener's own
// error (notify.ErrMessageRefused), which no retry of the same message cures.
func messageRefusal(t *testing.T, status int) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, messageRefusedText)
	}))
	t.Cleanup(server.Close)
	_, err := notify.New(server.URL, "").Publish(context.Background(), notify.RoleTopicPrefix+"legion-legion-legion-208-merger",
		"the refused message", nil, record.OutboxKey(1))
	var refused notify.ErrMessageRefused
	if !errors.As(err, &refused) {
		t.Fatalf("the publish refused %d answered %v, want a notify.ErrMessageRefused", status, err)
	}
	return err
}

// A row of each of the nine kinds whose role's claim is ready, and whose publish the listener
// refuses for itself (a 400 or a 413 with the listener's own error, notify.ErrMessageRefused), ends
// undelivered at that run, after that one publish: no retry of the same message cures the refusal,
// so no claim can take the row and it is not sent again on the capped backoff. The end is one line
// at Error naming the row's issue and kind, a role, its attempts and the listener's refusal; the row
// took no wait, so it writes no wait line (dispatch://INFRA-4131/spec#b-r-capped, the architect's
// ruling dispatch://INFRA-4131/comment/a6f3961a-6c17-45d2-ba41-d7f7d30ce2d4).
func TestRoleWaitCapEndsAMessageTheListenerRefusesForItself(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, tc, messageRefusal(t, status))
					h.claim(supervise.StateWorking, nil)
					rig := h.queue()
					rig.endsAtItsNextRun()
					if got := h.publisher.count(); got != 1 {
						t.Fatalf("a row whose message the listener refused %d published %d times, want the one refused publish", status, got)
					}
					logs := rig.logs.String()
					var errorLines []map[string]any
					for _, line := range rowLines(t, logs, rig.id) {
						if line["msg"] == wantWaitStartLine {
							t.Errorf("the refused row wrote the wait line %v, want no wait:\n%s", line, logs)
						}
						switch line["level"] {
						case slog.LevelError.String():
							errorLines = append(errorLines, line)
						case slog.LevelWarn.String():
							t.Errorf("the refused row wrote a WARN line %v, want its end at Error alone:\n%s", line, logs)
						}
					}
					if len(errorLines) != 1 {
						t.Fatalf("the refused row wrote %d lines at Error, want one that says it ended:\n%s", len(errorLines), logs)
					}
					end := errorLines[0]
					if end["issue"] != "LEGION-208" || end["kind"] != string(tc.kind) {
						t.Errorf("the end line %v names issue %v and kind %v, want LEGION-208 and %s", end, end["issue"], end["kind"], tc.kind)
					}
					if role, _ := end["role"].(string); role == "" {
						t.Errorf("the end line %v names no role", end)
					}
					if _, named := end["attempts"].(float64); !named {
						t.Errorf("the end line %v names no attempts", end)
					}
					if !strings.Contains(fmt.Sprint(end), messageRefusedText) {
						t.Errorf("the end line %v does not name the listener's refusal %q", end, messageRefusedText)
					}
				})
			}
		})
	}
}
