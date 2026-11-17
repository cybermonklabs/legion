package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// BlockRef names one block of one document: the document's artifact id and the block's id.
type BlockRef struct {
	ArtifactID string
	BlockID    string
}

// logUnindexedAskBlocks logs each ask block with no ask row that a write to issueKey's
// documents marked resolved: it is not an ask yet, so it has no record to show. A settlement
// while the issue is closed keeps it resolved and opens no ask for it, and once the issue
// reopens, which marks its document owed (MarkReopenOwedAskDocuments), a settlement indexes it
// as a new open ask. WriteIssueAskStates and RebuildDocument each log the blocks they mark
// themselves, so every caller sees them logged once, whatever it does with them.
func logUnindexedAskBlocks(issueKey string, blocks []BlockRef) {
	for _, block := range blocks {
		slog.Warn("dispatch: mark resolved an ask block no settlement indexed before its issue closed",
			"issue", issueKey, "artifact", block.ArtifactID, "block", block.BlockID)
	}
}

// errSettleJoined refuses SettleIssueDocuments inside a transaction: each settlement opens a
// transaction of its own and takes the issue's row, which a caller's open transaction can hold.
var errSettleJoined = errors.New("docs: an issue's documents settle outside every transaction")

// issueDocumentIDs lists the documents of the issue $1, in id order: the order WriteIssueAskStates
// writes them and SettleIssueDocuments settles them.
const issueDocumentIDs = `select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id`

type lifecycleWriteContextKey struct{}

// withLifecycleWrite marks the live writes made with ctx as server lifecycle writes
// (WriteIssueAskStates): the server writing into an ask's blocks the state an issue's close or
// reopen gave it. Such a write is no edit of the document, so it credits no pending author and
// leaves the document's latest edit source (roomState.lastActor) as it was (applyLive). A
// settlement then names the person who last edited the document, as the author of an ask block it
// indexes with no author of its own and on its derived events, never the closer or the reopener,
// however the room is released and loaded between the close and the reopen: where a settlement
// while the issue was closed deleted the pending row that named that person, the row the reopen's
// write inserts names the editor that settlement kept (doc_closed_edit_sources,
// settlementPendingStatement). A settlement while the issue is closed indexes no ask block, and
// gives its derived events the settlement's own actor (reconcileAskBlocks, planRepairs).
func withLifecycleWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, lifecycleWriteContextKey{}, true)
}

func isLifecycleWrite(ctx context.Context) bool {
	lifecycle, _ := ctx.Value(lifecycleWriteContextKey{}).(bool)
	return lifecycle
}

