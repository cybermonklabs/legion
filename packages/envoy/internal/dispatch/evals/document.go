package evals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's `evals-md` document holds one section per eval: a `##` heading that is the eval's
// slug, then the eval's method block, a callout typed block whose id the eval's doc_block names.
// The method is one block so that one id names all of it, whatever its text holds, and so a new
// method replaces it whole: an insert after the block, then its delete, in one batch of edit
// operations (writeMethod), as the edit route applies a caller's batch (api's editArtifact).
const (
	// DocumentSlug and DocumentName are the `evals-md` document's slug and file name, as an
	// upload of `evals.md` would name it.
	DocumentSlug = "evals-md"
	DocumentName = "evals.md"
)

// evalsDocument is a project's `evals-md` document, as its writes name it.
type evalsDocument struct {
	id   string
	name string
}

// errDocumentRemoved is lockDocument's answer when the project's `evals-md` it found was removed
// before the write held the document's owner lock. The write has stored nothing by then, and the
// one lock it would take next to create the document anew, the project's documents' lock
// (docs.LockProjectArtifacts), comes before an owner lock in every writer, so the write is not let
// take it while it holds one: inWrite runs the write again, in a transaction of its own, which
// finds no document and creates it.
var errDocumentRemoved = errors.New("evals: the project's evals-md was removed while the write waited for its lock")

