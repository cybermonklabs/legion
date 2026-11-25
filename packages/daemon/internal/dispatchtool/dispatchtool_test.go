package dispatchtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// question and session are the answer every test's calls run in: the question's id and the
// stopped agent's own session, which every call declares as its own.
const (
	question = "dispatch:m-1"
	session  = "ses_tester"
	// token is what the daemon's Dispatch token file holds, which no line the tool logs and no
	// output it answers may carry.
	token = "dsp-fixture-token-never-logged"
)

var bounds = config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 60000, GHCalls: 100}

// fakeRecorder is answer_tool_calls as the tool writes it, with a hook that sees each start.
type fakeRecorder struct {
	mu      sync.Mutex
	rows    map[int]store.AnswerToolCall
	onStart func(store.AnswerToolCall)
}

func (f *fakeRecorder) StartAnswerToolCall(_ context.Context, call store.AnswerToolCall) error {
	f.mu.Lock()
	if _, ok := f.rows[call.Seq]; ok {
		f.mu.Unlock()
		return fmt.Errorf("dispatch call %s.%d %w", call.Question, call.Seq, store.ErrAnswerToolCallRecorded)
	}
	f.rows[call.Seq] = call
	onStart := f.onStart
	f.mu.Unlock()
	if onStart != nil {
		onStart(call)
	}
	return nil
}

func (f *fakeRecorder) FinishAnswerToolCall(_ context.Context, _ string, seq int, result string, exit *int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[seq]
	if !ok || row.Result != nil {
		return fmt.Errorf("finish dispatch call %d: no started, unfinished call has that number", seq)
	}
	row.Result, row.Exit, row.FinishedAt = &result, exit, &at
	f.rows[seq] = row
	return nil
}

func (f *fakeRecorder) row(seq int) (store.AnswerToolCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[seq]
	return row, ok
}

// fixture is one answer's dispatch tool over a stand-in dispatch: a shell script in a directory
// of the test's own that writes its arguments (each ended by a NUL), its standard input, the
// environment it was started with (/proc's copy, which no shell adds to) and what its HOME holds
// into the record directory, then prints a line and ends as the script's tail says.
type fixture struct {
	t      *testing.T
	record string
	host   Host
	answer *Answer
	calls  *fakeRecorder
	logs   *bytes.Buffer
}

func newFixture(t *testing.T, tail string) *fixture { return newPlacedFixture(t, tail, Place{}) }

// newPlacedFixture is newFixture's tool opened for an answer the loop posts at place.
func newPlacedFixture(t *testing.T, tail string, place Place) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, record: filepath.Join(root, "record"), calls: &fakeRecorder{rows: map[int]store.AnswerToolCall{}}, logs: &bytes.Buffer{}}
	for _, dir := range []string{f.record, filepath.Join(root, "bin"), filepath.Join(root, "bun")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nrec='" + f.record + "'\n" +
		"printf '%s\\0' \"$@\" > \"$rec/argv\"\n" +
		"cat > \"$rec/stdin\"\n" +
		"cat /proc/$$/environ > \"$rec/environ\"\n" +
		"ls -A \"$HOME\" > \"$rec/home\"\n" +
		"printf 'dispatch ran %s\\n' \"$1\"\n" + tail + "\n"
	dispatch := filepath.Join(root, "bin", "dispatch")
	if err := os.WriteFile(dispatch, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(root, "dispatch-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.host = Host{
		Dispatch: dispatch, BunDir: filepath.Join(root, "bun"), URL: "https://dispatch.internal.example", TokenFile: tokenFile,
		StateDir: filepath.Join(root, "state"), Bounds: bounds, Calls: f.calls, Log: slog.New(slog.NewJSONHandler(f.logs, nil)), Now: time.Now,
	}
	answer, err := Open(f.host, session, question, place)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = answer.Close() })
	f.answer = answer
	return f
}

// args is argv as the model gives it, a JSON array of strings, [] for none.
func args(argv ...string) string {
	data, err := json.Marshal(append([]string{}, argv...))
	if err != nil {
		panic(err)
	}
	return string(data)
}

// run makes the call seq of the answer, its seq also its number among the answer's dispatch calls,
// with argv and stdin, the stand-in's record cleared first.
func (f *fixture) run(seq int, stdin string, argv ...string) Result {
	f.t.Helper()
	f.clear()
	return f.answer.Run(context.Background(), Call{Seq: seq, N: seq, Args: args(argv...), Stdin: stdin})
}

