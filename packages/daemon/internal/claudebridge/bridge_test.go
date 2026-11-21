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
	"net/url"
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
//   - "models <model>…": an assistant message of text for each model named, each naming its own
//     model, as Claude Code names the model that wrote each message;
//   - "held <name>": the re-emission only once the test releases name (rig.release);
//   - "wait <name>": an assistant message "done" only once the test releases name;
//   - "die": exits, never re-emitting it;
//   - "drop <name>": once the test releases name, a success result and no re-emission: the step
//     Claude Code ended without taking the message, as the bridge reads a message Claude Code
//     dropped (errNotTaken). No recording shows Claude Code dropping a stdin message; this is the
//     bridge's own reading of one, the turn end it acts on;
//   - "fail <text>": an assistant message of text, then an error_during_execution result, as a
//     turn a tool failed in ends, which is no provider failure;
//   - "api-error-529 <text>": the same text, then the result a provider failure ends a turn with
//     — `terminal_reason` "api_error" and `api_error_status` 529 — as the recordings carry it
//     (testdata/api-error-529.jsonl:5);
//   - "unechoed-api-error-529 <text>": the same, with no re-emission of the message: a provider
//     failure that ends the turn before Claude Code re-emits it, which no recording shows or rules
//     out;
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
		if name, ok := strings.CutPrefix(body, "drop "); ok {
			released(name)
			result("success", false)
			continue
		}
		if rest, ok := strings.CutPrefix(body, "unechoed-api-error-529 "); ok {
			assistant(text(rest))
			providerFailed(529)
			continue
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
		case strings.HasPrefix(body, "models "):
			for _, model := range strings.Fields(strings.TrimPrefix(body, "models ")) {
				emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": model,
					"content": []map[string]any{text("answered")}}})
			}
		case strings.HasPrefix(body, "subagent "):
			// The main conversation's model calls a subagent, which answers on a model of its
			// own: Claude Code writes the subagent's messages as assistant lines that name the
			// tool call they serve in parent_tool_use_id ("Follow subagent messages").
			models := strings.Fields(strings.TrimPrefix(body, "subagent "))
			emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"role": "assistant", "model": models[0],
				"content": []map[string]any{{"type": "tool_use", "id": "tu-agent", "name": "Agent", "input": map[string]any{"prompt": "look"}}}}})
			emit(map[string]any{"type": "assistant", "parent_tool_use_id": "tu-agent", "message": map[string]any{"role": "assistant", "model": models[1],
				"content": []map[string]any{text("the subagent's answer")}}})
			emit(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tu-agent"}}}})
			emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"role": "assistant", "model": models[0],
				"content": []map[string]any{text("answered")}}})
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
// (daemonlink.go); and calls, the timing and network of every request the bridge sends — its seen
// reports, its takes and the re-sends of its turn-outcome reports — the zero value being
// production's.
func newRigWithEnv(t *testing.T, extra []string, calls daemonCalls, args ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	// Run opens the inbox under the process TMPDIR, so the rig places that directory itself, with
	// room for the inbox's socket.
	t.Setenv("TMPDIR", testtmp.Dir(t, InboxLongestPath))
	record, gate := filepath.Join(dir, "record"), filepath.Join(dir, "gate")
	argv := append([]string{os.Args[0]}, args...)
	// The daemon a pane names is the case's to give (newTakingRig, newReportingRig): one the test
	// process inherited, a Legion pod's own, would make every rig a claim's, whose ready gate holds
	// its deliveries until a ready line no case writes.
	inherited := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "LEGION_DAEMON_URL" || name == "LEGION_BOOT_TOKEN_FILE" || name == "LEGION_GENERATION"
	})
	env := append(inherited, fakeClaudeEnv+"=1", fakeClaudeRecordEnv+"="+record, fakeClaudeGateEnv+"="+gate,
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

// nextWithSummaries is the next frame the bridge wrote that is not the name of the model that
// answered (answerFrame), pane summaries (tool_execution_start) included.
func (r *rig) nextWithSummaries() map[string]any {
	r.t.Helper()
	for {
		if frame := r.nextRaw(); !answerFrame(frame) {
			return frame
		}
	}
}

// next is nextWithSummaries without the pane summaries: the cases that read next are about turns.
func (r *rig) next() map[string]any {
	r.t.Helper()
	for {
		if frame := r.nextWithSummaries(); frame["type"] != "tool_execution_start" {
			return frame
		}
	}
}

// answerFrame is whether frame is the message_end the bridge writes for each answer of a model,
// naming that model; the message_end of a failed turn names none.
func answerFrame(frame map[string]any) bool {
	message, _ := frame["message"].(map[string]any)
	_, named := message["model"]
	return frame["type"] == "message_end" && named
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

// quiet holds that the bridge writes no frame within a short while, but the name of a model that
// answered (answerFrame), which says nothing about a turn.
func (r *rig) quiet() {
	r.t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case frame := <-r.frames:
			if !answerFrame(frame) {
				r.t.Fatalf("got %v, want no frame", frame)
			}
		case <-deadline:
			return
		}
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
		frame := r.nextWithSummaries()
		typ, _ := frame["type"].(string)
		types = append(types, typ)
		if typ == "agent_end" {
			ends++
		}
	}
	// The first turn carries one tool call, the background Bash command, which the bridge shows
	// on the pane; the turn Claude Code started by itself calls no tool, so the bridge writes
	// nothing of its own between that turn's start and its end but the names of the model that
	// answered in it, which TestTheBridgeNamesClaudesModel reads.
	want := []string{"agent_start", "tool_execution_start", "agent_end", "agent_start", "agent_end"}
	if !slices.Equal(types, want) {
		t.Fatalf("the daemon saw %q, want %q", types, want)
	}
	r.quiet()
}

