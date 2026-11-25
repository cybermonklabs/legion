package ghtool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// helpDir is gh 2.98.0's saved help, beside `legion gh`'s tests (cmd/legion/testdata/gh-help).
func helpDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "legion", "testdata", "gh-help"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// servedRepositories is whether the test's bundles serve repo, as the daemon's rename rule answers
// it (appauth.GitHubCredential's served): the three repositories of two owners, the issue's own
// `acme` with a second repository beside the one the role writes to, and `Sjawhar`, whose
// repositories the role only reads, compared as GitHub compares names, regardless of case.
func servedRepositories(repo ghrepo.Repository) bool {
	return slices.ContainsFunc([]string{"acme/widgets", "acme/other", "Sjawhar/legion"}, func(served string) bool {
		return strings.EqualFold(served, repo.String())
	})
}

// fakeTokens mints a lease per App role and owner, a read-only one marked as such, as
// appauth.Manager does, and records what it was asked. It answers that each App is installed on
// the two owners of servedRepositories (appauth.OwnerInstallations), which a bundle reads at
// every build.
type fakeTokens struct {
	mu    sync.Mutex
	asked []string
}

func (f *fakeTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return f.lease("ghs_" + string(role) + "_" + strings.ToLower(owner))
}

func (f *fakeTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return f.lease("ghs_" + string(role) + "_" + owner + "_read")
}

func (f *fakeTokens) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return []string{"acme", "sjawhar"}, nil
}

func (f *fakeTokens) lease(token string) (appauth.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, token)
	return appauth.Lease{Token: token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// fakeRecorder is the call record in memory, with a hook each start and finish calls.
type fakeRecorder struct {
	mu       sync.Mutex
	rows     map[string]store.AnswerToolCall
	onStart  func(store.AnswerToolCall)
	onFinish func(question string, seq int)
}

func newFakeRecorder() *fakeRecorder { return &fakeRecorder{rows: map[string]store.AnswerToolCall{}} }

func callKey(question string, seq int) string { return fmt.Sprintf("%s.%d", question, seq) }

func (f *fakeRecorder) StartAnswerToolCall(_ context.Context, call store.AnswerToolCall) error {
	f.mu.Lock()
	if _, ok := f.rows[callKey(call.Question, call.Seq)]; ok {
		f.mu.Unlock()
		return fmt.Errorf("gh call %s.%d %w", call.Question, call.Seq, store.ErrAnswerToolCallRecorded)
	}
	f.rows[callKey(call.Question, call.Seq)] = call
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart(call)
	}
	return nil
}

func (f *fakeRecorder) FinishAnswerToolCall(_ context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	if f.onFinish != nil {
		f.onFinish(question, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[callKey(question, seq)]
	if !ok || row.Result != nil {
		return fmt.Errorf("finish gh call %s.%d: no started, unfinished call has that number", question, seq)
	}
	row.Result, row.Exit, row.FinishedAt = &result, exit, &at
	f.rows[callKey(question, seq)] = row
	return nil
}

func (f *fakeRecorder) AnswerToolCall(_ context.Context, question string, seq int) (store.AnswerToolCall, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[callKey(question, seq)]
	return row, ok, nil
}

func (f *fakeRecorder) row(question string, seq int) (store.AnswerToolCall, bool) {
	row, ok, _ := f.AnswerToolCall(context.Background(), question, seq)
	return row, ok
}

// fixture is one answer of an implementer on acme/widgets, in a project whose owners are acme and
// sjawhar, its gh the stand-in and its shared public leak check a stand-in of its own, which
// passes every call until a test writes another (fixture.leakCheck).
type fixture struct {
	t      *testing.T
	stand  string // the stand-in gh's directory
	check  string // the stand-in shared public leak check's directory
	path   string // the stand-in check itself, the installed check every call runs
	record string // the file that check records every run it was handed in
	host   Host
	tokens *fakeTokens
	calls  *fakeRecorder
	logs   *bytes.Buffer
	answer *Answer
}

// The stand-in shared public leak checks the tool's guard is driven against, leakguardtest's,
// each recording the call it was handed in the file LEAK_CHECK_RECORD names so a test can read
// that the check ran at all and with what (fixture.checked). checkRefuses answers as the check
// answers a write it refuses, its report and exit 3; checkCannotRun as one that could not be made
// to run.
var (
	checkPasses     = leakguardtest.Recording(leakguardtest.Passes)
	checkRefuses    = leakguardtest.Recording(leakguardtest.Refusing(checkRefusal))
	checkCannotRun  = leakguardtest.Recording(leakguardtest.CannotFinish)
	checkReadsStdin = leakguardtest.Recording(leakguardtest.Reading(0, ""))
)

// recordsIn is a stand-in check's script with LEAK_CHECK_RECORD set to record in its first line:
// the guard hands the check no name of this test's own (Answer.guardEnv), so the file it records
// in is written into the script, as leakguardtest.AnswersHelp writes its own.
func recordsIn(record, script string) string {
	return "LEAK_CHECK_RECORD='" + record + "'\n" + script
}

// checkRefusal is the report the refusing stand-in prints, the whole of what its refusal says
// about the call.
const checkRefusal = "LEAK CHECK: refusing the gh call: one of ours"

const question = "dispatch:m-1"

// roundHead is the head the test agents' recorded round read (Agent.Head), and pushedHead the one
// a push has since moved the pull request to; both are spelled as GitHub spells a head, 40
// lowercase hexadecimal characters, the only spelling the review intake reads as one
// (intake.PullRequestReview.LegionHead).
const (
	roundHead  = "8848896da05779c592221b1199ee55ddb11cc3f3"
	pushedHead = "4c1d0b9a7e52f38610d4c2b7a9e5f0318d6b2c47"
)

var implementer = Agent{Issue: "LEG-1", Role: claim.RoleImplementer, Session: "s-1", Phase: "implement", Repository: ghrepo.MustParse("acme/widgets"), AppID: "1001", Head: roundHead}

// reviewer is a stopped reviewer of the same issue, the review App's agent, whose recorded round
// read roundHead.
var reviewer = Agent{Issue: "LEG-1", Role: claim.RoleReviewer, Session: "s-2", Phase: "review", Repository: ghrepo.MustParse("acme/widgets"), AppID: "1002", Head: roundHead}

// headRead is the tool's read of acme/widgets#5's current head, which a review on it is checked
// against before it runs (Answer.headRefusal).
var headRead = []string{"api", "repos/acme/widgets/pulls/5", "--jq", ".head.sha"}

// headAt is the stand-in's answer to headRead: the pull request stands at head.
func headAt(head string) map[string]string {
	return map[string]string{strings.Join(headRead, " "): head + "\n"}
}

var bounds = config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 60000, GHCalls: 100}

// standInGH links this test binary as `gh` into a directory of its own, behaving as b says.
func standInGH(t *testing.T, b behaviour) string {
	t.Helper()
	dir := t.TempDir()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(dir, "gh")); err != nil {
		t.Fatal(err)
	}
	setBehaviour(t, dir, b)
	return dir
}

func setBehaviour(t *testing.T, dir string, b behaviour) {
	t.Helper()
	b.HelpDir = helpDir(t)
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "behaviour.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, agent Agent, b behaviour) *fixture {
	t.Helper()
	f := &fixture{t: t, stand: standInGH(t, b), check: t.TempDir(), tokens: &fakeTokens{}, calls: newFakeRecorder(), logs: &bytes.Buffer{}}
	root := t.TempDir()
	f.record = filepath.Join(root, "leak-check-record")
	f.path = leakguardtest.Write(t, f.check, recordsIn(f.record, checkPasses))
	f.host = Host{
		GH: filepath.Join(f.stand, "gh"), Git: filepath.Join(root, "gitbin", "git"), Legion: filepath.Join(root, "legion"),
		StateDir: filepath.Join(root, "state"), Calls: f.calls, Log: slog.New(slog.NewJSONHandler(f.logs, nil)), Now: time.Now,
		Credential: appauth.GitHubCredential(f.tokens, servedRepositories, ghrepo.Renames{}, nil, nil, slog.New(slog.DiscardHandler)),
		// The guard's own two seams, as the daemon fills them: the check an installed one, as a
		// Kubernetes daemon's is once its install lands, a stand-in of this test's, since the real
		// one is a script no clone of this repository holds, recording in the fixture's record
		// file; and the environment this process's own with a state home of this fixture's added,
		// where the guard keeps the public answers it read (leakguard's visibility cache), so no
		// answer another run kept stands in for this one's read.
		LeakCheck: installedAt(f.path),
		Environ:   append(os.Environ(), "XDG_STATE_HOME="+filepath.Join(root, "daemon-state")),
	}
	answer, err := Open(context.Background(), f.host, agent, question)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = answer.Close() })
	f.answer = answer
	return f
}

// leakCheck writes script as the body of the stand-in shared public leak check this fixture's
// calls are guarded by, recording in the fixture's record file, and replacing the one it started
// with at the same path: the guard runs what is at the check's path at every call, so a call after
// this is checked by the new one.
func (f *fixture) leakCheck(script string) {
	f.t.Helper()
	leakguardtest.Write(f.t, f.check, recordsIn(f.record, script))
}

// checked is what the stand-in check recorded of the calls it was handed, and "" where it was
// never run.
func (f *fixture) checked() string {
	f.t.Helper()
	recorded, err := os.ReadFile(f.record)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	return string(recorded)
}

// checkedCall is what a recording stand-in check (leakguardtest.Recording) writes for the guard's
// run of a gh call that begins with words: the check's gh-guard mode, `--` and the words, the call
// form leak-check-v33 takes (leakguard's modeArgv). It ends with no line end, so words may be the
// call's first words alone.
func checkedCall(words []string) string {
	return "checked gh-guard -- " + strings.Join(words, " ")
}

func args(argv ...string) string {
	data, err := json.Marshal(argv)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// run makes the call seq of the answer, its seq also its number among the answer's gh calls.
func (f *fixture) run(seq int, argv ...string) Result {
	return Run(context.Background(), Call{Answer: f.answer, Seq: seq, N: seq, Args: args(argv...), Bounds: bounds})
}

// invocations are the calls the stand-in gh was run for so far: helps says whether to include
// its help reads. The leak guard's own visibility read is in neither (guardReads): it is a child
// of the guard, not a call the tool made, and a test that counts the runs of a call counts the
// call's own.
func (f *fixture) invocations(helps bool) []invocation {
	f.t.Helper()
	var all []invocation
	for _, inv := range f.allInvocations() {
		if (helps || !inv.Help) && !inv.guardRead() {
			all = append(all, inv)
		}
	}
	return all
}

// guardReads are the leak guard's own reads of a repository's visibility, in the order they were
// made.
func (f *fixture) guardReads() []invocation {
	f.t.Helper()
	var reads []invocation
	for _, inv := range f.allInvocations() {
		if inv.guardRead() {
			reads = append(reads, inv)
		}
	}
	return reads
}

// headReads splits the calls the stand-in gh was run for so far, its help reads left out, into
// the tool's own reads of a pull request's head (Answer.headRefusal), and the rest.
func (f *fixture) headReads() (reads, calls []invocation) {
	f.t.Helper()
	for _, inv := range f.invocations(false) {
		if len(inv.Argv) == 4 && inv.Argv[0] == "api" && inv.Argv[2] == "--jq" && inv.Argv[3] == ".head.sha" {
			reads = append(reads, inv)
		} else {
			calls = append(calls, inv)
		}
	}
	return reads, calls
}

// allInvocations is every run of the stand-in gh recorded so far, whatever ran it.
func (f *fixture) allInvocations() []invocation {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.stand, "invocations.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var all []invocation
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var inv invocation
		if err := json.Unmarshal(scanner.Bytes(), &inv); err != nil {
			f.t.Fatal(err)
		}
		all = append(all, inv)
	}
	return all
}

func (f *fixture) clearInvocations() {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.stand, "invocations.jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
}

// logLines are the JSON log lines with msg.
func (f *fixture) logLines(msg string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			lines = append(lines, entry)
		}
	}
	return lines
}

// wantEnv is exactly the environment a call's gh runs with, its GH_CONFIG_DIR owner's.
func (f *fixture) wantEnv(owner string) []string {
	a := f.answer
	return slices.Sorted(slices.Values([]string{
		"PATH=" + f.stand + ":" + filepath.Dir(f.host.Git),
		"HOME=" + a.home, "XDG_CONFIG_HOME=" + a.config, "XDG_CACHE_HOME=" + a.cache, "XDG_DATA_HOME=" + a.data, "XDG_STATE_HOME=" + a.state,
		"GH_CONFIG_DIR=" + filepath.Join(a.dir, "owners", owner), "GH_PATH=" + filepath.Join(a.dir, "bin", "gh"),
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1",
	}))
}

