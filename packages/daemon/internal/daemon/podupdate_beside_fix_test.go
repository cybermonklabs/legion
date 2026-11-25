package daemon

// An update round's ask beside a fix: each role is told where it pushes (supervise.UpdatePush), a
// step beside the fix to a branch of its own, the fix to its own branch F, and the implementer
// nowhere while its fix reads its job. The sweep reads the pushes once for the issue through the
// workflow's engine (supervision.updatePushes, workflow.(*Engine).UpdatePushes) and copies each
// role's push onto its row (record.UpdateAsk), so these tests read the rows' typed fields and no
// text. They drive the pod-update rig of podupdate_test.go, whose supervision reads the pushes as
// the workflow's attach wires it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// besideX is the head of the task's pull request when fix 1 starts, and besideY the head the
	// fix ends at.
	besideX = "c0de0000000000000000000000000000000000a1"
	besideY = "c0de0000000000000000000000000000000000b2"
)

// besideRoles are the four ready claims of the issue's pod these tests ask.
var besideRoles = []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleArchitect}

// newBesideRig is the pod-update rig with the ready claims of besideRoles.
func newBesideRig(t *testing.T) *podUpdateRig {
	t.Helper()
	workers := make([]podUpdateRole, len(besideRoles))
	for i, role := range besideRoles {
		workers[i] = readyWorker(role)
	}
	return newPodUpdateRig(t, workers...)
}

// besideBranch is the branch beside fix 1 of task a of the rig's issue that role's step pushes to
// (claim.BesideBranch), which claim.IsBesideBranch accepts.
func besideBranch(t *testing.T, role claim.Role) string {
	t.Helper()
	branch, err := claim.BesideBranch(podUpdateIssue, 1, "a", 1, role)
	if err != nil || !claim.IsBesideBranch(branch) {
		t.Fatalf("BesideBranch(%s, 1, a, 1, %s) = %q, %v; want a branch beside the fix", podUpdateIssue, role, branch, err)
	}
	return branch
}

// pushOf is the push an ask's row carries.
func pushOf(ask record.UpdateAsk) supervise.UpdatePush {
	return supervise.UpdatePush{Branch: ask.Branch, OwnFix: ask.OwnFix, FixReads: ask.FixReads}
}

// besidePlace is place of task a, a step of the walk.
func besidePlace(place int) record.StepPlace {
	return record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}
}

// inTx runs write in one transaction over the rig's database, failing the test on its error.
func (r *podUpdateRig) inTx(what string, write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, write); err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// seedBesideTask writes the plan of the rig's issue: task a, which fixes its CI, with the steps
// implementer, tester, reviewer and the owner's acceptance, standing at place in phase p, and its
// pull request open at besideX.
func (r *podUpdateRig) seedBesideTask(place int, p phase.Phase) {
	r.t.Helper()
	r.movesTo(p)
	r.inTx("seed task a", func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: podUpdateIssue, Generation: 1, Version: 1, ArtifactID: "plan-" + podUpdateIssue,
			Source: record.PlanSourceOwner, GivenBy: "owner", GivenAt: podUpdateStart.Add(-time.Hour)}
		tasks := []record.PlanTask{{Issue: podUpdateIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.",
			State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 4, Role: "acceptance", AgentKey: "owner", Type: "review"},
		}
		if err := r.records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := r.records.MoveTask(ctx, tx, podUpdateIssue, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: place}, record.TaskRunning); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: podUpdateIssue,
			Repo: "acme/widgets", Number: 42, Branch: claim.IssueBranch(podUpdateIssue), HeadSHA: besideX, Failing: []string{}})
	})
}

// openFix writes fix 1 of task a for the check k-ci failed at besideX, at the implementer's place:
// reading its failed job, or running at the implementer's entry 2.
func (r *podUpdateRig) openFix(state record.FixState) {
	r.t.Helper()
	r.inTx("open fix 1", func(ctx context.Context, tx pgx.Tx) error {
		fix := record.TaskFix{Issue: podUpdateIssue, Generation: 1, Task: "a", Fix: 1, State: record.FixReading,
			Check: "k-ci", Head: besideX, Place: 1}
		if err := r.records.PutFix(ctx, tx, fix); err != nil || state == record.FixReading {
			return err
		}
		started := podUpdateStart.Add(-10 * time.Minute)
		fix.State, fix.Entry, fix.StartedAt = record.FixRunning, 2, &started
		return r.records.UpdateFix(ctx, tx, fix)
	})
}

