package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// These tests run the owners whose success work follows a write of theirs — the backfill's two
// writes, the boot's hold of recorded session names, the idle and unowned releases, the cancel of
// a release that did not finish and the wake of a released pod — over a real PostgreSQL through a
// proxy that cuts one transaction's commit (testdb.CommitFaults). An owner whose commit landed
// keeps its success work once, whether its reply came back or the store said later that it
// committed; an owner whose commit never landed runs none of it, and its retry does.

// ownerRig is one owner of success work, run through a faulted pool or store.
type ownerRig struct {
	fault *testdb.CommitFault
	// run runs the owner once and answers its failure: the error an owner returns, or the warning
	// an owner that returns nothing logs in its place.
	run func(t *testing.T) error
	// changed reads, through the direct pool, whether the owner's durable change is there.
	changed func(t *testing.T) bool
	// work counts each kind of success work the owner's commit runs, by name.
	work func(t *testing.T) map[string]int
}

// ownerFamily is an owner, the SQL its write sends, which the fault matches, and how its rig is
// built. retried is whether a second run after a committed one runs the owner again: a cancel
// that committed leaves no row behind it to run again. unknown, where set, is the family's run
// whose outcome the store cannot say until it answers again.
type ownerFamily struct {
	name    string
	match   string
	retried bool
	open    func(t *testing.T) *ownerRig
	unknown func(t *testing.T)
}

func ownerFamilies() []ownerFamily {
	return []ownerFamily{
		{name: "backfill fill", match: "update issues set repo =", retried: true, open: backfillOwner("fill")},
		{name: "backfill name", match: "update issues set github_repo =", retried: true, open: backfillOwner("name")},
		{name: "refresher backfill", match: "update issues set repo =", retried: true, open: backfillLaterOwner, unknown: backfillLaterUnknown},
		{name: "rename write", match: renameDeleteMatch, retried: true, open: renameWriteOwner, unknown: renameWriteUnknown},
		{name: "boot holds", match: "insert into claim_sessions", retried: true, open: bootHoldsOwner},
		{name: "idle release", match: "insert into issue_releases", retried: true, open: idleReleaseOwner},
		{name: "unowned release", match: "insert into issue_releases", retried: true, open: unownedReleaseOwner, unknown: unownedReleaseUnknown},
		{name: "release cancel", match: "delete from issue_releases", open: releaseCancelOwner, unknown: releaseCancelUnknown},
		{name: "left pod close", match: "insert into outbox", open: leftPodCloseOwner, unknown: leftPodCloseUnknown},
		{name: "news settle", match: newsSettleMatch, retried: true, open: newsSettleOwner, unknown: newsSettleUnknown},
	}
}

// An owner whose commit's reply is lost keeps its success work: the store says the transaction
// committed, so the work runs once, and a retry that finds the change made runs it no second
// time. A commit that never landed runs none of it, and the retry that makes the change runs it
// once.
func TestAnOwnerWhoseCommitReplyIsLostKeepsItsSuccessWork(t *testing.T) {
	t.Parallel()
	for _, family := range ownerFamilies() {
		t.Run(family.name, func(t *testing.T) {
			t.Run("reply_lost", func(t *testing.T) {
				rig := family.open(t)
				replyLost(t, rig, family.match)
			})
			t.Run("rollback", func(t *testing.T) {
				rig := family.open(t)
				fired := rig.fault.Cut(family.match, testdb.CutBeforeCommit)
				err := rig.run(t)
				testdb.Met(t, fired)
				if err == nil {
					t.Errorf("the owner whose commit never landed = nil, want its failure")
				}
				if rig.changed(t) {
					t.Fatalf("the change of a commit that never landed is there")
				}
				wantWork(t, rig, 0, "after a commit that never landed")
				if err := rig.run(t); err != nil {
					t.Fatalf("the retry with no fault = %v, want nil", err)
				}
				if !rig.changed(t) {
					t.Fatalf("the retry with no fault made no change")
				}
				wantWork(t, rig, 1, "after the retry that made the change")
			})
			if family.retried {
				t.Run("duplicate_retry", func(t *testing.T) {
					rig := family.open(t)
					replyLost(t, rig, family.match)
					if err := rig.run(t); err != nil {
						t.Errorf("the retry after a committed write = %v, want nil", err)
					}
					if !rig.changed(t) {
						t.Fatalf("the change is gone after the retry")
					}
					wantWork(t, rig, 1, "committed and its retry succeeded, but")
				})
			}
			if family.unknown != nil {
				t.Run("unknown", family.unknown)
			}
		})
	}
}