// WriteIssueAskStates writes the state of each of asks into the blocks that show it in every
// document of issueKey, in the documents' id order: an ask's own block on its document, and every
// block readCopiedAskSources classifies as a copy of one of asks on another, which keeps naming its
// source (copied_from, copied_from_document). Each block gets the server attributes settlement
// writes from the ask (setAskServerAttributes), so asks carry the state the caller wants shown:
// resolved for an issue's close, open for its reopen. Every other block is left as it is - the
// blocks of other asks, their copies, a block under one of asks' block ids that asks something
// else - and so is a block settlement could not read. Classification reads the database as it
// stands now: the caller writes the asks' rows after this for a close, and before it for a reopen.
//
// A document outside the Proof schema is left as it is, as settlement leaves it, and so is one
// whose stored history does not load (ErrDocumentUnloadable): nothing can read or open it, so no
// reader sees its blocks open, and the history a retry would meet is the same. Each is logged,
// naming the issue and the document, and the other documents are written as if it were not
// there, so the close, reopen or repair writing them is not refused for good. While the issue is
// closed, a rebuild from the latest version is the one way such a document loads again, and that
// rebuild writes the close's states into the document it seeds (RebuildDocument), so the blocks
// this left as they were do not come back open on the closed issue. Every other error, a failed or
// rebuilding room's ErrServiceUnavailable and the database's included, is returned.
//
// An answered ask, which the close's final sweep passes, is written into its copies alone, never
// into its own block: the answer route writes that block itself (SetBlockAttributes), and a write
// from the row could put back an answer it left out for size. Each copy gets the ask's state,
// answerer and time with the answer left out, as settlement writes a block whose document has no
// room for its answer (withholdAnswers): no write here is weighed for size, so none adds an
// answer's bytes to a document. A copy that already holds the answer in full is left as it is
// (answeredCopyAttributes), and the document's next settlement, which this write leaves owed and
// which runs whether or not the issue is closed, writes the answer into the rest as it writes any
// copy (withholdAnswers).
//
// With markUnindexed it also writes state="resolved" into every readable ask block that neither
// has an ask of its own on its document nor is a copy of another document's ask: a block no
// settlement has indexed yet, which no longer reads open once its issue has closed. A settlement
// while the issue is closed keeps it resolved with no ask, and once the issue reopens, which marks
// its document owed (MarkReopenOwedAskDocuments), a settlement indexes it as a new open ask.
// Once every document is written, it logs each such block it changed (logUnindexedAskBlocks), so
// no caller logs them again, and returns them for a caller that counts them. Among these is every
// readable ask block that repeats the id an earlier ask block of its document holds and does not
// read state="resolved" - a browser's paste copies a block with its id - which a write addressed
// by id cannot reach, since such a write lands on the id's first holder (repairLive). It is
// written by its place among the document's ask blocks instead (markRepeatedAskBlocks), and
// logged and returned under the id it repeats. Its id is left as it is: stamping it an id of its
// own is settlement's job. The document's next settlement stamps it, and keeps it resolved with no
// ask while the issue is closed, as it keeps any block with no row (reconcileAskBlocks); a
// settlement after the reopen indexes it as a new open ask.
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as settlement holds it, so the writes are owner-verified: they reach a document whose room
// is flagged closed, and one no room holds, while every other write to a closed issue's document
// is refused. A write credits no one (withLifecycleWrite): it adds no pending author, leaves the
// authors already pending and the document's latest edit source as they were, and writes no
// version: the settlement it leaves owed versions it, and none whose markdown repeats the latest
// version is written. A document it changes nothing in is written nothing.
func (s *Service) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]BlockRef, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return nil, errUnjoined
	}
	if len(asks) == 0 && !markUnindexed {
		return nil, nil
	}
	given := make(map[string]model.Ask, len(asks))
	for _, ask := range asks {
		given[ask.ID] = ask
	}
	rows, err := tx.Query(ctx, issueDocumentIDs, issueKey)
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	ctx = withLifecycleWrite(withOwnerVerified(ctx))
	owner := artifactOwner{IssueKey: &issueKey}
	var unindexed []BlockRef
	for _, document := range documents {
		marked, err := s.writeDocumentAskStates(ctx, tx, document, owner, given, actor, markUnindexed)
		if err != nil {
			return nil, fmt.Errorf("write the ask states of document %s: %w", document, err)
		}
		unindexed = append(unindexed, marked...)
	}
	logUnindexedAskBlocks(issueKey, unindexed)
	return unindexed, nil
}

// writeDocumentAskStates is WriteIssueAskStates for one document, artifactID, in one live write,
// and returns the unindexed blocks it marked resolved.
func (s *Service) writeDocumentAskStates(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	asks map[string]model.Ask,
	actor model.Actor,
	markUnindexed bool,
) ([]BlockRef, error) {
	given := func(row model.Ask) (model.Ask, bool) {
		ask, wanted := asks[row.ID]
		return ask, wanted
	}
	var writes *issueAskStateWrites
	err := s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current pmdoc.ValidDocument, _ func(func(*crdt.Transaction))) error {
		if current.Node() == nil {
			// Settlement reads none of its ask blocks either (settleRoomWithin).
			slog.Warn("dispatch: leave the ask blocks of a document outside Proof schema as they are", "artifact", artifactID)
			return nil
		}
		var err error
		writes, err = readIssueAskStateWrites(ctx, tx, artifactID, owner, current.Node(), given, markUnindexed)
		if err != nil || writes.empty() {
			return err
		}
		// The writes are made on the fork as it stands (rewriteLive), each on the block carrying its
		// id, as settlement makes its repairs (repairLive), and each repeat on the block at its place
		// (markRepeatedAskBlocks), all in one rewrite of the fork, whose tree is the one current read.
		// A document none of them changes is left as it was, and applyLive writes nothing for it.
		_, err = rewriteLive(doc, nil, writes.apply)
		return err
	})
	switch {
	case err == nil, errors.Is(err, websocket.ErrNoChanges):
	case errors.Is(err, ErrDocumentUnloadable) && !errors.Is(err, ErrServiceUnavailable):
		// Nothing can load the document, so no reader sees its blocks open, and a retry meets the
		// same history: the close, reopen or repair writing it goes on without it. The rebuild
		// that makes it load again writes its closed issue's states itself (RebuildDocument).
		slog.Warn("dispatch: leave the ask blocks of a document whose history cannot load as they are",
			"issue", *owner.IssueKey, "artifact", artifactID, "error", err)
		return nil, nil
	default:
		return nil, err
	}
	if writes == nil {
		return nil, nil
	}
	return writes.marked, nil
}

