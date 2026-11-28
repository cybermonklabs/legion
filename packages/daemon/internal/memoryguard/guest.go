package memoryguard

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// The kills. The kernel counts each process it kills in the cgroup's oom_kill (memory.events in v2,
// memory.oom_control in v1), and a killed grandchild's exit goes to its parent, not to the
// launcher. So each scan reads the counter after it reads the processes, and keeps every process it
// read, by pid and start time, with what the classifier said of it (Class), its name and its
// resident size (stat's rss). The watch reads the Class and the size again at each scan until a
// pass has read SIGKILL pending in the process (snapshot), so a victim the watch already knew is
// reported with its reads from the last scan whose snapshot came before the pass that showed its
// kill, its size before the kernel freed it. A process a scan reads first has its status read then,
// and one that already shows SIGKILL pending is never named. It counts against the kills pending of
// the burst it was first read in alone, whatever its role, from the pass after that scan on, until
// the first scan after it has left /proc, and for no burst where none was pending then (snapshot).
// What that first read costs is in the rule's one limit (below). A process is named as killed only
// on the evidence the kernel's kill leaves, read after the counter rose: the out-of-memory kill
// raises the counter, then sends SIGKILL to the victim's thread group, and that SIGKILL stays
// pending in the victim's ShdPnd, the signals pending for its whole thread group, while its memory
// is freed, through its exit and while it is a zombie, until its parent reaps it. SigPnd, a
// thread's own, is no evidence: the kernel adds SIGKILL to it in each thread of a process that any
// fatal signal is ending, such as the SIGTERM of role-init's drain (sigkillPending).
//
// So while kills are pending, each evidence pass (attribute) reads the status of every process the
// watch holds (a zombie among them, which the scan leaves out) that has not shown the kill, and one
// whose ShdPnd has SIGKILL, and whose stat, read after that status, has the start time the watch
// holds, has shown it. Its status is never read again, and it is a candidate of every pass until
// one settles it, by naming it or setting it aside; a settled process is evidence for no kill while
// the watch holds it, so a victim still freeing its memory, or a zombie, is evidence for one kill
// alone. The rule of every pass is one: it names a process only on the evidence that process shows,
// so a delay may leave a kill unnamed, and it names a wrong process only within the rule's one
// limit (below). Each pass first sets aside the candidates of a role whose session has ended
// (below), and names the rest, in pid order, one kill each, where they, counted with each process
// of the burst that shows SIGKILL and that no pass may name (counted), are no more than the kills
// pending. Where they are more, it reads the counter again. Where that read rose, the kernel has
// killed again since the last read: the pass names and settles none of the rest, and leaves them,
// with every read of them, to the next pass, which judges them against the larger count. That is
// the one case where a pass carries, and it carries after it has set aside the candidates of a role
// whose session has ended, which the next pass would set aside as well: a role's session that has
// ended stays ended for the burst (sessionEnded). Where that read did not rise, the evidence does
// not say which of them the kernel killed, and the pass sets the rest all aside, to count until the
// burst ends, or each until the first scan after it has left /proc. A pass that does not carry so
// decides every candidate, and a burst never ends with one left. A burst ends once no kill of it is
// pending. The end of its wait (unnamed) ends it and settles each process it counted (endBurst), so
// a later burst neither names nor counts it. A pass that names every kill pending ends it too, and
// has nothing to settle: it names only where its candidates and the counted processes are no more
// than the kills pending, so it held no counted process. A process is never named because it left
// /proc, or because it ranked first before the rise: a command that exits by itself in that second
// leaves as well.
//
// A role's session ends with a SIGKILL its role-init sends: once the role's shim has exited,
// role-init sends SIGTERM over the role's namespace, waits up to its drain for the role's processes
// to go, and sends SIGKILL to each one left (kill(-1)), which shows in ShdPnd as the kernel's does;
// on the stop path it sends that kill(-1) as soon as it has reaped the shim. So the watch keeps,
// for each role, the time it last saw a session process of that role (Class.Session) leave /proc
// (leftAt): its stat gone or back with another start time (gone), where a zombie has not left.
// Every such SIGKILL comes after a session process of its role has left, so each pass reads the
// session processes it holds once, after its status reads, and records each departure that read
// sees before it judges any candidate: a SIGKILL of role-init's that a status read saw came after
// its role's departure, which that later read sees, and the departure ends the role's session at
// that same pass. Each such SIGKILL reaches the role's own processes alone, so the pass judges
// every other role's candidates as ever. A scan lets go of a session process it no longer holds
// only once it has left, and records the departure then; it keeps a zombie. A role whose session
// process left no earlier than GuestConfig.SessionDrain before the burst's first rise, or during
// its wait, names nothing for the rest of the burst (sessionEnded): each of its candidates is set
// aside, and counts against no kill, so that the set-aside does not block another role's real
// victim. The kills stay pending, to end unnamed, unless a pass names another process in the
// victim's place within the rule's one limit (below). A process of that role that the watch counts
// already is not set aside, and goes on counting, within the same limit. In an issue pod the
// launcher sets SessionDrain to 3 s, role-init's 2 s drain and a second more for its kill to land.
// The controller's pod has no role-init: once its shim has exited, the launcher, PID 1, drains the
// exited generation itself, with SIGTERM over the container's processes, up to its stop grace for
// them to go, and SIGKILL to each one left. That drain's SIGKILL is the session's end there, read
// as role-init's is above, so there the launcher sets SessionDrain to its stop grace and a second
// more (controllerSessionDrain, packages/daemon/internal/launcher/memoryguard.go). The launcher's
// classifier counts every direct child of the shim as a session process, by its place in the tree,
// the agent and the shim's other children (the secrets renewer, the working copy's adoption, the
// CodeGraph warm-up) alike, which costs kills left unnamed, and a wrong name only within the rule's
// one limit (below). A session process that shows the kill before it leaves, as an agent the kernel
// killed does while it is a zombie its shim has not reaped, is named as any process is, unless the
// watch sees a session process of its role leave, itself among them, before the pass that settles
// it has read the session processes after its status reads: that departure sets it aside, as above.
//
// The kernel shows that evidence only while it reaps the victim, 51 to 85 ms for a 1.5 GiB
// process its parent reaps at once, and a scan every guestInterval alone named such a victim 3
// times of 5. So on cgroup v2 the watch also wakes on the file-modified event of memory.events,
// which the kernel raises whenever one of its counters changes (wake): it reads the counter at
// once, and while kills are pending it runs the evidence pass at once over the processes the last
// scan left it, and again after each pause of wakeRereads while kills stay pending, since the
// kernel raises the counter just before it sends the SIGKILL. It runs those passes once for each
// rise of the counter: at the wake whose read rose, or at the first wake after a scan that read
// the rise, since the rise's own event can come just after that scan's read. Any other wake while
// kills are pending reads the counter alone, so a stream of max and high events from a pod under
// pressure does not read statuses for the whole wait, and the scans go on reading. The scan stays
// the fallback, and it is the only pass where the cgroup is v1, whose memory.oom_control raises no
// such event, or where the watch of the event cannot start or ends, each of which has one line.
// The scan and the wake run the same pass, and neither depends on a pass having completed. The
// kills no process has been named for by guestKillWait after the burst's latest rise have one
// unnamed line, with the pod's memory at its first rise, and no role is told of them.
//
// The rule has one documented limit, where a pass can name a process in a kernel victim's place
// or leave the victim's kill unnamed. docs/kubernetes.md ("What the sign does not show") is its
// one statement: its class, its known cases and the SIGKILL senders of a microVM pod. This
// package's comments, and the launcher's, refer to that statement and do not repeat it.
//
// Each killed process has the guard's own endedLine, with its resident size at the last scan whose
// snapshot came before the pass that showed its kill and the pod's memory at the burst's first
// rise, the kernel as its rule and its sizes (ruleKernel, sizesKernel: the kernel chose it, by
// sizes of its own), no parent's size taken off (less_parent=0) and no read at the mark
// (new_read=-), and a Kill to the sink with its role, "" for a process of no role, the watch's id
// and its own sequence from 1, as the guard's kills have (writeKill), and Cause
// shimwire.MemoryKillCauseKernel, or shimwire.MemoryKillCauseKernelProtected for a process the
// classifier protected: one of the processes that run the role's session, whose end the role is
// told of in its own words.
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
	// killWriter is the watch's id, its log, its sink of the kills (GuestConfig.Sink) and its
	// count of kills (writeKill).
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
	// watchModified starts the watch of a file's file-modified event that the watch wakes on
	// (wake): watchModified, as NewGuest sets it. The tests set one that never fires, since they
	// write memory.events in place between two scans, and a test of the wake sets one it fires
	// itself, or this one back.
	watchModified func(path string) (*fileWatch, error)
	// pause waits between two evidence passes of a wake (wakeRereads): time.Sleep, as NewGuest
	// sets it. A test of the wake sets one that records each pause.
	pause func(time.Duration)

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
	// wakes is the watch of the file-modified event of the counter's memory.events, which wakes
	// the watch (wake), nil where the watch has none (stopWake).
	wakes *fileWatch
	// rereadsRun is a wake that has run its evidence passes (wakeRereads) since the counter's last
	// rise (count), so every other wake until the next rise reads the counter alone.
	rereadsRun bool
	// known is every process the watch has read and not let go, by pid and start time: each one
	// the last scan held, each one whose stat that scan could not read, each candidate of the
	// evidence passes, each session process that has not left /proc, and each counted process
	// (counted) that has not left it, until its burst ends (endBurst). The evidence pass runs
	// before this scan's snapshot lets go of what this scan no longer holds, so a process that has
	// just become a zombie, which the scan leaves out, is still known to it (attribute, snapshot).
	known map[procKey]*watched
	// sessionDrain is GuestConfig.SessionDrain, and sessionLeft the time the watch last saw a
	// session process of each role leave /proc (leftAt), by which a burst names nothing of a role
	// whose session has ended (sessionEnded).
	sessionDrain time.Duration
	sessionLeft  map[string]time.Time
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
	// Sink takes each kill, on the watch's loop, so it must not block, as the guard's Sink takes
	// its own: a kill of a process of no role has an empty Role. An issue pod's launcher's sink
	// hands a role's kill to that role and one of no role to the pod's queue of reports of no role
	// (serveMemoryGuard and noRoleReports, packages/daemon/internal/launcher/memoryguard.go), which
	// the daemon keeps and tells no role of; the controller pod's classifier gives every process to
	// the controller, whose sink reports each kill.
	Sink func(Kill)
	// Logf writes one line of the watch's; nil discards them.
	Logf func(format string, args ...any)
	// Clock is the time the watch reads and waits on; nil is the wall clock.
	Clock Clock
	// SessionDrain is how long after the watch sees a session process of a role (Class.Session)
	// leave /proc a SIGKILL that the role's processes show can still be its role-init's: once the
	// role's shim has exited, role-init sends SIGTERM over the role's namespace, waits up to its
	// drain for the role's processes to go, and then sends SIGKILL to each one left. A burst whose
	// first rise comes no later than SessionDrain after such a departure, or that sees one during
	// its wait, names no process of that role (Guest). In an issue pod the launcher sets it to
	// role-init's drain and a margin for its kill to land, and in the controller's pod, which has no
	// role-init, to its own drain of the exited generation, its stop grace, and the same margin
	// (controllerSessionDrain); zero takes in a departure during the wait alone.
	SessionDrain time.Duration
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

