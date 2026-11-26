package store

import (
	"context"
	"slices"
	"testing"
)

// The pairs are the rows that name a renamed_to, both names in lower case. A row whose names are
// one name in two cases renames nothing and is dropped, as is a row whose renamed_to is empty,
// and a row with no renamed_to is no pair. Two rows that name one old name in different cases
// are one pair, with the greater new name, so every reader takes the same new name for it.
func TestReadRepoRenamesAnswersOneLowerCasePairForEachOldName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedEmptyStore(t)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into repo_projects (repo, project, created_by, renamed_to) values
			('SJawhar/Legion', 'CORE', '{"kind":"user","id":"alice"}', 'CybermonkLabs/Legion'),
			('oldco/gadgets', 'CORE', '{"kind":"user","id":"alice"}', 'acme/gadgets'),
			('OldCo/Gadgets', 'CORE', '{"kind":"user","id":"alice"}', 'Acme/Gizmos'),
			('Acme/Widgets', 'CORE', '{"kind":"user","id":"alice"}', 'acme/widgets'),
			('acme/empty', 'CORE', '{"kind":"user","id":"alice"}', ''),
			('acme/plain', 'CORE', '{"kind":"user","id":"alice"}', null);
	`); err != nil {
		t.Fatalf("seed the mapping: %v", err)
	}
	pairs, err := ReadRepoRenames(ctx, store.Pool)
	if err != nil {
		t.Fatalf("read the pairs: %v", err)
	}
	want := []RepoRename{
		{From: "oldco/gadgets", To: "acme/gizmos"},
		{From: "sjawhar/legion", To: "cybermonklabs/legion"},
	}
	if !slices.Equal(pairs, want) {
		t.Fatalf("the pairs = %+v, want %+v", pairs, want)
	}
}

// dispatch://INFRA-4411 drops repo_projects. A process that boots after that reads no pairs and
// no error, so it serves, and every reader then names each repository as it is stored.
func TestReadRepoRenamesReadsNothingOnceRepoProjectsIsDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := migratedEmptyStore(t)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into repo_projects (repo, project, created_by, renamed_to)
			values ('sjawhar/legion', 'CORE', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
		drop table repo_projects;
	`); err != nil {
		t.Fatalf("seed a pair and drop the table: %v", err)
	}
	pairs, err := ReadRepoRenames(ctx, store.Pool)
	if err != nil || len(pairs) != 0 {
		t.Fatalf("the pairs with repo_projects dropped = %+v, %v; want none and no error", pairs, err)
	}
}
