// Package memoryguard ends a process it may end when a container nears its memory limit, the one
// that grew the most or, where none grew, the largest, so that the kernel does not end the whole
// sandbox instead (INFRA-3557, by the rules of INFRA-3531). Under gVisor the host kernel sees one
// sandbox and not its processes: a container that passes its limit loses every process at once,
// which in an issue pod is every role's turn.
//
// The guard reads the pod's anonymous memory, AnonPages of /proc/meminfo, and the container's
// limit, the cgroup's memory.limit_in_bytes, once each interval. The cgroup's counter,
// memory.usage_in_bytes, is not the reading: it counts the pages of mapped files, so a database's
// map raises it past any mark while AnonPages stays low, and one read of it costs about 18 ms
// under gVisor where a read of meminfo costs a fraction of one. The counter is read every few
// seconds for the hourly line, once the guard has decided on the reading (tick), and at each kill,
// and never at each interval.
//
// At or above the act share of the limit (the mark), the guard acts on the processes it may end,
// by the sizes it reads of those alone, where their sizes sum to at least the floor share of the
// limit: it sends SIGKILL through a pidfd to the first of them in its order of ends, then takes
// a reading at each interval. The order (rank, INFRA-3531 R1 of spec version 26) is the most grown
// first: a process's growth is its size at this read less its size at its oldest read within the
// last growthWindow (300 ms), and growth under growthNoise (1 MiB) is none. Of those that did not
// grow over the window, the most grown since the previous kill of the rise follow, by growth since
// that kill as grownSinceKill measures it, also from growthNoise; before a rise's first kill no
// process has any. Those that grew by neither follow, largest first, so where none grew the largest
// ends first. The middle tier is there because a throttled pod reads 170 to over 300 ms apart, so
// the window can hold one read of each process: in the latepool and sharedpool probes under gVisor
// (INFRA-3531 T17) the guard went on after a kill by ending the largest, a sleeping fork, while
// the growers lived. Under gVisor each forked process reports the pages it shares with its parent
// as its own, and no smaps field tells them apart, so the largest can be a sleeping fork whose end
// frees nothing while another process fills the pod; for the same reason a process with no earlier
// read in the window, a fresh fork among them, counts as not grown.
//
// So that it knows what grew when the reading reaches the mark, an armed guard reads those sizes
// at each interval already in the band, the bandPercent (10%) of the limit below the mark, keeps
// each process's reads by its pid and start time for the window (keepSizes), and holds the
// processes each band reading's assessment found, at the sizes it read, for the next reading
// (held). At the first reading at or above the mark it decides on what the band holds, and sends
// the kill before it scans the process table, reads any descriptor or the counter, or starts the
// hour's scan for the largest process (tick): it orders the held processes at that reading's own
// time, each by its newest held read less its oldest held read within the window of that time, so
// a step older than the window is no growth there; it checks the held sizes against the floor; it
// opens the first by its pidfd, checks its start time and its exit, and kills it; and only then
// does it assess that reading, whose sizes are the sizes as of the kill (endHeld). Where one of the
// held processes grew, it reads no smaps before the kill, and the kill's line and report carry the
// held size and that reading. Where none of them grew by the held reads, it reads the smaps of the
// held processes alone again before the kill, and orders them by those reads (rereadHeld): the
// step that takes the reading to the mark comes between the band's last reading and this one, and
// no held read shows it. Under gVisor the `shared` probe at 4 GiB, one grower stepping 64 MiB about
// every 250 ms beside three sleeping forks of a 1.5 GiB block, lost a sleeper first to a guard that
// decided on the held reads alone, since the grower's step before the crossing one was older than
// the window at the mark (INFRA-3531 T17). So the first kill comes within one interval of the
// reading reaching the mark, also where the pod's CPU is throttled (INFRA-3531 R1 of spec version
// 25, T16): under gVisor, with six growers faulting their pages in, the host throttles the whole pod
// by its CFS quota, so a band reading that costs about 2 ms of work took 80 to 270 ms of wall time,
// and a read of a growing process's /proc files at times waits on its memory locks. This issue's
// tester measured 194 ms from the reading at the mark to the first kill, where R1 asks for one
// interval (100 ms), with a mark reading that read every size again; deciding on the band's reads
// brought 9 of 9 runs in 19 builds Jobs to 1.3 to 71.8 ms. The guard assesses the reading at the
// mark and acts on it as on any later reading where nothing is held (the reading crossed the whole
// band within one interval, or the reading before was not an armed guard's in the band), where the
// held first has exited or its pid names another process (its start time changed), or where the
// held sizes, or the sizes read again, are under the floor or name no process: it never gives a
// rise up on held sizes alone. Below the band, while it is off and once it has given a rise up, it
// reads no size and keeps none: a pod that sits below its band pays for nothing but its readings.
//
// After a kill the guard reads the sizes at each interval too, and waits only while the killed
// process has not exited (its Handle's Exited), and no longer than the wait (INFRA-3531 R1 of spec
// version 25). The reading has fallen when it is below the reading at the kill by any amount, or
// below the mark (fell). The killed process the guard never ends or reports again: while it has
// not exited it is left out of what the guard may end, of their sum against the floor and of the
// growth since the kill (leaveOut). Once the process has exited or the wait has passed, the guard
// decides at each reading. Fallen and still at or above the mark, it ends the next in the same
// order, so a pool of forked workers (make -j, a Lean build), whose shared pages no one worker's
// kill frees, is ended one worker at a time. Not fallen, it ends the next in the order at once, at
// that reading, where a process it may end has grown since the previous kill of the rise and those
// processes hold the floor between them. Growth since the kill decides this, not the window that
// orders the processes: a process's size as of the kill is its size in the assessment the kill was
// made on, or its first read after the kill for a process first read then, and it has grown once
// its size is that plus growthNoise or more (atKill, grownSinceKill). So a process that grows in
// steps wider than the window still counts, and six growers that together outrun what one kill
// frees, which this issue's tester measured ending a 4 GiB pod while the guard waited the whole
// wait, are ended one at each reading. Where neither holds, the guard ends nothing and decides
// again at the next reading, until the wait has passed; then it gives the rise up with reason not
// fallen, so a rise that is a protected process's own, Oh My Pi's, in which nothing it may end
// grows, costs at most one kill. A rise given up, for that reason or because no process may be
// ended, has the guard log the sizes it saw and end nothing more for that rise, until the reading
// has gone below the mark; a stop that repeats is logged at most once a minute (stopRise).
//
// A process's size, by which the guard measures its growth and ranks it, is the sum of the
// `Anonymous:` lines of its smaps, the `Rss:` of its shared mappings of memory-backed files (a
// shared anonymous map, a memfd, POSIX or System V shared memory), and, for each memfd it holds
// open by a file descriptor and has not mapped, that memfd's size or the pod's Shmem reading,
// whichever is less, since its end frees that memfd's pages too and Shmem bounds them. A memfd
// that a process the guard read maps counts through that mapping's `Rss` alone, and one that
// several of them hold by a descriptor counts once, for the largest of them (smapsSize,
// heldMemfdBytes, addHeldMemfds).
// The scan leaves out a zombie and a dead process, which hold no memory, and reads nothing of
// either past its stat and status: under gVisor a zombie's exe link answers EACCES, which would
// otherwise count it unread. A process in either state whose status counts more than one thread
// is one whose main thread alone has exited, alive with all its memory, and is read as live. A
// process whose exe, ns/pid or smaps fails to read after its stat was read is left out the same
// way when its stat, read again, shows it gone or exited, since it may have exited in between.
//
// What it may end is its caller's to say (Classifier): the launcher protects Legion's own
// processes and the agents', in an issue pod's container and, as process 1 of the controller's
// container, in the controller's pod (INFRA-3531), whose every process it reports as the
// controller's and whose guard it starts at ControllerDefaults. The package protects only its own
// process.
package memoryguard

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Settings are what a guard runs by: the act share and the floor share of the limit, in percent,
// the longest wait after a kill for the reading to fall, and the time between two readings.
type Settings struct {
	ActPercent   int
	FloorPercent int
	Wait         time.Duration
	Interval     time.Duration
}