// watched is one process the watch read: what the classifier said of it (its role, whether it is
// protected, whether it runs its role's session), its name, and its resident size, at the watch's
// last read of it (snapshot), by which a kill is named and reported, and what the evidence passes
// (attribute), and a scan's first read (snapshot), have made of it.
type watched struct {
	key        procKey
	class      Class
	executable string
	resident   uint64
	evidence   evidence
}

// evidence is what the evidence passes (attribute), and a scan's first read (snapshot), have made
// of a process.
type evidence uint8

const (
	// unshown is a process no pass has read SIGKILL pending in: each pass reads its status.
	unshown evidence = iota
	// shown is a process a pass read SIGKILL pending in, whose status no pass reads again: a
	// candidate of every pass until one settles it, which the snapshot keeps with its reads.
	shown
	// settled is a shown process a pass named, or set aside because its role's session ended; a
	// counted one whose burst has ended (endBurst); and one a scan's snapshot first read with
	// SIGKILL already pending while no kill was pending (snapshot). It is evidence for no later
	// kill, and counts for none, while the watch holds it, so a victim that is still freeing its
	// memory when the counter rises again is not named twice, and the snapshot lets it go as any
	// process once the scan no longer holds it.
	settled
	// counted is a process that shows SIGKILL pending and that no pass may name: one a scan's
	// snapshot first read with SIGKILL already pending while kills were pending (snapshot), and
	// one a pass set aside because more processes showed the kill than kills were pending
	// (attribute). Its status no pass reads again, and it is counted with the candidates of every
	// pass against the kills pending until its burst ends (endBurst), or until the first scan after
	// it has left /proc, whose snapshot lets it go (a zombie has not left). So while it counts, and
	// the counter does not rise again in the wait, a held process that another sender SIGKILLs in
	// that wait meets a recount rather than a name. The burst's end settles it, so it counts at no
	// later burst.
	counted
)

