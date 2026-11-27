package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// mergeReadIssue is the closed issue every merge read test reads the merge of, at generation 3.
const mergeReadIssue = "ACME-1"

// mergeReadStart is when each rig's clock stands as it begins: a UTC day's late evening, so a test
// that waits two days crosses two UTC midnights.
var mergeReadStart = time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)

// mergeReadBoard is Dispatch as a merge read reads it: the issue's links and its results, each read
// counted, and each answering failErr while fail is set.
type mergeReadBoard struct {
	*outboxDispatch
	mu      sync.Mutex
	links   []dispatch.IssueLink
	results dispatch.IssueResults
	reads   map[string]int
	failErr error
}

func (d *mergeReadBoard) count(read string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads[read]++
	return d.failErr
}

func (d *mergeReadBoard) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	if err := d.count("issue"); err != nil {
		return dispatch.Issue{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return dispatch.Issue{Key: key, Status: "done", Links: slices.Clone(d.links)}, nil
}

func (d *mergeReadBoard) IssueResults(context.Context, string) (dispatch.IssueResults, error) {
	if err := d.count("results"); err != nil {
		return dispatch.IssueResults{}, err
	}
	return d.results, nil
}

// counted is how many reads of read the board answered or refused.
func (d *mergeReadBoard) counted(read string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads[read]
}

// mergeReadPull is one pull request as the GitHub stand-in answers it.
type mergeReadPull struct {
	branch, head, merge string
	merged              bool
	at                  time.Time
}

// mergeReadGitHub is GitHub as a merge read reads it: each pull request it holds, the pages of
// pull requests each commit is in, the redirects it answers instead, and the paths it fails, each
// GET counted by its path.
type mergeReadGitHub struct {
	url       string
	mu        sync.Mutex
	pulls     map[string]mergeReadPull // "owner/repo#n"
	pages     map[string][][]int       // "owner/repo@sha", each page's numbers
	redirects map[string]string        // request URI to the path it moved to
	failing   map[string]int           // request URI to failures left
	answered  func(path string)
	gets      []string
}

func newMergeReadGitHub(t *testing.T) *mergeReadGitHub {
	t.Helper()
	g := &mergeReadGitHub{pulls: map[string]mergeReadPull{}, pages: map[string][][]int{}, redirects: map[string]string{}, failing: map[string]int{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.gets = append(g.gets, r.URL.Path)
		failing := g.failing[r.URL.RequestURI()]
		if failing > 0 {
			g.failing[r.URL.RequestURI()] = failing - 1
		}
		moved := g.redirects[r.URL.RequestURI()]
		answered := g.answered
		g.mu.Unlock()
		if answered != nil {
			defer answered(r.URL.Path)
		}
		switch {
		case r.Method != http.MethodGet:
			githubtest.Unexpected(t, w, r)
		case failing > 0:
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		case moved != "":
			http.Redirect(w, r, moved, http.StatusMovedPermanently)
		default:
			g.answer(t, w, r)
		}
	}})
	return g
}

