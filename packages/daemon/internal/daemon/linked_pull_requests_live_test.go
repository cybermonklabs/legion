package daemon

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The whole-daemon tests of the pull requests an issue links in Dispatch (dispatch://INFRA-5910
// D4, D10): each boots the daemon in the test's process over a Postgres, the package's NATS, the
// real Dispatch publishing its events there (behindTheWebhookCheck), and a fake GitHub of two
// repositories, acme/widgets and acme/deploy.

// stackPlan is two tasks, a and b, b needing a, each need written as examplePlan's: a's work
// merges nothing, since b needs it, so a holds an open pull request of its own until the issue's
// last merge, b's, which is the last task.
const stackPlan = `:::plan_task{#a steps="implementer:i reviewer:r"}
Change the parser of the widget.
:::

:::plan_task{#b needs="%[1]sa" steps="implementer:i tester:t"}
Move the readers of the parser.
:::
`

// stackWalk is a walk of the tasks' rig (tasksRig) of stackPlan whose GitHub closes pull requests
// of acme/widgets through closing, and holds each close until hold closes; writes is each request
// that wrote to GitHub, "<method> <path>", in order.
type stackWalk struct {
	*tasksRig
	closing *closingGitHub
	hold    chan struct{}
	mu      sync.Mutex
	writes  []string
	// unrelated is the number of the open pull request of acme/widgets that no task holds, and
	// the number of the pull request of acme/deploy the issue links, on a's branch.
	unrelated int
}

// newStackWalk walks the issue of stackPlan to merging: a's pull request open, b's open and
// approved by its test, the merger launched; an unrelated pull request of acme/widgets open on a
// branch of its own; and a person linking the pull request of acme/deploy numbered as that one, on
// a's branch, which the daemon holds open in the issue's set.
func newStackWalk(t *testing.T) *stackWalk {
	t.Helper()
	// The broker is taken first: testnats.URL empties it of every stream, and the scratch Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it, which the daemon then consumes Dispatch's events from.
	natsURL := testnats.URL(t)
	dispatch := behindTheWebhookCheck(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL}))
	w := &stackWalk{tasksRig: newTasksRigWith(t, newLiveProofWith(t, natsURL, false, true), dispatch, "A stack of two tasks links a deploy"),
		hold: make(chan struct{})}
	t.Cleanup(w.release)
	w.givePlanText(stackPlan)
	w.firstStep()
	w.waitAt("a", 1)
	w.implementStep("a", "main")
	w.reviewStep("a")
	w.waitDone("a")
	w.waitAt("b", 1)
	w.implementStep("b", "main")
	w.testStep("b")
	w.retro("b")

	w.gh.repo.Commit(t, "unrelated", map[string]string{"unrelated.txt": "the work of no task\n"}, "the work of no task")
	w.unrelated = w.gh.open("unrelated", "main")
	w.closing = newClosingGitHub(map[int]string{w.pulls["a"]: "open", w.unrelated: "open"})
	w.gh.setFront(w.front)
	w.gh.openLinked(w.unrelated, w.branchOf("a"), "linked-head")
	w.linkTo(linkedDeploy, w.unrelated)
	return w
}

// front answers, before the fake repository, each write of a comment on a pull request of
// acme/widgets that closing holds, and each close of one (closing.handle), which waits for hold
// first and which the repository then answers closed (closePull). Every read of a pull request is
// the repository's own, and every write is kept in writes once a close's hold let it through.
func (w *stackWalk) front(rw http.ResponseWriter, r *http.Request) bool {
	rest, ofRepository := strings.CutPrefix(r.URL.Path, "/repos/"+liveRepo+"/")
	digits, closes := strings.CutPrefix(rest, "pulls/")
	if closes = ofRepository && closes && r.Method == http.MethodPatch; closes {
		<-w.hold
	}
	if r.Method != http.MethodGet {
		w.mu.Lock()
		w.writes = append(w.writes, r.Method+" "+r.URL.Path)
		w.mu.Unlock()
	}
	if !ofRepository || r.Method == http.MethodGet && strings.HasPrefix(rest, "pulls/") {
		return false
	}
	if !w.closing.handle(rw, r) {
		return false
	}
	if number, err := strconv.Atoi(digits); closes && err == nil {
		if state, _ := w.closing.pull(number); state == "closed" {
			w.gh.closePull(number)
		}
	}
	return true
}

// release lets every close front holds through, once.
func (w *stackWalk) release() {
	select {
	case <-w.hold:
	default:
		close(w.hold)
	}
}

