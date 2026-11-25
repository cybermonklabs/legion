package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// sweptPull is one open pull request of acme/widgets as GitHub's GraphQL API lists it to the
// writers' sweep: its number, head branch and head commit, and every review of it, oldest first.
type sweptPull struct {
	number  int
	ref     string
	head    string
	reviews []sweptReview
}

// sweptReview is one review of a pull request: GitHub's id, which orders an author's reviews, the
// author's login as REST spells it (a bot's ends in [bot], which GraphQL leaves off and marks by
// its type), GraphQL's state (APPROVED, CHANGES_REQUESTED, COMMENTED) and the body.
type sweptReview struct {
	id     int64
	author string
	state  string
	body   string
}

// latestPerAuthor is each author's last review of reviews, of every state where opinionated is
// false, as GraphQL's latestReviews lists them, and of approvals and requests for changes alone
// where it is true, as latestOpinionatedReviews lists them.
func latestPerAuthor(reviews []sweptReview, opinionated bool) []sweptReview {
	var latest []sweptReview
	for _, review := range reviews {
		if opinionated && review.state != "APPROVED" && review.state != "CHANGES_REQUESTED" {
			continue
		}
		if at := slices.IndexFunc(latest, func(kept sweptReview) bool { return kept.author == review.author }); at >= 0 {
			latest[at] = review
		} else {
			latest = append(latest, review)
		}
	}
	return latest
}

// sweepGitHub is verdictGitHub with what the writers' sweep reads besides: the open pull requests of
// acme/widgets through the GraphQL API, one pull request to a page so the sweep must follow the
// pages, with each author's last review under whichever of latestReviews and
// latestOpinionatedReviews the query names, and only where the query asks for them; the open pull
// requests at a commit, from the same list (GET /commits/{sha}/pulls); and the merge queue's
// gh-readonly-queue/ refs, each naming the commit of its group. lists counts the GraphQL pages
// read, and requests every request GitHub saw.
type sweepGitHub struct {
	*verdictGitHub
	open     []sweptPull
	groups   map[string]string
	lists    int
	requests int
}

// serve answers what the sweep reads, and hands every other request to verdicts, the rig's own
// handler.
func (g *sweepGitHub) serve(t *testing.T, verdicts http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests++
		g.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			g.listOpen(t, w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/commits/") && strings.HasSuffix(r.URL.Path, "/pulls"):
			commit := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/commits/"), "/pulls")
			g.mu.Lock()
			defer g.mu.Unlock()
			var pulls []fakePull
			for _, pull := range g.open {
				pulls = append(pulls, fakePull{number: pull.number, ref: pull.ref, head: pull.head})
			}
			if err := json.NewEncoder(w).Encode(pullsAtCommit(commit, pulls)); err != nil {
				t.Errorf("encode the pull requests at %s: %v", commit, err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/matching-refs/heads/gh-readonly-queue/":
			if r.Header.Get("Authorization") != "Bearer "+readToken("acme") {
				t.Errorf("the merge queue's refs were listed with %q, want the read token", r.Header.Get("Authorization"))
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			refs := []map[string]any{}
			for ref, group := range g.groups {
				refs = append(refs, map[string]any{"ref": ref, "object": map[string]any{"sha": group, "type": "commit"}})
			}
			if err := json.NewEncoder(w).Encode(refs); err != nil {
				t.Errorf("encode the refs: %v", err)
			}
		default:
			verdicts(w, r)
		}
	}
}

// listOpen answers the sweep's GraphQL read of the open pull requests of acme/widgets, one to a
// page from the cursor after, as GitHub answers it: a bot's login without its [bot], and each
// author's last review only where the query's reviews variable asks for them, under the field the
// query names.
func (g *sweepGitHub) listOpen(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var sent struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		t.Errorf("decode the GraphQL query: %v", err)
	}
	if r.Header.Get("Authorization") != "Bearer "+readToken("acme") || sent.Variables["owner"] != "acme" || sent.Variables["name"] != "widgets" ||
		!strings.Contains(sent.Query, "states:OPEN") {
		t.Errorf("the GraphQL read = %q with %v under %q, want the open pull requests of acme/widgets with the read token",
			sent.Query, sent.Variables, r.Header.Get("Authorization"))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lists++
	at := 0
	if after, _ := sent.Variables["after"].(string); after != "" {
		at, _ = strconv.Atoi(after)
	}
	// GitHub's schema names a review's id fullDatabaseId, a string, and deprecates databaseId; a
	// query that still names databaseId is answered as GitHub answers one once the field is gone.
	if strings.Contains(sent.Query, "databaseId") {
		if err := json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{
			"message": "Field 'databaseId' doesn't exist on type 'PullRequestReview'"}}}); err != nil {
			t.Errorf("encode the GraphQL error: %v", err)
		}
		return
	}
	nodes := []map[string]any{}
	if at < len(g.open) {
		pull := g.open[at]
		node := map[string]any{"number": pull.number, "headRefName": pull.ref, "headRefOid": pull.head, "updatedAt": time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)}
		field := "latestReviews"
		if strings.Contains(sent.Query, "latestOpinionatedReviews") {
			field = "latestOpinionatedReviews"
		}
		if sent.Variables["reviews"] == true {
			reviews := []map[string]any{}
			for _, review := range latestPerAuthor(pull.reviews, field == "latestOpinionatedReviews") {
				author := map[string]any{"__typename": "User", "login": review.author}
				if login, bot := strings.CutSuffix(review.author, "[bot]"); bot {
					author = map[string]any{"__typename": "Bot", "login": login}
				}
				reviews = append(reviews, map[string]any{"fullDatabaseId": strconv.FormatInt(review.id, 10), "author": author, "state": review.state, "body": review.body,
					"submittedAt": time.Date(2026, 11, 21, 7, 0, int(review.id), 0, time.UTC)})
			}
			node[field] = map[string]any{"nodes": reviews}
		}
		nodes = append(nodes, node)
	}
	answer := map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": at+1 < len(g.open), "endCursor": strconv.Itoa(at + 1)}, "nodes": nodes}}}}
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		t.Errorf("encode the GraphQL answer: %v", err)
	}
}

