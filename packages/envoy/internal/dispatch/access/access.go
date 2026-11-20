// Package access is Dispatch's one rule of who may see a project (INFRA-3862), and the reads and
// writes of what that rule reads: each project's owner and visibility, its access list of people
// and groups, the groups, and each person's member kind.
//
// The rule. A person P with member kind M sees project p when p's owner is P, when p's access
// list names P, when M is full and a group p's list names has P as a member whose people row is
// a full member's, or when p is public and M is full. A browser sees what its person sees. An
// agent credential with a list of projects sees the part of its list its person sees; with no
// list, the public part of what its person sees. A credential with no person sees the public
// projects, within its list when it holds one. A viewer is open when it is not a restricted member
// and holds no list: it reads every row that belongs to no project, as every caller did before.
//
// One home. The rule is written once in SQL (PersonSeesProjectSQL, VisibleProjectsSQL) and once in
// Go over a person already resolved (Credential.Sees), and a test holds the two equal over every
// seeded caller. The outside sign-in's question, whether some project names an email directly, is
// listed.go's, the same rule's direct arm read from the other side. Every SQL fragment this
// package hands a statement starts with Marker and its own name, so a statement that binds one
// says so wherever its text is read.
package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ErrUnresolvedActor is Resolve's refusal of an actor that carries no credential: one a write
// recorded (model.Actor.Recorded), or the person actingPerson answers. What a caller may see is
// read off the request's credential - its list of projects, and whether the broker proved its
// session - so such an actor has nothing to resolve, and answering it as a credential that lists
// nothing would quietly widen or narrow what it sees.
var ErrUnresolvedActor = errors.New("access: the actor carries no credential to resolve; resolve the request's caller (optionalActor)")

// Querier is what this package reads through: a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Marker opens every SQL fragment this package writes; the fragment's own name and ` */` follow.
const Marker = "/* project access: "

func fragment(name, sql string) string { return Marker + name + " */ " + sql }

// PersonSeesProjectSQL is the rule for one person over one project row: true when the person,
// whose member kind memberExpr answers, sees the project projectAlias names. The group arm
// counts a member only through a people row of a full member, so a restricted member, or a login
// nobody has signed in as, is never admitted by a group (v24 R2).
func PersonSeesProjectSQL(personExpr, memberExpr, projectAlias string) string {
	return fragment("PersonSeesProjectSQL", fmt.Sprintf(`(%[3]s.owner = %[1]s
		or exists (
			select 1 from project_access listed
			where listed.project_key = %[3]s.key and listed.principal_kind = 'person' and listed.principal = %[1]s
			offset 0)
		or exists (
			select 1
			from project_access listed
			join access_group_members member on member.group_name = listed.principal
			join people full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = %[3]s.key and listed.principal_kind = 'group' and member.person = %[1]s
			offset 0)
		or (%[3]s.visibility = 'public' and %[2]s = 'full'))`, personExpr, memberExpr, projectAlias))
}

// VisibleProjectsSQL answers one caller's viewer in one row: every project it sees as a sorted
// jsonb array, whether it is a restricted member, and whether any project is private. Its
// parameters are $1 the person (null for a credential that names none), $2 whether the
// credential is a broker token, whose person reads as a full member when no people row holds
// them (the broker signs in only people of its sign-in domain), $3 whether it is an agent
// credential rather than a browser, and $4 its list of projects (null or empty for none).
func VisibleProjectsSQL() string {
	return fragment("VisibleProjectsSQL", `
		with viewer as (
			select $1::text as person,
				case when $1::text is null then null
					else coalesce(
						(select member_kind from people where email = $1::text),
						case when $2::boolean then 'full' end)
				end as member_kind,
				coalesce(cardinality($4::text[]), 0) > 0 as listed
		)
		select
			coalesce(jsonb_agg(p.key order by p.key collate "C") filter (where
				case when viewer.person is null
					then p.visibility = 'public' and (not viewer.listed or p.key = any($4::text[]))
					else `+PersonSeesProjectSQL("viewer.person", "viewer.member_kind", "p")+`
						and (not $3::boolean
							or case when viewer.listed then p.key = any($4::text[]) else p.visibility = 'public' end)
				end), '[]'::jsonb),
			coalesce(viewer.member_kind = 'restricted', false),
			exists (select 1 from projects where visibility = 'private')
		from viewer
		left join projects p on true
		group by viewer.person, viewer.member_kind, viewer.listed`)
}

