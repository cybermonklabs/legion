package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestProcAliveSeesAZombieAndAReapedProcessAsGone(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("this test's own process reads as gone")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Until Wait reaps it, the exited child stays in /proc as a zombie.
	testwait.Eventually(t, fmt.Sprintf("the exited, unreaped child %d to read as gone", pid), func() bool { return !procAlive(pid) })
	_ = cmd.Wait()
	if procAlive(pid) {
		t.Fatalf("reaped child %d reads as alive", pid)
	}
}

func TestSamplerRecordsAStackExitOnceAndFailsTheStep(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	gone := cmd.Process.Pid
	s := newSampler(endpoints{pids: map[string]int{"dispatch": gone, "postgres": os.Getpid()}})
	first := time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)
	s.checkAlive(first)
	s.checkAlive(first.Add(5 * time.Second))
	exits := s.stackExits()
	if len(exits) != 1 || exits[0].Name != "dispatch" || exits[0].PID != gone || !exits[0].FirstSeenGone.Equal(first) {
		t.Fatalf("stack exits %+v, want dispatch once, first seen gone at %s", exits, first)
	}
	err := errStackExits(exits)
	if err == nil {
		t.Fatal("a step in which dispatch exited does not fail")
	}
	if msg := err.Error(); !strings.Contains(msg, "dispatch") || !strings.Contains(msg, "pid "+strconv.Itoa(gone)) {
		t.Fatalf("error %q does not name dispatch and pid %d", msg, gone)
	}
	if errStackExits(nil) != nil {
		t.Fatal("a step with no stack exit fails")
	}
}

func TestSamplerChecksNothingWithoutPIDs(t *testing.T) {
	s := newSampler(endpoints{pids: map[string]int{}})
	s.checkAlive(time.Now())
	if exits := s.stackExits(); len(exits) != 0 {
		t.Fatalf("stack exits %+v under -allow-foreign, want none", exits)
	}
}

// fakeSample is an in-window sample at second at, with pgTotal connections ("idle|" state),
// goroutines and heap in MiB (0: not read), and the reads named in failed marked as failed. A heap
// read, one that worked or failed, ended at the sample's time.
func fakeSample(at, pgTotal, goroutines int, heapMiB float64, failed ...string) sample {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	s := sample{
		At: t0.Add(time.Duration(at) * time.Second), InWindow: true,
		CPUTicks: map[string]float64{"dispatch": float64(at) * 50}, RSSKiB: map[string]float64{"dispatch": 1024},
		PGConns: map[string]int{"idle|": pgTotal}, PGTotal: pgTotal, Goroutines: goroutines, PoolWait: goroutines / 10,
		HeapLiveBytes: heapMiB * 1048576, FailedReads: failed,
	}
	if heapMiB > 0 || slices.Contains(failed, readHeap) {
		s.HeapAt, s.HeapInWindow = s.At, true
	}
	return s
}

func TestSummarizeResourcesLeavesFailedReadsOutOfTheStats(t *testing.T) {
	// Every Postgres and goroutine read failed, and the one heap read.
	none := summarizeResources([]sample{
		fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap),
		fakeSample(5, 0, 0, 0, readPostgres, readGoroutines),
		fakeSample(10, 0, 0, 0, readPostgres, readGoroutines),
	}, 0, 10)
	if none.PGConnsTotal.N != 0 || len(none.PGConnsByState) != 0 || none.Goroutines.N != 0 || none.PoolAcquireGoroutines.N != 0 || none.HeapLiveMiB.N != 0 {
		t.Fatalf("failed reads entered the stats: %+v", none)
	}
	if none.PGReadErrors != 3 || none.PprofReadErrors != 4 {
		t.Fatalf("pg_read_errors %d, pprof_read_errors %d; want 3 and 4", none.PGReadErrors, none.PprofReadErrors)
	}

	// Some reads failed: their zeros would pull each p50 down to 0.
	some := summarizeResources([]sample{
		fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap),
		fakeSample(5, 0, 100, 0, readPostgres),
		fakeSample(10, 10, 120, 0),
		fakeSample(15, 12, 140, 30),
	}, 0, 15)
	if some.PGConnsTotal.N != 2 || some.PGConnsTotal.P50 != 10 || some.PGConnsByState["idle|"].N != 2 {
		t.Fatalf("Postgres connections %+v by state %+v: want the 2 reads that succeeded, p50 10", some.PGConnsTotal, some.PGConnsByState)
	}
	if some.Goroutines.N != 3 || some.Goroutines.P50 != 120 || some.PoolAcquireGoroutines.N != 3 {
		t.Fatalf("goroutines %+v, pool waiters %+v: want the 3 reads that succeeded, p50 120", some.Goroutines, some.PoolAcquireGoroutines)
	}
	if some.HeapLiveMiB.N != 1 || some.HeapLiveMiB.Max != 30 {
		t.Fatalf("heap %+v, want the one read that succeeded", some.HeapLiveMiB)
	}
	if some.PGReadErrors != 2 || some.PprofReadErrors != 2 {
		t.Fatalf("pg_read_errors %d, pprof_read_errors %d; want 2 and 2", some.PGReadErrors, some.PprofReadErrors)
	}
}

