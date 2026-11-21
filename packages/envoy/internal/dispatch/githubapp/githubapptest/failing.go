package githubapptest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Failure is how FailingGitHub fails a read: each is the failure of one stage of the client's
// one sender, or an answer the GitHub proxy cannot decode.
type Failure string

const (
	// CloseBeforeAnswer closes the connection before it answers: the transport fails.
	CloseBeforeAnswer Failure = "close before answer"
	// AnswerLate answers nothing until the request's own context ends, past the client's
	// deadline: the transport fails on the deadline.
	AnswerLate Failure = "answer late"
	// ShortBody sends a Content-Length longer than the body it sends, and closes: the body
	// cannot be read.
	ShortBody Failure = "short body"
	// Oversized answers 200 with one byte more than the 1 MiB a read may hold.
	Oversized Failure = "oversized"
	// Undecodable answers 200 with a body that is no JSON.
	Undecodable Failure = "undecodable"
)

// OversizedBytes is the size of the Oversized answer.
const OversizedBytes = 1<<20 + 1

// FailingGitHub is a GitHub that answers the App's installation lookup of every repository with
// installation 7, and its token mint, and fails every other request as failure says. It returns
// the server's URL; the server is closed when the test ends.
func FailingGitHub(t testing.TB, failure Failure) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":7,"app_slug":"dispatch-test","permissions":{"pull_requests":"read","checks":"read","actions":"read"}}`)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_failing","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			return
		}
		switch failure {
		case CloseBeforeAnswer, ShortBody:
			conn, buffered, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack the connection: %v", err)
				return
			}
			if failure == ShortBody {
				fmt.Fprint(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"state\":")
				_ = buffered.Flush()
			}
			_ = conn.Close()
		case AnswerLate:
			<-r.Context().Done()
		case Oversized:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, strings.Repeat("x", OversizedBytes))
		case Undecodable:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "not json")
		default:
			t.Errorf("FailingGitHub: no failure %q", failure)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}
