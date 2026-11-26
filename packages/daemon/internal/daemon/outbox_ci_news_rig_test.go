package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The entries the readers of the whole-daemon CI news tests were given, and the number of the
// issue's pull request.
const (
	ciReviewerEntry = 4
	ciFixEntry      = 5
	ciMergerEntry   = 6
	ciPullRequest   = 42
)

// ciDaemonWait is how long a wait of the whole-daemon CI news tests gives the daemon: a delivery
// row's retries back off on the wall clock, doubling from a second, so a row that waited through a
// launch tries again within a minute or two.
const ciDaemonWait = 3 * time.Minute

// ciPublish is one publish the CI news listener took: its topic, its message and the key it was
// published under (notify's dedupe key).
type ciPublish struct {
	topic, message, key string
}

// ciGate holds the next publish to its topic: before the listener takes it (the publish is never
// taken) or after (the publish is taken and its answer withheld), until the test lets it go.
type ciGate struct {
	afterTake bool
	reached   chan struct{}
	release   chan struct{}
	once      sync.Once
}

// ciListener is Envoy's listener as the whole-daemon CI news tests' daemon reaches it. A publish to
// a role topic no session holds is answered 404 holder_lapsed with the holder gone, which notify
// reads as notify.ErrNoHolder (the production listener's answer for a released role's topic since
// cybermonklabs/legion#4088), and counted. A publish to any other topic, or to a role topic a test
// said a session holds (hold), is taken and kept with its key; JetStream keeps one message for
// each key in its dedupe window, so delivered counts a key once. A test may hold the next publish
// to a topic at a gate (gate). The clean session stop is answered as a listener with no record of
// the session answers it.
type ciListener struct {
	url     string
	mu      sync.Mutex
	held    map[string]bool
	refused map[string]int
	taken   []ciPublish
	gates   map[string]*ciGate
}

func newCIListener(t *testing.T) *ciListener {
	t.Helper()
	l := &ciListener{held: map[string]bool{}, refused: map[string]int{}, gates: map[string]*ciGate{}}
	server := httptest.NewServer(http.HandlerFunc(l.serve))
	t.Cleanup(server.Close)
	// Registered after the server's close, so it runs first: a gate a failed test left shut lets
	// its publish go before the close waits for it.
	t.Cleanup(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, gate := range l.gates {
			gate.once.Do(func() { close(gate.release) })
		}
	})
	l.url = server.URL
	return l
}

func (l *ciListener) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
		var body struct {
			Topic     string `json:"topic"`
			Message   string `json:"message"`
			DedupeKey string `json:"dedupe_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad body"}`)
			return
		}
		l.mu.Lock()
		if strings.HasPrefix(body.Topic, notify.RoleTopicPrefix) && !l.held[body.Topic] {
			l.refused[body.Topic]++
			l.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"the role's holder is not live","reason":"holder_lapsed","holder":"a-released-session","holder_state":"gone"}`)
			return
		}
		gate := l.gates[body.Topic]
		delete(l.gates, body.Topic)
		published := ciPublish{topic: body.Topic, message: body.Message, key: body.DedupeKey}
		if gate == nil || gate.afterTake {
			l.taken = append(l.taken, published)
		}
		l.mu.Unlock()
		if gate != nil {
			close(gate.reached)
			<-gate.release
			if !gate.afterTake {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"the listener took nothing"}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		_, _ = io.WriteString(w, `{"deleted":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}
}

// hold is token's session holding its role topic, as a role's process does once it is up.
func (l *ciListener) hold(token claim.Token) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[notify.RoleTopicPrefix+string(token)] = true
}

// unhold is token's session gone from its role topic, as a release leaves it.
func (l *ciListener) unhold(token claim.Token) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, notify.RoleTopicPrefix+string(token))
}

// gate holds the next publish to token's role topic: after the listener takes it where afterTake,
// before it otherwise. It answers the gate, whose reached channel closes once the publish has come.
func (l *ciListener) gate(token claim.Token, afterTake bool) *ciGate {
	l.mu.Lock()
	defer l.mu.Unlock()
	gate := &ciGate{afterTake: afterTake, reached: make(chan struct{}), release: make(chan struct{})}
	l.gates[notify.RoleTopicPrefix+string(token)] = gate
	return gate
}

// let lets the publish the gate holds go: taken, it is answered as taken; not taken, it is answered
// 503 and nothing of it is kept.
func (g *ciGate) let() { g.once.Do(func() { close(g.release) }) }

