package refs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// askScope is the document an ask (aliased k) belongs to: the one its block is on, the one it is
// anchored to, or the one that owns it, else its issue's spec (ks, askScopeJoin). The document a
// block is on is the one whose fragment holds it in an issue room, whose tree artifact keys the
// ask's row (AskBlockDocumentSQL), never the tree.
var askScope = `coalesce(` + AskBlockDocumentSQL("k") + `, (k.anchor->>'artifact_id')::uuid, k.artifact_id, ks.id)`

const askScopeJoin = `left join artifacts ks on ks.issue_key = k.issue_key and ks.is_primary`

// commentOwnScope is the document a comment (aliased c) names itself: the one it is anchored to,
// the one that owns it, or, for a reply to an ask, that ask's scope (commentOwnScopeJoin). A
// comment naming none is in its parent's scope when it is a reply in a thread, else in its issue's
// spec.
var commentOwnScope = `coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id, ` + askScope + `)`

const commentOwnScopeJoin = `left join asks k on k.id = c.ask_id ` + askScopeJoin

// commentScopeSelect reads, of one comment (aliased c), the three things its scope is decided
// from (scopeStep): the document it names itself, the comment it replies to, and its issue's
// spec (cs, commentSpecJoin). A comment's row and a pending comment's fields are read through
// this one select, so the rules are stated once (pendingScope).
var commentScopeSelect = `select ` + commentOwnScope + `::text, c.reply_to::text, cs.id::text`

const commentSpecJoin = `left join artifacts cs on cs.issue_key = c.issue_key and cs.is_primary`

// messageScopeJoin is the document an issue message (aliased m) belongs to: its issue's spec.
const messageScopeJoin = `join artifacts s on s.issue_key = m.issue_key and s.is_primary`

// ScopeOf is the id of the document the text of one reference source belongs to, the document a
// `#id` in that text resolves against (QuestionMap), or "" when it belongs to none:
//
//   - a document version (kind "artifact"): that document;
//   - an ask: the document its block is on, else the one it is anchored to, else the one that owns
//     it, else its issue's spec;
//   - a comment: the document it is anchored to, else the one that owns it, else for a reply to an
//     ask that ask's scope, else for a reply in a thread its parent's scope (so its thread root's),
//     else its issue's spec;
//   - an issue message: its issue's spec;
//   - a direct message (no issue), any other kind, and an id that is not a uuid: none.
//
// An id written in one of uuid.Parse's other spellings is read, and answered, as the canonical
// one (canonicalUUID).
//
// A reply whose chain of parents loops before any of them names a document has none. ScopeSources
// is its inverse, and the two must agree on every shape above (TestScopeOfAndScopeSourcesAgree);
// pendingScope answers the same scope before the source's row is written, and must agree with it
// on every shape too (TestPendingScopeAgreesWithScopeOf).
//
// It answers for who. An ask, a comment or a message who may not see belongs to no document, as
// one no row holds does, and so does one whose walk passes through a comment who may not see or
// ends at a document who may not see: a document's id is answered only to who may see it. A
// document's own id is answered as it is given, read or not, as an id no row holds is, and
// QuestionMap answers it nothing where who may not see it.
func ScopeOf(ctx context.Context, q Queryer, who model.Actor, kind, id string) (string, error) {
	return scopeOf(ctx, q, &who, kind, id)
}

// scopeOf is ScopeOf for who, or for no person at all where who is nil: the index-time
// resolver's read (Reader.Questions), which answers no reader.
func scopeOf(ctx context.Context, q Queryer, who *model.Actor, kind, id string) (string, error) {
	id, ok := canonicalUUID(id)
	if !ok {
		return "", nil
	}
	// Each statement is built before the call that sends it: the predicates append the acting
	// credential to args as they render, and Go does not order the read of args.values among
	// the arguments of one call against a call inside another of them.
	var args queryArgs
	key := args.param(id)
	var query string
	switch kind {
	case "artifact":
		return id, nil
	case "ask":
		query = `
			select ` + askScope + `::text from asks k ` + askScopeJoin + `
			left join artifacts s on s.id = ` + askScope + `
			where k.id = ` + key + args.andVisible(who, "ask", `k.id::text`) + args.andVisible(who, "artifact", `s.ref_key`)
	case "comment":
		return commentScope(ctx, q, who, id)
	case "message":
		query = `
			select s.id::text from messages m ` + messageScopeJoin + `
			where m.id = ` + key + args.andVisible(who, "message", `m.id::text`) + args.andVisible(who, "artifact", `s.ref_key`)
	default:
		return "", nil
	}
	return scopeRow(q.QueryRow(ctx, query, args.values...))
}

