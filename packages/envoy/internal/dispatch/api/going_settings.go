package api

// going_settings.go is a person's own setting of the command bar (R5): the mode that says which
// rows of a run's plan wait for them (going_modes.go). In Ask, the mode of a person who never chose
// one, every row waits; in Auto-review a review step of the run lets some through. It is held on
// the server as the person's own, keyed by their canonical login (user_going_settings,
// migrations 20261112220930 and 20261119201455), so it follows them to every browser they sign in
// from, and a run takes it when it starts (going_runs.mode). GET and PUT
// /api/v1/me/going/settings read and write it, as the `me` agent state routes read and write a
// person's per-agent state; nobody reads or writes another person's.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingSettings is a person's command-bar setting: which rows of a run's plan wait for them. A
// person who never set it reads ask.
type goingSettings struct {
	Mode string `json:"mode" enum:"ask,auto_review"`
}

// goingSettingsInput is PUT /api/v1/me/going/settings's body, a type of its own for the reason
// newIssueSettingsInput is one: a request's required field and a response's are reflected apart.
type goingSettingsInput struct {
	Mode string `json:"mode" required:"true" enum:"ask,auto_review"`
}

// readGoingSettings is login's setting, ask for a person who never set it.
func (s *server) readGoingSettings(ctx context.Context, login string) (goingSettings, error) {
	settings := goingSettings{Mode: goingModeAsk}
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce((select mode from user_going_settings where login = $1), $2)
	`, model.CanonicalLogin(login), goingModeAsk).Scan(&settings.Mode); err != nil {
		return goingSettings{}, fmt.Errorf("read the command bar settings: %w", err)
	}
	return settings, nil
}

// getGoingSettings is GET /api/v1/me/going/settings: the caller's own setting.
func (s *server) getGoingSettings(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	settings, err := s.readGoingSettings(r.Context(), caller.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// putGoingSettings is PUT /api/v1/me/going/settings, {mode}: the caller's own setting, answered
// as it now stands. A body whose mode is neither ask nor auto_review is 400 INVALID_STATE. A run
// takes the mode when it starts, so one already started keeps its own.
func (s *server) putGoingSettings(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input goingSettingsInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Mode != goingModeAsk && input.Mode != goingModeAutoReview {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "mode is required: ask or auto_review")
		return
	}
	if _, err := s.deps.Store.Pool.Exec(r.Context(), `
		insert into user_going_settings (login, mode, updated_at) values ($1, $2, now())
		on conflict (login) do update set mode = excluded.mode, updated_at = excluded.updated_at
	`, model.CanonicalLogin(caller.ID), input.Mode); err != nil {
		s.writeHandlerError(w, fmt.Errorf("write the command bar settings: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, goingSettings{Mode: input.Mode})
}
