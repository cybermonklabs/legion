package delivery

// What a `pull_request` envelope may write: GitHub's own answer about the pull request it names,
// and nothing the envelope itself carries. With no delivery settings, as the deployment that
// change was made for holds none.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// githubPulls is what the fake GitHub answers about each pull request of one repository: `GET
// /repos/{owner}/{repo}/pulls/{number}` from the answer a row last set, and 404 for a number it
// was never given, as GitHub answers a pull request it does not have. Each pull request's commits
// are the messages a test gave it (messages), else an empty page, and 500 for each read while
// commitsFailing is set. `GET /repos/{owner}/{repo}` answers the repository, as GitHub answers an
// installation that can read it. The absence query of GraphQL (absenceQuery, answerAbsenceQuery)
// answers the repository and the pull request, or GitHub's NOT_FOUND for a number it was never
// given. It counts the four reads, so a row can show what an envelope cost. limited answers every
// pull request read with a rate limit, and failing with a 500. hidden is the installation losing
// the repository: the token it minted before then is answered 404 for the repository and its pull
// requests, and the absence query a null repository, as GitHub answers a repository a token does
// not read. holdFirst, where a test sets it before any read, runs at the first pull request read
// after its answer is taken and before it is sent: a test moves GitHub on there, or holds the
// answer back. before, where a test sets it before any read, runs at each read of one number
// before its answer is taken. The fake answers the batched query of facts (readFactsBatch) from
// the same state, before the mux sees it (fakeGitHub.answerFactsQuery). factsReads counts each
// pull request that the query answers, and reads and commitReads do not. holdFirst and before do
// not run for it. A REST read of the rest of the messages after the query is a read of the
// commits, and commitReads counts it.
type githubPulls struct {
	mu             sync.Mutex
	pulls          map[int]map[string]any
	messages       map[int][]string
	limited        bool
	failing        bool
	commitsFailing bool
	hidden         bool
	holdFirst      func()
	before         map[int]func()
	reads          atomic.Int32
	commitReads    atomic.Int32
	repoReads      atomic.Int32
	absenceQueries atomic.Int32
	factsReads     atomic.Int32
}

func serveGitHubPulls(t *testing.T, fake *fakeGitHub, repo string) *githubPulls {
	t.Helper()
	g := &githubPulls{pulls: map[int]map[string]any{}}
	fake.served.Store(repo, g)
	fake.absenceRoute.Do(func() { fake.handle("POST /graphql", fake.answerAbsenceQuery) })
	fake.handle("GET /repos/"+repo+"/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		read := g.reads.Add(1)
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("a pull request read of %s: %v", r.URL.Path, err)
		}
		g.mu.Lock()
		before := g.before[number]
		g.mu.Unlock()
		if before != nil {
			before()
		}
		g.mu.Lock()
		pull, known := g.pulls[number]
		limited, failing, hidden, hold := g.limited, g.failing, g.hidden, g.holdFirst
		g.mu.Unlock()
		if read == 1 && hold != nil {
			hold()
		}
		switch {
		case limited:
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		case failing:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Server Error"}`)
		case !known, hidden:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		default:
			mustEncode(t, w, pull)
		}
	})
	fake.handle("GET /repos/"+repo+"/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		g.commitReads.Add(1)
		number, _ := strconv.Atoi(r.PathValue("number"))
		g.mu.Lock()
		messages, failing := g.messages[number], g.commitsFailing
		g.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		commits := make([]map[string]any, len(messages))
		for i, message := range messages {
			commits[i] = map[string]any{"sha": fmt.Sprintf("%040d", i+1), "commit": map[string]any{"message": message}}
		}
		mustEncode(t, w, commits)
	})
	fake.handle("GET /repos/"+repo, func(w http.ResponseWriter, _ *http.Request) {
		// The answer is taken before the read is counted, so a test that waits for the count
		// and then changes GitHub changes no answer already given.
		g.mu.Lock()
		hidden := g.hidden
		g.mu.Unlock()
		g.repoReads.Add(1)
		if hidden {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		mustEncode(t, w, map[string]any{"full_name": repo, "id": 1, "default_branch": "main"})
	})
	return g
}

