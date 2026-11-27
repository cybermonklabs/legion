package refs

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// prefixCandidateLimit bounds what one ResolvePrefix read loads, and what ResolvePrefixes loads
// for each of its ids: it has to tell one match from more than one and list the matches for a
// person to choose from, and ten is as long a list as anyone reads before writing more of the
// id. A prefix more items carry is listed to this bound.
const prefixCandidateLimit = 10

// prefixLimit is prefixCandidateLimit as the `limit` of every query of this file and of
// target_prefixes.go.
var prefixLimit = strconv.Itoa(prefixCandidateLimit)

// Candidate is one item an id prefix names, as ResolvePrefix answers with and
// ErrPrefixAmbiguous lists; the API serialises it.
//
// ID is the whole id the prefix shortens: the item's uuid for an ask, a comment, a message and a
// document, and the block id for a block, which is unique within its document alone - Ref
// addresses it. Ref is the whole `dispatch://` reference, written the way text writes one
// (itemRef, ArtifactRef, BlockRef); it is empty for an item that has no address, a direct
// message or a document of an agent's conversation.
//
// IssueKey and Document are the candidate's owner, at most one of them: the issue that owns it,
// or the reference of the project document it is written on where no issue owns it. A block
// takes its document's owner, and a document candidate of a project sets neither, its own Ref
// being that document. An item with no owner of either kind - a direct message - sets neither.
//
// FirstLine is the head of what a person reads to tell two candidates apart, as the function
// FirstLine cuts it: an ask's question, a comment's or a message's body, a document's name, a
// block's text. A direct message's is left out for a caller that may not read the conversation
// (Viewer).
//
// FirstLineTree is the display tree of FirstLine, which this package never fills: the API fills
// it for a caller that sends `Dispatch-Trees: 1`, beside the first line of every candidate but a
// document's, whose first line is its file's name (api.writeAmbiguousID). It is absent wherever
// FirstLine is.
//
// EditedAt, DeletedAt and DeletedBy are a comment's or a message's marks, as the reads of the post
// itself carry them (model.Comment, model.Message): the time its author last edited it, and the
// mark of a delete, whose cleared body leaves FirstLine empty, so a list of candidates says the
// post is deleted rather than showing an empty line. Each is nil on a post that stands unedited
// and on a candidate of any other kind; a block candidate is a block of a document
// (blockPrefixCandidates), which no correction marks.
type Candidate struct {
	Kind          string            `json:"kind" enum:"ask,comment,message,artifact,block"`
	ID            string            `json:"id"`
	Ref           string            `json:"ref"`
	IssueKey      string            `json:"issue_key,omitempty"`
	Document      string            `json:"document,omitempty"`
	FirstLine     string            `json:"first_line,omitempty"`
	FirstLineTree model.DisplayTree `json:"first_line_tree,omitempty"`
	EditedAt      *time.Time        `json:"edited_at,omitempty"`
	DeletedAt     *time.Time        `json:"deleted_at,omitempty"`
	DeletedBy     *model.Actor      `json:"deleted_by,omitempty"`
}

// ErrPrefixAmbiguous is the refusal of an id prefix two items or more carry, with the matches for
// the caller to choose from, in id order. Candidates holds at most prefixCandidateLimit of them:
// a prefix more items carry is refused with that many listed.
type ErrPrefixAmbiguous struct {
	Prefix     string
	Candidates []Candidate
}

func (e *ErrPrefixAmbiguous) Error() string {
	return fmt.Sprintf("the id prefix %q names %d items; write more of the id", e.Prefix, len(e.Candidates))
}

