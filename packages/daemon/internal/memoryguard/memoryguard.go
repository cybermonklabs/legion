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
// seconds for the hourly line, once the guard has decided on the reading (tick), and once for the
// kills of a reading, after every one of them is sent (reportKills), and never at each interval.
//
// At or above the act share of the limit (the mark), the guard acts on the processes it may end,
// by the sizes it reads of those alone, where their sizes sum to at least the floor share of the
// limit: it sends SIGKILL through a pidfd to the first of them in its order of ends, and, at a
// reading at or above the urgent share, the mark plus half the distance from the mark to the limit
// (92.5% at the default act share of 85), to every other of them that grew over the window, and,
// where the decision also holds one that is not new to this stay in the band, to every other that
// grew since the stay's first kill by growthNoise or more, that started during the stay, is not a
// fork and holds memory of its own, or that is new to the stay and holds memory of its own, too, so
// near the limit one kill a reading does not have to absorb a burst of several growers (endUrgent,
// endsInStay, INFRA-3531 R1 of spec versions 39, 41, 42 and 46): growers started between the band's
// last reading and the mark have one read each and no growth over the window, and at a82a9f2c one
// kill there left the pod 26.5 MiB from the kill point (`4g-latepool-4tpqx`). A stay in the band
// begins at its first reading at or above the band's floor, and a process started during it is one
// that stay's first process table did not list by its pid and start time (keepStayTable,
// startedInStay). The sets of growth since the stay's first kill and of starts during the stay are
// INFRA-3531 R1 of spec version 46, by INFRA-3368's architect's decisions of 2026-11-20T13:55Z,
// 14:00Z, 14:05Z and 14:58Z (`4g-r15latepool-r206`; endsInStay). A process that the stay's first
// table listed, grew in the band before the stay's first kill and has been flat since, as r206's
// 2.4 GB parent, is in neither set, and nor is a sleeping fork that started during the stay, an
// orphaned one included, since a process stays a fork while it runs the program its fork's read
// found.
// At the first reading at the mark the reads that choose the first kill share half an interval and
// can reach only some of the processes new to the stay; where the decision holds one that is not
// new, the guard reads on after its kills through the ones those reads did not reach, up to one
// interval from the reading, and ends each with memory of its own or grown since the stay's first
// kill as it reads it (readOn, INFRA-3531 R1 of spec versions 43 and 46): at 4d6665d5 a slow
// reading at the mark read one of six growers before its kill and ended it alone, and the host
// ended the pod (`4g-t4latepool-r3`). Where every process is new to the stay, as at the guard's
// first reading or after a reading that crossed the band within one interval, being new tells no
// burst from the rest of the pod, and the urgent share adds the window's growers alone, none of the
// three sets of the stay. A fork whose parent's size covers its own is not ended for being new to
// the stay or for starting during it. Then
// it takes a reading at each interval. The order (rank, INFRA-3531 R1 of spec version 31) is the
// most grown first: a process's growth is its size at this read less its size at its oldest read
// within the last growthWindow (300 ms), and growth under growthNoise
// (1 MiB) is none. Of those that did not grow over the window, those new to this stay in the band
// that hold memory of their own follow, the largest first: a process the guard has read at most
// once since the band's second-to-last reading, at the readings before the one it decides on, has
// no earlier read to grow from, or one alone, and can be a runaway that began between two readings
// or just before the band's last one (newToStay); a fork whose parent's size covers its own ranks
// at nothing, is not a process the guard has learned anything from, and ranks in the tiers below
// (inNewTier, INFRA-3531 R1 of spec version 34). The most grown since the first kill of this stay
// in the band follow, by growth since that kill as sinceStayKill measures it, also from
// growthNoise; before the stay's first kill no process has any. Those that grew by none of these
// follow, largest first, so where none grew and none is in the new tier the largest ends first.
// The tier of the new is there because this issue's tester measured, at e3a1923c, the latepool
// probe at 4 GiB (six growers starting together while a parent and three sleeping forks of a
// shared block held the pod at 84%) end a process that shares the block, a sleeping fork or the
// parent, first in 7 of 7 runs, the closest run's highest reading 2.4 MiB below the kernel's kill
// point; at 778c42de, where the tier held only the processes the reading before had not read, the
// run `4g-latepool-zrr4d` lost a sleeping fork first, its growers started 5 ms before the band's
// last reading. The tier of growth since the kill is there because a throttled pod reads 170 to
// over 300 ms apart, so the window can hold one read of each process: in the latepool and
// sharedpool probes under gVisor (INFRA-3531 T17) the guard went on after a kill by ending the
// largest, a sleeping fork, while the growers lived. It measures from the stay's first kill, not
// the previous one, since growers that step every 250 ms show no step in the 100 ms after the
// previous kill: at d22d8da3, `4g-latepool-kxv5k` and `4g-latepool-zxhnw` lost the probe's parent
// and a sleeping fork after a fall while the growers lived. Under gVisor each forked process
// reports the pages it shares with its parent as its own, and no smaps field tells them apart, so
// the largest can be a sleeping fork whose end frees nothing while another process fills the pod;
// for the same reason a process with no earlier read in the window, a fresh fork among them,
// counts as not grown. Where the tier of the new or the largest ranks by size, a process that runs
// its parent's executable with its parent's arguments, its parent read in this stay, is a fork
// that has not run a new program, and ranks by its size less its parent's at the guard's latest
// read of the parent, nothing where its size is less (INFRA-3531 R1 of spec version 33,
// discountForks): at 23fdd92f the new tier ended first a sleeping fork of a 2.4 GB parent in 2 of
// 7 latepool runs at 4 GiB. The parent itself ranks whole. Each kill's line names the tier that
// chose it, or the urgent rule for a process ended beside the first at an urgent reading, where
// the sizes it was chosen by came from and how much of its size its rank did not count (INFRA-3531
// R3 of spec version 31; lines.go).
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
// only then does it assess that reading, whose sizes are the window's newest reads (endHeld); a
// size as of the kill takes no read made after the kill but for a process the guard never read
// before it (keepKillSizes). Below the urgent share, where one of the held processes grew, it reads
// no smaps before the kill, and the kill's line and report carry the held size and that reading.
// Where none of them grew by the held reads, or at a reading at or above the urgent share, it reads
// again before the kill the smaps of the held processes that grew between two of their band reads
// of this stay or are new to it, keeps the rest at their held sizes, and orders them by those reads
// (rereadHeld): the step that takes the reading to the mark comes between the band's last reading
// and this one, and no held read shows it. Under gVisor the
// `shared` probe at 4 GiB, one grower stepping 64 MiB about every 250 ms beside three sleeping
// forks of a 1.5 GiB block, lost a sleeper first to a guard that decided on the held reads alone,
// since the grower's step before the crossing one was older than the window at the mark (INFRA-3531
// T17); a sleeping fork flat through the band is not read again, since its read changes no answer.
// With each smaps it reads again it reads again the executable and arguments of a held process the
// band read running its parent's program (reidentify): the band can read a process between its
// fork and its exec, as at 18c3c5bb in `4g-latepool-rjm8m`, where the probe's growers so read
// ranked at nothing as forks of the 2.4 GB parent and the parent ended first. A held process every
// read of which before the kill is a fork's read and whose newest read is no fork's, as where the
// band read it between its fork and its exec, has 0 as its size as of the kill (keepKillSizes, run
// r40 at 7512cc38), and so has one whose size as of the kill came from a fork's read and that is no
// fork at a later reading (keepGrowth). It reads the band's growers first, the most grown across
// the stay first, then those new to the stay that held memory of their own at their band read, the
// newest first (INFRA-3531 R1 of spec versions 31 and 38), for half the interval
// from the reading at the mark by its clock, the first of them always and none after the read in
// hand once that has passed, and those it has not read keep their held sizes: at 9d6d24df,
// `4g-latepool-lmnt5`'s six growers each had one band read, and reading all six again under the
// pod's CPU throttle put the first kill 100.68 ms after the reading at the mark. Where those
// reads show no growth either, or whatever they show at a reading at or above the urgent share,
// it looks for the processes started since the band's last reading, which no held read names
// (startedSince), and then reads the held processes new to the stay that ranked at nothing at
// their band read, forks whose parent's size covered their own (readHeldNew), each group the
// newest first, so the reads of sleeping forks do not spend the interval before a new process is
// read (INFRA-3531 R1 of spec version 38): at 43213c99, in `4g-latepool-rnr56`, three sleeping
// forks started inside the band were read first, each ranked at nothing, the search then read
// nothing, and the first kill went to their 2.4 GB parent 120.1 ms after the reading at the
// mark. At a reading at or above the urgent share where a held process grew, it sends the kill
// of the most grown before that search: a process the search finds has no growth over the window
// and cannot go before it, so the first kill waits for no search it does not need (INFRA-3531 R1
// of spec version 43).
// It takes the order's second tier as one set, the processes new to the stay that it read at
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
// reading at the mark has passed; and once it has, the reads that choose the first kill read
// nothing more past the budget. Where that interval passes with only forks that rank at nothing
// read, the tiers below the new decide. The budget governs those reads alone: at a reading at or
// above the urgent share in a stay with history, the read-on after the kills reads up to one
// interval from the reading at the mark, whatever they read (readOn).
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
// After the kills of a reading the guard reads the sizes at each interval too, and waits only
// while a process it killed there has not exited (each one's Handle's Exited), and no longer than
// the wait (INFRA-3531 R1 of spec versions 25 and 39). The reading has fallen when it is below the
// reading at the kills by any amount, or below the mark (fell). A process it has killed the guard
// never ends or reports again: while it has not exited it is left out of what the guard may end,
// of their sum against the floor and of the growth since the kill (leaveOut). Once every one of
// them has exited or the wait has passed, the guard decides at each reading. Fallen and still at
// or above the mark, it ends the next in the same order, so a pool of forked workers (make -j, a
// Lean build), whose shared pages no one worker's kill frees, is ended one worker at a time. Not
// fallen, it ends the next in the order at once, at that reading, where a process it may end has
// grown since the previous kill of the rise and those processes hold the floor between them.
// Growth since the kill decides this, not the window that orders the processes, and a process has
// grown once its size is its size as of the kill plus growthNoise or more (atKill, keepKillSizes,
// grownSinceKill). By INFRA-3368's architect's decision of 2026-11-20 on INFRA-3531 R1 of spec
// version 43, a process's size as of a kill is the lowest of its reads of the program it runs now,
// from its last read that finished before the kill's reading up to the decision's read: the
// assessment of that reading in act, at the mark its held size, its read again or the search's
// read of it before the kill, and at an urgent reading of the wait its read again before the first
// early kill (endKnown). A read is of another program than a later one only where it was a fork's
// read, the process running its parent's executable with its parent's arguments, its parent read
// in this stay, and the later one is not: R1's own fork test, not the executable and arguments
// alone, since an argument rewrite, as setproctitle or process.title makes, is not an exec
// (anotherProgram; INFRA-3368's architect's decision of 2026-11-20T08:13Z on INFRA-3531 R1 of spec
// version 43). Where every read of it before the kill is of another program, as where the band read
// it between its fork and its exec, its size is 0. A size as of a kill from a fork's read counts
// only while the process is a fork, and a process read as a fork stays one, its parent gone or
// not, until its executable or arguments differ from what that read found (discountFork;
// INFRA-3368's architect's answer of 2026-11-20T09:24Z): at a reading that finds it no longer
// one, as where it exec'd after the kill's reading, its size as of the kill is 0, and all that
// program holds is growth since the kill once it is growthNoise or more, as any growth is
// (keepGrowth; INFRA-3368's architect's ruling of 2026-11-20,
// dispatch://INFRA-3531/comment/a81dfa03-66d1-4f14-a0c8-d5ad7af91df9). Only a process the guard
// never read before the kill takes its first read after it, since a fork reports its parent's
// shared pages as its own. So a step that lands after a process's last read before the kill's
// reading counts as growth since the kill: at 7356e0f6, in `8g-latepool-q56hm`, the growers
// stepped between the reading at the mark and the read after its kill, and a guard that kept that
// read saw no growth at the next two readings, not fallen and 28 MiB above the kill's reading, and
// waited; at 0028eb7d, in r31, the growers stepped between a reading and act's reads at it, and a
// guard that kept those reads ended nothing at the next reading, 155 MiB above the reading at the
// kill; and at 7512cc38, in r40, a guard that kept the read after the kill of a grower the band
// read before its exec saw no growth since, and after a fall ended the probe's parent by
// rule=largest while the grower lived. So a process that grows in steps wider than the window
// still counts, and six growers that together outrun what one kill frees, which this issue's
// tester measured ending a 4 GiB pod while the guard waited the whole wait, are ended one at each
// reading below the urgent share, and at a reading at or above it every one that grew over
// the window, after a kill every one the guard read at the reading before that grew since that
// kill (endKnown, below), and, where the decision holds a process that is not new to the stay,
// every one that grew since the stay's first kill, started during the stay and is no fork with
// memory of its own, or is new to it with memory of its own: at 8023d298 this issue's tester
// measured one kill a reading leave the pod 2.8 MiB from the kill point after a first reading at
// the mark of 93.88% (`4g-t4latepool-r2`), at a82a9f2c 26.5 MiB after one at 93.95% that read six
// growers once each (`4g-latepool-4tpqx`), and at 00405eb3 2.3 MiB after a reading of the wait at
// 93.95% that ended one of five growers that started inside the band (`4g-r15latepool-r206`). At a
// reading at or above the urgent share where the processes it may end hold one that is not new to
// the stay, one of them of those three sets is a reason to end the next as growth since the kill
// is, under the same floor: the urgent rule ends it at that reading though it has not grown since
// the kill, so the processes new to the stay that the read-on at the mark did not reach end at the
// next such reading; where those sets are the one reason, the first kill goes to the first process
// in the order that the urgent rule ends, so that no process outside them, as a parent flat since
// before the stay's first kill, ends for that reason (urgentFirst, INFRA-3531 R1 of spec versions
// 43 and 46, verification version 36). At a reading at or above the urgent share, once every
// killed process has exited or the wait has passed, the guard first reads again, one at a time,
// the smaps of the processes it read at the reading before, in the order that reading ranked them,
// and ends each one that grew since the previous kill as soon as it reads it, the first by its
// tier, where those processes hold the floor with the sizes read again, and the rest by the urgent
// rule, one read before the first, at whose read they did not hold the floor, with that first
// kill; with that first kill it ends too each process that the urgent rule ends, the three sets of
// the stay where one of the processes it is to read again is not new to it and still runs, which
// it knows before its first read (urgentEnds): at once those it has read, and each it reads after
// it as it reads it. Only then does it list the processes, and that full read takes those reads in
// place of its own and decides only the rest, the urgent rule's kills (watchUrgent, endKnown,
// INFRA-3531 R1 of spec versions 43, 46 and 48, in the order verification version 36 states;
// INFRA-3368's architect, 2026-11-21T08:54Z, item 1). In the tester's run r17 at 41d4f45b the full
// read of the reading at 95.16%, after two readings that showed nothing grown since the kill, took
// 205 ms before the guard ended the four growers left, and the reading came 12.3 MiB from the 4 GiB
// kill point. Where none of these holds, the guard ends nothing and decides again at the next
// reading, until the wait has passed; then it gives the rise up with reason not fallen, so a rise
// that is a protected process's own, Oh My Pi's, in which nothing it may end grows, costs at most
// one kill below the urgent share (INFRA-3531 R1 of spec version 43); at or above it, in a stay
// with history, it can also cost each process of those three sets, which the urgent rule ends at
// the reading of that kill or at a later reading of the wait at or above that share. A rise given
// up, for that reason or because no process may be ended, has the guard log the sizes it saw and
// end nothing more for that rise, until the reading has gone below the mark; a stop that repeats is
// logged at most once a minute (stopRise).
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
// In this tier alone, a process the guard never read before that kill counts from firstReadSince's
// size: from 0 where its first read after that kill is no fork's read, and from that first read
// where it is one, its parent read at that reading before or after it (firstReadSince;
// INFRA-3368's architect's decision of 2026-11-20T08:13Z on INFRA-3531 R1 of spec version 43, and
// its rule of 2026-11-21T16:43Z, item 1, by which the flag is that read's own): at 28732f65, in
// `8g-latepool-r2`, the guard first read five of six growers after the stay's first kill, kept
// those reads, saw none grown since, and its third kill ended the probe's 4.8 GB parent by
// rule=largest while four of them lived. The go-on keeps that first read, so below the urgent
// share a process that never grew still cannot make the guard go on.
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Settings are what a guard runs by: the act share and the floor share of the limit, in percent,
// the act share setting the urgent share too (urgentShare), the longest wait after the kills of a
// reading for the killed processes to exit, after which the guard decides at each reading and,
// with the reading not fallen, nothing grown since the kills and, at a reading at or above the
// urgent share in a stay with history, no process of the sets the urgent rule ends for the stay
// (urgentFirst, endsInStay), gives the rise up (watch), and the time between two readings.
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
// every direct child of the shim. The classifier marks them by their place in the tree, the agent
// and the shim's other direct children (the secrets renewer, the working copy's adoption, the
// CodeGraph warm-up) alike, a superset of the agent whose only cost is a kill left unnamed, never
// a wrong name (Guest). Oh My Pi's helpers are not session processes, since one can end alone. The guest
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
// ruleNew, ruleGrewSinceKill or ruleLargest), or ruleUrgent for a process the urgent rule ended at
// a reading at or above the urgent share: beside the first, one that grew over the window or,
// where the decision holds a process that is not new to the stay, one of the sets the urgent rule
// ends for the stay, grown since its first kill, started during it and no fork with memory of its
// own, or new to it with memory of its own (endUrgent, endsInStay, and readOn after the first kill
// at the mark, INFRA-3531 R1 of spec versions 39, 41, 42, 43 and 46), or, at such a reading after a
// kill, one the guard read at the reading before that grew since that kill (endKnown, spec version
// 43); and the first kill of a reading of the wait where those sets are the one reason to act and
// the order puts another process first (urgentFirst, spec version 46). Sizes is where the sizes it
// was chosen by came from (sizesBand, sizesReread, sizesSearch or sizesReading), each ruleKernel and
// sizesKernel for a process the guest kernel killed (INFRA-3531 R3 of spec version 31). LessParent
// is how much of its size its rank did not count, as a fork that runs its parent's program
// (candidate.lessParent), 0 for every other process and every kill of the guest kernel (INFRA-3531
// R1 of spec version 33).
// NewRead is what the reads at the reading at the mark read of the processes new to the stay, for
// a first kill those reads decided (newRead.field), and the same for each kill of the urgent rule
// beside it, which carries the first kill's: how many they read, and of those how many hold memory
// of their own, the read-on's reads among them (readOn); "" for every other kill, which the line
// writes as `-`. The ended line carries the four (writeKill); the report on the wire carries none
// of them.
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
	// waiting has sent the kills of a reading, one or, at a reading at or above the urgent share,
	// beside the first every process that grew over the window, after a kill every one it read at
	// the reading before that grew since that kill, and, where the decision holds a process that
	// is not new to the stay, every one of the sets the urgent rule ends for the stay (act,
	// endUrgent, urgentEnds, readOn, endKnown), and reads those sizes while a process it killed
	// there has not exited and the wait has not passed, and from then on decides at each reading
	// whether to end the next (watch).
	waiting
	// stopped has given up on this rise, reads no size and acts on nothing until the reading is
	// below the mark.
	stopped
)

