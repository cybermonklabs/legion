package agent

// shown.go gives the model each value of a tool result that the issue page writes in words, beside
// the value as the result holds it: a priority, 0 to 3 or null, as P0 to P3 or "No priority", and
// a time, an RFC 3339 string, relative to the run's time now, as the web's relativeTimeLabel
// (packages/dispatch/web/src/features/refs/Timestamp.tsx) writes it. The answer writes that form,
// so the answer and the page say each value alike and the model does no sum of its own. The time
// itself stays beside its words, as the result holds it: the answer writes a time as a link whose
// words are these and whose address is that time (dispatch-time:2026-11-18T11:35:00Z), which the
// page draws with the full date on hover. packages/contracts/fixtures/shown-values.json holds both
// rules, written by hand from the web's, and a test of the web and one of this package each hold
// their side to it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// shownSuffix ends the name of the field that follows a value with its words: priority is
// followed by priority_shown, closed_at by closed_at_shown.
const shownSuffix = "_shown"

// noPriority is the words of a priority that holds none.
const noPriority = "No priority"

// The lengths, in milliseconds, of the units relativeTime writes in.
const (
	minuteMillis = int64(time.Minute / time.Millisecond)
	hourMillis   = 60 * minuteMillis
	dayMillis    = 24 * hourMillis
)

// relativeUnits are the units the web's relativeTimeLabel writes a time in, longest first, each
// with its length (its RELATIVE_UNITS), and what Intl.RelativeTimeFormat writes in English, with
// numeric "auto", for one of it before now and one after.
var relativeUnits = []struct {
	millis     int64
	name       string
	last, next string
}{
	{365 * dayMillis, "year", "last year", "next year"},
	{30 * dayMillis, "month", "last month", "next month"},
	{7 * dayMillis, "week", "last week", "next week"},
	{dayMillis, "day", "yesterday", "tomorrow"},
	{hourMillis, "hour", "1 hour ago", "in 1 hour"},
	{minuteMillis, "minute", "1 minute ago", "in 1 minute"},
}

// relativeTime is at relative to now as the web's relativeTimeLabel writes it: "just now" within
// a minute either side, and otherwise the count of the longest unit at least one of which lies
// between them, rounded as JavaScript's Math.round rounds, a half toward the later time: "5 minutes
// ago", "yesterday", "2 weeks ago", "in 3 days". Both are read to the millisecond, as a
// JavaScript Date holds a time.
func relativeTime(at, now time.Time) string {
	delta := at.UnixMilli() - now.UnixMilli()
	distance := max(delta, -delta)
	if distance < minuteMillis {
		return "just now"
	}
	for _, unit := range relativeUnits {
		if distance < unit.millis {
			continue
		}
		count := floorDiv(2*delta+unit.millis, 2*unit.millis)
		switch {
		case count == -1:
			return unit.last
		case count == 1:
			return unit.next
		case count < 0:
			return grouped(-count) + " " + unit.name + "s ago"
		default:
			return "in " + grouped(count) + " " + unit.name + "s"
		}
	}
	return ""
}

// floorDiv is a divided by b, b above 0, rounded down.
func floorDiv(a, b int64) int64 {
	quotient := a / b
	if a%b != 0 && a < 0 {
		quotient--
	}
	return quotient
}

// grouped is n, 0 or more, with its thousands apart by commas, as English writes a number: 2,027.
func grouped(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var out strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

// timeShape is the shape of a value relativeTime writes: an RFC 3339 time as the server writes one
// (time.RFC3339Nano) and the web reads it (packages/dispatch/web/src/lib/timestamps.ts).
var timeShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// shownWords is the words the issue page writes for value, a token of a decoder that reads
// numbers as json.Number, the value of the field named key, and whether it writes any: for a
// priority that holds 0 to 3 or null, P0 to P3 or "No priority"; for any field that holds an RFC
// 3339 time, the time relative to now.
func shownWords(key string, value any, now time.Time) (string, bool) {
	switch value := value.(type) {
	case nil:
		if key == "priority" {
			return noPriority, true
		}
	case json.Number:
		if key == "priority" && (value == "0" || value == "1" || value == "2" || value == "3") {
			return "P" + string(value), true
		}
	case string:
		if !timeShape.MatchString(value) {
			return "", false
		}
		if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return relativeTime(at, now), true
		}
	}
	return "", false
}

// shownMember is one field shownMembers writes into a body: text, a JSON member, which follows the
// value that ends at the body's byte at, a priority's where priority is set and a time's where not.
type shownMember struct {
	at       int
	text     string
	priority bool
}

