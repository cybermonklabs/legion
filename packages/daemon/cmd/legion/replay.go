package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/replay"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// newReplayCommand is `legion replay`, a developer's tool rather than an operator's: it replays a
// recorded window of a daemon's project through the daemon's own admission pass under a named
// slot rule (internal/replay), so a rule is judged by the daemon's own decisions over a window it
// lived through. Its commands collect a window (collect, collect-merges, collect-queue,
// collect-git), neutralize one for the repository's test data (neutralize), replay one (run), read
// the measures of a window's record (scorecard), read R11's signals of a window at a moment
// (signals), fit R11's load models on a window's record (models), check the replay against the
// record (validate) and against a recorded change of the rule (forecast), compare the rules on two
// windows (compare), plan R11's limit test and name each signal's limit from its runs (limits) and
// name the rules a replay can run (rules). None of them reaches a daemon.
func newReplayCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{Use: "replay", Short: "replay a recorded window of a daemon's project through the daemon's own admission pass under a named slot rule (a developer's tool)"},
		newReplayCollectCommand(stdout, stderr),
		newReplayCollectMergesCommand(stdout, stderr),
		newReplayCollectQueueCommand(stdout, stderr),
		newReplayCollectGitCommand(stdout, stderr),
		newReplayNeutralizeCommand(stdout, stderr),
		newReplayRunCommand(stdout, stderr),
		newReplayScorecardCommand(stdout, stderr),
		newReplaySignalsCommand(stdout, stderr),
		newReplayModelsCommand(stdout, stderr),
		newReplayValidateCommand(stdout, stderr),
		newReplayForecastCommand(stdout, stderr),
		newReplayCompareCommand(stdout, stderr),
		newReplayLimitsCommand(stdout, stderr),
		newReplayRulesCommand(stdout, stderr),
	)
}

// replayCollectLong is `legion replay collect --help`'s description.
const replayCollectLong = `Collect a recorded window of a daemon's project into a directory of its own.

The daemon's lines are read from the files and directories --log names, which a person
collected: the daemon's own standard error, ` + "`kubectl logs`" + ` output (with or without
--timestamps and --prefix), or a Cloud Logging export of {timestamp, jsonPayload} rows as JSON
lines or one JSON array; any of them gzipped or not. The lines of every kind the replay's trace
reads, written in [--from − --before, --to + --after), are kept in time order, a line read twice
once.

The events of every issue those lines name, and of every blocker and child those issues' events
name, in any project, are read from Dispatch (DISPATCH_URL) from each issue's first event up to
--to + --after, with GET alone, and so are those of every issue of the project, which collect
lists (GET /api/v1/issues?project=, a page at a time, every status), and those of every issue on
the Dispatch parent chain of each issue read, in any project, up to 32 levels above it, as far as
Dispatch's own walk of a chain reads for the priority it resolves. The project's settings events
(settings.project_default_assignee.updated among them) are read from Dispatch's whole event log
(GET /api/v1/events/log), from its first event to --to + --after, since an event before the
reach sets what holds at its start. The token is the file --dispatch-token-file names, else the
file DISPATCH_TOKEN_FILE names, else DISPATCH_TOKEN; it is never printed.

The deployment's records of the signals, each optional, are read in the same reach: --provider,
the model provider's record, one line a minute; --launcher-log, the launcher's lines of the
project's issue pods (an export's {timestamp, pod, text} rows or a Cloud Logging read of
{timestamp, resource.labels.pod_name, textPayload} entries), whose memory guard lines are the
record of its kills. Under a directory --launcher-log names, only the files named *.jsonl,
*.jsonl.gz, *.json and *.json.gz are read, so an export's manifest beside them is left out; a
file named on the command line is read whatever its name. --default-assignee is the project's
default assignee at the reach's start, where no settings event before it names one.

--after is the window's run-out: a replay runs on past --to through it, so the work the record
shows after --to runs where the replay moves it, and every measure reads [--from, --to) alone.

window.json records the project, the span, --before, --after, the cap of each change the slot
lines and the daemon's start lines show, the line kinds the log holds, the deployment's settings
(the flags below, each defaulting to the daemon's own default), the parts of the span (--part),
the collector's commit (--collector, else the binary's own), the default assignee, the spans
each signal has a record in, each ancestor whose events Dispatch does not show
(missingAncestors), and the sha256 of every file. Run collect-merges next for each base branch,
and collect-queue and collect-git for each repository.`

// newReplayCollectCommand is `legion replay collect` (replay.Collect).
func newReplayCollectCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect --project <KEY> --from <time> --to <time> --before <duration> [--after <duration>] --log <file|dir>... --out <dir>",
		Short: "collect a recorded window: the daemon's lines from log files, and the Dispatch events of the issues they name",
		Long:  replayCollectLong,
		Args:  noArguments,
	}
	defaults, defaultsErr := replay.DaemonSettings()
	flags := cmd.Flags()
	project := flags.String("project", "", "the Dispatch project key of the daemon the lines are from (required)")
	from := flags.String("from", "", "the window's start, RFC 3339 (required)")
	to := flags.String("to", "", "the window's end, RFC 3339 (required)")
	before := flags.Duration("before", 0, "how far before --from the inputs reach, so the state at --from can be rebuilt (required)")
	after := flags.Duration("after", 0, "the run-out: how far past --to the inputs reach, so a replay runs the work recorded after --to")
	logs := flags.StringArray("log", nil, "a file or a directory of files of the daemon's lines; repeat for more (required)")
	provider := flags.String("provider", "", "the file of the deployment's record of the model provider, one line a minute")
	launcherLogs := flags.StringArray("launcher-log", nil, "a file, or a directory whose *.jsonl, *.jsonl.gz, *.json and *.json.gz files are read, of the launcher's lines of the project's issue pods; repeat for more")
	defaultAssignee := flags.String("default-assignee", "", "the project's default assignee at the start of the reach, where no settings event before it names one")
	out := flags.String("out", "", "the directory the window is written to, missing or empty (required)")
	tokenFile := flags.String("dispatch-token-file", "", "the file holding the Dispatch token (else DISPATCH_TOKEN_FILE, else DISPATCH_TOKEN)")
	concurrency := flags.Int("concurrency", replay.DefaultConcurrency, "how many issues are read from Dispatch at once")
	collector := flags.String("collector", "", "the commit of cybermonklabs/legion this collect runs from (else the binary's own)")
	parts := flags.StringArray("part", nil, "a part of the span as <from>,<to>,<release>,<commit>,<rule>; repeat for each part")
	tick := flags.Duration("tick-interval", defaults.TickInterval, "the deployment's controller_wake_interval_seconds, the period of the tick passes")
	gateSteps := flags.String("gate-steps", designSteps(defaults.GateSteps), "the deployment's gates.design steps, comma-separated, or off")
	linger := flags.Duration("linger", defaults.Linger, "the deployment's linger_hours")
	rounds := flags.Int("review-round-cap", defaults.ReviewRoundCap, "the deployment's review_round_cap")
	stalled := flags.Duration("stalled-tree-notice", defaults.StalledTreeNotice, "the deployment's stalled_tree_notice_hours")
	silence := flags.Duration("worker-silence", defaults.WorkerSilence, "the deployment's worker_silence_minutes")
	background := flags.Duration("worker-background", defaults.WorkerBackground, "the deployment's worker_background_hours")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if defaultsErr != nil {
			fmt.Fprintf(stderr, "legion replay collect: %v\n", defaultsErr)
			return 1
		}
		if missing := missingFlag(cmd, "project", "from", "to", "before", "log", "out"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		span, code := replaySpan(cmd, *from, *to)
		if code != 0 {
			return code
		}
		steps, err := parseDesignSteps(*gateSteps)
		if err != nil {
			return refuse(cmd, "--gate-steps: %v", err)
		}
		var windowParts []replay.Part
		for _, text := range *parts {
			part, err := parsePart(text)
			if err != nil {
				return refuse(cmd, "--part %q: %v", text, err)
			}
			windowParts = append(windowParts, part)
		}
		commit := *collector
		if commit == "" {
			if commit = binaryCommit(); commit == "" {
				return refuse(cmd, "this binary carries no commit of its own; name the commit it was built from with --collector")
			}
		}
		base := os.Getenv("DISPATCH_URL")
		if base == "" {
			fmt.Fprintln(stderr, "legion replay collect: DISPATCH_URL is not set; set it to the Dispatch server the events are read from")
			return 1
		}
		token, err := dispatchToken(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay collect: %v\n", err)
			return 1
		}
		collected, err := replay.Collect(ctx, replay.CollectConfig{
			Project: *project, From: span[0], To: span[1], Before: *before, After: *after, Logs: *logs,
			Provider: *provider, LauncherLogs: *launcherLogs, DefaultAssignee: *defaultAssignee,
			Dispatch: replay.NewDispatchReader(base, token), Concurrency: *concurrency, Parts: windowParts, Collector: commit, Out: *out,
			Settings: replay.Settings{TickInterval: *tick, GateSteps: steps, Linger: *linger, ReviewRoundCap: *rounds,
				StalledTreeNotice: *stalled, WorkerSilence: *silence, WorkerBackground: *background},
		})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay collect: %v\n", err)
			return 1
		}
		w := collected.Window
		fmt.Fprintf(stdout, "legion replay collect: wrote %s: %d of %d daemon lines kept (%d kinds, %d read twice), %d lines that are no daemon line, the events of %d issues\n",
			*out, collected.Kept, collected.Read, len(w.Lines), collected.Duplicates, collected.NotLines, len(w.Events))
		fmt.Fprintf(stdout, "legion replay collect: %d issues of %s listed, %d settings events, %d provider minutes, %d launcher lines\n",
			collected.ProjectIssues, *project, collected.SettingsEvents, len(w.Provider), len(w.Launcher))
		if len(collected.Missing) > 0 {
			fmt.Fprintf(stdout, "legion replay collect: Dispatch has no issue %s\n", strings.Join(collected.Missing, ", "))
		}
		if len(collected.MissingAncestors) > 0 {
			fmt.Fprintf(stdout, "legion replay collect: window.json names the ancestors on a Dispatch parent chain that Dispatch does not show (missingAncestors): %s\n",
				strings.Join(collected.MissingAncestors, ", "))
		}
		return 0
	})
	return cmd
}

