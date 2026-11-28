package delivery

// The intake in a deployment that has never saved delivery settings. A pull request's head row is
// what a `before_merge` check's results are compared with and what a check CI proves is read at
// (api/results_ci.go, NewestPullRequestHead), and no part of the delivery timeline, so the intake
// binds its durable and writes that row whatever the settings say; only the delivery timeline's
// own work waits for the settings. Its first bind starts at the stream's next message, so a
// deployment that turns the intake on does not replay the stream's whole retained history.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// openedForKey is the `pull_request` envelope GitHub's `opened` makes for a pull request of
// deliveryRepo whose branch is legion/<key> and whose body names the issue on a `Dispatch:` line,
// at head sha: the shape a Legion implementer opens.
func openedForKey(number int, key, sha string) map[string]string {
	return map[string]string{
		"kind": "pr", "action": "opened", "merged": "false", "repo": deliveryRepo,
		"number": fmt.Sprint(number), "title": "feat: a widget",
		"body": "Dispatch: " + key, "author": "legion-implementer[bot]",
		"url":      fmt.Sprintf("https://github.com/%s/pull/%d", deliveryRepo, number),
		"head_ref": "legion/" + key, "head_repo": deliveryRepo, "head_sha": sha, "base_ref": "main",
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// headRows is how many pull_request_heads rows deliveryRepo's pull request number has.
func headRows(t *testing.T, ctx context.Context, pool *store.Pool, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`,
		deliveryRepo, number).Scan(&rows); err != nil {
		t.Fatalf("count the head rows of %s#%d: %v", deliveryRepo, number, err)
	}
	return rows
}

// publishAnswered makes GitHub hold deliveryRepo's pull request number as envelope reports it
// (its head, branch, title, body, state and merge), then publishes envelope: the intake writes a
// head from GitHub's answer to its read, never from the envelope's own fields
// (recordPullRequestHead), so a test whose envelope must write a head serves the pull request
// first. It answers the published message's stream sequence.
func publishAnswered(t *testing.T, client *bus.Client, github *githubPulls, number int, envelope map[string]string) uint64 {
	t.Helper()
	updatedAt, err := time.Parse(time.RFC3339, envelope["updated_at"])
	if err != nil {
		t.Fatalf("the envelope of %s#%d carries updated_at %q: %v", deliveryRepo, number, envelope["updated_at"], err)
	}
	state := "open"
	if envelope["action"] == "closed" {
		state = "closed"
	}
	github.set(deliveryRepo, number, state, envelope["head_sha"], envelope["head_ref"], envelope["body"], updatedAt)
	github.edit(number, func(pull map[string]any) {
		pull["title"] = envelope["title"]
		if envelope["merged"] == "true" {
			pull["merged"] = true
			pull["merged_at"] = envelope["updated_at"]
			pull["merge_commit_sha"] = strings.Repeat("f", 40)
		}
	})
	return publishPullRequestEnvelope(t, client, "acme", "widgets", number, envelope)
}

// failOtherCalls makes fake fail the test on every call to GitHub but the three an intake makes to
// read a pull request of deliveryRepo: its installation lookup, the token mint and the read of the
// pull request. Any other REST route is answered 404 and fails the test, and so does any GraphQL
// query, the batched query of facts (which the fake answers before its mux sees it) or any other.
// A count of token mints cannot stand in for this: the token minted for the first read is cached,
// so a later call reuses it.
func failOtherCalls(t *testing.T, fake *fakeGitHub) {
	t.Helper()
	routes := fake.mux
	repo := regexp.QuoteMeta(deliveryRepo)
	allowed := regexp.MustCompile(`^(GET /repos/` + repo + `/installation|POST /app/installations/\d+/access_tokens|GET /repos/` + repo + `/pulls/\d+)$`)
	refuse := func(w http.ResponseWriter, call string) {
		t.Errorf("the intake called GitHub's %s, want only the installation lookup, the token mint and a read of a pull request", call)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
	fake.mux = http.NewServeMux()
	fake.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if call := r.Method + " " + r.URL.Path; r.URL.Path == "/graphql" || !allowed.MatchString(call) {
			refuse(w, call)
			return
		}
		routes.ServeHTTP(w, r)
	})
	fake.facts = func(w http.ResponseWriter, _ *http.Request) { refuse(w, "POST /graphql (a batched query of facts)") }
}

// The reported case: a deployment with no delivery_settings row, and a pull request opened on a
// repository Dispatch serves, its branch legion/<KEY> and its body `Dispatch: <KEY>`. The intake
// binds its durable, writes the head row attributed to KEY, and the read `dispatch results` makes
// (NewestPullRequestHead) finds that head, so a check CI proves is derived at it rather than read
// as no_head. The intake writes a head from GitHub's answer to its read of the pull request, never
// from the envelope's own fields, so GitHub is asked for each pull request. A workflow run and a
// merge in the same deployment are the delivery timeline's: the run is acknowledged and fetches
// nothing, and the merge writes its head row from its one read and no delivery row. Every other
// call to GitHub fails the test (failOtherCalls).
func TestAnIntakeWithNoDeliverySettingsRecordsAPullRequestsHead(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 58)
	settings := loadedSettingsCache(t, ctx, pool)
	if held := settings.Load(); held != nil {
		t.Fatalf("the settings cache holds %+v in a database with no delivery_settings row, want nothing", held)
	}
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, deliveryRepo)
	failOtherCalls(t, fake)
	natsClient := intakeTestClient(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), settings, unrenamed())
	startIntake(t, intake)
	awaitPushBound(t, natsClient, "", "the intake of a deployment with no delivery settings did not bind its durable")

	const number = 5801
	sha := "5801580158015801580158015801580158015801"
	publishAnswered(t, natsClient, github, number, openedForKey(number, key, sha))
	waitFor(t, 20*time.Second, func() bool { return headRows(t, ctx, pool, number) == 1 }, func() string {
		return fmt.Sprintf("%s#%d has %d head rows 20s after its opened envelope, want 1\n%s",
			deliveryRepo, number, headRows(t, ctx, pool, number), intakeStateReport(t, natsClient))
	})
	head := readHead(t, unrenamed(), ctx, pool, deliveryRepo, number)
	if head.IssueKey == nil || *head.IssueKey != key || head.State != "open" || head.HeadSHA != sha {
		t.Fatalf("the head row of %s#%d = %+v, want open at %s and attributed to %s", deliveryRepo, number, head, sha, key)
	}
	newest, found, err := unrenamed().NewestPullRequestHead(ctx, pool, key, HeadAnswers{})
	if err != nil || !found || newest.Number != number || newest.HeadSHA != sha {
		t.Fatalf("NewestPullRequestHead(%s) = %+v, found %t, %v; want %s#%d at %s", key, newest, found, err, deliveryRepo, number, sha)
	}

	const merged = 5802
	mergedEnvelope := openedForKey(merged, key, "5802580258025802580258025802580258025802")
	mergedEnvelope["action"], mergedEnvelope["merged"], mergedEnvelope["author"] = "closed", "true", "octocat"
	publishAnswered(t, natsClient, github, merged, mergedEnvelope)
	publishWorkflowEnvelope(t, natsClient, "acme", "widgets", ".github/workflows/deploy.yml", 9101)
	awaitAcknowledged(t, natsClient)
	if rows := headRows(t, ctx, pool, merged); rows != 1 {
		t.Fatalf("%s#%d, merged with no delivery settings, has %d head rows, want 1", deliveryRepo, merged, rows)
	}
	var delivered int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests`).Scan(&delivered); err != nil {
		t.Fatalf("count the delivery rows: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivery_pull_requests rows = %d after a merge with no delivery settings, want 0", delivered)
	}
	if runs := countRuns(t, ctx, pool); runs != 0 {
		t.Fatalf("delivery_runs rows = %d after a workflow run with no delivery settings, want 0", runs)
	}
	if reads, commits := github.reads.Load(), github.commitReads.Load(); reads != 2 || commits != 0 {
		t.Fatalf("GitHub was asked for a pull request %d times and for its commits %d times, want 2 and 0: the intake reads each pull request envelope's pull request once, the merge's too, its branch names the issue, and with no settings a merge and a workflow run fetch nothing else", reads, commits)
	}
}

// The first bind of a deployment that never had the durable starts at the stream's next message,
// as an explicit start sequence: a pull request opened before the bind, which the stream still
// holds, gets no head row from it. A take-over of that durable before it has acknowledged anything
// replays none of it either, and a pull request opened after the take-over gets its row. The
// take-over's start rules, row by row, are TestATakeOverStartsAtTheFloorRuleOrTheTip's, and the
// tests beside it take a never-bound durable and one whose floor lies past the stream's end over on
// a server (takeover_start_test.go).
func TestAFirstBindStartsAtTheStreamsNextMessageAndATakeOverKeepsThatStart(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 59)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, deliveryRepo)
	publisher := intakeTestClient(t)

	const before, after = 5901, 5902
	stored := publishAnswered(t, publisher, github, before, openedForKey(before, key, "5901590159015901590159015901590159015901"))

	firstClient := intakeTestClient(t)
	first := newIntake(tuningFor(t), firstClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	firstCtx, stopFirst := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first.Run(firstCtx)
	}()
	t.Cleanup(func() {
		stopFirst()
		<-firstDone
	})
	bound := awaitPushBound(t, publisher, "", "the first bind did not bind the durable")
	if bound.Config.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || bound.Config.OptStartSeq != stored+1 {
		t.Fatalf("the first bind created the durable with deliver policy %v at start %d, want an explicit start at %d, the message after the one the stream held",
			bound.Config.DeliverPolicy, bound.Config.OptStartSeq, stored+1)
	}
	stopFirst()
	<-firstDone

	second := newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	startIntake(t, second)
	awaitPushBound(t, publisher, bound.Config.DeliverSubject, "the take-over did not bind the durable afresh")
	publishAnswered(t, publisher, github, after, openedForKey(after, key, "5902590259025902590259025902590259025902"))
	awaitAcknowledged(t, publisher)
	if rows := headRows(t, ctx, pool, after); rows != 1 {
		t.Fatalf("%s#%d, opened after the take-over, has %d head rows, want 1", deliveryRepo, after, rows)
	}
	if rows := headRows(t, ctx, pool, before); rows != 0 {
		t.Fatalf("%s#%d, opened before the first bind, has %d head rows, want 0: the take-over replayed the stream from before the durable's start", deliveryRepo, before, rows)
	}
}