// seededStart counts one start of place for role's claim with the store's writers, open, and, where
// branch is not "", binds it beside fix 1 to branch (MarkStepBesideFix, SetStepPushBranch).
func (r *podUpdateRig) seededStart(place int, step, typ string, role claim.Role, branch string) {
	r.t.Helper()
	at := besidePlace(place)
	started := podUpdateStart.Add(-5 * time.Minute)
	r.inTx("count a start of "+step, func(ctx context.Context, tx pgx.Tx) error {
		var starts int64
		if _, err := r.records.RaiseStepStarts(ctx, tx, podUpdateIssue, at, func(count int64) record.StepRecord {
			starts = count
			return record.StepRecord{ID: workflow.StepID(podUpdateIssue, 1, at, count), Task: "a", Step: place, Kind: record.PlaceStep,
				Role: step, Type: typ, Agent: string(r.tokens[role]), Generation: 1, StartedAt: &started}
		}); err != nil || branch == "" {
			return err
		}
		if err := r.records.MarkStepBesideFix(ctx, tx, podUpdateIssue, at, starts, 1); err != nil {
			return err
		}
		return r.records.SetStepPushBranch(ctx, tx, podUpdateIssue, at, starts, branch)
	})
}

// askedPushes opens the update round over the rig's pod and runs the sweep that asks, and answers
// each row the sweep wrote by its role. It fails where a role of roles is asked other than once,
// so an ask that wrote no row is no pass.
func (r *podUpdateRig) askedPushes(roles ...claim.Role) map[claim.Role]record.UpdateAsk {
	r.t.Helper()
	r.drifts(podUpdateDrift)
	r.tick()
	for _, role := range roles {
		r.awaitRound(role, podUpdateRound)
	}
	r.clock.advance(time.Minute)
	r.tick()
	asks := map[claim.Role]record.UpdateAsk{}
	counted := map[claim.Role]int{}
	for _, ask := range r.asks() {
		asks[ask.Role] = ask
		counted[ask.Role]++
	}
	for _, role := range roles {
		if counted[role] != 1 {
			r.t.Fatalf("the sweep asked the %s %d times, want once; it asked %v", role, counted[role], counted)
		}
	}
	return asks
}

// wantPushes holds each row of asks to the push want names for its role, and each branch it names
// to a branch beside a fix, which no reader takes for the issue's or a task's branch.
func wantPushes(t *testing.T, asks map[claim.Role]record.UpdateAsk, want map[claim.Role]supervise.UpdatePush) {
	t.Helper()
	for _, role := range besideRoles {
		got := pushOf(asks[role])
		if got != want[role] {
			t.Errorf("the ask to the %s carries %+v, want %+v", role, got, want[role])
		}
		if got.Branch != "" && !claim.IsBesideBranch(got.Branch) {
			t.Errorf("the ask to the %s names the branch %s, want a branch beside the fix", role, got.Branch)
		}
	}
}

