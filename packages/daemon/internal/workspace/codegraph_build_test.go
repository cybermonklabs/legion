package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fakeCodegraphBundle puts a stand-in `codegraph` first on PATH, laid out as the worker image
// installs CodeGraph: bin/codegraph in a bundle directory whose package.json names version
// (/opt/codegraph/package.json), or holds no package.json where version is "". The stand-in logs
// its first argument to callLog, one call per line, and runs cases, the arms of a `case "$1"`.
// It runs in the workspace, as every codegraph the warm-up starts does.
func fakeCodegraphBundle(t *testing.T, callLog, version, cases string) {
	t.Helper()
	bundle := t.TempDir()
	if err := os.Mkdir(filepath.Join(bundle, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		manifest := `{"name":"@colbymchenry/codegraph-linux-x64","version":"` + version + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(bundle, "package.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + callLog + "'\ncase \"$1\" in\n" + cases + "esac\n"
	if err := os.WriteFile(filepath.Join(bundle, "bin", "codegraph"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(bundle, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordPath is where the warm-up keeps the record of its last build in the workspace at dir.
func recordPath(dir string) string { return filepath.Join(dir, ".codegraph", warmBuildName) }

// writeWarmBuildRecord leaves rec as the record of an earlier warm-up's build in the workspace at
// dir, as that warm-up wrote it: one JSON line.
func writeWarmBuildRecord(t *testing.T, dir string, rec warmBuild) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath(dir), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readWarmBuildRecord is the record the warm-up left in the workspace at dir, and false where it
// left none. A record that is not one JSON line of a build fails the test.
func readWarmBuildRecord(t *testing.T, dir string) (warmBuild, bool) {
	t.Helper()
	body, err := os.ReadFile(recordPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return warmBuild{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "\n") != 1 || !strings.HasSuffix(string(body), "\n") {
		t.Fatalf("the build record is not one line: %q", body)
	}
	var rec warmBuild
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("the build record %q does not parse: %v", body, err)
	}
	return rec, true
}

// processEnded is whether pid has ended: no process has it, or the one that has it has exited
// and waits only for its parent's reap.
func processEnded(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}

// TestWarmCodegraphIndexEndsABuildAtItsMemoryCap: a warm-up build that grows past the cap is ended
// by the warm-up, with every process of its group, at the first read of the watch after it holds
// the memory, rather than left to grow to the launcher's guard or the pod's limit. The stand-in
// `init` holds 64 MiB of anonymous memory in its shell, under a cap of 16 MiB, with a `sleep` of
// its own beside it in its group, and would otherwise wait for ever. The watch runs on a fake
// clock, so the test moves it one poll on once the memory is held, and nothing but that one read
// can end the build. The build is the leader of a process group of its own, so the group the
// warm-up ends holds neither this test's process nor, in a pod, the shim and its agent. The
// warm-up says so in one line naming the cap and the peak it read, releases its lease, and leaves
// the record `memory cap`.
func TestWarmCodegraphIndexEndsABuildAtItsMemoryCap(t *testing.T) {
	saved := codegraphMemoryCap
	codegraphMemoryCap = 16 << 20
	t.Cleanup(func() { codegraphMemoryCap = saved })
	scratch := t.TempDir()
	callLog, pids, holding := filepath.Join(scratch, "calls.log"), filepath.Join(scratch, "pids"), filepath.Join(scratch, "holding")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":false}' ;;
init)
  held=$(head -c 67108864 /dev/zero | tr '\0' x)
  sleep 86400 &
  set -- $(sed 's/.*) //' /proc/$$/stat)
  echo "$$ $! $3" > '`+pids+`'
  : > '`+holding+`'
  wait
  ;;
`)
	dir := t.TempDir()
	captured := captureStderr(t)
	c := clocktest.New(time.Date(2026, 11, 24, 15, 59, 0, 0, time.UTC))

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		warmCodegraphIndexOn(context.Background(), c, procgroup.NoReaper, dir, time.Now())
	}()
	var standIn, sleeper, group int
	t.Cleanup(func() {
		// A warm-up that never ended its build leaves it running here: end both processes, so
		// the warm-up returns before the next test swaps os.Stderr.
		for _, pid := range []int{standIn, sleeper} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		testwait.Receive(t, "the warm-up to return once its build was gone", returned)
	})
	testwait.Eventually(t, "the stand-in init to hold its 64 MiB", func() bool {
		_, err := os.Stat(holding)
		return err == nil
	})
	body, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(body)); len(fields) == 3 {
		standIn, _ = strconv.Atoi(fields[0])
		sleeper, _ = strconv.Atoi(fields[1])
		group, _ = strconv.Atoi(fields[2])
	}
	if standIn == 0 || sleeper == 0 || group == 0 {
		t.Fatalf("the stand-in recorded %q, want its pid, its sleep's pid and its process group", body)
	}
	if group != standIn {
		t.Errorf("the build ran in process group %d, want a group of its own led by the build (%d); this test's group is %d", group, standIn, syscall.Getpgrp())
	}

	// The build's bound and the watch's first read were armed before the build started, so both
	// are pending now that it holds its memory: a warm-up with no watch has the bound alone. One
	// poll on is one read.
	if armed := c.Armed(); len(armed) < 2 {
		t.Fatalf("the fake clock holds %d timers (%v) once the build holds its memory, want the build's bound and the memory watch's first read", len(armed), armed)
	}
	c.Advance(codegraphMemoryPoll)
	select {
	case <-returned:
	case <-time.After(testwait.Bounded(t, 20*time.Second, 5*time.Second)):
		t.Fatalf("the warm-up was still building after the first read past its build's 64 MiB under a 16 MiB cap; stderr so far:\n%s", captured())
	}
	testwait.Eventually(t, "the build and the sleep in its group to have ended", func() bool {
		return processEnded(standIn) && processEnded(sleeper)
	})
	stderr := captured()
	line := regexp.MustCompile(`(?m)^\[legion\] codegraph init for ` + regexp.QuoteMeta(dir) + ` ended at its memory cap: its process group read ([0-9]+) MiB, over the cap of 16 MiB\b.*$`)
	found := line.FindStringSubmatch(stderr)
	if found == nil {
		t.Fatalf("stderr lacks the line naming the cap and the peak:\n%s", stderr)
	}
	peak, _ := strconv.Atoi(found[1])
	if peak < 16 {
		t.Errorf("the line names a peak of %d MiB, want one over the 16 MiB cap: %s", peak, found[0])
	}
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", warmLeaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease is still held after the build was ended (stat: %v)", err)
	}
	rec, ok := readWarmBuildRecord(t, dir)
	if !ok {
		t.Fatal("the warm-up left no record of the build it ended at its cap")
	}
	if rec.Outcome != warmOutcomeMemoryCap || rec.Subcommand != "init" || rec.Cap != 16<<20 || rec.Peak>>20 != uint64(peak) {
		t.Errorf("the record is %+v, want init ended %q at the 16 MiB cap with the peak the line names (%d MiB)", rec, warmOutcomeMemoryCap, peak)
	}
}

// TestWarmCodegraphIndexBoundsCodegraphsParseWorkers: CodeGraph's parse pool is
// clamp(max(3, CPUs) − 1, 1, 8) workers unless CODEGRAPH_PARSE_WORKERS says otherwise, and the
// warm-up's build runs at the count its cap was measured at, whatever CPUs the pod shows.
func TestWarmCodegraphIndexBoundsCodegraphsParseWorkers(t *testing.T) {
	scratch := t.TempDir()
	callLog, seen := filepath.Join(scratch, "calls.log"), filepath.Join(scratch, "init.env")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":false}' ;;
init) env > '`+seen+`' ;;
`)
	dir := t.TempDir()

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	listing, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the stand-in init recorded no environment: %v; calls %v", err, stubCalls(t, callLog))
	}
	vars := envlisting.Vars(string(listing))
	// 1, the count the cap was measured at (codegraphMemoryCap): at 1 worker both repositories'
	// builds peaked within 7% of their peak at 2.
	if got, set := vars["CODEGRAPH_PARSE_WORKERS"]; !set || got != "1" {
		t.Fatalf("the build's environment has CODEGRAPH_PARSE_WORKERS=%q (set: %v), want 1: %v", got, set, vars)
	}
}

// recordedOutcomeStandIn is a stand-in CodeGraph whose `status` reports the workspace
// uninitialized until an `init` has begun, and an index left `indexing` from then on, as a build
// that died leaves it; initEnd is how its `init` ends once it has begun. Its `index` exits 0: a
// warm-up that ran it would have repaired the index.
func recordedOutcomeStandIn(initEnd string) string {
	return `status) if [ -e .codegraph/begun ]; then echo '{"initialized":true,"index":{"state":"indexing"}}'; else echo '{"initialized":false}'; fi ;;
init) mkdir -p .codegraph; : > .codegraph/begun; ` + initEnd + ` ;;
index) exit 0 ;;
`
}

// warmUpTwiceAfterADeath runs two warm-ups of one workspace whose `init` ends as initEnd, the first
// by first and the second as a new shim's (indexed.Delete), and checks that the first left the
// record want and the second ran `status` alone: a build that died is not built again on the same
// disk.
func warmUpTwiceAfterADeath(t *testing.T, initEnd, want string, first func(dir string)) {
	t.Helper()
	callLog := filepath.Join(t.TempDir(), "calls.log")
	fakeCodegraphBundle(t, callLog, "1.5.0", recordedOutcomeStandIn(initEnd))
	dir := t.TempDir()

	first(dir)
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "init"}) {
		t.Fatalf("the first warm-up's calls = %v, want status then init", calls)
	}
	rec, ok := readWarmBuildRecord(t, dir)
	if !ok || rec.Outcome != want || rec.Subcommand != "init" || rec.CodeGraph != "1.5.0" || rec.Cap != codegraphMemoryCap {
		t.Fatalf("the first warm-up left the record %+v (present: %v), want init ended %q under CodeGraph 1.5.0 at the cap %d", rec, ok, want, codegraphMemoryCap)
	}
	indexed.Delete(dir)
	captured := captureStderr(t)

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	stderr := captured()
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "init", "status"}) {
		t.Fatalf("the calls across both warm-ups = %v, want the second to run status alone: the build died, and a repair would run it again on the same disk", calls)
	}
	if !strings.Contains(stderr, "codegraph init started "+rec.Started.Format(time.RFC3339)) || !strings.Contains(stderr, strconv.Quote(want)) {
		t.Errorf("the second warm-up's stderr lacks one line naming the build, its start and %q:\n%s", want, stderr)
	}
}

