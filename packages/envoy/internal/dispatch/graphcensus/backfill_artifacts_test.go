package graphcensus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheBackfillHangsAnOlderArtifactUnderItsIssue is the backfill's half of R16 of
// the issue-tree spec (T2a of its plan), under the graph-store spec's r-view-end contract. A
// database holds the rows a build before this one wrote, by SQL in their old shape: an issue, its
// markdown document and its image of two versions, and a project's document, none with a node or a
// marker; and a document a build of the closed-project spec wrote, whose node stands with no
// part_of and whose row names none. The run gives each row its node and names it on the row; each
// of the issue's documents is part_of its issue, dated at its row's own created_at and made by its
// row's author, at the sentinel; the project's document hangs under nothing; and the image's node
// holds the mime, the size and the sha256 of its newest version, dated at that version. A second
// run writes no node, no tuple and no row.
func TestTheBackfillHangsAnOlderArtifactUnderItsIssue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	created := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	// One statement of several takes no parameter, so the time is written in: it is created's.
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('OLD', 'Older', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank, created_at)
		values ('OLD-1', 'OLD', 1, 'One', '{"kind":"user","id":"alice"}', 'U', '2026-11-01 12:00:00+00');
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
		values ('OLD-1', 'OLD', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', '2026-11-01 12:00:00+00'),
		       ('OLD-1', 'OLD', 'shot-png', 'shot.png', 'image', '{"kind":"user","id":"alice"}', '2026-11-01 12:00:00+00'),
		       (null, 'OLD', 'runbook-md', 'Runbook.md', 'doc', '{"kind":"user","id":"alice"}', '2026-11-01 12:00:00+00');
	`); err != nil {
		t.Fatalf("seed the issue and its documents: %v", err)
	}
	first, second := []byte("\x89PNG\r\n\x1a\nthe first picture"), []byte("\x89PNG\r\n\x1a\nthe second, longer picture")
	secondAt := created.Add(2 * time.Minute)
	for _, version := range []struct {
		number  int
		content []byte
		authors string
		at      time.Time
	}{
		{1, first, `[{"kind":"user","id":"alice"}]`, created.Add(time.Minute)},
		{2, second, `[{"kind":"user","id":"bob"}]`, secondAt},
	} {
		sum := sha256.Sum256(version.content)
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, created_at)
			select id, $1, $2, 'image/png', $3, $4, $5::jsonb, $6 from artifacts where ref_key = 'OLD-1/shot-png'
		`, version.number, version.content, len(version.content), hex.EncodeToString(sum[:]), version.authors, version.at); err != nil {
			t.Fatalf("seed version %d of the image: %v", version.number, err)
		}
	}
	// An image whose ref_key is inside the store's bound but whose version's address, `@v1` after
	// it, is past it: its version is no node of the store and no row the version pass reads, so the
	// object pass states its bytes.
	long := "OLD-1/" + strings.Repeat("s", graphstore.MaxAddressBytes-len("OLD-1/")-1)
	longContent := []byte("\x89PNG\r\n\x1a\nthe picture under a long name")
	longAt := created.Add(3 * time.Minute)
	longSum := sha256.Sum256(longContent)
	if _, err := database.Pool.Exec(ctx, `
		with made as (
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
			values ('OLD-1', 'OLD', $1, $1, 'image', '{"kind":"user","id":"alice"}', $2)
			returning id
		)
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, created_at)
		select id, 1, $3, 'image/png', $4, $5, '[{"kind":"user","id":"alice"}]', $6 from made
	`, strings.TrimPrefix(long, "OLD-1/"), created, longContent, len(longContent), hex.EncodeToString(longSum[:]), longAt); err != nil {
		t.Fatalf("seed the image under a long name: %v", err)
	}
	if !graphstore.AddressFits(long) || graphstore.AddressFits(graphstore.VersionID(long, 1)) {
		t.Fatalf("the long image's ref_key fits %t and its version's address fits %t, want the ref_key inside the bound and the version past it",
			graphstore.AddressFits(long), graphstore.AddressFits(graphstore.VersionID(long, 1)))
	}
	// The closed-project spec's upload wrote the document's node before its row and named it
	// nowhere, and stated no part_of: its row's missing marker is what the run walks.
	drivenTx(t, database, "seed the document a build of the closed-project spec wrote", func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.ArtifactNode("OLD-1/plan-md", "OLD", "", alice, created),
		}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
			values ('OLD-1', 'OLD', 'plan-md', 'plan.md', 'doc', '{"kind":"user","id":"alice"}', $1)
		`, created)
		return err
	})
	documents := []string{"OLD-1/notes-md", "OLD-1/shot-png", "OLD-1/plan-md", "OLD/runbook-md", long}
	for _, refKey := range documents {
		if partOf := artifactPartOf(t, database, refKey); len(partOf) != 0 {
			t.Fatalf("before the run %s is part_of %+v, want nothing: the corpus holds no tuple of it", refKey, partOf)
		}
	}

	var out bytes.Buffer
	if _, err := Backfill(ctx, database, &out); err != nil {
		t.Fatalf("run the backfill: %v\n%s", err, out.String())
	}

	if kind, address := rowMarker(t, database, `select node_kind, node_address from issues where key = $1`, "OLD-1"); kind != graphstore.KindIssue || address != "OLD-1" {
		t.Errorf("after the run issue OLD-1 names node %q/%q, want %s/OLD-1", kind, address, graphstore.KindIssue)
	}
	for _, refKey := range documents {
		if kind, address := rowMarker(t, database, `select node_kind, node_address from artifacts where ref_key = $1`, refKey); kind != graphstore.KindArtifact || address != refKey {
			t.Errorf("after the run %s names node %q/%q, want %s/%s", refKey, kind, address, graphstore.KindArtifact, refKey)
		}
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, refKey); err != nil || !held {
			t.Errorf("after the run the store holds no node of %s (held %t, %v)", refKey, held, err)
		}
		partOf := artifactPartOf(t, database, refKey)
		if refKey == "OLD/runbook-md" {
			if len(partOf) != 0 {
				t.Errorf("the project's document %s is part_of %+v, want nothing: no issue owns it", refKey, partOf)
			}
			continue
		}
		if len(partOf) != 1 {
			t.Errorf("after the run %s is part_of %+v, want one tuple to issue:OLD-1", refKey, partOf)
			continue
		}
		if stated := partOf[0]; stated.ToKind != graphstore.KindIssue || stated.ToID != "OLD-1" || !stated.Asserted ||
			!stated.At.Equal(created) || stated.Who != alice || stated.SourceEvent != graphstore.NoEvent {
			t.Errorf("after the run %s's part_of is %+v, want issue:OLD-1 at the row's created_at %s, by %v, at the sentinel",
				refKey, stated, created, alice)
		}
	}

	// The image's node holds its newest version's bytes, dated at that version and by its author;
	// the markdown documents hold none.
	sum := sha256.Sum256(second)
	for attribute, want := range map[string]string{
		graphstore.AttributeMIME:   "image/png",
		graphstore.AttributeSize:   strconv.Itoa(len(second)),
		graphstore.AttributeSHA256: hex.EncodeToString(sum[:]),
	} {
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, "OLD-1/shot-png", attribute)
		if err != nil {
			t.Fatalf("read the %s of the image: %v", attribute, err)
		}
		if len(history) != 1 || history[0].ToKind != graphstore.KindValue || history[0].ToID != want ||
			!history[0].At.Equal(secondAt) || history[0].Who != bob || history[0].SourceEvent != graphstore.NoEvent {
			t.Errorf("after the run the image states %s %+v, want %q once, at its newest version's %s, by %v, at the sentinel",
				attribute, history, want, secondAt, bob)
		}
		for _, document := range []string{"OLD-1/notes-md", "OLD-1/plan-md", "OLD/runbook-md"} {
			if stated, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, document, attribute); err != nil || len(stated) != 0 {
				t.Errorf("the markdown document %s states %s %+v (%v), want none", document, attribute, stated, err)
			}
		}
	}
	// The long image's node holds its version's bytes, which the object pass stated, though no node
	// of that version stands, and the census reads the row and its node alike, its version counted
	// past the bound.
	for attribute, want := range map[string]string{
		graphstore.AttributeMIME:   "image/png",
		graphstore.AttributeSize:   strconv.Itoa(len(longContent)),
		graphstore.AttributeSHA256: hex.EncodeToString(longSum[:]),
	} {
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, long, attribute)
		if err != nil {
			t.Fatalf("read the %s of the long image: %v", attribute, err)
		}
		if len(history) != 1 || history[0].ToID != want || !history[0].At.Equal(longAt) || history[0].Who != alice {
			t.Errorf("after the run the long image states %s %+v, want %q once, at its version's %s, by %v",
				attribute, history, want, longAt, alice)
		}
	}
	if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, graphstore.VersionID(long, 1)); err != nil || held {
		t.Errorf("the store holds a node of the long image's version past the bound (held %t, %v), want none", held, err)
	}
	report := storeCensusOf(t, database)
	for _, name := range []string{graphstore.KindArtifact, graphstore.KindVersion} {
		kind := censusKind(t, report, name)
		for _, difference := range differencesOf(kind) {
			t.Errorf("after the run the census reads a difference of kind %s at %s: %s", name, difference.At, difference.Cause)
		}
		if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("after the run the census of %s reads %d row and %d node difference(s), want none", name, kind.RowDifferences, kind.NodeDifferences)
		}
	}
	if versions := censusKind(t, report, graphstore.KindVersion); versions.PastBound != 1 {
		t.Errorf("the census counts %d version(s) past the bound, want the long image's one", versions.PastBound)
	}

	// A second run reads no row the first marked.
	before := takeArtifactSnapshot(t, database)
	again, err := Backfill(ctx, database, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	for _, done := range again.Kinds {
		if done.Rows != 0 || done.Nodes != 0 || done.Tuples != 0 || !done.Complete {
			t.Errorf("the second run reports %#v for %s, want nothing written and nothing remaining", done, done.Kind)
		}
	}
	after := takeArtifactSnapshot(t, database)
	if after.nodes != before.nodes || after.tuples != before.tuples || after.highest != before.highest {
		t.Errorf("the second run left %d nodes, %d tuples and the highest sequence %d, want the %d, %d and %d the first left",
			after.nodes, after.tuples, after.highest, before.nodes, before.tuples, before.highest)
	}
	for _, written := range rowsRewritten(before.versions, after.versions) {
		t.Errorf("the second run wrote %s: it reads and writes no marked row", written)
	}
	// The check's control, over the rows the second run left: a row it had added reads as a row it
	// wrote, as a row it rewrote does, and so does a row it had taken away.
	added, rewritten, removed := maps.Clone(after.versions), maps.Clone(after.versions), maps.Clone(after.versions)
	added["issues PLANTED-1"] = "1"
	rewritten["issues OLD-1"] += "0"
	delete(removed, "issues OLD-1")
	for _, control := range []struct {
		name     string
		versions map[string]string
		want     string
	}{
		{"an added row", added, "issues PLANTED-1 (row version none, then 1)"},
		{"a rewritten row", rewritten, "issues OLD-1 (row version " + after.versions["issues OLD-1"] + ", then " + rewritten["issues OLD-1"] + ")"},
		{"a removed row", removed, "issues OLD-1 (row version " + after.versions["issues OLD-1"] + ", then none)"},
	} {
		if got := rowsRewritten(after.versions, control.versions); !slices.Equal(got, []string{control.want}) {
			t.Errorf("the second run's check over %s reads %q, want %q", control.name, got, control.want)
		}
	}
}

// rowsRewritten is each row whose version after holds apart from before, sorted: a row whose
// version moved, a row after holds that before did not, and a row before held that after does not,
// each with its version on both sides ("none" where a side holds no such row).
func rowsRewritten(before, after map[string]string) []string {
	version := func(versions map[string]string, row string) string {
		if held, ok := versions[row]; ok {
			return held
		}
		return "none"
	}
	rows := slices.Collect(maps.Keys(before))
	for row := range after {
		if _, held := before[row]; !held {
			rows = append(rows, row)
		}
	}
	slices.Sort(rows)
	var written []string
	for _, row := range rows {
		if was, is := version(before, row), version(after, row); was != is {
			written = append(written, row+" (row version "+was+", then "+is+")")
		}
	}
	return written
}

// artifactPartOf answers every part_of tuple out of the document's node, in the order appended.
func artifactPartOf(t *testing.T, database *store.Store, refKey string) []graphstore.Tuple {
	t.Helper()
	tuples, err := graphstore.History(context.Background(), database.Pool, graphstore.KindArtifact, refKey, graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the part_of of %s: %v", refKey, err)
	}
	return tuples
}

// rowMarker reads the node pair one row names, empty where it names none.
func rowMarker(t *testing.T, database *store.Store, read, key string) (kind, address string) {
	t.Helper()
	var nodeKind, nodeAddress *string
	if err := database.Pool.QueryRow(context.Background(), read, key).Scan(&nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the marker of %s: %v", key, err)
	}
	if nodeKind != nil {
		kind = *nodeKind
	}
	if nodeAddress != nil {
		address = *nodeAddress
	}
	return kind, address
}

// artifactSnapshot is what a second run must leave as it found: the store's counts and highest
// sequence, and the row version (xmin) of every issues, artifacts and graph_nodes row, which a
// statement writing the row moves even where it writes the values it read.
type artifactSnapshot struct {
	nodes, tuples, highest int64
	versions               map[string]string
}

func takeArtifactSnapshot(t *testing.T, database *store.Store) artifactSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := artifactSnapshot{versions: map[string]string{}}
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes), (select count(*) from graph_tuples),
		       (select coalesce(max(seq), 0) from graph_tuples)
	`).Scan(&snapshot.nodes, &snapshot.tuples, &snapshot.highest); err != nil {
		t.Fatalf("count the store's rows: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `
		select 'issues ' || key, xmin::text from issues
		union all select 'artifacts ' || ref_key, xmin::text from artifacts
		union all select 'graph_nodes ' || kind || ' ' || id, xmin::text from graph_nodes
	`)
	if err != nil {
		t.Fatalf("read the rows the backfill writes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row, version string
		if err := rows.Scan(&row, &version); err != nil {
			t.Fatalf("scan the rows the backfill writes: %v", err)
		}
		snapshot.versions[row] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows the backfill writes: %v", err)
	}
	return snapshot
}

// The object pass writes the node only of a row still there once it holds the row's owner
// (writeObjectBatch, heldObjects). A second transaction stands for the removal of an issue's
// document (api's removeArtifact): it takes the issue's owner lock (store.LockOwners), as the
// removal does through beginArtifactWrite, and deletes the document's row. The backfill starts
// while that transaction is open, so its read of the batch sees the row, and the test reads it
// waiting at the owner lock before it lets the removal commit. A pass that wrote from its first
// read would bring the removed document back as a live node hanging under its issue, with no row.
func TestTheBackfillWritesNoNodeForADocumentRemovedWhileItWaited(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedMarkedLockIssue(t, database)
	const removed, kept = "LOCK-1/removed-md", "LOCK-1/kept-md"
	if _, err := database.Pool.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('LOCK-1', 'LOCK', 'removed-md', 'removed.md', 'doc', '{"kind":"user","id":"alice"}'),
		       ('LOCK-1', 'LOCK', 'kept-md', 'kept.md', 'doc', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("seed the two documents an older build wrote: %v", err)
	}

	removal, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the removal: %v", err)
	}
	defer removal.Rollback(ctx)
	if err := store.LockOwners(ctx, removal, store.IssueOwner("LOCK-1")); err != nil {
		t.Fatalf("the removal could not take the issue's owner lock: %v", err)
	}
	if _, err := removal.Exec(ctx, `delete from artifacts where ref_key = $1`, removed); err != nil {
		t.Fatalf("the removal could not delete the document: %v", err)
	}

	pid, answered := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, removal, pid, answered); done {
		t.Fatalf("the backfill answered (report %#v, err %v) without waiting on the removal's owner lock", early.report, early.err)
	}
	t.Logf("the backfill waits behind the removal at: %s", lockstep.WaitingStatement(t, database, pid))
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}
	report := awaitBackfill(t, answered)

	var nodes, tuples int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2)
	`, graphstore.KindArtifact, removed).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("read what the store holds of %s: %v", removed, err)
	}
	if nodes != 0 || tuples != 0 {
		t.Errorf("the backfill wrote %d node(s) and %d tuple(s) for %s, a document removed while the backfill waited: it came back as a node no row names",
			nodes, tuples, removed)
	}
	if kind, address := rowMarker(t, database, `select node_kind, node_address from artifacts where ref_key = $1`, kept); kind != graphstore.KindArtifact || address != kept {
		t.Errorf("the kept document names node %q/%q, want %s/%s", kind, address, graphstore.KindArtifact, kept)
	}
	if partOf := artifactPartOf(t, database, kept); len(partOf) != 1 || partOf[0].ToID != "LOCK-1" {
		t.Errorf("the kept document is part_of %+v, want one tuple to issue:LOCK-1", partOf)
	}
	if done := reportedKind(t, report, graphstore.KindArtifact); done.Rows != 1 || done.Nodes != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for documents, want the kept document's one row and node and every row marked", done)
	}
}

