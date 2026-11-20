//go:build linux

package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// The cache trimmer's cases. The file builds only under Linux, as volumeguard_test.go does: two
// of its cases run a real process and read the container's own /proc, which is the pod the
// trimmer was written for and the one thing a fixture /proc cannot prove, and the rest keep
// company with them because they are the same trimmer driven a cheaper way.
//
// Every case builds a volume of its own — the four reclaimable roots rebased onto a temporary
// directory, each in the layout its own tool uses, with one file under every protected path —
// and drives the trimmer on an injected clock and an injected volume reader that counts its
// calls. Nothing here reads or writes the pod's own /legion: the trimmer takes its volume root,
// its /proc, its own pid and its roots as fields for exactly that reason.

// The helper child this file runs: the test binary re-executed to hold an open handle on a
// directory, which is what a role's `flock` on a cache root looks like to the trimmer. It
// registers its marker as volumeguard_test.go registers its own, so TestMain runs no
// fixtureenv.Guard in such a child (helperChildVariables, main_test.go).
const volumeTrimHandleEnv = "LEGION_LAUNCHER_VOLUME_TRIM_HANDLE"

func init() {
	helperChildVariables = append(helperChildVariables, volumeTrimHandleEnv)
}

// The volume the cases model: fifty thousand million bytes, about the 50 GiB claim of the issue
// pod this was written for (INFRA-2009's pod filled 45 GiB of 49), and divisible by a thousand
// so that one cache entry is exactly a tenth of a percentage point of it. A case can then say
// in points what a sweep must free and read the answer off the number of deletions.
const (
	fixtureVolumeBytes = uint64(50_000_000_000)
	fixtureEntryBytes  = fixtureVolumeBytes / 1000
)

// percentBytes is the used bytes of a reading at percent of the modelled volume.
func percentBytes(percent float64) uint64 {
	return uint64(float64(fixtureVolumeBytes) * percent / 100)
}

// fixtureEntry is one entry of one reclaimable root as a case built it.
type fixtureEntry struct {
	path string
	rule EntryRule
	at   time.Time
	// bytes is what the modelled volume reading loses when this entry goes. A hardlinked entry
	// records none: unlinking the cached copy frees nothing while the link in a workspace
	// stands, which is bun's install cache and the reason the sweep reads its progress from the
	// volume rather than counting the bytes it unlinked.
	bytes uint64
}

// fixtureFile is one file the trimmer must never touch, with what it held when it was written.
type fixtureFile struct {
	path    string
	content []byte
	at      time.Time
}

// The file each protected path of the pod holds in a case's tree, keyed by the path relative to
// the volume root. The table is driven from ProtectedPaths itself, so a path added to that
// constant without a line here fails every case of this file rather than quietly going
// unguarded.
var fixtureProtectedFiles = map[string]string{
	"workspaces":                       "cybermonklabs/legion/infra-2009/packages/daemon/go.mod",
	"repos":                            "github.com/cybermonklabs/legion/.jj/repo/store/op_heads",
	"feed":                             "issue.json",
	"scratch/state":                    "planner/omp/profile.json",
	"scratch/data":                     "omp/sessions.sqlite",
	"go/mod":                           "github.com/sjawhar/legion@v0.1.0/go.mod",
	"uv/cache":                         "wheels-v1/pkg-1.0-py3-none-any.whl",
	"uv/python":                        "cpython-3.13.1-linux-x86_64/bin/python3",
	"scratch/cache/ms-playwright":      "chromium-1200/chrome-linux/chrome",
	"scratch/cache/legion-claude-code": "node_modules/.package-lock.json",
	"scratch/leak-check":               "public-leak-check.sh",
}

// fixtureRoleWork is a role's own work in $TMPDIR, outside any `go-build` directory: the clone a
// role made in the pod's scratch, which is what the hand deletion of INFRA-2009 destroyed and
// which the trimmer's $TMPDIR rule — Go's own build trees and nothing else — must leave alone.
const fixtureRoleWork = "scratch/tmp/role-work/clone/.jj/repo/store/op_heads"

// fixtureKeptFiles is how many files of a case's tree the trimmer must never touch, which every
// sweeping case counts before it sweeps and reads back after: one under each protected path,
// the role's own work in $TMPDIR above, the notes of a $TMPDIR directory that is not Go's, and
// the three names of the Go build cache that are no entry — its own trim.txt and README, which
// sit in the cache root rather than in a shard, and a shard name ending in neither -a nor -d.
var fixtureKeptFiles = len(ProtectedPaths) + 5

// trimFixture is one pod's volume as a case drives it.
type trimFixture struct {
	t     *testing.T
	root  string // the volume root of this case
	proc  string // the /proc the trimmer reads: a fixture, or the container's own
	self  int
	roots []ReclaimableRoot
	// coverRoot holds every directory of the cover each sweep's deleter is built over (RoleCover,
	// volumedelete.go): the pod's per-role root, the directory the roles container mounts the
	// home's volumes under, the home and the fixed root. They sit under one directory of the
	// case's own, where the pod has /var/run/legion/roles, /var/run/legion/home, /home/legion and
	// /var/run/legion, and a case that drives the covers lays the image's own paths out beside
	// them (useRoleSystemRoot). A case of this file deletes in its own process and covers none of
	// them, so each is an empty directory: the cases that drive the covers are
	// volumedelete_test.go's.
	coverRoot string
	cover     RoleCover

	protected  []fixtureFile
	entries    []*fixtureEntry
	namedAfter int // how many entries the builder has made, which names and ages them

	clock *fakeClock
	ticks *fakeTicks

	mu      sync.Mutex
	set     *shimwire.LauncherVolumeTrim
	period  time.Duration // the interval of the last settings in force (setSettings)
	onSet   func()
	reads   int
	reading func(call int) volumeUsage
	onRead  func(call int)
	sent    []shimwire.LauncherVolumeReport
	sendErr error
	lines   []string
}

// newTrimFixture builds the pod's volume: every protected path with a file in it, and perRoot
// entries in each reclaimable root, in the layout that root's own tool uses. The entries are
// aged by a permutation of their creation order, so that no case can pass by deleting in the
// order the names or the directories read in.
func newTrimFixture(t *testing.T, perRoot int) *trimFixture {
	t.Helper()
	root := t.TempDir()
	cover := t.TempDir()
	f := &trimFixture{
		t:         t,
		root:      root,
		self:      os.Getpid(),
		roots:     rebasedRoots(root),
		coverRoot: cover,
		cover:     coverUnder(t, cover),
		clock:     newFakeClock(),
		ticks:     &fakeTicks{},
		reading: func(int) volumeUsage {
			return volumeUsage{UsedBytes: percentBytes(50), AvailableBytes: fixtureVolumeBytes - percentBytes(50)}
		},
	}
	f.proc, _ = writeProcFixture(t)
	f.buildProtected()
	f.buildRoots(perRoot)
	f.assertKeptBuilt()
	return f
}

// coverUnder is a cover of a case's own: one directory under root for each the pod's layout
// names, made from the one table that names them (roleCoverFlags), and every path under those a
// namespace built over the cover must replace (RoleCover.replaced), which the worker image lays
// out for a role and a case lays out here. So a directory added to the cover is a directory every
// case of these files is given without a second edit.
func coverUnder(t *testing.T, root string) RoleCover {
	t.Helper()
	var cover RoleCover
	for _, named := range roleCoverFlags {
		dir := filepath.Join(root, named.flag)
		*named.at(&cover) = dir
		mkdirFixture(t, dir)
	}
	for _, path := range cover.requiredUnder(root) {
		mkdirFixture(t, path)
	}
	return cover
}

// rebase is a path of the pod's volume on a case's own tree.
func rebase(root, path string) string {
	return filepath.Join(root, strings.TrimPrefix(path, VolumeRoot+"/"))
}

// rebasedRoots is the written-out ReclaimableRoots on a case's own tree, with every rule and
// every exception as the constant holds them: a case drives the trimmer's real list, so a root
// added to it or a rule widened is swept here too.
func rebasedRoots(root string) []ReclaimableRoot {
	out := make([]ReclaimableRoot, 0, len(ReclaimableRoots))
	for _, reclaimable := range ReclaimableRoots {
		out = append(out, ReclaimableRoot{
			Path:   rebase(root, reclaimable.Path),
			Entry:  reclaimable.Entry,
			Except: reclaimable.Except,
		})
	}
	return out
}

// rebasedOnto is roots on a case's own tree, with every rule and every exception as roots holds
// them. The fixture's trimmer takes the roots newTrimmer built through it (trimFixture.trimmer),
// so a case that empties the environment before it builds the trimmer sweeps the roots the
// trimmer built under that environment, and not a list the fixture built before it.
func rebasedOnto(root string, roots []ReclaimableRoot) []ReclaimableRoot {
	out := make([]ReclaimableRoot, 0, len(roots))
	for _, reclaimable := range roots {
		out = append(out, ReclaimableRoot{
			Path:   rebase(root, reclaimable.Path),
			Entry:  reclaimable.Entry,
			Except: reclaimable.Except,
		})
	}
	return out
}

func (f *trimFixture) buildProtected() {
	f.t.Helper()
	at := f.clock.at.Add(-90 * time.Minute)
	for _, protected := range ProtectedPaths {
		relative := strings.TrimPrefix(protected, VolumeRoot+"/")
		under, ok := fixtureProtectedFiles[relative]
		if !ok {
			f.t.Fatalf("the protected path %s has no file in this file's table: add one, or a case here would pass without ever showing the trimmer leaves it alone", protected)
		}
		f.keep(filepath.Join(rebase(f.root, protected), under), at)
	}
	f.keep(filepath.Join(f.root, fixtureRoleWork), at)
}

// keep writes one file the trimmer must never touch and records what it held.
func (f *trimFixture) keep(path string, at time.Time) {
	f.t.Helper()
	mkdirFixture(f.t, filepath.Dir(path))
	content := []byte("kept: " + path)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		f.t.Fatal(err)
	}
	f.protected = append(f.protected, fixtureFile{path: path, content: content, at: at})
}

// assertKeptBuilt counts, before any sweep, the files of the case's tree the trimmer must never
// touch, as they stand on the volume: one under each protected path — the workspaces, the clone,
// the feed, a role's state home, the data home, the Go module cache, uv's two directories,
// ms-playwright, legion-claude-code and the leak check — the role's own work and a directory's
// notes in $TMPDIR outside any `go-build` tree, and the Go build cache's three names that are no
// entry. Every case builds its tree in newTrimFixture, which asks this, so every sweeping case
// asserts the count before its sweep as well as after it (assertProtected), and a fixture that
// built none of one kind fails here rather than passing a case that never showed the trimmer
// leaving that kind alone.
func (f *trimFixture) assertKeptBuilt() {
	f.t.Helper()
	present := 0
	for _, file := range f.protected {
		if f.exists(file.path) {
			present++
		}
	}
	if present != fixtureKeptFiles {
		f.t.Fatalf("before any sweep the fixture holds %d of the files the trimmer must never touch (%d recorded), want the %d every case counts: %v",
			present, len(f.protected), fixtureKeptFiles, f.protected)
	}
	holds := func(under string, kept func(path string) bool) bool {
		return slices.ContainsFunc(f.protected, func(file fixtureFile) bool {
			return strings.HasPrefix(file.path, under+"/") && kept(file.path) && f.exists(file.path)
		})
	}
	for _, protected := range ProtectedPaths {
		if under := rebase(f.root, protected); !holds(under, func(string) bool { return true }) {
			f.t.Fatalf("before any sweep the fixture holds no file under the protected path %s (%s): a case would pass without ever showing the trimmer leaves it alone", protected, under)
		}
	}
	for _, root := range f.roots {
		if root.Entry != GoBuildTrees {
			continue
		}
		outsideGoBuild := func(path string) bool {
			first, _, _ := strings.Cut(strings.TrimPrefix(path, root.Path+"/"), "/")
			return !isGoBuildTree(first)
		}
		if !holds(root.Path, outsideGoBuild) {
			f.t.Fatalf("before any sweep the fixture holds no file in $TMPDIR (%s) outside a go-build directory: a case would pass without ever showing the trimmer leaves a role's own work there alone", root.Path)
		}
	}
}

// buildRoots fills each reclaimable root with perRoot entries in its own tool's layout.
func (f *trimFixture) buildRoots(perRoot int) {
	f.t.Helper()
	for _, root := range f.roots {
		mkdirFixture(f.t, root.Path)
		switch root.Entry {
		case GoBuildTrees:
			// What `go build` leaves in $TMPDIR: one directory per build, holding its own
			// shards, which Go names and owns.
			for at := range perRoot {
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("go-build%09d", at)), root.Entry)
			}
			// A directory of $TMPDIR that is not Go's: never an entry, whatever its age.
			f.keep(filepath.Join(root.Path, "tmp.XXXXscratch", "notes.md"), f.clock.at.Add(-30*24*time.Hour))
		case GoCacheEntries:
			// What GOCACHE holds: two-character shard directories of names ending `-a` and
			// `-d`, with the cache's own trim.txt and README in the root rather than in a
			// shard, and one name in a shard with neither suffix.
			for at := range perRoot {
				shard := filepath.Join(root.Path, fmt.Sprintf("%02x", at%16))
				mkdirFixture(f.t, shard)
				suffix := "-a"
				if at%4 == 3 {
					suffix = "-d"
				}
				name := filepath.Join(shard, fmt.Sprintf("%032x%s", at, suffix))
				if suffix == "-d" && at%8 == 3 {
					f.addTree(name, root.Entry) // the directory form, removed whole
					continue
				}
				f.addFile(name, root.Entry)
			}
			for _, own := range []string{"trim.txt", "README"} {
				f.keep(filepath.Join(root.Path, own), f.clock.at.Add(-30*24*time.Hour))
			}
			f.keep(filepath.Join(root.Path, "00", "0123456789abcdef0123456789abcdef"), f.clock.at.Add(-30*24*time.Hour))
		case ImmediateChildren:
			for at := range perRoot {
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("tool-%09d", at)), root.Entry)
			}
		}
	}
}

// addTree makes one entry that is a directory, with a file inside it and every time in it set to
// the entry's own age: the newest modification time anywhere in it is what the sweep orders by.
func (f *trimFixture) addTree(path string, rule EntryRule) *fixtureEntry {
	f.t.Helper()
	at := f.ageOf()
	inside := filepath.Join(path, "b1")
	mkdirFixture(f.t, inside)
	writeAged(f.t, filepath.Join(inside, "out"), at)
	for _, dir := range []string{inside, path} {
		if err := os.Chtimes(dir, at, at); err != nil {
			f.t.Fatal(err)
		}
	}
	return f.record(path, rule, at)
}

// addFile makes one entry that is a single file.
func (f *trimFixture) addFile(path string, rule EntryRule) *fixtureEntry {
	f.t.Helper()
	at := f.ageOf()
	writeAged(f.t, path, at)
	return f.record(path, rule, at)
}

// ageOf is the age of the next entry: a permutation of the creation order, so that the oldest
// entries are neither the first made nor the first in any directory listing.
func (f *trimFixture) ageOf() time.Time {
	at := f.namedAfter
	f.namedAfter++
	return f.clock.at.Add(-2 * time.Hour).Add(-time.Duration(at*37%1021) * time.Minute)
}

func (f *trimFixture) record(path string, rule EntryRule, at time.Time) *fixtureEntry {
	entry := &fixtureEntry{path: path, rule: rule, at: at, bytes: fixtureEntryBytes}
	f.entries = append(f.entries, entry)
	return entry
}

// oldestFirst is every entry the builder made, in the order the sweep must delete them.
func (f *trimFixture) oldestFirst() []*fixtureEntry {
	out := slices.Clone(f.entries)
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].at.Equal(out[b].at) {
			return out[a].at.Before(out[b].at)
		}
		return out[a].path < out[b].path
	})
	return out
}

// byName is every entry in the order their paths sort, which is the negative control: a trimmer
// that ordered by name rather than by modification time would delete these.
func (f *trimFixture) byName() []*fixtureEntry {
	out := slices.Clone(f.entries)
	sort.SliceStable(out, func(a, b int) bool { return out[a].path < out[b].path })
	return out
}

func (f *trimFixture) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// gone is every entry the sweep deleted.
func (f *trimFixture) gone() []string {
	var out []string
	for _, entry := range f.entries {
		if !f.exists(entry.path) {
			out = append(out, entry.path)
		}
	}
	return out
}

// modelled is the volume as this case's own tree: a base the rest of the volume holds plus the
// bytes of every entry still there. It is how a case drives the one rule the sweep's progress
// turns on — the reading moves when the volume moves, and not when the trimmer unlinks.
func (f *trimFixture) modelled(start float64) func(int) volumeUsage {
	var total uint64
	for _, entry := range f.entries {
		total += entry.bytes
	}
	base := percentBytes(start) - total
	return func(int) volumeUsage {
		used := base
		for _, entry := range f.entries {
			if f.exists(entry.path) {
				used += entry.bytes
			}
		}
		return volumeUsage{UsedBytes: used, AvailableBytes: fixtureVolumeBytes - used}
	}
}

// usageAt is one reading of the modelled volume at percent of it.
func usageAt(percent float64) volumeUsage {
	used := percentBytes(percent)
	return volumeUsage{UsedBytes: used, AvailableBytes: fixtureVolumeBytes - used}
}

// steady is a volume that never moves, whatever the sweep deletes.
func steady(percent float64) func(int) volumeUsage {
	return func(int) volumeUsage { return usageAt(percent) }
}

// useVolume installs the case's reader in place of the package's own and counts its calls, which
// is what the cases on the sweep's hundred-deletion cadence are asserted on.
func (f *trimFixture) useVolume(reading func(call int) volumeUsage) {
	f.mu.Lock()
	f.reading = reading
	f.mu.Unlock()
	previous := readVolumeUsage
	readVolumeUsage = func(path string) (volumeUsage, error) {
		if path != f.root {
			f.t.Errorf("the trimmer read the volume at %s, want the case's own tree %s", path, f.root)
		}
		f.mu.Lock()
		f.reads++
		call, answer, hook := f.reads, f.reading, f.onRead
		f.mu.Unlock()
		if hook != nil {
			hook(call)
		}
		return answer(call), nil
	}
	f.t.Cleanup(func() { readVolumeUsage = previous })
}

func (f *trimFixture) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// setOnRead installs the hook a case runs at an exact reading of the volume: it is how a case
// blocks a sweep where it stands, or panics in the trimmer's own work.
func (f *trimFixture) setOnRead(hook func(call int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRead = hook
}

// setOnSettings installs the hook a case runs when the trimmer asks for the settings in force,
// which a sweep does at every entry boundary.
func (f *trimFixture) setOnSettings(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onSet = hook
}

// settings is the trimmer's settings seam, with the hook a case uses to stop the trimmer in the
// middle of a sweep: the sweep asks at every entry boundary.
func (f *trimFixture) settings() *shimwire.LauncherVolumeTrim {
	f.mu.Lock()
	hook := f.onSet
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.set == nil {
		return nil
	}
	set := *f.set
	return &set
}

// trimming is the trimmer's second settings seam, the one a sweep asks at every entry boundary:
// it reads the same field through the same hook as settings and answers whether the trimmer can
// run what is there, so a case that stops the trimmer mid-sweep, with no settings or with settings
// it refuses, stops it here as the pod's own manager does (manager.volumeTrimming).
func (f *trimFixture) trimming() bool {
	set := f.settings()
	return set != nil && len(refusedTrimSettings(*set)) == 0
}

// setSettings is what the trimmer reads from now on. The interval of the last settings that
// were in force is kept, because the clock a case ticks is the one of that period and settings
// that stop the trimmer name none.
func (f *trimFixture) setSettings(set *shimwire.LauncherVolumeTrim) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = set
	if set != nil {
		f.period = settingsInterval(*set)
	}
}

// interval is the period of the clock the trimmer waits on between readings.
func (f *trimFixture) interval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.period
}

func (f *trimFixture) send(report shimwire.LauncherVolumeReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, report)
	return nil
}

func (f *trimFixture) refuseSends(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

func (f *trimFixture) reports() []shimwire.LauncherVolumeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

func (f *trimFixture) log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	f.mu.Lock()
	f.lines = append(f.lines, line)
	f.mu.Unlock()
	f.t.Log(line)
}

// logged is how many lines the trimmer wrote holding want.
func (f *trimFixture) logged(want string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, line := range f.lines {
		if strings.Contains(line, want) {
			count++
		}
	}
	return count
}

// linesWritten is every line the trimmer wrote, which every case holds to the one prefix a
// person greps for in the pod.
func (f *trimFixture) linesWritten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lines)
}

func (f *trimFixture) assertPrefixed() {
	f.t.Helper()
	lines := f.linesWritten()
	if len(lines) == 0 {
		f.t.Fatal("the trimmer wrote no line at all: a person reading the pod's log would see nothing of it")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, volumeTrimLog) {
			f.t.Errorf("the trimmer wrote %q, want every line behind the prefix %q", line, volumeTrimLog)
		}
	}
}

// trimmer is the trimmer of this case: the pod's own constructor, with the machine it reads
// pointed at the case's tree.
//
// Its roots are the ones newTrimmer built, rebased onto that tree (rebasedOnto), rather than the
// fixture's own list, which was built with the tree: a constructor that read the environment
// would otherwise never reach a sweep, whatever environment the case built the trimmer under.
//
// Its removals are made in this process through the handle the listing opened, where the pod's
// are made by a child in a mount namespace of its own: what the cases of this file are about is
// which entries a sweep chooses and which it passes over, and a fork, an exec and a mount per
// sweep would buy none of that. The containment itself, and the sweep's refusal to delete
// anything at all without it, are volumedelete_test.go's cases, which give the trimmer the
// deleter the pod runs (entryRemover, volumedelete.go).
func (f *trimFixture) trimmer() *trimmer {
	tr := newTrimmer(f.settings, f.trimming, f.send, f.log, f.cover, &pidGuard{})
	tr.volumeRoot, tr.procRoot, tr.self = f.root, f.proc, f.self
	tr.roots = rebasedOnto(f.root, tr.roots)
	tr.now, tr.ticks = f.clock.now, f.ticks.make
	tr.removals = inProcessRemovals
	return tr
}

// start runs the interval loop and stops it with the case.
func (f *trimFixture) start(tr *trimmer) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.run(ctx)
	}()
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testwait.Bounded(f.t, 10*time.Second, time.Second)):
			f.t.Error("the trimmer's loop did not return after the case cancelled it")
		}
	})
	return cancel, done
}

// tick delivers one interval tick on the clock the trimmer is waiting on and waits for the line
// that ends the interval, which is how a case holds the loop to one interval at a time.
//
// The clock is the one running at the interval of the settings in force, because the trimmer
// takes a second clock of the same seam for the size walk's budget, and a tick offered on that
// one would cut a walk rather than start an interval. The offer is tried again until it is
// taken, for the moment between a sweep's last line and the new interval clock the loop makes
// when the sweep returns, and because an offer that is taken is itself the proof that the
// interval before it ended: the loop is back at its select.
func (f *trimFixture) tick(ends string) {
	f.t.Helper()
	before := f.logged(ends)
	deadline := time.Now().Add(testwait.Bounded(f.t, 30*time.Second, time.Second))
	taken := false
	for !taken && time.Now().Before(deadline) {
		at, running := f.ticks.live(f.interval())
		if !running {
			time.Sleep(testwait.Poll)
			continue
		}
		taken = f.ticks.offer(f.t, at, 200*time.Millisecond)
	}
	if !taken {
		f.t.Fatal("the trimmer took no tick: it is not waiting on an interval clock")
	}
	testwait.Eventually(f.t, "the interval to end with a line holding "+strconv.Quote(ends), func() bool {
		return f.logged(ends) > before
	})
}

// fakeClock is the trimmer's clock under a case's hand. It moves by step on every reading, which
// is how a case spends the sweep's budget, and it runs a hook at an exact reading of it, which
// is how a case moves the world under a running sweep: a process that starts, a file that is
// written, a stop that arrives.
//
// It starts at the machine's own time rather than at a date of its own, because a case's tree
// is a real tree: a file the sweep's own removal rewrites, or one a case writes under a running
// sweep, takes the kernel's time, and the trimmer's minimum age must be read off the same clock
// those times come from.
type fakeClock struct {
	mu    sync.Mutex
	at    time.Time
	start time.Time
	step  time.Duration
	hook  func(clock *fakeClock)
}

func newFakeClock() *fakeClock {
	at := time.Now()
	return &fakeClock{at: at, start: at}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	hook := c.hook
	c.at = c.at.Add(c.step)
	c.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(by)
}

// moved is how far the case has taken the clock from where it started.
func (c *fakeClock) moved() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at.Sub(c.start)
}

func (c *fakeClock) setHook(hook func(clock *fakeClock)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = hook
}

// fakeTicks is the interval clock the trimmer takes, and the size walk's budget: every clock the
// trimmer made, with the period it asked for and whether it stopped it.
type fakeTicks struct {
	mu      sync.Mutex
	made    []chan time.Time
	every   []time.Duration
	stopped []bool
}

func (k *fakeTicks) make(every time.Duration) (<-chan time.Time, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	channel := make(chan time.Time)
	at := len(k.made)
	k.made = append(k.made, channel)
	k.every = append(k.every, every)
	k.stopped = append(k.stopped, false)
	return channel, func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.stopped[at] = true
	}
}

func (k *fakeTicks) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.made)
}

func (k *fakeTicks) periodOf(at int) time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.every[at]
}

// live is the newest clock of that period the trimmer has not stopped, and false while there is
// none: the trimmer takes one clock for its interval and another, of the walk's budget, while a
// report is being made.
func (k *fakeTicks) live(period time.Duration) (int, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for at := len(k.made) - 1; at >= 0; at-- {
		if !k.stopped[at] && k.every[at] == period {
			return at, true
		}
	}
	return -1, false
}

// offer sends one tick on the clock made at, and answers whether the trimmer took it.
func (k *fakeTicks) offer(t *testing.T, at int, within time.Duration) bool {
	t.Helper()
	k.mu.Lock()
	channel := k.made[at]
	k.mu.Unlock()
	select {
	case channel <- time.Time{}:
		return true
	case <-time.After(within):
		return false
	}
}

// The settings a case runs by unless it says otherwise: the marks of a pod whose volume is read
// every half minute.
func trimSettings() *shimwire.LauncherVolumeTrim {
	return &shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 30}
}

// A value outside the trimmer's own ranges is refused, the key and the value are logged, and the
// trimmer stays off rather than starting its ticker. The daemon that sent them is another binary
// across a connection this design expects to carry version skew both ways, and the cost of
// believing one is paid in the process that is PID 1 of the roles container: time.NewTicker
// panics on a non-positive period, a fault on each start of the interval loop, and three in a row
// would switch off the pod's one guard of the volume six live roles share (run).
func TestTheTrimmerRefusesSettingsOutsideItsRangesAndStaysOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  shimwire.LauncherVolumeTrim
		want []string
	}{
		{"an interval of zero seconds, which panics time.NewTicker",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 0},
			[]string{"interval_seconds is 0"}},
		{"an interval of a negative number of seconds",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: -5},
			[]string{"interval_seconds is -5"}},
		{"an interval beyond the hour the range allows",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 3601},
			[]string{"interval_seconds is 3601"}},
		{"a high mark of zero, which would sweep an empty volume",
			shimwire.LauncherVolumeTrim{HighPercent: 0, LowPercent: 0, MinAgeMinutes: 30, IntervalSeconds: 30},
			[]string{"high_percent is 0", "low_percent is 0"}},
		{"a high mark of a hundred, which no reading reaches",
			shimwire.LauncherVolumeTrim{HighPercent: 100, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 30},
			[]string{"high_percent is 100"}},
		{"a low mark at the high mark, which no sweep could reach",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 85, MinAgeMinutes: 30, IntervalSeconds: 30},
			[]string{"low_percent is 85 and high_percent is 85"}},
		{"a low mark above the high mark",
			shimwire.LauncherVolumeTrim{HighPercent: 70, LowPercent: 85, MinAgeMinutes: 30, IntervalSeconds: 30},
			[]string{"low_percent is 85 and high_percent is 70"}},
		{"a minimum age of zero minutes, which would delete a cache being written",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 0, IntervalSeconds: 30},
			[]string{"min_age_minutes is 0"}},
		// The value a daemon of another release could send, and the one the reviewer of this
		// work measured in a pod: at 2147483647 minutes the sweep's cutoff read 2035-09-05,
		// after the sweep's own start, because the minutes are multiplied into a time.Duration
		// of int64 nanoseconds (INFRA-2009, review round 5). The refusal is written from the
		// wire's own constant, as the loader's two cases are (config's volume_trim_test.go).
		{"a minimum age past the maximum, whose cutoff would land after the sweep's own start",
			shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: shimwire.VolumeMinAgeMax + 1, IntervalSeconds: 30},
			[]string{fmt.Sprintf("min_age_minutes is %d", shimwire.VolumeMinAgeMax+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 1)
			f.useVolume(steady(99))
			set := tc.set
			f.setSettings(&set)
			tr := f.trimmer()

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				tr.run(ctx)
			}()
			testwait.Eventually(t, "the trimmer to refuse the settings and stay off", func() bool {
				return f.logged("stays off") > 0
			})

			// The only clock it may have made is the one it asks for settings on again: a
			// ticker of the refused interval is the panic this check exists for.
			for at := range f.ticks.count() {
				if period := f.ticks.periodOf(at); period != volumeSettingsWait {
					t.Errorf("the trimmer made a clock of %s on refused settings, want none but the %s it waits for settings on: it must not start a ticker it would have to panic on", period, volumeSettingsWait)
				}
			}
			if reads := f.readCount(); reads != 0 {
				t.Errorf("the trimmer read the volume %d times on refused settings, want none", reads)
			}
			for _, want := range tc.want {
				if f.logged(want) == 0 {
					t.Errorf("the trimmer logged no line holding %q; it wrote %q", want, f.linesWritten())
				}
			}
			if f.logged("stays off") == 0 {
				t.Errorf("the trimmer did not log that it stays off; it wrote %q", f.linesWritten())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(testwait.Bounded(t, 10*time.Second, time.Second)):
				t.Fatal("run did not return when the container's own context ended")
			}
			f.assertPrefixed()
		})
	}

	// The control: the settings a pod runs by are accepted, so the refusals above are the
	// ranges doing their work and not a trimmer that refuses everything.
	if refusals := refusedTrimSettings(*trimSettings()); len(refusals) > 0 {
		t.Errorf("the settings a pod runs by were refused: %v", refusals)
	}
	for _, edge := range []shimwire.LauncherVolumeTrim{
		{HighPercent: 99, LowPercent: 1, MinAgeMinutes: 1, IntervalSeconds: 1},
		{HighPercent: 2, LowPercent: 1, MinAgeMinutes: shimwire.VolumeMinAgeMax, IntervalSeconds: 3600},
	} {
		if refusals := refusedTrimSettings(edge); len(refusals) > 0 {
			t.Errorf("the settings at the edge of the ranges %+v were refused: %v", edge, refusals)
		}
	}
}

// An age whose cutoff would overflow never reaches a sweep, and the entry a build wrote moments
// ago is still on the volume afterwards. The sweep's cutoff is its start less MinAgeMinutes of
// time.Duration, int64 nanoseconds: from 153,722,868 minutes up the product wraps and the
// cutoff lands after the sweep's own start, so `at.After(eligible)` is false for every entry
// and the listing takes every entry of every root however recently it was written (list) —
// with only the used-path guard and the pre-delete re-read between a build's own cache and the
// unlink. The bound is what keeps such a value out of the trimmer altogether
// (shimwire.VolumeMinAgeMax, refusedTrimSettings), and this is that bound read as the pod sees
// it rather than as a refusal's text.
func TestAnAgeWhoseCutoffWouldOverflowNeverSweepsAnEntryWrittenMomentsAgo(t *testing.T) {
	// The value the reviewer of this work measured in a pod: the cutoff read 2035-09-05, after
	// the sweep's start (INFRA-2009, review round 5).
	const overflowingAge = 2147483647
	f := newTrimFixture(t, 2)
	// At the high mark and never falling, so a sweep that starts deletes every entry it listed
	// rather than stopping at the low mark before it reaches the newest of them.
	f.useVolume(steady(99))

	// The cache entry of a build in flight: written moments ago, well inside any minimum age an
	// operator could ask for. It is retimed by hand, every path of it together, because addTree
	// ages an entry by the fixture's own permutation — the entry's age is the newest time
	// anywhere in it.
	fresh := f.addTree(filepath.Join(f.roots[3].Path, "fresh-install-cache-entry"), f.roots[3].Entry)
	for _, path := range []string{filepath.Join(fresh.path, "b1", "out"), filepath.Join(fresh.path, "b1"), fresh.path} {
		if err := os.Chtimes(path, f.clock.at, f.clock.at); err != nil {
			t.Fatal(err)
		}
	}
	fresh.at = f.clock.at

	// What the case rests on, counted before the trimmer runs: the entries a sweep would have
	// to list, and the fresh one among them at an age no sane minimum would let go.
	if len(f.entries) == 0 || !f.exists(fresh.path) {
		t.Fatalf("the fixture holds %d entries and the fresh one at %s is there: %v — the case would read a behaviour nothing staged", len(f.entries), fresh.path, f.exists(fresh.path))
	}
	if age := f.clock.at.Sub(fresh.at); age > 0 {
		t.Fatalf("the fresh entry reads %s old, want the moment the case wrote it", age)
	}

	set := trimSettings()
	set.MinAgeMinutes = overflowingAge
	f.setSettings(set)
	f.start(f.trimmer())

	// The trimmer is given every chance to sweep: a tick is offered for as long as it has an
	// interval clock to take one on. A trimmer that refuses these settings makes no such clock
	// — only the one it asks for settings on again — so nothing is offered and the wait ends
	// at the refusal instead.
	deadline := time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second))
	for time.Now().Before(deadline) && f.logged("stays off") == 0 && f.logged("the sweep ended") == 0 {
		if at, running := f.ticks.live(settingsInterval(*set)); running {
			f.ticks.offer(t, at, 200*time.Millisecond)
			continue
		}
		time.Sleep(testwait.Poll)
	}

	if !f.exists(fresh.path) {
		t.Errorf("the trimmer deleted %s, written moments ago, under a minimum age of %d minutes: that age wraps the sweep's cutoff past the sweep's own start, which makes every entry of every root eligible however recently it was written", fresh.path, overflowingAge)
	}
	if gone := f.gone(); len(gone) > 0 {
		t.Errorf("the trimmer deleted %d of the %d entries under an age it must refuse: %v", len(gone), len(f.entries), gone)
	}
	if f.logged("stays off") == 0 {
		t.Errorf("the trimmer did not refuse the settings and stay off; it wrote %q", f.linesWritten())
	}
	if reads := f.readCount(); reads != 0 {
		t.Errorf("the trimmer read the volume %d times on refused settings, want none", reads)
	}
	f.assertProtected()
	f.assertPrefixed()
}

// The greatest minimum age an operator may ask for is one the sweep's own cutoff carries: swept
// at it, an entry a minute older than that age is deleted and one a minute inside it is kept.
// The age goes in force as settings and the sweep runs by them, so what is read here is the
// cutoff the sweep computed for itself and not a subtraction the case did for it.
//
// The minutes are written out rather than read off shimwire.VolumeMinAgeMax, because a case
// staged from the constant moves with it: with the maximum lowered, these settings are refused,
// the trimmer sweeps nothing on them and the week-old entry below is still there, which is what
// turns this case red (refusedTrimSettings). It holds the maximum from below alone: a maximum
// raised rather than lowered is held by the case below
// (TestAnAgeAMinutePastTheGreatestIsRefusedAndSweepsNothing), since the overflow case above is
// written at 2147483647 and says nothing until the maximum reaches 153,722,868.
//
// It is the launcher's half of one bound: the loader reads the same constant for
// min_age_minutes (config's readVolumeTrim) and its own cases are written from it (config's
// volume_trim_test.go, "an age at the maximum" and "an age past the maximum"), because a
// maximum widened on one side alone would load cleanly and then be refused by every pod's
// trimmer, which then sweeps nothing until a daemon sends settings inside its ranges
// (shimwire.VolumeMinAgeMax).
func TestASweepAtTheGreatestMinimumAgeDeletesPastItAndKeepsInsideIt(t *testing.T) {
	// Seven days in minutes, which is shimwire.VolumeMinAgeMax written out, with a minute
	// either side of it.
	const (
		greatest = 7 * 24 * 60
		past     = (greatest + 1) * time.Minute
		inside   = (greatest - 1) * time.Minute
	)
	f := newTrimFixture(t, 1)
	// bun's install cache, whose entries are its immediate children: the minimum age is the
	// whole of their guard, with no allowance of Go's on top of it (goCacheMtimeSlack).
	root := f.roots[3]
	if root.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", root)
	}
	// addTree ages an entry by the fixture's own permutation, so each is retimed by hand to the
	// side of the cutoff the case puts it on, every path of it together — the entry's age is
	// the newest time anywhere in it.
	retime := func(entry *fixtureEntry, at time.Time) {
		t.Helper()
		for _, path := range []string{filepath.Join(entry.path, "b1", "out"), filepath.Join(entry.path, "b1"), entry.path} {
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
		entry.at = at
	}
	older := f.addTree(filepath.Join(root.Path, "tool-a-minute-past-the-week"), root.Entry)
	retime(older, f.clock.at.Add(-past))
	younger := f.addTree(filepath.Join(root.Path, "tool-a-minute-inside-the-week"), root.Entry)
	retime(younger, f.clock.at.Add(-inside))

	f.useVolume(steady(90))
	set := trimSettings()
	set.MinAgeMinutes = greatest
	f.setSettings(set)
	f.start(f.trimmer())

	f.tick("the sweep ended")

	if f.exists(older.path) {
		t.Errorf("the sweep kept %s, which read %s old under a minimum age of %d minutes: an entry past the age the settings asked for is the entry this trimmer exists to delete",
			older.path, f.clock.at.Sub(older.at), greatest)
	}
	if !f.exists(younger.path) {
		t.Errorf("the sweep deleted %s, which read %s old under a minimum age of %d minutes: an entry inside the age the settings asked for is one a role may still be using",
			younger.path, f.clock.at.Sub(younger.at), greatest)
	}
	f.assertProtected()
	f.assertPrefixed()
}

