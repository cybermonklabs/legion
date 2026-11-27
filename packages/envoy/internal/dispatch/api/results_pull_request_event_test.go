package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// headAfterEventBound is how long after GitHub's `pull_request` event the results route may take
// to compare a check with the head that event reports: v-head-no-settings says "within 5 seconds
// of the event".
const headAfterEventBound = 5 * time.Second

// The pull request a `before_merge` check compares its results with is the one GitHub's own
// event reports, in a deployment that holds no delivery settings.
//
// The case is a pull request opened after its issue's results were recorded: not a draft, in the
// issue's own repository, which is not the project's default repository, with a branch and a body
// that name the issue. Nobody links it to the issue by hand.
// Envoy relays GitHub's `pull_request` event for it, as it does for every repository the App's
// webhook covers, and the delivery intake is the one writer that turns that event into the head
// row the results read compares with. A deployment that never configured the delivery timeline
// holds no delivery settings, and the head of every pull request is still the basis of every
// `before_merge` result there, so the intake must write it all the same.
//
// GitHub's commit-to-pull-request answer names nothing here, at the record and at every read,
// which is what the results read's own read-through got for the commit in the deployment this
// issue was found in: so the event is the only way the head can reach Dispatch, and the test
// proves that way and no other. The intake takes from the event which pull request changed and
// reads that pull request from GitHub, so GitHub's single pull request read answers #7 as GitHub
// holds it.
func TestAPullRequestEventGivesTheResultsReadItsHeadWithNoDeliverySettings(t *testing.T) {
	t.Parallel()
	requireTheEventsHeadWithinTheBound(t, 0)
}

// The same within the same 5 seconds, with four envelopes no row can hold published just before
// GitHub's event: numbers 3000000000 and up, which do not fit the integer columns. The intake
// takes at most four envelopes at a time, so before cybermonklabs/legion#4209's round 3 these four
// were handed back for ever and held every slot, and GitHub's event behind them never reached the
// route (acceptance round 3, defect 5: 420 s and no head). Each is now final at its first
// delivery, so the event behind them is taken at once.
func TestAPullRequestEventBehindFourEnvelopesNoRowCanHoldGivesTheResultsReadItsHead(t *testing.T) {
	t.Parallel()
	requireTheEventsHeadWithinTheBound(t, intakeSlots)
}

// intakeSlots is how many envelopes the delivery intake holds at a time
// (delivery.intakeMaxAckPending).
const intakeSlots = 4

