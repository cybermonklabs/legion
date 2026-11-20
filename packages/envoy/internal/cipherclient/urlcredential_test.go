// packages/envoy/internal/cipherclient/urlcredential_test.go
//
// What a trade prints, and where it sends its tokens, when the configured broker address carries
// a credential. AGENT_SECRETS_URL may be http://user:password@host:port, or carry a query or a
// fragment, and `agent-secrets-helper` writes this client's log to stderr while `agent-secrets`
// prints its errors there: no log line and no error of a trade, taken or failed, may hold any
// part of that credential. And a trade carries the subject token, so a broker address answering
// 3xx sends nothing on to the host it names.

package cipherclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
)

// credentialMarkers stand for each part of a configured address that can carry a credential: its
// user name, its password, its query and its fragment. All are made up.
var credentialMarkers = []string{"demouser7q", "demopass9z-not-a-secret", "demoquery4k", "demofrag8w"}

// credentialed is base with each place an address can carry a credential filled in turn.
func credentialed(base string) map[string]string {
	return map[string]string{
		"a user name and password": strings.Replace(base, "://", "://demouser7q:demopass9z-not-a-secret@", 1),
		"a query":                  base + "/?token=demoquery4k",
		"a fragment":               base + "#demofrag8w",
	}
}

// leakedMarker is the first credential marker text holds, or "" when it holds none.
func leakedMarker(text string) string {
	for _, marker := range credentialMarkers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

// tradeOnce is one trade by a client of address, logging to a buffer, followed by one pass of the
// renewal loop, which logs a failed trade where Token does not: everything either logged, and
// the trade's error.
func tradeOnce(t *testing.T, address string, httpClient *http.Client) (string, error) {
	t.Helper()
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := New(Config{
		BrokerURL: cipheraddr.New(address), Key: testKey(t), Subject: projectedStub(), HTTP: httpClient,
		Log: slog.New(slog.NewTextHandler(&logged, nil)),
		// The renewal loop's first wait ends it: it has logged the pass by then.
		After: func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, tokenErr := client.Token(ctx)
	if err := client.Run(ctx, func(context.Context, *Token) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return logged.String(), tokenErr
}

// TestATradePrintsNoPartOfTheBrokerURLCredential: beside an address carrying a user name and a
// password, a query or a fragment, neither the log of a trade the broker takes nor any error of
// one that fails - a broker that never answers, an answer that is not JSON, a non-2xx status, a
// token the answer makes invalid, each branch of the answer's reading - holds any part of it,
// and neither does the renewal loop's line about it, even where the answer repeats it. Each
// trade reaches the broker, so none of this passes on a request that was never made.
func TestATradePrintsNoPartOfTheBrokerURLCredential(t *testing.T) {
	// repeated is every marker, as an answer that repeats the credential writes them.
	repeated := strings.Join(credentialMarkers, " ")
	for _, answer := range []struct {
		name   string
		status int
		body   string
		taken  bool
	}{
		{"a trade the broker takes", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":3600}`, true},
		{"a body that is not JSON", 200, `<html>a proxy's page</html>`, false},
		{"an expiry that is not a number and repeats the credential", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":"` + repeated + `"}`, false},
		{"a non-2xx status", 503, `{"error":"temporarily_unavailable"}`, false},
		{"a non-2xx status whose words repeat the credential", 503, `{"error":"` + repeated + `"}`, false},
		{"a non-2xx status with no body", 500, ``, false},
		{"no access token", 200, `{"token_type":"DPoP","expires_in":3600}`, false},
		{"a token type that is not DPoP", 200, `{"access_token":"demo-access-not-a-secret","token_type":"Bearer","expires_in":3600}`, false},
		{"a token type that repeats the credential", 200, `{"access_token":"demo-access-not-a-secret","token_type":"` + repeated + `","expires_in":3600}`, false},
		{"a refresh token", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":3600,"refresh_token":"r"}`, false},
		{"no usable expiry", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":0}`, false},
		{"a credential expiry that is no time", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":3600,"credential_expires_at":"soon"}`, false},
		{"a credential expiry that repeats the credential", 200, `{"access_token":"demo-access-not-a-secret","token_type":"DPoP","expires_in":3600,"credential_expires_at":"` + repeated + `"}`, false},
	} {
		var asked atomic.Int64
		base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			// Every path: a query or a fragment in the address moves the token path out of
			// the URL's path, and the trade must still be answered for its error to be read.
			asked.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(answer.status)
			_, _ = w.Write([]byte(answer.body))
		})
		for form, address := range credentialed(base) {
			t.Run(answer.name+" beside "+form, func(t *testing.T) {
				before := asked.Load()
				logged, tokenErr := tradeOnce(t, address, HTTPClient())
				if asked.Load() == before {
					t.Fatalf("the trade never reached the broker (Token: %v)", tokenErr)
				}
				switch {
				case answer.taken && tokenErr != nil:
					t.Fatalf("Token: %v, want the broker's token", tokenErr)
				case !answer.taken && tokenErr == nil:
					t.Fatal("Token took an answer that is not this grant's")
				case tokenErr != nil && leakedMarker(tokenErr.Error()) != "":
					t.Errorf("the trade's error holds %q: %s", leakedMarker(tokenErr.Error()), tokenErr)
				}
				if marker := leakedMarker(logged); marker != "" {
					t.Errorf("the trade's log holds %q:\n%s", marker, logged)
				}
				if !strings.Contains(logged, "broker=") {
					t.Errorf("the log names no broker at all:\n%s", logged)
				}
			})
		}
	}
}

// TestATradeTheBrokerNeverAnswersPrintsNoPartOfTheBrokerURLCredential: the same of a broker that
// refuses the connection, whose error net/http writes with the request's URL in it. The error
// still reads as the refused connection it is, through errors.Is.
func TestATradeTheBrokerNeverAnswersPrintsNoPartOfTheBrokerURLCredential(t *testing.T) {
	for form, address := range credentialed("http://127.0.0.1:1") {
		t.Run(form, func(t *testing.T) {
			logged, tokenErr := tradeOnce(t, address, HTTPClient())
			if !errors.Is(tokenErr, syscall.ECONNREFUSED) {
				t.Fatalf("Token: %v, want the refused connection", tokenErr)
			}
			if marker := leakedMarker(tokenErr.Error()); marker != "" {
				t.Errorf("the trade's error holds %q: %s", marker, tokenErr)
			}
			if marker := leakedMarker(logged); marker != "" {
				t.Errorf("the trade's log holds %q:\n%s", marker, logged)
			}
		})
	}
}

// TestACleanBrokerURLStillPrintsAsWritten is the control for the two above: an address that
// carries nothing prints whole, in the log line of a trade and in the error of one that fails,
// and so do the broker's words that error quotes.
func TestACleanBrokerURLStillPrintsAsWritten(t *testing.T) {
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	})
	logged, tokenErr := tradeOnce(t, base, HTTPClient())
	want := "the broker at " + base + TokenPath + " answered HTTP 503 temporarily_unavailable"
	if tokenErr == nil || !strings.Contains(tokenErr.Error(), want) {
		t.Errorf("Token: %v, want it to read %q", tokenErr, want)
	}
	if !strings.Contains(logged, "broker="+base) {
		t.Errorf("the log does not name the broker as written:\n%s", logged)
	}
}

// TestATradeFollowsNoRedirect: the trade's form carries the subject token, so a broker address
// answering 3xx gets that answer back as the non-2xx it is, and the host it names receives no
// request at all: not the form again (307, 308), and not the bodiless GET net/http turns a POST
// into on a 301, 302 or 303 either, which a check reading only bodies would let through. The
// broker receives the trade itself, once, with the client the caller supplies and with
// net/http's default alike.
func TestATradeFollowsNoRedirect(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		for name, httpClient := range map[string]*http.Client{"a supplied client": HTTPClient(), "the default client": nil} {
			t.Run(http.StatusText(status)+" with "+name, func(t *testing.T) {
				var asked, elsewhere atomic.Int64
				target := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
					elsewhere.Add(1)
					w.WriteHeader(http.StatusBadRequest)
				})
				broker := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
					asked.Add(1)
					http.Redirect(w, r, target+TokenPath, status)
				})
				client, err := New(Config{BrokerURL: cipheraddr.New(broker), Key: testKey(t), Subject: projectedStub(), HTTP: httpClient})
				if err != nil {
					t.Fatal(err)
				}
				_, tokenErr := client.Token(context.Background())
				if tokenErr == nil {
					t.Fatal("Token took a redirect as a token")
				}
				if got := elsewhere.Load(); got != 0 {
					t.Errorf("the redirect's target received %d requests, want none (Token: %v)", got, tokenErr)
				}
				if got := asked.Load(); got != 1 {
					t.Errorf("the broker received %d requests, want the trade once (Token: %v)", got, tokenErr)
				}
			})
		}
	}
}

