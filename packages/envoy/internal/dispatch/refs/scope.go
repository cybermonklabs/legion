package refs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// askScope is the document an ask (aliased k) belongs to: the one its block is on, the one it is
// anchored to, or the one that owns it, else its issue's spec (ks, askScopeJoin).
const askScope = `coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid, k.artifact_id, ks.id)`

const askScopeJoin = `left join artifacts ks on ks.issue_key = k.issue_key and ks.is_primary`

// commentOwnScope is the document a comment (aliased c) names itself: the one it is anchored to,
// the one that owns it, or, for a reply to an ask, that ask's scope (commentOwnScopeJoin). A
// comment naming none is in its parent's scope when it is a reply in a thread, else in its issue's
// spec.
const commentOwnScope = `coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id, ` + askScope + `)`

const commentOwnScopeJoin = `left join asks k on k.id = c.ask_id ` + askScopeJoin

// commentScopeSelect reads, of one comment (aliased c), the three things its scope is decided
// from (scopeStep): the document it names itself, the comment it replies to, and its issue's
// spec (cs, commentSpecJoin). A comment's row and a pending comment's fields are read through
// this one select, so the rules are stated once (pendingScope).
const commentScopeSelect = `select ` + commentOwnScope + `::text, c.reply_to::text, cs.id::text`

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
	// BlockArtifactID is the document an ask's block is on (block_artifact_id).
	BlockArtifactID *string
	// AskID is the ask a comment replies to (ask_id), whose own scope a reply to an ask takes.
	AskID *string
	// ReplyTo is the comment a comment replies to (reply_to), whose scope a reply in a thread
	// takes.
	ReplyTo *string
}

// pendingAskScopeQuery is askScope over the fields a queued ask row will hold rather than over
// the row: the same expression and the same join of its issue's spec, so the order an ask's
// scope coalesces in is stated once (askScope).
const pendingAskScopeQuery = `
	with k (block_artifact_id, anchor, artifact_id, issue_key) as (
		select $1::uuid, jsonb_build_object('artifact_id', $2::text), $3::uuid, $4::text
	)
	select ` + askScope + `::text from k ` + askScopeJoin

