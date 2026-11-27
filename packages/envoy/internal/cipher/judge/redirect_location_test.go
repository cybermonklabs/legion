package judge

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAJudgeReadRedirectPrintsNoLocationOfIt: whatever answers at Dispatch's address answers the
// judge's issue read, which carries the broker's account token, with a redirect whose Location
// quotes that token. One net/http cannot parse (unparsed) is net/http's error whatever the
// client's redirect policy, since Go parses a Location before it asks CheckRedirect; one that
// parses names an address on another host nothing listens at (elsewhere), which a following
// client would ask, naming the Location in its error, and which the judge's read, whose client
// follows no redirect, refuses as the 302 it is (TestAJudgeReadFollowsNoRedirect). The read's
// error, which a run's row and log carry, holds no run of the token and no word of the Location.
// Captures judge.redirect/<case>.
func TestAJudgeReadRedirectPrintsNoLocationOfIt(t *testing.T) {
	const token = "mz6rd-not-a-secret-RDX-c2f8"
	dead := redirectstest.Dead(t)
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := NewDispatch(redirectstest.EchoBearer(t, c.location), ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, token)), &http.Client{})
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