// answer writes the pull request or the page r asks for.
func (g *mergeReadGitHub) answer(t *testing.T, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case len(parts) == 5 && parts[0] == "repos" && parts[3] == "pulls":
		pull, ok := g.pulls[parts[1]+"/"+parts[2]+"#"+parts[4]]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		number, _ := strconv.Atoi(parts[4])
		answer := map[string]any{"number": number, "state": "closed", "merged": pull.merged, "merge_commit_sha": pull.merge,
			"head": map[string]string{"ref": pull.branch, "sha": pull.head}, "base": map[string]string{"ref": "main"}}
		if pull.merged {
			answer["merged_at"] = pull.at.Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(answer)
	case len(parts) >= 5 && parts[len(parts)-1] == "pulls" && parts[len(parts)-3] == "commits":
		repo, sha := parts[1]+"/"+parts[2], parts[len(parts)-2]
		if parts[0] == "repositories" {
			repo = "acme/widgets"
		}
		pages := g.pages[repo+"@"+sha]
		page := 1
		if asked := r.URL.Query().Get("page"); asked != "" {
			page, _ = strconv.Atoi(asked)
		}
		if page > len(pages) {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if page < len(pages) {
			w.Header().Set("Link", fmt.Sprintf("<%s%s?per_page=100&page=%d>; rel=\"next\"", g.url, r.URL.Path, page+1))
		}
		listed := []map[string]int{}
		for _, number := range pages[page-1] {
			listed = append(listed, map[string]int{"number": number})
		}
		_ = json.NewEncoder(w).Encode(listed)
	default:
		githubtest.Unexpected(t, w, r)
	}
}

// count is how many GETs the stand-in was asked, of every path whose suffix is suffix, "" for all.
func (g *mergeReadGitHub) count(suffix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, path := range g.gets {
		if strings.HasSuffix(path, suffix) {
			n++
		}
	}
	return n
}

// mergeReadRig is one merge read of mergeReadIssue over the real outbox runner and a record of its
// own, on a clock the test moves.
type mergeReadRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records record.Store
	board   *mergeReadBoard
	github  *mergeReadGitHub
	handler *outboxFactHandler
	runner  *outbox
	mu      sync.Mutex
	now     time.Time
}

func newMergeReadRig(t *testing.T) *mergeReadRig {
	t.Helper()
	r := &mergeReadRig{t: t, ctx: context.Background(), pool: isolatedOutboxPool(t), records: record.NewStore(),
		board: &mergeReadBoard{outboxDispatch: &outboxDispatch{}, reads: map[string]int{}}, github: newMergeReadGitHub(t),
		handler: &outboxFactHandler{}, now: mergeReadStart}
	putOutboxIssue(t, r.pool, r.records, record.Issue{Key: mergeReadIssue, Tree: mergeReadIssue, Project: "ACME", Title: mergeReadIssue,
		Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "todo", Rank: "A", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", LastDispatchSeq: 9})
	r.restart()
	return r
}

// restart is a new runner over the same record and stand-ins: a daemon that started again, whose
// pass begins empty and whose only memory of the read is the row's progress.
func (r *mergeReadRig) restart() {
	r.runner = &outbox{log: quietLogger(), pool: r.pool, records: r.records, dispatch: r.board, handlers: []intake.Handler{r.handler},
		tokens: outboxTokens{}, githubAPI: r.github.url, dispatchProject: "ACME", project: "acme", now: r.clock}
}

func (r *mergeReadRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// at moves the rig's clock to at.
func (r *mergeReadRig) at(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = at
}

// exec runs each statement on the record, in a transaction of its own.
func (r *mergeReadRig) exec(statements ...string) {
	r.t.Helper()
	for _, statement := range statements {
		if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, statement)
			return err
		}); err != nil {
			r.t.Fatalf("%s: %v", statement, err)
		}
	}
}

// clock records the clock a clear kept of acme/widgets#number for the issue.
func (r *mergeReadRig) clearedClock(number int) {
	r.exec(fmt.Sprintf(`insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
		values ('acme/widgets', %d, '%s', now(), now())`, number, mergeReadIssue))
}

// productionCheck records the issue's closed production check of pull request number at merge.
func (r *mergeReadRig) productionCheck(number int, merge string) {
	r.exec(fmt.Sprintf(`insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, closed_at, outcome)
		values ('%s', 'ACME', '%s', 'acme/widgets', %d, '%s', now(), now() + interval '1 hour', now(), 'recorded')`,
		mergeReadIssue, mergeReadIssue, number, merge))
}

// queue writes the issue's pending merge_read row, a person's request at generation 3.
func (r *mergeReadRig) queue() {
	r.t.Helper()
	row, err := record.NewOutboxRow(mergeReadIssue, record.MergeRead{Issue: mergeReadIssue, Generation: 3, Requester: record.RequesterPerson,
		Request: "issue.updated: todo"}, r.clock())
	if err != nil {
		r.t.Fatalf("build the merge_read row: %v", err)
	}
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, row) }); err != nil {
		r.t.Fatalf("queue the merge_read row: %v", err)
	}
}

