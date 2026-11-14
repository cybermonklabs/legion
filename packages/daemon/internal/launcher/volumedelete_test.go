//go:build linux

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// The cases of the sweep's escapes: the ways a role was able to aim a sweep out of the directory
// it had listed, and the two endings of a sweep whose deleter is not there. Three of them are
// about where an open lands (volumedir.go) and delete in this process. The rest are about the
// removal of an entry, below its own name, and each holds one bound by itself: the removal
// follows no link, raced in this process with no containment
// (TestTheDeletersRemovalFollowsNoLinkWithNoContainment), and the deleter's namespace bounds a
// removal that does follow one, raced in the deleter the pod runs with Root.RemoveAll in place of
// its removal (useFollowingContainedDeleter). Beside them, the deleter is shown deleting what the
// sweep lists and no hold on the roots it deletes from.
//
// Each case of an escape stages a bait tree, drives the removal, and reads back both halves: the
// bait untouched and the entries the sweep was supposed to delete gone. Each asserts the counts
// it rests on are not zero before it reads either, so that a sweep which deleted nothing at all,
// or a fixture which staged no bait, fails rather than passes.

// volumeTrimDeleteEnv carries what the deleter is built over to the test binary re-entered as
// `legion launcher volume-trim-delete` (TestVolumeTrimDeleteHelper), as JSON, as roleInitEnv
// carries role-init's own flags to its helper: the cover it would have been given on the command
// line (RoleCover), and the root of the image's own paths it replaces, which a case points at a
// tree of its own (roleSystemRoot). It registers its marker so TestMain runs no fixtureenv.Guard
// in such a child (helperChildVariables, main_test.go).
const volumeTrimDeleteEnv = "LEGION_LAUNCHER_VOLUME_TRIM_DELETE"

func init() {
	helperChildVariables = append(helperChildVariables, volumeTrimDeleteEnv, volumeTrimOwnHoldEnv)
}

// inProcessRemovals is the remover every case that is not about containment takes, the fixture's
// own included (trimFixture.trimmer): the removal the deleter makes (removeEntryAt), through the
// descriptor the listing opened, in this process, which is where the sweep's removals were made
// before the deleter and which inode they reach either way. What it does not have is the
// deleter's namespace, which is driven by the cases that start one.
func inProcessRemovals(RoleCover, []*sweepDir) (entryRemover, error) {
	return inProcessRemover{}, nil
}

type inProcessRemover struct{}

func (inProcessRemover) removeAll(dir *sweepDir, name string) error {
	return removeEntryAt(dir.handle.file, name, make([]byte, volumeRemovalNamesBytes))
}

func (inProcessRemover) process() int { return os.Getpid() }

func (inProcessRemover) close() {}

// trimDeleteHelperPlan is what the deleter of a case is built over: the cover the trimmer passes
// it, and the root the image's own paths are taken from, which the child must be told since it
// is a process of its own. Follows has it remove with followingRemoval in place of the shipped
// removal, which only the containment cases ask for (useFollowingContainedDeleter).
type trimDeleteHelperPlan struct {
	Cover      RoleCover `json:"cover"`
	SystemRoot string    `json:"systemRoot"`
	Follows    bool      `json:"follows,omitempty"`
}

// useContainedDeleter has the sweeps of a case run the deleter the pod runs, with the test binary
// re-entered as the subcommand since a test binary is not `legion`, as the role namespace cases
// re-enter it as role-init (roleInitHelperLaunch).
func useContainedDeleter(t *testing.T) {
	t.Helper()
	launchTestDeleter(t, false)
}

// useFollowingContainedDeleter is that deleter, in its namespaces and over its covers, removing
// with followingRemoval instead of the removal it ships with. The shipped removal follows no link
// (removeEntryAt), so a link it is raced into reaches nothing whatever the covers show; a case
// that reads back what the covers bound by themselves needs a removal that does follow one, and
// this is the one the deleter made before (Root.RemoveAll).
func useFollowingContainedDeleter(t *testing.T) {
	t.Helper()
	launchTestDeleter(t, true)
}