// TestARefusedSignInsLogNamesItsCodeOnlyBesideACleanAddress: the broker, or whoever answers at
// its address, refuses a person's trade for good with an OAuth error whose code repeats the
// credential the configured address carries. The sign-in falls back to this machine and its log
// line says so, and beside that address the line holds no part of the credential: the code is
// the answerer's own text, so the line names it as cipheraddr.WordsNotPrinted. Beside a clean
// address the same code is what a person acts on, and the line names it as it came.
func TestARefusedSignInsLogNamesItsCodeOnlyBesideACleanAddress(t *testing.T) {
	repeated := strings.Join(credentialMarkers, " ")
	var asked atomic.Int64
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Every path: a query or a fragment in the address moves the token path out of the
		// URL's path, and the trade must still be refused for the refusal to be logged.
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + repeated + `","error_description":"this sign-in was revoked"}`))
	})
	// refused is the log of one enrolment's Current under a sign-in the broker at address
	// refuses, which must answer no authority and no error, so the session enrols under this
	// machine.
	refused := func(t *testing.T, address string) string {
		t.Helper()
		rig := newSignInRig(t, nil)
		rig.signIn(t, "ada@example.com")
		var logged bytes.Buffer
		rig.authority.BrokerURL = cipheraddr.New(address)
		rig.authority.Log = slog.New(slog.NewTextHandler(&logged, nil))
		before := asked.Load()
		if authority, err := rig.authority.Current(context.Background()); authority != nil || err != nil {
			t.Fatalf("a refused sign-in answered %+v, %v; want nil and no error", authority, err)
		}
		if asked.Load() == before {
			t.Fatal("the trade never reached the broker")
		}
		if !strings.Contains(logged.String(), "this machine's sign-in was refused") {
			t.Fatalf("the refusal was not logged:\n%s", logged.String())
		}
		return logged.String()
	}
	for form, address := range credentialed(base) {
		t.Run("beside "+form, func(t *testing.T) {
			logged := refused(t, address)
			if marker := leakedMarker(logged); marker != "" {
				t.Errorf("the sign-in's log holds %q:\n%s", marker, logged)
			}
			if !strings.Contains(logged, cipheraddr.WordsNotPrinted) {
				t.Errorf("the sign-in's log does not say its code was not printed:\n%s", logged)
			}
		})
	}
	t.Run("beside a clean address", func(t *testing.T) {
		if logged := refused(t, base); !strings.Contains(logged, repeated) {
			t.Errorf("the sign-in's log does not name the broker's code:\n%s", logged)
		}
	})
}
