package memoryguard

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Guest is the memory watch of a pod that runs in a GKE Sandbox microVM (INFRA-3729), which a
// launcher runs in place of the guard (Guard): the microVM's kernel is the pod's own, and its
// out-of-memory killer stops one process rather than the whole sandbox, so the launcher ends
// nothing itself. It steers that kernel, names what the kernel stopped, and logs the pod's memory,
// from what an unprivileged container can read and write.
//
// The score. At each scan, every process of a role (a non-empty Class.Role) that the classifier
// does not protect is raised to oom_score_adj 1000 where it reads lower, its start time read
// again first so that a pid another process took is not raised; so the kernel picks an external
// program a command runs, a language server or an eval kernel before any protected process. Every
// protected process, and the watch's own, that reads above the watch's own starting oom_score_adj
// (its process's, read once at start) is set back to it, which needs no privilege: an agent that
// raises its own score in its in-process shell does not stay raised. A process of no role that the
// classifier does not protect is left as it is. A write that fails is counted, logged once for
// that process (its pid with its start time) and not tried again for as long as it lives. An
// external program runs at its parent's score until the next scan, guestInterval at most.
//
// The kills. The kernel counts each process it kills in the cgroup's oom_kill (memory.events in
// v2, memory.oom_control in v1), and a killed grandchild's exit goes to its parent, not to the
// launcher. So each scan reads the counter after it reads the processes, and keeps every process
// it read, by pid and start time, with its role, whether the classifier protects it, its name and
// its resident size (stat's rss). A process is named as killed only on the evidence the kernel's
// kill leaves, read after the counter rose: the out-of-memory kill raises the counter, then sends
// SIGKILL to the victim's thread group, and that SIGKILL stays pending in the victim's status
// while its memory is freed, through its exit and while it is a zombie, until its parent reaps
// it. So while kills are pending, each scan reads the status of every process the watch knows (a
// zombie the scan has just left out among them) that has not shown the kill yet, and one whose
// status has SIGKILL pending, and whose stat, read after that status, has the start time the
// watch holds, shows the kill (attribute). It is marked, and never read again while the watch
// holds it, so a victim still freeing its memory, or a zombie, is evidence for one kill alone. The
// processes newly marked at a scan are named, in pid order, one kill each, where they are no more
// than the kills pending; where they are more, the evidence does not say which of them the kernel
// killed, and none of them is named. A process is never named because it left /proc, or because
// it ranked first before the rise: a command that exits by itself in that second leaves as well.
// The kills no process has been named for by guestKillWait after the burst's latest rise have one
// unnamed line, with the pod's memory at its first rise, and no role is told of them. The rule has
// two limits. A process that something other than the kernel has sent SIGKILL, and that is still
// in /proc while kills are pending and the kernel's own victim has already left it, is named in
// the victim's place; in a microVM pod only a role's own kill -9 or a timeout tool sends it. And a
// victim that its parent reaps before the next scan shows nothing, and its kill has the unnamed
// line. Each killed process has the guard's own endedLine, with its resident size at the scan
// before the one that showed its kill and the pod's memory at the burst's first rise, and, in a
// role, a Kill to the sink with the watch's id and its own sequence from 1, as the guard's kills
// have (writeKill), and Cause shimwire.MemoryKillCauseKernel, or
// shimwire.MemoryKillCauseKernelProtected for a process the classifier protected: one of the
// processes that run the role's session, whose end the role is told of in its own words. A process
// of no role, protected or not, has the line alone.
//
// The hour. Each hour from its start, one line gives the cgroup's usage, its working set (the usage
// less the inactive file pages, cAdvisor's figure), each with its high and when, memory.stat's
// anonymous, file, inactive file and shared memory figures, the limit, and the hour's kills,
// unnamed kills, score writes and failed score writes. The usage is read at every scan and
// memory.stat with the limit every guestStatEvery.
//
// Without a readable kill counter the watch logs one line and names nothing, and goes on with the
// score and the hour, which do not depend on it. Without a readable /proc it logs one line, raises
// and names nothing, writes each rise of the counter as one unnamed line at once, and goes on with
// the hour from the cgroup.
type Guest struct {
	// killWriter is the watch's id, its log, its sink of the kills of a role's processes
	// (roleKills) and its count of kills (writeKill).
	killWriter
	proc      string
	cgroupDir string
	classify  Classifier
	clock     Clock
	self      int
	pageSize  uint64
	// writeScore writes a process's oom_score_adj: writeScoreFile, as NewGuest sets it. The tests
	// set one that fails as a write to a process that is not dumpable does.
	writeScore func(path string, value []byte) error

	// The rest is Run's own.
	cgroup cgroupFiles
	// watching is a /proc the watch can read: it raises and names only then.
	watching bool
	// start is the watch's own process's oom_score_adj at start, and startText the bytes of it
	// that a write of it writes.
	start     int64
	startText []byte
	// scoreFailures are the processes a score write failed on, by pid with the start time that
	// tells each from a process that takes its pid later.
	scoreFailures map[int]uint64
	// countsKills is a kill counter the watch read at start, and kills its last reading.
	countsKills bool
	kills       uint64
	// known is every process the watch has read and not let go, by pid and start time: each one
	// the last scan held, and each one whose stat that scan could not read. The evidence pass runs
	// before this scan's snapshot lets go of what this scan no longer holds, so a process that has
	// just become a zombie, which the scan leaves out, is still known to it (attribute, snapshot).
	known map[procKey]*watched
	// pending is the kills of the counter's rises that no process has been named for yet, a burst
	// from the rise that found none pending, which wait until deadline, guestKillWait after the
	// latest rise; riseAt and riseMemory are the time and the pod's memory at the first rise of
	// the burst, which each of its kill lines carries, named or not.
	pending    uint64
	deadline   time.Time
	riseAt     time.Time
	riseMemory killMemory
	window     guestWindow
	// usage is the cgroup's usage at this scan, and stat and limit its memory.stat and limit at
	// their last read, taken at statAt.
	usage  figure
	stat   cgroupStat
	limit  figure
	statAt time.Time
	// small is the buffer of the watch's reads of one short file, an oom_score_adj, and status the
	// one of its reads of a process's status (showsKill).
	small  []byte
	status []byte
}

