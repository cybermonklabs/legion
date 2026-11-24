package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// workerToken is the claim of the agent that works role's step of issue now, read from the issue's
// phases rows in a read-only transaction of its own (record.ReadWorkerClaim), as a completion reads
// its position (handoffPosition): the claim the step's entry wrote on the role's row, and the
// role's own token where that row runs no step of the plan. The operator's spawn and the
// controller's recoveries name a claim by its issue and role, and so reach the claim that works
// that role's step rather than a claim of the role's name that runs none. A daemon built with no
// record cannot say which claim that is, so it refuses, as every route that reads the record does.
func (s *server) workerToken(ctx context.Context, issue string, role claim.Role) (claim.Token, error) {
	if s.pool == nil || s.records == nil {
		return "", errors.New("record dependencies are unavailable")
	}
	var token claim.Token
	err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		token, err = record.ReadWorkerClaim(ctx, s.records, tx, s.project, issue, role)
		return err
	})
	return token, err
}

// worksStep is the standing of a request on token, the claim the route read as the worker on
// role's step of issue (workerToken): it holds while the record names that claim there still, read
// again where the request's act on the claim is decided (supervise.Standing).
func (s *server) worksStep(issue string, role claim.Role, token claim.Token) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		now, err := s.workerToken(ctx, issue, role)
		return now == token, err
	}
}

// refuseMovedSpawn answers a spawn whose launch of token, the claim it read as the worker on
// role's step of issue, found the step another claim's under the machine's lock
// (supervise.ErrStepLeft): nothing was launched or sent, and the answer names the claim the record
// names there now, read again here, so the operator can ask again of the step as it stands.
func (s *server) refuseMovedSpawn(w http.ResponseWriter, r *http.Request, issue string, role claim.Role, token claim.Token) {
	now, err := s.workerToken(r.Context(), issue, role)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
		"spawn refused: the %s's step of %s moved from claim %s to claim %s under the request; nothing was launched or sent, so ask again",
		role, issue, token, now))
}