// Viewer is who asks, which decides whether a direct message's first line is answered with it. A
// direct conversation belongs to a human and one session: a human reads any of them, a session
// only one it is in, and an index-time resolver, the zero Viewer, none - it writes the reference
// graph, which every authenticated caller reads.
//
// Human is a person, who gets every first line. Session is the session the caller claims, as
// `?session=` on the message routes carries it; it gets a direct message's first line only when
// that session is in the conversation, by the rule messageRead.hasSession applies: the thread
// root targets it, or it authored a reply in the thread. It is the caller's own claim, like
// `actor`, not an authorization boundary: nothing in Dispatch keeps a direct conversation's text
// from an authenticated caller, and this keeps one session from reading another's by mistake.
//
// ServerURL is the deployment the caller reads, whose dashboard URLs a candidate's first line is
// cut around as references (FirstLine); an index-time resolver, which keeps no first line, has
// none.
type Viewer struct {
	Human     bool
	Session   string
	ServerURL string
}

// Scope is where ResolvePrefix looks.
//
// IssueKey and Document name the one owner to look within: the items of that issue, or the items
// written on that document. Both empty looks among every item of the kind, which is what an
// owner-free API route asks for; the resolver that rewrites a short id in a body fills the one
// the reference names. Document is the document's ref_key - `<issue key or project>/<slug>`, or
// `agent/<session id>/<slug>` - the address refs stores an artifact target under, not its
// `dispatch://` reference.
//
// For a block, Document is the document whose blocks the prefix is matched among and is
// required, and Version is the version the reference pins (text.Ref.Pin), 0 for the document's
// latest indexed version.
//
// Naming an owner a kind cannot have - both fields at once, a document or a block owned by a
// document, a message owned by anything but an issue, a version on anything but a block - is a
// programming error at the call site, answered as no match rather than queried for.
type Scope struct {
	IssueKey string
	Document string
	Version  int
}

// ResolvePrefix is the one item kind names whose id starts with prefix: an ask, a comment, a
// message, a document (`artifact`) or a document block, looked for where scope says, as viewer.
// It is its two halves composed: the read of the candidates (readPrefixCandidates) and the choice
// among them (choosePrefixCandidate). Both callers that resolve one short id at a time read the
// same candidates and choose by the same rule (pickPrefixCandidate): the resolver that rewrites a
// body's short ids as it is indexed (text.Resolver.Items), for the source's writer, with the zero
// Viewer and the owner the reference names, which reads the two halves itself
// (Reader.resolveItem), and the API route that resolves one id for a person or a session through
// this function, with the viewer it authenticated and, where its route names no owner, the whole
// kind. ResolvePrefixes is the batched second copy of these queries, which GET
// /api/v1/reference-targets uses to resolve the short ids of a whole set at once. It shares the
// selection rule of this function (choosePrefixCandidate) and binds the same predicate of each
// kind for its reader, so a short id names the same item through either function for the same
// reader.
//
// It answers for who: an item who may not see is no candidate, so a prefix it alone carries
// matches nothing and one it shares with a visible item names that item, and neither its id nor
// its first line is answered (VisibleKind, each kind's own predicate bound into the read). The
// resolver answers for the writer of the text it indexes (Reader.Items), so what a short id
// resolves to, and whether it stays ambiguous, tells that writer nothing of an item they may not
// see.
//
// It answers the one match, ErrPrefixAmbiguous with the matches listed (at most
// prefixCandidateLimit of them, in id order) where two or more carry the prefix, and
// pgx.ErrNoRows where none does, which is the one no-match signal: a kind it does not know, a
// scope its kind cannot have and a prefix that can start no id are all no match too, not errors
// of their own.
//
// A failure reaching the database is answered wrapped, for errors.Is and errors.As, with a text
// built of constant words alone: it names what was read - the asks, the comments, the messages,
// the documents or the blocks, a word chosen by comparing kind with the kinds it reads and never
// kind itself - and holds nothing of the reference, neither the prefix nor its length nor the
// owner, and no id a row holds either. The resolver logs that error (Reader.failed), and every
// part of the reference is text a stored body wrote. ErrPrefixAmbiguous quotes the prefix, being
// the answer to the caller who wrote it; the resolver never builds one, since it chooses among the
// candidates by value (pickPrefixCandidate).
//
// prefix is an id written short as text.IsIDPrefix reads one: at least eight of the hex digits
// and hyphens an id's lowercase text form is written with, compared without case, and never a
// whole id, since an exact id needs no resolving and always wins. A value IsIDPrefix refuses is a
// programming error at the call site - every caller reads the id out of a reference the same
// grammar parsed - and is answered as no match without a query. Among the matches the id written
// exactly as the prefix wins, which is what a whole block id that is another block's prefix
// needs; where the id is a uuid, one that differs from the prefix in case alone wins after it,
// since a uuid's text form is lowercase hex and the prefix is read without case. A block id is
// text its author wrote, so `Deadbeef` and `deadbeef` are two blocks and the prefix names the one
// it is written as. That rule has one copy, pickPrefixCandidate.
//
// An item's prefix is read as a range on its table's uuid primary key, the prefix with its
// hyphens where a uuid has them and the rest filled with `0` and with `f`, which that key's btree
// index serves; a prefix whose hyphens are not where a uuid's are can start no id and matches
// nothing without a query. A block's is matched among the block ids of the one document version
// named. Either way it is one query, which fills every candidate's first line too.
func ResolvePrefix(ctx context.Context, q Queryer, who model.Actor, kind string, scope Scope, prefix string, viewer Viewer) (Candidate, error) {
	found, err := readPrefixCandidates(ctx, q, who, kind, scope, prefix, viewer)
	if err != nil {
		return Candidate{}, err
	}
	return choosePrefixCandidate(kind, prefix, found)
}