// Each answer of Claude Code's model reaches the daemon as Oh My Pi's message_end names one: the
// model under message.model, the provider anthropic. That is what the record of a step names as
// the model that served it, so each message names the model that wrote it: two models answering
// in one turn are named in their order, neither as the model of the recording nor as the one before.
// A message no model wrote — Claude Code's `<synthetic>` answer to a provider's 429 — names no model.
func TestTheBridgeNamesClaudesModel(t *testing.T) {
	framesUntilTurnEnds := func(r *rig) []map[string]any {
		var frames []map[string]any
		for {
			frame := r.nextRaw()
			frames = append(frames, frame)
			if frame["type"] == "agent_end" {
				return frames
			}
		}
	}

	r := newRig(t, "-p", "--resume", sessionID)
	r.turn("p1", backgroundTurn)
	answers := 0
	for _, frame := range framesUntilTurnEnds(r) {
		if frame["type"] != "message_end" {
			continue
		}
		message, _ := frame["message"].(map[string]any)
		if message["role"] != "assistant" || message["provider"] != "anthropic" || message["model"] != "claude-sonnet-4-5" {
			t.Fatalf("the bridge wrote %v, want a message_end naming anthropic's claude-sonnet-4-5", frame)
		}
		answers++
	}
	if answers == 0 {
		t.Fatal("the turn the model answered wrote no message_end naming its model")
	}

	changed := newRig(t, "-p", "--resume", sessionID)
	changed.turn("p1", "models claude-first-1 claude-second-2")
	var named []any
	for _, frame := range framesUntilTurnEnds(changed) {
		if answerFrame(frame) {
			message, _ := frame["message"].(map[string]any)
			named = append(named, message["model"])
		}
	}
	if want := []any{"claude-first-1", "claude-second-2"}; !slices.Equal(named, want) {
		t.Fatalf("a turn two models answered in names %v, want %v: each message names the model that wrote it", named, want)
	}

	// A subagent answers on a model of its own, and its messages are no answer of the worker's: a
	// turn whose main model calls one names the main model alone.
	parent := newRig(t, "-p", "--resume", sessionID)
	parent.turn("p1", "subagent claude-sonnet-5 claude-haiku-4-5")
	named = nil
	for _, frame := range framesUntilTurnEnds(parent) {
		if answerFrame(frame) {
			message, _ := frame["message"].(map[string]any)
			named = append(named, message["model"])
		}
	}
	if want := []any{"claude-sonnet-5", "claude-sonnet-5"}; !slices.Equal(named, want) {
		t.Fatalf("a turn whose model called a subagent on claude-haiku-4-5 names %v, want %v: a subagent's message is no answer of the worker's", named, want)
	}

	failed := newRig(t, "-p", "--resume", sessionID)
	failed.turn("p1", "replay api-error-429.jsonl")
	failed.open()
	for _, frame := range framesUntilTurnEnds(failed) {
		message, _ := frame["message"].(map[string]any)
		if frame["type"] == "message_end" && message["model"] != nil {
			t.Fatalf("the turn no model answered wrote %v, want no message_end naming a model", frame)
		}
	}
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
		frame := r.nextWithSummaries()
		if frame["type"] != "tool_execution_start" {
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
// and records each turn report it answered 200, which a case reads by its sequence (reportOf).
// While fault is set it fails every seen report and take that reaches it with fault instead: the
// failures of departure 7 a daemon's side makes. Each turn-outcome attempt is recorded, and
// answered by the first of reportAnswers the case queued, if any, in place of the 200.
type taskDaemon struct {
	t      *testing.T
	server *httptest.Server
	// answerSeen and answerTake answer a seen report and a take that is not failed: 204, and an
	// accepted take of "say the task", unless the case sets its own before the stand-in starts.
	answerSeen func(claim.TaskSeenRequest) (int, any)
	answerTake func(claim.TakeRequest) (int, any)

	mu             sync.Mutex
	fault          func(http.ResponseWriter, *http.Request)
	seen           []claim.TaskSeenRequest
	takes          []claim.TakeRequest
	reports        []claim.TurnOutcomeRequest
	reportAttempts []claim.TurnOutcomeRequest
	reportAnswers  []func(http.ResponseWriter, *http.Request)
}

// newTaskDaemon is a stand-in not yet serving: newTakingRig starts it, or a case starts it itself.
func newTaskDaemon(t *testing.T) *taskDaemon {
	t.Helper()
	d := &taskDaemon{t: t,
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
		if !d.decoded(body, &report) {
			return
		}
		d.mu.Lock()
		d.reportAttempts = append(d.reportAttempts, report)
		var answer func(http.ResponseWriter, *http.Request)
		if len(d.reportAnswers) > 0 {
			answer, d.reportAnswers = d.reportAnswers[0], d.reportAnswers[1:]
		}
		d.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		d.mu.Lock()
		d.reports = append(d.reports, report)
		d.mu.Unlock()
		writeAnswer(w, http.StatusOK, claim.TurnOutcomeResponse{})
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

// answerReports queues the answers of the next turn-outcome attempts, in the order they come; an
// attempt with none queued is answered 200 and handed to the case (reported).
func (d *taskDaemon) answerReports(answers ...func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reportAnswers = append(d.reportAnswers, answers...)
}

// reportsAttempted is every turn-outcome attempt that reached the stand-in, however it was
// answered, in the order they came.
func (d *taskDaemon) reportsAttempted() []claim.TurnOutcomeRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reportAttempts)
}

// hijack takes the connection of the request w answers, for a case that ends it as a daemon that
// is not serving does.
func hijack(t *testing.T, w http.ResponseWriter) *net.TCPConn {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("take the request's connection: %v", err)
		return nil
	}
	return conn.(*net.TCPConn)
}

// resetConnection answers a request with a reset connection, its answer lost.
func resetConnection(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if conn := hijack(t, w); conn != nil {
			_ = conn.SetLinger(0)
			_ = conn.Close()
		}
	}
}

