package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// MarkKind identifies the Proof or Dispatch mark type written into a document tree.
type MarkKind string

const (
	MarkAsk        MarkKind = "dispatchAsk"
	MarkComment    MarkKind = "proofComment"
	MarkSuggestion MarkKind = "proofSuggestion"
)

type MarkSpec struct {
	Kind  MarkKind
	ID    string
	By    model.Actor
	Attrs pmdoc.Attrs
}

// MarkReply is a reply projected to Proof's stored-mark format.
type MarkReply struct {
	By   string `json:"by"`
	Text string `json:"text"`
	At   string `json:"at"`
}

// MarkRecord is the server-owned projection of a comment or suggestion for Proof.
type MarkRecord struct {
	Kind      string      `json:"kind"`
	By        string      `json:"by"`
	CreatedAt string      `json:"createdAt"`
	Text      string      `json:"text"`
	Resolved  bool        `json:"resolved"`
	Replies   []MarkReply `json:"replies"`
	Content   string      `json:"content,omitempty"`
	Status    string      `json:"status,omitempty"`
}

var (
	// ErrAnchorMissing reports a mark that did not appear before verification timed out.
	ErrAnchorMissing = errors.New("anchor mark is not in the document")
	// ErrAnchorOrphaned reports a suggestion mark that no longer exists.
	ErrAnchorOrphaned = errors.New("anchor mark no longer exists")
	// ErrRetiredDocumentKeepsText answers RejectDeletedCommentSuggestion for a suggestion in a
	// retired document (RetireDocument): a retired document takes no new text, so the suggestion
	// can never apply there, and its mark, an insert's words with it, stays in the frozen text.
	// The reject writes nothing.
	ErrRetiredDocumentKeepsText = errors.New("a retired document keeps the suggestion in its text")
)

type skippedAnchorRefreshKey struct{}

// ActorRef returns the Proof actor reference for an actor.
func ActorRef(actor model.Actor) string {
	return actor.Kind + ":" + actor.ID
}

func (m MarkSpec) pmMark() pmdoc.Mark {
	if m.Attrs != nil {
		return pmdoc.Mark{Type: string(m.Kind), Attrs: m.Attrs}
	}
	attrs := pmdoc.Attrs{"id": m.ID, "by": ActorRef(m.By)}
	if m.Kind == MarkSuggestion {
		attrs["kind"] = "replace"
	}
	return pmdoc.Mark{Type: string(m.Kind), Attrs: attrs}
}

// Anchored is what an inline mark anchors to: the text it covers and the stable block that
// contains its full range — empty when the range spans top-level siblings, which have only
// the document root in common.
type Anchored struct {
	Quote   string
	BlockID string
}

// QuoteScope narrows where MarkQuote finds its quote and which match it takes: Occurrence, the
// zero-based match, and BlockID, where it is set, the one block of the document the quote is found
// inside, Occurrence then counting that block's matches alone (pmdoc.QuoteFinder.FindInBlock). A
// nil scope is the whole document and its one match.
type QuoteScope struct {
	Occurrence *int
	BlockID    string
}

// MarkQuote marks one matching quote, as scope narrows it, and returns what the new mark anchors
// to. A comment mark made under WithCommentMarks is written on a closed issue's document too; a
// suggestion's is not. A document of an issue room is marked in its own fragment of the room
// (targetOf), and scope's block is one of that fragment's.
func (s *Service) MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, scope *QuoteScope) (Anchored, error) {
	if mark.Kind == MarkComment {
		ctx = commentMarkOperation(ctx)
	}
	err := s.applyLive(ctx, artifactID, mark.By, func(doc liveDoc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		fragment := doc.documentFragment()
		valid, err := startTree(current, doc)
		if err != nil {
			return err
		}
		tree := valid.Node()

		var markErr error
		transact(func(txn *crdt.Transaction) {
			_, markErr = markQuoteInTxn(txn, fragment, tree, quote, scope, mark)
		})
		return markErr
	})
	if err != nil {
		return Anchored{}, err
	}
	// applyLive keeps the tree it read from the fork after this callback as the write's rendering,
	// so the anchor is read off it rather than off a second read of the fork.
	anchored, err := readAnchored(ledgerFrom(ctx).writtenRendering(artifactID).tree, mark.Kind, mark.ID)
	if errors.Is(err, ErrAnchorMissing) {
		return Anchored{}, fmt.Errorf("%w: written mark %q is missing", ErrDocSchema, mark.ID)
	}
	if err != nil {
		return Anchored{}, err
	}
	return anchored, nil
}