// newReplayCollectMergesCommand is `legion replay collect-merges` (replay.CollectMerges).
func newReplayCollectMergesCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect-merges --repo <owner/name> --branch <name> --from <time> --to <time> --github-token-env <VAR> --out <dir>",
		Short: "add a base branch's first-parent history in a span, read from GitHub, to a collected window",
		Long: `Read a base branch's first-parent history in [--from, --to) from GitHub's commit listing, with
GET alone, and write it into the window collect wrote in --out as merges/<owner>__<name>.jsonl,
beside the commits of the repository's other base branches, with window.json's digest of the
file written again. The listing reads the branch's commits from a day before --from to its tip,
with no end, since a commit's first parent can be committed after it, after --to too: the walk
goes from the tip along first parents through every listed commit. A walk that ends inside the
span, at a first parent the listing lacks, reads that parent: one committed before the listing's
start is where a branch quiet before the span ends, and one committed later is a commit the
listing lacks, so the walk is refused, with nothing written. Each commit walked is then placed at
GitHub's merge event, the merged_at of the pull request GitHub records as merged as that commit
(merge_commit_sha) into --branch (base.ref), read from the pull request its subject's squash
suffix " (#<n>)" names or from those GitHub lists for the commit, and keeps that pull request
("pr") with "event": true; a replay's conflict draw reads it to leave a stretch's own merge out
of its count. A commit GitHub records no such merge of keeps its committer's time, an estimate,
with no "event" and no "pr", whatever its subject names. So does a commit whose pull request
GitHub still refuses to read after the tries, with its 422 of too many files changed, where the
pull requests GitHub lists for the commit give nothing to match it by either: the summary line
counts those, and a run again later can place them. The commits so placed in [--from, --to)
are kept. The listing reaches commits committed from a day before --from alone, so a commit
committed earlier and merged in the span is not read. The token is read from the environment
variable --github-token-env names, and is never printed. ` + githubShareHelp + ` Run it once for
each base branch the window's pull requests name.`,
		Args: noArguments,
	}
	flags := cmd.Flags()
	repo := flags.String("repo", "", "the repository as owner/name (required)")
	branch := flags.String("branch", "", "the base branch (required)")
	from := flags.String("from", "", "the span's start, RFC 3339 (required)")
	to := flags.String("to", "", "the span's end, RFC 3339 (required)")
	tokenEnv := flags.String("github-token-env", "", "the environment variable that holds the GitHub token (required)")
	api := flags.String("github-api", replay.DefaultGitHubAPI, "GitHub's REST API")
	out := flags.String("out", "", "the directory of the window collect wrote (required)")
	reserve, rate := githubShareFlags(cmd)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if missing := missingFlag(cmd, "repo", "branch", "from", "to", "github-token-env", "out"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		span, code := replaySpan(cmd, *from, *to)
		if code != 0 {
			return code
		}
		if err := replay.CheckGitHubShare(*reserve, *rate); err != nil {
			return refuse(cmd, "--github-reserve %d, --github-rate %v: %v", *reserve, *rate, err)
		}
		token := strings.TrimSpace(os.Getenv(*tokenEnv))
		if token == "" {
			fmt.Fprintf(stderr, "legion replay collect-merges: the environment variable %s holds no token\n", *tokenEnv)
			return 1
		}
		collected, err := replay.CollectMerges(ctx, replay.MergesConfig{Repo: *repo, Branch: *branch, From: span[0], To: span[1],
			Token: token, API: *api, Window: *out, Reserve: *reserve, Rate: *rate})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay collect-merges: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion replay collect-merges: wrote %d first-parent commits of %s %s into %s; "+
			"commits walked at their committer's time because GitHub would not read their pull request: %d\n",
			len(collected.Merges), *repo, *branch, *out, collected.Unread)
		return 0
	})
	return cmd
}

// githubShareHelp is the help each command that reads GitHub gives of its share of the quota.
const githubShareHelp = `GitHub's quota is the installation's, which the daemon and the other
roles share, so the command leaves them --github-reserve requests of it: after an answer that
leaves that many or fewer, it sends nothing before the quota's reset, saying so on stderr. It
sends at most --github-rate requests a second. 0 turns either off.`

// githubShareFlags declares --github-reserve and --github-rate on cmd, a command that reads GitHub:
// its share of the quota the daemon and the other roles share (replay's githubReader).
func githubShareFlags(cmd *cobra.Command) (*int, *float64) {
	flags := cmd.Flags()
	reserve := flags.Int("github-reserve", replay.DefaultGitHubReserve,
		"the requests of the shared GitHub quota left to the daemon and the other roles: at or below it, wait for the quota's reset (0: no reserve)")
	rate := flags.Float64("github-rate", replay.DefaultGitHubRate, "the most requests a second sent to GitHub (0: no pacing)")
	return reserve, rate
}

// newReplayCollectQueueCommand is `legion replay collect-queue` (replay.CollectQueue).
func newReplayCollectQueueCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect-queue --repo <owner/name> --github-token-env <VAR> --out <dir>",
		Short: "add the merge queue events of a repository's pull requests of the window's issues, read from GitHub, to a collected window",
		Long: `Read the timeline of each pull request of --repo that the window's Dispatch events attribute to
an issue of its project (issue.pull_request_attribution_changed) or that a line of its log names
(pullRequest) for an issue whose repository, as its events give it, is --repo, with GET alone
(GET /repos/<owner>/<name>/issues/<n>/timeline, every page), and write their events of the kinds
added_to_merge_queue, removed_from_merge_queue and merged, exactly as GitHub serves them, each
with its pull request's number, into the window collect wrote in --out as
queue/<owner>__<name>.jsonl, with window.json's digest of the file and a record of the queue
signal over the window's reach written again. A pull request a line names for an issue whose
repository the window does not give is named, and not read. A line names no repository, so a
pull request that only lines name, and whose timeline GitHub answers 404 Not Found in --repo, is
named and skipped; a 404 of one an attribution names fails the read. The token is read from the
environment variable --github-token-env names, and is never printed. ` + githubShareHelp + ` Run it
once for each repository of the project's issues.`,
		Args: noArguments,
	}
	flags := cmd.Flags()
	repo := flags.String("repo", "", "the repository as owner/name (required)")
	tokenEnv := flags.String("github-token-env", "", "the environment variable that holds the GitHub token (required)")
	api := flags.String("github-api", replay.DefaultGitHubAPI, "GitHub's REST API")
	out := flags.String("out", "", "the directory of the window collect wrote (required)")
	reserve, rate := githubShareFlags(cmd)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if missing := missingFlag(cmd, "repo", "github-token-env", "out"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		if err := replay.CheckGitHubShare(*reserve, *rate); err != nil {
			return refuse(cmd, "--github-reserve %d, --github-rate %v: %v", *reserve, *rate, err)
		}
		token := strings.TrimSpace(os.Getenv(*tokenEnv))
		if token == "" {
			fmt.Fprintf(stderr, "legion replay collect-queue: the environment variable %s holds no token\n", *tokenEnv)
			return 1
		}
		queued, err := replay.CollectQueue(ctx, replay.QueueConfig{Repo: *repo, Token: token, API: *api, Window: *out, Reserve: *reserve, Rate: *rate})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay collect-queue: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion replay collect-queue: wrote %d merge queue events of %d pull requests of %s into %s\n",
			queued.Lines, len(queued.PullRequests), *repo, *out)
		if len(queued.NoRepo) > 0 {
			fmt.Fprintf(stdout, "legion replay collect-queue: the log names the pull requests %v of issues whose repository the window does not give; none of them is read\n",
				queued.NoRepo)
		}
		if len(queued.NotFound) > 0 {
			fmt.Fprintf(stdout, "legion replay collect-queue: only the log names the pull requests %v, and GitHub answers 404 Not Found for each of them in %s; none of them is written\n",
				queued.NotFound, *repo)
		}
		return 0
	})
	return cmd
}

// newReplayCollectGitCommand is `legion replay collect-git` (replay.CollectGit).
func newReplayCollectGitCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect-git --repo <owner/name> --branch <name> --github-token-env <VAR> --out <dir> [--github-only | --git-only]",
		Short: "add each pull request's heads, read from GitHub, and git's verdict of merging each head with its base, to a collected window",
		Long: `Write into the window collect wrote in --out, as git/<owner>__<name>.jsonl, with window.json's
digest of the file written again, what git says of each pull request of --repo of the window's
issues: the pull requests are those collect-queue reads. Run it after collect-merges of --branch,
whose merges file it reads; it refuses a window with no merges file of --repo.

The GitHub phase reads, with GET alone, each pull request and its timeline: its merge into
--branch, and its heads, each committed event's commit at its committer's time, which only
estimates when the commit was pushed, and each force push's new tip at the push's time. It writes
each head whose time falls in the window's reach, span or run-out, the head that stood at the
reach's start, and each head a read of a merge state in the window's log names, a head the
timeline does not give at the read's time. The token is read from the environment variable
--github-token-env names, and is never printed.

The git phase reads no GitHub API. It clones --repo once into --clones, the base branch alone and
every blob it reaches (git clone --no-checkout --bare --single-branch), with the credential your
git credential helper gives, which collect-git never reads, prints or stores, and fetches each
commit it needs that the clone lacks, its blobs with it, so no merge fetches. It writes the paths
each commit of the base branch changes, and for each head the paths it changes since its merge
base and git's verdict of merging it with the base as it stands at each later commit of the base
that changes one of those paths, up to the pull request's own merge (git merge-tree --stdin, up to
200 merges to a command). Each git command runs alone, its process group killed once its memory
passes --memory-cap or it runs past its time; a kill, and anything git cannot give, is a missing
input of the file, never a clean verdict, and the run goes on.

--github-only runs the GitHub phase alone, where a GitHub token is and no git credential need be;
--git-only runs the git phase alone, from the file the GitHub phase wrote, and takes no token: a
public repository's git phase runs in a build job, a private one's where your git credential is.
A run on a window whose git file exists asks GitHub and git for nothing that file holds, so a run
that was killed goes on where it stopped. It prints a line as it finishes each pull request.

` + githubShareHelp + ` --git-only takes neither.`,
		Args: noArguments,
	}
	flags := cmd.Flags()
	repo := flags.String("repo", "", "the repository as owner/name (required)")
	branch := flags.String("branch", "", "the base branch, whose merges file collect-merges wrote (required)")
	tokenEnv := flags.String("github-token-env", "", "the environment variable that holds the GitHub token (required but with --git-only)")
	api := flags.String("github-api", replay.DefaultGitHubAPI, "GitHub's REST API")
	out := flags.String("out", "", "the directory of the window collect wrote (required)")
	clones := flags.String("clones", "", "the directory the repository is cloned into, outside the window (else clones/ beside the window)")
	memoryCap := flags.Int64("memory-cap", replay.DefaultGitMemoryCap, "the bytes of resident memory one git command's processes may hold before they are killed")
	githubOnly := flags.Bool("github-only", false, "run the GitHub phase alone: the pull requests and their heads")
	gitOnly := flags.Bool("git-only", false, "run the git phase alone, from the heads the GitHub phase wrote, reading no GitHub API")
	reserve, rate := githubShareFlags(cmd)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if missing := missingFlag(cmd, "repo", "branch", "out"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		switch {
		case *githubOnly && *gitOnly:
			return refuse(cmd, "--github-only and --git-only exclude each other; give neither to run both phases")
		case *gitOnly && (cmd.Flags().Changed("github-token-env") || cmd.Flags().Changed("github-api") ||
			cmd.Flags().Changed("github-reserve") || cmd.Flags().Changed("github-rate")):
			return refuse(cmd, "--git-only reads no GitHub API, so it takes none of --github-token-env, --github-api, --github-reserve and --github-rate")
		case !*gitOnly && *tokenEnv == "":
			return refuse(cmd, "--github-token-env is required but with --git-only")
		case *memoryCap <= 0:
			return refuse(cmd, "--memory-cap %d: name the bytes git's processes may hold, above zero", *memoryCap)
		}
		if err := replay.CheckGitHubShare(*reserve, *rate); err != nil {
			return refuse(cmd, "--github-reserve %d, --github-rate %v: %v", *reserve, *rate, err)
		}
		config := replay.GitConfig{Repo: *repo, Branch: *branch, Window: *out, Clones: *clones, MemoryCap: *memoryCap,
			GitHubOnly: *githubOnly, GitOnly: *gitOnly, Finished: func(done replay.GitPullFinished) {
				if done.Kept {
					fmt.Fprintf(stdout, "legion replay collect-git: %s phase: #%d kept as the window's git file holds it\n", done.Phase, done.PR)
					return
				}
				fmt.Fprintf(stdout, "legion replay collect-git: %s phase: #%d: %d heads, %d verdicts, %d conflicts, %d missing inputs\n",
					done.Phase, done.PR, done.Heads, done.Verdicts, done.Conflicts, done.Missing)
			}}
		if !*gitOnly {
			if config.Token = strings.TrimSpace(os.Getenv(*tokenEnv)); config.Token == "" {
				fmt.Fprintf(stderr, "legion replay collect-git: the environment variable %s holds no token\n", *tokenEnv)
				return 1
			}
			config.API, config.Reserve, config.Rate = *api, *reserve, *rate
		}
		collected, err := replay.CollectGit(ctx, config)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay collect-git: %v\n", err)
			return 1
		}
		missing := "none"
		if len(collected.Missing) > 0 {
			var parts []string
			for _, reason := range slices.Sorted(maps.Keys(collected.Missing)) {
				parts = append(parts, fmt.Sprintf("%s %d", reason, collected.Missing[reason]))
			}
			missing = strings.Join(parts, ", ")
		}
		fmt.Fprintf(stdout, "legion replay collect-git: wrote the git file of %s %s into %s: %d pull requests, %d heads, %d verdicts, %d conflicts; "+
			"missing inputs: %s; %d reads of a merge state in the log; git's peak memory %d MiB; took %s\n",
			*repo, *branch, *out, collected.PullRequests, collected.Heads, collected.Verdicts, collected.Conflicts, missing, collected.Reads,
			collected.PeakMemory>>20, collected.Took.Round(time.Second))
		if len(collected.NoRepo) > 0 {
			fmt.Fprintf(stdout, "legion replay collect-git: the log names the pull requests %v of issues whose repository the window does not give; none of them is read\n",
				collected.NoRepo)
		}
		return 0
	})
	return cmd
}

