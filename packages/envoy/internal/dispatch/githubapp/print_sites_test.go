package githubapp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// printSiteStandIn answers GitHub for one print-site row: the installation lookup and the token
// mint that a call may make before its own read, and the read itself at every path under probe,
// with status, header and a body that echoes the Authorization it carried, as whatever answers in
// GitHub's place can. It answers the client, the stand-in's own URL and a reader of the
// credential the probe's last request carried — the App's JWT, or the token the call was given —
// of which no text this package prints may show a run.
func printSiteStandIn(t *testing.T, probe string, status int, header map[string]string) (client *Client, base string, carried func() string) {
	t.Helper()
	var mu sync.Mutex
	var held string
	handle := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/web/installation":
			fmt.Fprint(w, `{"id":7,"app_slug":"dispatch","permissions":{"contents":"read"}}`)
		case r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, redacttest.Token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.HasPrefix(r.URL.Path, probe):
			mu.Lock()
			held = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Unlock()
			for name, value := range header {
				w.Header().Set(name, value)
			}
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"the credential %s may not read this"}`, r.Header.Get("Authorization"))
		default:
			githubtest.Unexpected(t, w, r)
		}
	}
	base = githubtest.Server(t, githubtest.Spec{Handle: handle})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, base, func() string {
		mu.Lock()
		defer mu.Unlock()
		return held
	}
}

// TestEachAnswerThisClientPrintsHoldsNoCredentialItsReadCarried: each of this package's own prints
// of an answer GitHub gave — the status errors of CheckSource, Ref, Branch, Dir, the deliveries
// listing and a redelivery, Ref's reading of the answer it sends back to GitHub, and the body a
// rate limit's error carries (answeredLimit) — prints the answer only as the rule reads it, with
// every credential its request carried redacted (redact.Answer.Words, RemoteText.SendBack). The
// stand-in echoes the Authorization of the read in the body it answers, as whatever answers in
// GitHub's place can, and the twin shows that echo is a real one: read as it came, that body holds
// the credential. Captures githubapp.answer-words/<case>.
func TestEachAnswerThisClientPrintsHoldsNoCredentialItsReadCarried(t *testing.T) {
	ctx := context.Background()
	t.Run("twin", func(t *testing.T) {
		_, base, carried := printSiteStandIn(t, "/repos/acme/web", http.StatusInternalServerError, nil)
		request, err := http.NewRequest(http.MethodGet, base+"/repos/acme/web", nil)
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
			t.Errorf("the probe answered %q, which shows no run of the credential the read carried; the rows below would see no leak", said)
		}
	})

	for _, c := range []struct {
		name   string
		probe  string
		status int
		header map[string]string
		// read makes the call and answers the text it printed.
		read func(*Client) string
	}{{
		name:   "check-source",
		probe:  "/repos/acme/web",
		status: http.StatusInternalServerError,
		read: func(client *Client) string {
			_, err := client.CheckSource(ctx, "acme", "web")
			return fmt.Sprint(err)
		},
	}, {
		name:   "ref-status",
		probe:  "/repos/acme/web/commits/",
		status: http.StatusInternalServerError,
		read: func(client *Client) string {
			_, err := client.Ref(ctx, redacttest.Token, "acme", "web", "main")
			return fmt.Sprint(err)
		},
	}, {
		name:   "ref-no-sha",
		probe:  "/repos/acme/web/commits/",
		status: http.StatusOK,
		read: func(client *Client) string {
			// The answer leaves Ref only as a 40-hex SHA, so the row reads what it hands back
			// beside what it printed: GitHub's own text reaches its caller through neither.
			sha, err := client.Ref(ctx, redacttest.Token, "acme", "web", "main")
			return sha + " | " + fmt.Sprint(err)
		},
	}, {
		name:   "branch-status",
		probe:  "/repos/acme/web/git/ref/heads/",
		status: http.StatusInternalServerError,
		read: func(client *Client) string {
			_, err := client.Branch(ctx, redacttest.Token, "acme", "web", "main")
			return fmt.Sprint(err)
		},
	}, {
		name:   "dir-status",
		probe:  "/repos/acme/web/git/trees/",
		status: http.StatusInternalServerError,
		read: func(client *Client) string {
			_, err := client.Dir(ctx, redacttest.Token, "acme", "web", "c0ffee", "docs")
			return fmt.Sprint(err)
		},
	}, {
		name:   "deliveries-status",
		probe:  "/app/hook/deliveries",
		status: http.StatusInternalServerError,
		read: func(client *Client) string {
			_, err := client.Deliveries(ctx, time.Time{}, nil)
			return fmt.Sprint(err)
		},
	}, {
		name:   "redeliver-status",
		probe:  "/app/hook/deliveries/42/attempts",
		status: http.StatusInternalServerError,
		read:   func(client *Client) string { return fmt.Sprint(client.Redeliver(ctx, 42)) },
	}, {
		name:   "answered-limit",
		probe:  "/app/hook/deliveries",
		status: http.StatusForbidden,
		header: map[string]string{"X-Ratelimit-Remaining": "0"},
		read: func(client *Client) string {
			_, err := client.Deliveries(ctx, time.Time{}, nil)
			return fmt.Sprint(err)
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			client, _, carried := printSiteStandIn(t, c.probe, c.status, c.header)
			text := c.read(client)
			if carried() == "" {
				t.Fatalf("the probe was never read; the call printed %q", text)
			}
			redacttest.RequireNoPiece(t, "githubapp.answer-words/"+c.name, text, carried())
		})
	}
}
