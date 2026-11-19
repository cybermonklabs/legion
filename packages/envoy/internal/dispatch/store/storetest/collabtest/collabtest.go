// Package collabtest is the xid window rigs' structural check on Dispatch's collaborators: the
// services, stores and HTTP peers Dispatch calls outside its own Postgres. A call to one of them
// made inside a transaction's xid window - once the transaction has sent its first write batch
// (store.SentWriteBatch) and until it ends - holds that transaction id, and the pooled connection
// under it, for as long as the call takes, which starves the pool and stretches the window
// whatever the machine's speed. Each rig wraps the collaborators it hands the product in
// decorators that, at every method that takes a context, fail the test (Check) when the context
// carries that mark, then call through. A plain run checks it; the gap judgment, which reads the
// clock, runs only in the timing lane (storetest.XidJudgesGaps).
//
// The decorators sit beside the rigs that hand the collaborators: the api package's
// (api/collaborators_check_test.go: docs.API, identity, the embedder, the file store, the agent
// stream and publisher, the answerer and the token verifier) and the docs package's
// (docs/collaborators_check_test.go: the document store and identity). The docs and api
// interfaces cannot be wrapped here without an import cycle for those packages' own tests, and
// this package imports no collaborator's package, so a rig's test binary reads none it does not
// already read. What they share is here: Check, and the HTTP client for the Envoy listener and
// GitHub App clients.
package collabtest

import (
	"context"
	"net/http"
	"runtime/debug"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Check fails t, naming the collaborator, the method and the caller's stack, when ctx's
// transaction is past its first write batch (store.SentWriteBatch), and returns either way: the
// decorator calls through, so the row goes on to whatever else it would find. A call in the read
// phase, before that batch, holds no transaction id yet and is not a window's. It uses t.Errorf,
// which is safe from the goroutine a collaborator call runs on.
func Check(t testing.TB, ctx context.Context, collaborator, method string) {
	if !store.SentWriteBatch(ctx) {
		return
	}
	t.Errorf("%s.%s was called while its caller holds a connection of the pool (store.HoldsPooledConnection) "+
		"and its transaction is past its first write batch (store.SentWriteBatch): a collaborator call made then "+
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
