package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// previousReleaseCommit is the commit a process of the release before this one registers under,
// and nextBuildCommit the commit of a later build of this code: any commits but this build's own,
// which the test's service leaves empty, as an unstamped build's.
const (
	previousReleaseCommit = "1111111111111111111111111111111111111111"
	nextBuildCommit       = "2222222222222222222222222222222222222222"
)

// A version the release before this one writes during a roll, after this build's drain has
// cleared its document's mark, reaches the store, and the census reaches zero with no second
// backfill.
//
// The backfill marks each document it backfills with this build's own mark (store_owed_at), and
// this build's drain states every block of the document and clears the mark. A document that the
// release before this one edits after that holds no mark: that release writes the version's rows
// with no node, states nothing in the store and never sets the mark, so nothing would bring this
// build's pass to the document, and the census would refuse its rows until someone ran the
// backfill again, which nothing tells anyone to do. A document that release creates after the
// drain is the same: no backfill reached it, and its rows name no node. So every tick of this
// build, whether or not another build serves, marks owed every document that an artifact.version
// or artifact.created event after this build's cursor names and that its store has not stated at
// its version (readEventsAfterCursor), and the same tick's drain names every row's node. Once the
// fleet serves this build alone, one more tick marks what the last one left, and the tick after
// it reads no event at all, since none has been written since.
//
// The test seeds two documents as a build before the store wrote them, backfills, and drives this
// build's reconciliation until no document holds the mark. It then records the fleet as two
// builds: a live server_processes row of this build's commit and one of another, each through
// the registry's own insert path (processes.Register). The stand-in for the release before
// (indexAsThePreviousRelease) writes and indexes a new version of the first document, appending
// its artifact.version event as that release does, creates a third document as that release's
// upload does (createAsThePreviousRelease), appending its artifact.created event, and creates an
// issue with its spec as that release's issue create does (createIssueAsThePreviousRelease),
// appending its issue.created event, which names the issue and no document. This build's next
// tick must mark the three documents from those events and drain them. The stand-in then
// writes a version of the second document, and that release's process leaves the registry: the
// next tick finds this build serving alone and must still mark the second document, once, and the
// tick after it must read no event. An event written after that, which names no document, is read
// by the next tick, which marks nothing: each tick reads exactly the events since the last one.
//
// Each tick under test runs once the event log's safe horizon has reached every event already
// written (awaitedBlockIndexTick), so it reads every event the test wrote before it. A tick of a
// server reads such an event a tick or two later, once the Horizon has proven an observation that
// saw it.
func TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	// The log holds an event before this build first boots, as every deployed database's log
	// does, so the cursor starts above it rather than at an empty log.
	appendIssueEvent(t, database)
	slugs := []string{"rolled", "late"}
	documents := map[string]string{}
	for _, slug := range slugs {
		documents[slug] = createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
		settle(t, database, documents[slug])
	}
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-version-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-version-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	// The documents as a build before the store wrote them: their rows, and nothing of the store.
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Owed != int64(len(slugs)) || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v for the blocks, want the %d documents marked and the cutover complete", blocks, len(slugs))
	}
	ticks := drainBlockIndex(t, service, "the backfill's mark")
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) still hold this build's mark after %d tick(s) of its drain, want none", marked, ticks)
	}
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark, before any version of the previous release")

	// This build serving alone: the tick reads no event, since its drain's ticks have read every
	// one already.
	tick := awaitedBlockIndexTick(t, service)
	if tick.AnotherBuild || tick.Events != 0 || tick.Marked != 0 {
		t.Errorf("the tick with this build serving alone read %d event(s), marked %d document(s) and found another build %t, want none of them",
			tick.Events, tick.Marked, tick.AnotherBuild)
	}
	cursor := blockIndexEventCursor(t, database, "")

	// A roll: a process of this build and one of the release before serve side by side.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)

	// The release before this one edits the first document, whose mark this build's drain has
	// cleared: its index pass writes the new link's row naming no node, and no mark. It also
	// creates a document after the drain, and an issue with its spec, whose rows all name no node.
	rolled := documents["rolled"]
	added := moveLinkAsThePreviousRelease(t, database, service, rolled, "rolled")
	rolledEvent := latestVersionEvent(t, database, rolled)
	created, createdLink, createdEvent := createAsThePreviousRelease(t, database, service, "created", added)
	spec, specLink, issueEvent := createIssueAsThePreviousRelease(t, database, service, "CORE-3", 3, added)
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) hold this build's mark after the previous release's version, new document and new issue, want none: this test proves nothing", marked)
	}
	for _, document := range []string{rolled, created, spec} {
		if blocks, links := rowsNamingNoNode(t, database, document); blocks+links == 0 {
			t.Fatalf("every row the previous release wrote of document %s names its node, so this test proves nothing", document)
		}
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over what the previous release indexed, so this test proves nothing")
	}

	// This build's next tick marks the three documents from their events, and its drain states
	// them.
	newest := max(rolledEvent, createdEvent, issueEvent)
	pending := countEventsAfter(t, database, cursor)
	tick = awaitedBlockIndexTick(t, service)
	if !tick.AnotherBuild || pending == 0 || tick.Events != pending || tick.Marked != 3 || tick.Documents != 3 {
		t.Errorf("the tick with another build serving read %d event(s), marked %d document(s), indexed %d and found another build %t; want the %d event(s) after the cursor read, the three documents marked and indexed, and another build found",
			tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild, pending)
	}
	if cursor = blockIndexEventCursor(t, database, ""); cursor < newest {
		t.Errorf("the cursor stands at %d after the tick, want it at or past the newest of the previous release's events, %d",
			cursor, newest)
	}
	requireVersionInTheStore(t, database, rolled, added)
	requireVersionInTheStore(t, database, created, createdLink)
	requireVersionInTheStore(t, database, spec, specLink)
	requireCensusReadsZero(t, database, "after this build's next tick with the previous release serving, with no second backfill")

	// The release before edits the second document, and its process then leaves the fleet.
	late := documents["late"]
	lateAdded := moveLinkAsThePreviousRelease(t, database, service, late, "late")
	lateEvent := latestVersionEvent(t, database, late)
	previousLeaves()

	// The first tick that finds this build serving alone reads the events after the cursor, as
	// every tick does, and marks what the tick before it left.
	pending = countEventsAfter(t, database, cursor)
	tick = awaitedBlockIndexTick(t, service)
	if tick.AnotherBuild || pending == 0 || tick.Events != pending || tick.Marked != 1 || tick.Documents != 1 {
		t.Errorf("the first tick with this build serving alone read %d event(s), marked %d document(s), indexed %d and found another build %t; want the %d event(s) after the cursor read, the one document marked and indexed, and no other build",
			tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild, pending)
	}
	if cursor = blockIndexEventCursor(t, database, ""); cursor < lateEvent {
		t.Errorf("the cursor stands at %d after the tick, want it at or past the version's event %d", cursor, lateEvent)
	}
	requireVersionInTheStore(t, database, late, lateAdded)
	requireCensusReadsZero(t, database, "after this build's first tick serving alone, with no second backfill")

	// The tick after it reads no event: none has been written since.
	tick = awaitedBlockIndexTick(t, service)
	if tick.AnotherBuild || tick.Events != 0 || tick.Marked != 0 {
		t.Errorf("the second tick with this build serving alone read %d event(s), marked %d document(s) and found another build %t, want none of them",
			tick.Events, tick.Marked, tick.AnotherBuild)
	}

	// An event written after it, naming no document, is read by the next tick, which marks
	// nothing and moves the cursor past it.
	appended := appendIssueEvent(t, database)
	tick = awaitedBlockIndexTick(t, service)
	if tick.AnotherBuild || tick.Events != 1 || tick.Marked != 0 {
		t.Errorf("the tick after an event naming no document read %d event(s), marked %d document(s) and found another build %t, want that one event read and nothing marked",
			tick.Events, tick.Marked, tick.AnotherBuild)
	}
	if cursor = blockIndexEventCursor(t, database, ""); cursor < appended {
		t.Errorf("the cursor stands at %d after the tick, want it moved past event %d", cursor, appended)
	}
}

