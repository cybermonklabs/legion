package memoryguard

import (
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/termsafe"
)

// The guard's log lines. Each starts with linePrefix and carries `key=value` fields: sizes in
// bytes, times in RFC 3339 UTC, and a process's name (processName) last and quoted, so a reader
// takes everything from `executable="` to the line's last quote whatever the name holds. No line
// prints a process's arguments. A process the guard cannot read has its unreadLine once, settings
// the launcher refuses their refusedLine, and a process the kill failed on its killFailedLine
// once, so a kill that fails is never retried unseen at every reading. A stop that repeats has its
// stoppedLine at most once in each stoppedEvery, and the line written counts in left_out the
// stopped lines left out since the one before it (stopRise).
//
// A process the guard cannot read is one whose stat, exe, ns/pid or smaps did not read (a scan's
// unread; a missing exe or ns/pid of a process that has not ended among them), or one the kill
// failed on: the guard never ends it, since it cannot tell what it is or rank it. The
// stoppedLine's last field, unread_processes, names each of them in pid order, comma-separated:
// `<pid>:<bytes>` for one whose stat reads a state other than Z and whose smaps reads, and `<pid>`
// for every other, whose smaps does not read, whose stat did not read, or whose stat reads Z (a
// zombie leader's smaps reads 0 under gVisor while a live thread holds the process's memory, and
// `<pid>:0` would read as a size); `none` where there is none (INFRA-3531 R3 of spec versions 29
// and 30). Its unread is the sum of the sizes that field names. The field comes after every field
// the line carried before it, so a reader of those fields reads them as it did.
//
// The sizes the guard reads at each reading in the band below the mark and after a kill write no
// line: they decide the order of the next kill (rank), whose endedLine names the process it ended,
// with the size it read of it at the reading it decided on: for a first kill of a rise that the
// band's held reads decided at the reading at the mark (endHeld), the band's last reading, unless
// the reading at the mark read that process's smaps (rereadHeld, startedSince, readOn), and for
// every other kill the reading it is sent at. The line's reading is always the one the kill is sent
// at. Its rule and sizes, which come before the executable so that the name stays last, say why
// that process was first (INFRA-3531 R3 of spec version 31): rule is the tier of the order of ends
// that put it first, or the urgent rule for a process ended beside the first at a reading at or
// above the urgent share, one that grew over the window or, where the decision holds a process that
// is not new to the stay, one of the sets the urgent rule ends for the stay, grown since the stay's
// first kill, started during the stay and no fork with memory of its own, or new to it with memory
// of its own, which at the reading at the mark the read-on after the first kill can read
// (ruleUrgent, spec versions 39, 41, 42, 43 and 46), or, at such a reading after a kill, one the
// guard read at the reading before that grew since that kill, which the reads again before the full
// read end (endKnown, spec version 43), or a first kill of a reading of the wait where those sets
// are the one reason to act and the order puts another process first (urgentFirst, spec version
// 46), and sizes where the sizes the guard chose it by came from: the decision's for each kill of
// the reading. Its less_parent, after sizes, is how much of its size its rank did not count
// (INFRA-3531 R1 of spec version 33): for a fork that runs its parent's executable with its
// parent's arguments, its parent being one the guard has read in this stay, its parent's size at
// the guard's latest read of the parent, or its own size where that is less, so that size less
// less_parent is the size it ranked by where a tier ranks by size (candidate.lessParent); 0 for
// every other process and for a process the guest kernel killed. The line's size is always the
// process's own.
//
// Its new_read, after less_parent, says what the reads at the reading at the mark read of the
// processes new to the stay, those the band read once at its last reading and those started
// since it (INFRA-3531 R1 of spec versions 31, 32 and 34), for a first kill decided where no held
// process had grown by the band's reads, or at a reading at or above the urgent share, where the
// mark reads the band's growers again whatever the held reads show (endHeld, spec version 39),
// whatever its sizes: `<reached>:<own>`, how many of those processes they read the smaps of,
// and of them how many rank above nothing, holding memory of their own after the fork discount,
// their identity read again (newRead, reidentify); each process ended beside the first at an
// urgent reading carries the first's. A first kill sent before the search at an urgent reading,
// where a held process grew (endHeld, spec version 43), carries what every read at the mark read,
// the search's among them, since its line comes after every kill of the reading and the decision
// is one. At an urgent reading in a stay with history every kill of the reading at the mark
// carries the read-on's reads too, the reads after the first kill through the processes new to the
// stay that the reads before it did not reach, up to one interval from the reading (readOn, spec
// version 43): in the replay of `4g-t4latepool-r3` each of its six kills says `new_read=6:6`. So
// `new_read=3:0` reads that the mark read three processes new to the stay, each a fork whose
// parent's size covers its own, and the tiers below the new decided, and `new_read=0:0` that it
// read none. It is `-` where the kill was decided on no read at the mark: a first kill on the
// band's held reads alone, one of them having grown by them, below the urgent share, every kill on
// a reading's own assessment (act), every kill after a kill of the rise among them, and a process
// the guest kernel killed.
const (
	linePrefix     = "launcher: memory guard: "
	startedLine    = linePrefix + "started act_percent=%d floor_percent=%d wait=%s interval=%s limit=%d reading=%d"
	settingsLine   = linePrefix + "settings act_percent=%d floor_percent=%d wait=%s interval=%s"
	noSettingsLine = linePrefix + "off: the daemon's acknowledgement carries no settings"
	refusedLine    = linePrefix + "settings refused: %v; nothing changes"
	endedLine      = linePrefix + `ended pid=%d role=%s size=%d reading=%d counter=%d shmem=%d limit=%d rule=%s sizes=%s less_parent=%d new_read=%s executable="%s"`
	stoppedLine    = linePrefix + "stopped for this rise reason=%s reading=%d endable=%d protected=%d unread=%d unheld=%d limit=%d left_out=%d unread_processes=%s"
	hourLine       = linePrefix + "hour start=%s end=%s reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d" +
		` largest_pid=%d largest_role=%s largest_size=%d largest_executable="%s"`
	offLine        = linePrefix + "off reason=%s"
	unreadLine     = linePrefix + "unread pid=%d reason=%v"
	killFailedLine = linePrefix + "kill failed pid=%d reason=%v"
)