// timedOutConn is a connection whose far side stopped answering: what is written to it goes
// nowhere, and every read meets the kernel's timeout of the connection (ETIMEDOUT), as net wraps
// it. The embedded end of a pipe answers the rest of net.Conn.
type timedOutConn struct{ net.Conn }

func (timedOutConn) Write(b []byte) (int, error) { return len(b), nil }

func (timedOutConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
}

// reportOf is the report of the bridge's turn sequence, which the stand-in answered 200, once it
// has come, within 10 s. A report is read by its sequence, as the daemon keys it, and not by the
// order reports arrive in: the bridge sends each from a goroutine of its own, on a connection of
// its own (turnoutcome.go), so the reports of two turns that end close together, such as a turn's
// and that of the held task's turn it starts at once, can reach the stand-in either way round.
func (d *taskDaemon) reportOf(sequence uint64) claim.TurnOutcomeRequest {
	d.t.Helper()
	var report claim.TurnOutcomeRequest
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, r := range d.reports {
			if r.Sequence == sequence {
				report = r
				return true
			}
		}
		return false
	}) {
		d.t.Fatalf("the bridge reported no turn %d within 10s", sequence)
	}
	return report
}

// newTakingRig runs a resumed session's bridge whose pane names d at url — d's own server,
// started here, when url is "" — and which sends its seen reports and takes with calls. It is a
// plain *rig: the stand-in daemon is the d the caller already holds, so the rig keeps no second
// handle to it.
func newTakingRig(t *testing.T, d *taskDaemon, url string, calls daemonCalls) *rig {
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
	return r
}

// handLine writes line to the inbox as claude-envoy writes its lines (src/legion-inbox.ts), down a
// connection of its own, closed once the inbox has answered, and is the inbox's answer to it.
func (r *rig) handLine(line InboxDelivery) InboxAnswer {
	r.t.Helper()
	inbox := r.dialInbox()
	defer func() { _ = inbox.conn.Close() }()
	if err := json.NewEncoder(inbox.conn).Encode(line); err != nil {
		r.t.Fatal(err)
	}
	return inbox.answer()
}

// handTask hands the bridge the claim's task value as claude-envoy writes a relayed one to the
// inbox: one task line, finished 0 since a relay frame carries no reading, which the bridge
// answers once its loop has taken the value. Number 0 is a deleted value.
func (r *rig) handTask(number, revision uint64) {
	r.t.Helper()
	r.handReading(number, revision, 0)
}

// handReading hands the bridge the value a reading of the daemon's carried — the registration's,
// or claims/task's — with the number that reading says the claim finished.
func (r *rig) handReading(number, revision, finished uint64) {
	r.t.Helper()
	id := fmt.Sprintf("task-%d-at-%d", number, revision)
	answer := r.handLine(InboxDelivery{ID: id, Kind: InboxTask, Number: number, Epoch: testEpoch, Revision: revision, Finished: &finished})
	if answer != (InboxAnswer{ID: id, OK: true}) {
		r.t.Fatalf("the inbox answered the task line %+v", answer)
	}
}

// handFinished hands the bridge a reading of the daemon's that held no value, as claude-envoy
// writes one to the inbox: one finished line naming the number the reading says the claim
// finished.
func (r *rig) handFinished(finished uint64) {
	r.t.Helper()
	id := fmt.Sprintf("finished-%d", finished)
	if answer := r.handLine(InboxDelivery{ID: id, Kind: InboxFinished, Finished: &finished}); answer != (InboxAnswer{ID: id, OK: true}) {
		r.t.Fatalf("the inbox answered the finished line %+v", answer)
	}
}

