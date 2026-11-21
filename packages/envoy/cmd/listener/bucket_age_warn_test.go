package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener whose session bucket keeps a record no longer than the gone horizon says so at its
// start. Such a bucket drops a stalled session's record before the session reads gone. One WARN
// line names the bucket's age and the horizon, and /healthz names both. A bucket an older build
// created at 5 minutes is this case, and a production listener starts on one until
// ENVOY_SESSION_MAX_AGE raises it. The control is a listener that creates its own bucket: that
// bucket keeps a record past the horizon, and the listener logs no such line.
func TestAListenerWarnsWhenTheBucketAgeIsShort(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")

	t.Run("a bucket made at 5 minutes", func(t *testing.T) {
		uri := testnats.URL(t)
		conn := testnats.Connect(t, uri)
		t.Cleanup(conn.Close)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("jetstream: %v", err)
		}
		if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: 5 * time.Minute, Replicas: 1, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("create the session bucket at 5 minutes: %v", err)
		}

		listener := startBucketAgeListener(t, binary, uri, "short-session-bucket")
		if warnings := sessionAgeWarnings(listener); len(warnings) != 1 {
			t.Errorf("the listener logged %d WARN lines about the session bucket's age, want 1 that names 5m0s and the 1h0m0s gone horizon:\n%s", len(warnings), listener.Output.String())
		} else {
			for _, value := range []string{`"5m0s"`, `"1h0m0s"`, "ENVOY_SESSION_MAX_AGE"} {
				if !strings.Contains(warnings[0], value) {
					t.Errorf("the WARN line does not name %s: %s", value, warnings[0])
				}
			}
		}
		health := bucketAgeHealth(t, listener)
		if got := health["session_max_age"]; got != "5m0s" {
			t.Errorf("/healthz session_max_age = %v, want \"5m0s\", the age of the bucket the listener found", got)
		}
		if got := health["session_gone_horizon"]; got != "1h0m0s" {
			t.Errorf("/healthz session_gone_horizon = %v, want \"1h0m0s\", the gone horizon the listener judges a record by", got)
		}
	})

	t.Run("a bucket the listener creates", func(t *testing.T) {
		listener := startBucketAgeListener(t, binary, testnats.URL(t), "new-session-bucket")
		if warnings := sessionAgeWarnings(listener); len(warnings) != 0 {
			t.Errorf("a listener that created its own session bucket logged %d WARN lines about the bucket's age, want none: %v", len(warnings), warnings)
		}
		health := bucketAgeHealth(t, listener)
		if got := health["session_max_age"]; got != "2h0m0s" {
			t.Errorf("/healthz session_max_age = %v, want \"2h0m0s\", an hour past the gone horizon", got)
		}
		if got := health["session_gone_horizon"]; got != "1h0m0s" {
			t.Errorf("/healthz session_gone_horizon = %v, want \"1h0m0s\"", got)
		}
	})
}

// startBucketAgeListener starts a listener as machineID on the NATS at uri and returns once it is
// healthy and has logged its ready line. That line comes after the start's session bucket check
// in the same goroutine, so the output read after it holds every line that check wrote.
func startBucketAgeListener(t *testing.T, binary, uri, machineID string) *listenerProcess {
	t.Helper()
	listener := startListenerProcess(t, binary, uri, machineID)
	listener.waitHealthy(t)
	listener.WaitForOutput(t, "envoy-listener ready (NATS connected)")
	return listener
}

// sessionAgeWarnings is every WARN line of the listener's output that is about the session
// bucket's age: a JSON record at WARN that names ENVOY_SESSION_MAX_AGE or session_max_age.
func sessionAgeWarnings(p *listenerProcess) []string {
	var warnings []string
	for line := range strings.SplitSeq(p.Output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["level"] != "WARN" {
			continue
		}
		if strings.Contains(line, "ENVOY_SESSION_MAX_AGE") || strings.Contains(line, "session_max_age") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

// bucketAgeHealth is the listener's healthy /healthz answer, decoded.
func bucketAgeHealth(t *testing.T, p *listenerProcess) map[string]any {
	t.Helper()
	status, body := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(p.port)+"/healthz"))
	var health map[string]any
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode /healthz %d %s: %v", status, body, err)
	}
	if status != http.StatusOK || health["status"] != "healthy" {
		t.Fatalf("/healthz = %d %s, want 200 healthy", status, body)
	}
	return health
}
