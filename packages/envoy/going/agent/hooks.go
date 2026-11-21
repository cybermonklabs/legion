package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

// The agent's test hooks (Config.Hooks), which an end-to-end test of the agent process drives
// through the fake chat model's replies: the server hands them on to its agent only under its own
// test hooks (cmd/dispatch: DISPATCH_TEST_AGENT_HOOKS), never in a deployment. Each stands in for
// a defect the agent must outlive, or for a load it must carry beside its server.
const (
	// hookHang is a tool that ignores its context and never returns: a stuck tool.
	hookHang = "test_hang"
	// hookPanic is a tool that panics on the model loop's own goroutine, which agentloop recovers.
	hookPanic = "test_panic"
	// hookCrash is a tool that panics on a goroutine of its own, which nothing recovers: a panic
	// past the run's recovery, which ends the agent process.
	hookCrash = "test_crash"
	// hookSoftLimit is a tool that works the agent at its soft limit until the run's context ends
	// (workAtSoftLimit): the load of check c29, which the agent's CPUs and priority bound.
	hookSoftLimit = "test_soft_limit"
	// hookHangText is prose that, streamed by the model, makes the run's OnText ignore its
	// context and never return: a model stream that ignores its context.
	hookHangText = "[[test_hang]]"
)

// hookTools are the test hooks' tools in the step whose model call is numbered base, each run
// through the worker's toolRun as every tool of a run is.
func (w *worker) hookTools(base int) []agentloop.Tool {
	empty := json.RawMessage(`{"type":"object","properties":{}}`)
	tool := func(name, description string, run func(ctx context.Context)) agentloop.Tool {
		return agentloop.Tool{Name: name, Description: description, Schema: empty,
			Run: w.toolRun(base, name, func(ctx context.Context, _ int, _ json.RawMessage) (string, bool) {
				run(ctx)
				return "", false
			})}
	}
	return []agentloop.Tool{
		tool(hookHang, "A test hook: never returns.", func(context.Context) { select {} }),
		tool(hookPanic, "A test hook: panics.", func(context.Context) { panic("the test hook's own panic") }),
		tool(hookCrash, "A test hook: panics past every recover.", func(context.Context) {
			go panic("the test hook's own panic, past every recover")
			select {}
		}),
		tool(hookSoftLimit, "A test hook: works the agent at its soft limit until the run ends.", workAtSoftLimit),
	}
}

// hookOnText is the run's OnText under the test hooks: it never returns once the reply's prose
// holds hookHangText.
func hookOnText(delta string) error {
	if strings.Contains(delta, hookHangText) {
		select {}
	}
	return nil
}

// softLimitNode is one object of the soft-limit hook's heap, 64 bytes: a pointer, which each
// collection follows, and the rest.
type softLimitNode struct {
	next *softLimitNode
	_    [56]byte
}

// softLimitGarbage is where the soft-limit hook's goroutines put each chain they allocate, which
// the next chain replaces, so each one reaches the heap and is garbage at once.
var softLimitGarbage atomic.Pointer[softLimitNode]

// The soft-limit hook's sizes: each chain it holds is a mebibyte of nodes, and it changes what it
// holds by 2% of the memory limit at a time, after a collection whose peak use since the one
// before passed softLimitHigh of the limit, or stayed under softLimitLow.
const (
	softLimitChainBytes = 1 << 20
	softLimitStep       = 50
	softLimitHigh       = 1.02
	softLimitLow        = 0.95
	// softLimitEvery is how often the hook reads the runtime's use against its limit.
	softLimitEvery = 5 * time.Millisecond
)

// workAtSoftLimit works the agent at its soft limit until ctx ends. One goroutine for each CPU the
// runtime schedules on (GOMAXPROCS) allocates chains of pointers without pause, each garbage at
// once, while the hook holds chains of the same nodes, which each collection marks, and sizes what
// it holds so the memory the runtime counts against its limit (GOMEMLIMIT: all it has mapped less
// what it gave back to the system) peaks at the limit. It reads that use every softLimitEvery, and
// after each collection lets go of 2% of the limit's worth of chains where the peak since the
// collection before passed softLimitHigh of the limit, or takes on as much where it stayed under
// softLimitLow. So the heap reaches the limit before each collection and the collector runs all
// the time; held at a fixed share instead, the collection's own garbage, marked while it runs,
// took the runtime past its limit toward the ceiling its server kills it at (136% of the limit
// with nine tenths held). With no limit set it holds nothing and waits for ctx. What it held is
// garbage once it returns.
func workAtSoftLimit(ctx context.Context) {
	limit := debug.SetMemoryLimit(-1)
	if limit == math.MaxInt64 {
		slog.Warn("agent: " + hookSoftLimit + ": the agent runs with no memory limit, so the hook holds nothing")
		<-ctx.Done()
		return
	}
	var held []*softLimitNode
	hold := func(chains int) {
		for range chains {
			var chain *softLimitNode
			for range softLimitChainBytes / 64 {
				chain = &softLimitNode{next: chain}
			}
			held = append(held, chain)
		}
	}
	step := max(1, int(limit/softLimitStep/softLimitChainBytes))
	hold(int(limit / 2 / softLimitChainBytes))
	procs := runtime.GOMAXPROCS(0)
	slog.Info("agent: "+hookSoftLimit+": the agent works at its soft limit", "limit", limit, "held", len(held)*softLimitChainBytes, "goroutines", procs)
	var allocating sync.WaitGroup
	for range procs {
		allocating.Go(func() {
			for ctx.Err() == nil {
				var chain *softLimitNode
				for range 1024 {
					chain = &softLimitNode{next: chain}
				}
				softLimitGarbage.Store(chain)
			}
		})
	}
	samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(samples)
	cycle, peak := samples[0].Value.Uint64(), uint64(0)
	ticker := time.NewTicker(softLimitEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
		metrics.Read(samples)
		peak = max(peak, samples[1].Value.Uint64()-samples[2].Value.Uint64())
		if samples[0].Value.Uint64() == cycle {
			continue
		}
		cycle = samples[0].Value.Uint64()
		switch {
		case float64(peak) > softLimitHigh*float64(limit) && len(held) > step:
			clear(held[len(held)-step:])
			held = held[:len(held)-step]
		case float64(peak) < softLimitLow*float64(limit):
			hold(step)
		}
		peak = 0
	}
	allocating.Wait()
	runtime.KeepAlive(held)
}
