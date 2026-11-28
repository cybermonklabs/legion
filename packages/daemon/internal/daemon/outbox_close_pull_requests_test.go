package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// closingGitHub answers, beside the fake repository, the calls that close a pull request the fake
// did not open: the read of one, its close, the read of who closed it, the read of its comments and
// a comment on it. It holds the state of each pull request, the login GitHub names as the closer of
// each closed one (none recorded, or nil, for a closed_by of null), the comments of each with their
// authors, and the answers it is told to give in place of its own, keyed by method and path
// (closeRefusal). It records each close and each comment, with the bearer token of the request that
// made it. GitHub names the App whose token made a write its closer and its comment's author
// (tokenLogin).
type closingGitHub struct {
	mu       sync.Mutex
	state    map[int]string
	closedBy map[int]*string
	comments map[int][]closingComment
	refuse   map[string]*closeRefusal
	called   []string
}

// closingComment is one comment on a pull request: its author's login and its body.
type closingComment struct {
	author, body string
}

// closeRefusal is an answer closingGitHub gives a call in place of its own: the status, the body
// and the headers, for the first times calls, or for every call where times is 0.
type closeRefusal struct {
	status int
	body   string
	header map[string]string
	times  int
}

// tokenLogin is the bot login of the App a token roleTokens minted is named for, `<role>[bot]`, as
// roleTokens' AppLogin answers it.
func tokenLogin(token string) string {
	role, _, _ := strings.Cut(token, "-app-token-for-")
	return role + "[bot]"
}

// handle answers r where it is one of those calls, and reports whether it was.
func (g *closingGitHub) handle(w http.ResponseWriter, r *http.Request) bool {
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/")
	if !ok {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	kind, rest, _ := strings.Cut(path, "/")
	digits, comments := strings.CutSuffix(rest, "/comments")
	number, err := strconv.Atoi(digits)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, known := g.state[number]; err != nil || !known || kind != "pulls" && kind != "issues" || kind == "pulls" && comments {
		return false
	}
	if refused := g.refuse[r.Method+" /"+path]; refused != nil && refused.times >= 0 {
		switch {
		case refused.times == 1:
			refused.times = -1
		case refused.times > 1:
			refused.times--
		}
		for name, value := range refused.header {
			w.Header().Set(name, value)
		}
		w.WriteHeader(refused.status)
		_, _ = io.WriteString(w, refused.body)
		return true
	}
	switch {
	case kind == "pulls" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "state": g.state[number], "merged": false})
	case kind == "pulls" && r.Method == http.MethodPatch:
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return false
		}
		g.state[number] = body.State
		if body.State == "closed" {
			g.closedBy[number] = new(tokenLogin(token))
		}
		g.called = append(g.called, fmt.Sprintf("close #%d as %s with %s", number, body.State, token))
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "state": g.state[number]})
	case kind == "issues" && !comments && r.Method == http.MethodGet:
		issue := map[string]any{"number": number, "state": g.state[number], "closed_by": nil}
		if by := g.closedBy[number]; by != nil {
			issue["closed_by"] = map[string]string{"login": *by}
		}
		_ = json.NewEncoder(w).Encode(issue)
	case comments && r.Method == http.MethodGet:
		listed := []map[string]any{}
		for i, comment := range g.comments[number] {
			listed = append(listed, map[string]any{"id": i + 1, "body": comment.body, "user": map[string]string{"login": comment.author}})
		}
		_ = json.NewEncoder(w).Encode(listed)
	case comments && r.Method == http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return false
		}
		g.comments[number] = append(g.comments[number], closingComment{author: tokenLogin(token), body: body.Body})
		g.called = append(g.called, fmt.Sprintf("comment %q on #%d with %s", body.Body, number, token))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": len(g.called)})
	default:
		return false
	}
	return true
}

// newClosingGitHub is closingGitHub over pull requests in state, each with no closer recorded, no
// comment and no refusal.
func newClosingGitHub(state map[int]string) *closingGitHub {
	return &closingGitHub{state: state, closedBy: map[int]*string{}, comments: map[int][]closingComment{}, refuse: map[string]*closeRefusal{}}
}

// calls is each comment and close the handler recorded, in order.
func (g *closingGitHub) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.called)
}

// pull is the state of pull request number and its comments, in order.
func (g *closingGitHub) pull(number int) (string, []closingComment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state[number], slices.Clone(g.comments[number])
}

