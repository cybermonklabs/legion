// Package access is Dispatch's one rule of who may see a project, and the reads and
// writes of what that rule reads: each project's owner and visibility, its access list of people
// and groups, the groups, and each person's member kind.
//
// The rule. A person P with member kind M sees project p when p's owner is P, when p's access
// list names P, when M is full and a group p's list names has P as a member whose people row is
// a full member's, or when p is public and M is full. A browser sees what its person sees. An
// agent credential sees what its person sees and its list covers (CredentialCovers): a full
// member's list decides only which private projects it sees, so it sees each public project its
// person sees and the private ones it lists; a restricted member's list narrows what its person
// sees to the projects it lists, and with no list it sees the public part of what its person sees.
// A credential with no person sees the public projects and the private projects it lists. A viewer
// is open when it is not a restricted member: it reads every row that belongs to no project, as
// every caller did before.
//
// One home. The rule is written once in SQL (store.PersonSeesProjectSQL, which the store's own
// writes of a member kind read too, and VisibleProjectsSQL) and once in Go over a person already
// resolved (CredentialCovers, Credential.Sees), and a test holds the two equal over every
// combination. The outside sign-in's question, whether some project names an email directly, is
// listed.go's, the same rule's direct arm read from the other side. Every SQL fragment this
// package hands a statement, store.PersonSeesProjectSQL among them, starts with Marker and its own
// name, so a statement that binds one says so wherever its text is read.
package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// VisibleProjectsSQL answers one caller's viewer in one row: every project it sees as a sorted
// jsonb array, whether it is a restricted member, whether any project is private, and whether
// Resolve answers its list as Viewer.Lists. Its parameters are $1 the person (null for a credential that names
// none), $2 whether the credential is a broker token, whose person reads as a full member when no
// people row holds them (the broker signs in only people of its sign-in domain), $3 whether it is
// an agent credential rather than a browser, and $4 its list of projects (null or empty for none).
//
// A project passes when the person sees it (store.PersonSeesProjectSQL; a credential of no person
// has no person's limit) and the credential's list covers it, which is CredentialCovers in SQL: a
// browser is covered everywhere; a restricted member's credential that lists projects, for those
// alone; and every other credential for each public project and each private one it lists.
// TestTheListRuleIsOneInGoAndSQL holds the two equal over every combination. With no private
// project a full member's list decides nothing it reads, and the caller sees what a credential
// listing none sees (R9). Resolve answers the list as Viewer.Lists for every credential but a
// broker token in every state, and for a broker token while some project is private or its person
// is a restricted member: a broker token's list existed before projects could be private, when its
// caller was answered no list (R9), and a personal token's list is new with private projects.
func VisibleProjectsSQL() string {
	return fragment("VisibleProjectsSQL", `
		with deployment as (
			select exists (select 1 from projects where visibility = 'private') as private
		), person as (
			select $1::text as person,
				case when $1::text is null then null
					else coalesce(
						(select member_kind from people where email = $1::text),
						case when $2::boolean then 'full' end)
				end as member_kind
		), viewer as (
			select person.person, person.member_kind,
				coalesce(person.member_kind = 'restricted', false) as restricted,
				coalesce(cardinality($4::text[]), 0) > 0 as listed,
				deployment.private
			from deployment, person
		)
		select
			coalesce(jsonb_agg(p.key order by p.key collate "C") filter (where
				(viewer.person is null or `+store.PersonSeesProjectSQL("viewer.person", "viewer.member_kind", "p")+`)
				and ((viewer.person is not null and not $3::boolean)
					or case when viewer.restricted and viewer.listed
						then p.key = any($4::text[])
						else p.visibility = 'public' or coalesce(p.key = any($4::text[]), false)
					end)), '[]'::jsonb),
			viewer.restricted,
			viewer.private,
			viewer.listed and (not $2::boolean or viewer.private or viewer.restricted)
		from viewer
		left join projects p on true
		group by viewer.person, viewer.member_kind, viewer.restricted, viewer.listed, viewer.private`)
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

// HiddenPrivateProjectClause passes a row of the project keyExpr names when that project is
// private and the viewer does not see it: the projects whose ownership of a repository is to hide it
// from the viewer, in a read a later slice adds. It probes the projects primary key once per row: its
// `exists` ends in `offset 0`, without which Postgres hashes every private project once per
// statement in its place.
func HiddenPrivateProjectClause(keyExpr, viewerParam string) string {
	return clause("HiddenPrivateProjectClause", `(not `+ProjectClause(keyExpr, viewerParam)+` and exists (
		select from projects hiding where hiding.key = (`+keyExpr+`) and hiding.visibility = 'private'
		offset 0))`)
}

// OpenClause passes an open viewer: one that is not a restricted member, whatever its credential
// lists, which reads every row that belongs to no project and to no conversation, as every caller
// did before a project could be private. The server's own viewer is open.
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
		store.PersonSeesProjectSQL("("+loginExpr+")", "(select person_kind.member_kind from people person_kind where person_kind.email = ("+loginExpr+"))", "seen")))
}

