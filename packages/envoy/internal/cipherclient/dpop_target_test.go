package cipherclient

import (
	"net/http"
	"strings"
	"testing"
)

// TestADPoPProofNamesTheURLAsTheServerChecksIt: a DPoP proof's htu is the request's URL as the
// server rebuilds it from its own scheme, host and the path it was asked (proof.TargetURI): no
// query and no fragment, and none of the user information the URL carries, which the request
// still sends as its Basic credentials (user-information). Control: a URL with none of the three
// is named whole (plain).
func TestADPoPProofNamesTheURLAsTheServerChecksIt(t *testing.T) {
	type reached struct{ user, htu string }
	asked := make(chan reached, 1)
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, claims, _ := decodeProof(r.Header.Get("DPoP"))
		htu, _ := claims["htu"].(string)
		user, _, _ := r.BasicAuth()
		asked <- reached{user, htu}
		w.WriteHeader(http.StatusNoContent)
	})
	host := strings.TrimPrefix(base, "http://")
	checked := base + "/broker/v1/oauth/token"
	client := &http.Client{Transport: NewDPoPTransport(testKey(t), http.DefaultTransport)}
	for _, c := range []struct{ name, url, user string }{
		{"user-information", "http://" + "demouser7q:demopass9z-not-a-secret@" + host + "/broker/v1/oauth/token?key=k3y-7Qz4k#pane", "demouser7q"},
		{"plain", checked, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("the request failed: %v", err)
			}
			_ = response.Body.Close()
			if got := <-asked; got != (reached{c.user, checked}) {
				t.Errorf("a request to %q carried user %q and a proof naming htu %q; want user %q and htu %q", c.url, got.user, got.htu, c.user, checked)
			}
		})
	}
}