func (f *fixture) clear() {
	f.t.Helper()
	for _, name := range []string{"argv", "stdin", "environ", "home"} {
		if err := os.Remove(filepath.Join(f.record, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.t.Fatal(err)
		}
	}
}

// ran is whether the stand-in ran since the record was last cleared.
func (f *fixture) ran() bool {
	_, err := os.Stat(filepath.Join(f.record, "argv"))
	return err == nil
}

// argv is the arguments the stand-in was last run with.
func (f *fixture) argv() []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.record, "argv"))
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func (f *fixture) file(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.record, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// logLines are the tool's log lines whose message is msg.
func (f *fixture) logLines(msg string) []map[string]any {
	f.t.Helper()
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(f.logs.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			f.t.Fatalf("log line %s: %v", line, err)
		}
		if record["msg"] == msg {
			lines = append(lines, record)
		}
	}
	return lines
}

// A call runs dispatch with an environment built of nothing of the daemon's own: HOME the
// answer's empty directory under the state directory, the five DISPATCH variables naming the
// stopped agent's session, the daemon's Dispatch, its token file and the answer's own state
// directory, and PATH bun's directory and /bin, which the dispatch shim needs to start bun. No
// variable of the test's own environment reaches it, and its token reaches no output or log line.
func TestACallRunsWithTheEnvironmentTheDaemonBuildsAndNoOther(t *testing.T) {
	t.Setenv("LEGION_FIXTURE_DAEMON_SECRET", "daemon-only")
	t.Setenv("DISPATCH_TOKEN", "a-token-of-the-daemons-own")
	f := newFixture(t, "")
	got := f.run(1, "", "read", "LEGION-9")
	if !got.Ran || got.Exit != 0 || got.Output != "dispatch ran read\n" {
		t.Fatalf("the call = %+v; want it run, exit 0, with the stand-in's line", got)
	}
	dir := filepath.Join(f.host.StateDir, "answers", "dispatch:m-1", "dispatch")
	want := []string{
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=" + session,
		"DISPATCH_STATE_DIR=" + filepath.Join(dir, "state"),
		"DISPATCH_TOKEN_FILE=" + f.host.TokenFile,
		"DISPATCH_URL=https://dispatch.internal.example",
		"HOME=" + filepath.Join(dir, "home"),
		"PATH=" + f.host.BunDir + ":/bin",
	}
	env := strings.Split(strings.TrimSuffix(f.file("environ"), "\x00"), "\x00")
	slices.Sort(env)
	if !slices.Equal(env, want) {
		t.Errorf("dispatch ran with the environment\n%s\nwant exactly\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
	if home := f.file("home"); home != "" {
		t.Errorf("HOME held %q as the call ran; want it empty", home)
	}
	if strings.Contains(f.logs.String(), token) || strings.Contains(got.Output, token) {
		t.Errorf("the token reached the log or the output: %s", f.logs.String())
	}
}

// Each of the sixteen reads and the four writes the tool runs reaches dispatch as the model
// gave it, the write's mark aside.
func TestEachReadAndEachWriteRuns(t *testing.T) {
	f := newFixture(t, "")
	if len(ReadCommands) != 16 || len(WriteCommands) != 4 {
		t.Fatalf("the tool runs %d reads and %d writes, want 16 and 4: %v %v", len(ReadCommands), len(WriteCommands), ReadCommands, WriteCommands)
	}
	for i, command := range slices.Concat(ReadCommands, WriteCommands) {
		got := f.run(i+1, "", command, "--help")
		if !got.Ran || got.Exit != 0 || !f.ran() || f.argv()[0] != command {
			t.Errorf("dispatch %s = %+v, ran %t; want it run", command, got, f.ran())
		}
	}
}

// A command outside the twenty is refused before dispatch starts, with the reason the model reads:
// the daemon runs only those for a stopped agent, whose session holds no claim. One the dispatch
// command adds later is refused as well, since the tool keeps a list of what it runs and none of
// what it refuses.
func TestACommandOutsideTheTwentyIsRefusedBeforeItRuns(t *testing.T) {
	f := newFixture(t, "")
	for i, argv := range [][]string{
		{"issue-update", "--issue", "LEGION-9", "--status", "done"},
		{"claim", "--issue", "LEGION-9"},
		{"ask", "--issue", "LEGION-9", "--question", "Which?"},
		{"doc-edit", "--issue", "LEGION-9", "--edits-json", "[]"},
		{"issue-archive-everything", "--issue", "LEGION-9"},
		{"--help"},
		{},
	} {
		seq := i + 1
		got := f.run(seq, "", argv...)
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, "holds no claim") {
			t.Errorf("dispatch %q = %+v; want a refusal naming why", argv, got)
		}
		if f.ran() {
			t.Errorf("dispatch %q ran the command", argv)
		}
		row, ok := f.calls.row(seq)
		if !ok || row.Tool != store.ToolDispatch || row.Result == nil || *row.Result != got.Output || row.Exit != nil {
			t.Errorf("dispatch %q is recorded as %+v (%t); want its refusal, no exit", argv, row, ok)
		}
		if lines := f.logLines("answers: dispatch"); len(lines) != seq || lines[seq-1]["refused"] != got.Output {
			t.Errorf("dispatch %q logged %v; want one line naming its refusal", argv, lines)
		}
	}
}

// An option that reads a file of the daemon's host is refused for every command, in either of its
// forms, as is a picture to upload: the daemon hands dispatch no file of its host. The call's
// stdin is the one input a `-` reads.
func TestAFileOfTheHostIsRefusedAndStdinIsTheCallsInput(t *testing.T) {
	f := newFixture(t, "")
	for i, argv := range [][]string{
		{"comment", "--issue", "LEGION-9", "--body-file", "/etc/passwd"},
		{"comment", "--issue", "LEGION-9", "--body-file=/x"},
		{"read", "--payload-json-file", "x"},
		{"read", "--issue-file"},
		{"message", "--issue", "LEGION-9", "--body", "See.", "--image", "/etc/hostname"},
		{"comment", "--issue", "LEGION-9", "--body", "See.", "--image=/etc/hostname"},
	} {
		got := f.run(i+1, "", argv...)
		if got.Ran || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, "file of the daemon's host") {
			t.Errorf("dispatch %q = %+v; want a refusal naming the host's file", argv, got)
		}
		if f.ran() {
			t.Errorf("dispatch %q ran the command", argv)
		}
	}
	got := f.run(7, "LEGION-9", "read", "--issue-file", "-")
	if !got.Ran || f.file("stdin") != "LEGION-9" || !slices.Equal(f.argv(), []string{"read", "--issue-file", "-"}) {
		t.Errorf("a read of stdin = %+v, argv %q, stdin %q; want it run on the call's stdin, unchanged", got, f.argv(), f.file("stdin"))
	}
	got = f.run(8, "", "read", "--issue-file=-")
	if !got.Ran || f.file("stdin") != "" {
		t.Errorf("a read of an empty stdin = %+v, stdin %q; want it run on none", got, f.file("stdin"))
	}
}

