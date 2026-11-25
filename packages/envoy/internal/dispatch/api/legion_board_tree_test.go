// legion_board_tree_test.go holds the tests of the tree walk the board and the metrics share, of
// the Legion phase work that walk climbs through, and of the Delivery feed a failed Legion read
// leaves running.
package api

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAnIssueThatLeftTheDaemonsStateStaysTheTopOfItsTree is the reviewer's probe of
// dispatch://INFRA-1077's spec, "The top issue of a tree": the daemon reports an issue that is in
// its state or that Dispatch observed a phase of. The top issue was observed `planning` and then
// left the daemon's state, which writes its phase `null`; its child is still `implementing`, and
// neither carries a worker event or a handoff. The top issue carries no card, and both readers put
// the child in its tree rather than making the child a root of its own.
func TestAnIssueThatLeftTheDaemonsStateStaysTheTopOfItsTree(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hour := func(hours int) time.Time { return now.Add(time.Duration(hours-4) * time.Hour) }
	seedLegionIssue(t, database, "INFRA", 6, "The top issue", "a", nil, "")
	seedLegionIssue(t, database, "INFRA", 7, "Its child", "b", nil, "INFRA-6")
	seedLegionObservation(t, database, "INFRA", "INFRA-6", legion.FieldPhase, `"planning"`, hour(1))
	seedLegionObservation(t, database, "INFRA", "INFRA-6", legion.FieldPhase, `null`, hour(3))
	seedLegionObservation(t, database, "INFRA", "INFRA-7", legion.FieldPhase, `"implementing"`, hour(1))
	seedLegionRead(t, database, "INFRA", hour(3), "")

	board := legionBoardOf(t, handler, "INFRA")
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if card.Key == "INFRA-6" {
				t.Errorf("the board holds INFRA-6 in the %s column, want no card: the newest read does not carry it", column.ID)
			}
		}
	}
	if tree := legionCard(t, board, "INFRA-7").Tree; tree != "INFRA-6" {
		t.Errorf("the board puts INFRA-7 in tree %q, want INFRA-6: Dispatch observed a phase of its parent", tree)
	}
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-7"); root != "INFRA-6" {
		t.Errorf("the metrics put INFRA-7 in tree %q, want INFRA-6: Dispatch observed a phase of its parent", root)
	}
}

// TestTheBoardAndTheMetricsPutAnIssueInTheSameTree: the board and the four metrics read one
// tree-root rule (legion.TreeRoot), so no issue is a child on the board and a root in a report.
// The chain here is 40 deep, longer than the depth cap the board's own walk once stopped at, and
// it hangs under a tracking issue that carries no Legion work, which the metrics' population
// holds and the daemon never reported.
func TestTheBoardAndTheMetricsPutAnIssueInTheSameTree(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	// The tracking issue above the tree: Dispatch holds it, the daemon reports nothing of it.
	tracking := seedLegionIssue(t, database, "INFRA", 900, "The tracking issue", "a", nil, "")
	parent := tracking
	for number := 901; number <= 940; number++ {
		key := seedLegionIssue(t, database, "INFRA", number, "A deep issue", fmt.Sprintf("b%02d", number-900), nil, parent)
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, now.Add(-time.Hour))
		seedLegionObservation(t, database, "INFRA", key, legion.ClaimFieldPrefix+"implementer", `"working"`, now.Add(-time.Hour))
		parent = key
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	cards := 0
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards++
			if root := population.TreeRoot(card.Key); root != card.Tree {
				t.Errorf("%s is in tree %q on the board and %q in the metrics", card.Key, card.Tree, root)
			}
		}
	}
	if cards != 40 {
		t.Fatalf("the board shows %d cards, want the 40 issues the daemon reported", cards)
	}
	if root := population.TreeRoot("INFRA-940"); root != "INFRA-901" {
		t.Errorf("the deepest issue's tree root is %s, want INFRA-901 under the tracking issue", root)
	}
}

// seedLegionHandoff records one handoff of the issue, the other kind of Legion phase work
// (legion/metrics/population.go's readHandoffs).
func seedLegionHandoff(t *testing.T, database *store.Store, issue, phase string, at time.Time) {
	t.Helper()
	legionExec(t, database, `insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, $2, 1, '{}', '{"kind":"session","id":"s-handoff"}'::jsonb, $3)`, issue, phase, at)
}

