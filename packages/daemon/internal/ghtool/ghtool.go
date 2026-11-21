// Package ghtool is the `gh` tool of the daemon's answer loop: GitHub's own command line, run for
// an agent that is not running, with the GitHub access that agent's pane has. Per answer, Open
// writes the agent's credential bundle as a pane gets its own (one directory per owner its role's
// App is installed on, each holding that installation's lease at the role's own permissions, and
// the configured renames it routes by) and Close removes it; Run runs one call the model makes.
// A call runs only when it is one of the tool's commands (the allow list, checked before any
// process starts), routed to the one gh directory its repositories route to as a pane's
// `legion gh` routes it (internal/ghroute), cleared by the shared leak guard as every Legion
// write to a public repository is (internal/leakguard, the guard a pane's `legion gh` runs), with
// the agent's Legion footer on every comment or review it posts, under a scrubbed environment, a
// time bound and an output bound, and recorded before it runs and finished after
// (answer_tool_calls). A review runs only while its pull request's head is the head the agent's
// recorded round read, which its footer names. Lookup tells a restarted answer whether a comment
// or review call whose outcome was never recorded reached GitHub, by its footer.
package ghtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Agent is the stopped agent a call runs as: its issue, role, session and phase (the footer names
// the session and phase), the repository its issue targets, whose owner its role's App writes to,
// and the configured id of that App (config.GitHubApps), which the log line names beside its role.
// Head is the head its recorded round read: the commit its role's phases row records it completed
// against (record.PhaseRow.HandoffCommit), "" when the row records none. A review's footer names
// it, and a review runs only while it is the pull request's current head (Run). PullRequest is the
// number of the issue's pull request the daemon's record holds, 0 for none, which the tool's
// usage names beside the repository (Usage).
type Agent struct {
	Issue       string
	Role        claim.Role
	Session     string
	Phase       string
	Repository  ghrepo.Repository
	AppID       string
	Head        string
	PullRequest int
}

// App is the GitHub App the agent's role acts as: implement for an implementer or a merger, review
// for every other role (appauth.AppRoleFor).
func (a Agent) App() appauth.AppRole { return appauth.AppRoleFor(a.Role) }

// Usage is how the agent calls the tool: on its issue's repository and pull request.
func (a Agent) Usage() Usage { return Usage{Repository: a.Repository, PullRequest: a.PullRequest} }

// Recorder is where every call is recorded (answer_tool_calls): started before it runs, finished
// after, and read back for a restart's Lookup. *store.Store is the daemon's.
type Recorder interface {
	StartAnswerToolCall(ctx context.Context, call store.AnswerToolCall) error
	FinishAnswerToolCall(ctx context.Context, question string, seq int, result string, exit *int, at time.Time) error
	AnswerToolCall(ctx context.Context, question string, seq int) (store.AnswerToolCall, bool, error)
}

// Host is what every answer's calls run with. GH is the real gh boot found on the daemon's own
// host; Git the host's git, whose directory joins gh's on the call's PATH ("" for none); Legion
// this binary, absolute, which the wrapper GH_PATH names runs as `legion gh`. StateDir is the
// daemon's state directory, under whose answers/ each answer's directory lives. Credential is the
// function a pane's bundle comes from, appauth.GitHubCredential over the daemon's App tokens and
// every owner the role's App is installed on. Calls records each call, Log takes its line, Now is
// the clock.
//
// The last three are the leak guard's, which every call passes before gh is handed anything
// (Answer.leakGuard). LeakCheck is the shared public leak check the guard runs, required and
// asked at each call (LeakChecker), which hands the guard the check's path: a Kubernetes daemon's
// copy, installed at its pin (LeakCheck, which refuses the call until an install lands), or a
// tmux daemon's machine check, resolved in Environ at each call (MachineLeakCheck, which refuses
// the call with the setup failure naming the missing check where the machine carries none). The
// daemon states which of the two its runtime takes (internal/daemon's answersTools).
// LeakCheckManifestSHA256 is the deployment's manifest digest (leakguard.Setting.ManifestSHA256,
// config's leak_check), which the guard is told as leakguard.CheckerVar (Answer.guardEnv) and
// holds the check LeakCheck answers to, as a pane's or a pod's guard holds its role's; "" is a
// deployment that names no check, whose guard is told none.
// Environ is the daemon's own environment (os.Environ() form), of which the guard's own children —
// the check, and the gh read of a repository's visibility — start from the names guardKeeps
// allows; nil is this process's own, read at each call. It is the daemon's and not the call's: a
// call's environment carries two directories on its PATH and nothing else, which is no
// environment a shell script can run in, and resolves no check at all (Answer.guardEnv).
type Host struct {
	GH                      string
	Git                     string
	Legion                  string
	StateDir                string
	LeakCheck               LeakChecker
	LeakCheckManifestSHA256 string
	Environ                 []string
	Credential              runtime.GitHubCredential
	Calls                   Recorder
	Log                     *slog.Logger
	Now                     func() time.Time
}

// Answer is one answer's gh environment, Open's: the agent, the question, the agent's credential
// bundle, the empty directories a call runs in, the directory the leak guard copies the check
// into, the wrapper GH_PATH names, and the agent's usage as a refusal ends with it, rendered once.
// Close removes it.
type Answer struct {
	host     Host
	agent    Agent
	question string
	bundle   ghconfig.Bundle
	// dir is <state_dir>/answers/<question>/gh, everything below it the answer's own.
	dir                                   string
	owners                                string
	home, config, cache, data, state, cwd string
	// copies is dir's leak-check-copies, where the guard of each call makes its private copy of
	// the check under the deployment's digest (Answer.leakGuard, leakguard.Options.PrivateCopyBase).
	copies  string
	wrapper string
	usage   string
}

// Open builds question's gh environment for agent once, before its first call: a fresh 0700
// directory <state_dir>/answers/<question>/gh (whatever an earlier run of the same question left
// there removed first) holding the agent's bundle (host.Credential for its role and its issue's
// repository, written with ghconfig.WriteBundle, exactly as a tmux pane's is), the empty
// HOME, XDG and working directories every call runs with, the 0700 directory the leak guard
// copies the check into, and the GH_PATH wrapper. It logs the App and owners the bundle holds,
// never a token.
func Open(ctx context.Context, host Host, agent Agent, question string) (*Answer, error) {
	switch {
	case !filepath.IsAbs(host.GH):
		return nil, fmt.Errorf("the gh tool's gh %q is not an absolute path", host.GH)
	case host.Git != "" && !filepath.IsAbs(host.Git):
		return nil, fmt.Errorf("the gh tool's git %q is not an absolute path", host.Git)
	case !filepath.IsAbs(host.Legion):
		return nil, fmt.Errorf("the gh tool's legion %q is not an absolute path", host.Legion)
	case host.StateDir == "" || host.Credential == nil || host.Calls == nil || host.Log == nil || host.Now == nil:
		return nil, errors.New("the gh tool's host lacks its state directory, credential, recorder, log or clock")
	case host.LeakCheck == nil:
		return nil, errors.New("the gh tool's host names no shared public leak check for its guard to run before every call")
	case agent.Repository.IsZero():
		return nil, fmt.Errorf("the gh tool for %s's %s: the issue's repository is unknown", agent.Issue, agent.Role)
	}
	name := url.PathEscape(question)
	if question == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("the gh tool: %q is no question id a directory can be named for", question)
	}
	answers := filepath.Join(host.StateDir, "answers")
	a := &Answer{host: host, agent: agent, question: question, dir: filepath.Join(answers, name, "gh"), usage: agent.Usage().String()}
	a.owners = filepath.Join(a.dir, "owners")
	a.home, a.cwd = filepath.Join(a.dir, "home"), filepath.Join(a.dir, "work")
	a.config, a.cache = filepath.Join(a.dir, "xdg", "config"), filepath.Join(a.dir, "xdg", "cache")
	a.data, a.state = filepath.Join(a.dir, "xdg", "data"), filepath.Join(a.dir, "xdg", "state")
	a.wrapper, a.copies = filepath.Join(a.dir, "bin", "gh"), filepath.Join(a.dir, "leak-check-copies")
	if err := os.RemoveAll(a.dir); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: clear %s: %w", question, a.dir, err)
	}
	for _, d := range []string{a.home, a.cwd, a.config, a.cache, a.data, a.state, a.copies, filepath.Dir(a.wrapper)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("the gh tool for %s: %w", question, err)
		}
	}
	if err := os.Chmod(a.dir, 0o700); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: %w", question, err)
	}
	opened := false
	defer func() {
		if !opened {
			_ = os.RemoveAll(a.dir)
		}
	}()
	bundle, err := host.Credential(ctx, agent.Role, agent.Repository)
	if err != nil {
		return nil, fmt.Errorf("the gh tool for %s: the %s's github credential: %w", question, agent.Role, err)
	}
	if _, err := ghconfig.WriteBundle(a.owners, bundle); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: write the %s's github credential: %w", question, agent.Role, err)
	}
	a.bundle = bundle
	// git asks the helper gh names after GH_PATH, `<GH_PATH> auth git-credential`, for a token:
	// `legion gh auth git-credential` answers it from the directory of the owner in git's path, as
	// in a pane, and reads the bundle and the real gh from the two variables a pane's environment
	// holds, which the call's own environment does not.
	script := "#!/bin/sh\nLEGION_GH_PATH=" + shellprefix.Literal(host.GH) + "\nLEGION_GH_OWNERS_DIR=" + shellprefix.Literal(a.owners) +
		"\nexport LEGION_GH_PATH LEGION_GH_OWNERS_DIR\nexec " + shellprefix.Literal(host.Legion) + " gh \"$@\"\n"
	if err := os.WriteFile(a.wrapper, []byte(script), 0o700); err != nil {
		return nil, fmt.Errorf("the gh tool for %s: write its gh wrapper: %w", question, err)
	}
	written := bundle.Dirs[bundle.Default]
	host.Log.Info("answers: github credential written", "question", question, "issue", agent.Issue, "role", agent.Role,
		"app", written.App, "appID", agent.AppID, "expiresAt", written.ExpiresAt, "dirs", bundle.DirNames())
	opened = true
	return a, nil
}

