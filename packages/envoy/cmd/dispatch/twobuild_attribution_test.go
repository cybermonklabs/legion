//go:build twobuild

// The attribution row of the two-build suite: both builds name one issue for a pull request from
// one set of facts, through a backlog and a stop of this tree's build. The stand-in GitHub that
// both builds read is in twobuild_attribution_fake_test.go, and the gate on the delivery reconcile
// and the reads and writes that each step checks are in twobuild_attribution_harness_test.go
// (twobuild_harness_test.go holds the harness that starts the builds).

package main

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// ───── one pull request's issue, read by both builds ─────────────────────────

// The fixture of TestTwoBuildsAttributeAPullRequestFromOneSetOfFacts. Project TB has the
// repository attributionRepo, which holds pull request #81. Project ACME has backlogRepo, which
// holds the rows of steps 5 and 6. The stand-in gives each repository its own installation, so
// each has its own budget of facts queries in a pass (delivery.FactsQueriesPerPass).
const (
	attributionRepo   = "acme/widgets"
	backlogRepo       = "acme/backlog"
	attributionNumber = 81
	// backlogFirst and backlogRows are the backlog of step 5: merged rows with texts.
	backlogFirst, backlogRows = 1001, 1200
	// laterFirst and laterRows are the rows of step 6: merged rows with no facts.
	laterFirst, laterRows = 3001, 600
	// attributionCheck is the Verification document of each issue of the row: one check that a
	// result before the merge answers.
	attributionCheck = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe head of the issue's pull request passes.\n:::\n"
	// headPassLine is the message of the line that the head pass logs once in each pass
	// (delivery.Reconcile.reconcilePullRequestHeads), with its counts.
	headPassLine = `msg="dispatch delivery: pull request attribution"`
)

var (
	// firstHead and secondHead are the head of #81 before and after the move of step 4, and
	// firstUpdate and secondUpdate its update times.
	firstHead, secondHead     = strings.Repeat("c1", 20), strings.Repeat("c2", 20)
	firstUpdate, secondUpdate = time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	// backlogUpdate is the update time of each row of steps 5 and 6.
	backlogUpdate = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	// attributionInstallations is the installation of each repository that the App is installed
	// on.
	attributionInstallations = map[string]int{attributionRepo: 1, backlogRepo: 2}
	// attributionFactsAlias reads the aliases of the facts query (delivery's factsQuery) in their
	// order: a repository alias with the names of its two variables, or a pull request alias with
	// its number.
	attributionFactsAlias = regexp.MustCompile(`(r\d+): repository\(owner: \$(\w+), name: \$(\w+)\)|(p\d+): pullRequest\(number: (\d+)\)`)
	// attributionLogAttribute is one key=value of a slog text line, its value quoted or bare.
	attributionLogAttribute = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)
)

