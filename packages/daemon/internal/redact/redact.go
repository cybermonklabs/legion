// Package redact removes the credentials a URL can carry from text the daemon did not write: a
// far side's error, an HTTP client's error, a log line. Such text can hold a URL with a token in
// its userinfo or in a query parameter (a redirect to a signed download, a DSN, a remote with a
// token as its user name), and the daemon carries it on into a claim, the state, the store and
// the log.
//
// The rule is for error text only. Text that a person or an agent wrote (a handoff summary, a
// review body, an ask's note) keeps its links whole, query included (`?plain=1#L10`, `?pr=M`), so
// the store's own rule (internal/store/storetext.Text) does not apply this one.
//
// The daemon's own log takes the rule in its handler (HandlerOptions). The log of the worker's and
// the pod's own processes (the commands cmd/legion's podLogs names) takes it whole, through
// Writer: such a log mixes a far side's error with what an agent wrote (a tool call's summary),
// and a link of an agent's that loses a parameter in that log keeps it everywhere else.
//
// A far side's text that is cut to a bound is cut here, by Runes, Bytes or Tail, which give the
// whole text through URLCredentials before they cut it, or by Read, for a text a bounded read cut
// already. A far side is anything outside the process that writes the text: an HTTP answer, a
// child process's output, a frame from the daemon or the agent, a kubelet's message, an error that
// wraps any of those. The order is the point: a cut made before the rule can stop inside a URL's
// userinfo or its query and leave a part of either where the rule no longer finds it. Every such
// cut in the legion binary, the daemon's and the pod's commands alike, is one of theirs but those
// named here, each of which says where it cuts and what of a URL's credential it can keep: the
// role log's cut of a line longer than its bound (the launcher's logLineCut); the sandbox's drop
// of the head a cut log read starts inside, up to its first newline or else its first byte that
// ends a URL (dropCutHead, which readLog and lastBytes share), and readLog's drop of the line its
// read bound stopped inside; storetext.LogWhole's parts of a text the rule has read whole; and a
// detail whose own rules have masked every userinfo before its cut (capabilities' cleanDetail). A
// text the URL rule has already read whole, through URLCredentials or one of these helpers, may be
// cut by any means after it, as the workflow's quote of a failed claim's sentence and its death
// account is (quoteBounded): the supervisor keeps both through storetext's FarSideError and
// FarSideText. So what is cut and how is read from their callers and those rather than from every
// way Go shortens a string.
package redact