// warmUpOnce is the first warm-up of warmUpTwiceAfterADeath on the real clock, for a build that
// ends by itself.
func warmUpOnce(dir string) {
	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
}

// TestWarmCodegraphIndexDoesNotRepairABuildThatDiedUnseen: a build that died with its shim (a pod
// end, the kernel's out-of-memory kill, a shim killed at its stop grace) left its record
// `running`, and nothing else is building, since this warm-up holds the lease. Its index reads
// `indexing`, which before was a repair: now the warm-up runs `status` alone and says why.
func TestWarmCodegraphIndexDoesNotRepairABuildThatDiedUnseen(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
index) exit 0 ;;
`)
	dir := t.TempDir()
	started := time.Date(2026, 11, 24, 15, 59, 0, 0, time.UTC)
	died := warmBuild{Subcommand: "init", Started: started, Outcome: warmOutcomeRunning, CodeGraph: "1.5.0", Cap: codegraphMemoryCap}
	writeWarmBuildRecord(t, dir, died)
	captured := captureStderr(t)

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	stderr := captured()
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status"}) {
		t.Fatalf("codegraph calls = %v, want status alone on a workspace whose last build died unseen", calls)
	}
	want := regexp.MustCompile(`(?m)^\[legion\] codegraph warm-up for ` + regexp.QuoteMeta(dir) + ` skipped: its last build, codegraph init started 2026-11-24T15:59:00Z, ended "running", .*died unseen.*$`)
	if lines := want.FindAllString(stderr, -1); len(lines) != 1 {
		t.Errorf("stderr holds %d lines naming the build that died unseen, want one:\n%s", len(lines), stderr)
	}
	if rec, ok := readWarmBuildRecord(t, dir); !ok || rec != died {
		t.Errorf("the record is %+v (present: %v), want it left as it was, %+v", rec, ok, died)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codegraph", warmLeaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease is still held (stat: %v)", err)
	}
}

// TestWarmCodegraphIndexDoesNotRepairABuildASignalEnded: a build a signal ended that the warm-up
// did not send (the kernel's out-of-memory kill, the launcher's memory guard) is recorded
// `killed`, and the next warm-up, a new shim's, does not build again.
func TestWarmCodegraphIndexDoesNotRepairABuildASignalEnded(t *testing.T) {
	warmUpTwiceAfterADeath(t, "kill -KILL $$", warmOutcomeKilled, warmUpOnce)
}

// TestWarmCodegraphIndexDoesNotRepairABuildThatTimedOut: a build the warm-up's own bound ended
// would time out again, so it is recorded `timed out` and not built again. The bound runs on a
// fake clock: the stand-in's `init` waits for ever once it has begun, and the test moves the clock
// past the bound, so the bound alone ends it.
func TestWarmCodegraphIndexDoesNotRepairABuildThatTimedOut(t *testing.T) {
	warmUpTwiceAfterADeath(t, "exec sleep 86400", warmOutcomeTimedOut, func(dir string) {
		c := clocktest.New(time.Date(2026, 11, 24, 15, 59, 0, 0, time.UTC))
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			warmCodegraphIndexOn(context.Background(), c, procgroup.NoReaper, dir, time.Now())
		}()
		testwait.Eventually(t, "the stand-in init to have begun", func() bool {
			_, err := os.Stat(filepath.Join(dir, ".codegraph", "begun"))
			return err == nil
		})
		c.AwaitArmed(t, "the build's bound", 1)
		c.Advance(codegraphTimeout)
		testwait.Receive(t, "the warm-up to return once the build's bound ended it", returned)
	})
}

// TestWarmCodegraphIndexDoesNotRepairABuildThatExitedNonZero: a build that failed with a status
// of its own is recorded `exit N`, and not built again on the same disk.
func TestWarmCodegraphIndexDoesNotRepairABuildThatExitedNonZero(t *testing.T) {
	warmUpTwiceAfterADeath(t, "echo 'no database could be created' >&2; exit 1", "exit 1", warmUpOnce)
}

// TestWarmCodegraphIndexWritesItsRecordBeforeTheBuild: the record says `running` before the build
// starts, so a build that dies with its pod or its shim, which no line and no later write of
// this warm-up can report, is still on the disk for the next one. A build that exits 0 removes
// it, and so does a build the warm-up's own context ended: an orderly stop is built again at the
// next launch.
func TestWarmCodegraphIndexWritesItsRecordBeforeTheBuild(t *testing.T) {
	t.Run("a build that exits 0", func(t *testing.T) {
		scratch := t.TempDir()
		callLog, seen := filepath.Join(scratch, "calls.log"), filepath.Join(scratch, "seen")
		fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":false}' ;;
init) cp .codegraph/`+warmBuildName+` '`+seen+`' ;;
`)
		dir := t.TempDir()
		before := time.Now().Add(-time.Second)

		WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
		body, err := os.ReadFile(seen)
		if err != nil {
			t.Fatalf("the build found no record when it started: %v", err)
		}
		var rec warmBuild
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Fatalf("the record the build found, %q, does not parse: %v", body, err)
		}
		if rec.Subcommand != "init" || rec.Outcome != warmOutcomeRunning || rec.CodeGraph != "1.5.0" || rec.Cap != codegraphMemoryCap || rec.Started.Before(before) || rec.Started.After(time.Now()) {
			t.Errorf("the record the build found is %+v, want init %q under CodeGraph 1.5.0 at the cap %d, started during this warm-up", rec, warmOutcomeRunning, codegraphMemoryCap)
		}
		if rec, ok := readWarmBuildRecord(t, dir); ok {
			t.Errorf("a build that exited 0 left the record %+v", rec)
		}
	})
	t.Run("a build the warm-up's own stop ended", func(t *testing.T) {
		scratch := t.TempDir()
		callLog, begun := filepath.Join(scratch, "calls.log"), filepath.Join(scratch, "begun")
		fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":false}' ;;
init) : > '`+begun+`'; exec sleep 30 ;;
`)
		dir := t.TempDir()
		ctx, stop := context.WithCancel(context.Background())
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			WarmCodegraphIndex(ctx, procgroup.NoReaper, dir, time.Now())
		}()
		testwait.Eventually(t, "the stand-in init to have begun", func() bool {
			_, err := os.Stat(begun)
			return err == nil
		})
		if rec, ok := readWarmBuildRecord(t, dir); !ok || rec.Outcome != warmOutcomeRunning {
			t.Errorf("the record during the build is %+v (present: %v), want %q", rec, ok, warmOutcomeRunning)
		}
		stop()
		testwait.Receive(t, "the warm-up's return once its context ended", returned)
		if rec, ok := readWarmBuildRecord(t, dir); ok {
			t.Errorf("a build the warm-up's own stop ended left the record %+v; the next launch must build it again", rec)
		}
	})
}

// TestWarmCodegraphIndexClearsItsRecordOnACompleteIndex: an index that is complete needs no build,
// whoever completed it (an agent's own `codegraph index`, which the record never holds), and the
// record of an earlier death says nothing of it any longer.
func TestWarmCodegraphIndexClearsItsRecordOnACompleteIndex(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":true,"index":{"state":"complete"}}' ;;
`)
	dir := t.TempDir()
	writeWarmBuildRecord(t, dir, warmBuild{Subcommand: "init", Started: time.Now().Add(-time.Hour).UTC(), Outcome: warmOutcomeKilled, CodeGraph: "1.5.0", Cap: codegraphMemoryCap})

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status"}) {
		t.Fatalf("codegraph calls = %v, want status alone on a complete index", calls)
	}
	if rec, ok := readWarmBuildRecord(t, dir); ok {
		t.Errorf("a complete index left the record %+v", rec)
	}
}

