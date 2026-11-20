package redact

import "strings"

// WordsNotReadable is what a message prints in place of a remote field the rule cannot read as
// clean (Remote, Field). It is a different case from brokeraddr.WordsNotPrinted, which an address
// that is not clean prints in place of every word; brokeraddr.Address.Words prints both.
const WordsNotReadable = "(not printed: the answer's words hold a URL the rule cannot read as clean)"

// Remote is the rule for one field another party wrote - a refusal's words, a code, a body. It
// fails closed: it answers the field with each URL's user information removed, each query and
// fragment value removed (the names stay) and each bare query or fragment segment removed, and
// true, only when it can read every URL in the field with certainty; otherwise "" and false.
//
// The grammar, applied to the field:
//  1. A URL starts at a scheme ([A-Za-z][A-Za-z0-9+.-]*) followed by ':' and two slashes, or at
//     two slashes not preceded by a scheme character, ':' or a slash. Either slash may be a
//     backslash, since a browser reads one as a slash; step 3 rejects such a start.
//  2. A run is the text from a URL start to the next ASCII whitespace or the end of the field.
//     When it does not end inside a query or a fragment, the characters .,;:!?)]}'" at its end
//     are not part of it; when it does, they are part of the last value.
//  3. Not readable when a run holds a '%', a '\', a quote, '<', '>', a backtick, a control
//     character or any non-ASCII character (U+2028 and U+2029 among them); when its authority
//     (before the first '/', '?' or '#') holds more than one '@'; when it holds an '@' after its
//     authority; or when a port is not all digits.
//  4. Not readable when a query or fragment segment (split on '&' and ';') has a value that
//     holds '=' or '?'.
//  5. Not readable when a whitespace-delimited token, trimmed as in 2, holds an '@' with a ':'
//     before it (user:secret@10.0.0.5:443, proxy=user:pw@host). In a token that holds a URL
//     start this reads the text before the start; the run itself is read by 3 and 4, so a URL
//     with user information stays readable.
//  6. Whitespace can split a URL, so not readable when a token after the token that holds the
//     first URL start holds an '@'; when the token right after a run holds '?', '#' or '='; when
//     a run ends inside a query or a fragment and more text follows it in the field; or when a
//     run is a scheme, two slashes, one word of unreserved bytes and a ':' that ends its token,
//     and more text follows (https://user: password/x, splitUserInfo). A ':' after a quote, a
//     parenthesis or a port ('https://github.com': in git's errors) is prose.
//  7. An encoded URL has no start for 1 to find, so not readable when the field holds an encoded
//     '/', ':', '@', '?', '#', '=', '&', '%' or '\': %2F, %3A, %40, %3F, %23, %3D, %26, %25 or
//     %5C, the same as \u00XX, or as \xXX, in any case. An encoded '%' catches a URL encoded more
//     than once; a '%' with no such pair after it, as in "100%", is ordinary text.
//  8. Whitespace can split a URL or a credential however many gaps it puts in it, so the rule
//     reads the field again with its ASCII whitespace removed (markersAccounted) and fails
//     closed: from the earliest URL start step 1 finds in that text or in a token of the field
//     (a gap can hide a start as well as make one: "https ://host" reads "https://host"), every
//     '?', '#', '@' and key-led '=' (an '=' right after a letter, a digit, '_', '-' or '.') must
//     lie inside a run the first reading read as a clean URL; any other makes the field not
//     readable, whatever byte sits between it and the URL. Before that start, an '@' before a
//     byte a host can start with, a ':' before it with only user-information bytes between, and
//     a key-led '=' anywhere before that ':', is a key's split user information
//     (proxy=[user: pw@host]) and makes the field not readable (userInfoPiece).
//
// What step 8 cannot tell from text is the known residual, and prints as it came:
//   - a label, a ':' and an address with no key before them ("sign-in: ada@example.com"), which
//     is exactly what a split user:secret@host looks like once its scheme is gone, while refusals
//     print identities in this form; no syntax tells the two apart;
//   - the same inside a JSON member ({"proxy":"user: pw@host"}), whose key is followed by '":'
//     and not by '=', so it is a quoted label, an address and nothing that leads a credential.
//
// A field with no URL comes back as it is. Remote does not quote: Field puts its answer through
// OneLine. Its answer is not meant to be read by the rule again: a run whose last value was
// removed can end in '=' with text after it. The TypeScript half is readRemoteText in
// packages/contracts/src/remote-text.ts; both are held to the fixture at testdata/remote.json.
func Remote(field string) (string, bool) {
	if encodedSeparator(field) {
		return "", false
	}
	// read is what the reading of field's tokens did with each byte, which step 8 compares with;
	// a field with no whitespace has no second reading.
	var read []byteRead
	if strings.ContainsAny(field, " \t\n\v\f\r") {
		read = make([]byteRead, len(field))
	}
	var b strings.Builder
	b.Grow(len(field))
	seenURL, afterRun := false, false
	for i := 0; i < len(field); {
		if asciiSpace(field[i]) {
			b.WriteByte(field[i])
			i++
			continue
		}
		end := i
		for end < len(field) && !asciiSpace(field[end]) {
			end++
		}
		token := field[i:end]
		if seenURL && strings.IndexByte(token, '@') >= 0 || afterRun && strings.ContainsAny(token, "?#=") {
			return "", false
		}
		start := urlStart(token)
		outside := token
		if start >= 0 {
			outside = token[:start]
		}
		bare := trimRunEnd(outside)
		if colon := strings.IndexByte(bare, ':'); colon >= 0 && strings.LastIndexByte(bare, '@') > colon {
			return "", false
		}
		b.WriteString(outside)
		afterRun = false
		if start >= 0 {
			runAt := i + start
			shown, length, inQuery, ok := readRun(token[start:], func(from, to int) {
				for k := runAt + from; read != nil && k < runAt+to; k++ {
					read[k] = removedByRun
				}
			})
			for k := runAt; read != nil && k < runAt+length; k++ {
				read[k] = max(read[k], keptByRun)
			}
			more := strings.TrimLeft(field[end:], " \t\n\v\f\r") != ""
			if !ok || inQuery && more || more && splitUserInfo(token[start:]) {
				return "", false
			}
			b.WriteString(shown)
			b.WriteString(token[start+length:])
			seenURL, afterRun = true, true
		}
		i = end
	}
	if read != nil && !markersAccounted(field, read) {
		return "", false
	}
	return b.String(), true
}