// wrote is each write to GitHub whose path holds part.
func (w *stackWalk) wrote(part string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var named []string
	for _, write := range w.writes {
		if strings.Contains(write, part) {
			named = append(named, write)
		}
	}
	return named
}

// closedA waits for GitHub to close a's pull request and carry its close comment, which
// ghbranch.ClosePullRequest writes after the close PATCH, and answers the comments on it.
func (w *stackWalk) closedA() []closingComment {
	w.t.Helper()
	var comments []closingComment
	testwait.WithinPolling(w.t, stepRecordsWait, 100*time.Millisecond, "the stack to close and comment on a's pull request", func() bool {
		var state string
		state, comments = w.closing.pull(w.pulls["a"])
		return state == "closed" && len(comments) > 0
	})
	return comments
}

// mergeLinkedPull is GitHub merging the linked pull request of acme/deploy, and the daemon
// recording it.
func (w *stackWalk) mergeLinkedPull() {
	w.t.Helper()
	w.gh.mergeLinked(w.unrelated)
	w.linkedMerged(w.unrelated, w.branchOf("a"), "linked-head")
}

// The close of the other tasks' pull requests (record.TaskPullRequestsClose) leaves a linked pull
// request open, and a linked pull request holds the issue's close (dispatch://INFRA-5910 D4, D2).
// The whole daemon walks stackPlan to merging: a's pull request is open, since b needs a and a
// merges nothing, and b's is the last task's. The issue links a pull request of acme/deploy on a's
// branch name, numbered as an unrelated open pull request of acme/widgets.
//   - Case 1: b's merge, the last, closes a's pull request through GitHub with a comment that does
//     not say the issue is done; the linked pull request and the unrelated one stay open, nothing
//     writes to either, and the issue stays in merging until the linked pull request merges, which
//     ends it.
//   - Case 2: the linked pull request merges first, then b's: GET /legion/v1/state, read while
//     GitHub still holds a's pull request open, before its close is made, shows the issue done,
//     and the close's comment says so.
func TestTheStacksCloseOfTaskPullRequestsLeavesALinkedPullRequestOpen(t *testing.T) {
	t.Run("case 1: the last task's merge closes the task pull requests and no linked one", func(t *testing.T) {
		w := newStackWalk(t)
		w.release()
		w.merge("b")
		comments := w.closedA()
		if len(comments) != 1 || strings.Contains(comments[0].body, "is done") {
			t.Fatalf("the comments on a's pull request are %+v, want one that does not say the issue is done", comments)
		}
		if state, _ := w.closing.pull(w.unrelated); state != "open" {
			t.Errorf("the unrelated acme/widgets#%d is %s after the close, want open: the linked pull request's number names no pull request of the issue's repository", w.unrelated, state)
		}
		if writes := slices.Concat(w.wrote(fmt.Sprintf("/pulls/%d", w.unrelated)), w.wrote(fmt.Sprintf("/issues/%d/", w.unrelated)), w.wrote("acme/deploy")); len(writes) != 0 {
			t.Errorf("the daemon wrote %v to GitHub, want nothing written to the linked pull request or the unrelated one", writes)
		}
		if got := w.pullState(linkedDeploy, w.unrelated); got != string(record.PullRequestOpen) {
			t.Errorf("the record holds the linked acme/deploy#%d %q, want open", w.unrelated, got)
		}
		w.stands("b's merge, with the linked pull request open", phase.Merging)
		w.mergeLinkedPull()
		w.waitPhase(phase.Done)
	})

	t.Run("case 2: the linked pull request merged first, so the issue is done at the last task's merge", func(t *testing.T) {
		w := newStackWalk(t)
		w.mergeLinkedPull()
		w.stands("the linked pull request's merge, with b's open", phase.Merging)
		w.merge("b")
		w.waitPhase(phase.Done)
		if state, _ := w.closing.pull(w.pulls["a"]); state != "open" {
			t.Fatalf("a's pull request is %s when the state shows the issue done, want it open, its close held", state)
		}
		w.release()
		if comments := w.closedA(); len(comments) != 1 || !strings.Contains(comments[0].body, "is done") {
			t.Errorf("the comments on a's pull request are %+v, want one that says the issue is done", comments)
		}
	})
}

// newestMigration is the version of the newest embedded migration.
func newestMigration(t *testing.T) int {
	t.Helper()
	all, err := migrations.All()
	if err != nil || len(all) == 0 {
		t.Fatalf("read the embedded migrations: %d, %v", len(all), err)
	}
	return all[len(all)-1].Version
}