// afterKill is the kills a waiting guard watches, all sent at one reading: each process killed
// (kills), whose exits end the wait, and which the guard never ends again while it has not exited
// (leaveOut); the reading at the kills, which the reading must fall below (fell), and when the
// guard's clock took it (at), after whose first kill a read at that reading is a read after the
// kill (keepSize); and when the wait passes at the latest. Its kills keep their storage from one
// kill to the next (endWait).
type afterKill struct {
	kills    []killedProc
	reading  uint64
	at       time.Time
	deadline time.Time
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

// sentKill is one kill sent at a reading (sendFirst, sendUrgent), before its line and report
// (reportKills): the process as the decision read it, the rule its line names, and the guard's
// clock at its send (Kill.At).
type sentKill struct {
	target candidate
	rule   string
	at     time.Time
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
//
// The rest is what its size as of a kill sent at its newest read's reading is taken from
// (asOfKill), each read with whether it was a fork's read: before, its last read at a reading
// before that one, where it has one (readBefore); and atReading, its reads at that reading in the
// order the guard made them, the newest last, of which the first beforeKill came before that
// reading's first kill, with programs, the program each of them found, and parents, what each
// found of the process's parent (readParent), both in step with atReading. A read of atReading
// holds the flag discountFork set on it; its own flag by the fork-flag rule is read's, which
// programs and parents give. A later pass of that reading that reads the parent after the process,
// as the full read after a kill, flags only its own read of the process, so a read made before its
// parent's first read has no flag from discountFork, and is a fork's read all the same where it
// found its parent's program and that parent has a read at that reading, or where it found the
// program of a read there that is one. For a process the guard first read after the stay's first
// kill, atReading is also what firstReadSince takes its size as of that kill from, and firstTaken
// is whether atFirstKill holds the size firstReadSince took from these reads (takeFirstSince).
// forkProgram is the executable and arguments its latest fork's read by the fork test found it
// running, discountFork's or settleForks's, an empty executable where it has had none in this
// stay: while it runs that program, a later read of it is a fork's read too (discountFork).
type procReads struct {
	window      []sizeRead
	readings    int
	before      forkRead
	readBefore  bool
	firstTaken  bool
	atReading   []forkRead
	programs    []program
	parents     []readParent
	beforeKill  int
	forkProgram program
}

// readParent is what one read of a process's atReading found of the process's parent
// (procReads.parents), beside its size, the flag discountFork set on it and the program it found,
// and not in forkRead, which is also a size as of a kill (atKill) that the tests compare whole.
// key is the parent, by its pid and its start time, whose program the read found by discountFork's
// own test (sameProgram), the parent as the table of that read lists it, and the zero key where the
// read did not find its parent's program: sameProgram reads both argument lists whole where the
// scan's heads were cut, so its answer is kept with the read, and not worked out again later from
// heads that may be cut. readThere is whether that parent has a read at the reading of the read,
// set at that reading's last pass (settleForks): a read made before its parent's first read in
// this stay has no flag from discountFork, and is a fork's read by the fork-flag rule once the
// parent is read at the same reading, by a later pass (INFRA-3368's architect's rule of
// 2026-11-21T16:43Z on INFRA-3531 R1 of spec version 48, item 1; row (2) of asOfKill's table). A
// parent's read at a later reading makes it no fork's read, and the parent's newest read moves on
// at that later reading, so the answer is kept with the read, at its own reading.
type readParent struct {
	key       procKey
	readThere bool
}

// forkRead is one read of a process's size, with whether it was a fork's read (discountFork): one
// where the process ran its parent's executable with its parent's arguments, its parent listed in
// the table of the read and read by the guard in this stay, before the read or, by procReads.read,
// after it at the same reading (settleForks); or one where it still ran the executable and
// arguments of such a read of it, its parent gone or not. It is also a size as of a kill, with
// whether the read it came from was a fork's read (asOfKill, keepGrowth). That flag is what says
// whether two reads are of one program (anotherProgram).
type forkRead struct {
	size uint64
	fork bool
}

// program is what a process runs as the guard read it: its executable and its arguments, as the
// scan read them (readProcess) or the reads again at the mark (reidentify).
type program struct {
	exe  string
	args []string
}

// programOf is the program p runs as the guard read it. It keeps p's list of arguments, which a
// read never changes once made (reidentify makes a new one), and so it copies nothing.
func programOf(p *Process) program { return program{exe: p.Exe, args: p.Args} }

// is whether p and q are one program: the same executable with the same arguments.
func (p program) is(q program) bool { return p.exe == q.exe && slices.Equal(p.args, q.args) }

// anotherProgram is whether a read whose fork flag is read is of another program than a later read
// of the same process, or than that process at a later reading, whose flag is later: only where
// the read was a fork's read and the later one is not, as where the band read the process between
// its fork and its exec. A process that was no fork at a read has run no new program since,
// whatever its executable or arguments become, since an argument rewrite, as setproctitle or
// process.title makes, is not an exec (INFRA-3368's architect's decision of 2026-11-20T08:13Z on
// INFRA-3531 R1 of spec version 43, item 1; keepKillSizes names the cases it was measured by).
func anotherProgram(read, later bool) bool { return read && !later }

// bandStay is what a process's reads in the band of this stay say (keepStays): the least of them,
// and whether one of them is growthNoise or more above an earlier one. A band reading that does
// not read a process its table still lists leaves its stay as it was; one whose table no longer
// lists it drops it (INFRA-3531 R1). A stay begins at its first reading at or above the band's
// floor, whose table says which processes started during it (keepStayTable).
type bandStay struct {
	least uint64
	grew  bool
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
// panicked is the off reason of a scan that panicked (scanLargest), which the loop turns into the
// guard's end at its next reading (takeScan), and empty for every other scan. No scan that
// finishes later replaces one that panicked.
type largestScan struct {
	window   time.Time
	panicked string
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
	// counterRead, when set, is called before each read of the cgroup's counter (readCounter), on
	// the guard's loop. The tests set it to order those reads against the kills of a reading;
	// production leaves it nil.
	counterRead func()
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
	// memfds are the memfds an assessment's sizes have counted (addHeldMemfds, readAgain), emptied
	// at each assessment and kept for the next.
	memfds map[memfdID]bool
	// again are the reads again of an urgent reading of the wait (readAgain), which watchUrgent
	// alone uses: it empties them at each such reading and hands them to endKnown and to that
	// reading's assessment. Their storage is kept from one such reading to the next.
	again readAgain
	// sizes are the reads of the size of each process the latest assessment could end (keepSizes):
	// those within growthWindow before it, oldest first, which a process's growth is measured by,
	// how many readings of this stay in a row read it, which say whether it is new to the stay,
	// and the reads, each with whether it was a fork's read, that its size as of a kill at its
	// newest read's reading is taken from (asOfKill). They are emptied wherever the guard reads no
	// size (forgetSizes), where a stay ends.
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
	// (rereadHeld). A stay begins at its first reading at or above the band's floor and ends
	// wherever the guard drops its reads (forgetSizes); the reads at and above the mark are none of
	// these. They keep their storage from one reading to the next.
	stays map[procKey]bandStay
	// stayTable is the stay's first process table, the table of its first reading at or above the
	// band's floor (keepStayTable): a process it does not list by its pid and start time started
	// during the stay (startedInStay, INFRA-3531 R1 of spec version 46). It is nil until the stay
	// takes that table, and again from where the stay ends (forgetSizes).
	stayTable *Table
	// atKill are the sizes, by process, as of the previous kill of the rise, each with whether the
	// read it came from was a fork's read, from which growth since that kill is measured
	// (grownSinceKill), which says whether the guard goes on after it: each process's that the
	// guard read in this stay, kept once that kill's reading is read (keepKillSizes, asOfKill), and
	// for a process it first read after the kill, that first read (keepGrowth). A size from a
	// fork's read counts only while its process is still a fork (keepGrowth). A size is dropped
	// once the reading's table no longer lists its process (dropEnded), and not for a reading that
	// did not read it. They are emptied when the wait after the kill ends (endWait) and at the next
	// kill; their storage is kept from one kill to the next.
	atKill map[procKey]forkRead
	// atFirstKill are the sizes, by process, as of the first kill of this stay in the band, kept
	// the same way but for a process the guard never read before that kill, whose size there is
	// firstReadSince's, from which the order's third tier measures growth (sinceStayKill);
	// firstKillKept is whether the stay has had that kill. Both are let go of where the stay ends
	// (forgetSizes): below the band, while the guard is off and where it gives a rise up. A reading
	// below the mark that is in the band ends a rise and keeps them.
	atFirstKill   map[procKey]forkRead
	firstKillKept bool
	// sent are the kills sent at one reading (sendFirst, sendUrgent), whose lines and reports
	// reportKills writes once every one of them is sent; their storage is kept from one reading's
	// kills to the next.
	sent []sentKill
	// lastRead are the processes the guard may end that it read at the previous reading of the
	// wait after a kill, at the sizes it read and in the order that reading ranked them (rank):
	// the assessment that a kill was decided on (act), or the one that followed a kill on the
	// band's held reads (decide), and the assessment of each reading of the wait (watch).
	// lastTable is that reading's table. At an urgent reading of the wait the guard reads them
	// again first (endKnown). Both are let go of where the wait ends (endWait).
	lastRead  []candidate
	lastTable *Table

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
		again:        readAgain{sizes: map[procKey]uint64{}, memfds: map[memfdID]bool{}},
		sizes:        map[procKey]procReads{},
		atKill:       map[procKey]forkRead{},
		atFirstKill:  map[procKey]forkRead{},
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
// waits for ctx without reading again, and so does a guard whose loop panicked (step).
func (g *Guard) Run(ctx context.Context) {
	defer g.scans.Wait()
	defer g.release()
	if !g.step(g.begin) {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.clock.After(g.interval()):
		}
		if !g.step(func() bool { return g.tick(false) }) {
			<-ctx.Done()
			return
		}
	}
}

// step is run, the guard's first reading (begin) or a later one (tick), and false once the guard
// has turned off for good. A panic on the way turns the guard off for good with one line that
// names it (diePanicked) and goes no further: the launcher runs the guard on a goroutine of its
// own, and a panic that left it would end the launcher, PID 1 of the pod, which ends the pod, the
// very end the guard is there to prevent. That pod is the controller's or any gVisor issue pod,
// whose launchers run the same guard. Where the panic leaves the guard's state is not known, so
// the guard reads nothing more, as where it cannot read the pod (INFRA-3531 R1). A panic while the
// loop reads or parses a large smaps skips the release that ends each read, so step lets go of the
// loop's smaps buffer here (releaseSmaps).
func (g *Guard) step(run func() bool) (on bool) {
	defer func() {
		if p := recover(); p != nil {
			g.releaseSmaps()
			g.diePanicked(panicReason("panic", p))
			on = false
		}
	}()
	return run()
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
	if !g.rollWindow(now) {
		return false
	}
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
// process table, and, below the urgent share, before it reads any smaps where one of the held
// processes grew by the held reads; where none did, or at a reading at or above the urgent share,
// it reads first the smaps of the held processes that grew in the band or are new to this stay
// (rereadHeld), since the step that took the reading to the mark falls after
// the band's last reading and the held reads cannot show it, and a guard that did not read them
// again ended a sleeping fork before the grower in the `shared` probe under gVisor (INFRA-3531
// T17). Only then does it assess the reading, whose sizes are the window's newest reads; the sizes
// as of the kill take those reads only for a process the guard never read before the kill, and
// otherwise the band's and the decision's reads (keepKillSizes). tick runs nothing of the hourly
// line but its rollover before decide returns. That is the cut of INFRA-3531 T16, so the first
// kill comes within one interval of the reading reaching the mark, and within two where it must
// search for a process started after the band's last reading (R1 of spec version 35): with six
// growers on the pod's CPU the host
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
// the next rise does before it decides (INFRA-3531 R1 of spec version 37). The stay begins at its
// first reading at or above the band's floor, and its first table (keepStayTable), which says which
// processes started during the stay (startedInStay, INFRA-3531 R1 of spec version 46), is the first
// band reading's, or, where no armed reading in the band came before the reading at the mark (the
// guard had just started or been turned on, or the reading crossed the whole band within one
// interval), the reading at the mark's.
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
		g.held, g.heldTable = nil, nil
		if sent {
			// This reading's reads after the kill are the window's newest reads, and what the
			// wait's first reading reads again first at or above the urgent share (endKnown), in
			// the order of ends with the order's third tier (sinceStayKill), set before the sizes
			// as of the kill are kept, as act's callers rank before act. The order matters at the
			// stay's first kill: the stay has had none before keepKillSizes, so these reads keep
			// the order without that tier, since at the reading of that kill no process has grown
			// since it (endsInStay); kept first, those sizes would count a step between a held
			// process's band read and its read here as growth since that kill. At a later kill
			// either order sets the same tier: there keepKillSizes changes the sizes as of the
			// stay's first kill only by takes again that sinceStayKill makes too (takeFirstSince),
			// and by taking again the size of a process that ended before these reads, which
			// sinceStayKill has just dropped (dropEnded) and which none of these processes is
			// (seeds 11 and 97 of TestMemoryGuardKeepsTheRulesOfR1WhereGrowersStepAtRandomInstants).
			// These reads give a size as of the kill only to a process the guard never read before
			// it, but a fork's read among them makes each earlier read of its process at this
			// reading that found the same program a fork's read too, and a read of a fork's parent
			// among them makes each earlier read of the fork here that found that parent's program
			// one (keepKillSizes, procReads.read, settleForks).
			if a, ok := g.assessAt(now, readAgain{}); ok {
				if g.sinceStayKill(a.endable, a.table) {
					rank(a.endable)
				}
				g.keepKillSizes()
				g.lastRead, g.lastTable = a.endable, a.table
			}
			return
		}
		if g.isDead() {
			return
		}
	}
	a, ok := g.assessAt(now, readAgain{})
	if !ok {
		return
	}
	// Where this reading is the stay's first, its table is the stay's first (keepStayTable).
	g.keepStayTable(a.table)
	switch {
	case reading < mark:
		g.sinceStayKill(a.endable, a.table)
		g.held, g.heldTable = a.endable, a.table
		g.keepStays(a.endable, a.table)
	default:
		// A first kill of a rise that the band's reads did not decide: growth since the stay's
		// first kill orders it as it does every later kill.
		if g.sinceStayKill(a.endable, a.table) {
			rank(a.endable)
		}
		g.act(now, s, reading, limit, &a, 0, withHistory(a.endable))
	}
}

// endHeld ends the first of the held processes, the ones the assessment at the reading before, in
// the band, found the guard may end, at the reading taken at now, the first at or above the mark,
// and answers whether it sent the kill. It scans no process table and reads no descriptor before
// the kill (signalKill). The order is the order of ends (rank) at now, by each process's growth
// over its held reads (orderHeld). Below the urgent share, where one of them has grown, it decides
// on the held reads alone and reads no smaps before the kill, and the held processes keep the
// identities the band read: growth puts the first first there, and the fork discount orders only
// processes of equal growth. The kill's line says what the reads at the mark read of the processes
// new to the stay (new_read: newRead.field where it reads again, `-` where it does not).
//
// Where none of them has grown by the held reads, or at a reading at or above the urgent share
// (below), it reads again first the smaps of those that
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
// Where those reads show no growth either, or whatever they show at a reading at or above the
// urgent share (below), it looks for the processes it may end that started since the band's last
// reading (startedSince), which no held read names, and takes the order's second tier as one set,
// the processes new to this stay that it read at the mark (INFRA-3531 R1 of spec version 31): the
// held ones a read at the mark reached, at the sizes they read, and the ones it found started
// since. The largest of that set ends. A held process new to the stay that no read at the mark
// reached is not in the set: it ranks by its held size with the other held processes, the largest
// of which ends only where the set holds none (settleNew). This issue's tester measured, at
// e3a1923c, the latepool probe at 4 GiB, six growers starting together between the band's last
// reading and the mark while a parent and three sleeping forks of a shared block held the pod at
// 84%, end a process that shares the block, a sleeping fork or the parent, first in 7 of 7 runs,
// the closest run's highest reading 2.4 MiB below the kernel's kill point (INFRA-3531 R1 of spec
// version 27); and at 778c42de, in the latepool run `4g-latepool-zrr4d`, six growers that started
// 5 ms before the band's last reading, which read each of them once at a few MiB, lost a sleeping
// fork first to a guard that counted a held process as old, so the largest held process came
// first.
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
// the mark. Once it has read what it must, the decision reads nothing more past the budget; the
// read-on after the kills of an urgent reading in a stay with history is below (readOn). Where one
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
// Where the reading is at or above the urgent share, endUrgent ends beside the first every process
// that grew over the window and, where the decision holds one that is not new to the stay, every
// one of the sets the urgent rule ends for the stay: grown since the stay's first kill, started
// during the stay and no fork with memory of its own, or new to it with memory of its own
// (endsInStay, INFRA-3531 R1 of spec versions 39, 41, 42 and 46). Each held process's growth since
// the stay's first kill is set on the sizes the reads at the mark leave it, the held or the one
// read again, before the order is taken (sinceStayKill), and none has any where this reading's
// first kill is the stay's own: its sizes as of that kill are this reading's. A held process that
// started during the stay ends by those sizes and the fork test of its read too. So it reads the
// band's growers and the held processes new to the stay with memory of their own again within the
// half interval (rereadHeld) also where one of them grew by the held reads: a grower whose step
// came between the band's last reading and this one shows it only in a read at the mark, and each
// keeps the larger of its growth by the held reads and by its read at the mark. The reads again do
// not stop at the first grower they find; they stop once the half interval has passed. Whether one
// grew at the mark (grewSince) is the held processes' growth after those
// reads: of each it read, the larger of the two; of each it did not reach, its held growth; and
// none of one it left out, gone or with smaps it cannot read. Below the urgent share, where none
// grew so, the reading searches for the processes new to the stay, as R1 of spec version 39 asks
// where no process grew over the window. The tester's replay of `4g-latepool-r5` (bd8bb44c) models
// a held process that grew between the band's last two readings and is gone at the urgent reading
// at the mark: a guard that kept its held growth there searched for nothing, and ended the probe's
// 2.4 GB parent by `rule=largest sizes=reread new_read=0:0` while six growers started since
// lived. At or above the urgent share it searches whatever grewSince says, since the processes
// started since the band's last reading are the burst: at a82a9f2c, in `4g-latepool-4tpqx`, six
// growers started between the band's last reading and the mark, and the one ended there left the
// pod 26.5 MiB from the kill point.
//
// Where one grew there, it sends the kill of the most grown held process before the search
// (sendFirst), the floor read on the sizes read again: a process the search finds has no growth
// over the window, so none goes before that one, and the first kill waits for no search it does
// not need (INFRA-3531 R1 of spec version 43, which gives the first kill a second interval only
// where it must search for a process started after the band's last reading). In the model of a
// held grower beside processes started since, where each smaps read at the mark takes 60 ms, the
// guard at 15b73ba6, which searched first, sent that kill 120 ms after the reading at the mark;
// it now goes 60 ms after it, once the grower is read again. The kills the search adds, of the
// processes it finds and of a held process that a later read at the mark finds grown, go after it,
// by the urgent rule. The search has no budget of its own: it reads within what the reads again
// left of the half interval, stops after the read in hand once that has passed, and reads on past
// it only where no process it has read new to the stay holds memory of its own (readOnNew).
//
// The half-interval budget governs only the reads that choose the first kill. At an urgent reading
// where the decision holds a process it may end that is not new to the stay (withHistory), once the
// first kill and the urgent kills of the processes those reads reached are sent, the guard reads on
// through the processes new to the stay that no read at the mark has reached, in the same groups
// and order, up to one interval from the reading at the mark: it reads no process once that has
// passed, and ends the one whose read is in hand. It ends each it reads that grew over the window,
// holds memory of its own or, by that read, grew since the stay's first kill, by the urgent rule,
// its kill sent as that process is read and not after the last read (readOn, INFRA-3531 R1 of spec
// versions 43 and 46). In `4g-t4latepool-r3` at 4d6665d5 six growers started after the band's last
// reading; the first reading at the mark was at
// or above the urgent share, its search started after half an interval and each smaps read took 22
// ms, so it read one grower, and the guard ended that one alone; the host ended the pod with the
// highest reading 25.5 MiB below the kill point. Below the urgent share, and where every process is
// new to the stay, nothing is read after the first kill. The counter, the reading's Shmem and the
// lines and reports of every kill of the reading come after the last kill is sent, in the order of
// the kills, each with the decision's sizes and its new_read, the read-on's reads among those it
// counts (reportKills), and the wait after the kills holds every one of them.
func (g *Guard) endHeld(now time.Time, s Settings, reading, limit uint64) bool {
	floor := share(limit, s.FloorPercent)
	grew, heldBytes := g.orderHeld(now)
	if len(g.held) == 0 || heldBytes < floor {
		return false
	}
	sizes := sizesBand
	// readField is the kill line's new_read, "" where the guard reads nothing before the kill.
	var readField string
	// read is what the reads at now have read of the processes new to the stay, and search the
	// search for those started since the band's last reading, which the read-on resumes (readOn).
	var read newRead
	var search startedSearch
	// The held processes' parents are the band's, at their latest reads, the ones read again at
	// now among them; the found ones are discounted by the table that placed them, each as it is
	// read and a found fork again once the search reads its found parent, and one decision reads a
	// parent's whole arguments once (argsCache).
	var cache argsCache
	urgent := reading >= urgentShare(limit, s.ActPercent)
	half, whole := now.Add(s.Interval/2), now.Add(s.Interval)
	// sent is whether the first kill went before the search (below).
	sent := false
	if !grew || urgent {
		g.rereadHeld(now, half, &cache)
		grewSince := slices.ContainsFunc(g.held, func(c candidate) bool { return c.growth > 0 })
		g.discountForks(now, g.held, g.heldTable, &cache)
		read = g.heldNewRead(now)
		if grewSince && urgent {
			// A process the search finds has no growth over the window, so the most grown held
			// process is first whatever it finds, and its kill does not wait for it (INFRA-3531
			// R1 of spec version 43). The floor is read on the sizes read again.
			if sizeSum(g.held) < floor {
				return false
			}
			g.sinceStayKill(g.held, g.heldTable)
			if !g.sendFirst(now, s, reading, slices.MinFunc(g.held, orderOfEnds)) {
				return false
			}
			sent = true
		}
		if !grewSince || urgent {
			// The rest of the held processes new to the stay that held memory of their own at
			// their band read, then the processes started since, then the held forks that ranked
			// at nothing at their band read, each group the newest first, within half the
			// interval and past it while the reads at the mark read on (readOnNew).
			inBudget := func() bool { return g.clock.Now().Before(half) || g.readOnNew(read, whole) }
			take := func(*candidate) bool { return inBudget() }
			// The search reads one process at a time once half the interval has passed.
			pastHalf := func() bool { return !g.clock.Now().Before(half) }
			g.readHeldNew(now, false, take, &read, &cache, nil)
			g.startedSince(&search, now, pastHalf, inBudget, &read, &cache, nil)
			g.readHeldNew(now, true, take, &read, &cache, nil)
		}
		reread := g.settleNew(now)
		if !sent && (len(g.held) == 0 || sizeSum(g.held) < floor) {
			return false
		}
		if reread {
			sizes = sizesReread
		}
		readField = read.field()
	} else {
		g.discountForks(now, g.held, g.heldTable, &cache)
	}
	if len(search.found) > 0 {
		g.held = append(g.held, search.found...)
		sizes = sizesSearch
	}
	g.sinceStayKill(g.held, g.heldTable)
	rank(g.held)
	if !sent {
		if !g.sendFirst(now, s, reading, g.held[0]) {
			return false
		}
	}
	// Whether the decision holds a process that is not new to the stay, taken once on the set it
	// decides on (urgentEnds).
	history := withHistory(g.held)
	g.endUrgent(s, reading, limit, g.held, history)
	if urgent && history {
		// The kills' lines carry the read-on's reads in their new_read too. Its reads come after
		// the first kill, so a size as of the kills takes them only for a process the guard never
		// read before it (keepKillSizes).
		g.readOn(now, whole, &search, &read, &cache)
		readField = read.field()
	}
	g.reportKills(reading, limit, sizes, readField)
	return true
}

// sizeSum is the sum of the sizes of cs.
func sizeSum(cs []candidate) uint64 {
	var total uint64
	for _, c := range cs {
		total += c.size
	}
	return total
}

// newRead is what the reads at the mark have read of the processes new to the stay that the guard
// may end: how many (reached), and how many of them rank above nothing, with memory of their own
// (own). At an urgent reading in a stay with history it counts the read-on's reads too (readOn).
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

// readOnNew is whether the reads at the mark that the decision is made on (endHeld) read one more
// process new to the stay past half the interval, having read r of them: where they have read
// none the guard may end, whatever the clock; where they have read none that ranks above nothing,
// while the guard's clock is before whole, one interval from the reading at the mark (INFRA-3531
// R1 of spec version 34). It bounds the decision's reads alone, which choose the first kill: after
// the decision's kills, at an urgent reading in a stay with history, the read-on reads until whole
// whatever they have read (readOn, INFRA-3531 R1 of spec version 43).
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
	r := g.sizes[procKey{pid: c.PID, start: c.StartTime}]
	return r.readAt(now)
}

// readAt is whether r's newest read is at the reading taken at at: whether the guard has read the
// process at that reading.
func (r *procReads) readAt(at time.Time) bool {
	n := len(r.window)
	return n > 0 && r.window[n-1].at.Equal(at)
}

// newToStayAt is whether r's process is new to the stay at its read at the reading taken at at:
// fewer than two readings before that one read it in a row (readings), none or only the one
// before, a read already kept at that reading not counted among them. keepSize sets a read's
// newToStay by it, orderHeld a held process's at the mark before any read there, and endKnown asks
// it of a process before it reads it, for its history, so the three take newness by one test.
func (r *procReads) newToStayAt(at time.Time) bool {
	readings := r.readings
	if r.readAt(at) {
		readings--
	}
	return readings < 2
}

// readHeldNew reads at the reading taken at now the held processes new to the stay of one group
// of the reads at the mark that no read at now has reached, the newest first (rereadHeld's order):
// where forks is false, those that held memory of their own at their band read, which the reads
// again take within half the interval and this reads past it; where it is set, those that ranked
// at nothing at it (candidate.nothingAtBand), which come after the processes started since
// (INFRA-3531 R1 of spec version 38). It reads each that take allows, asked before each read:
// within half the interval, or past it while the guard reads on (readOnNew), for the decision's
// reads (endHeld); before one interval from now, and not a process the guard has killed or failed
// to kill at this reading, for the read-on after the decision's kills (readOn). It counts each in
// read. Each it reads it identifies again (reidentify) and discounts (discountFork) with cache,
// and it is reached there (candidate.unreached), and where end is set hands it to end at once,
// before the next read; one whose smaps or executable it cannot read it leaves out of the held
// processes, as rereadHeld does, and reads the next.
func (g *Guard) readHeldNew(now time.Time, forks bool, take func(c *candidate) bool, read *newRead, cache *argsCache, end func(c *candidate)) {
	defer g.releaseSmaps()
	kept := g.held[:0]
	for _, c := range g.held {
		if c.newToStay && c.nothingAtBand == forks && !g.readAt(&c, now) && take(&c) {
			size, err := g.size(c.PID, nil)
			if err != nil || !g.reidentify(c.Process, cache) {
				continue
			}
			c.size, c.unreached = size+c.descriptorBytes, false
			g.keepSize(now, &c)
			g.discountFork(now, &c, g.heldTable, cache)
			read.add(&c)
			if end != nil {
				end(&c)
			}
		}
		kept = append(kept, c)
	}
	g.held = kept
}

// settleNew makes the held processes new to the stay the ones the order's second tier takes at the
// mark, once the reads at the reading taken at now are made (INFRA-3531 R1 of spec version 31):
// those a read at now reached, at the size it read. One that no read reached is not in that tier
// (candidate.unreached), and ranks by its held size with the other held processes; it is still new
// to the stay, and endUrgent counts it so where it asks whether the decision holds a process that
// is not new (INFRA-3531 R1 of spec version 43), and it is in none of the sets the urgent rule ends
// for the stay until a read reaches it, the read-on's among them (endsInStay, readHeldNew). It
// answers whether a read at now read one of the held processes.
func (g *Guard) settleNew(now time.Time) (reread bool) {
	for i := range g.held {
		c := &g.held[i]
		at := g.readAt(c, now)
		c.unreached = c.newToStay && !at
		reread = reread || at
	}
	return reread
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
		c.newToStay = r.newToStayAt(now)
		c.nothingAtBand = c.newToStay && c.rankSize() == 0
		grew = grew || c.growth > 0
		heldBytes += c.size
	}
	return grew, heldBytes
}

