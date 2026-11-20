package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A read's answer names objects that are not its own rows: the issue an issue waits on, the
// document a design gate names, the session that follows an ask, the person an ask waits on, the
// issue a stored event's references_changed moved. The rule, that a reader sees only the objects
// of the projects it sees: such a reference reaches a reader only when the reader sees the object
// it names. One the reader does not see is left out of the answer as if it were absent, and a
// count counts only what remains.
// A link stored across projects stays stored, and a reader who sees both ends reads it.
//
// The rule has one form per kind of object, which every read binds: in a statement, the access
// package's clause for the kind (access.IssueProjectClause for an issue by key,
// access.DocumentClause for a document by id, access.SessionClause for a session,
// access.PersonClause for a person by login, and roleProjectSeenSQL, access.RoleClause's half that
// reads a Legion role's name, for the project a role works for); and over a decoded answer - a
// stored event's payload - seenReferences, which asks those same clauses of every object the answer
// names in one statement.

// referenceKind is a kind of object a read's answer names by its id.
type referenceKind string

const (
	issueReference    referenceKind = "issue"    // an issue, by key
	documentReference referenceKind = "document" // a document or a conversation's file, by uuid
	sessionReference  referenceKind = "session"  // an agent session, by id
	personReference   referenceKind = "person"   // a person, by login
	// roleProjectReference is the project a Legion role's name works for, by key
	// (contracts.RoleProject): a role of a private project its reader does not see reaches that
	// reader as no role at all, as access.RoleClause passes it.
	roleProjectReference referenceKind = "role_project"
)

// referenceKinds is every kind, in the order seenReferences binds them.
var referenceKinds = []referenceKind{issueReference, documentReference, sessionReference, personReference, roleProjectReference}

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

// actorReference is the object an actor names: a session by its id, a person by their login, and
// none for an actor of any other kind (a service, the deployment's shared token), which no clause
// holds back.
func actorReference(actor model.Actor) (referenceKind, string) {
	switch actor.Kind {
	case "session":
		return sessionReference, actor.ID
	case "user":
		return personReference, actor.ID
	}
	return "", ""
}

// nameActor adds the object actor names (actorReference), if it names one.
func (r references) nameActor(actor model.Actor) {
	if kind, id := actorReference(actor); kind != "" {
		r.name(kind, id)
	}
}

// holdsActor reports whether the set holds the object actor names; an actor that names none, or
// names one by no id, is held, since nothing it names is held back.
func (r references) holdsActor(actor model.Actor) bool {
	kind, id := actorReference(actor)
	return kind == "" || id == "" || r.holds(kind, id)
}

// routeRoleProject is the project the role a `role:<name>` route names works for, by the role's
// name (contracts.RoleProject), and "" for a route of any other kind and a role of no project.
func routeRoleProject(route string) string {
	role, ok := strings.CutPrefix(route, "role:")
	if !ok {
		return ""
	}
	if project := contracts.RoleProject(role); project != contracts.NoProject {
		return project
	}
	return ""
}

// nameRoute adds what a route names: the session a session route names, and the project the role a
// role route names works for. A nil route names nothing.
func (r references) nameRoute(route *string) {
	if route != nil {
		r.name(sessionReference, routeSession(*route))
		r.name(roleProjectReference, routeRoleProject(*route))
	}
}

// holdsRoute reports whether the set holds what route names: the session a `session:<id>` route
// names, and the project the role a `role:<name>` route names works for (routeRoleProject). A
// route that names neither is held.
func (r references) holdsRoute(route string) bool {
	if session := routeSession(route); session != "" && !r.holds(sessionReference, session) {
		return false
	}
	project := routeRoleProject(route)
	return project == "" || r.holds(roleProjectReference, project)
}

// leaveOutRoute writes a route as the reader the set answers reads it: a route that names a
// session, or a role of a project, the reader does not see is null, as an issue with no route. It
// runs before the roster is read (leaveOutHiddenRouteHolders says why), so no reach is judged yet.
func (r references) leaveOutRoute(route **string) {
	if *route != nil && !r.holdsRoute(**route) {
		*route = nil
	}
}

// routedReach is one answer's route, as leaveOutRoute left it, and the reach the roster judged
// for it.
type routedReach struct {
	route *string
	reach *model.IssueRouteReach
}