// pending is the issue's merge_read row as the outbox holds it: its progress and when it is due
// next, and whether it stands at all.
func (r *mergeReadRig) pending() (record.MergeReadProgress, time.Time, bool) {
	r.t.Helper()
	var payload []byte
	var due time.Time
	err := r.pool.QueryRow(r.ctx, `select payload, next_at from outbox where issue = $1 and kind = 'merge_read'`, mergeReadIssue).Scan(&payload, &due)
	if err == pgx.ErrNoRows {
		return record.MergeReadProgress{}, time.Time{}, false
	}
	if err != nil {
		r.t.Fatalf("read the merge_read row: %v", err)
	}
	decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindMergeRead, Issue: mergeReadIssue, Payload: payload})
	if err != nil {
		r.t.Fatalf("decode the merge_read row: %v", err)
	}
	return decoded.(record.MergeRead).Progress, due.UTC(), true
}

// turn runs the outbox once at the rig's clock: the row runs where it is due.
func (r *mergeReadRig) turn() {
	r.t.Helper()
	if err := r.runner.RunOnce(r.ctx); err != nil {
		r.t.Fatalf("run the outbox: %v", err)
	}
}

// gets is every GET the read sent: Dispatch's reads and GitHub's.
func (r *mergeReadRig) gets() int {
	return r.board.counted("issue") + r.board.counted("results") + r.github.count("")
}

// drain runs the read's turns, each at the moment its row is due, until the row is gone or waits
// unread or stopped, for at most limit turns, and answers each turn's GETs.
func (r *mergeReadRig) drain(limit int) []int {
	r.t.Helper()
	var sent []int
	for range limit {
		progress, due, standing := r.pending()
		if !standing || progress.State != "" && !progress.Refresh {
			return sent
		}
		if due.After(r.clock()) {
			r.at(due)
		}
		before := r.gets()
		r.turn()
		sent = append(sent, r.gets()-before)
	}
	r.t.Fatalf("the merge read did not end in %d turns", limit)
	return nil
}

// facts is the MergeRead facts the read posted.
func (r *mergeReadRig) facts() []intake.MergeRead {
	var read []intake.MergeRead
	for _, fact := range r.handler.facts() {
		if merge, ok := fact.(intake.MergeRead); ok {
			read = append(read, merge)
		}
	}
	return read
}