// Two builds on one database give a pull request one issue from one set of facts, through a
// backlog and a stop of this tree's build. The previous build reads the issue that a results row
// stores. This tree's build reads the statement over the stored facts, and its pass writes that
// answer to each stored row.
//
// The test holds the delivery reconcile's lease (reconcileGate), so no pass runs until the test
// lets exactly one run. It records the previous build's period as served, so the previous build
// runs no pass in the test (holdThePreviousBuildsPasses). This tree's build runs at 2 s, and its
// first tick comes at its start (DISPATCH_TEST_RECONCILE_AT_START=1).
//
//  1. Issues TB-1 and TB-2 have a check. Open pull request #81 has the branch work/no-key, the
//     title "TB-1: the change" and the head C1. The previous build records a result of TB-1 at
//     C1. Its read-through writes #81 with no texts and with its verdict TB-1.
//  2. Before a pass, this tree's build compares TB-1 with #81: R4, the stored answer.
//  3. After one pass, #81 has texts. Both builds compare TB-1 with #81.
//  4. The stand-in moves #81 to head C2, the title "TB-2: the change" and a later update time. The
//     previous build records a result of TB-2 at C2, and writes the new head and its verdict and no
//     texts. This tree's build compares TB-2 with #81 at once, because texts of C1 are no facts of
//     C2. After one pass, #81 has texts of C2, and the answer stays TB-2.
//  5. The backlog: 1,200 rows of backlogRepo with texts, each storing an older answer. After one
//     pass, the previous build gives the statement's answer for each of them, each side of the
//     moves carries 500 events, and 700 moves stay queued.
//  6. The stop: 600 merged rows with no facts are stored after the pass of step 5. Then this tree's
//     build gets SIGTERM, and no pass runs between that pass and the stop. The previous build
//     alone then gives, for each row, the issue that this tree's build gave before the stop. The
//     queue keeps its rows. After the first pass of the restarted build, the queue holds 500
//     fewer moves, and the facts line has read 500 of the 600. Each answer is the statement's.
//
// The control of step 4 is a build of this tree whose statement reads texts of another head as
// facts (ENVOY_TEST_GO_OVERLAY). It fails the test: this tree's build then gives TB-1 from the
// texts of C1.
func TestTwoBuildsAttributeAPullRequestFromOneSetOfFacts(t *testing.T) {
	builds := newTwoBuilds(t)
	database := builds.database
	gate := closedReconcileGate(t, database)
	pin := holdThePreviousBuildsPasses(t, database)
	fake := newAttributionFake()
	fake.put(attributionRepo, fakePull{number: attributionNumber, head: firstHead, branch: "work/no-key", title: "TB-1: the change",
		open: true, updatedAt: firstUpdate, commits: []string{firstHead}})
	app := githubAppEnv(t, "")
	previousEnv, currentEnv := maps.Clone(app), maps.Clone(app)
	previousEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.previousName())
	currentEnv["DISPATCH_GITHUB_API_BASE"] = fake.serve(t, builds.currentName())
	currentEnv["DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL"] = (2 * time.Second).String()
	currentEnv["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	previous := builds.startPrevious(t, previousEnv)
	current := builds.startCurrent(t, currentEnv)
	if err := delivery.PrepareAttribution(context.Background(), database.Pool); err != nil {
		t.Fatalf("build the statement in the test's process: %v", err)
	}
	current.createProject(t, "TB", "Two builds")
	mapRepositoryTo(t, current, attributionRepo, "TB")
	issueWithCheck(t, current, "TB", "The first issue", "TB-1")
	issueWithCheck(t, current, "TB", "The second issue", "TB-2")
	current.createProject(t, "ACME", "The backlog")
	mapRepositoryTo(t, current, backlogRepo, "ACME")
	issueWithCheck(t, current, "ACME", "The older answer", "ACME-1")
	issueWithCheck(t, current, "ACME", "The rule's answer", "ACME-2")
	previousName, currentName := builds.previousName(), builds.currentName()

	// 1. The previous build's read-through writes #81 with its verdict and no texts.
	recordPass(t, previous, "TB-1", firstHead)
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != firstHead || issue != "TB-1" || texts != "" {
		t.Fatalf("step 1: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-1 and no texts", head, issue, texts, firstHead)
	}
	wantBases(t, previous, previousName, "TB-1", []int{attributionNumber}, "step 1")

	// 2. Before any pass, this tree's build gives the stored answer (R4).
	if passes := current.logLines(headPassLine); len(passes) != 0 {
		t.Fatalf("step 2: the %s ran %d passes while the test held the lease:\n%s", currentName, len(passes), strings.Join(passes, "\n"))
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-1", false, "step 2")
	wantBases(t, current, currentName, "TB-1", []int{attributionNumber}, "step 2")
	wantBases(t, current, currentName, "TB-2", nil, "step 2")

	// 3. One pass reads the facts of #81. Both builds give TB-1.
	gate.onePass(t, current, "step 3")
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != firstHead || issue != "TB-1" || texts != firstHead {
		t.Fatalf("step 3: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-1 and texts of %s", head, issue, texts, firstHead, firstHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-1", true, "step 3")
	for _, p := range []*dispatchProcess{current, previous} {
		wantBases(t, p, builds.buildName(p), "TB-1", []int{attributionNumber}, "step 3")
	}

	// 4. The move. The previous build writes the new head and its verdict, and no texts.
	fake.move(attributionRepo, attributionNumber, secondHead, "TB-2: the change", secondUpdate)
	recordPass(t, previous, "TB-2", secondHead)
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != secondHead || issue != "TB-2" || texts != firstHead {
		t.Fatalf("step 4: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-2 and texts of %s", head, issue, texts, secondHead, firstHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-2", false, "step 4, before the pass")
	// Only TB-2 is read here. A result of TB-1 is at C1, which heads no row now, so a read of
	// TB-1 reads #81 through from GitHub and stores texts of C2 before the pass.
	if bases := basesOf(t, current, "TB-2"); bases[attributionNumber] != secondHead {
		t.Fatalf("step 4, before the pass: the %s compares TB-2 with %v, want #81 at %s: it gives TB-1 from the texts of %s, which are no facts of %s (it compares TB-1 with %v)",
			currentName, bases, secondHead, firstHead, secondHead, basesOf(t, current, "TB-1"))
	}
	gate.onePass(t, current, "step 4")
	if head, issue, texts := storedPullRequest(t, database, attributionRepo, attributionNumber); head != secondHead || issue != "TB-2" || texts != secondHead {
		t.Fatalf("step 4, after the pass: #81 stores the head %s, the issue %s and texts of %q, want %s, TB-2 and texts of %s", head, issue, texts, secondHead, secondHead)
	}
	wantStatement(t, statementAnswers(t, database), attributionNumber, "TB-2", true, "step 4, after the pass")
	for _, p := range []*dispatchProcess{current, previous} {
		wantBases(t, p, builds.buildName(p), "TB-2", []int{attributionNumber}, "step 4, after the pass")
		wantBases(t, p, builds.buildName(p), "TB-1", nil, "step 4, after the pass")
	}

	// 5. The backlog. Before the pass, the previous build gives each row's older answer: the
	// reader can fail. One pass writes the statement's answer to each row.
	storeBacklog(t, database, fake)
	backlog := numbersFrom(backlogFirst, backlogRows)
	wantBases(t, previous, previousName, "ACME-1", backlog, "step 5, before the pass")
	wantBases(t, previous, previousName, "ACME-2", nil, "step 5, before the pass")
	wantPassCounts(t, "step 5", gate.onePass(t, current, "step 5"),
		map[string]int{"facts_left_before": 0, "changes": backlogRows, "events": 2 * delivery.AttributionMovesPerPass, "moves_queued": backlogRows - delivery.AttributionMovesPerPass})
	answers := statementAnswers(t, database)
	if got := heldBy(answers, "ACME-2"); !slices.Equal(got, backlog) {
		t.Fatalf("step 5: the statement gives ACME-2 to %d pull requests, want the %d of the backlog", len(got), backlogRows)
	}
	wantBases(t, previous, previousName, "ACME-2", heldBy(answers, "ACME-2"), "step 5")
	wantBases(t, previous, previousName, "ACME-1", heldBy(answers, "ACME-1"), "step 5")
	wantStoredIsStatement(t, database, answers, "step 5")
	for _, issue := range []string{"ACME-1", "ACME-2"} {
		if events := attributionEventsOn(t, database, issue); events != delivery.AttributionMovesPerPass {
			t.Errorf("step 5: %s carries %d attribution events, want %d", issue, events, delivery.AttributionMovesPerPass)
		}
	}
	if queued := queuedMoves(t, database); queued != backlogRows-delivery.AttributionMovesPerPass {
		t.Fatalf("step 5: %d moves are queued after one pass, want %d", queued, backlogRows-delivery.AttributionMovesPerPass)
	}

	// 6. The stop, while moves are queued and facts are due, and while no pass runs.
	storeLaterRows(t, database, fake)
	issues := []string{"TB-1", "TB-2", "ACME-1", "ACME-2"}
	answers = statementAnswers(t, database)
	gave := map[string]map[int]string{}
	for _, issue := range issues {
		gave[issue] = basesOf(t, current, issue)
		wantBases(t, current, currentName, issue, heldBy(answers, issue), "step 6, before the stop")
	}
	queued := queuedMoves(t, database)
	// The stop of a deploy: SIGTERM, and the graceful shutdown (stop). No pass runs, because the
	// test holds the lease. dispatchProcess.signal is in the multiprocess build alone, so the
	// harness's stop sends the signal.
	stop(t, current)
	t.Logf("step 6: the %s exited with code %d after SIGTERM", currentName, current.Cmd.ProcessState.ExitCode())
	if after := queuedMoves(t, database); after != queued || queued != backlogRows-delivery.AttributionMovesPerPass {
		t.Errorf("step 6: %d moves were queued before the stop and %d after it, want %d both times", queued, after, backlogRows-delivery.AttributionMovesPerPass)
	}
	for _, issue := range issues {
		wantBases(t, previous, previousName, issue, slices.Sorted(maps.Keys(gave[issue])), "step 6, after the stop: what the "+currentName+" gave before it")
	}
	wantStoredIsStatement(t, database, answers, "step 6, after the stop")
	current = builds.startCurrent(t, currentEnv)
	read := func() int {
		return attributionCount(t, database, "the texts of the rows with no facts",
			`select count(*) from pull_request_texts where repo = $1 and number between $2::int and $2::int + $3::int - 1`, backlogRepo, laterFirst, laterRows)
	}
	factsRead := delivery.FactsQueriesPerPass * delivery.FactsBatchSize
	askedBefore := len(fake.askedOf(currentName))
	wantPassCounts(t, "step 6, the first pass after the restart", gate.onePass(t, current, "step 6"), map[string]int{
		"facts_left_before": laterRows, "facts_left": laterRows - factsRead, "changes": 0,
		"events": 2 * delivery.AttributionMovesPerPass, "moves_queued": queued - delivery.AttributionMovesPerPass,
	})
	if texts := read(); texts != factsRead {
		t.Errorf("step 6: after the first pass of the restarted build %d of the %d rows with no facts have texts, want %d", texts, laterRows, factsRead)
	}
	asked := map[int]bool{}
	for _, number := range fake.askedOf(currentName)[askedBefore:] {
		if number >= laterFirst && number < laterFirst+laterRows {
			asked[number] = true
		}
	}
	if len(asked) != factsRead {
		t.Errorf("step 6: the first pass of the restarted build asked GitHub the facts of %d of the %d rows with no facts, want %d", len(asked), laterRows, factsRead)
	}
	if after := queuedMoves(t, database); after != queued-delivery.AttributionMovesPerPass {
		t.Errorf("step 6: after the first pass of the restarted build %d moves are queued, want %d: %d fewer than at the stop", after, queued-delivery.AttributionMovesPerPass, delivery.AttributionMovesPerPass)
	}
	answers = statementAnswers(t, database)
	for number, answer := range answers {
		if !answer.Known {
			t.Errorf("step 6: the statement answers #%d %s, want a known answer", number, describeAnswer(answer, true))
		}
	}
	for _, issue := range issues {
		for _, p := range []*dispatchProcess{current, previous} {
			wantBases(t, p, builds.buildName(p), issue, heldBy(answers, issue), "step 6, after the first pass of the restarted build")
		}
	}
	wantStoredIsStatement(t, database, answers, "step 6, after the first pass of the restarted build")
	for _, issue := range []string{"ACME-1", "ACME-2"} {
		if events := attributionEventsOn(t, database, issue); events != 2*delivery.AttributionMovesPerPass {
			t.Errorf("step 6: %s carries %d attribution events, want %d", issue, events, 2*delivery.AttributionMovesPerPass)
		}
	}

	// The previous build ran no pass: its record of its period is the test's, and it read no pull
	// request by its number, as its head pass reads an open row again.
	var boundary time.Time
	if err := database.Pool.QueryRow(context.Background(), `select boundary from duty_passes where lease = $1`, pin).Scan(&boundary); err != nil || boundary.Year() != 2999 {
		t.Errorf("the record of the previous build's period holds %v (%v), want the test's of the year 2999: the previous build ran a pass", boundary, err)
	}
	for _, name := range []string{previousName, currentName} {
		reads, unexpected := fake.readsOf(name)
		t.Logf("the GitHub reads of the %s: %v; paths no route answers: %v", name, reads, unexpected)
	}
	if reads, _ := fake.readsOf(previousName); reads["pull"] > 0 {
		t.Errorf("the %s read pull requests by their number (%v): a pass of it ran in the test", previousName, reads)
	}
	t.Logf("the pull requests that the facts queries of the %s asked: %d", currentName, len(fake.askedOf(currentName)))
}
