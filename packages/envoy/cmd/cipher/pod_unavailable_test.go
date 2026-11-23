//go:build unix

// packages/envoy/cmd/cipher/pod_unavailable_test.go
//
// `enroll --pod` against a broker that answers 503: it makes the same call again after the
// answer's Retry-After, or its backoff where the answer names none, and gives up once its waits
// for one call would pass podUnavailableBudget. podRetryWait is replaced, so the waits are read
// rather than slept.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/serverwait"
)

// recordPodWaits replaces podRetryWait for the test with a timer that fires at once and records
// each wait it was asked for, and answers what it recorded.
func recordPodWaits(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	previous := podRetryWait
	podRetryWait = func(wait time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, wait)
		mu.Unlock()
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	t.Cleanup(func() { podRetryWait = previous })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(waits)
	}
}

// unavailableFirst is a broker that answers the first unavailable[path] calls to each path 503
// OIDC_ISSUER_UNAVAILABLE, with Retry-After retryAfter[path] where it names one, and then answers
// as servePodBroker's broker does. It records every call.
func unavailableFirst(t *testing.T, unavailable map[string]int, retryAfter map[string]string) (*httptest.Server, func() []podCall) {
	t.Helper()
	ok, _ := servePodBroker(t, "enrollment-of-the-pod")
	var mu sync.Mutex
	var calls []podCall
	answered := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]string{}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, podCall{path: r.URL.Path, proof: r.Header.Get("Proof"), body: body})
		answered[r.URL.Path]++
		down := answered[r.URL.Path] <= unavailable[r.URL.Path]
		mu.Unlock()
		if down {
			if wait := retryAfter[r.URL.Path]; wait != "" {
				w.Header().Set("Retry-After", wait)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "OIDC_ISSUER_UNAVAILABLE", "error": "the service-account token issuer cannot be reached; try again after Retry-After",
			})
			return
		}
		// The call the fake broker takes is passed on to servePodBroker's, with the headers the
		// pod sent, so its answer is the one TestEnrolAPodFromItsProjectedToken reads.
		forward, err := http.NewRequest(r.Method, ok.URL+r.URL.Path, bytes.NewReader(raw))
		if err != nil {
			t.Error(err)
			return
		}
		forward.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(forward)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []podCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

// TestAPodEnrollsThroughABrokerThatAnswers503: the token login answered 503 with Retry-After: 1,
// and the enrollment answered 503 naming no wait, are each made again, after one second and after
// renew's first backoff of two seconds, and the pod enrolls. Each wait says on stderr what could
// not be done and how long it waits, and the login's request object is signed again for the
// second login, since the broker refuses a jti it has seen.
func TestAPodEnrollsThroughABrokerThatAnswers503(t *testing.T) {
	waits := recordPodWaits(t)
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker, calls := unavailableFirst(t,
		map[string]int{"/v1/launcher-credentials/token": 1, "/v1/enrollments": 1},
		map[string]string{"/v1/launcher-credentials/token": "1"})
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod through a broker that answered 503 twice: exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-pod" {
		t.Fatalf("stdout = %q, want the enrollment id alone", stdout.String())
	}
	if got, want := waits(), []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v: the login's Retry-After, then renew's first backoff for an answer naming none", got, want)
	}
	for _, want := range []string{
		"could not log in: HTTP 503 OIDC_ISSUER_UNAVAILABLE", "trying again in 1s",
		"could not enrol: HTTP 503 OIDC_ISSUER_UNAVAILABLE", "trying again in 2s",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not say %q", stderr.String(), want)
		}
	}
	got := calls()
	paths := make([]string, len(got))
	for i, call := range got {
		paths[i] = call.path
	}
	if want := []string{"/v1/launcher-credentials/token", "/v1/launcher-credentials/token", "/v1/enrollments", "/v1/enrollments"}; !slices.Equal(paths, want) {
		t.Fatalf("broker calls = %v, want %v", paths, want)
	}
	first, err := record.VerifyRequestObject(got[0].body["request"], broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("the first login's request object: %v", err)
	}
	second, err := record.VerifyRequestObject(got[1].body["request"], broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("the second login's request object: %v", err)
	}
	if first.JTI == second.JTI {
		t.Fatalf("both logins sent the request object with jti %q, want a fresh one for the second", first.JTI)
	}
}

