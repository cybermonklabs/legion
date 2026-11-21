package access

import (
	"context"
	"testing"
)

// TestEveryClauseAnswersTrueOrFalseNeverNull holds each clause to a boolean answer for every viewer
// and every key, a missing one included. A clause that answers NULL passes no row in a positive
// WHERE, but its negation is NULL too, so `not <clause>` passes no row either: a rule written as
// "no recipient the viewer may not see" (`not exists (... where not SessionClause(...))`) then
// admits a row whose recipient the viewer may not see. Each case is read twice, as the clause and
// as its negation, and both must be a boolean.
func TestEveryClauseAnswersTrueOrFalseNeverNull(t *testing.T) {
	database := seedAccess(t)
	ctx := context.Background()
	viewers := map[string]string{
		"the closed viewer":                   `{"sees": [], "open": false, "all": false, "private": true}`,
		"a restricted member with no session": `{"person": "bob@other.example", "sees": ["PRIV"], "open": false, "all": false, "private": true}`,
		"a listing token of no person":        `{"sees": ["PUB"], "open": false, "all": false, "private": true}`,
	}
	clauses := map[string]string{
		"ProjectClause over a missing key":                        ProjectClause("null::text", "$1"),
		"ProjectClause over a hidden key":                         ProjectClause("'OTHER'", "$1"),
		"PersonClause over a missing login":                       PersonClause("null::text", "$1"),
		"PersonClause over a stranger":                            PersonClause("'stranger@example.com'", "$1"),
		"SessionClause over a missing session":                    SessionClause("null::text", "$1"),
		"SessionClause over another session":                      SessionClause("'session-of-nobody'", "$1"),
		"SessionContentClause over a missing session":             SessionContentClause("null::text", "$1"),
		"SessionContentClause over another session":               SessionContentClause("'session-of-nobody'", "$1"),
		"RoleClause over a missing role":                          RoleClause("null::text", "$1"),
		"RoleClause over a role nobody holds":                     RoleClause("'reviewer'", "$1"),
		"RoleProjectClause over a private key":                    RoleProjectClause("'PRIV'", "$1"),
		"HiddenPrivateProjectClause over a private key":           HiddenPrivateProjectClause("'PRIV'", "$1"),
		"PartyClause over no parties":                             PartyClause("null::text[]", "$1"),
		"PartyClause over another's parties":                      PartyClause("array['person:dave@example.com', 'session:session-of-nobody']", "$1"),
		"OpenClause":                                              OpenClause("$1"),
		"ListsClause over a missing key":                          ListsClause("null::text", "$1"),
		"ListsClause over a project no list names":                ListsClause("'OTHER'", "$1"),
		"IssueProjectClause over a missing key":                   IssueProjectClause("null::text", "$1"),
		"IssueProjectClause over no issue":                        IssueProjectClause("'NOPE-1'", "$1"),
		"ItemOwnerClause over no owner":                           ItemOwnerClause("null::text", "null::uuid", "$1"),
		"ItemOwnerClause over a missing document":                 ItemOwnerClause("null::text", "'00000000-0000-4000-8000-000000000000'::uuid", "$1"),
		"MessageClause over a message on no issue and of no node": MessageClause("message_row", "$1"),
		"MessageClause over a message on a missing issue":         MessageClause("missing_issue_row", "$1"),
		"DocumentClause over a document of no project or session": DocumentClause("document_row", "$1"),
		"BroadcastClause over a missing broadcast":                BroadcastClause("null::uuid", "$1"),
		"BroadcastClause over a broadcast nobody sent":            BroadcastClause("'00000000-0000-4000-8000-000000000003'::uuid", "$1"),
	}
	// Two message rows the MessageClause cases read, neither of which any table holds: one on no
	// issue whose node was never stated, and one on an issue nobody holds; and a document row that
	// names neither a project nor a session.
	const rows = ` from (select null::text as issue_key, '00000000-0000-4000-8000-000000000001'::uuid as id) message_row,
		(select 'NOPE-1'::text as issue_key, '00000000-0000-4000-8000-000000000002'::uuid as id) missing_issue_row,
		(select null::text as project_key, null::text as session_id) document_row`
	for viewerName, viewer := range viewers {
		for clauseName, clause := range clauses {
			for _, form := range []string{clause, "not " + clause} {
				var answer *bool
				if err := database.Pool.QueryRow(ctx, `select `+form+rows, viewer).Scan(&answer); err != nil {
					t.Fatalf("%s for %s: %v", clauseName, viewerName, err)
				}
				if answer == nil {
					t.Errorf("%s for %s answers NULL as `%.20s…`: a negation of it passes no row and fails nothing", clauseName, viewerName, form)
				}
			}
		}
	}
}
