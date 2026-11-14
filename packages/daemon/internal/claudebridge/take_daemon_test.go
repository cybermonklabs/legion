package claudebridge_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudebridge"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The test across the daemon and a plugin that check c2 of INFRA-2811 names: the daemon's own claim
// routes (claims/register, claims/ready, claims/task-seen, claims/take, claims/turn-outcome) over a
// real claim machine and a real store, its task bucket the in-memory one, and the bridge's own take
// loop with the package's stand-in in place of Claude Code. Package api's own harness lives in its
// test files, which no other package can import, so this one builds its own api.Options.
//
// The test plays the two parts that are neither the daemon nor the bridge. As claude-envoy it
// registers the claim and says it is ready, as the pane's claim boot does, and hands the bridge each
// value the bucket holds as a task line of its inbox, as the relayed frame does. As the shim and the
// worker stream it forwards the bridge's agent_start and agent_end to the claim's machine.

const (
	crossProject = "legion"
	crossIssue   = "LEGION-2811"
	crossSession = "11111111-2222-4333-8444-555555555555"
	// crossTask is the task's text: the stand-in fails the turn whose user message begins
	// api-error-529 on the provider, as a 529 ends a turn (testdata/api-error-529.jsonl:5).
	crossTask = "api-error-529 WAITING on the provider"
	// ladderFirstWait is the provider-error ladder's first wait (supervise's promptRetryDelay(1)),
	// whose end writes the task's value again.
	ladderFirstWait = 30 * time.Second
)

// A turn that ends in a provider error keeps the task under its number, and the ladder's wait ends
// in exactly one new write of its value; the bridge, handed that value, takes it exactly once, and
// the turn that take starts is the task's: it runs the task's text behind the provider error's
// note, and its report finishes the task.
func TestTakeAcrossTheDaemonAndTheBridgeAfterAProviderErrorIsMadeOnce(t *testing.T) {
	d := startCrossDaemon(t)
	token, boot := d.launch()
	registered := d.register(boot)
	d.ready(token, registered)
	b := startCrossBridge(t, d.server.URL, boot, registered.Generation)
	m := d.machine(token)

	if err := m.Handle(d.ctx, supervise.RequestDeliver{Claim: token, Task: crossTask}); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	first := d.value(token)
	b.handTask(first)
	b.turn(t, d, m)
	d.answered(t, "/legion/v1/claims/turn-outcome", 1)

	stored := d.stored(token)
	if stored.Pending == nil || stored.Pending.Number != first.Number || stored.Pending.ProviderErrorRetries != 1 ||
		stored.ProviderErrorRun != 1 || stored.TaskFinished != 0 {
		t.Fatalf("after the provider error the claim records pending %+v, provider-error run %d, finished %d; want task %d kept for the ladder's first wait",
			stored.Pending, stored.ProviderErrorRun, stored.TaskFinished, first.Number)
	}
	writes, takes := d.writes(token), d.posted("/legion/v1/claims/take")

	if fired := d.clock.fire(ladderFirstWait); fired != 1 {
		t.Fatalf("%d timers of the ladder's first wait fired, want one", fired)
	}
	if written := d.writes(token) - writes; written != 1 {
		t.Fatalf("the end of the ladder's wait wrote the task's value %d times, want once", written)
	}
	again := d.value(token)
	if again.Number != first.Number || again.Revision <= first.Revision {
		t.Fatalf("the value written again is %+v, want task %d at a revision after %d", again, first.Number, first.Revision)
	}
	b.handTask(again)
	b.turn(t, d, m)
	d.answered(t, "/legion/v1/claims/turn-outcome", 2)

	if took := d.posted("/legion/v1/claims/take") - takes; took != 1 {
		t.Fatalf("the bridge took the task %d times after the ladder's wait, want once", took)
	}
	if written := d.writes(token) - writes; written != 1 {
		t.Fatalf("the task's value was written %d times after the provider error, want once", written)
	}
	texts := b.userTexts(t)
	if len(texts) != 2 || texts[0] != crossTask || !strings.HasSuffix(texts[1], "\n\n"+crossTask) ||
		!strings.HasPrefix(texts[1], "Your previous turn on this task stopped because of a provider error (HTTP 529).") {
		t.Fatalf("Claude Code read %q, want the task's text, then the same text behind the provider error's note", texts)
	}
	if finished := d.stored(token); finished.TaskFinished != first.Number || finished.Pending != nil {
		t.Fatalf("after the second turn the claim records finished %d, pending %+v; want task %d finished by the turn its take started",
			finished.TaskFinished, finished.Pending, first.Number)
	}
}