// Close removes the answer's directory, its credential bundle with it.
func (a *Answer) Close() error {
	if err := os.RemoveAll(a.dir); err != nil {
		return fmt.Errorf("the gh tool for %s: remove %s: %w", a.question, a.dir, err)
	}
	return nil
}

// Call is one gh call the model makes in an answer: the answer it runs in (which names the agent
// and the question), its seq among the answer's calls, from 1, its arguments as the model gave
// them (a JSON array of strings, gh's arguments without `gh`; no shell reads them) and the
// answer's bounds.
type Call struct {
	Answer *Answer
	Seq    int
	Args   string
	Bounds config.AnswerBounds
}

// Result is what the model reads of a call: gh's output, stdout and stderr interleaved and cut to
// the bound with a note saying how much was cut, with the agent's usage after it where the call
// failed for naming no repository, or the refusal; gh's exit status, -1 when gh never ran or was
// stopped by a signal; and whether gh ran.
type Result struct {
	Output string
	Exit   int
	Ran    bool
}

// noRepository is git's own words, which gh passes on, for a call that named no repository: gh
// reads one from the working directory's git remotes, and the answer's working directory is
// empty, so `gh issue list --limit 1` fails with `failed to run git: fatal: not a git repository
// (or any of the parent directories): .git`. The model, told only that, makes the same call
// again; told the usage after it, it names the repository.
const noRepository = "not a git repository"

