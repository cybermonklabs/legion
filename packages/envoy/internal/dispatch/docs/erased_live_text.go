package docs

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// ErasedLiveText is what `envoy-dispatch erase` (grapherase.Erase) leaves of the live state of a
// document whose text, margin or stored history holds the bytes it erases: the live document with
// the erased block's text replaced by the erase's marker and the erased comment's margin entries
// taken out, every other block, anchor mark and margin record as it stands, seeded as the
// document's one stored update in place of every update it holds, through the statements a
// rebuild writes (RebuildTx). The rows are replaced rather than edited because a stored update is
// ygo's encoding of the edits: a deleted item keeps its bytes there until a fold collects them,
// and a text substitution inside the encoding would leave an update no load can decode.
//
// It is read first (ReadErasedLiveText) and written second (Replace), both in the erase's own
// transaction, so the erase lists what it would do before any of it is done, and a failure
// anywhere rolls back the whole erase.
type ErasedLiveText struct {
	// Held is whether the erased block's own text stood in the live text. A document that holds
	// none - one a version of which held the text, edited since, or one whose margin alone held an
	// erased comment - keeps its text, and its history, which can still hold the bytes in the
	// items that edit deleted or in the margin, is replaced all the same.
	Held bool
	// Left is whether the seed would still hold the bytes of the erased block's own text: the
	// block holds them where its markdown spells them otherwise than the erased text is written
	// - an escaped `*` or `_`, for one - so the replacement, which reads the markdown byte for
	// byte, found no place for the marker there. Bytes another block of the document holds are
	// that block's text and leave this false. The erase refuses a document this is true of
	// rather than report success over the bytes it would leave (grapherase.Erase).
	Left bool
	// Updates is how many stored updates the history holds, every one of which the seed replaces.
	Updates int64
	// AnchorMarks is how many comment, suggestion and ask anchors go with the erased block's text,
	// which the seed rewrites from markdown, and MarginRecords how many margin records the seed
	// drops or rewrites: an erased comment's own, and the record of the thread an erased reply
	// stands in. Every other mark and record stands in the seed as it stood in the live document.
	AnchorMarks, MarginRecords int
	// Text is the live text before the erase, whose stored display tree goes with it.
	Text string

	room string
	head persistence.Version
	seed []byte
}

// ErasedScope is what one erase takes out of one document's live state.
type ErasedScope struct {
	// BlockID is the erased node's own block of this document, empty where the node is no block
	// of it - a comment anchored to it, whose body the document holds in its margin and not in
	// its text.
	BlockID string
	// Records are the margin records that go whole, by their mark id: an erased comment's own.
	Records []string
	// Replies are the entries that go out of a thread root's margin record: an erased reply's.
	Replies []ErasedReply
	// Texts are the erased texts, longest first, and Marker what stands where one stood.
	Texts  []string
	Marker string
}

// ErasedReply names an erased reply's entry in the margin record of the thread it stands in: the
// root's mark id, the reply's author as the record writes it (ActorRef), and the time the reply
// was made as the record writes it (MarkTime). An entry of that record goes when its author and
// its time are those.
//
// The entry is named by who and when, never by its text: an edit of a reply projects the root's
// record no more (api's editComment re-projects an anchored comment alone), so the entry holds
// the body the reply had when the record was last written, which an erase after an edit no longer
// reads as any one of its texts.
type ErasedReply struct {
	Record, By, At string
}