// The merge read of a closed issue whose record proves no merge takes its candidates from every
// source and posts the newest merge that names the issue. Its sources are the issue's links, its
// cleared clock, and the merge commits Dispatch's results compare a check with, which GitHub maps
// to their pull requests. A production check of pull request 0 is one more such merge commit and
// proves no merge itself. A candidate whose branch names another issue is not taken, and of two
// merged candidates the later merge is posted. The fact names the pending row's issue and
// generation, and the row finishes. A failed stage posts no fact: it counts its attempt and keeps
// the row for its retry. A search with no answering candidate posts no fact, and saves its
// progress as completed and unread.
func TestAMergeReadRecoversTheSnapshot(t *testing.T) {
	t.Parallel()
	const resultsMerge, productionMerge = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	earlier, later, latest := mergeReadStart.Add(-72*time.Hour), mergeReadStart.Add(-48*time.Hour), mergeReadStart.Add(-24*time.Hour)

	t.Run("the newest merge that names the issue", func(t *testing.T) {
		t.Parallel()
		r := newMergeReadRig(t)
		r.clearedClock(41)
		r.productionCheck(0, productionMerge)
		r.board.links = []dispatch.IssueLink{{URL: "https://github.com/acme/widgets/pull/70", Kind: "github_pr"}, {URL: "https://tracker.example/ACME-1"}}
		r.board.results = dispatch.IssueResults{Issue: mergeReadIssue, Checks: []dispatch.CheckBlock{{BlockID: "k1", When: dispatch.CheckAfterMerge,
			ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: resultsMerge}}}}}
		r.github.pages["acme/widgets@"+resultsMerge] = [][]int{{72}}
		r.github.pages["acme/widgets@"+productionMerge] = [][]int{{73}}
		r.github.pulls["acme/widgets#41"] = mergeReadPull{branch: "legion/ACME-1", head: "h41"}
		r.github.pulls["acme/widgets#70"] = mergeReadPull{branch: "legion/ACME-1", head: "h70", merge: "m70", merged: true, at: earlier}
		r.github.pulls["acme/widgets#72"] = mergeReadPull{branch: "legion/ACME-1", head: "h72", merge: "m72", merged: true, at: later}
		r.github.pulls["acme/widgets#73"] = mergeReadPull{branch: "legion/ACME-9", head: "h73", merge: "m73", merged: true, at: latest}
		r.queue()

		sent := r.drain(40)
		for i, n := range sent {
			if n > 1 {
				t.Fatalf("turn %d sent %d GETs, want at most one: %v", i+1, n, sent)
			}
		}
		want := []intake.MergeRead{{Issue: mergeReadIssue, Generation: 3, Merge: record.Merge{Number: 72, Head: "h72", SHA: "m72", Base: "main",
			Repo: "acme/widgets", MergedAt: later}}}
		if got := r.facts(); !slices.Equal(got, want) {
			t.Fatalf("the read posted %+v, want the one fact of #72, the later merge that names the issue: #73 names ACME-9", got)
		}
		if _, _, standing := r.pending(); standing {
			t.Fatal("the merge_read row stands after the read posted its fact, want it finished")
		}
		for _, path := range []string{"/pulls/41", "/pulls/70", "/pulls/72", "/pulls/73", resultsMerge + "/pulls", productionMerge + "/pulls"} {
			if n := r.github.count(path); n != 1 {
				t.Fatalf("GitHub was asked %s %d times, want once: each candidate and base is read once", path, n)
			}
		}
		if r.board.counted("issue") != 1 || r.board.counted("results") != 1 {
			t.Fatalf("Dispatch was asked the issue %d and its results %d times, want once each", r.board.counted("issue"), r.board.counted("results"))
		}
	})

	t.Run("a failed stage posts no fact and keeps the row", func(t *testing.T) {
		t.Parallel()
		r := newMergeReadRig(t)
		r.board.failErr = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
		r.queue()
		r.turn()
		progress, due, standing := r.pending()
		if !standing || len(r.facts()) != 0 {
			t.Fatalf("after a failed read the row stands %t and %d facts were posted, want the row kept and no fact", standing, len(r.facts()))
		}
		if progress.Attempts != 1 || progress.Failures != 1 || progress.Reserved || progress.Cause == "" || !progress.RetryAt.Equal(mergeReadStart.Add(time.Minute)) ||
			!due.Equal(progress.RetryAt) {
			t.Fatalf("the progress after a failed read = %+v due %s, want one attempt and one failure, its cause, and its retry a minute on", progress, due)
		}
	})

	t.Run("a search no candidate answers saves its progress unread", func(t *testing.T) {
		t.Parallel()
		r := newMergeReadRig(t)
		r.clearedClock(41)
		r.github.pulls["acme/widgets#41"] = mergeReadPull{branch: "legion/ACME-1", head: "h41"}
		r.queue()
		r.drain(10)
		progress, _, standing := r.pending()
		if !standing || len(r.facts()) != 0 {
			t.Fatalf("after a search with no merge the row stands %t and %d facts were posted, want the row kept and no fact", standing, len(r.facts()))
		}
		if progress.State != "unread" || len(progress.Candidates) != 1 || progress.Candidates[0].Answer == nil || progress.Candidates[0].Answer.Merged {
			t.Fatalf("the progress = %+v, want it unread with #41 read and not merged", progress)
		}
	})
}