// TestTheBoardClimbsThroughAnIssueWithOnlyPhaseWork: the board and the metrics read one rule for
// what a Legion issue is (dispatch://INFRA-1077's spec, "The top issue of a tree") — the daemon
// reports it, or it carries Legion phase work — so a chain climbs through an issue the daemon
// never reported that carries a handoff, and through one that carries an event of a session
// titled `Legion <role> · <its key>`. Such an issue is an input to the walk and nothing else: it
// gets no card, and the board names no issue it did not name before.
func TestTheBoardClimbsThroughAnIssueWithOnlyPhaseWork(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	// Two chains, each a reported ancestor over a middle issue the daemon reports nothing of
	// over a reported child; under each child a closed issue, so the metrics' window reaches the
	// whole chain. The first middle carries a handoff, the second a Legion role session's event.
	for _, chain := range []struct{ ancestor, middle, card, closed int }{{800, 801, 802, 803}, {810, 811, 812, 813}} {
		for _, issue := range []struct {
			number int
			parent string
		}{
			{chain.ancestor, ""},
			{chain.middle, fmt.Sprintf("INFRA-%d", chain.ancestor)},
			{chain.card, fmt.Sprintf("INFRA-%d", chain.middle)},
			{chain.closed, fmt.Sprintf("INFRA-%d", chain.card)},
		} {
			key := seedLegionIssue(t, database, "INFRA", issue.number, fmt.Sprintf("Issue %d", issue.number),
				fmt.Sprintf("b%03d", issue.number), nil, issue.parent)
			if issue.number == chain.ancestor || issue.number == chain.card {
				seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
			}
		}
		closeLegionIssue(t, database, fmt.Sprintf("INFRA-%d", chain.closed), hourAgo)
	}
	seedLegionHandoff(t, database, "INFRA-801", "implement", hourAgo)
	seedLegionRoleEvent(t, database, "INFRA-811", "s-811", "tester", "INFRA-811", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	cards := map[string]legionBoardCard{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
		}
	}
	named := slices.Sorted(maps.Keys(cards))
	want := []string{"INFRA-800", "INFRA-802", "INFRA-810", "INFRA-812"}
	if !slices.Equal(named, want) {
		t.Fatalf("the board's cards are %v, want the four issues the daemon reports, %v", named, want)
	}
	for _, chain := range [][2]string{{"INFRA-802", "INFRA-800"}, {"INFRA-812", "INFRA-810"}} {
		card, root := chain[0], chain[1]
		if tree := cards[card].Tree; tree != root {
			t.Errorf("the board puts %s in tree %q, want %q, its ancestor above an issue with only phase work", card, tree, root)
		}
		if got := population.TreeRoot(card); got != root {
			t.Errorf("the metrics put %s in tree %q, want %q", card, got, root)
		}
	}
	for _, middle := range [][2]string{{"INFRA-801", "INFRA-800"}, {"INFRA-811", "INFRA-810"}} {
		if got := population.TreeRoot(middle[0]); got != middle[1] {
			t.Errorf("the metrics put the middle issue %s in tree %q, want %q", middle[0], got, middle[1])
		}
	}
	for key, card := range cards {
		if root := population.TreeRoot(key); root != card.Tree {
			t.Errorf("%s is in tree %q on the board and %q in the metrics", key, card.Tree, root)
		}
	}
}

