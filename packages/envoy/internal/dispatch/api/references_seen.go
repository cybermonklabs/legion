package api

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A read's answer names objects that are not its own rows: the issue an issue waits on, the
// document a design gate names, the session that follows an ask, the person an ask waits on, the
// issue a stored event's references_changed moved. The rule (the private projects' R2-R4): such a
// reference reaches a reader only when the reader sees the object it names. One the reader does
// not see is left out of the answer as if it were absent, and a count counts only what remains.
// A link stored across projects stays stored, and a reader who sees both ends reads it.
//
// The rule has one form per kind of object, which every read binds: in a statement, the access
// package's clause for the kind (access.IssueProjectClause for an issue by key, documentClause for
// a document by id, access.SessionClause for a session, access.PersonClause for a person by
// login); and over a decoded answer - a stored event's payload - seenReferences, which asks those
// same clauses of every object the answer names in one statement.

// referenceKind is a kind of object a read's answer names by its id.
type referenceKind string

const (
	issueReference    referenceKind = "issue"    // an issue, by key
	documentReference referenceKind = "document" // a document or a conversation's file, by uuid
	sessionReference  referenceKind = "session"  // an agent session, by id
	personReference   referenceKind = "person"   // a person, by login
)

// referenceKinds is every kind, in the order seenReferences binds them.
var referenceKinds = []referenceKind{issueReference, documentReference, sessionReference, personReference}

// references is a set of objects, by kind and id.
type references map[referenceKind]map[string]bool

// name adds the object id of kind; an empty id names nothing.
func (r references) name(kind referenceKind, id string) {
	if id == "" {
		return
	}
	if r[kind] == nil {
		r[kind] = map[string]bool{}
	}
	r[kind][id] = true
}

// holds reports whether the set holds the object id of kind.
func (r references) holds(kind referenceKind, id string) bool { return r[kind][id] }

// empty reports whether the set names nothing.
func (r references) empty() bool {
	for _, ids := range r {
		if len(ids) > 0 {
			return false
		}
	}
	return true
}

// seesEveryObject reports whether who sees every object of every kind, so that no reference it
// can be answered needs a read: the server's own reader, and an open viewer of a deployment that
// holds no private project, which sees every project, every session and every person, as every
// reader did before a project could be private. That second reader is answered byte for byte what
// it was answered then (R9).
func seesEveryObject(who model.Actor) bool {
	viewer := who.Viewer
	return viewer != nil && (viewer.All || (viewer.Open && !viewer.AnyPrivate))
}

// seenReferencesSQL answers, for the ids of each kind bound at $1..$4 and the viewer at $5, every
// (kind, id) the viewer sees.
var seenReferencesSQL = `
	select 'issue', named.id from unnest($1::text[]) as named (id)
	where ` + access.IssueProjectClause("named.id", "$5") + `
	union all
	select 'document', named.id from unnest($2::text[]) as named (id)
	where exists (select 1 from artifacts seen_document where seen_document.id = ` + store.CastableUUID("named.id") + `
		and ` + documentClause("seen_document", "$5") + ` offset 0)
	union all
	select 'session', named.id from unnest($3::text[]) as named (id)
	where ` + access.SessionClause("named.id", "$5") + `
	union all
	select 'person', named.id from unnest($4::text[]) as named (id)
	where ` + access.PersonClause("named.id", "$5")

// seenReferences answers the objects of named that who sees, in one statement over q. A reader
// that sees every object (seesEveryObject) is answered named itself, with no read; a set that
// names nothing reads nothing.
func seenReferences(ctx context.Context, q queryer, who model.Actor, named references) (references, error) {
	if seesEveryObject(who) || named.empty() {
		return named, nil
	}
	arguments := make([]any, 0, len(referenceKinds)+1)
	for _, kind := range referenceKinds {
		arguments = append(arguments, slices.Sorted(maps.Keys(named[kind])))
	}
	rows, err := q.Query(ctx, seenReferencesSQL, append(arguments, refs.VisibilityArg(who))...)
	if err != nil {
		return nil, fmt.Errorf("read which named objects the reader sees: %w", err)
	}
	seen := references{}
	var kind, id string
	if _, err := pgx.ForEachRow(rows, []any{&kind, &id}, func() error {
		seen.name(referenceKind(kind), id)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read which named objects the reader sees: %w", err)
	}
	return seen, nil
}

// leaveOutHiddenFollowers is followers, by ask, with every follower whose session who does not see
// left out, in one read for every ask.
func leaveOutHiddenFollowers(ctx context.Context, q queryer, who model.Actor, followers map[string][]model.AskFollower) error {
	if seesEveryObject(who) {
		return nil
	}
	named := references{}
	for _, list := range followers {
		for _, follower := range list {
			named.name(sessionReference, follower.SessionID)
		}
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for askID, list := range followers {
		followers[askID] = slices.DeleteFunc(list, func(follower model.AskFollower) bool {
			return !seen.holds(sessionReference, follower.SessionID)
		})
	}
	return nil
}

// leaveOutHiddenReaches is reaches with every session who does not see null: a claimant, a route
// to a session (with what that route reaches), and the session a route reaches.
func leaveOutHiddenReaches(ctx context.Context, q queryer, who model.Actor, reaches map[string]askReach) error {
	if seesEveryObject(who) {
		return nil
	}
	named := references{}
	for _, reach := range reaches {
		if reach.Claimant != nil {
			named.name(sessionReference, *reach.Claimant)
		}
		if reach.Route != nil {
			named.name(sessionReference, routeSession(*reach.Route))
		}
		if reach.RouteHolder != nil {
			named.name(sessionReference, *reach.RouteHolder)
		}
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for askID, reach := range reaches {
		if reach.Claimant != nil && !seen.holds(sessionReference, *reach.Claimant) {
			reach.Claimant = nil
		}
		if reach.Route != nil && routeSession(*reach.Route) != "" && !seen.holds(sessionReference, routeSession(*reach.Route)) {
			reach.Route, reach.IssueRouteReach = nil, model.IssueRouteReach{}
		}
		if reach.RouteHolder != nil && !seen.holds(sessionReference, *reach.RouteHolder) {
			reach.RouteHolder = nil
		}
		reaches[askID] = reach
	}
	return nil
}
