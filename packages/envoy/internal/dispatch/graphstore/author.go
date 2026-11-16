package graphstore

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ProvesAuthor reports whether a credential proves an author: a session or person id, the person
// a personal token belongs to, or the workload a service-account token proved. A credential that
// names none of them - the deployment's shared agent bearer, which resolves as {} - proves
// nobody: every holder of it presents the same value, so model.Actor.SameCredential reports two
// such credentials as one, and a node reserved for the author of such a write would be readable
// by every holder of the bearer rather than by its writer.
//
// This is the one rule, and every reader and every writer asks it rather than keeping a copy:
// the going routes (api.createGoingRequest, api.streamGoingAnswer), the visibility a write may
// ask for (VisibilityFor) and the SQL every author-private read binds (AuthorIsSQL, which states
// the same test over a stored author).
func ProvesAuthor(actor model.Actor) bool {
	return actor.ID != "" || actor.Owner != nil || actor.Service != nil
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

// authorCredential is the part of an actor two credentials are compared by, in the shape a
// stored `made_by` holds it: exactly the fields model.Actor.SameCredential compares, and no
// field two requests of one credential differ in. Owner and Service are pointers so a
// credential that names none is a JSON null here and a missing key there, which compare equal
// as jsonb, where an empty string would compare equal to neither.
type authorCredential struct {
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Owner   *string `json:"owner"`
	Service *string `json:"service"`
}

// ActingCredential is the one value an author-private read binds beside its own arguments: the
// acting credential as AuthorIsSQL compares it. A credential that proves no author binds nil,
// which is SQL null, which that comparison answers false for - so a reader the store cannot
// name reads no author-private node at all, rather than every node written on the shared
// bearer.
func ActingCredential(actor model.Actor) any {
	if !ProvesAuthor(actor) {
		return nil
	}
	encoded, err := json.Marshal(authorCredential{
		Kind: actor.Kind, ID: actor.ID, Owner: actor.Owner, Service: actor.Service,
	})
	if err != nil {
		// Four strings and two string pointers: json.Marshal has nothing here to fail on.
		return nil
	}
	return encoded
}

// AuthorIsSQL is the SQL half of the one rule: true when the author a node's `made_by` column
// records is somebody and is the credential bound at param (ActingCredential). It states in SQL
// exactly what ProvesAuthor states of a credential and model.Actor.SameCredential states of two,
// so a read and a route cannot answer differently.
//
// The whole test is wrapped in coalesce so it is false rather than unknown for an acting
// credential that binds null: a reader that proves no author reads no author-private node.
func AuthorIsSQL(column, param string) string {
	return `coalesce((` + authorIsSomebodySQL(column) + `) and ` + authorNameSQL(column) + ` = ` + param + `::jsonb, false)`
}

// authorIsSomebodySQL is ProvesAuthor over a stored author: the id a record holds, the person a
// personal token belongs to, or the workload a service-account token proved. An actor with no
// owner leaves the key out (model.Actor's omitempty), so the key's presence is the pointer's.
func authorIsSomebodySQL(column string) string {
	return `coalesce(` + column + `->>'id', '') <> '' or jsonb_exists(` + column + `, 'owner') or jsonb_exists(` + column + `, 'service')`
}

// authorNameSQL is a stored author as ActingCredential encodes the acting one: the kind and the
// id as text, empty where the record left them out, and the owner and the workload as the JSON
// they are, so a credential that names none compares equal to a null and not to an empty name.
func authorNameSQL(column string) string {
	return `jsonb_build_object(` +
		`'kind', coalesce(` + column + `->>'kind', ''), ` +
		`'id', coalesce(` + column + `->>'id', ''), ` +
		`'owner', ` + column + `->'owner', ` +
		`'service', ` + column + `->'service')`
}