// answerAbsenceQuery answers POST /graphql for the one query the reconcile asks of GraphQL about
// a refused read (absenceQuery), in GitHub's own shapes (pullRequestAbsent's doc comment): the
// count of the pull requests a repository serveGitHubPulls serves, beside the pull request it
// holds or GitHub's NOT_FOUND on the path repository.pullRequest for a number it does not hold,
// and a null repository with NOT_FOUND on the path repository for one it does not serve, one the
// App is not installed on (notInstalled), and one the installation lost (hidden). Each answer is
// taken before it is counted (absenceQueries). The fake answers the batched query of facts before
// its mux sees it (fakeGitHub.serveHTTP), so any other query is a test's mistake.
func (f *fakeGitHub) answerAbsenceQuery(w http.ResponseWriter, r *http.Request) {
	query, variables := decodeGraphQLRequest(f.t, r)
	if !strings.Contains(query, "pullRequest(number: $number)") {
		f.t.Errorf("POST /graphql asked %q; this fake answers only the absence query", query)
		http.Error(w, `{"message":"unexpected query"}`, http.StatusBadRequest)
		return
	}
	owner, _ := variables["owner"].(string)
	name, _ := variables["name"].(string)
	number, _ := variables["number"].(float64)
	repo := owner + "/" + name
	served, ok := f.served.Load(repo)
	if !ok {
		mustEncode(f.t, w, nullRepositoryAnswer(repo))
		return
	}
	g := served.(*githubPulls)
	_, notInstalled := f.notInstalled.Load(repo)
	g.mu.Lock()
	pull, known := g.pulls[int(number)]
	counted := map[string]any{"totalCount": len(g.pulls)}
	unread := g.hidden || notInstalled
	g.mu.Unlock()
	defer g.absenceQueries.Add(1)
	switch {
	case unread:
		mustEncode(f.t, w, nullRepositoryAnswer(repo))
	case known:
		mustEncode(f.t, w, map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequests": counted, "pullRequest": map[string]any{"number": pull["number"]}}}})
	default:
		mustEncode(f.t, w, map[string]any{
			"data": map[string]any{"repository": map[string]any{"pullRequests": counted, "pullRequest": nil}},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository", "pullRequest"},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", int(number))}},
		})
	}
}

// nullRepositoryAnswer is GitHub's answer to the absence query about a repository the token does
// not read, or one that does not exist.
func nullRepositoryAnswer(repo string) map[string]any {
	return map[string]any{
		"data": map[string]any{"repository": nil},
		"errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository"},
			"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repo)}},
	}
}

// set makes GitHub hold pull request number in repo at head, in state, with that branch and body
// and that updated_at.
func (g *githubPulls) set(repo string, number int, state, head, branch, body string, updatedAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = map[string]any{
		"number": number, "title": fmt.Sprintf("Change %d", number), "body": body, "state": state,
		"merged": false, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"updated_at": updatedAt.UTC().Format(time.RFC3339),
		"head":       map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": repo}},
		"base":       map[string]any{"ref": "main"},
	}
}

// prEnvelope is a `pull_request` envelope's payload as Envoy's normalizer builds it.
func prEnvelope(repo string, number int, action, head, branch, body string, updatedAt time.Time) map[string]string {
	return map[string]string{
		"kind": "pr", "action": action, "repo": repo, "number": strconv.Itoa(number),
		"title": fmt.Sprintf("Change %d", number), "body": body, "head_ref": branch, "head_repo": repo,
		"head_sha": head, "base_ref": "main", "merged": "false",
		"url":        fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"updated_at": updatedAt.UTC().Format(time.RFC3339),
	}
}

func headRowCount(t *testing.T, ctx context.Context, intake *Intake, repo string, number int) int {
	t.Helper()
	var count int
	if err := intake.pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&count); err != nil {
		t.Fatalf("count the head rows of %s#%d: %v", repo, number, err)
	}
	return count
}

