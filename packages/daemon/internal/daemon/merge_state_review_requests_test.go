package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The record keeps how many review requests GitHub holds open on a pull request beside its merge
// state, since a `blocked` head with one open waits for that review rather than ending its round
// (classify.MergeOutcomeOf). A GET /pulls/{number} answers them as `requested_reviewers` and
// `requested_teams`, people, Apps and teams alike, and both count; an answer that holds neither
// list is no count at all, which the round does not end on, never a count of zero.
func TestAReadOfOnePullRequestCountsItsOpenReviewRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       *int
	}{
		{"two reviewers and a team", `"requested_reviewers":[{"login":"octocat"},{"login":"legion-reviewer[bot]"}],"requested_teams":[{"slug":"engineering"}]`, new(3)},
		{"two empty lists", `"requested_reviewers":[],"requested_teams":[]`, new(0)},
		{"null lists", `"requested_reviewers":null,"requested_teams":null`, nil},
		{"no lists at all", `"title":"no lists"`, nil},
	} {
		var pull githubPullRequest
		if err := json.Unmarshal([]byte(`{"number":86,"state":"open","mergeable":true,"mergeable_state":"blocked",`+tc.body+`}`), &pull); err != nil {
			t.Fatal(err)
		}
		if got := pull.reviewsRequested(); (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("%s: the read counts %v review requests, want %v", tc.name, got, tc.want)
		}
	}
}

// A base's batch reads the count as `reviewRequests { totalCount }` and the head's rollup as the
// last commit's `statusCheckRollup`, its check runs and commit statuses counted by state, in its
// one query. The rollup speaks of the head only when the last commit is the head the answer
// names; a null rollup is a commit with no check and no status, so nothing of it runs; and the
// counts decide, never the rollup's own state: GitHub answers FAILURE as soon as one context fails
// while others still run (cybermonklabs/legion#4432: BLOCKED, MERGEABLE, rollup FAILURE, one
// check run IN_PROGRESS). A status GitHub counts EXPECTED, required and never reported, is not
// running. A rollup with no counts reads as no rollup.
func TestABatchAnswerReadsTheHeadsRollupOnlyForTheHeadItNames(t *testing.T) {
	node := func(head string, commits map[string]any) graphQLPull {
		var pull graphQLPull
		body := batchNode(86, head, "MERGEABLE", "BLOCKED")
		body["commits"] = commits
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &pull); err != nil {
			t.Fatal(err)
		}
		return pull
	}
	for _, tc := range []struct {
		name   string
		pull   graphQLPull
		checks record.HeadChecks
	}{
		{"every check run succeeded", node("h2", rollupOf("h2", "SUCCESS")), record.HeadChecksConcluded},
		{"a check run failed and none runs", node("h2", rollupCounted("h2", "FAILURE", map[string]int{"FAILURE": 3, "SUCCESS": 65, "SKIPPED": 18}, nil)), record.HeadChecksConcluded},
		{"a check run failed while another runs (#4432)", node("h2", rollupCounted("h2", "FAILURE",
			map[string]int{"FAILURE": 3, "SUCCESS": 65, "SKIPPED": 18, "IN_PROGRESS": 1}, nil)), record.HeadChecksRunning},
		{"a check run queued", node("h2", rollupCounted("h2", "PENDING", map[string]int{"QUEUED": 1, "SUCCESS": 4}, nil)), record.HeadChecksRunning},
		{"a check run waiting on an environment", node("h2", rollupCounted("h2", "PENDING", map[string]int{"WAITING": 1}, nil)), record.HeadChecksRunning},
		{"a commit status pending", node("h2", rollupCounted("h2", "PENDING", map[string]int{"SUCCESS": 2}, map[string]int{"PENDING": 1})), record.HeadChecksRunning},
		{"a required status nobody reported", node("h2", rollupCounted("h2", "FAILURE", map[string]int{"SUCCESS": 2}, map[string]int{"EXPECTED": 1})), record.HeadChecksConcluded},
		{"no check and no status", node("h2", rollupOf("h2", "")), record.HeadChecksConcluded},
		{"a rollup with no counts", node("h2", map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": "h2",
			"statusCheckRollup": map[string]any{"state": "SUCCESS"}}}}}), record.HeadChecksUnread},
		{"the rollup of another commit", node("h2", rollupOf("h1", "SUCCESS")), record.HeadChecksUnread},
		{"no commit at all", node("h2", map[string]any{"nodes": []any{}}), record.HeadChecksUnread},
	} {
		if got := tc.pull.githubPullRequest().batch.checks; got != tc.checks {
			t.Errorf("%s: the rollup reads %q, want %q", tc.name, got, tc.checks)
		}
	}
	nullCount := node("h2", rollupOf("h2", "SUCCESS"))
	nullCount.ReviewRequests = nil
	if got := nullCount.githubPullRequest().reviewsRequested(); got != nil {
		t.Errorf("a null reviewRequests counts %d, want no count", *got)
	}
}

