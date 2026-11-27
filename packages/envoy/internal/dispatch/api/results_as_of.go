package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
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
// the live text (checkBlocksOfRendering), with its issue, its reference, its last changed
// version from the index, and its digest from the saved spelling at the cut
// (checkDigestsAtTheCut). The text of a check's tree is the text that the index recorded, as the
// present read takes it from the live text. Where the index recorded nothing of a check, the
// answer leaves out the version and the digest (reason E), and checksAtTheCut leaves out whether
// the words of each of its results moved. Where the block index holds now, for that same version,
// another digest of a check, a re-index in place changed it at a time no record gives
// (clearReindexedDigests): the answer leaves out the digest and keeps the version (reason E), and
// checksAtTheCut leaves out whether the words of each of its results moved. One batch reads the
// version and its record, one statement compares the digests, and nothing is read from the live
// document.
func (s *server) checkBlocksAtTheCut(ctx context.Context, artifact model.Artifact, trees *displayTreeReader) ([]model.CheckBlock, error) {
	markdown, _, recorded, err := s.versionAtTheCut(ctx, artifact.ID, true)
	if err != nil {
		return nil, fmt.Errorf("read the checks of %s at the cut: %w", artifact.ID, err)
	}
	checks, err := checkBlocksOfRendering(artifact.ID, markdown, recorded.texts, trees)
	if err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return []model.CheckBlock{}, nil
	}
	indexed, err := checkDigestsAtTheCut(markdown, checks, recorded)
	if err != nil {
		return nil, err
	}
	if err := s.clearReindexedDigests(ctx, artifact.ID, recorded.number, indexed); err != nil {
		return nil, fmt.Errorf("compare the digests of the checks of %s at the cut with the block index: %w", artifact.ID, err)
	}
	addressChecks(checks, artifact, indexed)
	at := elementsOf("/checks")
	for index := range checks {
		place := at(index)
		switch check, held := indexed[checks[index].BlockID]; {
		case !held:
			leaveOut(ctx, place+"/version", place+"/sha256")
		case check.SHA256 == "":
			leaveOut(ctx, place+"/sha256")
		}
	}
	return checks, nil
}

// checkDigestsAtTheCut is, by block id, what the block index recorded at the cut of each of checks
// that it recorded (recorded, its record of markdown, the stored text of the newest version at
// the cut): the version in which the check's words last changed, and the digest that the index of
// that version wrote for the check, computed from the stored version. That digest is a candidate
// only. The present read gives the digest that document_blocks.text_sha256 holds, and an index
// after the version's own can move it with no new text: the index of a later version, which a
// read at the cut does not see, or a re-index of this version in place, which no record dates and
// which clearReindexedDigests compares for. The index of a version sums each block's slice of the
// rendering it indexed, the first block of each id (refs.PlanBlocks), and records that slice only
// where the version changed the block's text: a version that writes in this build's spelling a
// block whose recorded text another renderer version spelled keeps the block's changed version
// and records no text, and the sum moves to the new spelling (refs.BlockIndexing.KeepRespelled).
// So where this build renders markdown as it is stored (docs.VersionRenderingBlocks), as it
// renders every version its renderer wrote, the digest is the sum of the check's slice of that
// rendering (refs.BlockTextSum). Where it renders markdown otherwise, another renderer version
// wrote it in a spelling whose blocks this build cannot cut, and the digest is the sum of the
// text recorded, which is that version's own text of each check it changed. markdown is rendered
// only where the index holds a check.
func checkDigestsAtTheCut(markdown string, checks []model.CheckBlock, recorded blockIndex) (map[string]model.CheckBlock, error) {
	indexed := make(map[string]model.CheckBlock, len(checks))
	for _, check := range checks {
		if _, held := recorded.texts[check.BlockID]; held {
			indexed[check.BlockID] = model.CheckBlock{BlockID: check.BlockID, Version: recorded.versions[check.BlockID]}
		}
	}
	if len(indexed) == 0 {
		return indexed, nil
	}
	rendered, blocks, err := docs.VersionRenderingBlocks(markdown)
	if err != nil {
		return nil, err
	}
	if rendered != markdown {
		for id, check := range indexed {
			check.SHA256 = refs.BlockTextSum(recorded.texts[id])
			indexed[id] = check
		}
		return indexed, nil
	}
	for _, block := range blocks {
		// Only the first block of an id is summed: no sum is empty, so a later one is passed over.
		if check, held := indexed[block.ID]; held && check.SHA256 == "" {
			check.SHA256 = refs.BlockTextSum(rendered[block.From:block.To])
			indexed[block.ID] = check
		}
	}
	return indexed, nil
}

// checkDigestsIndexedNowStatement reads the present digests for the named checks of document $1,
// only when the index holds version $2, the newest version at the cut. Both tables are read as
// they are now (asof.CutReadTables).
const checkDigestsIndexedNowStatement = `
		select d.block_id, d.text_sha256
		from document_block_versions i
		join document_blocks d on d.artifact_id = i.artifact_id
		where i.artifact_id = $1 and i.version = $2
		  and d.type = 'check' and d.block_id = any($3::text[])`

// clearReindexedDigests clears a computed digest if the present index holds a different digest
// for the same saved version. Re-indexing that version in place can change a check's spelling
// without dating the change (refs.BlockIndexing.KeepRespelled). The answer keeps the check's
// changed version but cannot give the digest (reason E). A digest from a later version is not
// evidence about the version at the cut, even if the check's changed version is the same.
func (s *server) clearReindexedDigests(ctx context.Context, artifactID string, number int, indexed map[string]model.CheckBlock) error {
	if len(indexed) == 0 {
		return nil
	}
	ids := make([]string, 0, len(indexed))
	for id := range indexed {
		ids = append(ids, id)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, checkDigestsIndexedNowStatement, artifactID, number, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var now string
		if err := rows.Scan(&id, &now); err != nil {
			return err
		}
		if check := indexed[id]; now != check.SHA256 {
			check.SHA256 = ""
			indexed[id] = check
		}
	}
	return rows.Err()
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
// is (checkBlocksAtTheCut), by the same rule the present read uses (checkWordsMoved), and is left
// out with the digest elsewhere (reason E): where the index recorded nothing of the check, and
// where a re-index in place changed its digest (clearReindexedDigests).
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
		live := readsTheLiveDeploy(*check)
		for number := range check.Results {
			result := resultAt(number)
			leaveOut(ctx, result+"/commit_stands", result+"/currency")
			if live {
				leaveOut(ctx, result+"/not_counted")
			}
			if check.SHA256 == "" {
				leaveOut(ctx, result+"/check_words_moved")
				continue
			}
			moved := checkWordsMoved(check, &check.Results[number])
			check.Results[number].CheckWordsMoved = &moved
		}
	}
}