// reopenOwedAskDocuments lists, in id order, the documents of the issue $1 whose ask blocks a
// settlement after the issue's reopen has to reconcile: one that holds an ask block no settlement
// has indexed, the blocks doc_ask_block_authors keeps an author for; one that is the block
// document (asks.block_artifact_id) of an open ask, read once the reopen has restored the asks the
// close withdrew; and one that is the block document of an ask a settlement retracted. The
// retraction is settlementRetraction's rule in SQL, read off the stored resolution: kind
// "retracted", and an actor $2 (kind) and $3 (id) or a reason that opens with $4
// (SettlementActor, SettlementRetractionReason), so the two change together.
const reopenOwedAskDocuments = `
	select a.id::text from artifacts a
	where a.issue_key = $1 and a.kind = 'doc'
		and (exists (select from doc_ask_block_authors b where b.artifact_id = a.id)
			or exists (
				select from asks k
				where k.block_artifact_id = a.id and k.block_id is not null and k.state = 'open')
			or exists (
				select from asks k
				where k.block_artifact_id = a.id and k.block_id is not null and k.state = 'resolved'
					and k.resolution->>'kind' = 'retracted'
					and ((k.resolution->'actor'->>'kind' = $2 and k.resolution->'actor'->>'id' = $3)
						or starts_with(k.resolution->>'reason', $4))))
	order by a.id`

// MarkReopenOwedAskDocuments marks owing a settlement each document of issueKey whose ask blocks a
// settlement after the issue's reopen has to reconcile. The issue's reopen calls it once it has
// restored the asks the close withdrew and written their state into their blocks
// (WriteIssueAskStates), which marks owed only a document it writes: one holding a block of a
// restored ask, or a copy of one. A settlement of a closed issue's document changes no ask and
// deletes the document's pending row, as every settlement that commits does (reconcileAskBlocks,
// clearSettlementPending), so without this mark the documents below would get no settlement until
// their next edit:
//
//   - one that holds an ask block no settlement has indexed, a document with a row in
//     doc_ask_block_authors, the durable record of those blocks and their authors
//     (ask_authors.go): the closed issue's settlement saved such a block resolved and recorded its
//     author, and the settlement after the reopen opens its ask, credited to that author;
//   - one that is the block document of an open ask, the document asks.block_artifact_id names,
//     where settlement reads the ask's own block (loadAskBlocks): when the block of an ask the
//     close withdrew left that document while the issue was closed, the closed issue's settlement
//     left the ask withdrawn, the reopen restored it with no block to write, and the settlement
//     after the reopen retracts it, as it retracts an open ask whose block left on an open issue.
//     A copy of the block in another document of the issue is no block of the ask's own: the
//     reopen's ask-state write writes it, and the retraction settles it again (owedCopiesOf);
//   - one that is the block document of an ask a settlement retracted when its block left
//     (settlementRetracted): when the block came back while the issue was closed, the closed
//     issue's settlement left the ask retracted, and the settlement after the reopen restores it,
//     as it restores a returning block's ask on an open issue.
//
// A document marked for an open ask whose block it still holds, or for a retracted ask whose block
// has not come back, gets a settlement that finds nothing to change and writes no version.
//
// The mark sets marked_at to now, which a new row takes as its default, and leaves the latest
// edit source and the latest person sender a row names as they are; a row it inserts names the
// editor a settlement while the issue was closed kept (doc_closed_edit_sources,
// settlementPendingStatement), so the settlement the mark leaves owed names that editor on the
// events of the ask blocks that editor's text holds, and names SettlementActor on a version that
// adds none of that text (settlementAuthors).
// It arms nothing: the room's next load, a timer or the resumption runs the settlement
// (onLoadDocument, RunSettlementResumption).
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as a settlement of each of the issue's documents holds it, so no settlement reads or
// changes the rows this lists between the list and the mark. It reads the asks as that
// transaction has written them, so the restored asks read open.
func (s *Service) MarkReopenOwedAskDocuments(ctx context.Context, issueKey string) error {
	tx, joined := txFromContext(ctx)
	if !joined {
		return errUnjoined
	}
	rows, err := tx.Query(ctx, reopenOwedAskDocuments, issueKey, SettlementActor.Kind, SettlementActor.ID, SettlementRetractionReason)
	if err != nil {
		return fmt.Errorf("list the issue's documents whose ask blocks wait for its reopen: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the issue's documents whose ask blocks wait for its reopen: %w", err)
	}
	if len(documents) == 0 {
		return nil
	}
	return markSettlementPending(ctx, tx, documents, nil, false, true, nil, false)
}