// TestAPodGivesUpOnABrokerThatStaysUnavailable: a broker that answers every token login 503 with
// Retry-After: 30 is asked five times, four waits of 30 seconds filling podUnavailableBudget's two
// minutes, and then the pod refuses as it refuses any answer: exit 1, naming the 503.
func TestAPodGivesUpOnABrokerThatStaysUnavailable(t *testing.T) {
	waits := recordPodWaits(t)
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := unavailableFirst(t,
		map[string]int{"/v1/launcher-credentials/token": 1000},
		map[string]string{"/v1/launcher-credentials/token": "30"})
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	if got, want := waits(), slices.Repeat([]time.Duration{30 * time.Second}, 4); !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v: four waits fill two minutes", got, want)
	}
	if got := len(calls()); got != 5 {
		t.Fatalf("token logins = %d, want 5: the first and one after each wait", got)
	}
	if !strings.Contains(stderr.String(), "could not log in: HTTP 503 OIDC_ISSUER_UNAVAILABLE") {
		t.Fatalf("stderr %q does not name the 503 it gave up on", stderr.String())
	}
}

// TestARefusedPodLoginIsNotMadeAgain: the control of the two tests above. A 401 is the broker's
// verdict on the token, which the same call gets again: the login is made once, nothing waits,
// and the pod exits 1.
func TestARefusedPodLoginIsNotMadeAgain(t *testing.T) {
	waits := recordPodWaits(t)
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	var logins int
	var mu sync.Mutex
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		logins++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "TOKEN_INVALID", "error": "the ServiceAccount token does not verify"})
	}))
	t.Cleanup(broker.Close)
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != 1 || len(waits()) != 0 {
		t.Fatalf("token logins = %d and waits = %v, want one login and no wait", logins, waits())
	}
}

// TestARefusalsRetryAfterIsReadAsServerwaitReadsIt: the client reads a refusal's Retry-After by
// the module's one rule (serverwait.Read), not a rule of its own: whole seconds as themselves, a
// count past serverwait.MaxWait as MaxWait, an HTTP date as the time until it, and a header that
// names no wait as none.
func TestARefusalsRetryAfterIsReadAsServerwaitReadsIt(t *testing.T) {
	inTwentyMinutes := time.Now().Add(20 * time.Minute).UTC().Format(http.TimeFormat)
	for _, row := range []struct {
		name, header string
		least, most  time.Duration
	}{
		{"whole seconds", "1", time.Second, time.Second},
		{"a count past serverwait's bound", "86400", serverwait.MaxWait, serverwait.MaxWait},
		{"an HTTP date", inTwentyMinutes, 19 * time.Minute, 20 * time.Minute},
		{"no number and no date", "soon", 0, 0},
		{"no header", "", 0, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if row.header != "" {
					w.Header().Set("Retry-After", row.header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "OIDC_ISSUER_UNAVAILABLE", "error": "the service-account token issuer cannot be reached; try again after Retry-After"})
			}))
			t.Cleanup(broker.Close)
			_, err := newClient(cipheraddr.New(broker.URL)).do(context.Background(), http.MethodGet, broker.URL+"/v1/self", nil, nil)
			var refused *apiError
			if !errors.As(err, &refused) {
				t.Fatalf("the 503 read as %v, want an *apiError", err)
			}
			if refused.RetryAfter < row.least || refused.RetryAfter > row.most {
				t.Fatalf("Retry-After %q read as %s, want between %s and %s, as serverwait.Read reads it", row.header, refused.RetryAfter, row.least, row.most)
			}
		})
	}
}
