package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// flakyRequestID is the request flakyBroker leaves pending, and flakyValue the value its grant
// releases for FLAKY_ME.
const (
	flakyRequestID = "req-flaky"
	flakyValue     = "flaky-value"
)

// pollAnswer is one answer flakyBroker gives a status poll of flakyRequestID.
type pollAnswer func(t *testing.T, w http.ResponseWriter)

// resetPoll ends the poll's connection with a TCP reset (SO_LINGER 0) before any answer, so the
// CLI reads the `connection reset by peer` a production wait once ended on.
func resetPoll(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack the status poll: %v", err)
		return
	}
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Errorf("the status poll came on a %T, not a TCP connection to reset", conn)
		return
	}
	if err := tcp.SetLinger(0); err != nil {
		t.Errorf("SO_LINGER 0 on the status poll: %v", err)
	}
}

// cutPoll answers the poll's headers and the start of a body it promises to be longer, then ends
// the connection with a reset, so the CLI reads the headers and loses the answer mid-body: the
// same broken exchange as resetPoll's, a few bytes later.
func cutPoll(t *testing.T, w http.ResponseWriter) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack the status poll: %v", err)
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"state\":\"pend")
	if err := buf.Flush(); err != nil {
		t.Errorf("write the cut-off answer: %v", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Errorf("the status poll came on a %T, not a TCP connection to reset", conn)
		return
	}
	if err := tcp.SetLinger(0); err != nil {
		t.Errorf("SO_LINGER 0 on the status poll: %v", err)
	}
}

// statePoll answers the poll with the request in state, its grant grant-flaky once granted.
func statePoll(state string) pollAnswer {
	return func(_ *testing.T, w http.ResponseWriter) {
		var grantID any
		if state == "granted" {
			grantID = "grant-flaky"
		}
		writeJSON(w, map[string]any{
			"state": state, "grant_id": grantID, "record_id": "rec-flaky", "approver": "ada@example.com",
			"decided_at": nil, "decision": nil,
		})
	}
}

// refusedPoll answers the poll with status and body as contentType.
func refusedPoll(status int, contentType, body string) pollAnswer {
	return func(_ *testing.T, w http.ResponseWriter) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// flakyBroker serves one pending request, FLAKY_ME's flakyRequestID, whose status polls get the
// answers polls lists in turn, the last of them again for every poll after it; a granted answer
// names grant-flaky, which releases flakyValue. It counts the polls it served. Its keep-alives
// are off, so every poll is a connection of its own: net/http's transport sends a GET again on
// its own when a connection it reused fails before the answer, which would spend two of these
// answers on one poll.
func flakyBroker(t *testing.T, polls []pollAnswer) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	served := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"request_id": flakyRequestID, "state": "pending", "grant_id": nil, "record_id": "rec-flaky",
			"approver": "ada@example.com", "secrets": []map[string]string{{"name": "FLAKY_ME", "decision": "approval"}},
		})
	})
	mux.HandleFunc("GET /v1/requests/"+flakyRequestID, func(w http.ResponseWriter, r *http.Request) {
		n := int(served.Add(1))
		polls[min(n, len(polls))-1](t, w)
	})
	mux.HandleFunc("POST /v1/grants/grant-flaky/values", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"values": map[string]string{"FLAKY_ME": flakyValue}, "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fakeSettings)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, served
}