// sweepRig is a verdictRig whose runtime reads and writes through a sweepGitHub listing open.
type sweepRig struct {
	*verdictRig
	sweep *sweepGitHub
}

// newSweepRig is newVerdictRig with its fake answering what the sweep reads (sweepGitHub), listing
// open as acme/widgets' open pull requests.
func newSweepRig(t *testing.T, tokens appauth.Tokens, entries []config.VerdictRepository, seed func(*testing.T, *store.Pool), open ...sweptPull) *sweepRig {
	t.Helper()
	return newSweepRigOfApp(t, verdictAppID, tokens, entries, seed, open...)
}

// newSweepRigOfApp is newSweepRig whose writer's configuration names appID as the one App's id.
func newSweepRigOfApp(t *testing.T, appID string, tokens appauth.Tokens, entries []config.VerdictRepository, seed func(*testing.T, *store.Pool),
	open ...sweptPull) *sweepRig {
	t.Helper()
	var sweep *sweepGitHub
	rig := newVerdictRigOfApp(t, appID, tokens, entries, seed, func(github *verdictGitHub, verdicts http.HandlerFunc) http.HandlerFunc {
		sweep = &sweepGitHub{verdictGitHub: github, open: open, groups: map[string]string{}}
		return sweep.serve(t, verdicts)
	})
	return &sweepRig{verdictRig: rig, sweep: sweep}
}

// swept runs one of the writers' sweep's passes after the one at boot, and waits for every write it
// queued.
func (rig *sweepRig) swept(t *testing.T) {
	t.Helper()
	rig.w.sweepVerdicts(context.Background(), false)
	rig.written(t)
}

// booted starts the sweep's ticker as the runtime's run starts it, until the test ends.
func (rig *sweepRig) booted(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.tickVerdictSweep(ctx)
}

// foreign adds runs and statuses to what GitHub holds, as another writer of the one App left them.
func (rig *sweepRig) foreign(runs []fakeRun, statuses []fakeStatus) {
	rig.github.mu.Lock()
	defer rig.github.mu.Unlock()
	rig.github.runs = append(rig.github.runs, runs...)
	rig.github.statuses = append(rig.github.statuses, statuses...)
}

// queued puts pull request number in the merge queue, in a group whose commit is group, under the
// branch GitHub names such a group by: gh-readonly-queue/main/pr-<number>-<base sha>.
func (g *sweepGitHub) queued(number int, group string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.groups["refs/heads/gh-readonly-queue/main/pr-"+strconv.Itoa(number)+"-7f60bbfb57cb2a1d0e9f8a7b6c5d4e3f2a1b0c9d"] = group
}

// counts is how many GraphQL pages of open pull requests GitHub answered, and how many requests it
// saw in all.
func (g *sweepGitHub) counts() (lists, requests int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lists, g.requests
}

// inStore runs fn in one transaction of the store, failing the test on its error: a seed writes
// rows of the verdict tables straight in, as an earlier run of the daemon recorded them, with no
// write asked for, and a check reads them back.
func inStore(t *testing.T, pool *store.Pool, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error { return fn(context.Background(), tx) }); err != nil {
		t.Fatalf("the store: %v", err)
	}
}

// recordedAt is the time the seeded rows carry.
var recordedAt = time.Date(2026, 11, 21, 6, 30, 0, 0, time.UTC)

// widgets is acme/widgets, the rigs' verdict repository.
var widgets = ghrepo.MustParse("acme/widgets")

// seedReviewerVerdict records CAPTURE-1's reviewer's verdict at code, as the reviewer's completion
// recorded it before the daemon stopped, on the plan implementer tester reviewer whose last step is
// the reviewer's and whose every other step passed at code (reviewerLastAtCode): the verdict of
// its review step, step 3 of its task a.
func seedReviewerVerdict(word verdict.Word) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		reviewerLastAtCode(t, pool)
		seedStepVerdict(t, pool, "a", 3, "code", word, captureReviewer)
	}
}

