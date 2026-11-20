package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func TestOutboxMessageCrashRecoveryPostsOnceAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.MessagePost{Body: "The durable message."}, now))

	first := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	if remaining := outboxRows(t, pool); remaining != 1 {
		t.Fatalf("outbox rows after crash = %d, want leased unfinished row", remaining)
	}

	restarted := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want finished row", remaining)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	markers := 0
	for _, body := range bodies {
		if strings.Contains(body, "<!-- legion-outbox:") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("outbox-marked messages = %d in %#v, want one post across crash recovery", markers, bodies)
	}
}

// A long READY packet posts whole and reads back byte for byte. Dispatch bounds a message body
// block by block, so a 40,000-unit packet of ordinary paragraphs is taken; its message.created
// event carries only the first part of it and says so (body_truncated), which leaves the outbox's
// marker off the end of the event's body. The runner reads such a body from the message itself,
// so the row run again after a crash before its finish finds its own marker and posts nothing,
// and the issue holds exactly one message.
func TestOutboxPostsALongPacketWholeAndOnceAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	// Six paragraphs of 6,665 characters, 40,000 units in all: every block is far under
	// Dispatch's own block bound and the body is far over the units one event carries.
	packet := record.MessagePost{Body: strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 6665)+"\n\n", 6), "\n\n")}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, packet, now))
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox").Scan(&id); err != nil {
		t.Fatalf("read the queued row: %v", err)
	}

	first := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	restarted := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want the row posted and finished", remaining)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if want := packet.Posted(id); len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("Dispatch messages = %d of %d characters, want the 40,000-character packet posted once, whole, under row %d's marker",
			len(bodies), len(strings.Join(bodies, "")), id)
	}
	cut, truncated := messageCreatedEvent(t, baseURL, token, issue)
	if !truncated || strings.Contains(cut, record.MessageMarker(id)) {
		t.Fatalf("the message.created event carries %d characters (truncated=%v), want a cut body with no marker in it", len(cut), truncated)
	}
}

// A post Dispatch refuses the same way on every attempt — one block of the body over Dispatch's
// own block bound — is no retry's to repair. The row finishes and its issue's architect is told
// Dispatch's own refusal, rather than the row climbing its backoff for ever and telling nobody.
func TestOutboxFinishesAPostDispatchRefusesAndTellsTheArchitectAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	over := record.MessagePost{Body: "READY #42 at head for " + issue + "\n\n" + strings.Repeat("x", 32001)}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, over, now))
	runner := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %#v, want the refused post on no issue", bodies)
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(context.Background(), "select kind, payload from outbox").Scan(&kind, &payload); err != nil {
		t.Fatalf("read the row left behind: %v", err)
	}
	if kind != string(record.OutboxKindNotice) {
		t.Fatalf("the one row left is a %s row, want the refused post finished and its architect's notice in its place", kind)
	}
	var notice record.Notice
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if notice.Kind != "message-refused" || notice.Role != claim.RoleArchitect ||
		!strings.Contains(notice.Reason, issue) || !strings.Contains(notice.Reason, "CAP_EXCEEDED") ||
		!strings.Contains(notice.Reason, "READY #42 at head for "+issue) {
		t.Fatalf("the notice = %+v, want message-refused to the architect naming %s, the message's first line and Dispatch's refusal", notice, issue)
	}
}

// A done write posts its reason on the issue before it writes the status, and the reason here is
// one block over Dispatch's own block bound, which Dispatch refuses on every attempt. Against the
// real server: no message is on the issue, the status is set all the same, and the issue's
// architect is told what was refused and that the status stands. Without that, the daemon's
// record would read done while the Dispatch issue kept the status it had, and nobody would know.
func TestOutboxSetsTheStatusOfAWriteWhoseReasonDispatchRefusesAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	client := dispatch.New(baseURL, token)
	before, err := client.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the scratch issue: %v", err)
	}
	now := time.Now().UTC()
	reason := "Closed by its architect before its first phase: " + strings.Repeat("x", 32001)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue,
		record.OrdinaryStatusWrite("done", before.Status, reason), now))
	runner := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	bodies, err := client.MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %#v, want the refused reason on no issue", bodies)
	}
	after, err := client.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("read the scratch issue after the run: %v", err)
	}
	if after.Status != "done" {
		t.Fatalf("the issue stands at %q, want done written though its reason was refused", after.Status)
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(context.Background(), "select kind, payload from outbox").Scan(&kind, &payload); err != nil {
		t.Fatalf("read the row left behind: %v", err)
	}
	if kind != string(record.OutboxKindNotice) {
		t.Fatalf("the one row left is a %s row, want the status row finished and its architect's notice in its place", kind)
	}
	var notice record.Notice
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if notice.Kind != "message-refused" || notice.Role != claim.RoleArchitect ||
		!strings.Contains(notice.Reason, issue) || !strings.Contains(notice.Reason, "CAP_EXCEEDED") ||
		!strings.Contains(notice.Reason, "The status done was set on the issue all the same") {
		t.Fatalf("the notice = %+v, want message-refused to the architect naming %s, Dispatch's refusal and the status it set", notice, issue)
	}
}

