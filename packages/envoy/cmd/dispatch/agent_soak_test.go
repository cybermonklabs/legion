//go:build multiprocess

package main

// agent_soak_test.go holds check c21: background runs, and plans that wait for their person, hold
// no memory that grows with time. A real envoy-dispatch server on a real database runs its own
// envoy-dispatch agent, which calls a fake chat model over https (going/agent/agenttest) with the
// agent's test hooks on. The agent's live heap and goroutines are read from the collection its
// test hooks make at each SIGUSR1 (collectAgentOnSignal), and the server's live heap from /metrics
// just after its profiler's heap profile made it collect. It carries the multiprocess tag, as the
// other tests of the agent do (agent_loop_test.go), and the fake model's script it shares with
// the memory build's load test is agent_harness_test.go's.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agent/agenttest"
)

const (
	// soakRuns is how many runs the soak works, and soakWarmUpRuns how many work before it, to
	// fill the server's caches and the agent's connections, which the soak's runs reuse.
	soakRuns       = 1000
	soakWarmUpRuns = 100
	// soakWaitingPlans is how many plans wait for their person at once.
	soakWaitingPlans = 1000
	// soakBytesEach is less than what the agent's live heap may grow by for each run, and the
	// server's for each waiting plan.
	soakBytesEach = 1 << 10
	// soakBatch is how many runs the soak starts at once, four times the agent's 8 slots, each of
	// a person of its own, so no slot waits for the test and no person's bound of 3 runs at once.
	soakBatch = 32
	// soakLargeSpec is the length of the spec a large answer reads: a read of its text answers
	// the 1 MiB a call's answer may carry.
	soakLargeSpec = 1_040_000
)

// soakKind is what one run of the soak does.
type soakKind int

const (
	// soakAnswer answers at once.
	soakAnswer soakKind = iota
	// soakSearch searches Dispatch, then answers.
	soakSearch
	// soakLargeAnswer reads a spec of 1 MiB, then answers.
	soakLargeAnswer
	// soakApproval proposes a plan, which its person approves, and answers once the plan ran.
	soakApproval
	// soakFollowUp answers, and a follow-up of it answers too.
	soakFollowUp
	// soakStop is stopped by its person while its model call is in flight.
	soakStop
	// soakBudget stops at the output tokens of a Fast run's model call.
	soakBudget
	// soakLostLease loses its lease to another owner while its model call is in flight, and is
	// then stopped.
	soakLostLease
	// soakPanic calls a tool that panics, which the model loop recovers, then answers.
	soakPanic
	// soakKinds is how many kinds there are.
	soakKinds
)

var soakKindNames = [soakKinds]string{"answer", "search", "large answer", "approval", "follow-up", "stop", "budget", "lost lease", "panic"}

func (k soakKind) String() string { return soakKindNames[k] }

// end is how a run of the kind ends: its state, and what its stop reason starts with.
func (k soakKind) end() (state, reason string) {
	switch k {
	case soakStop, soakLostLease:
		return "stopped", "STOPPED"
	case soakBudget:
		return "stopped", agent.ReasonOutputTokens
	}
	return "done", ""
}

// soakRig is the soak's server and its agent, the fake model's script, and what the runs read and
// write: the issue an approved plan comments on and the spec a large answer reads.
type soakRig struct {
	server   agentServer
	model    *soakModel
	agentPid int
	profiler string
	issue    string
	spec     string
	// never is a channel nothing closes: a model call held on it waits until its client leaves.
	never chan struct{}
	// next numbers the next run's words, and mismatches counts the runs that ended otherwise
	// than their kind does.
	next, mismatches int
}

