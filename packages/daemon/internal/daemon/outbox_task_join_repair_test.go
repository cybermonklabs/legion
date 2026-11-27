package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The tests here follow the task of a first step whose join conflicted, as its agent does, with
// the real jj in a working copy of the fake GitHub's repository, and read the pull request of the
// task against its join branch as GitHub would list its files.

// taskNeedingD is e of the plan of seedJoinRepair: it needs d, so d is no task that merges, and the
// pull request of d, which needs a and c, is based on d's join branch.
var taskNeedingD = record.PlanTask{Task: "e", Needs: []string{"d"}}

// resolvedX is x.txt as the agent of d's first step repairs the conflict of a and c in it: the line
// of each.
const resolvedX = "from a\nfrom c\n"

// jjCommand is a command a task tells its agent to run: a jj command between backquotes.
var jjCommand = regexp.MustCompile("`(jj [^`]+)`")

// taskCopyDir is the directory under copies of the working copy of branch, or of the working copy
// name the engine hands its directory (workflow.Config.WorkspaceDir): the branch without legion/,
// as the runtime names a task's working copy (workspace.Name).
func taskCopyDir(copies, branch string) string {
	return filepath.Join(copies, strings.TrimPrefix(branch, "legion/"))
}

// seedJoinRepair is seedJoin's rig with taskNeedingD, whose engine gives each working copy its
// directory under the answered directory (taskCopyDir), so the task of d's first step names its
// working copy's directory in each jj command it tells.
func seedJoinRepair(t *testing.T) (*taskBranchRig, record.Issue, string) {
	t.Helper()
	copies := t.TempDir()
	rig, issue := seedJoin(t, func(name string, _ ghrepo.Repository) string { return taskCopyDir(copies, name) }, taskNeedingD)
	return rig, issue, copies
}

// newestStartTask is the task of the newest start row the outbox holds for issue.
func newestStartTask(t *testing.T, rig *taskBranchRig, issue record.Issue) string {
	t.Helper()
	var start record.SuperviseRequest
	if err := rig.pool.QueryRow(context.Background(), `select payload from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		order by id desc limit 1`, issue.Key).Scan(&start); err != nil {
		t.Fatalf("read the newest start row of %s: %v", issue.Key, err)
	}
	return start.Task
}

// taskCopy is the working copy of a task as the shim provisions it (workspace.ProvisionTask): a jj
// repository of the fake GitHub's repository, the task's branch tracked from origin, and the
// working copy an empty commit on that branch's head.
type taskCopy struct {
	t   *testing.T
	run fixturegit.Runner
	dir string
}

// cloneTaskCopy is the working copy of branch at dir.
func cloneTaskCopy(t *testing.T, rig *taskBranchRig, dir, branch string) taskCopy {
	t.Helper()
	c := taskCopy{t: t, run: fixturegit.Isolated(t, t.TempDir()), dir: dir}
	c.run.Run(filepath.Dir(dir), "jj", "git", "clone", rig.github.Dir(), dir)
	c.jj("bookmark", "track", branch+"@origin")
	c.jj("new", branch+"@origin")
	return c
}

// jj runs jj with args in the working copy, failing the test when it fails.
func (c taskCopy) jj(args ...string) string {
	c.t.Helper()
	return c.run.Run(c.dir, append([]string{"jj", "-R", c.dir}, args...)...)
}

// write writes each of files (path to content) in the working copy.
func (c taskCopy) write(files map[string]string) {
	c.t.Helper()
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(c.dir, path), []byte(body), 0o644); err != nil {
			c.t.Fatal(err)
		}
	}
}