// Open writes the agent's bundle as a pane's is written: the role's App (implement for an
// implementer, review for a reviewer) with its own installation lease, at the role's own
// permissions, in a directory for every owner the App is installed on, the issue's owner's the
// default, under a fresh 0700 <state_dir>/answers/<question>/gh, what an earlier run left there
// gone; Close removes it.
func TestOpenWritesTheAgentsBundleAsAPanesAndCloseRemovesIt(t *testing.T) {
	for _, tc := range []struct {
		agent Agent
		app   string
	}{
		{implementer, "implement"},
		{Agent{Issue: "LEG-1", Role: claim.RoleReviewer, Session: "s-2", Phase: "review", Repository: ghrepo.MustParse("Acme/widgets")}, "review"},
	} {
		stale := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(stale, "answers", "dispatch:m-1", "gh")
		if err := os.MkdirAll(filepath.Join(dir, "leftover"), 0o755); err != nil {
			t.Fatal(err)
		}
		tokens := &fakeTokens{}
		host := Host{
			GH: "/opt/gh/gh", Legion: "/opt/legion/legion", StateDir: stale, LeakCheck: installedAt("/opt/legion/leak-check/public-leak-check.sh"),
			Calls: newFakeRecorder(), Log: slog.New(slog.DiscardHandler), Now: time.Now,
			Credential: appauth.GitHubCredential(tokens, servedRepositories, ghrepo.Renames{}, nil, nil, slog.New(slog.DiscardHandler)),
		}
		answer, err := Open(context.Background(), host, tc.agent, question)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: the answer's directory %s = %v, %v; want 0700", tc.app, dir, info, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "leftover")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: what an earlier run left in %s is still there (%v)", tc.app, dir, err)
		}
		for owner, want := range map[string]string{"acme": "ghs_" + tc.app + "_acme", "sjawhar": "ghs_" + tc.app + "_sjawhar"} {
			hosts, err := os.ReadFile(filepath.Join(dir, "owners", owner, ghconfig.HostsFile))
			if err != nil {
				t.Fatal(err)
			}
			if token, err := ghconfig.TokenFromHosts(hosts); err != nil || token != want {
				t.Errorf("%s: %s's gh directory holds %q, %v; want %q", tc.app, owner, token, err, want)
			}
		}
		if answer.bundle.Default != "acme" {
			t.Errorf("%s: the bundle's default owner is %q, want the issue's owner acme", tc.app, answer.bundle.Default)
		}
		if err := answer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: after Close %s = %v, want it gone", tc.app, dir, err)
		}
	}
}

// The check the guard runs is the host's to name (Host.LeakCheck), and never the guard's to
// resolve: a host that names none is refused at Open, before any directory is made.
func TestOpenRefusesAHostThatNamesNoLeakCheck(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	host := Host{
		GH: "/opt/gh/gh", Legion: "/opt/legion/legion", StateDir: state, Calls: newFakeRecorder(), Log: slog.New(slog.DiscardHandler), Now: time.Now,
		Credential: appauth.GitHubCredential(&fakeTokens{}, servedRepositories, ghrepo.Renames{}, nil, nil, slog.New(slog.DiscardHandler)),
	}
	answer, err := Open(context.Background(), host, implementer, question)
	const want = "the gh tool's host names no shared public leak check for its guard to run before every call"
	if err == nil || err.Error() != want {
		if answer != nil {
			_ = answer.Close()
		}
		t.Fatalf("Open with no check opened an answer (%t) with the error %v; want the refusal %q", answer != nil, err, want)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Open made %s (%v), want nothing made", state, err)
	}
}

// Every refused form never starts gh: a command outside the allow list, an option opening a
// browser, gh api's --hostname, and every write form whose text the tool cannot add the footer to;
// arguments that are no JSON array of strings. A command whose gh directory cannot be read for
// certain, or that names an owner the role's App is not installed on, is refused once gh's help
// is read, the command itself never run: all but a read naming only such owners, which runs with
// the bundle's default directory as a pane's `legion gh` runs it (R4 of dispatch://INFRA-3399).
// Each refusal names what it refuses, is what the model reads, and is recorded with no exit.
func TestRefusedFormsNeverRun(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "ran"})
	for i, tc := range []struct {
		argv     []string
		raw      string // the call's arguments as given, when no JSON array
		want     string
		readHelp bool // refused by the routing, after reading gh's help
	}{
		{argv: []string{"extension", "install", "owner/gh-x"}, want: `"extension" is refused`},
		{argv: []string{"alias", "set", "co", "pr checkout"}, want: `"alias" is refused`},
		{argv: []string{"config", "get", "git_protocol"}, want: `"config" is refused`},
		{argv: []string{"auth", "token"}, want: `"auth" is refused`},
		{argv: []string{"codespace", "list"}, want: `"codespace" is refused`},
		{argv: []string{"browse", "-R", "acme/widgets"}, want: `"browse" is refused`},
		{argv: []string{"repo", "clone", "acme/widgets"}, want: `"repo clone" is refused`},
		{argv: []string{"repo", "list"}, want: `"repo list" is refused`},
		{argv: []string{"repo"}, want: `"repo" is refused`},
		{argv: []string{"pr", "checkout", "5", "-R", "acme/widgets"}, want: `"checkout" is refused`},
		{argv: []string{"-R", "acme/widgets", "pr", "co", "5"}, want: `"co" is refused`},
		{argv: []string{"gist", "list"}, want: `"gist" is refused`},
		{argv: []string{"secret", "list", "-R", "acme/widgets"}, want: `"secret" is refused`},
		{argv: []string{"ssh-key", "list"}, want: `"ssh-key" is refused`},
		{argv: []string{"gpg-key", "list"}, want: `"gpg-key" is refused`},
		{argv: []string{"release", "download", "v1", "-R", "acme/widgets"}, want: `"release download" is refused`},
		{argv: []string{"release", "upload", "v1", "a.tgz", "-R", "acme/widgets"}, want: `"release upload" is refused`},
		{argv: []string{"release", "create", "v1", "-R", "acme/widgets"}, want: `"release create" is refused`},
		{argv: []string{"run", "download", "7", "-R", "acme/widgets"}, want: `"download" is refused`},
		{argv: []string{"issue", "create", "-R", "acme/widgets", "--title", "t", "--body", "b"}, want: `"issue create" is refused`},
		{argv: []string{"co", "5"}, want: `"co" is refused`},
		{argv: []string{"help", "pr"}, want: `"help" is refused`},
		{argv: []string{"--version"}, want: `"--version" is refused`},
		{argv: []string{}, want: "names no gh command"},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "--web"}, want: `"--web" opens a web browser`},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "-w"}, want: `"-w" opens a web browser`},
		{argv: []string{"run", "view", "7", "-R", "acme/widgets", "--web=true"}, want: "opens a web browser"},
		{argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/widgets"}, want: "--hostname sends the request to another host"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "--body-file", "body.md"}, want: "use --body||pass the text itself with --body"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "-F", "body.md"}, want: "pass the text itself with --body"},
		{argv: []string{"issue", "comment", "3", "-R", "acme/widgets", "-Fbody.md"}, want: "pass the text itself with --body"},
		{argv: []string{"pr", "review", "5", "-R", "acme/widgets", "--approve", "-F", "-"}, want: "pass the text itself with --body"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "--editor"}, want: "--editor opens an editor"},
		{argv: []string{"pr", "comment", "5", "-R", "acme/widgets", "-e"}, want: "--editor opens an editor"},
		{argv: []string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=@body.md"}, want: `"body=@body.md" reads a field's value from a file`},
		{argv: []string{"api", "repos/acme/widgets/issues/5/comments", "--input", "body.json"}, want: "--input reads the request's body from a file"},
		{argv: []string{"api", "graphql", "-f", "query=mutation { addComment(input: {subjectId: \"x\", body: \"y\"}) { clientMutationId } }"}, want: "make the write through its REST endpoint"},
		{argv: []string{"workflow", "run", "ci.yml", "-R", "acme/widgets", "-F", "token=@/etc/passwd"}, want: "reads a field's value from a file"},
		{argv: []string{"pr", "create", "-R", "acme/widgets", "--recover", "state.json"}, want: "reads a file on the daemon's host"},
		{raw: `pr view 5`, want: "takes gh's arguments as a JSON array of strings"},
		{raw: `["pr", 5]`, want: "takes gh's arguments as a JSON array of strings"},
		{argv: []string{"api", "-X", "POST", "repos/other/thing/issues/5/comments", "-f", "body=Done."}, want: `the role's GitHub App is not installed on "other"`, readHelp: true},
		{argv: []string{"pr", "view", "5", "-R", "acme/widgets", "--bogus"}, want: `"--bogus" is no option`, readHelp: true},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, want: "more than one owner", readHelp: true},
		// An absolute endpoint that is not GitHub's own https://api.github.com/…: gh sends the
		// agent's token to the host and scheme it names, so an http:// one hands it over in the
		// clear before GitHub's 301 (the routing refuses it, for this caller and `legion gh`).
		{argv: []string{"api", "http://api.github.com/repos/acme/widgets"}, want: "is not https", readHelp: true},
		{argv: []string{"api", "-X", "POST", "HTTP://API.GITHUB.COM/repos/acme/widgets/issues/5/comments", "-f", "body=Done."}, want: "is not https", readHelp: true},
		{argv: []string{"api", "https://ghe.example.com/api/v3/repos/acme/widgets"}, want: "another host than api.github.com", readHelp: true},
	} {
		seq := i + 1
		f.clearInvocations()
		raw := tc.raw
		if raw == "" {
			raw = args(tc.argv...)
		}
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, N: seq, Args: raw, Bounds: bounds})
		ok := slices.ContainsFunc(strings.Split(tc.want, "||"), func(want string) bool { return strings.Contains(got.Output, want) })
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !ok {
			t.Errorf("gh %s = %+v; want a refusal containing %q, gh never run", raw, got, tc.want)
		}
		if ran := f.invocations(false); len(ran) != 0 {
			t.Errorf("gh %s ran gh: %+v", raw, ran)
		}
		if helps := f.invocations(true); !tc.readHelp && len(helps) != 0 {
			t.Errorf("gh %s started gh to read its help (%+v); a command the tool refuses by its words starts nothing", raw, helps)
		}
		row, recorded := f.calls.row(question, seq)
		if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.FinishedAt == nil {
			t.Errorf("gh %s is recorded as %+v (%t); want its refusal and no exit", raw, row, recorded)
		}
	}
}

// missingUsage are the words of agent's usage that output lacks: that gh runs with no checkout,
// -R and the other ways a call names its repository, the agent's repository, its issue's pull
// request where the record holds one, every command the tool takes and the example call as the
// tool's input; and, where the record holds no pull request, any pull request named at all.
func missingUsage(agent Agent, output string) []string {
	example, err := json.Marshal(map[string][]string{"args": agent.Usage().Example()})
	if err != nil {
		panic(err)
	}
	var missing []string
	for _, want := range []string{
		"no checkout of any repository", "-R <owner>/<repo>", "a github.com URL", "repos/<owner>/<repo>/… endpoint",
		"Your issue's repository is acme/widgets", allowedList, string(example),
	} {
		if !strings.Contains(output, want) {
			missing = append(missing, want)
		}
	}
	switch named := fmt.Sprintf("its pull request is #%d", agent.PullRequest); {
	case agent.PullRequest > 0 && !strings.Contains(output, named):
		missing = append(missing, named)
	case agent.PullRequest == 0 && strings.Contains(output, "pull request"):
		missing = append(missing, "no pull request named")
	}
	return missing
}

// A call naming no command the tool takes is refused with how the tool is called (Usage): a call
// naming no command at all, which is what gh's own --help or -h alone reads as, gh's --version, a
// command gh has and the tool does not, and one the allow list refuses. The refusal says gh runs
// with no checkout, so a call names -R <owner>/<repo> or the other two forms, and names the
// agent's repository, its issue's pull request where the record holds one and none where it holds
// none, every command the tool takes, and an example call, which runs as written. A call gh ran
// that failed for naming no repository carries gh's output and then the same usage, recorded as
// the model reads it; one that failed for another reason carries gh's output alone, and a call
// refused for another reason gives its own reason. The sentence the allow list refused with before,
// which named the commands alone, is held to the same words first and fails them, so the words
// are what the test reads and not that a refusal said something.
func TestACallNamingNoCommandOrNoRepositoryIsToldHowToCallTheTool(t *testing.T) {
	before := "refused: `gh --version` is not one of the gh tool's commands (\"--version\" is refused); the gh tool runs only " + allowedList
	if missing := missingUsage(implementer, before); len(missing) == 0 {
		t.Fatalf("the refusal before the usage, %q, carries every word of the usage", before)
	}
	withPullRequest := implementer
	withPullRequest.PullRequest = 7
	for _, agent := range []Agent{withPullRequest, implementer} {
		t.Run(fmt.Sprintf("pull request %d", agent.PullRequest), func(t *testing.T) {
			f := newFixture(t, agent, behaviour{Stdout: "ran\n"})
			seq := 0
			for _, argv := range [][]string{{}, {"--version"}, {"--help"}, {"-h"}, {"status"}, {"repo", "clone", "x"}} {
				seq++
				f.clearInvocations()
				got := f.run(seq, argv...)
				if got.Ran || !strings.HasPrefix(got.Output, "refused: ") {
					t.Errorf("gh %q = %+v; want it refused", argv, got)
				}
				if missing := missingUsage(agent, got.Output); len(missing) > 0 {
					t.Errorf("gh %q is refused with %q, which lacks %q", argv, got.Output, missing)
				}
				if ran := f.invocations(true); len(ran) != 0 {
					t.Errorf("gh %q started gh: %+v", argv, ran)
				}
			}

			seq++
			f.clearInvocations()
			example := agent.Usage().Example()
			if got := f.run(seq, example...); !got.Ran || got.Exit != 0 || got.Output != "ran\n" {
				t.Errorf("the example gh %q = %+v; want it run as written", example, got)
			}
			if ran := f.invocations(false); len(ran) != 1 || !slices.Equal(ran[0].Argv, example) {
				t.Errorf("the example gh %q ran gh as %+v; want it once, as written", example, ran)
			}

			seq++
			if got := f.run(seq, "pr", "view", "5", "-R", "acme/widgets", "--web"); !strings.Contains(got.Output, `"--web" opens a web browser`) {
				t.Errorf("a call opening a browser is refused with %q; want its own reason", got.Output)
			}

			const gitFailure = "failed to run git: fatal: not a git repository (or any of the parent directories): .git\n"
			setBehaviour(t, f.stand, behaviour{Stderr: gitFailure, Exit: 1})
			seq++
			got := f.run(seq, "issue", "list", "--limit", "1")
			if !got.Ran || got.Exit != 1 || !strings.HasPrefix(got.Output, gitFailure) {
				t.Errorf("gh issue list naming no repository = %+v; want gh's failure first", got)
			}
			if missing := missingUsage(agent, strings.TrimPrefix(got.Output, gitFailure)); len(missing) > 0 {
				t.Errorf("gh issue list naming no repository gives %q, which lacks %q after gh's output", got.Output, missing)
			}
			if row, _ := f.calls.row(question, seq); row.Result == nil || *row.Result != got.Output || row.Exit == nil || *row.Exit != 1 {
				t.Errorf("the failed call is recorded as %+v; want what the model read and exit 1", row)
			}

			setBehaviour(t, f.stand, behaviour{Stderr: "HTTP 404: Not Found\n", Exit: 1})
			seq++
			if got := f.run(seq, "pr", "view", "5", "-R", "acme/widgets"); got.Output != "HTTP 404: Not Found\n" {
				t.Errorf("a call that failed for another reason gives %q; want gh's output alone", got.Output)
			}
		})
	}
}