// lockDocument takes the lock every writer of the project's `evals-md` serialises on, the
// document's owner lock (store.LockOwner of store.ArtifactOwner, a transaction advisory lock),
// and answers the document, found false when the project has none. Every registry write that may
// write the document takes it in its read phase, after its eval locks (lockEval, lockEvals) and
// before it reads the document or stores anything: a first registration, a register write of a
// method and the seed. So registry writes queue on it holding no transaction id while no earlier
// build is in the fleet - beside one it takes the owner's row too, right after the advisory lock
// (store.LockOwner) - and hold back no reader of the event log (events.Horizon), whose horizon
// waits only for running transaction ids. It is the lock the edit route takes first
// (requireOpenOwnerStatus), and the docs package takes it again where an operation joins the
// document (lockArtifactOwner), which returns at once, so registry writes and every other writer
// of the document take turns on one lock in one order: the eval locks, which no document writer
// takes, then the owner lock, then the document's own (the writer slot and the document's
// advisory lock, docs' liveWrite).
//
// A project with no `evals-md` yet is read again under the lock every creation of a project
// document takes (docs.LockProjectArtifacts), which a miss leaves held for the rest of the
// transaction, so two writes that both miss the document create one: the second finds the first's
// once it commits and takes its owner lock after that lock, in the order a project upload takes
// the two. The document's row is read again in the statement after the owner lock, sent with it
// in one round trip (store.LockOwnerAndRead), so a removal that committed while the write waited
// for the lock is seen (errDocumentRemoved), and a title correction's name is the one the write's
// event carries.
func lockDocument(ctx context.Context, w *writeTx) (evalsDocument, bool, error) {
	document, found, err := findDocument(ctx, w)
	if err != nil {
		return evalsDocument{}, false, err
	}
	if !found {
		if err := docs.LockProjectArtifacts(ctx, w.Tx, w.project); err != nil {
			return evalsDocument{}, false, err
		}
		if document, found, err = findDocument(ctx, w); err != nil || !found {
			return document, found, err
		}
	}
	err = store.LockOwnerAndRead(ctx, w.Tx, store.ArtifactOwner(document.id), `
		select name from artifacts where id = $1 and issue_key is null
	`, []any{document.id}, &document.name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return evalsDocument{}, false, errDocumentRemoved
	case err != nil:
		return evalsDocument{}, false, fmt.Errorf("lock the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	return document, true, nil
}

func findDocument(ctx context.Context, w *writeTx) (evalsDocument, bool, error) {
	var document evalsDocument
	var kind string
	err := w.QueryRow(ctx, `
		select id::text, name, kind from artifacts
		where project_key = $1 and issue_key is null and session_id is null and slug = $2
	`, w.project, DocumentSlug).Scan(&document.id, &document.name, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return evalsDocument{}, false, nil
	}
	if err != nil {
		return evalsDocument{}, false, fmt.Errorf("read the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	if kind != "doc" {
		return evalsDocument{}, false, refusal(CodeDocumentNotDocument,
			"project %s's artifact %s is a %s, and the registry writes each eval's method into a document of that slug", w.project, DocumentSlug, kind)
	}
	return document, true, nil
}

// createDocument creates the project's `evals-md` as actor, its text the paragraph that says what
// the document is and then sections, and queues its `artifact.created` event, as an upload of a
// new project document does (docs.CreateDocument). The sections are in the seeded text rather
// than added by an edit after it: an edit reads a document no room holds from the store (docs'
// coldFork), where a document whose insert this transaction has only queued is not yet. The
// caller holds the project's documents' lock and found no `evals-md` under it (lockDocument).
//
// The document's settlement is scheduled only where its seed owes one (docs.WrittenText's
// OwesSettlement), as it does where a method holds an ask block or a check carrying `ci_check`.
// The text this writes otherwise - a heading, a paragraph, and a heading and a callout per eval,
// each block given its id by the parse - leaves a settlement nothing to write but the delete of
// the row that said one was owed, so such a create records nothing owed and no settlement loads
// the document's room for it, on a project create (api's createProject, through EnsureTx) and on
// a first registration alike.
func (s *Store) createDocument(ctx context.Context, w *writeTx, sections string, actor model.Actor) error {
	artifact, written, err := docs.CreateDocument(ctx, s.documents, docs.NewArtifact{
		Project: &w.project, RefPrefix: w.project, Slug: DocumentSlug, Name: DocumentName, CreatedBy: actor,
	}, documentIntroduction(w.project, w.projectName)+"\n"+sections)
	if err != nil {
		return fmt.Errorf("create the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	if err := s.queueDocumentEvent(w, artifact.ID, model.Event{
		ArtifactID: &artifact.ID, Type: "artifact.created", Actor: actor,
		Payload: docs.ArtifactCreatedEventPayload(artifact, written.Changes),
	}, written.Blocks); err != nil {
		return err
	}
	if written.OwesSettlement {
		w.owesSettlement(artifact.ID)
	}
	return nil
}

// queueDocumentEvent queues event, an event of the document artifactID, on w's ledger: its append
// (events.Broker.QueueAppend) and, in the same step, the stamp of the document's new reference
// edges with its id (refs.QueueStamp), after the append so stamping never touches sequence
// allocation, as api's queueStampedEvent queues a route's. Right after them it appends what the
// version's block index stated in the node and tuple store (blocks) under that event
// (graphstore.QueueCaused over graphstore.LatestEvent), as api's storeArtifact and
// commitArtifactVersionEvent do for an upload and an edit: the index ran in the read phase,
// before the event existed, and no tuple may be updated once it is appended. The append takes the
// document's owner lock again, which the write holds from its read phase (lockDocument), and the
// owner's row, in the write phase. The event is filled in once the ledger's Commit has run that
// phase, and published once the transaction has committed (committed).
func (s *Store) queueDocumentEvent(w *writeTx, artifactID string, event model.Event, blocks graphstore.Uncaused) error {
	appended := new(model.Event)
	if err := w.ledger.Queue(func(b *store.WriteBatch) error {
		if err := s.events.QueueAppend(b, event, func(written model.Event) { *appended = written }); err != nil {
			return err
		}
		refs.QueueStamp(b, "artifact", artifactID)
		graphstore.QueueCaused(b, blocks, graphstore.LatestEvent)
		return nil
	}); err != nil {
		return err
	}
	w.events = append(w.events, appended)
	return nil
}

// documentIntroduction is the start of a new `evals-md` document's text: its title, and what it
// holds.
func documentIntroduction(project, name string) string {
	return "# Evals of " + name + "\n\n" +
		"This document holds one section for each eval of project " + project + ": a number that the project's pages show. " +
		"The heading of a section is the eval's slug, and the block under it is the eval's method: what the number counts, " +
		"how, and what can make it wrong. Dispatch adds a section when an eval is registered, and a register write that " +
		"carries `doc_text` replaces the method of its eval. On a computed eval a register write sets only how the number " +
		"is judged and who answers for it: its target, the `min_n` of pr-ci's job series, its owner, its links, its state and " +
		"its method. The code its source names defines the rest.\n"
}

// writeMethod makes method, as actor writes it, the method block of the eval slug names, under
// the new block id blockID, in one write of document, the project's `evals-md` as lockDocument
// answered it (found false: the project has none). A section whose block oldBlock names gets the
// new block in place of that one, in one batch of edit operations: inserted after it, then the
// old block deleted. An eval with no such block in the document (none yet, or one a person
// deleted) gets a new section at the document's end. A project with no `evals-md` gets one,
// created holding that section.
//
// It is the document work of its write (WriteReadings, Register), done in the read phase, under
// the document's owner lock and before the write stores anything: it reads and renders the
// document and queues the document's writes and its event on the write's ledger, which Commit
// sends after the write's own rows, in the one transaction, so the eval's section exists once its
// registration commits and the write holds a transaction id only for its database statements. It
// takes the document's other locks in the order every writer of the document takes them (docs'
// liveWrite): ApplyOps has the other processes' rooms of the document stored, takes the owner
// lock again, which returns at once, then the writer slot and the document's advisory lock.
func (s *Store) writeMethod(ctx context.Context, w *writeTx, document evalsDocument, found bool, slug, oldBlock, blockID, method string, actor model.Actor) error {
	if !found {
		return s.createDocument(ctx, w, sectionMarkdown(slug, blockID, method), actor)
	}
	present := false
	if oldBlock != "" {
		blocks, err := s.documents.Blocks(ctx, document.id)
		if err != nil {
			return fmt.Errorf("read the blocks of the %s document of %s: %w", DocumentSlug, w.project, err)
		}
		present = hasBlock(blocks, oldBlock)
	}
	ops := []model.EditOp{{Op: "insert", After: "end", Markdown: sectionMarkdown(slug, blockID, method)}}
	if present {
		ops = []model.EditOp{
			{Op: "insert", After: "block:" + oldBlock, Markdown: methodBlock(blockID, method)},
			{Op: "delete", Block: oldBlock},
		}
	}
	return s.editDocument(ctx, w, document, ops, actor)
}

// holdsMethod reports whether the method block docBlock names in document, the project's
// `evals-md` as lockDocument answered it (found false: the project has none), holds method
// already (sameMethod): a register write whose doc_text is the method its eval's section holds
// writes no new block, and so no new version. It is false where docBlock is empty, or the project
// has no such document or the document no such block.
func (s *Store) holdsMethod(ctx context.Context, w *writeTx, document evalsDocument, found bool, docBlock, method string) (bool, error) {
	if docBlock == "" || !found {
		return false, nil
	}
	markdown, blocks, err := s.documents.TextWithBlocks(ctx, document.id)
	if err != nil {
		return false, fmt.Errorf("read the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	held, found := blockMethod(markdown, blocks, docBlock)
	return found && sameMethod(held, method), nil
}

// sameMethod reports whether a method a section holds and a method a register write sends read
// the same. The document keeps a method in its canonical markdown, each typed block in it given an
// id and its attributes their defaults, so the two are compared as that markdown with every
// block's id set aside (canonicalMethod); either failing to parse is no match.
func sameMethod(held, sent string) bool {
	heldCanonical, heldErr := canonicalMethod(held)
	sentCanonical, sentErr := canonicalMethod(sent)
	return heldErr == nil && sentErr == nil && heldCanonical == sentCanonical
}

// canonicalMethod is method as the document's markdown writes it (pmdoc.Parse, then
// pmdoc.Render), with every block's id the same placeholder, since a typed block is written with
// one.
func canonicalMethod(method string) (string, error) {
	tree, err := pmdoc.Parse(method)
	if err != nil {
		return "", err
	}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if _, held := node.Attrs[pmdoc.BlockIDAttr]; held {
			node.Attrs[pmdoc.BlockIDAttr] = "block"
		}
		return true
	})
	return pmdoc.Render(tree)
}

// hasBlock reports whether blocks holds the block id names, at any depth: whether the document
// those blocks are of still holds an eval's method block.
func hasBlock(blocks []model.ArtifactBlock, id string) bool {
	return slices.ContainsFunc(blocks, func(block model.ArtifactBlock) bool {
		return block.ID == id || slices.Contains(block.DescendantIDs, id)
	})
}

// editDocument applies ops to the document as one batch and, where the batch changed it, queues
// the version it reached and that version's `artifact.version` event, as the edit route does for
// a batch with no summary.
func (s *Store) editDocument(ctx context.Context, w *writeTx, document evalsDocument, ops []model.EditOp, actor model.Actor) error {
	edit, err := s.documents.ApplyOps(ctx, document.id, ops, actor, nil)
	if err != nil {
		return fmt.Errorf("edit the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	if edit.Applied > 0 {
		w.owesSettlement(document.id)
	}
	if !edit.Changed {
		return nil
	}
	written, err := s.documents.SnapshotVersion(ctx, document.id, actor)
	if err != nil {
		return fmt.Errorf("write the version of the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	if !written.Wrote {
		return nil
	}
	return s.queueDocumentEvent(w, document.id, model.Event{
		ArtifactID: &document.id, Type: "artifact.version", Actor: actor,
		Payload: docs.ArtifactVersionEventPayload(document.id, document.name, written.Version, nil, written.Changes),
	}, written.Blocks)
}

// owesSettlement records that the transaction wrote artifactID and left it owing a settlement,
// which is scheduled once it commits (committed).
func (w *writeTx) owesSettlement(artifactID string) {
	if !slices.Contains(w.settle, artifactID) {
		w.settle = append(w.settle, artifactID)
	}
}

// sectionMarkdown is an eval's section: its heading, the slug, and its method block.
func sectionMarkdown(slug, blockID, method string) string {
	return "## " + slug + "\n\n" + methodBlock(blockID, method)
}

// methodBlock is a method block: a callout whose markdown names its own id, so the version that
// links it is written knowing the id before the document holds the block. Its fence is longer
// than any run of colons that opens a line of method, so no line of the method closes it.
func methodBlock(blockID, method string) string {
	fence := strings.Repeat(":", fenceLength(method))
	return fence + "callout{#" + blockID + "}\n" + strings.TrimSpace(method) + "\n" + fence + "\n"
}

// Method reads the method an eval's section of its project's `evals-md` document holds: the text
// of the method block docBlock names, inside its fence, as the register write's doc_text gave
// it (methodBlock). found is false where the project has no such document, its artifact of that
// slug is no document, or the document holds no block of that id (a person deleted it).
func (s *Store) Method(ctx context.Context, project, docBlock string) (string, bool, error) {
	if docBlock == "" {
		return "", false, nil
	}
	var id, kind string
	err := s.database.Pool.QueryRow(ctx, `
		select id::text, kind from artifacts
		where project_key = $1 and issue_key is null and session_id is null and slug = $2
	`, project, DocumentSlug).Scan(&id, &kind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "doc") {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the %s document of %s: %w", DocumentSlug, project, err)
	}
	markdown, blocks, err := s.documents.TextWithBlocks(ctx, id)
	if err != nil {
		return "", false, fmt.Errorf("read the %s document of %s: %w", DocumentSlug, project, err)
	}
	method, found := blockMethod(markdown, blocks, docBlock)
	return method, found, nil
}

// blockMethod is the method the block id names holds among blocks, the blocks of markdown
// (methodText of its markdown), found false where markdown holds no block of that id.
func blockMethod(markdown string, blocks []model.ArtifactBlock, id string) (string, bool) {
	index := slices.IndexFunc(blocks, func(block model.ArtifactBlock) bool { return block.ID == id })
	if index < 0 || blocks[index].From < 0 || blocks[index].To > len(markdown) || blocks[index].From > blocks[index].To {
		return "", false
	}
	return methodText(markdown[blocks[index].From:blocks[index].To]), true
}

// methodText is a method block's markdown without its fence: the lines between the line that
// opens it (`:::callout{#<id>}`) and the run of colons that closes it, trimmed.
func methodText(block string) string {
	lines := strings.Split(strings.TrimSpace(block), "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), ":::") {
		lines = lines[1:]
	}
	if last := len(lines) - 1; last >= 0 && strings.Trim(strings.TrimSpace(lines[last]), ":") == "" {
		lines = lines[:last]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// fenceLength is three, the shortest fence a typed block opens with, or one more than the
// longest run of colons that starts a line of text.
func fenceLength(text string) int {
	longest := 2
	for line := range strings.SplitSeq(text, "\n") {
		opened := strings.TrimLeft(line, " \t")
		run := len(opened) - len(strings.TrimLeft(opened, ":"))
		longest = max(longest, run)
	}
	return longest + 1
}

// registrationSentence is the method a section holds until someone writes one: one sentence
// saying who registered the eval, through which write (a reading, ViaReading, or a register
// write), and when.
func registrationSentence(via string, actor model.Actor, at time.Time) string {
	when := at.UTC().Format(time.DateTime) + " UTC"
	who := describeActor(actor)
	if via == ViaReading {
		return "This eval was registered by a reading that " + who + " logged at " + when + ", and no one has written its method yet."
	}
	return "This eval was registered by a register write of " + who + " at " + when + ", and no one has written its method yet."
}

// describeActor names an actor in prose: its kind and id, and the person it acts for where its
// credential names one other than itself.
func describeActor(actor model.Actor) string {
	described := actor.Kind + " `" + actor.ID + "`"
	if actor.ID == "" {
		described = "a " + actor.Kind
	}
	if _, person := ownerOf(actor); person != "" && person != actor.ID {
		described += " for `" + person + "`"
	}
	return described
}