// pendingCommentScopeQuery is commentScopeSelect over the fields a queued comment row will hold:
// the document it names itself, the comment it replies to and its issue's spec, read as a stored
// comment's are (commentScopeRow), with the ask of a reply to an ask joined from the asks table,
// which holds it.
const pendingCommentScopeQuery = `
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
			source.BlockArtifactID, source.AnchorArtifactID, source.ArtifactID, source.IssueKey))
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
const commentSeedScope = `(` + commentOwnScope + ` = $1
		       or (` + commentOwnScope + ` is null and c.reply_to is null and cs.id = $1))`

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
const scopeSourcesQuery = `
	with recursive
	spec as (
		select issue_key from artifacts where id = $1 and is_primary
	),
	scoped_asks as (
		select k.id from asks k ` + askScopeJoin + `
		where (coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = $1
		       or k.artifact_id = $1
		       or k.issue_key in (select issue_key from spec))
		  and ` + askScope + ` = $1
	),
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
// phase has run.
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
		for _, ask := range pending.PendingBlockAsks(doc) {
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
			where k.block_artifact_id = $1 and k.block_id = any($2::text[])
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
	var unowned []string
	for _, block := range blocks {
		if _, own := questions[block.id]; !own {
			unowned = append(unowned, block.id)
		}
	}
	if len(unowned) == 0 {
		return questions, nil
	}
	stated, err := copiedFromTuples(ctx, q, doc, unowned)
	if err != nil {
		return nil, fmt.Errorf("read the copied questions of document %s: %w", doc, err)
	}
	for _, block := range blocks {
		if _, own := questions[block.id]; own {
			continue
		}
		source, held := stated[block.id]
		if !held {
			source = block.copiedFrom
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
		left join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid, k.artifact_id)
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

// copiedFromTuples is, for each of blocks, block ids of document doc, whose block node the
// document's block index holds (document_blocks), the ask that node's copied_from tuple names as it
// stands now (graphstore.TupleCopiedFrom, graphstore.CurrentTupleRead), keyed by block id. A node
// that holds no standing copied_from is absent, so a version the release before wrote, whose copy
// no settlement of this build has claimed, is read from its line instead (questionMap).
func copiedFromTuples(ctx context.Context, q Queryer, doc string, blocks []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select d.block_id, c.to_id
		from document_blocks d
		cross join lateral (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = d.node_kind and t.from_id = d.node_address and t.kind = '`+graphstore.TupleCopiedFrom+`'
			  and t.to_kind = '`+graphstore.KindAsk+`'`)+`
		) c
		where d.artifact_id = $1::uuid and d.block_id = any($2::text[]) and d.node_address is not null and c.asserted
	`, doc, blocks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stated := map[string]string{}
	for rows.Next() {
		var block, ask string
		if err := rows.Scan(&block, &ask); err != nil {
			return nil, err
		}
		if parsed, err := uuid.Parse(ask); err == nil {
			stated[block] = parsed.String()
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

// questionBlocks is every question block markdown declares, in document order, the first of each
// id: each typed block named ask that holds an id, where pmdoc's parser opens one. A fenced
// example of a directive is code to pmdoc, and a line of front matter is no markdown to it, so a
// `copied_from` either carries names no question of this document.
//
// It reads the document's blocks line by line as goldmark (github.com/yuin/goldmark v1.8.6) and
// pmdoc's own block parsers read them, and builds no tree: pmdoc.ParseRendering of an 848 kB
// document took 224 ms, and this read of it 5 ms, measured on one machine. The CLI and the web
// read ask openers with a TypeScript twin of this reader (readAskBlocks, packages/contracts'
// dispatch-block-reader.ts). The rules:
//
//   - A line first goes past the containers it continues: a quote's `>` and one column after it, a
//     list item's content columns, and a footnote definition's four columns. A blank line continues
//     a list item and a footnote definition, but not a quote.
//   - A block opens only within three columns of those containers. A line four columns or more past
//     them continues the paragraph before it, or else opens indented code. So the later line of an
//     inline code span, which keeps its spaces, opens no block there.
//   - A container opens within three columns too: a `>`, a list marker (`-`, `*`, `+`, or one to
//     nine digits and `.` or `)`) followed by a space, a tab or the line's end, or a footnote label
//     (footnoteLabelEnd). Every space and tab after a label belongs to it.
//   - An empty list item, or one numbered other than 1, opens no list where it would interrupt a
//     paragraph, or a block that the browser editor's parser holds open (interruptsOpenBlock).
//   - A list stays open at a line that reaches its empty last item's content column, with no blank
//     line before it (pmdoc's emptyItemGuard).
//   - A fence is three or more backticks or tildes, and a backtick fence's info string holds no
//     backtick. A run of the same character at least as long closes it, within three columns of its
//     containers and with only spaces and tabs after it. A container that ends ends the fence in it.
//   - A typed block's opening line is read by pmdoc.ParseTypedDirectiveOpen. The block closes at a
//     line of as many colons or more, whatever block inside it the line would otherwise continue.
//   - A line that a container does not continue, and that opens no block, continues the paragraph
//     in that container (a lazy line), and the container stays open.
//   - No line of the front matter the document opens with opens a block (frontMatterEnd). After a
//     front-matter opener that no later line closes, no quote, list, footnote definition or typed
//     block opens at the document's level (pmdoc's frontmatterAttempt).
//
// HTML blocks, which the server refuses in a write, are not read.
func questionBlocks(markdown string) []questionBlock {
	lines := strings.Split(markdown, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSuffix(line, "\r")
	}
	start, unclosed := frontMatterEnd(lines)
	scan := blockScan{
		lines:               lines[start:],
		unclosedFrontMatter: unclosed,
		blocks:              []questionBlock{},
		seen:                map[string]struct{}{},
	}
	document := &scanBlock{kind: scanDocument}
	var line scanLine
	for index, text := range scan.lines {
		scan.index = index
		line = scanLine{text: text}
		scan.readLine(document, &line)
	}
	return scan.blocks
}

// frontMatterEnd is the index of the first of lines past the front matter they open with, as
// pmdoc reads it (parseFrontmatterBlock): a first line that is `---` and any spaces or tabs opens
// it, and the first later line that is the same closes it. pmdoc keeps it as one node, word for
// word, so no line of it opens a block. It is 0 where the lines open with none, or with an opener
// no line closes, which the renderer never writes: a rule opening a document is written `***`.
// unclosed reports such an opener (questionBlocks says what it changes).
func frontMatterEnd(lines []string) (end int, unclosed bool) {
	if !frontMatterFence(lines[0]) {
		return 0, false
	}
	for index := 1; index < len(lines); index++ {
		if frontMatterFence(lines[index]) {
			return index + 1, false
		}
	}
	return 0, true
}

// frontMatterFence reports whether line opens or closes front matter: `---` and any spaces or tabs.
func frontMatterFence(line string) bool {
	return strings.TrimRight(line, " \t") == "---"
}

// blockScan is what questionBlocks carries from line to line, as goldmark's parser context
// carries it.
type blockScan struct {
	// lines is the markdown's lines past its front matter, and index the one being read.
	lines []string
	index int
	// opened is the blocks open, outermost first, as goldmark's pc.OpenedBlocks(). The document
	// is not among them.
	opened []*scanBlock
	// emptyItemWithBlankLines is goldmark's emptyListItemWithBlankLines, and skipList its
	// skipListParserKey: a list item closed at a sibling's marker.
	emptyItemWithBlankLines, skipList bool
	// unclosedFrontMatter is that the markdown opens with a front-matter opener that no later line
	// closes (frontMatterEnd).
	unclosedFrontMatter bool
	// blocks is each question block found so far, and seen holds their ids.
	blocks []questionBlock
	seen   map[string]struct{}
}

// scanKind is the kind of a block that blockScan holds open.
type scanKind uint8

const (
	scanDocument scanKind = iota
	scanQuote
	scanFootnote
	// scanLeaf is a block of one line: a heading, a rule or a directive the server refuses.
	scanLeaf
	scanParagraph
	// scanCode is indented code.
	scanCode
	scanList
	scanItem
	scanTyped
	// scanFence is fenced code.
	scanFence
)

// scanBlock is one block as blockScan holds it open.
type scanBlock struct {
	kind   scanKind
	parent *scanBlock
	// previous is the block opened before it in its parent (goldmark's PreviousSibling), and last
	// the last block opened in it, of children.
	previous, last *scanBlock
	children       int
	// lastLine is the index of a paragraph's last line, and of an indented code block's last line
	// that is not blank.
	lastLine int
	// table is whether a line of a paragraph after its first is a table's delimiter row.
	table bool
	// blankBefore is whether a blank line, or the document's start, stands before an indented code
	// block.
	blankBefore bool
	// char is the character a list continues only with (its bullet, or its ordered delimiter), or
	// a fence's character.
	char byte
	// run is how many colons a typed block opened with, or how many characters a fence's run holds:
	// only a line of as many or more closes it.
	run int
	// offset is the columns a list item's content stands past its container's, or a typed block's
	// opening line, which it takes off its lines.
	offset int
}

// scanContinuation is what a block's continuation does with a line, as goldmark's State.
type scanContinuation uint8

const (
	scanClose scanContinuation = iota
	scanContinue
	scanChildren
)

// scanOpened is what blockScan.openBlocks did with a line.
type scanOpened uint8

const (
	openedNone scanOpened = iota
	openedBlock
	continuedParagraph
)

// scanLine is one line of markdown, read from the column that the containers around a block have
// taken. A tab reaches the next multiple of four columns.
type scanLine struct {
	text string
	// pos is the index of the first character not taken, and start the column it starts at.
	// column is the column taken so far: start, or more where a tab at pos is partly taken.
	pos, start, column int
	// ahead is the index of the first character at or after pos that is no space or tab, and
	// aheadColumn the column it starts at, where measured: indent reads them again only once pos
	// passes ahead, so each open block's continuation does not count the same spaces again.
	measured           bool
	ahead, aheadColumn int
}

// nextColumn is the column after char, which starts at column.
func nextColumn(column int, char byte) int {
	if char == '\t' {
		return column + 4 - column%4
	}
	return column + 1
}

// columnAt is the column that the character at index, pos or after it, starts at.
func (line *scanLine) columnAt(index int) int {
	column := line.start
	for at := line.pos; at < index; at++ {
		column = nextColumn(column, line.text[at])
	}
	return column
}

// afterSpaces is the index of the first character from index on that is no space or tab.
func (line *scanLine) afterSpaces(index int) int {
	for index < len(line.text) && (line.text[index] == ' ' || line.text[index] == '\t') {
		index++
	}
	return index
}

// spacesAt is the columns of the spaces and tabs that start at index, pos or after it.
func (line *scanLine) spacesAt(index int) int {
	return line.columnAt(line.afterSpaces(index)) - line.columnAt(index)
}

// indent is the columns of space and tab from the column taken, and the index of the character
// after them.
func (line *scanLine) indent() (width, at int) {
	if !line.measured || line.pos > line.ahead {
		line.ahead = line.afterSpaces(line.pos)
		line.aheadColumn = line.columnAt(line.ahead)
		line.measured = true
	}
	return line.aheadColumn - line.column, line.ahead
}

// blank reports whether nothing but spaces and tabs is left.
func (line *scanLine) blank() bool {
	_, at := line.indent()
	return at == len(line.text)
}

// skip takes columns columns of the spaces and tabs ahead, a tab partly where it spans more.
func (line *scanLine) skip(columns int) {
	line.column += columns
	for line.pos < len(line.text) {
		next := nextColumn(line.start, line.text[line.pos])
		if next > line.column {
			return
		}
		line.pos, line.start = line.pos+1, next
	}
}

// advanceTo takes the line up to the character at index, pos or after it.
func (line *scanLine) advanceTo(index int) {
	line.start = line.columnAt(index)
	line.pos, line.column = index, line.start
}

// takeAll takes the rest of the line, as a block of the rest of the line does. It counts no
// columns: nothing is left to measure.
func (line *scanLine) takeAll() {
	line.pos, line.start = len(line.text), line.column
}

// lastOpened is the block opened last, nil where none is open.
func (scan *blockScan) lastOpened() *scanBlock {
	if len(scan.opened) == 0 {
		return nil
	}
	return scan.opened[len(scan.opened)-1]
}

// readLine takes one line through the open blocks, then opens the blocks it opens: the body of
// goldmark's parseBlocks loop.
func (scan *blockScan) readLine(document *scanBlock, line *scanLine) {
	if len(scan.opened) == 0 {
		if !line.blank() {
			scan.openBlocks(document, line)
		}
		return
	}
	last := len(scan.opened) - 1
	for index := range last + 1 {
		block := scan.opened[index]
		// openBlocks continues a paragraph, where the line opens no block that interrupts it.
		if block.kind != scanParagraph {
			continuation := scan.continueBlock(block, line)
			if continuation == scanChildren && index == last {
				scan.openBlocks(block, line)
				return
			}
			if continuation != scanClose {
				continue
			}
		}
		// The block closes here, or the line continues the paragraph opened last as a lazy line.
		parent := document
		if index > 0 {
			parent = scan.opened[index-1]
		}
		if scan.openBlocks(parent, line) != continuedParagraph {
			scan.opened = append(scan.opened[:index], scan.opened[last+1:]...)
		}
		return
	}
}

// openBlocks opens the blocks line opens inside parent, else continues the paragraph opened last
// with it: goldmark's openBlocks.
func (scan *blockScan) openBlocks(parent *scanBlock, line *scanLine) scanOpened {
	last := scan.lastOpened()
	continuable := last != nil && last.kind == scanParagraph
	result := openedNone
	for into := parent; ; {
		width, at := line.indent()
		if at == len(line.text) {
			break
		}
		block, children := scan.openBlock(into, line, width, at, continuable && result == openedNone)
		if block == nil {
			break
		}
		into.children++
		into.last = block
		scan.opened = append(scan.opened, block)
		result = openedBlock
		if !children {
			break
		}
		into = block
	}
	if result == openedNone && continuable && !line.blank() {
		if tableDelimiterRow(line.text[line.pos:]) {
			last.table = true
		}
		last.lastLine = scan.index
		return continuedParagraph
	}
	return result
}

// block is a new block of kind, empty, inside parent.
func (scan *blockScan) block(kind scanKind, parent *scanBlock) *scanBlock {
	return &scanBlock{kind: kind, parent: parent, previous: parent.last}
}

// leaf is a block of one line inside parent, which takes the rest of line.
func (scan *blockScan) leaf(parent *scanBlock, line *scanLine) *scanBlock {
	line.takeAll()
	return scan.block(scanLeaf, parent)
}

// frontMatterHolds reports whether a front-matter opener that no later line closes keeps a
// container from opening in parent (frontMatterEnd).
func (scan *blockScan) frontMatterHolds(parent *scanBlock) bool {
	return scan.unclosedFrontMatter && parent.kind == scanDocument
}

// openBlock is the block line opens inside parent, its text at index at and width columns past
// parent's, and children is whether that block holds the blocks the rest of the line opens. It
// tries goldmark's block parsers and Dispatch's own in goldmark's order of priority: setext heading
// (100), thematic break (200), list (300), list item (400), indented code (500), ATX heading (600),
// fenced code (700), blockquote (800), Dispatch's refused directive (900) and typed block (950),
// footnote definition (999), paragraph (1000). Where interrupting, the line would interrupt the
// paragraph opened last, which indented code and a paragraph do not (CanInterruptParagraph). No
// block but indented code opens four columns or more past parent (CanAcceptIndentedLine).
func (scan *blockScan) openBlock(parent *scanBlock, line *scanLine, width, at int, interrupting bool) (*scanBlock, bool) {
	if width > 3 {
		if interrupting {
			return nil, false
		}
		line.skip(4)
		code := scan.block(scanCode, parent)
		code.lastLine = scan.index
		code.blankBefore = scan.index == 0 || blankText(scan.lines[scan.index-1])
		return code, false
	}
	text := line.text[at:]
	last := scan.lastOpened()
	switch char := text[0]; char {
	case '-', '=', '*', '_', '+', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if (char == '-' || char == '=') && last != nil && last.kind == scanParagraph && last.parent == parent && setextUnderline(text) {
			return scan.leaf(parent, line), false
		}
		if (char == '-' || char == '*' || char == '_') && thematicBreak(text) {
			return scan.leaf(parent, line), false
		}
		if char == '=' || char == '_' {
			break
		}
		if list := scan.openList(parent, line, last); list != nil {
			return list, true
		}
		if item, children := scan.openItem(parent, line); item != nil {
			return item, children
		}
	case '#':
		if run := runOf(text, '#'); run <= 6 && (run == len(text) || text[run] == ' ' || text[run] == '\t') {
			return scan.leaf(parent, line), false
		}
	case '`', '~':
		run := runOf(text, char)
		// A backtick fence's info string holds no backtick: such a line is inline code.
		if run < 3 || char == '`' && strings.IndexByte(text[run:], '`') >= 0 {
			break
		}
		line.takeAll()
		fence := scan.block(scanFence, parent)
		fence.char, fence.run = char, run
		return fence, false
	case '>':
		if !scan.frontMatterHolds(parent) && quoteMarker(line) {
			return scan.block(scanQuote, parent), true
		}
	case ':':
		if block, ok := scan.openDirective(parent, line, width, text); ok {
			return block, block.kind == scanTyped
		}
	case '[':
		label, ok := footnoteLabelEnd(line.text, at)
		if !ok || scan.frontMatterHolds(parent) {
			break
		}
		// Every space and tab after the label's `]:` belongs to the definition's prefix, so a block
		// written on the same line starts at column zero inside it: pmdoc's
		// footnoteDefinitionParser.Open skips them, where goldmark leaves them to that block. A line
		// holding nothing but whitespace after the label keeps it.
		if content := line.afterSpaces(label); content < len(line.text) {
			label = content
		}
		line.advanceTo(label)
		return scan.block(scanFootnote, parent), true
	}
	if interrupting {
		return nil, false
	}
	line.takeAll()
	paragraph := scan.block(scanParagraph, parent)
	paragraph.lastLine = scan.index
	return paragraph, false
}

