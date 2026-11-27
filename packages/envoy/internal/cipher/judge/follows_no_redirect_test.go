package judge

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAJudgeReadFollowsNoRedirect: whatever answers at Dispatch's address answers the judge's
// issue read, which carries the broker's account token, with a 302, a 307 or a 308 whose Location
// names a stand-in on another host and quotes the token in its query. Under the default client (a
// nil client) and a client its caller gives, as cmd/cipherd does, the stand-in is sent nothing, so
// no request carries the token on to it, and the read fails on Dispatch's 3xx by its status. The
// twins show a client that follows each of them sends the stand-in the token, so the rows can see
// one sent on. Captures judge.redirect-followed/<client> (the 302) and
// judge.redirect-followed/<client>-<status>, what the stand-in was sent.
func TestAJudgeReadFollowsNoRedirect(t *testing.T) {
	const token = "wd3kc-not-a-secret-RDX-b7e2"
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	// moved is the stand-in at Dispatch's address for the 307 and 308 rows:
	// redirectstest.EchoBearer's, answering status where that one answers 302.
	moved := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, location)+"/api/v1/issues/"+testIssue, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireSentOn(t, request, token, sent)
	})
	clients := []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	}
	for _, c := range clients {
		t.Run(c.name, func(t *testing.T) {
			dispatch := NewDispatch(redirectstest.EchoBearer(t, location), ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, token)), c.client)
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
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		code := strconv.Itoa(status)
		t.Run("twin-"+code, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, moved(t, status)+"/api/v1/issues/"+testIssue, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			redirectstest.RequireSentOn(t, request, token, sent)
		})
		for _, c := range clients {
			t.Run(c.name+"-"+code, func(t *testing.T) {
				dispatch := NewDispatch(moved(t, status), ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, token)), c.client)
				_, err := dispatch.Issue(t.Context(), testIssue)
				text := sent()
				redacttest.RequireNoPiece(t, "judge.redirect-followed/"+c.name+"-"+code, text, token)
				if text != "" {
					t.Errorf("the stand-in the Location names was asked: %q", text)
				}
				if err == nil || !strings.Contains(err.Error(), "dispatch answered "+code) {
					t.Errorf("Issue = %v, want the read refused on Dispatch's %d", err, status)
				}
			})
		}
	}
}
