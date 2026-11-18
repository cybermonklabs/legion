// Package memoryguard ends a process it may end when a container nears its memory limit, the
// process that grew the most over its last few reads, else the largest one new to its stay in the
// band, else the one that grew the most since its stay's first kill, else the largest, a fork that
// still runs its parent's program counted by what it holds over its parent, so that the kernel
// does not end the whole sandbox instead (INFRA-3557, by the rules of INFRA-3531). Under gVisor
// the host kernel sees one sandbox and not its processes: a container that passes its limit loses
// every process at once, which in an issue pod is every role's turn.
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
// a reading at each interval. The order (rank, INFRA-3531 R1 of spec version 31) is the most grown
// first: a process's growth is its size at this read less its size at its oldest read within the
// last growthWindow (300 ms), and growth under growthNoise (1 MiB) is none. Of those that did not
// grow over the window, those new to this stay in the band that hold memory of their own follow,
// the largest first: a process the guard has read at most once since the band's second-to-last
// reading, at the readings before the one it decides on, has no earlier read to grow from, or one
// alone, and can be a runaway that began between two readings or just before the band's last one
// (newToStay); a fork whose parent's size covers its own ranks at nothing, is not a process the
// guard has learned anything from, and ranks in the tiers below (inNewTier, INFRA-3531 R1 of spec
// version 34). The most grown since the first kill of this stay in the band follow, by growth since
// that kill as sinceStayKill measures it, also from growthNoise; before the stay's first kill no
// process has any. Those that grew by none of these follow, largest first, so where none grew and
// none is in the new tier the largest ends first. The tier of the new is there because this
// issue's tester measured, at e3a1923c, the latepool probe at 4 GiB (six growers starting together
// while a parent and three sleeping forks of a shared block held the pod at 84%) end a process that
// shares the block, a sleeping fork or the parent, first in 7 of 7 runs, the closest run's highest
// reading 2.4 MiB below the kernel's kill point; at 778c42de, where the tier held only the
// processes the reading before had not read, the run `4g-latepool-zrr4d` lost a sleeping fork
// first, its growers started 5 ms before the band's last reading. The tier of growth since the kill
// is there because a throttled pod reads 170 to over 300 ms apart, so the window can hold one read
// of each process: in the latepool and sharedpool probes under gVisor (INFRA-3531 T17) the guard
// went on after a kill by ending the largest, a sleeping fork, while the growers lived. It measures
// from the stay's first kill, not the previous one, since growers that step every 250 ms show no
// step in the 100 ms after the previous kill: at d22d8da3, `4g-latepool-kxv5k` and
// `4g-latepool-zxhnw` lost the probe's parent and a sleeping fork after a fall while the growers
// lived. Under gVisor each forked process reports the pages it shares with its parent as its own,
// and no smaps field tells them apart, so the largest can be a sleeping fork whose end frees
// nothing while another process fills the pod; for the same reason a process with no earlier read
// in the window, a fresh fork among them, counts as not grown. Where the tier of the new or the
// largest ranks by size, a process that runs its parent's executable with its parent's arguments,
// its parent read in this stay, is a fork that has not run a new program, and ranks by its size
// less its parent's at the guard's latest read of the parent, nothing where its size is less
// (INFRA-3531 R1 of spec version 33, discountForks): at 23fdd92f the new tier ended first a
// sleeping fork of a 2.4 GB parent in 2 of 7 latepool runs at 4 GiB. The parent itself ranks whole.
// Each kill's line names the tier that chose it, where the sizes it was chosen by came from and how
// much of its size its rank did not count (INFRA-3531 R3 of spec version 31; lines.go).
//
// So that it knows what grew when the reading reaches the mark, an armed guard reads those sizes
// at each interval already in the band, the bandPercent (10%) of the limit below the mark, keeps
// each process's reads by its pid and start time for the window, with how many readings of the
// stay in a row read it (keepSizes), keeps the least of each one's reads in the band of this stay
// and whether it grew between two of them (keepStays), and holds the processes each band reading's
// assessment found, at the sizes it read, for the next reading (held). At the first reading at or
// above the mark it decides on what the band holds, and sends the kill before it scans the process
// table, reads any descriptor or the counter, or starts the hour's scan for the largest process
// (tick): it orders the held processes at that reading's own time, each by its newest held read
// less its oldest held read within the window of that time, so a step older than the window is no
// growth there, or, where that window holds two held reads of no process while the band's own last
// window holds two of one, over that last window, since throttled band readings about 200 ms apart
// leave one held read in the window at the mark (orderHeld); it checks the held sizes against the
// floor; it opens the first by its pidfd, checks its start time and its exit, and kills it; and
// only then does it assess that reading, whose sizes are the window's newest reads (endHeld); the
// sizes as of the kill are those the decision read, and the assessment's for a process it did not
// hold (keepKillSizes). Where one of the held processes grew, it reads no smaps before the kill,
// and the kill's line and report carry the held size and that reading. Where none of them grew by
// the held reads, it reads again before the kill the smaps of the held processes that grew between
// two of their band reads of this stay or are new to it, keeps the rest at their held sizes, and
// orders them by those reads (rereadHeld): the step that takes the reading to the mark comes
// between the band's last reading and this one, and no held read shows it. Under gVisor the
// `shared` probe at 4 GiB, one grower stepping 64 MiB about every 250 ms beside three sleeping
// forks of a 1.5 GiB block, lost a sleeper first to a guard that decided on the held reads alone,
// since the grower's step before the crossing one was older than the window at the mark (INFRA-3531
// T17); a sleeping fork flat through the band is not read again, since its read changes no answer.
// With each smaps it reads again it reads again the executable and arguments of a held process the
// band read running its parent's program (reidentify): the band can read a process between its
// fork and its exec, as at 18c3c5bb in `4g-latepool-rjm8m`, where the probe's growers so read
// ranked at nothing as forks of the 2.4 GB parent and the parent ended first. A held process whose
// program after the kill is not the one the decision read it running takes its first read after
// the kill as its size as of the kill (keepKillSizes). It reads the band's growers first, the most
// grown across the stay first, then those new to the stay that held memory of their own at their
// band read, the newest first (INFRA-3531 R1 of spec versions 31 and 38), for half the interval
// from the reading at the mark by its clock, the first of them always and none after the read in
// hand once that has passed, and those it has not read keep their held sizes: at 9d6d24df,
// `4g-latepool-lmnt5`'s six growers each had one band read, and reading all six again under the
// pod's CPU throttle put the first kill 100.68 ms after the reading at the mark. Where those reads
// show no growth either, it looks for the processes started since the band's last reading, which
// no held read names (startedSince), and then reads the held processes new to the stay that ranked
// at nothing at their band read, forks whose parent's size covered their own (readHeldNew), each
// group the newest first, so the reads of sleeping forks do not spend the interval before a new
// process is read (INFRA-3531 R1 of spec version 38): at 43213c99, in `4g-latepool-rnr56`, three
// sleeping forks started inside the band were read first, each ranked at nothing, the search then
// read nothing, and the first kill went to their 2.4 GB parent 120.1 ms after the reading at the
// mark. It takes the order's second tier as one set, the processes new to the stay that it read at
// the mark: the held ones a read at the mark reached, at the sizes they read, and the ones it found
// started since. The largest of that set ends; a held process new to the stay that no read at the
// mark reached ranks by its held size with the other held processes, the largest of which ends only
// where the set holds none (settleNew). The six latepool growers of e3a1923c started after the
// band's last reading, and zrr4d's just before it. The search's reads before the kill are the
// listing of the processes, the identities of the newest new ones (their stat, exe, ns/pid and
// arguments) and their smaps, these two within the same budget; it takes the band's processes as
// the band read them, with no stat read, and leaves out a new process whose parent it cannot place.
// At f949e5ad two of seven latepool runs at 4 GiB put the first kill 196.3 and 128.2 ms after the
// reading at the mark while it read a stat of every held process and the identity of every new one
// before the kill. Whatever the budget, the guard reads at the mark at least one process new to the
// stay that it may end where one exists, and one that holds memory of its own (INFRA-3531 R1 of
// spec versions 32 and 34): where it has read none that does when the half interval has passed, it
// reads on past it through the same groups, the next of them in that order, until it has read one
// the guard may end that holds memory of its own, or has read one at all and one interval from the
// reading at the mark has passed; and once it has, it reads nothing more past the budget. Where
// that interval passes with only forks that rank at nothing read, the tiers below the new decide.
// So the first kill comes within one interval of the reading reaching the mark, also where the
// pod's CPU is throttled, and within two where a stall delays a read or the first kill must search
// for a process started after the band's last reading (INFRA-3531 R1 of spec version 35, T16):
// under gVisor, with six growers faulting their pages in, the host throttles the whole pod by its
// CFS quota, so a band reading that costs about 2 ms of work took 80 to 270 ms of wall time, and a
// read of a growing process's /proc files at times waits on its memory locks. This issue's
// tester measured 194 ms from the reading at the mark to the first kill, where R1 asks for one
// interval (100 ms), with a mark reading that read every size again; deciding on the band's reads
// brought 9 of 9 runs in 19 builds Jobs to 1.3 to 71.8 ms. The guard assesses the reading at the
// mark and acts on it as on any later reading where nothing is held (the reading crossed the whole
// band within one interval, or the reading before was not an armed guard's in the band), where the
// held first has exited or its pid names another process (its start time changed), or where the
// held sizes, or the sizes once read again, are under the floor or name no process: it never gives
// a rise up on held sizes alone. Below the band, while it is off and once it has given a rise up,
// it reads no size and keeps none: a pod that sits below its band pays for nothing but its
// readings.
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
// orders the processes: a process's size as of the kill is its size as the decision that sent the
// kill read it, in the assessment of that reading or, for a kill on the band's held reads, its held
// size, its read again or the search's read of it, or its first read after the kill for a process
// that decision did not read, and it has grown once its size is that plus growthNoise or more
// (atKill, keepKillSizes, grownSinceKill). A read made after the kill is not the size as of it: at
// 7356e0f6, in `8g-latepool-q56hm`, the growers stepped between the reading at the mark and the
// read after its kill, a guard that kept that read saw no growth at the next two readings, not
// fallen and 28 MiB above the kill's reading, and it waited. So a process that grows in steps wider
// than the window still counts, and six growers that together outrun what one kill frees, which
// this issue's tester measured ending a 4 GiB pod while the guard waited the whole wait, are ended
// one at each reading. Where neither holds, the guard ends nothing and decides again at the next
// reading, until the wait has passed; then it gives the rise up with reason not fallen, so a rise
// that is a protected process's own, Oh My Pi's, in which nothing it may end grows, costs at most
// one kill. A rise given up, for that reason or because no process may be ended, has the guard log
// the sizes it saw and end nothing more for that rise, until the reading has gone below the mark; a
// stop that repeats is logged at most once a minute (stopRise).
//
// The order's third tier is growth since the first kill of this stay in the band, measured the same
// way from the sizes as of that kill (atFirstKill), and not since the previous kill, which decides
// only whether the guard goes on (INFRA-3531 R1 of spec version 34): growers that step every 250 ms
// in step with each other show no step in 100 ms since the previous kill nor in the window over
// reads 100 ms apart, and at d22d8da3, in `4g-latepool-kxv5k` and `4g-latepool-zxhnw`, the two
// kills after a fall went to the largest, the probe's parent and a sleeping fork, while the growers
// rose from 90.6% to 95.9% of the limit, 7.3 and 40.9 MiB from the kill point. A reading below the
// mark that is in the band ends a rise and not its stay, so those sizes are kept across it and the
// band's readings measure from them, and they go where the stay ends, below the band, while the
// guard is off and where it gives a rise up (forgetSizes; INFRA-3531 R1 of spec version 37): at
// 15bdbd3c, in `8g-latepool-r2`, `8g-latepool-r3` and `8g-latefork2-r1`, one grower was left, the
// reading went about 2 MB below the mark and came back, and a guard that let them go below the mark
// saw no growth since the dip and ended the probe's 4.8 GB parent first, which freed about 6 MB.
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
//
// In a pod that runs in a GKE Sandbox microVM (INFRA-3729) the launcher runs Guest in place of the
// guard (guest.go). The microVM's kernel is the pod's own, and its out-of-memory killer stops one
// process rather than the whole sandbox, so Guest ends nothing. At each scan it raises to
// oom_score_adj 1000 every process of a role that the same Classifier does not protect, and sets
// each protected process that reads above the starting score back to it, so that the kernel stops
// a command before an agent. It names each process that the kernel stopped from a rise of the
// cgroup's oom_kill counter, writes the guard's own ended line for it and gives the sink a Kill,
// through the guard's own writer (writeKill), with one of the kernel's two causes, the second for
// a process the Classifier protects; and it logs the cgroup's memory each hour. Like the guard, it
// runs only where its launcher is the container's first process.
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
// the longest wait after a kill for the killed process to exit, after which the guard decides at
// each reading and, with the reading not fallen and nothing grown since the kill, gives the rise
// up (watch), and the time between two readings.
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
//
// Session is a process whose end ends its role's session: the role's role-init, its shim, and
// every direct child of the shim. The classifier cannot tell the agent from the shim's other
// direct children (the secrets renewer, the working copy's adoption, the CodeGraph warm-up), so it
// marks them all, a superset of the agent whose only cost is a kill left unnamed, never a wrong
// name (Guest). Oh My Pi's helpers are not session processes, since one can end alone. The guest
// watch reads it; the guard ignores it.
type Class struct {
	Role      string
	Protected bool
	Session   bool
}

