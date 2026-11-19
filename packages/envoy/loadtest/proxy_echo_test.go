package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/loadtest/listenerclient"
)

// The credentials the rows' requests carry beside the stack's agent bearer (echoAgentBearer, which
// answertext_test.go holds with the gateway these rows share): the listener's API token, the pod's
// Google access token, and a URL's userinfo and password query parameter. Each is long enough that
// excerpt redacts every run of eight of its characters, and none holds a word, so no piece of one
// can turn up in a message's own prose.
const (
	echoListenerToken  = "ltlt-0d94b6e2a8c1f357"
	echoGoogleToken    = "ya29.c9b20d8465e1a7c3f"
	echoPprofUser      = "pu-31c8e4a2f6"
	echoPprofSecret    = "pw-8c2e4a6b19"
	echoQuerySecret    = "qp-b73d1f05e2"
	echoDispatchRefuse = `{"error":"issue LGA-7 has no references document","code":"NOT_FOUND"}`
)

// withPprofUserinfo is base with echoPprofUser and echoPprofSecret as its userinfo, which net/http
// sends as Basic authorization.
func withPprofUserinfo(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(echoPprofUser, echoPprofSecret)
	return u.String()
}

// TestProxyEchoDispatchAnswers: every message a run returns that names an answer of Dispatch's
// (callResult's body) names a gateway's page in one line with the agent bearer its request carried
// redacted (the loop writer's log lines are answertext_test.go's rows). Each row drives one of the
// callers through the real client; the gateway answers 403, which a retry does not repeat, or 200
// for an answer that must decode.
func TestProxyEchoDispatchAnswers(t *testing.T) {
	ctx := context.Background()
	is := func(method, suffix string) func(r *http.Request) bool {
		return func(r *http.Request) bool { return r.Method == method && strings.HasSuffix(r.URL.Path, suffix) }
	}
	every := func(*http.Request) bool { return true }
	returned := func(drive func(a *api) error) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			err := drive(newAPI(base, echoAgentBearer, 4))
			if err == nil {
				t.Fatal("the run returned no error")
			}
			return err.Error()
		}
	}
	const refused, unread = `a body that is not Dispatch's error JSON (`, `a body that is not the answer asked for (`
	for _, row := range []struct {
		name   string
		status int
		echo   func(r *http.Request) bool
		form   string
		drive  func(t *testing.T, base string) string
	}{
		{"seed.go create project, its URL's userinfo sent as Basic auth", http.StatusForbidden, is(http.MethodPost, "/api/v1/projects"), refused,
			func(t *testing.T, base string) string {
				withUserinfo := withPprofUserinfo(t, base)
				_, err := seed(ctx, newAPI(withUserinfo, echoAgentBearer, 4), 0, 1)
				if err == nil {
					t.Fatal("the run returned no error")
				}
				return err.Error()
			}},
		{"seed.go create issue", http.StatusForbidden, is(http.MethodPost, "/api/v1/issues"), refused, returned(func(a *api) error {
			_, err := seedIssueWith(ctx, a, "ses-seed-0", "LGA", 0, 0, 0)
			return err
		})},
		{"seed.go decode issue", http.StatusOK, is(http.MethodPost, "/api/v1/issues"), unread, returned(func(a *api) error {
			_, err := seedIssueWith(ctx, a, "ses-seed-0", "LGA", 0, 0, 0)
			return err
		})},
		{"seed.go seed comment", http.StatusForbidden, is(http.MethodPost, "/comments"), refused, returned(func(a *api) error {
			_, err := seedIssueWith(ctx, a, "ses-seed-0", "LGA", 0, 0, 1)
			return err
		})},
		{"seed.go route", http.StatusForbidden, is(http.MethodPatch, ""), refused, returned(func(a *api) error {
			_, err := seed(ctx, a, 0, 16)
			return err
		})},
		{"refs.go target version", http.StatusForbidden, every, refused, returned(func(a *api) error {
			_, err := writeRefTargets(ctx, a, refPlan{targets: []refTarget{{n: 0, issue: "LGA-1", versions: 1, writers: []int{0}, order: []int{0}}}})
			return err
		})},
		{"refs.go source", http.StatusForbidden, every, refused, returned(func(a *api) error {
			_, _, err := writeRefSources(ctx, a, refPlan{targets: []refTarget{{n: 0, issue: "LGA-1", versions: 1}},
				sources: map[string][]int{"LGA-2": {0}}, lga: []string{"LGA-2"}})
			return err
		})},
		{"refs.go references", http.StatusForbidden, every, refused, returned(func(a *api) error {
			return refsShape(ctx, a, seedFile{Issues: []seedIssue{{Key: "LGA-1", Project: "LGA"}}})
		})},
		{"seedfinish.go coverage", http.StatusForbidden, every, refused, returned(func(a *api) error {
			_, err := dispatchCoverage{a}.readCoverage(ctx)
			return err
		})},
	} {
		t.Run(row.name, func(t *testing.T) {
			gateway := newProxyEcho(t, row.status, row.echo, dispatchStub)
			out := row.drive(t, gateway.srv.URL)
			requireRedacted(t, row.name, out, gateway.credentials(), row.form)
		})
	}
}