// startedSearch is the search at the reading at the mark for the processes started since the
// band's last reading (startedSince), kept so that the read-on after the kills of an urgent reading
// in a stay with history resumes it where it stopped (readOn): whether it has listed the processes
// (listed); the table of the band's processes still listed and the new ones it placed (t), nil
// where none started or the listing failed; the listed pids no band table holds, the highest first
// (pids), and the first of them whose identity it has not read (next); the new processes it has
// read the identity of and not yet the smaps (started); whether its last round stopped before the
// smaps of a process it placed and may end (left); and the processes it read (found), the newest
// first.
type startedSearch struct {
	listed  bool
	t       *Table
	pids    []int
	next    int
	started map[int]*Process
	left    bool
	found   []candidate
}

// startedSince finds the processes the guard may end that started since the band's last reading,
// whose table the guard holds with the held processes (heldTable), into sr's found: each at its
// smaps size, kept as its read at now, the reading at the mark (keepSize), and new to this stay,
// since no reading of the stay read it, the newest first. Its reads before the kill are the listing
// of the processes, the identities of the newest new ones and their smaps, these two within half an
// interval from the reading at the mark: at f949e5ad two of seven latepool runs at 4 GiB
// sent the first kill 196.3 and 128.2 ms after the reading at the mark, past one interval, their
// growers with one band read at most and so found here, while it read the stat of every held pid
// and the four identity files of every new one before the kill, each a read that can wait out a
// stall of the pod's CPU throttle.
//
// It reads while goOn says so, asked before each read. For the reads of the decision (endHeld),
// that is within half an interval from the reading at the mark, and past it only while the guard
// reads on (readOnNew) with read, what the reads at the mark have read of the processes new to the
// stay, and what it reads itself: where no process new to the stay that the guard may end has been
// read, whatever the clock (INFRA-3531 R1 of spec version 32: "and at least one of them"), and
// where none read ranks above nothing, until whole, one interval from the reading at the mark (spec
// version 34). For the read-on after the kills of an urgent reading in a stay with history, it is
// until whole, whatever it has read (readOn, spec version 43). A round reads the identities of the
// new processes, and then the smaps of those it may end. Where oneAtATime says so, asked before
// each identity read but the round's first, the round reads no more identities, so it reads one
// process at a time, the newest first, its identity and, where the guard may end it, its smaps,
// after any processes the round before placed and left unread: for the reads of the decision, once
// half an interval from the reading at the mark has passed; for the read-on, always, so that each
// of its kills goes as its process is read, before the next identity read (end). It stops after the
// read in hand once goOn says no, and it reads nothing at all, the listing included, where goOn
// says no before the first read. Each process it reads it discounts as a fork (discountFork) with t
// and cache, and once it has read a process, each fork of it read before, which started after it
// and so came first, ranks by its size less that read: the search has read its parent in this stay
// (INFRA-3531 R1 of spec version 43), and a fork it so finds ranks at nothing where its parent's
// size covers its own.
//
// Where end is set, the read-on's, it hands end each process it reads at once, before the next
// read, but a fork of a process this round has placed, may end and has yet to read: that one waits
// for its parent's read, which can cover it, and goes to end right after it, or at the round's end
// where the parent is not read.
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
// past the budget, and for the processes it placed and did not read before the read-on, since only
// the classifier says which the guard may end. It reads the smaps of those it may end alone, the
// newest by start time first; it reads no descriptor, and one whose smaps it cannot read is left
// out. It finds none where none started, and asks the classifier nothing then.
func (g *Guard) startedSince(sr *startedSearch, now time.Time, oneAtATime, goOn func() bool, read *newRead, cache *argsCache, end func(c *candidate)) {
	if !sr.listed {
		if !goOn() {
			return
		}
		sr.listed = true
		g.listStarted(sr)
	}
	t := sr.t
	if t == nil {
		return
	}
	defer g.releaseSmaps()
	cmdline := g.cmdlineBuffer()
	// One round reads the identities the budget allows, or the next one alone where oneAtATime
	// says so, none where the round before left processes it placed unread, and then the smaps of
	// those it may end.
	for {
		from := sr.next
		for ; !sr.left && sr.next < len(sr.pids); sr.next++ {
			if sr.next > from && oneAtATime() || !goOn() {
				break
			}
			pid := sr.pids[sr.next]
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
				sr.started[pid] = p
			}
		}
		if sr.next == from && !sr.left {
			break
		}
		sr.left = false
		placed := placeStarted(t, sr.started)
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
		// waiting are the indexes in found of the forks that wait for their parent's read (end).
		var waiting []int
		for _, p := range placed {
			if !g.mayEnd(t, classes, p.PID) {
				continue
			}
			if !goOn() {
				sr.left = true
				break
			}
			delete(sr.started, p.PID)
			size, err := g.size(p.PID, nil)
			if err != nil {
				continue
			}
			c := candidate{Process: p, role: classes[p.PID].Role, size: size}
			g.keepSize(now, &c)
			g.discountFork(now, &c, t, cache)
			read.add(&c)
			// A fork of c read before it, which started after it, now has its parent read in this
			// stay and ranks by its size less c's (INFRA-3531 R1 of spec version 43); one that so
			// ranks at nothing no longer counts as holding memory of its own.
			for i := range sr.found {
				if f := &sr.found[i]; f.PPID == c.PID {
					own := f.rankSize() > 0
					g.discountFork(now, f, t, cache)
					if own && f.rankSize() == 0 {
						read.own--
					}
				}
			}
			sr.found = append(sr.found, c)
			if end == nil {
				continue
			}
			waiting = slices.DeleteFunc(waiting, func(i int) bool {
				if sr.found[i].PPID != c.PID {
					return false
				}
				end(&sr.found[i])
				return true
			})
			_, parentWaits := sr.started[c.PPID]
			if parentWaits && g.mayEnd(t, classes, c.PPID) && g.sameProgram(c.Process, t.byPID[c.PPID], cache) {
				waiting = append(waiting, len(sr.found)-1)
			} else {
				end(&sr.found[len(sr.found)-1])
			}
		}
		for _, i := range waiting {
			end(&sr.found[i])
		}
		if !goOn() {
			break
		}
	}
}

// listStarted lists the processes for sr (startedSince): the pids that the band's table, the one
// the guard holds with the held processes (heldTable), holds neither as a process nor as unread,
// the highest first, and a table of the band's processes still listed. It leaves sr's table nil
// where nothing is held, the listing fails or no process started since.
func (g *Guard) listStarted(sr *startedSearch) {
	band := g.heldTable
	if band == nil {
		return
	}
	entries, err := os.ReadDir(g.proc)
	if err != nil {
		return
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
		return
	}
	slices.SortFunc(newPIDs, func(x, y int) int { return cmp.Compare(y, x) })
	sr.t, sr.pids, sr.started = t, newPIDs, map[int]*Process{}
}

