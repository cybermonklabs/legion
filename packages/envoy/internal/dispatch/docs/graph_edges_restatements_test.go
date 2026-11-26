package docs

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// graphEdgesMigrations is where the migrations that state graph_edges are, and
// graphEdgesFirstStoreRestatement the first of them whose tuple arm reads the store: every
// restatement from it on is held below to answering no row the view does not.
const (
	graphEdgesMigrations            = "../store/migrations"
	graphEdgesFirstStoreRestatement = "20261109215947"
)

// graphEdgesHeads are the restatements two lines of migrations each ended with before they were
// merged, each re-stating the whole view from its own arm list: the one that moved the asks into
// the store and keeps only the kinds the tuple arm renders before it sorts them, and the one that
// moved the messages, the comments' pointers and every item's blocks into the store and added the
// identity arm. The last restatement states both, and the view it leaves answers exactly what the
// two answered between them. A later merge of two such lines names its own two heads here. The
// second line's migrations are numbered after the first's (20261126045001 to 20261126045005 after
// 20261125163554), so the union is the last migration of the two.
var graphEdgesHeads = []string{
	"20261125163554_graph_edges_asks_and_tuple_arm_filter.up.sql",
	"20261126045004_graph_edges_identity_mentions.up.sql",
}

// restatedViewName is the view a restatement's own statement is created as beside graph_edges.
func restatedViewName(file string) string {
	version, _, _ := strings.Cut(file, "_")
	return "restated_" + version
}

// graphEdgesRestatements is every migration from graphEdgesFirstStoreRestatement on that states
// graph_edges, in the order a database applies them, read from the migrations themselves rather
// than from a list, so a restatement a later change adds is held the day it lands.
func graphEdgesRestatements(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(graphEdgesMigrations)
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	timestamped := regexp.MustCompile(`^\d{14}_.*\.up\.sql$`)
	var restated []string
	for _, entry := range entries {
		name := entry.Name()
		if !timestamped.MatchString(name) || name[:14] < graphEdgesFirstStoreRestatement {
			continue
		}
		text, err := os.ReadFile(filepath.Join(graphEdgesMigrations, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(text), graphEdgesStatement) {
			restated = append(restated, name)
		}
	}
	return restated
}

// Each restatement of graph_edges replaces the whole view, and a database applies the migrations
// it has not recorded in version order, so the view it ends with is the last restatement's alone:
// an arm or a test an earlier one added lasts only where the last states it again. Over one store
// and one set of tables that exercises every arm - the documents, comments, message and ask the
// cutover fixture writes (seedGraphEdgeFixture), the asks and followers of the asks fixture
// (seedAskArmsFixture), the mentions of a person, an agent's session and a role out of a
// document's block, a comment and a message, and an issue's child, its blocking issue and its
// components - each restatement's own statement is created as a view beside graph_edges, and at
// each of three readings - before the backfill, with one document's store unwritten and the rows
// written before the store unmarked; after it; and beside rows the previous release wrote - the
// test holds:
//   - graph_edges answers every row each restatement from the store's first on answers, as often,
//     with its time and its causing write: no restatement's arm was dropped by a later one;
//   - graph_edges answers exactly the rows the two heads (graphEdgesHeads) answer between them,
//     each as often as the head that answers it most;
//   - across the three readings every arm of graph_edges and of each head answers a row, so
//     neither comparison passes over an arm the fixture left empty.
//
// It holds as well that the last restatement is no head: the union of the two must date after
// both, or a database that ran the heads in version order would end on one of them.
func TestGraphEdgesAnswersEveryRowEachRestatementOfItAnswered(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	restated := graphEdgesRestatements(t)
	for _, head := range graphEdgesHeads {
		if !slices.Contains(restated, head) {
			t.Fatalf("the head %s states no graph_edges among the restatements %v", head, restated)
		}
	}
	if last := restated[len(restated)-1]; slices.Contains(graphEdgesHeads, last) {
		t.Fatalf("the last restatement of graph_edges is the head %s, which states one line's arms alone: the union of %v must date after both",
			last, graphEdgesHeads)
	}
	earlier := restated[:len(restated)-1]
	for _, file := range earlier {
		createViewFrom(t, database, filepath.Join(graphEdgesMigrations, file), restatedViewName(file))
	}
	t.Logf("graph_edges is %s's; held to %v", restated[len(restated)-1], earlier)

	fixture := seedGraphEdgeFixture(t, database, service)
	seedAskArmsFixture(t, database, service)
	seedIdentityMentions(t, database, service)
	seedStructuralRows(t, database)
	unwriteTheStoreOfADocument(t, database, graphEdgeLegacyRefKey)

	views := []string{"graph_edges"}
	for _, head := range graphEdgesHeads {
		views = append(views, restatedViewName(head))
	}
	arms := make(map[string][]movedArm, len(views))
	answered := make(map[string][]int, len(views))
	for _, view := range views {
		for index, arm := range readViewArms(t, database, view) {
			arms[view] = append(arms[view], movedArm{place: index + 1, graphEdgesArm: arm})
		}
		answered[view] = make([]int, len(arms[view]))
	}

	for _, reading := range []struct {
		stage string
		first func()
	}{
		{stage: "before the backfill"},
		{stage: "after the backfill", first: func() {
			if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
				t.Fatalf("backfill the store: %v", err)
			}
		}},
		{stage: "beside rows the previous release wrote", first: func() { writeAsThePreviousRelease(t, database, fixture) }},
	} {
		if reading.first != nil {
			reading.first()
		}
		view := graphEdgeRows(t, database, "graph_edges")
		if len(view) == 0 {
			t.Fatalf("%s: graph_edges answers no row, so this test would prove nothing", reading.stage)
		}
		viewCounts := edgeCounts(view)
		// Each head is one of the earlier restatements, so its counts are the ones read here.
		perFile := make(map[string]map[string]int, len(earlier))
		for _, file := range earlier {
			perFile[file] = edgeCounts(graphEdgeRows(t, database, restatedViewName(file)))
			for row, count := range perFile[file] {
				if viewCounts[row] < count {
					t.Errorf("%s: %q: %s answers it %d times, and graph_edges %d: a later restatement dropped what that one answers",
						reading.stage, row, file, count, viewCounts[row])
				}
			}
		}
		heads := map[string]int{}
		for _, head := range graphEdgesHeads {
			for row, count := range perFile[head] {
				heads[row] = max(heads[row], count)
			}
		}
		for _, row := range slices.Sorted(maps.Keys(viewCounts)) {
			if viewCounts[row] != heads[row] {
				t.Errorf("%s: %q: graph_edges answers it %d times, and the heads %v at most %d",
					reading.stage, row, viewCounts[row], graphEdgesHeads, heads[row])
			}
		}
		for _, row := range slices.Sorted(maps.Keys(heads)) {
			if viewCounts[row] == 0 {
				t.Errorf("%s: %q: the heads answer it %d times at most, and graph_edges never", reading.stage, row, heads[row])
			}
		}
		// An arm that has answered a row at an earlier reading is held already.
		for _, view := range views {
			for index, arm := range arms[view] {
				if answered[view][index] > 0 {
					continue
				}
				answered[view][index] += len(armAnswers(t, database, arm))
			}
		}
	}
	for _, view := range views {
		for index, arm := range arms[view] {
			if answered[view][index] == 0 {
				t.Errorf("arm %d of %s (%s) answers no row at any reading, so neither comparison holds it to anything:\n%s",
					arm.place, view, arm.relation, arm.text)
			}
		}
	}
}