// requireTheEventsHeadWithinTheBound publishes poisonBefore envelopes no row can hold, then
// GitHub's own event of example/tools#7, and fails unless the results route compares check k1
// with #7's head within headAfterEventBound of that event's publish.
func requireTheEventsHeadWithinTheBound(t *testing.T, poisonBefore int) {
	t.Helper()
	handler, database, served, fake := readThroughGitHub(t, "example/tools")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "ACME", "name": "Acme"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project ACME: status=%d body=%s", response.Code, response.Body.String())
	}
	// The project's default is example/default, so the issue reaches example/tools by its own
	// repository alone: an issue resolves to its own repository, else to its project's default
	// (refs.IssueRepoResolved).
	setDefaultRepository(t, handler, "ACME", "example/default")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "ACME", "title": "The tools change", "spec": "A spec\n", "repo": "example/tools"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue in example/tools: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if issue.ResolvedRepo == nil || *issue.ResolvedRepo != "example/tools" {
		t.Fatalf("the issue resolves to %v, want its own example/tools, which is not its project's default", issue.ResolvedRepo)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if _, err := delivery.GetSettings(context.Background(), database.Pool); err == nil {
		t.Fatal("the deployment holds delivery settings, want none: the case is a deployment that never configured the delivery timeline")
	}

	publisher := startTheIntakes(t, database, served)

	updated := time.Now().UTC().Format(time.RFC3339)
	fake.pullAnswers(func(repo, number string) string {
		if repo != "example/tools" || number != "7" {
			return ""
		}
		return fmt.Sprintf(`{"number":7,"title":%q,"body":%q,"state":"open","merged":false,
			"html_url":"https://github.com/example/tools/pull/7","updated_at":%q,
			"head":{"ref":%q,"sha":%q,"repo":{"full_name":"example/tools"}},"base":{"ref":"main"}}`,
			"The tools change ("+issue.Key+")", "Dispatch: "+issue.Key+"\n\nThe change.", updated,
			"legion/"+issue.Key, headCommit)
	})
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "opened", "repo": "example/tools", "number": "7",
		"title":    "The tools change (" + issue.Key + ")",
		"body":     "Dispatch: " + issue.Key + "\n\nThe change.",
		"url":      "https://github.com/example/tools/pull/7",
		"author":   "implementer[bot]",
		"head_sha": headCommit, "head_ref": "legion/" + issue.Key, "head_repo": "example/tools",
		"base_ref": "main", "merged": "false", "updated_at": updated,
	})
	if err != nil {
		t.Fatalf("encode the pull request's payload: %v", err)
	}
	for i := range poisonBefore {
		number := fmt.Sprint(3_000_000_000 + i)
		poison, err := json.Marshal(map[string]string{
			"kind": "pr", "action": "opened", "repo": "example/tools", "number": number,
			"head_sha": headCommit, "head_ref": "feature/x", "base_ref": "main", "merged": "false", "updated_at": updated,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := publisher.Publish(contracts.Envelope{
			EventID: "example-tools-pr-" + number, Source: "github", SourceEventID: "example-tools-pr-" + number,
			TraceID: "example-tools-pr-" + number, Topic: contracts.GithubResourceSubject("example", "tools", "pr", number),
			Payload: string(poison), IssuedAt: time.Now().Unix(),
		}); err != nil {
			t.Fatalf("publish the envelope of #%s: %v", number, err)
		}
	}
	// The bound is v-head-no-settings' own: the results route compares a check with the head of
	// an open pull request within 5 seconds of GitHub's event, measured from the publish.
	published := time.Now()
	if err := publisher.Publish(contracts.Envelope{
		EventID: "example-tools-pr-7-opened", Source: "github", SourceEventID: "example-tools-pr-7-opened",
		TraceID: "example-tools-pr-7-opened", Topic: contracts.GithubResourceSubject("example", "tools", "pr", "7"),
		Payload: string(payload), IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish the pull request's opening: %v", err)
	}

	deadline := published.Add(headAfterEventBound)
	read := readResults(t, handler, issue.Key)
	for len(checkOf(t, read, "k1").ComparedWith) == 0 {
		if time.Now().After(deadline) {
			check := checkOf(t, read, "k1")
			t.Fatalf("%s after the pull request's event, check k1 compares with %+v for the reason %q and "+
				"the delivery durable is %s; want the head of example/tools#7, which the event reported",
				time.Since(published).Round(time.Millisecond), check.ComparedWith, check.ComparedNothing, durableState(publisher))
		}
		time.Sleep(100 * time.Millisecond)
		read = readResults(t, handler, issue.Key)
	}
	// The answer accepted is held to the bound too, and not only the answers that had no basis:
	// a route that answers right but late passes no bound.
	took := time.Since(published)
	if took > headAfterEventBound {
		t.Fatalf("the results route compared k1 with the event's head %s after the publish, past the %s of v-head-no-settings",
			took.Round(time.Millisecond), headAfterEventBound)
	}
	t.Logf("the results route compared k1 with the event's head %s after the publish", took.Round(time.Millisecond))
	stands := standsAt(t, read, "k1", recorded.Result.ID,
		"a result at the head of the pull request the event reported", model.CommitAtBasis, false)
	if stands.Basis == nil || stands.Basis.PullRequest == nil || *stands.Basis.PullRequest != 7 {
		t.Fatalf("the result stands at %+v, want the head of pull request 7", stands.Basis)
	}
	if stood := checkOf(t, read, "k1").PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("the check answers pass_stands %+v, want yes", stood)
	}
	if asked := fake.asks("example/tools"); asked > 2 {
		t.Fatalf("GitHub was asked %d times for the commit's pull requests, want the record's read-through and at most one read's: the head came from the event", asked)
	}
}

// durableState says what the NATS server holds of the delivery intake's durable, for a failure of
// the tests that start the intakes: absent, unbound or bound, so a red run names whether the intake
// ever took the event at all.
func durableState(client *bus.Client) string {
	info, err := client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return fmt.Sprintf("not on the server (%v)", err)
	}
	return fmt.Sprintf("bound=%v, delivered through stream sequence %d, acknowledged through %d",
		info.PushBound, info.Delivered.Stream, info.AckFloor.Stream)
}

