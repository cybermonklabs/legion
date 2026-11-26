package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// renameFrom and renameTo are INFRA's pair, the one every test of the rename pass configures.
const renameFrom, renameTo = "sjawhar/legion", "cybermonklabs/legion"

// renameSeed is one issue a test of the rename pass records, a done root on repo, with a merged
// pull request on branch (none where branch is ""), a closed production check and a repository
// note, each on repo, where check and note say.
type renameSeed struct {
	key, repo, branch string
	check, note       bool
}

func seedRenameRecords(t *testing.T, pool *store.Pool, project string, seeds ...renameSeed) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	opened := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	closed := opened.Add(time.Hour)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, seed := range seeds {
			if err := records.PutIssue(ctx, tx, record.Issue{Key: seed.key, Project: project, Tree: seed.key, Title: seed.key,
				Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", HandedOver: true, Rank: "A",
				LastDispatchSeq: 1, Repo: seed.repo, DefaultBranch: "main", GitHubRepo: seed.repo}); err != nil {
				return err
			}
			if seed.branch != "" {
				if err := records.PutPullRequest(ctx, tx, record.PullRequest{Issue: seed.key, Repo: seed.repo, Number: 7,
					Branch: seed.branch, HeadSHA: "head", State: record.PullRequestMerged}); err != nil {
					return err
				}
			}
			if seed.check {
				if err := records.PutProductionCheck(ctx, tx, record.ProductionCheck{Issue: seed.key, Project: project, Tree: seed.key,
					Repo: seed.repo, Number: 7, OpenedAt: opened, BoundAt: closed, Role: claim.RoleImplementer, Looks: 1,
					ClosedAt: &closed, Outcome: record.CheckRecorded}); err != nil {
					return err
				}
			}
			if seed.note {
				if err := records.PutRepoNote(ctx, tx, record.RepoNote{Issue: seed.key, ResolvedRepo: seed.repo, Note: "noted", PostedAt: opened}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the issues of the rename pass: %v", err)
	}
}

// storedRepositoryNames is every stored column of key's rows that holds a repository name, by
// table and column, a row the issue has none of left out.
func storedRepositoryNames(t *testing.T, pool *store.Pool, key string) map[string]string {
	t.Helper()
	names := map[string]string{}
	for _, read := range []struct{ column, sql string }{
		{"issues.repo", "select repo from issues where key = $1"},
		{"issues.github_repo", "select github_repo from issues where key = $1"},
		{"pull_requests.repo", "select repo from pull_requests where issue = $1"},
		{"production_checks.repo", "select repo from production_checks where issue = $1"},
		{"repo_notes.resolved_repo", "select resolved_repo from repo_notes where issue = $1"},
		{"cleared_pull_requests.repo", "select repo from cleared_pull_requests where issue = $1"},
	} {
		var name string
		switch err := pool.QueryRow(context.Background(), read.sql, key).Scan(&name); err {
		case nil:
			names[read.column] = name
		case pgx.ErrNoRows:
		default:
			t.Fatalf("read %s of %s: %v", read.column, key, err)
		}
	}
	return names
}

// namedAll answers whether every name of names is repo, and that there is at least one.
func namedAll(names map[string]string, repo string) bool {
	for _, name := range names {
		if name != repo {
			return false
		}
	}
	return len(names) > 0
}

// renameLine is one line the rename pass logs, as the daemon's JSON log writes it.
type renameLine struct {
	Level   string         `json:"level"`
	Msg     string         `json:"msg"`
	Table   string         `json:"table"`
	Written int            `json:"written"`
	Failed  int            `json:"failed"`
	Left    map[string]int `json:"left"`
	Issue   string         `json:"issue"`
	From    string         `json:"from"`
	To      string         `json:"to"`
	Kept    string         `json:"kept"`
	Branch  string         `json:"branch"`
	Reason  string         `json:"reason"`
	Error   string         `json:"error"`
}

// renameLines is each line of logged whose message is msg.
func renameLines(t *testing.T, logged, msg string) []renameLine {
	t.Helper()
	var lines []renameLine
	for text := range strings.SplitSeq(logged, "\n") {
		if !strings.Contains(text, msg) {
			continue
		}
		var line renameLine
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("decode the log line %s: %v", text, err)
		}
		if line.Msg == msg {
			lines = append(lines, line)
		}
	}
	return lines
}

// renameTableTally is one table's line: what the pass wrote, what failed and what is left on the
// old name.
type renameTableTally struct {
	written, failed, left int
}

// renameTables is each table's line in logged, by table, the old name's count left.
func renameTables(t *testing.T, logged string) map[string]renameTableTally {
	t.Helper()
	tables := map[string]renameTableTally{}
	for _, line := range renameLines(t, logged, renameTableLine) {
		tables[line.Table] = renameTableTally{written: line.Written, failed: line.Failed, left: line.Left[renameFrom]}
	}
	return tables
}

// renameBoot boots the workflow daemon of cfg logging to logged over a GitHub after the transfer of
// INFRA's pair: both role Apps answer not installed on sjawhar/legion and installed on every other
// repository, and GitHub spells the pair's `to` cybermonklabs/legion, at main. It returns once the
// daemon answers /healthz, which it does only after the boot's rename pass has run (run's
// renameAtBoot), so its table lines are logged by then.
func renameBoot(t *testing.T, cfg config.Config, logged *syncBuffer) *daemon {
	t.Helper()
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{missing: map[string]bool{"implement " + renameFrom: true, "review " + renameFrom: true}}
	o.githubAPI = newNamedGitHub(t, map[string]string{renameFrom: "301 " + renameTo, renameTo: renameTo + "@main"}).url
	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logged, nil)))
	if got := renameTables(t, logged.String()); len(got) != len(record.RenameTables) {
		t.Fatalf("the boot's rename pass logged the tables %+v by /healthz, want each of %v", got, record.RenameTables)
	}
	return d
}

// transferredRepositories is the repository state of a daemon that configures INFRA's pair
// (transferredLegion) once GitHub has answered for its transfer, as the refresher's read of the
// pairs (readPairs) and a preparation of the pair's `to` leave it: both role Apps installed on the
// `to`, read as GitHub spells it, and neither on the `from`. A rename pass over it writes the pair
// from the state alone, with no read of GitHub.
func transferredRepositories() *repositories {
	repos := newRepositories("LEGION", transferredLegion)
	repos.fill(ghrepo.MustParse(renameTo))
	answerInstalled(repos, renameFrom, false)
	return repos
}