// SessionClause passes a session, by id, when it is the viewer's own; to any other viewer, never
// when it acted in a private project the viewer does not see (a session_projects pair), whatever
// else it did, and otherwise to an open viewer, and to a viewer that is not open when it acted in a
// project the viewer sees. So an open member reads no session of a private project it is not on,
// and neither does a restricted member, though the session acted in one of theirs too.
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
// session_projects places every session in the projects it acted in only once the backfill a later
// slice adds has written session_projects_filled's row: migration
// 20261116081157_session_projects_proven empties both tables, and the backfill's reset does again
// in a deployment that may already hold a private project, then refills the pairs one by one.
// Until the row is written a session of a private project may have no row placing it there, or
// have its pair in a project the viewer sees back before its pair in one the viewer does not, so
// while any project is private no arm passes a session but the viewer's own: the open arm and the
// arm of a viewer that is not open alike (migration 20261121165416_session_clause_filled). The
// marker's read names no column of the row it tests, so a statement makes it once (an InitPlan),
// and only for a viewer of a deployment that holds a private project that the statement's own SQL
// does not answer.
//
// The server's viewer, the viewer's own session and an open viewer of a deployment that holds no
// private project are answered in the statement's own SQL, so those rows call nothing and read
// nothing (R9). Every other row asks session_clause_visible (the migration's function, which names
// each table by its schema and sets no search_path). Postgres never inlines that function, since
// its body reads tables, and runs it once per row it asks about; its declared cost is what keeps
// the statements that bind this clause on their indexes, which the same decision written into the
// statement does not (refs.TestBacklinkCountStatementReadsEveryArmThroughAnIndex). What one call
// runs is in TestTheListenerRecordUsesItsPrimaryKey: the listener record read through its primary
// key.
func SessionClause(sessionExpr, viewerParam string) string {
	return clause("SessionClause", fmt.Sprintf(`(%[3]s or (%[1]s) = %[2]s::jsonb->>'session'
		or (%[4]s and not %[5]s)
		or session_clause_visible((%[1]s), %[2]s::jsonb,
			case when %[5]s then exists (
				select 1 from session_projects_filled
				offset 0)
			else true end))`,
		sessionExpr, viewerParam, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"),
		ViewerFlag(viewerParam, "private")))
}

