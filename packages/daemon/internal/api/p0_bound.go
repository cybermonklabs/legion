package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The routes that change the project's P0 bound while the daemon runs (C3 of
// dispatch://INFRA-5636, R10 of dispatch://INFRA-3199): the controller's set and clear, under a
// controller grant the controller session or the operator's bearer minted (the `legion` tool's
// set_p0_bound and clear_p0_bound), and the operator's, under the operator's bearer (`legion
// project set-p0-bound` and `clear-p0-bound`). None names a project: the daemon changes its own. A
// P0 issue starts outside the admission cap, and the bound limits how many P0 issues may hold a
// slot at once. A set names a number, at least 1, or none, which is no bound whatever legion.yaml
// says; a clear takes the live value away, which returns the project to legion.yaml's
// admission_p0_bound.

// ProjectP0BoundRequest is the controller's POST /legion/v1/project/p0-bound: its grant, the bound
// to set, either `bound`, a whole number of at least 1, or `unbounded`, true for no bound, exactly
// one of the two, and why.
type ProjectP0BoundRequest struct {
	GrantID   string `json:"grantId"`
	Bound     *int   `json:"bound,omitempty"`
	Unbounded bool   `json:"unbounded,omitempty"`
	Reason    string `json:"reason"`
}

// ProjectP0BoundClearRequest is the controller's POST /legion/v1/project/p0-bound/clear: its grant
// and why.
type ProjectP0BoundClearRequest struct {
	GrantID string `json:"grantId"`
	Reason  string `json:"reason"`
}

// OperatorP0BoundRequest is the operator's POST /legion/v1/operator/project/p0-bound: the bound to
// set, `bound` or `unbounded` as the controller's request names it, and why.
type OperatorP0BoundRequest struct {
	Bound     *int   `json:"bound,omitempty"`
	Unbounded bool   `json:"unbounded,omitempty"`
	Reason    string `json:"reason"`
}

// OperatorP0BoundClearRequest is the operator's POST /legion/v1/operator/project/p0-bound/clear:
// why.
type OperatorP0BoundClearRequest struct {
	Reason string `json:"reason"`
}

// P0BoundView is the P0 bound as the state's `admission.p0Bound` and the P0 bound routes' answer
// carry it: `bound`, the bound in force, how many P0 issues may hold a slot at once, or null for
// none; `configured`, legion.yaml's admission_p0_bound, null where the file gives none, which is the
// bound in force while no live value stands, read at the daemon's boot (`daemon.startedAt`); and
// `live`, the value the controller or the operator set while the daemon runs and that stands, with
// who set it, when and why, absent where none was set or the newest change cleared it.
type P0BoundView struct {
	Bound      *int             `json:"bound"`
	Configured *int             `json:"configured"`
	Live       *P0BoundLiveView `json:"live,omitempty"`
}

// P0BoundLiveView is a P0 bound set while the daemon runs that stands: `bound`, the number it set,
// or null where it set none; `setBy`, who set it, a pause's actor view; `setAt`, when; and
// `reason`, why.
type P0BoundLiveView struct {
	Bound  *int           `json:"bound"`
	SetBy  PauseActorView `json:"setBy"`
	SetAt  time.Time      `json:"setAt"`
	Reason string         `json:"reason"`
}

// The codes of the P0 bound routes' own refusals. Each changes nothing.
const (
	// invalidP0Bound is a set that names neither a bound nor none, both, or a bound below 1.
	invalidP0Bound = "INVALID_P0_BOUND"
	// p0BoundReasonRequired is a change that says nothing of why it is made.
	p0BoundReasonRequired = "P0_BOUND_REASON_REQUIRED"
	// p0BoundUnavailable is a daemon with no workflow, which admits nothing.
	p0BoundUnavailable = "P0_BOUND_UNAVAILABLE"
)

// Routes of the P0 bound, as the fact ids name them.
const (
	p0BoundRoute              = "/legion/v1/project/p0-bound"
	p0BoundClearRoute         = "/legion/v1/project/p0-bound/clear"
	operatorP0BoundRoute      = "/legion/v1/operator/project/p0-bound"
	operatorP0BoundClearRoute = "/legion/v1/operator/project/p0-bound/clear"
)

// projectP0Bound is the controller's set_p0_bound: a controller grant, the controller session's or
// one the operator's bearer minted, sets the bound, and the bound is the operator's when the
// operator's bearer minted the grant (controllerActor). Its answer is a plugin's (AnswerP0Bound).
func (s *server) projectP0Bound(w http.ResponseWriter, r *http.Request) {
	var req ProjectP0BoundRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if by, ok := s.controllerActor(w, r, req.GrantID, "a change of the P0 bound requires a controller grant"); ok {
		s.setP0Bound(w, r, AnswerP0Bound, p0BoundRoute, req.Bound, req.Unbounded, by, req.Reason)
	}
}

// projectP0BoundClear is the controller's clear_p0_bound, which returns the project to
// legion.yaml's bound whoever set the value it clears. Its answer is a plugin's, as a set's is.
func (s *server) projectP0BoundClear(w http.ResponseWriter, r *http.Request) {
	var req ProjectP0BoundClearRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if by, ok := s.controllerActor(w, r, req.GrantID, "a change of the P0 bound requires a controller grant"); ok {
		s.changeP0Bound(w, r, AnswerP0Bound, p0BoundClearRoute,
			intake.P0BoundSet{Mode: store.P0BoundCleared, By: by, Reason: req.Reason})
	}
}

