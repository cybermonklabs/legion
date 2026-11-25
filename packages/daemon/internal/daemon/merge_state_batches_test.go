package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// batchRequest is the GraphQL query a read of a base's batch sends (chunkQuery), as the stand-ins
// read it.
type batchRequest struct {
	Query     string `json:"query"`
	Variables struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	} `json:"variables"`
}

// askedAlias is one pull request a batch query asks for: its alias and its number.
var askedAlias = regexp.MustCompile(`(\w+): pullRequest\(number: (\d+)\)`)

// readBatchRequest decodes r as a read of acme/widgets's pull requests by number, failing the test
// (from the stand-in's own goroutine, so with Errorf) for any other request to the GraphQL
// endpoint, and answers the numbers it asks for, in order.
func readBatchRequest(t *testing.T, r *http.Request) []int {
	t.Helper()
	var request batchRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode the GraphQL request: %v", err)
		return nil
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer outbox-token" ||
		request.Variables.Owner != "acme" || request.Variables.Name != "widgets" || strings.Contains(request.Query, "pullRequests(") {
		t.Errorf("GraphQL was sent %s %+v with %q, want acme/widgets's pull requests by number under the implement App's token",
			r.Method, request.Variables, r.Header.Get("Authorization"))
	}
	var numbers []int
	for _, match := range askedAlias.FindAllStringSubmatch(request.Query, -1) {
		number, _ := strconv.Atoi(match[2])
		if match[1] != pullAlias(number) {
			t.Errorf("the query asks for pull request %d under the alias %s, want %s", number, match[1], pullAlias(number))
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// batchNode is one pull request as the by-number query answers it: open on main at head, with
// GitHub's `mergeable` and `mergeStateStatus` in GraphQL's capitals, no review request open, and
// the head's rollup SUCCESS, as GitHub answers a pull request whose checks are done and that asks
// nobody for a review. A test that needs another count or rollup sets it on the node.
func batchNode(number int, head, mergeable, state string) map[string]any {
	return map[string]any{"number": number, "state": "OPEN", "headRefOid": head, "headRefName": fmt.Sprintf("legion/CAPTURE-%d", number-85),
		"baseRefName": "main", "mergeable": mergeable, "mergeStateStatus": state,
		"reviewRequests": map[string]any{"totalCount": 0}, "commits": rollupOf(head, "SUCCESS")}
}

// rollupOf is the `commits(last: 1)` of a pull request whose head commit is head and whose
// `statusCheckRollup` state is state, or null when state is "", with the counts GitHub gives such
// a rollup: one check run in state, IN_PROGRESS for a PENDING rollup, and no commit status.
func rollupOf(head, state string) map[string]any {
	if state == "" {
		return rollupCounted(head, "", nil, nil)
	}
	run := state
	if state == "PENDING" {
		run = "IN_PROGRESS"
	}
	return rollupCounted(head, state, map[string]int{run: 1}, nil)
}

// rollupCounted is the `commits(last: 1)` of a pull request whose head commit is head, whose
// rollup state is state, null when state is "", and whose check runs and commit statuses GitHub
// counts by state as runs and statuses say, every other state counted 0, as GitHub lists each.
func rollupCounted(head, state string, runs, statuses map[string]int) map[string]any {
	var rollup any
	if state != "" {
		counted := func(states []string, by map[string]int) []any {
			out := make([]any, 0, len(states))
			for _, s := range states {
				out = append(out, map[string]any{"state": s, "count": by[s]})
			}
			return out
		}
		rollup = map[string]any{"state": state, "contexts": map[string]any{
			"checkRunCountsByState": counted([]string{"ACTION_REQUIRED", "CANCELLED", "COMPLETED", "FAILURE", "IN_PROGRESS", "NEUTRAL",
				"PENDING", "QUEUED", "SKIPPED", "STALE", "STARTUP_FAILURE", "SUCCESS", "TIMED_OUT", "WAITING"}, runs),
			"statusContextCountsByState": counted([]string{"EXPECTED", "ERROR", "FAILURE", "PENDING", "SUCCESS"}, statuses)}}
	}
	return map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": head, "statusCheckRollup": rollup}}}}
}

// batchAnswer is the by-number query's answer to asked: each of nodes under its alias, and for
// each number asked that nodes holds none of, null and a NOT_FOUND error naming its alias, as
// GitHub answers a pull request it cannot resolve.
func batchAnswer(t *testing.T, asked []int, nodes ...map[string]any) string {
	t.Helper()
	repository := map[string]any{}
	var errs []map[string]any
	for _, number := range asked {
		alias := pullAlias(number)
		repository[alias] = nil
		for _, node := range nodes {
			if node["number"] == number {
				repository[alias] = node
			}
		}
		if repository[alias] == nil {
			errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{"repository", alias},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)})
		}
	}
	answer := map[string]any{"data": map[string]any{"repository": repository}}
	if len(errs) > 0 {
		answer["errors"] = errs
	}
	body, err := json.Marshal(answer)
	if err != nil {
		t.Errorf("encode the batch answer: %v", err)
	}
	return string(body)
}

// graphQLAnswerOf is rest, an answer to GET /pulls/86, as the by-number query answers the same
// pull request when asked: GitHub's `mergeable` null, true and false are UNKNOWN, MERGEABLE and
// CONFLICTING, its `mergeStateStatus` is the `mergeable_state` in capitals, and its base and head
// are the REST answer's. An answer that is no 2xx is the query's own status and body.
func graphQLAnswerOf(t *testing.T, rest pullAnswer, asked []int) (int, string) {
	t.Helper()
	if rest.status >= http.StatusMultipleChoices {
		return rest.status, rest.body
	}
	var pull githubPullRequest
	if err := json.Unmarshal([]byte(rest.body), &pull); err != nil {
		t.Errorf("decode the REST answer %s: %v", rest.body, err)
		return http.StatusInternalServerError, `{"message":"the stand-in's answer did not decode"}`
	}
	mergeable := "UNKNOWN"
	if pull.Mergeable != nil {
		mergeable = "CONFLICTING"
		if *pull.Mergeable {
			mergeable = "MERGEABLE"
		}
	}
	state := "OPEN"
	if pull.State != "open" {
		state = "CLOSED"
	}
	node := batchNode(pull.Number, pull.Head.SHA, mergeable, strings.ToUpper(pull.MergeableState))
	node["state"], node["headRefName"], node["baseRefName"] = state, pull.Head.Ref, pull.Base.Ref
	if count := pull.reviewsRequested(); count != nil {
		node["reviewRequests"] = map[string]any{"totalCount": *count}
	}
	return http.StatusOK, batchAnswer(t, asked, node)
}

