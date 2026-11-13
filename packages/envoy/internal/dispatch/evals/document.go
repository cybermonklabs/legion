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
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// existingDocument is the project's `evals-md` document, found false when the project has none.
// The lookup runs again under the lock every creation of a project document takes
// (docs.LockProjectArtifacts), which a miss leaves held for the rest of the transaction, so two
// writes that both miss the document create one: the second finds the first's once it commits.
func existingDocument(ctx context.Context, w *writeTx) (evalsDocument, bool, error) {
	if document, found, err := findDocument(ctx, w); found || err != nil {
		return document, found, err
	}
	if err := docs.LockProjectArtifacts(ctx, w.Tx, w.project); err != nil {
		return evalsDocument{}, false, err
	}
	return findDocument(ctx, w)
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
// the document is and then sections, and appends its `artifact.created` event, as an upload of a
// new project document does (docs.CreateDocument). The sections are in the seeded text rather
// than added by an edit after it: an edit reads a document no room holds from the store (docs'
// coldFork, through its own connection), where a seed this transaction has not committed is not
// yet.
func (s *Store) createDocument(ctx context.Context, w *writeTx, sections string, actor model.Actor) error {
	artifact, written, err := docs.CreateDocument(ctx, s.documents, docs.NewArtifact{
		Project: &w.project, Slug: DocumentSlug, Name: DocumentName, CreatedBy: actor,
	}, documentIntroduction(w.project, w.projectName)+"\n"+sections)
	if err != nil {
		return fmt.Errorf("create the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	event, err := s.events.Append(ctx, w.Tx, model.Event{
		ArtifactID: &artifact.ID, Type: "artifact.created", Actor: actor,
		Payload: docs.ArtifactCreatedEventPayload(artifact, written.Changes),
	})
	if err != nil {
		return err
	}
	if err := refs.Stamp(ctx, w.Tx, "artifact", artifact.ID, event.ID); err != nil {
		return err
	}
	w.events = append(w.events, event)
	w.owesSettlement(artifact.ID)
	return nil
}

// documentIntroduction is the start of a new `evals-md` document's text: its title, and what it
// holds.
func documentIntroduction(project, name string) string {
	return "# Evals of " + name + "\n\n" +
		"This document holds one section for each eval of project " + project + ": a number that the project's pages show. " +
		"The heading of a section is the eval's slug, and the block under it is the eval's method: what the number counts, " +
		"how, and what can make it wrong. Dispatch adds a section when an eval is registered, and a register write that " +
		"carries `doc_text` replaces the method of its eval.\n"
}

// writeMethod makes method, as actor writes it, the method block of the eval slug names, under
// the new block id blockID, in one write of the document. A section whose block oldBlock names
// gets the new block in place of that one, in one batch of edit operations: inserted after it,
// then the old block deleted. An eval with no such block in the document (none yet, or one a
// person deleted) gets a new section at the document's end. A project with no `evals-md` gets
// one, created holding that section.
func (s *Store) writeMethod(ctx context.Context, w *writeTx, slug, oldBlock, blockID, method string, actor model.Actor) error {
	document, found, err := existingDocument(ctx, w)
	if err != nil {
		return err
	}
	if !found {
		return s.createDocument(ctx, w, sectionMarkdown(slug, blockID, method), actor)
	}
	if err := lockDocumentOwner(ctx, w, document); err != nil {
		return err
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

// lockDocumentOwner locks the document's owner row before an edit takes its room, as the edit
// route locks it (requireOpenOwnerStatus), so a registry write takes the two in the order every
// other writer of the document does.
func lockDocumentOwner(ctx context.Context, w *writeTx, document evalsDocument) error {
	if _, err := w.Exec(ctx, `select true from artifacts where id = $1 and issue_key is null for no key update`, document.id); err != nil {
		return fmt.Errorf("lock the %s document of %s: %w", DocumentSlug, w.project, err)
	}
	return nil
}

// hasBlock reports whether blocks holds the block id names, at any depth: whether the document
// those blocks are of still holds an eval's method block.
func hasBlock(blocks []model.ArtifactBlock, id string) bool {
	return slices.ContainsFunc(blocks, func(block model.ArtifactBlock) bool {
		return block.ID == id || slices.Contains(block.DescendantIDs, id)
	})
}

// editDocument applies ops to the document as one batch and, where the batch changed it, writes
// the version it reached and appends that version's `artifact.version` event, as the edit route
// does for a batch with no summary.
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
	event, err := s.events.Append(ctx, w.Tx, model.Event{
		ArtifactID: &document.id, Type: "artifact.version", Actor: actor,
		Payload: docs.ArtifactVersionEventPayload(document.id, document.name, written.Version, nil, written.Changes),
	})
	if err != nil {
		return err
	}
	if err := refs.Stamp(ctx, w.Tx, "artifact", document.id, event.ID); err != nil {
		return err
	}
	w.events = append(w.events, event)
	return nil
}

// owesSettlement records that the transaction wrote artifactID, whose settlement is scheduled
// once it commits.
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
// saying who registered the eval, through which write, and when.
func registrationSentence(via string, actor model.Actor, at time.Time) string {
	when := at.UTC().Format("2006-01-02 15:04:05") + " UTC"
	who := describeActor(actor)
	switch via {
	case ViaReading:
		return "This eval was registered by a reading that " + who + " logged at " + when + ", and no one has written its method yet."
	case ViaSeed:
		return "This eval was registered by the seeded catalogue at " + when + ", and no one has written its method yet."
	case ViaSettings:
		return "This eval was registered by a settings write of " + who + " at " + when + ", and no one has written its method yet."
	default:
		return "This eval was registered by a register write of " + who + " at " + when + ", and no one has written its method yet."
	}
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