// replyLost runs the owner with its commit's reply cut after the commit landed, and wants the
// owner to answer nil with its change made and its success work done once.
func replyLost(t *testing.T, rig *ownerRig, match string) {
	t.Helper()
	fired := rig.fault.Cut(match, testdb.CutAfterCommit)
	err := rig.run(t)
	testdb.Met(t, fired)
	if err != nil {
		t.Errorf("the owner whose commit landed and whose reply was lost = %v, want nil", err)
	}
	if !rig.changed(t) {
		t.Fatalf("the commit whose reply was lost made no change: the cut came before the commit")
	}
	wantWork(t, rig, 1, "committed with its reply lost, but")
}

// wantWork fails the test for each kind of success work not counted want times.
func wantWork(t *testing.T, rig *ownerRig, want int, when string) {
	t.Helper()
	counts := rig.work(t)
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if counts[name] != want {
			t.Errorf("%s %s = %d, want %d", when, name, counts[name], want)
		}
	}
}

// count is how many times logs holds phrase.
func count(logs *syncBuffer, phrase string) int {
	return strings.Count(logs.String(), phrase)
}

// debugLog is watchedLog at Debug, for an owner whose success line is a Debug line.
func debugLog() (*syncBuffer, *slog.Logger) {
	logs := &syncBuffer{}
	return logs, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// recordLaneWakes has w's wakes of issue's lifecycle lane land on a lane the test holds, and
// answers how many have landed: the lane's channel holds one entry per wake, up to its size.
func recordLaneWakes(w *workflowRuntime, issue string) func() int {
	return recordWakes(w, record.LaneKey{Issue: issue, Lane: record.LaneOf(record.OutboxKindSupervise)})[0]
}

// recordWakes has w's wakes of each lane of keys land on a lane the test holds, and answers how
// many have landed on each, in keys' order. A wake of any other lane would start a lane of the
// recording outbox, which runs nothing, so a test records every lane its owner wakes.
func recordWakes(w *workflowRuntime, keys ...record.LaneKey) []func() int {
	lanes := make(map[record.LaneKey]*outboxLane, len(keys))
	landed := make([]func() int, len(keys))
	for i, key := range keys {
		lane := &outboxLane{key: key, wake: make(chan struct{}, 16)}
		lanes[key] = lane
		landed[i] = func() int { return len(lane.wake) }
	}
	w.outboxWake.Store(&outbox{running: true, lanes: lanes})
	return landed
}

// backfillOwner is the backfill's fill or name of one record's repository, the owner the boot
// runs before it subscribes to any repository.
func backfillOwner(kind string) func(t *testing.T) *ownerRig {
	return func(t *testing.T) *ownerRig {
		direct, faulted, fault := faultedPools(t)
		ctx := context.Background()
		repo, branch := "", ""
		if kind == "name" {
			repo, branch = "acme/widgets", "main"
		}
		if _, err := direct.Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
			values ('LEGION-REVIEW', 'LEGION-REVIEW', 'LEGION', 'review', 'planning', 1, 'in_progress', 'A', 1, $1, $2)`, repo, branch); err != nil {
			t.Fatalf("seed the record: %v", err)
		}
		github := newRepositoryGitHub(t, "main")
		github.fullName = "Acme/Widgets"
		named := newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)
		client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-REVIEW", ResolvedRepo: "acme/widgets"}}}
		logs, log := watchedLog()
		records := record.NewStore()
		state := t.TempDir()
		phrase := "legion workflow backfill filled"
		if kind == "name" {
			phrase = "legion workflow backfill named"
		}
		return &ownerRig{
			fault: fault,
			run: func(*testing.T) error {
				var err error
				if kind == "fill" {
					_, err = fillRepositories(ctx, faulted, records, client, "LEGION", state, named, ghrepo.Renames{}, productionRepositoryReads(), log)
				} else {
					_, err = nameRepositories(ctx, faulted, records, "LEGION", named, ghrepo.Renames{}, productionRepositoryReads(), log)
				}
				return err
			},
			changed: func(t *testing.T) bool {
				var spelled string
				if err := direct.QueryRow(ctx, "select github_repo from issues where key = 'LEGION-REVIEW'").Scan(&spelled); err != nil {
					t.Fatalf("read the record: %v", err)
				}
				return spelled == "Acme/Widgets"
			},
			work: func(*testing.T) map[string]int { return map[string]int{"journal lines": count(logs, phrase)} },
		}
	}
}

// bootHoldsOwner is the boot's hold of the session names claims recorded while no daemon held
// them: the rig's claim records names the table holds for no one.
func bootHoldsOwner(t *testing.T) *ownerRig {
	rig := newIdleRig(t)
	ctx := context.Background()
	if _, err := rig.pool.Exec(ctx, "delete from claim_sessions"); err != nil {
		t.Fatalf("stand for a boot before session-name holds: %v", err)
	}
	faulted, fault := faultedStoreOf(t, rig.st)
	logs, log := watchedLog()
	s := &supervision{supervisor: &supervisor{store: faulted}, plan: plan{project: "legion"}, log: log}
	held := func(t *testing.T) int {
		var names int
		if err := rig.pool.QueryRow(ctx, "select count(*) from claim_sessions").Scan(&names); err != nil {
			t.Fatalf("count the held names: %v", err)
		}
		return names
	}
	return &ownerRig{
		fault:   fault,
		run:     func(*testing.T) error { return s.holdRecordedSessions(ctx) },
		changed: func(t *testing.T) bool { return held(t) > 0 },
		work: func(t *testing.T) map[string]int {
			lines := 0
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "held session names claims recorded") && strings.Contains(line, fmt.Sprintf("names=%d", held(t))) {
					lines++
				}
			}
			return map[string]int{"journal lines naming the names the insert added": lines}
		},
	}
}

// idleReleaseOwner is the idle pass's release of the rig's quiet pod.
func idleReleaseOwner(t *testing.T) *ownerRig {
	rig := newIdleRig(t)
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.w.idleStore = faulted
	logs, log := watchedLog()
	rig.w.log = log
	wakes := recordLaneWakes(rig.w, rig.issue.Key)
	return &ownerRig{
		fault: fault,
		run: func(*testing.T) error {
			before := count(logs, "idle release: could not decide an issue's pod")
			rig.w.releaseIdlePod(context.Background(), rig.issue.Key)
			if count(logs, "idle release: could not decide an issue's pod") > before {
				return errors.New("the idle pass could not decide the pod")
			}
			return nil
		},
		changed: func(t *testing.T) bool { return rig.marked(t) },
		work: func(*testing.T) map[string]int {
			return map[string]int{
				"release journal lines": count(logs, "idle release: releasing an issue's pod,"),
				"lifecycle lane wakes":  wakes(),
			}
		},
	}
}

// unownedReleaseOwner is the pass over pods no claim owns, releasing the rig's pod, whose one
// claim failed.
func unownedReleaseOwner(t *testing.T) *ownerRig {
	rig := newIdleRig(t)
	rig.stoppedImplementer(t, false)
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.w.idleStore = faulted
	logs, log := debugLog()
	rig.w.log = log
	wakes := recordLaneWakes(rig.w, rig.issue.Key)
	return &ownerRig{
		fault: fault,
		run: func(*testing.T) error {
			before := count(logs, "unowned release: could not decide an issue's pod")
			rig.w.releaseUnownedPods(context.Background())
			if count(logs, "unowned release: could not decide an issue's pod") > before {
				return errors.New("the unowned pass could not decide the pod")
			}
			return nil
		},
		changed: func(t *testing.T) bool { return rig.marked(t) },
		work: func(*testing.T) map[string]int {
			return map[string]int{
				"release journal lines":                    count(logs, "unowned release: releasing an issue's pod"),
				"lifecycle lane wakes":                     wakes(),
				"pass summaries with decided=1 released=1": count(logs, "decided=1 released=1"),
			}
		},
	}
}

// cancelRig is an idle rig whose outbox has its store and pool swapped together to faulted, and its
// log read by the test (faultedRig). newCancelRig builds it over a marked release whose pod is given
// work after its mark, so the release's workspace check cancels it (outbox.cancelRelease,
// workflow.IdleHoldOvertaken); newLeftCloseRig builds it over a left pod's close.
type cancelRig struct {
	*idleRig
	fault *testdb.CommitFault
	logs  *syncBuffer
	runs  int
}

func newCancelRig(t *testing.T) *cancelRig {
	rig := newIdleRig(t)
	rig.w.releaseIdlePod(context.Background(), rig.issue.Key)
	if !rig.marked(t) {
		t.Fatal("the idle pass marked no release")
	}
	// A turn of the implementer after the mark: the check finds the pod given work since, and
	// cancels the release before it reads anything (store.IssueReleaseCheck, releaseOvertaken).
	if _, err := rig.pool.Exec(context.Background(), `update claims set last_turn_start = now() where issue = $1`,
		rig.issue.Key); err != nil {
		t.Fatalf("record a turn after the mark: %v", err)
	}
	return faultedRig(t, rig)
}

// faultedRig swaps the outbox's store and pool of rig together to faulted (faultedStoreOf), and
// its log to one the test reads.
func faultedRig(t *testing.T, rig *idleRig) *cancelRig {
	t.Helper()
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.runner.trees, rig.runner.pool, rig.runner.supervisor.store = faulted, faulted.Pool(), faulted
	logs, log := watchedLog()
	rig.runner.log = log
	return &cancelRig{idleRig: rig, fault: fault, logs: logs}
}

// later moves the runner's clock past every backoff and lease the runs before it left, so the
// next drain runs the check row again.
func (r *cancelRig) later() {
	r.runs++
	offset := time.Duration(r.runs) * 5 * time.Minute
	r.runner.now = func() time.Time { return time.Now().Add(offset) }
}

// holdRows is how many hold records of the cancelled release the pod has.
func (r *cancelRig) holdRows(t *testing.T) int {
	var holds int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from pod_holds where issue = $1 and reason = $2`,
		r.issue.Key, workflow.IdleHoldOvertaken.String()).Scan(&holds); err != nil {
		t.Fatalf("count the hold records: %v", err)
	}
	return holds
}

const cancelLine = "outbox: the idle release of an issue's pod did not finish"

// releaseCancelOwner is the cancel of a release whose pod was given work after its mark, called as
// the check row calls it. The row's drain is not the runner here: once the check row has failed,
// the drain runs the release's stop and teardown rows before the check row comes due again, so a
// retry by drain could find the release over and cancel nothing.
func releaseCancelOwner(t *testing.T) *ownerRig {
	rig := newCancelRig(t)
	return &ownerRig{
		fault: rig.fault,
		run: func(*testing.T) error {
			return rig.runner.cancelRelease(context.Background(), rig.issue.Key, rig.issue.Tree,
				workflow.IdleHoldOvertaken, "the pod was given work after its release was marked")
		},
		changed: func(t *testing.T) bool { return !rig.marked(t) },
		work: func(t *testing.T) map[string]int {
			return map[string]int{"cancel journal lines": count(rig.logs, cancelLine), "hold records": rig.holdRows(t)}
		},
	}
}

// An unowned pass whose release's outcome the store cannot say counts no release: its summary
// says released=0 and no line or wake is made yet. Once the store answers, the release's line and
// wake follow, and the pass's one summary stands as written.
func unownedReleaseUnknown(t *testing.T) {
	rig := newIdleRig(t)
	rig.stoppedImplementer(t, false)
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.w.idleStore = faulted
	logs, log := debugLog()
	rig.w.log = log
	wakes := recordLaneWakes(rig.w, rig.issue.Key)
	fired := fault.Cut("insert into issue_releases", testdb.CutAfterCommitRefusing)
	rig.w.releaseUnownedPods(context.Background())
	testdb.Met(t, fired)
	if !strings.Contains(logs.String(), store.ErrCommitOutcomeUnknown.Error()) {
		t.Errorf("the pass's log names no unknown outcome: %q", logs.String())
	}
	if got := count(logs, "decided=1 released=0"); got != 1 {
		t.Errorf("pass summaries with released=0 = %d, want 1: the pass counts only a release its transaction answered", got)
	}
	if got, woken := count(logs, "unowned release: releasing an issue's pod"), wakes(); got != 0 || woken != 0 {
		t.Fatalf("before the store answers: release lines = %d, wakes = %d; want none", got, woken)
	}
	fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the release's line and wake once the store says it committed", func() bool {
			return count(logs, "unowned release: releasing an issue's pod") == 1 && wakes() == 1
		})
	if got := count(logs, "unowned release: decided every pod of the project no claim owns"); got != 1 {
		t.Errorf("pass summaries = %d, want the one the pass wrote", got)
	}
	if got := count(logs, "decided=1 released=1"); got != 0 {
		t.Errorf("summaries with released=1 = %d, want none: no line changes what the pass counted", got)
	}
}