// batchRig is a readRuntime whose GraphQL stand-in answers each query as answer says, given its
// count among the queries, the numbers it asks for and the answer's headers to set, and counts both the queries and the
// REST reads of a pull request, which a batch makes none of; and the log the runtime writes. Its
// waits are cut to readRigWait and its reader runs until the test ends.
type batchRig struct {
	w       *workflowRuntime
	logs    *syncBuffer
	queries atomic.Int32
	rest    atomic.Int32
	mu      sync.Mutex
	asked   [][]int
}

func newBatchRig(t *testing.T, pool *store.Pool, answer func(query int32, asked []int, header http.Header) (int, string)) *batchRig {
	t.Helper()
	rig := &batchRig{logs: &syncBuffer{}}
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			rig.rest.Add(1)
			http.NotFound(w, r)
			return
		}
		asked := readBatchRequest(t, r)
		rig.mu.Lock()
		rig.asked = append(rig.asked, asked)
		rig.mu.Unlock()
		status, body := answer(rig.queries.Add(1), asked, w.Header())
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}})
	rig.w = readRuntime(pool, api, slog.New(slog.NewJSONHandler(rig.logs, nil)))
	rig.w.mergeStateRetry = readRigWait
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	return rig
}

// askedOf is every query's numbers, in the order the queries came.
func (rig *batchRig) askedOf() [][]int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return slices.Clone(rig.asked)
}

// ended is whether no batch ladder runs and none waits to.
func (rig *batchRig) ended() bool {
	running, waiting := ladders(rig.w)
	return running == 0 && waiting == 0
}

// seedOnMain records issue key of CAPTURE in at, with status, and its pull request number open on
// base main at head head-<number>, GitHub's last read finding it MERGEABLE and clean; the issue's
// implementer, and in reviewing a reviewer that has decided nothing.
func seedOnMain(t *testing.T, pool *store.Pool, key string, number int, at phase.Phase, status, rank string) {
	t.Helper()
	seedOnBase(t, pool, key, number, "main", at, status, rank)
}

// seedOnBase is seedOnMain with the pull request open on base.
func seedOnBase(t *testing.T, pool *store.Pool, key string, number int, base string, at phase.Phase, status, rank string) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: key, Project: "CAPTURE", Title: "root", Phase: at, Generation: 1, Status: status, Rank: rank}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets",
			Number: number, Branch: "legion/" + key, Base: base, HeadSHA: fmt.Sprintf("head-%d", number), Failing: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
			return err
		}
		rows := []record.PhaseRow{{Issue: key, Role: claim.RoleImplementer, Claim: claim.Token("implement-" + key)}}
		if at == phase.Reviewing {
			rows = append(rows, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: claim.Token("review-" + key)})
		}
		for _, row := range rows {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

// recordedPull is the pull request issue's record holds.
func recordedPull(t *testing.T, pool *store.Pool, issue string) *record.PullRequest {
	t.Helper()
	_, pr, err := issueRecord(context.Background(), pool, record.NewStore(), issue)
	if err != nil || pr == nil {
		t.Fatalf("read %s's pull request: %v, %v", issue, pr, err)
	}
	return pr
}

// pushOntoMain applies another pull request's merge onto main of acme/widgets, as Envoy relays the
// push (intake's decodePush reads it as an intake.BasePush), through w's engine.
func pushOntoMain(t *testing.T, pool *store.Pool, w *workflowRuntime, after string) {
	t.Helper()
	applyFact(t, pool, "github", "push-onto-main-"+after, intake.BasePush{Repo: "acme/widgets", Branch: "main", After: after}, w.handlers...)
}

// A push to a base owes a read of the merge state of every open pull request on it whose issue
// stands past implementing (classify.MergeStateReadOwed), and the daemon reads them in GraphQL
// queries that ask for those pull requests alone, by number, baseReadChunk to a query (two here):
// no listing of the base's open pull requests, which on a busy repository GitHub cannot answer
// within its 10 s, and no GET /pulls/{number} of each. Each owed pull request's answer is applied
// as that GET's would be - the record holds GitHub's state - and a pull request the push owes no
// read, one in implementing, is never asked for.
func TestABasePushReadsItsOwedPullRequestsByNumberInChunks(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Reviewing, "needs_review", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	seedOnMain(t, pool, "CAPTURE-3", 88, phase.Testing, "testing", "W")
	seedOnMain(t, pool, "CAPTURE-4", 89, phase.Implementing, "in_progress", "X")
	states := map[int]string{86: "BLOCKED", 87: "BEHIND", 88: "UNSTABLE", 89: "DIRTY"}
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		var nodes []map[string]any
		for _, number := range asked {
			nodes = append(nodes, batchNode(number, fmt.Sprintf("head-%d", number), "MERGEABLE", states[number]))
		}
		return http.StatusOK, batchAnswer(t, asked, nodes...)
	})
	rig.w.baseChunk = 2

	pushOntoMain(t, pool, rig.w, "main-2")
	want := map[string]record.MergeState{"CAPTURE-1": record.MergeStateBlocked, "CAPTURE-2": record.MergeStateBehind, "CAPTURE-3": record.MergeStateUnstable}
	testwait.Eventually(t, "the owed pull requests to hold GitHub's answers and the batch to end", func() bool {
		for issue, state := range want {
			if recordedPull(t, pool, issue).MergeState != state {
				return false
			}
		}
		return rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 2 || !slices.Equal(asked[0], []int{86, 87}) || !slices.Equal(asked[1], []int{88}) {
		t.Fatalf("the push asked GitHub for %v, want two queries, [86 87] and [88]: the owed pull requests by number, two to a query", asked)
	}
	if got := rig.rest.Load(); got != 0 {
		t.Fatalf("the push made %d REST reads, want none", got)
	}
	if pr := recordedPull(t, pool, "CAPTURE-4"); pr.MergeState != record.MergeStateClean || pr.Mergeability != record.MergeabilityMergeable || pr.ConflictRead != nil {
		t.Fatalf("the pull request of the issue in implementing holds %s/%s, first read %+v; want it left as it was: the push owes it no read",
			pr.Mergeability, pr.MergeState, pr.ConflictRead)
	}
	read := loggedMessages(t, rig.logs.String(), "github: read the merge state of pull requests on a base by number")
	if len(read) != 2 || read[0]["asked"] != float64(2) || read[1]["asked"] != float64(1) || read[0]["base"] != "main" {
		t.Fatalf("the batch logged %v, want one line for each of its two queries on main", read)
	}
}

// The batch of a base follows GitHub's `unknown` as one pull request's ladder does (dispatch://
// INFRA-4374 R4): it reads again, mergeStateLadderWait apart, up to mergeStateLadderReads reads in
// its climb, asking only for the owed pull requests GitHub has not answered yet, and then each on
// its own ladder, after a wait that doubles, each answer logged with its wait. A pull request
// GitHub answers a state for leaves the batch, and the batch ends once nothing is owed: GitHub is
// read no more after that.
func TestAnUnknownAnswerReadsTheBaseBatchAgainOnTheLadder(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		first := batchNode(86, "head-86", "UNKNOWN", "UNKNOWN")
		if query > mergeStateLadderReads {
			first = batchNode(86, "head-86", "MERGEABLE", "BLOCKED")
		}
		second := batchNode(87, "head-87", "MERGEABLE", "CLEAN")
		if query == 1 {
			second = batchNode(87, "head-87", "UNKNOWN", "UNKNOWN")
		}
		return http.StatusOK, batchAnswer(t, asked, first, second)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the read after the climb to answer 86 and the batch to end", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && rig.ended()
	})
	asked := rig.askedOf()
	want := [][]int{{86, 87}, {86, 87}, {86}, {86}, {86}, {86}}
	if len(asked) != len(want) {
		t.Fatalf("GitHub was asked for %v, want %v: the climb's five reads, each of what is still owed, and one read after it", asked, want)
	}
	for i := range want {
		if !slices.Equal(asked[i], want[i]) {
			t.Fatalf("GitHub was asked for %v, want %v", asked, want)
		}
	}
	if got := rig.rest.Load(); got != 0 {
		t.Fatalf("GitHub was read %d times over REST, want none", got)
	}
	if pr := recordedPull(t, pool, "CAPTURE-2"); pr.MergeState != record.MergeStateClean || pr.Mergeability != record.MergeabilityMergeable {
		t.Fatalf("87 holds %s/%s, want GitHub's MERGEABLE/clean from the second read", pr.Mergeability, pr.MergeState)
	}
	follows := loggedLines(t, rig.logs.String(), followReadLine)
	if len(follows) != 1 || follows[0]["issue"] != "CAPTURE-1" || follows[0]["attempt"] != float64(1) ||
		follows[0]["wait"] != float64(2*readRigWait) || follows[0]["answered"] != string(record.MergeStateBlocked) || follows[0]["head"] != "head-86" {
		t.Fatalf("the reads after the climb logged %v, want one for CAPTURE-1, after twice the ladder's wait, answering blocked", follows)
	}
	mark := rig.queries.Load()
	time.Sleep(2 * mergeStateFollowWaits * readRigWait)
	if got := rig.queries.Load(); got != mark {
		t.Fatalf("GitHub was queried %d times after the batch ended, want none", got-mark)
	}
}