// readPrefixCandidates is every candidate of kind whose id starts with prefix, looked for where
// scope says among the items who may see and read as viewer reads them, in id order and at most
// prefixCandidateLimit of them: the read ResolvePrefix answers from, and the one the resolver
// picks a short id's whole id from (Reader.resolveItem). A prefix text.IsIDPrefix refuses, a kind
// it does not know and a scope the kind cannot have are no candidate, answered without a query.
// An error is a failure reaching the database alone, whose text holds nothing of the reference
// (ResolvePrefix).
func readPrefixCandidates(ctx context.Context, q Queryer, who model.Actor, kind string, scope Scope, prefix string, viewer Viewer) ([]Candidate, error) {
	if !text.IsIDPrefix(prefix) {
		return nil, nil
	}
	switch kind {
	case "ask", "comment":
		return itemPrefixCandidates(ctx, q, &who, kind, scope, prefix, viewer.ServerURL)
	case "message":
		return messagePrefixCandidates(ctx, q, &who, scope, prefix, viewer)
	case "artifact":
		return artifactPrefixCandidates(ctx, q, &who, scope, prefix, viewer.ServerURL)
	case "block":
		return blockPrefixCandidates(ctx, q, &who, scope, prefix, viewer.ServerURL)
	}
	return nil, nil
}

// choosePrefixCandidate is ResolvePrefix's answer among found, every candidate of kind the read of
// prefix found, in id order: the one item pickPrefixCandidate picks, pgx.ErrNoRows where found
// holds none, and ErrPrefixAmbiguous, which lists them all, where it holds two or more and no id
// among them wins. ResolvePrefixes answers each of its ids by it too, so a short id names the
// same item either way.
func choosePrefixCandidate(kind, prefix string, found []Candidate) (Candidate, error) {
	if candidate, ok := pickPrefixCandidate(kind, prefix, found); ok {
		return candidate, nil
	}
	if len(found) == 0 {
		return Candidate{}, pgx.ErrNoRows
	}
	return Candidate{}, &ErrPrefixAmbiguous{Prefix: prefix, Candidates: found}
}