// A cancel whose outcome the store cannot say leaves its check row for the lane, with the hold it
// recorded already committed beside the cancel. Once the store answers, the cancel's line follows,
// and the row's run finds the release over and records no second hold.
func releaseCancelUnknown(t *testing.T) {
	rig := newCancelRig(t)
	fired := rig.fault.Cut("delete from issue_releases", testdb.CutAfterCommitRefusing)
	rig.later()
	// One pass, not the drain: the drain's next list is refused while the store is, and the
	// drain fails the test on a refused list.
	if err := rig.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	testdb.Met(t, fired)
	if !strings.Contains(rig.logs.String(), store.ErrCommitOutcomeUnknown.Error()) {
		t.Errorf("the row's failure names no unknown outcome: %q", rig.logs.String())
	}
	if got := rig.holdRows(t); got != 1 {
		t.Errorf("hold records before the store answers = %d, want the 1 that committed with the cancel", got)
	}
	if got := count(rig.logs, cancelLine); got != 0 {
		t.Errorf("cancel lines before the store answers = %d, want none", got)
	}
	rig.fault.Refuse(false)
	rig.later()
	rig.drain(t)
	if got := count(rig.logs, "outbox release workspace check finished without acting"); got != 1 {
		t.Errorf("check rows that found the release over = %d, want 1", got)
	}
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the cancel's line once the store says it committed", func() bool { return count(rig.logs, cancelLine) == 1 })
	if got := rig.holdRows(t); got != 1 {
		t.Errorf("hold records after the store answered = %d, want 1", got)
	}
}

