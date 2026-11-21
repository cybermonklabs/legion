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
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

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

func viewerFlag(viewerParam, flag string) string {
	return fmt.Sprintf(`coalesce((%s::jsonb->>'%s')::boolean, false)`, viewerParam, flag)
}

// ProjectClause passes a row of the project projectExpr names when the viewer sees that project.
func ProjectClause(projectExpr, viewerParam string) string {
	return fragment("ProjectClause", fmt.Sprintf(`(%s or (%s::jsonb->'sees') ? (%s))`,
		viewerFlag(viewerParam, "all"), viewerParam, projectExpr))
}

// PersonClause passes a person, by login, to an open viewer; to any other viewer, when it is the
// viewer's own person or a person of a project the viewer sees: one that the rule lets see it.
func PersonClause(loginExpr, viewerParam string) string {
	return fragment("PersonClause", fmt.Sprintf(`(%[2]s or %[3]s or (%[1]s) = %[4]s::jsonb->>'person'
		or exists (
			select 1 from projects seen
			where (%[4]s::jsonb->'sees') ? seen.key
				and %[5]s
			offset 0))`,
		loginExpr, viewerFlag(viewerParam, "all"), viewerFlag(viewerParam, "open"), viewerParam,
		PersonSeesProjectSQL("("+loginExpr+")", "(select member_kind from people where email = ("+loginExpr+"))", "seen")))
}

// SessionClause passes a session, by id, to an open viewer; to any other viewer, when it is the
// viewer's own session or it has acted in a project the viewer sees (session_projects).
func SessionClause(sessionExpr, viewerParam string) string {
	return fragment("SessionClause", fmt.Sprintf(`(%[2]s or %[3]s or (%[1]s) = %[4]s::jsonb->>'session'
		or exists (
			select 1 from session_projects acted
			where acted.session_id = (%[1]s) and (%[4]s::jsonb->'sees') ? acted.project_key
			offset 0))`,
		sessionExpr, viewerFlag(viewerParam, "all"), viewerFlag(viewerParam, "open"), viewerParam))
}

// RoleClause passes a role, by name, to an open viewer; to any other viewer, when the session that
// holds the role now (its held_by tuple not retracted since) passes SessionClause.
func RoleClause(roleExpr, viewerParam string) string {
	return fragment("RoleClause", fmt.Sprintf(`(%[2]s or %[3]s
		or exists (
			select 1 from graph_tuples holder
			where holder.from_kind = 'role' and holder.from_id = (%[1]s) and holder.kind = 'held_by'
				and holder.to_kind = 'session' and holder.asserted
				and not exists (
					select 1 from graph_tuples later
					where later.from_kind = holder.from_kind and later.from_id = holder.from_id
						and later.kind = holder.kind and later.to_kind = holder.to_kind
						and later.to_id = holder.to_id and later.seq > holder.seq
					offset 0)
				and %[4]s
			offset 0))`,
		roleExpr, viewerFlag(viewerParam, "all"), viewerFlag(viewerParam, "open"), SessionClause("holder.to_id", viewerParam)))
}

// PartyClause passes a conversation that belongs to no project, by its parties ("person:<login>",
// "session:<id>"), to the viewer when its own person or session is a party, or to an open viewer
// when every session party passes SessionClause. A restricted member and a credential that lists
// projects so read only their own direct conversations.
func PartyClause(partiesExpr, viewerParam string) string {
	return fragment("PartyClause", fmt.Sprintf(`(%[2]s
		or ('person:' || (%[4]s::jsonb->>'person')) = any(%[1]s)
		or ('session:' || (%[4]s::jsonb->>'session')) = any(%[1]s)
		or (%[3]s and not exists (
			select 1 from unnest(%[1]s) party
			where party like 'session:%%' and not %[5]s
			offset 0)))`,
		partiesExpr, viewerFlag(viewerParam, "all"), viewerFlag(viewerParam, "open"), viewerParam,
		SessionClause("substr(party, 9)", viewerParam)))
}

// Resolve answers what actor may see, in one read of VisibleProjectsSQL. The person is a
// browser's login or the person an agent credential acts for; a broker token is the credential
// whose session the broker proved; the list is the credential's Projects.
func Resolve(ctx context.Context, q Querier, actor model.Actor) (model.Viewer, error) {
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
	var listed []string
	broker := false
	if actor.Credential != nil {
		listed = actor.Credential.Projects
		broker = actor.Credential.Session != ""
	}
	viewer := model.Viewer{Person: login, Listed: len(listed) > 0}
	if actor.Kind == "session" {
		viewer.Session = actor.ID
	}
	if err := q.QueryRow(ctx, VisibleProjectsSQL(), person, broker, actor.Kind != "user", listed).
		Scan(&viewer.Sees, &viewer.Restricted, &viewer.AnyPrivate); err != nil {
		return model.Viewer{}, fmt.Errorf("resolve what the caller sees: %w", err)
	}
	viewer.Open = !viewer.Restricted && !viewer.Listed
	return viewer, nil
}
