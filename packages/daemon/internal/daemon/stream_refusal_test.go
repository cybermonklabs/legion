package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// streamRefusal is the error of the 409 the listener answers a send or publish whose forward a
// sealed stream refused, as writeBusUnavailable writes it (packages/envoy/cmd/listener/api.go):
// streamRefusedPrefix, then the bus's own error.
const streamRefusal = "envoy: the message bus stream refused the message, and refuses it however often it is sent: " +
	"bus: the stream did not acknowledge storing the forward: nats: invalid operation on sealed stream"

// streamRefusedLine holds that logs, the JSON lines an executor wrote, carry exactly one ERROR
// line, that it carries the stream's refusal under `refusal`, and that it carries each of fields
// with the value given. It returns the line.
func streamRefusedLine(t *testing.T, logs string, fields map[string]any) map[string]any {
	t.Helper()
	lines := loggedLines(t, logs, `"level":"ERROR"`)
	if len(lines) != 1 {
		t.Fatalf("ERROR lines = %v in\n%s\nwant exactly one, naming the stream's refusal", lines, logs)
	}
	line := lines[0]
	if line["refusal"] != streamRefusal {
		t.Fatalf("the ERROR line %v carries refusal %q, want the stream's refusal %q", line, line["refusal"], streamRefusal)
	}
	for key, want := range fields {
		if line[key] != want {
			t.Fatalf("the ERROR line %v carries %s = %v, want %v", line, key, line[key], want)
		}
	}
	return line
}

// A role message whose forward the message bus stream refuses is answered 409 under the
// listener's streamRefusedPrefix, with no Retry-After: the stream refuses that message however
// often it is sent (bus.StreamRefusesMessage: a sealed stream, a message past its maxima), until
// an operator changes the stream. So the row ends at the first answer, with one ERROR line naming
// the row, its issue, its role and the stream's refusal, rather than being retried for as long as
// the stream stands. The publisher is the daemon's own HTTPPublisher, against a listener answering
// the body writeBusUnavailable writes, since the row's fate turns on how that body is read.
func TestARoleMessageTheStreamRefusesEndsWithOneError(t *testing.T) {
	var publishes atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/messages/publish" {
			http.NotFound(w, req)
			return
		}
		publishes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"`+streamRefusal+`"}`)
	}))
	t.Cleanup(listener.Close)
	row := mustOutboxRow(t, "LEGION-2", record.RoleMessage{Role: "implementer", Body: "CI settled at head"}, time.Now())
	row.ID = 63
	logs := &syncBuffer{}
	runner := &outbox{log: jsonLogger(logs), notices: notify.New(listener.URL, "")}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute a role message the stream refused = %v, want the row ended: the stream refuses it however often it is sent", err)
	}
	if got := publishes.Load(); got != 1 {
		t.Fatalf("the listener was sent %d publishes, want the one the stream refused", got)
	}
	streamRefusedLine(t, logs.String(), map[string]any{"row": float64(row.ID), "issue": row.Issue, "role": "implementer"})
}
