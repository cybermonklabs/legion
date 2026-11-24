package docs

// A version's render stamp: what a version write records of the state its markdown was rendered
// from, and the text read that serves that markdown rather than rendering the document again.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// RenderStamp is the half of a version row's render stamp its writer holds in Go: the document
// token of the tree the version's markdown was rendered from, which a read answers beside that
// markdown, and the renderer version of the build that rendered it (pmdoc.RendererVersion), which
// a later build compares with its own before it serves the markdown. The other half - the head
// the tree stood at and the transaction that wrote the update row standing there - is read in the
// insert itself (RenderStampHead, RenderStampValues), so it is the head that insert's own appends
// left.
//
// Both are nil together: for a write holding no tree of the markdown it stores - an upload whose
// replacement changed nothing writes no tree - and for a tree holding a value no token encodes.
// The row then carries no token and no renderer version, and every read of it renders the
// document, as a read of a version written before the stamp existed does.
type RenderStamp struct {
	Token    *string
	Renderer *int
}

// RenderStampHead is the `with` clause a version write opens its insert with: the head of the
// room $1 names, read once as every reader of the document reads it (headVersion), for the two
// values RenderStampValues takes from it.
const RenderStampHead = `with head as (select ` + headVersion + ` as version)`

// RenderStampColumns are the four columns of a version row's render stamp, in the order a write
// supplies their values: the two RenderStampValues answers, then the token and the renderer
// version of the write's RenderStamp.
const RenderStampColumns = `head_version, doc_update_row, document_token, renderer_version`

// RenderStampValues are the first two values of RenderStampColumns, read from the head
// RenderStampHead names and the room $1 names: the head itself, and the transaction that wrote
// the doc_updates row standing at it, Postgres's xmin, which is the writing transaction's own for
// a row it appended in this transaction and null where no row stands there. Both read head as a
// subquery, so a statement selecting them is free of it in its own `from` - a version write
// aggregating over artifact_versions has no group the head belongs to - and the two reads
// evaluate it once, since Postgres materialises a `with` query two references read.
const RenderStampValues = `(select version from head),
		(select u.xmin::text from doc_updates u
			where u.artifact_id = $1 and u.version = (select version from head))`

// renderStampOf is the stamp a version write of tree stores: the tree's document token and this
// build's renderer version. A nil tree, or one holding a value no token encodes (nodeToken), is
// the empty stamp, which stores neither and reads as an unstamped version does.
func renderStampOf(artifactID string, tree *pmdoc.Node) RenderStamp {
	if tree == nil {
		return RenderStamp{}
	}
	token, err := nodeToken(tree)
	if err != nil {
		slog.Warn("dispatch: a document version stored no render stamp: its tree has no document token",
			"artifact", artifactID, "error", err)
		return RenderStamp{}
	}
	renderer := pmdoc.RendererVersion
	return RenderStamp{Token: &token, Renderer: &renderer}
}

// RenderStampOf is the render stamp of a version row storing markdown, for a caller that writes
// its version row itself rather than through writeVersionTx: an upload's and an issue's first
// spec (api/artifacts.go, api/issue_create.go). It stamps the tree this transaction's write of
// artifactID left, and only where that tree is what markdown was rendered from, as
// IndexDocumentReferences shares the write's rendering only where it is the version's markdown.
// Any other markdown takes the empty stamp.
func (l *Ledger) RenderStampOf(artifactID, markdown string) RenderStamp {
	rendered := l.writtenRendering(artifactID)
	if rendered.tree == nil || rendered.markdown != markdown {
		return RenderStamp{}
	}
	return renderStampOf(artifactID, rendered.tree)
}

// storedVersionSQL reads, for the room $1 names, everything a stored text read weighs, in one
// statement so the version row and the document's stamp are of one snapshot: the latest version's
// markdown, its document token and its render stamp, beside the document's DocumentStamp now
// (documentStampSQL), and whether the reader whose viewer $2 binds may read the document
// (access.ItemOwnerClause, the rule of a document's own row). Every other value is null for a
// document with no version, as the markdown and the stamp are for a version written before the
// stamp existed or by a write holding no tree. The rule is read once: a reader who may not read
// the document is answered pgx.ErrNoRows before any version value is read (storedText).
var storedVersionSQL = `
		select v.markdown, v.document_token, v.head_version, v.doc_update_row, v.renderer_version,
			s.version, s.row_xmin, ` + readableDocument + `
		from (` + documentStampSQL + `
		) s
		left join lateral (
			select markdown, document_token, head_version, doc_update_row, renderer_version
			from artifact_versions where artifact_id = $1
			order by number desc limit 1
		) v on true`

// readableDocument is whether the reader whose viewer $2 binds may read the document $1 names, by
// the rule of the document's own row (access.ItemOwnerClause): a document of a project the viewer
// sees, or a file of a conversation the viewer may read.
var readableDocument = access.ItemOwnerClause("null::text", "$1", "$2")

