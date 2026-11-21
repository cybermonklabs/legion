package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PersonSeesProjectSQL is the rule for one person over one project row: true when the person,
// whose member kind memberExpr answers, sees the project projectAlias names. The group arm
// counts a member only through a people row of a full member, so a restricted member, or a login
// nobody has signed in as, is never admitted by a group (v24 R2). It is the access package's
// rule (access: the package comment), written here because the store's own writes of a person's
// member kind read it too (PgPeopleStore's SignInSubject and ConfirmSubject, RecordReachedProjects),
// and the access package imports this one. It starts with the access package's marker and its own
// name (access.Marker), as every fragment of that package does
// (access.TestEverySQLFragmentStartsWithTheMarker).
func PersonSeesProjectSQL(personExpr, memberExpr, projectAlias string) string {
	return PersonSeesProjectSQLIn("", personExpr, memberExpr, projectAlias)
}

// PersonSeesProjectSQLIn is PersonSeesProjectSQL with every table it reads named under schema: ""
// in a statement, which names its tables as the rest of the statement does, and "public." in a
// function's body (access: sessionContentFunctionBody, projectMadePrivateFunctionBody), which runs
// under its caller's search_path and so names each table by its schema, as session_clause_visible
// does, rather than setting one.
func PersonSeesProjectSQLIn(schema, personExpr, memberExpr, projectAlias string) string {
	return fmt.Sprintf(`/* project access: PersonSeesProjectSQL */ (%[3]s.owner = %[1]s
		or exists (
			select 1 from %[4]sproject_access listed
			where listed.project_key = %[3]s.key and listed.principal_kind = 'person' and listed.principal = %[1]s
			offset 0)
		or exists (
			select 1
			from %[4]sproject_access listed
			join %[4]saccess_group_members member on member.group_name = listed.principal
			join %[4]speople full_member on full_member.email = member.person and full_member.member_kind = 'full'
			where listed.project_key = %[3]s.key and listed.principal_kind = 'group' and member.person = %[1]s
			offset 0)
		or (%[3]s.visibility = 'public' and %[2]s = 'full'))`, personExpr, memberExpr, projectAlias, schema)
}

// SessionOwnerLock names the advisory lock, one per person by login, that orders a session's tie
// to that person (access.TieSession, which takes it shared) with a change that takes a project
// from them (RecordReachedProjects and the switch to private's trigger, which take it exclusive):
// a tie committed first is one the change reads, and a tie written after waits for the change to
// commit, so the request it was written for reads what the person sees after the change.
const SessionOwnerLock = "dispatch-session-owner"

// RecordReachedProjects writes, in tx, a pair (session, project) in session_reached_projects for
// every session tied to one of people (session_owners) and every project of keys that person does
// not see as tx left it (PersonSeesProjectSQL; a person no people row holds reads as a full member,
// as a broker token's person does in access.VisibleProjectsSQL). Its callers name only people who
// saw the project before the change: the access package's writes name the person taken off the
// list, a group's full members, the members taken out of a group and the owner the project was
// taken from; PgPeopleStore's writes of a member kind name the person whose kind they change, with
// the projects that person saw under the kind the row held (projectsSeenAs). A switch of a project
// to private runs the same two statements in its trigger (MadePrivateReachedSQL). A session's
// transcript holds what it read while its person saw the project, so the content rule reads each
// pair as a project the session acted in (access.SessionContentClause); nothing removes one.
//
// It first takes each person's SessionOwnerLock, exclusive, in login order, by the statement the
// trigger takes its locks with (lockSessionOwnersSQL), so two changes that lock the same people
// never wait on each other in a cycle; it is the last advisory lock its callers take. A pair is
// written only where no row holds it, as session_projects' are.
func RecordReachedProjects(ctx context.Context, tx pgx.Tx, keys, people []string) error {
	if len(keys) == 0 || len(people) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, lockSessionOwnersSQL("select", `from unnest($1::text[]) as tied (login)`), people); err != nil {
		return fmt.Errorf("lock the sessions of the people who lost a project: %w", err)
	}
	lost := lostProjectSQL("", `
		join projects lost on lost.key = any($1::text[])`, `tied.login = any($2::text[])`, "lost")
	if _, err := tx.Exec(ctx, insertReachedSQL("", "lost.key", lost), keys, people); err != nil {
		return fmt.Errorf("record the projects the sessions of the people who lost one reached: %w", err)
	}
	return nil
}

// MadePrivateReachedSQL is RecordReachedProjects for the trigger of a switch of a project to
// private (access: projectMadePrivateFunctionBody), as two statements of a PL/pgSQL body: the
// people are each person tied to a session who saw the project by its row as the update found it
// (old) and does not see it by its row after (new), and the project is new's. Every table is named
// under public, as a function's body names them.
func MadePrivateReachedSQL() string {
	lost := lostProjectSQL("public.", "", `coalesce(`+PersonSeesProjectSQLIn("public.", "tied.login", "coalesce(tied_person.member_kind, 'full')", "old")+`, false)`, "new")
	return lockSessionOwnersSQL("perform", lost) + `;
	` + insertReachedSQL("public.", "new.key", lost) + `;`
}

// lostProjectSQL is the from and where of the sessions (tied, with its person's row tied_person)
// of the people who lost the project projectAlias names: join brings that project in when the
// statement reads it from a table, who names the people, and the person no longer sees it
// (PersonSeesProjectSQLIn, a person no people row holds reading as a full member).
func lostProjectSQL(schema, join, who, projectAlias string) string {
	return `from ` + schema + `session_owners tied` + join + `
		left join ` + schema + `people tied_person on tied_person.email = tied.login
		where ` + who + `
			and not coalesce(` + PersonSeesProjectSQLIn(schema, "tied.login", "coalesce(tied_person.member_kind, 'full')", projectAlias) + `, false)`
}

// lockSessionOwnersSQL takes SessionOwnerLock, exclusive, for each distinct login people (a from
// and where over an alias tied with a login column) answers, in login order: verb is select in a
// statement and perform in a PL/pgSQL body.
func lockSessionOwnersSQL(verb, people string) string {
	return verb + ` count(pg_advisory_xact_lock(hashtext('` + SessionOwnerLock + `'), hashtext(lost.login)))
	from (select distinct tied.login ` + people + `
		order by tied.login offset 0) as lost`
}

// insertReachedSQL writes a pair (session, project) for each session lost (lostProjectSQL) names,
// with key the project's key, where no row holds the pair.
func insertReachedSQL(schema, key, lost string) string {
	return `insert into ` + schema + `session_reached_projects (session_id, project_key)
	select distinct tied.session_id, ` + key + ` ` + lost + `
		and not exists (
			select 1 from ` + schema + `session_reached_projects recorded
			where recorded.session_id = tied.session_id and recorded.project_key = ` + key + `)`
}