// Defaults are the settings an issue pod's guard runs by before anyone has said otherwise, the
// shimwire defaults, which the daemon's configuration defaults for issue pods are too: one change
// moves both.
func Defaults() Settings {
	return SettingsOf(shimwire.LauncherMemoryGuard{
		ActPercent:           shimwire.MemoryGuardActDefault,
		FloorPercent:         shimwire.MemoryGuardFloorDefault,
		WaitSeconds:          shimwire.MemoryGuardWaitDefault,
		IntervalMilliseconds: shimwire.MemoryGuardIntervalDefault,
	})
}

// ControllerDefaults are the settings the controller pod's guard runs by before anyone has said
// otherwise (INFRA-3531, R4): Defaults with the controller pod's own wait after a kill,
// shimwire.MemoryGuardControllerWaitDefault, which the daemon's configuration default for
// `controller_pod` is too.
func ControllerDefaults() Settings {
	s := Defaults()
	s.Wait = shimwire.MemoryGuardControllerWaitDefault * time.Second
	return s
}

// SettingsOf is the settings an acknowledgement's member carries. It does not check the ranges
// (shimwire.LauncherMemoryGuard.Validate does, and Apply calls it).
func SettingsOf(w shimwire.LauncherMemoryGuard) Settings {
	return Settings{
		ActPercent:   w.ActPercent,
		FloorPercent: w.FloorPercent,
		Wait:         time.Duration(w.WaitSeconds) * time.Second,
		Interval:     time.Duration(w.IntervalMilliseconds) * time.Millisecond,
	}
}

// Class is what a classifier says of one process: the role whose process it is, "" outside
// every role (a line names it `none`), and whether the guard must spare it.
type Class struct {
	Role      string
	Protected bool
}

// Classifier says, for each process of a scan, whose it is and whether the guard may end it. It
// is called once a scan, from the guard's loop when it acts and from its hourly scan, which may
// run at the same time. A pid absent from the map is a process the guard may end, of no role.
type Classifier func(t *Table) map[int]Class

// Kill is one process the guard ended: the report its role is sent, which names the process by
// processName and carries none of its arguments, and Role, the role whose process it was, ""
// outside every role.
type Kill struct {
	shimwire.LauncherMemoryKill
	Role string
}

// Clock is the time a guard reads and waits on. After is called by the guard's loop alone, one
// wait at a time, so a clock may reuse one timer for every wait.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Signaler is how a guard ends a process: Open holds the process by its pid, so that the signal
// the handle sends reaches that process or none, even when the pid is taken by another process
// after the scan read it.
type Signaler interface {
	// Open holds pid. An error that errors.Is reads as errors.ErrUnsupported is a system with
	// no way to hold a process, on which the guard turns off.
	Open(pid int) (Handle, error)
}

// Handle is one process a Signaler holds.
type Handle interface {
	// Kill sends the process SIGKILL.
	Kill() error
	// Exited answers, without waiting, whether the process has exited.
	Exited() bool
	Close() error
}

// Config is what a guard reads, whom it asks, and where it reports.
type Config struct {
	// Proc is the root of the processes the guard reads, "/proc" when empty.
	Proc string
	// Cgroup is the container's memory cgroup directory, "/sys/fs/cgroup/memory" when empty.
	Cgroup string
	// Classify says which processes are protected and whose they are; nil protects nothing but
	// the guard's own process.
	Classify Classifier
	// Sink takes each kill, on the guard's loop, so it must not block.
	Sink func(Kill)
	// Logf writes one line of the guard's; nil discards them.
	Logf func(format string, args ...any)
	// Clock is the time the guard reads and waits on; nil is the wall clock.
	Clock Clock
	// Signal ends a process; nil is the pidfd kill, which only a Linux build has.
	Signal Signaler
	// Start is what the guard runs by until Apply gives it other settings; the zero Settings is
	// Defaults, an issue pod's. The controller pod's launcher starts its guard at
	// ControllerDefaults.
	Start Settings
	// Observe, when set, is called with each reading the guard takes, on its loop and before it
	// acts on the reading: the time its Clock read as it took the reading, AnonPages and the
	// limit, in bytes. It is a measurement seam for the live tests, which time the guard's
	// decisions and the gaps between its readings by the guard's own readings; production leaves
	// it nil. It must not block, since the guard's next reading waits for it.
	Observe func(now time.Time, reading, limit uint64)
}

// phase is where a guard is in a rise.
type phase int

const (
	// armed acts on the next reading at or above the mark, and reads the sizes of what it may end
	// at each reading in the band below the mark, which it holds for the next reading (held).
	armed phase = iota
	// waiting has sent a kill and reads, and reads those sizes, while the killed process has not
	// exited and the wait has not passed, and from then on decides at each reading whether to end
	// the next (watch).
	waiting
	// stopped has given up on this rise, reads no size and acts on nothing until the reading is
	// below the mark.
	stopped
)

// afterKill is the kill a waiting guard watches: the process held, whose exit ends the wait, the
// same process by its pid and start time (killed), which the guard never ends again while it has
// not exited (leaveOut), the reading at the kill, which the reading must fall below (fell), and
// when the wait passes at the latest.
type afterKill struct {
	handle   Handle
	killed   procKey
	reading  uint64
	deadline time.Time
	exited   bool
	fallen   bool
}

// The three numbers of the order of ends (rank, INFRA-3531 R1 of spec version 22). An armed guard
// reads the sizes of what it may end at each reading in the band, the bandPercent of the limit
// below the mark, so that it holds two reads or more of each by the time a fast rise reaches the
// mark: the fastest rise measured, the tester's pool probe at about 1.5 GiB a second, crosses a
// band of 10% of a 4 GiB limit (410 MiB) in about 267 ms, and of 8 GiB in about 533 ms, at least two
// readings at the default interval of 100 ms. 10% is the floor INFRA-3368's architect set for that
// reason, and the narrowest band is the one a pod that sits below its mark spends the least time
// reading sizes in, each a scan and a read of the smaps of what it may end. growthWindow is how
// far back a process's growth is measured, shorter than every time from the mark to the kernel's
// kill point measured (about 317 ms for the pool probe at 4 GiB, 1.9 s for a grower of 256 MiB a
// second), and two or three readings at 100 ms; an interval above it leaves no earlier read in
// the window, and the guard then ends the largest first. growthNoise is the least growth that
// counts, over the window and since a kill (grownSinceKill): a grower of 256 MiB a second grows
// about 25 MiB from one reading to the next.
const (
	bandPercent  = 10
	growthWindow = 300 * time.Millisecond
	growthNoise  = 1 << 20
)

// procKey is a process by its pid and its start time, which tells it from a process that takes
// its pid later.
type procKey struct {
	pid   int
	start uint64
}

// sizeRead is one read of a process's size, at the time by the guard's clock of the reading it
// was taken at.
type sizeRead struct {
	at   time.Time
	size uint64
}

// killSize is a process's size as of the previous kill of a rise, from which its growth since that
// kill is measured (grownSinceKill), and the reading by the guard's clock it was last read at, by
// which the guard drops a process it no longer reads.
type killSize struct {
	size uint64
	seen time.Time
}

// window is one hour of readings, from the guard's start onward: each hour's line gives its own
// highs and lows and nothing of the hour before. scannedHigh is the reading at which the window's
// last scan for its largest process started.
type window struct {
	start, end    time.Time
	readings      bool
	readingHigh   uint64
	readingLow    uint64
	readingHighAt time.Time
	counters      bool
	counterHigh   uint64
	counterLow    uint64
	scanned       bool
	scannedHigh   uint64
	largest       largest
}

// largest is the largest process one scan found.
type largest struct {
	pid        int
	role       string
	size       uint64
	executable string
}

// largestScan is a finished scan for the largest process, and the start of the window it was
// started in, so a scan that finishes after its hour is over is not credited to the next one.
type largestScan struct {
	window time.Time
	largest
}