// An update round over the pod of an issue in reviewing, where fix 1 of its task runs beside the
// review: the implementer's row holds the fix's own branch F with OwnFix, the tester's and the
// owner's rows each hold that step's own branch beside the fix, and the reviewer's, which pushes
// nothing, holds none. While fix 1 still reads its failed job, the implementer's row holds FixReads
// and no branch, since neither the fix's copy nor F exists yet, and every other row is as it was.
//
// The open bound starts of the tester and of the owner are seeded with the store's writers: a
// walk at this base ends the tester's run before the review starts (startStep's settleStart), so
// no walk stands with all three open. TestAnUpdateAfterTheFixEndsKeepsEachOpenStepsBranch drives
// each open step's start through the engine instead.
//
// The controls, kept: the same sweep for an issue with no fix writes each row with the zero push,
// main's ask; and with no read of the pushes (updatePushes nil, a deployment with no workflow)
// every role is still asked, each with the zero push.
func TestAnUpdateRoundBesideARunningFixAsksEachRoleToPushToItsOwnBranch(t *testing.T) {
	t.Parallel()
	fixBranch := besideBranch(t, claim.RoleImplementer)
	tester, owner := besideBranch(t, claim.RoleTester), besideBranch(t, claim.RoleArchitect)
	beside := func(t *testing.T, state record.FixState) *podUpdateRig {
		t.Helper()
		rig := newBesideRig(t)
		rig.seedBesideTask(3, phase.Reviewing)
		rig.openFix(state)
		rig.seededStart(2, "tester", "testing", claim.RoleTester, tester)
		rig.seededStart(3, "reviewer", "review", claim.RoleReviewer, "")
		rig.seededStart(4, "acceptance", "review", claim.RoleArchitect, owner)
		return rig
	}
	others := map[claim.Role]supervise.UpdatePush{
		claim.RoleTester:    {Branch: tester},
		claim.RoleArchitect: {Branch: owner},
		claim.RoleReviewer:  {},
	}
	with := func(implementer supervise.UpdatePush) map[claim.Role]supervise.UpdatePush {
		want := map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: implementer}
		for role, push := range others {
			want[role] = push
		}
		return want
	}

	t.Run("fix 1 runs", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixRunning)
		wantPushes(t, rig.askedPushes(besideRoles...), with(supervise.UpdatePush{Branch: fixBranch, OwnFix: true}))
	})
	t.Run("fix 1 reads its failed job", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixReading)
		wantPushes(t, rig.askedPushes(besideRoles...), with(supervise.UpdatePush{FixReads: true}))
	})
	t.Run("control: an issue with no fix asks every role with the zero push", func(t *testing.T) {
		t.Parallel()
		rig := newBesideRig(t)
		rig.seedBesideTask(3, phase.Reviewing)
		rig.seededStart(3, "reviewer", "review", claim.RoleReviewer, "")
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
	t.Run("control: with no read of the pushes every role is asked with the zero push", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixRunning)
		rig.s.updatePushes = nil
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
}

// startStanding writes the start of the step the rig's issue stands in through the engine
// (workflow.(*Engine).StartStepRecord), in a transaction opened and settled as a fact's is: the
// start a move into that step writes, which binds that step to its own branch beside a fix that
// reads or runs.
func (r *podUpdateRig) startStanding() {
	r.t.Helper()
	issue := r.issue()
	r.inTx("start the step of "+string(issue.Phase), func(ctx context.Context, tx pgx.Tx) error {
		fact := r.engine.OpenFact(ctx)
		if err := r.engine.StartStepRecord(fact, tx, issue); err != nil {
			return err
		}
		return r.engine.SettleFact(fact, tx)
	})
}

// endFix ends fix 1, which runs, at besideY, passed, with its way still to take (Way 0).
func (r *podUpdateRig) endFix() {
	r.t.Helper()
	r.inTx("end fix 1", func(ctx context.Context, tx pgx.Tx) error {
		fix, err := r.records.RunningFix(ctx, tx, podUpdateIssue, 1, "a")
		if err != nil {
			return err
		}
		if fix == nil || fix.State != record.FixRunning {
			return fmt.Errorf("fix 1 of task a = %+v, want it running", fix)
		}
		ended := podUpdateStart.Add(-time.Minute)
		fix.State, fix.Result, fix.EndHead, fix.EndedAt = record.FixEnded, "passed", besideY, &ended
		return r.records.UpdateFix(ctx, tx, *fix)
	})
}

// latestFix is the latest fix of task a (record.Store.LatestFix), and fails where it has none.
func (r *podUpdateRig) latestFix() record.TaskFix {
	r.t.Helper()
	var fix *record.TaskFix
	r.inTx("read the latest fix", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fix, err = r.records.LatestFix(ctx, tx, podUpdateIssue, 1, "a")
		return err
	})
	if fix == nil {
		r.t.Fatalf("task a of %s holds no fix", podUpdateIssue)
	}
	return *fix
}

