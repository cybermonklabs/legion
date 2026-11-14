package claudebridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeClaudeEnv makes the test binary a stand-in for `claude -p` in stream-json: it records every
// line it reads to the file fakeClaudeRecordEnv names and answers each as fakeClaude's script says.
const (
	fakeClaudeEnv       = "LEGION_TEST_FAKE_CLAUDE"
	fakeClaudeRecordEnv = "FAKE_CLAUDE_RECORD"
)

// fakeClaudeGateEnv names the file whose appearance lets a replay write its stream's last line
// (rig.open), so a test can act while the turn that line ends is still running.
const fakeClaudeGateEnv = "FAKE_CLAUDE_GATE"

// fakeClaudeModel is the model the stand-in's init line names, as the pinned Claude Code's names
// the model its turn runs on (testdata/api-error-529.jsonl:1).
const fakeClaudeModel = "claude-sonnet-4-5"

// TestMain runs the stand-in for Claude Code when the bridge under test starts this binary as one,
// and the package's tests otherwise. The test across the daemon and the bridge
// (take_daemon_test.go) runs the daemon's routes over a store, cloned from this package's
// template on LEGION_TEST_PG_DSN.
func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		os.Exit(fakeClaude())
	}
	os.Exit(testpg.Main("legion_claudebridge_test", m.Run))
}

// fakeClaude answers Claude Code's stream-json the way the probes recorded it: each user message
// gets a system/init, then the message re-emitted as --replay-user-messages does (the same message,
// marked isReplay, once the step takes it), then its script's lines, then a result. A user
// message's text picks its script:
//   - "/rename <title>": no init and no re-emission, an assistant line naming the title, a success
//     result;
//   - "handoff ok": a `legion` handoff_complete call and its result;
//   - "say <text>": an assistant message of text;
//   - "held <name>": the re-emission only once the test releases name (rig.release);
//   - "wait <name>": an assistant message "done" only once the test releases name;
//   - "die": exits, never re-emitting it;
//   - "fail <text>": an assistant message of text, then an error_during_execution result, as a
//     turn a tool failed in ends, which is no provider failure;
//   - "api-error-529 <text>": the same text, then the result a provider failure ends a turn with
//     — `terminal_reason` "api_error" and `api_error_status` 529 — as the recordings carry it
//     (testdata/api-error-529.jsonl:5);
//   - "refusal <text>": the result a content filter's refusal ends a turn with, which is the
//     same but names no `api_error_status` (testdata/refusal-turn.jsonl:5);
//   - "max-turns": a result Claude Code's own turn limit ended, which is no provider failure;
//   - "maxturns <text>": an assistant message of text, then an error_max_turns result, which is
//     Claude Code's own cap on the steps of one turn rather than the provider failing it;
//   - "fail-when <name>": a step the provider failed, held until the test opens the gate <name>,
//     so a test can decide what has reached the bridge before the failure does;
//   - "replay <file>": no init of the fake's own — the lines of testdata/<file>, a recorded Claude
//     Code stream, written as they were recorded, the last held back until the gate opens;
//   - anything else: an assistant message "done".
func fakeClaude() int {
	record, err := os.OpenFile(os.Getenv(fakeClaudeRecordEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer record.Close()
	fmt.Fprintf(record, "argv %s\n", strings.Join(os.Args[1:], " "))
	fmt.Fprintf(record, "inbox %s\n", os.Getenv(InboxVariable))
	out := json.NewEncoder(os.Stdout)
	emit := func(v any) { _ = out.Encode(v) }
	// released waits for the test to create name beside the record, and takes it.
	released := func(name string) {
		path := filepath.Join(filepath.Dir(os.Getenv(fakeClaudeRecordEnv)), name)
		for os.Remove(path) != nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	assistant := func(blocks ...map[string]any) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": blocks}})
	}
	text := func(t string) map[string]any { return map[string]any{"type": "text", "text": t} }
	result := func(subtype string, isError bool) {
		emit(map[string]any{"type": "result", "subtype": subtype, "is_error": isError})
	}
	// failed is the result of a step Claude Code could not finish on a tool or on itself, which
	// is no failure of the provider's: it names no terminal_reason.
	failed := func() {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"errors": []string{"API Error: 529 overloaded"}})
	}
	// providerFailed is the result a provider failure ends a turn with, as the recordings carry
	// it: subtype "success" with is_error true and terminal_reason "api_error"
	// (testdata/api-error-529.jsonl:5). status is the api_error_status it names, and zero is the
	// refusal, which names none (testdata/refusal-turn.jsonl:5).
	providerFailed := func(status int) {
		line := map[string]any{"type": "result", "subtype": "success", "is_error": true,
			"terminal_reason": "api_error", "errors": []string{"API Error: 529 overloaded"}}
		if status != 0 {
			line["api_error_status"] = status
		}
		emit(line)
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return 0
		}
		fmt.Fprintf(record, "in %s", line)
		var msg struct {
			Message json.RawMessage `json:"message"`
		}
		var user struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(line, &msg) != nil {
			return 3
		}
		if json.Unmarshal(msg.Message, &user) != nil {
			return 3
		}
		body := user.Content[0].Text
		if title, ok := strings.CutPrefix(body, "/rename "); ok {
			assistant(text("Session renamed to: " + title))
			result("success", false)
			continue
		}
		if file, ok := strings.CutPrefix(body, "replay "); ok {
			replay(record, file)
			continue
		}
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "s", "model": fakeClaudeModel})
		if name, ok := strings.CutPrefix(body, "held "); ok {
			released(name)
		}
		if body == "die" {
			return 0
		}
		emit(map[string]any{"type": "user", "message": msg.Message, "session_id": "s", "parent_tool_use_id": nil,
			"uuid": "u", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "isReplay": true})
		switch {
		case body == "handoff ok":
			assistant(map[string]any{"type": "tool_use", "id": "tu1", "name": "mcp__legion__legion", "input": map[string]any{"op": "handoff_complete", "summary": "s"}})
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tu1"}}}})
			assistant(text("completed"))
		case strings.HasPrefix(body, "say "):
			assistant(text(strings.TrimPrefix(body, "say ")))
		case strings.HasPrefix(body, "api-error-529 "):
			assistant(text(strings.TrimPrefix(body, "api-error-529 ")))
			providerFailed(529)
			continue
		case strings.HasPrefix(body, "refusal "):
			assistant(text(strings.TrimPrefix(body, "refusal ")))
			providerFailed(0)
			continue
		case strings.HasPrefix(body, "fail "):
			assistant(text(strings.TrimPrefix(body, "fail ")))
			failed()
			continue
		case body == "max-turns":
			// Claude Code's own end of a turn it ran out of turns in. This line is written here
			// rather than taken from a recording: no result of the measurement carries this
			// subtype (dispatch://INFRA-956/artifact/claude-code-2-1-310-refusal-measurement-tgz),
			// so it names the one terminal_reason that was measured, which is the value that
			// would count the turn refused were the subtype not excluded by name (bridge.go's
			// providerFailed).
			emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true,
				"terminal_reason": "api_error", "errors": []string{"Reached max turns (30)"}})
			continue
		case strings.HasPrefix(body, "maxturns "):
			assistant(text(strings.TrimPrefix(body, "maxturns ")))
			emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true,
				"errors": []string{"reached the maximum number of turns"}})
			continue
		case strings.HasPrefix(body, "fail-when "):
			// The step fails on the provider only once the test opens the gate. The fake cannot
			// order a delivery against its own result: both reach the bridge through two cases of
			// one select, and a queued delivery emits no frame for this process to see. So the
			// test sends the delivery, waits for the bridge's own log line saying it is queued,
			// and opens the gate; the failure then lands with the delivery already in the queue,
			// which is the order the case under test is about.
			released(strings.TrimPrefix(body, "fail-when "))
			assistant(text("trying"))
			providerFailed(529)
			continue
		case strings.HasPrefix(body, "wait "):
			released(strings.TrimPrefix(body, "wait "))
			assistant(text("done"))
		default:
			assistant(text("done"))
		}
		result("success", false)
	}
}