// startSoak starts the soak's server, with its profiler on a port the kernel chooses, and the
// project, the issue and the spec its runs work on.
func startSoak(t *testing.T) *soakRig {
	t.Helper()
	fake, model := newSoakModel(t)
	env := modelEnv(t, fake)
	env["DISPATCH_PPROF_PORT"] = "0"
	rig := &soakRig{server: startAgentServer(t, env), model: model, never: make(chan struct{})}
	rig.agentPid = rig.server.agentStarted(t, 1, 30*time.Second)
	rig.server.waitForAgentRenewal(t)
	listening := pprofListening.FindStringSubmatch(rig.server.Output.String())
	if listening == nil {
		t.Fatalf("the server logged no line naming its profiler's address:\n%s", rig.server.Output.String())
	}
	rig.profiler = listening[1]
	rig.server.createProject(t, "SOAK", "Soak")
	rig.issue, _ = rig.server.createIssueIn(t, "SOAK", "The issue the soak comments on", "Context.\n")
	var spec strings.Builder
	for paragraph := 0; spec.Len() < soakLargeSpec; paragraph++ {
		fmt.Fprintf(&spec, "Paragraph %d: %s\n\n", paragraph, strings.Repeat("the soak reads this spec whole. ", 31))
	}
	_, rig.spec = rig.server.createIssueIn(t, "SOAK", "A spec of 1 MiB", spec.String())
	return rig
}

// soakRun is one run of the soak as the test drives it.
type soakRun struct {
	kind         soakKind
	login, words string
	level        string
	// id is the run working now: the first, then the follow-up of a follow-up's.
	id string
	// release lets a lost lease's model call answer once its lease is another owner's.
	release chan struct{}
	// stage is how far the test has driven the run: 0 at its start, 1 once the test approved its
	// plan, stopped it, took its lease or started its follow-up, and 2 once it stopped a run
	// whose lease it took; stolen is when it took that lease.
	stage  int
	stolen time.Time
	ended  bool
}

// script makes the next run of the soak, of the kind its number gives, and scripts its replies.
func (r *soakRig) script() *soakRun {
	index := r.next
	r.next++
	run := &soakRun{kind: soakKind(index % int(soakKinds)), login: fmt.Sprintf("soak%02d", index%soakBatch), words: fmt.Sprintf("soak run %d", index)}
	answered := func(int) agenttest.Reply { return answer("Answered.") }
	readThenAnswer := func(read agenttest.Reply) func(int) agenttest.Reply {
		return func(n int) agenttest.Reply {
			if n == 1 {
				return read
			}
			return answer("Read it.")
		}
	}
	switch run.kind {
	case soakAnswer:
		r.model.set(run.words, answered)
	case soakSearch:
		r.model.set(run.words, readThenAnswer(readCall("dispatch_search", map[string]any{"query": "soak comments"})))
	case soakLargeAnswer:
		r.model.set(run.words, readThenAnswer(readCall("dispatch_doc_read", map[string]any{"artifact": r.spec})))
	case soakApproval:
		r.model.set(run.words, readThenAnswer(r.commentPlan(run.words)))
	case soakFollowUp:
		r.model.set(run.words, answered)
		r.model.set(run.words+", and again", answered)
	case soakStop:
		r.model.set(run.words, func(int) agenttest.Reply { return agenttest.Reply{Hold: r.never} })
	case soakBudget:
		run.level = "fast"
		r.model.set(run.words, func(int) agenttest.Reply {
			return agenttest.Reply{Text: "A reply cut at its tokens", Finish: "length", Output: 4000}
		})
	case soakLostLease:
		run.release = make(chan struct{})
		r.model.set(run.words, func(n int) agenttest.Reply {
			if n == 1 {
				return agenttest.Reply{Text: "Held.", Hold: run.release}
			}
			return answer("Never reached.")
		})
	case soakPanic:
		r.model.set(run.words, readThenAnswer(toolCall("test_panic", map[string]any{})))
	}
	return run
}

// commentPlan is the reply that proposes a plan of one comment on the soak's issue.
func (r *soakRig) commentPlan(words string) agenttest.Reply {
	return toolCall("propose_plan", map[string]any{"summary": "One comment.", "changes": []map[string]any{
		{"tool": "dispatch_comment", "arguments": map[string]any{"issue": r.issue, "body": "A comment of " + words + "."}},
	}})
}