// readAnchored reads a persisted mark's covered text and containing block from tree;
// ErrAnchorMissing when the mark is not there.
func readAnchored(tree *pmdoc.Node, kind MarkKind, id string) (Anchored, error) {
	r, quote, found := pmdoc.FindMark(tree, string(kind), id)
	if !found {
		return Anchored{}, ErrAnchorMissing
	}
	blockID, err := pmdoc.BlockIDForRange(tree, r)
	if err != nil {
		return Anchored{}, err
	}
	return Anchored{Quote: quote, BlockID: blockID}, nil
}

// markQuoteInTxn finds quote in doc, as scope narrows it (findQuoteIn), and writes the supplied
// mark within txn.
func markQuoteInTxn(txn *crdt.Transaction, fragment *crdt.YXmlFragment, doc *pmdoc.Node, quote string, scope *QuoteScope, spec MarkSpec) (pmdoc.Range, error) {
	range_, err := findQuoteIn(doc, quote, scope)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if err := pmdoc.MarkRange(txn, fragment, range_, spec.pmMark()); err != nil {
		return pmdoc.Range{}, documentSchemaError(err)
	}
	return range_, nil
}

// findQuoteIn is the range of quote in doc that scope names: inside scope's block alone where it
// names one, else the whole document's match scope's occurrence names, or its one match.
func findQuoteIn(doc *pmdoc.Node, quote string, scope *QuoteScope) (pmdoc.Range, error) {
	finder := pmdoc.NewQuoteFinder(doc)
	switch {
	case scope == nil:
		return finder.Find(quote, nil, nil)
	case scope.BlockID != "":
		return finder.FindInBlock(quote, scope.BlockID, scope.Occurrence)
	default:
		return finder.Find(quote, scope.Occurrence, nil)
	}
}

// VerifyMark returns what a browser-written mark anchors to, now or after its next document
// update. It subscribes to the room's updates inside the Apply that loads and holds the room: a
// room looked up again once that Apply returned can have been evicted in between. The wait ends
// with one last read rather than on the subscription alone, so a mark written into a room
// instance that replaced the subscribed one - which another process's eviction can leave behind -
// is still found. Each look reads docTree, which is never older than the stored head: a mark a
// browser on another process placed and that process stored is found at once, from the store
// while this room's catch-up has not applied it. One stored after a look wakes the next look
// through the room's update, a catch-up batch (roomCatchUps) when another process stored it, or a
// browser's update on this process. A comment mark read under WithCommentMarks is read on a
// closed issue's document too.
//
// On a closed issue each look reads the stored document instead (closedIssueCommentAnchor): the
// same tree the comment's anchor is pinned a version from (captureLiveTextAndAuthors) and
// written on (readFork). The usual mark_id is one the browser wrote into the room itself, and on
// a process that has not heard the close that browser is still writable, so its mark can be in
// the room while the store will never take it - the gate refuses the update that carries it
// (gateAppend). Verified from the room, such a comment was answered 201 with an anchor the next
// refresh orphaned at once. Read from the store, a mark the store does not hold is
// ErrAnchorMissing, which the browser's margin recovers from (closedIssueCommentAnchor says
// how). A mark whose append is still in flight - one the room took inside closeMargin, which the
// gate admits - is found by the same wait as any other, from the store it lands in; the version
// pinned after it holds it, since this read comes first and the store only grows.
//
// A document of an issue room is watched through its room and read from its own fragment of it
// (targetOf).
func (s *Service) VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (Anchored, error) {
	if kind == MarkComment {
		ctx = commentMarkOperation(ctx)
	}
	fromStore, err := s.closedIssueCommentAnchor(ctx, s.queryFrom(ctx), artifactID)
	if err != nil {
		return Anchored{}, err
	}
	target, err := s.targetOf(ctx, artifactID)
	if err != nil {
		return Anchored{}, err
	}
	updates := make(chan struct{}, 1)
	var unsubscribe func()
	err = s.srv.Apply(ctx, target.room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe = doc.OnUpdate(func(_ []byte, _ any) {
			select {
			case updates <- struct{}{}:
			default:
			}
		})
	})
	if unsubscribe != nil {
		defer unsubscribe()
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return Anchored{}, err
	}
	// The room can be evicted from here on: no close retires a room while an Apply is inside it.
	if s.afterReadWarm != nil {
		s.afterReadWarm(target.room)
	}

	timer := time.NewTimer(s.markWait)
	defer timer.Stop()
	final := false
	for {
		tree, err := s.anchorTree(ctx, target, fromStore)
		if err != nil {
			return Anchored{}, err
		}
		anchored, err := readAnchored(tree, kind, id)
		if err == nil {
			return anchored, nil
		}
		if !errors.Is(err, ErrAnchorMissing) {
			return Anchored{}, err
		}
		if final {
			return Anchored{}, ErrAnchorMissing
		}

		select {
		case <-updates:
		case <-timer.C:
			final = true
		case <-ctx.Done():
			return Anchored{}, ctx.Err()
		}
	}
}

