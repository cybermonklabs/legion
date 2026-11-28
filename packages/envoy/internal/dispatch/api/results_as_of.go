package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The results of an issue's checks at a cut (decision 11). The read runs its present statements
// under the cut, which the store binds to the shadows: the Verification document, the results
// and the retractions are the rows at the cut. The checks come from the document's newest version
// at the cut, which the read parses as the present read parses the live text
// (checkBlocksAtTheCut). Where a result's commit stands, what a check compared its results with
// and what CI's run said are GitHub reads now. So the read at a cut makes no read of GitHub and
// reads no pull request head and no branch commit. It also writes none of what the present read
// writes (pull_request_heads, branch_commits and the event issue.pull_request_attribution_changed).
// It leaves those fields out (checksAtTheCut).

// refuseResultsCommitAtCut refuses a read of the results at a cut that names a commit (?at=,
// refuseParameterAtCut): the read compares a result with the commit through GitHub now, and a
// read at a cut makes no GitHub read. It reports whether it answered.
func refuseResultsCommitAtCut(w http.ResponseWriter, r *http.Request) bool {
	return refuseParameterAtCut(w, r, "at",
		"a read of the results at a past time compares no result with a commit, because where a commit stands is a GitHub read now")
}

// checkBlocksAtTheCut is checkBlocksOf at the cut ctx carries: every check block of the newest
// version at the cut of artifact, the Verification document, parsed out of that version
// (checkBlocksOfRendering), with its issue and its reference. The present read takes each check
// from the block index (results_settled_check.go), which keeps no history: the block index of a
// version can be written after the version is saved (a write defers it, docs'
// IndexDocumentReferences), and the reconciliation re-indexes a version in place. No record gives
// the time of that write, so no record gives what the index held at the cut. So the read at a cut
// parses the version it holds, and the text of a check's tree is the text that the index recorded
// of that version. It reads and fills in no check's version or digest (reason E), and the answer
// leaves both out (checksAtTheCut). One batch reads the version and what the index recorded of
// its blocks, and nothing is read from the live document.
func (s *server) checkBlocksAtTheCut(ctx context.Context, artifact model.Artifact, trees *displayTreeReader) ([]model.CheckBlock, error) {
	markdown, _, recorded, err := s.versionAtTheCut(ctx, artifact.ID, true)
	if err != nil {
		return nil, fmt.Errorf("read the checks of %s at the cut: %w", artifact.ID, err)
	}
	checks, err := checkBlocksOfRendering(artifact.ID, markdown, recorded, trees)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	addressChecks(checks, artifact)
	return checks, nil
}

// checkBlocksOfRendering is every `check` block of markdown, a rendering of document artifactID,
// in document order, with its attributes and the references its `serves` attribute names
// (docs.CheckBlockOf), and its words (visitTypedBlocks), texts being the text the block index
// recorded for each block, by id. trees is the request's own reader, nil for a caller that asked
// for no tree: each check registers with it, as its text_tree, the markdown visitTypedBlocks reads
// the block's tree from, once the slice has stopped growing, since what is registered is a pointer
// into the row.
func checkBlocksOfRendering(artifactID, markdown string, texts map[string]string, trees *displayTreeReader) ([]model.CheckBlock, error) {
	var checks []model.CheckBlock
	var contents []string
	err := visitTypedBlocks(artifactID, "check", markdown, texts, trees != nil, func(node *pmdoc.Node, _, content string) {
		check := docs.CheckBlockOf(node)
		check.ArtifactID, check.Text, check.Results = artifactID, blockWords(node), []model.CheckResult{}
		checks = append(checks, check)
		if trees != nil {
			contents = append(contents, content)
		}
	})
	if err != nil {
		return nil, err
	}
	for index, content := range contents {
		trees.text(&checks[index].TextTree, content)
	}
	return checks, nil
}

// checksAtTheCut completes checks, the checks of a read of the results at the cut ctx carries,
// each at /checks/<index> of the answer with the results that the read loaded, a CI job's among
// them (ci_check_results at the cut). A read at a cut makes no GitHub read, so the answer leaves
// out what the present read takes from GitHub (reason H): of each check, what it compared its
// results with, why it compared them with nothing, and whether a pass of it stands; of a check
// that CI proves, what CI's run said (ci), and the result that the present read derives from that
// run, which the read at a cut does not make; and of each result, where its commit stands and the
// currency that older clients print. The branch heads of a check (branch_heads) and the head a
// result's commit is (at_branch_head) are parts of compared_with and of commit_stands that the
// present read moves out for an older command (keepFieldsOlderCommandsRead); the read at a cut
// makes neither, and a reader of this build reads them back into the two members the answer names
// absent. Of each result of a check that the present read compares with the program's current
// deploy (readsTheLiveDeploy), it leaves out not_counted, which the present read writes from that
// deploy now (markNotCounted). Of a check that CI proves, a result no run of its job recorded
// still counts for nothing and says why (notCountedOnCICheck): that is a fact of the rows and of
// the check's words at the cut, not of GitHub. It also leaves out, for reason E, each check's
// version and digest, which no record gives at the cut (checkBlocksAtTheCut), and whether the
// words of the check moved after each result, which compares them.
func checksAtTheCut(ctx context.Context, checks []model.CheckBlock) {
	at := elementsOf("/checks")
	for index := range checks {
		check := &checks[index]
		place := at(index)
		leaveOut(ctx, place+"/version", place+"/sha256")
		leaveOut(ctx, place+"/compared_with", place+"/compared_nothing", place+"/pass_stands")
		if validCICheck(*check) {
			notCountedOnCICheck(check, docs.CheckAttributesOfBlock(*check).CICheck)
			leaveOut(ctx, place+"/ci")
		}
		resultAt := elementsOf(place + "/results")
		live := readsTheLiveDeploy(*check)
		for number := range check.Results {
			result := resultAt(number)
			leaveOut(ctx, result+"/commit_stands", result+"/currency", result+"/check_words_moved")
			if live {
				leaveOut(ctx, result+"/not_counted")
			}
		}
	}
}