import (
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// URLCredentials is text with the userinfo of every URL written in it removed, and the value of
// every query parameter whose name is a credential's (credentialName).
//
// A URL starts at <scheme>://, where the scheme is a letter followed by letters, digits, "+", "-"
// or ".". It ends at the first white-space character, `"`, `'`, "`", "<" or ">" after the "://",
// or at the end of the text, so a URL that an error quotes (Go's %q, as a *url.Error writes its
// URL) or writes inside <…> ends where the quote does. A `file:` URL is left whole: it reaches no
// host, so it carries no credential, and its query is a module loader's own (`?mtime=1`). In any
// other URL:
//
//   - the authority runs from after the "://" up to the first "/", "?" or "#". Everything in it up
//     to and including its last "@" is the userinfo, and it is removed: a password and a token
//     written as the user name alike;
//   - the fragment starts at the first "#" after the authority, and it stays. A "?" after that "#"
//     is the fragment's;
//   - the query runs from the first "?" after the authority up to the fragment, or up to the end
//     of the URL. Its parameters are parted at each "&" and at each further "?" (a URL written in
//     the query of another starts a query of its own), each a name, and a value after its first
//     "=". The value of a parameter whose name is a credential's is removed, its name and "=" kept,
//     and every other parameter stays as written. A value that runs to the URL's end does not take
//     the run of ".", ",", ";", ":", "!", ")", "]" and "}" that ends it: that is the punctuation of
//     the prose around the URL.
//
// A URL written in the path, the query or the fragment of another (a redirect's target, an
// archived page) is a URL of its own, under the same rule. Nothing else changes: the scheme, the
// host and its port, the path, the fragment and every word around the URLs stay as written. The
// rule removes rather than masks, so neither a part nor the length of a credential is left, and
// URLCredentials(URLCredentials(s)) == URLCredentials(s).
//
// A text the rule does not change, a text with no "://" above all, is returned as it is, with no
// allocation. The rule takes time linear in the text's length, a run of "://" that nothing ends
// included, but for one sort of what it removes.
func URLCredentials(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	var cuts []span
	// parsed is how far the parameters of every query read so far have been read: a query that
	// overlaps an earlier one is a URL's written in it, which ends at the same fragment or URL end
	// (the first "#" after either authority, or the one end of the run), so each byte of a query
	// is read for parameters once.
	parsed := 0
	// The searches that read on to a URL's end are kept, so that a run of text holding many "://"
	// is read once rather than once for each of them: end is the first endsURL rune at or after
	// the position it was last looked for from, and slash, question and hash are the next "/",
	// "?" and "#" (nextByte). Each is looked for again only when it is behind the position now
	// asked about: one found from a position p is the answer for any later position it is not
	// behind, since none lies between p and it. So a kept answer is right only while no position
	// asked is before one asked earlier, and none is: in one URL at ≤ host, and host is at or
	// before the "/" of the next "://", so before the next URL's at.
	end := -1
	slash, question, hash := nextByte{c: '/', at: -1}, nextByte{c: '?', at: -1}, nextByte{c: '#', at: -1}
	for at := 0; ; {
		i := strings.Index(text[at:], "://")
		if i < 0 {
			break
		}
		sep := at + i
		at = sep + len("://")
		// schemeEndsAt reads back from sep over scheme characters alone, so no further than the
		// ":" of the "://" before: its reads of the whole text add up to the text's length.
		if !schemeEndsAt(text, sep) || fileSchemeEndsAt(text, sep) {
			continue
		}
		if end < at {
			end = at + orLen(strings.IndexFunc(text[at:], endsURL), len(text)-at)
		}
		host := min(slash.from(text, at), question.from(text, at), hash.from(text, at), end)
		// [at, host) ends at or before the "/" of the next "://", so the userinfo's searches read
		// spans apart from one another, the whole text once at most.
		if u := strings.LastIndexByte(text[at:host], '@'); u >= 0 {
			cuts = append(cuts, span{at, at + u + 1})
		}
		frag := min(hash.from(text, host), end)
		if q := question.from(text, host); q < frag {
			cuts = credentialValues(text, max(q+1, parsed), frag, end, cuts)
			parsed = max(parsed, frag)
		}
	}
	return removed(text, cuts)
}

// span is the bytes [from, to) of a text.
type span struct{ from, to int }

// removed is text without each of cuts, which may overlap and come in any order; text itself, with
// no allocation, where there are none.
func removed(text string, cuts []span) string {
	if len(cuts) == 0 {
		return text
	}
	slices.SortFunc(cuts, func(a, b span) int { return a.from - b.from })
	var out strings.Builder
	out.Grow(len(text))
	kept := 0
	for _, cut := range cuts {
		if cut.from > kept {
			out.WriteString(text[kept:cut.from])
		}
		kept = max(kept, cut.to)
	}
	out.WriteString(text[kept:])
	return out.String()
}

// credentialValues is cuts with the value of each parameter in text[from:to) whose name is a
// credential's added: the parameters are parted at each "&" and each "?", and end is the end of the
// URL, at which a value leaves the punctuation that ends it (proseEnd).
func credentialValues(text string, from, to, end int, cuts []span) []span {
	for start := from; start < to; {
		stop := start + orLen(strings.IndexAny(text[start:to], "&?"), to-start)
		if eq := strings.IndexByte(text[start:stop], '='); eq >= 0 && credentialName(text[start:start+eq]) {
			value, last := start+eq+1, stop
			if stop == end {
				last = proseEnd(text, value, stop)
			}
			if last > value {
				cuts = append(cuts, span{value, last})
			}
		}
		start = stop + 1
	}
	return cuts
}

// proseEnd is where text[from:to), a value that runs to a URL's end, stops before the run of
// punctuation that ends a sentence or closes a bracket around the URL.
func proseEnd(text string, from, to int) int {
	for to > from && strings.IndexByte(".,;:!)]}", text[to-1]) >= 0 {
		to--
	}
	return to
}

// credentialNames are the query parameter names whose value is a credential, in lower case: a
// token's, a key's, a password's, a signed URL's signature and credential (Amazon's and Google's),
// and an OAuth code.
var credentialNames = map[string]bool{
	"token": true, "access_token": true, "refresh_token": true, "id_token": true, "auth": true,
	"authorization": true, "key": true, "api_key": true, "apikey": true, "secret": true,
	"client_secret": true, "password": true, "passwd": true, "pwd": true, "sig": true,
	"signature": true, "x-amz-signature": true, "x-amz-credential": true,
	"x-amz-security-token": true, "x-goog-signature": true, "x-goog-credential": true, "code": true,
	"jwt": true,
}

// credentialWords are the last words of a query parameter name whose value is a credential, in
// lower case, beyond credentialNames. A service names its own parameter (`github_token`,
// `oauth_token`, `aws_secret`, `db_password`, `private_key`), and no list of whole names keeps up
// with every service's, so a name whose last word is one of these is a credential's too. The word
// must stand alone after a "_" or a "-" (credentialName): `monkey` and `pkey` end in "key", and
// name no credential.
var credentialWords = []string{"token", "secret", "password", "signature", "credential", "key"}

// credentialName reports whether name, a query parameter's name as written, is a credential's:
// one of credentialNames, or one of credentialWords alone or after a "_" or a "-", in any case,
// read after its percent-escapes as a server reads it.
func credentialName(name string) bool {
	if strings.Contains(name, "%") {
		if unescaped, err := url.QueryUnescape(name); err == nil {
			name = unescaped
		}
	}
	name = strings.ToLower(name)
	if credentialNames[name] {
		return true
	}
	for _, word := range credentialWords {
		if name == word || strings.HasSuffix(name, "_"+word) || strings.HasSuffix(name, "-"+word) {
			return true
		}
	}
	return false
}

// fileSchemeEndsAt reports whether the scheme that ends at sep, the index of a "://" in text, is
// `file`, in any case.
func fileSchemeEndsAt(text string, sep int) bool {
	const file = "file"
	if sep < len(file) || !strings.EqualFold(text[sep-len(file):sep], file) {
		return false
	}
	if sep == len(file) {
		return true
	}
	switch c := text[sep-len(file)-1]; {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '+', c == '-', c == '.':
		return false
	}
	return true
}

// nextByte is the index of the next c in a text at or after the position it was last looked for
// from, or the text's length when there is none.
type nextByte struct {
	c  byte
	at int
}

// from is the index of the next c in text at or after i, looked for again only when the one kept
// is behind i. It is right only when i is no smaller than every position asked before it.
func (n *nextByte) from(text string, i int) int {
	if n.at < i {
		n.at = i + orLen(strings.IndexByte(text[i:], n.c), len(text)-i)
	}
	return n.at
}

// schemeEndsAt reports whether a scheme ends at sep, the index of a "://" in text: the run of
// letters, digits, "+", "-" and "." before it holds a letter, where a scheme can start.
func schemeEndsAt(text string, sep int) bool {
	letter := false
	for i := sep - 1; i >= 0; i-- {
		switch c := text[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
			letter = true
		case '0' <= c && c <= '9', c == '+', c == '-', c == '.':
		default:
			return letter
		}
	}
	return letter
}

// endsURL reports whether r ends a URL: a white-space character, `"`, `'`, "`", "<" or ">". A rune
// below utf8.RuneSelf is the byte EndsURLByte reads: of those runes, unicode.IsSpace holds for the
// ASCII white space EndsURLByte names and for no other.
func endsURL(r rune) bool {
	if r < utf8.RuneSelf {
		return EndsURLByte(byte(r))
	}
	return unicode.IsSpace(r)
}

// EndsURLByte reports whether c is a byte that ends a URL for URLCredentials: an ASCII white-space
// character, `"`, `'`, "`", "<" or ">". None of them is part of a longer UTF-8 sequence, so a cut
// just after one never splits a rune.
func EndsURLByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r', '"', '\'', '`', '<', '>':
		return true
	}
	return false
}