// startTheIntakes starts the delivery intakes of served as the process starts them
// (delivery.StartIntakes), on a NATS server of the test's own, with database's pool, the intakes'
// callers of its lease pool and whatever delivery settings database holds (none unless the test
// wrote them), and answers a publisher that owns the stream: what it publishes is the intakes'
// input. It answers once the events intake has bound its durable: a first bind starts at the
// stream's next message (delivery's takeOverStart, from main's #5525), so an envelope published
// before it would never be delivered. The intakes stop, and then the publisher closes, when the
// test ends.
func startTheIntakes(t *testing.T, database *store.Store, served *server) *bus.Client {
	t.Helper()
	_, natsURL := testnats.Start(t)
	publisher, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	callers, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := delivery.ReserveIntakeLeases(callers)
	if err != nil {
		t.Fatalf("reserve the intakes' callers of the lease pool: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopIntakes, err := delivery.StartIntakes(ctx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, database.Pool, leases, served.deps.GitHub, served.deps.Events, nil, nil, served.deps.Attributor)
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopIntakes()
	})
	testwait.Until(t, "the delivery intake to bind its durable", func() (string, bool) {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return durableState(publisher), err == nil && info.PushBound
	})
	return publisher
}

// A pull request GitHub closed stops being the head a check CI proves is read at, within the same
// 5 seconds of GitHub's `pull_request` event, in a deployment that holds no delivery settings.
//
// The case is an issue in acme/widgets with one check CI proves and two pull requests Dispatch
// stores open: #5, the issue's own, at headCommit, where CI's run 41 passed, and #9, a negative
// control opened after it on a branch that names the issue, at newHeadCommit, where run 42 was
// cancelled. A result is derived at the head of each open pull request of the issue, and the
// check's `ci` is its read at the open head of the highest number (deriveCIResults, ciHeadsOf), so
// while #9 is stored open c1 reads #9's cancelled run, holds the one pass derived at #5's head, and
// answers pass_stands no: no pass stands at #9's head. GitHub then closes #9 without merging it,
// and its `closed` event is the one input that tells Dispatch so: GitHub names no pull request
// for any commit here and the reconcile is not running. The intake reads #9 from GitHub and
// writes it closed, a pull request closed unmerged is no head, and the read moves to #5, where the
// check holds the pass derived from run 41 and answers pass_stands yes.
//
// The deployment repository's #6070 and #6071 were held that way. GitHub closed both at 20:58Z
// on 2026-11-22. The deployment held no delivery settings, so no intake took their `closed`
// events, and Dispatch kept both stored open, and read the check at #6071's cancelled run, until
// the reconcile read them again at about 21:12:37Z.
//
// Red where the intake binds only under delivery settings, as at main before
// cybermonklabs/legion#4209 (Intake.Run waited for the settings cache to hold settings): the
// intake never binds its durable, so startTheIntakes's wait for that bind gives up at the test
// binary's deadline, and no event is ever taken.
func TestAPullRequestClosedEventMovesTheCIReadToTheOpenHeadWithNoDeliverySettings(t *testing.T) {
	t.Parallel()
	runs := newCheckRunsFake()
	runs.set(headCommit, completedRun(41, ciCheckRun, "success"))
	runs.set(newHeadCommit, completedRun(42, ciCheckRun, "cancelled"))
	// GitHub's answers name the issue and the moment GitHub closed #9: both are set below, before
	// anything reads a pull request from GitHub.
	var issueKey, branch, closedAt string
	handler, database, served := oneRepositoryGitHub(t, "acme/widgets", runs, func(number string) string {
		switch number {
		case "5":
			return fmt.Sprintf(`{"number":5,"title":%q,"body":"","state":"open","merged":false,"merged_at":null,
				"html_url":"https://github.com/acme/widgets/pull/5","updated_at":"2026-11-22T20:30:00Z",
				"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}`,
				"The change ("+issueKey+")", "legion/"+issueKey, headCommit)
		case "9":
			return fmt.Sprintf(`{"number":9,"title":"A negative control","body":"","state":"closed","merged":false,
				"merged_at":null,"closed_at":%q,"html_url":"https://github.com/acme/widgets/pull/9","updated_at":%q,
				"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}`,
				closedAt, closedAt, branch, newHeadCommit)
		}
		return ""
	})
	issue := issueInRepository(t, handler, "RESCE", "acme/widgets", "A negative control closed")
	issueKey = issue.Key
	// The branch names the issue in lower case, as production's negative-control branches did: a
	// key in a branch is read in either case.
	branch = "implementer/" + strings.ToLower(issue.Key) + "-breaks-b"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	if _, err := delivery.GetSettings(context.Background(), database.Pool); err == nil {
		t.Fatal("the deployment holds delivery settings, want none: production held none")
	}
	writeHead(t, database, "acme/widgets", 5, headCommit, "open", issue.Key)
	writeHead(t, database, "acme/widgets", 9, newHeadCommit, "open", issue.Key)
	if _, err := database.Pool.Exec(context.Background(),
		`update pull_request_heads set head_ref = $1 where repo = 'acme/widgets' and number = 9`, branch); err != nil {
		t.Fatalf("store #9 on its branch: %v", err)
	}
	storedState := func() string {
		var state string
		if err := database.Pool.QueryRow(context.Background(),
			`select state from pull_request_heads where repo = 'acme/widgets' and number = 9`).Scan(&state); err != nil {
			return fmt.Sprintf("unread (%v)", err)
		}
		return state
	}

	// Before the event the read is production's wrong one: #9 is stored open and has the higher
	// number, so c1 is read at #9's cancelled run, its one result is the pass derived at #5's
	// head, and that pass does not stand at #9's head.
	before := checkOf(t, readResults(t, handler, issue.Key), "c1")
	assertCI(t, before, ciCheckRun, model.CIStateNoResult, "cancelled", ciUnconcludedLineOf("c1", ciCheckRun, "cancelled"))
	if derived := assertDerived(t, before, 41, ciCheckRun, "success", "pass", headCommit); derived.CommitStands.Basis == nil ||
		derived.CommitStands.Basis.PullRequest == nil || *derived.CommitStands.Basis.PullRequest != 5 {
		t.Fatalf("before #9's closed event, the derived pass stands at %+v, want the head of #5", derived.CommitStands.Basis)
	}
	if stood := before.PassStands; stood.Answer != model.PassStandsNo {
		t.Fatalf("before #9's closed event, c1 answers pass_stands %+v, want no: run 42 at #9's head was cancelled", stood)
	}

	publisher := startTheIntakes(t, database, served)

	closedAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "repo": "acme/widgets", "number": "9",
		"title": "A negative control", "body": "",
		"url":      "https://github.com/acme/widgets/pull/9",
		"author":   "implementer[bot]",
		"head_sha": newHeadCommit, "head_ref": branch, "head_repo": "acme/widgets",
		"base_ref": "main", "merged": "false", "updated_at": closedAt,
	})
	if err != nil {
		t.Fatalf("encode the pull request's payload: %v", err)
	}
	// The bound and its measure are requireTheEventsHeadWithinTheBound's: v-head-no-settings' 5
	// seconds, from the publish.
	published := time.Now()
	if err := publisher.Publish(contracts.Envelope{
		EventID: "acme-widgets-pr-9-closed", Source: "github", SourceEventID: "acme-widgets-pr-9-closed",
		TraceID: "acme-widgets-pr-9-closed", Topic: contracts.GithubResourceSubject("acme", "widgets", "pr", "9"),
		Payload: string(payload), IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish the pull request's close: %v", err)
	}

	deadline := published.Add(headAfterEventBound)
	atHead := ciVerdictLineOf("c1", ciCheckRun, "pass", headCommit)
	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	for check.CI == nil || check.CI.Line != atHead {
		if time.Now().After(deadline) {
			reads := "no ci reading"
			if check.CI != nil {
				reads = fmt.Sprintf("case %q (%s)", check.CI.Case, check.CI.Detail)
			}
			t.Fatalf("%s after #9's closed event, c1 reads %s and holds %+v, #9 is stored %s and the delivery durable is %s; "+
				"want CI's pass of run 41 at %s, the head of acme/widgets#5: GitHub closed #9 unmerged, so its head is no head",
				time.Since(published).Round(time.Millisecond), reads, check.Results, storedState(), durableState(publisher), headCommit)
		}
		time.Sleep(100 * time.Millisecond)
		check = checkOf(t, readResults(t, handler, issue.Key), "c1")
	}
	// The answer accepted is held to the bound too, as requireTheEventsHeadWithinTheBound holds it.
	took := time.Since(published)
	if took > headAfterEventBound {
		t.Fatalf("the results route read c1 at #5's head %s after #9's closed event, past the %s of v-head-no-settings",
			took.Round(time.Millisecond), headAfterEventBound)
	}
	t.Logf("the results route read c1 at #5's head %s after #9's closed event, #9 now stored %s", took.Round(time.Millisecond), storedState())
	assertCI(t, check, ciCheckRun, model.CIStatePass, "success", atHead)
	result := assertDerived(t, check, 41, ciCheckRun, "success", "pass", headCommit)
	if basis := result.CommitStands.Basis; basis == nil || basis.PullRequest == nil || *basis.PullRequest != 5 {
		t.Fatalf("the derived result stands at %+v, want the head of #5", basis)
	}
	if stood := check.PassStands; stood.Answer != model.PassStandsYes {
		t.Fatalf("c1 answers pass_stands %+v, want yes", stood)
	}
}

