package refs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestThePullRequestCanonicalRepoFunctionAnswersWhatCanonicalAnswers holds the database's own
// canonical name of a repository (pull_request_canonical_repo, which the former heads' triggers,
// install and settler read) to the rename rule every Go reader holds (Renames.Canonical over the
// pairs store.ReadRepoRenames answers): the pull request heads a writer keys by the Go rule are
// found under the name the function answers. The repo_projects rows a deployment can hold are
// not all pairs: a new name in another case than the rule stores it, a row renamed to its own name
// in another case, an empty new name, and two rows of one old name in two cases each read as the
// rule reads them. A database where repo_projects is gone holds no pairs, and the function then
// answers each name as it is given, as ReadRepoRenames reads none.
func TestThePullRequestCanonicalRepoFunctionAnswersWhatCanonicalAnswers(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, default_repo)
		values ('RNAM', 'Renames', 'alice', 'acme/widgets')
	`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	for _, row := range []struct{ repo, renamedTo string }{
		{"sjawhar/legion", "CybermonkLabs/Legion"},
		{"acme/gadgets", "Acme/Gadgets"},
		{"acme/empty", ""},
		{"Acme/Old", "acme/alpha"},
		{"acme/old", "Acme/Beta"},
	} {
		if _, err := database.Pool.Exec(ctx, `
			insert into repo_projects (repo, project, created_by, is_default, renamed_to)
			values ($1, 'RNAM', '{"kind":"system","id":"refs-test"}'::jsonb, false, $2)
		`, row.repo, row.renamedTo); err != nil {
			t.Fatalf("seed %s renamed to %q: %v", row.repo, row.renamedTo, err)
		}
	}
	names := []string{
		"sjawhar/legion", "SJawhar/Legion", "cybermonklabs/legion", "CybermonkLabs/Legion",
		"acme/gadgets", "ACME/GADGETS", "acme/empty", "acme/old", "ACME/OLD", "acme/widgets",
	}
	compare := func(label string) {
		t.Helper()
		pairs, err := store.ReadRepoRenames(ctx, database.Pool)
		if err != nil {
			t.Fatalf("%s: read the rename pairs: %v", label, err)
		}
		renames := NewRenames(pairs)
		for _, repo := range names {
			var got string
			if err := database.Pool.QueryRow(ctx, `select pull_request_canonical_repo($1)`, repo).Scan(&got); err != nil {
				t.Fatalf("%s: read pull_request_canonical_repo(%q): %v", label, repo, err)
			}
			if want := renames.Canonical(repo); got != want {
				t.Errorf("%s: pull_request_canonical_repo(%q) = %q, want %q as Canonical answers over %v", label, repo, got, want, pairs)
			}
		}
	}
	compare("with the rows")
	if _, err := database.Pool.Exec(ctx, `drop table repo_projects`); err != nil {
		t.Fatalf("drop repo_projects: %v", err)
	}
	compare("with repo_projects gone")
}
