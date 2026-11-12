package daemon

// INFRA-3136 R3, the pull request a correction leaves behind: Dispatch moved the issue to another
// repository and the daemon's record followed it (record.Store.CorrectIssueRepository), while the
// pull request the issue opened before the correction is still open on the repository the issue
// has left. The correction moves the issue's row alone, so the pull request's row keeps the
// repository it was opened on, and the daemon matches every GitHub fact to an issue by the
// repository the record names now (workflow's ownPullRequest and issueForBranch): no event of
// that pull request reaches the issue any longer.
//
// That is a state a role reads rather than a loss nobody sees. These tests read the two halves of
// it: the launch read of the record (workflowRuntime.issueLaunchRepository, over a real
// PostgreSQL) and the sentences the next task of every claim of the issue carries
// (AddressingFragment, through the daemon's own specs). The record's own half, what `read_record`
// shows, is covered where the correction's other keys are (api's
// TestTheStateShowsARepositoryCorrectionAndAChangeStillToBeApplied).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// leftPullRequestKey is the recorded issue these tests correct, and leftPullRequestProject
	// the Dispatch project it belongs to.
	leftPullRequestKey     = "LEGION-500"
	leftPullRequestProject = "legion"
	// leftRepo is the repository the issue was recorded on and opened its pull request on, and
	// workedRepo the one Dispatch moved it to.
	leftRepo   = "acme/widgets"
	workedRepo = "zeta/tools"
	// leftPullRequestNumber is the pull request the issue opened on leftRepo.
	leftPullRequestNumber = 1251
)

// leftPullRequestMoment is the moment the correction was applied, which the task names.
var leftPullRequestMoment = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

// correctionSentences is what every launch of a corrected issue is told about the correction
// itself, which the pull request's sentences follow.
const correctionSentences = "The daemon changed your issue's repository from acme/widgets at 2026-11-12T09:30:00Z. " +
	"Your issue's branch, your issue's pull request and any rescue branch of the old pod are on acme/widgets. " +
	"Your work on this issue is on zeta/tools now."

// leftPullRequestSentences name the pull request the correction left open on the repository the
// issue has left, and what the daemon does with its events from then on.
const leftPullRequestSentences = "The pull request acme/widgets#1251 is open on acme/widgets. " +
	"The daemon does not apply GitHub events of that pull request to this issue."

// leftPullRequestRig is one test's daemon: the workflow's own record reads over a database of its
// own, and the specs that build a launch from them, which is the pair a task is rendered by.
type leftPullRequestRig struct {
	t       *testing.T
	pool    *pgxpool.Pool
	records record.Store
	specs   specs
	token   claim.Token
}

func newLeftPullRequestRig(t *testing.T) *leftPullRequestRig {
	t.Helper()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	token, err := claim.NewToken(leftPullRequestProject, leftPullRequestKey, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	r := &leftPullRequestRig{t: t, pool: isolatedOutboxPool(t), records: record.NewStore(), token: token}
	// The launch read is the workflow's own, over this test's database: nothing of it is faked,
	// so what the task names is what the record holds.
	w := &workflowRuntime{pool: r.pool, records: r.records, log: quietLogger()}
	r.specs = specs{
		stateDir: r.t.TempDir(), project: leftPullRequestProject, prompts: composer,
		repository: w.issueLaunchRepository,
		repositories: func() api.RepositoriesView {
			return api.RepositoriesView{Mapped: []api.MappedRepositoryView{{Repo: leftRepo}, {Repo: workedRepo}}}
		},
	}
	r.record(record.Issue{Key: leftPullRequestKey, Tree: leftPullRequestKey, Project: "LEGION", Title: "the issue that moved",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true,
		LastDispatchSeq: 1, Repo: leftRepo, DefaultBranch: "main", GitHubRepo: leftRepo})
	return r
}

// record writes the issue as the workflow records it.
func (r *leftPullRequestRig) record(issue record.Issue) {
	r.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutIssue(ctx, tx, issue)
	}); err != nil {
		r.t.Fatalf("record %s: %v", issue.Key, err)
	}
}

// opens records a pull request of the issue on repo, in state, as a GitHub fact of that
// repository does (workflow's pullRequestOpened).
func (r *leftPullRequestRig) opens(repo string, number int, state record.PullRequestState) {
	r.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: leftPullRequestKey, Repo: repo, Number: number,
			Branch: "legion/" + leftPullRequestKey, HeadSHA: "c4a7f0b", State: state})
	}); err != nil {
		r.t.Fatalf("record the pull request of %s: %v", leftPullRequestKey, err)
	}
}

