//go:build e2e

// The live proof's driver for the commit a later review round is told: the task the daemon prompts
// the reviewer with in its second review round holds ` Last reviewed commit: <sha>.`, the head the
// reviewer's own deciding review of the first round read, a review whose footer names the entry of
// that round (record.PhaseRow.ReviewedHead, which workflow's review writes and workflow's
// namingTheReviewRound names), and the task of the first round names `none` in that sentence.
// scripts/e2e/review-range.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against.
//
// The rig is the handoff records proof's (startOnDispatch, in handoff_records_live_test.go): the
// real Dispatch server of the checkout on a database of its own, the whole daemon booted with
// dispatch_url at it, the real `dispatch` CLI writing each role's handoff under the session its
// claim registered, and each claim driven over the daemon's worker stream as its pane's shim drives
// it (handoffClaimant), the task it is prompted with read as the shim reads it. The reviewer's
// review is the review App's own, published on the ENVOY_NOTIFICATIONS stream as Envoy relays
// GitHub's webhook (publishReviewAs), before the reviewer completes, as the reviewer posts it.
// Its Legion footer names the pull request's head, a commit id of 40 lowercase hexadecimal
// characters, which is what lets it decide the round (workflow's refusesRound), and the session
// the reviewer's claim registered, which is what makes it the reviewer's own review in the first
// round as in every later one (workflow's byReviewer, with the session the claims record holds
// for the claim the daemon starts the reviewer under): only the reviewer's own review decides the
// round, and a review of another session at the same head is set aside and records none. The
// footer also names the entry of its round, which the stand-in reviewer reads from the task the
// daemon delivered its shim, as an agent reads its task (entryNamedIn): only a review that names
// the entry the reviewer's phases row stands at records its head as the commit the reviewer read,
// and no time is compared. The implementer's push and GitHub's synchronize move that head before
// the second round (publishPushOf, publishSynchronizeOf), so the commit the second round is told is
// not the head it reviews. GitHub's REST API is reviewRangeGitHub; no agent runs. The three issues
// and their pull requests are seeded in testing through the store (seedIssues), each with its
// tester's launch for the boot to relaunch.
//
// Each check reads the reviewer's task of its round, and first holds that it is that task: it says
// `Phase: reviewing.` and names the tester's handoff of that round by its version. Without that, a
// check of a task naming no commit would pass on a task of another round.
package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// reviewRangeLive is one run of the proof: the handoff records rig booted over the real Dispatch,
// the GitHub stand-in, the two issues the steps walk through two review rounds, and the counters
// that keep each review its own fact.
type reviewRangeLive struct {
	*handoffRecordsLive
	pulls *reviewRangeGitHub
	// walked is the issue the first two checks walk; control the issue the third walks, with the
	// commit of its first review cleared from the record between its rounds; and other the issue
	// the fourth walks, whose first round another review-App session's review reaches.
	walked, control, other *reviewRangeIssue
	// seq numbers the review envelopes, so no two carry one event id, and reviewID numbers the
	// reviews, so each is newer than the one before it (record.ReviewOrder).
	seq, reviewID int64
}

// reviewRangeIssue is one issue the proof walks: its key, the name its sessions carry, its pull
// request, and its tester's and reviewer's claims once they register, which both of its rounds use.
type reviewRangeIssue struct {
	key, name string
	number    int
	// first is the head the pull request is opened at, which the first review reads; second is
	// the head the implementer pushes in the second round.
	first, second    string
	tester, reviewer *handoffClaimant
	// task is the reviewer's task of the round it was last prompted in, as the daemon delivered it
	// to the reviewer's shim; each review the stand-in reviewer writes names the entry it ends with.
	task string
}

// TestReviewRangeLive drives the proof's four checks against one booted daemon and one Dispatch.
func TestReviewRangeLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newReviewRangeLive(t)
	r.step("round-1-names-no-commit", r.roundOneNamesNoCommit)
	r.step("round-2-names-the-commit-of-round-1", r.roundTwoNamesTheCommitOfRoundOne)
	r.step("control-no-record-no-commit", r.controlNoRecordNoCommit)
	r.step("other-session-records-no-commit", r.otherSessionRecordsNoCommit)
}