// seedOutside records the outside pull request 90 at out-1, with the CI reviewer's approval at
// out-1 when approved, as webhooks recorded them before the daemon stopped.
func seedOutside(approved bool) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := store.RecordOutsidePullRequestTx(ctx, tx, store.OutsidePullRequest{Repository: widgets, Number: 90, Head: "out-1", RecordedAt: recordedAt}); err != nil || !approved {
				return err
			}
			_, err := store.RecordVerdictReviewTx(ctx, tx, store.VerdictReview{Repository: widgets, Number: 90, GitHubID: 1, Head: "out-1",
				Author: ciReviewer, State: verdict.StateApproved, SubmittedAt: recordedAt, RecordedAt: recordedAt})
			return err
		})
	}
}

// The open pull requests the sweep's rigs list: CAPTURE-1's pull request 86 at code, and the
// outside pull request 90 at out-1.
var (
	openCapture = sweptPull{number: 86, ref: "legion/CAPTURE-1", head: "code"}
	openOutside = sweptPull{number: 90, ref: "feature/faster-builds", head: "out-1"}
)

// ciReviewBody is a CI reviewer's review body, ending with the line that names the head it read.
func ciReviewBody(head string) string {
	return "No blocking findings.\n\n<!-- ci-review-head " + head + " -->\n"
}

// C2's sweep cases: at boot and every 10 minutes the sweep lists the open pull requests of a verdict
// repository whose entry names a ci_reviewer, with each author's last approval or request for
// changes, and counts the CI reviewer's listed review only where its body's ci-review-head line
// names the pull request's current head: an approval there writes success and a request for
// changes failure, and a comment the CI reviewer left after it hides neither. A listed review
// naming another commit or none writes nothing, and so does a person's listed approval, which
// GitHub lists at the current head whatever head it read. A comment decides nothing.
func TestLegionVerdictSweepCountsTheCIReviewersListedReviewOfTheCurrentHeadAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reviews []sweptReview
		want    []string
	}{
		{"the CI reviewer's approval naming the current head", []sweptReview{{1, ciReviewer, "APPROVED", ciReviewBody("out-1")}}, []string{"success"}},
		{"the CI reviewer's request for changes naming the current head", []sweptReview{{1, ciReviewer, "CHANGES_REQUESTED", ciReviewBody("out-1")}}, []string{"failure"}},
		{"the CI reviewer's approval, then its comment", []sweptReview{{1, ciReviewer, "APPROVED", ciReviewBody("out-1")}, {3, ciReviewer, "COMMENTED", "One question."}}, []string{"success"}},
		{"the CI reviewer's approval naming another commit", []sweptReview{{1, ciReviewer, "APPROVED", ciReviewBody("out-0")}}, nil},
		{"the CI reviewer's approval naming no commit", []sweptReview{{1, ciReviewer, "APPROVED", "No blocking findings."}}, nil},
		{"the CI reviewer's comment naming the current head", []sweptReview{{1, ciReviewer, "COMMENTED", ciReviewBody("out-1")}}, nil},
		{"a person's listed approval", []sweptReview{{2, "a-writer", "APPROVED", ciReviewBody("out-1")}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := openOutside
			pull.reviews = tc.reviews
			rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded, pull)
			rig.swept(t)
			rig.swept(t)
			var concluded []string
			for _, run := range rig.github.legionRuns("out-1") {
				concluded = append(concluded, run.Conclusion)
			}
			if !slices.Equal(concluded, tc.want) {
				t.Fatalf("legion-verdict on out-1 concluded %v after two sweeps, want %v", concluded, tc.want)
			}
			reviews := verdictReviews(t, rig.pool, 90)
			if tc.want == nil {
				if len(reviews) != 0 {
					t.Fatalf("the reviews recorded of pull request 90 = %+v, want none", reviews)
				}
				return
			}
			if len(reviews) != 1 || reviews[0].Head != "out-1" || reviews[0].Author != ciReviewer || reviews[0].GitHubID != 1 || reviews[0].AuthorCanWrite {
				t.Fatalf("the reviews recorded of pull request 90 after two sweeps = %+v, want the CI reviewer's once, at out-1", reviews)
			}
		})
	}
}