// The clauses below test one row against the viewer a statement binds as jsonb at viewerParam,
// in the shape of "the visibility argument": {"person", "session", "open", "all", "sees",
// "private"}. "all" is the server's own viewer (model.ServerViewer), which passes every clause.
//
// Each clause answers true or false, never NULL. A viewer that names no person or session, or a
// row whose key is NULL, makes a comparison NULL; a NULL clause passes no row in a positive WHERE,
// but its negation is NULL as well, so a rule written as `not <clause>` ("no recipient the viewer
// may not see") would admit the row. clause wraps each one so the negation fails closed too
// (TestEveryClauseAnswersTrueOrFalseNeverNull).
func clause(name, sql string) string { return fragment(name, "coalesce("+sql+", false)") }

// ViewerFlag is one boolean member of the visibility argument viewerParam binds - "all", "open" or
// "private" - false where it is absent. It is the one spelling of that read, which refs' rules
// share.
func ViewerFlag(viewerParam, flag string) string {
	return fmt.Sprintf(`coalesce((%s::jsonb->>'%s')::boolean, false)`, viewerParam, flag)
}

// ViewerPerson is the person of the visibility argument viewerParam binds, as text: null for a
// viewer that names no person, so an equality with it is null and passes no row.
func ViewerPerson(viewerParam string) string { return viewerParam + "::jsonb->>'person'" }

// ListsClause passes a row of the project projectExpr names when the credential of the viewer
// bound at viewerParam lists that project (Viewer.Lists); a credential that lists none passes no
// row.
func ListsClause(projectExpr, viewerParam string) string {
	return clause("ListsClause", "(("+viewerParam+"::jsonb->'lists') ? ("+projectExpr+"))")
}

// ProjectClause passes a row of the project projectExpr names when the viewer sees that project.
func ProjectClause(projectExpr, viewerParam string) string {
	return clause("ProjectClause", fmt.Sprintf(`(%s or (%s::jsonb->'sees') ? (%s))`,
		ViewerFlag(viewerParam, "all"), viewerParam, projectExpr))
}

// OpenClause passes an open viewer: one that is not a restricted member and holds no list, which
// reads every row that belongs to no project and to no conversation, as every caller did before a
// project could be private. The server's own viewer is open.
func OpenClause(viewerParam string) string {
	return clause("OpenClause", ViewerFlag(viewerParam, "open"))
}

// PersonClause passes a person, by login, to an open viewer; to any other viewer, when it is the
// viewer's own person or a person of a project the viewer sees: one that the rule lets see it.
// The projects are read from the viewer's own list, each by its key, so the clause probes the
// projects primary key once per project the viewer sees rather than reading every project. The
// person's member kind is read under an alias of its own (person_kind), so a loginExpr that names
// a column of the statement's own `people` row (people.email) is that row's and never the
// subquery's.
func PersonClause(loginExpr, viewerParam string) string {
	return clause("PersonClause", fmt.Sprintf(`(%[2]s or %[3]s or (%[1]s) = %[4]s::jsonb->>'person'
		or exists (
			select 1 from jsonb_array_elements_text(%[4]s::jsonb->'sees') as sees (key)
			join projects seen on seen.key = sees.key
			where %[5]s
			offset 0))`,
		loginExpr, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"), viewerParam,
		PersonSeesProjectSQL("("+loginExpr+")", "(select person_kind.member_kind from people person_kind where person_kind.email = ("+loginExpr+"))", "seen")))
}

