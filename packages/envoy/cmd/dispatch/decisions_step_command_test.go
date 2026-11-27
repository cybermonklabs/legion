package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// `envoy-dispatch restore-answered-asks` and `convert-decisions` refuse a --project that names no
// row of `projects` by its exact key - a key the database never held, and a project's key in
// another case - with exit 2 and a line naming it, before any phase runs; and a run over a project
// that owes nothing, dry or not, prints one `issues=0` line and exits 0. Without both, each of
// those runs exits 0 and prints nothing, and an operator cannot tell a typo from nothing owed.
func TestTheDecisionsStepCommandsRefuseAnUnknownProjectAndSayWhenNothingWasOwed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('OWED', 'Owes nothing', 'alice')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	run := decisionsStepCommand(t, database)
	for _, command := range []string{"restore-answered-asks", "convert-decisions"} {
		for _, project := range []string{"NOPE", "owed"} {
			code, stdout, stderr := run(command, "--project", project)
			if code != 2 || !strings.Contains(stderr, strconv.Quote(project)) || strings.Contains(stdout, "issues=") {
				t.Errorf("%s --project %s exited %d, printed %q and wrote %q to stderr; want exit 2 and a line naming %q, and no summary",
					command, project, code, stdout, stderr, project)
			}
		}
		for _, dry := range []bool{false, true} {
			args := []string{command, "--project", "OWED"}
			if dry {
				args = append(args, "--dry-run")
			}
			code, stdout, stderr := run(args...)
			want := command + ": issues=0 project=OWED dry_run=" + strconv.FormatBool(dry) + "\n"
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("%v over a project that owes nothing exited %d and printed %q (stderr %q); want exit 0 and %q",
					args, code, stdout, stderr, want)
			}
		}
	}
}

// A run of `envoy-dispatch convert-decisions` that leaves an issue because a document of it owes
// its block index says so: issue LEFT-1's spec and decisions document each hold a version no block
// index has read yet (the trigger document_block_versions_owe marks each owed), so the command,
// dry or not, prints a line naming LEFT-1 as left, prints no `issues=0`, and exits 1, since the
// issue is still owed. That the pass writes nothing of such an issue is
// TestAPassLeavesAnIssueADocumentOfWhichOwesItsBlockIndexForALaterPass's (docs).
func TestConvertDecisionsNamesAnIssueItLeavesForItsBlockIndexAndExitsOne(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	run := decisionsStepCommand(t, database)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('LEFT', 'Owes a block index', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			values ('LEFT-1', 'LEFT', 1, 'Left', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by) values
			('LEFT-1', 'LEFT', 'spec', 'spec.md', 'doc', true, null, '{"kind":"user","id":"alice"}'),
			('LEFT-1', 'LEFT', 'decisions-md', 'decisions.md', 'doc', false, 'decisions', '{"kind":"user","id":"alice"}')`,
		`insert into artifact_versions (artifact_id, number, markdown)
			select id, 1, '# ' || name || E'\n' from artifacts where issue_key = 'LEFT-1'`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the issue whose documents owe their block index: %v", err)
		}
	}
	var owed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_block_versions d join artifacts a on a.id = d.artifact_id
		where a.issue_key = 'LEFT-1' and d.version is null
	`).Scan(&owed); err != nil || owed != 2 {
		t.Fatalf("%d documents of LEFT-1 owe their block index (%v), want its spec and its decisions document", owed, err)
	}
	for _, dry := range []bool{false, true} {
		args := []string{"convert-decisions", "--project", "LEFT"}
		if dry {
			args = append(args, "--dry-run")
		}
		code, stdout, stderr := run(args...)
		if code != 1 || !strings.Contains(stdout, "convert-decisions: issue=LEFT-1 left: ") || strings.Contains(stdout, "issues=0") {
			t.Errorf("%v over a project whose issue owes its block index exited %d and printed %q (stderr %q); want exit 1, a line saying it left LEFT-1, and no issues=0",
				args, code, stdout, stderr)
		}
	}
}

// decisionsStepCommand runs an envoy-dispatch subcommand over database, answering its exit code,
// what it printed to stdout and what it wrote to stderr.
func decisionsStepCommand(t *testing.T, database *store.Store) func(args ...string) (int, string, string) {
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	return func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(context.Background(), args, env, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
}

// `envoy-dispatch backfill-block-ids` over a database that holds no document prints one
// `documents=0` line and exits 0, rather than nothing, as the decisions step's commands say
// `issues=0` above.
func TestBackfillBlockIDsSaysWhenNoDocumentWasOwed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	var stdout, stderr bytes.Buffer
	code := runSubcommand(ctx, []string{"backfill-block-ids"},
		envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()}), &stdout, &stderr)
	if printed := stdout.String() + stderr.String(); code != 0 || !strings.Contains(printed, "backfill-block-ids: documents=0\n") {
		t.Errorf("backfill-block-ids over a database with no document exited %d and printed %q; want exit 0 and a documents=0 line", code, printed)
	}
}
