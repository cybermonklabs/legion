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
// redact.RefusalClient's here.
package oauth2call

import (
	"context"
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