// newReviewRangeLive starts Dispatch, creates the three issues on it, seeds them in testing on open
// pull requests, and boots the daemon at Dispatch with reviewRangeGitHub as GitHub.
func newReviewRangeLive(t *testing.T) *reviewRangeLive {
	t.Helper()
	r := &reviewRangeLive{handoffRecordsLive: startOnDispatch(t, "scripts/e2e/review-range.sh", "RR", "Review range live proof")}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	// Dispatch refuses a second issue whose title shares most of a first one's words as a possible
	// duplicate, so the three are titled apart.
	r.walked = r.newIssue("walked", "a later review round names the commit the round before it read", int(number)+1)
	r.control = r.newIssue("control", "the control whose record loses that commit between its rounds", int(number)+2)
	r.other = r.newIssue("other", "another session's verdict leaves no reviewed head behind", int(number)+3)
	r.pulls = newReviewRangeGitHub(t, r.walked, r.control, r.other)
	r.github = r.pulls.url
	r.seedIssues([]dispatchSeed{
		{key: r.walked.key, phase: phase.Testing, role: claim.RoleTester, pr: r.walked.number, head: r.walked.first},
		{key: r.control.key, phase: phase.Testing, role: claim.RoleTester, pr: r.control.number, head: r.control.first},
		{key: r.other.key, phase: phase.Testing, role: claim.RoleTester, pr: r.other.number, head: r.other.first},
	})
	r.boot()
	return r
}

// newIssue creates an issue titled title on Dispatch, in progress with the legion label, as a
// running daemon leaves one, with its pull request numbered number and the two heads of its rounds.
func (r *reviewRangeLive) newIssue(name, title string, number int) *reviewRangeIssue {
	key := r.createIssue(title, "in_progress", []string{"legion"})
	return &reviewRangeIssue{key: key, name: name, number: number,
		first: headCommit(key, "review round 1"), second: headCommit(key, "review round 2")}
}

// Check round-1-names-no-commit. The walked issue reaches reviewing, and the task the daemon prompts
// the reviewer with names no reviewed commit: ` Last reviewed commit: none.` (namesNoCommit).
func (r *reviewRangeLive) roundOneNamesNoCommit() {
	task := r.roundOne(r.walked)
	r.namesNoCommit("the reviewer's task of round 1", task)
	r.note("the task names ` %s: none.` just before its entry", reviewround.CommitLabel)
}

// namesNoCommit fails the proof unless task, the reviewer's task named what, ends with the
// daemon's sentence naming no commit, ` Last reviewed commit: none.`, and then the entry's, and
// names the commit's label nowhere else: no review of this generation's reviewer has read a commit
// yet, or its record was cleared.
func (r *reviewRangeLive) namesNoCommit(what, task string) {
	r.t.Helper()
	want := " " + reviewround.CommitLabel + ": none. " + reviewround.EntryLabel + ": " + strconv.FormatInt(r.entryNamedIn(task), 10) + "."
	if !strings.HasSuffix(task, want) || strings.Count(task, reviewround.CommitLabel) != 1 {
		r.t.Fatalf("%s = %q, want it to end with %q and to name %q once: its record holds no reviewed commit", what, task, want, reviewround.CommitLabel)
	}
}

// Check round-2-names-the-commit-of-round-1. The reviewer requests changes at the walked issue's
// first head; the implementer pushes the second head and completes, and the tester passes; the task
// the daemon prompts the reviewer with names the first head as the last reviewed commit.
func (r *reviewRangeLive) roundTwoNamesTheCommitOfRoundOne() {
	issue := r.walked
	r.requestChanges(issue)
	task := r.roundTwo(issue)
	want := " " + reviewround.CommitLabel + ": " + issue.first + "."
	if !strings.Contains(task, want) {
		r.t.Fatalf("the reviewer's task of round 2 = %q, want it to hold %q, the head its review of round 1 read", task, want)
	}
	r.note("the task holds %q, the head round 1's review read, while the pull request is at %s", want, issue.second)
}