// newReplayNeutralizeCommand is `legion replay neutralize` (replay.Neutralize).
func newReplayNeutralizeCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "neutralize --in <dir> --out <dir> --project <KEY> --map <file>",
		Short: "write a collected window with every private name renamed, for test data a public repository can hold",
		Long: `Write the window in --in to --out with every private name renamed, the same name to the same
neutral name in every file: each issue key of every project and each claim token that spells
one, each repository but cybermonklabs/legion, each actor's and session's id, each session's
title, each person's login and email, each host name and machine name, each IPv4 and IPv6
address but a loopback, unspecified or documentation one, which becomes an address of the
documentation ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32), each uuid,
link, label and digest, and the free text of Dispatch's events. The keys and the people's logins
that a pod's name or a pull request's branch spells are renamed, the GitHub accounts of the merge
queue's events (login, numeric id, node id, links), the project's settings events as Dispatch's
events are, and the default assignee as a person. Every member, JSON type and list length is
kept, and so are the daemon's msgs, ranks, statuses and times. The window's own project becomes
--project, every other OTHERA, OTHERB, …. A window holding more IPv4 addresses than those ranges
hold is refused.

The map of every name replaced is written to --map, which must be outside --out and not there
yet: it holds every private name, so it is never committed. --deny names a word no file may
hold in any case (an organization's name): it is replaced with "withheld" wherever it stands,
and one left anywhere fails the run. A word that is part of "withheld" is refused, since its
replacement would hold it again.`,
		Args: noArguments,
	}
	flags := cmd.Flags()
	in := flags.String("in", "", "the window's directory (required)")
	out := flags.String("out", "", "the directory the neutral window is written to, missing or empty (required)")
	project := flags.String("project", "ACME", "the project key the window's own project becomes")
	mapFile := flags.String("map", "", "the file the map of names is written to, outside --out (required)")
	deny := flags.StringArray("deny", nil, "a word no file of the neutral window may hold, in any case; repeat for more")
	cmd.RunE = runs(func(_ context.Context, _ []string) int {
		if missing := missingFlag(cmd, "in", "out", "map"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		names, err := replay.Neutralize(replay.NeutralizeConfig{In: *in, Out: *out, Project: *project, Map: *mapFile, Deny: *deny})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay neutralize: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion replay neutralize: wrote %s, with %d issue keys and %d people renamed; the map is %s\n",
			*out, len(names.Keys), len(names.People), *mapFile)
		return 0
	})
	return cmd
}

// replayRunLong is `legion replay run --help`'s description.
const replayRunLong = `Replay the recorded window in --window through the daemon's own handler chain (the Dispatch
mirror, the workflow engine and the admission pass) under the slot rule --rule names, on a
simulated clock, in a database of the replay's own that it makes on the Postgres server
--pg-dsn names (a copy of a template database it makes once on that server for each set of the
daemon's migrations) and drops at the end unless --keep-db. It writes nothing to the database
--pg-dsn names itself, and refuses a server it cannot make a database on, with the server's
reason.

The window's Dispatch events, its recorded phase moves, ticks and boots are applied at their
recorded times or offsets, a worker stub runs the outbox rows the daemon's lanes would take after
each fact, and each work segment runs its recorded length times f^((n − m)/42) at --slowdown f.
The main branch keeps recorded conflicts. The git branch uses stored verdicts and draws conflicts
for unanswered reads. The drawn branch uses the same fitted generator for every eligible stretch.
The run prints the model with its intervals. Set knobs with --branch main+<knob>=<value>, and add
another +<knob>=<value> for each additional change. The knobs are timing, answer, afterSendBack,
firstStarts, neverStarted, and conflicts (recorded, git or drawn).
A window collected with a run-out (collect --after) is replayed on past its end through the
run-out, so the work the record shows after --to runs where the replay moves it; every measure
reads the window's own span alone. --cap replays the window at one admission cap from its start to
its end in place of the caps its record shows (0, the default, keeps the record's), and run.json
names it.
--limit <signal>=<value>, given once for each signal, sets that signal's limit (legion replay
signals names them); under a rule that admits below P0 by signals (signals, signals-backoff) a
pass then starts an issue below P0 only while each signal with a limit reads under it, and the
run keeps the project's one deadline, the next moment a signal would cross its limit with no
event, whose pass is a deadline's. A signal whose load model gives no limit has none in the run,
which says so, and a signal rule's run with no limit is one in which no signal binds. --idle-after
is d, how long a slot holder has been idle before the idle signal counts it. --probe, given once
for each moment, has run.json give each limited signal's value there. --token-limit is the
project's token limit in uncached input tokens a minute, which the replay takes in each minute
whose provider record holds no limit of the project's own (assumption 49); a limit the record
holds stands. The tokens model then limits the provider's signal and its refusals where it passes
R11's daily test or gives no test only because the window records no limit of the project's own.
Nothing outside is asked: no Dispatch, no GitHub, no NATS.

--out receives log.jsonl (the daemon's lines on the simulated clock), scorecard.json (the
measures read from the states the daemon's own admission passes left), scorecard-line.json (the
same measures read back from log.jsonl and the replay's Dispatch events, as a record's are),
run.json (what ran, the commit this binary was built from, or unknown, and the run's start on
the wall clock, what it counted, the gap of each measure between the two scorecards, the
conflict model, the limits, the token limit, d, the ceiling, each slot each pass gave out or took
back with the pass's kind, each change of the deadline and each probe's values) and, with
--trace-segments, segments.jsonl. The output names the branch, each
assumption the run rests on and the scenario it gave the work of an issue that never started,
and counts the issues admitted with no work with the slot-hours they hold and, under a drawn
scenario, how many issues drew their work from their own tree, from their priority and
repository, or from the whole window.`