// leftCloseLine is the line of a left pod's close whose workspace check read nothing that origin
// does not hold, and so queued the close's stops.
const leftCloseLine = "the close of the pod of an issue that left the workflow read its workspace and found nothing"

// leftCloseRig is the rig of a left pod's close (workflow.LeftPodClose) whose workspace check is
// queued and reads nothing that origin does not hold, so its run queues the close's stops and the
// issue's Sandbox suspension (outbox.checkLeftPodWorkspace), with the outbox's store and pool
// swapped together to faulted. row and payload are the check's row as the outbox lists it.
type leftCloseRig struct {
	*cancelRig
	row     record.OutboxRow
	payload record.IssueWorkspaceCheck
}

func newLeftCloseRig(t *testing.T) *leftCloseRig {
	t.Helper()
	rig := newLeftPodRig(t, false)
	row, check := queuedLeftPodCheck(t, rig)
	return &leftCloseRig{cancelRig: faultedRig(t, rig), row: row, payload: check}
}

// leftPodCloseOwner is the clean close's check, run as the lane runs its row. A close that
// committed finished its check row with it, so no row is left to run again.
func leftPodCloseOwner(t *testing.T) *ownerRig {
	rig := newLeftCloseRig(t)
	return &ownerRig{
		fault: rig.fault,
		run: func(*testing.T) error {
			return rig.runner.checkIssueWorkspace(context.Background(), rig.row, rig.payload)
		},
		changed: func(t *testing.T) bool { return slices.Contains(rig.rows(t), "issue_suspend") },
		work: func(t *testing.T) map[string]int {
			return map[string]int{"close journal lines": count(rig.logs, leftCloseLine)}
		},
	}
}