// commentScope walks a comment's chain of parents up to the first comment that names a document
// (commentOwnScope), or to the thread's root, whose issue's spec it then is. A comment who may
// not see ends the walk as a comment no row holds does, and a document who may not see is
// answered as none.
func commentScope(ctx context.Context, q Queryer, who *model.Actor, id string) (string, error) {
	seen := map[string]struct{}{}
	for {
		if _, loops := seen[id]; loops {
			return "", nil
		}
		seen[id] = struct{}{}
		own, parent, spec, err := commentScopeRow(ctx, q, who, id)
		if err != nil {
			return "", err
		}
		scope, next, resolved := scopeStep(own, parent, spec)
		if resolved {
			return scope, nil
		}
		id = next
	}
}

// commentScopeRow is what one stored comment's row says about its scope (commentScopeSelect),
// each of the three nil where the row holds none. A comment that does not exist, or that who may
// not see, reads as a row naming none, which has no scope; a document of the row's that who may
// not see reads as the empty document, which scopeStep answers as none.
func commentScopeRow(ctx context.Context, q Queryer, who *model.Actor, id string) (own, parent, spec *string, err error) {
	var ownVisible, specVisible bool
	var args queryArgs
	key := args.param(id)
	query := commentScopeSelect + `, ` + args.visible(who, "artifact", `own.ref_key`) + `, ` + args.visible(who, "artifact", `cs.ref_key`) + `
		from comments c ` + commentOwnScopeJoin + ` ` + commentSpecJoin + `
		left join artifacts own on own.id = ` + commentOwnScope + `
		where c.id = ` + key + args.andVisible(who, "comment", `c.id::text`)
	err = q.QueryRow(ctx, query, args.values...).Scan(&own, &parent, &spec, &ownVisible, &specVisible)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the scope of comment %s: %w", id, err)
	}
	none := ""
	if own != nil && !ownVisible {
		own = &none
	}
	if spec != nil && !specVisible {
		spec = &none
	}
	return own, parent, spec, nil
}

// scopeStep is the one rule a comment's scope follows, over what its row says (commentScopeRow):
// the document it names itself, else its parent's scope, else its issue's spec, else none. A
// comment that names no document and replies to one is not resolved here: next is the parent
// whose scope it then is, which commentScope walks to and pendingScope reads from the stored
// parent.
func scopeStep(own, parent, spec *string) (scope, next string, resolved bool) {
	switch {
	case own != nil:
		return *own, "", true
	case parent != nil:
		return "", *parent, false
	case spec != nil:
		return *spec, "", true
	}
	return "", "", true
}

func scopeRow(row pgx.Row) (string, error) {
	var scope *string
	if err := row.Scan(&scope); errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("read a reference source's scope: %w", err)
	}
	if scope == nil {
		return "", nil
	}
	return *scope, nil
}

// PendingSource is one reference source whose row the transaction writing it has queued and not
// run yet (INFRA-636: a route's write phase sends every statement at once), named by the fields
// of that row ScopeOf reads. A row nothing has written answers no read, so the writer hands
// these over and pendingScope answers the document its text's `#id`s resolve against. Each
// field is what the queued insert binds to the column of that name, nil for a column it leaves
// null.
type PendingSource struct {
	// IssueKey is the issue the row is written on (issue_key), whose spec is the scope of a
	// source naming no document of its own. Nil for a source on no issue: an ask or a comment
	// of a project document, a direct message.
	IssueKey *string
	// ArtifactID is the document that owns the row (artifact_id).
	ArtifactID *string
	// AnchorArtifactID is the document the row is anchored to (anchor->>'artifact_id').
	AnchorArtifactID *string
	// BlockArtifactID is the document an ask's block is on (block_artifact_id): for an ask of an
	// issue room, the room's tree artifact, whose block BlockID names (AskBlockDocumentSQL).
	BlockArtifactID *string
	// BlockID is the block an ask is the ask of (block_id), which names the document whose
	// fragment of an issue room holds it.
	BlockID *string
	// AskID is the ask a comment replies to (ask_id), whose own scope a reply to an ask takes.
	AskID *string
	// ReplyTo is the comment a comment replies to (reply_to), whose scope a reply in a thread
	// takes.
	ReplyTo *string
}

