// Package oauth2call is the library boundary of internal/redact (OAuth2, Scope, TokenSource and
// RefusalClient) for a module outside this one, which cannot import an internal package: the
// daemon's launcher and session proxy, say. Every call of golang.org/x/oauth2, of a subpackage of
// it, of github.com/coreos/go-oidc, google.golang.org/api/impersonate or
// cloud.google.com/go/cloudsqlconn that can answer an error is written oauth2call.Call(call(...))
// there, as it is redact.OAuth2(call(...)) here, and a call of a library that hands back only the
// text of its error is read under a scope: s := oauth2call.Scope[T](ctx), then
// oauth2call.Call(s.Read(call(s.Context(), ...))). The boundary census
// (internal/redact/testdata/wholeuse/boundary) holds both. Every HTTP client such a module builds
// or hands a library is oauth2call.RefusalClient's, or a scope's (s.Client), as it is
// redact.RefusalClient's here. Every answer such a module reads is read through ReadAnswer or
// StreamAnswer, as it is through redact's here: the answer census (cmd/redact-gate) holds it. A
// request such a module sends that got no answer prints through Unanswered, the one printer of it
// both modules share.
package oauth2call

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
)

// Call is redact.OAuth2: the value, and the error read with the record of the requests it came of,
// every answer x/oauth2 returned in it held as the rule reads its words, and an error no record
// covers withheld but for its class and status.
func Call[T any](value T, err error) (T, error) { return redact.OAuth2(value, err) }

// Scope is redact.Scope: the record of one call into a library that hands back only the text of
// its error.
func Scope[T any](ctx context.Context) *redact.CallScope[T] { return redact.Scope[T](ctx) }

// TokenSource is redact.TokenSource: source behind the boundary, for a token source handed to a
// function or a struct field of another module, which can ask it for a token and print the refusal
// it answers.
func TokenSource(source oauth2.TokenSource) *redact.BoundTokenSource {
	return redact.TokenSource(source)
}

// RefusalClient is redact.RefusalClient: base, whose every request is recorded, its record
// travelling with what the request led to, for every HTTP client a module outside this one builds
// or hands a library.
func RefusalClient(base *http.Client) *http.Client { return redact.RefusalClient(base) }

// Answer is redact.Answer: an HTTP answer's body, which decodes into typed values with the
// request's record, prints only as the rule reads it, and streams to a writer.
type Answer = redact.Answer

// RemoteText is redact.RemoteText: another party's words, which a module outside this one decodes
// an answer's member into (Answer.Decode) and prints only as the rule reads them.
type RemoteText = redact.RemoteText

// GitHubRefusal is the JSON GitHub refuses a request with, as a module outside this one decodes
// it (Answer.Decode): its message is GitHub's words, or a stand-in's, and prints only as the rule
// reads them, with the credentials the request carried redacted. It is declared here, beside the
// box, so its print test is this module's (holders_print_test.go).
type GitHubRefusal struct {
	Message RemoteText `json:"message"`
}

// CodedRefusal is the {"code","error"} JSON a service of this repository refuses a request with -
// Dispatch's, the secrets broker's, the Legion daemon's - as a module outside this one decodes it
// (Answer.Decode): a caller decides on the code as it came (RemoteText.Equal, IsEmpty), never on
// the rule's reading of it, which a value the request carried can cut, and prints the code and the
// error as the rule reads them, each word another party's, or a stand-in's, with the credentials
// the request carried redacted. Decoded field by field, a refusal whose error names a URL beside
// the JSON quote that ends it keeps its code, where the rule's reading of the whole body is not
// readable. It is declared here, beside the box, so its print test is this module's
// (holders_print_test.go).
type CodedRefusal struct {
	Code  RemoteText `json:"code"`
	Error RemoteText `json:"error"`
}

// ErrAnswerTooLarge is redact.ErrAnswerTooLarge.
var ErrAnswerTooLarge = redact.ErrAnswerTooLarge

// ReadAnswer is redact.ReadAnswer: resp's body, at most limit bytes, as an Answer.
func ReadAnswer(resp *http.Response, limit int64) (Answer, error) {
	return redact.ReadAnswer(resp, limit)
}

