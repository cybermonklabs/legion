package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// repoColumnsThePassNeverWrites are the columns of the migrated schema whose name says they hold
// a repository and that the pass leaves as they are, each with the package comment's reason.
var repoColumnsThePassNeverWrites = map[string]string{
	"repo_projects.repo": "the rename pairs themselves",
	"spec_check_results.repository": "the record of one CI run, with its repository as the run's results " +
		"token named it: nothing updates or deletes such a row, and no read selects or compares by it",
	"check_states.repo": "the settler's published states, which a reader keys by repository: the pass's " +
		"write of a pull request's row owes its issue a settle, which publishes each state under the new " +
		"name with an event and deletes the state under the old one",
	"check_state_sets.repo": "the settler's published set of checks at each head, which a reader keys by " +
		"repository: the settle the pass's write owes publishes each head's set under the new name and " +
		"deletes the set under the old one, as it does each state",
	"pull_request_former_heads_aliases.repo": "a name the settler met a pull request under, which never " +
		"leaves: an old name finds the identity of the new one by the rename rule, and a rewrite that " +
		"dropped it where the new name holds an identity not yet joined would part a history from its pull request",
	"pull_request_former_heads_moves.repo": "the writers' record of each write under the name of the row " +
		"written, which the settler applies by that name to its own state: the pass's write of the pull " +
		"request's row records its move from the old name to the new one itself",
	"pull_request_former_heads_pending.repo": "a row owed to an issue's settle, which drains it by its " +
		"issue and finds its pull request by an alias, which keeps the old name",
	"pull_request_former_heads_rows.repo": "the settler's own state of each open row, by the name the " +
		"row's records carry, which the settler moves to the new name when it applies the pass's record",
}

// TestEveryRepositoryColumnIsAFieldOfThePass reads the migrated schema, not a list: each text or
// text[] column of a table of the public schema whose name is repo, or ends in _repo, _repos or
// _repository, is a column a line of repoRenameLines writes, or one the pass never writes, for a
// reason named above. So a migration that adds a repository column fails here until it gives the
// column a line or a reason, rather than leaving rows under an old name that a reader keyed by the
// new one never finds.
func TestEveryRepositoryColumnIsAFieldOfThePass(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	rows, err := store.Pool.Query(ctx, `
		select c.table_name || '.' || c.column_name
		from information_schema.columns c
		join information_schema.tables t on t.table_schema = c.table_schema and t.table_name = c.table_name
		where c.table_schema = 'public' and t.table_type = 'BASE TABLE'
		  and c.column_name ~ '^(.*_)?repo(s|sitory)?$'
		  and c.udt_name in ('text', 'varchar', '_text', '_varchar')
		order by 1`)
	if err != nil {
		t.Fatalf("read the repository columns: %v", err)
	}
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("read a repository column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the repository columns: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("the migrated schema has no repository column the census reads: it cannot tell a column the pass misses")
	}
	pair := RepoRename{From: "sjawhar/legion", To: "cybermonklabs/legion"}
	r := repoRename{RepoRename: pair, url: NewRepoURLRule(pair.From, pair.To)}
	var written []string
	for _, line := range repoRenameLines {
		for _, step := range line.steps(r) {
			if step.write != "" {
				written = append(written, step.table+"."+step.write)
			}
		}
	}
	for _, column := range columns {
		if slices.Contains(written, column) {
			continue
		}
		if _, never := repoColumnsThePassNeverWrites[column]; never {
			continue
		}
		t.Errorf("%s holds a repository and no line of repoRenameLines writes it (the lines write %s)",
			column, strings.Join(written, ", "))
	}
	for column := range repoColumnsThePassNeverWrites {
		if !slices.Contains(columns, column) {
			t.Errorf("%s is listed as a column the pass never writes, and the schema has no such column", column)
		}
	}
}