// fileWatch is a watch of a file's file-modified event (watchModified). modified takes a value for
// each read of the events the kernel queued and closes when the watch ends; err, set before that
// close and so read once it is seen, says why the watch ended. stop ends it.
type fileWatch struct {
	modified <-chan struct{}
	stop     func()
	err      error
}

// errWatchIgnored is why a fileWatch ends once the kernel drops the watch (IN_IGNORED).
var errWatchIgnored = errors.New("the kernel dropped the watch (IN_IGNORED), as it does once the file goes away")

// killMemory is the pod's memory as a kill line writes it, read at the scan or the wake that saw
// the rise: memory.stat's anonymous memory (reading), the usage (counter), memory.stat's shared
// memory and the limit (reportLimit).
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
		killWriter:    newKillWriter(cfg.Logf, cfg.Sink),
		proc:          cmp.Or(cfg.Proc, "/proc"),
		cgroupDir:     cfg.Cgroup,
		classify:      cfg.Classify,
		clock:         cfg.Clock,
		self:          os.Getpid(),
		pageSize:      uint64(os.Getpagesize()),
		writeScore:    writeScoreFile,
		watchModified: watchModified,
		pause:         time.Sleep,
		scoreFailures: map[int]uint64{},
		known:         map[procKey]*watched{},
		sessionDrain:  cfg.SessionDrain,
		sessionLeft:   map[string]time.Time{},
		small:         make([]byte, 64),
	}
	if g.clock == nil {
		g.clock = &wallClock{}
	}
	return g
}