// TestExecFormWaitOutlivesFailedStatusPolls: a status poll that got no answer about the request
// (a reset connection, a 503 from the broker, a front door's bare 502) does not end the exec
// form's wait. The command keeps polling until --wait runs out, says so once per run of such
// failures, and runs the command when a later poll finds the request granted. A wait that runs
// out while its polls still fail exits 75, as a wait that ran out on a pending request does,
// naming the request, the last failure and the command that still collects the grant. A refusal
// no retry changes (an unknown request's 404) still ends the wait at once.
func TestExecFormWaitOutlivesFailedStatusPolls(t *testing.T) {
	binary := buildAgentSecrets(t)
	keyDir := newKeyDir(t)
	reset, pending, granted := resetPoll, statePoll("pending"), statePoll("granted")
	const (
		keepsWaiting = "; still waiting for request " + flakyRequestID + "\n"
		ranOut       = "agent-secrets: the wait for request " + flakyRequestID + " to "
		checkIt      = "; nothing was run. Check it with: agent-secrets status " + flakyRequestID + "\n"
	)
	for _, row := range []struct {
		name  string
		wait  string
		polls []pollAnswer
		exit  int
		// lines is how many keeps-waiting lines stderr carries: one per run of failed polls.
		lines int
		// says is what else stderr must carry.
		says []string
	}{
		{
			name: "two resets, then granted", wait: "1m", polls: []pollAnswer{reset, reset, granted},
			exit: 0, lines: 1, says: []string{"connection reset by peer" + keepsWaiting},
		},
		{
			name: "an answer cut off after its headers, then granted", wait: "1m", polls: []pollAnswer{cutPoll, granted},
			exit: 0, lines: 1, says: []string{"read response: ", keepsWaiting},
		},
		{
			name: "a 503, a front door's bare 502, then granted", wait: "1m",
			polls: []pollAnswer{
				refusedPoll(http.StatusServiceUnavailable, "application/json", `{"code":"SHUTTING_DOWN","error":"the broker is shutting down and is no longer taking new traffic"}`),
				refusedPoll(http.StatusBadGateway, "text/html", "<html><body><h1>502 Bad Gateway</h1></body></html>"),
				granted,
			},
			exit: 0, lines: 1, says: []string{"no longer taking new traffic (SHUTTING_DOWN); asked "},
		},
		{
			name: "every poll reset until the wait runs out", wait: "3s", polls: []pollAnswer{reset},
			exit: exitPending, lines: 1,
			says: []string{ranOut, "ran out while its status could not be read: ", "connection reset by peer" + checkIt},
		},
		{
			name: "a reset, a pending answer, then resets until the wait runs out", wait: "9s", polls: []pollAnswer{reset, pending, reset},
			exit: exitPending, lines: 2, says: []string{"connection reset by peer" + checkIt},
		},
		{
			name: "an unknown request", wait: "1m",
			polls: []pollAnswer{refusedPoll(http.StatusNotFound, "application/json", `{"code":"NOT_FOUND","error":"no such request"}`)},
			exit:  1, lines: 0, says: []string{"agent-secrets: no such request (NOT_FOUND); asked "},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			broker, served := flakyBroker(t, row.polls)
			stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, keyDir, nil,
				"FLAKY_ME", "--wait", row.wait, "--", "sh", "-c", `test "$FLAKY_ME" = `+flakyValue+` && echo ran-the-child`)
			ran := strings.Contains(stdout, "ran-the-child")
			if exit != row.exit || ran != (row.exit == 0) {
				t.Fatalf("exit %d, child ran %t; want exit %d, child ran %t: stdout=%q stderr=%q", exit, ran, row.exit, row.exit == 0, stdout, stderr)
			}
			if got := strings.Count(stderr, keepsWaiting); got != row.lines {
				t.Errorf("stderr says it keeps waiting %d times, want %d (once per run of failed polls): %q", got, row.lines, stderr)
			}
			for _, want := range row.says {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to carry %q", stderr, want)
				}
			}
			if row.exit == exitPending && strings.Contains(stderr, "is still waiting for approval") {
				t.Errorf("stderr = %q: a wait that ran out on failed polls says it read the request pending", stderr)
			}
			// A wait that ended on an answer asked exactly the polls the script lists; one that ran
			// out asked at least them.
			if n := int(served.Load()); n < len(row.polls) || (row.exit != exitPending && n != len(row.polls)) {
				t.Errorf("the broker served %d status polls, want %d", n, len(row.polls))
			}
		})
	}
}

// TestOnlyAFailureARetryCanCureIsRetried pins retryable, the exec form's wait's test of a failed
// status poll: a transport failure, or a 429, 502, 503 or 504 with the broker's envelope or
// without it, is polled again; every other refusal, and a call never sent, is not.
func TestOnlyAFailureARetryCanCureIsRetried(t *testing.T) {
	const base = "http://broker.example"
	reset := unreachable(base, http.MethodGet, base+"/v1/requests/"+flakyRequestID, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)})
	for _, row := range []struct {
		err  error
		want bool
	}{
		{reset, true},
		{fmt.Errorf("wrapped: %w", reset), true},
		{fmt.Errorf("GET /v1/requests/x: read response: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), true},
		{fmt.Errorf("GET /v1/requests/x: read response: %w", io.ErrUnexpectedEOF), true},
		{&unreachableError{"the broker's address is not a valid URL", base, http.MethodGet, base, errNothingSent}, false},
		{&apiError{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "slow down"}, true},
		{&apiError{Status: http.StatusBadGateway, Message: "<html>502 Bad Gateway</html>"}, true},
		{&apiError{Status: http.StatusServiceUnavailable, Code: "SHUTTING_DOWN", Message: "shutting down"}, true},
		{&apiError{Status: http.StatusGatewayTimeout, Message: "upstream timed out"}, true},
		{&apiError{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no such request"}, false},
		{&apiError{Status: http.StatusUnauthorized, Code: "PROOF_INVALID", Message: "proof invalid"}, false},
		{&apiError{Status: http.StatusInternalServerError, Message: "internal"}, false},
		{&refusedBeforeCall{"a request id must name one path segment"}, false},
		{errors.New("decode request status: unexpected end of JSON input"), false},
	} {
		if got := retryable(row.err); got != row.want {
			t.Errorf("retryable(%v) = %t, want %t", row.err, got, row.want)
		}
	}
}