// Check control-no-record-no-commit. The control issue walks the same first round and review, and
// its reviewer's phases row holds the commit that review read; then the commit is cleared from that
// row in the daemon's database, and the task of round 2 names no reviewed commit, `none`. The check
// before it is this run with the clear left out. The read before the clear is what lets the
// control fail on a first review that recorded nothing: PostgreSQL counts a row the clear matches
// even when it already holds the empty string, so the clear's one row says nothing of what the row
// held.
func (r *reviewRangeLive) controlNoRecordNoCommit() {
	issue := r.control
	task := r.roundOne(issue)
	r.namesNoCommit("the control's reviewer task of round 1", task)
	r.requestChanges(issue)
	if got := r.reviewedHead(issue.key); got != issue.first {
		r.t.Fatalf("phases.reviewed_head of %s's reviewer before the clear = %q, want %s, the head its review of round 1 read", issue.key, got, issue.first)
	}
	tag, err := r.st.Pool().Exec(context.Background(), "update phases set reviewed_head = '' where issue = $1 and role = $2", issue.key, string(claim.RoleReviewer))
	if err != nil || tag.RowsAffected() != 1 {
		r.t.Fatalf("clear phases.reviewed_head of %s's reviewer = %v rows, %v; want 1 row and no error", issue.key, tag.RowsAffected(), err)
	}
	if got := r.reviewedHead(issue.key); got != "" {
		r.t.Fatalf("phases.reviewed_head of %s's reviewer after the clear = %q, want \"\"", issue.key, got)
	}
	r.note("cleared phases.reviewed_head of %s's reviewer; it now reads \"\"", issue.key)
	task = r.roundTwo(issue)
	r.namesNoCommit("the control's reviewer task of round 2", task)
	r.note("the task names ` %s: none.` just before its entry", reviewround.CommitLabel)
}

// Check other-session-records-no-commit. The third issue walks the same first round; then the
// review App's REQUEST_CHANGES at its first head comes from a session the reviewer's claim did not
// register - another Legion role posting under the same login. The daemon sets it aside: the issue
// is still in reviewing, since the review decides nothing, and phases.reviewed_head of its
// reviewer stays empty. The reviewer's row names no claim before its first completion, so this is
// the round in which the reviewer's session is read by the claim the daemon starts the reviewer
// under and not by its row (workflow's reviewersClaim). The walked issue's second check is the
// same first round with the reviewer's own session, which moves the issue back to implementing
// and records the head, so this check can fail.
func (r *reviewRangeLive) otherSessionRecordsNoCommit() {
	issue := r.other
	task := r.roundOne(issue)
	r.namesNoCommit("the other issue's reviewer task of round 1", task)
	// The review differs from the reviewer's own in its session alone: it names the entry the
	// reviewer's task named, so the session is what sets it aside.
	other := "ses_" + issue.name + "_controller"
	entry := r.entryNamedIn(issue.task)
	r.seq++
	r.reviewID++
	r.publishReviewAs(issue.number, "changes_requested", issue.first, legionEntryFooter(other, issue.first, entry), r.seq, r.reviewID)
	r.note("published the review App's REQUEST_CHANGES of #%d, its footer naming session %s, head %s and entry %d", issue.number, other, issue.first, entry)
	if got := r.issue(issue.key).Phase; got != phase.Reviewing {
		r.t.Fatalf("%s after another session's review is in %s, want reviewing: the review names session %s and the reviewer's claim registered %s, so it decides nothing",
			issue.key, got, other, issue.reviewer.session)
	}
	r.note("read back: %s is still in reviewing after the review of another session", issue.key)
	if got := r.reviewedHead(issue.key); got != "" {
		r.t.Fatalf("phases.reviewed_head of %s's reviewer after another session's review = %q, want \"\": the reviewer's claim registered %s",
			issue.key, got, issue.reviewer.session)
	}
	r.note("phases.reviewed_head of %s's reviewer reads \"\" after the review of another session", issue.key)
}

