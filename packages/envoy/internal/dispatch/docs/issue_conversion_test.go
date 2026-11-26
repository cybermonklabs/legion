package docs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// unconvertedIssue is an issue of the test store as the release before the issue room stores it
// (createUnconvertedIssue): its three role documents, each a room of its own, and the text each
// one's room and latest version hold, by document id.
type unconvertedIssue struct {
	key                      string
	spec, verification, plan string
	markdown                 map[string]string
}

// createUnconvertedIssue writes, as alice, the issue DOC-<number> under parent ("" for none) with
// its spec, its verification-md and its plan-md, each seeded into a room of its own, versioned and
// indexed in one transaction, as a document written before its issue's conversion is.
func createUnconvertedIssue(t *testing.T, database *store.Store, service *Service, number int, parent, spec, verification, plan string) unconvertedIssue {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice')
		on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("create the test project: %v", err)
	}
	issue := unconvertedIssue{key: fmt.Sprintf("DOC-%d", number), markdown: map[string]string{}}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key)
		values ($1, 'DOC', $2, 'Conversion', '{"kind":"user","id":"alice"}', 'U', nullif($3, ''))
	`, issue.key, number, parent); err != nil {
		t.Fatalf("create %s: %v", issue.key, err)
	}
	issue.spec = writeUnconvertedDocument(t, database, service, &issue, "spec", "spec.md", true, "", spec)
	issue.verification = writeUnconvertedDocument(t, database, service, &issue, "verification-md", "verification.md", false, "verification", verification)
	issue.plan = writeUnconvertedDocument(t, database, service, &issue, "plan-md", "plan.md", false, "plan", plan)
	return issue
}

// writeUnconvertedDocument writes one document of issue: its row, its room's first text, its
// version 1 and that version's index, and records the text it stores.
func writeUnconvertedDocument(t *testing.T, database *store.Store, service *Service, issue *unconvertedIssue, slug, name string, primary bool, role, markdown string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($1, 'DOC', $2, $3, 'doc', $4, nullif($5, ''), '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue.key, slug, name, primary, role).Scan(&id); err != nil {
		t.Fatalf("create %s of %s: %v", slug, issue.key, err)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		written, err := service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		issue.markdown[id] = written
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write %s of %s: %v", slug, issue.key, err)
	}
	return id
}

// conversionCount is one count of the test store, failing t where the read fails.
func conversionCount(t *testing.T, database *store.Store, what, sql string, args ...any) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), sql, args...).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", what, err)
	}
	return count
}

// requireUnconverted fails t unless issue holds no tree, none of its documents a document_rooms
// row, and no tree artifact of it a stored update: what a conversion that wrote nothing leaves.
func requireUnconverted(t *testing.T, database *store.Store, issue unconvertedIssue) {
	t.Helper()
	for _, held := range []struct{ what, sql string }{
		{"the issue_trees rows of the issue", `select count(*)::int from issue_trees where issue_key = $1`},
		{"the document_rooms rows of the issue's documents", `
			select count(*)::int from document_rooms d join artifacts a on a.id = d.artifact_id where a.issue_key = $1`},
		{"the tree artifacts of the issue", `select count(*)::int from artifacts where issue_key = $1 and kind = 'tree'`},
		{"the stored updates of a tree of the issue", `
			select count(*)::int from doc_updates u join artifacts a on a.id = u.artifact_id where a.issue_key = $1 and a.kind = 'tree'`},
	} {
		if count := conversionCount(t, database, held.what, held.sql, issue.key); count != 0 {
			t.Fatalf("%s holds %d of %s, want none: the issue is not converted", issue.key, count, held.what)
		}
	}
}

// treeOfIssue is the tree artifact of issueKey and its ref_key, failing t where it holds none.
func treeOfIssue(t *testing.T, database *store.Store, issueKey string) (string, string) {
	t.Helper()
	var tree, refKey string
	if err := database.Pool.QueryRow(context.Background(), `
		select t.artifact_id::text, a.ref_key from issue_trees t join artifacts a on a.id = t.artifact_id where t.issue_key = $1
	`, issueKey).Scan(&tree, &refKey); err != nil {
		t.Fatalf("read the tree of %s: %v", issueKey, err)
	}
	return tree, refKey
}

// An issue converts whole or not at all (R6 of this issue's spec): a trigger that refuses the
// spec's document_rooms row fails the conversion, which leaves no tree, no document_rooms row and
// no stored update of a tree; once the trigger is gone the conversion writes the room's first
// update as its version 1, joins the three documents to the room, keeps each document's text in a
// version of its own, and states a moved_to from each block's old address to its node in the room.
// A second run passes the converted issue over.
func TestTheConversionConvertsAnIssueWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "",
		"The spec's opening paragraph.\n\n:::requirement{#r1}\nThe requirement.\n:::\n",
		"The verification's paragraph.\n", "The plan's paragraph.\n")
	for _, statement := range []string{
		`create function conversion_refuses_the_spec() returns trigger language plpgsql as $$
		begin
			if new.artifact_id = '` + issue.spec + `'::uuid then
				raise exception 'conversion_refuses_the_spec refuses %', new.artifact_id;
			end if;
			return new;
		end $$`,
		`create trigger conversion_refuses_the_spec before insert on document_rooms
			for each row execute function conversion_refuses_the_spec()`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create the trigger that refuses the spec's room: %v", err)
		}
	}
	if _, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false); err == nil || !strings.Contains(err.Error(), "conversion_refuses_the_spec") {
		t.Fatalf("the conversion of %s under the refusing trigger answered %v, want the trigger's refusal", issue.key, err)
	}
	requireUnconverted(t, database, issue)
	for _, statement := range []string{
		`drop trigger conversion_refuses_the_spec on document_rooms`,
		`drop function conversion_refuses_the_spec()`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("drop the trigger that refuses the spec's room: %v", err)
		}
	}
	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil {
		t.Fatalf("convert %s: %v", issue.key, err)
	}
	if result.Outcome != IssueConverted || result.Items == 0 {
		t.Fatalf("the conversion of %s answered %+v, want it converted with its first update's item count", issue.key, result)
	}
	tree, treeRef := treeOfIssue(t, database, issue.key)
	if joined := conversionCount(t, database, "the documents joined to the room", `
		select count(*)::int from document_rooms where room = $1::uuid and artifact_id = any($2::uuid[])
	`, tree, []string{issue.spec, issue.verification, issue.plan}); joined != 3 {
		t.Fatalf("%d of the three documents of %s name its tree %s as their room, want 3", joined, issue.key, tree)
	}
	if first := conversionCount(t, database, "the room's first update", `
		select coalesce(min(version), 0)::int from doc_updates where artifact_id = $1::uuid
	`, tree); first != 1 {
		t.Fatalf("the room of %s holds its first update at version %d, want 1", issue.key, first)
	}
	for _, document := range []string{issue.spec, issue.verification, issue.plan} {
		var number int
		var markdown string
		if err := database.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
		`, document).Scan(&number, &markdown); err != nil {
			t.Fatalf("read the newest version of %s: %v", document, err)
		}
		if number != 2 || markdown != issue.markdown[document] {
			t.Errorf("the newest version of %s is %d holding %q, want the conversion's version 2 holding the text its room held, %q",
				document, number, markdown, issue.markdown[document])
		}
	}
	old := refs.BlockAddress(graphstore.ArtifactRefKey(issue.key, "DOC", "", "spec"), "r1")
	moved, ok, err := graphstore.MovedTo(ctx, database.Pool, old)
	if err != nil {
		t.Fatalf("resolve %s: %v", old, err)
	}
	if want := refs.BlockAddress(treeRef, "r1"); !ok || moved.Address != want {
		t.Errorf("the old address %s resolves to %+v (%t), want the requirement's node in the room, %s", old, moved, ok, want)
	}
	again, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || again.Outcome != IssuePassedOver {
		t.Fatalf("a second conversion of %s answered %+v, %v, want it passed over", issue.key, again, err)
	}
}