// replay writes a recorded Claude Code stream line for line, holding its last line back until the
// gate opens. The recordings are of the pinned Claude Code (2.1.310, the version
// docker/claude-code/package-lock.json pins) in print mode: a line is what that binary wrote,
// never written by hand. background-turn.jsonl and background-during-prompt.jsonl were recorded
// against the provider; refusal-turn.jsonl, api-error-529.jsonl and api-error-429.jsonl against
// a stand-in endpoint that answered one refusal, one 529 and one 429
// (dispatch://INFRA-956/artifact/claude-code-2-1-310-refusal-measurement-tgz, whose
// out-refusal.jsonl, out-e529.jsonl and out-e429.jsonl these are). A stream is one recording
// trimmed to the lines a case needs, and commandBlockInTurn is the only one whose lines stand in
// another order than the recorded one, which its own comment says.
func replay(record *os.File, file string) {
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		fmt.Fprintln(record, "replay", err)
		return
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for i, line := range lines {
		if i == len(lines)-1 {
			waitForGate(record)
		}
		fmt.Fprintf(os.Stdout, "%s\n", line)
	}
}

// waitForGate waits up to 10s for the test to open the gate, and returns at once when none is set.
func waitForGate(record *os.File) {
	gate := os.Getenv(fakeClaudeGateEnv)
	if gate == "" {
		return
	}
	for range 1000 {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintln(record, "the gate never opened")
}

// rig runs one bridge over the fake Claude: frames go in through daemon, come out on frames.
type rig struct {
	t       *testing.T
	daemon  *io.PipeWriter
	frames  chan map[string]any
	record  string
	log     *lockedBuffer
	gate    string
	done    chan int
	stopped context.CancelFunc
}

// newRig runs one bridge over the fake Claude Code, with the environment a pane gives it.
func newRig(t *testing.T, args ...string) *rig {
	t.Helper()
	return newRigWithEnv(t, nil, daemonCalls{}, args...)
}

// newRigWithEnv is newRig with more of the pane's environment than a bare bridge needs: the
// daemon, boot token file and generation a bridge reports its turns and takes its task with
// (daemonlink.go); and calls, the timing and network of the bridge's seen reports and takes, the
// zero value being production's.
func newRigWithEnv(t *testing.T, extra []string, calls daemonCalls, args ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	// Run opens the inbox under the process TMPDIR, so the rig places that directory itself, with
	// room for the inbox's socket.
	t.Setenv("TMPDIR", testtmp.Dir(t, InboxLongestPath))
	record, gate := filepath.Join(dir, "record"), filepath.Join(dir, "gate")
	argv := append([]string{os.Args[0]}, args...)
	env := append(os.Environ(), fakeClaudeEnv+"=1", fakeClaudeRecordEnv+"="+record, fakeClaudeGateEnv+"="+gate,
		"CLAUDE_CONFIG_DIR="+dir, "LEGION_ROLE=implementer", "LEGION_ISSUE=ACME-7")
	env = append(env, extra...)
	cfg, err := Configure(argv, env)
	if err != nil {
		t.Fatal(err)
	}
	cfg.calls = calls
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	logged := &lockedBuffer{}
	cfg.Stdin, cfg.Stdout, cfg.Log = inR, outW, logged
	r := &rig{t: t, daemon: inW, frames: make(chan map[string]any, 100), record: record, gate: gate,
		log: logged, done: make(chan int, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.stopped = cancel
	go func() {
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Errorf("the bridge wrote a line that is not JSON: %q", scanner.Text())
				continue
			}
			r.frames <- frame
		}
	}()
	go func() {
		code, err := Run(ctx, cfg)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		_ = outW.Close()
		r.done <- code
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not end")
		}
	})
	return r
}

