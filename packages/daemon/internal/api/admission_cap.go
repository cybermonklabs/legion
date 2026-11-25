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

// The routes that change the project's admission cap while the daemon runs (dispatch://INFRA-5616):
// the controller's set and clear, under a controller grant the controller session or the operator's
// bearer minted (the `legion` tool's set_admission_cap and clear_admission_cap), and the
// operator's, under the operator's bearer (`legion project set-admission-cap` and
// `clear-admission-cap`). None names a project: the daemon changes its own. The deployment's cap
// (legion.yaml's admission_cap, else LEGION_ADMISSION_CAP) is the ceiling, and a cap above it is
// refused, so raising the ceiling stays the operator's change of legion.yaml.

// admissionCapContract is version.go's entry for dispatch://INFRA-5616: the four cap routes, the
// controller's answer (AnswerAdmissionCap), and the state's `admission.ceiling`,
// `admission.override` and `admission.capChanges`, which a caller below it is served without
// (shaping.go's withoutAdmissionCap). Its constant rather than its number stands in
// SupportedContracts, as OwnerPlanContract's does, so a renumber of the entry moves the two
// together.
const admissionCapContract = 268

// ProjectAdmissionCapRequest is the controller's POST /legion/v1/project/admission-cap: its grant,
// the cap to set, a whole number from 1 to the ceiling, and why: the person's words that ask for
// the change (dispatch://INFRA-5616/spec#b-r8-who-sets).
type ProjectAdmissionCapRequest struct {
	GrantID string `json:"grantId"`
	Cap     int    `json:"cap"`
	Reason  string `json:"reason"`
}

// ProjectAdmissionCapClearRequest is the controller's POST /legion/v1/project/admission-cap/clear:
// its grant and why.
type ProjectAdmissionCapClearRequest struct {
	GrantID string `json:"grantId"`
	Reason  string `json:"reason"`
}

// OperatorAdmissionCapRequest is the operator's POST /legion/v1/operator/project/admission-cap:
// the cap to set and why.
type OperatorAdmissionCapRequest struct {
	Cap    int    `json:"cap"`
	Reason string `json:"reason"`
}

// OperatorAdmissionCapClearRequest is the operator's POST
// /legion/v1/operator/project/admission-cap/clear: why.
type OperatorAdmissionCapClearRequest struct {
	Reason string `json:"reason"`
}

// AdmissionCapOverride is the cap the controller or the operator set while the daemon runs and that
// stands, as the state's `admission.override` and the cap routes' answer carry it: the cap set, who
// set it (a pause's actor view: the controller with its session, or the operator), when and why.
// Its cap may stand above the ceiling where a later configuration lowered the ceiling, and the
// daemon then admits under the ceiling.
type AdmissionCapOverride struct {
	Cap    int            `json:"cap"`
	SetBy  PauseActorView `json:"setBy"`
	SetAt  time.Time      `json:"setAt"`
	Reason string         `json:"reason"`
}

// AdmissionCapChangeView is one change of the cap as the state's `admission.capChanges` lists it,
// newest first: the record the project's Evals tab reads (dispatch://INFRA-5616/spec#b-r7-tick-log,
// dispatch://INFRA-979/spec#b-r39), so its names are a contract of their own. `id` is the change's
// own, rising; `cap` the cap it set, absent for a clear; `capBefore` and `capAfter` the caps the
// daemon admitted under before it and from it on; `ceiling` the deployment's cap then; `setBy` who
// made it, in the actor view of a pause; `setAt` when; and `reason` why. A set of the cap that
// stands and a clear where none stands change nothing and are no entry.
type AdmissionCapChangeView struct {
	ID        int64          `json:"id"`
	Cap       int            `json:"cap,omitempty"`
	CapBefore int            `json:"capBefore"`
	CapAfter  int            `json:"capAfter"`
	Ceiling   int            `json:"ceiling"`
	SetBy     PauseActorView `json:"setBy"`
	SetAt     time.Time      `json:"setAt"`
	Reason    string         `json:"reason"`
}

// AdmissionCapResponse answers a set or a clear of the cap: `cap`, the cap the daemon admits under
// from the change's own pass on; `ceiling`, the deployment's cap, the most a set may name; and
// `override`, the cap set while the daemon runs, absent after a clear. A change that changes
// nothing is answered the same, and the state's `admission.capChanges` shows that it recorded none.
type AdmissionCapResponse struct {
	Cap      int                   `json:"cap"`
	Ceiling  int                   `json:"ceiling"`
	Override *AdmissionCapOverride `json:"override,omitempty"`
}

