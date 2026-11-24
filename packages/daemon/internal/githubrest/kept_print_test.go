package githubrest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAnAnswerPrintsNoCredentialItKeeps: GitHub's refusal of a call, an *Answer, keeps the answer
// as it came (read), and once AnswerOf has read its words for Body, that answer keeps the token
// the call carried. A stand-in that echoes the call's Authorization shows the answer keeps it:
// Body prints it redacted. The token prints under no fmt verb and no bad verb, as the Answer,
// behind a pointer, in a slice or as a map's value, or inside a struct
// (oauth2calltest.KeptPrintTest): read keeps it two pointers below the Answer, and fmt spells out
// only the first. Captures kept/githubrest.Answer/<value>/<holding>/<verb>.
func TestAnAnswerPrintsNoCredentialItKeeps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "refused "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(server.Close)
	err := Client{API: server.URL, Token: oauth2calltest.Token}.Get(context.Background(), "/repos/acme/widgets", "", nil)
	var answer *Answer
	if !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want GitHub's *Answer", err)
	}
	if !answer.Says(oauth2calltest.Token) || strings.Contains(answer.Body, oauth2calltest.Token) {
		t.Fatalf("the stand-in's answer echoes the token %v and Body is %q; want it echoed and Body without it", answer.Says(oauth2calltest.Token), answer.Body)
	}
	twin := "refused " + oauth2calltest.Token
	t.Run("pointer", func(t *testing.T) {
		oauth2calltest.KeptPrintTest(t, "githubrest.Answer/pointer", answer, twin)
	})
	t.Run("value", func(t *testing.T) {
		oauth2calltest.KeptPrintTest(t, "githubrest.Answer/value", *answer, twin)
	})
}