// corrects moves the record onto repo at branch, keeping the repository it left and the moment,
// as the workflow's own pass does (workflow.(*Engine).ApplyRepositoryChange).
func (r *leftPullRequestRig) corrects(repo, branch string) {
	r.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		corrected, err := r.records.CorrectIssueRepository(ctx, tx, leftPullRequestKey, repo, branch, repo, leftPullRequestMoment)
		if err != nil {
			return err
		}
		if !corrected {
			r.t.Fatalf("the record of %s was not corrected onto %s", leftPullRequestKey, repo)
		}
		return nil
	}); err != nil {
		r.t.Fatalf("correct %s onto %s: %v", leftPullRequestKey, repo, err)
	}
}

// task is the addressing sentence the next launch of the issue's implementer is told.
func (r *leftPullRequestRig) task() string {
	r.t.Helper()
	spec, err := r.specs.SpawnSpec(context.Background(), supervise.Claim{Token: r.token, Issue: leftPullRequestKey,
		Tree: leftPullRequestKey, Role: claim.RoleImplementer})
	if err != nil {
		r.t.Fatalf("the launch of %s: %v", r.token, err)
	}
	return spec.Prompt.Addressing
}

// A correction that left an open pull request on the repository the issue has left names it in
// the next task of every claim of the issue, beside both repositories: the pull request's number
// and the repository it is open on, and that the daemon applies no event of it to this issue any
// more. Nothing else is sent — no notice of its own — and the sentences are rendered at every
// launch from then on, as the correction's own are.
func TestRepositoryCorrectionNamesThePullRequestLeftOnTheRepositoryTheIssueLeft(t *testing.T) {
	r := newLeftPullRequestRig(t)
	r.opens(leftRepo, leftPullRequestNumber, record.PullRequestOpen)
	r.corrects(workedRepo, "release/v7.1")

	task := r.task()
	if !strings.HasSuffix(task, correctionSentences+" "+leftPullRequestSentences) {
		t.Fatalf("the task of a corrected issue whose pull request is open on the repository it left = %q, want it to end with %q",
			task, correctionSentences+" "+leftPullRequestSentences)
	}
	// Both repositories are named with it: the one the issue works now, before the correction's
	// sentences, and the one it left, in them and in the pull request's own.
	if !strings.Contains(task, "Your issue's repository is zeta/tools (default branch release/v7.1);") {
		t.Fatalf("the task = %q, want the repository the issue works now named before the correction", task)
	}
	// The second launch of the same claim is told it again: the pull request stays on acme/widgets
	// for the life of the issue, so nothing stops saying so.
	if again := r.task(); again != task {
		t.Fatalf("the second launch's task = %q, want the first's %q", again, task)
	}
}

// Where no pull request of the issue stands on a repository the issue has left, the task carries
// no such sentence: a corrected issue with no pull request recorded at all, one whose pull request
// was merged or closed before the correction, and one whose pull request is on the repository the
// issue works now — a pull request opened after the correction, which nothing left behind. The
// correction's own sentences are told in every one of them.
func TestRepositoryCorrectionNamesNoPullRequestWhereNoneIsOpenOnARepositoryTheIssueLeft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opens func(r *leftPullRequestRig)
	}{
		{name: "no pull request recorded"},
		{name: "merged before the correction", opens: func(r *leftPullRequestRig) {
			r.opens(leftRepo, leftPullRequestNumber, record.PullRequestMerged)
		}},
		{name: "closed before the correction", opens: func(r *leftPullRequestRig) {
			r.opens(leftRepo, leftPullRequestNumber, record.PullRequestClosed)
		}},
		{name: "open on the repository the issue works now", opens: func(r *leftPullRequestRig) {
			r.opens(workedRepo, 17, record.PullRequestOpen)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLeftPullRequestRig(t)
			if tc.opens != nil {
				tc.opens(r)
			}
			r.corrects(workedRepo, "release/v7.1")
			task := r.task()
			if !strings.HasSuffix(task, correctionSentences) {
				t.Fatalf("the task = %q, want it to end with the correction's own sentences %q", task, correctionSentences)
			}
			if strings.Contains(task, "The pull request") {
				t.Fatalf("the task = %q, want no sentence about a pull request left behind", task)
			}
		})
	}
}

// An issue no correction has reached is told nothing of either kind, whatever its pull request:
// there is no repository it left, so its pull request is on the repository it works and there is
// nothing to say.
func TestRepositoryCorrectionNamesNoPullRequestForAnIssueNoCorrectionHasReached(t *testing.T) {
	r := newLeftPullRequestRig(t)
	r.opens(leftRepo, leftPullRequestNumber, record.PullRequestOpen)
	task := r.task()
	if strings.Contains(task, "The pull request") || strings.Contains(task, "changed your issue's repository") {
		t.Fatalf("the task of an issue no correction has reached = %q, want neither the correction's sentences nor the pull request's", task)
	}
	if !strings.HasSuffix(task, "the project's repositories are acme/widgets, zeta/tools.") {
		t.Fatalf("the task = %q, want it to end with the repositories clause", task)
	}
}