// TestTheBoardAndTheMetricsStopBelowAnIssueThatIsNoLegionIssue: the walk stops below the first
// issue above it that is neither reported by the daemon nor carrying Legion phase work
// (dispatch://INFRA-1077's spec, "The top issue of a tree"), rather than passing over it to the
// Legion issue above. One chain, bottom to top: a closed issue, so the metrics' window reaches
// the chain; a bottom the daemon reports; a middle in neither half of the rule, which an event
// of a session titled for another issue does not put in either; and a top the daemon reports.
// Both readers answer the bottom its own root, and the top roots nothing below the gap.
func TestTheBoardAndTheMetricsStopBelowAnIssueThatIsNoLegionIssue(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{840, "The top, which the daemon reports", "a", "", true},
		{841, "The tracking issue in neither half of the rule", "b", "INFRA-840", false},
		{842, "The bottom, which the daemon reports", "c", "INFRA-841", true},
		{843, "Its closed child, which brings the chain into the window", "d", "INFRA-842", false},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	// The middle carries an event of a Legion role, titled for the issue that role works and
	// not for this one: a tracking issue a worker reports under is written on by Legion
	// sessions and carries no phase work of its own.
	seedLegionRoleEvent(t, database, "INFRA-841", "s-842", "implementer", "INFRA-842", hourAgo)
	closeLegionIssue(t, database, "INFRA-843", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-842"); root != "INFRA-842" {
		t.Errorf("the metrics put INFRA-842 in tree %q, want INFRA-842: the issue above it is no Legion issue", root)
	}
	if root := population.TreeRoot("INFRA-840"); root != "INFRA-840" {
		t.Errorf("the metrics put INFRA-840 in tree %q, want itself", root)
	}
	cards := map[string]legionBoardCard{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
		}
	}
	if tree := cards["INFRA-842"].Tree; tree != "INFRA-842" {
		t.Errorf("the board puts INFRA-842 in tree %q, want INFRA-842, as the metrics do", tree)
	}
	for key, card := range cards {
		if key != "INFRA-840" && card.Tree == "INFRA-840" {
			t.Errorf("the board roots %s at INFRA-840, which is above an issue that is no Legion issue", key)
		}
		if root := population.TreeRoot(key); root != card.Tree {
			t.Errorf("%s is in tree %q on the board and %q in the metrics", key, card.Tree, root)
		}
	}
}