// newReplayRunCommand is `legion replay run` (replay.Run).
func newReplayRunCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run --window <dir> --rule <name> --pg-dsn <dsn> --out <dir> [--branch <name>] [--seed <n>] [--slowdown <f>] [--cap <n>] [--limit <signal>=<value>]... [--idle-after <duration>] [--probe <time>]... [--token-limit <tokens a minute>] [--keep-db] [--trace-segments]",
		Short: "replay a recorded window under a slot rule, in a database of its own, and write what it ran and counted",
		Long:  replayRunLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowDir := flags.String("window", "", "the directory of the recorded window (required)")
	ruleName := flags.String("rule", "", "the slot rule to replay under, as legion replay rules names it (required)")
	branchName := flags.String("branch", replay.MainBranch.Name, "the branch: main, or main+<knob>=<value>")
	seed := flags.Int64("seed", 1, "the seed of every draw the replay makes")
	slowdown := flags.Float64("slowdown", 1, "the factor f a segment's recorded length is scaled by for each 42 issues in flight above the record's")
	dsn := flags.String("pg-dsn", "", "a Postgres server the replay makes its own database on (required)")
	out := flags.String("out", "", "the directory the run's output is written to (required)")
	keepDB := flags.Bool("keep-db", false, "keep the replay's database after the run")
	traceSegments := flags.Bool("trace-segments", false, "write each work segment's n, m and length to segments.jsonl")
	capOverride := flags.Int("cap", 0, "the admission cap the replay serves under for the whole window, in place of the record's (0 keeps the record's)")
	limitTexts := flags.StringArray("limit", nil, "a signal's limit as <signal>=<value>, the signal one of "+strings.Join(signals.Names(), ", ")+", given once for each signal")
	idleAfter := flags.Duration("idle-after", signals.DefaultIdleAfter, "d, how long a slot holder has been idle before the idle signal counts it")
	probeTexts := flags.StringArray("probe", nil, "a moment, in RFC 3339, at which run.json gives each limited signal's value, given once for each")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if missing := missingFlag(cmd, "window", "rule", "pg-dsn", "out"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		rule, code := namedSlotRule(cmd, *ruleName)
		if code != 0 {
			return code
		}
		branch, err := replay.BranchNamed(*branchName)
		if err != nil {
			return refuse(cmd, "--branch: %v", err)
		}
		if *slowdown <= 0 {
			return refuse(cmd, "--slowdown %v: the factor must be above zero", *slowdown)
		}
		if *capOverride < 0 {
			return refuse(cmd, "--cap %d: the cap must be zero, which keeps the record's, or above it", *capOverride)
		}
		limits := make(map[string]float64, len(*limitTexts))
		for _, text := range *limitTexts {
			name, value, named := strings.Cut(text, "=")
			limit, err := strconv.ParseFloat(value, 64)
			switch _, twice := limits[name]; {
			case !named || !slices.Contains(signals.Names(), name):
				return refuse(cmd, "--limit %q names no signal: give <signal>=<value>, the signal one of %s", text, strings.Join(signals.Names(), ", "))
			case err != nil:
				return refuse(cmd, "--limit %q: the value is no number", text)
			case twice:
				return refuse(cmd, "--limit %q: the signal %s has a limit already; give each signal one", text, name)
			}
			if err := signals.CheckLimit(name, limit); err != nil {
				return refuse(cmd, "--limit %q: %v", text, err)
			}
			limits[name] = limit
		}
		if *idleAfter < 0 {
			return refuse(cmd, "--idle-after %s is below zero", *idleAfter)
		}
		probes := make([]time.Time, 0, len(*probeTexts))
		for _, text := range *probeTexts {
			at, err := time.Parse(time.RFC3339, text)
			if err != nil {
				return refuse(cmd, "--probe %q is no RFC 3339 time", text)
			}
			probes = append(probes, at)
		}
		if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
			return code
		}
		window, err := replay.ReadWindow(*windowDir)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay run: %v\n", err)
			return 1
		}
		result, err := replay.Run(ctx, replay.RunConfig{Window: window, Rule: rule, Branch: branch, Seed: *seed, Slowdown: *slowdown,
			PGDSN: *dsn, Out: *out, KeepDB: *keepDB, TraceSegments: *traceSegments, BuildCommit: binaryCommit(), Cap: *capOverride,
			Limits: limits, IdleAfter: *idleAfter, Probes: probes, TokenLimit: *tokenLimit})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay run: %v\n", err)
			return 1
		}
		kept := "dropped"
		if *keepDB {
			kept = "kept"
		}
		counted := result.Divergence
		capWord := "the record's cap"
		if *capOverride > 0 {
			capWord = fmt.Sprintf("the cap %d", *capOverride)
		}
		fmt.Fprintf(stdout, "legion replay run: replayed %s under the rule %s on the branch %s (seed %d, f %v, %s) in the database %s (%s); wrote %s\n",
			*windowDir, rule.Name, branch.Name, *seed, *slowdown, capWord, result.Database, kept, *out)
		if window.After > 0 {
			fmt.Fprintf(stdout, "legion replay run: replayed on to %s through the window's run-out of %s; every measure reads %s to %s alone\n",
				window.End().Format(time.RFC3339), time.Duration(window.After), window.From.Format(time.RFC3339), window.To.Format(time.RFC3339))
		}
		fmt.Fprintf(stdout, "legion replay run: %s\n", result.Counts())
		if len(result.MissingAncestors) > 0 {
			fmt.Fprintf(stdout, "legion replay run: %s\n", replay.MissingAncestorsLine(*windowDir, result.MissingAncestors))
		}
		for _, name := range slices.Sorted(maps.Keys(result.Unlimited)) {
			fmt.Fprintf(stdout, "legion replay run: the %s model gives no limit for its signal, so %s has no limit in this run\n", result.Unlimited[name], name)
		}
		if rule.Signals != signals.Off {
			if len(limits) == len(result.Unlimited) {
				fmt.Fprintf(stdout, "legion replay run: the rule %s admits below P0 by signals, and no signal has a limit in this run, so no signal binds\n", rule.Name)
			}
			byAction := map[string]int{}
			for _, entry := range result.Deadlines {
				byAction[entry.Action]++
			}
			fmt.Fprintf(stdout, "legion replay run: the deadlines of its signals: %d set, %d moved, %d dropped, %d fired\n",
				byAction["set"], byAction["moved"], byAction["dropped"], byAction["fired"])
		}
		for _, refused := range counted.Refused {
			fmt.Fprintf(stdout, "legion replay run: refused %s %s→%s by %s at %s: %s\n", refused.Issue, refused.From, refused.To, refused.Trigger,
				refused.At.Format(time.RFC3339), refused.Reason)
		}
		fmt.Fprintf(stdout, "legion replay run: the scenario of the work of an issue that never started: %s\n", result.Scorecard.Scenario)
		fmt.Fprintf(stdout, "legion replay run: the conflict model each stretch drew by: %s\n", result.Conflicts)
		if widest, ok := widestGap(result.Gaps); ok && widest.Gap() != 0 {
			fmt.Fprintf(stdout, "legion replay run: the widest gap between the snapshot and the line scorecards is %s: %v and %v\n",
				widest.Measure, widest.Snapshot, widest.Line)
		} else {
			fmt.Fprintln(stdout, "legion replay run: the snapshot and the line scorecards agree on every measure")
		}
		fmt.Fprintln(stdout, "legion replay run: the assumptions it rests on:")
		for _, number := range result.Assumptions {
			for _, assumption := range replay.Assumptions {
				if assumption.Number != number {
					continue
				}
				if finished := counted.Finished[number]; finished > 0 {
					fmt.Fprintf(stdout, "  %d. %s (%d outbox rows finished under it)\n", number, assumption.Text, finished)
				} else {
					fmt.Fprintf(stdout, "  %d. %s\n", number, assumption.Text)
				}
			}
		}
		return 0
	})
	return cmd
}

// widestGap is the measure of gaps whose snapshot and line values stand furthest apart, and false
// where gaps holds none.
func widestGap(gaps []replay.MeasureGap) (replay.MeasureGap, bool) {
	var widest replay.MeasureGap
	for i, gap := range gaps {
		if i == 0 || math.Abs(gap.Gap()) > math.Abs(widest.Gap()) {
			widest = gap
		}
	}
	return widest, len(gaps) > 0
}

// newReplayScorecardCommand is `legion replay scorecard` (replay.Score): the measures of a recorded
// window read from its record alone, by the code a replay's are read by, with no replay and no
// database.
func newReplayScorecardCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scorecard --window <dir> [--json]",
		Short: "read every measure of a recorded window from its record alone, with no replay",
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowDir := flags.String("window", "", "the directory of the recorded window (required)")
	asJSON := flags.Bool("json", false, "print the scorecard as JSON")
	cmd.RunE = runs(func(context.Context, []string) int {
		if missing := missingFlag(cmd, "window"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		window, err := replay.ReadWindow(*windowDir)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay scorecard: %v\n", err)
			return 1
		}
		card, missing, err := replay.ScoreRecord(window)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay scorecard: %v\n", err)
			return 1
		}
		if *asJSON {
			return printReplayJSON(stdout, card)
		}
		fmt.Fprintf(stdout, "the record of %s from %s to %s\n", window.Project, card.From.Format(time.RFC3339), card.To.Format(time.RFC3339))
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "%s\n", replay.MissingAncestorsLine(*windowDir, missing))
		}
		for _, measured := range replay.MeasuresRead(card, missing) {
			fmt.Fprintf(stdout, "%s %s\n", measured.Name, strconv.FormatFloat(measured.Value, 'f', -1, 64))
		}
		for _, key := range slices.Sorted(maps.Keys(card.FirstSlot)) {
			fmt.Fprintf(stdout, "firstSlot[%s] %s\n", key, card.FirstSlot[key].Format(time.RFC3339))
		}
		for _, key := range slices.Sorted(maps.Keys(card.DoneAt)) {
			fmt.Fprintf(stdout, "doneAt[%s] %s\n", key, card.DoneAt[key].Format(time.RFC3339))
		}
		return 0
	})
	return cmd
}

// replaySignalsLong is `legion replay signals --help`'s description.
const replaySignalsLong = `Print R11's signals of a recorded window at each moment --at names, each read from the
window's own record, and each one's next crossing of each limit --limit gives.

idle is the share of the slot holders idle for at least --idle-after (d); queue the merge
queue's wait in hours, the larger of the median wait of the pull requests that left it in the 6
hours before and the wait so far of the one longest in it; provider the share of the project's
limit of uncached input tokens the minute before used, with refusals, the refusals of the hour
before, by the project's own limit and by the provider; kills the memory guard's kills of the
hour before; approvals the asks open that wait on the project's default assignee. Lower is
better for each. A signal the window holds no record of at a moment is unrecorded, never zero,
and binds nothing.

--limit <signal>=<value>, given once for each limit, adds the signal's next crossing of that
limit: the next moment after --at at which it would start or stop binding, recorded and at or
above the limit, if no new event came, or none. approvals changes only at events, so it has no
next crossing.

--token-limit is the project's token limit in uncached input tokens a minute, which the
provider's headroom reads in each minute whose record holds no limit of the project's own
(assumption 49), as a replay given it does; a limit the record holds stands, and a minute outside
the record of the provider stays unrecorded.

Where the window holds the launcher's lines, it also prints the memory guard's kills of those
lines matched with the daemon's lines of the kills, kill by kill, and each kill one of the two
lacks.`

// newReplaySignalsCommand is `legion replay signals` (replay.SignalDefinitions, replay.MatchKills):
// R11's signals of a recorded window at each moment named, read from its record with no replay.
func newReplaySignalsCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "signals --window <dir> --at <time>... [--limit <signal>=<value>...] [--idle-after <duration>] [--token-limit <tokens a minute>]",
		Short: "print R11's signals of a recorded window at each moment named, with each one's next crossing of each limit given",
		Long:  replaySignalsLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowDir := flags.String("window", "", "the directory of the recorded window (required)")
	atTexts := flags.StringArray("at", nil, "a moment to read the signals at, in RFC 3339, given once for each (required)")
	limitTexts := flags.StringArray("limit", nil, "a limit as <signal>=<value>, the signal one of "+strings.Join(signals.Names(), ", ")+", given once for each")
	idleAfter := flags.Duration("idle-after", signals.DefaultIdleAfter, "d, how long a slot holder has been idle before the idle signal counts it")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	cmd.RunE = runs(func(context.Context, []string) int {
		if missing := missingFlag(cmd, "window", "at"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		moments := make([]time.Time, 0, len(*atTexts))
		for _, text := range *atTexts {
			at, err := time.Parse(time.RFC3339, text)
			if err != nil {
				return refuse(cmd, "--at %q is no RFC 3339 time", text)
			}
			moments = append(moments, at)
		}
		limits := make(map[string][]float64)
		for _, text := range *limitTexts {
			name, value, named := strings.Cut(text, "=")
			limit, err := strconv.ParseFloat(value, 64)
			switch {
			case !named || !slices.Contains(signals.Names(), name):
				return refuse(cmd, "--limit %q names no signal: give <signal>=<value>, the signal one of %s", text, strings.Join(signals.Names(), ", "))
			case err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0:
				return refuse(cmd, "--limit %q: the value is no number at or above zero", text)
			}
			limits[name] = append(limits[name], limit)
		}
		if *idleAfter < 0 {
			return refuse(cmd, "--idle-after %s is below zero", *idleAfter)
		}
		if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
			return code
		}
		window, err := replay.ReadWindow(*windowDir)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay signals: %v\n", err)
			return 1
		}
		signalRecord, err := replay.ReadSignalRecord(window, *idleAfter, *tokenLimit)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay signals: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "the signals of %s, idle after %s\n", window.Project, *idleAfter)
		for _, at := range moments {
			fmt.Fprintf(stdout, "at %s\n", at.Format(time.RFC3339))
			for _, def := range replay.SignalDefinitions() {
				var next func(float64) (time.Time, bool)
				if def.Next != nil {
					next = func(limit float64) (time.Time, bool) { return def.Next(signalRecord, at, limit) }
				}
				printReplaySignal(stdout, def.Name, replaySignalText(def.At(signalRecord, at)), limits[def.Name], next)
				if def.Name != replay.SignalProvider {
					continue
				}
				refusals, text := signalRecord.Refusals(at), "unrecorded"
				if refusals.Recorded {
					text = fmt.Sprintf("%d (own %d, provider %d)", refusals.Count(), refusals.Own, refusals.Provider)
				}
				printReplaySignal(stdout, "refusals", text, limits["refusals"], func(limit float64) (time.Time, bool) { return signalRecord.NextRefusals(at, limit) })
			}
		}
		if window.Launcher == nil {
			return 0
		}
		match, err := replay.MatchKills(window)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay signals: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "the memory guard's kills: %d in both the launcher's lines and the daemon's\n", match.Both)
		for _, side := range []struct {
			name  string
			kills []replay.GuardKill
		}{{"the launcher's lines", match.LauncherOnly}, {"the daemon's lines", match.DaemonOnly}} {
			for _, kill := range side.kills {
				fmt.Fprintf(stdout, "  only in %s: %s %s %s pid %d\n", side.name, kill.At.Format(time.RFC3339Nano), kill.Pod, kill.Role, kill.PID)
			}
		}
		return 0
	})
	return cmd
}