// A pull request that closes unmerged stops being the head a check CI proves is read at on its
// `closed` event alone, where the issue reaches its pull requests by the links it holds and the
// read of the closing one was made before it closed.
//
// The case is production's: an issue in acme/widgets links two pull requests by their URLs, #5,
// open at headCommit, where CI's run 41 passed, and #9, a draft negative control opened after it
// at newHeadCommit, where run 42 failed. Neither's branch or texts name the issue, so the links
// alone make them the issue's. Dispatch read both from GitHub before #9 closed and stores both
// open, so the read of a check CI proves is at the open head of the highest number, #9, where c1
// fails, and no role can record a result to correct it (409 CHECK_PROVED_BY_CI). GitHub then
// closes #9 unmerged. Its `closed` event is the one envelope the stream ever holds and nothing
// else reads GitHub: GitHub names no pull request for any commit here and the reconcile is not
// running. Within v-head-no-settings' 5 seconds of that event c1 must read CI's pass at #5's head.
//
// Production held such a pair: the read stayed at the closed pull request's failure for the 25
// minutes from its close to the open one's merge, and the merge moved it, not the close. The
// subtests are a deployment with no delivery settings, as production was, and one with them, whose
// intake also takes the merges of the delivery timeline.
func TestAClosedEventAloneMovesTheCIReadOffALinkedPullRequestReadBeforeItClosed(t *testing.T) {
	t.Parallel()
	for _, withSettings := range []bool{false, true} {
		name := "no delivery settings"
		if withSettings {
			name = "delivery settings"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireTheCloseAloneMovesTheLinkedRead(t, withSettings)
		})
	}
}