// startAt is the latest start of place of task a (record.Store.StepStarts).
func (r *podUpdateRig) startAt(place int) record.StepStart {
	r.t.Helper()
	var start record.StepStart
	r.inTx(fmt.Sprintf("read the start of place %d", place), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		start, err = r.records.StepStarts(ctx, tx, podUpdateIssue, besidePlace(place))
		return err
	})
	return start
}

// endStart ends the open run of place at besideX, as the end of its step does
// (record.Store.EndStepStart), and fails where the place has no open run.
func (r *podUpdateRig) endStart(place int) {
	r.t.Helper()
	r.inTx(fmt.Sprintf("end the run of place %d", place), func(ctx context.Context, tx pgx.Tx) error {
		_, ended, err := r.records.EndStepStart(ctx, tx, podUpdateIssue, besidePlace(place), besideX)
		if err == nil && !ended {
			err = fmt.Errorf("place %d of task a has no open run to end", place)
		}
		return err
	})
}

// entryOf is the entry role's phases row of the rig's issue names, 0 where the issue has no row
// of role.
func (r *podUpdateRig) entryOf(role claim.Role) int64 {
	r.t.Helper()
	var entry int64
	r.inTx("read the phases rows", func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.records.Phases(ctx, tx, podUpdateIssue)
		for _, row := range rows {
			if row.Role == role {
				entry = row.Entry
			}
		}
		return err
	})
	return entry
}

// besideOrigin is the repository on GitHub that the issue's pods clone, bare: its default branch
// main, the base, and the issue's branch, a commit of the task over it.
func besideOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin, seed := filepath.Join(dir, "widgets.git"), filepath.Join(dir, "seed")
	git := fixturegit.Isolated(t, t.TempDir())
	git.Run(dir, fixturegit.InitBare(origin, "main")...)
	git.Run(dir, fixturegit.InitWorkTree(seed, "main")...)
	for _, file := range []struct{ name, body, message string }{
		{"README.md", "widgets\n", "chore: the base"},
		{"feature.txt", "the task's change\n", "feat: the task's change"},
	} {
		if err := os.WriteFile(filepath.Join(seed, file.name), []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
		git.Run(seed, "git", "add", file.name)
		git.Run(seed, "git", "commit", "--quiet", "-m", file.message)
	}
	git.Run(seed, "git", "push", "--quiet", origin, "HEAD~1:refs/heads/main", "HEAD:refs/heads/"+claim.IssueBranch(podUpdateIssue))
	return origin
}

// besidePod is one pod of the issue as a node provisions it from origin, with the real git and
// jj: a clone that holds origin's main and the issue's branch and nothing else of origin,
// colocated with jj, and the issue's workspace of it, LEGION_WORKSPACE, at the issue's branch. An
// update round deletes the pod and its disk, so the pod after the round is a new besidePod.
type besidePod struct {
	t                          *testing.T
	jj, home, clone, workspace string
}

