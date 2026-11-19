// Package redact removes the credentials a URL can carry from text the daemon did not write: a
// far side's error, an HTTP client's error, a log line. Such text can hold a URL with a token in
// its query or in its userinfo (a redirect to a signed download, a DSN, a remote with a token as
// its user name), and the daemon carries it on into a claim, the state, the store and the log.
//
// The rule is for error text only. Text that a person or an agent wrote (a handoff summary, a
// review body, an ask's note) keeps its links whole, query included (`?plain=1#L10`, `?pr=M`), so
// the store's own rule (internal/store/storetext.Text) does not apply this one.
package redact

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// URLCredentials is text with the query and the userinfo of every URL written in it removed.
//
// A URL starts at <scheme>://, where the scheme is a letter followed by letters, digits, "+", "-"
// or ".". It ends at the first white-space character, `"`, `'`, "`", "<" or ">" after the "://",
// or at the end of the text, so a URL that an error quotes (Go's %q, as a *url.Error writes its
// URL) or writes inside <…> ends where the quote does. In the URL:
//
//   - the authority runs from after the "://" up to the first "/", "?" or "#". Everything in it up
//     to and including its last "@" is the userinfo, and it is removed: a password and a token
//     written as the user name alike;
//   - the fragment starts at the first "#" after the authority, and it stays. A "?" after that "#"
//     is the fragment's;
//   - the query runs from the first "?" after the authority up to the fragment, or up to the end
//     of the URL, and it is removed with its "?".
//
// A URL written in the path or the fragment of another (a redirect's target, an archived page)
// is a URL of its own, under the same rule. One written in a query goes with that query. Nothing
// else changes: the scheme, the host and its port, the path, the fragment and every word around
// the URLs stay as written. The rule removes rather than masks, so neither a part nor the length of
// a credential is left, and URLCredentials(URLCredentials(s)) == URLCredentials(s).
//
// A text the rule does not change, a text with no "://" above all, is returned as it is, with no
// allocation.
func URLCredentials(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	var out strings.Builder
	// text[:kept] is written to out or removed. Every removal starts after a "://", so kept is 0
	// until the first one.
	kept := 0
	remove := func(from, to int) {
		if kept == 0 {
			out.Grow(len(text))
		}
		out.WriteString(text[kept:from])
		kept = to
	}
	// A URL's query is removed once the search for the next "://" passes its start: a URL in the
	// path before it has the same query, and a userinfo of that URL comes first in the text.
	query, fragment := -1, -1
	for at := 0; ; {
		i := strings.Index(text[at:], "://")
		if query >= 0 && (i < 0 || at+i >= query) {
			remove(query, fragment)
			at, query = fragment, -1
			continue
		}
		if i < 0 {
			break
		}
		sep := at + i
		at = sep + len("://")
		if !schemeEndsAt(text, sep) {
			continue
		}
		end := at + orLen(strings.IndexFunc(text[at:], endsURL), len(text)-at)
		host := at + orLen(strings.IndexAny(text[at:end], "/?#"), end-at)
		if u := strings.LastIndexByte(text[at:host], '@'); u >= 0 {
			remove(at, at+u+1)
		}
		frag := host + orLen(strings.IndexByte(text[host:end], '#'), end-host)
		if q := strings.IndexByte(text[host:frag], '?'); q >= 0 {
			query, fragment = host+q, frag
		}
	}
	if kept == 0 {
		return text
	}
	out.WriteString(text[kept:])
	return out.String()
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

// endsURL reports whether r ends a URL: a white-space character, `"`, `'`, "`", "<" or ">".
func endsURL(r rune) bool {
	switch r {
	case '"', '\'', '`', '<', '>':
		return true
	}
	return unicode.IsSpace(r)
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