// mayEnd is whether the guard may end pid by t, a table the classifier answered classes for: one
// t counts read, not the guard's own and not protected.
func (g *Guard) mayEnd(t *Table, classes map[int]Class, pid int) bool {
	_, unread := t.unread[pid]
	return !unread && pid != g.self && !classes[pid].Protected
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
// (keepSize), which sets its growth at now. Every other held process keeps its held size and its
// growth by the held reads. It leaves the held processes in the order it read them in, each with
// its growth after the reads, from which endHeld decides whether one grew at the mark (grewSince).
// A process new to the stay started between the band's last two readings, and may be a grower the
// band has not yet seen grow: in T17's `4g-latepool-dmq44` and `4g-latepool-mgzv9` the growers had
// one band read each, and reading them again kept a grower the first ended. A process with two
// reads or more that never grew between them is not read again, since a sleeping fork's read
// changes no answer, and so a process flat through the whole band that grows only at the mark is
// not ended first for that. A held process whose smaps it cannot read, gone or not, it leaves out
// of the held processes, and so out of this decision; the assessment of the reading that follows
// reads it again.
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
// reads, none below the urgent share, where endHeld reads again only where no held read grew, so
// that there a process it reads alone can show growth at the mark; at a reading at or above it one
// it reads keeps the larger of that growth and its growth at the mark, and one it leaves out takes
// its held growth with it (INFRA-3531 R1 of spec version 39, `4g-latepool-r5`). So the decision is
// made on what it read; a held process new to the stay that it has not reached is not among the
// new ones the order takes, unless the guard reads it yet (readHeldNew, settleNew). At 9d6d24df,
// `4g-latepool-lmnt5` sent its first kill 100.68 ms after the reading at the mark, past one
// interval: its six growers had one band read each, and the guard read all six growing processes'
// smaps again before the kill under the pod's CPU throttle. endHeld gives it half the interval.
func (g *Guard) rereadHeld(now, deadline time.Time, cache *argsCache) {
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
			}
		}
		kept = append(kept, c)
	}
	g.held = kept
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
// it runs now, and a process that has run another program since ranks by its whole size. So its
// read at the mark is a fork's read by what it reads (discountFork): where it still runs its
// parent's executable with its parent's arguments, the flag of its held read, and none where it
// has run another program since (INFRA-3368's architect's decision of 2026-11-20T08:13Z on
// INFRA-3531 R1 of spec version 43). It answers false where the exe link cannot be read, which
// leaves p out of this decision, as a read of its smaps that fails does: a process that has ended
// since holds no memory, and one whose exe cannot be read is one a scan counts unread and the guard
// never ends (unreadOrEnded); the assessment of the reading reads it again. The arguments are best
// read, as a scan reads them (readProcess): where they cannot be read, p has none, which no
// discount matches.
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
// reading, by the rule of INFRA-3531 R1 of spec version 25 that decideWait states. It reads the
// sizes of what it may end at each reading, so that their order and their growth since the kill
// (grownSinceKill) are known whenever it decides, and it asks each killed process whether it has
// exited and keeps whether the reading has fallen since the kills (fell). At a reading at or above
// the urgent share, once every killed process has exited or the wait has passed, the urgent reading
// of the wait decides (watchUrgent); every other reading of the wait, and an urgent one at which
// the reads again before its full read end nothing (endKnown), decides on its full read
// (decideWait).
func (g *Guard) watch(now time.Time, s Settings, reading, limit, mark uint64) {
	w := &g.wait
	// Each handle is asked before the scan, so a process that exits after the scan read it is
	// still left out of this reading's decision; one that has exited is asked no more.
	exited := true
	for i := range w.kills {
		k := &w.kills[i]
		k.exited = k.exited || k.handle.Exited()
		exited = exited && k.exited
	}
	w.fallen = w.fallen || fell(w.reading, reading, mark)
	passed := !now.Before(w.deadline)
	if (exited || passed) && reading >= urgentShare(limit, s.ActPercent) {
		g.watchUrgent(now, s, reading, limit, exited, passed)
		return
	}
	if a, ok := g.assessAt(now, readAgain{}); ok {
		g.decideWait(now, s, reading, limit, &a, exited, passed)
	}
}

// decideWait decides on a, the full read of the reading taken at now in the wait after a kill,
// exited being whether every killed process has exited and passed whether the wait has passed
// (watch), by the rule of INFRA-3531 R1 of spec version 25: one kill, or, at a reading at or above
// the urgent share, beside the first the kills of every process that grew over the window and,
// where the decision holds a process that is not new to the stay, of every one of the sets the
// urgent rule ends for the stay (act, endUrgent, urgentEnds, spec versions 39, 41, 42 and 46).
// While a killed process has not exited it is left out of what the guard may end, of their sum
// against the floor and of the growth since the kill (leaveOut): the guard has ended it, and never
// ends or reports it again. While a killed process has not exited and the wait has not passed, it
// ends nothing, because the reading can fall below the mark as the rest of the process's memory
// goes. Once every killed process has exited or the wait has passed, it ends the next in the order
// (act), at once, where the reading has fallen, since the kills freed something and the pod is
// still at the mark, and where it has not fallen but a process it may end has grown since the kill
// and those processes hold at least the floor between them, since a kill that frees less than one
// interval's rise leaves the pod rising toward the kernel's kill point. At a reading at or above
// the urgent share where those processes hold one that is not new to the stay, one of the sets the
// urgent rule ends for the stay (endsInStay) is a reason to act as growth since the kill is, under
// the same floor, and the first kill goes to the process urgentFirst chooses: the urgent rule ends
// it at that reading (endUrgent), though it has not grown since the kill, and so the processes new
// to the stay that the read-on at the mark did not reach end at the next such reading (INFRA-3531
// R1 of spec versions 43 and 46, verification version 36). In the tester's replay of run
// `4g-t4latepool-r3` at 4d6665d5 with each read 22 ms after the first kill too, the read-on reached
// one grower of the five left, and the run's next reading, 92.55%, showed none of the other four
// grown since the kill; in `4g-r15latepool-r206` at 00405eb3 the reading at 93.95% after a kill
// held four growers that started during the stay and had not stepped since they were first read.
// Where none of these holds it ends nothing and decides again at the next reading, until the wait
// has passed; then it gives the rise up with reason not fallen, whichever failed, since the kill's
// freeing nothing is why it stops. So a rise that is a protected process's own, which no kill
// lowers and in which nothing the guard may end grows, costs at most one kill below the urgent
// share (INFRA-3531 R1 of spec version 43); at or above it, in a stay with history, it can also
// cost each process of those sets, which the urgent rule ends at the reading of that kill or at a
// later reading of the wait at or above that share.
//
// Whether the processes it may end hold one that is not new to the stay it takes once, at a
// reading where it may act, on a with each killed process that has not exited left out, for both
// the reason to act that urgentFirst reads and the kills act sends (urgentEnds). Where it ends
// nothing, a is what the next reading of the wait reads again first (lastRead).
func (g *Guard) decideWait(now time.Time, s Settings, reading, limit uint64, a *assessment, exited, passed bool) {
	w := &g.wait
	// A killed process that has not exited is left out of what the guard may end (leaveOut).
	for _, k := range w.kills {
		if !k.exited {
			a.leaveOut(k.key)
		}
	}
	// At every reading of the wait, so that a process first read after a kill has its first read
	// kept. It answers whether one grew since the previous kill, a go-on, and sets each one's
	// growth since the stay's first kill, the order's third tier (rank), so the processes are
	// ranked again by it.
	grown := g.grownSinceKill(a.endable, a.table)
	rank(a.endable)
	floor := share(limit, s.FloorPercent)
	switch {
	case !exited && !passed:
		// The wait goes on.
	case w.fallen || grown && a.endableBytes >= floor:
		g.endWait()
		g.act(now, s, reading, limit, a, 0, withHistory(a.endable))
		return
	default:
		history := withHistory(a.endable)
		// A process the urgent rule ends for the stay is a reason to act too, under the same
		// floor, and the kills go to the processes that rule ends (urgentFirst).
		if first := g.urgentFirst(s, reading, limit, a.endable, history); first >= 0 && a.endableBytes >= floor {
			g.endWait()
			g.act(now, s, reading, limit, a, first, history)
			return
		}
		if passed {
			g.endWait()
			g.stopRise(now, reasonNotFallen, reading, limit, a)
			return
		}
	}
	g.lastRead, g.lastTable = a.endable, a.table
}

// watchUrgent decides on the urgent reading of the wait after a kill: a reading at or above the
// urgent share, taken at now, once every killed process has exited or the wait has passed (watch,
// whose exited and passed it hands on). It first reads again, and ends, the processes the reading
// before read (endKnown), and then lists the processes, its full read on those reads (readAgain;
// INFRA-3531 R1 of spec versions 43 and 46, in the order of verification version 36). Where the
// reads again send a kill, the full read decides only the rest: it leaves out each process killed
// at this reading and each of the wait before that had not exited, sets the order's third tier
// (sinceStayKill), and then sends the kills the urgent rule asks for on one history for the
// reading, whether one of the processes the reads again take, which still runs, or one of the full
// read's is not new to the stay, writes every line and report, keeps the sizes as of the kills
// (keepKillSizes) and keeps the full read's processes as what the next reading of the wait reads
// again first (endRest). Where the guard turns off for good at the full read or at a kill (die),
// the kills it has sent still have their lines and reports. Where the reads again send none, the
// wait decides on the full read as at any of its readings (decideWait).
func (g *Guard) watchUrgent(now time.Time, s Settings, reading, limit uint64, exited, passed bool) {
	clear(g.again.sizes)
	clear(g.again.memfds)
	sent, history, left := g.endKnown(now, s, reading, limit, g.again)
	a, ok := g.assessAt(now, g.again)
	if !ok || g.isDead() {
		if sent {
			g.reportKills(reading, limit, sizesReading, "")
		}
		return
	}
	if !sent {
		g.decideWait(now, s, reading, limit, &a, exited, passed)
		return
	}
	// A killed process that has not exited is left out: each of the wait before these kills that
	// had not exited (left), and each killed at this reading, which its kill found running
	// (signalKill).
	for _, key := range left {
		a.leaveOut(key)
	}
	for _, k := range g.wait.kills {
		a.leaveOut(k.key)
	}
	// The order's third tier, for endUrgent and the next reading (lastRead). Growth since the
	// previous kill decides nothing here: the kills are sent, and keepKillSizes keeps theirs.
	g.sinceStayKill(a.endable, a.table)
	rank(a.endable)
	g.endRest(s, reading, limit, &a, history || withHistory(a.endable))
}

// readAgain is what the reads again of an urgent reading of the wait read before its full read
// (endKnown): the smaps size of each process they read, by process, and the memfds those reads
// found mapped. The full read of that reading takes each size in place of a read of its own, and
// counts each of those memfds as counted already (assess), so the order adds no smaps read; the
// history endKnown takes adds the stat read of a process not new to the stay, or of a few where
// those ranked before it have ended (stillRuns). Every other assessment takes none, the zero
// readAgain. watchUrgent empties the guard's (Guard.again) at each such reading, so they are that
// reading's reads alone.
type readAgain struct {
	sizes  map[procKey]uint64
	memfds map[memfdID]bool
}

// endKnown reads again, at the urgent reading of the wait after a kill, taken at now
// (watchUrgent), the smaps of the processes the guard may end that it read at the reading before
// (lastRead), one at a time, in the order that reading ranked them (rank). It ends each one whose
// read shows it grew since the previous kill, its size as of that kill (atKill) plus growthNoise or
// more, a fork or not as the reading before read it, since it reads no executable (discountFork,
// keepGrowth): the first by its tier (sendFirst), as soon as it has read it, before the next read,
// where the processes it read at the reading before hold the floor at their sizes there, with the
// sizes read again, and every other by the urgent rule (sendUrgent), one it read before that first
// kill, at whose read they did not hold the floor, with that kill, and one it reads after it as
// soon as it has read it. With that first kill it ends by the urgent rule too each process that
// rule ends (urgentEnds), on the history it took before its first read (INFRA-3531 R1 of spec
// versions 46 and 48): at once, before its next read, each it read before that kill, and each it
// reads after it as soon as it has read it. The rest the full read's endUrgent ends (watchUrgent).
// It reads and ends no process killed in this wait or whose kill failed (triedHere). Each read has
// the process's descriptors at what the reading before counted for them (descriptorBytes), is kept
// as its read at now (keepSize), which sets its growth over the window and whether it is new to the
// stay, and in again (readAgain); and the fork discount and the growth since the stay's first kill
// are set again by it (discountFork, atFirstKill), which the first kill's rule names. One whose
// smaps it cannot read it leaves to the full read. The first kill starts the wait afresh on the
// wait's storage (sendFirst), so it copies the wait's kills before it and, where it is sent, keeps
// the keys of those that had not exited, which the full read leaves out as the wait did, and closes
// their handles, which nothing asks again, once its reads are done or a panic has stopped them. It
// answers whether it sent a kill, that history, and those keys (left).
//
// This is INFRA-3531 R1 of spec version 43 in the order verification version 36 states: "at an
// urgent reading after a kill, every process the guard already knows grew since the previous kill
// is ended before the guard reads the other processes for that reading". In the tester's run r17
// at 41d4f45b the kills at 86.60% and 89.96% each ended one of six growers, the two readings after
// the second showed nothing grown since it, and the growers stepped before the next, 95.16%, where
// the guard listed and read every process for 205 ms before it ended the four left; the reading
// came 12.3 MiB from the 4 GiB kill point. The growers rank first there by their growth since the
// stay's first kill, before the probe's 2.4 GB parent and its sleeping forks, which grow by none
// of the order's tiers, so they are read first.
//
// The history is whether the processes it is to read again hold one that is not new to the stay at
// this reading and still runs. It takes it before its first read, and so before any kill, from what
// needs no read of a size: whether the process is new to the stay as its read at now would set it,
// by the readings in a row that read it (newToStayAt, by which keepSize sets it), and whether it
// still runs, by its stat (stillRuns). So the urgent rule ends the stay's sets as the reads again
// reach them, wherever the order puts the process that gives the history, and a process that has
// ended by this reading, which the full read leaves out, never gives it (the case of 98207b42,
// below). At c1c86b9e it took the history on the processes it had read again so far, the one in
// hand included, and so left to the full read each process of the sets that it read before the
// first one not new to the stay. Where the reading before ranked pids 400 and 401, both new to
// the stay, before pid 300, flat and not new to it, it ended pid 400, grown since the kill, and
// the full read's endUrgent ended pid 401, flat with memory of its own, by rule=urgent 200 ms
// later: the time the full read took for the one smaps the reads again had not read, as the full
// read of the tester's run r17 took 205 ms (thread PRRT_kwDORFy7ds66UWBx of review round 5 of
// cybermonklabs/legion#4273; the subtest "401 ranks after 400" of
// TestMemoryGuardEndsAtAnUrgentReadingOfTheWaitTheSetsItReadAgainBeforeItReadsOneNotNewToTheStayBeforeTheFullRead;
// INFRA-3531 R1 of spec version 48; INFRA-3368's architect, 2026-11-21T08:54Z, item 1: "The early
// kills at the wait's urgent reading must not wait for the full read"). The processes it read
// before the first kill end with that kill because the order can put one of the sets before the
// first process that grew since the kill: the reading before ranks the new tier by size, so pid 401
// of 350 MiB is read before pid 400 of 300 MiB, which then grows (the subtest "401 ranks before
// 400" of the same test).
//
// At 98207b42 it took the history on the processes the reading before read, before any read, so a
// process that had ended since still counted, and the answer turned on rank alone: where pid 300,
// the one process not new to the stay, had ended, it ended pid 401, new to the stay and flat, by
// rule=urgent where pid 401 ranked after pid 400 at the reading before, the grower it then ended
// first, and spared it where it ranked before (thread PRRT_kwDORFy7ds66JVV7 of review round 4 of
// cybermonklabs/legion#4273;
// TestMemoryGuardEndsNoProcessForTheStayAtAnUrgentReadingOfTheWaitWhereTheOneNotNewToItHasEnded;
// INFRA-3531 R1 of spec version 48; INFRA-3368's architect, 2026-11-20T19:50Z).
func (g *Guard) endKnown(now time.Time, s Settings, reading, limit uint64, again readAgain) (sent, history bool, left []procKey) {
	defer g.releaseSmaps()
	floor := share(limit, s.FloorPercent)
	// On a copy: DeleteFunc zeroes the entries it removes, and kept reuses known's array, so
	// filtering lastRead itself would leave it holding zeroed candidates until the wait sets it
	// again.
	known := slices.DeleteFunc(slices.Clone(g.lastRead), func(c candidate) bool { return g.triedHere(&c) })
	// The history, before the first read and so before any kill, by what needs no read of a size.
	history = slices.ContainsFunc(known, func(c candidate) bool {
		r := g.sizes[procKey{pid: c.PID, start: c.StartTime}]
		return !r.newToStayAt(now) && g.stillRuns(&c)
	})
	var cache argsCache
	kept := known[:0]
	// before are the wait's kills before the first kill, and swept is how many of kept the sweep
	// has passed on.
	var before []killedProc
	swept := 0
	// Once the first kill is sent, which starts the wait afresh, nothing asks the handles of
	// before again: they close after the loop, by a defer, so a panic in a later step of the loop,
	// a smaps read or growthSinceStayKill's broken invariant, leaks none of them (INFRA-3531 R1;
	// D3 of the code-reuse review in the simplify pass of 6a1d8e2f, where they closed after the
	// loop with no defer).
	defer func() {
		if sent {
			for _, k := range before {
				_ = k.handle.Close()
			}
		}
	}()
	for i, c := range known {
		key := procKey{pid: c.PID, start: c.StartTime}
		size, err := g.size(c.PID, again.memfds)
		if err != nil {
			continue
		}
		again.sizes[key] = size
		c.size = size + c.descriptorBytes
		g.keepSize(now, &c)
		g.discountFork(now, &c, g.lastTable, &cache)
		c.sinceFirstKill = g.growthSinceStayKill(&c)
		kept = append(kept, c)
		if !sent && keepGrowth(g.atKill, &c) > 0 && sizeSum(kept)+sizeSum(known[i+1:]) >= floor {
			before = slices.Clone(g.wait.kills)
			sent = g.sendFirst(now, s, reading, c)
		}
		if !sent {
			continue
		}
		// Who ends with the first kill and after it, by one test, before the next read: at the
		// first kill each process read so far, those at whose read the floor did not hold among
		// them, and after it the read in hand, where it grew since the previous kill or the urgent
		// rule ends it (INFRA-3531 R1 of spec version 43, verification version 36). keepGrowth
		// answers as it did at that read: atKill has held the process's key since then, and
		// nothing keeps sizes as of a kill before endRest (keepKillSizes). triedHere passes over
		// the first kill, the wait's one kill since sendFirst, and over each process read before
		// it whose own kill failed (noteKillFailure): a retry would send that kill again and log a
		// second kill-failed line.
		for ; swept < len(kept); swept++ {
			if p := &kept[swept]; !g.triedHere(p) && (keepGrowth(g.atKill, p) > 0 || g.urgentEnds(p, history)) {
				g.sendUrgent(*p)
			}
		}
	}
	if sent {
		for _, k := range before {
			if !k.exited {
				left = append(left, k.key)
			}
		}
	}
	return sent, history, left
}

// stillRuns is whether c, a process the guard read at an earlier reading, still runs: its pid's
// stat, read now, has c's start time and shows no exit (exited), so the pid still names c and c
// has not ended, a zombie its parent has not reaped among the ended (endedSince). A stat that
// cannot be read, as a missing process's (ENOENT, or ESRCH where it ends during the read), or
// that does not parse, shows no process that runs. It reads nothing of c's size (endKnown).
func (g *Guard) stillRuns(c *candidate) bool {
	dir := filepath.Join(g.proc, strconv.Itoa(c.PID))
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return false
	}
	p, err := parseStat(stat)
	return err == nil && p.StartTime == c.StartTime && !exited(dir, p.state)
}

