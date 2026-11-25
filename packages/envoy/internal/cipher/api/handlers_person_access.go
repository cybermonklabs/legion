// handlers_person_access.go: GET /v1/person-access — what a person holds at the broker that
// outlives their browser session, counted for Dispatch's server, which asks at a sign-out and in
// its membership pass whether to keep the person's refresh token. Part of the routes a listed
// service calls with its own ServiceAccount token (accountAuth); the person is the one the caller
// names, as it is for every list Dispatch's server relays (handlers_ui_personsignin.go).
package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// personAccessResponse is GET /v1/person-access's answer: wire.PersonAccessResponse itself, named
// here because the broker's generated HTTP reference documents a route's answer from a type this
// package declares, as personSignInsResponse names wire.PersonSignInsResponse.
type personAccessResponse = wire.PersonAccessResponse

// personAccess counts what the person ?person= names holds at the broker that outlives their
// browser: their own live machine logins (enroll.Service.OwnLiveCredentials, which leaves out
// every service's login, one they approved included, and the credentials their sign-ins hold),
// their sign-ins on a machine that are not revoked (store.Store.LivePersonSignIns), the exchange
// entries that name them (exchange.Set.EntriesNaming), and the accounts BROKER_POD_OPERATORS maps
// to them (enroll.Service.PodOperatorMappings), each counted whether or not a pod of it holds a
// live login, since a pod of one logs in as them at any time. Like an exchange entry, a mapping is
// configuration, which only an operator ends. The login is canonicalized before it is read on, as
// the sign-ins list does, and the answer names the person as it was read.
func (s *server) personAccess(w http.ResponseWriter, r *http.Request, _ string) {
	person := record.CanonicalLogin(r.URL.Query().Get("person"))
	if !requirePerson(w, person) {
		return
	}
	logins, err := s.deps.Enroll.OwnLiveCredentials(r.Context(), person)
	if err != nil {
		writeInternal(w, "list a person's own machine logins", err)
		return
	}
	signIns, err := s.deps.Enroll.Store.LivePersonSignIns(r.Context(), person)
	if err != nil {
		writeInternal(w, "count a person's sign-ins", err)
		return
	}
	entries := 0
	if s.deps.Exchange != nil {
		entries = s.deps.Exchange.EntriesNaming(person)
	}
	writeJSON(w, http.StatusOK, personAccessResponse{
		Person: person, MachineLogins: len(logins), SignIns: signIns, ExchangeEntries: entries,
		PodOperatorMappings: s.deps.Enroll.PodOperatorMappings(person),
	})
}
