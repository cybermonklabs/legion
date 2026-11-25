package docs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
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
	// Advance the old room's cursor without changing any text.
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1::uuid, head.version + n, $2, false
		from (select max(version) as version from doc_updates where artifact_id = $1::uuid) head
		cross join generate_series(1, 8) n
	`, issue.spec, emptyUpdateV1); err != nil {
		t.Fatal(err)
	}
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
		var cursor int64
		var seedExists bool
		if err := database.Pool.QueryRow(ctx, `
			select v.number, v.markdown, v.doc_update_version,
			       exists (select 1 from doc_updates u where u.artifact_id = $2 and u.version = v.doc_update_version)
			from artifact_versions v where v.artifact_id = $1 order by v.number desc limit 1
		`, document, tree).Scan(&number, &markdown, &cursor, &seedExists); err != nil {
			t.Fatalf("read the newest version of %s: %v", document, err)
		}
		if number != 2 || markdown != issue.markdown[document] {
			t.Errorf("the newest version of %s is %d holding %q, want the conversion's version 2 holding the text its room held, %q",
				document, number, markdown, issue.markdown[document])
		}
		if cursor != 1 || !seedExists {
			t.Errorf("version %d of %s points at update %d (exists in the new room: %t), want the seed at 1", number, document, cursor, seedExists)
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
	t.Run("backfill and convert in key order", func(t *testing.T) {
		ctx, database, issue, exec, step, tick, logged := issueRoomStepFixture(t)
		second, first, done := issue(2), issue(1), issue(3)
		exec(`update issues set status = 'done', closed_at = now() where key = $1`, done.key)
		instance := issueRoomOldProcess(ctx, database)
		if instance == "" {
			t.Fatal("no earlier process was inserted")
		}
		tick(step)
		requireUnconverted(t, database, first)
		requireUnconverted(t, database, second)
		if !strings.Contains(logged.String(), instance) || !strings.Contains(logged.String(), "an-earlier-build") {
			t.Fatalf("blocked tick did not log the earlier process: %s", logged.String())
		}
		exec(`insert into issue_links (issue_key, kind, target_key) values ($1, 'blocked_by', $2)`, first.key, done.key)
		exec(`delete from server_processes where instance = $1`, instance)
		tick(step)
		treeOfIssue(t, database, first.key)
		treeOfIssue(t, database, second.key)
		requireUnconverted(t, database, done)
		if count := conversionCount(t, database, "backfilled issue links", `
			select count(*) from issue_links where issue_key = $1 and target_key = $2 and node_address is not null`,
			first.key, done.key); count != 1 {
			t.Fatalf("the step backfilled %d issue links, want 1", count)
		}
		firstLog := strings.Index(logged.String(), "issue="+first.key+" ")
		secondLog := strings.Index(logged.String(), "issue="+second.key+" ")
		if firstLog < 0 || secondLog <= firstLog || strings.Count(logged.Lines("the issue is done"), "\n") != 1 {
			t.Fatalf("the step did not convert %s then %s and pass over %s: %s", first.key, second.key, done.key, logged.String())
		}
		tick(step)
		if strings.Count(logged.Lines("converted an issue"), "\n") != 2 || strings.Count(logged.Lines("the issue is done"), "\n") != 1 {
			t.Fatalf("the later tick did not leave the settled issues alone: %s", logged.String())
		}
	})

	t.Run("a process joins between issues", func(t *testing.T) {
		_, database, issue, exec, step, tick, logged := issueRoomStepFixture(t)
		first, second := issue(1), issue(2)
		var instance string
		step.gate = func(ctx context.Context, tx pgx.Tx) error {
			if err := IssueRoomGate(ctx, tx); err != nil {
				return err
			}
			if instance == "" {
				instance = issueRoomOldProcess(ctx, database)
				if instance == "" {
					return fmt.Errorf("insert the earlier process")
				}
			}
			return nil
		}
		tick(step)
		treeOfIssue(t, database, first.key)
		requireUnconverted(t, database, second)
		if !strings.Contains(logged.String(), instance) {
			t.Fatalf("the second issue's gate did not log the earlier process: %s", logged.String())
		}
		exec(`delete from server_processes where instance = $1`, instance)
		tick(step)
		treeOfIssue(t, database, second.key)
	})

	t.Run("failed conversion waits for next boot", func(t *testing.T) {
		_, database, issue, exec, step, tick, logged := issueRoomStepFixture(t)
		failed, other := issue(1), issue(2)
		exec(`create function reject_join() returns trigger language plpgsql as $$
			begin if new.artifact_id = '` + failed.spec + `'::uuid then raise exception 'join refused'; end if; return new; end $$`)
		exec(`create trigger reject_join before insert on document_rooms for each row execute function reject_join()`)
		tick(step)
		requireUnconverted(t, database, failed)
		treeOfIssue(t, database, other.key)
		if strings.Count(logged.Lines("join refused"), "\n") != 1 {
			t.Fatalf("the step did not log the failing issue: %s", logged.String())
		}
		exec(`drop trigger reject_join on document_rooms`)
		tick(step)
		requireUnconverted(t, database, failed)
		tick(NewIssueRoomStep(step.service, database, step.logger))
		treeOfIssue(t, database, failed.key)
	})

	t.Run("only the deferred issue waits", func(t *testing.T) {
		_, database, issue, exec, step, tick, logged := issueRoomStepFixture(t)
		deferred, other, unrelated := issue(1), issue(2), issue(3)
		exec(`update issues set status = 'done', closed_at = now() where key = $1`, unrelated.key)
		exec(`update document_block_versions set store_owed_at = now(), store_owed_rule = $2
			where artifact_id = $1`, deferred.plan, refs.BlockIndexMarkingRule)
		exec(`update document_block_versions set version = null where artifact_id = $1`, unrelated.spec)
		tick(step)
		requireUnconverted(t, database, deferred)
		treeOfIssue(t, database, other.key)
		tick(step) // The scan after converting the other issue.
		if step.owed {
			t.Fatal("the deferred issue kept the whole list owed")
		}
		tick(step) // A probe of the deferred issue only.
		if lines := logged.Lines("deferred an issue"); strings.Count(lines, "\n") != 1 || !strings.Contains(lines, deferred.plan) {
			t.Fatalf("the step did not log the exact waiting document once: %s", logged.String())
		}
		exec(`update document_block_versions set store_owed_at = null, store_owed_rule = null where artifact_id = $1`, deferred.plan)
		tick(step)
		treeOfIssue(t, database, deferred.key)
		requireUnconverted(t, database, unrelated)
	})

	t.Run("concurrent steps take one lock", func(t *testing.T) {
		ctx, database, issue, _, step, tick, logged := issueRoomStepFixture(t)
		first, second := issue(1), issue(2)
		other := NewIssueRoomStep(step.service, database, step.logger)
		inside, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		step.gate = func(ctx context.Context, tx pgx.Tx) error {
			if len(logged.Lines("converted an issue")) == 0 {
				close(inside)
				<-release
			}
			return IssueRoomGate(ctx, tx)
		}
		go func() { finished <- step.Tick(ctx) }()
		testwait.Receive(t, "the first step to hold its lock", inside)
		func() {
			defer close(release)
			tick(other)
			requireUnconverted(t, database, second)
		}()
		if err := testwait.Receive(t, "the first step to convert", finished); err != nil {
			t.Fatal(err)
		}
		tick(other)
		treeOfIssue(t, database, first.key)
		treeOfIssue(t, database, second.key)
		if strings.Count(logged.Lines("converted an issue"), "\n") != 2 {
			t.Fatalf("expected two conversions across both steps: %s", logged.String())
		}
	})
}

func issueRoomOldProcess(ctx context.Context, database *store.Store) string {
	var instance string
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, commit, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), 'an-earlier-build', '{fingerprint}', $1, $2, now() + interval '1 day', $3)
		returning instance::text
	`, pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol).Scan(&instance); err != nil {
		return ""
	}
	return instance
}