func (r *rig) send(frame shimwire.Frame) {
	r.t.Helper()
	line, err := json.Marshal(frame)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.daemon.Write(append(line, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// nextRaw is the next frame the bridge wrote, pane summaries (tool_execution_start) included.
func (r *rig) nextRaw() map[string]any {
	r.t.Helper()
	select {
	case frame := <-r.frames:
		return frame
	case <-time.After(10 * time.Second):
		r.t.Fatal("the bridge wrote nothing within 10s")
		return nil
	}
}

// next is the next frame the bridge wrote that is not a pane summary (tool_execution_start).
func (r *rig) next() map[string]any {
	r.t.Helper()
	for {
		if frame := r.nextRaw(); frame["type"] != "tool_execution_start" {
			return frame
		}
	}
}

// expect is the next frame, which must be a typ frame carrying every field of want.
func (r *rig) expect(typ string, want map[string]any) map[string]any {
	r.t.Helper()
	frame := r.next()
	if frame["type"] != typ {
		r.t.Fatalf("got %v, want a %s frame", frame, typ)
	}
	for k, v := range want {
		if frame[k] != v {
			r.t.Fatalf("got %v, want %s %v", frame, k, v)
		}
	}
	return frame
}

// quiet holds that the bridge writes no frame within a short while.
func (r *rig) quiet() {
	r.t.Helper()
	select {
	case frame := <-r.frames:
		r.t.Fatalf("got %v, want no frame", frame)
	case <-time.After(300 * time.Millisecond):
	}
}

// open lets a replay write the last line of its recorded stream (fakeClaudeGateEnv).
func (r *rig) open() {
	r.t.Helper()
	if err := os.WriteFile(r.gate, nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) recorded() string {
	raw, _ := os.ReadFile(r.record)
	return string(raw)
}

// waitFor waits up to 10 s for holds, failing with what it waited for.
func (r *rig) waitFor(what string, holds func() bool) {
	r.t.Helper()
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, holds) {
		r.t.Fatalf("waited 10s for %s; the bridge logged:\n%s\nClaude Code recorded:\n%s", what, r.log, r.recorded())
	}
}

// logged waits for the bridge to log text.
func (r *rig) logged(text string) {
	r.t.Helper()
	r.waitFor("the log line "+strconv.Quote(text), func() bool { return strings.Contains(r.log.String(), text) })
}

// release lets the fake's "held <name>" or "wait <name>" go on, once it has taken the release.
func (r *rig) release(name string) {
	r.t.Helper()
	path := filepath.Join(filepath.Dir(r.record), name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor("Claude Code to take the release "+name, func() bool {
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	})
}

// inboxClient is one connection to the bridge's inbox, which claude-envoy's channel server keeps
// for every delivery, writing each without waiting for the answers before it.
type inboxClient struct {
	t       *testing.T
	conn    net.Conn
	answers *bufio.Reader
}

func (r *rig) dialInbox() *inboxClient {
	r.t.Helper()
	r.waitFor("Claude Code to start", func() bool { return strings.Contains(r.recorded(), "\ninbox /") })
	conn, err := net.Dial("unix", inboxPathOf(r.t, r))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	return &inboxClient{t: r.t, conn: conn, answers: bufio.NewReader(conn)}
}

func (c *inboxClient) deliver(id, content string) {
	c.t.Helper()
	c.deliverBlocks(id, content, nil)
}

// deliverBlocks is one delivery that carries Claude Code's own content blocks beside its text, as
// claude-envoy writes a person's own message and the pictures it embeds (src/legion-inbox.ts).
func (c *inboxClient) deliverBlocks(id, content string, blocks []json.RawMessage) {
	c.t.Helper()
	if err := json.NewEncoder(c.conn).Encode(InboxDelivery{ID: id, Content: content, Blocks: blocks}); err != nil {
		c.t.Fatal(err)
	}
}

// answer is the inbox's next answer, which must come within 10 s.
func (c *inboxClient) answer() InboxAnswer {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.answers.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("the inbox answered %q, %v", line, err)
	}
	var answer InboxAnswer
	if err := json.Unmarshal(line, &answer); err != nil {
		c.t.Fatalf("the inbox answered %q: %v", line, err)
	}
	return answer
}

// noAnswer holds that the inbox answers nothing within a short while.
func (c *inboxClient) noAnswer() {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.answers.ReadBytes('\n')
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || len(line) > 0 {
		c.t.Fatalf("the inbox answered %q, %v; want no answer yet", line, err)
	}
}

// turn starts a turn of the bridge's own with message as its first user message: an Envoy
// delivery between turns, which is what starts one (the daemon pushes no task on the stream). It
// goes down an inbox connection of its own, which keeps its answer, so a test's own inbox client
// reads the answers of its own deliveries alone.
func (r *rig) turn(id, message string) {
	r.t.Helper()
	r.dialInbox().deliver(id, message)
}

func (r *rig) state(id string) bool {
	r.t.Helper()
	r.send(shimwire.GetState{ID: id})
	frame := r.expect("response", map[string]any{"id": id, "command": "get_state", "success": true})
	return frame["data"].(map[string]any)["isStreaming"].(bool)
}

const sessionID = "11111111-2222-4333-8444-555555555555"

func TestAFreshSessionIsSeededBeforeItsFirstTurn(t *testing.T) {
	r := newRig(t, "-p", "--session-id", sessionID)
	r.send(shimwire.NegotiateProtocol{ID: "n1", ProtocolVersion: 2})
	r.expect("response", map[string]any{"id": "n1", "command": "negotiate_protocol", "success": true})
	r.turn("ev1", "say hello WAITING")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	lines := strings.Split(r.recorded(), "\n")
	var inputs []string
	for _, line := range lines {
		if body, ok := strings.CutPrefix(line, "in "); ok {
			inputs = append(inputs, body)
		}
	}
	if len(inputs) < 2 || !strings.Contains(inputs[0], `"text":"/rename Legion implementer · ACME-7"`) || !strings.Contains(inputs[1], "say hello") {
		t.Fatalf("Claude Code read %q, want the seed and then the delivery", inputs)
	}
	if path := inboxPathOf(t, r); !strings.HasSuffix(path, "/inbox.sock") {
		t.Errorf("Claude Code was told the inbox %q", path)
	}
}

func TestAResumedSessionIsNotSeeded(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("ev1", "say WAITING for CI")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if strings.Contains(r.recorded(), "/rename") {
		t.Fatalf("a resumed session was seeded: %s", r.recorded())
	}
}

func TestATurnIsOneUserMessageWhenTheHandoffSucceeds(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	if r.state("g1") {
		t.Fatal("isStreaming before any turn")
	}
	r.turn("ev1", "handoff ok")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if strings.Count(r.recorded(), `"type":"user"`) != 1 {
		t.Errorf("a turn was more than one user message: %s", r.recorded())
	}
}

// get_state says a turn is running from its agent_start to its agent_end, whichever started it: a
// turn of the bridge's own, and one Claude Code started by itself when a background task
// finished. The gate holds each turn's own result back, so the turn is still running when the
// state is asked.
func TestGetStateSaysATurnIsRunningWhicheverStartedIt(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("ev1", backgroundDuringPrompt)
	r.expect("agent_start", nil)
	if !r.state("g1") {
		t.Error("isStreaming during the turn is false")
	}
	r.open()
	r.expect("agent_end", nil)
	if r.state("g2") {
		t.Error("isStreaming after the turn is true")
	}

	r = newRig(t, "-p", "--resume", sessionID)
	r.turn("ev1", backgroundTurn)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("agent_start", nil)
	if !r.state("g3") {
		t.Error("isStreaming during the turn Claude Code started is false")
	}
	r.open()
	r.expect("agent_end", nil)
	if r.state("g4") {
		t.Error("isStreaming after the turn Claude Code started is true")
	}
}

// A step Claude Code ends in error reaches the daemon as Oh My Pi reports a failed step: the
// assistant message_end with stopReason "error" and the failure as its errorMessage.
func TestAFailedStepIsOhMyPisFailedMessage(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("ev1", "fail WAITING on the provider")
	r.expect("agent_start", nil)
	frame := r.expect("message_end", nil)
	message, _ := frame["message"].(map[string]any)
	if message["role"] != "assistant" || message["stopReason"] != "error" {
		t.Fatalf("the failed step's message is %v, want an assistant message with stopReason error", frame)
	}
	if got, _ := message["errorMessage"].(string); !strings.Contains(got, "error_during_execution") ||
		!strings.Contains(got, "API Error: 529 overloaded") {
		t.Errorf("errorMessage %q names neither the result's subtype nor its error", got)
	}
	r.expect("agent_end", nil)
}

// An Envoy delivery during a turn is the turn's next user message; one between turns starts a turn.
// Either way the inbox answers it once Claude Code has taken it, and refuses a malformed one.
func TestEnvoyDeliveriesBecomeUserTurns(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "say WAITING on the CI")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("r1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
	if recorded := r.recorded(); !strings.Contains(recorded, `"text":"say WAITING on the CI"`) {
		t.Fatalf("the delivery was not a user message: %s", recorded)
	}

	inbox.deliver("ev2", "say WAITING still")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev2", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.logged("Envoy delivery ev2 starts a turn")
	if _, err := inbox.conn.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if answer := inbox.answer(); answer.OK || !strings.Contains(answer.Error, "an id and content") {
		t.Errorf("a malformed delivery was answered %+v", answer)
	}
}

// The inbox answers a delivery once Claude Code has taken it, re-emitting it as the user message it
// reads (--replay-user-messages), not once the bridge has queued or written it: a Claude Code that
// has read the delivery and not yet taken it holds the answer back.
func TestTheInboxAnswersOnceClaudeCodeTakesTheDelivery(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "held h1")
	r.expect("agent_start", nil)
	r.waitFor("Claude Code to read the delivery", func() bool { return strings.Contains(r.recorded(), `"text":"held h1"`) })
	inbox.noAnswer()
	r.release("h1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
}

// Deliveries that arrive during a turn, written down one connection without waiting, are the turn's
// next steps in the order they came, and each is answered as Claude Code takes it, not before.
func TestDeliveriesQueuedInATurnAreAnsweredAsEachIsTaken(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "wait d1")
	inbox.deliver("ev2", "wait d2")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.logged("Envoy delivery ev2 waits for the next step")
	inbox.noAnswer()
	r.release("r1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	inbox.noAnswer()
	r.release("d1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev2", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev2 taken", answer)
	}
	r.release("d2")
	r.expect("agent_end", nil)
	r.quiet()
}

// A delivery Claude Code never takes, because it exits first, is answered with an error: the one it
// read and exited without taking, and the one the bridge still held for the step after it.
func TestADeliveryClaudeCodeNeverTakesIsAnsweredWithAnError(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "die")
	inbox.deliver("ev2", "say never")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.logged("Envoy delivery ev2 waits for the next step")
	r.release("r1")
	for _, id := range []string{"ev1", "ev2"} {
		if answer := inbox.answer(); answer != (InboxAnswer{ID: id, Error: "Claude Code has exited"}) {
			t.Fatalf("the inbox answered %+v, want %s refused: Claude Code has exited", answer, id)
		}
	}
}

// backgroundTurn and backgroundDuringPrompt are the two recorded streams these cases drive the
// bridge with (replay). In backgroundTurn one user message starts a background Bash command and
// ends its turn; the task finishes between turns, and lines 13-15 — task_updated,
// task_notification and background_tasks_changed, each naming the task and its run — are followed
// by the system/init of the turn Claude Code starts by itself, its assistant lines and its own
// result. In backgroundDuringPrompt the task finishes while a foreground command of the same turn
// still runs, and the same three lines arrive inside that turn, which has one result, its last
// line.
const (
	backgroundTurn         = "replay background-turn.jsonl"
	backgroundDuringPrompt = "replay background-during-prompt.jsonl"
)

// A background task that finishes between turns wakes Claude Code into a turn of its own, which
// the daemon is told of as a turn: agent_start when the init of that turn arrives, agent_end when
// its result does, and in between only what Claude Code's own lines carry. The first turn ends
// on its own result first, so the daemon sees two turns, not one long one.
func TestATurnClaudeCodeStartsByItselfIsATurnOfTheDaemons(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", backgroundTurn)
	r.open()
	var types []string
	for ends := 0; ends < 2; {
		typ, _ := r.nextRaw()["type"].(string)
		types = append(types, typ)
		if typ == "agent_end" {
			ends++
		}
	}
	// The first turn carries one tool call, the background Bash command, which the bridge shows
	// on the pane; the turn Claude Code started by itself calls no tool, so the bridge writes
	// nothing of its own between that turn's start and its end.
	want := []string{"agent_start", "tool_execution_start", "agent_end", "agent_start", "agent_end"}
	if !slices.Equal(types, want) {
		t.Fatalf("the daemon saw %q, want %q", types, want)
	}
	r.quiet()
}

// The three lines a background task writes when it finishes during a turn end no turn: the daemon
// sees one agent_start, and the agent_end only once the turn's own result arrives.
func TestABackgroundTaskFinishingDuringATurnDoesNotEndIt(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", backgroundDuringPrompt)
	r.expect("agent_start", nil)
	// The turn's two tool calls, the background command and the foreground one; the task's three
	// lines come between the second call and the turn's result, which the gate holds back.
	for i := range 2 {
		if frame := r.nextRaw(); frame["type"] != "tool_execution_start" {
			t.Fatalf("got %v, want tool call %d of the turn", frame, i+1)
		}
	}
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	r.quiet()
}

// An Envoy delivery that arrives during a turn Claude Code started by itself is queued as it is
// during any turn — the turn runs on, and the inbox waits — and is the user message of the turn
// the bridge starts as soon as that one ends, answered once Claude Code takes it there.
func TestAnEnvoyDeliveryDuringAClaudeStartedTurnFollowsIt(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", backgroundTurn)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev9", "say the CI is green")
	r.logged("Envoy delivery ev9 waits for the next step")
	inbox.noAnswer()
	if strings.Contains(r.recorded(), "the CI is green") {
		t.Errorf("the delivery reached Claude Code during the turn it started: %s", r.recorded())
	}
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev9", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
	if !strings.Contains(r.recorded(), `"text":"say the CI is green"`) {
		t.Errorf("the delivery was no user message: %s", r.recorded())
	}
}

// commandBlock and the three streams beside it are the four the command cases drive the bridge
// with (replay), all four trimmed from one recorded run of the pinned Claude Code: a mod of the
// plugin's called `$.command.run({command:"context"})` nine seconds into a print-mode turn that
// was running `sleep 20`; the command waited for the turn, and its whole block — a
// `command_lifecycle` `started` row, a `system` `init`, the command's own output and a `result`
// of its own, then the `completed` row of the same command_uuid — landed after the turn's own
// result. commandBlock is the recorded order, the block between turns. commandBlockInTurn is the
// same recorded lines with the turn's own result moved last, so the block nests inside the turn
// it was called from, which is the shape the measurement did not produce and the bridge must
// read all the same. The two torn streams drop one lifecycle row each, as a real process's
// output can: commandBlockUnstarted a `completed` whose command never started, and
// commandBlockUnfinished a `started` whose `completed` never came.
const (
	commandBlock           = "replay command-block.jsonl"
	commandBlockInTurn     = "replay command-block-in-turn.jsonl"
	commandBlockUnstarted  = "replay command-block-unstarted.jsonl"
	commandBlockUnfinished = "replay command-block-unfinished.jsonl"
)

// A command's block between turns is no turn of anyone's: the daemon is told of no agent_start
// and no agent_end for it. The command's `init` is not a turn Claude Code started by itself, and
// its `result` ends nothing, so the delivery's turn is the one turn the daemon sees.
func TestACommandsBlockBetweenTurnsIsNoTurn(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", commandBlock)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	// The block's `started`, its `init`, the command's output and its `result`; the `completed`
	// row is the stream's last line, which the gate holds back.
	r.quiet()
	r.open()
	r.quiet()
	// The block did arrive: what the bridge passed over is what it read.
	r.logged("ignoring the init of the command Claude Code is running")
	r.logged("ignoring the success result of the command Claude Code is running")
}

// A command's block inside a turn ends no step of that turn: the delivery that waits for the next
// step is neither sent during the block nor answered errNotTaken by the command's result, and the
// turn's own result, which comes after the block, ends the step as it always did — the delivery
// is sent then, taken, and the turn ends.
func TestACommandsBlockInsideATurnEndsNoStep(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", commandBlockInTurn)
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "say the CI is green")
	r.logged("Envoy delivery ev1 waits for the next step")
	inbox.noAnswer()
	r.quiet()
	// The turn's own result, the stream's last line.
	r.open()
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want the delivery taken by the step the turn's own result started", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
	// The block did arrive: the result the bridge passed over is the command's own.
	r.logged("ignoring the success result of the command Claude Code is running")
}