// anchorTree is the document a mark is verified against, the target's own fragment of its room:
// the stored document for a comment's anchor on a closed issue (VerifyMark says why), the live one
// every other read sees (docTree).
func (s *Service) anchorTree(ctx context.Context, target liveTarget, fromStore bool) (*pmdoc.Node, error) {
	if fromStore {
		return s.storedTree(ctx, target)
	}
	return s.docTree(ctx, target.document)
}

// BlockForQuote returns the stable block that contains the one matching quote.
// A quote spanning top-level siblings has no block identity.
func (s *Service) BlockForQuote(ctx context.Context, artifactID, quote string) (string, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return "", err
	}
	r, err := pmdoc.FindQuote(tree, quote, nil, nil)
	if err != nil {
		return "", err
	}
	return pmdoc.BlockIDForRange(tree, r)
}

// SuggestionKind returns the kind recorded on a verified browser or server suggestion mark. A
// projection reads it under WithCommentMarks on a closed issue too: of its thread root, where
// a reply projects, and of the comment's own mark, where its author's edit does.
func (s *Service) SuggestionKind(ctx context.Context, artifactID, id string) (string, error) {
	tree, err := s.docTree(commentMarkOperation(ctx), artifactID)
	if err != nil {
		return "", err
	}
	attrs, found := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
	if !found {
		return "", ErrAnchorMissing
	}
	return suggestionKind(attrs, id)
}

// AcceptSuggestion applies the replacement for a suggestion mark.
func (s *Service) AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error {
	_, err := s.applySuggestion(context.WithValue(ctx, skippedAnchorRefreshKey{}, id), artifactID, id, replaceWith, actor, true)
	return err
}

// RejectSuggestion removes a suggestion mark and its inserted text when necessary.
func (s *Service) RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error {
	_, err := s.applySuggestion(context.WithValue(ctx, skippedAnchorRefreshKey{}, id), artifactID, id, "", actor, false)
	return err
}

// RejectDeletedCommentSuggestion rejects the pending suggestion of a comment its author is
// deleting: the mark goes and an insert suggestion's text goes with it, as RejectSuggestion
// does. Its caller's joined transaction holds the document owner's lock and has decided under that
// lock that the comment is its author's to delete - the same standing a comment's own anchor
// writes have - and the caller marks the context it passes here as theirs (WithCommentMarks). So
// this reject runs as those writes run, owner-verified under the caller's mark
// (commentMarkOperation), and a closed issue's document takes it: a closed issue still takes the
// delete of its comment, and the suggestion's replacement text is the comment's text too, so it
// leaves the document with the body. Owner-verified, the write meets no closed refusal
// (allowInject); a comment's mark write, it forks the stored document cold (closedIssueCommentMark,
// readFork says why). RejectSuggestion is refused on a closed issue as it always was, and so is
// this reject under a context no caller marked.
//
// It answers the kind the suggestion's mark recorded: "insert", "replace" or "delete"
// (suggestionKind). Only an insert's mark wraps text the suggestion itself put in the document,
// which this reject takes out again (rejectedInsert); a replace's and a delete's wrap document
// text, which an accept would replace and a reject leaves. So only an insert's anchor quote, the
// text its mark wrapped, is a copy of the comment's own text.
//
// A retired document (RetireDocument), read under its owner's lock as ProjectMark reads it
// (retiredUnderOwnerLock), takes no new text, so its suggestion can never apply and stays in the
// frozen text: the reject writes nothing and answers ErrRetiredDocumentKeepsText. A transaction
// that holds its write to the document already read the document standing under that lock
// (joinLiveWrite), so it reads nothing again.
func (s *Service) RejectDeletedCommentSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) (string, error) {
	retired, err := retiredOutsideLiveWrite(ctx, artifactID)
	if err != nil {
		return "", err
	}
	if retired {
		return "", ErrRetiredDocumentKeepsText
	}
	return s.applySuggestion(context.WithValue(commentMarkOperation(ctx), skippedAnchorRefreshKey{}, id), artifactID, id, "", actor, false)
}