// storedText is the markdown and the document token the document's latest version stored, served
// as the document's own text without reading or rendering its tree, and whether they were served,
// for who: a document who may not read is pgx.ErrNoRows, as a document no row holds is to every
// caller, which reads the document's row first. A read not served here reads the document as it
// always has (readDocument). A read joined to a transaction is that transaction's writer's own and
// reads as it always has.
//
// They are the document's text only while the document still stands where that version was
// written from, which is all of this:
//
//   - No transaction of this process holds a fork of the document. A read joined to a
//     transaction reads as it always has: the fork is the document for that transaction, and a
//     joined read holding no fork still takes the document's owner lock and waits for an open
//     writer (joinRead), neither of which this read does.
//   - The document's stamp now is the head and the row the version was written at
//     (DocumentStamp): every writer of doc_updates either moves the head or rewrites the row
//     standing at it, a compaction's fold included, so one stamp names one state.
//   - A room of this process holds the document, standing at that head through its cursor
//     (roomBehindHead's comparison), and holds no update it has not stored: an update a browser's
//     keystroke left it is counted from its update observer until the room's persistence has
//     appended it (hasPendingUpdates, hasDurableAppend), and until then the room's tree is ahead
//     of everything durable. A document no room holds is read as it always has been, which is
//     the read that folds its stored history: that fold is where a history that cannot load is
//     found, and a server that served such a document from its version would never answer
//     DOCUMENT_UNLOADABLE for it until a write failed (api's
//     TestAHistoryThatCannotLoadReadsAsDocumentUnloadable,
//     TestArtifactListsGiveANearCapDocumentsStoredSizeWithoutRenderingIt). Such a read costs the
//     fold once a stamp all the same: the renderings it makes are cached by that stamp
//     (documentReads), and every later read of it is already answered without rendering.
//   - The version was written by a build whose renderer is this one (pmdoc.RendererVersion): a
//     rendering an older renderer made is not what this build would answer.
//
// What the room holds is weighed after the version is read, the narrower order: an update a room
// took between the two either reached the store, which moved the stamp this read already holds,
// or is still owed here. That the room is there at all is read first, as it was before a read was
// made for a reader: a document no room holds costs a reader that sees every document
// (readsEveryDocument) nothing before it goes the way it always went, and any other reader the one
// statement that reads the rule of the document's row, which refuses a document it may not read
// whether a room holds it or not.
func (s *Service) storedText(ctx context.Context, who model.Actor, artifactID string) (string, string, bool, error) {
	if _, joined := txFromContext(ctx); joined {
		return "", "", false, nil
	}
	// A failed room's recovery is waited out first, as every read of the document waits for it
	// (heldTree), so a read of a room this process is rebuilding is refused rather than served
	// from a version written before the failure.
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return "", "", false, err
	}
	live := s.srv.GetDoc(artifactID)
	if live == nil && readsEveryDocument(who) {
		return "", "", false, nil
	}
	var markdown, token, row *string
	var head *int64
	var renderer *int
	var stamp DocumentStamp
	var readable bool
	if err := s.store.Pool.QueryRow(ctx, storedVersionSQL, artifactID, refs.VisibilityArg(who)).Scan(
		&markdown, &token, &head, &row, &renderer, &stamp.Version, &stamp.Row, &readable,
	); err != nil {
		return "", "", false, fmt.Errorf("read the document's stored version: %w", err)
	}
	if !readable {
		return "", "", false, pgx.ErrNoRows
	}
	if live == nil {
		return "", "", false, nil
	}
	if markdown == nil || token == nil || head == nil || row == nil || renderer == nil {
		return "", "", false, nil
	}
	if *renderer != pmdoc.RendererVersion || *head != stamp.Version || *row != stamp.Row {
		return "", "", false, nil
	}
	// The counts are the room's state's, which outlives the room itself (roomState), so an
	// update a room that has since been evicted or failed still owes keeps its document off this
	// path.
	if s.hasPendingUpdates(artifactID) || s.hasDurableAppend(artifactID) {
		return "", "", false, nil
	}
	// A room evicted since has no instance for the document this read found (instanceOf), so its
	// cursor is nil and the read is not served.
	if cursor := s.cursorOf(live); cursor == nil || cursor.heldThrough() != *head {
		return "", "", false, nil
	}
	return *markdown, *token, true, nil
}

// readsEveryDocument reports whether who reads every document, so that no rule of a document's row
// can refuse it one: the server's own reader, and an open viewer of a deployment that holds no
// private project, which sees every project and every conversation, as every reader did before a
// project could be private. It is the api package's seesEveryObject, read for the documents alone.
func readsEveryDocument(who model.Actor) bool {
	viewer := who.Viewer
	return viewer != nil && (viewer.All || (viewer.Open && !viewer.AnyPrivate))
}