// Every body a comment, a message or an agent's message posts carries the loop's mark, whether it
// is given in --body, in --body=, or on stdin under --body-file -, so the loop answers nothing its
// own tool posted; a read and a resolution run as they are given.
func TestWhatTheToolPostsCarriesTheLoopsMark(t *testing.T) {
	f := newFixture(t, "")
	mark := "\n\n<!-- legion-answer:dispatch:m-1." // the call's seq and the marker's end follow
	got := f.run(1, "", "comment", "--issue", "LEGION-12", "--body", "Plum.")
	if want := []string{"comment", "--issue", "LEGION-12", "--body", "Plum." + mark + "1 -->"}; !got.Ran || !slices.Equal(f.argv(), want) {
		t.Errorf("a comment ran with %q; want %q", f.argv(), want)
	}
	f.run(2, "", "message", "--issue", "LEGION-12", "--body=Plum.")
	if want := []string{"message", "--issue", "LEGION-12", "--body=Plum." + mark + "2 -->"}; !slices.Equal(f.argv(), want) {
		t.Errorf("a message ran with %q; want %q", f.argv(), want)
	}
	f.run(3, "Plum, from stdin.", "send-to-agent", "--session", "ses_asker", "--body-file", "-")
	if want := "Plum, from stdin." + mark + "3 -->"; f.file("stdin") != want || !slices.Equal(f.argv(), []string{"send-to-agent", "--session", "ses_asker", "--body-file", "-"}) {
		t.Errorf("an agent's message ran with %q on stdin %q; want the body %q", f.argv(), f.file("stdin"), want)
	}
	f.run(4, "Plum.", "comment", "--issue", "LEGION-12", "--body-file=-")
	if want := "Plum." + mark + "4 -->"; f.file("stdin") != want {
		t.Errorf("a comment's stdin %q; want %q", f.file("stdin"), want)
	}
	f.run(5, "", "read", "LEGION-12", "--events")
	if want := []string{"read", "LEGION-12", "--events"}; !slices.Equal(f.argv(), want) || f.file("stdin") != "" {
		t.Errorf("a read ran with %q, stdin %q; want %q as given", f.argv(), f.file("stdin"), want)
	}
	f.run(6, "", "resolve-comment", "--comment", "c-1")
	if want := []string{"resolve-comment", "--comment", "c-1"}; !slices.Equal(f.argv(), want) {
		t.Errorf("a resolution ran with %q; want %q as given", f.argv(), want)
	}
}

