package redact

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// OAuth2 is the one way this repository calls golang.org/x/oauth2, the subpackages of it it
// imports, github.com/coreos/go-oidc, google.golang.org/api/impersonate and
// cloud.google.com/go/cloudsqlconn: every call of theirs that can answer an error is written
// redact.OAuth2(call(...)), or redact.OAuth2(struct{}{}, call(...)) for a call that answers an
// error alone, and a call of a library that hands back only the text of its error (CallScope) is
// read under a scope, redact.OAuth2(s.Read(call(s.Context(), ...))); the census in
// testdata/wholeuse/boundary, which reads the libraries' own exported API, fails on any call
// written otherwise. The value comes back as it is.
//
// The error is read with the record of the requests it came of, which travels with it and is
// never matched by text (RefusalClient): the record an *oauth2.RetrieveError's answer carries (or,
// for an answer no RefusalClient carried one on, the requests net/http left on it), the record a
// failed hop's error carries, and that of the scope the call ran under. Every word of the error is
// read with every credential those requests carried redacted (Tokens), then by the rule. An OAuth
// error answer x/oauth2 returns becomes an *OAuthAnswer before the caller sees it, under a
// *url.Error that holds it as well, whose operation and URL the rule reads; an error that holds one
// under any other wrapper becomes the answer alone, since that wrapper's words can quote it. Every
// other error becomes a *LibraryError, which prints the rule's reading of its text and answers
// errors.Is and errors.As as it does.
//
// It fails closed: an error that reaches it with no record at all, under no scope, with no failed
// hop and no answer that holds a request, prints its class and its status alone (withheld), never
// a word of its text, since nothing says which credentials its words can echo.
func OAuth2[T any](value T, err error) (T, error) {
	return value, read(err, nil, false)
}

// read is err as the boundary hands it on, read with scoped, the records of the scope the call
// ran under, where it ran under one (inScope).
func read(err error, scoped []*record, inScope bool) error {
	switch err.(type) {
	case nil:
		return nil
	case *OAuthAnswer, *LibraryError:
		return err
	}
	carried, recorded := carriedBy(err, scoped, inScope)
	return answerOf(err, carried, recorded)
}

// carriedBy is every credential the requests err came of carried, as their records say: the
// scope's, each failed hop's in err (hopError), and that of each answer x/oauth2 made an error of
// in it (answerRecord); recorded reports whether there is any record at all.
func carriedBy(err error, scoped []*record, inScope bool) (carried []string, recorded bool) {
	recorded = inScope
	for _, rec := range scoped {
		carried = append(carried, rec.tokens()...)
	}
	eachError(err, func(e error) {
		var rec *record
		switch e := e.(type) {
		case *hopError:
			rec = e.rec
		case *oauth2.RetrieveError:
			rec = answerRecord(e.Response)
		}
		if rec != nil {
			carried, recorded = append(carried, rec.tokens()...), true
		}
	})
	return carried, recorded
}

// eachError calls visit with err and every error under it, down each Unwrap.
func eachError(err error, visit func(error)) {
	for depth := 0; err != nil && depth < 64; depth++ {
		visit(err)
		switch e := err.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range e.Unwrap() {
				eachError(inner, visit)
			}
			return
		case interface{ Unwrap() error }:
			err = e.Unwrap()
		default:
			return
		}
	}
}

// answerOf is err as OAuth2 hands it on, every word read with carried redacted, or withheld where
// no record covers it (recorded false).
func answerOf(err error, carried []string, recorded bool) error {
	switch e := err.(type) {
	case *OAuthAnswer, *LibraryError:
		return err
	case *oauth2.RetrieveError:
		return newOAuthAnswer(e, carried, recorded)
	}
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return newLibraryError(err, carried, recorded)
	}
	if ue, ok := err.(*url.Error); ok {
		if inner := answerOf(ue.Err, carried, recorded); inner != ue.Err {
			// Op and URL are the request's, whose URL a redirect the answering party named can
			// set: the rule reads both, the URL with the record's credentials redacted first, and
			// no URL is kept where no record covers it.
			held := ""
			if recorded {
				held = ruled(Tokens(ue.URL, -1, carried...))
			}
			return &url.Error{Op: ruled(ue.Op), URL: held, Err: inner}
		}
		return err
	}
	var held *OAuthAnswer
	if errors.As(err, &held) {
		return held
	}
	return newOAuthAnswer(re, carried, recorded)
}

