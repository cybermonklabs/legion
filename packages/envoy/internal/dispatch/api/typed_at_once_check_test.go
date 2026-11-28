package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// c-typed-at-once's own helpers.

// everyTreeView is every read of the tree c-typed-at-once reads a removed block out of: the read
// with no view, and each view the tree read answers (issueTreeViews).
func everyTreeView() []string {
	queries := []string{""}
	for _, view := range issueTreeViews {
		queries = append(queries, "?view="+view)
	}
	return queries
}

// disagreeingBlocks is a check, a requirement and a plan task numbered n, the check and the plan
// task serving the spec's r1: what one side of a write whose tree and markdown disagree holds, so
// the test reads no node of any of the three types it writes.
func disagreeingBlocks(n string) []acceptedBlock {
	return []acceptedBlock{acceptedCheck("c1", "r1"), acceptedCheck("c"+n, "r1"), acceptedRequirement("r" + n), acceptedPlanTask("t"+n, "r1")}
}

// requireCoverageHoldsTheChain fails t unless the coverage read of issueKey (GET
// /api/v1/issues/{key}/coverage) lists each requirement of chain by its block id and lists each
// check of chain by its node among the checks that prove each requirement it serves: the read
// reaches the requirement two levels under a criterion in a list, and places the check under a
// heading's list under the requirement it serves.
func requireCoverageHoldsTheChain(t *testing.T, handler http.Handler, issueKey string, chain []writtenBlock) {
	t.Helper()
	coverage := readCoverage(t, handler, issueKey)
	listed := func(id string) (model.RequirementCoverage, bool) {
		at := slices.IndexFunc(coverage.Requirements, func(requirement model.RequirementCoverage) bool { return requirement.BlockID == id })
		if at < 0 {
			return model.RequirementCoverage{}, false
		}
		return coverage.Requirements[at], true
	}
	for _, block := range chain {
		switch block.typ {
		case "requirement":
			if _, held := listed(block.id); !held {
				t.Errorf("right after the write the coverage read of %s does not list the requirement %s: %+v", issueKey, block.id, coverage.Requirements)
			}
		case "check":
			for _, served := range block.serves {
				requirement, held := listed(served)
				if !held || !slices.ContainsFunc(requirement.Proof, func(proof model.RequirementProof) bool { return proof.Check == block.address() }) {
					t.Errorf("right after the write the coverage read of %s does not list the check %s under the requirement %s it serves (listed %t): its checks are %+v",
						issueKey, block.address(), served, held, requirement.Proof)
				}
			}
		}
	}
}
