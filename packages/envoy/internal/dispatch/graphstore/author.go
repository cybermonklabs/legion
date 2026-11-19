package graphstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ProvesAuthor reports whether a credential proves an author: the person a cookie, a personal
// token or a session the broker verified for its owner proves, a session the broker proved
// (model.Actor's Proven), or the workload a service-account token proved, a session a pod's body
// named beside it included (provedAuthor). A credential that names none of them - the
// deployment's shared agent bearer, which resolves as {}, and a session id only a request's body
// named, not proven and with no workload - proves nobody: every holder of the bearer presents the
// same value, so model.Actor.SameCredential reports two such credentials as one, any caller can
// name any session id, and a node reserved for the author of such a write would be readable by
// every such caller rather than by its writer.
//
// This is the one rule, and every reader and every writer asks it rather than keeping a copy:
// the going POST, of the asker it records (api.createGoingRequest), the visibility a write may
// ask for (VisibilityFor) and the SQL every author-private read binds (AuthorIsSQL, which states
// the same test over a stored author). A run's owner is no question of it: the stream and a
// write made for a run hold the run to the credential that asked it, through NamesCredential and
// model.Actor.SameCredential (api's goingRunIsTheCallers).
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
// id; the actor's own kind, for a credential that proves its own id and no person: a session the
// broker proved and owned by nobody, or an actor of any other kind that carries an id, such as the
// server's own {system, premortem-convert} (api's PremortemConverter); or "service", with the
// workload a service-account token proved.
//
// Only a person joins credentials, and a workload the sessions a pod names. A session is its own
// author, by its own id, only where the record says its credential proved it (model.Actor's
// Proven, which only a broker-issued token sets on a session): every session of a workload
// carries the same workload, so an author by the workload would let each such session read what
// any other reserved. A session id that is not proven is one a request's body named, which
// proves nothing: beside the workload the pod's own token proved its author is that workload, as
// the pod's token's is, and with no workload it is nobody.
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
// refused with ErrAuthorNamesNobody otherwise. A word that is neither is refused by name with the
// InvalidAttributeValueError every write of this store refuses it with, so a misspelling is a
// failed write rather than a node nobody can read.
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
	return "", &InvalidAttributeValueError{Attribute: AttributeVisibility, Value: requested}
}

// authorCredential is the part of an actor two credentials are compared by, in the shape a
// stored `made_by` holds it: exactly the fields model.Actor.SameCredential compares, and no
// field two requests of one credential differ in. Owner and Service are pointers so a
// credential that names none is a JSON null here and a missing key there, which compare equal
// as jsonb, where an empty string would compare equal to neither.
//
// Author rides beside those four and is never compared with them: it is the author the credential
// proved (provedAuthor), which the one rule compares (AuthorIsSQL). AuthorCredentialIsSQL takes it
// off the argument before it compares the four.
type authorCredential struct {
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Owner   *string `json:"owner"`
	Service *string `json:"service"`
	Author  author  `json:"author"`
}

// ActingCredential is the one value an author-private read binds beside its own arguments: the
// acting credential as AuthorCredentialIsSQL compares it, and beside it the author it proved
// (provedAuthor), as AuthorIsSQL compares that. A credential that proves no author binds nil,
// which is SQL null, which both comparisons answer false for - so a reader the store cannot name
// reads no author-private node at all, rather than every node written on the shared bearer.
func ActingCredential(actor model.Actor) any {
	proved, ok := provedAuthor(actor)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(authorCredential{
		Kind: actor.Kind, ID: actor.ID, Owner: actor.Owner, Service: actor.Service, Author: proved,
	})
	if err != nil {
		// Strings and string pointers: json.Marshal has nothing here to fail on.
		return nil
	}
	return encoded
}

