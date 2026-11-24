package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

// signedURLs is a Signer that signs nothing and records each URL a proof is asked for.
type signedURLs struct{ urls []string }

func (s *signedURLs) Sign(_, url string) (string, error) {
	s.urls = append(s.urls, url)
	return "the-session-proof", nil
}

func (s *signedURLs) SignRequestObject(cipheraddr.Address, []string, string, *int, []string) (string, error) {
	return "", nil
}

// TestTheClientJoinsItsRoutesOntoTheBrokerAddress: AGENT_SECRETS_URL is the broker's address as
// configured, and it can carry a path, a query, user information and a port. Every call the client
// makes goes to the address's path with its route joined onto it, the address's query kept ahead of
// the route's own: a status read that asks the broker to hold it (GET
// /broker/v1/requests/req-1?sig=...&wait=5, signed) and an unsigned reread (POST
// /broker/v1/secrets/API_KEY/reread?sig=...), where the route appended to the address's text sat
// inside its query (/broker?sig=.../v1/requests/req-1?wait=5, the broker asked /broker) (query,
// query-trailing-slash). The proof names the call's URL as the broker checks it, proof.TargetURI:
// with no query, and without the user information the request still carries (user-information),
// which the broker's public URL never holds. An address with neither is called where it was
// (no-query). A stand-in records the path, the query and the Basic user each call reaches it with.
func TestTheClientJoinsItsRoutesOntoTheBrokerAddress(t *testing.T) {
	type reached struct{ method, path, query, user string }
	calls := make(chan reached, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		calls <- reached{r.Method, r.URL.Path, r.URL.RawQuery, user}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"pending"}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	signed := "http://" + host + "/broker/v1/requests/req-1"
	for _, c := range []struct{ name, address, query, user string }{
		{"query", "http://" + host + "/broker?sig=k3y-7Qz4k", "sig=k3y-7Qz4k", ""},
		{"query-trailing-slash", "http://" + host + "/broker/?sig=k3y-7Qz4k", "sig=k3y-7Qz4k", ""},
		{"user-information", "http://" + "ada:pass-7Qz4k@" + host + "/broker", "", "ada"},
		{"no-query", "http://" + host + "/broker", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := newClient(cipheraddr.New(c.address))
			signer := &signedURLs{}
			if _, _, err := client.GetRequest(context.Background(), signer, "req-1", 5); err != nil {
				t.Fatalf("GetRequest: %v", err)
			}
			if _, err := client.RereadSecret(context.Background(), nil, "API_KEY"); err != nil {
				t.Fatalf("RereadSecret: %v", err)
			}
			withWait := "wait=5"
			if c.query != "" {
				withWait = c.query + "&" + withWait
			}
			for _, want := range []reached{
				{http.MethodGet, "/broker/v1/requests/req-1", withWait, c.user},
				{http.MethodPost, "/broker/v1/secrets/API_KEY/reread", c.query, c.user},
			} {
				if got := <-calls; got != want {
					t.Errorf("an address %q was called %+v; want %+v", c.address, got, want)
				}
			}
			if len(signer.urls) != 1 || signer.urls[0] != signed {
				t.Errorf("the proof for an address %q signed %q; want one, for %q", c.address, signer.urls, signed)
			}
		})
	}
}
