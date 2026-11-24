package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// closingGitHub answers, beside the fake repository, the calls that close a pull request the fake
// did not open: the read of one, a comment on it and its close. It holds the state of each pull
// request and records each comment and each close, with the bearer token of the request that made
// it.
type closingGitHub struct {
	mu     sync.Mutex
	state  map[int]string
	called []string
}

// handle answers r where it is one of those calls, and reports whether it was.
func (g *closingGitHub) handle(w http.ResponseWriter, r *http.Request) bool {
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/")
	if !ok {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	defer g.mu.Unlock()
	if rest, ok := strings.CutPrefix(path, "pulls/"); ok && (r.Method == http.MethodGet || r.Method == http.MethodPatch) {
		number, err := strconv.Atoi(rest)
		if _, known := g.state[number]; err != nil || !known {
			return false
		}
		if r.Method == http.MethodPatch {
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return false
			}
			g.state[number] = body.State
			g.called = append(g.called, fmt.Sprintf("close #%d as %s with %s", number, body.State, token))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "state": g.state[number]})
		return true
	}
	if rest, ok := strings.CutPrefix(path, "issues/"); ok && r.Method == http.MethodPost && strings.HasSuffix(rest, "/comments") {
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return false
		}
		g.called = append(g.called, fmt.Sprintf("comment %q on #%s with %s", body.Body, strings.TrimSuffix(rest, "/comments"), token))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": len(g.called)})
		return true
	}
	return false
}

// calls is each comment and close the handler recorded, in order.
func (g *closingGitHub) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.called)
}

// The merge of the pull request of the task that merges closes each other open pull request of the
// issue, as the implement App, each with the comment `Merged in #<n>.`: b needs a, c and d, so b is
// the task that merges. a's pull request #1 is on the issue's branch, b's #2, c's #3 and d's #4 on
// their own. #4 merges first, which queues no close: d is no task that merges. Then #2 merges: the
// merge queues one row that closes #1 and #3, which comments `Merged in #2.` on each and then closes
// it, and leaves #2 and the merged #4 alone. The row run again finds both closed and does nothing
// more.
//
// The fault it catches is the pull requests of the other tasks left open after the issue merged,
// which GitHub leaves open (its control: merged without the row).
func TestTheMergeClosesEachOtherPullRequestOfTheIssue(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "b", needs: []string{"a", "c", "d"}}, plannedTask{id: "c"}, plannedTask{id: "d"})
	closing := &closingGitHub{state: map[int]string{1: "open", 3: "open"}}
	rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !closing.handle(w, r) && !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	ctx := context.Background()
	for _, seed := range []struct {
		number int
		task   string
	}{{1, "a"}, {2, "b"}, {3, "c"}, {4, "d"}} {
		pr := record.PullRequest{Issue: taskBranchIssue.Key, Repo: "acme/widgets", Number: seed.number, Branch: rig.branch(t, seed.task), Base: "main",
			HeadSHA: "head-" + seed.task, Failing: []string{}, State: record.PullRequestOpen, MergeState: record.MergeStateClean}
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error { return rig.records.PutPullRequest(ctx, tx, pr) }); err != nil {
			t.Fatalf("record pull request #%d: %v", seed.number, err)
		}
	}
	merge := func(number int) {
		t.Helper()
		fact := intake.PullRequestMerged{Repo: "acme/widgets", Number: number, MergeSHA: fmt.Sprintf("merge-%d", number)}
		if _, err := intake.ApplyFact(ctx, rig.pool, "test", fmt.Sprintf("merged-%d", number), fact, rig.runner.handlers...); err != nil {
			t.Fatalf("apply the merge of #%d: %v", number, err)
		}
	}
	closeRows := func() []record.TaskPullRequestsClose {
		t.Helper()
		rows, err := rig.pool.Query(ctx, `select payload from outbox where kind = $1 order by id`, string(record.OutboxKindTaskPullRequestsClose))
		var payloads [][]byte
		if err == nil {
			payloads, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
		}
		if err != nil {
			t.Fatalf("read the rows that close pull requests: %v", err)
		}
		closes := make([]record.TaskPullRequestsClose, len(payloads))
		for i, payload := range payloads {
			if err := json.Unmarshal(payload, &closes[i]); err != nil {
				t.Fatalf("decode a row that closes pull requests: %v", err)
			}
		}
		return closes
	}

	merge(4)
	if closes := closeRows(); len(closes) != 0 {
		t.Fatalf("the rows that close pull requests after d's merge = %+v, want none: d is no task that merges", closes)
	}

	merge(2)
	want := record.TaskPullRequestsClose{Generation: taskBranchIssue.Generation, Numbers: []int{1, 3}, Comment: "Merged in #2."}
	closes := closeRows()
	if len(closes) != 1 || !reflect.DeepEqual(closes[0], want) {
		t.Fatalf("the rows that close pull requests after b's merge = %+v, want the one %+v", closes, want)
	}
	row := mustOutboxRow(t, taskBranchIssue.Key, closes[0], time.Now())
	for range 2 {
		if err := rig.runner.execute(ctx, row); err != nil {
			t.Fatalf("execute the row that closes pull requests: %v", err)
		}
	}
	calls := []string{
		`comment "Merged in #2." on #1 with implement-app-token-for-acme`, "close #1 as closed with implement-app-token-for-acme",
		`comment "Merged in #2." on #3 with implement-app-token-for-acme`, "close #3 as closed with implement-app-token-for-acme",
	}
	if got := closing.calls(); !slices.Equal(got, calls) {
		t.Fatalf("the comments and closes GitHub received = %q, want %q", got, calls)
	}
}
