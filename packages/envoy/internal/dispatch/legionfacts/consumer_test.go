package legionfacts

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testBuild is the build every test's loop runs under: one string for one process, as a build
// is (Deps.Build), and never the empty string, which the loop refuses (run). A check that needs
// a second build writes that build into the project's record (build_whole_read_test.go's
// otherBuild) or runs a second loop under it (roll_test.go), and the check of the build a
// released binary carries starts two stamped binaries rather than one process
// (cmd/dispatch's TestASecondReleaseReadsTheProjectWholeAndRecordsItsOwnCommit).
const testBuild = "legion-envoy-v0.0.0-test-build"

// consumer is one test's loop: the seeded history, the Deps a pass reads through, and the clock
// each pass runs at, which the test moves rather than the wall clock. A pass at a moment the test
// chose is what lets a check say which rows a change moved, and that the clock moving alone moves
// none.
type consumer struct {
	history
	deps  Deps
	sched schedule
	at    time.Time
}

// newConsumer is a loop over a seeded history of trees trees, whose process started at the
// history's own moment, the moment its first pass runs at.
func newConsumer(t *testing.T, trees int) *consumer {
	t.Helper()
	seeded := seed(t, trees)
	return startConsumer(seeded, testBuild, seeded.now)
}

// startConsumer is a loop of build over the history, with a horizon of its own, whose process
// started at the moment given, the moment its first pass runs at: the process newConsumer starts,
// and the second process of a roll, which shares the first one's database (roll_test.go).
func startConsumer(seeded history, build string, at time.Time) *consumer {
	run := &consumer{history: seeded, at: at}
	run.deps = Deps{Pool: seeded.database.Pool, Build: build, Horizon: events.NewHorizon(seeded.database.Pool)}
	run.sched = schedule{page: feedPage, now: func() time.Time { return run.at }, started: at}
	return run
}

// restart is the loop's process starting again at the consumer's clock, under the same build:
// what a second process of one release is, which judges every record of a whole read against its
// own start (legionProjects).
func (c *consumer) restart() {
	c.deps.Horizon = events.NewHorizon(c.database.Pool)
	c.sched.started = c.at
}

// pass makes one pass as of the consumer's clock and answers the keys whose stored rows it
// changed, sorted.
//
// It awaits the event log's safe horizon first, so the pass reads every event written before it,
// as the server's pass reads the event whose commit woke it. A pass reads the horizon once and
// waits on nothing (pass), so a pass straight after a write can read a horizon that stands below
// it, and says so for its loop to ask for another soon (events.Horizon.HeldBack, runPass), which
// the server's loop runs and a rig would have to run itself. With the horizon awaited no event
// stands above what the pass reads, and attempt holds the pass to saying none did.
func (c *consumer) pass(t *testing.T) []string {
	t.Helper()
	changed, err := c.attempt(t)
	if err != nil {
		t.Fatalf("the Legion facts pass failed: %v", err)
	}
	return changed
}

// attempt is pass, with the failure left to the caller: what a check of a pass that fails reads.
func (c *consumer) attempt(t *testing.T) ([]string, error) {
	t.Helper()
	if _, err := c.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	changed, heldBack, err := pass(t.Context(), c.deps, c.sched)
	if err != nil {
		return nil, err
	}
	if heldBack {
		t.Errorf("a pass that read every event written before it says its safe horizon held one back")
	}
	slices.Sort(changed)
	return changed, nil
}

