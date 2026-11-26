package graphcensus

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// passingStoreReport is a store census that passes: every kind the census holds the store to
// (projections) checked a row and found no difference in either direction, and every
// project-owned kind (projectKinds) holds a row whose node is complete. No sizing was asked for.
func passingStoreReport() StoreReport {
	report := StoreReport{}
	for _, held := range projections {
		report.Kinds = append(report.Kinds, KindReport{Kind: held.name, NodeKind: held.nodeKind, Table: held.table, Rows: 1, Nodes: 1})
	}
	for _, kind := range projectKinds {
		report.Projects = append(report.Projects, ProjectKindReport{Kind: kind, Rows: 1})
	}
	return report
}

// One project finding refuses the store census on its own. A report with every store kind checked,
// no difference and no project finding passes; the same report with exactly one project finding
// added, of each class in turn (missing, gone, unscoped, differs, orphaned), each on a
// project-owned kind that class can be found on - gone and orphaned only on a kind whose rows only
// its own creates write (objectKinds) - refuses, while it still reads every store kind checked
// and no difference.
//
// Its control is the project findings dropped from Refused: each of the five reports then passes.
func TestAProjectFindingAloneRefusesTheStoreCensus(t *testing.T) {
	passing := passingStoreReport()
	if len(passing.Kinds) == 0 || len(passing.Projects) == 0 {
		t.Fatalf("the passing report holds %d store kinds and %d project kinds, so the test would hold nothing", len(passing.Kinds), len(passing.Projects))
	}
	if passing.Refused() || passing.ProjectFindings() != 0 {
		t.Fatalf("the report with every kind checked and nothing found refuses %t with %d project findings, want it to pass with none",
			passing.Refused(), passing.ProjectFindings())
	}
	for _, class := range []struct {
		name, kind string
		add        func(*ProjectKindReport)
	}{
		{classMissing, refs.BlockNodeKind, func(kind *ProjectKindReport) { kind.Missing = 1 }},
		{classGone, graphstore.KindArtifact, func(kind *ProjectKindReport) { kind.Gone = 1 }},
		{classUnscoped, graphstore.KindComment, func(kind *ProjectKindReport) { kind.Unscoped = 1 }},
		{classDiffers, graphstore.KindMessage, func(kind *ProjectKindReport) { kind.Differs = 1 }},
		{classOrphaned, graphstore.KindIssue, func(kind *ProjectKindReport) { kind.Orphaned = 1 }},
	} {
		t.Run(class.name, func(t *testing.T) {
			report := passingStoreReport()
			at := slices.IndexFunc(report.Projects, func(kind ProjectKindReport) bool { return kind.Kind == class.kind })
			if at < 0 {
				t.Fatalf("the passing report holds no %s project kind: %+v", class.kind, report.Projects)
			}
			class.add(&report.Projects[at])
			if report.ProjectFindings() != 1 || report.Differences() != 0 || len(report.Unpopulated()) != 0 {
				t.Fatalf("the report with one %s finding on %s reads %d project findings, %d differences and unpopulated kinds %v; want 1, 0 and none",
					class.name, class.kind, report.ProjectFindings(), report.Differences(), report.Unpopulated())
			}
			if !report.Refused() {
				t.Errorf("the store census with one %s finding on %s and nothing else found passes, want it refused", class.name, class.kind)
			}
		})
	}
}
