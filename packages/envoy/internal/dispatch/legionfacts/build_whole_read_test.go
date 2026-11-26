package legionfacts

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legionfacts/independent"
)

// otherBuild is the build k11 writes into the project's record before the third pass. No build
// of this repository is called it, so the pass that reads the record finds a build that is not
// its own, which is what a release leaves behind.
const otherBuild = "legion-envoy-v0.0.0-another-build"

// readingHourTolerance is how far apart the stored hours and the independent reading's hours may
// stand: a tenth of an hour, which is what R1's own check holds its two sides to
// (independent/independent_test.go).
const readingHourTolerance = 0.1

// passReading is what one pass of the loop did, as k30 records it: the build the pass ran under,
// the build the record held for the project when the pass started, whether the pass read the
// project whole, how many issues a whole read read, how many rows the pass wrote, and how long it
// took.
type passReading struct {
	ranUnder string
	held     string
	whole    bool
	issues   int
	wrote    int
	took     time.Duration
}

// String is the one line k30 asks a reader to record for each pass.
func (p passReading) String() string {
	read := "read nothing whole"
	if p.whole {
		read = fmt.Sprintf("read it whole over %d issues", p.issues)
	}
	return fmt.Sprintf("ran under build %q, found %s recorded for the project, %s, wrote %d rows and took %s",
		p.ranUnder, p.held, read, p.wrote, p.took.Round(time.Millisecond))
}

// TestEachBuildReadsTheProjectWholeOnceAndNeverAgain is k30, the check of R27: a process whose
// build is not the one the project's record names, where that record was written before the
// process started, reads that project whole, and a process of the recorded build reads nothing
// whole and, with no input moving, writes no row.
//
// Four passes run over one seeded project. The first finds no record and reads the project
// whole, which writes the record and the build that made it. The second is the first pass of a
// second process of that same build, which reads nothing whole and writes no row. The third
// runs after another build string is written into the record, which keeps its old time, older
// than the second process's start, and reads the project whole again. The fourth runs after the
// record is deleted and reads the project whole once more.
//
// Between the second pass and the third the test writes the input R27 exists for: an issue a
// release gives Legion phase work, which no feed names (gainedPhaseWork). The third pass's rows
// are then read against the independent reading of the same moment, which has that issue among
// its own, so a pass that read nothing whole is caught by the row it never wrote rather than by
// the record alone.
func TestEachBuildReadsTheProjectWholeOnceAndNeverAgain(t *testing.T) {
	run := newConsumer(t, 6)
	project := run.fixture.Project
	if len(run.fixture.PhaseWork) == 0 {
		t.Fatal("the rig wrote no issue with Legion phase work, so no pass of it can write a row: this test examined nothing")
	}
	if otherBuild == run.deps.Build {
		t.Fatalf("this build is called %q, which is the build the third pass writes into the record, so that pass would find its own build: this test examined nothing",
			run.deps.Build)
	}

	first := makePass(t, run)
	t.Logf("the first pass %s", first)
	if !first.whole {
		t.Errorf("the first pass read nothing of %s whole, which no build had read whole", project)
	}
	if build, recorded := markedBuild(t, run); !recorded || build != run.deps.Build {
		t.Errorf("the record of %s names the build %q (recorded %t), want the build the pass ran under, %q",
			project, build, recorded, run.deps.Build)
	}

	run.at = run.at.Add(time.Minute)
	run.restart()
	second := makePass(t, run)
	t.Logf("the second pass, the first of a second process of the same build, %s", second)
	if second.whole {
		t.Errorf("the second process read %s whole under %q, the build its record already named", project, run.deps.Build)
	}
	if second.wrote != 0 {
		t.Errorf("the second process wrote %d rows with no input moving, want none", second.wrote)
	}

	// The release. It falls between the second process's pass and the pass under the new build,
	// as a release falls between two passes of the loop.
	gained := gainedPhaseWork(t, run)
	if _, has := run.rows(t)[gained]; has {
		t.Fatalf("%s holds a stored row before any pass under the new build ran, so no pass has to find it: this test examined nothing",
			gained)
	}

	run.at = run.at.Add(time.Minute)
	run.exec(t, "record another build against the project", `
		update issue_facts_computed set computed_build = $1 where project_key = $2
	`, otherBuild, project)
	third := makePass(t, run)
	t.Logf("the third pass %s", third)
	if !third.whole {
		t.Errorf("the third pass read nothing of %s whole, whose record named the build %q and not the build it ran under, %q",
			project, otherBuild, run.deps.Build)
	}
	compared := storedRowsAgainstTheReading(t, run)
	if _, has := run.rows(t)[gained]; !has {
		t.Errorf("%s carries Legion phase work no feed named and holds no stored row after the pass under a build the record did not name", gained)
	}
	t.Logf("the third pass's %d stored rows were read against the independent reading of %s over the %d issues that pass read, and the release gave %s the phase work no feed names",
		compared, project, third.issues, gained)

	run.at = run.at.Add(time.Minute)
	run.exec(t, "delete the project's record", `delete from issue_facts_computed where project_key = $1`, project)
	fourth := makePass(t, run)
	t.Logf("the fourth pass %s", fourth)
	if !fourth.whole {
		t.Errorf("the fourth pass read nothing of %s whole, which holds no record of a whole read at all", project)
	}
	if build, recorded := markedBuild(t, run); !recorded || build != run.deps.Build {
		t.Errorf("the record of %s names the build %q (recorded %t) after the fourth pass, want %q",
			project, build, recorded, run.deps.Build)
	}
}