// SessionContentClause passes a session whose whole conversation the viewer bound at viewerParam
// may read: the transcript the live agent view relays, whose tool results hold whatever the
// session read. A read records nothing (session_projects counts only what a session wrote), so no
// rule over what a session wrote bounds what its transcript holds. It is the rule of a session's
// content: the agent view's, and PartyClause's for every session party of a conversation, whose
// files and messages can hold what the session read too. The agents list keeps SessionClause,
// whose rows carry no content (K6).
//
//   - The server's viewer and the viewer's own session pass, and every session passes an open
//     viewer of a deployment that holds no private project (R9).
//   - Every other viewer is relayed no session that acted in or reached a project it does not
//     see: a session_projects pair, or a session_reached_projects pair, which a change that took
//     the project from a person the session is tied to wrote (store.RecordReachedProjects: an
//     access change, or a write of the person's member kind; a switch to private writes the same
//     pairs through its trigger, projectMadePrivateFunctionBody), since the transcript keeps what
//     the session read while its person saw the project, and the rules below judge the session's
//     people by what they see now.
//   - A viewer that is not open (a restricted member) is relayed only a session that its own
//     person's credential alone proved: every person a write or a broker-issued token of the
//     session recorded (session_owners, the person the token acts for) is the viewer's person, and
//     one is. Such a session acts with a credential of the viewer's person, which reaches only what
//     that person sees. Where the listener lists the session's projects, each must be one the
//     viewer sees. Where it lists none, nothing bounds what the session read but its person's
//     sight, so it passes only a viewer whose credential sees every project its person sees (the
//     person's browser, or a token whose list names each): a restricted member's credential is
//     held to its list for every project, public ones too, and one that sees fewer projects than
//     its person must not read what a session on a wider credential read, a public project made
//     private since included, which no pair records while the person still sees it.
//     A session tied to nobody, or one also tied to another person, answers as one nobody holds,
//     whatever else it did; another person's credential acting in it ties it to them, which the
//     live view's re-ask before the replay and before each batch of frames reads.
//   - An open viewer, while a project is private, is relayed a session that wrote in no project
//     the viewer does not see, once the later slice's backfill has filled the pairs
//     (session_projects_filled). Where the listener record lists the session's credential's
//     projects, each must be one the viewer sees ([] included), and the session must have written
//     in a project or list one. That list is the whole rule for such a session: a full member's
//     credential reads a private project only where its list names it (CredentialCovers), and
//     what it read of a public project was every full member's to read then. A switch to private
//     hides what is written in the project from the switch on; it does not withdraw what sessions
//     read while the project was public. Where it lists none (the listener sent no list, as every
//     listener does until it learns each session's projects), the session's people bound what its
//     credential could read: a proved write must have tied it to somebody (session_owners), and
//     every private project each person tied to it sees (store.PersonSeesProjectSQL) must be one
//     the viewer sees, which holds a full member's credential that sees fewer private projects
//     than its person off that person's own sessions. So a session only people on no private
//     project proved is watched as before, one a private project's person proved only by a viewer
//     who sees that project too, and one with neither a list nor a tie by nobody.
//
// A listener list counts only from a stored listener answer (route_roster) that has not passed its
// expires_at; a session of an expired answer is judged as one whose listener sent no list. The
// reader a later slice adds, which asks this rule again over time, is to refresh the stored answer
// first, and a refresh that fails leaves it expired, which fails closed.
//
// The server's viewer, the viewer's own session and an open viewer of a deployment that holds no
// private project are answered in the statement's own SQL, so those rows call nothing and read
// nothing (R9: with no private project an open viewer reads every session's content, as before).
// Every other row asks session_content_visible, the migration's function whose body is
// sessionContentFunctionBody, once per row, as SessionClause asks session_clause_visible: its
// declared cost keeps the statements that bind this clause on their indexes, which the same rule
// written into the statement does not (refs.TestBacklinkCountStatementReadsEveryArmThroughAnIndex
// names the arms such a statement moves off their indexes).
func SessionContentClause(sessionExpr, viewerParam string) string {
	return clause("SessionContentClause", fmt.Sprintf(`(%[3]s or (%[1]s) = %[2]s::jsonb->>'session'
		or (%[4]s and not %[5]s)
		or session_content_visible((%[1]s), %[2]s::jsonb))`,
		sessionExpr, viewerParam, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"),
		ViewerFlag(viewerParam, "private")))
}