// urgentFirst is, at a reading at or above the urgent share (urgentShare) where cs, the processes
// the guard may end in the order of ends (rank), hold one that is not new to the stay (history,
// withHistory) and one of the sets the urgent rule ends for the stay (endsInStay), the index in cs
// of the first process that rule ends there (urgentEnds). Below the urgent share, where every
// process is new to the stay, and where none is of those sets, it is -1. In the wait after a kill
// such a process is a reason to act, as growth since the kill is, and the first kill goes to that
// first one (decideWait, act; INFRA-3531 R1 of spec version 46: "a process of these three sets is
// a reason to act, as a process new to the stay is"). That first one is cs[0] where a process grew
// over the window, since rank puts those first, and where the new tier or the growth since the
// stay's first kill is the reason, since the order puts those before the rest; but where a process
// that started during the stay, is no fork and holds memory of its own is the one reason, the order
// can put before it a larger process that has not grown since the stay's first kill, such as
// r206's 2.4 GB parent (`4g-r15latepool-r206`; endsInStay), which INFRA-3368's architect's
// decision of 2026-11-20T14:00Z keeps the urgent rule from ending.
func (g *Guard) urgentFirst(s Settings, reading, limit uint64, cs []candidate, history bool) int {
	if reading < urgentShare(limit, s.ActPercent) || !history || !slices.ContainsFunc(cs, func(c candidate) bool { return g.endsInStay(&c) }) {
		return -1
	}
	return slices.IndexFunc(cs, func(c candidate) bool { return g.urgentEnds(&c, history) })
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

// urgentShare is the reading at and above which a kill ends, beside the first in the order, every
// process the guard may end that grew over the window and, where the decision holds a process that
// is not new to the stay, every one of the sets the urgent rule ends for the stay (endUrgent,
// endsInStay, INFRA-3531 R1 of spec versions 39, 41, 42 and 46): the mark at act percent of limit
// plus half the distance from the mark to limit, 92.5% of limit at the default act share of 85. It
// derives from the act share, so it has no setting of its own.
func urgentShare(limit uint64, act int) uint64 {
	mark := share(limit, act)
	return mark + (limit-mark)/2
}

// assessAt is assess's assessment of the processes at the reading taken at now, on again, the reads
// again of that reading (readAgain), with the guard's smaps buffer let go of once it has grown past
// maxHeldFile, and false where the guard cannot list the processes, which turns it off for good.
func (g *Guard) assessAt(now time.Time, again readAgain) (assessment, bool) {
	a, err := g.assess(now, again)
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
// stops there, unless a process it may end has grown since the kill or, at a reading at or above
// the urgent share in a stay with history, one is of the sets the urgent rule ends for the stay
// (decideWait, urgentFirst), rather than end process after process for memory no kill frees.
func fell(atKill, reading, mark uint64) bool { return reading < atKill || reading < mark }

// act ends the first process of a, the assessment of this reading, in the guard's order of ends
// (rank), or, where first is not 0, the process of a at first, which urgentFirst chose in the wait
// after a kill, or gives the rise up when the processes it may end hold less than the floor between
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
// sizes (sizesReading), but where, at an urgent reading of the wait, the reads again before the
// full read have ended a process that grew since the kill (endKnown). The kills, one or, at a
// reading at or above the urgent share, beside it each the urgent rule ends, on history, whether a
// holds a process that is not new to the stay, which act's caller takes once for the decision
// (urgentEnds), are sent as below, and a is then what the next reading of the wait reads again
// first (endRest, lastRead). Its reads come after the reading, so a step between the reading and
// them is in them: a size as of the kills is the lowest of a process's last read before the
// reading and its reads at it before the kill, its read in a and any read at the mark before it
// among them, of those of the program it runs (keepKillSizes; run r31 at 0028eb7d).
//
// Above the floor it sends the first (sendFirst), whose line names the urgent rule where first is
// not 0, since the order did not put it first (INFRA-3531 R1 of spec version 46), and then the rest
// (endRest). Where it does not send the first, it sends none and keeps nothing, and the guard
// decides again at the next reading. endHeld sends its kills as act does, its first before its
// search where a held process grew at an urgent reading at the mark, and at an urgent reading in a
// stay with history reads on after them, ending what the read-on reads (readOn), before the lines
// and reports.
func (g *Guard) act(now time.Time, s Settings, reading, limit uint64, a *assessment, first int, history bool) {
	if len(a.endable) == 0 || a.endableBytes < share(limit, s.FloorPercent) {
		g.stopRise(now, reasonNothingToEnd, reading, limit, a)
		return
	}
	if !g.sendFirst(now, s, reading, a.endable[first]) {
		return
	}
	if first > 0 {
		// The order puts another process first: the urgent rule chose this one (urgentFirst).
		g.sent[0].rule = ruleUrgent
	}
	g.endRest(s, reading, limit, a, history)
}

// endRest finishes the kills of a reading whose first kill is sent (sendFirst), a being the
// decision's processes in the order of ends: it ends each other that the urgent rule ends on
// history, the decision's, at a reading at or above the urgent share (endUrgent, urgentEnds),
// writes every kill's line and report with the reading's own sizes (reportKills, sizesReading),
// keeps the sizes as of the kills (keepKillSizes), and keeps a as what the next reading of the wait
// reads again first (lastRead), in that order. act and the urgent reading of the wait after its
// reads again (watchUrgent) call it.
func (g *Guard) endRest(s Settings, reading, limit uint64, a *assessment, history bool) {
	g.endUrgent(s, reading, limit, a.endable, history)
	g.reportKills(reading, limit, sizesReading, "")
	g.keepKillSizes()
	g.lastRead, g.lastTable = a.endable, a.table
}

// sendFirst sends the first kill of the reading taken at now, of first, the first in the order of
// ends or, at an urgent reading of the wait, the first process read again that grew since the
// kill (endKnown), with the rule that put it first (candidate.rule), and answers whether it sent
// it. Each kill is signalKill's: a process that ended or whose pid another process took is not
// killed, and then nothing is sent. Once it is sent, the wait after the kills of the reading
// starts (watch), in place of any wait before it, whose kills endWait or endKnown has taken: it
// lasts while one of them has not exited, and no longer than the wait setting. From then on a read
// at now is one after the kill (keepSize). Each kill of the reading is at its own time by the
// guard's clock (Kill.At), and none has its line and report before every kill of the reading is
// sent (reportKills).
func (g *Guard) sendFirst(now time.Time, s Settings, reading uint64, first candidate) bool {
	h := g.signalKill(first)
	if h == nil {
		return false
	}
	g.phase = waiting
	g.wait = afterKill{
		kills:    append(g.wait.kills[:0], killedProc{handle: h, key: procKey{pid: first.PID, start: first.StartTime}}),
		reading:  reading,
		at:       now,
		deadline: now.Add(s.Wait),
	}
	g.sent = append(g.sent[:0], sentKill{target: first, rule: first.rule(), at: g.clock.Now()})
	return true
}

// endUrgent ends, at a reading at or above the urgent share (urgentShare), each process of cs, the
// decision's processes in the order of ends, other than the first kill sendFirst sent, that the
// urgent rule ends (urgentEnds), history being the decision's, by that rule (sendUrgent). Below
// the urgent share it ends nothing. At the reading at the mark the processes new to the stay that
// the reads of the decision did not reach are the read-on's, after these kills (readOn). At an
// urgent reading of the wait where the reads again before the full read sent kills (endKnown), cs
// is the full read's processes with those killed at that reading left out, and these kills follow
// theirs (watchUrgent).
//
// rank puts the processes that grew over the window first, the most grown first, the new tier next,
// the largest first, and the growth since the stay's first kill after them, but a process that
// started during the stay, is no fork, holds memory of its own and is in none of these ranks with
// the rest by its size, after a larger one that never grew: the processes endUrgent ends are no run
// of cs, so it reads the whole of cs, and passes over the first kill wherever that is: where
// endHeld sent it before its search, a held process that a later read at the mark found grown more
// can rank before it, and in the wait it can be one urgentFirst chose. A kill it does not send, of
// a process that ended or whose pid another process took, is left out, and the others go on.
func (g *Guard) endUrgent(s Settings, reading, limit uint64, cs []candidate, history bool) {
	if reading < urgentShare(limit, s.ActPercent) {
		return
	}
	first := g.wait.kills[0].key
	for i := range cs {
		c := &cs[i]
		if (procKey{pid: c.PID, start: c.StartTime}) != first && g.urgentEnds(c, history) {
			g.sendUrgent(*c)
		}
	}
}

// urgentEnds is the urgent rule's test of a process, the one test of whom it ends (endUrgent,
// readOn, endKnown, urgentFirst): whether, at a reading at or above the urgent share
// (urgentShare), the rule ends c, a process the guard may end: where c grew over the window, and,
// where history holds, where c is of the sets of the stay (endsInStay). So every such process ends
// at that reading: near the limit one kill a reading cannot absorb a burst of several growers
// (INFRA-3531 R1 of spec versions 39, 41, 42 and 46, the last by INFRA-3368's architect's decisions
// of 2026-11-20T13:55Z, 14:00Z, 14:05Z and 14:58Z). This issue's tester measured, at 8023d298, a
// reading 148 ms late on a 1.5-CPU pod let six growers' first 64 MiB steps land before the first
// reading at the mark, which read 93.88% of the limit, and one kill a reading then left the pod
// 2.8 MiB from the kill point (`4g-t4latepool-r2`); and at a82a9f2c six growers started between
// the band's last reading and the mark, so that the search found each with one read and no growth
// over the window at the first reading at the mark, 93.95%, which ended one of them, and the pod
// came 26.5 MiB from the kill point (`4g-latepool-4tpqx`); and at 00405eb3 four growers that
// started together inside the band and had not yet stepped were in no set it ended at an urgent
// reading of the wait (`4g-r15latepool-r206`; endsInStay).
//
// history is whether the decision holds a process that is not new to the stay (withHistory), one
// read at two readings of it in a row before the one decided on, on the set it decides on once its
// reads have left out what they could not read or that has ended. At the urgent reading of the wait
// the reads again take it before they read, from a process they are to read again that is not new
// to the stay and still runs by its stat, so one whose smaps then cannot be read still gives it;
// where they send a kill, the full read's kills take it too (endKnown, stillRuns, watchUrgent;
// INFRA-3368's architect, 2026-11-21T08:54Z, item 1). Each caller says how it takes it (endHeld,
// act, decideWait, endKnown, watchUrgent; INFRA-3531 R1 of spec versions 42, 43 and 48,
// INFRA-3368's architect, 2026-11-20T19:50Z). It is by each process's newness alone, so a held
// process new to the stay that no read at the mark reached is new here (candidate.unreached). Only
// where one is not new does being new to the stay tell a burst from the rest of the pod: where no
// reading of the stay came before the one decided on, or one alone, as at the guard's first
// reading, at a reading that crossed the whole band within one interval and at a band of one
// reading, every process is new to the stay, and the urgent rule would end every process the guard
// may end with memory of its own; there urgency adds the window's growers alone, and none of the
// sets of the stay, though a process started during the stay: the decision holds no process that
// tells the burst from the rest (INFRA-3368's architect's decision of 2026-11-20T14:05Z). In
// `4g-latepool-4tpqx` the band held six readings, so the probe's parent and its forks were not new
// and the six growers were.
func (g *Guard) urgentEnds(c *candidate, history bool) bool {
	return c.growth > 0 || history && g.endsInStay(c)
}

// withHistory is whether cs, the processes a decision is made on, holds one that is not new to the
// stay: the stay has history, and being new to it, starting during it or growing since its first
// kill tells a burst from the rest of the pod (urgentEnds; INFRA-3531 R1 of spec versions 42, 43
// and 46, the last by INFRA-3368's architect's decision of 2026-11-20T14:05Z).
func withHistory(cs []candidate) bool {
	return slices.ContainsFunc(cs, func(c candidate) bool { return !c.newToStay })
}

// endsInStay is whether c, a process the guard may end, is of the sets the urgent rule ends for the
// stay beside the processes that grew over the window, where the decision holds one that is not
// new to the stay (urgentEnds): where c is new to the stay and holds memory of its own (inNewTier);
// where it grew since the stay's first kill, growthNoise or more, as the order's third tier
// measures it with this reading's reads (sinceFirstKill), none at the reading of that kill itself,
// whose sizes as of it are that reading's own; and where it started during the stay
// (startedInStay), is not a fork and holds memory of its own, its rank size above nothing
// (INFRA-3531 R1 of spec version 46, by INFRA-3368's architect's decisions of 2026-11-20T13:55Z,
// 14:00Z, 14:05Z and 14:58Z). A fork is one by R1's fork test, and stays one while it runs the
// program its fork's read found (candidate.fork, discountFork): an orphaned fork is still one, and
// a fork that has exec'd is no longer one, as r40's grower after its exec. So a sleeping fork that
// started during the stay is not ended for that, whether it ranks at nothing beside its parent, at
// the 1 MiB it holds over it (`4g-latepool-6gd5t`) or, its parent ended, at its whole size. A held
// process new to the stay that no read at the mark has reached is in none of these
// (candidate.unreached): the read-on reads it and decides on that read (readOn), as for the new
// tier.
//
// They are there because of `4g-r15latepool-r206` at 00405eb3: four growers that started together
// inside the band, read three times and not yet stepped at an urgent reading of 93.95%, were in no
// set the urgent rule ended there, which ended the one grower that had stepped; the four then
// stepped together within one 131 ms gap and brought the pod to 2.3 MiB from the kill point. Each
// of them started during the stay, is no fork and grew since its first kill from nothing
// (firstReadSince). The probe's 2.4 GB parent, which grew in the band before the first kill and has
// been flat since, is in none of them.
func (g *Guard) endsInStay(c *candidate) bool {
	return !c.unreached && (c.inNewTier() || c.sinceFirstKill > 0 || !c.fork && c.rankSize() > 0 && g.startedInStay(c))
}

// startedInStay is whether c started during this stay in the band: no process table of the stay
// before c's listed it by its pid and start time, which holds where the stay's first table, the
// table of its first reading at or above the band's floor (bandStart), does not list it (stayTable,
// lists; INFRA-3531 R1 of spec version 46, by INFRA-3368's architect's decision of
// 2026-11-20T14:05Z). A process that started in the interval just before that reading is listed
// there, and did not start during the stay (the architect's decision of 14:06Z). It is false while
// the stay has no first table.
func (g *Guard) startedInStay(c *candidate) bool {
	return g.stayTable != nil && !lists(g.stayTable, procKey{pid: c.PID, start: c.StartTime})
}

// sendUrgent sends the kill of c beside the first kill of the reading, by the urgent rule
// (ruleUrgent), at once: the wait after the kills holds it, and its line and report come with the
// reading's others (reportKills). A kill it does not send, of a process that ended or whose pid
// another process took, is left out (signalKill).
func (g *Guard) sendUrgent(c candidate) {
	if h := g.signalKill(c); h != nil {
		g.wait.kills = append(g.wait.kills, killedProc{handle: h, key: procKey{pid: c.PID, start: c.StartTime}})
		g.sent = append(g.sent, sentKill{target: c, rule: ruleUrgent, at: g.clock.Now()})
	}
}

// readOn reads on, after the kills of an urgent reading at the mark in a stay with history (the
// decision holds a process it may end that is not new to the stay, withHistory), through the
// processes new to the stay that no read at the reading taken at now has reached, in the groups
// and the order of the reads of the decision: the held ones that held memory of their own at their
// band read (readHeldNew), the ones started since the band's last reading, resuming sr, the
// decision's search, and listing them where that search did not (startedSince), and the held forks
// that ranked at nothing at their band read (readHeldNew), each group the newest first. It reads
// no process once one interval has passed since the reading at the mark (whole), and ends the one
// whose read is in hand: the clock is asked before each read. It reads no process it has killed at
// this reading, or whose kill failed, again. Each it reads it counts in read, and ends at once,
// before the next read, by the urgent rule (sendUrgent), where that rule ends it, the decision
// having history (urgentEnds): every process it reads is new to the stay, so that is where it grew
// over the window, holds memory of its own, its rank above nothing after the fork discount, or, by
// that read, grew since the stay's first kill, which no process has where this reading's first
// kill is the stay's first. A fork whose read parent covers it is not ended, and one whose parent
// the same round of the search reads after it waits for that read (startedSince). It does not read
// the floor again: the decision read it. A process the classifier protects, or one it cannot place,
// it neither reads nor ends.
//
// This is INFRA-3531 R1 of spec versions 43 and 46: at an urgent reading in a stay with history
// every process new to the stay that holds memory of its own ends, and every one grown since the
// stay's first kill. The half-interval budget bounds the reads that decide the first kill alone
// (readOnNew), so a slow reading at the mark reaches only
// some of those processes before it: in `4g-t4latepool-r3` at 4d6665d5, six growers started after
// the band's last reading, and at the first reading at the mark, at or above the urgent share, the
// search started after half an interval, each smaps read taking 22 ms, and read one grower past it;
// the guard ended that one alone, and the host ended the pod with the highest reading 25.5 MiB
// below the kill point. A process new to the stay that the read-on does not reach within one
// interval ends at the next reading of the wait at or above the urgent share (watch, urgentFirst;
// verification version 36).
func (g *Guard) readOn(now, whole time.Time, sr *startedSearch, read *newRead, cache *argsCache) {
	before := func() bool { return g.clock.Now().Before(whole) }
	take := func(c *candidate) bool { return before() && !g.triedHere(c) }
	end := func(c *candidate) {
		// Its growth since the stay's first kill by the read in hand, none where that kill is
		// this reading's (sinceStayKill).
		if g.firstKillKept {
			c.sinceFirstKill = g.growthSinceStayKill(c)
		}
		// The read-on runs only where the decision has history (endHeld).
		if g.urgentEnds(c, true) {
			g.sendUrgent(*c)
		}
	}
	g.readHeldNew(now, false, take, read, cache, end)
	// The read-on reads the processes started since one at a time from its first read, so that
	// each of its kills goes as its process is read (startedSince).
	oneAtATime := func() bool { return true }
	g.startedSince(sr, now, oneAtATime, before, read, cache, end)
	g.readHeldNew(now, true, take, read, cache, end)
}

// triedHere is whether the guard has killed c at this reading, once its first kill is sent
// (sendFirst), which starts the wait's kills afresh, or a kill of c has failed (noteKillFailure).
func (g *Guard) triedHere(c *candidate) bool {
	key := procKey{pid: c.PID, start: c.StartTime}
	if start, failed := g.killFailures[c.PID]; failed && start == c.StartTime {
		return true
	}
	return slices.ContainsFunc(g.wait.kills, func(k killedProc) bool { return k.key == key })
}

// reportKills writes the ended line and the report of each kill sent at the reading (sent), in the
// order of the kills, each carrying sizes and newRead, the decision's: it reads the counter and the
// reading's Shmem once, once every kill of the reading is sent, and builds the fields the kills
// share once. A read of the counter costs about 18 ms under gVisor, and a grower's kill that waited
// behind it and the lines and reports before it would wait for nothing the kill needs.
func (g *Guard) reportKills(reading, limit uint64, sizes, newRead string) {
	shared := Kill{
		LauncherMemoryKill: shimwire.LauncherMemoryKill{
			ReadingBytes: reading,
			CounterBytes: g.readCounter(g.clock.Now()),
			LimitBytes:   limit,
			Cause:        shimwire.MemoryKillCauseGuard,
		},
		Sizes:   sizes,
		NewRead: newRead,
	}
	shmem, _ := meminfoBytes(g.meminfoData, "Shmem")
	for _, k := range g.sent {
		kill := shared
		kill.PID = k.target.PID
		kill.Executable = processName(k.target.Process)
		kill.SizeBytes = k.target.size
		kill.At = k.at
		kill.Role = k.target.role
		kill.Rule = k.rule
		kill.LessParent = k.target.lessParent
		g.writeKill(kill, shmem)
	}
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
	if start, err := ReadStartTime(g.proc, target.PID); err != nil || start != target.StartTime {
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
// before it reads every size, the order's tier of the new takes those alone that a read at that
// reading reached: unreached is whether it is a held process new to the stay that no read at the
// mark reached (settleNew), which ranks with the other held processes and is new to the stay all
// the same, so whether the decision holds a process that is not new reads newToStay alone
// (endUrgent, INFRA-3531 R1 of spec version 43). nothingAtBand is, for a held process new to the
// stay at the reading at the mark, whether it ranked at nothing at its band read, a fork whose
// parent's size covered its own (orderHeld): the reads at the mark take those after the processes
// started since (readHeldNew). sinceFirstKill is how much it grew since the first kill of this
// stay in the band (sinceStayKill), 0 under growthNoise and wherever the stay has had no kill.
// descriptorBytes is the part of size that the memfds it holds by a descriptor alone make up
// (addHeldMemfds), which a read of its smaps again at the mark adds as it was counted
// (rereadHeld). lessParent is how much of size the tiers that rank by size do not count, for a
// fork that has not run a new program, and 0 for every other process, by the fork test alone
// (discountForks). fork is whether its read is a fork's read (discountFork): set where the read
// passes that test, or where the process still runs the executable and arguments that a read which
// passed it found (forkProgram), its parent gone or not (INFRA-3368's architect's answer of
// 2026-11-20T09:24Z). It says at a reading that measures growth since a kill whether a size as of
// the kill from a fork's read still counts (keepGrowth), and at an urgent reading whether a process
// that started during the stay is in the set the urgent rule ends for that (endsInStay; INFRA-3531
// R1 of spec version 46, by INFRA-3368's architect's decision of 2026-11-20T14:58Z).
type candidate struct {
	*Process
	role            string
	size            uint64
	growth          uint64
	newToStay       bool
	unreached       bool
	nothingAtBand   bool
	fork            bool
	sinceFirstKill  uint64
	descriptorBytes uint64
	lessParent      uint64
}

// rankSize is the size c ranks by where its tier ranks by size: its size less its parent's where
// it is a fork that has not run a new program (lessParent).
func (c *candidate) rankSize() uint64 { return c.size - c.lessParent }

// inNewTier is whether c is in the order's tier of the processes new to the stay: new to it, read
// at the reading at the mark where it is a held process there (unreached), and holding memory of
// its own, its rank size above nothing (INFRA-3531 R1 of spec versions 31 and 34). A fork whose
// parent's size covers its own is not a process the guard has learned anything from, and the tiers
// below rank it, by its growth since the stay's first kill or its rank size.
func (c *candidate) inNewTier() bool { return c.newToStay && !c.unreached && c.rankSize() > 0 }

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
//
// It sets each one's fork too, and the flag of its read at now, the reading taken at now, where
// the guard has one: whether that read is a fork's read, which says what a size as of a kill is
// (asOfKill, keepGrowth; INFRA-3368's architect's decision of 2026-11-20T08:13Z on INFRA-3531 R1
// of spec version 43, item 1). A read is a fork's read where it passes the same test, or where the
// process still runs the executable and arguments that a read which passed it found (forkProgram),
// its parent gone or not (the architect's answer of 2026-11-20T09:24Z); lessParent follows the
// test alone (discountFork). It sets that read's flag alone, never one copied from a read of
// another program, and keeps with it the parent whose program it found by the test
// (readParent; procReads.read; INFRA-3368's architect's rule of 2026-11-21T16:43Z on
// INFRA-3531 R1 of spec version 48, item 1, whose table asOfKill states).
func (g *Guard) discountForks(now time.Time, cs []candidate, t *Table, cache *argsCache) {
	for i := range cs {
		g.discountFork(now, &cs[i], t, cache)
	}
}

// discountFork sets c's lessParent and fork, as discountForks does, and the fork flag of c's read
// at now where the guard has one, with the parent whose program that read found (readParent).
// Where c has a read at now, every caller runs it after keeping that read (keepSize), so it sees
// the parent's reads as the discount does: a parent read at now counts as read in this stay for
// the flag as for lessParent, whether keepSizes kept its read before or after c's; and a fork the
// search read before its parent has both set again once that parent is read (startedSince). A
// later pass that reads the parent flags only its own read of c (procReads.read).
//
// A process stays a fork after a read that passed the test for as long as it runs the executable
// and arguments that read found (forkProgram), its parent gone or not: its fork flag stays set,
// and only its lessParent follows the test. An orphan still runs its parent's program, and under
// gVisor it still reports the pages it shared with its parent as its own, so losing its parent
// is no new program. With the test alone, a parent's sleeping forks counted as grown by their
// whole size once the guard had killed the parent, and ended by rule=grew-since-kill in the
// latepool replays of TestMemoryGuardEndsAGrowerBeforeTheSleepingForksOfAParentThatRunItsProgram,
// and in 48 of the 200 seeds of the simulation (rule orphan of
// TestMemoryGuardKeepsTheRulesOfR1WhereGrowersStepAtRandomInstants). This is INFRA-3368's
// architect's answer of 2026-11-20T09:24Z on its decision of 08:13Z on INFRA-3531 R1 of spec
// version 43. A fork that rewrites its arguments runs another program by this rule and counts from
// 0, the risk that decision accepts.
func (g *Guard) discountFork(now time.Time, c *candidate, t *Table, cache *argsCache) {
	c.lessParent, c.fork = 0, false
	// parent is the process whose program c's read found, the zero key where it found none.
	var parent procKey
	if p, listed := t.byPID[c.PPID]; listed && g.sameProgram(c.Process, p, cache) {
		parent = procKey{pid: p.PID, start: p.StartTime}
		if r := g.sizes[parent]; len(r.window) > 0 {
			c.lessParent, c.fork = min(r.window[len(r.window)-1].size, c.size), true
		}
	}
	key := procKey{pid: c.PID, start: c.StartTime}
	r, kept := g.sizes[key]
	if !kept {
		return
	}
	if c.fork {
		// forkProgram changes only where the fork's program does, so a fork read again makes no
		// map write.
		if p := programOf(c.Process); !r.forkProgram.is(p) {
			r.forkProgram = p
			g.sizes[key] = r
		}
	} else {
		c.fork = r.forkProgram.exe != "" && r.forkProgram.is(programOf(c.Process))
	}
	// r's atReading and parents share their arrays with the kept ones, so the read needs no map
	// write.
	if r.readAt(now) {
		i := len(r.atReading) - 1
		r.atReading[i].fork, r.parents[i].key = c.fork, parent
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

// assess scans the processes, asks the classifier, and reads the size of each process it may end
// (mayEnd): one that is not protected, not the guard's own and not unread. A process whose smaps
// cannot be read is unread too, since it cannot be ranked, unless it has ended since the scan
// read its stat (endedSince): it is then left out of the processes it may end and of unread
// alike. The processes it may end are the only ones whose files it reads, so they alone are the
// holders and the mappers of memfds among which a memfd counts once (addHeldMemfds). Each size is
// kept as read at now, the time of the reading the assessment is made at, and its growth measured
// from the reads kept before it (keepSizes). again is the reads made at that reading before it
// (readAgain).
func (g *Guard) assess(now time.Time, again readAgain) (assessment, error) {
	t, err := Scan(g.proc)
	if err != nil {
		return assessment{}, err
	}
	g.applyKillFailures(t)
	classes := g.classes(t)
	a := assessment{table: t}
	clear(g.memfds)
	maps.Copy(g.memfds, again.memfds)
	for _, p := range t.Processes() {
		if !g.mayEnd(t, classes, p.PID) {
			continue
		}
		size, reread := again.sizes[procKey{pid: p.PID, start: p.StartTime}]
		if !reread {
			var err error
			if size, err = g.size(p.PID, g.memfds); err != nil {
				if !errors.Is(err, fs.ErrNotExist) && !endedSince(filepath.Join(g.proc, strconv.Itoa(p.PID)), p.StartTime) {
					t.unread[p.PID] = fmt.Errorf("read the smaps: %w", err)
				}
				continue
			}
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
	g.discountForks(now, a.endable, t, &cache)
	rank(a.endable)
	a.endableBytes = sizeSum(a.endable)
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
// guard has read before allocate nothing. The assessment is the last pass of its reading (keepSize),
// so once its reads are kept every read of that reading is, and keepSizes settles the fork flags
// of the reads at now of each process read there (settleForks).
func (g *Guard) keepSizes(now time.Time, cs []candidate, t *Table) {
	for i := range cs {
		g.keepSize(now, &cs[i])
	}
	for key, r := range g.sizes {
		switch {
		case r.readAt(now):
			g.settleForks(now, key, r)
		case !lists(t, key):
			delete(g.sizes, key)
		case r.readings != 0:
			r.readings = 0
			g.sizes[key] = r
		}
	}
}

// settleForks settles the fork flags of r's reads at the reading taken at now, the reads of the
// process key names, at that reading's last pass, its assessment (keepSizes). A read that found
// its parent's program by discountFork's own test while that parent had no read in this stay has
// no flag from discountFork: the parent's read came after it, by a later pass of the reading, as
// where the search at the mark reads a fork before its parent and stops, and the full read after
// the kill reads the parent. That read is a fork's read where the parent has a read at now
// (readParent.readThere), whether or not the later pass reads the process again, and whatever
// program it finds it running there (INFRA-3368's architect's rule of 2026-11-21T16:43Z on
// INFRA-3531 R1 of spec version 48, item 1; row (2) of asOfKill's table). This runs once for each
// reading of the process, at that reading, and the answer stays with the read once the parent's
// newest read moves on at a later reading, so a parent read only at a later reading makes it no
// fork's read (TestMemoryGuardTakesNoForksReadFromAParentFirstReadAtALaterReading). The process's
// latest fork's read by the test, discountFork's or one this settles, gives its fork program
// (forkProgram), so an orphan that still runs that program stays a fork, as after a read
// discountFork flagged (the architect's answer of 2026-11-20T09:24Z;
// TestMemoryGuardTakesTheSearchsReadOfAForkAsAForksReadWhereTheFullReadReadsItsParentAndNotTheFork).
// It writes the map only where the fork program changes, and allocates nothing: parents shares its
// array with the kept one.
//
// At 6a1d8e2f a read took a flag only from discountFork or from a read there that discountFork
// flagged and that found the same program. In the efficiency review's probe of the simplify pass
// of 6a1d8e2f, the search at a later mark read a fork before its parent and stopped, the kill
// went, and the fork ran a program of its own before the full read after the kill, which read the
// parent: no read of the fork was a fork's read, its size as of the kill was its parent's shared
// pages, 305 MiB, as a size that counts whatever it runs, its program's growth since the kill was
// hidden, and the guard did not go on (kills [300 402];
// TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkThatRunsItsOwnProgramBeforeTheFullRead).
func (g *Guard) settleForks(now time.Time, key procKey, r procReads) {
	latest := r.forkProgram
	for i := range r.parents {
		p := &r.parents[i]
		if p.key == (procKey{}) {
			continue
		}
		if !r.atReading[i].fork {
			parent := g.sizes[p.key]
			p.readThere = parent.readAt(now)
		}
		if r.atReading[i].fork || p.readThere {
			latest = r.programs[i]
		}
	}
	if !r.forkProgram.is(latest) {
		r.forkProgram = latest
		g.sizes[key] = r
	}
}

// keepSize keeps c's size as read at now and sets c's growth, its size less its size at its oldest
// read kept within growthWindow before now where that is growthNoise or more. A read older than
// the window counts for nothing and is dropped. A read already kept at now, a read at the mark
// (rereadHeld, readHeldNew, startedSince) where the assessment of that reading follows it, is
// replaced in the window: a process has one read at each reading there. A process with no earlier
// read in the window has not grown: a fresh fork reads its parent's shared pages as its own.
//
// It counts the reading as one more that read c (procReads.readings) and sets c's newToStay where
// fewer than two readings before now read it in a row: none, or only the one before
// (newToStayAt, which endKnown asks too before it reads c). A reading that does not read a
// process its table still lists restarts its count (keepSizes), and the guard drops all of them
// wherever it reads none (forgetSizes), where a stay ends, so the count is of the readings of this
// stay, and two of them in a row that read c are the band's last two at the reading at the mark,
// and the two before the one decided on at any later reading of the rise.
//
// It keeps too what c's size as of a kill sent at now is taken from (asOfKill): at the first read
// of c at now, its newest read before it, with that read's own fork flag (procReads.read), as its
// last read before this reading (before); and every read of c at now, in the order the guard makes
// them (atReading), counting those before this reading's first kill (sendFirst, which sets the
// wait's at to now) in beforeKill, each with the program it found (programs) and, by discountFork,
// what it found of its parent (parents). That first read clears firstTaken (takeFirstSince). A
// reading can read c more than once before its kill: at the mark, the reads again (rereadHeld)
// and, where they send no kill (endHeld), the assessment act decides on, and a step between two of
// them is growth since the kill. So a read after a kill at the mark replaces the decision's read in
// the window, and not in atReading. Which of those reads are of another program than the one c
// runs now asOfKill says, by each read's own fork flag, since a read is of another program only
// where it was a fork's read and the later one is not (anotherProgram; INFRA-3368's architect's
// decision of 2026-11-20T08:13Z on INFRA-3531 R1 of spec version 43, and its rule of
// 2026-11-21T16:43Z, item 1). Whether this read is a fork's read discountFork says, after this,
// once every read it compares the parent's with is kept: keepSizes keeps a whole reading's reads
// before discountForks runs, and the search finds a fork's parent after the fork (startedSince); a
// later pass flags only its own read (procReads.read). A reading reads c at most twice: once at the
// mark, where each read there (rereadHeld, readHeldNew, startedSince, readOn) takes only a process
// no read before it reached at now, or in the wait before its full read (endKnown), and once by its
// assessment (assess), which a reading makes once (decide, watch), act reading nothing after it:
// the assessment is the reading's last pass. So c's first read gives atReading, programs and
// parents room for two each, and all of it keeps its storage: a process read before allocates
// nothing. A program holds the read's own executable and list of arguments and copies neither
// (programOf).
func (g *Guard) keepSize(now time.Time, c *candidate) {
	key := procKey{pid: c.PID, start: c.StartTime}
	r := g.sizes[key]
	// again is whether c has a read at now already: windowStart never drops it, so it stays the
	// window's newest.
	again := r.readAt(now)
	if !again {
		r.readBefore = len(r.window) > 0
		if r.readBefore {
			r.before = r.read(len(r.atReading) - 1)
		} else {
			r.atReading, r.programs, r.parents = make([]forkRead, 0, 2), make([]program, 0, 2), make([]readParent, 0, 2)
		}
		r.atReading, r.programs, r.parents, r.beforeKill, r.firstTaken = r.atReading[:0], r.programs[:0], r.parents[:0], 0, false
	}
	// A reading's reads before its first kill come before every read after it.
	if !g.wait.at.Equal(now) {
		r.beforeKill++
	}
	r.atReading, r.programs, r.parents = append(r.atReading, forkRead{size: c.size}), append(r.programs, programOf(c.Process)), append(r.parents, readParent{})
	c.newToStay = r.newToStayAt(now)
	reads := r.window[:copy(r.window, r.window[windowStart(r.window, now):])]
	if again {
		reads = reads[:len(reads)-1]
		r.readings--
	}
	c.growth = growthOver(reads, c.size)
	r.window, r.readings = append(reads, sizeRead{at: now, size: c.size}), r.readings+1
	g.sizes[key] = r
}

// keepStays counts each of cs's reads at the armed guard's reading in the band, whose table is t,
// as one of its stay in the band (stays), and keeps whether it has grown by growthNoise or more
// between two of them: above the least of its earlier reads of the stay, so a grower whose steps
// are wider apart than the window, or slower than growthNoise from one reading to the next,
// counts. A process t lists that this reading did not read keeps its stay as it was: a read that
// failed between two reads of the stay does not undo the growth between them (INFRA-3531 R1:
// "grew in the band" is by two of its reads in the band). The stays of each process t no longer
// lists are dropped, so those kept are bounded by the processes the pod runs, and a process read
// before allocates nothing; every process of cs is one t lists.
func (g *Guard) keepStays(cs []candidate, t *Table) {
	for _, c := range cs {
		key := procKey{pid: c.PID, start: c.StartTime}
		stay, known := g.stays[key]
		if known && c.size >= stay.least+growthNoise {
			stay.grew = true
		}
		if !known || c.size < stay.least {
			stay.least = c.size
		}
		g.stays[key] = stay
	}
	for key := range g.stays {
		if !lists(t, key) {
			delete(g.stays, key)
		}
	}
}

// keepStayTable keeps t, the table of an armed guard's reading at or above the band's floor
// (bandStart), as the stay's first table where the stay has none (stayTable is nil): that table
// lists, by pid and start time, every process the scan read a stat of, protected ones among them.
// A stay begins at its first reading at or above the band's floor (INFRA-3531 R1 of spec version
// 46), and its first table is the first band reading's, or, where no armed reading in the band came
// before the reading at the mark (the guard had just started or been turned on, or the reading
// crossed the whole band within one interval), the reading at the mark's. No later reading of the
// stay, above the mark or after a kill, replaces it, and it goes where the stay ends (forgetSizes).
// A process that table does not list started during the stay (startedInStay).
func (g *Guard) keepStayTable(t *Table) {
	if g.stayTable == nil {
		g.stayTable = t
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
// (held), the stays in the band, the stay's first table (stayTable) and the sizes as of the stay's
// first kill (atFirstKill), wherever it reads none: below the band, while off and while a rise is
// given up (decide, stopRise). The reads it keeps are so those of readings in a row in the band, at
// the mark and after a kill, and a stay in the band starts again at the next armed reading at or
// above the band's floor, with no first table and no first kill. A reading below the mark that is
// in the band ends a rise and not its stay, and drops none of them.
func (g *Guard) forgetSizes() {
	clear(g.sizes)
	clear(g.stays)
	g.held, g.heldTable = nil, nil
	g.stayTable = nil
	if g.firstKillKept {
		clear(g.atFirstKill)
		g.firstKillKept = false
	}
}

// keepKillSizes keeps the size as of the kills of the wait (g.wait), sent at the reading taken at
// its at, of each process the guard has read in this stay that the table of that reading's last
// read lists (sizes). Every path that sends a kill calls it once those kills are sent and that
// last read is made: act and the wait's urgent reading after its early kills (endRest), and decide
// after a kill on the band's held reads (endHeld). One helper, asOfKill, says what each size is,
// by INFRA-3368's architect's decision of 2026-11-20 on INFRA-3531 R1 of spec version 43: the
// lowest of the process's reads of the program it runs now, from its last read that finished
// before the kills' reading up to the decision's read; 0 where every read of it before the kills
// is of another program; and its first read after the kills only where the guard never read it
// before them. That anchors the sizes at the same reading as R1's "the reading at the kill".
// "Another program" is R1's own fork test, by INFRA-3368's architect's decision of
// 2026-11-20T08:13Z on INFRA-3531 R1 of spec version 43: a read is of another program than a
// later one only where it was a fork's read and the later one is not (anotherProgram). Each size
// is kept with whether the read it came from was a fork's read, by that read's own flag
// (procReads.read; INFRA-3368's architect's rule of 2026-11-21T16:43Z, item 1, whose table
// asOfKill states): a read at the kills' reading is a fork's read where it found its parent's
// program, its parent read there before or after it (settleForks), and its flag is never copied
// from a read of another program; a read before that reading keeps the flag it had at its own
// reading. firstReadSince takes the same flag. A size from a fork's read counts only while the
// process is still a fork: at a reading that finds it no longer one, its size as of the kills is 0
// (keepGrowth). A process read as a fork stays one, its parent gone or not, until its executable or
// arguments differ from what that read found (discountFork; the architect's answer of
// 2026-11-20T09:24Z). A size from any other read counts whatever the process's executable or
// arguments become.
//
// Each size is kept as the size as of the previous kill of the rise (atKill), from which
// grownSinceKill measures growth until the next kill, a go-on; the sizes of the kill before go.
// At the first kill of the stay in the band each is kept as the size as of that kill too
// (atFirstKill), from which sinceStayKill measures the order's third tier for the rest of the
// stay, across a dip below the mark that stays in the band. At the stay's first kill a process the
// guard never read before it has firstReadSince's size as of it. A process the guard first reads at
// a later reading has no size here: keepGrowth keeps one from that first read, and
// growthSinceStayKill keeps one in atFirstKill by firstReadSince. At a later kill this takes again
// each such size still marked (procReads.firstTaken), from all of the process's reads at the
// reading it was taken at (takeFirstSince).
//
// So a size as of the kills is never above a process's last read of its program before the kills'
// reading, and a step after that read counts as growth since the kills; a read after the first
// kill gives a size only to a process never read before, but where it is a fork's read it makes
// each read of the same program at the kills' reading one too (procReads.read), and where it is
// a read of a process's parent it makes each earlier read there that found that parent's program
// one too (settleForks). These runs and tests measured what a later read or another identity
// hides:
//   - At 7356e0f6, in `8g-latepool-q56hm`, the growers stepped between the reading at the mark and
//     the guard's read after its kill. A guard that kept that read saw no growth at the next two
//     readings, not fallen and 28 MiB above the kill's reading, and waited.
//   - At 0028eb7d, in r31, act's reads at a reading came after that reading, and four growers
//     stepped 64 MiB in between. A guard that kept those reads saw nothing grown since that kill
//     at the next reading, 155 MiB above the reading at the kill, and ended nothing. Their reads at
//     the reading before are lower, so the steps count as growth since the kill.
//   - At 7512cc38, in r40, the band read a grower between its fork and its exec, at its parent's
//     2.4 GB. A guard that took the grower's read after the kill, 69.15 MiB, saw no growth since
//     the kill, and after a fall ended the probe's parent by rule=largest while the grower lived.
//     Every read of it before the kill is a fork's read, and its read after the kill is not, so its
//     size as of the kill is 0, or, where the kill's reading read it before the kill, a fork's
//     read, which counts from 0 at a reading where it is no fork.
//   - A held process whose read again at the mark fails has its band read before the kill, so it
//     keeps that read, not its read after the kill.
//   - A process the band read between its fork and its exec that execs only after the kill's
//     reading is a fork at every read up to the last of that reading, and so has its parent's size
//     as of the kill, from a fork's read. At a reading where it is no fork, that size is 0, so all
//     that its program holds counts as growth since the kill, as in r40.
//   - The tester's test of a28d75a8 has a process rewrite its arguments after a kill, as
//     setproctitle does. At 28732f65 a guard that took executable and arguments for the program
//     measured the 600 MiB that process always held from 0 and ended it by rule=grew-since-kill.
//     It was no fork, so its size as of the kill counts whatever its arguments become.
//   - At 28732f65, in `8g-latepool-r2`, the guard first read five of six growers after the stay's
//     first kill, kept those first reads as their sizes as of it, saw none grown since, and its
//     third kill ended the probe's 4.8 GB parent by rule=largest while four of them lived. None of
//     them was a fork at that first read, so each counts from 0 in the third tier.
//   - At c1c86b9e, in the deep review's probe of run r40's shape, the search at a later mark read
//     a fork before its parent and stopped before the parent, the kill went, and the full read
//     after it read the parent and flagged only its own read of the fork. A guard that took the
//     search's read by the flag discountFork set on it kept the fork's 305 MiB, its parent's
//     shared pages, as a size that counts whatever the fork runs; once the fork ran a program of
//     its own that came to hold 140 MiB, it saw no growth since the kill and did not go on. The
//     search's read found its parent's program, read at that reading, so it is a fork's read too,
//     and that size counts from 0 once the fork runs a program of its own; at 6a1d8e2f that held
//     only where the full read found the fork still running that program (settleForks; thread
//     PRRT_kwDORFy7ds66UWB0 of review round 5 of cybermonklabs/legion#4273;
//     TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkTheSearchReadBeforeItsParentBeforeTheKill;
//     INFRA-3368's architect, 2026-11-21T08:54Z, item 2).
//   - At 6b8c4d9b, in the deep review's probe of round 6, the reads again at a mark read a fork
//     that runs its parent's program, a fork's read, and the fork ran a program of its own, at
//     150 MiB, before the assessment's read of the same reading, which act's kill followed. A
//     guard that gave each read of the kill's reading the flag of any of them kept 150 MiB from a
//     fork's read, which counts from 0 once the process is no fork, saw 150 MiB of growth since
//     the kill where nothing grew, and ended the fork by rule=grew-since-kill. Each read keeps its
//     own flag, so the fork's read is of another program and that size is the new program's read,
//     no fork's (thread PRRT_kwDORFy7ds66ZPu6 of review round 6 of cybermonklabs/legion#4273;
//     TestMemoryGuardTakesNoGrowthSinceAKillFromAForkThatRunsItsOwnProgramBetweenTwoReadsOfTheKillsReading).
func (g *Guard) keepKillSizes() {
	first := !g.firstKillKept
	g.firstKillKept = true
	clear(g.atKill)
	for key, r := range g.sizes {
		asOf, afterKill := r.asOfKill(g.wait.at)
		g.atKill[key] = asOf
		switch {
		case first && afterKill:
			g.takeFirstSince(key, r)
		case first:
			g.atFirstKill[key] = asOf
		case r.firstTaken:
			// A size firstReadSince took from the reads of the process's newest read's reading,
			// whether or not that is this kill's: the next reading that reads the process clears
			// the mark (keepSize), so a process this kill's reading listed and did not read keeps
			// it, and its size is taken again from an earlier reading's reads. Taken from the reads
			// the process has, the size is the same either way, and where those reads are an
			// earlier reading's it is the one atFirstKill already holds. So it is taken again,
			// whether atFirstKill still holds it or not, since the outcome is the same.
			// firstTaken is set only by takeFirstSince, which keeps the size in atFirstKill with
			// it, and atFirstKill loses a size only where the stay ends (forgetSizes), which
			// empties sizes too, or by dropEnded, which drops a key the reading's table does not
			// list: the process has ended, since a pid that names another process has another
			// start time, so another key. So a marked size gone here is that of a process that
			// ended: read at the mark, its size taken, and ended before the full read after a kill
			// there, whose reads made now sizes keeps (keepSizes), as seeds 11 and 97 of
			// TestMemoryGuardKeepsTheRulesOfR1WhereGrowersStepAtRandomInstants reach. takeFirstSince
			// then keeps a size for that key and changes nothing else: the mark is set, so it
			// writes no read. atFirstKill is read only by key, for a process a reading lists
			// (growthSinceStayKill and keepGrowth, by a candidate's key), so nothing reads that
			// size, and the next sinceStayKill drops it (dropEnded), or the stay's end does, as the
			// stay's first kill keeps one above
			// (TestMemoryGuardGoesOnWithTheThirdTierCountedFromNothingWhereAProcessReadAtALaterMarkEndedBeforeTheFullRead;
			// INFRA-3368's architect, 2026-11-21T10:09Z; thread PRRT_kwDORFy7ds66UWB6 of review
			// round 5 of cybermonklabs/legion#4273).
			g.takeFirstSince(key, r)
		}
	}
}

// asOfKill is the size as of a kill sent at the reading taken at at of the process whose reads r
// are, r's newest read being at that reading or before it (keepKillSizes), with whether the read
// it came from was a fork's read; and afterKill, whether it is the process's first read after the
// kill. Each read has its own fork flag (read): a read at that reading is a fork's read where it
// found its parent's program, its parent read there before or after it, and its last read before
// that reading keeps the flag it had at its own reading. The program the process runs now is its
// newest read's, and a read is of another program only where it was a fork's read and the newest
// is not (anotherProgram).
//   - Where that reading did not read it, its newest read is its last before that reading, and
//     that read is its size.
//   - Where it did, its size is the lowest of its reads of the program it runs now among its last
//     read before that reading (before) and its reads at that reading before the reading's first
//     kill (the first beforeKill of atReading), with that read's own flag.
//   - Where each of those reads is of another program, as where the band read it between its fork
//     and its exec, it has run a new program since, and its size is 0: all that program holds came
//     after those reads.
//   - Where it has neither of those reads, the guard never read it before the kill, and its size
//     is its first read after the kill, with that read's own flag. Only that process takes a read
//     after the kill, since a fork reports its parent's shared pages as its own. At the stay's
//     first kill keepKillSizes takes firstReadSince's size in its place.
//
// At c1c86b9e it took each read at that reading by the flag of the process's newest read. Where
// the search at a mark read a fork before its parent and stopped, and the full read after the kill
// read the parent, the search's read had no flag, so the fork's size as of the kill, its parent's
// shared pages, counted whatever program it ran next, and its program's growth since the kill was
// hidden (thread PRRT_kwDORFy7ds66UWB0 of review round 5 of cybermonklabs/legion#4273; INFRA-3531
// R1 of spec version 48; INFRA-3368's architect, 2026-11-21T08:54Z, item 2).
//
// The fork-flag rule is INFRA-3368's architect's rule of 2026-11-21T16:43Z on INFRA-3531 R1 of
// spec version 48 (dispatch://INFRA-3531/comment/4c8b69b6-6047-4b2b-9380-55e6536809aa, item 1):
//   - A read is a fork's read when the program it found, its executable and arguments, is its
//     parent's program, as read anywhere in the same reading or earlier in the stay.
//   - The flag belongs to each read on its own. It is never copied from a read of another program.
//   - The size at a kill is the lowest of the reads of the program the process runs now, together
//     with that read's own flag. With no such read, the size is 0, unless the guard never read the
//     process before the kill.
//
// The rule's table has one row for each case the architect named. Each row gives the case, which
// of the process's reads are a fork's reads, its size as of a kill that follows, and the test that
// pins the row. "The kill's reading" is the reading the kill is sent at. "The reading before" is
// the process's last read at an earlier reading (before). The sizes in MiB are those of the tests.
//   - (1) The same search reads the fork before its parent, and then the parent. Fork's reads: the
//     search's read, which the search flags once it reads the parent (startedSince), and each
//     later read there that finds the parent's program, as the full read's after the kill. Size:
//     the lowest of them before the kill, a fork's read (305). It counts from 0 once the fork runs
//     a program of its own (keepGrowth), so that program's growth makes the guard go on. Test: the
//     subtest "control, the search reads pid 400 after pid 401" of
//     TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkTheSearchReadBeforeItsParentBeforeTheKill.
//   - (2) The search reads the fork before its parent and stops, and a later pass of the same
//     reading, the full read after the kill, reads the parent. Fork's reads: each of its reads
//     there that found its parent's program. The search's read found it by discountFork's own
//     test, and the parent has a read at that reading, though discountFork set no flag on it: that
//     later pass flags only its own read (settleForks). That holds whether the full read reads the
//     fork again or not, and whatever program it finds it running. Size: the lowest of its reads
//     before the kill of the program it runs now, as for any process. Where the full read finds it
//     still running its parent's program, that is the search's read, a fork's read (305). Where
//     the fork runs a program of its own between the search's read and the full read, its one
//     read before the kill is of another program, and the size is 0. Where the kill came before
//     the search, the guard never read the fork before the kill, and its size is its first read
//     after the kill, the search's, a fork's read (305), whatever it runs at the full read. A
//     fork's read counts from 0 once the fork runs a program of its own. Tests, each of which
//     asserts the size: the subtest "the budget stops the search before pid 400" of
//     TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkTheSearchReadBeforeItsParentBeforeTheKill;
//     for the fork that runs a program of its own before the full read,
//     TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkThatRunsItsOwnProgramBeforeTheFullRead;
//     for the fork the full read does not read again, which then stays a fork as an orphan,
//     TestMemoryGuardTakesTheSearchsReadOfAForkAsAForksReadWhereTheFullReadReadsItsParentAndNotTheFork;
//     and, with the kill before the search,
//     TestMemoryGuardCountsItsProgramsGrowthSinceTheKillForAForkTheSearchReadBeforeItsParentAfterTheKill
//     and, where the fork runs a program of its own before the full read,
//     TestMemoryGuardTakesTheSearchsForkReadAsTheSizeAsOfTheStaysFirstKillForAForkThatRunsItsOwnProgramBeforeTheFullRead.
//     The three of those where the full read makes no read of the fork that finds its parent's
//     program fail at 6a1d8e2f, where a read took a fork's flag only from discountFork or from a
//     read there that it flagged and that found the same program: no read of the fork was a fork's
//     read, so its size as of the kill was 305 MiB, no fork's, which counts whatever it runs, and
//     where it ran a program of its own its growth since the kill was hidden (the efficiency review
//     in the simplify pass of 6a1d8e2f). A parent the guard reads first at a later reading makes no
//     read of the fork a fork's read:
//     TestMemoryGuardTakesNoForksReadFromAParentFirstReadAtALaterReading.
//   - (3) The fork starts a program of its own between the reading before and the kill's reading.
//     Fork's reads: the reading before's alone. Size: the lowest of its reads at the kill's reading
//     before the kill, no fork's (150). The reading before's read is of another program, so it is
//     not the size, though it is lower (100). With no read at the kill's reading before the kill,
//     the size is 0. Tests:
//     TestMemoryGuardTakesTheNewProgramsReadAsTheSizeAsOfAKillWhereTheReadingBeforeReadAForkLower;
//     and, for the size 0, which it asserts at the stay's first kill,
//     TestMemoryGuardEndsAGrowerTheBandReadBeforeItsExecBeforeTheParentAfterAFall.
//   - (4) The fork starts a program of its own between two reads of the kill's reading, both
//     before the kill. Fork's reads: the first of the two, and the reading before's. Size:
//     the lowest of the reads of its own program before the kill, no fork's (150), whether the
//     fork's read at that reading is higher (260) or lower (100). Tests:
//     TestMemoryGuardTakesNoGrowthSinceAKillFromAForkThatRunsItsOwnProgramBetweenTwoReadsOfTheKillsReading
//     and TestMemoryGuardTakesTheNewProgramsReadAsTheSizeAsOfAKillWhereTheForksReadAtThatReadingIsLower.
//     Both fail at 6b8c4d9b. There forkAtReading gave each read of the kill's reading the flag of
//     any of them, so the size was the lowest of the fork's reads and its own program's, with a
//     fork's flag, which counts from 0; and the guard ended the fork too (thread
//     PRRT_kwDORFy7ds66ZPu6 of review round 6 of cybermonklabs/legion#4273).
//   - (5) An orphan: the fork's parent has ended. Fork's reads: each read that finds the program
//     of its last read that passed the fork test (forkProgram), its parent gone or not, until its
//     executable or arguments change (discountFork; the architect's answer of 2026-11-20T09:24Z).
//     Size: the lowest of those reads, a fork's read (600). It counts while the orphan runs that
//     program, and from 0 once that program changes. Test:
//     TestMemoryGuardKeepsAnOrphansForkReadAsItsSizeAsOfAKillUntilItsArgumentsChange.
//   - (6) An argument rewrite, as setproctitle makes, with the same executable. Fork's reads: for a
//     process that was no fork, none, and the rewrite is no new program (anotherProgram); for a
//     fork, its reads before the rewrite, and none after it. Size: for a process that was no fork,
//     no fork's read (600), which counts whatever its arguments become; for a fork, a fork's read
//     (600), which counts from 0 once it has rewritten them, the risk that the architect's decision
//     of 2026-11-20T08:13Z accepts. Tests: the subtest "pid 311 was no fork" of
//     TestMemoryGuardCountsFromNothingAForkThatRewritesItsArgumentsAfterAKillAndNotAProcessThatWasNoFork,
//     which asserts the size, and TestMemoryGuardTakesNoGrowthSinceAKillFromAProcessThatRewritesItsArguments,
//     which asserts the kills, for a process that was no fork; and the subtest "pid 311 was a
//     fork" of the first, for a fork.
//   - (7) A zombie: the process has exited, and its parent has not reaped it. Fork's reads: none.
//     The scan leaves a zombie out, and keepSizes drops the reads of a process that the table no
//     longer lists. Size: none. No size or flag as of the kill is kept for it, and no kill goes to
//     it. Test: the subtest "its stat reads a zombie" of
//     TestMemoryGuardTakesNoSizeOrFlagAsOfAKillFromAZombieOrFromAPidsEarlierProcess.
//   - (8) A pid that another process now holds, with another start time. Fork's reads: its own
//     alone. A read is kept by pid and start time, so no read of the earlier process is its read.
//     Size: as for any process, from its own reads. Where the guard never read it before the kill,
//     the size is its first read after the kill (150, no fork's). It takes neither the size of the
//     earlier process (600) nor that process's flag (a fork's). Test: the subtest "its pid names a
//     process started since" of the same test.
func (r *procReads) asOfKill(at time.Time) (asOf forkRead, afterKill bool) {
	newest := len(r.atReading) - 1
	if !r.readAt(at) {
		return r.read(newest), false
	}
	if !r.readBefore && r.beforeKill == 0 {
		return r.read(0), true
	}
	fork := r.read(newest).fork
	found := r.readBefore && !anotherProgram(r.before.fork, fork)
	if found {
		asOf = r.before
	}
	for i := range r.beforeKill {
		if read := r.read(i); !anotherProgram(read.fork, fork) && (!found || read.size < asOf.size) {
			asOf, found = read, true
		}
	}
	return asOf, false
}

// read is r's read i at its newest read's reading (atReading) with its own fork flag, by
// INFRA-3368's architect's rule of 2026-11-21T16:43Z on INFRA-3531 R1 of spec version 48, item 1
// (asOfKill's table): a fork's read where the program it found is its parent's program, as read
// anywhere at that reading or earlier in the stay. discountFork flags a read that finds so when the
// read is made. A read made before its parent's first read in the stay has no flag from it unless
// the same search reads the parent after it (startedSince): a later pass, as the full read after a
// kill, flags only its own read. So a read is a fork's read too where it found its parent's program
// by discountFork's own test and that parent has a read at that reading (readParent.readThere,
// which settleForks sets at the reading's last pass), or where a fork's read there found the same
// program, compared as forkProgram is, since that program is the parent's. The flag is never taken
// from a read of another program: where the process runs a program of its own between two reads of
// the reading, its read of that program is no fork's read, and its read of its parent's program
// keeps its flag. It allocates nothing.
func (r *procReads) read(i int) forkRead {
	read := r.atReading[i]
	for j := 0; !read.fork && j < len(r.atReading); j++ {
		read.fork = (r.atReading[j].fork || r.parents[j].readThere) && r.programs[j].is(r.programs[i])
	}
	return read
}

// firstReadSince is the size as of the stay's first kill of a process the guard never read before
// that kill, from r's reads at the reading of its first read after the kill (atReading;
// takeFirstSince, growthSinceStayKill): its first read there, as a fork's read, where that read is
// a fork's read by its own flag (read), its parent read at that reading before or after it, since
// a fork reports its parent's shared pages as its own; and 0 where it is not. By INFRA-3368's
// architect's decision of 2026-11-20T08:13Z on INFRA-3531 R1 of spec version 43, item 2, a process
// that is no fork there holds memory of its own, and the order's third tier is there so that a
// grower ranks before a process that never grew. It is for that tier alone: the go-on's sizes as of
// a kill (atKill) keep that first read, so below the urgent share a process that never grew still
// cannot make the guard go on.
//
// The size is the first read's, the one nearest the kill, so a step after it counts as growth
// since the kill. Its flag is that read's own and never a later read's of another program, by
// INFRA-3368's architect's rule of 2026-11-21T16:43Z, item 1, which asOfKill takes at a kill's
// reading too. A later read there can show that the first was a fork's read: a fork's read that
// found the same program, or a read of the parent whose program it found (settleForks). So the
// callers take it again while these reads are the process's (takeFirstSince).
//
// At 98207b42 it took the flag discountFork set on the first read. Where the search read a fork
// before its parent and stopped, at the stay's first kill or at a later mark, the fork counted its
// whole size as growth since that kill, and the guard ended it by rule=grew-since-kill or
// rule=urgent while its parent lived (thread PRRT_kwDORFy7ds66JVV0 of review round 4 of
// cybermonklabs/legion#4273;
// TestMemoryGuardCountsNoGrowthSinceTheStaysFirstKillForAForkTheSearchReadBeforeItsParent;
// INFRA-3531 R1 of spec version 48; INFRA-3368's architect, 2026-11-20T19:50Z).
func (r *procReads) firstReadSince() forkRead {
	// read(0) panics where atReading is empty: every caller keeps a read first
	// (growthSinceStayKill).
	if read := r.read(0); read.fork {
		return read
	}
	return forkRead{}
}

// grownSinceKill answers whether a process of cs, the processes the guard may end at a reading
// whose table is t, has grown since the previous kill of the rise: its size is its size as of the
// kill (atKill), 0 where that size came from a fork's read and it is no fork now (keepGrowth), plus
// growthNoise or more, a go-on of watch; the other, at a reading at or above the urgent share in a
// stay with history, is a process of the sets the urgent rule ends for the stay (urgentFirst),
// which need not have grown since the kill. It sets each one's
// sinceFirstKill to its growth since the stay's first kill too (sinceStayKill). Growth since a
// kill, not over the window that orders the processes, is what it measures, so a process that
// grows in steps wider than the window counts as grown. A process with no size as of a kill, one
// first read after it, has that first read kept as its size as of the kill, and has not grown at
// it: a fork reads its parent's shared pages as its own. The sizes of each process t no longer
// lists are dropped (dropEnded), so those kept are bounded by the processes the pod runs, and a
// process read before allocates nothing.
func (g *Guard) grownSinceKill(cs []candidate, t *Table) bool {
	g.sinceStayKill(cs, t)
	dropEnded(g.atKill, t)
	grew := false
	for i := range cs {
		grew = keepGrowth(g.atKill, &cs[i]) > 0 || grew
	}
	return grew
}

// sinceStayKill sets the sinceFirstKill of each of cs, the processes the guard may end at a reading
// whose table is t, to its growth since the first kill of this stay in the band, from its size as
// of that kill (atFirstKill), growthNoise or more, measured as keepGrowth measures it; and it drops
// the sizes of each process t no longer lists (dropEnded). That is the order's third tier (rank,
// INFRA-3531 R1 of spec versions 34 and 37). A process the guard never read before that kill
// counts from firstReadSince's size (growthSinceStayKill). It answers whether the stay has had its
// first kill: before it no process has any such growth, and it sets and keeps nothing. decide
// measures it at each band reading after that kill and at the reading at the mark of each rise
// after it, and watch at each reading after a kill (grownSinceKill), so a dip below the mark that
// stays in the band keeps it: at 15bdbd3c, in `8g-latepool-r2`, `8g-latepool-r3` and
// `8g-latefork2-r1`, one grower was left, the reading went about 2 MB below the mark and came back,
// and a guard that dropped these sizes below the mark saw no growth since the dip and ended the
// probe's 4.8 GB parent first, which freed about 6 MB, and the grower at the next reading.
func (g *Guard) sinceStayKill(cs []candidate, t *Table) bool {
	if !g.firstKillKept {
		return false
	}
	dropEnded(g.atFirstKill, t)
	for i := range cs {
		cs[i].sinceFirstKill = g.growthSinceStayKill(&cs[i])
	}
	return true
}

// growthSinceStayKill is how much c, a process read at a reading after the first kill of this stay
// in the band, grew since that kill, the order's third tier (sinceStayKill, endKnown, readOn),
// measured from its size as of that kill (atFirstKill) as keepGrowth measures it. Where atFirstKill
// holds no size of c, which the guard first read after that kill, or holds one still marked
// (procReads.firstTaken), it takes firstReadSince's from c's reads at its newest read's reading
// (takeFirstSince). Every caller keeps c's read first (keepSize): endKnown and readOn read c before
// they measure it, and sinceStayKill's processes are an assessment's (assess), the held ones the
// band's assessment kept, and the ones the search found (startedSince), each read before. So c has
// reads kept, and a missing one is a broken invariant: firstReadSince then has no read to take,
// and its panic turns the guard off by its off line with reason=panic, rather than measure c from
// a size no read of c's reading gave (thread PRRT_kwDORFy7ds66UWB6 of review round 5 of
// cybermonklabs/legion#4273).
func (g *Guard) growthSinceStayKill(c *candidate) uint64 {
	key := procKey{pid: c.PID, start: c.StartTime}
	r := g.sizes[key]
	if _, known := g.atFirstKill[key]; !known || r.firstTaken {
		g.takeFirstSince(key, r)
	}
	return keepGrowth(g.atFirstKill, c)
}

// takeFirstSince keeps in atFirstKill firstReadSince's size from r's reads at the reading of its
// newest read (procReads.atReading), the size as of the stay's first kill of the process key names,
// which the guard never read before that kill, and marks it (procReads.firstTaken). A marked size
// is taken again while r's reads are those of the reading it was taken at, since a later read there
// that is a fork's read makes the first one a fork's read too where it found the same program
// (procReads.read), and so does a later read there of the parent whose program it found
// (settleForks), as where the search at the mark read a fork before its parent:
// growthSinceStayKill takes it again at each measure, and keepKillSizes at each kill, after the
// last read of that kill's reading. The next reading that reads the process clears the mark
// (keepSize), and the size then stays as taken. A size from a read before the stay's first kill
// (asOfKill) is never marked, and never taken again.
func (g *Guard) takeFirstSince(key procKey, r procReads) {
	g.atFirstKill[key] = r.firstReadSince()
	if !r.firstTaken {
		r.firstTaken = true
		g.sizes[key] = r
	}
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
func dropEnded(kept map[procKey]forkRead, t *Table) {
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

// keepGrowth is how much c, a process read at a reading, grew since the kill whose sizes kept
// holds: its size less its size as of the kill, where that is growthNoise or more, and 0
// otherwise. A size as of a kill from a fork's read counts only while c is still a fork (c.fork),
// which it is, its parent gone or not, until its executable or arguments differ from those of its
// fork's read (discountFork): at a reading where it is no longer one, as where it exec'd after the
// kill's reading, its size as of the kill is 0, and all its program holds is growth since the
// kill, from growthNoise on as any growth is. A size from a read that was no fork's counts whatever
// c's executable or arguments become: an argument rewrite, as setproctitle or process.title makes,
// is not an exec (anotherProgram). By INFRA-3368's architect's decision of 2026-11-20T08:13Z on
// INFRA-3531 R1 of spec version 43, item 1, that fork test is what "the program it runs now" of its
// ruling of 2026-11-20 (dispatch://INFRA-3531/comment/a81dfa03-66d1-4f14-a0c8-d5ad7af91df9)
// compares, at the reading that measures growth. A process kept holds no size of, one the guard
// first reads after the kill, has that read kept as its size as of the kill and has not grown at
// it. growthSinceStayKill keeps such a process's size as of the stay's first kill (atFirstKill) by
// firstReadSince before it measures growth here.
func keepGrowth(kept map[procKey]forkRead, c *candidate) uint64 {
	key := procKey{pid: c.PID, start: c.StartTime}
	asOf, known := kept[key]
	if !known {
		kept[key] = forkRead{size: c.size, fork: c.fork}
		return 0
	}
	if anotherProgram(asOf.fork, c.fork) {
		asOf.size = 0
	}
	if c.size >= asOf.size+growthNoise {
		return c.size - asOf.size
	}
	return 0
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
func rank(cs []candidate) { slices.SortFunc(cs, orderOfEnds) }

// orderOfEnds compares x and y in the order of ends (rank): negative where x ends before y. The
// first of a set is so the one rank puts first (endHeld).
func orderOfEnds(x, y candidate) int {
	// Growth since the stay's first kill orders those that are not in the new tier; the ones in it
	// go by size alone.
	sinceFirstKill := 0
	if !x.inNewTier() && !y.inNewTier() {
		sinceFirstKill = cmp.Compare(y.sinceFirstKill, x.sinceFirstKill)
	}
	return cmp.Or(cmp.Compare(y.growth, x.growth), cmp.Compare(ordinal(y.inNewTier()), ordinal(x.inNewTier())),
		sinceFirstKill, cmp.Compare(y.rankSize(), x.rankSize()), cmp.Compare(y.StartTime, x.StartTime), cmp.Compare(y.PID, x.PID))
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
// processes the guard may end, at their sizes, the killed processes that have not exited among them
// (leaveOut), which no decision counts but which are of their kind and hold their memory still; in
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

// endWait lets go of the processes a waiting guard holds, of the sizes as of their kill (atKill)
// and of the processes it read at the wait's last reading (lastRead): the wait ends at the next
// kill of the rise, which keeps its own, and when the rise ends. The kills keep their storage for
// the next.
func (g *Guard) endWait() {
	if g.phase != waiting {
		return
	}
	for _, k := range g.wait.kills {
		_ = k.handle.Close()
	}
	g.wait = afterKill{kills: g.wait.kills[:0]}
	clear(g.atKill)
	g.lastRead, g.lastTable = nil, nil
	g.phase = armed
}

// rollWindow credits a finished scan for the largest process to the window it was started in
// (takeScan), and closes the window whose hour has passed, with its line, for the one the reading
// taken at now falls in. It is the one part of the hour's work that runs before the guard decides
// on the reading (tick), so that a kill's counter read counts for the window of its own hour. It is
// false where the scan panicked, which has turned the guard off for good (takeScan).
func (g *Guard) rollWindow(now time.Time) bool {
	if !g.takeScan() {
		return false
	}
	if !now.Before(g.window.end) {
		g.closeWindow()
		start, end := nextWindow(g.window.end, now)
		g.window = window{start: start, end: end}
	}
	return true
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
	if reading == w.readingHigh && (!w.scanned || reading >= w.scannedHigh+limit/100) {
		if g.startScan(w.start) {
			w.scanned, w.scannedHigh = true, reading
		}
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
// It calls counterRead first, where a test has set it.
func (g *Guard) readCounter(now time.Time) uint64 {
	if g.counterRead != nil {
		g.counterRead()
	}
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

// takeScan credits a finished scan to the window it was started in, and is false where the scan
// panicked: it turns the guard off for good here, on the loop, with the scan's panic as its reason.
func (g *Guard) takeScan() bool {
	g.scanMu.Lock()
	done := g.scanDone
	g.scanDone = nil
	g.scanMu.Unlock()
	if done == nil {
		return true
	}
	if done.panicked != "" {
		g.diePanicked(done.panicked)
		return false
	}
	if done.window.Equal(g.window.start) {
		g.window.largest = done.largest
	}
	return true
}

// scanLargest finds the largest process of every process the pod shows, protected ones among
// them: the hourly line says what held the memory, whatever the guard may end. It ranks them by
// the sizes and in the order an assessment ranks the processes the guard may end (addHeldMemfds),
// every process whose smaps it read being a holder and a mapper of memfds here, and shmem the
// Shmem of the reading that started it (startScan). A panic here would end the launcher as one on
// the loop would (step), so the scan finishes with it as its off reason (largestScan.panicked),
// and the loop turns the guard off at its next reading, since the loop's state is the loop's own.
// A scan that finishes never replaces a finished scan that panicked: the loop can start the next
// scan in the same reading as the panicked one finishes after its takeScan, and that scan can
// finish before the next takeScan. However the scan ends, a panic included, it lets go of its
// smaps buffer once that has grown past maxHeldFile (keepSmaps), before the next scan can start
// (scanBusy) and take the buffer over.
func (g *Guard) scanLargest(start time.Time, shmem uint64) {
	defer g.scans.Done()
	found := largestScan{window: start}
	defer func() {
		if p := recover(); p != nil {
			found = largestScan{window: start, panicked: panicReason("panic in the scan for the largest process", p)}
		}
		g.scanSmaps = keepSmaps(g.scanSmaps)
		g.scanMu.Lock()
		if g.scanDone == nil || g.scanDone.panicked == "" {
			g.scanDone = &found
		}
		g.scanBusy = false
		g.scanMu.Unlock()
	}()
	t, err := Scan(g.proc)
	if err != nil {
		return
	}
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
	addHeldMemfds(read, g.proc, g.memfdLink, g.scanMemfds, shmem)
	if len(read) > 0 {
		biggest := read[0]
		found.pid, found.role, found.size = biggest.PID, biggest.role, biggest.size
		found.executable = processName(biggest.Process)
	}
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

// die turns the guard off for good, with one line that says why, and lets go of every process it
// holds, the kills of the wait (endWait).
func (g *Guard) die(reason string) {
	g.logf(offLine, reason)
	g.mu.Lock()
	g.dead = true
	g.mu.Unlock()
	g.endWait()
}

// diePanicked is die after a panic on the guard's loop or in its scan (step, takeScan), with the
// panic as its reason. die ends the wait after a kill, whose state may be what panicked: where die
// panics again, the guard is marked off for good all the same and the wait is dropped with its
// handles open, so neither this end nor release meets them again, and a few descriptors stay open
// for the pod's life rather than end it.
func (g *Guard) diePanicked(reason string) {
	defer func() {
		if recover() != nil {
			g.mu.Lock()
			g.dead = true
			g.mu.Unlock()
			g.wait, g.phase = afterKill{}, armed
		}
	}()
	g.die(reason)
}

// panicReason is the reason a recovered panic p gives, as the off line or the error that ends what
// panicked: what, which names the panic, then p and where it was raised (panicSite). The deferred
// function that recovered p calls it.
func panicReason(what string, p any) string {
	return fmt.Sprintf("%s: %v at %s", what, p, panicSite())
}

// panicSite is where the panic being recovered was raised, as the function and its file and line:
// the first frame past runtime.gopanic, the runtime's panic, that is not the runtime's own, which
// passes over the runtime's frames where the runtime raised the panic, as for a nil dereference.
// It walks the stack down to runtime.gopanic rather than skipping a fixed number of frames, so the
// function that panicked is found however many calls stand between the recover and panicSite, a
// helper among them (panicReason; INFRA-3531 R1, thread PRRT_kwDORFy7ds66JVWR of review round 4 of
// cybermonklabs/legion#4273).
func panicSite() string {
	var pcs [32]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	pastPanic := false
	for {
		f, more := frames.Next()
		if pastPanic && !strings.HasPrefix(f.Function, "runtime.") {
			return fmt.Sprintf("%s (%s:%d)", f.Function, filepath.Base(f.File), f.Line)
		}
		pastPanic = pastPanic || f.Function == "runtime.gopanic"
		if !more {
			return "an unknown place"
		}
	}
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
