package access

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestDocumentClauseAnswersAsItemOwnerClauseOverEveryDocument: DocumentClause, which reads a
// document's own row, answers every seeded viewer exactly as ItemOwnerClause does over the same
// document by its issue and its id - an issue's spec in PRIV, a document of PUB, a file of an
// agent's conversation whose session acted in PRIV, and a file of carol's session that acted in PUB
// alone - since a document's project_key is its issue's project, written with the row and never
// changed after. And it answers the rule: bob, bob's token and carol's token read PRIV's spec,
// alice does not; alice and carol's token, a full member's, whose list decides only which private
// projects it reads, read PUB's document, bob and his token do not; the conversation's file, whose
// session acted in PRIV, is read by carol's token, an open viewer that sees PRIV, and by none of
// the others, its session's own conversation, and by the server, which reads every one.
//
// A file can hold whatever its session read, so it follows the session content rule
// (SessionContentClause), not the agents list's: carol proved the PUB session and carol sees PRIV,
// so alice, who lists that session among the agents (SessionClause), does not read its file, and
// carol's token does. With no private project alice reads both files, as before (R9).
func TestDocumentClauseAnswersAsItemOwnerClauseOverEveryDocument(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into session_projects (session_id, project_key) values ('ses_priv', 'PRIV'), ('ses_pub_carol', 'PUB');
		insert into session_owners (session_id, login) values ('ses_priv', 'carol@example.com'), ('ses_pub_carol', 'carol@example.com');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"olga@example.com"}', 'U');
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('PRIV-1', 'PRIV', 'spec', 'spec.md', 'doc', '{"kind":"user","id":"olga@example.com"}');
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PUB', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"olga@example.com"}');
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('ses_priv', 'plan', 'plan.md', 'doc', '{"kind":"session","id":"ses_priv"}');
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('ses_pub_carol', 'shot', 'shot.png', 'image', '{"kind":"session","id":"ses_pub_carol"}');
	`); err != nil {
		t.Fatalf("seed the documents: %v", err)
	}
	callers := map[string]model.Actor{
		"alice": cookie(alice), "bob": cookie(bob), "carol's token naming PRIV": personalToken(carol, "PRIV"),
		"bob's token naming PRIV": personalToken(bob, "PRIV"),
	}
	viewers := map[string]model.Viewer{"the server": *model.ServerViewer}
	for name, actor := range callers {
		viewer, err := Resolve(ctx, database.Pool, actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		viewers[name] = viewer
	}
	want := map[string]map[string]bool{
		"spec":  {"alice": false, "bob": true, "carol's token naming PRIV": true, "bob's token naming PRIV": true, "the server": true},
		"notes": {"alice": true, "bob": false, "carol's token naming PRIV": true, "bob's token naming PRIV": false, "the server": true},
		"plan":  {"alice": false, "bob": false, "carol's token naming PRIV": true, "bob's token naming PRIV": false, "the server": true},
		"shot":  {"alice": false, "bob": false, "carol's token naming PRIV": true, "bob's token naming PRIV": false, "the server": true},
	}
	read := func(viewer model.Viewer) map[string][2]bool {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select a.slug, `+DocumentClause("a", "$1")+`, `+ItemOwnerClause("a.issue_key", "a.id", "$1")+`
			from artifacts a order by a.slug`, viewerArg(t, viewer))
		if err != nil {
			t.Fatalf("read the documents: %v", err)
		}
		defer rows.Close()
		answers := map[string][2]bool{}
		for rows.Next() {
			var slug string
			var document, owner bool
			if err := rows.Scan(&slug, &document, &owner); err != nil {
				t.Fatalf("scan: %v", err)
			}
			answers[slug] = [2]bool{document, owner}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the documents: %v", err)
		}
		return answers
	}
	compared := 0
	for name, viewer := range viewers {
		for slug, answer := range read(viewer) {
			compared++
			if answer[0] != answer[1] {
				t.Errorf("%s reads %s: DocumentClause %t, ItemOwnerClause %t", name, slug, answer[0], answer[1])
			}
			if expected, ok := want[slug][name]; !ok || answer[0] != expected {
				t.Errorf("%s reads %s: DocumentClause %t, want %t (known %t)", name, slug, answer[0], expected, ok)
			}
		}
	}
	if compared != len(viewers)*len(want) {
		t.Fatalf("compared %d documents, want %d: the seed's documents were not all read", compared, len(viewers)*len(want))
	}

	if _, err := database.Pool.Exec(ctx, `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	alice, err := Resolve(ctx, database.Pool, callers["alice"])
	if err != nil {
		t.Fatalf("alice: Resolve: %v", err)
	}
	if alice.AnyPrivate || !alice.Open {
		t.Fatalf("alice resolves as %+v with every project public, want an open viewer and no private project", alice)
	}
	answers := read(alice)
	for _, slug := range []string{"plan", "shot"} {
		if answer, ok := answers[slug]; !ok || !answer[0] || !answer[1] {
			t.Errorf("with no private project alice reads %s: DocumentClause and ItemOwnerClause %v (read %t), want both true (R9)", slug, answer, ok)
		}
	}
}