// pickPrefixCandidate is the one item prefix names among found, every candidate of kind the read
// of that prefix found, in id order, and false where it names none or two items or more, which
// len(found) tells apart. It is the one copy of the rule every reading of a short id answers by:
// ResolvePrefix and ResolvePrefixes through choosePrefixCandidate, and the resolver by value
// (Reader.resolveItem), which so builds no refusal that quotes the prefix a body wrote. The id
// written exactly as the prefix wins; where kind is no block, an id that differs from the prefix
// in case alone wins after it; then the one candidate, where found holds one.
func pickPrefixCandidate(kind, prefix string, found []Candidate) (Candidate, bool) {
	for _, candidate := range found {
		if candidate.ID == prefix {
			return candidate, true
		}
	}
	if kind != "block" {
		for _, candidate := range found {
			if strings.EqualFold(candidate.ID, prefix) {
				return candidate, true
			}
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return Candidate{}, false
}

// itemPrefixCandidates is the asks or comments prefix names, oldest id first. An ask and a
// comment are owned by an issue or by a document (asks_one_owner, comments_one_owner), so either
// scope narrows the uuid range by the column that owns the row. An item who may not see is none
// of them. Its errors name the table it reads, which kind chooses and does not spell
// (ResolvePrefix).
func itemPrefixCandidates(ctx context.Context, q Queryer, who *model.Actor, kind string, scope Scope, prefix, serverURL string) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.IssueKey != "" && scope.Document != "" {
		return nil, nil
	}
	table, column := "asks", "question"
	if kind == "comment" {
		table, column = "comments", "body"
	}
	var args queryArgs
	query := `select item.id::text, item.issue_key, coalesce(issue.project_key, doc.project_key, ''), coalesce(doc.slug, ''), item.` + column + `,
		       ` + ItemKind(kind).postMarkColumns("item") + `
		from ` + table + ` item
		left join issues issue on issue.key = item.issue_key
		left join artifacts doc on doc.id = item.artifact_id
		where item.id >= ` + args.param(lower) + `::uuid and item.id <= ` + args.param(upper) + `::uuid`
	switch {
	case scope.IssueKey != "":
		query += ` and item.issue_key = ` + args.param(scope.IssueKey)
	case scope.Document != "":
		query += ` and item.artifact_id = (select id from artifacts where ref_key = ` + args.param(scope.Document) + `)`
	}
	query += args.andVisible(who, kind, `item.id::text`) + ` order by item.id limit ` + prefixLimit
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the %s an id prefix names: %w", table, err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var id, project, slug, body string
		var issueKey *string
		var marks postMarks
		if err := rows.Scan(append([]any{&id, &issueKey, &project, &slug, &body}, marks.targets()...)...); err != nil {
			return nil, fmt.Errorf("scan the %s an id prefix names: %w", table, err)
		}
		candidate, err := itemCandidate(kind, id, issueKey, project, slug, body, serverURL, marks)
		if err != nil {
			return nil, err
		}
		found = append(found, candidate)
	}
	return found, rows.Err()
}

