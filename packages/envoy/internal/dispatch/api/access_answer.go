package api

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The membership answer (K3) is what the secrets broker reads to decide a project's secrets and a
// token's list: one project's owner, visibility and Slack channels, and whether one person sees
// it, one person's member kind and whether Dispatch holds them (personStanding), which the
// broker's hold gate reads, and every private project's Slack channels. It names no
// repositories. It is plain data read outside any transaction. Only a Kubernetes service account
// the deployment names in DISPATCH_ACCESS_ACCOUNTS reads it, with its own projected token. Before
// anything is read, a caller with no credential, or one whose credential does not verify, is
// refused as every route refuses it (401 NO_IDENTITY for none: optionalActor's error,
// writeAuthenticationError), and every other caller, a person included, is 403
// ACCESS_ACCOUNT_REQUIRED (the read class accessAccount, which withinReadScope holds its routes
// to). A deployment that names no such account answers every caller so.
//
// A question that names a person may also say which Google account the credential being checked
// records, and the answer then says whether that credential's account holds the person's address
// (holder, accountHolder): ?account_issuer=&account_subject=, the broker's introspection's own two,
// or ?account=none, each with ?credential_created_at=, when the credential was made at the broker
// (Unix seconds, the introspection's own field), where it has one. A credential of an account
// Dispatch no longer records for the address acts for nobody, from the moment another account's
// sign-in to Dispatch records itself; one recording no account is held to when it was made,
// against when Dispatch recorded the address's account (people.sub_recorded_at), with
// brokerCredentialMargin between the two clocks; with nothing revoked and nothing ended for anyone
// else. A question that says nothing of the credential gets the answer it always got: no holder,
// and visible as the project's rule reads it. Every broker before this one asks that way, and a
// broker asks it for an operator entry's token, which no account made (brokerPersonAdmitted).

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
