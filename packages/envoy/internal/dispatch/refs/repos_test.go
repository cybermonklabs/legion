package refs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A caller splicing a bare column name gets its own row's canonical name, not the one rename the
// table happens to hold. The expression reads repo_projects itself, so an inner table exposing a
// column named `repo` would capture a caller's bare `repo`: the comparison would be that column
// against itself, true for every row, and every row of every caller would answer the greatest
// renamed_to in the table. The mappings here are a renamed pair and two mappings that were never
// renamed, sorting either side of the pair, so a capture is caught whichever row the reader
// reaches first (cireconcile's Reconciler.covered is the caller that splices a bare column).
func TestTheCanonicalNameRuleOverATableOfItsOwnColumnAnswersEachRowsOwnName(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('REPO', 'Repository project', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	for _, mapping := range []struct{ repo, renamedTo string }{
		{repo: "acme/gadgets"},
		{repo: "acme/widgets"},
		{repo: "acme/widgets-before", renamedTo: "acme/widgets"},
		{repo: "zed/other"},
	} {
		if _, err := database.Pool.Exec(ctx, `
			insert into repo_projects (repo, project, created_by, renamed_to)
			values ($1, 'REPO', '{"kind":"system","id":"test"}'::jsonb, nullif($2, ''))
		`, mapping.repo, mapping.renamedTo); err != nil {
			t.Fatalf("map %s: %v", mapping.repo, err)
		}
	}

	// `from repo_projects` with no alias and a bare `repo`, which is the shape the rule has to
	// read correctly: the caller's column is the only `repo` in scope.
	rows, err := database.Pool.Query(ctx, `select repo, `+CanonicalRepoSQL("repo")+` as canonical
		from repo_projects order by repo`)
	if err != nil {
		t.Fatalf("read every mapping's canonical name: %v", err)
	}
	defer rows.Close()
	canonical := map[string]string{}
	for rows.Next() {
		var repo, name string
		if err := rows.Scan(&repo, &name); err != nil {
			t.Fatalf("read a mapping's canonical name: %v", err)
		}
		canonical[repo] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read every mapping's canonical name: %v", err)
	}

	want := map[string]string{
		"acme/gadgets":        "acme/gadgets",
		"acme/widgets":        "acme/widgets",
		"acme/widgets-before": "acme/widgets",
		"zed/other":           "zed/other",
	}
	for repo, name := range want {
		if canonical[repo] != name {
			t.Errorf("%s canonicalises to %q, want %q: the whole answer was %v", repo, canonical[repo], name, canonical)
		}
	}
}

// A statement that names no column the expression can read fails loudly rather than answering
// anything: the rule's own tables carry no column a caller could be spliced over by accident, so
// a misspelled or missing column is the database's error and never a silently wrong name.
func TestTheCanonicalNameRuleRefusesAColumnTheCallersStatementDoesNotHave(t *testing.T) {
	database := storetest.Open(t)
	var canonical string
	err := database.Pool.QueryRow(context.Background(), `select `+CanonicalRepoSQL("repo")).Scan(&canonical)
	if err == nil {
		t.Fatalf("the rule over a statement with no repo column answered %q", canonical)
	}
	if !strings.Contains(err.Error(), `column "repo" does not exist`) {
		t.Fatalf("the rule over a statement with no repo column failed with %v, want the missing column", err)
	}
}

// A name is held to GitHub's bound as a GitHub path carries it, since the halves SplitGitHubRepo
// answers go into the path as they are: a stored name that still ends in `.git` is up to 100
// characters with its suffix, so `acme/<96 characters>.git` is taken and `acme/<97 characters>.git`
// is refused, though 97 characters are left once that `.git` is trimmed, and a stored
// `acme/<100 characters>.git`, 104 characters in the path, is refused. Once that `.git` is
// trimmed the name is still held to the rule, so a name GitHub would hold as nothing, `.` or `..`
// is refused. The halves come back as full spells them.
//
// The negative control is the bound put to the trimmed name alone, as it was, which takes
// `acme/<97 characters>.git` and `acme/<100 characters>.git`.
func TestSplitGitHubRepoBoundsTheNameAsTheGitHubPathCarriesIt(t *testing.T) {
	for _, row := range []struct {
		full string
		ok   bool
	}{
		{full: "acme/widgets", ok: true},
		{full: "Acme/Widgets.git", ok: true},
		{full: "acme/" + strings.Repeat("a", 100), ok: true},
		{full: "acme/" + strings.Repeat("a", 101)},
		{full: "acme/" + strings.Repeat("a", 96) + ".git", ok: true},
		{full: "acme/" + strings.Repeat("a", 97) + ".git"},
		{full: "acme/" + strings.Repeat("a", 100) + ".git"},
		{full: "acme/x.git.git", ok: true},
		{full: "acme/.git"},
		{full: "acme/...git"},
		{full: "acme/..git"},
		{full: "acme/.."},
		{full: "acme/."},
		{full: "acme/"},
		{full: "acme"},
		{full: "-acme/widgets"},
	} {
		owner, name, err := SplitGitHubRepo(row.full)
		switch {
		case row.ok && (err != nil || owner+"/"+name != row.full):
			t.Errorf("SplitGitHubRepo(%q) = %q, %q, %v; want its halves as written", row.full, owner, name, err)
		case !row.ok && (!errors.Is(err, ErrNotGitHubRepo) || !strings.Contains(err.Error(), `"`+row.full+`"`)):
			t.Errorf("SplitGitHubRepo(%q) = %q, %q, %v; want ErrNotGitHubRepo naming it", row.full, owner, name, err)
		}
	}
}