// An answer makes at most gh_calls dispatch calls, counted apart from its gh calls: the call's
// number among the answer's dispatch calls, an earlier run's counted, is what the bound reads, not
// its seq among every call of the answer. The call past it is refused with the bound, dispatch
// never started.
func TestTheCallPastTheBoundIsRefused(t *testing.T) {
	f := newFixture(t, "")
	capped := Call{Args: args("whoami")}
	f.answer.host.Bounds = config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 100, GHCalls: 2}
	for _, c := range []struct{ seq, n int }{{4, 1}, {7, 2}, {8, 3}} {
		f.clear()
		capped.Seq, capped.N = c.seq, c.n
		got := f.answer.Run(context.Background(), capped)
		switch {
		case c.n <= 2 && (!got.Ran || !f.ran()):
			t.Errorf("dispatch call %d (seq %d) of 2 = %+v; want it run", c.n, c.seq, got)
		case c.n == 3 && (got.Ran || f.ran() || !strings.Contains(got.Output, "this answer has made its 2 dispatch calls")):
			t.Errorf("dispatch call 3 of 2 = %+v, ran %t; want it refused with the bound", got, f.ran())
		}
	}
}

// What dispatch prints reaches the model cut at gh_output_chars characters, with the note gh's
// output is cut with; a call past gh_seconds is ended, its whole process group, with the bound's
// note, and recorded finished; and an exit is dispatch's own.
func TestOutputIsCutAndASlowCallIsEnded(t *testing.T) {
	f := newFixture(t, "printf '%s' 'éééééééééééééééééééééééééééééé'\nexit 1")
	f.answer.host.Bounds = config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 10, GHCalls: 5}
	got := f.run(1, "", "search", "--query", "plum")
	if want := "dispatch r\n[cut: 40 more characters]"; got.Output != want || got.Exit != 1 || !got.Ran {
		t.Errorf("a long answer = %+v; want %q, exit 1", got, want)
	}

	slow := newFixture(t, "sleep 60")
	slow.answer.host.Bounds = config.AnswerBounds{GHCall: 2 * time.Second, GHOutputChars: 100, GHCalls: 5}
	start := time.Now()
	got = slow.run(1, "", "read", "LEGION-9")
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a dispatch sleeping a minute under a 2s bound returned after %s", elapsed)
	}
	if !got.Ran || !strings.Contains(got.Output, "[stopped: dispatch ran past its 2s bound (answers.bounds.gh_seconds)]") {
		t.Errorf("a slow dispatch = %+v; want it ended with the note", got)
	}
	if row, _ := slow.calls.row(1); row.Result == nil || row.Exit == nil {
		t.Errorf("the ended call is recorded as %+v; want it finished", row)
	}
}