// Run runs call. It is refused, with gh never started, when its arguments are no JSON array of
// strings, its seq is past the answer's gh_calls bound, its command is none of the tool's (the
// allow list, whose refusal says how the tool is called: Usage), it submits a review the agent's
// recorded round cannot decide (reviewOf: the record names no head, the review names no pull
// request by repository and number, or names another commit_id), or it posts a comment or review
// in a form the agent's footer cannot be added to; it is refused with gh never started too when
// the host's check answers none (Host.LeakCheck: a Kubernetes daemon's not installed yet, a
// machine that carries none), with that setup failure, logged and recorded as the guard's own
// refusals are; a review is refused after reading its pull
// request's head, with the review itself never run, when that head is not the one the agent's
// recorded round read (Answer.headRefusal); a call is refused after reading gh's help, with the
// command itself never run, when the gh directory it needs cannot be read for certain, or the
// bundle holds none for it and the call writes (a read naming only such owners runs with the
// bundle's default directory, as ghroute.TargetOf routes it), or gh's help names its command otherwise than its words do, and when
// the leak guard refuses it or could not be made to run (Answer.leakGuard), which is checked last
// of all, once the call's repository and the files it would send are known and before anything
// is recorded or started. Every other call runs with the footer added to what it posts (a
// review's naming the head its round read), the gh directory it was routed to, the scrubbed
// environment, the working directory the answer's empty one, the gh_seconds bound and the
// gh_output_chars bound. It is routed by its arguments as the model gave them (Answer.route),
// since the footer adds a body and names no repository; the call with the footer is what the
// leak guard reads and what gh runs. A call that ran and failed for naming no repository
// (noRepository) has the agent's usage after gh's output. Every call is recorded with what the
// model reads of it: an allowed one before gh starts and finished after it ends, a refused one
// with its refusal; one whose start cannot be recorded does not run. Each logs one `answers: gh`
// line, and a call the guard refused one line of its own before it.
func Run(ctx context.Context, call Call) Result {
	a := call.Answer
	started := a.host.Now()
	argv, err := decodeArgs(call.Args)
	if err != nil {
		return a.refuse(ctx, call.Seq, []string{call.Args}, started, err.Error())
	}
	switch {
	case call.Seq < 1:
		return a.refuse(ctx, call.Seq, argv, started, fmt.Sprintf("call %d is no call of the answer: calls are numbered from 1", call.Seq))
	case call.Bounds.GHCall <= 0 || call.Bounds.GHOutputChars <= 0:
		return a.refuse(ctx, call.Seq, argv, started, "the gh tool has no time or output bound (answers.bounds), so it runs nothing")
	case call.Seq > call.Bounds.GHCalls:
		return a.refuse(ctx, call.Seq, argv, started, fmt.Sprintf("this answer has made its %d gh calls (answers.bounds.gh_calls); answer with what the calls so far found", call.Bounds.GHCalls))
	}
	words := commandWords(argv)
	if why := refusal(argv, words, a.usage); why != "" {
		return a.refuse(ctx, call.Seq, argv, started, why)
	}
	pr, review, why := reviewOf(argv, words, a.agent.Head)
	if why != "" {
		return a.refuse(ctx, call.Seq, argv, started, why)
	}
	head := ""
	if review {
		head = a.agent.Head
	}
	run, why := withFooter(argv, words, footer(a.agent, a.question, call.Seq, head))
	if why != "" {
		return a.refuse(ctx, call.Seq, argv, started, why)
	}
	check, err := a.host.LeakCheck.Check()
	if err != nil {
		return a.guardRefusal(ctx, call.Seq, run, started, "", err)
	}
	ctx, cancel := context.WithTimeout(ctx, call.Bounds.GHCall)
	defer cancel()
	if review {
		if why := a.headRefusal(ctx, pr, readLimit(call.Bounds)); why != "" {
			return a.refuse(ctx, call.Seq, run, started, why)
		}
	}
	target, owner, dir, err := a.route(ctx, argv)
	if err != nil {
		return a.refuse(ctx, call.Seq, run, started, err.Error())
	}
	if why := canonicalRefusal(words, target.Command, a.usage); why != "" {
		return a.refuse(ctx, call.Seq, run, started, why)
	}
	if err := a.leakGuard(ctx, check, run, target, dir); err != nil {
		return a.guardRefusal(ctx, call.Seq, run, started, targetRepo(target), err)
	}
	if err := a.host.Calls.StartAnswerToolCall(ctx, a.record(call.Seq, run, owner, started)); err != nil {
		result := Result{Output: fmt.Sprintf("refused: the call could not be recorded, so it did not run: %v", err), Exit: -1}
		a.log(call.Seq, run, owner, result)
		return result
	}
	result := a.runGH(ctx, run, dir, call.Bounds)
	if result.Ran && result.Exit != 0 && strings.Contains(result.Output, noRepository) {
		result.Output += "\n" + a.usage
	}
	var exit *int
	if result.Ran {
		exit = &result.Exit
	}
	// The call ran: its record is finished whatever the answer's own deadline did meanwhile.
	if err := a.host.Calls.FinishAnswerToolCall(context.WithoutCancel(ctx), a.question, call.Seq, result.Output, exit, a.host.Now()); err != nil {
		a.host.Log.Error("answers: gh call not finished in its record", "question", a.question, "seq", call.Seq, "error", err)
	}
	a.log(call.Seq, run, owner, result)
	return result
}

