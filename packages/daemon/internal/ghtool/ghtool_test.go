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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
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

// servedRepositories is the project's mapping the test's bundles are built from: two owners, the
// issue's own `acme` with a second repository beside the one the role writes to, and `Sjawhar`,
// whose repositories the role only reads.
func servedRepositories() []ghrepo.Repository {
	return []ghrepo.Repository{
		ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("acme/other"), ghrepo.MustParse("Sjawhar/legion"),
	}
}

// fakeTokens mints a lease per App role and scope, a read-only one marked as such, as
// appauth.Manager does, and records what it was asked.
type fakeTokens struct {
	mu    sync.Mutex
	asked []string
}

func (f *fakeTokens) Token(_ context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return f.lease("ghs_" + string(role) + "_" + strings.ToLower(repo.Owner()))
}

func (f *fakeTokens) ReadToken(_ context.Context, role appauth.AppRole, owner string, _ []string) (appauth.Lease, error) {
	return f.lease("ghs_" + string(role) + "_" + owner + "_read")
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
	record string // the file that check records every run it was handed in
	host   Host
	tokens *fakeTokens
	calls  *fakeRecorder
	logs   *bytes.Buffer
	answer *Answer
}

// The stand-in shared public leak checks the tool's guard is driven against, leakguardtest's,
// each recording the call it was handed in LEAK_CHECK_RECORD so a test can read that the check
// ran at all and with what (fixture.checked). checkRefuses answers as the check answers a write
// it refuses, its report and exit 3; checkCannotRun as one that could not be made to run.
var (
	checkPasses     = leakguardtest.Recording(leakguardtest.Passes)
	checkRefuses    = leakguardtest.Recording(leakguardtest.Refusing(checkRefusal))
	checkCannotRun  = leakguardtest.Recording(leakguardtest.CannotFinish)
	checkReadsStdin = leakguardtest.Recording(leakguardtest.Reading(0, ""))
)

// checkRefusal is the report the refusing stand-in prints, the whole of what its refusal says
// about the call.
const checkRefusal = "LEAK CHECK: refusing the gh call: one of ours"

const question = "dispatch:m-1"