// A base push records the count and the rollup the batch read beside the merge state, so a
// `blocked` head's round decides on what GitHub answered, never on a zero it did not.
func TestABasePushRecordsTheReviewRequestsAndRollupGitHubAnswered(t *testing.T) {
	t.Parallel()
	query := chunkQuery([]int{86})
	for _, field := range []string{"reviewRequests { totalCount }", "checkRunCountsByState { state count }", "statusContextCountsByState { state count }"} {
		if !strings.Contains(query, field) {
			t.Fatalf("the batch query asks %q, want each pull request's %s", query, field)
		}
	}
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		node := batchNode(86, "head-86", "MERGEABLE", "BLOCKED")
		node["reviewRequests"] = map[string]any{"totalCount": 2}
		node["commits"] = rollupOf("head-86", "PENDING")
		return http.StatusOK, batchAnswer(t, asked, node)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the batch's answer for 86 to be recorded", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && rig.ended()
	})
	pr := recordedPull(t, pool, "CAPTURE-1")
	if pr.ReviewsRequested == nil || *pr.ReviewsRequested != 2 || pr.HeadChecks != record.HeadChecksRunning || !classify.EvidenceStands(*pr) {
		t.Fatalf("the record holds %v review requests and checks %q read at %v beside a read at %v for 86, want the 2 and the running rollup GitHub answered, written by that read",
			pr.ReviewsRequested, pr.HeadChecks, pr.EvidenceReadAt, pr.StateReadAt)
	}
}

// An error inside an answered pull request names one of its fields, and GitHub answers that field
// null. A batch that met one reads neither the count nor the rollup of that pull request - they are
// the nullable parts the round decides on - so the record holds no count and no rollup rather than
// none open and nothing running, and the base's reads keep reading it, since a `blocked` answer
// missing either is one the round cannot decide on (classify.MergeAnswerIncomplete).
func TestAFieldErrorLeavesTheCountAndRollupUnread(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		body := batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"))
		if query > 1 {
			return http.StatusOK, body
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Errorf("decode the batch answer: %v", err)
		}
		answer["errors"] = []map[string]any{{"type": "FORBIDDEN", "path": []any{"repository", pullAlias(86), "reviewRequests"},
			"message": "Resource not accessible by integration"}}
		raw, err := json.Marshal(answer)
		if err != nil {
			t.Errorf("encode the batch answer: %v", err)
		}
		return http.StatusOK, string(raw)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the first answer for 86 to be recorded", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked
	})
	if first := recordedPull(t, pool, "CAPTURE-1"); rig.queries.Load() == 1 && (first.ReviewsRequested != nil || first.HeadChecks != record.HeadChecksUnread) {
		t.Fatalf("after the field error the record holds %v review requests and checks %q, want neither read", first.ReviewsRequested, first.HeadChecks)
	}
	testwait.Eventually(t, "the base's reads to read 86 again and end on a whole answer", func() bool {
		pr := recordedPull(t, pool, "CAPTURE-1")
		return rig.queries.Load() >= 2 && pr.ReviewsRequested != nil && pr.HeadChecks == record.HeadChecksConcluded && rig.ended()
	})
}

