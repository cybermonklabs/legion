package projectaccess_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	ada = "ada@example.com"
	// projected is the broker's projected token for the exchange's audience, the subject a
	// ReadToken trades.
	projected = "the-brokers-projected-token"
)

// writeSubject writes token into a file of t's own, as the kubelet writes a projected token, and
// answers its path.
func writeSubject(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker-exchange-token")
	rewriteSubject(t, path, token)
	return path
}

func rewriteSubject(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newReadToken is the broker's read token traded at brokerURL through httpClient (nil for
// http.DefaultClient) from the projected token in subjectFile, on clock.
func newReadToken(t *testing.T, brokerURL string, httpClient *http.Client, subjectFile string, clock *fakeClock) *projectaccess.ReadToken {
	t.Helper()
	readToken, err := projectaccess.NewReadToken(projectaccess.ReadTokenConfig{BrokerURL: brokerURL, HTTP: httpClient, SubjectFile: subjectFile, Now: clock.read})
	if err != nil {
		t.Fatalf("NewReadToken: %v", err)
	}
	return readToken
}

// at is a client whose every request connects to address, whatever URL it names.
func at(t *testing.T, address string) *http.Client {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: brokerclient.AtAddress(parsed, http.DefaultTransport)}
}

// proofClaims decodes a compact DPoP proof's claims, unverified: the stand-in verified it, and the
// test reads what was signed.
func proofClaims(t *testing.T, proof string) map[string]any {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("the proof %q is no compact JWS", proof)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// tokenHash is the ath a proof presenting token carries.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestPersonAccessUsesExchangedDPoP: the client asks Dispatch's person route and project route
// with the broker's own read token, which it traded once at the broker's token endpoint for the
// projected token in its subject file: each request presents `Authorization: DPoP <token>` and a
// proof of its own (a fresh jti) for its method and URL, naming the token's hash; no request
// carries a bearer, and the projected token never reaches Dispatch. The trade names no session,
// since the broker's read entry fixes it.
//
// Controls: projectaccess.Client.get setting `Authorization: Bearer <token>`: the stand-in, which
// checks the proof as Dispatch does, refuses every call (ErrBrokerRefused). The read token's
// Token answering the projected token instead of trading it: the stand-in refuses it.
func TestPersonAccessUsesExchangedDPoP(t *testing.T) {
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{"PRIV": {Visibility: "private", Owner: "carol@example.com"}},
		map[string]string{ada: projectaccess.MemberFull})
	exchange := dispatch.NewExchange(t)
	clock := &fakeClock{now: time.Now()}
	client := projectaccess.New(dispatch.URL, newReadToken(t, exchange.URL, nil, writeSubject(t, projected), clock), nil)
	client.UseClock(clock.read)
	ctx := t.Context()
	if _, err := client.Person(ctx, ada); err != nil {
		t.Fatalf("Person(%s) = %v, want Dispatch's answer", ada, err)
	}
	clock.advance(projectaccess.CacheTTL + time.Second)
	if _, err := client.Person(ctx, ada); err != nil {
		t.Fatalf("Person(%s) past the cache = %v, want Dispatch's answer", ada, err)
	}
	if _, err := client.Access(ctx, "PRIV", ada); err != nil {
		t.Fatalf("Access(PRIV, %s) = %v, want Dispatch's answer", ada, err)
	}

	trades := exchange.Trades()
	if len(trades) != 1 || trades[0].SubjectToken != projected || trades[0].ProofURL != exchange.URL+brokerclient.TokenPath || trades[0].Session != "" {
		t.Fatalf("the broker's token endpoint saw %+v, want one trade of %q signed for %s naming no session", trades, projected, exchange.URL+brokerclient.TokenPath)
	}
	presented := dispatch.Presentations()
	if len(presented) != 3 {
		t.Fatalf("Dispatch was asked %d times, want 3: the person route twice and the project route once", len(presented))
	}
	token, ok := strings.CutPrefix(presented[0].Authorization, "DPoP ")
	if !ok || !strings.HasPrefix(token, "read-token-1-") {
		t.Fatalf("the first call presented %q, want the read token the trade answered, under the DPoP scheme", presented[0].Authorization)
	}
	seen := map[any]bool{}
	for i, want := range []string{"/api/v1/access/person", "/api/v1/access/person", "/api/v1/access/projects/PRIV"} {
		if presented[i].Authorization != "DPoP "+token {
			t.Errorf("call %d presented %q, want DPoP %s and never a bearer", i+1, presented[i].Authorization, token)
		}
		claims := proofClaims(t, presented[i].Proof)
		if claims["htm"] != http.MethodGet || claims["htu"] != dispatch.URL+want || claims["ath"] != tokenHash(token) {
			t.Errorf("call %d's proof names %v %v ath %v, want GET %s and the token's hash", i+1, claims["htm"], claims["htu"], claims["ath"], dispatch.URL+want)
		}
		if seen[claims["jti"]] {
			t.Errorf("call %d's proof repeats jti %v: each request signs a proof of its own", i+1, claims["jti"])
		}
		seen[claims["jti"]] = true
		if strings.Contains(presented[i].Authorization, projected) {
			t.Errorf("call %d presented the projected token to Dispatch", i+1)
		}
	}
}