// FollowClosedIssueVersions does, at the reopen of issueKey, what writeVersionTx skips for a
// version that a settlement of a closed issue's document writes (versionWrite.closedIssue): for
// each document of the issue, in id order, it moves the open approval request that names the
// document to the document's latest version (MoveApprovalAsk), and then carries an approval to
// that version (carryStandingApproval), as the version's own write does on an open issue. Before
// a closed issue's document settled, the first settlement after the reopen wrote that text and
// did both. Without them a restored request names the version from before the close, so a
// person's approval from it reviews that version and reads stale, and an approved text that a
// settlement wrote back while the issue was closed stays without its approval. The issue's reopen
// calls it once it has restored the asks the close withdrew, so each restored request is open
// again, and once their events are in, so a request's move and the carry's answer follow its
// ask.opened.
//
// A document whose stored updates are ahead of its latest version (contentChangedSinceVersion) is
// left to the settlement it owes. The reopen's own ask-state write is such an update, and that
// settlement's version holds the text the reopen leaves, which its own write compares with the
// version the request names and with the approved text. A reopen that writes back the text the
// person was asked about then keeps the request with the person, which a move to the closed
// issue's version, whose blocks show the close's resolved states, would not. Where that
// settlement's version lists no author, it adds none of the kept editor's text and names
// SettlementActor, but its move of a request that names a version from before the close spans the
// closed issue's versions and names the kept editor, as this move does (versionWrite.keptEditor).
//
// The one case this leaves stale is a document whose stored updates past its latest version render
// that version again. Its settlement writes no version, since a settlement versions only a
// rendering that differs from the latest version, so nothing moves the request or runs the carry,
// and the request keeps naming the version from before the close. Such updates come only after
// the closed issue's settlement that wrote the latest version, which stored every update it read.
// So they are a browser's edit and its exact undo, both stored after that settlement and inside
// the close margin, the one window in which the append gate stores a closed issue's browser
// update, so that settlement must also have run inside the margin; or the seed of a rebuild, from
// that version, of a document whose history cannot load. The close's sweep and the ask repair
// write nothing into a document whose blocks already show the states they write.
//
// Nothing is moved or carried twice. MoveApprovalAsk moves nothing when the request already names
// the latest version, as it does after every version written while the request was open, whose
// own write moved it. The carry runs only while the newest review approves a version before the
// latest one: a version whose own write carried an approval, or that this carried at an earlier
// reopen, is already approved. A version whose own write ran the carry and carried nothing is
// refused again, since the rule reads its stored text and the rows as they stand. So the ask
// repair's restore after a reopen, and a later reopen, change nothing more.
//
// The move names the editor that a settlement while the issue was closed credited and kept for the
// document (doc_closed_edit_sources, clearSettlementPending): the edit that moves the request is
// that editor's, saved in that settlement's version, not the reopener's act, and the version's
// own write on an open issue names its editor so. Before a closed issue's document settled, the
// first settlement after the reopen named the same editor, read off the pending row that stood
// through the closed period, so a move that hands the request to its agent wakes the route and
// the claimant as it did then. A document with no kept source, whose closed issue's settlements
// named no editor, is moved in the name of SettlementActor, the actor of a settlement that cannot
// name the editor: such an ask.edited wakes nobody itself (events.Broker.Notify), the reopen's own
// ask.opened in the reopener's name does, and it reaches the request's followers as every
// ask.edited does that is not quiet. The carry's events are in the approver's name, as every
// carry's are.
//
// Once every document is followed, it deletes the kept source of each document of the issue that
// owes no settlement: no settlement left owed by the reopen names it, and a row a later write
// inserts naming no editor of its own would name that old editor (settlementPendingStatement).
// The kept source of a document that owes one stays for that settlement, which deletes it.
//
// It runs in the transaction ctx joined (errUnjoined otherwise), and its caller holds the issue's
// row, as a settlement of each of the issue's documents holds it, so no settlement writes a
// version, or a kept source, between the read of the latest one and the move. It takes no copy
// lock and writes no pending row: the move and the carry write asks rows, reviews and events, and
// the cleanup deletes kept sources alone. It returns the events it appended, in that order, for
// the caller to publish once it commits, after its own.
func (s *Service) FollowClosedIssueVersions(ctx context.Context, issueKey string) ([]model.Event, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return nil, errUnjoined
	}
	rows, err := tx.Query(ctx, issueDocumentEditSources, issueKey)
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (closedEditSource, error) {
		var document closedEditSource
		err := row.Scan(&document.artifactID, &document.lastActor)
		return document, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the issue's documents: %w", err)
	}
	var appended []model.Event
	for _, document := range documents {
		mover := SettlementActor
		if document.lastActor != nil {
			if err := json.Unmarshal(document.lastActor, &mover); err != nil {
				return nil, fmt.Errorf("decode the closed edit source of document %s: %w", document.artifactID, err)
			}
		}
		followed, err := s.followLatestVersion(ctx, tx, document.artifactID, mover)
		if err != nil {
			return nil, fmt.Errorf("follow the latest version of document %s: %w", document.artifactID, err)
		}
		appended = append(appended, followed...)
	}
	if _, err := tx.Exec(ctx, dropSettledClosedEditSources, issueKey); err != nil {
		return nil, fmt.Errorf("delete the closed edit sources of the issue's settled documents: %w", err)
	}
	return appended, nil
}

