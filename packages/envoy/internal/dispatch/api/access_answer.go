package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The membership answer (K3) is what the secrets broker reads to decide a project's secrets and a
// token's list: one project's owner, visibility and Slack channels, and whether one person sees
// it, one person's member kind, and every private project's Slack channels. It names no
// repositories. It is plain data read outside any transaction. Only a Kubernetes service account
// the deployment names in DISPATCH_ACCESS_ACCOUNTS reads it, with its own projected token. Before
// anything is read, a caller with no credential, or one whose credential does not verify, is
// refused as every route refuses it (401 NO_IDENTITY for none: optionalActor's error,
// writeAuthenticationError), and every other caller, a person included, is 403
// ACCESS_ACCOUNT_REQUIRED (the read class accessAccount, which withinReadScope holds the three
// routes to). A deployment that names no such account answers every caller so.

// accessAccountOf answers the service-account subject a request's verified projected token names,
// and false for every other credential: a broker-issued token that names a fleet as its service
// proved a session, not a pod's account, so it is not one.
func accessAccountOf(actor model.Actor) (string, bool) {
	if actor.Service == nil || actor.Kind != "" || actor.Credential == nil ||
		actor.Credential.Session != "" || actor.Credential.TokenID != "" {
		return "", false
	}
	return *actor.Service, true
}

// requireAccessAccount admits a request only from a service account DISPATCH_ACCESS_ACCOUNTS names:
// withinReadScope's check of every row of the read class accessAccount.
func (s *server) requireAccessAccount(w http.ResponseWriter, r *http.Request) bool {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return false
	}
	if subject, ok := accessAccountOf(actor); ok && slices.Contains(s.deps.AccessAccounts, subject) {
		return true
	}
	writeError(w, "ACCESS_ACCOUNT_REQUIRED", http.StatusForbidden,
		"the membership answer is read only by a service account this deployment names in DISPATCH_ACCESS_ACCOUNTS")
	return false
}

// getAccessProject answers project {key}'s membership answer for ?person=, which may be left out
// to read the project alone (visible false, member_kind none).
func (s *server) getAccessProject(w http.ResponseWriter, r *http.Request) {
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.Answer(r.Context(), pool, r.PathValue("key"), r.URL.Query().Get("person"))
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// getAccessPerson answers ?person='s member kind.
func (s *server) getAccessPerson(w http.ResponseWriter, r *http.Request) {
	person := strings.TrimSpace(r.URL.Query().Get("person"))
	if person == "" {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "person is required: the email whose member kind to read")
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.PersonAnswer(r.Context(), pool, person)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// privateProjectsAnswer is GET /api/v1/access/private-projects' body. Projects is always an
// array, [] when no project is private (access.PrivateProjects), and never left out.
type privateProjectsAnswer struct {
	Projects []model.PrivateProjectChannels `json:"projects" nullable:"false"`
}

// getAccessPrivateProjects answers every private project's Slack channels and when each list last
// changed, by key, read live on each request. Its read class, accessAccount, has withinReadScope
// check the caller before it runs (requireAccessAccount), once, as for the two other answers.
func (s *server) getAccessPrivateProjects(w http.ResponseWriter, r *http.Request) {
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	projects, err := access.PrivateProjects(r.Context(), pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, privateProjectsAnswer{Projects: projects})
}