// A boot of a daemon that configures a rename, over a GitHub that shows the transfer — both role
// Apps installed on the pair's `to` and neither on its `from` — writes the pair's new name, as
// GitHub spells it and not as legion.yaml does (CyberMonkLabs/Legion), on every stored column that
// held the old one, in any spelling: the record's repository and GitHub's spelling, the pull
// request, the production check, the repository note and the pull request a new generation
// cleared, the note of an issue the project never recorded among them. Where the new name would
// give the pull requests' unique key one repository and branch twice, the row under the new name
// stays and the other issue's is deleted, one line naming both issues. Each table's line counts
// the rows written, the rows failed and the rows left on the old name, which is none; an issue on
// another repository is left alone. A second boot writes nothing. Controls: with the pass deleted
// from the boot (run's renameAtBoot), every row is left on the old name; with the configured
// spelling written (renameTransfer answering the pair's own `to`), every row written holds
// CyberMonkLabs/Legion, a spelling nothing publishes under.
func TestTheBootWritesEachRenamesNewNameOverItsOldOnce(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.RepositoryRenames = ghrepo.MustRenames(ghrepo.MustRename(renameFrom, "CyberMonkLabs/Legion"))
	p := cfg.Project
	seedRenameRecords(t, pool, p,
		renameSeed{key: p + "-1", repo: renameFrom, branch: "legion/" + p + "-1", check: true, note: true},
		renameSeed{key: p + "-2", repo: renameFrom, branch: "release/shared"},
		renameSeed{key: p + "-3", repo: renameTo, branch: "release/shared"},
		renameSeed{key: p + "-4", repo: "SJAWHAR/Legion"},
		renameSeed{key: p + "-6", repo: "acme/widgets", branch: "legion/" + p + "-6", check: true, note: true},
	)
	if _, err := pool.Exec(context.Background(), `insert into repo_notes (issue, resolved_repo, note, posted_at) values ($1, $2, 'not admitted', now())`,
		p+"-9", renameFrom); err != nil {
		t.Fatal(err)
	}
	for issue, repo := range map[string]string{p + "-1": "SJAWHAR/Legion", p + "-6": "acme/widgets"} {
		if _, err := pool.Exec(context.Background(), `insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
			values ($1, 6, $2, now(), now())`, repo, issue); err != nil {
			t.Fatal(err)
		}
	}

	var logged syncBuffer
	d := renameBoot(t, cfg, &logged)
	stored := map[string]map[string]string{}
	for _, key := range []string{p + "-1", p + "-2", p + "-3", p + "-4", p + "-9"} {
		stored[key] = storedRepositoryNames(t, pool, key)
		if names := stored[key]; !namedAll(names, renameTo) {
			t.Errorf("%s's stored names after the boot = %v, want each on %s, as GitHub spells it", key, names, renameTo)
		}
	}
	if names := stored[p+"-2"]; names["pull_requests.repo"] != "" {
		t.Errorf("%s's pull request = %v, want it deleted: %s-3's holds release/shared on %s", p+"-2", names, p, renameTo)
	}
	if names := storedRepositoryNames(t, pool, p+"-6"); len(names) != 6 || !namedAll(names, "acme/widgets") {
		t.Errorf("%s's stored names = %v, want all six left on acme/widgets", p+"-6", names)
	}
	deleted := renameLines(t, logged.String(), renameDeletedLine)
	if len(deleted) != 1 || deleted[0].Issue != p+"-2" || deleted[0].Kept != p+"-3" || deleted[0].Branch != "release/shared" || deleted[0].Level != "WARN" {
		t.Errorf("deletion lines = %+v, want one WARN line naming %s-2 deleted and %s-3 kept on release/shared", deleted, p, p)
	}
	if left := renameLines(t, logged.String(), renameLeftLine); len(left) != 0 {
		t.Errorf("lines of a pair left = %+v, want none: GitHub shows the transfer", left)
	}
	want := map[string]renameTableTally{"issues": {written: 3}, "pull_requests": {written: 1}, "cleared_pull_requests": {written: 1},
		"production_checks": {written: 1}, "repo_notes": {written: 2}}
	if got := renameTables(t, logged.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("table lines of the first boot = %+v, want %+v", got, want)
	}

	d.stop()
	rebindHeldPorts(t, &cfg)
	var again syncBuffer
	renameBoot(t, cfg, &again)
	want = map[string]renameTableTally{"issues": {}, "pull_requests": {}, "cleared_pull_requests": {}, "production_checks": {}, "repo_notes": {}}
	if got := renameTables(t, again.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("table lines of the second boot = %+v, want nothing written and nothing left", got)
	}
	if lines := renameLines(t, again.String(), renameDeletedLine); len(lines) != 0 {
		t.Errorf("deletion lines of the second boot = %+v, want none", lines)
	}
}