// pendingAskScopeQuery is askScope over the fields a queued ask row will hold rather than over
// the row: the same expression and the same join of its issue's spec, so the order an ask's
// scope coalesces in is stated once (askScope). Its block id is among them, since the document an
// ask's block is on, in an issue room, is read off it (AskBlockDocumentSQL).
var pendingAskScopeQuery = `
	with k (block_artifact_id, block_id, anchor, artifact_id, issue_key) as (
		select $1::uuid, $5::text, jsonb_build_object('artifact_id', $2::text), $3::uuid, $4::text
	)
	select ` + askScope + `::text from k ` + askScopeJoin

// pendingCommentScopeQuery is commentScopeSelect over the fields a queued comment row will hold:
// the document it names itself, the comment it replies to and its issue's spec, read as a stored
// comment's are (commentScopeRow), with the ask of a reply to an ask joined from the asks table,
// which holds it.
var pendingCommentScopeQuery = `
	with c (anchor, artifact_id, ask_id, reply_to, issue_key) as (
		select jsonb_build_object('artifact_id', $1::text), $2::uuid, $3::uuid, $4::uuid, $5::text
	)
	` + commentScopeSelect + ` from c ` + commentOwnScopeJoin + ` ` + commentSpecJoin

// pendingMessageScopeQuery is the scope of a queued issue message: its issue's spec, joined as a
// stored message's is (messageScopeJoin). A message on no issue joins nothing and has none.
const pendingMessageScopeQuery = `
	with m (issue_key) as (select $1::text)
	select s.id::text from m ` + messageScopeJoin

// pendingScope is ScopeOf for a source of kind kind whose row is queued and not written yet: the
// document its text belongs to, answered from the fields that row will hold (PendingSource), or
// "" when it belongs to none. The kinds are ScopeOf's own, "ask", "comment" and "message"; a
// document version, which every writer of one has, and any other kind have none here.
//
// It is the same answer ScopeOf gives once the row is durable, on every shape
// (TestPendingScopeAgreesWithScopeOf): the two read one set of SQL expressions, and what a
// pending source does not carry - its issue's spec, the scope of the ask or of the parent
// comment a reply takes - is read from the stored rows that hold it.
//
// It is read through PendingDocument, which reads it only for a text that needs it: every
// writer of a queued source goes through that, so a body naming no question block costs this
// read nothing.
func pendingScope(ctx context.Context, q Queryer, kind string, source PendingSource) (string, error) {
	switch kind {
	case "ask":
		return scopeRow(q.QueryRow(ctx, pendingAskScopeQuery,
			source.BlockArtifactID, source.AnchorArtifactID, source.ArtifactID, source.IssueKey, source.BlockID))
	case "comment":
		var own, parent, spec *string
		if err := q.QueryRow(ctx, pendingCommentScopeQuery,
			source.AnchorArtifactID, source.ArtifactID, source.AskID, source.ReplyTo, source.IssueKey,
		).Scan(&own, &parent, &spec); err != nil {
			return "", fmt.Errorf("read the scope of a pending comment: %w", err)
		}
		scope, next, resolved := scopeStep(own, parent, spec)
		if resolved {
			return scope, nil
		}
		return commentScope(ctx, q, nil, next)
	case "message":
		return scopeRow(q.QueryRow(ctx, pendingMessageScopeQuery, source.IssueKey))
	}
	return "", nil
}

// PendingDocument is the document a source whose row the writing transaction has queued belongs
// to, read when a text of that source first asks for it and not before (Reader.Questions, which
// asks only for a body holding a `#` candidate). A create whose body names no question block -
// most comments, replies, messages, asks and approval requests - costs the read nothing, and a
// direct message, whose scope is always none, never sends the query that says so.
//
// One of these is the document of one queued row, however many texts that row's writer indexes:
// an ask's question and its parts are read through readers of their own (NewReaderInScope), and
// sharing this between them reads the scope once for the write. It is one write's object, as a
// Reader is, and is used from one goroutine.
type PendingDocument struct {
	kind     string
	source   PendingSource
	document string
	err      error
	read     bool
}

