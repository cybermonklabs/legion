//go:build twobuild

// The bus half of the two-build suite: the messages and subscription settings the two builds pass
// and reconcile while both serve one NATS (twobuild_harness_test.go holds the harness both halves
// start their builds with).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testnats"
)

// ───── messages and subscription settings ───────────────────────────────────

// settingsPolls is two of the delivery intake's settings polls (delivery.settingsPollInterval,
// 30 s, unexported) and a margin: the window a standby of the build before generations compares
// the consumers' settings with its own, twice.
const settingsPolls = 2*30*time.Second + 5*time.Second

// consumerGenerationKey is the consumer metadata key this tree stamps its generation under
// (delivery.consumerGenerationKey, unexported).
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerSettings is what a delivery durable's settings must keep while two builds run.
type consumerSettings struct {
	FilterSubject string
	AckWait       time.Duration
	MaxAckPending int
	Generation    string
}

// streamSettings is what the notification stream's settings must keep while two builds run.
type streamSettings struct {
	MaxAge     time.Duration
	Duplicates time.Duration
	Subjects   []string
	Replicas   int
}

type busSettings struct {
	Consumers map[string]consumerSettings
	Stream    streamSettings
}

func readBusSettings(t *testing.T, js natsgo.JetStreamContext) busSettings {
	t.Helper()
	settings := busSettings{Consumers: map[string]consumerSettings{}}
	for _, durable := range deliveryDurables {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read the durable %s: %v", durable, err)
		}
		settings.Consumers[durable] = consumerSettings{
			FilterSubject: info.Config.FilterSubject,
			AckWait:       info.Config.AckWait,
			MaxAckPending: info.Config.MaxAckPending,
			Generation:    info.Config.Metadata[consumerGenerationKey],
		}
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	settings.Stream = streamSettings{
		MaxAge:     info.Config.MaxAge,
		Duplicates: info.Config.Duplicates,
		Subjects:   slices.Clone(info.Config.Subjects),
		Replicas:   info.Config.Replicas,
	}
	return settings
}

func requireBusSettings(t *testing.T, js natsgo.JetStreamContext, want busSettings, when string) {
	t.Helper()
	if got := readBusSettings(t, js); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("%s, the bus settings are\n%+v\nwant them unchanged from\n%+v", when, got, want)
	}
}

// pullRequestFake is the loopback GitHub both builds' processes call: the App's installation and
// token, the reads a delivery reconcile pass makes (an empty pull-request search and empty
// workflow run and job listings, as multiprocess_test.go's githubFakeAnswer answers them, which
// this tag does not compile), and a merged pull request and its first commit. It is one state
// served by one server per build (serve), so each call is recorded with the build that made it.
//
// The pull request is what GitHub answers for a merge of the deploy repository by a population
// author: labelled non-task, its head in acme/deploy, and titled with the issue the row's pull
// requests are for (setIssue). So a handling of its message runs the merge path to its end: the
// head row with its attribution's event, the delivery row (delivery.UpsertPullRequest) and the
// deploy record (issue.deployed). The title also names the build whose server answered
// (pullRequestTitle), which is how the row tells which build wrote a delivery row: the row stores
// the title its handling's own fetch read (deliveryWrites).
//
// A read is not a handling. A reconcile reads the same pull request through the same call when it
// judges again a merged row an older attribution rule judged (the backfill a raised
// delivery.AttributionRuleVersion starts, ListStalePullRequestHeads), and writes its head row and
// nothing else. The row judges each handling by the delivery row it wrote, and keeps the reads
// only to name them in its log.
//
// The fake takes delay to answer the commit listing, which spreads the messages' handling over a
// stop, and a listing whose process went away while it waited (the request's context ended) is
// counted as abandoned: NATS delivers that message again. A request that would change anything on
// GitHub (any method but GET, the token mint and the GraphQL search, a read, excepted) is recorded
// as a write. Handling a merged pull request posts nothing to GitHub, so the row requires none.
type pullRequestFake struct {
	delay time.Duration

	mu        sync.Mutex
	issue     string
	read      map[int][]handling
	abandoned map[int]int
	writes    []string
}

// handling is one handling of a pull request, or one read of it: the build that made it, and when.
type handling struct {
	build string
	at    time.Time
}

func (h handling) String() string { return h.build + " at " + h.at.Format("15:04:05.000") }

func startPullRequestFake(delay time.Duration) *pullRequestFake {
	return &pullRequestFake{delay: delay, read: map[int][]handling{}, abandoned: map[int]int{}}
}

// setIssue makes issue the key every pull request's title names from now on.
func (f *pullRequestFake) setIssue(issue string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issue = issue
}

// pullRequestTitleBuild is the text of a pull request's title after which pullRequestTitle names
// the build whose GitHub answered.
const pullRequestTitleBuild = ": A pull request, as the GitHub of the "

// pullRequestTitle is the title build's GitHub answers for a pull request of issue.
func pullRequestTitle(issue, build string) string {
	return issue + pullRequestTitleBuild + build + " answers it"
}