// applySuggestion accepts or rejects the suggestion mark id, and answers the kind that mark
// recorded (suggestionKind).
func (s *Service) applySuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor, accept bool) (string, error) {
	var kind string
	err := s.applyLive(ctx, artifactID, actor, func(doc liveDoc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		fragment := doc.documentFragment()
		// Read before the Yjs transaction opens: the state vector takes the document lock.
		since := authoredClock(ctx, artifactID, doc.Doc)
		valid, err := startTree(current, doc)
		if err != nil {
			return err
		}
		tree := valid.Node()
		range_, _, ok := pmdoc.FindMark(tree, string(MarkSuggestion), id)
		if !ok {
			return ErrAnchorOrphaned
		}
		attrs, ok := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
		if !ok {
			return fmt.Errorf("%w: suggestion mark %q has no attributes", ErrDocSchema, id)
		}
		kind, err = suggestionKind(attrs, id)
		if err != nil {
			return err
		}

		splice := (accept && (kind == "replace" || kind == "delete")) || (!accept && kind == "insert")
		if !splice {
			var unmarkErr error
			transact(func(txn *crdt.Transaction) {
				unmarkErr = documentSchemaError(pmdoc.Unmark(txn, fragment, string(MarkSuggestion), id))
			})
			if unmarkErr != nil {
				return unmarkErr
			}
			return nil
		}

		// An accept is one implicit operation, so every run its own update inserts is its own and
		// no tree diff tells them apart: recordInsertedText reads the blocks it inserted into and
		// this hands them straight back (editBatch.writes, LEGION-269). It stamps no block ids -
		// a replacement's blocks are minted by pmdoc.Parse already, and EnsureBlockIDs would also
		// repair a repeat the live document carries, which is settlement's to repair and a
		// write's to leave as it found it (TestWritesBesideALiveRepeatedBlockIDAreTaken). Text
		// left in a block with no id at all - the far half of a split, which no caller can
		// address either - is simply not tracked, so the check under-reports there rather than
		// naming a block nobody can name.
		write := func(next *pmdoc.Node) error {
			written := editBatch{tree: next, operations: 1}
			return recordInsertedText(ctx, artifactID, id, fragment, doc.fragment, since, written.writes, func() error {
				var updateErr error
				transact(func(txn *crdt.Transaction) {
					updateErr = pmdoc.Update(txn, fragment, next)
				})
				return updateErr
			})
		}
		// A reject is not checked: it removes the text a browser insert added, which gives back
		// the document the insert started from.
		if !accept {
			next, err := rejectedInsert(tree, id)
			if err != nil {
				return err
			}
			return write(next)
		}

		// A suggestion's text reaches the document with line feeds alone (pmdoc.LineFeeds). It is
		// stored so when the suggestion is created, but one created before that holds its text as
		// sent.
		with := pmdoc.LineFeeds(replaceWith)
		// The replacement's markdown and the tables its splice cuts are this accept's to pad.
		budget := pmdoc.NewWriteBudget()
		at, _ := pmdoc.ContainingTextblock(tree, range_.From)
		code := at.Node.Type == "code_block"
		var replacement *pmdoc.Node
		if code {
			replacement = codeReplacement(acceptedCode(tree, with, at, range_))
		} else if replacement, err = inlineAware(with, edgesOf(at, range_), opensDocument(tree, range_.From), budget); err != nil {
			return err
		}
		// Accepted text stays inside every ask and comment anchor the suggestion lay wholly inside,
		// as an edit's replacement does, and the suggestion's own mark goes with the text it
		// replaced. Only text that stays in the textblock carries them: a block replacement can
		// land a code block, which an ask's mark cannot cover.
		inline := code || isInlineDocument(replacement)
		if inline {
			pmdoc.AddMarks(replacement, slices.DeleteFunc(pmdoc.AnchorMarksCovering(tree, range_), func(mark pmdoc.Mark) bool {
				return mark.Type == string(MarkSuggestion) && mark.Attrs["id"] == id
			}))
		}
		next, err := valid.Splice(range_, replacement)
		if err != nil {
			return acceptSpliceRefusal(err)
		}
		// Where the suggestion lies in the document the accept started from, which every refusal
		// below reads against the tree it holds then (matchedBlocks).
		found, err := matchedBlocks(valid, range_)
		if err != nil {
			return err
		}
		// The browser editor's table plugin pads a table the accept cut to its width, as it does
		// after a reject, before anything reads the accept back, where the browser's accept writes
		// the replacement where Splice does (padsLikeTheBrowser).
		if padsLikeTheBrowser(tree, range_, at, inline) {
			if next, err = padCutTables(next, found, budget); err != nil {
				return acceptSpliceRefusal(err)
			}
		}
		// The checks below read the blocks this accept changed, several of them the same blocks:
		// each block is rendered and parsed once for all of them (blockReadBacks). A block the
		// settlement rewrites (settleAccepted) is another node, and is read back again.
		backs := blockReadBacks{}
		if code {
			if err := refuseAcceptedCodeThatReshapes(valid, next, found, at, with, backs); err != nil {
				return err
			}
		}
		next = settleAccepted(valid, next, found, with)
		if err := refuseCodePipeInTableCell(valid, next, found, "replace_with", "replace_with", with, "reject the suggestion, or reply asking for text the cell can hold"); err != nil {
			return err
		}
		if !insideAsk(at) {
			if err := refuseUnreadableAccept(valid, next, found, range_, at, with, replacement, backs); err != nil {
				return err
			}
			if err := refuseReshapedAccept(valid, next, found, range_, at, with, replacement, backs); err != nil {
				return err
			}
		}
		if err := pmdoc.RepeatedBlockID(tree, next, replacement); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidMarkdown, err)
		}
		if err := refuseBrokenAsks(tree, next); err != nil {
			return err
		}
		if err := refuseMisreadAccept(tree, next, with); err != nil {
			return err
		}
		return write(next)
	})
	if err != nil {
		return "", err
	}
	return kind, nil
}