// An entry exactly the minimum age old is deleted, and one a second inside the age is kept. R3 has
// the trimmer delete "no entry whose newest modification time is inside the minimum age", so an
// entry whose newest time is the cutoff itself is outside it, and the listing keeps out only an
// entry newer than the cutoff (`at.After(eligible)`, list). The other cases stand a minute either
// side of an age (TestASweepAtTheGreatestMinimumAgeDeletesPastItAndKeepsInsideIt) or further, and a
// listing that kept out the entry at the cutoff itself is green with them. The trimmer's clock is
// moved onto a whole second first, so that the cutoff the sweep computes is a time a file on this
// volume holds exactly, and every time of both entries is read back before the sweep to show it
// stands where the case put it. The entries are bun's install cache's, which the minimum age
// alone guards, with no allowance of Go's on top of it (goCacheMtimeSlack), and the age is
// written out rather than read off trimSettings, as the greatest age's case writes its own.
func TestAnEntryExactlyTheMinimumAgeOldIsDeletedAndOneASecondInsideItIsKept(t *testing.T) {
	const minAge = 30 * time.Minute
	f := newTrimFixture(t, 0)
	root := f.roots[3]
	if root.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", root)
	}
	f.clock.advance(f.clock.at.Truncate(time.Second).Add(time.Second).Sub(f.clock.at))
	cutoff := f.clock.at.Add(-minAge)
	retime := func(entry *fixtureEntry, at time.Time) {
		t.Helper()
		for _, path := range []string{filepath.Join(entry.path, "b1", "out"), filepath.Join(entry.path, "b1"), entry.path} {
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(at) {
				t.Fatalf("%s reads as written at %s, want the %s the case wrote: the case would not stand where it says", path, info.ModTime(), at)
			}
		}
		entry.at = at
	}
	exactly := f.addTree(filepath.Join(root.Path, "tool-exactly-the-minimum-age"), root.Entry)
	retime(exactly, cutoff)
	inside := f.addTree(filepath.Join(root.Path, "tool-a-second-inside-the-minimum-age"), root.Entry)
	retime(inside, cutoff.Add(time.Second))

	f.useVolume(steady(90))
	set := trimSettings()
	set.MinAgeMinutes = int(minAge / time.Minute)
	f.setSettings(set)
	out := f.trimmer().sweep(context.Background(), *set, steady(90)(0))

	if f.exists(exactly.path) {
		t.Errorf("the sweep kept %s, whose newest time is exactly %s before the sweep began under a minimum age of %s: that entry is not inside the age, and R3 keeps only the entries that are (%s)",
			exactly.path, minAge, minAge, out.reason)
	}
	if !f.exists(inside.path) {
		t.Errorf("the sweep deleted %s, whose newest time is a second inside the minimum age of %s: an entry inside the age is one a role may still be using", inside.path, minAge)
	}
	if out.deletions != 1 {
		t.Errorf("the sweep deleted %d entries, want the one at the minimum age: %s", out.deletions, out.reason)
	}
	f.assertProtected()
}

// The age a minute past the greatest an operator may ask for is refused: the trimmer stays off,
// reads the volume not once, and the entry older than that age is still on the volume
// afterwards.
//
// 10081 is written out — seven days and a minute — rather than read off
// shimwire.VolumeMinAgeMax + 1, because a case staged from the constant moves with it: raised,
// the constant takes the case's value up with it and the case goes on passing. This is the
// maximum held from above, as the case before it holds it from below, and nothing held it: the
// one other case past the maximum sends 2147483647
// (TestAnAgeWhoseCutoffWouldOverflowNeverSweepsAnEntryWrittenMomentsAgo) and every other value
// in this package is derived from the constant, so with shimwire.VolumeMinAgeMax raised to
// 200000000 — past 153,722,868, where the sweep's cutoff wraps into the future and every entry
// of every root becomes eligible however recently it was written (list; INFRA-2009, review
// round 5) — the whole package was green (measured on 2026-11-12; INFRA-2009, review rounds 7
// and 8).
//
// The loader bounds the same key by the same constant (config's readVolumeTrim, and "an age
// past the maximum" in its volume_trim_test.go), so one value is refused on both sides of the
// wire: a maximum widened on one side alone would load cleanly and then be refused by every
// pod's trimmer, which then sweeps nothing until a daemon sends settings inside its ranges.
func TestAnAgeAMinutePastTheGreatestIsRefusedAndSweepsNothing(t *testing.T) {
	// Seven days and a minute: shimwire.VolumeMinAgeMax + 1 written out.
	const pastGreatest = 10081
	f := newTrimFixture(t, 2)
	// At the high mark and never falling, so a trimmer that ran by these settings would sweep
	// and would go on deleting until nothing eligible was left.
	f.useVolume(steady(99))

	// The entry such a sweep would take: older than the age the settings ask for, in bun's
	// install cache, whose entries are its immediate children. It is retimed by hand, every
	// path of it together, because addTree ages an entry by the fixture's own permutation —
	// the entry's age is the newest time anywhere in it.
	root := f.roots[3]
	stale := f.addTree(filepath.Join(root.Path, "tool-past-the-week"), root.Entry)
	aged := f.clock.at.Add(-(pastGreatest + 1) * time.Minute)
	for _, path := range []string{filepath.Join(stale.path, "b1", "out"), filepath.Join(stale.path, "b1"), stale.path} {
		if err := os.Chtimes(path, aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	stale.at = aged

	// What the case rests on, counted before the trimmer runs: the entries a sweep would list,
	// and the staged one past the age these settings name.
	if len(f.entries) == 0 || !f.exists(stale.path) {
		t.Fatalf("the fixture holds %d entries and the staged one at %s is there: %v — the case would read a behaviour nothing staged", len(f.entries), stale.path, f.exists(stale.path))
	}
	if age := f.clock.at.Sub(stale.at); age <= pastGreatest*time.Minute {
		t.Fatalf("the staged entry reads %s old, want more than the %d minutes these settings ask for: a sweep running by them must have it to delete", age, pastGreatest)
	}

	// The refusal itself, asked of the value and not of the constant.
	refusals := refusedTrimSettings(shimwire.LauncherVolumeTrim{HighPercent: 85, LowPercent: 70, MinAgeMinutes: pastGreatest, IntervalSeconds: 30})
	if !slices.ContainsFunc(refusals, func(refusal string) bool { return strings.Contains(refusal, "min_age_minutes is 10081") }) {
		t.Errorf("refusedTrimSettings answered %v for a minimum age of %d minutes, want the key named with its value: seven days is the greatest age an operator may ask for", refusals, pastGreatest)
	}

	set := trimSettings()
	set.MinAgeMinutes = pastGreatest
	f.setSettings(set)
	f.start(f.trimmer())

	// The trimmer is given every chance to sweep: a tick is offered for as long as it has an
	// interval clock to take one on. A trimmer that refuses these settings makes no such clock
	// — only the one it asks for settings on again — so nothing is offered and the wait ends
	// at the refusal instead.
	deadline := time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second))
	for time.Now().Before(deadline) && f.logged("stays off") == 0 && f.logged("the sweep ended") == 0 {
		if at, running := f.ticks.live(settingsInterval(*set)); running {
			f.ticks.offer(t, at, 200*time.Millisecond)
			continue
		}
		time.Sleep(testwait.Poll)
	}

	if !f.exists(stale.path) {
		t.Errorf("the trimmer deleted %s under a minimum age of %d minutes, which is past the greatest an operator may ask for: settings outside the trimmer's own ranges are refused and the trimmer stays off", stale.path, pastGreatest)
	}
	if gone := f.gone(); len(gone) > 0 {
		t.Errorf("the trimmer deleted %d of the %d entries under an age it must refuse: %v", len(gone), len(f.entries), gone)
	}
	if f.logged(fmt.Sprintf("min_age_minutes is %d", pastGreatest)) == 0 {
		t.Errorf("the trimmer refused the settings without naming the key and the value; it wrote %q", f.linesWritten())
	}
	if f.logged("stays off") == 0 {
		t.Errorf("the trimmer did not refuse the settings and stay off; it wrote %q", f.linesWritten())
	}
	if reads := f.readCount(); reads != 0 {
		t.Errorf("the trimmer read the volume %d times on refused settings, want none", reads)
	}
	f.assertProtected()
	f.assertPrefixed()
}

// A refusal the trimmer is still in is written once. Settings outside the ranges are a daemon's
// standing answer and not an event — a daemon that widened a maximum this launcher does not
// know sends them with every acknowledgement, and the trimmer they stopped reads them again
// every volumeSettingsWait while it waits (awaitSettings) — so a line per poll would put two or
// more lines every ten seconds into the roles container's log for the life of the pod, and that
// log is the only place a person sees this work. A refusal whose text changes is news, and so is
// the same text after a spell of settings the trimmer could run: the pod was not in that state a
// moment ago.
func TestARefusalThatDoesNotChangeIsLoggedOnce(t *testing.T) {
	f := newTrimFixture(t, 1)
	f.useVolume(steady(50)) // below the high mark: every interval reads the volume and sweeps nothing
	f.setSettings(trimSettings())
	f.start(f.trimmer())
	f.tick("nothing to sweep")

	// A daemon of another version widens the high mark past the range this launcher knows. Its
	// interval is the one already in force, so the clock the case ticks does not move under it.
	wide := *trimSettings()
	wide.HighPercent = shimwire.VolumeMarkMax + 1
	refusedHigh := fmt.Sprintf("high_percent is %d", wide.HighPercent)
	f.setSettings(&wide)
	f.tick("it starts no ticker on refused settings")
	const polls = 4
	for range polls {
		offerSettingsTick(t, f)
	}
	switch {
	case f.logged(refusedHigh) != 1:
		t.Errorf("the trimmer wrote %d lines refusing the same settings over %d polls, want the one a standing state gets; it wrote %q",
			f.logged(refusedHigh), polls, f.linesWritten())
	case f.logged("it starts no ticker on refused settings") != 1:
		t.Errorf("the trimmer wrote %d of the refusal's trailing lines, want one", f.logged("it starts no ticker on refused settings"))
	}

	// A refusal whose text changes is news: the second reason is said, and the first with it.
	wider := wide
	wider.MinAgeMinutes = 0
	f.setSettings(&wider)
	offerSettingsTick(t, f)
	testwait.Eventually(t, "the trimmer to say the refusal whose text changed", func() bool {
		return f.logged("min_age_minutes is 0") == 1
	})
	if said := f.logged(refusedHigh); said != 2 {
		t.Errorf("the first reason was written %d times, want it said again as part of the refusal it is now half of", said)
	}

	// Settings the trimmer can run clear what it remembers, so the first refusal is news again.
	f.setSettings(trimSettings())
	offerSettingsTicksUntil(t, f, "the trimmer to start again on settings it can run", func() bool {
		return f.logged(volumeTrimLog+"started: trimmer") == 2
	})
	f.tick("nothing to sweep")
	f.setSettings(&wide)
	f.tick("it starts no ticker on refused settings")
	if said := f.logged(refusedHigh); said != 3 {
		t.Errorf("the refusal that came back after settings the trimmer ran by was written %d times in all, want three: a state the pod has left is news when it returns", said)
	}
	// Only the two intervals that ran by settings the trimmer can run read the volume: a refusal
	// stops the trimmer at the interval that meets it (TestARefusedAnswerSwitchesARunningTrimmerOff).
	if reads := f.readCount(); reads != 2 {
		t.Errorf("the trimmer read the volume %d times, want the 2 intervals that ran by settings it can run", reads)
	}
	f.assertPrefixed()
}

// R2: "The trimmer holds the same ranges a second time, on its own side of the wire: it refuses
// a value outside them, logs the key and the value, and stays off rather than starting its
// ticker", and "the settings in force are that connection's latest answer alone". An answer it
// refuses that arrives while it runs is that latest answer, so the trimmer stops as it does on a
// first answer it refuses: it reads and sweeps nothing more, makes no interval clock, and waits
// on the clock it asks for settings on until an answer carries settings it can run.
//
// A report it was holding is tried rather than discarded, since only an answer with the member
// absent discards one and the daemon that sent these settings has a case for a report
// (awaitSettings); an answer with no settings after that discards it.
func TestARefusedAnswerSwitchesARunningTrimmerOff(t *testing.T) {
	wide := *trimSettings()
	wide.HighPercent = shimwire.VolumeMarkMax + 1
	refusedHigh := fmt.Sprintf("high_percent is %d", wide.HighPercent)

	t.Run("it stays off and names the key and the value", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.useVolume(steady(50)) // below the high mark: the interval reads the volume and sweeps nothing
		f.setSettings(trimSettings())
		f.start(f.trimmer())
		f.tick("nothing to sweep")

		// The volume is at the high mark from here, so any interval that read it would sweep.
		f.useVolume(steady(90))
		f.setSettings(&wide)
		f.tick("it starts no ticker on refused settings")
		testwait.Eventually(t, "the interval clock to be stopped", func() bool {
			_, running := f.ticks.live(f.interval())
			return !running
		})
		for range 3 {
			offerSettingsTick(t, f)
		}
		switch {
		case f.logged(refusedHigh) != 1:
			t.Errorf("the trimmer wrote %d lines naming %q, want the one refusal; it wrote %q", f.logged(refusedHigh), refusedHigh, f.linesWritten())
		case f.readCount() != 1:
			t.Errorf("the trimmer read the volume %d times, want only the interval before the refusal: it must stay off on settings it refuses", f.readCount())
		case len(f.gone()) != 0:
			t.Errorf("%d entries were deleted after the trimmer refused its settings", len(f.gone()))
		case f.logged("sweeping") != 0:
			t.Errorf("the trimmer swept %d times after the refusal, want none", f.logged("sweeping"))
		}
		f.assertProtected()
		f.assertPrefixed()
	})

	t.Run("a report it was holding is sent on the connection that answered", func(t *testing.T) {
		f := newTrimFixture(t, 25) // a hundred entries, ten points: the sweep ends above the low mark and reports
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		f.start(f.trimmer())
		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})

		f.refuseSends(nil)
		f.setSettings(&wide)
		f.tick("it starts no ticker on refused settings")
		testwait.Eventually(t, "the held report to reach the daemon", func() bool { return len(f.reports()) == 1 })
		if report := f.reports()[0]; report.UsedBytes != int64(percentBytes(80)) {
			t.Errorf("the report sent carries %d used bytes, want the %d of the sweep that was held", report.UsedBytes, percentBytes(80))
		}
	})

	t.Run("an answer with no settings afterwards discards a report it still holds", func(t *testing.T) {
		f := newTrimFixture(t, 25)
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		f.start(f.trimmer())
		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report the outage refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})

		// The send fails again at the refusal, so the report is still held while the trimmer is off.
		f.setSettings(&wide)
		f.tick("it starts no ticker on refused settings")
		testwait.Eventually(t, "the trimmer to try the held report at the refusal", func() bool {
			return f.logged("the held report could not be sent either") == 1
		})

		f.setSettings(nil)
		offerSettingsTick(t, f)
		testwait.Eventually(t, "the trimmer to wait on an answer that carries no settings", func() bool {
			return f.logged("no settings are in force, so the trimmer stays off") == 1
		})

		// Settings it can run come back on a connection that takes reports: the interval that
		// starts tries any held report first (tick's flush), so a report still held is sent here.
		f.refuseSends(nil)
		f.setSettings(trimSettings())
		offerSettingsTicksUntil(t, f, "the trimmer to start again on settings it can run", func() bool {
			return f.logged(volumeTrimLog+"started: trimmer") == 2
		})
		f.tick("nothing to sweep")
		if sent := f.reports(); len(sent) != 0 {
			t.Errorf("the trimmer sent %d reports after an answer with no settings, want the held one discarded: %+v", len(sent), sent)
		}
	})
}

// A volume this process cannot read is said once, as a refused setting is, and said again when
// it comes back after a reading that worked.
//
// The case above covers the refusal half of that rule and nothing covers this half: measured by
// the tester at 37dc8da0f4830719bd0a6a150b3f31c4557610a9, a copy whose statfs error is written
// on every interval left this whole package green over two runs of its own. The trimmer reads
// the volume once an interval for the life of the pod, so an unreadable volume without this
// memory is one line a minute for as long as the fault lasts, in the roles container's log six
// agents and an operator read — which is the cost review round 2 named for the refusal.
//
// The intervals are driven one at a time rather than through the running loop, because an
// interval that writes no line is exactly what this case asserts and the fixture's tick waits
// for one.
func TestAVolumeTheTrimmerCannotReadIsSaidOnce(t *testing.T) {
	f := newTrimFixture(t, 1)
	f.useVolume(steady(50)) // below the high mark: an interval that reads sweeps nothing
	f.setSettings(trimSettings())

	// The reader the case drives, over the fixture's own: it answers the same statfs error
	// while failing is set, and the fixture's reading when it is not.
	working := readVolumeUsage
	var mu sync.Mutex
	failing := true
	readVolumeUsage = func(path string) (volumeUsage, error) {
		mu.Lock()
		bad := failing
		mu.Unlock()
		if bad {
			return volumeUsage{}, errors.New("launcher: statfs " + path + ": input/output error")
		}
		return working(path)
	}
	t.Cleanup(func() { readVolumeUsage = working })
	fails := func(bad bool) {
		mu.Lock()
		failing = bad
		mu.Unlock()
	}

	tr := f.trimmer()
	set := *trimSettings()
	const unreadable = "could not be read"
	for at := 1; at <= 3; at++ {
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval %d ended the trimmer (%v), want a volume it cannot read to be no ending at all", at, ending)
		}
		if said := f.logged(unreadable); said != 1 {
			t.Fatalf("the trimmer wrote %d lines about a volume it cannot read over %d intervals, want the one a standing state gets; it wrote %q",
				said, at, f.linesWritten())
		}
	}

	// A reading that works is the end of that state, so the same error after it is news again.
	fails(false)
	tr.tick(context.Background(), &set)
	fails(true)
	tr.tick(context.Background(), &set)
	if said := f.logged(unreadable); said != 2 {
		t.Errorf("the error that came back after a reading that worked was written %d times in all, want two: a state the pod has left is news when it returns; it wrote %q",
			said, f.linesWritten())
	}
	f.assertPrefixed()
}

// A reading below the high mark sweeps nothing: no entry is deleted, and no file and no
// modification time anywhere on the volume is changed. The trimmer is a process with root's
// reach over six roles' caches, so the case that must hold hardest is the one where it does
// nothing at all.
func TestAReadingBelowTheHighMarkSweepsNothing(t *testing.T) {
	f := newTrimFixture(t, 6)
	f.useVolume(steady(84.9))
	f.setSettings(trimSettings())
	before := treeSnapshot(t, f.root)
	if len(before) == 0 {
		t.Fatal("the fixture built no file at all")
	}

	tr := f.trimmer()
	f.start(tr)
	f.tick("nothing to sweep")

	if reads := f.readCount(); reads != 1 {
		t.Errorf("the trimmer read the volume %d times in one interval below the high mark, want exactly 1", reads)
	}
	if gone := f.gone(); len(gone) > 0 {
		t.Errorf("the trimmer deleted %d entries below the high mark: %v", len(gone), gone)
	}
	assertTreeUnchanged(t, f.root, before)
	f.assertPrefixed()
	if f.logged("84.9% full") == 0 {
		t.Errorf("the trimmer logged no reading; it wrote %q", f.linesWritten())
	}
}

// treeSnapshot is every path under root with what it holds and when it was last written.
func treeSnapshot(t *testing.T, root string) map[string]fixtureFile {
	t.Helper()
	out := map[string]fixtureFile{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := fixtureFile{path: path, at: info.ModTime()}
		if info.Mode().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file.content = content
		}
		out[path] = file
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]fixtureFile) {
	t.Helper()
	after := treeSnapshot(t, root)
	for path, was := range before {
		is, there := after[path]
		switch {
		case !there:
			t.Errorf("%s is gone, want the whole tree left alone", path)
		case !is.at.Equal(was.at):
			t.Errorf("%s was last written at %s, want the %s it held: the trimmer must not write to what it does not delete", path, is.at, was.at)
		case string(is.content) != string(was.content):
			t.Errorf("%s holds %q, want %q", path, is.content, was.content)
		}
	}
	for path := range after {
		if _, there := before[path]; !there {
			t.Errorf("%s was made under the volume, want the trimmer to write nothing", path)
		}
	}
}

// assertProtected is the guard every sweeping case runs: every file under a protected path is
// still there, with the content and the modification time it was written with. Presence alone
// would not show it — the requirement is that the trimmer never writes to a role's work — so
// the case reads each one back.
func (f *trimFixture) assertProtected() {
	f.t.Helper()
	if len(f.protected) != fixtureKeptFiles {
		f.t.Fatalf("the fixture holds %d protected files, want the %d this case counts: %v", len(f.protected), fixtureKeptFiles, f.protected)
	}
	for _, file := range f.protected {
		info, err := os.Lstat(file.path)
		if err != nil {
			f.t.Errorf("the protected file %s: %v", file.path, err)
			continue
		}
		if !info.ModTime().Equal(file.at) {
			f.t.Errorf("the protected file %s was last written at %s, want the %s it was made with", file.path, info.ModTime(), file.at)
		}
		content, err := os.ReadFile(file.path)
		if err != nil {
			f.t.Errorf("the protected file %s: %v", file.path, err)
			continue
		}
		if string(content) != string(file.content) {
			f.t.Errorf("the protected file %s holds %q, want %q", file.path, content, file.content)
		}
	}
	for _, root := range f.roots {
		if !f.exists(root.Path) {
			f.t.Errorf("the reclaimable root %s was unlinked, want the trimmer to delete only entries inside it: a role's tool holds its flock on the root's own inode", root.Path)
		}
	}
}

// The sweep deletes the oldest entries first, across every root rather than root by root, until
// a fresh reading of the volume is at or below the low mark. The volume is modelled by the
// entries still on it, so the sweep stops where a real volume would, and the reader's call count
// holds the hundred-deletion cadence the rule names.
//
// The case runs with an empty environment, because the four roots and the protected set are
// written-out constants and not reads of TMPDIR, GOCACHE, XDG_CACHE_HOME or
// BUN_INSTALL_CACHE_DIR: a roles container's own environment holds the pod's uid and nothing
// else, so a trimmer that read them would sweep nothing at all in the pod it was written for.
//
// Its negative control is the same tree swept by name: the entries a trimmer ordering by name
// would delete are a different set, and the case shows the ones it would wrongly have taken are
// still there.
//
// The trimmer is built after the environment is emptied, and the roots it sweeps are the ones
// newTrimmer built then (trimFixture.trimmer), held here to the written-out constant before the
// sweep reads them: a constructor that read TMPDIR or GOCACHE would build a different list under
// the empty environment, and the sweep below would run on it.
func TestTheSweepDeletesOldestFirstAcrossEveryRootUntilTheLowMark(t *testing.T) {
	const perRoot = 80
	f := newTrimFixture(t, perRoot)
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	withEmptyEnvironment(t)

	if len(f.entries) != 4*perRoot {
		t.Fatalf("the fixture made %d entries, want %d: four roots of %d", len(f.entries), 4*perRoot, perRoot)
	}
	tr := f.trimmer()
	sameRoot := func(a, b ReclaimableRoot) bool {
		return a.Path == b.Path && a.Entry == b.Entry && slices.Equal(a.Except, b.Except)
	}
	if !slices.EqualFunc(tr.roots, f.roots, sameRoot) {
		t.Fatalf("the trimmer built under an empty environment sweeps the roots %+v, want the written-out ReclaimableRoots %+v on this case's tree: its roots are a read of the environment",
			tr.roots, f.roots)
	}
	out := tr.sweep(context.Background(), *trimSettings(), f.modelled(90)(0))

	// 90 per cent, with each entry a tenth of a point: the low mark of 70 is 200 entries away,
	// and the sweep reads the volume every hundred deletions, so it stops on the reading after
	// the two hundredth.
	if out.deletions != 200 {
		t.Errorf("the sweep deleted %d entries, want the 200 that take the modelled volume from 90%% to the low mark of 70%%: %s", out.deletions, out.reason)
	}
	if !strings.Contains(out.reason, "low mark") {
		t.Errorf("the sweep stopped because %q, want the low mark", out.reason)
	}
	if out.report {
		t.Error("the sweep that reached the low mark asks for a report, want none: the daemon hears only about a sweep that could not get there")
	}
	if reads := f.readCount(); reads != 2 {
		t.Errorf("the sweep read the volume %d times, want 2: one after each hundred deletions, with the interval's own reading passed into it", reads)
	}

	oldest := f.oldestFirst()
	gone := f.gone()
	if len(gone) != 200 {
		t.Fatalf("%d entries are gone, want the 200 the sweep counted", len(gone))
	}
	for at, entry := range oldest {
		switch {
		case at < 200 && f.exists(entry.path):
			t.Errorf("the %dth oldest entry %s (written %s) is still there, want the oldest 200 deleted", at, entry.path, entry.at)
		case at >= 200 && !f.exists(entry.path):
			t.Errorf("the %dth oldest entry %s (written %s) is gone, want the sweep to stop at the low mark", at, entry.path, entry.at)
		}
	}
	// Oldest first across the roots and not root by root: the first two hundred by age come
	// from all four roots, so a sweep that emptied one root before starting the next would have
	// deleted a different set.
	roots := map[string]int{}
	for _, entry := range oldest[:200] {
		for _, root := range f.roots {
			if strings.HasPrefix(entry.path, root.Path+"/") {
				roots[root.Path]++
			}
		}
	}
	if len(roots) != 4 {
		t.Errorf("the 200 oldest entries come from %d roots (%v), want all four: the case cannot tell a sweep across the roots from one root at a time", len(roots), roots)
	}

	// The negative control: by name, the sweep would have taken a different 200.
	byName := f.byName()[:200]
	wrongly := 0
	for _, entry := range byName {
		if f.exists(entry.path) {
			wrongly++
		}
	}
	if wrongly == 0 {
		t.Fatal("every entry a name-ordered sweep would have deleted is gone, so this case cannot tell the two orders apart")
	}
	t.Logf("the control: a sweep ordering by name would have deleted %d entries this one left alone", wrongly)

	f.assertProtected()
}

// withEmptyEnvironment runs the rest of the case with nothing in the process's environment, and
// puts it back afterwards.
func withEmptyEnvironment(t *testing.T) {
	t.Helper()
	was := os.Environ()
	os.Clearenv()
	t.Cleanup(func() {
		os.Clearenv()
		for _, entry := range was {
			name, value, _ := strings.Cut(entry, "=")
			if err := os.Setenv(name, value); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// An entry's age is the newest modification time anywhere in it: a directory whose own time is a
// month old and which holds one file written a minute ago is fresh, and the sweep leaves it. A
// `go-build` tree's own time changes only when its entries are added or removed, so a trimmer
// reading that time would delete the cache a live build is writing into.
func TestAnEntryIsAsFreshAsTheNewestFileAnywhereInIt(t *testing.T) {
	f := newTrimFixture(t, 4)
	ancient := f.clock.at.Add(-30 * 24 * time.Hour)
	tree := filepath.Join(f.roots[0].Path, "go-build999999999")
	inside := filepath.Join(tree, "b1")
	mkdirFixture(t, inside)
	writeAged(t, filepath.Join(inside, "out"), f.clock.at.Add(-time.Minute))
	for _, dir := range []string{inside, tree} {
		if err := os.Chtimes(dir, ancient, ancient); err != nil {
			t.Fatal(err)
		}
	}
	// The control: by its own modification time this tree is the oldest thing on the volume, so
	// a trimmer reading that time would delete it first of all.
	for _, entry := range f.entries {
		if !entry.at.After(ancient) {
			t.Fatalf("the fixture holds an entry %s as old as the trap, so the case proves nothing", entry.path)
		}
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if !f.exists(tree) {
		t.Errorf("the trimmer deleted %s, whose own time is %s but which holds a file written a minute ago: an entry is as fresh as the newest file anywhere in it", tree, ancient)
	}
	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d entries, want the %d eligible ones: %s", out.deletions, len(f.entries), out.reason)
	}
	f.assertProtected()
}

// Of $TMPDIR the sweep deletes only what Go names a build's working directory, `go-build` and
// one or more decimal digits (isGoBuildTree): a name that only begins `go-build` is a role's own
// work, and nothing automatic deletes a role's own work. The case stands a role's
// `go-build-repro`, `go-builder`, a bare `go-build` and a `go-build.log`, every one a month old,
// beside a real `go-build123` two hours old, which is the control and goes. The negative control
// is the rule as it stood at b7085e21, `strings.HasPrefix(name, "go-build")`, which deleted the
// three directories and passed the file over with a line naming it as an entry of the wrong kind.
func TestOnlyANameGoGivesABuildTreeIsAnEntryOfTMPDIR(t *testing.T) {
	f := newTrimFixture(t, 0)
	tmp := f.roots[0]
	if tmp.Entry != GoBuildTrees {
		t.Fatalf("the first reclaimable root is %+v, want $TMPDIR", tmp)
	}
	built := f.addTree(filepath.Join(tmp.Path, "go-build123"), tmp.Entry)
	month := f.clock.at.Add(-30 * 24 * time.Hour)
	var own []string
	for _, name := range []string{"go-build-repro", "go-builder", "go-build"} {
		path := filepath.Join(tmp.Path, name)
		agedUnder(t, tmp.Path, filepath.Join(path, "work", "notes.md"), month)
		own = append(own, path)
	}
	logFile := filepath.Join(tmp.Path, "go-build.log")
	writeAged(t, logFile, month)
	own = append(own, logFile)
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	out := f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

	if f.exists(built.path) {
		t.Errorf("the sweep left %s, Go's own build tree two hours old, so the case shows nothing: %s", built.path, out.reason)
	}
	for _, path := range own {
		if !f.exists(path) {
			t.Errorf("the sweep deleted %s, a role's own name that only begins go-build", path)
		}
		if said := f.logged(filepath.Base(path) + `"`); said != 0 {
			t.Errorf("the sweep wrote %d lines naming %s, want none: it is no entry of $TMPDIR at all; it wrote %q", said, path, f.linesWritten())
		}
	}
	if out.deletions != 1 {
		t.Errorf("the sweep deleted %d entries, want the one build tree: %s", out.deletions, out.reason)
	}
	f.assertProtected()
}

// The used-path set the removal acts on is never older than volumeUsedPathsMaxAge. The sweep
// asks the set of an entry at the entry's boundary, then re-reads the whole entry's tree before
// the delete (newerThan), and that re-read takes as long as the tree is large: so the set is
// asked again immediately before the removal and read again first once the re-read has taken it
// past a second. The case moves the trimmer's clock a second and a millisecond inside the
// re-read of one late directory entry, at its first directory, and a process takes hold of that
// entry at the same moment; the set asked at the boundary was fresh and named no such process.
// The entry must stand, and every other entry go. The control is the same sweep with no process
// appearing, where that entry goes. The negative control is the sweep as it stood at b7085e21,
// which asked the set at the boundary alone and deleted the entry.
func TestTheUsedPathSetIsAskedAgainAfterAPreDeleteReReadThatTookItPastASecond(t *testing.T) {
	sweep := func(t *testing.T, appears bool) (survived bool, deletions, entries int) {
		f := newTrimFixture(t, 4)
		order := f.oldestFirst()
		late := -1
		for at := len(order) - 1; at >= 0; at-- {
			if info, err := os.Lstat(order[at].path); err == nil && info.IsDir() {
				late = at
				break
			}
		}
		if late <= 0 {
			t.Fatal("the fixture holds no late directory entry for the process to take hold of")
		}
		entry, before := order[late], order[:late]
		// The trimmer's clock is read three times from the moment every entry before this one
		// is gone: the boundary's halted, the boundary's question of the set's age, which finds
		// it fresh, and the halt before the first directory the entry's re-read opens.
		reads := 0
		f.clock.setHook(func(clock *fakeClock) {
			if reads == 0 && (!f.exists(entry.path) || slices.ContainsFunc(before, func(e *fixtureEntry) bool { return f.exists(e.path) })) {
				return
			}
			reads++
			if reads != 3 {
				return
			}
			if appears {
				placeProcess(t, f.proc, 4343, filepath.Join(entry.path, "b1"))
			}
			clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
		})
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		out := f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))
		if reads < 3 {
			t.Fatalf("the case never reached the entry's re-read, having seen %d readings of the clock there: %s", reads, out.reason)
		}
		return f.exists(entry.path), out.deletions, len(f.entries)
	}

	survived, deletions, entries := sweep(t, true)
	if !survived {
		t.Error("the sweep deleted the entry a process took hold of during its re-read, more than a second after the set was read: the removal acted on a stale set")
	}
	if deletions != entries-1 {
		t.Errorf("the sweep deleted %d entries, want the %d that nothing holds", deletions, entries-1)
	}
	t.Run("the control, with no process appearing", func(t *testing.T) {
		survived, deletions, entries := sweep(t, false)
		if survived || deletions != entries {
			t.Errorf("the control sweep left the entry %t and deleted %d of %d entries, want every one deleted", survived, deletions, entries)
		}
	})
}

// The Go build cache rule reaches the names ending `-a` and `-d` inside a shard directory and
// nothing else: the cache's own trim.txt and README sit in the cache root rather than in a
// shard, and a name with neither suffix is not an entry whatever its age. A `-d` that is a
// directory is removed whole, as Go's own trim removes it (trimSubdir, cmd/go/internal/cache).
func TestTheGoCacheRuleReachesOnlyTheSuffixedNamesInsideAShard(t *testing.T) {
	const perRoot = 40
	f := newTrimFixture(t, perRoot)
	root := f.roots[1]
	if root.Entry != GoCacheEntries {
		t.Fatalf("the second reclaimable root is %+v, want the Go build cache", root)
	}
	// What the rule must never reach, counted before the sweep.
	nonEntries := []string{
		filepath.Join(root.Path, "trim.txt"),
		filepath.Join(root.Path, "README"),
		filepath.Join(root.Path, "00", "0123456789abcdef0123456789abcdef"),
	}
	for _, path := range nonEntries {
		if !f.exists(path) {
			t.Fatalf("the fixture is missing %s, which the rule must pass over", path)
		}
	}
	if len(nonEntries) != 3 {
		t.Fatalf("the case counted %d names the rule must not reach, want 3", len(nonEntries))
	}
	directories := 0
	for _, entry := range f.entries {
		if entry.rule != GoCacheEntries {
			continue
		}
		if info, err := os.Lstat(entry.path); err == nil && info.IsDir() {
			directories++
		}
	}
	if directories == 0 {
		t.Fatal("the fixture holds no directory-form cache entry, so the case cannot show one removed whole")
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d entries, want every one of the %d the fixture made: %s", out.deletions, len(f.entries), out.reason)
	}
	for _, path := range nonEntries {
		if !f.exists(path) {
			t.Errorf("%s is gone, want a name that is not in a shard or does not end -a or -d to be no entry at all", path)
		}
	}
	for _, entry := range f.entries {
		if entry.rule == GoCacheEntries && f.exists(entry.path) {
			t.Errorf("the cache entry %s is still there, want every shard name ending -a or -d deleted, the directory form whole", entry.path)
		}
	}
	f.assertProtected()
}

// An entry of the Go build cache is listed only once it is older than the minimum age and Go's
// mtime hour together (goCacheMtimeSlack): Go moves a cache entry's modification time on use at
// most once an hour, so one that reads thirty minutes old may have been used a minute ago, and
// a build that holds it through its importcfg has no handle a /proc read can see. A thirty
// minute old entry therefore survives a sweep whose minimum age is fifteen.
//
// The hour itself is held from both sides, by two entries of the same cache built the same way
// under the same shard: one fifty-nine minutes past the minimum age, which survives, and one a
// second past the minimum age and the hour together, which the same sweep deletes. That pair
// is what pins the allowance to Go's own mtimeInterval — `mtimeInterval = 1 * time.Hour`, the
// interval markUsed bumps an entry's time on at most once and the same one Go's own trim
// subtracts from its cutoff (cmd/go/internal/cache/cache.go:348, :352-372 and :427-429, read
// in this pod's own go1.27.3) — without comparing two constants of this repository, which no
// Go release can move: with the thirty minutes alone this case passed with the slack anywhere
// from fifteen minutes to sixty-one, and with an entry a minute rather than a second past the
// hour it passed with the slack at sixty-one too, measured against this very case (INFRA-2009,
// review round 6). Without the stale one the case would pass on a sweep that deleted nothing
// at all, and both counts are asserted non-zero before either behaviour is read.
//
// Its second control is an entry of another root at the same thirty minutes, which the same
// sweep must delete: that is what holds the hour to the Go build cache alone. Every age below
// is a literal and none is read off goCacheMtimeSlack, because a case staged from the constant
// it tests moves with it — with the constant at two hours, and with the hour given to every
// rule, the earlier form of this case and the whole of this package passed, since every entry
// any fixture stages is at least two hours old (ageOf; INFRA-2009, review round 5).
func TestAGoCacheEntryInsideGosMtimeHourSurvivesAndAnOlderOneIsDeleted(t *testing.T) {
	const (
		minAge = 15 * time.Minute
		// Past the minimum age and well inside Go's hour: the Go cache entry at this age is
		// the one Go's allowance keeps, and an entry of any other root at it is not.
		recent = 30 * time.Minute
		// Past the minimum age by fifty-nine minutes, which is a minute inside Go's hour: an
		// entry Go marked used a minute before the sweep reads exactly this old.
		almost = minAge + 59*time.Minute
		// Past the minimum age and Go's hour together, by a second: an entry Go could have
		// marked used no later than a second before the hour began is past the allowance, and
		// a slack of an hour and a minute would keep it.
		stale = minAge + time.Hour + time.Second
	)
	f := newTrimFixture(t, 4)
	cache := f.roots[1]
	if cache.Entry != GoCacheEntries {
		t.Fatalf("the second reclaimable root is %+v, want the Go build cache", cache)
	}
	// Both are the directory form of a cache entry inside a shard directory, as the fixture's
	// own `-d` entries are (buildRoots): one rule reaches them both, and only their times
	// differ. addTree ages a tree by the fixture's permutation, so each is retimed by hand to
	// the side of the guard the case puts it on, every path of it together — the entry's age is
	// the newest time anywhere in it.
	shard := filepath.Join(cache.Path, "7f")
	mkdirFixture(t, shard)
	retime := func(entry *fixtureEntry, at time.Time) {
		t.Helper()
		for _, path := range []string{filepath.Join(entry.path, "b1", "out"), filepath.Join(entry.path, "b1"), entry.path} {
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
		entry.at = at
	}
	retime(f.addTree(filepath.Join(shard, strings.Repeat("c", 32)+"-d"), GoCacheEntries), f.clock.at.Add(-recent))
	// The two that hold Go's hour itself, a minute inside it and a minute past it.
	edge := f.addTree(filepath.Join(shard, strings.Repeat("e", 32)+"-d"), GoCacheEntries)
	retime(edge, f.clock.at.Add(-almost))
	retime(f.addTree(filepath.Join(shard, strings.Repeat("d", 32)+"-d"), GoCacheEntries), f.clock.at.Add(-stale))
	// The entry of another root at the same thirty minutes: an immediate child of bun's
	// install cache, which a `bun install` refills from the registry and whose times no tool
	// writes Go's way, so the minimum age is the whole of its guard.
	other := f.roots[3]
	if other.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", other)
	}
	elsewhere := f.addTree(filepath.Join(other.Path, "tool-thirty-minutes-old"), other.Entry)
	retime(elsewhere, f.clock.at.Add(-recent))

	// What each half of the case rests on, counted before the sweep runs and by the ages this
	// case staged rather than by the constant under test: the Go cache entries inside Go's
	// hour, which must survive, and those past the minimum age and that hour together, which
	// must go. The fixture's own cache entries are at least two hours old (ageOf), so they
	// count with the control. The entry at the hour's own edge is in neither count and is read
	// on its own below, which is where this case says where that boundary falls.
	var inside, past []*fixtureEntry
	for _, entry := range f.entries {
		if entry.rule != GoCacheEntries {
			continue
		}
		switch age := f.clock.at.Sub(entry.at); {
		case age > minAge && age < time.Hour:
			inside = append(inside, entry)
		case age >= stale:
			past = append(past, entry)
		}
	}
	if len(inside) == 0 || len(past) == 0 {
		t.Fatalf("the fixture holds %d Go cache entries inside Go's hour and %d past it, want both: the case would read a behaviour nothing staged", len(inside), len(past))
	}
	for _, entry := range slices.Concat(inside, past, []*fixtureEntry{elsewhere, edge}) {
		if !f.exists(entry.path) {
			t.Fatalf("%s is not on the volume before the sweep, so the case shows nothing about it", entry.path)
		}
	}

	set := trimSettings()
	set.MinAgeMinutes = int(minAge / time.Minute)
	f.useVolume(steady(90))
	f.setSettings(set)
	tr := f.trimmer()

	out := tr.sweep(context.Background(), *set, steady(90)(0))

	if out.deletions == 0 {
		t.Fatalf("the sweep deleted nothing at all, so no half of the case was reached: %s", out.reason)
	}
	for _, entry := range inside {
		if !f.exists(entry.path) {
			t.Errorf("the sweep deleted the Go cache entry %s, which read %s old under a %s minimum age: Go moves a cache entry's mtime on use at most once an hour, so a build may have been reading it a minute ago",
				entry.path, f.clock.at.Sub(entry.at).Round(time.Minute), minAge)
		}
	}
	if !f.exists(edge.path) {
		t.Errorf("the sweep deleted the Go cache entry %s, which read %s old under a %s minimum age: that is a minute inside Go's hour, and Go moves an entry's mtime on use at most once an hour, so an entry a build marked used a minute before this sweep reads exactly that old",
			edge.path, f.clock.at.Sub(edge.at).Round(time.Minute), minAge)
	}
	for _, entry := range past {
		if f.exists(entry.path) {
			t.Errorf("the control %s is still there, and it read %s old, past the minimum age and Go's hour together: the slack is one hour and not an hour and a little",
				entry.path, f.clock.at.Sub(entry.at).Round(time.Second))
		}
	}
	if f.exists(elsewhere.path) {
		t.Errorf("the sweep left %s, an entry of %s that read %s old under a %s minimum age: Go's hour is the Go build cache's allowance and that rule's alone, since no other tool on this volume moves an entry's modification time on use",
			elsewhere.path, other.Path, f.clock.at.Sub(elsewhere.at).Round(time.Minute), minAge)
	}
	f.assertProtected()
}

// A root that is absent and a root that is not a directory are skipped and reported, and the
// other roots are swept: a pod whose roles have not started yet has no bun cache, and a regular
// file where a cache directory belongs is what an interrupted `flock` leaves (installBlock,
// internal/runtime/sandbox/plugins.go).
func TestARootThatIsAbsentOrIsNotADirectoryIsSkippedAndReported(t *testing.T) {
	f := newTrimFixture(t, 4)
	absent := f.roots[0]
	notADirectory := f.roots[3]
	if err := os.RemoveAll(absent.Path); err != nil {
		t.Fatal(err)
	}
	for _, entry := range slices.Clone(f.entries) {
		if strings.HasPrefix(entry.path, absent.Path+"/") {
			f.entries = slices.DeleteFunc(f.entries, func(other *fixtureEntry) bool { return other == entry })
		}
	}
	if err := os.RemoveAll(notADirectory.Path); err != nil {
		t.Fatal(err)
	}
	writeAged(t, notADirectory.Path, f.clock.at.Add(-time.Hour))
	for _, entry := range slices.Clone(f.entries) {
		if strings.HasPrefix(entry.path, notADirectory.Path+"/") {
			f.entries = slices.DeleteFunc(f.entries, func(other *fixtureEntry) bool { return other == entry })
		}
	}
	if len(f.entries) == 0 {
		t.Fatal("the case took away every entry, so a sweep of nothing would pass it")
	}

	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d entries, want the %d of the two roots that are there: %s", out.deletions, len(f.entries), out.reason)
	}
	if f.logged(absent.Path) == 0 {
		t.Errorf("the absent root %s was not reported; the trimmer wrote %q", absent.Path, f.linesWritten())
	}
	if f.logged(notADirectory.Path) == 0 {
		t.Errorf("the root that is a regular file, %s, was not reported; the trimmer wrote %q", notADirectory.Path, f.linesWritten())
	}
	if !f.exists(notADirectory.Path) {
		t.Errorf("%s is gone: a root that is not a directory is skipped, never deleted", notADirectory.Path)
	}
	f.assertPrefixed()
}

// An entry larger than what is left to free is still deleted. More free space costs nothing, and
// a trimmer that kept an entry for being too big would leave the largest entries on the volume
// for ever — which on this pod is the one case that matters, since the bytes are in a handful of
// huge caches.
func TestAnEntryLargerThanWhatIsLeftToFreeIsStillDeleted(t *testing.T) {
	f := newTrimFixture(t, 3)
	oldest := f.oldestFirst()[0]
	// Ten points of the volume in one entry, against the two points the sweep needs.
	oldest.bytes = percentBytes(10)
	f.useVolume(f.modelled(72))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), f.modelled(72)(0))

	if f.exists(oldest.path) {
		t.Errorf("the oldest entry %s is still there: it is larger than the %d points the sweep had left to free, which is no reason to keep it", oldest.path, 2)
	}
	if out.freed <= 0 {
		t.Errorf("the sweep freed %d B, want the bytes of the entry it deleted", out.freed)
	}
}

