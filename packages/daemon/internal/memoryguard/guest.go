package memoryguard

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// launcher. So each scan keeps every process the scan read, by pid and start time, with its
// resident size (stat's rss) and its oom_score, and reads the counter after the scan. When the
// counter rises by n, the killed are the processes the watch held by the scan that saw the rise
// (the first rise's, while rises add up) that a scan no longer holds (exited, a zombie, or a pid
// another process took), ranked by their last oom_score before that rise, then their resident
// size, highest first; the first n are named (attribute). A process the watch first held at a
// later scan was not among the processes that scan read, so it is neither named nor waited for:
// a command that starts during the wait and exits by itself is no kill. A process the kernel
// killed may still run while it frees its memory, so a rise waits, up to guestKillWait, while a
// process ranked above the n-th gone one still runs; at the wait's end the n highest gone ones are
// named, and a rise beyond them has one unnamed line. Each killed process has the guard's own
// endedLine and, in a role, a Kill to the sink with the watch's id and its own sequence from 1,
// as the guard's kills have (writeKill), and Cause shimwire.MemoryKillCauseKernel, or
// shimwire.MemoryKillCauseKernelProtected for a process the classifier protected before the rise:
// one of the processes that run the role's session, whose end the role is told of in its own
// words. A process of no role, protected or not, has the line alone.
//
// The hour. Each hour from its start, one line gives the cgroup's usage, its working set (the usage
// less the inactive file pages, cAdvisor's figure), each with its high and when, memory.stat's
// anonymous, file, inactive file and shared memory figures, the limit, and the hour's kills,
// unnamed kills, score writes and failed score writes. The usage is read at every scan and
// memory.stat with the limit every guestStatEvery.
//
// Without a readable kill counter the watch logs one line and names nothing, and goes on with the
// score and the hour, which do not depend on it. Without a readable /proc it logs one line, raises
// and names nothing, and goes on with the hour from the cgroup.
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
	// the last scan held, and each one gone since, until no rise waits (snapshot, attribute).
	known map[procKey]*watched
	// scans counts the scans that read the processes, by which snapshot marks what a scan held
	// and the scan that first held each process.
	scans uint64
	// pending is the kills of the counter's rises that no process has been named for yet, a burst
	// from the rise that found none pending, which wait until deadline, guestKillWait after the
	// latest rise; riseAt, riseMemory and riseScan are the time, the pod's memory and the scan at
	// the first rise of the burst, after whose scan a process first held is taken for none of
	// them (attribute).
	pending    uint64
	deadline   time.Time
	riseAt     time.Time
	riseMemory killMemory
	riseScan   uint64
	window     guestWindow
	// usage is the cgroup's usage at this scan, and stat and limit its memory.stat and limit at
	// their last read, taken at statAt.
	usage  figure
	stat   cgroupStat
	limit  figure
	statAt time.Time
	// small is the buffer of the watch's reads of one short file: an oom_score, an oom_score_adj.
	small []byte
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

// guestKillWait is the longest a rise of the kill counter waits for a process ranked above the
// ones gone to leave the scan, since the process the kernel killed runs until it has freed its
// memory: the kernel's own oom_reaper gives a victim two seconds to exit before it frees the
// victim's memory itself.
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

// watched is one process the watch read: its role, whether the classifier protects it, and its
// name, and its resident size and oom_score, each at its last read before any pending rise, by
// which a kill is ranked, named and reported. first is the scan that first held it, seen the last
// scan that held it, and gone a process a later scan no longer held.
type watched struct {
	key        procKey
	role       string
	protected  bool
	executable string
	resident   uint64
	score      int64
	first      uint64
	seen       uint64
	gone       bool
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

// tick is one scan: the hour's readings, the scores, the kill counter, and the processes it
// names.
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
	g.snapshot(t, classes)
	if g.pending > 0 {
		g.attribute(now)
		if g.pending == 0 {
			maps.DeleteFunc(g.known, func(_ procKey, w *watched) bool { return w.gone })
		}
	}
}

// look scans the processes, counts the scan, and sets their scores, or answers nil where the watch
// reads no /proc: a scan that fails turns the watch's processes off for good, with one line.
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
	g.scans++
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

