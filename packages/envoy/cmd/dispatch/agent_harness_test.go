package main

// agent_harness_test.go holds what the tests of the server's agent process share: a real
// envoy-dispatch server on a database of its own, which starts its own envoy-dispatch agent, the
// fake chat model over https it may call (going/agent/agenttest), and the reads of its runs, its
// log and its /metrics. It carries no build tag, since the untagged, memory, twobuild and
// multiprocess builds all start agents through it; the tests of the agent's loop, its supervision,
// its soak and its bounds (agent_loop_test.go, agent_supervisor_test.go, agent_soak_test.go) carry
// the multiprocess tag and run in CI's dispatch-multiprocess job.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// serveAgentHellos listens for an agent at path as a server's end of agent/v1 does, answers each
// hello and sends the secret it carried on the channel it returns; every other call it holds
// until the agent closes its connection, as a claim and a renew wait at a server.
func serveAgentHellos(t *testing.T, path string) <-chan []byte {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen for the agent at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	hellos := make(chan []byte, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				payload, err := agentwire.ReadFrame(conn, agentwire.MaxRequestLimit)
				var request agentwire.Request
				if err != nil || json.Unmarshal(payload, &request) != nil {
					return
				}
				if request.Call != agentwire.CallHello {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				select {
				case hellos <- request.Secret:
				default:
				}
				answer, _ := json.Marshal(agentwire.Answer{Body: json.RawMessage(`{"freed":0}`)})
				_ = agentwire.WriteFrame(conn, answer, agentwire.AnswerLimit(agentwire.CallHello))
			}()
		}
	}()
	return hellos
}

// The shapes of a test run: one this build reads (going_runs.shape, api's goingRunShape), and one
// above it, which no claim of this build takes, so a test holds a run back from the agent.
const (
	readShape   = 1
	unreadShape = 99
)

// agentServer is a Dispatch process a test started, which runs its agent process, and the
// database it serves.
type agentServer struct {
	*dispatchProcess
	database *store.Store
}

// startAgentServer starts Dispatch with env on a database of its own, in a process group of its
// own, so a SIGSTOP of its agent reaches neither the test's group nor a group the kernel hangs
// up, and returns once it is healthy.
func startAgentServer(t *testing.T, env map[string]string) agentServer {
	t.Helper()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: env, OwnProcessGroup: true})
	process.waitHealthy(t)
	return agentServer{dispatchProcess: process, database: database}
}

// agentStartedLine is the line the server writes each time it starts an agent, with its pid.
var agentStartedLine = regexp.MustCompile(`msg="dispatch: started the agent process" pid=(\d+)`)