// A clean close whose outcome the store cannot say has queued the close's stops and the issue's
// Sandbox suspension in a transaction that committed. Once the store answers, the lane runs those
// rows, and nothing reads the workspace again: a check row left for its retry would read it after
// the stop took the role's shim away, and decline a close already done, with a workspace_unread
// decline and a hold streak that no pass ends, since no pass visits a pod with no Sandbox.
func leftPodCloseUnknown(t *testing.T) {
	rig := newLeftCloseRig(t)
	fired := rig.fault.Cut("insert into outbox", testdb.CutAfterCommitRefusing)
	rig.later()
	// One pass, not the drain: the drain's next list is refused while the store is, and the
	// drain fails the test on a refused list.
	if err := rig.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	testdb.Met(t, fired)
	if !strings.Contains(rig.logs.String(), store.ErrCommitOutcomeUnknown.Error()) {
		t.Errorf("the row's failure names no unknown outcome: %q", rig.logs.String())
	}
	// The close's stop ends the planner's process, and its shim with it: a read made after that
	// fails, as a read through a gone shim does.
	rig.rt.FailWorkspaceWork(rig.issue.Key, errors.New("the planner's shim is gone"))
	rig.fault.Refuse(false)
	rig.later()
	rig.drain(t)
	if read, _ := rig.rt.reads(); !slices.Equal(read, []claim.Token{rig.token}) {
		t.Errorf("the workspace was read through %v, want the close's one read through the planner", read)
	}
	if rig.declined(t) {
		t.Error("the close that committed was declined after its stop ran")
	}
	if held := holdsOf(t, rig.idleRig, workflow.IdleHoldWorkspaceUnread); len(held) != 0 {
		t.Errorf("holds = %+v, want no %s", held, workflow.IdleHoldWorkspaceUnread)
	}
	for _, left := range rig.rows(t) {
		if left == "issue_bundle" || left == "supervise:suspend" || left == "issue_suspend" {
			t.Errorf("rows after the drain = %v, want the check finished with the close and its stops run", rig.rows(t))
			break
		}
	}
	if got := rig.storedClaim(t, rig.token); got.State != supervise.StateSuspended {
		t.Errorf("the stored planner = %s, want it suspended by the close's stop", got.State)
	}
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the close's line once the store says it committed", func() bool { return count(rig.logs, leftCloseLine) == 1 })
}

// podHoldRow is one pod_holds row as the late-hook test reads it.
type podHoldRow struct {
	reason  string
	ended   bool
	passes  int
	firstAt time.Time
	at      time.Time
}

