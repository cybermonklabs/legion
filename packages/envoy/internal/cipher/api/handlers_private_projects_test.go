// handlers_private_projects_test.go drives GET /v1/private-projects, the broker's own list of
// private projects, over real HTTP against a real Postgres: the shared session watch alone reads
// the list wired to the route, sorted and each key once, and a broker that wires none, or a list
// that cannot be read, is a 503 naming the kind of failure, never a list.
package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// watchAccount is the ServiceAccount BROKER_API_ACCOUNTS names as the caller session-watch,
	// the shared session watch, which holds readsPrivateList and nothing else.
	watchAccount = "system:serviceaccount:example-ns:example-session-watch"
	watchCaller  = "session-watch"
	// privateListPath is the route under test.
	privateListPath = "/v1/private-projects"
	// privateListLine is the route's own log line for a list it could not read.
	privateListLine = "the list of private projects is unknown"
)

// wirePrivateList is GET /v1/private-projects' answer as the wire carries it.
type wirePrivateList struct {
	PrivateProjects []string `json:"private_projects"`
}

// privateListServer is the test server with the session watch listed as its own caller and list
// wired to the route (nil wires none). It answers the server and the watch's own token.
func privateListServer(t *testing.T, list api.PrivateProjects) (*testServer, string) {
	t.Helper()
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.APIAccounts[watchAccount] = ciphertest.Caller(t, watchCaller)
		d.PrivateProjects = list
	})
	return ts, ts.accountToken(t, watchAccount, "", time.Hour)
}

// privateList reads the list as the caller whose ServiceAccount token is bearer.
func (ts *testServer) privateList(t *testing.T, bearer string) (int, []byte) {
	t.Helper()
	return ts.req(t, http.MethodGet, privateListPath, map[string]string{"Authorization": "Bearer " + bearer}, nil)
}

// listOf is a list that answers keys, in the order given.
func listOf(keys ...string) api.PrivateProjects {
	return func(context.Context) ([]string, error) { return keys, nil }
}

// clientList is the keys the broker's real client of Dispatch at url answers, presenting bearer:
// how the rows that drive one of Dispatch's answers through the client reach the route. The list
// the broker wires is a later change's union of its declared and Dispatch's private projects.
func clientList(url, bearer string) api.PrivateProjects {
	client := projectaccess.New(url, projectaccesstest.StaticAccount(bearer), nil)
	return func(ctx context.Context) ([]string, error) {
		projects, err := client.PrivateProjects(ctx)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(projects))
		for _, project := range projects {
			keys = append(keys, project.Key)
		}
		return keys, nil
	}
}

// stubDispatch is a Dispatch that answers every request status with body, until t ends.
func stubDispatch(t *testing.T, status int, body string) string {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(stub.Close)
	return stub.URL
}

// wantUnknown fails t unless the answer is 503 PRIVATE_LIST_UNKNOWN whose message carries cause,
// the kind of failure it names.
func wantUnknown(t *testing.T, what string, status int, body []byte, cause string) {
	t.Helper()
	answer := decode[wireError](t, body)
	if status != http.StatusServiceUnavailable || answer.Code != "PRIVATE_LIST_UNKNOWN" {
		t.Fatalf("%s = %d %s, want 503 PRIVATE_LIST_UNKNOWN: a list the broker cannot read in full is no list", what, status, body)
	}
	if !strings.Contains(answer.Error, cause) {
		t.Fatalf("%s answers %q, which does not name the kind of failure %q", what, answer.Error, cause)
	}
}

// TestOnlyTheSessionWatchReadsThePrivateList: dispatch, listener and access-review, each listed
// for its own powers, are refused 403 ACCOUNT_LACKS_POWER naming the caller and the route, and an
// account the broker does not list is refused 403 ACCOUNT_NOT_LISTED; session-watch alone reads
// the list, sorted and each key once.
func TestOnlyTheSessionWatchReadsThePrivateList(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.APIAccounts[watchAccount] = ciphertest.Caller(t, watchCaller)
		d.APIAccounts[listenerAccount] = ciphertest.Caller(t, listenerCaller)
		d.APIAccounts[reviewAccount] = ciphertest.Caller(t, reviewCaller)
		d.PrivateProjects = listOf("PRIV", "OTHER", "PRIV")
	})
	route := http.MethodGet + " " + privateListPath
	for _, c := range []struct {
		caller, account, code string
	}{
		{dispatchCaller, dispatchAccount, "ACCOUNT_LACKS_POWER"},
		{listenerCaller, listenerAccount, "ACCOUNT_LACKS_POWER"},
		{reviewCaller, reviewAccount, "ACCOUNT_LACKS_POWER"},
		{"an unlisted account", unlistedAccount, "ACCOUNT_NOT_LISTED"},
	} {
		t.Run(c.caller, func(t *testing.T) {
			status, body := ts.privateList(t, ts.accountToken(t, c.account, "", time.Hour))
			answer := decode[wireError](t, body)
			if status != http.StatusForbidden || answer.Code != c.code {
				t.Fatalf("%s as %s = %d %s, want 403 %s: only the session watch reads the private list", route, c.caller, status, body, c.code)
			}
			if c.code == "ACCOUNT_LACKS_POWER" && (!strings.Contains(answer.Error, c.caller) || !strings.Contains(answer.Error, route)) {
				t.Errorf("the refusal reads %q, which does not name the caller %q and the route %q", answer.Error, c.caller, route)
			}
		})
	}
	status, body := ts.privateList(t, ts.accountToken(t, watchAccount, "", time.Hour))
	if got := decode[wirePrivateList](t, body).PrivateProjects; status != http.StatusOK || strings.Join(got, ",") != "OTHER,PRIV" {
		t.Fatalf("%s as session-watch = %d %s, want 200 [OTHER PRIV]: the list wired to the route, sorted and each key once", route, status, body)
	}
}