// headRefusal is the reason a review on pr is refused once the pull request's head is read, ""
// for none: its current head, read from GitHub by the daemon's own read (Answer.read: routed, the
// scrubbed environment, the answer's empty working directory, gh's answer at most limit
// characters, one `answers: gh head read` line), is not Agent.Head, the head the agent's recorded
// round read and the review's footer names, or could not be read. A review App's review decides a
// round only when the head its footer names is the pull request's current one (workflow's
// refusesRound), so a review naming the head the round read decides nothing once a push has moved
// the pull request past it, and naming the new one would say the agent read code it never read:
// the round waits for the reviewer itself.
func (a *Answer) headRefusal(ctx context.Context, pr pullRequest, limit int) string {
	out, err := a.read(ctx, headReadLine, []string{"api", "repos/" + pr.repo + "/pulls/" + pr.number, "--jq", ".head.sha"}, limit)
	current := strings.TrimSpace(string(out))
	switch {
	case err != nil:
		return fmt.Sprintf("the gh tool reads the head of the pull request a review is on before it submits the review, and the head of %s#%s could not be read: %v", pr.repo, pr.number, err)
	case current == "":
		return fmt.Sprintf("the gh tool reads the head of the pull request a review is on before it submits the review, and gh answered no head for %s#%s", pr.repo, pr.number)
	case current != a.agent.Head:
		return fmt.Sprintf("%s#%s stands at %s now, and the head your recorded round read is %s: you have not read the new head, "+
			"so no review is submitted from here (a review decides a round only when its footer names the pull request's current head, and the head it names is the one you read). "+
			"The round waits for the reviewer itself to read and review %s; answer with that.", pr.repo, pr.number, current, a.agent.Head, current)
	}
	return ""
}