// roundOne walks issue's first review round: the seeded tester, which the boot relaunched, is
// prompted to resume its phase, writes its handoff and passes; the daemon moves the issue to
// reviewing, and the reviewer it starts registers and is prompted. It returns that task, held to be
// the reviewing task naming the tester's handoff of round 1, and keeps it as issue's task.
func (r *reviewRangeLive) roundOne(issue *reviewRangeIssue) string {
	issue.tester = r.claimant(claim.RoleTester, issue.key, "ses_"+issue.name+"_tester")
	issue.tester.awaitPrompt("Phase: testing.", "Resume the existing phase work.")
	test := r.pass(issue, 1)
	issue.reviewer = r.claimant(claim.RoleReviewer, issue.key, "ses_"+issue.name+"_reviewer")
	issue.task = issue.reviewer.awaitPrompt("Phase: reviewing.", handoffLine(issue.key, "test", test.version)).Message
	r.note("round 1: the reviewer's shim was prompted: %q", issue.task)
	return issue.task
}

// requestChanges publishes the reviewer's request for changes at issue's first head, its footer
// naming the session the reviewer's claim registered and the entry the reviewer's task names, and
// waits for the issue to move back to implementing; the reviewer's turn then ends, as a worker's
// does after its review.
func (r *reviewRangeLive) requestChanges(issue *reviewRangeIssue) {
	session := issue.reviewer.session
	entry := r.entryNamedIn(issue.task)
	r.seq++
	r.reviewID++
	r.publishReviewAs(issue.number, "changes_requested", issue.first, legionEntryFooter(session, issue.first, entry), r.seq, r.reviewID)
	r.note("published the review App's REQUEST_CHANGES of #%d, its footer naming session %s, head %s and entry %d, the entry its task named", issue.number, session, issue.first, entry)
	r.waitPhase(issue.key, phase.Implementing)
	r.note("read back: %s is back in implementing; phases.reviewed_head of its reviewer is %q", issue.key, r.reviewedHead(issue.key))
	issue.reviewer.endTurn()
}

// entryNamedIn is the entry of the round task names, read as an agent reads it: the number of its
// last sentence, ` Review entry: <n>.` (reviewround.EntryLabel). It fails the proof when task
// does not end naming a positive entry, since a review written without one moves no commit.
func (r *reviewRangeLive) entryNamedIn(task string) int64 {
	r.t.Helper()
	label := " " + reviewround.EntryLabel + ": "
	at := strings.LastIndex(task, label)
	if at >= 0 {
		if number, last := strings.CutSuffix(task[at+len(label):], "."); last {
			if entry, err := strconv.ParseInt(number, 10, 64); err == nil && entry > 0 {
				return entry
			}
		}
	}
	r.t.Fatalf("the reviewer's task %q does not end naming its %q", task, reviewround.EntryLabel)
	return 0
}

// roundTwo walks issue's second round from implementing: the implementer pushes the second head,
// which GitHub's synchronize moves the pull request to, writes its handoff and completes at that
// head; the tester passes; and the reviewer is prompted again. It returns that task, held to be the
// reviewing task naming the tester's handoff of round 2.
func (r *reviewRangeLive) roundTwo(issue *reviewRangeIssue) string {
	t := r.t
	implementer := r.claimant(claim.RoleImplementer, issue.key, "ses_"+issue.name+"_implementer")
	implementer.awaitPrompt("Phase: implementing.")
	r.pulls.head(issue.key, issue.second)
	r.publishPushOf(issue.key, issue.first, issue.second)
	r.publishSynchronizeOf(issue.number, issue.key, issue.second, "main", "the pull request of "+issue.key, time.Now())
	if got := r.pullRequestHead(issue.key); got != issue.second {
		t.Fatalf("the head the record holds for %s after the push = %q, want %s", issue.key, got, issue.second)
	}
	r.note("published the implementer's push of %s onto %s and GitHub's synchronize; the record holds the pull request at %s", issue.second, issue.first, issue.second)
	implement := r.write(implementer.session, issue.key, "implement", r.smallBody(issue.key, "implement", "round 2"))
	status, failure := implementer.complete(api.HandoffCompleteRequest{Summary: "implemented round 2", Commit: issue.second, HandoffID: implement.id, HandoffVersion: implement.version})
	if status != http.StatusOK {
		t.Fatalf("the implementer's completion of round 2 = %d %+v, want 200", status, failure)
	}
	implementer.endTurn()
	r.waitPhase(issue.key, phase.Testing)
	r.note("the implementer completed at %s naming %s; read back: %s is in testing", issue.second, implement.line, issue.key)
	issue.tester.awaitPrompt("Phase: testing.", handoffLine(issue.key, "implement", implement.version))
	test := r.pass(issue, 2)
	issue.task = issue.reviewer.awaitPrompt("Phase: reviewing.", handoffLine(issue.key, "test", test.version)).Message
	r.note("round 2: the reviewer's shim was prompted: %q", issue.task)
	return issue.task
}

