package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// redirectRowBearer is the bearer the redirect rows' requests carry, which their stand-ins echo.
const redirectRowBearer = "not-a-secret-stack-RDX-3f58"

// bearerEcho serves, until t ends, a stand-in that answers every request 302 Found with the
// Location location makes of the bearer the request carried, and answers its base URL.
func bearerEcho(t *testing.T, location func(bearer string) string) string {
	t.Helper()
	return redirectstest.Echo(t, "Authorization", func(authorization string) string {
		return location(strings.TrimPrefix(authorization, "Bearer "))
	})
}

// redirectCases are the two Locations a print row is answered with: one net/http cannot parse
// (unparsed), which is net/http's error whatever the client's redirect policy, since Go parses a
// Location before it asks CheckRedirect, and one naming an address on another host nothing
// listens at (elsewhere), which a following client asks, naming the Location in its error.
func redirectCases(t *testing.T) []struct {
	name     string
	location func(bearer string) string
} {
	dead := redirectstest.Dead(t)
	return []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
	}
}

// requireNoLocation fails t where printed, which capture names, holds a run of the bearer or a
// word of the Location.
func requireNoLocation(t *testing.T, capture, printed string) {
	t.Helper()
	redacttest.RequireNoPiece(t, capture, printed, redirectRowBearer)
	if strings.Contains(printed, "moved") {
		t.Errorf("%s names the Location: %q", capture, printed)
	}
}

// TestARedirectEchoReachesNetHTTPsOwnError is the twin of every row here: the stand-in's echo of
// the bearer reaches net/http's own error, so a row whose output holds none of it has kept it out.
func TestARedirectEchoReachesNetHTTPsOwnError(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, bearerEcho(t, redirectstest.Unparsed)+"/api/v1/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+redirectRowBearer)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
	}
	if !strings.Contains(err.Error(), redirectRowBearer) {
		t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
	}
}