// openList is the list line opens inside parent, nil where it opens none: goldmark's list parser,
// held off an empty item, or one numbered other than a lone `1`, that would interrupt a paragraph
// or an indented code block (pmdoc's emptyItemGuard, stricter than goldmark, which lets `01.`
// interrupt a paragraph and anything interrupt code). last is the block opened last.
func (scan *blockScan) openList(parent *scanBlock, line *scanLine, last *scanBlock) *scanBlock {
	if last != nil && last.kind == scanList || scan.skipList {
		scan.skipList = false
		return nil
	}
	marker, ok := listMarker(line)
	if !ok || scan.frontMatterHolds(parent) {
		return nil
	}
	if marker.empty || marker.number != "" && marker.number != "1" {
		if last != nil && last.kind == scanParagraph && last.parent == parent || scan.interruptsOpenBlock(parent) {
			return nil
		}
	}
	scan.emptyItemWithBlankLines = false
	list := scan.block(scanList, parent)
	list.char = marker.char
	return list
}

// openItem is the list item line opens inside parent, a list, and whether it holds the blocks the
// rest of the line opens: goldmark's list item parser, the spaces and tabs after the marker
// measured from the column they stand at (pmdoc's listItemColumns).
func (scan *blockScan) openItem(parent *scanBlock, line *scanLine) (*scanBlock, bool) {
	if parent.kind != scanList {
		return nil, false
	}
	previous := 0
	if parent.last != nil && parent.last.kind == scanItem {
		previous = parent.last.offset
	}
	marker, ok := listMarker(line)
	if !ok || marker.indent-previous > 3 {
		return nil, false
	}
	scan.emptyItemWithBlankLines = false
	// The content's columns past the marker: one, where the item opens with indented code.
	spacing := line.spacesAt(marker.end)
	if marker.empty || spacing > 4 {
		spacing = 1
	}
	item := scan.block(scanItem, parent)
	item.offset = line.columnAt(marker.end) - line.column + spacing
	if marker.empty {
		return item, false
	}
	line.advanceTo(marker.end)
	line.skip(spacing)
	return item, true
}