// follow runs each jj command task tells, in the order it names them, in the working copy, as an
// agent that does what its task says: a command that lists the paths a conflict leaves (`jj resolve
// --list`) is answered by writing resolved's content over each path it lists, and lists none where
// jj finds no conflict; every other command runs as written, through a shell, and its failure fails
// the test. A task that tells no jj command fails the test.
func (c taskCopy) follow(task string, resolved map[string]string) {
	c.t.Helper()
	commands := jjCommand.FindAllStringSubmatch(task, -1)
	if len(commands) == 0 {
		c.t.Fatalf("the task tells no jj command to follow: %q", task)
	}
	for _, command := range commands {
		line := command[1]
		if !strings.Contains(line, " resolve --list") {
			c.run.Run(c.dir, "sh", "-c", line)
			continue
		}
		out, err := c.run.Try(c.dir, "sh", "-c", line)
		if err != nil {
			if strings.Contains(out, "No conflicts found") {
				continue
			}
			c.t.Fatalf("%s: %v\n%s", line, err, out)
		}
		for _, listed := range strings.Split(strings.TrimSpace(out), "\n") {
			path := strings.Fields(listed)[0]
			body, ok := resolved[path]
			if !ok {
				c.t.Fatalf("%s lists %s, which the test has no repair of:\n%s", line, path, out)
			}
			c.write(map[string]string{path: body})
		}
	}
}

// work is the agent's own work after the repair, as an implementer does it: files written in the
// working copy, described with message, and branch moved there and pushed.
func (c taskCopy) work(branch string, files map[string]string, message string) {
	c.t.Helper()
	c.write(files)
	c.jj("describe", "-m", message)
	c.jj("bookmark", "set", branch, "-r", "@")
	c.jj("git", "push", "--bookmark", branch)
}

// pullRequestFiles is the files of a pull request of head against base, as GitHub lists them: the
// files of the fake's compare of the two, in order.
func (rig *taskBranchRig) pullRequestFiles(t *testing.T, base, head string) []string {
	t.Helper()
	answer, err := http.Get(rig.runner.githubAPI + "/repos/acme/widgets/compare/" + base + "..." + head)
	if err != nil {
		t.Fatalf("compare %s with %s: %v", head, base, err)
	}
	defer answer.Body.Close()
	var comparison struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("compare %s with %s answered %s", head, base, answer.Status)
	}
	if err := json.NewDecoder(answer.Body).Decode(&comparison); err != nil {
		t.Fatalf("decode the compare of %s with %s: %v", head, base, err)
	}
	files := make([]string, 0, len(comparison.Files))
	for _, file := range comparison.Files {
		files = append(files, file.Filename)
	}
	slices.Sort(files)
	return files
}

// holds says whether commit holds ancestor in the fake's repository.
func (rig *taskBranchRig) holds(t *testing.T, commit, ancestor string) bool {
	t.Helper()
	_, err := fixturegit.Isolated(t, t.TempDir()).Try(rig.github.Dir(), "git", "merge-base", "--is-ancestor", ancestor, commit)
	return err == nil
}

// show is path as commit holds it in the fake's repository.
func (rig *taskBranchRig) show(t *testing.T, commit, path string) string {
	t.Helper()
	return fixturegit.Isolated(t, t.TempDir()).Raw(rig.github.Dir(), "git", "show", commit+":"+path)
}

// joinRepair is the state after the agent of d's first step followed its task: the rig, the issue,
// d's working copy, d's branch and its join branch, and the heads of a and c.
type joinRepair struct {
	rig          *taskBranchRig
	issue        record.Issue
	copy         taskCopy
	branch, join string
	a, c         string
}

// repairedJoin is seedJoinRepair's join of d run (runJoinOfD): GitHub refuses c's merge into d's
// join branch. The agent of d's first step then follows its task in d's working copy, repairing
// x.txt with the lines of both (resolvedX), and does its own work, own, which it pushes on d's
// branch.
func repairedJoin(t *testing.T, own map[string]string) joinRepair {
	t.Helper()
	rig, issue, copies := seedJoinRepair(t)
	runJoinOfD(t, rig, issue)
	branch := rig.branch(t, "d")
	r := joinRepair{rig: rig, issue: issue, branch: branch, join: branch + claim.JoinBranchSuffix,
		a: rig.github.Head(t, rig.branch(t, "a")), c: rig.github.Head(t, rig.branch(t, "c"))}
	r.copy = cloneTaskCopy(t, rig, taskCopyDir(copies, branch), branch)
	r.copy.follow(newestStartTask(t, rig, issue), map[string]string{"x.txt": resolvedX})
	r.copy.work(branch, own, "LEGION-208: the work of task d")
	return r
}