// refusedTo is how many publishes to token's role topic were refused for want of a holder.
func (l *ciListener) refusedTo(token claim.Token) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refused[notify.RoleTopicPrefix+string(token)]
}

// takenTo is every publish to token's role topic the listener took, under key where key is not "",
// a repeat of a key included, oldest first.
func (l *ciListener) takenTo(token claim.Token, key string) []ciPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	var to []ciPublish
	for _, published := range l.taken {
		if published.topic == notify.RoleTopicPrefix+string(token) && (key == "" || published.key == key) {
			to = append(to, published)
		}
	}
	return to
}

// delivered is how many messages the publishes to token's role topic under key left on the stream:
// one for each key, as the stream's dedupe window keeps them.
func (l *ciListener) delivered(token claim.Token, key string) int {
	var keys []string
	for _, published := range l.takenTo(token, key) {
		if !slices.Contains(keys, published.key) {
			keys = append(keys, published.key)
		}
	}
	return len(keys)
}

// ciDaemon is the whole daemon of the CI news tests: the pause's rig (newPauseDaemonRig), booted
// through startDaemon on a database of its own with the fake runtime, PostgreSQL and NATS, whose
// Envoy listener is ciListener, and which hands the test its outbox before the outbox's lanes run
// (overrides.outbox), so a test wakes a lane once it has queued a row as the workflow's own commit
// would. Its one issue is the rig's first, on liveRepo, with an open pull request, number
// ciPullRequest, at a head the test names.
type ciDaemon struct {
	*pauseDaemonRig
	listener *ciListener
	key      string

	runnerMu sync.Mutex
	runner   *outbox
}

// newCIDaemon readies the rig; nothing is seeded or booted yet.
func newCIDaemon(t *testing.T) *ciDaemon {
	t.Helper()
	listener := newCIListener(t)
	r := newPauseDaemonRig(t, func(cfg *config.Config) { cfg.EnvoyURL = listener.url })
	d := &ciDaemon{pauseDaemonRig: r, listener: listener, key: r.issue(1)}
	r.o.outbox = func(o *outbox) {
		d.runnerMu.Lock()
		defer d.runnerMu.Unlock()
		d.runner = o
	}
	return d
}

// seedIssue records the issue standing in at, at generation, as an admitted tree with claims, and
// its open pull request at head.
func (d *ciDaemon) seedIssue(at phase.Phase, generation uint64, head string, claims ...supervise.Claim) {
	d.t.Helper()
	root := d.root(d.key, at)
	root.Generation = generation
	d.seed(root, claims...)
	d.pullRequest(d.key, ciPullRequest, head)
}

// releasedReader is role's claim of the issue as an idle release left it, serving generation and
// given the work of entry.
func (d *ciDaemon) releasedReader(role claim.Role, generation uint64, entry int64) supervise.Claim {
	c := d.released(d.key, role)
	c.ServingGeneration, c.GivenEntry = generation, entry
	return c
}

// launchingReader is role's claim of the issue as a daemon stopped mid-launch left it, given the
// work of entry: the boot launches it again, and it is no released claim to wake.
func (d *ciDaemon) launchingReader(role claim.Role, entry int64) supervise.Claim {
	c := d.unfinished(d.key, role)
	c.GivenEntry = entry
	return c
}

// entry writes role's phases row of the issue at entry.
func (d *ciDaemon) entry(role claim.Role, entry int64) {
	d.t.Helper()
	token := d.token(d.key, role)
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		return d.records.PutPhase(ctx, tx, record.PhaseRow{Issue: d.key, Role: role, Claim: token, Entry: entry,
			EntryTask: "work", EntryRaisedAt: time.Now().UTC()})
	}); err != nil {
		d.t.Fatalf("write the %s's phases row: %v", role, err)
	}
}

// runningFix records fix 1 of the task default running at the implementer's entry entry, at head.
func (d *ciDaemon) runningFix(entry int64, head string) {
	d.t.Helper()
	d.entry(claim.RoleImplementer, entry)
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		fix := record.TaskFix{Issue: d.key, Generation: 1, Task: "default", Fix: 1, State: record.FixReading, Check: "k-ci", Head: head, Place: 1}
		if err := d.records.PutFix(ctx, tx, fix); err != nil {
			return err
		}
		fix.State, fix.Entry = record.FixRunning, entry
		return d.records.UpdateFix(ctx, tx, fix)
	}); err != nil {
		d.t.Fatalf("record the running fix: %v", err)
	}
}