// Each call is recorded as the dispatch tool's, with no App and no owner, before dispatch starts,
// with its arguments as run, and finished after with what the model read and dispatch's exit.
// Its one `answers: dispatch` line names the question, the seq, the session it ran as, the command
// and the exit.
func TestEachCallIsRecordedBeforeItRunsAndFinishedAfterAndLogged(t *testing.T) {
	f := newFixture(t, "exit 3")
	var ranAtStart []bool
	f.calls.onStart = func(store.AnswerToolCall) { ranAtStart = append(ranAtStart, f.ran()) }
	got := f.run(1, "", "comment", "--issue", "LEGION-12", "--body", "Plum.")
	if got.Exit != 3 || !got.Ran {
		t.Fatalf("the call = %+v; want it run, exit 3", got)
	}
	if !slices.Equal(ranAtStart, []bool{false}) {
		t.Errorf("dispatch had run when the call's start was recorded (%v); want it recorded first", ranAtStart)
	}
	row, ok := f.calls.row(1)
	if !ok || row.Tool != store.ToolDispatch || row.App != "" || row.Owner != "" || !slices.Equal(row.Args, f.argv()) ||
		row.Result == nil || *row.Result != got.Output || row.Exit == nil || *row.Exit != 3 || row.FinishedAt == nil {
		t.Errorf("the call is recorded as %+v; want the dispatch tool's, no App or owner, its arguments as run, finished with exit 3", row)
	}
	lines := f.logLines("answers: dispatch")
	if len(lines) != 1 {
		t.Fatalf("%d answers: dispatch lines, want 1: %s", len(lines), f.logs.String())
	}
	for key, want := range map[string]any{"question": question, "seq": 1.0, "session": session, "command": "comment", "exit": 3.0, "ran": true} {
		if lines[0][key] != want {
			t.Errorf("the line's %s is %v, want %v", key, lines[0][key], want)
		}
	}
	if _, refused := lines[0]["refused"]; refused {
		t.Errorf("the line of a call that ran names a refusal: %v", lines[0])
	}
}

// Close removes the answer's directory, its HOME and state with it.
func TestCloseRemovesTheAnswersDirectory(t *testing.T) {
	f := newFixture(t, "")
	f.run(1, "", "whoami")
	dir := filepath.Join(f.host.StateDir, "answers", "dispatch:m-1", "dispatch")
	if _, err := os.Stat(filepath.Join(dir, "home")); err != nil {
		t.Fatalf("the answer's HOME before Close: %v", err)
	}
	if err := f.answer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the answer's directory after Close: %v; want it gone", err)
	}
}

// Writes are the dispatch calls a question's record holds that ran, or may have, a write: every
// command but the dispatch command's reads, one the daemon stopped during and one the tool does
// not know among them, and no call it refused before dispatch started, no read, no write's help
// or dry run, which post nothing, and no gh call.
func TestWritesAreTheDispatchCallsThatRanAWrite(t *testing.T) {
	ok, failed := 0, 1
	posted, refused := "posted", "refused: `dispatch claim` is none of the commands this tool runs"
	calls := []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "comment", "7", "--body", "x"}, Owner: "acme", Result: &posted, Exit: &ok},
		{Seq: 2, Tool: store.ToolDispatch, Args: []string{"read", "LEGION-9"}, Result: &posted, Exit: &ok},
		{Seq: 3, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "LEGION-9", "--body", "x"}, Result: &posted, Exit: &ok},
		{Seq: 4, Tool: store.ToolDispatch, Args: []string{"claim", "--issue", "LEGION-9"}, Result: &refused},
		{Seq: 5, Tool: store.ToolDispatch, Args: []string{"message", "--issue", "LEGION-9", "--body", "x"}},
		{Seq: 6, Tool: store.ToolDispatch, Args: []string{"resolve-comment", "--comment", "c-1"}, Result: &posted, Exit: &failed},
		{Seq: 7, Tool: store.ToolDispatch, Args: []string{"issue-archive", "LEGION-9"}, Result: &posted, Exit: &ok},
		{Seq: 8, Tool: store.ToolDispatch, Args: []string{"comment", "--issue", "LEGION-9", "--help"}, Result: &posted, Exit: &ok},
		{Seq: 9, Tool: store.ToolDispatch, Args: []string{"message", "--issue", "LEGION-9", "--body", "x", "--dry-run"}, Result: &posted, Exit: &ok},
	}
	want := []Write{{Seq: 3, Command: "comment", Exit: &ok}, {Seq: 5, Command: "message"}, {Seq: 6, Command: "resolve-comment", Exit: &failed}, {Seq: 7, Command: "issue-archive", Exit: &ok}}
	if got := Writes(calls); !reflect.DeepEqual(got, want) {
		t.Errorf("Writes = %+v; want %+v", got, want)
	}
}