// A task that needs two tasks whose changes conflict in one file has its join repaired on its join
// branch, the base of its pull request, so that pull request shows the task's own work alone:
// GitHub refuses the merge of c into d's join branch, which stands at a's head, and the agent of
// d's first step follows its task with the real jj. The join branch then holds the work of a and c
// with x.txt repaired, d's branch holds the join branch, and the pull request of d against its join
// branch lists d.txt, d's own file, alone.
//
// The fault it catches is the repair committed on the task's own branch: the pull request of d then
// shows c's work as d's. Its control is that repair, done by hand: the pull request of d lists c.txt,
// so the files the test reads can show the work of a need.
func TestAJoinThatConflictsIsRepairedOnTheJoinBranch(t *testing.T) {
	t.Parallel()
	t.Run("the repair the task of the first step tells", func(t *testing.T) {
		r := repairedJoin(t, map[string]string{"d.txt": "d\n"})
		if files := r.rig.pullRequestFiles(t, r.join, r.branch); !slices.Equal(files, []string{"d.txt"}) {
			t.Fatalf("the pull request of d against %s lists %v, want d's own file alone [d.txt]", r.join, files)
		}
		join := r.rig.github.Head(t, r.join)
		if !r.rig.holds(t, join, r.a) || !r.rig.holds(t, join, r.c) {
			t.Fatalf("d's join branch %s stands at %s, want it to hold a's head %s and c's head %s", r.join, join, r.a, r.c)
		}
		if x := r.rig.show(t, join, "x.txt"); x != resolvedX {
			t.Fatalf("x.txt on d's join branch = %q, want the repair %q", x, resolvedX)
		}
		if head := r.rig.github.Head(t, r.branch); !r.rig.holds(t, head, join) {
			t.Fatalf("d's branch stands at %s, want it to hold its repaired join branch's head %s", head, join)
		}
	})
	t.Run("control: the repair on the task's branch shows c's work as d's", func(t *testing.T) {
		rig, issue, copies := seedJoinRepair(t)
		runJoinOfD(t, rig, issue)
		branch := rig.branch(t, "d")
		d := cloneTaskCopy(t, rig, taskCopyDir(copies, branch), branch)
		d.jj("git", "fetch")
		d.jj("new", "@-", rig.branch(t, "c")+"@origin", "-m", "LEGION-208: join the work of task c")
		d.write(map[string]string{"x.txt": resolvedX})
		d.jj("new")
		d.work(branch, map[string]string{"d.txt": "d\n"}, "LEGION-208: the work of task d")
		if files := rig.pullRequestFiles(t, branch+claim.JoinBranchSuffix, branch); !slices.Contains(files, "c.txt") {
			t.Fatalf("the pull request of d against its join branch lists %v, want c.txt among them: the repair on d's branch is d's work there", files)
		}
	})
}