// Every allowed form runs once, with exactly its arguments, the gh directory of the owner it
// names (the issue owner's for a command naming none), exactly the scrubbed environment, and the
// answer's empty working directory, HOME and XDG directories. Its record starts before gh runs
// and finishes after, with gh's output and exit, and one log line names its App and owner.
func TestAllowedFormsRunWithTheRoutedOwnerAndTheScrubbedEnvironment(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer\n"})
	ranAtStart := map[int]int{}
	f.calls.onStart = func(call store.AnswerToolCall) { ranAtStart[call.Seq] = len(f.invocations(false)) }
	ranAtFinish := map[int]int{}
	f.calls.onFinish = func(_ string, seq int) { ranAtFinish[seq] = len(f.invocations(false)) }
	for i, tc := range []struct {
		argv  []string
		owner string
	}{
		{[]string{"pr", "view", "5", "-R", "sjawhar/legion", "--json", "title"}, "sjawhar"},
		{[]string{"-R", "acme/widgets", "pr", "ls"}, "acme"},
		{[]string{"pr", "diff", "5", "-R", "acme/widgets"}, "acme"},
		{[]string{"pr", "checks", "5", "-R", "acme/widgets"}, "acme"},
		{[]string{"api", "repos/sjawhar/legion/contents/README.md"}, "sjawhar"},
		{[]string{"api", "user"}, "acme"},
		{[]string{"search", "issues", "repo:sjawhar/legion", "is:open"}, "sjawhar"},
		{[]string{"run", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"run", "view", "7", "-R", "acme/widgets", "--log-failed"}, "acme"},
		{[]string{"workflow", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"repo", "view", "sjawhar/legion", "--json", "name"}, "sjawhar"},
		{[]string{"release", "list", "-R", "sjawhar/legion"}, "sjawhar"},
		{[]string{"release", "view", "v1", "-R", "sjawhar/legion"}, "sjawhar"},
		{[]string{"issue", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"issue", "view", "3", "-R", "acme/widgets"}, "acme"},
		{[]string{"label", "list", "-R", "acme/widgets"}, "acme"},
		{[]string{"cache", "list", "-R", "acme/widgets"}, "acme"},
		// Writes that post no comment or review run as they are.
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--squash"}, "acme"},
		{[]string{"run", "rerun", "7", "-R", "acme/widgets", "--failed"}, "acme"},
		{[]string{"workflow", "run", "ci.yml", "-R", "acme/widgets", "-f", "level=1"}, "acme"},
		{[]string{"api", "repos/acme/widgets/issues/5/comments"}, "acme"},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		if !got.Ran || got.Exit != 0 || got.Output != "gh's answer\n" {
			t.Errorf("gh %q = %+v; want gh's answer, exit 0", tc.argv, got)
			continue
		}
		ran := f.invocations(false)
		if len(ran) != 1 {
			t.Fatalf("gh %q ran gh %d times: %+v", tc.argv, len(ran), ran)
		}
		inv := ran[0]
		if !slices.Equal(inv.Argv, tc.argv) {
			t.Errorf("gh %q ran gh with %q, want its arguments as given", tc.argv, inv.Argv)
		}
		if want := f.wantEnv(tc.owner); !slices.Equal(inv.Env, want) {
			t.Errorf("gh %q ran with the environment\n%q\nwant exactly\n%q", tc.argv, inv.Env, want)
		}
		if inv.Cwd != f.answer.cwd {
			t.Errorf("gh %q ran in %s, want the answer's working directory %s", tc.argv, inv.Cwd, f.answer.cwd)
		}
		for name, empty := range inv.Empty {
			if !empty {
				t.Errorf("gh %q ran with %s not empty", tc.argv, name)
			}
		}
		if ranAtStart[seq] != 0 || ranAtFinish[seq] != 1 {
			t.Errorf("gh %q: gh had run %d times when its record started and %d when it finished; want 0 and 1", tc.argv, ranAtStart[seq], ranAtFinish[seq])
		}
		row, _ := f.calls.row(question, seq)
		if row.Result == nil || *row.Result != got.Output || row.Exit == nil || *row.Exit != 0 || row.Owner != tc.owner || row.App != "implement" || !slices.Equal(row.Args, tc.argv) {
			t.Errorf("gh %q is recorded as %+v; want its output, exit 0, App implement and owner %s", tc.argv, row, tc.owner)
		}
	}
	var logged bool
	for _, line := range f.logLines("answers: gh") {
		if line["seq"] == float64(1) {
			logged = true
			if line["question"] != question || line["app"] != "implement" || line["appID"] != "1001" || line["owner"] != "sjawhar" || line["exit"] != float64(0) || fmt.Sprint(line["args"]) != fmt.Sprint([]any{"pr", "view", "5", "-R", "sjawhar/legion", "--json", "title"}) {
				t.Errorf("the log line of call 1 = %v; want its question, App implement 1001, owner sjawhar, arguments and exit 0", line)
			}
		}
	}
	if !logged {
		t.Errorf("no `answers: gh` line for call 1 in %s", f.logs)
	}
}

// The tool adds the agent's footer to every comment or review it posts: pr and issue comment's
// --body in each spelling, a review's --body, a bare --approve, --request-changes or --comment as
// --body with the footer alone, a closing or reopening --comment, a gh api write's literal body
// field, and `-f body=<footer>` on a POST creating or submitting a review with none. A review's
// footer names the head the agent's recorded round read, once the pull request's head is read to
// be that one, and a comment's names none. The review intake still reads the agent's session
// from it, and a review's head.
func TestTheFooterIsAddedToEveryWriteForm(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n", Responses: headAt(roundHead)})
	foot := func(seq int, review bool) string {
		head := ""
		if review {
			head = `"head":"` + roundHead + `",`
		}
		return `<!-- legion: {"session":"s-1","phase":"implement",` + head + `"answer":"dispatch:m-1.` + fmt.Sprint(seq) + `"} -->`
	}
	repo := []string{"-R", "acme/widgets"}
	for i, tc := range []struct {
		argv   []string
		review bool
		want   func(foot string) []string
	}{
		{append([]string{"pr", "comment", "5", "--body", "Done."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-b", "Done."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "comment", "5", "-b", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--body=Done."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body=Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-bDone."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "comment", "5", "-bDone.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited.\n\n" + s}, repo...)
		}},
		{append([]string{"issue", "comment", "3", "--body", "Done."}, repo...), false, func(s string) []string {
			return append([]string{"issue", "comment", "3", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve", "--body", "LGTM"}, repo...), true, func(s string) []string {
			return append([]string{"pr", "review", "5", "--approve", "--body", "LGTM\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve"}, repo...), true, func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--approve"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "--request-changes"}, repo...), true, func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--request-changes"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "-c"}, repo...), true, func(s string) []string {
			return append(append([]string{"pr", "review", "5", "-c"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "close", "5", "-c", "Superseded by #6."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "close", "5", "-c", "Superseded by #6.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "reopen", "5", "--comment", "Back."}, repo...), false, func(s string) []string {
			return append([]string{"pr", "reopen", "5", "--comment", "Back.\n\n" + s}, repo...)
		}},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done."}, false, func(s string) []string {
			return []string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done."}, false, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done."}, false, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done.\n\n" + s}
		}},
		{[]string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited."}, false, func(s string) []string {
			return []string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE"}, true, func(s string) []string {
			return []string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "body=" + s}
		}},
		{[]string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT"}, true, func(s string) []string {
			return []string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT", "-f", "body=" + s}
		}},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		want := tc.want(foot(seq, tc.review))
		reads, ran := f.headReads()
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with %q", tc.argv, got, ran, want)
			continue
		}
		wantReads, wantHead := 0, ""
		if tc.review {
			wantReads, wantHead = 1, roundHead
		}
		if len(reads) != wantReads {
			t.Errorf("gh %q read the pull request's head %d times, want %d", tc.argv, len(reads), wantReads)
		}
		if env := f.wantEnv("acme"); !slices.Equal(ran[0].Env, env) {
			t.Errorf("gh %q ran with %q, want %q", tc.argv, ran[0].Env, env)
		}
		posted := intake.PullRequestReview{Body: want[slices.IndexFunc(want, func(arg string) bool { return strings.Contains(arg, "<!-- legion:") })]}
		if session := posted.LegionSession(); session != "s-1" {
			t.Errorf("the review intake reads the session %q from %q, want s-1", session, posted.Body)
		}
		if head := posted.LegionHead(); head != wantHead {
			t.Errorf("the review intake reads the head %q from %q, want %q", head, posted.Body, wantHead)
		}
	}
}

// A call is routed by its arguments as the model gave them, and gh is handed them with the footer
// added: the footer adds a body and names no repository, so the two route alike. For each form
// the tool adds a footer to — a comment's --body, a bare review's --body of the footer alone, and
// a gh api review's body field, given or added — routing the model's arguments and routing the
// call with its footer name the same target, owner and gh directory, those of the repository the
// call names; and the call runs once with its footer, which is what the leak guard read, under
// that owner's directory.
func TestACallIsRoutedByTheModelsArgumentsAndRunsWithItsFooter(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n", Responses: headAt(roundHead)})
	ctx := context.Background()
	for i, tc := range []struct {
		argv   []string
		review bool
	}{
		{[]string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}, false},
		{[]string{"pr", "review", "5", "-R", "acme/widgets", "--approve"}, true},
		{[]string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "body=LGTM"}, true},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE"}, true},
	} {
		seq := i + 1
		head := ""
		if tc.review {
			head = roundHead
		}
		run, why := withFooter(tc.argv, commandWords(tc.argv), footer(implementer, question, seq, head))
		if why != "" || slices.Equal(run, tc.argv) {
			t.Fatalf("gh %q with its footer = %q (%s); want the call with the footer added", tc.argv, run, why)
		}
		target, owner, dir, err := f.answer.route(ctx, tc.argv)
		if err != nil {
			t.Fatalf("route gh %q: %v", tc.argv, err)
		}
		runTarget, runOwner, runDir, err := f.answer.route(ctx, run)
		if err != nil {
			t.Fatalf("route gh %q: %v", run, err)
		}
		if !reflect.DeepEqual(runTarget, target) || runOwner != owner || runDir != dir {
			t.Errorf("gh %q routes to %+v, owner %s, directory %s; with its footer, to %+v, owner %s, directory %s",
				tc.argv, target, owner, dir, runTarget, runOwner, runDir)
		}
		if target.Owner != "acme" || target.Repo != "widgets" || owner != "acme" {
			t.Errorf("gh %q routes to %+v as %s; want acme/widgets, the repository it names, as acme", tc.argv, target, owner)
		}
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		_, ran := f.headReads()
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, run) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with its footer, %q", tc.argv, got, ran, run)
			continue
		}
		if env := f.wantEnv(owner); !slices.Equal(ran[0].Env, env) {
			t.Errorf("gh %q ran with %q, want %q", tc.argv, ran[0].Env, env)
		}
		if checked := f.checked(); !strings.Contains(checked, checkedCall(run)) {
			t.Errorf("the check was handed %q, want the whole call with its footer, %q", checked, run)
		}
	}
}