// prompt is a prompt to read Dispatch for role's reader of the task default at generation and
// head, replacing the head's set of checks with checks.
func (d *ciDaemon) prompt(role claim.Role, generation uint64, head string, reader record.CIReader, checks ...string) record.CINews {
	set := checks
	return record.CINews{Purpose: record.CINewsState, Role: string(d.token(d.key, role)), Generation: generation, Task: "default",
		Repo: liveRepo, PullRequest: ciPullRequest, Head: head, Reader: &reader, Checks: &set, Body: record.ReadPromptBody(d.key, head, checks)}
}

// report is a report of a run of the listener's settlement for role's reader at head.
func (d *ciDaemon) report(role claim.Role, head string, reader record.CIReader) record.CINews {
	return record.CINews{Purpose: record.CINewsRuns, Role: string(d.token(d.key, role)), Generation: 1, Task: "default",
		Repo: liveRepo, PullRequest: ciPullRequest, Head: head, Reader: &reader,
		Runs: []record.CIRun{{Name: "unit", ID: 9, Head: head, Conclusion: "failure"}}, Body: "Pull request #42: CI run unit failed."}
}

// lanes is the daemon's outbox, as the boot handed it over.
func (d *ciDaemon) lanes() *outbox {
	d.runnerMu.Lock()
	defer d.runnerMu.Unlock()
	return d.runner
}

// wake wakes the lane that runs kind for the issue, as the commit of a row's write through the
// workflow's store does (projectRecords).
func (d *ciDaemon) wake(kind record.OutboxKind) {
	if o := d.lanes(); o != nil {
		o.wake(d.key, kind)
	}
}

// queue queues payload for the issue, due now, wakes its lane, and answers the row's id.
func (d *ciDaemon) queue(payload record.OutboxPayload) int64 {
	d.t.Helper()
	id := d.enqueue(d.key, payload)
	d.wake(payload.OutboxKind())
	return id
}

// hurry makes the issue's rows of kind due now and wakes their lane, so a row backing off on the
// wall clock runs again at once.
func (d *ciDaemon) hurry(kind record.OutboxKind) {
	d.t.Helper()
	d.makeDue(kind, d.key)
	d.wake(kind)
}

// left is how many rows of kind the issue still holds.
func (d *ciDaemon) left(kind record.OutboxKind) int {
	d.t.Helper()
	return d.count(`select count(*) from outbox where issue = $1 and kind = $2`, d.key, string(kind))
}

// rowLeft is whether outbox row id still stands.
func (d *ciDaemon) rowLeft(id int64) bool {
	d.t.Helper()
	return d.count(`select count(*) from outbox where id = $1`, id) == 1
}

// wakes is how many delivery wakes role's claim of the issue took: the wake reservations its rows
// took (store's TakePodWake, pods.WakeDelivery).
func (d *ciDaemon) wakes(role claim.Role) int {
	d.t.Helper()
	return d.count(`select count(*) from pod_resumes where issue = $1 and role = $2 and kind = $3`, d.key, string(role), pods.WakeDelivery)
}

// waiting is whether the wake pacing holds a wake of token.
func (d *ciDaemon) waiting(token claim.Token) bool {
	d.t.Helper()
	for _, wake := range d.state().WakesWaiting {
		if wake.Claim == token {
			return true
		}
	}
	return false
}

// until waits within ciDaemonWait for condition, moving the test clock to the turn of each wake
// of token the wake pacing holds as it waits, as awaitLaunch does: the test clock stands still, so
// a launch the pacing parks comes only once the clock reaches its turn.
func (d *ciDaemon) until(token claim.Token, what string, condition func() bool) {
	d.t.Helper()
	testwait.WithinPolling(d.t, testwait.Bounded(d.t, ciDaemonWait, time.Second), 100*time.Millisecond, what, func() bool {
		if condition() {
			return true
		}
		for _, wake := range d.state().WakesWaiting {
			if wake.Claim == token {
				d.clock.advanceTo(wake.DueAt)
			}
		}
		return false
	})
}