// work works at least runs runs, soakBatch at a time, and answers how many it worked.
func (r *soakRig) work(t *testing.T, runs int) int {
	t.Helper()
	worked := 0
	for worked < runs {
		batch := make([]*soakRun, 0, soakBatch)
		for range soakBatch {
			batch = append(batch, r.script())
		}
		worked += r.drive(t, batch)
	}
	return worked
}

// drive starts runs and drives each to its end - it approves each plan, stops each run its person
// stops, takes the lease of each that loses it, and starts each follow-up - and fails the test
// where a run ends otherwise than its kind does. It answers how many runs it started.
func (r *soakRig) drive(t *testing.T, runs []*soakRun) int {
	t.Helper()
	starts := make([]agentRun, len(runs))
	for index, run := range runs {
		starts[index] = agentRun{login: run.login, words: run.words, level: run.level}
	}
	for index, id := range r.server.begin(t, starts...) {
		runs[index].id = id
	}
	started := len(runs)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var working []string
		for _, run := range runs {
			if !run.ended {
				working = append(working, run.id)
			}
		}
		if len(working) == 0 {
			return started
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d runs of the soak did not end within 5 minutes:\n%s", len(working), r.server.Output.String())
		}
		rows := r.server.runRows(t, working)
		for _, run := range runs {
			if !run.ended && r.advance(t, run, rows[run.id]) {
				started++
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// advance drives run one step on from row, as the run's row stands now, and reports whether it
// started a follow-up.
func (r *soakRig) advance(t *testing.T, run *soakRun, row runRow) bool {
	t.Helper()
	switch row.state {
	case "done", "stopped", "failed":
		if run.kind == soakFollowUp && run.stage == 0 && row.state == "done" {
			run.id = r.followUp(t, run)
			run.stage = 1
			return true
		}
		run.ended = true
		if state, reason := run.kind.end(); row.state != state || !strings.HasPrefix(row.reason, reason) {
			r.mismatches++
			if r.mismatches <= 20 {
				t.Errorf("%s, a run of kind %s, ended %s with %q, want %s with %s", run.words, run.kind, row.state, row.reason, state, reason)
			}
		}
		return false
	}
	switch {
	case run.kind == soakApproval && run.stage == 0 && row.state == "waiting":
		r.as(t, run.login, http.MethodPost, "/api/v1/going/runs/"+run.id+"/plan/approve", `{"plan":1}`)
		run.stage = 1
	case run.kind == soakStop && run.stage == 0 && r.model.requests(run.words) > 0:
		r.as(t, run.login, http.MethodPost, "/api/v1/going/runs/"+run.id+"/stop", "")
		run.stage = 1
	case run.kind == soakLostLease && run.stage == 0 && r.model.requests(run.words) > 0:
		r.server.exec(t, `
			update going_runs set lease_token = gen_random_uuid(), lease_owner = 'an agent of another server', lease_ends_at = now() + interval '1 hour'
			where id = $1::uuid
		`, run.id)
		close(run.release)
		run.stage, run.stolen = 1, time.Now()
	case run.kind == soakLostLease && run.stage == 1 && time.Since(run.stolen) > time.Second:
		r.server.stop(t, run.id)
		run.stage = 2
	}
	return false
}

// as sends a request of the person login's and fails the test unless it is answered 2xx.
func (r *soakRig) as(t *testing.T, login, method, path, body string) httpAnswer {
	t.Helper()
	answered := r.server.send(t, method, path, body, asUser(login))
	if answered.status/100 != 2 {
		t.Fatalf("%s %s as %s answered %s", method, path, login, answered)
	}
	return answered
}

// followUp starts the follow-up of run, as its person does, and answers its id.
func (r *soakRig) followUp(t *testing.T, run *soakRun) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"words": run.words + ", and again", "continues": run.id})
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.as(t, run.login, http.MethodPost, "/api/v1/going/runs", string(body)).body, &started); err != nil || started.ID == "" {
		t.Fatalf("the follow-up of %s answered no run id (%v)", run.words, err)
	}
	return started.ID
}