// Progress is read from the volume and never counted from the bytes unlinked. bun hardlinks out
// of its install cache into a workspace's node_modules, so unlinking the cached copy frees
// nothing at all while those links stand: the trimmer sees three readings in a row with no
// decrease and stops, leaving the cache almost whole, where a trimmer counting the bytes it
// unlinked would empty the tree and report a volume it had not moved.
func TestTheSweepReadsItsProgressFromTheVolumeAndNotFromTheBytesItUnlinked(t *testing.T) {
	const entries = 1200
	f := newTrimFixture(t, 0)
	root := f.roots[1] // GOCACHE, whose entries are single files, as bun's cached files are
	links := filepath.Join(f.root, "workspaces", "cybermonklabs", "legion", "infra-2009", "node_modules")
	mkdirFixture(t, links)
	for at := range entries {
		shard := filepath.Join(root.Path, fmt.Sprintf("%02x", at%16))
		mkdirFixture(t, shard)
		entry := f.addFile(filepath.Join(shard, fmt.Sprintf("%032x-a", at)), root.Entry)
		// The cached copy is hardlinked into a workspace, so unlinking it frees nothing: the
		// entry's bytes stay on the volume, which is what the modelled reading says.
		entry.bytes = 0
		if err := os.Link(entry.path, filepath.Join(links, fmt.Sprintf("file-%09d", at))); err != nil {
			t.Fatal(err)
		}
	}
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), f.modelled(90)(0))

	// Three readings in a row showing no decrease, each a hundred deletions apart, counted off
	// the trimmer's own deletions and written out as the numbers: R1's "It reads the volume
	// again after every hundred deletions" and "it stops the sweep when three of those readings
	// in a row show no decrease". Fewer deletions to a reading makes the statfs a measurable
	// part of the sweep's own cost and more lets a sweep of a full volume run on past the low
	// mark it has already reached; one reading fewer ends a sweep that is freeing space on the
	// volume it was started for, since the kernel accounts a deletion asynchronously, and one
	// more goes on deleting caches the roles are taking back as fast as they are freed. The
	// sweep stops at another count than this one when either number moves, which is what a
	// comparison of the constants could not say (INFRA-2009, review round 6).
	const (
		readEvery     = 100
		stallReadings = 3
	)
	if want := stallReadings * readEvery; out.deletions != want {
		t.Errorf("the sweep deleted %d entries, want %d: %d readings in a row with no decrease, %d deletions apart (%s)",
			out.deletions, want, stallReadings, readEvery, out.reason)
	}
	if !strings.Contains(out.reason, "no decrease") {
		t.Errorf("the sweep stopped because %q, want the three readings showing no decrease", out.reason)
	}
	if out.freed != 0 {
		t.Errorf("the sweep reported %d B freed, want none: every byte it unlinked is still held by the link in the workspace", out.freed)
	}
	left := entries - len(f.gone())
	if left < entries*3/4 {
		t.Errorf("%d of %d entries are left, want the sweep to leave the cache almost whole once it saw the volume was not moving", left, entries)
	}
	// The control: a trimmer counting the bytes it unlinked would have deleted every one of
	// these entries, since all of them together are a long way short of the twenty points the
	// sweep set out to free, and the volume would not have moved a byte.
	var unlinkable int64
	for _, entry := range f.entries {
		if info, err := os.Lstat(entry.path); err == nil {
			unlinkable += info.Size()
		}
	}
	if needed := int64(percentBytes(20)); unlinkable >= needed {
		t.Fatalf("the entries hold %d B against the %d B the sweep needs, so a trimmer counting unlinked bytes would have stopped on its own and this control proves nothing", unlinkable, needed)
	}
	t.Logf("the control: a trimmer counting unlinked bytes would have deleted all %d entries, whose %d B are a twenty-point sweep's %d B short, and freed nothing",
		entries, unlinkable, percentBytes(20))
}

// A reading a byte under the one before it is a decrease, and starts the count of readings that
// show none over: R1 stops the sweep "when three of those readings in a row show no decrease",
// and the case above stands every reading on the one before it, with which a count that took a
// one-byte decrease for no decrease is as green. The sweep here deletes four hundred entries
// with a reading after each hundred, each a byte under the one before and every one far above the
// low mark, so it deletes all four hundred and ends with no entry eligible, where a count of
// three readings showing no decrease would end it at three hundred.
func TestAReadingAByteUnderTheOneBeforeItIsADecrease(t *testing.T) {
	const entries = 400
	f := newTrimFixture(t, 0)
	root := f.roots[1] // GOCACHE, whose entries are single files, the cheapest entry to make
	for at := range entries {
		shard := filepath.Join(root.Path, fmt.Sprintf("%02x", at%16))
		mkdirFixture(t, shard)
		f.addFile(filepath.Join(shard, fmt.Sprintf("%032x-a", at)), root.Entry)
	}
	first := usageAt(90)
	// The reading after the n-th hundred deletions is n bytes under the sweep's first.
	f.useVolume(func(call int) volumeUsage {
		used := first.UsedBytes - uint64(call)
		return volumeUsage{UsedBytes: used, AvailableBytes: fixtureVolumeBytes - used}
	})
	f.setSettings(trimSettings())

	out := f.trimmer().sweep(context.Background(), *trimSettings(), first)

	if out.deletions != entries || strings.Contains(out.reason, "no decrease") {
		t.Errorf("the sweep deleted %d of %d entries and stopped because %q, want every one deleted: each reading was a byte under the one before it, which is a decrease",
			out.deletions, entries, out.reason)
	}
	if reads := f.readCount(); reads != entries/100 {
		t.Errorf("the sweep read the volume %d times, want %d: one after each hundred deletions", reads, entries/100)
	}
}

// The sweep skips an entry any process of the pod is using, driven one /proc entry at a time:
// each case builds a /proc holding one process that names the entry through that entry alone,
// so a trimmer reading only open file descriptors fails five of the six. A path on the entry
// itself counts as much as one under it, because a `flock` is held on a directory and not on a
// file in it. The control in every case is the other entries of the same tree, which are
// deleted: a trimmer that skipped everything would pass nothing here.
func TestTheSweepSkipsAnEntryALiveProcessIsUsing(t *testing.T) {
	sources := []struct {
		name  string
		place func(process *fixtureProcess, path string)
	}{
		{"an open file descriptor", func(p *fixtureProcess, path string) { p.fds = []string{path} }},
		{"the working directory", func(p *fixtureProcess, path string) { p.cwd = path }},
		{"the executable", func(p *fixtureProcess, path string) { p.exe = path }},
		{"the root", func(p *fixtureProcess, path string) { p.root = path }},
		{"a mapped file", func(p *fixtureProcess, path string) { p.maps = []string{path} }},
		{"a thread's working directory", func(p *fixtureProcess, path string) { p.threads = map[int]string{p.pid + 1: path} }},
	}
	for _, source := range sources {
		for _, held := range []string{"the entry itself", "a path under the entry"} {
			t.Run(source.name+", "+held, func(t *testing.T) {
				f := newTrimFixture(t, 3)
				entry := f.oldestDirectory()
				path := entry.path
				if held == "a path under the entry" {
					path = filepath.Join(entry.path, "b1", "out")
				}
				process := fixtureProcess{pid: 4101}
				source.place(&process, path)
				f.proc, _ = writeProcFixture(t, process)
				f.useVolume(steady(90))
				f.setSettings(trimSettings())

				tr := f.trimmer()
				out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

				if !f.exists(entry.path) {
					t.Errorf("the sweep deleted %s, which a process holds through %s", entry.path, source.name)
				}
				if want := len(f.entries) - 1; out.deletions != want {
					t.Errorf("the sweep deleted %d entries, want the %d that no process holds: %s", out.deletions, want, out.reason)
				}
				f.assertProtected()
			})
		}
	}
}

// oldestDirectory is the oldest entry of the fixture that is a directory, which is the entry a
// case drives when it needs a path under the entry as well as the entry itself.
func (f *trimFixture) oldestDirectory() *fixtureEntry {
	f.t.Helper()
	for _, entry := range f.oldestFirst() {
		if info, err := os.Lstat(entry.path); err == nil && info.IsDir() {
			return entry
		}
	}
	f.t.Fatal("the fixture holds no entry that is a directory")
	return nil
}

// placeProcess puts one more process into a /proc a case has already built, naming one path
// through its working directory: the cases below need a process that appears while a sweep is
// running, and the six entries it could appear through are driven one at a time above. It answers
// the pid it placed, which writeFakeProcess moves off the test's own as it moves every fake
// process of these cases.
func placeProcess(t *testing.T, proc string, pid int, cwd string) int {
	t.Helper()
	return writeFakeProcess(t, proc, fixtureProcess{pid: pid, cwd: cwd})
}

// placeRefusedProcess puts in a process whose /proc the kernel refuses, staged as a regular file
// where the fd directory belongs, which refuses every reader including root (fixtureProcess's
// fdIsFile). It answers the pid it placed, as placeProcess does.
func placeRefusedProcess(t *testing.T, proc string, pid int, cwd string) int {
	t.Helper()
	return writeFakeProcess(t, proc, fixtureProcess{pid: pid, cwd: cwd, fdIsFile: true})
}

// A process that starts using an entry after the sweep has begun still protects it: the
// used-path set is read again at least once a second while the sweep runs, which the case
// drives on the injected clock. Its control is the same tree swept with no process appearing,
// where that entry is deleted.
//
// The second is driven from both sides and written out as the second rather than read off
// volumeUsedPathsMaxAge: with a second and a millisecond of the sweep's clock gone the set is
// read again and the entry survives, and with a millisecond less than a second gone it is not
// read again and the entry goes. That window is what R3's set read "once at the start of a
// sweep and again at least once a second during it" allows and bounds — a longer age leaves a
// tool that starts under a running sweep unprotected for that long on the full volume the sweep
// is deleting from, and a shorter one spends another pass over /proc per sweep than the design
// measured, which is six entries read per process of the pod (INFRA-2009, review round 6).
func TestAProcessThatStartsAfterTheSweepBeganStillProtectsItsEntry(t *testing.T) {
	// Of the trimmer's own clock, moved at the entry boundary after the first deletion.
	const (
		pastASecond   = time.Second + time.Millisecond
		insideASecond = time.Second - time.Millisecond
	)
	sweep := func(t *testing.T, appears bool, gone time.Duration) (survived bool, deletions int, entries int) {
		f := newTrimFixture(t, 4)
		order := f.oldestFirst()
		first := order[0]
		var last *fixtureEntry
		for at := len(order) - 1; at >= 0; at-- {
			if info, err := os.Lstat(order[at].path); err == nil && info.IsDir() {
				last = order[at]
				break
			}
		}
		if last == nil || last == first {
			t.Fatal("the fixture holds no late directory entry for the process to take hold of")
		}
		placed := false
		f.clock.setHook(func(clock *fakeClock) {
			if placed || f.exists(first.path) {
				return
			}
			placed = true
			if appears {
				placeProcess(t, f.proc, 4242, filepath.Join(last.path, "b1"))
			}
			// What the trimmer reads to decide whether to read /proc again.
			clock.advance(gone)
		})
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
		if !placed {
			t.Fatal("the case never reached the moment it stages: the sweep deleted nothing at all")
		}
		return f.exists(last.path), out.deletions, len(f.entries)
	}

	survived, deletions, entries := sweep(t, true, pastASecond)
	if !survived {
		t.Error("the sweep deleted the entry a process took hold of after the sweep began, want the used-path set read again at least once a second")
	}
	if deletions != entries-1 {
		t.Errorf("the sweep deleted %d entries, want the %d that nothing holds", deletions, entries-1)
	}
	// The control: with no process appearing, that same entry goes.
	t.Run("the control, with no process appearing", func(t *testing.T) {
		survived, deletions, entries := sweep(t, false, pastASecond)
		if survived {
			t.Error("the entry survived a sweep with no process holding it, so the case above shows nothing")
		}
		if deletions != entries {
			t.Errorf("the control sweep deleted %d of %d entries, want every one", deletions, entries)
		}
	})
	// The other side of the same second: a process that appears with less than a second of the
	// sweep's clock gone is not in the set the sweep deletes by. That is the exposure the
	// second bounds, and the re-read of the entry's own time immediately before the delete is
	// what narrows it (TestAnEntryWhoseTimeMovedBetweenTheScanAndTheDeleteIsSkipped).
	t.Run("a process that appears with less than a second of the sweep's clock gone", func(t *testing.T) {
		survived, deletions, entries := sweep(t, true, insideASecond)
		if survived {
			t.Error("the sweep kept the entry a process took hold of with less than a second of its clock gone, want the used-path set read again no sooner than a second: a pass over /proc at every entry is the cost the design measured and refused")
		}
		if deletions != entries {
			t.Errorf("the sweep deleted %d of %d entries, want every one: no reading of the set it deletes by holds them", deletions, entries)
		}
	})
	// The second itself: a set exactly a second old is read again, since the sweep reads it again
	// "whenever it is volumeUsedPathsMaxAge old" (sweep's guard), so a process that appears with
	// exactly a second of the sweep's clock gone protects its entry. The two cases either side of
	// it stand a millisecond off, and a set read again only once it was older than a second is
	// green with them.
	t.Run("a process that appears with exactly a second of the sweep's clock gone", func(t *testing.T) {
		survived, deletions, entries := sweep(t, true, time.Second)
		if !survived {
			t.Error("the sweep deleted the entry a process took hold of with exactly a second of its clock gone, want the used-path set read again once it is a second old")
		}
		if deletions != entries-1 {
			t.Errorf("the sweep deleted %d entries, want the %d that nothing holds", deletions, entries-1)
		}
	})
}

// Immediately before the delete the entry's newest time is read again, and an entry something
// wrote in since the listing is skipped: a build that started while the sweep was listing must
// keep the cache it is writing. A directory is asked with newerThan and a single file with the
// lstat the delete needs anyway, so the case drives both. Its control is the same tree with
// nothing written, where the entry goes.
func TestAnEntryWhoseTimeMovedBetweenTheScanAndTheDeleteIsSkipped(t *testing.T) {
	for _, kind := range []string{"a directory entry", "a single file entry"} {
		for _, written := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, written again %t", kind, written), func(t *testing.T) {
				f := newTrimFixture(t, 4)
				order := f.oldestFirst()
				first := order[0]
				var moved *fixtureEntry
				for at := len(order) - 1; at >= 0; at-- {
					info, err := os.Lstat(order[at].path)
					if err != nil {
						t.Fatal(err)
					}
					if info.IsDir() == (kind == "a directory entry") && order[at] != first {
						moved = order[at]
						break
					}
				}
				if moved == nil {
					t.Fatalf("the fixture holds no late entry of the kind %q", kind)
				}
				touched := false
				f.clock.setHook(func(_ *fakeClock) {
					if touched || f.exists(first.path) {
						return
					}
					touched = true
					if !written {
						return
					}
					path := moved.path
					if kind == "a directory entry" {
						path = filepath.Join(moved.path, "b1", "out")
					}
					fresh := f.clock.at
					if err := os.Chtimes(path, fresh, fresh); err != nil {
						t.Error(err)
					}
				})
				f.useVolume(steady(90))
				f.setSettings(trimSettings())

				tr := f.trimmer()
				out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
				if !touched {
					t.Fatal("the case never reached the moment it stages: the sweep deleted nothing at all")
				}
				switch {
				case written && !f.exists(moved.path):
					t.Errorf("the sweep deleted %s although its time moved between the listing and the delete", moved.path)
				case written && out.deletions != len(f.entries)-1:
					t.Errorf("the sweep deleted %d entries, want the %d whose time had not moved", out.deletions, len(f.entries)-1)
				case !written && f.exists(moved.path):
					t.Errorf("the control left %s alone although nothing wrote in it, so the case shows nothing", moved.path)
				}
			})
		}
	}
}

// An entry written a second after the time its listing read is skipped, as one written at the
// sweep's own time is: the pre-delete re-read skips an entry anything in which is newer than the
// time the listing read (moved, newerThan). The case above writes the entry at the sweep's own
// time, two hours and more past the time the listing read, and a re-read that allowed a second of
// slack is as green with it as with its control, which writes nothing. Each of the three reads of
// that re-read is driven: a file inside a directory entry, which newerThan's walk reads; the
// directory entry's own time, which newerThan's lstat of it reads and which the case above never
// moves, since it writes only the file inside; and a single file entry, which moved's own lstat
// reads. Each is written a second past the time read off it before the sweep, the time the
// listing reads too, and read back to show it stands there.
func TestAnEntryWrittenASecondAfterTheTimeItsListingReadIsSkipped(t *testing.T) {
	for _, kind := range []struct {
		name    string
		dir     bool   // whether the entry is a directory, or a single file
		written string // what of the entry is written, below its path
	}{
		{"a file in a directory entry", true, filepath.Join("b1", "out")},
		{"a directory entry's own time", true, ""},
		{"a single file entry", false, ""},
	} {
		t.Run(kind.name, func(t *testing.T) {
			f := newTrimFixture(t, 4)
			order := f.oldestFirst()
			first := order[0]
			var moved *fixtureEntry
			for at := len(order) - 1; at >= 0; at-- {
				info, err := os.Lstat(order[at].path)
				if err != nil {
					t.Fatal(err)
				}
				if info.IsDir() == kind.dir && order[at] != first {
					moved = order[at]
					break
				}
			}
			if moved == nil {
				t.Fatalf("the fixture holds no late entry of the kind %q", kind.name)
			}
			path := filepath.Join(moved.path, kind.written)
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			later := info.ModTime().Add(time.Second)
			touched := false
			f.clock.setHook(func(_ *fakeClock) {
				if touched || f.exists(first.path) {
					return
				}
				touched = true
				if err := os.Chtimes(path, later, later); err != nil {
					t.Error(err)
				}
				switch info, err := os.Lstat(path); {
				case err != nil:
					t.Error(err)
				case !info.ModTime().Equal(later):
					t.Errorf("%s reads as written at %s, want the %s the case wrote", path, info.ModTime(), later)
				}
			})
			f.useVolume(steady(90))
			f.setSettings(trimSettings())

			out := f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))
			if !touched {
				t.Fatal("the case never reached the moment it stages: the sweep deleted nothing at all")
			}
			if !f.exists(moved.path) {
				t.Errorf("the sweep deleted %s, written a second after the time its listing read: something wrote in it between the listing and the delete", moved.path)
			}
			if out.deletions != len(f.entries)-1 {
				t.Errorf("the sweep deleted %d entries, want the %d whose time had not moved: %s", out.deletions, len(f.entries)-1, out.reason)
			}
		})
	}
}

// A role owns the directories its caches sit in — /legion/scratch is writable by the role uid
// (`drwx--S--- legion legion`, measured in an issue pod of this deployment at
// 2026-11-11T08:05Z) — so between the listing and the delete it can rename a cache root, or a
// shard of the Go build cache, out of the way and put a symbolic link where it stood. A sweep
// that asked its questions of the entry's path string would resolve that link on every one of
// them and unlink what it points at; in this pod the launcher runs as the roles' own uid in the
// container's mount namespace, where /var/run/legion/roles holds every other role's private,
// state and agent-secrets directories, so the link can be aimed at another role's files
// (INFRA-2009, review 6).
//
// The case plays the role. It waits for the sweep's first deletion, by which time the listing
// is over, then renames the cache home and one shard of the Go build cache away and puts links
// to a bait directory of its own where they stood. The bait is a sibling of this case's volume,
// so it is outside every reclaimable root, and it holds one directory under every name the
// swapped entries have, aged as those entries are aged, so that a sweep walking the links finds
// an entry that passes every guard and deletes a file the role may not reach. What must happen
// instead is the whole of the fix: the sweep deletes through the handle it opened on each
// directory while listing it, so the entries it read are the entries that go and the bait is
// untouched.
func TestTheSweepDeletesThroughTheDirectoriesItListedAndNotThroughLinksPutInTheirPlace(t *testing.T) {
	const perRoot = 12
	f := newTrimFixture(t, perRoot)
	bait := t.TempDir() // a sibling of the volume root, outside every reclaimable root
	const baitHeld = "another role's agent-secrets token"
	home := f.roots[2]  // XDG_CACHE_HOME, the root whose rename the review names
	cache := f.roots[1] // GOCACHE, whose entries sit one directory deeper, in a shard

	order := f.oldestFirst()
	first := order[0]
	// The shard of the newest Go cache entry: the swap is staged after the first deletion, so a
	// shard whose entries come last is one no deletion has reached when it lands.
	shard := ""
	for at := len(order) - 1; at >= 0 && shard == ""; at-- {
		if strings.HasPrefix(order[at].path, cache.Path+"/") {
			shard = filepath.Dir(order[at].path)
		}
	}
	if shard == "" {
		t.Fatal("the fixture holds no entry of the Go build cache, so the shard swap would stage nothing")
	}

	// Every entry the two swaps hide, with the name a sweep resolving path strings again would
	// reach in the bait and the name the handle reaches in the directory the listing opened.
	var baited, listed []string
	for _, entry := range f.entries {
		var moved string
		switch {
		case entry == first:
			continue // deleted before the swap: it stages the case rather than being read by it
		case filepath.Dir(entry.path) == home.Path:
			moved = home.Path + ".real"
		case filepath.Dir(entry.path) == shard:
			moved = shard + ".real"
		default:
			continue
		}
		name := filepath.Base(entry.path)
		token := filepath.Join(bait, name, "TOKEN")
		mkdirFixture(t, filepath.Dir(token))
		if err := os.WriteFile(token, []byte(baitHeld), 0o600); err != nil {
			t.Fatal(err)
		}
		// Aged as the entry it stands in for: a bait written in this second would be newer than
		// the time the listing recorded, and the pre-delete re-read would pass it over for that
		// and not for the link.
		for _, aged := range []string{token, filepath.Dir(token)} {
			if err := os.Chtimes(aged, entry.at, entry.at); err != nil {
				t.Fatal(err)
			}
		}
		baited = append(baited, token)
		listed = append(listed, filepath.Join(moved, name))
	}
	if len(baited) == 0 {
		t.Fatal("the case staged no bait, so a sweep that deleted nothing at all would pass it")
	}
	if len(listed) == 0 {
		t.Fatal("the case staged no entry under the swapped directories, so it could not tell the two apart")
	}

	swapped := false
	f.clock.setHook(func(_ *fakeClock) {
		if swapped || f.exists(first.path) {
			return
		}
		swapped = true
		for _, place := range []string{home.Path, shard} {
			if err := os.Rename(place, place+".real"); err != nil {
				t.Error(err)
				return
			}
			if err := os.Symlink(bait, place); err != nil {
				t.Error(err)
				return
			}
		}
	})
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
	if !swapped {
		t.Fatal("the case never reached the moment it stages: the sweep deleted nothing at all")
	}

	for _, token := range baited {
		switch content, err := os.ReadFile(token); {
		case err != nil:
			t.Errorf("the bait %s: %v; the sweep deleted outside every reclaimable root, through a link a role put where a directory it had listed stood", token, err)
		case string(content) != baitHeld:
			t.Errorf("the bait %s holds %q, want %q", token, content, baitHeld)
		}
	}
	for _, entry := range listed {
		if f.exists(entry) {
			t.Errorf("%s is still there: the sweep must delete the entries it listed, through the handle it opened on the directory it read them from", entry)
		}
	}
	// The two children of the cache home that are never entries went with the root the role
	// renamed, and are still there: nothing of the swap makes the sweep reach them either.
	for _, kept := range []string{playwrightRegistryDir, claudeCodeCacheDir} {
		if !f.exists(filepath.Join(home.Path+".real", kept)) {
			t.Errorf("%s of the renamed cache home is gone, want a root's exceptions left alone whatever a role did to the root's name", kept)
		}
	}
	// Every entry of the fixture is eligible and nothing holds any of them, so a sweep that
	// deleted the entries it listed deleted all of them: a lower count is a sweep that walked
	// into the bait for some of its entries and found nothing of the name it was looking for.
	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d entries, want the %d it listed: %s", out.deletions, len(f.entries), out.reason)
	}
}

// A /proc read the kernel refused on the first read of a sweep deletes nothing; one refused
// later ends the sweep at the next entry boundary and keeps what it deleted. Either names the
// process and is carried into the report, because a refused process is one whose open files are
// unknown and the operator is the one who can go and look.
func TestARefusedProcReadStopsTheSweepAndNamesTheProcess(t *testing.T) {
	t.Run("on the first read of the sweep", func(t *testing.T) {
		f := newTrimFixture(t, 3)
		refused := strconv.Itoa(placeRefusedProcess(t, f.proc, 5101, filepath.Join(f.root, "scratch")))
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

		if out.deletions != 0 || len(f.gone()) != 0 {
			t.Errorf("the sweep deleted %d entries after a refused first read of /proc, want none", out.deletions)
		}
		if len(tr.refusedReads) != 1 || !strings.Contains(tr.refusedReads[0], refused) {
			t.Errorf("the sweep handed the trimmer the refusals %v, want exactly the process %s this case made unreadable", tr.refusedReads, refused)
		}
		if !strings.Contains(out.reason, "refused") {
			t.Errorf("the sweep stopped because %q, want the refusal named", out.reason)
		}
		if !out.report {
			t.Error("the sweep that deleted nothing on a refused /proc asks for no report, want one: the volume is still above the low mark and nobody else can tell the operator")
		}
	})

	t.Run("while the sweep runs", func(t *testing.T) {
		f := newTrimFixture(t, 6)
		first := f.oldestFirst()[0]
		placed := false
		var refused string
		f.clock.setHook(func(clock *fakeClock) {
			if placed || f.exists(first.path) {
				return
			}
			placed = true
			refused = strconv.Itoa(placeRefusedProcess(t, f.proc, 5202, filepath.Join(f.root, "scratch")))
			clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
		})
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

		if !placed {
			t.Fatal("the case never reached the moment it stages")
		}
		if out.deletions == 0 {
			t.Error("the sweep kept nothing of what it had deleted before the refusal, want the deletions to stand")
		}
		if out.deletions >= len(f.entries) {
			t.Errorf("the sweep deleted all %d entries, want it to stop at the first entry boundary after the refusal", out.deletions)
		}
		if len(tr.refusedReads) != 1 || !strings.Contains(tr.refusedReads[0], refused) {
			t.Errorf("the sweep handed the trimmer the refusals %v, want the process %s named", tr.refusedReads, refused)
		}
		if !strings.Contains(out.reason, "refused") {
			t.Errorf("the sweep stopped because %q, want the refusal named", out.reason)
		}
	})
}

// R3: a refused /proc read "names the process it could not read and is carried into the next
// report". A sweep whose later read was refused and whose last reading is at or below the low
// mark sends no report of its own, so the refusal is kept and the next report the trimmer sends
// names the process, though the sweep behind that report read /proc in full.
func TestARefusedProcReadOfASweepThatSendsNoReportIsCarriedIntoTheNext(t *testing.T) {
	f := newTrimFixture(t, 6)
	first := f.oldestFirst()[0]
	placed := false
	var refused string
	f.clock.setHook(func(clock *fakeClock) {
		if placed || f.exists(first.path) {
			return
		}
		placed = true
		refused = strconv.Itoa(placeRefusedProcess(t, f.proc, 5303, filepath.Join(f.root, "scratch")))
		clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
	})
	// The first interval's reading starts a sweep, whose last reading is under the low mark; the
	// second interval's starts another, whose last reading is above it.
	f.useVolume(script(90, 60, 90, 80))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	f.tick("the sweep ended")
	if !placed {
		t.Fatal("the case never reached the moment it stages")
	}
	if f.logged("a /proc read the kernel refused while the sweep ran") != 1 {
		t.Fatalf("the first sweep did not stop on the refused read; the trimmer wrote %q", f.linesWritten())
	}
	if sent := len(f.reports()); sent != 0 {
		t.Fatalf("the sweep that ended under the low mark sent %d reports, want none", sent)
	}

	// The process goes, so the second sweep reads every process of the pod.
	if err := os.RemoveAll(filepath.Join(f.proc, refused)); err != nil {
		t.Fatal(err)
	}
	f.tick("the sweep ended")
	testwait.Eventually(t, "the report of the second sweep", func() bool { return len(f.reports()) == 1 })
	report := f.reports()[0]
	if len(report.RefusedReads) != 1 || !strings.Contains(report.RefusedReads[0], refused) {
		t.Errorf("the report names the refused reads %v, want the process %s the earlier sweep could not read", report.RefusedReads, refused)
	}
	if strings.Contains(report.StopReason, "refused") {
		t.Errorf("the second sweep stopped because %q, want it to have read /proc in full", report.StopReason)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse this report: %v", err)
	}
}

// The refused /proc reads a trimmer carries between reports are kept up to the most a report
// names and counted past it (carryRefused): twenty-five carried are twenty-five kept and none
// counted, and one more is counted and not kept, so a pod that never reports holds no more than
// the bound for the life of the launcher process. The other cases carry one, or two hundred into a
// report, whose own bound cuts any list to the same cap (bound) and so is green with a carry that
// kept twenty-six; what the trimmer holds is read here, after each carry. The figures are written
// out rather than read off shimwire.VolumeReportRefusedReads, which a case staged from would move
// with.
func TestTheTrimmerCarriesTwentyFiveRefusedReadsAndCountsTheTwentySixth(t *testing.T) {
	const most = 25
	tr := newTrimFixture(t, 0).trimmer()
	refusals := make([]string, most+1)
	for at := range refusals {
		refusals[at] = fmt.Sprintf("process %d: fd: permission denied", 7000+at)
	}

	tr.carryRefused(refusals[:most])
	if len(tr.refusedReads) != most || tr.refusedDropped != 0 {
		t.Errorf("the trimmer holds %d refused reads and counts %d more after %d were carried, want all %d held and none counted", len(tr.refusedReads), tr.refusedDropped, most, most)
	}
	tr.carryRefused(refusals[most:])
	if len(tr.refusedReads) != most || tr.refusedDropped != 1 {
		t.Errorf("the trimmer holds %d refused reads and counts %d more after %d were carried, want %d held and the last counted", len(tr.refusedReads), tr.refusedDropped, most+1, most)
	}
	if !slices.Equal(tr.refusedReads, refusals[:most]) {
		t.Errorf("the trimmer holds %q, want the first %d it was carried", tr.refusedReads, most)
	}
}

// trimHandleHold is how long the helper child holds its handle before giving up on its own: a
// case that failed before killing it must not leave a process holding a directory of the pod's
// /legion open for ever.
const trimHandleHold = 2 * time.Minute

// startHandleHelper runs the test binary as another process of the pod holding one directory
// open under a `flock`, which is what every rendered install does to its cache root
// (installBlock, internal/runtime/sandbox/plugins.go), waits until it is holding it, and
// answers its pid.
func startHandleHelper(t *testing.T, path string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "holding")
	child := exec.Command(self, "-test.run=^TestVolumeTrimHandleHelper$")
	child.Env = append(os.Environ(), volumeTrimHandleEnv+"="+path+"\n"+marker)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	testwait.Eventually(t, "the helper to hold "+path+" open", func() bool {
		_, err := os.Lstat(marker)
		return err == nil
	})
	return child.Process.Pid
}