// messagePrefixCandidates is the messages prefix names, oldest id first. A message is owned by an
// issue alone, so a document scope names nothing. A message who may not see is none of them.
//
// A direct message's first line is the one a caller may not read, so for a session viewer the
// same query reports, for each direct message among the candidates, whether that session is in
// its conversation: the thread root, found up in_reply_to as messageThreadCTE walks it, targets
// the session, or the session authored one of the messages of the thread, found back down
// in_reply_to. A human needs neither arm, and nor does an index-time resolver, which reads no
// direct message's first line at all.
func messagePrefixCandidates(ctx context.Context, q Queryer, who *model.Actor, scope Scope, prefix string, viewer Viewer) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.Document != "" {
		return nil, nil
	}
	var args queryArgs
	candidates := `select m.id, m.issue_key, m.body, ` + ItemMessage.postMarkColumns("m") + `
			from messages m
			where m.id >= ` + args.param(lower) + `::uuid and m.id <= ` + args.param(upper) + `::uuid`
	if scope.IssueKey != "" {
		candidates += ` and m.issue_key = ` + args.param(scope.IssueKey)
	}
	candidates += args.andVisible(who, "message", `m.id::text`) + ` order by m.id limit ` + prefixLimit
	threads, inConversation := "", "false"
	if !viewer.Human && viewer.Session != "" {
		threads = `,
		up as (
			select c.id as candidate, m.id as node, m.target, m.in_reply_to
			from candidates c join messages m on m.id = c.id
			where c.issue_key is null
			union all
			select u.candidate, m.id, m.target, m.in_reply_to
			from up u join messages m on m.id = u.in_reply_to
		),
		root as (select candidate, node, target from up where in_reply_to is null),
		thread as (
			select candidate, node from root
			union all
			select t.candidate, m.id from thread t join messages m on m.in_reply_to = t.node
		)`
		inConversation = `coalesce((select r.target from root r where r.candidate = c.id) = ` + args.param("session:"+viewer.Session) + `, false)
		       or exists (
		         select 1 from thread t join messages m on m.id = t.node
		         where t.candidate = c.id and m.author->>'kind' = 'session' and m.author->>'id' = ` + args.param(viewer.Session) + `
		       )`
	}
	rows, err := q.Query(ctx, `with recursive candidates as (`+candidates+`)`+threads+`
		select c.id::text, c.issue_key, coalesce(i.project_key, ''), c.body, c.edited_at, c.deleted_at, c.deleted_by, `+inConversation+`
		from candidates c
		left join issues i on i.key = c.issue_key
		order by c.id`, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the messages an id prefix names: %w", err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var id, project, body string
		var issueKey *string
		var marks postMarks
		var inThread bool
		if err := rows.Scan(append(append([]any{&id, &issueKey, &project, &body}, marks.targets()...), &inThread)...); err != nil {
			return nil, fmt.Errorf("scan the messages an id prefix names: %w", err)
		}
		candidate, err := itemCandidate("message", id, issueKey, project, "", body, viewer.ServerURL, marks)
		if err != nil {
			return nil, err
		}
		if issueKey == nil && !viewer.Human && !inThread {
			candidate.FirstLine, candidate.FirstLineTree = "", nil
		}
		found = append(found, candidate)
	}
	return found, rows.Err()
}

// itemCandidate is one ask, comment or message row as a candidate, addressed as the graph
// addresses it (itemRef), owned by its issue, or by the project document it is written on, and
// carrying the marks its row's postMarkColumns read. serverURL is the deployment whose dashboard
// URLs in body are references (FirstLine). Its error, a delete mark that does not decode, names
// neither kind nor id: the resolver logs it, kind is the word a reference wrote, and the id starts
// with the prefix it wrote (ResolvePrefix).
func itemCandidate(kind, id string, issueKey *string, project, slug, body, serverURL string, marks postMarks) (Candidate, error) {
	candidate := Candidate{
		Kind:      kind,
		ID:        id,
		Ref:       itemRef(kind, issueKey, project, slug, id),
		FirstLine: FirstLine(body, serverURL),
		EditedAt:  marks.editedAt,
	}
	var err error
	if candidate.DeletedAt, candidate.DeletedBy, err = marks.deleted(); err != nil {
		return Candidate{}, fmt.Errorf("decode the delete mark of a candidate: %w", err)
	}
	switch {
	case issueKey != nil:
		candidate.IssueKey = *issueKey
	case project != "" && slug != "":
		candidate.Document = ArtifactRef(nil, project, nil, slug, "", false)
	}
	return candidate, nil
}

// artifactPrefixCandidates is the documents and files prefix names, oldest id first. A document
// is owned by an issue or by a project, never by a document, so a document scope names nothing.
// A document who may not see is none of them.
func artifactPrefixCandidates(ctx context.Context, q Queryer, who *model.Actor, scope Scope, prefix, serverURL string) ([]Candidate, error) {
	lower, upper, ok := uuidRange(prefix)
	if !ok || scope.Version != 0 || scope.Document != "" {
		return nil, nil
	}
	var args queryArgs
	query := `select ` + documentColumns("a") + `, a.name
		from artifacts a
		where a.id >= ` + args.param(lower) + `::uuid and a.id <= ` + args.param(upper) + `::uuid`
	if scope.IssueKey != "" {
		query += ` and a.issue_key = ` + args.param(scope.IssueKey)
	}
	query += args.andVisible(who, "artifact", `a.ref_key`) + ` order by a.id limit ` + prefixLimit
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the documents an id prefix names: %w", err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var document documentParent
		var name string
		if err := rows.Scan(append(document.targets(), &name)...); err != nil {
			return nil, fmt.Errorf("scan the documents an id prefix names: %w", err)
		}
		found = append(found, artifactCandidate(document, name, serverURL))
	}
	return found, rows.Err()
}