// exec runs one statement of a test's own setup.
func (c *consumer) exec(t *testing.T, what, statement string, args ...any) {
	t.Helper()
	if _, err := c.database.Pool.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// writeEvent writes one event of the issue at the moment given, as a Legion role's session
// wrote it: the actor carries the session title the phase-work rule reads
// (legiontest.SessionActor), so the event is work of that issue.
func (c *consumer) writeEvent(t *testing.T, issue, role, kind, payload string, at time.Time) {
	t.Helper()
	c.writeEventAs(t, issue, legiontest.SessionActor(role, issue), kind, payload, at)
}

// rows is every stored row of the history's project, by issue key, with every column (readWhole).
func (c *consumer) rows(t *testing.T) map[string]Row {
	t.Helper()
	stored, err := readWhole(t.Context(), c.database.Pool, c.fixture.Project)
	if err != nil {
		t.Fatalf("read the stored facts: %v", err)
	}
	return stored
}

// cursor is where one cursor stands and the moment the pass that wrote it computed as of.
type cursor struct {
	position  int64
	updatedAt time.Time
}

// cursors is the three cursor rows, by name, and no entry for a cursor with no row.
func (c *consumer) cursors(t *testing.T) map[string]cursor {
	t.Helper()
	rows, err := c.database.Pool.Query(t.Context(), `select name, position, updated_at from issue_facts_cursor`)
	if err != nil {
		t.Fatalf("read the cursors: %v", err)
	}
	defer rows.Close()
	cursors := map[string]cursor{}
	for rows.Next() {
		var name string
		var at cursor
		if err := rows.Scan(&name, &at.position, &at.updatedAt); err != nil {
			t.Fatalf("read the cursors: %v", err)
		}
		cursors[name] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the cursors: %v", err)
	}
	return cursors
}

// head is where a feed itself stands, which is where a pass that read all of it leaves its
// cursor.
func (c *consumer) head(t *testing.T, statement string) int64 {
	t.Helper()
	var position int64
	if err := c.database.Pool.QueryRow(t.Context(), statement).Scan(&position); err != nil {
		t.Fatalf("read a feed's head (%s): %v", statement, err)
	}
	return position
}

// feedHeads is where the feeds stand, by the cursor each is read by: the observations' head for
// both of that feed's cursors, which a pass that holds no observation leaves there.
func (c *consumer) feedHeads(t *testing.T) map[string]int64 {
	t.Helper()
	observations := c.head(t, `select coalesce(max(id), 0) from legion_observations`)
	return map[string]int64{
		eventsCursor:            c.head(t, `select coalesce(max(id), 0) from events`),
		observationsCursor:      observations,
		observationsAheadCursor: observations,
		changesCursor:           c.head(t, `select coalesce(max(seq), 0) from delivery_changes`),
	}
}

// statedSet is the set the pass about to run states for the project: the rows its changes touch
// through the links as they stand (rowsTouchedBy) and the rows of the tree a moved issue left
// (leftTrees), which is rowsToCompute of a page that read no repository move and no move of the
// admission block, read here from the same database at the same moment.
func statedSet(t *testing.T, run *consumer, keys []string, moved map[string]bool) []string {
	t.Helper()
	above, err := metrics.TreeLinks(t.Context(), run.database.Pool, run.fixture.Project)
	if err != nil {
		t.Fatalf("read the links above the project's issues: %v", err)
	}
	stated, err := rowsToCompute(t.Context(), run.database.Pool, run.fixture.Project, keys, moved, nil, false, above)
	if err != nil {
		t.Fatalf("read the set the pass states: %v", err)
	}
	return stated
}

// outside is the keys of got that want does not hold.
func outside(got, want []string) []string {
	left := []string{}
	for _, key := range got {
		if !slices.Contains(want, key) {
			left = append(left, key)
		}
	}
	return left
}

// TestOnePassWritesEveryIssueWithPhaseWorkAndAdvancesEveryCursor: one pass over a seeded project
// writes a row for every issue the rig says carries Legion phase work, and leaves each cursor at
// the head of its own feed. An observation after the moment a pass computes at is no input of
// that pass: the observations' page holds it, the observations cursor stays below it, and the
// pass says it held an input back, and a pass at a later moment reads it.
func TestOnePassWritesEveryIssueWithPhaseWorkAndAdvancesEveryCursor(t *testing.T) {
	run := newConsumer(t, 4)
	// A merged pull request of one issue, whose insert the delivery trigger logs as a change,
	// so the third feed has a row for the pass to read and its cursor a position to move to.
	run.exec(t, "seed a merged pull request", `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key)
		values ('cybermonklabs/legion', 1, 'A pull request', 'https://example.test/pull/1', 'alice', $1, $2, $3)
	`, run.at.Add(-2*time.Hour), run.at.Add(-time.Hour), run.fixture.Keys[1])

	changed := run.pass(t)
	stored := run.rows(t)

	for _, key := range run.fixture.PhaseWork {
		if _, has := stored[key]; !has {
			t.Errorf("%s carries Legion phase work and the pass stored no row for it", key)
		}
	}
	for _, key := range run.fixture.NoPhaseWork {
		if row, has := stored[key]; has {
			t.Errorf("%s has the stored row %s, and no Legion role worked it", key, show(row))
		}
	}
	if len(stored) != len(run.fixture.PhaseWork) {
		t.Errorf("the pass stored %d rows, want the %d issues with Legion phase work", len(stored), len(run.fixture.PhaseWork))
	}
	// A cursor stands at its feed's head once the pass has read it, with one exception the facts
	// feed makes for itself: the pass's own write appends an issue_facts change row for every row
	// it wrote, after it read that feed, so the delivery_changes head runs ahead of the cursor by
	// exactly those rows. Nothing is lost, and the second pass below is the proof: it reads them,
	// keeps none, writes nothing and brings every cursor to its head.
	heads, cursors := run.feedHeads(t), run.cursors(t)
	for _, name := range cursorNames {
		at, has := cursors[name]
		switch {
		case !has:
			t.Errorf("the %s cursor has no row after a pass", name)
		case at.position > heads[name]:
			t.Errorf("the %s cursor stands at %d, past its feed's head %d", name, at.position, heads[name])
		case at.position < heads[name] && name != changesCursor:
			t.Errorf("the %s cursor stands at %d, want its feed's head %d", name, at.position, heads[name])
		case at.position == 0:
			t.Errorf("the %s cursor stands at 0, so the pass read none of that feed", name)
		case !at.updatedAt.Equal(run.at):
			t.Errorf("the %s cursor is stamped %s, want the moment the pass computed as of, %s",
				name, at.updatedAt.UTC(), run.at.UTC())
		}
	}
	if rewritten := run.pass(t); len(rewritten) != 0 {
		t.Errorf("a second pass with nothing moved rewrote %d rows, want none: %v", len(rewritten), rewritten)
	}
	settled, afterwards := run.feedHeads(t), run.cursors(t)
	for _, name := range cursorNames {
		if afterwards[name].position != settled[name] {
			t.Errorf("after a second pass the %s cursor stands at %d, want its feed's head %d",
				name, afterwards[name].position, settled[name])
		}
	}

	// An observation of an open issue a minute after the moment the next pass computes at.
	open := run.fixture.Open[0]
	run.observe(t, open, legion.FieldPhase, legion.PhaseTesting, run.at.Add(time.Minute))
	before := run.rows(t)[open]
	if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	early, heldBack, err := pass(t.Context(), run.deps, run.sched)
	if err != nil {
		t.Fatalf("the pass before the observation's moment failed: %v", err)
	}
	if !heldBack {
		t.Errorf("the pass at %s met an observation of %s at %s and says it held no input back", run.at.UTC(), open, run.at.Add(time.Minute).UTC())
	}
	if at := run.cursors(t)[observationsCursor].position; at != settled[observationsCursor] {
		t.Errorf("the pass at %s moved the observations cursor to %d, past the observation after its moment; want it at %d",
			run.at.UTC(), at, settled[observationsCursor])
	}
	if slices.Contains(early, open) || !sameRow(run.rows(t)[open], before) {
		t.Errorf("the pass at %s changed %v and %s's row from an observation after its moment", run.at.UTC(), early, open)
	}
	run.at = run.at.Add(2 * time.Minute)
	later := run.pass(t)
	if !slices.Contains(later, open) {
		t.Errorf("the pass after the observation's moment changed %v, which does not hold %s", later, open)
	}
	if at, head := run.cursors(t)[observationsCursor].position, run.feedHeads(t)[observationsCursor]; at != head {
		t.Errorf("after the pass past the observation's moment the observations cursor stands at %d, want its feed's head %d", at, head)
	}
	t.Logf("one pass: %d issues seeded, %d with Legion phase work, %d rows stored, %d rows changed; cursors events=%d observations=%d delivery_changes=%d; an observation of %s after the moment held the observations cursor at %d until a pass past it changed %v",
		len(run.fixture.Keys), len(run.fixture.PhaseWork), len(stored), len(changed),
		cursors[eventsCursor].position, cursors[observationsCursor].position, cursors[changesCursor].position,
		open, settled[observationsCursor], later)
}

// TestAnEventChangesTheIssuesRowAndItsAncestorsAndNoRowOutsideThatSet: an event on one issue
// moves that issue's row and its ancestors' rows, because an issue's hours carry the time it
// waited while a child worked, and moves no row outside that set. The clock moves ten minutes
// between the two passes, and that moves no row: an open issue's row is closed at the newest input
// its facts read (metrics.ProjectFactsClosed), so the change's own rows are the only rows that
// can move.
//
// The event is a role session's first write on the follow-up a person filed, which no Legion
// role had worked: the issue carries Legion phase work from that event on, so the pass gives it
// the row a reader takes for "Legion worked this issue". The write is a status write that takes
// the follow-up out of `triage`, where it was filed: an hour in `triage` is out of the workflow,
// outside its work time (dispatch://INFRA-1077's 9f994275a7), so a follow-up worked while still
// in `triage` makes its root wait on nothing.
func TestAnEventChangesTheIssuesRowAndItsAncestorsAndNoRowOutsideThatSet(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	if _, had := run.rows(t)[run.fixture.NoPhaseWork[0]]; had {
		t.Fatalf("%s has a stored row before any role worked it", run.fixture.NoPhaseWork[0])
	}

	child := run.fixture.NoPhaseWork[0]
	root := run.fixture.Roots[0]
	run.writeEvent(t, child, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-10*time.Hour))

	want := statedSet(t, run, []string{child}, nil)
	run.at = run.at.Add(10 * time.Minute)
	changed := run.pass(t)
	strayed := outside(changed, want)

	if !slices.Contains(changed, child) {
		t.Errorf("the event is on %s and the pass changed %v, which does not hold it", child, changed)
	}
	if !slices.Contains(changed, root) {
		t.Errorf("%s is the root above the event's issue and the pass left its row alone; it changed %v", root, changed)
	}
	if len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the set the change touches: %v", len(strayed), strayed)
	}
	if !slices.Equal(want, []string{root, child}) && !slices.Equal(want, []string{child, root}) {
		t.Errorf("the set the change touches is %v, want the issue %s and its root %s alone", want, child, root)
	}
	t.Logf("an event on %s: the set it touches is %v (%d keys), %d rows changed (%v), %d of them outside that set",
		child, want, len(want), len(changed), changed, len(strayed))
}