// A call the model asks for writes, by its arguments as the tool is handed them, exactly when its
// command is one of WriteCommands, the four the tool runs that write, and it asks for neither
// help nor a dry run, wherever its `--help` or `--dry-run` stands. Every read is none, and so is a
// write's help or dry run, which posts nothing, a command the tool does not run and arguments
// that are no JSON array of strings, which the tool refuses before dispatch starts. The project's
// pause refuses the calls that write and runs the rest (internal/answer's toolSet).
func TestCallWritesIsACallOfOneOfTheFourWrites(t *testing.T) {
	for _, command := range WriteCommands {
		if args := `["` + command + `","--issue","LEGION-9","--body","x"]`; !CallWrites(args) {
			t.Errorf("CallWrites(%s) = false, want true", args)
		}
		for _, args := range []string{`["` + command + `","--issue","LEGION-9","--help"]`, `["` + command + `","--help","--issue","LEGION-9"]`,
			`["` + command + `","--issue","LEGION-9","--body","x","--dry-run"]`} {
			if CallWrites(args) {
				t.Errorf("CallWrites(%s) = true, want false: it posts nothing", args)
			}
		}
	}
	for _, command := range ReadCommands {
		if args := `["` + command + `","LEGION-9"]`; CallWrites(args) {
			t.Errorf("CallWrites(%s) = true, want false", args)
		}
	}
	for _, args := range []string{`["claim","--issue","LEGION-9"]`, `"comment --issue LEGION-9"`, `[]`} {
		if CallWrites(args) {
			t.Errorf("CallWrites(%s) = true, want false", args)
		}
	}
}

// Each call's `answers: dispatch` line says whether it wrote (write), so its reader need not judge
// the arguments: true for a call dispatch ran that writes as CallWrites and Writes read one, a
// comment and a resolution; false for a read, a write's help and its dry run, which post
// nothing, a command the tool does not run and a write aimed where the loop posts the answer,
// both refused before dispatch started.
func TestEachCallsLineSaysWhetherItWrote(t *testing.T) {
	f := newPlacedFixture(t, "", Place{Ask: theAsk})
	for i, c := range []struct {
		argv       []string
		ran, write bool
	}{
		{[]string{"read", "LEGION-9"}, true, false},
		{[]string{"comment", "--issue", "LEGION-9", "--body", "Plum."}, true, true},
		{[]string{"resolve-comment", "--comment", "c-1"}, true, true},
		{[]string{"comment", "--issue", "LEGION-9", "--help"}, true, false},
		{[]string{"message", "--issue", "LEGION-9", "--body", "Plum.", "--dry-run"}, true, false},
		{[]string{"claim", "--issue", "LEGION-9"}, false, false},
		{[]string{"comment", "--issue", "LEGION-9", "--reply-to-ask", theAsk, "--body", "Plum."}, false, false},
	} {
		seq := i + 1
		got := f.run(seq, "", c.argv...)
		lines := f.logLines("answers: dispatch")
		if got.Ran != c.ran || len(lines) != seq {
			t.Errorf("dispatch %q = %+v with %d lines; want ran %t and one line more", c.argv, got, len(lines), c.ran)
			continue
		}
		if write, ok := lines[seq-1]["write"].(bool); !ok || write != c.write || lines[seq-1]["ran"] != c.ran {
			t.Errorf("dispatch %q logged %v; want ran %t, write %t", c.argv, lines[seq-1], c.ran, c.write)
		}
	}
}

// The ids of the places TestAWriteWhereTheLoopPostsTheAnswerIsRefused aims at, and of the places
// elsewhere a write of the same command may go.
const (
	theAsk         = "e10f5bb8-0000-4000-8000-000000000001"
	theAskReply    = "c0000000-0000-4000-8000-0000000000a1"
	theRoot        = "c0000000-0000-4000-8000-000000000001"
	theReply       = "c0000000-0000-4000-8000-000000000002"
	theMention     = "c0000000-0000-4000-8000-000000000003"
	theMessage     = "d0000000-0000-4000-8000-000000000001"
	theAsker       = "ses_asker"
	anotherAsk     = "e10f5bb9-0000-4000-8000-000000000002"
	anotherComment = "c0000000-0000-4000-8000-000000000009"
	anotherMessage = "d0000000-0000-4000-8000-000000000009"
)