// artifactCandidate is one document row as a candidate: addressed as the graph addresses it
// (documentParent.parent) and owned by its issue, where an issue owns it. serverURL is the
// deployment whose dashboard URLs in name are references (FirstLine).
func artifactCandidate(document documentParent, name, serverURL string) Candidate {
	candidate := Candidate{Kind: "artifact", ID: *document.id, FirstLine: FirstLine(name, serverURL)}
	if parent := document.parent(); parent != nil {
		candidate.Ref = parent.Ref
	}
	if document.issueKey != nil {
		candidate.IssueKey = *document.issueKey
	}
	return candidate
}

// blockPrefixCandidates is the blocks of one version of the document scope names whose id starts
// with prefix, in block id order. The version is the document's latest indexed one (Version 0),
// whose blocks document_blocks holds with the excerpt a mention of each shows, or the one the
// reference pins, whose blocks are the ids document_block_texts records a text of at or before
// it, the latest of those texts each, and none at all unless a block-aware build indexed that
// version (BlockTextAt reads one block the same way). A block id is text, not a uuid, so the
// prefix is matched on it, which is a read of the one document's blocks and not a scan. A block
// who may not see is none of them, at its node's id, the document's ref_key, '#' and the block
// id, whichever version names it.
func blockPrefixCandidates(ctx context.Context, q Queryer, who *model.Actor, scope Scope, prefix, serverURL string) ([]Candidate, error) {
	if scope.Document == "" || scope.IssueKey != "" || scope.Version < 0 || scope.Version > math.MaxInt32 {
		return nil, nil
	}
	var args queryArgs
	document, written := args.param(scope.Document), args.param(strings.ToLower(prefix))
	query := `select d.block_id, d.excerpt, ` + documentColumns("a") + `
		from document_blocks d
		join artifacts a on a.id = d.artifact_id
		where a.ref_key = ` + document + ` and starts_with(lower(d.block_id), ` + written + `)` +
		args.andVisible(who, "block", `d.node_id`) + `
		order by d.block_id limit ` + prefixLimit
	if scope.Version > 0 {
		version := args.param(scope.Version)
		query = `select t.block_id, t.text, ` + documentColumns("a") + `
			from artifacts a
			cross join lateral (
				select distinct on (b.block_id) b.block_id, b.text
				from document_block_texts b
				where b.artifact_id = a.id and b.version <= ` + version + ` and starts_with(lower(b.block_id), ` + written + `)
				order by b.block_id, b.version desc
			) t
			where a.ref_key = ` + document + ` and t.text is not null
			  and exists (select 1 from document_indexed_versions i where i.artifact_id = a.id and i.version = ` + version + `)` +
			args.andVisible(who, "block", `a.ref_key || '#' || t.block_id`) + `
			order by t.block_id limit ` + prefixLimit
	}
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("read the blocks an id prefix names in one document version: %w", err)
	}
	defer rows.Close()
	var found []Candidate
	for rows.Next() {
		var blockID, body string
		var document documentParent
		if err := rows.Scan(append([]any{&blockID, &body}, document.targets()...)...); err != nil {
			return nil, fmt.Errorf("scan the blocks an id prefix names in one document version: %w", err)
		}
		found = append(found, blockCandidate(blockID, body, document, serverURL))
	}
	return found, rows.Err()
}