// NewPendingDocument is the document of a source of kind kind - "ask", "comment" or "message",
// the kinds pendingScope answers for - whose row source says this transaction has queued.
func NewPendingDocument(kind string, source PendingSource) *PendingDocument {
	return &PendingDocument{kind: kind, source: source}
}

// PendingAskDocument is NewPendingDocument for an ask whose insert a transaction has queued,
// over the ask as that row will stand: every route that opens one - an issue's or a document's
// ask, an approval request, the ask settlement opens for a new question block - names its scope
// through this one call, so none of them states the fields of that row for itself.
func PendingAskDocument(ask model.Ask) *PendingDocument {
	source := PendingSource{
		IssueKey:        ask.IssueKey,
		ArtifactID:      ask.ArtifactID,
		BlockArtifactID: ask.BlockArtifactID,
		BlockID:         ask.BlockID,
	}
	if ask.Anchor != nil {
		source.AnchorArtifactID = &ask.Anchor.ArtifactID
	}
	return NewPendingDocument("ask", source)
}

// Scope is the document, read over q on the first call and answered from that read after, its
// failure included: a reader that swallowed one (Reader.failed) has already left the text
// indexed as it was written, and a second reader of the same source must not pay the read again
// to be told the same thing.
func (d *PendingDocument) Scope(ctx context.Context, q Queryer) (string, error) {
	if !d.read {
		d.read = true
		d.document, d.err = pendingScope(ctx, q, d.kind, d.source)
	}
	return d.document, d.err
}

// commentSeedJoins are the sides every arm of the seed below reads to answer commentSeedScope:
// the ask a comment replies to and that ask's scope (commentOwnScopeJoin), and the primary
// document - the spec - of the comment's own issue (commentSpecJoin, the join a stored
// comment's own scope is read through). Each is a left join on a unique key, asks' primary key
// and artifacts_one_primary, the one primary document an issue has
// (store/migrations/0001_init.up.sql:24), so none of them answers a comment twice.
const commentSeedJoins = commentOwnScopeJoin + `
		` + commentSpecJoin

// commentSeedScope is what every arm of the seed tests, and the whole of what makes a comment one
// of the walk's roots: it names the document itself (commentOwnScope), or it names none and is a
// thread root of the issue whose spec the document is. It is scopeStep's rule written as one
// predicate, over the same three things that rule reads. An arm narrows which comments are read,
// never which of the read ones are answered.
var commentSeedScope = `(` + commentOwnScope + ` = $1
		       or (` + commentOwnScope + ` is null and c.reply_to is null and cs.id = $1))`

// scopedAsksTerm is scopeSourcesQuery's scoped_asks, every ask whose scope (askScope) is the
// document $1: read from the asks whose block is keyed by $1 or by the issue room $1 is edited in
// (DocumentRoomSQL), the asks anchored to it or owning it, and those of the issue whose spec it
// is (spec), each then held to its scope.
var scopedAsksTerm = `
	scoped_asks as (
		select k.id from asks k ` + askScopeJoin + `
		where (coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) in ($1, ` + DocumentRoomSQL("$1") + `)
		       or k.artifact_id = $1
		       or k.issue_key in (select issue_key from spec))
		  and ` + askScope + ` = $1
	),`