// ID is the watch's own id, random for each watch, which every kill report carries: with the
// report's sequence it names one kill, so a report sent twice is applied once.
func (g *Guest) ID() string { return g.id }

// Run watches until ctx ends: one start line, then a scan every guestInterval, and between two
// scans a wake at each file-modified event of memory.events (wake), on this one goroutine, so the
// wake and the scan share the watch's state with no lock. A wake does not move the next scan, which
// comes guestInterval after the last one however many events come between. Where the event's watch
// ends, the watch has one line, with the error that ended it, and goes on with the scan alone. A
// step that panics turns the watch off for good with one line and stops its wake (step), and Run
// waits for ctx.
func (g *Guest) Run(ctx context.Context) {
	defer g.cgroup.close()
	defer g.stopWake()
	if !g.step(g.begin) {
		<-ctx.Done()
		return
	}
	scan := g.clock.After(guestInterval)
	for {
		var modified <-chan struct{}
		if g.wakes != nil {
			modified = g.wakes.modified
		}
		on := true
		select {
		case <-ctx.Done():
			return
		case _, open := <-modified:
			if !open {
				ended := g.wakes.err
				g.stopWake()
				g.logf(guestWakeOffLine, fmt.Sprintf("the watch of %s ended: %v", g.cgroup.kills.path, ended))
				continue
			}
			on = g.step(func() { g.wake(g.clock.Now()) })
		case <-scan:
			if on = g.step(func() { g.tick(g.clock.Now()) }); on {
				scan = g.clock.After(guestInterval)
			}
		}
		if !on {
			<-ctx.Done()
			return
		}
	}
}