// replaySignalText is a signal's value as `legion replay signals` prints it: the number, or
// unrecorded.
func replaySignalText(value signals.Value) string {
	if !value.Recorded {
		return "unrecorded"
	}
	return strconv.FormatFloat(value.Value, 'f', -1, 64)
}

// printReplaySignal prints one signal at a moment, text its value, and its next crossing of each of
// limits as next gives it, or, where next is nil, that it changes only at events.
func printReplaySignal(stdout io.Writer, name, text string, limits []float64, next func(float64) (time.Time, bool)) {
	fmt.Fprintf(stdout, "  %s %s\n", name, text)
	for _, limit := range limits {
		shown := strconv.FormatFloat(limit, 'f', -1, 64)
		if next == nil {
			fmt.Fprintf(stdout, "    limit %s: no crossing, it changes only at events\n", shown)
			continue
		}
		if at, due := next(limit); due {
			fmt.Fprintf(stdout, "    limit %s: crosses at %s\n", shown, at.Format(time.RFC3339))
		} else {
			fmt.Fprintf(stdout, "    limit %s: no crossing due\n", shown)
		}
	}
}

// replayModelsLong is `legion replay models --help`'s description.
const replayModelsLong = `Fit R11's three load models on a recorded window's record alone, with no replay, and print
each model's form, parameters, daily test and rates by load.

Each model keeps what the window records and moves with the work in flight of a replay: tokens,
each minute's recorded uncached input tokens for each issue in flight, times the replay's issues in
flight, a minute over the project's own limit being a refused minute and the provider's refusals
kept as recorded; memory, each memory guard kill at its offset in the work segment of its own
issue, and the day's kills for each hour of an issue in flight for work the record never ran;
queue, a queue for each repository, served by the record's own exits from its head and by its
rate of them for each hour in which it held a pull request. No model reads the pods' memory.

R11's daily test, at the recorded work in flight: on a day with at least 10 refused minutes or
kills recorded, the model within 30% of the record; on each other day, at most 10; the merge
queue's median wait within 30% on each day. The kill and queue models give the record by their
form there, and the output says so. Beside each model it prints the signal's rate in each range
of 20 issues in flight that the window records, with the hours spent in it.

With --against, it fits the models on that record too and runs R11's test across two records:
each model's rate on --window within 30% of the same rate on --against, the tokens for each minute
of an issue in flight, the kills for each hour of an issue in flight counted from --kills-from,
and the exits from the head of each repository's queue for each hour in which it held a pull
request, for each repository with at least 50 such exits in each record; the output names each
other repository as untested.

A model is fitted and tested only where the window holds its record, and the output names each
span it skips and why. A model gives a limit for its signal only where it passes the daily test
and, with --against, the test across two records; without --against the output says that the test
across records did not run. --json prints the same as JSON.`

// killsFromDefault is where R11's test across two records counts the kills from by default:
// 2026-11-18, the first day after the first full day of the memory guard, which the record keeps
// as it is (spec R11).
const killsFromDefault = "2026-11-18T00:00:00Z"

// newReplayModelsCommand is `legion replay models` (replay.FitLoadModels): R11's load models fitted
// on a recorded window's record with no replay, each day tested by R11's daily test, and, with
// --against, R11's test across two records (replay.CompareRecords).
func newReplayModelsCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models --window <dir> [--against <dir>] [--kills-from <time>] [--json]",
		Short: "fit R11's load models on a recorded window's record and print each one's tests and rates by load, with no replay",
		Long:  replayModelsLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowDir := flags.String("window", "", "the directory of the recorded window (required)")
	againstDir := flags.String("against", "", "the directory of a second recorded window, for R11's test across two records")
	killsFromText := flags.String("kills-from", killsFromDefault, "the moment, in RFC 3339, from which the test across two records counts the kills")
	asJSON := flags.Bool("json", false, "print the models as JSON")
	cmd.RunE = runs(func(context.Context, []string) int {
		if missing := missingFlag(cmd, "window"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		killsFrom, err := time.Parse(time.RFC3339, *killsFromText)
		if err != nil {
			return refuse(cmd, "--kills-from %q is no RFC 3339 time", *killsFromText)
		}
		fit := func(dir string) (replay.LoadModels, error) {
			window, err := replay.ReadWindow(dir)
			if err != nil {
				return replay.LoadModels{}, err
			}
			return replay.FitLoadModels(window)
		}
		models, err := fit(*windowDir)
		if err == nil && *againstDir != "" {
			var against replay.LoadModels
			if against, err = fit(*againstDir); err == nil {
				models, err = replay.CompareRecords(models, against, killsFrom)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion replay models: %v\n", err)
			return 1
		}
		if *asJSON {
			return printReplayJSON(stdout, models)
		}
		if err := models.Write(stdout); err != nil {
			fmt.Fprintf(stderr, "legion replay models: %v\n", err)
			return 1
		}
		return 0
	})
	return cmd
}

// replayValidateLong is `legion replay validate --help`'s description.
const replayValidateLong = `Check the replay of a recorded window against its record (the tool's validation).

The window in --window is replayed under --rule, the rule its daemon ran, on every branch the tool
offers: the main branch and each change of one knob from it, each at the slowdown factors 1, 1.2
and 1.6, with 5 seeds for a branch that draws (a conflict, or the work of an issue that never
started) and the seed 1 alone for one that draws nothing. Up to --parallel runs run at once, each
in a database of its own on the server --pg-dsn names. Each run's run.json, scorecard.json and
scorecard-line.json are written under --out, in <window>/<rule>/<branch>@<f>/seed-<n>/, beside the
window's record.json; a run whose files are there already is not run again, and each run's log is
deleted once the run has read it back, unless --keep-logs. With --main-only only main at f 1 runs.
Main keeps recorded conflicts. At each slowdown validation reports the issues main starts that
the record did not start, and those it leaves out that the record filed. The git branch reports
read counts by verdict and kind, with boot reads separate, and loop hours by source. The drawn
branch reports its validation verdict. A failing drawn branch gives no comparison on that branch.

It prints, for each branch and each measure, the recorded value, the replayed value (the mean of
the seeds, with the lowest and the highest seed's), the gap, the allowance and the test; for each
issue the error of its first slot and of its done, with their median and 90th percentile; whether
the window's main branch (main at f 1) passes; and which branches give no comparison. The record
and each replay are read from their line timelines, so like is compared with like. The gaps: 10%
of the issues finished and of the slots given out on each day, 0.05 of the share of priority
taken, 60 minutes at the median of each issue's first-slot error, and 30% of every other measure.
Beside the tests, and testing nothing, it prints for each branch and seed how many issues the
replay gave a first slot that the record did not, and how many the record gave one that the replay
did not; and for each branch the trees whose design gate stood closed more than 30% longer or shorter
than in the record. A validation whose replays give no issue a first slot or a done is refused.

With --cap-backtest --cap <n> the validation is the cap backtest: the main branch at f 1 alone, with
its seeds, runs at the caps the record shows into --out and again at the one cap --cap from the
window's start to its end into --out's cap-<n>, and it prints the slots each gives out in each span
of one cap the daemon served against the record's: under the recorded caps each span within 10%,
and under --cap each span whose cap is not --cap more than 10% off, a span at --cap not read. A
backtest that fails either says that the tool cannot tell a change of the cap. A span of fewer than
10 slots fails on a miss of one slot, and the backtest names each such span.

--token-limit gives every run the project's token limit in uncached input tokens a minute, which
each takes in each minute whose provider record holds no limit of the project's own (legion replay
run --token-limit, assumption 49); a run --out holds already that ran with another one, or with
none, is refused.

With --from-runs nothing runs: the validation is read from the directories of runs a validate or
a compare wrote, --window naming the window by its directory's name (needed only where they hold
more than one) and --rule defaulting to the rule its daemon ran. Each run keeps the token limit it
ran with, in its run.json, so --token-limit does not apply.`

// newReplayValidateCommand is `legion replay validate` (replay.RunValidate, replay.Runs.Validation).
func newReplayValidateCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate --window <dir> --rule <name> --pg-dsn <dsn> --out <dir> [--parallel <n>] [--keep-logs] [--main-only | --cap-backtest --cap <n>] [--token-limit <tokens a minute>] | --from-runs <dir>... [--window <name>] [--rule <name>]",
		Short: "replay a window under the rule its daemon ran on every branch, and test each branch against the record",
		Long:  replayValidateLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowDir := flags.String("window", "", "the directory of the recorded window; with --from-runs, the window's name")
	ruleName := flags.String("rule", "", "the rule the window's daemon ran, as legion replay rules names it")
	dsn := flags.String("pg-dsn", "", "a Postgres server each run makes its own database on")
	out := flags.String("out", "", "the directory of runs every run is written under")
	parallel := flags.Int("parallel", 1, "how many runs run at once")
	keepLogs := flags.Bool("keep-logs", false, "keep each run's log.jsonl")
	mainOnly := flags.Bool("main-only", false, "run the main branch at f 1 alone, with its seeds, in place of every branch")
	capBacktest := flags.Bool("cap-backtest", false, "run the cap backtest: the main branch at f 1 at the recorded caps and at --cap, read in each span of one cap")
	capFlat := flags.Int("cap", 0, "with --cap-backtest, the one admission cap the backtest replays the window at from its start to its end")
	fromRuns := flags.StringArray("from-runs", nil, "a directory of runs to read the validation from, running nothing; repeat for more")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	asJSON := flags.Bool("json", false, "print the validation as JSON")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		var validation replay.Validation
		if len(*fromRuns) > 0 {
			if set := changedFlag(cmd, "pg-dsn", "out", "parallel", "keep-logs", "main-only", "cap-backtest", "cap", "token-limit"); set != "" {
				return refuse(cmd, "--from-runs reads runs and makes none, so --%s does not apply", set)
			}
			read, err := replay.ReadRuns(*fromRuns...)
			if err != nil {
				fmt.Fprintf(stderr, "legion replay validate: %v\n", err)
				return 1
			}
			name := *windowDir
			if name == "" {
				windows := read.Windows()
				if len(windows) != 1 {
					return refuse(cmd, "the runs hold %d windows (%s); name one with --window", len(windows), strings.Join(windows, ", "))
				}
				name = windows[0]
			}
			rule := *ruleName
			if rule == "" {
				if rule, err = read.Rule(name); err != nil {
					fmt.Fprintf(stderr, "legion replay validate: %v\n", err)
					return 1
				}
			}
			if validation, err = read.Validation(name, rule); err != nil {
				fmt.Fprintf(stderr, "legion replay validate: %v\n", err)
				return 1
			}
		} else {
			if missing := missingFlag(cmd, "window", "rule", "pg-dsn", "out"); missing != "" {
				return refuse(cmd, "--%s is required", missing)
			}
			rule, code := namedSlotRule(cmd, *ruleName)
			if code != 0 {
				return code
			}
			if *parallel < 1 {
				return refuseParallel(cmd, *parallel)
			}
			switch {
			case *capBacktest && *mainOnly:
				return refuse(cmd, "--cap-backtest runs the main branch alone already, so --main-only does not apply")
			case *capBacktest && *capFlat <= 0:
				return refuse(cmd, "--cap-backtest needs --cap, the one cap above zero the window is replayed at")
			case !*capBacktest && cmd.Flags().Changed("cap"):
				return refuse(cmd, "--cap is the cap backtest's alone; give --cap-backtest with it")
			}
			if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
				return code
			}
			var err error
			validation, err = replay.RunValidate(ctx, replay.ValidateConfig{Window: *windowDir, Rule: rule, PGDSN: *dsn, Parallel: *parallel,
				Out: *out, KeepLogs: *keepLogs, MainOnly: *mainOnly, CapBacktest: *capBacktest, Cap: *capFlat, Progress: replayProgress(stderr, "validate"),
				BuildCommit: binaryCommit(), TokenLimit: *tokenLimit})
			if err != nil {
				fmt.Fprintf(stderr, "legion replay validate: %v\n", err)
				return 1
			}
		}
		if *asJSON {
			return printReplayJSON(stdout, validation)
		}
		if err := validation.Write(stdout, 1, func(replay.BranchAt) bool { return true }); err != nil {
			return 1
		}
		return 0
	})
	return cmd
}