// A cancel's hook that runs late, after a newer pass recorded a hold of another reason, leaves
// that newer streak as it is: the cancel's hold committed with the cancel, and the hook only logs.
func TestALateCancelHookLeavesANewerHoldStreak(t *testing.T) {
	t.Parallel()
	rig := newCancelRig(t)
	ctx := context.Background()
	fired := rig.fault.Cut("delete from issue_releases", testdb.CutAfterCommitRefusing)
	rig.later()
	if err := rig.runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	testdb.Met(t, fired)
	atB := time.Now().UTC()
	recordIdleHold(ctx, rig.st, "legion", rig.issue.Key, workflow.IdleHoldDelivery, "", atB, quietLogger())
	rig.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the cancel's hook once the store says it committed", func() bool { return count(rig.logs, cancelLine) == 1 })

	want := func(when string) {
		t.Helper()
		rows, err := rig.pool.Query(ctx, `select reason, ended, passes, first_at, at from pod_holds where issue = $1 order by id`, rig.issue.Key)
		if err != nil {
			t.Fatalf("read the hold records: %v", err)
		}
		var got []podHoldRow
		for rows.Next() {
			var row podHoldRow
			if err := rows.Scan(&row.reason, &row.ended, &row.passes, &row.firstAt, &row.at); err != nil {
				t.Fatalf("read a hold record: %v", err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the hold records: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("%s: hold records = %+v, want two: the cancel's, ended, and the newer pass's", when, got)
		}
		if first := got[0]; first.reason != workflow.IdleHoldOvertaken.String() || !first.ended || first.passes != 1 {
			t.Errorf("%s: the first record = %+v, want %s, ended, with one pass", when, first, workflow.IdleHoldOvertaken)
		}
		second := got[1]
		if second.reason != workflow.IdleHoldDelivery.String() || second.ended || second.passes != 1 {
			t.Errorf("%s: the second record = %+v, want %s, running, with one pass", when, second, workflow.IdleHoldDelivery)
		}
		if !second.firstAt.Equal(atB.Truncate(time.Microsecond)) || !second.at.Equal(atB.Truncate(time.Microsecond)) {
			t.Errorf("%s: the newer streak runs from %s to %s, want both %s", when, second.firstAt, second.at, atB)
		}
	}
	want("once the late hook ran")
	rig.later()
	rig.drain(t)
	want("after the check row ran again")
}

// A wake whose take's outcome the store cannot say fails, and its row's retry resumes the claim:
// the take committed, so the retry's take is refused by the open resume the first one recorded,
// and the refused branch resumes a claim still released. The take's line follows the store's
// answer.
func TestAWakeWhoseTakeOutcomeIsUnknownIsResumedByItsRetry(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the release = %s by %s, want it released", got.State, got.SuspendedBy)
	}
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.runner.trees, rig.runner.pool = faulted, faulted.Pool()
	logs, log := watchedLog()
	rig.runner.log = log
	before := len(rig.rt.CallsOf("Resume"))

	fired := fault.Cut("insert into pod_resumes", testdb.CutAfterCommitRefusing)
	_, _, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	testdb.Met(t, fired)
	if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
		t.Fatalf("the wake whose take's outcome is unknown = %v, want store.ErrCommitOutcomeUnknown", err)
	}
	if got := rig.machine.Claim(); !got.Released() {
		t.Fatalf("the claim after the failed wake = %s, want it still released", got.State)
	}
	fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the take's line once the store says it committed", func() bool {
			return count(logs, "wake: a released pod is coming back for a delivery") == 1
		})

	woken, _, err := rig.runner.wakeForDelivery(ctx, rig.token, "a notice of "+rig.issue.Key, 0)
	if err != nil || !woken {
		t.Fatalf("the row's retry = %t, %v; want the claim woken", woken, err)
	}
	if got := count(logs, "a released pod's resume stands open with no process behind it"); got != 1 {
		t.Errorf("open-resume lines = %d, want 1: the retry's take is refused by the resume the first take recorded", got)
	}
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the machine is still released after the retry, want it resumed")
	}
	if resumed := len(rig.rt.CallsOf("Resume")) - before; resumed != 1 {
		t.Errorf("resumes the retry handled = %d, want 1", resumed)
	}
}

// seedStoppedIssue seeds a second issue beside the rig's, whose one claim, an implementer, failed
// half an hour ago: a pod no claim owns, as the rig's own after stoppedImplementer.
func seedStoppedIssue(t *testing.T, rig *idleRig, key string) {
	t.Helper()
	ctx := context.Background()
	issue := rig.issue
	issue.Key, issue.Tree = key, key
	putOutboxIssue(t, rig.pool, rig.issues, issue)
	token := mustClaimToken(t, key, claim.RoleImplementer)
	if err := rig.st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: key, Issue: key,
		Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1, ServingGeneration: 1, State: supervise.StateFailed,
		Session: "ses-" + key, SessionFile: "/tmp/" + key + ".jsonl", PluginContract: rig.machine.Claim().PluginContract}); err != nil {
		t.Fatalf("seed the failed implementer of %s: %v", key, err)
	}
	if _, err := rig.pool.Exec(ctx, `update claims set launch_failures = $2, updated_at = now() - interval '30 minutes' where token = $1`,
		string(token), stoppedImplementerFailures); err != nil {
		t.Fatalf("backdate the failed implementer of %s: %v", key, err)
	}
}

