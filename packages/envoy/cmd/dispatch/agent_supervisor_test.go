//go:build multiprocess

package main

// agent_supervisor_test.go holds the server's supervision of its agent process as a deployment
// meets it: a real envoy-dispatch server on a real database, which starts its own envoy-dispatch
// agent, kills it and starts it again, and shows both processes on /metrics. A server here names no
// chat model unless a row gives it the fake one (agent_loop_test.go), so its agent ends each run it
// claims failed with agent.ReasonNoModel. Its rows run two processes and wait out the bounds of
// a background run, so they carry the multiprocess tag and run in CI's dispatch-multiprocess job;
// the agent's limits and environment, which take seconds, are agent_environment_test.go's.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/testtmp"
)

// recordingAgentEnd is a server's end of agent/v1 that answers no call: the supervisor's agent,
// given a socket nobody listens on, fails its hello and exits at once. It counts the exits the
// supervisor reports.
type recordingAgentEnd struct {
	path    string
	mu      sync.Mutex
	closeIn func()
	exits   int
}

func (e *recordingAgentEnd) Path() string                     { return e.path }
func (e *recordingAgentEnd) Secret() []byte                   { return make([]byte, agentwire.SecretSize) }
func (e *recordingAgentEnd) LastRenewal() time.Time           { return time.Time{} }
func (e *recordingAgentEnd) Health() (agentwire.Health, bool) { return agentwire.Health{}, false }
func (e *recordingAgentEnd) SetRunSlots(int)                  {}

func (e *recordingAgentEnd) SetCloseStandardInput(closeIn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeIn = closeIn
}

func (e *recordingAgentEnd) AgentExited(context.Context, bool) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exits++
	return false, nil
}

func (e *recordingAgentEnd) Stop() {
	e.mu.Lock()
	closeIn := e.closeIn
	e.mu.Unlock()
	if closeIn != nil {
		closeIn()
	}
}

func (e *recordingAgentEnd) Close(time.Time) { e.Stop() }