// crossDaemon is the daemon's claim routes over a real machine and store: the api server, served
// over HTTP for the bridge, counting each request it answered by route.
type crossDaemon struct {
	t       *testing.T
	ctx     context.Context
	store   *store.Store
	runtime *fake.Runtime
	values  *supervisetest.TaskValues
	clock   *ladderClock
	claims  *crossClaims
	server  *httptest.Server

	mu      sync.Mutex
	answers map[string]int
}

func startCrossDaemon(t *testing.T) *crossDaemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := testpg.CloneStore(t, "legion_claudebridge_test")
	tokens := api.NewBootTokens(st)
	rt := fake.NewRuntime()
	values := supervisetest.NewTaskValues()
	clock := &ladderClock{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	claims := &crossClaims{ctx: ctx, store: st, machines: map[claim.Token]*supervise.Machine{}, deps: supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: tokens.Recording(st), Specs: crossSpecs{}, Clock: clock, Log: quiet,
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
		Timeouts: supervise.Timeouts{
			Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 45 * time.Second, Stop: 10 * time.Second, LaunchBackoff: time.Millisecond,
		},
		PluginContract:     api.DaemonAPIVersion,
		PluginRelaunchable: true,
		RefreshGate:        supervise.NewRefreshGate(0),
		TaskValues:         values,
	}}
	routes := api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: claims, BootTokens: tokens, Project: crossProject, Controller: st, Log: quiet,
		Pool: st.Pool(), Record: record.NewStore(), Releaser: rt, Trees: st, ReleaseRecords: st, ClaimActivity: st,
		PluginRelaunchable: true,
	})
	d := &crossDaemon{t: t, ctx: ctx, store: st, runtime: rt, values: values, clock: clock, claims: claims, answers: map[string]int{}}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes.Handler.ServeHTTP(w, r)
		d.mu.Lock()
		d.answers[r.URL.Path]++
		d.mu.Unlock()
	}))
	t.Cleanup(d.server.Close)
	return d
}

// launch creates the implementer's claim and launches it, as the spawn route does, and answers its
// token and the boot token its launch carried.
func (d *crossDaemon) launch() (claim.Token, string) {
	d.t.Helper()
	token, err := claim.NewToken(crossProject, crossIssue, claim.RoleImplementer)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.store.OpenTreeLifecycle(d.ctx, crossProject, crossIssue, treelifecycle.AuthorityWorkflow); err != nil {
		d.t.Fatalf("open the tree's lifecycle: %v", err)
	}
	m, _, err := d.claims.Create(d.ctx, supervise.Claim{Token: token, Project: crossProject, Tree: crossIssue, Issue: crossIssue,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "role prompt")
	if err != nil {
		d.t.Fatalf("create the claim: %v", err)
	}
	if err := m.Handle(d.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		d.t.Fatalf("launch the claim: %v", err)
	}
	for _, call := range d.runtime.Calls() {
		if call.Method == "Spawn" && call.Spec.Claim == token {
			return token, call.Spec.BootToken
		}
	}
	d.t.Fatal("the claim's launch reached no runtime")
	return "", ""
}

// register is claude-envoy's registration of the pane's claim, through the route.
func (d *crossDaemon) register(boot string) claim.RegisterResponse {
	d.t.Helper()
	var registered claim.RegisterResponse
	d.post("/legion/v1/claims/register", claim.RegisterRequest{BootToken: boot, SessionID: crossSession,
		OmpSessionFile: "/claude/projects/-work/" + crossSession + ".jsonl", AgentID: crossSession,
		PluginContract: api.DaemonAPIVersion}, http.StatusOK, &registered)
	return registered
}

// ready is claude-envoy's ready, through the route.
func (d *crossDaemon) ready(token claim.Token, registered claim.RegisterResponse) {
	d.t.Helper()
	d.post("/legion/v1/claims/ready", claim.ReadyRequest{ClaimToken: token, SessionID: crossSession, Secret: registered.Secret,
		Generation: registered.Generation}, http.StatusNoContent, nil)
}

func (d *crossDaemon) post(path string, request any, status int, into any) {
	d.t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		d.t.Fatal(err)
	}
	response, err := http.Post(d.server.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		d.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		d.t.Fatalf("POST %s = %d %s, want %d", path, response.StatusCode, answer, status)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			d.t.Fatalf("decode the answer of %s %s: %v", path, answer, err)
		}
	}
}

