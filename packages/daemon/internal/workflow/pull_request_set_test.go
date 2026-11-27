package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// linkedAt is GitHub's clock of the reads of linked pull requests these tests apply.
var linkedAt = time.Date(2026, 11, 25, 3, 46, 0, 0, time.UTC)

// linksOf is Dispatch's event of key at seq, at status, that names the issue's links whole: each
// of links, as owner/name and number.
func linksOf(key string, seq int64, status string, links ...intake.PullRequestLink) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, ResolvedRepo: "sjawhar/legion", Seq: seq, Type: "issue.updated", Status: status, Title: "root", Rank: "U",
		Links: append([]intake.PullRequestLink{}, links...), LinksKnown: true}
}

// linkRead is the daemon's read of pull request number of repo that key links, which GitHub
// answered state at a head of repo itself on branch onto main.
func linkRead(key, repo string, number int, state record.PullRequestState, branch string) intake.PullRequestLinkReadDone {
	read := intake.PullRequestLinkReadDone{Issue: key, Repo: repo, Number: number, State: state, HeadSHA: fmt.Sprintf("head-%d", number), HeadRepo: repo,
		Branch: branch, Base: "main", URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), UpdatedAt: linkedAt}
	if state == record.PullRequestMerged {
		read.MergeSHA = fmt.Sprintf("merge-%d", number)
	}
	return read
}

// storedLinks is each link of key the record holds (record.Store.PullRequestLinksOf).
func storedLinks(t *testing.T, pool *pgxpool.Pool, key string) []record.PullRequestLink {
	t.Helper()
	var links []record.PullRequestLink
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		links, err = record.NewStore().PullRequestLinksOf(t.Context(), tx, key)
		return err
	})
	return links
}

// planRoot is LEGION-208 in testing (tasksRoot) with a plan of one task, a, running at its tester's
// step on its own branch legion/LEGION-208.a.
func planRoot(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := tasksRoot(t, pool)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"tester:a"}, agents: []claim.Agent{"test-a"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208.a")
	return root
}