// The daemon's board lane publishes an issue's role sessions to a real Dispatch built from this
// tree, and the route stores them: the tester's ready claim is the one holder Dispatch lists. Once
// the claim retires, the next publication sends the empty set and the tester's session leaves the
// list. The first read is made before the lane runs, so the holder Dispatch lists after it is the
// publication's own and not a set something else wrote.
func TestRoleHoldersLandInScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	r := newRoleHoldersRig(t)
	r.runner.dispatch = dispatch.New(baseURL, token)
	logs := &syncBuffer{}
	r.runner.log = slog.New(slog.NewTextHandler(logs, nil))
	tester := r.seedClaim(issue, claim.RoleTester, "ses-tester", "ready", time.Minute)

	holders := func() []dispatch.RoleHolder {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+url.PathEscape(issue)+"/role-holders", nil)
		if err != nil {
			t.Fatalf("build the role-holder read: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := pollClient.Do(request)
		if err != nil {
			t.Fatalf("read the role holders of %s: %v", issue, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("read the role holders of %s = %s %s", issue, response.Status, body)
		}
		var read struct {
			Issue   string                `json:"issue"`
			Holders []dispatch.RoleHolder `json:"holders"`
		}
		if err := json.NewDecoder(response.Body).Decode(&read); err != nil {
			t.Fatalf("decode the role holders of %s: %v", issue, err)
		}
		if read.Issue != issue {
			t.Fatalf("the read answered issue %q, want %s", read.Issue, issue)
		}
		return read.Holders
	}
	run := func() {
		t.Helper()
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		if rows := outboxRows(t, r.pool); rows != 0 {
			t.Fatalf("the outbox holds %d rows after the run, want the publication finished", rows)
		}
	}

	r.enqueue(issue)
	if got := holders(); len(got) != 0 {
		t.Fatalf("before the lane ran, Dispatch lists %+v, want no holder", got)
	}
	run()
	if got, want := holders(), []dispatch.RoleHolder{{Role: "tester", Session: "ses-tester"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the publication, Dispatch lists %+v, want %+v", got, want)
	}

	r.moveClaim(tester, "retired")
	r.enqueue(issue)
	run()
	if got := holders(); len(got) != 0 {
		t.Fatalf("after the tester retired, Dispatch lists %+v, want no holder", got)
	}

	// A set the route refuses ends the row on the first answer, rather than being sent for as long
	// as the issue lives: a claim whose session is no session id, which the route answers 400
	// INVALID_ROLE_HOLDER. The daemon logs one ERROR line naming the issue and the code, and
	// Dispatch keeps the set it holds.
	r.seedClaim(issue, claim.RoleReviewer, "ses reviewer", "ready", time.Minute)
	r.enqueue(issue)
	run()
	if got := holders(); len(got) != 0 {
		t.Fatalf("after the refused publication, Dispatch lists %+v, want the set it held, no holder", got)
	}
	var refused []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "role-holders publish refused by Dispatch") {
			refused = append(refused, line)
		}
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "level=ERROR") || !strings.Contains(refused[0], "issue="+issue) ||
		!strings.Contains(refused[0], "code=INVALID_ROLE_HOLDER") {
		t.Fatalf("the refusal was logged as %q, want one ERROR line naming %s and INVALID_ROLE_HOLDER", refused, issue)
	}
}