// A pull request that closed without a merge stays closed through every later event of it.
// GitHub sends `labeled`, `unlabeled`, `edited`, `assigned` and other actions for a closed pull
// request too, and the envelope carries no state of its own, so a head row written from such an
// event must not read as open: an open row's head is a basis of the issue's `before_merge` check
// (HeadAttributedTo), so a pass at the head of a closed pull request would stand again
// (acceptance of cybermonklabs/legion#4209, defect 1). The state is GitHub's: the fake answers
// the pull request as GitHub holds it after each event.
func TestALaterEventOfAClosedPullRequestLeavesItClosed(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 71)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	const head = "7171717171717171717171717171717171717171"
	branch, body := "legion/"+issue, "Dispatch: "+issue
	at := func(hour int) time.Time { return time.Date(2026, 2, 4, hour, 0, 0, 0, time.UTC) }
	for hour, step := range []struct{ action, state string }{{"opened", "open"}, {"closed", "closed"}} {
		github.set("acme/widgets", 71, step.state, head, branch, body, at(hour))
		if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 71, step.action, head, branch, body, at(hour))); err != nil {
			t.Fatalf("handle the %s envelope: %v", step.action, err)
		}
	}
	// The control: the close itself is stored as closed, and its head is no basis.
	if got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", 71); got.State != "closed" {
		t.Fatalf("#71 after its close = %+v, want state closed", got)
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || attributed {
		t.Fatalf("HeadAttributedTo(%s) after the close = %v, %v; want false", issue, attributed, err)
	}
	for i, action := range []string{"labeled", "unlabeled", "edited", "assigned"} {
		github.set("acme/widgets", 71, "closed", head, branch, body, at(2+i))
		if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 71, action, head, branch, body, at(2+i))); err != nil {
			t.Fatalf("handle the %s envelope: %v", action, err)
		}
		if got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", 71); got.State != "closed" {
			t.Errorf("#71 after a %s event of the closed pull request = state %q, want closed", action, got.State)
		}
		if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || attributed {
			t.Errorf("HeadAttributedTo(%s) after a %s event of the closed pull request = %v, %v; want false: a pass at the head of a closed pull request stands", issue, action, attributed, err)
		}
	}
}

// An envelope GitHub never sent moves nothing: its head, its state and its updated_at are a
// caller's fields, and the row is GitHub's answer about the pull request the envelope names
// (acceptance of cybermonklabs/legion#4209, defect 2). A synchronize naming an older commit, an
// hour ahead of GitHub's own clock, leaves the head where GitHub has it; a close of an open pull
// request leaves it open; and an envelope naming a pull request GitHub does not have writes no
// row at all.
func TestAnEnvelopeWritesGitHubsHeadAndStateAndNotItsOwn(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 72)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	const (
		real   = "7272727272727272727272727272727272727272"
		forged = "7070707070707070707070707070707070707070"
	)
	branch, body := "legion/"+issue, "Dispatch: "+issue
	opened := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	github.set("acme/widgets", 72, "open", real, branch, body, opened)
	if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 72, "opened", real, branch, body, opened)); err != nil {
		t.Fatalf("handle GitHub's own opened envelope: %v", err)
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, real); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, GitHub's head) = %v, %v; want true (the control)", issue, attributed, err)
	}

	for _, step := range []struct{ action, head string }{{"synchronize", forged}, {"closed", real}} {
		if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 72, step.action, step.head, branch, body, opened.Add(time.Hour))); err != nil {
			t.Fatalf("handle the %s envelope GitHub never sent: %v", step.action, err)
		}
		got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", 72)
		if got.HeadSHA != real || got.State != "open" {
			t.Errorf("#72 after a %s envelope naming %s = head %s, state %s; want GitHub's %s, open", step.action, step.head[:12], got.HeadSHA, got.State, real)
		}
		if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, forged); err != nil || attributed {
			t.Errorf("HeadAttributedTo(%s, the envelope's head) after the %s = %v, %v; want false", issue, step.action, attributed, err)
		}
	}

	if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", 79, "opened", forged, branch, body, opened)); err != nil {
		t.Fatalf("handle an envelope naming a pull request GitHub does not have: %v", err)
	}
	if count := headRowCount(t, ctx, intake, "acme/widgets", 79); count != 0 {
		t.Errorf("an envelope naming a pull request GitHub does not have wrote %d head rows, want none", count)
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, forged); err != nil || attributed {
		t.Errorf("HeadAttributedTo(%s, the unknown pull request's head) = %v, %v; want false", issue, attributed, err)
	}
}