// laneRan waits until the issue's lane of role messages, the lane its CI news rows run on, has run
// once past every row of the issue on it that was due before the call. It queues a row of that
// lane that no pause holds and that changes nothing, due now (dueNow): a second read of the head
// of the issue's pull request whose first read the record does not hold (record.ConflictConfirm),
// which finishes without a read (conflictReadLeft). It waits within ciDaemonWait for that row to
// finish, and moves no clock. A lane runs its due rows one at a time, in the order they came due
// and were written (outbox's drain, record's DueRows), so by then each row due before it that the
// lane may run has had its attempt, and a row the lane may not run, one a pause holds, has had
// none. It answers the row's id.
func (d *ciDaemon) laneRan() int64 {
	d.t.Helper()
	id := d.queue(record.ConflictConfirm{Number: ciPullRequest, Head: ciHeadX, FirstReadAt: d.dueNow()})
	testwait.WithinPolling(d.t, testwait.Bounded(d.t, ciDaemonWait, time.Second), 100*time.Millisecond,
		fmt.Sprintf("row %d, queued behind the issue's due rows of its lane, to finish", id), func() bool { return !d.rowLeft(id) })
	return id
}

// ciLines is each line of the daemon's log whose message is msg and that names row id.
func (d *ciDaemon) ciLines(msg string, id int64) []map[string]any {
	var of []map[string]any
	for _, line := range d.lines(msg) {
		if line["row"] == float64(id) {
			of = append(of, line)
		}
	}
	return of
}

// upAndIdle brings token's woken claim up and leaves it idle, as its pod's launcher and its agent
// do: the launch's shim dials the worker stream and says hello with the launch's boot token, the
// agent registers the session the claim keeps and says it is ready, takes each task the daemon
// gives it in a turn that ends (the shim answers every prompt, from a goroutine of its own, until
// the test ends), and reports, once its claim is idle, that it runs no background work.
func (d *ciDaemon) upAndIdle(token claim.Token) {
	d.t.Helper()
	launch := lastLaunch(d.t, d.rt.Runtime, token)
	d.built.mu.Lock()
	address := d.built.address
	d.built.mu.Unlock()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "tcp://"))
	if err != nil {
		d.t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	sh := newTestShim(d.t, conn)
	sh.send(shimwire.Hello2{BootToken: launch.BootToken})
	if frame := sh.next(); frame.FrameType() != shimwire.TypeHelloAck {
		d.t.Fatalf("the daemon answered the hello with %#v, want hello_ack", frame)
	}
	sh.serve(func() {
		for {
			line, err := sh.lines.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				continue
			}
			var answers []shimwire.Frame
			switch f := frame.(type) {
			case shimwire.NegotiateProtocol:
				answers = []shimwire.Frame{shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true}}
			case shimwire.Prompt:
				answers = []shimwire.Frame{shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true},
					shimwire.AgentStart{}, shimwire.AgentEnd{}, shimwire.PromptResult{ID: f.ID, AgentInvoked: true}}
			}
			for _, answer := range answers {
				select {
				case <-sh.stopping:
					return
				default:
				}
				if sh.writer.WriteFrame(answer) != nil {
					return
				}
			}
		}
	})
	session := "ses-" + string(token)
	status, body := d.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		d.t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		d.t.Fatalf("decode the registration of %s: %v", token, err)
	}
	if status, body := d.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		d.t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	d.until(token, "the woken role's turn on the ready worker's task to end", func() bool {
		c := d.claim(token)
		return c.State == supervise.StateIdle && !c.LastTurnEnd.IsZero()
	})
	d.clock.advance(time.Minute)
	if known, err := d.st.PutClaimActivity(d.ctx, token, launch.Generation, claim.SessionActivity{At: d.clock.Now().UTC()}); err != nil || !known {
		d.t.Fatalf("record %s's report of no background work = %t, %v", token, known, err)
	}
}

// ciGitHub is GitHub's REST API as the daemon of the deletion's tests calls it: refs's branches
// and their deletion (githubtest.Refs), and startedAgainGitHub's answers to the reads and the
// branch create every boot and start make.
func ciGitHub(t *testing.T, refs *githubtest.Refs) string {
	t.Helper()
	return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		if isRepository && len(parts) >= 2 {
			repository := "/repos/" + parts[0] + "/" + parts[1]
			switch {
			case r.Method == http.MethodGet && len(parts) == 2:
				_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
				return
			case r.Method == http.MethodGet && r.URL.Path == repository+"/git/ref/heads/main":
				_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+branchBaseCommit+`","type":"commit"}}`)
				return
			case r.Method == http.MethodPost && r.URL.Path == repository+"/git/refs":
				status, answer := branchExists()
				w.WriteHeader(status)
				_, _ = io.WriteString(w, answer)
				return
			case r.Method == http.MethodGet && r.URL.Path == repository+"/pulls":
				_, _ = io.WriteString(w, `[]`)
				return
			}
		}
		if refs.Handle(w, r) {
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"message":"Not Found"}`)
	}})
}
