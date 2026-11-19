package agentsecrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A proxy in the broker's place can answer with a page of its own that echoes the request back,
// its headers and its body. Every credential the request carried is redacted from the refusal,
// which names the page in one line: the token login's ServiceAccount token and request object, in
// its body; the launcher proof an enrollment or a revoke carries; the pod's own token in an
// enrollment's or a registration's body; and the daemon's bearer on a registration.
func TestEveryRefusalNamesAProxysPageWithoutTheCredentialsItCarried(t *testing.T) {
	var mu sync.Mutex
	var carried []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		mu.Lock()
		_, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		for _, sent := range []any{bearer, r.Header.Get("Proof"), fields["token"], fields["request"], fields["pod_token"]} {
			if text, _ := sent.(string); text != "" {
				carried = append(carried, text)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<html><body><h1>403 Forbidden</h1><pre>Authorization: %s\nProof: %s\n%s</pre></body></html>",
			r.Header.Get("Authorization"), r.Header.Get("Proof"), body)
	}))
	defer server.Close()
	ctx := context.Background()
	c := &Client{URL: server.URL, HTTP: server.Client(), TokenFile: tokenFile(t, "sa-token-Q7vK2mXr9LtW4nBz")}

	_, login := c.Login(ctx)
	withCredential(t, c, "cred-1")
	_, enroll := c.Enroll(ctx, podEnrollment)
	revoke := c.Revoke(ctx, "enr-1")
	_, carriedList := c.Carried(ctx, []string{"ses_implementer"})
	_, register := c.RegisterPodSession(ctx, "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 1)

	if len(carried) < 8 {
		t.Fatalf("the requests carried %d credentials, want the login's two, the enrollment's two, the revoke's one, the carried list's one and the registration's two", len(carried))
	}
	const page = `broker answered 403 UNKNOWN: a body that is not the broker's error JSON ("403 Forbidden Authorization: `
	for name, err := range map[string]error{"login": login, "enrollment": enroll, "revoke": revoke, "carried list": carriedList, "registration": register} {
		if err == nil || !strings.Contains(err.Error(), page) {
			t.Errorf("the %s's refusal = %v, want one naming the page as %q…", name, err, page)
			continue
		}
		for _, sent := range carried {
			if strings.Contains(err.Error(), sent) {
				t.Errorf("the %s's refusal prints the credential %q: %v", name, sent, err)
			}
		}
	}
}