// splitUserInfo reports whether run, the token's text from a URL start to the token's end, is a
// scheme, two slashes, one word of unreserved bytes (a letter, a digit, '-', '.', '_', '~') and a
// ':' that ends the token: https://user: password/x, a user information whitespace split after
// its ':' (Remote's step 6), when more text follows in the field. A ':' after punctuation, as in
// 'https://github.com': or (dispatch://CORE-1):, or after a port, is prose.
func splitUserInfo(run string) bool {
	head := 2
	if !slash(run[0]) {
		head = strings.IndexByte(run, ':') + 3
	}
	word, ok := strings.CutSuffix(run[min(head, len(run)):], ":")
	if !ok || word == "" {
		return false
	}
	for i := range len(word) {
		if c := word[i]; !letter(c) && (c < '0' || c > '9') && strings.IndexByte("-._~", c) < 0 {
			return false
		}
	}
	return true
}

// byteRead is what Remote's reading of a field's tokens did with one byte: read it outside every
// run, keep it in a run, or remove it from one.
type byteRead uint8

const (
	outsideRuns byteRead = iota
	keptByRun
	removedByRun
)

// markersAccounted is Remote's step 8: whether field, read with its ASCII whitespace removed,
// holds no '?', '#', '@' or key-led '=' after the earliest URL start (found in that text or in a
// token of the field) that read, the reading of its tokens, did not read inside a clean run, and
// before that start no key's split user information (userInfoPiece).
func markersAccounted(field string, read []byteRead) bool {
	squeezed := make([]byte, 0, len(field))
	at := make([]int, 0, len(field))
	start := -1
	for i := 0; i < len(field); {
		if asciiSpace(field[i]) {
			i++
			continue
		}
		end := i
		for end < len(field) && !asciiSpace(field[end]) {
			end++
		}
		if u := urlStart(field[i:end]); u >= 0 && start < 0 {
			start = len(squeezed) + u
		}
		for k := i; k < end; k++ {
			squeezed = append(squeezed, field[k])
			at = append(at, k)
		}
		i = end
	}
	text := string(squeezed)
	if u := urlStart(text); u >= 0 && (start < 0 || u < start) {
		start = u
	}
	before := text
	if start >= 0 {
		before = text[:start]
	}
	if userInfoPiece(before) {
		return false
	}
	for k := max(start, 0); start >= 0 && k < len(text); k++ {
		if read[at[k]] == outsideRuns && marker(text, k) {
			return false
		}
	}
	return true
}

