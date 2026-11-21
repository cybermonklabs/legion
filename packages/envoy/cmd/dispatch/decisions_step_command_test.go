package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

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
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, args, env, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
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
