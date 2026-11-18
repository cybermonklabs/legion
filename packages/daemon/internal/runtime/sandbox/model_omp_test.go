package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// runOmp runs one Oh My Pi command to its end on the frames given, and returns what it wrote. The
// child is told nothing but environ: a pod's own PI_CONFIG_FILES would otherwise outrank the
// profile the measurement is of.
func runOmp(t *testing.T, argv []string, dir string, environ []string, stdin string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.CommandContext(testwait.Context(t), argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, environ, strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// withPromptFile is argv, an agentArgv, with the generation's prompt file it names replaced by a
// file of the test's own holding text, as the launcher writes that file in a pod before the agent
// starts (launcherCommand's Files): Oh My Pi would read a path it cannot open as the prompt's text
// itself, which is no launch a pod makes.
func withPromptFile(t *testing.T, argv []string, text string) []string {
	t.Helper()
	at := slices.Index(argv, "--append-system-prompt")
	if at < 0 || at+1 == len(argv) {
		t.Fatalf("the agent argv %q names no --append-system-prompt file", argv)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(argv[at+1]))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	argv = slices.Clone(argv)
	argv[at+1] = path
	return argv
}

// requestSystem is the system text of one anthropic-messages request body: its `system` member,
// which is one string or a list of text blocks, the blocks' texts joined in order, and "" for a
// body that carries none.
func requestSystem(body []byte) string {
	var request struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.System) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(request.System, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(request.System, &blocks) != nil {
		return ""
	}
	var joined strings.Builder
	for _, block := range blocks {
		joined.WriteString(block.Text)
	}
	return joined.String()
}

// Oh My Pi appends the whole of the file its `--append-system-prompt` names, however long: a pod's
// agent is named its generation's prompt file (systemPromptPath), so a prompt past the 131072
// bytes Linux lets one argv string be reaches the model whole. Measured on the pinned binary: a
// 200000-byte prompt ending in a line of its own, and the system text of a request the turn makes
// to an anthropic-messages stand-in holding every byte of it. The control is a path to a file that
// does not exist, which Oh My Pi silently takes as the prompt's text itself and sends the model as
// such, with no error: that is why the launcher writes the file before the agent starts
// (launcherCommand), since a missing file would fail no launch and leave the agent without its
// prompt.
func TestOhMyPiAppendsTheWholePromptFileItIsNamed(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var systems []string
	// The provider endpoint records only a request under the profile's API key ("unused"), the
	// credential Oh My Pi presents to it (ompProfileKey); any other is another process's, answered
	// 404.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ompProfileKey(r) {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		systems = append(systems, requestSystem(body))
		mu.Unlock()
		testbin.AnthropicAnswer(w, "configured", "ok")
	}))
	defer server.Close()
	// taken is every system text the server has been sent since it was last asked.
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		seen := systems
		systems = nil
		return seen
	}
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home,
		"providers:\n  operator:\n    baseUrl: "+server.URL+"\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: configured\n        name: Configured\n",
		"modelRoles:\n  default: operator/configured\n")
	environ := testbin.OMPEnviron(home)

	const size = 200_000
	line, last := "A line of a system prompt longer than one argv string may be.\n", "The prompt's own last line."
	prompt := strings.Repeat(line, size/len(line)+1)[:size-len(last)-1] + "\n" + last
	frames, stderr := promptOnce(t, withPromptFile(t, launch{}.agentArgv([]string{omp}), prompt), work, environ, "Reply ok.")
	if !slices.ContainsFunc(frames, func(frame testbin.OMPFrame) bool { return frame.Type == "message_end" }) {
		t.Fatalf("the turn ended with no answer\nstderr: %s", stderr)
	}
	seen := taken()
	if !slices.ContainsFunc(seen, func(system string) bool { return strings.Contains(system, prompt) }) {
		lengths := make([]int, len(seen))
		for i, system := range seen {
			lengths[i] = len(system)
		}
		t.Errorf("none of the turn's %d requests carries the whole %d-byte prompt in its system text (system texts of %v bytes; its last line reached the model: %t)\nstderr: %s",
			len(seen), len(prompt), lengths, slices.ContainsFunc(seen, func(system string) bool { return strings.Contains(system, last) }), stderr)
	}

	missing := filepath.Join(dir, "absent", "system-prompt.md")
	control := launch{}.agentArgv([]string{omp})
	control[slices.Index(control, "--append-system-prompt")+1] = missing
	_, stderr = promptOnce(t, control, work, environ, "Reply ok.")
	seen = taken()
	if !slices.ContainsFunc(seen, func(system string) bool { return strings.Contains(system, missing) }) {
		t.Errorf("a path to no file did not reach the model as the prompt's own text: no system text of the turn's %d requests names %s\nstderr: %s",
			len(seen), missing, stderr)
	}
	if slices.ContainsFunc(seen, func(system string) bool { return strings.Contains(system, last) }) {
		t.Errorf("the control's turn carries the long prompt's last line, which no file it named holds")
	}
}