// requireTheRestartWaits runs the supervisor over the real agent binary, which exits at once each
// time, and holds the wait before each start again: 1 s after the first exit, then twice as long
// after each quick exit, 30 s at the most. The waits are recorded, not slept.
func requireTheRestartWaits(t *testing.T) {
	t.Helper()
	end := &recordingAgentEnd{path: testtmp.Socket(t, "agent.sock")}
	supervisor := newAgentSupervisor(end, []string{cmdtest.Build(t, "envoy-dispatch"), "agent"},
		[]string{"DISPATCH_AGENT_SOCKET=" + end.path}, agentLimits(0))
	waits := make(chan time.Duration, 16)
	supervisor.after = func(wait time.Duration) <-chan time.Time {
		waits <- wait
		now := make(chan time.Time, 1)
		now <- time.Now()
		return now
	}
	go supervisor.run()
	t.Cleanup(func() { supervisor.Close(time.Now()) })
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	var got []time.Duration
	for range want {
		select {
		case wait := <-waits:
			got = append(got, wait)
		case <-time.After(30 * time.Second):
			t.Fatalf("the supervisor started the agent again after %v, then no more within 30 s", got)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the waits before each start again: %v, want %v", got, want)
	}
	end.mu.Lock()
	exits := end.exits
	end.mu.Unlock()
	if exits < len(want) {
		t.Errorf("the supervisor reported %d exits of the agent to the server's end, want at least %d", exits, len(want))
	}
}

// TestAStuckOrPanickingRunStopsOnlyTheAgent: a panic or a stuck run stops only the agent process
// (check c19). An agent killed for memory is started again while the server serves on; a SIGSTOP
// holds the agent's renewals, and the server kills it once it has missed 3 of them and starts
// another within 40 s; the close of the agent's standard input at the server's stop ends the agent
// within 1 s; each quick exit doubles the wait before the next start, up to 30 s; a tool and a
// model stream that ignore their context are given up by the watchdog, and at 3 the agent exits
// and starts again; a panic in a callback of the model loop is recovered; a run that panics past
// every recover fails at its third death, and a run beside it is not counted; and a route that
// panics during a call of the agent answers an error while the server serves on.
func TestAStuckOrPanickingRunStopsOnlyTheAgent(t *testing.T) {
	t.Run("an agent killed for memory is started again and the server serves on", func(t *testing.T) {
		server := startAgentServer(t, map[string]string{"DISPATCH_TEST_AGENT_CEILING": "1"})
		first := server.agentStarted(t, 1, 30*time.Second)
		server.waitForLog(t, 10*time.Second, `msg="dispatch: agent killed for memory" pid=`+strconv.Itoa(first)+" ")
		second := server.agentStarted(t, 2, 10*time.Second)
		if second == first {
			t.Fatalf("the agent started again under the pid of the one killed, %d", first)
		}
		server.requireServing(t)
	})

	t.Run("a stopped agent is killed once it has missed 3 renewals, and another starts within 40 s", func(t *testing.T) {
		server := startAgentServer(t, nil)
		stoppedAgent := server.agentStarted(t, 1, 30*time.Second)
		server.waitForAgentRenewal(t)
		stopped := time.Now()
		signalProcess(t, stoppedAgent, syscall.SIGSTOP)
		next := server.agentStarted(t, 2, 40*time.Second)
		started := time.Since(stopped)
		if started > 40*time.Second {
			t.Fatalf("the next agent started %v after the SIGSTOP, want within 40 s", started)
		}
		// The stopped agent's last renew came at most 10 s before the stop, so it missed its third
		// renewal 20 s after it at the earliest.
		if started < 19*time.Second {
			t.Errorf("the next agent started %v after the SIGSTOP, before the stopped one could miss 3 renewals", started)
		}
		if !server.logged(`msg="dispatch: agent killed: it renewed nothing for 30s" pid=` + strconv.Itoa(stoppedAgent) + " ") {
			t.Errorf("the server did not log the stopped agent %d killed for its renewals:\n%s", stoppedAgent, server.Output.String())
		}
		if !processGone(stoppedAgent) {
			t.Errorf("the stopped agent %d still runs beside the next one, %d", stoppedAgent, next)
		}
		server.requireServing(t)
		server.waitForAgentRenewal(t)
	})

	t.Run("the agent exits within 1 s of the server closing its standard input", func(t *testing.T) {
		server := startAgentServer(t, nil)
		pid := server.agentStarted(t, 1, 30*time.Second)
		server.waitForAgentRenewal(t)
		server.Terminate(t)
		var gone time.Time
		for deadline := time.Now().Add(30 * time.Second); gone.IsZero(); time.Sleep(5 * time.Millisecond) {
			if processGone(pid) {
				gone = time.Now()
			} else if time.Now().After(deadline) {
				t.Fatalf("the agent %d still ran 30 s after the server's SIGTERM:\n%s", pid, server.Output.String())
			}
		}
		closed := `msg="dispatch: closed the agent's standard input" pid=` + strconv.Itoa(pid)
		server.waitForLog(t, 10*time.Second, closed)
		if exited := gone.Sub(logTime(t, server.logLines(closed)[0])); exited > time.Second {
			t.Errorf("the agent exited %v after its standard input closed, want within 1 s", exited)
		}
		server.WaitExit(t, "SIGTERM")
	})

	t.Run("each quick exit doubles the wait before the agent starts again, up to 30 s", requireTheRestartWaits)

	t.Run("a tool and a model stream that ignore their context: the watchdog gives up the lease, and after 3 the agent exits and starts again",
		requireTheWatchdogEndsAStuckAgent)
	t.Run("a panic in a model-loop callback: the run goes on and the agent's process id does not change", requireAPanicInACallbackIsRecovered)
	t.Run("one run that panics past its recovery 3 times fails with that reason, and a run beside it is not counted",
		requireARunThatPanicsPastItsRecoveryFails)
	t.Run("a route that panics during a call of the agent answers an error, and the server's process id does not change",
		requireAPanickingRouteAnswersAnError)
}

// TestMetricsShowBothProcesses: /metrics shows the goroutines, live heap, total memory and memory
// limit of the server and of its agent, the bytes of the server's three caches, the runs working,
// queued and waiting, the agent's slots, the slots in use and the stuck goroutines, the agent's
// kills for memory and when each process started; each kill writes the line agent killed for
// memory; and container limits of 24 GiB, 8 GiB and none give the limits of section 5 (check c23).
func TestMetricsShowBothProcesses(t *testing.T) {
	t.Run("the figures of both processes, the caches and the runs", func(t *testing.T) {
		before := time.Now()
		server := startAgentServer(t, nil)
		// Three runs work under another server's agent, so the two queued behind them for the same
		// person stay queued: a person works 3 runs at once.
		for range 3 {
			server.recordRun(t, "alice", "working", readShape)
		}
		for range 2 {
			server.recordRun(t, "alice", "queued", readShape)
		}
		server.recordRun(t, "alice", "waiting", readShape)
		server.waitForAgentRenewal(t)
		figures := server.scrape(t)

		for _, process := range []string{"server", "agent"} {
			for _, name := range []string{"goroutines", "heap_live_bytes", "memory_total_bytes", "memory_limit_bytes"} {
				sample := `envoy_dispatch_process_` + name + `{process="` + process + `"}`
				if value, shown := figures[sample]; !shown || value <= 0 {
					t.Errorf("%s = %d (shown %t), want a figure above 0", sample, value, shown)
				}
			}
		}
		container, err := containerMemoryLimit()
		if err != nil {
			t.Fatal(err)
		}
		limits := agentLimits(container)
		wantServerLimit := limits.ServerLimit
		if wantServerLimit == 0 {
			wantServerLimit = math.MaxInt64
		}
		if got := figures[`envoy_dispatch_process_memory_limit_bytes{process="server"}`]; got != wantServerLimit {
			t.Errorf("the server's memory limit is %d, want %d for a container limit of %d", got, wantServerLimit, container)
		}
		if got := figures[`envoy_dispatch_process_memory_limit_bytes{process="agent"}`]; got != limits.AgentLimit {
			t.Errorf("the agent's memory limit is %d, want its GOMEMLIMIT %d for a container limit of %d", got, limits.AgentLimit, container)
		}
		serverStart, agentStart := figures[`envoy_dispatch_process_start_time_seconds{process="server"}`], figures[`envoy_dispatch_process_start_time_seconds{process="agent"}`]
		if serverStart < before.Unix()-1 || serverStart > time.Now().Unix() {
			t.Errorf("the server started at %d, want between %d and now", serverStart, before.Unix())
		}
		if agentStart < serverStart || agentStart > time.Now().Unix() {
			t.Errorf("the agent started at %d, want between the server's start %d and now", agentStart, serverStart)
		}
		for _, cache := range []string{"read", "display_tree", "list_answer"} {
			if value, shown := figures[`envoy_dispatch_cache_bytes{cache="`+cache+`"}`]; !shown || value < 0 {
				t.Errorf("the %s cache's bytes = %d (shown %t)", cache, value, shown)
			}
		}
		for state, want := range map[string]int64{"working": 3, "queued": 2, "waiting": 1} {
			if got, shown := figures[`envoy_dispatch_going_runs{state="`+state+`"}`]; !shown || got != want {
				t.Errorf("runs %s = %d (shown %t), want %d", state, got, shown, want)
			}
		}
		for sample, want := range map[string]int64{
			agentSlotsSample:                        int64(limits.Slots),
			`envoy_dispatch_agent_slots_in_use`:     0,
			`envoy_dispatch_agent_stuck_goroutines`: 0,
			agentMemoryKills:                        0,
		} {
			if got, shown := figures[sample]; !shown || got != want {
				t.Errorf("%s = %d (shown %t), want %d", sample, got, shown, want)
			}
		}
	})

	t.Run("each kill for memory is counted and logs agent killed for memory with the memory", func(t *testing.T) {
		server := startAgentServer(t, map[string]string{"DISPATCH_TEST_AGENT_CEILING": "1"})
		server.eventually(t, 30*time.Second, "two kills for memory", func() bool { return server.scrape(t)[agentMemoryKills] >= 2 })
		// The log and the count are read between two scrapes that agree, so no kill came between
		// them: the kills are 1 s, then 2 s, 4 s and 8 s apart.
		var kills int64
		var lines []string
		server.eventually(t, 30*time.Second, "a log read between two kills", func() bool {
			time.Sleep(300 * time.Millisecond)
			kills = server.scrape(t)[agentMemoryKills]
			lines = server.logLines(`msg="dispatch: agent killed for memory"`)
			return server.scrape(t)[agentMemoryKills] == kills
		})
		if int64(len(lines)) != kills {
			t.Errorf("/metrics counts %d kills for memory and the log %d:\n%s", kills, len(lines), strings.Join(lines, "\n"))
		}
		for _, line := range lines {
			if memory := killedMemory(t, line); memory <= 1 {
				t.Errorf("a kill above the ceiling of 1 byte names the memory %d: %s", memory, line)
			}
		}
	})

	t.Run("container limits of 24 GiB, 8 GiB and none give the agent's ceiling, its GOMEMLIMIT and the server's limit", requireTheContainerLimits)

	t.Run("the container's limit as automemlimit reads it, no limit and no control group reading as none", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			limit uint64
			err   error
			want  int64
		}{
			{"a limit", 24 << 30, nil, 24 << 30},
			{"no limit", 0, memlimit.ErrNoLimit, 0},
			{"no control group", 0, memlimit.ErrNoCgroup, 0},
			{"no control groups on the system", 0, memlimit.ErrCgroupsNotSupported, 0},
		} {
			got, err := readContainerMemoryLimit(func() (uint64, error) { return c.limit, c.err })
			if err != nil || got != c.want {
				t.Errorf("%s: %d, %v; want %d", c.name, got, err, c.want)
			}
		}
		if _, err := readContainerMemoryLimit(func() (uint64, error) { return 0, os.ErrPermission }); err == nil {
			t.Error("a control group the process may not read reads as no limit, not as a failure")
		}
	})
}