// waitingPlans starts count runs that each propose a plan and leaves every plan waiting for its
// person, a hundred at a time.
func (r *soakRig) waitingPlans(t *testing.T, count int) {
	t.Helper()
	for first := 0; first < count; first += 100 {
		starts := make([]agentRun, 0, 100)
		for index := first; index < min(first+100, count); index++ {
			words := fmt.Sprintf("a plan that waits, %d", index)
			r.model.set(words, func(int) agenttest.Reply { return r.commentPlan(words) })
			starts = append(starts, agentRun{login: fmt.Sprintf("plan%03d", index%100), words: words})
		}
		ids := r.server.begin(t, starts...)
		r.server.eventually(t, 5*time.Minute, fmt.Sprintf("plans %d to %d to wait for their person", first, first+len(ids)), func() bool {
			for _, row := range r.server.runRows(t, ids) {
				if row.state != "waiting" {
					if row.state != "queued" && row.state != "working" {
						t.Fatalf("a run that proposes a plan ended %s with %q, want waiting", row.state, row.reason)
					}
					return false
				}
			}
			return true
		})
	}
}

// agentCollection is one collection the agent made at a SIGUSR1 (collectAgentOnSignal): its
// number, the heap it found live and the goroutines that ran.
type agentCollection struct {
	number     int
	live       int64
	goroutines int
}

// agentCollectedLine is the line the agent writes once the collection a SIGUSR1 asked for has run.
var agentCollectedLine = regexp.MustCompile(`agent: collection (\d+) found (\d+) bytes live and (\d+) goroutines\n`)

// collectAgent asks agent pid for a collection and answers it once the server has logged it.
func (s agentServer) collectAgent(t *testing.T, pid int) agentCollection {
	t.Helper()
	from := len(s.Output.String())
	signalProcess(t, pid, syscall.SIGUSR1)
	var collected agentCollection
	s.eventually(t, 30*time.Second, "the agent's collection", func() bool {
		match := agentCollectedLine.FindStringSubmatch(s.Output.String()[from:])
		if match == nil {
			return false
		}
		collected.number, _ = strconv.Atoi(match[1])
		collected.live, _ = strconv.ParseInt(match[2], 10, 64)
		collected.goroutines, _ = strconv.Atoi(match[3])
		return true
	})
	return collected
}

// agentAtRest is the agent's heap at rest: of three collections 200 ms apart, the one that found
// the least live, so a renew or a claim in flight at one of them is not counted.
func (s agentServer) agentAtRest(t *testing.T, pid int) agentCollection {
	t.Helper()
	least := s.collectAgent(t, pid)
	for range 2 {
		time.Sleep(200 * time.Millisecond)
		if collected := s.collectAgent(t, pid); collected.live < least.live {
			least = collected
		}
	}
	return least
}

// agentSettled waits up to 30 s for the agent at rest to run goroutines goroutines, and answers
// its collection at rest once it does, or the last one when it does not.
func (s agentServer) agentSettled(t *testing.T, pid, goroutines int) agentCollection {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		collected := s.agentAtRest(t, pid)
		if collected.goroutines == goroutines || time.Now().After(deadline) {
			return collected
		}
		time.Sleep(time.Second)
	}
}

