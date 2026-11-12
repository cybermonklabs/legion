package api

// going_settings.go is a person's own setting of the command bar (R4): whether every write the
// bar makes for them waits for their confirmation. It is held on the server as the person's own,
// keyed by their canonical login (user_going_settings, migration 20261112220930), so it follows
// them to every browser they sign in from, and the writes route enforces it: with confirm_writes
// on, a write sent as `auto` is refused (goingAutoRefusal) and the bar must send `confirmed`.
// GET and PUT /api/v1/me/going/settings read and write it, as the `me` agent state routes read
// and write a person's per-agent state; nobody reads or writes another person's.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingSettings is a person's command-bar setting: whether every write waits for their
// confirmation. A person who never set it reads false.
type goingSettings struct {
	ConfirmWrites bool `json:"confirm_writes"`
}

// goingSettingsInput is PUT /api/v1/me/going/settings's body, a type of its own for the reason
// goingDisplayFitInput is one: a request's required field and a response's are reflected apart.
type goingSettingsInput struct {
	ConfirmWrites *bool `json:"confirm_writes" required:"true"`
}

// readGoingSettings is login's setting, false for a person who never set it.
func (s *server) readGoingSettings(ctx context.Context, login string) (goingSettings, error) {
	var settings goingSettings
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce((select confirm_writes from user_going_settings where login = $1), false)
	`, model.CanonicalLogin(login)).Scan(&settings.ConfirmWrites); err != nil {
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

// putGoingSettings is PUT /api/v1/me/going/settings, {confirm_writes}: the caller's own setting,
// answered as it now stands. A body without confirm_writes is 400 INVALID_STATE.
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
	if input.ConfirmWrites == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "confirm_writes is required: true or false")
		return
	}
	if _, err := s.deps.Store.Pool.Exec(r.Context(), `
		insert into user_going_settings (login, confirm_writes, updated_at) values ($1, $2, now())
		on conflict (login) do update set confirm_writes = excluded.confirm_writes, updated_at = excluded.updated_at
	`, model.CanonicalLogin(caller.ID), *input.ConfirmWrites); err != nil {
		s.writeHandlerError(w, fmt.Errorf("write the command bar settings: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, goingSettings{ConfirmWrites: *input.ConfirmWrites})
}