// agentPids is the pid of each agent the server started, in order.
func (s agentServer) agentPids() []int {
	var pids []int
	for _, match := range agentStartedLine.FindAllStringSubmatch(s.Output.String(), -1) {
		pid, err := strconv.Atoi(match[1])
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// agentStarted waits up to within for the server's count-th agent to start, and answers its pid.
func (s agentServer) agentStarted(t *testing.T, count int, within time.Duration) int {
	t.Helper()
	var pid int
	s.eventually(t, within, fmt.Sprintf("the server's agent number %d to start", count), func() bool {
		pids := s.agentPids()
		if len(pids) < count {
			return false
		}
		pid = pids[count-1]
		return true
	})
	return pid
}

// eventually waits up to within for done, failing the test with the server's output when it is
// not, or when the server exits first.
func (s agentServer) eventually(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		select {
		case <-s.Exited:
			t.Fatalf("the server exited while the test waited for %s:\n%s", what, s.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v:\n%s", what, within, s.Output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// scrape is the server's /metrics, each whole-number sample by its name and labels as the text
// format writes them.
func (s agentServer) scrape(t *testing.T) map[string]int64 {
	t.Helper()
	response, err := http.Get(s.url("/metrics"))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: %d %v", response.StatusCode, err)
	}
	samples := map[string]int64{}
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		at := strings.LastIndexByte(line, ' ')
		if value, err := strconv.ParseInt(line[at+1:], 10, 64); at > 0 && err == nil {
			samples[line[:at]] = value
		}
	}
	return samples
}

// The /metrics samples of the agent process.
const (
	agentGoroutines  = `envoy_dispatch_process_goroutines{process="agent"}`
	agentMemoryKills = `envoy_dispatch_agent_memory_kills`
	agentSlotsSample = `envoy_dispatch_agent_slots`
)

// waitForAgentRenewal waits for the running agent's first renew, which carries its health, so
// its claim waits at the server too.
func (s agentServer) waitForAgentRenewal(t *testing.T) {
	t.Helper()
	s.eventually(t, 30*time.Second, "the agent's first renew", func() bool {
		_, renewed := s.scrape(t)[agentGoroutines]
		return renewed
	})
}

// requireServing fails the test unless the server still runs and answers /healthz 200.
func (s agentServer) requireServing(t *testing.T) {
	t.Helper()
	if err := s.healthyWithin(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func (s agentServer) exec(t *testing.T, statement string, arguments ...any) {
	t.Helper()
	if _, err := s.database.Pool.Exec(context.Background(), statement, arguments...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// recordRun records a run of login's in state, of shape, as a run's start records one, and
// answers its id; a working run is held under a lease of another server's agent for an hour.
func (s agentServer) recordRun(t *testing.T, login, state string, shape int) string {
	t.Helper()
	id := uuid.NewString()
	s.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at,
		  lease_token, lease_owner, lease_ends_at)
		select $1::uuid, $2, 'the words of a test run', $1::uuid, 'standard', 'ask', $3, $4, clock_timestamp(),
		  case when $3 = 'working' then gen_random_uuid() end,
		  case when $3 = 'working' then 'an agent of another server' end,
		  case when $3 = 'working' then now() + interval '1 hour' end
	`, id, login, state, shape)
	return id
}

// runRow is what a test reads of a run.
type runRow struct {
	state, reason, owner string
	kills                int
}

func (s agentServer) run(t *testing.T, id string) runRow {
	t.Helper()
	var row runRow
	if err := s.database.Pool.QueryRow(context.Background(), `
		select state, coalesce(stop_reason, ''), coalesce(lease_owner, ''), kills from going_runs where id = $1::uuid
	`, id).Scan(&row.state, &row.reason, &row.owner, &row.kills); err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return row
}

// stopHolding stops agent pid with SIGSTOP while its claim waits at the server, then makes run,
// recorded at unreadShape, claimable, so the server leases it to the stopped agent, which can do
// nothing with it; it answers the run's lease owner and leaves the agent stopped. A stop that
// lands between two of the agent's claims leases nothing: the run is held back again, the agent
// continued, and the stop tried again.
func (s agentServer) stopHolding(t *testing.T, pid int, run string) string {
	t.Helper()
	for range 5 {
		time.Sleep(200 * time.Millisecond)
		signalProcess(t, pid, syscall.SIGSTOP)
		s.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, readShape)
		s.exec(t, `select pg_notify($1, 'ready')`, api.GoingRunsChannel)
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if row := s.run(t, run); row.state == "working" {
				return row.owner
			}
		}
		s.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, unreadShape)
		if row := s.run(t, run); row.state == "working" {
			return row.owner
		}
		signalProcess(t, pid, syscall.SIGCONT)
	}
	t.Fatalf("the server never leased run %s to the stopped agent %d:\n%s", run, pid, s.Output.String())
	return ""
}

// signalProcess sends sig to process pid. A process the test stops is killed when the test ends
// if it is still stopped then, so no stopped agent outlives its test.
func signalProcess(t *testing.T, pid int, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(pid, sig); err != nil {
		t.Fatalf("send %v to process %d: %v", sig, pid, err)
	}
	if sig == syscall.SIGSTOP {
		t.Cleanup(func() {
			if processState(pid) == 'T' {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
	}
}

// processState is the state /proc/<pid>/stat gives process pid ('R', 'S', 'T', 'Z' and so on),
// 0 where the process is gone.
func processState(pid int) byte {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The command name in parentheses may hold anything, ") " among it: the state follows the
	// last closing parenthesis.
	at := strings.LastIndexByte(string(stat), ')')
	if at < 0 || at+2 >= len(stat) {
		return 0
	}
	return stat[at+2]
}

// processGone reports whether process pid has exited: its /proc entry is gone, or it is a zombie
// its parent has not reaped yet.
func processGone(pid int) bool {
	switch processState(pid) {
	case 0, 'Z', 'X':
		return true
	}
	return false
}

// logTime is the time= attribute of a server's log line.
func logTime(t *testing.T, line string) time.Time {
	t.Helper()
	match := regexp.MustCompile(`time=(\S+)`).FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("the line carries no time: %s", line)
	}
	at, err := time.Parse(time.RFC3339Nano, match[1])
	if err != nil {
		t.Fatalf("the line's time %q: %v", match[1], err)
	}
	return at
}

// killedMemory is the memory= attribute of an "agent killed for memory" line.
func killedMemory(t *testing.T, line string) int64 {
	t.Helper()
	match := regexp.MustCompile(` memory=(\d+) `).FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("the kill names no memory: %s", line)
	}
	memory, _ := strconv.ParseInt(match[1], 10, 64)
	return memory
}

// modelScript is what a fake chat model answers, by the person's words of the run that asks: each
// row's reply to the n-th request of its run, from 1.
type modelScript struct {
	mu   sync.Mutex
	rows map[string]func(n int) agenttest.Reply
}

// set scripts the replies to the run whose person's words are words.
func (s *modelScript) set(words string, reply func(n int) agenttest.Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[words] = reply
}

// newFakeModel is a fake chat model over https that answers as its script says.
func newFakeModel(t *testing.T) (*agenttest.Model, *modelScript) {
	t.Helper()
	script := &modelScript{rows: map[string]func(int) agenttest.Reply{}}
	var fake *agenttest.Model
	fake = agenttest.NewTLS(t, func(request *agenttest.Request) agenttest.Reply {
		script.mu.Lock()
		defer script.mu.Unlock()
		for words, reply := range script.rows {
			if request.Asks(words) {
				return reply(len(fake.Asking(words)))
			}
		}
		return agenttest.Reply{Text: "No row of this test asks that."}
	})
	return fake, script
}

// modelEnv is the environment of a server whose agent calls fake, with the agent's test hooks on.
func modelEnv(t *testing.T, fake *agenttest.Model) map[string]string {
	t.Helper()
	return map[string]string{
		"DISPATCH_ANSWERER":         "endpoint",
		"DISPATCH_CHAT_ENDPOINT":    fake.URL(),
		"DISPATCH_CHAT_MODEL":       agenttest.ModelID,
		"DISPATCH_CHAT_TOKEN_FILE":  agenttest.TokenFile(t),
		"SSL_CERT_FILE":             fake.CertFile(t),
		"DISPATCH_TEST_AGENT_HOOKS": "1",
	}
}

// modelServer is a server whose agent calls a fake chat model, and the script the fake answers by.
type modelServer struct {
	agentServer
	fake   *agenttest.Model
	script *modelScript
}

// startModelServer starts Dispatch with env and the fake model's settings on a database of its own.
func startModelServer(t *testing.T, env map[string]string) modelServer {
	t.Helper()
	fake, script := newFakeModel(t)
	merged := modelEnv(t, fake)
	maps.Copy(merged, env)
	return modelServer{agentServer: startAgentServer(t, merged), fake: fake, script: script}
}

// toolCall is the reply that calls tool with arguments.
func toolCall(tool string, arguments map[string]any) agenttest.Reply {
	return agenttest.Reply{Calls: []agenttest.Call{{Name: tool, Arguments: arguments}}}
}

// readCall is the reply that reads with tool of Dispatch, by call_tool, with arguments.
func readCall(tool string, arguments map[string]any) agenttest.Reply {
	return toolCall("call_tool", map[string]any{"tool": tool, "arguments": arguments})
}

// answer is the reply that answers text and calls nothing.
func answer(text string) agenttest.Reply { return agenttest.Reply{Text: text} }

// agentGrant is the credential a run acts with: the header identity of a test server where kind is
// empty, a sign-in's generation and expiry, or a personal token's agent_tokens row (kind token).
type agentGrant struct {
	kind       string
	generation int64
	expires    time.Time
	token      string
}

// agentRun is a run a row starts, as the route that starts a run records one: login's words at
// level, with what it already worked, under grant.
type agentRun struct {
	login, words, level string
	workingMS           int64
	grant               agentGrant
}

// begin records runs queued with their grants, in one transaction that announces them, so every
// agent's claim finds them at once; it answers their ids.
func (s agentServer) begin(t *testing.T, runs ...agentRun) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		id := uuid.NewString()
		if run.level == "" {
			run.level = "standard"
		}
		if _, err := tx.Exec(ctx, `
			insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at, budget_used)
			values ($1::uuid, $2, $3, $1::uuid, $4, 'ask', 'queued', $5, clock_timestamp(),
			  jsonb_build_object('calls', 0, 'working_ms', $6::bigint, 'tokens', 0))
		`, id, run.login, run.words, run.level, readShape, run.workingMS); err != nil {
			t.Fatalf("record run %q: %v", run.words, err)
		}
		grant := run.grant
		if grant.kind == "" {
			grant.kind = "header"
		}
		var generation *int64
		var expires *time.Time
		var token *string
		switch grant.kind {
		case "sign_in":
			generation, expires = &grant.generation, &grant.expires
		case "token":
			token = &grant.token
		}
		if _, err := tx.Exec(ctx, `
			insert into going_run_grants (run_id, kind, login, generation, expires_at, token_id) values ($1::uuid, $2, $3, $4, $5, $6::uuid)
		`, id, grant.kind, run.login, generation, expires, token); err != nil {
			t.Fatalf("grant run %q: %v", run.words, err)
		}
		ids = append(ids, id)
	}
	if _, err := tx.Exec(ctx, `select pg_notify($1, 'ready')`, api.GoingRunsChannel); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

// ended waits up to within for run to leave queued and working, and answers its row.
func (s agentServer) ended(t *testing.T, run string, within time.Duration) runRow {
	t.Helper()
	var row runRow
	s.eventually(t, within, "run "+run+" to end", func() bool {
		row = s.run(t, run)
		return row.state != "queued" && row.state != "working"
	})
	return row
}

// toolResults is what the model was told of each tool call of run that saved its step, in order.
func (s agentServer) toolResults(t *testing.T, run string) []string {
	t.Helper()
	rows, err := s.database.Pool.Query(context.Background(), `
		select body->>'result' from going_run_steps where run_id = $1::uuid and kind = $2 order by step
	`, run, agentwire.StepTool)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	return results
}

// steps is how many steps run saved.
func (s agentServer) steps(t *testing.T, run string) int {
	t.Helper()
	var steps int
	if err := s.database.Pool.QueryRow(context.Background(), `select count(*) from going_run_steps where run_id = $1::uuid`, run).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	return steps
}

// endFrame is the end frame of run's events.
func (s agentServer) endFrame(t *testing.T, run string) agentwire.EndFrame {
	t.Helper()
	var data []byte
	if err := s.database.Pool.QueryRow(context.Background(), `
		select data from going_run_events where run_id = $1::uuid and name = $2 order by sequence desc limit 1
	`, run, agentwire.FrameEnd).Scan(&data); err != nil {
		t.Fatalf("read run %s's end frame: %v", run, err)
	}
	var frame agentwire.EndFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

// runs is how many runs are in state.
func (s agentServer) runs(t *testing.T, state string) int {
	t.Helper()
	var count int
	if err := s.database.Pool.QueryRow(context.Background(), `select count(*) from going_runs where state = $1`, state).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// stop stops run as its person's Stop does: it is set stopped and its lease freed, and the stop is
// announced, in one transaction.
func (s agentServer) stop(t *testing.T, run string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		update going_runs set state = 'stopped', stop_reason = 'STOPPED: its person stopped it', lease_token = null, lease_owner = null,
		  lease_ends_at = null, finished_at = now()
		where id = $1::uuid
	`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select pg_notify($1, 'stop')`, api.GoingRunsChannel); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// hookTimes is when each call the agent-call test hook held entered it (what "holds") or left it
// (what "released"), in order.
func (s agentServer) hookTimes(t *testing.T, what string) []time.Time {
	t.Helper()
	var times []time.Time
	for _, line := range s.logLines(`msg="dispatch: a test hook ` + what + ` a call of the agent"`) {
		times = append(times, logTime(t, line))
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	return times
}

// soakModel is the fake chat model's script: each run's replies by its person's words, and how
// many requests each run has made. A request is answered with one lookup of its words, where
// modelScript's scan of every earlier request would grow with the soak.
type soakModel struct {
	mu      sync.Mutex
	replies map[string]func(n int) agenttest.Reply
	asked   map[string]int
}

// newSoakModel is a fake chat model over https that answers as its script says.
func newSoakModel(t *testing.T) (*agenttest.Model, *soakModel) {
	t.Helper()
	script := &soakModel{replies: map[string]func(int) agenttest.Reply{}, asked: map[string]int{}}
	fake := agenttest.NewTLS(t, func(request *agenttest.Request) agenttest.Reply {
		words := askedWords(request.Text())
		script.mu.Lock()
		reply, scripted := script.replies[words]
		script.asked[words]++
		n := script.asked[words]
		script.mu.Unlock()
		if !scripted {
			return agenttest.Reply{Text: "No run of the soak asks that."}
		}
		return reply(n)
	})
	return fake, script
}

// askedWords is the words of the run a request is of: the line after the last agenttest.Asked of
// the request's text, which is the turn the run works now.
func askedWords(text string) string {
	at := strings.LastIndex(text, agenttest.Asked)
	if at < 0 {
		return ""
	}
	words, _, _ := strings.Cut(text[at+len(agenttest.Asked):], "\n")
	return words
}

// set scripts the replies to the run whose words are words.
func (m *soakModel) set(words string, reply func(n int) agenttest.Reply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies[words] = reply
}

// requests is how many requests the run whose words are words has made.
func (m *soakModel) requests(words string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asked[words]
}

// runRows is the row of each of runs, by id.
func (s agentServer) runRows(t *testing.T, runs []string) map[string]runRow {
	t.Helper()
	rows, err := s.database.Pool.Query(t.Context(), `
		select id::text, state, coalesce(stop_reason, ''), coalesce(lease_owner, ''), kills from going_runs where id = any($1::uuid[])
	`, runs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	read := make(map[string]runRow, len(runs))
	for rows.Next() {
		var id string
		var row runRow
		if err := rows.Scan(&id, &row.state, &row.reason, &row.owner, &row.kills); err != nil {
			t.Fatal(err)
		}
		read[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return read
}
