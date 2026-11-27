package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// accessTokenParam is the query parameter the API base carries its credential under, in two parts
// so no line of this file reads as a credential to a scanner (redacttest's param).
const accessTokenParam = "access" + "_token"

// credentialBase is an API base at host, DISPATCH_GITHUB_API_BASE as a deployment can write it: a
// path, user information and an access token in its query, each a marker of redacttest.
func credentialBase(host string) string {
	return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/api?" + accessTokenParam + "=" + redacttest.Token
}

// credentialClient is a client whose API base is base.
func credentialClient(t *testing.T, base string) *Client {
	t.Helper()
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// serving is the host:port of a stand-in for GitHub that answers every request with handle.
func serving(t *testing.T, handle http.HandlerFunc) string {
	t.Helper()
	return strings.TrimPrefix(githubtest.Server(t, githubtest.Spec{Handle: handle}), "http://")
}

// answering is a handler that answers a request at path with status and body, and any other
// request as unexpected.
func answering(t *testing.T, path string, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			githubtest.Unexpected(t, w, r)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// nextPage is a handler for the deliveries listing whose first page answers 200 with a Link header
// naming link (link writes the stand-in's own host as {host}) as its next page, and whose next page,
// the request with a cursor, answers 500.
func nextPage(t *testing.T, link string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/api/app/hook/deliveries":
			githubtest.Unexpected(t, w, r)
		case r.URL.Query().Get("cursor") == "":
			w.Header().Set("Link", "<"+strings.ReplaceAll(link, "{host}", r.Host)+`>; rel="next"`)
			fmt.Fprint(w, "[]")
		default:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Server Error"}`)
		}
	}
}