// requireTheCloseAloneMovesTheLinkedRead is TestAClosedEventAloneMovesTheCIReadOffALinkedPullRequestReadBeforeItClosed's
// case, in a deployment with delivery settings when withSettings is true and with none otherwise.
func requireTheCloseAloneMovesTheLinkedRead(t *testing.T, withSettings bool) {
	t.Helper()
	runs := newCheckRunsFake()
	runs.set(headCommit, completedRun(41, ciCheckRun, "success"))
	runs.set(newHeadCommit, completedRun(42, ciCheckRun, "failure"))
	// GitHub's answer for #9 names the moment GitHub closed it, set below before the intakes start.
	var closedAt string
	handler, database, served := oneRepositoryGitHub(t, "acme/widgets", runs, func(number string) string {
		switch number {
		case "5":
			return fmt.Sprintf(`{"number":5,"title":"A change","body":"","state":"open","merged":false,"merged_at":null,
				"draft":false,"html_url":"https://github.com/acme/widgets/pull/5","updated_at":"2026-11-20T09:00:00Z",
				"head":{"ref":"topic","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}`, headCommit)
		case "9":
			return fmt.Sprintf(`{"number":9,"title":"A negative control","body":"","state":"closed","merged":false,
				"merged_at":null,"closed_at":%q,"draft":true,"html_url":"https://github.com/acme/widgets/pull/9","updated_at":%q,
				"head":{"ref":"control","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}`,
				closedAt, closedAt, newHeadCommit)
		}
		return ""
	})
	issue := issueInRepository(t, handler, "RCLOSE", "acme/widgets", "Two linked pull requests")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckKept))
	if withSettings {
		if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
			DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
			ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
			PopulationAuthors: []string{"octocat"},
		}, model.Actor{Kind: "system", ID: "test"}); err != nil {
			t.Fatalf("write the delivery settings: %v", err)
		}
	} else if _, err := delivery.GetSettings(context.Background(), database.Pool); err == nil {
		t.Fatal("the deployment holds delivery settings, want none: production held none")
	}
	// Both rows as a read of GitHub made before #9 closed stores them, with texts that name no
	// issue; the links are written after them, so neither link owes a read.
	storeLinkedHead(t, database, "acme/widgets", 5, headCommit)
	storeLinkedHead(t, database, "acme/widgets", 9, newHeadCommit)
	linkPullRequests(t, handler, issue.Key, "https://github.com/acme/widgets/pull/5", "https://github.com/acme/widgets/pull/9")
	storedState := func() string {
		var state string
		if err := database.Pool.QueryRow(context.Background(),
			`select state from pull_request_heads where repo = 'acme/widgets' and number = 9`).Scan(&state); err != nil {
			return fmt.Sprintf("unread (%v)", err)
		}
		return state
	}

	// Before the close the read is production's wrong one: c1 fails at #9's head.
	atControl := ciVerdictLineOf("c1", ciCheckRun, "fail", newHeadCommit)
	assertCI(t, checkOf(t, readResults(t, handler, issue.Key), "c1"), ciCheckRun, model.CIStateFail, "failure", atControl)

	publisher := startTheIntakes(t, database, served)
	closedAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "repo": "acme/widgets", "number": "9",
		"title": "A negative control", "body": "", "url": "https://github.com/acme/widgets/pull/9",
		"author": "implementer[bot]", "head_sha": newHeadCommit, "head_ref": "control", "head_repo": "acme/widgets",
		"base_ref": "main", "merged": "false", "updated_at": closedAt,
	})
	if err != nil {
		t.Fatalf("encode the pull request's payload: %v", err)
	}
	published := time.Now()
	if err := publisher.Publish(contracts.Envelope{
		EventID: "acme-widgets-pr-9-closed", Source: "github", SourceEventID: "acme-widgets-pr-9-closed",
		TraceID: "acme-widgets-pr-9-closed", Topic: contracts.GithubResourceSubject("acme", "widgets", "pr", "9"),
		Payload: string(payload), IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish the pull request's close: %v", err)
	}

	deadline := published.Add(headAfterEventBound)
	atHead := ciVerdictLineOf("c1", ciCheckRun, "pass", headCommit)
	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	for check.CI == nil || check.CI.Line != atHead {
		if time.Now().After(deadline) {
			reads := "no ci reading"
			if check.CI != nil {
				reads = fmt.Sprintf("case %q (%s)", check.CI.Case, check.CI.Detail)
			}
			t.Fatalf("%s after #9's closed event, c1 reads %s, #9 is stored %s and the delivery durable is %s; "+
				"want CI's pass of run 41 at %s, the head of the linked acme/widgets#5: GitHub closed #9 unmerged, so its head is no head",
				time.Since(published).Round(time.Millisecond), reads, storedState(), durableState(publisher), headCommit)
		}
		time.Sleep(100 * time.Millisecond)
		check = checkOf(t, readResults(t, handler, issue.Key), "c1")
	}
	if took := time.Since(published); took > headAfterEventBound {
		t.Fatalf("the results route read c1 at #5's head %s after #9's closed event, past the %s of v-head-no-settings",
			took.Round(time.Millisecond), headAfterEventBound)
	}
	// The close moved the read, and nothing else did: the stream holds that one envelope.
	info, err := publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("the stream holds %d envelopes, want only #9's close", info.State.Msgs)
	}
	assertCI(t, check, ciCheckRun, model.CIStatePass, "success", atHead)
	result := assertDerived(t, check, 41, ciCheckRun, "success", "pass", headCommit)
	if basis := result.CommitStands.Basis; basis == nil || basis.PullRequest == nil || *basis.PullRequest != 5 {
		t.Fatalf("the derived result stands at %+v, want the head of #5", basis)
	}
	if state := storedState(); state != "closed" {
		t.Fatalf("#9 is stored %s, want closed, as GitHub answered", state)
	}
}