// A version the release before this one writes while no process of this build runs - after a
// rollback that stops this build before it starts that release, and before the roll forward that
// stops that release before it starts this build again - reaches the store, and the census
// reaches zero with no second backfill.
//
// Neither end of such a deploy has two builds serving at once, so no tick of this build ever
// finds another build serving: its last tick before the rollback found it alone, and so does the
// first tick after it starts again. Were finding another build a reason to read the event log,
// that first tick would read nothing and move the cursor to the horizon over the version the
// previous release wrote, whose rows name no node, and nothing would reach them. Every tick reads
// the events after its build's cursor instead (readEventsAfterCursor), so the first tick of the
// new process reads from where its build's cursor stood through the newest event, and the process
// here is a new Service of the same commit, as the same build returns.
func TestAVersionThePreviousReleaseWritesWhileThisBuildIsStoppedReachesTheStore(t *testing.T) {
	requireAVersionWrittenWhileNoBuildOfThisCodeServesReachesTheStore(t, "")
}

// A version the release before this one writes while no process of a build of this code runs
// reaches the store when the roll forward starts a later build of this code instead of the one
// that stopped, and the census reaches zero with no second backfill.
//
// The later build holds no row of block_index_event_cursors yet. Were its row to start at the
// tick's horizon, as a build's does where no build of its marking rule has left a row and no
// backfill of its rule has recorded one, it would start above the version the previous release
// wrote while it served alone. Both builds are of this code's marking rule, so its first tick with
// a proven horizon starts the row at the newest cursor a build of that rule left, where the build
// before it stopped reading (readEventsAfterCursor), and reads from there through the newest
// event.
func TestAVersionThePreviousReleaseWritesBeforeTheNextBuildStartsReachesTheStore(t *testing.T) {
	requireAVersionWrittenWhileNoBuildOfThisCodeServesReachesTheStore(t, nextBuildCommit)
}

// requireAVersionWrittenWhileNoBuildOfThisCodeServesReachesTheStore is the deploy both tests above
// drive: an unstamped build of this code seeds, backfills and drains one document and ticks once
// with the fleet serving it alone, and its process stops. The release before serves alone and the
// stand-in writes a version of the document with no tick of this code in between
// (moveLinkAsThePreviousRelease, through the stopped service's room, which is only how the edit
// reaches the store), and that release's process stops too. A new process of the build of started
// then serves alone, and its first tick must mark the document and drain it, and the tick after
// that must read the one event written since, which names no document, and mark nothing.
func requireAVersionWrittenWhileNoBuildOfThisCodeServesReachesTheStore(t *testing.T, started string) {
	database, stopped := blockIndexService(t)
	ctx := context.Background()

	// The log holds an event before this build first boots, as every deployed database's log does.
	appendIssueEvent(t, database)
	const slug = "away"
	document := createOwedDocument(t, database, stopped, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-away-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-away-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, stopped, "seed")
	takeTheStoreOff(t, database)
	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	if blocks := backfilledKind(t, report, refs.BlockNodeKind); blocks.Owed != 1 || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v for the blocks, want the one document marked and the cutover complete", blocks)
	}
	drainBlockIndex(t, stopped, "the backfill's mark")
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) still hold this build's mark after its drain, want none", marked)
	}
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark, before the rollback")

	// This build's last tick before the rollback finds it serving alone.
	thisStops := registerProcess(t, database, "")
	tick := awaitedBlockIndexTick(t, stopped)
	if tick.AnotherBuild || tick.Events != 0 || tick.Marked != 0 {
		t.Errorf("the last tick before the rollback read %d event(s), marked %d document(s) and found another build %t, want none of them",
			tick.Events, tick.Marked, tick.AnotherBuild)
	}
	left := blockIndexEventCursor(t, database, "")
	thisStops()

	// The rollback: the release before this one serves alone and edits the document, and the roll
	// forward stops its process before it starts the next one of this code.
	previousStops := registerProcess(t, database, previousReleaseCommit)
	added := moveLinkAsThePreviousRelease(t, database, stopped, document, slug)
	event := latestVersionEvent(t, database, document)
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) hold this build's mark after the previous release's version, want none: this test proves nothing", marked)
	}
	if blocks, links := rowsNamingNoNode(t, database, document); blocks+links == 0 {
		t.Fatalf("every row of the previous release's version names its node, so this test proves nothing")
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over the version the previous release indexed, so this test proves nothing")
	}
	previousStops()
	process := blockIndexProcess(t, database, started)
	registerProcess(t, database, started)

	// The new process's first tick finds its build serving alone, reads from where its cursor
	// starts - the stopped build's own row for the same build, and for a later build of the same
	// marking rule the newest row a build of that rule left, which is the same row - through the
	// newest event, marks the document and drains it.
	start := left
	pending := countEventsAfter(t, database, start)
	tick = awaitedBlockIndexTick(t, process)
	if tick.AnotherBuild || pending == 0 || tick.Events != pending || tick.Marked != 1 || tick.Documents != 1 {
		t.Errorf("the new process's first tick read %d event(s), marked %d document(s), indexed %d and found another build %t; want the %d event(s) after event %d read (the stopped build's cursor %d), the one document marked and indexed, and no other build",
			tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild, pending, start, left)
	}
	if cursor := blockIndexEventCursor(t, database, started); cursor < event {
		t.Errorf("the cursor of build %q stands at %d after its first tick, want it at or past the version's event %d",
			started, cursor, event)
	}
	requireVersionInTheStore(t, database, document, added)
	requireCensusReadsZero(t, database, "after the first tick of the process started after the rollback, with no second backfill")

	// The tick after it reads the one event written since, which names no document, marks
	// nothing, and moves the cursor past it.
	appended := appendIssueEvent(t, database)
	tick = awaitedBlockIndexTick(t, process)
	if tick.AnotherBuild || tick.Events != 1 || tick.Marked != 0 {
		t.Errorf("the new process's second tick read %d event(s), marked %d document(s) and found another build %t, want the one event written since read and nothing marked",
			tick.Events, tick.Marked, tick.AnotherBuild)
	}
	if cursor := blockIndexEventCursor(t, database, started); cursor < appended {
		t.Errorf("the cursor of build %q stands at %d after the second tick, want it moved past event %d",
			started, cursor, appended)
	}
}