// handReady writes the ready line, as claude-envoy does once the claim's ready has answered, and
// returns once the bridge's loop has taken it.
func (r *rig) handReady() {
	r.t.Helper()
	if answer := r.handLine(InboxDelivery{ID: "ready", Kind: InboxReady}); answer != (InboxAnswer{ID: "ready", OK: true}) {
		r.t.Fatalf("the inbox answered the ready line %+v", answer)
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
	r.handReady()

	r.handTask(testTask, 1)
	r.logged("the daemon refused the take of task 7 (hold in-turn)")
	r.handTask(testTask, 1)
	r.quiet()

	r.handTask(testTask, 2)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean || report.ProviderError != nil {
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
	r.handReady()
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
	if report := d.reportOf(1); report.Task != testTask {
		t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
	}
}

// A value handed over during a turn is reported seen at once, and its task is asked for only once
// that turn has ended; the turn that was running is reported as no task's.
func TestNoTakeWhileARunIsInProgress(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

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
	if report := d.reportOf(1); report.Task != 0 {
		t.Fatalf("the delivery's turn was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(2); report.Task != testTask {
		t.Fatalf("the task's turn was reported %+v, want the turn of task %d", report, testTask)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
	}
}

// The report of a task's turn the provider failed names the task and how the provider failed it:
// the status Claude Code named, the provider, the model the turn's init line named, and its own
// words. A content filter's refusal names no status (testdata/refusal-turn.jsonl:5). A provider
// failure that ends the turn before Claude Code re-emits the task's text names the task too,
// whether or not the text reached Claude Code: its report takes the task to the daemon's
// provider-error ladder, which gives it again after its wait, rather than counting the refusal
// toward the launch-failure run (result).
func TestTheTurnOutcomeNamesTheTaskAndTheProviderError(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       claim.ProviderError
	}{
		{name: "a provider failure with a status", text: "api-error-529 WAITING on the provider",
			want: claim.ProviderError{Status: 529, Provider: claudeProvider, Model: fakeClaudeModel, Retryable: true}},
		{name: "a content filter's refusal, which names none", text: "refusal WAITING on the provider",
			want: claim.ProviderError{Provider: claudeProvider, Model: fakeClaudeModel}},
		{name: "a provider failure before Claude Code re-emitted the text", text: "unechoed-api-error-529 WAITING on the provider",
			want: claim.ProviderError{Status: 529, Provider: claudeProvider, Model: fakeClaudeModel, Retryable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(claim.TakeRequest) (int, any) { return accepted(tc.text) }
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()
			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			report := d.reportOf(1)
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
// until it answers (dispatch://INFRA-2811): on a refused, a reset and a closed connection, an
// unreachable host and network, a name that does not resolve, a dial and a read that timed out,
// an attempt with no answer within its bound, and a 502, a 503 and a 504 that name no Legion
// error code. Once the daemon answers, the task is taken once and runs once.
func TestATakeAndASeenReportAreSentAgainOnEachListedFailure(t *testing.T) {
	proxyStatus := func(status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "upstream connect error", status) }
	}
	// failedDial is a dial that fails as net.Dial fails with err, before the request leaves.
	failedDial := func(err error) func(context.Context, string, string) (net.Conn, error) {
		return func(_ context.Context, network, _ string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
	}
	for _, tc := range []struct {
		name, why string
		// fault fails each request at the daemon's side; nil for a failure a request meets
		// before it reaches the daemon, which refused and dial make.
		fault   func(t *testing.T) func(http.ResponseWriter, *http.Request)
		refused bool
		// dial is what a dial of the daemon meets until the failure lifts.
		dial func(ctx context.Context, network, address string) (net.Conn, error)
	}{
		{name: "a refused connection", why: "the connection was refused", refused: true},
		{name: "a reset connection", why: "the connection was reset", fault: resetConnection},
		{name: "a closed connection", why: "the connection was closed", fault: func(t *testing.T) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				if conn := hijack(t, w); conn != nil {
					_ = conn.Close()
				}
			}
		}},
		{name: "an unreachable host", why: "the daemon's host was unreachable",
			dial: failedDial(os.NewSyscallError("connect", syscall.EHOSTUNREACH))},
		{name: "an unreachable network", why: "the daemon's network was unreachable",
			dial: failedDial(os.NewSyscallError("connect", syscall.ENETUNREACH))},
		{name: "a name that does not resolve", why: "the daemon's name did not resolve",
			dial: failedDial(&net.DNSError{Err: "no such host", Name: "legion-daemon", IsNotFound: true})},
		// The dialer's own timeout, as net.Dialer makes it: a deadline already past.
		{name: "a dial that timed out", why: "the connection timed out",
			dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				dialer := net.Dialer{Deadline: time.Now().Add(-time.Second)}
				return dialer.DialContext(ctx, network, address)
			}},
		// The kernel's timeout of a connection whose far side stopped answering, met at the
		// read of the answer; the request goes nowhere.
		{name: "a read that timed out", why: "the connection timed out",
			dial: func(context.Context, string, string) (net.Conn, error) {
				local, far := net.Pipe()
				_ = far.Close()
				return timedOutConn{local}, nil
			}},
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
			case tc.dial != nil:
				var mu sync.Mutex
				lifted := false
				calls.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					mu.Lock()
					up := lifted
					mu.Unlock()
					if !up {
						return tc.dial(ctx, network, address)
					}
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, address)
				}
				lift = func() {
					mu.Lock()
					defer mu.Unlock()
					lifted = true
				}
			default:
				d.setFault(tc.fault(t))
				lift = func() { d.setFault(nil) }
			}
			r := newTakingRig(t, d, url, calls)
			r.handReady()

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
			if report := d.reportOf(1); report.Task != testTask {
				t.Errorf("the turn was reported %+v, want the turn of task %d", report, testTask)
			}
			if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
				t.Errorf("Claude Code read %q, want the task's text once", texts)
			}
		})
	}
}