// closedEditSource is one document of an issue as issueDocumentEditSources lists it: its id and
// the editor a settlement while the issue was closed kept for it, the stored jsonb, nil for none.
type closedEditSource struct {
	artifactID string
	lastActor  []byte
}

// issueDocumentEditSources lists the documents of the issue $1 in id order, as issueDocumentIDs
// does, each with its kept closed edit source (doc_closed_edit_sources), null for none.
const issueDocumentEditSources = `
	select a.id::text, kept.last_actor
	from artifacts a left join doc_closed_edit_sources kept on kept.artifact_id = a.id
	where a.issue_key = $1 and a.kind = 'doc'
	order by a.id`

// dropSettledClosedEditSources deletes the kept closed edit source of each document of the issue
// $1 that owes no settlement (FollowClosedIssueVersions).
const dropSettledClosedEditSources = `
	delete from doc_closed_edit_sources kept
	using artifacts a
	where a.id = kept.artifact_id and a.issue_key = $1 and a.kind = 'doc'
		and not exists (select from doc_settlements_pending p where p.artifact_id = kept.artifact_id)`

// followLatestVersion is FollowClosedIssueVersions for one document, artifactID, moving its
// request in mover's name.
func (s *Service) followLatestVersion(ctx context.Context, tx pgx.Tx, artifactID string, mover model.Actor) ([]model.Event, error) {
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	owed, err := contentChangedSinceVersion(ctx, tx, artifactID, latest.docUpdateVersion)
	if err != nil || owed {
		return nil, err
	}
	// A latest version the request does not name yet was written while the issue was closed,
	// since every version written while it was open moved the request: by a settlement, or by a
	// comment anchor's snapshot (SnapshotVersion under WithCommentMarks), the one route that writes
	// a closed issue's version. No stored version says which wrote it, so the move reads it as a
	// settlement's and keeps the person's turn where that settlement's own version write would
	// have kept it.
	moved, err := MoveApprovalAsk(ctx, tx, s.events, artifactID, latest.Version, latest.markdown, mover, true, s.serverURL)
	if err != nil {
		return nil, err
	}
	newest, err := NewestReview(ctx, tx, artifactID)
	if err != nil || newest == nil || newest.State != "approved" || newest.Version >= latest.Number {
		return moved, err
	}
	// The answer carry reads the version's blocks from its tree, as the version's own write reads
	// the live tree it rendered: the stored text's read-back is that tree.
	tree, err := pmdoc.ParseRendering(latest.markdown)
	if err != nil {
		return moved, skipUnreadableVersion(artifactID, latest.Number, err)
	}
	carried, err := s.carryStandingApproval(ctx, tx, artifactID, latest.Number, renderedTree{tree: tree, markdown: latest.markdown}, *newest)
	if err != nil {
		return nil, err
	}
	return append(moved, carried...), nil
}

// issueAskStateWrites is what WriteIssueAskStates writes into one document of an issue, read off
// one tree of it (readIssueAskStateWrites) and made on a tree by apply.
type issueAskStateWrites struct {
	artifactID string
	// writes are the attribute writes to the blocks carrying their ids (repairLive).
	writes settlementReconciliation
	// repeats are the blocks repeating an earlier ask block's id, each marked resolved at its
	// place (markRepeatedAskBlocks).
	repeats []repeatedAskBlock
	// marked are the unindexed blocks apply changed to read state="resolved".
	marked []BlockRef
}