// ReadErasedLiveText reads, inside tx, the live state of the document artifactID names as the
// erase would leave it (scope). It answers nil for a document with no stored history, which
// holds no live text. It writes nothing.
//
// The erased block's text is replaced inside that block and nowhere else: a block of the same
// document holding the same words is another node, whose text the erase lists and leaves (R5).
// The erased comment's margin entries go and no others: every other comment's record and every
// anchor mark outside the erased block are other nodes' too, and a suggestion whose mark went
// would answer that its anchor is orphaned when someone accepts it.
//
// It takes the document's owner row and then its advisory lock, the order every writer of a
// document takes them in (lockArtifactOwner, lockDocumentRoom), and holds both until tx ends, so
// the history it reads is the one Replace replaces and no edit lands between the two.
//
// The erased block's new text is the server's own rendering of the live tree with that block's
// text replaced, parsed back as a write over that tree is parsed (pmdoc.ParseWritten), and the
// block it reads back takes the erased block's place in the live tree (splicedBlock). A history
// that does not load, a tree outside the schema, and a replacement that breaks an ask's content
// rule or drops an open ask block each refuse the erase, naming the document: a document the
// erase could not leave whole is one an operator repairs first, by its rebuild or a replacement
// from markdown.
func ReadErasedLiveText(ctx context.Context, tx pgx.Tx, artifactID string, scope ErasedScope) (*ErasedLiveText, error) {
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
	markdown, offsets, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		return nil, fmt.Errorf("render document %s: %w", artifactID, err)
	}
	redacted := redactedBlock(markdown, offsets, scope.BlockID, scope.Texts, scope.Marker)
	seeded := tree
	if redacted != markdown {
		target, _, err := pmdoc.ParseWritten(redacted, tree)
		if err != nil {
			return nil, fmt.Errorf("read back the redacted text of document %s: %w", artifactID, err)
		}
		seeded = splicedBlock(tree, target, scope.BlockID)
	}
	if err := validateStoredAskBlocks(seeded); err != nil {
		return nil, fmt.Errorf("the redacted text of document %s: %w", artifactID, &ErrInvalidAskBlock{Reason: err})
	}
	// The erase is an operator's (envoy-dispatch erase), no agent's write.
	if err := refuseDroppedAskBlocksIn(ctx, tx, artifactID, seeded, dropper{person: true}); err != nil {
		return nil, fmt.Errorf("the redacted text of document %s: %w", artifactID, err)
	}
	margin, moved := keptMargin(marginOf(doc), scope)
	if erased.seed, err = encodeDocumentSeed(seeded, tree, margin); err != nil {
		return nil, fmt.Errorf("seed the redacted document %s: %w", artifactID, err)
	}
	erased.Held = redacted != markdown
	erased.Left = erasedBytesLeft(tree, seeded, scope.BlockID, scope.Texts, scope.Marker, erased.seed)
	erased.AnchorMarks = len(pmdoc.ListMarks(tree)) - len(pmdoc.ListMarks(seeded))
	erased.MarginRecords = moved
	erased.Text = markdown
	return erased, nil
}

// splicedBlock is the live tree with the block blockID names replaced by the block target, the
// redacted text read back, holds at the same place: every other node of the live tree, its anchor
// marks and its block ids with it, stands as it is. The parse back gives a block whose text
// changed a fresh id (pmdoc's carryBlockIDs), so the block is found by its place and not by that
// id. Where the redaction moved the document's shape, so that the place holds no block of the
// same kind in target, the seed is target whole, and the anchor marks of the live tree go with
// it: a run that cannot say which block of target is the erased one keeps none of the live tree
// it cannot match.
func splicedBlock(live, target *pmdoc.Node, blockID string) *pmdoc.Node {
	path := blockPath(live, blockID)
	if path == nil {
		return target
	}
	was, now := nodeAt(live, path), nodeAt(target, path)
	if was == nil || now == nil || was.Type != now.Type {
		return target
	}
	return replacedAt(live, path, now)
}

// replacedAt is a copy of tree with the node at path replaced by with. Only the nodes along the
// path are copied, so tree itself is left as it was.
func replacedAt(tree *pmdoc.Node, path []int, with *pmdoc.Node) *pmdoc.Node {
	if len(path) == 0 {
		return with
	}
	copied := *tree
	copied.Children = slices.Clone(tree.Children)
	copied.Children[path[0]] = replacedAt(tree.Children[path[0]], path[1:], with)
	return &copied
}

