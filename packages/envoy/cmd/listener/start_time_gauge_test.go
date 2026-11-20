package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener serves its process's start on /metrics as envoy_listener_start_time_seconds, in Unix
// seconds, from the first scrape it answers. A reader of two scrapes of a counter, as
// packages/envoy/scripts/legacy-subjects-gate.sh compares envoy_listener_legacy_subjects_total 72
// hours apart, holds the two to one process only while this sample stays the same; for a listener
// outside a cluster nothing else shows a restart. The sample lies between the moment before the
// test started the process and the moment its first answer arrived, and a second process started
// after the first answered reports a start no earlier than that answer. The gate's own test holds
// the reading side: a counter a restart reset, read through this sample, is caught where the pod's
// restart count stayed the same (legacy-subjects-gate.test.sh).
func TestTheStartTimeGaugeIsTheProcessStart(t *testing.T) {
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")

	before := time.Now().Unix()
	first := startListenerProcess(t, binary, uri, "start-time-gauge-first")
	started := startTimeSample(t, first)
	answered := time.Now().Unix()
	if started < before || started > answered {
		t.Fatalf("envoy_listener_start_time_seconds = %d, want the process start, between %d (before the process was started) and %d (its first answer)", started, before, answered)
	}

	second := startListenerProcess(t, binary, uri, "start-time-gauge-second")
	if again := startTimeSample(t, second); again < answered {
		t.Fatalf("the second process reports envoy_listener_start_time_seconds = %d, before %d, when the first had already answered: the sample is not its own process's start", again, answered)
	}
}

// startTimeSample scrapes listener's /metrics until it answers, at most 30 seconds, and returns its
// one envoy_listener_start_time_seconds sample, failing the test when the scrape carries none, more
// than one, or one that is not a whole number.
func startTimeSample(t *testing.T, listener *listenerProcess) int64 {
	t.Helper()
	url := "http://127.0.0.1:" + strconv.Itoa(listener.port) + "/metrics"
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := http.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				return oneStartTimeSample(t, string(body))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not answer 200 within 30s (last error %v)\n%s", url, err, listener.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// oneStartTimeSample reads scrape's one envoy_listener_start_time_seconds sample, with its TYPE
// line naming it a gauge.
func oneStartTimeSample(t *testing.T, scrape string) int64 {
	t.Helper()
	const name = "envoy_listener_start_time_seconds"
	if !strings.Contains(scrape, "# TYPE "+name+" gauge\n") {
		t.Fatalf("/metrics carries no TYPE line naming %s a gauge:\n%s", name, scrape)
	}
	var samples []string
	for line := range strings.SplitSeq(scrape, "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			samples = append(samples, value)
		}
	}
	if len(samples) != 1 {
		t.Fatalf("/metrics carries %d samples of %s, want 1:\n%s", len(samples), name, scrape)
	}
	value, err := strconv.ParseInt(samples[0], 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, want a whole number of seconds: %v", name, samples[0], err)
	}
	return value
}
