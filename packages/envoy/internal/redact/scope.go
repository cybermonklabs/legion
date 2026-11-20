package redact

import (
	"context"
	"net/http"
	"slices"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// CallScope is the record of one call into a library that hands back only the text of its error,
// with no answer or request left in it to read a record from: github.com/coreos/go-oidc,
// golang.org/x/oauth2/google and its external_account exchange, google.golang.org/api/impersonate,
// and cloud.google.com/go/cloudsqlconn, whose Admin API errors are googleapi's text. Such a call is
// written
//
//	s := redact.Scope[T](ctx)
//	v, err := redact.OAuth2(s.Read(call(s.Context(), ...)))
//
// or redact.OAuth2(struct{}{}, s.Err(call(...))) for a call that answers an error alone, and the
// census in testdata/wholeuse/boundary fails on a call of those libraries written otherwise. Every
// request the call sends through a RefusalClient under s.Context(), or through a client s.Client
// made, writes its answer's record into s; Read and Err then read the call's error with every
// credential those requests carried redacted, and with nothing of any other call's.
//
// A token source the call answers, an oauth2.TokenSource or a *google.Credentials's, captured the
// context or the client it was built with, so its later requests write into s as well: Read hands
// it back bound to s (BoundTokenSource), each Token of which reads its error with a fresh record of
// that Token's requests alone.
type CallScope[T any] struct {
	ctx   context.Context
	scope *scope
}

// Scope opens a record for one call under ctx. T is the call's value, so the method that reads it
// (Read) is typed by it: a Go method cannot take a type parameter of its own.
func Scope[T any](ctx context.Context) *CallScope[T] {
	return &CallScope[T]{ctx: ctx, scope: &scope{open: true}}
}

// Context is ctx carrying the scope, whose x/oauth2 HTTP client (oauth2.HTTPClient, which go-oidc
// and golang.org/x/oauth2/google send with) is a RefusalClient: the one ctx carries, or
// RefusalClient over the client it carries, or over http.DefaultClient where it carries none.
func (s *CallScope[T]) Context() context.Context {
	ctx := context.WithValue(s.ctx, scopeKey{}, s.scope)
	if client, _ := ctx.Value(oauth2.HTTPClient).(*http.Client); !refusalMade(client) {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, RefusalClient(client))
	}
	return ctx
}

// Client is RefusalClient(base) bound to the scope, for a library given a client rather than a
// context (option.WithHTTPClient), which sends with a context of its own.
func (s *CallScope[T]) Client(base *http.Client) *http.Client { return refusalClient(base, s.scope) }

// Read is the call's value, a token source in it bound to the scope (CallScope), and its error as
// the boundary reads it (OAuth2) with the record the scope holds: every credential the call's
// requests carried. It closes the scope; a request that writes into it afterwards is kept
// nowhere.
func (s *CallScope[T]) Read(value T, err error) (T, error) {
	records := s.scope.take()
	if err == nil {
		value = s.bind(value)
	}
	return value, read(err, records, true)
}

// Err is the error of a call that answers an error alone, as Read reads it.
func (s *CallScope[T]) Err(err error) error { return read(err, s.scope.take(), true) }

// bind is value with the token source it holds bound to the scope: an oauth2.TokenSource where T
// can hold a *BoundTokenSource, and a *google.Credentials's TokenSource.
func (s *CallScope[T]) bind(value T) T {
	switch v := any(value).(type) {
	case *google.Credentials:
		if v != nil && v.TokenSource != nil {
			v.TokenSource = &BoundTokenSource{source: v.TokenSource, scope: s.scope}
		}
	case oauth2.TokenSource:
		if bound, ok := any(&BoundTokenSource{source: v, scope: s.scope}).(T); ok {
			return bound
		}
	}
	return value
}

// scope is what a CallScope keeps: each answer's record a RefusalClient wrote into it while it was
// open, in the order written, each once.
type scope struct {
	mu      sync.Mutex
	open    bool
	records []*record
}

// scopeKey is the context key CallScope.Context carries the scope under.
type scopeKey struct{}

// scopeIn is the scope ctx carries, or nil.
func scopeIn(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// add keeps rec while the scope is open.
func (s *scope) add(rec *record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open && !slices.Contains(s.records, rec) {
		s.records = append(s.records, rec)
	}
}

// take closes the scope and answers what it kept.
func (s *scope) take() []*record {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.records
	s.open, s.records = false, nil
	return records
}

// reopen opens the scope again with nothing kept, for one Token of a source bound to it.
func (s *scope) reopen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open, s.records = true, nil
}