// linkProcFixture is a /proc of the processes a case names, each entry the kernel's own: a
// directory of symbolic links to /proc/<pid>, which the reader walks into and reads the real
// `fd`, `exe` and `maps` of.
//
// The pod's trimmer reads /proc whole, as PID 1 of the roles container, where every process is
// the pod's own and readable. A test process on a shared machine is refused the processes of
// other users — in this container /proc/1 belongs to root — and one refusal makes a sweep
// delete nothing, which is the trimmer doing as it is told and would leave the two cases below
// proving nothing. They name their own processes instead, and what they read of each is the
// kernel's answer and not a fixture's.
func linkProcFixture(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, pid := range pids {
		linkProcEntry(t, root, pid)
	}
	return root
}

// linkProcEntry adds one process to a /proc a case built, which is how a case starts a process
// under a running sweep: the reader lists the directory again at every refresh of the used-path
// set (readUsedPaths), so the process is in that reading and in none taken before it.
func linkProcEntry(t *testing.T, root string, pid int) {
	t.Helper()
	if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(root, strconv.Itoa(pid))); err != nil {
		t.Fatal(err)
	}
}

// TestVolumeTrimHandleHelper is the test binary re-executed as another process of the pod: it
// opens the directory its case names, takes the `flock` a role's tool takes on it, says so and
// waits to be killed.
func TestVolumeTrimHandleHelper(t *testing.T) {
	plan := os.Getenv(volumeTrimHandleEnv)
	if plan == "" {
		return
	}
	path, marker, _ := strings.Cut(plan, "\n")
	handle, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim handle:", err)
		os.Exit(2)
	}
	if err := unix.Flock(int(handle.Fd()), unix.LOCK_EX); err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim handle:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "volume-trim handle:", err)
		os.Exit(2)
	}
	time.Sleep(trimHandleHold)
	os.Exit(0)
}

// A root another process holds open is left alone whole: the rendered install scripts take a
// `/usr/bin/flock` on the cache root itself for the length of an install, and deleting entries
// under a root a tool has locked is deleting the cache that tool is filling. The case runs a
// real process and reads the container's own /proc, which is the pod this was written for, and
// its control is the other three roots, which are swept.
func TestTheSweepSkipsEveryEntryOfARootAProcessHoldsOpen(t *testing.T) {
	f := newTrimFixture(t, 3)
	locked := f.roots[3] // bun's install cache, the flock target of every rendered install
	holder := startHandleHelper(t, locked.Path)
	f.proc = linkProcFixture(t, os.Getpid(), holder)
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	kept, swept := 0, 0
	for _, entry := range f.entries {
		under := strings.HasPrefix(entry.path, locked.Path+"/")
		switch {
		case under && !f.exists(entry.path):
			t.Errorf("the sweep deleted %s, under the root %s another process holds open", entry.path, locked.Path)
		case under:
			kept++
		case f.exists(entry.path):
			t.Errorf("the sweep left %s, which nothing holds", entry.path)
		default:
			swept++
		}
	}
	if kept == 0 || swept == 0 {
		t.Fatalf("the case kept %d entries and swept %d, want both: it can tell nothing apart otherwise", kept, swept)
	}
	if out.deletions != swept {
		t.Errorf("the sweep counted %d deletions against the %d entries that are gone", out.deletions, swept)
	}
	if f.logged(locked.Path) == 0 {
		t.Errorf("the locked root %s was not reported; the trimmer wrote %q", locked.Path, f.linesWritten())
	}
	f.assertProtected()
}

// A root that is taken open after the listing has passed it is skipped from that moment too:
// the rendered install that takes the `flock` runs at every role launch (installBlock,
// internal/runtime/sandbox/plugins.go), and a full volume is exactly when roles are being
// relaunched, so a sweep that may run five minutes meets `bun install` starting under it. The
// used-path set is read again every volumeUsedPathsMaxAge for that reason, and the question
// asked of each reading is of the entry's root (usedPaths.Holds): Covers is of the entry
// itself and ignores a process holding an ancestor of it by design.
//
// The lock is taken at the first entry boundary after the sweep's first deletion, which is
// after the listing by construction, and the entries that stood under the root at that moment
// are the ones the sweep must now leave. Its control is the other three roots, swept whole.
func TestTheSweepSkipsARootAProcessTakesOpenWhileItRuns(t *testing.T) {
	f := newTrimFixture(t, 6)
	locked := f.roots[3] // bun's install cache, the flock target of every rendered install
	f.proc = linkProcFixture(t, os.Getpid())
	oldest := f.oldestFirst()[0]
	var held []string // the entries of the locked root that stood when the lock was taken
	taken := false
	f.clock.setHook(func(clock *fakeClock) {
		if taken || f.exists(oldest.path) {
			return
		}
		taken = true
		linkProcEntry(t, f.proc, startHandleHelper(t, locked.Path))
		for _, entry := range f.entries {
			if strings.HasPrefix(entry.path, locked.Path+"/") && f.exists(entry.path) {
				held = append(held, entry.path)
			}
		}
		// The age at which the delete loop reads /proc again, so the next reading of the clock
		// is the one that takes the reading the lock is in.
		clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
	})
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if !taken {
		t.Fatalf("the case never reached the moment it stages: the sweep deleted nothing, so no lock was taken while it ran (%s)", out.reason)
	}
	kept, swept := 0, 0
	for _, entry := range f.entries {
		under := strings.HasPrefix(entry.path, locked.Path+"/")
		switch {
		case under && !f.exists(entry.path):
			if slices.Contains(held, entry.path) {
				t.Errorf("the sweep deleted %s, which stood under %s when a process took that root open; the trimmer wrote %q",
					entry.path, locked.Path, f.linesWritten())
			}
			// Anything else under the root went before the lock was taken, which is the sweep
			// doing as it was told.
		case under:
			kept++
		case f.exists(entry.path):
			t.Errorf("the sweep left %s, which nothing holds; the trimmer wrote %q", entry.path, f.linesWritten())
		default:
			swept++
		}
	}
	if kept == 0 || swept == 0 {
		t.Fatalf("the case kept %d entries under %s and swept %d elsewhere, want both: it can tell nothing apart otherwise; the trimmer wrote %q",
			kept, locked.Path, swept, f.linesWritten())
	}
	if out.deletions != len(f.gone()) {
		t.Errorf("the sweep counted %d deletions against the %d entries that are gone", out.deletions, len(f.gone()))
	}
	if said := f.logged(locked.Path); said != 1 {
		t.Errorf("the trimmer wrote %d lines naming %s, want the one line a root that became held is worth: %d entries were skipped under it, and the roles container's log is the only place this work is visible; it wrote %q",
			said, locked.Path, kept, f.linesWritten())
	}
	f.assertProtected()
}

// The trimmer leaves its own process out of the used-path set: listing a root and removing an
// entry both hold a handle under a root, and a sweep that read its own handle as a live use
// would skip everything it was about to delete. The case holds the same handle the case above
// gives another process, in this process, and the sweep still deletes.
func TestTheSweepLeavesItsOwnProcessOutOfTheUsedPathSet(t *testing.T) {
	f := newTrimFixture(t, 3)
	root := f.roots[3]
	handle, err := os.Open(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := unix.Flock(int(handle.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	f.proc = linkProcFixture(t, os.Getpid())
	f.self = os.Getpid()
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if out.deletions != len(f.entries) {
		t.Errorf("the sweep deleted %d of %d entries while this process held its own handle on %s, want every one: %s",
			out.deletions, len(f.entries), root.Path, out.reason)
	}
	f.assertProtected()
}

// The sweep's budget runs from its start with the listing included, so a tree whose listing
// alone spends five minutes ends on the budget having deleted nothing, and says so. The listing
// is the larger half of the work on a full volume — one newestModTime per entry, which is a
// walk of a whole `go-build` tree — so a budget that started at the first deletion would bound
// the cheaper half alone. A sweep that ended on its budget still reports, with the sizes of a
// walk: it is the sweep most worth telling the daemon about.
// Its sizes carry their two age splits, the bytes untouched for more than an hour and for more
// than a day, which is what tells a reader a working set from cold bytes: one target of the
// volume holds a file written ten minutes before the sweep, one two hours before it and one two
// days before it, and its size in that report is held to each split.
func TestTheSweepsBudgetRunsFromItsStartWithTheListingIncluded(t *testing.T) {
	f := newTrimFixture(t, 20)
	// A minute of the trimmer's clock per reading of it, which the listing asks at every entry.
	f.clock.step = time.Minute
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	// The target whose two splits the report is held to, beside the fixture's own tree: three
	// files of three ages and of sizes that tell every sum apart. Their ages are the trimmer's
	// clock's at the walk less its few minutes of readings, which leaves each on its own side of
	// both splits.
	splits := filepath.Join(f.root, "workspaces", "age-splits")
	mkdirFixture(t, splits)
	for _, file := range []struct {
		name  string
		age   time.Duration
		bytes int
	}{
		{"fresh", 10 * time.Minute, 100},
		{"hour", 2 * time.Hour, 20},
		{"day", 48 * time.Hour, 3},
	} {
		path, at := filepath.Join(splits, file.name), f.clock.at.Add(-file.age)
		if err := os.WriteFile(path, make([]byte, file.bytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), f.modelled(90)(0))

	if out.deletions != 0 {
		t.Errorf("the sweep deleted %d entries, want none: the budget was spent before the listing ended", out.deletions)
	}
	if !strings.Contains(out.reason, "budget") {
		t.Errorf("the sweep stopped because %q, want the budget named", out.reason)
	}
	if !out.report {
		t.Fatal("a sweep that ended on its budget above the low mark asks for no report, want one")
	}
	report := tr.report(context.Background(), out)
	if len(report.Sizes) == 0 {
		t.Error("the report of a sweep that ended on its budget carries no sizes, want the walk's: the budget bounds the sweep and not the walk")
	}
	if report.SizesAt.IsZero() {
		t.Error("the report carries no time for its sizes, want when they were read")
	}
	at := slices.IndexFunc(report.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == splits })
	if at < 0 {
		t.Fatalf("the report of a sweep that ended on its budget carries no size of %s: %+v", splits, report.Sizes)
	}
	switch size := report.Sizes[at]; {
	case size.Bytes != 123:
		t.Errorf("the report's size of %s is %d B, want the 123 of the three files in it", splits, size.Bytes)
	case size.BytesOverHour != 23:
		t.Errorf("the report's size of %s carries %d B untouched for more than an hour, want the 23 of the two files written before it: the report of a sweep that ended on its budget carries each size's two age splits", splits, size.BytesOverHour)
	case size.BytesOverDay != 3:
		t.Errorf("the report's size of %s carries %d B untouched for more than a day, want the 3 of the one file written before it: the report of a sweep that ended on its budget carries each size's two age splits", splits, size.BytesOverDay)
	}
	f.assertProtected()
	f.assertNothingHalfRemoved()
}

// assertNothingHalfRemoved holds every entry still on the volume to everything it was built
// with: an ending taken at an entry boundary leaves nothing part way removed.
func (f *trimFixture) assertNothingHalfRemoved() {
	f.t.Helper()
	examined := 0
	for _, entry := range f.entries {
		info, err := os.Lstat(entry.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			f.t.Error(err)
			continue
		}
		examined++
		if !info.IsDir() {
			continue
		}
		if inside := filepath.Join(entry.path, "b1", "out"); !f.exists(inside) {
			f.t.Errorf("%s is on the volume without the %s it was built with: an entry was left half removed", entry.path, inside)
		}
	}
	if examined == 0 {
		f.t.Fatal("no entry is left at all, so this case says nothing about what a half removal would look like")
	}
}

// The budget, a stop from the settings and the container's own stop each take effect at the
// next entry boundary once deleting has begun, or in the next entry's pre-delete re-read, before
// its removal begins: what was deleted stands, nothing is left half removed, and the container's
// stop lets run return at once rather than after the budget.
func TestTheThreeEndingsTakeEffectAtTheNextEntryBoundary(t *testing.T) {
	const before = 3 // deletions before the ending arrives

	// The budget is five minutes, and it is driven here from both sides rather than written
	// out: at an entry boundary the clock is advanced by exactly it, where the sweep must end,
	// and in the second case by a millisecond less, where it must not. R1 has it as "the
	// sweep's budget of five minutes": a longer one lets one sweep hold the process that is
	// PID 1 of the roles container past the five minutes the requirement promises, and a
	// shorter one cuts the sweep of a full volume — 25 to 40 seconds to order a 17 GiB build
	// cache alone — before it has deleted its way down to the low mark. The advance is this
	// case's own figure and not volumeSweepBudget, because a case staged from the constant it
	// tests moves with it and fails for no value of it (INFRA-2009, review round 6).
	for _, tc := range []struct {
		name    string
		advance time.Duration
		ends    bool
	}{
		{"the budget, spent at an entry boundary", 5 * time.Minute, true},
		{"the budget, a millisecond of it still to run", 5*time.Minute - time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 5)
			f.useVolume(steady(90))
			f.setSettings(trimSettings())
			spent := false
			f.clock.setHook(func(clock *fakeClock) {
				if spent || len(f.gone()) < before {
					return
				}
				spent = true
				clock.advance(tc.advance)
			})

			tr := f.trimmer()
			out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

			if !spent {
				t.Fatal("the case never reached the moment it stages")
			}
			if !tc.ends {
				// The sweep runs to its end and every entry goes, so there is nothing left to
				// hold to its parts: assertNothingHalfRemoved is asked of the ending alone,
				// which is the half of this case that stops a sweep in flight.
				if out.deletions != len(f.entries) {
					t.Errorf("the sweep deleted %d of the %d entries with %s of its budget still to run, want every one: %s",
						out.deletions, len(f.entries), time.Millisecond, out.reason)
				}
				if strings.Contains(out.reason, "budget") {
					t.Errorf("the sweep stopped because %q with %s of its budget still to run", out.reason, time.Millisecond)
				}
				return
			}
			if out.deletions != before {
				t.Errorf("the sweep deleted %d entries, want the %d it had made when the budget ran out", out.deletions, before)
			}
			if !strings.Contains(out.reason, "budget") {
				t.Errorf("the sweep stopped because %q, want the budget named", out.reason)
			}
			f.assertNothingHalfRemoved()
		})
	}

	t.Run("a stop from the settings", func(t *testing.T) {
		f := newTrimFixture(t, 5)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		stopped := false
		f.setOnSettings(func() {
			if stopped || len(f.gone()) < before {
				return
			}
			stopped = true
			f.setSettings(nil)
		})

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

		if !stopped {
			t.Fatal("the case never reached the moment it stages")
		}
		if out.deletions != before {
			t.Errorf("the sweep deleted %d entries, want the %d it had made when the settings stopped it", out.deletions, before)
		}
		if !out.settingsStop {
			t.Errorf("the sweep ended with %q, want the ending the interval acts on itself: no report, and the loop ends", out.reason)
		}
		if len(f.gone()) != before {
			t.Errorf("%d entries are gone, want the %d deletions to stand", len(f.gone()), before)
		}
		f.assertNothingHalfRemoved()
	})

	t.Run("the container's stop, which run returns on at once", func(t *testing.T) {
		f := newTrimFixture(t, 5)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		// The entry the sweep reaches after its third deletion, and whether its pre-delete
		// re-read has a tree to walk.
		next, err := os.Lstat(f.oldestFirst()[before].path)
		if err != nil {
			t.Fatal(err)
		}
		cancel, done := f.start(tr)
		stopped := false
		f.clock.setHook(func(_ *fakeClock) {
			if stopped || len(f.gone()) < before {
				return
			}
			stopped = true
			cancel()
		})

		f.tick("sweeping")
		select {
		case <-done:
		case <-time.After(testwait.Bounded(t, 20*time.Second, time.Second)):
			t.Fatal("run did not return on the container's stop: it is waiting out the sweep's budget")
		}
		if !stopped {
			t.Fatal("the case never reached the moment it stages")
		}
		if moved := f.clock.moved(); moved >= volumeSweepBudget {
			t.Errorf("the trimmer's clock moved %s before run returned, want less than the %s budget: the stop and not the budget ended it", moved, volumeSweepBudget)
		}
		// The boundary that reads the clock has already asked the context, so the stop reaches
		// the sweep in the entry after the third deletion, and no removal has begun there. A
		// directory's pre-delete re-read asks the context before it opens the directory
		// (newerThan), so the stop is taken there and that entry stands whole; a file's re-read
		// is one lstat that asks nothing, so its removal begins and the stop is taken at the
		// boundary after it. Either way an ending is never taken in the middle of a removal.
		want := before
		if !next.IsDir() {
			want = before + 1
		}
		if gone := len(f.gone()); gone != want {
			t.Errorf("%d entries are gone, want the %d deletions that stood when the stop was taken (the entry after the third is a directory: %t)", gone, want, next.IsDir())
		}
		if gone := len(f.gone()); gone >= len(f.entries) {
			t.Errorf("every one of the %d entries is gone: the sweep ran to its end rather than stopping", len(f.entries))
		}
		f.assertNothingHalfRemoved()
	})
}

// A stop that reaches the sweep while it re-reads an entry's age just before deleting it ends the
// sweep there, with that entry whole: the re-read walks the entry's tree (newerThan), which a role
// can grow as deep or as wide as it likes between the listing and the delete, and it asks the
// sweep's endings before each directory it opens, as the listing's walk does. The stop is the
// settings', staged at the second time the sweep asks them after its third deletion: the first is
// the boundary before the fourth entry, and the second is that entry's re-read, before its
// removal begins. Every entry is a directory, so every re-read has a tree to walk. The negative
// control is newerThan asking nothing, as it did at b7085e21: the fourth entry was deleted and
// the stop was taken at the boundary after it.
func TestAStopInAnEntrysPreDeleteReReadEndsTheSweepWithTheEntryWhole(t *testing.T) {
	const before = 3 // deletions before the stop arrives
	f := newTrimFixture(t, 0)
	root := f.roots[3]
	for n := range 6 {
		f.addTree(filepath.Join(root.Path, fmt.Sprintf("tool-%02d", n)), root.Entry)
	}
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	asked, stopped := 0, false
	f.setOnSettings(func() {
		if stopped || len(f.gone()) < before {
			return
		}
		if asked++; asked == 2 {
			stopped = true
			f.setSettings(nil)
		}
	})

	out := f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

	if !stopped {
		t.Fatalf("the case never reached the moment it stages: the sweep asked the settings %d times after its third deletion (%s)", asked, out.reason)
	}
	if out.deletions != before || len(f.gone()) != before {
		t.Errorf("the sweep deleted %d entries and %d are gone, want the %d it had made when the stop reached the next entry's re-read", out.deletions, len(f.gone()), before)
	}
	if next := f.oldestFirst()[before]; !f.exists(next.path) {
		t.Errorf("the sweep deleted %s, the entry whose re-read the stop reached before its removal began", next.path)
	}
	if !out.settingsStop {
		t.Errorf("the sweep ended with %q, want the settings' stop it met in the re-read", out.reason)
	}
	f.assertNothingHalfRemoved()
}

// A removal that fails part way is reported and passed over, and the part-removed entry a later
// sweep meets is an ordinary entry of its root: the removal moved its modification time, so it
// is kept until one minimum age has passed and is deleted then.
func TestARemovalThatFailsPartWayIsReportedAndLeavesAnOrdinaryEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which removes a directory it has no write permission on: a removal that fails part way cannot be staged for it")
	}
	f := newTrimFixture(t, 2)
	entry := f.oldestDirectory()
	blocked := filepath.Join(entry.path, "b2")
	mkdirFixture(t, blocked)
	writeAged(t, filepath.Join(blocked, "out"), entry.at)
	for _, path := range []string{blocked, entry.path} {
		if err := os.Chtimes(path, entry.at, entry.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()

	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
	if f.logged("failed part way") != 1 {
		t.Errorf("the trimmer reported %d removals that failed part way, want 1; it wrote %q", f.logged("failed part way"), f.linesWritten())
	}
	if !f.exists(entry.path) {
		t.Fatalf("%s is gone, want the part-removed entry left where the removal stopped", entry.path)
	}
	if f.exists(filepath.Join(entry.path, "b1")) {
		t.Errorf("%s still holds b1, so the removal did not get part way and this case proves nothing", entry.path)
	}
	if want := len(f.entries) - 1; out.deletions != want {
		t.Errorf("the sweep deleted %d entries, want the %d it could: the failure is passed over and the sweep goes on", out.deletions, want)
	}

	// The second sweep meets it as an ordinary entry, and the removal moved its time: it is
	// inside the minimum age, so nothing is tried again.
	second := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
	if second.deletions != 0 || f.logged("failed part way") != 1 {
		t.Errorf("the second sweep deleted %d entries and reported %d failures, want it to pass over an entry whose time the removal moved",
			second.deletions, f.logged("failed part way"))
	}

	// One minimum age later, with what blocked the removal gone, it is deleted like any other.
	if err := os.Chmod(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	aged := f.clock.at.Add(-2 * time.Hour)
	for _, path := range []string{filepath.Join(blocked, "out"), blocked, entry.path} {
		if err := os.Chtimes(path, aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	third := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
	if third.deletions != 1 || f.exists(entry.path) {
		t.Errorf("the third sweep deleted %d entries and left %s, want the part-removed entry deleted once it had aged past the minimum", third.deletions, entry.path)
	}
}

// The sweep's line for a removal that failed part way names the entry and the removal's error
// with every name a role chose quoted, so a name holding a newline is written with it escaped and
// the line stays one line of the pod's log, as every line of the trimmer's is behind its prefix.
// The entry here is an immediate child of bun's install cache whose name holds a newline and a
// line of text after it, holding a directory made 0555 whose file cannot be unlinked. The
// negative control is the line and removalFailure writing the names as they are, which put the
// role's line of text in the log as a line of its own.
func TestARemovalThatFailsPartWayIsLoggedWithTheRoleChosenNameQuoted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which removes a directory it has no write permission on: a removal that fails part way cannot be staged for it")
	}
	f := newTrimFixture(t, 1)
	root := f.roots[3]
	if root.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", root)
	}
	entry := f.addTree(filepath.Join(root.Path, "tool\nforged: a line the role wrote"), root.Entry)
	blocked := filepath.Join(entry.path, "b1")
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()

	tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
	var failed []string
	for _, line := range f.linesWritten() {
		if strings.Contains(line, "\n") {
			t.Errorf("the trimmer wrote %q, which holds a newline of the name a role chose", line)
		}
		if strings.Contains(line, "failed part way") {
			failed = append(failed, line)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("the trimmer reported %d removals that failed part way, want the one of %q; it wrote %q", len(failed), entry.path, f.linesWritten())
	}
	if want := "removing " + strconv.Quote(entry.path) + " failed part way"; !strings.Contains(failed[0], want) {
		t.Errorf("the trimmer wrote %q, want it to name the entry as %s", failed[0], want)
	}
}

// The sweep's line for an entry whose age could not be read names the entry and the guard's error
// with every name a role chose quoted, so a name holding a newline is written with it escaped and
// the line stays one line of the pod's log (quotedPathError, removalFailure). The entry is an
// immediate child of bun's install cache whose name holds a newline and a line of text after it,
// and its child b1 is a directory the age read cannot pass: made 0000, its open fails
// (openElementAt), and made 0400, it opens and its listing fails (within). The guard's error
// names b1 from the entry, by the entry's name and b1's (walkEntries), as the deleter names what
// it failed on. The negative controls are each of those two answering the fs.PathError it
// raised, which put the role's line of text in the log as a line of its own, "forged: a line the
// role wrote/b1: permission denied", for the child of that mode.
func TestAnAgeThatCannotBeReadIsLoggedWithTheRoleChosenNameQuoted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which opens and lists a directory whatever its mode: an age read that fails cannot be staged for it")
	}
	for _, child := range []struct {
		what string
		mode os.FileMode
	}{
		{what: "a child that cannot be opened", mode: 0},
		{what: "a child that cannot be listed", mode: 0o400},
	} {
		t.Run(child.what, func(t *testing.T) {
			f := newTrimFixture(t, 1)
			root := f.roots[3]
			if root.Entry != ImmediateChildren {
				t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache, whose entries are its immediate children", root)
			}
			entry := f.addTree(filepath.Join(root.Path, "tool\nforged: a line the role wrote"), root.Entry)
			blocked := filepath.Join(entry.path, "b1")
			if err := os.Chmod(blocked, child.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
			f.useVolume(steady(90))
			f.setSettings(trimSettings())
			tr := f.trimmer()

			tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
			var unread []string
			for _, line := range f.linesWritten() {
				for _, written := range strings.Split(line, "\n") {
					if strings.HasPrefix(written, "forged:") {
						t.Errorf("the trimmer wrote %q as a line of its own, which is text of the name a role chose", written)
					}
				}
				if strings.Contains(line, "the age of "+strconv.Quote(entry.path)) {
					unread = append(unread, line)
				}
			}
			if len(unread) != 1 {
				t.Fatalf("the trimmer reported %d entries whose age could not be read, want the one of %q; it wrote %q", len(unread), entry.path, f.linesWritten())
			}
			if strings.Contains(unread[0], "\n") {
				t.Errorf("the trimmer wrote %q, whose error holds a newline of the name a role chose", unread[0])
			}
			if want := "openat " + strconv.Quote(filepath.Base(entry.path)+"/b1") + ": " + unix.EACCES.Error(); !strings.HasSuffix(unread[0], want) {
				t.Errorf("the trimmer wrote %q, want the guard's error to name the child as %s", unread[0], want)
			}
		})
	}
}

// No sweep starts while one is running, and the next reading comes one interval after the sweep
// ends rather than one interval after it began: the trimmer makes a new interval clock when the
// sweep returns, so a sweep that ran for ten intervals is not followed by ten readings at once.
func TestNoSweepStartsWhileOneRunsAndTheNextReadingIsAnIntervalAfterItEnds(t *testing.T) {
	f := newTrimFixture(t, 30)
	reached, release := make(chan struct{}), make(chan struct{})
	f.setOnRead(func(call int) {
		if call != 2 {
			return
		}
		close(reached)
		<-release
	})
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	testwait.Eventually(t, "the trimmer to start its interval clock", func() bool { return f.ticks.count() > 0 })
	if !f.ticks.offer(t, 0, testwait.Bounded(t, 10*time.Second, time.Second)) {
		f.t.Fatal("the trimmer took no tick on the clock it started with")
	}
	<-reached

	// A tick while the sweep runs is taken by nobody: the loop is inside the sweep.
	if f.ticks.offer(t, 0, 300*time.Millisecond) {
		t.Error("the trimmer took a second tick while a sweep was running, want no sweep to overlap another")
	}
	close(release)
	testwait.Eventually(t, "the sweep to end", func() bool { return f.logged("the sweep ended") == 1 })

	// The clock it waited on before the sweep is stopped, and the new one it made after the
	// sweep is what the next reading comes on.
	if f.ticks.offer(t, 0, 300*time.Millisecond) {
		t.Error("the trimmer is still waiting on the interval clock it had before the sweep, want the next reading one interval after the sweep ended")
	}
	if made := f.ticks.count(); made < 2 {
		t.Fatalf("the trimmer made %d clocks, want a new interval clock after the sweep", made)
	}
	live, running := f.ticks.live(f.interval())
	if !running {
		t.Fatal("the trimmer is waiting on no clock at all after its sweep")
	}
	if period := f.ticks.periodOf(live); period != settingsInterval(*trimSettings()) {
		t.Errorf("the clock the trimmer now waits on runs every %s, want the interval %s", period, settingsInterval(*trimSettings()))
	}
	f.tick("nothing to sweep")
	if sweeps := f.logged("sweeping"); sweeps != 1 {
		t.Errorf("the trimmer swept %d times, want the one sweep: the reading after it is below the high mark", sweeps)
	}
}

// A sweep that freed less than one point of the volume starts no new sweep for the next ten
// intervals, counted from the end of that sweep, and the interval reading continues through the
// pause: the reading is what ends an episode, and a trimmer that stopped reading would never
// tell the daemon the volume had recovered.
func TestASweepThatFreesLessThanAPointStartsNoSweepForTenIntervals(t *testing.T) {
	f := newTrimFixture(t, 1) // four entries, four tenths of a point
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	f.tick("the sweep ended")
	if f.logged("no new sweep for the next") != 1 {
		t.Fatalf("the trimmer did not engage the pause after a sweep that freed four tenths of a point; it wrote %q", f.linesWritten())
	}
	for at := range volumeQuietIntervals {
		f.tick("no sweep for another")
		if sweeps := f.logged("sweeping"); sweeps != 1 {
			t.Fatalf("the trimmer swept %d times by the %dth interval of the pause, want no new sweep until the pause is out", sweeps, at+1)
		}
	}
	// The reading continued through every interval of the pause: one for the sweep's interval,
	// one the sweep itself made at its end, and one for each interval of the pause.
	if reads, want := f.readCount(), 2+volumeQuietIntervals; reads != want {
		t.Errorf("the trimmer read the volume %d times, want %d: the interval reading continues through the pause", reads, want)
	}
	if readings, want := f.logged("the volume is"), 1+volumeQuietIntervals; readings != want {
		t.Errorf("the trimmer logged %d interval readings, want %d: a person reading the pod's log must see every one", readings, want)
	}

	f.tick("the sweep ended")
	if sweeps := f.logged("sweeping"); sweeps != 2 {
		t.Errorf("the trimmer swept %d times, want a second sweep on the interval after the pause ran out", sweeps)
	}
	f.assertPrefixed()
	if f.logged("started: trimmer") != 1 {
		t.Errorf("the trimmer did not log its start with the settings in force; it wrote %q", f.linesWritten())
	}
}

// The pause is ten intervals, written here as the number and not as the constant.
//
// The case above drives `volumeQuietIntervals` intervals and compares counts derived from the
// same constant, so a copy whose pause is nought drives no interval, compares no count and
// stays green while the trimmer sweeps on every interval of a volume it cannot bring down —
// the listing of a 17 GiB build cache once a minute in the process that is PID 1 of the roles
// container, which is the cost rule 2 of the design exists to stop (dispatch://INFRA-2009 spec,
// "When the low mark cannot be reached"). Measured by the tester at
// 2ba2c7ad44b174963ff159cc66d4bfecf9d6d300: with volumeQuietIntervals set to 0 the package's
// own suite was green.
//
// So this case writes the ten out: the line the trimmer logs must name ten intervals, ten
// intervals must pass with no second sweep, and the eleventh must sweep.
func TestThePauseAfterASweepThatFreedUnderAPointIsTenIntervals(t *testing.T) {
	f := newTrimFixture(t, 1) // four entries, four tenths of a point
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	f.tick("the sweep ended")
	if f.logged("no new sweep for the next 10 intervals") != 1 {
		t.Fatalf("the trimmer did not announce a ten-interval pause after a sweep that freed under a point; it wrote %q", f.linesWritten())
	}
	for at := 1; at <= 10; at++ {
		f.tick("no sweep for another")
		if sweeps := f.logged("sweeping"); sweeps != 1 {
			t.Fatalf("the trimmer swept %d times by interval %d of the pause, want the one sweep before it", sweeps, at)
		}
	}
	f.tick("the sweep ended")
	if sweeps := f.logged("sweeping"); sweeps != 2 {
		t.Errorf("the trimmer swept %d times, want the second sweep on the eleventh interval after the first", sweeps)
	}
}

// What engages that pause is a sweep that freed under one point of the volume: one that freed
// nine tenths of a point pauses, and one that freed eleven tenths does not. R1 has it as "A
// sweep that frees less than one percent of the volume starts no new sweep for the next ten
// intervals". A higher figure engages the pause on a sweep that did free real space, so a full
// volume waits ten intervals with caches the trimmer could have deleted; a lower one lets a
// trimmer that can free nothing list the volume's whole reclaimable set once an interval in
// the process that is PID 1 of the roles container. A sweep that freed exactly one point is not
// under it and engages no pause: the two rows a tenth either side of the point are green with a
// pause engaged at the point itself, so the third row stands on it, and every row first reads
// the figure the model's two readings give on the side it stands on.
//
// Both halves are staged from the volume the reading models and never from
// volumeSweepMinFreedPoints — one entry carries every byte the sweep can free and the others
// carry none — so the figure the trimmer computed for itself is what decides, as the ten
// intervals are counted out in the case above (INFRA-2009, review round 6).
func TestThePauseIsEngagedByASweepUnderAPointAndNotByOneOverIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		points float64
		pauses bool
	}{
		{"a sweep that freed nine tenths of a point", 0.9, true},
		{"a sweep that freed eleven tenths of a point", 1.1, false},
		{"a sweep that freed exactly one point", 1.0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The sweep's first reading and its last, as the model below gives them.
			left := percentBytes(90) - percentBytes(tc.points)
			freed := usageAt(90).Percent() - (volumeUsage{UsedBytes: left, AvailableBytes: fixtureVolumeBytes - left}).Percent()
			if (tc.points == 1 && freed != 1) || (freed < 1) != tc.pauses {
				t.Fatalf("the model's readings free %v points, which is not the side of the one point this row stands on", freed)
			}
			f := newTrimFixture(t, 1) // four entries, of which one holds the bytes
			for at, entry := range f.entries {
				entry.bytes = 0
				if at == 0 {
					entry.bytes = percentBytes(tc.points)
				}
			}
			f.useVolume(f.modelled(90))
			f.setSettings(trimSettings())
			f.start(f.trimmer())

			f.tick("the sweep ended")
			if gone := len(f.gone()); gone != len(f.entries) {
				t.Fatalf("the sweep deleted %d of the %d entries, want every one: the points it freed are what this case reads", gone, len(f.entries))
			}
			paused := f.logged("no new sweep for the next")
			switch {
			case tc.pauses && paused != 1:
				t.Errorf("the trimmer wrote %d lines engaging the pause after a sweep that freed %.1f points of the volume, want the one: a sweep that freed under a point has no more it can free, and listing the volume's whole reclaimable set again next interval costs the pod minutes for nothing; it wrote %q",
					paused, tc.points, f.linesWritten())
			case !tc.pauses && paused != 0:
				t.Errorf("the trimmer engaged the pause after a sweep that freed %.1f points of the volume, want none: that sweep freed real space, and ten intervals of waiting leave a full volume holding caches the trimmer could have deleted; it wrote %q",
					tc.points, f.linesWritten())
			}
			f.assertPrefixed()
		})
	}
}

// A volume the trimmer cannot bring under the high mark is swept at least once every eleven
// intervals, since the pause is ten. A volume whose first sweep deletes every eligible entry is
// swept twice and then once every eleven intervals only while nothing new ages past the minimum
// age between its sweeps: the first sweep frees what it can, the second finds nothing eligible and
// so frees under a point, and that engages the ten-interval pause, but a later sweep that finds
// entries newly past the minimum age and frees a point or more engages none, and the next interval
// sweeps again. A sweep that frees a point or more engages no pause, so one whose roles refill
// what each sweep frees is swept every interval. The case drives the first three sweeps of that
// run, on a tree whose entries are all eligible from the start and to which nothing is added.
func TestTheSweepAfterOneThatTookEveryEligibleEntryPausesTenIntervals(t *testing.T) {
	f := newTrimFixture(t, 10) // forty entries, four points, which leaves the volume at 86%
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	f.tick("the sweep ended")
	if f.logged("no new sweep for the next") != 0 {
		t.Fatalf("the first sweep engaged the pause although it freed four points; the trimmer wrote %q", f.linesWritten())
	}
	if gone := len(f.gone()); gone != len(f.entries) {
		t.Fatalf("the first sweep deleted %d of %d entries, want every eligible one: the second sweep must find nothing", gone, len(f.entries))
	}

	f.tick("the sweep ended")
	if f.logged("no new sweep for the next") != 1 {
		t.Errorf("the second sweep, which found nothing eligible, did not engage the pause; the trimmer wrote %q", f.linesWritten())
	}
	for range volumeQuietIntervals {
		f.tick("no sweep for another")
	}
	if sweeps := f.logged("sweeping"); sweeps != 2 {
		t.Fatalf("the trimmer swept %d times through the pause, want the two before it", sweeps)
	}
	f.tick("the sweep ended")
	if sweeps := f.logged("sweeping"); sweeps != 3 {
		t.Errorf("the trimmer swept %d times, want one more on the eleventh interval after the second sweep", sweeps)
	}
}