var implementer = Agent{Issue: "LEG-1", Role: claim.RoleImplementer, Session: "s-1", Phase: "implement", Repository: ghrepo.MustParse("acme/widgets"), AppID: "1001"}

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
	f.host = Host{
		GH: filepath.Join(f.stand, "gh"), Git: filepath.Join(root, "gitbin", "git"), Legion: filepath.Join(root, "legion"),
		StateDir: filepath.Join(root, "state"), Calls: f.calls, Log: slog.New(slog.NewJSONHandler(f.logs, nil)), Now: time.Now,
		Credential: appauth.GitHubCredential(f.tokens, servedRepositories, ghrepo.Renames{}, nil, slog.New(slog.DiscardHandler)),
		// The guard's own two seams, as the daemon fills them: the check a stand-in of this
		// test's, since the real one is a script no clone of this repository holds, and the
		// environment this process's own with the record file the stand-in writes to added, and
		// a state home of this fixture's, where the guard keeps the public answers it read
		// (leakguard's visibility cache), so no answer another run kept stands in for this one's
		// read.
		LeakCheck: leakguardtest.Write(t, f.check, checkPasses),
		Environ:   append(os.Environ(), "LEAK_CHECK_RECORD="+f.record, "XDG_STATE_HOME="+filepath.Join(root, "daemon-state")),
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
// calls are guarded by, replacing the one it started with: the guard resolves the check at every
// call, so a call after this is checked by the new one.
func (f *fixture) leakCheck(script string) {
	f.t.Helper()
	leakguardtest.Write(f.t, f.check, script)
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

func args(argv ...string) string {
	data, err := json.Marshal(argv)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (f *fixture) run(seq int, argv ...string) Result {
	return Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: args(argv...), Bounds: bounds})
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
// implementer, review for a reviewer) with the write lease for the issue's owner, the default, and
// a read-only lease for every other owner of the project, under a fresh 0700
// <state_dir>/answers/<question>/gh, what an earlier run left there gone; Close removes it.
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
			GH: "/opt/gh/gh", Legion: "/opt/legion/legion", StateDir: stale, Calls: newFakeRecorder(), Log: slog.New(slog.DiscardHandler), Now: time.Now,
			Credential: appauth.GitHubCredential(tokens, servedRepositories, ghrepo.Renames{}, nil, slog.New(slog.DiscardHandler)),
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
		for owner, want := range map[string]string{"acme": "ghs_" + tc.app + "_acme", "sjawhar": "ghs_" + tc.app + "_sjawhar_read"} {
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

// Every refused form never starts gh: a command outside the allow list, an option opening a
// browser, gh api's --hostname, and every write form whose text the tool cannot add the footer to;
// arguments that are no JSON array of strings. A command whose owner cannot be read for certain or
// is none of the project's is refused once gh's help is read, the command itself never run. Each
// refusal names what it refuses, is what the model reads, and is recorded with no exit.
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
		{argv: []string{"api", "repos/other/thing"}, want: `"other" is not an owner of this project`, readHelp: true},
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
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: raw, Bounds: bounds})
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
// field, and `-f body=<footer>` on a POST creating or submitting a review with none. The review
// intake still reads the agent's session from it.
func TestTheFooterIsAddedToEveryWriteForm(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	foot := func(seq int) string {
		return `<!-- legion: {"session":"s-1","phase":"implement","answer":"dispatch:m-1.` + fmt.Sprint(seq) + `"} -->`
	}
	repo := []string{"-R", "acme/widgets"}
	for i, tc := range []struct {
		argv []string
		want func(foot string) []string
	}{
		{append([]string{"pr", "comment", "5", "--body", "Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-b", "Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "-b", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--body=Done."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--body=Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "-bDone."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "-bDone.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited."}, repo...), func(s string) []string {
			return append([]string{"pr", "comment", "5", "--edit-last", "--body", "Edited.\n\n" + s}, repo...)
		}},
		{append([]string{"issue", "comment", "3", "--body", "Done."}, repo...), func(s string) []string {
			return append([]string{"issue", "comment", "3", "--body", "Done.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve", "--body", "LGTM"}, repo...), func(s string) []string {
			return append([]string{"pr", "review", "5", "--approve", "--body", "LGTM\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "review", "5", "--approve"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--approve"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "--request-changes"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "--request-changes"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "review", "5", "-c"}, repo...), func(s string) []string {
			return append(append([]string{"pr", "review", "5", "-c"}, repo...), "--body", s)
		}},
		{append([]string{"pr", "close", "5", "-c", "Superseded by #6."}, repo...), func(s string) []string {
			return append([]string{"pr", "close", "5", "-c", "Superseded by #6.\n\n" + s}, repo...)
		}},
		{append([]string{"pr", "reopen", "5", "--comment", "Back."}, repo...), func(s string) []string {
			return append([]string{"pr", "reopen", "5", "--comment", "Back.\n\n" + s}, repo...)
		}},
		{[]string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done."}, func(s string) []string {
			return []string{"api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done."}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "-F", "body=Done.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done."}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/issues/5/comments", "--raw-field=body=Done.\n\n" + s}
		}},
		{[]string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited."}, func(s string) []string {
			return []string{"api", "-X", "PATCH", "repos/acme/widgets/issues/comments/7", "-f", "body=Edited.\n\n" + s}
		}},
		{[]string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE"}, func(s string) []string {
			return []string{"api", "repos/acme/widgets/pulls/5/reviews", "-f", "event=APPROVE", "-f", "body=" + s}
		}},
		{[]string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT"}, func(s string) []string {
			return []string{"api", "-X", "POST", "/repos/acme/widgets/pulls/5/reviews/9/events", "-f", "event=COMMENT", "-f", "body=" + s}
		}},
	} {
		seq := i + 1
		f.clearInvocations()
		got := f.run(seq, tc.argv...)
		want := tc.want(foot(seq))
		ran := f.invocations(false)
		if !got.Ran || len(ran) != 1 || !slices.Equal(ran[0].Argv, want) {
			t.Errorf("gh %q = %+v, ran %+v; want gh run once with %q", tc.argv, got, ran, want)
			continue
		}
		if env := f.wantEnv("acme"); !slices.Equal(ran[0].Env, env) {
			t.Errorf("gh %q ran with %q, want %q", tc.argv, ran[0].Env, env)
		}
		posted := want[slices.IndexFunc(want, func(arg string) bool { return strings.Contains(arg, "<!-- legion:") })]
		if session := (intake.PullRequestReview{Body: posted}).LegionSession(); session != "s-1" {
			t.Errorf("the review intake reads the session %q from %q, want s-1", session, posted)
		}
	}
}

