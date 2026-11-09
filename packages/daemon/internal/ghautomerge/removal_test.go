package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// failedRun is the merge_group run whose checks failed on the commit the merge queue built for #42.
const failedRun = "https://github.com/acme/widgets/actions/runs/7"

// unexplained is a hypothetical queue removal reason, one GitHub gives no other meaning: GitHub's
// own are merged, failed_checks, manual and merge_conflict, and this stands for any it adds.
const unexplained = "timed_out"

// enqueued and enabled are GitHub's timeline records of the pull request entering the merge queue
// and of its auto-merge being enabled.
const (
	enqueued = `{"__typename":"AddedToMergeQueueEvent","createdAt":"2026-10-16T05:00:00Z"}`
	enabled  = `{"__typename":"AutoMergeEnabledEvent","createdAt":"2026-10-16T05:00:00Z"}`
)

// pagedChecks is a merge queue commit whose checks GitHub lists over two pages: the first, cut at
// 100, all passed, and its integration failure is on the second, secondChecksPage, which moreChecks
// reads after the cursor after-100.
var pagedChecks = func() string {
	var passing []string
	for i := range 100 {
		passing = append(passing, fmt.Sprintf(`{"__typename":"CheckRun","name":"check-%d","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"%s/job/%d","checkSuite":{"workflowRun":{"url":"%s"}}}`, i, failedRun, i, failedRun))
	}
	return `{"oid":"group-sha","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":true,"endCursor":"after-100"},"nodes":[` + strings.Join(passing, ",") + `]}}}`
}()

const secondChecksPage = `{"pageInfo":{"hasNextPage":false,"endCursor":"after-101"},"nodes":[` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/200","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}`

// dequeued is GitHub's record of the pull request taken out of the merge queue by actor for reason,
// with commit, the commit the queue built for it, or null.
func dequeued(reason, actor, commit string) string {
	return fmt.Sprintf(`{"__typename":"RemovedFromMergeQueueEvent","id":"RFMQE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"actor":{"login":%q},"beforeCommit":%s}`, reason, actor, commit)
}

// disableReasons is GitHub's sentence for each auto-merge reason code these tests use, as its
// timeline answers them.
var disableReasons = map[string]string{
	"manually_disabled":    "Manually disabled by user",
	"merge_commit_blocked": "Merge commits are not allowed on this repository",
	"closed":               "Pull request was closed",
}

// disabled is GitHub's record of the pull request's auto-merge turned off for reasonCode, with its
// sentence, by actor, or naming no actor when actor is empty.
func disabled(reasonCode, actor string) string {
	reason, ok := disableReasons[reasonCode]
	if !ok {
		panic("no sentence of GitHub's for auto-merge reason code " + reasonCode)
	}
	login := "null"
	if actor != "" {
		login = fmt.Sprintf(`{"login":%q}`, actor)
	}
	return fmt.Sprintf(`{"__typename":"AutoMergeDisabledEvent","id":"AMDE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"reasonCode":%q,"actor":%s}`, reason, reasonCode, login)
}