// A call is routed by the pick a pane's `legion gh` makes, over the answer's own bundle
// (ghroute.DirFor), the configured renames that bundle carries included: a repository a pair moved
// to another owner is answered from the directory of the owner holding it now, every other
// repository of the owner it moved from from that owner's own, and the owner the call runs as is
// the one whose directory it runs with. A call whose repositories route to two directories is
// refused, naming both, as `legion gh` refuses it.
func TestACallNamingARenamedRepositoryRunsWithTheDirectoryOfTheOwnerHoldingIt(t *testing.T) {
	root := t.TempDir()
	moved := ghrepo.MustRenames(ghrepo.MustRename("acme/legacy", "Sjawhar/legion"))
	host := Host{
		GH: filepath.Join(standInGH(t, behaviour{}), "gh"), Legion: filepath.Join(root, "legion"), StateDir: filepath.Join(root, "state"),
		LeakCheck: installedAt(filepath.Join(root, "leak-check", "public-leak-check.sh")), Calls: newFakeRecorder(), Log: slog.New(slog.DiscardHandler), Now: time.Now,
		Credential: appauth.GitHubCredential(&fakeTokens{}, servedRepositories, moved, nil, nil, slog.New(slog.DiscardHandler)),
	}
	answer, err := Open(context.Background(), host, implementer, question)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = answer.Close() })
	ctx := context.Background()
	for _, tc := range []struct {
		argv             []string
		owner, repo, dir string
	}{
		{[]string{"pr", "view", "1", "-R", "acme/legacy"}, "acme", "legacy", "sjawhar"},
		{[]string{"api", "repos/acme/legacy/pulls/1"}, "acme", "legacy", "sjawhar"},
		{[]string{"pr", "view", "1", "-R", "acme/widgets"}, "acme", "widgets", "acme"},
		{[]string{"pr", "view", "1", "-R", "sjawhar/legion"}, "sjawhar", "legion", "sjawhar"},
	} {
		target, owner, dir, err := answer.route(ctx, tc.argv)
		if err != nil || target.Owner != tc.owner || target.Repo != tc.repo || owner != tc.dir || dir != filepath.Join(answer.owners, tc.dir) {
			t.Errorf("gh %q routes to %+v as %s with %s, %v; want %s/%s as %s with %s's directory", tc.argv, target, owner, dir, err, tc.owner, tc.repo, tc.dir, tc.dir)
		}
	}
	argv := []string{"search", "issues", "--repo", "acme/legacy", "--repo", "acme/widgets"}
	if target, _, _, err := answer.route(ctx, argv); err == nil || !strings.Contains(err.Error(), "more than one owner") ||
		!strings.Contains(err.Error(), "sjawhar (--repo acme/legacy)") || !strings.Contains(err.Error(), "acme (--repo acme/widgets)") {
		t.Errorf("gh %q routes to %+v, %v; want a refusal naming both directories", argv, target, err)
	}
}

// A call may end its options with `--`, after which gh reads every argument as a positional
// (pflag stops at it): `gh pr review -R acme/widgets --approve -- 5`. An option the tool adds for
// the footer — a bare review's --body, a gh api review's -f body= — goes before that `--`, where gh
// reads it as the option it is, so the call with its footer routes as the model's arguments do
// and gh posts the body with the footer. The control is the same option added after the `--`, as
// the tool once added it: gh reads it as arguments its usage has no place for, and so does the
// routing, which refuses it.
func TestAFooterAddedToACallGoesBeforeTheDashDashEndingItsOptions(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n", Responses: headAt(roundHead)})
	ctx := context.Background()
	for i, tc := range []struct {
		argv  []string
		added func(foot string) []string // the options the footer adds
	}{
		{[]string{"pr", "review", "-R", "acme/widgets", "--approve", "--", "5"}, func(s string) []string { return []string{"--body", s} }},
		{[]string{"api", "-X", "POST", "--", "repos/acme/widgets/pulls/5/reviews"}, func(s string) []string { return []string{"-f", "body=" + s} }},
		{[]string{"api", "-f", "event=APPROVE", "--", "repos/acme/widgets/pulls/5/reviews"}, func(s string) []string { return []string{"-f", "body=" + s} }},
	} {
		seq := i + 1
		foot := footer(implementer, question, seq, roundHead)
		dash := slices.Index(tc.argv, "--")
		want := slices.Concat(tc.argv[:dash], tc.added(foot), tc.argv[dash:])
		run, why := withFooter(tc.argv, commandWords(tc.argv), foot)
		if why != "" || !slices.Equal(run, want) {
			t.Errorf("gh %q with its footer = %q (%s); want %q, the footer before the --", tc.argv, run, why, want)
			continue
		}
		target, owner, dir, err := f.answer.route(ctx, tc.argv)
		if err != nil {
			t.Fatalf("route gh %q: %v", tc.argv, err)
		}
		runTarget, runOwner, runDir, err := f.answer.route(ctx, run)
		if err != nil {
			t.Fatalf("route gh %q: %v", run, err)
		}
		if !reflect.DeepEqual(runTarget, target) || runOwner != owner || runDir != dir || owner != "acme" || target.Repo != "widgets" {
			t.Errorf("gh %q routes to %+v, owner %s, directory %s; with its footer, to %+v, owner %s, directory %s; want both acme/widgets as acme",
				tc.argv, target, owner, dir, runTarget, runOwner, runDir)
		}
		after := slices.Concat(tc.argv, tc.added(foot))
		if _, _, _, err := f.answer.route(ctx, after); err == nil || !strings.Contains(err.Error(), "usage has no place for") {
			t.Errorf("gh %q, the footer after its --, routes with error %v; want it refused as arguments the usage has no place for", after, err)
		}
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		_, ran := f.headReads()
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with %q", tc.argv, got, ran, want)
			continue
		}
		if env := f.wantEnv(owner); !slices.Equal(ran[0].Env, env) {
			t.Errorf("gh %q ran with %q, want %q", tc.argv, ran[0].Env, env)
		}
	}
}

// A body whose text begins with a hyphen — a removed line of a diff quoted back, `-Fixed in
// abc123` — is text, not an option: the tool reads a write form's options as pflag does and never
// reads the values they are given, so the call runs with the footer added to the body as given
// (a review's naming the head its round read, at which the pull request stands). A browser or
// file option of its own is still refused, in either spelling, and nothing runs.
func TestABodyBeginningWithAHyphenIsTextAndTheCallRuns(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n", Responses: headAt(roundHead)})
	foot := func(seq int, argv []string) string {
		head := ""
		if argv[1] == "review" {
			head = `"head":"` + roundHead + `",`
		}
		return `<!-- legion: {"session":"s-1","phase":"implement",` + head + `"answer":"dispatch:m-1.` + fmt.Sprint(seq) + `"} -->`
	}
	repo := []string{"-R", "acme/widgets"}
	leak := "-worker.Stop() is gone, so the leak is fixed"
	fixed := "-Fixed in abc123"
	seq := 0
	for _, tc := range []struct {
		argv []string
		want func(foot string) []string
	}{
		{append([]string{"pr", "comment", "5", "--body", leak}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body", leak + "\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-b", fixed}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "-b", fixed + "\n\n" + s}, repo...)
		}},
		{append([]string{"issue", "comment", "3", "--body", fixed}, repo...), func(s string) []string {
			return append([]string{"issue", "comment", "3", "--body", fixed + "\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve", "--body", leak}, repo...), func(s string) []string {
			return append([]string{"pr", "review", "5", "--approve", "--body", leak + "\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "close", "5", "--comment", fixed}, repo...), func(s string) []string {
			return append([]string{"pr", "close", "5", "--comment", fixed + "\n\n" + s}, repo...)
		}},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=" + fixed}, func(s string) []string {
			return []string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=" + fixed + "\n\n" + s}
		}},
		// A pull request of the agent's own carries no footer, and its title and body are text
		// too: the call runs with the arguments as given.
		{append([]string{"pr", "create", "--title", fixed, "--body", leak}, repo...), func(string) []string {
			return append([]string{"pr", "create", "--title", fixed, "--body", leak}, repo...)
		}},
	} {
		seq++
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		want := tc.want(foot(seq, tc.argv))
		_, ran := f.headReads()
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with %q", tc.argv, got, ran, want)
		}
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{append([]string{"pr", "comment", "5", "--web"}, repo...), "opens a web browser"},
		{append([]string{"pr", "comment", "5", "-w"}, repo...), "opens a web browser"},
		{append([]string{"pr", "comment", "5", "--body-file", "body.md"}, repo...), "pass the text itself with --body"},
		{append([]string{"pr", "review", "5", "--approve", "-F", "body.md"}, repo...), "pass the text itself with --body"},
	} {
		seq++
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		if got.Ran || !strings.Contains(got.Output, tc.want) || len(f.invocations(true)) != 0 {
			t.Errorf("gh %q = %+v, ran %+v; want a refusal containing %q with gh never started", tc.argv, got, f.invocations(true), tc.want)
		}
	}
}

// A review at the head the agent's recorded round read runs once the tool has read that its pull
// request still stands there, and the body gh is given carries a footer naming that head: the
// review intake reads it as the head the review read, beside the agent's session, which is what
// lets a review App's review decide the round (workflow's refusesRound). The footer a call
// carried before reviews named their head gives the intake no head at all. The head is read
// first, as a lookup reads, with the routed owner's gh directory and the scrubbed environment, of
// the pull request each form names: by number and -R, by URL, and by a gh api endpoint.
func TestAReviewAtTheRecordedHeadRunsWithThatHeadInItsFooter(t *testing.T) {
	f := newFixture(t, reviewer, behaviour{Stdout: "reviewed\n", Responses: headAt(roundHead)})
	for i, argv := range [][]string{
		{"pr", "review", "5", "-R", "acme/widgets", "--approve", "--body", "Approved."},
		{"pr", "review", "https://github.com/acme/widgets/pull/5", "--request-changes", "--body", "Changes."},
		{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "commit_id=" + roundHead, "-f", "body=Approved."},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, argv...)
		ran := f.invocations(false)
		if !got.Ran || got.Exit != 0 || got.Output != "reviewed\n" || len(ran) != 2 {
			t.Errorf("gh %q = %+v, ran %+v; want the head read, then the review", argv, got, ran)
			continue
		}
		if read := ran[0]; !slices.Equal(read.Argv, headRead) || !slices.Equal(read.Env, f.wantEnv("acme")) || read.Cwd != f.answer.cwd {
			t.Errorf("gh %q first ran %q in %s with %q; want %q with the routed owner's directory, the scrubbed environment and the answer's working directory",
				argv, read.Argv, read.Cwd, read.Env, headRead)
		}
		at := slices.IndexFunc(ran[1].Argv, func(arg string) bool { return strings.Contains(arg, footerMarker) })
		if at < 0 {
			t.Errorf("gh %q ran the review with %q, which carries no footer", argv, ran[1].Argv)
			continue
		}
		posted := intake.PullRequestReview{Body: strings.TrimPrefix(ran[1].Argv[at], "body=")}
		if head, session := posted.LegionHead(), posted.LegionSession(); head != roundHead || session != reviewer.Session {
			t.Errorf("the review intake reads the head %q and the session %q from the body gh was given, %q; want %s and %s",
				head, session, posted.Body, roundHead, reviewer.Session)
		}
		before := intake.PullRequestReview{Body: "Approved.\n\n" + footer(reviewer, question, seq, "")}
		if head, session := before.LegionHead(), before.LegionSession(); head != "" || session != reviewer.Session {
			t.Errorf("the review intake reads the head %q and the session %q from a footer naming no head, %q; want none and %s",
				head, session, before.Body, reviewer.Session)
		}
	}
}

// A review of a pull request a push has moved past the head the agent's recorded round read is
// refused before it runs: gh is run for the read of the head and nothing else, the leak guard
// never runs, and the model reads that the agent has not read the new head, both heads named, and
// that the round waits for the reviewer itself. The refusal is recorded and logged as every
// refusal is, and the head read logs one `answers: gh head read` line of its own, never the
// restart lookup's `answers: gh lookup`. The identical call, once the pull request stands at the
// recorded head, runs.
func TestAReviewPastTheRecordedHeadIsRefusedWithOnlyItsHeadRead(t *testing.T) {
	for _, argv := range [][]string{
		{"pr", "review", "5", "-R", "acme/widgets", "--approve"},
		{"api", "-X", "POST", "repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=APPROVE"},
	} {
		f := newFixture(t, reviewer, behaviour{Stdout: "reviewed\n", Responses: headAt(pushedHead)})
		got := f.run(1, argv...)
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") {
			t.Errorf("gh %q = %+v; want it refused, gh never run for it", argv, got)
		}
		for _, want := range []string{pushedHead, roundHead, "you have not read the new head", "The round waits for the reviewer itself"} {
			if !strings.Contains(got.Output, want) {
				t.Errorf("the refusal of gh %q, %q, does not say %q", argv, got.Output, want)
			}
		}
		if ran := f.invocations(false); len(ran) != 1 || !slices.Equal(ran[0].Argv, headRead) {
			t.Errorf("gh %q ran gh for %+v; want the read of the head alone", argv, ran)
		}
		if reads, checked := f.guardReads(), f.checked(); len(reads) != 0 || checked != "" {
			t.Errorf("the leak guard ran for gh %q (visibility reads %+v, check %q); a review refused before it runs reaches no guard", argv, reads, checked)
		}
		row, recorded := f.calls.row(question, 1)
		if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.FinishedAt == nil {
			t.Errorf("the refused review is recorded as %+v (%t); want its refusal and no exit", row, recorded)
		}
		if lines := f.logLines("answers: gh"); len(lines) != 1 || lines[0]["refused"] != got.Output {
			t.Errorf("the refused review logged %v; want one `answers: gh` line carrying its refusal", lines)
		}
		if lines := f.logLines(headReadLine); len(lines) != 1 || fmt.Sprint(lines[0]["args"]) != fmt.Sprint(headRead) || lines[0]["owner"] != "acme" {
			t.Errorf("the head read logged %v; want one %q line naming its arguments and the owner acme", lines, headReadLine)
		}
		if lines := f.logLines(lookupLine); len(lines) != 0 {
			t.Errorf("the head read logged %v; a %q line is a restart's lookup alone", lines, lookupLine)
		}

		setBehaviour(t, f.stand, behaviour{Stdout: "reviewed\n", Responses: headAt(roundHead)})
		f.clearInvocations()
		again := f.run(2, argv...)
		if reads, ran := f.headReads(); !again.Ran || again.Output != "reviewed\n" || len(reads) != 1 || len(ran) != 1 {
			t.Errorf("gh %q at the recorded head = %+v, read the head %d times and ran %+v; want the review run once", argv, again, len(reads), ran)
		}
	}
}