// serverLiveHeap is the server's live heap at rest: its profiler's heap profile makes it collect
// (gc=1), and /metrics then shows the heap that collection found live; the least of three such
// reads 200 ms apart, so a request in flight at one of them is not counted.
func (s agentServer) serverLiveHeap(t *testing.T, profiler string) int64 {
	t.Helper()
	var least int64
	for read := range 3 {
		if read > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		response, err := http.Get("http://" + profiler + "/debug/pprof/heap?gc=1")
		if err != nil {
			t.Fatalf("GET /debug/pprof/heap?gc=1 on the server's profiler: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET /debug/pprof/heap?gc=1 on the server's profiler answered %d", response.StatusCode)
		}
		live := s.scrape(t)[`envoy_dispatch_process_heap_live_bytes{process="server"}`]
		if read == 0 || live < least {
			least = live
		}
	}
	return least
}

// TestBackgroundRunsHoldNoMemory: runs and the plans that wait hold no memory that grows with time
// (check c21). Once the server's caches are full, a soak of 1,000 runs - answers, searches, reads
// of a spec of 1 MiB, approved plans, follow-ups, runs their person stops, runs stopped at a
// bound of their level, runs that lose their lease and runs whose tool panics - grows the agent's
// live heap by less than 1 KiB for each run, and the agent's goroutines come back to their count
// at the soak's start. Then 1,000 plans wait for their person: the agent runs no goroutine more
// and holds less than 1 KiB for each, and so does the server. goleak holds the goroutines of the
// runs' own test packages (going/agent, going/agentwire, internal/dispatch/api).
func TestBackgroundRunsHoldNoMemory(t *testing.T) {
	rig := startSoak(t)
	rig.work(t, soakWarmUpRuns)

	start := rig.server.agentAtRest(t, rig.agentPid)
	worked := rig.work(t, soakRuns)
	end := rig.server.agentSettled(t, rig.agentPid, start.goroutines)
	grown := end.live - start.live
	t.Logf("the soak worked %d runs: the agent's live heap %d -> %d bytes (%+d, %.1f bytes a run), its goroutines %d -> %d",
		worked, start.live, end.live, grown, float64(grown)/float64(worked), start.goroutines, end.goroutines)
	if end.goroutines != start.goroutines {
		t.Errorf("after the soak the agent runs %d goroutines, want the %d it ran at the soak's start", end.goroutines, start.goroutines)
	}
	if grown >= int64(worked)*soakBytesEach {
		t.Errorf("over %d runs the agent's live heap grew by %d bytes, want less than %d bytes a run (%d)", worked, grown, soakBytesEach, int64(worked)*soakBytesEach)
	}

	serverBefore := rig.server.serverLiveHeap(t, rig.profiler)
	rig.waitingPlans(t, soakWaitingPlans)
	waiting := rig.server.agentSettled(t, rig.agentPid, end.goroutines)
	serverWaiting := rig.server.serverLiveHeap(t, rig.profiler)
	t.Logf("%d plans wait: the agent's live heap %d -> %d bytes (%+d), its goroutines %d -> %d; the server's live heap %d -> %d bytes (%+d, %.1f bytes a plan)",
		soakWaitingPlans, end.live, waiting.live, waiting.live-end.live, end.goroutines, waiting.goroutines,
		serverBefore, serverWaiting, serverWaiting-serverBefore, float64(serverWaiting-serverBefore)/soakWaitingPlans)
	if waiting.goroutines != end.goroutines {
		t.Errorf("with %d plans waiting the agent runs %d goroutines, want the %d it ran before them", soakWaitingPlans, waiting.goroutines, end.goroutines)
	}
	if held := waiting.live - end.live; held >= soakWaitingPlans*soakBytesEach {
		t.Errorf("with %d plans waiting the agent holds %d bytes more, want less than %d bytes a plan", soakWaitingPlans, held, soakBytesEach)
	}
	if held := serverWaiting - serverBefore; held >= soakWaitingPlans*soakBytesEach {
		t.Errorf("with %d plans waiting the server holds %d bytes more, want less than %d bytes a plan", soakWaitingPlans, held, soakBytesEach)
	}
	if pids := rig.server.agentPids(); len(pids) != 1 {
		t.Errorf("the server started %d agents, want the one the soak read, which nothing killed", len(pids))
	}
}