// A write aimed where the loop posts the answer itself is a second reply there, and is refused
// before dispatch starts, with a reason that says the daemon posts the reply there and that the
// answer goes in the reply: for each kind of question, in each form the CLI reads the option that
// aims it (`--x v`, `--x=v`, and `--x-file -` with the value on stdin) and each spelling of the id
// it reads (a part or block reference, an ask's id prefix, a uuid's other spellings). The refusal
// is recorded and logged as every refusal is, so it is no write (Writes) and its line reads ran
// false. The same command aimed anywhere else runs, as do a resolution, a help call and a dry run
// of the aimed write, which post nothing. A tool opened with no place refuses none of them.
func TestAWriteWhereTheLoopPostsTheAnswerIsRefused(t *testing.T) {
	type aimed struct {
		stdin string
		argv  []string
	}
	at := func(argv ...string) aimed { return aimed{argv: argv} }
	onStdin := func(stdin string, argv ...string) aimed { return aimed{stdin: stdin, argv: argv} }
	for _, c := range []struct {
		kind      string
		place     Place
		refused   []aimed
		elsewhere []aimed
	}{
		{
			// A person's reply on the agent's ask: the loop posts on the ask's thread (postOnAsk),
			// where a reply to the person's own comment lands too, since Dispatch climbs it to the ask.
			kind: "an ask reply", place: Place{Ask: theAsk, Comments: []string{theAskReply}},
			refused: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--turn", "agent", "--body", "The flaky test is plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask="+theAsk, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", "e10f5bb8", "--turn", "human", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", "E10F5BB8000040008000000000000001", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", "dispatch://ALP-8/ask/"+theAsk+"#b-b2", "--body", "Plum."),
				onStdin(theAsk, "comment", "--issue", "ALP-8", "--reply-to-ask-file", "-", "--body", "Plum."),
				onStdin(theAsk, "comment", "--issue", "ALP-8", "--reply-to-ask-file=-", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", theAskReply, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to={"+theAskReply+"}", "--body", "Plum."),
			},
			elsewhere: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", anotherAsk, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", "e10f5bb9", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", anotherComment, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--body", "Plum."),
				at("resolve-comment", "--comment", theAskReply),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--body", "Plum.", "--help"),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--body", "Plum.", "--dry-run"),
			},
		},
		{
			// A person's answer to the ask: the loop posts on its thread too.
			kind: "an answer to the ask", place: Place{Ask: theAsk},
			refused: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask=e10f5bb8-0000", "--body", "Plum."),
			},
			elsewhere: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", anotherAsk, "--body", "Plum."),
			},
		},
		{
			// A reply in a comment thread: the loop replies in that thread (postInThread), and a reply
			// naming any comment of it lands there, Dispatch climbing it to the thread's root.
			kind: "a thread reply", place: Place{Comments: []string{theReply, theRoot}},
			refused: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to", theRoot, "--body-file", "-"),
				at("comment", "--issue", "ALP-8", "--reply-to="+theReply, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", "dispatch://ALP-8/comment/"+theRoot+"#b-b1", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", "urn:uuid:"+theReply, "--body", "Plum."),
				onStdin(theRoot, "comment", "--issue", "ALP-8", "--reply-to-file", "-", "--body", "Plum."),
			},
			elsewhere: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to", anotherComment, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--body", "Plum."),
				at("resolve-comment", "--comment", theRoot),
				at("message", "--issue", "ALP-8", "--body", "Plum."),
			},
		},
		{
			// A comment mentioning the agent in a thread: Dispatch's reply route puts the loop's
			// reply to the mention in the mention's thread.
			kind: "a mention", place: Place{Comments: []string{theMention, theRoot}},
			refused: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to", theMention, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to="+theRoot, "--body", "Plum."),
			},
			elsewhere: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to", anotherComment, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--body", "Plum."),
			},
		},
		{
			// A comment mentioning the agent under an ask: Dispatch puts the reply on that ask.
			kind: "a mention under an ask", place: Place{Ask: theAsk, Comments: []string{theMention}},
			refused: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", theAsk, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", theMention, "--body", "Plum."),
			},
			elsewhere: []aimed{
				at("comment", "--issue", "ALP-8", "--reply-to-ask", anotherAsk, "--body", "Plum."),
			},
		},
		{
			// A Dispatch message: the loop replies to it (its delivery's reply route), as a message
			// replying to it does, on its issue or in a direct conversation.
			kind: "a message", place: Place{Message: theMessage},
			refused: []aimed{
				at("message", "--issue", "ALP-8", "--in-reply-to", theMessage, "--body", "Plum."),
				at("message", "--in-reply-to="+theMessage, "--body", "Plum."),
				at("message", "--in-reply-to", "dispatch://ALP-8/message/"+theMessage, "--body", "Plum."),
				at("message", "--in-reply-to", "dispatch://ALP-8/message/"+theMessage+"#b-b1", "--body", "Plum."),
				onStdin(theMessage, "message", "--in-reply-to-file", "-", "--body", "Plum."),
				at("send-to-agent", "--session", session, "--in-reply-to", theMessage, "--body", "Plum.", "--delivery", "btw"),
			},
			elsewhere: []aimed{
				at("message", "--issue", "ALP-8", "--body", "Plum."),
				at("message", "--issue", "ALP-8", "--in-reply-to", anotherMessage, "--body", "Plum."),
				at("comment", "--issue", "ALP-8", "--reply-to", theMessage, "--body", "Plum."),
				at("message", "--issue", "ALP-8", "--in-reply-to", theMessage, "--body", "Plum.", "--dry-run"),
			},
		},
		{
			// Another agent's Envoy message: the loop sends its reply back to that session.
			kind: "an Envoy message", place: Place{Session: theAsker},
			refused: []aimed{
				at("send-to-agent", "--session", theAsker, "--body", "Plum.", "--delivery", "btw"),
				at("send-to-agent", "--session="+theAsker, "--body", "Plum.", "--delivery", "aside"),
				onStdin(theAsker, "send-to-agent", "--session-file", "-", "--body", "Plum.", "--delivery", "btw"),
			},
			elsewhere: []aimed{
				at("send-to-agent", "--session", "ses_reviewer", "--body", "Plum.", "--delivery", "btw"),
				at("message", "--issue", "ALP-8", "--body", "Plum."),
				at("send-to-agent", "--session", theAsker, "--body", "Plum.", "--delivery", "btw", "--help"),
			},
		},
	} {
		f := newPlacedFixture(t, "", c.place)
		seq := 0
		for _, call := range c.refused {
			seq++
			got := f.run(seq, call.stdin, call.argv...)
			if got.Ran || got.Exit != -1 || f.ran() || !strings.HasPrefix(got.Output, "refused: ") ||
				!strings.Contains(got.Output, "the daemon posts your reply") || !strings.Contains(got.Output, "in your reply") {
				t.Errorf("%s: dispatch %q (stdin %q) = %+v, ran %t; want it refused before it ran, saying the daemon posts the reply there", c.kind, call.argv, call.stdin, got, f.ran())
				continue
			}
			row, ok := f.calls.row(seq)
			if !ok || !row.RefusedBeforeRun() || len(Writes([]store.AnswerToolCall{row})) != 0 {
				t.Errorf("%s: dispatch %q is recorded as %+v (%t); want its refusal, no exit, no write", c.kind, call.argv, row, ok)
			}
			if lines := f.logLines("answers: dispatch"); len(lines) != seq || lines[seq-1]["ran"] != false || lines[seq-1]["refused"] != got.Output {
				t.Errorf("%s: dispatch %q logged %v; want a line reading ran false with its refusal", c.kind, call.argv, lines)
			}
		}
		for _, call := range c.elsewhere {
			seq++
			if got := f.run(seq, call.stdin, call.argv...); !got.Ran || !f.ran() {
				t.Errorf("%s: dispatch %q = %+v; want it run, aimed elsewhere or posting nothing", c.kind, call.argv, got)
			}
		}
		unplaced := newFixture(t, "")
		for i, call := range c.refused {
			if got := unplaced.run(i+1, call.stdin, call.argv...); !got.Ran || !unplaced.ran() {
				t.Errorf("%s: dispatch %q with no place = %+v; want it run", c.kind, call.argv, got)
			}
		}
	}
}