// A review from an agent whose record names no head its round read is refused before anything
// runs, gh never started, not for its help nor for the pull request's head: a review naming no
// head decides nothing, so the model reads that the record names none and that the round waits
// for the reviewer itself. The refusal is recorded as every refusal is.
func TestAReviewWithNoRecordedHeadIsRefusedBeforeAnythingRuns(t *testing.T) {
	unread := reviewer
	unread.Head = ""
	f := newFixture(t, unread, behaviour{Stdout: "reviewed\n", Responses: headAt(roundHead)})
	for i, argv := range [][]string{
		{"pr", "review", "5", "-R", "acme/widgets", "--approve", "--body", "Approved."},
		{"pr", "review", "5", "-R", "acme/widgets", "-c"},
		{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=REQUEST_CHANGES", "-f", "body=Changes."},
		{"api", "-X", "PUT", "repos/acme/widgets/pulls/5/reviews/9", "-f", "body=Approved, with the nit fixed."},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, argv...)
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: your record names no head your round read") ||
			!strings.Contains(got.Output, "The round waits for the reviewer itself") {
			t.Errorf("gh %q = %+v; want it refused for the head the record does not name", argv, got)
		}
		if ran := f.invocations(true); len(ran) != 0 {
			t.Errorf("gh %q started gh: %+v", argv, ran)
		}
		row, recorded := f.calls.row(question, seq)
		if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil {
			t.Errorf("the refused review is recorded as %+v (%t); want its refusal and no exit", row, recorded)
		}
	}
}

// A gh api review whose commit_id names another commit than the head the agent's recorded round
// read is refused before anything runs, both commits named, with the pull request standing at
// the recorded head all the same: GitHub would hold the review to the commit it names. The same
// review naming the recorded head runs.
func TestAnAPIReviewNamingAnotherCommitIsRefusedBeforeAnythingRuns(t *testing.T) {
	f := newFixture(t, reviewer, behaviour{Stdout: "reviewed\n", Responses: headAt(roundHead)})
	review := func(commit string) []string {
		return []string{"api", "-X", "POST", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "commit_id=" + commit}
	}
	got := f.run(1, review(pushedHead)...)
	if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, pushedHead) || !strings.Contains(got.Output, roundHead) {
		t.Errorf("a review naming commit_id %s = %+v; want it refused, both commits named", pushedHead, got)
	}
	if ran := f.invocations(true); len(ran) != 0 {
		t.Errorf("a review naming commit_id %s started gh: %+v", pushedHead, ran)
	}
	f.clearInvocations()
	again := f.run(2, review(roundHead)...)
	if reads, ran := f.headReads(); !again.Ran || len(reads) != 1 || len(ran) != 1 {
		t.Errorf("a review naming commit_id %s = %+v, read the head %d times and ran %+v; want it run once", roundHead, again, len(reads), ran)
	}
}

// A review whose arguments do not name its pull request by repository and number is refused
// before anything runs, naming what it lacks: the tool reads the head of the pull request a
// review is on before it submits it, and from the answer's empty working directory gh resolves
// neither a repository nor a branch's pull request.
func TestAReviewNamingNoPullRequestIsRefusedBeforeAnythingRuns(t *testing.T) {
	f := newFixture(t, reviewer, behaviour{Stdout: "reviewed\n", Responses: headAt(roundHead)})
	for i, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"pr", "review", "5", "--approve"}, "it names no repository"},
		{[]string{"pr", "review", "-R", "acme/widgets", "--approve"}, "it names no pull request"},
		{[]string{"pr", "review", "feature-x", "-R", "acme/widgets", "--approve"}, `its selector "feature-x" is no pull request number`},
		{[]string{"api", "repos/{owner}/{repo}/pulls/5/reviews", "-f", "event=APPROVE"}, "its endpoint's repository"},
		{[]string{"api", "repos/acme/widgets/pulls/latest/reviews", "-f", "event=APPROVE"}, `its endpoint's pull request "latest" is no number`},
		{[]string{"api", "-X", "PUT", "repos/{owner}/{repo}/pulls/5/reviews/9", "-f", "body=Edited."}, "its endpoint's repository"},
	} {
		f.clearInvocations()
		got := f.run(i+1, tc.argv...)
		if got.Ran || !strings.HasPrefix(got.Output, "refused: "+reviewNamesNoPullRequest) || !strings.Contains(got.Output, tc.want) {
			t.Errorf("gh %q = %+v; want a refusal saying %q", tc.argv, got, tc.want)
		}
		if ran := f.invocations(true); len(ran) != 0 {
			t.Errorf("gh %q started gh: %+v", tc.argv, ran)
		}
	}
}

// A gh api PUT replacing a submitted review's body is a review too: the review intake reads the
// last Legion footer of the body the review holds now, so a new body whose footer names no head
// would strip the head the review's earlier footer named. So the PUT is refused while the pull
// request stands past the recorded head, gh run for the head read alone, and at the recorded head
// it runs with a footer naming that head. A review's dismissal is no review: it runs as it did,
// with no head read, at whichever head the pull request stands.
func TestAnAPIPutReplacingAReviewsBodyIsAReview(t *testing.T) {
	f := newFixture(t, reviewer, behaviour{Stdout: "edited\n", Responses: headAt(pushedHead)})
	argv := []string{"api", "-X", "PUT", "repos/acme/widgets/pulls/5/reviews/9", "-f", "body=Approved, with the nit fixed."}
	got := f.run(1, argv...)
	if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, pushedHead) ||
		!strings.Contains(got.Output, roundHead) || !strings.Contains(got.Output, "you have not read the new head") {
		t.Errorf("gh %q at a moved head = %+v; want it refused, both heads named", argv, got)
	}
	if ran := f.invocations(false); len(ran) != 1 || !slices.Equal(ran[0].Argv, headRead) {
		t.Errorf("gh %q at a moved head ran gh for %+v; want the read of the head alone", argv, ran)
	}

	setBehaviour(t, f.stand, behaviour{Stdout: "edited\n", Responses: headAt(roundHead)})
	f.clearInvocations()
	again := f.run(2, argv...)
	ran := f.invocations(false)
	if !again.Ran || again.Output != "edited\n" || len(ran) != 2 || !slices.Equal(ran[0].Argv, headRead) {
		t.Fatalf("gh %q at the recorded head = %+v, ran %+v; want the head read, then the PUT", argv, again, ran)
	}
	if want := append(slices.Clone(argv[:len(argv)-1]), argv[len(argv)-1]+"\n\n"+footer(reviewer, question, 2, roundHead)); !slices.Equal(ran[1].Argv, want) {
		t.Errorf("gh %q ran as %q, want %q", argv, ran[1].Argv, want)
	}
	posted := intake.PullRequestReview{Body: strings.TrimPrefix(ran[1].Argv[len(ran[1].Argv)-1], "body=")}
	if head, session := posted.LegionHead(), posted.LegionSession(); head != roundHead || session != reviewer.Session {
		t.Errorf("the review intake reads the head %q and the session %q from the replaced body %q; want %s and %s",
			head, session, posted.Body, roundHead, reviewer.Session)
	}

	setBehaviour(t, f.stand, behaviour{Stdout: "dismissed\n", Responses: headAt(pushedHead)})
	f.clearInvocations()
	dismiss := []string{"api", "-X", "PUT", "repos/acme/widgets/pulls/5/reviews/9/dismissals", "-f", "message=Superseded by the next round."}
	if got := f.run(3, dismiss...); !got.Ran || got.Output != "dismissed\n" {
		t.Errorf("gh %q = %+v; want it run", dismiss, got)
	}
	if reads, ran := f.headReads(); len(reads) != 0 || len(ran) != 1 || !slices.Equal(ran[0].Argv, dismiss) {
		t.Errorf("gh %q read the head %d times and ran %+v; want it run once as given, no head read", dismiss, len(reads), ran)
	}
}

// A comment that is no review runs as it did: gh is run for it alone, with no read of the pull
// request's head, at whichever head the pull request stands, and its footer names no head,
// whether or not the agent's record names one.
func TestACommentRunsWithNoHeadRead(t *testing.T) {
	unread := reviewer
	unread.Head = ""
	for _, agent := range []Agent{reviewer, unread} {
		f := newFixture(t, agent, behaviour{Stdout: "posted\n", Responses: headAt(pushedHead)})
		argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
		got := f.run(1, argv...)
		want := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done.\n\n" + footer(agent, question, 1, "")}
		reads, ran := f.headReads()
		if !got.Ran || got.Output != "posted\n" || len(reads) != 0 || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q as an agent with the head %q = %+v, read the head %d times and ran %+v; want it run once as %q, no head read",
				argv, agent.Head, got, len(reads), ran, want)
		}
		if head := (intake.PullRequestReview{Body: want[len(want)-1]}).LegionHead(); head != "" {
			t.Errorf("a comment's footer names the head %q, want none", head)
		}
	}
}

// No value an allowed command is given is read as an option: a body, a title, a merge subject,
// a lock reason, a search query and a query word after `--` all begin with a hyphen where the
// text does, and the tool reads each command's options as pflag does (optionSpecs) rather than
// every argument. An option of its own — a browser, a file read on the daemon's host, a typed
// field reading one — is still refused, and so is a `-w` of a command whose own `-w` opens a
// browser, while `gh run list -w <workflow>`, where gh's own `-w` is --workflow, runs. gh's
// saved help holds no `pr edit`, `pr merge` or `pr lock`, which the routing would refuse for
// that alone, so the forms are read here rather than run.
func TestNoValueOfAnAllowedCommandIsReadAsAnOption(t *testing.T) {
	for _, argv := range [][]string{
		{"pr", "create", "-R", "acme/widgets", "--title", "-Fix the leak", "--body", "-worker.Stop() is gone"},
		{"pr", "create", "-R", "acme/widgets", "--fill", "--body", "-Fixed in abc123"},
		{"pr", "create", "-R", "acme/widgets", "-t", "-Fix", "-b", "-worker.Stop() is gone"},
		{"pr", "edit", "5", "-R", "acme/widgets", "--body", "-worker.Stop() is gone"},
		{"pr", "edit", "5", "-R", "acme/widgets", "--title", "-Fix the leak"},
		{"pr", "edit", "5", "-R", "acme/widgets", "--add-label", "-weird-label"},
		{"pr", "merge", "5", "-R", "acme/widgets", "--squash", "--body", "-worker.Stop() is gone"},
		{"pr", "merge", "5", "-R", "acme/widgets", "--squash", "-t", "-Fix the leak"},
		{"pr", "merge", "5", "-R", "acme/widgets", "--merge", "-A", "-weird@example.com"},
		{"pr", "lock", "5", "-R", "acme/widgets", "--reason", "-off_topic"},
		{"pr", "revert", "5", "-R", "acme/widgets", "--title", "-Fix", "--body", "-worker.Stop() is gone"},
		{"pr", "list", "-R", "acme/widgets", "--search", "-wip in:title"},
		{"pr", "ls", "-R", "acme/widgets", "--search", "-wip in:title"},
		{"issue", "list", "-R", "acme/widgets", "--search", "-wip in:title"},
		{"label", "list", "-R", "acme/widgets", "--search", "-wip"},
		{"search", "issues", "--", "-wip", "repo:acme/widgets"},
		{"pr", "view", "5", "-R", "acme/widgets", "--jq", "-1 + .number"},
		{"run", "list", "-R", "acme/widgets", "-w", "ci.yml"},
	} {
		if why := refusal(argv, commandWords(argv), ""); why != "" {
			t.Errorf("gh %q is refused: %s; want it run, since the hyphen begins a value and no option", argv, why)
		}
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"pr", "create", "-R", "acme/widgets", "--web"}, "opens a web browser"},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill", "-w"}, "opens a web browser"},
		{[]string{"pr", "create", "-R", "acme/widgets", "--body-file", "body.md"}, "pass the text itself with --body"},
		{[]string{"pr", "create", "-R", "acme/widgets", "-F", "body.md"}, "pass the text itself with --body"},
		{[]string{"pr", "create", "-R", "acme/widgets", "--recover", "state.json"}, "reads a file on the daemon's host"},
		{[]string{"pr", "edit", "5", "-R", "acme/widgets", "--body-file", "body.md"}, "pass the text itself with --body"},
		{[]string{"pr", "edit", "5", "-R", "acme/widgets", "-Fbody.md"}, "pass the text itself with --body"},
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--squash", "-F", "body.md"}, "pass the text itself with --body"},
		{[]string{"pr", "revert", "5", "-R", "acme/widgets", "--body-file", "body.md"}, "pass the text itself with --body"},
		{[]string{"pr", "list", "-R", "acme/widgets", "--web"}, "opens a web browser"},
		{[]string{"search", "issues", "-w", "repo:acme/widgets"}, "opens a web browser"},
		{[]string{"workflow", "run", "ci.yml", "-R", "acme/widgets", "-F", "token=@/etc/passwd"}, "reads a field's value from a file"},
		// An option no spec holds is read as every argument is: a later gh's new option never
		// lets a browser through.
		{[]string{"pr", "create", "-R", "acme/widgets", "--brand-new", "x", "-w"}, "opens a web browser"},
	} {
		if why := refusal(tc.argv, commandWords(tc.argv), ""); !strings.Contains(why, tc.want) {
			t.Errorf("gh %q is refused with %q; want a refusal containing %q", tc.argv, why, tc.want)
		}
	}
}

// ghReferenceCommand is one command of gh's saved reference
// (cmd/legion/testdata/gh-help/reference.txt): the options it names, by long name, and the
// cobra aliases gh runs it under.
type ghReferenceCommand struct {
	flags   map[string]flagSpec
	aliases []string
}

