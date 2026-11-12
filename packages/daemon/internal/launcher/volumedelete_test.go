//go:build linux

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// The cases of the sweep's escapes: the ways a role was able to aim a sweep out of the directory
// it had listed, and the two endings of a sweep whose deleter is not there. Three of them are
// about where an open lands (volumedir.go) and one about where the kernel lands inside a removal
// the sweep makes and does not drive (volumedelete.go), which is why that one runs the deleter
// the pod runs and the others delete in this process.
//
// Each stages a bait tree, drives the sweep, and reads back both halves: the bait untouched and
// the entries the sweep was supposed to delete gone. Each asserts the counts it rests on are not
// zero before it reads either, so that a sweep which deleted nothing at all, or a fixture which
// staged no bait, fails rather than passes.

// volumeTrimDeleteEnv carries the three directories the deleter covers (roleCover) to the test
// binary re-entered as `legion launcher volume-trim-delete` (TestVolumeTrimDeleteHelper), as
// JSON, as roleInitEnv carries role-init's own flags to its helper. It registers its marker so
// TestMain runs no fixtureenv.Guard in such a child (helperChildVariables, main_test.go).
const volumeTrimDeleteEnv = "LEGION_LAUNCHER_VOLUME_TRIM_DELETE"

func init() {
	helperChildVariables = append(helperChildVariables, volumeTrimDeleteEnv)
}

// inProcessRemovals is the remover every case that is not about containment takes, the fixture's
// own included (trimFixture.trimmer): the removal is made through the descriptor the listing
// opened, in this process, which is where the sweep's removals were made before the deleter and
// which inode they reach either way. What it does not bound is where the kernel goes inside that
// removal, which is the whole of the contained deleter's work and is driven by the one case that
// starts one.
func inProcessRemovals(roleCover, []*sweepDir) (entryRemover, error) {
	return inProcessRemover{}, nil
}

type inProcessRemover struct{}

func (inProcessRemover) removeAll(dir *sweepDir, name string) error {
	return dir.handle.root.RemoveAll(name)
}

func (inProcessRemover) close() {}

// useContainedDeleter has the sweeps of a case run the deleter the pod runs, with the test binary
// re-entered as the subcommand since a test binary is not `legion`, as the role namespace cases
// re-enter it as role-init (roleInitHelperLaunch).
func useContainedDeleter(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous := trimDeleteLaunch
	trimDeleteLaunch = func(cover roleCover) ([]string, []string, error) {
		body, err := json.Marshal(cover)
		if err != nil {
			return nil, nil, err
		}
		// A binary built with -race sleeps atexit_sleep_ms before it exits, which a deleter
		// reaped inside the sweep's grace would spend on every sweep (roleInitHelperLaunch).
		gorace := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
		return []string{self, "-test.run=^TestVolumeTrimDeleteHelper$"},
			[]string{volumeTrimDeleteEnv + "=" + string(body), "GORACE=" + gorace}, nil
	}
	t.Cleanup(func() { trimDeleteLaunch = previous })
}

