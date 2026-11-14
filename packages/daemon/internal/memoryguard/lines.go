package memoryguard

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
)

// The guard's log lines. Each starts with linePrefix and carries `key=value` fields: sizes in
// bytes, times in RFC 3339 UTC, and a command last and quoted, so a reader takes everything from
// `command="` to the line's last quote whatever the command holds. A process the guard cannot read
// has its unreadLine once, settings the launcher refuses their refusedLine, and a process the kill
// failed on its killFailedLine once, so a kill that fails is never retried unseen at every
// reading. A stop that repeats has its stoppedLine at most once in each stoppedEvery, and the line
// written counts in left_out the stopped lines left out since the one before it (stopRise).
const (
	linePrefix     = "launcher: memory guard: "
	startedLine    = linePrefix + "started act_percent=%d floor_percent=%d wait=%s interval=%s limit=%d reading=%d"
	settingsLine   = linePrefix + "settings act_percent=%d floor_percent=%d wait=%s interval=%s"
	noSettingsLine = linePrefix + "off: the daemon's acknowledgement carries no settings"
	refusedLine    = linePrefix + "settings refused: %v; nothing changes"
	endedLine      = linePrefix + `ended pid=%d role=%s size=%d reading=%d counter=%d shmem=%d limit=%d command="%s"`
	stoppedLine    = linePrefix + "stopped for this rise reason=%s reading=%d endable=%d protected=%d unread=%d unheld=%d limit=%d left_out=%d"
	hourLine       = linePrefix + "hour start=%s end=%s reading_high=%d reading_high_at=%s reading_low=%d counter_high=%d counter_low=%d" +
		` largest_pid=%d largest_role=%s largest_size=%d largest_command="%s"`
	offLine        = linePrefix + "off reason=%s"
	unreadLine     = linePrefix + "unread pid=%d reason=%v"
	killFailedLine = linePrefix + "kill failed pid=%d reason=%v"
)

// The two reasons a rise is given up on.
const (
	reasonNotFallen    = "not fallen"
	reasonNothingToEnd = "nothing to end"
)

// logCommandBytes is the most bytes of a command a log line carries: twice the report's, since
// the log is where a reader goes for more, and still a bound, since an agent's own arguments can
// hold its whole system prompt.
const logCommandBytes = 512

// rawCommandBytes is the most bytes of a process's arguments the guard masks (commandLine): eight
// times logCommandBytes, so what is left fills a log line even where masking shortens it, and a
// bound, so the work after a kill, which the guard's loop does before its next reading, does not
// grow with the killed process's arguments. The scan reads one byte more of `cmdline`
// (readProcess), so it can tell arguments that ran past it.
const rawCommandBytes = 8 * logCommandBytes

// ellipsis marks where a command was cut.
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

// commandText is p's command line (commandLine, at rawCommandBytes) with each rune that does not
// print escaped, a `\u202e` that would reorder what follows it among them. Each reader cuts it to
// its own bound (cut) after the masking, so that cut never leaves part of a credential that the
// line would have masked.
func commandText(p *Process) string {
	var b strings.Builder
	escape(&b, commandLine(p, rawCommandBytes))
	return b.String()
}

// commandLine is p's arguments as one line with no credential in it: joined by spaces and made
// one line by the rules a capability report's detail is cleaned by (capabilities.RedactedLine:
// each run of whitespace one space, each control rune, control sequence or byte that is not UTF-8
// read away, every credential masked), since an argument can carry a token. A process that shows
// no arguments is named by its stat name in brackets, as ps names it.
//
// Arguments that run past bound bytes (argsCut, or a joined text longer than bound) are cut to
// their first bound bytes, on a character boundary, before they are masked, so the masking costs
// no more for a linker's arguments or a long `python3 -c` script than for a short command. The
// cut text then loses its last word twice, before the masking and after it, and the line ends
// with an ellipsis, so that the cut shows nothing the whole line would have masked:
//   - Before: the rules read a control rune as held, not as a space, and a credential runs across
//     it, while the line shows it as a space, so one run of the text can be several words of the
//     line. Every rule that needs something after its value to match (userinfo's `@`, a token's
//     least length, a JWT's second dot, `AKIA`'s sixteen characters and `\b`) matches within one
//     run with no whitespace in it, and the cut parts only the text's last run, which goes.
//   - After: a rule whose value may hold a space (a quoted `password=`, an `Authorization` value,
//     a JSON `api_key`) still matches a value the cut parts, since what closes it is optional, and
//     masks it to the end of the line, where the whole line keeps the quote or bracket after its
//     `***` and may read a scheme (`Bearer`) apart from the value: that is the line's last word.
func commandLine(p *Process, bound int) string {
	raw := "[" + p.Name + "]"
	if len(p.Args) > 0 {
		raw = strings.Join(p.Args, " ")
	}
	if !p.argsCut && len(raw) <= bound {
		return capabilities.RedactedLine(raw)
	}
	if len(raw) > bound {
		end := bound
		for end > 0 && !utf8.RuneStart(raw[end]) {
			end--
		}
		raw = raw[:end]
	}
	raw = raw[:max(strings.LastIndexFunc(raw, unicode.IsSpace), 0)]
	line := capabilities.RedactedLine(raw)
	return line[:max(strings.LastIndexByte(line, ' '), 0)] + ellipsis
}

// escape writes s to b with each control character, and each other character that does not
// print, as a Go escape (`\n`, `\x1b`, `\u202e`), and each byte that is not UTF-8 as `\xNN`, so
// a command can neither split a log line nor colour or reorder what follows it.
func escape(b *strings.Builder, s string) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < utf8.RuneSelf && !unicode.IsPrint(r):
			fmt.Fprintf(b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r <= 0xFFFF:
			fmt.Fprintf(b, `\u%04x`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(b, `\U%08x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
}

// cut is text at most max bytes long, cut on a character boundary with an ellipsis where it was
// cut; the ellipsis counts in max.
func cut(text string, max int) string {
	if len(text) <= max {
		return text
	}
	end := max - len(ellipsis)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + ellipsis
}