// GuestConfig is what a guest watch reads, whom it asks, and where it reports.
type GuestConfig struct {
	// Proc is the root of the processes the watch reads, "/proc" when empty. Its
	// self/oom_score_adj is the watch's starting score, and its self/cgroup and self/mountinfo
	// find the cgroup when Cgroup is empty.
	Proc string
	// Cgroup is the container's memory cgroup directory, of either version, which the files it
	// holds tell apart; empty finds the cgroup of the watch's own process (findCgroup).
	Cgroup string
	// Classify says which processes are protected and whose they are; nil protects nothing but
	// the watch's own process, and gives no process a role, so the watch raises nothing.
	Classify Classifier
	// Sink takes each kill of a role's process, on the watch's loop, so it must not block.
	Sink func(Kill)
	// Logf writes one line of the watch's; nil discards them.
	Logf func(format string, args ...any)
	// Clock is the time the watch reads and waits on; nil is the wall clock.
	Clock Clock
}

// guestInterval is the time between two scans: the guard's default interval, which bounds how long
// an external program keeps its parent's score before the watch raises it.
const guestInterval = time.Duration(shimwire.MemoryGuardIntervalDefault) * time.Millisecond

// guestStatEvery is how often the watch reads memory.stat and the limit for its hourly line.
const guestStatEvery = 5 * time.Second

// guestKillWait is how long, after the latest rise of a burst, the watch reads for the processes
// the burst killed to show the kill before it writes the kills none showed as unnamed: the kernel
// raises the counter just before it sends the SIGKILL a victim shows, and its own oom_reaper gives
// a victim two seconds to exit before it frees the victim's memory itself.
const guestKillWait = 2 * time.Second

// raisedScore is the oom_score_adj the watch raises a process to, the highest, and
// raisedScoreText the bytes a write of it writes.
const raisedScore = 1000

var raisedScoreText = []byte(strconv.Itoa(raisedScore))

// procKey is a process by its pid and its start time, which tells it from a process that takes
// its pid later.
type procKey struct {
	pid   int
	start uint64
}

