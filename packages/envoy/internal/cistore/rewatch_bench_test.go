package cistore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rewatchBenchChecks is how many checks each benchmark record carries, which makes it about
// 2.4 KB, a production CI record's size.
const rewatchBenchChecks = 8

// benchWarmUp is one "cistore cache warm-up" line: the entries the scan delivered, each of which
// the store's applyWatched decoded, and how the scan ended.
type benchWarmUp struct {
	Entries int64  `json:"entries"`
	Outcome string `json:"outcome"`
}

// warmUpWriter is the io.Writer under the store's logger. slog's JSON handler writes one record per
// Write, so it decodes each warm-up record and hands it to warmUps, and drops every other line. The
// watcher logs that line once per generation, when the current watcher's scan reaches its nil
// marker and only for the generation still current (kvwatch.Watcher.consume), so the line after a
// Rewatch is the new generation's.
type warmUpWriter struct{ warmUps chan<- benchWarmUp }

func (w warmUpWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte(`"msg":"cistore cache warm-up"`)) {
		return len(p), nil
	}
	var line benchWarmUp
	if err := json.Unmarshal(p, &line); err != nil {
		return 0, err
	}
	w.warmUps <- line
	return len(p), nil
}

// awaitBenchWarmUp returns the next warm-up line, failing b when none comes before the shared wait
// gives up (testwait.Receive) or the scan timed out. A rewatch that resumes from the last revision
// it applied logs "resumed", a full read "completed"; either reached its nil marker.
func awaitBenchWarmUp(b *testing.B, warmUps <-chan benchWarmUp) benchWarmUp {
	b.Helper()
	line := testwait.Receive(b, "the store's next cistore cache warm-up line", warmUps)
	if line.Outcome != "completed" && line.Outcome != "resumed" {
		b.Fatalf("warm-up ended %q after %d entries, want completed or resumed", line.Outcome, line.Entries)
	}
	return line
}