// readIssueAskStateWrites reads, in tx, the writes WriteIssueAskStates makes to tree, the document
// artifactID of owner, an issue whose row the caller holds. For each readable ask block that an
// ask of the document indexes, or that readCopiedAskSources classifies as a copy of an ask on
// another document, wanted is handed that ask's row and answers the ask whose state the block is
// to show and whether it is to be written at all. An answered ask's own block is never written,
// and its copies are written with the answer left out (answeredCopyAttributes). With
// markUnindexed, every other readable ask block, and every one repeating an earlier ask block's id
// that does not read resolved, is marked resolved. It changes nothing in tree.
func readIssueAskStateWrites(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	owner artifactOwner,
	tree *pmdoc.Node,
	wanted func(row model.Ask) (model.Ask, bool),
	markUnindexed bool,
) (*issueAskStateWrites, error) {
	found := &issueAskStateWrites{artifactID: artifactID}
	blocks, repeats := readableAskBlocks(tree)
	if markUnindexed {
		found.repeats = repeats
	}
	if len(blocks) == 0 {
		return found, nil
	}
	rows, err := loadAskBlocks(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	// An issue's documents take no copy lock (lockProjectCopies): the caller holds the issue's
	// row, as every transaction reading or marking the copies among its documents does.
	sources, err := readCopiedAskSources(ctx, tx, artifactID, owner, blocks, unindexedAskBlockIDs(blocks, rows))
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if row, indexed := rows[block.id]; indexed {
			if ask, write := wanted(row); write && ask.State != "answered" {
				found.writes.repairs = append(found.writes.repairs, askRepair(block.id, askServerAttributes(ask, "", false)))
			}
			continue
		}
		if source, copied := sources[block.id]; copied {
			if ask, write := wanted(source.ask); write {
				set := askServerAttributes(ask, source.document, false)
				if ask.State == "answered" {
					set = answeredCopyAttributes(ask, source.document)
				}
				found.writes.repairs = append(found.writes.repairs, askRepair(block.id, set))
			}
			continue
		}
		if markUnindexed {
			ref := BlockRef{ArtifactID: artifactID, BlockID: block.id}
			found.writes.repairs = append(found.writes.repairs, askRepair(block.id, func(node *pmdoc.Node) bool {
				if !resolveUnindexedAskBlock(node) {
					return false
				}
				found.marked = append(found.marked, ref)
				return true
			}))
		}
	}
	return found, nil
}

// empty reports whether w has nothing to write.
func (w *issueAskStateWrites) empty() bool {
	return len(w.writes.repairs) == 0 && len(w.repeats) == 0
}

// apply makes w's writes on live, a tree of w's document, recording each unindexed block it marks
// resolved, and reports whether any changed it.
func (w *issueAskStateWrites) apply(live *pmdoc.Node) bool {
	repaired := w.writes.repairLive(live)
	repeated := markRepeatedAskBlocks(live, w.artifactID, w.repeats)
	w.marked = append(w.marked, repeated...)
	return repaired || len(repeated) > 0
}

// closedIssueAskState is the ask a block showing row shows once row's issue has closed, as the
// close's final sweep writes it: the row as it stands, for a resolved ask - among them every ask
// the close withdrew - and for an answered one. An open row is none: no close leaves one, and the
// repair that withdraws the asks a close by an older release left open writes their blocks.
func closedIssueAskState(row model.Ask) (model.Ask, bool) {
	return row, row.State == "resolved" || row.State == "answered"
}

// resolveUnindexedAskBlock writes state="resolved" into node, a readable ask block that is no ask
// yet - no ask row indexes it, or it repeats the id an earlier ask block of its document holds,
// and it is no copy of another document's ask - and reports whether that changed it: the state
// such a block shows while its issue is closed, since the close withdrew every ask of the issue.
// It is the one write of that state. The close's final sweep makes it (WriteIssueAskStates with
// markUnindexed: readIssueAskStateWrites, markRepeatedAskBlocks), and so does a settlement of a
// closed issue's document, which opens no ask for such a block (reconcileAskBlocks). The block
// stays without a row, and a settlement after the issue reopens opens its ask.
func resolveUnindexedAskBlock(node *pmdoc.Node) bool {
	if node.Attrs["state"] == "resolved" {
		return false
	}
	node.Attrs["state"] = "resolved"
	return true
}

// answeredCopyAttributes is the write WriteIssueAskStates makes to a copy of ask, an answered ask,
// whose source document's address is copied (copied_from_document): the copy's server attributes
// are made to agree with ask with the answer left out (setAskServerAttributes with withholdAnswer).
// A copy whose attributes already agree with ask, answer and all, as a settlement before the
// issue's close wrote them, is left as it is: leaving its answer out would take from the closed
// issue's document an answer its row still holds.
func answeredCopyAttributes(ask model.Ask, copied string) func(*pmdoc.Node) bool {
	return func(node *pmdoc.Node) bool {
		if !changesAskBlock(node, askServerAttributes(ask, copied, false)) {
			return false
		}
		changed, _ := setAskServerAttributes(node, ask, copied, true)
		return changed
	}
}

