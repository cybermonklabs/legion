package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// secondBase is the base the second open pull request of legion/CAPTURE-1 is opened onto, as
// INFRA-1276's branch held theorem-labs/agent-infra#3654 onto main and then the draft #3955 onto
// legion/INFRA-1276-ci-base.
const secondBase = "legion/CAPTURE-1-ci-base"

// twoOnTheBranch is GitHub's REST API for acme/widgets holding two open pull requests of one
// branch, legion/CAPTURE-1, both of acme/widgets itself: #86 onto main, CAPTURE-1's own, and #87,
// opened after it, onto secondBase. GitHub shows both at head, the head of their one branch, last
// updated at updated, mergeable and clean, with no reviews. Listed by the branch (GET
// /pulls?head=acme:legion/CAPTURE-1&state=open), it answers both newest first, as GitHub orders
// its list of pull requests (sort created, direction desc), so the first it lists is #87. It
// answers the branch's activity, newest first, with activity. Every request is counted in reads.
type twoOnTheBranch struct {
	head     string
	updated  time.Time
	activity []string
	reads    pathReads
}

func (g *twoOnTheBranch) serve(t *testing.T) *httptest.Server {
	t.Helper()
	pull := func(number int, base string) string {
		return fmt.Sprintf(`{"number":%d,"state":"open","html_url":"https://github.com/acme/widgets/pull/%d","body":"Dispatch: CAPTURE-1",`+
			`"base":{"ref":%q},"head":{"ref":"legion/CAPTURE-1","sha":%q,"repo":{"full_name":"acme/widgets"}},"updated_at":%q,"mergeable":true,"mergeable_state":"clean"}`,
			number, number, base, g.head, g.updated.Format(time.RFC3339))
	}
	return githubtest.Serve(t, githubtest.Spec{Token: "outbox-token", Record: func(r *http.Request) {
		g.reads.add(r.URL.Path)
	}, Handle: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls":
			if r.URL.Query().Get("head") != "acme:legion/CAPTURE-1" || r.URL.Query().Get("state") != "open" {
				t.Errorf("listed pull requests by %q, want the open ones on acme:legion/CAPTURE-1", r.URL.RawQuery)
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte("[" + pull(87, secondBase) + "," + pull(86, "main") + "]"))
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull(86, "main")))
		case "/repos/acme/widgets/pulls/87":
			w.Write([]byte(pull(87, secondBase)))
		case "/repos/acme/widgets/pulls/86/reviews", "/repos/acme/widgets/pulls/87/reviews":
			w.Write([]byte(`[]`))
		case "/repos/acme/widgets/activity":
			if ref := r.URL.Query().Get("ref"); ref != "refs/heads/legion/CAPTURE-1" {
				t.Errorf("read the activity of %q, want refs/heads/legion/CAPTURE-1", ref)
			}
			w.Write([]byte("[" + strings.Join(g.activity, ",") + "]"))
		default:
			http.NotFound(w, r)
		}
	}})
}

// twoOnTheBranchRecords is #86 and #87 as the record holds them open on legion/CAPTURE-1 at head
// code, the head their one branch stood at when their records last moved, each onto its own base,
// #86 first: the order GitHub opened them in.
func twoOnTheBranchRecords() []record.PullRequest {
	var prs []record.PullRequest
	for _, pr := range []struct {
		number int
		base   string
	}{{86, "main"}, {87, secondBase}} {
		prs = append(prs, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: pr.number,
			Branch: "legion/CAPTURE-1", Base: pr.base, HeadSHA: "code", HeadUpdatedAt: captureHeadUpdated, CheckedHead: "code", Failing: []string{},
			CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}}, Generation: 1, Snapshot: "settled",
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean})
	}
	return prs
}