// openDirective is the block a line of directive syntax, text, opens inside parent, width columns
// past it, and ok is false where it opens none. A line that pmdoc refuses
// (unsupportedDirectiveReason) opens a leaf; a stored document holds none. A typed block's opening
// line is read by the one parser of that line (pmdoc.ParseTypedDirectiveOpen), so an id written
// either way (`#id` and `id="…"`, pmdoc.TypedBlockIDAttribute) is read as the document's own parse
// reads it. An ask block's id and copied_from are recorded (record).
func (scan *blockScan) openDirective(parent *scanBlock, line *scanLine, width int, text string) (*scanBlock, bool) {
	if !strings.HasPrefix(text, ":::") {
		// A leaf directive (`::name`), and a text directive (`:name{…}`) opening the line.
		if strings.HasPrefix(text, "::") && len(text) > 2 && directiveNameByte(text[2]) ||
			len(text) > 1 && directiveNameByte(text[1]) && strings.Contains(text, "{") {
			return scan.leaf(parent, line), true
		}
		return nil, false
	}
	if text == ":::" {
		return nil, false
	}
	name, attrs, ok := pmdoc.ParseTypedDirectiveOpen(text)
	if !ok {
		return scan.leaf(parent, line), true
	}
	if scan.frontMatterHolds(parent) {
		return nil, false
	}
	if name == "ask" {
		scan.record(attrs)
	}
	line.takeAll()
	typed := scan.block(scanTyped, parent)
	typed.run, typed.offset = runOf(text, ':'), width
	return typed, true
}