// watched is one process the watch read: its role, whether the classifier protects it, its name,
// and its resident size, at the watch's last read of it, by which a kill is named and reported.
// sigkill is a process that has shown the kernel's kill (attribute), named or not: the watch reads
// it no more, and it is evidence for no later kill while the watch holds it, so a victim that is
// still freeing its memory when the counter rises again is not named twice.
type watched struct {
	key        procKey
	role       string
	protected  bool
	executable string
	resident   uint64
	sigkill    bool
}

// killMemory is the pod's memory as a kill line writes it, read at the scan that saw the rise:
// memory.stat's anonymous memory (reading), the usage (counter), memory.stat's shared memory and
// the limit (reportLimit).
type killMemory struct{ reading, counter, shmem, limit uint64 }

// guestWindow is one hour of the watch's readings and counts, from its start onward: each hour's
// line gives its own highs and counts and nothing of the hour before.
type guestWindow struct {
	start, end           time.Time
	current, currentHigh figure
	currentHighAt        time.Time
	workingSet           figure
	workingSetHigh       figure
	workingSetHighAt     time.Time
	oomKills, unnamed    uint64
	raised, unraised     uint64
}

// NewGuest is a guest watch of cfg, with an id of its own.
func NewGuest(cfg GuestConfig) *Guest {
	g := &Guest{
		killWriter:    newKillWriter(cfg.Logf, roleKills(cfg.Sink)),
		proc:          cmp.Or(cfg.Proc, "/proc"),
		cgroupDir:     cfg.Cgroup,
		classify:      cfg.Classify,
		clock:         cfg.Clock,
		self:          os.Getpid(),
		pageSize:      uint64(os.Getpagesize()),
		writeScore:    writeScoreFile,
		scoreFailures: map[int]uint64{},
		known:         map[procKey]*watched{},
		small:         make([]byte, 64),
	}
	if g.clock == nil {
		g.clock = &wallClock{}
	}
	return g
}

// roleKills is sink for the kills of a role's processes alone, or nil where sink is: a process of
// no role the kernel killed has its line and no report (Guest).
func roleKills(sink func(Kill)) func(Kill) {
	if sink == nil {
		return nil
	}
	return func(k Kill) {
		if k.Role != "" {
			sink(k)
		}
	}
}

// ID is the watch's own id, random for each watch, which every kill report carries: with the
// report's sequence it names one kill, so a report sent twice is applied once.
func (g *Guest) ID() string { return g.id }

// Run watches until ctx ends: one start line, then a scan every guestInterval.
func (g *Guest) Run(ctx context.Context) {
	defer g.cgroup.close()
	g.begin()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.clock.After(guestInterval):
		}
		g.tick(g.clock.Now())
	}
}

// begin finds the cgroup and the starting score, logs the start line and what the watch cannot
// do, and takes the first scan.
func (g *Guest) begin() {
	now := g.clock.Now()
	version, dir, cgroupErr := g.findCgroup()
	g.cgroup = newCgroupFiles(version, dir)
	scorePath := filepath.Join(g.proc, "self", "oom_score_adj")
	start, startErr := readScore(scorePath, g.small)
	score := "-"
	if startErr == nil {
		g.start, g.startText = start, strconv.AppendInt(nil, start, 10)
		score = string(g.startText)
	}
	g.logf(guestStartLine, cmp.Or(version, "-"), cmp.Or(dir, "-"), g.cgroup.oomGroup(), score)
	if cgroupErr == nil {
		g.kills, cgroupErr = g.cgroup.readKills()
	}
	if cgroupErr != nil {
		g.logf(guestKillsUnnamedLine, cgroupErr)
	} else {
		g.countsKills = true
	}
	if startErr != nil {
		g.logf(guestOffLine, fmt.Sprintf("no readable starting score in %s: %v", scorePath, startErr))
	} else {
		g.watching = true
	}
	g.window = guestWindow{start: now, end: now.Add(time.Hour)}
	g.tick(now)
}

// findCgroup is the cgroup the config names, its version by the files it holds, or the one the
// watch's own process is in.
func (g *Guest) findCgroup() (version, dir string, err error) {
	if g.cgroupDir == "" {
		return findCgroup(g.proc)
	}
	if version := cgroupVersionOf(g.cgroupDir); version != "" {
		return version, g.cgroupDir, nil
	}
	return "", g.cgroupDir, fmt.Errorf("%s holds no memory cgroup file", g.cgroupDir)
}