// seedTwoOnTheBranch records CAPTURE-1 in testing, with no plan rows, its implementer's and its
// tester's claims, and #86 and then #87 open on its branch (twoOnTheBranchRecords), each written
// as its open records it, the second after the first.
func seedTwoOnTheBranch(t *testing.T, pool *store.Pool) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1",
			Project: "CAPTURE", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"}); err != nil {
			return err
		}
		for _, row := range []record.PhaseRow{{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim"},
			{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"}} {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed CAPTURE-1: %v", err)
	}
	for _, pr := range twoOnTheBranchRecords() {
		if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			return records.PutPullRequest(ctx, tx, pr)
		}); err != nil {
			t.Fatalf("seed #%d: %v", pr.Number, err)
		}
	}
}

// numberedPullRequest is pull request number of acme/widgets as the store reads it back by its
// repository and its number (record.Store.PullRequestByNumber), nil where the record holds no row
// of it.
func numberedPullRequest(t *testing.T, pool *store.Pool, number int) *record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequestByNumber(ctx, tx, "acme/widgets", number)
		return err
	}); err != nil {
		t.Fatalf("read #%d: %v", number, err)
	}
	return pr
}

// assertOpenOnItsBase fails the test unless the record holds #number open onto base at head, read
// by its repository and its number, after what.
func assertOpenOnItsBase(t *testing.T, pool *store.Pool, after string, number int, base, head string) {
	t.Helper()
	pr := numberedPullRequest(t, pool, number)
	switch {
	case pr == nil:
		t.Errorf("after %s the record holds no row of #%d; want #%d open onto %s at %s: each open pull request on legion/CAPTURE-1 is a row of its own",
			after, number, number, base, head)
	case pr.State != record.PullRequestOpen || pr.Base != base || pr.HeadSHA != head:
		t.Errorf("after %s #%d is %s onto %s at %s; want it open onto %s at %s", after, number, pr.State, pr.Base, pr.HeadSHA, base, head)
	}
}

