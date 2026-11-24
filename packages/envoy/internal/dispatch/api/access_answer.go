package api

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

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
// withinReadScope's check of every row of the read class accessAccount. Its refusal names the row,
// method and pattern, as deploymentReadRefusal's does.
func (s *server) requireAccessAccount(w http.ResponseWriter, r *http.Request, method, pattern string) bool {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return false
	}
	if subject, ok := accessAccountOf(actor); ok && slices.Contains(s.deps.AccessAccounts, subject) {
		return true
	}
	writeError(w, "ACCESS_ACCOUNT_REQUIRED", http.StatusForbidden, fmt.Sprintf(
		"%s %s is read only by a service account this deployment names in DISPATCH_ACCESS_ACCOUNTS", method, pattern))
	return false
}
