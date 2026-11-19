package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// renameFrom and renameTo are INFRA's pair, the one every test of the rename pass configures.
const renameFrom, renameTo = "sjawhar/legion", "cybermonklabs/legion"

func renamePair() ghrepo.Renames {
	return ghrepo.MustRenames(ghrepo.MustRename(renameFrom, renameTo))
}

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
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
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

// renameBoot boots the workflow daemon of cfg logging to logged, and returns once the boot's rename
// pass has logged its table lines and the daemon answers /healthz.
func renameBoot(t *testing.T, cfg config.Config, logged *syncBuffer) *daemon {
	t.Helper()
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logged, nil)))
	testwait.Eventually(t, "the boot's rename pass logging each table", func() bool {
		return len(renameTables(t, logged.String())) == len(record.RenameTables)
	})
	return d
}

// A boot of a daemon that configures a rename writes the pair's new name on every stored column
// that held the old one, in any spelling: the record's repository and GitHub's spelling, the pull
// request, the production check and the repository note, the note of an issue the project never
// recorded among them. Where the new name would give the pull requests' unique key one repository
// and branch twice, the row under the new name stays and the other issue's is deleted, one line
// naming both issues. Each table's line counts the rows written, the rows failed and the rows left
// on the old name, which is none; an issue on another repository is left alone. A second boot
// writes nothing. Control: with the pass deleted from the boot (run's renameAtBoot), every row is
// left on the old name.
func TestTheBootWritesEachRenamesNewNameOverItsOldOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.RepositoryRenames = renamePair()
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

	var logged syncBuffer
	d := renameBoot(t, cfg, &logged)
	for _, key := range []string{p + "-1", p + "-2", p + "-3", p + "-4", p + "-9"} {
		if names := storedRepositoryNames(t, pool, key); !namedAll(names, renameTo) {
			t.Errorf("%s's stored names after the boot = %v, want each on %s", key, names, renameTo)
		}
	}
	if names := storedRepositoryNames(t, pool, p+"-2"); names["pull_requests.repo"] != "" {
		t.Errorf("%s's pull request = %v, want it deleted: %s-3's holds release/shared on %s", p+"-2", names, p, renameTo)
	}
	if names := storedRepositoryNames(t, pool, p+"-6"); len(names) != 5 || !namedAll(names, "acme/widgets") {
		t.Errorf("%s's stored names = %v, want all five left on acme/widgets", p+"-6", names)
	}
	deleted := renameLines(t, logged.String(), renameDeletedLine)
	if len(deleted) != 1 || deleted[0].Issue != p+"-2" || deleted[0].Kept != p+"-3" || deleted[0].Branch != "release/shared" || deleted[0].Level != "WARN" {
		t.Errorf("deletion lines = %+v, want one WARN line naming %s-2 deleted and %s-3 kept on release/shared", deleted, p, p)
	}
	want := map[string]renameTableTally{"issues": {written: 3}, "pull_requests": {written: 1}, "production_checks": {written: 1}, "repo_notes": {written: 2}}
	if got := renameTables(t, logged.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("table lines of the first boot = %+v, want %+v", got, want)
	}

	d.stop()
	rebindHeldPorts(t, &cfg)
	var again syncBuffer
	renameBoot(t, cfg, &again)
	want = map[string]renameTableTally{"issues": {}, "pull_requests": {}, "production_checks": {}, "repo_notes": {}}
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
	pool := isolatedOutboxPool(t)
	cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.RepositoryRenames = renamePair()
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
		"production_checks": {}, "repo_notes": {}}
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
	want = map[string]renameTableTally{"issues": {written: 1}, "pull_requests": {written: 1}, "production_checks": {}, "repo_notes": {}}
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
	rig := newIdleRig(t)
	ctx := context.Background()
	if _, err := rig.pool.Exec(ctx, "update issues set repo = $2, github_repo = $2 where key = $1", rig.issue.Key, renameFrom); err != nil {
		t.Fatal(err)
	}
	var logged syncBuffer
	rig.w.log = slog.New(slog.NewJSONHandler(&logged, nil))
	rig.w.repositories = newRepositories("LEGION", renamePair())
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

// presenceRuntime answers, by issue, the Sandbox and the pod a Kubernetes runtime holds.
type presenceRuntime struct {
	*fake.Runtime
	sandboxes map[string]string
	pods      map[string]string
	unread    map[string]bool
}