// A pod's agent resumes the session its claim recorded and runs the model the pod's overlay puts
// on the `default` role, not the one that session saved: `--model @default` rides the recorded
// argv, and @default is Oh My Pi's own name for that role. Measured on the pinned binary: a
// session saved on one model, the profile's `modelRoles.default` moved to another, and the
// recorded argv's own answer naming the profile's current model. The same argv with the flag
// taken out is the control, and reports the saved model instead.
//
// **The control runs first, and it must.** A resume carrying a role alias writes a
// `model_change` of its own into the session it resumed, naming what the role resolved to
// (upstream oh-my-pi #18534, in the fork from 18.18.6; through 18.16.0 it wrote nothing). So the
// flagged arm leaves the session pinned to `operator/configured`, and a control run after it
// would read that back and measure nothing but the arm before it. Run before the flagged arm,
// the control reads the pin the saving turn wrote, which is the only thing it was ever asking
// about, and it reads the same on either build.
//
// The resumes leave the session's earlier bytes alone and add no second file. That is worth
// checking but explains nothing about role resolution: the flag rides every launch because the
// operator's current role is resolved per launch (TestAgentArgvCarriesTheConfiguredModel).
func TestTheResumedAgentRunsTheConfiguredModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testbin.AnthropicAnswer(w, "saved", "ok")
	}))
	defer server.Close()
	testbin.OMPHome(t, omp, home)
	agent := testbin.WriteOMPProfile(t, home,
		"providers:\n  operator:\n    baseUrl: "+server.URL+"\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: saved\n        name: Saved\n      - id: configured\n        name: Configured\n",
		"modelRoles:\n  default: operator/saved\n")
	environ := testbin.OMPEnviron(home)

	// A session of the profile's own, saved on operator/saved: print mode answers one short turn
	// through the test's provider and writes the file the resume below names. The launch argv is
	// not used here — it is what the resume measures — and the model is the profile's role.
	if stdout, stderr, err := runOmp(t, []string{omp, "-p", "Reply ok."}, work, environ, ""); err != nil {
		t.Fatalf("saving a session: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	sessions := filepath.Join(agent, "sessions")
	saved := testbin.SessionFiles(t, sessions)
	if len(saved) != 1 {
		t.Fatalf("the profile holds %q after one print-mode turn, want one session file", saved)
	}
	before, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte(`"model":"operator/saved"`)) {
		t.Fatalf("the saved session names no model_change on operator/saved:\n%s", before)
	}

	// The operator moves the role, as apply.sh does for a running deployment's next pods.
	if err := os.WriteFile(filepath.Join(agent, "config.yml"), []byte("modelRoles:\n  default: operator/configured\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	argv := withPromptFile(t, launch{spec: runtime.SpawnSpec{ResumeSessionFile: saved[0]}}.agentArgv([]string{omp}), "The test's system prompt.")

	// The control, and it runs first: the same launch with the flag taken out reads the session's
	// own pin, which at this point is the one the saving turn wrote and nothing else.
	without := slices.DeleteFunc(slices.Clone(argv), func(word string) bool { return word == "--model" || word == "@default" })
	plain, stderr, err := runOmp(t, without, work, environ, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v\nstdout: %s\nstderr: %s", strings.Join(without[1:], " "), err, plain, stderr)
	}
	if got := testbin.StateModel(t, plain); got != "operator/saved" {
		t.Errorf("without the flag the resumed agent runs %s, want the session's own operator/saved", got)
	}
	if got, want := testbin.ModelChanges(t, saved[0]), []string{"operator/saved"}; !slices.Equal(got, want) {
		t.Errorf("after the plain control, recorded model changes = %q, want %q", got, want)
	}

	// The recorded argv itself, which carries the flag: it answers on the profile's role whatever
	// the session was pinned to.
	stdout, stderr, err := runOmp(t, argv, work, environ, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v\nstdout: %s\nstderr: %s", strings.Join(argv[1:], " "), err, stdout, stderr)
	}
	if got := testbin.StateModel(t, stdout); got != "operator/configured" {
		t.Errorf("the resumed agent runs %s, want the profile's operator/configured\nstderr: %s", got, stderr)
	}
	if got, want := testbin.ModelChanges(t, saved[0]), []string{"operator/saved", "operator/configured"}; !slices.Equal(got, want) {
		t.Errorf("after the role-alias resume, recorded model changes = %q, want %q", got, want)
	}

	// Moving the role back cannot recover the old pin: a later plain resume reads the role-alias
	// resume's durable replacement.
	if err := os.WriteFile(filepath.Join(agent, "config.yml"), []byte("modelRoles:\n  default: operator/saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, stderr, err = runOmp(t, without, work, environ, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v\nstdout: %s\nstderr: %s", strings.Join(without[1:], " "), err, plain, stderr)
	}
	if got, want := testbin.StateModel(t, plain), "operator/configured"; got != want {
		t.Errorf("after the role moves back, the plain resumed agent runs %s, want the durable %s", got, want)
	}

	after, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Errorf("the resumed session's earlier bytes changed:\nbefore: %s\nafter:  %s", before, after)
	}
	if files := testbin.SessionFiles(t, sessions); !slices.Equal(files, saved) {
		t.Errorf("the profile holds %q after the resumes, want the one session file %q", files, saved)
	}
}

// A role's fallback chain still walks under the launch's explicit model: a chain is keyed by the
// model string, and @default resolves to that same string, so `--model @default` costs the role
// nothing it had. Measured on the pinned binary against one loopback server serving two
// providers: the role's own model answers 429 with a `retry-after` past `retry.maxDelayMs`, which
// is Oh My Pi's signal to stop waiting, and the chain's next model answers the turn.
func TestAFallbackChainStillWalksUnderAnExplicitModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	hits := map[string]int{}
	// Each provider endpoint counts only a request under the profile's API key ("unused"), the
	// credential Oh My Pi presents to it (ompProfileKey); any other is another process's, answered
	// 404.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ompProfileKey(r) {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		first := strings.HasPrefix(r.URL.Path, "/first")
		if first {
			hits["first"]++
		} else {
			hits["second"]++
		}
		mu.Unlock()
		if first {
			// Over the settings' maxDelayMs: Oh My Pi surfaces the failure rather than parking
			// the turn, and the role's chain is walked.
			w.Header().Set("retry-after", "120")
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		testbin.AnthropicAnswer(w, "second-model", "second-model answered")
	}))
	defer server.Close()
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home,
		"providers:\n  first:\n    baseUrl: "+server.URL+"/first\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: first-model\n        name: First\n"+
			"  second:\n    baseUrl: "+server.URL+"/second\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: second-model\n        name: Second\n",
		"modelRoles:\n  default: first/first-model\n"+
			"retry:\n  modelFallback: true\n  maxDelayMs: 1000\n  fallbackChains:\n    first/first-model:\n      - second/second-model\n")

	argv := withPromptFile(t, launch{}.agentArgv([]string{omp}), "The test's system prompt.")
	frames, stderr := promptOnce(t, argv, work, testbin.OMPEnviron(home), "Reply ok.")
	answered := ""
	for _, frame := range frames {
		if frame.Type == "message_end" {
			answered = frame.Message.Provider + "/" + frame.Message.Model
		}
	}
	if answered != "second/second-model" {
		t.Errorf("the turn was answered by %q, want the chain's second/second-model\nstderr: %s", answered, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["first"] == 0 || hits["second"] == 0 {
		t.Errorf("the server was called %v, want both the role's model and the chain's next", hits)
	}
}

// Measured, not wished for: with no `modelRoles.default` in its settings, `--model @default` does
// not refuse the launch — Oh My Pi reads @default as its own built-in chat role and starts on the
// model behind it, here claude-fable-5-1@default on google-vertex, which is no model of the
// profile's and reachable from no pod. (A role the settings never name at all is the loud case:
// `--model @nosuchrole` exits with `Model "@nosuchrole" not found`.) So the flag is no guard
// against an overlay that omits the role; the deployment's guard is the operator's, and
// deploy/kubernetes/operator-route/README.md says so.
func TestMeasuredNoDefaultRoleLaunchesOnABuiltInModel(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	testbin.OMPHome(t, omp, home)
	testbin.WriteOMPProfile(t, home,
		"providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n"+
			"    apiKey: unused\n    models:\n      - id: configured\n        name: Configured\n",
		"enabledModels:\n  - operator/configured\n")

	argv := withPromptFile(t, launch{}.agentArgv([]string{omp}), "The test's system prompt.")
	stdout, stderr, err := runOmp(t, argv, work, testbin.OMPEnviron(home), `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("omp %s: %v, want a launch\nstdout: %s\nstderr: %s", strings.Join(argv[1:], " "), err, stdout, stderr)
	}
	got := testbin.StateModel(t, stdout)
	if strings.HasPrefix(got, "operator/") {
		t.Errorf("with no modelRoles.default the launch runs %s, want a model that is not the profile's", got)
	}
	t.Logf("with no modelRoles.default, --model @default silently runs %s", got)
}

// promptOnce runs argv, sends it one prompt, and returns every frame it wrote up to the end of
// that prompt. Oh My Pi exits when its standard input closes, so the input is held open until the
// turn is answered.
func promptOnce(t *testing.T, argv []string, dir string, environ []string, message string) ([]testbin.OMPFrame, string) {
	t.Helper()
	cmd := exec.CommandContext(testwait.Context(t), argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, environ
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, `{"type":"prompt","id":"r2","message":`+strconv.Quote(message)+"}\n"); err != nil {
		t.Fatal(err)
	}
	var frames []testbin.OMPFrame
	closed := false
	lines := bufio.NewScanner(stdout)
	// A frame is up to Oh My Pi's maxFrameBytes, which its ready frame reports as 1 MiB.
	lines.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for lines.Scan() {
		var frame testbin.OMPFrame
		if json.Unmarshal(lines.Bytes(), &frame) != nil {
			continue
		}
		frames = append(frames, frame)
		if frame.Type == "prompt_result" && frame.ID == "r2" && !closed {
			closed = true
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !closed {
		if err := stdin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("omp %s: %v\nstderr: %s", strings.Join(argv[1:], " "), err, errOut.String())
	}
	if !closed {
		t.Fatalf("omp %s never answered the prompt\nstderr: %s", strings.Join(argv[1:], " "), errOut.String())
	}
	return frames, errOut.String()
}

// ompProfileKey reports whether r carries the API key the test profiles give each provider,
// "unused": omp 18.16.0 sends it as a bearer (Authorization: Bearer unused), and Anthropic's own
// header (x-api-key) is taken too.
func ompProfileKey(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer unused" || r.Header.Get("x-api-key") == "unused"
}
