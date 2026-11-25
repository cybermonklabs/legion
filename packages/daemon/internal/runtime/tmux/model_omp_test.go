package tmux

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A pane of a role that names no model, resuming the session its claim recorded, runs the model
// the operator's settings put on the `default` role, not the one that session saved: the pane's
// command carries `--model @default`, and @default is Oh My Pi's own name for that role. Measured on the pinned binary: a
// session saved on one model, the profile's `modelRoles.default` moved to another, and the pane's
// own command answering with the profile's current model. The same command with the flag taken
// out is the control, and reports the saved model instead.
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
// operator's current role is resolved per launch (TestInnerCommand).
func TestTheResumedPaneRunsTheConfiguredModel(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dir, "role.md"), []byte("The test's role prompt.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := testbin.OMPEnviron(home)
	run := func(command, stdin string) (stdout, stderr string, exitErr error) {
		t.Helper()
		cmd := exec.CommandContext(testwait.Context(t), shell, "-c", command)
		cmd.Dir, cmd.Env, cmd.Stdin = work, environ, strings.NewReader(stdin)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	// A session of the profile's own, saved on operator/saved: print mode answers one short turn
	// through the test's provider and writes the file the pane resumes below.
	if stdout, stderr, err := run(shellprefix.Word(omp)+" -p 'Reply ok.'", ""); err != nil {
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

	spec := runtime.SpawnSpec{Claim: "legion-omp-legion-43-tester", ResumeSessionFile: saved[0], Prompt: runtime.PromptParts{RolePromptPaths: []string{filepath.Join(dir, "role.md")}}}
	host := Host{StateDir: dir}
	promptFile := writeLaunchPrompt(t, spec, host)
	inner, pairs, err := NewOmpLauncher(nil, shellprefix.Word(omp)).Launch(spec, host, promptFile)
	if want := []string{"-e", runtime.SessionExclusiveVariable + "=1"}; err != nil || !slices.Equal(pairs, want) {
		t.Fatalf("Launch = %q, %v; want the resumed pane's one pair %q", pairs, err, want)
	}
	// Every resume below runs in the resumed pane's environment, its one pair included.
	environ = append(environ, pairs[1])

	// The control, and it runs first: the same command with the flag taken out reads the session's
	// own pin, which at this point is the one the saving turn wrote and nothing else.
	without := strings.Replace(inner, " --model @default", "", 1)
	if without == inner {
		t.Fatalf("the pane's command carries no --model @default: %s", inner)
	}
	plain, stderr, err := run(without, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", without, err, plain, stderr)
	}
	if got := testbin.StateModel(t, plain); got != "operator/saved" {
		t.Errorf("without the flag the resumed pane runs %s, want the session's own operator/saved", got)
	}
	if got, want := testbin.ModelChanges(t, saved[0]), []string{"operator/saved"}; !slices.Equal(got, want) {
		t.Errorf("after the plain control, recorded model changes = %q, want %q", got, want)
	}

	// The pane's own command, which carries the flag: it answers on the profile's role whatever
	// the session was pinned to.
	stdout, stderr, err := run(inner, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", inner, err, stdout, stderr)
	}
	if got := testbin.StateModel(t, stdout); got != "operator/configured" {
		t.Errorf("the resumed pane runs %s, want the profile's operator/configured\nstderr: %s", got, stderr)
	}
	if got, want := testbin.ModelChanges(t, saved[0]), []string{"operator/saved", "operator/configured"}; !slices.Equal(got, want) {
		t.Errorf("after the role-alias resume, recorded model changes = %q, want %q", got, want)
	}

	// Moving the role back cannot recover the old pin: a later plain resume reads the role-alias
	// resume's durable replacement.
	if err := os.WriteFile(filepath.Join(agent, "config.yml"), []byte("modelRoles:\n  default: operator/saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain, stderr, err = run(without, `{"type":"get_state","id":"1"}`+"\n")
	if err != nil {
		t.Fatalf("%s: %v\nstdout: %s\nstderr: %s", without, err, plain, stderr)
	}
	if got, want := testbin.StateModel(t, plain), "operator/configured"; got != want {
		t.Errorf("after the role moves back, the plain resumed pane runs %s, want the durable %s", got, want)
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