// marginOf is every margin record doc holds, by its mark id.
func marginOf(doc *crdt.Doc) map[string]any {
	margin := map[string]any{}
	doc.GetMap(marksMapName).ForEach(func(key string, record any) { margin[key] = record })
	return margin
}

// keptMargin is margin as the erase leaves it, and how many records it dropped or rewrote: every
// record but those scope names whole, and the record of each thread an erased reply stands in
// with that reply's entry taken out (ErasedReply). Every other record is another comment's, which
// stands as it is.
func keptMargin(margin map[string]any, scope ErasedScope) (map[string]any, int) {
	moved := 0
	for _, key := range scope.Records {
		if _, held := margin[key]; held {
			delete(margin, key)
			moved++
		}
	}
	for _, reply := range scope.Replies {
		record, ok := margin[reply.Record].(map[string]any)
		if !ok {
			continue
		}
		replies, _ := record["replies"].([]any)
		kept := make([]any, 0, len(replies))
		for _, entry := range replies {
			fields, _ := entry.(map[string]any)
			by, _ := fields["by"].(string)
			at, _ := fields["at"].(string)
			if by == reply.By && at == reply.At {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == len(replies) {
			continue
		}
		rewritten := maps.Clone(record)
		rewritten["replies"] = kept
		margin[reply.Record] = rewritten
		moved++
	}
	return margin, moved
}

// KeptMarginQuoting answers the mark id of every margin record of the document artifactID names
// that the erase keeps (scope) and that holds one of scope.Texts, read through q from the stored
// history and taking no lock, so a run without --confirm lists them in its read-only transaction.
//
// Such a record is another comment's - a suggestion whose replacement, or a comment whose body,
// quotes the erased text - and it is that comment's text, which the erase leaves (R5). It stands
// in the seed the erase writes in place of the document's history, so those bytes stay in that
// document's doc_updates, and the listing names each such record as left rather than reading the
// whole history as the erased node's.
func KeptMarginQuoting(ctx context.Context, q Queryer, artifactID string, scope ErasedScope) ([]string, error) {
	head, err := NewPgVersioned(nil).head(ctx, q, artifactID)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, nil
	}
	doc, err := documentThrough(artifactID, head, storedUpdates(ctx, q, artifactID, head), maxUpdateItems)
	if err != nil {
		return nil, fmt.Errorf("load document %s: %w", artifactID, err)
	}
	if doc == nil {
		return nil, nil
	}
	kept, _ := keptMargin(marginOf(doc), scope)
	var quoting []string
	for key, record := range kept {
		if holdsAnyText(record, scope.Texts) {
			quoting = append(quoting, key)
		}
	}
	slices.Sort(quoting)
	return quoting, nil
}

// holdsAnyText is whether any string in value, at any depth, holds one of texts.
func holdsAnyText(value any, texts []string) bool {
	switch value := value.(type) {
	case string:
		for _, text := range texts {
			if strings.Contains(value, text) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if holdsAnyText(item, texts) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if holdsAnyText(item, texts) {
				return true
			}
		}
	}
	return false
}

// encodeDocumentSeed writes tree and margin into a fresh document and returns its one durable
// update, as encodeDocumentTreeHolding does for a tree alone: the erase's seed carries the
// document's margin records as well as its text and its anchor marks. held is the live tree the
// seed is cut from, whose elements the document already holds (pmdoc.UpdateHolding), so an
// element a newer build wrote there is put back rather than refused.
func encodeDocumentSeed(tree, held *pmdoc.Node, margin map[string]any) ([]byte, error) {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	marks := doc.GetMap(marksMapName)
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		if err := pmdoc.UpdateHolding(transaction, fragment, tree, held); err != nil {
			return err
		}
		for _, key := range slices.Sorted(maps.Keys(margin)) {
			marks.Set(transaction, key, margin[key])
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil), nil
}

// redactedBlock is markdown with each of texts replaced by marker inside the byte range the
// block blockID names and nowhere else (pmdoc.BlockOffset, recorded by the same render). The
// markdown stands as it is where blockID is empty, where the document's live tree holds no block
// of that id - a block a later version dropped - and where the block's own range holds none of
// the texts: the bytes anywhere else are another block's, and a block erase that rewrote them
// would take away text whose own node nobody erased.
func redactedBlock(markdown string, offsets []pmdoc.BlockOffset, blockID string, texts []string, marker string) string {
	if blockID == "" {
		return markdown
	}
	for _, offset := range offsets {
		if offset.ID != blockID || offset.From < 0 || offset.To > len(markdown) || offset.From > offset.To {
			continue
		}
		held := markdown[offset.From:offset.To]
		redacted := held
		for _, text := range texts {
			redacted = strings.ReplaceAll(redacted, text, marker)
		}
		if redacted == held {
			return markdown
		}
		return markdown[:offset.From] + redacted + markdown[offset.To:]
	}
	return markdown
}

// erasedBytesLeft is whether the seed still holds bytes of the erased block's own text
// (ErasedLiveText.Left). A text the marker itself holds cannot be told from the marker in the
// seed, and is left to the replacement.
//
// It reads the erased block as the seed's tree holds it, found at the place blockID stands at in
// the live tree: the parse back gives a block whose text changed a fresh id (pmdoc's
// carryBlockIDs), so the block is found by its place and not by that id, and a block the
// redaction did not rewrite is the live block itself. A node that is no block of this document
// (an empty blockID) and a block the live tree does not hold leave nothing of the node in the
// live text, so neither can leave bytes there. Where the redaction moved the document's shape,
// so that the place holds no block of the same kind, the whole seed is read instead - its margin
// records included: a run that cannot say which block holds the bytes refuses rather than leave
// them.
func erasedBytesLeft(live, seeded *pmdoc.Node, blockID string, texts []string, marker string, seed []byte) bool {
	if blockID == "" {
		return false
	}
	path := blockPath(live, blockID)
	if path == nil {
		return false
	}
	held := func(text string) bool { return bytes.Contains(seed, []byte(text)) }
	if was, now := nodeAt(live, path), nodeAt(seeded, path); now != nil && was != nil && now.Type == was.Type {
		content := blockContent(now)
		held = func(text string) bool { return strings.Contains(content, text) }
	}
	for _, text := range texts {
		if !strings.Contains(marker, text) && held(text) {
			return true
		}
	}
	return false
}

// blockPath is the place of the block blockID names in tree, as the indexes of the children that
// lead to it, or nil where the tree holds no such block.
func blockPath(tree *pmdoc.Node, blockID string) []int {
	if tree == nil {
		return nil
	}
	if id, _ := tree.Attrs[pmdoc.BlockIDAttr].(string); id == blockID {
		return []int{}
	}
	for index, child := range tree.Children {
		if below := blockPath(child, blockID); below != nil {
			return append([]int{index}, below...)
		}
	}
	return nil
}

// nodeAt is the node at a place blockPath named, or nil where the tree is shorter there.
func nodeAt(tree *pmdoc.Node, path []int) *pmdoc.Node {
	node := tree
	for _, index := range path {
		if node == nil || index >= len(node.Children) {
			return nil
		}
		node = node.Children[index]
	}
	return node
}

// blockContent is what a block holds that a search of the stored history finds: its text and
// every attribute value under it, which a typed block's opening line writes. The seed holds each
// as the bytes they are, so a text held in either is a text the seed holds.
func blockContent(node *pmdoc.Node) string {
	var held strings.Builder
	held.WriteString(pmdoc.TextContent(node))
	var attributes func(*pmdoc.Node)
	attributes = func(node *pmdoc.Node) {
		for _, value := range node.Attrs {
			if text, ok := value.(string); ok {
				held.WriteString("\n")
				held.WriteString(text)
			}
		}
		for _, child := range node.Children {
			attributes(child)
		}
	}
	attributes(node)
	return held.String()
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
