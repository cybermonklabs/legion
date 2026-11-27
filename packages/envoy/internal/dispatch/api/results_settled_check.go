package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// results_settled_check.go reads every fact of a check that the results routes judge or serve
// from one state of its Verification document: the block as the block index holds it. That is
// its changed version and its digest (document_blocks) and the text whose digest that is, which
// the index recorded at that version (document_block_texts); the check's attributes (`ci_check`,
// `when`, run, negative control, surface) and its words are read out of that text. The digest is
// the sha256 of the block's whole markdown, its opening line and that line's attributes included
// (refs.BlockTextSum), so a result stored against it names the job, the `when` and the words it
// was judged under. One block is the exception refs.BlockIndexing.KeepRespelled makes: a block
// whose changed version another renderer version stored, and which this build renders in another
// spelling of the same tree, keeps that version and takes this spelling's digest, while the text
// recorded at the version stays the other renderer's spelling; that text is read as the check,
// since it reads as the same tree, under the row's digest (settledCheckNode). No fact of a check
// comes from the live document: an edit a browser made that has not settled changes nothing for a
// write or a read until its settlement indexes it, and a check the index does not hold yet is no
// check of either route.

// settledCheckText joins each document_blocks row d to the text the block index recorded of it at
// its changed version, t, the text its text_sha256 is the digest of, and to that version's row,
// sv, whose renderer_version says which renderer spelled that text (settledCheckNode).
const settledCheckText = `left join document_block_texts t
		on t.artifact_id = d.artifact_id and t.block_id = d.block_id and t.version = d.changed_version
		left join artifact_versions sv on sv.artifact_id = d.artifact_id and sv.number = d.changed_version`

// settledCheckNode is check block blockID of document artifactID as the block index holds it:
// parsed out of stored, the text the index recorded at the block's changed version, which must be
// the text whose digest is sum, unless stamp, that version's renderer_version, names another
// renderer than this build's (pmdoc.OtherRenderer). Dispatch writes the row and the text in one
// transaction (refs.BlockIndexing.Queue), so for a version this build rendered a text that is
// missing or answers another digest is a fault of Dispatch's own records, never a state to read a
// check from. For a version another renderer stored, the row's digest is this build's spelling of
// the block and the text is that renderer's spelling of the same tree, which
// refs.BlockIndexing.KeepRespelled checked when it kept the version (pmdoc.FragmentReadsAs), so
// the text is read as the check and sum stays the check's digest.
func settledCheckNode(artifactID, blockID string, stored *string, sum string, stamp *int) (*pmdoc.Node, error) {
	if stored == nil || (refs.BlockTextSum(*stored) != sum && !pmdoc.OtherRenderer(stamp)) {
		return nil, fmt.Errorf("read check %s of document %s: the block index holds no text of it whose digest is %s", blockID, artifactID, sum)
	}
	tree, err := pmdoc.ParseRendering(*stored)
	if err != nil {
		return nil, fmt.Errorf("read check %s of document %s as the block index holds it: %w", blockID, artifactID, err)
	}
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); found == nil && node.Type == "check" && id == blockID {
			found = node
		}
		return found == nil
	})
	if found == nil {
		return nil, fmt.Errorf("read check %s of document %s: the text the block index holds of it is no check block %s", blockID, artifactID, blockID)
	}
	return found, nil
}

// settledResolution is the reading of check block check at indexed, out of stored, the block
// index's text of it at indexed.Version, whose digest is indexed.SHA256 unless stamp, that
// version's renderer_version, names another renderer (settledCheckNode): its attributes and its
// surface, carried with that version and digest.
func settledResolution(check checkBlockRef, indexed indexedCheck, stored *string, stamp *int) (checkResolution, error) {
	node, err := settledCheckNode(check.ArtifactID, check.BlockID, stored, indexed.SHA256, stamp)
	if err != nil {
		return checkResolution{}, err
	}
	block := docs.CheckBlockOf(node)
	return checkResolution{indexedCheck: indexed, Attributes: docs.CheckAttributesOfBlock(block), Surface: block.Surface}, nil
}