// scopeSourcesQuery lists, by kind and id, every reference source whose ScopeOf is the document $1.
// The comments are found from the thread roots and the comments that name the document
// (commentOwnScope) down through the replies that name none, which is ScopeOf's walk run the
// other way; a loop of replies naming no document is reached from neither.
//
// The seed is one arm per way a comment reaches the document, each arm an equality one index of
// comments serves: comments_anchor_artifact and comments_artifact_id
// (store/migrations/0032_refs_provenance.up.sql:47-48), comments_ask_id
// (store/migrations/0006_ask_threads.up.sql:3) and comments_issue_created
// (store/migrations/0040_comment_query_indexes.up.sql:2). Written as one select the four are
// `or`-ed, and the whole of them `and`-ed with commentSeedScope's coalesce over further columns,
// so no arm is an index condition and the planner reads the whole table: on the deployment
// measured for INFRA-988 that seed scanned 494,000 comments, 253,611 shared buffers, to answer
// 69 rows, and this statement, run once per document the block index re-indexes
// (docs/block_index.go:858), was 28.1% of the database's time. One arm per index answers the same
// rows there in 1.3 ms where the one select took 1,074 ms.
//
// The arms answer exactly what the one select answered, because each carries the whole of
// commentSeedScope: their union is `(anchored or owned or replying to a scoped ask or in the
// spec's issue) and commentSeedScope`, which is what the one select tested
// (TestScopeSourcesSeedArmsAnswerTheOneSeed). The narrowing is redundant either way - a comment
// commentSeedScope holds names the document through one of those four columns, or through the
// scope of the ask it replies to, which is a scoped_asks row - and is written out so that each
// read is keyed.
//
// Nothing is answered twice. The arms and the recursive term are joined by `union`, not
// `union all`, so a comment two arms match - anchored to the document and in its issue - enters
// the working table once, and a recursive union drops a row already in the result rather than
// recursing on it again. That is why the walk down the replies cannot loop or repeat either: a
// comment has one parent (comments.reply_to), and a cycle of replies naming no document reaches a
// comment already answered, where the iteration stops with an empty working table.
var scopeSourcesQuery = `
	with recursive
	spec as (
		select issue_key from artifacts where id = $1 and is_primary
	),` + scopedAsksTerm + `
	scoped_comments(id) as (
		select c.id from comments c ` + commentSeedJoins + `
		where (c.anchor->>'artifact_id')::uuid = $1 and ` + commentSeedScope + `
		union
		select c.id from comments c ` + commentSeedJoins + `
		where c.artifact_id = $1 and ` + commentSeedScope + `
		union
		select c.id from scoped_asks ka
		join comments c on c.ask_id = ka.id ` + commentSeedJoins + `
		where ` + commentSeedScope + `
		union
		select c.id from spec
		join comments c on c.issue_key = spec.issue_key ` + commentSeedJoins + `
		where ` + commentSeedScope + `
		union
		select c.id from scoped_comments p
		join comments c on c.reply_to = p.id ` + commentOwnScopeJoin + `
		where ` + commentOwnScope + ` is null
	)
	select kind, id::text from (
		select 'artifact' as kind, id from artifacts where id = $1
		union all
		select 'ask', id from scoped_asks
		union all
		select 'comment', id from scoped_comments
		union all
		select 'message', m.id from messages m where m.issue_key in (select issue_key from spec)
	) sources
	order by kind, id`

// ScopeSources is the inverse of ScopeOf: every reference source, by kind and id, whose text
// belongs to the document doc, in kind then id order. A document that does not exist, or an id
// that is not a uuid, has none; one written in another of uuid.Parse's spellings is read as the
// canonical one (canonicalUUID).
func ScopeSources(ctx context.Context, q Queryer, doc string) ([]Target, error) {
	doc, ok := canonicalUUID(doc)
	if !ok {
		return []Target{}, nil
	}
	sources, err := scanTargets(q.Query(ctx, scopeSourcesQuery, doc))
	if err != nil {
		return nil, fmt.Errorf("list the sources in the scope of document %s: %w", doc, err)
	}
	return sources, nil
}

// QuestionMap is the question map of the document doc: each question block id its latest version
// declares (a `:::ask{#id …}` block outside fenced and indented code, questionBlocks) to the
// reference a `dispatch://` address of its ask makes, as text.Extract reads that address, so a
// `#id` edge is stored as that address's edge is. The ask is the row settlement made for the block
// on doc, or, for a copied block, the ask its block node's copied_from tuple names, which
// settlement states (docs' claimCopies), or, for a block whose node holds none - a copy no
// settlement of this build has claimed yet, in a version the release before wrote - the ask its
// opening line names as copied_from. A block with neither (not settled yet) is absent. It reads
// the latest version's text (sourceText) line by line for its blocks (questionBlocks), never
// parsing the document into a tree, and takes no lock.
//
// The map is the document's as the reading transaction leaves it: the version and the block asks
// it has queued and not written yet are read from what those statements write (Pending), since
// no read of the tables answers for them. A `#id` naming a block of the very version being
// written, or one whose ask the same transaction opens - settlement makes the ask of every new
// question block of an edit in one transaction - resolves to the ask it will name once the write
// phase has run, and so does a copy whose opening line in that version names its source: the
// line comes before its node's copied_from tuple, which the same write phase claims after. An ask
// of a block of an issue room is queued under the room's key, its block_artifact_id, so the asks
// queued under the room doc is edited in are read beside doc's own (pendingQuestionAsks).
//
// It answers for who. A document who may not see has no question, as one no row holds has none,
// so not even its block ids are answered, and an ask who may not see is no question of it, nor is
// a copied ask whose own document who may not see. The asks this transaction has queued are the
// writer's own, read by the index for no person.
func QuestionMap(ctx context.Context, q Queryer, who model.Actor, doc string) (map[string]text.Ref, error) {
	return questionMap(ctx, q, &who, doc)
}

