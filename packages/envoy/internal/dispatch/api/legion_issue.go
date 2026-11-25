// legion_issue.go serves GET /api/v1/issues/{key}/legion, one issue's Legion state from what the
// sampler recorded (dispatch://INFRA-4869): whether the issue's project names a daemon, the
// issue's phase, the claims of its roles, its slot, its place in the daemon's waiting line, what
// holds it and what it waits on from a person, the newest read of the project's daemon, and the
// revision of the issue's runs. readLegionIssue computes that body from the key alone, so the
// `legion_issue` live view of the issue answers through the same function and sends the GET body
// byte for byte, the hub's rule for every kind (live_frames.go).
//
// Like the board (legion_board.go) it reads what Dispatch holds and never calls the daemon, and it
// reads an issue's observations through the board's own per-issue rule (legion.IssueState), so a
// card and this answer agree on the phase, the roles in a turn and the slot (the spec's R1).
// Unlike the board it refuses no project: the daemons of other projects write runs to this
// Dispatch too, and an issue's runs are its own whatever daemon its project names, so a project
// that names none answers `configured` false, with no state and no read, and still the runs'
// revision, rather than the board's LEGION_NOT_CONFIGURED (the plan's decisions 1 and 2).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// legionIssueResponse is GET /api/v1/issues/{key}/legion: the issue, the project its row names,
// whether that project names a Legion daemon, the issue's state as that daemon recorded it, the
// newest read of the daemon, and the revision of the issue's runs.
// Mirrors packages/contracts/src/dispatch-api.ts's LegionIssueResponse exactly.
type legionIssueResponse struct {
	Issue string `json:"issue"`
	// Project is the issue row's project_key: the project whose daemon the state and the read are
	// of. No route moves an issue to another project.
	Project string `json:"project"`
	// Configured is whether the project names a daemon, a legion_settings row. Where it is false
	// State and Read are null whatever rows an earlier daemon left in legion_current,
	// legion_observations and legion_reads: DELETE /api/v1/projects/{key}/legion/daemon leaves
	// them (legion.DeleteSettings), and they are no daemon's state now.
	Configured bool `json:"configured"`
	// State is null where the daemon does not record the issue: it has no `phase` row, or its
	// newest `phase` row is `null`, which covers every issue of a project whose daemon no read has
	// answered. The answer is a 200 with a null state and not a 404, so the live view answers that
	// case and then moves when the daemon records the issue, and a page tells an issue the daemon
	// does not hold from a read that failed.
	State *legionIssueState `json:"state"`
	// Read is the newest read of the project's daemon as the board's header takes it
	// (readDaemonRead), null where no read of that daemon was ever recorded, and always null
	// where Configured is false.
	Read *legionIssueRead `json:"read"`
	// RunsRevision is the count of the tuples of the issue's runs (workflowstore.IssueRunsRevision),
	// 0 for an issue Legion never ran, whatever Configured says: each write of a run that stores
	// something new raises it, while a write sent again, which stores nothing, leaves it, and that
	// rise is how an open page learns of a run, since a run's write appends no event. It is in the
	// GET body as well as the live one, so the two are one body.
	RunsRevision int64 `json:"runs_revision"`
}

// legionIssueState is the state of an issue the daemon records, each value read through the one
// per-issue rule the board's cards read (legion.IssueState) and the project's admission block
// (legion.Admission's Place and BlockersOf).
type legionIssueState struct {
	// Phase is the newest phase the daemon reported, and PhaseSince when Dispatch stored it.
	// PhaseStartSeen is whether Dispatch saw the issue enter that phase
	// (legion.IssueCurrent.PhaseStartSeen): where it is false the phase is the first value
	// Dispatch stored for the issue, so PhaseSince is when Dispatch first saw the issue there,
	// not when the phase began.
	Phase          string    `json:"phase"`
	PhaseSince     time.Time `json:"phase_since"`
	PhaseStartSeen bool      `json:"phase_start_seen"`
	// Roles is every role with a claim on the issue and the claim's state, a `retired` claim and a
	// role Dispatch does not know included: the roles legionagents.Roles names first, in that
	// order, then the others by name (legion.IssueState.Roles). It is [] where no role has a claim.
	Roles []legion.RoleClaim `json:"roles"`
	// Slot is the index of the admission slot the issue holds, null while it holds none.
	Slot *int `json:"slot"`
	// WaitingLine is where the issue stands in the daemon's waiting line, null where that line does
	// not name it, and for a project whose daemon recorded no admission block.
	WaitingLine *legionIssueWaitingLine `json:"waiting_line"`
	// BlockedBy is the issues the admission block names as holding this one, in the order the
	// daemon stored them, and [] where it names none: never null, so a page reads the list and
	// needs no second rule for its absence.
	BlockedBy []string `json:"blocked_by"`
	// WaitingOn is what the issue waits on from a person, the daemon's object exactly as the
	// sampler stored it, null where it waits on nothing. It stays bytes, as legion.IssueState holds
	// it, so a field a later daemon adds reaches the page. rawFieldSchemas names the fields the
	// daemon writes today (its WaitingOnView), which give the generated type its members, and the
	// card of the board takes three of them (legionBoardWaitOf). The emitter leaves a field that
	// table names out of `required` unless its tag says otherwise, since absent is one of the
	// three states of a request's raw field; this one is always written, null or an object, so its
	// tag says so.
	WaitingOn json.RawMessage `json:"waiting_on" required:"true"`
}