// A torn block — a `completed` row for a command that never started, a `started` row whose
// `completed` never came — leaves the bridge working: the stream is a real process's output, and
// a line that never arrives must not leave the bridge ignoring the result of every turn after it.
// A `started` row owes one init and one result, and the lines after those two are read as any
// other turn's, so the lost row costs the bridge that one block and nothing after it.
func TestATornCommandBlockLeavesALaterTurnWorking(t *testing.T) {
	for _, torn := range []struct{ name, stream string }{
		{"a completed row for a command that never started", commandBlockUnstarted},
		{"a started row whose completed never came", commandBlockUnfinished},
	} {
		t.Run(torn.name, func(t *testing.T) {
			r := newRig(t, "-p", "--resume", sessionID)
			r.turn("p1", torn.stream)
			r.expect("agent_start", nil)
			r.expect("agent_end", nil)
			r.open()
			r.quiet()
			r.turn("p2", "say the torn block cost nothing")
			r.expect("agent_start", nil)
			r.expect("agent_end", nil)
		})
	}
}

// A command still running when the bridge writes its next user message — its `started` read, its
// `completed` not — ends no step of the bridge's. A person sends `/compact` to an idle pane (6 to
// 29 s, measured in this issue's earlier rounds), and while it runs an Envoy delivery arrives, so
// the bridge starts a turn inside the open block. The command's own result must not end that
// turn: a delivery answered errNotTaken is sent to the agent a second time, and the turn's end
// written before it has run leaves the real turn to be read as one Claude Code started by itself.
//
// The fake writes the command's result before it reads the user message waiting on its stdin,
// which is one of the two orders Claude Code can write them in; the bridge's rule — spend the one
// result each `started` owes, close no block whose `completed` has not come — holds in either,
// since no line of the stream says when Claude Code took its stdin.
func TestACommandStillRunningEndsNoStepTheBridgeStarted(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", commandBlockUnfinished)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	// The block is open: its `started` row and its own `init` are read, and its own `result`
	// is the stream's last line, which the gate holds back.
	r.logged("ignoring the init of the command Claude Code is running")
	inbox := r.dialInbox()
	inbox.deliver("ev1", "held h1")
	r.expect("agent_start", nil)
	inbox.noAnswer()
	// The command's own result. Claude Code reads the delivery behind it, and answers it only
	// once the test releases h1, so no frame of the delivery's turn stands in the way here.
	r.open()
	inbox.noAnswer()
	r.quiet()
	r.logged("ignoring the success result of the command Claude Code is running")
	r.release("h1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want the delivery taken by the turn begun inside the block", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
}

// A delivery that carries a person's pictures reaches Claude Code as the blocks claude-envoy
// wrote, picture and all, and is answered once Claude Code takes it: the echo's joined text
// blocks are the delivery's content, which the blocks carry unchanged.
func TestADeliveryWithPicturesIsSentAsItsBlocks(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()
	inbox.deliverBlocks("ev1", "say what this is", []json.RawMessage{
		json.RawMessage(`{"type":"text","text":"say what this is"}`),
		json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`),
	})
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want the delivery taken", answer)
	}
	r.expect("agent_end", nil)
	blocks := `"content":[{"type":"text","text":"say what this is"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]`
	if !strings.Contains(r.recorded(), blocks) {
		t.Errorf("Claude Code read %s, want the delivery's own blocks %s", r.recorded(), blocks)
	}
}

// An inbox client still connected when the bridge stops (claude-envoy's channel server keeps one
// connection for every delivery, and may outlive Claude Code) neither holds the bridge's exit nor
// keeps its inbox's directory.
func TestTheBridgeEndsWithAnInboxClientConnected(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", "say WAITING for CI")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	path := inboxPathOf(t, r)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// An answer proves the inbox accepted the connection and serves it, so its client is one the
	// inbox's close waits for: a malformed delivery is answered with its refusal.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if answer, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil || !strings.Contains(string(answer), "an id and content") {
		t.Fatalf("the inbox answered %q, %v", answer, err)
	}
	r.stopped()
	select {
	case code := <-r.done:
		r.done <- code
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not end within 10s with an inbox client connected")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the inbox's directory %s is still there (%v)", filepath.Dir(path), err)
	}
}

// A Claude Code that stops reading its stdin while it makes control requests fills the queue of
// the bridge's refusals: the bridge says so once, ends Claude Code, and exits with its inbox
// removed. (A shim that stops reading holds Claude Code's output back instead; cmd/legion's
// TestClaudeBridgeHoldsClaudeCodeWhileTheShimPauses.)
func TestAClaudeCodeThatStopsReadingEndsTheBridge(t *testing.T) {
	// A refusal repeats its request's id, so a long id fills the pipe within a few dozen refusals.
	controlRequest := `{"type":"control_request","request_id":"` + strings.Repeat("r", 1000) + `","request":{"subtype":"can_use_tool"}}`
	// The inbox is opened under the process TMPDIR, so the test places that directory, with room
	// for the inbox's socket.
	tmp := testtmp.Dir(t, InboxLongestPath)
	t.Setenv("TMPDIR", tmp)
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream")
	if err := os.WriteFile(stream, []byte(strings.Repeat(controlRequest+"\n", 2*claudeQueueLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, daemon := io.Pipe()
	t.Cleanup(func() { _ = daemon.Close() })
	logged := &lockedBuffer{}
	cfg := Config{Argv: []string{"/bin/sh", "-c", "cat " + stream + "\nexec sleep 300\n"}, Env: os.Environ(),
		SessionID: sessionID, Stdin: stdin, Stdout: io.Discard, Log: logged}
	done := make(chan int, 1)
	go func() {
		code, err := Run(context.Background(), cfg)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- code
	}()
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit status %d, want %d, Claude Code's once SIGTERM ended it:\n%s", code, 128+int(syscall.SIGTERM), logged)
		}
	case <-time.After(30 * time.Second):
		// Claude Code still runs, unreaped, under a bridge that never ended.
		if match := regexp.MustCompile(`\(pid (\d+)\)`).FindStringSubmatch(logged.String()); match != nil {
			pid, _ := strconv.Atoi(match[1])
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("the bridge did not end within 30s of Claude Code no longer reading:\n%s", logged)
	}
	overflow := "Claude Code has left " + strconv.Itoa(claudeQueueLength) + " lines unread"
	if n := strings.Count(logged.String(), overflow); n != 1 {
		t.Errorf("the bridge said %q %d times, want once:\n%s", overflow, n, logged)
	}
	if !strings.Contains(logged.String(), "Claude Code stopped reading its input: sending SIGTERM to Claude Code") {
		t.Errorf("the bridge did not end Claude Code:\n%s", logged)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "legion-claude-*")); len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// lockedBuffer is a log the bridge's goroutines write while a test reads it.
type lockedBuffer = testlog.Buffer

// inboxPathOf is the inbox Claude Code was told, as the fake recorded it.
func inboxPathOf(t *testing.T, r *rig) string {
	t.Helper()
	for _, line := range strings.Split(r.recorded(), "\n") {
		if path, ok := strings.CutPrefix(line, "inbox "); ok && path != "" {
			return path
		}
	}
	t.Fatalf("Claude Code was told no inbox: %s", r.recorded())
	return ""
}

func TestUnknownRequestsAreRefusedByName(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.send(shimwire.Raw{Type: "steer", JSON: []byte(`{"type":"steer","id":"s1","message":"x"}`)})
	frame := r.expect("response", map[string]any{"id": "s1", "command": "steer", "success": false})
	if !strings.Contains(frame["error"].(string), "does not support steer") {
		t.Errorf("the refusal is %q", frame["error"])
	}
}

// The bridge's exit status is Claude Code's, and the shim closing the bridge's input ends it.
func TestTheShimClosingInputEndsClaudeCode(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", "say WAITING")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	_ = r.daemon.Close()
	select {
	case code := <-r.done:
		if code != 0 {
			t.Errorf("exit status %d, want Claude Code's 0", code)
		}
		r.done <- code
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge outlived its input")
	}
}

func TestConfigure(t *testing.T) {
	base := []string{"LEGION_ROLE=tester", "LEGION_ISSUE=ACME-2"}
	cfg, err := Configure([]string{"claude", "-p", "--session-id=" + sessionID}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Seed || cfg.SessionID != sessionID || cfg.Title != "Legion tester · ACME-2" {
		t.Errorf("Configure = %+v", cfg)
	}
	cfg, err = Configure([]string{"claude", "--resume", sessionID, "-p"}, nil)
	if err != nil || cfg.Seed || cfg.SessionID != sessionID {
		t.Errorf("resume: %+v, %v", cfg, err)
	}
	for _, tc := range []struct {
		argv []string
		env  []string
		want string
	}{
		{nil, base, "no claude command"},
		{[]string{"claude", "-p"}, base, "exactly one --session-id or --resume"},
		{[]string{"claude", "--session-id", "a", "--resume", "a"}, base, "exactly one"},
		{[]string{"claude", "--resume="}, base, "--resume names no session"},
		{[]string{"claude", "--session-id", "a"}, nil, "LEGION_ROLE and LEGION_ISSUE"},
	} {
		if _, err := Configure(tc.argv, tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Configure(%q) = %v, want %q", tc.argv, err, tc.want)
		}
	}
}

// testEpoch is the task bucket's epoch the cases' values are written at: its stream's creation
// time, RFC 3339 with nanoseconds. testTask is the number of the task the cases' claim is owed.
const (
	testEpoch = "2026-11-13T16:21:44.123456789Z"
	testTask  = 7
)

// quickCalls are the waits the cases of departure 7 send again with: an attempt with no answer after
// 200 ms, and waits from 20 ms doubling to 80 ms, so a case sees several attempts within a second.
var quickCalls = daemonCalls{answerTimeout: 200 * time.Millisecond, resendFirst: 20 * time.Millisecond, resendMax: 80 * time.Millisecond}

// taskDaemon stands in for the daemon's claim routes a bridge calls while it takes its claim's
// task (take.go): it records each seen report and take it answers, answers each as the case says,
// and hands each turn report to the case. While fault is set it fails every seen report and take
// that reaches it with fault instead: the failures of departure 7 a daemon's side makes.
type taskDaemon struct {
	t       *testing.T
	server  *httptest.Server
	reports chan claim.TurnOutcomeRequest
	// answerSeen and answerTake answer a seen report and a take that is not failed: 204, and an
	// accepted take of "say the task", unless the case sets its own before the stand-in starts.
	answerSeen func(claim.TaskSeenRequest) (int, any)
	answerTake func(claim.TakeRequest) (int, any)

	mu    sync.Mutex
	fault func(http.ResponseWriter, *http.Request)
	seen  []claim.TaskSeenRequest
	takes []claim.TakeRequest
}

// newTaskDaemon is a stand-in not yet serving: newTakingRig starts it, or a case starts it itself.
func newTaskDaemon(t *testing.T) *taskDaemon {
	t.Helper()
	d := &taskDaemon{t: t, reports: make(chan claim.TurnOutcomeRequest, 16),
		answerSeen: func(claim.TaskSeenRequest) (int, any) { return http.StatusNoContent, nil },
		answerTake: func(claim.TakeRequest) (int, any) { return accepted("say the task") },
	}
	d.server = httptest.NewUnstartedServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

// accepted is an accepted take's answer, whose turn runs text.
func accepted(text string) (int, any) {
	return http.StatusOK, claim.TakeResponse{Outcome: takeAccepted, Text: text}
}

// startAt starts the stand-in serving at addr, an address nothing listened on until now.
func (d *taskDaemon) startAt(addr string) {
	d.t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		d.t.Fatalf("listen on %s: %v", addr, err)
	}
	_ = d.server.Listener.Close()
	d.server.Listener = listener
	d.server.Start()
}

func (d *taskDaemon) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Errorf("read the bridge's %s: %v", r.URL.Path, err)
		return
	}
	switch r.URL.Path {
	case turnOutcomePath:
		var report claim.TurnOutcomeRequest
		if d.decoded(body, &report) {
			d.reports <- report
			writeAnswer(w, http.StatusOK, claim.TurnOutcomeResponse{})
		}
	case taskSeenPath:
		var seen claim.TaskSeenRequest
		if d.decoded(body, &seen) && !d.failed(w, r) {
			d.mu.Lock()
			d.seen = append(d.seen, seen)
			d.mu.Unlock()
			status, out := d.answerSeen(seen)
			writeAnswer(w, status, out)
		}
	case takePath:
		var take claim.TakeRequest
		if d.decoded(body, &take) && !d.failed(w, r) {
			d.mu.Lock()
			d.takes = append(d.takes, take)
			d.mu.Unlock()
			status, out := d.answerTake(take)
			writeAnswer(w, status, out)
		}
	default:
		d.t.Errorf("the bridge posted to %s, which is no route it calls", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *taskDaemon) decoded(body []byte, into any) bool {
	if err := json.Unmarshal(body, into); err != nil {
		d.t.Errorf("decode the bridge's request %s: %v", body, err)
		return false
	}
	return true
}

// failed fails the request with the fault set, if one is, and says whether it did.
func (d *taskDaemon) failed(w http.ResponseWriter, r *http.Request) bool {
	d.mu.Lock()
	fault := d.fault
	d.mu.Unlock()
	if fault == nil {
		return false
	}
	fault(w, r)
	return true
}

// writeAnswer answers status with body as JSON, or with no body for nil.
func writeAnswer(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (d *taskDaemon) setFault(fault func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fault = fault
}

// seenReports and takesMade are the seen reports and the takes that reached the stand-in and
// were not failed, in the order they came.
func (d *taskDaemon) seenReports() []claim.TaskSeenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

func (d *taskDaemon) takesMade() []claim.TakeRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.takes)
}

// takesAt is how many takes of the revision reached the stand-in.
func (d *taskDaemon) takesAt(revision uint64) int {
	n := 0
	for _, take := range d.takesMade() {
		if take.Revision == revision {
			n++
		}
	}
	return n
}

// reported is the next turn the bridge reported.
func (d *taskDaemon) reported() claim.TurnOutcomeRequest {
	d.t.Helper()
	select {
	case report := <-d.reports:
		return report
	case <-time.After(10 * time.Second):
		d.t.Fatal("the bridge reported no turn within 10s")
		return claim.TurnOutcomeRequest{}
	}
}

// takingRig is a bridge whose pane names the stand-in daemon, which it reports its turns to and
// takes its claim's task from.
type takingRig struct {
	*rig
	daemon *taskDaemon
}

// newTakingRig runs a resumed session's bridge whose pane names d at url — d's own server,
// started here, when url is "" — and which sends its seen reports and takes with calls.
func newTakingRig(t *testing.T, d *taskDaemon, url string, calls daemonCalls) *takingRig {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if url == "" {
		d.server.Start()
		url = d.server.URL
	}
	r := newRigWithEnv(t, []string{
		"LEGION_DAEMON_URL=" + url,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, calls, "-p", "--resume", sessionID)
	return &takingRig{rig: r, daemon: d}
}

// handTask hands the bridge the claim's task value as claude-envoy writes it to the inbox
// (src/legion-inbox.ts): one task line, down a connection of its own, which the bridge answers
// once its loop has taken the value. Number 0 is a deleted value.
func (r *rig) handTask(number, revision uint64) {
	r.t.Helper()
	inbox := r.dialInbox()
	id := fmt.Sprintf("task-%d-at-%d", number, revision)
	line := InboxDelivery{ID: id, Kind: InboxTask, Number: number, Epoch: testEpoch, Revision: revision}
	if err := json.NewEncoder(inbox.conn).Encode(line); err != nil {
		r.t.Fatal(err)
	}
	if answer := inbox.answer(); answer != (InboxAnswer{ID: id, OK: true}) {
		r.t.Fatalf("the inbox answered the task line %+v", answer)
	}
}

// userTexts is the text of every user message Claude Code read, in the order it read them.
func (r *rig) userTexts() []string {
	var texts []string
	for _, line := range strings.Split(r.recorded(), "\n") {
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

// pairsOf names the value each of the bridge's seen reports or takes named, as "<number> at
// <revision>", failing on one that does not name this launch's claim in the boot form or names
// another epoch.
func pairsOf[R claim.TaskSeenRequest | claim.TakeRequest](t *testing.T, requests []R) []string {
	t.Helper()
	var pairs []string
	for _, request := range requests {
		value := claim.TakeRequest(request)
		if value.BootToken != testBootToken || value.AgentCaller != (claim.AgentCaller{}) || value.Generation != 4 || value.Epoch != testEpoch {
			t.Fatalf("the bridge sent %+v, want the boot form of generation 4 at epoch %s", value, testEpoch)
		}
		pairs = append(pairs, fmt.Sprintf("%d at %d", value.Number, value.Revision))
	}
	return pairs
}

// Each value the bridge is handed is reported seen, and its task asked for once Claude Code is
// idle, once per revision: the same value relayed again — by the registration's answer and by its
// relayed frame, or after a hold refused it — asks nothing, an older revision asks nothing, and only
// the value the hold's end writes, at its next revision, is asked about again. The accepted take
// runs as a turn whose report names the task.
func TestATaskLineIsReportedSeenAndTakenOncePerRevision(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			return http.StatusOK, claim.TakeResponse{Outcome: takeRefused, Hold: "in-turn"}
		}
		return accepted("say the task")
	}
	r := newTakingRig(t, d, "", daemonCalls{})

	r.handTask(testTask, 1)
	r.logged("the daemon refused the take of task 7 (hold in-turn)")
	r.handTask(testTask, 1)
	r.quiet()

	r.handTask(testTask, 2)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reported(); report.Task != testTask || report.Outcome != claim.TurnClean || report.ProviderError != nil {
		t.Fatalf("the task's turn was reported %+v, want a clean turn of task %d", report, testTask)
	}
	r.handTask(testTask, 2)
	r.handTask(testTask, 1)
	r.quiet()

	want := []string{"7 at 1", "7 at 2"}
	if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, want) {
		t.Errorf("the bridge reported seen %q, want %q", seen, want)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, want) {
		t.Errorf("the bridge asked to take %q, want %q", takes, want)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the accepted take's text once", texts)
	}
}

// An Envoy delivery that arrives while a take of the claim's task waits for its answer starts no
// turn: it waits, and the accepted take's text starts the turn, the delivery following it as the
// turn's next step.
func TestATakenTaskRunsAheadOfAQueuedCard(t *testing.T) {
	d := newTaskDaemon(t)
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-released
		return accepted("say the task")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	inbox := r.dialInbox()

	r.handTask(testTask, 1)
	r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })
	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 waits for the take of the claim's task")
	r.quiet()

	release()
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task", "say the card"}) {
		t.Fatalf("Claude Code read %q, want the task's text and then the card", texts)
	}
	if report := d.reported(); report.Task != testTask {
		t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
	}
}

// A value handed over during a turn is reported seen at once, and its task is asked for only once
// that turn has ended; the turn that was running is reported as no task's.
func TestNoTakeWhileARunIsInProgress(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})

	r.turn("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v during a turn", takes)
	}

	r.release("r1")
	r.expect("agent_end", nil)
	if report := d.reported(); report.Task != 0 {
		t.Fatalf("the delivery's turn was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reported(); report.Task != testTask {
		t.Fatalf("the task's turn was reported %+v, want the turn of task %d", report, testTask)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
	}
}

// The report of a task's turn the provider failed names the task and how the provider failed it:
// the status Claude Code named, the provider, the model the turn's init line named, and its own
// words. A content filter's refusal names no status (testdata/refusal-turn.jsonl:5).
func TestTheTurnOutcomeNamesTheTaskAndTheProviderError(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       claim.ProviderError
	}{
		{name: "a provider failure with a status", text: "api-error-529 WAITING on the provider",
			want: claim.ProviderError{Status: 529, Provider: claudeProvider, Model: fakeClaudeModel, Retryable: true}},
		{name: "a content filter's refusal, which names none", text: "refusal WAITING on the provider",
			want: claim.ProviderError{Provider: claudeProvider, Model: fakeClaudeModel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(claim.TakeRequest) (int, any) { return accepted(tc.text) }
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			report := d.reported()
			if report.Task != testTask || report.Outcome != claim.TurnRefused || report.ProviderError == nil {
				t.Fatalf("the task's turn was reported %+v, want task %d refused with its provider error", report, testTask)
			}
			got := *report.ProviderError
			if got.Message != report.Message || !strings.Contains(got.Message, "API Error: 529 overloaded") {
				t.Fatalf("the provider error's message is %q beside the report's %q, want Claude Code's own words in both", got.Message, report.Message)
			}
			got.Message = ""
			if got != tc.want {
				t.Fatalf("the provider error is %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A seen report and a take that get no answer from the daemon are sent again, the same request,
// until it answers (departure 7 of INFRA-2811's plan): on a refused, a reset and a closed
// connection, a name that does not resolve, an attempt with no answer within its bound, and a 502,
// a 503 and a 504 that name no Legion error code. Once the daemon answers, the task is taken once
// and runs once.
func TestATakeAndASeenReportAreSentAgainOnEachListedFailure(t *testing.T) {
	proxyStatus := func(status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "upstream connect error", status) }
	}
	hijack := func(t *testing.T, w http.ResponseWriter) *net.TCPConn {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("take the request's connection: %v", err)
			return nil
		}
		return conn.(*net.TCPConn)
	}
	for _, tc := range []struct {
		name, why string
		// fault fails each request at the daemon's side; nil for the two failures a request meets
		// before it reaches the daemon, which refused and dns make.
		fault        func(t *testing.T) func(http.ResponseWriter, *http.Request)
		refused, dns bool
	}{
		{name: "a refused connection", why: "the connection was refused", refused: true},
		{name: "a reset connection", why: "the connection was reset", fault: func(t *testing.T) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				if conn := hijack(t, w); conn != nil {
					_ = conn.SetLinger(0)
					_ = conn.Close()
				}
			}
		}},
		{name: "a closed connection", why: "the connection was closed", fault: func(t *testing.T) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				if conn := hijack(t, w); conn != nil {
					_ = conn.Close()
				}
			}
		}},
		{name: "a name that does not resolve", why: "the daemon's name did not resolve", dns: true},
		{name: "no answer within the attempt's bound", why: "no answer within 200ms", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
		}},
		{name: "a 502 with no code", why: "502 with no Legion error code", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusBadGateway)
		}},
		{name: "a 503 with no code", why: "503 with no Legion error code", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusServiceUnavailable)
		}},
		{name: "a 504 with no code", why: "504 with no Legion error code", fault: func(*testing.T) func(http.ResponseWriter, *http.Request) {
			return proxyStatus(http.StatusGatewayTimeout)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			calls, url := quickCalls, ""
			var lift func()
			switch {
			case tc.refused:
				// The daemon's address, which nothing listens on until the daemon starts.
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := listener.Addr().String()
				_ = listener.Close()
				url = "http://" + addr
				lift = func() { d.startAt(addr) }
			case tc.dns:
				var mu sync.Mutex
				resolves := false
				calls.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					mu.Lock()
					up := resolves
					mu.Unlock()
					if !up {
						return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: "legion-daemon", IsNotFound: true}}
					}
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, address)
				}
				lift = func() {
					mu.Lock()
					defer mu.Unlock()
					resolves = true
				}
			default:
				d.setFault(tc.fault(t))
				lift = func() { d.setFault(nil) }
			}
			r := newTakingRig(t, d, url, calls)

			r.handTask(testTask, 1)
			r.logged("claims/task-seen of task 7 got no answer from the daemon (" + tc.why + ")")
			r.logged("claims/take of task 7 got no answer from the daemon (" + tc.why + ")")
			lift()
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			r.quiet()

			if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, []string{"7 at 1"}) {
				t.Errorf("the daemon answered the seen reports %q, want one", seen)
			}
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
				t.Errorf("the daemon answered the takes %q, want one", takes)
			}
			if report := d.reported(); report.Task != testTask {
				t.Errorf("the turn was reported %+v, want the turn of task %d", report, testTask)
			}
			if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
				t.Errorf("Claude Code read %q, want the task's text once", texts)
			}
		})
	}
}

// A refusal the daemon made names a Legion error code, whatever its status, and is an answer: a
// seen report and a take refused so are sent once and never again, and the take holds the queue
// back no more.
func TestACodedRefusalIsNotSentAgain(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerSeen = func(claim.TaskSeenRequest) (int, any) {
		return http.StatusConflict, map[string]string{"code": "claim_superseded", "error": "this launch is not the claim's current one"}
	}
	d.answerTake = func(claim.TakeRequest) (int, any) {
		return http.StatusServiceUnavailable, map[string]string{"code": "daemon_stopping", "error": "the daemon is stopping"}
	}
	r := newTakingRig(t, d, "", quickCalls)
	inbox := r.dialInbox()

	r.handTask(testTask, 1)
	r.logged("the take of task 7 failed (the daemon refused it: 503 daemon_stopping: the daemon is stopping)")
	r.logged("the daemon refused the seen report of task 7 at revision 1: 409 claim_superseded")
	// Ten of the longest waits a re-send would take pass, and nothing is sent again.
	time.Sleep(10 * quickCalls.resendMax)
	if seen, takes := len(d.seenReports()), len(d.takesMade()); seen != 1 || takes != 1 {
		t.Fatalf("the daemon was sent %d seen reports and %d takes, want one of each", seen, takes)
	}
	if strings.Contains(r.log.String(), "got no answer") {
		t.Fatalf("a coded refusal was taken for no answer:\n%s", r.log)
	}

	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 starts a turn")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
}

// A newer task line replaces the take still being sent for the older one: the older take is sent no
// more, whether the newer value names a task — taken and run ahead of the card that waited — or the
// value was deleted, which lets the card go.
func TestANewerTaskLineEndsTheResendOfTheOlderTake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		number uint64
		texts  []string
	}{
		{name: "a newer value", number: testTask + 1, texts: []string{"say the newer task", "say the card"}},
		{name: "a deleted value", number: 0, texts: []string{"say the card"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(take claim.TakeRequest) (int, any) {
				if take.Revision == 1 {
					return http.StatusServiceUnavailable, nil
				}
				return accepted("say the newer task")
			}
			r := newTakingRig(t, d, "", quickCalls)
			inbox := r.dialInbox()

			r.handTask(testTask, 1)
			r.waitFor("the take to be sent again", func() bool { return d.takesAt(1) >= 2 })
			inbox.deliver("ev1", "say the card")
			r.logged("Envoy delivery ev1 waits for the take of the claim's task")

			r.handTask(tc.number, 2)
			r.logged("the take of task 7 at revision 1 is replaced by a newer value and sent no more")
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			older := d.takesAt(1)
			// Ten of the longest waits a re-send would take pass, and the older take is not sent.
			time.Sleep(10 * quickCalls.resendMax)
			if again := d.takesAt(1); again != older {
				t.Fatalf("the take at revision 1 was sent %d times more after the newer value", again-older)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Fatalf("Claude Code read %q, want %q", texts, tc.texts)
			}
		})
	}
}