// TestAMovedParentLinkChangesTheMovedIssuesSubtree: an issue whose parent link moved changes its
// own row, its new ancestors' rows and the rows of its whole subtree, because the root and the
// tree depth of every issue below it moved with the link. The moved issue is the second tree's
// root, whose subtree is two levels deep: its reviewed child is first moved under its implemented
// child. The rows the move changes, and the root and the depth each of them stores after it, are
// written out by hand from the rig's trees.
func TestAMovedParentLinkChangesTheMovedIssuesSubtree(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)

	// The second tree: its root, then its implemented, reviewed, open and no-measured-work
	// children; its last child, which no Legion role worked, holds no row.
	under := run.fixture.Roots[0]
	moved, implemented, reviewed, open, instant := run.fixture.Keys[6], run.fixture.Keys[7], run.fixture.Keys[8], run.fixture.Keys[9], run.fixture.Keys[10]
	if moved != run.fixture.Roots[1] {
		t.Fatalf("the second tree's first key is %s, not its root %s", moved, run.fixture.Roots[1])
	}
	run.exec(t, "move the reviewed child under the implemented one", `update issues set parent_key = $1 where key = $2`, implemented, reviewed)
	run.writeEvent(t, reviewed, "architect", "issue.updated", `{"changed":["parent"]}`, run.at.Add(-2*time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if stood := run.rows(t)[reviewed]; stood.Tree != moved || stood.TreeDepth != 2 {
		t.Fatalf("%s stands under %s and stores the root %s at depth %d, want %s at depth 2", reviewed, implemented, stood.Tree, stood.TreeDepth, moved)
	}
	before := run.rows(t)

	run.exec(t, "move a root under another", `update issues set parent_key = $1 where key = $2`, under, moved)
	run.writeEvent(t, moved, "architect", "issue.updated", `{"changed":["parent"]}`, run.at.Add(-time.Hour))
	run.at = run.at.Add(time.Minute)
	changed := run.pass(t)
	after := run.rows(t)

	// The moved root now stands one link below the root it moved under, its children two and its
	// reviewed child three, each storing that root; the root it moved under stays a root.
	depths := map[string]int{under: 0, moved: 1, implemented: 2, open: 2, instant: 2, reviewed: 3}
	for key, depth := range depths {
		is, stored := after[key]
		switch {
		case !stored:
			t.Errorf("%s holds no row after the move", key)
		case is.Tree != under || is.TreeDepth != depth:
			t.Errorf("%s stores the root %s at depth %d after the move, want %s at depth %d", key, is.Tree, is.TreeDepth, under, depth)
		}
	}
	if !after[under].Root || after[moved].Root {
		t.Errorf("after the move %s reads as a root %t and %s %t, want true and false", under, after[under].Root, moved, after[moved].Root)
	}
	for _, key := range []string{moved, implemented, reviewed, open, instant} {
		if !slices.Contains(changed, key) {
			t.Errorf("%s is the moved root or in its subtree and the pass left its row alone; it changed %v", key, changed)
		}
	}
	if strayed := outside(changed, slices.Sorted(maps.Keys(depths))); len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the moved root, its subtree and the root it moved under: %v", len(strayed), strayed)
	}
	t.Logf("%s moved under %s with a subtree two levels deep: the pass changed %d rows (%v); the moved root's depth went %d to %d and its reviewed child's %d to %d",
		moved, under, len(changed), changed, before[moved].TreeDepth, after[moved].TreeDepth, before[reviewed].TreeDepth, after[reviewed].TreeDepth)
}

