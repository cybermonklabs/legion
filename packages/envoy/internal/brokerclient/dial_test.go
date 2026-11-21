// packages/envoy/internal/brokerclient/dial_test.go

package brokerclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestAtAddressConnectsElsewhereAndKeepsTheSignedURL: a request for a canonical URL, sent through a
// DPoP transport wrapped around AtAddress, reaches the server at the address AtAddress names with
// its path, query and headers, and its proof names the canonical URL, the one the request named, as
// a server reached at an internal address checks it (RFC 9449's htu). The token's hash rides along
// (ath), since the proof is signed above the rewrite.
//
// Control: AtAddress returning base itself: the request dials the canonical host, which no server
// answers, and fails.
func TestAtAddressConnectsElsewhereAndKeepsTheSignedURL(t *testing.T) {
	type seen struct{ path, query, authorization, proof string }
	got := make(chan seen, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("DPoP")}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: NewDPoPTransport(testKey(t), AtAddress(address, http.DefaultTransport))}

	const canonical = "https://dispatch.example.invalid"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, canonical+"/api/v1/access/person?person=ada%40example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP the-read-token")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET through AtAddress(%s): %v", server.URL, err)
	}
	response.Body.Close()
	at := <-got
	if at.path != "/api/v1/access/person" || at.query != "person=ada%40example.com" || at.authorization != "DPoP the-read-token" {
		t.Fatalf("the server at %s saw %+v, want the request's own path, query and Authorization", server.URL, at)
	}
	_, claims, err := decodeProof(at.proof)
	if err != nil {
		t.Fatalf("the request reached the server with proof %q: %v", at.proof, err)
	}
	if claims["htu"] != canonical+"/api/v1/access/person" || claims["htm"] != http.MethodGet {
		t.Fatalf("the proof names htm %v htu %v, want GET %s/api/v1/access/person: the URL the request named, not the address it reached", claims["htm"], claims["htu"], canonical)
	}
	if claims["ath"] != tokenHash("the-read-token") {
		t.Fatalf("the proof's ath = %v, want the hash of the token the request presented", claims["ath"])
	}
}