// tick is one scan: the hour's readings, the scores, the kill counter, while kills are pending the
// evidence pass that names the processes the kernel killed (attribute), and then the snapshot of
// the processes, which comes after that pass (snapshot).
func (g *Guest) tick(now time.Time) {
	g.hour(now)
	t, classes := g.look()
	if !g.countsKills {
		return
	}
	g.count(now)
	if t == nil {
		g.unnamed(g.pending)
		return
	}
	if g.pending > 0 {
		g.attribute(now)
	}
	g.snapshot(t, classes)
}

// look scans the processes and sets their scores, or answers nil where the watch reads no /proc: a
// scan that fails turns the watch's processes off for good, with one line.
func (g *Guest) look() (*Table, map[int]Class) {
	if !g.watching {
		return nil, nil
	}
	t, err := scan(g.proc)
	if err != nil {
		g.watching = false
		clear(g.known)
		g.logf(guestOffLine, fmt.Sprintf("cannot list the processes in %s: %v", g.proc, err))
		return nil, nil
	}
	var classes map[int]Class
	if g.classify != nil {
		classes = g.classify(t)
	}
	g.score(t, classes)
	return t, classes
}

// score raises each process of a role the classifier does not protect to raisedScore, and sets
// each protected process, and the watch's own, back to the starting score, where each reads
// otherwise (Guest).
func (g *Guest) score(t *Table, classes map[int]Class) {
	for pid, start := range g.scoreFailures {
		if p, held := t.byPID[pid]; !held || p.StartTime != start {
			delete(g.scoreFailures, pid)
		}
	}
	for pid, p := range t.byPID {
		class := classes[pid]
		protected := class.Protected || pid == g.self
		if !protected && class.Role == "" {
			continue
		}
		if _, failed := g.scoreFailures[pid]; failed {
			continue
		}
		path := filepath.Join(g.proc, strconv.Itoa(pid), "oom_score_adj")
		adj, err := readScore(path, g.small)
		if err != nil {
			continue
		}
		want, text := int64(raisedScore), raisedScoreText
		if protected {
			if adj <= g.start {
				continue
			}
			want, text = g.start, g.startText
		} else if adj >= raisedScore {
			continue
		}
		if start, err := readStartTime(g.proc, pid); err != nil || start != p.StartTime {
			continue
		}
		if err := g.writeScore(path, text); err != nil {
			g.scoreFailures[pid] = p.StartTime
			g.window.unraised++
			g.logf(guestScoreFailedLine, pid, p.StartTime, want, err)
			continue
		}
		g.window.raised++
	}
}

// count reads the kill counter. A rise that finds no kill pending starts a burst, the wait for the
// processes it killed to show the kill, as of this scan (riseAt, riseMemory); a rise while kills
// are pending adds its own to the burst, which keeps the first rise's time and memory, and moves
// the wait's end to guestKillWait after it. A read that fails sees no rise; a counter below the
// last reading, a cgroup made anew, is the next rise's base.
func (g *Guest) count(now time.Time) {
	n, err := g.cgroup.readKills()
	if err != nil || n == g.kills {
		return
	}
	if n < g.kills {
		g.kills = n
		return
	}
	rise := n - g.kills
	g.kills = n
	g.window.oomKills += rise
	if g.pending == 0 {
		g.riseAt, g.riseMemory = now, g.memoryNow()
	}
	g.pending += rise
	g.deadline = now.Add(guestKillWait)
}