// record adds the question block an ask block's opening line names by attrs, the first of each
// id: the block's id and, where the line carries one, the uuid of the ask its `copied_from`
// names. An ask block with no id names no question.
func (scan *blockScan) record(attrs pmdoc.Attrs) {
	id, _ := attrs[pmdoc.BlockIDAttr].(string)
	if id == "" {
		return
	}
	if _, repeated := scan.seen[id]; repeated {
		return
	}
	scan.seen[id] = struct{}{}
	block := questionBlock{id: id}
	// Only the one attribute carries an ask: every other value on the line - the state, the
	// asker, the question's own wording - is left unread.
	if copied, ok := attrs["copied_from"].(string); ok {
		if parsed, err := uuid.Parse(copied); err == nil {
			block.copiedFrom = parsed.String()
		}
	}
	scan.blocks = append(scan.blocks, block)
}

// interruptsOpenBlock is pmdoc's interruptsOpenBlock: whether a list opening in parent interrupts
// a block the browser editor's parser holds open there, other than a paragraph parent holds: a
// paragraph that ends on the line before, reached across the containers the line opens first and
// forming no table, or an indented code block with only blank lines after it, unless that code
// stands right after a list, or right after a quote with no blank line between them. Where the
// code is parent's own last block, a line of quote markers after it leaves it open too.
func (scan *blockScan) interruptsOpenBlock(parent *scanBlock) bool {
	previous := parent.last
	opensContainers := previous == nil
	for node := parent; previous == nil && node != nil && node.kind != scanDocument; node = node.parent {
		previous = node.previous
	}
	if previous == nil {
		return false
	}
	switch previous.kind {
	case scanParagraph:
		return opensContainers && !previous.table && scan.index-previous.lastLine <= 1
	case scanCode:
		if before := previous.previous; before != nil && (before.kind == scanList || before.kind == scanQuote && !previous.blankBefore) {
			return false
		}
		for _, row := range scan.lines[previous.lastLine+1 : scan.index] {
			if !blankText(row) && (opensContainers || strings.Trim(row, " \t>") != "") {
				return false
			}
		}
		return true
	}
	return false
}

