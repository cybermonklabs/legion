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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	f := &trimFixture{
		t:     t,
		root:  root,
		self:  os.Getpid(),
		roots: rebasedRoots(root),
		clock: newFakeClock(),
		ticks: &fakeTicks{},
		reading: func(int) volumeUsage {
			return volumeUsage{UsedBytes: percentBytes(50), AvailableBytes: fixtureVolumeBytes - percentBytes(50)}
		},
	}
	f.proc = writeProcFixture(t)
	f.buildProtected()
	f.buildRoots(perRoot)
	return f
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
// it reads the same field through the same hook as settings, so a case that stops the trimmer
// mid-sweep stops it here as the pod's own manager does (manager.volumeTrimming).
func (f *trimFixture) trimming() bool {
	return f.settings() != nil
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
func (f *trimFixture) trimmer() *trimmer {
	tr := newTrimmer(f.settings, f.trimming, f.send, f.log)
	tr.volumeRoot, tr.procRoot, tr.self, tr.roots = f.root, f.proc, f.self, f.roots
	tr.now, tr.ticks = f.clock.now, f.ticks.make
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
// panics on a non-positive period, which would take six live roles down with it.
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
		{HighPercent: 2, LowPercent: 1, MinAgeMinutes: 1, IntervalSeconds: 3600},
	} {
		if refusals := refusedTrimSettings(edge); len(refusals) > 0 {
			t.Errorf("the settings at the edge of the ranges %+v were refused: %v", edge, refusals)
		}
	}
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

	// Three readings in a row showing no decrease, each a hundred deletions apart.
	if want := volumeSweepStallReadings * volumeSweepReadEvery; out.deletions != want {
		t.Errorf("the sweep deleted %d entries, want %d: %d readings in a row with no decrease, a hundred deletions apart (%s)",
			out.deletions, want, volumeSweepStallReadings, out.reason)
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
				f.proc = writeProcFixture(t, process)
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
// running, and the six entries it could appear through are driven one at a time above.
func placeProcess(t *testing.T, proc string, pid int, cwd string) {
	t.Helper()
	dir := filepath.Join(proc, strconv.Itoa(pid))
	mkdirFixture(t, dir)
	if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}
}

// placeRefusedProcess puts in a process whose /proc the kernel refuses, staged as a regular file
// where the fd directory belongs, which refuses every reader including root (writeProcFixture's
// fdIsFile).
func placeRefusedProcess(t *testing.T, proc string, pid int, cwd string) {
	t.Helper()
	placeProcess(t, proc, pid, cwd)
	if err := os.WriteFile(filepath.Join(proc, strconv.Itoa(pid), "fd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A process that starts using an entry after the sweep has begun still protects it: the
// used-path set is read again at least once a second while the sweep runs, which the case
// drives on the injected clock. Its control is the same tree swept with no process appearing,
// where that entry is deleted.
func TestAProcessThatStartsAfterTheSweepBeganStillProtectsItsEntry(t *testing.T) {
	sweep := func(t *testing.T, appears bool) (survived bool, deletions int, entries int) {
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
			// A second of the trimmer's own clock, which is what makes it read /proc again.
			clock.advance(volumeUsedPathsMaxAge + time.Millisecond)
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

	survived, deletions, entries := sweep(t, true)
	if !survived {
		t.Error("the sweep deleted the entry a process took hold of after the sweep began, want the used-path set read again at least once a second")
	}
	if deletions != entries-1 {
		t.Errorf("the sweep deleted %d entries, want the %d that nothing holds", deletions, entries-1)
	}
	// The control: with no process appearing, that same entry goes.
	t.Run("the control, with no process appearing", func(t *testing.T) {
		survived, deletions, entries := sweep(t, false)
		if survived {
			t.Error("the entry survived a sweep with no process holding it, so the case above shows nothing")
		}
		if deletions != entries {
			t.Errorf("the control sweep deleted %d of %d entries, want every one", deletions, entries)
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

// A /proc read the kernel refused on the first read of a sweep deletes nothing; one refused
// later ends the sweep at the next entry boundary and keeps what it deleted. Either names the
// process and is carried into the report, because a refused process is one whose open files are
// unknown and the operator is the one who can go and look.
func TestARefusedProcReadStopsTheSweepAndNamesTheProcess(t *testing.T) {
	t.Run("on the first read of the sweep", func(t *testing.T) {
		f := newTrimFixture(t, 3)
		placeRefusedProcess(t, f.proc, 5101, filepath.Join(f.root, "scratch"))
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

		if out.deletions != 0 || len(f.gone()) != 0 {
			t.Errorf("the sweep deleted %d entries after a refused first read of /proc, want none", out.deletions)
		}
		if len(out.refused) != 1 || !strings.Contains(out.refused[0], "5101") {
			t.Errorf("the sweep reported the refusals %v, want exactly the process 5101 this case made unreadable", out.refused)
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
		f.clock.setHook(func(clock *fakeClock) {
			if placed || f.exists(first.path) {
				return
			}
			placed = true
			placeRefusedProcess(t, f.proc, 5202, filepath.Join(f.root, "scratch"))
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
		if len(out.refused) != 1 || !strings.Contains(out.refused[0], "5202") {
			t.Errorf("the sweep reported the refusals %v, want the process 5202 named", out.refused)
		}
		if !strings.Contains(out.reason, "refused") {
			t.Errorf("the sweep stopped because %q, want the refusal named", out.reason)
		}
	})
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
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(root, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	return root
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
func TestTheSweepsBudgetRunsFromItsStartWithTheListingIncluded(t *testing.T) {
	f := newTrimFixture(t, 20)
	// A minute of the trimmer's clock per reading of it, which the listing asks at every entry.
	f.clock.step = time.Minute
	f.useVolume(f.modelled(90))
	f.setSettings(trimSettings())

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
// next entry boundary once deleting has begun: what was deleted stands, nothing is left half
// removed, and the container's stop lets run return at once rather than after the budget.
func TestTheThreeEndingsTakeEffectAtTheNextEntryBoundary(t *testing.T) {
	const before = 3 // deletions before the ending arrives

	t.Run("the budget", func(t *testing.T) {
		f := newTrimFixture(t, 5)
		f.useVolume(steady(90))
		f.setSettings(trimSettings())
		spent := false
		f.clock.setHook(func(clock *fakeClock) {
			if spent || len(f.gone()) < before {
				return
			}
			spent = true
			clock.advance(volumeSweepBudget + time.Minute)
		})

		tr := f.trimmer()
		out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

		if !spent {
			t.Fatal("the case never reached the moment it stages")
		}
		if out.deletions != before {
			t.Errorf("the sweep deleted %d entries, want the %d it had made when the budget ran out", out.deletions, before)
		}
		if !strings.Contains(out.reason, "budget") {
			t.Errorf("the sweep stopped because %q, want the budget named", out.reason)
		}
		f.assertNothingHalfRemoved()
	})

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
		// One more entry than the stop was staged after: the boundary that reads the clock has
		// already asked the context, so the entry it was on is removed whole and the stop is
		// taken at the boundary after it, which is the rule — an ending is never taken in the
		// middle of a removal.
		if gone := len(f.gone()); gone != before+1 {
			t.Errorf("%d entries are gone, want the %d deletions that stood when the stop was taken at the next boundary", gone, before+1)
		}
		if gone := len(f.gone()); gone >= len(f.entries) {
			t.Errorf("every one of the %d entries is gone: the sweep ran to its end rather than stopping", len(f.entries))
		}
		f.assertNothingHalfRemoved()
	})
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

// The worst case of a volume the trimmer cannot bring under the high mark is two sweeps and
// then one every eleven intervals: the first sweep frees what it can, the second finds nothing
// eligible and so frees under a point, and that engages the ten-interval pause.
func TestTheWorstCaseIsTwoSweepsAndThenOneEveryEleventhInterval(t *testing.T) {
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
// one last report saying so. The recovery is a departure from this daemon's practice, which has
// none in any production path, because this goroutine runs in the process that is PID 1 of the
// roles container: its crash costs six live roles their round over a cache sweep.
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
	for at := range volumeFaultLimit {
		f.tick("recovered a panic")
		if faults := f.logged("recovered a panic"); faults != at+1 {
			t.Fatalf("the trimmer logged %d recovered panics after %d, want one for each", faults, at+1)
		}
	}
	if f.logged("off for the life of this launcher process") != 1 {
		t.Errorf("the trimmer did not log that it is off for good; it wrote %q", f.linesWritten())
	}
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
	case len(report.Faults) != volumeFaultLimit:
		t.Errorf("the last report carries %d faults, want the %d in a row that stopped it", len(report.Faults), volumeFaultLimit)
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

// The episode number follows the volume across the marks: the first reading at the high mark
// opens episode 1, a reading below the low mark after that opens the next, and a reading five
// points above the highest reading a reported sweep of this episode started from opens another.
// A reading between the marks raises nothing, and a volume oscillating between them raises
// nothing, which is what keeps the daemon's three-notice bound from being spent on a trimmer
// doing its job.
//
// The anchor is the pre-sweep reading and never the figure a report carried, and the ninth
// reading of the script is the control: at 94 it is more than five points above the 88.5 the
// last report carried and less than five above the 90 that sweep started from, so a trimmer
// anchored on the report would raise the episode there and this case would fail.
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
		90, 88, // the first sweep: episode 1, first
		88, 86.5, // a second sweep of the same episode
		75,       // between the marks: nothing
		69,       // below the low mark after the high one: episode 2, recovery
		90, 88.5, // a sweep of episode 2, which anchors it at 90
		94, 92.5, // the control: five points above the report's 88.5, under five above the 90
		99, 97, // five points above the anchor: episode 3, rise
		86, 84.5, // back to the high mark from a post-sweep low point: nothing
		84, // between the marks again: nothing
	))
	f.setSettings(trimSettings())
	tr := f.trimmer()
	f.start(tr)

	for at, ends := range []string{
		"the sweep ended", "the sweep ended", "nothing to sweep", "nothing to sweep",
		"the sweep ended", "the sweep ended", "the sweep ended", "the sweep ended", "nothing to sweep",
	} {
		f.tick(ends)
		if t.Failed() {
			t.Fatalf("the case stopped at interval %d", at+1)
		}
	}

	want := []struct {
		episode  int
		raisedBy string
	}{
		{1, shimwire.VolumeEpisodeFirst},
		{1, shimwire.VolumeEpisodeFirst},
		{2, shimwire.VolumeEpisodeRecovery},
		{2, shimwire.VolumeEpisodeRecovery},
		{3, shimwire.VolumeEpisodeRise},
		{3, shimwire.VolumeEpisodeRise},
	}
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

// A report the connection refuses is held and sent on the next attempt, as the launcher's
// bundleRestore holds its own confirmation: a report the daemon never got is a volume nobody
// was told about. Settings that stop the trimmer discard it, because the pod is being told to
// run no trimmer at all.
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
		if call <= 8 {
			return usageAt(50) // the eight interval readings of the fault phase, below the high mark
		}
		return usageAt(90) // the interval that sweeps, and every reading the sweep takes
	})
	f.setOnRead(func(call int) {
		if call <= 7 && call%2 == 1 {
			panic(fmt.Sprintf("the volume reader fell over on call %d", call))
		}
	})
	f.setSettings(trimSettings())
	tr := f.trimmer()
	// Half the targets are measured and the walk is cut at the rest, so both lists of paths the
	// report carries are as long as the volume made them.
	const walkedTargets = planted / 2
	walked := 0
	tr.measure = func(ctx context.Context, path string, at time.Time) measured {
		walked++
		if walked > walkedTargets {
			return measured{size: shimwire.LauncherVolumeSize{Path: path}, cut: true}
		}
		return measureTree(ctx, path, at)
	}
	f.start(tr)

	// Four faults, never three in a row: a clean interval between them resets the count, so the
	// trimmer goes on and the sweep's report is what carries them.
	const faults = 4
	for range faults {
		f.tick("recovered a panic")
		f.tick("nothing to sweep")
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
	case len(report.Sizes) != volumeReportSizes:
		t.Errorf("the report carries %d measured directories, want the %d this launcher bounds it to", len(report.Sizes), volumeReportSizes)
	case len(report.Sizes)+report.SizesOmitted != walkedTargets:
		t.Errorf("the report carries %d measured directories and says %d were left out, want the %d the walk measured",
			len(report.Sizes), report.SizesOmitted, walkedTargets)
	case len(report.SizesMissing) != volumeReportSizesMissing:
		t.Errorf("the report names %d unmeasured directories, want the %d this launcher bounds it to", len(report.SizesMissing), volumeReportSizesMissing)
	case len(report.SizesMissing)+report.SizesMissingOmitted < planted-walkedTargets:
		t.Errorf("the report names %d unmeasured directories and says %d were left out, want at least the %d the walk never reached",
			len(report.SizesMissing), report.SizesMissingOmitted, planted-walkedTargets)
	case len(report.Faults) != volumeReportFaults:
		t.Errorf("the report carries %d faults, want the newest %d", len(report.Faults), volumeReportFaults)
	case len(report.Faults)+report.FaultsOmitted != faults:
		t.Errorf("the report carries %d faults and says %d were left out, want the %d the trimmer recovered",
			len(report.Faults), report.FaultsOmitted, faults)
	}

	cut := 0
	for _, path := range append(pathsOf(report.Sizes), report.SizesMissing...) {
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
		case len(report.RefusedReads) != volumeReportRefusedReads:
			t.Errorf("the report names %d refused reads, want the %d this launcher bounds it to", len(report.RefusedReads), volumeReportRefusedReads)
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
}

// pathsOf is the path of each size a report carries.
func pathsOf(sizes []shimwire.LauncherVolumeSize) []string {
	paths := make([]string, len(sizes))
	for at, size := range sizes {
		paths[at] = size.Path
	}
	return paths
}