// A done issue converts not at all (R22), and an issue one of whose documents still owes its block
// index is deferred, naming that document (R6): neither leaves anything written.
func TestTheConversionPassesOverADoneIssueAndDefersOneThatOwesItsBlockIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	done := createUnconvertedIssue(t, database, service, 1, "", "The done spec.\n", "The done verification.\n", "The done plan.\n")
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, done.key); err != nil {
		t.Fatalf("close %s: %v", done.key, err)
	}
	result, err := service.ConvertIssue(ctx, done.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssuePassedOver || !strings.Contains(result.Reason, "done") {
		t.Fatalf("the conversion of the done issue %s answered %+v, %v, want it passed over because it is done", done.key, result, err)
	}
	requireUnconverted(t, database, done)
	owed := createUnconvertedIssue(t, database, service, 2, "", "The owed spec.\n", "The owed verification.\n", "The owed plan.\n")
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1::uuid`, owed.plan); err != nil {
		t.Fatalf("owe the plan of %s its block index: %v", owed.key, err)
	}
	result, err = service.ConvertIssue(ctx, owed.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssueDeferred || result.Document != owed.plan || !strings.Contains(result.Reason, "plan-md") {
		t.Fatalf("the conversion of %s, whose plan owes its block index, answered %+v, %v, want it deferred naming the plan %s",
			owed.key, result, err, owed.plan)
	}
	requireUnconverted(t, database, owed)
}

// The conversion writes `t19b` into the issue's record (R23): a child that holds no requirement of
// its own takes the view mark and is a view on its ancestor's spec; a child that holds one takes
// no mark, keeps the own-spec mark its index set, and is no view.
func TestTheConversionWritesItsRowInTheIssuesRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	parent := createUnconvertedIssue(t, database, service, 1, "",
		":::requirement{#r1}\nThe parent's requirement.\n:::\n", "The parent's verification.\n", "The parent's plan.\n")
	view := createUnconvertedIssue(t, database, service, 2, parent.key, "The child's design.\n", "The child's verification.\n", "The child's plan.\n")
	own := createUnconvertedIssue(t, database, service, 3, parent.key,
		":::requirement{#r2}\nThe child's own requirement.\n:::\n", "The second child's verification.\n", "The second child's plan.\n")
	for _, child := range []unconvertedIssue{view, own} {
		if result, err := service.ConvertIssue(ctx, child.key, IssueRoomGate, false); err != nil || result.Outcome != IssueConverted {
			t.Fatalf("convert %s: %+v, %v", child.key, result, err)
		}
	}
	for _, want := range []struct {
		issue     string
		marked    bool
		ownSpec   bool
		viewOf    string
		described string
	}{
		{view.key, true, false, parent.key, "a child holding no requirement of its own"},
		{own.key, false, true, "", "a child holding a requirement of its own"},
	} {
		var converted string
		var marked, ownSpec bool
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(converted, ''), view_marked, own_spec_at is not null from issue_views where issue_key = $1
		`, want.issue).Scan(&converted, &marked, &ownSpec); err != nil {
			t.Fatalf("read the record of %s: %v", want.issue, err)
		}
		if converted != "t19b" || marked != want.marked || ownSpec != want.ownSpec {
			t.Errorf("%s, %s, holds converted %q, view_marked %t and an own-spec mark %t, want t19b, %t and %t",
				want.issue, want.described, converted, marked, ownSpec, want.marked, want.ownSpec)
		}
		viewOf, err := ViewOf(ctx, database.Pool, want.issue)
		if err != nil {
			t.Fatalf("read whether %s is a view: %v", want.issue, err)
		}
		if viewOf != want.viewOf {
			t.Errorf("%s, %s, is a view of %q, want %q", want.issue, want.described, viewOf, want.viewOf)
		}
	}
}