// LibraryError is an error a library call across OAuth2 answered that holds no OAuth error answer:
// a transport's failure, an ended context, go-oidc's discovery error, which quotes the answer's
// status line and body (`<status>: <body>`), the external-account exchange's (`oauth2/google:
// status code <n>: <body>`), impersonate's, googleapi's, a verifier's, which quotes a token's
// claims. Its Error is the rule's reading of the error's text with every credential its record
// says the requests carried redacted first (OAuth2), or, where no record covers it, the error's
// class alone (withheld). It answers errors.Is and errors.As as the error does, through Is and As,
// and has no Unwrap, so errors.Unwrap cannot hand back the error and the text it prints. It holds
// the error behind a pointer to the interface, which fmt prints as an address under every verb at
// every depth, as RemoteText's *string: a plain error field, or a pointer to a struct holding one,
// lets fmt's reflection spell out a library error of a value type, its text and all.
type LibraryError struct {
	text RemoteText
	err  *error
}

// newLibraryError is err held as a LibraryError: its text with carried redacted, or its class
// alone where no record covers it.
func newLibraryError(err error, carried []string, recorded bool) *LibraryError {
	text := withheld(err)
	if recorded {
		text = Tokens(err.Error(), -1, carried...)
	}
	return &LibraryError{text: NewRemoteText(text), err: &err}
}

// withheld is what the boundary prints of an error no record covers: its class, the Go type of
// the error and of each it wraps, and the status of an OAuth error answer in it, each a closed
// value, and never a word of its text.
func withheld(err error) string {
	var classes []string
	for e := err; e != nil && len(classes) < 8; e = errors.Unwrap(e) {
		classes = append(classes, fmt.Sprintf("%T", e))
	}
	text := strings.Join(classes, " over ") + " (message withheld: no record of its requests)"
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		text += ", status " + statusLine(re.Response.StatusCode)
	}
	return text
}

// Error is the rule's reading of the library's error.
func (e *LibraryError) Error() string { return e.text.String() }

// Is is errors.Is of the library's error.
func (e *LibraryError) Is(target error) bool { return errors.Is(*e.err, target) }

// As is errors.As of the library's error.
func (e *LibraryError) As(target any) bool { return errors.As(*e.err, target) }

// BoundTokenSource is an oauth2.TokenSource as this repository hands it to another library - a
// Secret Manager client, an http.Client oauth2.NewClient builds, a google.Credentials, the Cloud
// SQL connector: its Token is the source's Token read by the boundary, so a refusal that library
// prints in an error of its own holds only the rule's reading of the answer's words. A source a
// scoped call built (CallScope.Read) is bound to that call's scope: each Token reads its error
// with a fresh record of that Token's own requests, one Token at a time. One TokenSource makes is
// read with a record of no request, for a source of this repository's own, whose requests go
// through no client of a library's. The census fails on a token source handed to a function or a
// struct field of another module that is not one.
type BoundTokenSource struct {
	source oauth2.TokenSource
	scope  *scope // the scope the source's library sends through, or nil
	mu     sync.Mutex
}

// TokenSource is source behind the boundary; a source that already is one comes back as it is.
func TokenSource(source oauth2.TokenSource) *BoundTokenSource {
	if bound, ok := source.(*BoundTokenSource); ok {
		return bound
	}
	return &BoundTokenSource{source: source}
}