// The codes of the cap routes' own refusals. Each changes nothing.
const (
	// invalidAdmissionCap is a cap that is not a whole number of at least 1.
	invalidAdmissionCap = "INVALID_ADMISSION_CAP"
	// admissionCapAboveCeiling is a cap above the deployment's ceiling, which the sentence names.
	admissionCapAboveCeiling = "ADMISSION_CAP_ABOVE_CEILING"
	// admissionCapReasonRequired is a change that says nothing of why it is made.
	admissionCapReasonRequired = "ADMISSION_CAP_REASON_REQUIRED"
	// admissionCapUnavailable is a daemon with no workflow, which admits nothing.
	admissionCapUnavailable = "ADMISSION_CAP_UNAVAILABLE"
)

// Routes of the cap, as the fact ids name them.
const (
	admissionCapRoute              = "/legion/v1/project/admission-cap"
	admissionCapClearRoute         = "/legion/v1/project/admission-cap/clear"
	operatorAdmissionCapRoute      = "/legion/v1/operator/project/admission-cap"
	operatorAdmissionCapClearRoute = "/legion/v1/operator/project/admission-cap/clear"
)

// projectAdmissionCap is the controller's set_admission_cap: a controller grant, the controller
// session's or one the operator's bearer minted, sets the cap, and the cap is the operator's when
// the operator's bearer minted the grant (controllerActor). Its answer is a plugin's
// (AnswerAdmissionCap).
func (s *server) projectAdmissionCap(w http.ResponseWriter, r *http.Request) {
	var req ProjectAdmissionCapRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if by, ok := s.controllerActor(w, r, req.GrantID, "a change of the admission cap"); ok {
		s.setAdmissionCap(w, r, AnswerAdmissionCap, admissionCapRoute,
			intake.AdmissionCapSet{Cap: req.Cap, By: by, Reason: req.Reason})
	}
}

// projectAdmissionCapClear is the controller's clear_admission_cap, which returns the project to
// the deployment's cap whoever set the cap it clears. Its answer is a plugin's, as a set's is.
func (s *server) projectAdmissionCapClear(w http.ResponseWriter, r *http.Request) {
	var req ProjectAdmissionCapClearRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) {
		return
	}
	if by, ok := s.controllerActor(w, r, req.GrantID, "a change of the admission cap"); ok {
		s.changeAdmissionCap(w, r, AnswerAdmissionCap, admissionCapClearRoute,
			intake.AdmissionCapSet{By: by, Reason: req.Reason})
	}
}

// operatorProjectAdmissionCap is `legion project set-admission-cap --cap <n> --reason <text>`: the
// operator's bearer sets the cap. Its answer is the `legion` command's, which no plugin parses
// (answerUnparsed, writeUnparsedAnswer).
func (s *server) operatorProjectAdmissionCap(w http.ResponseWriter, r *http.Request) {
	var req OperatorAdmissionCapRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.setAdmissionCap(w, r, answerUnparsed, operatorAdmissionCapRoute,
		intake.AdmissionCapSet{Cap: req.Cap, By: store.PauseActor{Kind: store.PauseByOperator}, Reason: req.Reason})
}

// operatorProjectAdmissionCapClear is `legion project clear-admission-cap --reason <text>`: the
// operator's bearer clears the cap. Its answer is the `legion` command's, as a set's is.
func (s *server) operatorProjectAdmissionCapClear(w http.ResponseWriter, r *http.Request) {
	var req OperatorAdmissionCapClearRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.changeAdmissionCap(w, r, answerUnparsed, operatorAdmissionCapClearRoute,
		intake.AdmissionCapSet{By: store.PauseActor{Kind: store.PauseByOperator}, Reason: req.Reason})
}

// setAdmissionCap refuses a cap below 1 and one above the deployment's ceiling, naming the ceiling
// and where it is raised, and otherwise changes the cap to the one set names (changeAdmissionCap).
func (s *server) setAdmissionCap(w http.ResponseWriter, r *http.Request, answer AnswerID, route string, set intake.AdmissionCapSet) {
	if set.Cap < 1 {
		s.writeFailure(w, r, http.StatusBadRequest, invalidAdmissionCap, fmt.Sprintf(
			"cap is how many issues the daemon may have started at once, a whole number from 1 to this deployment's ceiling of %d, and %d is not; nothing changed",
			s.admissionCeiling, set.Cap))
		return
	}
	if set.Cap > s.admissionCeiling {
		s.writeFailure(w, r, http.StatusConflict, admissionCapAboveCeiling, fmt.Sprintf(
			"the cap %d is above this deployment's ceiling of %d, legion.yaml's admission_cap (or LEGION_ADMISSION_CAP where the file sets none); a cap above it needs the operator to raise admission_cap and restart the daemon; nothing changed",
			set.Cap, s.admissionCeiling))
		return
	}
	s.changeAdmissionCap(w, r, answer, route, set)
}