// questionMap is QuestionMap for who, or for no person at all where who is nil: the index-time
// resolver's read (Reader.Questions), which answers no reader.
func questionMap(ctx context.Context, q Queryer, who *model.Actor, doc string) (map[string]text.Ref, error) {
	// Each statement is built before the call that sends it, as scopeOf's are.
	if who != nil {
		var args queryArgs
		key := args.param(doc)
		query := `select ` + args.visible(who, "artifact", `a.ref_key`) + ` from artifacts a where a.id = ` + key + `::uuid`
		var visible bool
		err := q.QueryRow(ctx, query, args.values...).Scan(&visible)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !visible {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read whether the reader may see document %s: %w", doc, err)
		}
	}
	pending := pendingOf(ctx)
	markdown, queued := "", false
	if pending != nil {
		markdown, queued = pending.PendingMarkdown(doc)
	}
	if !queued {
		stored, _, found, err := sourceText(ctx, q, "artifact", doc)
		if err != nil {
			return nil, fmt.Errorf("read the latest version of document %s: %w", doc, err)
		}
		if !found {
			return nil, nil
		}
		markdown = stored
	}
	blocks := questionBlocks(markdown)
	if len(blocks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(blocks))
	for index, block := range blocks {
		ids[index] = block.id
	}
	queuedBlocks, queuedAsks, queuedIssues := []string{}, []string{}, []*string{}
	if pending != nil {
		asks, err := pendingQuestionAsks(ctx, q, pending, doc)
		if err != nil {
			return nil, err
		}
		for _, ask := range asks {
			queuedBlocks = append(queuedBlocks, ask.BlockID)
			queuedAsks = append(queuedAsks, ask.ID)
			queuedIssues = append(queuedIssues, ask.IssueKey)
		}
	}
	// The queued rows are unioned into the one read, so a block's ask is read as the same three
	// columns and answered as the same reference whether its row is stored or queued
	// (scanQuestions); a block the transaction has queued a write of is read from that write
	// alone, which is the row as it will stand.
	settled := queryArgs{values: []any{doc, ids, queuedBlocks, queuedAsks, queuedIssues}}
	query := `
		with queued (block_id, id, issue_key) as (
			select * from unnest($3::text[], $4::uuid[], $5::text[])
		)
		select b.block_id, b.id::text, b.issue_key, a.project_key, a.slug
		from (
			select k.block_id, k.id, k.issue_key from asks k
			where ` + AsksOfDocumentSQL("k", "$1::uuid") + ` and k.block_id = any($2::text[])
			  and k.block_id not in (select block_id from queued)` + settled.andVisible(who, "ask", `k.id::text`) + `
			union all
			select q.block_id, q.id, q.issue_key from queued q where q.block_id = any($2::text[])
		) b join artifacts a on a.id = $1`
	questions, err := scanQuestions(q.Query(ctx, query, settled.values...))
	if err != nil {
		return nil, fmt.Errorf("read the questions of document %s: %w", doc, err)
	}
	copiedBy := map[string][]string{}
	sources := []string{}
	// A copy names its source on its block node's copied_from tuple, which settlement states once
	// the version's blocks are indexed (docs' claimCopies), and on its opening line, which every
	// version this build writes keeps in step with that tuple (docs' keptAskState). A version this
	// transaction has queued is read before its own claim is written, so where it names a source
	// on the line, that line is the newer of the two, and the tuple is read only for a copy whose
	// line names none (unread). A stored version's line may be one the release before wrote, so
	// there the tuple is read first.
	var unowned, unread []string
	for _, block := range blocks {
		if _, own := questions[block.id]; own {
			continue
		}
		unowned = append(unowned, block.id)
		if !queued || block.copiedFrom == "" {
			unread = append(unread, block.id)
		}
	}
	if len(unowned) == 0 {
		return questions, nil
	}
	var stated map[string]BlockCopiedFrom
	if len(unread) > 0 {
		if stated, err = copiedFromTuples(ctx, q, doc, unread); err != nil {
			return nil, fmt.Errorf("read the copied questions of document %s: %w", doc, err)
		}
	}
	for _, block := range blocks {
		if _, own := questions[block.id]; own {
			continue
		}
		source := block.copiedFrom
		if copied, held := stated[block.id]; held {
			source = copied.Ask
		}
		if source == "" {
			continue
		}
		if _, listed := copiedBy[source]; !listed {
			sources = append(sources, source)
		}
		copiedBy[source] = append(copiedBy[source], block.id)
	}
	if len(sources) == 0 {
		return questions, nil
	}
	var copiedArgs queryArgs
	query = `
		select k.id::text, k.id::text, k.issue_key, a.project_key, coalesce(a.slug, '')
		from asks k
		left join artifacts a on a.id = coalesce(` + AskBlockDocumentSQL("k") + `, (k.anchor->>'artifact_id')::uuid, k.artifact_id)
		where k.id = any(` + copiedArgs.param(sources) + `::uuid[])` +
		copiedArgs.andVisible(who, "ask", `k.id::text`) + copiedArgs.andVisible(who, "artifact", `a.ref_key`)
	copied, err := scanQuestions(q.Query(ctx, query, copiedArgs.values...))
	if err != nil {
		return nil, fmt.Errorf("read the copied questions of document %s: %w", doc, err)
	}
	for source, ref := range copied {
		for _, blockID := range copiedBy[source] {
			questions[blockID] = ref
		}
	}
	return questions, nil
}