func issueRoomStepFixture(t *testing.T) (context.Context, *store.Store, func(int) unconvertedIssue,
	func(string, ...any), *IssueRoomStep, func(*IssueRoomStep), *logtest.Buffer) {
	t.Helper()
	ctx := t.Context()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	logged := new(logtest.Buffer)
	step := NewIssueRoomStep(service, database, slog.New(slog.NewTextHandler(logged, nil)))
	issue := func(n int) unconvertedIssue {
		t.Helper()
		return createUnconvertedIssue(t, database, service, n, "", "The spec.\n", "Its verification.\n", "Its plan.\n")
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	tick := func(step *IssueRoomStep) {
		t.Helper()
		if err := step.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, database, issue, exec, step, tick, logged
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

func TestTheConversionWritesEachCarriedApprovalBeforeItsVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "",
		":::requirement{#shared}\nKeep the requirement's words.\n:::\n",
		":::check{#shared}\nKeep the check's words.\n:::\n", "The plan.\n")
	approveStoredVersion(t, database, issue.spec, 1, "alice")
	// The previous row shape predates the pin-on-insert trigger.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`alter table artifact_reviews disable trigger artifact_reviews_content_sha256_insert`,
		`insert into artifact_reviews (artifact_id, version, state, actor)
		 values ('` + issue.verification + `', 1, 'approved', '{"kind":"user","id":"bob"}')`,
		`alter table artifact_reviews enable trigger artifact_reviews_content_sha256_insert`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if filled, err := FillReviewContentHashes(ctx, database.Pool); err != nil || filled.Filled != 1 {
		t.Fatalf("pin the legacy approval: %+v, %v", filled, err)
	}
	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssueConverted {
		t.Fatalf("convert %s: %+v, %v", issue.key, result, err)
	}
	for _, document := range []struct{ name, id, reviewer string }{
		{"spec", issue.spec, "alice"}, {"verification", issue.verification, "bob"},
	} {
		order := readCarriedOrder(t, database, document.name, document.id, 2)
		requireApprovalFirst(t, document.name, order)
		if order.reviewSeq == nil || *order.reviewSeq != order.approvedSeq ||
			order.reviewDigest == nil || order.approvedDigest == nil || *order.reviewDigest != *order.approvedDigest {
			t.Errorf("%s: the carried review and approval event disagree: %+v", document.name, order)
		}
		review, err := NewestReview(ctx, database.Pool, document.id)
		if err != nil || review == nil || review.Actor.ID != document.reviewer || review.Version != 2 ||
			review.CarriedFromVersion == nil || *review.CarriedFromVersion != 1 {
			t.Errorf("%s: want the carry from version 1 to 2 in %s's name, got %+v, %v", document.name, document.reviewer, review, err)
		}
	}
}

func TestTheConversionPassesOverUnreadableOrRewordedDocuments(t *testing.T) {
	t.Parallel()
	const original = ":::requirement{#r1}\nThe original words.\n:::\n"
	for _, tc := range []struct{ name, stored, reason string }{
		{"unreadable", "- x\n  :::callout{}\n\n  [^g]: d\n", "does not read back"},
		{"reworded", ":::requirement{#r1}\nOther words.\n:::\n", "would change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			database := storetest.Open(t)
			service := newTestServiceOn(t, database)
			issue := createUnconvertedIssue(t, database, service, 1, "", original, "Verification.\n", "Plan.\n")
			if _, err := database.Pool.Exec(ctx, `update artifact_versions set markdown = $2 where artifact_id = $1`, issue.spec, tc.stored); err != nil {
				t.Fatal(err)
			}
			result, err := service.ConvertIssue(ctx, issue.key, func(context.Context, pgx.Tx) error {
				return &ConvertRefusal{Reason: "gate must not run"}
			}, false)
			if err != nil || result.Outcome != IssuePassedOver || !strings.Contains(result.Reason, tc.reason) {
				t.Fatalf("conversion answered %+v, %v, want pass-over naming %q before the gate", result, err, tc.reason)
			}
			requireUnconverted(t, database, issue)
		})
	}
}

