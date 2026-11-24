package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// userinfoEcho serves, until t ends, a stand-in that answers every request 302 Found with a
// Location net/http cannot parse that quotes the password of the Basic credentials the request
// carried (redirectstest.Unparsed). It answers its base URL with redacttest's user name and
// password as its user information, which net/http sends as those Basic credentials.
func userinfoEcho(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, _ := r.BasicAuth()
		w.Header().Set("Location", redirectstest.Unparsed(password))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return "http://" + redacttest.User + ":" + redacttest.Password + "@" + srv.Listener.Addr().String()
}

// unreadableLineServer serves, until t ends, a stand-in that answers every request, a live
// socket's upgrade among them, with a status line whose version is redacttest.Code, a URL that
// carries the probe's credentials. Neither net/http nor gorilla can read it, and each quotes the
// version in its error (net/http/response.go of go1.27.3, lines 187-189, which gorilla's dial
// reads the answer with). It answers its base URL.
func unreadableLineServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
					_, _ = io.WriteString(conn, redacttest.Code+" 200 OK\r\n\r\n")
				}
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// TestADashboardProbePrintsNoWordOfItsAnswerers: whatever answers at Dispatch's address answers
// the dashboard probe's two requests with words that hold a credential, and the probe's error holds
// none of them. userinfo: a foreign -url carries user information, which gorilla refuses to dial
// and the SSE request sends as Basic credentials; the answer is a redirect whose Location does not
// parse and quotes the password (userinfoEcho), and net/http's error also names the URL with its
// user name. status-line: the live socket's upgrade and the SSE request are each answered with a
// status line neither client can read, which quotes a URL with the probe's credentials
// (unreadableLineServer). Each stream's failure prints by its class. The twins show each client's
// own error holds the credential. Captures loadtest.dashboard-words/<case>.
func TestADashboardProbePrintsNoWordOfItsAnswerers(t *testing.T) {
	userinfo := userinfoEcho(t)
	line := unreadableLineServer(t)
	t.Run("twin-userinfo", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, userinfo+ssePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		redirectstest.RequireEcho(t, request, redacttest.Password)
	})
	t.Run("twin-status-line", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, line+ssePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		redirectstest.RequireEcho(t, request, redacttest.Token)
		liveURL, err := liveSocketURL(line)
		if err != nil {
			t.Fatal(err)
		}
		conn, _, err := websocket.DefaultDialer.Dial(liveURL, nil)
		if err == nil {
			_ = conn.Close()
		}
		if err == nil || !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("gorilla's own error holds no word of the status line, so the row cannot see one: %v", err)
		}
	})
	for _, c := range []struct{ name, dispatch string }{{"userinfo", userinfo}, {"status-line", line}} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := probeDashboard(context.Background(), c.dispatch)
			if err == nil {
				t.Fatal("probeDashboard answered no error")
			}
			redacttest.RequireNoPiece(t, "loadtest.dashboard-words/"+c.name, err.Error(), redacttest.Password)
			if want := "/ws/live answered " + redact.ClassRequestFailed + ", and GET " + ssePath + " answered " + redact.ClassRequestFailed; !strings.Contains(err.Error(), want) {
				t.Errorf("probeDashboard's error is %q, want %q in it", err, want)
			}
		})
	}
}