// messageCreatedEvent is the issue's message.created event as Dispatch serves it: the body the
// event carries, and whether Dispatch says that body is only the first part of the message's.
func messageCreatedEvent(t *testing.T, baseURL, token, issue string) (string, bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+url.PathEscape(issue)+"/events?limit=200", nil)
	if err != nil {
		t.Fatalf("create the events request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET the issue's events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the issue's events = %s", response.Status)
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			Body          string `json:"body"`
			BodyTruncated bool   `json:"body_truncated"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		t.Fatalf("decode the issue's events: %v", err)
	}
	for _, event := range events {
		if event.Type == "message.created" {
			return event.Payload.Body, event.Payload.BodyTruncated
		}
	}
	t.Fatal("the issue has no message.created event")
	return "", false
}

type crashAfterPostStore struct{ record.Store }

func (crashAfterPostStore) FinishOutbox(context.Context, pgx.Tx, int64, string) error {
	return errors.New("simulated crash before finish")
}

// startScratchDispatch starts the real Dispatch server against a scratch Postgres database of its
// own with no NATS at all, and returns its base URL and agent token. Nothing it does reaches a
// bus: a test that only drives the outbox against it wants no events.
func startScratchDispatch(t *testing.T) (string, string) {
	t.Helper()
	d := startScratchDispatchOn(t, scratchDispatchOptions{})
	return d.base, d.token
}

// startScratchDispatchWithNATS is startScratchDispatch with its events turned on: the same server,
// publishing every issue event on natsURL, which is the NATS server the test holds
// (testnats.URL). Dispatch reconciles the ENVOY_NOTIFICATIONS stream there on its first
// connection, which it makes in the background once it starts, so the caller creates none — and
// must take the NATS server before this call, since testnats.URL empties it of every stream.
func startScratchDispatchWithNATS(t *testing.T, natsURL string) (string, string) {
	t.Helper()
	if natsURL == "" {
		t.Fatal("startScratchDispatchWithNATS was given no NATS server; use startScratchDispatch for a server with no events")
	}
	d := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})
	return d.base, d.token
}

// scratchDispatchOptions are what a scratch Dispatch server's callers vary.
type scratchDispatchOptions struct {
	// natsURL is the NATS server the server publishes its events on; empty runs it with NATS off.
	natsURL string
	// root is the checkout whose packages/envoy the server runs in and whose
	// packages/dispatch/web/dist it serves; empty is this one, four directories above the
	// package's. The binary is not built from it: testbuild.EnvoyBinary builds the checkout
	// testbuild.RootEnv names, else the one holding the working directory.
	root string
	// log, when set, receives the server's output as well.
	log io.Writer
}

// scratchDispatch is a real Dispatch server a test builds and runs: its binary, its own database,
// the port it serves on (kept across a restart, since whatever the test pointed at the server
// names it), the socket the test holds on that port, and the running process. stop and start take
// it down and bring it back on the same port, which the test holds between them, and the same
// database; stopWithStandIn also serves the port with a stand-in that closes each connection at
// once until the next start. The test's end stops it and drops the database.
type scratchDispatch struct {
	t        *testing.T
	binary   string
	dir      string
	webDist  string
	database string
	natsURL  string
	token    string
	home     string
	log      io.Writer
	port     int
	// socket is the listening socket on port that every start hands Dispatch (DISPATCH_LISTEN_FD).
	// The test holds it from the choice of the port until the test ends, so no other program can
	// take the port before Dispatch serves it or while it is stopped.
	socket *os.File
	// standIn serves socket from stopWithStandIn until the next start, closing each connection at
	// once; nil at any other time.
	standIn *closingStandIn
	base    string
	command *exec.Cmd
	exited  chan error
	output  *syncBuffer
}

// startScratchDispatchOn gives the server a scratch database of its own on the test DSN's
// Postgres (testdb.Database) and starts the cmd/dispatch this process built.
func startScratchDispatchOn(t *testing.T, options scratchDispatchOptions) *scratchDispatch {
	t.Helper()
	database := testdb.Database(t, "dispatch_scratch_test")
	root := options.root
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	d := &scratchDispatch{
		t: t, binary: testbuild.EnvoyBinary(t, "./cmd/dispatch"), dir: filepath.Join(root, "packages", "envoy"),
		webDist: filepath.Join(root, "packages", "dispatch", "web", "dist"), database: database,
		natsURL: options.natsURL, token: scratchAgentToken, home: filepath.Join(t.TempDir(), "home"),
		log: options.log,
	}
	if err := os.MkdirAll(d.home, 0o700); err != nil {
		t.Fatalf("create scratch Dispatch home: %v", err)
	}
	t.Cleanup(d.stop)
	d.start()
	// Registered after start's hold of the socket, so it runs before the socket is closed: a test's
	// cleanups run last registered first.
	t.Cleanup(func() {
		if err := d.endStandIn(); err != nil {
			t.Errorf("end the stand-in on scratch Dispatch's port %d: %v", d.port, err)
		}
	})
	return d
}

// start runs the server and returns once GET /api/v1 answers 200. The first start serves on a port
// the kernel chooses; a restart serves on the same one again.
func (d *scratchDispatch) start() {
	t := d.t
	t.Helper()
	// Readiness is a server process starting and migrating a fresh database, whose time is the
	// machine's load: bounded by the test binary's own deadline rather than a fixed span, so a
	// slow start is not read as a server that will never answer.
	deadline := time.Now().Add(2 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-10*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-10 * time.Second)
	}
	// Dispatch serves on a socket the test holds (DISPATCH_LISTEN_FD), so no other process can take
	// the port before Dispatch serves it: the first start holds a port the kernel chose
	// (heldSocket), and a restart is handed the same socket again, once the stand-in
	// stopWithStandIn put on it has ended (endStandIn).
	if d.socket == nil {
		d.socket, d.port = heldSocket(t)
		d.base = fmt.Sprintf("http://127.0.0.1:%d", d.port)
	}
	if err := d.endStandIn(); err != nil {
		t.Fatalf("end the stand-in on scratch Dispatch's port %d: %v", d.port, err)
	}
	command := exec.Command(d.binary)
	command.Dir = d.dir
	// Nothing of the caller's Dispatch, Envoy or NATS settings reaches the scratch server, as
	// run-server.sh drops them: a Legion pane exports DISPATCH_URL and friends.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DISPATCH_") && !strings.HasPrefix(entry, "ENVOY_") && !strings.HasPrefix(entry, "NATS_") && !strings.HasPrefix(entry, "HOME=") {
			command.Env = append(command.Env, entry)
		}
	}
	// The bus is the one difference between the two forms: NATS off, or the test's own server,
	// whose ENVOY_NOTIFICATIONS stream this process reconciles as it starts.
	bus := "DISPATCH_NATS_DISABLED=1"
	if d.natsURL != "" {
		bus = "NATS_URLS=" + d.natsURL
	}
	command.Env = append(command.Env,
		"DATABASE_URL="+d.database,
		"DISPATCH_AGENT_TOKEN="+d.token,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		bus,
		// ExtraFiles' first entry is descriptor 3 in Dispatch.
		"DISPATCH_LISTEN_FD=3",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		fmt.Sprintf("DISPATCH_PORT=%d", d.port),
		"DISPATCH_SERVER_URL="+d.base,
		"DISPATCH_WEB_DIST="+d.webDist,
		"HOME="+d.home,
	)
	command.ExtraFiles = []*os.File{d.socket}
	output := &syncBuffer{}
	command.Stdout = output
	if d.log != nil {
		command.Stdout = io.MultiWriter(output, d.log)
	}
	command.Stderr = command.Stdout
	// The test keeps its own copy of the socket, so a Dispatch that exits leaves the port held: a
	// connection to it waits unanswered until the next start serves it.
	if err := command.Start(); err != nil {
		t.Fatalf("start scratch Dispatch: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	d.command, d.exited, d.output = command, exited, output
	if !testwait.Holds(time.Until(deadline), 50*time.Millisecond, func() bool {
		select {
		case err := <-exited:
			d.command = nil
			t.Fatalf("scratch Dispatch exited before it was ready (%v):\n%s", err, output.String())
		default:
		}
		// Each poll is bounded (pollClient): until Dispatch serves, the held socket queues the
		// connection unanswered.
		if response, err := pollClient.Get(d.base + "/api/v1"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("scratch Dispatch did not become ready:\n%s", output.String())
	}
}

// stop interrupts the running server and waits for it to exit, killing it after ten seconds. A
// server that is not running is left as it is.
func (d *scratchDispatch) stop() {
	if d.command == nil {
		return
	}
	command := d.command
	d.command = nil
	_ = command.Process.Signal(os.Interrupt)
	select {
	case err := <-d.exited:
		if err != nil {
			d.t.Errorf("scratch Dispatch stopped: %v\n%s", err, d.output.String())
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-d.exited
		d.t.Error("scratch Dispatch did not stop after SIGINT")
	}
}

// stopWithStandIn is stop, and then serves the socket the test holds with a stand-in that accepts
// each connection and closes it at once (closingStandIn), until the next start. A client of the
// stopped Dispatch is so told at once that its connection failed, as it is outside a test, where
// a held socket with no server would leave it waiting unanswered. A10 of
// handoff_records_live_test.go proves what a client sees then: a connection closed before any
// answer. The test holds the port all the while, so no other program can bind it.
func (d *scratchDispatch) stopWithStandIn() {
	d.t.Helper()
	d.stop()
	if d.standIn != nil {
		return
	}
	standIn, err := closeEachConnection(d.socket)
	if err != nil {
		d.t.Fatalf("stand in for the stopped scratch Dispatch on port %d: %v", d.port, err)
	}
	d.standIn = standIn
}

// endStandIn ends the stand-in stopWithStandIn started, if one runs (closingStandIn.end).
func (d *scratchDispatch) endStandIn() error {
	if d.standIn == nil {
		return nil
	}
	standIn := d.standIn
	d.standIn = nil
	return standIn.end()
}

// closingStandIn stands in for a stopped server on a socket the test holds: it accepts each
// connection and closes it at once, reading nothing of it. It serves the socket through a
// descriptor of its own (net.FileListener), so ending it closes only that descriptor, and the
// test's still holds the port.
type closingStandIn struct {
	socket   *os.File
	listener net.Listener
	// ended is closed once the accept loop has ended; failed is why it ended, when it ended before
	// end closed its listener.
	ended  chan struct{}
	failed error
}

// closeEachConnection starts a closingStandIn on socket.
func closeEachConnection(socket *os.File) (*closingStandIn, error) {
	listener, err := net.FileListener(socket)
	if err != nil {
		return nil, fmt.Errorf("listen on the held socket: %w", err)
	}
	s := &closingStandIn{socket: socket, listener: listener, ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					s.failed = err
				}
				return
			}
			_ = conn.Close()
		}
	}()
	return s, nil
}

// end stops the stand-in and closes its own descriptor, then closes each connection still queued
// on the socket (closeQueued), so no connection made while the server was down reaches the server
// the socket is handed to next. It fails when the stand-in stopped accepting before end stopped
// it, since connections then waited unanswered.
func (s *closingStandIn) end() error {
	_ = s.listener.Close()
	<-s.ended
	if s.failed != nil {
		return fmt.Errorf("the stand-in stopped accepting connections: %w", s.failed)
	}
	return closeQueued(s.socket)
}

// closeQueued accepts each connection queued on the listening socket and closes it at once. It
// never waits: it stops at the first accept that finds the queue empty (EAGAIN).
func closeQueued(socket *os.File) error {
	raw, err := socket.SyscallConn()
	if err != nil {
		return fmt.Errorf("read the held socket's descriptor: %w", err)
	}
	var failed error
	if err := raw.Control(func(fd uintptr) {
		// Non-blocking, so an empty queue is EAGAIN rather than a wait for the next connection.
		if err := syscall.SetNonblock(int(fd), true); err != nil {
			failed = fmt.Errorf("make the held socket non-blocking: %w", err)
			return
		}
		for {
			// An accepted descriptor is not close-on-exec until it is closed: ForkLock keeps a
			// process started meanwhile from inheriting it, and with it the connection.
			syscall.ForkLock.RLock()
			conn, _, err := syscall.Accept(int(fd))
			if err == nil {
				_ = syscall.Close(conn)
			}
			syscall.ForkLock.RUnlock()
			switch err {
			case nil, syscall.EINTR, syscall.ECONNABORTED:
			case syscall.EAGAIN:
				return
			default:
				failed = fmt.Errorf("accept a queued connection: %w", err)
				return
			}
		}
	}); err != nil {
		return fmt.Errorf("read the held socket's descriptor: %w", err)
	}
	return failed
}

func createScratchDispatchIssue(t *testing.T, baseURL string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(path string, body any) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s: %v", path, err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("create %s request: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dispatch-User", "smoke")
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return response
	}
	project := request("/api/v1/projects", map[string]string{"key": "LEGION", "name": "Scratch Legion"})
	if project.StatusCode != http.StatusCreated {
		project.Body.Close()
		t.Fatalf("create scratch project = %s", project.Status)
	}
	project.Body.Close()
	issue := request("/api/v1/issues", map[string]string{"project": "LEGION", "title": "Crash-safe outbox message"})
	defer issue.Body.Close()
	if issue.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch issue = %s", issue.Status)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(issue.Body).Decode(&created); err != nil {
		t.Fatalf("decode scratch issue: %v", err)
	}
	if created.Key == "" {
		t.Fatal("scratch Dispatch created an issue without a key")
	}
	return created.Key
}