// TestAChildMovedBetweenClosedRootsChangesBothRootsRows: an issue moved from one tree to
// another takes its hours with it, so the root it left stops counting the time it waited while
// that issue worked and the root it joined starts. Both roots are closed, and no pass computes a
// row its inputs did not name, so a pass that stated the new tree alone would leave the old
// root's row wrong for good.
func TestAChildMovedBetweenClosedRootsChangesBothRootsRows(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	before := run.rows(t)

	from, to := run.fixture.Roots[1], run.fixture.Roots[0]
	child := run.fixture.Keys[7]
	for _, key := range []string{from, to, child} {
		if before[key].ClosedAt == nil {
			t.Fatalf("%s is open, and this check needs the two roots and the child closed", key)
		}
	}
	if before[child].Tree != from {
		t.Fatalf("%s stands under %s before the move, want %s", child, before[child].Tree, from)
	}

	run.exec(t, "move a child to another tree", `update issues set parent_key = $1 where key = $2`, to, child)
	run.writeEvent(t, child, "architect", "issue.updated", `{"changed":["parent"]}`, run.at.Add(-time.Hour))

	want := statedSet(t, run, []string{child}, map[string]bool{child: true})
	changed := run.pass(t)
	strayed := outside(changed, want)
	after := run.rows(t)

	if after[child].Tree != to {
		t.Errorf("%s moved to %s and its stored root is %s", child, to, after[child].Tree)
	}
	if !slices.Contains(changed, to) {
		t.Errorf("%s is the root %s moved to and the pass left its row alone; it changed %v", to, child, changed)
	}
	if !slices.Contains(changed, from) {
		t.Errorf("%s is the root %s left and the pass left its row alone; it changed %v", from, child, changed)
	}
	if !slices.Contains(want, from) {
		t.Errorf("the set the pass stated is %v, which does not hold the root %s left", want, from)
	}
	if len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the set it stated: %v", len(strayed), strayed)
	}
	// Each of the two roots now holds the row a whole read of the project computes after the move.
	whole, _, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute the project whole after the move: %v", err)
	}
	checked := 0
	for _, row := range whole {
		if row.Issue != from && row.Issue != to {
			continue
		}
		checked++
		// The write's own moment is no fact: a rewrite at the moment of the row before it stores
		// a microsecond past it (upsert).
		row.UpdatedAt = after[row.Issue].UpdatedAt
		if difference := rowDifference(after[row.Issue], row); difference != "" {
			t.Errorf("%s, a root the move touched, stores a row a whole read of the project does not compute: %s", row.Issue, difference)
		}
	}
	if checked != 2 {
		t.Errorf("a whole read of the project computes %d of the two roots %s and %s", checked, from, to)
	}
	t.Logf("%s moved from %s to %s, both closed: the pass stated %v (%d keys), %d rows changed (%v), %d of them outside that set; the child's stored root went %s to %s, the root it left holds %v and the root it joined %v, each as a whole read computes it",
		child, from, to, want, len(want), len(changed), changed, len(strayed),
		before[child].Tree, after[child].Tree, after[from].PhaseHours, after[to].PhaseHours)
}

// TestAPassWithNoInputMovingWritesNoRowOpenOrClosed: with no input moving, passes ten minutes,
// two hours and three days after the first write no row, open or closed, and log no change of the
// facts, because a row is closed at the newest input its facts read and no clock moves it
// (metrics.ProjectFactsClosed). The open issue's facts still grow: a read ten minutes on answers
// its work hours grown by ten minutes at the rate its row stores, and its last moment of work at
// the read's clock, while a closed issue reads as stored.
func TestAPassWithNoInputMovingWritesNoRowOpenOrClosed(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	before := run.rows(t)
	logged := run.factsChanges(t)

	open := run.fixture.Open[0]
	closed := ""
	for _, key := range run.fixture.PhaseWork {
		if !slices.Contains(run.fixture.Open, key) && !slices.Contains(run.fixture.Roots, key) &&
			!slices.Contains(run.fixture.NoMeasuredWork, key) {
			closed = key
			break
		}
	}
	if closed == "" {
		t.Fatal("the rig wrote no closed issue with measured work, so this check has nothing to hold still")
	}
	if rate := before[open].Growth["work_hours"]; rate != 1 {
		t.Fatalf("%s is open and its row stores a work-hours growth of %g, want 1: this check reads the growth of an issue at work", open, rate)
	}
	if len(before[closed].Growth) != 0 {
		t.Errorf("%s is closed and its row stores the growth %v, want none", closed, before[closed].Growth)
	}

	started := run.at
	for _, later := range []time.Duration{10 * time.Minute, 2 * time.Hour, 72 * time.Hour} {
		run.at = started.Add(later)
		if changed := run.pass(t); len(changed) != 0 {
			t.Errorf("a pass %s on with no input moving changed %d rows: %v", later, len(changed), changed)
		}
	}
	after := run.rows(t)
	moved := 0
	for key, was := range before {
		if difference := rowDifference(after[key], was); difference != "" {
			moved++
			t.Errorf("%s moved over passes with no input moving: %s", key, difference)
		}
	}
	if more := run.factsChanges(t) - logged; more != 0 {
		t.Errorf("passes with no input moving logged %d issue_facts changes, want none", more)
	}

	readAt, tenOn := started, started.Add(10*time.Minute)
	first, err := readStored(t.Context(), run.database.Pool, run.fixture.Project, readAt)
	if err != nil {
		t.Fatalf("read the facts at %s: %v", readAt, err)
	}
	second, err := readStored(t.Context(), run.database.Pool, run.fixture.Project, tenOn)
	if err != nil {
		t.Fatalf("read the facts at %s: %v", tenOn, err)
	}
	grew := second[open].WorkHours - first[open].WorkHours
	if math.Abs(grew-tenOn.Sub(readAt).Hours()) > 1e-3 {
		t.Errorf("%s is open and its work hours read %g then %g ten minutes on, a growth of %g, want %g",
			open, first[open].WorkHours, second[open].WorkHours, grew, tenOn.Sub(readAt).Hours())
	}
	if last := second[open].WorkLastAt; last == nil || !last.Equal(tenOn) {
		t.Errorf("%s is open and at work, and its last moment of work reads %v at %s, want the read's clock", open, utc(last), tenOn.UTC())
	}
	if !sameRow(second[closed], first[closed]) {
		t.Errorf("%s is closed and its row read ten minutes on moved:\n\tgot  %s\n\twant %s", closed, show(second[closed]), show(first[closed]))
	}
	t.Logf("three passes with no input moving, %s to %s on: %d rows of %d moved and %d changes logged; %s (open) reads %g work hours then %g ten minutes on, %s (closed) reads as stored",
		10*time.Minute, 72*time.Hour, moved, len(after), run.factsChanges(t)-logged, open, first[open].WorkHours, second[open].WorkHours, closed)
}