// The two reasons a rise is given up on. A rise is given up with reasonNothingToEnd where, at the
// reading the guard would end a process at, the processes it may end hold less than the floor
// between them, or there are none. It is given up with reasonNotFallen where the wait after a
// kill has passed with the reading not below the reading at the kill, and either no process the
// guard may end has grown since that kill (grownSinceKill) and, at a reading at or above the urgent
// share in a stay with history, none is of the sets the urgent rule ends for the stay, grown since
// the stay's first kill, started during the stay and no fork with memory of its own, or new to it
// with memory of its own (urgentFirst), or they hold less than the floor: the guard goes on after a
// kill that lowered nothing only while one of them has grown since it (INFRA-3531 R1 of spec
// version 25) or, at such a reading, one is of those sets, which the urgent rule ends (spec
// versions 43 and 46, verification version 36). So below the urgent share a rise that is a
// protected process's own, which no kill of the guard's lowers, ends at most one process before
// this reason; at or above it, in a stay with history, it can also end each process of those sets,
// at the reading of that kill or at a later reading of the wait at or above that share.
const (
	reasonNotFallen    = "not fallen"
	reasonNothingToEnd = "nothing to end"
)

// The ended line's rule, the tier of the order of ends (rank) that put the ended process first:
// ruleGrew, it grew over growthWindow by growthNoise or more, the most of those that did; ruleNew,
// none grew so, and it is the largest of the processes new to this stay in the band, read at most
// once at the readings of the stay before the one decided on (candidate.newToStay), that hold
// memory of their own: a fork whose parent's size covers its own ranks at nothing and is in the
// tiers below, and its line names the one that put it first (candidate.inNewTier, INFRA-3531 R1 of
// spec version 34); ruleGrewSinceKill, none grew or is new, and it grew the most since the first
// kill of this stay in the band, a stay that a dip below the mark in the band does not end (spec
// versions 34 and 37; candidate.sinceFirstKill); ruleLargest, none of these applies and it is the
// largest. ruleUrgent is a process ended beside the first at a reading at or above the urgent
// share, the mark plus half the distance from the mark to the limit, where the guard ends every
// process the urgent rule ends (urgentEnds): each that grew over the window and, where the decision
// holds a process that is not new to the stay, each of the sets of the stay (endsInStay), each with
// its line, the first naming its own tier and each other this rule (endUrgent, INFRA-3531 R1 of
// spec versions 39, 41, 42 and 46, the last by INFRA-3368's architect's decisions of
// 2026-11-20T13:55Z, 14:00Z, 14:05Z and 14:58Z; `4g-latepool-4tpqx`, and `4g-r15latepool-r206`),
// and, at the reading at the mark, those the read-on after the first kill reads up to one interval
// from the reading (readOn, spec version 43; `4g-t4latepool-r3`). At such a reading after a kill
// the guard also ends by this rule, as it reads them again before the full read, the processes it
// read at the reading before that grew since that kill, and after the first of those kills each
// process it reads that the urgent rule ends (urgentEnds), but the first of its kills, whose line
// names its own tier (endKnown, spec versions 43 and 46; the tester's run r17 at 41d4f45b). And
// where, at such a reading, those sets are the one reason to act, the first kill goes to the first
// process in the order that the urgent rule ends at that reading, one that grew over the window or
// one of those sets, and not to a process the rule does not end, as a pool's parent flat since
// before the stay's first kill; its line names this rule only where the order put such a process
// before it, and its own tier otherwise (urgentFirst, spec version 46). Where every process is new
// to the stay, it ends the window's growers alone beside the first.
// ruleKernel is a process the guest kernel killed (Guest), which the kernel chose by rules of its
// own.
const (
	ruleGrew          = "grew"
	ruleNew           = "new"
	ruleGrewSinceKill = "grew-since-kill"
	ruleLargest       = "largest"
	ruleUrgent        = "urgent"
	ruleKernel        = "kernel"
)