// StreamAnswer is redact.StreamAnswer: resp's body as a stream of data of at most limit bytes.
func StreamAnswer(resp *http.Response, limit int64) io.ReadCloser {
	return redact.StreamAnswer(resp, limit)
}

// Holds is redact.Holds: a test of an answer's body as it came, read in place and kept nowhere,
// for a decision made on what the answer says before anything of it prints.
func Holds(a Answer, test func(body []byte) bool) bool { return redact.Holds(a, test) }

// Echoes is redact.Echoes: whether text, words read from a's bytes that a caller hands on where no
// print of the rule reads them, holds a run of a credential a's requests carried.
func Echoes(a Answer, text string) bool { return redact.Echoes(a, text) }

// ContainsFold is redact.ContainsFold: whether body holds phrase, lower-case ASCII, in whatever
// case it is written.
func ContainsFold(body []byte, phrase string) bool { return redact.ContainsFold(body, phrase) }

// DecodeError is redact.DecodeError: err, encoding/json's error decoding data into the value into
// points at, as fixed text that names the type, the member and the JSON kind and quotes no value,
// unwrapping to nothing, for a decode of bytes no answer's Decode reads whose error prints.
func DecodeError(err error, data []byte, into any) error { return redact.DecodeError(err, data, into) }

// Unmarshal is redact.Unmarshal: a's body decoded by encoding/json as the answer's data, for a
// destination Answer.Decode refuses (a json.RawMessage), its error DecodeError's fixed text.
// Nothing it fills holds the request's record, so nothing it fills may print.
func Unmarshal(a Answer, into any) error { return redact.Unmarshal(a, into) }

// UnmarshalData is redact.UnmarshalData: data decoded by encoding/json, for bytes no answer's
// Decode reads (a message a stream carries, a member an answer carried as data), its error
// DecodeError's fixed text, which quotes no value.
func UnmarshalData(data []byte, into any) error { return redact.UnmarshalData(data, into) }

// Unanswered is redact.Unanswered: err, the failure of an HTTP request that got no answer
// (http.Client.Do's error, or http.NewRequest's with request nil), as a message may print it:
// net/http's operation, the request named shown or, where shown is "", by the URL it asked as
// redact.Unanswered names it, and the failure by its transport text, a TLS record or certificate
// failure's fixed words, or its class, never net/http's or x509's text of what the answering party
// sent. shown is a caller's own words for the call, never a name of its URL another rule made. It
// unwraps to err, so errors.Is and errors.As still find the failure.
func Unanswered(err error, request *http.Request, shown string) error {
	return redact.Unanswered(err, request, shown)
}

// AskedURL is redact.AskedURL: u, the URL a request asked, as Unanswered names it (its scheme,
// host and path, never its user information, query or fragment, and redact.NotPrinted where the
// URL rule refuses what remains), for a line about a request that got an answer, so the URL has
// one name wherever it prints.
func AskedURL(u *url.URL) string { return redact.AskedURL(u) }

// StatusLine is redact.StatusLine: an answer's status line as a message may print it, the code and
// its standard reason phrase, never the phrase the answering party wrote (Response.Status).
func StatusLine(code int) string { return redact.StatusLine(code) }

// Header is redact.Header: the first value of an answer's header as another party's words, held
// with the record of the requests that led to the answer, printed only as the rule reads it.
func Header(resp *http.Response, name string) RemoteText { return redact.Header(resp, name) }

// StreamedWords is redact.StreamedWords: text read out of an answer's body streamed as data (a tar
// entry's name) as another party's words, held with the record of the requests that led to the
// answer, printed only as the rule reads it.
func StreamedWords(resp *http.Response, text string) RemoteText {
	return redact.StreamedWords(resp, text)
}

// TokenPiece is redact.TokenPiece: the shortest run of a credential's characters Tokens redacts
// wherever it stands.
const TokenPiece = redact.TokenPiece

// Tokens is redact.Tokens: text with each run of at least TokenPiece characters of any of tokens
// redacted, for text another party wrote that can echo a credential this process sent it.
func Tokens(text string, limit int, tokens ...string) string {
	return redact.Tokens(text, limit, tokens...)
}

// Field is redact.Field: a remote field as a message may print it, the rule's reading on one line,
// or redact.WordsNotReadable where the rule cannot read it. The rule fails closed.
func Field(field string) string { return redact.Field(field) }