// count reads the kill counter. A rise that finds no kill pending starts a burst, the wait for
// the processes it killed, as of this scan (riseAt, riseMemory, riseScan); a rise while kills are
// pending adds its own to the burst, which keeps the first rise's scan, so that a process first
// held after it is named for none of them, and moves the wait's end to guestKillWait after it. A
// read that fails sees no rise; a counter below the last reading, a cgroup made anew, is the next
// rise's base.
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
		g.riseAt, g.riseMemory, g.riseScan = now, g.memoryNow(), g.scans
	}
	g.pending += rise
	g.deadline = now.Add(guestKillWait)
}

// snapshot takes t into known: each process it holds that the watch had not read, with the scan
// that first held it, its role, whether the classifier protects it, its size and its score, and
// each it holds that the watch had, read again unless a rise waits, whose ranking is by the reads
// before it. Each the watch had that t does not hold is let go where no rise waits, and marked
// gone while one does, for attribute to name. A pid whose stat the scan could not read is not
// gone, since nothing says it is.
func (g *Guest) snapshot(t *Table, classes map[int]Class) {
	for pid, p := range t.byPID {
		key := procKey{pid: pid, start: p.StartTime}
		w := g.known[key]
		fresh := w == nil
		if fresh {
			w = &watched{key: key, executable: processName(p), first: g.scans}
			g.known[key] = w
		}
		if fresh || g.pending == 0 {
			w.role, w.protected = classes[pid].Role, classes[pid].Protected
			w.resident = p.Resident * g.pageSize
			g.read(w)
		}
		w.seen = g.scans
	}
	for _, w := range g.known {
		if w.seen == g.scans || w.gone {
			continue
		}
		if _, held := t.byPID[w.key.pid]; !held {
			if _, unread := t.unread[w.key.pid]; unread {
				w.seen = g.scans
				continue
			}
		}
		if g.pending == 0 {
			delete(g.known, w.key)
			continue
		}
		w.gone = true
	}
}

// read reads w's oom_score, keeping the last that does not read.
func (g *Guest) read(w *watched) {
	if score, err := readScore(filepath.Join(g.proc, strconv.Itoa(w.key.pid), "oom_score"), g.small); err == nil {
		w.score = score
	}
}

// attribute names the processes the pending kills killed: walking every process the watch knows,
// highest last oom_score first, then largest resident size, it takes each gone one until it has
// one for each kill. A process that still runs, met before that, may be one the kernel killed
// that has not exited yet, so the walk waits for the next scan, until the wait passes; then it
// passes over each one that runs. A process first held after the scan that saw the first rise of
// the burst (riseScan) is passed over whether it runs or not: it was not among the processes that
// scan read, and every kill of the burst is ranked by the reads before that rise (snapshot), so a
// command that started during the wait, after any rise of the burst, is not named for a kill, and
// does not hold the walk. The kills left over have one unnamed line.
func (g *Guest) attribute(now time.Time) {
	final := !now.Before(g.deadline)
	ranked := slices.SortedFunc(maps.Values(g.known), func(a, b *watched) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.resident, a.resident),
			cmp.Compare(b.key.start, a.key.start), cmp.Compare(b.key.pid, a.key.pid))
	})
	var killed []*watched
	for _, w := range ranked {
		if uint64(len(killed)) == g.pending {
			break
		}
		if w.first > g.riseScan {
			continue
		}
		if w.gone {
			killed = append(killed, w)
			continue
		}
		if !final {
			return
		}
	}
	for _, w := range killed {
		g.report(w)
		delete(g.known, w.key)
	}
	g.unnamed(g.pending - uint64(len(killed)))
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

// unnamed writes n kills no process was named for, if any, as one line, and lets them go.
func (g *Guest) unnamed(n uint64) {
	g.pending = 0
	if n == 0 {
		return
	}
	g.window.unnamed += n
	g.logf(guestKilledUnnamedLine, n, g.kills)
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

// readScore reads an oom_score or oom_score_adj, a signed number on one line, through buf.
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