// The sweep reads the open pull requests a page at a time, following GitHub's cursor, and records an
// open pull request no record holds, on a branch that is no issue's, at the head GitHub lists, so
// its write reads nothing more of GitHub; a head that moved since the record's is recorded again.
func TestLegionVerdictSweepRecordsEachOpenOutsidePullRequestAtTheHeadGitHubLists(t *testing.T) {
	moved := sweptPull{number: 92, ref: "feature/faster-tests", head: "out-9"}
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
		inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.RecordOutsidePullRequestTx(ctx, tx, store.OutsidePullRequest{Repository: widgets, Number: 92, Head: "out-8", RecordedAt: recordedAt})
			return err
		})
	}, openOutside, moved)
	rig.swept(t)
	if lists, _ := rig.sweep.counts(); lists != 2 {
		t.Fatalf("the sweep read %d pages of open pull requests, want 2: one to a page, following the cursor", lists)
	}
	for number, head := range map[int]string{90: "out-1", 92: "out-9"} {
		var sighted store.OutsidePullRequest
		var found bool
		inStore(t, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			sighted, found, err = store.OutsidePullRequestTx(ctx, tx, widgets, number)
			return err
		})
		if !found || sighted.Head != head {
			t.Fatalf("the outside pull request %d reads %+v (found %v), want it at %s", number, sighted, found, head)
		}
	}
}

// A write that failed - here GitHub's 502 to the create after the reviewer's approve - leaves the
// head with a verdict the record holds and no run of this daemon's. The sweep writes it: the
// success run appears, and the round, which GitHub's `blocked` held while the head had no run,
// settles as approved on the merge-state read a success asks for.
func TestLegionVerdictSweepWritesAHeldVerdictAFailedWriteLeftUnwritten(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewerLastAtCode, openCapture)
	rig.github.mu.Lock()
	rig.github.failCreates = 1
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	if runs := rig.github.legionRuns("code"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on code = %+v, want none after GitHub failed the create", runs)
	}
	rig.swept(t)
	runs := rig.github.legionRuns("code")
	if len(runs) != 1 || runs[0].Conclusion != "success" || runs[0].App != verdictAppSlug {
		t.Fatalf("legion-verdict runs on code after the sweep = %+v, want the daemon's success", runs)
	}
	if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 1 || recorded[0].GitHubID != runs[0].ID || recorded[0].ForeignID != 0 {
		t.Fatalf("the store records %+v, want the sweep's run %d as the daemon's own", recorded, runs[0].ID)
	}
	testwait.Eventually(t, "the round to settle as approved once the sweep's success stands", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Retro
	})
}

// A sweep queues a write on every open head, and a fact's write queued behind a long batch of them
// is made first: the fact's issue holds its merge-state reads on that write (verdictWriter.await),
// so a write behind the sweep's would hold them for the writer's whole bound. The sweep's writes
// then go on in the order they were queued, and a fact queued while they run goes before the rest.
func TestLegionVerdictAFactsWriteGoesBeforeTheSweepsWrites(t *testing.T) {
	writer := newVerdictWriter([]config.VerdictRepository{widgetsEntry}, verdictAppID, slog.New(slog.DiscardHandler))
	for n := 1; n <= 50; n++ {
		writer.add(verdictTarget{repository: widgets, number: 100 + n, head: fmt.Sprintf("swept-%d", n), overForeign: true})
	}
	writer.add(verdictTarget{issue: "CAPTURE-1", repository: widgets, number: 86, head: "code"})
	var order []string
	take := func() {
		t.Helper()
		target, ok := writer.next()
		if !ok {
			t.Fatalf("the writer took nothing after %v", order)
		}
		order = append(order, target.head)
		writer.done(target)
	}
	take()
	take()
	writer.add(verdictTarget{issue: "CAPTURE-2", repository: widgets, number: 87, head: "code-87"})
	take()
	take()
	if want := []string{"code", "swept-1", "code-87", "swept-2"}; !slices.Equal(order, want) {
		t.Fatalf("the writer made its writes in the order %v, want %v: each fact's before the sweep's that wait", order, want)
	}
}

// A 10-minute pass of the sweep over a Legion head whose success already stands, as the writer
// wrote it, writes nothing and asks no more merge-state reads: the writer asks one more read only
// after a pass that wrote a conclusion or changed one, for GitHub to count the run it wrote, and
// only the pass at boot asks one of a standing success. The pass still reads what R19 asks of the
// head, the head's legion-verdict runs and its commit statuses, and what deciding the head reads:
// the open pull requests at the head, for the branch its change is compared with, and the change's
// commits, for the Omp-Session test of every step passed there.
func TestLegionVerdictSweepAsksNoMergeStateReadOfAnUnchangedSuccessHead(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, acceptingAtCode, openCapture)
	rig.acceptedAndWritten(t)
	reads := func() int {
		_, events := rig.github.recorded()
		return len(slices.DeleteFunc(events, func(e string) bool { return !strings.HasPrefix(e, "read pull request 86") }))
	}
	// quiet waits until GitHub has seen no read of pull request 86 for 50 of the rig's waits between
	// reads, and answers how many reads it has seen and how many requests in all.
	quiet := func() (int, int) {
		t.Helper()
		for {
			n := reads()
			if !testwait.Holds(50*readRigWait, readRigWait, func() bool { return reads() > n }) {
				_, requests := rig.sweep.counts()
				return n, requests
			}
		}
	}
	readsBefore, requestsBefore := quiet()
	rig.swept(t)
	readsAfter, requestsAfter := quiet()
	if readsAfter != readsBefore {
		t.Fatalf("the sweep over an unchanged success head made %d more reads of pull request 86, want none", readsAfter-readsBefore)
	}
	if made := requestsAfter - requestsBefore; made != 6 {
		t.Fatalf("the sweep made %d requests, want 6: its page of open pull requests, the merge queue's refs, the pull requests at the head, the change's commits, and the head's runs and statuses", made)
	}
	if writes, _ := rig.github.recorded(); len(writes) != 1 {
		t.Fatalf("writes = %+v, want only the acceptance's", writes)
	}
}