// marker reports whether text[k] is one of the bytes step 8 requires a clean run to account for:
// '?', '#', '@', or an '=' right after a key byte (keyLedEquals).
func marker(text string, k int) bool {
	switch text[k] {
	case '?', '#', '@':
		return true
	}
	return keyLedEquals(text, k)
}

// keyLedEquals reports whether text[k] is an '=' right after a letter, a digit, '_', '-' or '.':
// a key's, as in proxy= or access_token=.
func keyLedEquals(text string, k int) bool {
	if text[k] != '=' || k == 0 {
		return false
	}
	c := text[k-1]
	return letter(c) || '0' <= c && c <= '9' || c == '_' || c == '-' || c == '.'
}

// userInfoPiece reports whether text holds a key's split user information as step 8 reads one:
// an '@' before a byte a host can start with (hostByte), a ':' before it with only user-information
// bytes (userInfoByte) other than ':' between them, and a key-led '=' anywhere before that ':'
// (keyLedEquals), whatever bytes lie between the key and the user: proxy=[user: pw@host] and
// proxy=("user: pw@host") read as proxy=user: pw@host does. A ':' and an address with no key
// before them is a label and an address.
func userInfoPiece(text string) bool {
	// The first key-led '=': a ':' after it has a key before it.
	key := -1
	for k := 1; k < len(text) && key < 0; k++ {
		if keyLedEquals(text, k) {
			key = k
		}
	}
	for a := 0; key >= 0 && a+1 < len(text); a++ {
		if text[a] != '@' || !hostByte(text[a+1]) {
			continue
		}
		colon := a
		for colon > 0 && text[colon-1] != ':' && userInfoByte(text[colon-1]) {
			colon--
		}
		if colon > 0 && text[colon-1] == ':' && key < colon-1 {
			return true
		}
	}
	return false
}

// userInfoByte reports whether c is a byte RFC 3986 lets a user information hold: unreserved
// (a letter, a digit, '-', '.', '_', '~'), a sub-delimiter (!$&'()*+,;=), ':' or the '%' of an
// escape.
func userInfoByte(c byte) bool {
	return letter(c) || '0' <= c && c <= '9' || strings.IndexByte("-._~!$&'()*+,;=:%", c) >= 0
}

// hostByte reports whether c can start the host a user information's '@' comes before: a byte
// RFC 3986 lets a host name hold, other than ':', or the '[' of an IPv6 address.
func hostByte(c byte) bool { return c != ':' && userInfoByte(c) || c == '[' }

// Field is a remote field as a message may print it: what Remote answers, on one line (OneLine),
// or WordsNotReadable where Remote cannot read it.
func Field(field string) string {
	if shown, ok := Remote(field); ok {
		return OneLine(shown)
	}
	return WordsNotReadable
}

// urlStart is where the first URL in token starts (Remote's step 1), or -1.
func urlStart(token string) int {
	for i := 0; i+1 < len(token); i++ {
		if token[i] == ':' && i+2 < len(token) && slash(token[i+1]) && slash(token[i+2]) {
			if start := schemeStart(token, i); start >= 0 {
				return start
			}
			continue
		}
		if slash(token[i]) && slash(token[i+1]) && (i == 0 || !schemeByte(token[i-1]) && token[i-1] != ':' && !slash(token[i-1])) {
			return i
		}
	}
	return -1
}

