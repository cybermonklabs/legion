package claudebridge

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// modelRequestRig drives stream lines into the reporter and reads the wire requests at its daemon.
type modelRequestRig struct {
	reporter *modelRequests
	clock    *windowClock
	logged   *lockedBuffer
	posts    chan []string
}

func newModelRequestRig(t *testing.T, answer func(http.ResponseWriter, []string, int)) *modelRequestRig {
	t.Helper()
	r := &modelRequestRig{clock: &windowClock{at: time.Now().UTC()}, logged: &lockedBuffer{}, posts: make(chan []string, 8)}
	var mu sync.Mutex
	attempt := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body claim.ModelRequestsRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("read model request batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ids := make([]string, 0, len(body.Requests))
		for _, entry := range body.Requests {
			ids = append(ids, entry.ID)
		}
		mu.Lock()
		defer mu.Unlock()
		attempt++
		select {
		case r.posts <- ids:
		default:
			t.Error("reporter sent more batches than the test could read")
		}
		if answer != nil {
			answer(w, ids, attempt)
		} else {
			_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
		}
	}))
	t.Cleanup(daemon.Close)
	cfg := Config{DaemonURL: daemon.URL, Generation: 4, Clock: r.clock}
	r.reporter = startModelRequests(cfg, &daemonPoster{
		url: daemon.URL, generation: cfg.Generation, bootToken: testBootToken,
		client: daemon.Client(), clock: cfg.Clock, log: log.New(r.logged, "", 0),
	})
	return r
}

func (r *modelRequestRig) assistant(id string, at time.Time) {
	r.reporter.read(streamLine{
		kind: "assistant", session: capturedSession, timestamp: at.Format(time.RFC3339Nano),
		message: json.RawMessage(fmt.Sprintf(`{"id":%q,"model":%q,"usage":{"input_tokens":1}}`, id, capturedModel)),
	})
}

func (r *modelRequestRig) result() { r.reporter.read(streamLine{kind: "result"}) }

// expectBatch detects both loss and re-sends of a kept ID.
func (r *modelRequestRig) expectBatch(t *testing.T, want []string) {
	t.Helper()
	if got := testwait.Receive(t, "the model request batch", r.posts); !slices.Equal(got, want) {
		t.Fatalf("batch has %d IDs, want %d; got %q, want %q", len(got), len(want), got, want)
	}
}

func (r *modelRequestRig) enqueueBatch(n int, prefix string) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%03d", prefix, i)
		r.assistant(ids[i], r.clock.Now())
	}
	return ids
}

func (r *modelRequestRig) awaitAnswer(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the daemon's answer to be processed", func() bool {
		r.reporter.mu.Lock()
		defer r.reporter.mu.Unlock()
		return !r.reporter.sending
	})
}

func TestModelRequestsWindowFlushesOpenRequestDuringLongToolCall(t *testing.T) {
	r := newModelRequestRig(t, nil)
	r.assistant("tool-call", r.clock.Now())
	if fired := r.clock.fire(); fired != 1 {
		t.Fatalf("fired %d windows while the tool ran, want 1", fired)
	}
	r.expectBatch(t, []string{"tool-call"})
	r.awaitAnswer(t)
	r.assistant("tool-call", r.clock.Now())
	r.assistant("after-tool", r.clock.Now())
	r.result()
	r.expectBatch(t, []string{"after-tool"})
}

func TestModelRequestsReadsFullMaxBatchKeptAnswer(t *testing.T) {
	r := newModelRequestRig(t, nil)
	ids := r.enqueueBatch(claim.MaxModelRequests, "refused-"+capturedSession+"-0000000000000000")
	r.result()
	r.expectBatch(t, ids)
	r.awaitAnswer(t)
	r.assistant("next", r.clock.Now())
	r.result()
	r.expectBatch(t, []string{"next"})
}

func TestModelRequestsBacksOffOnDaemonFailureWithOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"server error", "unavailable", http.StatusInternalServerError},
		{"unreadable answer", "not JSON", http.StatusOK},
		{"no progress", `{"kept":[]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
				if attempt == 1 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
			})
			ids := r.enqueueBatch(claim.MaxModelRequests+5, "req")
			if fired := r.clock.fire(); fired != 1 {
				t.Fatalf("fired %d initial windows, want 1", fired)
			}
			r.expectBatch(t, ids[:claim.MaxModelRequests])
			r.awaitAnswer(t)
			select {
			case extra := <-r.posts:
				t.Fatalf("sent %d IDs without waiting for a retry window", len(extra))
			default:
			}
			if fired := r.clock.fire(); fired != 1 {
				t.Fatalf("fired %d retry windows, want 1", fired)
			}
			r.expectBatch(t, ids[:claim.MaxModelRequests])
			r.expectBatch(t, ids[claim.MaxModelRequests:])
		})
	}
}

func TestModelRequestsDrainsOverflowImmediatelyAfterKeptAnswer(t *testing.T) {
	r := newModelRequestRig(t, nil)
	ids := r.enqueueBatch(claim.MaxModelRequests+5, "req")
	r.result()
	r.expectBatch(t, ids[:claim.MaxModelRequests])
	r.expectBatch(t, ids[claim.MaxModelRequests:])
}

func TestModelRequestsDropsRequestsPastKeepWindow(t *testing.T) {
	r := newModelRequestRig(t, func(w http.ResponseWriter, ids []string, attempt int) {
		if attempt == 1 {
			ids = nil
		}
		_ = json.NewEncoder(w).Encode(claim.ModelRequestsResponse{Kept: ids})
	})
	r.assistant("old", r.clock.Now().Add(-time.Hour+time.Second))
	r.result()
	r.expectBatch(t, []string{"old"})
	// The request expires while waiting after the first answer did not keep it.
	r.awaitAnswer(t)
	r.assistant("fresh", r.clock.Now())
	r.clock.fire()
	r.expectBatch(t, []string{"fresh"})
	if n := strings.Count(r.logged.String(), `dropped model request "old"`); n != 1 {
		t.Fatalf("logged %d drops, want one: %s", n, r.logged)
	}
}
