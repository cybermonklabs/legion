//go:build unix

// packages/envoy/cmd/cipher/pod_broker_test.go
//
// The fake broker `enroll --pod` is driven against: it takes the token login, the enrollment and
// a renewal, and records each, so a test can say exactly what the pod sent and in which order.
// It verifies nothing — the real broker's own tests do that — and holds no key. A test can have
// it answer a route the broker's own 503 first (podBrokerOutages), and stamp each call with the
// clock the code under test waits on (podBrokerClock).
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api/unreadablekeysettest"
)

// podCall is one request the fake broker took: its path, the Proof header it carried, its JSON
// body as strings, and the moment it came on the broker's clock (podBrokerClock).
type podCall struct {
	path  string
	proof string
	body  map[string]string
	at    time.Time
}

// podBrokerOutage is a route the fake broker answers with answer, the broker's own 503 through a
// key set it could not read (unreadablekeysettest), to its first times calls, or to every call
// where times is 0, before it answers the route as it does otherwise.
type podBrokerOutage struct {
	path   string
	answer unreadablekeysettest.Answer
	times  int
}

// podBrokerOption changes how servePodBroker answers.
type podBrokerOption func(*podBrokerConfig)

type podBrokerConfig struct {
	now     func() time.Time
	outages map[string]podBrokerOutage
}

// podBrokerClock stamps each call the broker records (podCall.at) with now's moment, the clock the
// code under test waits on; the wall clock's otherwise.
func podBrokerClock(now func() time.Time) podBrokerOption {
	return func(c *podBrokerConfig) { c.now = now }
}

// podBrokerOutages has the broker answer each outage's route with its answer first.
func podBrokerOutages(outages ...podBrokerOutage) podBrokerOption {
	return func(c *podBrokerConfig) {
		for _, outage := range outages {
			c.outages[outage.path] = outage
		}
	}
}

// servePodBroker is a broker that takes the token login, the enrollment and a renewal, recording
// each. It answers a thumbprint it has already enrolled 200 with the same enrollment id and,
// like the real broker, without extending its lease, so a re-run has to renew.
func servePodBroker(t *testing.T, enrollmentID string, options ...podBrokerOption) (*httptest.Server, func() []podCall) {
	t.Helper()
	config := podBrokerConfig{now: time.Now, outages: map[string]podBrokerOutage{}}
	for _, option := range options {
		option(&config)
	}
	var mu sync.Mutex
	var calls []podCall
	enrolled := map[string]bool{}
	outageAnswers := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]string{}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, podCall{path: r.URL.Path, proof: r.Header.Get("Proof"), body: body, at: config.now()})
		outage, down := config.outages[r.URL.Path]
		down = down && (outage.times == 0 || outageAnswers[r.URL.Path] < outage.times)
		if down {
			outageAnswers[r.URL.Path]++
		}
		mu.Unlock()
		if down {
			outage.answer.Write(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential_id": "credential-for-the-pod",
				"expires_at":    time.Now().Add(time.Hour),
			})
		case "/v1/enrollments":
			mu.Lock()
			live := enrolled[body["thumbprint"]]
			enrolled[body["thumbprint"]] = true
			mu.Unlock()
			if live {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enrollment_id":    enrollmentID,
				"lease_expires_at": time.Now().Add(time.Hour),
				"slot":             nil,
			})
		case "/v1/enrollments/" + enrollmentID + "/renew":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []podCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]podCall(nil), calls...)
	}
}