// orLen is i, or n when i is -1 (not found).
func orLen(i, n int) int {
	if i < 0 {
		return n
	}
	return i
}

// HandlerOptions is a copy of opts, or of the zero options when opts is nil, whose ReplaceAttr
// gives the text of every attribute through URLCredentials: a string value, an error value (its
// Error()) and a fmt.Stringer value (its String()), the record's message among them. An error or a
// Stringer comes out as a string attribute of what the rule leaves. Every other value (a number, a
// time, a bool, any other type) stays as it is. The caller's own ReplaceAttr, when opts has one,
// runs first, and the rule reads what it returns. A handler calls ReplaceAttr for each attribute
// inside a group and for each attribute a Logger.With carries too, so those take the rule as well.
func HandlerOptions(opts *slog.HandlerOptions) *slog.HandlerOptions {
	var o slog.HandlerOptions
	if opts != nil {
		o = *opts
	}
	own := o.ReplaceAttr
	o.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if own != nil {
			a = own(groups, a)
			// The caller's function may return an unresolved value, which the handler resolves
			// after this function: the rule reads the value the handler writes.
			a.Value = a.Value.Resolve()
		}
		switch a.Value.Kind() {
		case slog.KindString:
			a.Value = slog.StringValue(URLCredentials(a.Value.String()))
		case slog.KindAny:
			if text, ok := textOf(a.Value.Any()); ok {
				a.Value = slog.StringValue(URLCredentials(text))
			}
		}
		return a
	}
	return &o
}

// textOf is the text of an error or a fmt.Stringer. A method that panics (a nil pointer's,
// mostly) gives none, and the value is left to the handler, which writes such a value under its
// own recovery, as it does with no ReplaceAttr: ReplaceAttr runs outside that recovery, and a
// panic there would end the caller's log call.
func textOf(v any) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	switch v := v.(type) {
	case error:
		return v.Error(), true
	case fmt.Stringer:
		return v.String(), true
	}
	return "", false
}
