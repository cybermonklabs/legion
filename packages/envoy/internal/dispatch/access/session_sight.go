package access

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A session's sight: whether Dispatch may send a session an event of a project, or give it work
// of one. It is the package's one rule read from the session's side: what the person or the
// credential the session acts for sees, which is the most its agent may be shown. The outbox asks
// it of every session a destination of an event reaches on its own subject, and of a role's holder
// before a forward (outbox: destinations.withheld, resolveRoleHolders); the api asks it before a
// mention's or a targeted message's send, and before it routes an issue to a session, adds a
// session to an ask's followers, claims an issue in a session's name or records a comment's
// mention of a session.

// SessionSeesProjectClause passes the project row projectAlias names to the session sessionExpr
// names when that session may be shown it:
//
//   - The person limit. Every person the session is tied to (session_owners: the people its proved
//     writes and its broker-issued tokens acted for) sees the project (store.PersonSeesProjectSQL),
//     a tied person no people row holds reading as a full member, as a broker token's person reads
//     in VisibleProjectsSQL and as store.RecordReachedProjects reads the people tied to a session.
//     A session tied to two people is shown what both see.
//   - The credential limit, read from the session's listener record (route_roster_session_projects,
//     an answer that has not passed its expires_at, as SessionContentClause reads it): where that
//     record lists the session's projects, the list covers the project as CredentialCovers says -
//     a restricted member's list that names projects covers those alone, and every other list, a
//     restricted member's empty one among them, each public project and the private ones it names,
//     so a restricted member's empty list covers the public projects its person sees. Where it
//     lists none, a session tied to a person is bounded by that person's sight alone, as the
//     content rule bounds what such a session read; a session tied to nobody is a credential of no
//     person that lists none, which sees the public projects.
//
// A restricted member's session is the outside session this exists for: its person sees only the
// projects its access lists name, public ones included, and its credential only the one project
// its token lists. A deployment that holds no private project shows every session of a full
// member, and every session tied to nobody, every project (R9).
//
// It answers true or false, never NULL (clause), so its negation fails closed too.
func SessionSeesProjectClause(sessionExpr, projectAlias string) string {
	listed := `(select listener.projects from route_roster_session_projects listener
			join route_roster roster on roster.id = listener.id
			where listener.id = true and roster.expires_at > now())`
	return clause("SessionSeesProjectClause", fmt.Sprintf(`(not exists (
			select 1 from session_owners tied
			left join people tied_person on tied_person.email = tied.login
			where tied.session_id = %[1]s
			  and not coalesce(%[3]s, false)
			offset 0)
		and case when jsonb_typeof(%[4]s -> %[1]s) = 'array'
			then (%[4]s -> %[1]s) ? %[2]s.key
				or (%[2]s.visibility = 'public' and ((%[4]s -> %[1]s) = '[]'::jsonb or not exists (
					select 1 from session_owners tied
					join people tied_person on tied_person.email = tied.login and tied_person.member_kind = 'restricted'
					where tied.session_id = %[1]s
					offset 0)))
			else %[2]s.visibility = 'public'
				or exists (select 1 from session_owners tied where tied.session_id = %[1]s offset 0)
			end)`,
		sessionExpr, projectAlias,
		store.PersonSeesProjectSQL("tied.login", "coalesce(tied_person.member_kind, 'full')", projectAlias),
		listed))
}

// SightListSQL is two boolean columns of a select, for the session sessionExpr names over the
// project row projectAlias names. The first is whether that pair's verdict of
// SessionSeesProjectClause can depend on a listener list: the project is private, or the session is
// tied to a restricted member, the two cases where the clause answers otherwise with a list than
// without one. The second is whether the stored listener record (route_roster) holds an answer
// that has not passed its expires_at; a silence the API stored for a listener that did not answer
// lists no session, so it is no answer. The clause reads a record past its expires_at as no list
// at all, and read in the clause's own statement, on its one now(), the second column says whether
// the clause had a list to read, so it needs no margin: a reader whose verdict of a pair depends on
// a list and finds the record expired refreshes it and reads again before it judges that pair (the
// outbox's ListenerRefresh, the api's refreshedSessionSight).
func SightListSQL(sessionExpr, projectAlias string) string {
	return fmt.Sprintf(`%[2]s.visibility = 'private'
		or exists (select 1 from session_owners tied
			join people tied_person on tied_person.email = tied.login and tied_person.member_kind = 'restricted'
			where tied.session_id = %[1]s),
		coalesce((select answered and expires_at > now() from route_roster), false)`,
		sessionExpr, projectAlias)
}