// step runs one step of the watch, its begin, a wake or a scan, and is false where the step
// panicked: the watch then logs one off line, with the panic and where it was raised, stops its
// wake at once (stopWake), and reads nothing more, since the launcher runs it on a goroutine of
// its own and a panic that left it would end the launcher, PID 1 of the pod, which ends every
// role's turn. A panic in begin's first scan comes after begin has started the wake (watchKills),
// whose inotify descriptor and reader would otherwise run until ctx ends.
func (g *Guest) step(run func()) (on bool) {
	defer func() {
		if p := recover(); p != nil {
			g.logf(guestOffLine, panicReason("panic", p))
			g.stopWake()
			on = false
		}
	}()
	run()
	return true
}

// begin finds the cgroup and the starting score, logs the start line and what the watch cannot
// do, starts the wake where it reads a kill counter (watchKills), and takes the first scan.
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
		g.watchKills()
	}
	if startErr != nil {
		g.logf(guestOffLine, fmt.Sprintf("no readable starting score in %s: %v", scorePath, startErr))
	} else {
		g.watching = true
	}
	g.window = guestWindow{start: now, end: now.Add(time.Hour)}
	g.tick(now)
}

// watchKills starts the watch of the file-modified event of v2's memory.events, which wakes the
// watch (wake), or logs why there is none: v1's memory.oom_control raises no such event, and a
// watch that cannot start says why. Without it the scan alone reads the counter.
func (g *Guest) watchKills() {
	if g.cgroup.version != cgroupV2 {
		g.logf(guestWakeOffLine, "cgroup v1's memory.oom_control raises no file-modified event")
		return
	}
	wakes, err := g.watchModified(g.cgroup.kills.path)
	if err != nil {
		g.logf(guestWakeOffLine, err)
		return
	}
	g.wakes = wakes
}

// stopWake ends the watch of the file-modified event, if one runs, and the wakes with it.
func (g *Guest) stopWake() {
	if g.wakes != nil {
		g.wakes.stop()
	}
	g.wakes = nil
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
// the processes, which comes after that pass and keeps every candidate it leaves, with every read
// of it, so the next pass judges what this one carried (snapshot).
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
	g.snapshot(now, t, classes)
}

// wakeRereads are the pauses of a wake before each of its evidence passes after the first, about
// 31 ms in all: the kernel raises the counter just before it sends the SIGKILL a victim shows
// (__oom_kill_process), so the pass at the event's instant can read the victim before the signal.
var wakeRereads = [...]time.Duration{
	1 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond,
}

// wake is the kill pass at a file-modified event of memory.events (Run). It reads the counter
// alone (count): most such events are another of the file's counters changing, max or high, and
// they cost that one read. It scans no process and takes no snapshot. Where kills are then
// pending and no wake has run its passes since the counter's last rise (rereadsRun), it runs the
// evidence pass at once over the processes the last scan left known (attribute), and again after
// each pause of wakeRereads while kills stay pending; where the watch reads no /proc, it writes
// them unnamed at once, as a scan does. So a rise has those passes once, at the wake that reads it
// or at the first wake after the scan that did, whose event can come just after the scan's read,
// and the events of a pod under pressure that raise no counter read no status. The pass writes the
// kills no process showed unnamed only once the wait has passed, so a wake writes none before then.
func (g *Guest) wake(now time.Time) {
	g.count(now)
	if g.pending == 0 || g.rereadsRun {
		return
	}
	g.rereadsRun = true
	if !g.watching {
		g.unnamed(g.pending)
		return
	}
	g.attribute(now)
	for _, d := range wakeRereads {
		if g.pending == 0 {
			return
		}
		g.pause(d)
		g.attribute(now)
	}
}