// counterEvery is how often the guard reads the cgroup's counter for the hourly line.
const counterEvery = 5 * time.Second

// stoppedEvery is the least time between two stopped lines: a stop that repeats, as a reading
// that holds near the mark crosses it again and again, writes one line in each stoppedEvery and
// counts the rest (stopRise).
const stoppedEvery = time.Minute

// noLimit is the smallest limit that is no limit at all: cgroup v1 writes an unlimited
// container's limit as the largest page-aligned int64, and no container is given 4 EiB.
const noLimit = 1 << 62

// Guard is one memory guard. One runs in a launcher (Run), and the settings of each role's
// acknowledgement reach it from that role's connection (Apply).
type Guard struct {
	proc     string
	classify Classifier
	sink     func(Kill)
	logf     func(format string, args ...any)
	clock    Clock
	signal   Signaler
	id       string
	self     int
	// observe is Config.Observe. The issue pod's live test (live_test.go) sets it on the guard
	// itself, the controller pod's (the launcher's) through Config.
	observe func(now time.Time, reading, limit uint64)
	// smapsRead, when set, is called with the pid of each smaps the guard's loop reads (size),
	// before it reads it. The tests set it to count which processes the guard reads at which
	// reading, in the band, after a kill and at a stop; production leaves it nil.
	smapsRead func(pid int)
	// scanSmapsRead, when set, is called with the pid of each smaps the scan for the largest
	// process reads (scanLargest), on that scan's goroutine, before it reads it. The tests set it
	// to order those reads against a kill; production leaves it nil.
	scanSmapsRead func(pid int)
	// memfdLink is how the link of a descriptor that holds a memfd starts (heldMemfdBytes):
	// memfdPrefix, as New sets it. The tests set their fake pod's own, since a fake descriptor's
	// link must lead to a file the guard can stat.
	memfdLink string

	mu       sync.Mutex
	settings Settings
	on       bool
	// dead is the guard turned off for good: it cannot read the pod or end a process.
	dead bool

	// The rest is Run's own.
	meminfoFile, limitFile, counterFile heldFile
	// meminfoData is the meminfo of the last reading, which a kill, an assessment and the start
	// of a scan for the largest process read Shmem from.
	meminfoData  []byte
	smaps        []byte
	phase        phase
	wait         afterKill
	sequence     uint64
	window       window
	counterAt    time.Time
	unreadLogged map[int]bool
	// killFailures are the processes the guard could not end, for as long as each lives, by pid
	// with the start time that tells each from a process that takes its pid later: they count as
	// unread from then on, so a refusal does not have the guard pick the same process at every
	// reading while the next in the order grows.
	killFailures map[int]uint64
	// stoppedAt is when the guard last wrote its stopped line, by its clock, and stoppedLeftOut
	// how many stopped lines it has left out since (stopRise).
	stoppedAt      time.Time
	stoppedLeftOut int
	// memfds are the memfds an assessment's sizes have counted (addHeldMemfds), emptied at each
	// assessment and kept for the next.
	memfds map[memfdID]bool
	// sizes are the reads of the size of each process the latest assessment could end, oldest
	// first, none older than growthWindow before it (keepSizes): the reads a process's growth is
	// measured by. They are emptied wherever the guard reads no size (forgetSizes).
	sizes map[procKey][]sizeRead
	// held are the processes an armed guard's assessment at the reading before found it may end,
	// in the band below the mark, each at the size that assessment read, its newest read in
	// sizes: what the next reading, where it is at or above the mark, decides on (endHeld). A
	// reading in the band holds its own, and every other drops them, the one at the mark once it
	// has decided on them, so they are never of a reading older than the one before and never
	// held across a kill; forgetSizes drops them with the reads.
	held []candidate
	// atKill are the sizes, by process, as of the previous kill of the rise, from which growth
	// since that kill is measured (grownSinceKill): the sizes of the assessment of the reading the
	// kill was sent at (act; for a kill on held sizes, the assessment that follows it, endHeld),
	// and for a process first read after the kill, its first read after it. They are emptied when
	// the wait after the kill ends (endWait), and their storage is kept from one kill to the next.
	atKill map[procKey]killSize

	scans    sync.WaitGroup
	scanMu   sync.Mutex
	scanBusy bool
	scanDone *largestScan
	// The rest is scanLargest's own, one scan at a time, since startScan starts none while one
	// runs: its smaps buffer and the memfds its sizes have counted.
	scanSmaps  []byte
	scanMemfds map[memfdID]bool
}

// New is a guard of cfg, at cfg.Start or the default settings and on, with an id of its own.
func New(cfg Config) *Guard {
	var id [8]byte
	_, _ = rand.Read(id[:])
	proc := cmp.Or(cfg.Proc, "/proc")
	cgroup := cmp.Or(cfg.Cgroup, "/sys/fs/cgroup/memory")
	g := &Guard{
		proc:         proc,
		classify:     cfg.Classify,
		sink:         cfg.Sink,
		logf:         cfg.Logf,
		clock:        cfg.Clock,
		signal:       cfg.Signal,
		observe:      cfg.Observe,
		id:           hex.EncodeToString(id[:]),
		self:         os.Getpid(),
		settings:     cmp.Or(cfg.Start, Defaults()),
		on:           true,
		meminfoFile:  heldFile{path: filepath.Join(proc, "meminfo")},
		limitFile:    heldFile{path: filepath.Join(cgroup, "memory.limit_in_bytes")},
		counterFile:  heldFile{path: filepath.Join(cgroup, "memory.usage_in_bytes")},
		unreadLogged: map[int]bool{},
		killFailures: map[int]uint64{},
		memfdLink:    memfdPrefix,
		memfds:       map[memfdID]bool{},
		sizes:        map[procKey][]sizeRead{},
		atKill:       map[procKey]killSize{},
		scanMemfds:   map[memfdID]bool{},
	}
	if g.logf == nil {
		g.logf = func(string, ...any) {}
	}
	if g.clock == nil {
		g.clock = &wallClock{}
	}
	if g.signal == nil {
		g.signal = defaultSignal()
	}
	return g
}

// ID is the guard's own id, random for each guard, which every kill report carries: with the
// report's sequence it names one kill, so a report sent twice is applied once.
func (g *Guard) ID() string { return g.id }

// Apply takes the settings of a role's acknowledgement in place. No settings turn the guard off
// and a later acknowledgement that carries them turns it on again, each with one line; settings
// that differ from the ones it runs by replace them with one line, and the same settings again
// change nothing and log nothing, since every role's connection carries them. Off, the guard
// keeps its readings and its hourly lines and ends nothing. Settings outside shimwire's ranges
// change nothing, with one line. A guard that has turned off for good takes nothing. Apply is
// safe to call from any goroutine.
func (g *Guard) Apply(w *shimwire.LauncherMemoryGuard) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return
	}
	if w == nil {
		if g.on {
			g.on = false
			g.logf(noSettingsLine)
		}
		return
	}
	if err := w.Validate(); err != nil {
		g.logf(refusedLine, err)
		return
	}
	s := SettingsOf(*w)
	if g.on && s == g.settings {
		return
	}
	g.on, g.settings = true, s
	g.logf(settingsLine, s.ActPercent, s.FloorPercent, s.Wait, s.Interval)
}

// Run reads and acts until ctx ends. It logs its start line once, with the first reading; a guard
// that cannot read the pod, or cannot end a process on this system, logs one line instead and
// waits for ctx without reading again.
func (g *Guard) Run(ctx context.Context) {
	defer g.scans.Wait()
	defer g.release()
	if !g.begin() {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.clock.After(g.interval()):
		}
		if !g.tick(false) {
			<-ctx.Done()
			return
		}
	}
}