// Once the row has seen READY's head armed or queued (armed), an open pull request at that head with
// nothing armed is a removal someone made, which Submit reads from the pull request's timeline and
// never answers with a second submission (LEGION-800). The merge queue taking it out because the
// merge failed - GitHub's reason failed_checks, or a reason GitHub gives no other meaning with a run
// that failed on the commit the queue built - is MergeFailed, naming each failing check and its run
// and not one that passed. Anyone else's removal since the last arm (a person's dequeue, a person
// turning auto-merge off) is Removed, naming who, GitHub's reason and whether it was the queue,
// and the queue finding a conflict is a ConflictRemoval instead, no Removed at all, carrying the
// merge state GitHub answered for the pull request once the entry was gone: a head that conflicts
// with the base itself reads DIRTY, a head GitHub still merges cleanly conflicted with an entry
// ahead of it in the queue, and GitHub answers UNKNOWN where it has computed no state at all, so
// what that removal means is the merge state's to say and the daemon's to decide (INFRA-1442).
// The queue's removal for the merge itself waits for GitHub to show the
// merge, routinely, and a timeline with no removal since the arm waits on GitHub too, but not
// routinely. Only removals that restart the submission submit again: the App submitting's own
// (GraphQL's viewer: the daemon's disarm landing late), and auto-merge a close turned off, which the
// reopen at READY's head undid. Only a queue removal for failed_checks, or for a reason GitHub gives
// no other meaning, reads its commit's checks past the first page: the merge, a person's dequeue and
// a conflict mean what they say, so they read none, and a page GitHub fails to answer never turns
// the merge's wait into a failure.
// The name is the one the issue's check k3 runs (`go test ./internal/ghautomerge/ -run
// TestARemoval`), since this is the table every removal reason is read by: the test the review
// asked be rewritten rather than joined by a second one, folded back under that prefix.
func TestARemovalTheSubmissionDidNotMakeIsReadInsteadOfSubmittingAgain(t *testing.T) {
	red := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"IN_PROGRESS","conclusion":null,"detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"TIMED_OUT","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"StatusContext","context":"freeze","state":"SUCCESS","targetUrl":"https://example.com/freeze"}]}}}`
	green := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// cancelled is the queue's commit after a person dequeues it while its merge_group runs are in
	// flight: they end cancelled, which is no failed merge.
	cancelled := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"CANCELLED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"COMPLETED","conclusion":"SKIPPED","detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// needsAction is the queue's commit with a run that ended needing action, which Legion's rule for
	// a check run counts as failed (classify.RunResult).
	needsAction := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"ACTION_REQUIRED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	for _, tc := range []struct {
		name  string
		queue bool
		// mergeState is what GitHub answers for the pull request in the read that found nothing
		// armed, where a row needs a state of its own: a conflict removal is read by it, and
		// every other row takes the state its base implies (CLEAN queued, BLOCKED not).
		mergeState string
		timeline   []string
		// failPages fails every read of a further page of the queue commit's checks, and pages is how
		// many such reads Submit makes.
		failPages bool
		pages     int
		// failed names what MergeFailed must name; removed what Removed must; conflict what the
		// queue's ConflictRemoval must, which is no Removed and ends no merge row here; wait the
		// wait, merging (GitHub's own merge, routine) or disappeared (not routine); resubmit is a
		// second submission.
		failed, removed, conflict []string
		wait                      string
		resubmit                  bool
	}{
		{name: "a person dequeues it while its merge group runs", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", cancelled)},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue removes it for an unexplained reason on a merge group that only cancelled", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", cancelled)},
			removed: []string{"github-merge-queue", "merge queue", unexplained}},
		{name: "the merge queue removes it for an unexplained reason on a merge group whose run needs action", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", needsAction)},
			failed: []string{unexplained, "integration (action_required, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{"failed_checks", "integration (failure, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{unexplained, "integration (failure, " + failedRun + ")"}},
		{name: "a person dequeues it from a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", pagedChecks)},
			failPages: true, removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue finds a conflict in a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", pagedChecks)},
			failPages: true, conflict: []string{"#42", "head", "merge_conflict", "CLEAN"}},
		{name: "the merge queue takes it out to merge a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", pagedChecks)},
			failPages: true, wait: "merging"},
		{name: "the merge queue removes it for failed_checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", red)},
			failed: []string{"#42", "head", "failed_checks", "group-sha", "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason on a red merge group", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", red)},
			failed: []string{unexplained, "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks naming no commit", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", "null")},
			failed: []string{"failed_checks", "named no commit"}},
		{name: "the merge queue removes it for failed_checks with no failing run read", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", green)},
			failed: []string{"failed_checks", "no failing run", "group-sha"}},
		{name: "a person dequeues it", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual", "#42", "head"}},
		{name: "the merge queue finds a conflict", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			conflict: []string{"#42", "head", "merge_conflict", "CLEAN"}},
		{name: "the merge queue finds a conflict with the base itself", queue: true, mergeState: "DIRTY",
			timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			conflict: []string{"#42", "head", "merge_conflict", "DIRTY"}},
		{name: "the merge queue finds a conflict whose merge state GitHub has not computed", queue: true, mergeState: "UNKNOWN",
			timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			conflict: []string{"#42", "head", "merge_conflict", "UNKNOWN"}},
		{name: "a person turns its auto-merge off", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer")},
			removed: []string{"a-maintainer turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "auto-merge is turned off with GitHub naming no actor", timeline: []string{enabled, disabled("manually_disabled", "")},
			removed: []string{"someone turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "GitHub turns its auto-merge off, naming the App that enabled it", timeline: []string{enabled, disabled("merge_commit_blocked", "legion-implementer")},
			removed: []string{"GitHub turned off the auto-merge", "naming legion-implementer, the App that submitted it", "merge_commit_blocked: Merge commits are not allowed on this repository"}},
		{name: "the daemon's own disarm lands late", timeline: []string{enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "the daemon's own disarm, then a person's dequeue", queue: true, timeline: []string{enqueued, disabled("manually_disabled", "legion-implementer"), dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "a person's removal before the last arm, then the daemon's own", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer"), enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "a close a reopen at READY's head undid", timeline: []string{enabled, disabled("closed", "a-maintainer")}, resubmit: true},
		{name: "the merge queue takes it out to merge it", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", green)}, wait: "merging"},
		{name: "the timeline names no removal since the arm", queue: true, timeline: []string{dequeued("failed_checks", "github-merge-queue", red), enqueued}, wait: "disappeared"},
		{name: "the timeline names no arm or removal at all", wait: "disappeared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A merge-queue base GitHub reports mergeable, or a repository that allows auto-merge
			// whose required checks still run: a second submission would be an enable either way.
			mergeState := "BLOCKED"
			if tc.queue {
				mergeState = "CLEAN"
			}
			if tc.mergeState != "" {
				mergeState = tc.mergeState
			}
			g := &github{head: "head", mergeState: mergeState, queueRequired: tc.queue, autoMergeOK: true, timeline: tc.timeline,
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: tc.failPages}
			client, _ := clients(t, g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var conflict *ConflictRemoval
			var merging *Merging
			var disappeared *Disappeared
			var routine interface{ Routine() bool }
			var armed *Armed
			switch {
			case tc.failed != nil:
				if !errors.As(err, &failed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want MergeFailed", err, err)
				}
				if failed.Event != "RFMQE_9" {
					t.Fatalf("MergeFailed event %q, want GitHub's id for the removal", failed.Event)
				}
				for _, want := range tc.failed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("MergeFailed %q, want it to name %q", err, want)
					}
				}
				for _, passed := range []string{"lint", "e2e", "freeze"} {
					if strings.Contains(err.Error(), passed) {
						t.Fatalf("MergeFailed %q names %s, which did not fail", err, passed)
					}
				}
			case tc.removed != nil:
				if !errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) || removed.Queue != tc.queue {
					t.Fatalf("Submit = %v (%T), want Removed from the merge queue %t", err, err, tc.queue)
				}
				for _, want := range tc.removed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Removed %q, want it to name %q", err, want)
					}
				}
				if (removed.Detail == "") != removed.Queue {
					t.Fatalf("Removed %q with Detail %q, want GitHub's sentence in Detail for an auto-merge disable and none for a queue removal", err, removed.Detail)
				}
			case tc.conflict != nil:
				if !errors.As(err, &conflict) || errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want a ConflictRemoval that is no Removed: the daemon decides that removal", err, err)
				}
				for _, want := range tc.conflict {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("ConflictRemoval %q, want it to name %q", err, want)
					}
				}
				if conflict.MergeState != mergeState {
					t.Fatalf("ConflictRemoval at merge state %q, want the %q GitHub answered once the entry was gone", conflict.MergeState, mergeState)
				}
				ended := conflict.Ended()
				if !ended.Queue || ended.Actor != "github-merge-queue" || ended.Reason != "merge_conflict" || ended.Head != "head" || ended.Number != 42 {
					t.Fatalf("ConflictRemoval ended as %+v, want the merge queue's removal of #42 at head for merge_conflict", *ended)
				}
			case tc.wait == "merging":
				if !errors.As(err, &merging) || !errors.Is(err, wait.ErrWaiting) || !merging.Routine() {
					t.Fatalf("Submit = %v (%T), want a routine Merging wait", err, err)
				}
			case tc.wait == "disappeared":
				if !errors.As(err, &disappeared) || !errors.Is(err, wait.ErrWaiting) || errors.As(err, &routine) && routine.Routine() {
					t.Fatalf("Submit = %v (%T), want a Disappeared wait that is not routine", err, err)
				}
			case tc.resubmit:
				if !errors.As(err, &armed) {
					t.Fatalf("Submit = %v (%T), want READY's head submitted again and an Armed wait", err, err)
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			var want []string
			if tc.resubmit {
				want = []string{submitted("enable")}
			}
			if !slices.Equal(g.mutations, want) {
				t.Fatalf("mutations %v, want %v", g.mutations, want)
			}
			if g.pageReads != tc.pages {
				t.Fatalf("reads of the queue commit's further checks %d, want %d", g.pageReads, tc.pages)
			}
			// Deciding a removal reads no log at all: Submit answers each failing run with the
			// Actions job it ran as, and what that job's log says is read afterwards, once, by
			// the caller that tells of the removal (ReadFailingTests, the daemon's readyMerge).
			// So a submission a row retries — an UNKNOWN merge state it waits on, a fact whose
			// apply failed — costs GitHub no read it has already made.
			if g.logReads != 0 {
				t.Fatalf("job logs read %d while the removal was decided, want none", g.logReads)
			}
		})
	}
}

// A queue removal whose checks GitHub fails to answer past their first page is a failure the row
// retries, never a judgment of the page it read: failed_checks judged on that page alone would say
// GitHub reported no failing run, and an unexplained reason would read as a removal by someone. The
// retry, with the page answered, finds the failure on it.
func TestSubmitRetriesAQueueRemovalWhoseChecksGitHubFailsToRead(t *testing.T) {
	for _, reason := range []string{"failed_checks", unexplained} {
		t.Run(reason, func(t *testing.T) {
			g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, autoMergeOK: true,
				timeline:     []string{enqueued, dequeued(reason, "github-merge-queue", pagedChecks)},
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: true}
			client, _ := clients(t, g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var refused *Refused
			if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &failed) || errors.As(err, &removed) || errors.As(err, &refused) ||
				strings.Contains(err.Error(), "no failing run") || !strings.Contains(err.Error(), "after-100") {
				t.Fatalf("Submit with the queue commit's second page of checks unanswered = %v (%T), want a failure to retry naming the page", err, err)
			}
			g.mu.Lock()
			g.failPages = false
			g.mu.Unlock()
			_, err = Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			if !errors.As(err, &failed) || !strings.Contains(err.Error(), "integration (failure, "+failedRun+")") {
				t.Fatalf("the retry once GitHub answers the page = %v (%T), want MergeFailed naming the failure on it", err, err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.mutations) != 0 || g.pageReads != 2 {
				t.Fatalf("mutations %v and page reads %d, want none and one each run", g.mutations, g.pageReads)
			}
		})
	}
}

// failedGroupWithJob is the commit the merge queue built, with one check run that passed and one,
// integration, that failed as Actions job failingJob.
const failedGroupWithJob = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","databaseId":1,"checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/2","databaseId":128479266994,"checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`