// Each chunk's answers are applied as the chunk is answered, stamped with that moment: a later
// chunk can take long, and meanwhile a newer read of a pull request of an earlier chunk can reach
// the record - a climb of its own, or GitHub's own event. The workflow drops a read older than the
// one the record holds, so the earlier chunk's answer changes nothing of the newer one: here it
// would have released a CONFLICTING first read and with it the second read INFRA-4374 R2 requires.
func TestABatchAnswerIsStampedWithItsOwnChunksTime(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	// second is the stand-in's word that the batch asked for its second chunk, which it does only
	// once it has applied the first.
	second := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	releaseSecond := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseSecond)
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		if slices.Contains(asked, 86) {
			return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "CLEAN"))
		}
		nudge(second)
		<-release
		return http.StatusOK, batchAnswer(t, asked, batchNode(87, "head-87", "MERGEABLE", "BLOCKED"))
	})
	rig.w.baseChunk = 1

	pushOntoMain(t, pool, rig.w, "main-2")
	select {
	case <-second:
	case <-time.After(30 * time.Second):
		t.Fatalf("the batch asked for no second chunk")
	}
	// While the second chunk is held, a read of 86 made after the first chunk was answered finds
	// it CONFLICTING, and the record takes it as its first conflicting read.
	applyFact(t, pool, "github", "newer-read-of-86", intake.PullRequestMergeability{Repo: "acme/widgets", Number: 86, Base: "main",
		HeadSHA: "head-86", Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, ReadAt: time.Now().UTC(),
		Rules: &record.BaseRules{Required: []string{"lint"}}}, rig.w.handlers...)
	releaseSecond()
	testwait.Eventually(t, "the second chunk's answer to be recorded and the batch to end", func() bool {
		return recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && rig.ended()
	})
	pr := recordedPull(t, pool, "CAPTURE-1")
	if pr.Mergeability != record.MergeabilityConflicting || pr.MergeState != record.MergeStateDirty || pr.ConflictRead == nil || pr.ConflictRead.Head != "head-86" {
		t.Fatalf("86 holds %s/%s with first read %+v, want the newer CONFLICTING read standing: the first chunk's answer is older than it",
			pr.Mergeability, pr.MergeState, pr.ConflictRead)
	}
}

// A chunk that fails costs the chunks answered before it nothing: their answers were applied when
// they came, so the pull requests GitHub answered a state for have left the batch and are not
// asked for again, and only the failed chunk's pull requests are read again.
func TestAFailedChunkKeepsTheChunksReadBeforeIt(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		if query == 2 {
			return http.StatusBadGateway, `{"message":"Server Error"}`
		}
		return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"), batchNode(87, "head-87", "MERGEABLE", "CLEAN"))
	})
	rig.w.baseChunk = 1

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "both pull requests answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateClean && rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 3 || !slices.Equal(asked[0], []int{86}) || !slices.Equal(asked[1], []int{87}) || !slices.Equal(asked[2], []int{87}) {
		t.Fatalf("GitHub was asked for %v, want [86], the failed [87] and [87] again: 86's answer was kept", asked)
	}
}