// A merge read resumes where it stopped and spends a bounded pass. Its results compare checks with
// two merge commits, one of them twice, which is read once. The first commit's pull requests take
// a full page of 100, then a page GitHub moved, whose redirect is a GET of its own. Each turn sends
// at most one GET, and eight GET attempts spend a pass, so the ninth turn sends nothing and waits
// for the next pass. A daemon that starts again goes on from the saved progress, and replays no
// first page and loses no candidate. A failed attempt spends one too. A turn that sent its GET and
// stopped before it saved the answer leaves its attempt spent, and the next turn counts it
// uncertain.
func TestAMergeReadResumesItsPageAndBudget(t *testing.T) {
	t.Parallel()
	const first, second = "3333333333333333333333333333333333333333", "4444444444444444444444444444444444444444"
	r := newMergeReadRig(t)
	full := make([]int, 100)
	for i := range full {
		full[i] = 1000 + i
	}
	r.board.results = dispatch.IssueResults{Issue: mergeReadIssue, Checks: []dispatch.CheckBlock{
		{BlockID: "k1", When: dispatch.CheckAfterMerge, ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: first},
			{Kind: dispatch.BasisBranch, Branch: "release", MergeCommit: first}}},
		{BlockID: "k2", When: dispatch.CheckAfterMerge, ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: first},
			{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: second}}},
	}}
	r.github.pages["acme/widgets@"+first] = [][]int{full, {2000}}
	r.github.pages["acme/widgets@"+second] = [][]int{{3000}}
	r.github.redirects["/repos/acme/widgets/commits/"+first+"/pulls?per_page=100&page=2"] = "/repositories/42/commits/" + first + "/pulls?per_page=100&page=2"
	for _, number := range append(slices.Clone(full), 2000, 3000) {
		r.github.pulls["acme/widgets#"+strconv.Itoa(number)] = mergeReadPull{branch: "legion/ACME-1", head: fmt.Sprintf("h%d", number)}
	}
	r.queue()

	var sent []int
	for range 8 {
		before := r.gets()
		r.turn()
		sent = append(sent, r.gets()-before)
		_, due, _ := r.pending()
		r.at(due)
	}
	if !slices.Equal(sent, []int{1, 1, 1, 1, 1, 1, 1, 1}) {
		t.Fatalf("the first eight turns sent %v GETs, want one each", sent)
	}
	if r.github.count(first+"/pulls") != 3 || r.github.count(second+"/pulls") != 1 {
		t.Fatalf("GitHub was asked %d pages of %s (its two pages and the redirect, want 3) and %d of %s (want 1)",
			r.github.count(first+"/pulls"), first, r.github.count(second+"/pulls"), second)
	}
	before := r.gets()
	r.turn()
	progress, due, _ := r.pending()
	if r.gets() != before || !due.Equal(mergeReadStart.Add(mergeReadPass)) {
		t.Fatalf("the ninth turn sent %d GETs and waits until %s, want none and the next pass at %s", r.gets()-before, due, mergeReadStart.Add(mergeReadPass))
	}
	if progress.Attempts != 8 || len(progress.Candidates) != 102 {
		t.Fatalf("the progress after the pass = %d attempts and %d candidates, want 8 and the 102 the pages listed", progress.Attempts, len(progress.Candidates))
	}

	// The daemon starts again: its pass begins empty, and the read goes on from the row.
	r.restart()
	r.at(due)
	next := progress.Candidates[2]
	failing := fmt.Sprintf("/repos/acme/widgets/pulls/%d", next.Number)
	r.github.mu.Lock()
	r.github.failing[failing] = 1
	r.github.mu.Unlock()
	before = r.gets()
	r.turn()
	progress, due, _ = r.pending()
	if r.gets()-before != 1 || progress.Attempts != 9 || progress.Failures != 1 {
		t.Fatalf("the restarted turn sent %d GETs to %d attempts and %d failures, want the one failed GET counted as the ninth attempt", r.gets()-before,
			progress.Attempts, progress.Failures)
	}
	if r.github.count(first+"/pulls") != 3 || len(progress.Candidates) != 102 || progress.Candidates[2] != next {
		t.Fatalf("after the restart GitHub was asked %d pages of %s and the read holds %d candidates, want no page replayed and none lost",
			r.github.count(first+"/pulls"), first, len(progress.Candidates))
	}

	// The retry's answer comes, and the daemon stops before it saves it.
	r.at(due)
	r.github.mu.Lock()
	r.github.answered = func(path string) {
		if path == failing {
			r.exec(fmt.Sprintf(`update outbox set lease_token = 'a-later-runner', lease_until = '%s' where kind = 'merge_read'`,
				due.Add(-time.Second).Format(time.RFC3339Nano)))
		}
	}
	r.github.mu.Unlock()
	r.turn()
	r.github.mu.Lock()
	r.github.answered = nil
	r.github.mu.Unlock()
	progress, _, _ = r.pending()
	if !progress.Reserved || progress.Attempts != 10 || progress.Candidates[2].Answer != nil {
		t.Fatalf("the progress after a turn that stopped before it saved = %+v, want its tenth attempt reserved and no answer", progress)
	}
	r.restart()
	before = r.gets()
	r.turn()
	progress, _, _ = r.pending()
	if r.gets() != before || progress.Reserved || progress.Uncertain != 1 || progress.Attempts != 10 || progress.Failures != 2 {
		t.Fatalf("the turn after the stop sent %d GETs and left %+v, want none sent, the reserved attempt spent and counted uncertain", r.gets()-before, progress)
	}
}