// makePass runs one pass over the seeded project and answers what that pass did.
//
// Whether the pass read the project whole is read from the record the pass writes in the
// transaction that wrote its rows (markComputedWhole): a whole read moves that record to the
// moment the pass ran at, and a pass that read no project whole writes no record. The moment is
// the test's own clock, which each caller moves before its pass, so a record left by an earlier
// pass cannot be read as this pass's own. How many issues a whole read read is the project's
// issues, which is what ComputeProject reads (metrics.ReadProject reads every issue of the
// project), counted before the pass runs.
func makePass(t *testing.T, run *consumer) passReading {
	t.Helper()
	build, recorded := markedBuild(t, run)
	before, marked := computedWholeAt(t, run)[run.fixture.Project]
	if marked && before.Equal(run.at) {
		t.Fatalf("the record of %s already names %s, the moment this pass runs at, so what this pass does cannot be read from it: this test examined nothing",
			run.fixture.Project, run.at.UTC())
	}
	issues := projectIssues(t, run)

	started := time.Now()
	changed := run.pass(t)
	took := time.Since(started)

	after, marked := computedWholeAt(t, run)[run.fixture.Project]
	reading := passReading{ranUnder: run.deps.Build, held: "no build", wrote: len(changed), took: took}
	if recorded {
		reading.held = fmt.Sprintf("the build %q", build)
	}
	if marked && after.Equal(run.at) {
		reading.whole, reading.issues = true, issues
	}
	return reading
}

// markedBuild is the build the project's record of its last whole read names, and false where
// the table holds no record of the project at all.
func markedBuild(t *testing.T, run *consumer) (string, bool) {
	t.Helper()
	var build string
	err := run.database.Pool.QueryRow(t.Context(),
		`select computed_build from issue_facts_computed where project_key = $1`, run.fixture.Project).Scan(&build)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the build recorded for %s: %v", run.fixture.Project, err)
	}
	return build, true
}

// projectIssues is how many issues the project holds, which is what a whole read of it reads.
func projectIssues(t *testing.T, run *consumer) int {
	t.Helper()
	var issues int
	if err := run.database.Pool.QueryRow(t.Context(),
		`select count(*) from issues where project_key = $1`, run.fixture.Project).Scan(&issues); err != nil {
		t.Fatalf("count the issues of %s: %v", run.fixture.Project, err)
	}
	return issues
}

