package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// The phase-work set of the Legion board: when it is read again, and when it answers from what
// it holds (legion_phase_work.go). These tests build on legion_board_test.go's board fixtures;
// the four helpers at the end of this file are theirs alone.

// TestTheLiveBoardReadsPhaseWorkAsTheOneReadDoesAfterARename: the issues that carry Legion phase
// work are one read (legion.PhaseWorkKeys), which takes each session's newest title the one way
// Dispatch resolves a title (delivery.ResolveSessionTitles). A session renamed after it wrote is
// where a second rule built from each event's own title answers differently: it kept the issue
// the session wrote on under the title it wrote under, while the one read no longer names it at
// all. The live board, whose set the event feed keeps, and the route's own read must still place
// the issue below it in the same tree as the metrics.
func TestTheLiveBoardReadsPhaseWorkAsTheOneReadDoesAfterARename(t *testing.T) {
	rig := newHubRig(t, nil)
	holdAGeneration(t, rig)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{860, "The top, which the daemon reports", "a", "", true},
		{861, "The tracking issue the renamed session wrote on", "b", "INFRA-860", false},
		{862, "The bottom, which the daemon reports", "c", "INFRA-861", true},
		{863, "Its closed child", "d", "INFRA-862", false},
	} {
		key := seedLegionIssue(t, rig.database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, rig.database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	closeLegionIssue(t, rig.database, "INFRA-863", hourAgo)
	seedLegionRead(t, rig.database, "INFRA", now.Add(-time.Minute), "")

	// One session, two events: it wrote on the tracking issue under that issue's own title, and
	// was renamed to the issue it went on to work. Its newest title is the only title Dispatch
	// resolves, so no issue carries phase work by the first event any more.
	seedLegionRoleEvent(t, rig.database, "INFRA-861", "s-860", "implementer", "INFRA-861", hourAgo)
	seedLegionRoleEvent(t, rig.database, "INFRA-862", "s-860", "implementer", "INFRA-862", now.Add(-30*time.Minute))
	rig.applyEvents(t, "the two events the rename wrote")

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, 5*time.Second, "b")
	var live legionBoardResponse
	if err := json.Unmarshal(snapshot.Body, &live); err != nil {
		t.Fatalf("read the live board: %v", err)
	}
	population, err := metrics.ReadProject(t.Context(), rig.database.Pool, nil, "INFRA", now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-862"); root != "INFRA-862" {
		t.Errorf("the metrics put INFRA-862 in tree %q, want INFRA-862: no session titled for INFRA-861 wrote on it", root)
	}
	if tree := legionCard(t, live, "INFRA-862").Tree; tree != "INFRA-862" {
		t.Errorf("the live board puts INFRA-862 in tree %q, want INFRA-862, as the metrics do", tree)
	}
	requireSameBody(t, "the board the event feed kept", snapshot.Body,
		getBody(t, rig.handler, windowTarget(t, legionBoardView, snapshot.Body, "")))
}

// TestAnOrdinarySessionsEventLeavesThePhaseWorkSetCurrent: the issues that carry Legion phase
// work are one read of every project's whole event log and of every handoff
// (legion.PhaseWorkKeys), so the live hub marks the set stale only for an event that can move
// what that read answers (legionPhaseWork.observe). A session that is no Legion role writes
// most of Dispatch's feed and can neither give an issue phase work nor take it away: the rule
// reads its newest title and finds no role in it, now and at the read before.
//
// The handoff this records behind the event's back is how the test sees that no read was made:
// it is written into the store with no event of its own, so a board read that made the read
// again would climb through the issue it names, and one that answered from the set it held
// would not.
func TestAnOrdinarySessionsEventLeavesThePhaseWorkSetCurrent(t *testing.T) {
	rig := newHubRig(t, nil)
	holdAGeneration(t, rig)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	_, middle, bottom := seedPhaseWorkChain(t, rig, 880, now.Add(-time.Hour))
	rig.applyEvents(t, "the issues the test seeded")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q before the event, want itself: the issue above it carries no phase work", bottom, tree)
	}

	seedLegionHandoff(t, rig.database, middle, "plan", now.Add(-30*time.Minute))
	seedSessionEvent(t, rig.database, bottom, "s-880", "A person's own session", now.Add(-time.Minute))
	rig.applyEvents(t, "the ordinary session's event")

	if !rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an ordinary session's event marked the phase-work set stale, so the next board read scans every project's whole event log again")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Errorf("the board puts %s in tree %q after an ordinary session's event, want itself: the handoff recorded behind the event says the set was read again", bottom, tree)
	}
}