// AuthorIsSQL is the SQL half of the one rule: true when the author a node's `made_by` column
// records proved somebody, and that somebody is the author bound at param (ActingCredential). It
// states in SQL exactly what provedAuthor states of a credential, so a read and a route cannot
// answer differently. A login is compared in the form user_threads_login gives it on both sides,
// the stored one and the bound one, so the two are canonical by one function. It is the
// comparison of every kind refs' registry holds but an interaction: every kind
// dispatch://INFRA-1149 registers, and a lens, a finding of one, a risk and a constraint
// (dispatch://INFRA-1950's R19), so a person's cookie, her personal token and a session the
// broker verified as hers read what any of them wrote.
//
// The whole test is wrapped in coalesce so it is false rather than unknown for an acting
// credential that binds null, and for a stored author that names nobody: a reader that proves no
// author reads no author-private node, and a node no author wrote is nobody's.
func AuthorIsSQL(column, param string) string {
	bound := `(` + param + `::jsonb -> 'author')`
	return `coalesce(` + authorSQL(column) + ` = ` + authorKeySQL(bound+`->>'kind'`, bound+`->>'id'`) + `, false)`
}

// AuthorCredentialIsSQL is the comparison of a kind whose predicate compares the whole acting
// credential rather than the author it proved (refs' registry states which a kind compares), an
// interaction alone: true when the author a node's `made_by` column records names a credential at
// all (namesCredentialSQL) and is the credential bound at param. It states in SQL what
// model.Actor.SameCredential states of two credentials, so a run is read by the credential that
// asked it and by no other, as the going stream reads it, and a run whose author is nobody - the
// shared bearer's empty author - by nobody. It compares the credential's four fields and nothing
// else: the argument also carries the author the credential proved, which is taken off it first,
// since the stored author this compares it with is those four fields alone.
//
// It does not ask the one rule of the stored author, and need not: a run's made_by is the
// credential its POST proved, and the bound credential is nil unless the reader proves an author
// (ActingCredential), so the four-field equality already holds the run to the one credential
// that asked it. Asking the rule instead would hide from its own session a run recorded before
// model.Actor's Proven existed, by a broker session that names no owner and no workload: the rule
// counts such a record as nobody, while the session that asked it, proved by the broker now,
// presents the same four fields.
//
// It is wrapped in coalesce as AuthorIsSQL is, and for the same reason.
func AuthorCredentialIsSQL(column, param string) string {
	return `coalesce((` + namesCredentialSQL(column) + `) and ` + authorNameSQL(column) + ` = ` + actingCredentialSQL(param) + `, false)`
}

// NamesCredential reports whether a recorded actor names a credential at all: an id, an owner or
// a workload. It is the test a run's owner is held to beside model.Actor.SameCredential (api's
// goingRunIsTheCallers), as AuthorCredentialIsSQL holds it in SQL (namesCredentialSQL): the
// shared bearer's empty author names none, so SameCredential's reading of two such authors as one
// credential reaches nobody's run. It is no test of the one rule (ProvesAuthor): a run is read by
// the credential that asked it, whatever that credential's record proves.
func NamesCredential(actor model.Actor) bool {
	return actor.ID != "" || (actor.Owner != nil && *actor.Owner != "") || (actor.Service != nil && *actor.Service != "")
}

// actingCredentialSQL is the acting credential bound at param as authorNameSQL builds a stored
// one: its four fields, with the author ActingCredential sets beside them taken off.
func actingCredentialSQL(param string) string {
	return `(` + param + `::jsonb - 'author'::text)`
}