// gainedPhaseWork writes the issue a release gives Legion phase work to and answers its key.
//
// R27's seventh input is a release that makes an issue Legion's work, and no feed carries it: the
// rule is the build's own (legion.PhaseWorkKeys over the role names and title shape of
// legion/session_title.go and over the handoffs), so such a release writes no event, no
// observation and no delivery change. A handoff row written with no event of its own is that
// input as a test can write it, because legion.PhaseWorkKeys takes every issue a handoff names,
// and a write to handoffs outside Dispatch reaches the rule the same silent way (legionProjects).
//
// The issue is closed and holds no stored row, and no feed names it, so only a whole read
// reaches it. It stands at the top of a tree of its own, so the descent from a key a feed names
// does not reach it either (rowsTouchedBy).
func gainedPhaseWork(t *testing.T, run *consumer) string {
	t.Helper()
	key := fmt.Sprintf("%s-9001", run.fixture.Project)
	run.exec(t, "write the issue a release gave Legion phase work", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at)
		values ($1, $2, 9001, 'An issue a release gave Legion phase work', 'done', '00009001',
		        '{"kind":"user","id":"alice"}'::jsonb, $3, $4)
	`, key, run.fixture.Project, run.fixture.To.Add(-3*time.Hour), run.fixture.To.Add(-2*time.Hour))
	run.exec(t, "record the handoff that gives that issue its phase work", `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, 'implement', 1, '{}', '{"kind":"user","id":"alice"}'::jsonb, $2)
	`, key, run.fixture.To.Add(-2*time.Hour))
	return key
}

// storedRowsAgainstTheReading reads every stored row of the project, closed at the consumer's
// clock (Row.At), against the independent reading of the same project at the same moment
// (package independent, the second side of R1's own check): every issue that reading holds
// Legion phase work for has a stored row, no stored row stands for an issue it does not hold, and
// the facts of each agree. It answers how many issues it compared, and a run that compared none
// fails.
func storedRowsAgainstTheReading(t *testing.T, run *consumer) int {
	t.Helper()
	reading, err := independent.Read(t.Context(), run.database.Pool,
		independent.Scope{Project: run.fixture.Project}, independent.Window{To: run.at})
	if err != nil {
		t.Fatalf("read %s independently: %v", run.fixture.Project, err)
	}
	stored := run.rows(t)
	compared, differed := 0, 0
	for _, key := range slices.Sorted(maps.Keys(eitherSide(reading.Issues, stored))) {
		mine, read := reading.Issues[key]
		row, has := stored[key]
		switch {
		case !read:
			t.Errorf("%s holds a stored row and the independent reading gives it no Legion phase work", key)
			differed++
			continue
		case !has:
			t.Errorf("%s carries Legion phase work by the independent reading and holds no stored row", key)
			differed++
			continue
		}
		compared++
		differed += factsAgree(t, key, mine, row.At(run.at))
	}
	// A run that compared nothing fails here rather than ending the test, so the passes after
	// this comparison are run and recorded too: what a pass did is the reading this check is
	// made of.
	if compared == 0 {
		t.Errorf("the independent reading holds %d issues and the table %d stored rows, and the two sides compared none of them: this test examined nothing",
			len(reading.Issues), len(stored))
	}
	if differed > 0 {
		t.Errorf("%d facts differ between the stored rows and the independent reading, over the %d issues the two sides compared",
			differed, compared)
	}
	return compared
}

// factsAgree reads one issue's stored row against the independent reading of it and answers how
// many of its facts differ: the hours to a tenth of an hour, as R1's own check compares them,
// and the review rounds and the tree root exactly.
func factsAgree(t *testing.T, key string, mine independent.Facts, row Row) int {
	t.Helper()
	differences := 0
	if math.Abs(mine.WorkHours-row.WorkHours) > readingHourTolerance {
		t.Errorf("%s worked %g h by the independent reading and %g h by its stored row",
			key, mine.WorkHours, row.WorkHours)
		differences++
	}
	if mine.ReviewRounds != row.ReviewRounds {
		t.Errorf("%s has %d review rounds by the independent reading and %d by its stored row",
			key, mine.ReviewRounds, row.ReviewRounds)
		differences++
	}
	if mine.Tree != row.Tree {
		t.Errorf("%s is in tree %s by the independent reading and under root %s by its stored row",
			key, mine.Tree, row.Tree)
		differences++
	}
	// A phase one side left out holds no hours of that phase, so the two are compared over every
	// phase either side names.
	for _, phase := range slices.Sorted(maps.Keys(eitherSide(mine.PhaseHours, row.PhaseHours))) {
		if math.Abs(mine.PhaseHours[phase]-row.PhaseHours[phase]) > readingHourTolerance {
			t.Errorf("%s spent %g h in %s by the independent reading and %g h by its stored row",
				key, mine.PhaseHours[phase], phase, row.PhaseHours[phase])
			differences++
		}
	}
	return differences
}

// eitherSide is every key either side holds.
func eitherSide[Left, Right any](left map[string]Left, right map[string]Right) map[string]struct{} {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return keys
}