// TestALegionTitleAndARenameMarkThePhaseWorkSetStale: the other half of the narrow rule. An
// event whose title makes its session a Legion role working the issue it is written on gives
// that issue phase work, and an event that renames such a session takes it away again
// (legion.PhaseWorkKeys reads each session's newest title), so both mark the set stale and the
// board read after each answers the set as it stands.
func TestALegionTitleAndARenameMarkThePhaseWorkSetStale(t *testing.T) {
	rig := newHubRig(t, nil)
	holdAGeneration(t, rig)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	top, middle, bottom := seedPhaseWorkChain(t, rig, 890, now.Add(-time.Hour))
	rig.applyEvents(t, "the issues the test seeded")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q before the event, want itself: the issue above it carries no phase work", bottom, tree)
	}

	// The session's first event under a Legion role's title, on the very issue that title
	// names: the middle issue carries phase work from here on, and the walk climbs through it.
	seedLegionRoleEvent(t, rig.database, middle, "s-890", "implementer", middle, now.Add(-30*time.Minute))
	rig.applyEvents(t, "the Legion role's event")
	if rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an event that makes its session a Legion role of the issue it wrote on left the phase-work set current, so the next board read answers without it")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != top {
		t.Errorf("the board puts %s in tree %q after the Legion role's event, want %s: the issue above it carries phase work now", bottom, tree, top)
	}

	// The same session renamed to the issue it went on to work: its newest title is the only
	// title Dispatch resolves, so the middle issue carries no phase work by it any more.
	seedLegionRoleEvent(t, rig.database, bottom, "s-890", "implementer", bottom, now.Add(-15*time.Minute))
	rig.applyEvents(t, "the renamed session's event")
	if rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("an event that renamed a Legion role's session left the phase-work set current, so the next board read still holds the issue it wrote on before")
	}
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Errorf("the board puts %s in tree %q after the rename, want itself: no session titled for %s wrote on it", bottom, tree, middle)
	}
}

// TestAClosedPageLeavesNoPhaseWorkSetToAnswerFrom: the issues that carry Legion phase work are
// kept by the live hub's event feed, and the hub reads that feed only while a page holds a
// subscription — the last page to close stops the generation, and nothing marks the set stale
// from then until the next one starts (deliveryHub.dropLocked). GET
// /api/v1/projects/{key}/legion/board is answered for any caller, page or no page, so a board
// read between pages must make the one read itself; one that answers from the set the closed
// page left behind places an issue in the tree it stood in when that page closed.
func TestAClosedPageLeavesNoPhaseWorkSetToAnswerFrom(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	top, middle, bottom := seedPhaseWorkChain(t, rig, 900, now.Add(-time.Hour))

	// One page opens, which starts the generation that seeds the set and reads the feed. Its
	// view is a timeline, which reads no Legion board, so every board read here is the test's.
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("page", presetView(deliveryLiveTimeline, 7, "")))
	rig.applyEvents(t, "the issues the test seeded")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q while the page is open, want itself: the issue above it carries no phase work yet", bottom, tree)
	}

	// The page closes, which stops the generation: no feed is read from here on.
	rig.hub.Close(conn)
	if rig.hub.server.legionPhaseWork.isCurrent() {
		t.Error("the phase-work set is current with no page open, so a board read answers from a set no event can mark stale")
	}

	// The event no feed carries now: a session's first event under a Legion role's title, on
	// the very issue that title names, which gives the middle issue phase work.
	seedLegionRoleEvent(t, rig.database, middle, "s-900", "implementer", middle, now.Add(-30*time.Minute))

	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != top {
		t.Errorf("with no page open the board puts %s in tree %q, want %s: the issue above it carries phase work now", bottom, tree, top)
	}
}

// A stopping generation's intake can reach the feed-closed branch after the generation that
// replaced it has seeded and watched the set, and the watch it finds there is not its own to
// drop. So unwatch names the feed it is unwatching and clears the watch only while that feed is
// the one watching. Dropping another generation's watch costs no wrong answer — every read
// scans afresh while nothing watches — but it throws away the live page's own freshness, so the
// board and every Legion recompute read the whole log until that generation stops.
func TestUnwatchLeavesTheWatchOfTheGenerationThatReplacedItAlone(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	live := make(chan struct{})
	defer close(live)

	work := &legionPhaseWork{}
	work.watch(live)
	if !work.isCurrent() {
		t.Fatal("a set watched by an open feed answers no read, want it current")
	}
	work.unwatch(stopped)
	if !work.isCurrent() {
		t.Error("the stopped generation's unwatch took the live generation's watch off")
	}
	work.unwatch(live)
	if work.isCurrent() {
		t.Error("the live generation's own unwatch left the set current, want it watched by nothing")
	}
}