// The events of one pull request that waited together cost one read of it, and one more for its
// events of the second GitHub's answer holds. A first start replays what the stream kept: here 200
// pull requests, each opened and pushed twice, 600 envelopes, the last push of each at the second
// of GitHub's answer, and none naming an issue, so the first read of each reads the pull request's
// commit messages too. The first envelope of each reads GitHub, and the row it writes holds
// GitHub's answer, which answers for the first push, ten minutes older (headReadCovers). The last
// push is of the answer's own second, which an answer covers no event of, since GitHub's updated_at
// is whole seconds and that answer may be one GitHub gave before the event; so it reads GitHub once
// more. That read reads no commits: the stored texts hold the messages of the same head and base
// (storePullRequestFacts).
// The repository is the row's own, so no commit messages another row of this test binary read
// answer for these.
func TestABacklogOfOnePullRequestsEventsCostsOneReadAndOneForItsLastSecond(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	const repo = "acme/backlog"
	github := serveGitHubPulls(t, fake, repo)
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	const pulls = 200
	latest := time.Now().Add(-time.Hour).Truncate(time.Second)
	head := func(number, push int) string { return fmt.Sprintf("%02x%038d", push, number) }
	for number := 1; number <= pulls; number++ {
		github.set(repo, number, "open", head(number, 2), fmt.Sprintf("feature/%d", number), "No key here.", latest)
	}
	for push, at := range []time.Time{latest.Add(-20 * time.Minute), latest.Add(-10 * time.Minute), latest} {
		action := map[int]string{0: "opened"}[push]
		if action == "" {
			action = "synchronize"
		}
		for number := 1; number <= pulls; number++ {
			envelope := prEnvelope(repo, number, action, head(number, push), fmt.Sprintf("feature/%d", number), "No key here.", at)
			if _, err := intake.handlePullRequestEnvelope(ctx, nil, envelope); err != nil {
				t.Fatalf("handle %s of #%d: %v", action, number, err)
			}
		}
	}
	if reads, commits := github.reads.Load(), github.commitReads.Load(); reads != 2*pulls || commits != pulls {
		t.Errorf("600 envelopes of %d pull requests read GitHub's pull request %d times and its commits %d times; want %d and %d", pulls, reads, commits, 2*pulls, pulls)
	}
	t.Logf("600 envelopes: %d pull request reads, %d commit reads", github.reads.Load(), github.commitReads.Load())
	if got := readHead(t, unrenamed(), ctx, pool, repo, pulls); got.HeadSHA != head(pulls, 2) {
		t.Errorf("#%d's head = %s, want GitHub's %s", pulls, got.HeadSHA, head(pulls, 2))
	}
}

// A rate limit met in the middle writes no row and is ErrHeadNotStored with GitHub's wait kept
// inside it, which Intake.deliver owes a read for and acknowledges (the read is the reconcile's,
// after the limit: githubapp.Client sends nothing while it stands). Once GitHub answers again, a
// read of the same envelope writes the row.
func TestARateLimitedPullRequestReadWritesNoRowAndIsAHeadNotStored(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, nil, unrenamed())
	const head = "7474747474747474747474747474747474747474"
	at := time.Now().Truncate(time.Second)
	github.set("acme/widgets", 74, "open", head, "feature/74", "", at)
	github.mu.Lock()
	github.limited = true
	github.mu.Unlock()
	envelope := prEnvelope("acme/widgets", 74, "opened", head, "feature/74", "", at)
	_, err := intake.handlePullRequestEnvelope(ctx, nil, envelope)
	limited, isLimit := githubapp.AsRateLimit(err)
	if !errors.Is(err, ErrHeadNotStored) || !isLimit || limited.Wait < time.Minute {
		t.Fatalf("a rate-limited read = %v; want ErrHeadNotStored carrying GitHub's wait of about 120 s", err)
	}
	if count := headRowCount(t, ctx, intake, "acme/widgets", 74); count != 0 {
		t.Fatalf("a rate-limited read wrote %d head rows, want none", count)
	}
	github.mu.Lock()
	github.limited = false
	github.mu.Unlock()
	if _, err := pool.Exec(ctx, `delete from github_rate_limits`); err != nil {
		t.Fatalf("end the recorded limit, as its wait running out does: %v", err)
	}
	if _, err := intake.handlePullRequestEnvelope(ctx, nil, envelope); err != nil {
		t.Fatalf("the envelope read again once the limit ended: %v", err)
	}
	if got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", 74); got.HeadSHA != head {
		t.Errorf("#74 after the limit = %+v, want head %s", got, head)
	}
}