// One read that timed out reaches the bridge in either of two shapes, as net/http's transport
// decides by a race between its read and write loops: the read's own error under the client's
// *url.Error, or that error wrapped first as "readLoopPeekFailLocked: %w". The tester's probe in
// round 4 of dispatch://INFRA-2811, 400 attempts of the bridge's own attempt through
// timedOutConn, met the second shape 1 to 2 times in 400, and TestATakeAndASeenReportAreSentAgainOnEachListedFailure's
// "a read that timed out" fails whenever it meets it. Both are a read that timed out, which the
// bridge sends again until the daemon answers; a take sent once and lost so is charged at the
// first bound once its seen report reached the daemon.
func TestAReadThatTimedOutIsSentAgainInEitherShapeTheTransportGivesIt(t *testing.T) {
	read := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
	d := &daemonLink{calls: daemonCalls{answerTimeout: time.Second}}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "the read's own error", err: &url.Error{Op: "Post", URL: "http://daemon/legion/v1/claims/take", Err: read}},
		{name: "the read's error wrapped by the transport's read loop", err: &url.Error{Op: "Post", URL: "http://daemon/legion/v1/claims/take",
			Err: fmt.Errorf("readLoopPeekFailLocked: %w", read)}},
	} {
		if why := d.unanswered(0, nil, tc.err); why != "the connection timed out" {
			t.Errorf("%s (%v): unanswered = %q, want \"the connection timed out\": a read that timed out is sent again until the daemon answers", tc.name, tc.err, why)
		}
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
	r.handReady()
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
			r.handReady()
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

// The end report of a task's turn is what finishes the task, so it is sent until the daemon
// answers it (the plan's decision 8): its first attempt, lost to a reset connection, and its
// second, which the daemon answered 500 — its failure to record the report — are sent again, the
// same report, and the third lands. The task finishes once: one report of its turn is recorded,
// and its text ran once.
func TestTheFirstEndReportDroppedThenTheTaskFinishesOnce(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerReports(resetConnection(t), func(w http.ResponseWriter, _ *http.Request) {
		writeAnswer(w, http.StatusInternalServerError, map[string]string{"error": "turn outcome failed: the daemon could not record it"})
	})
	r := newTakingRig(t, d, "", quickCalls)
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.logged("claims/turn-outcome of turn 1 got no answer from the daemon (the connection was reset)")
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean {
		t.Fatalf("the daemon recorded %+v, want the clean report of task %d's turn, the bridge's first", report, testTask)
	}
	r.quiet()

	attempts := d.reportsAttempted()
	if len(attempts) != 3 || attempts[1] != attempts[0] || attempts[2] != attempts[0] {
		t.Fatalf("the bridge sent the reports %+v, want the one report three times", attempts)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Errorf("the bridge asked to take %q, want task 7 once", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the task's text once", texts)
	}
}

// An end report whose attempt fails with no answer the daemon gave is sent again whatever the
// transport names the failure, not only where unanswered's list names it: an attempt that never
// reached the daemon applied nothing, and one that did is applied once. Here something in the
// daemon's place answers what is no HTTP response, and then a response whose header is malformed,
// two failures no case of the list names; the third attempt lands, and the task finishes once.
func TestAnEndReportIsSentAgainOnATransportFailureNoListNames(t *testing.T) {
	notHTTP := func(answer string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			if conn := hijack(t, w); conn != nil {
				_, _ = conn.Write([]byte(answer))
				_ = conn.Close()
			}
		}
	}
	d := newTaskDaemon(t)
	d.answerReports(notHTTP("not an answer of HTTP\r\n\r\n"), notHTTP("HTTP/1.1 200 OK\r\nContent-Length: many\r\n\r\n"))
	r := newTakingRig(t, d, "", quickCalls)
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.logged("claims/turn-outcome of turn 1 got no answer from the daemon (the transport failed:")
	if report := d.reportOf(1); report.Task != testTask || report.Outcome != claim.TurnClean {
		t.Fatalf("the daemon recorded %+v, want the clean report of task %d's turn", report, testTask)
	}
	r.quiet()

	attempts := d.reportsAttempted()
	if len(attempts) != 3 || attempts[1] != attempts[0] || attempts[2] != attempts[0] {
		t.Fatalf("the bridge sent the reports %+v, want the one report three times", attempts)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task"}) {
		t.Errorf("Claude Code read %q, want the task's text once", texts)
	}
}

// runFollowedByOneOfItsOwn is the turn of the card "wait r1", which the case holds open, whose
// next step is a card that replays a recorded stream: that stream ends the card's turn and then has
// Claude Code start a turn of its own (backgroundTurn), whose result the gate holds back. A value
// the case hands during the card's turn is asked for once that turn ends, and the turn Claude Code
// starts by itself begins while that take waits for its answer.
func (r *rig) runFollowedByOneOfItsOwn(d *taskDaemon, inbox *inboxClient) {
	r.t.Helper()
	inbox.deliver("ev2", backgroundTurn)
	r.logged("Envoy delivery ev2 waits for the next step")
	r.release("r1")
	r.expect("agent_end", nil)
	if report := d.reportOf(1); report.Task != 0 {
		r.t.Fatalf("the card's turn was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.logged("Claude Code started a turn of its own")
}

// A take the daemon accepts while Claude Code is in a turn starts nothing in it (the plan's
// decision 9): the take, asked for between two turns, is answered `accepted` during the turn
// Claude Code started by itself after it, which the daemon gave the take as the first to start
// after its accept. The text is held, that turn is reported as no task's and as holding the
// take's text unrun (held) — which puts the take back to awaiting its turn on the daemon, also
// where its count of the process's turns is not known —, and the text starts a turn of its own
// once Claude Code is idle, whose report names the task and holds nothing. A report before the
// take's answer came holds nothing either.
func TestATakeAcceptedDuringARunStartsNothingInIt(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-answered
		return accepted("say the task")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()
	inbox := r.dialInbox()

	inbox.deliver("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.runFollowedByOneOfItsOwn(d, inbox)
	r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })

	answer()
	r.logged("the daemon accepted the take of task 7 during a turn")
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 || report.Held != testTask {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task holding task %d", report, testTask)
	}
	if report := d.reportOf(1); report.Held != 0 {
		t.Fatalf("the card's turn, before the take's answer, was reported %+v, want it holding nothing", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask || report.Held != 0 {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d holding nothing", report, testTask)
	}
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the task's text", texts)
	}
}

// The lost-answer case (the plan's departure 7 and decision 9): the take's first attempt is lost
// to a reset connection, and while it waits to be sent again Claude Code starts a turn of its own;
// the daemon answers the take sent again `accepted` — a take of the accepted number is answered
// the same text until the task finishes — during that turn. The text is held, that turn is
// reported as no task's, and the text runs next, once.
func TestALostAnswersRepeatAcceptedDuringARunIsHeldAndRunsNextOnce(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(claim.TakeRequest) (int, any) {
		<-answered
		return accepted("say the task")
	}
	// Quick waits between attempts, and an attempt bound longer than the case: the attempt the
	// daemon holds until the case answers it is the one that is answered.
	calls := quickCalls
	calls.answerTimeout = time.Minute
	r := newTakingRig(t, d, "", calls)
	r.handReady()
	inbox := r.dialInbox()

	inbox.deliver("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
	reset := resetConnection(t)
	d.setFault(func(w http.ResponseWriter, req *http.Request) {
		d.setFault(nil)
		reset(w, req)
	})
	r.runFollowedByOneOfItsOwn(d, inbox)
	r.logged("claims/take of task 7 got no answer from the daemon (the connection was reset)")
	r.waitFor("the take sent again to reach the daemon", func() bool { return len(d.takesMade()) == 1 })

	answer()
	r.logged("the daemon accepted the take of task 7 during a turn")
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the task's text once", texts)
	}
}

// The plan's decision 9: a number held as accepted is a new offer at a newer revision of it only
// while its text has not reached Claude Code. A text held through a turn the bridge did not start
// was never written to it: where the daemon writes the number's value again meanwhile — it took the
// task back, or only wrote the value again —, that turn's end starts nothing of the held text, the
// take is made at the newer revision once Claude Code is idle, and the text that answer carries
// runs, once.
func TestTakeOfATextHeldThroughATurnIsMadeAgainAtANewerRevisionAndRunsOnce(t *testing.T) {
	d := newTaskDaemon(t)
	answered := make(chan struct{})
	answer := sync.OnceFunc(func() { close(answered) })
	t.Cleanup(answer)
	d.answerTake = func(take claim.TakeRequest) (int, any) {
		if take.Revision == 1 {
			<-answered
			return accepted("say the task")
		}
		return accepted("say the task taken again")
	}
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()
	inbox := r.dialInbox()

	inbox.deliver("ev1", "wait r1")
	r.expect("agent_start", nil)
	r.handTask(testTask, 1)
	r.runFollowedByOneOfItsOwn(d, inbox)
	r.waitFor("the take to reach the daemon", func() bool { return len(d.takesMade()) == 1 })
	answer()
	r.logged("the daemon accepted the take of task 7 during a turn")

	// The daemon writes the number's value again while the text is held: the value is reported
	// seen at once, and asked about only once the turn has ended.
	r.handTask(testTask, 2)
	r.waitFor("the newer value to be reported seen", func() bool { return len(d.seenReports()) == 2 })
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	if report := d.reportOf(2); report.Task != 0 {
		t.Fatalf("the turn Claude Code started by itself was reported %+v, want a turn of no task", report)
	}
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if report := d.reportOf(3); report.Task != testTask {
		t.Fatalf("the turn after it was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1", "7 at 2"}) {
		t.Errorf("the bridge asked to take %q, want task 7 at revision 1 and again at revision 2", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"wait r1", backgroundTurn, "say the task taken again"}) {
		t.Fatalf("Claude Code read %q, want the two cards and then the newer answer's text once", texts)
	}
}

// Once the text is written to Claude Code a newer revision of its number changes nothing, also
// while Claude Code has not taken the text yet: the bridge starts the task's turn as it writes the
// text (startTask), asks nothing while that turn runs, and holds the number finished from the
// turn's end (the run-end mark). The text runs once (plan review round 68: a guard keyed on Claude
// Code having begun the turn would hand the text a second time here).
func TestTakeOfANewerRevisionBeforeClaudeCodeTakesTheTextHandsNothingMore(t *testing.T) {
	d := newTaskDaemon(t)
	d.answerTake = func(claim.TakeRequest) (int, any) { return accepted("held r1") }
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handTask(testTask, 1)
	r.expect("agent_start", nil)
	r.handTask(testTask, 2)
	r.waitFor("the newer value to be reported seen", func() bool { return len(d.seenReports()) == 2 })
	r.quiet()

	r.release("r1")
	r.untilTurnEnds()
	if report := d.reportOf(1); report.Task != testTask {
		t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
	}
	r.quiet()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
		t.Errorf("the bridge asked to take %q, want task 7 once", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"held r1"}) {
		t.Fatalf("Claude Code read %q, want the task's text once", texts)
	}
}

// The plan's decision 9 in the bridge: the task's text has reached Claude Code only once Claude
// Code re-emits it (--replay-user-messages), as an Envoy delivery has. A task's turn that ends with
// no echo of the task's own text is a text Claude Code dropped: that turn is reported naming no
// task, which puts the take back on the daemon (decision 8), nothing is held finished, and the
// number is taken again at its value's next revision, whose accepted text is written once, at
// idle, and runs once, re-emitted, its turn's report naming the number. An Envoy delivery's echo
// in the dropped text's turn is no echo of the task's.
func TestATaskTextClaudeCodeDoesNotReEmitIsTakenAgainAndRunsOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		card  bool
		texts []string
	}{
		{name: "no echo of the text", texts: []string{"drop r1", "say the task taken again"}},
		{name: "an Envoy delivery's echo only", card: true, texts: []string{"drop r1", "say the card", "say the task taken again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			d.answerTake = func(take claim.TakeRequest) (int, any) {
				if take.Revision == 1 {
					return accepted("drop r1")
				}
				return accepted("say the task taken again")
			}
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()
			inbox := r.dialInbox()

			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			if tc.card {
				inbox.deliver("ev1", "say the card")
				r.logged("Envoy delivery ev1 waits for the next step")
			}
			r.release("r1")
			if tc.card {
				if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
					t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
				}
			}
			r.untilTurnEnds()
			if report := d.reportOf(1); report.Task != 0 {
				t.Fatalf("the turn whose text Claude Code never re-emitted was reported %+v, want a turn of no task", report)
			}
			r.logged("Claude Code ended task 7's turn without re-emitting its text")
			r.quiet()

			// The daemon writes the number's value again, as it does once it has taken the task
			// back: the take is made at that revision, and its text runs, once.
			r.handTask(testTask, 2)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			if report := d.reportOf(2); report.Task != testTask {
				t.Fatalf("the turn of the text taken again was reported %+v, want the turn of task %d", report, testTask)
			}
			// That turn's report finishes the task: a later revision of its number runs nothing.
			r.handTask(testTask, 3)
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1", "7 at 2"}) {
				t.Errorf("the bridge asked to take %q, want task 7 at revision 1 and again at revision 2", takes)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Fatalf("Claude Code read %q, want %q", texts, tc.texts)
			}
		})
	}
}

