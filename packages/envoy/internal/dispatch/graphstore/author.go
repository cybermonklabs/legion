package graphstore

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ProvesAuthor reports whether a credential proves an author: the person a cookie, a personal
// token or a session the broker verified for its owner proves, a session the broker proved
// (model.Actor's Proven), or the workload a service-account token proved, a session a pod's body
// named beside it included (provedAuthor). A credential that names none of them - the
// deployment's shared agent bearer, which resolves as {}, and a session id only a request's body
// named, with no proof and no workload - proves nobody: every holder of the bearer presents the
// same value, so model.Actor.SameCredential reports two such credentials as one, any caller can
// name any session id, and a node reserved for the author of such a write would be readable by
// every such caller rather than by its writer.
//
// This is the one rule, and every reader and every writer asks it rather than keeping a copy:
// the going routes' asker (api.createGoingRequest), the visibility a write may ask for
// (VisibilityFor) and the SQL every author-private read binds (AuthorIsSQL, which states the
// same test over a stored author).
func ProvesAuthor(actor model.Actor) bool {
	_, proved := provedAuthor(actor)
	return proved
}

// SameAuthor is the one rule stated in Go over two actors, as AuthorIsSQL states it in SQL: true
// when the author writer's record proved (provedAuthor) and the author reader's credential proves
// are one author, and false where either proves none.
func SameAuthor(writer, reader model.Actor) bool {
	written, proved := provedAuthor(writer)
	read, reads := provedAuthor(reader)
	return proved && reads && written == read
}

// author is who a credential proved, the one thing an author-private node is read by: the root's
// R19 makes an author the person a credential proved, so two credentials of one person - her
// cookie, her personal token, a session the broker verified as hers - are one author, and a node
// any of them wrote is hers under each of them. Kind is "person", with the canonical login as the
// id; the actor's own kind, for a credential that proves its own id and no person, which is a
// session the broker proved and owned by nobody; or "service", with the workload a
// service-account token proved.
//
// Only a person joins credentials, and a workload the sessions a pod names. A session is its own
// author, by its own id, only where the record says the broker proved it (model.Actor's Proven,
// which a broker-issued token's session carries): every session of a workload carries the same
// workload, so an author by the workload would let each such session read what any other
// reserved. A session id with no such proof is one a request's body named, which proves nothing:
// beside the workload the pod's own token proved its author is that workload, as the pod's
// token's is, and with no workload it is nobody.
type author struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Author kinds that are no actor kind.
const (
	authorPerson  = "person"
	authorService = "service"
)

// provedAuthor is the author a credential proved, in the order authorSQL reads a stored one: the
// owner a session or a personal token carries, then a person at a browser, then the credential's
// own id, which a session proves only where the broker proved it (author), then a workload, which
// a credential with no id it proves is authored by. proved is false for a credential that names
// none of them.
func provedAuthor(actor model.Actor) (author, bool) {
	switch {
	case actor.Owner != nil && *actor.Owner != "":
		return author{Kind: authorPerson, ID: model.CanonicalLogin(*actor.Owner)}, true
	case actor.Kind == "user" && actor.ID != "":
		return author{Kind: authorPerson, ID: model.CanonicalLogin(actor.ID)}, true
	case actor.ID != "" && (actor.Kind != "session" || actor.Proven):
		return author{Kind: actor.Kind, ID: actor.ID}, true
	case actor.Service != nil && *actor.Service != "":
		return author{Kind: authorService, ID: *actor.Service}, true
	}
	return author{}, false
}

// NamesCredential reports whether a recorded actor names a credential at all: an id, an owner or
// a workload. It is the test a run's owner is held to beside model.Actor.SameCredential (api's
// goingRunIsTheCallers): the shared bearer's empty author names none, so SameCredential's reading
// of two such authors as one credential reaches nobody's run. It is no test of the one rule
// (ProvesAuthor): a run is read by the credential that asked it, whatever that credential's
// record proves.
func NamesCredential(actor model.Actor) bool {
	return actor.ID != "" || (actor.Owner != nil && *actor.Owner != "") || (actor.Service != nil && *actor.Service != "")
}

