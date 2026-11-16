package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version whose markdown the fresh document state holds - the latest saved one, with a closed
// issue's ask states written into it (RebuildDocument), or the one a rebuild from supplied
// markdown that changed the document wrote; Head is the new monotonic update version.
type RebuildReport struct {
	Head               int64  `json:"head"`
	RemovedCheckpoints int64  `json:"removed_checkpoints"`
	RemovedSnapshots   int64  `json:"removed_snapshots"`
	RemovedUpdates     int64  `json:"removed_updates"`
	SourceVersion      int    `json:"source_version"`
	ValidationError    string `json:"validation_error"`
}

// RebuildDocument replaces a history ygo cannot load with one fresh update built from its latest
// saved markdown, or supplied markdown, inside the transaction the context's ledger joined. It is
// deliberately never a load fallback: a history that loads can contain unsettled edits newer than
// its latest version and is refused unchanged.
//
// Every refusal comes before the first write: a room resident in this server or in another
// envoy-dispatch process (a holds query, roomQueries.ask), a history that loads, a history another
// rebuild or an append moved past the one this rebuild loaded (RebuildTx), a closed issue (for
// supplied markdown, which changes the document), a latest version holding an element this build
// does not know (ErrDocNewerContent, as every read of it answers), and markdown that is not a
// Proof document, drops an open ask block, introduces a block pointer at a block the attribute
// may not name (refuseBlockPointers), or introduces a check block that breaks a rule of a check CI
// proves (refuseCICheckBlocks), both against the latest version. Supplied markdown is caller
// text: it is parsed as an upload is, and its rendering is weighed as a new document's is
// (SeedText). The latest version is the server's own rendering of the document, read back as
// every stored rendering is (pmdoc.ParseRendering), so a document whose version is past what one
// write may make - stored before that bound, or grown past it by browser edits - still rebuilds,
// and so does one holding an element this build reads but may not create yet, which the seed puts
// back rather than creates (encodeDocumentTreeHolding). Supplied markdown that differs from the
// latest version is a document change, so the same transaction writes its version, which moves
// the open approval request to it (writeVersionTx, MoveApprovalAsk); the VersionResult is what
// the caller's artifact.version event names. A rebuild of a closed issue's document from its
// latest version writes the close's ask states into the tree it seeds, as the close's final sweep
// writes them into the issue's other documents (closedIssueAskState), and marks the document
// owing the settlement those writes leave owed, so no ask block the close withdrew reads open
// again once the rebuild commits. A failure anywhere rolls the whole rebuild back with the
// transaction. The room refuses loads until that transaction ends
// (Ledger.holdRebuild): a load before the commit would read the old history, and a second rebuild
// would preflight against it. That is also why the close's states go into the seed rather than
// through a live write after it (WriteIssueAskStates): such a write's fork is refused while the
// rebuild holds the room, and would read the old history through the pool that owns loads.
func (s *Service) RebuildDocument(ctx context.Context, artifactID string, markdown *string, actor model.Actor) (RebuildReport, VersionResult, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return RebuildReport{}, VersionResult{}, errUnjoined
	}
	if _, loaded := s.rebuilding.LoadOrStore(artifactID, struct{}{}); loaded {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: document %s is already being rebuilt", ErrDocumentLive, artifactID)
	}
	ledgerFrom(ctx).holdRebuild(artifactID)
	// A room still loading is not resident yet. It reads the same history the check below reads,
	// so it can only fail on a history that cannot load, and its successor load is refused above.
	// A room stays resident for roomIdleTimeout after its last editor leaves, and ygo's idle
	// sweep that then evicts it runs every 30 seconds, so the refusal names two minutes. A load
	// that starts after this check reads the history this transaction has not yet replaced and
	// fails on it, since it cannot load, or, once the rebuild commits, reads the rebuilt head:
	// versions only increase, so its room holds the rebuilt update and none it would mistake for it.
	//
	// The other envoy-dispatch processes' rooms are asked the same through a holds query: a member
	// that answers it holds a room of the document refuses the rebuild as this server's room does.
	// A member that does not answer within the query's wait, or a room that loads there after it
	// answered, is covered by room sync instead. RebuildTx replaces the whole history with one
	// update at head+1 and announces it, so an instance over there finds its stored history
	// replaced (historyReplaced): its catch-up closes and reloads the room
	// (roomCatchUps.closeReplaced), and until then its settlement and its browser's append are
	// refused (ErrRoomBehindHead) and lead to the same close, and the reload reads the rebuilt
	// document.
	if s.srv.GetDoc(artifactID) != nil {
		return RebuildReport{}, VersionResult{}, liveRebuildRefusal(ctx, tx, artifactID, roomOpenHere, askAfterTwoMinutes)
	}
	answers, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryHolds)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: ask the other processes whether they hold the document: %v", ErrServiceUnavailable, err)
	}
	for _, answer := range answers {
		if answer.holds {
			return RebuildReport{}, VersionResult{}, liveRebuildRefusal(ctx, tx, artifactID, roomOpenElsewhere, askAfterTwoMinutes)
		}
	}
	// The head of the history the refusal below judges. RebuildTx replaces that history only
	// while the head is still this one, and Load answers no head when it fails, which is the
	// case this route exists for, so it is read here instead - as Load reads it, and before the
	// load, so an append that lands between the two leaves the head behind the history judged
	// and the rebuild refused rather than let through over a history nothing judged.
	loadedHead, err := s.persistence.Head(ctx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: read the document's head before rebuild: %v", ErrServiceUnavailable, err)
	}
	// The pre-load is only how a history that loads, which this refuses, is told from one that
	// does not: the rebuild's source is the latest saved version or the supplied markdown, never
	// the stored updates. So a load that fails because the history does not load is that same
	// answer, and the rebuild goes on, as it does for a history that loads into nothing a reader
	// can use. That is every ErrDocumentUnloadable: a state that does not decode, and one ygo
	// refuses from its clocks, which will not even merge, so Load itself fails rather than
	// returning an update validateUpdate can refuse (mergedThrough). The load error is then what
	// the report names as the history's fault. Any other load failure is the store's, not the
	// document's, and the caller may retry past it.
	var validationErr error
	loaded, err := s.persistence.Load(ctx, artifactID)
	switch {
	case errors.Is(err, ErrDocumentUnloadable):
		validationErr = err
	case err != nil:
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	default:
		validationErr = validateUpdate(loaded.Update, s.maxPendingItems)
		loadedHead = loaded.Version
	}
	if validationErr == nil {
		latest, err := savedVersionNumber(ctx, tx, artifactID)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		return RebuildReport{}, VersionResult{}, fmt.Errorf("%w (document %s, latest saved version %d)", ErrDocumentLoads, artifactID, latest)
	}
	// The owner row before the room lock the write below takes, as every writer that takes both
	// does (lockDocumentRoom). Held, it also keeps the open asks the replacement must keep, and
	// the approval request the version moves, from changing under this transaction.
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	latest, err := latestVersion(ctx, tx, artifactID)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// stored is the latest version's own tree, as the version holds it, when the rebuild seeds from
	// that version: the seed puts back what the version holds, so an element this build reads and
	// may not create yet is held already, not created (pmdoc.UpdateHolding). It is nil for supplied
	// markdown, which creates every element it holds.
	var tree, stored *pmdoc.Node
	if markdown == nil {
		// The version's markdown is the server's own, not the caller's, so an element this build
		// does not know in it is newer content, as every read of the document answers it, not
		// invalid markdown.
		parsed, parseErr := pmdoc.ParseRendering(latest.markdown)
		if newer := documentSchemaError(parseErr); errors.Is(newer, ErrDocNewerContent) {
			return RebuildReport{}, VersionResult{}, newer
		}
		// uploadedInput copies parsed, so the close's states written into tree below leave
		// stored as the version holds it.
		tree, err = uploadedInput(parsed, parseErr)
		stored = parsed
	} else {
		if !open {
			return RebuildReport{}, VersionResult{}, ErrIssueClosed
		}
		tree, err = parseInput(*markdown)
	}
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// A caller's markdown is markdown written in whole, so an ask written with its options and no
	// question is refused (validateWrittenAskBlocks), as the markdown of such an ask was before
	// both parsers read the empty paragraph the schema fills in ahead of its options. The latest
	// version's own markdown is held to the tree rule instead (validateStoredAskBlocks): a
	// person's edit can empty an ask's question in the live document, which this package keeps
	// and repairs through that question (refuseEmptyQuestionEdit), and a document the product
	// stores must be one a rebuild can restore - refusing it here would leave it with no way
	// back. Both doors hold every ask to the wording bound it shares.
	content := validateWrittenAskBlocks
	if markdown == nil {
		content = validateStoredAskBlocks
	}
	if err := content(tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	if err := s.refuseDroppedAskBlocks(ctx, artifactID, tree); err != nil {
		return RebuildReport{}, VersionResult{}, &ErrInvalidAskBlock{Reason: err}
	}
	// A closed issue's document shows its asks as the close left them, and the tree read from the
	// latest version need not: settlement, which versions a document, runs on no closed issue, so
	// that version can predate the close, and the close's live writes to the document either never
	// reached it, its history not loading then, or are in the history this replaces. So the
	// close's final sweep is written into the tree here (closedIssueAskState): every resolved ask's
	// block and copy, and every answered ask's copy, from the rows as they stand under the issue's
	// row this transaction holds, and every readable ask block with no ask row marked resolved. The
	// ask rows are taken before RebuildTx takes the document's advisory lock, the order the close's
	// own write takes them in (WriteIssueAskStates). Once this transaction commits, the rebuilt
	// document reads as the issue's other documents do.
	var closedStates *issueAskStateWrites
	if !open {
		closedStates, err = readIssueAskStateWrites(ctx, tx, artifactID, owner, tree, closedIssueAskState, true)
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if !closedStates.apply(tree) {
			closedStates = nil
		}
	}
	canonical, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if markdown != nil {
		if err := weighRendering("", canonical); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		// Supplied markdown is a caller's write, so a block pointer or a check block it introduces
		// over the latest version is checked. That version is read back as the tree before the
		// write; one that does not read back holds no pointer and no check block.
		var prior *pmdoc.Node
		if latestTree, err := pmdoc.ParseRendering(latest.markdown); err == nil {
			prior = latestTree
		}
		if err := refuseBlockPointers(ctx, tx, artifactID, tree, prior, offsets); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if err := refuseCICheckBlocks(ctx, tx, artifactID, tree, prior); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
	}
	seed, err := encodeDocumentTreeHolding(tree, stored)
	if err != nil {
		return RebuildReport{}, VersionResult{}, fmt.Errorf("seed rebuilt document tree: %w", err)
	}

	// RebuildTx replaces the history only if its head is still the one loaded above, which is the
	// history validated as one that cannot load: a second rebuild of the document on another
	// process that loaded the same history waits on the owner row above until this one commits,
	// then finds the head moved and is refused.
	report, err := s.persistence.RebuildTx(ctx, tx, artifactID, loadedHead, seed)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = latest.Number
	report.ValidationError = validationErr.Error()
	if closedStates != nil {
		// markdown is nil here: a closed issue refuses supplied markdown above. The seed differs
		// from the latest version by the close's states, as each of the issue's documents the
		// close wrote differs from its own, so it owes the settlement those writes leave owed
		// (updateOwedStatement): none runs while the issue is closed, and the one its reopen runs
		// versions the document and indexes, as new open asks, the blocks marked resolved here
		// with no ask row. RebuildTx holds the document's advisory lock, which the mark is ordered
		// by. The writes credit no one, as the close's do (the zero owedUpdates): no pending author
		// is added, and the latest edit source the row names is kept.
		owed, err := updateOwedStatement(artifactID, owedUpdates{})
		if err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		if err := owed.exec(ctx, tx); err != nil {
			return RebuildReport{}, VersionResult{}, err
		}
		logUnindexedAskBlocks(*owner.IssueKey, closedStates.marked)
	}
	if markdown == nil || canonical == latest.markdown {
		return report, VersionResult{Version: latest.Version}, nil
	}
	// RebuildTx holds the document's advisory lock until this transaction ends, so the pending
	// authors read here are the ones the version's commit deletes.
	capture, err := s.capturePendingAuthors(ctx, tx, artifactID, nil, &actor, everyCredit)
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	written, err := s.writeVersionTx(ctx, tx, artifactID, renderedTree{tree: tree, markdown: canonical, offsets: offsets}, actor, &versionWrite{
		authors: actorSlice(capture.authors),
		capture: &capture,
	})
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	// A rebuild replaces the document's content with markdown its own actor wrote, so that actor
	// made the document's latest change, whatever edit the broken history still had pending. It is
	// recorded as the latest edit source for the versions that follow, and names this version's
	// edit notice (recordEditNotice), which goes when that actor is a person.
	if err := recordLatestEditSource(ctx, tx, artifactID, &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := s.recordEditNotice(ctx, tx, artifactID, written.version.Number, actorSlice(capture.authors), &actor); err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	report.SourceVersion = written.version.Number
	return report, VersionResult{Version: written.version, Wrote: true, Changes: written.changes, Blocks: written.blocks}, nil
}

// ErasedLiveText is what `envoy-dispatch erase` (grapherase.Erase) leaves of the live state of a
// document whose text, or whose stored history, holds the bytes it erases: the live text with each
// erased text replaced by the erase's marker, seeded as the document's one stored update in place
// of every update it holds, through the statements a rebuild writes (RebuildTx). The rows are
// replaced rather than edited because a stored update is ygo's encoding of the edits: a deleted
// item keeps its bytes there until a fold collects them, and a text substitution inside the
// encoding would leave an update no load can decode.
//
// It is read first (ReadErasedLiveText) and written second (Replace), both in the erase's own
// transaction, so the erase lists what it would do before any of it is done, and a failure
// anywhere rolls back the whole erase.
type ErasedLiveText struct {
	// Held is whether the live text holds one of the erased texts. A document that holds none -
	// one a version of which held the text, edited since - keeps its text, and its history, which
	// can still hold the bytes in the items that edit deleted, is replaced all the same.
	Held bool
	// Left is whether the seed would still hold the bytes of an erased text: the live text holds
	// them where its markdown spells them otherwise than the erased text is written - an escaped
	// `*` or `_`, for one - so the replacement, which reads the markdown byte for byte, found no
	// place for the marker there. The erase refuses such a document rather than report success
	// over the bytes it would leave (grapherase.Erase).
	Left bool
	// Updates is how many stored updates the history holds, every one of which the seed replaces.
	Updates int64
	// AnchorMarks is how many comment, suggestion and ask anchors the live text carries, and
	// MarginRecords how many records its margin holds. The seed is parsed from markdown, which
	// carries neither, so both go with the history, as they go with RebuildDocument's.
	AnchorMarks, MarginRecords int
	// Text is the live text before the erase, whose stored display tree goes with it.
	Text string

	room string
	head persistence.Version
	seed []byte
}

// ReadErasedLiveText reads, inside tx, the live state of the document artifactID names as the
// erase would leave it, with each of texts replaced by marker. It answers nil for a document with
// no stored history, which holds no live text. It writes nothing.
//
// It takes the document's owner row and then its advisory lock, the order every writer of a
// document takes them in (lockArtifactOwner, lockDocumentRoom), and holds both until tx ends, so
// the history it reads is the one Replace replaces and no edit lands between the two.
//
// The text is the server's own rendering of the live tree with each erased text replaced, parsed
// back as a write over that tree is parsed (pmdoc.ParseWritten), so every block the replacement
// leaves unchanged keeps its id. A history that does not load, a tree outside the schema, and a
// replacement that breaks an ask's content rule or drops an open ask block each refuse the erase,
// naming the document: a document the erase could not leave whole is one an operator repairs
// first, by its rebuild or a replacement from markdown.
func ReadErasedLiveText(ctx context.Context, tx pgx.Tx, artifactID string, texts []string, marker string) (*ErasedLiveText, error) {
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		return nil, err
	}
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		return nil, err
	}
	erased := &ErasedLiveText{room: artifactID}
	head, err := erased.versioned().head(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, nil
	}
	erased.head = head
	if err := tx.QueryRow(ctx, `select count(*) from doc_updates where artifact_id = $1 and version <= $2`,
		artifactID, int64(head)).Scan(&erased.Updates); err != nil {
		return nil, fmt.Errorf("count the stored updates of document %s: %w", artifactID, err)
	}
	doc, err := documentThrough(artifactID, head, storedUpdates(ctx, tx, artifactID, head), maxUpdateItems)
	if err != nil {
		return nil, fmt.Errorf("load document %s: %w", artifactID, err)
	}
	if doc == nil {
		return nil, nil
	}
	tree, err := treeOf(doc)
	if err != nil {
		return nil, fmt.Errorf("read document %s: %w", artifactID, err)
	}
	markdown, err := documentMarkdown(tree)
	if err != nil {
		return nil, fmt.Errorf("render document %s: %w", artifactID, err)
	}
	redacted := markdown
	for _, text := range texts {
		redacted = strings.ReplaceAll(redacted, text, marker)
	}
	target, _, err := pmdoc.ParseWritten(redacted, tree)
	if err != nil {
		return nil, fmt.Errorf("read back the redacted text of document %s: %w", artifactID, err)
	}
	if err := validateStoredAskBlocks(target); err != nil {
		return nil, fmt.Errorf("the redacted text of document %s: %w", artifactID, &ErrInvalidAskBlock{Reason: err})
	}
	if err := refuseDroppedAskBlocksIn(ctx, tx, artifactID, target); err != nil {
		return nil, fmt.Errorf("the redacted text of document %s: %w", artifactID, &ErrInvalidAskBlock{Reason: err})
	}
	if erased.seed, err = encodeDocumentTree(target); err != nil {
		return nil, fmt.Errorf("seed the redacted document %s: %w", artifactID, err)
	}
	erased.Held = redacted != markdown
	// The seed is read for each erased text's own bytes, which is what a search of doc_updates
	// finds once it is stored. A text the marker itself holds cannot be told from the marker
	// there, and is left to the replacement above.
	for _, text := range texts {
		if !strings.Contains(marker, text) && bytes.Contains(erased.seed, []byte(text)) {
			erased.Left = true
		}
	}
	erased.AnchorMarks = len(pmdoc.ListMarks(tree))
	erased.MarginRecords = len(doc.GetMap(marksMapName).Keys())
	erased.Text = markdown
	return erased, nil
}