// A merge read tries a failed stage three times: a minute and then five minutes after a failure.
// No turn in between sends anything. The third failure stops the read with its progress unread,
// and no fourth attempt comes, across days, until new input does: a door's refresh whose links
// name a merged pull request. A search that ended with no answer sends nothing either across two
// UTC days of turns. A refresh with the same links reads the links alone, and a refresh with a new
// link reads it and posts its merge.
func TestAMergeReadStopsAfterThreeStageFailures(t *testing.T) {
	t.Parallel()
	refresh := func(r *mergeReadRig) {
		r.exec(fmt.Sprintf(`update outbox set payload = jsonb_set(payload, '{progress,refresh}', 'true'), next_at = '%s' where kind = 'merge_read'`,
			r.clock().Format(time.RFC3339Nano)))
	}
	twoDays := func(r *mergeReadRig) {
		t.Helper()
		before := r.gets()
		for hour := 1; hour <= 50; hour++ {
			r.at(r.clock().Add(time.Hour))
			r.turn()
		}
		if r.gets() != before {
			t.Fatalf("across two days of turns the read sent %d GETs, want none", r.gets()-before)
		}
	}

	t.Run("three failures stop the read until new input", func(t *testing.T) {
		t.Parallel()
		r := newMergeReadRig(t)
		r.board.failErr = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
		r.queue()
		for i, wait := range []time.Duration{0, time.Minute, 5 * time.Minute} {
			r.at(r.clock().Add(wait - 30*time.Second))
			r.turn()
			if r.board.counted("issue") != i {
				t.Fatalf("a turn before attempt %d is due sent %d reads in all, want %d", i+1, r.board.counted("issue"), i)
			}
			r.at(r.clock().Add(30 * time.Second))
			r.turn()
			if r.board.counted("issue") != i+1 {
				t.Fatalf("attempt %d sent %d reads in all, want %d", i+1, r.board.counted("issue"), i+1)
			}
		}
		progress, _, _ := r.pending()
		if progress.State != "stopped" || progress.Failures != 3 || progress.Attempts != 3 {
			t.Fatalf("the progress after three failures = %+v, want it stopped after three attempts", progress)
		}
		twoDays(r)
		r.board.mu.Lock()
		r.board.failErr = nil
		r.board.links = []dispatch.IssueLink{{URL: "https://github.com/acme/widgets/pull/70", Kind: "github_pr"}}
		r.board.mu.Unlock()
		r.github.pulls["acme/widgets#70"] = mergeReadPull{branch: "legion/ACME-1", head: "h70", merge: "m70", merged: true, at: mergeReadStart}
		refresh(r)
		r.drain(5)
		if got := r.facts(); len(got) != 1 || got[0].Merge.Number != 70 {
			t.Fatalf("after the refresh the read posted %+v, want #70's merge", got)
		}
	})

	t.Run("a search with no answer sends nothing until a link changes", func(t *testing.T) {
		t.Parallel()
		r := newMergeReadRig(t)
		r.clearedClock(41)
		r.github.pulls["acme/widgets#41"] = mergeReadPull{branch: "legion/ACME-1", head: "h41"}
		r.queue()
		r.drain(10)
		if progress, _, _ := r.pending(); progress.State != "unread" {
			t.Fatalf("the search ended %+v, want it unread", progress)
		}
		twoDays(r)
		before, results := r.github.count(""), r.board.counted("results")
		refresh(r)
		r.drain(5)
		if r.board.counted("issue") != 2 || r.board.counted("results") != results || r.github.count("") != before {
			t.Fatalf("a refresh with the same links read the issue %d times in all, the results %d and GitHub %d more, want the links read once more and nothing else",
				r.board.counted("issue"), r.board.counted("results")-results, r.github.count("")-before)
		}
		r.board.mu.Lock()
		r.board.links = []dispatch.IssueLink{{URL: "https://github.com/acme/widgets/pull/80", Kind: "url"}}
		r.board.mu.Unlock()
		r.github.pulls["acme/widgets#80"] = mergeReadPull{branch: "legion/ACME-1", head: "h80", merge: "m80", merged: true, at: mergeReadStart}
		refresh(r)
		r.drain(5)
		if got := r.facts(); len(got) != 1 || got[0].Merge.Number != 80 || r.github.count("/pulls/80") != 1 {
			t.Fatalf("after a new link the read posted %+v, want #80's merge from one read of it", got)
		}
	})
}