// A REST read answers no rollup of the head, so a read GitHub answers `blocked` and MERGEABLE asks
// GitHub's GraphQL API for its whole answer before it is applied (readWholeAnswer), and takes the
// count of review requests from that same answer, so the count and the rollup are one response's:
// a review requested between the REST read and the query is in both or in neither. A GraphQL
// answer about another head, or none, is no rollup and no count, which the readers read again for.
func TestAReadOfABlockedHeadAsksForItsRollup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		rollup    string
		head      string
		requested int
		want      record.HeadChecks
		count     *int
	}{
		{"a check still running", "PENDING", "code", 0, record.HeadChecksRunning, new(0)},
		{"every check done", "SUCCESS", "code", 0, record.HeadChecksConcluded, new(0)},
		// The REST answer lists no request, and a review requested before the query is in its
		// answer: the record takes the query's count beside the query's rollup.
		{"a review requested between the two calls", "SUCCESS", "code", 1, record.HeadChecksConcluded, new(1)},
		{"an answer about another head", "SUCCESS", "older", 0, record.HeadChecksUnread, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			captureInTesting(t, pool)
			rest := `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`
			api := pullStandInWith(t, rest, func(asked []int) string {
				node := batchNode(86, tc.head, "MERGEABLE", "BLOCKED")
				node["commits"] = rollupOf(tc.head, tc.rollup)
				node["reviewRequests"] = map[string]any{"totalCount": tc.requested}
				return batchAnswer(t, asked, node)
			})
			w := readRuntime(pool, api.URL, quietLogger())
			state, incomplete, _, err := w.readMergeState(context.Background(), "CAPTURE-1")
			if err != nil || state != record.MergeStateBlocked {
				t.Fatalf("the read answered %s, %v; want blocked", state, err)
			}
			pr := recordedPull(t, pool, "CAPTURE-1")
			if pr.HeadChecks != tc.want || !reflect.DeepEqual(pr.ReviewsRequested, tc.count) {
				t.Fatalf("the record holds checks %q and %v review requests, want %q and %v", pr.HeadChecks, pr.ReviewsRequested, tc.want, tc.count)
			}
			if wantIncomplete := tc.want == record.HeadChecksUnread; incomplete != wantIncomplete {
				t.Fatalf("the read is incomplete = %t, want %t", incomplete, wantIncomplete)
			}
		})
	}
}

// The shape cybermonklabs/legion#4432 was read in: GitHub answers the head BLOCKED and MERGEABLE
// with its rollup FAILURE, three check runs failed and one still IN_PROGRESS, no review asked for,
// and the head's own settlement concluded. The round waits in reviewing, and nobody is told: a
// check of the head runs. The control is the same head once that run concludes - the rollup still
// FAILURE, nothing running - and the next read ends the round, which moves the issue to accepting.
func TestAFailedRollupBesideARunningCheckKeepsTheRoundUntilTheRunConcludes(t *testing.T) {
	t.Parallel()
	var running atomic.Bool
	running.Store(true)
	pool := isolatedOutboxPool(t)
	approvedCapture(t, pool)
	rest := `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`
	api := pullStandInWith(t, rest, func(asked []int) string {
		runs := map[string]int{"FAILURE": 3, "SUCCESS": 65, "SKIPPED": 18}
		if running.Load() {
			runs["IN_PROGRESS"] = 1
		}
		node := batchNode(86, "code", "MERGEABLE", "BLOCKED")
		node["commits"] = rollupCounted("code", "FAILURE", runs, nil)
		return batchAnswer(t, asked, node)
	})
	w := readRuntime(pool, api.URL, quietLogger())
	w.mergeStateRetry = readRigWait
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.readMergeStates(ctx)
	applyFact(t, pool, "github", "checks-on-code", settledChecksOnCode(), w.handlers...)
	testwait.Eventually(t, "the read of the running head to be recorded", func() bool {
		pr := recordedPull(t, pool, "CAPTURE-1")
		running, waiting := ladders(w)
		return pr.MergeState == record.MergeStateBlocked && pr.HeadChecks == record.HeadChecksRunning && running == 0 && waiting == 0
	})
	if got, _ := capturePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("with a check of the head IN_PROGRESS beside a FAILURE rollup the issue is in %s, want reviewing", got)
	}
	if told := noticeReasons(t, pool, "review-stuck"); len(told) != 0 {
		t.Fatalf("review-stuck notices %q, want none while a check of the head runs", told)
	}
	// Control: the run concludes, and its settlement asks for a read that finds nothing running.
	running.Store(false)
	later := settledChecksOnCode()
	later.Generation, later.Snapshot = 2, "settled-later"
	later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
	applyFact(t, pool, "github", "checks-on-code-concluded", later, w.handlers...)
	testwait.Eventually(t, "the round to end once nothing of the head runs", func() bool {
		got, _ := capturePhase(t, pool)
		return got == phase.Accepting
	})
	if pr := recordedPull(t, pool, "CAPTURE-1"); pr.HeadChecks != record.HeadChecksConcluded || pr.MergeState != record.MergeStateBlocked {
		t.Fatalf("the record holds %s with checks %q, want the same blocked with nothing running", pr.MergeState, pr.HeadChecks)
	}
}