// shownMembers is the field that follows each value of body, a tool result's JSON, that the issue
// page writes in words (shownWords): `,"priority_shown":"P1"` after a priority of 1, and
// `,"closed_at_shown":"2 days ago"` after a closed_at two days before now. A value in a list names
// no field and is followed by none. Body is read as far as it is JSON, so a body cut short
// (shortenedNote) gives the members of the values before its cut, and one that is no JSON none.
func shownMembers(body []byte, now time.Time) []shownMember {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	// open is the objects and lists the decoder is inside, the innermost last: whether each is
	// an object, and, for an object, the key of the value it reads next, where it has read one.
	type container struct {
		object bool
		key    string
		keyed  bool
	}
	var open []container
	var members []shownMember
	for {
		token, err := decoder.Token()
		if err != nil {
			return members
		}
		var inner *container
		if len(open) > 0 {
			inner = &open[len(open)-1]
		}
		if delim, isDelim := token.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				if inner != nil {
					inner.keyed = false
				}
				open = append(open, container{object: delim == '{'})
			default:
				open = open[:len(open)-1]
			}
			continue
		}
		if inner == nil || !inner.object {
			continue
		}
		if !inner.keyed {
			inner.key, inner.keyed = token.(string), true
			continue
		}
		inner.keyed = false
		if words, shown := shownWords(inner.key, token, now); shown {
			members = append(members, shownMember{at: int(decoder.InputOffset()), text: "," + jsonString(inner.key+shownSuffix) + ":" + jsonString(words),
				priority: inner.key == "priority"})
		}
	}
}

// jsonString is text as a JSON string.
func jsonString(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// shownResult is result, a tool result as resultText saved it, as the model is given it: the same
// text with each value of its body that the issue page writes in words followed by a field of
// those words (shownMembers), relative to now where it is a time. The body and its words stay
// within modelResultLimit bytes, and a body the model was given whole stays whole: where the words
// of each of its values do not fit beside it, the words of its priorities go first, then those of
// its times, each in the body's order, as many as fit, and each value past them is given as the
// result holds it. A body given only as its first bytes (shortenedNote) is cut further, so each
// value it gives is followed by its words, and its note names the bytes now given. A result that
// holds no call's answer, or no such value, is given as it was saved. The saved step keeps the
// result as the server answered it, which draw_block and ReadSavedResult read.
func shownResult(result string, now time.Time) string {
	saved, start, answered := readResult(result)
	if !answered {
		return result
	}
	members := shownMembers(saved.Body, now)
	if len(members) == 0 {
		return result
	}
	body := saved.Body
	tail := result[start+len(body):]
	var out strings.Builder
	out.Grow(len(result) + modelResultLimit - len(body) + len(shortenedNote))
	out.WriteString(result[:start])
	if !saved.Shortened {
		writeMembers(&out, body, fittingMembers(members, modelResultLimit-len(body)))
		out.WriteString(tail)
		return out.String()
	}
	// kept is how many members fit, added their bytes, and cut the bytes of body given.
	kept, added, cut := 0, 0, len(body)
	for _, member := range members {
		if member.at+added+len(member.text) > modelResultLimit {
			cut = member.at
			break
		}
		added += len(member.text)
		kept++
	}
	cut = min(cut, modelResultLimit-added)
	for cut > 0 && cut < len(body) && !utf8.RuneStart(body[cut]) {
		cut--
	}
	writeMembers(&out, body[:cut], members[:kept])
	if sizes := shortenedStart.FindStringSubmatch(tail); sizes != nil {
		total, _ := strconv.Atoi(sizes[1])
		given, _ := strconv.Atoi(sizes[2])
		fmt.Fprintf(&out, shortenedNote, total, cut)
		tail = strings.TrimPrefix(tail, fmt.Sprintf(shortenedNote, total, given))
	}
	out.WriteString(tail)
	return out.String()
}

// fittingMembers is the members of a whole body that fit in room bytes: a priority's before any
// time's, each kind in the body's order until one does not fit, in the body's order.
func fittingMembers(members []shownMember, room int) []shownMember {
	keep := make([]bool, len(members))
	for _, priorities := range []bool{true, false} {
		for index, member := range members {
			if member.priority != priorities {
				continue
			}
			if len(member.text) > room {
				break
			}
			keep[index] = true
			room -= len(member.text)
		}
	}
	fitting := make([]shownMember, 0, len(members))
	for index, member := range members {
		if keep[index] {
			fitting = append(fitting, member)
		}
	}
	return fitting
}

// writeMembers writes body to out with each of members, in the body's order, after its value.
func writeMembers(out *strings.Builder, body []byte, members []shownMember) {
	from := 0
	for _, member := range members {
		out.Write(body[from:member.at])
		out.WriteString(member.text)
		from = member.at
	}
	out.Write(body[from:])
}