// A second build of this code's marking rule, with no event cursor of its own, starts reading the
// event log at the cursor the first build left, and reads no event at or below it, however far
// below it the backfill's horizon stands.
//
// The first build read every event below its cursor and marked, with the mark this build drains,
// every document they name that their marking rule owes, and gave every comment they name its
// node. A build of the same rule owes exactly that (R13), so a second build that started at the
// backfill's horizon instead would read every one of those events again, on every deploy that
// keeps the rule, as a consumer of a log takes up its group's committed offset and not the oldest
// one it could start from. The test backfills, has the release before this one edit the
// backfilled document and write a comment, which the first build's ticks read, mark, drain and
// give its node, so its cursor stands above the horizon with those events between the two, and
// then runs the first tick of a second build, which holds no row.
func TestASecondBuildsFirstTickReadsNoEventBelowTheCursorAnotherBuildLeft(t *testing.T) {
	database, first := blockIndexService(t)
	const slug = "second"
	document := backfilledDocument(t, database, first, slug)
	horizon := recordedBackfillHorizon(t, database, first.markingRule)

	added := moveLinkAsThePreviousRelease(t, database, first, document, slug)
	comment, _ := commentAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "Alice"}, "", "A remark the previous release writes after the backfill.")
	tick := awaitedBlockIndexTick(t, first)
	if tick.Marked != 1 || tick.Comments != 1 || tick.Documents != 1 {
		t.Fatalf("the first build's tick marked %d document(s), wrote %d comment(s) and indexed %d document(s), want the one of each the previous release wrote: this test proves nothing",
			tick.Marked, tick.Comments, tick.Documents)
	}
	requireVersionInTheStore(t, database, document, added)
	if !commentNamesItsNode(t, database, comment) {
		t.Fatalf("the first build's tick left comment %s naming no node", comment)
	}
	cursor := blockIndexEventCursor(t, database, "")
	between := countEventsAfter(t, database, horizon) - countEventsAfter(t, database, cursor)
	if cursor <= horizon || between == 0 {
		t.Fatalf("the first build's cursor %d stands at or below the backfill's horizon %d with %d event(s) between them, want it above with events between: this test proves nothing",
			cursor, horizon, between)
	}

	second := blockIndexProcess(t, database, nextBuildCommit)
	tick = awaitedBlockIndexTick(t, second)
	if read := tick.Events - countEventsAfter(t, database, cursor); read != 0 {
		t.Errorf("the first tick of a build with no cursor of its own read %d event(s) at or below the cursor %d another build left, want none",
			read, cursor)
	}
	if tick.Marked != 0 || tick.Comments != 0 {
		t.Errorf("the first tick of the second build marked %d document(s) and wrote %d comment(s) the first build's cursor had passed, want none",
			tick.Marked, tick.Comments)
	}
	if started := blockIndexEventCursor(t, database, nextBuildCommit); started < cursor {
		t.Errorf("the second build's cursor stands at %d after its first tick, below the cursor %d the first build left", started, cursor)
	}
	requireCensusReadsZero(t, database, "after the second build's first tick, with no second backfill")
}

