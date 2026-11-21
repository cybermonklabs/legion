package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// A cold read of the cluster's key set slower than the listener's 3 s wait but inside its 10 s
// timeout is waited out: the exchange answers the token, where a wait that gave up first would
// have answered the pod a refusal it never trades past.
func TestAnExchangeWaitsOutASlowKeySetRead(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	f.cluster.DelayKeys(3500 * time.Millisecond)
	start := time.Now()
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusOK {
		t.Fatalf("exchange while the key set takes 3.5 s to answer = %d %v after %s, want 200", status, body, time.Since(start).Round(time.Millisecond))
	}
}

// A subject token the exchange cannot judge because its issuer's key set cannot be read is
// answered 503 temporarily_unavailable, never invalid_request: cipherclient, which every pod and
// daemon trades with, stops for good on a 4xx refusal (RefusedError.Permanent) and trades again
// after a 5xx, so an outage answered as a refusal ended the trade where the next one lands. The
// client here is cipherclient itself against this broker's token endpoint, and
// the first answer it was given is read back as it went out.
func TestAKeySetOutageAtTheExchangeIsTradedAgain(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	podToken := f.podToken(t, "pod-uid-1", nil)
	f.cluster.FailKeys(http.StatusInternalServerError)

	var (
		mu         sync.Mutex
		trades     int
		refusals   int
		first      int
		firstError any
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+cipherclient.TokenPath, func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		f.provider.Token(recorder, r, oauth.TokenHooks{})
		mu.Lock()
		trades++
		if recorder.Code != http.StatusOK {
			refusals++
		}
		if trades == 1 {
			var body map[string]any
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			first, firstError = recorder.Code, body["error"]
		}
		mu.Unlock()
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
	broker := httptest.NewServer(mux)
	t.Cleanup(broker.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(podToken), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(broker.URL), Key: key, Subject: cipherclient.NewProjectedToken(tokenFile), Session: "implementer-g1",
		// Each retry waits a tenth of a second rather than the 15 s the loop starts at.
		After: func(time.Duration) <-chan time.Time { return time.After(100 * time.Millisecond) },
	})
	if err != nil {
		t.Fatalf("cipherclient.New: %v", err)
	}
	// The issuer comes back after the client has been answered the outage at least once.
	time.AfterFunc(300*time.Millisecond, func() { f.cluster.FailKeys(0) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var landed *cipherclient.Token
	err = client.Run(ctx, func(_ context.Context, token *cipherclient.Token) error {
		landed = token
		cancel()
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	if first != http.StatusServiceUnavailable || firstError != "temporarily_unavailable" {
		t.Errorf("the first trade, while the key set answered 500, was answered %d %v, want 503 temporarily_unavailable: the token was never judged", first, firstError)
	}
	if err != nil {
		t.Fatalf("Run = %v after %d trades; want the outage traded through, not the end of the loop", err, trades)
	}
	if landed == nil || landed.AccessToken == "" {
		t.Fatalf("Run ended with no token after %d trades, %d of them refused: want one once the key set reads again", trades, refusals)
	}
	if refusals < 1 {
		t.Errorf("the client traded %d times and none was answered the outage: this test proves nothing about trading past one", trades)
	}
}

// A subject token its own claims refuse is refused 400 invalid_request while its issuer's key set
// cannot be read, naming the claim, as it is refused while the set reads: its issuer, audience and
// expiry are decided before the outage, as the verifier's classify decides them
// (internal/oidc/verifier.go), so cipherclient stops on a trade no read of the set could land,
// rather than trading it again until the outage ends. The sound token first is what shows the set
// is down for every row: it is answered the outage.
func TestAnExchangeRefusesATokenItsClaimsRefuseDuringAKeySetOutage(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	f.cluster.FailKeys(http.StatusInternalServerError)

	sound := exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	if status, body := f.exchange(t, sound, key); status != http.StatusServiceUnavailable || body["error"] != "temporarily_unavailable" {
		t.Fatalf("a sound token while the key set answers 500 = %d %v, want 503 temporarily_unavailable: without the outage the rows below prove nothing", status, body)
	}
	for _, row := range []struct {
		name   string
		change func(map[string]any)
		// names is what the refusal's description must carry: the claim it refused.
		names string
	}{
		{"an expired token", func(claims map[string]any) {
			now := time.Now()
			claims["iat"], claims["exp"] = now.Add(-2*time.Hour).Unix(), now.Add(-time.Minute).Unix()
		}, "expired"},
		{"a token for another audience", func(claims map[string]any) {
			claims["aud"] = []string{"https://another.example/audience"}
		}, podAudience},
		{"a token from another issuer", func(claims map[string]any) {
			claims["iss"] = "https://another-issuer.example"
		}, f.cluster.URL()},
	} {
		t.Run(row.name, func(t *testing.T) {
			form := exchangeForm(f.podToken(t, "pod-uid-1", row.change), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
			status, body := f.exchange(t, form, key)
			if status != http.StatusBadRequest || body["error"] != "invalid_request" {
				t.Fatalf("%s while the key set answers 500 = %d %v, want 400 invalid_request: its own claims refuse it, and a 5xx sends cipherclient to trade it again for the whole outage", row.name, status, body)
			}
			if hint, _ := body["error_description"].(string); !strings.Contains(hint, row.names) {
				t.Errorf("the refusal reads %q, which does not name %q, the claim that refused it", hint, row.names)
			}
		})
	}
}
