package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// startingSentinelRig is a fresh-mode listener's main client over a server with the stream and a
// selection bucket whose sentinel sentinel leaves as it is (no bucket at all for a nil sentinel),
// and a plain connection.
func startingSentinelRig(t *testing.T, sentinel func(t *testing.T, conn *natsgo.Conn)) (*bus.Client, *natsgo.Conn) {
	t.Helper()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	if sentinel == nil {
		return client, conn
	}
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateKeyValue(context.Background(), consumernames.SessionBucketConfig(1)); err != nil {
		t.Fatal(err)
	}
	sentinel(t, conn)
	return client, conn
}

// writeSentinel writes the bucket's sentinel of family with the cutover done, over a key never
// written. It puts the sentinel under the key as bytes, as a principal with a write of the bucket
// can, since the store's own WriteSentinel writes the session family alone.
func writeSentinel(family string) func(t *testing.T, conn *natsgo.Conn) {
	return writeSentinelState(family, consumernames.CutoverDone)
}

// writeSentinelState is writeSentinel with the cutover marker cutover.
func writeSentinelState(family, cutover string) func(t *testing.T, conn *natsgo.Conn) {
	return func(t *testing.T, conn *natsgo.Conn) {
		t.Helper()
		data, err := json.Marshal(consumernames.StoreState{Key: consumernames.SentinelKey, Family: family, Cutover: cutover, Boundary: 1})
		if err != nil {
			t.Fatal(err)
		}
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.Publish("$KV."+consumernames.SessionBucket+"."+consumernames.SentinelKey, data, natsgo.ExpectLastSequencePerSubject(0)); err != nil {
			t.Fatal(err)
		}
	}
}

// A fresh listener that starts over a selection state that refuses every call binds no durable
// however long it retries, so its /healthz says so while it starts: 503 not_ready naming the
// bucket, the key envoy.bucket where there is one, and why: the bucket missing, the cutover not
// run or rolled back, or the sentinel absent, of another family or malformed (the independent
// tester's F2). The control is a sound sentinel, over which the start is only "starting". Before,
// a missing bucket and a cutover not done answered 200 "starting", and the process exited at the
// bind's deadline with "subscribe failed after max attempts", again at every start.
func TestAFreshStartWhoseSelectionRefusesSaysSoOnHealthz(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sentinel func(t *testing.T, conn *natsgo.Conn)
		reason   string
		key      string
	}{
		{name: "no bucket", reason: "missing"},
		{name: "before the cutover", sentinel: writeSentinelState(consumernames.SessionFamily, ""), reason: "cutover not done", key: consumernames.SentinelKey},
		{name: "rolled back", sentinel: writeSentinelState(consumernames.SessionFamily, consumernames.CutoverRolledBack), reason: "cutover not done", key: consumernames.SentinelKey},
		{name: "malformed", sentinel: storeStreamInfoOverTheSentinel, reason: "malformed", key: consumernames.SentinelKey},
		{name: "absent", sentinel: func(*testing.T, *natsgo.Conn) {}, reason: "absent", key: consumernames.SentinelKey},
		{name: "another family", sentinel: writeSentinel(consumernames.DispatchFamily), reason: "another family", key: consumernames.SentinelKey},
		{name: "control: a sound sentinel", sentinel: writeSentinel(consumernames.SessionFamily)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := startingSentinelRig(t, tc.sentinel)
			var deps atomic.Pointer[listenerDeps]
			var starting atomic.Pointer[bus.Client]
			starting.Store(client)
			recorder := httptest.NewRecorder()
			healthzHandler(&deps, &starting, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			var body map[string]any
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			if tc.reason == "" {
				if recorder.Code != http.StatusOK || body["status"] != "starting" {
					t.Fatalf("control: /healthz = %d %v; want 200 starting", recorder.Code, body)
				}
				return
			}
			key, _ := body["key"].(string)
			if recorder.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" || key != tc.key ||
				body["bucket"] != consumernames.SessionBucket || body["reason"] != tc.reason {
				t.Fatalf("/healthz = %d %v; want 503 not_ready naming %q of %s, %s", recorder.Code, body, tc.key, consumernames.SessionBucket, tc.reason)
			}
		})
	}
}

