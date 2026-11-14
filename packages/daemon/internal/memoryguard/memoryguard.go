// Package memoryguard ends the largest process it may end when a container nears its memory
// limit, so that the kernel does not end the whole sandbox instead (INFRA-3557, by the rules of
// INFRA-3531). Under gVisor the host kernel sees one sandbox and not its processes: a container
// that passes its limit loses every process at once, which in an issue pod is every role's turn.
//
// The guard reads the pod's anonymous memory, AnonPages of /proc/meminfo, and the container's
// limit, the cgroup's memory.limit_in_bytes, once each interval. The cgroup's counter,
// memory.usage_in_bytes, is not the reading: it counts the pages of mapped files, so a database's
// map raises it past any mark while AnonPages stays low, and one read of it costs about 18 ms
// under gVisor where a read of meminfo costs a fraction of one. The counter is read every few
// seconds for the hourly line and at each kill, and never at each interval.
//
// At or above the act share of the limit (the mark), the guard lists the processes it may end,
// reads the smaps of those alone, and acts where their sizes sum to at least the floor share of
// the limit: it sends SIGKILL to the largest through a pidfd, then takes a reading at each
// interval. The reading has fallen when it is at least half the killed process's size below the
// reading at the kill, or below the mark. Fallen and still at or above the mark, once the process
// has exited, the guard ends the next largest. When the wait passes and the reading has not
// fallen, or when no process may be ended, the guard logs the sizes it saw and ends nothing more
// for that rise, until the reading has gone below the mark.
//
// What it may end is its caller's to say (Classifier): the launcher protects Legion's own
// processes and the agents', and INFRA-3531's controller pod protects its own. The package
// protects only its own process.
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

// Defaults are the settings a guard runs by before anyone has said otherwise, the shimwire
// defaults, which the daemon's configuration defaults are too: one change moves both.
func Defaults() Settings {
	return Settings{
		ActPercent:   shimwire.MemoryGuardActDefault,
		FloorPercent: shimwire.MemoryGuardFloorDefault,
		Wait:         shimwire.MemoryGuardWaitDefault * time.Second,
		Interval:     shimwire.MemoryGuardIntervalDefault * time.Millisecond,
	}
}

// SettingsOf is the settings an acknowledgement's member carries, and false for an
// acknowledgement that carries none, which is the guard off. It does not check the ranges
// (shimwire.LauncherMemoryGuard.Validate does, and Apply calls it).
func SettingsOf(w *shimwire.LauncherMemoryGuard) (Settings, bool) {
	if w == nil {
		return Settings{}, false
	}
	return Settings{
		ActPercent:   w.ActPercent,
		FloorPercent: w.FloorPercent,
		Wait:         time.Duration(w.WaitSeconds) * time.Second,
		Interval:     time.Duration(w.IntervalMilliseconds) * time.Millisecond,
	}, true
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

// Kill is one process the guard ended. Guard is the guard's own id and Sequence its count of
// kills from 1; Command is the process's command cut to shimwire.MemoryKillCommandBytes; SizeBytes
// is the process's size, ReadingBytes the pod's AnonPages at the kill, CounterBytes the cgroup's
// counter, ShmemBytes the pod's Shmem of /proc/meminfo (tmpfs pages and memfds, which a kill frees
// only when no other process keeps them) and LimitBytes the container's limit.
type Kill struct {
	Guard        string
	Sequence     uint64
	PID          int
	Role         string
	Command      string
	SizeBytes    uint64
	ReadingBytes uint64
	CounterBytes uint64
	ShmemBytes   uint64
	LimitBytes   uint64
	At           time.Time
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
}

// phase is where a guard is in a rise.
type phase int

const (
	// armed acts on the next reading at or above the mark.
	armed phase = iota
	// waiting has sent a kill and reads until the reading falls or the wait passes.
	waiting
	// stopped has given up on this rise and acts on nothing until the reading is below the mark.
	stopped
)

// afterKill is the kill a waiting guard watches: the process held, its size and the reading at
// the kill, which the reading must fall half that size below, and when the wait passes.
type afterKill struct {
	handle   Handle
	size     uint64
	reading  uint64
	deadline time.Time
	exited   bool
	fallen   bool
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
	pid     int
	role    string
	size    uint64
	command string
}

// largestScan is a finished scan for the largest process, and the start of the window it was
// started in, so a scan that finishes after its hour is over is not credited to the next one.
type largestScan struct {
	window time.Time
	largest
}

// counterEvery is how often the guard reads the cgroup's counter for the hourly line.
const counterEvery = 5 * time.Second

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

	mu       sync.Mutex
	settings Settings
	on       bool
	// dead is the guard turned off for good: it cannot read the pod or end a process.
	dead bool

	// The rest is Run's own.
	meminfoFile, limitFile, counterFile heldFile
	// meminfoData is the meminfo of the last reading, which a kill reads Shmem from.
	meminfoData  []byte
	smaps        []byte
	phase        phase
	wait         afterKill
	sequence     uint64
	window       window
	counterAt    time.Time
	unreadLogged map[int]bool
	// killFailures are the processes the guard could not end, for as long as each lives: they
	// count as unread from then on, so a refusal does not have the guard pick the same process at
	// every reading while the next largest grows.
	killFailures map[int]killFailure

	scans    sync.WaitGroup
	scanMu   sync.Mutex
	scanBusy bool
	scanDone *largestScan
}