// sectionFiveRows is every limit of section 5 of the background-runs design (v11), both its tables,
// by the words of its Limit column: TestEachBoundOfABackgroundRun holds a subtest for each.
var sectionFiveRows = []string{
	"Runs working at once, in the agent",
	"Runs working at once, and queued, for one person",
	"Model calls, time, output tokens",
	"One model call",
	"One read or write, to the model",
	"One run's history to the model",
	"One step's time",
	"Memory of the agent",
	"A run that kills the agent",
	"Kills for memory that no run alone causes",
	"Readers of a run's events",
	"calls that the API runs for its agents at once",
	"One call in the API",
	"A second try of one idempotency key",
	"A lease",
	"One claim",
	"A restart of the agent",
	"The short answer and Jev's call, in the API",
}

// TestEachBoundOfABackgroundRun: each limit of section 5 of the background-runs design is crossed
// once, and the run waits or stops with the reason the section names (check c22). It fails on a
// row of the section with no subtest.
func TestEachBoundOfABackgroundRun(t *testing.T) {
	subtests := map[string]func(*testing.T){
		"Runs working at once, in the agent":               requireTheAgentsSlots,
		"Runs working at once, and queued, for one person": requireOnePersonsRuns,
		"Model calls, time, output tokens":                 requireTheLevelsBudgets,
		"One model call":                                   requireOneModelCallsBounds,
		"One read or write, to the model":                  requireOneReadToTheModel,
		"One run's history to the model":                   requireTheHistoryBound,
		"One step's time":                                  requireOneStepsTime,
		"Memory of the agent":                              requireTheAgentsMemoryBound,
		"A run that kills the agent":                       requireARunThatKillsTheAgentThreeTimesFails,
		"Kills for memory that no run alone causes":        requireUnexplainedKillsHalveTheSlots,
		"Readers of a run's events":                        requireTheRunsReaders,
		"calls that the API runs for its agents at once":   requireTheServersCallSlots,
		"One call in the API":                              requireOneCallsTime,
		"A second try of one idempotency key":              requireASecondTryWaitsForTheFirst,
		"A lease":                                          requireAStoppedOwnerLosesItsRun,
		"One claim":                                        requireOneClaim,
		"A restart of the agent":                           requireTheRestartBound,
		"The short answer and Jev's call, in the API":      requireTheShortAnswersBounds,
	}
	for _, row := range sectionFiveRows {
		test, ok := subtests[row]
		if !ok {
			t.Errorf("section 5's row %q has no subtest here", row)
			continue
		}
		t.Run(row, test)
		delete(subtests, row)
	}
	for row := range subtests {
		t.Errorf("a subtest for %q, which section 5 does not list", row)
	}
}

