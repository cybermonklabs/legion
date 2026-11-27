// Package precommit is the seam a write uses to end its own transaction with statements of its
// own: a settle runs after the transaction's body has returned and before its commit
// (store.TransactWith), on the same transaction, so what it writes commits with the body's writes
// or not at all. It is a package of its own so that internal/record, which internal/store
// imports, can register a settle on the transaction its caller opened.
//
// A settle is registered once per id in a transaction (Once), so every writer of one kind of
// change shares one settle, however many of its writes the transaction makes. A transaction that
// joins an open one (store.TransactWith's join) registers on the open one, so its settles run at
// that transaction's end. A joined write that fails rolls back to its savepoint, and its settles
// go with its rows (Scope.Truncate): none of them runs, and each id it registered first is free
// for a later write of the open transaction to register again.
package precommit

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type scopeKey struct{}

// Scope is one transaction's settles, in the order they were registered, each with the id it was
// registered under.
type Scope struct {
	ids     map[any]struct{}
	settles []entry
}

// entry is one settle of a Scope and the id Once registered it under.
type entry struct {
	id     any
	settle func(context.Context, pgx.Tx) error
}

// Open answers ctx carrying a new, empty Scope, and the Scope, for store.TransactWith to run at
// its transaction's end (Scope.Run).
func Open(ctx context.Context) (context.Context, *Scope) {
	scope := &Scope{ids: map[any]struct{}{}}
	return context.WithValue(ctx, scopeKey{}, scope), scope
}

// Scoped reports whether ctx's transaction runs settles (Open), and whether id is already
// registered on it.
func Scoped(ctx context.Context, id any) (scoped, registered bool) {
	scope, ok := ctx.Value(scopeKey{}).(*Scope)
	if !ok {
		return false, false
	}
	_, registered = scope.ids[id]
	return true, registered
}

// Once registers settle under id on ctx's transaction, unless one is registered under id already.
// It answers false, and registers nothing, for a ctx that runs no settles.
func Once(ctx context.Context, id any, settle func(context.Context, pgx.Tx) error) bool {
	scope, ok := ctx.Value(scopeKey{}).(*Scope)
	if !ok {
		return false
	}
	if _, registered := scope.ids[id]; !registered {
		scope.ids[id] = struct{}{}
		scope.settles = append(scope.settles, entry{id: id, settle: settle})
	}
	return true
}

// Len is the number of settles registered on s so far: the mark a joined write takes before it
// runs, to cut its own settles back off should it fail (Truncate).
func (s *Scope) Len() int { return len(s.settles) }

// Truncate drops every settle registered on s after the first n (Len), and frees the id each was
// registered under, so s runs none of them and a later Once under one of those ids registers its
// settle. The settles registered before the mark, and their ids, stay as they were.
func (s *Scope) Truncate(n int) {
	for _, dropped := range s.settles[n:] {
		delete(s.ids, dropped.id)
	}
	s.settles = s.settles[:n]
}

// Run runs each settle in the order it was registered, a settle another registered while it ran
// among them, on ctx and tx, and stops at the first error.
func (s *Scope) Run(ctx context.Context, tx pgx.Tx) error {
	for i := 0; i < len(s.settles); i++ {
		if err := s.settles[i].settle(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}
