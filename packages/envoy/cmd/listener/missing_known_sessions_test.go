package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// countLogLines counts the lines of output, a listener's JSON log, at level whose msg is msg.
func countLogLines(output, level, msg string) int {
	count := 0
	for line := range strings.SplitSeq(output, "\n") {
		var entry struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == level && entry.Msg == msg {
			count++
		}
	}
	return count
}

// knownBucketNotOpenLine is the WARN the known-session store logs at its open for a bucket that
// does not open (session.OpenKnownSessions).
const knownBucketNotOpenLine = "the known-session bucket is not open; this listener answers no session's 30-day record and writes none until an attempt of its own opens the bucket"

// A listener serves without the known-session bucket, the one copied bucket it can: a start that
// finds it missing goes on, /healthz answers 200 "degraded" naming it, a registration is answered
// with its 30-day record not written, and the listener creates no bucket. It logs the bucket once
// at WARN (the store's line at its open), and through the self-health monitor's first tick neither
// missingBucketLine at ERROR, which pages, nor a failed tick. The control is the same start with
// the session bucket missing instead: it waits at its gates, 503 "waiting" naming that bucket,
// refuses the registration and logs missingBucketLine at ERROR, which the count here reads. At
// b20f45e30a the start with the known-session bucket missing waited the same way, 503 "waiting"
// naming it, every route answering 503.
func TestAListenerStartsAndServesWithTheKnownSessionBucketMissing(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "envoy-listener")
	for _, row := range []struct {
		name, bucket string
		serves       bool
	}{
		{"the known-session bucket missing", bus.KnownSessionBucket, true},
		{"control: the session bucket missing", bus.SessionBucket, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			uri := testnats.URL(t)
			conn := bustest.Connect(t, uri)
			if err := bustest.Provision(conn, 1); err != nil {
				t.Fatalf("provision the server: %v", err)
			}
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			if err := js.DeleteKeyValue(row.bucket); err != nil {
				t.Fatalf("delete the %s bucket: %v", row.bucket, err)
			}
			run := id.New()
			registration := consumerRegistrationIn(t, "ses_optional_"+run, "epoch-1", "/work/optional", "notifications.team.optional-"+run)

			// The server is provisioned already, so the harness provisions nothing and the bucket
			// stays missing for this start.
			started := time.Now()
			listener := startListenerProcess(t, binary, uri, "optional-bucket")
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("the listener's output:\n%s", listener.Output.String())
				}
			})

			if !row.serves {
				listener.WaitForOutput(t, missingBucketLine)
				if code, status, missing := healthzMissing(t, listener.port); code != http.StatusServiceUnavailable || status != "waiting" || !slices.Equal(missing, []string{row.bucket}) {
					t.Fatalf("control: /healthz of a start with %s missing = %d %q %v, want 503 waiting naming it", row.bucket, code, status, missing)
				}
				if code, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", registration); code != http.StatusServiceUnavailable {
					t.Fatalf("control: a registration while the start waits on %s answered %d %s, want 503", row.bucket, code, answer)
				}
				if n := countLogLines(listener.Output.String(), "ERROR", missingBucketLine); n == 0 {
					t.Fatalf("control: no ERROR %q line for %s, so the count below says nothing", missingBucketLine, row.bucket)
				}
				return
			}

			deadline := time.Now().Add(60 * time.Second)
			for {
				code, status, missing := healthzMissing(t, listener.port)
				if code == http.StatusOK && status == "degraded" {
					if !slices.Equal(missing, []string{bus.KnownSessionBucket}) {
						t.Fatalf("/healthz = 200 degraded naming %v, want [%s]", missing, bus.KnownSessionBucket)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("/healthz of a start with the known-session bucket missing = %d %q %v after 60 s, want 200 degraded naming %s: the start serves without that bucket", code, status, missing, bus.KnownSessionBucket)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if code, answer := callListener(t, listener.port, http.MethodPost, "/v1/interests/subscribe", registration); code != http.StatusOK {
				t.Fatalf("a registration with the known-session bucket missing answered %d %s, want 200 with no record written", code, answer)
			}
			requireBucketMissing(t, js, bus.KnownSessionBucket, "after the registration")

			// Past the self-health monitor's first tick, which reads the bucket missing again.
			time.Sleep(time.Until(started.Add(35 * time.Second)))
			if code, status, missing := healthzMissing(t, listener.port); code != http.StatusOK || status != "degraded" || !slices.Equal(missing, []string{bus.KnownSessionBucket}) {
				t.Fatalf("/healthz after the monitor's first tick = %d %q %v, want 200 degraded naming %s", code, status, missing, bus.KnownSessionBucket)
			}
			requireBucketMissing(t, js, bus.KnownSessionBucket, "after the monitor's first tick")
			output := listener.Output.String()
			for _, line := range []struct {
				level, msg string
				want       int
			}{
				{"WARN", knownBucketNotOpenLine, 1},
				{"WARN", optionalBucketMissingLine, 0},
				{"ERROR", missingBucketLine, 0},
				{"WARN", "self-health probe failed", 0},
			} {
				if n := countLogLines(output, line.level, line.msg); n != line.want {
					t.Errorf("%d %s lines %q, want %d", n, line.level, line.msg, line.want)
				}
			}
		})
	}
}
