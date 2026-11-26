// Package logmask is what keeps a placeholder, and the bytes a peer sent, off the broker's log.
//
// Much of what a line names is a command's own text - the method, the path, the host and port it
// asked for, a reason or an error that quotes them, a refusal's correlation id from
// X-Correlation-Id, the name in a request's path - and a command can write its own placeholder
// into any of it. So the broker writes every line through Writer, under its JSON handler, which
// writes each placeholder in the line as Mark, and points slog and the log package at that handler
// (SetDefault).
//
// The mask reads the line the handler wrote, not the values it was given: slog's JSON handler
// writes each record as one JSON object in one Write, under its own lock. The mask decodes each
// string of that object - each key, a group's name among them, and each string value, which is
// how the handler writes a message, an error, a value's MarshalJSON or MarshalText, and the text
// slog writes for a value whose method failed - and writes again, with the placeholder as Mark,
// only a string whose decoded text holds one, leaving every other byte of the line as the handler
// wrote it. So it reads a value once, as written, after any ReplaceAttr; it calls no method of a
// value, and cannot panic for one; a placeholder hidden in a JSON escape is masked; a caller's
// text changes no token but its own, and the line stays one JSON object. What it cannot see is a
// placeholder the handler writes in another spelling: a []byte, which the JSON handler writes in
// base64 at any depth, a placeholder a value writes percent-escaped or split, and the 32
// characters after the prefix written without it. The broker logs no such value.
//
// A real value the swap put in is no placeholder, and the mask cannot see it. What keeps it off
// a line is that no line reads a request's headers or body, and that each run of bytes a library
// quotes from a peer, which can hold it, is written as PeerBytesShown (HidePeerBytes).
package logmask

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// Mark is what a line writes in place of a placeholder.
const Mark = "[placeholder]"

// markToken is Mark as one JSON string.
const markToken = `"` + Mark + `"`

// NotJSON is the line the mask writes in place of a line that is no JSON object: one it cannot
// read is one it cannot mask, so none of it is written.
const NotJSON = `{"severity":"ERROR","message":"broker: a log line that was no JSON object was not written"}`

// prefix is the default prefix a placeholder starts with (placeholder.Prefix), which every
// placeholder the broker mints opens with unless its kind sets a prefix of its own (AddPrefixes),
// in any case: Go's case folding, which also reads ſ as s.
var prefix = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(placeholder.Prefix))

// mintedWord is a minted kind's word and hyphen (placeholder.MintedPrefix) right after a prefix,
// in any case, so the characters a minted grant's placeholder draws after them are masked as a
// release's are. A release's placeholder holds no hyphen, so the word is read only where a kind's
// prefix put one, or where the next prefix starts: of a prefix written twice, the first may read
// the word "agent" of the second, which takes nothing from the second, whose opening is a Mark of
// its own all the same (maskText).
var mintedWord = regexp.MustCompile(`^(?i)[a-z]{1,` + strconv.Itoa(placeholder.MaxMintedWord) + `}-`)

// prefixSet is every prefix the mask reads: placeholder.Prefix, and each kind's own prefix that
// AddPrefixes added. A set is never changed once current holds it; AddPrefixes stores a new one.
type prefixSet struct {
	// kinds is each kind's own prefix, as maskText finds it: in any case, as prefix is found.
	kinds []kindPrefix
	// folded is each prefix as mayHoldPlaceholder looks for it in a line's bytes,
	// placeholder.Prefix first: in lower case, with ſ written s and the Kelvin sign k, the ASCII
	// letters Go's case folding reads them as. No other character past ASCII folds to one in
	// ASCII.
	folded []string
	// first is whether a byte is the first of a prefix in folded, in either ASCII case.
	first [256]bool
	// escaped is whether a kind's prefix holds a character JSON can write with an escape other
	// than \u - " \ / and the controls \b \f \n \r \t - so that a string's decoded text can hold
	// the prefix where the line's bytes do not.
	escaped bool
}