// The ended line's sizes, where the sizes the ended process was chosen by came from: sizesBand, the
// band's held reads alone, at the first reading at or above the mark (endHeld); sizesReread, those
// and the smaps of held processes read again at that reading (rereadHeld); sizesSearch, those and
// the smaps of processes started since the band's last reading that the reading at the mark found
// (startedSince); sizesReading, the assessment of the reading the kill is sent at (act), as every
// kill after a kill of the rise and every first kill the band's reads cannot decide has; at an
// urgent reading of the wait the smaps the guard reads again before the full read are among them,
// since that reading's assessment takes them (endKnown).
// sizesKernel is a process the guest kernel killed, by sizes of its own.
const (
	sizesBand    = "band"
	sizesReread  = "reread"
	sizesSearch  = "search"
	sizesReading = "reading"
	sizesKernel  = "kernel"
)

// The guest watch's lines (Guest). Each starts with guestPrefix and carries `key=value` fields as
// the guard's do, each figure the watch cannot read written `-`; a process the guest kernel killed
// has the guard's own endedLine, so a reader finds a process stopped for memory by one line,
// whichever stopped it. The start line names the cgroup's version, its directory, v2's
// memory.oom.group and the starting score; a watch with no readable kill counter has its
// guestKillsUnnamedLine once, and one with no readable starting score or /proc its guestOffLine
// once. Such a watch goes on scanning, so a step of it that panics later writes a second one, with
// the panic; a watch whose step panicked (Guest's step) writes nothing more. A watch that
// reads the kill counter and does not wake on the file-modified event of memory.events (Guest's
// wake) has its guestWakeOffLine once, with the reason: its cgroup is v1, whose memory.oom_control
// raises no such event, or the watch of the event could not start, or it ended, with the error
// that ended it; the watch then reads the counter at its scans alone. The kills of a burst of the
// counter's rises that no process was named for by the wait's end, since none showed the kernel's
// kill, or more showed it at one pass than kills were pending, or each that showed it was of a
// role whose session had ended (Guest's sessionEnded), and every kill of a rise where the watch
// reads no /proc, have one guestKilledUnnamedLine, which names no process: the count of those
// kills, the counter's total, and the pod's memory at the burst's first rise, the fields an
// endedLine gives it (reading, counter, shmem and limit). A process whose score write failed has
// its guestScoreFailedLine once, with the score the write would have set.
const (
	guestPrefix            = "launcher: guest memory: "
	guestStartLine         = guestPrefix + "start cgroup=%s dir=%s oom_group=%s score=%s"
	guestOffLine           = guestPrefix + "off reason=%s"
	guestKillsUnnamedLine  = guestPrefix + "kills unnamed reason=%v"
	guestWakeOffLine       = guestPrefix + "wake off reason=%s"
	guestKilledUnnamedLine = guestPrefix + "killed unnamed count=%d oom_kill=%d reading=%d counter=%d shmem=%d limit=%d"
	guestScoreFailedLine   = guestPrefix + "score failed pid=%d start_time=%d score=%d reason=%v"
	guestHourLine          = guestPrefix + "hour start=%s end=%s current=%s current_high=%s current_high_at=%s" +
		" working_set=%s working_set_high=%s working_set_high_at=%s anon=%s file=%s inactive_file=%s shmem=%s limit=%s" +
		" oom_kills=%s unnamed=%s raised=%s unraised=%s"
)

// roleName is a class's role as a line names it: `none` outside every role.
func roleName(role string) string {
	if role == "" {
		return "none"
	}
	return role
}

// stamp is t as a line writes it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// processName is how every line and every report names p: the base name of its executable, the
// `exe` link the scan read, or its stat name (comm) where the scan could not read that link,
// escaped and cut to shimwire.MemoryKillExecutableBytes (termsafe.Escaped). It holds none of p's arguments,
// and the guard masks nothing: an argument is text the process chose, and it may hold a credential
// no masking rule names, such as a positional password, while the role that started the process
// already has its arguments (dispatch://INFRA-3557, W4).
func processName(p *Process) string {
	name := p.Name
	if p.Exe != "" {
		name = filepath.Base(p.Exe)
	}
	return termsafe.Escaped(name, shimwire.MemoryKillExecutableBytes)
}