// The issue room step converts only while every live process lists the issue room (R5, R6): with
// a live process that lists none, a tick converts nothing and logs that process; once it is gone,
// the next tick converts the issue in flight and passes the done one over, logging one line for
// each, and a later tick takes neither again.
func TestTheIssueRoomStepConvertsOnlyWhileEveryLiveProcessListsTheIssueRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	inFlight := createUnconvertedIssue(t, database, service, 1, "", "The spec in flight.\n", "Its verification.\n", "Its plan.\n")
	done := createUnconvertedIssue(t, database, service, 2, "", "The done spec.\n", "Its verification.\n", "Its plan.\n")
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, done.key); err != nil {
		t.Fatalf("close %s: %v", done.key, err)
	}
	var instance string
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, commit, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), 'an-earlier-build', '{fingerprint}', $1, $2, now() + interval '1 day', $3)
		returning instance::text
	`, pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("add a live process that lists no issue room: %v", err)
	}
	var logged bytes.Buffer
	step := NewIssueRoomStep(service, database, slog.New(slog.NewTextHandler(&logged, nil)))
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("tick beside a process that lists no issue room: %v", err)
	}
	if !strings.Contains(logged.String(), instance) || !strings.Contains(logged.String(), "an-earlier-build") {
		t.Fatalf("the tick beside the process %s logged %q, want a line naming it and its build", instance, logged.String())
	}
	requireUnconverted(t, database, inFlight)
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
		t.Fatalf("take the live process away: %v", err)
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("tick with every live process listing the issue room: %v", err)
	}
	treeOfIssue(t, database, inFlight.key)
	requireUnconverted(t, database, done)
	lines := func(words ...string) int {
		count := 0
		for _, line := range strings.Split(logged.String(), "\n") {
			held := true
			for _, word := range words {
				held = held && strings.Contains(line, word)
			}
			if held {
				count++
			}
		}
		return count
	}
	converted, passed := lines("converted an issue", "issue="+inFlight.key+" "), lines("passed over", "issue="+done.key+" ", "done")
	if converted != 1 || passed != 1 {
		t.Fatalf("the step logged %d lines converting %s and %d passing %s over, want one each: %s",
			converted, inFlight.key, passed, done.key, logged.String())
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("tick once more: %v", err)
	}
	if again, over := lines("converted an issue", "issue="+inFlight.key+" "), lines("passed over", "issue="+done.key+" "); again != 1 || over != 1 {
		t.Fatalf("a later tick took the settled issues again: %d lines converting %s and %d passing %s over", again, inFlight.key, over, done.key)
	}
}

// The command's gate (BuildGate) refuses an unstamped binary, a live process of another build,
// naming that process and its build, and a newest build announcement naming another commit,
// naming it; it lets the conversion run once every live process and the newest announcement are
// of the binary's own commit.
func TestTheBuildGateRefusesAnotherBuildAndAnotherAnnouncement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	run := func(gate ConvertGate) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the gate's transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		return gate(ctx, tx)
	}
	refused := func(err error, words ...string) {
		t.Helper()
		refusal, isRefusal := err.(*ConvertRefusal)
		if !isRefusal {
			t.Fatalf("the gate answered %v, want a refusal naming %v", err, words)
		}
		for _, word := range words {
			if !strings.Contains(refusal.Reason, word) {
				t.Errorf("the gate's refusal %q names no %q", refusal.Reason, word)
			}
		}
	}
	refused(run(BuildGate("")), "-X main.buildCommit")
	const own, other = "commit-of-this-conversion", "commit-of-another-build"
	var instance string
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, commit, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), $1, '{fingerprint}', $2, $3, now() + interval '1 day', $4)
		returning instance::text
	`, other, pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("add a live process of another build: %v", err)
	}
	refused(run(BuildGate(own)), instance, other)
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
		t.Fatalf("take the live process away: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, other); err != nil {
		t.Fatalf("announce another build: %v", err)
	}
	refused(run(BuildGate(own)), other)
	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, own); err != nil {
		t.Fatalf("announce the conversion's own build: %v", err)
	}
	if err := run(BuildGate(own)); err != nil {
		t.Fatalf("the gate refused once the fleet and the newest announcement are of %s: %v", own, err)
	}
}