// replayForecastLong is `legion replay forecast --help`'s description.
const replayForecastLong = `Forecast the window after a recorded change of the rule from the window before it.

The window in --before, recorded before a change of its daemon's rule, is replayed under --rule,
the rule after the change, on the main branch (main at f 1) with 5 seeds where it draws and the
seed 1 alone where it draws nothing, up to --parallel runs at once, each in a database of its own
on the server --pg-dsn names; each run is written under --out as validate writes it, beside the
record.json of both windows. The forecast is compared with the record of the window in --after,
read from its line timeline as the replays are. On the same branch it also replays the window in
--before under the rule its daemon ran, the window in --after under the rule its daemon ran, and,
the forecast's control, the window in --after under the rule the daemon of --before ran.

It compares rates per hour, averages and shares, never totals, since the two windows differ in
length and in work, and each passes within twice its gap: R5's gap, or, for the daily maxima of
each count of issues, the loops of the redefined causes together and the wait of urgent work of P0
and of P1, the replay's own error on the measure, the larger of the two windows' errors, each
window replayed under its own rule against its own record. A send-back cause the change of the
workflow redefines (red checks or a conflict, a conflict, a send-back by the architect or the
merger) is scored only inside the total of all send-back loops and of those causes together; each
cause both releases define the same way (a review that asks for changes, a failed test, a drop
from the merge queue, a worker's own move back) is scored apart. The change moves a number where
the forecast stands more than twice its gap from the window before replayed under its own rule;
where the control too stands within twice its gap of the record after the change on such a
number, the forecast says that it cannot tell the two rules apart on it. It names the workflow
commits between the two windows' daemons that the forecast carries beside the rule. A run --out
holds already, a validate's or an earlier forecast's, is not run again where this binary made it,
and is refused where another did; the forecast names each part it found whole. --token-limit
gives every run the project's token limit (legion replay run --token-limit), and a run --out holds
already that ran with another one, or with none, is refused.

With --from-runs nothing runs: the forecast is read from the directories of runs a forecast or a
compare wrote, --before and --after naming the windows by their directories' names and --rule
defaulting to the rule the daemon of the window after ran. Each run keeps the token limit it ran
with, in its run.json, so --token-limit does not apply.`

// newReplayForecastCommand is `legion replay forecast` (replay.RunForecast, replay.Runs.Forecast).
func newReplayForecastCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forecast --before <dir> --after <dir> --rule <name> --pg-dsn <dsn> --out <dir> [--parallel <n>] [--keep-logs] [--token-limit <tokens a minute>] | --from-runs <dir>... --before <name> --after <name> [--rule <name>]",
		Short: "replay the window before a recorded change of the rule under the rule after it, and compare it with the window after",
		Long:  replayForecastLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	before := flags.String("before", "", "the directory of the window before the change; with --from-runs, its name (required)")
	after := flags.String("after", "", "the directory of the window after the change; with --from-runs, its name (required)")
	ruleName := flags.String("rule", "", "the rule after the change, as legion replay rules names it")
	dsn := flags.String("pg-dsn", "", "a Postgres server each run makes its own database on")
	out := flags.String("out", "", "the directory of runs every run is written under")
	parallel := flags.Int("parallel", 1, "how many runs run at once")
	keepLogs := flags.Bool("keep-logs", false, "keep each run's log.jsonl")
	fromRuns := flags.StringArray("from-runs", nil, "a directory of runs to read the forecast from, running nothing; repeat for more")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	asJSON := flags.Bool("json", false, "print the forecast as JSON")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		if missing := missingFlag(cmd, "before", "after"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		var forecast replay.ForecastResult
		if len(*fromRuns) > 0 {
			if set := changedFlag(cmd, "pg-dsn", "out", "parallel", "keep-logs", "token-limit"); set != "" {
				return refuse(cmd, "--from-runs reads runs and makes none, so --%s does not apply", set)
			}
			read, err := replay.ReadRuns(*fromRuns...)
			if err != nil {
				fmt.Fprintf(stderr, "legion replay forecast: %v\n", err)
				return 1
			}
			rule := *ruleName
			if rule == "" {
				if rule, err = read.Rule(*after); err != nil {
					fmt.Fprintf(stderr, "legion replay forecast: %v\n", err)
					return 1
				}
			}
			if forecast, err = read.Forecast(*before, *after, rule); err != nil {
				fmt.Fprintf(stderr, "legion replay forecast: %v\n", err)
				return 1
			}
		} else {
			if missing := missingFlag(cmd, "rule", "pg-dsn", "out"); missing != "" {
				return refuse(cmd, "--%s is required", missing)
			}
			rule, code := namedSlotRule(cmd, *ruleName)
			if code != 0 {
				return code
			}
			if *parallel < 1 {
				return refuseParallel(cmd, *parallel)
			}
			if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
				return code
			}
			var err error
			forecast, err = replay.RunForecast(ctx, replay.ForecastConfig{Before: *before, After: *after, Rule: rule, PGDSN: *dsn,
				Parallel: *parallel, Out: *out, KeepLogs: *keepLogs, Progress: replayProgress(stderr, "forecast"), BuildCommit: binaryCommit(),
				TokenLimit: *tokenLimit})
			if err != nil {
				fmt.Fprintf(stderr, "legion replay forecast: %v\n", err)
				return 1
			}
		}
		if *asJSON {
			return printReplayJSON(stdout, forecast)
		}
		if err := forecast.Write(stdout, 1); err != nil {
			return 1
		}
		return 0
	})
	return cmd
}

// replayCompareLong is `legion replay compare --help`'s description.
const replayCompareLong = `Compare the slot rules on two recorded windows, and write the result as one Markdown document
and each issue's times under each rule in a second file beside it.

The windows are given window before the change of the rule first: --window twice. The rules are
each --rule, every rule the repository keeps where none is given, with main and the rule each
window's daemon ran added where they are left out. Under each rule, each window is replayed on
every branch the tool offers (the main branch and each change of one knob from it, each at the
slowdown factors 1, 1.2 and 1.6), with 5 seeds for a branch that draws and the seed 1 alone for
one that draws nothing, and on the main branch at each slowdown factor from 1 to 2 by 0.1 for the
break-even search. Up to --parallel runs run at once, each in a database of its own on the server
--pg-dsn names, each written under --runs as validate writes it; a run whose files are there
already is not run again, and each run's log is deleted once read unless --keep-logs. The
document is then written to --out from the runs, and the issues file to --out's path with its
extension replaced by -issues.md: --out comparison.md writes comparison-issues.md beside it.
--token-limit gives every run the project's token limit (legion replay run --token-limit), and a
run --runs holds already that ran with another one, or with none, is refused.

The work splits by window and rule: --only-window <name> and --only-rule <name> run that part
alone into --runs and write no document, so each pod of an indexed Job can run one part. Then
--from-runs <dir>... --out <file> writes the document and its issues file from the directories
the parts wrote, running nothing; they are the same files a single process writes. Each run keeps
the token limit it ran with, in its run.json, so --token-limit does not apply there.

The document names the build commit of the runs it read, the commit of the legion binary that
made them, with how many runs each made. It gives each window's scorecard read from its record;
for each rule with a grace, how many of the waits on another issue that gave a slot back in each
window's record the grace keeps the slot through, of how many, and the slot-hours it keeps; each
window's validation under its own rule, with which branches give no comparison; the forecast
across the change, which replays the window before under the rule of the window after, with the
workflow commits it carries too; which window measures how a rule changes conflicts; the value of
every measure for every rule, window and branch, read from the snapshot timeline, with the lowest
and the highest seed; the gap of each measure between the snapshot and the line timelines; each
rule's break-even slowdown factor against main; the rule it names best, or that none is, with
each rule's trade and why each rule the spec does not name was added; and the assumptions and the
scenario each run used. The document names the issues file, which gives, for each window and each
rule, each issue that held a slot in the record or on the main branch of the rule's replay: its
first slot and its done in the record and in the replay, each replayed time the median over the
main branch's seeds, "no slot" or "not done" where half the seeds or more give none.`