// The merge of a task's pull request closes the open pull request of each task whose work it
// holds, as the implement App, each with the comment `Merged in #<n>.`: b needs a, c and d, so its
// merge holds their work. a's pull request #1 is on the issue's branch, b's #2, c's #3 and d's #4 on
// their own; a and c are done at their step, b stands at its merge, and d still works its step. #4
// merges first, which queues no close: d stands at no merge. Then #2 merges: the merge queues one
// row that closes #1 and #3, which comments `Merged in #2.` on each and then closes it, and leaves
// #2 and the merged #4 alone. The row run again finds both closed and does nothing more.
//
// The fault it catches is the pull requests of the other tasks left open after the issue merged,
// which GitHub leaves open (its control: merged without the row).
func TestTheMergeClosesEachOtherPullRequestOfTheIssue(t *testing.T) {
	t.Parallel()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "b", needs: []string{"a", "c", "d"}}, plannedTask{id: "c"}, plannedTask{id: "d"})
	ctx := context.Background()
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, move := range []struct {
			task  string
			place record.TaskPlace
			state record.TaskState
		}{
			{"a", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskDone},
			{"c", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskDone},
			{"b", record.TaskPlace{Kind: record.PlaceMerge}, record.TaskRunning},
		} {
			if err := rig.records.MoveTask(ctx, tx, taskBranchIssue.Key, taskBranchIssue.Generation, move.task, move.place, move.state); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("move a and c to done and b to its merge: %v", err)
	}
	closing := newClosingGitHub(map[int]string{1: "open", 3: "open"})
	rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !closing.handle(w, r) && !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})

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
		t.Fatalf("the rows that close pull requests after d's merge = %+v, want none: d stands at no merge", closes)
	}

	merge(2)
	want := record.TaskPullRequestsClose{Generation: taskBranchIssue.Generation, Repo: taskBranchIssue.GitHubRepo, Numbers: []int{1, 3}, Comment: "Merged in #2."}
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
		"close #1 as closed with implement-app-token-for-acme", `comment "Merged in #2." on #1 with implement-app-token-for-acme`,
		"close #3 as closed with implement-app-token-for-acme", `comment "Merged in #2." on #3 with implement-app-token-for-acme`,
	}
	if got := closing.calls(); !slices.Equal(got, calls) {
		t.Fatalf("the comments and closes GitHub received = %q, want %q", got, calls)
	}
}

// closeRig is a rig whose GitHub answers the close of pull requests from closing, and whose outbox
// logs to the buffer it answers.
func closeRig(t *testing.T, closing *closingGitHub) (*taskBranchRig, *syncBuffer) {
	t.Helper()
	rig := newTaskBranchRig(t, plannedTask{id: "a"})
	logged := &syncBuffer{}
	rig.runner.log, rig.runner.now = slog.New(slog.NewTextHandler(logged, nil)), time.Now
	rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !closing.handle(w, r) && !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	return rig, logged
}

// queueClose queues the row that closes numbers of taskBranchIssue with comment at at, and answers
// its id.
func (rig *taskBranchRig) queueClose(t *testing.T, numbers []int, comment string, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	row, err := record.NewOutboxRow(taskBranchIssue.Key, record.TaskPullRequestsClose{Generation: taskBranchIssue.Generation, Numbers: numbers, Comment: comment}, at)
	if err != nil {
		t.Fatalf("make the row that closes %v: %v", numbers, err)
	}
	var id int64
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		id, err = rig.records.EnqueueID(ctx, tx, row)
		return err
	}); err != nil {
		t.Fatalf("queue the row that closes %v: %v", numbers, err)
	}
	return id
}