// A GitHub outage loses no pull request's head: the intake owes each failed read and
// acknowledges its envelope, and once GitHub answers again the reconcile's next pass writes the
// head of a pull request that opened during the outage, and of one that was pushed during it,
// with no later event of either (acceptance of cybermonklabs/legion#4209, round 2, defect 3).
//
// Taken from the acceptance tester's TestAcceptAGitHubOutageLongerThanTheCapLosesNoHead, with
// three changes. The step after the outage runs one reconcile pass, since the fix is that the
// reconcile reads every pull request whose read the intake owed, and nothing else does; the
// tester's test starts no reconcile, which reads only the rows that exist. The outage ends once
// the intake has acknowledged both envelopes, not after a fixed sleep. And a second pull request,
// #9202, stands at an earlier head GitHub answered just before the outage, so its row is one the
// reconcile's own five-minute arm would not read yet: what reads it is the read its event is owed.
// With no delivery settings, as the deployment the pull request fixes holds none.
func TestAGitHubOutageLosesNoHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 9202)
	const (
		opened     = 9201
		openedHead = "9201920192019201920192019201920192019201"
		pushed     = 9202
		headBefore = "9202920292029202920292029202920292029202"
		headPushed = "9292929292929292929292929292929292929292"
	)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	branch, body := "legion/"+issue, "Dispatch: "+issue
	before := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	github.set("acme/widgets", pushed, "open", headBefore, branch, body, before)
	if _, err := newIntake(tuningFor(t), nil, pool, nil, client, nil, nil, unrenamed()).handlePullRequestEnvelope(ctx, nil,
		prEnvelope("acme/widgets", pushed, "opened", headBefore, branch, body, before)); err != nil {
		t.Fatalf("handle #%d's opening before the outage: %v", pushed, err)
	}

	during := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", opened, "open", openedHead, "feature/y", "", during)
	github.set("acme/widgets", pushed, "open", headPushed, branch, body, during)
	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", opened,
		prEnvelope("acme/widgets", opened, "opened", openedHead, "feature/y", "", during))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", pushed,
		prEnvelope("acme/widgets", pushed, "synchronize", headPushed, branch, body, during))

	// GitHub stays down until the intake has owed both reads and acknowledged both envelopes.
	waitFor(t, 90*time.Second, func() bool {
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		if err != nil {
			return false
		}
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.AckFloor.Stream >= stream.State.LastSeq && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the intake did not acknowledge both envelopes while GitHub was down: %+v", deliveryConsumerInfo(t, natsClient))
	})
	if count := headRowCount(t, ctx, &Intake{pool: pool}, "acme/widgets", opened); count != 0 {
		t.Fatalf("#%d has %d head rows while GitHub answers nothing, want none: no read of GitHub answered for it", opened, count)
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, headPushed); err != nil || attributed {
		t.Fatalf("HeadAttributedTo(%s, the pushed head) while GitHub answers nothing = %v, %v; want false", issue, attributed, err)
	}
	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)

	stored := func(number int, head string) int {
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from pull_request_heads where repo = 'acme/widgets' and number = $1 and head_sha = $2",
			number, head).Scan(&count); err != nil {
			t.Fatalf("read the head row: %v", err)
		}
		return count
	}
	waitFor(t, 30*time.Second, func() bool { return stored(opened, openedHead) == 1 && stored(pushed, headPushed) == 1 }, func() string {
		return fmt.Sprintf("after GitHub answers again and the reconcile's next pass, acme/widgets#%d has %d head rows at %s and #%d %d at %s, want one each: the envelopes that named them were acknowledged with their reads owed during the outage; the durable is %+v",
			opened, stored(opened, openedHead), openedHead[:12], pushed, stored(pushed, headPushed), headPushed[:12], deliveryConsumerInfo(t, natsClient))
	})
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, headPushed); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, the pushed head) after the pass = %v, %v; want true", issue, attributed, err)
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, headBefore); err != nil || attributed {
		t.Fatalf("HeadAttributedTo(%s, the head before the push) after the pass = %v, %v; want false", issue, attributed, err)
	}
}

