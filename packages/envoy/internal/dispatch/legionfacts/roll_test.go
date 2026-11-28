package legionfacts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// writers is the build each stored row of the history's project records as its writer, by key.
func (c *consumer) writers(t *testing.T) map[string]string {
	t.Helper()
	return c.writtenIn(t, "issue_facts")
}

// filingWriters is the build each stored filing of the history's project records as its writer,
// by filed issue.
func (c *consumer) filingWriters(t *testing.T) map[string]string {
	t.Helper()
	return c.writtenIn(t, "issue_filings")
}

// writtenIn is the writer of each row of table, issue_facts or issue_filings, of the history's
// project, by issue key.
func (c *consumer) writtenIn(t *testing.T, table string) map[string]string {
	t.Helper()
	rows, err := c.database.Pool.Query(t.Context(),
		`select issue_key, written_build from `+table+` where project_key = $1`, c.fixture.Project)
	if err != nil {
		t.Fatalf("read the writers of %s: %v", table, err)
	}
	defer rows.Close()
	writers := map[string]string{}
	for rows.Next() {
		var key, build string
		if err := rows.Scan(&key, &build); err != nil {
			t.Fatalf("read the writers of %s: %v", table, err)
		}
		writers[key] = build
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the writers of %s: %v", table, err)
	}
	return writers
}

