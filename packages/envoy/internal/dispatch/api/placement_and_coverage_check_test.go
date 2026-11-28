package api

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The helpers of c-placement-and-coverage's reads of the coverage answer's entries, of a result
// recorded on the one check that serves three requirements, and of the parent's verification
// record, which TestPlacementAndCoverageAreTwoRelations makes.

// placementProofIDs is, for each requirement of the coverage read of issue, the ids of the checks
// its proof lists: the entries the coverage answer carries under each requirement.
func placementProofIDs(t *testing.T, handler http.Handler, issue string) map[string][]string {
	t.Helper()
	ids := map[string][]string{}
	for _, requirement := range readCoverage(t, handler, issue).Requirements {
		ids[requirement.BlockID] = []string{}
		for _, proof := range requirement.Proof {
			ids[requirement.BlockID] = append(ids[requirement.BlockID], proof.Check)
		}
	}
	return ids
}

// placementNewEntries is the entries the coverage answer of issue lists under requirement that
// before did not list under it: those of the check a test wrote since before was read.
func placementNewEntries(coverage model.IssueCoverage, before map[string][]string, requirement string) []model.RequirementProof {
	var entries []model.RequirementProof
	for _, held := range coverage.Requirements {
		if held.BlockID != requirement {
			continue
		}
		for _, proof := range held.Proof {
			if !slices.Contains(before[requirement], proof.Check) {
				entries = append(entries, proof)
			}
		}
	}
	return entries
}

// requireOneCheckEntry fails unless the coverage read of CORE-1 lists, under each requirement of
// served, exactly one entry that before did not list, all of them carrying one id, address, the
// check's own node, and not one id per requirement: the check is one node, never a copy per
// requirement it serves. Where proves is not "", each entry also reads that one result.
func requireOneCheckEntry(t *testing.T, handler http.Handler, what, address, proves string, before map[string][]string, served ...string) {
	t.Helper()
	coverage := readCoverage(t, handler, "CORE-1")
	var ids []string
	for _, requirement := range served {
		entries := placementNewEntries(coverage, before, requirement)
		if len(entries) != 1 {
			t.Errorf("%s: the coverage answer lists under %s the new entries %+v, want exactly one, of the check %s", what, requirement, entries, address)
			continue
		}
		if !slices.Contains(ids, entries[0].Check) {
			ids = append(ids, entries[0].Check)
		}
		if proves != "" && entries[0].Proves != proves {
			t.Errorf("%s: the entry under %s reads %q, want %q: the one result recorded on the check reads under each of its entries", what, requirement, entries[0].Proves, proves)
		}
	}
	if len(ids) != 1 || ids[0] != address {
		t.Errorf("%s: the %d entries under %v carry the ids %v, want one id, %s, and not one per requirement: the check is one node, not a copy", what, len(served), served, ids, address)
	}
}

// placementResultsStored is how many results the store holds of the check blockID of issue: one
// for a check that is one node, whatever number of requirements it serves.
func placementResultsStored(t *testing.T, database *store.Store, issue, blockID string) int {
	t.Helper()
	return int(countRows(t, database, `select count(*) from check_results where check_issue_key = $1 and check_block_id = $2`, issue, blockID))
}

// recordOnePlacementPass records one pass of the check blockID on CORE-1, and fails unless it is
// taken.
func recordOnePlacementPass(t *testing.T, handler http.Handler, what, blockID string) {
	t.Helper()
	if response := recordResult(t, handler, "CORE-1", resultBody(blockID, "pass", headCommit, nil)); response.Code != http.StatusCreated {
		t.Fatalf("record one pass of %s: status=%d body=%s", what, response.Code, response.Body.String())
	}
}

// requireParentRecordHoldsNoChildNode fails unless the record of the verification query of the
// issue parent holds the node own, its own check, and none of addresses, the nodes of its child
// child: the parent's verification record holds no node of the child.
func requireParentRecordHoldsNoChildNode(t *testing.T, database *store.Store, parent, child, own string, addresses ...string) {
	t.Helper()
	record, err := docs.ReadQueryRecord(context.Background(), database.Pool, parent, graphstore.QueryVerification, refs.TreeCut{})
	if err != nil {
		t.Fatalf("read the verification record of %s: %v", parent, err)
	}
	held := make([]string, len(record.Nodes))
	for index, node := range record.Nodes {
		held[index] = node.Address()
	}
	if !slices.Contains(held, refs.BlockNodeKind+":"+own) {
		t.Errorf("the verification record of %s holds the nodes %v, want its own check %s among them", parent, held, own)
	}
	for _, address := range addresses {
		if slices.Contains(held, refs.BlockNodeKind+":"+address) || bytes.Contains(record.Record, []byte(address)) {
			t.Errorf("the verification record of %s holds the node %s of its child %s (its nodes %v), want no node of the child", parent, address, child, held)
		}
	}
	for _, address := range held {
		if strings.Contains(address, ":"+child+"/") {
			t.Errorf("the verification record of %s holds the node %s of its child %s, want no node of the child", parent, address, child)
		}
	}
}

// requireParentListsChildEntry fails unless the coverage read of parent lists, under requirement,
// one more check and one more plan task than base did, the check's entry carrying its id, address,
// marked with child, its issue; and no more of either under any other requirement.
func requireParentListsChildEntry(t *testing.T, handler http.Handler, parent, child, requirement, address string, base model.IssueCoverage) {
	t.Helper()
	coverage := readCoverage(t, handler, parent)
	for _, before := range base.Requirements {
		after := requirementOf(t, coverage, before.BlockID)
		wantChecks, wantPlanTasks := before.Checks, before.PlanTasks
		if before.BlockID == requirement {
			wantChecks, wantPlanTasks = wantChecks+1, wantPlanTasks+1
		}
		if after.Checks != wantChecks || after.PlanTasks != wantPlanTasks {
			t.Errorf("the coverage answer of %s lists %d check(s) and %d plan task(s) under %s, want %d and %d: the check and the plan node of %s serve %s alone",
				parent, after.Checks, after.PlanTasks, before.BlockID, wantChecks, wantPlanTasks, child, requirement)
		}
	}
	entries := requirementOf(t, coverage, requirement).Proof
	if !slices.ContainsFunc(entries, func(proof model.RequirementProof) bool { return proof.Check == address && proof.Issue == child }) {
		t.Errorf("the coverage answer of %s lists under %s the entries %+v, want the check %s through serves, marked with %s", parent, requirement, entries, address, child)
	}
}