// TestTheObjectBatchTakesTheStoreLockOrder holds writeObjectBatch, which names an issue's and a
// document's node on its row (the issue-tree spec), to the store's lock order, as
// TestEveryBackfillBatchTakesTheStoreLockOrder holds the other batches: the owner row the batch's
// rows stand under first, then the node rows, then, for a document, its own artifacts row. An
// issue's own row is its owner row, so its batch holds it from the start and names the node on it
// without waiting again. The project's row is held throughout, though no batch here takes it.
func TestTheObjectBatchTakesTheStoreLockOrder(t *testing.T) {
	t.Run("an issue's batch takes its row, then its node row", func(t *testing.T) {
		database := storetest.Open(t)
		seedLockProject(t, database)
		if _, err := database.Pool.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LOCK-1', 'LOCK', 1, 'One', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			t.Fatalf("seed the issue: %v", err)
		}
		owner, project := holdLockOwner(t, database)
		node := holdWrite(t, database, &lockstep.Hold{
			Name: "issue LOCK-1's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindIssue, ID: "LOCK-1",
			Statement: "insert into graph_nodes (",
		}, []graphstore.Node{graphstore.IssueNode("LOCK-1", "LOCK", model.Actor{Kind: "user", ID: "alice"}, time.Now())}, nil)

		report := followTheBackfill(t, database, "writeObjectBatch", []*lockstep.Hold{owner, project, node})
		if kind, address := rowMarker(t, database, `select node_kind, node_address from issues where key = $1`, "LOCK-1"); kind != graphstore.KindIssue || address != "LOCK-1" {
			t.Errorf("issue LOCK-1 names node %q/%q, want %s/LOCK-1: the run the test followed is not one that committed", kind, address, graphstore.KindIssue)
		}
		if done := reportedKind(t, report, graphstore.KindIssue); done.Rows != 1 || !done.Complete {
			t.Errorf("the backfill reports %#v for issues, want 1 row and every row marked", done)
		}
	})
	t.Run("a document's batch takes its issue's row, then its node row, then its own row", func(t *testing.T) {
		database := storetest.Open(t)
		ctx := context.Background()
		seedMarkedLockIssue(t, database)
		alice := model.Actor{Kind: "user", ID: "alice"}
		const refKey = "LOCK-1/notes-md"
		if _, err := database.Pool.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('LOCK-1', 'LOCK', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		`); err != nil {
			t.Fatalf("seed the document: %v", err)
		}
		owner, project := holdLockOwner(t, database)
		node := holdWrite(t, database, &lockstep.Hold{
			Name: "the document's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindArtifact, ID: refKey,
			Statement: "insert into graph_nodes (",
		}, []graphstore.Node{graphstore.ArtifactNode(refKey, "LOCK", "", alice, time.Now())}, nil)
		row := lockstep.HoldRow(t, database, &lockstep.Hold{
			Name: "the document's artifacts row", Step: lockstep.StepProjection, Taken: true,
			Statement: "update artifacts set node_kind",
		}, `select 1 from artifacts where ref_key = $1 for no key update`, refKey)

		report := followTheBackfill(t, database, "writeObjectBatch", []*lockstep.Hold{owner, project, node, row})
		if kind, address := rowMarker(t, database, `select node_kind, node_address from artifacts where ref_key = $1`, refKey); kind != graphstore.KindArtifact || address != refKey {
			t.Errorf("%s names node %q/%q, want %s/%s: the run the test followed is not one that committed", refKey, kind, address, graphstore.KindArtifact, refKey)
		}
		if partOf := artifactPartOf(t, database, refKey); len(partOf) != 1 || partOf[0].ToID != "LOCK-1" {
			t.Errorf("%s is part_of %+v, want one tuple to issue:LOCK-1", refKey, partOf)
		}
		if done := reportedKind(t, report, graphstore.KindArtifact); done.Rows != 1 || !done.Complete {
			t.Errorf("the backfill reports %#v for documents, want 1 row and every row marked", done)
		}
	})
}

// seedLockProject writes the project the object batch's lock steps run in.
func seedLockProject(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name, default_assignee) values ('LOCK', 'Lock steps', 'alice')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}

// seedMarkedLockIssue writes project LOCK and its issue LOCK-1 as this build's create writes
// them, the issue's node and then its row naming that node, so the issue pass reads none of its
// rows and the first batch that takes the issue's row is the one over its documents.
func seedMarkedLockIssue(t *testing.T, database *store.Store) {
	t.Helper()
	seedLockProject(t, database)
	ctx := context.Background()
	drivenTx(t, database, "seed the issue", func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.IssueNode("LOCK-1", "LOCK", model.Actor{Kind: "user", ID: "alice"}, time.Now())}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
			values ('LOCK-1', 'LOCK', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'issue', 'LOCK-1')
		`)
		return err
	})
}