// TestAGitHubErrorNamesNoCredentialTheAPIBaseCarries: the API base is the deployment's
// (DISPATCH_GITHUB_API_BASE), and it can carry a credential in its user information and its query,
// which the URL every call asks holds too (urljoin.JoinURL). Each error and note of this package
// that names a call names it by the API origin as the URL rule names a URL a request asked
// (redact.AskedURL: scheme, host and path, the authority of a base with user information as
// redact.URLWithCredentials, written {origin} below) with the route Dispatch built after it
// (Client.target), a next page link by its scheme, host and path (nextCall), and the origin itself
// the same way (nextLink's refusal), so none prints a run of the base's user, password or token: a
// status error (Ref), send's errors of a request that got no answer, of an answer past the
// reader's limit and of a body that could not be read, the failure send notes on a leased pass
// (failedAnswer), the deliveries listing's status error on its first page and on a next page whose
// link repeats the base's query as GitHub's links do, a redelivery's 404 and its other status, the
// repository read's status error (CheckRepositoryResponse), and nextLink's refusal of a link to
// another origin. The twin shows the target as the client writes it holds all three, and the
// stand-in is asked the token. Captures githubapp.asked-target/<case>.
func TestAGitHubErrorNamesNoCredentialTheAPIBaseCarries(t *testing.T) {
	const failure = `{"message":"Server Error"}`
	t.Run("twin", func(t *testing.T) {
		var mu sync.Mutex
		var query string
		host := serving(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			query = r.URL.RawQuery
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		})
		base := credentialBase(host)
		asked, err := urljoin.Join(base, "/repos/acme/web/commits/main")
		if err != nil {
			t.Fatalf("join the twin's target: %v", err)
		}
		if held := redacttest.Held(asked.String()); len(held) != len(redacttest.Secrets) {
			t.Errorf("the target as the client writes it, %q, holds %v; want every marker, so a target printed as written shows them", asked, held)
		}
		if _, err := credentialClient(t, base).Ref(context.Background(), "ghs_test", "acme", "web", "main"); err == nil {
			t.Fatal("a read GitHub answered 500 answered no error")
		}
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(query, redacttest.Token) {
			t.Errorf("the stand-in was asked the query %q; want the API base's token in it, as the client asked it", query)
		}
	})

	ref := func(ctx context.Context, client *Client) string {
		_, err := client.Ref(ctx, "ghs_test", "acme", "web", "main")
		return fmt.Sprint(err)
	}
	deliveries := func(ctx context.Context, client *Client) string {
		_, err := client.Deliveries(ctx, time.Time{}, nil)
		return fmt.Sprint(err)
	}
	for _, c := range []struct {
		name string
		// host is the stand-in's host:port, under which the client's API base is credentialBase's.
		host func(t *testing.T) string
		// read makes the call and answers the text it printed or noted.
		read func(ctx context.Context, client *Client) string
		// names is what the text names the call by, {host} standing for the stand-in's host:port and
		// {origin} for the API base's origin as redact.AskedURL names it, its authority the mark.
		names string
	}{{
		name: "status",
		host: func(t *testing.T) string {
			return serving(t, answering(t, "/api/repos/acme/web/commits/main", 500, failure))
		},
		read:  ref,
		names: "GET {origin}/api/repos/acme/web/commits/main: status 500",
	}, {
		name:  "no-answer",
		host:  func(t *testing.T) string { return redirectstest.Dead(t) },
		read:  ref,
		names: "GET {origin}/api/repos/acme/web/commits/main: ",
	}, {
		name: "too-large",
		host: func(t *testing.T) string {
			return serving(t, answering(t, "/api/repos/acme/web/pulls", 200, `[{"number":7,"head":{"sha":"c0ffee"}}]`))
		},
		read: func(ctx context.Context, client *Client) string {
			_, _, _, err := client.ReadWithin(ctx, "ghs_test", "/repos/acme/web/pulls?state=open", 16)
			return fmt.Sprint(err)
		},
		names: "GET {origin}/api/repos/acme/web/pulls?state=open: ",
	}, {
		name: "read",
		host: func(t *testing.T) string {
			return strings.TrimPrefix(redacttest.CutServer(t, http.StatusOK, `{"sha":"c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"}`), "http://")
		},
		read:  ref,
		names: "read GET {origin}/api/repos/acme/web/commits/main response: ",
	}, {
		name: "failure",
		host: func(t *testing.T) string {
			return serving(t, answering(t, "/api/repos/acme/web/commits/main", 500, failure))
		},
		read: func(ctx context.Context, client *Client) string {
			ctx, attempt := githubreads.Begin(ctx)
			_, _ = client.Ref(ctx, "ghs_test", "acme", "web", "main")
			_, failed, cause := attempt.Outcome()
			return fmt.Sprintf("failed %t: %s", failed, cause)
		},
		names: "failed true: GET {origin}/api/repos/acme/web/commits/main: status 500",
	}, {
		name:  "deliveries",
		host:  func(t *testing.T) string { return serving(t, answering(t, "/api/app/hook/deliveries", 500, failure)) },
		read:  deliveries,
		names: "GET {origin}/api/app/hook/deliveries?per_page=100: status 500",
	}, {
		name: "deliveries-next",
		host: func(t *testing.T) string {
			return serving(t, nextPage(t, "http://{host}/api/app/hook/deliveries?"+accessTokenParam+"="+redacttest.Token+"&per_page=100&cursor=v1_2"))
		},
		read:  deliveries,
		names: "GET http://{host}/api/app/hook/deliveries: status 500",
	}, {
		name: "redeliver",
		host: func(t *testing.T) string {
			return serving(t, answering(t, "/api/app/hook/deliveries/42/attempts", 404, failure))
		},
		read:  func(ctx context.Context, client *Client) string { return fmt.Sprint(client.Redeliver(ctx, 42)) },
		names: "POST {origin}/api/app/hook/deliveries/42/attempts: status 404",
	}, {
		name: "redeliver-status",
		host: func(t *testing.T) string {
			return serving(t, answering(t, "/api/app/hook/deliveries/42/attempts", 500, failure))
		},
		read:  func(ctx context.Context, client *Client) string { return fmt.Sprint(client.Redeliver(ctx, 42)) },
		names: "POST {origin}/api/app/hook/deliveries/42/attempts: status 500",
	}, {
		name: "check-repository",
		host: func(t *testing.T) string { return serving(t, answering(t, "/api/repos/acme/web", 500, failure)) },
		read: func(ctx context.Context, client *Client) string {
			_, err := client.DefaultBranch(ctx, "ghs_test", "acme", "web")
			return fmt.Sprint(err)
		},
		names: "GET {origin}/api/repos/acme/web: status 500",
	}, {
		name: "next-link-origin",
		host: func(t *testing.T) string {
			return serving(t, nextPage(t, "http://collector.example/app/hook/deliveries?cursor=v1_2"))
		},
		read:  deliveries,
		names: " leaves the API origin {origin}/api",
	}} {
		t.Run(c.name, func(t *testing.T) {
			host := c.host(t)
			text := c.read(context.Background(), credentialClient(t, credentialBase(host)))
			if text == "" || text == "<nil>" {
				t.Fatal("the call printed nothing; want the error that names it")
			}
			redacttest.RequireNoPiece(t, "githubapp.asked-target/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if names := strings.NewReplacer("{host}", host, "{origin}", "http://"+redact.URLWithCredentials).Replace(c.names); !strings.Contains(text, names) {
				t.Errorf("the call printed %q; want it to name the call as %q", text, names)
			}
		})
	}
}