// A build of a newer marking rule, with no event cursor of its own, starts reading the event log
// at the horizon the backfill of its own rule recorded, and not at the cursor a build of an older
// rule left above it, and marks the version between the two that its rule owes and the older one
// left unmarked.
//
// A build of the older rule marks a document only where its store_version is not its version
// (markVersionedDocuments), and a build of a newer rule can owe a version a build of the older
// rule indexed itself, which is what a later change raising the rule to 2 is for. So a build
// that owes more than another must not inherit its cursor (R13), as a consumer takes a peer's
// committed offset only where the two do the same work. This code holds one marking statement for
// every rule, so the test stands for the debt the newer rule's own statement would read by setting
// the version's store_version back to null once the older rule's ticks have passed it. The newer
// rule's backfill is a row of graph_backfill_horizon written as graphcensus's
// recordBackfillHorizon writes one, since this package cannot run a backfill of another rule.
func TestABuildOfANewerMarkingRuleReadsFromTheBackfillHorizon(t *testing.T) {
	database, older := blockIndexService(t)
	const slug = "newer"
	document := backfilledDocument(t, database, older, slug)
	newerRule := older.markingRule + 1
	horizon := recordHorizonOfRule(t, database, newerRule)

	// The older rule's build writes the document's next version itself and indexes it in the
	// request, so its stand is at the version, and its ticks read the version's event, mark
	// nothing and move its cursor above the newer rule's horizon.
	version, event := writeNextVersionAsThisBuild(t, older, document, nestedSpec(slug)+"\nA paragraph the next version adds.\n")
	tick := awaitedBlockIndexTick(t, older)
	cursor := blockIndexEventCursor(t, database, "")
	if tick.Events == 0 || tick.Marked != 0 || cursor < event || event <= horizon {
		t.Fatalf("the older rule's tick read %d event(s) and marked %d document(s), its cursor at %d, the version's event %d and the newer rule's horizon %d; want the event read, nothing marked and the event between the horizon and the cursor: this test proves nothing",
			tick.Events, tick.Marked, cursor, event, horizon)
	}
	oweTheNewerRule(t, database, document)

	newer := markingRuleBuild(t, database, nextBuildCommit, newerRule)
	tick = awaitedBlockIndexTick(t, newer)
	if started := tickStart(t, database, tick); started != horizon {
		t.Errorf("the first tick of a build of marking rule %d started at %d, the cursor a build of rule %d left, want the backfill's horizon %d",
			newerRule, started, older.markingRule, horizon)
	}
	if row := blockRowOf(t, database, document); !row.marked && !row.standsAt(version) {
		t.Errorf("the version %d of %s below that cursor is not marked owed, want it marked", version, document)
	}
	requireCensusReadsZero(t, database, "after the newer rule's first tick, with no second backfill")
}

// A build of this code's marking rule (refs.BlockIndexMarkingRule, 2), with no event cursor of its
// own, starts reading the event log at the horizon the backfill of its own rule recorded, and not
// at the cursor a build of rule 1 left above it, and marks the version between the two. Rule 1 is
// the rule a build had before every node carried its scope: a version such a build indexed wrote
// its block nodes with no project, and a build of rule 2 that took rule 1's cursor as its own would
// leave them so. TestABuildOfANewerMarkingRuleReadsFromTheBackfillHorizon holds any two rules to
// this; this row holds the two this code ships, by the rule the build is built with. Control: with
// BlockIndexMarkingRule kept at 1 the two builds share a rule, this build starts at the cursor the
// rule-1 build left, and the version is left unmarked.
func TestARuleOneCursorDoesNotStopARuleTwoBuildFromMarking(t *testing.T) {
	database, ruleOne := blockIndexService(t)
	const slug = "rule-one"
	// The document is seeded, backfilled and drained at this build's rule, so the census reads zero
	// before the rule-1 build writes; that build's own ticks then record rule 1 on its cursor row.
	document := backfilledDocument(t, database, ruleOne, slug)
	ruleOne.markingRule = 1
	horizon := recordHorizonOfRule(t, database, refs.BlockIndexMarkingRule)

	// The rule-1 build writes the document's next version itself and indexes it in the request,
	// so its stand is at the version, and its ticks read the version's event, mark nothing and move
	// its cursor above this build's horizon.
	version, event := writeNextVersionAsThisBuild(t, ruleOne, document, nestedSpec(slug)+"\nA paragraph the next version adds.\n")
	tick := awaitedBlockIndexTick(t, ruleOne)
	cursor := blockIndexEventCursor(t, database, "")
	if tick.Events == 0 || tick.Marked != 0 || cursor < event || event <= horizon {
		t.Fatalf("the rule-1 build's tick read %d event(s) and marked %d document(s), its cursor at %d, the version's event %d and this build's horizon %d; want the event read, nothing marked and the event between the horizon and the cursor: this test proves nothing",
			tick.Events, tick.Marked, cursor, event, horizon)
	}
	oweTheNewerRule(t, database, document)

	current := markingRuleBuild(t, database, nextBuildCommit, refs.BlockIndexMarkingRule)
	tick = awaitedBlockIndexTick(t, current)
	if started := tickStart(t, database, tick); started != horizon {
		t.Errorf("the first tick of this build, of marking rule %d, started at %d, the cursor a build of rule 1 left, want its own rule's horizon %d",
			refs.BlockIndexMarkingRule, started, horizon)
	}
	if row := blockRowOf(t, database, document); !row.marked && !row.standsAt(version) {
		t.Errorf("the version %d of %s below the rule-1 build's cursor is not marked owed by this build of marking rule %d, want it marked",
			version, document, refs.BlockIndexMarkingRule)
	}
}

// A build of a higher marking rule, with no event cursor of its own, starts at the horizon the
// backfill of its own rule recorded, even where a backfill of a lower rule recorded a higher one
// since, and marks the version between the two that the lower rule leaves unmarked.
//
// The lower rule's backfill marked what the lower rule owes; a version it read and did not mark,
// because the lower rule indexed that version itself, is one the higher rule may still owe. Were
// one horizon shared by every rule, or read whatever its rule, the higher rule's first tick would
// start at the lower rule's horizon and pass over that version (R13). The higher rule's debt and
// its backfill's horizon stand as in TestABuildOfANewerMarkingRuleReadsFromTheBackfillHorizon.
func TestAHigherRuleStartsAtItsOwnHorizonAboveALowerRulesBackfill(t *testing.T) {
	database, lower := blockIndexService(t)
	ctx := context.Background()
	appendIssueEvent(t, database)
	const slug = "between"
	document := createOwedDocument(t, database, lower, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	drainBlockIndex(t, lower, "seed")
	higherRule := lower.markingRule + 1
	higherHorizon := recordHorizonOfRule(t, database, higherRule)

	// The lower rule's build writes the next version itself, and a backfill of the lower rule then
	// records its horizon above that version's event and marks nothing.
	version, event := writeNextVersionAsThisBuild(t, lower, document, nestedSpec(slug)+"\nA paragraph the next version adds.\n")
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store at marking rule %d: %v", lower.markingRule, err)
	}
	lowerHorizon := recordedBackfillHorizon(t, database, lower.markingRule)
	if row := blockRowOf(t, database, document); row.marked || !row.standsAt(version) || event <= higherHorizon || lowerHorizon < event {
		t.Fatalf("the version %d (event %d) stands between the horizons %d and %d with its row %s; want it between them, stated at its version and unmarked: this test proves nothing",
			version, event, higherHorizon, lowerHorizon, row)
	}
	oweTheNewerRule(t, database, document)

	higher := markingRuleBuild(t, database, nextBuildCommit, higherRule)
	tick := awaitedBlockIndexTick(t, higher)
	if started := tickStart(t, database, tick); started != higherHorizon {
		t.Errorf("the first tick of a build of marking rule %d started at %d, want its own rule's horizon %d",
			higherRule, started, higherHorizon)
	}
	if row := blockRowOf(t, database, document); !row.marked && !row.standsAt(version) {
		t.Errorf("the version %d of %s between the two horizons is not marked owed, want it marked", version, document)
	}
}