// continueBlock is what block, open, does with line: goldmark's Continue of each block parser.
func (scan *blockScan) continueBlock(block *scanBlock, line *scanLine) scanContinuation {
	switch block.kind {
	case scanQuote:
		if quoteMarker(line) {
			return scanChildren
		}
		return scanClose
	case scanList:
		return scan.continueList(block, line)
	case scanItem:
		return scan.continueItem(block, line)
	case scanFootnote:
		if line.blank() {
			return scanChildren
		}
		if width, _ := line.indent(); width < 4 {
			return scanClose
		}
		line.skip(4)
		return scanChildren
	case scanTyped:
		width, at := line.indent()
		// A line of at least the fence's colons closes the block, whatever block inside it the line
		// would otherwise continue (pmdoc's typedDirectiveParser.Continue).
		if width < 4 && at < len(line.text) && fenceColons(line.text[at:]) >= block.run {
			line.takeAll()
			return scanClose
		}
		if at < len(line.text) {
			line.skip(min(width, block.offset))
		}
		return scanChildren
	case scanFence:
		width, at := line.indent()
		if run := runOf(line.text[at:], block.char); width < 4 && run >= block.run && blankText(line.text[at+run:]) {
			line.takeAll()
			return scanClose
		}
		line.takeAll()
		return scanContinue
	case scanCode:
		if line.blank() {
			return scanContinue
		}
		if width, _ := line.indent(); width < 4 {
			return scanClose
		}
		block.lastLine = scan.index
		line.takeAll()
		return scanContinue
	}
	// A heading, a rule or a refused directive holds one line.
	return scanClose
}

