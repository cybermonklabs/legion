package cireconcile

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGraphQLRefusalNamesGitHubsErrorsAsTheRuleReadsThem: GitHub, or whatever answers in its
// place, refuses the pass's query with an errors entry whose type and message quote a credential's
// URL and echo the installation token the query carried. The query's error names each entry's type
// and message as the rule reads them, with the request's credentials redacted (each held as
// redact.RemoteText, decoded with the answer's record), and none of what the stand-in wrote.
// Captures cireconcile.answer-words/errors.
func TestAGraphQLRefusalNamesGitHubsErrorsAsTheRuleReadsThem(t *testing.T) {
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		echoed := r.Header.Get("Authorization")
		echoed = echoed[strings.LastIndex(echoed, " ")+1:]
		fmt.Fprintf(w, `{"data":{"repository":null},"errors":[{"type":"%s","message":"token %s refused; %s"}]}`, redacttest.Code, echoed, redacttest.Words)
	}})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.cireconcile", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = (&Reconciler{GitHub: client}).query(context.Background(), reconciled{owner: "acme", repo: "web", token: redacttest.Token}, "", &counts{})
	if err == nil {
		t.Fatal("the query GitHub refused answered no error")
	}
	redacttest.RequireNone(t, "cireconcile.answer-words/errors", err.Error())
	for _, want := range []string{"GitHub refused the GraphQL query for acme/web: " + redacttest.CodeShown + " token", " refused; " + redacttest.Shown} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the query's error is %q, want %q in it", err, want)
		}
	}
}
