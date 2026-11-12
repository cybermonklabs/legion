package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A registration whose body arrives after the route's bound gets the retryable refusal, not the
// 400 a malformed body gets. The bound moves the connection's read deadline to the route's own
// deadline (server.bound), so a body the caller is slow to send fails to read with the deadline's
// i/o timeout: that is the daemon's bound running out while it read, not a request it could not
// decode, and R4 answers it past the bound with the uncoded 503, having recorded nothing
// (dispatch://INFRA-2883). The plugin retries that answer and a 400 it never retries.
//
// It runs over a real connection because the read deadline is the connection's: a recorder has no
// connection to bound. The negative control is the 400 the route gave before this row:
// `invalid request body: read tcp …: i/o timeout`.
func TestARegistrationWhoseBodyArrivesAfterTheBoundIsToldToRetry(t *testing.T) {
	h := boundHarness(t)
	_, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	server := httptest.NewServer(h.handler)
	t.Cleanup(server.Close)

	body, err := json.Marshal(claim.RegisterRequest{
		BootToken: boot, SessionID: "ses_implementer", OmpSessionFile: "/sessions/ses_implementer.jsonl",
		AgentID: "agent-implementer", PluginContract: DaemonAPIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// The headers go now and the body only once the route's bound has passed.
	if _, err := fmt.Fprintf(conn, "POST /legion/v1/claims/register HTTP/1.1\r\nHost: daemon.test\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(shortBound + 200*time.Millisecond)
	_, _ = conn.Write(body)

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the registration's answer: %v", err)
	}
	defer response.Body.Close()
	var answer struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the registration's answer: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || answer.Code != "" || answer.Error != boundRefusal {
		t.Fatalf("a registration whose body came after the bound = %d %+v, want the uncoded 503 %q",
			response.StatusCode, answer, boundRefusal)
	}
}