// TestAnUnstartedChildIsCountedOnTheNearestCardAndNeverLosesItsOwn: a card an issue would carry
// is never lost to the widened walk, and the count of an unstarted child lands on a card someone
// can read. The chain here hangs under an issue that carries only Legion phase work, so the walk
// climbs through it and no card stands at the tree's root: the admitted issue below it keeps its
// own card, and the unstarted child below that is counted on it rather than on the root the
// board shows nothing of.
func TestAnUnstartedChildIsCountedOnTheNearestCardAndNeverLosesItsOwn(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	// No admission block is recorded, so the Queue is Dispatch's rank order over the issues
	// waiting for a slot (the board's own fallback).
	seedLegionIssue(t, database, "INFRA", 820, "The issue with only phase work", "a", nil, "")
	seedLegionHandoff(t, database, "INFRA-820", "plan", hourAgo)
	for _, issue := range []struct {
		number int
		title  string
		rank   string
		parent string
	}{
		{821, "The admitted issue", "b", "INFRA-820"},
		{822, "Its unstarted child", "c", "INFRA-821"},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"admitted"`, hourAgo)
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	cards := map[string]legionBoardCard{}
	columns := map[string]string{}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			cards[card.Key] = card
			columns[card.Key] = column.ID
		}
	}
	named := slices.Sorted(maps.Keys(cards))
	if want := []string{"INFRA-821"}; !slices.Equal(named, want) {
		t.Fatalf("the board's cards are %v, want %v: the admitted issue keeps the card it carried", named, want)
	}
	if column := columns["INFRA-821"]; column != legionColumnQueue {
		t.Errorf("INFRA-821 is in the %q column, want %q: it is waiting for a slot", column, legionColumnQueue)
	}
	if children := cards["INFRA-821"].Children; children != 1 {
		t.Errorf("INFRA-821 counts %d unstarted children, want the one below it: a count on a root the board shows no card of is a count nobody reads", children)
	}
	if tree := cards["INFRA-821"].Tree; tree != "INFRA-820" {
		t.Errorf("INFRA-821 is in tree %q, want INFRA-820, the issue with phase work above it", tree)
	}
}

// TestTheMetricsClimbThroughPhaseWorkOutsideTheirWindow: the phase-work half of what makes an
// issue Legion's is a fact of the project and not of a window, so the metrics read it for every
// issue of the project and not only for the issues their window's scope covers
// (legion/metrics/population.go's scopeKeys). Nothing here closed in the window, so nothing but
// that project-wide read puts the card issue in its ancestor's tree.
func TestTheMetricsClimbThroughPhaseWorkOutsideTheirWindow(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	for _, issue := range []struct {
		number   int
		title    string
		rank     string
		parent   string
		reported bool
	}{
		{830, "The reported ancestor", "a", "", true},
		{831, "The issue with only phase work", "b", "INFRA-830", false},
		{832, "The card issue", "c", "INFRA-831", true},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	seedLegionHandoff(t, database, "INFRA-831", "implement", hourAgo)
	seedLegionRoleEvent(t, database, "INFRA-831", "s-831", "reviewer", "INFRA-831", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	board := legionBoardOf(t, handler, "INFRA")
	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-832"); root != "INFRA-830" {
		t.Errorf("the metrics put INFRA-832 in tree %q, want INFRA-830: no issue of this chain finished in the window", root)
	}
	if root := population.TreeRoot("INFRA-831"); root != "INFRA-830" {
		t.Errorf("the metrics put INFRA-831 in tree %q, want INFRA-830", root)
	}
	for _, column := range board.Columns {
		for _, card := range column.Cards {
			if root := population.TreeRoot(card.Key); root != card.Tree {
				t.Errorf("%s is in tree %q on the board and %q in the metrics", card.Key, card.Tree, root)
			}
		}
	}
}

// TestTheBoardReadsPhaseWorkAsTheOneReadDoesAfterARename: the issues that carry Legion phase work
// are one rule over one read of the same rows (legion.PhaseWorkKeys for the metrics,
// legion.PhaseWorkOf for the board), which takes each session's newest title the one way Dispatch
// resolves a title (delivery.ResolveSessionTitles). A session renamed after it wrote is where a
// rule built from each event's own title answers differently: it kept the issue the session wrote
// on under the title it wrote under, while the newest title no longer names it at all. The board
// reads the phase work of that issue alone, and the rename is an event written on another issue,
// so its read must still take the session's title from everything the session wrote, and put the
// issue below in the same tree as the metrics do.
func TestTheBoardReadsPhaseWorkAsTheOneReadDoesAfterARename(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
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
		{863, "Its closed child, which brings the chain into the window", "d", "INFRA-862", false},
	} {
		key := seedLegionIssue(t, database, "INFRA", issue.number, issue.title, issue.rank, nil, issue.parent)
		if issue.reported {
			seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, hourAgo)
		}
	}
	closeLegionIssue(t, database, "INFRA-863", hourAgo)
	seedLegionRead(t, database, "INFRA", now.Add(-time.Minute), "")

	// One session, two events: it wrote on the tracking issue under that issue's own title, and
	// was renamed to the issue it went on to work. Its newest title is the only title Dispatch
	// resolves, so no issue carries phase work by the first event any more.
	seedLegionRoleEvent(t, database, "INFRA-861", "s-860", "implementer", "INFRA-861", hourAgo)
	seedLegionRoleEvent(t, database, "INFRA-862", "s-860", "implementer", "INFRA-862", now.Add(-30*time.Minute))

	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", now.Add(-14*24*time.Hour), now)
	if err != nil {
		t.Fatalf("read the metrics population: %v", err)
	}
	if root := population.TreeRoot("INFRA-862"); root != "INFRA-862" {
		t.Errorf("the metrics put INFRA-862 in tree %q, want INFRA-862: no session titled for INFRA-861 wrote on it", root)
	}
	if tree := legionCard(t, legionBoardOf(t, handler, "INFRA"), "INFRA-862").Tree; tree != "INFRA-862" {
		t.Errorf("the board puts INFRA-862 in tree %q, want INFRA-862, as the metrics do", tree)
	}
}

// TestATitlelessEventOfARoleSessionGivesItsIssuePhaseWork: the phase-work rule reads an event that
// carried no title of its own under the session's newest title all the same (legion/phase_work.go),
// so such an event can be the first that gives its issue phase work: a write with no title takes
// no name away, and the Dispatch CLI sends one whenever the session's title is unset
// (envoy-client's dispatch-cli.ts). The board reads the phase work of the issue the event is
// written on alone, and the session's title is on an event of another issue, so its read must
// take that title from everything the session wrote.
func TestATitlelessEventOfARoleSessionGivesItsIssuePhaseWork(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	top := seedLegionIssue(t, database, "INFRA", 990, "The top, which the daemon reports", "a", nil, "")
	middle := seedLegionIssue(t, database, "INFRA", 991, "The tracking issue in neither half of the rule", "b", nil, top)
	bottom := seedLegionIssue(t, database, "INFRA", 992, "The bottom, which the daemon reports", "c", nil, middle)
	for _, key := range []string{top, bottom} {
		seedLegionObservation(t, database, "INFRA", key, legion.FieldPhase, `"implementing"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, database, "INFRA", now.Add(-time.Hour), "")

	// The session's title names the middle issue, on an event it wrote on the bottom one: the
	// middle carries no phase work yet.
	seedLegionRoleEvent(t, database, bottom, "s-990", "implementer", middle, now.Add(-45*time.Minute))
	if tree := legionCard(t, legionBoardOf(t, handler, "INFRA"), bottom).Tree; tree != bottom {
		t.Fatalf("the board puts %s in tree %q, want itself: the issue above it carries no phase work yet", bottom, tree)
	}

	// The same session writes on the middle issue carrying no title: the rule reads it under the
	// session's newest title, so the middle carries phase work from here and the bottom's tree
	// climbs past it.
	seedSessionEvent(t, database, middle, "s-990", "", now.Add(-30*time.Minute))
	if tree := legionCard(t, legionBoardOf(t, handler, "INFRA"), bottom).Tree; tree != top {
		t.Errorf("after the title-less event the board puts %s in tree %q, want %s: the event gave %s phase work", bottom, tree, top, middle)
	}
}