// kindPrefix is one kind's own prefix (proxyrules.Kind.PlaceholderPrefix), as it was added, and
// the pattern that finds it in any case.
type kindPrefix struct {
	text    string
	pattern *regexp.Regexp
}

// current is the prefixSet every line is read with. addMu is held by AddPrefixes from its read of
// current to its store, so two calls never drop each other's prefixes.
var (
	current atomic.Pointer[prefixSet]
	addMu   sync.Mutex
)

func init() { current.Store(newPrefixSet(nil)) }

// AddPrefixes makes the mask read each of prefixes as a placeholder's prefix, beside
// placeholder.Prefix: the placeholder_prefix of each kind of the profiles file
// (proxyrules.Kind.PlaceholderPrefix), which the release mints that kind's placeholders with in
// place of placeholder.Prefix. A placeholder opens a proxy connection, so no line may write one;
// a kind's prefix opens a placeholder only where it is whole (maskText). The broker
// (cmd/cipherd) calls it once it has read the profiles file and before it serves anything, so
// before any line can quote a command's text. Each line written after it returns is read with the
// prefixes, from any goroutine; a line written while it runs is read with them or without them. A
// prefix once added is read for the life of the process. "", a prefix already read, and
// placeholder.Prefix itself in any case, which the mask reads with no boundary and in part
// already, add nothing. Each prefix is valid UTF-8, as each string encoding/json decodes is.
func AddPrefixes(prefixes []string) {
	addMu.Lock()
	defer addMu.Unlock()
	held := current.Load()
	kinds := make([]string, 0, len(held.kinds)+len(prefixes))
	for _, k := range held.kinds {
		kinds = append(kinds, k.text)
	}
	for _, p := range prefixes {
		read := func(k string) bool { return strings.EqualFold(k, p) }
		if p != "" && !read(placeholder.Prefix) && !slices.ContainsFunc(kinds, read) {
			kinds = append(kinds, p)
		}
	}
	if len(kinds) > len(held.kinds) {
		current.Store(newPrefixSet(kinds))
	}
}

// newPrefixSet is the prefixSet of placeholder.Prefix and kinds.
func newPrefixSet(kinds []string) *prefixSet {
	set := &prefixSet{}
	for _, p := range append([]string{placeholder.Prefix}, kinds...) {
		folded := foldASCII(p)
		set.folded = append(set.folded, folded)
		set.first[folded[0]] = true
		if 'a' <= folded[0] && folded[0] <= 'z' {
			set.first[folded[0]-('a'-'A')] = true
		}
	}
	for _, p := range kinds {
		set.kinds = append(set.kinds, kindPrefix{p, regexp.MustCompile(`(?i)` + regexp.QuoteMeta(p))})
		set.escaped = set.escaped || strings.ContainsAny(p, "\"\\/\b\f\n\r\t")
	}
	return set
}

// foldASCII is text as mayHoldPlaceholder compares it: each ASCII capital letter written small,
// ſ written s, the Kelvin sign written k, and every other character as it is.
func foldASCII(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 'ſ':
			return 's'
		case r == '\u212a':
			return 'k'
		case 'A' <= r && r <= 'Z':
			return r + ('a' - 'A')
		}
		return r
	}, text)
}

// Writer is w behind the placeholder mask, for the lines slog's JSON handler writes: each line of
// each Write is read as one JSON object, each of its strings holding a placeholder is written
// again with it as Mark, and a line that is no JSON object is written as NotJSON. Each Write is
// read on its own, so a record must arrive in one Write, as slog's handler writes it
// (TestSlogWritesEachRecordInOneWrite); one split over two would be two lines that are no JSON
// object.
func Writer(w io.Writer) io.Writer { return maskWriter{w} }

type maskWriter struct{ w io.Writer }

func (m maskWriter) Write(p []byte) (int, error) {
	masked, same := maskLines(p)
	if same {
		return m.w.Write(p)
	}
	if _, err := m.w.Write(masked); err != nil {
		return 0, err
	}
	return len(p), nil
}