// pass has issue's tester write its handoff of round and complete with a pass, and waits for the
// issue to reach reviewing. Its turn ends with its completion, as a worker's does: the reviewer's
// task is held while another role of the issue is in a turn (supervise's sendPending).
func (r *reviewRangeLive) pass(issue *reviewRangeIssue, round int) handoffWritten {
	t := r.t
	t.Helper()
	test := r.write(issue.tester.session, issue.key, "test", r.smallBody(issue.key, "test", fmt.Sprintf("round %d passes", round)))
	status, failure := issue.tester.complete(api.HandoffCompleteRequest{Summary: "tested", Verdict: "pass", HandoffID: test.id, HandoffVersion: test.version})
	if status != http.StatusOK {
		t.Fatalf("the tester's passing completion of round %d = %d %+v, want 200", round, status, failure)
	}
	issue.tester.endTurn()
	r.waitPhase(issue.key, phase.Reviewing)
	r.note("round %d: the tester passed naming %s; read back: %s is in reviewing", round, test.line, issue.key)
	return test
}

// reviewedHead is phases.reviewed_head of key's reviewer, read from the daemon's own database.
func (r *reviewRangeLive) reviewedHead(key string) string {
	r.t.Helper()
	var head string
	if err := r.st.Pool().QueryRow(context.Background(), "select reviewed_head from phases where issue = $1 and role = $2", key, string(claim.RoleReviewer)).Scan(&head); err != nil {
		r.t.Fatalf("read phases.reviewed_head of %s's reviewer: %v", key, err)
	}
	return head
}

// pullRequestHead is the head the daemon's record holds for key's pull request.
func (r *reviewRangeLive) pullRequestHead(key string) string {
	r.t.Helper()
	var head string
	if err := r.st.Pool().QueryRow(context.Background(), "select head_sha from pull_requests where issue = $1", key).Scan(&head); err != nil {
		r.t.Fatalf("read the head of %s's pull request: %v", key, err)
	}
	return head
}

// reviewRangeGitHub is GitHub's REST API as the daemon reads it through this proof: every
// repository has default branch main, and the open pull requests of an issue's branch are that
// issue's one, at the head the step last set. The implementer's completion reads that list before
// it moves its issue on. Every other route is a 404, as in handoffRecordsDaemonGitHub, so the
// daemon's own read of a pull request (readIssuePullRequest) fails and the record stands as the
// envelopes left it.
type reviewRangeGitHub struct {
	url   string
	mu    sync.Mutex
	pulls map[string]*reviewRangePull
}

// reviewRangePull is one issue's open pull request, by the issue's branch.
type reviewRangePull struct {
	number int
	head   string
}

func newReviewRangeGitHub(t *testing.T, issues ...*reviewRangeIssue) *reviewRangeGitHub {
	t.Helper()
	g := &reviewRangeGitHub{pulls: map[string]*reviewRangePull{}}
	for _, issue := range issues {
		g.pulls[claim.IssueBranch(issue.key)] = &reviewRangePull{number: issue.number, head: issue.first}
	}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.ServeHTTP})
	return g
}

// head moves key's pull request to sha, as a push does on GitHub.
func (g *reviewRangeGitHub) head(key, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[claim.IssueBranch(key)].head = sha
}

func (g *reviewRangeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
	switch parts := strings.Split(rest, "/"); {
	case r.Method == http.MethodGet && isRepository && len(parts) == 2:
		_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
	case r.Method == http.MethodGet && isRepository && len(parts) == 3 && parts[2] == "pulls":
		// The query names the branch as `<owner>:<branch>` (githubrest's OpenPullRequestOn).
		_, branch, _ := strings.Cut(r.URL.Query().Get("head"), ":")
		pull, open := g.pulls[branch]
		if !open {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":%d,"state":"open","head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"updated_at":"2026-10-28T00:00:00Z"}]`, pull.number, branch, pull.head)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}