// TestAFailedLegionReadLeavesTheDeliveryFeedRunning: the board's reads, its phase-work read among
// them (legionBoardPopulation.readPhaseWork), are the Legion view's own, and a read that fails is
// that view's failure alone: the live hub retries the view (recompute), and the event feed every
// Delivery view of the hub reads (seedEvents) never waits on a Legion read. Here the phase-work
// read cannot succeed — the test's own database has its handoffs table renamed away — while a
// Legion board and a timeline are open on one hub. The board's read fails, and the timeline's feed
// must still carry every event the test writes.
//
// So the hub's generation makes no Legion read beside its feed's cursor: a read made there that
// failed would cancel the feed and try again, and while it failed no Delivery view of any
// deployment, with a Legion setting or without one, would hear an event.
func TestAFailedLegionReadLeavesTheDeliveryFeedRunning(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC()
	// A chain whose walk climbs onto an issue the daemon reports nothing of, so the board's read
	// reads that issue's phase work.
	top := seedLegionIssue(t, rig.database, "INFRA", 870, "The top, which the daemon reports", "a", nil, "")
	middle := seedLegionIssue(t, rig.database, "INFRA", 871, "The tracking issue the daemon reports nothing of", "b", nil, top)
	bottom := seedLegionIssue(t, rig.database, "INFRA", 872, "The bottom, which the daemon reports", "c", nil, middle)
	for _, key := range []string{top, bottom} {
		seedLegionObservation(t, rig.database, "INFRA", key, legion.FieldPhase, `"implementing"`, now.Add(-time.Hour))
	}
	seedLegionRead(t, rig.database, "INFRA", now.Add(-time.Minute), "")
	legionExec(t, rig.database, `alter table handoffs rename to handoffs_unreadable`)
	if _, err := rig.hub.server.readLegionBoard(t.Context(), everyReader, "INFRA"); err == nil {
		t.Fatal("the board's read succeeded with no handoffs table, so nothing here fails a Legion read")
	}

	board, timeline := newFakeLiveConn(), newFakeLiveConn()
	rig.hub.Subscribe(board, subscribeFrame("board", legionView("INFRA")))
	rig.hub.Subscribe(timeline, subscribeFrame("timeline", presetView(deliveryLiveTimeline, 7, "")))
	t.Cleanup(func() {
		rig.hub.Close(board)
		rig.hub.Close(timeline)
	})

	seedSessionEvent(t, rig.database, bottom, "s-870", "A person's own session", now)
	head := eventHead(t, rig.database)
	notifyChannel(t, rig.database, EventsChannel, "")
	rig.waitForEventCursor(t, head, "an event written while every Legion read fails")
}