// requireTheRunsReaders: a server serves at most 4 streams of one run's events and 20 of one
// person's runs; the next of either is refused 429 GOING_RUN_READERS_FULL with its reason. The
// open streams are woken by the run's NOTIFY: the person's Stop reaches each stream of the run as
// its end frame within a second, and ends it.
func requireTheRunsReaders(t *testing.T) {
	server := startAgentServer(t, nil)
	// Queued at a shape no build reads, so no agent takes one while it is read.
	runs := make([]string, 6)
	for index := range runs {
		runs[index] = server.recordRun(t, "alice", "queued", unreadShape)
	}
	// open opens a stream of run's events and answers its lines, closed when the stream ends, or
	// the refusal's status and body.
	open := func(t *testing.T, run string) (int, string, <-chan string) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.url("/api/v1/going/runs/"+run+"/events"), nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request.Header.Set("X-Dispatch-User", "alice")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatalf("open a stream of %s: %v", run, err)
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			cancel()
			return response.StatusCode, string(body), nil
		}
		lines := make(chan string, 16)
		go func() {
			defer close(lines)
			defer response.Body.Close()
			scanner := bufio.NewScanner(response.Body)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()
		t.Cleanup(cancel)
		return http.StatusOK, "", lines
	}
	var first []<-chan string
	for reader := 1; reader <= 4; reader++ {
		status, body, opened := open(t, runs[0])
		if status != http.StatusOK {
			t.Fatalf("reader %d of the run answered %d %s, want 200", reader, status, body)
		}
		first = append(first, opened)
	}
	if status, body, _ := open(t, runs[0]); status != http.StatusTooManyRequests ||
		!strings.Contains(body, "GOING_RUN_READERS_FULL") || !strings.Contains(body, "4 streams open") {
		t.Errorf("the fifth reader of the run answered %d %s, want 429 GOING_RUN_READERS_FULL naming the run's 4 streams", status, body)
	}
	for _, run := range runs[1:5] {
		for reader := 1; reader <= 4; reader++ {
			if status, body, _ := open(t, run); status != http.StatusOK {
				t.Fatalf("reader %d of run %s answered %d %s, want 200", reader, run, status, body)
			}
		}
	}
	if status, body, _ := open(t, runs[5]); status != http.StatusTooManyRequests ||
		!strings.Contains(body, "GOING_RUN_READERS_FULL") || !strings.Contains(body, "20 streams of your runs") {
		t.Errorf("the 21st reader of alice's runs answered %d %s, want 429 GOING_RUN_READERS_FULL naming her 20 streams", status, body)
	}
	stopped := time.Now()
	if answer := server.send(t, http.MethodPost, "/api/v1/going/runs/"+runs[0]+"/stop", "", asUser("alice")); answer.status != http.StatusOK {
		t.Fatalf("stop the run answered %s", answer)
	}
	for index, lines := range first {
		// Bounded, so a stream that never ends fails the row rather than hanging it.
		deadline := time.After(10 * time.Second)
		ended := false
	read:
		for {
			select {
			case line, more := <-lines:
				if !more {
					break read
				}
				if line == "event: end" {
					if waited := time.Since(stopped); waited > time.Second {
						t.Errorf("reader %d was written the end frame %s after the stop, want within 1 s", index+1, waited)
					}
					ended = true
				}
			case <-deadline:
				t.Errorf("reader %d's stream did not end within 10 s of the stop", index+1)
				break read
			}
		}
		if !ended {
			t.Errorf("reader %d's stream ended with no end frame", index+1)
		}
	}
}