// resolveCheck finds the check a result names among the Verification documents of the recording
// issue and of every issue above it, as the block index holds each. The value is a block id, or
// the canonical block reference (`dispatch://KEY/artifact/verification-md#b-<id>`) that names one
// document when a block id is not unique in that set: an id two documents share is 409
// CHECK_AMBIGUOUS, and an id no Verification document of the set holds — or one that is not a
// `check` block, or a check the index does not hold yet, which a browser wrote and its settlement
// has not indexed — is 400 UNKNOWN_CHECK. The check's version, digest, attributes and surface are
// one state of it (results_settled_check.go).
//
// caller is the one project the request's credential names, empty for a credential that names
// none. Such a caller does not see a check of an issue of a private project that the credential
// does not name, so resolveCheck leaves out each such check before it counts the matches (seenBy):
// a private check alone is 400 UNKNOWN_CHECK, the answer of a check that does not exist, and a
// 409 CHECK_AMBIGUOUS names no issue that the caller does not see.
func (s *server) resolveCheck(ctx context.Context, q queryer, issueKey, caller, value string) (resolvedCheck, error) {
	ancestors, err := refs.IssueAncestors(ctx, q, issueKey)
	if err != nil {
		return resolvedCheck{}, err
	}
	scope := append([]string{issueKey}, ancestors...)

	blockID, artifactFilter := value, ""
	if ref, ok := text.CanonicalBlockRef(value); ok {
		// A block of a document carries its document's fields and no Item; a block of a comment
		// or a message names that item in Item and is no check.
		if ref.Item != "" || ref.IssueKey == "" || ref.ID == "" || ref.Block == "" {
			return resolvedCheck{}, unknownCheck(value)
		}
		blockID = ref.Block
		document, held, err := s.verificationDocument(ctx, q, ref.IssueKey, ref.ID)
		if err != nil {
			return resolvedCheck{}, err
		}
		if !held {
			return resolvedCheck{}, unknownCheck(value)
		}
		artifactFilter = document.ID
	}

	rows, err := q.Query(ctx, `
		select a.issue_key, a.slug, d.artifact_id::text, d.changed_version, d.text_sha256, t.text, sv.renderer_version,
		       i.project_key, p.visibility = 'private'
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		join issues i on i.key = a.issue_key
		join projects p on p.key = i.project_key
		`+settledCheckText+`
		where `+aliasedVerificationRole+`
		  and a.issue_key = any($1) and d.type = 'check' and d.block_id = $2
		  and ($3 = '' or d.artifact_id = $3::uuid)
		order by a.issue_key
	`, scope, blockID, artifactFilter)
	if err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	defer rows.Close()
	// matchedCheck is one check the value matches, with the block index's text of it and that
	// version's renderer, the reference its document's slug spells, and its issue's project and
	// whether that project is private, which the caller's own project is judged against below.
	type matchedCheck struct {
		check     resolvedCheck
		settled   *string
		stamp     *int
		reference string
		project   string
		private   bool
	}
	var matched []matchedCheck
	for rows.Next() {
		match := matchedCheck{check: resolvedCheck{checkBlockRef: checkBlockRef{BlockID: blockID}}}
		var slug string
		if err := rows.Scan(&match.check.IssueKey, &slug, &match.check.ArtifactID, &match.check.Version, &match.check.SHA256,
			&match.settled, &match.stamp, &match.project, &match.private); err != nil {
			return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
		}
		if reference, ok := text.FormatBlockRef(text.Ref{
			Kind: "block", IssueKey: match.check.IssueKey, ID: slug, Block: blockID,
		}); ok {
			match.check.Ref = reference
			match.reference = reference
		}
		matched = append(matched, match)
	}
	if err := rows.Err(); err != nil {
		return resolvedCheck{}, fmt.Errorf("resolve check %q of %s: %w", value, issueKey, err)
	}
	if caller != "" {
		matched = seenBy(caller, matched, func(match matchedCheck) (string, bool) { return match.project, match.private })
	}
	found := make([]resolvedCheck, 0, len(matched))
	texts := make([]*string, 0, len(matched))
	stamps := make([]*int, 0, len(matched))
	var references []string
	for _, match := range matched {
		found, texts, stamps = append(found, match.check), append(texts, match.settled), append(stamps, match.stamp)
		if match.reference != "" {
			references = append(references, match.reference)
		}
	}
	if len(found) == 0 {
		return resolvedCheck{}, unknownCheck(value)
	}
	if len(found) > 1 {
		issues := make([]string, len(found))
		for index, check := range found {
			issues[index] = check.IssueKey
		}
		return resolvedCheck{}, errorf(http.StatusConflict, "CHECK_AMBIGUOUS",
			"check %s is a check of the Verification document of %s: name one with its reference, %s",
			blockID, strings.Join(issues, " and "), strings.Join(references, " or "))
	}
	check := found[0]
	if check.checkResolution, err = settledResolution(check.checkBlockRef, check.indexedCheck, texts[0], stamps[0]); err != nil {
		return resolvedCheck{}, err
	}
	return check, nil
}

