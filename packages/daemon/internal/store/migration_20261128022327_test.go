package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// branchRefusalsVersion is the version of the migration this file tests, so the test can name the
// store of the release before it whatever lands beside it.
const branchRefusalsVersion = 20261128022327

// branchRefusalsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const branchRefusalsRelease = "the release that ends a branch row GitHub refuses for good"

// The way back of the release that ends a branch row GitHub refuses for good, run as the
// migration's header and docs/kubernetes.md carry it: one transaction, here on a connection of its
// own. The store holds what this release writes for two projects, a refusal of each issue's branch
// row and a branch-refused notice in each of the outbox's two notice kinds, and beside them rows
// the release before reads: a notice and a controller notice of other kinds, and a branch row. The
// way back deletes the two branch-refused notices, which the release before does not decode, keeps
// every other row of the outbox and every issue, drops the table and deletes the migration's
// version row. The store is then exactly the store the release before migrated, which is the store
// that release boots on. The guide says each statement runs again with no error, so the block runs
// a second time and must leave the store as the first run left it. The roll forward then applies
// the migration again: the table comes back, empty, and the schema is the one this release
// migrated.
//
// The negative controls are the block without `if exists` on its drop, whose second run fails on
// the table the first run dropped, and the block without its delete, after which the outbox still
// holds the two branch-refused notices.
func TestTheWayBackOfBranchRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	queue := func(kind, issue, payload string) string {
		return fmt.Sprintf(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('%s', '%s', '%s', 0, now(), '')`, kind, issue, payload)
	}
	execAll(t, store,
		doneByChecksIssue("ACME-1", "ACME-1", "implementing", "in_progress", 2, 2),
		doneByChecksIssue("OTHER-1", "OTHER-1", "planning", "in_progress", 1, 1),
		`insert into branch_refusals (issue, generation, row_id, branch_write, kind, reason, refused_at)
			values ('ACME-1', 2, 121, 'move', 'no-node-id', 'read the node id of acme/widgets: GitHub named none', now())`,
		`insert into branch_refusals (issue, generation, row_id, branch_write, kind, reason, refused_at)
			values ('OTHER-1', 1, 122, 'create', 'name-refused', 'the deployment refuses the name legion/OTHER-1', now())`,
		queue("notice", "ACME-1", `{"kind": "branch-refused", "role": "architect", "reason": "GitHub refused the move of legion/ACME-1"}`),
		queue("controller_notice", "OTHER-1", `{"kind": "branch-refused", "reason": "GitHub refused the create of legion/OTHER-1"}`),
		queue("notice", "ACME-1", `{"kind": "worker-died", "role": "implementer"}`),
		queue("controller_notice", "OTHER-1", `{"kind": "held"}`),
		queue("issue_branch", "OTHER-1", `{"generation": 1}`),
	)
	const issues = `select row(key, tree, phase, generation, status)::text from issues order by key`
	issuesBefore := rowsAsText(t, store, issues)
	migrated := schemaShape(t, store)
	released := emptyStore(t)
	migrateAllBut(t, released, branchRefusalsVersion)
	releasedShape := schemaShape(t, released)

	block := wayBackOf(t, branchRefusalsVersion, branchRefusalsRelease)
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) < 3 || lines[0] != "begin;" || lines[len(lines)-1] != "commit;" {
		t.Fatalf("the way back of %s is not one transaction:\n%s", branchRefusalsRelease, block)
	}
	conn := openWayBackConn(t, store)
	for run := 1; run <= 2; run++ {
		if _, err := conn.run(t, block, ""); err != nil {
			t.Fatalf("run %d of the way back: %v", run, err)
		}
		const outbox = `select kind || ' ' || issue || ' ' || coalesce(payload->>'kind', '') from outbox order by id`
		if got, want := rowsAsText(t, store, outbox), []string{
			"notice ACME-1 worker-died", "controller_notice OTHER-1 held", "issue_branch OTHER-1 ",
		}; !slices.Equal(got, want) {
			t.Fatalf("the outbox after run %d of the way back = %q, want %q: the branch-refused notices go, and every other row stays", run, got, want)
		}
		if got := rowsAsText(t, store, issues); !reflect.DeepEqual(got, issuesBefore) {
			t.Fatalf("the issues after run %d of the way back = %v, want them as they were: %v", run, got, issuesBefore)
		}
		if got := schemaShape(t, store); !reflect.DeepEqual(got, releasedShape) {
			extra, missing := shapeDiff(got, releasedShape)
			t.Fatalf("the store after run %d of the way back is not the store the release before migrated:\nit holds besides: %q\nit lacks: %q", run, extra, missing)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, `select count(*)::text from branch_refusals`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the refusals after the roll forward = %v, want none: the way back dropped every one", got)
	}
	if got := schemaShape(t, store); !reflect.DeepEqual(got, migrated) {
		extra, missing := shapeDiff(got, migrated)
		t.Fatalf("the store after the roll forward is not the store this release migrated:\nit holds besides: %q\nit lacks: %q", extra, missing)
	}
}