// snapshot takes t into known, after the scan's evidence pass (tick), so a process is named with
// its reads from the scan before the one that showed its kill, its size before the kernel freed
// it: each process t holds that the watch had not read, with its name, and each it holds that the
// watch had and that has not shown the kill, read again: its role, whether the classifier protects
// it, and its resident size. One that has shown the kill keeps the reads it was named by. Each
// process the watch had that t no longer holds (exited, a zombie, or a pid another process took)
// is let go, but for a pid whose stat the scan could not read, since nothing says it is gone.
func (g *Guest) snapshot(t *Table, classes map[int]Class) {
	for pid, p := range t.byPID {
		key := procKey{pid: pid, start: p.StartTime}
		w := g.known[key]
		if w == nil {
			w = &watched{key: key, executable: processName(p)}
			g.known[key] = w
		} else if w.sigkill {
			continue
		}
		w.role, w.protected = classes[pid].Role, classes[pid].Protected
		w.resident = p.Resident * g.pageSize
	}
	for key := range g.known {
		p, held := t.byPID[key.pid]
		if held && p.StartTime == key.start {
			continue
		}
		if _, unread := t.unread[key.pid]; !held && unread {
			continue
		}
		delete(g.known, key)
	}
}

// attribute is the evidence pass, which runs while kills are pending: it marks each process the
// watch knows that shows the kernel's kill (showsKill) and has not shown it before. Where the
// processes it newly marks are no more than the kills pending, it names each of them, in pid order,
// for one kill; where they are more, the evidence does not say which of them the kernel killed, so
// it names none of them, and their kills stay pending. Once the wait has passed (deadline), the
// kills still pending have one unnamed line.
func (g *Guest) attribute(now time.Time) {
	var shown []*watched
	for _, w := range g.known {
		if !w.sigkill && g.showsKill(w) {
			w.sigkill = true
			shown = append(shown, w)
		}
	}
	if uint64(len(shown)) <= g.pending {
		slices.SortFunc(shown, func(a, b *watched) int { return cmp.Compare(a.key.pid, b.key.pid) })
		for _, w := range shown {
			g.report(w)
		}
		g.pending -= uint64(len(shown))
	}
	if !now.Before(g.deadline) {
		g.unnamed(g.pending)
	}
}

// showsKill is whether w shows the kernel's kill: its status has SIGKILL pending (sigkillPending),
// and its stat, read after that status, has the start time the watch holds. A pid is not reused
// while its process is in /proc, a zombie too, so that start time makes the status w's and not that
// of a process that took its pid. A read that fails shows nothing.
func (g *Guest) showsKill(w *watched) bool {
	status, err := readAll(filepath.Join(g.proc, strconv.Itoa(w.key.pid), "status"), g.status)
	g.status = status
	if err != nil || !sigkillPending(status) {
		return false
	}
	start, err := readStartTime(g.proc, w.key.pid)
	return err == nil && start == w.key.start
}

// sigkillBit is SIGKILL's bit in a status's signal masks, which hold signal n at bit n-1.
const sigkillBit = 1 << (syscall.SIGKILL - 1)

// sigkillPending is whether a /proc/<pid>/status has SIGKILL pending: its bit set in SigPnd, the
// signals pending for the thread, or in ShdPnd, those pending for its whole thread group, each a
// mask in hex. The kernel's out-of-memory kill sends SIGKILL to the thread group, so ShdPnd shows
// it while the process frees its memory, exits and is a zombie, until its parent reaps it. A mask
// that does not parse shows nothing.
func sigkillPending(status []byte) bool {
	for len(status) > 0 {
		var line []byte
		line, status, _ = bytes.Cut(status, []byte{'\n'})
		key, mask, ok := bytes.Cut(line, []byte{':'})
		if !ok || (string(key) != "SigPnd" && string(key) != "ShdPnd") {
			continue
		}
		if n, err := strconv.ParseUint(string(bytes.TrimSpace(mask)), 16, 64); err == nil && n&sigkillBit != 0 {
			return true
		}
	}
	return false
}

// report writes the guard's own line for w, which the kernel killed, and gives a role's process's
// kill to the sink, as the guard writes and reports its own (writeKill): with the kernel's cause,
// or, for a process the classifier protected, one of those that run the role's session, the
// kernel's protected cause. Every kill counts in the sequence, as the guard's do, and a process
// of no role has the line alone (roleKills).
func (g *Guest) report(w *watched) {
	m := g.riseMemory
	cause := shimwire.MemoryKillCauseKernel
	if w.protected {
		cause = shimwire.MemoryKillCauseKernelProtected
	}
	g.writeKill(Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			PID: w.key.pid, Executable: w.executable, SizeBytes: w.resident,
			ReadingBytes: m.reading, CounterBytes: m.counter, LimitBytes: m.limit, At: g.riseAt, Cause: cause,
		},
		Role: w.role,
	}, m.shmem)
}