// leaveOutHiddenRouteHolders writes each reach of a role route as who reads it: a holder who does
// not see reads as no holder (model.RouteNoHolder), never live or holder_unresponsive with no
// holder named, which no reach answers otherwise. A session route's holder is the session the
// route names, which leaveOutRoute already judged, so only a role route is read here.
//
// The reads that answer a reach judge what their answer names in two steps, so that what a hidden
// object costs is what its absence costs: first the routes and the claims, before the roster is
// read at all (leaveOutRoute, in seenReferences' one statement), so a route or a claim the reader
// does not see reads no roster, as no route and no claim read none; then, once the roster named
// each role's holder, this one statement, made whenever a role route remains, a holder or none, so
// a role held by a hidden session costs what an unheld role costs. A reader that sees every object
// reads nothing here, as before.
func leaveOutHiddenRouteHolders(ctx context.Context, q queryer, who model.Actor, routes []routedReach) error {
	if seesEveryObject(who) {
		return nil
	}
	roles := false
	named := references{}
	for _, routed := range routes {
		if routed.route == nil || !strings.HasPrefix(*routed.route, "role:") {
			continue
		}
		roles = true
		if routed.reach.RouteHolder != nil {
			named.name(sessionReference, *routed.reach.RouteHolder)
		}
	}
	if !roles {
		return nil
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for _, routed := range routes {
		if holder := routed.reach.RouteHolder; holder != nil && !seen.holds(sessionReference, *holder) {
			*routed.reach = model.IssueRouteReach{RouteStatus: new(model.RouteNoHolder)}
		}
	}
	return nil
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

// seenReferencesSQL answers, for the ids of each kind bound at $1..$5 and the viewer at $6, every
// (kind, id) the viewer sees.
var seenReferencesSQL = `
	select 'issue', named.id from unnest($1::text[]) as named (id)
	where ` + access.IssueProjectClause("named.id", "$6") + `
	union all
	select 'document', named.id from unnest($2::text[]) as named (id)
	where exists (select 1 from artifacts seen_document where seen_document.id = ` + store.CastableUUID("named.id") + `
		and ` + access.DocumentClause("seen_document", "$6") + ` offset 0)
	union all
	select 'session', named.id from unnest($3::text[]) as named (id)
	where ` + access.SessionClause("named.id", "$6") + `
	union all
	select 'person', named.id from unnest($4::text[]) as named (id)
	where ` + access.PersonClause("named.id", "$6") + `
	union all
	select 'role_project', named.id from unnest($5::text[]) as named (id)
	where ` + roleProjectSeenSQL("named.id", "$6")

// roleProjectSeenSQL passes the project key keyExpr gives, the project a Legion role's name works
// for, unless it is a private project the viewer bound at viewerParam does not see: the half of
// access.RoleClause that reads a role's name, for a role whose holder is judged by the listener's
// answer rather than by a held_by tuple (a route's reach, a delivery's resolve). A key no project
// holds, a public project's and a null one pass, and a deployment that holds no private project
// reads no project.
func roleProjectSeenSQL(keyExpr, viewerParam string) string {
	return `not (` + access.ViewerFlag(viewerParam, "private") + ` and exists (
		select 1 from projects hidden
		where hidden.key = (` + keyExpr + `) and hidden.visibility = 'private'
			and not ` + access.ProjectClause("hidden.key", viewerParam) + `
		offset 0))`
}

// seenReferences answers the objects of named that who sees, in one statement over q, whether or
// not named holds anything: a read's cost never tells whether it named an object its reader does
// not see, so an answer that names a hidden session, person, issue, document or role costs what
// one that names none costs. A reader that sees every object (seesEveryObject) is answered named
// itself, with no read, as before (R9). A caller with no rows to write reads nothing, since how
// many rows an answer holds is the reader's to see.
func seenReferences(ctx context.Context, q queryer, who model.Actor, named references) (references, error) {
	if seesEveryObject(who) {
		return named, nil
	}
	rows, err := q.Query(ctx, seenReferencesSQL, seenReferencesArgs(who, named)...)
	if err != nil {
		return nil, fmt.Errorf("read which named objects the reader sees: %w", err)
	}
	seen := references{}
	if err := scanSeenReferences(rows, seen); err != nil {
		return nil, fmt.Errorf("read which named objects the reader sees: %w", err)
	}
	return seen, nil
}

// seenReferencesArgs is what seenReferencesSQL binds for named and who: the ids of each kind,
// sorted, in referenceKinds' order, then who's viewer (refs.VisibilityArg).
func seenReferencesArgs(who model.Actor, named references) []any {
	arguments := make([]any, 0, len(referenceKinds)+1)
	for _, kind := range referenceKinds {
		arguments = append(arguments, slices.Sorted(maps.Keys(named[kind])))
	}
	return append(arguments, refs.VisibilityArg(who))
}

// scanSeenReferences names in seen every (kind, id) the rows of seenReferencesSQL answer.
func scanSeenReferences(rows pgx.Rows, seen references) error {
	var kind, id string
	_, err := pgx.ForEachRow(rows, []any{&kind, &id}, func() error {
		seen.name(referenceKind(kind), id)
		return nil
	})
	return err
}

// leaveOutHiddenFollowers is followers, by ask, with every follower whose session who does not see
// left out, in one read for every ask; no ask reads nothing.
func leaveOutHiddenFollowers(ctx context.Context, q queryer, who model.Actor, followers map[string][]model.AskFollower) error {
	if seesEveryObject(who) || len(followers) == 0 {
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

// leaveOutHiddenReaches is reaches with every session who does not see null, judged before the
// roster is read (leaveOutHiddenRouteHolders), in one statement whatever they name: a claimant who
// does not see is null, and so is a route to a session, or to a role of a project, who does not see.
func leaveOutHiddenReaches(ctx context.Context, q queryer, who model.Actor, reaches map[string]askReach) error {
	if seesEveryObject(who) || len(reaches) == 0 {
		return nil
	}
	named := references{}
	for _, reach := range reaches {
		if reach.Claimant != nil {
			named.name(sessionReference, *reach.Claimant)
		}
		named.nameRoute(reach.Route)
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for askID, reach := range reaches {
		if reach.Claimant != nil && !seen.holds(sessionReference, *reach.Claimant) {
			reach.Claimant = nil
		}
		seen.leaveOutRoute(&reach.Route)
		reaches[askID] = reach
	}
	return nil
}