// A panic in the trimmer's own work is recovered and logged with its stack, the interval is
// passed over, and three in a row stop the trimmer for the life of the launcher process with
// one last report saying so. The recovery is a departure from this daemon's practice, whose
// production code recovers no other panic of its own work but in two cases: leakguard's compile
// recovers one only to turn regexp.MustCompile's into an error, and the memory guard recovers its
// loop (Guard.step) and its scan (scanLargest), as the microVM guest watch recovers its loop
// (Guest.step) and its wake's reader (readModified), for the trimmer's reason: that code runs in
// PID 1 of the pod. This goroutine runs in the process that is PID 1 of the roles container, so
// its crash costs six live roles their round over a cache sweep.
func TestAPanicIsRecoveredAndThreeInARowSwitchTheTrimmerOff(t *testing.T) {
	f := newTrimFixture(t, 1)
	f.setOnRead(func(call int) {
		if call >= 2 && call <= 4 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.useVolume(steady(50))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	f.tick("nothing to sweep")
	// Three in a row and not two: R1 has the trimmer contain its own faults and "after three in
	// a row it stays off and sends one last report saying so". The three are counted out here
	// rather than read off volumeFaultLimit, and both sides of the number are read — the
	// trimmer is still running after the second, and only the third switches it off — so a
	// limit moved either way turns this case red (INFRA-2009, review round 6). A smaller limit
	// switches the pod's only protection off over a panic or two that an interval apart may
	// never repeat; a larger one keeps a trimmer that panics on every interval running in the
	// process that is PID 1 of the roles container while the volume it no longer guards fills.
	const faultLimit = 3
	for at := range faultLimit {
		f.tick("recovered a panic")
		if faults := f.logged("recovered a panic"); faults != at+1 {
			t.Fatalf("the trimmer logged %d recovered panics after %d, want one for each", faults, at+1)
		}
		if off := f.logged("off for the life of this launcher process"); at < faultLimit-1 && off != 0 {
			t.Fatalf("the trimmer switched itself off after %d recovered panics in a row, want it to go on until %d: an interval that panicked once is an interval passed over and not a pod left unguarded", at+1, faultLimit)
		}
	}
	// Waited for rather than read at once: the tick above returns when the third fault line is
	// written, and the trimmer's own goroutine writes this one after it, so a bare read here is
	// a race the case loses about once in thirty runs (measured 2026-11-12).
	testwait.Eventually(t, "the trimmer to log that it is off for good", func() bool {
		return f.logged("off for the life of this launcher process") == 1
	})
	for _, line := range f.linesWritten() {
		if strings.Contains(line, "recovered a panic") && !strings.Contains(line, "volumetrim.go") {
			t.Errorf("the fault line %q carries no stack, want the stack of the panic", line)
		}
	}

	reports := f.reports()
	if len(reports) != 1 {
		t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off", len(reports))
	}
	report := reports[0]
	switch {
	case !report.SwitchedOff:
		t.Error("the last report does not say the trimmer switched itself off")
	case len(report.Faults) != faultLimit:
		t.Errorf("the last report carries %d faults, want the %d in a row that stopped it", len(report.Faults), faultLimit)
	case len(report.Sizes) != 0:
		t.Errorf("the last report carries %d sizes, want none: the volume may be healthy and a walk would cost the pod a minute for nothing", len(report.Sizes))
	case report.UsedBytes != int64(percentBytes(50)):
		t.Errorf("the last report carries %d used bytes, want the %d of the last reading the trimmer took", report.UsedBytes, percentBytes(50))
	case report.Trimmer != tr.id:
		t.Errorf("the last report names the trimmer %q, want %q", report.Trimmer, tr.id)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the last report: %v", err)
	}
	for _, fault := range report.Faults {
		if !strings.Contains(fault, "volumetrim.go") {
			t.Errorf("the fault %q carries no stack", fault)
		}
	}

	t.Run("a fault is carried into the next report", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) {
			if call == 1 {
				panic("the volume reader fell over on the first reading")
			}
		})
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		f.start(tr)

		f.tick("recovered a panic")
		f.tick("the sweep ended")
		testwait.Eventually(t, "the report of the sweep after the fault", func() bool { return len(f.reports()) == 1 })
		report := f.reports()[0]
		if len(report.Faults) != 1 {
			t.Errorf("the report carries %d faults, want the one the trimmer recovered before it", len(report.Faults))
		}
		if report.SwitchedOff {
			t.Error("the report says the trimmer switched itself off after one fault, want it to go on")
		}
	})
}

// The report of a trimmer that switched itself off triggers no size walk: it carries the faults
// and the last reading and no sizes (switchOff), because the volume may be perfectly healthy and a
// walk would cost the pod a minute for a number nobody asked for. Each walk takes its budget's
// clock from the trimmer's ticker seam (sizeWalk.after), so the walks are counted there, and the
// targets each measured at the measurement seam beside it. The first sweep reports and walks, which
// is the control that a report needing sizes is counted. An hour and a minute then pass on the
// trimmer's clock, so a report made after them would walk afresh rather than carry that walk's
// numbers (trimmer.sizes), and three panics in a row switch the trimmer off. The negative control
// is a switchOff that asks for sizes as a sweep's report does: it walks the volume a second time,
// which this case counts whether or not the report then carries what the walk measured.
func TestTheSwitchedOffReportTriggersNoSizeWalk(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: the first sweep frees nothing, reports and pauses
	f.setOnRead(func(call int) {
		if call >= 2 && call <= 4 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	var measuredTargets atomic.Int32
	measure := tr.measure
	tr.measure = func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		measuredTargets.Add(1)
		return measure(ctx, dir, name, at)
	}
	walks := func() int {
		count := 0
		for at := range f.ticks.count() {
			if f.ticks.periodOf(at) == volumeWalkBudget {
				count++
			}
		}
		return count
	}
	_, done := f.start(tr)

	f.tick("the sweep ended")
	testwait.Eventually(t, "the first sweep's report", func() bool { return len(f.reports()) == 1 })
	first := f.reports()[0]
	if walked, targets := walks(), measuredTargets.Load(); walked != 1 || targets == 0 || len(first.Sizes) == 0 {
		t.Fatalf("the first sweep's report made %d walks measuring %d targets and carries %d sizes, want one walk with sizes: the case would count no walk at all",
			walked, targets, len(first.Sizes))
	}

	f.clock.advance(volumeWalkInterval + time.Minute)
	if since := f.clock.now().Sub(first.SizesAt); since < volumeWalkInterval {
		t.Fatalf("the first walk is %s old, want at least the %s after which a report walks afresh", since, volumeWalkInterval)
	}
	targetsBefore := measuredTargets.Load()
	for range volumeFaultLimit {
		f.tick("recovered a panic")
	}
	awaitClosed(t, done, "the switched-off trimmer's loop to return once its last report is sent")

	reports := f.reports()
	if len(reports) != 2 || !reports[1].SwitchedOff {
		t.Fatalf("the trimmer sent %d reports, want the first sweep's and the switched-off trimmer's last: %+v", len(reports), reports)
	}
	if walked := walks(); walked != 1 {
		t.Errorf("the trimmer made %d size walks, want the one of the first sweep's report: the switched-off report walked the volume", walked)
	}
	if targets := measuredTargets.Load(); targets != targetsBefore {
		t.Errorf("the trimmer measured %d targets after the first sweep's report, want none: the switched-off report walked the volume", targets-targetsBefore)
	}
	last := reports[1]
	if len(last.Sizes) != 0 || last.SizesOmitted != 0 || !last.SizesAt.IsZero() || last.SizesPartial || len(last.SizesMissing) != 0 {
		t.Errorf("the switched-off report carries %d sizes (%d omitted) read at %s, partial %t, missing %v, want no size walk at all in it",
			len(last.Sizes), last.SizesOmitted, last.SizesAt, last.SizesPartial, last.SizesMissing)
	}
}

// R1 has the trimmer, "after three in a row", stay off "and send one last report saying so", and
// R2 has it hold its report while the bound connection is down "until that connection is up
// again". So the report of a trimmer that switched itself off with no connection up is held, and
// sent once a connection takes it; the trimmer's loop returns then, or on the container's stop,
// or on an answer with no settings, which discards it as it discards any report the trimmer holds.
func TestTheSwitchedOffReportIsHeldUntilTheConnectionIsUp(t *testing.T) {
	switchOffDisconnected := func(t *testing.T) (*trimFixture, *trimmer, <-chan struct{}) {
		t.Helper()
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
		f.useVolume(steady(50))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		tr := f.trimmer()
		_, done := f.start(tr)
		for range volumeFaultLimit {
			f.tick("recovered a panic")
		}
		testwait.Eventually(t, "the trimmer to hold its last report", func() bool {
			return f.logged("the report could not be sent (no daemon connection is up to carry it); holding it for the next attempt") == 1
		})
		return f, tr, done
	}

	t.Run("it is sent once the connection is up", func(t *testing.T) {
		f, tr, done := switchOffDisconnected(t)
		// Three attempts while the connection is still down: nothing is sent, the reason is
		// said once, and the loop has not returned.
		offerSettingsTick(t, f)
		offerSettingsTick(t, f)
		offerSettingsTick(t, f)
		select {
		case <-done:
			t.Fatal("the trimmer's loop returned with its last report still unsent")
		default:
		}
		if said := f.logged("the last report of the switched-off trimmer could not be sent"); said != 1 {
			t.Errorf("the trimmer wrote %d lines for the failed attempts of a connection that stayed down, want one", said)
		}
		if sent := len(f.reports()); sent != 0 {
			t.Fatalf("the trimmer sent %d reports with no connection up", sent)
		}

		// The attempt the last tick started may still be running, and the connection can come up
		// under it: whichever attempt finds it up sends the report.
		f.refuseSends(nil)
		offerSettingsTicksUntil(t, f, "the trimmer's loop to return once its last report is sent", func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		reports := f.reports()
		if len(reports) != 1 {
			t.Fatalf("the trimmer sent %d reports once the connection was up, want its one last report", len(reports))
		}
		report := reports[0]
		switch {
		case !report.SwitchedOff:
			t.Errorf("the report %+v does not say the trimmer switched itself off", report)
		case len(report.Faults) != volumeFaultLimit:
			t.Errorf("the last report carries %d faults, want the %d in a row that stopped it", len(report.Faults), volumeFaultLimit)
		case report.Trimmer != tr.id:
			t.Errorf("the last report names the trimmer %q, want %q", report.Trimmer, tr.id)
		}
		if err := report.Validate(); err != nil {
			t.Errorf("the daemon would refuse the last report: %v", err)
		}
	})

	t.Run("an answer with no settings discards it", func(t *testing.T) {
		f, _, done := switchOffDisconnected(t)
		f.setSettings(nil)
		f.refuseSends(nil)
		offerSettingsTick(t, f)
		awaitClosed(t, done, "the trimmer's loop to return once its last report is discarded")
		if f.logged("the last report of the switched-off trimmer is discarded") != 1 {
			t.Errorf("the trimmer did not say it discarded its last report; it wrote %q", f.linesWritten())
		}
		if sent := len(f.reports()); sent != 0 {
			t.Errorf("the trimmer sent %d reports after an answer with no settings, want none", sent)
		}
	})
}

// R1 has the switched-off trimmer "send one last report saying so", and that report is the last
// the daemon hears of it (switchOff). So a report the trimmer still holds as it switches off goes
// into the last report, its faults and refused /proc reads kept or counted under the bound
// (replace), and is held no more: the last report is the one report the switched-off trimmer
// sends, and the one its later attempts try (deliverLast). Each subtest holds the first sweep's
// report, which carries a fault and a refused read, refuses its resend before each next reading,
// and makes three readings in a row panic; the connection then takes the last report, and takes
// the held one too in the first subtest and refuses it in the second. At 9588bff0 switchOff
// handed its report to deliver, whose flush tried the held report first: in the first subtest the
// daemon was sent two reports, the held one first, and in the second the held report stayed held
// once the last report was sent, and deliverLast tried it as the last report every
// volumeSettingsWait, so the trimmer never ended.
func TestAReportHeldAtTheSwitchOffGoesIntoTheLastReportAndIsSentNoOtherWay(t *testing.T) {
	const (
		refused = "process 7: /proc/7/fd: permission denied"
		fell    = "the volume reader fell over"
	)
	// switchOffHolding switches a trimmer off while it holds the first sweep's report. Every send
	// is refused until the third reading panic, and from then on each goes to connection.
	switchOffHolding := func(t *testing.T, connection func(f *trimFixture, report shimwire.LauncherVolumeReport) error) {
		t.Helper()
		f := newTrimFixture(t, 10) // forty entries, four points: the first sweep leaves 86% and sets no pause
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		var up atomic.Bool
		tr.send = func(report shimwire.LauncherVolumeReport) error {
			if !up.Load() {
				return errors.New("no daemon connection is up to carry it")
			}
			return connection(f, report)
		}
		// What the first sweep's report carries: a fault and a refused read, as the readings before
		// it and its own sweep hand them to the trimmer (fault, carryRefused).
		tr.fault("a panic before the first sweep", []byte("goroutine 1 [running]:\nthe stack"))
		tr.carryRefused([]string{refused})
		_, done := f.start(tr)
		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report its send refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})

		var panics atomic.Int32
		f.setOnRead(func(int) {
			if panics.Add(1) == volumeFaultLimit {
				up.Store(true)
			}
			panic(fell)
		})
		for range volumeFaultLimit {
			f.tick("recovered a panic")
		}
		awaitClosed(t, done, "the trimmer's loop to return once its last report is sent, holding nothing")

		reports := f.reports()
		if len(reports) != 1 || !reports[0].SwitchedOff {
			var sent []string
			for _, report := range reports {
				sent = append(sent, fmt.Sprintf("%q (switched off: %t)", report.StopReason, report.SwitchedOff))
			}
			t.Fatalf("the switched-off trimmer sent %d reports, %s, want its one last report with the held one in it", len(reports), strings.Join(sent, ", "))
		}
		last := reports[0]
		if !slices.Equal(last.RefusedReads, []string{refused}) || last.RefusedReadsOmitted != 0 {
			t.Errorf("the last report names the refused reads %q and %d left out, want the held report's %q", last.RefusedReads, last.RefusedReadsOmitted, refused)
		}
		if len(last.Faults) != volumeFaultLimit || last.FaultsOmitted != 1 {
			t.Errorf("the last report carries the faults %q and counts %d more, want the %d reading panics and the held report's fault counted as left out", last.Faults, last.FaultsOmitted, volumeFaultLimit)
		}
		for at, fault := range last.Faults {
			if !strings.HasPrefix(fault, fell) {
				t.Errorf("fault %d of the last report is %q, want a reading panic: the bound keeps the newest", at+1, fault)
			}
		}
		if err := last.Validate(); err != nil {
			t.Errorf("the daemon would refuse the last report: %v", err)
		}
		if _, waiting := f.ticks.live(volumeSettingsWait); waiting {
			t.Error("the trimmer still waits on a clock to try a report after its last report was sent")
		}
	}

	t.Run("the connection would take the held report too", func(t *testing.T) {
		switchOffHolding(t, func(f *trimFixture, report shimwire.LauncherVolumeReport) error {
			return f.send(report)
		})
	})

	t.Run("the connection refuses the held report", func(t *testing.T) {
		switchOffHolding(t, func(f *trimFixture, report shimwire.LauncherVolumeReport) error {
			if !report.SwitchedOff {
				return errors.New("the connection refuses the held report")
			}
			return f.send(report)
		})
	})
}

// R1 has the trimmer contain its own faults, and the trimmer's goroutine runs in the launcher
// process that is PID 1 of the roles container: a panic that escapes it ends the container and
// every role in it. tick recovers its own interval; these are the paths the goroutine runs outside
// that recovery (run), each made to panic through a seam the trimmer already takes, its send or its
// logger. At e6725f56 each of the first four ended this test process (INFRA-2009, review round 34).
//
//   - The resend of a held report in the wait for settings (awaitSettings' flush) is a fault as
//     tick's are: written with its stack, counted in the sweep row toward three in a row, since a
//     held report is a sweep's report (flush), and carried into the next report, and the trimmer
//     goes back to its wait and on to its interval loop.
//   - The line tick writes of a fault it recovered runs in tick's deferred handler, so its panic is
//     raised while one is being handled; it is counted as the next fault in the row of the fault
//     it was writing, here the reading row, and the third in that row switches the trimmer off
//     with all three in its last report.
//   - The last report of the switched-off trimmer, as switchOff makes it or as deliverLast tries it
//     again, ends the trimmer with its panic written and its stack, and the report is not tried again.
//   - A logger that panics on every line of a fault and on the line run writes of the last report's
//     panic leaves each of those lines to fall over inside run's own contain, where it is dropped:
//     the trimmer still counts its faults, switches itself off and ends, and the test process
//     stands. With either of those two contains taken out the panic leaves the trimmer's goroutine
//     and ends this test process (INFRA-2009, review round 36).
func TestAPanicOutsideTheIntervalIsContainedAndCounted(t *testing.T) {
	// written is the one line the trimmer wrote holding want.
	written := func(t *testing.T, f *trimFixture, want string) string {
		t.Helper()
		var lines []string
		for _, line := range f.linesWritten() {
			if strings.Contains(line, want) {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 {
			t.Fatalf("the trimmer wrote %d lines holding %q, want one: %q", len(lines), want, f.linesWritten())
		}
		return lines[0]
	}

	t.Run("the resend of a held report in the wait for settings", func(t *testing.T) {
		f := newTrimFixture(t, 10) // forty entries, four points: the first sweep leaves 86% and sets no pause
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		tr := f.trimmer()
		const fell = "the connection fell over resending the held report"
		var falling atomic.Bool
		tr.send = func(report shimwire.LauncherVolumeReport) error {
			if falling.Load() {
				panic(fell)
			}
			return f.send(report)
		}
		_, done := f.start(tr)
		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report its send refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})

		// Settings it refuses end the interval loop, and the wait tries the held report once it
		// has said why it waits (awaitSettings): that resend falls over.
		falling.Store(true)
		wide := *trimSettings()
		wide.HighPercent = shimwire.VolumeMarkMax + 1
		f.setSettings(&wide)
		f.tick("refusing the settings")
		testwait.Eventually(t, "the panic of the resend to be recovered", func() bool {
			return f.logged("recovered a panic") == 1
		})
		if line := written(t, f, "recovered a panic"); !strings.Contains(line, "(1 in a row of sweeps and reports)") || !strings.Contains(line, fell) || !strings.Contains(line, "(*trimmer).flush") {
			t.Errorf("the trimmer wrote %q, want the panic of the resend counted as the first fault in the sweep row, with the stack it was raised on", line)
		}
		select {
		case <-done:
			t.Fatal("the trimmer's loop returned after one fault, want it waiting for settings it can run")
		default:
		}

		// Settings it can run start the interval loop again: the held report goes at its first
		// interval, and the report of that interval's sweep carries the fault.
		falling.Store(false)
		f.refuseSends(nil)
		f.setSettings(trimSettings())
		offerSettingsTicksUntil(t, f, "the interval loop to start again", func() bool { return f.logged("started:") == 2 })
		f.tick("the sweep ended")
		testwait.Eventually(t, "the held report and the next sweep's to be sent", func() bool { return len(f.reports()) == 2 })
		reports := f.reports()
		if len(reports[0].Faults) != 0 {
			t.Errorf("the held report carries the faults %q, want none: it was made before the fault", reports[0].Faults)
		}
		next := reports[1]
		switch {
		case len(next.Faults) != 1 || next.FaultsOmitted != 0:
			t.Errorf("the next report carries the faults %q and counts %d more, want the one fault of the resend", next.Faults, next.FaultsOmitted)
		case !strings.Contains(next.Faults[0], fell) || !strings.Contains(next.Faults[0], "(*trimmer).flush"):
			t.Errorf("the next report carries the fault %q, want the panic of the resend with its stack", next.Faults[0])
		case next.SwitchedOff:
			t.Error("the next report says the trimmer switched itself off after one fault, want it to go on")
		}
	})

	t.Run("the line of a fault tick recovered", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) {
			if call == 2 || call == 3 {
				panic(fmt.Sprintf("the volume reader fell over on call %d", call))
			}
		})
		f.useVolume(steady(50))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		const fell = "the log fell over writing the line of a fault"
		var fallen atomic.Bool
		tr.logf = func(format string, args ...any) {
			if strings.Contains(format, "recovered a panic") && fallen.CompareAndSwap(false, true) {
				panic(fell)
			}
			f.log(format, args...)
		}
		_, done := f.start(tr)
		f.tick("nothing to sweep")

		// The second reading falls over, and so does the line tick's handler writes of it.
		f.tick("recovered a panic")
		if line := written(t, f, "recovered a panic"); !strings.Contains(line, "(2 in a row of readings)") || !strings.Contains(line, fell) || !strings.Contains(line, "(*trimmer).fault") {
			t.Errorf("the trimmer wrote %q, want the panic of the line counted as the second fault in the reading row, the row of the fault it was writing, with the stack it was raised on", line)
		}

		// The third reading falls over too: the third fault in the reading row switches the trimmer off.
		f.tick("recovered a panic")
		awaitClosed(t, done, "the trimmer's loop to return once three faults in a row switched it off")
		reports := f.reports()
		if len(reports) != 1 {
			t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off", len(reports))
		}
		report := reports[0]
		if !report.SwitchedOff {
			t.Error("the last report does not say the trimmer switched itself off")
		}
		if !strings.Contains(report.StopReason, "3 panics in a row of readings") {
			t.Errorf("the last report's stop reason is %q, want the three panics of the reading row that stopped it", report.StopReason)
		}
		want := []string{"the volume reader fell over on call 2", fell, "the volume reader fell over on call 3"}
		if len(report.Faults) != len(want) {
			t.Fatalf("the last report carries %d faults, want the %d in a row that stopped it: %q", len(report.Faults), len(want), report.Faults)
		}
		for at, fault := range report.Faults {
			if !strings.HasPrefix(fault, want[at]) {
				t.Errorf("fault %d of the last report is %q, want the panic %q", at+1, fault, want[at])
			}
		}
	})

	t.Run("the last report as the third fault switches the trimmer off", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
		f.useVolume(steady(50))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		const fell = "the connection fell over writing the last report"
		var attempts atomic.Int32
		tr.send = func(shimwire.LauncherVolumeReport) error {
			attempts.Add(1)
			panic(fell)
		}
		_, done := f.start(tr)
		for range volumeFaultLimit {
			f.tick("recovered a panic")
		}
		awaitClosed(t, done, "the trimmer's loop to end once its last report panicked")
		if line := written(t, f, "the last report of the switched-off trimmer panicked"); !strings.Contains(line, fell) || !strings.Contains(line, "(*trimmer).switchOff") {
			t.Errorf("the trimmer wrote %q, want the panic of its last report with the stack it was raised on", line)
		}
		if tries := attempts.Load(); tries != 1 {
			t.Errorf("the trimmer tried its last report %d times, want once: a report whose making panicked is not tried again", tries)
		}
		if faults := f.logged("recovered a panic in the trimmer's own work"); faults != volumeFaultLimit {
			t.Errorf("the trimmer wrote %d faults, want the %d that switched it off: the panic of the last report ends it and counts nothing", faults, volumeFaultLimit)
		}
		if _, waiting := f.ticks.live(volumeSettingsWait); waiting {
			t.Error("the trimmer still waits on a clock to try its last report again")
		}
	})

	t.Run("the last report's next attempt", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
		f.useVolume(steady(50))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("no daemon connection is up to carry it"))
		tr := f.trimmer()
		const fell = "the connection fell over sending the last report"
		var attempts atomic.Int32
		tr.send = func(report shimwire.LauncherVolumeReport) error {
			if attempts.Add(1) > 1 {
				panic(fell)
			}
			return f.send(report)
		}
		_, done := f.start(tr)
		for range volumeFaultLimit {
			f.tick("recovered a panic")
		}
		testwait.Eventually(t, "the trimmer to hold its last report", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})
		offerSettingsTick(t, f)
		awaitClosed(t, done, "the trimmer's loop to end once the attempt at its last report panicked")
		if line := written(t, f, "the last report of the switched-off trimmer panicked"); !strings.Contains(line, fell) || !strings.Contains(line, "(*trimmer).deliverLast") {
			t.Errorf("the trimmer wrote %q, want the panic of the attempt with the stack it was raised on", line)
		}
		if tries := attempts.Load(); tries != 2 {
			t.Errorf("the trimmer tried its last report %d times, want twice: once as it switched off, once more on the clock, and not again after that panicked", tries)
		}
		if _, waiting := f.ticks.live(volumeSettingsWait); waiting {
			t.Error("the trimmer still waits on a clock to try its last report again")
		}
		if sent := len(f.reports()); sent != 0 {
			t.Errorf("the trimmer sent %d reports, want none", sent)
		}
	})

	t.Run("a logger that panics on the line of every fault and of the last report's panic", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		f.setOnRead(func(call int) { panic(fmt.Sprintf("the volume reader fell over on call %d", call)) })
		f.useVolume(steady(50))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		var faultLines, lastLines atomic.Int32
		tr.logf = func(format string, args ...any) {
			switch {
			case strings.Contains(format, "recovered a panic"):
				faultLines.Add(1)
				panic("the log fell over writing the line of a fault")
			case strings.Contains(format, "the last report of the switched-off trimmer panicked"):
				lastLines.Add(1)
				panic("the log fell over writing the line of the last report's panic")
			}
			f.log(format, args...)
		}
		var sends atomic.Int32
		tr.send = func(shimwire.LauncherVolumeReport) error {
			sends.Add(1)
			panic("the connection fell over writing the last report")
		}
		_, done := f.start(tr)
		testwait.Eventually(t, "the interval loop to start", func() bool { return f.logged("started:") == 1 })

		// The first reading falls over, and so do the line tick's handler writes of it and the
		// line run writes of that line's panic, which run drops: the loop starts again from the
		// wait, with a started: line of its own.
		f.tick("started:")
		// The second does too, and its row of readings is now at four: two panics of the volume
		// reader and two of the lines of their faults (fault, run). The switched-off trimmer's
		// report then panics in its send, and so does the line run writes of that.
		f.tick("off for the life of this launcher process")
		awaitClosed(t, done, "the trimmer's loop to end once its last report panicked")

		if written, last := faultLines.Load(), lastLines.Load(); written != 4 || last != 1 {
			t.Fatalf("the logger was asked for %d fault lines and %d lines of the last report's panic, want 4 and 1: run's own lines were never reached", written, last)
		}
		if said := f.logged("4 panics in a row of readings: the trimmer is off for the life of this launcher process"); said != 1 {
			t.Errorf("the trimmer wrote %q, want it switched off by the four panics of its reading row", f.linesWritten())
		}
		if tries := sends.Load(); tries != 1 {
			t.Errorf("the trimmer tried its last report %d times, want once: a report whose send panicked is not tried again", tries)
		}
		if sent := len(f.reports()); sent != 0 {
			t.Errorf("the trimmer sent %d reports, want none", sent)
		}
	})
}

// The episode number follows the volume across the marks: the first reading at the high mark
// opens episode 1, a reading at or below the low mark after that opens the next, and two reported
// sweeps in a row that start five points or more above the episode's anchor, here the lowest start
// of its reported sweeps, open another. A reading between the marks raises nothing, and a volume
// oscillating between them raises nothing, which is what keeps the daemon's three-notice bound
// from being spent on a trimmer doing its job.
//
// The anchor is the pre-sweep reading and never the figure a report carried, and the sweep from
// 96 is the control: the lower of its start and the one before it, 94, is more than five points
// above the 88.5 the first report of episode 2 carried and less than five above the 90 that sweep
// started from, so a trimmer anchored on the report would raise the episode there and this case
// would fail.
func TestTheEpisodeNumberFollowsTheVolumeAcrossTheMarks(t *testing.T) {
	f := newTrimFixture(t, 1)
	root := f.roots[0]
	added := 0
	f.setOnRead(func(int) {
		// One fresh entry per reading, so that every sweep has something to delete and the
		// script below stays in step: a sweep with no deletion takes no reading of its own.
		added++
		f.addTree(filepath.Join(root.Path, fmt.Sprintf("go-build8%08d", added)), root.Entry)
	})
	f.useVolume(script(
		90, 88, // the first sweep: episode 1, first, anchored at 90
		88, 86.5, // a second sweep of the same episode, which lowers its anchor to 88
		75,       // between the marks: nothing
		69,       // below the low mark after the high one: episode 2, recovery
		90, 88.5, // a sweep of episode 2, which anchors it at 90
		94, 92.5, // one start more than five points above the report's 88.5 and under five above the 90: nothing
		96, 94.5, // the control: starts of 94 and 96, the lower five points above the 88.5 and under five above the 90
		99, 97, // starts of 96 and 99, the lower more than five points above the 90: episode 3, rise
		86, 84.5, // back to the high mark from a post-sweep low point: nothing
		84, // between the marks again: nothing
	))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	for at, ends := range []string{
		"the sweep ended", "the sweep ended", "nothing to sweep", "nothing to sweep", "the sweep ended",
		"the sweep ended", "the sweep ended", "the sweep ended", "the sweep ended", "nothing to sweep",
	} {
		f.tick(ends)
		if t.Failed() {
			t.Fatalf("the case stopped at interval %d", at+1)
		}
	}

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst},
		{1, shimwire.VolumeEpisodeFirst},
		{2, shimwire.VolumeEpisodeRecovery},
		{2, shimwire.VolumeEpisodeRecovery},
		{2, shimwire.VolumeEpisodeRecovery},
		{3, shimwire.VolumeEpisodeRise},
		{3, shimwire.VolumeEpisodeRise},
	})
	if raises := f.logged("episode "); raises != 3 {
		t.Errorf("the trimmer raised the episode %d times, want 3: the readings between the marks and the oscillation raise nothing", raises)
	}
}

// script is the volume as a case writes it out, one reading per call of the reader.
func script(percents ...float64) func(int) volumeUsage {
	return func(call int) volumeUsage {
		at := call - 1
		if at >= len(percents) {
			at = len(percents) - 1
		}
		return usageAt(percents[at])
	}
}

// The rise that raises an episode is a floor five points above the anchor, the floor being the
// lower of the last two reported sweep starts: two starts four and nine tenths above the anchor,
// here the start of episode 1's first reported sweep, raise nothing; the first start exactly
// five above raises nothing either, since the start before it is still under five; and the
// second raises it, one reported sweep after the volume first stood five points above the
// anchor. A smaller rise reads the roles' ordinary rebuilding between the marks as a fresh
// deterioration and wakes the issue's architect on every cycle of a volume that is merely
// oscillating; a larger one keeps a higher reading inside the episode it is in, under that
// episode's three notices; and a rise read from one start alone is raised by a peak that recurs
// at sweep starts on every cycle (TestARecurringPeakAtSweepStartsRaisesNoEpisodeOnEveryCycle).
//
// The starts are put through episodeOf and riseAt in the order tick puts a reported sweep's start
// through them, which is where the trimmer decides this, with no sweep between them. The loop's
// own path through the same boundary, where each start is a sweep that reports, is
// TestTheAnchorOfAnEpisodeNoRiseRaisedIsTheLowestStartOfItsReportedSweeps. The points are this
// case's own figures and not volumeRisePoints, because a case staged from the constant it tests
// moves with it (INFRA-2009, review round 6).
func TestAFloorFivePointsAboveTheAnchorRaisesTheEpisodeAndOneUnderItDoesNot(t *testing.T) {
	const (
		anchored = 90.0
		under    = anchored + 4.9
		at       = anchored + 5.0
	)
	f := newTrimFixture(t, 0)
	set := *trimSettings()
	tr := f.trimmer()
	reported := func(start float64) {
		tr.episodeOf(start, set)
		tr.riseAt(start)
	}

	// The volume reaches the high mark, which opens episode 1, and the sweep it started
	// reported, which anchors the episode at that start.
	reported(anchored)
	if tr.episode != 1 || tr.raisedBy != shimwire.VolumeEpisodeFirst {
		t.Fatalf("the first reading at the high mark left the trimmer in episode %d (%s), want episode 1 (%s): the case would read a rise against no anchor", tr.episode, tr.raisedBy, shimwire.VolumeEpisodeFirst)
	}

	reported(under)
	reported(under)
	if tr.episode != 1 {
		t.Errorf("two reported sweeps starting at %.1f%%, %.1f points above the %.1f%% the episode is anchored at, raised the episode to %d (%s), want episode 1 held: a volume the trimmer is keeping within a few points of where it swept from is a trimmer doing its job, and the daemon's three-notice bound is spent on it",
			under, under-anchored, anchored, tr.episode, tr.raisedBy)
	}

	reported(at)
	if tr.episode != 1 {
		t.Errorf("a reported sweep starting at %.1f%%, after one that started at %.1f%%, raised the episode to %d (%s), want episode 1 held: the lower of the two starts is %.1f points above the %.1f%% anchor, under five, and one start alone is no floor",
			at, under, tr.episode, tr.raisedBy, under-anchored, anchored)
	}

	reported(at)
	if tr.episode != 2 || tr.raisedBy != shimwire.VolumeEpisodeRise {
		t.Errorf("two reported sweeps in a row starting at %.1f%%, %.1f points above the %.1f%% the episode is anchored at, left the trimmer in episode %d (%s), want episode 2 (%s): a floor five points above the anchor raises the episode",
			at, at-anchored, anchored, tr.episode, tr.raisedBy, shimwire.VolumeEpisodeRise)
	}
	f.assertPrefixed()
}

// A volume that keeps filling slowly is reported at each step of its deterioration: each five
// points its sweep starts climb above the episode's anchor open an episode of its own, however
// little the volume climbs from one reported sweep to the next.
//
// The volume climbs two tenths of a point an interval from 86% to full, in files no sweep may
// delete: the fixture has no entry, so every sweep frees nothing, ends above the low mark and
// reports, and engages the ten-interval pause, which is what spaces the sweeps out in the pod.
// The sweeps start at 86%, 88.2%, 90.4%, 92.6%, 94.8%, 97%, 99.2% and then 100%, each 2.2 points
// above the one before until the volume is full. The fill is fourteen points of deterioration,
// two steps of five, and each must open a rise episode: one first reported before the volume
// reaches 96%, and one at 96% or above. A trimmer that moved the anchor up to each reported
// sweep's start would never see five points between them, so all nine reports would carry one
// episode, of which the daemon tells the architect three. The rule raises the first rise at the
// sweep from 94.8%, where the starts of 92.6% and 94.8% both stand five points above the 86%,
// and anchors episode 2 at their floor, 92.6%; the second comes at the first sweep from 100%,
// after the one from 99.2%. The points are this case's own figures and not volumeRisePoints
// (INFRA-2009, review round 6).
func TestASlowFillIsReportedAgainAtEachFivePointsItsSweepStartsClimb(t *testing.T) {
	f := newTrimFixture(t, 0)
	// The reading of interval n, counted in tenths of a point so that each start is exact:
	// interval 1 reads 86%, and each interval after it two tenths more, up to full.
	f.useVolume(func(interval int) volumeUsage { return usageAt(min(100, float64(858+2*interval)/10)) })
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 9)

	var below96, from96 bool
	for _, first := range firstReports(f) {
		if first.raisedBy != shimwire.VolumeEpisodeRise {
			continue
		}
		switch {
		case first.percent >= 91 && first.percent < 96:
			below96 = true
		case first.percent >= 96:
			from96 = true
		}
	}
	if !below96 || !from96 {
		t.Errorf("a volume filling from 86%% to full opened a rise episode first reported under 96%%: %t, and one at 96%% or above: %t, want both, one for each five points of the fill; the reports were %q",
			below96, from96, reportsSeen(f))
	}
}

// The anchor of an episode no rise raised is the lowest start of its reported sweeps: the start
// of its first reported sweep anchors it, a later reported sweep that starts lower lowers it, and
// one that starts higher leaves it where it is. No reported sweep moves it to the figure its
// report carried. An episode a rise raised is anchored at the floor that raised it instead, and
// never lowered, which TestARiseAnchorsTheEpisodeItRaisesAtTheFloorThatRaisedIt holds.
//
// The first sweep starts at 90% and leaves 88.5%; the second starts higher, at 94.9%, four and
// nine tenths above the 90, which raises nothing and leaves the anchor at 90; the third starts
// lower, at 87%, still at the high mark, and leaves 85.5%, which lowers the anchor to 87. Then
// sweeps start at 91.9%, four and nine tenths above the 87, which raises nothing, and at 92%
// twice: the first of those has the 91.9 before it and raises nothing, and the second raises the
// rise. An anchor left at the first sweep's 90, or at the floor of the episode's first two starts,
// also 90, would raise nothing at 92%; one moved up to a higher start, the 94.9%, the 91.9% or
// the 92%, would raise nothing either; and one at the lowest figure a report carried, the 85.5,
// would raise the episode a sweep early, at the first 92%, where the 91.9 before it is more than
// five points above the 85.5. The case runs through the loop, so each of those readings starts a
// sweep that reports, as it would in the pod.
func TestTheAnchorOfAnEpisodeNoRiseRaisedIsTheLowestStartOfItsReportedSweeps(t *testing.T) {
	f := newTrimFixture(t, 1)
	root := f.roots[0]
	added := 0
	f.setOnRead(func(int) {
		// One fresh entry per reading, so that every sweep deletes something and takes a reading
		// of its own at its end, the post-sweep figure its report carries.
		added++
		f.addTree(filepath.Join(root.Path, fmt.Sprintf("go-build8%08d", added)), root.Entry)
	})
	f.useVolume(script(
		90, 88.5, // the first reported sweep: episode 1, anchored at 90
		94.9, 93.4, // a later reported sweep that starts higher, four and nine tenths above: the anchor stays at 90
		87, 85.5, // one that starts lower, still at the high mark: anchored at 87
		91.9, 90.4, // four and nine tenths above the 87: nothing, and the anchor stays at 87
		92, 90.5, // five above the 87, after the 91.9: nothing
		92, 90.5, // five above the 87 twice in a row: episode 2, rise
		80, // between the marks: nothing
	))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	for at, ends := range []string{
		"the sweep ended", "the sweep ended", "the sweep ended", "the sweep ended", "the sweep ended", "the sweep ended",
		"nothing to sweep",
	} {
		f.tick(ends)
		if t.Failed() {
			t.Fatalf("the case stopped at interval %d", at+1)
		}
	}

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // the sweep from 90%
		{1, shimwire.VolumeEpisodeFirst}, // the sweep from 94.9%
		{1, shimwire.VolumeEpisodeFirst}, // the sweep from 87%
		{1, shimwire.VolumeEpisodeFirst}, // the sweep from 91.9%
		{1, shimwire.VolumeEpisodeFirst}, // the first sweep from 92%
		{2, shimwire.VolumeEpisodeRise},  // the second sweep from 92%
	})
	assertOneRise(t, f, "the floor of the last two reported sweep starts, 92.0% and 92.0%, is 92.0%, 5.0 points above the 87.0%")
}

