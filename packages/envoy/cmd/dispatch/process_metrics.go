package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/metrics"
)

// runCountTimeout bounds the one statement a scrape of /metrics makes, the count of the runs in
// each state; a scrape it does not answer within it carries no runs' series.
const runCountTimeout = 2 * time.Second

// processFigures is what /metrics reads the figures of section 7 of the background-runs design
// from.
type processFigures struct {
	// started is when this server process started.
	started time.Time
	// readCacheBytes, listAnswerBytes and runs are the server's document read cache, its list-answer
	// cache and its count of the runs in each state.
	readCacheBytes  func() int64
	listAnswerBytes func() int64
	runs            func(context.Context) (api.GoingRunCounts, error)
	// agent is the supervisor of the agent process, nil where the server runs none.
	agent *agentSupervisor
}

// registerProcessMetrics adds to registry the figures of both processes that /metrics shows:
//
//   - envoy_dispatch_process_goroutines, _heap_live_bytes, _memory_total_bytes and
//     _memory_limit_bytes, each with a process label, "server" read from this process's runtime
//     and "agent" as the agent's latest renew carried it (no agent series before its first renew
//     or after its exit), and _start_time_seconds, when each process started, which the leak alert
//     needs to leave out a process's first day;
//   - envoy_dispatch_cache_bytes with a cache label: the document read cache ("read"), the
//     display-tree cache ("display_tree") and the list-answer cache ("list_answer");
//   - envoy_dispatch_going_runs with a state label: the runs working, queued and waiting, across
//     every server process of the database;
//   - envoy_dispatch_agent_slots, the runs the agent may work at once, _slots_in_use and
//     _stuck_goroutines as its latest renew carried them, and _memory_kills, how many times this
//     server killed its agent for memory since it started.
func registerProcessMetrics(registry *metrics.Registry, figures processFigures) {
	type processHealth struct {
		label  string
		health agentwire.Health
	}
	healths := func() []processHealth {
		all := []processHealth{{"server", agentwire.RuntimeHealth()}}
		if figures.agent != nil {
			if health, ok := figures.agent.end.Health(); ok {
				all = append(all, processHealth{"agent", health})
			}
		}
		return all
	}
	perProcess := func(name, help string, value func(agentwire.Health) uint64) {
		registry.NewGaugeSetFunc(name, help, func() []metrics.GaugeSample {
			all := healths()
			samples := make([]metrics.GaugeSample, 0, len(all))
			for _, process := range all {
				samples = append(samples, metrics.GaugeSample{Labels: [][2]string{{"process", process.label}}, Value: int64(value(process.health))})
			}
			return samples
		})
	}
	perProcess("envoy_dispatch_process_goroutines", "Goroutines of the server process and of its agent process",
		func(health agentwire.Health) uint64 { return uint64(health.Goroutines) })
	perProcess("envoy_dispatch_process_heap_live_bytes", "Live heap of the server process and of its agent process as of their last collection, in bytes",
		func(health agentwire.Health) uint64 { return health.HeapLiveBytes })
	perProcess("envoy_dispatch_process_memory_total_bytes", "All the memory the Go runtime of the server process and of its agent process has mapped, in bytes",
		func(health agentwire.Health) uint64 { return health.TotalBytes })
	perProcess("envoy_dispatch_process_memory_limit_bytes", "The Go memory limit of the server process and of its agent process, in bytes",
		func(health agentwire.Health) uint64 { return health.MemoryLimitBytes })
	registry.NewGaugeSetFunc("envoy_dispatch_process_start_time_seconds", "When the server process and its agent process started, in Unix seconds",
		func() []metrics.GaugeSample {
			samples := []metrics.GaugeSample{{Labels: [][2]string{{"process", "server"}}, Value: figures.started.Unix()}}
			if figures.agent != nil {
				if started, running := figures.agent.running(); running {
					samples = append(samples, metrics.GaugeSample{Labels: [][2]string{{"process", "agent"}}, Value: started.Unix()})
				}
			}
			return samples
		})
	registry.NewGaugeSetFunc("envoy_dispatch_cache_bytes", "Bytes the server's document read cache, display-tree cache and list-answer cache hold",
		func() []metrics.GaugeSample {
			return []metrics.GaugeSample{
				{Labels: [][2]string{{"cache", "read"}}, Value: figures.readCacheBytes()},
				{Labels: [][2]string{{"cache", "display_tree"}}, Value: pmdoc.DisplayCacheHeld()},
				{Labels: [][2]string{{"cache", "list_answer"}}, Value: figures.listAnswerBytes()},
			}
		})
	registry.NewGaugeSetFunc("envoy_dispatch_going_runs", "Command-bar runs working, queued for an agent and waiting for their person",
		func() []metrics.GaugeSample {
			ctx, cancel := context.WithTimeout(context.Background(), runCountTimeout)
			defer cancel()
			counts, err := figures.runs(ctx)
			if err != nil {
				slog.Warn("dispatch: count the runs in each state for /metrics", "error", err)
				return nil
			}
			return []metrics.GaugeSample{
				{Labels: [][2]string{{"state", "working"}}, Value: counts.Working},
				{Labels: [][2]string{{"state", "queued"}}, Value: counts.Queued},
				{Labels: [][2]string{{"state", "waiting"}}, Value: counts.Waiting},
			}
		})
	if figures.agent == nil {
		return
	}
	agent := figures.agent
	registry.NewGaugeFunc("envoy_dispatch_agent_slots", "Runs the agent process may work at once: its slots, halved for an hour after two kills for memory no run alone caused",
		func() int64 { return int64(agent.runSlots()) })
	registry.NewOptionalGaugeFunc("envoy_dispatch_agent_slots_in_use", "Runs the agent process works on now, as its latest renew carried it",
		func() (int64, bool) {
			health, ok := agent.end.Health()
			return int64(health.Working), ok
		})
	registry.NewOptionalGaugeFunc("envoy_dispatch_agent_stuck_goroutines", "Goroutines the agent process's watchdog counts stuck, as its latest renew carried it",
		func() (int64, bool) {
			health, ok := agent.end.Health()
			return int64(health.StuckGoroutines), ok
		})
	registry.NewGaugeFunc("envoy_dispatch_agent_memory_kills", "Times this server process killed its agent process for memory since it started",
		func() int64 { return agent.memoryKills.Load() })
}