func launchTestDeleter(t *testing.T, follows bool) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous := trimDeleteLaunch
	trimDeleteLaunch = func(cover RoleCover) ([]string, []string, error) {
		body, err := json.Marshal(trimDeleteHelperPlan{Cover: cover, SystemRoot: roleSystemRoot, Follows: follows})
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

// followingRemoval is Root.RemoveAll over the directory dir holds, which opens every directory
// it recurses into with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a link
// under, and re-opens a directory by its name after every 1024 names it has read
// (os/removeall_at.go, go1.27.3). It is the removal the deleter made before removeEntryAt, kept
// here as what the containment cases race and what the R3 case's red control is.
func followingRemoval(dir *os.File, name string, _ []byte) error {
	root, err := os.OpenRoot(descriptorPath(int(dir.Fd())))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(name)
}

// useRoleSystemRoot points the image's own paths a namespace replaces — the temporary
// directories, the lock directories, the devices and /proc — at a tree of the case's own, in this
// process and in every deleter it starts (useContainedDeleter). A case that reads back what the
// deleter covers must plant a file under each of them, which it can do in a tree of its own and
// not in the machine's /proc or /dev/pts, and must not do in the machine's /tmp.
func useRoleSystemRoot(t *testing.T, cover RoleCover, root string) {
	t.Helper()
	previous := roleSystemRoot
	roleSystemRoot = root
	t.Cleanup(func() { roleSystemRoot = previous })
	// The image's own paths, under the case's root: a namespace built over the case's cover
	// refuses a path it must replace and cannot find (eachReplaced), and the worker image has
	// every one of them. The paths it may do without are the case's to make, which a bait
	// planted under one does (plantReplacedBait).
	for _, path := range cover.requiredUnder(root) {
		mkdirFixture(t, path)
	}
}

// TestVolumeTrimDeleteHelper is the deleter itself, as `legion launcher volume-trim-delete` runs
// it: the sweep clones it into namespaces of its own with the listing's descriptors, and it reads
// what it would have been given on the command line from its environment. It exits rather than
// returning, so nothing of the test framework is written on the standard output the replies go
// down.
func TestVolumeTrimDeleteHelper(t *testing.T) {
	body := os.Getenv(volumeTrimDeleteEnv)
	if body == "" {
		return
	}
	var plan trimDeleteHelperPlan
	if err := json.Unmarshal([]byte(body), &plan); err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim-delete: what this deleter was given:", err)
		os.Exit(2)
	}
	roleSystemRoot = plan.SystemRoot
	if plan.Follows {
		removeListedEntry = followingRemoval
	}
	if err := VolumeTrimDelete(plan.Cover, os.Stdin, os.Stdout); err != nil {
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

// The same rename a moment earlier: a reclaimable root a role had already replaced with a
// symbolic link when the sweep opened it is skipped, and the tree that link names is untouched.
// Here the link stands at the root's own last element rather than above it, which is the element
// os.OpenRoot resolved itself and the one the lstat of the root's path agreed with, so this is
// the half of the swap the deleted path check was the only guard against (INFRA-2009, review
// round 7): both review agents replaced that comparison with `if false` and the package stayed
// green. What refuses it now is the open itself — one element at a time, O_NOFOLLOW and no
// O_DIRECTORY, with the kind read off the descriptor's own fstat (volumedir_linux.go) — so
// there is nothing left to compare a path against.
//
// The bait is aimed as a role would aim it: the launcher runs as the roles' own uid in the
// container's mount namespace, where /var/run/legion/roles holds every role's private, state and
// agent-secrets directories, and /legion/scratch is writable by that uid (`drwx--S--- legion
// legion`, measured in an issue pod of this deployment at 2026-11-11T08:05Z).
//
// The three roots the case leaves alone are swept as usual, so a trimmer that skipped
// everything fails here (TestAReclaimableRootUnderALinkedElementIsSkippedAndTheRestAreSwept
// reads the same half for an element above the root).
func TestARootAlreadyALinkWhenTheSweepOpensItIsSkippedAndItsBaitUntouched(t *testing.T) {
	f := newTrimFixture(t, 4)
	swapped := f.roots[3] // bun's install cache: no protected path lies under it
	if swapped.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", swapped)
	}
	bait := t.TempDir() // a sibling of the volume root, outside every reclaimable root
	at := f.clock.at.Add(-24 * time.Hour)
	// What a sweep listing through the link would meet: entries of the kind that root's rule
	// names, aged well past any minimum age, each holding a file the role may not reach.
	planted := plantBait(t, bait, at,
		filepath.Join("tool-000000001", "TOKEN"),
		filepath.Join("tool-000000002", "TOKEN"),
	)

	// The role's swap, made before the sweep opens anything: the cache root is renamed away and
	// a link to the bait is left standing under its name.
	renamed := swapped.Path + ".real"
	if err := os.Rename(swapped.Path, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bait, swapped.Path); err != nil {
		t.Fatal(err)
	}
	// The entries that went with the renamed root, which a skipped root keeps, and the entries
	// of the three roots still in place, which the sweep must take.
	var hidden, kept []string
	for _, entry := range f.entries {
		if strings.HasPrefix(entry.path, swapped.Path+"/") {
			hidden = append(hidden, filepath.Join(renamed, filepath.Base(entry.path)))
			continue
		}
		kept = append(kept, entry.path)
	}
	if len(hidden) == 0 || len(kept) == 0 {
		t.Fatalf("the case staged %d entries under the renamed root and %d to sweep, want some of each: a sweep that deleted nothing at all would pass it", len(hidden), len(kept))
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	assertBaitHeld(t, planted, "the sweep opened a reclaimable root a role had already replaced with a link to it")
	if out.deletions != len(kept) {
		t.Errorf("the sweep deleted %d entries, want the %d of the three roots still in place: %s", out.deletions, len(kept), out.reason)
	}
	for _, path := range kept {
		if f.exists(path) {
			t.Errorf("%s is still there, want the roots no link stands at swept as usual", path)
		}
	}
	for _, path := range hidden {
		if !f.exists(path) {
			t.Errorf("%s of the renamed root is gone: a root the sweep skips is one it deletes nothing of", path)
		}
	}
	if f.logged(fmt.Sprintf("the reclaimable root %s could not be opened and is skipped", swapped.Path)) == 0 {
		t.Errorf("the root the role had replaced with a link was not reported as skipped; the trimmer wrote %q", f.linesWritten())
	}
	if !f.exists(swapped.Path) {
		t.Errorf("%s is gone: a root the sweep skips is never unlinked, whatever a role left under its name", swapped.Path)
	}
	f.assertProtected()
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

// raceSwaps is the raced removal as a case drives it, a trial at a time because the window is
// the kernel's, not the case's: each trial builds the entry at victim (buildRacedEntry), arms the
// racer with a link to target (raceTheSwap), and hands trial the racer's answer. trial is the
// case's own half: it runs the removal the case races, reads the case's bait back, and answers
// whether the swap landed where the case needs it to.
// It stops once enough swaps have landed or tries trials have run, prints how many landed, and
// fails the case naming target when none did rather than passing on a window that never opened,
// or at the first trial that deleted a bait, which is gone for every trial after it.
func raceSwaps(t *testing.T, victim, target string, enough, tries int, at time.Time, trial func(trial int, swapped <-chan bool) (landed bool)) {
	t.Helper()
	sub := filepath.Join(victim, "sub")
	landed, trials := 0, 0
	deadline := time.Now().Add(testwait.Bounded(t, 2*time.Minute, 10*time.Second))
	for landed < enough && trials < tries && time.Now().Before(deadline) {
		trials++
		buildRacedEntry(t, victim, sub, at)
		swapped := make(chan bool, 1)
		go raceTheSwap(sub, target, time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second)), swapped)
		if trial(trials, swapped) {
			landed++
		}
		for _, left := range []string{victim, sub + ".real"} {
			if err := os.RemoveAll(left); err != nil {
				t.Fatal(err)
			}
		}
		if t.Failed() {
			t.Fatalf("the case ends at this trial of the swap to %s, %d of %d landed: a bait it deleted is gone for every trial after it", target, landed, trials)
		}
	}

	t.Logf("the swap to %s landed inside the removal in %d of %d trials", target, landed, trials)
	if landed == 0 {
		t.Fatalf("the swap to %s landed in none of %d trials: the window this case drives never opened, so it shows nothing", target, trials)
	}
}

// racedVictim is the entry the sweeping cases race: a name of bun's install cache, whose entries
// are its immediate children.
func racedVictim(f *trimFixture) string {
	return filepath.Join(f.roots[3].Path, "racer-victim")
}

// sweepTrial is the trial of the containment cases (raceSwaps): one sweep through the contained
// deleter the case set up, its deletions added to deletions, the swap counted only when the
// recursion re-opened the name after it (swapLanded), and every bait of planted read back by what
// it is under. The deletions are asked for so that a case whose deleter deleted nothing at all
// does not pass.
func sweepTrial(t *testing.T, f *trimFixture, target string, planted map[string][]baitFile, deletions *int) func(int, <-chan bool) bool {
	sub := filepath.Join(racedVictim(f), "sub")
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	return func(_ int, swapped <-chan bool) bool {
		tr := f.trimmer()
		tr.removals = startRemover(&pidGuard{}) // the contained deleter, and no fallback behind it
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
		*deletions += out.deletions

		landed := <-swapped && swapLanded(sub)
		for under, bait := range planted {
			assertBaitHeld(t, bait, fmt.Sprintf("a removal recursed through a link to %s, which a role left where a subdirectory of the entry stood, into %s", target, under))
		}
		return landed
	}
}

// The second bound, by itself: what the covers leave a removal that does follow a link. The
// deleter's own removal follows none: TestTheDeletersRemovalFollowsNoLinkWithNoContainment races
// it with no cover at all and reads its bait back held, so racing it here would read every bait
// back held whether its cover was in place or not. This case gives the contained deleter the
// removal it made before instead (followingRemoval, useFollowingContainedDeleter):
// Root.RemoveAll unlinks a name, and on EISDIR opens it again with the raw
// `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a link under, and re-opens it that way once
// more after every 1024 entries it has read (os/removeall_at.go, go1.27.3). A role that swaps a
// subdirectory of an entry for a link in that window has the recursion delete the link's target,
// and a role can name any absolute path in a link it leaves.
//
// So the case aims the link at every path a role's own namespace replaces, one at a time: the
// cover the launcher passes — the roles directory, which holds another role's private, state
// and agent-secrets directories and its launcher token; the home mounts directory, where the
// container mounts the pod's shared config home that each role's own config home is copied
// from; the home, which is the lower layer of every role's home overlay; and the fixed root's
// own mount points — and the image's own paths, which this case points at a tree of its own
// (useRoleSystemRoot). The container's mount namespace shows the launcher all of them as the
// container holds them and a role's own namespace shows it none of them, which is what
// containment takes away (volumedelete.go).
//
// The link names the covered path itself, never the directory it sits in. A removal let into
// the directory above meets each cover at its mount point, where unlinkat answers EBUSY in a
// tree whose directories are writable, and Root.RemoveAll recurses only on EISDIR, EPERM or
// EACCES: it passes the cover over unread, so a cover that still showed the container's own
// files there kept the case green (INFRA-2009, review round 10, with the fixed root's private
// mount point bound onto itself). Named itself, the covered path is what the recursion re-opens,
// and what it then deletes is whatever the deleter is shown there: an empty filesystem of its
// own, or the container's files with the bait among them. At /proc, where the deleter is given
// a procfs of its own, the link names a directory under it that only the container's tree
// holds instead (replacedBait.within).
//
// There is a bait under each path (roleReplacedBait) and every trial reads every one of them
// back, so a cover left out of the deleter's list, or one that shows what the container holds
// at its path, is that path's bait deleted and named in the trial whose link names it. Each path
// is driven until its swap has landed once, and a path whose swap never landed fails by name.
//
// So this case runs the deleter the pod runs but for its removal: every removal of the sweep is
// made by a child in a user, mount, PID and IPC namespace of its own, and a link the recursion
// follows to one of those paths lands in what that child is shown there.
func TestASubdirectorySwappedForALinkWhileARemovalRecursesReachesNoOtherRolesFiles(t *testing.T) {
	requireRoleNamespaces(t)
	f := newTrimFixture(t, 1)
	system := filepath.Join(f.coverRoot, "system")
	useRoleSystemRoot(t, f.cover, system)
	useFollowingContainedDeleter(t)
	at := f.clock.at.Add(-2 * time.Hour)
	// What the roles container holds for the roles and no role holds itself, one bait under
	// every path the deleter covers: what a sweep that followed the link would delete, as
	// review 6 found it deleting another role's files, review 8 the pod's shared config home
	// and review 9 what the container holds at a role's own /tmp.
	planted := plantReplacedBait(t, f.cover, system, at)

	deletions := 0
	for _, under := range roleReplacedBait(f.cover, system) {
		target := under.swapTarget()
		raceSwaps(t, racedVictim(f), target, 1, 4, at, sweepTrial(t, f, target, planted, &deletions))
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

// The working directory is the one way into a covered directory that no mount closes: a child
// inherits the launcher's, and an inherited directory is a handle, which /proc/self/cwd names
// whatever the path it was opened through holds now. Measured by review 9 against the deleter
// before this change, which set none: the swap reached the launcher's own working directory
// through /proc/self/cwd, in one trial of one. Like the case above it races a deleter that removes
// with followingRemoval, since a removal that follows no link reaches no cover at all.
//
// So the deleter stands at the root of its own namespace before it reads a request
// (containDeletions), and this case puts the launcher's working directory inside a covered
// directory and aims the swap at a name under /proc/self/cwd. It is a name under it and not the
// directory itself because /proc/self/cwd of a deleter standing at the root of its namespace
// names `/`, which no cover of a process that must reach /legion takes away and which this case
// has no business driving a removal of: what it reads back is that the inherited directory is
// not a way past a cover.
func TestTheDeletersInheritedWorkingDirectoryIsNoWayPastACover(t *testing.T) {
	requireRoleNamespaces(t)
	f := newTrimFixture(t, 1)
	useRoleSystemRoot(t, f.cover, filepath.Join(f.coverRoot, "system"))
	useFollowingContainedDeleter(t)
	at := f.clock.at.Add(-2 * time.Hour)
	// Another role's own files, under a name of the roles directory the deleter covers.
	const inside = "tester"
	planted := plantBait(t, f.cover.RolesDir, at,
		filepath.Join(inside, RolePrivateDir, "notes.md"),
		filepath.Join(inside, RoleStateDir, "omp", "profile.json"),
		filepath.Join(inside, RoleLauncherDir, LauncherTokenFile),
	)
	// The launcher's working directory, which its child inherits where nothing sets one.
	t.Chdir(f.cover.RolesDir)

	deletions := 0
	target := filepath.Join("/proc/self/cwd", inside)
	raceSwaps(t, racedVictim(f), target, 4, 4, at,
		sweepTrial(t, f, target, map[string][]baitFile{"the working directory the deleter inherited": planted}, &deletions))
	if deletions == 0 {
		t.Fatal("the sweeps deleted nothing at all, so the containment the case reads back was never asked to delete anything")
	}
	f.assertProtected()
}

// The deleter the pod runs, over the image's own paths rather than a tree of a case's own: it
// covers the container's /tmp, /var/tmp, lock directories, shared memory, terminals and message
// queues where they are, mounts a procfs of its own PID namespace over the container's /proc,
// and still deletes what the sweep lists — through the descriptors the trimmer passed it, which
// it opens in that procfs of its own (descriptorPath, volumedir.go). The two cases above point
// those paths at a tree of their own, since nothing can plant a bait in the machine's /proc, so
// neither says whether the deletions still work when the paths covered are the image's.
//
// The cover is laid out outside the machine's temporary directory, because a cover under /tmp
// is a cover this deleter hides from itself (namespaceScratch, as the role namespace cases do
// it). The volume the sweep deletes in needs no such thing: every removal is made through a
// descriptor the listing opened and passed, and no path of the volume is resolved in the child.
func TestTheDeleterOverTheImagesOwnPathsDeletesWhatTheSweepLists(t *testing.T) {
	requireRoleNamespaces(t)
	useContainedDeleter(t)
	f := newTrimFixture(t, 4)
	f.cover = coverUnder(t, namespaceScratch(t))
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	tr.removals = startRemover(&pidGuard{}) // the deleter the pod runs, and no fallback behind it
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d of the %d entries the fixture staged: %s", out.deletions, len(f.entries), out.reason)
	}
	if left := len(f.entries) - len(f.gone()); left != 0 {
		t.Errorf("%d of the %d entries the fixture staged are still there, want them deleted by a deleter built over the image's own paths", left, len(f.entries))
	}
	// Nothing of the pod's log is read back here: a sweep that deleted everything it listed
	// through a deleter that started and answered writes no line at all (trimFixture.logged is
	// where a case about the log reads one).
	f.assertProtected()
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
	tr.removals = func(cover RoleCover, dirs []*sweepDir) (entryRemover, error) {
		asked++
		// Every directory the deleter covers, and not the roles directory alone: a cover the
		// sweep does not pass is a cover the child cannot mount (RoleCover).
		if cover != f.cover {
			t.Errorf("the sweep asked for a deleter covering %+v, want the cover this launcher was given, %+v", cover, f.cover)
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
	tr.removals = func(RoleCover, []*sweepDir) (entryRemover, error) {
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
	return inProcessRemover{}.removeAll(dir, name)
}

func (r *dyingRemover) process() int { return os.Getpid() }

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
			tr.removals = func(RoleCover, []*sweepDir) (entryRemover, error) {
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
	return inProcessRemover{}.removeAll(dir, name)
}

func (r *countingRemover) process() int { return os.Getpid() }

func (r *countingRemover) close() { *r.closed++ }