// look scans the processes and sets their scores, or answers nil where the watch reads no /proc: a
// scan that fails turns the watch's processes off for good, with one line.
func (g *Guest) look() (*Table, map[int]Class) {
	if !g.watching {
		return nil, nil
	}
	t, err := Scan(g.proc)
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
		if start, err := ReadStartTime(g.proc, pid); err != nil || start != p.StartTime {
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
// processes it killed to show the kill, as of this read (riseAt, riseMemory); a rise while kills
// are pending adds its own to the burst, which keeps the first rise's time and memory, and moves
// the wait's end to guestKillWait after it. Each rise leaves the next wake its evidence passes
// (rereadsRun). A read that fails sees no rise; a counter below the last reading, a cgroup made
// anew, is the next rise's base.
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
	g.rereadsRun = false
}

// snapshot takes t into known, after the scan's evidence pass (tick). Each process t holds that the
// watch had not read is read: its name, its Class and its resident size, and its status (its first
// read, below). Each it holds that the watch had is read again, its Class and its size, until a
// pass has read SIGKILL pending in it, so a process is named with its reads from the last scan
// whose snapshot came before the pass that showed its kill, and one that a pass has settled keeps
// the reads it was settled by. That is its size before the kernel freed it: the kernel raises the
// counter before it sends that SIGKILL and frees the memory only after it, and the scan reads t
// before the counter (count) and the pass reads the statuses after both, so a process the watch
// already knew that t holds as its memory is freed has shown its kill at this scan's pass, or has
// been reaped and is named for no kill. The pass reads only the processes the watch knew, so a
// process that started after the last scan and that the kernel killed before this one is first read
// here, as its memory is freed. So a process whose first status read shows SIGKILL pending
// (showsKill) is never named, at that size or for any kill: that SIGKILL is the evidence of a kill
// counted already, of one whose rise comes after this scan's count, since the kernel can raise the
// counter and send the SIGKILL between the count and this read, or of none, another sender's. Where
// kills are pending it is counted: from the pass after this scan on, every pass of the burst counts
// it with its candidates against the kills pending (attribute), until the first scan after it has
// left /proc, whose snapshot lets it go, or until the burst ends (endBurst), which settles it, so
// it counts at no later burst. So where one kill is pending and another process shows SIGKILL
// beside it, the pass reads the counter again and, where that read did not rise, sets that process
// aside, and the kill ends on the unnamed line. Where no kill is pending it is settled at once, and
// counts for no burst. It is counted or settled whatever its role. What this first read costs is in
// the rule's one limit (Guest). The status is read at every first read, whether or not kills are
// pending, one read for each new process, so that such a process is never named: one taken in
// unshown would be a candidate of the next rise's pass. Each process the watch had that t no longer
// holds (exited, a zombie, or a pid another process took) is let go, but for a pid whose stat the
// scan could not read, since nothing says it is gone; for a candidate of the evidence passes
// (shown), which a pass that carried leaves to the next, a zombie among them; and for a session
// process, or a counted one until its burst ends, that has not left /proc (gone), a zombie among
// them, since a zombie has not left. A settled process is let go as any other, so a zombie a burst
// counted is let go once that burst has ended, unless it is a session process. A session process
// let go has left /proc by now (leftAt).
func (g *Guest) snapshot(now time.Time, t *Table, classes map[int]Class) {
	for pid, p := range t.byPID {
		key := procKey{pid: pid, start: p.StartTime}
		w := g.known[key]
		if w == nil {
			w = &watched{key: key, executable: processName(p)}
			if g.showsKill(w) {
				w.evidence = settled
				if g.pending > 0 {
					w.evidence = counted
				}
			}
			g.known[key] = w
		} else if w.evidence != unshown {
			continue
		}
		w.class = classes[pid]
		w.resident = p.Resident * g.pageSize
	}
	for key, w := range g.known {
		p, held := t.byPID[key.pid]
		if held && p.StartTime == key.start {
			continue
		}
		if _, unread := t.unread[key.pid]; !held && unread {
			continue
		}
		if w.evidence == shown {
			continue
		}
		if (w.session() || w.evidence == counted) && !g.gone(w) {
			continue
		}
		if w.session() {
			g.leftAt(w, now)
		}
		delete(g.known, key)
	}
}

// attribute is the evidence pass, which runs while kills are pending, at a scan and at a wake
// alike. It reads the status of each process the watch knows that has not shown SIGKILL pending,
// and marks each that shows it now (showsKill) as shown; then it reads, once, whether each session
// process it holds, of a role whose session has not ended, has left /proc (departed). That one
// read after the status reads is enough: a role-init's SIGKILL that a status read saw came after a
// departure of its role, which that read sees, and since now is no earlier than the burst's first
// rise, it ends the role's session at this pass (sessionEnded). The candidates are then every
// shown process no pass has settled, this pass's and those a pass before it carried. The pass sets
// aside those of a role whose session has ended, since their SIGKILL can be the role-init's, which
// reaches the role's own processes alone. It counts the rest with each counted process of the
// burst, of any role, which it never names. Where they are no more than the kills pending, it
// names each of the rest, in pid order, for one kill. Where they are more, it reads the counter
// again (count): where it rose, the kernel has killed again since the last read, and the pass
// names and settles none of them and leaves them to the next pass, which judges them against the
// larger count, the one case where a pass carries; where it did not, the evidence does not say
// which of them the kernel killed, so it sets the rest all aside as counted, and their kills stay
// pending: they go on counting until the burst ends, or each until the first scan after it has left
// /proc, so while they count, and the counter does not rise again in the wait, a process that
// another sender SIGKILLs later in the wait meets a recount rather than a name. So every pass that
// does not carry decides every candidate. A pass that names every kill pending had no counted
// process, since it names only where its candidates and the counted processes are no more than the
// kills pending, so there is nothing to settle. Once the wait has passed (deadline), the kills
// still pending have one unnamed line, which ends the burst and settles each counted process
// (endBurst).
func (g *Guest) attribute(now time.Time) {
	for _, w := range g.known {
		if w.evidence == unshown && g.showsKill(w) {
			w.evidence = shown
		}
	}
	g.departed(now)
	var candidates []*watched
	// neverNamed is the number of counted processes, the burst's own, which count against the
	// kills pending and which no pass names.
	var neverNamed uint64
	for _, w := range g.known {
		if w.evidence == counted {
			neverNamed++
			continue
		}
		if w.evidence != shown {
			continue
		}
		if g.sessionEnded(w.class.Role) {
			w.evidence = settled
			continue
		}
		candidates = append(candidates, w)
	}
	if neverNamed+uint64(len(candidates)) > g.pending {
		before := g.pending
		g.count(now)
		if g.pending > before {
			return
		}
		for _, w := range candidates {
			w.evidence = counted
		}
	} else {
		slices.SortFunc(candidates, func(a, b *watched) int { return cmp.Compare(a.key.pid, b.key.pid) })
		for _, w := range candidates {
			w.evidence = settled
			g.report(w)
		}
		g.pending -= uint64(len(candidates))
	}
	if !now.Before(g.deadline) {
		g.unnamed(g.pending)
	}
}

// departed reads whether each session process the watch holds, of a role whose session has not
// ended for the burst (sessionEnded), has left /proc (gone), and records each one that has
// (leftAt).
func (g *Guest) departed(now time.Time) {
	for _, w := range g.known {
		if w.session() && !g.sessionEnded(w.class.Role) && g.gone(w) {
			g.leftAt(w, now)
		}
	}
}

// session is whether w runs its role's session (Class.Session): no process of no role does.
func (w *watched) session() bool { return w.class.Session && w.class.Role != "" }

// leftAt records that w, which the watch held, had left /proc by at (sessionLeft): the latest such
// time of each role is the one kept. Its caller has checked that w is a session process (session).
func (g *Guest) leftAt(w *watched, at time.Time) { g.sessionLeft[w.class.Role] = at }

// sessionEnded is whether role's session has ended for the burst: the watch saw a session process
// of it leave /proc no earlier than sessionDrain before the burst's first rise (riseAt), which
// takes in each departure during its wait, so a SIGKILL its processes show can be its role-init's
// (GuestConfig.SessionDrain). No process of no role is a session process.
func (g *Guest) sessionEnded(role string) bool {
	left, ok := g.sessionLeft[role]
	return ok && !left.Before(g.riseAt.Add(-g.sessionDrain))
}

// gone is whether w has left /proc: its stat is gone, or it names a process of another start time,
// which took w's pid. A zombie has not left: its stat stays, with its start time, until its parent
// reaps it. A read that fails otherwise does not say that w left.
func (g *Guest) gone(w *watched) bool {
	start, err := ReadStartTime(g.proc, w.key.pid)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	return start != w.key.start
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
	start, err := ReadStartTime(g.proc, w.key.pid)
	return err == nil && start == w.key.start
}

// sigkillBit is SIGKILL's bit in a status's signal masks, which hold signal n at bit n-1.
const sigkillBit = 1 << (syscall.SIGKILL - 1)

// sigkillPending is whether a /proc/<pid>/status has SIGKILL pending for its whole thread group:
// its bit set in ShdPnd, a mask in hex. The kernel's out-of-memory kill sends SIGKILL to the
// thread group, so ShdPnd shows it while the process frees its memory, exits and is a zombie,
// until its parent reaps it. SigPnd, the signals pending for one thread, is no evidence: the
// kernel adds SIGKILL to it in each thread of a process that any fatal signal is ending
// (complete_signal in kernel/signal.c), a SIGTERM, SIGINT or SIGPIPE the process does not handle
// among them, such as the SIGTERM role-init's drain sends every process of a role whose agent has
// ended. A mask that does not parse shows nothing.
func sigkillPending(status []byte) bool {
	mask, ok := keyValue(status, "ShdPnd")
	if !ok {
		return false
	}
	n, err := strconv.ParseUint(string(bytes.TrimSpace(mask)), 16, 64)
	return err == nil && n&sigkillBit != 0
}

// report writes the guard's own line for w, which the kernel killed, and gives its kill to the
// sink, as the guard writes and reports its own (writeKill): with w's role, "" for a process of no
// role, the kernel as its rule and its sizes, and the kernel's cause, or, for a process the
// classifier protected, one of those that run the role's session, the kernel's protected cause.
// Every kill counts in the sequence, as the guard's do.
func (g *Guest) report(w *watched) {
	m := g.riseMemory
	cause := shimwire.MemoryKillCauseKernel
	if w.class.Protected {
		cause = shimwire.MemoryKillCauseKernelProtected
	}
	g.writeKill(Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			PID: w.key.pid, Executable: w.executable, SizeBytes: w.resident,
			ReadingBytes: m.reading, CounterBytes: m.counter, LimitBytes: m.limit, At: g.riseAt, Cause: cause,
		},
		Role: w.class.Role, Rule: ruleKernel, Sizes: sizesKernel,
	}, m.shmem)
}

