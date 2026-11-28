package charts

import (
	"iter"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:generate python3 testdata/gen-unicode-tables.py --out unicode_tables.go

// The text checks read text as Python does: by code point, not by byte (codePoints), with the
// printable and white space characters of Python 3.13's Unicode 15.1 (unicode_tables.go, which
// the script named above writes), not Go's newer ones. A string may hold a lone surrogate, which
// JSON can spell and Go's UTF-8 cannot: it is kept as the three bytes WTF-8 gives it, and
// codePoints reads it back.

const (
	// definitionTextMaxChars is the longest a line of plain text may be.
	definitionTextMaxChars = 2000
	shortLabelMaxChars     = 120
)

var (
	// seriesPattern is every series name a reading may log, so a chart can name any logged series.
	seriesPattern = "[a-z0-9][a-z0-9._-]{0,63}"
	seriesName    = whole(seriesPattern)
	// slug is a chart id: lowercase words joined by single hyphens.
	slug      = whole(`[a-z0-9]+(?:-[a-z0-9]+)*`)
	attribute = whole(`[A-Za-z_][A-Za-z0-9_.-]*`)
	team      = whole(`[A-Za-z0-9][A-Za-z0-9_.-]*`)
	folder    = whole(`[A-Za-z0-9_.-]+`)
)

// whole compiles a pattern that must match all of a string, as Python's re.fullmatch does.
func whole(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`^(?:` + pattern + `)$`)
}

// matches is Python's isinstance(value, str) and pattern.fullmatch(value).
func matches(pattern *regexp.Regexp, value any) bool {
	text, ok := value.(string)
	return ok && pattern.MatchString(text)
}

// codePoints is the code points of text as Python's str holds them: a lone surrogate, which
// WTF-8 writes as ED followed by A0 to BF and a continuation byte, is one code point.
func codePoints(text string) iter.Seq[rune] {
	return func(yield func(rune) bool) {
		for at := 0; at < len(text); {
			char, size := utf8.DecodeRuneInString(text[at:])
			if char == utf8.RuneError && size == 1 && len(text) >= at+3 && text[at] == 0xED &&
				text[at+1] >= 0xA0 && text[at+1] <= 0xBF && text[at+2]&0xC0 == 0x80 {
				char, size = rune(text[at]&0x0F)<<12|rune(text[at+1]&0x3F)<<6|rune(text[at+2]&0x3F), 3
			}
			if !yield(char) {
				return
			}
			at += size
		}
	}
}

// surrogate is the WTF-8 spelling of a lone surrogate.
func surrogate(char rune) string {
	return string([]byte{0xE0 | byte(char>>12), 0x80 | byte(char>>6)&0x3F, 0x80 | byte(char)&0x3F})
}

// pySpace is Python's str.isspace() of one character, which str.split() and str.strip() read.
func pySpace(char rune) bool {
	return unicode.Is(spaceTable, char)
}

// words is Python's len(text.split()).
func words(text string) int {
	return len(strings.FieldsFunc(text, pySpace))
}

// blank is Python's not text.strip().
func blank(text string) bool {
	return strings.TrimFunc(text, pySpace) == ""
}

// chars is Python's len(text): its code points.
func chars(text string) int {
	count := 0
	for range codePoints(text) {
		count++
	}
	return count
}

// printable is Python's str.isprintable(); an empty string is printable.
func printable(text string) bool {
	for char := range codePoints(text) {
		if !unicode.Is(printableTable, char) {
			return false
		}
	}
	return true
}

// plainText is one nonempty line of printable text of at most definitionTextMaxChars characters.
func plainText(value any) bool {
	text, ok := value.(string)
	return ok && !blank(text) && printable(text) && chars(text) <= definitionTextMaxChars
}

// plainTextOf is plainText of at most most characters.
func plainTextOf(value any, most int) bool {
	return plainText(value) && chars(value.(string)) <= most
}

func shortLabel(value any) bool {
	return plainTextOf(value, shortLabelMaxChars)
}

// lineOf is one line of plain text of at most most words.
func lineOf(value any, most int) bool {
	return plainText(value) && words(value.(string)) <= most
}