// appCheckGroup is the commit the merge queue built with one failing check run GitHub names no
// workflow run for: a third-party App's, which is no Actions job however GitHub numbers it, and
// whose log route answers 404.
const appCheckGroup = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"licence-scan","status":"COMPLETED","conclusion":"FAILURE",` +
	`"detailsUrl":"https://scanner.example/checks/9","databaseId":128479266994,"checkSuite":{"workflowRun":null}}]}}}`

// A removal names each run that failed on the commit the queue built with the Actions job it ran
// as, and says what that job's log states failed once its caller has read the logs
// (ReadFailingTests), so the tree the merge queue ejected reads the cause instead of fetching and
// grepping a merge-group log (INFRA-1442). Deciding the removal reads nothing: the removal's own
// sentence names the runs alone until the read, which the daemon's row makes once it is about to
// tell of it (its readyMerge). The runs of the queue's conflict removal are read the same way,
// where the daemon decides to post it on the issue, and a check run GitHub names no workflow run
// for — a third-party App's — is named by its details URL and read for nothing, since reading it
// would add "the fetch of its log failed" to a removal nothing is wrong with. What one job's line
// says is read of jobFailures itself (failingtests_test.go).
func TestARemovalsFailingRunsAreReadOnceItIsTold(t *testing.T) {
	for _, tc := range []struct {
		name string
		// group is the commit the queue built, conflict reads the removal as the queue's
		// conflict, which the daemon submits once more, rather than as the failed merge the
		// other rows are, and reads is how many logs the telling reads.
		group    string
		conflict bool
		reads    int
		// runs is what the removal must name before any log is read, and says what the one line
		// of each must say once they are.
		runs   []FailingRun
		says   []string
		absent []string
	}{
		{name: "a failing job whose log names the tests that failed", group: failedGroupWithJob, reads: 1,
			runs: []FailingRun{{Name: "integration", Result: "failure", URL: failedRun, JobID: failingJob}},
			says: []string{"integration (failure, " + failedRun + ")", "its log names TestTheQueueTakesTheEntryOut"}},
		{name: "a conflict removal whose commit carries a failing run", group: failedGroupWithJob, conflict: true, reads: 1,
			runs: []FailingRun{{Name: "integration", Result: "failure", URL: failedRun, JobID: failingJob}},
			says: []string{"integration (failure, " + failedRun + ")", "its log names TestTheQueueTakesTheEntryOut"}},
		{name: "a check run GitHub names no workflow run for", group: appCheckGroup,
			runs:   []FailingRun{{Name: "licence-scan", Result: "failure", URL: "https://scanner.example/checks/9"}},
			says:   []string{"licence-scan (failure, https://scanner.example/checks/9)"},
			absent: []string{"could not read", "its log names"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := "failed_checks"
			if tc.conflict {
				reason = "merge_conflict"
			}
			g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, autoMergeOK: true,
				timeline: []string{enqueued, dequeued(reason, "github-merge-queue", tc.group)},
				jobLogs:  map[int64]string{failingJob: goLog}}
			client, logs := clients(t, g)
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var conflict *ConflictRemoval
			var runs []FailingRun
			switch {
			case tc.conflict:
				if !errors.As(err, &conflict) {
					t.Fatalf("Submit = %v (%T), want the queue's ConflictRemoval", err, err)
				}
				// A ConflictRemoval is no *Removed, so what the daemon hands the read is the
				// runs of the removal it carries, where it decides to tell of it (its
				// conflictRemoval).
				runs = conflict.Removal.Runs
			default:
				if !errors.As(err, &failed) {
					t.Fatalf("Submit = %v (%T), want MergeFailed: no read of a log fails the row", err, err)
				}
				runs = failed.Runs
			}
			if !slices.Equal(runs, tc.runs) {
				t.Fatalf("the removal names %+v, want %+v: the job to read and nothing read yet", runs, tc.runs)
			}
			if text := err.Error(); strings.Contains(text, "its log names") || strings.Contains(text, "could not read") {
				t.Fatalf("the removal %q, want it to say nothing of any log before one is read", text)
			}
			g.mu.Lock()
			read := g.logReads
			g.mu.Unlock()
			if read != 0 {
				t.Fatalf("job logs read %d while the removal was decided, want none", read)
			}
			ReadFailingTests(context.Background(), logs, runs)
			line := runs[0].String()
			for _, want := range tc.says {
				if !strings.Contains(line, want) {
					t.Fatalf("the failing run's line %q, want it to say %q", line, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(line, absent) {
					t.Fatalf("the failing run's line %q says %q, want it left out", line, absent)
				}
			}
			if !strings.Contains(err.Error(), line) {
				t.Fatalf("the removal %q, want it to carry the failing run's line %q", err, line)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.logReads != tc.reads {
				t.Fatalf("job logs read %d once the caller read them, want %d: a check that passed, and one that is no Actions job, have no log to read", g.logReads, tc.reads)
			}
		})
	}
}