// The line that raises a rise names the gap the trimmer read, not the five points that make a
// rise: a person reading the pod's log reads from it how far the volume climbed past the anchor
// it is measured from. The volume reads 86% through the first sweep and its pause, then 95%, so
// the second and third sweeps start at 95%: the second has the 86 before it and raises nothing,
// and the third raises the rise from a floor of 95%, nine points above the 86% the first sweep
// started from. The negative control is the line as it was at b844518c, which printed the rule's
// five points as the gap ("5 points above the 86.0%"), and the line that printed the gap as the
// constant again: of the other cases that read the rise line, every one but the subtest "from an
// anchor above 95%" of TestARiseAnchorsTheEpisodeItRaisesAtTheFloorThatRaisedIt, which rises 5.5
// points from 90%, rises at exactly five points, where the two figures agree, so those cases
// could not tell the two lines apart. Round 30 restaged this case with a third sweep, since a
// floor of two starts can raise no rise at the second reported sweep of an episode no rise
// raised, which the first episode is.
func TestTheRiseLineNamesTheGapTheTrimmerRead(t *testing.T) {
	f := newTrimFixture(t, 0)
	f.useVolume(func(interval int) volumeUsage {
		if interval <= 1+volumeQuietIntervals {
			return usageAt(86)
		}
		return usageAt(95)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 3)

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // the sweep from 86%, which anchors the episode
		{1, shimwire.VolumeEpisodeFirst}, // the first sweep from 95%, after the one from 86%
		{2, shimwire.VolumeEpisodeRise},  // the second sweep from 95%
	})
	assertOneRise(t, f, "the floor of the last two reported sweep starts, 95.0% and 95.0%, is 95.0%, 9.0 points above the 86.0%", "where 5 points raise a new episode")
}

// A volume that only goes up and down between the same two readings raises no new episode on
// every cycle: the five points of a rise are what keeps the roles' ordinary rebuilding between
// the marks from waking the issue's architect again and again, and a rise-raised episode's
// reports skip the pod's hour, so an episode raised on every cycle is a notice on every cycle
// with no bound at all. The volume reads 86% at every sweep's start, and 91% for one reading in
// the middle of each pause — a build's temporary files, gone again a minute later — so it never
// climbs past 91%. Over twelve sweeps its reports may carry the first episode and at most one
// rise. Measured at 72566309 in an issue pod: the twelve reports carried twelve episodes, each
// after the first raised by a rise, and through the daemon's glue with its clock moving eleven
// minutes a report the architect was told of all twelve in 2h12m. That trimmer set no anchor at
// a rise, and the next reported sweep anchored the new episode at its own start, 86% again, so
// the next 91% was a rise once more.
func TestAVolumeThatOnlyOscillatesRaisesNoNewEpisodeOnEveryCycle(t *testing.T) {
	f := newTrimFixture(t, 0)
	f.useVolume(func(interval int) volumeUsage {
		if (interval-1)%(1+volumeQuietIntervals) == 5 {
			return usageAt(91)
		}
		return usageAt(86)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 12)

	reports := f.reports()
	if len(reports) != 12 {
		t.Fatalf("the trimmer sent %d reports, want one for each of the twelve sweeps, each ending above the low mark", len(reports))
	}
	episodes := map[int]bool{}
	for _, report := range reports {
		episodes[report.Episode] = true
	}
	if len(episodes) > 2 {
		t.Errorf("the reports of a volume that never climbs past 91%% carry %d episodes, want the first and at most one rise: every cycle of it is told to the architect, past the hour and past the three",
			len(episodes))
	}
}

// One transient reading does not silence a volume that then keeps filling. The volume sweeps from
// 86% and frees nothing, and one reading in the middle of the first pause reads 97%: a role's
// build or extraction that wrote two and a half gigabytes and removed them within the minute.
// From the next sweep on, the volume climbs a tenth of a point an interval, from 86% to full.
// The fill is a deterioration of fourteen points from where the volume stood, and "one that keeps
// filling is reported at each step of its deterioration" (the spec's R4 bound): at least one
// step of it, a rise read once the volume stands five points above the 86% it swept from and
// before it reaches 96%, must open an episode of its own. A trimmer that anchored the episode the
// spike raised at the 97% that raised it waits for 102%, which no reading reaches, so after the
// spike episode's three notices, spent within the first half hour because a rise-raised report
// skips the pod's hour, the architect hears nothing while the volume climbs from 88% to full.
func TestOneTransientReadingDoesNotSilenceAVolumeThatThenKeepsFilling(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	cycle := 1 + volumeQuietIntervals
	f.useVolume(func(call int) volumeUsage {
		if call == 6 { // the middle of the first pause
			return usageAt(97)
		}
		percent := 86.0
		if call > cycle {
			percent = min(100, 86+0.1*float64(call-cycle))
		}
		return usageAt(percent)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 20)

	first := map[int]float64{} // each episode's first report, by the reading that report carries
	raisedBy := map[int]string{}
	var seen []string
	for _, report := range f.reports() {
		used := 100 * float64(report.UsedBytes) / float64(report.UsedBytes+report.FreeBytes)
		seen = append(seen, fmt.Sprintf("episode %d (%s) at %.1f%%", report.Episode, report.RaisedBy, used))
		if _, ok := first[report.Episode]; !ok {
			first[report.Episode], raisedBy[report.Episode] = used, report.RaisedBy
		}
	}
	for episode, used := range first {
		if raisedBy[episode] == shimwire.VolumeEpisodeRise && used >= 90 && used < 96 {
			return
		}
	}
	t.Errorf("no rise opened an episode while the volume filled from 90%% to 96%%: one reading of 97%% in a pause silenced the fill that followed it; the reports were %q", seen)
}

// An episode a rise raised is anchored at the floor that raised it, the lower of the two starts
// the rise read, and a reported sweep of that episode that starts lower leaves the anchor there.
// Every sweep frees nothing, reports and pauses, and each pause reads the lower of the starts
// beside it (sweepStarts).
//
// From an anchor of 95%: the sweeps start at 90%, which anchors episode 1; at 95%, which raises
// nothing, since the 90 before it is the lower of the two; and at 96%, where the starts of 95%
// and 96% raise a rise and anchor episode 2 at their floor, 95%. A sweep from 90% follows, which
// leaves a rise's anchor at 95, and then sweeps from 100%, 99.9%, 100% and 100%: the first three
// each have a start under 100% beside them and raise nothing, and the fourth, the second start
// at exactly 100.0% in a row, raises a second rise and anchors episode 3 at 100%. A trimmer that
// anchored a rise at its own start, the 96%, would raise nothing at 100%; one that lowered a
// rise's anchor to a lower start, the 90%, would raise the second rise at 99.9%. The last sweep
// is the limit seen from the other side, as the second subtest is: an anchor above 95% can never
// be passed, since a reading is at most 100%, so no rise follows episode 3, and none follows an
// episode anchored at 95.5% however long the volume stays full.
func TestARiseAnchorsTheEpisodeItRaisesAtTheFloorThatRaisedIt(t *testing.T) {
	t.Run("from an anchor of 95%", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
		starts := []float64{90, 95, 96, 90, 100, 99.9, 100, 100, 100}
		f.useVolume(sweepStarts(starts...))
		f.setSettings(trimSettings())
		f.start(f.trimmer())

		sweepsAndPauses(f, len(starts))

		assertReportEpisodes(t, f, []reportEpisode{
			{1, shimwire.VolumeEpisodeFirst}, // the sweep from 90%, which anchors episode 1 at 90%
			{1, shimwire.VolumeEpisodeFirst}, // from 95%, after the 90%
			{2, shimwire.VolumeEpisodeRise},  // from 96%, after the 95%: the first rise, anchored at 95%
			{2, shimwire.VolumeEpisodeRise},  // from 90%, which leaves the anchor at 95%
			{2, shimwire.VolumeEpisodeRise},  // from 100%, after the 90%
			{2, shimwire.VolumeEpisodeRise},  // from 99.9%, after a 100%
			{2, shimwire.VolumeEpisodeRise},  // from 100%, after the 99.9%
			{3, shimwire.VolumeEpisodeRise},  // from 100%, after a 100%: the second rise, anchored at 100%
			{3, shimwire.VolumeEpisodeRise},  // from 100% once more, which raises nothing
		})
		assertRises(t, f,
			"the floor of the last two reported sweep starts, 95.0% and 96.0%, is 95.0%, 5.0 points above the 90.0%",
			"the floor of the last two reported sweep starts, 100.0% and 100.0%, is 100.0%, 5.0 points above the 95.0%",
		)
	})

	t.Run("from an anchor above 95%", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
		starts := []float64{90, 95.5, 95.5, 100, 100, 100, 100}
		f.useVolume(sweepStarts(starts...))
		f.setSettings(trimSettings())
		f.start(f.trimmer())

		sweepsAndPauses(f, len(starts))

		assertReportEpisodes(t, f, []reportEpisode{
			{1, shimwire.VolumeEpisodeFirst}, // the sweep from 90%, which anchors episode 1 at 90%
			{1, shimwire.VolumeEpisodeFirst}, // from 95.5%, after the 90%
			{2, shimwire.VolumeEpisodeRise},  // from 95.5%, after a 95.5%: the rise, anchored at 95.5%
			{2, shimwire.VolumeEpisodeRise},  // from 100%, four sweeps in a row, which raise nothing
			{2, shimwire.VolumeEpisodeRise},
			{2, shimwire.VolumeEpisodeRise},
			{2, shimwire.VolumeEpisodeRise},
		})
		assertRises(t, f, "the floor of the last two reported sweep starts, 95.5% and 95.5%, is 95.5%, 5.5 points above the 90.0%")
	})
}

// A volume that sits still above the high mark keeps one episode: a reading that does not move
// is no rise, so every sweep of it reports under the first episode, and the daemon tells the
// architect of three of those reports and no more (internal/workflow's
// TestAFourthSweepOfOneTrimmerAndEpisodeTellsNobodyHoweverLongItWaits).
func TestAVolumeThatSitsStillAboveTheHighMarkKeepsOneEpisode(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 4)

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst},
		{1, shimwire.VolumeEpisodeFirst},
		{1, shimwire.VolumeEpisodeFirst},
		{1, shimwire.VolumeEpisodeFirst},
	})
	if raised := f.logged("episode "); raised != 1 {
		t.Errorf("the trimmer raised the episode %d times, want once: a volume that sits still at 90%% is no rise; it wrote %q", raised, f.linesWritten())
	}
}

// A transient reading at a sweep's start does not silence a volume that then keeps filling,
// wherever the start falls. In each subtest a role's build pushes the volume to 97% for a moment,
// which one sweep's start reads, or two at an episode's first sweeps, and the volume then falls
// back to 86% and fills to full in steps of two points a sweep; every sweep frees nothing, reports
// and pauses, and only the starts read 97% (sweepStarts). The fill is fourteen points of
// deterioration from where the volume stood, and at least one step of it must open an episode of
// its own, first reported after the fall to 86% and before the volume is full.
//
// On the first sweep's start, the 97% anchors episode 1, which the next start, 86%, lowers: an
// anchor fixed at the start of the episode's first sweep waits for two starts of 102%, which no
// reading reaches, so after its three notices the architect hears nothing while the volume fills.
// On the first two sweeps' starts, the 86% lowers the anchor all the same, where one fixed at the
// floor of the episode's first two starts would stay at 97%. On a later sweep's start, the 97% has
// an 86% beside it and raises nothing. On the start of the sweep that raises a rise, after a start
// of 91%, the rise is anchored at the floor of the two, 91%, and not at the 97%, so the fill raises
// a second rise once two starts stand at 96% or above. A transient that two starts in a row read
// in the middle of an episode is not among them: it raises a rise anchored at its own level, which
// a fill from lower down passes only with two starts five points above that level, and never where
// the level is above 95%.
func TestATransientSweepStartDoesNotSilenceAVolumeThatThenKeepsFilling(t *testing.T) {
	for _, staged := range []struct {
		name   string
		starts []float64
		fallen int // the sweep from 86% after the 97%, counted from 0
	}{
		{"on the first sweep's start", []float64{97, 86, 88, 90, 92, 94, 96, 98, 100, 100}, 1},
		{"on the first two sweeps' starts", []float64{97, 97, 86, 88, 90, 92, 94, 96, 98, 100, 100}, 2},
		{"on a later sweep's start", []float64{86, 97, 86, 88, 90, 92, 94, 96, 98, 100}, 2},
		{"on the start of the sweep that raises a rise", []float64{86, 91, 97, 86, 88, 90, 92, 94, 96, 98, 100, 100}, 3},
	} {
		t.Run(staged.name, func(t *testing.T) {
			f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
			f.useVolume(sweepStarts(staged.starts...))
			f.setSettings(trimSettings())
			f.start(f.trimmer())

			sweepsAndPauses(f, len(staged.starts))

			for _, first := range firstReports(f) {
				if first.raisedBy == shimwire.VolumeEpisodeRise && first.report > staged.fallen && first.percent < 100 {
					return
				}
			}
			t.Errorf("no rise opened an episode while the volume filled from 86%% to full after the 97%%: the transient silenced the fill; the reports were %q", reportsSeen(f))
		})
	}
}

// A peak that recurs at sweep starts over a floor that does not climb raises no episode on every
// cycle: a rise-raised episode's reports skip the pod's hour, so a rise on every cycle is a
// notice on every cycle with no bound at all. Every sweep frees nothing, reports and pauses, and
// each pause reads the lower of the starts beside it (sweepStarts), so a 91% between two 86%s is
// read at its start alone.
//
// Starts alternating 86% and 91% have an 86% beside every 91%, so the floor never leaves 86% and
// no new episode is raised. A trimmer that read a rise from one start against the lowest start of
// the episode raised one at every 91%, after the 86% before it had lowered the anchor again:
// about 130 notices a day for one pod. Starts repeating 86%, 91% and 91% hold a floor of 91% at
// every second 91%, which raises a rise once and anchors it at 91%; a rise's anchor never comes
// down, so the next 91% floor is no rise, where a trimmer that lowered it to the next 86% raised a
// rise every third sweep.
func TestARecurringPeakAtSweepStartsRaisesNoEpisodeOnEveryCycle(t *testing.T) {
	t.Run("starts alternating 86% and 91%", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
		starts := slices.Repeat([]float64{86, 91}, 6)
		f.useVolume(sweepStarts(starts...))
		f.setSettings(trimSettings())
		f.start(f.trimmer())

		sweepsAndPauses(f, len(starts))

		if episodes := firstReports(f); len(episodes) != 1 {
			t.Errorf("the reports of sweeps starting at 86%% and 91%% in turn carry %d episodes, want the first alone: the floor of every two starts is 86%%; the reports were %q",
				len(episodes), reportsSeen(f))
		}
	})

	t.Run("starts repeating 86%, 91% and 91%", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
		starts := slices.Repeat([]float64{86, 91, 91}, 4)
		f.useVolume(sweepStarts(starts...))
		f.setSettings(trimSettings())
		f.start(f.trimmer())

		sweepsAndPauses(f, len(starts))

		if rises := f.logged("(" + shimwire.VolumeEpisodeRise + ")"); rises > 1 {
			t.Errorf("four cycles of sweeps starting at 86%%, 91%% and 91%% raised %d rises, want at most one: a rise's anchor never comes down, so the next 91%% floor is no rise; the reports were %q",
				rises, reportsSeen(f))
		}
	})
}

// The rises of a chain anchored at the high mark are at most (100 − high mark) / 5, rounded down,
// before a recovery: each rise lifts the anchor five points or more, a rise's anchor never comes
// down, every floor is a reported sweep's start and so at or above the high mark, which this case
// never changes, and a reading is at most 100%. The bound is the high mark's, not a flat three: at
// a high mark of 50%, ten. The case drives the longest chain there is to its end: the first sweep
// starts at the high mark, each next floor stands exactly five points above the anchor before it,
// two starts in a row at 55%, at 60% and so on to 100%, and three more sweeps from 100% raise
// nothing.
func TestTheRisesBeforeARecoveryAreBoundedByTheRoomAboveTheHighMark(t *testing.T) {
	const high = 50
	starts := []float64{high}
	for floor := high + 5; floor <= 100; floor += 5 {
		starts = append(starts, float64(floor), float64(floor))
	}
	starts = append(starts, 100, 100, 100)
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(sweepStarts(starts...))
	f.setSettings(&shimwire.LauncherVolumeTrim{HighPercent: high, LowPercent: 40, MinAgeMinutes: 30, IntervalSeconds: 30})
	f.start(f.trimmer())

	sweepsAndPauses(f, len(starts))

	want := (100 - high) / 5
	if rises := f.logged("(" + shimwire.VolumeEpisodeRise + ")"); rises != want {
		t.Errorf("a chain from a high mark of %d%% raised %d rises, want %d, one for each five points between the high mark and full; the reports were %q",
			high, rises, want, reportsSeen(f))
	}
	reports := f.reports()
	if last := reports[len(reports)-1]; last.Episode != want+1 || last.RaisedBy != shimwire.VolumeEpisodeRise {
		t.Errorf("the last report carries episode %d (%s), want episode %d (%s): the first and %d rises",
			last.Episode, last.RaisedBy, want+1, shimwire.VolumeEpisodeRise, want)
	}
}

// Each recovery starts a chain of its own, which can rise again: a volume that falls to the low
// mark or below and fills back past the high mark raises a recovery episode, anchored afresh at the
// lowest start of its reported sweeps, and then a rise once two reported sweeps in a row start
// five points or more above that anchor. In each cycle here the sweeps start at 86%, 91% and
// 91%, and one reading in the third pause falls to 65%, below the low mark, after which the volume
// stands at 80% until the next cycle's first sweep. Every sweep frees nothing, reports and pauses.
// So each cycle after the first raises a recovery episode and one rise, whose reports skip the
// pod's hour.
func TestEachRecoveryStartsAChainThatCanRiseAgain(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	cycle := []float64{86, 91, 91}
	f.useVolume(func(interval int) volumeUsage {
		sweep, at := (interval-1)/(1+volumeQuietIntervals), (interval-1)%(1+volumeQuietIntervals)
		switch {
		case sweep%len(cycle) == len(cycle)-1 && at == 5:
			return usageAt(65)
		case sweep%len(cycle) == len(cycle)-1 && at > 5:
			return usageAt(80)
		}
		return usageAt(cycle[sweep%len(cycle)])
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 4*len(cycle))

	var want []reportEpisode
	for at := range 4 {
		opened := reportEpisode{2*at + 1, shimwire.VolumeEpisodeRecovery}
		if at == 0 {
			opened.raisedBy = shimwire.VolumeEpisodeFirst
		}
		want = append(want, opened, opened, reportEpisode{2*at + 2, shimwire.VolumeEpisodeRise})
	}
	assertReportEpisodes(t, f, want)
}

// Starts of 86% and 91% raise nothing, and a third of 92% raises a rise: the 86% anchors episode
// 1, the 91% has the 86% beside it, a floor of 86% and no rise, and the 92% has the 91% beside it,
// a floor of 91%, five points above the anchor. Every sweep frees nothing, reports and pauses, and
// each pause reads the lower of the starts beside it (sweepStarts). The negative controls are a
// copy that reads a rise from the one start, which raises it at the 91%, and a copy that wants the
// floor more than five points above the anchor, which raises none.
func TestStartsOf86And91RaiseNothingAndAThirdOf92RaisesARise(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(sweepStarts(86, 91, 92))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 3)

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 86%, which anchors episode 1 at 86%
		{1, shimwire.VolumeEpisodeFirst}, // from 91%, after the 86%: a floor of 86%, no rise
		{2, shimwire.VolumeEpisodeRise},  // from 92%, after the 91%: a floor of 91%, the rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 91.0% and 92.0%, is 91.0%, 5.0 points above the 86.0%")
}

// A lower start lowers the anchor of an episode no rise raised, so a high first start does not
// stand in for the episode's anchor: starts of 97%, 86%, 91% and 92% raise a rise at the 92%, as
// starts of 86%, 91% and 92% do. The 97% anchors episode 1, the 86% lowers the anchor to 86%, the
// 91% has the 86% beside it, and the 92% has the 91% beside it, a floor five points above the 86%.
// The negative control is a copy that never lowers an anchor, under which episode 1 stays anchored
// at 97% and the 92% raises nothing.
func TestALowerStartLowersTheAnchorSoStartsOf97Then86And91And92RaiseARise(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(sweepStarts(97, 86, 91, 92))
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, 4)

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 97%, which anchors episode 1 at 97%
		{1, shimwire.VolumeEpisodeFirst}, // from 86%, which lowers the anchor to 86%
		{1, shimwire.VolumeEpisodeFirst}, // from 91%, after the 86%: a floor of 86%, no rise
		{2, shimwire.VolumeEpisodeRise},  // from 92%, after the 91%: a floor of 91%, the rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 91.0% and 92.0%, is 91.0%, 5.0 points above the 86.0%")
}

// Two starts in a row at 97% after an anchor of 86% raise a rise anchored at 97%, the floor that
// raised it, and a fill from lower down then raises nothing, since no floor stands five points
// above 97%, until a recovery, which starts a chain that can rise again. The sweeps start at 86%,
// 97% and 97%, then at 86% and up in steps of two points to 100%, twice; one reading in the pause
// after the second 100% falls to 65%, below the low mark, and the pause then reads 80%; the sweeps
// of the next chain start at 86%, 91% and 92%. Every sweep frees nothing, reports and pauses. The
// negative control is a copy that anchors a rise five points above the anchor before it, at 91%
// here, rather than at the floor that raised it: the fill then raises a second rise at 98%.
func TestARiseTwoStartsOf97RaiseHoldsAFillFromLowerDownUntilARecovery(t *testing.T) {
	starts := []float64{86, 97, 97, 86, 88, 90, 92, 94, 96, 98, 100, 100, 86, 91, 92}
	const recovered = 11 // the sweep from the second 100%, counted from 0, whose pause falls to 65%
	steps := sweepStarts(starts...)
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(func(interval int) volumeUsage {
		sweep, at := (interval-1)/(1+volumeQuietIntervals), (interval-1)%(1+volumeQuietIntervals)
		switch {
		case sweep == recovered && at == 5:
			return usageAt(65)
		case sweep == recovered && at > 5:
			return usageAt(80)
		}
		return steps(interval)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	sweepsAndPauses(f, len(starts))

	want := []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 86%, which anchors episode 1 at 86%
		{1, shimwire.VolumeEpisodeFirst}, // from 97%, after the 86%: a floor of 86%, no rise
		{2, shimwire.VolumeEpisodeRise},  // from 97%, after a 97%: the rise, anchored at 97%
	}
	for range recovered - 2 { // from 86% to the second 100%: no floor five points above 97%
		want = append(want, reportEpisode{2, shimwire.VolumeEpisodeRise})
	}
	want = append(want,
		reportEpisode{3, shimwire.VolumeEpisodeRecovery}, // from 86%, after the fall to 65%: anchored afresh at 86%
		reportEpisode{3, shimwire.VolumeEpisodeRecovery}, // from 91%, after the 86%: a floor of 86%, no rise
		reportEpisode{4, shimwire.VolumeEpisodeRise},     // from 92%, after the 91%: the new chain's rise
	)
	assertReportEpisodes(t, f, want)
	assertRises(t, f,
		"the floor of the last two reported sweep starts, 97.0% and 97.0%, is 97.0%, 11.0 points above the 86.0%",
		"the floor of the last two reported sweep starts, 91.0% and 92.0%, is 91.0%, 5.0 points above the 86.0%",
	)
}

// A sweep the settings stopped neither anchors an episode nor pairs with the next reported sweep's
// start: it sends no report, and riseAt reads only a reported sweep (tick). An answer with no
// settings arrives as two sweeps start, so each ends at its first entry boundary, and the trimmer
// waits for settings and runs again once they are back. The first stopped sweep starts at 85%,
// the first of the episode; the reported sweeps after it start at 90% and 90%; the second stopped
// sweep starts at 97%; and the reported sweeps after it at 95% and 95%. Read as the code reads
// them, the first 90% anchors episode 1, and the rise is the second 95%, a floor of 95% five points
// above it. The negative control is a copy that reads the stopped sweep's start too: the 85%
// anchors the episode, so the second 90% raises a rise, and the 97% pairs with the first 95%.
func TestASweepTheSettingsStoppedNeitherAnchorsNorPairs(t *testing.T) {
	pause := func(percent float64) []float64 { return slices.Repeat([]float64{percent}, volumeQuietIntervals) }
	f := newTrimFixture(t, 0) // no entry it may delete: every sweep frees nothing and takes no reading of its own
	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			// One entry too young to delete: the listing asks the settings before it reads an
			// entry's age (list), so each sweep has an entry boundary the answer stops it at, and
			// the sweeps that run delete nothing.
			mkdirFixture(t, filepath.Join(root.Path, "tool-fresh"))
		}
	}
	f.useVolume(script(slices.Concat(
		[]float64{85},     // stopped by the settings
		[]float64{90},     // reported: anchors episode 1 at 90%
		pause(90),         // its pause
		[]float64{90},     // reported: a floor of 90%, no rise
		pause(90),         // its pause
		[]float64{97},     // stopped by the settings
		[]float64{95},     // reported: a floor of 90%, no rise
		pause(95),         // its pause
		[]float64{95, 95}, // reported: a floor of 95%, the rise, and the reading after it
	)...))
	f.setSettings(trimSettings())
	var stopping atomic.Bool
	f.clock.setHook(func(*fakeClock) {
		// The clock's first reading of an interval is the sweep's own start (sweep): the answer
		// with no settings arrives there, after the interval's own read of the settings.
		if stopping.CompareAndSwap(true, false) {
			f.setSettings(nil)
		}
	})
	f.start(f.trimmer())
	testwait.Eventually(t, "the interval loop to start", func() bool { return f.logged("started:") == 1 })

	stopped := func() {
		t.Helper()
		waited, started := f.logged("no settings are in force, so the trimmer stays off"), f.logged("started:")
		stopping.Store(true)
		f.tick("the settings stopped the trimmer")
		testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
			return f.logged("no settings are in force, so the trimmer stays off") == waited+1
		})
		f.setSettings(trimSettings())
		offerSettingsTicksUntil(t, f, "the interval loop to start again", func() bool { return f.logged("started:") == started+1 })
	}
	reported := func(pauses int) {
		t.Helper()
		f.tick("the sweep ended:")
		for range pauses {
			f.tick("no sweep for another")
		}
	}
	stopped()
	reported(volumeQuietIntervals)
	reported(volumeQuietIntervals)
	stopped()
	reported(volumeQuietIntervals)
	reported(0)
	if t.Failed() {
		t.FailNow()
	}
	testwait.Eventually(t, "the last sweep's report", func() bool { return len(f.reports()) == 4 })

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst}, // from 90%, after the stopped 85%: anchors episode 1 at 90%
		{1, shimwire.VolumeEpisodeFirst}, // from 90%, after the 90%: a floor of 90%, no rise
		{1, shimwire.VolumeEpisodeFirst}, // from 95%, after the stopped 97%: a floor of 90%, no rise
		{2, shimwire.VolumeEpisodeRise},  // from 95%, after the 95%: a floor of 95%, the rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 95.0% and 95.0%, is 95.0%, 5.0 points above the 90.0%")
	if said := f.logged("the settings stopped the trimmer"); said != 2 {
		t.Errorf("the settings stopped %d sweeps, want the two the case stopped: %q", said, f.linesWritten())
	}
}

// A sweep the settings stopped right after a recovery neither anchors the chain that recovery
// started nor pairs with that chain's first reported sweep: it sends no report, and riseAt reads
// only a reported sweep (tick). The case above stops a sweep before the first episode has an anchor
// and inside it; this one stops the first sweep of a new chain. The first sweep starts at 86% and
// reports, anchoring episode 1; an interval after its pause reads 65%, below the low mark, which is
// a recovery; the next sweep starts at 86% and an answer with no settings stops it at its first
// entry boundary; the trimmer runs again once settings are back, and the two reported sweeps after
// it start at 91% and 91%. Read as the code reads them, the first 91% anchors episode 2 and the
// second raises nothing. The negative control is a copy that reads the stopped sweep's start in
// the settings' branch of tick: the 86% anchors the new chain and is the start the first 91% pairs
// with, and the second 91%, a floor five points above it, raises a rise.
func TestASweepTheSettingsStoppedRightAfterARecoveryNeitherAnchorsNorPairs(t *testing.T) {
	pause := func(percent float64) []float64 { return slices.Repeat([]float64{percent}, volumeQuietIntervals) }
	f := newTrimFixture(t, 0) // no entry it may delete: every sweep frees nothing and takes no reading of its own
	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			// One entry too young to delete: the listing asks the settings before it reads an
			// entry's age (list), so each sweep has an entry boundary the answer stops it at, and
			// the sweeps that run delete nothing.
			mkdirFixture(t, filepath.Join(root.Path, "tool-fresh"))
		}
	}
	f.useVolume(script(slices.Concat(
		[]float64{86},     // reported: anchors episode 1 at 86%
		pause(86),         // its pause
		[]float64{65},     // below the low mark after the high one: episode 2, recovery
		[]float64{86},     // stopped by the settings, the first sweep of the new chain
		[]float64{91},     // reported: anchors episode 2 at 91%
		pause(91),         // its pause
		[]float64{91, 91}, // reported: a floor of 91%, no rise, and the reading after it
	)...))
	f.setSettings(trimSettings())
	var stopping atomic.Bool
	f.clock.setHook(func(*fakeClock) {
		// The clock's first reading of an interval is the sweep's own start (sweep): the answer
		// with no settings arrives there, after the interval's own read of the settings.
		if stopping.CompareAndSwap(true, false) {
			f.setSettings(nil)
		}
	})
	f.start(f.trimmer())
	testwait.Eventually(t, "the interval loop to start", func() bool { return f.logged("started:") == 1 })

	f.tick("the sweep ended:")
	for range volumeQuietIntervals {
		f.tick("no sweep for another")
	}
	f.tick("nothing to sweep")
	if recovered := f.logged("episode 2 (" + shimwire.VolumeEpisodeRecovery + "): the volume fell to 65.0%"); recovered != 1 {
		t.Fatalf("the reading of 65%% after the pause raised %d recoveries, want the one the case stops the next sweep after: %q", recovered, f.linesWritten())
	}

	waited, started := f.logged("no settings are in force, so the trimmer stays off"), f.logged("started:")
	stopping.Store(true)
	f.tick("the settings stopped the trimmer")
	testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
		return f.logged("no settings are in force, so the trimmer stays off") == waited+1
	})
	f.setSettings(trimSettings())
	offerSettingsTicksUntil(t, f, "the interval loop to start again", func() bool { return f.logged("started:") == started+1 })

	f.tick("the sweep ended:")
	for range volumeQuietIntervals {
		f.tick("no sweep for another")
	}
	f.tick("the sweep ended:")
	if t.Failed() {
		t.FailNow()
	}
	testwait.Eventually(t, "the last sweep's report", func() bool { return len(f.reports()) == 3 })

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst},    // from 86%: anchors episode 1 at 86%
		{2, shimwire.VolumeEpisodeRecovery}, // from 91%, after the 65% and the stopped 86%: anchors episode 2 at 91%
		{2, shimwire.VolumeEpisodeRecovery}, // from 91%, after the 91%: a floor of 91%, no rise
	})
	assertRises(t, f)
	if said := f.logged("the settings stopped the trimmer"); said != 1 {
		t.Errorf("the settings stopped %d sweeps, want the one the case stopped: %q", said, f.linesWritten())
	}
}

// A sweep that reached the low mark neither anchors an episode nor pairs with the next reported
// sweep's start: it sends no report, and riseAt reads only a reported sweep (tick), so its start is
// no start at all to the rise. The first chain's sweeps start at 86% and 91%, which anchors episode
// 1 at 86% and leaves the 91% as the start the next reported sweep pairs with. Then five old
// entries arrive, and the next sweep, from 91%, deletes them and leaves the volume at 69.9%: a
// recovery, and no report. The two sweeps after it start at 96% and 96%, and both report the
// recovery's episode, anchored afresh at the first 96%, with no rise. The negative controls are the
// two places a copy could read the clearing sweep's start, each turning this case red: before the
// recovery its 91% pairs with the 91% before it, a floor five points above the 86% anchor, and
// raises a rise of the old chain, so the reports after it carry episode 3; after the recovery it
// anchors the new chain at 91%, so the second 96% raises a rise. The clearing sweep starts at 91%
// rather than higher, because a start above every start after it anchors nothing those starts do
// not lower at once (riseAt), and then the second control is green.
func TestASweepThatReachedTheLowMarkNeitherAnchorsNorPairs(t *testing.T) {
	f := newTrimFixture(t, 0)
	var mu sync.Mutex
	level := 86.0
	sweepReads := map[int]float64{} // the clearing sweep's own two readings, by read
	f.useVolume(func(call int) volumeUsage {
		mu.Lock()
		defer mu.Unlock()
		if reading, ok := sweepReads[call]; ok {
			return usageAt(reading)
		}
		return usageAt(level)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	at := func(reading float64, ends string) {
		t.Helper()
		mu.Lock()
		level = reading
		mu.Unlock()
		f.tick(ends)
		if t.Failed() {
			t.FailNow()
		}
	}
	reported := func(start float64, pauses int) {
		t.Helper()
		at(start, "the sweep ended")
		for range pauses {
			at(start, "no sweep for another")
		}
	}
	reported(86, volumeQuietIntervals)
	reported(91, volumeQuietIntervals)

	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			for n := range 5 {
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("late-%d", n)), root.Entry)
			}
			break
		}
	}
	mu.Lock()
	next := f.readCount() + 1
	sweepReads[next], sweepReads[next+1] = 91, 69.9
	mu.Unlock()
	before := len(f.reports())
	at(91, "the sweep ended")
	if got := len(f.reports()); got != before {
		t.Fatalf("the sweep that left the volume at 69.9%% sent %d reports, want none: it ended below the low mark", got-before)
	}
	if recovered := f.logged("(" + shimwire.VolumeEpisodeRecovery + "): the volume fell to 69.9%"); recovered != 1 {
		t.Fatalf("the sweep that left the volume at 69.9%% raised %d recoveries, want one: %q", recovered, f.linesWritten())
	}

	reported(96, volumeQuietIntervals)
	reported(96, 0)
	testwait.Eventually(t, "the last sweep's report", func() bool { return len(f.reports()) == before+2 })

	assertReportEpisodes(t, f, []reportEpisode{
		{1, shimwire.VolumeEpisodeFirst},    // from 86%: anchors episode 1 at 86%
		{1, shimwire.VolumeEpisodeFirst},    // from 91%, after the 86%: a floor of 86%, no rise
		{2, shimwire.VolumeEpisodeRecovery}, // from 96%, after the clearing sweep from 91% to 69.9%: anchors episode 2 at 96%
		{2, shimwire.VolumeEpisodeRecovery}, // from 96%, after the 96%: a floor of 96%, no rise
	})
	assertRises(t, f)
}

// A sweep whose report panics while it is being made has been read for a rise all the same: tick
// reads its start before it makes its report (riseAt), so its start anchors or pairs as a sent
// report's does, and the panic is a fault the next report carries. The sweeps start at 86%, 91%
// and 92%, and the bound of the first two reports panics (boundReport): the 86% anchors episode
// 1, the 91% pairs with the 92%, and the third sweep's report, the one sent, carries the rise and
// the two faults. The negative control is a copy that reads a sweep's start only once its report
// is made: the first two are never read, the 92% anchors the episode, and its report carries
// episode 1.
func TestASweepWhoseReportPanicsHasBeenReadForARise(t *testing.T) {
	const fell = "the report's bound fell over"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(sweepStarts(86, 91, 92))
	f.setSettings(trimSettings())
	var bounds atomic.Int32
	previous := boundReport
	boundReport = func(trim *trimmer, report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
		if bounds.Add(1) <= 2 {
			panic(fell)
		}
		return previous(trim, report)
	}
	t.Cleanup(func() { boundReport = previous })
	f.start(f.trimmer())

	sweepsAndPauses(f, 3)

	assertReportEpisodes(t, f, []reportEpisode{
		{2, shimwire.VolumeEpisodeRise}, // from 92%, after the 91% whose report panicked: the rise
	})
	assertRises(t, f, "the floor of the last two reported sweep starts, 91.0% and 92.0%, is 91.0%, 5.0 points above the 86.0%")
	if faults := f.reports()[0].Faults; len(faults) != 2 || !strings.HasPrefix(faults[0], fell) || !strings.HasPrefix(faults[1], fell) {
		t.Errorf("the report carries the faults %q, want the two panics of the reports before it", faults)
	}
}