// Every read of the daemon that has only a branch takes each open pull request on it
// (dispatch://INFRA-5910/spec#b-d1: "A push, which carries a branch and no number, applies to each
// open pull request on that branch, and so does each read that has only a branch"). This is the
// daemon's part of the inventory of those reads, rows b6 to b9 of the plan's decision 20; the
// workflow's (b1 to b5), the API's (b10) and ghbranch's (b11) hold a test of this name in their own
// packages. In each row CAPTURE-1, an issue with no plan rows, has two open pull requests on its
// branch legion/CAPTURE-1 (twoOnTheBranch): #86 onto main and #87, opened after it, onto
// legion/CAPTURE-1-ci-base, as INFRA-1276's branch held theorem-labs/agent-infra#3654 onto main and
// the draft #3955 onto legion/INFRA-1276-ci-base. Both are read, and neither takes the place of the
// other.
//   - b6, the per-issue read's list of the branch (issuePullRequest, openPullRequestOn): with no
//     pull request recorded, the read records each open pull request GitHub lists on the branch,
//     and the pull request of the issue's work is #86, the first opened (the plan's decision 5),
//     though GitHub lists #87 first.
//   - b7, the per-issue read's pull request of the task (issueRecords.pullRequest): with both
//     recorded, the read judges #86, the task's pull request by decision 5, and reads it on GitHub.
//   - b8, the pushes the read rebuilds from the branch's activity (lostPushes): GitHub shows the
//     branch moved by a push whose webhook was lost, and the push the read rebuilds reaches each
//     open pull request on the branch, as a push's webhook does.
//   - b9, the follow of a head (followHead, openRecord): the follow of #86 to the head GitHub shows
//     reads #86's record by its repository and its number, so it moves #86 and leaves #87 at its
//     own head.
//
// The fault it catches is a read by the branch that answers one pull request
// (githubrest.Client.OpenPullRequestOn answers the first GitHub lists), a pick of the first row of
// a task (issueRecords.pullRequest, openRecord's record.Store.PullRequestRowOfTask), and a record
// keyed by the issue and the branch, so the second pull request's row takes the place of the
// first's.
func TestEachReadByBranchTakesEveryOpenPullRequestOnIt(t *testing.T) {
	t.Parallel()
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	lostPush := activityEntry("legion/CAPTURE-1", "push", "code", "pushed", implementerLogin, read.Add(-time.Minute))

	t.Run("b6 the per-issue read's list of the branch", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedUnrecorded(t, pool, "handoff")
		github := &twoOnTheBranch{head: "handoff", updated: read}
		readCapture(t, readRuntime(pool, github.serve(t).URL, quietLogger()))
		assertOpenOnItsBase(t, pool, "the read of the branch", 86, "main", "handoff")
		assertOpenOnItsBase(t, pool, "the read of the branch", 87, secondBase, "handoff")
		var own *record.PullRequest
		if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			own, err = record.NewStore().PullRequestOfTask(ctx, tx, "CAPTURE-1", "")
			return err
		}); err != nil {
			t.Fatalf("read the pull request of CAPTURE-1's work: %v", err)
		}
		switch {
		case own == nil:
			t.Errorf("after the read of the branch the record holds no pull request of CAPTURE-1's work; want #86, the first opened of the open pull requests on its branch")
		case own.Number != 86:
			t.Errorf("after the read of the branch the pull request of CAPTURE-1's work is #%d onto %s; want #86 onto main, the first opened of the open pull requests on its branch, though GitHub lists #87 first",
				own.Number, own.Base)
		}
	})

	t.Run("b7 the per-issue read's pull request of the task", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedTwoOnTheBranch(t, pool)
		github := &twoOnTheBranch{head: "code", updated: captureHeadUpdated}
		readCapture(t, readRuntime(pool, github.serve(t).URL, quietLogger()))
		if counts := github.reads.counts(); counts["/repos/acme/widgets/pulls/86"] == 0 {
			t.Errorf("the per-issue read of CAPTURE-1 read %v of GitHub; want #86 among them, the pull request of its work, the first opened on its branch", counts)
		}
		assertOpenOnItsBase(t, pool, "the per-issue read", 86, "main", "code")
		assertOpenOnItsBase(t, pool, "the per-issue read", 87, secondBase, "code")
	})

	t.Run("b8 the pushes the read rebuilds from the branch's activity", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedTwoOnTheBranch(t, pool)
		github := &twoOnTheBranch{head: "pushed", updated: read, activity: []string{lostPush}}
		readCapture(t, reviewAppRuntime(pool, github.serve(t).URL, quietLogger()))
		for _, number := range []int{86, 87} {
			pr := numberedPullRequest(t, pool, number)
			switch {
			case pr == nil:
				t.Errorf("after the read the record holds no row of #%d to hold the push code..pushed it rebuilt; want it held by each open pull request on legion/CAPTURE-1", number)
			case !slices.ContainsFunc(pr.Pushes, func(push record.ClassifiedPush) bool { return push.Before == "code" && push.SHA == "pushed" }) &&
				!slices.ContainsFunc(pr.Moves, func(move record.Move) bool { return move.Before == "code" && move.SHA == "pushed" }):
				t.Errorf("after the read #%d holds the pushes %+v and the moves %+v; want the push code..pushed the read rebuilt among them: a push names a branch and no number, so it reaches each open pull request on legion/CAPTURE-1",
					number, pr.Pushes, pr.Moves)
			}
		}
	})

	t.Run("b9 the follow of a head", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedTwoOnTheBranch(t, pool)
		github := &twoOnTheBranch{head: "pushed", updated: read, activity: []string{lostPush}}
		w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
		listed := twoOnTheBranchRecords()[0]
		client, pull, err := w.readPullRequest(context.Background(), listed)
		if err != nil {
			t.Fatalf("read #86: %v", err)
		}
		got, err := w.followHead(context.Background(), client, listed, pull)
		if err != nil || got.Number != 86 || got.HeadSHA != "pushed" {
			t.Errorf("the follow of #86 = #%d at %q, %v; want #86 at pushed, the head GitHub shows: the follow reads #86's record by its repository and its number", got.Number, got.HeadSHA, err)
		}
		assertOpenOnItsBase(t, pool, "the follow of #86", 86, "main", "pushed")
		assertOpenOnItsBase(t, pool, "the follow of #86", 87, secondBase, "code")
	})
}