// A query of several pull requests that runs out of GitHub's time - GitHub answers it 502, saying
// it could not respond in time - is asked again at once one pull request at a time, so a slow pull
// request holds up no other and each is answered by the same read.
func TestAChunkGitHubCannotAnswerWholeIsAskedAgainOnePullRequestAtATime(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		if len(asked) > 1 {
			return http.StatusBadGateway, timedOutAnswer
		}
		return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"), batchNode(87, "head-87", "MERGEABLE", "CLEAN"))
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "both pull requests answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateClean && rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 3 || !slices.Equal(asked[0], []int{86, 87}) || !slices.Equal(asked[1], []int{86}) || !slices.Equal(asked[2], []int{87}) {
		t.Fatalf("GitHub was asked for %v, want [86 87] and, after its 502, [86] and [87] in the same read", asked)
	}
}

// A batch read that fails - here GitHub answers a chunk with GraphQL errors and no data - leaves
// every owed pull request owed, a push that asked for them again while that read ran among them,
// and the ladder reads them again at its next step, as it reads a head GitHub answered `unknown`
// for.
func TestAFailedBatchReadKeepsItsRequestsAndReadsThemAgain(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	held := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	releaseFailure := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseFailure)
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		switch query {
		case 1:
			return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "UNKNOWN", "UNKNOWN"))
		case 2:
			nudge(held)
			<-release
			return http.StatusOK, `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong while executing your query."}]}`
		}
		return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"), batchNode(87, "head-87", "MERGEABLE", "CLEAN"))
	})
	rig.w.baseChunk = 1

	pushOntoMain(t, pool, rig.w, "main-2")
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatalf("the batch read no second chunk")
	}
	// Another push onto main asks for both reads again while the failing read runs.
	pushOntoMain(t, pool, rig.w, "main-3")
	releaseFailure()
	testwait.Eventually(t, "the reads after the failure to answer both and the batch to end", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateClean && rig.ended()
	})
	if failed := loggedMessages(t, rig.logs.String(), "github: read the merge state of the open pull requests on a base whose reads were asked for"); len(failed) != 1 {
		t.Fatalf("the batch logged %v, want the one failed read", failed)
	}
}

// GitHub answers each pull request a query asks for by number on its own: one it cannot answer is
// null in the data, with an error naming its alias, and the others are answered beside it. Those
// are applied, and only the one GitHub left out is read again.
func TestAPullRequestGitHubLeavesOutOfAnAnswerKeepsTheOthersAnswered(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		nodes := []map[string]any{batchNode(86, "head-86", "MERGEABLE", "BLOCKED")}
		if query > 1 {
			nodes = append(nodes, batchNode(87, "head-87", "MERGEABLE", "CLEAN"))
		}
		return http.StatusOK, batchAnswer(t, asked, nodes...)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "both pull requests answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateClean && rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 2 || !slices.Equal(asked[0], []int{86, 87}) || !slices.Equal(asked[1], []int{87}) {
		t.Fatalf("GitHub was asked for %v, want [86 87] and then 87 alone: 86's answer beside 87's error was kept", asked)
	}
	left := loggedMessages(t, rig.logs.String(), "github: GitHub answered nothing for a pull request owed a merge-state read")
	if len(left) != 1 || left[0]["pullRequest"] != float64(87) || !strings.Contains(fmt.Sprint(left[0]["error"]), "Could not resolve") {
		t.Fatalf("the batch logged %v, want one line for 87 naming GitHub's error", left)
	}
}

// A read GitHub's rate limit refuses is read again no sooner than GitHub asks: a secondary limit's
// Retry-After here, one second, where the ladder would read again a hundredth of a second on.
func TestARateLimitedBatchReadWaitsWhatGitHubAsks(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	var mu sync.Mutex
	var at []time.Time
	rig := newBatchRig(t, pool, func(query int32, asked []int, header http.Header) (int, string) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		if query == 1 {
			header.Set("Retry-After", "1")
			return http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
		}
		return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "MERGEABLE", "BLOCKED"))
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "86 answered after the limit and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && rig.ended()
	})
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 2 || at[1].Sub(at[0]) < time.Second {
		t.Fatalf("GitHub was queried at %v, want twice, the second at least the second GitHub's Retry-After asked after the first", at)
	}
}

// A pull request owed one read (baseOwed.once, a base push's for an issue in merging, or a failed
// start read's) leaves the batch after a read of it, whatever GitHub answered, but a query that
// read nothing of it - one that fails, a 502 here, or one GitHub's rate limit refuses - made no read
// of it, so it keeps its one read until a query answers (missed, readStopped): the next query
// answers `unknown`, which ends it, and no query after that is made. The negative control is
// missed dropping a one-read place on every read that answered nothing, as it did before: the
// record then stands at the state it held, GitHub is queried once, and no read of the head was
// made.
func TestAOneReadWhoseQueryReadNothingIsMadeByTheNextQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		first func(header http.Header) (int, string)
	}{
		{name: "a failed query", first: func(http.Header) (int, string) { return http.StatusBadGateway, `{"message":"Server Error"}` }},
		{name: "a rate limit", first: func(header http.Header) (int, string) {
			header.Set("Retry-After", "1")
			return http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedOnMain(t, pool, "CAPTURE-1", 86, phase.Merging, "retro", "U")
			rig := newBatchRig(t, pool, func(query int32, asked []int, header http.Header) (int, string) {
				if query == 1 {
					return tc.first(header)
				}
				return http.StatusOK, batchAnswer(t, asked, batchNode(86, "head-86", "UNKNOWN", "UNKNOWN"))
			})

			pushOntoMain(t, pool, rig.w, "main-2")
			testwait.Eventually(t, "the second query to answer the one read and the batch to end", func() bool {
				return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateUnknown && rig.ended()
			})
			time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
			if got := rig.queries.Load(); got != 2 {
				t.Fatalf("GitHub was queried %d times, want twice: the first read nothing of the pull request, the second was its one read", got)
			}
		})
	}
}

