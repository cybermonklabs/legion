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
// `command="` to the line's last quote whatever the command holds. The plan of INFRA-3557 names
// each line and its fields; unreadLine and refusedLine are two it asks for without naming a shape
// (a process the guard cannot read is "logged once", and a launcher checks the daemon's settings),
// and killFailedLine is this package's own: a kill that fails is never retried unseen at every
// reading.
const (
	linePrefix     = "launcher: memory guard: "
	startedLine    = linePrefix + "started act_percent=%d floor_percent=%d wait=%s interval=%s limit=%d reading=%d"
	settingsLine   = linePrefix + "settings act_percent=%d floor_percent=%d wait=%s interval=%s"
	noSettingsLine = linePrefix + "off: the daemon's acknowledgement carries no settings"
	refusedLine    = linePrefix + "settings refused: %v; nothing changes"
	endedLine      = linePrefix + `ended pid=%d role=%s size=%d reading=%d counter=%d shmem=%d limit=%d command="%s"`
	stoppedLine    = linePrefix + "stopped for this rise reason=%s reading=%d endable=%d protected=%d unread=%d unheld=%d limit=%d"
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

// commandText is p's arguments as one printable line with no credential in it: joined by spaces,
// every character that does not print escaped, and passed through the credential rules a
// capability report's detail is cleaned by, since an argument can carry a token. A process that
// shows no arguments is named by its stat name in brackets, as ps names it. The text is whole;
// each reader cuts it to its own bound (cut), after the redaction, so a cut never leaves part of
// a credential that the whole would have matched.
func commandText(p *Process) string {
	var b strings.Builder
	if len(p.Args) == 0 {
		b.WriteByte('[')
		escape(&b, p.Name)
		b.WriteByte(']')
	}
	for i, arg := range p.Args {
		if i > 0 {
			b.WriteByte(' ')
		}
		escape(&b, arg)
	}
	return capabilities.RedactCredentials(b.String())
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