// SessionClause passes a session, by id, when it is the viewer's own; to any other viewer, never
// when it acted in a private project the viewer does not see (a session_projects pair), whatever
// else it did, and otherwise to an open viewer, and to a viewer that is not open when it acted in a
// project the viewer sees. So an open member reads no session of a private project it is not on,
// and neither does a restricted member or a credential that lists projects, though the session
// acted in one of theirs too.
//
// A session with no pair, which has acted in no project, is judged by its listener record's
// projects (route_roster_session_projects, the migration's primary-key projection of the
// credential it registered with): it passes an open viewer when every project the record names is
// one the viewer sees, an empty list included, and a viewer that is not open when the record names
// a project and every project it names is one the viewer sees (K6), so a session that lists a
// project the viewer cannot see reaches no viewer of another. A record with no projects key is a
// listener that does not know the session's projects, never a credential that lists none: while a
// project is private, a session with neither a pair nor a listed record passes no viewer but its
// own, since nothing Dispatch holds says which projects it works for. A deployment that holds no
// private project skips the reads of the private pairs and of the listener records: every project
// is public, the open viewer sees each one, no pair is of a private project, and a viewer that is
// not open reads a session by its pairs alone, as before (R9).
//
// session_projects places every session in the projects it acted in only once
// `envoy-dispatch backfill-session-projects` has written session_projects_filled's row: migration
// 20261116081157_session_projects_proven empties both tables, and the command's --reset does again
// in a deployment that may already hold a private project. Until the row is written a session of
// a private project may have no row placing it there, so the open arm passes no session but the
// viewer's own while any project is private. The marker's read names no column of the row it
// tests, so a statement makes it once (an InitPlan), and only for an open viewer of a deployment
// that holds a private project.
//
// The clause is SQL of the statement that binds it, never a call of a database function: a SQL
// function whose body reads a table is not inlined (Postgres inlines a scalar SQL function only
// when its body holds no subquery), so it would run once per row it tests, with no private
// project as well, and EXPLAIN would not show what it reads. Written here, the planner plans its
// reads with the statement: the listener record's read names no column of the row either, so it
// too is one primary-key probe a statement makes (an InitPlan) at its first use, and a deployment
// that holds no private project reads nothing for an open viewer
// (TestTheListenerRecordUsesItsPrimaryKey).
func SessionClause(sessionExpr, viewerParam string) string {
	listed := `((select listener.projects from route_roster_session_projects listener where listener.id = true) -> (` + sessionExpr + `))`
	return clause("SessionClause", fmt.Sprintf(`case
		when %[3]s or (%[1]s) = %[2]s::jsonb->>'session' then true
		when %[4]s then case
			when not %[5]s then true
			when not exists (select 1 from session_projects_filled offset 0) then false
			when exists (
				select 1 from session_projects unseen
				where unseen.session_id = (%[1]s) and not ((%[2]s::jsonb->'sees') ? unseen.project_key)
				offset 0) then false
			when exists (select 1 from session_projects acted where acted.session_id = (%[1]s) offset 0) then true
			else coalesce(%[6]s <@ (%[2]s::jsonb->'sees'), false)
		end
		when exists (
			select 1 from session_projects acted
			where acted.session_id = (%[1]s) and (%[2]s::jsonb->'sees') ? acted.project_key
			offset 0) then
			not %[5]s or not exists (
				select 1 from session_projects unseen
				join projects hidden on hidden.key = unseen.project_key and hidden.visibility = 'private'
				where unseen.session_id = (%[1]s) and not ((%[2]s::jsonb->'sees') ? unseen.project_key)
				offset 0)
		when %[5]s and not exists (select 1 from session_projects acted where acted.session_id = (%[1]s) offset 0) then
			coalesce(%[6]s <> '[]'::jsonb and %[6]s <@ (%[2]s::jsonb->'sees'), false)
		else false
	end`,
		sessionExpr, viewerParam, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"),
		ViewerFlag(viewerParam, "private"), listed))
}

// roleProjectSQL is contracts.RoleProject in SQL, over the role name roleExpr gives: the project
// key a Legion role's name, legion-<token>-…, gives, its token of lower-case letters and digits
// upper-cased when that is a project key, and null for every other name. translate upper-cases the
// token the same in every locale, as strings.ToUpper does the ASCII a token holds.
func roleProjectSQL(roleExpr string) string {
	return `translate(substring((` + roleExpr + `) from '^legion-([a-z][a-z0-9]{1,9})-.'), 'abcdefghijklmnopqrstuvwxyz', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ')`
}