// TestADispatchCallRedirectPrintsNoLocationOfIt: whatever answers at Dispatch's address answers a
// load-test call, which carries the stack's bearer, with a redirect whose Location quotes that
// bearer (redirectCases). What a caller prints of the call (its status, its words and its error,
// as the seed's messages print them) holds no run of the bearer and no word of the Location, and
// the error's class, which a run's summary counts by, is net/http's own error's. The stand-in a
// Location on another host names is asked nothing, by a call (followed) or by a replay's action
// (replay-followed). Captures loadtest.api-redirect/<case>.
func TestADispatchCallRedirectPrintsNoLocationOfIt(t *testing.T) {
	for _, c := range redirectCases(t) {
		t.Run(c.name, func(t *testing.T) {
			res := newAPI(bearerEcho(t, c.location), redirectRowBearer, 1).call(context.Background(), http.MethodGet, "/api/v1/projects", who{}, nil)
			if res.ok() {
				t.Fatalf("the call answered %d; want a failure", res.status)
			}
			requireNoLocation(t, "loadtest.api-redirect/"+c.name, fmt.Sprintf("%d %s %v", res.status, res.text(), res.err))
			if c.name == "unparsed" {
				response, own := http.Get(bearerEcho(t, c.location))
				if own == nil {
					_ = response.Body.Close()
					t.Fatal("net/http answered the unparsed Location with no error")
				}
				if got, want := errClass(res.err), errClass(own); got != want {
					t.Errorf("errClass = %q; want %q, net/http's own error's", got, want)
				}
			}
		})
	}
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		res := newAPI(bearerEcho(t, func(bearer string) string { return redirectstest.To(target, bearer) }), redirectRowBearer, 1).
			call(context.Background(), http.MethodGet, "/api/v1/projects", who{}, nil)
		if res.err != nil || res.status != http.StatusFound {
			t.Errorf("the call answered %d, %v; want the redirect's own 302", res.status, res.err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
	t.Run("replay-followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		// The issue the action writes on is the replay fixture's first.
		key := rpState().Issues[0].Key
		p := rpPlan(t, rpOptions(), []string{
			rpEvent(1, "comment.created", key, rpSession, time.Second, map[string]any{"id": "N1", "body": "b"}),
		}, nil)
		rr := &replayRun{setup: &replaySetup{plan: p, opt: rpOptions()}, futures: map[replayRef]*replayFuture{}, lg: log.New(io.Discard, "", 0),
			ids: map[[2]string]string{{idIssue, key}: key}, writes: map[string]*replayWriteCount{}, skipped: map[string]int{}}
		for _, a := range p.Actions {
			if rr.writes[a.Kind] == nil {
				rr.writes[a.Kind] = &replayWriteCount{}
			}
			for _, st := range a.Stages {
				for _, r := range st {
					for _, pr := range r.Produces {
						rr.futures[pr.Ref] = &replayFuture{done: make(chan struct{})}
					}
				}
			}
		}
		recs := newCollector[reqRec](4)
		base := bearerEcho(t, func(bearer string) string { return redirectstest.To(target, bearer) })
		for _, a := range p.Actions {
			rr.do(context.Background(), base, redirectRowBearer, a, time.Now(), recs)
		}
		if got := recs.snapshot(); len(got) != 1 || got[0].status != http.StatusFound {
			t.Errorf("the replay recorded %+v; want one request, answered by the redirect's own 302", got)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}

// TestAGuardCheckRedirectPrintsNoLocationOfIt: whatever answers at Dispatch's address answers the
// guard's authenticated read, which carries the stack's agent token, with a redirect whose
// Location quotes that token (redirectCases). The guard's refusal holds no run of the token and no
// word of the Location, and the stand-in a Location on another host names is asked nothing
// (followed). Captures loadtest.guard-redirect/<case>.
func TestAGuardCheckRedirectPrintsNoLocationOfIt(t *testing.T) {
	for _, c := range redirectCases(t) {
		t.Run(c.name, func(t *testing.T) {
			err := checkStack(context.Background(), endpoints{dispatch: bearerEcho(t, c.location), token: redirectRowBearer})
			if err == nil || !strings.HasPrefix(err.Error(), "guard: dispatch check failed: ") {
				t.Fatalf("checkStack = %v; want the dispatch check refused", err)
			}
			requireNoLocation(t, "loadtest.guard-redirect/"+c.name, err.Error())
		})
	}
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		dispatch := bearerEcho(t, func(bearer string) string { return redirectstest.To(target, bearer) })
		if err := checkStack(context.Background(), endpoints{dispatch: dispatch, token: redirectRowBearer}); err == nil || !strings.Contains(err.Error(), "answered 302") {
			t.Errorf("checkStack = %v; want the dispatch check refused by the redirect's own 302", err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}

// TestACloudSQLReadRedirectPrintsNoLocationOfIt: whatever answers at Cloud Monitoring's address
// answers cloudsql-cpu's read, which carries the pod's access token, with a redirect whose
// Location quotes that token (redirectCases). The command's error holds no run of the token and no
// word of the Location, and the stand-in a Location on another host names is asked nothing
// (followed). Captures loadtest.cloudsql-redirect/<case>.
func TestACloudSQLReadRedirectPrintsNoLocationOfIt(t *testing.T) {
	for _, c := range redirectCases(t) {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := runCloudSQLCPUAgainst(t, bearerEcho(t, c.location))
			if err == nil {
				t.Fatal("cloudsql-cpu succeeded against a server that answers a redirect")
			}
			redacttest.RequireNoPiece(t, "loadtest.cloudsql-redirect/"+c.name, err.Error(), cloudsqlTestToken)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("cloudsql-cpu's error names the Location: %q", err)
			}
		})
	}
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		_, _, err := runCloudSQLCPUAgainst(t, bearerEcho(t, func(token string) string { return redirectstest.To(target, token) }))
		if err == nil || !strings.Contains(err.Error(), "Cloud Monitoring answered 302") {
			t.Errorf("cloudsql-cpu = %v; want the read refused by the redirect's own 302", err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}

// TestALoopListenerFollowsNoRedirect: whatever answers at a loop session's listener answers its
// publish, which carries the run's listener bearer, with a redirect to a stand-in on another host,
// which is asked nothing (followed): the publish is refused by the redirect's own status.
func TestALoopListenerFollowsNoRedirect(t *testing.T) {
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		listener := newLoopListener(bearerEcho(t, func(bearer string) string { return redirectstest.To(target, bearer) }), redirectRowBearer)
		if err := listener.Publish(context.Background(), listenerwire.PublishRequest{Topic: "notifications.team.a"}); err == nil || !strings.Contains(err.Error(), ": 302 ") {
			t.Errorf("Publish = %v; want the redirect's own 302", err)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}