// requireTheHistoryBound: a run whose last 8 model turns, each with its tool results, pass 2 MiB
// fails with that reason when the agent claims it, and one whose last 8 turns fit is given to the
// agent. Each turn is a model step and the two tool results it called for, and the results hold
// the bytes, so the bound is crossed by the turns: the last 8 steps alone hold under 1 MiB.
func requireTheHistoryBound(t *testing.T) {
	server := startAgentServer(t, nil)
	server.waitForAgentRenewal(t)
	record := func(resultBytes int) string {
		t.Helper()
		ctx := context.Background()
		tx, err := server.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		id := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at)
			values ($1::uuid, 'alice', 'the words of a test run', $1::uuid, 'deep', 'ask', 'queued', $2, clock_timestamp())
		`, id, readShape); err != nil {
			t.Fatal(err)
		}
		result := `{"result": "` + strings.Repeat("x", resultBytes) + `"}`
		for turn := range 8 {
			first := 1 + 3*turn
			for at, kind := range []string{"model", "tool", "tool"} {
				body := `{"text": "a reply"}`
				if kind == "tool" {
					body = result
				}
				if _, err := tx.Exec(ctx, `insert into going_run_steps (run_id, step, kind, body, note) values ($1::uuid, $2, $3, $4::jsonb, 'a step')`,
					id, first+at, kind, body); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := tx.Exec(ctx, `select pg_notify($1, 'ready')`, api.GoingRunsChannel); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	tooLarge, fits := record(150<<10), record(100<<10)
	server.eventually(t, 30*time.Second, "both runs to end", func() bool {
		return server.run(t, tooLarge).state == "failed" && server.run(t, fits).state == "failed"
	})
	if row := server.run(t, tooLarge); !strings.HasPrefix(row.reason, "HISTORY_TOO_LARGE") {
		t.Errorf("the run whose last 8 turns hold 2.4 MiB stopped with %q, want HISTORY_TOO_LARGE", row.reason)
	}
	if row := server.run(t, fits); row.reason != agent.ReasonNoModel {
		t.Errorf("the run whose last 8 turns hold 1.6 MiB stopped with %q, want the agent's own end of it, %q", row.reason, agent.ReasonNoModel)
	}
}

// requireTheAgentsMemoryBound: the agent runs with the GOMEMLIMIT and the CPUs its limits give
// it, and the server kills it within its first reads, every 50 ms, once it is above its ceiling.
func requireTheAgentsMemoryBound(t *testing.T) {
	t.Run("the agent runs with GOMEMLIMIT 75% of its ceiling and at most 4 CPUs", func(t *testing.T) {
		server := startAgentServer(t, nil)
		pid := server.agentStarted(t, 1, 30*time.Second)
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			t.Fatalf("read the agent's environment: %v", err)
		}
		variables := map[string]string{}
		for entry := range strings.SplitSeq(strings.TrimRight(string(environ), "\x00"), "\x00") {
			name, value, _ := strings.Cut(entry, "=")
			variables[name] = value
		}
		container, err := containerMemoryLimit()
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.FormatInt(agentLimits(container).AgentLimit, 10); variables["GOMEMLIMIT"] != want {
			t.Errorf("the agent's GOMEMLIMIT is %q, want %s for a container limit of %d", variables["GOMEMLIMIT"], want, container)
		}
		if procs, err := strconv.Atoi(variables["GOMAXPROCS"]); err != nil || procs < 1 || procs > agentMaxProcs {
			t.Errorf("the agent's GOMAXPROCS is %q, want 1 to %d", variables["GOMAXPROCS"], agentMaxProcs)
		}
	})
	t.Run("the server kills the agent above its ceiling at its first reads, 50 ms apart", func(t *testing.T) {
		server := startAgentServer(t, map[string]string{"DISPATCH_TEST_AGENT_CEILING": "1"})
		pid := strconv.Itoa(server.agentStarted(t, 1, 30*time.Second))
		killed := `msg="dispatch: agent killed for memory" pid=` + pid + " "
		server.waitForLog(t, 10*time.Second, killed)
		line := server.logLines(killed)[0]
		if memory := killedMemory(t, line); memory <= 1 {
			t.Errorf("the kill names the memory %d, not above the ceiling of 1 byte: %s", memory, line)
		}
		started := logTime(t, server.logLines(`msg="dispatch: started the agent process" pid=` + pid + " ")[0])
		if after := logTime(t, line).Sub(started); after > 500*time.Millisecond {
			t.Errorf("the agent was killed %v after it started, above its ceiling from the start, want within its first reads", after)
		}
		server.requireServing(t)
	})
	t.Run("no new claim above 70% of the agent's GOMEMLIMIT", requireTheClaimGate)
}

// requireARunThatKillsTheAgentThreeTimesFails: a run the agent held alone each time it died is
// charged each death, and fails with that reason at the third.
func requireARunThatKillsTheAgentThreeTimesFails(t *testing.T) {
	t.Run("a run the agent held alone at each of 3 deaths fails with that reason", func(t *testing.T) {
		server := startAgentServer(t, nil)
		run := server.recordRun(t, "alice", "queued", unreadShape)
		for death := 1; death <= 3; death++ {
			pid := server.agentStarted(t, death, 30*time.Second)
			server.waitForAgentRenewal(t)
			server.stopHolding(t, pid, run)
			// Held back again, so the next agent cannot take it before the test hands it over.
			server.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, unreadShape)
			signalProcess(t, pid, syscall.SIGKILL)
			server.eventually(t, 15*time.Second, fmt.Sprintf("death %d charged to the run", death), func() bool {
				return server.run(t, run).kills == death
			})
		}
		row := server.run(t, run)
		if row.state != "failed" || !strings.HasPrefix(row.reason, "AGENT_KILLED") {
			t.Errorf("the run the agent died on 3 times is %s with %q, want failed with AGENT_KILLED", row.state, row.reason)
		}
		server.requireServing(t)
	})
	t.Run("a run beside it is not counted", requireARunThatPanicsPastItsRecoveryFails)
}

// requireUnexplainedKillsHalveTheSlots: two kills for memory within an hour that no run alone
// caused halve the agent's slots, and each later one halves them again, to 1 at the least. The
// agent is killed at each start, the kills 1 s, 2 s, 4 s and 8 s apart; the slots are read once
// each count of kills has stood 300 ms, since they follow the kill once the server has read who
// held what.
func requireUnexplainedKillsHalveTheSlots(t *testing.T) {
	server := startAgentServer(t, map[string]string{"DISPATCH_TEST_AGENT_CEILING": "1"})
	want := map[int64]int64{1: 8, 2: 4, 3: 2, 4: 1, 5: 1}
	read := map[int64]int64{}
	var kills int64
	var since time.Time
	server.eventually(t, 60*time.Second, "5 kills for memory", func() bool {
		figures := server.scrape(t)
		if figures[agentMemoryKills] != kills {
			kills, since = figures[agentMemoryKills], time.Now()
			return false
		}
		if _, done := read[kills]; !done && kills > 0 && time.Since(since) >= 300*time.Millisecond {
			read[kills] = figures[agentSlotsSample]
		}
		_, fifth := read[5]
		return fifth
	})
	// The first kill can come before the server answers its first scrape; each later count stands
	// 2 s at the least.
	for count := int64(2); count <= 5; count++ {
		if got, done := read[count]; !done || got != want[count] {
			t.Errorf("after %d kills for memory with no run held, the agent's slots are %d (read %t), want %d", count, got, done, want[count])
		}
	}
	if got, done := read[1]; done && got != want[1] {
		t.Errorf("after 1 kill for memory, the agent's slots are %d, want %d", got, want[1])
	}
}

// requireAStoppedOwnerLosesItsRun: an agent that renews nothing holds its run no longer: it is
// killed once it has missed 3 renewals, and the next agent claims the run.
func requireAStoppedOwnerLosesItsRun(t *testing.T) {
	server := startAgentServer(t, nil)
	pid := server.agentStarted(t, 1, 30*time.Second)
	server.waitForAgentRenewal(t)
	run := server.recordRun(t, "alice", "queued", unreadShape)
	owner := server.stopHolding(t, pid, run)
	server.agentStarted(t, 2, 40*time.Second)
	server.eventually(t, 30*time.Second, "the next agent to claim the run", func() bool { return server.run(t, run).state == "failed" })
	if row := server.run(t, run); row.reason != agent.ReasonNoModel {
		t.Errorf("the run the stopped agent %s held stopped with %q, want the next agent's end of it, %q", owner, row.reason, agent.ReasonNoModel)
	}
	if !server.logged(`msg="dispatch: agent killed: it renewed nothing for 30s" pid=` + strconv.Itoa(pid) + " ") {
		t.Errorf("the stopped agent %d was not killed for its renewals:\n%s", pid, server.Output.String())
	}
}

// requireTheRestartBound: the agent starts again 1 s after an exit, then twice as long after each
// quick exit, 30 s at the most, while Dispatch serves on. That the runs wait for the next agent is
// "A lease"'s: an agent killed at each start claims nothing, so a run left queued here would say
// only that no agent lived long enough to claim it.
func requireTheRestartBound(t *testing.T) {
	t.Run("each quick exit doubles the wait, up to 30 s", requireTheRestartWaits)
	t.Run("Dispatch serves on while the agent starts again", func(t *testing.T) {
		server := startAgentServer(t, map[string]string{"DISPATCH_TEST_AGENT_CEILING": "1"})
		server.agentStarted(t, 3, 30*time.Second)
		server.requireServing(t)
	})
}