// legionIssueWaitingLine is where an issue stands in the daemon's waiting line: Place counts from
// 1 for the first in the line, and Length is the line's length (legion.Admission.Place).
type legionIssueWaitingLine struct {
	Place  int `json:"place"`
	Length int `json:"length"`
}

// legionIssueRead is the newest read of a project's daemon: when it was made, whether it succeeded,
// and why it did not, null where it did.
type legionIssueRead struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Error *string   `json:"error"`
}

// getLegionIssue answers GET /api/v1/issues/{key}/legion, for any authenticated caller, as the
// board is. Its one refusal beside authentication is a key Dispatch holds no issue of, or one of a
// project the caller does not see, which is 404 NOT_FOUND as GET /api/v1/issues/{key} answers it
// (readLegionIssue).
func (s *server) getLegionIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	answer, err := s.readLegionIssue(r.Context(), who, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// readLegionIssue is GET /api/v1/issues/{key}/legion's body for key, computed from the key alone,
// so the route and the live view of one issue answer one body through one function. Every read it
// makes is one read-only, repeatable-read transaction, as the board's readSampled is, so
// `configured`, the state and the read answer one moment:
//
//   - the issue's row, for its project, as who sees it (access.ProjectClause). A key Dispatch
//     holds no issue of, or one of a project who does not see, is 404 NOT_FOUND in the
//     code and the words writeHandlerError answers pgx.ErrNoRows with, which is GET
//     /api/v1/issues/{key}'s refusal of that key (readIssue), so a mistyped key never reads as an
//     issue the daemon does not record. It is an *apiError, so a live view, which writes no HTTP
//     answer, is refused in the same code and words (viewReads.refusal);
//   - the project's Legion setting. A project with none is the `configured` false body, never an
//     error: this function does not return legion.ErrNoSettings, which the hub would refuse with;
//   - only where the setting exists, the newest read of its daemon (readDaemonRead, the statement
//     the board's header reads) and the issue's state (legion.ReadIssue, which reads the issue's
//     rows and the project's admission row and no other issue's);
//   - in every case, the runs' revision (workflowstore.IssueRunsRevision).
func (s *server) readLegionIssue(ctx context.Context, who model.Actor, key string) (legionIssueResponse, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return legionIssueResponse{}, err
	}
	defer tx.Rollback(ctx)
	viewer := refs.VisibilityArg(who)
	answer := legionIssueResponse{Issue: key}
	if err := tx.QueryRow(ctx, `select project_key from issues where key = $1 and `+access.ProjectClause("project_key", "$2"),
		key, viewer).Scan(&answer.Project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return legionIssueResponse{}, errorf(http.StatusNotFound, "NOT_FOUND", "not found")
		}
		return legionIssueResponse{}, err
	}
	_, err = legion.GetSettings(ctx, tx, answer.Project, viewer)
	switch {
	case errors.Is(err, legion.ErrNoSettings):
	case err != nil:
		return legionIssueResponse{}, err
	default:
		answer.Configured = true
		read, err := readDaemonRead(ctx, tx, answer.Project, viewer)
		if err != nil {
			return legionIssueResponse{}, err
		}
		if read != nil {
			answer.Read = &legionIssueRead{At: read.at.UTC(), OK: read.ok, Error: read.failure}
		}
		current, err := legion.ReadIssue(ctx, tx, answer.Project, key)
		if err != nil {
			return legionIssueResponse{}, err
		}
		answer.State = legionIssueStateOf(key, current)
	}
	if answer.RunsRevision, err = workflowstore.IssueRunsRevision(ctx, tx, who, key); err != nil {
		return legionIssueResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return legionIssueResponse{}, err
	}
	return answer, nil
}

// legionIssueStateOf is the answer's state of key as legion.ReadIssue read it, nil where the daemon
// does not record the issue. The blockers are the admission block's, by the one rule the board's
// cards read them through (legion.Admission.Blockers, which BlockersOf reads, [] where it names
// none), and the place in the line by its sibling (legion.Admission.Place).
func legionIssueStateOf(key string, current legion.IssueCurrent) *legionIssueState {
	if !current.Recorded {
		return nil
	}
	state := &legionIssueState{
		Phase: current.State.Phase, PhaseSince: current.State.PhaseSince.UTC(), PhaseStartSeen: current.PhaseStartSeen,
		Roles: current.State.Roles(), Slot: current.State.Slot,
		BlockedBy: current.Admission.BlockersOf(key), WaitingOn: current.State.WaitingOn,
	}
	if place, length, ok := current.Admission.Place(key); ok {
		state.WaitingLine = &legionIssueWaitingLine{Place: place, Length: length}
	}
	return state
}