// begin turns the guard off for good where it has no kill to send, and otherwise takes its first
// reading. It holds its own process once to learn whether pidfd_open is there at all, so a
// sandbox without it says so at the start rather than at the first kill, when the pod is already
// near its limit.
func (g *Guard) begin() bool {
	if g.signal == nil {
		g.die("this build has no pidfd kill; the memory guard runs on Linux alone")
		return false
	}
	if h, err := g.signal.Open(g.self); err == nil {
		_ = h.Close()
	} else if errors.Is(err, errors.ErrUnsupported) {
		g.die(fmt.Sprintf("pidfd_open is not supported here: %v", err))
		return false
	}
	return g.tick(true)
}

// tick is one reading and what follows from it, and false once the guard has turned off for
// good. Of the hourly line, only the window's rollover runs before the guard decides on the
// reading (rollWindow), so that a kill's counter read counts for the window of its own hour; the
// rest of the hour's work, the reading's high and low, the counter read every counterEvery and the
// start of a scan for the largest process, runs after the decision (hour). Nothing of it so runs
// between a reading at the mark and its kill: under gVisor a read of the counter costs about 18 ms,
// and a scan started before the kill would read every process's smaps on its own goroutine while
// the guard reads toward the kill, on a pod whose CPU the host throttles (decide).
func (g *Guard) tick(first bool) bool {
	now := g.clock.Now()
	reading, limit, reason := g.sample()
	if reason != "" {
		g.die(reason)
		return false
	}
	if g.observe != nil {
		g.observe(now, reading, limit)
	}
	s, on := g.current()
	if first {
		g.logf(startedLine, s.ActPercent, s.FloorPercent, s.Wait, s.Interval, limit, reading)
		g.window = window{start: now, end: now.Add(time.Hour)}
	}
	g.rollWindow(now)
	g.decide(now, s, on, reading, limit)
	if g.isDead() {
		return false
	}
	g.hour(now, reading, limit)
	return true
}

// sample is one reading: the pod's AnonPages and the container's limit, in bytes, or why the guard
// cannot read them. It allocates nothing unless it fails.
func (g *Guard) sample() (reading, limit uint64, reason string) {
	data, err := g.meminfoFile.read()
	if err != nil {
		return 0, 0, fmt.Sprintf("no readable AnonPages in %s: %v", g.meminfoFile.path, err)
	}
	g.meminfoData = data
	reading, ok := meminfoBytes(data, "AnonPages")
	if !ok {
		return 0, 0, "no AnonPages line in " + g.meminfoFile.path
	}
	data, err = g.limitFile.read()
	if err != nil {
		return 0, 0, fmt.Sprintf("no readable limit in %s: %v", g.limitFile.path, err)
	}
	limit, ok = parseCount(data)
	switch {
	case !ok || limit == 0:
		return 0, 0, fmt.Sprintf("no readable limit in %s: it reads %q", g.limitFile.path, bytes.TrimSpace(data))
	case limit >= noLimit:
		return 0, 0, fmt.Sprintf("the limit %d in %s is no limit", limit, g.limitFile.path)
	}
	return reading, limit, ""
}

// decide acts on one reading by the guard's phase in the rise. An armed guard whose reading is in
// the band below the mark (bandStart) reads the sizes of what it may end (assessAt) and holds them
// for the next reading (held). At its first reading at or above the mark it decides on those held
// sizes and the reads kept with them (endHeld): it sends the kill before it scans the process
// table, and before it reads any smaps where one of the held processes grew by the held reads; where
// none did, it reads the smaps of the held processes alone first (rereadHeld), since the step that
// took the reading to the mark falls after the band's last reading and the held reads cannot show
// it, and a guard that did not read them again ended a sleeping fork before the grower in the
// `shared` probe under gVisor (INFRA-3531 T17). Only then does it assess the reading, whose sizes
// are the sizes as of the kill; tick runs nothing of the hourly line but its rollover before
// decide returns. That is the cut of INFRA-3531 T16, so the first kill comes within one interval of
// the reading reaching the mark: with six growers on the pod's CPU the host throttles the whole
// pod, a band reading of about 2 ms of work took 80 to 270 ms of wall time, and a mark reading that
// read every size again put its kill 194 ms after the reading, where one that decided on the band's
// reads put it 1.3 to 71.8 ms after it in 9 of 9 runs. Where nothing is held (the reading crossed
// the whole band within one interval, or the reading before was not an armed guard's in the band),
// where the held first has exited or its pid names another process (its start time changed), or
// where the held sizes, or the sizes read again, are under the floor or name no process, the guard
// assesses the reading at the mark and acts on it (act), as on any reading of a rise. A reading
// that ends a rise, below the mark after a kill or after the rise was given up, is an armed
// guard's, and reads and holds the sizes where it is in the band. Below the band, while off and
// while the rise is given up, the guard reads no size and keeps none (forgetSizes).
func (g *Guard) decide(now time.Time, s Settings, on bool, reading, limit uint64) {
	mark := share(limit, s.ActPercent)
	if !on {
		g.endWait()
		g.forgetSizes()
		if g.phase == stopped && reading < mark {
			g.phase = armed
		}
		return
	}
	switch g.phase {
	case stopped:
		if reading >= mark {
			return
		}
		g.phase = armed
	case waiting:
		if reading >= mark {
			g.watch(now, s, reading, limit, mark)
			return
		}
		// The rise is over.
		g.endWait()
	}
	if reading < bandStart(limit, s.ActPercent) {
		g.forgetSizes()
		return
	}
	if reading >= mark && g.held != nil {
		sent := g.endHeld(now, s, reading, limit)
		g.held = nil
		if sent {
			// The kill is sent: this reading's sizes are the sizes as of it, and the window's
			// newest reads.
			if a, ok := g.assessAt(now); ok {
				g.keepKillSizes(now, a.endable)
			}
			return
		}
		if g.isDead() {
			return
		}
	}
	a, ok := g.assessAt(now)
	switch {
	case !ok:
	case reading < mark:
		g.held = a.endable
	default:
		g.act(now, s, reading, limit, &a)
	}
}

// endHeld ends the first of the held processes, the ones the assessment at the reading before,
// in the band, found the guard may end, at the reading taken at now, the first at or above the
// mark, and answers whether it sent the kill. It scans no process table and reads no descriptor
// before the kill (sendKill). The order is the order of ends (rank) at now: each process's growth
// is its newest held read, the size it is held at, less its oldest held read within growthWindow
// before now, growthNoise or more, so a read the window held at the band's reading and no longer
// holds at now counts for nothing, and a step older than the window is no growth; a process with
// no earlier read in the window has not grown. Where one of them has grown, it decides on the held
// reads alone and reads no smaps before the kill.
//
// Where none of them has grown by the held reads, it reads their smaps again first (rereadHeld)
// and decides on those reads, since the held reads cannot show the step that took the reading to
// the mark: that step comes between the band's last reading and this one. Under gVisor at the head
// that decided on the held reads alone (INFRA-3531 T17), the `shared` probe at 4 GiB, a grower
// stepping 64 MiB about every 250 ms beside three sleeping forks of a 1.5 GiB block, had its first
// kill end a sleeper: the grower's step before the crossing one was older than the window at the
// mark, so the held order showed no growth and put the largest first, a fork whose end frees
// nothing; the sharedpool and latepool probes lost a sleeper at the first kill of each rise the
// same way, their band readings spread 170 to 210 ms apart by the throttle and one held read left
// in the window. Where nothing grew at all, the reads again still put the largest first.
//
// The floor is read on the held sizes, and again on the sizes read again. It sends no kill where
// they are under the floor or name no process, or where the first has exited, its pid names
// another process or its kill fails, and decide then assesses the reading and acts on it: what
// the band held never gives a rise up.
func (g *Guard) endHeld(now time.Time, s Settings, reading, limit uint64) bool {
	floor := share(limit, s.FloorPercent)
	var heldBytes uint64
	grew := false
	for i := range g.held {
		c := &g.held[i]
		reads := g.sizes[procKey{pid: c.PID, start: c.StartTime}]
		c.growth = growthOver(reads[windowStart(reads, now):], c.size)
		grew = grew || c.growth > 0
		heldBytes += c.size
	}
	if len(g.held) == 0 || heldBytes < floor {
		return false
	}
	if grew {
		rank(g.held)
	} else if g.rereadHeld(now) < floor || len(g.held) == 0 {
		return false
	}
	return g.sendKill(now, s, reading, limit, g.held[0])
}