// The set of an issue's pull requests is the pull request of each of its tasks and each pull
// request it links in Dispatch, in any repository, and no other (dispatch://INFRA-5910 D2, D3,
// decisions 4, 5, 8 and 18). These are the workflow's rows of it; intake holds a test of this name
// for the decode of the links. Each row is a record of its own, its set read as the store reads it
// (record.Store.PullRequestSetsOf):
//   - a task's pull request, opened on its branch, is in the set;
//   - a pull request on the branch of a task a later version removed leaves the set with its task,
//     unless the issue links it;
//   - a pull request of another repository that the issue links is in the set, whatever its branch,
//     a task's branch name among them, and it is no task's;
//   - a pull request of another repository that names the issue in its branch and its body, and that
//     the issue does not link, is in no set;
//   - a linked pull request whose head is in a fork is refused as a fork's, with one pr-link-refused
//     however often it is read, and is in no set;
//   - a link to a pull request the record holds for another issue is refused as that issue's, with
//     one pr-link-refused however often the link is named, and the row stays with its issue;
//   - a pull request linked after the retro began joins the set;
//   - a removed link takes its pull request out of the set and deletes its row, open or merged, and
//     keeps the row of one on a task's branch, which stays in the set as the task's;
//   - an event that names no links changes none.
//
// The faults it catches: a set taken from the branch name or the body, which fails the rows of the
// linked pull request of another repository and of the unlinked one; a set that keeps a removed
// task's pull request, or that drops a linked one with the task, which fails the removed task's
// row; a set that takes a fork's pull request; a link that moves another issue's row; a set read
// once and never again, which fails the row of the link after the retro began; and links unknown
// read as links none, which fails the last row.
func TestThePullRequestsOfAnIssueAreItsTasksAndItsLinks(t *testing.T) {
	t.Parallel()
	const status = "in_progress"
	deploy := func(number int) intake.PullRequestLink {
		return intake.PullRequestLink{Repo: "acme/deploy", Number: number}
	}
	assertSet := func(t *testing.T, pool *pgxpool.Pool, key, after string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if got := pullRequestSetOf(t, pool, key); !slices.Equal(got, want) {
			t.Errorf("after %s the set of %s is %v, want %v", after, key, got, want)
		}
	}

	t.Run("a task's pull request is in the set", func(t *testing.T) {
		pool := migratedPool(t)
		root := planRoot(t, pool)
		mustSpend(t, "the open of a's #44", applyFacts(t, pool, stepEngine(nil))("opened-44", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 44,
			Branch: "legion/LEGION-208.a", Base: "main", HeadSHA: "head-a", HeadRepo: "sjawhar/legion", UpdatedAt: linkedAt}))
		assertSet(t, pool, root.Key, "the open of a's pull request", "sjawhar/legion#44")
	})

	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("the pull request of a task a later version removed, linked %t", linked), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Testing)
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "a", steps: []string{"tester:a"}, agents: []claim.Agent{"test-a"}, state: record.TaskRunning, place: atStep(1)},
				planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskStopped, place: atStep(1)},
				planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
			seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208")
			var bBranch string
			seedRecord(t, pool, func(tx pgx.Tx) (err error) {
				bBranch, err = record.NewStore().AssignTaskBranch(t.Context(), tx, root.Key, root.Generation, "b")
				return err
			})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 45,
				Branch: bBranch, Base: "main", HeadSHA: "head-b", Failing: []string{}})
			apply := applyFacts(t, pool, engine)
			if linked {
				mustSpend(t, "the link of #45", apply("linked-45", linksOf(root.Key, 2, status, intake.PullRequestLink{Repo: "sjawhar/legion", Number: 45})))
			}
			mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2,
				keyed(t, "a", "", []string{"tester:a"}), keyed(t, "z", "", []string{"reviewer:z"}, "a"))))
			if linked {
				assertSet(t, pool, root.Key, "version 2 removed b", "sjawhar/legion#42", "sjawhar/legion#45")
				if got := closedNumbers(t, pool, root.Key, "Task b was removed from the plan."); len(got) != 0 {
					t.Errorf("version 2 closed %v, want none: the issue links #45, which the stack never closes", got)
				}
				return
			}
			assertSet(t, pool, root.Key, "version 2 removed b", "sjawhar/legion#42")
		})
	}

	t.Run("a pull request of another repository the issue links is in the set whatever its branch", func(t *testing.T) {
		pool := migratedPool(t)
		root := planRoot(t, pool)
		apply := applyFacts(t, pool, stepEngine(nil))
		mustSpend(t, "the links", apply("linked", linksOf(root.Key, 2, status, deploy(6990), deploy(6991))))
		mustSpend(t, "the read of #6990", apply("read-6990", linkRead(root.Key, "acme/deploy", 6990, record.PullRequestOpen, "fix/kept")))
		mustSpend(t, "the read of #6991", apply("read-6991", linkRead(root.Key, "acme/deploy", 6991, record.PullRequestOpen, "legion/LEGION-208.a")))
		assertSet(t, pool, root.Key, "the reads of the linked pull requests", "acme/deploy#6990", "acme/deploy#6991")
		if pr := pullRequestOf(t, pool, "acme/deploy", 6991); pr == nil || !pr.Linked || pr.Task != "" {
			t.Errorf("#6991 of acme/deploy is %+v, want it linked and no task's: a task's branch is of the issue's own repository", pr)
		}
	})

	t.Run("an unlinked pull request of another repository that names the issue is in no set", func(t *testing.T) {
		pool := migratedPool(t)
		root := tasksRoot(t, pool)
		mustSpend(t, "the open of acme/deploy#6992", applyFacts(t, pool, stepEngine(nil))("opened-6992", intake.PullRequestOpened{Repo: "acme/deploy",
			Number: 6992, Branch: "legion/LEGION-208", Base: "main", HeadSHA: "head-6992", HeadRepo: "acme/deploy", Body: "Fixes LEGION-208.",
			UpdatedAt: linkedAt}))
		if pr := pullRequestOf(t, pool, "acme/deploy", 6992); pr != nil {
			t.Errorf("acme/deploy#6992 is recorded as %+v, want no row: naming the issue makes no pull request the issue's", pr)
		}
		assertSet(t, pool, root.Key, "the open of acme/deploy#6992")
	})

	t.Run("a fork's linked pull request is refused, told once", func(t *testing.T) {
		pool := migratedPool(t)
		root := tasksRoot(t, pool)
		apply := applyFacts(t, pool, stepEngine(nil))
		mustSpend(t, "the link of #7000", apply("linked-7000", linksOf(root.Key, 2, status, deploy(7000))))
		read := linkRead(root.Key, "acme/deploy", 7000, record.PullRequestOpen, "legion/LEGION-208")
		read.HeadRepo = "stranger/deploy"
		mustSpend(t, "the read of #7000", apply("read-7000", read))
		read.UpdatedAt = read.UpdatedAt.Add(time.Minute)
		mustSpend(t, "the read of #7000 again", apply("read-7000-again", read))
		if links := storedLinks(t, pool, root.Key); len(links) != 1 || links[0].Refused != record.LinkRefusedFork {
			t.Errorf("the links of %s are %+v, want #7000 refused as a fork's", root.Key, links)
		}
		if told := noticesOf(t, pool, root.Key, "pr-link-refused"); len(told) != 1 || !strings.Contains(told[0].Reason, "acme/deploy#7000") {
			t.Errorf("the refusal sent pr-link-refused %+v, want one that names acme/deploy#7000", told)
		}
		if pr := pullRequestOf(t, pool, "acme/deploy", 7000); pr != nil {
			t.Errorf("a fork's pull request is recorded as %+v, want no row", pr)
		}
		assertSet(t, pool, root.Key, "the reads of a fork's linked pull request")
	})

	t.Run("a link to another issue's pull request is refused, told once, and the row stays with its issue", func(t *testing.T) {
		pool := migratedPool(t)
		root := tasksRoot(t, pool)
		seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-209",
			Project: "LEGION", Title: "another root", Phase: phase.Testing, Generation: 1, Status: status, Rank: "V", LastDispatchSeq: 1})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 50,
			Branch: "legion/LEGION-209", Base: "main", HeadSHA: "head-50", Failing: []string{}})
		apply := applyFacts(t, pool, stepEngine(nil))
		theirs := intake.PullRequestLink{Repo: "sjawhar/legion", Number: 50}
		mustSpend(t, "the link of LEGION-209's #50", apply("linked-50", linksOf(root.Key, 2, status, theirs)))
		mustSpend(t, "the link named again", apply("linked-50-again", linksOf(root.Key, 3, status, theirs)))
		if links := storedLinks(t, pool, root.Key); len(links) != 1 || links[0].Refused != record.LinkRefusedOtherIssue {
			t.Errorf("the links of %s are %+v, want #50 refused as another issue's", root.Key, links)
		}
		if told := noticesOf(t, pool, root.Key, "pr-link-refused"); len(told) != 1 ||
			!strings.Contains(told[0].Reason, "sjawhar/legion#50") || !strings.Contains(told[0].Reason, "LEGION-209") {
			t.Errorf("the refusal sent pr-link-refused %+v, want one that names sjawhar/legion#50 and LEGION-209", told)
		}
		if pr := pullRequestOf(t, pool, "sjawhar/legion", 50); pr == nil || pr.Issue != "LEGION-209" {
			t.Errorf("#50 is %+v, want it LEGION-209's still", pr)
		}
		assertSet(t, pool, root.Key, "the link to LEGION-209's #50")
		assertSet(t, pool, "LEGION-209", "the link to its #50 by LEGION-208", "sjawhar/legion#50")
	})

	t.Run("a pull request linked after the retro began joins the set", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
			Project: "LEGION", Title: "root", Phase: phase.Retro, Generation: 1, Status: PhaseStatus(phase.Retro), Rank: "U", LastDispatchSeq: 1})
		seedTreeLifecycle(t, pool, "LEGION-208")
		apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
		mustSpend(t, "the link of #6990", apply("linked-6990", linksOf("LEGION-208", 2, PhaseStatus(phase.Retro), deploy(6990))))
		mustSpend(t, "the read of #6990", apply("read-6990", linkRead("LEGION-208", "acme/deploy", 6990, record.PullRequestOpen, "fix/kept")))
		assertSet(t, pool, "LEGION-208", "a link in the retro", "acme/deploy#6990")
		if got := storedIssue(t, pool, "LEGION-208"); got.Phase != phase.Retro {
			t.Errorf("after the link %s stands in %s, want retro: a link moves no phase", got.Key, got.Phase)
		}
	})

	for _, state := range []record.PullRequestState{record.PullRequestOpen, record.PullRequestMerged} {
		t.Run(fmt.Sprintf("a removed link takes its %s pull request out of the set", state), func(t *testing.T) {
			pool := migratedPool(t)
			root := tasksRoot(t, pool)
			apply := applyFacts(t, pool, stepEngine(nil))
			mustSpend(t, "the link of #6990", apply("linked-6990", linksOf(root.Key, 2, status, deploy(6990))))
			mustSpend(t, "the read of #6990", apply("read-6990", linkRead(root.Key, "acme/deploy", 6990, state, "fix/kept")))
			assertSet(t, pool, root.Key, "the read of #6990", "acme/deploy#6990")
			mustSpend(t, "the removal of the link", apply("unlinked", linksOf(root.Key, 3, status)))
			assertSet(t, pool, root.Key, "the removal of its link")
			if pr := pullRequestOf(t, pool, "acme/deploy", 6990); pr != nil {
				t.Errorf("after its link went acme/deploy#6990 is %+v, want no row", pr)
			}
		})
	}

	t.Run("a removed link keeps the row of a task's pull request", func(t *testing.T) {
		pool := migratedPool(t)
		root := planRoot(t, pool)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 44,
			Branch: "legion/LEGION-208.a", Base: "main", HeadSHA: "head-a", Failing: []string{}})
		apply := applyFacts(t, pool, stepEngine(nil))
		mustSpend(t, "the link of a's #44", apply("linked-44", linksOf(root.Key, 2, status, intake.PullRequestLink{Repo: "sjawhar/legion", Number: 44})))
		if pr := pullRequestOf(t, pool, "sjawhar/legion", 44); pr == nil || !pr.Linked {
			t.Fatalf("after its link a's #44 is %+v, want it linked", pr)
		}
		mustSpend(t, "the removal of the link", apply("unlinked-44", linksOf(root.Key, 3, status)))
		if pr := pullRequestOf(t, pool, "sjawhar/legion", 44); pr == nil || pr.Linked || pr.Task != "a" {
			t.Errorf("after its link went #44 is %+v, want a's, unlinked", pr)
		}
		assertSet(t, pool, root.Key, "the removal of a's pull request's link", "sjawhar/legion#44")
	})

	t.Run("an event that names no links changes none", func(t *testing.T) {
		pool := migratedPool(t)
		root := tasksRoot(t, pool)
		apply := applyFacts(t, pool, stepEngine(nil))
		mustSpend(t, "the link of #6990", apply("linked-6990", linksOf(root.Key, 2, status, deploy(6990))))
		mustSpend(t, "the read of #6990", apply("read-6990", linkRead(root.Key, "acme/deploy", 6990, record.PullRequestOpen, "fix/kept")))
		unknown := linksOf(root.Key, 3, status)
		unknown.Links, unknown.LinksKnown = nil, false
		mustSpend(t, "an event that names no links", apply("links-unknown", unknown))
		if links := storedLinks(t, pool, root.Key); len(links) != 1 || links[0].Number != 6990 {
			t.Errorf("after an event that names no links the links of %s are %+v, want #6990 still", root.Key, links)
		}
		assertSet(t, pool, root.Key, "an event that names no links", "acme/deploy#6990")
	})
}
