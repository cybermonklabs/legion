package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Reader is the database one write reads every text of its source through, over the pool or
// transaction that write runs in and for the one source whose text it is: its kind and id decide
// the document a `#id` resolves against, its writer the items a short id resolves among, and the
// server's origins are the ones the mentions are read with. It is one write's object, not a cache:
// a caller builds it, reads every text of that source through it - a comment's body, an ask's
// question and each of its parts - and drops it, so the database answers once per kind for a
// write however many texts it holds, and what it answered is the one transaction's view. It is
// the text.Resolver every write of a text reads that text through (Mentions) before it plans what
// to index (PlanReplace, PlanItemIndex, PlanAskParts), so a write that holds two texts of one
// source - an ask's question and its parts - reads both through one.
//
// Every failure it meets reaching the database is logged here and answered as nothing resolved
// for that kind, and no call site handles one: the swallow lives in this one place because the
// rule is the Reader's own ("A write must never fail because of what its body cites", refs.go,
// CountedChanges), because answering per kind leaves the other kinds of one body resolved where a
// call site could only drop them all, and because it leaves text.Resolver's error return for a
// reader that has a reason to fail a write. A prefix that names nothing and one two items or
// more carry are no failures at all: the mention stays as the text wrote it, which is what
// text.Mentions does with an answer that leaves it out.
type Reader struct {
	// ctx is the write's context, held because text.Resolver's methods take none: the reader
	// lives for one write and is never stored.
	ctx      context.Context
	q        Queryer
	kind, id string
	origins  text.Origins
	// writer is the author of the source's text, whom its short ids resolve for (Items).
	writer model.Actor
	// seer is writer with the viewer its short ids are resolved under (seenBy): the writer as
	// given where it carries one, else the person it records as the read rule resolves them;
	// resolved at most once, the first time a short id is asked.
	seer *model.Actor

	// source is what the reader reads of its source and not of its writer, which every reader of
	// one write of that source shares (writtenBy), so the write asks the database once per kind
	// for it whichever writer's text asks.
	source *sourceReads
	// items is the whole id each (kind, scope, prefix) this reader resolved names, "" for one
	// that names nothing or two items or more, so a body writing the same short id twice is one
	// query.
	items map[prefixKey]string
}

// sourceReads is what the readers of one write of a source read of that source and not of its
// writer: the issues its bare keys name and the question map of its document.
type sourceReads struct {
	// issues is every key a reader was asked about, to whether it names an issue, so the
	// parts of one ask ask about a repeated key once.
	issues map[string]bool
	// questions is the question map of the source's document, loaded at most once (asked), and
	// nil for a source that belongs to no document or whose map could not be read.
	questions map[string]text.Ref
	asked     bool
	// pending is the document the source's text belongs to for a source whose row this
	// transaction has queued, which the caller hands over because no read of that row answers
	// it (NewReaderInScope); nil for a source whose row is stored, whose document the reader
	// reads with ScopeOf. Either way the read is made at most once, by Questions, and not at
	// all for a text that names no question block.
	pending *PendingDocument
}

// prefixKey is one short id as the resolver reads its candidates (readPrefixCandidates): the kind
// of item it names, the owner it is resolved within, and the prefix itself.
type prefixKey struct {
	kind   string
	scope  Scope
	prefix string
}

// NewReader is the Reader of one write of the text of the source fromKind/fromID, over q, the
// pool or transaction that write runs in, with origins the server its mentions are read with,
// for writer, the source's author: a comment's, a message's or an ask's author, and a document
// version's writer, the first author its row records (IndexedVersion). A short id the text writes
// resolves only among the items writer may see, through the predicate every read of the graph
// binds (readPrefixCandidates, VisibleKind): an answer that counted an item writer may not see -
// a prefix it shares with a visible one, left ambiguous - would tell the writer that item exists
// (R9, R19). The writer is the author the source records and not the credential that made the
// write, so every path that indexes the stored text again (RebuildAll, ReindexOwed) resolves it
// for the same person the write did. A source that records no author resolves for the zero actor,
// which proves nobody and so sees no item reserved for its author. An ask's answer part is the one
// text of a source that another person wrote, and it resolves for its answerer (writtenBy).
func NewReader(ctx context.Context, q Queryer, writer model.Actor, fromKind, fromID string, origins text.Origins) *Reader {
	return &Reader{ctx: ctx, q: q, writer: writer, kind: fromKind, id: fromID, origins: origins, source: &sourceReads{}}
}