// A pull request can move to another base with its head unchanged, which the record learns of from
// no event it reads. The query asks GitHub for each pull request's base by number, so the answer
// names the base GitHub holds it on: the record takes it, and GitHub's `unknown` there is followed
// by the batch of that base, with no REST read, until GitHub answers a state.
func TestARetargetedPullRequestIsFollowedOnTheBaseGitHubNames(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		node := batchNode(86, "head-86", "UNKNOWN", "UNKNOWN")
		if query > 1 {
			node = batchNode(86, "head-86", "MERGEABLE", "BLOCKED")
		}
		node["baseRefName"] = "release"
		return http.StatusOK, batchAnswer(t, asked, node)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the pull request to be read on its new base and its reads to end", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && rig.ended()
	})
	if pr := recordedPull(t, pool, "CAPTURE-1"); pr.Base != "release" || pr.Mergeability != record.MergeabilityMergeable {
		t.Fatalf("86 is recorded on %q as %s, want GitHub's base release and its MERGEABLE", pr.Base, pr.Mergeability)
	}
	if got, queries := rig.rest.Load(), rig.queries.Load(); got != 0 || queries != 2 {
		t.Fatalf("86 was read %d times over REST in %d queries, want no REST read and two queries", got, queries)
	}
	moved := loggedMessages(t, rig.logs.String(), "github: GitHub holds a pull request owed a merge-state read on another base")
	onRelease := slices.DeleteFunc(loggedMessages(t, rig.logs.String(), "github: read the merge state of pull requests on a base by number"),
		func(line map[string]any) bool { return line["base"] != "release" })
	if len(moved) != 1 || moved[0]["now"] != "release" || len(onRelease) != 1 {
		t.Fatalf("the batch logged moves %v and reads on release %v, want one move to release and one read there, which answered blocked", moved, onRelease)
	}
}

// A pull request whose head on GitHub differs from the record's - a push the record has not heard
// of - is answered for GitHub's head, which the workflow does not record, so the record keeps the
// old base too. Its reads go by GitHub's base alone, so it moves to that base once and stays: the
// record's base never sends it back.
func TestARetargetedPullRequestDoesNotBounceBetweenBases(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		node := batchNode(86, "head-elsewhere", "UNKNOWN", "UNKNOWN")
		node["baseRefName"] = "release"
		return http.StatusOK, batchAnswer(t, asked, node)
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "three reads after the climb", func() bool {
		return len(loggedLines(t, rig.logs.String(), followReadLine)) >= 3
	})
	if pr := recordedPull(t, pool, "CAPTURE-1"); pr.Base != "main" || pr.HeadSHA != "head-86" {
		t.Fatalf("86 is recorded on %q at %q, want the record left on main at head-86: GitHub answered another head", pr.Base, pr.HeadSHA)
	}
	moved := loggedMessages(t, rig.logs.String(), "github: GitHub holds a pull request owed a merge-state read on another base")
	if len(moved) != 1 {
		t.Fatalf("the batch logged %d moves %v, want one move to release and none back", len(moved), moved)
	}
	rig.w.mergeStateMu.Lock()
	held := rig.w.baseReadOf["CAPTURE-1"]
	rig.w.mergeStateMu.Unlock()
	if want := baseReadKey(ghrepo.MustParse("acme/widgets"), "release"); held != want {
		t.Fatalf("CAPTURE-1 is followed by %q, want %q", held, want)
	}
}

// A head handed over to a batch that already follows another pull request starts its own ladder:
// its first read comes two of the ladder's waits after the handover, and each next one twice as
// long after, whatever the other's ladder stands at, and the other's ladder goes on as it was. The
// two are read apart while their reads fall due apart.
func TestAHeadHandedOverIsReadOnItsOwnLadder(t *testing.T) {
	t.Parallel()
	const unit = 25 * time.Millisecond
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	var reads87 atomic.Int32
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		nodes := []map[string]any{batchNode(86, "head-86", "UNKNOWN", "UNKNOWN")}
		if slices.Contains(asked, 87) {
			state := "UNKNOWN"
			if reads87.Add(1) > 1 {
				state = "BLOCKED"
			}
			nodes = append(nodes, batchNode(87, "head-87", "MERGEABLE", state))
			if state == "UNKNOWN" {
				nodes[1]["mergeable"] = "UNKNOWN"
			}
		}
		return http.StatusOK, batchAnswer(t, asked, nodes...)
	})
	rig.w.mergeStateRetry = unit
	repo := ghrepo.MustParse("acme/widgets")
	followsOf := func(issue string) []map[string]any {
		return slices.DeleteFunc(loggedLines(t, rig.logs.String(), followReadLine), func(line map[string]any) bool { return line["issue"] != issue })
	}

	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-1": "head-86"}, false)
	testwait.Eventually(t, "86's third read after its handover", func() bool { return len(followsOf("CAPTURE-1")) >= 3 })
	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-2": "head-87"}, false)
	testwait.Eventually(t, "87's two reads and 86's fourth", func() bool {
		return len(followsOf("CAPTURE-2")) >= 2 && len(followsOf("CAPTURE-1")) >= 4
	})
	first, second := followsOf("CAPTURE-2")[0], followsOf("CAPTURE-2")[1]
	if first["attempt"] != float64(1) || first["wait"] != float64(2*unit) || second["attempt"] != float64(2) || second["wait"] != float64(4*unit) ||
		second["answered"] != string(record.MergeStateBlocked) {
		t.Fatalf("87's reads logged %v and %v, want its own ladder: attempt 1 after two of the ladder's waits, then 2 after four, answering blocked", first, second)
	}
	for i, line := range followsOf("CAPTURE-1")[:4] {
		if want := float64(unit) * float64(int(2)<<i); line["attempt"] != float64(i+1) || line["wait"] != want {
			t.Fatalf("86's read %d logged %v, want attempt %d after %s: 87's handover leaves its ladder as it was", i+1, line, i+1, time.Duration(want))
		}
	}
	for _, asked := range rig.askedOf() {
		if slices.Contains(asked, 87) && !slices.Equal(asked, []int{87}) {
			t.Fatalf("GitHub was asked for %v, want 87 read on its own while 86's next read is not due", rig.askedOf())
		}
	}
}