// GitHub answers a `blocked` head `mergeable` null while it computes whether the head merges, and
// may go on doing so. Nothing here injects a read: the record holds a null read of the head two
// minutes old, past classify.NullMergeableFloor, the settlement asks for a read, the ladder reads
// the head again because a null is no answer the round can decide on
// (classify.MergeAnswerIncomplete), and that read, the second null one in a row, tells the
// architect, once, naming the reads. The round stays in reviewing.
func TestAMergeabilityGitHubNeverComputesIsToldOnItsSecondRead(t *testing.T) {
	t.Parallel()
	nullPull := pullAnswer{body: `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":null,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`}
	nullTwoMinutesAgo := func(t *testing.T, pool *store.Pool) {
		t.Helper()
		approvedCapture(t, pool)
		if _, err := pool.Exec(context.Background(), `update pull_requests set merge_state = 'blocked', mergeability = 'UNKNOWN',
			null_mergeable_reads = 1, null_mergeable_since = now() - interval '2 minutes', state_read_at = now() - interval '2 minutes',
			evidence_read_at = now() - interval '2 minutes' where issue = 'CAPTURE-1'`); err != nil {
			t.Fatalf("record the first null read: %v", err)
		}
	}
	rig := newReadRig(t, nullTwoMinutesAgo, func(int32) pullAnswer { return nullPull })
	rig.start(t)
	testwait.Eventually(t, "the review-stuck notice of the null mergeability", func() bool {
		return len(noticeReasons(t, rig.pool, "review-stuck")) > 0
	})
	told := noticeReasons(t, rig.pool, "review-stuck")
	if len(told) != 1 || !strings.Contains(told[0], "GitHub has answered `mergeable` null for it on 2 read(s) in a row") {
		t.Fatalf("review-stuck notices %q, want one naming two null reads in a row", told)
	}
	if got, _ := capturePhase(t, rig.pool); got != phase.Reviewing {
		t.Fatalf("the issue is in %s, want reviewing: GitHub has not said the head merges", got)
	}
	testwait.Eventually(t, "the ladder to read the head past its second read", func() bool { return rig.reads.Load() > 2 })
	if again := noticeReasons(t, rig.pool, "review-stuck"); len(again) != 1 {
		t.Fatalf("review-stuck notices %q after more null reads, want the one", again)
	}
}

// A climb whose first read GitHub refuses - 403 once the REST budget is spent - hands the head to
// its base's reads when no base holds the issue yet, so the head is read again although no event
// is coming: the base's read answers clean, and the round ends. Before, such a climb answered
// nothing and the head stood unread until its next event (dispatch://INFRA-4192 review
// 6049029052).
func TestAClimbWhoseFirstReadFailsHandsTheHeadToItsBase(t *testing.T) {
	t.Parallel()
	rig := newReadRig(t, approvedOnMain, func(int32) pullAnswer { return cleanPull })
	rig.refuseREST.Store(true)
	rig.start(t)
	testwait.Eventually(t, "the base's read to end the round", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Accepting
	})
	const climbRead = "read GitHub's merge state for an issue whose read was asked for"
	refused := loggedLines(t, rig.logs.String(), climbRead)
	if len(refused) != 1 || refused[0]["read"] != float64(1) || refused[0]["handedToBase"] != true {
		t.Fatalf("the climb logged %v failed reads, want its first read refused and the head handed to its base", refused)
	}
	if follows := rig.follows(t); len(follows) == 0 || follows[0]["answered"] != string(record.MergeStateClean) {
		t.Fatalf("the base's reads logged %v, want a read of the handed head answering clean", follows)
	}
}

// approvedOnMain is approvedCapture with the base the pull request's opened event records
// (workflow's pullRequestOpened), so the record names main before any read of GitHub has answered.
func approvedOnMain(t *testing.T, pool *store.Pool) {
	t.Helper()
	approvedCapture(t, pool)
	if _, err := pool.Exec(context.Background(), "update pull_requests set base = 'main' where issue = 'CAPTURE-1'"); err != nil {
		t.Fatalf("record the base: %v", err)
	}
}