// changesAskBlock reports whether set, a write to an ask block's attributes, would change node,
// by running it on a copy of node's attributes, which leaves node as it is.
func changesAskBlock(node *pmdoc.Node, set func(*pmdoc.Node) bool) bool {
	clone := &pmdoc.Node{Type: node.Type, Attrs: make(pmdoc.Attrs, len(node.Attrs)+len(askServerAttributeNames))}
	maps.Copy(clone.Attrs, node.Attrs)
	return set(clone)
}

// readableAskBlocks is every ask block of tree settlement reads (parseAskBlock), each id's first
// holder alone: a write addressed by id lands on the first block carrying it (repairLive), and a
// later block repeating it is no ask until settlement stamps it an id of its own. Unlike
// collectAskBlocksForSettlement, a repeated id leaves the rest of the document readable, so a
// browser's paste does not stop an issue from closing.
//
// It returns apart, as repeats, every later holder settlement reads that carries a non-empty id an
// earlier ask block of tree already holds and does not read state="resolved", each with its place
// among tree's ask blocks in document order: the address a write to it takes, which its id is not
// (markRepeatedAskBlocks).
func readableAskBlocks(tree *pmdoc.Node) ([]askBlock, []repeatedAskBlock) {
	var blocks []askBlock
	var repeats []repeatedAskBlock
	seen := map[string]struct{}{}
	index := -1
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		index++
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated := seen[id]; repeated {
			if id != "" && node.Attrs["state"] != "resolved" {
				if _, err := parseAskBlock(node); err == nil {
					repeats = append(repeats, repeatedAskBlock{index: index, id: id})
				}
			}
			return true
		}
		seen[id] = struct{}{}
		if block, err := parseAskBlock(node); err == nil {
			blocks = append(blocks, block)
		}
		return true
	})
	return blocks, repeats
}

// repeatedAskBlock is an ask block that repeats the id an earlier ask block of its document holds
// (readableAskBlocks): index is its place among the document's ask blocks in document order, and
// id the id it repeats.
type repeatedAskBlock struct {
	index int
	id    string
}

// markRepeatedAskBlocks writes state="resolved" into each of repeats in live, the tree of the
// document artifactID that readableAskBlocks read them from: into the ask block at the repeat's
// place among live's ask blocks, while that block still carries the id the repeat names and does
// not read resolved already. It returns a BlockRef, under the repeated id, for each block it
// changed. repeats are in document order, as readableAskBlocks returns them.
func markRepeatedAskBlocks(live *pmdoc.Node, artifactID string, repeats []repeatedAskBlock) []BlockRef {
	if len(repeats) == 0 {
		return nil
	}
	var marked []BlockRef
	next, index := 0, -1
	pmdoc.Walk(live, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		index++
		if index < repeats[next].index {
			return true
		}
		repeat := repeats[next]
		next++
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == repeat.id && resolveUnindexedAskBlock(node) {
			marked = append(marked, BlockRef{ArtifactID: artifactID, BlockID: repeat.id})
		}
		return next < len(repeats)
	})
	return marked
}

// The bounds on SettleIssueDocuments' retries. Every settlement it runs is one the close of an
// issue waits for before it opens its transaction, and the request that closes the issue waits
// with it, so the retries are few and short: a settlement that stopped short because the room's
// updates had not landed yet, or because the room had to be reloaded at its durable head, settles
// on the next attempt, and one that has not settled in three is not going to settle while a person
// waits on the close. The budget is for the whole issue, not each document. It bounds the waits for
// the rooms' durable appends and whether a retry starts, not a settlement already running, and
// every document gets its first attempt whatever is left of it. The pause is a few times the
// millisecond the room's own waits poll at, long enough for a persistence worker to store the
// update an attempt met.
const (
	issueSettleAttempts   = 3
	issueSettleBudget     = 3 * time.Second
	issueSettleRetryPause = 20 * time.Millisecond
)