// A trimmer started afresh carries an identifier of its own, never the one the trimmer before it
// used: the launcher builds one trimmer per launcher process (serveVolumeTrim, newTrimmer), and the
// identifier is what makes a restarted roles container a new counting line for the daemon's notice
// bounds (shimwire.LauncherVolumeReport). Two trimmers built the way the launcher builds one each
// make one sweep's report. The negative control is a copy that mints one identifier for the whole
// process and gives it to every trimmer, under which the two reports name the same trimmer.
func TestATrimmerStartedAfreshCarriesAnIdentifierOfItsOwn(t *testing.T) {
	f := newTrimFixture(t, 0) // no entry: the sweep frees nothing and reports
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	set := *trimSettings()
	var ids []string
	for range 2 {
		tr := f.trimmer() // newTrimmer, with the machine pointed at the case's tree
		tr.tick(context.Background(), &set)
		ids = append(ids, tr.id)
	}
	reports := f.reports()
	if len(reports) != 2 {
		t.Fatalf("the two trimmers sent %d reports, want one each", len(reports))
	}
	for at, report := range reports {
		if report.Trimmer != ids[at] || len(ids[at]) != 32 {
			t.Errorf("trimmer %d is %q and its report names %q, want its own identifier of 32 hex characters (randomID)", at+1, ids[at], report.Trimmer)
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("two trimmers started afresh carry the one identifier %q, want one of its own each: the daemon would count the second's episodes against the first's three notices", ids[0])
	}
}

// The rises of a chain are bounded by the room above its anchor rounded down, which a high mark
// where (100 − high mark) / 5 leaves a remainder exercises: at 84%, 3.2, so three rises and no
// fourth. The first sweep starts at the high mark, each next floor stands exactly five points above
// the anchor before it, two starts in a row at 89%, at 94% and at 99%, and three more sweeps from
// 100% raise nothing, since 100% is one point above the last anchor. Every sweep frees nothing,
// reports and pauses. TestTheRisesBeforeARecoveryAreBoundedByTheRoomAboveTheHighMark is the bound
// at a high mark of 50%, which five divides. The negative control is a copy that raises a rise at
// a full volume whatever the room above the anchor, the bound rounded up, under which the second
// 100% raises a fourth.
func TestTheRisesAboveAHighMarkOf84AreThreeTheRoomRoundedDown(t *testing.T) {
	const high = 84
	starts := []float64{high, 89, 89, 94, 94, 99, 99, 100, 100, 100}
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(sweepStarts(starts...))
	f.setSettings(&shimwire.LauncherVolumeTrim{HighPercent: high, LowPercent: 70, MinAgeMinutes: 30, IntervalSeconds: 30})
	f.start(f.trimmer())

	sweepsAndPauses(f, len(starts))

	assertRises(t, f,
		"the floor of the last two reported sweep starts, 89.0% and 89.0%, is 89.0%, 5.0 points above the 84.0%",
		"the floor of the last two reported sweep starts, 94.0% and 94.0%, is 94.0%, 5.0 points above the 89.0%",
		"the floor of the last two reported sweep starts, 99.0% and 99.0%, is 99.0%, 5.0 points above the 94.0%",
	)
	reports := f.reports()
	if last := reports[len(reports)-1]; last.Episode != 4 || last.RaisedBy != shimwire.VolumeEpisodeRise {
		t.Errorf("the last report carries episode %d (%s), want episode 4 (%s): the first and three rises", last.Episode, last.RaisedBy, shimwire.VolumeEpisodeRise)
	}
}

// A sweep that brings the volume below the low mark ends the episode, as a reading below it
// between sweeps does: "an episode ends when the trimmer's own reading falls below the low mark"
// (the spec's Design section, where its R4 names the interval reading alone), and the sweep's
// last reading is the trimmer's own. The sweeps start at 86%, 91% and 91%, which raises a rise
// and anchors episode 2 at 91%, and that episode's report is told at once, since a rise-raised
// report skips the pod's hour. Then five old entries arrive and the next sweep, from 97%, deletes
// them and leaves the volume at 69.9%, so it sends no report. One interval later the roles have
// written three tenths of a point, and every reading after it, at 70.2%, 78% and then 86%, is at
// or above the low mark. The sweep from 86% is the second fill's first report: it must carry a
// new episode, raised by the recovery, which the daemon counts afresh. A trimmer that reads a
// recovery from the interval's reading alone keeps episode 2: the second fill's reports spend
// what is left of that episode's three notices, and a rise is then read only once two starts
// stand at 96%, so once the three are spent the architect hears nothing more of the second fill
// below 96%, and nothing at all where the rise's anchor stood above 95%. Whether the tick after a
// clearing sweep reads 69.9% or 70.2% is three tenths of a point the roles write in a minute.
func TestASweepThatBringsTheVolumeBelowTheLowMarkEndsTheEpisode(t *testing.T) {
	f := newTrimFixture(t, 0)
	var mu sync.Mutex
	level := 86.0
	sweepReads := map[int]float64{} // the clearing sweep's own two readings, by read
	f.useVolume(func(call int) volumeUsage {
		mu.Lock()
		defer mu.Unlock()
		if reading, ok := sweepReads[call]; ok {
			return usageAt(reading)
		}
		return usageAt(level)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	at := func(reading float64, ends string) {
		t.Helper()
		mu.Lock()
		level = reading
		mu.Unlock()
		f.tick(ends)
		if t.Failed() {
			t.FailNow()
		}
	}
	for _, start := range []float64{86, 91, 91} {
		at(start, "the sweep ended")
		for range volumeQuietIntervals {
			at(start, "no sweep for another")
		}
	}
	before := f.reports()
	if last := before[len(before)-1]; last.Episode != 2 || last.RaisedBy != shimwire.VolumeEpisodeRise {
		t.Fatalf("the sweeps from 86%%, 91%% and 91%% left the trimmer in episode %d (%s), want 2 (%s); the reports were %q",
			last.Episode, last.RaisedBy, shimwire.VolumeEpisodeRise, reportsSeen(f))
	}

	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			for n := range 5 {
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("late-%d", n)), root.Entry)
			}
			break
		}
	}
	mu.Lock()
	next := f.readCount() + 1
	sweepReads[next], sweepReads[next+1] = 97, 69.9
	mu.Unlock()
	at(97, "the sweep ended")
	if got := len(f.reports()); got != len(before) {
		t.Fatalf("the sweep that left the volume at 69.9%% sent %d reports, want none: it ended below the low mark", got-len(before))
	}
	at(70.2, "nothing to sweep")
	at(78, "nothing to sweep")
	at(86, "the sweep ended")

	reports := f.reports()
	if len(reports) != len(before)+1 {
		t.Fatalf("the sweep from 86%% of the second fill sent %d reports, want one", len(reports)-len(before))
	}
	if refill := reports[len(reports)-1]; refill.Episode <= 2 || refill.RaisedBy != shimwire.VolumeEpisodeRecovery {
		t.Errorf("the second fill's first report carries episode %d (%s), want a new episode raised by %q: the sweep before it left the volume at 69.9%%, below the low mark, and the episode it was in is over; the reports were %q",
			refill.Episode, refill.RaisedBy, shimwire.VolumeEpisodeRecovery, reportsSeen(f))
	}
}

// A sweep that ends exactly at the low mark ends the episode, as one that ends under it does: the
// sweep stops at the first reading at or below the low mark and reports only where it ends above
// it (sweep), so a recovery read only under the mark would leave a sweep that lands on it neither
// reporting nor recovering, and the episode whose notices are spent would carry through the next
// fill. The case is TestASweepThatBringsTheVolumeBelowTheLowMarkEndsTheEpisode with the clearing
// sweep's last reading at 70.0% in place of 69.9%, which the fixture reads as exactly the mark;
// that case is the reading a tenth under the boundary, and this one the reading on it. At
// f01689f0, where the recovery asked for a reading under the mark, the second fill's first report
// carried episode 2 (rise).
func TestASweepThatEndsExactlyAtTheLowMarkEndsTheEpisode(t *testing.T) {
	if mark := usageAt(70).Percent(); mark != 70 {
		t.Fatalf("the fixture reads 70%% as %v, want exactly the low mark 70: the case would not stand on the boundary", mark)
	}
	f := newTrimFixture(t, 0)
	var mu sync.Mutex
	level := 86.0
	sweepReads := map[int]float64{} // the clearing sweep's own two readings, by read
	f.useVolume(func(call int) volumeUsage {
		mu.Lock()
		defer mu.Unlock()
		if reading, ok := sweepReads[call]; ok {
			return usageAt(reading)
		}
		return usageAt(level)
	})
	f.setSettings(trimSettings())
	f.start(f.trimmer())

	at := func(reading float64, ends string) {
		t.Helper()
		mu.Lock()
		level = reading
		mu.Unlock()
		f.tick(ends)
		if t.Failed() {
			t.FailNow()
		}
	}
	for _, start := range []float64{86, 91, 91} {
		at(start, "the sweep ended")
		for range volumeQuietIntervals {
			at(start, "no sweep for another")
		}
	}
	before := f.reports()
	if last := before[len(before)-1]; last.Episode != 2 || last.RaisedBy != shimwire.VolumeEpisodeRise {
		t.Fatalf("the sweeps from 86%%, 91%% and 91%% left the trimmer in episode %d (%s), want 2 (%s); the reports were %q",
			last.Episode, last.RaisedBy, shimwire.VolumeEpisodeRise, reportsSeen(f))
	}

	for _, root := range f.roots {
		if root.Entry == ImmediateChildren {
			for n := range 5 {
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("late-%d", n)), root.Entry)
			}
			break
		}
	}
	mu.Lock()
	next := f.readCount() + 1
	sweepReads[next], sweepReads[next+1] = 97, 70.0
	mu.Unlock()
	at(97, "the sweep ended")
	if got := len(f.reports()); got != len(before) {
		t.Fatalf("the sweep that left the volume at 70.0%% sent %d reports, want none: it ended at the low mark", got-len(before))
	}
	at(70.2, "nothing to sweep")
	at(78, "nothing to sweep")
	at(86, "the sweep ended")

	reports := f.reports()
	if len(reports) != len(before)+1 {
		t.Fatalf("the sweep from 86%% of the second fill sent %d reports, want one", len(reports)-len(before))
	}
	if refill := reports[len(reports)-1]; refill.Episode <= 2 || refill.RaisedBy != shimwire.VolumeEpisodeRecovery {
		t.Errorf("the second fill's first report carries episode %d (%s), want a new episode raised by %q: the sweep before it left the volume at 70.0%%, at the low mark, and the episode it was in is over; the reports were %q",
			refill.Episode, refill.RaisedBy, shimwire.VolumeEpisodeRecovery, reportsSeen(f))
	}
}

// An interval reading exactly at the low mark after one at the high mark is a recovery, and one a
// tenth above the mark is none: the boundary of the interval's reading is the sweep's, at or below
// the low mark (episodeOf). Each side fills to 90%, where a sweep frees a point and a half and
// reports in episode 1, reads the boundary's side between two intervals, and fills to 90% again,
// whose sweep's report carries the episode that reading left: a new one raised by the recovery on
// the mark, the first one still a tenth above it. At f01689f0, where the recovery asked for a
// reading under the mark, the side on the mark failed.
func TestAnIntervalReadingAtTheLowMarkIsARecoveryAndOneATenthAboveItIsNot(t *testing.T) {
	for _, side := range []struct {
		name    string
		reading float64
		want    reportEpisode
	}{
		{"exactly at the low mark", 70.0, reportEpisode{2, shimwire.VolumeEpisodeRecovery}},
		{"a tenth above the low mark", 70.1, reportEpisode{1, shimwire.VolumeEpisodeFirst}},
	} {
		t.Run(side.name, func(t *testing.T) {
			if percent := usageAt(side.reading).Percent(); (side.reading == 70 && percent != 70) || (side.reading != 70 && percent <= 70) {
				t.Fatalf("the fixture reads %v%% as %v, which is not the side of the low mark 70 this case stands on", side.reading, percent)
			}
			f := newTrimFixture(t, 1)
			root := f.roots[0]
			added := 0
			f.setOnRead(func(int) {
				// One fresh entry per reading, so that every sweep has something to delete and
				// the script stays in step: a sweep with no deletion takes no reading of its own.
				added++
				f.addTree(filepath.Join(root.Path, fmt.Sprintf("go-build7%08d", added)), root.Entry)
			})
			f.useVolume(script(
				90, 88.5, // the first sweep: episode 1, first
				side.reading,
				90, 88.5, // the next fill's sweep, whose report carries the episode the reading left
			))
			f.setSettings(trimSettings())
			f.start(f.trimmer())

			for at, ends := range []string{"the sweep ended", "nothing to sweep", "the sweep ended"} {
				f.tick(ends)
				if t.Failed() {
					t.Fatalf("the case stopped at interval %d", at+1)
				}
			}

			assertReportEpisodes(t, f, []reportEpisode{{1, shimwire.VolumeEpisodeFirst}, side.want})
			if recoveries := f.logged("(" + shimwire.VolumeEpisodeRecovery + ")"); recoveries != side.want.episode-1 {
				t.Errorf("the reading of %.1f%% raised %d recoveries, want %d; the trimmer wrote %q",
					side.reading, recoveries, side.want.episode-1, f.linesWritten())
			}
		})
	}
}

// An interval reading exactly at the high mark makes a sweep, raises episode 1 (first) and puts
// the trimmer above the mark, so that a reading at the low mark after it is a recovery, and one a
// byte under the mark does none of the three: the boundary is at or above the high mark, for the
// sweep (tick), for the first episode and for the reading above the mark (episodeOf) alike, as R1
// has it — "At or above the high mark it makes a sweep". The other cases read the mark's sides at
// 84.9 and 90 (TestAReadingBelowTheHighMarkSweepsNothing,
// TestTheEpisodeNumberFollowsTheVolumeAcrossTheMarks), and a trimmer that swept only above the
// mark, or from a twentieth of a point under it, is green with both. A byte is the least a
// reading of the volume moves by, so the mark moved either way by any amount turns a side red.
//
// Each side is two intervals of a trimmer that has read nothing before them, over a volume of one
// entry per root, every one old enough to go. The side on the mark deletes them, reads the volume
// once more at the sweep's end, a sweep's own last reading, and reports from episode 1 with the
// volume still above the low mark, and its second interval, inside the pause that sweep engaged,
// reads the low mark and raises a recovery; the side a byte under the mark reads the volume once,
// deletes nothing and raises no episode, and its reading at the low mark raises nothing either.
// The intervals are driven through tick itself rather than the running loop, as in
// TestAVolumeTheTrimmerCannotReadIsSaidOnce, so a boundary moved either way is a failure read at
// once rather than a wait for a line that never comes.
//
// A trimmer still in episode 0 reads a reading under the mark no further than the first episode's
// own test of it (episodeOf), so a byte under the mark meets the test of the reading above the
// mark only once an episode is open and a recovery has closed the last stay above the mark. The
// last subtest puts readings through episodeOf there, in the order tick reads them: a byte under
// the mark leaves the low mark no recovery, and the mark itself makes it one.
func TestAnIntervalReadingAtTheHighMarkSweepsAndRaisesTheFirstEpisodeAndOneAByteUnderItDoesNot(t *testing.T) {
	under := percentBytes(85) - 1
	aByteUnder := volumeUsage{UsedBytes: under, AvailableBytes: fixtureVolumeBytes - under}
	if at, below := usageAt(85).Percent(), aByteUnder.Percent(); at != 85 || below >= 85 {
		t.Fatalf("the fixture reads the high mark 85%% as %v and a byte under it as %v, which are not the two sides of the mark this case stands on", at, below)
	}
	for _, side := range []struct {
		name string
		// The first interval's reading, the sweep's last where it sweeps, above the low mark so
		// that the sweep reports, and the second interval's reading, at the low mark.
		readings []volumeUsage
		reads    int    // in the first interval: its reading, and the sweep's last where it sweeps
		episode  int    // the episode the first interval leaves the trimmer in
		raisedBy string // and what raised it
		recovery bool   // whether the second interval's reading at the low mark is a recovery
	}{
		{"exactly at the high mark", []volumeUsage{usageAt(85), usageAt(84.6), usageAt(70)}, 2, 1, shimwire.VolumeEpisodeFirst, true},
		{"a byte under the high mark", []volumeUsage{aByteUnder, usageAt(70)}, 1, 0, "", false},
	} {
		t.Run(side.name, func(t *testing.T) {
			percent := side.readings[0].Percent()
			f := newTrimFixture(t, 1)
			f.useVolume(func(call int) volumeUsage { return side.readings[min(call, len(side.readings))-1] })
			set := *trimSettings()
			f.setSettings(&set)
			tr := f.trimmer()

			if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
				t.Fatalf("the interval ended the trimmer (%v), want it to go on", ending)
			}

			swept := side.episode == 1
			if reads := f.readCount(); reads != side.reads {
				t.Errorf("the interval reading %v%% read the volume %d times, want %d", percent, reads, side.reads)
			}
			if gone := len(f.gone()); (swept && gone != len(f.entries)) || (!swept && gone != 0) {
				t.Errorf("the interval reading %v%% deleted %d of the %d entries, want every one where it sweeps and none where it does not; the trimmer wrote %q",
					percent, gone, len(f.entries), f.linesWritten())
			}
			if tr.episode != side.episode || tr.raisedBy != side.raisedBy {
				t.Errorf("the interval reading %v%% left the trimmer in episode %d raised by %q, want %d raised by %q; the trimmer wrote %q",
					percent, tr.episode, tr.raisedBy, side.episode, side.raisedBy, f.linesWritten())
			}
			var want []reportEpisode
			if swept {
				want = []reportEpisode{{1, shimwire.VolumeEpisodeFirst}}
			}
			assertReportEpisodes(t, f, want)

			if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
				t.Fatalf("the second interval ended the trimmer (%v), want it to go on", ending)
			}
			episode, raisedBy := side.episode, side.raisedBy
			if side.recovery {
				episode, raisedBy = side.episode+1, shimwire.VolumeEpisodeRecovery
			}
			if tr.episode != episode || tr.raisedBy != raisedBy {
				t.Errorf("the reading at the low mark after the interval reading %v%% left the trimmer in episode %d raised by %q, want %d raised by %q: a reading at the high mark is a reading above it, which the low mark after it recovers from; the trimmer wrote %q",
					percent, tr.episode, tr.raisedBy, episode, raisedBy, f.linesWritten())
			}
			f.assertProtected()
			f.assertPrefixed()
		})
	}

	t.Run("a byte under the high mark and exactly at it after a recovery", func(t *testing.T) {
		f := newTrimFixture(t, 0)
		tr := f.trimmer()
		set := *trimSettings()
		for at, step := range []struct {
			reading  volumeUsage
			episode  int
			raisedBy string
		}{
			{usageAt(90), 1, shimwire.VolumeEpisodeFirst},
			{usageAt(70), 2, shimwire.VolumeEpisodeRecovery},
			{aByteUnder, 2, shimwire.VolumeEpisodeRecovery},
			{usageAt(70), 2, shimwire.VolumeEpisodeRecovery}, // no recovery: nothing since the last one was at the high mark
			{usageAt(85), 2, shimwire.VolumeEpisodeRecovery},
			{usageAt(70), 3, shimwire.VolumeEpisodeRecovery}, // a recovery: the reading before it was at the high mark
		} {
			tr.episodeOf(step.reading.Percent(), set)
			if tr.episode != step.episode || tr.raisedBy != step.raisedBy {
				t.Fatalf("reading %d, %v%%, left the trimmer in episode %d raised by %q, want %d raised by %q; the trimmer wrote %q",
					at+1, step.reading.Percent(), tr.episode, tr.raisedBy, step.episode, step.raisedBy, f.linesWritten())
			}
		}
		f.assertPrefixed()
	})
}

// A sweep whose reading stands a byte above the low mark goes on deleting, and one whose last
// reading stands there reports: the sweep stops at the first reading at or below the low mark and
// reports where it ends above it (sweep). The cases on the mark read it at 70.0 — the reading
// after the two hundredth deletion of TestTheSweepDeletesOldestFirstAcrossEveryRootUntilTheLowMark,
// where the sweep stops and asks for no report, and the last reading of
// TestASweepThatEndsExactlyAtTheLowMarkEndsTheEpisode — and every reading above the mark that a
// sweep goes on from or reports at stands four points or more over it, so a stop or a report moved
// up is green with them. A byte is the least a reading of the volume moves by, so a stop or a
// report moved up by any amount turns this case red. The sweep here has a hundred and fifty
// entries and reads the volume a byte above 70% after its hundredth deletion and again at its end:
// it deletes every entry, ends with none eligible, and asks for a report.
func TestASweepReadingAByteAboveTheLowMarkGoesOnAndReportsWhereItEnds(t *testing.T) {
	const entries = 150
	above := percentBytes(70) + 1
	aByteAbove := volumeUsage{UsedBytes: above, AvailableBytes: fixtureVolumeBytes - above}
	if percent := aByteAbove.Percent(); percent <= 70 {
		t.Fatalf("the fixture reads a byte above 70%% as %v, which is not above the low mark 70 this case stands over", percent)
	}
	f := newTrimFixture(t, 0)
	root := f.roots[3]
	for at := range entries {
		f.addTree(filepath.Join(root.Path, fmt.Sprintf("tool-%03d", at)), root.Entry)
	}
	f.useVolume(func(int) volumeUsage { return aByteAbove })
	f.setSettings(trimSettings())

	out := f.trimmer().sweep(context.Background(), *trimSettings(), usageAt(90))

	if out.deletions != entries || strings.Contains(out.reason, "low mark") {
		t.Errorf("the sweep deleted %d of %d entries and stopped because %q, want every one deleted: a reading a byte above the low mark is no reason to stop",
			out.deletions, entries, out.reason)
	}
	if reads := f.readCount(); reads != 2 {
		t.Errorf("the sweep read the volume %d times, want 2: after its hundredth deletion and at its end", reads)
	}
	if !out.report {
		t.Errorf("the sweep that ended at %v%%, a byte above the low mark, asks for no report, want one: R4 has a sweep that ends above the low mark send one", out.last.Percent())
	}
}

// A reading a byte above the low mark after one at the high mark is no recovery, and a reading at
// the low mark after it is one: a recovery is a reading of the trimmer's own at or below the low
// mark (episodeOf). The case on the mark's two sides through the running loop,
// TestAnIntervalReadingAtTheLowMarkIsARecoveryAndOneATenthAboveItIsNot, stands its side above the
// mark a tenth over it, with which a recovery read up to a twentieth of a point above the mark is
// as green. A byte is the least a reading of the volume moves by, so a recovery moved up by any
// amount turns this case red. The readings are put through episodeOf in the order tick puts its
// readings through it, as TestAFloorFivePointsAboveTheAnchorRaisesTheEpisodeAndOneUnderItDoesNot
// puts its starts.
func TestAReadingAByteAboveTheLowMarkIsNoRecoveryAndOneAtItIs(t *testing.T) {
	above := percentBytes(70) + 1
	aByteAbove := volumeUsage{UsedBytes: above, AvailableBytes: fixtureVolumeBytes - above}
	if at, over := usageAt(70).Percent(), aByteAbove.Percent(); at != 70 || over <= 70 {
		t.Fatalf("the fixture reads the low mark 70%% as %v and a byte above it as %v, which are not the two sides of the mark this case stands on", at, over)
	}
	f := newTrimFixture(t, 0)
	tr := f.trimmer()
	set := *trimSettings()
	for at, step := range []struct {
		reading  volumeUsage
		episode  int
		raisedBy string
	}{
		{usageAt(90), 1, shimwire.VolumeEpisodeFirst},
		{aByteAbove, 1, shimwire.VolumeEpisodeFirst},
		{usageAt(70), 2, shimwire.VolumeEpisodeRecovery},
	} {
		tr.episodeOf(step.reading.Percent(), set)
		if tr.episode != step.episode || tr.raisedBy != step.raisedBy {
			t.Fatalf("reading %d, %v%%, left the trimmer in episode %d raised by %q, want %d raised by %q; the trimmer wrote %q",
				at+1, step.reading.Percent(), tr.episode, tr.raisedBy, step.episode, step.raisedBy, f.linesWritten())
		}
	}
	f.assertPrefixed()
}

// sweepsAndPauses ticks the loop through count sweeps of a volume no sweep brings down, each
// followed by the intervals of the pause it engages (volumeQuietIntervals). The loop takes a tick
// only once the interval before it has ended, so by the end every sweep's report has been sent.
func sweepsAndPauses(f *trimFixture, count int) {
	f.t.Helper()
	for sweep := range count {
		f.tick("the sweep ended")
		for range volumeQuietIntervals {
			f.tick("no sweep for another")
		}
		if f.t.Failed() {
			f.t.Fatalf("the case stopped at sweep %d", sweep+1)
		}
	}
}

// reportEpisode is the episode a report carries and what raised it.
type reportEpisode struct {
	episode  int
	raisedBy string
}

// assertReportEpisodes holds the reports the trimmer sent to want, one entry per report in order.
func assertReportEpisodes(t *testing.T, f *trimFixture, want []reportEpisode) {
	t.Helper()
	reports := f.reports()
	if len(reports) != len(want) {
		t.Fatalf("the trimmer sent %d reports, want %d, one per sweep that ended above the low mark: %+v", len(reports), len(want), reports)
	}
	for at, report := range reports {
		if report.Episode != want[at].episode || report.RaisedBy != want[at].raisedBy {
			t.Errorf("report %d carries episode %d raised by %q, want %d by %q",
				at+1, report.Episode, report.RaisedBy, want[at].episode, want[at].raisedBy)
		}
	}
}

// assertOneRise holds that the trimmer raised one episode by a rise and nothing else besides the
// first, in a line holding each of says: the floor that raised it and the anchor it was measured
// from.
func assertOneRise(t *testing.T, f *trimFixture, says ...string) {
	t.Helper()
	if raised := f.logged("episode "); raised != 2 {
		t.Errorf("the trimmer raised the episode %d times, want twice, the first episode and one rise; it wrote %q", raised, f.linesWritten())
	}
	var rises []string
	for _, line := range f.linesWritten() {
		if strings.Contains(line, "("+shimwire.VolumeEpisodeRise+")") {
			rises = append(rises, line)
		}
	}
	if len(rises) != 1 {
		t.Fatalf("the trimmer wrote %d lines raising a rise, want one: %q", len(rises), rises)
	}
	for _, want := range says {
		if !strings.Contains(rises[0], want) {
			t.Errorf("the rise was raised by the line %q, want it to hold %q", rises[0], want)
		}
	}
}

// assertRises holds that the trimmer wrote one line raising a rise for each of want, in order,
// each line holding its want.
func assertRises(t *testing.T, f *trimFixture, want ...string) {
	t.Helper()
	var rises []string
	for _, line := range f.linesWritten() {
		if strings.Contains(line, "("+shimwire.VolumeEpisodeRise+")") {
			rises = append(rises, line)
		}
	}
	if len(rises) != len(want) {
		t.Fatalf("the trimmer wrote %d lines raising a rise, want %d: %q", len(rises), len(want), rises)
	}
	for at, says := range want {
		if !strings.Contains(rises[at], says) {
			t.Errorf("rise %d was raised by the line %q, want it to hold %q", at+1, rises[at], says)
		}
	}
}

// sweepStarts is a volume whose sweeps start at starts in turn, one to each sweep and its pause,
// for a fixture whose sweeps free nothing and pause. Each pause reads the lower of the start before
// it and the start after it, so a start above both of its neighbours is read at that start alone,
// as a transient is; after the last start the volume stays there.
func sweepStarts(starts ...float64) func(int) volumeUsage {
	return func(interval int) volumeUsage {
		sweep := min((interval-1)/(1+volumeQuietIntervals), len(starts)-1)
		if (interval-1)%(1+volumeQuietIntervals) == 0 || sweep == len(starts)-1 {
			return usageAt(starts[sweep])
		}
		return usageAt(min(starts[sweep], starts[sweep+1]))
	}
}

// episodeFirst is the first report of one episode: its place among the reports, counted from 0,
// the volume it carried in percent, and what raised the episode.
type episodeFirst struct {
	report   int
	percent  float64
	raisedBy string
}

// firstReports is the first report of each episode the trimmer reported, in order.
func firstReports(f *trimFixture) []episodeFirst {
	var firsts []episodeFirst
	seen := map[int]bool{}
	for at, report := range f.reports() {
		if seen[report.Episode] {
			continue
		}
		seen[report.Episode] = true
		firsts = append(firsts, episodeFirst{at, reportPercent(report), report.RaisedBy})
	}
	return firsts
}

// reportsSeen is every report the trimmer sent, as a failure message names them.
func reportsSeen(f *trimFixture) []string {
	var seen []string
	for _, report := range f.reports() {
		seen = append(seen, fmt.Sprintf("episode %d (%s) at %.1f%%", report.Episode, report.RaisedBy, reportPercent(report)))
	}
	return seen
}

// reportPercent is the volume a report carried, in percent.
func reportPercent(report shimwire.LauncherVolumeReport) float64 {
	return 100 * float64(report.UsedBytes) / float64(report.UsedBytes+report.FreeBytes)
}

// One report per sweep that ends with the volume still above the low mark, and none at all from
// a sweep that reached it: the daemon hears about the volume the trimmer could not bring down,
// and nothing about the one it could.
func TestOneReportPerSweepThatEndsAboveTheLowMarkAndNoneWhenItReachesIt(t *testing.T) {
	f := newTrimFixture(t, 10) // forty entries, four points: the sweep ends at 86%
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	f.tick("the sweep ended")
	testwait.Eventually(t, "the report of the sweep", func() bool { return len(f.reports()) == 1 })
	report := f.reports()[0]
	switch {
	case report.Trimmer != tr.id || report.Trimmer == "":
		t.Errorf("the report names the trimmer %q, want the identifier %q minted for this launcher process", report.Trimmer, tr.id)
	case report.Episode != 1 || report.RaisedBy != shimwire.VolumeEpisodeFirst:
		t.Errorf("the report carries episode %d raised by %q, want the first episode", report.Episode, report.RaisedBy)
	case report.UsedBytes != int64(percentBytes(86)):
		t.Errorf("the report carries %d used bytes, want the %d of the sweep's last reading", report.UsedBytes, percentBytes(86))
	case report.FreeBytes != int64(fixtureVolumeBytes-percentBytes(86)):
		t.Errorf("the report carries %d free bytes, want the %d of the sweep's last reading", report.FreeBytes, fixtureVolumeBytes-percentBytes(86))
	case report.FreedBytes != int64(percentBytes(90)-percentBytes(86)):
		t.Errorf("the report carries %d freed bytes, want the %d between the sweep's first reading and its last", report.FreedBytes, percentBytes(90)-percentBytes(86))
	case report.StopReason == "":
		t.Error("the report says nothing about why the sweep stopped")
	case len(report.Sizes) == 0:
		t.Error("the report carries no sizes, want the walk's")
	case report.SizesAt.IsZero():
		t.Error("the report carries no time for its sizes")
	case report.SwitchedOff || len(report.Faults) != 0:
		t.Errorf("the report of a sweep that ran carries faults %v and switched-off %t, want neither", report.Faults, report.SwitchedOff)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse this report: %v", err)
	}
	for _, size := range report.Sizes {
		if !strings.HasPrefix(size.Path, f.root) {
			t.Errorf("the report carries a size for %s, outside the volume %s it measured", size.Path, f.root)
		}
	}

	t.Run("a sweep that reaches the low mark sends none", func(t *testing.T) {
		f := newTrimFixture(t, 80) // three hundred and twenty entries: the low mark is reachable
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		f.start(tr)

		f.tick("the sweep ended")
		if f.logged("back at the low mark") != 1 {
			t.Fatalf("the sweep did not reach the low mark; the trimmer wrote %q", f.linesWritten())
		}
		if reports := f.reports(); len(reports) != 0 {
			t.Errorf("the trimmer sent %d reports for a sweep that reached the low mark, want none: %+v", len(reports), reports)
		}
	})
}

// The lines a sweep ends on say what the sweep freed, and a sweep the roles outwrote, whose last
// reading of the volume is above its first, freed nothing: the sweep's line says the volume grew
// by that many bytes while the sweep ran, and the pause's line after it by that many points, as
// the notice the daemon makes of its report says it (internal/workflow's volumeNoticeReason),
// and neither says the sweep freed a negative number. The case sweeps a volume whose every
// reading after the interval's own is half a point above it. The negative control is the two
// lines as they stood at a3f5d0965f3f, which read `40 entries deleted, -250000000 B freed` and
// `the sweep freed -0.50 points of the volume, under 1`.
func TestTheLineOfASweepTheRolesOutwroteSaysTheVolumeGrew(t *testing.T) {
	const grown = fixtureVolumeBytes / 200
	f := newTrimFixture(t, 10)
	f.useVolume(func(call int) volumeUsage {
		used := percentBytes(90)
		if call > 1 {
			used += grown
		}
		return volumeUsage{UsedBytes: used, AvailableBytes: fixtureVolumeBytes - used}
	})
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	// The pause's line is written after the sweep's, so the tick waits for it.
	f.tick("no new sweep for the next")
	for _, want := range []string{
		fmt.Sprintf("entries deleted, the volume grew by %d B while the sweep ran, leaving the volume at", grown),
		fmt.Sprintf("the volume grew by 0.50 points while the sweep ran, so the sweep freed under 1: no new sweep for the next %d intervals", volumeQuietIntervals),
	} {
		if said := f.logged(want); said != 1 {
			t.Errorf("the trimmer wrote %d lines saying %q, want one: %q", said, want, f.linesWritten())
		}
	}
	if said := f.logged(fmt.Sprintf("-%d", grown)) + f.logged("B freed") + f.logged("freed -"); said != 0 {
		t.Errorf("the trimmer wrote %d lines of bytes or points freed by a sweep that freed none: %q", said, f.linesWritten())
	}
}

// A sweep that freed exactly nothing says it freed 0 B and 0.00 points, and one whose volume grew
// by a single byte while it ran says the volume grew, in its sweep's line and in its pause's: each
// line reads the volume as grown only where the sweep's last reading is above its first (tick,
// which tests the bytes for the one and the points for the other), and the case above stands its
// last reading half a point above, with which lines that took a sweep freeing nothing for one the
// volume grew under, or a byte's growth for none, are as green. Each sweep deletes the fixture's
// four entries from a volume whose readings the case writes out, and is one interval driven
// through tick.
func TestASweepThatFreedNothingSaysSoAndOneTheVolumeGrewAByteUnderSaysThat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grown uint64 // what the sweep's last reading holds over its first
		want  []string
	}{
		{"freed nothing", 0, []string{
			"4 entries deleted, 0 B freed, leaving the volume at",
			"the sweep freed 0.00 points of the volume, under 1: no new sweep for the next",
		}},
		{"grew by a byte", 1, []string{
			"4 entries deleted, the volume grew by 1 B while the sweep ran, leaving the volume at",
			"the volume grew by 0.00 points while the sweep ran, so the sweep freed under 1: no new sweep for the next",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrimFixture(t, 1)
			f.useVolume(func(call int) volumeUsage {
				used := percentBytes(90)
				if call > 1 {
					used += tc.grown
				}
				return volumeUsage{UsedBytes: used, AvailableBytes: fixtureVolumeBytes - used}
			})
			set := *trimSettings()
			f.setSettings(&set)
			tr := f.trimmer()

			tr.tick(context.Background(), &set)

			for _, want := range tc.want {
				if said := f.logged(want); said != 1 {
					t.Errorf("the trimmer wrote %d lines saying %q, want one: %q", said, want, f.linesWritten())
				}
			}
		})
	}
}

// A report the connection refuses is held and sent on the next attempt, as the launcher's
// bundleRestore holds its own confirmation: a report the daemon never got is a volume nobody
// was told about. An answer with no settings discards it, because the daemon that sent that
// answer has no case for a report; settings the trimmer refuses keep it held and try it
// (awaitSettings).
func TestTheTrimmerHoldsAReportItsSendRefused(t *testing.T) {
	f := newTrimFixture(t, 25) // a hundred entries, ten points: the volume ends below the high mark
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	f.refuseSends(errors.New("the bound connection is gone"))
	tr := f.trimmer()
	f.start(tr)

	f.tick("the sweep ended")
	testwait.Eventually(t, "the trimmer to hold the report its send refused", func() bool {
		return f.logged("holding it for the next attempt") == 1
	})
	if reports := f.reports(); len(reports) != 0 {
		t.Fatalf("the trimmer sent %d reports although every send was refused: %+v", len(reports), reports)
	}

	f.refuseSends(nil)
	f.tick("nothing to sweep")
	testwait.Eventually(t, "the held report to be sent on the next attempt", func() bool { return len(f.reports()) == 1 })
	if report := f.reports()[0]; report.FreedBytes != int64(percentBytes(90)-percentBytes(80)) {
		t.Errorf("the report that got across carries %d freed bytes, want the %d of the sweep that was held", report.FreedBytes, percentBytes(90)-percentBytes(80))
	}

	t.Run("settings that stop the trimmer discard a held report", func(t *testing.T) {
		f := newTrimFixture(t, 25)
		f.useVolume(f.modelled(90))
		f.setSettings(trimSettings())
		f.refuseSends(errors.New("the bound connection is gone"))
		tr := f.trimmer()
		_, done := f.start(tr)

		f.tick("the sweep ended")
		testwait.Eventually(t, "the trimmer to hold the report its send refused", func() bool {
			return f.logged("holding it for the next attempt") == 1
		})
		f.refuseSends(nil)
		f.setSettings(nil)
		f.tick("discarded")
		testwait.Eventually(t, "the trimmer to wait for settings it can run", func() bool {
			return f.logged("no settings are in force, so the trimmer stays off until the daemon sends settings it can run") == 1
		})
		select {
		case <-done:
			t.Fatal("run returned when the settings went away, want it waiting for them to come back: the pod keeps its trimmer for the life of the pod")
		default:
		}
		if reports := f.reports(); len(reports) != 0 {
			t.Errorf("the trimmer sent %d reports after being stopped, want the held one discarded: %+v", len(reports), reports)
		}
		if tr.held != nil {
			t.Errorf("the trimmer still holds a report after being stopped: %+v", tr.held)
		}
	})
}

// A report that replaces a held one carries what only the held one carried. The trimmer clears
// its faults and refused /proc reads into the report it builds (report), so when report A is
// held, its flush fails and the newer report B fails too, B replaces A and must take A's fault
// and refused read with it: R1 has the trimmer carry a fault into the next report it sends, and
// R3 a refused read. The case makes two failed sends in a row — A's own, then A's flush and B's
// together — and then lets one through. The negative control is the replacement as it stood at
// b7085e21, `t.held = &report`, which sent B with no fault and no refused read. The log line of
// A's failed flush is read too: it said A was held for the next attempt with no condition, two
// lines before B took its place.
func TestAReportThatReplacesAHeldOneCarriesItsFaultsAndRefusedReads(t *testing.T) {
	const (
		refusedA = "process 7: /proc/7/fd: permission denied"
		refusedB = "process 8: /proc/8/fd: permission denied"
	)
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	tr.episode, tr.raisedBy = 1, shimwire.VolumeEpisodeFirst
	f.refuseSends(errors.New("the bound connection is gone"))

	tr.fault("a panic of A's interval", []byte("goroutine 1 [running]:\nA's stack"))
	tr.carryRefused([]string{refusedA}) // as A's sweep hands the trimmer what it met (sweep)
	tr.deliver(tr.report(context.Background(), sweepOutcome{reason: "A"}))
	tr.carryRefused([]string{refusedB, refusedA}) // B's sweep met both
	tr.deliver(tr.report(context.Background(), sweepOutcome{reason: "B"}))
	if tr.held == nil || tr.held.StopReason != "B" {
		t.Fatalf("the trimmer holds %+v after two failed sends, want report B in place of A", tr.held)
	}
	if f.logged("holding it for the next attempt") != 1 || f.logged("it replaces the held report, taking in that report's 1 recovered faults and 1 refused /proc reads with its own, each kept or counted under the report's bound") != 1 {
		t.Errorf("the trimmer wrote %q, want A held once and B said to replace it with A's fault and refused read", f.linesWritten())
	}
	if flushed := f.logged("the held report could not be sent either"); flushed != 1 || f.logged("either (the bound connection is gone); holding it for the next attempt") != 0 {
		t.Errorf("the trimmer wrote %q, want A's failed flush said once and not as a report held for the next attempt", f.linesWritten())
	}

	f.refuseSends(nil)
	tr.flush()
	sent := f.reports()
	if len(sent) != 1 {
		t.Fatalf("the trimmer sent %d reports once the connection took them, want B alone: %+v", len(sent), sent)
	}
	report := sent[0]
	if report.StopReason != "B" {
		t.Errorf("the report sent stopped because %q, want B's", report.StopReason)
	}
	if len(report.Faults) != 1 || !strings.HasPrefix(report.Faults[0], "a panic of A's interval") || report.FaultsOmitted != 0 {
		t.Errorf("the report sent carries faults %q and %d left out, want the one fault A carried", report.Faults, report.FaultsOmitted)
	}
	if want := []string{refusedA, refusedB}; !slices.Equal(report.RefusedReads, want) || report.RefusedReadsOmitted != 0 {
		t.Errorf("the report sent names the refused reads %q and %d left out, want %q: A's first, then B's own, and A's once though B met it again", report.RefusedReads, report.RefusedReadsOmitted, want)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the report sent: %v", err)
	}
	if tr.held != nil {
		t.Errorf("the trimmer still holds %+v after the connection took it", tr.held)
	}
}