// C56: the sweep writes the daemon's own conclusion over each legion-verdict run and status of the
// one App at an open head that its record does not hold: a run is updated to failure where no
// verdict stands and to success where R4's verdict stands, with no run of its own beside it; a
// status the App's bot created gets a newer status of the daemon's own with that state. Each such
// write is in the record and in a legion_verdict_write line naming the foreign id. A run the record
// holds is left as it is, and so is a run or a status another account made. The same holds on a
// merge group's commit, and on a legion/<KEY> head of another project's issue, where this daemon
// holds no step of that issue, so no verdict stands (R4).
func TestTheSweepWritesOverAForeignLegionVerdict(t *testing.T) {
	foreignWrite := func(t *testing.T, rig *sweepRig, head string, kind store.VerdictWriteKind, foreign int64, conclusion string) {
		t.Helper()
		recorded := verdictWrites(t, rig.pool, head)
		if len(recorded) != 1 || recorded[0].Kind != kind || recorded[0].ForeignID != foreign || string(recorded[0].Conclusion) != conclusion {
			t.Fatalf("the store records %+v on %s, want one %s written over %d, concluding %s", recorded, head, kind, foreign, conclusion)
		}
		lines := rig.writeLines(t)
		if len(lines) != 1 || lines[0]["head"] != head || lines[0]["foreign_id"] != float64(foreign) || lines[0]["kind"] != string(kind) ||
			lines[0]["conclusion"] != conclusion {
			t.Fatalf("legion_verdict_write lines = %v, want one naming %s and the foreign id %d", lines, head, foreign)
		}
	}
	t.Run("a run at a head no verdict stands at turns to failure", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedOutside(false), openOutside)
		rig.foreign([]fakeRun{
			{ID: 500, Head: "out-1", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug},
			{ID: 501, Head: "out-1", Name: verdict.CheckName, Conclusion: "success", App: "github-actions"},
		}, nil)
		rig.swept(t)
		runs := rig.github.legionRuns("out-1")
		if len(runs) != 2 || runs[0].ID != 500 || runs[0].Conclusion != "failure" || runs[1].ID != 501 || runs[1].Conclusion != "success" {
			t.Fatalf("legion-verdict runs on out-1 = %+v, want the App's run 500 at failure, another App's left alone, and no new run", runs)
		}
		foreignWrite(t, rig, "out-1", store.VerdictWriteCheckRun, 500, "failure")
	})
	t.Run("a run at a head R4's verdict stands at turns to success", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedOutside(true), openOutside)
		rig.foreign([]fakeRun{{ID: 500, Head: "out-1", Name: verdict.CheckName, Conclusion: "failure", App: verdictAppSlug}}, nil)
		rig.swept(t)
		if runs := rig.github.legionRuns("out-1"); len(runs) != 1 || runs[0].ID != 500 || runs[0].Conclusion != "success" {
			t.Fatalf("legion-verdict runs on out-1 = %+v, want the App's run 500 at success and no run beside it", runs)
		}
		foreignWrite(t, rig, "out-1", store.VerdictWriteCheckRun, 500, "success")
	})
	t.Run("a status of the App's bot gets a newer status of the daemon's own", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedOutside(false), openOutside)
		rig.foreign(nil, []fakeStatus{
			{ID: 600, Head: "out-1", Context: verdict.CheckName, State: "success", Creator: verdictAppLogin},
			{ID: 601, Head: "out-1", Context: verdict.CheckName, State: "success", Creator: "a-writer"},
		})
		rig.swept(t)
		rig.github.mu.Lock()
		statuses := slices.Clone(rig.github.statuses)
		rig.github.mu.Unlock()
		if len(statuses) != 3 || statuses[2].State != "failure" || statuses[2].Creator != verdictAppLogin || statuses[2].Context != verdict.CheckName {
			t.Fatalf("statuses on out-1 = %+v, want a newer legion-verdict status of the daemon's own at failure over the App's, and the person's left alone", statuses)
		}
		foreignWrite(t, rig, "out-1", store.VerdictWriteStatus, 600, "failure")
	})
	t.Run("a run the record holds is left as it is", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
			seedOutside(true)(t, pool)
			inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := store.RecordVerdictWriteTx(ctx, tx, store.VerdictWrite{Repository: widgets, Head: "out-1", Kind: store.VerdictWriteCheckRun,
					GitHubID: 700, Conclusion: store.VerdictConclusionSuccess, WrittenAt: recordedAt})
				return err
			})
		}, openOutside)
		rig.foreign([]fakeRun{{ID: 700, Head: "out-1", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug}}, nil)
		rig.swept(t)
		if writes, _ := rig.github.recorded(); len(writes) != 0 {
			t.Fatalf("writes = %+v, want none over the run the record holds", writes)
		}
	})
	t.Run("a run on a merge group's commit turns to the group's conclusion", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewingWithPass, openCapture)
		rig.sweep.queued(86, "group-86")
		rig.foreign([]fakeRun{{ID: 800, Head: "group-86", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug}}, nil)
		rig.swept(t)
		if runs := rig.github.legionRuns("group-86"); len(runs) != 1 || runs[0].ID != 800 || runs[0].Conclusion != "failure" {
			t.Fatalf("legion-verdict runs on group-86 = %+v, want the App's run 800 at failure, since no verdict stands at pull request 86's head", runs)
		}
		foreignWrite(t, rig, "group-86", store.VerdictWriteCheckRun, 800, "failure")
	})
	t.Run("a run on another project's issue head turns to failure", func(t *testing.T) {
		rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded,
			sweptPull{number: 91, ref: "legion/OTHER-1", head: "other-1"})
		rig.foreign([]fakeRun{{ID: 900, Head: "other-1", Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug}}, nil)
		rig.swept(t)
		if runs := rig.github.legionRuns("other-1"); len(runs) != 1 || runs[0].ID != 900 || runs[0].Conclusion != "failure" {
			t.Fatalf("legion-verdict runs on other-1 = %+v, want the App's run 900 at failure, since no verdict stands on another project's issue head", runs)
		}
		foreignWrite(t, rig, "other-1", store.VerdictWriteCheckRun, 900, "failure")
	})
}