// TestWarmCodegraphIndexRetriesOnceWhenTheCodegraphPinChanged: the record holds a build for the
// CodeGraph and the cap it died under, since the same build dies again; another CodeGraph, read
// from the package.json of the image's bundle, or another cap, allows one new attempt. That
// attempt's own death is recorded under the image's version, and holds the next warm-up.
func TestWarmCodegraphIndexRetriesOnceWhenTheCodegraphPinChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		cap     uint64
	}{
		{"another CodeGraph version", "1.4.0", codegraphMemoryCap},
		{"another cap", "1.5.0", codegraphMemoryCap + 1<<20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callLog := filepath.Join(t.TempDir(), "calls.log")
			fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
index) kill -KILL $$ ;;
`)
			dir := t.TempDir()
			writeWarmBuildRecord(t, dir, warmBuild{Subcommand: "init", Started: time.Now().Add(-time.Hour).UTC(), Outcome: warmOutcomeKilled, CodeGraph: tc.version, Cap: tc.cap})

			WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
			if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "index"}) {
				t.Fatalf("codegraph calls = %v, want status then index: %s allows one new attempt", calls, tc.name)
			}
			rec, ok := readWarmBuildRecord(t, dir)
			if !ok || rec.Subcommand != "index" || rec.Outcome != warmOutcomeKilled || rec.CodeGraph != "1.5.0" || rec.Cap != codegraphMemoryCap {
				t.Fatalf("the attempt left the record %+v (present: %v), want index %q under the image's CodeGraph 1.5.0 at the cap %d", rec, ok, warmOutcomeKilled, codegraphMemoryCap)
			}
			indexed.Delete(dir)

			WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
			if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "index", "status"}) {
				t.Fatalf("codegraph calls = %v, want the next warm-up to run status alone: the one new attempt died too", calls)
			}
		})
	}
}

// TestWarmCodegraphIndexBuildsAWorkspaceNewerThanItsRecord: a workspace provisioning created after
// the recorded build started (.jj/legion-created) is not the one that build died in, and is built.
func TestWarmCodegraphIndexBuildsAWorkspaceNewerThanItsRecord(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
index) exit 0 ;;
`)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".jj"), 0o700); err != nil {
		t.Fatal(err)
	}
	died := time.Now().Add(-time.Hour).UTC()
	if err := RecordCreated(dir, died.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	writeWarmBuildRecord(t, dir, warmBuild{Subcommand: "init", Started: died, Outcome: warmOutcomeMemoryCap, CodeGraph: "1.5.0", Cap: codegraphMemoryCap})

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "index"}) {
		t.Fatalf("codegraph calls = %v, want status then index on a workspace created after its record's build", calls)
	}
	if rec, ok := readWarmBuildRecord(t, dir); ok {
		t.Errorf("the build that exited 0 left the record %+v", rec)
	}
}