// An unowned pass counts only the releases its own transactions answered: a release whose
// outcome was unknown is not counted, even when the watcher runs its hook while the pass is still
// running. The pass is held open before its second issue while the store answers the first.
func TestAnUnownedPassCountsOnlyTheReleasesItsTransactionsAnswered(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	rig.stoppedImplementer(t, false)
	seedStoppedIssue(t, rig, "LEGION-209")
	faulted, fault := faultedStoreOf(t, rig.st)
	rig.w.idleStore = faulted
	logs, log := debugLog()
	rig.w.log = log
	gate := make(chan struct{})
	reached := make(chan string, 4)
	rig.rt.mu.Lock()
	rig.rt.sandboxGate = map[string]chan struct{}{"LEGION-209": gate}
	rig.rt.sandboxReached = reached
	rig.rt.mu.Unlock()

	fired := fault.Cut("insert into issue_releases", testdb.CutAfterCommitRefusing)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.w.releaseUnownedPods(context.Background())
	}()
	testdb.Met(t, fired)
	bound := time.After(testwait.Bounded(t, 30*time.Second, time.Second))
	for waiting := true; waiting; {
		select {
		case issue := <-reached:
			waiting = issue != "LEGION-209"
		case <-bound:
			close(gate)
			t.Fatal("the pass never reached LEGION-209")
		}
	}
	fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"LEGION-208's release line while the pass is live", func() bool {
			return strings.Contains(logs.String(), `msg="unowned release: releasing an issue's pod, which no claim of it can use" issue=LEGION-208`)
		})
	close(gate)
	select {
	case <-done:
	case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
		t.Fatal("the pass did not return once its gate opened")
	}
	if got := count(logs, "unowned release: decided every pod of the project no claim owns"); got != 1 {
		t.Errorf("pass summaries = %d, want 1", got)
	}
	if got := count(logs, "decided=2 released=1"); got != 1 {
		t.Errorf("summaries with decided=2 released=1 = %d, want 1: the pass counts only the release its transaction answered; log=%q", got, logs.String())
	}
	if got := count(logs, "unowned release: releasing an issue's pod"); got != 2 {
		t.Errorf("release lines = %d, want one per issue", got)
	}
	var marks int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) from issue_releases where kind = 'unowned'`).Scan(&marks); err != nil {
		t.Fatalf("count the marks: %v", err)
	}
	if marks != 2 {
		t.Errorf("unowned marks = %d, want 2", marks)
	}
}

// pauseStopRig is the pause rig's architect stopped once its pause has ended, which owes the role
// a launch: the stop transaction takes a held wake of the pause with its start row and logs that
// row as released (TestAStopThatCommitsAfterItsPauseEndedOwesTheRoleALaunch). The transaction's
// store is swapped to faulted, with the role-holder publication on and the daemon's own wake of it
// (workflowRuntime.wakeRoleHolders), as a daemon with a workflow turns it on: the stop writes the
// architect's claim out of a state that holds its role, so the claim's write queues its issue's
// publication. The issue's lifecycle lane, which the start row's wake reaches, and its board lane,
// which the publication's wake reaches, are recorded.
type pauseStopRig struct {
	*pauseRig
	fault     *testdb.CommitFault
	held      store.Pause
	wakes     func() int
	published func() int
}

func newPauseStopRig(t *testing.T) *pauseStopRig {
	t.Helper()
	r := newPauseRig(t, claim.RoleArchitect)
	held := r.pauseLegion()
	r.resume()
	faulted, fault := faultedStoreOf(t, r.st)
	faulted.PublishRoleHolders(r.pauses.workflow.wakeRoleHolders)
	r.pauses.store = faulted
	landed := recordWakes(r.pauses.workflow,
		record.LaneKey{Issue: pauseIssue, Lane: record.LaneOf(record.OutboxKindSupervise)},
		record.LaneKey{Issue: pauseIssue, Lane: record.LaneOf(record.OutboxKindRoleHoldersPublish)})
	return &pauseStopRig{pauseRig: r, fault: fault, held: held, wakes: landed[0], published: landed[1]}
}

// stop runs the stop transaction as the architect's machine does when it writes its suspension
// for the pause, and again when it makes a stop whose commit's answer it did not get.
func (r *pauseStopRig) stop() error {
	return r.pauses.stopped(r.ctx, r.claim(claim.RoleArchitect, supervise.StateSuspended), supervise.PauseStop{Pause: r.held.ID})
}

// recorded is whether the stop is on record as the transaction writes it: stopped, owing a launch.
func (r *pauseStopRig) recorded() bool {
	stops := r.stops(r.held.ID, claim.RoleArchitect)
	return len(stops) == 1 && stops[0] == (pauseStopRecord{end: store.PauseStopStopped, owed: true})
}

func (r *pauseStopRig) work() map[string]int {
	return map[string]int{"released-row lines": len(r.releasedLines()), "lifecycle lane wakes": r.wakes(),
		"role-holder publication wakes": r.published()}
}

func (r *pauseStopRig) wantWork(t *testing.T, want int, when string) {
	t.Helper()
	counts := r.work()
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if counts[name] != want {
			t.Errorf("%s %s = %d, want %d", when, name, counts[name], want)
		}
	}
}

const pauseStopMatch = "update project_pause_stops"

// A stop transaction whose commit's reply is lost keeps what its commit owes: the store says it
// committed, so the released row's line, the wake of its start row's lane and the wake of the
// role-holder publication its claim's write queued come once each, as they do for a stop whose
// reply came back, and the stop the machine makes again, which writes no second start row and
// changes no holder, does none of them again. A stop whose commit never landed does none of them,
// and the stop made again does. A stop whose outcome the store cannot say fails, so the machine
// owes it; its line and wakes follow the store's answer, and the stop made again then adds
// nothing.
func TestAPauseStopWhoseCommitReplyIsLostKeepsItsSuccessWork(t *testing.T) {
	t.Parallel()
	replyLost := func(t *testing.T, r *pauseStopRig) {
		t.Helper()
		fired := r.fault.Cut(pauseStopMatch, testdb.CutAfterCommit)
		err := r.stop()
		testdb.Met(t, fired)
		if err != nil {
			t.Errorf("the stop whose commit landed and whose reply was lost = %v, want nil", err)
		}
		if !r.recorded() {
			t.Fatalf("the stop whose reply was lost is not on record: the cut came before the commit")
		}
		r.wantWork(t, 1, "committed with its reply lost, but")
	}
	t.Run("committed", func(t *testing.T) {
		r := newPauseStopRig(t)
		if err := r.stop(); err != nil {
			t.Fatalf("the stop with no fault = %v, want nil", err)
		}
		if !r.recorded() {
			t.Fatal("the stop with no fault is not on record")
		}
		r.wantWork(t, 1, "committed with its reply, but")
	})
	t.Run("reply_lost", func(t *testing.T) {
		replyLost(t, newPauseStopRig(t))
	})
	t.Run("rollback", func(t *testing.T) {
		r := newPauseStopRig(t)
		fired := r.fault.Cut(pauseStopMatch, testdb.CutBeforeCommit)
		err := r.stop()
		testdb.Met(t, fired)
		if err == nil {
			t.Error("the stop whose commit never landed = nil, want its failure")
		}
		if r.recorded() {
			t.Fatal("the stop of a commit that never landed is on record")
		}
		r.wantWork(t, 0, "after a commit that never landed")
		if err := r.stop(); err != nil {
			t.Fatalf("the stop made again with no fault = %v, want nil", err)
		}
		if !r.recorded() {
			t.Fatal("the stop made again with no fault is not on record")
		}
		r.wantWork(t, 1, "after the stop made again")
	})
	t.Run("duplicate_retry", func(t *testing.T) {
		r := newPauseStopRig(t)
		replyLost(t, r)
		if err := r.stop(); err != nil {
			t.Errorf("the stop made again after a committed one = %v, want nil", err)
		}
		if !r.recorded() {
			t.Fatal("the stop is no longer on record as made")
		}
		r.wantWork(t, 1, "committed and made again, but")
	})
	t.Run("unknown", func(t *testing.T) {
		r := newPauseStopRig(t)
		fired := r.fault.Cut(pauseStopMatch, testdb.CutAfterCommitRefusing)
		err := r.stop()
		testdb.Met(t, fired)
		if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
			t.Fatalf("the stop whose outcome the store cannot say = %v, want ErrCommitOutcomeUnknown", err)
		}
		if !r.recorded() {
			t.Fatal("the direct store holds no record of the stop: the cut came before the commit")
		}
		r.wantWork(t, 0, "before the store answers")
		r.fault.Refuse(false)
		testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
			"the stop's line and wakes once the store says it committed", func() bool {
				return len(r.releasedLines()) == 1 && r.wakes() == 1 && r.published() >= 1
			})
		if err := r.stop(); err != nil {
			t.Fatalf("the stop the machine makes again = %v, want nil", err)
		}
		r.wantWork(t, 1, "after the stop made again")
	})
}
