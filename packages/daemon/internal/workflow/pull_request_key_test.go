package workflow

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// sharedBranch is LEGION-208's own branch, from which the tests of two pull requests of one branch
// open both, and secondBase is the base the second of them is opened onto: INFRA-1276's branch
// held theorem-labs/agent-infra#3654 onto main and then the draft #3955 onto
// legion/INFRA-1276-ci-base, and these tests hold #42 onto main and #43 onto secondBase.
const (
	sharedBranch = "legion/LEGION-208"
	secondBase   = "legion/LEGION-208-ci-base"
)

// numberedPullRequest is pull request number of sjawhar/legion as the store reads it back by its
// repository and its number (record.Store.PullRequestByNumber), nil where the record holds no row
// of it.
func numberedPullRequest(t *testing.T, pool *pgxpool.Pool, number int) *record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequestByNumber(t.Context(), tx, "sjawhar/legion", number)
		return err
	})
	return pr
}

// rowStands is what a test holds of one pull request's row: its state, the base it merges into,
// its head, and the merge commit of a merged one.
type rowStands struct {
	state             record.PullRequestState
	base, head, merge string
}

func (s rowStands) String() string {
	if s.merge != "" {
		return fmt.Sprintf("%s onto %s at %s, merged as %s", s.state, s.base, s.head, s.merge)
	}
	return fmt.Sprintf("%s onto %s at %s", s.state, s.base, s.head)
}

// rowStandsOf is what pr's row holds of rowStands.
func rowStandsOf(pr record.PullRequest) rowStands {
	return rowStands{state: pr.State, base: pr.Base, head: pr.HeadSHA, merge: pr.MergeSHA}
}

// assertRowsStand fails the test unless the record holds a row of each pull request want numbers,
// read by its repository and its number, standing as want says, after what names the fact that
// left them: each pull request of the branch is a row of its own, and none takes another's place.
// It names every row that is wrong, not the first alone.
func assertRowsStand(t *testing.T, pool *pgxpool.Pool, after string, want map[int]rowStands) {
	t.Helper()
	for _, number := range slices.Sorted(maps.Keys(want)) {
		pr := numberedPullRequest(t, pool, number)
		if pr == nil {
			t.Errorf("after %s the record holds no row of #%d; want #%d %s: each pull request of %s is a row of its own, which no other pull request's write replaces",
				after, number, number, want[number], sharedBranch)
			continue
		}
		if got := rowStandsOf(*pr); got != want[number] {
			t.Errorf("after %s #%d is %s; want %s", after, number, got, want[number])
		}
	}
}

// holdsPush says whether pr holds the push from before to after: among the pushes waiting for the
// move they make (record.PullRequest.Pushes), or as a move the record made by it (Moves), which
// spends the push its head arrived by.
func holdsPush(pr record.PullRequest, before, after string) bool {
	return slices.ContainsFunc(pr.Pushes, func(push record.ClassifiedPush) bool { return push.Before == before && push.SHA == after }) ||
		slices.ContainsFunc(pr.Moves, func(move record.Move) bool { return move.Before == before && move.SHA == after })
}

// assertPushReached fails the test unless each pull request numbers names is recorded and holds
// the push from before to after (holdsPush), after what names the fact.
func assertPushReached(t *testing.T, pool *pgxpool.Pool, after string, before, to string, numbers ...int) {
	t.Helper()
	for _, number := range numbers {
		pr := numberedPullRequest(t, pool, number)
		switch {
		case pr == nil:
			t.Errorf("after %s the record holds no row of #%d to hold the push %s..%s; want it held by each open pull request on %s", after, number, before, to, sharedBranch)
		case !holdsPush(*pr, before, to):
			t.Errorf("after %s #%d holds the pushes %+v and the moves %+v; want the push %s..%s among them: a push names a branch and no number, so it reaches each open pull request on %s",
				after, number, pr.Pushes, pr.Moves, before, to, sharedBranch)
		}
	}
}