// After a restart the start read is what reads each issue's pull request, and a round that waits
// quietly has no handoff coming that would read it again. So a start read that fails - here
// GitHub's 502, which is no rate limit - hands the record's head to its base's reads, whose read
// answers clean, and the round ends; nothing injects a read.
func TestAStartReadThatFailsHandsTheHeadToItsBase(t *testing.T) {
	t.Parallel()
	rig := newReadRig(t, approvedOnMain, func(read int32) pullAnswer {
		if read == 1 {
			return unavailablePull
		}
		return cleanPull
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	concludedOnCode(t, rig.pool)
	rig.w.readIssuesAtStart(ctx)
	testwait.Eventually(t, "the base's read to end the round after the start read failed", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Accepting
	})
	if failed := loggedLines(t, rig.logs.String(), "read an issue's pull request as the daemon starts; the reads of its base read it again"); len(failed) != 1 {
		t.Fatalf("the start read logged %v failures, want the one 502", failed)
	}
}

// A base's read whose token the implement App cannot mint stops before GitHub answers, and a
// failure that repeats would stop every turn. Each pull request's round is asked again all the
// same (readFailed), so a head whose `mergeable` GitHub answered null on a read the bound has
// outlived is told though no read answers; nothing injects the fact.
func TestABaseReadWhoseMintFailsStillTellsAPersistentNull(t *testing.T) {
	t.Parallel()
	rig := newReadRig(t, approvedOnMain, func(int32) pullAnswer { return cleanPull })
	concludedOnCode(t, rig.pool)
	// The record a null read wrote an hour ago: GitHub has not computed whether head code merges.
	if _, err := rig.pool.Exec(context.Background(), `update pull_requests set merge_state = 'blocked', mergeability = 'UNKNOWN',
		null_mergeable_reads = 1, null_mergeable_since = now() - interval '1 hour', state_read_at = now() - interval '1 hour',
		evidence_read_at = now() - interval '1 hour' where issue = 'CAPTURE-1'`); err != nil {
		t.Fatalf("record the null read: %v", err)
	}
	rig.w.tokens = neverMints{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	rig.w.queueBaseRead(ghrepo.MustParse("acme/widgets"), "main", map[string]string{"CAPTURE-1": "code"}, false)
	testwait.Eventually(t, "the review-stuck notice of the null mergeable", func() bool {
		return len(noticeReasons(t, rig.pool, "review-stuck")) > 0
	})
	if told := noticeReasons(t, rig.pool, "review-stuck"); len(told) != 1 || !strings.Contains(told[0], "GitHub has answered `mergeable` null for it on 1 read(s) in a row") {
		t.Fatalf("review-stuck notices %q, want one naming the null read the bound has outlived", told)
	}
	if got := rig.reads.Load(); got != 0 {
		t.Fatalf("GitHub was read %d times, want none: no token could be minted", got)
	}
}

// neverMints is a token source whose every mint fails, as an implement App that cannot mint does.
type neverMints struct{ appauth.Tokens }

func (neverMints) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{}, mintTimedOut(role, owner)
}

// concludedOnCode records head code's own checks concluded, as a settlement before a restart left
// them, straight into the row: a settlement applied as a fact would ask a read of its own, whose
// climb would read the head and hide the path a test holds.
func concludedOnCode(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update pull_requests set checked_head = 'code',
		check_runs = '[{"name":"pr-checks-result","id":3,"head":"code","conclusion":"succeeded"}]' where issue = 'CAPTURE-1'`); err != nil {
		t.Fatalf("record head code's checks concluded: %v", err)
	}
}

// The verdict, the checks and the count of a REST read GitHub answered `blocked` and MERGEABLE all
// come from the one GraphQL answer the read asks next: a push to the base between the two turns
// the head CONFLICTING and DIRTY, and the record takes that, not the REST answer it replaced.
func TestABlockedRestReadTakesItsVerdictFromTheGraphQLAnswer(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	captureInTesting(t, pool)
	rest := `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`
	api := pullStandInWith(t, rest, func(asked []int) string {
		return batchAnswer(t, asked, batchNode(86, "code", "CONFLICTING", "DIRTY"))
	})
	w := readRuntime(pool, api.URL, quietLogger())
	state, _, _, err := w.readMergeState(context.Background(), "CAPTURE-1")
	if err != nil || state != record.MergeStateDirty {
		t.Fatalf("the read answered %s, %v; want the GraphQL answer's dirty", state, err)
	}
	if pr := recordedPull(t, pool, "CAPTURE-1"); pr.MergeState != record.MergeStateDirty || pr.Mergeability != record.MergeabilityConflicting {
		t.Fatalf("the record holds %s/%s, want the GraphQL answer's dirty/CONFLICTING", pr.MergeState, pr.Mergeability)
	}
}

// A start read that GitHub's rate limit refuses ends the start read, since every read after it on
// the installation meets the same limit, but it hands the limited head to its base's reads all the
// same: the hand-off makes no GitHub call. The base's read answers clean, and the round ends;
// nothing injects a read.
func TestARateLimitedStartReadHandsItsHeadToItsBase(t *testing.T) {
	t.Parallel()
	rig := newReadRig(t, approvedOnMain, func(int32) pullAnswer { return cleanPull })
	rig.refuseREST.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	concludedOnCode(t, rig.pool)
	rig.w.readIssuesAtStart(ctx)
	testwait.Eventually(t, "the base's read to end the round after the start read was rate-limited", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Accepting
	})
	limited := loggedLines(t, rig.logs.String(), "read an issue's pull request as the daemon starts: GitHub's rate limit")
	if len(limited) != 1 || limited[0]["left"] != float64(1) {
		t.Fatalf("the start read logged %v, want one rate limit with the one issue left handed to its base", limited)
	}
}

// A base query the rate limit refuses sends each pull request in it back along its ladder and asks
// its round again once (readStopped): a head whose null `mergeable` run stands gets one
// PullRequestMergeStateUnread for the refused query, not one from the query and one from the stop.
func TestARateLimitedBaseQueryAsksEachRoundOnce(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	if _, err := pool.Exec(context.Background(), `update pull_requests set merge_state = 'blocked', mergeability = 'UNKNOWN',
		null_mergeable_reads = 1, null_mergeable_since = now(), state_read_at = now(), evidence_read_at = now()
		where issue = 'CAPTURE-1'`); err != nil {
		t.Fatalf("record the null read: %v", err)
	}
	rig := newBatchRig(t, pool, func(query int32, asked []int, header http.Header) (int, string) {
		if query == 1 {
			header.Set("Retry-After", "1")
			return http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
		}
		return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"))
	})
	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "86 answered after the limit and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").Mergeability == record.MergeabilityMergeable && rig.ended()
	})
	var asked int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where source = 'github' and event_id like 'merge-state-unread:%'`).Scan(&asked); err != nil {
		t.Fatalf("count the rounds asked again: %v", err)
	}
	if asked != 1 {
		t.Fatalf("the refused query asked the round again %d times, want once", asked)
	}
}

