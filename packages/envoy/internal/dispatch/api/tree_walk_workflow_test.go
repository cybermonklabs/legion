package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphseedtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// TestTheTreeWalkLeavesOutTheWorkflowsRecords is c-tree-read-views' proof that the walk leaves out
// the records of the issue's workflow (R8): a run of a step, the plan a run ran from and the
// handoff a run reported. An issue born converted holds one requirement. In one transaction the
// test seeds the types of the three kinds, which no migration of the store holds
// (graphseedtest.SeedSupertags), writes one node of each kind and a lens visible to the project
// through graphstore.PlanUncaused and its node write (graphstore.QueueNodes), with a part_of from
// each of the three to the issue's node and from the lens to the step, and appends those tuples
// under the event that created the issue (graphstore.Caused), as the store holds the records once
// the workflow store moves into it. It reads, in graph_tuples, the standing part_of from each of
// the three to the issue's node (graphstore.CurrentOut), so the
// case proves the walk's rule and not a write that did not land. Neither the walk of the issue's
// tree nor the tree read with no view answers the step, the plan, the handoff or the lens under
// the step, or a tuple that names one, and both answer the requirement.
func TestTheTreeWalkLeavesOutTheWorkflowsRecords(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "WALK")
	issue := createDigestIssue(t, handler, database, "WALK", "The walked issue", ":::requirement{#r1}\nThe issue states it.\n:::\n", false)
	var event int64
	if err := database.Pool.QueryRow(ctx, `select id from events where issue_key = $1 and type = 'issue.created'`, issue.Key).Scan(&event); err != nil {
		t.Fatalf("read the event that created %s: %v", issue.Key, err)
	}

	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	node := func(kind string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: uuid.NewString(), Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			Project: "WALK", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}
	}
	partOf := func(child graphstore.Node, kind, id string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: child.Kind, FromID: child.ID, Kind: graphstore.TuplePartOf, ToKind: kind, ToID: id,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	kinds := []string{workflowstore.KindStep, workflowstore.KindPlan, workflowstore.KindHandoff}
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	for _, kind := range kinds {
		record := node(kind)
		nodes = append(nodes, record)
		tuples = append(tuples, partOf(record, graphstore.KindIssue, issue.Key))
	}
	lens := node(graphstore.KindLens)
	nodes = append(nodes, lens)
	tuples = append(tuples, partOf(lens, nodes[0].Kind, nodes[0].ID))
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphseedtest.SeedSupertags(ctx, tx, kinds...); err != nil {
			return err
		}
		write := graphstore.PlanUncaused(nodes, tuples)
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueNodes(b, write)
			return nil
		}); err != nil {
			return err
		}
		return graphstore.Caused(ctx, tx, write, event)
	}); err != nil {
		t.Fatalf("write the records of the workflow of %s: %v", issue.Key, err)
	}
	left := map[refs.Target]string{}
	for _, record := range nodes {
		left[refs.Target{Kind: record.Kind, ID: record.ID}] = record.Kind
	}

	for _, record := range nodes[:len(kinds)] {
		if !slices.Contains(farEnds(t, database, record.Kind, record.ID, graphstore.TuplePartOf), graphstore.KindIssue+"/"+issue.Key) {
			t.Fatalf("the store holds no part_of from the %s node to %s", record.Kind, issue.Key)
		}
	}

	tree, err := docs.WalkIssueTree(ctx, database.Pool, issue.Key, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", issue.Key, err)
	}
	requirement := false
	for _, walked := range tree.Nodes {
		if kind, held := left[refs.Target{Kind: walked.Kind, ID: walked.ID}]; held {
			t.Errorf("the walk of %s answers the %s node %s", issue.Key, kind, walked.ID)
		}
		requirement = requirement || walked.Type == requirementBlockType
	}
	if !requirement {
		t.Errorf("the walk of %s answers no requirement", issue.Key)
	}

	read := treeRead(t, handler, issue.Key, "", "alice", false)
	for _, answered := range read.Nodes {
		if kind, held := left[refs.Target{Kind: answered.Kind, ID: answered.ID}]; held {
			t.Errorf("the tree read of %s answers the %s node %s", issue.Key, kind, answered.ID)
		}
	}
	for _, tuple := range read.Tuples {
		for _, end := range []model.IssueTreeEnd{tuple.From, tuple.To} {
			if kind, held := left[refs.Target{Kind: end.Kind, ID: end.ID}]; held {
				t.Errorf("the tree read of %s answers the %s node %s in a %s tuple", issue.Key, kind, end.ID, tuple.Kind)
			}
		}
	}
	if len(read.supertagged(requirementBlockType)) == 0 {
		t.Errorf("the tree read of %s answers no requirement", issue.Key)
	}
}