// A start again of a task whose own work conflicts with what its needs did since is told to merge
// its join branch into its branch, and the join branch takes no commit of that repair: after the
// first repair (repairedJoin), d's own work adds y.txt; c then adds y.txt too, and d starts again.
// GitHub merges c into d's join branch, and refuses the merge of the join branch into d's branch.
// The record says so apart from the needs' conflicts: d holds no conflict of a need, and its join
// branch did not merge into its branch. The agent of d's first step follows its task: d's branch
// then holds the join branch with c's new work, the join branch stands where the join left it, the
// merge the agent pushed stays under its own work, which rewrites no pushed commit, and the pull
// request of d lists d's own files alone.
//
// The fault it catches is that conflict told as a conflict of the needs on the join branch, which
// holds their work already (its control: the executor recording every need as a conflict, as it
// did, under which the record holds a and c).
func TestAJoinBranchThatCannotMergeIntoTheTasksBranchIsMergedThere(t *testing.T) {
	t.Parallel()
	r := repairedJoin(t, map[string]string{"d.txt": "d\n", "y.txt": "from d\n"})
	moved := r.rig.github.Commit(t, r.rig.branch(t, "c"), map[string]string{"y.txt": "from c\n"}, "c again")
	if err := store.Transact(context.Background(), r.rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.rig.records.StopTask(context.Background(), tx, r.issue.Key, r.issue.Generation, "d", "task d of LEGION-208 was stopped by its owner"); err != nil {
			return err
		}
		return r.rig.records.MoveTask(context.Background(), tx, r.issue.Key, r.issue.Generation, "d", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning)
	}); err != nil {
		t.Fatalf("start d again: %v", err)
	}
	r.rig.run(t, "d")

	d := r.rig.task(t, "d")
	var unmerged bool
	if err := r.rig.pool.QueryRow(context.Background(), `select join_unmerged from plan_tasks where issue = $1 and generation = $2 and task = 'd'`,
		r.issue.Key, int64(r.issue.Generation)).Scan(&unmerged); err != nil {
		t.Fatalf("read whether d's join branch merged into its branch: %v", err)
	}
	if len(d.JoinConflicts) != 0 || !unmerged {
		t.Fatalf("task d after its start again holds the join conflicts %v and an unmerged join %v, want none and true", d.JoinConflicts, unmerged)
	}
	join := r.rig.github.Head(t, r.join)
	if !r.rig.holds(t, join, moved) {
		t.Fatalf("d's join branch stands at %s, want it to hold c's new head %s", join, moved)
	}
	r.copy.follow(newestStartTask(t, r.rig, r.issue), map[string]string{"y.txt": "from c\nfrom d\n"})
	merged := r.rig.github.Head(t, r.branch)
	r.copy.work(r.branch, map[string]string{"d2.txt": "d again\n"}, "LEGION-208: the work of task d again")
	if head := r.rig.github.Head(t, r.branch); !r.rig.holds(t, head, merged) {
		t.Fatalf("d's branch stands at %s, want it to hold the merge %s its agent pushed: the agent's own work rewrote a pushed commit", head, merged)
	}

	if now := r.rig.github.Head(t, r.join); now != join {
		t.Fatalf("d's join branch stands at %s after the repair, want it where the join left it, %s", now, join)
	}
	if head := r.rig.github.Head(t, r.branch); !r.rig.holds(t, head, join) {
		t.Fatalf("d's branch stands at %s, want it to hold its join branch's head %s", head, join)
	}
	if files := r.rig.pullRequestFiles(t, r.join, r.branch); !slices.Equal(files, []string{"d.txt", "d2.txt", "y.txt"}) {
		t.Fatalf("the pull request of d against %s lists %v, want d's own files alone [d.txt d2.txt y.txt]", r.join, files)
	}
}

// A task that merges has no join branch, so its join is made on its own branch, and its first step
// repairs a join that conflicts there: the plan of seedJoin, whose d needs a and c and no task needs
// d. GitHub refuses the merge of c into d's branch, and the agent of d's first step follows its task
// with the real jj: d's branch then holds a's and c's heads with x.txt repaired, and the join the
// agent pushed stays under its own work, which rewrites no pushed commit.
//
// The fault it catches is a task text whose commands leave the agent's own work in the pushed join
// commit, so its next push rewrites that commit (its control: the text without its last `jj new`,
// under which d's head no longer holds the join it pushed).
func TestAJoinOfATaskThatMergesIsRepairedOnItsBranch(t *testing.T) {
	t.Parallel()
	copies := t.TempDir()
	rig, issue := seedJoin(t, func(name string, _ ghrepo.Repository) string { return taskCopyDir(copies, name) })
	runJoinOfD(t, rig, issue)
	branch := rig.branch(t, "d")
	a, c := rig.github.Head(t, rig.branch(t, "a")), rig.github.Head(t, rig.branch(t, "c"))
	d := cloneTaskCopy(t, rig, taskCopyDir(copies, branch), branch)
	d.follow(newestStartTask(t, rig, issue), map[string]string{"x.txt": resolvedX})
	joined := rig.github.Head(t, branch)
	if !rig.holds(t, joined, a) || !rig.holds(t, joined, c) {
		t.Fatalf("d's branch stands at %s after the repair, want it to hold a's head %s and c's head %s", joined, a, c)
	}
	if x := rig.show(t, joined, "x.txt"); x != resolvedX {
		t.Fatalf("x.txt on d's branch after the repair = %q, want %q", x, resolvedX)
	}
	d.work(branch, map[string]string{"d.txt": "d\n"}, "LEGION-208: the work of task d")
	if head := rig.github.Head(t, branch); !rig.holds(t, head, joined) {
		t.Fatalf("d's branch stands at %s, want it to hold the join %s its agent pushed: the agent's own work rewrote a pushed commit", head, joined)
	}
}