// Replace writes the seed ReadErasedLiveText made in place of the history it read, inside tx,
// through RebuildTx: every stored update, checkpoint and snapshot of the document goes, the seed is
// written at the version after the head, and the change is announced, so a room of the document
// that any server process holds finds its history replaced and is closed and reloaded at the seed
// (historyReplaced). That room's unstored updates are refused from then on, as they are after a
// rebuild, which is what an editor open on the document loses.
func (e *ErasedLiveText) Replace(ctx context.Context, tx pgx.Tx) (RebuildReport, error) {
	return e.versioned().RebuildTx(ctx, tx, e.room, e.head, e.seed)
}

// versioned is the versioned store the erase reads and replaces a history through. It holds no
// store: the erase runs in a process of its own over one transaction, and head and RebuildTx read
// and write the transaction they are handed and nothing else. Its process id is empty, which is
// how a store built outside a server announces a change (PgVersioned.processID): every server
// process's room sync takes it for another process's, which is what it is.
func (e *ErasedLiveText) versioned() *PgVersioned {
	return NewPgVersioned(nil)
}

// The causes and next steps of the rebuild refusals liveRebuildRefusal builds.
const (
	roomOpenHere       = "is open in this Dispatch process, or its last editor left it less than two minutes ago"
	roomOpenElsewhere  = "is open in another Dispatch process, or its last editor left it less than two minutes ago"
	askAfterTwoMinutes = "Stop. Ask a person whether to rebuild the document after two minutes"
	readAgainFirst     = "Another rebuild may already have repaired the document: read it again, and ask a person whether to rebuild it only if it still does not load"
)

// liveRebuildRefusal is every ErrDocumentLive refusal of a rebuild that names what a rebuild would
// discard: why Dispatch refused (cause), the latest saved version a rebuild from it would go back
// to, read on tx, and what the caller does next (next). One builder, so no refusal keeps advice
// another has dropped. The one ErrDocumentLive refusal it does not build is "already being
// rebuilt" (RebuildDocument), which says only that a rebuild is running and offers no repair.
func liveRebuildRefusal(ctx context.Context, tx pgx.Tx, artifactID, cause, next string) error {
	latest, err := savedVersionNumber(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: document %s %s, so Dispatch refused the rebuild. A rebuild from the latest saved version, %d, discards every edit made after it. %s",
		ErrDocumentLive, artifactID, cause, latest, next)
}

// savedVersionNumber is the number of artifactID's latest saved version, which a refused rebuild
// names: a rebuild discards every edit made after it.
func savedVersionNumber(ctx context.Context, tx pgx.Tx, artifactID string) (int, error) {
	var number int
	if err := tx.QueryRow(ctx, `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&number); err != nil {
		return 0, fmt.Errorf("read latest document version number: %w", err)
	}
	return number, nil
}
