package githubapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoingDataGitHub is a GitHub whose installation token is redacttest.Token and that answers
// every other read 200 with the body answer writes from the Authorization the read carried, as
// whatever answers in GitHub's place can. It answers a client of it, its own URL, and a reader of
// the credential the last read carried.
func echoingDataGitHub(t *testing.T, answer func(authorization string) string) (client *githubapp.Client, base string, carried func() string) {
	t.Helper()
	var mu sync.Mutex
	var held string
	base = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read","checks":"read"}}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, redacttest.Token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			mu.Lock()
			held = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, answer(r.Header.Get("Authorization")))
		}
	}})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxy", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, base, func() string {
		mu.Lock()
		defer mu.Unlock()
		return held
	}
}

// TestTheProxysRelayOfADataAnswerHoldsNoTokenItsReadCarried: whatever answers in GitHub's place
// answers the proxy's read 200, as the data the web app asked for, with words that echo the
// installation token the read carried. The body the proxy writes to the page holds no run of the
// token, and the rest of the answer as GitHub's place wrote it: for a read the proxy forwards
// whole (relayed-data, a pull request) and for the workflow runs it trims (workflow-runs, whose
// event and name it keeps). The twin shows the echo is a real one: the same answer read as it
// came holds the token. Captures githubapi.answer-tokens/<case>.
func TestTheProxysRelayOfADataAnswerHoldsNoTokenItsReadCarried(t *testing.T) {
	pull := func(authorization string) string {
		return fmt.Sprintf(`{"state":"open","title":"seen %s"}`, authorization)
	}
	t.Run("twin", func(t *testing.T) {
		_, base, carried := echoingDataGitHub(t, pull)
		request, err := http.NewRequest(http.MethodGet, base+"/repos/acme/web/pulls/7", nil)
		if err != nil {
			t.Fatalf("the twin's request: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+redacttest.Token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("the twin's read: %v", err)
		}
		defer response.Body.Close()
		said, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("the twin's body: %v", err)
		}
		if piece := redacttest.Piece(string(said), carried()); piece == "" {
			t.Errorf("GitHub's place answered %q, which shows no run of the credential the read carried; the rows below would see no leak", said)
		}
	})
	for _, c := range []struct {
		name, path string
		answer     func(string) string
		want       string
	}{
		{"relayed-data", "repos/acme/web/pulls/7", pull, `{"state":"open","title":"seen Bearer [redacted]"}`},
		{"workflow-runs", "repos/acme/web/actions/runs?head_sha=" + headSHA, func(authorization string) string {
			return fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":5,"check_suite_id":6,"event":"push %s","name":"CI %s"}]}`, authorization, authorization)
		}, `{"total_count":1,"workflow_runs":[{"check_suite_id":6,"event":"push Bearer [redacted]","name":"CI Bearer [redacted]"}]}` + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, _, carried := echoingDataGitHub(t, c.answer)
			response := proxy(client, http.MethodGet, "/api/github/rest/"+c.path)
			if response.Code != http.StatusOK {
				t.Fatalf("the proxy answered %d, want GitHub's 200 relayed: %q", response.Code, response.Body.String())
			}
			if carried() == "" {
				t.Fatal("GitHub was never read")
			}
			redacttest.RequireNoPiece(t, "githubapi.answer-tokens/"+c.name, response.Body.String(), carried())
			if response.Body.String() != c.want {
				t.Errorf("the proxy wrote %q, want %q", response.Body.String(), c.want)
			}
		})
	}
}
