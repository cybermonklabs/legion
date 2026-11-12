//go:build multiprocess

// The build the Legion facts consumer records against a project it read whole, read through two
// real binaries stamped as the image build stamps a release (R9 of dispatch://INFRA-2915).
//
// Nothing here assigns legionfacts.Deps.Build. The commit is stamped into the binary
// (-ldflags -X main.buildCommit), processBuild answers it, and main hands it to the loop, which
// is the one path an image takes and the one an in-process test cannot run: a break anywhere
// along it leaves every image answering `devel`, since the image holds no .git for
// internal/buildversion to read, and R9 would then read no project whole after the first
// release.
//
// It carries the `multiprocess` build tag, which is the tag this package gives a row that starts
// real binaries of given builds (multiprocess_test.go, cmdtest.BuildStamped). The row builds the
// server twice, once per stamped commit, because go build relinks the whole binary into each -o
// path (cmdtest.BuildCommand), and it waits out two process boots and two consumer passes. The
// untagged tests of this package share go test's ten-minute package deadline, and the
// dispatch-multiprocess job exists so a row of this shape need not.

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two commits this row's binaries are stamped with, which stand for two releases of one
// deployment: 40 hex characters each, the shape packages/envoy/docker/Dockerfile refuses to
// leave short.
const (
	factsReleaseC1 = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	factsReleaseC2 = "f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2"
)

// factsSeedTrees is how many trees the seeded project holds. It is small because this row reads
// which build the consumer recorded rather than what a whole read costs, and a whole read of
// three trees takes milliseconds: the cost is measured over populations of its own in
// legionfacts (SweepEvery).
const factsSeedTrees = 3

// factsRecordWait is how long each process is given to record its own build against the project.
// Its first pass runs at start, so the time is the boot and one pass; a minute is many times
// both and still fails this row rather than hanging it.
const factsRecordWait = time.Minute

// TestASecondReleaseReadsTheProjectWholeAndRecordsItsOwnCommit is R9 through the value a released
// binary carries. Two binaries are built and stamped with a commit each, and run one after the
// other against one seeded database, as a deploy replaces a release with the next.
//
// The first records its own commit against the project. Only a whole read writes that record
// (legionfacts.markComputedWhole), so the record naming C1's commit is the pass that read the
// project whole under C1. The second finds a record that names a commit that is not its own,
// reads the project whole again, and moves the record to its own commit at a later moment. A
// second process that swept instead would leave the record where the first left it, and this row
// would fail on the build it still named.
func TestASecondReleaseReadsTheProjectWholeAndRecordsItsOwnCommit(t *testing.T) {
	database := storetest.Open(t)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Trees: factsSeedTrees})
	if len(fixture.PhaseWork) == 0 {
		t.Fatal("the seeded project holds no issue with Legion phase work, so a whole read of it writes no row: this test examined nothing")
	}
	databaseURL := databaseURLOf(t, database)
	c1 := cmdtest.BuildStamped(t, "envoy-dispatch-facts-c1", factsReleaseC1)
	c2 := cmdtest.BuildStamped(t, "envoy-dispatch-facts-c2", factsReleaseC2)

	first := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: c1})
	first.waitHealthy(t)
	readWhole := waitForComputedBuild(t, database, first, fixture.Project, factsReleaseC1)
	written := storedFactRows(t, database, fixture.Project)
	if written == 0 {
		t.Fatalf("the process stamped %s recorded a whole read of %s and the table holds no row of that project, and %d of its issues carry Legion phase work",
			factsReleaseC1, fixture.Project, len(fixture.PhaseWork))
	}
	t.Logf("the binary stamped %s read %s whole as of %s and wrote %d rows", factsReleaseC1, fixture.Project, readWhole.UTC(), written)

	// The release: the first process stops and a binary built from the next commit takes its
	// place, which is the one thing that moved.
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM to the process stamped "+factsReleaseC1)

	second := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: c2})
	second.waitHealthy(t)
	readAgain := waitForComputedBuild(t, database, second, fixture.Project, factsReleaseC2)
	if !readAgain.After(readWhole) {
		t.Errorf("the record of %s moved to %s as of %s, which is not after the %s the release before it recorded: a whole read computes as of the moment its pass started",
			fixture.Project, factsReleaseC2, readAgain.UTC(), readWhole.UTC())
	}
	t.Logf("the binary stamped %s read %s whole again as of %s, %s after the release before it",
		factsReleaseC2, fixture.Project, readAgain.UTC(), readAgain.Sub(readWhole).Round(time.Millisecond))
}

// waitForComputedBuild waits up to factsRecordWait for the project's record of its last whole
// read to name want, and answers the moment that read computed as of. It fails the test with the
// process's own output when the process exits first or when the record still names another
// build, because the build a process records is the one it carries and nothing it reads can
// delay that past its first pass.
//
// The column is read through coalesce because it is null on a row the migration adding it found
// and on no row a pass writes (20261112193821_issue_facts_computed_build.up.sql): a null here is
// a failure this row reports rather than a scan error it stops on.
func waitForComputedBuild(t *testing.T, database *store.Store, process *dispatchProcess, project, want string) time.Time {
	t.Helper()
	deadline := time.Now().Add(factsRecordWait)
	for {
		build := "no record at all"
		var at time.Time
		err := database.Pool.QueryRow(t.Context(), `
			select coalesce(computed_build, 'null'), computed_at
			from issue_facts_computed where project_key = $1
		`, project).Scan(&build, &at)
		switch {
		case err == nil && build == want:
			return at
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			t.Fatalf("read the record of %s's last whole read: %v", project, err)
		}
		select {
		case <-process.Exited:
			t.Fatalf("the process stamped %s exited before it recorded that build against %s, whose record names %q:\n%s",
				want, project, build, process.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s after the process stamped %s started serving, the record of %s names %q, want %q:\n%s",
				factsRecordWait, want, project, build, want, process.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// storedFactRows is how many stored Legion facts rows the project holds, which says that the
// whole read the record names read issues rather than nothing at all.
func storedFactRows(t *testing.T, database *store.Store, project string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(t.Context(),
		`select count(*) from issue_facts where project_key = $1`, project).Scan(&rows); err != nil {
		t.Fatalf("count the stored facts of %s: %v", project, err)
	}
	return rows
}