// settledCheckUnderLocks reads, under the locks the record route's transaction holds
// (recordResultUnderOwnerLocks), what the block index holds for one check block: the version
// its words last changed in, their digest, and the text whose digest that is, in one statement.
// A block the index no longer holds is no check any more. A pair that is the one resolved, the
// reading made before the transaction opened, is the same text, so the same reading, and that
// reading is the answer; a pair that moved is read again whole out of its own text, the one
// state of the check the row is stored against (settledResolution), and the caller judges the
// result again by it.
//
// The digest is document_blocks.text_sha256, the sha256 of the block's whole markdown as the
// index writes it (refs.BlockTextSum over markdown[From:To]), its opening line and that line's
// attributes included, so a check reworded from before_merge to after_merge, or given a
// ci_check, carries a digest of its own. The version is changed_version, which every version that
// changes the block's text moves, one that restores words an earlier version replaced included.
// Both are compared, though it is the version that catches every rewording, since
// changed_version moves on each of them. The digest is the backstop for a block whose text the
// index rewrote without that number moving, which no writer does today: refs.BlockIndexing.Queue
// writes both in one upsert.
//
// None of this is a read of the live document, which never happens under these locks: the
// pair and its text are one statement over the block index, and the document's owner lock,
// which a settlement writes the index under (refs.BlockIndexing.Queue), is the check's issue's,
// one of the two these locks hold, so what the statement reads stands until the commit.
func settledCheckUnderLocks(ctx context.Context, q queryer, check checkBlockRef, resolved checkResolution, named string) (checkResolution, error) {
	var indexed indexedCheck
	var settled *string
	var stamp *int
	err := q.QueryRow(ctx, `
		select d.changed_version, d.text_sha256, t.text, sv.renderer_version
		from document_blocks d
		`+settledCheckText+`
		where d.artifact_id = $1::uuid and d.block_id = $2 and d.type = 'check'
	`, check.ArtifactID, check.BlockID).Scan(&indexed.Version, &indexed.SHA256, &settled, &stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return checkResolution{}, unknownCheck(named)
	}
	if err != nil {
		return checkResolution{}, fmt.Errorf("read the check %s of document %s: %w", check.BlockID, check.ArtifactID, err)
	}
	if indexed == resolved.indexedCheck {
		return resolved, nil
	}
	return settledResolution(check, indexed, settled, stamp)
}

// checkBlocksOf is every `check` block of one Verification document as the block index holds it:
// each with its attributes and its words read out of the text the index recorded at its changed
// version, that version and its digest, its issue and its reference (results_settled_check.go).
// They come in the order of the document's newest stored version, and a block the index holds that
// that version does not, which its settlement has not indexed away yet, after them by id. A check
// a browser wrote and its settlement has not indexed is no check of the read until it is, as it is
// none of the record route (resolveCheck). The reference is built from the document's own slug,
// which is a property of the artifacts row and not of a block row. trees is the request's own
// reader, nil for a caller that asked for no tree: each check registers with it, as its
// text_tree, blockTextContent of that same stored text, the rule GET /api/v1/reference-targets
// reads the same block by, once the slice has stopped growing, since what is registered is a
// pointer into the row.
func (s *server) checkBlocksOf(ctx context.Context, q queryer, artifact model.Artifact, trees *displayTreeReader) ([]model.CheckBlock, error) {
	issueKey := ""
	if artifact.IssueKey != nil {
		issueKey = *artifact.IssueKey
	}
	rows, err := q.Query(ctx, `
		select d.block_id, d.changed_version, d.text_sha256, t.text, sv.renderer_version
		from document_blocks d
		`+settledCheckText+`
		where d.artifact_id = $1::uuid and d.type = 'check'
	`, artifact.ID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	defer rows.Close()
	settled := map[string]model.CheckBlock{}
	contents := map[string]string{}
	for rows.Next() {
		var blockID, sum string
		var version int
		var settledText *string
		var stamp *int
		if err := rows.Scan(&blockID, &version, &sum, &settledText, &stamp); err != nil {
			return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
		}
		node, err := settledCheckNode(artifact.ID, blockID, settledText, sum, stamp)
		if err != nil {
			return nil, err
		}
		check := docs.CheckBlockOf(node)
		check.ArtifactID, check.Text, check.Results = artifact.ID, blockWords(node), []model.CheckResult{}
		check.IssueKey, check.Version, check.SHA256 = issueKey, version, sum
		check.Ref, _ = text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: issueKey, ID: artifact.Slug, Block: blockID})
		settled[blockID] = check
		if trees != nil {
			contents[blockID] = blockTextContent(*settledText, true)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the indexed checks of %s: %w", artifact.ID, err)
	}
	order, err := storedCheckOrder(ctx, q, artifact.ID)
	if err != nil {
		return nil, err
	}
	checks := make([]model.CheckBlock, 0, len(settled))
	for _, blockID := range order {
		if check, held := settled[blockID]; held {
			checks = append(checks, check)
			delete(settled, blockID)
		}
	}
	rest := make([]string, 0, len(settled))
	for blockID := range settled {
		rest = append(rest, blockID)
	}
	slices.Sort(rest)
	for _, blockID := range rest {
		checks = append(checks, settled[blockID])
	}
	if trees != nil {
		for index := range checks {
			trees.text(&checks[index].TextTree, contents[checks[index].BlockID])
		}
	}
	return checks, nil
}

// storedCheckOrder is the id of each `check` block of document artifactID's newest stored
// version, in document order, the first holder of a repeated id alone, as the block index keeps
// it (typedBlocksOf). A document with no stored markdown version names none.
func storedCheckOrder(ctx context.Context, q queryer, artifactID string) ([]string, error) {
	var markdown string
	err := q.QueryRow(ctx, `
		select markdown from artifact_versions
		where artifact_id = $1::uuid and markdown is not null
		order by number desc limit 1
	`, artifactID).Scan(&markdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest version of document %s: %w", artifactID, err)
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, fmt.Errorf("read the check blocks of the newest version of document %s: %w", artifactID, err)
	}
	var order []string
	typedBlocksOf(tree, "check", func(_ *pmdoc.Node, id string) { order = append(order, id) })
	return order, nil
}