// rereadHeld reads again, at the reading taken at now, the smaps of each held process, and of no
// other: it scans no process table, asks no classifier and reads no descriptor. Each one's size is
// its smaps size now and the memfds it holds by a descriptor alone at what the band's assessment
// counted for them (descriptorBytes). It keeps those reads as the window's reads at now
// (keepSizes), which sets each one's growth at now, ranks the held processes by them, and answers
// their sum. A held process whose smaps it cannot read, gone or not, it leaves out of the held
// processes, and so out of this decision; the assessment of the reading that follows reads it again.
func (g *Guard) rereadHeld(now time.Time) uint64 {
	defer g.releaseSmaps()
	read := g.held[:0]
	var total uint64
	for _, c := range g.held {
		size, err := g.size(c.PID, nil)
		if err != nil {
			continue
		}
		c.size = size + c.descriptorBytes
		total += c.size
		read = append(read, c)
	}
	g.held = read
	g.keepSizes(now, g.held)
	rank(g.held)
	return total
}

// watch decides on a reading at or above the mark while the guard waits after a kill, by the rule
// of INFRA-3531 R1 of spec version 25. It reads the sizes of what it may end at each reading, so
// that their order and their growth since the kill (grownSinceKill) are known whenever it decides.
// While the killed process has not exited it is left out of what the guard may end, of their sum
// against the floor and of the growth since the kill (leaveOut): the guard has ended it, and never
// ends or reports it again. While the killed process has not exited and the wait has not passed,
// it ends nothing, because the reading can fall below the mark as the rest of the process's memory
// goes. Once the process has exited or the wait has passed, it ends the next in the order (act),
// at once, where the reading has fallen, since the kill freed something and the pod is still at
// the mark, and where it has not fallen but a process it may end has grown since the kill and
// those processes hold at least the floor between them, since a kill that frees less than one
// interval's rise leaves the pod rising toward the kernel's kill point. Where neither holds it ends
// nothing and decides again at the next reading, until the wait has passed; then it gives the rise
// up with reason not fallen, whichever of the two failed, since the kill's freeing nothing is why
// it stops. So a rise that is a protected process's own, which no kill lowers and in which nothing
// the guard may end grows, costs at most one kill.
func (g *Guard) watch(now time.Time, s Settings, reading, limit, mark uint64) {
	w := &g.wait
	w.exited = w.exited || w.handle.Exited()
	w.fallen = w.fallen || fell(w.reading, reading, mark)
	passed := !now.Before(w.deadline)
	a, ok := g.assessAt(now)
	if !ok {
		return
	}
	if !w.exited {
		a.leaveOut(w.killed)
	}
	// At every reading of the wait, so that a process first read after the kill has its first
	// read kept. It sets each one's growth since the kill, the order's middle tier (rank), so the
	// processes are ranked again by it.
	grown := g.grownSinceKill(now, a.endable)
	rank(a.endable)
	switch {
	case !w.exited && !passed:
		// The wait goes on.
	case w.fallen || (grown && a.endableBytes >= share(limit, s.FloorPercent)):
		g.endWait()
		g.act(now, s, reading, limit, &a)
	case passed:
		g.endWait()
		g.stopRise(now, reasonNotFallen, reading, limit, &a)
	}
}

// bandStart is where the band below the mark at act percent of limit starts: bandPercent of limit
// below the mark, or 0 for a mark below bandPercent.
func bandStart(limit uint64, act int) uint64 {
	mark, band := share(limit, act), share(limit, bandPercent)
	if band > mark {
		return 0
	}
	return mark - band
}

// assessAt is an assessment of the processes at the reading taken at now (assess), with the
// guard's smaps buffer let go of once it has grown past maxHeldFile, and false where the guard
// cannot list the processes, which turns it off for good.
func (g *Guard) assessAt(now time.Time) (assessment, bool) {
	a, err := g.assess(now)
	g.releaseSmaps()
	if err != nil {
		g.die(fmt.Sprintf("cannot list the processes in %s: %v", g.proc, err))
		return assessment{}, false
	}
	return a, true
}

// fell is whether a reading has fallen after a kill taken at the reading atKill: it is below that
// reading by any amount, or below the mark. Any fall counts, not one the size of the killed
// process, because a forked worker shares most of its pages with its parent and its siblings: its
// kill frees only the pages it wrote itself, far less than the size its smaps read, and a rule
// that waited for that size would stop for the rise after the first worker of a pool that is
// still filling the pod. A kill that lowers the reading by nothing freed nothing, and the guard
// stops there, unless a process it may end has grown since the kill (watch), rather than end
// process after process for memory no kill frees.
func fell(atKill, reading, mark uint64) bool { return reading < atKill || reading < mark }

// act ends the first process of a, the assessment of this reading, in the guard's order of ends
// (rank), or gives the rise up when the processes it may end hold less than the floor between
// them: ending one of them would free too little to matter. The first kill of a rise is not act's
// where the band holds what to decide on: at the first reading at or above the mark the guard
// decides on the band's held reads before it scans the process table (endHeld), reading again the
// smaps of the held processes alone where none of them grew by the held reads (rereadHeld), since
// with six growers on the pod's CPU the host throttles the whole pod, and a mark reading that
// read every size again, as act's assessment does, put the first kill 194 ms after the reading,
// where R1 asks for one interval (100 ms); deciding on the band's reads put it 1.3 to 71.8 ms after
// it in 9 of 9 runs (INFRA-3531 T16). act decides that first kill where the band's reads cannot:
// nothing is held (the reading crossed the whole band within one interval, or the reading before
// was not an armed guard's in the band), the held first has exited or its pid names another process
// (its start time changed), or the held sizes, or the sizes read again, are under the floor or name
// no process. It decides every kill after a kill of the rise too (watch), on each reading's own
// sizes. The kill, and what keeps it from a process that ended or whose pid another process took,
// is sendKill's.
func (g *Guard) act(now time.Time, s Settings, reading, limit uint64, a *assessment) {
	if len(a.endable) == 0 || a.endableBytes < share(limit, s.FloorPercent) {
		g.stopRise(now, reasonNothingToEnd, reading, limit, a)
		return
	}
	if g.sendKill(now, s, reading, limit, a.endable[0]) {
		g.keepKillSizes(now, a.endable)
	}
}

// sendKill sends SIGKILL to target through a pidfd, logs the kill's ended line and reports it, with
// target's size as the guard read it and reading, the reading taken at now that the kill is sent
// at, and starts the wait after it; it answers whether it sent the kill. Before the kill it reads
// nothing of the pod but target's stat, for its start time. A process that ended (exited, whether
// its parent has reaped it yet or not), or whose pid another process took, since it was read is
// not killed and not reported, and the guard decides again: at this reading where the band's held
// reads named it (endHeld), and otherwise at the next reading, whose scan leaves out a zombie. One
// that exits in the instant between the handle's Exited and its Kill is still reported, since no
// read can close that window. A process the kill fails on for any other reason is logged once and
// counted unread from then on (noteKillFailure), so the next decision ends the next in the order.
func (g *Guard) sendKill(now time.Time, s Settings, reading, limit uint64, target candidate) bool {
	h, err := g.signal.Open(target.PID)
	if err != nil {
		g.killFailed(target.Process, err)
		return false
	}
	if start, err := readStartTime(g.proc, target.PID); err != nil || start != target.StartTime {
		_ = h.Close()
		return false
	}
	// An exited process its parent has not reaped yet keeps its pid and its start time, so the
	// check above passes it, and the kill of it answers no error. The handle is asked because it
	// holds the very process the kill goes to, so its answer is of that process and of no other.
	if h.Exited() {
		_ = h.Close()
		return false
	}
	if err := h.Kill(); err != nil {
		_ = h.Close()
		g.killFailed(target.Process, err)
		return false
	}
	at := g.clock.Now()
	counter := g.readCounter(at)
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	executable := processName(target.Process)
	g.sequence++
	g.logf(endedLine, target.PID, roleName(target.role), target.size, reading, counter, shmem, limit, executable)
	if g.sink != nil {
		g.sink(Kill{
			LauncherMemoryKill: shimwire.LauncherMemoryKill{
				Guard: g.id, Sequence: g.sequence, PID: target.PID,
				Executable: executable, SizeBytes: target.size,
				ReadingBytes: reading, CounterBytes: counter, LimitBytes: limit, At: at,
			},
			Role: target.role,
		})
	}
	g.phase = waiting
	killed := procKey{pid: target.PID, start: target.StartTime}
	g.wait = afterKill{handle: h, killed: killed, reading: reading, deadline: now.Add(s.Wait)}
	return true
}