// benchProcessCPU is the CPU time this process has used, user and system. The test NATS server
// runs in this process, so it counts the server's side of a replay as well as the store's.
func benchProcessCPU(b *testing.B) time.Duration {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatalf("getrusage: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// benchRecord is the i-th benchmark commit's record as this listener writes it (encodeRecord):
// a pull request's head with rewatchBenchChecks completed checks and one suite, stored under the
// key its identity builds, so the store's apply decodes and caches it.
func benchRecord(b *testing.B, i int) (string, []byte) {
	b.Helper()
	st := State{
		Owner:       "example-org",
		Repo:        "example-repo",
		Number:      fmt.Sprint(10000 + i),
		SHA:         fmt.Sprintf("%040x", i),
		Checks:      map[string]Check{},
		Suites:      map[string]Suite{},
		LastEventAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC).UnixMilli(),
		Generation:  1,
	}
	for c := range rewatchBenchChecks {
		runID := uint64(90000000000 + i*rewatchBenchChecks + c)
		st.Checks[fmt.Sprintf("ci / test-shard-%02d", c)] = Check{
			Name:       fmt.Sprintf("ci / test-shard-%02d", c),
			CheckRunID: checkRunID(runID),
			URL:        fmt.Sprintf("https://github.com/example-org/example-repo/actions/runs/%d/job/%d", 18000000000+i, runID),
			Status:     "completed",
			Conclusion: "success",
			ObservedAt: "2026-10-27T12:00:00Z",
		}
	}
	st.Suites["41000000000"] = Suite{ID: "41000000000", AppID: "15368", Status: "completed", Conclusion: "success", ObservedAt: "2026-10-27T12:00:00Z"}
	value, err := encodeRecord(&st)
	if err != nil {
		b.Fatalf("encode record %d: %v", i, err)
	}
	return Key(st.Owner, st.Repo, st.Number, st.SHA), value
}

// fillCIBucket stores keys records in the CI bucket with asynchronous publishes, so setup does not
// wait out one round trip per key, and fails b unless the bucket then holds them all. It waits for
// the publishes' acknowledgements through the shared wait, and reads the bucket's status through
// its jetstream handle, whose call takes the shared wait's deadline as its context.
func fillCIBucket(b *testing.B, conn *natsgo.Conn, kv bus.KeyValue, keys int) {
	b.Helper()
	if keys == 0 {
		return
	}
	js, err := conn.JetStream(natsgo.PublishAsyncMaxPending(1024))
	if err != nil {
		b.Fatalf("jetstream: %v", err)
	}
	for i := range keys {
		key, value := benchRecord(b, i)
		if _, err := js.PublishAsync("$KV."+kv.Bucket()+"."+key, value, stallWait(b)); err != nil {
			b.Fatalf("publish record %d: %v", i, err)
		}
	}
	acknowledged := js.PublishAsyncComplete()
	testwait.Until(b, fmt.Sprintf("the %d asynchronous publishes of the benchmark's records to be acknowledged (the reading is how many are pending)", keys), func() (int, bool) {
		select {
		case <-acknowledged:
			return 0, true
		default:
			return js.PublishAsyncPending(), false
		}
	})
	status, err := kv.WatchHandle().Status(testwait.Context(b))
	if err != nil {
		b.Fatalf("bucket status: %v", err)
	}
	if got := status.Values(); got != uint64(keys) {
		b.Fatalf("bucket holds %d values, want %d", got, keys)
	}
}

// stallWait is how long an asynchronous publish of fillCIBucket waits for room among its 1,024
// pending publishes: until the shared wait gives up (testwait.GiveUpAt), where nats.go gives up
// after 200 ms of its own (ErrTooManyStalledMsgs), which a loaded machine reaches. nats.go refuses
// a context for an asynchronous publish (ErrContextAndTimeout), so this span is the one form the
// shared wait's end reaches the call in. Under -timeout 0 there is no end, and the span is the
// longest a time.Duration holds.
func stallWait(b *testing.B) natsgo.PubOpt {
	end, bounded := testwait.GiveUpAt(b)
	if !bounded {
		return natsgo.StallWait(time.Duration(math.MaxInt64))
	}
	return natsgo.StallWait(max(time.Until(end), time.Nanosecond))
}

// BenchmarkRewatch times one Rewatch of the CI store on its own live connection, from the call
// until the new generation's scan has delivered every record through the store's own apply (which
// decodes each one) and reached its nil marker, over a bucket of 0, 1,000 and 25,000 records of
// about 2.4 KB. It reports the entries the new generation delivered per op (entries/op) and the
// process's CPU per op (cpu-ms/op), which includes the in-process NATS server's.
func BenchmarkRewatch(b *testing.B) {
	for _, keys := range []int{0, 1000, 25000} {
		b.Run(fmt.Sprintf("keys=%d", keys), func(b *testing.B) {
			benchmarkStoreRewatch(b, keys)
		})
	}
}

func benchmarkStoreRewatch(b *testing.B, keys int) {
	_, uri := testnats.Start(b)
	conn := testnats.Connect(b, uri)
	b.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		b.Fatalf("jetstream: %v", err)
	}
	// The bucket as Open makes it, so Open finds it already filled.
	kv, err := bus.EnsureKeyValue(b.Context(), conn, js, &natsgo.KeyValueConfig{
		Bucket:   bus.CIStateBucket,
		Replicas: 1,
		Storage:  natsgo.FileStorage,
		TTL:      time.Hour,
	})
	if err != nil {
		b.Fatalf("CI bucket: %v", err)
	}
	_, sample := benchRecord(b, 0)
	b.Logf("each record is %d bytes", len(sample))
	fillCIBucket(b, conn, kv, keys)

	warmUps := make(chan benchWarmUp, 4)
	s, err := Open(b.Context(), conn, logging.NewWithWriter("bench", warmUpWriter{warmUps: warmUps}), WithReplicas(1), WithTTL(time.Hour))
	if err != nil {
		b.Fatalf("open cistore: %v", err)
	}
	b.Cleanup(s.StopWatch)
	if err := s.WaitForCacheReady(testwait.Context(b)); err != nil {
		b.Fatalf("cache not ready: %v", err)
	}
	// The first generation logs its warm-up before it releases readiness.
	if line := awaitBenchWarmUp(b, warmUps); line.Entries != int64(keys) {
		b.Fatalf("first warm-up delivered %d entries, want %d", line.Entries, keys)
	}
	if got := len(s.List()); got != keys {
		b.Fatalf("store cached %d records, want %d: a benchmark record does not decode", got, keys)
	}

	b.ReportAllocs()
	var entries int64
	cpuBefore := benchProcessCPU(b)
	for b.Loop() {
		if err := s.Rewatch(context.Background(), conn); err != nil {
			b.Fatalf("Rewatch: %v", err)
		}
		// Readiness stays released after the first generation, so the new generation's own
		// warm-up line is what says its scan reached the nil marker. Nothing writes during the
		// loop, so a full read delivers every key and a resume none.
		line := awaitBenchWarmUp(b, warmUps)
		want := int64(keys)
		if line.Outcome == "resumed" {
			want = 0
		}
		if line.Entries != want {
			b.Fatalf("%s warm-up delivered %d entries, want %d", line.Outcome, line.Entries, want)
		}
		entries += line.Entries
	}
	cpu := benchProcessCPU(b) - cpuBefore
	b.ReportMetric(float64(entries)/float64(b.N), "entries/op")
	b.ReportMetric(float64(cpu)/float64(time.Millisecond)/float64(b.N), "cpu-ms/op")
}
