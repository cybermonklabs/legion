package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// owedRequirement is a spec marking one requirement, r-owed, that no check serves.
const owedRequirement = ":::requirement{#r-owed}\nThe page draws no gap from an index that is owed.\n:::\n"

// owedPlan is a plan document holding one plan task, t-owed, that serves no requirement: with it
// the issue has a plan, so a requirement no plan task serves lacks one (coverRequirements).
const owedPlan = ":::plan_task{#t-owed}\nA task that serves no requirement.\n:::\n"

// coverageSurface is the gap lines one reader of an issue's coverage answers, by its name.
type coverageSurface struct {
	name  string
	lines []string
}

// approvalCoverage names the reader of the coverage an approval request of the spec answers, the
// one reader that never writes a requirement's missing plan task, since the plan comes after the
// approval (missingCheckGaps).
const approvalCoverage = "the coverage of POST /api/v1/artifacts/{id}/approval-requests"

// coverageSurfaces reads the coverage of the issue key, whose spec is specID, through each of its
// three readers: the coverage read's gaps, the coverage an approval request of the spec answers,
// and the `Coverage: ` lines `dispatch read --issue` prints, run against url. It answers the
// coverage read too, whose requirements carry their own gaps.
func coverageSurfaces(t *testing.T, handler http.Handler, url, key, specID string) (model.IssueCoverage, []coverageSurface) {
	t.Helper()
	coverage := readCoverage(t, handler, key)
	approval := requestSpecApproval(t, handler, specID)
	read := runDispatchCLI(t, url, "read", "--issue", key)
	if read.code != 0 {
		t.Fatalf("dispatch read --issue %s: %s", key, read)
	}
	var printed []string
	for line := range strings.SplitSeq(read.stdout, "\n") {
		if gap, ok := strings.CutPrefix(line, "Coverage: "); ok {
			printed = append(printed, gap)
		}
	}
	return coverage, []coverageSurface{
		{name: "GET /api/v1/issues/{key}/coverage", lines: coverage.Gaps},
		{name: approvalCoverage, lines: approval.Coverage},
		{name: "dispatch read --issue", lines: printed},
	}
}

// TestAnOwedDocumentDrawsNoCheckGap: the issue holds a plan whose one plan task serves no
// requirement, so its requirement lacks both a check and a plan task. While the spec owes its
// block index - a version written through the upload route past the bound (docs'
// deferBlockIndexBlocks), which leaves its blocks to the block-index reconciliation - the coverage
// read, the approval request's answer and `dispatch read --issue` each answer the line that names
// the spec and that version as still being indexed, and none names a requirement as lacking a
// check or a plan task, nor does the requirement's own gaps. Once the reconciliation has indexed
// the spec, each answers the requirement's missing check again, the coverage read and `dispatch
// read --issue` its missing plan task too, the approval request's answer no missing plan task,
// and the owed line is gone (spec requirement r-owed-coverage).
func TestAnOwedDocumentDrawsNoCheckGap(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "OWED")
	issue := createSpecIssue(t, handler, "OWED", owedRequirement)
	key, spec := issue.Key, issue.PrimaryArtifactID
	uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", owedPlan)
	reconcileUntilNothingOwed(t, handler, deps, key)
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)

	written := uploadDocument(t, handler, "/api/v1/issues/"+key, "spec.md",
		owedRequirement+"\n"+deferralList(itemsOverTheBlockBound, "end"))
	if written.Artifact.ID != spec {
		t.Fatalf("the upload of spec.md to %s wrote the document %s, want its spec %s", key, written.Artifact.ID, spec)
	}
	version := written.Version.Number
	if owed := readCoverage(t, handler, key).OwedDocuments; !slices.Contains(owed, model.OwedDocument{ArtifactID: spec, Version: version}) {
		t.Fatalf("owed_documents of %s does not name the spec at version %d: %+v", key, version, owed)
	}

	const document, block = "spec.md", "r-owed"
	owedLine := fmt.Sprintf("%s version %d is still being indexed; its coverage shows once that ends.", document, version)
	coverage, surfaces := coverageSurfaces(t, handler, served.URL, key, spec)
	for _, surface := range surfaces {
		if !slices.Contains(surface.lines, owedLine) {
			t.Errorf("%s of %s answers no line for %s at version %d: it answers %q, want %q among them",
				surface.name, key, document, version, surface.lines, owedLine)
		}
		for _, line := range surface.lines {
			if strings.HasSuffix(line, noCheckReason) || strings.HasSuffix(line, noPlanTaskReason) {
				t.Errorf("%s of %s answers %s while %s owes its block index at version %d", surface.name, key, line, document, version)
			}
		}
	}
	for _, reason := range []string{noCheckReason, noPlanTaskReason} {
		if own := requirementOf(t, coverage, block).Gaps; slices.Contains(own, reason) {
			t.Errorf("the gaps of requirement %s in GET /api/v1/issues/{key}/coverage of %s answers %s while %s owes its block index at version %d",
				block, key, reason, document, version)
		}
	}
	t.Logf("while %s owes its block index at version %d, the three readers answer %q", document, version, surfaces)

	reconcileUntilNothingOwed(t, handler, deps, key)
	requireNothingOwed(t, handler, key, "once the index caught up")
	missing, missingTask := noCheckGap(block), block+" "+noPlanTaskReason
	_, surfaces = coverageSurfaces(t, handler, served.URL, key, spec)
	for _, surface := range surfaces {
		if !slices.Contains(surface.lines, missing) {
			t.Errorf("%s of %s answers no has no check line for %s once the index caught up: it answers %q", surface.name, key, block, surface.lines)
		}
		planTasks := surface.name != approvalCoverage
		if planTasks && !slices.Contains(surface.lines, missingTask) {
			t.Errorf("%s of %s answers no has no plan task line for %s once the index caught up: it answers %q", surface.name, key, block, surface.lines)
		}
		if !planTasks && slices.Contains(surface.lines, missingTask) {
			t.Errorf("%s of %s answers %q once the index caught up, want no missing plan task: the plan comes after the approval", surface.name, key, missingTask)
		}
		if slices.Contains(surface.lines, owedLine) {
			t.Errorf("%s of %s still answers %q once the index caught up", surface.name, key, owedLine)
		}
	}
	t.Logf("once the index caught up, the three readers answer %q", surfaces)
}