// The daemon refuses a take before the claim's ready has answered, so the bridge asks for none
// before claude-envoy's ready line, and its ready gate holds every Envoy delivery until that line,
// whatever value it holds (the plan's decision 16): claude-envoy writes the reading of claims/task
// it makes once the ready has answered ahead of the ready line, and the ready decision can write a
// value the registration did not carry.
//
//   - The registration's value, handed before the ready line, is reported seen at once, and an
//     Envoy delivery waits behind it. Once the ready line comes the task is taken, and runs ahead
//     of the delivery, which is that turn's next step.
//   - The registration held no value, and a delivery — the session's own direct message — comes
//     before the reading of the value the ready decision wrote: it starts no turn, and the task the
//     reading names runs first, the delivery its next step.
//   - The read after the ready failed, so no reading line comes, only the ready line: that line
//     opens the gate all the same, and the delivery starts its turn.
func TestNothingIsTakenBeforeTheReadyLine(t *testing.T) {
	// taskRunsAheadOfTheCard takes the ready line and holds that the task is taken once and runs
	// first, the delivery ev1 the next step of its turn.
	taskRunsAheadOfTheCard := func(t *testing.T, d *taskDaemon, r *rig, inbox *inboxClient) {
		t.Helper()
		r.handReady()
		r.expect("agent_start", nil)
		if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
			t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
		}
		r.expect("agent_end", nil)
		if report := d.reportOf(1); report.Task != testTask {
			t.Fatalf("the turn was reported %+v, want the turn of task %d", report, testTask)
		}
		if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
			t.Fatalf("the bridge asked to take %q, want task 7 once", takes)
		}
		if texts := r.userTexts(); !slices.Equal(texts, []string{"say the task", "say the card"}) {
			t.Fatalf("Claude Code read %q, want the task's text and then the card", texts)
		}
	}

	t.Run("the registration's value", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handReading(testTask, 1, testTask-1)
		r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the take of the claim's task")
		r.quiet()
		if takes := d.takesMade(); len(takes) != 0 {
			t.Fatalf("the bridge asked to take %+v before the ready line", takes)
		}
		taskRunsAheadOfTheCard(t, d, r, inbox)
	})

	t.Run("a card ahead of the reading of the value the ready decision wrote", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handFinished(testTask - 1)
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the claim's ready line")
		r.quiet()
		r.handReading(testTask, 1, testTask-1)
		r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
		r.quiet()
		taskRunsAheadOfTheCard(t, d, r, inbox)
	})

	t.Run("a failed read, with no reading line", func(t *testing.T) {
		d := newTaskDaemon(t)
		r := newTakingRig(t, d, "", daemonCalls{})
		inbox := r.dialInbox()

		r.handFinished(testTask - 1)
		inbox.deliver("ev1", "say the card")
		r.logged("Envoy delivery ev1 waits for the claim's ready line")
		r.quiet()
		r.handReady()
		r.expect("agent_start", nil)
		if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
			t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
		}
		r.expect("agent_end", nil)
		if report := d.reportOf(1); report.Task != 0 {
			t.Fatalf("the card's turn was reported %+v, want a turn of no task", report)
		}
		if takes := d.takesMade(); len(takes) != 0 {
			t.Fatalf("the bridge asked to take %+v with no value held", takes)
		}
		if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card"}) {
			t.Fatalf("Claude Code read %q, want the card", texts)
		}
	})
}