// seedLockSpec writes project LOCK, its issue LOCK-1 and the issue's spec as a build before the
// store writes them, rows that name no node, before any decision of the spec, and answers the
// spec's id, its ref_key and the address of its block r1.
func seedLockSpec(t *testing.T, database *store.Store) (document, refKey, pointer string) {
	t.Helper()
	seedLockProject(t, database)
	drivenTx(t, database, "write issue LOCK-1 and its spec", func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LOCK-1', 'LOCK', 1, 'Kept', '{"kind":"user","id":"alice"}', 'U')
		`); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('LOCK-1', 'LOCK', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`).Scan(&document, &refKey)
	})
	issue := "LOCK-1"
	return document, refKey, refs.BlockRef(refs.ArtifactRef(&issue, "LOCK", nil, "spec", "doc", true), "r1")
}

// holdLockOwner holds issue LOCK-1's owner lock, the owner lock every batch here takes first
// (events.LockOwnerRows), at the statement that lock is taken with and before any node or tuple,
// and project LOCK's row, which no batch here takes.
func holdLockOwner(t *testing.T, database *store.Store) (owner, project *lockstep.Hold) {
	t.Helper()
	owner = lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue LOCK-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, store.IssueOwner("LOCK-1"))
	project = lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project LOCK's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for update`, "LOCK")
	return owner, project
}
