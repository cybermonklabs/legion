//go:build memory

package main

// agent_load_test.go holds checks c26 and c29, which the dispatch-memory job runs (-tags memory):
// the server's heap and the agent's memory under 8 Deep runs at their limits on a Dispatch seeded
// as large as production, and the server's CPU while the agent works at its soft limit. Each
// starts a real envoy-dispatch server whose own agent calls a fake chat model over https, the
// soak's script (soakModel, agent_harness_test.go) answering it.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// The seed of c26: as large as production for each kind the runs' routes read where a figure of
// production gives the size, and otherwise the size named here.
const (
	// loadRosterSessions is the sessions the Envoy listener lists. No figure of production's roster
	// is recorded, so it is the 200,000 c26's negative control names.
	loadRosterSessions = 200_000
	// loadIssues is the issues of the project the runs list: production's largest project held at
	// least 4,580 on 2026-11-20, the number of the latest issue its records name
	// (dispatch://INFRA-4580), since an issue's key numbers it within its project.
	loadIssues = 5_000
	// loadComments is the comments of the issue whose events the runs read, each an event, which
	// the search finds too. No figure of production's is recorded.
	loadComments = 1_000
	// loadRuns is the Deep runs that work at once, each reading its own spec of 1 MiB and editing
	// it.
	loadRuns = 8
	// loadSpec is the length of each run's spec, which a read of its text answers whole within the
	// 1 MiB a call's answer may carry.
	loadSpec = 1_000_000
)

// The bounds of c26.
const (
	// loadServerRise is the most the server's live heap outside the document rooms may rise above
	// its level before the runs, and loadSettleShare the most it may stay above that level once
	// the rooms have closed.
	loadServerRise  = 1 << 30
	loadSettleShare = 0.05
	// loadClaimGate is the share of the agent's GOMEMLIMIT its claim gate stands at (going/agent's
	// claimGate), from which the slots are set.
	loadClaimGate = 0.7
)

// The shape of a run at its limits: each model reply carries loadReplyText of prose and pads its
// stream to loadPadding, under the 8 MiB a stream may carry. The last 8 model turns of a run's
// history go to the model whole, each a reply and the 32 KiB of its one read, so 8 turns of a
// 160 KiB reply stay under the 2 MiB a history may hold.
const (
	loadReplyText = 160 << 10
	loadPadding   = 6 << 20
)

// loadRooms matches a frame of the document rooms, ygo's and the docs package's room code, whose
// heap dispatch://INFRA-179's room budget bounds: c26 bounds the heap outside them.
var loadRooms = regexp.MustCompile(`reearth/ygo|internal/dispatch/docs\.\(\*room`)

// loadRig is c26's server, its profiler, the fake model's script, the agent's limits, and what the
// runs read: the project they list, the issue whose events they read, and each run's spec, which
// the run reads and edits.
type loadRig struct {
	server   agentServer
	model    *soakModel
	profiler string
	limits   agentMemory
	project  string
	events   string
	specs    []loadSpecOf
}

// loadSpecOf is the spec of one run: its issue and its document, and how many edits runs have
// made of it, the n-th of which replaces markerN with marker(N+1).
type loadSpecOf struct {
	issue, document string
	edits           int
}

// startLoad starts the server of c26 beside an Envoy listener that lists loadRosterSessions
// sessions, and seeds it.
func startLoad(t *testing.T) *loadRig {
	t.Helper()
	listener := rosterListener(t, loadRosterSessions)
	fake, model := newSoakModel(t)
	env := modelEnv(t, fake)
	env["DISPATCH_PPROF_PORT"] = "0"
	env["ENVOY_URL"] = listener
	rig := &loadRig{server: startAgentServer(t, env), model: model, project: "LOAD"}
	rig.server.waitForAgentRenewal(t)
	listening := pprofListening.FindStringSubmatch(rig.server.Output.String())
	if listening == nil {
		t.Fatalf("the server logged no line naming its profiler's address:\n%s", rig.server.Output.String())
	}
	rig.profiler = listening[1]
	container, err := containerMemoryLimit()
	if err != nil {
		t.Fatal(err)
	}
	rig.limits = agentLimits(container)
	rig.seed(t)
	return rig
}

// rosterListener is an Envoy listener whose session list holds sessions sessions, newest seen
// first, and answers its URL.
func rosterListener(t *testing.T, sessions int) string {
	t.Helper()
	now := time.Now().UnixMilli()
	var listed strings.Builder
	listed.WriteByte('[')
	for index := range sessions {
		if index > 0 {
			listed.WriteByte(',')
		}
		fmt.Fprintf(&listed, `{"session_id":"load-session-%06d","title":"A worker of the load test, number %d","state":"live","last_seen":%d,"roles":["worker"],"capabilities":["chat"],"machine_id":"load-machine","host":"load-host"}`,
			index, index, now-int64(index))
	}
	listed.WriteByte(']')
	body := []byte(listed.String())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// seed writes the project of loadIssues issues, the issue of loadComments comments, and a spec of
// 1 MiB for each run, and fills the document read cache to its budget, as production's is
// (/debug/vars of 2026-11-20: 268,213,454 bytes of its 268,435,456).
func (r *loadRig) seed(t *testing.T) {
	t.Helper()
	s := r.server
	s.createProject(t, r.project, "Load")
	r.events, _ = s.createIssueIn(t, r.project, "The issue whose events the runs read", "Context.\n")
	r.parallel(t, loadIssues, func(index int) (string, string) {
		return "/api/v1/issues", fmt.Sprintf(`{"project":%q,"title":"Issue %d of the load test","force":true}`, r.project, index)
	})
	r.parallel(t, loadComments, func(index int) (string, string) {
		return "/api/v1/issues/" + r.events + "/comments", fmt.Sprintf(`{"body":"Load comment %d, which the runs search for."}`, index)
	})
	spec := loadMarkdown(loadSpec)
	for index := range loadRuns {
		issue, document := s.createIssueIn(t, r.project, fmt.Sprintf("The spec run %d reads and edits", index), spec)
		r.specs = append(r.specs, loadSpecOf{issue: issue, document: document})
	}
	r.fillReadCache(t, spec)
}

// loadMarkdown is prose of at least length bytes, in paragraphs of about a kilobyte, with the word
// marker0 in its first, which the first run's edit replaces.
func loadMarkdown(length int) string {
	var markdown strings.Builder
	markdown.WriteString("The marker0 of this spec.\n\n")
	for paragraph := 0; markdown.Len() < length; paragraph++ {
		fmt.Fprintf(&markdown, "Paragraph %d: %s\n\n", paragraph, strings.Repeat("a run at its limits reads this spec whole. ", 23))
	}
	return markdown.String()
}

// parallel sends count requests of the test's login, eight at a time, each the POST at the path
// with the body request names for its index, and fails the test unless each is answered 2xx.
func (r *loadRig) parallel(t *testing.T, count int, request func(index int) (path, body string)) {
	t.Helper()
	var mu sync.Mutex
	var failures []string
	indexes := make(chan int)
	var senders sync.WaitGroup
	for range 8 {
		senders.Go(func() {
			for index := range indexes {
				path, body := request(index)
				answered, err := r.server.trySend(http.MethodPost, path, body, asUser(dispatchTestLogin))
				if err != nil || answered.status/100 != 2 {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("POST %s: %s %v", path, answered, err))
					mu.Unlock()
				}
			}
		})
	}
	for index := range count {
		indexes <- index
	}
	close(indexes)
	senders.Wait()
	if len(failures) > 0 {
		t.Fatalf("%d of %d seed requests failed, the first: %s", len(failures), count, failures[0])
	}
}