// ghReferenceHeading opens a command of the reference, `### gh pr list [flags]`, and
// ghReferenceFlag is one of its option lines, `  -b, --body text   The comment body text`: the
// shorthand, the long name, and a placeholder one space after the name where the option takes a
// value, the description two or more spaces after it.
var (
	ghReferenceHeading = regexp.MustCompile(`^#+ gh ([a-z0-9][a-z0-9 -]*?)(?: [\[{<].*)?$`)
	ghReferenceFlag    = regexp.MustCompile(`^ {2,}(?:-([A-Za-z]), )?--([a-z0-9-]+)(?: ([^\s]+))?(?: {2,}|\s*$)`)
)

// ghReferenceCommands reads every command of gh 2.98.0's saved reference.
func ghReferenceCommands(t *testing.T) map[string]ghReferenceCommand {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(helpDir(t), "reference.txt"))
	if err != nil {
		t.Fatal(err)
	}
	commands, name, inAliases := map[string]ghReferenceCommand{}, "", false
	for _, line := range strings.Split(string(data), "\n") {
		switch heading := ghReferenceHeading.FindStringSubmatch(line); {
		case heading != nil:
			name, inAliases = strings.TrimSpace(heading[1]), false
			commands[name] = ghReferenceCommand{flags: map[string]flagSpec{}}
		case name == "":
		case strings.TrimSpace(line) == "Aliases":
			inAliases = true
		case inAliases:
			if alias, ok := strings.CutPrefix(line, "gh "); ok {
				command := commands[name]
				command.aliases = append(command.aliases, strings.TrimSpace(alias))
				commands[name] = command
			}
		default:
			if flag := ghReferenceFlag.FindStringSubmatch(line); flag != nil {
				spec := flagSpec{value: flag[3] != ""}
				if flag[1] != "" {
					spec.short = flag[1][0]
				}
				commands[name].flags[flag[2]] = spec
			}
		}
	}
	return commands
}

// The options the tool reads each allowed command's arguments against are gh's own: every
// option of gh 2.98.0's reference, with its shorthand and its arity, and nothing besides the
// inherited --help and -R/--repo the reference leaves out; every cobra alias gh lists is read
// as the command it runs; and every command the allow list holds has them, so no allowed call
// falls back to reading its values as options. A gh that adds, drops or re-spells an option
// fails here when its saved help is updated, rather than silently refusing a body or letting an
// option through.
func TestEveryAllowedCommandsOptionsAreGhsOwn(t *testing.T) {
	reference := ghReferenceCommands(t)
	inherited := commandSpec{"help": helpFlag, "repo": repoFlag}
	specs := maps.Clone(optionSpecs)
	specs["api"] = apiSpec
	for name, spec := range specs {
		command, ok := reference[name]
		if !ok {
			t.Errorf("the gh tool holds the options of `gh %s`, which gh's reference does not name", name)
			continue
		}
		for option, flag := range command.flags {
			if read, ok := spec[option]; !ok || read != flag {
				t.Errorf("the gh tool reads `gh %s`'s --%s as %+v (held: %t); gh's own help names it %+v", name, option, read, ok, flag)
			}
		}
		for option, flag := range spec {
			if _, ok := command.flags[option]; ok {
				continue
			}
			if want, ok := inherited[option]; !ok || want != flag {
				t.Errorf("the gh tool reads an option --%s (%+v) of `gh %s` gh's own help does not name", option, flag, name)
			}
		}
		for _, alias := range command.aliases {
			if ghAliases[alias] != name && specs[alias] == nil {
				t.Errorf("gh runs `gh %s` as `gh %s`, and the gh tool reads it as %q", alias, name, ghAliases[alias])
			}
		}
	}
	for name := range reference {
		words := strings.Fields(name)
		if len(words) != 2 || allowed(words, "") != "" {
			continue
		}
		if spec, _ := specOf(words); spec == nil {
			t.Errorf("`gh %s` is one of the gh tool's commands and the tool holds none of its options, so it reads every argument of such a call as an option", name)
		}
	}
	for alias, name := range ghAliases {
		if spec, key := specOf(strings.Fields(alias)); key != name || spec == nil {
			t.Errorf("`gh %s` is read as `gh %s` (%t), want the options of `gh %s`", alias, key, spec != nil, name)
		}
	}
}

// gh's stdout and stderr reach the model interleaved and cut at gh_output_chars characters, a
// multi-byte character counting once, with a note of how many were cut; a gh running past
// gh_seconds is stopped, whatever it printed so far kept with a note; and a gh something else
// killed keeps what it printed, with the signal named.
func TestOutputIsCutAndASlowGhIsStopped(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: strings.Repeat("é", 30), Stderr: "warning", Exit: 1})
	got := Run(context.Background(), Call{Answer: f.answer, Seq: 1, N: 1, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 10, GHCalls: 5}})
	if want := strings.Repeat("é", 10) + "\n[cut: 27 more characters]"; got.Output != want || got.Exit != 1 || !got.Ran {
		t.Errorf("a long answer = %+v; want %q, exit 1", got, want)
	}
	whole := Run(context.Background(), Call{Answer: f.answer, Seq: 2, N: 2, Args: args("api", "user"), Bounds: bounds})
	if want := strings.Repeat("é", 30) + "warning"; whole.Output != want {
		t.Errorf("stdout and stderr read %q, want %q", whole.Output, want)
	}

	setBehaviour(t, f.stand, behaviour{Stdout: "partial", Sleep: time.Minute})
	start := time.Now()
	got = Run(context.Background(), Call{Answer: f.answer, Seq: 3, N: 3, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 2 * time.Second, GHOutputChars: 100, GHCalls: 5}})
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a gh sleeping a minute under a 2s bound returned after %s", elapsed)
	}
	if !got.Ran || !strings.Contains(got.Output, "[stopped: gh ran past its 2s bound (answers.bounds.gh_seconds)]") {
		t.Errorf("a slow gh = %+v; want it stopped with the note", got)
	}
	if row, _ := f.calls.row(question, 3); row.Result == nil || row.Exit == nil {
		t.Errorf("the stopped call is recorded as %+v; want it finished", row)
	}

	// A gh a signal ended from outside the tool ran: what it printed stands, with the signal's
	// words, and its exit is -1, since a signal leaves no status (procgroup.Ended).
	setBehaviour(t, f.stand, behaviour{Stdout: "partial", Killed: true})
	got = Run(context.Background(), Call{Answer: f.answer, Seq: 4, N: 4, Args: args("api", "user"), Bounds: bounds})
	if want := "partial\n[gh was ended by signal: killed]"; !got.Ran || got.Exit != -1 || got.Output != want {
		t.Errorf("a gh a signal ended = %+v; want it run, exit -1 and %q", got, want)
	}
	if row, _ := f.calls.row(question, 4); row.Result == nil || row.Exit == nil {
		t.Errorf("the killed call is recorded as %+v; want it finished", row)
	}
}

// An answer makes at most gh_calls gh calls, counted apart from its calls of every other tool:
// the call's number among the answer's gh calls is what the bound reads, not its seq among every
// call of the answer. The gh call past the bound is refused without gh starting.
func TestTheCallPastTheBoundIsRefused(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "ok"})
	capped := config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 100, GHCalls: 2}
	for _, c := range []struct{ seq, n int }{{3, 1}, {6, 2}, {7, 3}} {
		f.clearInvocations()
		got := Run(context.Background(), Call{Answer: f.answer, Seq: c.seq, N: c.n, Args: args("api", "user"), Bounds: capped})
		switch {
		case c.n <= 2 && !got.Ran:
			t.Errorf("gh call %d (seq %d) of 2 = %+v; want it run", c.n, c.seq, got)
		case c.n == 3 && (got.Ran || !strings.Contains(got.Output, "this answer has made its 2 gh calls")):
			t.Errorf("gh call 3 of 2 = %+v; want it refused", got)
		case c.n == 3 && len(f.invocations(true)) != 0:
			t.Errorf("gh call 3 of 2 started gh: %+v", f.invocations(true))
		}
	}
}

// Each call's `answers: gh` line says whether it wrote (write), so its reader need not judge the
// arguments: true for a call gh ran that writes as Writes and CallWrites read one (writeCommand),
// false for a read, a write command's help, which posts nothing, and a write refused before gh
// started, here the call past the gh_calls bound.
func TestEachCallsLineSaysWhetherItWrote(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer\n"})
	capped := config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 100, GHCalls: 3}
	for i, c := range []struct {
		argv       []string
		ran, write bool
	}{
		{[]string{"pr", "view", "5", "-R", "acme/widgets"}, true, false},
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--squash"}, true, true},
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--help"}, true, false},
		{[]string{"pr", "merge", "5", "-R", "acme/widgets", "--squash"}, false, false},
	} {
		seq := i + 1
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, N: seq, Args: args(c.argv...), Bounds: capped})
		if got.Ran != c.ran {
			t.Errorf("gh %q (call %d of 3) = %+v; want ran %t", c.argv, seq, got, c.ran)
			continue
		}
		var line map[string]any
		for _, l := range f.logLines("answers: gh") {
			if l["seq"] == float64(seq) {
				line = l
			}
		}
		if write, ok := line["write"].(bool); !ok || write != c.write || line["ran"] != c.ran {
			t.Errorf("gh %q (call %d of 3) logged %v; want ran %t, write %t", c.argv, seq, line, c.ran, c.write)
		}
	}
}