// closeRowsQueued is how many rows that close pull requests the outbox holds.
func (rig *taskBranchRig) closeRowsQueued(t *testing.T) int {
	t.Helper()
	var queued int
	if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1`,
		string(record.OutboxKindTaskPullRequestsClose)).Scan(&queued); err != nil {
		t.Fatalf("count the rows that close pull requests: %v", err)
	}
	return queued
}

// closeRefusedNotices is each pull-request-close-refused notice the outbox holds, in order.
func (rig *taskBranchRig) closeRefusedNotices(t *testing.T) []record.Notice {
	t.Helper()
	rows, err := rig.pool.Query(context.Background(), `select payload from outbox
		where kind = 'notice' and payload->>'kind' = 'pull-request-close-refused' order by id`)
	var payloads [][]byte
	if err == nil {
		payloads, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
	}
	if err != nil {
		t.Fatalf("read the pull-request-close-refused notices: %v", err)
	}
	notices := make([]record.Notice, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &notices[i]); err != nil {
			t.Fatalf("decode a pull-request-close-refused notice: %v", err)
		}
	}
	return notices
}

// A close GitHub refuses for good ends its row and tells the issue's architect once, and a failure
// that can pass runs the row again. The row closes #1 and #3 of LEGION-208 with `Merged in #2.`;
// #3 is open and closes with its one comment in each case, and #1:
//   - its close answered 403 with no rate-limit header: #1 stays open with no comment, the row
//     ends, and one pull-request-close-refused notice to the architect names #1, the comment and
//     GitHub's answer, and not #3; no pass of the outbox sends the row again;
//   - its close answered 403 with x-ratelimit-remaining: 0 once: the first run leaves the row
//     queued and tells nobody, and the next closes #1 and ends the row, telling nobody;
//   - its read answered 502 once: the same;
//   - its read answered 404: as the 403 with no rate-limit header;
//   - closed by a person already: #1 gets no comment, the row ends, nobody is told, and one INFO
//     line names #1 and that login;
//   - closed with a closed_by of null: the same, the INFO line naming #1 and the null.
//
// The faults it catches are a close refused for good that ends its row and tells nobody (its
// control: the finish of the refusal with no notice), a rate limit read as a refusal for good (its
// control: every 403 read as refused for good, under which the second case ends the row with a
// notice), and a closer GitHub names as null retried for ever, so the pull requests after it are
// never closed (its control: a null read as a failure that can pass, under which #3 stays open and
// the row stays queued).
func TestACloseGitHubRefusesForGoodEndsItsRowAndTellsTheArchitectOnce(t *testing.T) {
	t.Parallel()
	forbidden := `{"message":"Resource not accessible by integration"}`
	merged := []closingComment{{author: "implement[bot]", body: "Merged in #2."}}
	for _, tc := range []struct {
		name     string
		state1   string
		closedBy *string
		refuse   map[string]*closeRefusal
		runs     int
		told     []string
		after1   string
		comments []closingComment
		logged   []string
	}{
		{name: "its close refused for good", state1: "open", refuse: map[string]*closeRefusal{"PATCH /pulls/1": {status: http.StatusForbidden, body: forbidden}},
			runs: 1, told: []string{"pull request #1 of LEGION-208", `"Merged in #2."`, "403 to PATCH /pulls/1", "Resource not accessible by integration"}, after1: "open"},
		{name: "its close rate limited once", state1: "open",
			refuse: map[string]*closeRefusal{"PATCH /pulls/1": {status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`, header: map[string]string{"X-RateLimit-Remaining": "0"}, times: 1}},
			runs:   2, after1: "closed", comments: merged},
		{name: "its read answered 502 once", state1: "open", refuse: map[string]*closeRefusal{"GET /pulls/1": {status: http.StatusBadGateway, body: "bad gateway", times: 1}},
			runs: 2, after1: "closed", comments: merged},
		{name: "its read answered 404", state1: "open", refuse: map[string]*closeRefusal{"GET /pulls/1": {status: http.StatusNotFound, body: `{"message":"Not Found"}`}},
			runs: 1, told: []string{"pull request #1 of LEGION-208", `"Merged in #2."`, "404 to GET /pulls/1", "Not Found"}, after1: "open"},
		{name: "closed by a person", state1: "closed", closedBy: new("a-person"), runs: 1, after1: "closed",
			logged: []string{"level=INFO", "pullRequest=1 ", "closedBy=a-person"}},
		{name: "closed with a closed_by of null", state1: "closed", runs: 1, after1: "closed",
			logged: []string{"level=INFO", "pullRequest=1 ", `unread="closed_by is null"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			closing := newClosingGitHub(map[int]string{1: tc.state1, 3: "open"})
			if tc.closedBy != nil {
				closing.closedBy[1] = tc.closedBy
			}
			maps.Copy(closing.refuse, tc.refuse)
			rig, logged := closeRig(t, closing)
			queued := time.Now().UTC()
			id := rig.queueClose(t, []int{1, 3}, "Merged in #2.", queued)

			first := rig.attemptAt(t, id, queued)
			if tc.runs == 2 {
				if first == nil || rig.closeRowsQueued(t) != 1 || len(rig.closeRefusedNotices(t)) != 0 {
					t.Fatalf("the first run = %v, with %d rows queued and the notices %+v; want a failure that leaves the row queued and tells nobody",
						first, rig.closeRowsQueued(t), rig.closeRefusedNotices(t))
				}
				if state, _ := closing.pull(1); state != "open" {
					t.Fatalf("#1 after the first run is %s, want open", state)
				}
				if err := rig.attemptAt(t, id, queued.Add(2*time.Hour)); err != nil {
					t.Fatalf("the second run: %v", err)
				}
			}
			if queuedRows := rig.closeRowsQueued(t); queuedRows != 0 {
				t.Errorf("the rows that close pull requests after the last run = %d, want none", queuedRows)
			}
			if state, comments := closing.pull(3); state != "closed" || !slices.Equal(comments, merged) {
				t.Fatalf("#3 is %s with the comments %+v, want closed with %+v", state, comments, merged)
			}
			if state, comments := closing.pull(1); state != tc.after1 || !slices.Equal(comments, tc.comments) {
				t.Fatalf("#1 is %s with the comments %+v, want %s with %+v", state, comments, tc.after1, tc.comments)
			}
			notices := rig.closeRefusedNotices(t)
			if tc.told == nil {
				if len(notices) != 0 {
					t.Fatalf("the pull-request-close-refused notices = %+v, want none", notices)
				}
			} else {
				if len(notices) != 1 || notices[0].Role != claim.RoleArchitect {
					t.Fatalf("the pull-request-close-refused notices = %+v, want one to the architect", notices)
				}
				for _, part := range tc.told {
					if !strings.Contains(notices[0].Reason, part) {
						t.Fatalf("the notice's reason %q does not name %q", notices[0].Reason, part)
					}
				}
				if strings.Contains(notices[0].Reason, "#3") {
					t.Fatalf("the notice's reason %q names #3, which closed", notices[0].Reason)
				}
				var due []record.OutboxRow
				if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					due, err = rig.records.DueRows(ctx, tx, "LEGION", taskBranchIssue.Key, nil, queued.Add(3*time.Hour))
					return err
				}); err != nil {
					t.Fatalf("read the due rows of %s: %v", taskBranchIssue.Key, err)
				}
				if slices.ContainsFunc(due, func(row record.OutboxRow) bool { return row.Kind == record.OutboxKindTaskPullRequestsClose }) {
					t.Fatalf("the due rows of %s hold a row that closes pull requests, want none: no pass sends it again", taskBranchIssue.Key)
				}
			}
			if tc.logged != nil {
				if !slices.ContainsFunc(strings.Split(logged.String(), "\n"), func(line string) bool {
					return !slices.ContainsFunc(tc.logged, func(part string) bool { return !strings.Contains(line, part) })
				}) {
					t.Fatalf("the outbox logged:\n%s\nwant one line holding each of %q", logged.String(), tc.logged)
				}
			}
		})
	}
}