// paragraphsOf is one line of plain text, or a list of 1 to most such lines, of at most total
// words in all.
func paragraphsOf(value any, most, total int) bool {
	paragraphs, ok := value.([]any)
	if text, isText := value.(string); isText {
		paragraphs, ok = []any{text}, true
	}
	if !ok || len(paragraphs) < 1 || len(paragraphs) > most {
		return false
	}
	count := 0
	for _, paragraph := range paragraphs {
		if !plainText(paragraph) {
			return false
		}
		count += words(paragraph.(string))
	}
	return count <= total
}

// plainList is a list of 1 to most distinct lines of plain text: the shape of every list of
// names a declaration gives.
func plainList(value any, least, most int) bool {
	list, ok := value.([]any)
	if !ok || len(list) < least || len(list) > most {
		return false
	}
	seen := map[string]bool{}
	for _, item := range list {
		if !plainText(item) || seen[item.(string)] {
			return false
		}
		seen[item.(string)] = true
	}
	return true
}

// distinctNames is Python's len(set(names)) != len(names) turned round, for a list of strings.
func distinctNames(names []any) bool {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name.(string)] {
			return false
		}
		seen[name.(string)] = true
	}
	return true
}

// link is a printable line of at most definitionTextMaxChars characters that is one of the
// schemes, "://" and at least one character that is not a space.
func link(value any, schemes ...string) bool {
	text, ok := value.(string)
	if !ok || chars(text) > definitionTextMaxChars || !printable(text) {
		return false
	}
	for _, scheme := range schemes {
		if rest, found := strings.CutPrefix(text, scheme+"://"); found {
			return rest != "" && !strings.ContainsFunc(rest, pySpace)
		}
	}
	return false
}

// utcTime reads a UTC ISO time such as 2026-11-11T09:00:00Z, with up to six digits of a second's
// fraction and Z or +00:00, and a date and time of day that exist.
func utcTime(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || len(text) < 20 {
		return time.Time{}, false
	}
	moment, ok := dateOf(text[:10])
	if !ok || text[10] != 'T' || !digitsAt(text[11:19], 0, 1, 3, 4, 6, 7) || text[13] != ':' || text[16] != ':' {
		return time.Time{}, false
	}
	rest := text[19:]
	nanos := 0
	if strings.HasPrefix(rest, ".") {
		end := 1
		for end < len(rest) && '0' <= rest[end] && rest[end] <= '9' {
			end++
		}
		if end == 1 || end > 7 {
			return time.Time{}, false
		}
		fraction := rest[1:end] + strings.Repeat("0", 9-(end-1))
		nanos = atoi(fraction)
		rest = rest[end:]
	}
	if rest != "Z" && rest != "+00:00" {
		return time.Time{}, false
	}
	hour, minute, second := atoi(text[11:13]), atoi(text[14:16]), atoi(text[17:19])
	if hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	return moment.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute +
		time.Duration(second)*time.Second + time.Duration(nanos)), true
}

// utcDate reads a UTC date YYYY-MM-DD that exists, as its first moment.
func utcDate(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || len(text) != 10 {
		return time.Time{}, false
	}
	return dateOf(text)
}

func dateOf(text string) (time.Time, bool) {
	if !digitsAt(text, 0, 1, 2, 3, 5, 6, 8, 9) || text[4] != '-' || text[7] != '-' {
		return time.Time{}, false
	}
	year, month, day := atoi(text[:4]), atoi(text[5:7]), atoi(text[8:10])
	if year < 1 || month < 1 || month > 12 || day < 1 {
		return time.Time{}, false
	}
	moment := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if moment.Day() != day {
		return time.Time{}, false
	}
	return moment, true
}

func digitsAt(text string, places ...int) bool {
	for _, place := range places {
		if place >= len(text) || text[place] < '0' || text[place] > '9' {
			return false
		}
	}
	return true
}

func atoi(digits string) int {
	value := 0
	for _, digit := range digits {
		value = value*10 + int(digit-'0')
	}
	return value
}