// Token is the source's Token, its error read by the boundary with the record of its requests.
func (s *BoundTokenSource) Token() (*oauth2.Token, error) {
	if s.scope == nil {
		token, err := s.source.Token()
		return token, read(err, nil, true)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scope.reopen()
	token, err := s.source.Token()
	return token, read(err, s.scope.take(), true)
}

// OAuthAnswer is an OAuth error answer x/oauth2 returned, as OAuth2 holds it once it has crossed
// the boundary. Its code and description are RemoteText, read only through RefusalCode and
// Refusal, which split the device authorization endpoint's "<CODE>: <message>" before either part
// is held (NewOAuthRefusal). It unwraps to an *oauth2.RetrieveError every field of which is a
// closed value or the rule's reading: the rule's reading of the code, the description and the
// error_uri, the body as a message may print it (Field), the answer's status code with its
// standard reason phrase (statusLine) and its Retry-After as the number or date it parses as
// (retryAfter), and no other header and no request, so errors.As, the status and Retry-After
// reads, and this package's printing read it as the answer it is, and no reader reaches the
// words. The status line the answering party wrote is not kept: x/oauth2's text of an answer with
// no error code prints it. Every word is read with every credential the answer's record says its
// requests carried redacted first (Tokens): something in the token endpoint's place, a gateway or
// a proxy, can answer with a page of its own that echoes the request back, its headers and its
// form, and the rule reads only the URLs in a field. An answer no record covers keeps no word at
// all: its code, description, error_uri and body are empty, so its Error is x/oauth2's text of its
// status alone, while RefusalCode still reads the code as it came. Its Error is x/oauth2's text of
// that reading.
type OAuthAnswer struct {
	code           RemoteText
	refusalCode    RemoteText
	refusalMessage RemoteText
	answer         *oauth2.RetrieveError
}

// newOAuthAnswer is re as OAuth2 holds it. Every word of the answer it prints, its code,
// description, error_uri and body, is read with carried redacted first (Tokens), then by the rule;
// the closed code (RefusalCode) is read from the code as it came, which prints nothing, so a
// request value that shares a run with a code cannot turn the code into one this client does not
// name. Where no record covers re (recorded false), no word is kept.
func newOAuthAnswer(re *oauth2.RetrieveError, carried []string, recorded bool) *OAuthAnswer {
	answer := &oauth2.RetrieveError{Response: &http.Response{}}
	if r := re.Response; r != nil {
		answer.Response = &http.Response{Status: statusLine(r.StatusCode), StatusCode: r.StatusCode, Header: retryAfter(r.Header)}
	}
	if !recorded {
		return &OAuthAnswer{code: NewRemoteText(re.ErrorCode), answer: answer}
	}
	refusalCode, refusalMessage := NewOAuthRefusal(re.ErrorCode, re.ErrorDescription, carried...)
	answer.ErrorCode = ruled(Tokens(re.ErrorCode, -1, carried...))
	answer.ErrorDescription = ruled(Tokens(re.ErrorDescription, -1, carried...))
	answer.ErrorURI = ruled(Tokens(re.ErrorURI, -1, carried...))
	if len(re.Body) > 0 {
		// An HTML page can write a URL's separators as character references, which a reader of
		// the page decodes: the rule reads the body as that reader does (UnescapeHTML).
		answer.Body = []byte(Field(Tokens(UnescapeHTML(string(re.Body)), -1, carried...)))
	}
	return &OAuthAnswer{code: NewRemoteText(re.ErrorCode), refusalCode: refusalCode, refusalMessage: refusalMessage, answer: answer}
}

// statusLine is the status line of an answer with status code: the code and its standard reason
// phrase (http.StatusText), never the phrase the answering party wrote, which can quote a
// credential's URL and which x/oauth2 prints for an answer with no error code.
func statusLine(code int) string {
	return strings.TrimSpace(strconv.Itoa(code) + " " + http.StatusText(code))
}

// retryAfter is h's Retry-After, the one header of a refusal this repository reads (the helper's
// wait after a 429, serverwait.Read), rebuilt from what it parses as, as serverwait reads it: a
// count of seconds, one past an int64 as the largest or smallest, or an HTTP date. A header with
// no Retry-After, or one that is neither, is nil.
func retryAfter(h http.Header) http.Header {
	value := h.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		return http.Header{"Retry-After": {strconv.FormatInt(seconds, 10)}}
	}
	if at, err := http.ParseTime(value); err == nil {
		return http.Header{"Retry-After": {at.UTC().Format(http.TimeFormat)}}
	}
	return nil
}

// Error is x/oauth2's text of the rule's reading of the answer.
func (a *OAuthAnswer) Error() string { return a.answer.Error() }

// Unwrap is the rule's reading of the answer as x/oauth2's own type.
func (a *OAuthAnswer) Unwrap() error { return a.answer }

// RefusalCode is the answer's code read as the closed OAuthCode, from the code as it came.
func (a *OAuthAnswer) RefusalCode() OAuthCode { return a.code.OAuthCode() }

// Refusal is the answer's code and description as NewOAuthRefusal splits them.
func (a *OAuthAnswer) Refusal() (code, message RemoteText) { return a.refusalCode, a.refusalMessage }