// TestWarmCodegraphIndexReadsAnUnreadableRecordAsNone: a record the warm-up cannot read or parse
// (a write cut short, a file an agent put there) holds nothing: the warm-up says so in one line
// and builds, as it does where no record is at all.
func TestWarmCodegraphIndexReadsAnUnreadableRecordAsNone(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	fakeCodegraphBundle(t, callLog, "1.5.0", `status) echo '{"initialized":true,"index":{"state":"indexing"}}' ;;
index) exit 0 ;;
`)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codegraph"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath(dir), []byte(`{"subcommand":"init","outc`), 0o600); err != nil {
		t.Fatal(err)
	}
	captured := captureStderr(t)

	WarmCodegraphIndex(context.Background(), procgroup.NoReaper, dir, time.Now())
	stderr := captured()
	if calls := stubCalls(t, callLog); !slices.Equal(calls, []string{"status", "index"}) {
		t.Fatalf("codegraph calls = %v, want status then index: an unreadable record holds nothing", calls)
	}
	if want := "[legion] codegraph warm-up for " + dir + " reads its build record " + recordPath(dir) + " as none: "; !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks the line %q:\n%s", want, stderr)
	}
}

// TestAWarmBuildRecordHolds table-tests the one rule a found record is judged by: it holds the
// build while the image's CodeGraph and the cap are the ones the recorded build died under and
// the workspace is the one it died in. A workspace with no creation record is judged by the rest.
func TestAWarmBuildRecordHolds(t *testing.T) {
	started := time.Date(2026, 11, 24, 15, 59, 0, 0, time.UTC)
	rec := warmBuild{Subcommand: "init", Started: started, Outcome: warmOutcomeMemoryCap, CodeGraph: "1.5.0", Cap: 2 << 30}
	for _, tc := range []struct {
		name               string
		version            string
		cap                uint64
		created            time.Time
		createdOK, holding bool
	}{
		{"the same CodeGraph and cap, the workspace created before the build: holds", "1.5.0", 2 << 30, started.Add(-time.Hour), true, true},
		{"the same CodeGraph and cap, no creation record: holds", "1.5.0", 2 << 30, time.Time{}, false, true},
		{"another CodeGraph: one new attempt", "1.6.0", 2 << 30, started.Add(-time.Hour), true, false},
		{"a CodeGraph that could not be read: one new attempt", "", 2 << 30, started.Add(-time.Hour), true, false},
		{"another cap: one new attempt", "1.5.0", 3 << 30, started.Add(-time.Hour), true, false},
		{"a workspace created after the build started: a new workspace", "1.5.0", 2 << 30, started.Add(time.Second), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rec.holds(tc.version, tc.cap, tc.created, tc.createdOK); got != tc.holding {
				t.Fatalf("holds(%q, %d, %s, %v) = %v, want %v", tc.version, tc.cap, tc.created, tc.createdOK, got, tc.holding)
			}
		})
	}
}