// newReplayCompareCommand is `legion replay compare` (replay.RunCompare, replay.Runs.Comparison).
func newReplayCompareCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare --window <dir> --window <dir> [--rule <name>]... --pg-dsn <dsn> --runs <dir> (--out <file> | --only-window <name> --only-rule <name>) [--parallel <n>] [--keep-logs] [--token-limit <tokens a minute>] | --from-runs <dir>... --out <file>",
		Short: "compare the slot rules on two recorded windows over every branch, and write the result as one document",
		Long:  replayCompareLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windows := flags.StringArray("window", nil, "the directory of a recorded window, the window before the change first; give two")
	ruleNames := flags.StringArray("rule", nil, "a rule to compare, as legion replay rules names it; repeat for more (every rule where none is given)")
	dsn := flags.String("pg-dsn", "", "a Postgres server each run makes its own database on")
	runsDir := flags.String("runs", "", "the directory of runs every run is written under")
	out := flags.String("out", "", "the file the document is written to; the issues file goes beside it, its name ending -issues.md")
	parallel := flags.Int("parallel", 1, "how many runs run at once")
	onlyWindow := flags.String("only-window", "", "run the part of this window alone, by its directory's name, and write no document")
	onlyRule := flags.String("only-rule", "", "run the part of this rule alone, and write no document")
	keepLogs := flags.Bool("keep-logs", false, "keep each run's log.jsonl")
	fromRuns := flags.StringArray("from-runs", nil, "a directory of runs to write the document from, running nothing; repeat for more")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		part := *onlyWindow != "" || *onlyRule != ""
		dirs := *fromRuns
		if len(dirs) > 0 {
			if set := changedFlag(cmd, "window", "rule", "pg-dsn", "runs", "parallel", "only-window", "only-rule", "keep-logs", "token-limit"); set != "" {
				return refuse(cmd, "--from-runs writes the document from runs and makes none, so --%s does not apply", set)
			}
			if missing := missingFlag(cmd, "out"); missing != "" {
				return refuse(cmd, "--%s is required", missing)
			}
		} else {
			if missing := missingFlag(cmd, "window", "pg-dsn", "runs"); missing != "" {
				return refuse(cmd, "--%s is required", missing)
			}
			if len(*windows) != 2 {
				return refuse(cmd, "--window is given %d times; a comparison reads two windows, the window before the change first", len(*windows))
			}
			switch {
			case part && cmd.Flags().Changed("out"):
				return refuse(cmd, "a part (--only-window, --only-rule) writes no document, so --out does not apply; write it with --from-runs")
			case !part && !cmd.Flags().Changed("out"):
				return refuse(cmd, "--out is required")
			case *parallel < 1:
				return refuseParallel(cmd, *parallel)
			}
			if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
				return code
			}
			rules := make([]record.SlotRule, 0, len(*ruleNames))
			for _, name := range *ruleNames {
				rule, code := namedSlotRule(cmd, name)
				if code != 0 {
					return code
				}
				rules = append(rules, rule)
			}
			if len(rules) == 0 {
				rules = record.SlotRules()
			}
			if err := replay.RunCompare(ctx, replay.CompareConfig{Windows: *windows, Rules: rules, PGDSN: *dsn, Parallel: *parallel, Runs: *runsDir,
				OnlyWindow: *onlyWindow, OnlyRule: *onlyRule, KeepLogs: *keepLogs, Progress: replayProgress(stderr, "compare"),
				BuildCommit: binaryCommit(), TokenLimit: *tokenLimit}); err != nil {
				fmt.Fprintf(stderr, "legion replay compare: %v\n", err)
				return 1
			}
			if part {
				fmt.Fprintf(stdout, "legion replay compare: ran the part into %s; write the document with --from-runs\n", *runsDir)
				return 0
			}
			dirs = []string{*runsDir}
		}
		read, err := replay.ReadRuns(dirs...)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay compare: %v\n", err)
			return 1
		}
		issuesFile := replay.IssuesFile(*out)
		document, issues, err := read.Comparison(issuesFile)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay compare: %v\n", err)
			return 1
		}
		if err := os.WriteFile(*out, []byte(document), 0o644); err != nil {
			fmt.Fprintf(stderr, "legion replay compare: write the document: %v\n", err)
			return 1
		}
		if err := os.WriteFile(issuesFile, []byte(issues), 0o644); err != nil {
			fmt.Fprintf(stderr, "legion replay compare: write the issues file: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion replay compare: wrote %s and %s from the runs in %s\n", *out, issuesFile, strings.Join(dirs, ", "))
		return 0
	})
	return cmd
}

// replayLimitsLong is `legion replay limits --help`'s description.
const replayLimitsLong = `Plan the limit test of R11 and, given the directories of its runs, name each signal's limit and
print its table. It runs no replay: each entry of the plan is one run of legion replay run, which an
indexed Job runs one to an index, and the runs are read back from --runs.

The list of limits of each signal is its 50th, 75th, 90th, 95th and 99th percentile over --window-one's
record, read at each minute where the window records the signal, and off; a percentile outside the
signal's range (the range legion replay run --limit checks) is left out of the list with the reason, never
clamped into it. The provider's headroom and its refusals each have a list and a sweep of their own, and
the table groups them under the provider. A signal that a load model gives (the provider's headroom and
refusals, the kills, the merge queue's wait) has steps only where its model passed R11's tests, read from
the two outputs of legion replay models --against --json: --models, window one against the other record,
and --models-reverse, the other record against window one. The model must fail the daily test on neither
record, pass it on at least one, and pass the test across the two records in both outputs. Where the window
records no token limit of the project's own the tokens model has no daily test, and it gives the provider's
limits only under --token-limit, the project's token limit, which every run of the plan takes (legion replay
run --token-limit, assumption 49). The idle share and the approvals have no model and always have steps.

Step A runs each signal alone at each limit, and off, on the main branch of both windows. Step B runs, for
each signal, the limit with the most issues finished that no other measure of the owner's three costs or of
the wait of urgent work is worse at than under today's rule on step A's branches, and the next tighter limit,
on every branch of both windows that passes R5, and, where the first is worse on one, the next tighter limit
again, until one holds or the list ends; the idle share's named limit is also run at d of 10 minutes and of 4
hours, and printed beside the hour's. Step C runs signals and signals-backoff with every named limit on every
branch of both windows. Each step runs at the recorded cap and at --ceiling. --baseline names the directories
of runs of validate and compare that hold both windows' records and their runs of main and of each window's
own rule on every branch, which say what a limit is worse than and which branches pass R5.

Given --runs, the table gives each limit's measures on window one's main branch, the signal's own reading
there, the most issues the runs held in flight, the limit named for each signal at each ceiling or that none
holds, and what the limit rests on: each tested day of the model with its recorded and its modelled value, the
ratio across the two records, and for the provider the token limit, the span of each window's provider record
and the refused minutes the model gives under the token limit that the record cannot confirm. The range of work
in flight that the windows record runs from the lowest day's average of issues in flight in the models' days to
the larger of the two windows' scorecard maxima; a named limit whose runs hold more issues in flight than that
is an extrapolation, and the table says so. --plan writes the entries still to run as a JSON list to the file
named, each with the arguments of legion replay run that make it but for --pg-dsn; plan again once they have
run, since step B waits for the runs of step A and step C for those of step B. --json prints the whole result
as JSON.`

// newReplayLimitsCommand is `legion replay limits` (replay.ReadLimitsInput, replay.PlanLimits): the
// limit test of R11.
func newReplayLimitsCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "limits --window-one <dir> --window-two <dir> --models <file> --models-reverse <file> --baseline <dir>... --ceiling <n> [--runs <dir>] [--plan <file>] [--token-limit <tokens a minute>] [--json]",
		Short: "plan R11's limit test, and name each signal's limit from the runs of its steps",
		Long:  replayLimitsLong,
		Args:  noArguments,
	}
	flags := cmd.Flags()
	windowOne := flags.String("window-one", "", "the directory of window one, whose record the lists of limits are read from (required)")
	windowTwo := flags.String("window-two", "", "the directory of window two (required)")
	models := flags.String("models", "", "legion replay models --against --json of window one against the other record (required)")
	reverse := flags.String("models-reverse", "", "the same of the other record against window one (required)")
	baseline := flags.StringArray("baseline", nil, "a directory of runs of validate or compare to judge the limits against; repeat for more (required)")
	runsDir := flags.String("runs", "", "the directory the sweep's runs are written under; the plan marks each run as run or not by what it holds")
	plan := flags.String("plan", "", "write the entries still to run, as a JSON list, to this file")
	tokenLimit := flags.Int64("token-limit", 0, tokenLimitUsage)
	ceiling := flags.Int("ceiling", 0, "the second ceiling, a cap above zero, beside the recorded cap (required)")
	asJSON := flags.Bool("json", false, "print the result as JSON")
	cmd.RunE = runs(func(context.Context, []string) int {
		if missing := missingFlag(cmd, "window-one", "window-two", "models", "models-reverse", "baseline", "ceiling"); missing != "" {
			return refuse(cmd, "--%s is required", missing)
		}
		if *ceiling <= 0 {
			return refuse(cmd, "--ceiling %d: the second ceiling is a cap above zero, beside the recorded cap", *ceiling)
		}
		if code := refuseTokenLimit(cmd, *tokenLimit); code != 0 {
			return code
		}
		if *plan != "" && *runsDir == "" {
			return refuse(cmd, "--plan lists the arguments of each run under --runs, so --runs is required with it")
		}
		in, err := replay.ReadLimitsInput(replay.LimitsConfig{WindowOne: *windowOne, WindowTwo: *windowTwo, Models: *models, ModelsReverse: *reverse,
			Baseline: *baseline, Runs: *runsDir, TokenLimit: *tokenLimit, Ceiling: *ceiling})
		if err != nil {
			fmt.Fprintf(stderr, "legion replay limits: %v\n", err)
			return 1
		}
		result, err := replay.PlanLimits(in)
		if err != nil {
			fmt.Fprintf(stderr, "legion replay limits: %v\n", err)
			return 1
		}
		if *plan != "" {
			pending := result.Pending()
			for i := range pending {
				pending[i].Index = i
			}
			data, err := json.MarshalIndent(pending, "", "  ")
			if err == nil {
				err = os.WriteFile(*plan, append(data, '\n'), 0o644)
			}
			if err != nil {
				fmt.Fprintf(stderr, "legion replay limits: write the plan: %v\n", err)
				return 1
			}
		}
		if *asJSON {
			return printReplayJSON(stdout, result)
		}
		if err := result.Write(stdout); err != nil {
			fmt.Fprintf(stderr, "legion replay limits: %v\n", err)
			return 1
		}
		if *plan != "" {
			fmt.Fprintf(stdout, "\nlegion replay limits: wrote the %d entries still to run to %s\n", len(result.Pending()), *plan)
		}
		return 0
	})
	return cmd
}

