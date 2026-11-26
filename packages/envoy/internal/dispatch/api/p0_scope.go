package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// p0ScopeRefusal is the message of the 403 OAUTH_SCOPE_REQUIRED a write that makes an issue's own
// priority 0 (P0) is refused with, where its credential does not hold the p0 scope. It names the
// scope and says how to get it, since the command prints it as it is.
const p0ScopeRefusal = "this request's credential does not hold the p0 scope, which a priority of 0 needs: " +
	"a person sets P0 at a browser, or gives p0 to a personal token in Settings, Agent tokens"

// refuseP0WithoutScope is the one rule that holds an issue's own priority 0 to the p0 scope
// (model.ScopeP0): it answers the 403 OAUTH_SCOPE_REQUIRED, with the challenge naming p0
// (writeHandlerError writes it from the error), for a write that makes the issue's own priority 0,
// and nil for every other write. Every write that sets an issue's priority asks it: the create
// (createIssue), and the PATCH twice (writeIssue), under the issue's owner lock and, for a close,
// before the close settles the issue's documents (refuseP0BeforeSettlement).
//
// named is the priority the write names: nil for a write that names none and for one that clears
// the priority, since neither makes it 0. stored is the issue's own stored priority, the column
// and never a priority the issue's parent has, and nil for an issue that has none and for a new
// issue, which has none yet. A write makes the priority 0 when it names 0 and the issue's own is
// not 0 already, so a write that keeps P0, lowers it or sets 1 to 3 needs no scope.
//
// credential is the credential of the request, from the caller that optionalActor resolved, and
// nil for a writer with none: it holds no scope, so a writer with no credential that names a
// priority of 0 is refused here like any other. R25's fix has no credential, and a status
// change's steps carry the request's; neither names a priority, so neither reaches a refusal.
func refuseP0WithoutScope(credential *model.Credential, named, stored *int) error {
	if named == nil || *named != 0 || (stored != nil && *stored == 0) || credential.Holds(model.ScopeP0) {
		return nil
	}
	return missingScopeError(model.ScopeP0, p0ScopeRefusal)
}

// refuseP0BeforeSettlement is refuseP0WithoutScope for a PATCH that closes the issue key, asked
// before the close settles the issue's documents (settleIssueBeforeClose), which writes document
// versions and asks: a close refused for p0 writes nothing of its request. It reads the issue's own
// priority on the pool only where the write names 0 from a credential without p0, and with no
// lock held, so it only spares the settlement: the check under the owner lock decides every case,
// and answers each write this read let through. An issue that is not there is left to the lock
// step's own answer.
func (s *server) refuseP0BeforeSettlement(ctx context.Context, credential *model.Credential, key string, named *int) error {
	if refuseP0WithoutScope(credential, named, nil) == nil {
		return nil
	}
	pool, err := s.readyPool()
	if err != nil {
		return err
	}
	var stored *int
	err = pool.QueryRow(ctx, `select priority from issues where key = $1`, key).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return refuseP0WithoutScope(credential, named, stored)
}