// buildOfTitle is the build a stored title names (pullRequestTitle), "" for any other title.
func buildOfTitle(title string) string {
	_, rest, found := strings.Cut(title, pullRequestTitleBuild)
	if !found {
		return ""
	}
	build, _ := strings.CutSuffix(rest, " answers it")
	return build
}

// serve starts a server on the fake's state whose calls are build's, and returns its URL: the
// DISPATCH_GITHUB_API_BASE of build's processes.
func (f *pullRequestFake) serve(t *testing.T, build string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1,"app_slug":"twobuild","account":{"login":"acme"},"permissions":{"contents":"read","actions":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_twobuild","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"search":{"issueCount":0,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.read[number] = append(f.read[number], handling{build: build, at: time.Now()})
		issue := f.issue
		f.mu.Unlock()
		title, err := json.Marshal(pullRequestTitle(issue, build))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"number":%d,"title":%s,"html_url":"https://github.com/acme/deploy/pull/%d",`+
			`"user":{"login":"octocat"},"labels":[{"name":"non-task"}],"state":"closed","merged":true,`+
			`"created_at":"2026-10-26T00:00:00Z","merged_at":"2026-10-27T00:00:00Z","merge_commit_sha":"%040d",`+
			`"additions":1,"deletions":1,"head":{"ref":"a-branch","sha":"%040d","repo":{"full_name":"acme/deploy"}},`+
			`"base":{"ref":"main"}}`, number, title, number, number, number)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			f.mu.Lock()
			f.abandoned[number]++
			f.mu.Unlock()
			return
		}
		_, _ = io.WriteString(w, `[{"sha":"0000000000000000000000000000000000000001","commit":{"message":"A change.","author":{"date":"2026-10-26T00:00:00Z"},"committer":{"date":"2026-10-26T00:00:00Z"}}}]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			f.mu.Lock()
			f.writes = append(f.writes, build+": "+r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reconcileListing(r.URL.Path))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// reconcileListing is GitHub's answer to a listing a reconcile pass reads: the App's one
// installation, on acme/deploy; that installation's one repository; and the deploy and PR-checks
// workflows' runs and a run's jobs, with nothing in them, in the shape GitHub gives each. Any other
// path is answered with an empty list.
func reconcileListing(path string) string {
	switch {
	case path == "/app/installations":
		return `[{"id":1,"app_slug":"twobuild","account":{"login":"acme"},"permissions":{"contents":"read","actions":"read","pull_requests":"read"}}]`
	case path == "/installation/repositories":
		return `{"total_count":1,"repositories":[{"id":1,"name":"deploy","full_name":"acme/deploy","private":true,"owner":{"login":"acme"}}]}`
	case strings.Contains(path, "/actions/workflows/") && strings.HasSuffix(path, "/runs"):
		return `{"total_count":0,"workflow_runs":[]}`
	case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
		return `{"total_count":0,"jobs":[]}`
	}
	return "[]"
}

// calls is each pull request's reads, by number, in the order they were made; how many times a
// stopping process abandoned its commit listing; and every write a build sent GitHub.
func (f *pullRequestFake) calls() (read map[int][]handling, abandoned map[int]int, writes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read = make(map[int][]handling, len(f.read))
	for number, reads := range f.read {
		read[number] = slices.Clone(reads)
	}
	return read, maps.Clone(f.abandoned), slices.Clone(f.writes)
}

// deliveryWritesTable is the table recordDeliveryWrites has the database append each write of a
// delivery row to.
const deliveryWritesTable = "twobuild_delivery_writes"

// recordDeliveryWrites has the row's database record every write of a pull request's delivery
// row, the effect a handling of a merged pull request's message completes with: a trigger on
// delivery_pull_requests, after an insert or an update that sets the title, appends the row's
// repository, number and title to a table of the row's own, at the database's clock. The title is
// set by delivery.UpsertPullRequest alone (completePullRequest's write), so the deploy record's
// marker and the attribution's own updates, which set other columns, are no handling's; an upsert
// its `where` refuses updates no row, and is none either. A redelivery writes the same facts again,
// so the table holding a second write of a pull request's row is the only trace a second handling
// leaves in the store. The database runs on the test's host, so its clock is the test's.
func recordDeliveryWrites(t *testing.T, builds *twoBuilds) {
	t.Helper()
	for _, statement := range []string{
		`create table ` + deliveryWritesTable + ` (
			id bigint generated always as identity primary key,
			repo text not null, number int not null, title text not null,
			at timestamptz not null default clock_timestamp())`,
		`create function ` + deliveryWritesTable + `_record() returns trigger language plpgsql as $$
		begin
			insert into ` + deliveryWritesTable + ` (repo, number, title) values (new.repo, new.number, new.title);
			return null;
		end $$`,
		`create trigger ` + deliveryWritesTable + ` after insert or update of title on delivery_pull_requests
			for each row execute function ` + deliveryWritesTable + `_record()`,
	} {
		if _, err := builds.database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("record the delivery rows' writes: %v", err)
		}
	}
}

// deliveryWrites is every write of a delivery row of acme/deploy, by pull request number, in the
// order the database made them, each with the build whose GitHub answered the fetch of the
// handling that wrote it (buildOfTitle).
func deliveryWrites(t *testing.T, builds *twoBuilds) map[int][]handling {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(),
		`select number, title, at from `+deliveryWritesTable+` where repo = 'acme/deploy' order by at, id`)
	if err != nil {
		t.Fatalf("read the delivery rows' writes: %v", err)
	}
	defer rows.Close()
	written := map[int][]handling{}
	for rows.Next() {
		var number int
		var title string
		var at time.Time
		if err := rows.Scan(&number, &title, &at); err != nil {
			t.Fatalf("read the delivery rows' writes: %v", err)
		}
		written[number] = append(written[number], handling{build: buildOfTitle(title), at: at})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the delivery rows' writes: %v", err)
	}
	return written
}

