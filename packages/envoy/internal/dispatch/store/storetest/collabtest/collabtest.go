// Package collabtest is the xid window rigs' structural check on Dispatch's collaborators: the
// services, stores and HTTP peers Dispatch calls outside its own Postgres. A call to one of them
// made inside a transaction's xid window - from the transaction's first statement that writes,
// inline or batched, until it ends (store.InXidWindow, which storetest.XidTracer opens) - holds
// that transaction id, and the pooled connection under it, for as long as the call takes, which
// starves the pool and stretches the window whatever the machine's speed. Each rig wraps the
// collaborators it hands the product in decorators that, at every method that takes a context,
// fail the test (Check) when the context carries that mark, then call through. Every run checks
// it, as it checks the document work the tracer sees through docwork; neither reads a clock.
//
// The decorators sit beside the rigs that hand the collaborators: the api package's
// (api/collaborators_check_test.go: docs.API, the embedder, the file store, the agent stream and
// publisher, the answerer and the token verifier) and the docs package's
// (docs/collaborators_check_test.go: the document store). The docs and api interfaces cannot be
// wrapped here without an import cycle for those packages' own tests. The identity both rigs hand
// is wrapped here (CheckedIdentity): identity is the one collaborator's package this package
// imports, so a rig's test binary reads no other collaborator's package through it. What the rigs
// share is here: Check, CheckedIdentity, the HTTP client for the Envoy listener and GitHub App
// clients, and RecordedFailures, the test a control hands Check in place of its own.
package collabtest

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Check fails t, naming the collaborator, the method and the caller's stack, when ctx's
// transaction is inside its xid window (store.InXidWindow), and returns either way: the decorator
// calls through, so the row goes on to whatever else it would find. A call before the
// transaction's first write holds no transaction id yet and is not a window's. It uses t.Errorf,
// which is safe from the goroutine a collaborator call runs on.
func Check(t testing.TB, ctx context.Context, collaborator, method string) {
	if !store.InXidWindow(ctx) {
		return
	}
	t.Errorf("%s.%s was called while its caller holds a connection of the pool (store.HoldsPooledConnection) "+
		"and its transaction has written (store.InXidWindow): a collaborator call made then "+
		"holds that transaction id, and the connection under it, for as long as the call takes\n%s",
		collaborator, method, debug.Stack())
}

// HTTPClient is an HTTP client whose every request is checked before it is sent (Check, with the
// request's own context and its method and URL), for a rig that hands the product an Envoy
// listener's URL or a GitHub App (api.DepsInput.CollaboratorHTTPClient).
func HTTPClient(t testing.TB) *http.Client {
	return &http.Client{Transport: checkedTransport{t: t, next: http.DefaultTransport}}
}

type checkedTransport struct {
	t    testing.TB
	next http.RoundTripper
}

func (c checkedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	Check(c.t, request.Context(), "HTTP "+request.URL.Host, request.Method+" "+request.URL.Path)
	return c.next.RoundTrip(request)
}

// CheckedIdentity is the window rigs' decorator of the identity Dispatch resolves a person with,
// which the api rig hands the product and the docs rig a document service: it checks Login, which
// takes its context from the request it resolves (Check), and calls through. A nil identity
// stays nil, for a rig that hands the product none.
func CheckedIdentity(t testing.TB, next identity.Identity) identity.Identity {
	if next == nil {
		return nil
	}
	return checkedLogin{t: t, next: next}
}

type checkedLogin struct {
	t    testing.TB
	next identity.Identity
}

func (c checkedLogin) Login(r *http.Request) (string, error) {
	Check(c.t, r.Context(), "identity.Identity", "Login")
	return c.next.Login(r)
}

// RecordedFailures is a testing.TB that records what a check reports through Errorf instead of
// failing the test, so a control can assert that the check fires and on what. Everything else -
// Cleanup, Helper, Fatalf - is the test's own: a control makes one as &RecordedFailures{TB: t}.
type RecordedFailures struct {
	testing.TB
	mu       sync.Mutex
	failures []string
}

func (r *RecordedFailures) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// Reported is every failure Errorf recorded so far, in order, as a copy the caller may keep.
func (r *RecordedFailures) Reported() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.failures)
}