// refuseBrokenAsks refuses the first ask a write left unreadable whose id the document could read
// before it, with settlement's reason (the one the edit route's ApplyOps gives), so a replacement
// an ask cannot hold, such as a question given a code block, writes nothing. An id the document
// already held unreadable, malformed or repeated, is not the write's to refuse: a browser edit can
// leave one and an upload can carry it on, settlement flags it, and a write elsewhere must not fail
// over it. An id that gains an ask is refused whatever it held, since settlement's id repair keeps
// the id for the first ask in document order and would hand the held ask's row and answer to it.
func refuseBrokenAsks(before, after *pmdoc.Node) error {
	unreadable, held := map[string]bool{}, map[string]int{}
	askReadability(before, func(id string, reason error) bool {
		unreadable[id] = unreadable[id] || reason != nil
		held[id]++
		return true
	})
	var refusal error
	written := map[string]int{}
	askReadability(after, func(id string, reason error) bool {
		written[id]++
		if reason != nil && (!unreadable[id] || written[id] > held[id]) {
			refusal = reason
		}
		return refusal == nil
	})
	if refusal != nil {
		return &ErrInvalidAskBlock{Reason: refusal}
	}
	return nil
}

func suggestionKind(attrs pmdoc.Attrs, id string) (string, error) {
	value, ok := attrs["kind"]
	if !ok {
		return "replace", nil
	}
	kind, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: suggestion mark %q kind is %T", ErrDocSchema, id, value)
	}
	switch kind {
	case "replace", "delete", "insert":
		return kind, nil
	default:
		return "", fmt.Errorf("%w: suggestion mark %q has kind %q", ErrDocSchema, id, kind)
	}
}

// ProjectMark writes a comment or suggestion record for Proof's margin projection. Under
// WithCommentMarks - a comment's own record, or its thread root's that a reply adds itself to -
// it is written on a closed issue's document too. A document that is retired (RetireDocument),
// read under its owner's lock (retiredUnderOwnerLock), is written nothing: a thread's resolve,
// reopen, edit and reply change the thread's own rows, and the retired document, which takes no new
// text, keeps the record it holds. A transaction that holds its write to the document's own room
// already read the document standing under that lock (joinLiveWrite, artifactOwner.Retired),
// which no retirement passes before the transaction ends, so it reads nothing again. A write to an
// issue room proves nothing of a document of it the transaction only resolved into that room
// (targetOf): the write serves every document of the room, and its owner read is the room's own
// artifact's, so such a document's retirement is read here, under the owner lock the write holds
// already.
func (s *Service) ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	if retired, err := retiredOutsideLiveWrite(ctx, artifactID); err != nil || retired {
		return err
	}
	plain, err := toPlain(record)
	if err != nil {
		return err
	}
	return s.applyMargin(commentMarkOperation(ctx), artifactID, actor, markID, plain)
}