// SightMayDependOnAListSQL is a select of one boolean: whether a verdict of
// SessionSeesProjectClause can depend on a listener list anywhere in the deployment - a project is
// private, or a person is a restricted member - SightListSQL's first column over every pair at
// once. A reader that judges a session it cannot name before its transaction opens refreshes the
// stored listener record first only where it holds (the api's refreshRosterBeforeSight).
const SightMayDependOnAListSQL = `select exists (select 1 from projects where visibility = 'private')
	or exists (select 1 from people where member_kind = 'restricted')`

// RowsQuerier is the one read ReadSessionSight makes: a pool, a transaction, or a reader that runs
// a query, such as the outbox's lookup reader.
type RowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SessionSightSQL is ReadSessionSight's one statement: a row for each pair of a session of $1 and
// a project row of $2, with whether the session sees the project (SessionSeesProjectClause) beside
// SightListSQL's two columns for that pair. A key no project row holds pairs with no session. A
// census of the statements a reader sends names it by this text.
var SessionSightSQL = `
	select p.key, s.id, ` + SessionSeesProjectClause("s.id", "p") + `, ` + SightListSQL("s.id", "p") + `
	from projects p
	cross join unnest($1::text[]) as s(id)
	where p.key = any($2::text[])`

// SessionSight is what one read of sessions' sight answers (ReadSessionSight). Sees is, for each
// project key, the sessions that see it (SessionSeesProjectClause). ListExpired is, for each
// project key, the sessions whose verdict of it could depend on a listener list while the stored
// listener record held no answer that had not expired (SightListSQL), so the clause judged that
// pair by the session's ties alone: a reader that must not judge by a list it could not read
// refreshes the record and reads again (the outbox's ListenerRefresh, the api's
// refreshedSessionSight), and judges no pair it still holds.
type SessionSight struct {
	Sees        map[string]map[string]bool
	ListExpired map[string]map[string]bool
}

// ReadSessionSight answers, in one statement (SessionSightSQL), which of sessions see which of
// projects, and which of those verdicts depended on a list of a listener record that had expired.
// A session or a project it names no pair for sees, or is seen by, nothing. With no session or no
// project it reads nothing, and no verdict depends on a list.
func ReadSessionSight(ctx context.Context, q RowsQuerier, sessions, projects []string) (SessionSight, error) {
	sight := SessionSight{Sees: map[string]map[string]bool{}, ListExpired: map[string]map[string]bool{}}
	if len(sessions) == 0 || len(projects) == 0 {
		return sight, nil
	}
	rows, err := q.Query(ctx, SessionSightSQL, sessions, projects)
	if err != nil {
		return SessionSight{}, fmt.Errorf("read which sessions see which projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, session string
		var sees, depends, fresh bool
		if err := rows.Scan(&project, &session, &sees, &depends, &fresh); err != nil {
			return SessionSight{}, fmt.Errorf("read which sessions see which projects: %w", err)
		}
		if sees {
			markPair(sight.Sees, project, session)
		}
		if depends && !fresh {
			markPair(sight.ListExpired, project, session)
		}
	}
	if err := rows.Err(); err != nil {
		return SessionSight{}, fmt.Errorf("read which sessions see which projects: %w", err)
	}
	return sight, nil
}

// markPair records session under project in pairs, either of SessionSight's maps.
func markPair(pairs map[string]map[string]bool, project, session string) {
	if pairs[project] == nil {
		pairs[project] = map[string]bool{}
	}
	pairs[project][session] = true
}

// SessionsSeeProjects answers, in one statement (ReadSessionSight), which of sessions see which of
// projects: for each project key, the sessions that see it. A session or a project it names no
// pair for sees, or is seen by, nothing.
func SessionsSeeProjects(ctx context.Context, q RowsQuerier, sessions, projects []string) (map[string]map[string]bool, error) {
	sight, err := ReadSessionSight(ctx, q, sessions, projects)
	return sight.Sees, err
}

// SessionSeesProject reports, in one statement (ReadSessionSight), whether session sees project
// (SessionSeesProjectClause), and whether that verdict depended on a list of a listener record
// that had expired (SessionSight.ListExpired). A read that fails answers false to both.
func SessionSeesProject(ctx context.Context, q RowsQuerier, session, project string) (sees, listExpired bool, err error) {
	sight, err := ReadSessionSight(ctx, q, []string{session}, []string{project})
	if err != nil {
		return false, false, err
	}
	return sight.Sees[project][session], sight.ListExpired[project][session], nil
}