// A write the database refuses is logged with its error, naming the issue, and changes nothing
// else: the issue's rows stay on the old name and count as failed, the pass goes on to the next
// issue and writes it, and the boot comes up. The next boot writes the rows. The refusal is a
// trigger on the issue's pull request row, so the issue's whole write, its record's columns
// included, is undone with it. Control: with the pass deleted from the boot, the next boot writes
// nothing either; with a failed write stopping the boot, the first boot never answers /healthz.
func TestARenameWriteThatFailsIsLoggedAndTheNextBootWritesIt(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.RepositoryRenames = transferredLegion
	p := cfg.Project
	refused, written := p+"-5", p+"-7"
	seedRenameRecords(t, pool, p,
		renameSeed{key: refused, repo: renameFrom, branch: "legion/" + refused},
		renameSeed{key: written, repo: renameFrom},
	)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `create function refuse_the_rename() returns trigger language plpgsql as $$
		begin raise exception 'the test refuses the write of %', old.issue; end $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `create trigger refuse_the_rename before update on pull_requests for each row
		when (old.issue = '`+refused+`') execute function refuse_the_rename()`); err != nil {
		t.Fatal(err)
	}

	var logged syncBuffer
	d := renameBoot(t, cfg, &logged)
	if names := storedRepositoryNames(t, pool, refused); !namedAll(names, renameFrom) {
		t.Errorf("%s's stored names after the refused write = %v, want each left on %s", refused, names, renameFrom)
	}
	if names := storedRepositoryNames(t, pool, written); !namedAll(names, renameTo) {
		t.Errorf("%s's stored names = %v, want the pass to go on and write %s", written, names, renameTo)
	}
	failed := renameLines(t, logged.String(), renameFailedLine)
	if len(failed) != 1 || failed[0].Issue != refused || failed[0].Level != "WARN" || !strings.Contains(failed[0].Error, "the test refuses the write of "+refused) {
		t.Errorf("failure lines = %+v, want one WARN line naming %s and the database's refusal", failed, refused)
	}
	want := map[string]renameTableTally{"issues": {written: 1, failed: 1, left: 1}, "pull_requests": {failed: 1, left: 1},
		"cleared_pull_requests": {}, "production_checks": {}, "repo_notes": {}}
	if got := renameTables(t, logged.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("table lines of the first boot = %+v, want %+v", got, want)
	}

	d.stop()
	if _, err := pool.Exec(ctx, "drop trigger refuse_the_rename on pull_requests"); err != nil {
		t.Fatal(err)
	}
	rebindHeldPorts(t, &cfg)
	var again syncBuffer
	renameBoot(t, cfg, &again)
	if names := storedRepositoryNames(t, pool, refused); !namedAll(names, renameTo) {
		t.Errorf("%s's stored names after the next boot = %v, want each on %s", refused, names, renameTo)
	}
	want = map[string]renameTableTally{"issues": {written: 1}, "pull_requests": {written: 1}, "cleared_pull_requests": {}, "production_checks": {},
		"repo_notes": {}}
	if got := renameTables(t, again.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("table lines of the next boot = %+v, want %+v", got, want)
	}
}

// renamePodRuntime is the idle rig's runtime answering which pod the issue's Sandbox holds, as the
// Kubernetes runtime's IssuePod does: the released pod while the runtime holds the Sandbox.
type renamePodRuntime struct{ *releaseRuntime }

func (r renamePodRuntime) IssuePod(issue string) (string, bool) {
	if _, present := r.IssueSandbox(issue); present {
		return releasedPodUID, true
	}
	return "", true
}

// An issue whose pod is live at boot is left on the old name — a role launched into that pod keeps
// the workspace the pod made — and the line says why; the end of its pod, here its idle release,
// writes the new name and drops the repository kept for it. At the end of the pod the pass reads
// no Sandbox: the teardown's return is the proof the pod is gone, though this runtime still
// answers the Sandbox present. Control: with the live-pod rule taken out (bootKeep keeping
// nothing), the boot writes the new name under the live pod; with the release's pod end removed
// (releaseIssue's endPods), the issue stays on the old name after its pod is gone.
func TestAnIssueWithALivePodKeepsItsNamesUntilItsPodEnds(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	if _, err := rig.pool.Exec(ctx, "update issues set repo = $2, github_repo = $2 where key = $1", rig.issue.Key, renameFrom); err != nil {
		t.Fatal(err)
	}
	var logged syncBuffer
	rig.w.log = slog.New(slog.NewJSONHandler(&logged, nil))
	rig.w.repositories = transferredRepositories()
	rig.w.launched = map[string]ghbranch.Repository{strings.ToLower(rig.issue.Key): {Name: ghrepo.MustParse(renameFrom), DefaultBranch: "main"}}
	rt := renamePodRuntime{rig.rt}
	rig.runner.podEnded = rig.w.podEndsOn(rt)

	rig.w.renameAtBoot(ctx, rt)
	if names := storedRepositoryNames(t, rig.pool, rig.issue.Key); !namedAll(names, renameFrom) {
		t.Fatalf("%s's stored names after the boot = %v, want them left on %s under its live pod", rig.issue.Key, names, renameFrom)
	}
	kept := renameLines(t, logged.String(), renameKeptLine)
	if len(kept) != 1 || kept[0].Issue != rig.issue.Key || !strings.Contains(kept[0].Reason, rig.rt.sandboxName) {
		t.Fatalf("kept lines = %+v, want one naming %s and its Sandbox %s", kept, rig.issue.Key, rig.rt.sandboxName)
	}

	rig.w.releaseIdlePods(ctx)
	rig.drain(t)
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the release's one", suspended)
	}
	if names := storedRepositoryNames(t, rig.pool, rig.issue.Key); !namedAll(names, renameTo) {
		t.Fatalf("%s's stored names after its pod's release = %v, want each on %s", rig.issue.Key, names, renameTo)
	}
	if _, held := rig.w.launched[strings.ToLower(rig.issue.Key)]; held {
		t.Fatalf("the repository kept for %s outlived its pod", rig.issue.Key)
	}
	if got := renameTables(t, logged.String())["issues"]; got != (renameTableTally{written: 1}) {
		t.Fatalf("the pod end's issues line = %+v, want one row written and none left", got)
	}
}

// The next pod after a rename works the new name. An issue's idle
// release reads its workspace while its record names the pair's `from`, finds nothing origin does
// not hold, and the end of that release's pod (outbox.releaseIssue, after its teardown) writes the
// pair's `to` over it. The read of the live records the pass wakes (wakeLive,
// refreshCacheRepositories) then caches the `to`, and the issue's next launch's spec
// (specs.repository, workflowRuntime.launchRepository) names the `to` at the record's default
// branch, which is where the next pod's workspace is made from: the release took the pod only
// because no working copy or bookmark of its workspace held a commit origin does not hold, so no
// such work is left to carry across the rename. Control: with the pass at that hook deleted (releaseIssue's endPods), the next launch's
// spec names the `from`.
func TestTheNextPodAfterARenameWorksTheNewName(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	key := rig.issue.Key
	if _, err := rig.pool.Exec(ctx, "update issues set repo = $2, github_repo = $2 where key = $1", key, renameFrom); err != nil {
		t.Fatal(err)
	}
	rig.w.repositories = transferredRepositories()
	rig.w.liveWake = make(chan struct{}, 1)
	rig.runner.podEnded = rig.w.podEndsOn(renamePodRuntime{rig.rt})
	// Inside the teardown, before the pod end: the release has read the workspace by then, and
	// the record still names the `from`.
	var atTeardown struct {
		repo string
		err  error
	}
	rig.rt.beforeAuthorize = func() {
		atTeardown.err = rig.pool.QueryRow(ctx, "select repo from issues where key = $1", key).Scan(&atTeardown.repo)
	}

	rig.w.releaseIdlePods(ctx)
	rig.drain(t)
	if read, suspended := rig.rt.reads(); suspended != 1 || len(read) != 1 {
		t.Fatalf("Sandbox teardowns = %d after %d workspace reads, want the release's one after its one read", suspended, len(read))
	}
	if atTeardown.err != nil || atTeardown.repo != renameFrom {
		t.Fatalf("at the release's teardown %s's record names %q (%v), want %s", key, atTeardown.repo, atTeardown.err, renameFrom)
	}
	if names := storedRepositoryNames(t, rig.pool, key); !namedAll(names, renameTo) {
		t.Errorf("%s's stored names after its release's pod end = %v, want each on %s", key, names, renameTo)
	}
	select {
	case <-rig.w.liveWake:
	default:
		t.Errorf("the pod end woke no read of the live records, want one: its pass wrote %s's rows", key)
	}
	// The refresher's read of the live records, which that wake runs (refreshLive), over the
	// workflow's own records.
	rig.w.records = rig.issues
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error { return rig.w.refreshCacheRepositories(ctx, tx) }); err != nil {
		t.Fatalf("read the live records again: %v", err)
	}
	if cached := repositoryNames(rig.w.cacheRepositories()); !slices.Equal(cached, []string{renameTo}) {
		t.Errorf("the cache set after the read of the live records = %v, want [%s]", cached, renameTo)
	}

	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	launches := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, appOwners: noAppOwners, repository: rig.w.launchRepository}
	spec, err := launches.SpawnSpec(ctx, rig.machine.Claim())
	if err != nil {
		t.Fatalf("build the next launch of %s: %v", rig.token, err)
	}
	if spec.Repository.String() != renameTo || spec.DefaultBranch != "main" {
		t.Errorf("the next launch's spec works %s at %s, want %s at main: the pod after the rename works the new name",
			spec.Repository, spec.DefaultBranch, renameTo)
	}
}

// An idle release under keep_volume keeps the issue's Sandbox, Suspended, with its kept volume, for
// the issue's next pod (sandbox.Runtime.ReleaseIssue), and that pod mounts the volume with the
// workspace directory under the name the record holds (workspace.Location). So the end of that
// release's pod leaves the issue's stored names on the old name and logs one kept line that names
// the issue and says the runtime keeps its volume. With the new name written, the next pod would
// make a fresh workspace under the new name, and the workspace on the kept volume, with its
// session's files and caches, would stay under the old name. A later pass writes such an issue
// once no kept volume is left: here, once the runtime
// keeps no volume of the issue, as after the teardown of its close (SuspendIssue), the next pod end
// writes the new name, and its issues line counts one row written and none left. Control: the same
// release with no kept volume writes the new name at the pod end and logs no kept line, as an
// ordinary release does (TestAnIssueWithALivePodKeepsItsNamesUntilItsPodEnds); with podEndKeep
// keeping nothing under a runtime that runs a pod per issue, the release that keeps the volume
// writes the new name too; with podEndKeep keeping the issue whatever IssueKeepsVolume answers,
// that later pod end leaves it on the old name.
func TestAReleaseThatKeepsTheVolumeLeavesTheIssueOnItsOldName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kept   bool
		want   string
		issues renameTableTally
	}{
		{name: "a release that keeps the issue's volume", kept: true, want: renameFrom, issues: renameTableTally{left: 1}},
		{name: "a release that keeps no volume", kept: false, want: renameTo, issues: renameTableTally{written: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			key := rig.issue.Key
			if _, err := rig.pool.Exec(ctx, "update issues set repo = $2, github_repo = $2 where key = $1", key, renameFrom); err != nil {
				t.Fatal(err)
			}
			var logged syncBuffer
			rig.w.log = slog.New(slog.NewJSONHandler(&logged, nil))
			rig.w.repositories = transferredRepositories()
			rig.runner.podEnded = rig.w.podEndsOn(renamePodRuntime{rig.rt})
			if tc.kept {
				// The Sandbox and its volume were made an hour ago, at the issue's first pod.
				rig.rt.ScriptKeptVolume(key, time.Now().Add(-time.Hour))
			}

			rig.w.releaseIdlePods(ctx)
			rig.drain(t)
			if _, suspended := rig.rt.reads(); suspended != 1 {
				t.Fatalf("Sandbox teardowns = %d, want the release's one", suspended)
			}
			if names := storedRepositoryNames(t, rig.pool, key); !namedAll(names, tc.want) {
				t.Errorf("%s's stored names after its release's pod end = %v, want each on %s", key, names, tc.want)
			}
			kept := renameLines(t, logged.String(), renameKeptLine)
			switch {
			case tc.kept && (len(kept) != 1 || kept[0].Issue != key || kept[0].Reason != renameKeptVolume):
				t.Errorf("kept lines = %+v, want one naming %s with the reason %q", kept, key, renameKeptVolume)
			case !tc.kept && len(kept) != 0:
				t.Errorf("kept lines = %+v, want none: the runtime keeps no volume of %s", kept, key)
			}
			if got := renameTables(t, logged.String())["issues"]; got != tc.issues {
				t.Errorf("the pod end's issues line = %+v, want %+v", got, tc.issues)
			}
			if tc.kept {
				// The runtime keeps no volume of the issue now, as after its close's teardown, which
				// deletes the Sandbox and its volume: the pass at the next pod end writes the issue.
				rig.rt.ScriptKeptVolume(key, time.Time{})
				rig.w.podEndsOn(renamePodRuntime{rig.rt})(ctx, []string{key})
				if names := storedRepositoryNames(t, rig.pool, key); !namedAll(names, renameTo) {
					t.Errorf("%s's stored names after a pod end with no kept volume left = %v, want each on %s", key, names, renameTo)
				}
				if got := renameTables(t, logged.String())["issues"]; got != (renameTableTally{written: 1}) {
					t.Errorf("the issues line of the pod end with no kept volume left = %+v, want one row written and none left", got)
				}
			}
		})
	}
}

// presenceRuntime answers, by issue, the pod a Kubernetes runtime holds, with no Sandbox of any,
// each on the configured image, and, through the embedded fake, whether the runtime keeps an
// issue's volume for its next pod (fake.Runtime.ScriptKeptVolume).
type presenceRuntime struct {
	*fake.Runtime
	pods   map[string]string
	unread map[string]bool
}

func (r presenceRuntime) IssuePodRunsConfiguredImage(string) bool { return true }

func (r presenceRuntime) IssueSandbox(string) (string, bool) { return "", false }

func (r presenceRuntime) IssuePod(issue string) (string, bool) {
	if r.unread[issue] {
		return "", false
	}
	return r.pods[issue], true
}

// At boot the pass keeps each issue the runtime answers no Sandbox of but may still hold the
// workspace of: one it cannot say it holds no pod of (its Sandbox store cannot be read for it),
// one whose pod it holds with no Sandbox left, and one whose volume it keeps for the issue's next
// pod though it holds no pod of it. That last is how the Kubernetes runtime answers the Sandbox an
// idle release kept under keep_volume (sandbox.Runtime.IssueSandbox), and the next pod mounts
// that volume with the workspace directory under the old name. Each kept line says why. Control:
// the issue the runtime holds nothing of is written; with the live-pod rule taken out, the three
// kept issues are written too; with bootKeep not asking the runtime about the volume, LEGION-4 is
// written.
func TestABootKeepsEachIssueWhoseWorkspaceTheRuntimeMayStillHold(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION",
		renameSeed{key: "LEGION-1", repo: renameFrom}, renameSeed{key: "LEGION-2", repo: renameFrom}, renameSeed{key: "LEGION-3", repo: renameFrom},
		renameSeed{key: "LEGION-4", repo: renameFrom})
	var logged syncBuffer
	w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: transferredRepositories(),
		log: slog.New(slog.NewJSONHandler(&logged, nil))}
	rt := presenceRuntime{Runtime: fake.NewRuntime(), unread: map[string]bool{"LEGION-1": true},
		pods: map[string]string{"LEGION-2": "pod-of-legion-2"}}
	// LEGION-4's Sandbox and its volume were made an hour ago, at its first pod.
	rt.ScriptKeptVolume("LEGION-4", time.Now().Add(-time.Hour))
	w.renameAtBoot(context.Background(), rt)

	for key, repo := range map[string]string{"LEGION-1": renameFrom, "LEGION-2": renameFrom, "LEGION-3": renameTo, "LEGION-4": renameFrom} {
		if names := storedRepositoryNames(t, pool, key); !namedAll(names, repo) {
			t.Errorf("%s's stored names = %v, want them on %s", key, names, repo)
		}
	}
	reasons := map[string]string{}
	for _, line := range renameLines(t, logged.String(), renameKeptLine) {
		reasons[line.Issue] = line.Reason
	}
	if want := map[string]string{"LEGION-1": "the runtime cannot say whether it holds a pod of the issue",
		"LEGION-2": "the runtime holds its pod pod-of-legion-2", "LEGION-4": renameKeptVolume}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("kept lines = %v, want %v", reasons, want)
	}
	if got := renameTables(t, logged.String())["issues"]; got != (renameTableTally{written: 1, left: 3}) {
		t.Errorf("issues line = %+v, want one written and the three kept left", got)
	}
}

// Under tmux, which runs no pod, an issue is kept while a workspace directory of it is on the host
// under the old name, in the spelling its record holds, at every hook: here a workflow tree's
// close, which writes the new name on each issue of the tree but the child whose workspace is
// still there, and a later boot, which writes the child once the directory is gone (INFRA-4401
// P2). Control: with the directory rule taken out (workspaceKeep keeping nothing), the tree's
// close writes the child under its workspace too; with the tree close's pod end removed
// (endTreePods), the root stays on the old name.
func TestUnderTmuxAnIssueKeepsItsNamesWhileItsWorkspaceIsUnderTheOldName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	until := time.Now().Add(-time.Minute)
	const spelled = "Sjawhar/Legion"
	root := record.Issue{Key: "LEGION-208", Repo: renameFrom, DefaultBranch: "main", GitHubRepo: renameFrom, Project: "LEGION", Tree: "LEGION-208",
		Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
	parent := root.Key
	child := record.Issue{Key: "LEGION-209", Repo: spelled, DefaultBranch: "main", GitHubRepo: spelled, Project: "LEGION", Tree: root.Key,
		Parent: &parent, Title: "child", Phase: phase.Done, Generation: 1, Status: "done", Rank: "V"}
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	stateDir := t.TempDir()
	located, err := workspace.Location(stateDir, ghrepo.MustParse(spelled), child.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(located.Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	var logged syncBuffer
	w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: stateDir, repositories: transferredRepositories(),
		log: slog.New(slog.NewJSONHandler(&logged, nil))}
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st,
		project: "legion", stateDir: stateDir}
	runner.podEnded = w.podEndsOn(rt)

	if err := runner.execute(ctx, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "tree_close", Tree: root.Key, Role: claim.RoleArchitect,
		Generation: 1, Linger: 1}, time.Now())); err != nil {
		t.Fatalf("close the tree: %v", err)
	}
	if names := storedRepositoryNames(t, pool, root.Key); !namedAll(names, renameTo) {
		t.Fatalf("%s's stored names after its tree's close = %v, want each on %s", root.Key, names, renameTo)
	}
	if names := storedRepositoryNames(t, pool, child.Key); !namedAll(names, spelled) {
		t.Fatalf("%s's stored names after its tree's close = %v, want them left on %s while its workspace is there", child.Key, names, spelled)
	}
	kept := renameLines(t, logged.String(), renameKeptLine)
	if len(kept) != 1 || kept[0].Issue != child.Key || !strings.Contains(kept[0].Reason, located.Dir) {
		t.Fatalf("kept lines = %+v, want one naming %s and its workspace %s", kept, child.Key, located.Dir)
	}

	if err := os.RemoveAll(located.Dir); err != nil {
		t.Fatal(err)
	}
	w.renameAtBoot(ctx, rt)
	if names := storedRepositoryNames(t, pool, child.Key); !namedAll(names, renameTo) {
		t.Fatalf("%s's stored names after a boot with its workspace gone = %v, want each on %s", child.Key, names, renameTo)
	}
}

// closingRuntime is a Kubernetes runtime's issue close: it asks the store under its pod lock and
// stops the pod when the store authorizes it. Its informer still holds the issue's Sandbox, as a
// Kubernetes runtime's may for a moment after the close, its pod on the configured image.
type closingRuntime struct {
	*fake.Runtime
	stopped int
}

func (r *closingRuntime) SuspendIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error),
	_ ...func(context.Context, runtime.ActRead) error) error {
	act, err := authorize(ctx)
	if err == nil && act {
		r.stopped++
	}
	return err
}

func (r *closingRuntime) IssuePodRunsConfiguredImage(string) bool { return true }

func (r *closingRuntime) IssueSandbox(issue string) (string, bool) {
	return "sandbox-of-" + issue, true
}

func (r *closingRuntime) IssuePod(string) (string, bool) { return "", true }

// closeAuthority is the store's answer to an issue close's suspension, and nothing else of the
// outbox's trees.
type closeAuthority struct {
	outboxTrees
	authorized bool
}

func (c closeAuthority) IssueSuspension(context.Context, string, store.IssueClose, func(string) bool) (bool, error) {
	return c.authorized, nil
}

// EndPausePodHold: the stand-in holds no pause, so the end of a pod's hold finds none.
func (c closeAuthority) EndPausePodHold(context.Context, string, string, store.PausePodHoldEnd, time.Time) (bool, error) {
	return false, nil
}

// An issue close the store authorizes ends the issue's pod, and its end writes the new name and
// drops the repository kept for the issue without reading the Sandbox the runtime may still hold;
// a close the store refuses finishes without acting, may leave the pod standing, and ends
// nothing. Control: with the close's pod end removed (suspendIssue's endPods), the authorized
// close leaves the old name; with it run whatever the store answered, the refused one writes the
// new name under the pod it left standing.
func TestAnIssueCloseEndsItsPodOnlyWhenTheStoreAuthorizedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		authorized bool
		want       string
	}{
		{name: "a close the store authorized", authorized: true, want: renameTo},
		{name: "a close the store refused", authorized: false, want: renameFrom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			const key = "LEGION-208"
			seedRenameRecords(t, pool, "LEGION", renameSeed{key: key, repo: renameFrom, branch: "legion/" + key, check: true, note: true})
			rt := &closingRuntime{Runtime: fake.NewRuntime()}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Runtime = rt
			w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: transferredRepositories(),
				log: quietLogger(), launched: map[string]ghbranch.Repository{strings.ToLower(key): {Name: ghrepo.MustParse(renameFrom), DefaultBranch: "main"}}}
			runner := &outbox{pool: pool, records: record.NewStore(), project: "legion", dispatchProject: "LEGION", supervisor: sup,
				trees: closeAuthority{authorized: tc.authorized}, log: quietLogger(), now: time.Now}
			runner.podEnded = w.podEndsOn(rt)

			if err := runner.execute(ctx, mustOutboxRow(t, key, record.IssueSuspend{Tree: key, Generation: 1, TreeGeneration: 1}, time.Now())); err != nil {
				t.Fatalf("run the close: %v", err)
			}
			if names := storedRepositoryNames(t, pool, key); len(names) != 5 || !namedAll(names, tc.want) {
				t.Fatalf("%s's stored names after the close = %v, want all five on %s", key, names, tc.want)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.authorized]; rt.stopped != want {
				t.Fatalf("the close stopped the pod %d times, want %d: the runtime acts only on the store's authorization", rt.stopped, want)
			}
			if _, held := w.launched[strings.ToLower(key)]; held != !tc.authorized {
				t.Fatalf("the repository kept for %s held = %t after the close, want %t", key, held, !tc.authorized)
			}
		})
	}
}

// renameRuntime is a workflow runtime over pool for the rename pass of INFRA's pair, as a process
// of its own boots it: its repository state holds no answer yet, its installation reads are
// installs, and its GitHub, for the pass's read of the pair's `to`, answers at githubAPI.
func renameRuntime(t *testing.T, pool *store.Pool, installs appauth.Installations, githubAPI string, logged *syncBuffer) *workflowRuntime {
	t.Helper()
	repos := newRepositories("LEGION", transferredLegion)
	return &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: t.TempDir(), installs: installs, repositories: repos,
		defaultBranches: newDefaultBranches(roleTokens{}, repos, githubAPI, defaultBranchTTL, time.Now), log: slog.New(slog.NewJSONHandler(logged, nil))}
}

// A pair configured before its transfer is left as it is: GitHub answers both role Apps still
// installed on the pair's `from`, as on its `to` (a repository made there before the transfer), so
// the boot's pass writes no row, and one WARN line names the pair and why. Once GitHub's answers
// change, the transfer done, a later pass writes it: the refresher's read of the pairs (readPairs)
// keeps GitHub's new answers, so the end of LEGION-1's pod writes LEGION-1, and the next boot,
// whose process reads the pair again, writes LEGION-2 (dispatch://INFRA-4401 check a-go, item
// 10). Control: with the gate taken out (run writing every configured pair, as though
// renameTransfer left none), the boot writes both issues while the `from` still answers installed.
func TestAPairConfiguredBeforeItsTransferIsLeftUntilGitHubShowsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-1", repo: renameFrom, note: true}, renameSeed{key: "LEGION-2", repo: renameFrom})
	installs := &installations{}
	github := newNamedGitHub(t, map[string]string{renameTo: renameTo + "@main"}).url
	rt := fake.NewRuntime()
	var logged syncBuffer
	w := renameRuntime(t, pool, installs, github, &logged)

	w.renameAtBoot(ctx, rt)
	for _, key := range []string{"LEGION-1", "LEGION-2"} {
		if names := storedRepositoryNames(t, pool, key); !namedAll(names, renameFrom) {
			t.Fatalf("%s's stored names after the boot = %v, want them left on %s: GitHub does not show the transfer", key, names, renameFrom)
		}
	}
	left := renameLines(t, logged.String(), renameLeftLine)
	if want := "the implement and review Apps are still installed on " + renameFrom; len(left) != 1 || left[0].Level != "WARN" ||
		left[0].From != renameFrom || left[0].To != renameTo || left[0].Reason != want {
		t.Fatalf("lines of a pair left = %+v, want one WARN line naming %s, %s and %q", left, renameFrom, renameTo, want)
	}
	if got := renameTables(t, logged.String()); got["issues"] != (renameTableTally{left: 2}) || got["repo_notes"] != (renameTableTally{left: 1}) {
		t.Fatalf("the boot's table lines = %+v, want nothing written and every row left", got)
	}

	installs.setMissing(true, "implement "+renameFrom, "review "+renameFrom)
	w.readPairs(ctx, false)
	w.podEndsOn(rt)(ctx, []string{"LEGION-1"})
	for key, repo := range map[string]string{"LEGION-1": renameTo, "LEGION-2": renameFrom} {
		if names := storedRepositoryNames(t, pool, key); !namedAll(names, repo) {
			t.Fatalf("%s's stored names after LEGION-1's pod end = %v, want them on %s", key, names, repo)
		}
	}

	renameRuntime(t, pool, installs, github, &logged).renameAtBoot(ctx, rt)
	if names := storedRepositoryNames(t, pool, "LEGION-2"); !namedAll(names, renameTo) {
		t.Fatalf("LEGION-2's stored names after the next boot = %v, want each on %s", names, renameTo)
	}
	if left := renameLines(t, logged.String(), renameLeftLine); len(left) != 1 {
		t.Fatalf("lines of a pair left = %+v, want the first boot's alone", left)
	}
}

// A pair whose reads GitHub does not answer is left as it is, and one WARN line names the read:
// both role Apps' installation reads of the pair's `to`, of which the implement App's fails, or,
// once both Apps answer installed there and neither on the `from`, the read of the `to`'s name and
// default branch, which GitHub answers with a 502. The next boot reads them again
// (TestAPairConfiguredBeforeItsTransferIsLeftUntilGitHubShowsIt). Control: with the gate taken out
// (run writing every configured pair), the boot writes the pair in each case.
func TestAPairWhoseReadsDoNotAnswerIsLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		github func(t *testing.T) string
		fail   []string
		reason string
	}{
		{name: "an installation read of the new name", fail: []string{"implement " + renameTo},
			github: func(t *testing.T) string {
				return newNamedGitHub(t, map[string]string{renameTo: renameTo + "@main"}).url
			},
			reason: "GitHub has not answered both role Apps' installation reads of " + renameTo + ": the implement App's installation on " +
				renameTo + " could not be read: GitHub answered 502"},
		{name: "a read of the new name's name and default branch",
			github: func(t *testing.T) string {
				return githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }})
			},
			reason: "the name and default branch of " + renameTo + " could not be read: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-1", repo: renameFrom})
			installs := &installations{missing: map[string]bool{"implement " + renameFrom: true, "review " + renameFrom: true}}
			installs.setFailing(&appauth.TransientError{Err: errors.New("GitHub answered 502")}, tc.fail...)
			var logged syncBuffer
			renameRuntime(t, pool, installs, tc.github(t), &logged).renameAtBoot(context.Background(), fake.NewRuntime())
			if names := storedRepositoryNames(t, pool, "LEGION-1"); !namedAll(names, renameFrom) {
				t.Fatalf("LEGION-1's stored names = %v, want them left on %s: GitHub's read did not answer", names, renameFrom)
			}
			left := renameLines(t, logged.String(), renameLeftLine)
			if len(left) != 1 || left[0].Level != "WARN" || left[0].From != renameFrom || !strings.HasPrefix(left[0].Reason, tc.reason) {
				t.Fatalf("lines of a pair left = %+v, want one WARN line naming %s and starting %q", left, renameFrom, tc.reason)
			}
		})
	}
}

// A pair whose `to` a role App is not installed on is left as it is, and one WARN line names the
// App: GitHub answers neither role App installed on the pair's `from`, the transfer done, and the
// implement App installed on its `to` but not the review App, so a record written under the `to`
// would work a repository its reviewer cannot reach. The boot's pass writes no row, and each
// table's line counts the rows left on the `from` (dispatch://INFRA-4401 check a-go, item 10).
// Control: with the gate taken out (run writing every configured pair, as though renameTransfer
// left none), the boot writes the pair.
func TestAPairWhoseNewNameARoleAppIsNotInstalledOnIsLeft(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-1", repo: renameFrom, note: true})
	installs := &installations{missing: map[string]bool{"implement " + renameFrom: true, "review " + renameFrom: true, "review " + renameTo: true}}
	var logged syncBuffer
	renameRuntime(t, pool, installs, newNamedGitHub(t, map[string]string{renameTo: renameTo + "@main"}).url, &logged).
		renameAtBoot(context.Background(), fake.NewRuntime())
	if names := storedRepositoryNames(t, pool, "LEGION-1"); !namedAll(names, renameFrom) {
		t.Fatalf("LEGION-1's stored names = %v, want them left on %s: the review App is not installed on %s", names, renameFrom, renameTo)
	}
	left := renameLines(t, logged.String(), renameLeftLine)
	if want := "the review App is not installed on " + renameTo; len(left) != 1 || left[0].Level != "WARN" ||
		left[0].From != renameFrom || left[0].To != renameTo || left[0].Reason != want {
		t.Fatalf("lines of a pair left = %+v, want one WARN line naming %s, %s and %q", left, renameFrom, renameTo, want)
	}
	if got := renameTables(t, logged.String()); got["issues"] != (renameTableTally{left: 1}) || got["repo_notes"] != (renameTableTally{left: 1}) {
		t.Fatalf("the boot's table lines = %+v, want nothing written and every row left", got)
	}
}

// A GitHub that answers the installation read of the pair's old name as it answers a moved
// repository's — GET /repos/sjawhar/legion/installation with a 301 to where the repository is now,
// which answers installed under the App's JWT — and installed for the new name shows the transfer:
// a pair's names are read under their own names, following no redirect (readInstallations,
// appauth's InstalledUnder), so neither role App answers installed on the `from`, the rename rule
// serves a record on the `from` under the `to`, and the boot's pass writes the pair. GitHub is never
// asked for the installation the redirect names. Control: with a pair's names read following the
// redirect (readInstallations reading every name by InstalledOn), both Apps answer installed on
// the `from`, the rule serves it as itself, and the pass leaves the pair.
func TestAGitHubRedirectOfAPairsOldNameShowsItsTransfer(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	const moved = "/repositories/1146927990/installation"
	var mu sync.Mutex
	var asked []string
	server := githubtest.Serve(t, githubtest.Spec{
		Record: func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, r.URL.Path)
		},
		Handle: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/" + renameFrom + "/installation":
				http.Redirect(w, r, moved, http.StatusMovedPermanently)
			case moved, "/repos/" + renameTo + "/installation":
				fmt.Fprint(w, `{"id":77}`)
			case "/repos/" + renameTo:
				fmt.Fprint(w, `{"full_name":"`+renameTo+`","default_branch":"main"}`)
			default:
				githubtest.Unexpected(t, w, r)
			}
		},
	})
	installs := appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-1", repo: renameFrom, branch: "legion/LEGION-1"})
	var logged syncBuffer
	w := renameRuntime(t, pool, installs, server.URL, &logged)

	w.renameAtBoot(context.Background(), fake.NewRuntime())
	if served := w.repositories.serving(ghrepo.MustParse(renameFrom)); served.String() != renameTo {
		t.Fatalf("a record on %s is served as %s, want %s: GitHub redirects the old name", renameFrom, served, renameTo)
	}
	if names := storedRepositoryNames(t, pool, "LEGION-1"); !namedAll(names, renameTo) {
		t.Fatalf("LEGION-1's stored names after the boot = %v, want each on %s", names, renameTo)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(asked, moved) {
		t.Fatalf("GitHub was asked %v, want no read of the installation its redirect names", asked)
	}
}

// transferredGitHub is a GitHub after the transfer of INFRA's pair, read through real App
// credentials (the implement App id 1 and the review App id 2), so that every request a reader of
// the pair makes reaches it and is kept, as "<method> <path>": neither App is installed under the
// pair's `from` (404) and both are under its `to` (installation 77); the implement App's token for
// the `to` owner is minted through the installations' discovery, the installation's permissions
// read, its exchange and the reads of the App's bot identity (GET /app, GET /users/<bot>); and GET
// /repos/cybermonklabs/legion names the repository, at main.
type transferredGitHub struct {
	apps  *appauth.Manager
	url   string
	mu    sync.Mutex
	asked []string
}

func newTransferredGitHub(t *testing.T) *transferredGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := &transferredGitHub{}
	server := githubtest.Serve(t, githubtest.Spec{
		Record: func(r *http.Request) {
			github.mu.Lock()
			defer github.mu.Unlock()
			github.asked = append(github.asked, r.Method+" "+r.URL.Path)
		},
		Handle: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/" + renameFrom + "/installation":
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			case "/repos/" + renameTo + "/installation":
				fmt.Fprint(w, `{"id":77}`)
			case "/app/installations":
				fmt.Fprint(w, `[{"id":77,"account":{"login":"`+ghrepo.MustParse(renameTo).Owner()+`"}}]`)
			case "/app/installations/77":
				fmt.Fprint(w, `{"id":77,"permissions":{"actions":"write","checks":"write","contents":"write","metadata":"read","pull_requests":"write","statuses":"write","workflows":"write"}}`)
			case "/app/installations/77/access_tokens":
				mintedToken(w, "installation-token", exchangeBody(t, r))
			case "/app":
				fmt.Fprintf(w, `{"slug":"legion-test-%s"}`, jwtIssuer(t, r))
			case "/users/legion-test-1[bot]", "/users/legion-test-2[bot]":
				fmt.Fprint(w, `{"id":42}`)
			case "/repos/" + renameTo:
				fmt.Fprint(w, `{"full_name":"`+renameTo+`","default_branch":"main"}`)
			default:
				githubtest.Unexpected(t, w, r)
			}
		},
	})
	github.url = server.URL
	github.apps = appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
	return github
}

// take is each request g was sent since the last take, in the order they came.
func (g *transferredGitHub) take() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	asked := g.asked
	g.asked = nil
	return asked
}

// The rename pass sends GitHub no request for a pair that no stored row names (dispatch://INFRA-4401
// A2 and A10, check a-go, item 10): it reads each pair's rows first, and asks GitHub whether the
// pair's transfer has happened only where a row still names its `from`. Each pass here runs on a
// workflow runtime of its own, as a process boots it, with no background loop running and a
// repository state that holds no answer (renameRuntime), and every request it could make for the
// pair — both role Apps' installation reads of both names, the mint of the implement App's token
// for the `to` owner and the read of the `to`'s name and default branch — reaches one stand-in
// GitHub, which counts it (transferredGitHub). Where the project's rows name the pair's `to` and
// another repository, but not its `from`, the boot's pass (renameAtBoot) and the end of both
// issues' pods (podEndsOn) send it nothing and log no line of a pair left, and the boot still logs
// each table's line, nothing left on the `from`. Once a row names the `from`, the boot's pass reads
// the pair, and writes it. Control: with the pair decided before its rows are read (run asking
// transferred before candidates), the boot's pass and the pod end each send GitHub the pair's
// reads.
func TestTheRenamePassSendsGitHubNoRequestForAPairNoRowNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := newTransferredGitHub(t)
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-1", repo: renameTo, branch: "legion/LEGION-1", check: true, note: true},
		renameSeed{key: "LEGION-2", repo: "acme/widgets"})
	rt := fake.NewRuntime()
	process := func(logged *syncBuffer) *workflowRuntime {
		w := renameRuntime(t, pool, github.apps, github.url, logged)
		w.defaultBranches = newDefaultBranches(github.apps, w.repositories, github.url, defaultBranchTTL, time.Now)
		return w
	}

	var boot syncBuffer
	process(&boot).renameAtBoot(ctx, rt)
	if asked := github.take(); len(asked) != 0 {
		t.Errorf("the boot's rename pass sent GitHub %v, want no request: no row names %s", asked, renameFrom)
	}
	if left := renameLines(t, boot.String(), renameLeftLine); len(left) != 0 {
		t.Errorf("the boot's lines of a pair left = %+v, want none: no row names %s", left, renameFrom)
	}
	want := map[string]renameTableTally{"issues": {}, "pull_requests": {}, "cleared_pull_requests": {}, "production_checks": {}, "repo_notes": {}}
	if got := renameTables(t, boot.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("the boot's table lines = %+v, want each table's, nothing written and nothing left", got)
	}

	var podEnd syncBuffer
	process(&podEnd).podEndsOn(rt)(ctx, []string{"LEGION-1", "LEGION-2"})
	if asked := github.take(); len(asked) != 0 {
		t.Errorf("the pod end's rename pass sent GitHub %v, want no request: no row of its issues names %s", asked, renameFrom)
	}
	if lines := renameLines(t, podEnd.String(), renameLeftLine); len(lines) != 0 {
		t.Errorf("the pod end's lines of a pair left = %+v, want none", lines)
	}
	if lines := renameLines(t, podEnd.String(), renameTableLine); len(lines) != 0 {
		t.Errorf("the pod end's table lines = %+v, want none: no row of its issues names %s", lines, renameFrom)
	}

	seedRenameRecords(t, pool, "LEGION", renameSeed{key: "LEGION-3", repo: renameFrom})
	var next syncBuffer
	process(&next).renameAtBoot(ctx, rt)
	asked := github.take()
	for _, read := range []string{"GET /repos/" + renameFrom + "/installation", "GET /repos/" + renameTo + "/installation", "GET /repos/" + renameTo} {
		if !slices.Contains(asked, read) {
			t.Errorf("the next boot's rename pass sent GitHub %v, want %s among them: LEGION-3 names %s", asked, read, renameFrom)
		}
	}
	if names := storedRepositoryNames(t, pool, "LEGION-3"); !namedAll(names, renameTo) {
		t.Errorf("LEGION-3's stored names after the next boot = %v, want each on %s", names, renameTo)
	}
}
