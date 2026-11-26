package refs

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestTheNeighbourhoodReadRanksSharedTargetsAndLeavesOutLinkedIssues(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceProject(t, tx, "ELSE")
	actor := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	const (
		parent    = "CORE-1"
		blocker   = "CORE-2"
		filing    = "CORE-3"
		mentioned = "CORE-4"
		duplicate = "CORE-5"
		a         = "CORE-6"
		b         = "CORE-7"
		r         = "CORE-8"
		c         = "CORE-9"
		h         = "CORE-10"
		y         = "CORE-11"
		d         = "CORE-12"
		e         = "ELSE-1"
		component = "CORE/platform"
	)
	// Seed the read's issue projections directly, and state its relations through the graph writer
	// below. The update order of H, C and Y differs from their creation order C, H, Y.
	for number, issue := range []struct {
		key, title, parent, status string
		update                     time.Duration
	}{
		{parent, "Pelican", "", "triage", 40},
		{blocker, "Xylophone", "", "triage", 41},
		{filing, "Lichen", "", "triage", 44},
		{mentioned, "Narwhal", "", "triage", 42},
		{duplicate, "Quartz", "", "triage", 43},
		{a, "Amaranth", parent, "triage", 6},
		{b, "Basalt", parent, "done", 7},
		{r, "Rhodium", "", "done", 8},
		{c, "Cicada", "", "triage", 30},
		{h, "Hyacinth", "", "triage", 29},
		{y, "Yew", mentioned, "triage", 31},
		{d, "Dahlia", parent, "triage", 45},
		{e, "Echidna", "", "triage", 46},
	} {
		project := "CORE"
		if issue.key == e {
			project = "ELSE"
		}
		execFixture(t, tx, "seed "+issue.key, `
			insert into issues (key, project_key, number, title, parent_key, status, created_by, rank, created_at, updated_at)
			values ($1, $2, $3, $4, nullif($5, ''), $6, '{"kind":"user","id":"alice"}', 'U', $7, $8)
		`, issue.key, project, number+1, issue.title, issue.parent, issue.status, at.Add(time.Duration(number)*time.Second), at.Add(issue.update*time.Second))
	}
	ask := scanFixture(t, tx, "seed filing ask", `
		insert into asks (issue_key, author, question) values ($1, '{"kind":"user","id":"alice"}', 'Marmoset?') returning id::text
	`, filing)
	cDoc, cSpec := createReferenceArtifact(t, tx, c, "CORE", "spec")
	_, dSpec := createReferenceArtifact(t, tx, d, "CORE", "spec")
	_, eSpec := createReferenceArtifact(t, tx, e, "ELSE", "spec")
	cBlock := cSpec + "#b1"
	insertDocumentBlock(t, tx, cDoc, "b1", cBlock)

	// The parent owns a component; Y has no attachment of its own.
	source := scanFixture(t, tx, "seed architecture source", `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'example/architecture', 'main', 1, '{"kind":"user","id":"alice"}') returning id::text
	`)
	snapshot := scanFixture(t, tx, "seed architecture snapshot", `
		insert into architecture_snapshots (project_key, commit, files) values ('CORE', 'head', '{}') returning id::text
	`)
	execFixture(t, tx, "seed component", `
		insert into components (project_key, id, title, prose, snapshot_id, source_id)
		values ('CORE', 'platform', 'Platform', '', $1::bigint, $2::bigint)
	`, snapshot, source)
	var tuples []graphstore.Tuple
	for _, key := range []string{parent, blocker, filing, mentioned, duplicate, a, b, d} {
		execFixture(t, tx, "attach "+key, `insert into issue_components (issue_key, mode) values ($1, 'explicit')`, key)
		execFixture(t, tx, "attach "+key, `insert into issue_component_members (issue_key, project_key, component_id) values ($1, 'CORE', 'platform')`, key)
		tuples = append(tuples, graphstore.ComponentRelation(graphstore.KindIssue, key, graphstore.TupleAffects, component, true, actor, at))
	}
	for _, key := range []string{a, b, d} {
		tuples = append(tuples, graphstore.IssueChildOf(key, parent, actor, at))
	}
	for _, key := range []string{h, r, d} {
		tuples = append(tuples, graphstore.Tuple{FromKind: "issue", FromID: key, Kind: "filed_from", ToKind: "ask", ToID: ask, At: at, Who: actor, Asserted: true})
	}
	tuples = append(tuples,
		graphstore.IssueChildOf(y, mentioned, actor, at),
		graphstore.IssueNeeds(a, blocker, true, actor, at),
		graphstore.IssueNeeds(d, blocker, true, actor, at),
		graphstore.Tuple{FromKind: "issue", FromID: d, Kind: "duplicate_of", ToKind: "issue", ToID: duplicate, At: at, Who: actor, Asserted: true},
		graphstore.ArtifactMentions(dSpec, "issue", mentioned, true, actor, at),
		graphstore.ArtifactMentions(eSpec, "ask", ask, true, actor, at),
		graphstore.Tuple{FromKind: "block", FromID: cBlock, Kind: "mentions", ToKind: "ask", ToID: ask, At: at, Who: actor, Asserted: true},
		// A block and its document may both cite the same target. The count and the list keep it once.
		graphstore.ArtifactMentions(cSpec, "ask", ask, true, actor, at),
	)
	if err := graphstore.Write(ctx, tx, nil, tuples); err != nil {
		t.Fatalf("state the neighbourhood's relations: %v", err)
	}
	kRef := "dispatch://CORE/component/platform"
	pRef := "dispatch://" + parent
	mRef := "dispatch://" + filing + "/ask/" + ask
	want := []model.IssueNeighbour{
		{Key: a, Status: "triage", Title: "Amaranth", Shares: []string{kRef, pRef, "dispatch://" + blocker}},
		{Key: y, Status: "triage", Title: "Yew", Shares: []string{"dispatch://" + mentioned}},
		{Key: c, Status: "triage", Title: "Cicada", Shares: []string{mRef}},
		{Key: h, Status: "triage", Title: "Hyacinth", Shares: []string{mRef}},
		{Key: b, Status: "done", Title: "Basalt", Shares: []string{kRef, pRef}},
		{Key: r, Status: "done", Title: "Rhodium", Shares: []string{mRef}},
	}
	for index := range want {
		want[index].Href = "/issues/" + want[index].Key
	}
	for _, limit := range []int{5, 10} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			got, err := IssueNeighbours(ctx, tx, actor, d, limit)
			if err != nil {
				t.Fatal(err)
			}
			expected := want[:min(limit, len(want))]
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("neighbours of %s (limit %d):\n got %+v\nwant %+v", d, limit, got, expected)
			}
		})
	}
}