func (r presenceRuntime) IssueSandbox(issue string) (string, bool) {
	name, present := r.sandboxes[issue]
	return name, present
}

func (r presenceRuntime) IssuePod(issue string) (string, bool) {
	if r.unread[issue] {
		return "", false
	}
	return r.pods[issue], true
}

// At boot the pass keeps an issue the runtime cannot say it holds no pod of — its Sandbox store
// cannot be read for it — as it keeps one whose pod it holds with no Sandbox left, and writes the
// issue the runtime holds nothing of. Control: with the live-pod rule taken out, the first two
// are written too.
func TestAnIssueTheRuntimeCannotSayOfKeepsItsNames(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedRenameRecords(t, pool, "LEGION",
		renameSeed{key: "LEGION-1", repo: renameFrom}, renameSeed{key: "LEGION-2", repo: renameFrom}, renameSeed{key: "LEGION-3", repo: renameFrom})
	var logged syncBuffer
	w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: newRepositories("LEGION", renamePair()),
		log: slog.New(slog.NewJSONHandler(&logged, nil))}
	w.renameAtBoot(context.Background(), presenceRuntime{Runtime: fake.NewRuntime(), unread: map[string]bool{"LEGION-1": true},
		pods: map[string]string{"LEGION-2": "pod-of-legion-2"}})

	for key, repo := range map[string]string{"LEGION-1": renameFrom, "LEGION-2": renameFrom, "LEGION-3": renameTo} {
		if names := storedRepositoryNames(t, pool, key); !namedAll(names, repo) {
			t.Errorf("%s's stored names = %v, want them on %s", key, names, repo)
		}
	}
	reasons := map[string]string{}
	for _, line := range renameLines(t, logged.String(), renameKeptLine) {
		reasons[line.Issue] = line.Reason
	}
	if want := map[string]string{"LEGION-1": "the runtime cannot say whether it holds a pod of the issue",
		"LEGION-2": "the runtime holds its pod pod-of-legion-2"}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("kept lines = %v, want %v", reasons, want)
	}
	if got := renameTables(t, logged.String())["issues"]; got != (renameTableTally{written: 1, left: 2}) {
		t.Errorf("issues line = %+v, want one written and the two kept left", got)
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

	rt := fake.NewRuntime()
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	var logged syncBuffer
	w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: stateDir, repositories: newRepositories("LEGION", renamePair()),
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
// Kubernetes runtime's may for a moment after the close.
type closingRuntime struct {
	*fake.Runtime
	stopped int
}

func (r *closingRuntime) SuspendIssue(ctx context.Context, _, _ string, authorize func(context.Context) (bool, error)) error {
	act, err := authorize(ctx)
	if err == nil && act {
		r.stopped++
	}
	return err
}

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

// An issue close the store authorizes ends the issue's pod, and its end writes the new name and
// drops the repository kept for the issue without reading the Sandbox the runtime may still hold;
// a close the store refuses finishes without acting, may leave the pod standing, and ends
// nothing. Control: with the close's pod end removed (the record.IssueSuspend case's endPods), the
// authorized close leaves the old name; with it run whatever the store answered, the refused one
// writes the new name under the pod it left standing.
func TestAnIssueCloseEndsItsPodOnlyWhenTheStoreAuthorizedIt(t *testing.T) {
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
			w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", stateDir: t.TempDir(), repositories: newRepositories("LEGION", renamePair()),
				log: quietLogger(), launched: map[string]ghbranch.Repository{strings.ToLower(key): {Name: ghrepo.MustParse(renameFrom), DefaultBranch: "main"}}}
			runner := &outbox{pool: pool, records: record.NewStore(), project: "legion", dispatchProject: "LEGION", supervisor: sup,
				trees: closeAuthority{authorized: tc.authorized}, log: quietLogger()}
			runner.podEnded = w.podEndsOn(rt)

			if err := runner.execute(ctx, mustOutboxRow(t, key, record.IssueSuspend{Tree: key, Generation: 1, TreeGeneration: 1}, time.Now())); err != nil {
				t.Fatalf("run the close: %v", err)
			}
			if names := storedRepositoryNames(t, pool, key); len(names) != 5 || !namedAll(names, tc.want) {
				t.Fatalf("%s's stored names after the close = %v, want all five on %s", key, names, tc.want)
			}
			if _, held := w.launched[strings.ToLower(key)]; held != !tc.authorized {
				t.Fatalf("the repository kept for %s held = %t after the close, want %t", key, held, !tc.authorized)
			}
		})
	}
}