func (d *crossDaemon) machine(token claim.Token) *supervise.Machine {
	d.t.Helper()
	m, ok := d.claims.Machine(token)
	if !ok {
		d.t.Fatalf("no machine for %s", token)
	}
	return m
}

// value is the value the bucket holds for the claim, which must hold one.
func (d *crossDaemon) value(token claim.Token) supervise.TaskValue {
	d.t.Helper()
	value, held := d.values.Value(token)
	if !held {
		d.t.Fatalf("the bucket holds no value for %s", token)
	}
	return value
}

// writes is how many times the claim's value has been written.
func (d *crossDaemon) writes(token claim.Token) int {
	n := 0
	for _, write := range d.values.Writes() {
		if write.Token == token {
			n++
		}
	}
	return n
}

// posted is how many requests the route at path has answered.
func (d *crossDaemon) posted(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answers[path]
}

// answered waits until the route at path has answered n requests.
func (d *crossDaemon) answered(t *testing.T, path string, n int) {
	t.Helper()
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool { return d.posted(path) >= n }) {
		t.Fatalf("%s answered %d requests within 10s, want %d", path, d.posted(path), n)
	}
}

func (d *crossDaemon) stored(token claim.Token) supervise.Claim {
	d.t.Helper()
	claims, err := d.store.Claims(d.ctx)
	if err != nil {
		d.t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	d.t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// crossClaims is the daemon's claim directory as the routes see it (api.Supervisor), over real
// machines and the real store.
type crossClaims struct {
	ctx      context.Context
	deps     supervise.Deps
	store    *store.Store
	mu       sync.Mutex
	machines map[claim.Token]*supervise.Machine
}

func (c *crossClaims) Machine(token claim.Token) (*supervise.Machine, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.machines[token]
	return m, ok
}

func (c *crossClaims) Create(ctx context.Context, cl supervise.Claim, _ string) (*supervise.Machine, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.machines[cl.Token]; ok {
		return m, false, nil
	}
	bound, err := c.deps.Store.AdmitClaim(ctx, cl)
	if err != nil {
		return nil, false, err
	}
	m, err := supervise.NewMachine(c.ctx, c.deps, bound)
	if err != nil {
		return nil, false, err
	}
	c.machines[cl.Token] = m
	return m, true, nil
}

func (c *crossClaims) Claims(ctx context.Context) ([]supervise.Claim, error) {
	return c.store.Claims(ctx)
}

func (c *crossClaims) Step(ctx context.Context, token claim.Token) (supervise.StepRecord, error) {
	return c.store.ClaimStep(ctx, token)
}

// crossSpecs is a launch the fake runtime accepts: the machine fills in who it is for.
type crossSpecs struct{}

func (crossSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// ladderClock is time that moves only when the test says: every timer a machine arms is kept, and
// fire runs those armed for one wait, as that wait's end would.
type ladderClock struct {
	mu     sync.Mutex
	timers []*ladderTimer
}

type ladderTimer struct {
	clock *ladderClock
	after time.Duration
	run   func()
	done  bool
}

func (c *ladderClock) Now() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

func (c *ladderClock) AfterFunc(after time.Duration, run func()) supervise.Cancel {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &ladderTimer{clock: c, after: after, run: run}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *ladderClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (t *ladderTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	stopped := !t.done
	t.done = true
	return stopped
}

// fire runs every timer armed for after that has neither run nor been stopped, and answers how
// many it ran.
func (c *ladderClock) fire(after time.Duration) int {
	c.mu.Lock()
	var due []*ladderTimer
	for _, timer := range c.timers {
		if !timer.done && timer.after == after {
			timer.done = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.run()
	}
	return len(due)
}

// crossBridge is the bridge of the claim's pane, run in this process over the package's stand-in
// for Claude Code, its frames read as the shim reads them.
type crossBridge struct {
	t      *testing.T
	record string
	frames chan map[string]any
}

func startCrossBridge(t *testing.T, daemonURL, boot string, generation uint64) *crossBridge {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", testtmp.Dir(t, claudebridge.InboxLongestPath))
	tokenFile := filepath.Join(dir, "boot-token")
	if err := os.WriteFile(tokenFile, []byte(boot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record")
	env := append(os.Environ(), claudebridge.FakeClaudeEnv+"=1", claudebridge.FakeClaudeRecordEnv+"="+record,
		"CLAUDE_CONFIG_DIR="+dir, "LEGION_ROLE=implementer", "LEGION_ISSUE="+crossIssue,
		"LEGION_DAEMON_URL="+daemonURL, "LEGION_BOOT_TOKEN_FILE="+tokenFile, "LEGION_GENERATION="+strconv.FormatUint(generation, 10))
	cfg, err := claudebridge.Configure([]string{os.Args[0], "-p", "--resume", crossSession}, env)
	if err != nil {
		t.Fatal(err)
	}
	stdin, shim := io.Pipe()
	frames, stdout := io.Pipe()
	cfg.Stdin, cfg.Stdout, cfg.Log = stdin, stdout, io.Discard
	b := &crossBridge{t: t, record: record, frames: make(chan map[string]any, 100)}
	go func() {
		scanner := bufio.NewScanner(frames)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &frame); err == nil {
				b.frames <- frame
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := claudebridge.Run(ctx, cfg); err != nil {
			t.Errorf("run the bridge: %v", err)
		}
		_ = stdout.Close()
	}()
	t.Cleanup(func() {
		cancel()
		_ = shim.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not end")
		}
	})
	return b
}

// turn forwards the bridge's next turn to the claim's machine: its agent_start, and then its
// agent_end, as the worker stream reports them.
func (b *crossBridge) turn(t *testing.T, d *crossDaemon, m *supervise.Machine) {
	t.Helper()
	token := m.Claim().Token
	for _, frame := range []struct {
		typ   string
		event supervise.Event
	}{{"agent_start", supervise.StreamTurnStart{Claim: token}}, {"agent_end", supervise.StreamTurnEnd{Claim: token}}} {
		b.until(t, frame.typ)
		if err := m.Handle(d.ctx, frame.event); err != nil {
			t.Fatalf("%s on %s: %v", frame.typ, token, err)
		}
	}
}

// until reads the bridge's frames up to and including the next of type typ.
func (b *crossBridge) until(t *testing.T, typ string) {
	t.Helper()
	for {
		select {
		case frame := <-b.frames:
			if frame["type"] == typ {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the bridge wrote no %s within 10s", typ)
		}
	}
}

// handTask hands the bridge the claim's value as claude-envoy writes it to the inbox: one task
// line, which the bridge answers once its loop has taken it.
func (b *crossBridge) handTask(value supervise.TaskValue) {
	b.t.Helper()
	var inboxPath string
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		raw, _ := os.ReadFile(b.record)
		for _, line := range strings.Split(string(raw), "\n") {
			if path, ok := strings.CutPrefix(line, "inbox "); ok && path != "" {
				inboxPath = path
				return true
			}
		}
		return false
	}) {
		b.t.Fatal("Claude Code was told no inbox within 10s")
	}
	conn, err := net.Dial("unix", inboxPath)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	id := "task-" + strconv.FormatUint(value.Revision, 10)
	if err := json.NewEncoder(conn).Encode(claudebridge.InboxDelivery{ID: id, Kind: claudebridge.InboxTask,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}); err != nil {
		b.t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		b.t.Fatalf("the inbox answered the task line %q, %v", line, err)
	}
	var answer claudebridge.InboxAnswer
	if err := json.Unmarshal(line, &answer); err != nil || answer != (claudebridge.InboxAnswer{ID: id, OK: true}) {
		b.t.Fatalf("the inbox answered the task line %s", line)
	}
}

// userTexts is the text of every user message Claude Code read, in the order it read them.
func (b *crossBridge) userTexts(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(b.record)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(string(raw), "\n") {
		body, ok := strings.CutPrefix(line, "in ")
		if !ok {
			continue
		}
		var input struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(body), &input) == nil && len(input.Message.Content) > 0 {
			texts = append(texts, input.Message.Content[0].Text)
		}
	}
	return texts
}