func newBesidePod(t *testing.T, jj, origin string) *besidePod {
	t.Helper()
	state := t.TempDir()
	p := &besidePod{t: t, jj: jj, home: t.TempDir(),
		clone:     filepath.Join(state, "repos", "github.com", "acme", "widgets"),
		workspace: filepath.Join(state, "workspaces", "acme", "widgets", strings.ToLower(podUpdateIssue))}
	for _, dir := range []string{p.clone, filepath.Dir(p.workspace)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := fixturegit.Isolated(t, p.home)
	git.Run(p.clone, fixturegit.InitWorkTree("", "main")...)
	git.Run(p.clone, "git", "remote", "add", "origin", origin)
	git.Run(p.clone, "git", "fetch", "--quiet", "origin", "main", claim.IssueBranch(podUpdateIssue))
	git.With("JJ_USER=setup", "JJ_EMAIL=setup@example.test").Run(p.clone, append([]string{jj}, fixturegit.JJInit(p.clone, true)[1:]...)...)
	p.jjIn(p.clone, "workspace", "add", p.workspace, "--name", filepath.Base(p.workspace), "--revision", claim.IssueBranch(podUpdateIssue)+"@origin")
	return p
}

// shell runs command with sh in the pod's workspace, as an agent's shell runs a command its text
// gives it: LEGION_WORKSPACE names the workspace and the real jj is first on PATH. It holds the
// command's exit status to 0.
func (p *besidePod) shell(command string) {
	p.t.Helper()
	run := exec.Command("sh", "-c", command)
	run.Dir = p.workspace
	run.Env = append(fixtureenv.Isolated(p.home), "JJ_USER=setup", "JJ_EMAIL=setup@example.test",
		"LEGION_WORKSPACE="+p.workspace, "PATH="+filepath.Dir(p.jj)+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := run.CombinedOutput()
	p.t.Logf("%s:\n%s", command, output)
	if err != nil {
		p.t.Fatalf("%s in the pod's workspace: %v\n%s", command, err, output)
	}
}

// jjIn runs the real jj on the repository of dir, in dir, and fails the test where it fails.
func (p *besidePod) jjIn(dir string, args ...string) string {
	p.t.Helper()
	command := exec.Command(p.jj, append(args, "-R", dir)...)
	command.Dir = dir
	command.Env = append(fixtureenv.Isolated(p.home), "JJ_USER=setup", "JJ_EMAIL=setup@example.test")
	output, err := command.CombinedOutput()
	if err != nil {
		p.t.Fatalf("jj %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

// commit writes name in the workspace's working copy with body and commits it there, as a role's
// step does.
func (p *besidePod) commit(name, body string) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.workspace, name), []byte(body+"\n"), 0o644); err != nil {
		p.t.Fatal(err)
	}
	p.jjIn(p.workspace, "commit", "-m", body)
}

// remove deletes the pod's clone and workspace, as the update round deletes the pod's disk.
func (p *besidePod) remove() {
	p.t.Helper()
	for _, dir := range []string{p.workspace, p.clone} {
		if err := os.RemoveAll(dir); err != nil {
			p.t.Fatal(err)
		}
	}
}

// An update round after fix 1 ended at Y, with its way still to take (Way 0), keeps the branch of
// each step that is still open beside it: the tester, whose start the engine bound to its branch B
// while the fix ran, still tests at X, and its row holds B; and, as a case of its own, the owner
// in accepting, whose phases row raised no entry and whose named acceptance start is open, holds
// its own B. The ended fix's implementer holds neither OwnFix nor FixReads, and no branch. Each
// step's work survives the round: in the pod before the round the step commits its work and runs
// the command its row's branch names (workspace.BesidePushCommand), the pod goes, and a new pod's
// clone, which holds nothing of B, fetches B and finds that work.
//
// It was run red with the engine's seam updateByRunningFix, which answers a branch only from a
// fix that reads or runs: after the fix ended, both open steps' rows lost B.
//
// The controls, kept: a start with no fix binds no branch, so every row holds the zero push; the
// run of the tester that ended keeps its bound branch on its row, and no row names it; and a
// later start of that place, beside no fix, takes no binding of the ended one.
func TestAnUpdateAfterTheFixEndsKeepsEachOpenStepsBranch(t *testing.T) {
	t.Parallel()
	jj := fixturegit.CompiledJJ(t)
	prefix, err := claim.BesideTaskPrefix(podUpdateIssue, 1, "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		place int
		at    phase.Phase
		role  claim.Role
		file  string
	}{
		{name: "the tester still tests at X", place: 2, at: phase.Testing, role: claim.RoleTester, file: "red_test.txt"},
		{name: "the accepting owner, whose phases row raised no entry", place: 4, at: phase.Accepting, role: claim.RoleArchitect, file: "acceptance.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newBesideRig(t)
			rig.seedBesideTask(tc.place, tc.at)
			rig.openFix(record.FixRunning)
			rig.startStanding()
			rig.inTx("probe: bind the start as the engine will", func(ctx context.Context, tx pgx.Tx) error {
				at := besidePlace(tc.place)
				start, err := rig.records.StepStarts(ctx, tx, podUpdateIssue, at)
				if err != nil {
					return err
				}
				if err := rig.records.MarkStepBesideFix(ctx, tx, podUpdateIssue, at, start.Starts, 1); err != nil {
					return err
				}
				return rig.records.SetStepPushBranch(ctx, tx, podUpdateIssue, at, start.Starts, besideBranch(t, tc.role))
			})
			rig.endFix()
			if fix := rig.latestFix(); fix.State != record.FixEnded || fix.EndHead != besideY || fix.Way != 0 {
				t.Fatalf("fix 1 after its end = %+v, want it ended at %s with its way still to take (Way 0)", fix, besideY)
			}
			own := besideBranch(t, tc.role)
			if start := rig.startAt(tc.place); start.Run != record.RunOpen || start.PushBranch != own {
				t.Fatalf("the start of the %s after fix 1 ended = %+v, want it open and bound to %s", tc.role, start, own)
			}
			if entry := rig.entryOf(tc.role); tc.role == claim.RoleArchitect && entry != 0 {
				t.Fatalf("the owner's phases row names entry %d, want none raised", entry)
			}

			asks := rig.askedPushes(besideRoles...)
			wantPushes(t, asks, map[claim.Role]supervise.UpdatePush{tc.role: {Branch: own}})
			branch := asks[tc.role].Branch
			if branch == "" {
				t.Fatalf("the ask to the %s names no branch, so its work has nowhere to go before the pod does", tc.role)
			}

			origin := besideOrigin(t)
			work := "the " + string(tc.role) + "'s work at X"
			before := newBesidePod(t, jj, origin)
			before.commit(tc.file, work)
			before.shell(workspace.BesidePushCommand(branch))
			before.remove()
			after := newBesidePod(t, jj, origin)
			if held := after.jjIn(after.workspace, "log", "-r", `remote_bookmarks(glob:"`+prefix+`*")`, "--no-graph", "-T", `commit_id ++ "\n"`); held != "" {
				t.Fatalf("the new pod's clone holds a branch beside the fix before any fetch:\n%s", held)
			}
			after.shell(`jj -R "$LEGION_WORKSPACE" git fetch --remote origin --branch ` + branch)
			if body := after.jjIn(after.workspace, "file", "show", "-r", branch+"@origin", tc.file); body != work {
				t.Errorf("the new pod fetches %s holding %s as %q, want %q", branch, tc.file, body, work)
			}
		})
	}
	t.Run("control: a start with no fix binds no branch", func(t *testing.T) {
		t.Parallel()
		rig := newBesideRig(t)
		rig.seedBesideTask(2, phase.Testing)
		rig.startStanding()
		if start := rig.startAt(2); start.Starts != 1 || start.Run != record.RunOpen || start.PushBranch != "" {
			t.Fatalf("the tester's start with no fix = %+v, want start 1 open and bound to no branch", start)
		}
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
	t.Run("control: the tester's ended run names its branch on no row", func(t *testing.T) {
		t.Parallel()
		rig := newBesideRig(t)
		rig.seedBesideTask(2, phase.Testing)
		rig.openFix(record.FixRunning)
		rig.startStanding()
		rig.endFix()
		rig.endStart(2)
		own := besideBranch(t, claim.RoleTester)
		if start := rig.startAt(2); start.Run != record.RunEnded || start.PushBranch != own {
			t.Fatalf("the tester's ended start = %+v, want it ended with its binding %s still on it", start, own)
		}
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
	t.Run("a later start of the tester's place takes no binding of the ended one", func(t *testing.T) {
		t.Parallel()
		rig := newBesideRig(t)
		rig.seedBesideTask(2, phase.Testing)
		rig.openFix(record.FixRunning)
		rig.startStanding()
		rig.endFix()
		rig.endStart(2)
		rig.startStanding()
		if start := rig.startAt(2); start.Starts != 2 || start.Run != record.RunOpen || start.PushBranch != "" {
			t.Fatalf("the tester's later start = %+v, want start 2 open and bound to no branch", start)
		}
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
}
