package oidc

// keys_order_test.go holds Keys to the order its reads end in, under testing/synctest, whose fake
// clock does not move while a read runs: a read that succeeds and the read after it that fails end
// at the same instant, so nothing that compares their times can tell which came first.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// scriptedKeySet is a key-set endpoint inside the synctest bubble: it answers at once, with the
// set it holds or a 503 while it fails, and counts the reads it gets.
type scriptedKeySet struct {
	mu      sync.Mutex
	body    string
	failing bool
	reads   int
}

func (s *scriptedKeySet) RoundTrip(*http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	status := http.StatusOK
	if s.failing {
		status = http.StatusServiceUnavailable
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func (s *scriptedKeySet) set(failing bool, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = failing
	if body != "" {
		s.body = body
	}
}

func (s *scriptedKeySet) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// keySetBody is a key set holding one ES256 key named kid.
func keySetBody(t *testing.T, kids ...string) string {
	t.Helper()
	set := jose.JSONWebKeySet{}
	for _, kid := range kids {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: string(jose.ES256), Use: "sig"})
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// scriptedKeys is a key source reading endpoint, with the package's defaults.
func scriptedKeys(endpoint *scriptedKeySet) *Keys {
	keys := NewKeysAt("https://keys.example/keys")
	keys.HTTPClient = &http.Client{Transport: endpoint}
	return keys
}

// A rotation read that fails at the very instant the last good read ended is still the later
// read: a token naming the key it was looking for is ErrKeySet, the issuer being unavailable,
// never ErrNoKey; no other read starts within FailureSpacing; and once FailureSpacing has passed,
// to the nanosecond, the key is looked for again and found.
func TestAFailedReadAtTheInstantOfTheLastGoodOneIsTheLater(t *testing.T) {
	first, rotated := keySetBody(t, "key-a"), keySetBody(t, "key-a", "key-b")
	synctest.Test(t, func(t *testing.T) {
		endpoint := &scriptedKeySet{body: first}
		keys := scriptedKeys(endpoint)
		if _, err := keys.Key(context.Background(), "key-a", string(jose.ES256)); err != nil {
			t.Fatalf("Key(the published key): %v", err)
		}
		endpoint.set(true, "")
		for call := range 3 {
			if _, err := keys.Key(context.Background(), "key-b", string(jose.ES256)); !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
				t.Fatalf("token %d naming key-b, its read failed at the instant the last good one ended: err = %v, want ErrKeySet and not ErrNoKey", call, err)
			}
			synctest.Wait()
		}
		if reads := endpoint.readCount(); reads != 2 {
			t.Fatalf("key set read %d times, want 2: the good read and the one key-b prompted", reads)
		}

		endpoint.set(false, rotated)
		time.Sleep(keys.FailureSpacing)
		if _, err := keys.Key(context.Background(), "key-b", string(jose.ES256)); err != nil {
			t.Fatalf("key-b at FailureSpacing after its failed read, now published: %v", err)
		}
		if reads := endpoint.readCount(); reads != 3 {
			t.Fatalf("key set read %d times, want 3: one more read at FailureSpacing", reads)
		}
	})
}

// With no Lifetime, every Key reads the key set first. A read that fails at the very instant the
// last good read ended still starts FailureSpacing: the callers after it are answered from the
// cached set with no read of their own.
func TestAFailedReadAtTheInstantOfTheLastGoodOneStartsTheSpacing(t *testing.T) {
	body := keySetBody(t, "key-a")
	synctest.Test(t, func(t *testing.T) {
		endpoint := &scriptedKeySet{body: body}
		keys := scriptedKeys(endpoint)
		keys.Lifetime, keys.MinRefresh = 0, 0
		if _, err := keys.Key(context.Background(), "key-a", string(jose.ES256)); err != nil {
			t.Fatalf("Key(the published key): %v", err)
		}
		endpoint.set(true, "")
		for call := range 4 {
			if _, err := keys.Key(context.Background(), "key-a", string(jose.ES256)); err != nil {
				t.Fatalf("token %d naming the cached key, the key set failing: %v, want the cached key", call, err)
			}
			synctest.Wait()
		}
		if reads := endpoint.readCount(); reads != 2 {
			t.Fatalf("key set read %d times, want 2: the good read and one failed read, then FailureSpacing", reads)
		}
	})
}