// factsChanges is how many changes of kind issue_facts the change log and its outbox hold, which
// a write of a stored row records (migration 20261117100002).
func (c *consumer) factsChanges(t *testing.T) int {
	t.Helper()
	var count int
	if err := c.database.Pool.QueryRow(t.Context(), `
		select (select count(*) from delivery_changes where kind = 'issue_facts')
		     + (select count(*) from delivery_changes_pending where kind = 'issue_facts')
	`).Scan(&count); err != nil {
		t.Fatalf("count the issue_facts changes: %v", err)
	}
	return count
}

// TestAPassDrainsTheChangesItsWriteLeftPending (R7): the triggers of issue_facts and
// issue_filings record their changes in the outbox (delivery_changes_pending) and take no lock,
// and the pass moves them into the change log once its transaction has committed
// (writePass, delivery.DrainPending), with no drain by anyone else. After the first pass, a whole
// read, and after a pass that an event moves, the outbox holds nothing, and the change log holds
// one issue_facts change for each row the pass changed, naming the project and that row's issue.
func TestAPassDrainsTheChangesItsWriteLeftPending(t *testing.T) {
	run := newConsumer(t, 2)
	pending := func() int {
		t.Helper()
		var count int
		if err := run.database.Pool.QueryRow(t.Context(), `select count(*) from delivery_changes_pending`).Scan(&count); err != nil {
			t.Fatalf("count the outbox's changes: %v", err)
		}
		return count
	}
	// logged is the issue of each issue_facts change of the project the change log holds after seq.
	logged := func(seq int64) ([]string, int64) {
		t.Helper()
		rows, err := run.database.Pool.Query(t.Context(), `
			select ref, seq from delivery_changes where kind = 'issue_facts' and repo = $1 and seq > $2 order by ref`,
			run.fixture.Project, seq)
		if err != nil {
			t.Fatalf("read the issue_facts changes after %d: %v", seq, err)
		}
		refs, head := []string{}, seq
		var ref string
		var at int64
		if _, err := pgx.ForEachRow(rows, []any{&ref, &at}, func() error {
			refs, head = append(refs, ref), max(head, at)
			return nil
		}); err != nil {
			t.Fatalf("read the issue_facts changes after %d: %v", seq, err)
		}
		return refs, head
	}
	if left := pending(); left != 0 {
		t.Fatalf("the outbox holds %d changes before any pass, so this test cannot tell what the pass left there", left)
	}

	whole := run.pass(t)
	if len(whole) == 0 {
		t.Fatal("the first pass changed no row, so it left nothing in the outbox to drain: this test examined nothing")
	}
	if left := pending(); left != 0 {
		t.Errorf("the first pass wrote %d rows and left %d changes in the outbox", len(whole), left)
	}
	moved, head := logged(0)
	if !slices.Equal(moved, whole) {
		t.Errorf("after the first pass the change log holds issue_facts changes of %v, want one of each row it changed, %v", moved, whole)
	}

	open := run.fixture.Open[0]
	run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))
	run.at = run.at.Add(time.Minute)
	narrow := run.pass(t)
	if !slices.Contains(narrow, open) {
		t.Fatalf("the pass after an event on %s changed %v, which does not hold it: this test examined nothing", open, narrow)
	}
	if left := pending(); left != 0 {
		t.Errorf("the pass after an event wrote %d rows and left %d changes in the outbox", len(narrow), left)
	}
	if after, _ := logged(head); !slices.Equal(after, narrow) {
		t.Errorf("after the pass after an event the change log gained issue_facts changes of %v, want one of each row it changed, %v", after, narrow)
	}
	t.Logf("the first pass changed %d rows and the pass after an event %d; the outbox held nothing after either, and the change log held one issue_facts change of each changed row",
		len(whole), len(narrow))
}

// TestABackfillOverAnEmptiedTableRestoresEveryRow: a project whose last whole computation
// answered rows and which holds none now has lost them to something this loop did not do, and
// no narrow pass can name a row that is not there, so the pass reads the project whole
// (legionProjects, over the mark that whole read wrote). An emptied table is that case, and the
// rows it restores are the rows it held.
func TestABackfillOverAnEmptiedTableRestoresEveryRow(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	before := run.rows(t)
	if len(before) == 0 {
		t.Fatal("the first pass stored no row, so this check has nothing to restore")
	}

	run.exec(t, "empty the facts", `delete from issue_facts`)
	changed := run.pass(t)
	after := run.rows(t)

	if len(after) != len(before) {
		t.Errorf("the backfill restored %d rows, want the %d it held", len(after), len(before))
	}
	for key, was := range before {
		is, has := after[key]
		if !has {
			t.Errorf("the backfill restored no row for %s", key)
			continue
		}
		if !sameRow(is, was) {
			t.Errorf("the backfill restored %s as\n\tgot  %s\n\twant %s", key, show(is), show(was))
		}
	}
	t.Logf("a backfill over an emptied table: %d rows held, %d restored, %d of them written by the backfill",
		len(before), len(after), len(changed))
}

// TestAPassThatFailsPartWayLeavesItsCursorsWhereTheyWere: a cursor moves in the transaction that
// wrote the rows it covers, so a write that fails leaves every cursor where the last committed
// pass left it, and the next pass reads the same rows again.
func TestAPassThatFailsPartWayLeavesItsCursorsWhereTheyWere(t *testing.T) {
	run := newConsumer(t, 2)
	run.pass(t)
	before := run.cursors(t)

	// A trigger that refuses every write of a row: the pass computes its rows, the write of the
	// first of them fails, and the cursors it would have moved are in that transaction.
	run.refuseRowWrites(t)
	run.writeEvent(t, run.fixture.Open[0], "implementer", "comment.created", `{}`, run.at.Add(-time.Hour))
	run.at = run.at.Add(10 * time.Minute)

	changed, err := run.attempt(t)
	if err == nil {
		t.Fatalf("the pass over a table that refuses every write answered no error, and changed %v", changed)
	}
	after := run.cursors(t)

	if len(after) != len(before) {
		t.Errorf("the failed pass left %d cursor rows, want the %d the pass before it wrote", len(after), len(before))
	}
	for name, was := range before {
		is := after[name]
		if is.position != was.position {
			t.Errorf("the failed pass moved the %s cursor from %d to %d", name, was.position, is.position)
		}
		if !is.updatedAt.Equal(was.updatedAt) {
			t.Errorf("the failed pass stamped the %s cursor %s, from %s", name, is.updatedAt.UTC(), was.updatedAt.UTC())
		}
	}
	t.Logf("a pass that failed part way: %d cursors held at events=%d observations=%d delivery_changes=%d, stamped %s; the error was: %v",
		len(after), after[eventsCursor].position, after[observationsCursor].position,
		after[changesCursor].position, after[eventsCursor].updatedAt.UTC(), err)
}