// RoleClause passes a role, by name, to no viewer but the server's when it works for a private
// project the viewer does not see (contracts.RoleProject: legion-<token>-…), whether or not a
// session holds it. Any other role it passes to an open viewer unless a session that holds it now
// (its held_by tuple not retracted since) does not pass SessionClause, and to any other viewer when
// a session that holds it now passes SessionClause. So a role held by a session of a private
// project the viewer does not see reaches no viewer that may not read that session. A deployment
// that holds no private project skips the read of the role's project and the open viewer's read of
// the holders: SessionClause then passes every session to it (R9).
func RoleClause(roleExpr, viewerParam string) string {
	session := SessionClause("holder.to_id", viewerParam)
	holder := func(condition string) string {
		return fmt.Sprintf(`exists (
			select 1 from graph_tuples holder
			where holder.from_kind = 'role' and holder.from_id = (%[1]s) and holder.kind = 'held_by'
				and holder.to_kind = 'session' and holder.asserted
				and not exists (
					select 1 from graph_tuples later
					where later.from_kind = holder.from_kind and later.from_id = holder.from_id
						and later.kind = holder.kind and later.to_kind = holder.to_kind
						and later.to_id = holder.to_id and later.seq > holder.seq
					offset 0)
				and %[2]s
			offset 0)`, roleExpr, condition)
	}
	return clause("RoleClause", fmt.Sprintf(`(%[1]s
		or (not (%[3]s and exists (
				select 1 from projects hidden
				where hidden.key = %[6]s and hidden.visibility = 'private'
					and not ((%[7]s::jsonb->'sees') ? hidden.key)
				offset 0))
			and ((%[2]s and (not %[3]s or not %[4]s)) or %[5]s)))`,
		ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"), ViewerFlag(viewerParam, "private"),
		holder("not "+session), holder(session), roleProjectSQL(roleExpr), viewerParam))
}

// PartyClause passes a conversation that belongs to no project, by its parties ("person:<login>",
// "session:<id>"), when the viewer's own person or session is a party, or the viewer is open, and
// every session party passes SessionClause.
func PartyClause(partiesExpr, viewerParam string) string {
	return clause("PartyClause", fmt.Sprintf(`(%[2]s
		or ((('person:' || (%[4]s::jsonb->>'person')) = any(%[1]s)
				or ('session:' || (%[4]s::jsonb->>'session')) = any(%[1]s)
				or %[3]s)
			and not exists (
				select 1 from unnest(%[1]s) party
				where party like 'session:%%' and not %[5]s
				offset 0)))`,
		partiesExpr, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"), viewerParam,
		SessionClause("substr(party, 9)", viewerParam)))
}

// Resolve answers what actor may see, in one read of VisibleProjectsSQL. actor is the request's
// caller as optionalActor resolved it, with its credential: one that carries none is refused with
// ErrUnresolvedActor before any read. The person is a browser's login or the person an agent
// credential acts for; a broker token is the credential whose session the broker proved; the list
// is the credential's Projects. A caller is a browser only when it is a person acting with no
// agent: a person-scope token acting for its person (actingPerson) is a user that carries an act,
// and it sees what its own list lets it see, never everything its person sees.
func Resolve(ctx context.Context, q Querier, actor model.Actor) (model.Viewer, error) {
	if actor.Credential == nil {
		return model.Viewer{}, ErrUnresolvedActor
	}
	var person *string
	login := ""
	switch {
	case actor.Kind == "user":
		login = model.CanonicalLogin(actor.ID)
	case actor.Owner != nil:
		login = model.CanonicalLogin(*actor.Owner)
	}
	if login != "" {
		person = &login
	}
	listed := actor.Credential.Projects
	broker := actor.Credential.Session != ""
	agent := actor.Kind != "user" || actor.Act != nil
	viewer := model.Viewer{Person: login}
	if len(listed) > 0 {
		viewer.Lists = listed
	}
	if actor.Kind == "session" {
		viewer.Session = actor.ID
	}
	if err := q.QueryRow(ctx, VisibleProjectsSQL(), person, broker, agent, listed).
		Scan(&viewer.Sees, &viewer.Restricted, &viewer.AnyPrivate); err != nil {
		return model.Viewer{}, fmt.Errorf("resolve what the caller sees: %w", err)
	}
	viewer.Open = !viewer.Restricted && len(viewer.Lists) == 0
	return viewer, nil
}