// readRun reads the run at the start of text, which runs to text's end (Remote's steps 2, 3 and
// 4): it answers the run as it prints, its length in text, whether it ends inside a query or a
// fragment, and whether it is readable, and calls cut with each span of text, [from, to), that
// it removes: the user information with its '@', each value, and each bare segment.
func readRun(text string, cut func(from, to int)) (shown string, length int, inQuery, ok bool) {
	head := 2
	if !slash(text[0]) {
		head = strings.IndexByte(text, ':') + 3
	}
	run := text
	inQuery = strings.ContainsAny(run[head:], "?#")
	if !inQuery {
		run = trimRunEnd(run)
	}
	for i := range len(run) {
		if runRefuses(run[i]) {
			return "", 0, false, false
		}
	}
	rest := run[head:]
	authEnd := len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authEnd = i
	}
	authority, after := rest[:authEnd], rest[authEnd:]
	if strings.Count(authority, "@") > 1 || strings.IndexByte(after, '@') >= 0 {
		return "", 0, false, false
	}
	at := strings.IndexByte(authority, '@')
	host := authority[at+1:]
	if !digitPort(host) {
		return "", 0, false, false
	}
	if at >= 0 {
		cut(head, head+at+1)
	}
	path, query, fragment := after, "", ""
	queryAt, fragmentAt := head+authEnd, head+authEnd
	if i := strings.IndexByte(after, '#'); i >= 0 {
		path, fragment = after[:i], after[i:]
		fragmentAt += i
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i:]
		queryAt += i
	}
	var b strings.Builder
	b.WriteString(run[:head])
	b.WriteString(host)
	b.WriteString(path)
	for _, part := range []struct {
		text string
		at   int
	}{{query, queryAt}, {fragment, fragmentAt}} {
		if part.text == "" {
			continue
		}
		b.WriteByte(part.text[0])
		segments, at := part.text[1:], part.at+1
		for {
			end := strings.IndexAny(segments, "&;")
			segment := segments
			if end >= 0 {
				segment = segments[:end]
			}
			name, value, named := strings.Cut(segment, "=")
			if strings.ContainsAny(value, "=?") {
				return "", 0, false, false
			}
			switch {
			case named:
				b.WriteString(name)
				b.WriteByte('=')
				if value != "" {
					cut(at+len(name)+1, at+len(segment))
				}
			case segment != "":
				cut(at, at+len(segment))
			}
			if end < 0 {
				break
			}
			b.WriteByte(segments[end])
			segments, at = segments[end+1:], at+end+1
		}
	}
	return b.String(), len(run), inQuery, true
}

// digitPort reports whether host's port, if it names one, is all digits: after "]:" for a
// bracketed IPv6 host, and after the first ':' otherwise. A '[' with no ']' is not read.
func digitPort(host string) bool {
	port := ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return false
		}
		switch rest := host[end+1:]; {
		case rest == "":
		case rest[0] == ':':
			port = rest[1:]
		default:
			return false
		}
	} else if i := strings.IndexByte(host, ':'); i >= 0 {
		port = host[i+1:]
	}
	return strings.Trim(port, "0123456789") == ""
}

// encodedSeparators are the escapes Remote's step 7 rejects, lower-cased: of '/', ':', '@', '?',
// '#', '=', '&', '%' and '\', each as %XX, \u00XX and \xXX.
var encodedSeparators = func() []string {
	var all []string
	for _, hex := range []string{"2f", "3a", "40", "3f", "23", "3d", "26", "25", "5c"} {
		all = append(all, "%"+hex, `\u00`+hex, `\x`+hex)
	}
	return all
}()

// encodedSeparator reports whether field holds one of encodedSeparators, in any case. None holds
// whitespace, so a field holds one exactly where one of its tokens does.
func encodedSeparator(field string) bool {
	lower := strings.ToLower(field)
	for _, e := range encodedSeparators {
		if strings.Contains(lower, e) {
			return true
		}
	}
	return false
}

// trimRunEnd is s without the characters .,;:!?)]}'" it ends in (Remote's steps 2 and 5).
func trimRunEnd(s string) string { return strings.TrimRight(s, `.,;:!?)]}'"`) }

// schemeStart is where the scheme ending at colon starts in s: the first letter of the longest
// run of scheme bytes before colon, or -1 when that run holds no letter.
func schemeStart(s string, colon int) int {
	start := colon
	for start > 0 && schemeByte(s[start-1]) {
		start--
	}
	for start < colon && !letter(s[start]) {
		start++
	}
	if start == colon {
		return -1
	}
	return start
}

// runRefuses reports whether a run that holds c is not readable (Remote's step 3): a '%', a '\',
// a quote, '<', '>', a backtick, a control character, or a byte of a non-ASCII character.
func runRefuses(c byte) bool {
	return c < ' ' || c >= 0x7f || strings.IndexByte("%\\\"'<>`", c) >= 0
}

func slash(c byte) bool { return c == '/' || c == '\\' }

func letter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

func schemeByte(c byte) bool {
	return letter(c) || '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'
}

// asciiSpace reports whether c is ASCII whitespace, which ends a run and a token.
func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