// pendingQuestionAsks is the block asks the reading transaction has queued that doc's question
// map reads beside the stored ones (Pending.PendingBlockAsks): those queued under doc itself, and
// those queued under the issue room doc is edited in (DocumentRoomSQL), the room's own key, which
// every ask of a block of the room is queued under, its block_artifact_id (docs' recordAskBlock).
// The room's block ids are unique across its fragments, and the read the map makes keeps the asks
// of doc's own question blocks alone. A document that is its own room reads its room in the same
// statement and nothing beside its own.
func pendingQuestionAsks(ctx context.Context, q Queryer, pending Pending, doc string) ([]PendingBlockAsk, error) {
	asks := pending.PendingBlockAsks(doc)
	var room string
	if err := q.QueryRow(ctx, `select (`+DocumentRoomSQL("$1::uuid")+`)::text`, doc).Scan(&room); err != nil {
		return nil, fmt.Errorf("read the room document %s is edited in: %w", doc, err)
	}
	if room == doc {
		return asks, nil
	}
	own := make(map[string]struct{}, len(asks))
	for _, ask := range asks {
		own[ask.BlockID] = struct{}{}
	}
	for _, ask := range pending.PendingBlockAsks(room) {
		if _, held := own[ask.BlockID]; !held {
			asks = append(asks, ask)
		}
	}
	return asks, nil
}

// BlockCopiedFrom is the copied_from a block's node holds as it stands now: the node the
// document's block index names for the block (document_blocks.node_address), and the ask that
// node's copied_from tuple names.
type BlockCopiedFrom struct {
	Node, Ask string
}

// copiedFromTuples is, for each of blocks, block ids of document doc, whose block node the block
// index of the room doc is edited in holds (DocumentRoomSQL: doc itself, or the tree artifact of
// the issue room that keys the rows of every fragment's blocks, whose ids are unique across the
// room), the copied_from that node holds as it stands now (readCopiedFrom), keyed by block id. A
// node that holds no standing copied_from is absent, so a version the release before wrote, whose
// copy no settlement of this build has claimed, is read from its line instead (questionMap).
func copiedFromTuples(ctx context.Context, q Queryer, doc string, blocks []string) (map[string]BlockCopiedFrom, error) {
	return readCopiedFrom(ctx, q, doc, DocumentRoomSQL("$1::uuid"), ` and d.block_id = any($2::text[])`, blocks)
}

