package api

// R7 scratch (R7DispatchCode): in production the body deadline does not follow progress.
// deadlinedBody is built with idle = s.drainTime() (bodyDrainLimit, a minute, where no test sets
// drainReadLimit) and whole = docs.WriteRequestBound (45 s). Every read that brings bytes rearms
// the deadline, but now+idle is always past the ceiling, so every rearm sets the same instant:
// first read + 45 s. A body that never stops arriving is cut there and answered BODY_TOO_SLOW.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// deadlineRecorder is a response writer whose connection records every read deadline set on it.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	set []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(at time.Time) error {
	d.set = append(d.set, at)
	return nil
}

// trickle hands out one byte per read, pausing before each, as a slow but steady link does.
type trickle struct {
	left  int
	pause time.Duration
}

func (t *trickle) Read(p []byte) (int, error) {
	if t.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(t.pause)
	t.left--
	p[0] = 'a'
	return 1, nil
}

func (t *trickle) Close() error { return nil }

func TestR7ProductionBodyDeadlineNeverMovesWithProgress(t *testing.T) {
	served := &server{} // drainReadLimit zero: what every deployed server runs with
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	body := &deadlinedBody{ReadCloser: &trickle{left: 6, pause: 200 * time.Millisecond}, writer: writer, idle: served.drainTime(), whole: docs.WriteRequestBound}
	buf := make([]byte, 1)
	for range 6 {
		if _, err := body.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	// Drop the final lift (zero time) if any.
	var armed []time.Time
	for _, at := range writer.set {
		if !at.IsZero() {
			armed = append(armed, at)
		}
	}
	distinct := map[time.Time]bool{}
	for _, at := range armed {
		distinct[at] = true
	}
	t.Logf("idle=%s whole=%s: %d deadlines armed over %d reads 200ms apart, %d distinct instant(s); atCeiling=%t",
		served.drainTime(), docs.WriteRequestBound, len(armed), 6, len(distinct), body.atCeiling)
	if len(distinct) == 1 && body.atCeiling {
		t.Errorf("every rearm set the same instant (first read + %s): the production body deadline is absolute, not idle", docs.WriteRequestBound)
	}
}

// End to end, at production settings: a JSON message whose body arrives one byte every 800 ms,
// never stalling for more than 0.8 s, is cut at 45 s and refused BODY_TOO_SLOW. On main no
// deadline bounds a handler's body read.
func TestR7ASteadyBodyIsNotCutByTheProductionCeiling(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	if served.drainReadLimit != 0 {
		t.Fatalf("drainReadLimit = %s, want the deployment's zero", served.drainReadLimit)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	issue := createArtifactIssue(t, handler)
	body := []byte(`{"body":"` + strings.Repeat("a", 52) + `"}`) // 64 bytes
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	started := time.Now()
	held := sendPartOfBody(t, server.Listener.Addr().String(), "/api/v1/issues/"+issue.Key+"/messages", header, len(body), body[:1])
	if err := held.connection.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sent := 1; sent < len(body); sent++ {
			time.Sleep(800 * time.Millisecond)
			if _, err := held.connection.Write(body[sent : sent+1]); err != nil {
				return
			}
		}
	}()
	response, answer := held.answer()
	elapsed := time.Since(started)
	t.Logf("a %d-byte body sent one byte every 800ms (never idle past 0.8s) was answered %d %s after %s",
		len(body), response.StatusCode, answer, elapsed.Round(time.Millisecond))
	if response.StatusCode == http.StatusServiceUnavailable && refusalCode(answer) == "BODY_TOO_SLOW" {
		t.Errorf("a body that never stopped arriving was cut at the absolute %s ceiling", docs.WriteRequestBound)
	}
}
