package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of what a key source prints of its key set's address: the jwks_uri an issuer's
// discovery document names, the issuer's own words, prints in every error Key returns only as the
// rule for remote text reads it. The helpers they share with the configured issuer's rows are in
// configured_issuer_test.go.

// jwksProbe is a key-set address at base, the URL a test server answers at, that carries the
// probe's user information, and its access token in a query and in a fragment.
func jwksProbe(base string) string {
	return credentialed(base) + "/keys?" + probeParam + "=" + redacttest.Token + "#" + probeParam + "=" + redacttest.Token
}

// keySetAnswer starts a key-set endpoint that answers every read 200 with body.
func keySetAnswer(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// droppingServer closes every connection a request arrives on without answering, so the client's
// Do fails with a transport error and no answer.
func droppingServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// TestTheKeySetsAddressPrintsOnlyAsTheRuleReadsIt: a key source whose key-set address carries a
// credential names the address, in each error Key returns, only as the rule for remote text reads
// it:
//   - a token for a key id the cached set lacks, while the read that would settle it failed
//     (miss);
//   - a caller that stops waiting on the read (waiting);
//   - an address that does not parse, whose parse error prints only its class (unparsed);
//   - a read with no answer (unanswered);
//   - a refused read (refused);
//   - an answer too long to read (too-large);
//   - and an answer that is no key set (not-a-set).
//
// net/http's own text in a read with no answer is not this row's: it masks a password, so that
// row's address carries a password alone. Captures oidc.key-set-url/<row>.
func TestTheKeySetsAddressPrintsOnlyAsTheRuleReadsIt(t *testing.T) {
	ctx := context.Background()
	t.Run("miss", func(t *testing.T) {
		issuer := oidctest.New(t)
		issuer.PublishKey(t, "key-a")
		keys := NewKeysAt(jwksProbe(issuer.URL()))
		if _, err := keys.Key(ctx, "key-a", testAlgorithm); err != nil {
			t.Fatalf("Key(the published key): %v", err)
		}
		issuer.FailKeys(http.StatusInternalServerError)
		if _, err := keys.Key(ctx, "key-b", testAlgorithm); !errors.Is(err, ErrKeySet) {
			t.Fatalf("the first token naming key-b, whose read fails: err = %v, want ErrKeySet", err)
		}
		_, err := keys.Key(ctx, "key-b", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/miss", err, "is not in the cached set, and the last read of")
	})
	t.Run("waiting", func(t *testing.T) {
		gone, cancel := context.WithCancel(ctx)
		cancel()
		_, err := NewKeysAt(jwksProbe(silentServer(t))).Key(gone, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/waiting", err, "waiting on")
	})
	t.Run("unparsed", func(t *testing.T) {
		address := "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/keys%zz?" + probeParam + "=" + redacttest.Token
		_, err := NewKeysAt(address).Key(ctx, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/unparsed", err, "requesting")
		if !strings.Contains(err.Error(), redact.ClassNotAURL) {
			t.Errorf("Key's error does not name the class %q: %q", redact.ClassNotAURL, err)
		}
	})
	t.Run("unanswered", func(t *testing.T) {
		address := strings.Replace(droppingServer(t), "://", "://keys-reader:"+redacttest.Password+"@", 1) + "/keys"
		_, err := NewKeysAt(address).Key(ctx, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/unanswered", err, "EOF")
	})
	t.Run("refused", func(t *testing.T) {
		_, err := NewKeysAt(jwksProbe(refusingServer(t))).Key(ctx, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/refused", err, "answered 500 Internal Server Error")
	})
	t.Run("too-large", func(t *testing.T) {
		_, err := NewKeysAt(jwksProbe(keySetAnswer(t, strings.Repeat("a", maxKeySetBytes+1)))).Key(ctx, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/too-large", err, "longer than its reader's limit")
	})
	t.Run("not-a-set", func(t *testing.T) {
		_, err := NewKeysAt(jwksProbe(keySetAnswer(t, "not a key set"))).Key(ctx, "key-a", testAlgorithm)
		requireKeySetRefusal(t, "oidc.key-set-url/not-a-set", err, "parsing")
	})
}

// requireKeySetRefusal fails t unless err is ErrKeySet saying says, and captures its text and its
// %+v under capture, each of which must hold none of the probe's credentials.
func requireKeySetRefusal(t *testing.T, capture string, err error, says string) {
	t.Helper()
	if !errors.Is(err, ErrKeySet) {
		t.Fatalf("Key = %v, want %v", err, ErrKeySet)
	}
	if !strings.Contains(err.Error(), says) {
		t.Fatalf("Key = %v, want an error that says %q", err, says)
	}
	redacttest.RequireNone(t, capture+"/error", err.Error())
	redacttest.RequireNone(t, capture+"/plus-v", fmt.Sprintf("%+v", err))
}
