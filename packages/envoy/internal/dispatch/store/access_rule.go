package store

import (
	"context"
	"fmt"
	"slices"

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
	return fmt.Sprintf(`/* project access: PersonSeesProjectSQL */ (%[3]s.owner = %[1]s
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
		or (%[3]s.visibility = 'public' and %[2]s = 'full'))`, personExpr, memberExpr, projectAlias)
}

// SessionOwnerLock names the advisory lock, one per person by login, that orders a session's tie
// to that person (access.TieSession, which takes it shared) with a change that takes a project
// from them (RecordReachedProjects, which takes it exclusive): a tie committed first is one the
// change reads, and a tie written after waits for the change to commit, so the request it was
// written for reads what the person sees after the change.
const SessionOwnerLock = "dispatch-session-owner"

// RecordReachedProjects writes, in tx, a pair (session, project) in session_reached_projects for
// every session tied to one of people (session_owners) and every project of keys that person does
// not see as tx left it (PersonSeesProjectSQL; a person no people row holds reads as a full member,
// as a broker token's person does in access.VisibleProjectsSQL). Its callers name only people who
// saw the project before the change: the access package's writes name the person taken off the
// list, a group's full members, the members taken out of a group and the owner the project was
// taken from; PgPeopleStore's writes of a member kind name the person whose kind they change, with
// the projects that person saw under the kind the row held (projectsSeenAs). A session's
// transcript holds what it read while its person saw the project, so the content rule reads each
// pair as a project the session acted in (access.SessionContentClause); nothing removes one.
//
// It first takes each person's SessionOwnerLock, exclusive, in login order, so two changes that
// lock the same people never wait on each other in a cycle; it is the last advisory lock its
// callers take. A pair is written only where no row holds it, as session_projects' are.
func RecordReachedProjects(ctx context.Context, tx pgx.Tx, keys, people []string) error {
	if len(keys) == 0 || len(people) == 0 {
		return nil
	}
	people = slices.Clone(people)
	slices.Sort(people)
	people = slices.Compact(people)
	if _, err := tx.Exec(ctx, `
		select count(pg_advisory_xact_lock(hashtext($1), hashtext(person)))
		from (select person from unnest($2::text[]) with ordinality as locked (person, position) order by position offset 0) as people
	`, SessionOwnerLock, people); err != nil {
		return fmt.Errorf("lock the sessions of the people who lost a project: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into session_reached_projects (session_id, project_key)
		select distinct tied.session_id, lost.key
		from session_owners tied
		join projects lost on lost.key = any($1::text[])
		left join people tied_person on tied_person.email = tied.login
		where tied.login = any($2::text[])
			and not coalesce(`+PersonSeesProjectSQL("tied.login", "coalesce(tied_person.member_kind, 'full')", "lost")+`, false)
			and not exists (
				select 1 from session_reached_projects recorded
				where recorded.session_id = tied.session_id and recorded.project_key = lost.key)
	`, keys, people); err != nil {
		return fmt.Errorf("record the projects the sessions of the people who lost one reached: %w", err)
	}
	return nil
}
