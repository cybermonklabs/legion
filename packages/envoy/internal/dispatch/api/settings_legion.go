// settings_legion.go serves a project's Legion daemon (dispatch://INFRA-1077), a named
// sub-resource of the project it serves: GET, PUT and DELETE /api/v1/projects/{key}/legion/daemon,
// served at the same path under /api/v2 too, since they are rows of the INFRA-979 tree
// (v2Infra979Rows, OfINFRA979).
// No route lists every project's daemon: R26 of dispatch://INFRA-979's spec permits one
// cross-project read, and this is not it, so a page that shows every daemon reads each project's.
//
// A project's daemon is the one Dispatch reads that project's state from
// (internal/dispatch/legion). The path names the project, but writing one belongs to the
// deployment rather than to that project — it points Dispatch at a host, which a credential that
// names one project has no business doing — so the PUT and the DELETE are classed
// scopeDeployment beside the project's default-repository row, not scopeProject (v2Infra979Rows,
// the rows' Scope).
//
// Like putDeliverySettings, these write no event. The write belongs to
// the deployment, so there is no project whose event it would be, and events/broker.go's
// eventOwnerKey refuses an ownerless event; the row's own created_by and updated_at are the
// record of who set it and when.
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// getLegionSetting answers GET /api/v1/projects/{key}/legion/daemon and GET
// /api/v2/projects/{key}/legion/daemon with the project's daemon, or null when it names none, as
// GET /api/v1/settings/delivery answers a setting nobody wrote. An unknown project is 404
// PROJECT_NOT_FOUND.
func (s *server) getLegionSetting(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	settings, err := legion.GetSettings(r.Context(), s.deps.Store.Pool, project)
	if err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			WriteJSON(w, http.StatusOK, nil)
			return
		}
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// putLegionSettingInput is the body of PUT /api/v1/projects/{key}/legion/daemon and of PUT
// /api/v2/projects/{key}/legion/daemon: the daemon base URL the project's state is read from, and
// the acting session where the caller's credential proved none. The handler refuses a body with no
// state_url (400 LEGION_SETTING_INPUT, legionStateURL), which is what makes the field required.
type putLegionSettingInput struct {
	StateURL string       `json:"state_url" required:"true"`
	Actor    *model.Actor `json:"actor"`
}

// putLegionSetting answers PUT /api/v1/projects/{key}/legion/daemon and PUT
// /api/v2/projects/{key}/legion/daemon: name the daemon that project's state is read from. An
// unknown project is 404 PROJECT_NOT_FOUND, and a state_url that is not an absolute http(s) URL
// with a host and no path is 400 LEGION_SETTING_INPUT.
//
// In its own transaction the write notifies legion.SettingsChannel, on which the server's shared
// listener wakes the Legion state intake, and resets the project's event cursor or leaves it as it
// is (legion.PutSettings), by the rule legion's resetCursor holds, which says too when the intake
// then reads the daemon whole.
func (s *server) putLegionSetting(w http.ResponseWriter, r *http.Request) {
	var input putLegionSettingInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	stateURL, refusal := legionStateURL(input.StateURL)
	if refusal != "" {
		writeError(w, r, apicode.LegionSettingInput, refusal)
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	settings, err := legion.PutSettings(r.Context(), s.deps.Store.Pool, project, stateURL, actor)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// An open Legion board of this project was told LEGION_NOT_CONFIGURED and otherwise waits for
	// the intake's read of the new daemon to hear that it now has one. The write announces itself
	// on the same channel a read does, so the board appears as soon as the URL is set.
	if err := legion.NotifyRead(r.Context(), s.deps.Store.Pool, project); err != nil {
		slog.Warn("dispatch: announce a Legion setting write", "project", project, "error", err)
	}
	WriteJSON(w, http.StatusOK, settings)
}

// deleteLegionSetting answers DELETE /api/v1/projects/{key}/legion/daemon and DELETE
// /api/v2/projects/{key}/legion/daemon: stop reading that project's daemon. What was already read
// stays; nothing re-reads it. In its own transaction the delete resets the project's event cursor,
// by the rule legion's resetCursor holds, and notifies legion.SettingsChannel
// (legion.DeleteSettings). A project with no setting is 404 NOT_FOUND, and an unknown one 404
// PROJECT_NOT_FOUND.
func (s *server) deleteLegionSetting(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	project, ok := s.legionProject(w, r)
	if !ok {
		return
	}
	deleted, err := legion.DeleteSettings(r.Context(), s.deps.Store.Pool, project)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !deleted {
		writeError(w, r, apicode.NotFound, "this project has no Legion daemon configured")
		return
	}
	if err := legion.NotifyRead(r.Context(), s.deps.Store.Pool, project); err != nil {
		slog.Warn("dispatch: announce a Legion setting removal", "project", project, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// legionProject is the {key} of the path, once it is a project key and a project of that key
// exists. Every Legion route of one project is under /api/v1/projects/{key}/legion/ and
// /api/v2/projects/{key}/legion/, the three daemon routes and the board alike, so each reads its
// project here and answers alike in both versions: a badly spelled key is 400 INVALID_PROJECT,
// and an unknown project loadProject's 404 PROJECT_NOT_FOUND.
func (s *server) legionProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	project := strings.TrimSpace(r.PathValue("key"))
	if !projectKeyPattern.MatchString(project) {
		writeError(w, r, apicode.InvalidProject, projectKeyProblem("project", project))
		return "", false
	}
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, r, err)
		return "", false
	}
	return project, true
}

// legionStateURL is the daemon base URL a setting may hold, trimmed, or the refusal that names
// what is wrong with it. The rule is the one Dispatch holds DISPATCH_AGENT_SECRETS_URL to: an
// absolute http(s) URL with a host and no path, carrying no user, password, query or fragment,
// since every call this server makes to the daemon is that base with a fixed path after it.
func legionStateURL(raw string) (string, string) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", "state_url is required"
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
		return "", "state_url must be an absolute http(s) URL with a host and no path"
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "state_url must carry no user, password, query or fragment"
	}
	return trimmed, ""
}