// A body whose text begins with a hyphen — a removed line of a diff quoted back, `-Fixed in
// abc123` — is text, not an option: the tool reads a write form's options as pflag does and never
// reads the values they are given, so the call runs with the footer added to the body as given.
// A browser or file option of its own is still refused, in either spelling, and nothing runs.
func TestABodyBeginningWithAHyphenIsTextAndTheCallRuns(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	foot := func(seq int) string {
		return `<!-- legion: {"session":"s-1","phase":"implement","answer":"dispatch:m-1.` + fmt.Sprint(seq) + `"} -->`
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
		want := tc.want(foot(seq))
		ran := f.invocations(false)
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
		if why := refusal(argv, commandWords(argv)); why != "" {
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
		if why := refusal(tc.argv, commandWords(tc.argv)); !strings.Contains(why, tc.want) {
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
		if len(words) != 2 || allowed(words) != "" {
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
// gh_seconds is stopped, whatever it printed so far kept with a note.
func TestOutputIsCutAndASlowGhIsStopped(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: strings.Repeat("é", 30), Stderr: "warning", Exit: 1})
	got := Run(context.Background(), Call{Answer: f.answer, Seq: 1, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 10, GHCalls: 5}})
	if want := strings.Repeat("é", 10) + "\n[cut: 27 more characters]"; got.Output != want || got.Exit != 1 || !got.Ran {
		t.Errorf("a long answer = %+v; want %q, exit 1", got, want)
	}
	whole := Run(context.Background(), Call{Answer: f.answer, Seq: 2, Args: args("api", "user"), Bounds: bounds})
	if want := strings.Repeat("é", 30) + "warning"; whole.Output != want {
		t.Errorf("stdout and stderr read %q, want %q", whole.Output, want)
	}

	setBehaviour(t, f.stand, behaviour{Stdout: "partial", Sleep: time.Minute})
	start := time.Now()
	got = Run(context.Background(), Call{Answer: f.answer, Seq: 3, Args: args("api", "user"), Bounds: config.AnswerBounds{GHCall: 2 * time.Second, GHOutputChars: 100, GHCalls: 5}})
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a gh sleeping a minute under a 2s bound returned after %s", elapsed)
	}
	if !got.Ran || !strings.Contains(got.Output, "[stopped: gh ran past its 2s bound (answers.bounds.gh_seconds)]") {
		t.Errorf("a slow gh = %+v; want it stopped with the note", got)
	}
	if row, _ := f.calls.row(question, 3); row.Result == nil || row.Exit == nil {
		t.Errorf("the stopped call is recorded as %+v; want it finished", row)
	}
}

// An answer makes at most gh_calls calls: the call past the bound is refused without gh starting.
func TestTheCallPastTheBoundIsRefused(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "ok"})
	capped := config.AnswerBounds{GHCall: 20 * time.Second, GHOutputChars: 100, GHCalls: 2}
	for seq := 1; seq <= 3; seq++ {
		f.clearInvocations()
		got := Run(context.Background(), Call{Answer: f.answer, Seq: seq, Args: args("api", "user"), Bounds: capped})
		switch {
		case seq <= 2 && !got.Ran:
			t.Errorf("call %d of 2 = %+v; want it run", seq, got)
		case seq == 3 && (got.Ran || !strings.Contains(got.Output, "this answer has made its 2 gh calls")):
			t.Errorf("call 3 of 2 = %+v; want it refused", got)
		case seq == 3 && len(f.invocations(true)) != 0:
			t.Errorf("call 3 of 2 started gh: %+v", f.invocations(true))
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
	foot := func(seq int) string { return footer(implementer, question, seq) }
	comments := fmt.Sprintf(`[[{"id":11,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-11","body":%q},{"id":12,"html_url":"https://github.com/acme/widgets/pull/5#issuecomment-12","body":%q}],[{"id":13,"html_url":"x","body":"no footer"}]]`,
		"Earlier.\n\n"+foot(1), "Done.\n\n"+foot(2))
	reviews := fmt.Sprintf(`[[{"id":21,"html_url":"https://github.com/acme/widgets/pull/5#pullrequestreview-21","body":%q}]]`, "Earlier review.\n\n"+foot(1))
	setBehaviour(t, f.stand, behaviour{Responses: map[string]string{
		"api repos/acme/widgets/issues/5/comments --paginate --slurp":  comments,
		"api repos/acme/widgets/pulls/5/reviews --paginate --slurp":    reviews,
		"pr view feature-x -R acme/widgets --json number --jq .number": "5\n",
	}})
	start := func(seq int, argv ...string) {
		if err := f.calls.StartAnswerToolCall(context.Background(), store.AnswerToolCall{Question: question, Seq: seq, Args: argv, App: "implement", Owner: "acme", StartedAt: time.Now()}); err != nil {
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
			setUp: func(f *fixture) { f.answer.host.LeakCheck = filepath.Join(f.check, "moved-away.sh") },
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
			want := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done.\n\n" + footer(implementer, question, 1)}
			if !slices.Equal(ran[0].Argv, want) {
				t.Errorf("gh ran with %q, want %q", ran[0].Argv, want)
			}
			if env := f.wantEnv("acme"); !slices.Equal(ran[0].Env, env) {
				t.Errorf("gh ran with the environment\n%q\nwant exactly\n%q", ran[0].Env, env)
			}
			if checked := f.checked(); !strings.Contains(checked, "checked gh-guard "+strings.Join(want, " ")) {
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
		run, why := withFooter(argv, commandWords(argv), footer(implementer, question, 1))
		if why != "" {
			t.Fatalf("the call's footer: %s", why)
		}
		guard := leakguard.New(leakguard.Options{
			Check: f.answer.host.LeakCheck, Env: f.answer.host.Environ,
			Visibility: func(context.Context, string) (leakguard.Visibility, error) { return leakguard.Public, nil },
		})
		_, err := guard.GhCall(context.Background(), leakguard.GhRequest{Args: run, Repo: "acme/widgets"})
		var setup *leakguard.SetupFailure
		if !errors.As(err, &setup) {
			t.Fatalf("the guard reading %q with no files named = %v; want the setup failure its own table makes of the body", run, err)
		}
	})
}
