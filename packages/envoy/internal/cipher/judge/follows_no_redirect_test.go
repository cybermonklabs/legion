package judge

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAJudgeReadFollowsNoRedirect: whatever answers at Dispatch's address answers the judge's
// issue read, which carries the broker's account token, with a 302 whose Location names a stand-in
// on another host and quotes the token in its query. Under the default client (a nil HTTP) and a
// client its caller gives, as cmd/cipherd does, the stand-in is sent nothing, so no request
// carries the token on to it, and the read fails on Dispatch's 302 by its status. The twin shows
// a client that follows the redirect sends the stand-in the token, so the row can see one sent on.
// Captures judge.redirect-followed/<client>, what the stand-in was sent.
func TestAJudgeReadFollowsNoRedirect(t *testing.T) {
	const token = "wd3kc-not-a-secret-RDX-b7e2"
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, location)+"/api/v1/issues/"+testIssue, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireSentOn(t, request, token, sent)
	})
	for _, c := range []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := Dispatch{BaseURL: redirectstest.EchoBearer(t, location), Account: ciphertoken.NewAccountToken(tokenFile(t, token)), HTTP: c.client}
			_, err := dispatch.Issue(t.Context(), testIssue)
			text := sent()
			redacttest.RequireNoPiece(t, "judge.redirect-followed/"+c.name, text, token)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), "dispatch answered 302") {
				t.Errorf("Issue = %v, want the read refused on Dispatch's 302", err)
			}
		})
	}
}