// fillReadCache uploads one document of markdown, copies it in the database until the copies weigh
// more than the read cache's budget, and reads each copy's text, which the cold read puts in the
// cache, until the cache is within two entries of its budget.
func (r *loadRig) fillReadCache(t *testing.T, markdown string) {
	t.Helper()
	weight, err := docs.DocumentReadWeightForTest(markdown)
	if err != nil {
		t.Fatalf("weigh the document that fills the read cache: %v", err)
	}
	budget := roomFigures(t, r.profiler).ReadCache.Budget
	body, err := json.Marshal(map[string]string{"name": "read-cache-fill.md", "content": markdown})
	if err != nil {
		t.Fatal(err)
	}
	var uploaded struct {
		Artifact struct{ ID string } `json:"artifact"`
	}
	if err := json.Unmarshal(r.server.call(t, http.MethodPost, "/api/v1/issues/"+r.events+"/artifacts", string(body), http.StatusCreated), &uploaded); err != nil {
		t.Fatal(err)
	}
	r.waitSettled(t, uploaded.Artifact.ID)
	copies := int(budget/weight) + 4
	for number := range copies {
		copied := cloneArtifact(t, r.server.database, uploaded.Artifact.ID, fmt.Sprintf("read-cache-fill-%03d", number))
		r.server.call(t, http.MethodGet, "/api/v1/artifacts/"+copied+"/text", "", http.StatusOK)
	}
	held := roomFigures(t, r.profiler).ReadCache
	t.Logf("the read cache holds %d entries of %d MiB, its budget %d MiB, after %d reads of documents weighing %d MiB", held.Entries, held.Bytes>>20, held.Budget>>20, copies, weight>>20)
	if held.Entries == 0 || held.Bytes <= held.Budget-2*(held.Bytes/int64(held.Entries)) {
		t.Fatalf("the read cache holds %d entries of %d MiB, want within two entries of its %d MiB budget", held.Entries, held.Bytes>>20, held.Budget>>20)
	}
}