// TestATitlelessEventOfARoleSessionMarksThePhaseWorkSet: the phase-work rule reads an event
// that carried no title of its own under the session's newest title all the same
// (legion_phase_work.go, legion/phase_work.go), so such an event can be the first that gives
// its issue phase work. The hub's intake must therefore mark the set stale for it, although it
// is no retitle and marks no view: a write with no title takes no name away, and the Dispatch
// CLI sends one whenever the session's title is unset (envoy-client's dispatch-cli.ts).
//
// Before this round the intake took main's early return for such an event and never reached
// observe, so a board read went on answering from a set that predates the work.
func TestATitlelessEventOfARoleSessionMarksThePhaseWorkSet(t *testing.T) {
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	top, middle, bottom := seedPhaseWorkChain(t, rig, 990, now.Add(-time.Hour))
	holdAGeneration(t, rig)

	// The session is known to the set as the middle issue's implementer, by an event it wrote
	// on the bottom issue: its title names the middle, which carries no phase work yet.
	seedLegionRoleEvent(t, rig.database, bottom, "s-990", "implementer", middle, now.Add(-45*time.Minute))
	rig.applyEvents(t, "the role session's first event, written on another issue")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q, want itself: the issue above it carries no phase work yet", bottom, tree)
	}

	// The same session writes on the middle issue carrying no title: the rule reads it under
	// the title the set knows, so the middle carries phase work from here and the bottom's
	// tree climbs past it.
	seedSessionEvent(t, rig.database, middle, "s-990", "", now.Add(-30*time.Minute))
	rig.applyEvents(t, "the role session's title-less event on the issue its title names")
	if tree := legionCard(t, legionBoardOnTheHub(t, rig, "INFRA"), bottom).Tree; tree != top {
		t.Errorf("after the title-less event the board puts %s in tree %q, want %s: the event gave %s phase work and the set answered from before it", bottom, tree, top, middle)
	}
}

// seedPhaseWorkChain seeds three issues of INFRA, top to bottom: a top the daemon reports, a
// middle it reports nothing of, and a bottom it reports. The walk stops below the middle while
// the middle is no Legion issue, and climbs to the top the moment it carries phase work, so the
// bottom's tree is what a board read says of the phase-work set.
func seedPhaseWorkChain(t *testing.T, rig *hubRig, first int, at time.Time) (top, middle, bottom string) {
	t.Helper()
	top = seedLegionIssue(t, rig.database, "INFRA", first, "The top, which the daemon reports", "a", nil, "")
	middle = seedLegionIssue(t, rig.database, "INFRA", first+1, "The tracking issue in neither half of the rule", "b", nil, top)
	bottom = seedLegionIssue(t, rig.database, "INFRA", first+2, "The bottom, which the daemon reports", "c", nil, middle)
	for _, key := range []string{top, bottom} {
		seedLegionObservation(t, rig.database, "INFRA", key, legion.FieldPhase, `"implementing"`, at)
	}
	seedLegionRead(t, rig.database, "INFRA", at, "")
	return top, middle, bottom
}

// legionBoardOnTheHub is the board the hub's own server reads, the one whose phase-work set the
// event feed keeps: the rig builds the hub on a server of its own, so the handler's set is
// another one, filled by its own reads and marked stale by nothing.
func legionBoardOnTheHub(t *testing.T, rig *hubRig, project string) legionBoardResponse {
	t.Helper()
	population, err := rig.hub.server.readLegionBoard(t.Context(), project)
	if err != nil {
		t.Fatalf("read %s's board on the hub's server: %v", project, err)
	}
	return legionBoardCompute(population, time.Now().UTC())
}

// holdAGeneration keeps one generation of the hub running for the rest of the test: nothing of
// the hub runs while no conn holds a sub (deliveryHub.Subscribe), and these tests read what the
// event feed does to the phase-work set rather than what any view answers. The sub is a
// timeline view, which reads no Legion board, so every board read here is the test's own, and
// the generation stands from before the events the test writes, so the feed carries them.
func holdAGeneration(t *testing.T, rig *hubRig) {
	t.Helper()
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("hold", presetView(deliveryLiveTimeline, 7, "")))
	t.Cleanup(func() { rig.hub.Close(conn) })
}

// applyEvents wakes the relay the hub reads its events from and waits until the hub holds every
// event the test wrote. The generation holdAGeneration started is what applies them.
func (r *hubRig) applyEvents(t *testing.T, what string) {
	t.Helper()
	head := eventHead(t, r.database)
	notifyChannel(t, r.database, EventsChannel, "")
	r.waitForEventCursor(t, head, what)
}