// NewReaderInScope is NewReader for a source whose row the write's own transaction may have
// queued and not run yet (INFRA-636), so no read of that row answers the document its `#id`s
// resolve against: the writer hands that document over, as PendingDocument answers it from the
// fields the queued row will hold, and this reader reads it from there instead of from
// ScopeOf. A nil document is a source whose row is stored, whose scope this reader reads for
// itself as NewReader's does. Its short ids resolve for writer, the author the queued row will
// record, as NewReader's do.
//
// The document is no more read here than ScopeOf is: both are read by Questions, for a body
// that holds a `#` candidate and for no other, so a create whose text names no question block
// sends neither query (PendingDocument).
func NewReaderInScope(ctx context.Context, q Queryer, writer model.Actor, fromKind, fromID string, origins text.Origins, document *PendingDocument) *Reader {
	reader := NewReader(ctx, q, writer, fromKind, fromID, origins)
	reader.source.pending = document
	return reader
}

// Origins are the deployments whose dashboard URLs the reader reads as references: what a write
// that cuts the head of the same text an edge shows cuts it against (EditedTargets).
func (r *Reader) Origins() text.Origins {
	return r.origins
}

// writtenBy is the Reader of the same write for a text of r's source that writer wrote rather
// than r's own writer - an ask's answer, which its answerer wrote beside the asker's question
// (askPartPairs). Its short ids resolve for writer, and what does not depend on the writer - the
// issues a bare key names and the source's question map - it shares with r (sourceReads), so the
// write still asks the database once per kind for those.
func (r *Reader) writtenBy(writer model.Actor) *Reader {
	return &Reader{ctx: r.ctx, q: r.q, writer: writer, kind: r.kind, id: r.id, origins: r.origins, source: r.source}
}

// writerOf decodes the author a source's row records, a single-actor column, as the writer a
// Reader of that source resolves for (NewReader). SQL NULL and JSON null - a document version
// whose row records no author - are nobody recorded, the zero actor; any other value that does
// not decode as a model.Actor fails the read.
func writerOf(raw []byte) (model.Actor, error) {
	var writer model.Actor
	if raw == nil {
		return writer, nil
	}
	if err := json.Unmarshal(raw, &writer); err != nil {
		return model.Actor{}, fmt.Errorf("decode the writer: %w", err)
	}
	return writer, nil
}

// Mentions is the references body writes, read with the whole grammar (text.Mentions): the
// `dispatch://` and dashboard addresses it spells out, the issue keys it writes bare, the
// `#id`s naming the question blocks of the document the reader's source belongs to, and the
// ask, comment, message and block ids it writes short, each resolved to the whole id through
// the reader. Every indexing path reads its text through this call, so a comment, an ask, a
// message, a document, a block, an excerpt, the full rebuild (RebuildAll) and the
// re-index of old text (ReindexOwed) all store the same edges for the same text. Each mention
// keeps its byte offset, so a caller that places mentions in blocks passes the result through
// unchanged.
//
// A lookup that fails is not a mention that fails: the reader answers "nothing resolved" for
// that kind, and the text is indexed as it is written, the way a mention of a target that does
// not exist is (CountedChanges). What it does not do is save a write: a query that fails inside
// the write's own transaction aborts that transaction, so the write fails at its next statement
// whatever this answers.
func (r *Reader) Mentions(body string) []text.Located {
	located, err := text.Mentions(body, r.origins, r)
	if err != nil {
		// Unreachable: r answers every failure as nothing resolved and returns none. A build
		// that gives it a reason to fail must still not fail the write, so the text is indexed
		// as the grammar alone reads it.
		r.failed("resolve the references of a text", err)
		return text.ExtractAt(body, r.origins)
	}
	return located
}