// A build of a marking rule that finds no event cursor and no backfill horizon of its own rule
// starts reading the event log at its first tick's safe horizon, and reads no event below it,
// even where a backfill of a lower rule recorded a horizon below.
//
// The lower rule's backfill marked what the lower rule owes, so its horizon says nothing of what
// this build owes: every event below this build's first horizon is left to the backfill of this
// build's own rule, which reads every row whatever its age when it runs (R13).
func TestABuildWithOnlyALowerRulesHorizonStartsAtItsSafeHorizon(t *testing.T) {
	database, lower := blockIndexService(t)
	ctx := context.Background()
	appendIssueEvent(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store at marking rule %d: %v", lower.markingRule, err)
	}
	lowerHorizon := recordedBackfillHorizon(t, database, lower.markingRule)
	appendIssueEvent(t, database)
	appendIssueEvent(t, database)
	if countEventsAfter(t, database, lowerHorizon) == 0 {
		t.Fatalf("no event stands above the lower rule's horizon %d: this test proves nothing", lowerHorizon)
	}

	higherRule := lower.markingRule + 1
	higher := markingRuleBuild(t, database, nextBuildCommit, higherRule)
	tick := awaitedBlockIndexTick(t, higher)
	safe := blockIndexEventCursor(t, database, nextBuildCommit)
	if tick.Events != 0 {
		t.Errorf("the first tick of a build of marking rule %d started at %d with only the horizon of rule %d standing, want its safe horizon %d",
			higherRule, tickStart(t, database, tick), lower.markingRule, safe)
	}
}

// markingRuleBuild is one process's document service on database, serving the build of commit,
// that applies the block index's marking rule rule (Service.markingRule): a build of another rule
// than this one's, beside it.
func markingRuleBuild(t *testing.T, database *store.Store, commit string, rule int) *Service {
	t.Helper()
	service := blockIndexProcess(t, database, commit)
	service.markingRule = rule
	return service
}

// backfilledDocument seeds document slug of issue CORE-1, a nestedSpec, as a build before the
// store wrote it, beside the comments and the route TestTheBlockParentCensusReachesZeroAfterTheBackfill
// seeds so the census can pass only on agreement, backfills the store and drains the backfill's
// mark with service, and answers the document once the census reads zero.
func backfilledDocument(t *testing.T, database *store.Store, service *Service, slug string) string {
	t.Helper()
	ctx := context.Background()
	appendIssueEvent(t, database)
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"marking-rule-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:marking-rule-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after the backfill's mark drained")
	return document
}

// recordHorizonOfRule records the newest event id of the log as the horizon of a backfill of the
// marking rule rule, which every event the test wrote is at or below, as graphcensus's
// recordBackfillHorizon records its own rule's row, and answers it.
func recordHorizonOfRule(t *testing.T, database *store.Store, rule int) int64 {
	t.Helper()
	var horizon int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into graph_backfill_horizon (marking_rule, event_id)
		select $1, coalesce(max(id), 0) from events
		on conflict (marking_rule) do update set event_id = excluded.event_id, recorded_at = now()
		returning event_id
	`, rule).Scan(&horizon); err != nil {
		t.Fatalf("record the horizon of a backfill of marking rule %d: %v", rule, err)
	}
	return horizon
}

// writeNextVersionAsThisBuild writes the next version of document, whose text is markdown, as a
// person's edit through this build's edit route writes it (writeVersionThroughTheRealPath), which
// indexes the version in the request, and answers the version and its event.
func writeNextVersionAsThisBuild(t *testing.T, service *Service, document, markdown string) (int, int64) {
	t.Helper()
	if _, err := joinedReplaceText(service, document, markdown, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit the document the next version records: %v", err)
	}
	written, event := writeVersionThroughTheRealPath(t, service, document, "The next version")
	if row := blockRowOf(t, service.store, document); !row.standsAt(written.Version.Number) {
		t.Fatalf("version %d of %s left its row at %s, want it stated at the request", written.Version.Number, document, row)
	}
	return written.Version.Number, event
}

// oweTheNewerRule stands for the debt a build of a newer marking rule owes a version a build of the
// older rule indexed itself: this code holds one marking statement for every rule, which marks a
// document only where its store_version is not its version, so the test sets the version's
// store_version back to null, which is what that statement reads of a version its rule's store
// has not stated.
func oweTheNewerRule(t *testing.T, database *store.Store, document string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update document_block_versions set store_version = null where artifact_id = $1
	`, document); err != nil {
		t.Fatalf("owe the newer rule the version of %s: %v", document, err)
	}
}