// linkInDispatch has a person link the walk's issue to pull request number of acme/deploy, which
// GitHub holds open on linkedBranch, writing more besides when more names fields; it answers the
// issue as Dispatch answered the write.
func (r *stepRecordsRig) linkInDispatch(number int, more map[string]any) createdIssue {
	r.t.Helper()
	r.github.openIn(linkedDeploy, linkedBranch, number)
	body := map[string]any{"external_links": []map[string]string{{"url": fmt.Sprintf("https://github.com/%s/pull/%d", linkedDeploy, number)}}}
	for name, value := range more {
		body[name] = value
	}
	var linked createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, body, http.StatusOK, &linked)
	return linked
}

// seedAsMain records the walk's issue as main's daemon left it in a store of main's migrations,
// in the columns that release writes: the issue in at, with status, the rank and the sequence
// Dispatch gave; its implementer's phases row at stepRecordsHead; and its own pull request, the
// one row main keys by the issue, open at stepRecordsHead and clean. It holds no link, since
// main's store has none.
func (r *stepRecordsRig) seedAsMain(at phase.Phase, status, rank string, seq int64) {
	r.t.Helper()
	ctx := context.Background()
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`insert into issues (key, tree, project, title, phase, generation, status, dispatch_status, rank, last_dispatch_seq,
				repo, default_branch, github_repo, handed_over)
			values ($1, $1, $2, $3, $4, 1, $5, $5, $6, $7, $8, 'main', $8, true)`,
			[]any{r.key, r.cfg.Project, r.title, string(at), status, rank, seq, liveRepo}},
		{`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry) values ($1, 'implementer', $2, $3, 1, '', 1)`,
			[]any{r.key, string(r.token(claim.RoleImplementer)), stepRecordsHead}},
		{`insert into pull_requests (issue, repo, number, branch, base, head_sha, head_updated_at, failing, fix_attempts, blocked_attempts,
				check_runs, generation, snapshot, head_counted, state, merge_state)
			values ($1, $2, $3, 'legion/' || $1, 'main', $4, now(), '[]', 0, 0, '[]', 1, '', '', 'open', 'clean')`,
			[]any{r.key, liveRepo, r.number, stepRecordsHead}},
	} {
		if _, err := r.st.Pool().Exec(ctx, seed.sql, seed.args...); err != nil {
			r.t.Fatalf("record %s as main's daemon did: %v", r.key, err)
		}
	}
}

// implementerRow is the claim, the handoff commit and the entry of the issue's implementer's
// phases row.
func (r *stepRecordsRig) implementerRow() string {
	r.t.Helper()
	var row string
	if err := r.st.Pool().QueryRow(context.Background(), `select row(claim, handoff_commit, entry)::text from phases
		where issue = $1 and role = 'implementer'`, r.key).Scan(&row); err != nil {
		r.t.Fatalf("read the implementer's phases row of %s: %v", r.key, err)
	}
	return row
}