// changeAdmissionCap refuses a change that says nothing of why, then applies the change as one fact
// through the workflow's handlers, whose admission records it and makes its pass in the fact's one
// transaction, so the cap is in force from that pass on, and answers the cap as the state then
// reads it. A daemon with no workflow (no dispatch_url) has no admission and refuses it
// ADMISSION_CAP_UNAVAILABLE.
func (s *server) changeAdmissionCap(w http.ResponseWriter, r *http.Request, answer AnswerID, route string, change intake.AdmissionCapSet) {
	if strings.TrimSpace(change.Reason) == "" {
		s.writeFailure(w, r, http.StatusBadRequest, admissionCapReasonRequired,
			"a change of the admission cap needs a reason: say in reason why the cap changes; nothing changed")
		return
	}
	if s.pool == nil || len(s.handlers) == 0 {
		s.writeFailure(w, r, http.StatusConflict, admissionCapUnavailable,
			"this daemon cannot change its admission cap: it runs no workflow (legion.yaml names no dispatch_url), so it admits nothing; nothing changed")
		return
	}
	change.At = s.now().UTC()
	if _, err := s.apply(r.Context(), "api", requestFactID(route), change); err != nil {
		s.logRequestFailure(r, err, "api: the change of the admission cap failed", "cap", change.Cap,
			"by", string(change.By.Kind), "by_session", change.By.Session)
		s.writeFailure(w, r, http.StatusInternalServerError, "ADMISSION_CAP_FAILED", "the change of the admission cap failed: "+err.Error())
		return
	}
	view := AdmissionCapResponse{Ceiling: s.admissionCeiling}
	if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		view.Cap, view.Override, err = AdmissionCapStateTx(ctx, tx, s.dispatchProject, s.admissionCeiling)
		return err
	}); err != nil {
		s.logRequestFailure(r, err, "api: read the admission cap after its change")
		s.writeFailure(w, r, http.StatusInternalServerError, "ADMISSION_CAP_FAILED",
			"the change of the admission cap was applied, and reading the cap back failed: "+err.Error())
		return
	}
	s.writeAnswer(w, r, answer, http.StatusOK, view)
}

// AdmissionCapStateTx is, through tx, the cap the daemon admits under and the cap set while it
// runs, where one stands: the state's `admission.cap` and `admission.override`, and a cap route's
// answer. ceiling is the deployment's cap and project the Dispatch key. The cap is
// store.EffectiveAdmissionCap of the project's newest change, the reading every admission pass
// makes (admit's capAt), so the state says the cap the passes admit under and no other.
func AdmissionCapStateTx(ctx context.Context, tx pgx.Tx, project string, ceiling int) (int, *AdmissionCapOverride, error) {
	newest, found, err := store.AdmissionCapTx(ctx, tx, project)
	if err != nil {
		return 0, nil, err
	}
	effective := store.EffectiveAdmissionCap(ceiling, newest, found)
	if !found || newest.Cleared() {
		return effective, nil, nil
	}
	return effective, &AdmissionCapOverride{Cap: newest.Cap, SetBy: pauseActorViewOf(newest.By), SetAt: newest.At,
		Reason: newest.Reason}, nil
}

// AdmissionCapChangesTx is, through tx, the state's `admission.capChanges`: project's newest
// changes of the cap, newest first, at most store.AdmissionCapChangesShown of them. Never nil.
func AdmissionCapChangesTx(ctx context.Context, tx pgx.Tx, project string) ([]AdmissionCapChangeView, error) {
	changes, err := store.AdmissionCapChangesTx(ctx, tx, project, store.AdmissionCapChangesShown)
	if err != nil {
		return nil, err
	}
	views := make([]AdmissionCapChangeView, len(changes))
	for i, c := range changes {
		views[i] = AdmissionCapChangeView{ID: c.ID, Cap: c.Cap, CapBefore: c.CapBefore, CapAfter: c.CapAfter,
			Ceiling: c.Ceiling, SetBy: pauseActorViewOf(c.By), SetAt: c.At, Reason: c.Reason}
	}
	return views, nil
}