// B2 (R19): the sweep compares each run of the one App, as GitHub shows it now, with what the head
// should show, the runs its record holds included, and updates the run where the two differ. A
// token of the App that turns a run back to success, on a head where no verdict stands, sees it
// turned to failure again at the next sweep: a run the sweep once wrote failure over, and the
// daemon's own run, which it wrote success on and then failure once the verdict no longer stood.
// Once GitHub shows the run at failure, the next sweep writes nothing more.
func TestTheSweepWritesOverARecordedRunAnotherTokenTurnedBack(t *testing.T) {
	recorded := func(id int64, foreign int64, conclusions ...store.VerdictConclusion) func(string) func(*testing.T, *store.Pool) {
		return func(head string) func(*testing.T, *store.Pool) {
			return func(t *testing.T, pool *store.Pool) {
				t.Helper()
				inStore(t, pool, func(ctx context.Context, tx pgx.Tx) error {
					for _, conclusion := range conclusions {
						if _, err := store.RecordVerdictWriteTx(ctx, tx, store.VerdictWrite{Repository: widgets, Head: head, Kind: store.VerdictWriteCheckRun,
							GitHubID: id, Conclusion: conclusion, ForeignID: foreign, WrittenAt: recordedAt}); err != nil {
							return err
						}
					}
					return nil
				})
			}
		}
	}
	for _, tc := range []struct {
		name   string
		seed   func(*testing.T, *store.Pool)
		writes func(string) func(*testing.T, *store.Pool)
		open   sweptPull
		id     int64
	}{
		{"a run the sweep wrote failure over, at an outside head", seedOutside(false), recorded(500, 500, store.VerdictConclusionFailure), openOutside, 500},
		{"the daemon's own run, at a Legion head whose verdict no longer stands", reviewingWithPass,
			recorded(900, 0, store.VerdictConclusionSuccess, store.VerdictConclusionFailure), openCapture, 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.open.head
			rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, func(t *testing.T, pool *store.Pool) {
				tc.seed(t, pool)
				tc.writes(head)(t, pool)
			}, tc.open)
			rig.foreign([]fakeRun{{ID: tc.id, Head: head, Name: verdict.CheckName, Conclusion: "success", App: verdictAppSlug}}, nil)
			rig.swept(t)
			if runs := rig.github.legionRuns(head); len(runs) != 1 || runs[0].ID != tc.id || runs[0].Conclusion != "failure" {
				t.Fatalf("legion-verdict runs on %s = %+v, want run %d turned back to failure, and no run beside it", head, runs, tc.id)
			}
			writes := verdictWrites(t, rig.pool, head)
			if last := writes[len(writes)-1]; last.GitHubID != tc.id || last.Conclusion != store.VerdictConclusionFailure || last.ForeignID != 0 {
				t.Fatalf("the store records %+v on %s, want run %d written at failure last, as a run of its own", writes, head, tc.id)
			}
			if lines := rig.writeLines(t); len(lines) != 1 || lines[0]["id"] != float64(tc.id) || lines[0]["conclusion"] != "failure" {
				t.Fatalf("legion_verdict_write lines = %v, want one naming run %d at failure", lines, tc.id)
			}
			made, _ := rig.github.recorded()
			rig.swept(t)
			if again, _ := rig.github.recorded(); len(again) != len(made) {
				t.Fatalf("the next sweep wrote %+v, want nothing: GitHub shows run %d at failure", again[len(made):], tc.id)
			}
		})
	}
}