// decodeArgs reads the model's arguments, a JSON array of strings.
func decodeArgs(raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil || argv == nil {
		return nil, fmt.Errorf("refused: the gh tool takes gh's arguments as a JSON array of strings, without `gh` (%q is none)", raw)
	}
	return argv, nil
}

// refuse records and logs a call refused before gh ran it, and answers its refusal.
func (a *Answer) refuse(ctx context.Context, seq int, argv []string, started time.Time, why string) Result {
	if !strings.HasPrefix(why, "refused: ") {
		why = "refused: " + why
	}
	result := Result{Output: why, Exit: -1}
	ctx = context.WithoutCancel(ctx)
	switch err := a.host.Calls.StartAnswerToolCall(ctx, a.record(seq, argv, "", started)); {
	case err != nil:
		a.host.Log.Error("answers: refused gh call not recorded", "question", a.question, "seq", seq, "error", err)
	default:
		if err := a.host.Calls.FinishAnswerToolCall(ctx, a.question, seq, result.Output, nil, a.host.Now()); err != nil {
			a.host.Log.Error("answers: refused gh call not finished in its record", "question", a.question, "seq", seq, "error", err)
		}
	}
	a.log(seq, argv, "", result)
	return result
}

// guardRefusal refuses a call over err, the leak guard's refusal, setup failure or not-found
// answer, logging the one line of its own a guard's refusal gets before the call's: the issue,
// the role, the repository the call was routed to ("" for one refused before routing) and the
// reason.
func (a *Answer) guardRefusal(ctx context.Context, seq int, run []string, started time.Time, repository string, err error) Result {
	a.host.Log.Warn("answers: gh call refused by the leak guard", "question", a.question, "seq", seq,
		"issue", a.agent.Issue, "role", a.agent.Role, "repository", repository, "why", err.Error())
	return a.refuse(ctx, seq, run, started, err.Error())
}

// record is the row a call starts with.
func (a *Answer) record(seq int, argv []string, owner string, started time.Time) store.AnswerToolCall {
	return store.AnswerToolCall{Question: a.question, Seq: seq, Args: argv, App: string(a.agent.App()), Owner: owner, StartedAt: started}
}

// log is the call's one line: the question and seq, the App (its role and configured id) and the
// owner whose gh directory it ran with ("" for a call refused before routing), its arguments as
// run, and its exit (-1 when gh never ran).
func (a *Answer) log(seq int, argv []string, owner string, result Result) {
	attrs := []any{"question", a.question, "seq", seq, "issue", a.agent.Issue, "role", a.agent.Role,
		"app", string(a.agent.App()), "appID", a.agent.AppID, "owner", owner, "args", argv, "exit", result.Exit, "ran", result.Ran}
	if !result.Ran {
		attrs = append(attrs, "refused", result.Output)
	}
	a.host.Log.Info("answers: gh", attrs...)
}
