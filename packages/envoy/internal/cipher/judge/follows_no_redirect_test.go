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
	location := func(credential string) string {
		return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", location)+"/api/v1/issues/"+testIssue, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("a client that follows the redirect failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, token) {
			t.Fatalf("a client that follows the redirect sent the stand-in no token, so the row cannot see one sent on: %q", text)
		}
	})
	for _, c := range []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := Dispatch{BaseURL: redirectstest.Echo(t, "Authorization", location), Account: ciphertoken.NewAccountToken(tokenFile(t, token)), HTTP: c.client}
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