// maskLines is p with each of its lines masked (maskLine), and true where that changed nothing:
// then it is p itself, and no line of it was copied. A Write that changed ends in a newline, so a
// line written as NotJSON is a line of its own.
func maskLines(p []byte) ([]byte, bool) {
	if len(p) == 0 {
		return p, true
	}
	var out []byte
	start, rest := 0, bytes.TrimSuffix(p, []byte("\n"))
	for {
		line, after, more := bytes.Cut(rest, []byte("\n"))
		masked, kept := maskLine(line)
		switch {
		case !kept && out == nil:
			out = append(append(make([]byte, 0, len(p)), p[:start]...), masked...)
		case out != nil:
			out = append(out, masked...)
		}
		if !more {
			break
		}
		if out != nil {
			out = append(out, '\n')
		}
		start, rest = start+len(line)+1, after
	}
	if out == nil {
		return p, true
	}
	return append(out, '\n'), false
}

// maskLine is line, one JSON object, with each string whose decoded text holds a placeholder
// written again with Mark in its place and every other byte as it came, and true where no string
// did; a line that is no JSON object is NotJSON. A line that mayHoldPlaceholder clears is read no
// further.
func maskLine(line []byte) ([]byte, bool) {
	if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(line) {
		return []byte(NotJSON), false
	}
	if !mayHoldPlaceholder(line) {
		return line, true
	}
	var out []byte
	kept := 0
	for i := 0; i < len(line); {
		if line[i] != '"' {
			i++
			continue
		}
		end := stringEnd(line, i)
		if masked, ok := maskString(line[i:end]); ok {
			out = append(append(out, line[kept:i]...), masked...)
			kept = end
		}
		i = end
	}
	if out == nil {
		return line, true
	}
	return append(out, line[kept:]...), false
}

// mayHoldPlaceholder is false only for a line no string of which can hold a placeholder once
// decoded: one of ASCII alone, with no \u escape, that holds no prefix the mask reads
// (placeholder.Prefix and each kind's, AddPrefixes) in any ASCII case. Every other escape JSON has
// (\" \\ \/ \b \f \n \r \t) decodes to a character that is neither one of placeholder.Prefix's nor
// one of a placeholder's after its prefix, so a placeholder the decoded text holds is written in
// the line's own bytes too; where a kind's prefix holds such a character (prefixSet.escaped), a
// line with any escape is read string by string. A line with a \u escape or a byte past ASCII is
// read string by string, so the escape is decoded and a letter Go's case folding reads as one of a
// prefix's, such as ſ, is read as that letter; and where a kind's prefix holds ſ or the Kelvin
// sign, it is looked for as the s or k an ASCII line holds in its place (prefixSet.folded).
func mayHoldPlaceholder(line []byte) bool {
	set := current.Load()
	for i, b := range line {
		switch {
		case b == '\\' && (set.escaped || i+1 < len(line) && line[i+1] == 'u'), b >= utf8.RuneSelf:
			return true
		case set.first[b] && set.opensAt(line[i:]):
			return true
		}
	}
	return false
}

// opensAt is whether text opens with a prefix of s, in any ASCII case.
func (s *prefixSet) opensAt(text []byte) bool {
	for _, folded := range s.folded {
		if len(text) >= len(folded) && opensWithASCIIFold(text, folded) {
			return true
		}
	}
	return false
}

// opensWithASCIIFold is whether text opens with lower, written in lower case, in any ASCII case.
func opensWithASCIIFold(text []byte, lower string) bool {
	for j := range len(lower) {
		c := text[j]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[j] {
			return false
		}
	}
	return true
}

// stringEnd is the index just past the JSON string that opens at line[start], in a line that is
// valid JSON: past the first quotation mark after it that no backslash escapes.
func stringEnd(line []byte, start int) int {
	for i := start + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(line)
}

