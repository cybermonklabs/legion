package holdrecord_test

// The operator's client against stand-ins in the broker's place, which answer what no broker
// writes and whatever answers at the broker's address can: a redirect, a Location or a status line
// that does not parse, a body that cannot be read, and words of an answer that does not verify,
// each echoing the operator's token the request carried. None of it reaches an error the client
// answers, and the client's token is sent nowhere but the broker. These rows need no database.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// operatorToken is the operator's ServiceAccount token in these rows. Eleven of its characters,
// operatorKeyID, are the unpadded base64url of eight bytes, the one text signedrecord reads a key
// id in, so a stand-in can answer them as a signature's key id.
const (
	operatorToken = "hold-ab-RDX-cdeA-not-a-secret-token-7c1e"
	operatorKeyID = "ab-RDX-cdeA"
	wordsHold     = "h-words"
)

// bearerOf is the operator's token the request carried, as a stand-in echoes it.
func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// wordsClient is a client of brokerURL with operatorToken and a signing key of its own, whose
// public half it verifies under.
func wordsClient(t *testing.T, brokerURL string) (*holdrecord.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := holdrecord.New(holdrecord.Config{
		BrokerURL:  brokerURL,
		Token:      func(context.Context) (string, error) { return operatorToken, nil },
		SigningKey: priv,
		VerifyKeys: []ed25519.PublicKey{pub},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, priv
}

// cohortAnswer is the records list a broker answers for wordsHold's cohort: one record, signed
// with key, which the client verifies and then deletes, signing the delete over its gen.
func cohortAnswer(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	value := json.RawMessage(`{"raised":["a"]}`)
	sig, err := signedrecord.Sign(key, signedrecord.Record(wordsHold, "cohort", "cohort", 1, value))
	if err != nil {
		t.Fatal(err)
	}
	return `{"records":[{"key":"cohort","value":` + string(value) + `,"gen":1,"sig":"` + sig + `"}]}`
}

// deleteSig is the signature the client's delete of cohortAnswer's record carries in its query.
func deleteSig(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	sig, err := signedrecord.Sign(key, signedrecord.RecordDelete(wordsHold, "cohort", "cohort", 1))
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// standIn serves handler in the broker's place until t ends, and answers its URL.
func standIn(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// cohortThen is a stand-in handler that answers the cohort's read with list and every other
// request through then.
func cohortThen(list string, then http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/records") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, list)
			return
		}
		then(w, r)
	}
}

// rawBroker serves, until t ends, a stand-in that reads each request on a connection of its own
// and writes answer of it on the connection itself, then closes it: an answer net/http's own
// server never writes. It answers the stand-in's base URL.
func rawBroker(t *testing.T, answer func(request *http.Request) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, answer(request))
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

// operatorRequest is a request to url carrying operatorToken as the client sends it, for a twin.
func operatorRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+operatorToken)
	return request
}

// TestTheRouteIsJoinedOntoTheBrokerURL: a broker URL with a path and a query of its own, as a
// gateway in front of the broker names it, is asked each route under its path, with its own query
// first and the route's after it (urljoin.JoinURL): the row's read (row) reaches
// /broker/v1/nats-hold with tenant=a, and the cohort's signed delete (delete)
// /broker/v1/nats-hold/records with tenant=a and then the delete's query, its signature among it.
// A broker URL's fragment, which no request sends, is dropped (fragment). Joined by text, the route
// sat inside the URL's query or fragment, and the request went to /broker.
func TestTheRouteIsJoinedOntoTheBrokerURL(t *testing.T) {
	ctx := context.Background()
	type asked struct{ method, path, query string }
	// keep is a stand-in that keeps each request but the cohort's read, answering it 200 {} (a
	// delete) or 404 no_row (the row's read).
	keep := func(seen *[]asked) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*seen = append(*seen, asked{r.Method, r.URL.Path, r.URL.RawQuery})
			if r.Method == http.MethodDelete {
				_, _ = io.WriteString(w, "{}")
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no_row","message":"no row"}`)
		}
	}
	for _, x := range []struct{ name, suffix string }{{"row", "/broker?tenant=a"}, {"fragment", "/broker#tenant"}} {
		t.Run(x.name, func(t *testing.T) {
			var seen []asked
			c, _ := wordsClient(t, standIn(t, keep(&seen))+x.suffix)
			var refusal *holdrecord.RouteError
			if _, err := c.Row(ctx); !errors.As(err, &refusal) || refusal.Code != "no_row" {
				t.Fatalf("Row answered %v, want the stand-in's no_row", err)
			}
			want := asked{http.MethodGet, "/broker/v1/nats-hold", ""}
			if x.name == "row" {
				want.query = "tenant=a"
			}
			if len(seen) != 1 || seen[0] != want {
				t.Errorf("the broker URL %s was asked %+v, want %+v", x.suffix, seen, want)
			}
		})
	}
	t.Run("delete", func(t *testing.T) {
		var seen []asked
		var key ed25519.PrivateKey
		handler := func(w http.ResponseWriter, r *http.Request) { cohortThen(cohortAnswer(t, key), keep(&seen))(w, r) }
		c, signing := wordsClient(t, standIn(t, handler)+"/broker?tenant=a")
		key = signing
		if err := c.DeleteCohort(ctx, wordsHold); err != nil {
			t.Fatalf("DeleteCohort answered %v", err)
		}
		if len(seen) != 1 || seen[0].method != http.MethodDelete || seen[0].path != "/broker/v1/nats-hold/records" || !strings.HasPrefix(seen[0].query, "tenant=a&") {
			t.Fatalf("the delete was asked %+v, want DELETE /broker/v1/nats-hold/records with tenant=a first", seen)
		}
		query, err := url.ParseQuery(seen[0].query)
		if err != nil || query.Get("hold_id") != wordsHold || query.Get("key") != "cohort" || query.Get("gen") != "1" || query.Get("sig") != deleteSig(t, key) {
			t.Errorf("the delete's query is %q, %v; want the hold, the record and its signed gen after tenant=a", seen[0].query, err)
		}
	})
}

// errText is err's text, and "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