func toPlain(record MarkRecord) (map[string]any, error) {
	if record.Replies == nil {
		record.Replies = []MarkReply{}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var plain map[string]any
	if err := json.Unmarshal(encoded, &plain); err != nil {
		return nil, err
	}
	return plain, nil
}

// anchorTarget is one table a document's anchored rows live in: its name, the SQL expression
// that reads an anchor's artifact_id as a uuid there, the anchor index that carries exactly
// that expression, and the predicate that holds one of its rows open. The expression differs
// per table because the index does: an ask's is store.CanonicalUUID's guarded cast, written out
// in migration 0148, a comment's the plain cast of comments_anchor_artifact (0032). The open
// predicate differs because the state does: an ask carries a state column, a comment a resolved
// flag.
type anchorTarget struct {
	table      string
	artifactID string
	index      string
	open       string
}

// asksAnchorTarget and commentsAnchorTarget are the two tables every anchor read of this package
// finds a document's rows in. Each table's facts are written once, here: the uuid expression,
// which must stay byte-identical to the one its index is built on, that index's name, which the
// plan tests hold each read to, and the predicate that holds a row of the table open. A read
// names its target instead of spelling any of them again.
var (
	asksAnchorTarget     = anchorTarget{table: "asks", artifactID: store.CanonicalUUID(`anchor->>'artifact_id'`), index: "asks_anchor_artifact_id", open: "state = 'open'"}
	commentsAnchorTarget = anchorTarget{table: "comments", artifactID: `(anchor->>'artifact_id')::uuid`, index: "comments_anchor_artifact", open: "not resolved"}

	// anchorTargets are both, in the order the reads that read both tables read them.
	anchorTargets = []anchorTarget{asksAnchorTarget, commentsAnchorTarget}
)

// anchoredOn is the predicate that finds the rows of the target's table anchored on the document
// bound to $1: `anchor is not null`, the text comparison the anchor reads of the release before
// migration 20261101182419 ran, and the uuid comparison this table's anchor index is built on,
// in that order.
//
// `anchor is not null` is the partial predicate of asks_anchor_artifact_id (0148), which Postgres
// cannot prove from a clause on anchor->>'artifact_id' alone, so without it that index is not
// eligible at all; ->> is strict, so a row either comparison matches carries a non-null anchor
// already and no read loses a row to it.
//
// The text comparison keeps the row set exactly the one that release returned, and it is what
// docs/write_hold_test.go's counter sees a refresh pass by, so a read that drops it both widens
// its answer and goes uncounted. A row whose artifact_id is not a lower-case document id - an
// upper-case spelling of a real id, or, on asks, a value that is no uuid at all - is left out of
// asks_anchor_artifact_id by migration 0148's guard and was never matched by the text comparison
// either, while the uuid comparison alone would return the upper-case one. On comments the uuid
// side is comments_anchor_artifact's plain (anchor->>'artifact_id')::uuid (0032), which raises
// 22P02 on an artifact_id that is no uuid where the text comparison returns no row; only that
// index's own cast keeps such a row out of comments today, and the text comparison is what holds
// the read to that release's answer whatever the table carries.
//
// The uuid comparison is written out as its index's expression, byte for byte, so the read is an
// index scan rather than a scan of a table holding every ask or comment of the instance. Every
// read that binds $1 is passed a document id the server holds, never a caller's text, so the cast
// of the parameter raises on no input.
func (t anchorTarget) anchoredOn() string {
	return `anchor is not null and anchor->>'artifact_id' = $1 and ` + t.artifactID + ` = $1::uuid`
}

// recordedMarksQuery is the statement that lists the anchor and mark type of every row of
// target's table whose anchor names the artifact $1.
func recordedMarksQuery(target anchorTarget) string {
	return fmt.Sprintf(`
		select anchor, %s
		from %s
		where %s
	`, commentMarkType(target.table), target.table, target.anchoredOn())
}

// recordedMarkRefs is every mark an ask or comment row anchored on one of documents records, read
// by the anchor's artifact alone whatever an ask's block_artifact_id holds: an ask that also names
// an ask block on another document keeps its mark on its anchor's document, as refreshAnchors and
// the other anchor readers read it. A sweep of an issue room passes every document of the room
// (sweepUnrecordedMarks): a row records a mark against the document whose fragment holds it.
func (s *Service) recordedMarkRefs(ctx context.Context, documents ...string) (map[pmdoc.MarkRef]struct{}, error) {
	recorded := make(map[pmdoc.MarkRef]struct{})
	for _, document := range documents {
		for _, target := range anchorTargets {
			if err := s.readRecordedMarks(ctx, target, document, recorded); err != nil {
				return nil, err
			}
		}
	}
	return recorded, nil
}

// readRecordedMarks adds to recorded every mark a row of target's table anchored on document
// records (recordedMarksQuery).
func (s *Service) readRecordedMarks(ctx context.Context, target anchorTarget, document string, recorded map[pmdoc.MarkRef]struct{}) error {
	rows, err := s.queryFrom(ctx).Query(ctx, recordedMarksQuery(target), document)
	if err != nil {
		return fmt.Errorf("list recorded %s marks: %w", target.table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var encoded []byte
		var markType string
		if err := rows.Scan(&encoded, &markType); err != nil {
			return fmt.Errorf("scan recorded %s mark: %w", target.table, err)
		}
		var anchor model.Anchor
		if err := model.DecodeAnchor(encoded, &anchor); err != nil {
			return fmt.Errorf("decode recorded %s mark: %w", target.table, err)
		}
		if anchor.MarkID != "" {
			recorded[pmdoc.MarkRef{Type: markType, ID: anchor.MarkID}] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list recorded %s marks: %w", target.table, err)
	}
	return nil
}

// sweepUnrecordedMarks removes every ask, comment or suggestion mark of this tree that no row
// records and that has been unrecorded for unrecordedMarkTTL, counted from the first settlement
// that saw it: the provisional mark a composer the reader abandoned left behind.
//
// A mark whose author still has a browser on the room is the composer still open in front of
// somebody, whose Send would be refused as a missing anchor were the mark taken from under it, so
// it is kept and counted from this sweep instead. Once that browser has gone the mark ages from
// the author's last presence (state.departed), not from the sweep that last saw them: the sweeps
// of a quiet document are a TTL apart, so a drop a second before one - an idle reconnect - would
// otherwise lose the mark a second after it rather than a TTL after it. A tab that closes or goes
// offline therefore loses its stray mark one TTL later, as every other unrecorded mark does.
//
// The tree of an issue room is every fragment of layout joined, whose top-level blocks counts
// parts (roomLayout.parts): each fragment's marks are its own document's, which a row records
// against that document. So what every document of the room records is read before any fragment
// is swept, and each expired mark is taken out of the fragment of the document that holds it
// (unmarkExpired): no mark a document records is taken from it. A room that is its own document
// has an empty layout and one fragment.
func (s *Service) sweepUnrecordedMarks(room string, layout roomLayout, tree *pmdoc.Node, counts []int) {
	documents := layout.documents(room)
	parts := []*pmdoc.Node{tree}
	if len(layout) > 0 && tree != nil {
		if parts = layout.parts(tree, counts); parts == nil {
			slog.Error("dispatch: the settled tree of an issue room does not part into its fragments, so none of its marks is swept",
				"room", room, "fragments", len(documents))
			return
		}
	}
	recordedRefs, err := s.recordedMarkRefs(context.Background(), documents...)
	if err != nil {
		slog.Error("dispatch: list recorded marks for sweep", "room", room, "error", err)
		return
	}
	now := s.now()
	// expired is each fragment's marks to take out, beside parts.
	expired := make([][]pmdoc.MarkRef, len(parts))
	seen := make(map[pmdoc.MarkRef]struct{})
	var next time.Duration
	state := s.lockState(room)
	// With nothing unrecorded, every mark here is seen for the first time and its author cannot
	// change the answer, so the settlement of a room with no stray mark - the steady state - builds
	// no set and concatenates no reference.
	var authors map[string]struct{}
	if len(state.unrecorded) > 0 {
		authors = connectedMarkAuthors(state.connected)
	}
	for index, part := range parts {
		for _, mark := range pmdoc.ListMarks(part) {
			if mark.Type != string(MarkAsk) && mark.Type != string(MarkComment) && mark.Type != string(MarkSuggestion) {
				continue
			}
			seen[mark.MarkRef] = struct{}{}
			if _, found := recordedRefs[mark.MarkRef]; found {
				delete(state.unrecorded, mark.MarkRef)
				continue
			}
			firstSeen, seenBefore := state.unrecorded[mark.MarkRef]
			_, writing := authors[mark.By]
			if writing || !seenBefore {
				firstSeen = now
				state.unrecorded[mark.MarkRef] = firstSeen
			}
			since := firstSeen
			// A mark written while its author was already away ages from its own first sighting:
			// the later of the two is when it was last anybody's open composer. A mark with no
			// author matches no departure, as it matches no connected browser.
			if left, away := state.departed[mark.By]; away && left.After(since) {
				since = left
			}
			age := now.Sub(since)
			if age >= s.unrecordedMarkTTL {
				expired[index] = append(expired[index], mark.MarkRef)
			} else if wait := s.unrecordedMarkTTL - age; next == 0 || wait < next {
				next = wait
			}
		}
	}
	for mark := range state.unrecorded {
		if _, found := seen[mark]; !found {
			delete(state.unrecorded, mark)
		}
	}
	// A departure a TTL old answers nothing this sweep has not already decided: every mark of that
	// author is at least that old too, so the first sighting alone expires it from here on.
	for author, left := range state.departed {
		if now.Sub(left) >= s.unrecordedMarkTTL {
			delete(state.departed, author)
		}
	}
	if next > 0 {
		s.scheduleSettleAfterLocked(room, state, next)
	}
	s.unlockState(room, state)
	for index, marks := range expired {
		if len(marks) == 0 {
			continue
		}
		if err := s.unmarkExpired(documents[index], marks); err != nil {
			slog.Error("dispatch: sweep unrecorded marks", "room", room, "document", documents[index], "error", err)
		}
	}
}

// connectedMarkAuthors is every spelling of a mark's `by` attribute that names a user with a
// browser on the room: the bare login a browser's own mark carries, and the user:<login> of a
// mark the server wrote (ActorRef). It is nil when no such browser is there, which answers every
// mark the same way. An actor with no id names nobody, so a mark with no author matches nothing
// here. Its caller holds state.mu.
func connectedMarkAuthors(connected map[uint64]model.Actor) map[string]struct{} {
	var authors map[string]struct{}
	for _, actor := range connected {
		if actor.Kind != "user" || actor.ID == "" {
			continue
		}
		if authors == nil {
			authors = make(map[string]struct{}, 2*len(connected))
		}
		authors[actor.ID] = struct{}{}
		authors[ActorRef(actor)] = struct{}{}
	}
	return authors
}

// recordMarkAuthorDeparture records in state that actor's last browser left the room at now, from
// when an unrecorded mark of theirs ages (sweepUnrecordedMarks). Another browser of the same
// actor still on the room leaves nothing to record: the composer is still open in front of them.
// Both spellings of a mark's `by` are recorded, the two connectedMarkAuthors answers, so the
// sweep reads a departure with the one lookup it already makes. Its caller holds state.mu.
func recordMarkAuthorDeparture(state *roomState, actor model.Actor, now time.Time) {
	if actor.Kind != "user" || actor.ID == "" {
		return
	}
	for _, other := range state.connected {
		if other.Kind == actor.Kind && other.ID == actor.ID {
			return
		}
	}
	state.departed[actor.ID] = now
	state.departed[ActorRef(actor)] = now
}

// forgetMarkAuthorDeparture takes actor's departure off state: a browser of theirs is on the room
// again, so their marks are open composers once more and age from no earlier absence. Its caller
// holds state.mu.
func forgetMarkAuthorDeparture(state *roomState, actor model.Actor) {
	if actor.Kind != "user" || actor.ID == "" {
		return
	}
	delete(state.departed, actor.ID)
	delete(state.departed, ActorRef(actor))
}

// unmarkExpired removes from artifactID's document, its fragment of the live room it is edited in
// (targetOf), each of expired that the fragment still holds.
func (s *Service) unmarkExpired(artifactID string, expired []pmdoc.MarkRef) error {
	ctx := context.Background()
	target, err := s.targetOf(ctx, artifactID)
	if err != nil {
		return err
	}
	var sweepErr error
	err = s.srv.Apply(ctx, target.room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact, release := s.serviceTransact(transact)
		defer release()
		fragment := doc.GetXmlFragment(target.fragment)
		// The tree is read inside the transaction that unmarks it, under the document's lock: a
		// walk of the live tree outside one takes no lock and can read a peer's write halfway.
		transact(func(txn *crdt.Transaction) {
			fresh, readErr := treeOfTransaction(txn, fragment)
			if readErr != nil {
				sweepErr = readErr
				return
			}
			for _, mark := range expired {
				if _, _, found := pmdoc.FindMark(fresh, mark.Type, mark.ID); !found {
					continue
				}
				if err := pmdoc.Unmark(txn, fragment, mark.Type, mark.ID); err != nil {
					sweepErr = documentSchemaError(err)
					return
				}
			}
		})
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		sweepErr = err
	}
	return sweepErr
}