func TestTheConversionReportsACensusDifferenceAndRollsBack(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "",
		":::requirement{#r1}\nKeep the requirement.\n:::\n", "Verification.\n", "Plan.\n")
	for _, statement := range []string{
		`create function omit_redirect() returns trigger language plpgsql as $$
		begin if new.kind = 'moved_to' and new.from_id = 'DOC-1/spec#r1' then return null; end if; return new; end $$`,
		`create trigger omit_redirect before insert on graph_tuples for each row execute function omit_redirect()`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssuePassedOver || !strings.Contains(result.Reason, "block redirects: 1 difference(s), first at DOC-1/spec#r1") {
		t.Fatalf("conversion answered %+v, %v, want the missing redirect reported", result, err)
	}
	requireUnconverted(t, database, issue)
	if version := latestVersionNumber(t, service, issue.spec); version != 1 {
		t.Errorf("the failed census left spec version %d, want only version 1", version)
	}
}

func TestTheConversionKeepsRepeatedTypedBlockIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "",
		":::requirement{#same}\nFirst requirement words.\n:::\n\n:::requirement{#same}\nOther requirement words.\n:::\n",
		"Verification.\n", "Plan.\n")
	if strings.Count(issue.markdown[issue.spec], "#same") != 2 {
		t.Fatal("the old room must contain two typed blocks with the same ID")
	}
	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssueConverted {
		t.Fatalf("convert a document whose ID repeats: %+v, %v", result, err)
	}
	tree, _ := treeOfIssue(t, database, issue.key)
	var indexed, markdown string
	if err := database.Pool.QueryRow(ctx, `
		select t.text, v.markdown
		from document_block_texts t join artifact_versions v on v.artifact_id = $2 and v.number = t.version
		where t.artifact_id = $1 and t.block_id = 'same'`, tree, issue.spec).Scan(&indexed, &markdown); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexed, "First requirement words.") || strings.Contains(indexed, "Other requirement words.") || markdown != issue.markdown[issue.spec] {
		t.Errorf("the conversion indexed %q and wrote %q, want the first block indexed and the full text unchanged", indexed, markdown)
	}
}