// tickStart is where tick started its read of the event log: the id below the oldest event it
// read, as every event above that id is one it read (awaitedBlockIndexTick), and the newest event
// id where it read none.
func tickStart(t *testing.T, database *store.Store, tick BlockIndexTick) int64 {
	t.Helper()
	var start int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select id from events order by id desc offset $1 limit 1), 0)
	`, tick.Events).Scan(&start); err != nil {
		t.Fatalf("read where the tick started: %v", err)
	}
	return start
}

// blockRowState is a document's document_block_versions row: its version, its stand
// (store_version), whether it holds this build's mark (store_owed_at) and the marking rule the row
// records (store_owed_rule).
type blockRowState struct {
	version, stand *int
	marked         bool
	rule           int
}

// standsAt is whether the row stands at version on both columns.
func (row blockRowState) standsAt(version int) bool {
	return row.version != nil && *row.version == version && row.stand != nil && *row.stand == version
}

func (row blockRowState) String() string {
	return fmt.Sprintf("version %s, stand %s, marked %t, rule %d", standsText(row.version), standsText(row.stand), row.marked, row.rule)
}

// blockRowOf reads document's document_block_versions row, which has to be there.
func blockRowOf(t *testing.T, database *store.Store, document string) blockRowState {
	t.Helper()
	var row blockRowState
	if err := database.Pool.QueryRow(context.Background(), `
		select version, store_version, store_owed_at is not null, store_owed_rule
		from document_block_versions where artifact_id = $1
	`, document).Scan(&row.version, &row.stand, &row.marked, &row.rule); err != nil {
		t.Fatalf("read the block index row of %s: %v", document, err)
	}
	return row
}

// A version the release before this one writes while it holds the shared block-index lease, which
// only drops a block of a document this build's drain has stated, reaches the store, and the
// census reaches zero with no second backfill.
//
// That release's index pass deletes the dropped block's row and its link's row, writes no row at
// all and states nothing in the store: the block's node stays live and its link asserted with
// nothing projecting either, and no row of the document names no node. So this build's tick marks
// the document from the version's event by the two versions its row records - version, which that
// release moved, against store_version, which only this build's index pass writes
// (readEventsAfterCursor) - and by no row naming no node, of which there is none here. Its drain
// then reads what the store still asserts of the document beside the rows (refs.IndexBlocks) and
// states the dropped block gone and its link retracted.
//
// The release before takes every period of the lease while it serves, as in
// TestAVersionThePreviousReleaseWritesWhileItHoldsTheLeaseReachesTheStore, so no tick of this build
// runs between that release's version and its leaving.
func TestAVersionThePreviousReleaseWritesThatOnlyDropsABlockReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "dropping"
	closer := "Closer of " + slug
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug)+"\n"+closer+", after dispatch://CORE-2.\n", "")
	settle(t, database, document)
	// Rows of every other kind the census reads, so the census can pass only on agreement.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-dropping-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-dropping-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")

	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}
	dropped := blockLinkRow{blockID: holderOf(t, service, document, closer)}
	if err := database.Pool.QueryRow(ctx, `
		select kind, to_kind, to_id from block_refs where artifact_id = $1 and block_id = $2
	`, document, dropped.blockID).Scan(&dropped.kind, &dropped.toKind, &dropped.toID); err != nil {
		t.Fatalf("read the closing paragraph's link: %v", err)
	}
	droppedAddress := refKey + "#" + dropped.blockID

	// A process of the release before joins and takes every period of the lease: it writes a
	// version that drops the closing paragraph and adds nothing, and leaves, and no tick of this
	// build runs meanwhile. Its version write settles the re-index queue's row of the version as
	// it indexes the text (moveLinkAsThePreviousRelease), so the version's event is the only way
	// left for this build to reach the document.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	indexAsThePreviousRelease(t, database, service, document, removeBlockWithID(dropped.blockID), []blockLinkRow{dropped}, nil)
	if _, err := database.Pool.Exec(ctx, `delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's version write does: %v", slug, err)
	}
	if rows := blockRowsOf(t, database, document, dropped.blockID); rows != 0 {
		t.Fatalf("the closing paragraph keeps %d row(s) after the previous release's version, want its row dropped: this test proves nothing", rows)
	}
	if blocks, links := rowsNamingNoNode(t, database, document); blocks+links != 0 {
		t.Fatalf("the previous release's version left %d block row(s) and %d link row(s) naming no node, want none: the version only drops a block, which is what this test is about", blocks, links)
	}
	if marked := markedDocuments(t, database); marked != 0 {
		t.Fatalf("%d document(s) hold this build's mark after the previous release's version, want none: this test proves nothing", marked)
	}
	if before := takeStoreCensus(t, database); !before.Refused() {
		t.Fatalf("the census passes over the block the previous release dropped, so this test proves nothing")
	}
	previousLeaves()

	// This build's next tick, serving alone again, and its drain.
	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), marked %d document(s), indexed %d, another build %t",
		tick.Events, tick.Marked, tick.Documents, tick.AnotherBuild)
	if tick.Marked != 1 {
		t.Errorf("the first tick after the previous release left marked %d document(s), want the one whose version only drops a block", tick.Marked)
	}
	drainBlockIndex(t, service, "after the previous release left")

	requireDroppedBlockGone(t, database, droppedAddress, dropped)
	if marked := markedDocuments(t, database); marked != 0 {
		t.Errorf("%d document(s) still hold this build's mark after its drain, want none", marked)
	}
	requireCensusReadsZero(t, database, "after the previous release, which only dropped a block while it held the block-index lease, left, with no second backfill")
}

// registerProcess records a live server process serving commit, as a process registers once it
// has bound its port (processes.Register), and answers the call that deregisters it, which the
// test's cleanup makes too where the test has not.
func registerProcess(t *testing.T, database *store.Store, commit string) func() {
	t.Helper()
	registration, err := processes.Register(context.Background(), database.Pool, processes.Instance{
		Commit:          commit,
		SigningKeys:     []string{"rolling-update-signing-key"},
		SchemaVersion:   pmdoc.SchemaVersion(),
		SchemaCreatable: pmdoc.SchemaCreatable(),
	})
	if err != nil {
		t.Fatalf("register a process serving commit %q: %v", commit, err)
	}
	deregister := sync.OnceFunc(func() {
		if err := registration.Deregister(context.Background()); err != nil {
			t.Errorf("deregister the process serving commit %q: %v", commit, err)
		}
	})
	t.Cleanup(deregister)
	return deregister
}

