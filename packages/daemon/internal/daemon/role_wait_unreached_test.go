package daemon

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// publishFailures are the failed publishes that are no answer of the listener's own: a listener
// the daemon cannot reach, one that drops the connection before it answers, and one a proxy in
// front of it answers with a 5xx other than the 504 of receipt_timeout and delivery_failed, as a
// listener's rollout or outage makes them. Each is a failed publish of a row in scope that keeps
// the row (dispatch://INFRA-4131/spec#b-r-capped,
// dispatch://INFRA-4131/comment/19e7d9e9-97a3-42a1-b0eb-38aaa8e2f502).
var publishFailures = []string{"listener unreachable", "listener drops the connection", "listener answers 503"}

// publishFailure is the error the daemon's own publisher (notify.HTTPPublisher) answers for the
// failure name names, made by a real publish against the listener that failure describes.
func publishFailure(t *testing.T, name string) error {
	t.Helper()
	var url string
	switch name {
	case "listener unreachable":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("take a port for the unreachable listener: %v", err)
		}
		url = "http://" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatalf("close the unreachable listener's port: %v", err)
		}
	case "listener drops the connection":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("the test server's writer %T cannot be hijacked", w)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack the publish's connection: %v", err)
				return
			}
			_ = conn.Close()
		}))
		t.Cleanup(server.Close)
		url = server.URL
	case "listener answers 503":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream connect error or disconnect/reset before headers. reset reason: connection failure",
				http.StatusServiceUnavailable)
		}))
		t.Cleanup(server.Close)
		url = server.URL
	default:
		t.Fatalf("unknown publish failure %q", name)
	}
	_, err := notify.New(url, "").Publish(context.Background(), notify.RoleTopicPrefix+"legion-legion-legion-208-merger",
		"the failure's publish", nil, record.OutboxKey(1))
	if err == nil {
		t.Fatalf("the publish to a %s answered no error", name)
	}
	return err
}

// R2 and R4: a row of each of the nine kinds whose role's claim is ready, and whose every publish
// fails without reaching the listener or is answered by a 5xx other than 504, waits on the capped
// backoff like any other failed publish that keeps its row: publishes at 0, 1, 3, 7, 15, 31, 63,
// 127, 255 and 511 s, then every 300 s, not every 64 s. It logs its wait once when it starts and
// once at Error when it has waited 30 minutes, each line naming the failure as the listener's last
// answer, and every other run at Debug: no `outbox row failed` at each attempt.
func TestRoleWaitCapAndLoggedOnceForAPublishThatDoesNotReachTheListener(t *testing.T) {
	t.Parallel()
	for _, tc := range roleWaitCases {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			for _, failure := range publishFailures {
				t.Run(failure, func(t *testing.T) {
					t.Parallel()
					h := newRoleWaitHarness(t, tc, publishFailure(t, failure))
					h.claim(supervise.StateWorking, nil)
					rig := h.queue()

					rig.runTo(roleWaitRuns)
					got := publishOffsets(h.publisher.publishes, h.start)
					if len(got) != roleWaitRuns {
						t.Fatalf("publishes = %d over %d runs, want one at each run", len(got), roleWaitRuns)
					}
					if !slices.Equal(got[:10], roleWaitFirstTen) {
						t.Fatalf("first 10 publish offsets = %v, want %v", got[:10], roleWaitFirstTen)
					}
					for i := 10; i < len(got); i++ {
						if gap := got[i] - got[i-1]; gap != record.RoleWaitBackoffMost {
							t.Fatalf("gap between publish %d and %d = %s, want %s (offsets %v)", i-1, i, gap, record.RoleWaitBackoffMost, got)
						}
					}
					wantWaitLogged(t, rig.logs.String(), rig.id, "LEGION-208", tc.kind, rig.runs, waitLog{level: slog.LevelError,
						attempts: roleWaitLongAttempts, waitsFor: []any{"role", string(h.token)}, answer: true})
				})
			}
		})
	}
}