// writtenBy is the keys of writers whose writer is build, sorted.
func writtenBy(writers map[string]string, build string) []string {
	keys := []string{}
	for key, writer := range writers {
		if writer == build {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// deliveriesWake is the consumer's Deps.Deliveries as the server wires it: the wake of a real
// LISTEN on delivery.ChangesChannel (pglisten.Hub, the hub's wake cmd/dispatch hands the loop),
// once that LISTEN holds. The hub signals every wake once its LISTEN holds, so a subscriber
// catches up what it missed; that one signal is taken here, and the wake then carries the
// notifications of changes drained after it alone.
func deliveriesWake(t *testing.T, c *consumer) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "legionfacts roll test", Dial: c.database.Pool.Listener}
	wake := hub.Wake(delivery.ChangesChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	testwait.Receive(t, "the LISTEN on delivery.ChangesChannel", connected)
	testwait.Receive(t, "the wake the LISTEN on delivery.ChangesChannel sends once it holds", wake)
	return wake
}

// TestARollEndsWithEveryRowTheNewerBuildsAndNoBuildWritesWithNoInput is how a roll ends with no
// timer (R27, otherBuildsRows, otherBuildsFilings): two loops of two builds on one database,
// sharing its cursors as two processes of a roll do, the newer started after the older read the
// project whole.
//
//  1. The newer process's first pass reads the project whole, since the record names a build
//     recorded before it started, and records its own build.
//  2. The older process, still serving, writes rows by its own rule for an event its feed reads,
//     and neither reads the project whole nor takes the record back, since the record names a
//     build recorded after it started.
//  3. The newer process's next pass, which the older write's change wakes - the newer process's
//     Deps.Deliveries is a real LISTEN on delivery.ChangesChannel (pglisten.Hub), as the server
//     wires it, and the pass runs once that wake arrives - computes every row and filing the
//     older wrote again, so none is left that another build wrote.
//  4. The older process meets a row and a filing its rule computes none for and the newer
//     build's keeps. It deletes neither, since one it deleted would leave the newer build
//     nothing to compute again, and stamps both with its own build; the newer process's next
//     pass computes them again and decides them by its own rule, as it does a filing the older
//     build wrote that no row another build wrote names.
//  5. The older process's next pass rewrites nothing of the newer's, and with no input moving
//     neither process writes a row, however far the clock moves.
func TestARollEndsWithEveryRowTheNewerBuildsAndNoBuildWritesWithNoInput(t *testing.T) {
	older := newConsumer(t, 3)
	project := older.fixture.Project
	older.pass(t)
	if build, _ := markedBuild(t, older); build != testBuild {
		t.Fatalf("the older process's first pass recorded %q, want %q", build, testBuild)
	}

	newer := startConsumer(older.history, otherBuild, older.at.Add(time.Minute))

	// 1. The newer build reads the project whole and records itself.
	first := newer.pass(t)
	if build, _ := markedBuild(t, newer); build != otherBuild {
		t.Fatalf("the newer process's first pass left the record naming %q, want its own build %q", build, otherBuild)
	}
	if left := writtenBy(newer.writers(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build's whole read %d rows still name the older build as their writer: %v", len(left), left)
	}
	if left := writtenBy(newer.filingWriters(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build's whole read %d filings still name the older build as their writer: %v", len(left), left)
	}
	recordedAt := computedWholeAt(t, newer)[project]
	newer.deps.Deliveries = deliveriesWake(t, newer)

	// 2. The older build writes rows for an event its feed reads.
	older.at = newer.at.Add(time.Minute)
	issue := older.fixture.Open[0]
	older.writeEvent(t, issue, "implementer", "comment.created", `{"status":"in_progress"}`, older.at.Add(-time.Minute))
	olderWrote := older.pass(t)
	if !slices.Contains(olderWrote, issue) {
		t.Fatalf("the older process's pass after an event on %s wrote %v, which does not hold it: this test examined nothing", issue, olderWrote)
	}
	if build, _ := markedBuild(t, older); build != otherBuild {
		t.Errorf("the older process took the record: it names %q, want the newer build %q", build, otherBuild)
	}
	if at := computedWholeAt(t, older)[project]; !at.Equal(recordedAt) {
		t.Errorf("the older process read %s whole at %s; the newer build had recorded it at %s", project, at, recordedAt)
	}
	stamped := writtenBy(older.writers(t), testBuild)
	if !slices.Equal(stamped, olderWrote) {
		t.Errorf("the rows that name the older build are %v, want the %v its pass wrote", stamped, olderWrote)
	}

	// 3. The older build's write wakes the newer build, whose next pass computes them again.
	testwait.Receive(t, "the wake the older build's write sends the newer build on delivery.ChangesChannel", newer.deps.Deliveries)
	newer.at = older.at.Add(time.Second)
	corrected := newer.pass(t)
	if left := writtenBy(newer.writers(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build's next pass %d rows still name the older build as their writer: %v", len(left), left)
	}
	if left := writtenBy(newer.filingWriters(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build's next pass %d filings still name the older build as their writer: %v", len(left), left)
	}
	for _, key := range stamped {
		if !slices.Contains(corrected, key) {
			t.Errorf("%s was written by the older build and the newer build's next pass left it alone; it changed %v", key, corrected)
		}
	}
	if outsideIt := outside(corrected, stamped); len(outsideIt) > 0 {
		t.Errorf("the newer build's correcting pass changed %d rows the older build had not written: %v", len(outsideIt), outsideIt)
	}

	// 4. A row and a filing the newer build's rule keeps and the older's computes none for. The
	// two builds of this test share one rule, so the newer build's row and filing are written by
	// hand, as that build would write them, of an issue no Legion role worked and no Legion
	// session filed, and a person's comment on that issue names it to the older build's feed.
	kept := older.fixture.NoPhaseWork[0]
	if _, filed := older.filingWriters(t)[kept]; filed {
		t.Fatalf("%s holds a filing a Legion session made, so the older build computes one for it and this case examines nothing", kept)
	}
	if _, err := WriteProject(t.Context(), older.database.Pool, ProjectWrite{Project: project, Build: otherBuild, Rows: []Row{{
		Project: project, Issue: kept, UpdatedAt: newer.at, ClosedFacts: metrics.ClosedFacts{
			IssueFacts: metrics.IssueFacts{OpenedAt: newer.at.Add(-48 * time.Hour), Root: true, Tree: kept}, AsOf: newer.at,
		},
	}}}); err != nil {
		t.Fatalf("write the newer build's row of %s: %v", kept, err)
	}
	if err := WriteFilings(t.Context(), older.database.Pool, FilingsWrite{Project: project, Build: otherBuild, Filings: []Filing{{
		Project: project, IssueFiling: metrics.IssueFiling{FiledIssue: metrics.FiledIssue{
			Key: kept, At: newer.at.Add(-time.Hour), Filer: "implementer", FilerIssue: older.fixture.Open[0], Class: metrics.ClassOutOfTree,
		}, Root: kept},
	}}}); err != nil {
		t.Fatalf("write the newer build's filing of %s: %v", kept, err)
	}
	// A filing the older build wrote by its own rule, a controller's of an issue of the last tree,
	// which no row another build wrote names: neither its filed issue nor its filer's issue (the
	// controller works none) is a key the newer build computes for a row, so only the read of the
	// filings another build wrote brings it to the newer build (otherBuildsFilings).
	lastTree := older.fixture.Keys[slices.Index(older.fixture.Keys, older.fixture.Roots[len(older.fixture.Roots)-1]):]
	lone := ""
	for _, key := range older.fixture.NoPhaseWork {
		if slices.Contains(lastTree, key) {
			lone = key
		}
	}
	if _, filed := older.filingWriters(t)[lone]; lone == "" || filed || slices.Contains(lastTree, kept) || slices.Contains(lastTree, issue) {
		t.Fatalf("the last tree %v holds no issue no Legion role worked or filed apart from %s and %s, so the lone filing case examines nothing", lastTree, kept, issue)
	}
	if err := WriteFilings(t.Context(), older.database.Pool, FilingsWrite{Project: project, Build: testBuild, Filings: []Filing{{
		Project: project, IssueFiling: metrics.IssueFiling{FiledIssue: metrics.FiledIssue{
			Key: lone, At: newer.at.Add(-time.Hour), Filer: "controller", Class: metrics.ClassByTheController,
		}, Root: lone},
	}}}); err != nil {
		t.Fatalf("write the older build's filing of %s: %v", lone, err)
	}
	older.at = newer.at.Add(time.Minute)
	older.exec(t, "comment on "+kept+" as a person", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
		        'comment.created', '{"kind":"user","id":"alice"}', '{}', false, $2)
	`, kept, older.at.Add(-time.Minute))
	handed := older.pass(t)
	switch writer, stands := older.writers(t)[kept]; {
	case !stands:
		t.Errorf("the older build deleted %s's row, which the newer build wrote: no row of the older build is left for the newer build to compute again", kept)
	case writer != testBuild || !slices.Contains(handed, kept):
		t.Errorf("after the older build's pass, which computed no row for %s and changed %v, its row names %q as its writer, want the older build %q",
			kept, handed, writer, testBuild)
	}
	switch writer, stands := older.filingWriters(t)[kept]; {
	case !stands:
		t.Errorf("the older build deleted %s's filing, which the newer build wrote: no filing of the older build is left for the newer build to compute again", kept)
	case writer != testBuild:
		t.Errorf("after the older build's pass, which computed no filing of %s, its filing names %q as its writer, want the older build %q", kept, writer, testBuild)
	}
	newer.at = older.at.Add(time.Second)
	decided := newer.pass(t)
	if _, stands := newer.writers(t)[kept]; stands || !slices.Contains(decided, kept) {
		t.Errorf("the newer build's next pass changed %v and left %s's row standing (%t): its rule, the older's here, computes none for it", decided, kept, stands)
	}
	for _, filed := range []string{kept, lone} {
		if _, stands := newer.filingWriters(t)[filed]; stands {
			t.Errorf("the newer build's next pass left %s's filing standing: its rule, the older's here, computes none for it", filed)
		}
	}
	if left := writtenBy(newer.writers(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build decided the stamped row, %d rows still name the older build as their writer: %v", len(left), left)
	}
	if left := writtenBy(newer.filingWriters(t), testBuild); len(left) != 0 {
		t.Errorf("after the newer build decided the stamped filing, %d filings still name the older build as their writer: %v", len(left), left)
	}

	// 5. The older build rewrites nothing of the newer's, and with no input neither writes.
	writers, filingWriters := newer.writers(t), newer.filingWriters(t)
	for _, later := range []time.Duration{time.Minute, time.Hour, 72 * time.Hour} {
		older.at, newer.at = newer.at.Add(later), newer.at.Add(later)
		if changed := older.pass(t); len(changed) != 0 {
			t.Errorf("the older process's pass %s on, with no input moving, changed %v", later, changed)
		}
		if changed := newer.pass(t); len(changed) != 0 {
			t.Errorf("the newer process's pass %s on, with no input moving, changed %v", later, changed)
		}
	}
	if after := newer.writers(t); !maps.Equal(after, writers) {
		t.Errorf("the rows' writers moved over passes with no input moving: %v, from %v", after, writers)
	}
	if after := newer.filingWriters(t); !maps.Equal(after, filingWriters) {
		t.Errorf("the filings' writers moved over passes with no input moving: %v, from %v", after, filingWriters)
	}
	t.Logf("the newer build's whole read changed %d rows; the older build then wrote %d rows by its own rule (%v); the newer build's next pass computed %d again (%v); the older build stamped %s's row and filing rather than delete them (its pass changed %v) and the newer build's next pass decided them (%v); %d rows and %d filings name the newer build; three passes of each with no input wrote nothing",
		len(first), len(olderWrote), olderWrote, len(corrected), corrected, kept, handed, decided,
		len(writtenBy(writers, otherBuild)), len(writtenBy(filingWriters, otherBuild)))
}

// The two releases of TestAPreviousReleaseThatStartsAgainDuringARollDoesNotKeepItsRowsAfterItStops
// and TestTheRunningBuildReadsEachProjectWholeOnceAfterTheLastProcessOfAnotherBuildStops: commits
// as the image build stamps them (40 hex characters), which the registry of live processes names
// each process by.
const (
	previousRelease = "1111111111111111111111111111111111111111"
	newRelease      = "2222222222222222222222222222222222222222"
)

// register records the consumer's process in the registry of live processes (processes.Register),
// as the server registers itself, under the commit a release stamped or "" for a build no release
// stamped, and answers the stop of that process: its registration ends (Deregister), and the
// process leaves processes.Live. The test's cleanup stops a process the test left running.
func register(t *testing.T, c *consumer, commit string) func() {
	t.Helper()
	registration, err := processes.Register(t.Context(), c.database.Pool, processes.Instance{
		Commit: commit, SigningKeys: []string{"legionfacts-roll-test-key"},
	})
	if err != nil {
		t.Fatalf("register the process of build %q: %v", c.deps.Build, err)
	}
	stop := sync.OnceFunc(func() {
		if err := registration.Deregister(context.Background()); err != nil {
			t.Errorf("deregister the process of build %q: %v", c.deps.Build, err)
		}
	})
	t.Cleanup(stop)
	return stop
}

// TestAPreviousReleaseThatStartsAgainDuringARollDoesNotKeepItsRowsAfterItStops is k30 (R3): a
// process yields to a record of another build written after it started, and stops yielding once
// no live process serves that build (processes.Live), so a process of the previous release that
// starts again during a roll and takes the record leaves no row of its rule once it stops.
//
// Two stamped releases share one database, each process registered as the server registers it.
// The new release reads the project whole and records itself. A process of the previous release
// starts, reads the project whole, takes the record and keeps it while the new release yields to
// it. It stops, and the new release's next pass reads the project whole once, takes the record
// and leaves every row its own; the pass after it reads nothing whole.
//
// Two builds no release stamped register no commit, so the registry cannot say whether the build
// a record names still runs: the process that yields keeps yielding, as before, and the two never
// trade whole reads however many passes each makes. The run records the whole reads of each
// process.
func TestAPreviousReleaseThatStartsAgainDuringARollDoesNotKeepItsRowsAfterItStops(t *testing.T) {
	t.Run("two stamped releases", func(t *testing.T) {
		seeded := seed(t, 3)
		project := seeded.fixture.Project
		newer := startConsumer(seeded, newRelease, seeded.now)
		register(t, newer, newRelease)
		reads := map[string]int{}
		passOf := func(run *consumer, what string) passReading {
			t.Helper()
			reading := makePass(t, run)
			if reading.whole {
				reads[run.deps.Build]++
			}
			t.Logf("%s %s", what, reading)
			return reading
		}

		if first := passOf(newer, "the new release's first pass"); !first.whole {
			t.Fatalf("the new release's first pass read nothing of %s whole, which no build had read whole", project)
		}

		previous := startConsumer(seeded, previousRelease, newer.at.Add(time.Minute))
		stopPrevious := register(t, previous, previousRelease)
		if restarted := passOf(previous, "the previous release's first pass"); !restarted.whole {
			t.Fatalf("the previous release's first pass read nothing of %s whole, whose record names a build recorded before it started", project)
		}
		if build, _ := markedBuild(t, previous); build != previousRelease {
			t.Fatalf("the previous release's whole read left the record naming %q, want %q", build, previousRelease)
		}

		// The previous release still serves, so the new release yields to its record.
		newer.at = previous.at.Add(time.Minute)
		if yielded := passOf(newer, "the new release's pass beside the previous release"); yielded.whole {
			t.Errorf("the new release read %s whole while a process of the previous release, whose record it names, still serves", project)
		}
		if build, _ := markedBuild(t, newer); build != previousRelease {
			t.Errorf("the new release took the record while the previous release serves: it names %q", build)
		}

		// The previous release's process stops.
		stopPrevious()
		newer.at = newer.at.Add(time.Minute)
		if taken := passOf(newer, "the new release's pass after the previous release stopped"); !taken.whole {
			t.Errorf("the new release read nothing of %s whole once no live process served the build %q its record names", project, previousRelease)
		}
		if build, _ := markedBuild(t, newer); build != newRelease {
			t.Errorf("after the previous release stopped the record names %q, want the new release %q", build, newRelease)
		}
		if left := writtenBy(newer.writers(t), previousRelease); len(left) != 0 {
			t.Errorf("after the previous release stopped %d rows still name it as their writer: %v", len(left), left)
		}
		if left := writtenBy(newer.filingWriters(t), previousRelease); len(left) != 0 {
			t.Errorf("after the previous release stopped %d filings still name it as their writer: %v", len(left), left)
		}
		if mine := writtenBy(newer.writers(t), newRelease); len(mine) != len(seeded.fixture.PhaseWork) {
			t.Errorf("%d rows name the new release as their writer, want every one of the %d issues with Legion phase work", len(mine), len(seeded.fixture.PhaseWork))
		}

		newer.at = newer.at.Add(time.Minute)
		if settled := passOf(newer, "the new release's next pass"); settled.whole || settled.wrote != 0 {
			t.Errorf("the new release's pass after it took the record back read whole (%t) and wrote %d rows, want neither", settled.whole, settled.wrote)
		}
		if reads[newRelease] != 2 || reads[previousRelease] != 1 {
			t.Errorf("the new release read %s whole %d times and the previous release %d times, want 2 and 1", project, reads[newRelease], reads[previousRelease])
		}
		t.Logf("whole reads of %s: the new release %d, the previous release %d", project, reads[newRelease], reads[previousRelease])
	})

	t.Run("two builds no release stamped", func(t *testing.T) {
		seeded := seed(t, 3)
		project := seeded.fixture.Project
		first := startConsumer(seeded, "devel first-process", seeded.now)
		register(t, first, "")
		reads := map[string]int{}
		passOf := func(run *consumer, what string) {
			t.Helper()
			reading := makePass(t, run)
			if reading.whole {
				reads[run.deps.Build]++
			}
			t.Logf("%s %s", what, reading)
		}
		passOf(first, "the first process's first pass")
		second := startConsumer(seeded, "devel second-process", first.at.Add(time.Minute))
		register(t, second, "")
		passOf(second, "the second process's first pass")
		for round := range 3 {
			first.at = second.at.Add(time.Minute)
			passOf(first, "the first process's pass")
			second.at = first.at.Add(time.Minute)
			passOf(second, "the second process's pass")
			if build, _ := markedBuild(t, first); build != second.deps.Build {
				t.Errorf("after round %d the record names %q, want the second process's %q", round+1, build, second.deps.Build)
			}
		}
		if reads[first.deps.Build] != 1 || reads[second.deps.Build] != 1 {
			t.Errorf("the two processes read %s whole %d and %d times, want once each: they traded whole reads",
				project, reads[first.deps.Build], reads[second.deps.Build])
		}
		t.Logf("whole reads of %s: the first process %d, the second process %d", project, reads[first.deps.Build], reads[second.deps.Build])
	})
}

// TestTheRunningBuildReadsEachProjectWholeOnceAfterTheLastProcessOfAnotherBuildStops is k35 (R2,
// R3). This build's inserts into legion_observations take the observations' lock (6901) before
// they draw their ids, and the previous release's intake takes none. So while a process of that
// release serves, one of its inserts can commit an id below one the consumer's cursor has passed,
// and that issue's row misses the observation. Once no live process serves another stamped build
// (processes.Live), the running build reads each project whole once and takes the record, and a
// process of the running build that finds the record written after the other build left reads
// nothing whole. Which process of the build makes the read does not matter: the moment of the
// last pass that found the other build serving is a row of issue_facts_cursor (rollCursor), so a
// process that starts after the other build's last process stopped owes the read as much as one
// that served beside it.
//
// Each case starts from the same roll (startMissedRoll), whose row of one issue misses an
// observation the previous release's intake committed below the cursor.
//
//   - A process that served beside it: the previous release's process stops, the first process's
//     next pass reads the project whole and the row holds the observation, and the second
//     process's next pass and the first process's pass after that read nothing whole.
//   - A process that starts after it stopped: the running build's two processes stop, and then
//     the previous release's, so no process that found it serving still runs. A third process of
//     the running build starts, its first pass reads the project whole and the row holds the
//     observation, and its next pass reads nothing whole.
//
// The run records the whole reads of each process.
func TestTheRunningBuildReadsEachProjectWholeOnceAfterTheLastProcessOfAnotherBuildStops(t *testing.T) {
	t.Parallel()
	t.Run("a process that served beside it", func(t *testing.T) {
		roll := startMissedRoll(t)
		project := roll.seeded.fixture.Project
		roll.stopPrevious()
		if reading := roll.pass(t, roll.first, "the running build's first process, after the previous release's process stopped,"); !reading.whole {
			t.Errorf("the running build read nothing of %s whole once no live process served the previous release %q", project, previousRelease)
		}
		roll.requireObservation(t, roll.first, "after the previous release's process stopped")
		if build, _ := markedBuild(t, roll.first); build != newRelease {
			t.Errorf("after the running build's whole read the record of %s names %q, want %q", project, build, newRelease)
		}
		if reading := roll.pass(t, roll.second, "the running build's second process, after the first read the project whole,"); reading.whole || reading.wrote != 0 {
			t.Errorf("the running build's second process read %s whole (%t) and wrote %d rows after the first took the record, want neither", project, reading.whole, reading.wrote)
		}
		if reading := roll.pass(t, roll.first, "the running build's first process, the pass after its whole read,"); reading.whole || reading.wrote != 0 {
			t.Errorf("the running build's first process read %s whole (%t) and wrote %d rows on the pass after its whole read, want neither", project, reading.whole, reading.wrote)
		}
		if roll.reads[roll.first] != 2 || roll.reads[roll.second] != 0 {
			t.Errorf("the running build's first process read %s whole %d times and its second process %d times, want 2 and 0", project, roll.reads[roll.first], roll.reads[roll.second])
		}
		t.Logf("whole reads of %s: the running build's first process %d, its second process %d, the previous release's process none, as it made no pass",
			project, roll.reads[roll.first], roll.reads[roll.second])
	})

	t.Run("a process that starts after it stopped", func(t *testing.T) {
		roll := startMissedRoll(t)
		project := roll.seeded.fixture.Project
		roll.stopFirst()
		roll.stopSecond()
		roll.stopPrevious()
		third := startConsumer(roll.seeded, newRelease, roll.at)
		register(t, third, newRelease)
		if reading := roll.pass(t, third, "the running build's third process, started after the previous release's process stopped,"); !reading.whole {
			t.Errorf("the running build's third process read nothing of %s whole, and the previous release %q served beside its build after the record was written", project, previousRelease)
		}
		roll.requireObservation(t, third, "after the third process's first pass")
		if build, _ := markedBuild(t, third); build != newRelease {
			t.Errorf("after the third process's whole read the record of %s names %q, want %q", project, build, newRelease)
		}
		if reading := roll.pass(t, third, "the running build's third process, the pass after its whole read,"); reading.whole || reading.wrote != 0 {
			t.Errorf("the running build's third process read %s whole (%t) and wrote %d rows on the pass after its whole read, want neither", project, reading.whole, reading.wrote)
		}
		if roll.reads[roll.first] != 1 || roll.reads[roll.second] != 0 || roll.reads[third] != 1 {
			t.Errorf("the running build's processes read %s whole %d, %d and %d times, want 1, 0 and 1", project, roll.reads[roll.first], roll.reads[roll.second], roll.reads[third])
		}
		t.Logf("whole reads of %s: the running build's first process %d, its second process %d, its third process %d, the previous release's process none, as it made no pass",
			project, roll.reads[roll.first], roll.reads[roll.second], roll.reads[third])
	})
}

// missedRoll is the roll k35 starts each case from (startMissedRoll): the running build's two
// processes beside a process of the previous release on one database, the stop of each process,
// the issue whose row misses an observation the previous release's intake committed below the
// observations cursor, the clock of the last pass and the whole reads of each process.
type missedRoll struct {
	seeded                              history
	first, second                       *consumer
	stopFirst, stopSecond, stopPrevious func()
	missed                              string
	missedAt                            int64
	at                                  time.Time
	reads                               map[*consumer]int
}

// startMissedRoll starts the roll k35 reads, each process registered as the server registers
// itself. The previous release's process makes no pass, and stands for its intake alone. The
// running build's first process reads the project whole beside it, and its second process starts
// and reads nothing whole. The previous release's intake inserts an observation of one open issue
// with no lock and holds its id open while an observation of another open issue commits above it.
// A pass reads the later one and moves the cursor past both ids, the first then commits below the
// cursor, and the first process's pass after it and a pass of the second process leave that
// issue's row missing the observation, with every process still serving.
func startMissedRoll(t *testing.T) *missedRoll {
	t.Helper()
	seeded := seed(t, 3)
	project := seeded.fixture.Project
	roll := &missedRoll{seeded: seeded, at: seeded.now, reads: map[*consumer]int{}}
	roll.first = startConsumer(seeded, newRelease, roll.at)
	roll.stopFirst = register(t, roll.first, newRelease)
	roll.stopPrevious = register(t, startConsumer(seeded, previousRelease, roll.at), previousRelease)
	if reading := roll.pass(t, roll.first, "the running build's first process, beside the previous release,"); !reading.whole {
		t.Fatalf("the running build's first pass read nothing of %s whole, which no build had read whole", project)
	}
	roll.second = startConsumer(seeded, newRelease, roll.at)
	roll.stopSecond = register(t, roll.second, newRelease)
	if reading := roll.pass(t, roll.second, "the running build's second process, beside the previous release,"); reading.whole {
		t.Errorf("the running build's second process read %s whole, whose record its build already holds", project)
	}

	missed, above := seeded.fixture.Open[0], seeded.fixture.Open[1]
	roll.missed = missed
	before := roll.first.rows(t)[missed]
	// The previous release's intake writes with no observations' lock: its insert draws its id,
	// and the transaction holds that id open while a later insert commits a higher one.
	write, err := seeded.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("open the previous release's write: %v", err)
	}
	t.Cleanup(func() { _ = write.Rollback(context.Background()) })
	if _, err := write.Exec(t.Context(), `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, project, missed, legion.FieldPhase, fmt.Sprintf("%q", legion.PhaseTesting), roll.at); err != nil {
		t.Fatalf("write the previous release's observation of %s: %v", missed, err)
	}
	roll.first.observe(t, above, legion.FieldPhase, legion.PhaseTesting, roll.at)
	roll.pass(t, roll.first, "the running build's first process, over the observation of "+above+",")
	if err := write.Commit(t.Context()); err != nil {
		t.Fatalf("commit the previous release's observation of %s: %v", missed, err)
	}
	cursor := roll.first.cursors(t)[observationsCursor].position
	roll.missedAt = observationID(t, roll.first, missed)
	if roll.missedAt >= cursor {
		t.Fatalf("the previous release's observation of %s took the id %d, not below the observations cursor at %d: this test examined nothing", missed, roll.missedAt, cursor)
	}
	roll.pass(t, roll.first, "the running build's first process, after the previous release's write committed below its cursor,")
	if reading := roll.pass(t, roll.second, "the running build's second process, beside the previous release,"); reading.whole {
		t.Errorf("the running build's second process read %s whole while the previous release still serves", project)
	}
	stale := roll.first.rows(t)[missed]
	want := computedWhole(t, roll.first)[missed]
	want.UpdatedAt = stale.UpdatedAt
	if !sameRow(stale, before) || rowDifference(stale, want) == "" {
		t.Fatalf("after the observation of %s committed at the id %d below the cursor at %d, its row moved or already holds it: this test examined nothing", missed, roll.missedAt, cursor)
	}
	return roll
}

// pass makes one pass of run a minute after the roll's last, counts it where it read the project
// whole, and logs it as what.
func (r *missedRoll) pass(t *testing.T, run *consumer, what string) passReading {
	t.Helper()
	r.at = r.at.Add(time.Minute)
	run.at = r.at
	reading := makePass(t, run)
	if reading.whole {
		r.reads[run]++
	}
	t.Logf("%s %s", what, reading)
	return reading
}

// requireObservation fails the test where the stored row of the missed issue, read through run,
// is not the row a whole computation of the project answers, which holds the missed observation.
func (r *missedRoll) requireObservation(t *testing.T, run *consumer, when string) {
	t.Helper()
	stored := run.rows(t)[r.missed]
	want := computedWhole(t, run)[r.missed]
	want.UpdatedAt = stored.UpdatedAt
	if difference := rowDifference(stored, want); difference != "" {
		t.Errorf("%s, the row of %s still misses its observation at the id %d: %s", when, r.missed, r.missedAt, difference)
	}
}
