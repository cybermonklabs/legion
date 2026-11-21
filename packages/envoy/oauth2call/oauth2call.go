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
// StreamAnswer, as it is through redact's here: the answer census (cmd/redact-gate) holds it.
package oauth2call

import (
	"context"
	"io"
	"net/http"

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

// RoleLookup is the Envoy listener's answer naming a role's holder as a module outside this one
// decodes it (Answer.Decode) to log or print what it names: a role lookup's (GET /v1/roles/<role>)
// holder on a 200, and on a 404 the reason no live session holds the role and the lapsed holder's
// state, the shapes of listenerwire.RoleHolder and listenerwire.RoleRefusal, and a role publish's
// refusal, whose holder, event id and error a 504 (listenerwire.RoleSendFailure) and a 403
// (listenerwire.CapabilityRefusal, listenerwire.APIError) name. Each is the listener's words, or a
// stand-in's, and prints only as the rule reads them, with the credentials the request carried
// redacted; a caller that tells answers apart compares them (RemoteText.Equal), or decides on the
// listener's own shape decoded from the answer as it came. It is declared here, beside the box, so
// its print test is this module's (holders_print_test.go).
type RoleLookup struct {
	Holder      RemoteText `json:"holder"`
	HolderState RemoteText `json:"holder_state"`
	Reason      RemoteText `json:"reason"`
	EventID     RemoteText `json:"event_id"`
	Error       RemoteText `json:"error"`
	LastSeen    *int64     `json:"last_seen"`
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

// ContainsFold is redact.ContainsFold: whether body holds phrase, lower-case ASCII, in whatever
// case it is written.
func ContainsFold(body []byte, phrase string) bool { return redact.ContainsFold(body, phrase) }

// StatusLine is redact.StatusLine: an answer's status line as a message may print it, the code and
// its standard reason phrase, never the phrase the answering party wrote (Response.Status).
func StatusLine(code int) string { return redact.StatusLine(code) }

// Header is redact.Header: the first value of an answer's header as another party's words, held
// with the record of the requests that led to the answer, printed only as the rule reads it.
func Header(resp *http.Response, name string) RemoteText { return redact.Header(resp, name) }
