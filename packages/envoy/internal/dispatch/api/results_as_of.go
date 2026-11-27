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
// the words of each of its results moved. The answer gives a check's digest only where the block
// index holds now that same version and, for the check, that same digest (checkDigestsIndexedNow,
// clearReindexedDigests): no record gives the digest that a re-index of the version in place
// wrote, or its time, and an index that holds a later version, or none, holds no digest of the
// version at the cut. Everywhere else the answer leaves out the digest and keeps the version
// (reason E), and checksAtTheCut leaves out whether the words of each of its results moved. One
// batch reads the version and its record, one statement reads the digests that the block index
// holds now, the version is rendered only where that statement reads one, and nothing is read
// from the live document.
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
	// indexed is each check that the index recorded at the cut, with the version in which its words
	// last changed; checkDigestsAtTheCut gives it its digest where the block index holds it now.
	indexed := make(map[string]model.CheckBlock, len(checks))
	for _, check := range checks {
		if _, held := recorded.texts[check.BlockID]; held {
			indexed[check.BlockID] = model.CheckBlock{BlockID: check.BlockID, Version: recorded.versions[check.BlockID]}
		}
	}
	now, err := s.checkDigestsIndexedNow(ctx, artifact.ID, recorded.number, indexed)
	if err != nil {
		return nil, fmt.Errorf("read the digests that the block index holds now of the checks of %s: %w", artifact.ID, err)
	}
	if err := checkDigestsAtTheCut(markdown, recorded, indexed, now); err != nil {
		return nil, err
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

// checkDigestsAtTheCut gives each check of indexed, the checks that the block index recorded at
// the cut (recorded, its record of markdown, the stored text of the newest version at the cut),
// the digest that the index of that version wrote for the check, computed from the stored
// version, where now, the digests that the block index holds now at that same version
// (checkDigestsIndexedNow), holds that digest for the check. Every other check of indexed keeps
// no digest. The computed digest is a candidate only. The present read gives the digest that
// document_blocks.text_sha256 holds, and an index after the version's own can move it with no new
// text: the index of a later version, which a read at the cut does not see and whose digests now
// never holds, or a re-index of this version in place, which no record dates and which
// clearReindexedDigests compares for. The index of a version sums each block's slice of the
// rendering it indexed, the first block of each id (refs.PlanBlocks), and records that slice only
// where the version changed the block's text: a version that writes in this build's spelling a
// block whose recorded text another renderer version spelled keeps the block's changed version
// and records no text, and the sum moves to the new spelling (refs.BlockIndexing.KeepRespelled).
// So where this build renders markdown as it is stored (docs.VersionRenderingBlocks), as it
// renders every version its renderer wrote, the candidate is the sum of the check's slice of that
// rendering (refs.BlockTextSum). Where it renders markdown otherwise, another renderer version
// wrote it in a spelling whose blocks this build cannot cut, and the candidate is the sum of the
// text recorded, which is that version's own text of each check it changed. markdown is rendered
// only where now holds a digest, and a candidate is computed only for a check whose digest now
// holds.
func checkDigestsAtTheCut(markdown string, recorded blockIndex, indexed map[string]model.CheckBlock, now map[string]string) error {
	if len(now) == 0 {
		return nil
	}
	rendered, blocks, err := docs.VersionRenderingBlocks(markdown)
	if err != nil {
		return err
	}
	if rendered != markdown {
		for id, check := range indexed {
			if _, compared := now[id]; compared {
				check.SHA256 = refs.BlockTextSum(recorded.texts[id])
				indexed[id] = check
			}
		}
	} else {
		for _, block := range blocks {
			// Only the first block of an id is summed: no sum is empty, so a later one is passed over.
			check, held := indexed[block.ID]
			if _, compared := now[block.ID]; held && compared && check.SHA256 == "" {
				check.SHA256 = refs.BlockTextSum(rendered[block.From:block.To])
				indexed[block.ID] = check
			}
		}
	}
	clearReindexedDigests(indexed, now)
	return nil
}

// checkDigestsIndexedNowStatement reads the digests that the block index holds now for the named
// checks of document $1, only where it holds now version $2, the newest version at the cut: where
// it holds a later version, or none (a document owed its index, or one it never indexed), it
// reads no row. Both tables are read as they are now (asof.CutReadTables).
const checkDigestsIndexedNowStatement = `
		select d.block_id, d.text_sha256
		from document_block_versions i
		join document_blocks d on d.artifact_id = i.artifact_id
		where i.artifact_id = $1 and i.version = $2
		  and d.type = 'check' and d.block_id = any($3::text[])`

// checkDigestsIndexedNow is, by block id, the digest that the block index holds now for each check
// of indexed that it holds a row of, where it holds now version number of document artifactID,
// the newest version at the cut (checkDigestsIndexedNowStatement). It reads nothing where indexed
// holds no check, and it is empty where the index holds another version or none: no digest it
// holds then is a digest of the version at the cut, so checkDigestsAtTheCut renders nothing and
// the answer gives no digest.
func (s *server) checkDigestsIndexedNow(ctx context.Context, artifactID string, number int, indexed map[string]model.CheckBlock) (map[string]string, error) {
	if len(indexed) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(indexed))
	for id := range indexed {
		ids = append(ids, id)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, checkDigestsIndexedNowStatement, artifactID, number, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := make(map[string]string, len(ids))
	for rows.Next() {
		var id, digest string
		if err := rows.Scan(&id, &digest); err != nil {
			return nil, err
		}
		now[id] = digest
	}
	return now, rows.Err()
}

// clearReindexedDigests clears each candidate digest of indexed (checkDigestsAtTheCut) that
// differs from the digest that the block index holds now for the check at the version at the cut
// (now). A re-index of that version in place can move a check's digest with no new version and no
// record of the digest it wrote or of its time (refs.BlockIndexing.KeepRespelled), so the answer
// gives a digest only where the two agree, and then the stored version's own, never the present
// row's. Where they differ the answer keeps the check's changed version but gives no digest
// (reason E). A candidate alone is no evidence: that this build's renderer writes the stored
// version back as it is stored shows the digest this renderer gives, not the digest that an
// earlier re-index wrote.
func clearReindexedDigests(indexed map[string]model.CheckBlock, now map[string]string) {
	for id, check := range indexed {
		if check.SHA256 != "" && check.SHA256 != now[id] {
			check.SHA256 = ""
			indexed[id] = check
		}
	}
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
// where the block index does not hold now the version at the cut with the check's digest in it
// (checkBlocksAtTheCut).
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