// seedIdentityMentions writes a mention of each identity kind the identity arm answers, through
// the paths that write one: a document of CORE-1 whose block names a person, an agent's session
// and a role, indexed by the block-index tick, and a comment and a message on CORE-1 whose bodies
// name the same three, each indexed in its write's own transaction as this build indexes it.
func seedIdentityMentions(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	const body = "Over to dispatch://person/person-1@example.com, dispatch://agent/session-1 and dispatch://role/reviewer.\n"
	document := createOwedDocument(t, database, service, "CORE-1", "", "identities", "# Identities\n\n"+body, "")
	settle(t, database, document)
	if ticks := drainBlockIndex(t, service, "the document naming identities"); ticks < 2 {
		t.Fatal("the fixture indexed no document naming identities")
	}
	writeCommentAsThisBuild(t, database, graphstore.CommentPointers{}, body)
	writeMessageAsThisBuild(t, database, "", "", body)
}

// seedStructuralRows writes the rows of the arms no item writer fills: CORE-2 a child of CORE-1
// and blocked by it, and a component tree of CORE that CORE-1 affects.
func seedStructuralRows(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = 'CORE-1' where key = 'CORE-2';
		insert into issue_links (issue_key, kind, target_key) values ('CORE-2', 'blocked_by', 'CORE-1');
		with source as (
			insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
			values ('CORE', 'acme/core', 'main', 1, '{"kind":"user","id":"alice"}') returning id
		), snapshot as (
			insert into architecture_snapshots (project_key, commit, files) values ('CORE', 'abc123', '{}'::jsonb) returning id
		)
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
		select 'CORE', c.id, c.id, '', c.parent, snapshot.id, source.id
		from source, snapshot, (values ('core', null), ('api', 'core')) c (id, parent);
		insert into component_depends (project_key, from_id, to_id) values ('CORE', 'api', 'core');
		insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit');
		insert into issue_component_members (issue_key, project_key, component_id) values ('CORE-1', 'CORE', 'core');
	`); err != nil {
		t.Fatalf("seed the structural rows: %v", err)
	}
}