// killFailed acts on a kill that failed: a system with no way to hold or signal a process turns
// the guard off, a process that has ended (ESRCH) leaves the guard to decide again (sendKill), and
// any other failure is noted against the process (noteKillFailure).
func (g *Guard) killFailed(p *Process, err error) {
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		g.die(fmt.Sprintf("pidfd is not supported here: %v", err))
	case errors.Is(err, syscall.ESRCH):
	default:
		g.noteKillFailure(p, err)
	}
}

// errKillFailed is why a process the kill failed on counts unread at each later scan; the failure
// itself has had its line when it was noted (noteKillFailure).
var errKillFailed = errors.New("the kill failed")

// noteKillFailure logs a process the kill failed on, once, and keeps it by its start time, which
// tells it from a process that takes its pid later, so that every later scan counts it unread for
// as long as it lives.
func (g *Guard) noteKillFailure(p *Process, err error) {
	g.killFailures[p.PID] = p.StartTime
	g.logf(killFailedLine, p.PID, err)
}

// applyKillFailures counts each process the kill failed on unread in t, and forgets each that has
// ended or whose pid another process now has.
func (g *Guard) applyKillFailures(t *Table) {
	for pid, start := range g.killFailures {
		if p, listed := t.byPID[pid]; listed && p.StartTime == start {
			t.unread[pid] = errKillFailed
		} else {
			delete(g.killFailures, pid)
		}
	}
}

// candidate is a process with its size as the guard ranks it, and its role: in an assessment, a
// process the guard may end. growth is how much its size grew over the window (keepSizes, and for
// a held process at the reading at the mark, endHeld), 0 for one that grew by less than
// growthNoise or has no earlier read in the window, and for every process a scan for the hourly
// line reads. sinceKill is how much it grew since the previous kill of the rise (grownSinceKill),
// 0 under growthNoise and wherever no kill of the rise has been made. descriptorBytes is the part
// of size that the memfds it holds by a descriptor alone make up (addHeldMemfds), which a read of
// its smaps again at the mark adds as it was counted (rereadHeld).
type candidate struct {
	*Process
	role            string
	size            uint64
	growth          uint64
	sinceKill       uint64
	descriptorBytes uint64
}

// assessment is one scan as the guard acts on it: the processes it may end, in the order it ends
// them (rank), and their sizes' sum. leftOut is the pid of the process leaveOut took out of them,
// 0 for none, and leftOutBytes its size.
type assessment struct {
	table        *Table
	endable      []candidate
	endableBytes uint64
	leftOut      int
	leftOutBytes uint64
}

// leaveOut takes the process key out of a, the process the guard has killed and which has not
// exited: out of the processes it may end, the order of the rest unchanged, so that the next it
// ends is another; and out of their sum, so that the floor counts only what it may still end. A
// process the guard has killed is so never killed or reported twice while it lingers. It is kept
// as leftOut for the stopped line alone (stopRise).
func (a *assessment) leaveOut(key procKey) {
	for i, c := range a.endable {
		if c.PID == key.pid && c.StartTime == key.start {
			a.endableBytes -= c.size
			a.endable = slices.Delete(a.endable, i, i+1)
			a.leftOut, a.leftOutBytes = c.PID, c.size
			return
		}
	}
}

// assess scans the processes, asks the classifier, and reads the size of each process it may
// end: one that is not protected, not the guard's own and not unread. A process whose smaps
// cannot be read is unread too, since it cannot be ranked, unless it has ended since the scan
// read its stat (endedSince): it is then left out of the processes it may end and of unread
// alike. The processes it may end are the only ones whose files it reads, so they alone are the
// holders and the mappers of memfds among which a memfd counts once (addHeldMemfds). Each size is
// kept as read at now, the time of the reading the assessment is made at, and its growth measured
// from the reads kept before it (keepSizes).
func (g *Guard) assess(now time.Time) (assessment, error) {
	t, err := scan(g.proc)
	if err != nil {
		return assessment{}, err
	}
	g.applyKillFailures(t)
	classes := g.classes(t)
	a := assessment{table: t}
	clear(g.memfds)
	for _, p := range t.Processes() {
		if _, unread := t.unread[p.PID]; unread || p.PID == g.self || classes[p.PID].Protected {
			continue
		}
		size, err := g.size(p.PID, g.memfds)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !endedSince(filepath.Join(g.proc, strconv.Itoa(p.PID)), p.StartTime) {
				t.unread[p.PID] = fmt.Errorf("read the smaps: %w", err)
			}
			continue
		}
		a.endable = append(a.endable, candidate{Process: p, role: classes[p.PID].Role, size: size})
	}
	// A held memfd counts at most the Shmem of the reading the decision is made on. A meminfo
	// with no Shmem line gives 0, and a memfd held and not mapped then counts nothing; Linux and
	// gVisor both print the line.
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	addHeldMemfds(a.endable, g.proc, g.memfdLink, g.memfds, shmem)
	// addHeldMemfds ranked them by size alone, the order where none grew.
	if g.keepSizes(now, a.endable) {
		rank(a.endable)
	}
	for _, c := range a.endable {
		a.endableBytes += c.size
	}
	g.logUnread(t)
	return a, nil
}

// keepSizes keeps the size of each of cs as read at now, and sets each one's growth: its size less
// its size at its oldest read kept within growthWindow before now, where that is growthNoise or
// more. A read older than the window counts for nothing and is dropped, and so are the reads of
// each process cs does not hold, one that has ended, whose pid another process took (a read is
// kept by pid and start time) or that the guard may no longer end, so the reads kept are bounded
// by the processes the guard may end and the readings one window holds. A read already kept at
// now, the held processes' read at the mark (rereadHeld) where the assessment of that reading
// follows it, is replaced: a process has one read at each reading. A process with no earlier read
// in the window has not grown: a fresh fork reads its parent's shared pages as its own. It answers
// whether any of cs grew. Each process's reads keep their storage from one reading to the next, so
// the reads of a process the guard has read before allocate nothing.
func (g *Guard) keepSizes(now time.Time, cs []candidate) bool {
	grew := false
	for i := range cs {
		c := &cs[i]
		key := procKey{pid: c.PID, start: c.StartTime}
		reads := g.sizes[key]
		reads = reads[:copy(reads, reads[windowStart(reads, now):])]
		if n := len(reads); n > 0 && reads[n-1].at.Equal(now) {
			reads = reads[:n-1]
		}
		if c.growth = growthOver(reads, c.size); c.growth > 0 {
			grew = true
		}
		g.sizes[key] = append(reads, sizeRead{at: now, size: c.size})
	}
	for key, reads := range g.sizes {
		if !reads[len(reads)-1].at.Equal(now) {
			delete(g.sizes, key)
		}
	}
	return grew
}

// windowStart is the index of the oldest of reads, oldest first, within growthWindow before now,
// len(reads) where none is.
func windowStart(reads []sizeRead, now time.Time) int {
	i := 0
	for i < len(reads) && now.Sub(reads[i].at) > growthWindow {
		i++
	}
	return i
}

