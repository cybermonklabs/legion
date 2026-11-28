package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ErrNoViewer is the request pool refusing a statement, before any SQL, under a context that
// carries neither a viewer (WithViewer) nor the system-work mark (WithSystemWork), and
// RequireViewer refusing a context with no viewer whatever its mark: a request path that lost its
// viewer fails loudly rather than reading no row, or reading as the owner (plan D2, D3).
var ErrNoViewer = errors.New("store: the request pool runs a statement only under a viewer (store.WithViewer) or, for the server's own work, the system-work mark (store.WithSystemWork), and this context carries neither")

// ErrRequestPoolRefuses is the request pool refusing a method that would send a statement outside
// the viewer's settings: a connection it hands out (Acquire, AcquireFunc, AcquireAllIdle,
// Listener, Dedicated), a copy (CopyFrom) and every side pool (plan D1, D2).
var ErrRequestPoolRefuses = errors.New("store: the request pool hands out no connection, copy or side pool, since a statement sent on one would run outside the viewer's settings; take it from the system pool")

// errRequestPoolNotBuilt is what each part of the request pool answers until INFRA-6168's work
// step 2 builds it.
var errRequestPoolNotBuilt = errors.New("store: the request pool and the viewer are not built yet (INFRA-6168 work step 2)")

// viewerKey is the context key WithViewer stores the viewer under.
type viewerKey struct{}

// systemWorkKey is the context key WithSystemWork marks the server's own work with.
type systemWorkKey struct{}

// WithViewer returns ctx carrying viewer, as access.Resolve answers it for the request's caller:
// each statement the request pool runs under ctx runs as dispatch_app with the viewer's settings
// (plan D2).
func WithViewer(ctx context.Context, viewer model.Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, viewer)
}

// ViewerFrom is the viewer ctx carries (WithViewer), and false when it carries none.
func ViewerFrom(ctx context.Context) (model.Viewer, bool) {
	viewer, ok := ctx.Value(viewerKey{}).(model.Viewer)
	return viewer, ok
}

// WithSystemWork marks ctx as the server's own work: under it, with no viewer, the request store's
// pool runs a statement on the system pool it holds; a viewer wins over the mark (plan D3).
func WithSystemWork(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemWorkKey{}, true)
}

// RequireViewer refuses (ErrNoViewer) a context that carries no viewer, whatever its mark: a read
// that must run under a viewer on a lifetime context, which the system-work mark would otherwise
// send to the owner (plan D3).
func RequireViewer(ctx context.Context) error {
	// INFRA-6168 scaffold: work step 2
	return errRequestPoolNotBuilt
}

// AsOwner runs fn on tx as the table owner where tx runs as dispatch_app, and puts dispatch_app
// back before it returns, also when fn fails; on a transaction of any other role it runs fn and
// changes no role (plan D5).
func AsOwner(ctx context.Context, tx pgx.Tx, fn func(tx pgx.Tx) error) error {
	// INFRA-6168 scaffold: work step 2
	return errRequestPoolNotBuilt
}