// DocumentCopiedFrom is copiedFromTuples for every block of the room doc, a document that is its
// own room or an issue room's tree artifact, whose row in the room's block index names a node,
// whatever the block's type now: the copied_from each such node holds as it stands now, keyed by
// block id, with the node the row names. Settlement reads it once, in its read phase, before its
// own block index is written, to retract the copied_from of each block that is no longer an ask
// block (docs' claimCopies): one a person retyped, or one the settlement's version drops, whose
// row and node the read still names. A block of a retired document of an issue room is left out:
// its fragment left the room, so no settlement claims it, and every pass over the room keeps its
// row and its node as the retirement left them (BlockIndexing.retained).
func DocumentCopiedFrom(ctx context.Context, q Queryer, doc string) (map[string]BlockCopiedFrom, error) {
	return readCopiedFrom(ctx, q, doc, "$1::uuid", ` and not `+retiredDocumentSQL(graphstore.RoomBlockRowSQL("d.artifact_id", "d.block_id")))
}

// readCopiedFrom is the one read of copiedFromTuples and DocumentCopiedFrom: each document_blocks
// row d keyed by keyed, the artifact SQL over doc at $1, that names a node, cut by narrow, a
// further condition on d that binds args from $2 on, with the node it names and the ask that
// node's newest copied_from tuple names while that tuple asserts (graphstore.TupleCopiedFrom,
// graphstore.CurrentTupleRead), keyed by block id.
func readCopiedFrom(ctx context.Context, q Queryer, doc, keyed, narrow string, args ...any) (map[string]BlockCopiedFrom, error) {
	rows, err := q.Query(ctx, `
		select d.block_id, d.node_address, c.to_id
		from document_blocks d
		cross join lateral (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = d.node_kind and t.from_id = d.node_address and t.kind = '`+graphstore.TupleCopiedFrom+`'
			  and t.to_kind = '`+graphstore.KindAsk+`'`)+`
		) c
		where d.artifact_id = `+keyed+` and d.node_address is not null and c.asserted`+narrow+`
	`, append([]any{doc}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stated := map[string]BlockCopiedFrom{}
	for rows.Next() {
		var block, node, ask string
		if err := rows.Scan(&block, &node, &ask); err != nil {
			return nil, err
		}
		if parsed, err := uuid.Parse(ask); err == nil {
			stated[block] = BlockCopiedFrom{Node: node, Ask: parsed.String()}
		}
	}
	return stated, rows.Err()
}

// AskRef is the `dispatch://` address of one ask a question map holds, as every writer of an
// ask's address writes it (itemRef): under its issue, or under the project document that owns
// it. It is "" for an ask no address names - one on an agent conversation's document - which is
// still the ask the `#id` names, stored as the edge its id makes.
func AskRef(ref text.Ref) string {
	if ref.IssueKey != "" {
		return itemRef("ask", &ref.IssueKey, "", "", ref.ID)
	}
	return itemRef("ask", nil, ref.Project, ref.Document, ref.ID)
}

// scanQuestions reads rows of (key, ask id, issue key, project key, document slug) into each
// ask's reference by key: an ask on an issue as `dispatch://<KEY>/ask/<id>` reads, one on a
// project document as `dispatch://<PROJECT>/artifact/<slug>/ask/<id>` does (itemRef writes
// both, and the document of the second is the owner its reference names). An ask whose document
// is an agent conversation's has no address (itemRef answers none); it is still the ask the
// `#id` names, and the edge stored for it is the one its id makes.
func scanQuestions(rows pgx.Rows, err error) (map[string]text.Ref, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]text.Ref{}
	for rows.Next() {
		var key, id, slug string
		var issueKey, project *string
		if err := rows.Scan(&key, &id, &issueKey, &project, &slug); err != nil {
			return nil, err
		}
		ref := text.Ref{Kind: "ask", ID: id}
		if issueKey != nil {
			ref.IssueKey = *issueKey
		} else if project != nil {
			ref.Project, ref.Document = *project, slug
		}
		found[key] = ref
	}
	return found, rows.Err()
}

// questionBlock is one question block a document's text declares: its id and, for a copy, the
// uuid of the ask settlement wrote into it as copied_from, which the copy's block node's
// copied_from tuple holds too and the question map reads first (copiedFromTuples).
type questionBlock struct {
	id, copiedFrom string
}

// canonicalUUID is id in the one spelling the graph and Postgres read it by, and false for a
// value that is no uuid at all. uuid.Parse reads `urn:uuid:<uuid>`, the braced spelling and 32
// hex digits beside the canonical one, and every id this package stores, binds and answers with
// is the canonical one: Postgres refuses `urn:uuid:` on a uuid column, so a value bound as
// written would fail the read where the id it names answers it, and a document answered back in
// another spelling would name a node nothing else writes.
func canonicalUUID(id string) (string, bool) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}