// pullRequestEvents is how many events of eventType the database holds for each pull request of
// acme/deploy, by number: each delivery event names its pull request in its payload.
func pullRequestEvents(t *testing.T, builds *twoBuilds, eventType string) map[int]int {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `
		select (payload->>'number')::int, count(*) from events
		where type = $1 and payload->>'repo' = 'acme/deploy' group by 1`, eventType)
	if err != nil {
		t.Fatalf("count the %s events: %v", eventType, err)
	}
	defer rows.Close()
	counts := map[int]int{}
	for rows.Next() {
		var number, count int
		if err := rows.Scan(&number, &count); err != nil {
			t.Fatalf("count the %s events: %v", eventType, err)
		}
		counts[number] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the %s events: %v", eventType, err)
	}
	return counts
}

// shipPullRequestsTo creates the issue the row's pull requests are for, in a project whose default
// repository is acme/deploy, so the key in a pull request's title is held to it (attribution.go),
// and stores a deploy of acme/deploy's main whose production job succeeded after every one of
// them merged, so each has shipped once its delivery row is written: a handling then appends the
// pull request's attribution event and its deploy record as well. It answers the issue's key.
func shipPullRequestsTo(t *testing.T, builds *twoBuilds) string {
	t.Helper()
	ctx := context.Background()
	project := uniqueProjectKey(t, "PRS")
	builds.current().createProject(t, project, "Merged pull requests")
	if _, err := builds.database.Pool.Exec(ctx, `
		update projects set default_repo = 'acme/deploy' where key = $1
	`, project); err != nil {
		t.Fatalf("make acme/deploy %s's default repository: %v", project, err)
	}
	issue, _ := createDocument(t, builds.current(), project, "The work the pull requests ship", "The work.")
	const runID = 7001
	created := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	done := created.Add(time.Hour)
	run, job := delivery.DeliveryRunConclusionSuccess, delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRun(ctx, builds.database.Pool, delivery.DeliveryRun{
		Repo: "acme/deploy", RunID: runID, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", runID),
		HeadCommitAt: created, StartedAt: created, CreatedAt: &created, CompletedAt: &done, Conclusion: &run,
		URL: fmt.Sprintf("https://github.com/acme/deploy/actions/runs/%d", runID), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("store the deploy run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, builds.database.Pool, "acme/deploy", runID, []delivery.DeliveryRunJob{{
		Repo: "acme/deploy", RunID: runID, Name: "production", StartedAt: &created, CompletedAt: &done, Conclusion: &job,
	}}, true); err != nil {
		t.Fatalf("store the deploy run's production job: %v", err)
	}
	return issue
}

// headsAnOlderRuleJudged is the merged pull requests of acme/deploy whose head row a rule older
// than this build's judged (pull_request_heads.attribution_rule), by number: the rows this build's
// reconcile offers again once (ListStalePullRequestHeads).
func headsAnOlderRuleJudged(t *testing.T, builds *twoBuilds) []int {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `
		select number from pull_request_heads
		where repo = 'acme/deploy' and state = 'merged' and attribution_rule < $1 order by number`,
		delivery.AttributionRuleVersion)
	if err != nil {
		t.Fatalf("read the heads an older rule judged: %v", err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read the heads an older rule judged: %v", err)
	}
	return numbers
}