// refuseRowWrites installs a trigger that refuses every write of a row, so a pass computes its
// rows and the write of the first of them fails, and answers the statement that drops it again.
func (c *consumer) refuseRowWrites(t *testing.T) string {
	t.Helper()
	c.exec(t, "refuse every write of a row", `
		create function refuse_issue_facts() returns trigger language plpgsql as $$
		begin raise exception 'this write is refused'; end $$;
		create trigger refuse_issue_facts before insert on issue_facts
		for each row execute function refuse_issue_facts()
	`)
	return `drop trigger refuse_issue_facts on issue_facts`
}

// TestAFailedPassAsksForAnotherARetryOn: a pass that fails leaves its cursors where they were, so
// the event whose wake started it is still unread, and the pass as the loop runs it (runPass)
// asks for the next one PassRetry on, by the wall clock the loop's timer runs on, rather than
// leave that event to an unrelated wake. Once the write is accepted again, that pass reads the
// event and writes its row.
func TestAFailedPassAsksForAnotherARetryOn(t *testing.T) {
	run := newConsumer(t, 2)
	run.pass(t)
	open := run.fixture.Open[0]
	accept := run.refuseRowWrites(t)
	run.at = run.at.Add(10 * time.Minute)
	run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))
	if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}

	asked := time.Now()
	due := runPass(t.Context(), run.deps, run.sched, &duty.Backoff{First: horizonRetry, Max: PassRetry})
	answered := time.Now()
	if due.Before(asked.Add(PassRetry)) || due.After(answered.Add(PassRetry)) {
		t.Errorf("the loop's pass over a table that refuses every write asks for its next %s after it was asked for, want PassRetry (%s) after it answered",
			due.Sub(asked), PassRetry)
	}

	run.exec(t, "accept every write of a row again", accept)
	run.at = run.at.Add(PassRetry)
	retried := run.pass(t)
	if !slices.Contains(retried, open) {
		t.Errorf("the pass the failed one asked for changed %v, which does not hold %s, whose event the failed pass left unread", retried, open)
	}
	t.Logf("a failed pass asked for its next %s after it began, PassRetry on, and that pass changed %d rows, %s among them",
		due.Sub(asked).Round(time.Millisecond), len(retried), open)
}

