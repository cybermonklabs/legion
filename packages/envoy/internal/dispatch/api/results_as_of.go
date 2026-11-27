package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
// version at the cut of artifact, the Verification document, parsed as the present read parses
// the live text (checkBlocksOfRendering), with its issue and its reference. The version in which
// the words of a check last changed, and their digest, come from what the block index recorded of
// that version (versionAtTheCut). The text of a check's tree is the text that the index
// recorded, as the present read takes it from the live text. Where the index recorded nothing of
// a check, the answer leaves out the version and the digest (reason E), and checksAtTheCut leaves
// out whether the words of each of its results moved. One batch sends the statements, and
// nothing is read from the live document.
func (s *server) checkBlocksAtTheCut(ctx context.Context, artifact model.Artifact, trees *displayTreeReader) ([]model.CheckBlock, error) {
	markdown, _, recorded, err := s.versionAtTheCut(ctx, artifact.ID, true)
	if err != nil {
		return nil, fmt.Errorf("read the checks of %s at the cut: %w", artifact.ID, err)
	}
	indexed := make(map[string]model.CheckBlock, len(recorded.texts))
	for id, text := range recorded.texts {
		indexed[id] = model.CheckBlock{BlockID: id, Version: recorded.versions[id], SHA256: refs.BlockTextSum(text)}
	}
	checks, err := checkBlocksOfRendering(artifact.ID, markdown, recorded.texts, trees)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	addressChecks(checks, artifact, indexed)
	at := elementsOf("/checks")
	for index := range checks {
		if _, held := indexed[checks[index].BlockID]; !held {
			place := at(index)
			leaveOut(ctx, place+"/version", place+"/sha256")
		}
	}
	return checks, nil
}

// checksAtTheCut completes checks, the checks of a read of the results at the cut ctx carries,
// each at /checks/<index> of the answer with the results that the read loaded. A read at a cut
// makes no GitHub read, so the answer leaves out what the present read takes from GitHub (reason
// H): of each check, what it compared its results with, why it compared them with nothing, and
// whether a pass of it stands; of a check that CI proves, what CI's run said (ci), and the result
// that the present read derives from that run, which the read at a cut does not make; and of each
// result, where its commit stands and the currency that older clients print. Of each result of a
// check that the present read compares with the program's current deploy (readsTheLiveDeploy), it
// leaves out not_counted, which the present read writes from that deploy now (markNotCounted).
// Whether the words of a check moved after a result is given where the check's digest at the cut
// is (checkBlocksAtTheCut), and is left out with the digest elsewhere (reason E).
func checksAtTheCut(ctx context.Context, checks []model.CheckBlock) {
	at := elementsOf("/checks")
	for index := range checks {
		check := &checks[index]
		place := at(index)
		leaveOut(ctx, place+"/compared_with", place+"/compared_nothing", place+"/pass_stands")
		if validCICheck(*check, len(check.Results) > 0) {
			leaveOut(ctx, place+"/ci")
		}
		resultAt := elementsOf(place + "/results")
		for number := range check.Results {
			result := resultAt(number)
			leaveOut(ctx, result+"/commit_stands", result+"/currency")
			if readsTheLiveDeploy(*check) {
				leaveOut(ctx, result+"/not_counted")
			}
			if check.SHA256 == "" {
				leaveOut(ctx, result+"/check_words_moved")
				continue
			}
			moved := check.SHA256 != check.Results[number].CheckSHA256
			check.Results[number].CheckWordsMoved = &moved
		}
	}
}
