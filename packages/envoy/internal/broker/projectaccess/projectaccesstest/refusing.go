package projectaccesstest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RefusalBody is how the body of a refusing server's 401 or 403 goes wrong (NewRefusing).
type RefusalBody string

const (
	// RefusalOversized is a body past the 64 KiB projectaccess reads of an answer.
	RefusalOversized RefusalBody = "past 64 KiB"
	// RefusalCutShort is a body that ends, its connection closed, before the length its
	// Content-Length declares, so reading it fails.
	RefusalCutShort RefusalBody = "cut short of its Content-Length"
)

// RefusalBodies is every RefusalBody, for a test that runs each.
var RefusalBodies = []RefusalBody{RefusalOversized, RefusalCutShort}

// NewRefusing serves status to every request, with a body that goes wrong as body says, until t
// ends, and answers its base URL. It is the one fixture for a Dispatch whose refusal of the
// broker's own account token the client cannot read whole, so the client's tests and the
// exchange's exercise the same bytes.
func NewRefusing(t testing.TB, status int, body RefusalBody) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if body == RefusalOversized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"code":"ACCESS_ACCOUNT_REQUIRED","error":"%s"}`, strings.Repeat("x", 70<<10))
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the refusal's connection: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"code\":\"ACCESS_ACC", status, http.StatusText(status))
		_ = buffered.Flush()
	}))
	t.Cleanup(server.Close)
	return server.URL
}