// moveLinkAsThePreviousRelease has the stand-in for the release before this one write and index
// the next version of document slug, a nestedSpec whose blocks this build has stated: the inner
// paragraph's mention of CORE-2 moves onto the middle paragraph. That release's index pass deletes
// the inner paragraph's link row, inserts the middle paragraph's naming no node and states nothing
// in the store (indexAsThePreviousRelease). It answers the link the version adds.
//
// That release's version write also settles the row the version's insert owes the re-index queue
// (refs_grammar_owed, migration 20261103205041), since it indexes the version's own text with the
// whole grammar in the same transaction (refs.SettleOwed). The stand-in leaves that row, and this
// build's drain of it would owe the document a block index with this build's own mark
// (oweDocumentIndex): a mark that release never sets. So the row is settled here, before any
// tick runs, and the only way left for this build to reach the document is the version's event.
func moveLinkAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, document, slug string) blockLinkRow {
	t.Helper()
	innerWords, middleWords := "Inner of "+slug, "Middle of "+slug
	inner := holderOf(t, service, document, innerWords)
	middle := holderOf(t, service, document, middleWords)
	dropped := blockLinkRow{blockID: inner}
	if err := database.Pool.QueryRow(context.Background(), `
		select kind, to_kind, to_id from block_refs where artifact_id = $1 and block_id = $2
	`, document, inner).Scan(&dropped.kind, &dropped.toKind, &dropped.toID); err != nil {
		t.Fatalf("read the inner paragraph's link in %s: %v", slug, err)
	}
	added := blockLinkRow{blockID: middle, kind: dropped.kind, toKind: dropped.toKind, toID: dropped.toID}
	var mention string
	indexAsThePreviousRelease(t, database, service, document, func(tree *pmdoc.Node) *pmdoc.Node {
		mention = cutEnding(tree, innerWords)
		replaceText(tree, middleWords+".", middleWords+mention+".")
		return tree
	}, []blockLinkRow{dropped}, []blockLinkRow{added})
	if _, err := database.Pool.Exec(context.Background(), `
		delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid
	`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's version write does: %v", slug, err)
	}
	if mention == "" {
		t.Fatalf("no text of %s starts with %q, so the edit moved nothing", slug, innerWords)
	}
	if holder := holderOf(t, service, document, mention); holder != middle {
		t.Fatalf("the edit moved the mention of %s to block %s, want the middle paragraph %s: the room minted new ids and this test proves nothing",
			slug, holder, middle)
	}
	return added
}

// createAsThePreviousRelease has the stand-in for the release before this one create document
// slug of issue CORE-1, a nestedSpec, as that release's upload does: the artifact, its live text
// and version 1 (createOwedDocument), then, in one transaction, the artifact.created event, whose
// payload carries the whole artifact (api's artifactCreatedEventPayload) and whose seq it takes
// off the issue's own counter as that release's events.Append does, and that release's index
// pass of version 1, which writes every block row and the inner paragraph's link row naming no
// node and states nothing in the store (indexVersionAsThePreviousRelease). The link takes its kind
// and far end from like, a link to the same issue. The version's insert owes the re-index queue a
// row, which that release's write settles as it indexes the text (refs.SettleOwed), so it is
// settled here too, as moveLinkAsThePreviousRelease settles its own. It answers the document, its
// link and the event that created it.
func createAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, slug string, like blockLinkRow) (string, blockLinkRow, int64) {
	t.Helper()
	ctx := context.Background()
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	markdown, blocks, err := service.TextWithBlocks(ctx, serverReader, document)
	if err != nil {
		t.Fatalf("read the created document %s: %v", slug, err)
	}
	link := blockLinkRow{blockID: holderOf(t, service, document, "Inner of "+slug), kind: like.kind, toKind: like.toKind, toID: like.toID}
	var versionAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select created_at from artifact_versions where artifact_id = $1 and number = 1
	`, document).Scan(&versionAt); err != nil {
		t.Fatalf("read version 1 of %s: %v", slug, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	payload, err := json.Marshal(map[string]any{"artifact": map[string]any{
		"id": document, "issue_key": "CORE-1", "project": "CORE", "slug": slug, "name": slug + ".md", "kind": "doc",
	}})
	if err != nil {
		t.Fatalf("encode the creation's event of %s: %v", slug, err)
	}
	event, err := documentEvent(ctx, tx, "artifact.created", payload)
	if err != nil {
		t.Fatalf("append the creation's event of %s: %v", slug, err)
	}
	indexVersionAsThePreviousRelease(t, tx, document, 1, versionAt, event, markdown, blocks, nil, []blockLinkRow{link})
	if _, err := tx.Exec(ctx, `delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's upload does: %v", slug, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return document, link, event
}