// TestPersonAccessSendsNothingWithoutItsReadToken: with no read token to present, the client asks
// Dispatch nothing at all. A trade the broker refuses for good (invalid_grant) and a subject file
// it cannot read are the broker's own credential broken, ErrBrokerRefused, the second naming
// BROKER_PROJECT_ACCESS_TOKEN_FILE; a token endpoint that answers 503 is a failure to ask and not a
// refusal. None falls back to a bearer.
//
// Control: projectaccess.Client.get presenting the projected token as a bearer when the trade
// fails: Dispatch is asked, and refuses it.
func TestPersonAccessSendsNothingWithoutItsReadToken(t *testing.T) {
	for _, c := range []struct {
		name    string
		break_  func(t *testing.T, exchange *projectaccesstest.Exchange, subject string)
		refused bool
		names   string
	}{
		{"a trade refused for good", func(_ *testing.T, e *projectaccesstest.Exchange, _ string) {
			e.Refuse(http.StatusBadRequest, "invalid_grant")
		}, true, "invalid_grant"},
		{"a subject file it cannot read", func(t *testing.T, _ *projectaccesstest.Exchange, subject string) {
			if err := os.Remove(subject); err != nil {
				t.Fatal(err)
			}
		}, true, "BROKER_PROJECT_ACCESS_TOKEN_FILE"},
		{"a token endpoint answering 503", func(_ *testing.T, e *projectaccesstest.Exchange, _ string) {
			e.Refuse(http.StatusServiceUnavailable, "server_error")
		}, false, "503"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
			exchange := dispatch.NewExchange(t)
			subject := writeSubject(t, projected)
			c.break_(t, exchange, subject)
			client := projectaccess.New(dispatch.URL, newReadToken(t, exchange.URL, nil, subject, &fakeClock{now: time.Now()}), nil)
			_, err := client.Person(t.Context(), ada)
			if err == nil || errors.Is(err, projectaccess.ErrBrokerRefused) != c.refused || !strings.Contains(err.Error(), c.names) {
				t.Fatalf("Person(%s) = %v, want an error naming %s that is ErrBrokerRefused: %t", ada, err, c.names, c.refused)
			}
			if calls := dispatch.Calls(); calls != 0 {
				t.Fatalf("Dispatch was asked %d times with no read token to present, want none", calls)
			}
		})
	}
}