// operatorProjectP0Bound is `legion project set-p0-bound (--bound <n> | --none) --reason <text>`:
// the operator's bearer sets the bound. Its answer is the `legion` command's, which no plugin
// parses (answerUnparsed, writeUnparsedAnswer).
func (s *server) operatorProjectP0Bound(w http.ResponseWriter, r *http.Request) {
	var req OperatorP0BoundRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.setP0Bound(w, r, answerUnparsed, operatorP0BoundRoute, req.Bound, req.Unbounded,
		store.PauseActor{Kind: store.PauseByOperator}, req.Reason)
}

// operatorProjectP0BoundClear is `legion project clear-p0-bound --reason <text>`: the operator's
// bearer clears the live bound. Its answer is the `legion` command's, as a set's is.
func (s *server) operatorProjectP0BoundClear(w http.ResponseWriter, r *http.Request) {
	var req OperatorP0BoundClearRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.changeP0Bound(w, r, answerUnparsed, operatorP0BoundClearRoute,
		intake.P0BoundSet{Mode: store.P0BoundCleared, By: store.PauseActor{Kind: store.PauseByOperator}, Reason: req.Reason})
}

// setP0Bound refuses a set that names neither a bound nor none, both, or a bound below 1, and
// otherwise changes the bound to the one it names (changeP0Bound).
func (s *server) setP0Bound(w http.ResponseWriter, r *http.Request, answer AnswerID, route string, bound *int, unbounded bool,
	by store.PauseActor, why string,
) {
	switch {
	case bound != nil && unbounded:
		s.writeFailure(w, r, http.StatusBadRequest, invalidP0Bound,
			"a set of the P0 bound names bound, a whole number of at least 1, or unbounded, for no bound, and this one names both; nothing changed")
	case bound == nil && !unbounded:
		s.writeFailure(w, r, http.StatusBadRequest, invalidP0Bound,
			"a set of the P0 bound names bound, a whole number of at least 1, or unbounded, for no bound, and this one names neither; a clear, which returns to legion.yaml's admission_p0_bound, is its own route; nothing changed")
	case unbounded:
		s.changeP0Bound(w, r, answer, route, intake.P0BoundSet{Mode: store.P0BoundUnbounded, By: by, Reason: why})
	case *bound < 1:
		s.writeFailure(w, r, http.StatusBadRequest, invalidP0Bound, fmt.Sprintf(
			"bound is how many P0 issues may hold a slot at once, a whole number of at least 1, and %d is not; unbounded sets no bound; nothing changed", *bound))
	default:
		s.changeP0Bound(w, r, answer, route, intake.P0BoundSet{Mode: store.P0BoundSet, Bound: *bound, By: by, Reason: why})
	}
}

// changeP0Bound refuses a change that says nothing of why, then applies the change as one fact
// through the workflow's handlers, whose admission records it and makes its pass in the fact's one
// transaction, so the bound is in force from that pass on, and answers the bound as the state then
// reads it. A daemon with no workflow (no dispatch_url) has no admission and refuses it
// P0_BOUND_UNAVAILABLE.
func (s *server) changeP0Bound(w http.ResponseWriter, r *http.Request, answer AnswerID, route string, change intake.P0BoundSet) {
	if strings.TrimSpace(change.Reason) == "" {
		s.writeFailure(w, r, http.StatusBadRequest, p0BoundReasonRequired,
			"a change of the P0 bound needs a reason: say in reason why the bound changes; nothing changed")
		return
	}
	if s.pool == nil || len(s.handlers) == 0 {
		s.writeFailure(w, r, http.StatusConflict, p0BoundUnavailable,
			"this daemon cannot change its P0 bound: it runs no workflow (legion.yaml names no dispatch_url), so it admits nothing; nothing changed")
		return
	}
	change.At = s.now().UTC()
	if _, err := s.apply(r.Context(), "api", requestFactID(route), change); err != nil {
		s.logRequestFailure(r, err, "api: the change of the P0 bound failed", "mode", string(change.Mode), "bound", change.Bound,
			"by", string(change.By.Kind), "by_session", change.By.Session)
		s.writeFailure(w, r, http.StatusInternalServerError, "P0_BOUND_FAILED", "the change of the P0 bound failed: "+err.Error())
		return
	}
	var view P0BoundView
	if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		view, err = P0BoundStateTx(ctx, tx, s.dispatchProject, s.p0BoundConfigured)
		return err
	}); err != nil {
		s.logRequestFailure(r, err, "api: read the P0 bound after its change")
		s.writeFailure(w, r, http.StatusInternalServerError, "P0_BOUND_FAILED",
			"the change of the P0 bound was applied, and reading the bound back failed: "+err.Error())
		return
	}
	s.writeAnswer(w, r, answer, http.StatusOK, view)
}

// P0BoundStateTx is, through tx, the state's `admission.p0Bound` and a P0 bound route's answer:
// the bound in force, legion.yaml's, and the live value that stands, where one does. configured is
// legion.yaml's admission_p0_bound, nil where the file gives none, and project the Dispatch key.
// The bound in force is store.EffectiveP0Bound of the project's newest change, the reading every
// admission pass makes (admit's p0BoundAt), so the state says the bound the passes admit under and
// no other.
func P0BoundStateTx(ctx context.Context, tx pgx.Tx, project string, configured *int) (P0BoundView, error) {
	newest, found, err := store.P0BoundTx(ctx, tx, project)
	if err != nil {
		return P0BoundView{}, err
	}
	view := P0BoundView{Bound: store.EffectiveP0Bound(configured, newest, found), Configured: configured}
	if found && newest.Stands() {
		live := &P0BoundLiveView{SetBy: pauseActorViewOf(newest.By), SetAt: newest.At.UTC(), Reason: newest.Reason}
		if newest.Mode == store.P0BoundSet {
			live.Bound = new(newest.Bound)
		}
		view.Live = live
	}
	return view, nil
}
