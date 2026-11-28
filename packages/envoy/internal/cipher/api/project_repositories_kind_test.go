package api_test

import (
	"net/http"
	"testing"
	"time"
)

// TestAnEdgeListenerIsKnownByItsAccountsKindToo: an edge listener's entry names a Kubernetes
// service account or a Google one, and only a token of that kind is that edge listener. A Google
// identity token whose email names the Kubernetes edge listener's subject, and a cluster token
// whose subject names the Google edge listener's address, are each 403 NOT_AN_EDGE_LISTENER, and
// neither reaches Dispatch; each edge listener's own token is answered its list, so the refusal is
// the kind's and not the caller's.
func TestAnEdgeListenerIsKnownByItsAccountsKindToo(t *testing.T) {
	ts, dispatch, googleToken := googleEdgeServer(t, "edge-listener/edge-a/PRIV="+edgeAccount+" edge-listener/edge-g/OTHER=google:"+edgeGoogleAccount)
	for _, row := range []struct {
		name, bearer, project string
	}{
		{"a Google token naming the Kubernetes edge listener's subject", googleToken(edgeAccount), "PRIV"},
		{"a cluster token naming the Google edge listener's address", ts.accountToken(t, edgeGoogleAccount, "", time.Hour), "OTHER"},
	} {
		t.Run(row.name, func(t *testing.T) {
			status, body, err := askRepositories(ts.URL, row.bearer, row.project)
			if err != nil {
				t.Fatal(err)
			}
			requireRefused(t, status, body, http.StatusForbidden, "NOT_AN_EDGE_LISTENER")
		})
	}
	if calls := dispatch.Calls(); calls != 0 {
		t.Fatalf("the refused calls asked Dispatch %d times, want none", calls)
	}
	status, body, err := askRepositories(ts.URL, ts.accountToken(t, edgeAccount, "", time.Hour), "PRIV")
	if err != nil {
		t.Fatal(err)
	}
	requireRepositories(t, status, body, "PRIV", []string{"acme/pub", "acme/two"})
	status, body, err = askRepositories(ts.URL, googleToken(edgeGoogleAccount), "OTHER")
	if err != nil {
		t.Fatal(err)
	}
	requireRepositories(t, status, body, "OTHER", []string{"acme/other"})
}
