package githubapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// largeRefusal is a body GitHub could answer a refused request with: a JSON object that names its
// message first, filled to the 1 MiB a response may hold.
var largeRefusal = `{"message":"Server Error","filler":"` + strings.Repeat("x", 1<<20-len(`{"message":"Server Error","filler":""}`)) + `"}`

// An error's text shows GitHub's body to a bound, and names the body's size where it cut it, at
// every place this package puts a body GitHub answered into an error: a line that logs the error
// writes it whole, and a body may be 1 MiB, so a WARN line carried 1,048,697 bytes in one field
// (review of cybermonklabs/legion#4209 at def907052c, H3). The start of the body, where GitHub
// writes its message, is kept. Message reads the whole body, not the text.
func TestAnErrorShowsGitHubsBodyToABound(t *testing.T) {
	ctx := context.Background()
	failing := refusingGitHub(t, false)
	answering := refusingGitHub(t, true)
	refused := githubapp.CheckResponse(http.StatusInternalServerError, http.Header{}, []byte(largeRefusal))
	for _, call := range []struct {
		name   string
		status string
		err    func() error
	}{
		{"a status error", "status 500: ", func() error { return refused }},
		{"a rate limit", "status 403", func() error {
			return &githubapp.RateLimitError{Status: http.StatusForbidden, Wait: time.Minute, Body: largeRefusal}
		}},
		{"an installation lookup", "status 500: ", func() error { _, err := failing.Installation(ctx, "acme", "widgets"); return err }},
		{"a token mint", "status 500: ", func() error { _, err := failing.Token(ctx, 1); return err }},
		{"a source check", "status 500: ", func() error { _, err := answering.CheckSource(ctx, "acme", "widgets"); return err }},
		{"a commit read", "status 500: ", func() error { _, err := failing.Ref(ctx, "ghs_test", "acme", "widgets", "main"); return err }},
		{"a branch read", "status 500: ", func() error { _, err := failing.Branch(ctx, "ghs_test", "acme", "widgets", "main"); return err }},
		{"a directory read", "status 500: ", func() error {
			_, err := failing.Dir(ctx, "ghs_test", "acme", "widgets", "abc", "notes")
			return err
		}},
		{"a blob read", "status 500: ", func() error {
			_, err := failing.DirFiles(ctx, "ghs_test", "acme", "widgets", githubapp.Dir{
				SHA: "abc", Files: []githubapp.DirFile{{Name: "a.md", Path: "notes/a.md", SHA: "def", Size: 1}}})
			return err
		}},
		{"a delivery listing", "status 500: ", func() error { _, err := failing.Deliveries(ctx, time.Now().Add(-time.Hour), nil); return err }},
		{"a redelivery", "status 500: ", func() error { return failing.Redeliver(ctx, 7) }},
	} {
		t.Run(strings.ReplaceAll(call.name, " ", "_"), func(t *testing.T) {
			err := call.err()
			if err == nil {
				t.Fatal("the call answered no error, want GitHub's refusal")
			}
			text := err.Error()
			t.Logf("bytes=%d", len(text))
			if len(text) > 1024 || !strings.Contains(text, call.status) ||
				!strings.Contains(text, `{"message":"Server Error"`) || !strings.Contains(text, "1048576 bytes") {
				t.Errorf("the error's text is %d bytes and begins %.200q, want at most 1,024 that name %q, keep the start of GitHub's body and name its 1048576 bytes", len(text), text, call.status)
			}
		})
	}
	var status *githubapp.StatusError
	if !errors.As(refused, &status) || status.Message() != "Server Error" || len(status.Body) != 1<<20 {
		t.Errorf("the status error is %T, want a *githubapp.StatusError that keeps the whole body and its message", refused)
	}
}

// A body is shown whole up to 512 runes and cut at 512 past that, at a rune and never inside one,
// naming the whole body's bytes, whether the body is a string or bytes; a byte that is no UTF-8
// counts as one rune.
func TestShownBodyCutsAtTheBoundOfRunesAndNeverInsideOne(t *testing.T) {
	for _, body := range []struct {
		name, body, want string
	}{
		{"a short body", `{"message":"Not Found"}`, `{"message":"Not Found"}`},
		{"512 two-byte letters", strings.Repeat("é", 512), strings.Repeat("é", 512)},
		{"513 two-byte letters", strings.Repeat("é", 513), strings.Repeat("é", 512) + "… (1026 bytes)"},
		{"600 four-byte letters", strings.Repeat("😀", 600), strings.Repeat("😀", 512) + "… (2400 bytes)"},
		{"600 bytes that are no UTF-8", strings.Repeat("\xff", 600), strings.Repeat("\xff", 512) + "… (600 bytes)"},
	} {
		t.Run(strings.ReplaceAll(body.name, " ", "_"), func(t *testing.T) {
			if got := githubapp.ShownBody(body.body); got != body.want {
				t.Errorf("ShownBody of %d bytes = %.80q (%d bytes), want %.80q (%d bytes)", len(body.body), got, len(got), body.want, len(body.want))
			}
			if got := githubapp.ShownBody([]byte(body.body)); got != body.want {
				t.Errorf("ShownBody of the same %d bytes as a []byte = %.80q, want %.80q", len(body.body), got, body.want)
			}
		})
	}
}

// refusingGitHub is a client of a GitHub that answers every request with a 500 and largeRefusal;
// where the App answers, its installation lookup and token mint of acme/widgets answer as GitHub
// does, so a call gets to the read after them.
func refusingGitHub(t *testing.T, appAnswers bool) *githubapp.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case appAnswers && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/installation":
			encodeAnswer(t, w, http.StatusOK, map[string]any{"id": 1, "app_slug": "acme-app", "permissions": map[string]string{"contents": "read"}})
		case appAnswers && r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens":
			encodeAnswer(t, w, http.StatusCreated, map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		default:
			w.WriteHeader(http.StatusInternalServerError)
			if _, err := io.WriteString(w, largeRefusal); err != nil {
				t.Errorf("write the refusal: %v", err)
			}
		}
	}))
	t.Cleanup(server.Close)
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.refusing", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func encodeAnswer(t *testing.T, w http.ResponseWriter, status int, answer any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		t.Errorf("encode the answer: %v", err)
	}
}
