package legionfacts

import (
	"testing"
	"time"
)

// The two builds this check runs the loop under: two full commit shas, as the image build
// stamps them. packages/envoy/docker/Dockerfile builds cmd/dispatch with
// -ldflags "-X main.buildCommit=${LEGION_COMMIT}" and refuses anything but a full sha, and
// cmd/dispatch hands that commit to the loop (processBuild, Deps.Build). Two releases are built
// from two commits, so these two strings stand for two releases of one deployment.
const (
	releaseOneCommit = "6f3f0f0b1c2d4e5a6b7c8d9e0a1b2c3d4e5f6071"
	releaseTwoCommit = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"
)

// TestTwoReleasesCompareDifferentAndTheSecondReadsTheProjectWhole is R9's rule read through the
// value the released binary really carries: the loop's build is the one the process was given
// (Deps.Build), so two releases of the image give it two different strings and the pass under
// the second reads every project whole again.
//
// Two passes run over one seeded project. The first runs under the first release's commit: it
// finds no record, reads the project whole, and writes the record that names its commit.
// Between the two passes the test writes the input R9 exists for, an issue a release gives
// Legion phase work that no feed names (gainedPhaseWork), and moves the loop's build to the
// second release's commit. The second pass finds a commit that is not its own and reads the
// project whole, which is what writes that issue's row.
//
// k11 (TestEachBuildReadsTheProjectWholeOnceAndSweepsItAfterThat) writes the other build into
// the record with an update, which examines the comparison. This check moves no record: it
// moves only what the process is, which is what the released image has to get right.
func TestTwoReleasesCompareDifferentAndTheSecondReadsTheProjectWhole(t *testing.T) {
	run := newConsumer(t, 6)
	project := run.fixture.Project
	if len(run.fixture.PhaseWork) == 0 {
		t.Fatal("the rig wrote no issue with Legion phase work, so no pass of it can write a row: this test examined nothing")
	}

	run.deps.Build = releaseOneCommit
	first := makePass(t, run)
	t.Logf("the first release's first pass %s", first)
	if !first.whole {
		t.Errorf("the first pass under %q swept %s, which no build had read whole", releaseOneCommit, project)
	}
	if build, recorded := markedBuild(t, run); !recorded || build != releaseOneCommit {
		t.Errorf("the record of %s names the build %q (recorded %t), want the commit the pass ran under, %q",
			project, build, recorded, releaseOneCommit)
	}

	// The release: a new image, built from the next commit, takes the lease from the old one.
	// The issue it gives Legion phase work is written first, as a release writes it: by the
	// rule the new build holds and with no event, observation or delivery change of its own.
	gained := gainedPhaseWork(t, run)
	if _, has := run.rows(t)[gained]; has {
		t.Fatalf("%s holds a stored row before the pass under the second release ran, so no pass has to find it: this test examined nothing",
			gained)
	}

	run.at = run.at.Add(time.Minute)
	run.deps.Build = releaseTwoCommit
	second := makePass(t, run)
	t.Logf("the second release's first pass %s", second)
	if !second.whole {
		t.Errorf("the pass under %q swept %s, whose record names the commit of the release before it, %q",
			releaseTwoCommit, project, releaseOneCommit)
	}
	if _, has := run.rows(t)[gained]; !has {
		t.Errorf("%s carries Legion phase work no feed named and holds no stored row after the pass under the second release's commit %q",
			gained, releaseTwoCommit)
	}
	if build, recorded := markedBuild(t, run); !recorded || build != releaseTwoCommit {
		t.Errorf("the record of %s names the build %q (recorded %t) after the pass under the second release, want %q",
			project, build, recorded, releaseTwoCommit)
	}
}