// Classifier says, for each process of a scan, whose it is and whether the guard may end it. It
// is called once a scan, from the guard's loop when it acts and from its hourly scan, which may
// run at the same time. A pid absent from the map is a process the guard may end, of no role.
type Classifier func(t *Table) map[int]Class

// Kill is one process the guard ended: the report its role is sent, which names the process by
// processName and carries none of its arguments, and Role, the role whose process it was, ""
// outside every role. Rule is which rule of the order of ends put it first (rank: ruleGrew,
// ruleNew, ruleGrewSinceKill or ruleLargest) and Sizes where the sizes it was chosen by came from
// (sizesBand, sizesReread, sizesSearch or sizesReading), each ruleKernel and sizesKernel for a
// process the guest kernel killed (INFRA-3531 R3 of spec version 31). LessParent is how much of its
// size its rank did not count, as a fork that runs its parent's program (candidate.lessParent), 0
// for every other process and every kill of the guest kernel (INFRA-3531 R1 of spec version 33).
// NewRead is what the reads at the reading at the mark read of the processes new to the stay, for
// a first kill those reads decided (newRead.field): how many they read, and of those how many hold
// memory of their own; "" for every other kill, which the line writes as `-`. The ended line
// carries the four (writeKill); the report on the wire carries none of them.
type Kill struct {
	shimwire.LauncherMemoryKill
	Role       string
	Rule       string
	Sizes      string
	LessParent uint64
	NewRead    string
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

// afterKill is the kills a waiting guard watches, all sent at one reading: each process killed
// (kills), whose exits end the wait, and which the guard never ends again while it has not exited
// (leaveOut); the reading at the kills, which the reading must fall below (fell); and when the wait
// passes at the latest. exited is whether every one of them has exited. Its kills keep their
// storage from one kill to the next (endWait).
type afterKill struct {
	kills    []killedProc
	reading  uint64
	deadline time.Time
	exited   bool
	fallen   bool
}

// killedProc is one process a waiting guard killed: the process held, by its pidfd, whose answer
// to Exited is of that process alone, the same process by its pid and start time (key), and
// whether it has exited.
type killedProc struct {
	handle Handle
	key    procKey
	exited bool
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

// procReads is what the guard keeps of one process's reads (keepSize): window, its reads within
// growthWindow before the newest, oldest first, which its growth is measured by; and readings, how
// many readings of this stay in a row, up to the newest, read it, which say whether it is new to
// the stay (candidate.newToStay). A reading that does not read a process its table still lists
// keeps its window and restarts its count, so the count starts again from the next one that reads
// it; a reading whose table no longer lists the process drops both (keepSizes, INFRA-3531 R1).
type procReads struct {
	window   []sizeRead
	readings int
}

// killSize is a process's size as of a kill, from which its growth since that kill is measured
// (grownSinceKill, sinceStayKill), and the reading by the guard's clock it was last read at, by
// which keepKillSizes knows the sizes the kill it keeps has kept already. A size is dropped once
// the reading's table no longer lists its process (dropEnded), and not for a reading that did not
// read it.
type killSize struct {
	size uint64
	seen time.Time
}

// bandStay is what a process's reads in the band of this stay say (keepStays): the least of them,
// whether one of them is growthNoise or more above an earlier one, and the reading by the guard's
// clock it was last read at. A band reading that does not read a process its table still lists
// leaves its stay as it was; one whose table no longer lists it drops it (INFRA-3531 R1).
type bandStay struct {
	least uint64
	grew  bool
	seen  time.Time
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
	// killWriter is the guard's id, its log, its sink and its count of kills (writeKill).
	killWriter
	proc     string
	classify Classifier
	clock    Clock
	signal   Signaler
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
	// identityRead, when set, is called with the pid of each process started since the band's
	// last reading whose stat, exe, ns/pid and arguments the reading at the mark reads
	// (startedSince), before it reads them. The tests set it to count those reads and to move
	// their clock on; production leaves it nil.
	identityRead func(pid int)
	// argsRead, when set, is called with the pid of each cmdline the fork discount reads whole
	// (sameProgram), before it reads it. The tests set it to count those reads; production leaves
	// it nil.
	argsRead func(pid int)
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
	// sizes are the reads of the size of each process the latest assessment could end (keepSizes):
	// those within growthWindow before it, oldest first, which a process's growth is measured by,
	// and how many readings of this stay in a row read it, which say whether it is new to the
	// stay. They are emptied wherever the guard reads no size (forgetSizes), where a stay ends.
	sizes map[procKey]procReads
	// held are the processes an armed guard's assessment at the reading before found it may end,
	// in the band below the mark, each at the size that assessment read, its newest read in
	// sizes: what the next reading, where it is at or above the mark, decides on (endHeld). A
	// reading in the band holds its own, and every other drops them, the one at the mark once it
	// has decided on them, so they are never of a reading older than the one before and never
	// held across a kill; forgetSizes drops them with the reads.
	held []candidate
	// heldTable is the table of the assessment held is of, the band's last reading's: the
	// processes by pid and start time that the reading at the mark tells a process started since
	// then from (startedSince). It is held and dropped with held.
	heldTable *Table
	// stays are, by process, its reads at the armed guard's readings in the band of this stay in
	// the band (keepStays): the least of them, and whether it grew between two of them, which say
	// with the count sizes keeps whether the reading at the mark reads its smaps again
	// (rereadHeld). A stay starts at the first armed reading in the band and ends wherever the
	// guard drops its reads (forgetSizes); the reads at and above the mark are none of its. They
	// keep their storage from one reading to the next.
	stays map[procKey]bandStay
	// atKill are the sizes, by process, as of the previous kill of the rise, from which growth
	// since that kill is measured (grownSinceKill), which says whether the guard goes on after it:
	// the sizes the decision that sent the kill read (keepKillSizes), the assessment of the reading
	// (act) or the held processes as endHeld decided on them, and for a process that decision did
	// not read, its first read after the kill. They are emptied when the wait after the kill ends
	// (endWait), and their storage is kept from one kill to the next.
	atKill map[procKey]killSize
	// atFirstKill are the sizes, by process, as of the first kill of this stay in the band, kept
	// the same way, from which the order's third tier measures growth (sinceStayKill);
	// firstKillKept is whether the stay has had that kill. Both are let go of where the stay ends
	// (forgetSizes): below the band, while the guard is off and where it gives a rise up. A
	// reading below the mark that is in the band ends a rise and keeps them.
	atFirstKill   map[procKey]killSize
	firstKillKept bool

	scans    sync.WaitGroup
	scanMu   sync.Mutex
	scanBusy bool
	scanDone *largestScan
	// The rest is scanLargest's own, one scan at a time, since startScan starts none while one
	// runs: its smaps buffer and the memfds its sizes have counted.
	scanSmaps  []byte
	scanMemfds map[memfdID]bool
	// cmdline is the buffer the reading at the mark reads a process's first classifierArgsBytes
	// of arguments into (startedSince, reidentify), made at its first use and kept.
	cmdline []byte
}

// New is a guard of cfg, at cfg.Start or the default settings and on, with an id of its own.
func New(cfg Config) *Guard {
	proc := cmp.Or(cfg.Proc, "/proc")
	cgroup := cmp.Or(cfg.Cgroup, "/sys/fs/cgroup/memory")
	g := &Guard{
		killWriter:   newKillWriter(cfg.Logf, cfg.Sink),
		proc:         proc,
		classify:     cfg.Classify,
		clock:        cfg.Clock,
		signal:       cfg.Signal,
		observe:      cfg.Observe,
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
		sizes:        map[procKey]procReads{},
		atKill:       map[procKey]killSize{},
		atFirstKill:  map[procKey]killSize{},
		stays:        map[procKey]bandStay{},
		scanMemfds:   map[memfdID]bool{},
	}
	if g.clock == nil {
		g.clock = &wallClock{}
	}
	if g.signal == nil {
		g.signal = defaultSignal()
	}
	return g
}

// newWatchID is a watch's own id, 8 random bytes in hex: the guard's (New) and the guest watch's
// (NewGuest) alike.
func newWatchID() string {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// killWriter is how a watch writes each process it names as stopped for memory and hands that
// process's report to its sink: the guard's own kills (Guard.act) and the ones the guest kernel
// made (Guest.report) alike, so either watch writes the same line and the same report, under the
// watch's own id and its own count of kills from 1. logf writes every other line of the watch's
// too. sequence is the watch's loop's own.
type killWriter struct {
	id       string
	logf     func(format string, args ...any)
	sink     func(Kill)
	sequence uint64
}

// newKillWriter is a watch's killWriter, with an id of its own (newWatchID): a nil logf discards
// every line, and a nil sink takes no report.
func newKillWriter(logf func(format string, args ...any), sink func(Kill)) killWriter {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return killWriter{id: newWatchID(), logf: logf, sink: sink}
}

// writeKill takes the next sequence for k, a process the watch names as stopped for memory: it
// writes the guard's own endedLine for k, with shmem, the pod's shared memory then, which the line
// alone carries, and k's NewRead, `-` where it has none, and hands the sink k under the watch's id
// and that sequence.
func (w *killWriter) writeKill(k Kill, shmem uint64) {
	w.sequence++
	k.Guard, k.Sequence = w.id, w.sequence
	newRead := k.NewRead
	if newRead == "" {
		newRead = "-"
	}
	w.logf(endedLine, k.PID, roleName(k.Role), k.SizeBytes, k.ReadingBytes, k.CounterBytes, shmem, k.LimitBytes, k.Rule, k.Sizes, k.LessParent, newRead, k.Executable)
	if w.sink != nil {
		w.sink(k)
	}
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
// the band below the mark (bandStart) reads the sizes of what it may end (assessAt), counts them as
// reads of this stay in the band (keepStays) and holds them for the next reading (held). At its
// first reading at or above the mark it decides on those held sizes and the reads kept with them
// (endHeld), ordered over the window at that reading or, where it holds two reads of no held
// process, over the band's own last window (orderHeld): it sends the kill before it scans the
// process table, and before it reads any smaps where one of the held processes grew by the held
// reads; where none did, it reads first the smaps of the held processes that grew in the band or
// are new to this stay (rereadHeld), since the step that took the reading to the mark falls after
// the band's last reading and the held reads cannot show it, and a guard that did not read them
// again ended a sleeping fork before the grower in the `shared` probe under gVisor (INFRA-3531
// T17). Only then does it assess the reading, whose sizes are the window's newest reads and the
// first reads after the kill of the processes the decision did not hold; the sizes as of the kill
// are the ones the decision read (keepKillSizes). tick runs nothing of the hourly line but its
// rollover before decide returns. That is the cut of INFRA-3531 T16, so the first kill comes within
// one interval of the reading reaching the mark, and within two where it must search for a process
// started after the band's last reading (R1 of spec version 35): with six growers on the pod's CPU
// the host
// throttles the whole pod, a band reading of about 2 ms of work took 80 to 270 ms of wall time, and
// a mark reading that read every size again put its kill 194 ms after the reading, where one that
// decided on the band's reads put it 1.3 to 71.8 ms after it in 9 of 9 runs. Where nothing is held
// (the reading crossed the whole band within one interval, or the reading before was not an armed
// guard's in the band), where the held first has exited or its pid names another process (its start
// time changed), or where the held sizes, or the sizes once read again, are under the floor or name
// no process, the guard assesses the reading at the mark and acts on it (act), as on any reading of
// a rise. A reading that ends a rise, below the mark after a kill or after the rise was given up,
// is an armed guard's, and reads and holds the sizes where it is in the band. Below the band, while
// off and while the rise is given up, the guard reads no size and keeps none (forgetSizes), and the
// stay in the band ends there; a reading below the mark that is in the band ends a rise and not the
// stay, so the sizes as of the stay's first kill (atFirstKill) are kept across it, and each band
// reading after that kill measures growth since it (sinceStayKill), as the reading at the mark of
// the next rise does before it decides (INFRA-3531 R1 of spec version 37).
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
		decided := g.held
		g.held, g.heldTable = nil, nil
		if sent {
			// The kill is sent on the sizes the decision read, which are the sizes as of it; this
			// reading's, read after the kill, are the window's newest reads and the first reads
			// after the kill of the processes the decision did not hold.
			if a, ok := g.assessAt(now); ok {
				g.keepKillSizes(now, decided, a.endable)
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
		g.sinceStayKill(now, a.endable, a.table)
		g.held, g.heldTable = a.endable, a.table
		g.keepStays(now, a.endable, a.table)
	default:
		// A first kill of a rise that the band's reads did not decide: growth since the stay's
		// first kill orders it as it does every later kill.
		if g.sinceStayKill(now, a.endable, a.table) {
			rank(a.endable)
		}
		g.act(now, s, reading, limit, &a)
	}
}

// endHeld ends the first of the held processes, the ones the assessment at the reading before,
// in the band, found the guard may end, at the reading taken at now, the first at or above the
// mark, and answers whether it sent the kill. It scans no process table and reads no descriptor
// before the kill (signalKill). The order is the order of ends (rank) at now, by each process's
// growth over its held reads (orderHeld). Where one of them has grown, it decides on the held
// reads alone and reads no smaps before the kill, and the held processes keep the identities the
// band read: growth puts the first first there, and the fork discount orders only processes of
// equal growth. The kill's line says what the reads at the mark read of the processes new to the
// stay (new_read: newRead.field where it reads again, `-` where it does not).
//
// Where none of them has grown by the held reads, it reads again first the smaps of those that
// grew between two of their reads in the band of this stay, then of those new to it that held
// memory of their own at their one band read, and decides on those reads (rereadHeld), since the
// held reads cannot show the step that took the reading to the mark: that step comes between the
// band's last reading and this one. Under gVisor at the head that decided on the held reads alone
// (INFRA-3531 T17), the `shared` probe at 4 GiB, a grower stepping 64 MiB about every 250 ms
// beside three sleeping forks of a 1.5 GiB block, had its first kill end a sleeper: the grower's
// step before the crossing one was older than the window at the mark, so the held order showed no
// growth and put the largest first, a fork whose end frees nothing; the sharedpool and latepool
// probes lost a sleeper at the first kill of each rise the same way, their band readings spread
// 170 to 210 ms apart by the throttle and one held read left in the window. With each held process
// whose smaps it reads at the mark where the band read it running its parent's program, it reads
// that process's executable and arguments again (reidentify), since the band may have read it
// between its fork and its exec, and one that has run another program since ranks by its whole
// size; a held process whose smaps it does not read again keeps the identity the band read.
//
// Where those reads show no growth either, it looks for the processes it may end that started
// since the band's last reading (startedSince), which no held read names, and takes the order's
// second tier as one set, the processes new to this stay that it read at the mark (INFRA-3531 R1
// of spec version 31): the held ones a read at the mark reached, at the sizes they read, and the
// ones it found started since. The largest of that set ends. A held process new to the stay that no
// read at the mark reached is not in the set: it ranks by its held size with the other held
// processes, the largest of which ends only where the set holds none (settleNew). This issue's
// tester measured, at e3a1923c, the latepool probe at 4 GiB, six growers starting together between
// the band's last reading and the mark while a parent and three sleeping forks of a shared block
// held the pod at 84%, end a process that shares the block, a sleeping fork or the parent, first
// in 7 of 7 runs, the closest run's highest reading 2.4 MiB below the kernel's kill point
// (INFRA-3531 R1 of spec version 27); and at 778c42de, in the latepool run `4g-latepool-zrr4d`,
// six growers that started 5 ms before the band's last reading, which read each of them once at a
// few MiB, lost a sleeping fork first to a guard that counted a held process as old, so the
// largest held process came first.
//
// The reads at the mark share half the interval from now by the guard's clock, in this order
// (INFRA-3531 R1 of spec version 38): the reads again take the band's growers, the most grown
// first, and then the held processes new to the stay that held memory of their own at their one
// band read (rereadHeld); the search then takes the processes started since, the ones with no
// earlier read (startedSince); and the held processes new to the stay that ranked at nothing at
// their band read, forks whose parent's size covered their own (candidate.nothingAtBand), come last
// (readHeldNew); each group the newest first. Whatever the budget, it reads at least one process
// new to the stay that it may end where one exists (INFRA-3531 R1 of spec version 32: "and at
// least one of them"), and one that holds memory of its own: a fork whose parent's size covers its
// own, which ranks at nothing (candidate.rankSize), does not count, and where it has read none that
// does, it reads on past the half interval up to one interval from now, stopping after the read in
// hand once that has passed (INFRA-3531 R1 of spec version 34; readOnNew). It reads past the budget
// in the same order: the held processes new to the stay with memory of their own that the reads
// again did not reach, then the processes started since, then the forks that ranked at nothing. At
// d22d8da3, in `4g-latepool-54nzl` and `4g-latepool-zxhnw`, the reads again reached one sleeping
// fork of the probe's 2.4 GB parent, which spent the half interval and ranked at nothing, the
// search read nothing, and the fork ended first while six growers that started after the band's
// last reading lived; at 43213c99, in `4g-latepool-rnr56`, the reads of three such forks spent the
// interval, the search read nothing, and their parent ended first, 120.1 ms after the reading at
// the mark. Once it has read what it must, it reads nothing more past the budget. Where one
// interval passes with only forks that rank at nothing read, those are not in the new tier
// (inNewTier), and the tiers below it decide: growth since the first kill of this stay in the band
// (sinceStayKill), none before that kill, and then the largest by rank size. A rise that follows a
// dip below the mark in the band is of the same stay, so a grower left from the rise before ranks
// by its growth since that rise's first kill, though no read since the dip has seen it grow
// (INFRA-3531 R1 of spec version 37).
// The floor is read on the held sizes, and again on the sizes once read again. It sends no kill
// where they are under the floor or name no process, or where the first has exited, its pid names
// another process or its kill fails, and decide then assesses the reading and acts on it: what the
// band held never gives a rise up. The kill's line says which sizes it was chosen by: the held
// reads alone (sizesBand), with smaps it read again (sizesReread), or with processes it found
// started since (sizesSearch).
//
// Where the reading is at or above the urgent share it ends every held process that grew over the
// window, not only the first (endAt, INFRA-3531 R1 of spec version 39), and so it reads the band's
// growers and the held processes new to the stay with memory of their own again within the half
// interval (rereadHeld) also where one of them grew by the held reads: a grower whose step came
// between the band's last reading and this one shows it only in a read at the mark, and each keeps
// the larger of its growth by the held reads and by its read at the mark. The reads again do not
// stop at the first grower they find; they stop once the half interval has passed.
func (g *Guard) endHeld(now time.Time, s Settings, reading, limit uint64) bool {
	floor := share(limit, s.FloorPercent)
	grew, heldBytes := g.orderHeld(now)
	if len(g.held) == 0 || heldBytes < floor {
		return false
	}
	sizes := sizesBand
	var found []candidate
	// readField is the kill line's new_read, "" where the guard reads nothing before the kill.
	var readField string
	// The held processes' parents are the band's, at their latest reads, the ones read again at
	// now among them; the found ones are discounted by the table that placed them, each as it is
	// read, and one decision reads a parent's whole arguments once (argsCache).
	var cache argsCache
	if !grew || reading >= urgentShare(limit, s.ActPercent) {
		half, whole := now.Add(s.Interval/2), now.Add(s.Interval)
		grewSince := g.rereadHeld(now, half, &cache) || grew
		g.discountForks(g.held, g.heldTable, &cache)
		read := g.heldNewRead(now)
		if !grewSince {
			// The rest of the held processes new to the stay that held memory of their own at
			// their band read, then the processes started since, then the held forks that ranked
			// at nothing at their band read, each group the newest first.
			g.readHeldNew(now, half, whole, false, &read, &cache)
			found = g.startedSince(half, whole, &read, &cache)
			g.readHeldNew(now, half, whole, true, &read, &cache)
		}
		readBytes, reread := g.settleNew(now)
		if readBytes < floor || len(g.held) == 0 {
			return false
		}
		if reread {
			sizes = sizesReread
		}
		readField = read.field()
	} else {
		g.discountForks(g.held, g.heldTable, &cache)
	}
	if len(found) > 0 {
		g.held = append(g.held, found...)
		sizes = sizesSearch
	}
	g.sinceStayKill(now, g.held, g.heldTable)
	rank(g.held)
	return g.endAt(now, s, reading, limit, g.held, sizes, readField)
}

// newRead is what the reads at the mark have read of the processes new to the stay that the guard
// may end: how many (reached), and how many of them rank above nothing, with memory of their own
// (own).
type newRead struct{ reached, own int }

// add counts c, a process new to the stay the reads at the mark read, its discount set.
func (r *newRead) add(c *candidate) {
	r.reached++
	if c.rankSize() > 0 {
		r.own++
	}
}

// field is r as the ended line's new_read writes it: reached and own, a colon between them.
func (r newRead) field() string { return strconv.Itoa(r.reached) + ":" + strconv.Itoa(r.own) }

// readOnNew is whether the reads at the mark read one more process new to the stay past half the
// interval, having read r of them: where they have read none the guard may end, whatever the
// clock; where they have read none that ranks above nothing, while the guard's clock is before
// whole, one interval from the reading at the mark (INFRA-3531 R1 of spec version 34).
func (g *Guard) readOnNew(r newRead, whole time.Time) bool {
	return r.reached == 0 || r.own == 0 && g.clock.Now().Before(whole)
}

// heldNewRead is what the reads again at the reading taken at now (rereadHeld) read of the held
// processes new to the stay, their discounts set.
func (g *Guard) heldNewRead(now time.Time) newRead {
	var r newRead
	for i := range g.held {
		if c := &g.held[i]; c.newToStay && g.readAt(c, now) {
			r.add(c)
		}
	}
	return r
}

// readAt is whether the guard has read c's size at the reading taken at now.
func (g *Guard) readAt(c *candidate, now time.Time) bool {
	w := g.sizes[procKey{pid: c.PID, start: c.StartTime}].window
	return len(w) > 0 && w[len(w)-1].at.Equal(now)
}

// readHeldNew reads at the reading taken at now the held processes new to the stay of one group
// of the reads at the mark that no read at now has reached, the newest first (rereadHeld's order):
// where forks is false, those that held memory of their own at their band read, which the reads
// again take within half the interval and this reads past it; where it is set, those that ranked
// at nothing at it (candidate.nothingAtBand), which come after the processes started since
// (INFRA-3531 R1 of spec version 38). It reads each while half has not passed, or past it while
// the guard reads on with read (readOnNew), and counts each in read. Each it reads it identifies
// again (reidentify) and discounts (discountFork) with cache; one whose smaps or executable it
// cannot read it leaves out of the held processes, as rereadHeld does, and reads the next.
func (g *Guard) readHeldNew(now, half, whole time.Time, forks bool, read *newRead, cache *argsCache) {
	defer g.releaseSmaps()
	kept := g.held[:0]
	for _, c := range g.held {
		if c.newToStay && c.nothingAtBand == forks && !g.readAt(&c, now) &&
			(g.clock.Now().Before(half) || g.readOnNew(*read, whole)) {
			size, err := g.size(c.PID, nil)
			if err != nil || !g.reidentify(c.Process, cache) {
				continue
			}
			c.size = size + c.descriptorBytes
			g.keepSize(now, &c)
			g.discountFork(&c, g.heldTable, cache)
			read.add(&c)
		}
		kept = append(kept, c)
	}
	g.held = kept
}

// settleNew makes the held processes new to the stay the ones the order's second tier takes at the
// mark, once the reads at the reading taken at now are made (INFRA-3531 R1 of spec version 31):
// those a read at now reached, at the size it read. One that no read reached is not new there, and
// ranks by its held size with the other held processes. It answers the held processes' sum and
// whether a read at now read one of them.
func (g *Guard) settleNew(now time.Time) (total uint64, reread bool) {
	for i := range g.held {
		c := &g.held[i]
		at := g.readAt(c, now)
		c.newToStay = c.newToStay && at
		reread = reread || at
		total += c.size
	}
	return total, reread
}

// orderHeld sets each held process's growth at the reading taken at now, the first at or above
// the mark, and whether it is new to this stay, and answers whether one of them grew and the sum
// of their held sizes. A process's growth is its newest held read, the size it is held at, less
// its oldest held read within growthWindow before now, growthNoise or more, so a read the window
// held at the band's last reading and no longer holds at now counts for nothing, and a step older
// than the window is no growth; a process with no earlier read in the window has not grown. A held
// process is new to the stay where fewer than two readings in a row read it, so where the band
// read it at its last reading alone (candidate.newToStay), and of those it records the ones that
// ranked at nothing at that read, by the band's discount (candidate.nothingAtBand): together they
// say which group of the reads at the mark takes each (rereadHeld, readHeldNew); the order takes
// as new those of them alone that a read at now reached (settleNew).
//
// Where that window holds two held reads of no held process, while the window that ends at the
// newest held read, the band's own last window, holds two of at least one, the growth is measured
// over that last window instead (INFRA-3531 T17). Throttled band readings come about 200 ms apart,
// so the read before last can be more than growthWindow before the reading at the mark, and the
// window at the mark then shows no growth however much the band saw: at the head that measured
// over it alone, `4g-protpool-cpu1-x9nlv` (band readings 193 and 197 ms apart, then 188 ms to the
// mark) and `8g-latepool-tgvnz` (151 and 120 ms, then 204 ms) read their held processes again
// under the throttle and sent their first kills 201 and 101.75 ms after the reading at the mark,
// past one interval. Where no window holds two held reads (band readings more than growthWindow
// apart), neither shows growth.
func (g *Guard) orderHeld(now time.Time) (grew bool, heldBytes uint64) {
	atMark, atBand := false, false
	for _, c := range g.held {
		reads := g.sizes[procKey{pid: c.PID, start: c.StartTime}].window
		if len(reads) == 0 {
			continue
		}
		atMark = atMark || len(reads)-windowStart(reads, now) >= 2
		atBand = atBand || len(reads)-windowStart(reads, reads[len(reads)-1].at) >= 2
	}
	bandWindow := !atMark && atBand
	for i := range g.held {
		c := &g.held[i]
		r := g.sizes[procKey{pid: c.PID, start: c.StartTime}]
		end := now
		if bandWindow && len(r.window) > 0 {
			end = r.window[len(r.window)-1].at
		}
		c.growth = growthOver(r.window[windowStart(r.window, end):], c.size)
		c.newToStay = r.readings < 2
		c.nothingAtBand = c.newToStay && c.rankSize() == 0
		grew = grew || c.growth > 0
		heldBytes += c.size
	}
	return grew, heldBytes
}

// startedSince is the processes the guard may end that started since the band's last reading,
// whose table the guard holds with the held processes (heldTable), each at its smaps size and new
// to this stay, since no reading of the stay read it, the newest first. Its reads before the kill
// are the listing of the processes, the identities of the newest new ones and their smaps, these
// two within deadline, half an interval from the reading at the mark: at f949e5ad two of seven
// latepool runs at 4 GiB sent the first kill 196.3 and 128.2 ms after the reading at the mark, past
// one interval, their growers with one band read at most and so found here, while it read the stat
// of every held pid and the four identity files of every new one before the kill, each a read that
// can wait out a stall of the pod's CPU throttle.
//
// It reads within half, half an interval from the reading at the mark, and past it only while the
// guard reads on (readOnNew) with read, what the reads at the mark have read of the processes new
// to the stay, and what it reads itself: where no process new to the stay that the guard may end
// has been read, whatever the clock (INFRA-3531 R1 of spec version 32: "and at least one of
// them"), and where none read ranks above nothing, until whole, one interval from the reading at
// the mark (spec version 34). Past half it reads one process at a time, the newest first, its
// identity and, where the guard may end it, its smaps, and stops after the read in hand once it
// need not read on; it reads nothing at all, the listing included, where half has passed and it
// need not. Each process it reads it discounts as a fork (discountFork) with t and cache.
//
// It lists the processes once, and takes the band's table as it is: a listed pid that table holds
// is that process, and it reads nothing of it. A pid that another process took within one interval
// is improbable, and signalKill reads its target's start time before the kill, so a reused pid is
// never signalled: decide then assesses the reading. Every other listed pid is a process started
// since, whose stat, exe, ns/pid and arguments it reads as a scan does (readProcess), the highest
// pid first, the order gVisor and Linux allocate pids in, so the newest first; a pid space that has
// wrapped changes only which are read first. A new process whose parent is neither one of the
// band's processes still listed nor a new process it read and placed is left out of this decision,
// neither one it may end nor one it protects, since the classifier cannot place it in the tree;
// the assessment of the reading after the kill reads it.
//
// It asks the classifier about the table of the band's processes still listed and the new ones it
// placed, so a process started since is protected or may be ended by the same rules as any, an Oh
// My Pi helper started under the agent among them: once, and once more for each process it reads
// past the budget, since only the classifier says which the guard may end. It reads the smaps of
// those it may end alone, the newest by start time first; it reads no descriptor, and one whose
// smaps it cannot read is left out. It answers none where none started, and asks the classifier
// nothing then.
func (g *Guard) startedSince(half, whole time.Time, read *newRead, cache *argsCache) []candidate {
	band := g.heldTable
	// pastBudget is whether half has passed and the reads at the mark need not read on.
	pastBudget := func() bool { return !g.clock.Now().Before(half) && !g.readOnNew(*read, whole) }
	if band == nil || pastBudget() {
		return nil
	}
	entries, err := os.ReadDir(g.proc)
	if err != nil {
		return nil
	}
	t := &Table{proc: g.proc, byPID: make(map[int]*Process, len(band.byPID)), children: map[int][]*Process{}, unread: map[int]error{}}
	var newPIDs []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, held := band.byPID[pid]
		reason, unread := band.unread[pid]
		if !held && !unread {
			newPIDs = append(newPIDs, pid)
			continue
		}
		if held {
			t.byPID[pid] = p
		}
		if unread {
			t.unread[pid] = reason
		}
	}
	if len(newPIDs) == 0 {
		return nil
	}
	slices.SortFunc(newPIDs, func(x, y int) int { return cmp.Compare(y, x) })
	defer g.releaseSmaps()
	cmdline := g.cmdlineBuffer()
	// started are the new processes it has read the identity of and not yet the smaps.
	started := map[int]*Process{}
	var found []candidate
	// One round reads the identities the budget allows, or the next one alone where it has passed
	// and the guard reads on, and then the smaps of those of them it may end.
	for next := 0; next < len(newPIDs); {
		from := next
		for ; next < len(newPIDs); next++ {
			if !g.clock.Now().Before(half) && (next > from || !g.readOnNew(*read, whole)) {
				break
			}
			pid := newPIDs[next]
			if g.identityRead != nil {
				g.identityRead(pid)
			}
			p, err := readProcess(g.proc, pid, cmdline)
			if err != nil {
				if p == nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, errExited)) {
					continue
				}
				t.unread[pid] = err
			}
			if p != nil {
				started[pid] = p
			}
		}
		if next == from {
			break
		}
		placed := placeStarted(t, started)
		for _, p := range placed {
			t.byPID[p.PID] = p
		}
		clear(t.children)
		t.linkChildren()
		g.applyKillFailures(t)
		classes := g.classes(t)
		slices.SortFunc(placed, func(x, y *Process) int {
			return cmp.Or(cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
		})
		for _, p := range placed {
			if _, unread := t.unread[p.PID]; unread || p.PID == g.self || classes[p.PID].Protected {
				continue
			}
			if pastBudget() {
				break
			}
			delete(started, p.PID)
			size, err := g.size(p.PID, nil)
			if err != nil {
				continue
			}
			c := candidate{Process: p, role: classes[p.PID].Role, size: size, newToStay: true}
			g.discountFork(&c, t, cache)
			read.add(&c)
			found = append(found, c)
		}
		if pastBudget() {
			break
		}
	}
	return found
}

// placeStarted is the processes of started, new processes the reading at the mark read by pid,
// that the classifier can place in the tree of t, which holds the band's processes still listed and
// the new ones placed before: one whose parent is one of those, or a new process placed itself.
// Every other is left out of what the decision at the mark may end or protects.
func placeStarted(t *Table, started map[int]*Process) []*Process {
	placed := make(map[int]bool, len(started))
	for again := true; again; {
		again = false
		for pid, p := range started {
			if _, listed := t.byPID[p.PPID]; !placed[pid] && (listed || placed[p.PPID]) {
				placed[pid], again = true, true
			}
		}
	}
	out := make([]*Process, 0, len(placed))
	for pid, p := range started {
		if placed[pid] {
			out = append(out, p)
		}
	}
	return out
}

// rereadHeld reads again, at the reading taken at now, the smaps of the held processes that grew
// by growthNoise or more between two of their reads in the band of this stay (stays), or that are
// new to the stay, the band having read them at its last reading alone (candidate.newToStay), and
// of no other process: it scans no process table, asks no classifier and reads no descriptor. Each
// one it reads has as its size its smaps size now and the memfds it holds by a descriptor alone at
// what the band's assessment counted for them (descriptorBytes), kept as its read at now
// (keepSize), which sets its growth at now. Every other held process keeps its held size and no
// growth. It answers whether one it read grew, and leaves the held processes in the order it read
// them in. A process new to the stay started between the band's last two readings, and may be a
// grower the band has not yet seen grow: in T17's `4g-latepool-dmq44` and `4g-latepool-mgzv9` the
// growers had one band read each, and reading them again kept a grower the first ended. A process
// with two reads or more that never grew between them is not read again, since a sleeping fork's
// read changes no answer, and so a process flat through the whole band that grows only at the mark
// is not ended first for that. A held process whose smaps it cannot read, gone or not, it leaves
// out of the held processes, and so out of this decision; the assessment of the reading that
// follows reads it again.
//
// It reads them one at a time, in a fixed order (rereadOrder): first those that grew in the band,
// the most grown across the stay first, then those new to the stay that held memory of their own
// at their band read, the newest by start time first (INFRA-3531 R1 of spec versions 31 and 38:
// each is new to the stay, and the newest is the likeliest runaway, one that began just before the
// band's last reading); a held process new to the stay that ranked at nothing at its band read is
// read after the processes started since (readHeldNew). It reads them to deadline by the guard's
// clock, which it reads between two smaps reads and at no other time: it reads the first however
// much of the budget the reading at the mark itself took, and once deadline has passed it stops
// after the read in hand. Those it has not read keep their held sizes and their growth by the held
// reads, none below the urgent share, where endHeld reads again only where no held read grew; at a
// reading at or above it one it reads keeps the larger of that growth and its growth at the mark
// (INFRA-3531 R1 of spec version 39). So the decision is made on what it read; a held process new
// to the stay that it has not reached is not among the new ones the order takes, unless the guard
// reads it yet (readHeldNew, settleNew). At 9d6d24df, `4g-latepool-lmnt5` sent its first kill
// 100.68 ms after the reading at the mark, past one interval: its six growers had one band read
// each, and the guard read all six growing processes' smaps again before the kill under the pod's
// CPU throttle. endHeld gives it half the interval.
func (g *Guard) rereadHeld(now, deadline time.Time, cache *argsCache) (grew bool) {
	defer g.releaseSmaps()
	slices.SortFunc(g.held, func(x, y candidate) int {
		xTier, xGrowth := g.rereadOrder(x)
		yTier, yGrowth := g.rereadOrder(y)
		return cmp.Or(cmp.Compare(xTier, yTier), cmp.Compare(yGrowth, xGrowth),
			cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
	})
	kept := g.held[:0]
	read, passed := false, false
	for _, c := range g.held {
		if tier, _ := g.rereadOrder(c); !passed && tier < notReread {
			passed = read && !g.clock.Now().Before(deadline)
			if !passed {
				read = true
				size, err := g.size(c.PID, nil)
				if err != nil || !g.reidentify(c.Process, cache) {
					continue
				}
				c.size = size + c.descriptorBytes
				// Where a held read showed growth (orderHeld), at a reading at or above the urgent
				// share, the read at the mark keeps it: the window at the mark can hold no earlier
				// read of a process the band read last more than growthWindow before.
				held := c.growth
				g.keepSize(now, &c)
				c.growth = max(c.growth, held)
				grew = grew || c.growth > 0
			}
		}
		kept = append(kept, c)
	}
	g.held = kept
	return grew
}

// reidentify reads again the exe link and the cmdline of p, a held process the band's scan read
// and the reading at the mark reads the smaps of again (rereadHeld, readHeldNew), where the fork
// discount would apply to it by the identity that scan read: its parent in the band's table and p
// running its parent's executable with its parent's arguments (sameProgram, with cache). A process
// the band read between its fork and its exec, as Python's subprocess.Popen starts one, carries its
// parent's executable and arguments there, so the discount took a grower for a fork: at 18c3c5bb,
// `4g-latepool-djcvt` ended a grower with all of its size taken off (`less_parent=31051776`), and
// `4g-latepool-rjm8m` ended the probe's 2.4 GB parent first, 115.1 ms after the reading at the
// mark, while six growers the band had read so lived, each ranking at nothing. Such a process
// ranked at nothing at its band read, so the reads at the mark take it after the processes started
// since (readHeldNew). The reads come with the smaps read they follow, so they count against the
// budgets of the reads at the mark (half the interval, and up to one interval where the guard reads
// on), and a held process whose smaps the mark does not read again keeps the identity the band
// read. It keeps what it reads in p, so the discount (discountFork) and the kill's line take p as
// it runs now, and a process that has run another program since ranks by its whole size. It answers
// false where the exe link cannot be read, which leaves p out of this decision, as a read of its
// smaps that fails does: a process that has ended since holds no memory, and one whose exe cannot
// be read is one a scan counts unread and the guard never ends (unreadOrEnded); the assessment of
// the reading reads it again. The arguments are best read, as a scan reads them (readProcess):
// where they cannot be read, p has none, which no discount matches.
func (g *Guard) reidentify(p *Process, cache *argsCache) bool {
	parent, listed := g.heldTable.byPID[p.PPID]
	if !listed || !g.sameProgram(p, parent, cache) {
		return true
	}
	dir := filepath.Join(g.proc, strconv.Itoa(p.PID))
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return false
	}
	p.Exe, p.Args, p.argsWhole = exe, nil, false
	buf := g.cmdlineBuffer()
	if cmdline, err := readHead(filepath.Join(dir, "cmdline"), buf); err == nil {
		p.Args, p.argsWhole = splitCmdline(cmdline), len(cmdline) < len(buf)
	}
	return true
}

// cmdlineBuffer is the guard's buffer for a process's first classifierArgsBytes of arguments
// (cmdline), made at its first use.
func (g *Guard) cmdlineBuffer() []byte {
	if g.cmdline == nil {
		g.cmdline = make([]byte, classifierArgsBytes)
	}
	return g.cmdline
}

// The tiers of the held processes' reads again at the mark (rereadOrder), in the order the guard
// reads them: those that grew in the band, those new to the stay that held memory of their own at
// their band read, and those it does not read again.
const (
	grewInTheBand = iota
	newToTheStay
	notReread
)

// rereadOrder is c's place in the order the reads again at the mark take (rereadHeld): its tier,
// and for a held process that grew in the band, which is first, its growth across the stay, its
// held size less the least of its band reads, by which those are read, the most grown first. One
// new to the stay that held memory of its own at its band read follows, read the newest by start
// time first. One new to the stay that ranked at nothing at its band read, a fork whose parent's
// size covered its own, is read after the processes started since (readHeldNew, INFRA-3531 R1 of
// spec version 38), and the reads again do not take it. One with two band reads or more that never
// grew between them is not read again.
func (g *Guard) rereadOrder(c candidate) (tier int, growth uint64) {
	switch stay := g.stays[procKey{pid: c.PID, start: c.StartTime}]; {
	case stay.grew:
		return grewInTheBand, c.size - min(stay.least, c.size)
	case c.newToStay && !c.nothingAtBand:
		return newToTheStay, 0
	}
	return notReread, 0
}

// watch decides on a reading at or above the mark while the guard waits after the kills of a
// reading, by the rule of INFRA-3531 R1 of spec version 25: one kill, or, at a reading at or above
// the urgent share, the kills of every process that grew over the window (endAt, spec version 39).
// It reads the sizes of what it may end at each reading, so that their order and their growth
// since the kill (grownSinceKill) are known whenever it decides. While a killed process has not
// exited it is left out of what the guard may end, of their sum against the floor and of the
// growth since the kill (leaveOut): the guard has ended it, and never ends or reports it again.
// While a killed process has not exited and the wait has not passed, it ends nothing, because the
// reading can fall below the mark as the rest of the process's memory goes. Once every killed
// process has exited or the wait has passed, it ends the next in the order (act), at once, where
// the reading has fallen, since the kills freed something and the pod is still at the mark, and
// where it has not fallen but a process it may end has grown since the kill and those processes
// hold at least the floor between them, since a kill that frees less than one interval's rise
// leaves the pod rising toward the kernel's kill point. Where neither holds it ends nothing and
// decides again at the next reading, until the wait has passed; then it gives the rise up with
// reason not fallen, whichever of the two failed, since the kill's freeing nothing is why it
// stops. So a rise that is a protected process's own, which no kill lowers and in which nothing
// the guard may end grows, costs at most one kill.
func (g *Guard) watch(now time.Time, s Settings, reading, limit, mark uint64) {
	w := &g.wait
	if !w.exited {
		// Each handle is asked before the scan, so a process that exits after the scan read it
		// is still left out of this reading's decision.
		w.exited = true
		for i := range w.kills {
			k := &w.kills[i]
			k.exited = k.exited || k.handle.Exited()
			w.exited = w.exited && k.exited
		}
	}
	w.fallen = w.fallen || fell(w.reading, reading, mark)
	passed := !now.Before(w.deadline)
	a, ok := g.assessAt(now)
	if !ok {
		return
	}
	for _, k := range w.kills {
		if !k.exited {
			a.leaveOut(k.key)
		}
	}
	// At every reading of the wait, so that a process first read after a kill has its first read
	// kept. It answers whether one grew since the previous kill, the go-on, and sets each one's
	// growth since the stay's first kill, the order's third tier (rank), so the processes are
	// ranked again by it.
	grown := g.grownSinceKill(now, a.endable, a.table)
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

// urgentShare is the reading at and above which a kill ends every process the guard may end that
// grew over the window, and not only the first in the order (endAt, INFRA-3531 R1 of spec version
// 39): the mark at act percent of limit plus half the distance from the mark to limit, 92.5% of
// limit at the default act share of 85. It derives from the act share, so it has no setting of its
// own.
func urgentShare(limit uint64, act int) uint64 {
	mark := share(limit, act)
	return mark + (limit-mark)/2
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
// sizes (sizesReading). The kills, one or, at a reading at or above the urgent share, every
// process that grew over the window, are endAt's.
func (g *Guard) act(now time.Time, s Settings, reading, limit uint64, a *assessment) {
	if len(a.endable) == 0 || a.endableBytes < share(limit, s.FloorPercent) {
		g.stopRise(now, reasonNothingToEnd, reading, limit, a)
		return
	}
	if g.endAt(now, s, reading, limit, a.endable, sizesReading, "") {
		g.keepKillSizes(now, a.endable, nil)
	}
}

// endAt ends cs[0], the first of cs, the processes the guard may end at the reading taken at now
// in its order of ends (rank), and, where the reading is at or above the urgent share
// (urgentShare), each other of cs that grew over the window, so every process that grew over it
// ends at that reading, each with its own ended line and its own report: near the limit one kill a
// reading cannot absorb a burst of several growers (INFRA-3531 R1 of spec version 39). This issue's
// tester measured, at 8023d298, a reading 148 ms late on a 1.5-CPU pod let six growers' first 64
// MiB steps land before the first reading at the mark, which read 93.88% of the limit, and one kill
// a reading then left the pod 2.8 MiB from the kill point (`4g-t4latepool-r2`). rank puts the
// processes that grew over the window first, so those are a run of cs from its first; where none
// grew, endAt ends one process. The first keeps the rule that put it first (candidate.rule), and
// each other the urgent rule (ruleUrgent); each carries sizes and newRead, the same decision's.
// The floor was read on cs as a whole, once, by the caller. Each kill is signalKill's, and one that
// it does not send, of a process that ended or whose pid another process took, is left out: where
// the first is not sent endAt sends none and answers false, so the caller decides again. Once the
// first is sent, the wait after the kills starts (watch): it lasts while one of them has not
// exited, and no longer than the wait setting.
func (g *Guard) endAt(now time.Time, s Settings, reading, limit uint64, cs []candidate, sizes, newRead string) bool {
	h := g.signalKill(cs[0])
	if h == nil {
		return false
	}
	at := g.clock.Now()
	counter := g.readCounter(at)
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	g.reportKill(cs[0], cs[0].rule(), sizes, newRead, reading, counter, limit, shmem, at)
	g.phase = waiting
	g.wait.kills = append(g.wait.kills[:0], killedProc{handle: h, key: procKey{pid: cs[0].PID, start: cs[0].StartTime}})
	g.wait.reading, g.wait.deadline, g.wait.exited, g.wait.fallen = reading, now.Add(s.Wait), false, false
	if reading < urgentShare(limit, s.ActPercent) {
		return true
	}
	for i := 1; i < len(cs) && cs[i].growth > 0; i++ {
		if h := g.signalKill(cs[i]); h != nil {
			g.reportKill(cs[i], ruleUrgent, sizes, newRead, reading, counter, limit, shmem, g.clock.Now())
			g.wait.kills = append(g.wait.kills, killedProc{handle: h, key: procKey{pid: cs[i].PID, start: cs[i].StartTime}})
		}
	}
	return true
}

// signalKill sends SIGKILL to target through a pidfd and answers the pidfd, nil where it sent no
// kill. Before the kill it reads nothing of the pod but target's stat, for its start time. A
// process that ended (exited, whether its parent has reaped it yet or not), or whose pid another
// process took, since it was read is not killed and not reported, and the guard decides again: at
// this reading where the band's held reads named it (endHeld), and otherwise at the next reading,
// whose scan leaves out a zombie. One that exits in the instant between the handle's Exited and its
// Kill is still killed and reported, since no read can close that window. A process the kill fails
// on for any other reason is logged once and counted unread from then on (noteKillFailure), so the
// next decision ends the next in the order.
func (g *Guard) signalKill(target candidate) Handle {
	h, err := g.signal.Open(target.PID)
	if err != nil {
		g.killFailed(target.Process, err)
		return nil
	}
	if start, err := readStartTime(g.proc, target.PID); err != nil || start != target.StartTime {
		_ = h.Close()
		return nil
	}
	// An exited process its parent has not reaped yet keeps its pid and its start time, so the
	// check above passes it, and the kill of it answers no error. The handle is asked because it
	// holds the very process the kill goes to, so its answer is of that process and of no other.
	if h.Exited() {
		_ = h.Close()
		return nil
	}
	if err := h.Kill(); err != nil {
		_ = h.Close()
		g.killFailed(target.Process, err)
		return nil
	}
	return h
}

// reportKill logs the ended line of target, killed at at, and reports it, with target's size as
// the guard read it and reading, the reading taken at the kill, rule, the rule that ended it,
// sizes, where the sizes it was chosen by came from, and newRead, what the reads at the mark read
// of the processes new to the stay for a kill they decided (newRead.field), "" for any other; and
// counter and shmem, read once for the kills of a reading.
func (g *Guard) reportKill(target candidate, rule, sizes, newRead string, reading, counter, limit, shmem uint64, at time.Time) {
	g.writeKill(Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			PID: target.PID, Executable: processName(target.Process), SizeBytes: target.size,
			ReadingBytes: reading, CounterBytes: counter, LimitBytes: limit, At: at, Cause: shimwire.MemoryKillCauseGuard,
		},
		Role: target.role, Rule: rule, Sizes: sizes, LessParent: target.lessParent, NewRead: newRead,
	}, shmem)
}

// killFailed acts on a kill that failed: a system with no way to hold or signal a process turns
// the guard off, a process that has ended (ESRCH) leaves the guard to decide again (signalKill),
// and any other failure is noted against the process (noteKillFailure).
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
// line reads. newToStay is whether it is new to this stay in the band (INFRA-3531 R1 of spec
// version 31): read at most once since the band's second-to-last reading, counting the reads of the
// readings before the one decided on alone, which the guard counts as fewer than two readings of
// the stay in a row that read it (keepSize, orderHeld): one first read at the band's last reading
// is new at the reading at the mark, and no longer at the next one, as one first read at a reading
// after a kill is new at the reading after it; one the reading at the mark finds started since the
// band's last reading is (startedSince); and every process is where no reading of the stay came
// before the one decided on. Of the held processes at the reading at the mark, which decides
// before it reads every size, the order takes as new those alone that a read at that reading
// reached (settleNew). nothingAtBand is, for a held process new to the stay at the reading at the
// mark, whether it ranked at nothing at its band read, a fork whose parent's size covered its own
// (orderHeld): the reads at the mark take those after the processes started since (readHeldNew).
// sinceFirstKill is how much it grew since the first kill of this stay in the band
// (sinceStayKill), 0 under growthNoise and wherever the stay has had no kill. descriptorBytes is
// the part of size that the memfds it holds by a descriptor alone make up (addHeldMemfds), which a
// read of its smaps again at the mark adds as it was counted (rereadHeld). lessParent is how much
// of size the tiers that rank by size do not count, for a fork that has not run a new program
// (discountForks), and 0 for every other process.
type candidate struct {
	*Process
	role            string
	size            uint64
	growth          uint64
	newToStay       bool
	nothingAtBand   bool
	sinceFirstKill  uint64
	descriptorBytes uint64
	lessParent      uint64
}

// rankSize is the size c ranks by where its tier ranks by size: its size less its parent's where
// it is a fork that has not run a new program (lessParent).
func (c *candidate) rankSize() uint64 { return c.size - c.lessParent }

// inNewTier is whether c is in the order's tier of the processes new to the stay: new to it, and
// holding memory of its own, its rank size above nothing (INFRA-3531 R1 of spec version 34). A
// fork whose parent's size covers its own is not a process the guard has learned anything from,
// and the tiers below rank it, by its growth since the stay's first kill or its rank size.
func (c *candidate) inNewTier() bool { return c.newToStay && c.rankSize() > 0 }

// discountForks sets the lessParent of each of cs, the processes the guard may end at a decision
// whose table is t (INFRA-3531 R1 of spec version 33). A process that runs its parent's executable
// with its parent's arguments (sameProgram), its parent being in t and read by the guard in this
// stay, is a fork that has not run a new program: under gVisor it reports the pages it shares with
// its parent as its own, so a sleeping fork of a large parent reads as large and its end frees
// nothing. Its lessParent is its parent's size at the guard's latest read of the parent, the
// newest of the parent's reads that sizes keeps, or its own size where that is less, so it ranks
// near nothing where it only shares its parent's pages, at nothing where it reads less than its
// parent, and never past every other process, as a size less a larger parent's would wrap to, and
// by what it holds over them where it grows. Every other process has none: one whose parent the
// guard has not read in this stay (a protected parent, or one no reading of the stay could read),
// and one that runs another executable or the same with other arguments, which has run a program
// of its own. At 23fdd92f, in 2 of 7 latepool runs at 4 GiB, the new tier ended first one of three
// sleeping forks of a 2.4 GB parent that started between the band's last two readings, or after
// the last, beside six growers that started after them (`4g-latepool-v5s8l`, `4g-latepool-6gd5t`).
// Growth over the window and since a kill are measured on each process's own sizes, and the floor
// and the reading count every size whole: the discount orders, and changes nothing else.
func (g *Guard) discountForks(cs []candidate, t *Table, cache *argsCache) {
	for i := range cs {
		g.discountFork(&cs[i], t, cache)
	}
}

// discountFork sets c's lessParent, as discountForks does.
func (g *Guard) discountFork(c *candidate, t *Table, cache *argsCache) {
	c.lessParent = 0
	parent, listed := t.byPID[c.PPID]
	if !listed || !g.sameProgram(c.Process, parent, cache) {
		return
	}
	r := g.sizes[procKey{pid: parent.PID, start: parent.StartTime}]
	if len(r.window) > 0 {
		c.lessParent = min(r.window[len(r.window)-1].size, c.size)
	}
}

// argsCache is the cmdline files one decision has read whole, by process, nil for one it could
// not read (sameProgram): a pool's parent is read once however many of its forks are compared with
// it. It is made at the decision's first such read.
type argsCache map[procKey][]byte

// sameProgram is whether p runs parent's executable with parent's arguments: both executables read
// and equal, and both argument lists read and equal whole. An exe link the scan could not read, or
// arguments it read none of, are no match. The scan read the first classifierArgsBytes of each
// list alone, and a list that filled them, of that length or longer, may be cut: where the two
// heads are equal and either filled them, it reads both cmdline files whole now and compares them,
// gVisor and Linux answering the whole list, and a list it cannot read whole is no match. Only that
// case reads more, so the discount sets no bound on the arguments' length (INFRA-3531 R1 of spec
// version 33). cache keeps each file it read whole for the rest of the decision.
func (g *Guard) sameProgram(p, parent *Process, cache *argsCache) bool {
	if p.Exe == "" || p.Exe != parent.Exe || len(p.Args) == 0 || !slices.Equal(p.Args, parent.Args) {
		return false
	}
	if p.argsWhole && parent.argsWhole {
		return true
	}
	own := g.wholeArgs(p, cache)
	return own != nil && bytes.Equal(own, g.wholeArgs(parent, cache))
}

// wholeArgs is p's cmdline read whole, or nil where it cannot be read, from cache where this
// decision has read it before (sameProgram).
func (g *Guard) wholeArgs(p *Process, cache *argsCache) []byte {
	key := procKey{pid: p.PID, start: p.StartTime}
	if data, read := (*cache)[key]; read {
		return data
	}
	if g.argsRead != nil {
		g.argsRead(p.PID)
	}
	data, err := os.ReadFile(filepath.Join(g.proc, strconv.Itoa(p.PID), "cmdline"))
	if err != nil {
		data = nil
	}
	if *cache == nil {
		*cache = argsCache{}
	}
	(*cache)[key] = data
	return data
}

// rule is the tier of the order of ends (rank) that puts c first where it is first, which the
// ended line names: ruleGrew where it grew over the window, else ruleNew where it is in the tier of
// the processes new to the stay (inNewTier), else ruleGrewSinceKill where it grew since the rise's
// first kill, else ruleLargest.
func (c *candidate) rule() string {
	switch {
	case c.growth > 0:
		return ruleGrew
	case c.inNewTier():
		return ruleNew
	case c.sinceFirstKill > 0:
		return ruleGrewSinceKill
	}
	return ruleLargest
}

// assessment is one scan as the guard acts on it: the processes it may end, in the order it ends
// them (rank), and their sizes' sum. leftOut is the pids of the processes leaveOut took out of
// them, and leftOutBytes their sizes' sum.
type assessment struct {
	table        *Table
	endable      []candidate
	endableBytes uint64
	leftOut      []int
	leftOutBytes uint64
}

// leaveOut takes the process key out of a, a process the guard has killed and which has not
// exited: out of the processes it may end, the order of the rest unchanged, so that the next it
// ends is another; and out of their sum, so that the floor counts only what it may still end. A
// process the guard has killed is so never killed or reported twice while it lingers. It is kept
// in leftOut for the stopped line alone (stopRise).
func (a *assessment) leaveOut(key procKey) {
	for i, c := range a.endable {
		if c.PID == key.pid && c.StartTime == key.start {
			a.endableBytes -= c.size
			a.endable = slices.Delete(a.endable, i, i+1)
			a.leftOut = append(a.leftOut, c.PID)
			a.leftOutBytes += c.size
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
	// addHeldMemfds ranked them by size alone; keepSizes sets the growth and whether each is new
	// to the stay, which rank orders by first.
	g.keepSizes(now, a.endable, t)
	var cache argsCache
	g.discountForks(a.endable, t, &cache)
	rank(a.endable)
	for _, c := range a.endable {
		a.endableBytes += c.size
	}
	g.logUnread(t)
	return a, nil
}

// keepSizes keeps the size of each of cs as read at now, the processes the guard may end at a
// reading whose table is t, and sets each one's growth and whether it is new to the stay
// (keepSize). A process t lists that this reading did not read, its smaps or its executable
// unreadable at it, or one the guard may no longer end, keeps its window reads, so its growth over
// the window counts again once it is read within growthWindow of them, and its count of readings
// in a row restarts at none, so it is new to the stay again (INFRA-3531 R1: growth is a process's
// size less its oldest read within the window, and new to the stay is read at most once since the
// band's second-to-last reading, which counts reads). The reads of a process t no longer lists,
// one that has ended or whose pid another process took (a read is kept by pid and start time),
// are dropped, so the reads kept are bounded by the processes the pod runs and the readings one
// window holds; keepSize drops a read older than the window when it next reads the process. Each
// process's reads keep their storage from one reading to the next, so the reads of a process the
// guard has read before allocate nothing.
func (g *Guard) keepSizes(now time.Time, cs []candidate, t *Table) {
	for i := range cs {
		g.keepSize(now, &cs[i])
	}
	for key, r := range g.sizes {
		switch {
		case r.window[len(r.window)-1].at.Equal(now):
		case !lists(t, key):
			delete(g.sizes, key)
		case r.readings != 0:
			r.readings = 0
			g.sizes[key] = r
		}
	}
}

// keepSize keeps c's size as read at now, sets c's growth, its size less its size at its oldest
// read kept within growthWindow before now where that is growthNoise or more, and answers whether
// it grew. A read older than the window counts for nothing and is dropped. A read already kept at
// now, the held processes' read at the mark (rereadHeld) where the assessment of that reading
// follows it, is replaced: a process has one read at each reading. A process with no earlier read
// in the window has not grown: a fresh fork reads its parent's shared pages as its own.
//
// It counts the reading as one more that read c (procReads.readings) and sets c's newToStay where
// fewer than two readings before now read it in a row: none, or only the one before. A reading
// that does not read a process its table still lists restarts its count (keepSizes), and the
// guard drops all of them wherever it reads none (forgetSizes), where a stay ends, so the count is
// of the readings of this stay, and two of them in a row that read c are the band's last two at
// the reading at the mark, and the two before the one decided on at any later reading of the rise.
func (g *Guard) keepSize(now time.Time, c *candidate) bool {
	key := procKey{pid: c.PID, start: c.StartTime}
	r := g.sizes[key]
	reads := r.window[:copy(r.window, r.window[windowStart(r.window, now):])]
	if n := len(reads); n > 0 && reads[n-1].at.Equal(now) {
		reads = reads[:n-1]
		r.readings--
	}
	c.newToStay = r.readings < 2
	c.growth = growthOver(reads, c.size)
	g.sizes[key] = procReads{window: append(reads, sizeRead{at: now, size: c.size}), readings: r.readings + 1}
	return c.growth > 0
}

// keepStays counts each of cs's reads at the armed guard's reading in the band taken at now, whose
// table is t, as one of its stay in the band (stays), and keeps whether it has grown by growthNoise
// or more between two of them: above the least of its earlier reads of the stay, so a grower whose
// steps are wider apart than the window, or slower than growthNoise from one reading to the next,
// counts. A process t lists that this reading did not read keeps its stay as it was: a read that
// failed between two reads of the stay does not undo the growth between them (INFRA-3531 R1:
// "grew in the band" is by two of its reads in the band). The stays of each process t no longer
// lists are dropped, so those kept are bounded by the processes the pod runs, and a process read
// before allocates nothing.
func (g *Guard) keepStays(now time.Time, cs []candidate, t *Table) {
	for _, c := range cs {
		key := procKey{pid: c.PID, start: c.StartTime}
		stay, known := g.stays[key]
		if known && c.size >= stay.least+growthNoise {
			stay.grew = true
		}
		if !known || c.size < stay.least {
			stay.least = c.size
		}
		stay.seen = now
		g.stays[key] = stay
	}
	for key, stay := range g.stays {
		if !stay.seen.Equal(now) && !lists(t, key) {
			delete(g.stays, key)
		}
	}
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

// forgetSizes drops every size read the guard keeps, the processes it holds for the next reading
// (held), the stays in the band and the sizes as of the stay's first kill (atFirstKill), wherever
// it reads none: below the band, while off and while a rise is given up (decide, stopRise). The
// reads it keeps are so those of readings in a row in the band, at the mark and after a kill, and a
// stay in the band starts again at the next armed reading in it, with no first kill. A reading
// below the mark that is in the band ends a rise and not its stay, and drops none of them.
func (g *Guard) forgetSizes() {
	clear(g.sizes)
	clear(g.stays)
	g.held, g.heldTable = nil, nil
	if g.firstKillKept {
		clear(g.atFirstKill)
		g.firstKillKept = false
	}
}

// keepKillSizes keeps the sizes as of a kill sent at the reading taken at now: of each of decided,
// the processes as the decision that sent the kill read them, its size there, and of each of
// after, the processes the guard may end as a read after the kill found them, that decided does
// not hold, its first read after the kill (INFRA-3531 R1: a process's growth since the kill is its
// size at this reading less its size at the reading the kill was sent at, or less its first size
// read after the kill for a process the guard first read then). act decides on the assessment of
// the reading, read before the kill, and has no after. A kill on the band's held reads (decide)
// keeps the held processes at the sizes endHeld decided on, each its held size, its read again or
// the search's read of it, and the assessment that follows the kill for the rest: a size read after
// the kill is not the size as of it, and at 7356e0f6, in `8g-latepool-q56hm`, a guard that kept
// that assessment's sizes counted the growers' step between the reading at the mark and its read
// after the kill as before the kill, saw no growth at the next two readings, not fallen and 28 MiB
// above the kill's reading, and waited.
//
// A held process whose program after the kill is not the one the decision read it running
// (sameIdentity) takes its first read after the kill: the band read it before its exec, and its
// size there was another program's. A grower the band read between its fork and its exec reports
// its parent's pages as its own under gVisor, as the probe's growers did at 18c3c5bb in
// `4g-latepool-rjm8m`, so its held size is its parent's, and a growth measured from it would show
// none until it outgrew its parent: the guard would not go on while it grew, and after a fall it
// would rank at nothing by growth since the stay's first kill. Each size is kept as the size as of
// that kill (atKill), from which grownSinceKill measures growth until the next kill, and at the
// first kill of the stay in the band as the size as of that one too (atFirstKill), from which
// sinceStayKill measures the order's third tier for the rest of the stay, across a dip below the
// mark that stays in the band. The sizes of the kill before were emptied when its wait ended
// (endWait), so those of one kill alone are kept, in the storage of the kills before.
func (g *Guard) keepKillSizes(now time.Time, decided, after []candidate) {
	first := !g.firstKillKept
	g.firstKillKept = true
	for _, c := range decided {
		g.keepKillSize(now, c, first)
	}
	for _, c := range after {
		k, kept := g.atKill[procKey{pid: c.PID, start: c.StartTime}]
		if !kept || !k.seen.Equal(now) || !sameIdentity(decided, c.Process) {
			g.keepKillSize(now, c, first)
		}
	}
}

// sameIdentity is whether p, read after a kill, runs the executable with the arguments the
// process of decided with its pid and start time ran as the decision read it.
func sameIdentity(decided []candidate, p *Process) bool {
	for _, d := range decided {
		if d.PID == p.PID && d.StartTime == p.StartTime {
			return d.Exe == p.Exe && slices.Equal(d.Args, p.Args)
		}
	}
	return false
}

// keepKillSize keeps c's size as its size as of the kill sent at now (atKill), and where first,
// that kill being the stay's first, as its size as of that one too (atFirstKill).
func (g *Guard) keepKillSize(now time.Time, c candidate, first bool) {
	key := procKey{pid: c.PID, start: c.StartTime}
	g.atKill[key] = killSize{size: c.size, seen: now}
	if first {
		g.atFirstKill[key] = killSize{size: c.size, seen: now}
	}
}

// grownSinceKill answers whether a process of cs, the processes the guard may end at the reading
// taken at now, whose table is t, has grown since the previous kill of the rise: its size is its
// size as of the kill (atKill) plus growthNoise or more, the go-on of watch. It sets each one's
// sinceFirstKill to its growth since the stay's first kill too (sinceStayKill). Growth since a
// kill, not over the window that orders the processes, is what it measures, so a process that
// grows in steps wider than the window counts as grown. A process with no size as of a kill, one
// first read after it, has that first read kept as its size as of the kill, and has not grown at
// it: a fork reads its parent's shared pages as its own. The sizes of each process t no longer
// lists are dropped (dropEnded), so those kept are bounded by the processes the pod runs, and a
// process read before allocates nothing.
func (g *Guard) grownSinceKill(now time.Time, cs []candidate, t *Table) bool {
	g.sinceStayKill(now, cs, t)
	dropEnded(g.atKill, t)
	grew := false
	for i := range cs {
		grew = keepGrowth(g.atKill, procKey{pid: cs[i].PID, start: cs[i].StartTime}, cs[i].size, now) > 0 || grew
	}
	return grew
}

// sinceStayKill sets the sinceFirstKill of each of cs, the processes the guard may end at the
// reading taken at now, whose table is t, to its growth since the first kill of this stay in the
// band, from its size as of that kill (atFirstKill), growthNoise or more, measured as keepGrowth
// measures it, so one first read after that kill has that first read kept and has not grown at it;
// and it drops the sizes of each process t no longer lists (dropEnded). That is the order's third
// tier (rank, INFRA-3531 R1 of spec versions 34 and 37). It answers whether the stay has had its
// first kill: before it no process has any such growth, and it sets and keeps nothing. decide
// measures it at each band reading after that kill and at the reading at the mark of each rise
// after it, and watch at each reading after a kill (grownSinceKill), so a dip below the mark that
// stays in the band keeps it: at 15bdbd3c, in `8g-latepool-r2`, `8g-latepool-r3` and
// `8g-latefork2-r1`, one grower was left, the reading went about 2 MB below the mark and came back,
// and a guard that dropped these sizes below the mark saw no growth since the dip and ended the
// probe's 4.8 GB parent first, which freed about 6 MB, and the grower at the next reading.
func (g *Guard) sinceStayKill(now time.Time, cs []candidate, t *Table) bool {
	if !g.firstKillKept {
		return false
	}
	dropEnded(g.atFirstKill, t)
	for i := range cs {
		c := &cs[i]
		c.sinceFirstKill = keepGrowth(g.atFirstKill, procKey{pid: c.PID, start: c.StartTime}, c.size, now)
	}
	return true
}

// dropEnded drops each size of kept, sizes as of a kill, whose process t, the table of the reading
// the guard decides on, no longer lists by its pid and its start time: it has ended, or its pid
// names another process. A process t lists that the reading did not read, its smaps or its
// executable unreadable at that reading (assess counts it unread; the reads at the mark leave it
// out of their decision), keeps its size, so its growth since the kill is measured from it when it
// is read again: a size dropped there would be taken again from that later read, after the growth
// it missed, and the process would count as not grown, behind a larger one that never grew and
// with no go-on for its growth (INFRA-3531 R1). At the reading at the mark t is the band's table
// (heldTable), which lists every held process; a process the search found started since has its
// size kept after the drop.
func dropEnded(kept map[procKey]killSize, t *Table) {
	for key := range kept {
		if !lists(t, key) {
			delete(kept, key)
		}
	}
}

// lists is whether t, the table of a reading, lists the process key by its pid and its start
// time: one t does not has ended, or its pid names another process. What the guard keeps of a
// process, its reads (keepSizes), its stay in the band (keepStays) and its sizes as of a kill
// (dropEnded), it drops for a process its reading's table no longer lists, and keeps for one it
// lists that the reading did not read (INFRA-3531 R1).
func lists(t *Table, key procKey) bool {
	p, listed := t.byPID[key.pid]
	return listed && p.StartTime == key.start
}

// keepGrowth is how much size, the size of the process key read at now, grew since the kill whose
// sizes kept holds, growthNoise or more, and 0 otherwise; a process kept holds no size of keeps
// size as its size as of the kill, its first read after it, and has not grown at it. It marks the
// process as read at now.
func keepGrowth(kept map[procKey]killSize, key procKey, size uint64, now time.Time) uint64 {
	k, known := kept[key]
	growth := uint64(0)
	switch {
	case !known:
		k.size = size
	case size >= k.size+growthNoise:
		growth = size - k.size
	}
	k.seen = now
	kept[key] = k
	return growth
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

// rank sorts cs in the guard's order of ends (INFRA-3531 R1 of spec version 31): the most grown
// over the window first (growth); then, of those that did not grow over the window, those new to
// this stay in the band that hold memory of their own (inNewTier), the largest first, a process
// new to the stay that ranks at nothing ranking in the tiers below (spec version 34); then the most
// grown since the first kill of this stay in the band (sinceFirstKill, set by sinceStayKill at each
// reading after that kill, and none before it; spec versions 34 and 37); then, of those that grew
// by none of these, the largest first; and the newest first among equal sizes: of a parent and the
// child it forked that has run a program of its own, which read the same, the child is the one
// that was started for the work. Where none of cs grew or is in the new tier, the largest is first,
// and where all of them are in it, as where no reading of the stay came before the one decided on,
// the order is by size. Each size it orders by is the rank size, the size less what discountForks
// takes off a fork that runs its parent's program (INFRA-3531 R1 of spec version 33). The ended
// line names the tier that put the first one first (candidate.rule).
//
// The tier of the processes new to the stay is there because a runaway can begin between two
// readings, or just before the band's last one, and then has no earlier read to grow from, or one
// alone: this issue's tester measured, at e3a1923c, the latepool probe at 4 GiB (six growers
// starting together while a parent and three sleeping forks of a shared block held the pod at 84%)
// end a process that shares the block, a sleeping fork or the parent, first in 7 of 7 runs, the
// closest run's highest reading 2.4 MiB below the kernel's kill point; and at 778c42de the run
// `4g-latepool-zrr4d` ended a sleeping fork first where its growers had started 5 ms before the
// band's last reading, which read them once, to a guard whose tier held only the processes the
// reading before had not read. The tier of growth since a kill is there because a throttled pod's
// readings come 170 to over 300 ms apart, so the window can hold one read of each process and show
// no growth at all: in the latepool and sharedpool probes under gVisor (INFRA-3531 T17), the guard
// then went on after a kill by ending the largest, a sleeping fork whose end frees nothing, while
// the growers lived. It measures from the stay's first kill and not the previous one, since
// growers that step every 250 ms in step with each other show no step in the 100 ms since the
// previous kill: at d22d8da3, in `4g-latepool-kxv5k` and `4g-latepool-zxhnw`, the guard then ended
// the probe's parent and a sleeping fork after a fall while the growers lived. And it measures from
// the stay's first kill and not the rise's, since a dip below the mark that stays in the band ends
// a rise and not the stay: at 15bdbd3c, in `8g-latepool-r2`, `8g-latepool-r3` and
// `8g-latefork2-r1`, one grower left after a dip of about 2 MB below the mark lost the first kill
// of the next rise to the probe's 4.8 GB parent, which freed about 6 MB.
func rank(cs []candidate) {
	slices.SortFunc(cs, func(x, y candidate) int {
		// Growth since the stay's first kill orders those that are not in the new tier; the ones
		// in it go by size alone.
		sinceFirstKill := 0
		if !x.inNewTier() && !y.inNewTier() {
			sinceFirstKill = cmp.Compare(y.sinceFirstKill, x.sinceFirstKill)
		}
		return cmp.Or(cmp.Compare(y.growth, x.growth), cmp.Compare(ordinal(y.inNewTier()), ordinal(x.inNewTier())),
			sinceFirstKill, cmp.Compare(y.rankSize(), x.rankSize()), cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
	})
}

// ordinal is b as a number that orders true after false.
func ordinal(b bool) int {
	if b {
		return 1
	}
	return 0
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
// The line names each process the assessment counted unread, by its pid and, where its smaps
// reads, its size there (unread_processes, INFRA-3531 R3 of spec versions 29 and 30), every pid of
// a.table.unread, those whose stat could not be read among them, in pid order: a process the guard
// could not read is one it never ends (assess), and the line says which they are and what each
// holds. A process whose stat reads Z, or whose stat did not read, is named by its pid alone, and
// its smaps is neither read nor counted in unread: a zombie leader's smaps reads 0 under gVisor
// while a live thread holds the process's memory, and `<pid>:0` would read as a size, and nothing
// counts that memory by the leader's smaps anyway. Under gVisor at 8f4ff73f such a process was one
// whose main thread had exited while another thread held its memory (stat Z with two threads, exe
// and ns/pid EACCES, smaps with no Anonymous), a non-dumpable process in the container's own user
// namespace (exe and smaps EACCES), and a process being killed, whose exe read EACCES for a few
// milliseconds.
//
// A stop that repeats, as a reading that holds near the mark goes below it and back above it,
// writes its line at most once in each stoppedEvery by the guard's clock: each stop in between is
// counted and left out, and the next line written carries that count (left_out). What the line
// alone needs, the smaps of every process the guard may not end and the list of those it could
// not read, is read and built only for a line written. The scan a stop is decided on, with the
// smaps of the processes the guard may end, is read at every rise, since it decides whether to act.
func (g *Guard) stopRise(now time.Time, reason string, reading, limit uint64, a *assessment) {
	defer g.releaseSmaps()
	g.phase = stopped
	g.forgetSizes()
	if !g.stoppedAt.IsZero() && now.Sub(g.stoppedAt) < stoppedEvery {
		g.stoppedLeftOut++
		return
	}
	var protected, unread uint64
	counted := make(map[int]bool, len(a.endable)+len(a.leftOut))
	for _, c := range a.endable {
		counted[c.PID] = true
	}
	for _, pid := range a.leftOut {
		counted[pid] = true
	}
	for pid := range a.table.byPID {
		if _, isUnread := a.table.unread[pid]; counted[pid] || isUnread {
			continue
		}
		if size, err := g.size(pid, nil); err == nil {
			protected += size
		}
	}
	var named []byte
	for _, pid := range slices.Sorted(maps.Keys(a.table.unread)) {
		if len(named) > 0 {
			named = append(named, ',')
		}
		named = strconv.AppendInt(named, int64(pid), 10)
		if p, listed := a.table.byPID[pid]; !listed || p.state == 'Z' {
			continue
		}
		if size, err := g.size(pid, nil); err == nil {
			unread += size
			named = strconv.AppendUint(append(named, ':'), size, 10)
		}
	}
	unreadProcesses := "none"
	if len(named) > 0 {
		unreadProcesses = string(named)
	}
	// The line says where the memory is, and a killed process that has not exited still holds
	// its own: it counts in endable here, though every decision leaves it out (leaveOut).
	endable := a.endableBytes + a.leftOutBytes
	unheld := int64(reading) - int64(endable+protected+unread)
	g.logf(stoppedLine, reason, reading, endable, protected, unread, unheld, limit, g.stoppedLeftOut, unreadProcesses)
	g.stoppedAt, g.stoppedLeftOut = now, 0
}

// endWait lets go of the processes a waiting guard holds, and of the sizes as of their kill
// (atKill): the wait ends at the next kill of the rise, which keeps its own, and when the rise
// ends. The kills keep their storage for the next.
func (g *Guard) endWait() {
	if g.phase != waiting {
		return
	}
	for _, k := range g.wait.kills {
		_ = k.handle.Close()
	}
	g.wait = afterKill{kills: g.wait.kills[:0]}
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
		start, end := nextWindow(g.window.end, now)
		g.window = window{start: start, end: end}
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

// nextWindow is the start and the end of the hour now falls in, counted in whole hours from end,
// the end of an hourly window that has passed: the guard's (Guard.rollWindow) and the guest
// watch's (Guest.hour) alike.
func nextWindow(end, now time.Time) (time.Time, time.Time) {
	start := end.Add(now.Sub(end).Truncate(time.Hour))
	return start, start.Add(time.Hour)
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