// TestAnUnknownListIsA503NeverAShortList: a broker that wires no list answers the watch 503
// PRIVATE_LIST_UNKNOWN saying so, and a list that cannot be read, here a Dispatch that is down
// before any answer, 503 naming the kind of failure; neither is ever a list.
func TestAnUnknownListIsA503NeverAShortList(t *testing.T) {
	t.Run("no list wired", func(t *testing.T) {
		ts, watch := privateListServer(t, nil)
		status, body := ts.privateList(t, watch)
		wantUnknown(t, "the list on a broker that wires none", status, body, "not wired")
	})
	t.Run("the list cannot be read", func(t *testing.T) {
		down := downDispatch(t)
		ts, watch := privateListServer(t, clientList(down.URL, down.Token))
		status, body := ts.privateList(t, watch)
		wantUnknown(t, "the list while Dispatch is down", status, body, redact.ClassRequestFailed)
	})
}

// TestAMalformedK3AnswerIsK3Down: a Dispatch answering 200 with no projects key is a list that
// cannot be read, never one with no private project, so the route answers 503, not 200 []. The
// client refuses each malformed answer (projectaccess/private_projects_test.go, its 404, {},
// null and non-array rows); this one row shows that refusal reaching the route as a 503.
func TestAMalformedK3AnswerIsK3Down(t *testing.T) {
	ts, watch := privateListServer(t, clientList(stubDispatch(t, http.StatusOK, `{}`), "example-dispatch-access"))
	status, body := ts.privateList(t, watch)
	wantUnknown(t, "the list while Dispatch answers {}", status, body, redact.ClassRequestFailed)
}

// TestThePrivateListNeverRepeatsTheBrokersToken: a refusal code, a refusal of the broker's token
// and a malformed projects value can each repeat the bearer the broker presented, and neither the
// 503 nor the route's log line carries it.
func TestThePrivateListNeverRepeatsTheBrokersToken(t *testing.T) {
	const token = "example-private-list-broker-token"
	logged := policytest.CaptureLog(t)
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"a refusal code", http.StatusServiceUnavailable, fmt.Sprintf(`{"code":%q}`, token)},
		{"a refused token", http.StatusForbidden, fmt.Sprintf(`{"code":%q}`, token)},
		{"a malformed list", http.StatusOK, fmt.Sprintf(`{"projects":%q}`, token)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts, watch := privateListServer(t, clientList(stubDispatch(t, c.status, c.body), token))
			status, body := ts.privateList(t, watch)
			if status != http.StatusServiceUnavailable || decode[wireError](t, body).Code != "PRIVATE_LIST_UNKNOWN" {
				t.Fatalf("the list with %s = %d %s, want 503 PRIVATE_LIST_UNKNOWN", c.name, status, body)
			}
			if strings.Contains(string(body), token) {
				t.Fatal("the private-list refusal repeats the broker's token")
			}
		})
	}
	text := logged.String()
	if !strings.Contains(text, privateListLine) {
		t.Fatalf("the log capture holds no %q line, so it cannot show the token is absent:\n%s", privateListLine, text)
	}
	if strings.Contains(text, token) {
		t.Fatal("the private-list log line repeats the broker's token")
	}
}

// TestThePrivateListLogsItsFailureOncePerCacheTTL: the watch asks on every pass, from every
// replica, so a list that cannot be read is logged once per projectaccess.CacheTTL, not once per
// call: three failed calls in a row write one line.
func TestThePrivateListLogsItsFailureOncePerCacheTTL(t *testing.T) {
	logged := policytest.CaptureLog(t)
	ts, watch := privateListServer(t, func(context.Context) ([]string, error) {
		return nil, errors.New("dispatch is down in this test")
	})
	for range 3 {
		status, body := ts.privateList(t, watch)
		wantUnknown(t, "the list while it cannot be read", status, body, redact.ClassRequestFailed)
	}
	if lines := strings.Count(logged.String(), privateListLine); lines != 1 {
		t.Fatalf("three failed reads within CacheTTL wrote %d %q lines, want 1:\n%s", lines, privateListLine, logged)
	}
}

// TestThePrivateListAnswersAnEmptyArrayWhenNoProjectIsPrivate: with no project private, the answer
// carries private_projects as an empty array, never JSON null and never no key, which a reader
// could take for a list it could not read.
func TestThePrivateListAnswersAnEmptyArrayWhenNoProjectIsPrivate(t *testing.T) {
	ts, watch := privateListServer(t, listOf())
	status, body := ts.privateList(t, watch)
	if status != http.StatusOK || !strings.Contains(string(body), `"private_projects":[]`) {
		t.Fatalf("the list with no private project = %d %s, want 200 carrying \"private_projects\":[]", status, body)
	}
}