// unnamed writes n kills no process was named for, if any, as one line with the pod's memory at
// the burst's first rise, and lets them go, which ends the burst (endBurst).
func (g *Guest) unnamed(n uint64) {
	g.endBurst()
	if n == 0 {
		return
	}
	g.window.unnamed += n
	m := g.riseMemory
	g.logf(guestKilledUnnamedLine, n, g.kills, m.reading, m.counter, m.shmem, m.limit)
}

// endBurst ends the burst: no kill is pending, and each counted process, which counted for this
// burst alone, is settled, so a later burst neither names nor counts it, and the snapshot lets it
// go once the scan no longer holds it, a zombie too, unless it is a session process, which it
// keeps until it leaves /proc (snapshot).
func (g *Guest) endBurst() {
	g.pending = 0
	for _, w := range g.known {
		if w.evidence == counted {
			w.evidence = settled
		}
	}
}

// memoryNow is the pod's memory for the kill lines of a rise: memory.stat, the limit and the usage
// read again, so a wake, which reads no usage of its own, writes the usage at the rise.
func (g *Guest) memoryNow() killMemory {
	g.stat, g.limit = g.cgroup.readStat(), g.cgroup.readLimit()
	return killMemory{reading: g.stat.anon.n, counter: g.cgroup.readUsage().n, shmem: g.stat.shmem.n, limit: g.reportLimit()}
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