// The notice of a close GitHub refuses for good names each pull request it left open, in the words
// for one and for several, the comment and GitHub's answer to each.
func TestTheNoticeOfACloseGitHubRefusesForGoodNamesEachPullRequest(t *testing.T) {
	t.Parallel()
	forbidden := &githubrest.Answer{Method: http.MethodPatch, Path: "/pulls/43", Status: http.StatusForbidden, Body: `{"message":"Resource not accessible by integration"}`}
	missing := func(number int) error {
		return fmt.Errorf("close pull request #%d on acme/widgets: %w", number,
			&githubrest.Answer{Method: http.MethodGet, Path: fmt.Sprintf("/pulls/%d", number), Status: http.StatusNotFound, Body: `{"message":"Not Found"}`})
	}
	for _, tc := range []struct {
		name    string
		refused []closeRefused
		want    string
	}{
		{"one", []closeRefused{{number: 43, err: forbidden}},
			`GitHub refused Legion's close of pull request #43 of LEGION-209, as it would refuse it again, so it stays open and Legion will not try again. ` +
				`Close it with gh pr close where its work should not land. The comment Legion was writing: "Merged in #45." ` +
				`GitHub answered: 403 to PATCH /pulls/43: {"message":"Resource not accessible by integration"}`},
		{"two", []closeRefused{{number: 43, err: forbidden}, {number: 44, err: missing(44)}},
			`GitHub refused Legion's close of pull requests #43 and #44 of LEGION-209, as it would refuse each again, so each stays open and Legion will not try again. ` +
				`Close each with gh pr close where its work should not land. The comment Legion was writing: "Merged in #45." ` +
				`GitHub answered: #43: 403 to PATCH /pulls/43: {"message":"Resource not accessible by integration"}; #44: 404 to GET /pulls/44: {"message":"Not Found"}`},
		{"three", []closeRefused{{number: 43, err: forbidden}, {number: 44, err: missing(44)}, {number: 47, err: missing(47)}},
			`GitHub refused Legion's close of pull requests #43, #44 and #47 of LEGION-209, as it would refuse each again, so each stays open and Legion will not try again. ` +
				`Close each with gh pr close where its work should not land. The comment Legion was writing: "Merged in #45." ` +
				`GitHub answered: #43: 403 to PATCH /pulls/43: {"message":"Resource not accessible by integration"}; #44: 404 to GET /pulls/44: {"message":"Not Found"}; ` +
				`#47: 404 to GET /pulls/47: {"message":"Not Found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notice := (&pullRequestsCloseRefused{issue: "LEGION-209", comment: "Merged in #45.", refused: tc.refused}).notice()
			if notice.Kind != "pull-request-close-refused" || notice.Role != claim.RoleArchitect || notice.Reason != tc.want {
				t.Fatalf("the notice = %s to %s: %q, want pull-request-close-refused to the architect: %q", notice.Kind, notice.Role, notice.Reason, tc.want)
			}
		})
	}
}