// continueList is goldmark's listParser.Continue, which pmdoc's emptyItemGuard keeps from closing
// the list at a line that holds its empty last item's content: a line that reaches the item's
// content column, a list marker there included, with no blank line before it.
func (scan *blockScan) continueList(list *scanBlock, line *scanLine) scanContinuation {
	last := list.last
	if line.blank() {
		if last == nil || last.children == 0 {
			scan.emptyItemWithBlankLines = true
		}
		return scanChildren
	}
	width, at := line.indent()
	continuation := scan.continueListAt(list, line, width, at)
	if continuation == scanClose && last != nil && last.kind == scanItem && last.children == 0 && width >= last.offset &&
		scan.index > 0 && strings.Trim(scan.lines[scan.index-1], " \t>") != "" {
		return scanChildren
	}
	return continuation
}

// continueListAt is goldmark's listParser.Continue of a line that is not blank, its text at index
// at and width columns past the list's container.
func (scan *blockScan) continueListAt(list *scanBlock, line *scanLine, width, at int) scanContinuation {
	last := list.last
	lastIsEmpty := last == nil || last.children == 0
	offset := 0
	if last != nil && last.kind == scanItem {
		offset = last.offset
	}
	if width < offset || lastIsEmpty {
		if marker, ok := listMarker(line); ok && width < 4 && marker.indent-offset < 4 {
			if marker.char != list.char {
				return scanClose
			}
			// A thematic break outranks a list item, unless it underlines a paragraph as a heading.
			text := line.text[at:]
			opened := scan.lastOpened()
			heading := opened != nil && opened.kind == scanParagraph && setextUnderline(text)
			if thematicBreak(text) && !heading {
				return scanClose
			}
			return scanChildren
		}
		if !lastIsEmpty {
			return scanClose
		}
	}
	if lastIsEmpty && width < offset {
		return scanClose
	}
	// Content cannot follow an empty item and blank lines in the same list.
	if scan.emptyItemWithBlankLines {
		return scanClose
	}
	return scanChildren
}

// continueItem is goldmark's listItemParser.Continue, a blank line taking no more than the item's
// columns (pmdoc's listItemColumns).
func (scan *blockScan) continueItem(item *scanBlock, line *scanLine) scanContinuation {
	width, _ := line.indent()
	if line.blank() {
		line.skip(min(width, item.offset))
		return scanChildren
	}
	isEmpty := item.children == 0 && scan.emptyItemWithBlankLines
	if (isEmpty || width < item.offset) && width < 4 {
		if _, ok := listMarker(line); ok {
			scan.skipList = true
			return scanClose
		}
		if !isEmpty {
			return scanClose
		}
	}
	line.skip(min(width, item.offset))
	return scanChildren
}

// scanListMarker is a list item's marker opening what is left of a line.
type scanListMarker struct {
	// indent is the columns of indentation before it, and end the index after it.
	indent, end int
	// char is the character a list continues only with: the bullet, or the ordered delimiter.
	char byte
	// number is an ordered marker's number as written, "" for a bullet.
	number string
	// empty is that nothing but spaces and tabs follows it.
	empty bool
}

// listMarker is the list item's marker that opens what is left of line, ok false where none does:
// goldmark's parseListItem, its indentation measured in the columns its tabs span (pmdoc's
// tabIndented).
func listMarker(line *scanLine) (marker scanListMarker, ok bool) {
	width, at := line.indent()
	if width > 3 || at == len(line.text) {
		return scanListMarker{}, false
	}
	text := line.text
	end := at
	if first := text[at]; first == '-' || first == '*' || first == '+' {
		end++
	} else {
		for end < len(text) && end-at <= 9 && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if end == at || end-at > 9 || end == len(text) || text[end] != '.' && text[end] != ')' {
			return scanListMarker{}, false
		}
		marker.number = text[at:end]
		end++
	}
	if end < len(text) && text[end] != ' ' && text[end] != '\t' {
		return scanListMarker{}, false
	}
	marker.indent, marker.end, marker.char, marker.empty = width, end, text[end-1], blankText(text[end:])
	return marker, true
}