// A batch's wake only has its follow's wait look again whether a climb was asked, nothing is owed,
// or a read falls due sooner: a wake whose request a ladder already took up leaves the wait
// waiting for the read it waited for, a climb asked ends it at once, and the ladder goes on while
// anything is owed, with no request standing, ending only once nothing is.
func TestAWakeAloneEndsNoFollowWhileAPullRequestIsOwed(t *testing.T) {
	t.Parallel()
	w := readRuntime(nil, "", quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	repo := ghrepo.MustParse("acme/widgets")
	key := baseReadKey(repo, "main")
	const wait = 100 * time.Millisecond
	due := func() {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		if w.baseReads == nil {
			w.baseReads = map[string]*baseRead{key: {repo: repo, base: "main", wake: make(chan struct{}, 1), owed: map[string]baseOwed{}}}
			w.baseReadOf = map[string]string{}
		}
		w.baseAsks++
		w.baseReads[key].owed["CAPTURE-1"] = baseOwed{head: "head-86", asked: w.baseAsks, wait: wait, due: time.Now().UTC().Add(wait)}
		w.baseReadOf["CAPTURE-1"] = key
	}
	waited := func() (bool, time.Duration) {
		t.Helper()
		start := time.Now()
		read := w.baseFollowWait(ctx, key)
		return read, time.Since(start)
	}

	due()
	nudge(w.baseReads[key].wake)
	if read, took := waited(); !read || took < wait*9/10 {
		t.Fatalf("with a wake whose request was taken up, the follow's wait answered read %t after %s, want a read when the pull request falls due, %s on", read, took, wait)
	}
	if climb, goes := w.baseLadderNext(ctx, key); !goes || climb {
		t.Fatalf("with a pull request owed and no request standing the ladder goes on %t, climbing %t; want it to follow on", goes, climb)
	}
	due()
	go func() {
		time.Sleep(wait / 4)
		w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-3": ""}, true)
	}()
	if read, took := waited(); read || took >= wait {
		t.Fatalf("with a climb asked during it, the follow's wait answered read %t after %s, want it ended before its %s", read, took, wait)
	}
	if climb, goes := w.baseLadderNext(ctx, key); !goes || !climb {
		t.Fatalf("with a climb asked the ladder goes on %t, climbing %t; want it to climb", goes, climb)
	}
	w.mergeStateMu.Lock()
	for _, issue := range []string{"CAPTURE-1", "CAPTURE-3"} {
		w.leaveBaseLocked(issue)
	}
	w.mergeStateMu.Unlock()
	if _, goes := w.baseLadderNext(ctx, key); goes {
		t.Fatalf("with nothing owed the ladder goes on, want it ended")
	}
	if running, _ := ladders(w); running != 0 {
		t.Fatalf("%d ladders are marked running after the batch ended, want none", running)
	}
}

// A move of a pull request to the batch of the base it is on now (moveBase) is decided on a read
// of its old base's batch, and a newer head can reach the new base's batch in the meantime: a
// climb of the pull request's own, which took it and handed the head it still read `unknown` over
// (followOnBase). The move checks that the old batch still holds the pull request under the read's
// request, and moves it, in one step, so the newer head stands whichever lands first: a move after
// the newer handover does nothing, and a newer handover after the move replaces the moved head.
// Many moves and handovers raced on two goroutines hold that in every interleaving the scheduler
// gives them, the one where the handover lands between the old batch's check and the move among
// them, which lost the newer head before the two were made one step.
func TestAMoveDecidedOnAnOlderReadKeepsTheNewerHeadHandedOver(t *testing.T) {
	t.Parallel()
	w := readRuntime(nil, "", quietLogger())
	repo := ghrepo.MustParse("acme/widgets")
	from, to := baseReadKey(repo, "main"), baseReadKey(repo, "release")
	// owedOnMain puts CAPTURE-1 in main's batch alone, following old, and answers its place there
	// as a read of that batch takes it up.
	owedOnMain := func() baseOwed {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		w.baseAsks++
		place := baseOwed{head: "old", asked: w.baseAsks}
		w.baseReads = map[string]*baseRead{from: {repo: repo, base: "main", wake: make(chan struct{}, 1),
			owed: map[string]baseOwed{"CAPTURE-1": place}}}
		w.baseReadOf = map[string]string{"CAPTURE-1": from}
		return place
	}
	handOver := func() { w.queueBaseRead(repo, "release", map[string]string{"CAPTURE-1": "newer"}, false) }
	followed := func() (string, string) {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		held := w.baseReadOf["CAPTURE-1"]
		if read := w.baseReads[held]; read != nil {
			return held, read.owed["CAPTURE-1"].head
		}
		return held, ""
	}

	place := owedOnMain()
	handOver()
	w.moveBase(from, "CAPTURE-1", place, repo, "release")
	if held, head := followed(); held != to || head != "newer" {
		t.Fatalf("after the newer handover and then the older move, CAPTURE-1 is followed by %q at %q; want release's batch at newer", held, head)
	}

	place = owedOnMain()
	w.moveBase(from, "CAPTURE-1", place, repo, "release")
	handOver()
	if held, head := followed(); held != to || head != "newer" {
		t.Fatalf("after the older move and then the newer handover, CAPTURE-1 is followed by %q at %q; want release's batch at newer", held, head)
	}

	for round := range 2000 {
		place := owedOnMain()
		start := make(chan struct{})
		var both sync.WaitGroup
		both.Add(2)
		go func() {
			defer both.Done()
			<-start
			runtime.Gosched()
			w.moveBase(from, "CAPTURE-1", place, repo, "release")
		}()
		go func() {
			defer both.Done()
			<-start
			handOver()
		}()
		close(start)
		both.Wait()
		if held, head := followed(); held != to || head != "newer" {
			t.Fatalf("round %d: with the older move and the newer handover raced, CAPTURE-1 is followed by %q at %q; want release's batch at newer", round, held, head)
		}
	}
}

// timedOutAnswer is GitHub's answer to a query that ran out of its time, which it sends with 502.
const timedOutAnswer = `{"message":"We couldn't respond to your request in time. Sorry about that. Please try resubmitting your request and contact us if the problem persists."}`

// queryAt is one query a stand-in served: the pull requests it asked for, and when the stand-in
// began and ended its answer.
type queryAt struct {
	asked      []int
	start, end time.Time
}

// queryLog keeps the queries a stand-in served, safely for its goroutines.
type queryLog struct {
	mu      sync.Mutex
	queries []queryAt
}

func (l *queryLog) add(q queryAt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, q)
}

func (l *queryLog) all() []queryAt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.queries)
}

// eachAsked is the batch answer that answers each pull request asked as node says.
func eachAsked(t *testing.T, asked []int, node func(number int) map[string]any) string {
	t.Helper()
	nodes := make([]map[string]any, 0, len(asked))
	for _, number := range asked {
		nodes = append(nodes, node(number))
	}
	return batchAnswer(t, asked, nodes...)
}

// A query GitHub fails at once with a 5xx did not run out of GitHub's time, so asking for its pull
// requests one at a time would only multiply the failed requests: it is a read of the chunk that
// answered nothing, and the chunk's pull requests are read again together on their climb.
func TestAFastServerErrorIsAFailedReadOfItsChunk(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		if query == 1 {
			return http.StatusBadGateway, `{"message":"Server Error"}`
		}
		return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any { return batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", "BLOCKED") })
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "both pull requests answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 2 || !slices.Equal(asked[0], []int{86, 87}) || !slices.Equal(asked[1], []int{86, 87}) {
		t.Fatalf("GitHub was asked for %v, want [86 87] twice: a fast 502 is a failed read of the chunk, not a reason to ask for each alone", asked)
	}
}