// The listener binary gives that answer on the /healthz route main registers (registerProbeRoutes,
// behind the start's own answer): its main client, once connected (startingClient), is the client
// the answer reads, so a fresh listener over a server with no selection bucket answers 503
// not_ready naming the missing bucket while its durable's bind retries. The control is the same
// start in the fixed mode, which reads no selection bucket and goes healthy.
func TestTheListenerBinaryAnswersAFreshStartsSelectionRefusalOnItsHealthzRoute(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, mode := range []consumernames.Mode{consumernames.ModeFixed, consumernames.ModeFresh} {
		t.Run(string(mode), func(t *testing.T) {
			listener := startListenerProcess(t, binary, testnats.URL(t), "starting-route-"+string(mode), consumernames.ModeEnvVar+"="+string(mode))
			if mode == consumernames.ModeFixed {
				listener.waitHealthy(t)
				return
			}
			client := &http.Client{Timeout: 5 * time.Second}
			url := "http://127.0.0.1:" + strconv.Itoa(listener.port) + "/healthz"
			var last string
			for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("/healthz never answered 503 not_ready naming the missing %s bucket within 60 s; its last answer: %s\n%s", consumernames.SessionBucket, last, listener.Output.String())
				}
				response, err := client.Get(url)
				if err != nil {
					last = err.Error()
					continue
				}
				var body map[string]any
				err = json.NewDecoder(response.Body).Decode(&body)
				_ = response.Body.Close()
				last = strconv.Itoa(response.StatusCode) + " " + fmt.Sprint(body)
				if err == nil && response.StatusCode == http.StatusServiceUnavailable && body["status"] == "not_ready" &&
					body["bucket"] == consumernames.SessionBucket && body["reason"] == "missing" {
					return
				}
			}
		})
	}
}

// syncBuffer is a log the bind writes from its goroutine and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(text string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), text)
}

// The start's bind logs the sentinel's refusal once per change, not once per attempt: one line,
// naming the reason, across every attempt over an absent sentinel, and one more once the operator
// writes the sentinel and the durable binds. The control is the bind's own retry line, which names
// no key.
func TestTheBindLogsTheSentinelsRefusalOncePerChange(t *testing.T) {
	client, conn := startingSentinelRig(t, func(*testing.T, *natsgo.Conn) {})
	var log syncBuffer
	logger := logging.NewWithWriter("test", &log)
	durable := bus.ListenerDurable("starting-sentinel-" + id.New())
	done := make(chan error, 1)
	go func() {
		_, err := bindListenerDurable(context.Background(), client, durable, func(msg *natsgo.Msg) { _ = msg.Ack() }, logger, 20*time.Millisecond, 30*time.Second)
		done <- err
	}()
	const waits, cleared = "the listener's durable waits on the selection bucket's sentinel", "no longer refuses the listener's durable"
	for deadline := time.Now().Add(10 * time.Second); log.count(waits) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the bind logged no sentinel refusal within 10 s: %s", log.buf.String())
		}
	}
	time.Sleep(300 * time.Millisecond) // about fifteen more attempts over the same absent sentinel
	if log.count("subscribe failed, retrying") == 0 {
		t.Fatal("control: the bind's own retry line is missing")
	}
	writeSentinel(consumernames.SessionFamily)(t, conn)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the bind once the sentinel was written: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the bind did not end within 20 s of the sentinel's write")
	}
	if got := log.count(waits); got != 1 || log.count(`"reason":"absent"`) != 1 || log.count(cleared) != 1 {
		t.Fatalf("the bind logged the refusal %d times and its end %d times; want each once:\n%s", got, log.count(cleared), log.buf.String())
	}
}