// A value naming a task the daemon's reading says the claim finished is owed nothing: it is
// reported seen, as every value is, and not taken, and an Envoy delivery does not wait for it. The
// highest finished number a line carried stands: a later relayed value of the same number, which
// carries none, is not taken either, and a value naming a later task is.
func TestAValueNamingAFinishedNumberIsReportedSeenAndNotTaken(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	r.handReading(testTask, 1, testTask)
	r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
	r.turn("ev1", "say the card")
	r.logged("Envoy delivery ev1 starts a turn")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.handTask(testTask, 2)
	r.waitFor("the relayed value to be reported seen", func() bool { return len(d.seenReports()) == 2 })
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v, a task the claim finished", takes)
	}

	r.handTask(testTask+1, 3)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	// The seen report and the take of a value are sent apart, each until the daemon answers, so the
	// take can land, and its turn run, before the seen report of the same value does.
	r.waitFor("the third value to be reported seen", func() bool { return len(d.seenReports()) == 3 })
	if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, []string{"7 at 1", "7 at 2", "8 at 3"}) {
		t.Errorf("the bridge reported seen %q, want every value", seen)
	}
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"8 at 3"}) {
		t.Errorf("the bridge asked to take %q, want task 8 alone", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card", "say the task"}) {
		t.Errorf("Claude Code read %q, want the card and then task 8's text", texts)
	}
}