// blockCandidate is one block of document as a candidate, body being its text: addressed under
// its document's reference and owned by its document's owner, the issue, or else the project
// document itself. serverURL is the deployment whose dashboard URLs in body are references
// (FirstLine).
func blockCandidate(blockID, body string, document documentParent, serverURL string) Candidate {
	candidate := Candidate{Kind: "block", ID: blockID, FirstLine: FirstLine(body, serverURL)}
	if parent := document.parent(); parent != nil {
		candidate.Ref = BlockRef(parent.Ref, blockID)
		if document.issueKey != nil {
			candidate.IssueKey = *document.issueKey
		} else if document.project != "" {
			candidate.Document = parent.Ref
		}
	}
	return candidate
}

// queryArgs are the arguments of a query this file or target_prefixes.go builds by
// concatenation: param appends one and answers the placeholder that reads it, so the text and
// the arguments of a query whose shape depends on its scope cannot drift apart. A query that
// binds its fixed arguments as literal placeholders starts with them in values, so the
// placeholders param and visible answer follow them.
type queryArgs struct {
	values []any
	// seen is the placeholder the acting credential is bound at, once the query binds a
	// predicate (andVisible), and "" until then.
	seen string
}

func (a *queryArgs) param(value any) string {
	a.values = append(a.values, value)
	return "$" + strconv.Itoa(len(a.values))
}

// visible is the predicate of node kind over address for who, the acting credential bound once
// among the query's arguments (VisibilityArg), or `true` where who is nil: a read for no person,
// as the index-time resolver's reads of the document a source's text belongs to and of that
// document's question blocks are (scopeOf, questionMap). The short ids that resolver resolves
// are no such read: it resolves them for the source's writer (Reader.Items,
// readPrefixCandidates). A query whose version binds no predicate binds no credential either,
// since Postgres refuses a statement with a parameter it never reads.
func (a *queryArgs) visible(who *model.Actor, kind, address string) string {
	if who == nil {
		return "true"
	}
	if a.seen == "" {
		a.seen = a.param(VisibilityArg(*who))
	}
	return VisibleKind(kind, address, a.seen)
}

// andVisible is ` and ` the predicate visible renders, and nothing where who is nil.
func (a *queryArgs) andVisible(who *model.Actor, kind, address string) string {
	if who == nil {
		return ""
	}
	return ` and ` + a.visible(who, kind, address)
}

// uuidRange is the range of uuids prefix names: the prefix, lowercased, with its remaining hex
// digits filled with `0` and with `f` and its remaining hyphens where a uuid writes them, so a
// comparison against it is one range on a uuid primary key. ok is false when prefix can start no
// uuid's text form - it is longer than one, or writes a hyphen where a uuid has a hex digit, or a
// hex digit where a uuid has a hyphen, as a prefix of an id whose hyphens it drops does - and
// nothing is then queried, since nothing can match.
func uuidRange(prefix string) (lower, upper string, ok bool) {
	// The canonical form a uuid's text is compared as: 8-4-4-4-12 hex digits.
	const canonical = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	if len(prefix) > len(canonical) {
		return "", "", false
	}
	low, high := []byte(canonical), []byte(canonical)
	for at := range len(canonical) {
		hyphen := canonical[at] == '-'
		if at >= len(prefix) {
			if hyphen {
				low[at], high[at] = '-', '-'
			} else {
				low[at], high[at] = '0', 'f'
			}
			continue
		}
		digit := prefix[at]
		if digit >= 'A' && digit <= 'F' {
			digit += 'a' - 'A'
		}
		if hyphen != (digit == '-') || !hyphen && !isHexDigit(digit) {
			return "", "", false
		}
		low[at], high[at] = digit, digit
	}
	return string(low), string(high), true
}

// CanStartUUID reports whether prefix can start a uuid's text form, which is uuidRange's own ok:
// a caller that has to tell a prefix no item carries from one no item can carry - the 404 an id
// route answers for a value that is no prefix of any id (api.itemNotFound) - asks the rule the
// range itself is built by rather than keeping a second copy of it.
func CanStartUUID(prefix string) bool {
	_, _, ok := uuidRange(prefix)
	return ok
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}