// TestProxyEchoServersKeepTheirOwnRefusal: each server's own error JSON is printed as it always
// was, whole, after its status: Dispatch's, the listener's and Google's.
func TestProxyEchoServersKeepTheirOwnRefusal(t *testing.T) {
	const listenerRefuse = `{"error":"unauthorized"}`
	const googleRefuse = `{"error":{"code":403,"message":"Permission monitoring.timeSeries.list denied","status":"PERMISSION_DENIED"}}`
	ctx := context.Background()
	ws := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name, refusal string
		status        int
		drive         func(base string) error
	}{
		{"Dispatch", echoDispatchRefuse, http.StatusNotFound, func(base string) error {
			return refsShape(ctx, newAPI(base, echoAgentBearer, 4), seedFile{Issues: []seedIssue{{Key: "LGA-7", Project: "LGA"}}})
		}},
		{"the listener", listenerRefuse, http.StatusUnauthorized, func(base string) error {
			reg := &registrar{listener: &listenerclient.Client{URL: base, Token: echoListenerToken, HTTP: http.DefaultClient},
				bodies: []listenerwire.SubscribeRequest{{SessionID: "ses-lg-0"}}, relay: newRelaySessions(1, 1)}
			return reg.register(ctx, 0)
		}},
		{"Google", googleRefuse, http.StatusForbidden, func(base string) error {
			_, err := readCloudSQLSeries(ctx, http.DefaultClient, base, echoGoogleToken, "lt-project",
				"cloudsql.googleapis.com/database/cpu/utilization", "lt-project:lt-instance", ws, ws.Add(time.Hour))
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(row.status)
				_, _ = io.WriteString(w, row.refusal)
			}))
			t.Cleanup(server.Close)
			if err := row.drive(server.URL); err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("the refusal reads %v, want %s's own error JSON whole", err, row.name)
			}
		})
	}
}

// TestProxyEchoGuard: the guard's two authenticated checks name a gateway's page with the token
// each check presented redacted.
func TestProxyEchoGuard(t *testing.T) {
	for _, row := range []struct {
		name, route, form string
	}{
		{"guard.go dispatch check", "/api/v1/projects", `a body that is not Dispatch's error JSON (`},
		{"guard.go listener check", "/v1/sessions", `a body that is not the listener's error JSON (`},
	} {
		t.Run(row.name, func(t *testing.T) {
			gateway := newProxyEcho(t, http.StatusForbidden, func(r *http.Request) bool { return r.URL.Path == row.route }, dispatchStub)
			err := checkStack(context.Background(), endpoints{dispatch: gateway.srv.URL, token: echoAgentBearer,
				listener: gateway.srv.URL, listenerToken: echoListenerToken})
			if err == nil {
				t.Fatal("the guard passed a gateway's page")
			}
			requireRedacted(t, row.name, err.Error(), gateway.credentials(), row.form)
		})
	}
}