// pullRequestSet is each pull request the record holds of the issue, "<repo>#<number> <state>",
// in the order of their repositories and numbers.
func (r *stepRecordsRig) pullRequestSet() []string {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select repo || '#' || number || ' ' || state from pull_requests
		where issue = $1 order by lower(repo), number`, r.key)
	var set []string
	if err == nil {
		set, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		r.t.Fatalf("read the pull requests of %s: %v", r.key, err)
	}
	return set
}

// A linked pull request that joins the set of an issue that runs leaves its phase and its work as
// they were (dispatch://INFRA-5910 D10), whichever phase it stands in. The daemon boots over a store
// of main's migrations (store.MigrateMainsThrough), which the boot upgrades, holding one issue in
// reviewing and one in merging, each as main's daemon recorded it: its implementer's phases row and
// its own pull request on acme/widgets, and no link, since that store has none. Dispatch holds each
// issue linking an open pull request of acme/deploy. After the boot each issue stands in its
// phase, as GET /legion/v1/state shows it, with its status and its implementer's row as they were,
// and its set holds the linked pull request, open, beside its own.
func TestARunningIssueKeepsItsPhaseWhenALinkedPullRequestJoinsTheSet(t *testing.T) {
	natsURL := testnats.URL(t)
	proof := newLiveProofWith(t, natsURL, false, false)
	if err := proof.st.MigrateMainsThrough(context.Background(), newestMigration(t)); err != nil {
		t.Fatal(err)
	}
	stands := []struct {
		at     phase.Phase
		status string
	}{{phase.Reviewing, "needs_review"}, {phase.Merging, "retro"}}
	before := map[string]string{}
	r := newStepRecordsRigSeeded(t, proof, behindTheWebhookCheck(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})), len(stands),
		func(r *stepRecordsRig, created createdIssue) bool {
			stand := stands[len(before)]
			linked := r.linkInDispatch(r.linkedNumber(), nil)
			r.seedAsMain(stand.at, stand.status, created.Rank, linked.LastSeq)
			r.seedArchitect()
			before[r.key] = r.implementerRow()
			return true
		})
	for place, stand := range stands {
		r.on(r.issues[place].key)
		testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the boot to read "+r.key+"'s linked pull request", func() bool {
			return r.pullState(linkedDeploy, r.linkedNumber()) == string(record.PullRequestOpen)
		})
		r.stands("the boot", stand.at)
		if got := linkedIssue(t, r.st.Pool(), r.key); got.Phase != stand.at || got.Status != stand.status {
			t.Errorf("%s is %s with status %q after the boot, want %s with %q, as it stood", r.key, got.Phase, got.Status, stand.at, stand.status)
		}
		if got := r.implementerRow(); got != before[r.key] {
			t.Errorf("%s's implementer's phases row (claim, handoff commit, entry) is %s after the boot, want %s, as it was", r.key, got, before[r.key])
		}
		want := []string{fmt.Sprintf("%s#%d open", linkedDeploy, r.linkedNumber()), fmt.Sprintf("%s#%d open", liveRepo, r.number)}
		if got := r.pullRequestSet(); !slices.Equal(got, want) {
			t.Errorf("the set of %s after the boot is %v, want %v: the linked pull request beside its own", r.key, got, want)
		}
	}
}

// An issue an earlier daemon closed is not reopened by its links (dispatch://INFRA-5910 D10). The
// daemon boots over a store that holds one issue done, its own pull request merged, which Dispatch
// holds done and linking an open pull request of acme/deploy (Dispatch refuses any edit of a done
// issue, ISSUE_CLOSED, so the link is written before it is done). Once the boot's fact is applied
// the issue is still done, with its status as it was, and the boot read nothing of it: the record
// holds no link of it and no row of the linked pull request.
func TestAClosedIssueIsNotReopenedByItsLinks(t *testing.T) {
	natsURL := testnats.URL(t)
	r := newStepRecordsRigSeeded(t, newLiveProofWith(t, natsURL, false, true),
		behindTheWebhookCheck(t, startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})), 1,
		func(r *stepRecordsRig, created createdIssue) bool {
			linked := r.linkInDispatch(r.linkedNumber(), map[string]any{"status": "done"})
			if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
				if err := r.records.PutIssue(ctx, tx, record.Issue{
					Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
					Title: r.title, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done",
					Rank: created.Rank, LastDispatchSeq: linked.LastSeq, HandedOver: true,
				}); err != nil {
					return err
				}
				pr := stepRecordsPullRequest(r.key, r.number)
				pr.State, pr.MergeSHA = record.PullRequestMerged, "merge-"+stepRecordsHead
				return r.records.PutPullRequest(ctx, tx, pr)
			}); err != nil {
				t.Fatalf("record %s done: %v", r.key, err)
			}
			return false
		})
	r.github.merged(r.number)
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the daemon to apply its boot's fact", func() bool {
		var booted bool
		if err := r.st.Pool().QueryRow(context.Background(), `select exists(select 1 from processed_events
			where source = 'daemon' and event_id like 'boot:%')`).Scan(&booted); err != nil {
			t.Fatalf("read the boot's processed event: %v", err)
		}
		return booted
	})
	if got := linkedIssue(t, r.st.Pool(), r.key); got.Phase != phase.Done || got.Status != "done" {
		t.Errorf("%s is %s with status %q after the boot, want it done: a done issue is never reopened by its links", r.key, got.Phase, got.Status)
	}
	var links int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from pull_request_links where issue = $1`, r.key).Scan(&links); err != nil {
		t.Fatalf("count the links of %s: %v", r.key, err)
	}
	if got := r.pullState(linkedDeploy, r.linkedNumber()); links != 0 || got != "" {
		t.Errorf("the boot recorded %d links of the done issue and its linked acme/deploy#%d as %q, want it to read nothing of a done issue", links, r.linkedNumber(), got)
	}
}