// maskString is token, one JSON string, written again with each placeholder its decoded text
// holds as Mark (maskText), HTML left unescaped as slog's handler leaves it, and false for a
// string that holds none. A string with no escape is its own text, read where it lies. The mask
// fails closed: a string it cannot decode, or cannot write again, is written as Mark, whole, since
// the mask cannot say what it holds. Writer reads only a line json.Valid passes, whose every string
// decodes, and a Go string always encodes, so neither happens from Writer.
func maskString(token []byte) ([]byte, bool) {
	text := token[1 : len(token)-1]
	if bytes.IndexByte(text, '\\') >= 0 {
		var decoded string
		if err := json.Unmarshal(token, &decoded); err != nil {
			return []byte(markToken), true
		}
		text = []byte(decoded)
	}
	masked, ok := maskText(text)
	if !ok {
		return nil, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(string(masked)); err != nil {
		return []byte(markToken), true
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), true
}

// maskText is text with each placeholder in it written as Mark, and false where it holds none. A
// placeholder opens with a prefix in any case, as Go's case folding reads one, so ſ and the Kelvin
// sign are letters here too, and is one of these:
//
//   - placeholder.Prefix, anywhere, then a minted kind's word and hyphen where they follow it
//     (mintedWord), then the letters and digits after them, at most as many as the broker mints
//     (placeholder.Length): so the prefix written alone is masked too;
//   - a kind's own prefix (AddPrefixes) and the placeholder.Length letters and digits after it,
//     which must all be there, and with no ASCII letter or digit right before it as the mask
//     writes the text: the prefix opens the text, or the character before it is no ASCII letter
//     or digit, or it is a character of a placeholder masked before it, which the mask writes as
//     Mark. A kind's prefix is short, and the release mints it whole, so tinker's tml- in html-
//     and a hash is no placeholder, and a placeholder written right after another one is one.
//
// Any character may follow a placeholder. Each placeholder is a Mark of its own, which takes the
// text from where it opens to where it ends, or to where every placeholder that opened before it
// ends, if that is further: so of two placeholders written together, or of the prefix written
// before a placeholder, each is a Mark, and no character of either is left in clear, whichever
// prefixes they open with.
func maskText(text []byte) ([]byte, bool) {
	var out []byte
	covered, last := 0, -1
	for _, o := range openingsIn(text, current.Load()) {
		if o.kind && o.start > covered {
			if r, _ := utf8.DecodeLastRune(text[:o.start]); asciiLetterOrDigit(r) {
				continue
			}
		}
		if o.start != last {
			if o.start > covered {
				out = append(out, text[covered:o.start]...)
			}
			out = append(out, Mark...)
			last = o.start
		}
		covered = max(covered, o.end)
	}
	if last < 0 {
		return nil, false
	}
	return append(out, text[covered:]...), true
}

// opening is where a placeholder may open in a text and where it ends, and whether it opens with a
// kind's own prefix, which maskText opens only after a character that is no ASCII letter or digit.
type opening struct {
	start, end int
	kind       bool
}

// openingsIn is each opening of a placeholder in text, by where it starts: each placeholder.Prefix,
// with the minted kind's word and the minted characters that follow it, and each of a kind's own
// prefixes that placeholder.Length minted characters follow, wherever it starts, so a prefix that
// starts inside another is read too.
func openingsIn(text []byte, set *prefixSet) []opening {
	var openings []opening
	for _, at := range prefix.FindAllIndex(text, -1) {
		end := at[1]
		if word := mintedWord.FindIndex(text[end:]); word != nil {
			end += word[1]
		}
		end, _ = mintedRun(text, end)
		openings = append(openings, opening{start: at[0], end: end})
	}
	for _, kind := range set.kinds {
		for from := 0; from < len(text); {
			at := kind.pattern.FindIndex(text[from:])
			if at == nil {
				break
			}
			start := from + at[0]
			if end, whole := mintedRun(text, from+at[1]); whole {
				openings = append(openings, opening{start: start, end: end, kind: true})
			}
			_, size := utf8.DecodeRune(text[start:])
			from = start + size
		}
	}
	if len(set.kinds) > 0 {
		slices.SortFunc(openings, func(a, b opening) int { return cmp.Compare(a.start, b.start) })
	}
	return openings
}

// mintedRun is where the run of minted characters (minted) that text holds from start ends, after
// at most placeholder.Length of them, and whether it holds that many.
func mintedRun(text []byte, start int) (int, bool) {
	end := start
	for range placeholder.Length {
		r, size := utf8.DecodeRune(text[end:])
		if size == 0 || !minted(r) {
			return end, false
		}
		end += size
	}
	return end, true
}

// asciiLetterOrDigit is whether r is an ASCII letter or digit, the characters that keep a kind's
// prefix right after them from opening a placeholder. Every other character, ſ and the Kelvin
// sign among them, leaves it open: the mask fails closed.
func asciiLetterOrDigit(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// minted is whether r is a character of a placeholder after its prefix: one of the alphabet the
// broker mints from (placeholder.Alphabet, both cases), or a letter Go's case folding
// reads as one of it, ſ (s) and the Kelvin sign (k).
func minted(r rune) bool {
	return strings.ContainsRune(placeholder.Alphabet, r) || r == 'ſ' || r == '\u212a'
}

// SetDefault makes handler the process's default logger and points the log package at it
// (StandardLog), so every line written through slog or the log package is handler's: the
// broker's own logging (cmd/cipherd's setUpLogging) calls it, and so does a test that routes lines
// as the broker does. slog.SetDefault points the log package at a writer of its own for every
// handler but slog's own, which writes each record through the log package, whose output holds
// the log package's lock while it writes: a log package pointed at that handler would wait on its
// own lock, and every log call with it. So where slog.SetDefault leaves the log package's output
// as it was, as it does for that handler and for any copy of it (WithAttrs, WithGroup), SetDefault
// leaves it too.
func SetDefault(handler slog.Handler) {
	logger := slog.New(handler)
	output := log.Writer()
	slog.SetDefault(logger)
	if log.Writer() == output {
		return
	}
	log.SetFlags(0)
	log.SetOutput(StandardLog(logger))
}

// PeerBytesShown is what a line writes in place of the bytes a library quoted from a peer.
const PeerBytesShown = `"[service bytes not shown]"`

// HidePeerBytes is text with each line's text from its first quotation mark to its end written as
// PeerBytesShown. net/http, net/textproto and httputil quote with %q every run of bytes they repeat
// from a peer - an answer's bytes on an idle connection, a header or trailer line they could not
// read, a status line, an ALPN list - and a bound service is sent the real value the swap put in,
// so it can send it back; the mask cannot see a real value. A reading that paired quotation marks
// would be misled by a stray one before a run, which would close where the run opens and leave the
// run itself in clear, so the whole rest of the line goes: a line keeps only its own words before
// the first quotation mark, which say what kind of thing went wrong.
func HidePeerBytes(text string) string {
	if !strings.Contains(text, `"`) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if quote := strings.IndexByte(line, '"'); quote >= 0 {
			lines[i] = line[:quote] + PeerBytesShown
		}
	}
	return strings.Join(lines, "\n")
}

// LibraryLines is a write a library made to a log of its own, as one line takes it: its peer
// bytes hidden (HidePeerBytes), its final newline dropped, and cut at its first newline into its
// first line and the rest, its stack, "" for a write of one line.
func LibraryLines(p []byte) (first, stack string) {
	first, stack, _ = strings.Cut(HidePeerBytes(strings.TrimSuffix(string(p), "\n")), "\n")
	return first, stack
}

// StandardLog is the log package's output once SetDefault has run: one line of logger at INFO
// for each write, as slog.SetDefault's own writer makes one. Every write is a library's text
// (LibraryLines). The line's message is the first line cut to answer.FieldBytes (answer.Clip),
// since a library's first line can repeat what a client or a service sent, and its stack is every
// line after it, whole: a recovered panic's stack, which a server with no ErrorLog of its own
// writes here and which the cut would take the frames off.
func StandardLog(logger *slog.Logger) io.Writer { return standardLog{logger} }

type standardLog struct{ log *slog.Logger }

func (w standardLog) Write(p []byte) (int, error) {
	first, stack := LibraryLines(p)
	if stack == "" {
		w.log.Info(answer.Clip(first))
	} else {
		w.log.Info(answer.Clip(first), "stack", stack)
	}
	return len(p), nil
}