// Issues reports which of keys names an issue that exists, so a bare key is stored as a
// reference only when it has a target. Mentions asks with the distinct keys of one body, and
// this reads the ones no reader of the write's source has been asked about yet (sourceReads),
// which is all of them for the first text the write reads and the new ones alone for every text
// after it.
func (r *Reader) Issues(keys []string) (map[string]bool, error) {
	source := r.source
	unknown := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, known := source.issues[key]; !known {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		found, err := resolveIssueTargets(r.ctx, r.q, unknown)
		if err != nil {
			r.failed("read the issues a text writes bare", err)
			return nil, nil
		}
		if source.issues == nil {
			source.issues = make(map[string]bool, len(unknown))
		}
		for _, key := range unknown {
			_, exists := found[key]
			source.issues[key] = exists
		}
	}
	answer := make(map[string]bool, len(keys))
	for _, key := range keys {
		answer[key] = source.issues[key]
	}
	return answer, nil
}

// Questions is the question map of the document the source's text belongs to (the scope, then
// QuestionMap), loaded at most once for the readers of the write's source (sourceReads) and not
// at all unless a body holds a `#` candidate, which is when text.Mentions asks. A source that
// belongs to no document - a direct message, a reply whose thread names none - has no question,
// and a text on an issue but on no document resolves against the issue's spec, which the scope
// answers with. Both are read for no person (scopeOf, questionMap): the index answers no reader,
// and the edge a `#id` makes is one of its own source, which every read of the graph holds to
// the predicates of both its ends.
//
// Both reads the scope costs are paid here and nowhere earlier: a body with no `#` candidate
// never asks, so a create whose text names no question block sends neither the query a queued
// row's scope takes nor the one a stored row's does.
func (r *Reader) Questions() (map[string]text.Ref, error) {
	source := r.source
	if source.asked {
		return source.questions, nil
	}
	source.asked = true
	document, err := r.scope()
	if err != nil {
		r.failed("read the document a text belongs to", err)
		return nil, nil
	}
	if document == "" {
		return nil, nil
	}
	questions, err := questionMap(r.ctx, r.q, nil, document)
	if err != nil {
		r.failed("read the question blocks a text's `#id` names", err)
		return nil, nil
	}
	source.questions = questions
	return source.questions, nil
}

// scope is the document the source's text belongs to: the one the writer handed over for a row
// this transaction has only queued (NewReaderInScope), which no read of that row can answer
// yet, else the one the source's stored row answers for no person (scopeOf).
func (r *Reader) scope() (string, error) {
	if pending := r.source.pending; pending != nil {
		return pending.Scope(r.ctx, r.q)
	}
	return scopeOf(r.ctx, r.q, nil, r.kind, r.id)
}

// Items is the whole id of each short id the text writes, resolved within the owner the reference
// itself names and not across the whole kind, among the items the reader's writer may see
// (resolveItem, which reads them as ResolvePrefix does, for the writer and as the zero Viewer: the
// index writes the graph every authenticated caller reads, so it reads no direct message's first
// line). An item the writer may not see is no candidate, so a prefix it alone carries names
// nothing and one it shares with a visible item names that item, as ResolvePrefix answers the
// writer. A prefix that names nothing, one two items or more carry, and one whose owner no Scope
// holds are all left out of the answer, which leaves the mention as the text wrote it.
func (r *Reader) Items(prefixes []text.Prefix) (map[int]string, error) {
	resolved := make(map[int]string, len(prefixes))
	for index, prefix := range prefixes {
		kind, scope, id, ok := PrefixScope(prefix)
		if !ok {
			continue
		}
		key := prefixKey{kind: kind, scope: scope, prefix: id}
		whole, known := r.items[key]
		if !known {
			var err error
			if whole, err = r.resolveItem(key); err != nil {
				r.failed("resolve the short ids a text writes", err)
				return resolved, nil
			}
			if r.items == nil {
				r.items = make(map[prefixKey]string, len(prefixes))
			}
			r.items[key] = whole
		}
		if whole != "" {
			resolved[index] = whole
		}
	}
	return resolved, nil
}