// createIssueAsThePreviousRelease has the stand-in for the release before this one create issue
// key, number number of project CORE, with its spec, as that release's issue create does (api's
// issue create): the issue row, its primary document spec.md, a nestedSpec, with its live text and
// version 1, then, in one transaction, the issue.created event, which is the issue's own, names
// the issue in issue_key and in its payload's key and no document, records the spec's version 1
// and takes its seq off the issue's own counter as that release's events.Append does, and that
// release's index pass of that version (indexVersionAsThePreviousRelease), which writes every
// block row and the inner paragraph's link row naming no node and states nothing in the store.
// The link takes its kind and far end from like, a link to the same issue, and the re-index
// queue's row of the version is settled as that release's write settles it
// (createAsThePreviousRelease). It answers the spec, its link and the issue's event.
func createIssueAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, key string, number int, like blockLinkRow) (string, blockLinkRow, int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'CORE', $2, 'An issue the previous release created', '{"kind":"user","id":"alice"}', 'W')
	`, key, number); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
	const slug = "spec"
	document := createOwedDocument(t, database, service, key, "", slug, nestedSpec(slug), "")
	if _, err := database.Pool.Exec(ctx, `update artifacts set is_primary = true where id = $1`, document); err != nil {
		t.Fatalf("make the spec of %s its primary document: %v", key, err)
	}
	settle(t, database, document)
	markdown, blocks, err := service.TextWithBlocks(ctx, serverReader, document)
	if err != nil {
		t.Fatalf("read the spec of %s: %v", key, err)
	}
	link := blockLinkRow{blockID: holderOf(t, service, document, "Inner of "+slug), kind: like.kind, toKind: like.toKind, toID: like.toID}
	var versionAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select created_at from artifact_versions where artifact_id = $1 and number = 1
	`, document).Scan(&versionAt); err != nil {
		t.Fatalf("read version 1 of the spec of %s: %v", key, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	payload, err := json.Marshal(map[string]any{"key": key, "project": "CORE", "title": "An issue the previous release created"})
	if err != nil {
		t.Fatalf("encode the issue.created event of %s: %v", key, err)
	}
	event, err := recordIssueEvent(ctx, tx, key, "issue.created", aliceActor, payload)
	if err != nil {
		t.Fatalf("append the issue.created event of %s: %v", key, err)
	}
	indexVersionAsThePreviousRelease(t, tx, document, 1, versionAt, event, markdown, blocks, nil, []blockLinkRow{link})
	if _, err := tx.Exec(ctx, `delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of the spec of %s as the previous release's issue create does: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return document, link, event
}

// cutEnding cuts from the one text under node that starts with words what follows them, up to the
// sentence's closing full stop, and answers what it cut: the empty string where no text starts
// with words.
func cutEnding(node *pmdoc.Node, words string) string {
	if node.Type == "text" && strings.HasPrefix(node.Text, words) {
		ending := strings.TrimSuffix(node.Text[len(words):], ".")
		node.Text = words + "."
		return ending
	}
	for _, child := range node.Children {
		if ending := cutEnding(child, words); ending != "" {
			return ending
		}
	}
	return ""
}

// requireVersionInTheStore holds document, once this build has drained it, to what its latest
// version holds: no row of it names no node, it holds no mark of this build's, and the store
// states the link added out of its block.
func requireVersionInTheStore(t *testing.T, database *store.Store, document string, added blockLinkRow) {
	t.Helper()
	ctx := context.Background()
	if blocks, links := rowsNamingNoNode(t, database, document); blocks != 0 || links != 0 {
		t.Errorf("%d block row(s) and %d link row(s) of document %s name no node after this build's tick, want none", blocks, links, document)
	}
	var marked bool
	if err := database.Pool.QueryRow(ctx, `
		select store_owed_at is not null from document_block_versions where artifact_id = $1
	`, document).Scan(&marked); err != nil {
		t.Fatalf("read this build's mark on document %s: %v", document, err)
	}
	if marked {
		t.Errorf("document %s still holds this build's mark after the tick that drained it", document)
	}
	var address string
	if err := database.Pool.QueryRow(ctx, `select ref_key || '#' || $2 from artifacts where id = $1`, document, added.blockID).Scan(&address); err != nil {
		t.Fatalf("read the address of the block holding the added link: %v", err)
	}
	held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{added.kind})
	if err != nil {
		t.Fatalf("read the links the store states out of %s: %v", address, err)
	}
	if len(held) != 1 || held[0].ToKind != added.toKind || held[0].ToID != added.toID {
		t.Errorf("the store states %#v out of %s, want the %s %s:%s the version holds", held, address, added.kind, added.toKind, added.toID)
	}
}

// requireCensusReadsZero takes the census and fails the test on any difference it reads, of any
// kind and in either direction.
func requireCensusReadsZero(t *testing.T, database *store.Store, when string) {
	t.Helper()
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) %s, want zero", census.Differences(), when)
	}
}

// awaitedBlockIndexTick runs one tick, from the start of every read, once the safe horizon the
// tick reads the event log through has reached every event already written
// (events.Horizon.Await), so the tick reads every event the test wrote before it.
func awaitedBlockIndexTick(t *testing.T, service *Service) BlockIndexTick {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := service.blockIndexHorizon().Await(ctx); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	return reconcileBlockIndex(t, service, "")
}

// blockIndexEventCursor reads the event_id of the row of block_index_event_cursors of the build of
// commit (the empty string: an unstamped build's), the highest event id its ticks have read or
// passed, which has to be there.
func blockIndexEventCursor(t *testing.T, database *store.Store, commit string) int64 {
	t.Helper()
	var cursor int64
	if err := database.Pool.QueryRow(context.Background(), `
		select event_id from block_index_event_cursors where build_commit = $1
	`, commit).Scan(&cursor); err != nil {
		t.Fatalf("read the block-index event cursor of build %q: %v", commit, err)
	}
	return cursor
}

// recordedBackfillHorizon reads the horizon the newest backfill of the marking rule rule recorded
// it began at (graph_backfill_horizon), which has to be there.
func recordedBackfillHorizon(t *testing.T, database *store.Store, rule int) int64 {
	t.Helper()
	var horizon int64
	if err := database.Pool.QueryRow(context.Background(), `
		select event_id from graph_backfill_horizon where marking_rule = $1
	`, rule).Scan(&horizon); err != nil {
		t.Fatalf("read the horizon the backfill began at: %v", err)
	}
	return horizon
}

// appendIssueEvent appends one event of issue CORE-1 that names no document, as the stand-in for
// the previous release appends its own (indexAsThePreviousRelease), its seq taken off the issue's
// own counter as that release's events.Append takes it (recordIssueEvent), and answers its id.
func appendIssueEvent(t *testing.T, database *store.Store) int64 {
	t.Helper()
	id, err := recordIssueEvent(context.Background(), database.Pool, "CORE-1", "issue.updated", aliceActor, []byte(`{}`))
	if err != nil {
		t.Fatalf("append an event of CORE-1: %v", err)
	}
	return id
}

// latestVersionEvent is the id of the newest artifact.version event naming document.
func latestVersionEvent(t *testing.T, database *store.Store, document string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select max(id) from events where type = 'artifact.version' and payload->>'artifact_id' = $1
	`, document).Scan(&id); err != nil {
		t.Fatalf("read the version event of document %s: %v", document, err)
	}
	return id
}

// countEventsAfter is how many events the log holds past id after.
func countEventsAfter(t *testing.T, database *store.Store, after int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from events where id > $1`, after).Scan(&count); err != nil {
		t.Fatalf("count the events after %d: %v", after, err)
	}
	return count
}

// markedDocuments is how many documents hold this build's own mark (store_owed_at).
func markedDocuments(t *testing.T, database *store.Store) int {
	t.Helper()
	var marked int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from document_block_versions where store_owed_at is not null`).Scan(&marked); err != nil {
		t.Fatalf("count the documents holding this build's mark: %v", err)
	}
	return marked
}

// rowsNamingNoNode is how many document_blocks rows and block_refs rows of document name no node.
func rowsNamingNoNode(t *testing.T, database *store.Store, document string) (blocks, links int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_blocks where artifact_id = $1 and node_kind is null),
		       (select count(*) from block_refs where artifact_id = $1 and node_kind is null)
	`, document).Scan(&blocks, &links); err != nil {
		t.Fatalf("count the rows of document %s that name no node: %v", document, err)
	}
	return blocks, links
}