// sessionContentFunctionBody is the body of session_content_visible(session_id text, viewer
// jsonb), the migration's function: SessionContentClause's decision for the session $1 and the
// visibility argument $2 for every viewer the clause's own SQL does not answer first, the rule
// SessionContentClause's comment states. It is written here, beside the clause whose rule it is,
// over store.PersonSeesProjectSQLIn, and the migration holds it as this text renders it:
// TestTheContentFunctionIsTheBodyGoRenders fails while the function a migrated database holds
// differs from it, so a change of either rule here ships with a migration that replaces the
// function. The function runs under its caller's search_path and sets none, which would save and
// restore the setting on every call, so every table it reads is named by its schema, as
// session_clause_visible names its tables.
//
// The listener record it reads is the projection of route_roster's one row, which serves reads
// only until its expires_at (routeRosterTTL past the listener's answer, by the database's clock).
// A row past it is a listener answer nobody has asked again, whose lists may be older than a
// session's credential is: a token traded again with a private project is still listed without
// it. So a record past expires_at is no list at all: the session is judged as one whose listener
// sent none, by the people its writes tied it to, and a session tied to nobody passes no viewer but
// its own.
func sessionContentFunctionBody() string {
	held := `(select listener.projects from public.route_roster_session_projects listener join public.route_roster roster on roster.id = listener.id where listener.id = true and roster.expires_at > now())`
	open := ViewerFlag("$2", "open")
	// ownOnly is a restricted viewer's arm: the viewer's own person is tied to the session, and
	// nobody else is.
	ownOnly := fmt.Sprintf(`(exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login = %[1]s
				offset 0)
			and not exists (
				select 1 from public.session_owners tied
				where tied.session_id = $1 and tied.login is distinct from %[1]s
				offset 0))`, ViewerPerson("$2"))
	// coversItsPerson is the restricted viewer's test of its own credential, for a session whose
	// listener sent no list: every project the viewer's person sees is one the viewer sees.
	coversItsPerson := fmt.Sprintf(`not exists (
				select 1 from public.projects seen
				left join public.people viewer_person on viewer_person.email = %[1]s
				where not (($2->'sees') ? seen.key) and %[2]s
				offset 0)`,
		ViewerPerson("$2"), store.PersonSeesProjectSQLIn("public.", ViewerPerson("$2"), "viewer_person.member_kind", "seen"))
	return fmt.Sprintf(`select coalesce(not exists (
				select 1 from public.session_projects unseen
				where unseen.session_id = $1 and not (($2->'sees') ? unseen.project_key)
				offset 0)
			and not exists (
				select 1 from public.session_reached_projects reached
				where reached.session_id = $1 and not (($2->'sees') ? reached.project_key)
				offset 0)
			and case when coalesce(%[2]s ? $1, false) then
				(%[2]s->$1) <@ ($2->'sees')
				and ((%[1]s
						and exists (select 1 from public.session_projects_filled offset 0)
						and (jsonb_array_length(%[2]s->$1) > 0
							or exists (select 1 from public.session_projects acted where acted.session_id = $1 offset 0)))
					or (not %[1]s and %[4]s))
			else
				(%[1]s
					and exists (select 1 from public.session_projects_filled offset 0)
					and exists (select 1 from public.session_owners tied where tied.session_id = $1 offset 0)
					and not exists (
						select 1 from public.session_owners tied
						join public.projects hidden on hidden.visibility = 'private' and not (($2->'sees') ? hidden.key)
						left join public.people tied_person on tied_person.email = tied.login
						where tied.session_id = $1 and %[3]s
						offset 0))
				or (not %[1]s and %[4]s and %[5]s)
			end, false)`,
		open, held, store.PersonSeesProjectSQLIn("public.", "tied.login", "tied_person.member_kind", "hidden"), ownOnly, coversItsPerson)
}

// roleProjectSQL is contracts.RoleProject in SQL, over the role name roleExpr gives: the project
// key a Legion role's name, legion-<token>-…, gives, its token of lower-case letters and digits
// upper-cased when that is a project key, and null for every other name. translate upper-cases the
// token the same in every locale, as strings.ToUpper does the ASCII a token holds.
func roleProjectSQL(roleExpr string) string {
	return `translate(substring((` + roleExpr + `) from '^legion-([a-z][a-z0-9]{1,9})-.'), 'abcdefghijklmnopqrstuvwxyz', 'ABCDEFGHIJKLMNOPQRSTUVWXYZ')`
}