// A report takes the faults and the refused /proc reads the trimmer carries only once the bounded
// report that holds them exists (report): a panic in the bound before that leaves them with the
// trimmer, and the next report carries them, before the fault of that panic, which tick recovers.
// The case carries one fault and one refused read of an earlier sweep that sent no report, makes
// the bound of the next sweep's report panic, and reads the report of the sweep after it. The
// negative control is report as it stood at e6725f56, which took them from the trimmer before the
// bound ran: the report sent carried the bound's own fault alone and no refused read.
func TestAReportWhoseBoundPanicsLeavesWhatItCarriesForTheNext(t *testing.T) {
	const refused = "process 7: /proc/7/fd: permission denied"
	f := newTrimFixture(t, 10) // forty entries, four points: the first sweep leaves 86% and sets no pause
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	set := *trimSettings()
	tr.fault("a panic of an earlier interval", []byte("goroutine 1 [running]:\nan earlier stack"))
	tr.carryRefused([]string{refused})

	previous := boundReport
	boundReport = func(*trimmer, shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
		panic("the bound fell over")
	}
	t.Cleanup(func() { boundReport = previous })
	if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
		t.Fatalf("the interval whose bound panicked ended the trimmer (%v), want it passed over", ending)
	}
	if faults := f.logged("recovered a panic"); faults != 2 {
		t.Fatalf("the trimmer wrote %d faults, want the earlier one and the bound's: %q", faults, f.linesWritten())
	}
	if sent := len(f.reports()); sent != 0 {
		t.Fatalf("the trimmer sent %d reports from an interval whose bound panicked, want none", sent)
	}

	boundReport = previous
	tr.tick(context.Background(), &set)
	sent := f.reports()
	if len(sent) != 1 {
		t.Fatalf("the trimmer sent %d reports from the sweep after the one whose bound panicked, want one", len(sent))
	}
	report := sent[0]
	if len(report.Faults) != 2 || report.FaultsOmitted != 0 ||
		!strings.HasPrefix(report.Faults[0], "a panic of an earlier interval") || !strings.HasPrefix(report.Faults[1], "the bound fell over") {
		t.Errorf("the report carries the faults %q and counts %d more, want the earlier fault and then the bound's own", report.Faults, report.FaultsOmitted)
	}
	if !slices.Equal(report.RefusedReads, []string{refused}) || report.RefusedReadsOmitted != 0 {
		t.Errorf("the report names the refused reads %q and counts %d more, want the one of the earlier sweep", report.RefusedReads, report.RefusedReadsOmitted)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the report: %v", err)
	}
}

// A report that would replace a held one, and whose merge with it panics, gives back what it
// carried (deliver, replace): report took its faults and refused /proc reads from the trimmer once
// it was bounded, and it is neither sent nor held, so the trimmer must carry them again. The held
// report stays held, and the next report carries the newer one's fault and refused read, before
// the merge's own fault, which tick recovers. The case holds report A, carries a fault and a
// refused read into report B, whose send fails too, and makes the bound panic on its second call,
// the one that merges A into B. The negative control is deliver with no give-back, as it stood at
// e6725f56: the report sent after it carried the merge's fault alone and no refused read.
func TestAReportWhoseMergeWithAHeldOnePanicsGivesBackWhatItCarried(t *testing.T) {
	const refused = "process 8: /proc/8/fd: permission denied"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	f.refuseSends(errors.New("the bound connection is gone"))
	tr := f.trimmer()
	set := *trimSettings()
	ctx := context.Background()

	tr.tick(ctx, &set) // report A, held
	if tr.held == nil {
		t.Fatalf("the trimmer holds no report after a send that failed; it wrote %q", f.linesWritten())
	}
	tr.pause = 0 // the pause after a sweep that freed nothing (tick), which would hold off B's sweep
	tr.fault("a panic before B", []byte("goroutine 1 [running]:\nan earlier stack"))
	tr.carryRefused([]string{refused})

	calls := 0
	previous := boundReport
	boundReport = func(trim *trimmer, report shimwire.LauncherVolumeReport) shimwire.LauncherVolumeReport {
		if calls++; calls == 2 {
			panic("the merge fell over")
		}
		return previous(trim, report)
	}
	t.Cleanup(func() { boundReport = previous })
	if _, ending := tr.tick(ctx, &set); ending != trimGoesOn {
		t.Fatalf("the interval whose merge panicked ended the trimmer (%v), want it passed over", ending)
	}
	if calls != 2 || f.logged("recovered a panic") != 2 {
		t.Fatalf("the bound ran %d times and the trimmer wrote %d faults, want B's bound, the merge that panicked, and that panic's fault after the earlier one: %q",
			calls, f.logged("recovered a panic"), f.linesWritten())
	}
	if tr.held == nil || len(tr.held.Faults) != 0 || len(tr.held.RefusedReads) != 0 {
		t.Fatalf("the trimmer holds %+v after the merge panicked, want report A as it was held", tr.held)
	}

	boundReport = previous
	f.refuseSends(nil)
	tr.pause = 0
	tr.tick(ctx, &set) // A goes, and then report C
	sent := f.reports()
	if len(sent) != 2 {
		t.Fatalf("the trimmer sent %d reports once the connection took them, want A and C", len(sent))
	}
	if len(sent[0].Faults) != 0 || len(sent[0].RefusedReads) != 0 {
		t.Errorf("the held report sent carries the faults %q and the refused reads %q, want A's own, none", sent[0].Faults, sent[0].RefusedReads)
	}
	report := sent[1]
	if len(report.Faults) != 2 || report.FaultsOmitted != 0 ||
		!strings.HasPrefix(report.Faults[0], "a panic before B") || !strings.HasPrefix(report.Faults[1], "the merge fell over") {
		t.Errorf("the report after the merge carries the faults %q and counts %d more, want B's fault and then the merge's own", report.Faults, report.FaultsOmitted)
	}
	if !slices.Equal(report.RefusedReads, []string{refused}) || report.RefusedReadsOmitted != 0 {
		t.Errorf("the report after the merge names the refused reads %q and counts %d more, want B's one", report.RefusedReads, report.RefusedReadsOmitted)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the report: %v", err)
	}
}

// A report whose send panics leaves what it carries for the next report: R1 has a recovered
// fault and R3 a refused /proc read reach the architect, and report took both from the trimmer
// once the bounded report existed, so a report that is then neither sent nor held must give them
// back, as one whose merge with a held report panics does (replace). The case carries one fault
// and one refused read of an earlier sweep that sent no report, makes the send of the next
// sweep's report panic with no report held, and reads the report of the sweep after it, which
// must carry the earlier fault, the send's own fault and the refused read. At c3874f11 it
// carried the send's fault alone and no refused read.
func TestAReportWhoseSendPanicsLeavesWhatItCarriedForTheNext(t *testing.T) {
	const refused = "process 9: /proc/9/fd: permission denied"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing and reports
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	set := *trimSettings()
	ctx := context.Background()
	tr.fault("a panic of an earlier interval", []byte("goroutine 1 [running]:\nan earlier stack"))
	tr.carryRefused([]string{refused})

	var sends atomic.Int32
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if sends.Add(1) == 1 {
			panic("the connection fell over sending the report")
		}
		return f.send(report)
	}
	if _, ending := tr.tick(ctx, &set); ending != trimGoesOn {
		t.Fatalf("the interval whose send panicked ended the trimmer (%v), want it passed over", ending)
	}
	if tr.held != nil || len(f.reports()) != 0 {
		t.Fatalf("the trimmer holds %v and sent %d reports after a send that panicked, want neither", tr.held, len(f.reports()))
	}

	tr.pause = 0 // the pause after a sweep that freed nothing (tick), which would hold off the next sweep
	tr.tick(ctx, &set)
	sent := f.reports()
	if len(sent) != 1 {
		t.Fatalf("the trimmer sent %d reports from the sweep after the one whose send panicked, want one", len(sent))
	}
	report := sent[0]
	if len(report.Faults)+report.FaultsOmitted != 2 || !strings.HasPrefix(report.Faults[0], "a panic of an earlier interval") {
		t.Errorf("the report carries the faults %q and counts %d more, want the earlier fault and the send's own: the report that took the earlier one was neither sent nor held", report.Faults, report.FaultsOmitted)
	}
	if !slices.Equal(report.RefusedReads, []string{refused}) || report.RefusedReadsOmitted != 0 {
		t.Errorf("the report names the refused reads %q and counts %d more, want the one of the earlier sweep", report.RefusedReads, report.RefusedReadsOmitted)
	}
}

// A sweep whose size walk panics leaves its own refused /proc reads with the trimmer for the
// next report: the comment on report says a panic in the walk leaves them there, and R3 has the
// report name any /proc read the kernel refused. The case's /proc holds a process whose fd entry
// is a file, which the reader refuses whoever it runs as, so the first sweep reports and deletes
// nothing; its walk panics at the first target it measures. The next sweep reads a /proc it is
// refused nothing in, and its report must name the first sweep's refusal. At c3874f11 report
// walked before it carried the sweep's refusals, so the next report named none.
func TestASweepWhoseWalkPanicsLeavesItsRefusedReadsForTheNextReport(t *testing.T) {
	f := newTrimFixture(t, 0)
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	refusing, pids := writeProcFixture(t, fixtureProcess{pid: 4242, fdIsFile: true})
	tr.procRoot = refusing
	set := *trimSettings()
	ctx := context.Background()

	measure := tr.measure
	var walking atomic.Bool
	walking.Store(true)
	tr.measure = func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		if walking.Load() {
			panic("the walk fell over measuring a target")
		}
		return measure(ctx, dir, name, at)
	}
	if _, ending := tr.tick(ctx, &set); ending != trimGoesOn {
		t.Fatalf("the interval whose walk panicked ended the trimmer (%v), want it passed over", ending)
	}
	if f.logged("recovered a panic") != 1 || len(f.reports()) != 0 {
		t.Fatalf("the trimmer wrote %q and sent %d reports, want the walk's panic recovered and no report", f.linesWritten(), len(f.reports()))
	}

	walking.Store(false)
	tr.procRoot = f.proc
	tr.pause = 0 // the pause after a sweep that freed nothing (tick), which would hold off the next sweep
	tr.tick(ctx, &set)
	sent := f.reports()
	if len(sent) != 1 {
		t.Fatalf("the trimmer sent %d reports from the sweep after the one whose walk panicked, want one", len(sent))
	}
	named := strings.Join(sent[0].RefusedReads, "; ")
	if !strings.Contains(named, strconv.Itoa(pids[0])) {
		t.Errorf("the report names the refused reads %q and counts %d more, want the first sweep's refusal of process %d: its walk panicked before the report carried it", sent[0].RefusedReads, sent[0].RefusedReadsOmitted, pids[0])
	}
}

// "Three in a row" keeps one row for each kind of the trimmer's work (R1, as the architect's
// comment ccc812fc on INFRA-2009 reads it, round 36's rule): a report's panic adds to the sweep
// row, which only a sweep run to its end ends, and a tick that only reads the volume, in a pause
// or below the high mark, ends the reading row alone. So a trimmer whose every report panics,
// with the ten-interval pause after each sweep, switches itself off at its third panic and tries
// its switched-off report. The case's sweeps free nothing, so each one pauses, and the send of
// each sweep's report panics, where the send of the switched-off report goes through. At c3874f11
// every interval of a pause ended the row, so each fault was the first in a row and the trimmer
// faulted after every sweep for the life of the pod, with nothing beyond its log to show for it.
func TestATrimmerWhoseEveryReportPanicsSwitchesItselfOffAcrossItsPauses(t *testing.T) {
	const fell = "the connection fell over sending the report"
	f := newTrimFixture(t, 0) // no entry: every sweep frees nothing, reports and pauses
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	tr.send = func(report shimwire.LauncherVolumeReport) error {
		if !report.SwitchedOff {
			panic(fell)
		}
		return f.send(report)
	}
	_, done := f.start(tr)

	// Written out rather than read off volumeFaultLimit, as the case of three faults with nothing
	// between them counts them (TestAPanicIsRecoveredAndThreeInARowSwitchTheTrimmerOff).
	const faultLimit = 3
	for sweep := 1; sweep < faultLimit; sweep++ {
		f.tick("the sweep ended")
		for range volumeQuietIntervals {
			f.tick("no sweep for another")
		}
		if row := fmt.Sprintf("(%d in a row of sweeps and reports)", sweep); f.logged(row) != 1 {
			t.Fatalf("after sweep %d and its pause the trimmer wrote %q, want the panic of each report so far counted, this one %s: an interval of the pause is a reading, which ends the reading row and not the sweep row",
				sweep, f.linesWritten(), row)
		}
		if f.logged("off for the life of this launcher process") != 0 {
			t.Fatalf("the trimmer switched itself off after %d panics, want it on until the %dth", sweep, faultLimit)
		}
	}
	f.tick("the sweep ended")
	awaitClosed(t, done, "the trimmer's loop to return once the third report's panic switched it off and its last report was sent")

	if off := f.logged("off for the life of this launcher process"); off != 1 {
		t.Errorf("the trimmer wrote %d lines saying it is off, want one", off)
	}
	reports := f.reports()
	if len(reports) != 1 {
		t.Fatalf("the trimmer sent %d reports, want the one last report of a trimmer that switched itself off", len(reports))
	}
	report := reports[0]
	switch {
	case !report.SwitchedOff:
		t.Error("the last report does not say the trimmer switched itself off")
	case !strings.Contains(report.StopReason, fmt.Sprintf("%d panics in a row", faultLimit)):
		t.Errorf("the last report's stop reason is %q, want the %d panics in a row that stopped it", report.StopReason, faultLimit)
	case len(report.Faults) != faultLimit || report.FaultsOmitted != 0:
		t.Errorf("the last report carries the faults %q and counts %d more, want the %d panics of the reports, each given back to the trimmer by the report whose send panicked (deliver)",
			report.Faults, report.FaultsOmitted, faultLimit)
	}
	for at, fault := range report.Faults {
		if !strings.HasPrefix(fault, fell) {
			t.Errorf("fault %d of the last report is %q, want the panic %q", at+1, fault, fell)
		}
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the last report: %v", err)
	}
}

// A panic in the interval whose settings changed interval_seconds leaves the interval clock at the
// new period: tick puts the new settings in force before it reads the volume, and the loop makes
// its clock again whenever the period changed (intervals). The case's first interval runs at
// thirty seconds; the daemon then shortens the period to ten, and the reading of the interval that
// takes the change falls over. The trimmer must ask for no clock but those two periods, stop the
// thirty-second one and take its next interval on the ten-second one. At c3874f11 tick's recovery
// cleared restart, so the loop went on ticking every thirty seconds under settings that said ten.
func TestAPanicInTheIntervalThatChangedThePeriodLeavesTheClockAtTheNewOne(t *testing.T) {
	f := newTrimFixture(t, 1)
	f.setOnRead(func(call int) {
		if call == 2 {
			panic("the volume reader fell over in the interval that changed the period")
		}
	})
	f.useVolume(steady(50))
	f.setSettings(trimSettings())
	f.start(f.trimmer())
	f.tick("nothing to sweep")

	started := settingsInterval(*trimSettings())
	shorter := *trimSettings()
	shorter.IntervalSeconds = 10
	f.setSettings(&shorter)
	offerTickOf(t, f, started, "the clock of the period it started with")
	testwait.Eventually(t, "the panic of the interval that changed the period to be recovered", func() bool {
		return f.logged("recovered a panic") == 1
	})
	if f.logged("new settings:") != 1 {
		t.Fatalf("the trimmer wrote %q, want the interval that panicked to have put the new settings in force first", f.linesWritten())
	}
	testwait.Eventually(t, "the trimmer to wait on a clock of the new period", func() bool {
		_, running := f.ticks.live(settingsInterval(shorter))
		return running
	})
	if _, running := f.ticks.live(started); running {
		t.Errorf("the trimmer still runs its clock of %s, want it stopped once the settings say %s", started, settingsInterval(shorter))
	}

	f.tick("nothing to sweep") // on the clock of the period the settings in force name (f.interval)
	var asked []time.Duration
	for at := range f.ticks.count() {
		asked = append(asked, f.ticks.periodOf(at))
	}
	if want := []time.Duration{started, settingsInterval(shorter)}; !slices.Equal(asked, want) {
		t.Errorf("the trimmer asked for clocks of %v, want %v: the one it started with, and one of the new period once the interval that changed it panicked", asked, want)
	}
}

// A panic inside the listing or inside a walk measurement closes every directory handle the work
// opened, as the handles' comments say they are closed whichever way the work ends (sweepDir,
// sweep, measureTarget): the trimmer runs in the process that is PID 1 of the roles container for
// the life of the pod, and it recovers such a panic and goes on (tick), so a handle a panic left
// open is a descriptor leaked for as long as the pod lives. Each subtest counts the descriptors of
// this process open under the volume before the interval and after its panic is recovered, with
// the collector off between the two so that a leaked handle's finaliser cannot close it out of
// sight, and reads at the panic that handles were open. At c3874f11 the listing's handles were
// closed by the sweep only once the listing had returned them, and the handle on a target's child
// only once its measurement had returned.
func TestAPanicInTheListingOrAWalkMeasurementLeavesNoHandleOpen(t *testing.T) {
	// realRoot is the volume root of f as a descriptor's link names it.
	realRoot := func(t *testing.T, f *trimFixture) string {
		t.Helper()
		root, err := filepath.EvalSymlinks(f.root)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("a panic inside the listing", func(t *testing.T) {
		f := newTrimFixture(t, 2)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		root := realRoot(t, f)
		tmp, shard := filepath.Join(root, "scratch", "tmp"), filepath.Join(root, "go", "build", "00")
		// The settings read the listing makes before each entry falls over at the first entry of
		// a Go build cache shard, once the listing holds the shard open, the cache root above it
		// and $TMPDIR's root before them.
		var atPanic []string
		tr.trimming = func() bool {
			if atPanic == nil {
				if open := descriptorsUnder(t, root); slices.Contains(open, shard) {
					atPanic = open
					panic("the settings read fell over while the listing held a shard open")
				}
			}
			return f.trimming()
		}
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		before := descriptorsUnder(t, root)
		set := *trimSettings()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval whose listing panicked ended the trimmer (%v), want it passed over", ending)
		}
		if !slices.Contains(atPanic, tmp) || f.logged("recovered a panic") != 1 {
			t.Fatalf("at the panic the listing held %q open and the trimmer wrote %q, want $TMPDIR's root %s and the shard %s open and the panic recovered", atPanic, f.linesWritten(), tmp, shard)
		}
		if left := descriptorsUnder(t, root); !slices.Equal(left, before) {
			t.Errorf("after the listing's panic was recovered the trimmer holds %q open under the volume, want %q as before the interval: the listing's handles are the sweep's to close however the listing ends", left, before)
		}
	})

	t.Run("a panic inside a walk measurement", func(t *testing.T) {
		f := newTrimFixture(t, 0) // no entry: the sweep frees nothing and reports, and its report walks
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		tr := f.trimmer()
		root := realRoot(t, f)
		scratch := filepath.Join(root, "scratch")
		// The first target is a directory under /scratch, which the walk measures first, so the
		// measurement is given a handle opened on /scratch for it alone.
		var atPanic []string
		tr.measure = func(context.Context, *dirHandle, string, time.Time) measured {
			atPanic = descriptorsUnder(t, root)
			panic("the walk fell over measuring a target")
		}
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		before := descriptorsUnder(t, root)
		set := *trimSettings()
		if _, ending := tr.tick(context.Background(), &set); ending != trimGoesOn {
			t.Fatalf("the interval whose walk panicked ended the trimmer (%v), want it passed over", ending)
		}
		if !slices.Contains(atPanic, scratch) || f.logged("recovered a panic") != 1 {
			t.Fatalf("at the panic the walk held %q open and the trimmer wrote %q, want the handle on %s the measurement was given open and the panic recovered", atPanic, f.linesWritten(), scratch)
		}
		if left := descriptorsUnder(t, root); !slices.Equal(left, before) {
			t.Errorf("after the walk's panic was recovered the trimmer holds %q open under the volume, want %q as before the interval: a measurement's handle is closed however the measurement ends", left, before)
		}
	})
}

// descriptorsUnder is every descriptor of this process open on root or on a path under it, by the
// path each names (openDescriptors), sorted.
func descriptorsUnder(t *testing.T, root string) []string {
	t.Helper()
	var under []string
	for _, target := range openDescriptors(t) {
		if target == root || strings.HasPrefix(target, root+"/") {
			under = append(under, target)
		}
	}
	slices.Sort(under)
	return under
}

// A report that replaces a held one is bounded twice — where it is made, and again with the held
// report's faults and refused reads in it — and the pod's log says once what it leaves out, with
// the counts of the report as it is held: a person counting those lines counts reports. The case
// makes two reports past the cap on sizes, A and B, whose sends both fail, so B replaces A, and
// the log must hold one line of what each leaves out. The negative control is the line written
// by the bound itself, as at 8a30be78, which wrote B's twice: three lines for two reports.
func TestAReportThatReplacesAHeldOneSaysOnceWhatItLeavesOut(t *testing.T) {
	f := newTrimFixture(t, 0)
	tr := f.trimmer()
	f.refuseSends(errors.New("the bound connection is gone"))
	over := func(reason string) shimwire.LauncherVolumeReport {
		return shimwire.LauncherVolumeReport{Trimmer: tr.id, Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst, StopReason: reason,
			Sizes: make([]shimwire.LauncherVolumeSize, shimwire.VolumeReportSizes+2)}
	}

	tr.deliver(tr.bound(over("A")))
	tr.deliver(tr.bound(over("B")))

	if tr.held == nil || tr.held.StopReason != "B" || tr.held.SizesOmitted != 2 {
		t.Fatalf("the trimmer holds %+v after two failed sends, want report B in place of A, two sizes left out", tr.held)
	}
	if said := f.logged("leaves out"); said != 2 {
		t.Errorf("the trimmer wrote %d lines of what a report leaves out, want one for A and one for B: %q", said, f.linesWritten())
	}
}

// A report that replaces a held one is bounded again once the held one's faults and refused
// reads are in it (carryHeld), so the report finally sent holds no list past its cap however
// many sends failed before it. The case makes three reports, A, B and C, each with the three
// faults and the twenty-five refused reads a report may hold, none of them alike, whose sends
// all fail, and then lets the held report through. What is sent keeps A's refused reads, the
// first, and C's faults, the newest, counts the other fifty refused reads and six faults, and is
// one the daemon takes (LauncherVolumeReport.Validate). The negative control is deliver holding
// carryHeld's report with no bound after it: the report sent named 75 refused reads and carried
// nine faults, counting none, and the daemon refused it for the refused reads past their cap.
func TestAReportSentAfterHeldOnesItReplacedIsWithinTheCaps(t *testing.T) {
	f := newTrimFixture(t, 0)
	tr := f.trimmer()
	f.refuseSends(errors.New("the bound connection is gone"))
	full := func(name string) shimwire.LauncherVolumeReport {
		report := shimwire.LauncherVolumeReport{Trimmer: tr.id, Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst, StopReason: name}
		for at := range shimwire.VolumeReportFaults {
			report.Faults = append(report.Faults, fmt.Sprintf("panic %d of %s's interval", at, name))
		}
		for at := range shimwire.VolumeReportRefusedReads {
			report.RefusedReads = append(report.RefusedReads, fmt.Sprintf("process %d of %s: /proc/%d/fd: permission denied", at, name, at))
		}
		return report
	}

	for _, name := range []string{"A", "B", "C"} {
		tr.deliver(tr.bound(full(name)))
	}
	f.refuseSends(nil)
	tr.flush()

	sent := f.reports()
	if len(sent) != 1 || sent[0].StopReason != "C" {
		t.Fatalf("the trimmer sent %d reports once the connection took them, want report C alone: %+v", len(sent), sent)
	}
	report := sent[0]
	if want := full("A").RefusedReads; !slices.Equal(report.RefusedReads, want) || report.RefusedReadsOmitted != 2*shimwire.VolumeReportRefusedReads {
		t.Errorf("the report sent names %d refused reads and counts %d more, want A's %d and the %d of B and C counted",
			len(report.RefusedReads), report.RefusedReadsOmitted, shimwire.VolumeReportRefusedReads, 2*shimwire.VolumeReportRefusedReads)
	}
	if want := full("C").Faults; !slices.Equal(report.Faults, want) || report.FaultsOmitted != 2*shimwire.VolumeReportFaults {
		t.Errorf("the report sent carries %d faults and counts %d more, want C's %d and the %d of A and B counted",
			len(report.Faults), report.FaultsOmitted, shimwire.VolumeReportFaults, 2*shimwire.VolumeReportFaults)
	}
	if err := report.Validate(); err != nil {
		t.Errorf("the daemon would refuse the report sent: %v", err)
	}
}

// plantWalkTargets puts count directories under the volume's scratch, each named with the 255
// bytes a directory component allows. Every one is a target of the size walk, which measures
// every directory two levels under the volume root, and every one is one mkdir by any role of
// the pod: /legion/scratch is writable by the role uid (measured in an issue pod of this
// deployment at 2026-11-11T08:05Z, `drwx--S--- legion legion /legion/scratch`, the role process
// being uid 1000).
func plantWalkTargets(t *testing.T, scratch string, count int) {
	t.Helper()
	for at := range count {
		name := fmt.Sprintf("%09d", at) + strings.Repeat("n", 255-9)
		if err := os.Mkdir(filepath.Join(scratch, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// What one report carries is this launcher's own bound and never a size the volume happened to
// have. A role can make thousands of directories under /legion/scratch, each a target of the
// size walk and each named with as many bytes as a directory component allows, and a report that
// carried them whole would be a line over shimwire.MaxFrameBytes: the writer refuses it, deliver
// holds it, flush tries the held one first and keeps it on failure, and every later report of
// that hour carries the same cached walk — the trimmer goes silent on exactly the pod the notice
// exists to describe. The bound says how many entries it left out rather than dropping them
// quietly.
func TestAReportOfAVolumeHoldingThousandsOfDirectoriesIsOneTheConnectionTakes(t *testing.T) {
	f := newTrimFixture(t, 1)
	const planted = 4000
	plantWalkTargets(t, filepath.Join(f.root, "scratch"), planted)
	f.useVolume(func(call int) volumeUsage {
		switch {
		case call == 5:
			return usageAt(90) // the interval between the second fault and the third, which sweeps
		case call <= 10:
			return usageAt(50) // the rest of the fault phase, below the high mark, and that sweep's last reading
		}
		return usageAt(90) // the interval that sweeps, and every reading the sweep takes
	})
	f.setOnRead(func(call int) {
		if call == 1 || call == 3 || call == 7 || call == 9 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.setSettings(trimSettings())
	tr := f.trimmer()
	// Half the targets are measured and the walk is cut at the rest, so both lists of paths the
	// report carries are as long as the volume made them.
	const walkedTargets = planted / 2
	walked := 0
	tr.measure = func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		walked++
		if walked > walkedTargets {
			return measured{cut: true}
		}
		return measureTree(ctx, dir, name, at)
	}
	f.start(tr)

	// Four faults, never three in a row: the sweep between the second and the third runs to its
	// end, which ends the row (tick), where an interval that only reads the volume below the high
	// mark neither ends it nor adds to it. That sweep deletes the one entry of each root and leaves
	// the volume under the low mark, so it sends no report and the faults stay with the trimmer;
	// the trimmer goes on, and the last sweep's report is what carries all four.
	const faults = 4
	for fault := range faults {
		f.tick("recovered a panic")
		f.tick("nothing to sweep")
		if fault == 1 {
			f.tick("the sweep ended")
		}
	}
	f.tick("the sweep ended")
	testwait.Eventually(t, "the report of the sweep that followed the faults", func() bool { return len(f.reports()) == 1 })
	report := f.reports()[0]

	var line bytes.Buffer
	if err := shimwire.NewWriter(&line).WriteFrame(report); err != nil {
		t.Fatalf("the connection refuses the report of a volume holding %d directories: %v", planted, err)
	}
	if line.Len() >= shimwire.MaxFrameBytes {
		t.Fatalf("the report encodes to %d bytes, and one line may carry %d: a report this long is one no connection takes",
			line.Len(), shimwire.MaxFrameBytes)
	}

	switch {
	case len(report.Sizes) != shimwire.VolumeReportSizes:
		t.Errorf("the report carries %d sizes, want the %d this launcher bounds it to", len(report.Sizes), shimwire.VolumeReportSizes)
	case len(report.Sizes)+report.SizesOmitted != walkedTargets:
		t.Errorf("the report carries %d sizes and says %d were left out, want the %d the walk measured",
			len(report.Sizes), report.SizesOmitted, walkedTargets)
	case len(report.SizesMissing) != shimwire.VolumeReportSizesMissing:
		t.Errorf("the report names %d unmeasured directories, want the %d this launcher bounds it to", len(report.SizesMissing), shimwire.VolumeReportSizesMissing)
	case len(report.SizesMissing)+report.SizesMissingOmitted < planted-walkedTargets:
		t.Errorf("the report names %d unmeasured directories and says %d were left out, want at least the %d the walk never reached",
			len(report.SizesMissing), report.SizesMissingOmitted, planted-walkedTargets)
	case len(report.Faults) != shimwire.VolumeReportFaults:
		t.Errorf("the report carries %d faults, want the newest %d", len(report.Faults), shimwire.VolumeReportFaults)
	case len(report.Faults)+report.FaultsOmitted != faults:
		t.Errorf("the report carries %d faults and says %d were left out, want the %d the trimmer recovered",
			len(report.Faults), report.FaultsOmitted, faults)
	}

	cut := 0
	for _, path := range append(pathsOf(report.Sizes), missingPaths(report.SizesMissing)...) {
		if runes := utf8.RuneCountInString(path); runes > volumeReportPathRunes {
			t.Errorf("the report carries a path of %d runes, want none over the %d bound", runes, volumeReportPathRunes)
		}
		if strings.HasSuffix(path, "…") {
			cut++
		}
	}
	if cut == 0 {
		t.Error("no path of the report was cut, although every planted directory is named with 255 bytes: the case proves nothing")
	}
	for _, fault := range report.Faults {
		if runes := utf8.RuneCountInString(fault); runes > volumeReportFaultRunes {
			t.Errorf("the report carries a fault of %d runes, want none over the %d bound", runes, volumeReportFaultRunes)
		}
	}
	if said := f.logged("leaves out"); said != 1 {
		t.Errorf("the trimmer wrote %d lines saying what the report leaves out, want one: a person reading the pod's log must know the report is not the whole truth; it wrote %q",
			said, f.linesWritten())
	}

	t.Run("a /proc the kernel refused for hundreds of processes", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		const refusedProcesses = 200
		for at := range refusedProcesses {
			placeRefusedProcess(t, f.proc, 1000+at, filepath.Join(f.root, "scratch", "tmp"))
		}
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		f.start(f.trimmer())

		f.tick("the sweep ended")
		testwait.Eventually(t, "the report of the sweep that deleted nothing", func() bool { return len(f.reports()) == 1 })
		report := f.reports()[0]
		switch {
		case len(report.RefusedReads) != shimwire.VolumeReportRefusedReads:
			t.Errorf("the report names %d refused reads, want the %d this launcher bounds it to", len(report.RefusedReads), shimwire.VolumeReportRefusedReads)
		case len(report.RefusedReads)+report.RefusedReadsOmitted != refusedProcesses:
			t.Errorf("the report names %d refused reads and says %d were left out, want the %d processes the sweep met",
				len(report.RefusedReads), report.RefusedReadsOmitted, refusedProcesses)
		case utf8.RuneCountInString(report.StopReason) > volumeReportStopReasonRunes:
			t.Errorf("the report's stop reason is %d runes, want none over the %d bound: it carries the joined text of every refused read",
				utf8.RuneCountInString(report.StopReason), volumeReportStopReasonRunes)
		}
		var line bytes.Buffer
		if err := shimwire.NewWriter(&line).WriteFrame(report); err != nil {
			t.Fatalf("the connection refuses the report of a pod whose /proc was refused %d times: %v", refusedProcesses, err)
		}
	})

	// The two strings this package mints rather than reads off the pod are cut as well. Neither
	// is long in a report this launcher made — randomID writes 32 hex characters, and what
	// raised the episode is one of shimwire's three words — and the daemon renders both into
	// the sentence the architect's session reads, and the frame's own rule checks neither one's
	// length (LauncherVolumeReport.Validate), so where this launcher sent the report their part
	// of the sentence's size rests on this bound.
	t.Run("the identifier and the raised-by this package mints", func(t *testing.T) {
		f := newTrimFixture(t, 1)
		long := strings.Repeat("l", volumeReportTrimmerRunes*3)
		report := f.trimmer().bound(shimwire.LauncherVolumeReport{Trimmer: long, Episode: 1, RaisedBy: long})
		for what, got := range map[string]string{"trimmer": report.Trimmer, "raisedBy": report.RaisedBy} {
			if runes := utf8.RuneCountInString(got); runes != volumeReportTrimmerRunes {
				t.Errorf("the report's %s is %d runes, want the %d this launcher bounds it to", what, runes, volumeReportTrimmerRunes)
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("the report's %s was cut without saying so: %q", what, got)
			}
		}
		// The control: what this launcher really mints passes through whole.
		whole := f.trimmer()
		kept := whole.bound(shimwire.LauncherVolumeReport{Trimmer: whole.id, Episode: 1, RaisedBy: shimwire.VolumeEpisodeRecovery})
		if kept.Trimmer != whole.id || kept.RaisedBy != shimwire.VolumeEpisodeRecovery {
			t.Errorf("the bound cut a report this launcher made: trimmer %q and raised by %q, want %q and %q",
				kept.Trimmer, kept.RaisedBy, whole.id, shimwire.VolumeEpisodeRecovery)
		}
	})
}

// Every string a report carries is kept whole at its bound in runes and cut at one rune more, to
// the bound with an ellipsis in place of its last rune (cutRunes, bound). The other cases cut
// strings at three times their bound or more and read only that the result is within it
// (TestAReportOfAVolumeHoldingThousandsOfDirectoriesIsOneTheConnectionTakes,
// TestDirectoriesReadInPartAreBoundedInTheReportAndTheLog), with which a cut at the bound itself
// or a rune short of it is as green; only the identifier and the raised-by are held to their
// length. Each string is of a rune two bytes long, so a cut counted in bytes is no cut in runes.
// The bounds are written out rather than read off the constants, as the other bounds of this
// package are, since a case staged from the constant it tests moves with it.
func TestEveryStringOfAReportIsKeptWholeAtItsBoundAndCutAtOneRuneMore(t *testing.T) {
	const (
		pathRunes       = 200
		stopReasonRunes = 400
		faultRunes      = 2000
		trimmerRunes    = 100
	)
	tr := newTrimFixture(t, 0).trimmer()
	for _, past := range []int{0, 1} {
		of := func(bound int) string { return strings.Repeat("é", bound+past) }
		report := tr.bound(shimwire.LauncherVolumeReport{
			Trimmer: of(trimmerRunes), Episode: 1, RaisedBy: of(trimmerRunes),
			StopReason:   of(stopReasonRunes),
			Sizes:        []shimwire.LauncherVolumeSize{{Path: of(pathRunes), Refused: of(pathRunes)}},
			SizesMissing: []shimwire.LauncherVolumeMissing{{Path: of(pathRunes), Reason: of(pathRunes)}},
			RefusedReads: []string{of(pathRunes)},
			Faults:       []string{of(faultRunes)},
		})
		for _, field := range []struct {
			name  string
			got   string
			bound int
		}{
			{"trimmer", report.Trimmer, trimmerRunes},
			{"raised-by", report.RaisedBy, trimmerRunes},
			{"stop reason", report.StopReason, stopReasonRunes},
			{"size's path", report.Sizes[0].Path, pathRunes},
			{"size's refused read", report.Sizes[0].Refused, pathRunes},
			{"unmeasured path", report.SizesMissing[0].Path, pathRunes},
			{"unmeasured path's reason", report.SizesMissing[0].Reason, pathRunes},
			{"refused /proc read", report.RefusedReads[0], pathRunes},
			{"fault", report.Faults[0], faultRunes},
		} {
			want := of(field.bound)
			if past > 0 {
				want = strings.Repeat("é", field.bound-1) + "…"
			}
			if field.got != want {
				t.Errorf("the report's %s of %d runes is carried as %d runes ending %q, want %d runes ending %q: whole at its bound of %d, and cut to it with an ellipsis one rune past",
					field.name, field.bound+past, utf8.RuneCountInString(field.got), field.got[max(0, len(field.got)-8):], utf8.RuneCountInString(want), want[len(want)-8:], field.bound)
			}
		}
	}
}

// pathsOf is the path of each size a report carries.
func pathsOf(sizes []shimwire.LauncherVolumeSize) []string {
	paths := make([]string, len(sizes))
	for at, size := range sizes {
		paths[at] = size.Path
	}
	return paths
}

// missingPaths is the path of each directory a walk or a report names as unmeasured.
func missingPaths(missing []shimwire.LauncherVolumeMissing) []string {
	paths := make([]string, len(missing))
	for at, one := range missing {
		paths[at] = one.Path
	}
	return paths
}