// When a task's turn ends with no provider error its report finishes the task on the daemon, so
// the bridge holds the number finished from that end: a newer revision of it, written before the
// report lands — the daemon writes the value again from its record where the bucket lost it or
// holds another write, and would answer its take with the same text —, is reported seen and not
// taken. A 2xx keeps the mark, the record having finished the task; a 4xx refusal applied nothing
// and the record still owes the task, so the mark is cleared, and the newer revision is taken and
// runs once.
func TestATaskFinishedAtItsTurnsEndIsNotTakenAgainUntilItsReportIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		answer        any
		takes, texts  []string
		reportedAgain bool
	}{
		{name: "the report recorded", status: http.StatusOK, answer: claim.TurnOutcomeResponse{},
			takes: []string{"7 at 1"}, texts: []string{"say the task"}},
		{name: "the report refused with a 4xx", status: http.StatusConflict,
			answer: map[string]string{"error": "turn outcome: this launch is not the claim's current one"},
			takes:  []string{"7 at 1", "7 at 2"}, texts: []string{"say the task", "say the task"}, reportedAgain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTaskDaemon(t)
			lands, answered := make(chan struct{}), make(chan struct{})
			land := sync.OnceFunc(func() { close(lands) })
			t.Cleanup(land)
			d.answerReports(func(w http.ResponseWriter, _ *http.Request) {
				defer close(answered)
				<-lands
				writeAnswer(w, tc.status, tc.answer)
			})
			r := newTakingRig(t, d, "", daemonCalls{})
			r.handReady()

			r.handTask(testTask, 1)
			r.expect("agent_start", nil)
			r.untilTurnEnds()
			r.waitFor("the task's report to reach the daemon", func() bool { return len(d.reportsAttempted()) == 1 })
			r.handTask(testTask, 2)
			r.waitFor("the newer value to be reported seen", func() bool { return len(d.seenReports()) == 2 })
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"7 at 1"}) {
				t.Fatalf("before the report landed the bridge asked to take %q, want task 7 once", takes)
			}

			land()
			<-answered
			if tc.reportedAgain {
				r.logged("the daemon refused the outcome of turn 1 (clean): 409")
				r.expect("agent_start", nil)
				r.untilTurnEnds()
				if report := d.reportOf(2); report.Task != testTask {
					t.Fatalf("the turn was reported %+v, want the second turn, of task %d", report, testTask)
				}
			}
			r.quiet()
			if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, tc.takes) {
				t.Errorf("the bridge asked to take %q, want %q", takes, tc.takes)
			}
			if texts := r.userTexts(); !slices.Equal(texts, tc.texts) {
				t.Errorf("Claude Code read %q, want %q", texts, tc.texts)
			}
			if seen := pairsOf(t, d.seenReports()); !slices.Equal(seen, []string{"7 at 1", "7 at 2"}) {
				t.Errorf("the bridge reported seen %q, want both values", seen)
			}
		})
	}
}

// A task line of the shape a claude-envoy older than this bridge wrote — no finished number, which
// that claude-envoy held itself — is refused naming why, and nothing is reported seen or taken
// from it: the bridge would otherwise weigh the value with half of what it needs.
func TestAnOlderShapeTaskLineIsRefusedNamingWhy(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	r.handReady()

	answer := r.handLine(InboxDelivery{ID: "older", Kind: InboxTask, Number: testTask, Epoch: testEpoch, Revision: 1})
	want := InboxAnswer{ID: "older", Error: "an inbox task line is one JSON object with an id, a number, an epoch, a revision and " +
		"the claim's finished number: this one names no finished number, as a claude-envoy older than this bridge writes it, so no task is taken from it"}
	if answer != want {
		t.Fatalf("the inbox answered the older task line %+v, want %+v", answer, want)
	}
	r.quiet()
	if seen, takes := len(d.seenReports()), len(d.takesMade()); seen != 0 || takes != 0 {
		t.Fatalf("the bridge sent %d seen reports and %d takes for a line it refused, want none", seen, takes)
	}
}

// A reading of the daemon's that holds no value still says which task the claim finished, and
// the bridge holds that number beside the ones its task lines carried, as Oh My Pi's take raises
// its own from such a reading: a value it already holds that names that task is owed nothing from
// then on, so the delivery waiting behind it waits only for the ready gate, and the ready line
// takes nothing and lets it go. A value naming a later task is taken. A finished line that names
// no number is refused naming why.
func TestAFinishedLineOfAReadingWithNoValueIsHeldBesideTheTaskLines(t *testing.T) {
	d := newTaskDaemon(t)
	r := newTakingRig(t, d, "", daemonCalls{})
	inbox := r.dialInbox()

	if answer, want := r.handLine(InboxDelivery{ID: "no-number", Kind: InboxFinished}),
		(InboxAnswer{ID: "no-number", Error: "an inbox finished line is one JSON object with an id and the claim's finished number"}); answer != want {
		t.Fatalf("the inbox answered a finished line with no number %+v, want %+v", answer, want)
	}
	r.handTask(testTask, 1)
	r.waitFor("the value to be reported seen", func() bool { return len(d.seenReports()) == 1 })
	inbox.deliver("ev1", "say the card")
	r.logged("Envoy delivery ev1 waits for the take of the claim's task")

	r.handFinished(testTask)
	r.quiet()
	r.handReady()
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	r.expect("agent_end", nil)
	r.quiet()
	if takes := d.takesMade(); len(takes) != 0 {
		t.Fatalf("the bridge asked to take %+v, a task the claim finished", takes)
	}

	r.handTask(testTask+1, 2)
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	if takes := pairsOf(t, d.takesMade()); !slices.Equal(takes, []string{"8 at 2"}) {
		t.Errorf("the bridge asked to take %q, want task 8 alone", takes)
	}
	if texts := r.userTexts(); !slices.Equal(texts, []string{"say the card", "say the task"}) {
		t.Errorf("Claude Code read %q, want the card and then task 8's text", texts)
	}
}
