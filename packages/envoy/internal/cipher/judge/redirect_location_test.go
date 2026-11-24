package judge

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAJudgeReadRedirectPrintsNoLocationOfIt: whatever answers at Dispatch's address answers the
// judge's issue read, which carries the broker's account token, with a redirect whose Location
// quotes that token. One net/http cannot parse (unparsed) is net/http's error whatever the
// client's redirect policy, since Go parses a Location before it asks CheckRedirect; one that
// parses names an address on another host nothing listens at (elsewhere), which a following
// client asks, naming the Location in its error. The read's error, which a run's row and log
// carry, holds no run of the token and no word of the Location. Captures judge.redirect/<case>.
func TestAJudgeReadRedirectPrintsNoLocationOfIt(t *testing.T) {
	const token = "mz6rd-not-a-secret-RDX-c2f8"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", func(bearer string) string { return "/moved%zz" + bearer }},
		{"elsewhere", func(bearer string) string { return "http://" + dead + "/moved?access_token=" + bearer }},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", c.location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
				w.WriteHeader(http.StatusFound)
			}))
			defer server.Close()
			dispatch := Dispatch{BaseURL: server.URL, Account: ciphertoken.NewAccountToken(tokenFile(t, token)), HTTP: &http.Client{}}
			_, err := dispatch.Issue(t.Context(), testIssue)
			if err == nil {
				t.Fatal("the issue read answered no error")
			}
			redacttest.RequireNoPiece(t, "judge.redirect/"+c.name, err.Error(), token)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the read's error names the Location: %q", err)
			}
		})
	}
}
