package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGitHubRefusalPrintsNoTokenItsAnswerEchoes: an answer in GitHub's place refuses a read
// with a message that repeats the Authorization the read carried. The *StatusError a caller
// makes of that answer (CheckResponse), and its message, hold none of it: send, the one function
// that sends anything to GitHub, hands a refusal's body on with every credential its request
// carried redacted (redact.Carried), as every client of this repository reads a refused answer.
// Captures githubapp.answer-echo.
func TestAGitHubRefusalPrintsNoTokenItsAnswerEchoes(t *testing.T) {
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"message":"the credential %s cannot read this repository"}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(stand.Close)
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, stand.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	body, status, header, err := client.Read(context.Background(), redacttest.Token, "/repos/acme/web/pulls/7")
	if err != nil || status != http.StatusUnprocessableEntity {
		t.Fatalf("Read answered %d, %v; want the stand-in's 422", status, err)
	}
	refused := CheckResponse(status, header, body)
	if refused == nil {
		t.Fatal("CheckResponse took a 422 as data")
	}
	redacttest.RequireNone(t, "githubapp.answer-echo", fmt.Sprintf("%v | %s", refused, NewStatusError(status, body).Message()))
}