// A token the implement App could not mint for a query stops the read where it stands, as a rate
// limit does (readStopped): the read sends no query after it, and every pull request it owed goes
// on along its climb, with no query of its own for any of them. The failure is logged as the
// mint's, once, and the climb's next read asks for each again. Read as any other failed query, the
// failed mint would have sent the next chunk's query in the same read, and asked for the first
// chunk's pull request alone from then on.
func TestAFailedMintStopsTheRead(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any { return batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", "BLOCKED") })
	})
	rig.w.baseChunk = 1
	armed := &armedFailure{}
	armed.on.Store(true)
	rig.w.tokens = failingTokens{Tokens: rig.w.tokens, armed: armed}

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "both pull requests answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && rig.ended()
	})
	if asked := rig.askedOf(); len(asked) != 2 || !slices.Equal(asked[0], []int{86}) || !slices.Equal(asked[1], []int{87}) {
		t.Fatalf("GitHub was asked for %v, want [86] and then [87], both by the climb's next read: the read whose first mint failed sends no query", asked)
	}
	if minted := loggedLines(t, rig.logs.String(), "github: mint the implement App token to read the merge state"); len(minted) != 1 {
		t.Fatalf("the daemon logged %v, want the failed mint once", minted)
	}
}

// A pull request whose query of its own ran out of GitHub's time, after its chunk's did, is asked
// for alone on the reads after, until GitHub answers it, so it costs the pull requests beside it no
// second timeout; the others are asked for together again.
func TestAPullRequestThatFailedAloneIsAskedForAloneUntilGitHubAnswersIt(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	seedOnMain(t, pool, "CAPTURE-3", 88, phase.Testing, "testing", "W")
	var alone87 atomic.Int32
	rig := newBatchRig(t, pool, func(query int32, asked []int, _ http.Header) (int, string) {
		switch {
		case len(asked) > 1 && slices.Contains(asked, 87):
			return http.StatusBadGateway, timedOutAnswer
		case slices.Equal(asked, []int{87}) && alone87.Add(1) == 1:
			return http.StatusBadGateway, timedOutAnswer
		}
		state := "BLOCKED"
		if query <= 4 {
			state = "UNKNOWN"
		}
		return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any {
			node := batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", state)
			if state == "UNKNOWN" {
				node["mergeable"] = "UNKNOWN"
			}
			return node
		})
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the three pull requests answered and the batch ended", func() bool {
		for _, issue := range []string{"CAPTURE-1", "CAPTURE-2", "CAPTURE-3"} {
			if recordedPull(t, pool, issue).MergeState != record.MergeStateBlocked {
				return false
			}
		}
		return rig.ended()
	})
	want := [][]int{{86, 87, 88}, {86}, {87}, {88}, {86, 88}, {87}}
	asked := rig.askedOf()
	if len(asked) != len(want) {
		t.Fatalf("GitHub was asked for %v, want %v", asked, want)
	}
	for i := range want {
		if !slices.Equal(asked[i], want[i]) {
			t.Fatalf("GitHub was asked for %v, want %v: after its own query timed out, 87 is asked for alone and the others together", asked, want)
		}
	}
}

// GitHub may answer a query 200 for its secondary limit, with errors of no type or another than
// GraphQL's own: its Retry-After, or the secondary limit's message, make it a limit all the same.
// The installation's queries are held before anything else is asked - no query of one pull
// request alone, no later chunk - and the chunk is read again whole once the hold has passed; a
// message alone holds them a minute.
func TestA200SecondaryLimitHoldsTheInstallationBeforeAnySplit(t *testing.T) {
	t.Parallel()
	for _, limit := range []struct {
		name string
		body string
	}{
		{name: "no type", body: `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`},
		{name: "another type", body: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Try again later."}]}`},
	} {
		t.Run(limit.name+" with a Retry-After", func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
			seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
			queries := &queryLog{}
			rig := newBatchRig(t, pool, func(query int32, asked []int, header http.Header) (int, string) {
				queries.add(queryAt{asked: asked, start: time.Now()})
				if query == 1 {
					header.Set("Retry-After", "1")
					return http.StatusOK, limit.body
				}
				return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any { return batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", "BLOCKED") })
			})
			pushOntoMain(t, pool, rig.w, "main-2")
			testwait.Eventually(t, "both answered after the hold and the batch ended", func() bool {
				return recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && rig.ended()
			})
			served := queries.all()
			if len(served) != 2 || !slices.Equal(served[1].asked, []int{86, 87}) || served[1].start.Sub(served[0].start) < time.Second {
				t.Fatalf("GitHub was queried %v, want twice for [86 87], the second at least the Retry-After's second after the first", served)
			}
		})
	}

	t.Run("the secondary limit's message alone", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
		seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
		rig := newBatchRig(t, pool, func(int32, []int, http.Header) (int, string) {
			return http.StatusOK, `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit."}]}`
		})
		pushOntoMain(t, pool, rig.w, "main-2")
		testwait.Eventually(t, "the first query", func() bool { return rig.queries.Load() >= 1 })
		gate := rig.w.queryGate("implement write acme")
		testwait.Eventually(t, "the installation's hold", func() bool {
			gate.mu.Lock()
			defer gate.mu.Unlock()
			return time.Until(gate.hold) > 55*time.Second
		})
		time.Sleep(300 * time.Millisecond)
		if got := rig.queries.Load(); got != 1 {
			t.Fatalf("GitHub was queried %d times, want once: the message alone holds the installation a minute", got)
		}
	})
}

// GitHub limits the installation, not the base: a limit one base's query meets holds the queries
// of every base of the installation's repositories, so the other base asks nothing until it ends.
func TestTwoBasesOfOneInstallationShareItsHold(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnBase(t, pool, "CAPTURE-1", 86, "main", phase.Testing, "testing", "U")
	seedOnBase(t, pool, "CAPTURE-2", 87, "release", phase.Testing, "testing", "V")
	queries := &queryLog{}
	rig := newBatchRig(t, pool, func(query int32, asked []int, header http.Header) (int, string) {
		queries.add(queryAt{asked: asked, start: time.Now()})
		if query == 1 {
			header.Set("Retry-After", "1")
			return http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
		}
		return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any {
			node := batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", "BLOCKED")
			if n == 87 {
				node["baseRefName"] = "release"
			}
			return node
		})
	})

	pushOntoMain(t, pool, rig.w, "main-2")
	applyFact(t, pool, "github", "push-onto-release", intake.BasePush{Repo: "acme/widgets", Branch: "release", After: "release-2"}, rig.w.handlers...)
	testwait.Eventually(t, "both bases answered and their batches ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked &&
			recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && rig.ended()
	})
	served := queries.all()
	for _, later := range served[1:] {
		if later.start.Sub(served[0].start) < time.Second {
			t.Fatalf("GitHub was queried %v, want every query after the limited one at least the Retry-After's second after it, whatever its base", served)
		}
	}
}

