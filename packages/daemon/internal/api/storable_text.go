package api

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/dispatchbody"
)

// PostgreSQL's text and jsonb cannot hold U+0000, so a statement given such text fails, with an
// error that names nothing the caller could fix. The decoder of every request body (decodeJSON)
// refuses such text before the route reads it, naming where the caller wrote the character. A
// member a route decodes into a Go string holds no other text PostgreSQL refuses, since decoding
// writes each byte that is not UTF-8, and each lone surrogate escape, as U+FFFD. A json.RawMessage
// or []byte member keeps the bytes or the escapes as sent, and no body a route of this package
// decodes has one: a route that adds one, to store it, holds it to UTF-8 and to no lone surrogate
// itself.

// unstorableJSON refuses the first string of the JSON value body holds - a member name or a
// value, read in the order the body writes them, a repeated member's every copy among them - that
// holds U+0000, naming it by its place in the body: planReason, tasks[0].text, or "body" for a
// bare string. The walk is Dispatch's own (dispatchbody.NulInJSON), so the daemon and Dispatch
// read a body's strings one way. It names the place and never the string, since a body can carry a
// secret (refuseBadBody). The body decoded already (decodeJSON), so the walk reads it whole.
func unstorableJSON(body []byte) error {
	place, value, found := dispatchbody.NulInJSON(body)
	if !found {
		return nil
	}
	return unstorableText(place, value)
}

// unstorableText refuses value when it holds U+0000, naming place and where the first one
// stands, counted from 1 in code points as the daemon's caps count (steprecord.RatingReasonMax).
func unstorableText(place, value string) error {
	at := strings.IndexByte(value, 0)
	if at < 0 {
		return nil
	}
	return fmt.Errorf("%s holds a NUL character (U+0000) at character %d, which the daemon cannot store", place, utf8.RuneCountInString(value[:at])+1)
}