// TestSummarizeResourcesTakesHeapReadsByWhenTheyEnded: a heap read enters the window by when it
// ended, not by the sample that carries it: a read that ended before the window, carried by the
// window's first sample, is left out; one that ended in the window, carried by the first sample
// past it, counts.
func TestSummarizeResourcesTakesHeapReadsByWhenTheyEnded(t *testing.T) {
	early := fakeSample(0, 1, 1, 0)
	early.HeapLiveBytes, early.HeapAt, early.HeapInWindow = 50*1048576, early.At.Add(-time.Second), false
	late := fakeSample(15, 1, 1, 0)
	late.InWindow = false
	late.HeapLiveBytes, late.HeapAt, late.HeapInWindow = 70*1048576, late.At.Add(-2*time.Second), true
	failedLate := fakeSample(20, 1, 1, 0, readHeap)
	failedLate.InWindow = false
	res := summarizeResources([]sample{early, fakeSample(5, 1, 1, 0), fakeSample(10, 1, 1, 0), late, failedLate}, 0, 10)
	if res.HeapLiveMiB.N != 1 || res.HeapLiveMiB.Max != 70 || res.PprofReadErrors != 1 {
		t.Fatalf("heap %+v, pprof_read_errors %d; want the one read that worked and ended in the window (70 MiB), and the failed one counted",
			res.HeapLiveMiB, res.PprofReadErrors)
	}
}

func TestGoroutineCountsFailsWithoutAProfile(t *testing.T) {
	const profile = "goroutine profile: total 7\n" +
		"3 @ 0x1 0x2\n#\t0x1\tgithub.com/jackc/puddle/v2.(*Pool[...]).Acquire+0x1\tpool.go:1\n\n" +
		"4 @ 0x3\n#\t0x3\truntime.gopark+0x1\tproc.go:1\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ok/"):
			fmt.Fprint(w, profile)
		case strings.HasPrefix(r.URL.Path, "/empty/"):
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	if total, pool, err := goroutineCounts(srv.URL + "/ok"); err != nil || total != 7 || pool != 3 {
		t.Fatalf("goroutineCounts = %d, %d, %v; want 7 goroutines, 3 in the pool's Acquire", total, pool, err)
	}
	for _, base := range []string{srv.URL + "/down", srv.URL + "/empty", "http://127.0.0.1:1"} {
		if _, _, err := goroutineCounts(base); err == nil {
			t.Errorf("goroutineCounts(%s) read no profile and no error", base)
		}
		if _, _, err := heapLive(context.Background(), base); err == nil {
			t.Errorf("heapLive(%s) read no profile and no error", base)
		}
	}
}