// growthOver is how much size grew from the oldest of reads, a process's reads within the window,
// where that is growthNoise or more, and 0 otherwise or where reads holds none.
func growthOver(reads []sizeRead, size uint64) uint64 {
	if len(reads) > 0 && size >= reads[0].size+growthNoise {
		return size - reads[0].size
	}
	return 0
}

// forgetSizes drops every size read the guard keeps, and the processes it holds for the next
// reading (held), wherever it reads none: below the band, while off and while a rise is given up
// (decide, stopRise). The reads it keeps are so those of readings in a row in the band, at the
// mark and after a kill.
func (g *Guard) forgetSizes() {
	clear(g.sizes)
	g.held = nil
}

// keepKillSizes keeps the size of each of cs, the processes of the assessment of the reading a
// kill was sent at (act's, or for a kill on held sizes the one that follows it, decide), read at
// now, as its size as of that kill (atKill), from which grownSinceKill measures its growth until
// the next kill. The sizes of the kill before were emptied when its wait ended (endWait), so
// those of one kill alone are kept, in the storage of the kills before.
func (g *Guard) keepKillSizes(now time.Time, cs []candidate) {
	for _, c := range cs {
		g.atKill[procKey{pid: c.PID, start: c.StartTime}] = killSize{size: c.size, seen: now}
	}
}

// grownSinceKill answers whether a process of cs, the processes the guard may end at the reading
// taken at now, has grown since the previous kill of the rise: its size is its size as of the kill
// (atKill) plus growthNoise or more. It sets each one's sinceKill to that growth, 0 under
// growthNoise, which orders those that did not grow over the window (rank). Growth since the kill,
// not over the window that orders the processes, is what it measures, so a process that grows in
// steps wider than the window counts as grown. A process with no size as of the kill, one first
// read after it, has that first read kept as its size as of the kill, and has not grown at it: a
// fork reads its parent's shared pages as its own. The sizes of each process cs does not hold, one
// that has ended or that the guard may no longer end, are dropped, so those kept are bounded by the
// processes it may end, and a process read before allocates nothing.
func (g *Guard) grownSinceKill(now time.Time, cs []candidate) bool {
	grew := false
	for i := range cs {
		c := &cs[i]
		key := procKey{pid: c.PID, start: c.StartTime}
		k, known := g.atKill[key]
		switch {
		case !known:
			k.size = c.size
		case c.size >= k.size+growthNoise:
			c.sinceKill, grew = c.size-k.size, true
		}
		k.seen = now
		g.atKill[key] = k
	}
	for key, k := range g.atKill {
		if !k.seen.Equal(now) {
			delete(g.atKill, key)
		}
	}
	return grew
}

// addHeldMemfds adds to the size of each process of cs, read under proc, the memfds it holds open
// by a descriptor, each at its size or shmem, the pod's Shmem reading, whichever is less
// (heldMemfdBytes), keeps what it added as the process's descriptorBytes, and ranks cs by those
// sizes (rank, on candidates whose growth is not yet set). Ending a holder frees only its memfds'
// resident pages, which Shmem bounds and which under gVisor neither a memfd's size nor its block
// count shows. counted holds, on entry, the memfds that the reads of cs's smaps found mapped, which
// count through their mappings alone. cs is ranked by those smaps sizes before any descriptor is
// read, so the first of a memfd's holders to read it, the one it counts for, is the largest of
// them by the size without any memfd a descriptor holds, the newest among equal sizes.
func addHeldMemfds(cs []candidate, proc, link string, counted map[memfdID]bool, shmem uint64) {
	rank(cs)
	for i := range cs {
		cs[i].descriptorBytes = heldMemfdBytes(filepath.Join(proc, strconv.Itoa(cs[i].PID)), link, counted, shmem)
		cs[i].size += cs[i].descriptorBytes
	}
	rank(cs)
}