// seedPullRequestSettings writes the delivery settings the intakes route by: acme/deploy as the
// deploy repository and octocat as the one author whose merged pull requests are read.
func seedPullRequestSettings(t *testing.T, builds *twoBuilds) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), builds.database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
		PopulationAuthors:    []string{"octocat"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// publishMergedPullRequest publishes a GitHub-shaped message, a merge of pull request number of
// acme/deploy by octocat, titled for issue, on notifications.github.acme.deploy.pr.<number>, with
// delivery as its message id, and answers the sequence the stream stored it at. That sequence is
// what tells a repeat of the message from a replay of settled work: a take-over creates the durable
// again at the ack floor it found, so the messages it delivers again are exactly those above that
// floor.
//
// The payload names the head -- its repository, its branch and its commit -- and the title and
// URL, as publishMergedPullRequests does in multiprocess_test.go, so that handling it once judges
// the pull request's head row whole: the title's key is held to acme/deploy, and the row is
// stamped read and credited to the issue. An envelope naming no head repository, or no title,
// attributes nobody, and a reconcile's later read of GitHub would then move the row to the issue
// and append an attribution event a handling did not.
func publishMergedPullRequest(t *testing.T, js natsgo.JetStreamContext, number int, issue, delivery string) uint64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true",
		"repo": "acme/deploy", "number": strconv.Itoa(number), "author": "octocat",
		"title": issue + ": A pull request", "url": fmt.Sprintf("https://github.com/acme/deploy/pull/%d", number),
		"head_repo": "acme/deploy", "head_ref": "a-branch", "head_sha": fmt.Sprintf("%040d", number),
	})
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       delivery,
		Source:        "github",
		SourceEventID: delivery,
		Topic:         contracts.GithubResourceSubject("acme", "deploy", "pr", strconv.Itoa(number)),
		DedupeKey:     delivery,
		IssuedAt:      time.Now().Unix(),
		Payload:       string(payload),
		TraceID:       delivery,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	ack, err := js.Publish(envelope.Topic, data, natsgo.MsgId(delivery))
	if err != nil {
		t.Fatalf("publish %s: %v", envelope.Topic, err)
	}
	return ack.Sequence
}

// boundLines is how many times p has logged that it bound durable.
func boundLines(p *dispatchProcess, durable string) int {
	return len(p.logLines(`msg="dispatch delivery: NATS consumer bound"`, "name="+durable))
}