// The GraphQL answer a REST read of a `blocked`, MERGEABLE head takes in its place is stamped with
// the moment it arrived, not the REST read's: the workflow drops a read older than the one it
// holds, so a batch answer that landed between the two calls must not outdate this one.
func TestAWholeAnswerCarriesItsOwnReadTime(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	captureInTesting(t, pool)
	rest := `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`
	api := pullStandInWith(t, rest, func(asked []int) string {
		return batchAnswer(t, asked, batchNode(86, "code", "MERGEABLE", "BLOCKED"))
	})
	w := readRuntime(pool, api.URL, quietLogger())
	var pull githubPullRequest
	if err := json.Unmarshal([]byte(rest), &pull); err != nil {
		t.Fatal(err)
	}
	_, pr := capturePhase(t, pool)
	restAt := time.Now().UTC().Add(-time.Hour)
	whole, at := w.readWholeAnswer(context.Background(), *pr, pull, restAt)
	if whole.batch == nil || !at.After(restAt) {
		t.Fatalf("the whole answer is a batch answer %t read at %v, want the GraphQL answer read after the REST read at %v", whole.batch != nil, at, restAt)
	}
}

// A conflict's second read decides on the answer it applied. A REST read that answers `blocked`
// and MERGEABLE is replaced by GitHub's GraphQL answer, which here is `mergeable` null: that is no
// answer that decides, so the row reads GitHub again rather than finishing on the REST answer it
// replaced.
func TestAConflictSecondReadDecidesOnTheAnswerItApplied(t *testing.T) {
	t.Parallel()
	blockedPull := pullAnswer{body: `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`}
	rig := newReadRig(t, captureInTesting, func(read int32) pullAnswer {
		switch read {
		case 1:
			return blockedPull
		case 2:
			return unknownPull
		}
		return conflictingPull
	})
	first := time.Now().UTC().Truncate(time.Microsecond)
	rig.firstRead(t, first)
	if ran := runSecondReads(t, rig.runner, first.Add(time.Minute)); ran != 1 {
		t.Fatalf("%d second reads ran a minute after the first read, want one", ran)
	}
	if got := rig.reads.Load(); got != 2 {
		t.Fatalf("GitHub was read %d times, want the REST read and the GraphQL read that replaced it", got)
	}
	if _, attempts, found := secondReadRow(t, rig.pool); !found || attempts != 1 {
		t.Fatalf("the second read's row is queued %t after %d attempts, want it queued to read again: the applied answer was unknown", found, attempts)
	}
}