// GH_PATH names a wrapper running this binary's `legion gh`, so the helper gh names for its git
// (`<GH_PATH> auth git-credential`) is `legion gh auth git-credential`, told the real gh and the
// answer's bundle as a pane's environment tells them.
func TestGHPathRunsLegionGhOverTheAnswersBundle(t *testing.T) {
	f := newFixture(t, implementer, behaviour{})
	got := filepath.Join(t.TempDir(), "legion-ran")
	if err := os.WriteFile(f.host.Legion, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" \"$LEGION_GH_PATH\" \"$LEGION_GH_OWNERS_DIR\" \"$GH_CONFIG_DIR\" > '"+got+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(f.answer.wrapper, "auth", "git-credential", "get")
	cmd.Env = f.answer.env(filepath.Join(f.answer.owners, "acme"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the GH_PATH wrapper: %v: %s", err, out)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := "gh auth git-credential get\n" + f.host.GH + "\n" + f.answer.owners + "\n" + filepath.Join(f.answer.owners, "acme") + "\n"; string(data) != want {
		t.Errorf("the wrapper ran legion with %q, want %q", data, want)
	}
}

// A restart finds the comment or review a call with no recorded result posted, by its footer's
// answer key, on the pull request or issue the call wrote to, reading GitHub through the stand-in
// with the routed owner's directory; a call whose key is on none is not found. A finished call, an
// unrecorded one and one posting nothing are not looked up.
func TestLookupFindsTheCommentByItsAnswerKey(t *testing.T) {
	f := newFixture(t, implementer, behaviour{})
	foot := func(seq int) string { return footer(implementer, question, seq, "") }
	comments := fmt.Sprintf(`[[{"id":11,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-11","body":%q},{"id":12,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-12","body":%q}],[{"id":13,"html_url":"x","body":"no footer"}]]`,
		"Earlier.\n\n"+foot(1), "Done.\n\n"+foot(2))
	reviews := fmt.Sprintf(`[[{"id":21,"html_url":"https://github.com/acme/widgets/pull/5#pullrequestreview-21","body":%q}]]`, "Earlier review.\n\n"+foot(1))
	setBehaviour(t, f.stand, behaviour{Responses: map[string]string{
		"api repos/acme/widgets/issues/5/comments --paginate --slurp":  comments,
		"api repos/acme/widgets/pulls/5/reviews --paginate --slurp":    reviews,
		"pr view feature-x -R acme/widgets --json number --jq .number": "5\n",
	}})
	start := func(seq int, argv ...string) {
		if err := f.calls.StartAnswerToolCall(context.Background(), store.AnswerToolCall{Question: question, Seq: seq, Tool: store.ToolGH, Args: argv, App: "implement", Owner: "acme", StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	start(2, "pr", "comment", "5", "-R", "acme/widgets", "--body", "Done.\n\n"+foot(2))
	start(3, "pr", "review", "5", "-R", "acme/widgets", "--approve", "--body", foot(3))
	start(4, "pr", "comment", "feature-x", "-R", "acme/widgets", "--body", "Done.\n\n"+foot(4))
	start(5, "api", "repos/acme/widgets/issues/5/comments", "-f", "body=Done.\n\n"+foot(2))
	start(6, "pr", "view", "5", "-R", "acme/widgets")
	start(7, "pr", "comment", "https://github.com/acme/widgets/pull/5", "--body", "Done.\n\n"+foot(7))

	for _, tc := range []struct {
		seq  int
		want Found
	}{
		{2, Found{Exists: true, URL: "https://github.com/acme/widgets/pull/5#issuecomment-12", ID: 12}},
		{3, Found{}},
		{4, Found{}},
		{5, Found{}},
		{7, Found{}},
	} {
		f.clearInvocations()
		got, err := Lookup(context.Background(), f.answer, tc.seq, bounds)
		if err != nil || got != tc.want {
			t.Errorf("Lookup of call %d = %+v, %v; want %+v", tc.seq, got, err, tc.want)
		}
		for _, inv := range f.invocations(false) {
			if want := f.wantEnv("acme"); !slices.Equal(inv.Env, want) {
				t.Errorf("Lookup of call %d ran gh %q with %q, want %q", tc.seq, inv.Argv, inv.Env, want)
			}
		}
	}
	if _, err := Lookup(context.Background(), f.answer, 6, bounds); !errors.Is(err, ErrNotAWrite) {
		t.Errorf("Lookup of a pr view = %v, want ErrNotAWrite", err)
	}
	if _, err := Lookup(context.Background(), f.answer, 9, bounds); err == nil {
		t.Errorf("Lookup of an unrecorded call succeeded, want an error")
	}
	exit := 0
	if err := f.calls.FinishAnswerToolCall(context.Background(), question, 2, "posted", &exit, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(context.Background(), f.answer, 2, bounds); err == nil || !strings.Contains(err.Error(), "has its result recorded") {
		t.Errorf("Lookup of a finished call = %v, want a refusal", err)
	}
	if row, _ := f.calls.row(question, 3); row.Result != nil {
		t.Errorf("Lookup wrote to the call record: %+v", row)
	}
}

// A restart's lookup takes gh's answer up to gh_output_chars characters for each of gh_calls
// (readLimit), a multi-byte character counting once: an answer of exactly that many is read and
// the comment found, also where one call's gh_output_chars alone is short of it, and one
// character more is refused with an error naming the bound rather than read whole.
func TestLookupRefusesAnAnswerPastItsBound(t *testing.T) {
	f := newFixture(t, implementer, behaviour{})
	body := "Done — é.\n\n" + footer(implementer, question, 2, "")
	comments := fmt.Sprintf(`[[{"id":12,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-12","body":%q}]]`, body)
	setBehaviour(t, f.stand, behaviour{Responses: map[string]string{"api repos/acme/widgets/issues/5/comments --paginate --slurp": comments}})
	if err := f.calls.StartAnswerToolCall(context.Background(), store.AnswerToolCall{Question: question, Seq: 2, Tool: store.ToolGH, Args: []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", body}, App: "implement", Owner: "acme", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	chars := utf8.RuneCountInString(comments)
	found := Found{Exists: true, URL: "https://github.com/acme/widgets/pull/5#issuecomment-12", ID: 12}
	for _, at := range []config.AnswerBounds{
		{GHCall: 20 * time.Second, GHOutputChars: chars, GHCalls: 1},
		{GHCall: 20 * time.Second, GHOutputChars: (chars + 1) / 2, GHCalls: 2},
	} {
		if got, err := Lookup(context.Background(), f.answer, 2, at); err != nil || got != found {
			t.Errorf("Lookup under %d characters for each of %d calls of an answer of %d = %+v, %v; want %+v", at.GHOutputChars, at.GHCalls, chars, got, err, found)
		}
	}
	past := config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: chars - 1, GHCalls: 1}
	got, err := Lookup(context.Background(), f.answer, 2, past)
	want := fmt.Sprintf("printed %d characters, past the %d a read of the daemon's own takes (answers.bounds.gh_output_chars times answers.bounds.gh_calls)", chars, chars-1)
	if err == nil || !strings.Contains(err.Error(), want) || got != (Found{}) {
		t.Errorf("Lookup under %d characters of an answer of %d = %+v, %v; want an error containing %q", chars-1, chars, got, err, want)
	}
}

// With the daemon's Postgres store, a call's row is written before gh runs, with no result, and
// finished after with gh's output and exit; a refused call's row has its refusal and no exit.
func TestCallsAreRecordedInPostgres(t *testing.T) {
	ctx := context.Background()
	s := testpg.CloneStore(t, "legion_ghtool")
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer"})
	f.answer.host.Calls = checkedStore{Store: s, t: t, f: f}
	got := f.run(1, "pr", "view", "5", "-R", "sjawhar/legion")
	if !got.Ran {
		t.Fatalf("the call = %+v, want it run", got)
	}
	row, ok, err := s.AnswerToolCall(ctx, question, 1)
	if err != nil || !ok || row.Result == nil || *row.Result != "gh's answer" || row.Exit == nil || *row.Exit != 0 || row.FinishedAt == nil || row.Owner != "sjawhar" || row.App != "implement" {
		t.Errorf("the call's row = %+v, %t, %v; want it finished with gh's answer, exit 0, App implement, owner sjawhar", row, ok, err)
	}
	f.clearInvocations()
	f.run(2, "auth", "token")
	if row, ok, err := s.AnswerToolCall(ctx, question, 2); err != nil || !ok || row.Result == nil || !strings.Contains(*row.Result, `"auth" is refused`) || row.Exit != nil {
		t.Errorf("the refused call's row = %+v, %t, %v; want its refusal and no exit", row, ok, err)
	}
}

// checkedStore is the Postgres store, checking at each start that the row holds no result and gh
// has not run, and at each finish that gh has run.
type checkedStore struct {
	*store.Store
	t *testing.T
	f *fixture
}

func (c checkedStore) StartAnswerToolCall(ctx context.Context, call store.AnswerToolCall) error {
	if err := c.Store.StartAnswerToolCall(ctx, call); err != nil {
		return err
	}
	row, ok, err := c.Store.AnswerToolCall(ctx, call.Question, call.Seq)
	if err != nil || !ok || row.Result != nil || len(c.f.invocations(false)) != 0 {
		c.t.Errorf("at the start of call %d: row %+v, %t, %v and gh run %d times; want the row with no result and gh not run", call.Seq, row, ok, err, len(c.f.invocations(false)))
	}
	return nil
}

func (c checkedStore) FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error {
	if exit != nil && len(c.f.invocations(false)) != 1 {
		c.t.Errorf("at the finish of call %d gh had run %d times, want once", seq, len(c.f.invocations(false)))
	}
	return c.Store.FinishAnswerToolCall(ctx, question, seq, result, exit, at)
}

// Every call passes the shared leak guard before gh is handed anything, as every Legion write to
// a public repository does (cybermonklabs/legion#3425): a call the shared check refuses never
// starts gh, the model reads the check's own report as the call's result, the call is recorded
// with that refusal and no exit, and one line of the package's own says which issue, which role's
// name the write would have carried, which repository and why.
func TestACallTheLeakGuardRefusesNeverStartsGh(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	f.leakCheck(checkRefuses)
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	got := f.run(1, argv...)
	if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, checkRefusal) {
		t.Fatalf("gh %q = %+v; want a refusal carrying the check's report %q, gh never run", argv, got, checkRefusal)
	}
	if ran := f.invocations(false); len(ran) != 0 {
		t.Errorf("gh ran for a call the guard refused: %+v", ran)
	}
	if f.checked() == "" {
		t.Errorf("the shared check was never run for %q", argv)
	}
	row, recorded := f.calls.row(question, 1)
	if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.FinishedAt == nil {
		t.Errorf("the refused call is recorded as %+v (%t); want the guard's refusal and no exit", row, recorded)
	}
	lines := f.logLines("answers: gh call refused by the leak guard")
	if len(lines) != 1 {
		t.Fatalf("the guard's refusal logged %d lines, want one: %s", len(lines), f.logs)
	}
	line := lines[0]
	if line["issue"] != "LEG-1" || line["role"] != "implementer" || line["repository"] != "acme/widgets" ||
		!strings.Contains(fmt.Sprint(line["why"]), checkRefusal) || line["question"] != question || line["seq"] != float64(1) {
		t.Errorf("the refusal line = %v; want the issue LEG-1, the role implementer, acme/widgets and the guard's reason", line)
	}
	if ordinary := f.logLines("answers: gh"); len(ordinary) != 1 {
		t.Errorf("the refused call logged %d `answers: gh` lines, want the one every call logs: %s", len(ordinary), f.logs)
	}
}

// A check that could not be made to run refuses the call exactly as a refusal does — a check that
// did not run has cleared nothing — in the shared check's own setup-error words: one that exits a
// status it does not document, and one that is not there at all.
func TestACheckThatCannotBeRunRefusesTheCall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(f *fixture)
		want  string
	}{
		{
			name:  "the check could not finish",
			setUp: func(f *fixture) { f.leakCheck(checkCannotRun) },
			want:  "LEAK CHECK SETUP ERROR: the gh call",
		},
		{
			name:  "no check is there to run",
			setUp: func(f *fixture) { f.answer.host.LeakCheck = installedAt(filepath.Join(f.check, "moved-away.sh")) },
			want:  "LEAK CHECK SETUP ERROR: the gh call: the shared public leak check could not be found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
			tc.setUp(f)
			argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
			got := f.run(1, argv...)
			if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, tc.want) {
				t.Fatalf("gh %q = %+v; want a refusal containing %q, gh never run", argv, got, tc.want)
			}
			if ran := f.invocations(false); len(ran) != 0 {
				t.Errorf("gh ran for a call no check cleared: %+v", ran)
			}
			row, recorded := f.calls.row(question, 1)
			if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil {
				t.Errorf("the refused call is recorded as %+v (%t); want the guard's refusal and no exit", row, recorded)
			}
			lines := f.logLines("answers: gh call refused by the leak guard")
			if len(lines) != 1 || lines[0]["repository"] != "acme/widgets" || !strings.Contains(fmt.Sprint(lines[0]["why"]), tc.want) {
				t.Errorf("the refusal logged %v; want one line naming acme/widgets and %q", lines, tc.want)
			}
		})
	}
}

// A call whose repository GitHub answers the guard's visibility read of with 404 is refused as
// `legion gh` refuses it: the guard read no visibility, so it has cleared nothing
// (leakguard.NotFound). gh never starts the call, the model reads the guard's own words, which
// name GitHub's 404 and hold neither a refusal's marker nor a setup error's, the call is recorded
// with that refusal and no exit, and the guard's one line names the issue, the role, the
// repository and why, as for each of the guard's refusals. The check ran, and the guard made one
// read of the repository, which the stand-in gh answers as gh printed GitHub's 404
// (leakguardtest.GhReadNotFound).
func TestACallWhoseRepositoryGitHubAnswers404ForIsRefused(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n", VisibilityRefused: leakguardtest.GhReadNotFound})
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	got := f.run(1, argv...)
	why := (&leakguard.NotFound{What: leakguard.WhatGhCall, Repo: "acme/widgets"}).Error()
	if got.Ran || got.Exit != -1 || got.Output != "refused: "+why {
		t.Fatalf("gh %q = %+v; want the refusal %q, gh never run", argv, got, "refused: "+why)
	}
	for _, marker := range []string{"LEAK CHECK: refusing", "LEAK CHECK SETUP ERROR"} {
		if strings.Contains(got.Output, marker) {
			t.Errorf("the refusal %q holds %q; want GitHub's 404 told as such", got.Output, marker)
		}
	}
	if ran := f.invocations(false); len(ran) != 0 {
		t.Errorf("gh ran for a call whose repository GitHub answered 404 for: %+v", ran)
	}
	if f.checked() == "" {
		t.Errorf("the shared check was never run for %q", argv)
	}
	if reads := f.guardReads(); len(reads) != 1 || !slices.Contains(reads[0].Argv, "repos/acme/widgets") {
		t.Errorf("the guard's visibility reads were %+v; want one read of repos/acme/widgets", reads)
	}
	row, recorded := f.calls.row(question, 1)
	if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.FinishedAt == nil {
		t.Errorf("the refused call is recorded as %+v (%t); want the guard's refusal and no exit", row, recorded)
	}
	lines := f.logLines("answers: gh call refused by the leak guard")
	if len(lines) != 1 {
		t.Fatalf("the guard's refusal logged %d lines, want one: %s", len(lines), f.logs)
	}
	if line := lines[0]; line["issue"] != "LEG-1" || line["role"] != "implementer" || line["repository"] != "acme/widgets" || line["why"] != why {
		t.Errorf("the refusal line = %v; want the issue LEG-1, the role implementer, acme/widgets and %q", line, why)
	}
	if ordinary := f.logLines("answers: gh"); len(ordinary) != 1 {
		t.Errorf("the refused call logged %d `answers: gh` lines, want the one every call logs: %s", len(ordinary), f.logs)
	}
}

// A call the check passes runs exactly as it did before the guard: gh started once, with the
// arguments and the environment the tool gives it, and its record finished with gh's exit. The
// check read the whole call, the footer and all.
//
// The tool hands gh no standard input, so the guard is given none either; a check that asks for
// it anyway — the exit-4 handshake, which a check reading a body on standard input makes — is
// answered with an empty buffer rather than left waiting, and the call runs.
func TestACallTheLeakGuardClearsRunsAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check string
		read  string
	}{
		{name: "a check that reads the call alone", check: checkPasses},
		{name: "a check that asks for the call's standard input", check: checkReadsStdin, read: "checked stdin \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
			f.leakCheck(tc.check)
			argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
			got := f.run(1, argv...)
			if !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
				t.Fatalf("gh %q = %+v; want gh's answer, exit 0", argv, got)
			}
			ran := f.invocations(false)
			if len(ran) != 1 {
				t.Fatalf("gh %q ran gh %d times: %+v", argv, len(ran), ran)
			}
			want := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done.\n\n" + footer(implementer, question, 1, "")}
			if !slices.Equal(ran[0].Argv, want) {
				t.Errorf("gh ran with %q, want %q", ran[0].Argv, want)
			}
			if env := f.wantEnv("acme"); !slices.Equal(ran[0].Env, env) {
				t.Errorf("gh ran with the environment\n%q\nwant exactly\n%q", ran[0].Env, env)
			}
			if checked := f.checked(); !strings.Contains(checked, checkedCall(want)) {
				t.Errorf("the check was handed %q, want the whole call %q", checked, want)
			}
			if tc.read != "" && !strings.Contains(f.checked(), tc.read) {
				t.Errorf("the check read %q on the call's standard input, want the empty buffer %q", f.checked(), tc.read)
			}
			row, _ := f.calls.row(question, 1)
			if row.Result == nil || *row.Result != got.Output || row.Exit == nil || *row.Exit != 0 {
				t.Errorf("the call is recorded as %+v; want gh's output and exit 0", row)
			}
		})
	}
}