// unnamed writes n kills no process was named for, if any, as one line with the pod's memory at
// the burst's first rise, and lets them go.
func (g *Guest) unnamed(n uint64) {
	g.pending = 0
	if n == 0 {
		return
	}
	g.window.unnamed += n
	m := g.riseMemory
	g.logf(guestKilledUnnamedLine, n, g.kills, m.reading, m.counter, m.shmem, m.limit)
}

// memoryNow is the pod's memory for the kill lines of a rise: memory.stat and the limit read
// again, and this scan's usage.
func (g *Guest) memoryNow() killMemory {
	g.stat, g.limit = g.cgroup.readStat(), g.cgroup.readLimit()
	return killMemory{reading: g.stat.anon.n, counter: g.usage.n, shmem: g.stat.shmem.n, limit: g.reportLimit()}
}

// reportLimit is the limit a kill line and its report carry: the cgroup's, or where it sets none,
// the MemTotal of the guest's meminfo, the microVM's own memory, which then bounds the pod. A
// report needs one (shimwire.LauncherMemoryKill.Validate).
func (g *Guest) reportLimit() uint64 {
	if g.limit.ok {
		return g.limit.n
	}
	data, err := os.ReadFile(filepath.Join(g.proc, "meminfo"))
	if err != nil {
		return 0
	}
	total, _ := meminfoBytes(data, "MemTotal")
	return total
}

// hour closes the hourly window whose hour has passed with its line, reads memory.stat and the
// limit every guestStatEvery, and takes this scan's usage and working set into the window.
func (g *Guest) hour(now time.Time) {
	if !now.Before(g.window.end) {
		g.closeWindow()
		start, end := nextWindow(g.window.end, now)
		g.window = guestWindow{start: start, end: end}
	}
	if g.statAt.IsZero() || now.Sub(g.statAt) >= guestStatEvery {
		g.statAt = now
		g.stat, g.limit = g.cgroup.readStat(), g.cgroup.readLimit()
	}
	g.usage = g.cgroup.readUsage()
	if !g.usage.ok {
		return
	}
	w := &g.window
	w.current = g.usage
	if !w.currentHigh.ok || g.usage.n > w.currentHigh.n {
		w.currentHigh, w.currentHighAt = g.usage, now
	}
	if !g.stat.inactiveFile.ok {
		return
	}
	w.workingSet = figure{n: g.usage.n - min(g.usage.n, g.stat.inactiveFile.n), ok: true}
	if !w.workingSetHigh.ok || w.workingSet.n > w.workingSetHigh.n {
		w.workingSetHigh, w.workingSetHighAt = w.workingSet, now
	}
}

// closeWindow logs the hourly line of the window that has passed: its last usage and working set
// and their highs, the last memory.stat and limit read, and its counts, each `-` where the watch
// cannot read it.
func (g *Guest) closeWindow() {
	w := &g.window
	g.logf(guestHourLine, stamp(w.start), stamp(w.end),
		w.current, w.currentHigh, stampOrDash(w.currentHighAt),
		w.workingSet, w.workingSetHigh, stampOrDash(w.workingSetHighAt),
		g.stat.anon, g.stat.file, g.stat.inactiveFile, g.stat.shmem, g.limit,
		figure{n: w.oomKills, ok: g.countsKills}, figure{n: w.unnamed, ok: g.countsKills},
		figure{n: w.raised, ok: g.watching}, figure{n: w.unraised, ok: g.watching})
}

// stampOrDash is t as a line writes it, `-` for no time.
func stampOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return stamp(t)
}

// readScore reads an oom_score_adj, a signed number on one line, through buf.
func readScore(path string, buf []byte) (int64, error) {
	data, err := readHead(path, buf)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s reads %q", path, data)
	}
	return n, nil
}

// writeScoreFile writes value to the oom_score_adj at path, as a shell's redirection does. A
// process that has ended has no file, which the open refuses rather than create one.
func writeScoreFile(path string, value []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(value)
	return errors.Join(err, f.Close())
}
