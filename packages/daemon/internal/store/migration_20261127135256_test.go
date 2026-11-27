package store

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// mergedNewWorkVersion is the version of the migration this file tests, so each test can migrate
// through everything before it whatever lands beside it.
const mergedNewWorkVersion = 20261127135256

// mergedNewWorkRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const mergedNewWorkRelease = "the release that starts a next change for a request on a merged issue"

// The statements of the three things the migration admits, for issue ACME-1 at generation 2: a
// request for new work, a merge read of its history, and a read of its never-merged confirmation.
const (
	mergedNewWorkRequest = `insert into defects (issue, generation, kind, text, role, reported_at)
		values ('ACME-1', 2, 'request', 'Reopened after an early close: the export drops its header', 'person', now())`
	mergedNewWorkRead = `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('merge_read', 'ACME-1', '{"issue": "ACME-1", "generation": 2, "requester": "person", "request": "todo"}', 0, now(), '')`
	mergedNewWorkConfirmed = `select never_merged_confirmed::text from issues where key = 'ACME-1'`
)

// The migration admits a request for new work among the reasons of a next change, a merge read
// among the outbox's kinds, and an issue's never-merged confirmation, which every issue the
// upgrade finds holds false. Before it, the defects' kind check refuses the request, the outbox's
// kind check the merge read, and the issues hold no such column. It moves no row.
//
// Its way back, run as the migration's header carries it, prints the three lists a person keeps:
// each open request, each issue whose merge read was pending, and each issue whose confirmation
// was not taken. Each is seeded alone, so a list is held to its own rows and the two others print
// empty. It deletes the rows the release before cannot read, the requests, the merge reads and
// the merge-unread notices, and keeps every other notice. The store is then exactly the store the
// release before migrated, which is the store that release boots on.
//
// The negative controls are the migration absent, where each of the three is refused after Migrate
// as before and the test fails at the first, and the way back without its restore of the defects'
// kind check, where the store after it holds that check with request in it.
func TestTheMigrationOfMergedNewWork(t *testing.T) {
	ctx := t.Context()
	store := emptyStore(t)
	migrateThrough(t, store, mergedNewWorkVersion-1)
	execAll(t, store, doneByChecksIssue("ACME-1", "ACME-1", "done", "done", 2, 2))
	for name, statement := range map[string]string{"a request": mergedNewWorkRequest, "a merge read": mergedNewWorkRead, "the confirmation": mergedNewWorkConfirmed} {
		if _, err := store.Pool().Exec(ctx, statement); err == nil {
			t.Fatalf("%s before the migration was taken, want it refused: %s", name, statement)
		}
	}
	const kept = `select row(key, tree, phase, generation, status)::text from issues order by key`
	before := rowsAsText(t, store, kept)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the merged-new-work migration: %v", err)
	}

	for name, statement := range map[string]string{"a request": mergedNewWorkRequest, "a merge read": mergedNewWorkRead} {
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s after the migration = %v, want it taken", name, err)
		}
	}
	if got := rowsAsText(t, store, mergedNewWorkConfirmed); !reflect.DeepEqual(got, []string{"false"}) {
		t.Fatalf("the confirmation of the issue the upgrade found = %v, want false", got)
	}
	if got := rowsAsText(t, store, kept); !reflect.DeepEqual(got, before) {
		t.Fatalf("the issues after the migration = %v, want them as they were: %v", got, before)
	}

	block := wayBackOf(t, mergedNewWorkVersion, mergedNewWorkRelease)
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) < 3 || lines[0] != "begin;" || lines[len(lines)-1] != "commit;" {
		t.Fatalf("the way back of %s is not one transaction:\n%s", mergedNewWorkRelease, block)
	}
	notice := func(kind, noticeKind string) string {
		return `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('` + kind + `', 'ACME-1', '{"kind": "` + noticeKind + `"}', 0, now(), '')`
	}
	for _, tc := range []struct {
		name  string
		seed  []string
		lists [3]string
		// outbox is each row the outbox holds after the way back, as its kind and its notice kind.
		outbox []string
	}{
		{name: "an open request", seed: []string{
			mergedNewWorkRequest,
			`insert into defects (issue, generation, kind, text, role, reported_at, withdrawn_at, withdraw_reason)
				values ('ACME-1', 2, 'request', 'a withdrawn request', 'controller', now(), now(), 'the work is done')`,
		}, lists: [3]string{`"text":"Reopened after an early close: the export drops its header"`, "[]", "[]"}},
		{name: "a pending merge read", seed: []string{
			mergedNewWorkRead, notice("notice", "merge-unread"), notice("controller_notice", "merge-unread"), notice("notice", "worker-died"),
		}, lists: [3]string{"[]", `"issue":"ACME-1"`, "[]"}, outbox: []string{"notice worker-died"}},
		{name: "an unconsumed confirmation", seed: []string{
			doneByChecksIssue("ACME-2", "ACME-2", "done", "backlog", 2, 2),
			`update issues set never_merged_confirmed = true where key = 'ACME-2'`,
		}, lists: [3]string{"[]", "[]", `["ACME-2"]`}},
	} {
		t.Run("its way back of "+tc.name, func(t *testing.T) {
			store := emptyStore(t)
			if _, err := store.Migrate(t.Context()); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			execAll(t, store, doneByChecksIssue("ACME-1", "ACME-1", "done", "todo", 2, 2))
			execAll(t, store, tc.seed...)
			notices, err := openWayBackConn(t, store).run(t, block, "")
			if err != nil {
				t.Fatalf("run the way back: %v", err)
			}
			if len(notices) != 3 {
				t.Fatalf("the way back printed %q, want its three lists", notices)
			}
			for i, want := range tc.lists {
				if want == "[]" && !strings.HasSuffix(notices[i], ": []") || want != "[]" && !strings.Contains(notices[i], want) {
					t.Fatalf("list %d of the way back = %q, want it to hold %s", i+1, notices[i], want)
				}
			}
			if strings.Contains(notices[0], "a withdrawn request") {
				t.Fatalf("the list of open requests = %q, which names a withdrawn one", notices[0])
			}
			if got := rowsAsText(t, store, `select kind || ' ' || coalesce(payload->>'kind', '') from outbox order by id`); !slices.Equal(got, tc.outbox) {
				t.Fatalf("the outbox after the way back = %v, want %v: the merge reads and the merge-unread notices go, and every other row stays", got, tc.outbox)
			}
			released := emptyStore(t)
			migrateAllBut(t, released, mergedNewWorkVersion)
			if got, want := schemaShape(t, store), schemaShape(t, released); !reflect.DeepEqual(got, want) {
				extra, missing := shapeDiff(got, want)
				t.Fatalf("the store after the way back is not the store the release before migrated:\nit holds besides: %q\nit lacks: %q", extra, missing)
			}
		})
	}
}