// An issue whose record holds its merge (ANSWERED) takes no candidate read. Where the known merge
// lacks its final head and base, one ordinary read of its pull request gives them, and the posted
// fact carries the reviewed head, which is not the merge commit.
func TestAKnownMergeReadsMissingBranchMetadataOnce(t *testing.T) {
	t.Parallel()
	r := newMergeReadRig(t)
	merged := mergeReadStart.Add(-time.Hour)
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.records.PutMergeSnapshot(ctx, tx, mergeReadIssue, record.Merge{Number: 70, SHA: "m70", Repo: "acme/widgets", MergedAt: merged})
		return err
	}); err != nil {
		t.Fatalf("store the merge snapshot: %v", err)
	}
	r.clearedClock(41)
	r.github.pulls["acme/widgets#70"] = mergeReadPull{branch: "legion/ACME-1", head: "reviewed", merge: "m70", merged: true, at: merged}
	r.queue()
	r.drain(5)
	want := []intake.MergeRead{{Issue: mergeReadIssue, Generation: 3, Merge: record.Merge{Number: 70, Head: "reviewed", SHA: "m70", Base: "main",
		Repo: "acme/widgets", MergedAt: merged}}}
	if got := r.facts(); !slices.Equal(got, want) {
		t.Fatalf("the read posted %+v, want %+v", got, want)
	}
	if r.github.count("") != 1 || r.github.count("/pulls/70") != 1 || r.board.counted("issue") != 0 || r.board.counted("results") != 0 {
		t.Fatalf("the read sent %d GETs to GitHub, %d to Dispatch, want one read of #70 alone", r.github.count(""),
			r.board.counted("issue")+r.board.counted("results"))
	}
}