// What the guard is given is the call as the routing read it: the repository it targets, which is
// the destination whose visibility decides whether this repository's own rules run, and the files
// the parse found, which is no file for every command of this tool — none of them reads one, and
// the guard therefore reads the argv with no table of its own, which would take the `-Fixed in
// abc123` of a body for a file nobody named and refuse the call over it.
//
// The visibility read is the guard's own gh child, and it is the real gh that makes it, on the
// guard's PATH, with the routed owner's gh directory and no token of the daemon's to answer in
// the App's place: the wrapper GH_PATH names runs `legion gh`, which would call a guard of its
// own over the read.
func TestTheGuardReadsTheCallsRepositoryAndFiles(t *testing.T) {
	t.Run("the repository the call targets, read by the real gh as the agent", func(t *testing.T) {
		f := newFixture(t, implementer, behaviour{Stdout: "gh's answer\n"})
		// A token and a wrapper of the daemon's own, which this environment must not carry to
		// the guard's children.
		f.answer.host.Environ = append(f.answer.host.Environ,
			"GH_TOKEN=ghp_the_daemons_own", "GITHUB_TOKEN=ghp_the_daemons_own", "GH_PATH="+f.answer.wrapper)
		argv := []string{"pr", "view", "5", "-R", "sjawhar/legion", "--json", "title"}
		if got := f.run(1, argv...); !got.Ran {
			t.Fatalf("gh %q = %+v; want it run", argv, got)
		}
		reads := f.guardReads()
		if len(reads) != 1 {
			t.Fatalf("the guard made %d visibility reads, want the one of the call's repository: %+v", len(reads), reads)
		}
		read := reads[0]
		if want := leakguard.VisibilityReadWithHeadersArgs("sjawhar/legion"); !slices.Equal(read.Argv, want) {
			t.Errorf("the guard read %q, want the visibility of the repository the call targets, %q", read.Argv, want)
		}
		values := map[string]string{}
		for _, pair := range read.Env {
			name, value, _ := strings.Cut(pair, "=")
			values[name] = value
		}
		if want := filepath.Join(f.answer.owners, "sjawhar"); values["GH_CONFIG_DIR"] != want {
			t.Errorf("the read ran with GH_CONFIG_DIR %q, want the routed owner's %q", values["GH_CONFIG_DIR"], want)
		}
		for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_PATH"} {
			if value, ok := values[name]; ok {
				t.Errorf("the read ran with %s=%q; a token or a wrapper of the daemon's answers in the App's place", name, value)
			}
		}
		path := strings.Split(values["PATH"], string(filepath.ListSeparator))
		if len(path) == 0 || path[0] != f.stand {
			t.Errorf("the read's PATH is %q, want the real gh's directory %s first", values["PATH"], f.stand)
		}
		if slices.Contains(path, filepath.Dir(f.answer.wrapper)) {
			t.Errorf("the read's PATH holds the GH_PATH wrapper's directory %s, which runs `legion gh`", filepath.Dir(f.answer.wrapper))
		}
	})

	t.Run("the files the parse found, which no command of this tool sends", func(t *testing.T) {
		f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
		body := "-Fixed in abc123"
		argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", body}
		got := f.run(1, argv...)
		if !got.Ran || got.Exit != 0 {
			t.Fatalf("gh %q = %+v; want it run: the parse found no file, so the guard reads none", argv, got)
		}
		// The same call with no files named, which is what a caller that leaves the guard to
		// read the argv itself hands it: the guard's own table takes the body for a file.
		run, why := withFooter(argv, commandWords(argv), footer(implementer, question, 1, ""))
		if why != "" {
			t.Fatalf("the call's footer: %s", why)
		}
		guard := leakguard.New(leakguard.Options{
			Check: f.path, Env: f.answer.host.Environ,
			Visibility: func(context.Context, string) (leakguard.Visibility, error) { return leakguard.Public, nil },
		})
		_, err := guard.GhCall(context.Background(), leakguard.GhRequest{Args: run, Repo: "acme/widgets"})
		var setup *leakguard.SetupFailure
		if !errors.As(err, &setup) {
			t.Fatalf("the guard reading %q with no files named = %v; want the setup failure its own table makes of the body", run, err)
		}
	})
}

// The guard is given the names of the daemon's environment that guardKeeps allows and no other
// name of it: a secret of the daemon's own, a trust store and the variable naming the check (the
// guard is handed the check's path, Host.LeakCheck) stay out, and every allowed name the daemon
// holds reaches the guard, and its gh read of the call's repository, with the daemon's value.
func TestTheGuardIsGivenOnlyTheAllowedNamesOfTheDaemonsEnvironment(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "gh's answer\n"})
	allowed := map[string]string{
		"HOME": t.TempDir(), "TMPDIR": t.TempDir(),
		"TERM": "dumb", "TZ": "UTC", "LANG": "C", "LC_ALL": "C", "LC_MESSAGES": "C",
		"HTTPS_PROXY": "http://proxy.invented:3128", "https_proxy": "http://proxy.invented:3128",
		"NO_PROXY": "*", "no_proxy": "*",
		"XDG_CONFIG_HOME": t.TempDir(), "XDG_CACHE_HOME": t.TempDir(), "XDG_DATA_HOME": t.TempDir(), "XDG_STATE_HOME": t.TempDir(),
	}
	keptOut := map[string]string{"LEGION_TEST_DAEMON_SECRET": "the daemon's own secret", "SSL_CERT_FILE": "/the/daemons/ca.pem",
		leakguard.CheckVar: f.path}
	daemon := []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range allowed {
		daemon = append(daemon, name+"="+value)
	}
	for name, value := range keptOut {
		daemon = append(daemon, name+"="+value)
	}
	f.answer.host.Environ = daemon

	configDir := filepath.Join(f.answer.owners, "acme")
	want := []string{"PATH=" + f.stand + ":" + filepath.Dir(f.host.Git) + ":" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + configDir,
		"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1"}
	for name, value := range allowed {
		want = append(want, name+"="+value)
	}
	if got := slices.Sorted(slices.Values(f.answer.guardEnv(configDir))); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("the guard is given\n%q\nwant exactly\n%q", got, slices.Sorted(slices.Values(want)))
	}

	argv := []string{"pr", "view", "5", "-R", "acme/widgets", "--json", "title"}
	if got := f.run(1, argv...); !got.Ran || got.Exit != 0 {
		t.Fatalf("gh %q = %+v; want it run", argv, got)
	}
	reads := f.guardReads()
	if len(reads) != 1 {
		t.Fatalf("the guard made %d visibility reads, want one: %+v", len(reads), reads)
	}
	values := map[string]string{}
	for _, pair := range reads[0].Env {
		name, value, _ := strings.Cut(pair, "=")
		values[name] = value
	}
	for name, value := range allowed {
		if values[name] != value {
			t.Errorf("the guard's read ran with %s=%q, want the daemon's %q", name, values[name], value)
		}
	}
	for name := range keptOut {
		if value, ok := values[name]; ok {
			t.Errorf("the guard's read ran with the daemon's %s=%q", name, value)
		}
	}
}

// The recorded calls of an answer that write are the ones Writes names, in their order, each by
// its command words, the repository its arguments name and gh's exit: a merge, a review, a
// comment, a workflow run, a run's rerun, a close, an edit, and a gh api call of every method but
// GET and HEAD, whatever form names the repository. A read is none of them (a pr, run or issue
// read, a search, a gh api GET or HEAD, a GraphQL query), and neither is a call asking for help
// or one the tool refused, which it records with no owner. A write the daemon stopped during is
// one, with no exit, and one naming no repository is named with none.
func TestWritesNamesEveryCallThatWritesAndNoRead(t *testing.T) {
	exit := func(n int) *int { return &n }
	ran := func(seq int, code *int, argv ...string) store.AnswerToolCall {
		return store.AnswerToolCall{Question: "q-1", Seq: seq, Tool: store.ToolGH, Args: argv, App: "implement", Owner: "acme", Exit: code}
	}
	calls := []store.AnswerToolCall{
		ran(1, exit(0), "pr", "view", "7", "-R", "acme/widgets", "--json", "title"),
		ran(2, exit(0), "pr", "merge", "7", "-R", "acme/widgets", "--squash"),
		ran(3, exit(0), "pr", "review", "https://github.com/acme/widgets/pull/7", "--approve", "--body", "Looks right."),
		ran(4, exit(1), "-R", "acme/widgets", "issue", "comment", "9", "--body", "On it."),
		ran(5, exit(0), "workflow", "run", "ci.yml", "--repo", "acme/widgets", "-f", "level=1"),
		ran(6, exit(0), "run", "rerun", "41", "-R", "acme/widgets", "--failed"),
		ran(7, exit(0), "api", "-X", "PUT", "repos/acme/widgets/pulls/7/merge"),
		ran(8, exit(0), "api", "repos/acme/widgets/issues/9/comments", "-f", "body=Done."),
		ran(9, exit(0), "api", "--method", "delete", "/repos/acme/widgets/issues/comments/3"),
		ran(10, exit(0), "api", "repos/acme/widgets/pulls/7"),
		ran(11, exit(0), "api", "-X", "HEAD", "repos/acme/widgets"),
		ran(12, exit(0), "api", "graphql", "-f", "query={ viewer { login } }"),
		ran(13, exit(0), "search", "prs", "--repo", "acme/widgets", "leak"),
		ran(14, exit(0), "run", "view", "41", "-R", "acme/widgets", "--log-failed"),
		ran(15, exit(0), "issue", "list", "-R", "acme/widgets"),
		ran(16, exit(0), "pr", "merge", "--help"),
		ran(17, nil, "pr", "close", "7", "-R", "acme/widgets", "--comment", "Superseded."),
		{Question: "q-1", Seq: 18, Tool: store.ToolGH, Args: []string{"pr", "merge", "8", "-R", "acme/widgets"}, App: "implement"},
		ran(19, exit(1), "pr", "edit", "7", "--add-label", "ready"),
	}
	want := []record.AgentQuestionWrite{
		{Seq: 2, Command: "pr merge", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 3, Command: "pr review", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 4, Command: "issue comment", Repository: "acme/widgets", Exit: exit(1)},
		{Seq: 5, Command: "workflow run", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 6, Command: "run rerun", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 7, Command: "api PUT repos/acme/widgets/pulls/7/merge", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 8, Command: "api POST repos/acme/widgets/issues/9/comments", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 9, Command: "api DELETE repos/acme/widgets/issues/comments/3", Repository: "acme/widgets", Exit: exit(0)},
		{Seq: 17, Command: "pr close", Repository: "acme/widgets"},
		{Seq: 19, Command: "pr edit", Exit: exit(1)},
	}
	got := Writes(calls)
	if !reflect.DeepEqual(got, want) {
		describe := func(writes []record.AgentQuestionWrite) string {
			var lines []string
			for _, w := range writes {
				exit := "none"
				if w.Exit != nil {
					exit = fmt.Sprint(*w.Exit)
				}
				lines = append(lines, fmt.Sprintf("%d %q on %q, exit %s", w.Seq, w.Command, w.Repository, exit))
			}
			return strings.Join(lines, "\n")
		}
		t.Fatalf("Writes =\n%s\nwant\n%s", describe(got), describe(want))
	}
}

// A call the model asks for writes, by its arguments as the tool is handed them, exactly when
// Writes would name it once it ran: a merge, a review, a comment, a close, an edit, a workflow run,
// a run's rerun, a gh api call of every method but GET and HEAD, and a command the allow list
// refuses. A read is none (a pr, run or issue read, a search, a gh api GET or HEAD, a GraphQL
// query), and neither is a call asking for help, one naming no subcommand, nor one whose arguments
// are no JSON array of strings, which the tool refuses before gh starts. The project's pause
// refuses the calls that write and runs the rest (internal/answer's toolSet).
func TestCallWritesIsWritesReadingOfTheModelsArguments(t *testing.T) {
	for args, writes := range map[string]bool{
		`["pr","merge","7","-R","acme/widgets","--squash"]`:                    true,
		`["pr","review","https://github.com/acme/widgets/pull/7","--approve"]`: true,
		`["-R","acme/widgets","issue","comment","9","--body","On it."]`:        true,
		`["pr","close","7","-R","acme/widgets","--comment","Superseded."]`:     true,
		`["pr","edit","7","--add-label","ready"]`:                              true,
		`["workflow","run","ci.yml","--repo","acme/widgets","-f","level=1"]`:   true,
		`["run","rerun","41","-R","acme/widgets","--failed"]`:                  true,
		`["api","-X","PUT","repos/acme/widgets/pulls/7/merge"]`:                true,
		`["api","repos/acme/widgets/issues/9/comments","-f","body=Done."]`:     true,
		`["pr","view","7","-R","acme/widgets","--json","title"]`:               false,
		`["run","view","41","-R","acme/widgets","--log-failed"]`:               false,
		`["issue","list","-R","acme/widgets"]`:                                 false,
		`["search","prs","--repo","acme/widgets","leak"]`:                      false,
		`["api","repos/acme/widgets/pulls/7"]`:                                 false,
		`["api","-X","HEAD","repos/acme/widgets"]`:                             false,
		`["api","graphql","-f","query={ viewer { login } }"]`:                  false,
		`["pr","merge","--help"]`:                                              false,
		`["pr"]`:                                                               false,
		`"pr merge 7"`:                                                         false,
		`["gist","create","notes.md"]`:                                         true,
	} {
		if got := CallWrites(args); got != writes {
			t.Errorf("CallWrites(%s) = %t, want %t", args, got, writes)
		}
	}
}