// TestPersonAccessRenewsBeforeExpiry: a request finds the read token in hand while it is good for
// more than five minutes, and trades for a new one inside its own call once it is not; and the
// renewal loop (ReadToken.Run) trades again five minutes before the token's expiry, on its own
// timer, with nothing asking.
//
// Controls: brokerclient.DefaultRenewBefore of 0: the request at 301 seconds presents the first
// token. ReadToken.Run trading once and returning: the loop's second trade never comes.
func TestPersonAccessRenewsBeforeExpiry(t *testing.T) {
	t.Run("a request", func(t *testing.T) {
		dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
		exchange := dispatch.NewExchange(t)
		exchange.SetExpiresIn(600)
		clock := &fakeClock{now: time.Now()}
		client := projectaccess.New(dispatch.URL, newReadToken(t, exchange.URL, nil, writeSubject(t, projected), clock), nil)
		client.UseClock(clock.read)
		ask := func(when string, wantTrades int) string {
			t.Helper()
			if _, err := client.Person(t.Context(), ada); err != nil {
				t.Fatalf("Person(%s) %s = %v", ada, when, err)
			}
			if trades := len(exchange.Trades()); trades != wantTrades {
				t.Fatalf("%s the broker's token endpoint saw %d trades, want %d", when, trades, wantTrades)
			}
			presented := dispatch.Presentations()
			return presented[len(presented)-1].Authorization
		}
		first := ask("at first", 1)
		// Each step is more than CacheTTL after the one before, so each asks Dispatch.
		clock.advance(270 * time.Second)
		if again := ask("270 seconds on, its token good for 330 more", 1); again != first {
			t.Fatalf("270 seconds on the client presented %q, want the token in hand %q", again, first)
		}
		clock.advance(31 * time.Second)
		if renewed := ask("301 seconds on, its token good for 299 more", 2); renewed == first {
			t.Fatalf("301 seconds on the client presented the first token %q, want the one it traded for then", first)
		}
	})
	t.Run("the renewal loop", func(t *testing.T) {
		dispatch := projectaccesstest.NewServer(t, nil, nil)
		exchange := dispatch.NewExchange(t)
		exchange.SetExpiresIn(600)
		waits, fire := make(chan time.Duration, 4), make(chan time.Time)
		readToken, err := projectaccess.NewReadToken(projectaccess.ReadTokenConfig{
			BrokerURL: exchange.URL, SubjectFile: writeSubject(t, projected),
			After: func(d time.Duration) <-chan time.Time { waits <- d; return fire },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(t.Context())
		ran := make(chan error, 1)
		go func() { ran <- readToken.Run(ctx) }()
		if wait := (<-waits).Round(time.Second); wait != 300*time.Second {
			t.Fatalf("the loop waits %s after its first trade, want 300s: five minutes before the 600-second token's expiry", wait)
		}
		fire <- time.Now()
		<-waits
		if trades := len(exchange.Trades()); trades != 2 {
			t.Fatalf("the broker's token endpoint saw %d trades once the loop's timer fired, want 2", trades)
		}
		stop()
		if err := <-ran; err != nil {
			t.Fatalf("Run = %v, want nil once its context ends", err)
		}
	})
}

// TestPersonAccessRereadsProjectedIdentity: each trade reads the subject file again, so a
// projected token the kubelet rewrote is the one the next trade presents.
//
// Control: NewReadToken reading the file once, at construction: the second trade presents the
// first token.
func TestPersonAccessRereadsProjectedIdentity(t *testing.T) {
	dispatch := projectaccesstest.NewServer(t, nil, map[string]string{ada: projectaccess.MemberFull})
	exchange := dispatch.NewExchange(t)
	exchange.SetExpiresIn(600)
	clock := &fakeClock{now: time.Now()}
	subject := writeSubject(t, "projected-1")
	client := projectaccess.New(dispatch.URL, newReadToken(t, exchange.URL, nil, subject, clock), nil)
	client.UseClock(clock.read)
	if _, err := client.Person(t.Context(), ada); err != nil {
		t.Fatal(err)
	}
	rewriteSubject(t, subject, "projected-2")
	clock.advance(301 * time.Second)
	if _, err := client.Person(t.Context(), ada); err != nil {
		t.Fatal(err)
	}
	trades := exchange.Trades()
	if len(trades) != 2 || trades[0].SubjectToken != "projected-1" || trades[1].SubjectToken != "projected-2" {
		t.Fatalf("the broker's token endpoint saw %+v, want the file's first token and then its rewrite", trades)
	}
}

// TestPersonAccessSignsCanonicalOrigin: with BROKER_PROJECT_ACCESS_URL a canonical name the broker
// cannot reach and the dial address (BROKER_PROJECT_ACCESS_DIAL_URL) the stand-in's own, every call
// connects to the stand-in and its proof names the canonical URL, which is the one the stand-in
// checks, as Dispatch checks DISPATCH_SERVER_URL; the trade connects to the token endpoint's
// address and signs the broker's public URL. A client that signs the dial address instead is
// refused.
//
// Control: the same client built without the dial transport: it dials the canonical name, which
// nothing answers, and Person fails without asking.
func TestPersonAccessSignsCanonicalOrigin(t *testing.T) {
	const canonical, brokerPublic = "https://dispatch.example.invalid", "https://broker.example.invalid"
	dispatch := projectaccesstest.NewServerAt(t, canonical, nil, map[string]string{ada: projectaccess.MemberFull})
	exchange := dispatch.NewExchange(t)
	readToken := newReadToken(t, brokerPublic, at(t, exchange.URL), writeSubject(t, projected), &fakeClock{now: time.Now()})
	if _, err := projectaccess.New(canonical, readToken, at(t, dispatch.URL)).Person(t.Context(), ada); err != nil {
		t.Fatalf("Person(%s) at %s through %s = %v, want Dispatch's answer", ada, canonical, dispatch.URL, err)
	}
	if trades := exchange.Trades(); len(trades) != 1 || trades[0].ProofURL != brokerPublic+brokerclient.TokenPath {
		t.Fatalf("the trade signed %+v, want the broker's public URL %s", trades, brokerPublic+brokerclient.TokenPath)
	}
	presented := dispatch.Presentations()
	if claims := proofClaims(t, presented[0].Proof); claims["htu"] != canonical+"/api/v1/access/person" {
		t.Fatalf("the call reaching %s signed %v, want %s/api/v1/access/person", dispatch.URL, claims["htu"], canonical)
	}
	if _, err := projectaccess.New(dispatch.URL, readToken, nil).Person(t.Context(), ada); !errors.Is(err, projectaccess.ErrBrokerRefused) {
		t.Fatalf("Person(%s) signing the dial address %s = %v, want Dispatch's refusal (ErrBrokerRefused)", ada, dispatch.URL, err)
	}
}

// TestPersonAccessTradesWithinItsRequestsDeadline: a request that finds no token in hand trades
// within its own deadline, and so does a request that waits behind another's trade: with the
// token endpoint holding every trade, each Token call answers when its context ends, an error that
// is a failure to ask and not ErrBrokerRefused; once the endpoint answers, the next call trades.
//
// Control: ReadToken.Token waiting on brokerclient.Client.Token without its context: the second
// call waits behind the first's trade until the endpoint is released, past its deadline.
func TestPersonAccessTradesWithinItsRequestsDeadline(t *testing.T) {
	dispatch := projectaccesstest.NewServer(t, nil, nil)
	exchange := dispatch.NewExchange(t)
	release := exchange.Hold()
	t.Cleanup(release)
	readToken := newReadToken(t, exchange.URL, nil, writeSubject(t, projected), &fakeClock{now: time.Now()})
	first := make(chan error, 1)
	go func() {
		_, err := readToken.Token(context.Background())
		first <- err
	}()
	if !testwait.Holds(10*time.Second, time.Millisecond, func() bool { return exchange.Waiting() > 0 }) {
		t.Fatal("the first call's trade never reached the token endpoint")
	}
	for _, name := range []string{"behind the first call's trade", "again behind it"} {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		answered := make(chan error, 1)
		go func() {
			_, err := readToken.Token(ctx)
			answered <- err
		}()
		select {
		case err := <-answered:
			if err == nil || errors.Is(err, projectaccess.ErrBrokerRefused) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Token %s = %v, want its own deadline, which is no refusal", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Token %s did not answer within 10 s of its 200 ms deadline", name)
		}
		cancel()
	}
	release()
	if err := <-first; err != nil {
		t.Fatalf("the first call, once the endpoint answered = %v, want a token", err)
	}
	if token, err := readToken.Token(t.Context()); err != nil || !strings.HasPrefix(token, "read-token-") {
		t.Fatalf("Token once the endpoint answers = %q, %v, want the token in hand", token, err)
	}
}