// rank sorts cs in the guard's order of ends (INFRA-3531 R1 of spec version 26): the most grown
// over the window first (growth); then, of those that did not grow over the window, the most grown
// since the previous kill of the rise (sinceKill, set at each reading after a kill by
// grownSinceKill, and none before a rise's first kill); then, of those that grew by neither, the
// largest first; and the newest first among equal sizes: of a parent and the child it forked,
// which read the same, the child is the one that was started for the work. Where none of cs grew,
// the largest is first. The middle tier is there because a throttled pod's readings come 170 to
// over 300 ms apart, so the window can hold one read of each process and show no growth at all:
// in the latepool and sharedpool probes under gVisor (INFRA-3531 T17), the guard then went on
// after a kill by ending the largest, a sleeping fork whose end frees nothing, while the growers
// lived. Growth since the kill still shows who grew.
func rank(cs []candidate) {
	slices.SortFunc(cs, func(x, y candidate) int {
		return cmp.Or(cmp.Compare(y.growth, x.growth), cmp.Compare(y.sinceKill, x.sinceKill),
			cmp.Compare(y.size, x.size), cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
	})
}

// logUnread logs each unread process once for as long as its pid lives. A process the kill failed
// on has had its own line (noteKillFailure).
func (g *Guard) logUnread(t *Table) {
	for pid := range g.unreadLogged {
		_, listed := t.byPID[pid]
		_, unread := t.unread[pid]
		if !listed && !unread {
			delete(g.unreadLogged, pid)
		}
	}
	for _, pid := range slices.Sorted(maps.Keys(t.unread)) {
		if _, failed := g.killFailures[pid]; !failed && !g.unreadLogged[pid] {
			g.unreadLogged[pid] = true
			g.logf(unreadLine, pid, t.unread[pid])
		}
	}
}

// stopRise gives up on this rise with one line that says where the reading's memory is: in the
// processes the guard may end, at their sizes, the killed process that has not exited among them
// (leaveOut), which no decision counts but which is of their kind and holds its memory still; in
// the protected ones and in the ones it could not read, at their smaps sizes alone (smapsSize),
// since the line does not read their descriptors; and the rest, which no process holds (tmpfs
// pages, which no kill frees) or only a descriptor of a process the guard may not end holds (a
// memfd it has not mapped), or, negative, what the processes' sizes count more than once (a page
// a parent and its forked child share). a is the assessment the decision was made on, this
// reading's (act, watch). A guard that has given the rise up reads no size until the reading is
// below the mark, and keeps none (forgetSizes).
//
// A stop that repeats, as a reading that holds near the mark goes below it and back above it,
// writes its line at most once in each stoppedEvery by the guard's clock: each stop in between is
// counted and left out, and the next line written carries that count (left_out). What the line
// alone needs, the smaps of every process the guard may not end, is read only for a line written.
// The scan a stop is decided on, with the smaps of the processes the guard may end, is read at
// every rise, since it decides whether to act.
func (g *Guard) stopRise(now time.Time, reason string, reading, limit uint64, a *assessment) {
	defer g.releaseSmaps()
	g.phase = stopped
	g.forgetSizes()
	if !g.stoppedAt.IsZero() && now.Sub(g.stoppedAt) < stoppedEvery {
		g.stoppedLeftOut++
		return
	}
	var protected, unread uint64
	counted := make(map[int]bool, len(a.endable)+1)
	for _, c := range a.endable {
		counted[c.PID] = true
	}
	if a.leftOut != 0 {
		counted[a.leftOut] = true
	}
	pids := slices.Collect(maps.Keys(a.table.byPID))
	for pid := range a.table.unread {
		if _, listed := a.table.byPID[pid]; !listed {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		if counted[pid] {
			continue
		}
		size, err := g.size(pid, nil)
		if err != nil {
			continue
		}
		if _, isUnread := a.table.unread[pid]; isUnread {
			unread += size
		} else {
			protected += size
		}
	}
	// The line says where the memory is, and a killed process that has not exited still holds
	// its own: it counts in endable here, though every decision leaves it out (leaveOut).
	endable := a.endableBytes + a.leftOutBytes
	unheld := int64(reading) - int64(endable+protected+unread)
	g.logf(stoppedLine, reason, reading, endable, protected, unread, unheld, limit, g.stoppedLeftOut)
	g.stoppedAt, g.stoppedLeftOut = now, 0
}

// endWait lets go of the process a waiting guard holds, and of the sizes as of its kill (atKill):
// the wait ends at the next kill of the rise, which keeps its own, and when the rise ends.
func (g *Guard) endWait() {
	if g.phase != waiting {
		return
	}
	_ = g.wait.handle.Close()
	g.wait = afterKill{}
	clear(g.atKill)
	g.phase = armed
}

// rollWindow credits a finished scan for the largest process to the window it was started in
// (takeScan), and closes the window whose hour has passed, with its line, for the one the reading
// taken at now falls in. It is the one part of the hour's work that runs before the guard decides
// on the reading (tick), so that a kill's counter read counts for the window of its own hour.
func (g *Guard) rollWindow(now time.Time) {
	g.takeScan()
	if !now.Before(g.window.end) {
		g.closeWindow()
		start := g.window.end
		start = start.Add(now.Sub(start).Truncate(time.Hour))
		g.window = window{start: start, end: start.Add(time.Hour)}
	}
}

// hour takes the reading taken at now into the hour's window, once the guard has decided on it
// (tick), so that none of it runs between a reading at the mark and its kill. It reads the counter
// every counterEvery and at the window's first reading, unless a kill at this reading has read it
// already (readCounter sets counterAt), and starts a scan for the largest process when the reading
// is a new high of the hour at least 1% of the limit above the high the last scan started at. The
// scan runs on its own goroutine and the reading never waits for it.
func (g *Guard) hour(now time.Time, reading, limit uint64) {
	w := &g.window
	first := !w.readings
	if first || reading > w.readingHigh {
		w.readingHigh, w.readingHighAt = reading, now
	}
	if first || reading < w.readingLow {
		w.readingLow = reading
	}
	w.readings = true
	if (first && g.counterAt.Before(now)) || now.Sub(g.counterAt) >= counterEvery {
		g.readCounter(now)
	}
	if reading == w.readingHigh && (!w.scanned || reading >= w.scannedHigh+limit/100) && g.startScan(w.start) {
		w.scanned, w.scannedHigh = true, reading
	}
}

// closeWindow logs the hourly line of the window that has passed, if it holds any reading.
func (g *Guard) closeWindow() {
	w := g.window
	if !w.readings {
		return
	}
	g.logf(hourLine, stamp(w.start), stamp(w.end), w.readingHigh, stamp(w.readingHighAt), w.readingLow,
		w.counterHigh, w.counterLow, w.largest.pid, roleName(w.largest.role), w.largest.size, w.largest.executable)
}

// readCounter reads the cgroup's counter into the window, keeps now as when it last read it
// (counterAt), and answers it: 0 when it cannot be read, which it is not the guard's to act on.
func (g *Guard) readCounter(now time.Time) uint64 {
	g.counterAt = now
	data, err := g.counterFile.read()
	if err != nil {
		return 0
	}
	n, ok := parseCount(data)
	if !ok {
		return 0
	}
	w := &g.window
	if !w.counters || n > w.counterHigh {
		w.counterHigh = n
	}
	if !w.counters || n < w.counterLow {
		w.counterLow = n
	}
	w.counters = true
	return n
}

// startScan starts a scan for the largest process in the window that starts at start, unless one
// is running. It reads here, on the loop's goroutine, the Shmem of the reading that starts the
// scan, by which the scan caps each memfd a descriptor holds, and hands the scan that value: the
// loop replaces meminfoData at each reading while the scan runs.
func (g *Guard) startScan(start time.Time) bool {
	g.scanMu.Lock()
	defer g.scanMu.Unlock()
	if g.scanBusy {
		return false
	}
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	g.scanBusy = true
	g.scans.Add(1)
	go g.scanLargest(start, shmem)
	return true
}

// takeScan credits a finished scan to the window it was started in.
func (g *Guard) takeScan() {
	g.scanMu.Lock()
	done := g.scanDone
	g.scanDone = nil
	g.scanMu.Unlock()
	if done != nil && done.window.Equal(g.window.start) {
		g.window.largest = done.largest
	}
}

// scanLargest finds the largest process of every process the pod shows, protected ones among
// them: the hourly line says what held the memory, whatever the guard may end. It ranks them by
// the sizes and in the order an assessment ranks the processes the guard may end (addHeldMemfds),
// every process whose smaps it read being a holder and a mapper of memfds here, and shmem the
// Shmem of the reading that started it (startScan).
func (g *Guard) scanLargest(start time.Time, shmem uint64) {
	defer g.scans.Done()
	found := largestScan{window: start}
	if t, err := scan(g.proc); err == nil {
		classes := g.classes(t)
		clear(g.scanMemfds)
		read := make([]candidate, 0, len(t.byPID))
		for _, p := range t.Processes() {
			if g.scanSmapsRead != nil {
				g.scanSmapsRead(p.PID)
			}
			data, err := readAll(g.smapsPath(p.PID), g.scanSmaps)
			g.scanSmaps = data
			if err != nil {
				continue
			}
			read = append(read, candidate{Process: p, role: classes[p.PID].Role, size: smapsSize(data, g.scanMemfds)})
		}
		g.scanSmaps = keepSmaps(g.scanSmaps)
		addHeldMemfds(read, g.proc, g.memfdLink, g.scanMemfds, shmem)
		if len(read) > 0 {
			biggest := read[0]
			found.pid, found.role, found.size = biggest.PID, biggest.role, biggest.size
			found.executable = processName(biggest.Process)
		}
	}
	g.scanMu.Lock()
	g.scanDone, g.scanBusy = &found, false
	g.scanMu.Unlock()
}

// classes is the classifier's answer for t, or none.
func (g *Guard) classes(t *Table) map[int]Class {
	if g.classify == nil {
		return nil
	}
	return g.classify(t)
}

// size is pid's smaps size (smapsSize), read into the guard's own buffer, which adds each memfd
// pid maps shared to mapped when mapped is not nil.
func (g *Guard) size(pid int, mapped map[memfdID]bool) (uint64, error) {
	if g.smapsRead != nil {
		g.smapsRead(pid)
	}
	data, err := readAll(g.smapsPath(pid), g.smaps)
	g.smaps = data
	if err != nil {
		return 0, err
	}
	return smapsSize(data, mapped), nil
}

// releaseSmaps lets go of the guard's smaps buffer once it has grown past maxHeldFile (keepSmaps).
func (g *Guard) releaseSmaps() { g.smaps = keepSmaps(g.smaps) }

// keepSmaps is buf, kept for the next read of a smaps, or nil once it has grown past maxHeldFile:
// a process with many mappings grows the buffer to the size of its smaps, and the guard does not
// hold that much of the pod's memory from one scan to the next.
func keepSmaps(buf []byte) []byte {
	if cap(buf) > maxHeldFile {
		return nil
	}
	return buf
}

func (g *Guard) smapsPath(pid int) string {
	return filepath.Join(g.proc, strconv.Itoa(pid), "smaps")
}

// die turns the guard off for good, with one line that says why.
func (g *Guard) die(reason string) {
	g.logf(offLine, reason)
	g.mu.Lock()
	g.dead = true
	g.mu.Unlock()
	g.endWait()
}

func (g *Guard) isDead() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dead
}

func (g *Guard) current() (Settings, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settings, g.on
}

func (g *Guard) interval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settings.Interval
}

// release lets go of what Run holds when it returns.
func (g *Guard) release() {
	g.endWait()
	g.meminfoFile.close()
	g.limitFile.close()
	g.counterFile.close()
}

// share is percent of limit, with no product that could pass a uint64.
func share(limit uint64, percent int) uint64 {
	p := uint64(percent)
	return limit/100*p + limit%100*p/100
}

// wallClock is the time of the machine, with one timer reused for every wait.
type wallClock struct{ timer *time.Timer }

func (*wallClock) Now() time.Time { return time.Now() }

func (c *wallClock) After(d time.Duration) <-chan time.Time {
	if c.timer == nil {
		c.timer = time.NewTimer(d)
	} else {
		c.timer.Reset(d)
	}
	return c.timer.C
}
