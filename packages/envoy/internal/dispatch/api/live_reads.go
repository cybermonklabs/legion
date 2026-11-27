package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// sessionHiddenFrom reports whether reader may not see session id (access.SessionClause): the rule
// every read of one session keeps over HTTP (sessionHiddenFromReader, and listAgentMessages's
// statement). An open reader of a deployment that holds no
// private project sees every session and costs no read (R9); a server with no database sees none,
// failing closed.
func (s *server) sessionHiddenFrom(ctx context.Context, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return true, nil
	}
	return sessionHiddenThrough(ctx, s.deps.Store.Pool, reader, id)
}

// sessionHiddenThrough is sessionHiddenFrom's read made through q, which a write's read phase
// makes on the transaction it holds (createStoredMessage): one statement, none for an open reader
// of a deployment that holds no private project (R9).
func sessionHiddenThrough(ctx context.Context, q queryer, reader model.Actor, id string) (bool, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return false, nil
	}
	var sees bool
	err := q.QueryRow(ctx, `select `+access.SessionClause("$1::text", "$2"), id, refs.VisibilityArg(reader)).Scan(&sees)
	return !sees, err
}