// N5: the sweep reads a listed review's id from fullDatabaseId, the string GitHub's schema gives in
// place of the deprecated databaseId, parsed whole: the CI reviewer's approval whose id is past
// 2^32 is recorded under that id, and writes success.
func TestLegionVerdictSweepReadsAReviewsFullDatabaseID(t *testing.T) {
	pull := openOutside
	pull.reviews = []sweptReview{{6009805204, ciReviewer, "APPROVED", ciReviewBody("out-1")}}
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded, pull)
	rig.swept(t)
	if reviews := verdictReviews(t, rig.pool, 90); len(reviews) != 1 || reviews[0].GitHubID != 6009805204 {
		t.Fatalf("the reviews recorded of pull request 90 = %+v, want the CI reviewer's under GitHub's id 6009805204", reviews)
	}
	if runs := rig.github.legionRuns("out-1"); len(runs) != 1 || runs[0].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on out-1 = %+v, want one concluding success", runs)
	}
}

// unnamedGroupLine is what the sweep logs of a merge queue's group branch whose name gives no pull
// request.
const unnamedGroupLine = "verdict: a merge queue's group branch names no pull request, so legion-verdict is not written on its commit"

// N6: a gh-readonly-queue/ branch whose name is not a group's of a pull request (as GitHub names it
// today, gh-readonly-queue/<base>/pr-<number>-<base sha>) gives the sweep no pull request to
// decide, so it writes nothing on its commit, and says so, naming the branch and its commit, each
// pass; a branch of the usual shape is written on.
func TestLegionVerdictSweepLogsAGroupBranchThatNamesNoPullRequest(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedReviewerVerdict(verdict.ReviewerApprove), openCapture)
	rig.sweep.queued(86, "group-86")
	rig.sweep.mu.Lock()
	rig.sweep.groups["refs/heads/gh-readonly-queue/main/entry-86-7f60bbfb57cb2a1d0e9f8a7b6c5d4e3f2a1b0c9d"] = "group-odd"
	rig.sweep.mu.Unlock()
	rig.swept(t)
	if runs := rig.github.legionRuns("group-odd"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on group-odd = %+v, want none: its branch names no pull request", runs)
	}
	groupConcluded(t, rig.github, rig.pool, "group-86", "success")
	lines := loggedMessages(t, rig.logs.String(), unnamedGroupLine)
	if len(lines) != 1 || lines[0]["ref"] != "refs/heads/gh-readonly-queue/main/entry-86-7f60bbfb57cb2a1d0e9f8a7b6c5d4e3f2a1b0c9d" ||
		lines[0]["group"] != "group-odd" || lines[0]["repository"] != "acme/widgets" {
		t.Fatalf("%q lines = %v, want one naming the branch, its commit and acme/widgets", unnamedGroupLine, lines)
	}
}

// Review Focus 4: a restart between the check run and its record leaves a run of the daemon's own
// that its record does not hold. The boot sweep writes its own conclusion over that run, and that
// conclusion is verdict.Decide's at the head, whichever way the run had concluded: the run becomes
// one the record holds, and no run is made beside it.
func TestTheBootSweepTakesItsOwnUnrecordedRunToTheRightConclusion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		word     verdict.Word
		left     string
		decision verdict.Conclusion
	}{
		{"a success the reviewer's changes no longer back", verdict.ReviewerChanges, "success", verdict.Failure},
		{"a failure the reviewer's approve no longer backs", verdict.ReviewerApprove, "failure", verdict.Success},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedReviewerVerdict(tc.word), openCapture)
			rig.foreign([]fakeRun{{ID: 1001, Head: "code", Name: verdict.CheckName, Conclusion: tc.left, App: verdictAppSlug}}, nil)
			decided, err := rig.w.decideVerdict(context.Background(), widgetsEntry, widgets, 86, "code")
			if err != nil || decided.conclusion != tc.decision {
				t.Fatalf("verdict.Decide at code = %q, %v; want %q", decided.conclusion, err, tc.decision)
			}
			rig.booted(t)
			testwait.Eventually(t, "the boot sweep to write over the run its record does not hold", func() bool {
				return len(verdictWrites(t, rig.pool, "code")) == 1
			})
			rig.written(t)
			if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].ID != 1001 || runs[0].Conclusion != string(decided.conclusion) {
				t.Fatalf("legion-verdict runs on code = %+v, want run 1001 alone at %s, verdict.Decide's", runs, decided.conclusion)
			}
			if recorded := verdictWrites(t, rig.pool, "code"); len(recorded) != 1 || recorded[0].GitHubID != 1001 || recorded[0].ForeignID != 1001 ||
				recorded[0].Conclusion != decided.conclusion {
				t.Fatalf("the store records %+v, want run 1001 written over at %s, so the record holds it", recorded, decided.conclusion)
			}
		})
	}
}