// quoteMarker takes a quote's marker off what is left of line, as goldmark's blockquote parser
// does: a `>` within three columns, then one column of the space or tab after it. It reports
// whether the line holds one, and takes nothing where it does not.
func quoteMarker(line *scanLine) bool {
	width, at := line.indent()
	if width > 3 || at == len(line.text) || line.text[at] != '>' {
		return false
	}
	line.advanceTo(at + 1)
	if at+1 < len(line.text) && (line.text[at+1] == ' ' || line.text[at+1] == '\t') {
		line.skip(1)
	}
	return true
}

// footnoteLabelEnd is the index after the `]:` of the footnote label at index at of text, as
// goldmark's footnote parser reads one (extension/footnote.go, through util.FindClosure) and pmdoc
// keeps it: `[^`, the label, `]`, then `:` at once. A backslash before an ASCII punctuation
// character escapes that character, so `\]` and `\[` are part of the label, as the renderer writes
// a label that holds a bracket (pmdoc's escapeFootnoteLabel). An unescaped `[` makes the line no
// label, and so does a label of nothing but white space. ok is false where no label stands at at.
func footnoteLabelEnd(text string, at int) (end int, ok bool) {
	if !strings.HasPrefix(text[at:], "[^") {
		return 0, false
	}
	bracket := at + 2
	for ; bracket < len(text) && text[bracket] != ']'; bracket++ {
		if text[bracket] == '[' {
			return 0, false
		}
		if text[bracket] == '\\' && bracket+1 < len(text) && asciiPunctuation(text[bracket+1]) {
			bracket++
		}
	}
	// The label is blank as goldmark's util.IsBlank reads it: white space alone.
	if bracket+1 >= len(text) || text[bracket+1] != ':' || strings.Trim(text[at+2:bracket], " \t\r\n\v\f") == "" {
		return 0, false
	}
	return bracket + 2, true
}

// asciiPunctuation reports whether char is ASCII punctuation, which a backslash before it escapes
// (goldmark's util.IsPunct).
func asciiPunctuation(char byte) bool {
	return char >= '!' && char <= '/' || char >= ':' && char <= '@' || char >= '[' && char <= '`' || char >= '{' && char <= '~'
}

// directiveNameByte reports whether char can stand in a directive's name (pmdoc's
// directiveNameByte).
func directiveNameByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-'
}

// thematicBreak is goldmark's isThematicBreak: three or more of one of `*`, `-` and `_`, with
// spaces and tabs between them and nothing else.
func thematicBreak(text string) bool {
	var mark byte
	count := 0
	for index := range len(text) {
		switch char := text[index]; {
		case char == ' ' || char == '\t':
		case char != '*' && char != '-' && char != '_', mark != 0 && char != mark:
			return false
		default:
			mark = char
			count++
		}
	}
	return count >= 3
}

// setextUnderline is goldmark's matchesSetextHeadingBar: a run of `=` or of `-`, then spaces and
// tabs alone.
func setextUnderline(text string) bool {
	run := runOf(text, text[0])
	return (text[0] == '=' || text[0] == '-') && blankText(text[run:])
}

// tableDelimiterRow reports whether text is a table's delimiter row (GFM's tables), which makes
// the paragraph above it a table: cells of one or more `-`, each with an optional `:` on either
// side, between `|`, with at least one `|` and an optional one at either end.
func tableDelimiterRow(text string) bool {
	text = strings.Trim(text, " \t")
	if !strings.Contains(text, "|") {
		return false
	}
	text = strings.TrimPrefix(text, "|")
	text = strings.TrimSuffix(text, "|")
	for cell := range strings.SplitSeq(text, "|") {
		cell = strings.TrimPrefix(strings.TrimSuffix(strings.Trim(cell, " \t"), ":"), ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

// fenceColons is pmdoc's fenceColons: the colons text holds when they are all it holds but spaces
// and tabs, and 0 otherwise.
func fenceColons(text string) int {
	text = strings.Trim(text, " \t")
	if text == "" || strings.Trim(text, ":") != "" {
		return 0
	}
	return len(text)
}

// runOf is how many times char stands in a row at the start of text.
func runOf(text string, char byte) int {
	run := 0
	for run < len(text) && text[run] == char {
		run++
	}
	return run
}

// blankText reports whether text holds nothing but spaces and tabs.
func blankText(text string) bool {
	return strings.Trim(text, " \t") == ""
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