// New is a guard of cfg, at the default settings and on, with an id of its own.
func New(cfg Config) (*Guard, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("mint the memory guard's id: %w", err)
	}
	proc := cmp.Or(cfg.Proc, "/proc")
	cgroup := cmp.Or(cfg.Cgroup, "/sys/fs/cgroup/memory")
	g := &Guard{
		proc:         proc,
		classify:     cfg.Classify,
		sink:         cfg.Sink,
		logf:         cfg.Logf,
		clock:        cfg.Clock,
		signal:       cfg.Signal,
		id:           hex.EncodeToString(id[:]),
		self:         os.Getpid(),
		settings:     Defaults(),
		on:           true,
		meminfoFile:  heldFile{path: filepath.Join(proc, "meminfo")},
		limitFile:    heldFile{path: filepath.Join(cgroup, "memory.limit_in_bytes")},
		counterFile:  heldFile{path: filepath.Join(cgroup, "memory.usage_in_bytes")},
		unreadLogged: map[int]bool{},
		killFailures: map[int]killFailure{},
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
	return g, nil
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
	s, _ := SettingsOf(w)
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
// good.
func (g *Guard) tick(first bool) bool {
	now := g.clock.Now()
	reading, limit, reason := g.sample()
	if reason != "" {
		g.die(reason)
		return false
	}
	s, on := g.current()
	if first {
		g.logf(startedLine, s.ActPercent, s.FloorPercent, s.Wait, s.Interval, limit, reading)
		g.window = window{start: now, end: now.Add(time.Hour)}
	}
	g.hour(now, reading, limit)
	g.decide(now, s, on, reading, limit)
	return !g.isDead()
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

// decide acts on one reading by the guard's phase in the rise.
func (g *Guard) decide(now time.Time, s Settings, on bool, reading, limit uint64) {
	mark := share(limit, s.ActPercent)
	if !on {
		g.endWait()
		if g.phase == stopped && reading < mark {
			g.phase = armed
		}
		return
	}
	switch g.phase {
	case stopped:
		if reading < mark {
			g.phase = armed
		}
		return
	case waiting:
		w := &g.wait
		w.exited = w.exited || w.handle.Exited()
		w.fallen = w.fallen || reading < mark || (w.reading >= w.size/2 && reading <= w.reading-w.size/2)
		passed := !now.Before(w.deadline)
		switch {
		case reading < mark:
			// The rise is over.
			g.endWait()
			return
		case w.fallen && (w.exited || passed):
			// The kill freed what it held and the pod is still at the mark: the next largest.
			// The guard waits for the exit, while the wait lasts, because the reading can fall
			// below the mark as the rest of the process's memory goes.
			g.endWait()
		case passed:
			g.endWait()
			g.stopRise(reasonNotFallen, reading, limit, nil)
			return
		default:
			return
		}
	case armed:
		if reading < mark {
			return
		}
	}
	g.act(now, s, reading, limit)
}

// act ends the largest process the guard may end, or gives the rise up when the processes it may
// end hold less than the floor between them: ending one of them would free too little to matter.
// A process that ended, or whose pid another process took, between the scan and the kill is not
// killed, and the next reading decides again. A process the kill fails on for any other reason is
// logged once and counted unread from then on (noteKillFailure), and the next reading ends the
// next largest.
func (g *Guard) act(now time.Time, s Settings, reading, limit uint64) {
	a, err := g.assess()
	if err != nil {
		g.die(fmt.Sprintf("cannot list the processes in %s: %v", g.proc, err))
		return
	}
	if len(a.endable) == 0 || a.endableBytes < share(limit, s.FloorPercent) {
		g.stopRise(reasonNothingToEnd, reading, limit, &a)
		return
	}
	target := a.endable[0]
	h, err := g.signal.Open(target.PID)
	if err != nil {
		g.killFailed(target.Process, err)
		return
	}
	if start, err := readStartTime(g.proc, target.PID); err != nil || start != target.StartTime {
		_ = h.Close()
		return
	}
	if err := h.Kill(); err != nil {
		_ = h.Close()
		g.killFailed(target.Process, err)
		return
	}
	at := g.clock.Now()
	counter := g.readCounter(at)
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	command := commandText(target.Process)
	g.sequence++
	g.logf(endedLine, target.PID, roleName(target.role), target.size, reading, counter, shmem, limit, cut(command, logCommandBytes))
	if g.sink != nil {
		g.sink(Kill{
			Guard: g.id, Sequence: g.sequence, PID: target.PID, Role: target.role,
			Command: cut(command, shimwire.MemoryKillCommandBytes), SizeBytes: target.size,
			ReadingBytes: reading, CounterBytes: counter, ShmemBytes: shmem, LimitBytes: limit, At: at,
		})
	}
	g.phase = waiting
	g.wait = afterKill{handle: h, size: target.size, reading: reading, deadline: now.Add(s.Wait)}
}

// killFailed acts on a kill that failed: a system with no way to hold or signal a process turns
// the guard off, a process that has ended (ESRCH) leaves the next reading to decide again, and any
// other failure is noted against the process (noteKillFailure).
func (g *Guard) killFailed(p *Process, err error) {
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		g.die(fmt.Sprintf("pidfd is not supported here: %v", err))
	case errors.Is(err, syscall.ESRCH):
	default:
		g.noteKillFailure(p, err)
	}
}

// killFailure is a process the guard could not end: its start time, which tells it from a process
// that takes its pid later, and why.
type killFailure struct {
	start uint64
	err   error
}

// noteKillFailure logs a process the kill failed on, once, and keeps it so that every later scan
// counts it unread for as long as it lives.
func (g *Guard) noteKillFailure(p *Process, err error) {
	g.killFailures[p.PID] = killFailure{start: p.StartTime, err: err}
	g.logf(killFailedLine, p.PID, err)
}

// applyKillFailures counts each process the kill failed on unread in t, and forgets each that has
// ended or whose pid another process now has.
func (g *Guard) applyKillFailures(t *Table) {
	for pid, failure := range g.killFailures {
		if p, listed := t.byPID[pid]; listed && p.StartTime == failure.start {
			t.unread[pid] = fmt.Errorf("the kill failed: %w", failure.err)
		} else {
			delete(g.killFailures, pid)
		}
	}
}

// candidate is a process the guard may end, with its size.
type candidate struct {
	*Process
	role string
	size uint64
}

// assessment is one scan as the guard acts on it: the processes it may end, largest first, and
// their sizes' sum.
type assessment struct {
	table        *Table
	endable      []candidate
	endableBytes uint64
}

// assess scans the processes, asks the classifier, and reads the smaps of each process it may
// end: one that is not protected, not the guard's own and not unread. A process whose smaps
// cannot be read is unread too, since it cannot be ranked.
func (g *Guard) assess() (assessment, error) {
	t, err := scan(g.proc)
	if err != nil {
		return assessment{}, err
	}
	g.applyKillFailures(t)
	classes := g.classes(t)
	a := assessment{table: t}
	for _, p := range t.Processes() {
		if _, unread := t.unread[p.PID]; unread || p.PID == g.self || classes[p.PID].Protected {
			continue
		}
		size, err := g.size(p.PID)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				t.unread[p.PID] = fmt.Errorf("read the smaps: %w", err)
			}
			continue
		}
		a.endable = append(a.endable, candidate{Process: p, role: classes[p.PID].Role, size: size})
		a.endableBytes += size
	}
	// The newest first among equal sizes: of a parent and the child it forked, which read the
	// same, the child is the one that was started for the work.
	slices.SortFunc(a.endable, func(x, y candidate) int {
		return cmp.Or(cmp.Compare(y.size, x.size), cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
	})
	g.logUnread(t)
	return a, nil
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
// processes the guard may end, in the protected ones, in the ones it could not read, and the rest,
// which no process holds (tmpfs pages, which no kill frees) or, negative, what the processes'
// sizes count more than once (a page a parent and its forked child share). a is the scan the
// decision was made on, or nil to scan again.
func (g *Guard) stopRise(reason string, reading, limit uint64, a *assessment) {
	g.phase = stopped
	var endable, protected, unread uint64
	if a == nil {
		if fresh, err := g.assess(); err == nil {
			a = &fresh
		}
	}
	if a != nil {
		endable = a.endableBytes
		counted := make(map[int]bool, len(a.endable))
		for _, c := range a.endable {
			counted[c.PID] = true
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
			size, err := g.size(pid)
			if err != nil {
				continue
			}
			if _, isUnread := a.table.unread[pid]; isUnread {
				unread += size
			} else {
				protected += size
			}
		}
	}
	unheld := int64(reading) - int64(endable+protected+unread)
	g.logf(stoppedLine, reason, reading, endable, protected, unread, unheld, limit)
}

// endWait lets go of the process a waiting guard holds.
func (g *Guard) endWait() {
	if g.phase != waiting {
		return
	}
	_ = g.wait.handle.Close()
	g.wait = afterKill{}
	g.phase = armed
}

// hour keeps the hourly window: it closes the window whose hour has passed with its line, takes
// this reading into the window it falls in, reads the counter every counterEvery and at each
// window's start, and starts a scan for the largest process when the reading is a new high of the
// hour at least 1% of the limit above the high the last scan started at. The scan runs on its own
// goroutine and the reading never waits for it.
func (g *Guard) hour(now time.Time, reading, limit uint64) {
	g.takeScan()
	if !now.Before(g.window.end) {
		g.closeWindow()
		start := g.window.end
		start = start.Add(now.Sub(start).Truncate(time.Hour))
		g.window = window{start: start, end: start.Add(time.Hour)}
	}
	w := &g.window
	first := !w.readings
	if first || reading > w.readingHigh {
		w.readingHigh, w.readingHighAt = reading, now
	}
	if first || reading < w.readingLow {
		w.readingLow = reading
	}
	w.readings = true
	if first || now.Sub(g.counterAt) >= counterEvery {
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
		w.counterHigh, w.counterLow, w.largest.pid, roleName(w.largest.role), w.largest.size, w.largest.command)
}

// readCounter reads the cgroup's counter into the window, and answers it: 0 when it cannot be
// read, which it is not the guard's to act on.
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
// is running.
func (g *Guard) startScan(start time.Time) bool {
	g.scanMu.Lock()
	defer g.scanMu.Unlock()
	if g.scanBusy {
		return false
	}
	g.scanBusy = true
	g.scans.Add(1)
	go g.scanLargest(start)
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
// them: the hourly line says what held the memory, whatever the guard may end.
func (g *Guard) scanLargest(start time.Time) {
	defer g.scans.Done()
	found := largestScan{window: start}
	if t, err := scan(g.proc); err == nil {
		classes := g.classes(t)
		var buf []byte
		var biggest *Process
		for _, p := range t.Processes() {
			data, err := readAll(g.smapsPath(p.PID), buf)
			buf = data
			if err != nil {
				continue
			}
			if size := smapsSize(data); biggest == nil || size > found.size {
				biggest, found.pid, found.size = p, p.PID, size
			}
		}
		if biggest != nil {
			found.role = classes[biggest.PID].Role
			found.command = cut(commandText(biggest), logCommandBytes)
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

// size is pid's size from its smaps (smapsSize), read into the guard's own buffer.
func (g *Guard) size(pid int) (uint64, error) {
	data, err := readAll(g.smapsPath(pid), g.smaps)
	g.smaps = data
	if err != nil {
		return 0, err
	}
	return smapsSize(data), nil
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