// heapProfileBody is a gzipped binary heap profile, as /debug/pprof/heap serves it, of two stacks:
// in use, 3 MiB in 5 objects and 1 MiB in 2 objects, so 4 MiB in 7 objects; allocated, far more.
func heapProfileBody(t *testing.T) []byte {
	t.Helper()
	p := cpProfile(heapTypes,
		cpStack{frames: []string{"main.main", "main.a"}, values: []int64{50, 300 * mib1, 5, 3 * mib1}},
		cpStack{frames: []string{"main.main", "main.b"}, values: []int64{20, 100 * mib1, 2, 1 * mib1}})
	var b bytes.Buffer
	if err := p.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestHeapLiveSumsTheBinaryProfilesInuse: the heap read takes /debug/pprof/heap, the binary
// profile, never its ?debug=1 text, and records the sums of its inuse_space and inuse_objects; a
// profile with no inuse_space is a failed read, not 0.
func TestHeapLiveSumsTheBinaryProfilesInuse(t *testing.T) {
	body := heapProfileBody(t)
	noInuse := cpProfile(cpuTypes, cpuStack(10, "main.main"))
	var cpu bytes.Buffer
	if err := noInuse.Write(&cpu); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/ok/debug/pprof/heap":
			w.Write(body)
		case "/cpu/debug/pprof/heap":
			w.Write(cpu.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b, o, err := heapLive(context.Background(), srv.URL+"/ok")
	if err != nil || b != 4*mib1 || o != 7 {
		t.Fatalf("heapLive = %v bytes, %v objects, %v; want the profile's 4 MiB in 7 objects in use", b, o, err)
	}
	if _, _, err := heapLive(context.Background(), srv.URL+"/cpu"); err == nil || !strings.Contains(err.Error(), "inuse_space") {
		t.Errorf("a profile with no inuse_space read as %v, want an error naming inuse_space", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if q != "" {
			t.Errorf("the heap read asked with query %q, want the binary profile, no ?debug", q)
		}
	}
}

// samplerStart is where the sampler rows' fake clocks stand when they begin.
var samplerStart = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// samplerRun runs a sampler on a fake clock against a Dispatch that serves its goroutine profile
// and its binary heap profile, with the periodic heap read on or off. drive moves the fake and
// waits for what its row needs; then the step ends, and samplerRun returns the request URIs
// Dispatch was asked for and the samples the run took.
func samplerRun(t *testing.T, heapSamples bool, drive func(clk *clocktest.Fake, s *sampler)) ([]string, []sample) {
	t.Helper()
	heapBody := heapProfileBody(t)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.URL.Path == "/debug/pprof/heap" && r.URL.RawQuery == "":
			w.Write(heapBody)
		case strings.HasPrefix(r.URL.Path, "/debug/pprof/goroutine"):
			fmt.Fprint(w, "goroutine profile: total 7\n")
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	clk := clocktest.New(samplerStart)
	s := newSampler(endpoints{pprof: srv.URL, natsMon: "http://127.0.0.1:1", pg: unreachablePG, pids: map[string]int{}})
	s.heapSamples = heapSamples
	s.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.loop(ctx, samplerStart, samplerStart.Add(time.Minute), log.New(io.Discard, "", 0))
	}()
	drive(clk, s)
	cancel()
	testwait.Receive(t, "the sampler to stop once its step ends", stopped)

	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(asked), s.all()
}

// TestSamplerAsksDispatchForNoHeapProfile: Dispatch writes any heap profile itself, so a heap read
// in the window is charged to the CPU the harness measures for Dispatch - the text form this
// sampler once read was about half of it (46-51% over six runs) - and measures the harness's own
// sampling (README.md, "Why the harness reads no heap profile in the window"). Without
// -heap-samples the sampler must ask Dispatch for its goroutine profile and for no form of
// /debug/pprof/heap. Only the sampler is driven here: the binary profiles at the window's edges
// (heap-start.pprof and heap-end.pprof in runReplay, heap.pprof and heap.txt in step.profile)
// stay, and driving them needs a whole run.
func TestSamplerAsksDispatchForNoHeapProfile(t *testing.T) {
	asked, _ := samplerRun(t, false, func(_ *clocktest.Fake, s *sampler) {
		testwait.Eventually(t, "the sampler's first sample, taken as it starts", func() bool { return len(s.all()) >= 1 })
	})
	goroutines := 0
	for _, uri := range asked {
		if strings.HasPrefix(uri, "/debug/pprof/heap") {
			t.Errorf("the sampler asked Dispatch for %q; without -heap-samples its window must hold no heap read", uri)
		}
		if strings.HasPrefix(uri, "/debug/pprof/goroutine") {
			goroutines++
		}
	}
	if goroutines == 0 {
		t.Fatalf("the sampler read no goroutine profile (it asked for %v), so it proves nothing about the heap", asked)
	}
}

// TestSamplerUnderHeapSamplesReadsTheBinaryHeapProfile: with -heap-samples on, the sampler does
// read Dispatch's heap, and reads the binary profile alone, never ?debug=1, the text form no
// sampler reads now. The read lands in a sample, as heap_live_bytes and heap_live_objects.
func TestSamplerUnderHeapSamplesReadsTheBinaryHeapProfile(t *testing.T) {
	carriesTheRead := func(s sample) bool {
		return s.HeapLiveBytes == 4*mib1 && s.HeapLiveObjects == 7 && s.HeapInWindow
	}
	asked, samples := samplerRun(t, true, func(clk *clocktest.Fake, s *sampler) {
		// The first heap read runs beside the first sample. Once the sampler has armed its next
		// sample and readHeaps its next read, that read has ended and been handed on, so the
		// first sample or the one the move below takes carries it.
		clk.AwaitArmed(t, "the sampler's next sample and its next heap read", 2)
		clk.Advance(sampleEvery)
		testwait.Eventually(t, "a sample carrying the heap read", func() bool { return slices.ContainsFunc(s.all(), carriesTheRead) })
	})
	heaps := 0
	for _, uri := range asked {
		switch {
		case uri == "/debug/pprof/heap":
			heaps++
		case strings.HasPrefix(uri, "/debug/pprof/heap"):
			t.Errorf("the sampler asked Dispatch for %q, want the binary profile alone", uri)
		}
	}
	if heaps == 0 {
		t.Fatalf("the sampler read no heap profile under -heap-samples; it asked Dispatch for %v", asked)
	}
	if !slices.ContainsFunc(samples, carriesTheRead) {
		t.Fatalf("no sample carries the heap read (the profile's 4 MiB in 7 objects): %+v", samples)
	}
}

// natsSample is fakeSample at second at with the NATS /varz counts in and out, or, when ok is
// false, with its NATS read marked as failed.
func natsSample(at int, in, out float64, ok bool) sample {
	s := fakeSample(at, 1, 1, 0)
	s.NATS = map[string]float64{}
	if !ok {
		s.FailedReads = append(s.FailedReads, readNATS)
		return s
	}
	s.NATS["in_msgs"], s.NATS["out_msgs"], s.NATS["slow_consumers"] = in, out, 2
	return s
}

// TestSummarizeResourcesTakesNATSRatesFromTheReadsThatWorked: a failed /varz read is counted and
// enters no rate: the NATS rates come from the first and last reads that worked, and are null when
// fewer than two worked.
func TestSummarizeResourcesTakesNATSRatesFromTheReadsThatWorked(t *testing.T) {
	// The first and the last reads failed: the rates are over the reads at 5 s and 15 s.
	some := summarizeResources([]sample{
		natsSample(0, 0, 0, false), natsSample(5, 1000, 3000, true), natsSample(10, 1500, 4500, true),
		natsSample(15, 2000, 6000, true), natsSample(20, 0, 0, false),
	}, 0, 20)
	if some.NATSInMsgsPerS == nil || some.NATSOutMsgsPerS == nil || *some.NATSInMsgsPerS != 100 || *some.NATSOutMsgsPerS != 300 {
		t.Fatalf("NATS in %v, out %v per s; want 100 and 300, from the reads that worked", some.NATSInMsgsPerS, some.NATSOutMsgsPerS)
	}
	if some.NATSSlowConsumers == nil || *some.NATSSlowConsumers != 2 || some.NATSReadErrors != 2 {
		t.Fatalf("NATS slow consumers %v, nats_read_errors %d; want 2 and 2", some.NATSSlowConsumers, some.NATSReadErrors)
	}
	one := summarizeResources([]sample{natsSample(0, 0, 0, false), natsSample(5, 1000, 3000, true), natsSample(10, 0, 0, false)}, 0, 10)
	if one.NATSInMsgsPerS != nil || one.NATSOutMsgsPerS != nil || one.NATSSlowConsumers == nil || one.NATSReadErrors != 2 {
		t.Fatalf("one read worked: NATS in %v, out %v, slow consumers %v, nats_read_errors %d; want no rate, the read's slow consumers, 2 errors",
			one.NATSInMsgsPerS, one.NATSOutMsgsPerS, one.NATSSlowConsumers, one.NATSReadErrors)
	}
	none := summarizeResources([]sample{natsSample(0, 0, 0, false), natsSample(5, 0, 0, false)}, 0, 5)
	if none.NATSInMsgsPerS != nil || none.NATSOutMsgsPerS != nil || none.NATSSlowConsumers != nil || none.NATSReadErrors != 2 {
		t.Fatalf("no read worked: NATS in %v, out %v, slow consumers %v, nats_read_errors %d; want none and 2 errors",
			none.NATSInMsgsPerS, none.NATSOutMsgsPerS, none.NATSSlowConsumers, none.NATSReadErrors)
	}
}

// TestSamplerMarksNoReadThatFailedOnceTheStepEnded: a read that fails while the step runs is
// marked in its sample and logged; a sample taken once ctx has ended is not kept at all, and its
// failed reads are neither counted nor logged, as poll does. The heap read runs on its own (under
// -heap-samples, which this sampler is given), so its failure lands in the first or the second
// sample.
func TestSamplerMarksNoReadThatFailedOnceTheStepEnded(t *testing.T) {
	down := endpoints{pprof: "http://127.0.0.1:1", natsMon: "http://127.0.0.1:1", pg: unreachablePG, pids: map[string]int{}}
	now := samplerStart

	var logged bytes.Buffer
	clk := clocktest.New(samplerStart)
	running := newSampler(down)
	running.heapSamples = true
	running.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		running.loop(ctx, now, now.Add(time.Minute), log.New(&logged, "", 0))
	}()
	// Once the sampler has armed its next sample and readHeaps its next read, the first heap
	// read has failed and been handed on, so the first sample or the second carries it.
	clk.AwaitArmed(t, "the sampler's next sample and its next heap read", 2)
	clk.Advance(sampleEvery)
	testwait.Eventually(t, "the sampler's second sample", func() bool { return len(running.all()) >= 2 })
	cancel()
	testwait.Receive(t, "the sampler to stop once its step ends", stopped)
	ss := running.all()
	if len(ss) != 2 {
		t.Fatalf("%d samples once the step ended after the second, want 2", len(ss))
	}
	heaps := 0
	for _, s := range ss {
		others := slices.DeleteFunc(slices.Clone(s.FailedReads), func(r string) bool { return r == readHeap })
		heaps += len(s.FailedReads) - len(others)
		if !slices.Equal(others, []string{readPostgres, readGoroutines, readNATS}) {
			t.Errorf("sample at %s marks %v, want postgres, goroutines and nats", s.At, s.FailedReads)
		}
	}
	if heaps != 1 {
		t.Errorf("samples mark %v and %v, want the one heap read marked once", ss[0].FailedReads, ss[1].FailedReads)
	}
	for _, name := range []string{"sampler: postgres", "sampler: pprof", "sampler: nats"} {
		if !strings.Contains(logged.String(), name) {
			t.Errorf("run.log does not log the failed %s read:\n%s", name, logged.String())
		}
	}

	logged.Reset()
	ended := newSampler(down)
	endedCtx, end := context.WithCancel(context.Background())
	end()
	ended.loop(endedCtx, now, now.Add(time.Minute), log.New(&logged, "", 0))
	if ss := ended.all(); len(ss) != 0 || logged.Len() != 0 {
		t.Fatalf("reads that failed once ctx had ended: samples %+v, log %q; want no sample and no log line", ss, logged.String())
	}
}

// TestSamplerHeapReadDelaysNoSample: under -heap-samples, a heap read that Dispatch holds past
// the sample interval delays no sample: the sampler's clock is a fake the test moves, and the
// stand-in holds the heap read until the test lets it go, at 7 s, while samples come at 0 and 5 s.
// The read lands in the first sample after it ended (10 s), with its own end time, and no second
// heap read starts before the step ends, which the test ends once that sample is taken.
func TestSamplerHeapReadDelaysNoSample(t *testing.T) {
	heapBody := heapProfileBody(t)
	release := make(chan struct{})
	var heapAsked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/debug/pprof/goroutine":
			fmt.Fprint(w, "goroutine profile: total 7\n")
		case "/debug/pprof/heap":
			heapAsked.Add(1)
			if r.URL.RawQuery != "" {
				http.Error(w, "the sampler reads the binary profile only", http.StatusBadRequest)
				return
			}
			select {
			case <-release:
				w.Write(heapBody)
			case <-r.Context().Done():
			}
		case "/varz":
			fmt.Fprint(w, `{"in_msgs": 1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// A failure before the release below still lets the held read go, before the server closes.
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()

	clk := clocktest.New(samplerStart)
	s := newSampler(endpoints{pprof: srv.URL, natsMon: srv.URL, pg: unreachablePG, pids: map[string]int{}})
	s.heapSamples = true
	s.clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		s.loop(ctx, samplerStart, samplerStart.Add(time.Minute), log.New(io.Discard, "", 0))
	}()
	testwait.Eventually(t, "sample 0, and the heap read the stand-in holds", func() bool {
		return len(s.all()) == 1 && heapAsked.Load() == 1
	})
	clk.AwaitArmed(t, "the sampler's next sample", 1)
	clk.Advance(sampleEvery)
	// Taken while the stand-in still holds the heap read: a sampler that waited on the read
	// would take no sample here.
	testwait.Eventually(t, "sample 1, taken while the heap read is held", func() bool { return len(s.all()) == 2 })
	clk.Advance(2 * time.Second)
	letGo()
	// Once readHeaps has armed its next read, the held one has ended (at 7 s) and been handed on.
	clk.AwaitArmed(t, "the sampler's next sample and readHeaps' next read", 2)
	clk.Advance(3 * time.Second)
	testwait.Eventually(t, "sample 2", func() bool { return len(s.all()) == 3 })
	cancel()
	testwait.Receive(t, "the sampler to stop once its step ends", ended)

	ss := s.all()
	if len(ss) != 3 {
		t.Fatalf("%d samples, want 3: at 0, 5 and 10 s", len(ss))
	}
	for i, x := range ss {
		if want := samplerStart.Add(time.Duration(i) * sampleEvery); !x.At.Equal(want) {
			t.Errorf("sample %d at %s, want %s: one each sampleEvery with the heap read in flight", i, x.At, want)
		}
		if x.Goroutines != 7 || slices.Contains(x.FailedReads, readGoroutines) || slices.Contains(x.FailedReads, readHeap) {
			t.Errorf("sample %d: goroutines %d, failed reads %v; want 7 goroutines, no failed goroutine or heap read", i, x.Goroutines, x.FailedReads)
		}
	}
	for i, x := range ss[:2] {
		if !x.HeapAt.IsZero() || x.HeapLiveBytes != 0 {
			t.Errorf("sample %d carries a heap read (ended %s, %v bytes) before the read could end", i, x.HeapAt, x.HeapLiveBytes)
		}
	}
	got := ss[2]
	if got.HeapLiveBytes != 4*mib1 || got.HeapLiveObjects != 7 || !got.HeapInWindow {
		t.Errorf("sample 2 heap live %v bytes, %v objects, in window %v; want the profile's 4 MiB in 7 objects, in the window",
			got.HeapLiveBytes, got.HeapLiveObjects, got.HeapInWindow)
	}
	if want := samplerStart.Add(7 * time.Second); !got.HeapAt.Equal(want) {
		t.Errorf("sample 2 carries a heap read that ended at %s, want its own end, %s", got.HeapAt, want)
	}
	if n := heapAsked.Load(); n != 1 {
		t.Errorf("heap asked %d times, want once: the next heap slot, 15 s, is past the step's end", n)
	}
}

// TestXIDRateReturnsAFailedRead: given a Postgres it cannot reach, the transaction-id probe
// returns no rate and the error, which run.log holds.
func TestXIDRateReturnsAFailedRead(t *testing.T) {
	var logged bytes.Buffer
	now := time.Now()
	rate, err := xidRate(testwait.Context(t), unreachablePG, now, now, log.New(&logged, "", 0))
	if rate != nil || err == nil {
		t.Fatalf("xidRate on an unreachable Postgres = %v, %v; want no rate and an error", rate, err)
	}
	if !strings.Contains(logged.String(), err.Error()) {
		t.Fatalf("run.log does not hold the error %q:\n%s", err, logged.String())
	}
}