// ErrAuthorNamesNobody refuses a node reserved for its author written on a credential that
// proves none. It is a sentinel so a route can answer it as a refusal of the caller's rather
// than as a failure of the server's, and its words are the reason itself: they say what is
// wrong with the credential and what would be wrong with the node.
var ErrAuthorNamesNobody = errors.New(
	"this credential names nobody, so a node reserved for its author would be nobody's to read: " +
		"every holder of the deployment's shared bearer presents the same value, and two such " +
		"credentials compare as one credential, so write it project-visible or write it under a " +
		"credential that names a session, a person or a workload")

// VisibilityFor is the visibility a write gets for what it asked for. A write that asks for
// nothing is project-visible, which is what every node Dispatch writes today is; a write that
// asks to reserve a node for its author gets that only on a credential that proves one, and is
// refused with ErrAuthorNamesNobody otherwise. A word that is neither is refused by name, so a
// misspelling is a failed write rather than a node nobody can read.
func VisibilityFor(requested string, actor model.Actor) (string, error) {
	switch requested {
	case "", VisibilityProject:
		return VisibilityProject, nil
	case VisibilityAuthor:
		if !ProvesAuthor(actor) {
			return "", ErrAuthorNamesNobody
		}
		return VisibilityAuthor, nil
	}
	return "", fmt.Errorf("visibility %q is no visibility this store holds, which are %q and %q",
		requested, VisibilityProject, VisibilityAuthor)
}

// ActingCredential is the one value an author-private read binds beside its own arguments: the
// author the acting credential proved (provedAuthor), as AuthorIsSQL compares it. A credential
// that proves no author binds nil, which is SQL null, which that comparison answers false for -
// so a reader the store cannot name reads no author-private node at all, rather than every node
// written on the shared bearer.
func ActingCredential(actor model.Actor) any {
	proved, ok := provedAuthor(actor)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(proved)
	if err != nil {
		// Two strings: json.Marshal has nothing here to fail on.
		return nil
	}
	return encoded
}

// AuthorIsSQL is the SQL half of the one rule: true when the author a node's `made_by` column
// records proved somebody, and that somebody is the author bound at param (ActingCredential). It
// states in SQL exactly what provedAuthor states of a credential, so a read and a route cannot
// answer differently. A login is compared in the form user_threads_login gives it on both sides,
// the stored one and the bound one, so the two are canonical by one function.
//
// The whole test is wrapped in coalesce so it is false rather than unknown for an acting
// credential that binds null, and for a stored author that names nobody: a reader that proves no
// author reads no author-private node, and a node no author wrote is nobody's.
func AuthorIsSQL(column, param string) string {
	bound := `(` + param + `::jsonb)`
	return `coalesce(` + authorSQL(column) + ` = ` + authorKeySQL(bound+`->>'kind'`, bound+`->>'id'`) + `, false)`
}

// authorSQL is provedAuthor over a stored author, case for case and in the same order, and null
// for a stored author that names none. An actor with no owner or no workload leaves the key out
// (model.Actor's omitempty), which reads as null here as an empty name does.
func authorSQL(column string) string {
	return `case
		when coalesce(` + column + `->>'owner', '') <> ''
			then ` + authorKeySQL(`'`+authorPerson+`'`, column+`->>'owner'`) + `
		when ` + column + `->>'kind' = 'user' and coalesce(` + column + `->>'id', '') <> ''
			then ` + authorKeySQL(`'`+authorPerson+`'`, column+`->>'id'`) + `
		when coalesce(` + column + `->>'id', '') <> ''
		     and (coalesce(` + column + `->>'kind', '') <> 'session' or coalesce((` + column + `->>'proven')::boolean, false))
			then ` + authorKeySQL(`coalesce(`+column+`->>'kind', '')`, column+`->>'id'`) + `
		when coalesce(` + column + `->>'service', '') <> ''
			then ` + authorKeySQL(`'`+authorService+`'`, column+`->>'service'`) + `
	end`
}

// authorKeySQL is one author as the rule compares two: its kind, and its id, a person's login put
// through user_threads_login, the form every SQL read of this tree compares a login in.
func authorKeySQL(kind, id string) string {
	return `jsonb_build_object('kind', ` + kind + `, 'id', case when ` + kind + ` = '` + authorPerson +
		`' then user_threads_login(` + id + `) else ` + id + ` end)`
}