// waitForBound waits for p to have logged that it bound durable more than before times.
func waitForBound(t *testing.T, p *dispatchProcess, name, durable string, before int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for boundLines(p, durable) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the %s did not bind %s within %v:\n%s", name, durable, within, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// standbyWindow is how long a restarted process of this tree is watched while a process of the
// build before holds delivers from a durable: a hold's whole TTL (delivery.intakeHoldTTL, 15 s),
// two bind attempts (delivery.durableBindInterval, 2 s each) and a margin. A hold the process left
// behind when it stopped has lapsed within it, so a take-over that waits for that lapse is seen.
const standbyWindow = 15*time.Second + 2*2*time.Second + 3*time.Second

// consumerBinding is where the durable delivers and whether a subscriber is bound there.
func consumerBinding(t *testing.T, js natsgo.JetStreamContext, durable string) string {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	return fmt.Sprintf("delivering to %s (bound %t)", info.Config.DeliverSubject, info.PushBound)
}

// pullRequestMessages is how many GitHub-shaped messages the row publishes: half across a stop of
// each build while it holds the intake.
const pullRequestMessages = 50

// handOver is one stop of the process holding the events durable and the take-over that follows
// it: which build stopped and which took over, when the stopping process had exited, the stream
// sequence each of that half's messages was stored at, the sequence the durable delivered from
// once the take-over had bound it, the messages the stopping process logged handing back, and
// the durable's in-flight window.
//
// Those two last are what bound the messages a hand-over may have handled twice, and between
// them they are the whole of it, since a message is delivered a second time only where the
// durable was made to deliver it again:
//
//   - A take-over of this tree's deletes the durable and creates it again at the ack floor it
//     read plus one (delivery.takeOverConfig), and every message from that sequence on is
//     delivered to the new holder. The floor is contiguous and four handlers run at once (the
//     durable's MaxAckPending), so the messages above it are the ones the stopping process had
//     in its handlers and the ones whose acknowledgement landed while an earlier message's had
//     not. The stopping process can still be finishing those handlers while the take-over runs:
//     a build before holds unsubscribes first and acknowledges after (its own StopDelivering
//     says so), which is what leaves its last handlings on both sides of the hand-over.
//   - A hand-back is the other way, and the stopping process says so in its log, naming the
//     message's subject: a handler the stop gave up, or an acknowledgement neither it nor the
//     stop could publish.
//
// A take-over that binds the durable as it stands, which is what a build before holds does
// when it finds it unbound, delivers nothing again: start is then past every sequence, and a
// hand-back is the only repeat that half allows.
type handOver struct {
	stopping, taking string
	at               time.Time
	sequences        map[int]uint64
	start            uint64
	handedBack       map[int]bool
	window           int
}

// holds reports whether number is one of the messages this hand-over's half published.
func (h handOver) holds(number int) bool { _, published := h.sequences[number]; return published }

// replayed reports whether this hand-over had number delivered a second time: the take-over
// created the durable again at or below its sequence, or the stopping process handed it back.
func (h handOver) replayed(number int) bool {
	return h.sequences[number] >= h.start || h.handedBack[number]
}

// publishAcrossStop publishes a merged pull request of issue for each of numbers while holder
// delivers from the events durable, stops holder gracefully once ten of them are handled, and
// waits for standby to take both delivery durables over and for every one of numbers to be
// handled. A pull request is handled once a handling has written its delivery row
// (deliveryWrites). It returns that hand-over: its time is after holder's process exited, so every
// handling holder did is before it.
//
// Nothing reads a durable from holder's stop until standby has bound it. A take-over of this
// tree's deletes the durable and creates it again (delivery.takeOverConfig), so between those two
// calls the durable does not exist, and standby can be in that gap as holder's process exits:
// holder lets go of its hold before it exits, and standby's next poll takes the hold and the
// durable. Every release from legion-envoy-v30.21.0 on takes the durables over that way, so the
// gap comes in both hand-overs. The window is therefore read while holder still delivers.
func publishAcrossStop(t *testing.T, builds *twoBuilds, js natsgo.JetStreamContext, issue string, numbers []int, holder, standby *dispatchProcess) handOver {
	t.Helper()
	handledOf := func() int {
		written := deliveryWrites(t, builds)
		count := 0
		for _, number := range numbers {
			if len(written[number]) > 0 {
				count++
			}
		}
		return count
	}
	sequences := make(map[int]uint64, len(numbers))
	for _, number := range numbers {
		sequences[number] = publishMergedPullRequest(t, js, number, issue, fmt.Sprintf("twobuild-delivery-%d-%s", number, strings.ToLower(uniqueProjectKey(t, ""))))
	}
	// The holder stops with the messages part handled: past the first ten, well before the last,
	// at 300 ms a message.
	deadline := time.Now().Add(60 * time.Second)
	for handledOf() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("the %s handled %d of %d pull requests within 60s:\n%s", builds.buildName(holder), handledOf(), len(numbers), holder.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	standbyBound := make(map[string]int, len(deliveryDurables))
	for _, durable := range deliveryDurables {
		standbyBound[durable] = boundLines(standby, durable)
	}
	before := deliverSubjectOf(t, js, bus.DispatchDeliveryDurable)
	window := readBusSettings(t, js).Consumers[bus.DispatchDeliveryDurable].MaxAckPending
	stop(t, holder)
	over := handOver{
		stopping: builds.buildName(holder), taking: builds.buildName(standby), at: time.Now(),
		sequences: sequences, window: window,
	}
	t.Logf("stopped the %s with %d of %d pull requests handled", over.stopping, handledOf(), len(numbers))
	for _, durable := range deliveryDurables {
		waitForBound(t, standby, over.taking, durable, standbyBound[durable], 60*time.Second)
	}
	// Read once the take-over has bound the durable: a take-over that created it again delivers
	// from the sequence it created it at, and one that bound the durable as it stands -- which
	// leaves the deliver subject it had -- delivers nothing again.
	over.start = startedFrom(t, js, bus.DispatchDeliveryDurable, before)
	over.handedBack = handedBackNumbers(holder)
	t.Logf("the %s took the durable over delivering from sequence %d; the %s had logged handing back %v",
		over.taking, over.start, over.stopping, slices.Sorted(maps.Keys(over.handedBack)))
	deadline = time.Now().Add(120 * time.Second)
	for handledOf() < len(numbers) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d pull requests were handled within 120s of the %s's stop:\n%s",
				handledOf(), len(numbers), over.stopping, standby.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return over
}

// deliverSubjectOf is where durable delivers now.
func deliverSubjectOf(t *testing.T, js natsgo.JetStreamContext, durable string) string {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	return info.Config.DeliverSubject
}

// startedFrom is the stream sequence durable delivers from after a take-over that left it
// delivering somewhere other than before: its OptStartSeq, which a take-over of this tree sets
// to the ack floor it read plus one. A durable still delivering at before was bound as it
// stands and delivers nothing again, which is every sequence past the stream's last.
func startedFrom(t *testing.T, js natsgo.JetStreamContext, durable, before string) uint64 {
	t.Helper()
	info, err := js.ConsumerInfo(bus.Stream, durable)
	if err != nil {
		t.Fatalf("read the durable %s: %v", durable, err)
	}
	if info.Config.DeliverSubject == before || info.Config.OptStartSeq == 0 {
		return math.MaxUint64
	}
	return info.Config.OptStartSeq
}

// handedBackNumbers is every pull request p logged handing back rather than acknowledging: a
// handler its stop gave up, and an acknowledgement neither the handler nor the stop could
// publish. Both lines name the message's subject, whose last token is the pull request number.
func handedBackNumbers(p *dispatchProcess) map[int]bool {
	numbers := map[int]bool{}
	lines := append(
		p.logLines(`msg="dispatch delivery: the intake stopped before the event's handling finished; handing it back"`),
		p.logLines(`msg="dispatch delivery: answer the held message"`)...)
	for _, line := range lines {
		_, after, found := strings.Cut(line, "subject=notifications.github.acme.deploy.pr.")
		if !found {
			continue
		}
		digits := after
		if end := strings.IndexAny(digits, " \t\""); end >= 0 {
			digits = digits[:end]
		}
		if number, err := strconv.Atoi(digits); err == nil {
			numbers[number] = true
		}
	}
	return numbers
}

// pullRequestNumbers is count pull request numbers from first.
func pullRequestNumbers(first, count int) []int {
	numbers := make([]int, count)
	for index := range numbers {
		numbers[index] = first + index
	}
	return numbers
}

// Messages and subscription settings pass between the two builds with none skipped, and no
// message's effects made twice but where at-least-once delivery makes it so. Issues created on
// either build produce outbox events whose ids are exactly the ids the stream holds. GitHub-shaped
// messages, merges of acme/deploy's pull requests, are each handled across a graceful stop of the
// build that holds the intake: this tree first, then the previous build.
//
// A message is judged by what its handling leaves, not by what it reads. A handling writes the pull
// request's head row, its delivery row (delivery.UpsertPullRequest), the attribution's event on
// the issue the pull request is credited to, and the deploy record (issue.deployed) of a pull
// request a production deploy has shipped, and posts nothing to GitHub. A second handling of the
// same message writes the same rows again and appends nothing: the head row and the delivery row
// are upserts of the same facts, the attribution's event is appended only on a move of the issue,
// and the deploy record only while the row's marker is unset. Only the delivery row's write itself
// is made again (its updated_at), so the row counts each pull request's delivery-row writes
// (recordDeliveryWrites), requires exactly one attribution event and one deploy record per pull
// request, and requires that no build wrote anything to GitHub.
//
// A hand-over is where a repeated handling can come from, and the durable's ack floor is what
// bounds it. A take-over creates the durable again at the floor it found plus one
// (delivery.takeOverConfig), so the next holder is delivered every message above that floor. Four
// handlers run at once (the durable's MaxAckPending), each finishing in its own time, so at the
// moment a holder's process exits the messages above its floor are the ones it had in its
// handlers and the ones whose acknowledgement landed while an earlier message's had not. Each of
// those may be handled a second time, by the build that took the durable over, after the stopping
// process exited.
//
// So a second write of a delivery row is allowed only where the row can show it was one of those:
// the message's stream sequence is above the floor the stopping process left (handOver.replayed),
// its first write is the stopping build's before that process exited, its second is the taking
// build's after, and there are no more of them at one hand-over than the durable's in-flight
// window. Every other repeat fails the row — a delivery row written twice by one build, a third
// write, a write once the hand-over has settled, and above all a repeat of a message at or below
// the floor, which is work the hand-over had no reason to deliver again. A message whose delivery
// row no build wrote fails it too.
//
// A read of a pull request is not a handling. This build's reconcile judges again, once, each
// merged pull request a rule older than its own attribution rule judged (the backfill a raised
// delivery.AttributionRuleVersion starts): it reads the pull request from GitHub and writes its
// head row, and nothing else. This build runs a reconcile pass as it starts and every
// reconcileEvery after, so the backfill reaches the pull requests the previous build handled
// before this build's restart inside the row's window every run, and the row requires each of
// them judged by this build's rule by its end: with two builds of one rule there is none.
//
// The delivery durables' filter, ack wait, ack bound and generation, and the stream's age,
// duplicate window, subjects and replicas, stay as they are across two settings polls with both
// builds running, and across a restart of each build in turn. While a process of the previous
// build delivers from the durable, a restarted process of this tree stands by rather than take
// the durable from it. From legion-envoy-v30.21.0 on, the previous build holds the durable's
// hold, and the restarted process stands by because takeHold refuses a live hold. A build before
// holds, such as the first-rollout build, delivers under no hold, and the restarted process
// stands by because it finds the durable bound to a subject no hold recorded (boundElsewhere).
func TestTwoBuildsPassMessagesBothWays(t *testing.T) {
	builds := newTwoBuilds(t)
	seedPullRequestSettings(t, builds)
	recordDeliveryWrites(t, builds)
	// One App key for both builds; each build calls its own server of the one fake, so each
	// call is recorded with the build that made it.
	fake := startPullRequestFake(300 * time.Millisecond)
	app := githubAppEnv(t, "")
	previousEnv, currentEnv := maps.Clone(app), maps.Clone(app)
	previousEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.previousName())
	currentEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.currentName())
	// The backfill's start is the row's own: a pass as this build starts, and one every
	// reconcileEvery, whose lease period it is too (duty.Exclusive), so the restarted process's
	// first pass is never refused as served by its first start's.
	currentEnv["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	currentEnv["DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL"] = reconcileEvery.String()

	// This tree creates the durables, stamped with its generation, and holds them; the previous
	// build starts beside it as a standby, which is a rolling release's first moment.
	builds.startCurrent(t, currentEnv)
	for _, durable := range deliveryDurables {
		waitForBound(t, builds.current(), builds.currentName(), durable, 0, 60*time.Second)
	}
	builds.startPrevious(t, previousEnv)

	js, err := testnats.Connect(t, builds.nats).JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	baseline := readBusSettings(t, js)
	t.Logf("bus settings with both builds running: %+v", baseline)

	t.Run("outbox events", func(t *testing.T) {
		project := uniqueProjectKey(t, "OBX")
		builds.current().createProject(t, project, "Two builds")
		for _, writer := range []*dispatchProcess{builds.previous(), builds.current(), builds.previous(), builds.current()} {
			key, _ := createDocument(t, writer, project, "Created on the "+builds.buildName(writer), "spec")
			writer.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", `{"body":"A comment."}`, http.StatusCreated)
		}
		waitForEveryEventPublished(t, builds.database, 60*time.Second)
		events := publishedEventIDs(t, builds)
		streamed := map[string]bool{}
		for _, item := range readStreamFrom(t, js, head) {
			if item.Source == "dispatch" {
				streamed[item.SourceEventID] = true
			}
		}
		if got, want := slices.Sorted(maps.Keys(streamed)), slices.Sorted(maps.Keys(events)); !slices.Equal(got, want) {
			t.Errorf("the stream holds the events\n%v\nwant exactly the events both builds wrote\n%v", got, want)
		}
	})
	// After the outbox row, whose comparison reads every event the database has published.
	issue := shipPullRequestsTo(t, builds)
	fake.setIssue(issue)

	time.Sleep(settingsPolls)
	requireBusSettings(t, js, baseline, "after two settings polls with both builds running")

	if boundLines(builds.previous(), bus.DispatchDeliveryDurable) != 0 {
		t.Fatalf("the %s bound %s while the %s held it:\n%s", builds.previousName(), bus.DispatchDeliveryDurable,
			builds.currentName(), builds.previous().Output.String())
	}
	half := pullRequestMessages / 2
	currentHandOver := publishAcrossStop(t, builds, js, issue, pullRequestNumbers(1001, half), builds.current(), builds.previous())

	// This tree, stopped above, comes back while the previous build delivers from the durable: it
	// stands by and never takes the binding from it. A previous build from legion-envoy-v30.21.0 on
	// holds the durable's hold, which takeHold refuses while it is live; a build before holds
	// delivers under no hold, and the restarted build finds the durable bound elsewhere. Its
	// reconcile's first pass, as it starts, judges again the pull requests the previous build's
	// older rule judged.
	backfilled := headsAnOlderRuleJudged(t, builds)
	binding := consumerBinding(t, js, bus.DispatchDeliveryDurable)
	builds.startCurrent(t, currentEnv)
	time.Sleep(standbyWindow)
	if bound, now := boundLines(builds.current(), bus.DispatchDeliveryDurable), consumerBinding(t, js, bus.DispatchDeliveryDurable); bound > 0 || now != binding {
		t.Errorf("the restarted %s took %s from the %s, which delivers from it under no hold: the durable was %s and is %s, and the restarted build logged %d binds:\n%s",
			builds.currentName(), bus.DispatchDeliveryDurable, builds.previousName(), binding, now, bound, builds.current().Output.String())
	}
	requireBusSettings(t, js, baseline, "after the current build restarted")

	// The previous build, now the holder, stops with the second half part handled; this tree
	// takes the durables over, and the previous build comes back.
	previousHandOver := publishAcrossStop(t, builds, js, issue, pullRequestNumbers(1001+half, pullRequestMessages-half), builds.previous(), builds.current())
	builds.startPrevious(t, previousEnv)
	requireBusSettings(t, js, baseline, "after the previous build restarted")

	// Read last, after both restarts, so a message either hand-over delivered again and had
	// handled again is counted. A handling a stopping process gave up after its delivery-row
	// write is one of them: NATS delivers that message again, so the build that took the durable
	// over writes the row once more -- a repeat the hand-over's in-flight window bounds, with the
	// messages whose acknowledgement landed above the floor.
	time.Sleep(5 * time.Second)
	done := deliveryWrites(t, builds)
	handOvers := []handOver{currentHandOver, previousHandOver}
	var twice, again, unhandled []string
	repeats := map[string]int{}
	for number, handlings := range done {
		over, found := handOverOf(handOvers, number)
		switch {
		case len(handlings) == 1 && found:
		case len(handlings) == 2 && found && over.replayed(number) &&
			handlings[0].build == over.stopping && handlings[0].at.Before(over.at) &&
			handlings[1].build == over.taking && handlings[1].at.After(over.at):
			repeats[over.stopping]++
			again = append(again, fmt.Sprintf("#%d %v", number, handlings))
		default:
			twice = append(twice, fmt.Sprintf("#%d seq %d delivery row written %d time(s) %v (the %s's hand-over delivered from sequence %d and handed back %v)",
				number, over.sequences[number], len(handlings), handlings, over.stopping, over.start,
				slices.Sorted(maps.Keys(over.handedBack))))
		}
	}
	for _, number := range pullRequestNumbers(1001, pullRequestMessages) {
		if len(done[number]) == 0 {
			unhandled = append(unhandled, "#"+strconv.Itoa(number))
		}
	}
	if len(done) != pullRequestMessages || len(twice) > 0 || len(unhandled) > 0 {
		slices.Sort(twice)
		t.Errorf("the %d GitHub messages wrote the delivery rows of %d distinct pull requests; written again outside what the hand-overs had to deliver again: %v; never written: %v",
			pullRequestMessages, len(done), twice, unhandled)
	}
	for _, over := range handOvers {
		if repeats[over.stopping] > over.window {
			t.Errorf("the hand-over from the %s had %d message(s) handled again, more than the durable's in-flight window of %d: the stop left more unsettled than it can have had in its handlers (it delivered again from sequence %d)",
				over.stopping, repeats[over.stopping], over.window, over.start)
		}
	}
	requireOncePerPullRequest(t, builds, delivery.AttributionEventType, "attribution event", 0)
	// A deploy record a handling could not write is the reconcile's to write on its next pass.
	requireOncePerPullRequest(t, builds, delivery.DeployedEventType, "deploy record", 2*reconcileEvery)
	read, abandoned, writes := fake.calls()
	if len(writes) > 0 {
		t.Errorf("the builds sent GitHub %d write(s), where handling a merged pull request and judging one again post nothing: %v", len(writes), writes)
	}
	if left := headsAnOlderRuleJudged(t, builds); len(backfilled) > 0 {
		if still := slices.DeleteFunc(slices.Clone(backfilled), func(number int) bool { return !slices.Contains(left, number) }); len(still) > 0 {
			t.Errorf("the current build's reconcile, which runs as it starts and every %s, did not judge again by its attribution rule %d the pull requests an older rule judged before its restart: %v of %v",
				reconcileEvery, delivery.AttributionRuleVersion, still, backfilled)
		}
		t.Logf("the current build's reconcile judged again, by its attribution rule %d, the %d pull requests an older rule judged before its restart: %v",
			delivery.AttributionRuleVersion, len(backfilled), backfilled)
	} else {
		t.Logf("no pull request an older attribution rule judged before the current build's restart: the two builds judge by one rule, so no backfill runs")
	}
	readsWithoutWrite := map[string][]int{}
	for number, reads := range read {
		for _, build := range []string{builds.previousName(), builds.currentName()} {
			made := slices.IndexFunc(reads, func(h handling) bool { return h.build == build })
			wrote := slices.ContainsFunc(done[number], func(h handling) bool { return h.build == build })
			if made >= 0 && !wrote {
				readsWithoutWrite[build] = append(readsWithoutWrite[build], number)
			}
		}
	}
	for build, numbers := range readsWithoutWrite {
		slices.Sort(numbers)
		t.Logf("the %s read %d pull request(s) it wrote no delivery row of (a judging again, or a handling the stop gave up before its write): %v", build, len(numbers), numbers)
	}
	if len(abandoned) > 0 {
		t.Logf("commit listings a stopping process abandoned unfinished: %v", abandoned)
	}
	if len(again) > 0 {
		slices.Sort(again)
		t.Logf("messages a hand-over had to deliver again, whose delivery row the build that took the durable over wrote once more: %v", again)
	}
}

// reconcileEvery is how often the current build's delivery reconcile runs in
// TestTwoBuildsPassMessagesBothWays (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL), as the
// multiprocess suite's frozen-holder row runs it: well inside the restarted build's standby
// window, so the backfill lands there every run.
const reconcileEvery = 10 * time.Second

// requireOncePerPullRequest fails the test unless the database holds exactly one event of
// eventType, what names, for each of the row's pull requests, waiting up to within for each to
// have one: a handling appends the attribution's event inside its head write, and a deploy record
// it could not write is the next reconcile pass's.
func requireOncePerPullRequest(t *testing.T, builds *twoBuilds, eventType, what string, within time.Duration) {
	t.Helper()
	numbers := pullRequestNumbers(1001, pullRequestMessages)
	deadline := time.Now().Add(within)
	counts := pullRequestEvents(t, builds, eventType)
	for time.Now().Before(deadline) && slices.ContainsFunc(numbers, func(number int) bool { return counts[number] == 0 }) {
		time.Sleep(time.Second)
		counts = pullRequestEvents(t, builds, eventType)
	}
	var wrong []string
	for _, number := range numbers {
		if counts[number] != 1 {
			wrong = append(wrong, fmt.Sprintf("#%d %d", number, counts[number]))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("each pull request has one %s (%s), these another number of them: %v", what, eventType, wrong)
	}
}

// handOverOf is the hand-over whose half published number, and whether one did.
func handOverOf(overs []handOver, number int) (handOver, bool) {
	for _, over := range overs {
		if over.holds(number) {
			return over, true
		}
	}
	return handOver{}, false
}

// publishedEventIDs is the id of every event the outbox has published, as the stream's envelopes
// carry it (source_event_id).
func publishedEventIDs(t *testing.T, builds *twoBuilds) map[string]bool {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `select id::text from events where published_at is not null`)
	if err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	defer rows.Close()
	published, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the published events: %v", err)
	}
	ids := make(map[string]bool, len(published))
	for _, id := range published {
		ids[id] = true
	}
	return ids
}