// namesCredentialSQL is NamesCredential over a stored author.
func namesCredentialSQL(column string) string {
	return `coalesce(` + column + `->>'id', '') <> '' or coalesce(` + column + `->>'owner', '') <> ''` +
		` or coalesce(` + column + `->>'service', '') <> ''`
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

// authorSQL is provedAuthor over a stored author, case for case and in the same order, and null
// for a stored author that names none. An actor with no owner, no workload or no proven mark
// leaves the key out (model.Actor's omitempty), which reads as null here as an empty name does.
func authorSQL(column string) string {
	return `case
		when coalesce(` + column + `->>'owner', '') <> ''
			then ` + authorKeySQL(`'`+authorPerson+`'`, column+`->>'owner'`) + `
		when ` + column + `->>'kind' = 'user' and coalesce(` + column + `->>'id', '') <> ''
			then ` + authorKeySQL(`'`+authorPerson+`'`, column+`->>'id'`) + `
		when coalesce(` + column + `->>'id', '') <> ''
		     and (coalesce(` + column + `->>'kind', '') <> 'session' or ` + column + `->>'proven' = 'true')
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

// ErrVisibilityNotAdmitted refuses a node reserved for its author whose kind takes no such node.
// A kind takes one only where every read of it binds the visibility predicate, which the read
// registry states of each kind it flags and hands the store at start (AdmitAuthorOnly): a node of
// any other kind reserved for its author would be answered to everybody by whichever read of it
// binds none. It is a sentinel so a route can answer it as a refusal of the caller's, 400
// VISIBILITY_NOT_ADMITTED; what a write is refused with is a VisibilityNotAdmittedError, which
// wraps it and names the kind.
var ErrVisibilityNotAdmitted = errors.New("a node of this kind is never reserved for its author")

// VisibilityNotAdmittedError is ErrVisibilityNotAdmitted for one kind: the kind of the node a
// write reserved for its author, as the graph addresses it (AddressKind).
type VisibilityNotAdmittedError struct{ Kind string }

func (e *VisibilityNotAdmittedError) Error() string {
	return fmt.Sprintf("the node kind %s takes no node reserved for its author, because not every read of %s binds "+
		"the visibility predicate: write it project-visible", e.Kind, WithArticle(e.Kind))
}

func (e *VisibilityNotAdmittedError) Unwrap() error { return ErrVisibilityNotAdmitted }

// authorOnlyKinds is every node kind the store takes a node reserved for its author of, by the
// kind the graph addresses it by (AddressKind). It is filled once at start, by the read
// registry's init (AdmitAuthorOnly), before any write runs, and only read after that, so it takes
// no lock. A program that never loads the registry holds none, and its store takes no
// author-only node at all.
var authorOnlyKinds = map[string]struct{}{}

// AdmitAuthorOnly is the hand-off of the kinds that may hold a node reserved for its author: the
// read registry's init (refs) hands it, once, every kind its registry flags as one whose every
// read binds the predicate. It runs at init and at no other time. A kind handed twice stops the
// program, naming the kind: each is handed by the one registry entry that flags it, so a second
// hand-off is a second place that thinks it decides the kind, and the two can drift.
func AdmitAuthorOnly(kinds ...string) {
	for _, kind := range kinds {
		if _, admitted := authorOnlyKinds[kind]; admitted {
			panic("graphstore: the node kind " + kind + " is admitted as one that takes a node reserved for its author twice: " +
				"the read registry's flag is the one place that admits it")
		}
		authorOnlyKinds[kind] = struct{}{}
	}
}

// AuthorOnlyKinds is every kind the store takes a node reserved for its author of, sorted.
func AuthorOnlyKinds() []string {
	return slices.Sorted(maps.Keys(authorOnlyKinds))
}

// CheckVisibilityAdmitted is the rule every node write of this store holds a node to before its
// first statement (checkBaseValues): a node of kind and id at visibility author is refused with a
// VisibilityNotAdmittedError naming the kind the graph addresses it by (AddressKind), unless the
// read registry admitted that kind (AdmitAuthorOnly). Any other visibility passes, the values the
// attribute takes being CheckAttributeValue's to hold. A writer that keeps a thing out of the
// store and so reaches no node write - a comment past the address bound (api's commentStored) -
// asks it itself, so the thing is refused as its node would have been.
func CheckVisibilityAdmitted(kind, id, visibility string) error {
	if visibility != VisibilityAuthor {
		return nil
	}
	addressed := AddressKind(kind, id)
	if _, admitted := authorOnlyKinds[addressed]; admitted {
		return nil
	}
	return &VisibilityNotAdmittedError{Kind: addressed}
}