// TestProxyEchoLoopListener: a loop session's refused post (listenerclient's Refusal), and a 2xx
// unsubscribe answer that does not decode, name a gateway's page with the listener token redacted.
func TestProxyEchoLoopListener(t *testing.T) {
	ctx := context.Background()
	t.Run("listenerclient.go refusal", func(t *testing.T) {
		gateway := newProxyEcho(t, http.StatusForbidden, nil, nil)
		err := newLoopListener(gateway.srv.URL, echoListenerToken).Publish(ctx, listenerwire.PublishRequest{Topic: "issue.LOOP-1"})
		if err == nil {
			t.Fatal("a gateway's 403 was taken")
		}
		requireRedacted(t, "the post's error", err.Error(), gateway.credentials(), `a body that is not the listener's error JSON (`)
	})
	t.Run("listenerclient.go unsubscribe", func(t *testing.T) {
		gateway := newProxyEcho(t, http.StatusOK, nil, nil)
		s := &loopSession{id: "ses-lg-unit-00001", listener: newLoopListener(gateway.srv.URL, echoListenerToken), topics: []string{"issue.LOOP-1"}}
		_, err := (&loopSessions{}).unsubscribe(ctx, s)
		if err == nil {
			t.Fatal("a gateway's 200 page was read as the listener's answer")
		}
		requireRedacted(t, "the unsubscribe's error", err.Error(), gateway.credentials(), `a body that is not the listener's unsubscribe answer (`)
	})
}

// TestProxyEchoRegistrar: a step's refused registration names a gateway's page with the listener
// token redacted.
func TestProxyEchoRegistrar(t *testing.T) {
	gateway := newProxyEcho(t, http.StatusForbidden, nil, nil)
	reg := &registrar{listener: &listenerclient.Client{URL: gateway.srv.URL, Token: echoListenerToken, HTTP: http.DefaultClient},
		bodies: []listenerwire.SubscribeRequest{{SessionID: "ses-lg-0"}}, relay: newRelaySessions(1, 1)}
	err := reg.register(context.Background(), 0)
	if err == nil {
		t.Fatal("a gateway's 403 was taken as a registration")
	}
	requireRedacted(t, "sessions.go register", err.Error(), gateway.credentials(), `a body that is not the listener's error JSON (`)
}

// TestProxyEchoCloudMonitoring: each refused or undecodable Cloud Monitoring read names a gateway's
// page with the pod's access token redacted.
func TestProxyEchoCloudMonitoring(t *testing.T) {
	ws := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name   string
		status int
		form   string
	}{
		{"cloudsqlcpu.go 403", http.StatusForbidden, `a body that is not Google's error JSON (`},
		{"cloudsqlcpu.go 404", http.StatusNotFound, `a body that is not Google's error JSON (`},
		{"cloudsqlcpu.go other status", http.StatusBadGateway, `a body that is not Google's error JSON (`},
		{"cloudsqlcpu.go 200 that does not decode", http.StatusOK, `a body that is not a timeSeries list (`},
	} {
		t.Run(row.name, func(t *testing.T) {
			gateway := newProxyEcho(t, row.status, nil, nil)
			_, err := readCloudSQLSeries(context.Background(), http.DefaultClient, gateway.srv.URL, echoGoogleToken, "lt-project",
				"cloudsql.googleapis.com/database/cpu/utilization", "lt-project:lt-instance", ws, ws.Add(time.Hour))
			if err == nil {
				t.Fatal("a gateway's page was read as a time series")
			}
			requireRedacted(t, row.name, err.Error(), gateway.credentials(), row.form)
		})
	}
}

// TestProxyEchoURLUserinfo: a fetch of a URL that carries userinfo, which net/http sends as
// Authorization: Basic, and a password query parameter names a gateway's page with the encoding,
// the user name and both passwords redacted, as its URL is printed without them.
func TestProxyEchoURLUserinfo(t *testing.T) {
	gateway := newProxyEcho(t, http.StatusForbidden, nil, nil)
	withCredentials := withPprofUserinfo(t, gateway.srv.URL)
	t.Run("util.go fetchTo", func(t *testing.T) {
		var out bytes.Buffer
		fetchTo(withCredentials+"/debug/pprof/heap?password="+echoQuerySecret, filepath.Join(t.TempDir(), "heap.pprof"), 10*time.Second, log.New(&out, "", 0))
		requireRedacted(t, "the fetch's log line", out.String(), gateway.credentials(), `a body that is not the answer asked for (`)
	})
	t.Run("sample.go getOK", func(t *testing.T) {
		_, err := getOK(context.Background(), http.DefaultClient, withCredentials+"/varz?password="+echoQuerySecret)
		if err == nil {
			t.Fatal("a gateway's 403 was taken")
		}
		requireRedacted(t, "the read's error", err.Error(), gateway.credentials(), `a body that is not the answer asked for (`)
	})
}