// resolveItem is the whole id key names for the reader's writer, "" where it names nothing or two
// items or more. It is ResolvePrefix's two halves read apart: the candidates as that function
// reads them (readPrefixCandidates) and the item among them by value (pickPrefixCandidate), so its
// one error is a candidate reader's failure reaching the database, and no refusal that quotes the
// prefix a body wrote is built on the way to the log (Reader.failed).
func (r *Reader) resolveItem(key prefixKey) (string, error) {
	writer, err := r.seenBy()
	if err != nil {
		return "", err
	}
	found, err := readPrefixCandidates(r.ctx, r.q, writer, key.kind, key.scope, key.prefix, Viewer{})
	if err != nil {
		return "", err
	}
	if candidate, ok := pickPrefixCandidate(key.kind, key.prefix, found); ok {
		return candidate.ID, nil
	}
	return "", nil
}

// seenBy is the reader's writer as the visibility predicate binds it (VisibilityArg). A writer
// the caller resolved carries its viewer, and is used as it is. A writer read from a stored row,
// the author a source records, carries none, and with no viewer the predicate sees nothing
// (VisibilityArg's closed viewer), so no short id would resolve: such a writer is read as the
// person it records sees now (access.RecordedViewer: their member kind and the projects whose
// access names them, no credential's list), which is what every later rebuild of the same text
// resolves it as too. A session author names its owner, and one that records no person is no
// person: it sees what a credential naming nobody sees.
func (r *Reader) seenBy() (model.Actor, error) {
	if r.seer != nil {
		return *r.seer, nil
	}
	writer := r.writer
	if writer.Viewer == nil {
		viewer, err := access.RecordedViewer(r.ctx, r.q, writer)
		if err != nil {
			return model.Actor{}, err
		}
		writer.Viewer = &viewer
	}
	r.seer = &writer
	return writer, nil
}

// PrefixScope is what one short id asks ResolvePrefix: the kind the reference names, the owner
// it names as a Scope, and the prefix itself. A block's is matched among the blocks of the
// document the block is of, at the version the reference pins; an item's within the owner its
// reference names, the issue or the project document it is written on
// (`dispatch://<PROJECT>/artifact/<slug>/ask/<id>`, whose slug the parsed reference carries in
// Document). Both callers that resolve one short id at a time ask it: the reader that rewrites a
// body's short ids as it is indexed, and the `?to=` route that answers a node under its whole
// id. GET /api/v1/reference-targets asks it too, for each reference of a set, and gives the
// answers to ResolvePrefixes, the batched second copy of the queries of ResolvePrefix, which
// shares its selection rule (choosePrefixCandidate).
//
// ok is false for a reference whose owner no Scope holds, which is then left as the text wrote
// it: an item of no owner at all, and a block of an ask, a comment or a message, whose blocks
// are numbered (`b2`) and are no id a prefix can shorten.
func PrefixScope(prefix text.Prefix) (kind string, scope Scope, id string, ok bool) {
	ref := prefix.Ref
	if prefix.Block {
		if ref.Item != "" {
			return "", Scope{}, "", false
		}
		document := ref
		document.Kind, document.Block, document.Pin = "artifact", "", 0
		return "block", Scope{Document: text.TargetID(document), Version: ref.Pin}, ref.Block, true
	}
	kind = ref.Kind
	if kind == "block" {
		kind = ref.Item
	}
	switch {
	case ref.IssueKey != "":
		return kind, Scope{IssueKey: ref.IssueKey}, ref.ID, true
	case ref.Project != "" && ref.Document != "":
		document := text.Ref{Kind: "artifact", Project: ref.Project, ID: ref.Document}
		return kind, Scope{Document: text.TargetID(document)}, ref.ID, true
	}
	return "", Scope{}, "", false
}

// failed logs a failure reaching the database, which leaves the text indexed as it is written.
func (r *Reader) failed(what string, err error) {
	slog.Error("dispatch: could not "+what+"; it is indexed as it is written",
		"kind", r.kind, "id", r.id, "error", err)
}