// A pull request is one row of the record, keyed by its repository and its number, with its issue
// a column of the row (dispatch://INFRA-5910/spec#b-d1): two pull requests of one branch are two
// rows, and a fact that names a number changes that number's row alone. LEGION-208 stands in
// testing with no plan rows, so its own branch, legion/LEGION-208, is its work's, and GitHub opens
// #42 from it onto main and then #43 from it onto legion/LEGION-208-ci-base, as INFRA-1276's branch
// held theorem-labs/agent-infra#3654 onto main and then the draft #3955 onto
// legion/INFRA-1276-ci-base. Each row of the table is one fact of GitHub's, applied through the
// engine in the table's order, and after each the record holds both rows as that fact left them:
//   - the opens record each pull request, onto its own base;
//   - a push of the branch names no number, so it reaches each open pull request on the branch and
//     moves no head;
//   - the synchronize of #42 moves #42's head alone, and #43 waits for its own;
//   - the close of #43 closes #43 alone;
//   - the merge of #42 merges #42 alone, and #43 stays closed.
//
// The fault it catches is a record keyed by the issue and the branch (record.Store.PutPullRequest's
// conflict on them, and 0004's unique pull_requests_repo_branch): the open of #43 takes the place of
// #42, and every fact after it finds one row where two pull requests stand, so the synchronize of
// #42, which the workflow reads by its branch, moves #43's head.
func TestAPullRequestIsKeyedByRepositoryAndNumber(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	tasksRoot(t, pool)
	apply := applyFacts(t, pool, engine)
	at := func(minute int) time.Time { return time.Date(2026, 11, 17, 9, minute, 0, 0, time.UTC) }
	opened := func(number int, base string, minute int) intake.PullRequestOpened {
		return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: number, Branch: sharedBranch, Base: base, HeadSHA: headOne,
			HeadRepo: "sjawhar/legion", UpdatedAt: at(minute)}
	}
	for _, row := range []struct {
		name  string
		facts []intake.Fact
		want  map[int]rowStands
		// pushed is the pull requests that hold the push of the branch after the row's facts.
		pushed []int
	}{
		{
			name:  "the opens of #42 onto main and of #43 onto another base",
			facts: []intake.Fact{opened(42, "main", 0), opened(43, secondBase, 5)},
			want: map[int]rowStands{
				42: {state: record.PullRequestOpen, base: "main", head: headOne},
				43: {state: record.PullRequestOpen, base: secondBase, head: headOne},
			},
		},
		{
			name:  "a push of the branch",
			facts: []intake.Fact{intake.Push{Repo: "sjawhar/legion", Branch: sharedBranch, Before: headOne, After: headTwo, Pusher: "a-writer", Sender: "a-writer"}},
			want: map[int]rowStands{
				42: {state: record.PullRequestOpen, base: "main", head: headOne},
				43: {state: record.PullRequestOpen, base: secondBase, head: headOne},
			},
			pushed: []int{42, 43},
		},
		{
			name: "the synchronize of #42",
			facts: []intake.Fact{intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: sharedBranch, Base: "main",
				Before: headOne, HeadSHA: headTwo, Sender: "a-writer", UpdatedAt: at(10)}},
			want: map[int]rowStands{
				42: {state: record.PullRequestOpen, base: "main", head: headTwo},
				43: {state: record.PullRequestOpen, base: secondBase, head: headOne},
			},
		},
		{
			name:  "the close of #43",
			facts: []intake.Fact{intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 43, HeadSHA: headTwo, UpdatedAt: at(15)}},
			want: map[int]rowStands{
				42: {state: record.PullRequestOpen, base: "main", head: headTwo},
				43: {state: record.PullRequestClosed, base: secondBase, head: headTwo},
			},
		},
		{
			name:  "the merge of #42",
			facts: []intake.Fact{intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42", HeadSHA: headTwo}},
			want: map[int]rowStands{
				42: {state: record.PullRequestMerged, base: "main", head: headTwo, merge: "merge-42"},
				43: {state: record.PullRequestClosed, base: secondBase, head: headTwo},
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			for i, fact := range row.facts {
				mustSpend(t, row.name, apply(fmt.Sprintf("%s-%d", row.name, i), fact))
			}
			assertRowsStand(t, pool, row.name, row.want)
			assertPushReached(t, pool, row.name, headOne, headTwo, row.pushed...)
		})
	}
}