// replayProgress is what validate, forecast and compare tell of each run as it ends, on stderr: the
// run, whether it was there already, and what its run.json counted, so a pilot can size a Job and
// read how many issues each run admitted with no work, and the slot-hours they held.
func replayProgress(stderr io.Writer, command string) func(replay.RunDone) {
	return func(done replay.RunDone) {
		if done.Kept {
			fmt.Fprintf(stderr, "legion replay %s: %d of %d: %s under %s on %s, seed %d, was already run: %d issues admitted with no work holding %.1f slot-hours\n",
				command, done.Ended, done.Of, done.Window, done.Rule, done.Branch.Label(), done.Seed, done.NoWorkAdmissions, done.NoWorkSlotHours)
			return
		}
		fmt.Fprintf(stderr, "legion replay %s: %d of %d: ran %s under %s on %s, seed %d: %d facts, %d passes, %.1f s, peak memory %d MiB, "+
			"%d issues admitted with no work holding %.1f slot-hours\n",
			command, done.Ended, done.Of, done.Window, done.Rule, done.Branch.Label(), done.Seed, done.Facts, done.Passes, done.WallSeconds,
			done.PeakMemoryBytes>>20, done.NoWorkAdmissions, done.NoWorkSlotHours)
	}
}

// changedFlag is the first of names cmd was given, "" when it was given none.
func changedFlag(cmd *cobra.Command, names ...string) string {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return name
		}
	}
	return ""
}

// tokenLimitUsage is the help of --token-limit, which each command that runs or reads a signal
// rule's run takes (replay.RunConfig.TokenLimit).
const tokenLimitUsage = "the project's token limit in uncached input tokens a minute, read in each minute whose provider record holds no limit of the project's own (assumption 49); a whole number above zero"

// refuseTokenLimit refuses a --token-limit that cmd was given at zero or below, naming it, and
// answers 0 where it was given none or one above zero.
func refuseTokenLimit(cmd *cobra.Command, limit int64) int {
	if cmd.Flags().Changed("token-limit") && limit <= 0 {
		return refuse(cmd, "--token-limit %d: the project's token limit is a whole number of tokens a minute above zero", limit)
	}
	return 0
}

// printReplayJSON prints v as indented JSON, answering the exit status.
func printReplayJSON(stdout io.Writer, v any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return 1
	}
	return 0
}

// newReplayRulesCommand is `legion replay rules`: every slot rule a replay can run
// (record.SlotRules), the daemon's own first, with its commit and each of its choices.
func newReplayRulesCommand(stdout, _ io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules [--json]",
		Short: "print every slot rule a replay can run, with its commit and each of its choices",
		Args:  noArguments,
	}
	asJSON := cmd.Flags().Bool("json", false, "print the rules as JSON")
	cmd.RunE = runs(func(context.Context, []string) int {
		rules := record.SlotRules()
		if *asJSON {
			views := make([]slotRuleView, 0, len(rules))
			for _, rule := range rules {
				views = append(views, slotRuleViewOf(rule))
			}
			return printReplayJSON(stdout, views)
		}
		for i, rule := range rules {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			keeps := make([]string, 0, len(rule.KeepsSlotIn))
			for _, at := range rule.KeepsSlotIn {
				keeps = append(keeps, string(at))
			}
			fmt.Fprintf(stdout, "%s (commit %s)\n", rule.Name, rule.Commit)
			fmt.Fprintf(stdout, "  keeps a slot in: %s\n", strings.Join(keeps, ", "))
			fmt.Fprintf(stdout, "  a link wait gives the slot back: %t\n", rule.LinkWaitGivesBack)
			fmt.Fprintf(stdout, "  a children wait gives the slot back: %t\n", rule.ChildrenWaitGivesBack)
			fmt.Fprintf(stdout, "  a wait gives the slot back after: %s\n", rule.WaitGrace)
			fmt.Fprintf(stdout, "  a wait stops the phase under way: %t\n", rule.WaitStopsPhase)
			fmt.Fprintf(stdout, "  the line's order: %s\n", rule.Order)
			fmt.Fprintf(stdout, "  the line stands by priority first: %t\n", rule.PriorityFirst)
			fmt.Fprintf(stdout, "  a start takes: %s\n", rule.Take)
			fmt.Fprintf(stdout, "  a conflicting head: %s\n", rule.Conflict)
			fmt.Fprintf(stdout, "  the slot goes back at the ready mark: %t\n", rule.ReleaseAtReady)
			fmt.Fprintf(stdout, "  a P0 starts outside the cap: %t\n", rule.StartsNow)
			fmt.Fprintf(stdout, "  an idle wait on children or a person gives the slot back after: %s\n", rule.IdleReturn)
			if rule.Signals != signals.Off {
				fmt.Fprintf(stdout, "  admits below P0 by signals: %s\n", rule.Signals)
			}
		}
		return 0
	})
	return cmd
}

// slotRuleView is a record.SlotRule as `legion replay rules --json` prints it: the rule's fields
// under JSON names, since the rule itself is no wire type, and its grace and its idle return as Go
// durations ("5m0s"), as the rule's text prints them.
type slotRuleView struct {
	Name                  string              `json:"name"`
	Commit                string              `json:"commit"`
	KeepsSlotIn           []phase.Phase       `json:"keepsSlotIn"`
	LinkWaitGivesBack     bool                `json:"linkWaitGivesBack"`
	ChildrenWaitGivesBack bool                `json:"childrenWaitGivesBack"`
	WaitGrace             string              `json:"waitGrace"`
	WaitStopsPhase        bool                `json:"waitStopsPhase"`
	Order                 record.LineOrder    `json:"order"`
	PriorityFirst         bool                `json:"priorityFirst"`
	Take                  record.StartTake    `json:"take"`
	Conflict              record.ConflictPath `json:"conflict"`
	ReleaseAtReady        bool                `json:"releaseAtReady"`
	StartsNow             bool                `json:"startsNow"`
	IdleReturn            string              `json:"idleReturn"`
	Signals               signals.Mode        `json:"signals,omitempty"`
}

// slotRuleViewOf is rule as `legion replay rules --json` prints it (slotRuleView).
func slotRuleViewOf(rule record.SlotRule) slotRuleView {
	return slotRuleView{Name: rule.Name, Commit: rule.Commit, KeepsSlotIn: rule.KeepsSlotIn, LinkWaitGivesBack: rule.LinkWaitGivesBack,
		ChildrenWaitGivesBack: rule.ChildrenWaitGivesBack, WaitGrace: rule.WaitGrace.String(), WaitStopsPhase: rule.WaitStopsPhase,
		Order: rule.Order, PriorityFirst: rule.PriorityFirst, Take: rule.Take, Conflict: rule.Conflict, ReleaseAtReady: rule.ReleaseAtReady,
		StartsNow: rule.StartsNow, Signals: rule.Signals,
		IdleReturn: rule.IdleReturn.String()}
}

// missingFlag is the first of names cmd was not given, "" when each was.
func missingFlag(cmd *cobra.Command, names ...string) string {
	for _, name := range names {
		if !cmd.Flags().Changed(name) {
			return name
		}
	}
	return ""
}

// namedSlotRule is the rule record.SlotRules keeps under name, refusing a name it keeps none
// under as a usage error, in replaySpan's value-and-exit-code way.
func namedSlotRule(cmd *cobra.Command, name string) (record.SlotRule, int) {
	rule, ok := record.SlotRuleNamed(name)
	if !ok {
		return rule, refuse(cmd, "--rule %q names no slot rule; `legion replay rules` lists them", name)
	}
	return rule, 0
}

// refuseParallel refuses a --parallel of parallel, below one, as a usage error.
func refuseParallel(cmd *cobra.Command, parallel int) int {
	return refuse(cmd, "--parallel %d: at least one run runs at a time", parallel)
}

// replaySpan reads --from and --to as RFC 3339 times, refusing either that is not one, and a
// span that is empty, as a usage error.
func replaySpan(cmd *cobra.Command, from, to string) ([2]time.Time, int) {
	var span [2]time.Time
	for i, pair := range [][2]string{{"from", from}, {"to", to}} {
		at, err := time.Parse(time.RFC3339, pair[1])
		if err != nil {
			return span, refuse(cmd, "--%s %q is no RFC 3339 time", pair[0], pair[1])
		}
		span[i] = at
	}
	if !span[0].Before(span[1]) {
		return span, refuse(cmd, "--from %s is not before --to %s", from, to)
	}
	return span, 0
}

// designSteps is steps as --gate-steps takes them: comma-separated, or off for none.
func designSteps(steps []record.DesignStep) string {
	if len(steps) == 0 {
		return "off"
	}
	return strings.Join(record.StepNames(steps), ",")
}

// parseDesignSteps reads --gate-steps as gates.design reads its value: off for no step,
// root-issues for the single step spec, and otherwise design steps, each once.
func parseDesignSteps(text string) ([]record.DesignStep, error) {
	if gate := config.DesignGate(strings.TrimSpace(text)); gate.Valid() {
		return gate.Steps(), nil
	}
	var steps []record.DesignStep
	for _, word := range strings.Split(text, ",") {
		step := record.DesignStep(strings.TrimSpace(word))
		if !step.Valid() {
			return nil, fmt.Errorf("%q is no design step (%s), and the value is not off or root-issues", step, record.StepWords())
		}
		if slices.Contains(steps, step) {
			return nil, fmt.Errorf("%q is named twice", step)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// parsePart reads a --part, <from>,<to>,<release>,<commit>,<rule>.
func parsePart(text string) (replay.Part, error) {
	fields := strings.Split(text, ",")
	if len(fields) != 5 {
		return replay.Part{}, errors.New("a part is <from>,<to>,<release>,<commit>,<rule>")
	}
	var span [2]time.Time
	for i := range span {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[i]))
		if err != nil {
			return replay.Part{}, fmt.Errorf("%q is no RFC 3339 time", fields[i])
		}
		span[i] = at
	}
	return replay.Part{From: span[0], To: span[1], Release: strings.TrimSpace(fields[2]), Commit: strings.TrimSpace(fields[3]),
		Rule: strings.TrimSpace(fields[4])}, nil
}

// binaryCommit is the commit this binary was built from: the one the build linked in
// (revision), else the one Go's build recorded from the checkout; "" for neither.
func binaryCommit() string {
	if revision != "" {
		return revision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

// dispatchToken is the Dispatch token a collect reads with, as the dispatch command finds one:
// the file --dispatch-token-file names, else the file DISPATCH_TOKEN_FILE names (one that is set
// and cannot be read, or is blank, is the answer, never a fall back), else DISPATCH_TOKEN. Each
// file's contents are trimmed; the token is never printed.
func dispatchToken(file string) (string, error) {
	if file != "" {
		return config.ReadSecretPointer("--dispatch-token-file", file)
	}
	if file := os.Getenv("DISPATCH_TOKEN_FILE"); file != "" {
		return config.ReadSecretPointer("DISPATCH_TOKEN_FILE", file)
	}
	if token := strings.TrimSpace(os.Getenv("DISPATCH_TOKEN")); token != "" {
		return token, nil
	}
	return "", errors.New("no Dispatch token: name its file with --dispatch-token-file, or set DISPATCH_TOKEN_FILE or DISPATCH_TOKEN")
}
