package main

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestRebuildRefsReadsTheServedAndFormerOrigins: rebuild-refs reads a dashboard link at
// DISPATCH_SERVER_URL, at an address DISPATCH_SERVED_ORIGINS lists and at one
// DISPATCH_FORMER_ORIGINS lists as a reference to the issue its page names, as the server's own
// index reads it (the API's TestEveryReaderOfADashboardLinkTakesTheServedAndFormerOrigins), and a
// link at any other address as none, in a document's version and in a comment alike. A list the
// server would refuse at its start stops rebuild-refs before it rewrites any reference: it exits 1
// naming the setting, and the index holds what it held.
func TestRebuildRefsReadsTheServedAndFormerOrigins(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	// The issues each link names, by the address it is written at, and whether it is a reference.
	links := []struct {
		address, origin, issue string
		reference              bool
	}{
		{"the canonical address", "https://a.example.test", "CORE-1", true},
		{"a served address", "https://b.example.test", "CORE-2", true},
		{"a former address", "https://old.example.test", "CORE-3", true},
		{"an unrelated address", "https://other.example.test", "CORE-4", false},
	}
	var pages []string
	for _, link := range links {
		pages = append(pages, link.origin+"/issues/"+link.issue)
	}
	text := "The plan is at " + strings.Join(pages, " and ") + " today.\n"
	// What an earlier build wrote and indexed no reference of: four issues the links name, a fifth
	// holding a document whose version holds the links, and a comment holding them.
	const artifactID = "4f1d5c3e-8c55-4f43-9a7e-2b57b2f2b0a1"
	const author = `{"kind":"user","id":"alice"}`
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice')`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	for number := 1; number <= 5; number++ {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-' || $1::int, 'CORE', $1, 'An issue', $2, 'U')
		`, number, author); err != nil {
			t.Fatalf("seed issue CORE-%d: %v", number, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
		values ($1, 'CORE-5', 'CORE', 'links', 'links.md', 'doc', $2)
	`, artifactID, author); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown) values ($1, 1, $2)`, artifactID, "# Links\n\n"+text); err != nil {
		t.Fatalf("seed the document's version: %v", err)
	}
	var commentID string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-5', $1, $2) returning id::text
	`, author, text).Scan(&commentID); err != nil {
		t.Fatalf("seed the comment: %v", err)
	}
	rebuild := func(overrides map[string]string) (int, string) {
		values := map[string]string{
			"DATABASE_URL":            database.Pool.Config().ConnString(),
			"XDG_CONFIG_HOME":         t.TempDir(),
			"DISPATCH_SERVER_URL":     "https://a.example.test",
			"DISPATCH_SERVED_ORIGINS": "https://b.example.test",
			"DISPATCH_FORMER_ORIGINS": "https://old.example.test",
		}
		maps.Copy(values, overrides)
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, []string{"rebuild-refs"}, envGetter(values), &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	if code, output := rebuild(nil); code != 0 {
		t.Fatalf("rebuild-refs exited %d: %s", code, output)
	}
	for _, source := range []struct{ kind, id string }{{"artifact", artifactID}, {"comment", commentID}} {
		cited := citedIssues(t, database, source.kind, source.id)
		for _, link := range links {
			if got := slices.Contains(cited, link.issue); got != link.reference {
				t.Errorf("after rebuild-refs the %s cites %s, the link at %s (%s): %t, want %t (it cites %v)", source.kind, link.issue, link.address, link.origin, got, link.reference, cited)
			}
		}
	}

	for _, refused := range []struct{ setting, value string }{
		{"DISPATCH_SERVED_ORIGINS", "b.example.test"},
		{"DISPATCH_FORMER_ORIGINS", "https://old.example.test/issues"},
	} {
		// The index holds nothing before a run that must write nothing, so whatever it holds after
		// is that run's.
		if _, err := database.Pool.Exec(ctx, `delete from refs`); err != nil {
			t.Fatalf("empty the index: %v", err)
		}
		code, output := rebuild(map[string]string{refused.setting: refused.value})
		if code != 1 || !strings.Contains(output, refused.setting) {
			t.Errorf("rebuild-refs with %s=%q exited %d: %q; want 1, naming %s", refused.setting, refused.value, code, output, refused.setting)
		}
		var edges int
		if err := database.Pool.QueryRow(ctx, `select count(*) from refs`).Scan(&edges); err != nil {
			t.Fatal(err)
		}
		if edges != 0 {
			t.Errorf("rebuild-refs with %s=%q wrote %d references, want none: it must stop before it rewrites any", refused.setting, refused.value, edges)
		}
	}
}

// citedIssues is the keys of the issues the refs index holds an edge to from the source kind id.
func citedIssues(t *testing.T, database *store.Store, kind, id string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_id from refs where from_kind = $1 and from_id = $2 and to_kind = 'issue' order by to_id
	`, kind, id)
	if err != nil {
		t.Fatalf("read the references of %s %s: %v", kind, id, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the references of %s %s: %v", kind, id, err)
	}
	return keys
}