// GitHub's 404 for a pull request its own event names is read again by the reconcile: GitHub's
// answer to a read made a moment after its webhook may not know the pull request yet, and a 404
// writes no row, so without the read it is owed the pull request would have no head until a
// later event of it. A pull request GitHub never had, in a repository the App reads, is settled
// at that read with one more call, the one GraphQL answer that shows the repository readable and
// the pull request absent (pullRequestAbsent).
func TestAPullRequestGitHubDidNotKnowAtItsEventIsReadByTheNextReconcilePass(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 77)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	intake := newIntake(tuningFor(t), nil, pool, nil, client, nil, nil, unrenamed())
	const (
		lagged = 77
		never  = 78
		head   = "7777777777777777777777777777777777777777"
	)
	branch, body := "legion/"+issue, "Dispatch: "+issue
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	for _, number := range []int{lagged, never} {
		if _, err := intake.handlePullRequestEnvelope(ctx, nil, prEnvelope("acme/widgets", number, "opened", head, branch, body, at)); err != nil {
			t.Fatalf("handle #%d's opening, which GitHub answers 404 for: %v", number, err)
		}
		if count := headRowCount(t, ctx, intake, "acme/widgets", number); count != 0 {
			t.Fatalf("#%d has %d head rows after GitHub's 404, want none: nothing an envelope says is GitHub's answer", number, count)
		}
	}
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || attributed {
		t.Fatalf("HeadAttributedTo(%s) after two 404s = %v, %v; want false: a read owed is no head", issue, attributed, err)
	}

	github.set("acme/widgets", lagged, "open", head, branch, body, at)
	reconcile := NewReconcile(pool, client, events.NewBroker(), unrenamed())
	reconcile.runOnce(ctx)
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, GitHub's head of #%d) after the reconcile's next pass = %v, %v; want true", issue, lagged, attributed, err)
	}
	owed := func(number int) int {
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from pull_request_reads_owed where repo = 'acme/widgets' and number = $1", number).Scan(&count); err != nil {
			t.Fatalf("read the owed reads: %v", err)
		}
		return count
	}
	if owed(lagged) != 0 || owed(never) != 0 {
		t.Fatalf("after one pass #%d is owed %d read(s) and #%d %d, want none of either: GitHub answered for the first, and for the second the repository it can read has no such pull request", lagged, owed(lagged), never, owed(never))
	}
	if asked, reads := github.absenceQueries.Load(), github.repoReads.Load(); asked != 1 || reads != 0 {
		t.Errorf("the pass asked GraphQL whether the repository holds #%d %d times and read the repository %d times, want once and never: the one more call of #%d's refusal is one answer", never, asked, reads, never)
	}
	if count := headRowCount(t, ctx, intake, "acme/widgets", never); count != 0 {
		t.Fatalf("#%d has %d head rows, want none", never, count)
	}
}

// A pull request whose branch, title and body name no issue reaches its issue only through a link
// the issue holds, and with no delivery settings the intake still stores its head, so after the
// link and a push the issue lists the pull request at the pushed head (HeadsAttributedTo, what
// the results route compares each check with). #302 is the control: the same pull request with
// no link is listed nowhere. At origin/main 7e33d96621 no head row was stored for either, so the
// issue listed nothing and every check read "Compared with nothing" (three live cases; the probe
// of the planner of the next change: "head rows: #301 0, #302 0"). Each envelope
// goes through Intake.deliver with the settings production holds: none.
func TestAKeyFreePullRequestLinkedOnItsIssueIsListedAtItsPushedHeadWithNoDeliverySettings(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 9301)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, newSettingsCache(pool, nil, tuningFor(t).log), unrenamed())
	const opened, pushed = "4301430143014301430143014301430143014301", "e273e273e273e273e273e273e273e273e273e273"
	const branch, body = "test/nats-hooked-server", "No key here."
	deliver := func(number int, action, head string, at time.Time) {
		t.Helper()
		github.set("acme/widgets", number, "open", head, branch, body, at)
		payload, err := json.Marshal(prEnvelope("acme/widgets", number, action, head, branch, body, at))
		if err != nil {
			t.Fatal(err)
		}
		intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
			msg:      &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr." + strconv.Itoa(number)},
			envelope: &contracts.Envelope{Payload: string(payload)},
		})
	}
	at := time.Date(2026, 11, 17, 9, 12, 0, 0, time.UTC)
	for _, number := range []int{301, 302} {
		deliver(number, "opened", opened, at)
	}
	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`,
		issue, "https://github.com/acme/widgets/pull/301"); err != nil {
		t.Fatalf("link #301 to %s: %v", issue, err)
	}
	for _, number := range []int{301, 302} {
		deliver(number, "synchronize", pushed, at.Add(48*time.Minute))
	}
	heads, err := unrenamed().HeadsAttributedTo(ctx, pool, issue, HeadAnswers{})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int]string{}
	for _, head := range heads {
		listed[head.Number] = head.HeadSHA
	}
	t.Logf("head rows: #301 %d, #302 %d; %s lists %v", headRowCount(t, ctx, intake, "acme/widgets", 301),
		headRowCount(t, ctx, intake, "acme/widgets", 302), issue, listed)
	if listed[301] != pushed {
		t.Errorf("%s lists %v, want #301 at the pushed head %.12s: the results route compares its checks with nothing", issue, listed, pushed)
	}
	if _, has := listed[302]; has {
		t.Errorf("%s lists #302, which no link and no key names", issue)
	}
}