// RoleProjectClause passes the project key keyExpr gives - the project a Legion role's name works
// for (contracts.RoleProject; roleProjectSQL reads it off a role's name) - unless it is a private
// project the viewer does not see (ProjectClause). A null key, a key no project holds and a public
// project's key pass, and a deployment that holds no private project reads no project: the
// viewer's "private" flag is false there, so the clause is true before it reads a row (R9).
//
// It is the one spelling of the half of a role's rule that reads the role's name. RoleClause
// splices it beside the role's held_by holders; the api's reads splice it where a role's holder
// comes from the Envoy listener's answer instead of a tuple (a route's reach, a delivery's
// resolve), each keeping its own holder lookup.
func RoleProjectClause(keyExpr, viewerParam string) string {
	return clause("RoleProjectClause", fmt.Sprintf(`(not (%[2]s and exists (
		select 1 from projects hidden
		where hidden.key = (%[1]s) and hidden.visibility = 'private'
			and not %[3]s
		offset 0)))`,
		keyExpr, ViewerFlag(viewerParam, "private"), ProjectClause("hidden.key", viewerParam)))
}

// RoleClause passes a role, by name, to no viewer but the server's when it works for a private
// project the viewer does not see (RoleProjectClause over contracts.RoleProject: legion-<token>-…),
// whether or not a session holds it. Any other role it passes to a viewer when a session that holds
// it now (its held_by tuple not retracted since) passes SessionClause for that viewer, and to an
// open viewer as well when no session that holds it now fails SessionClause, a role nobody holds
// included. So a role held only by sessions of a private project the viewer does not see reaches no
// viewer that may not read one of them, and a role one of whose holders the viewer may read reaches
// it, the hidden holder staying hidden wherever holders are listed. A deployment that holds no
// private project skips the read of the role's project and the open viewer's read of the holders:
// SessionClause then passes every session to it (R9).
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
		or (%[6]s
			and ((%[2]s and (not %[3]s or not %[4]s)) or %[5]s)))`,
		ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"), ViewerFlag(viewerParam, "private"),
		holder("not "+session), holder(session), RoleProjectClause(roleProjectSQL(roleExpr), viewerParam)))
}

// PartyClause passes a conversation that belongs to no project, by its parties ("person:<login>",
// "session:<id>"), when the viewer's own person or session is a party, or the viewer is open, or
// every party is a session, and every session party passes both SessionClause and
// SessionContentClause. Every such conversation is a session's content: a file of its conversation
// and that file's node, an ask or a comment on such a file, and a direct message to or from it with
// every reply under it. What a session writes there can hold whatever it read, which no rule over
// what it wrote bounds, so each session party is held to the rule the live agent view keeps for the
// session's transcript too. The content rule only narrows (SessionClause still holds), and the
// agents list, whose rows carry no content, keeps SessionClause alone (K6). An open viewer of a
// deployment that holds no private project passes SessionContentClause for every session in its
// SQL's own `open and not private` arm, as it passes SessionClause, so it reads every conversation
// as before (R9). A viewer that is not open reads one with a session only where its own person
// alone proved that session, as its agent view watches it: its direct message to the session, and
// the session's files and their nodes, whose parties are the session alone. Another person's write
// as the session ties the session to them too, and from that write on each answers as one nobody
// holds.
//
// A conversation that names no party - null parties, a direct message whose node Dispatch does not
// hold, or an empty list - says nothing of who may read it: it passes an open viewer while no
// project is private, as every conversation did before one could be (R9), and no viewer but the
// server's once one is, as a node naming neither a project nor parties does (refs' unscoped rule).
// The switch to private refuses while a direct message is one of them (api's
// privateProjectIncomplete).
func PartyClause(partiesExpr, viewerParam string) string {
	return clause("PartyClause", fmt.Sprintf(`(%[2]s
		or ((('person:' || (%[4]s::jsonb->>'person')) = any(%[1]s)
				or ('session:' || (%[4]s::jsonb->>'session')) = any(%[1]s)
				or (%[3]s and (not %[6]s or coalesce(cardinality(%[1]s), 0) > 0))
				or (not %[3]s and coalesce(cardinality(%[1]s), 0) > 0 and not exists (
					select 1 from unnest(%[1]s) party
					where party not like 'session:%%'
					offset 0)))
			and not exists (
				select 1 from unnest(%[1]s) party
				where party like 'session:%%' and not (%[5]s and %[7]s)
				offset 0)))`,
		partiesExpr, ViewerFlag(viewerParam, "all"), ViewerFlag(viewerParam, "open"), viewerParam,
		SessionClause("substr(party, 9)", viewerParam), ViewerFlag(viewerParam, "private"),
		SessionContentClause("substr(party, 9)", viewerParam)))
}

// Resolve answers what actor may see, in one read of VisibleProjectsSQL. actor is the request's
// caller as optionalActor resolved it, with its credential: one that carries none is refused with
// ErrUnresolvedActor before any read. The person is a browser's login or the person an agent
// credential acts for; a broker token is the credential whose session the broker proved; the list
// is the credential's Projects. A caller is a browser only when it is a person acting with no
// agent: a person-scope token acting for its person (actingPerson) is a user that carries an act,
// and it sees what its own list lets it see, never everything its person sees.
//
// A list decides what its credential reads (CredentialCovers): for a restricted member's
// credential, every project it sees, whatever the deployment holds (R3), so it sees exactly the
// projects its list and its person both see; for every other credential, only which private
// projects it sees, so with no private project it reads as a credential that lists none, exactly as
// it read before (R9), and once one is private it sees each public project and the private ones it
// lists. Viewer.Lists is the credential's list as a later slice's whoami tells the caller it and
// its resume reads it: every credential's but a broker token's, whose list is answered only while
// some project is private or its person is a restricted member, as it was before (R9). Open is
// every viewer that is not a restricted member's.
// What a broker token scoped to one project writes is decided outside this package, by the write
// scope such a token had before projects could be private, which no list here decides.
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
	if actor.Kind == "session" {
		viewer.Session = actor.ID
	}
	var held bool
	if err := q.QueryRow(ctx, VisibleProjectsSQL(), person, broker, agent, listed).
		Scan(&viewer.Sees, &viewer.Restricted, &viewer.AnyPrivate, &held); err != nil {
		return model.Viewer{}, fmt.Errorf("resolve what the caller sees: %w", err)
	}
	if held {
		viewer.Lists = listed
	}
	viewer.Open = !viewer.Restricted
	return viewer, nil
}

// RecordedViewer answers what the person a stored row records as its author may see now, in one
// read of VisibleProjectsSQL: the person a user author names, or a session author's owner, read as
// that person at a browser (their member kind and every project access names them on, restricted
// or full, as Resolve reads them, and no credential's list). It is not Resolve, which takes the
// request's own caller with its credential: it is the viewer refs.Reader resolves the short ids of
// a stored text for, at a write and at every rebuild of that text alike (refs.Reader.seenBy), so it
// reads no credential and sees no more than the person does. A session author, and a user author
// that acted through a token (Act), are read as their person too, not as an agent credential
// naming no list, which sees no private project: the row stores no credential a rebuild could read
// again, and such a credential would never resolve a short id of a private project's nodes for an
// agent of a person on it, nor keep, at a rebuild, an edge resolved while the project was public.
// A reader still sees a reference only through its own visibility. An author that records no
// person sees what a credential naming nobody sees: the public projects.
func RecordedViewer(ctx context.Context, q Querier, recorded model.Actor) (model.Viewer, error) {
	login := ""
	switch {
	case recorded.Kind == "user":
		login = model.CanonicalLogin(recorded.ID)
	case recorded.Owner != nil:
		login = model.CanonicalLogin(*recorded.Owner)
	}
	var person *string
	if login != "" {
		person = &login
	}
	viewer := model.Viewer{Person: login}
	if recorded.Kind == "session" {
		viewer.Session = recorded.ID
	}
	var held bool
	if err := q.QueryRow(ctx, VisibleProjectsSQL(), person, false, false, []string(nil)).
		Scan(&viewer.Sees, &viewer.Restricted, &viewer.AnyPrivate, &held); err != nil {
		return model.Viewer{}, fmt.Errorf("resolve what a recorded author sees: %w", err)
	}
	viewer.Open = !viewer.Restricted
	return viewer, nil
}