// TestVolumeTrimDeleteHelper is the deleter itself, as `legion launcher volume-trim-delete` runs
// it: the sweep clones it into namespaces of its own with the listing's descriptors, and it reads
// the directories it would have been given on the command line from its environment. It exits
// rather than returning, so nothing of the test framework is written on the standard output the
// replies go down.
func TestVolumeTrimDeleteHelper(t *testing.T) {
	body := os.Getenv(volumeTrimDeleteEnv)
	if body == "" {
		return
	}
	var cover roleCover
	if err := json.Unmarshal([]byte(body), &cover); err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim-delete: the cover this deleter was given:", err)
		os.Exit(2)
	}
	if err := VolumeTrimDelete(cover.RolesDir, cover.HomeMounts, cover.Home, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim-delete:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// baitFile is one file a sweep that escaped would delete, with what it held.
type baitFile struct {
	path    string
	content string
}

// plantBait writes one file at each path under dir and ages everything down to it, so that a tree
// a swapped link aims the sweep at passes every guard the sweep has: the kind, the minimum age,
// and the pre-delete re-read of the time.
func plantBait(t *testing.T, dir string, at time.Time, paths ...string) []baitFile {
	t.Helper()
	var planted []baitFile
	for _, under := range paths {
		path := filepath.Join(dir, under)
		mkdirFixture(t, filepath.Dir(path))
		content := "bait: " + under
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		planted = append(planted, baitFile{path: path, content: content})
	}
	for _, bait := range planted {
		for over := bait.path; len(over) > len(dir); over = filepath.Dir(over) {
			if err := os.Chtimes(over, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(planted) == 0 {
		t.Fatal("the case staged no bait, so a sweep that deleted nothing at all would pass it")
	}
	return planted
}

// assertBaitHeld reads every bait file back: anything missing or changed is the sweep having
// deleted outside the directories it listed, through the escape the case drives.
func assertBaitHeld(t *testing.T, planted []baitFile, what string) {
	t.Helper()
	for _, bait := range planted {
		switch content, err := os.ReadFile(bait.path); {
		case err != nil:
			t.Errorf("the bait %s: %v; %s", bait.path, err, what)
		case string(content) != bait.content:
			t.Errorf("the bait %s holds %q, want %q; %s", bait.path, content, bait.content, what)
		}
	}
}

// agedUnder writes one file and gives every directory above it, up to but not including root, the
// same time: an entry the sweep must find old enough to delete.
func agedUnder(t *testing.T, root, path string, at time.Time) {
	t.Helper()
	mkdirFixture(t, filepath.Dir(path))
	writeAged(t, path, at)
	for over := filepath.Dir(path); len(over) > len(root); over = filepath.Dir(over) {
		if err := os.Chtimes(over, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A role owns the directories above a reclaimable root — /legion/scratch is writable by the role
// uid (`drwx--S--- legion legion`, measured in an issue pod of this deployment at
// 2026-11-11T08:05Z) — so it can rename `scratch` and leave a symbolic link to a tree of its own
// in its place, with no race at all. A sweep that opened `/legion/scratch/cache` in one call
// landed in that tree: this cluster's kernel follows a link under the
// `openat(O_RDONLY|O_NOFOLLOW|O_DIRECTORY)` os.Root resolves every non-final element with
// (volumedir.go, where it is measured), and the lstat of the same path the sweep checked its
// handle against followed that very element too, so the two agreed and the sweep listed and
// deleted in the link's target (INFRA-2009, review 7).
//
// What must happen instead: each element of the root's path is opened in turn, a link anywhere in
// it refuses the open, and the root is skipped with a line naming it. The one root this case
// leaves above the link is swept as usual, so a trimmer that skipped everything fails here.
func TestAReclaimableRootUnderALinkedElementIsSkippedAndTheRestAreSwept(t *testing.T) {
	f := newTrimFixture(t, 4)
	bait := t.TempDir()
	at := f.clock.at.Add(-2 * time.Hour)
	// What the three roots under `scratch` would show if the link were followed: an entry of
	// each root's own kind, so that every guard the sweep has passes it.
	planted := plantBait(t, bait, at,
		filepath.Join("tmp", "go-build000000000", "b1", "out"),
		filepath.Join("cache", "tool-bait", "b1", "out"),
		filepath.Join("bun", "install", "cache", "pkg-bait", "b1", "out"),
	)

	// The one root that is not under scratch, whose entries the sweep must still delete.
	var kept []string
	for _, entry := range f.entries {
		if strings.HasPrefix(entry.path, f.roots[1].Path+"/") {
			kept = append(kept, entry.path)
		}
	}
	if len(kept) == 0 {
		t.Fatal("the fixture staged no entry outside scratch, so a sweep that deleted nothing would pass this case")
	}

	scratch := filepath.Join(f.root, "scratch")
	if err := os.Rename(scratch, scratch+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bait, scratch); err != nil {
		t.Fatal(err)
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	assertBaitHeld(t, planted, "the sweep opened a reclaimable root through a link a role left above its last element")
	if out.deletions != len(kept) {
		t.Errorf("the sweep deleted %d entries, want the %d of the one root no link stands above: %s", out.deletions, len(kept), out.reason)
	}
	for _, path := range kept {
		if f.exists(path) {
			t.Errorf("%s is still there, want the roots a link does not stand above swept as usual", path)
		}
	}
	for _, under := range []int{0, 2, 3} {
		if f.logged(f.roots[under].Path) == 0 {
			t.Errorf("the root %s was skipped without a line; the trimmer wrote %q", f.roots[under].Path, f.linesWritten())
		}
	}
	f.assertPrefixed()
}

// A shard of the Go build cache is a directory of a root a role owns, so between the root's
// listing and the shard's own open a role can rename the shard away and leave a link in its
// place. The link this case leaves is the shape os.Root followed: `0f -> zz/sh` is relative and
// stays lexically inside the root, so Root.OpenRoot read it and went on to resolve its own
// elements — and `zz -> /bait` is an absolute escape it would have refused, which it never saw,
// because the open it resolves a non-final element with is the one this kernel follows a link
// under (volumedir.go, where it is measured). The sweep then listed and deleted in /bait/sh.
//
// The swap is staged from the trimmer's own clock at the first reading after the listing has
// reached the entries of the first shard, which the line the kind guard writes for the link
// planted there shows: by then the root's own listing is read — it is one read, and it recorded
// `0f` as the directory it then was — and no shard after `00` has been opened yet. That is the
// race the fix has to survive, driven without a racing goroutine.
//
// Only the Go build cache root is swept here, so what the case reads back is the shard that was
// swapped and the shards that were not.
func TestAShardSwappedForARelativeLinkCrossingAnAbsoluteOneIsSkipped(t *testing.T) {
	f := newTrimFixture(t, 8)
	cache := f.roots[1]
	if cache.Entry != GoCacheEntries {
		t.Fatalf("the second reclaimable root is %+v, want the Go build cache, whose entries sit in a shard", cache)
	}
	at := f.clock.at.Add(-3 * time.Hour)
	bait := t.TempDir()
	planted := plantBait(t, bait, at,
		filepath.Join("sh", "0000000000000000000000000000000000000000000000000000000000000001-a"),
		filepath.Join("sh", "0000000000000000000000000000000000000000000000000000000000000002-d"),
	)
	// The absolute link the relative one crosses, left in the cache root as a role can leave it:
	// the root's own listing reads it as the link it is and never opens it.
	if err := os.Symlink(bait, filepath.Join(cache.Path, "zz")); err != nil {
		t.Fatal(err)
	}
	// The shard the swap hides, with entries of its own.
	swapped := filepath.Join(cache.Path, "0f")
	var hidden []string
	for _, name := range []string{
		"000000000000000000000000000000000000000000000000000000000000000a-a",
		"000000000000000000000000000000000000000000000000000000000000000b-d",
	} {
		agedUnder(t, cache.Path, filepath.Join(swapped, name), at)
		hidden = append(hidden, name)
	}
	// The marker the swap is staged on: a link named as a cache entry in the first shard, which
	// the kind guard passes over with a line of its own while the listing is inside that shard.
	const marker = "00000000000000000000000000000000000000000000000000000000000000ff-a"
	if err := os.Symlink(filepath.Join(cache.Path, "zz"), filepath.Join(cache.Path, "00", marker)); err != nil {
		t.Fatal(err)
	}

	// Every entry of the shards the swap leaves alone, which the sweep must delete.
	var kept []string
	for _, entry := range f.entries {
		if strings.HasPrefix(entry.path, cache.Path+"/") {
			kept = append(kept, entry.path)
		}
	}
	if len(kept) == 0 {
		t.Fatal("the fixture staged no Go cache entry outside the swapped shard, so a sweep that deleted nothing would pass this case")
	}

	staged := false
	f.clock.setHook(func(*fakeClock) {
		if staged || f.logged(marker) == 0 {
			return
		}
		staged = true
		if err := os.Rename(swapped, swapped+".real"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(filepath.Join("zz", "sh"), swapped); err != nil {
			t.Error(err)
		}
	})
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	tr.roots = []ReclaimableRoot{cache}
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if !staged {
		t.Fatal("the case never swapped the shard: the listing never reached the entries of the first shard, so it drove nothing")
	}
	assertBaitHeld(t, planted, "the sweep opened a shard through a relative link that crossed an absolute one")
	if out.deletions != len(kept) {
		t.Errorf("the sweep deleted %d entries, want the %d of the shards no link stands in: %s", out.deletions, len(kept), out.reason)
	}
	for _, path := range kept {
		if f.exists(path) {
			t.Errorf("%s is still there, want the shards the swap left alone swept as usual", path)
		}
	}
	for _, name := range hidden {
		if !f.exists(filepath.Join(swapped+".real", name)) {
			t.Errorf("%s of the renamed shard is gone, want the entries of a shard the sweep could not open left where they are", name)
		}
	}
	if f.logged(swapped) == 0 {
		t.Errorf("the swapped shard was skipped without a line; the trimmer wrote %q", f.linesWritten())
	}
	f.assertPrefixed()
}

// A role can put a named pipe where a cache root belongs. Every open the trimmer makes carries
// O_NONBLOCK for that reason: measured in an issue pod of this deployment on 2026-11-12, kernel
// 4.19.0-gvisor, os.OpenRoot of a FIFO was still blocked three seconds after the call, and the
// goroutine it would block is the one the launcher's Run waits on at the container's stop
// (volumedir.go). The open answers at once instead, the fstat refuses what it opened, and the
// root is skipped with a line while the rest of the volume is swept.
//
// The sweep runs in a goroutine under a bound of its own so that a trimmer which blocks again
// fails here rather than hanging the package until the test binary's deadline.
func TestAFifoWhereACacheRootBelongsIsSkippedAndDoesNotBlockTheSweep(t *testing.T) {
	f := newTrimFixture(t, 4)
	piped := f.roots[3]
	if err := os.RemoveAll(piped.Path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(piped.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, entry := range f.entries {
		if !strings.HasPrefix(entry.path, piped.Path+"/") {
			kept = append(kept, entry.path)
		}
	}
	if len(kept) == 0 || len(kept) == len(f.entries) {
		t.Fatalf("the fixture staged %d entries of which %d are outside the piped root, want some of each", len(f.entries), len(kept))
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	swept := make(chan sweepOutcome, 1)
	go func() { swept <- tr.sweep(context.Background(), *trimSettings(), steady(90)(0)) }()

	var out sweepOutcome
	select {
	case out = <-swept:
	case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
		t.Fatalf("the sweep did not return: an open of the named pipe at %s is waiting for a writer that never comes, and the launcher's Run waits on this goroutine at the container's stop", piped.Path)
	}

	if out.deletions != len(kept) {
		t.Errorf("the sweep deleted %d entries, want the %d of the roots that are directories: %s", out.deletions, len(kept), out.reason)
	}
	for _, path := range kept {
		if f.exists(path) {
			t.Errorf("%s is still there, want the roots beside the piped one swept as usual", path)
		}
	}
	switch info, err := os.Lstat(piped.Path); {
	case err != nil:
		t.Errorf("the named pipe at %s: %v; a root that is not a directory is skipped, never deleted", piped.Path, err)
	case info.Mode()&os.ModeNamedPipe == 0:
		t.Errorf("%s is %s, want the named pipe this case left there", piped.Path, kindOf(info.Mode()))
	}
	if f.logged(piped.Path) == 0 {
		t.Errorf("the piped root was skipped without a line; the trimmer wrote %q", f.linesWritten())
	}
	f.assertProtected()
	f.assertPrefixed()
}

// The racer's entry: how many names the subdirectory of the swapped entry holds. One more than
// the 1024 a removal reads in a batch, so the recursion must close and re-open the name at least
// once — without a second batch there is no re-open and no window at all. They are directories
// rather than files so that each costs the recursion an open, a read and a remove rather than one
// unlink, which is what makes the first batch long enough to swap inside.
const racedChildren = 1024 + 8

// buildRacedEntry makes the entry one trial of the raced case deletes: one subdirectory holding
// racedChildren directories, every one of them old enough for the sweep to delete the entry (the
// age guard reads the newest time anywhere in it).
func buildRacedEntry(t *testing.T, victim, sub string, at time.Time) {
	t.Helper()
	mkdirFixture(t, sub)
	for child := range racedChildren {
		if err := os.Mkdir(filepath.Join(sub, fmt.Sprintf("d%04d", child)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for child := range racedChildren {
		path := filepath.Join(sub, fmt.Sprintf("d%04d", child))
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{sub, victim} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// raceTheSwap waits for the removal to begin inside sub — the first of its names gone, which
// nothing but the recursion does — and then renames sub away and puts a link to target in its
// place, which is what a role with the pod's own uid can do to a directory of its own cache. It
// answers whether it got the swap in at all; a trial it could not is a trial that drove nothing,
// and the case counts those.
func raceTheSwap(sub, target string, deadline time.Time, swapped chan<- bool) {
	for time.Now().Before(deadline) {
		held, err := os.Open(sub)
		if err != nil {
			break // the entry is gone: the removal finished before the swap
		}
		names, err := held.Readdirnames(-1)
		_ = held.Close()
		if err != nil || len(names) >= racedChildren {
			continue
		}
		if err := os.Rename(sub, sub+".real"); err != nil {
			break
		}
		swapped <- os.Symlink(target, sub) == nil
		return
	}
	swapped <- false
}

// swapLanded is whether the recursion re-opened the name after the swap, which is the window this
// case exists to drive: the link is still there — a removal that met it before it had opened the
// name at all would have unlinked it as the link it is — and the renamed directory still holds
// the names of the batches the recursion never came back for, which a removal that met the swap
// after it had emptied the directory would have left empty.
func swapLanded(sub string) bool {
	info, err := os.Lstat(sub)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	left, err := os.ReadDir(sub + ".real")
	return err == nil && len(left) > 0
}

// The one escape no handle of the sweep's can bound: the kernel's own walk inside a removal the
// sweep makes once and does not drive. Root.RemoveAll unlinks a name, and on EISDIR opens it
// again with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a link under, and
// re-opens it that way once more after every 1024 entries it has read (os/removeall_at.go,
// go1.27.3). A role that swaps a subdirectory of an entry for a link in that window has the
// recursion delete the link's target, and a role can name any absolute path in a link it
// leaves.
//
// So the target this case gives it is the directory the three the deleter covers sit under
// (roleCover): the roles directory, which holds another role's private, state and
// agent-secrets directories and its launcher token; the home mounts directory, where the
// container mounts the pod's shared config home that each role's own config home is copied
// from; and the home, which is the lower layer of every role's home overlay. The container's
// mount namespace shows the launcher all three and a role's own namespace shows it none of
// them, which is what containment takes away (volumedelete.go). They are one directory apart
// here where the pod has /var/run/legion/roles, /var/run/legion/home and /home/legion
// (trimFixture.coverRoot), so that one swap drives every cover rather than one of them.
//
// There is a bait under each, and a cover left out of the deleter's list is that cover's bait
// deleted and named. Measured on 2026-11-12, kernel 4.19.0-gvisor, under go1.27.3, each cover
// taken out on its own: the swap landed in four trials of four and that cover's bait alone was
// gone in every one of them — the shared config home's jj configuration with the home mounts
// directory out of roleCover.covered, the home's with the home out of containDeletions, and
// the four files of another role's directories with the roles directory out of covered.
//
// So this is the one case that runs the deleter the pod runs: every removal of the sweep is made
// by a child in a user, mount, PID and IPC namespace of its own with those three covered by an
// empty filesystem, and a link the recursion follows lands in the cover.
//
// It is a trial at a time because the window is the kernel's, not the case's. Each trial prints
// nothing on its own; the case prints how many of them landed, so a reader sees what it drove,
// and fails when none did rather than passing on a window that never opened.
func TestASubdirectorySwappedForALinkWhileARemovalRecursesReachesNoOtherRolesFiles(t *testing.T) {
	requireRoleNamespaces(t)
	useContainedDeleter(t)
	f := newTrimFixture(t, 1)
	at := f.clock.at.Add(-2 * time.Hour)
	// What the roles container holds for the roles and no role holds itself, one bait under
	// each directory the deleter covers: what a sweep that followed the link would delete, as
	// review 6 found it deleting another role's files and review 8 the pod's shared config
	// home.
	planted := map[string][]baitFile{
		"the roles directory, where another role's own credentials are": plantBait(t, f.rolesDir, at,
			filepath.Join("tester", RolePrivateDir, "notes.md"),
			filepath.Join("tester", RoleStateDir, "omp", "profile.json"),
			filepath.Join("tester", RoleAgentSecretsDir, "ANTHROPIC_API_KEY"),
			filepath.Join("tester", RoleLauncherDir, LauncherTokenFile),
		),
		"the home mounts directory, where the pod's shared config home is mounted": plantBait(t, f.homeMounts, at,
			filepath.Join(ConfigHomeDir, "jj", "config.toml"),
		),
		"the home, which is the lower layer of every role's own": plantBait(t, f.home, at,
			filepath.Join(ConfigHomeDir, "jj", "config.toml"),
			".bashrc",
		),
	}
	target := f.coverRoot

	victim := filepath.Join(f.roots[3].Path, "racer-victim")
	sub := filepath.Join(victim, "sub")
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	landed, trials, deletions := 0, 0, 0
	deadline := time.Now().Add(testwait.Bounded(t, 2*time.Minute, 10*time.Second))
	for trials < 4 && time.Now().Before(deadline) {
		trials++
		buildRacedEntry(t, victim, sub, at)
		swapped := make(chan bool, 1)
		go raceTheSwap(sub, target, time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second)), swapped)

		tr := f.trimmer()
		tr.removals = startRemover // the deleter the pod runs, and no fallback behind it
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
		deletions += out.deletions

		if <-swapped && swapLanded(sub) {
			landed++
		}
		for under, bait := range planted {
			assertBaitHeld(t, bait, "a removal recursed through a link a role left where a subdirectory of the entry stood, into "+under)
		}
		for _, left := range []string{victim, sub + ".real"} {
			if err := os.RemoveAll(left); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Logf("the swap landed inside the removal's re-open in %d of %d trials", landed, trials)
	if landed == 0 {
		t.Fatalf("the swap landed in none of %d trials: the window this case drives never opened, so it shows nothing", trials)
	}
	// The sweep's own work, which the contained deleter did and a case that only proved the bait
	// untouched would not have asked for: every entry the fixture staged is gone.
	if deletions == 0 {
		t.Fatal("the sweeps deleted nothing at all, so the containment the case reads back was never asked to delete anything")
	}
	if left := len(f.entries) - len(f.gone()); left != 0 {
		t.Errorf("%d of the %d entries the fixture staged are still there, want them deleted by the contained deleter", left, len(f.entries))
	}
	f.assertProtected()
}

// Every directory the deleter covers reaches it on its command line. The case above drives the
// covers end to end through this test binary re-entered, which carries them in its environment
// since a test binary is not `legion` (useContainedDeleter), so this is what holds the argv of
// the real `legion launcher volume-trim-delete` to the whole of the cover: a directory of
// roleCover the trimmer does not pass is a directory the pod's child never covers.
func TestTheDeletersCommandLineNamesEveryDirectoryItCovers(t *testing.T) {
	cover := roleCover{RolesDir: "/var/run/legion/roles", HomeMounts: "/var/run/legion/home", Home: "/home/legion"}
	argv, _, err := trimDeleteLaunch(cover)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, VolumeTrimDeleteSubcommand) {
		t.Fatalf("the deleter is run as %v, want this binary's own %s subcommand", argv, VolumeTrimDeleteSubcommand)
	}
	flags := map[string]string{}
	for i := 0; i+1 < len(argv); i++ {
		if strings.HasPrefix(argv[i], "--") {
			flags[argv[i]] = argv[i+1]
		}
	}
	for _, named := range []struct{ flag, dir string }{
		{"--roles-dir", cover.RolesDir},
		{"--home-mounts", cover.HomeMounts},
		{"--home", cover.Home},
	} {
		if flags[named.flag] != named.dir {
			t.Errorf("the deleter is run with %s %q, want %q: %v", named.flag, flags[named.flag], named.dir, argv)
		}
	}
	// The whole list and not the three named above: a directory added to roleCover.covered is
	// one more the child must be told, and a flag carrying it is the only way it is.
	for _, dir := range append(cover.covered(), cover.Home) {
		if !slices.Contains(argv, dir) {
			t.Errorf("the deleter is run as %v, which names %s nowhere: a directory it is not told is a directory it does not cover", argv, dir)
		}
	}
}

// A sweep whose deleter will not start deletes nothing at all. There is no fallback to deleting
// in the launcher's own view: that is the behaviour this change exists to end, and a sweep that
// skips a cycle costs the pod one interval of a volume that is already full. The reason is in
// one line of the pod's log and in the report the daemon gets, because a trimmer silently
// freeing nothing on a full volume is the pod nobody is told about.
func TestASweepWhoseDeleterCannotStartDeletesNothingAndSaysSo(t *testing.T) {
	f := newTrimFixture(t, 4)
	refused := errors.New("clone the deleter into a mount namespace of its own: operation not permitted")
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	asked := 0
	tr.removals = func(cover roleCover, dirs []*sweepDir) (entryRemover, error) {
		asked++
		// Every directory the deleter covers, and not the roles directory alone: a cover the
		// sweep does not pass is a cover the child cannot mount (roleCover).
		if cover != f.cover() {
			t.Errorf("the sweep asked for a deleter covering %+v, want the three this launcher was given, %+v", cover, f.cover())
		}
		if len(dirs) == 0 {
			t.Error("the sweep asked for a deleter holding no directory of the listing, want every directory it will delete out of")
		}
		return nil, refused
	}
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if asked != 1 {
		t.Errorf("the sweep started %d deleters, want exactly one for the whole of its deletions", asked)
	}
	if out.deletions != 0 || len(f.gone()) != 0 {
		t.Errorf("the sweep reported %d deletions and %d entries are gone, want nothing deleted without a deleter to delete through", out.deletions, len(f.gone()))
	}
	if !strings.Contains(out.reason, refused.Error()) {
		t.Errorf("the sweep stopped because %q, want the reason the deleter would not start carried into the report", out.reason)
	}
	if !out.report {
		t.Error("the sweep that deleted nothing asks for no report, want one: the volume is still above the low mark and nobody else can tell the operator")
	}
	if said := f.logged(refused.Error()); said != 1 {
		t.Errorf("the trimmer wrote %d lines naming why the deleter would not start, want the one line a person reads in the pod; it wrote %q", said, f.linesWritten())
	}
	f.assertProtected()
	f.assertPrefixed()
}

// A deleter that dies while the sweep runs ends the sweep where it stands: what it deleted
// stands, nothing more is deleted, and the reason reaches the log and the report, as a /proc read
// the kernel refused mid-sweep does. The sweep must not finish its list in the launcher's own
// view, which is what a fallback would do.
func TestASweepWhoseDeleterDiesKeepsWhatItDeletedAndStops(t *testing.T) {
	const before = 2
	f := newTrimFixture(t, 4)
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	died := fmt.Errorf("%w: read its answer: EOF", errRemoverGone)
	closed := 0
	tr.removals = func(roleCover, []*sweepDir) (entryRemover, error) {
		return &dyingRemover{after: before, err: died, closed: &closed}, nil
	}
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if out.deletions != before {
		t.Errorf("the sweep deleted %d entries, want the %d its deleter answered before it died: %s", out.deletions, before, out.reason)
	}
	if len(f.gone()) != before {
		t.Errorf("%d entries of the fixture are gone, want the %d the deleter removed: a sweep that went on without its deleter deleted the rest in the launcher's own view", len(f.gone()), before)
	}
	if !strings.Contains(out.reason, errRemoverGone.Error()) {
		t.Errorf("the sweep stopped because %q, want the deleter named as the reason", out.reason)
	}
	if said := f.logged(errRemoverGone.Error()); said != 1 {
		t.Errorf("the trimmer wrote %d lines naming the deleter that died, want one; it wrote %q", said, f.linesWritten())
	}
	if closed != 1 {
		t.Errorf("the sweep closed its deleter %d times, want once however the sweep ended: no child of it may outlive the sweep that made it", closed)
	}
	f.assertProtected()
	f.assertPrefixed()
}

// dyingRemover removes after entries through the handle the listing opened and answers err to
// every request after that, as the contained deleter answers once its child is gone
// (containedRemover.ended).
type dyingRemover struct {
	after  int
	err    error
	closed *int
}

func (r *dyingRemover) removeAll(dir *sweepDir, name string) error {
	if r.after == 0 {
		return r.err
	}
	r.after--
	return dir.handle.root.RemoveAll(name)
}

func (r *dyingRemover) close() { *r.closed++ }

// No child of a sweep's may outlive the sweep that made it. The launcher's Run waits on the
// trimmer's goroutine at the container's stop and gives it the pod's whole termination grace,
// and a deleter left behind would hold the listing's descriptors on directories nobody is
// deleting out of, in the process that is PID 1 of the roles container (volumedelete.go).
//
// So the deleter is closed on every way out of the sweep. Three of the four are here — the
// sweep that ran out of entries, the settings that stopped the trimmer mid-sweep, and the
// container's stop — and the fourth, the deleter that died under it, is the case above. The two
// stops are staged after the first deletion, because a stop that arrived during the listing
// ends the sweep before any deleter is started at all.
func TestASweepClosesItsDeleterHoweverItEnded(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(f *trimFixture, cancel context.CancelFunc)
	}{
		{"no entry left to delete", func(*trimFixture, context.CancelFunc) {}},
		{"the settings stopped the trimmer", func(f *trimFixture, _ context.CancelFunc) { f.setSettings(nil) }},
		{"the container is stopping", func(_ *trimFixture, cancel context.CancelFunc) { cancel() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 4)
			f.useVolume(steady(90))
			f.setSettings(trimSettings())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.clock.setHook(func(*fakeClock) {
				if len(f.gone()) > 0 {
					tc.stop(f, cancel)
				}
			})
			tr := f.trimmer()
			started, closed := 0, 0
			tr.removals = func(roleCover, []*sweepDir) (entryRemover, error) {
				started++
				return &countingRemover{closed: &closed}, nil
			}
			out := tr.sweep(ctx, *trimSettings(), steady(90)(0))

			if started != 1 {
				t.Fatalf("the sweep started %d deleters, want the one it makes its deletions through: %s", started, out.reason)
			}
			if out.deletions == 0 {
				t.Fatalf("the sweep deleted nothing, so it never reached the ending this case stages: %s", out.reason)
			}
			if closed != 1 {
				t.Errorf("the sweep closed its deleter %d times, want exactly once: %s", closed, out.reason)
			}
		})
	}
}

// countingRemover removes through the handle the listing opened and counts the closes, which is
// all a case about the deleter's lifetime reads.
type countingRemover struct{ closed *int }

func (r *countingRemover) removeAll(dir *sweepDir, name string) error {
	return dir.handle.root.RemoveAll(name)
}

func (r *countingRemover) close() { *r.closed++ }