// waitSettled waits up to 2 minutes for the document to settle every write it has had.
func (r *loadRig) waitSettled(t *testing.T, artifactID string) {
	t.Helper()
	r.server.eventually(t, 2*time.Minute, "document "+artifactID+" to settle", func() bool {
		var owed bool
		if err := r.server.database.Pool.QueryRow(t.Context(),
			`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID).Scan(&owed); err != nil {
			t.Fatal(err)
		}
		return !owed
	})
}

// script scripts the run whose words are words as a Deep run at its limits on spec: ten reads -
// the project's issue listing, a search, the roster, the events of the issue of comments and its
// own spec of 1 MiB, twice over - each reply carrying loadReplyText of prose in a stream padded to
// loadPadding, then a plan that edits its spec, the spec's next marker for its last, then its
// answer.
func (r *loadRig) script(words string, spec *loadSpecOf) {
	from, to := fmt.Sprintf("marker%d", spec.edits), fmt.Sprintf("marker%d", spec.edits+1)
	spec.edits++
	reads := []agenttest.Reply{
		readCall("dispatch_issues", map[string]any{"project": r.project}),
		readCall("dispatch_search", map[string]any{"query": "load comment"}),
		readCall("read_roster", map[string]any{}),
		readCall("dispatch_read", map[string]any{"issue": r.events, "all_events": true}),
		readCall("dispatch_doc_read", map[string]any{"artifact": spec.document}),
	}
	prose := strings.Repeat("A Deep run at its limits thinks aloud. ", loadReplyText/39)
	r.model.set(words, func(n int) agenttest.Reply {
		var reply agenttest.Reply
		switch {
		case n <= 2*len(reads):
			reply = reads[(n-1)%len(reads)]
		case n == 2*len(reads)+1:
			reply = toolCall("propose_plan", map[string]any{"summary": "Edit the spec.", "changes": []map[string]any{{
				"tool": "dispatch_doc_edit", "arguments": map[string]any{"issue": spec.issue, "artifact": "spec",
					"ops": []map[string]any{{"op": "replace", "find": from, "with": to}}},
			}}})
		default:
			return answer("Read everything and edited the spec.")
		}
		reply.Text, reply.Padding = prose, loadPadding
		return reply
	})
}

// loadPeaks is the most the server's live heap outside the rooms and the agent's total Go memory
// reached while runs worked.
type loadPeaks struct {
	serverOutside, agentTotal int64
}

// work starts a Deep run at its limits on each of specs, approves each plan as it waits, and
// fails the test unless each run ends done. While they work it reads the server's live heap outside
// the rooms every 2 s and the agent's total Go memory, as its renews carry it, every 500 ms, and
// once they have ended it reads the agent's for two renews more, since a renew carries it at most
// every 10 s and the runtime's total does not fall; it answers the most of each.
func (r *loadRig) work(t *testing.T, label string, specs []loadSpecOf) loadPeaks {
	t.Helper()
	starts := make([]agentRun, len(specs))
	for index := range specs {
		words := fmt.Sprintf("%s, run %d", label, index)
		r.script(words, &specs[index])
		starts[index] = agentRun{login: fmt.Sprintf("load%d", index), words: words, level: "deep"}
	}
	ids := r.server.begin(t, starts...)
	var peaks loadPeaks
	sampleAgent := func() {
		peaks.agentTotal = max(peaks.agentTotal, r.server.scrape(t)[`envoy_dispatch_process_memory_total_bytes{process="agent"}`])
	}
	approved := map[string]bool{}
	var heapAt, agentAt time.Time
	deadline := time.Now().Add(15 * time.Minute)
	for {
		if time.Since(heapAt) >= 2*time.Second {
			peaks.serverOutside = max(peaks.serverOutside, r.heapOutsideRooms(t))
			heapAt = time.Now()
		}
		if time.Since(agentAt) >= 500*time.Millisecond {
			sampleAgent()
			agentAt = time.Now()
		}
		ended := 0
		for id, row := range r.server.runRows(t, ids) {
			switch row.state {
			case "waiting":
				if !approved[id] {
					r.server.send(t, http.MethodPost, "/api/v1/going/runs/"+id+"/plan/approve", `{"plan":1}`, asUser(starts[slices.Index(ids, id)].login))
					approved[id] = true
				}
			case "done":
				ended++
			case "stopped", "failed":
				t.Fatalf("%s: run %s ended %s with %q, want done", label, id, row.state, row.reason)
			}
		}
		if ended == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d of %d runs ended within 15 minutes:\n%s", label, ended, len(ids), r.server.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for renewed := time.Now(); time.Since(renewed) < 25*time.Second; time.Sleep(500 * time.Millisecond) {
		sampleAgent()
	}
	return peaks
}

// heapOutsideRooms is the server's live heap outside the document rooms as one collection finds
// it: its profiler's heap profile makes it collect (gc=1, takeC28Set), /metrics shows the heap that
// collection found live, and what that profile's samples with a frame of the rooms hold is taken
// from it.
func (r *loadRig) heapOutsideRooms(t *testing.T) int64 {
	t.Helper()
	set := takeC28Set(t, r.profiler)
	live := r.server.scrape(t)[`envoy_dispatch_process_heap_live_bytes{process="server"}`]
	return live - set.inuse(t, loadRooms, nil)
}

// loadRest is the server at rest between rounds of runs: its live heap outside the rooms, the heap
// profile of the collection that found it, the bytes of its three caches as /metrics shows them,
// and what the database owed the server when the rooms had closed.
type loadRest struct {
	outside int64
	set     c28Set
	caches  map[string]int64
	owed    owedBeforeStart
}

// rest reads the server at rest. It waits for the rooms to close and then for the database to owe
// the server nothing - no settlement, no block index, no re-index (memoryHarness.readOwed), the
// work a server does on its own after the runs' writes, which reads whole documents outside the
// rooms - and fails the test where it still owes something after 5 minutes. Of three collections
// 200 ms apart it then keeps the one that found the least live outside the rooms, so a request in
// flight at one of them is not counted.
func (r *loadRig) rest(t *testing.T) loadRest {
	t.Helper()
	waitForResidentRooms(t, r.profiler, 0)
	owing := &memoryHarness{t: t, database: r.server.database}
	first := owing.readOwed(t)
	deadline := time.Now().Add(5 * time.Minute)
	for owed := first; owed != (owedBeforeStart{}); owed = owing.readOwed(t) {
		if time.Now().After(deadline) {
			t.Fatalf("5 minutes after the runs the database still owes the server %+v, so its heap at rest cannot be read", owed)
		}
		time.Sleep(500 * time.Millisecond)
	}
	var rest loadRest
	for read := range 3 {
		if read > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		set := takeC28Set(t, r.profiler)
		figures := r.server.scrape(t)
		outside := figures[`envoy_dispatch_process_heap_live_bytes{process="server"}`] - set.inuse(t, loadRooms, nil)
		if read == 0 || outside < rest.outside {
			rest = loadRest{outside: outside, set: set, caches: map[string]int64{}}
			for _, cache := range []string{"read", "display_tree", "list_answer"} {
				rest.caches[cache] = figures[`envoy_dispatch_cache_bytes{cache="`+cache+`"}`]
			}
		}
	}
	rest.owed = first
	return rest
}

// grewMost is the five functions whose flat heap outside the rooms grew most from before to after,
// as c28 traces a growth (pprof's -top -diff_base).
func grewMost(t *testing.T, before, after c28Set) []functionGrowth {
	t.Helper()
	flat := func(set c28Set) map[string]int64 {
		samples, index := set.kept(t, nil, loadRooms)
		byFunction := map[string]int64{}
		for _, sample := range samples {
			byFunction[allocatingFunction(sample)] += sample.Value[index]
		}
		return byFunction
	}
	was, now := flat(before), flat(after)
	growths := make([]functionGrowth, 0, len(now))
	for function, bytes := range now {
		growths = append(growths, functionGrowth{function: function, bytes: bytes - was[function]})
	}
	slices.SortFunc(growths, func(a, b functionGrowth) int { return cmp.Compare(b.bytes, a.bytes) })
	return growths[:min(5, len(growths))]
}

// TestTheServerHeapUnderEightDeepRuns: the server's memory for the agent's calls, and the agent's
// memory at its slots, have the bounds a load test measures (check c26). The seeded Dispatch holds
// as many rows as production of each kind the runs' routes read (the constants above say how many
// and why), and its read cache is full. One Deep run at its limits measures one run's peak - the
// most the agent's total Go memory rises while it works - which sets the slots: the claim gate,
// 70% of the agent's GOMEMLIMIT, less the agent's base, divided by that peak, 8 at the most; the
// agent's slots (agentSlots) hold that value. 8 Deep runs at once then fill every cache the runs
// fill, and the server is read at rest (loadRest). Then 8 Deep runs at once each read the issue
// listing, search, the roster, an issue's events and a spec of 1 MiB, twice, and edit their spec:
// the server's live heap outside the document rooms stays under 1 GiB above its level at rest
// before them and, at rest once they have ended, comes back to within 5% of it, and the agent's
// total Go memory stays under its GOMEMLIMIT.
func TestTheServerHeapUnderEightDeepRuns(t *testing.T) {
	rig := startLoad(t)

	base := rig.server.scrape(t)[`envoy_dispatch_process_memory_total_bytes{process="agent"}`]
	one := rig.work(t, "one run's peak", rig.specs[:1])
	peak := one.agentTotal - base
	gate := int64(loadClaimGate * float64(rig.limits.AgentLimit))
	slots := loadRuns
	if peak > 0 {
		slots = int(min(int64(loadRuns), max(1, (gate-base)/peak)))
	}
	t.Logf("one Deep run at its limits raised the agent's total Go memory from its base of %d MiB by %d MiB; the claim gate of %d MiB (70%% of the agent's GOMEMLIMIT of %d MiB) less the base, divided by that peak, sets %d slots (8 at the most)",
		base>>20, peak>>20, gate>>20, rig.limits.AgentLimit>>20, slots)
	if slots != agentSlots {
		t.Errorf("one run's measured peak of %d MiB sets %d slots, and the agent works %d (agentSlots): set agentSlots to %d, citing this measurement", peak>>20, slots, agentSlots, slots)
	}

	rig.work(t, "eight Deep runs that fill the caches", rig.specs)
	before := rig.rest(t)
	eight := rig.work(t, "eight Deep runs", rig.specs)
	after := rig.rest(t)
	t.Logf("the server's live heap outside the rooms: %d MiB at rest before the 8 runs, %d MiB at most while they worked (%+d MiB), %d MiB at rest after them; the agent's total Go memory at most %d MiB with its %d slots full, its GOMEMLIMIT %d MiB",
		before.outside>>20, eight.serverOutside>>20, (eight.serverOutside-before.outside)>>20, after.outside>>20, eight.agentTotal>>20, slots, rig.limits.AgentLimit>>20)
	t.Logf("once the rooms had closed the database owed the server %+v before the 8 runs and %+v after them; the caches held %v bytes before and %v bytes after; the functions whose flat heap outside the rooms grew most: %v",
		before.owed, after.owed, before.caches, after.caches, grewMost(t, before.set, after.set))
	if rise := eight.serverOutside - before.outside; rise >= loadServerRise {
		t.Errorf("under 8 Deep runs the server's live heap outside the rooms rose %d MiB above its level before them, want under %d MiB", rise>>20, loadServerRise>>20)
	}
	if settled := float64(after.outside-before.outside) / float64(before.outside); settled > loadSettleShare {
		t.Errorf("at rest after the 8 runs the server's live heap outside the rooms is %d MiB, %.1f%% above its %d MiB before them, want within %.0f%%",
			after.outside>>20, 100*settled, before.outside>>20, 100*loadSettleShare)
	}
	if eight.agentTotal >= rig.limits.AgentLimit {
		t.Errorf("with its slots full the agent's total Go memory reached %d MiB, want under its GOMEMLIMIT of %d MiB", eight.agentTotal>>20, rig.limits.AgentLimit>>20)
	}
}

// The load of c29.
const (
	// cpuAgentLimit is the GOMEMLIMIT the agent works at the soft limit of: a gigabyte, so the
	// runner holds it with room to spare.
	cpuAgentLimit = 1 << 30
	// cpuReads is the fewest times the fixed read is timed in a window, idle and busy.
	cpuReads = 500
	// cpuWindowLeast is the shortest a window of reads lasts, idle and busy, the reads going on
	// past cpuReads until it has: time for the agent at its soft limit to make collections, which
	// the test requires of a window it judges. On a GKE node of 4 CPUs the hook's allocation filled
	// the heap from its live ~600 MiB to its goal of ~920 MiB in about 300 ms, while 500 reads took
	// 236 to 288 ms there (INFRA-1588, 2026-11-21), so a window of 500 reads could miss every
	// collection; a second holds about three, and still one at a third of that allocation rate.
	cpuWindowLeast = time.Second
	// cpuLivez is the longest /livez may take to answer while the agent works.
	cpuLivez = time.Second
	// cpuAtLimitLow and cpuAtLimitHigh are the memory the agent's runtime counts against its limit,
	// as a share of it, at each end of the window at its soft limit, outside which the test does not
	// judge the window: the agent was not at its soft limit.
	cpuAtLimitLow  = 0.90
	cpuAtLimitHigh = 1.10
)

// cpuRead is one timed request: when it started and how long it took.
type cpuRead struct {
	at   time.Time
	took time.Duration
}

// timedReads is each read of path, one after another as the test's login, each answered 200, with
// when it started and how long it took, until it has made count reads and least has passed since
// the first.
func (s agentServer) timedReads(t *testing.T, path string, count int, least time.Duration) []cpuRead {
	t.Helper()
	reads := make([]cpuRead, 0, count)
	for first := time.Now(); len(reads) < count || time.Since(first) < least; {
		started := time.Now()
		s.call(t, http.MethodGet, path, "", http.StatusOK)
		reads = append(reads, cpuRead{at: started, took: time.Since(started)})
	}
	return reads
}

// p99Rank is which of n times, counted from the slowest, percentile gives as their p99.
func p99Rank(n int) int {
	return n - max(0, int(math.Ceil(0.99*float64(n)))-1)
}

// tookOf is how long each of reads took.
func tookOf(reads []cpuRead) []time.Duration {
	times := make([]time.Duration, len(reads))
	for index, read := range reads {
		times[index] = read.took
	}
	return times
}

// procFigures is what /proc tells of a set of processes, their threads summed: the time they ran on
// a CPU and the time they waited on a run queue for one (each thread's schedstat), the times the
// kernel took a CPU from them (nonvoluntary_ctxt_switches), their minor page faults, their CPU time
// in user space and in the kernel (each process's stat) and their resident memory (VmRSS).
type procFigures struct {
	ran, waited  time.Duration
	preempted    int64
	faults       int64
	user, system time.Duration
	resident     int64
}

// readProcFigures reads the figures of processes pids now. A process or a thread gone between two
// reads is left out.
func readProcFigures(pids ...int) procFigures {
	var figures procFigures
	for _, pid := range pids {
		tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
		for _, task := range tasks {
			dir := fmt.Sprintf("/proc/%d/task/%s/", pid, task.Name())
			if schedstat, err := os.ReadFile(dir + "schedstat"); err == nil {
				if fields := strings.Fields(string(schedstat)); len(fields) >= 2 {
					ran, _ := strconv.ParseInt(fields[0], 10, 64)
					waited, _ := strconv.ParseInt(fields[1], 10, 64)
					figures.ran += time.Duration(ran)
					figures.waited += time.Duration(waited)
				}
			}
			if status, err := os.ReadFile(dir + "status"); err == nil {
				figures.preempted += statusValue(string(status), "nonvoluntary_ctxt_switches:")
			}
		}
		// The fields of a stat after the command's closing parenthesis start at the state, the
		// third, so the minor faults, the tenth, are the eighth of them, and the CPU time in user
		// space and in the kernel, the fourteenth and fifteenth, in the kernel's ticks of 10 ms
		// (USER_HZ), the twelfth and thirteenth.
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			text := string(stat)
			if fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:]); len(fields) > 12 {
				faults, _ := strconv.ParseInt(fields[7], 10, 64)
				user, _ := strconv.ParseInt(fields[11], 10, 64)
				system, _ := strconv.ParseInt(fields[12], 10, 64)
				figures.faults += faults
				figures.user += time.Duration(user) * 10 * time.Millisecond
				figures.system += time.Duration(system) * 10 * time.Millisecond
			}
		}
		if status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			figures.resident += statusValue(string(status), "VmRSS:") << 10
		}
	}
	return figures
}

// statusValue is the first number of the line of a /proc status that starts with name, 0 where it
// holds none.
func statusValue(status, name string) int64 {
	for line := range strings.Lines(status) {
		if rest, found := strings.CutPrefix(line, name); found {
			if fields := strings.Fields(rest); len(fields) > 0 {
				value, _ := strconv.ParseInt(fields[0], 10, 64)
				return value
			}
		}
	}
	return 0
}

// since is what the processes did from earlier to f, and their resident memory at f.
func (f procFigures) since(earlier procFigures) procFigures {
	return procFigures{ran: f.ran - earlier.ran, waited: f.waited - earlier.waited, preempted: f.preempted - earlier.preempted,
		faults: f.faults - earlier.faults, user: f.user - earlier.user, system: f.system - earlier.system, resident: f.resident}
}

func (f procFigures) String() string {
	return fmt.Sprintf("ran %v (%v in user space, %v in the kernel, in 10 ms ticks), waited %v to run, lost a CPU %d times, %d minor faults",
		f.ran.Round(time.Microsecond), f.user, f.system, f.waited.Round(time.Microsecond), f.preempted, f.faults)
}

// agentFigures is the agent's runtime figures as its test hook writes them at a SIGUSR2
// (reportAgentFiguresOnSignal), in the order of agentFigureNames.
type agentFigures struct {
	cycles                       uint64
	scavengeAssist, scavengeBack float64
	collector                    float64
	released, total, live, goal  uint64
	limit, limiterLast           uint64
}

// agentFiguresLine is the line the agent writes at a SIGUSR2.
var agentFiguresLine = regexp.MustCompile(`agent: runtime figures ([0-9. ]+)\n`)

// agentRuntime asks agent pid for its runtime figures and answers them once the server has logged
// them.
func (s agentServer) agentRuntime(t *testing.T, pid int) agentFigures {
	t.Helper()
	from := len(s.Output.String())
	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		t.Fatalf("ask the agent for its runtime figures: %v", err)
	}
	var fields []string
	s.eventually(t, 30*time.Second, "the agent's runtime figures", func() bool {
		match := agentFiguresLine.FindStringSubmatch(s.Output.String()[from:])
		if match != nil {
			fields = strings.Fields(match[1])
		}
		return match != nil
	})
	if len(fields) != len(agentFigureNames) {
		t.Fatalf("the agent wrote %d runtime figures, want %d: %v", len(fields), len(agentFigureNames), fields)
	}
	whole := func(index int) uint64 { value, _ := strconv.ParseUint(fields[index], 10, 64); return value }
	seconds := func(index int) float64 { value, _ := strconv.ParseFloat(fields[index], 64); return value }
	return agentFigures{cycles: whole(0), scavengeAssist: seconds(1), scavengeBack: seconds(2), collector: seconds(3),
		released: whole(4), total: whole(5), live: whole(6), goal: whole(7), limit: whole(8), limiterLast: whole(9)}
}

// used is what the agent's runtime counts against its memory limit, as its claim gate does
// (going/agent's overMemory): all it has mapped less what it gave back to the system, as a share
// of the limit.
func (f agentFigures) used() float64 {
	return float64(f.total-f.released) / float64(f.limit)
}

// describeAgentRuntime writes what the agent's runtime did from before to after, for the test's
// log: its collections, the CPU its scavenger and its collector took, the heap it gave back to the
// system, and where it stood against its memory limit and its claim gate at each end.
func describeAgentRuntime(before, after agentFigures) string {
	limiter := "did not cap a collection in it"
	if after.limiterLast > before.cycles {
		limiter = fmt.Sprintf("last capped collection %d, within it", after.limiterLast)
	}
	return fmt.Sprintf("%d collections; the scavenger's CPU %.3fs assisting and %.3fs in the background, the collector's %.3fs; the GC CPU limiter %s; given back to the system %d -> %d MiB, mapped %d -> %d MiB, live %d MiB, heap goal %d MiB; against its limit of %d MiB it used %.0f%% -> %.0f%% (the claim gate at %.0f%%)",
		after.cycles-before.cycles, after.scavengeAssist-before.scavengeAssist, after.scavengeBack-before.scavengeBack, after.collector-before.collector,
		limiter, before.released>>20, after.released>>20, before.total>>20, after.total>>20, after.live>>20, after.goal>>20, after.limit>>20,
		100*before.used(), 100*after.used(), 100*loadClaimGate)
}

// kernelFacts is the kernel's release and build line, and the build's tick rate and preemption
// model and the system's transparent huge pages where this machine lets the test read them.
func kernelFacts() string {
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	version, _ := os.ReadFile("/proc/version")
	facts := []string{strings.TrimSpace(string(version))}
	if config, err := os.ReadFile("/boot/config-" + strings.TrimSpace(string(release))); err == nil {
		for line := range strings.Lines(string(config)) {
			if strings.HasPrefix(line, "CONFIG_HZ=") || strings.HasPrefix(line, "CONFIG_PREEMPT") {
				facts = append(facts, strings.TrimSpace(line))
			}
		}
	}
	if huge, err := os.ReadFile("/sys/kernel/mm/transparent_hugepage/enabled"); err == nil {
		facts = append(facts, "transparent huge pages "+strings.TrimSpace(string(huge)))
	}
	return strings.Join(facts, "; ")
}

// preemptLazy reports whether this kernel was built with the lazy preemption model
// (CONFIG_PREEMPT_LAZY=y in its /boot/config), under which a woken thread can wait for the next
// tick to take the CPU from a running one. GitHub's arm64 runners boot such a kernel (7.0); the
// GKE nodes production runs on boot the 6.12 line, which predates the model.
func preemptLazy() bool {
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	config, err := os.ReadFile("/boot/config-" + strings.TrimSpace(string(release)))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(config)) {
		if strings.TrimSpace(line) == "CONFIG_PREEMPT_LAZY=y" {
			return true
		}
	}
	return false
}

// processesNamed is the id of each process whose command is name, as /proc/<pid>/comm writes it:
// the database's processes where they run on this machine, none where they do not.
func processesNamed(name string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil && strings.TrimSpace(string(comm)) == name {
			pids = append(pids, pid)
		}
	}
	return pids
}

// serverGC is the server's collections as its profiler's /debug/vars tells them (expvar's
// memstats): how many it has made, and the end and the stop-the-world pause of each of its last
// 256.
type serverGC struct {
	NumGC    uint32
	PauseEnd [256]uint64
	PauseNs  [256]uint64
}

// readServerGC reads the server's collections from its profiler at profiler.
func readServerGC(t *testing.T, profiler string) serverGC {
	t.Helper()
	response, err := http.Get("http://" + profiler + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars on the server's profiler: %v", err)
	}
	defer response.Body.Close()
	var vars struct {
		Memstats serverGC `json:"memstats"`
	}
	if err := json.NewDecoder(response.Body).Decode(&vars); err != nil {
		t.Fatalf("decode the server's /debug/vars: %v", err)
	}
	return vars.Memstats
}

// cpuPause is one stop-the-world pause of the server's collector: when it ended and how long it was.
type cpuPause struct {
	end  time.Time
	took time.Duration
}

// since is the pause of each collection the server made after earlier, up to g.
func (g serverGC) since(earlier serverGC) []cpuPause {
	var pauses []cpuPause
	first := earlier.NumGC + 1
	if g.NumGC >= 256 {
		// The ring holds the last 256 collections alone.
		first = max(first, g.NumGC-255)
	}
	for number := first; number <= g.NumGC; number++ {
		at := (number + 255) % 256
		pauses = append(pauses, cpuPause{end: time.Unix(0, int64(g.PauseEnd[at])), took: time.Duration(g.PauseNs[at])})
	}
	return pauses
}

// cpuWindow is one window of timed reads and what went on in it: the reads, the server's
// collections, the /livez requests in flight, and what the server's, the test's, the database's
// and the agent's threads did, with the agent's resident memory at its start.
type cpuWindow struct {
	reads                         []cpuRead
	pauses                        []cpuPause
	livez                         []cpuRead
	server, test, database, agent procFigures
	databaseProcesses             int
	agentResidentBefore           int64
	agentBefore, agentAfter       agentFigures
}

// watchWindow times reads of path, at least count of them and over at least cpuWindowLeast
// (timedReads), and reads what went on around them, the agent's runtime figures included.
func (s agentServer) watchWindow(t *testing.T, profiler string, agentPid int, path string, count int) cpuWindow {
	t.Helper()
	database := processesNamed("postgres")
	agentRuntimeBefore := s.agentRuntime(t, agentPid)
	gcBefore := readServerGC(t, profiler)
	serverBefore, testBefore := readProcFigures(s.Cmd.Process.Pid), readProcFigures(os.Getpid())
	databaseBefore, agentBefore := readProcFigures(database...), readProcFigures(agentPid)
	reads := s.timedReads(t, path, count, cpuWindowLeast)
	window := cpuWindow{
		reads:               reads,
		server:              readProcFigures(s.Cmd.Process.Pid).since(serverBefore),
		test:                readProcFigures(os.Getpid()).since(testBefore),
		database:            readProcFigures(database...).since(databaseBefore),
		agent:               readProcFigures(agentPid).since(agentBefore),
		databaseProcesses:   len(database),
		agentResidentBefore: agentBefore.resident,
		agentBefore:         agentRuntimeBefore,
	}
	window.pauses = readServerGC(t, profiler).since(gcBefore)
	window.agentAfter = s.agentRuntime(t, agentPid)
	return window
}

// describe writes the window for the test's log: its reads, how many took above slow, its 8
// slowest - each by its start within the window, how long it took, and whether a pause of the
// server's collector ended or a /livez request ran within it - and what the processes did.
func (w cpuWindow) describe(slow time.Duration) string {
	start, end := w.span()
	slowest := slices.Clone(w.reads)
	slices.SortFunc(slowest, func(a, b cpuRead) int { return cmp.Compare(b.took, a.took) })
	above := 0
	for _, read := range w.reads {
		if read.took > slow {
			above++
		}
	}
	notes := make([]string, 0, 8)
	for _, read := range slowest[:min(8, len(slowest))] {
		note := fmt.Sprintf("+%v %v", read.at.Sub(start).Round(100*time.Microsecond), read.took.Round(10*time.Microsecond))
		finished := read.at.Add(read.took)
		for _, pause := range w.pauses {
			if !pause.end.Before(read.at) && !pause.end.After(finished) {
				note += fmt.Sprintf(" (a collector pause of %v ended in it)", pause.took)
			}
		}
		for _, livez := range w.livez {
			if livez.at.Before(finished) && livez.at.Add(livez.took).After(read.at) {
				note += " (/livez in flight)"
			}
		}
		notes = append(notes, note)
	}
	pauses := make([]time.Duration, len(w.pauses))
	for index, pause := range w.pauses {
		pauses[index] = pause.took
	}
	return fmt.Sprintf("%d reads in %v, %d of them above %v; the 8 slowest: %s; the server's collector paused %d times in it (%v); the server's threads %s; the test's %s; the database's %d processes %s; the agent's %s, its resident memory %d MiB at the start and %d MiB at the end; the agent's runtime around it: %s",
		len(w.reads), end.Sub(start).Round(time.Millisecond), above, slow, strings.Join(notes, ", "), len(w.pauses), pauses,
		w.server, w.test, w.databaseProcesses, w.database, w.agent, w.agentResidentBefore>>20, w.agent.resident>>20,
		describeAgentRuntime(w.agentBefore, w.agentAfter))
}

// span is when the window's first read started and its last ended.
func (w cpuWindow) span() (time.Time, time.Time) {
	last := w.reads[len(w.reads)-1]
	return w.reads[0].at, last.at.Add(last.took)
}

// databaseTimes is how long each of count round trips of `select 1` from the test to the server's
// database took, one after another: the part of a read the server's process does not run.
func (s agentServer) databaseTimes(t *testing.T, count int) []time.Duration {
	t.Helper()
	times := make([]time.Duration, 0, count)
	for range count {
		started := time.Now()
		var one int
		if err := s.database.Pool.QueryRow(t.Context(), `select 1`).Scan(&one); err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(started))
	}
	return times
}

// percentile is the share q of times at or under it, q from 0 to 1.
func percentile(times []time.Duration, q float64) time.Duration {
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	return sorted[max(0, int(math.Ceil(q*float64(len(sorted))))-1)]
}

// spread writes the p50, the p90 and the p99 of times, for the test's log.
func spread(times []time.Duration) string {
	return fmt.Sprintf("p50 %v, p90 %v, p99 %v", percentile(times, 0.5), percentile(times, 0.9), percentile(times, 0.99))
}

// policyNames names the scheduling policies a thread's stat gives by number (sched(7)).
var policyNames = map[int]string{0: "SCHED_OTHER", 1: "SCHED_FIFO", 2: "SCHED_RR", 3: "SCHED_BATCH", schedIdle: "SCHED_IDLE", 6: "SCHED_DEADLINE"}

// threadPolicies is how many threads process pid runs under each scheduling policy, each read from
// its /proc/<pid>/task/<tid>/stat, whose fields after the command's closing parenthesis start at
// the state, the third, so the policy, the forty-first, is the thirty-ninth of them.
func threadPolicies(t *testing.T, pid int) map[string]int {
	t.Helper()
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatalf("list the agent's threads: %v", err)
	}
	policies := map[string]int{}
	for _, task := range tasks {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/stat", pid, task.Name()))
		if err != nil {
			continue
		}
		text := string(stat)
		fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
		if len(fields) <= 38 {
			t.Fatalf("the agent's thread %s has a stat of %d fields, want the policy at the forty-first: %q", task.Name(), len(fields)+2, text)
		}
		policy, err := strconv.Atoi(fields[38])
		if err != nil {
			t.Fatalf("read the policy of the agent's thread %s from %q: %v", task.Name(), text, err)
		}
		name, known := policyNames[policy]
		if !known {
			name = strconv.Itoa(policy)
		}
		policies[name]++
	}
	return policies
}

// processVariable is the value of name in process pid's environment, "" where it holds none.
func processVariable(t *testing.T, pid int, name string) string {
	t.Helper()
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatalf("read process %d's environment: %v", pid, err)
	}
	for entry := range strings.SplitSeq(string(environ), "\x00") {
		if value, found := strings.CutPrefix(entry, name+"="); found {
			return value
		}
	}
	return ""
}

// cpuAgentShare is the share of the CPUs the agent schedules on in production: 4 (GOMAXPROCS) of
// a container's 16.
const cpuAgentShare = 4

// TestTheAgentLeavesTheServerItsCPU: the agent cannot take the server's CPU (check c29). The
// agent works at its soft limit, its garbage collection included (going/agent's test_soft_limit
// hook: allocation without pause on each CPU its runtime schedules on, beside a live heap the hook
// sizes so the memory its runtime counts against GOMEMLIMIT peaks at the limit); the test does not
// judge a window at whose start or end the agent used less than 90% or more than 110% of its
// limit, or in which its collector made no collection. Each window times the read at least 500
// times and for at least a second (cpuWindowLeast), long enough to hold a collection of the agent
// at its soft limit. Meanwhile the p99 of one fixed read of the server, an issue, over all the
// reads of the window stays under twice its p99 with the agent idle, and /livez, asked every
// 100 ms, answers each time in under 1 s. Every thread of the agent runs under the SCHED_IDLE
// policy, idle and at the load. The agent holds production's share of the runner's CPUs, a
// quarter, 1 at the least (DISPATCH_TEST_AGENT_PROCS), where production gives it 4 of a
// container's 16: on a runner of 4 CPUs the 4 a server gives its agent would be every one. The test
// logs the CPUs it runs on, the agent's GOMAXPROCS, and the spread of the read and of a round trip
// to the database, idle and at the load, which says what a slower p99 is made of; and of each
// window of reads, its 8 slowest reads by their time within it, each beside the server's collector
// pauses and the /livez requests it met, and what the server's, the test's, the database's and the
// agent's threads did in it (cpuWindow.describe).
//
// It skips on a kernel built with lazy preemption (preemptLazy). On GitHub's arm64 runners, whose
// 7.0 kernel is such a build, the read's p99 at the load was 2.0 to 2.6 times its idle p99 in five
// runs, the server's threads waiting 136 to 250 ms to run beside the agent at SCHED_IDLE against
// 9 to 10 ms beside it idle; on a GKE node of the 6.12 line production runs it was 1.00 and 1.06
// times (INFRA-1588, 2026-11-21). So check c29 runs it as a builds Job on a build-nosandbox node
// (cmd/dispatch/README.md, "c29").
func TestTheAgentLeavesTheServerItsCPU(t *testing.T) {
	if preemptLazy() {
		t.Skipf("this kernel preempts lazily (%s), where a woken thread waits for the next tick for the CPU a running one holds, whatever its policy; c29 runs on the GKE kernel line production runs", kernelFacts())
	}
	fake, model := newSoakModel(t)
	env := modelEnv(t, fake)
	env["DISPATCH_TEST_AGENT_GOMEMLIMIT"] = strconv.Itoa(cpuAgentLimit)
	procs := min(agentMaxProcs, max(1, runtime.GOMAXPROCS(0)/cpuAgentShare))
	env["DISPATCH_TEST_AGENT_PROCS"] = strconv.Itoa(procs)
	// The server's profiler, on a port the kernel chooses, tells its collections (readServerGC).
	env["DISPATCH_PPROF_PORT"] = "0"
	server := startAgentServer(t, env)
	server.waitForAgentRenewal(t)
	pid := server.agentStarted(t, 1, 30*time.Second)
	if got := processVariable(t, pid, "GOMAXPROCS"); got != strconv.Itoa(procs) {
		t.Errorf("the agent runs with GOMAXPROCS=%q, want %d, a quarter of the runner's %d CPUs", got, procs, runtime.GOMAXPROCS(0))
	}
	listening := pprofListening.FindStringSubmatch(server.Output.String())
	if listening == nil {
		t.Fatalf("the server logged no line naming its profiler's address:\n%s", server.Output.String())
	}
	profiler := listening[1]
	server.createProject(t, "CPU", "CPU")
	key, _ := server.createIssueIn(t, "CPU", "The issue the fixed read reads", "Context.\n")
	read := "/api/v1/issues/" + key
	server.timedReads(t, read, 100, 0)
	idleWindow := server.watchWindow(t, profiler, pid, read, cpuReads)
	idleDatabase := server.databaseTimes(t, cpuReads)
	idleThreads := threadPolicies(t, pid)

	const words = "work at the soft limit"
	model.set(words, func(n int) agenttest.Reply {
		if n == 1 {
			return toolCall("test_soft_limit", map[string]any{})
		}
		return answer("Done.")
	})
	run := server.begin(t, agentRun{login: dispatchTestLogin, words: words, level: "deep"})[0]
	server.waitForLog(t, 2*time.Minute, `msg="agent: test_soft_limit: the agent works at its soft limit"`)
	// The hook's heap is built once its line is written; the collector reaches the limit within
	// a few of its cycles.
	time.Sleep(5 * time.Second)

	var livez []cpuRead
	var livezFailures []string
	asking := make(chan struct{})
	var asked sync.WaitGroup
	asked.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-asking:
				return
			case <-ticker.C:
			}
			started := time.Now()
			response, err := http.Get(server.url("/livez"))
			took := time.Since(started)
			if err != nil {
				livezFailures = append(livezFailures, err.Error())
				continue
			}
			_ = response.Body.Close()
			if response.StatusCode/100 != 2 {
				livezFailures = append(livezFailures, response.Status)
			}
			livez = append(livez, cpuRead{at: started, took: took})
		}
	})
	busyWindow := server.watchWindow(t, profiler, pid, read, cpuReads)
	busyDatabase := server.databaseTimes(t, cpuReads)
	close(asking)
	asked.Wait()
	busyWindow.livez = livez
	busyThreads := threadPolicies(t, pid)
	server.send(t, http.MethodPost, "/api/v1/going/runs/"+run+"/stop", "", asUser(dispatchTestLogin))
	server.ended(t, run, time.Minute)

	idleReads, busyReads := tookOf(idleWindow.reads), tookOf(busyWindow.reads)
	idle, busy := percentile(idleReads, 0.99), percentile(busyReads, 0.99)
	slowest := slices.Max(append(tookOf(livez), 0))
	t.Logf("the test runs on %d CPUs (GOMAXPROCS %d) and the agent with GOMAXPROCS=%s; the agent's threads by scheduling policy, idle %v and at its soft limit %v; the kernel: %s",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), processVariable(t, pid, "GOMAXPROCS"), idleThreads, busyThreads, kernelFacts())
	t.Logf("the fixed read with the agent idle: %s; at its soft limit: %s (the p99 %.2f times; the idle p99 the %d-th slowest of %d reads, the p99 at the soft limit the %d-th slowest of %d); a round trip to the database idle: %s; at the soft limit: %s; /livez answered %d times, the slowest in %v",
		spread(idleReads), spread(busyReads), float64(busy)/float64(idle), p99Rank(len(idleReads)), len(idleReads), p99Rank(len(busyReads)), len(busyReads),
		spread(idleDatabase), spread(busyDatabase), len(livez), slowest)
	t.Logf("the window with the agent idle: %s", idleWindow.describe(2*idle))
	t.Logf("the window with the agent at its soft limit: %s", busyWindow.describe(2*idle))
	// What the agent's policy leaves the agent while the server works beside it: the CPU time its
	// threads had in the busy window against the window's length and the agent's GOMAXPROCS, which
	// says whether a run at the server's side would spend its time budget waiting for a CPU.
	busyStart, busyEnd := busyWindow.span()
	busyLength := busyEnd.Sub(busyStart)
	t.Logf("the agent's threads ran %v of the %v the window at its soft limit lasted (%.0f%% of its %d CPUs) and waited %v to run",
		busyWindow.agent.ran.Round(time.Millisecond), busyLength.Round(time.Millisecond), 100*float64(busyWindow.agent.ran)/float64(busyLength)/float64(procs),
		procs, busyWindow.agent.waited.Round(time.Millisecond))
	for _, threads := range []map[string]int{idleThreads, busyThreads} {
		for policy, count := range threads {
			if policy != policyNames[schedIdle] {
				t.Errorf("%d of the agent's threads run under %s, want every one under SCHED_IDLE (threads by policy %v)", count, policy, threads)
			}
		}
	}
	// The read's p99 says what an agent at its soft limit leaves the server only where the agent
	// was there: using its limit at each end of the window, its collector having run in it. A
	// window where it was not is no evidence either way, so the test stops before it judges one.
	for _, end := range []struct {
		name    string
		figures agentFigures
	}{{"start", busyWindow.agentBefore}, {"end", busyWindow.agentAfter}} {
		if used := end.figures.used(); used < cpuAtLimitLow || used > cpuAtLimitHigh {
			t.Fatalf("at the %s of the window the agent used %.0f%% of its memory limit, outside the %.0f%% to %.0f%% of an agent at its soft limit, so the window is not judged: %s",
				end.name, 100*used, 100*cpuAtLimitLow, 100*cpuAtLimitHigh, describeAgentRuntime(busyWindow.agentBefore, busyWindow.agentAfter))
		}
	}
	if busyWindow.agentAfter.cycles == busyWindow.agentBefore.cycles {
		t.Fatalf("the agent's collector made no collection in the window, so the agent was not at its soft limit and the window is not judged: %s",
			describeAgentRuntime(busyWindow.agentBefore, busyWindow.agentAfter))
	}
	if busy >= 2*idle {
		t.Errorf("with the agent at its soft limit the fixed read's p99 is %v, want under twice its %v with the agent idle", busy, idle)
	}
	if len(livezFailures) > 0 || len(livez) == 0 || slowest >= cpuLivez {
		t.Errorf("with the agent at its soft limit /livez answered %d times, the slowest in %v, and failed %d times (%v), want every answer under %v",
			len(livez), slowest, len(livezFailures), livezFailures, cpuLivez)
	}
}