// The installation's queries are paced: after each, the next waits as long as it took, so the
// batches spend at most half of any stretch in GitHub's GraphQL work, inside GitHub's limit on a
// query's processing time, however many pull requests a climb reads.
func TestTheInstallationsQueriesArePaced(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	seedOnMain(t, pool, "CAPTURE-3", 88, phase.Testing, "testing", "W")
	queries := &queryLog{}
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		start := time.Now()
		time.Sleep(40 * time.Millisecond)
		body := eachAsked(t, asked, func(n int) map[string]any { return batchNode(n, fmt.Sprintf("head-%d", n), "MERGEABLE", "BLOCKED") })
		queries.add(queryAt{asked: asked, start: start, end: time.Now()})
		return http.StatusOK, body
	})
	rig.w.baseChunk = 1

	pushOntoMain(t, pool, rig.w, "main-2")
	testwait.Eventually(t, "the three answered and the batch ended", func() bool {
		return recordedPull(t, pool, "CAPTURE-3").MergeState == record.MergeStateBlocked && rig.ended()
	})
	served := queries.all()
	if len(served) != 3 {
		t.Fatalf("GitHub was queried %v, want three queries of one pull request each", served)
	}
	for i := 1; i < len(served); i++ {
		if pause, took := served[i].start.Sub(served[i-1].end), served[i-1].end.Sub(served[i-1].start); pause < took {
			t.Fatalf("query %d began %s after query %d ended, which took %s: want each query to wait as long as the one before it took", i+1, pause, i, took)
		}
	}
}

// A pull request on its own ladder is read once it is due and never before, whatever other pull
// request of its base falls due near it: here two handed over a ladder's wait apart, each read
// first two of the ladder's waits after its handover and then each read at least its logged wait
// after the read before.
func TestAFollowerIsReadNoSoonerThanItIsDue(t *testing.T) {
	t.Parallel()
	const unit = 50 * time.Millisecond
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	queries := &queryLog{}
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		start := time.Now()
		body := eachAsked(t, asked, func(n int) map[string]any { return batchNode(n, fmt.Sprintf("head-%d", n), "UNKNOWN", "UNKNOWN") })
		queries.add(queryAt{asked: asked, start: start, end: time.Now()})
		return http.StatusOK, body
	})
	rig.w.mergeStateRetry = unit
	repo := ghrepo.MustParse("acme/widgets")
	handed := map[int]time.Time{}
	handed[86] = time.Now()
	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-1": "head-86"}, false)
	time.Sleep(unit)
	handed[87] = time.Now()
	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-2": "head-87"}, false)
	followsOf := func(issue string) []map[string]any {
		return slices.DeleteFunc(loggedLines(t, rig.logs.String(), followReadLine), func(line map[string]any) bool { return line["issue"] != issue })
	}
	testwait.Eventually(t, "three reads of each", func() bool { return len(followsOf("CAPTURE-1")) >= 3 && len(followsOf("CAPTURE-2")) >= 3 })

	for number, issue := range map[int]string{86: "CAPTURE-1", 87: "CAPTURE-2"} {
		var reads []queryAt
		for _, q := range queries.all() {
			if slices.Contains(q.asked, number) {
				reads = append(reads, q)
			}
		}
		logs := followsOf(issue)
		for k := range 3 {
			wait := time.Duration(logs[k]["wait"].(float64))
			since := handed[number]
			if k > 0 {
				since = reads[k-1].end
			}
			if got := reads[k].start.Sub(since); got < wait {
				t.Fatalf("%s's read %d came %s after the one before it (or its handover), where its log names a wait of %s: a read comes no sooner than it is due", issue, k+1, got, wait)
			}
		}
	}
}

// A climb reads the pull requests in it and those already due, and leaves the ladders of the
// others as they are: a push that asks for one pull request's read neither reads early, nor starts
// over, the ladder of another its base already follows.
func TestAClimbLeavesTheOtherFollowersOnTheirLadders(t *testing.T) {
	t.Parallel()
	const unit = 50 * time.Millisecond
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	rig := newBatchRig(t, pool, func(_ int32, asked []int, _ http.Header) (int, string) {
		return http.StatusOK, eachAsked(t, asked, func(n int) map[string]any {
			if n == 87 {
				return batchNode(n, "head-87", "MERGEABLE", "BLOCKED")
			}
			return batchNode(n, "head-86", "UNKNOWN", "UNKNOWN")
		})
	})
	rig.w.mergeStateRetry = unit
	repo := ghrepo.MustParse("acme/widgets")
	followsOf := func(issue string) []map[string]any {
		return slices.DeleteFunc(loggedLines(t, rig.logs.String(), followReadLine), func(line map[string]any) bool { return line["issue"] != issue })
	}

	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-1": "head-86"}, false)
	testwait.Eventually(t, "86's second read on its ladder", func() bool { return len(followsOf("CAPTURE-1")) >= 2 })
	rig.w.queueBaseRead(repo, "main", map[string]string{"CAPTURE-2": ""}, true)
	testwait.Eventually(t, "87 answered and 86's fourth read", func() bool {
		return recordedPull(t, pool, "CAPTURE-2").MergeState == record.MergeStateBlocked && len(followsOf("CAPTURE-1")) >= 4
	})
	for i, line := range followsOf("CAPTURE-1")[:4] {
		if want := float64(unit) * float64(int(2)<<i); line["attempt"] != float64(i+1) || line["wait"] != want {
			t.Fatalf("86's read %d logged %v, want attempt %d after %s: 87's climb leaves 86's ladder as it was", i+1, line, i+1, time.Duration(want))
		}
	}
	for _, asked := range rig.askedOf() {
		if slices.Contains(asked, 86) && slices.Contains(asked, 87) {
			t.Fatalf("GitHub was asked for %v, want 87's climb to read 87 alone while 86 is not due", rig.askedOf())
		}
	}
}