// SettleIssueDocuments runs, on the calling goroutine, the settlement each document of issueKey
// owes, as Shutdown's drain does: it waits for the room's durable appends to land, reads whether
// the document owes a settlement, and settles it at its room's current generation, in place of the
// timer armed for it, if one is. Every new ask block the documents hold is then an ask, credited
// to its author as settlement credits one, which the close of an issue withdraws with the rest:
// a settlement after the close opens no ask, and saves a block it finds with none resolved
// (reconcileAskBlocks).
//
// A settlement that stops short (settleOutcome) is run again, up to issueSettleAttempts times for
// each document and within issueSettleBudget for the whole issue, no retry starting once the
// budget is spent (every document still gets its first attempt, as before the retries): a timer's
// settlement would retry it later (followUpSettlement), but the close does not wait for later, and
// a block left unindexed when it commits is no ask it can withdraw with a record. A settlement
// stops short on a room that holds updates its persistence has not stored or made durable yet,
// which the next attempt, after a pause of issueSettleRetryPause, finds stored; on one superseded
// by a write that landed while it ran, which the next attempt settles with that write; and on a
// room whose stored history was replaced under it, which the settlement closes for a reload
// (reloadReplacedRoom) and leaves owing its settlement, so the next attempt warms the room from
// the head and settles every update. A room another envoy-dispatch process's append left behind
// is caught up under the settlement's own lock (catchUpRoom), and that settlement goes on. A
// settlement still short after its last attempt, or once ctx ends or the service stops, which no
// further attempt would get past, is logged and returned, joined with the others: the document's
// pending-settlement row keeps the debt, for the room's next load or the resumption
// (RunSettlementResumption).
//
// It opens a transaction of its own for each settlement, which takes the issue's row, so it is
// refused inside a transaction: a joined context's, or, for a context the pool tracks, any
// (store.ErrNestedAcquire).
func (s *Service) SettleIssueDocuments(ctx context.Context, issueKey string) error {
	if ledgerFrom(ctx).inTransaction() {
		return errSettleJoined
	}
	rooms, err := s.documentRooms(ctx, "the issue's documents", issueDocumentIDs, issueKey)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(issueSettleBudget)
	var failures []error
	for _, room := range rooms {
		var err error
		for attempt := 1; attempt <= issueSettleAttempts && (attempt == 1 || time.Now().Before(deadline)); attempt++ {
			if attempt > 1 && (ctx.Err() != nil || s.stopping.Load()) {
				break
			}
			if err = s.settleOwedNow(ctx, room, deadline); err == nil || attempt == issueSettleAttempts {
				break
			}
			pause := time.NewTimer(min(issueSettleRetryPause, time.Until(deadline)))
			select {
			case <-ctx.Done():
				pause.Stop()
			case <-pause.C:
			}
		}
		if err != nil {
			slog.Warn("dispatch: settle a document of an issue", "issue", issueKey, "room", room, "error", err)
			failures = append(failures, fmt.Errorf("settle document %s: %w", room, err))
		}
	}
	return errors.Join(failures...)
}

// settleOwedNow settles room, when its document owes a settlement, on the calling goroutine, and
// reports why the document still owes one afterwards: the reason a settlement that stopped short
// gives (settleOutcome), whatever stopped it, in place of the retry or re-arming a timer's
// settlement would do (followUpSettlement); or, for one that ended with no reason - among them one
// another write superseded and one the gate refused on a room behind its durable head, which leave
// the debt to the pending-settlement row - that the row still stands. Its caller decides whether to
// run it again (SettleIssueDocuments). It waits for the room's durable appends to land first, but
// not past deadline, so no attempt hangs past its caller's budget waiting for them; the settlement
// itself runs under ctx alone. It takes the settleWG count of the timer it runs in place of, or one
// of its own, so Shutdown joins it as it would the timer's settlement, and holds the room's state
// across it, so the state the settlement looks up is the one whose generation it runs at.
func (s *Service) settleOwedNow(ctx context.Context, room string, deadline time.Time) error {
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	err := s.waitForDurableAppends(waitCtx, room)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for the room's appends to become durable: %w", err)
	}
	owes, err := settlementPending(ctx, s.store.Pool, room)
	if err != nil || !owes {
		return err
	}
	state := s.lockState(room)
	if !s.takeSettleTimer(state.settle) && !s.addUnlessStopping(&s.settleWG) {
		s.unlockState(room, state)
		return ErrServiceUnavailable
	}
	generation := state.roomGeneration
	state.settling++
	s.unlockState(room, state)
	outcome := s.settleRoomWithin(ctx, room, generation, false)
	state.mu.Lock()
	state.settling--
	s.unlockState(room, state)
	s.retireSettlementRoom(room)
	s.settleWG.Done()
	if outcome.err != nil {
		return outcome.err
	}
	if owes, err = settlementPending(ctx, s.store.Pool, room); err != nil {
		return err
	}
	if owes {
		return errors.New("the document still owes a settlement")
	}
	return nil
}
