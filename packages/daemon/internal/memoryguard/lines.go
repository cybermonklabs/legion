package memoryguard

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
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
// the reading at the mark read that process's smaps (rereadHeld, startedSince), and for every
// other kill the reading it is sent at. The line's reading is always the one the kill is sent at.
// Its rule and sizes, which come before the executable so that the name stays last, say why that
// process was first (INFRA-3531 R3 of spec version 31): rule is the tier of the order of ends that
// put it first, or the urgent rule for a process ended beside the first at a reading at or above
// the urgent share (ruleUrgent, spec version 39), and sizes where the sizes the guard chose it by
// came from. Its less_parent, after sizes, is how much of its size its rank did not count
// (INFRA-3531 R1 of spec version 33): for a fork that runs its parent's executable with its
// parent's arguments, its parent being one the guard has read in this stay, its parent's size at
// the guard's latest read of the parent, or its own size where that is less, so that size less
// less_parent is the size it ranked by where a tier ranks by size (candidate.lessParent); 0 for
// every other process and for a process the guest kernel killed. The line's size is always the
// process's own.
//
// Its new_read, after less_parent, says what the reads at the reading at the mark read of the
// processes new to the stay, those the band read once at its last reading and those started since
// it (INFRA-3531 R1 of spec versions 31, 32 and 34), for a first kill decided where no held process
// had grown by the band's reads, or at a reading at or above the urgent share, where the mark reads
// the band's growers again whatever the held reads show (endHeld, spec version 39), whatever its
// sizes: `<reached>:<own>`, how many of those processes they read the smaps of, and of them how
// many rank above nothing, holding memory of their own after the fork discount, their identity read
// again (newRead, reidentify); each process ended beside the first at an urgent reading carries the
// first's. So `new_read=3:0` reads that the mark read three processes new to the stay, each a fork
// whose parent's size covers its own, and the tiers below the new decided, and `new_read=0:0` that
// it read none. It is `-` where the kill was decided on no read at the mark: a first kill on the
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
// guard may end has grown since that kill (grownSinceKill) or they hold less than the floor: the
// guard goes on after a kill that lowered nothing only while one of them has grown since it
// (INFRA-3531 R1 of spec version 25), so a rise that is a protected process's own, which no kill
// of the guard's lowers, ends at most one process before this reason.
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
// process that grew over the window, each with its line, the first naming its own tier and each
// other this rule (endAt, INFRA-3531 R1 of spec version 39). ruleKernel is a process the guest
// kernel killed (Guest), which the kernel chose by rules of its own.
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
// kill after a kill of the rise and every first kill the band's reads cannot decide has.
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
// guestKillsUnnamedLine once, and one with no readable /proc its guestOffLine once. A watch that
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

// ellipsis marks where escaped cut its text.
const ellipsis = "…"

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
// escaped and cut to shimwire.MemoryKillExecutableBytes (escaped). It holds none of p's arguments,
// and the guard masks nothing: an argument is text the process chose, and it may hold a credential
// no masking rule names, such as a positional password, while the role that started the process
// already has its arguments (dispatch://INFRA-3557, W4).
func processName(p *Process) string {
	name := p.Name
	if p.Exe != "" {
		name = filepath.Base(p.Exe)
	}
	return escaped(name, shimwire.MemoryKillExecutableBytes)
}

// escaped is s with each control character, and each other character that does not print, as a
// Go escape (`\n`, `\x1b`, `\u202e`), and each byte that is not UTF-8 as `\xNN`, so the text can
// neither split a log line nor colour or reorder what follows it. It is at most max bytes long:
// text that would run past max is cut after the last character, or the last escape, that leaves
// room for an ellipsis, which marks the cut and counts in max, so no escape is cut in two.
func escaped(s string, max int) string {
	var b strings.Builder
	fits := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < utf8.RuneSelf && !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r <= 0xFFFF:
			fmt.Fprintf(&b, `\u%04x`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
		if b.Len() > max {
			return b.String()[:fits] + ellipsis
		}
		if b.Len() <= max-len(ellipsis) {
			fits = b.Len()
		}
	}
	return b.String()
}