// TestAPassBelowAnOpenWriteWaitsForNothingAndAsksForAnotherSoon: a pass reads the event log up to
// the safe horizon it reads once (pass), and an event that committed while a write holding a
// lower event id is still open stands above it. The pass returns at once rather than wait for the
// lower write, so the lease it runs under is not held for as long as that write stays open: here
// a deadline stands in for the lease's other process, and a pass that waited would fail on it.
// The pass leaves the event unread and its issue's row as it was, and says its horizon held an
// event back, since the end of the lower write wakes nothing. The pass as the loop runs it
// (runPass) asks for the next horizonRetry on, and the one after that, still below the open
// write, twice that on. Once that write commits, the next pass reads the event.
func TestAPassBelowAnOpenWriteWaitsForNothingAndAsksForAnotherSoon(t *testing.T) {
	run := newConsumer(t, 2)
	ctx := t.Context()
	run.pass(t)
	// A pass whose horizon has proven nothing yet awaits it (SafeOrAwait), which the open write
	// below would hold; the first pass proved the seeded events.
	if run.deps.Horizon.Proven() == 0 {
		t.Fatal("the first pass proved no event, so the pass below would await the open write rather than read below it")
	}
	open := run.fixture.Open[0]
	before := run.rows(t)[open]

	lower, err := run.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower write: %v", err)
	}
	t.Cleanup(func() { _ = lower.Rollback(context.Background()) })
	// The write takes its transaction id before it draws its event id, as Broker.Append does: the
	// writer invariant events.Horizon's proof rests on.
	if _, err := lower.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the lower write's transaction id: %v", err)
	}
	var held int64
	if err := lower.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'legionfacts.test', '{"kind":"user","id":"alice"}', '{}', false)
		returning id`).Scan(&held); err != nil {
		t.Fatalf("draw the lower event id: %v", err)
	}
	run.at = run.at.Add(10 * time.Minute)
	run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))
	var event int64
	if err := run.database.Pool.QueryRow(ctx, `select max(id) from events where issue_key = $1`, open).Scan(&event); err != nil {
		t.Fatalf("read the event of %s: %v", open, err)
	}
	if event <= held {
		t.Fatalf("the event of %s drew id %d, not above the open write's %d", open, event, held)
	}

	// Each pass gets a deadline of its own, so a pass that waited for the open write fails on its
	// own deadline rather than on what the passes before it left of a shared one.
	bounded := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 5*time.Second) }
	first, cancel := bounded()
	defer cancel()
	started := time.Now()
	changed, heldBack, err := pass(first, run.deps, run.sched)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("the pass below the open write failed after %s, so it waited for the write: %v", took.Round(time.Millisecond), err)
	}
	if proven := run.deps.Horizon.Proven(); proven >= held {
		t.Fatalf("the horizon stands at %d while the write holding %d is open", proven, held)
	}
	if slices.Contains(changed, open) || !sameRow(run.rows(t)[open], before) {
		t.Errorf("the pass below the open write changed %v and %s's row, so it read past its safe horizon", changed, open)
	}
	if !heldBack {
		t.Errorf("the pass that left event %d above its safe horizon says its horizon held none back", event)
	}
	retry := &duty.Backoff{First: horizonRetry, Max: PassRetry}
	waits := []time.Duration{}
	for i, want := range []time.Duration{horizonRetry, 2 * horizonRetry} {
		within, cancel := bounded()
		asked := time.Now()
		due := runPass(within, run.deps, run.sched, retry)
		answered := time.Now()
		cancel()
		waits = append(waits, due.Sub(answered))
		if due.Before(asked.Add(want)) || due.After(answered.Add(want)) {
			t.Errorf("pass %d below the open write, as the loop runs it, asks for its next %s after it answered, want %s",
				i+1, due.Sub(answered), want)
		}
	}

	if err := lower.Commit(ctx); err != nil {
		t.Fatalf("commit the lower write: %v", err)
	}
	after := run.pass(t)
	if !slices.Contains(after, open) {
		t.Errorf("the pass after the lower write committed changed %v, which does not hold %s", after, open)
	}
	t.Logf("the pass below the open write (id %d) returned in %s and left event %d unread; the loop asked for the next %v on; the pass after the commit changed %d rows, %s among them",
		held, took.Round(time.Millisecond), event, waits, len(after), open)
}

// TestTheSetOfRowsOneChangeTouches: the set a change touches (rowsTouchedBy) is the issues named,
// the issues above them and the issues below them, whatever the change was, and a walk up a parent
// chain that loops ends. It is the set the pass computes before it writes; this test checks the
// set alone, and the pass tests above it hold that a pass writes no row outside it
// (TestAnEventChangesTheIssuesRowAndItsAncestorsAndNoRowOutsideThatSet).
func TestTheSetOfRowsOneChangeTouches(t *testing.T) {
	// One tree: ROOT over MIDDLE over LEAF and SIBLING, with OTHER a root of its own, and a
	// pair whose parent links point at each other, which the walk must not follow for ever.
	parents := map[string]string{
		"ROOT": "", "MIDDLE": "ROOT", "LEAF": "MIDDLE", "SIBLING": "MIDDLE",
		"OTHER": "", "LOOP-1": "LOOP-2", "LOOP-2": "LOOP-1",
	}
	for _, check := range []struct {
		what string
		keys []string
		want []string
	}{
		{"an issue and the issues above it", []string{"LEAF"}, []string{"LEAF", "MIDDLE", "ROOT"}},
		{"a root, with the issues below it", []string{"ROOT"}, []string{"LEAF", "MIDDLE", "ROOT", "SIBLING"}},
		{"two issues of one tree", []string{"LEAF", "SIBLING"}, []string{"LEAF", "MIDDLE", "ROOT", "SIBLING"}},
		{"an issue with issues above it and below it", []string{"MIDDLE"},
			[]string{"LEAF", "MIDDLE", "ROOT", "SIBLING"}},
		{"a leaf, which has no issue below it", []string{"SIBLING"},
			[]string{"MIDDLE", "ROOT", "SIBLING"}},
		{"an issue of another tree", []string{"OTHER"}, []string{"OTHER"}},
		{"a parent chain that loops", []string{"LOOP-1"}, []string{"LOOP-1", "LOOP-2"}},
	} {
		got := rowsTouchedBy(check.keys, parents)
		if !slices.Equal(got, check.want) {
			t.Errorf("%s: %v touches %v, want %v", check.what, check.keys, got, check.want)
		}
		t.Logf("%s: %v touches %d rows (%s)", check.what, check.keys, len(got), strings.Join(got, " "))
	}
}

// TestCursorAgeAnswersTheOldestCursor: the board's header reads how fresh the served facts are
// from the cursors, and a database no pass has committed on has no cursor row at all. The three
// cursors are stamped at three moments, the observations' the oldest, so the age read is the
// oldest of them and neither the newest nor the change cursor's.
func TestCursorAgeAnswersTheOldestCursor(t *testing.T) {
	run := newConsumer(t, 1)

	age, has, err := CursorAge(t.Context(), run.database.Pool, run.at)
	if err != nil {
		t.Fatalf("read the cursors' age: %v", err)
	}
	if has {
		t.Errorf("a database no pass has committed on answers an age of %s", age)
	}

	run.pass(t)
	stamps := map[string]time.Duration{eventsCursor: time.Minute, observationsCursor: 3 * time.Minute, changesCursor: 2 * time.Minute}
	for name, earlier := range stamps {
		run.exec(t, "stamp the "+name+" cursor", `update issue_facts_cursor set updated_at = $2 where name = $1`, name, run.at.Add(-earlier))
	}
	age, has, err = CursorAge(t.Context(), run.database.Pool, run.at.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("read the cursors' age: %v", err)
	}
	if !has {
		t.Fatal("a database one pass has committed on answers no age")
	}
	if want := 8 * time.Minute; age != want {
		t.Errorf("five minutes after the pass, with the cursors stamped 1, 3 and 2 minutes before it, the facts are %s old, want the oldest cursor's %s", age, want)
	}
	t.Logf("the cursors' age: none before a pass, and %s five minutes after one whose cursors were stamped %v before it", age, stamps)
}

// loopLatencyBound is how long after an event's commit the loop may take to store the row the
// event moves in TestAnEventAfterTheBootPassMovesItsRowAtOnce: a pass of the rig's size takes well
// under a second, so the bound holds on a loaded machine, and a loop whose MinGap held a pass to
// 10 s after the start of the one before it would take about twice the bound.
const loopLatencyBound = 5 * time.Second

// TestAnEventAfterTheBootPassMovesItsRowAtOnce: the loop as the server runs it (run, under its
// lease), woken by hand as the hub wakes it. An event of an open issue written once the boot pass
// ended wakes the pass that moves the issue's row within loopLatencyBound of the event's commit,
// since nothing holds one pass back from the next (the loop's MinGap is zero).
func TestAnEventAfterTheBootPassMovesItsRowAtOnce(t *testing.T) {
	c := newConsumer(t, 2)
	c.at = c.at.Add(10 * time.Minute)
	open := c.fixture.Open[0]
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)
	wake := make(chan struct{}, 1)
	deps := c.deps
	deps.Leases = storetest.Leases(t, c.database.Pool)
	deps.Events = wake
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, deps, c.sched)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// ran is how many passes have run: duty.Exclusive logs each one that ran under the lease, as
	// it ends.
	ran := func() int {
		n := 0
		for _, record := range recorder.Records() {
			if record.Message != "dispatch duty: leased pass ran" {
				continue
			}
			record.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "lease" && attr.Value.String() == Lease {
					n++
					return false
				}
				return true
			})
		}
		return n
	}
	testwait.Until(t, "the loop's boot pass to end", func() (int, bool) {
		n := ran()
		return n, n >= 1
	})
	booted := time.Now()

	before := c.rows(t)[open]
	c.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, c.at.Add(-time.Minute))
	committed := time.Now()
	select {
	case wake <- struct{}{}:
	default:
	}
	moved := testwait.Until(t, "the pass the event of "+open+" woke to move its row", func() (time.Time, bool) {
		if sameRow(c.rows(t)[open], before) {
			return time.Time{}, false
		}
		return time.Now(), true
	})
	latency := moved.Sub(committed)
	if latency > loopLatencyBound {
		t.Errorf("the event of %s, committed %s after the boot pass ended, moved its row %s after its commit, want within %s",
			open, committed.Sub(booted).Round(time.Millisecond), latency.Round(time.Millisecond), loopLatencyBound)
	}
	t.Logf("the event of %s, committed %s after the boot pass ended, moved its row %s after its commit (bound %s), in the loop's pass %d",
		open, committed.Sub(booted).Round(time.Millisecond), latency.Round(time.Millisecond), loopLatencyBound, ran())
}

// TestAPassHoldsTheFreshnessRequirement: a stored row stands no more than Freshness behind the
// event that changed it, at the rig's own population and at four times it. No pass runs on a
// timer and none is held back from the one before it: a commit wakes the loop, whose next pass
// starts as soon as the pass running when the event committed ends, and a process that finds the
// lease held tries again PassRetry later and up to a tenth again. The pass the event waits behind
// and its own pass therefore share Freshness less one retry, 49 s, and the longest such pair is a
// whole read and the pass after an event.
//
// Each size takes two lengths and two reads: the first pass of a new build, which reads the
// project whole (ComputeProject and WriteProject, in the loop's own pass) once per project; one
// pass that reads one event of one open issue, which is what a wake runs; and the read of the
// rows another build wrote (otherBuildsRows), which every first page of the build a project's
// record names makes, timed with none to find, as a settled deployment holds, and with every row
// of the project written by another build, as a roll's first pass can find them.
//
// The rig's own population is 252 issues (legiontest.DefaultTrees trees), and the second size is
// four times it, so a reader sees how the costs grow with the population rather than one number
// at one size. INFRA holds 13,103 issues, and the cost of a whole read at that size is the R8
// Job's (legion/legionbench), which scope.go's legionProjects records.
func TestAPassHoldsTheFreshnessRequirement(t *testing.T) {
	budget := Freshness - (PassRetry + PassRetry/10)
	for _, size := range []struct {
		what  string
		trees int
	}{
		{"the rig's own population", legiontest.DefaultTrees},
		{"four times the rig's population", 4 * legiontest.DefaultTrees},
	} {
		t.Run(size.what, func(t *testing.T) {
			run := newConsumer(t, size.trees)
			project := run.fixture.Project

			// The first pass: it finds no record of a whole read, reads the project whole,
			// records it and leaves the three feeds at their heads, so the pass after the event
			// reads that event alone.
			started := time.Now()
			written := run.pass(t)
			whole := time.Since(started)
			open := run.fixture.Open[0]
			run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))
			// The horizon is awaited before the timer starts, so the length is the pass's own:
			// the wait is this test's (consumer.pass), which a server's woken pass does not make.
			if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
				t.Fatalf("await the event log's safe horizon: %v", err)
			}
			started = time.Now()
			changed, heldBack, err := pass(t.Context(), run.deps, run.sched)
			woken := time.Since(started)
			if err != nil {
				t.Fatalf("the pass after an event on %s failed: %v", open, err)
			}
			if heldBack {
				t.Errorf("a pass that read every event written before it says its safe horizon held one back")
			}
			if !slices.Contains(changed, open) {
				t.Errorf("the pass after an event on %s changed %v, which does not hold it", open, changed)
			}

			settled, settledRows := timeOtherBuildsRows(t, run)
			run.exec(t, "stamp every row with another build", `update issue_facts set written_build = $1 where project_key = $2`, otherBuild, project)
			rolled, rolledRows := timeOtherBuildsRows(t, run)

			for _, length := range []struct {
				what string
				took time.Duration
			}{{"one whole read", whole}, {"one pass after an event", woken}, {"a whole read and then the pass after an event", whole + woken}} {
				if length.took >= budget {
					t.Errorf("at %d issues %s takes %s, above the %s the pass an event waits behind and its own pass share",
						len(run.fixture.Keys), length.what, length.took.Round(time.Millisecond), budget)
				}
			}
			if settledRows != 0 {
				t.Errorf("with every row of %s written by this build the read of the rows another build wrote answered %d rows, want none", project, settledRows)
			}
			if stored := len(run.rows(t)); rolledRows != stored {
				t.Errorf("with every row of %s written by another build the read answered %d rows, want the %d stored", project, rolledRows, stored)
			}
			t.Logf("over %d issues, %d of them with Legion phase work: one whole read took %s and wrote %d rows; one pass after an event took %s and changed %d rows; against the %s the pass an event waits behind and its own pass share",
				len(run.fixture.Keys), len(run.fixture.PhaseWork), whole.Round(time.Millisecond), len(written),
				woken.Round(time.Millisecond), len(changed), budget)
			t.Logf("the read of the rows another build wrote took %s finding %d rows, and %s finding %d with every row of the project written by another build",
				settled.Round(10*time.Microsecond), settledRows, rolled.Round(10*time.Microsecond), rolledRows)
		})
	}
}

// timeOtherBuildsRows times otherBuildsRows under the consumer's build, best of five, and answers
// how many rows of the history's project it found.
func timeOtherBuildsRows(t *testing.T, run *consumer) (time.Duration, int) {
	t.Helper()
	best, found := time.Duration(0), 0
	for attempt := range 5 {
		started := time.Now()
		other, err := otherBuildsRows(t.Context(), run.database.Pool, run.deps.Build)
		took := time.Since(started)
		if err != nil {
			t.Fatalf("read the rows another build wrote: %v", err)
		}
		if attempt == 0 || took < best {
			best = took
		}
		found = len(other[run.fixture.Project])
	}
	return best, found
}