// C4's restart case: a merge group of pull request 86 stands in the queue with no legion-verdict
// run on its commit, as after a restart lost the write its event asked for, while the record holds
// the reviewer's approve at the pull request's head. The sweep the runtime runs at boot writes the
// missing check on the gh-readonly-queue/ branch's commit, without waiting out its 10 minutes.
func TestLegionVerdictMergeGroupAfterARestartTheBootSweepWritesTheMissingCheck(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, seedReviewerVerdict(verdict.ReviewerApprove), openCapture)
	rig.sweep.queued(86, "group-86")
	rig.booted(t)
	testwait.Eventually(t, "the boot sweep to write legion-verdict on the merge group's commit", func() bool {
		return len(rig.github.legionRuns("group-86")) > 0
	})
	rig.written(t)
	groupConcluded(t, rig.github, rig.pool, "group-86", "success")
}

// A restart lost the merge-state read the writer asks for after a success (writeVerdictOver's
// timer, held in memory): the reviewer's approve, the last step of its plan, wrote success on
// code, GitHub answered both reads after it with a stale `blocked`, and the round still waits, with
// nothing left in memory to read again. The rig stands for the daemon booted again over that
// record, which holds no more than a new runtime would. The boot pass asks for a read of each issue
// whose success it finds standing, although it writes nothing there, and GitHub's clean answer
// settles the round as approved.
func TestTheBootSweepReadsTheMergeStateOfAStandingSuccessARestartLeftUnsettled(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, reviewerLastAtCode, openCapture)
	rig.github.mu.Lock()
	rig.github.staleReads = 2
	rig.github.mu.Unlock()
	rig.apply(t, reviewerVerdict("approve", "code", 1))
	rig.written(t)
	testwait.Eventually(t, "GitHub to answer both reads after the write with a stale blocked", func() bool {
		_, events := rig.github.recorded()
		return len(slices.DeleteFunc(events, func(e string) bool { return e != "read pull request 86: blocked" })) == 2
	})
	if got, _ := capturePhase(t, rig.pool); got != phase.Reviewing {
		t.Fatalf("the issue is in %s before the restart, want reviewing: the stale blocked answers hold the round", got)
	}
	rig.booted(t)
	testwait.Eventually(t, "the boot sweep's read to settle the round as approved", func() bool {
		got, _ := capturePhase(t, rig.pool)
		return got == phase.Retro
	})
	if runs := rig.github.legionRuns("code"); len(runs) != 1 || runs[0].Conclusion != "success" {
		t.Fatalf("legion-verdict runs on code = %+v, want the approve's success alone", runs)
	}
}

// N7 (R4, R19): a legion/<KEY> head of another project in the open list is swept as every head of
// a verdict repository is. This daemon holds no step of that issue, so no verdict stands there: with
// no run or status of the App on the head it writes nothing there, the group of that pull request in
// the merge queue gets failure, and one line per pull request says why no verdict stands.
func TestLegionVerdictSweepWritesNoSuccessOnAnotherProjectsIssueHead(t *testing.T) {
	rig := newSweepRig(t, roleTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded,
		sweptPull{number: 91, ref: "legion/OTHER-1", head: "other-1"})
	rig.sweep.queued(91, "group-91")
	rig.swept(t)
	rig.swept(t)
	if runs := rig.github.legionRuns("other-1"); len(runs) != 0 {
		t.Fatalf("legion-verdict runs on other-1 = %+v, want none: no verdict stands there, and no run of the App is there to write over", runs)
	}
	groupConcluded(t, rig.github, rig.pool, "group-91", "failure")
	lines := loggedMessages(t, rig.logs.String(), otherProjectsLine)
	if len(lines) != 1 || lines[0]["pull_request"] != float64(91) || lines[0]["issue"] != "OTHER-1" {
		t.Fatalf("lines = %v, want one naming pull request 91 and OTHER-1", lines)
	}
}

// A daemon in token mode mints no verdict token, so its sweep reads nothing of GitHub, whose reads
// could lead to no write, and the writer's line says why once.
func TestLegionVerdictSweepInTokenModeReadsNothing(t *testing.T) {
	rig := newSweepRig(t, tokenModeTokens{}, []config.VerdictRepository{widgetsEntry}, nothingSeeded, openOutside)
	rig.swept(t)
	rig.swept(t)
	if _, requests := rig.sweep.counts(); requests != 0 {
		t.Fatalf("GitHub saw %d requests in token mode, want none", requests)
	}
	if lines := loggedMessages(t, rig.logs.String(), tokenModeLine); len(lines) != 1 {
		t.Fatalf("token-mode lines = %v, want one", lines)
	}
}
