package refs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ShortID is one id written short as ResolvePrefixes is asked it: the kind of item it names, the
// owner it is resolved within, and the prefix itself. These are the three values PrefixScope
// reads out of a reference.
type ShortID struct {
	Kind   string
	Scope  Scope
	Prefix string
}

// ResolvePrefixes is ResolvePrefix for many short ids at once. The read that resolves a set of
// references (GET /api/v1/reference-targets) uses it, so that a set costs one statement per kind
// of item, two for the blocks of a document, and not a statement per short id. It reads one
// statement for each kind that the ids name, however many ids and owners they hold: the asks,
// the comments, the messages, the documents, the blocks of the latest indexed version of a
// document, and the blocks of a version that a reference pins, which are read from other tables.
// A kind that no id names costs no statement, so a set of no id costs none. For each id, the
// statement reads the candidates that the query of ResolvePrefix reads for that id alone: the
// same range on the uuid key or the same match on the block id, the same owner, the same
// visibility predicate for who, the same order, and at most prefixCandidateLimit of them. Each
// id is then answered by the rule that ResolvePrefix answers by (choosePrefixCandidate), so an
// id names the same item through either function for the same reader.
//
// It answers for who, as ResolvePrefix does: an item who may not see is no candidate, its
// kind's own predicate bound into the statement of that kind ahead of its limit (VisibleKind,
// through queryArgs.andVisible). So an id that such an item alone carries names nothing, and an
// id that it shares with one item who may see names that item and is not ambiguous. A nil who
// is no person and reads every item.
//
// The first answer, whole, holds the one item that each id names, under that id. An id that
// nothing carries has no entry. Nor has an id that ResolvePrefix answers as no match without a
// query: a prefix that text.IsIDPrefix refuses or that can start no uuid, a kind that it does
// not know, and a scope that the kind cannot have. The second answer, ambiguous, is true for
// each id that two items or more carry, which has no entry in whole: where ResolvePrefix
// refuses such an id with ErrPrefixAmbiguous, this function answers it under its own id and
// answers every other id of the call too. ambiguous is nil when no id is ambiguous. An id that
// ids holds twice is read once.
//
// Every candidate is read as the zero Viewer reads it. A short message id in a reference always
// names the issue of its message (PrefixScope), so a message is resolved within its issue alone.
// Thus no direct message is ever a candidate, and this function needs no part that decides
// which session can read the first line of a direct message, as ResolvePrefix has. A message id
// whose scope names no issue is no match.
func ResolvePrefixes(ctx context.Context, q Queryer, who *model.Actor, ids []ShortID) (whole map[ShortID]Candidate, ambiguous map[ShortID]bool, err error) {
	var asks, comments, messages, documents, latest, pinned, order []ShortID
	seen := make(map[ShortID]bool, len(ids))
	for _, id := range ids {
		if seen[id] || !text.IsIDPrefix(id.Prefix) {
			continue
		}
		seen[id] = true
		scope := id.Scope
		// The scopes each kind can have, as the query of that kind in prefix.go reads them.
		item := CanStartUUID(id.Prefix) && scope.Version == 0
		switch {
		case id.Kind == "ask" && item && (scope.IssueKey == "" || scope.Document == ""):
			asks = append(asks, id)
		case id.Kind == "comment" && item && (scope.IssueKey == "" || scope.Document == ""):
			comments = append(comments, id)
		case id.Kind == "message" && item && scope.IssueKey != "" && scope.Document == "":
			messages = append(messages, id)
		case id.Kind == "artifact" && item && scope.Document == "":
			documents = append(documents, id)
		case id.Kind == "block" && scope.Document != "" && scope.IssueKey == "" && scope.Version == 0:
			latest = append(latest, id)
		case id.Kind == "block" && scope.Document != "" && scope.IssueKey == "" && scope.Version > 0 && scope.Version <= math.MaxInt32:
			pinned = append(pinned, id)
		default:
			continue
		}
		order = append(order, id)
	}
	found := make(map[ShortID][]Candidate, len(order))
	for _, read := range []func() error{
		func() error { return itemShortIDCandidates(ctx, q, who, "ask", asks, found) },
		func() error { return itemShortIDCandidates(ctx, q, who, "comment", comments, found) },
		func() error { return messageShortIDCandidates(ctx, q, who, messages, found) },
		func() error { return artifactShortIDCandidates(ctx, q, who, documents, found) },
		func() error { return blockShortIDCandidates(ctx, q, who, latest, false, found) },
		func() error { return blockShortIDCandidates(ctx, q, who, pinned, true, found) },
	} {
		if err := read(); err != nil {
			return nil, nil, err
		}
	}
	whole = make(map[ShortID]Candidate, len(order))
	var many *ErrPrefixAmbiguous
	for _, id := range order {
		// choosePrefixCandidate answers one item, ErrPrefixAmbiguous, or pgx.ErrNoRows for an
		// id that nothing carries, which has no entry in either answer.
		candidate, err := choosePrefixCandidate(id.Kind, id.Prefix, found[id])
		switch {
		case err == nil:
			whole[id] = candidate
		case errors.As(err, &many):
			if ambiguous == nil {
				ambiguous = make(map[ShortID]bool)
			}
			ambiguous[id] = true
		}
	}
	return whole, ambiguous, nil
}

// itemShortIDCandidates reads into found, under each of ids, the asks or the comments that its
// prefix names, oldest id first, as itemPrefixCandidates reads them for one id: within the issue
// or the document that its scope names, or among every item of the kind where its scope names
// neither. An item who may not see is none of them. Each carries the marks its row's
// postMarkColumns read, as itemPrefixCandidates' do.
func itemShortIDCandidates(ctx context.Context, q Queryer, who *model.Actor, kind string, ids []ShortID, found map[ShortID][]Candidate) error {
	if len(ids) == 0 {
		return nil
	}
	table, column := "asks", "question"
	if kind == "comment" {
		table, column = "comments", "body"
	}
	lows, highs, issues := shortIDRanges(ids)
	documents := make([]string, len(ids))
	for index, id := range ids {
		documents[index] = id.Scope.Document
	}
	args := queryArgs{values: []any{lows, highs, issues, documents}}
	rows, err := q.Query(ctx, `select p.n, item.id::text, item.issue_key, coalesce(issue.project_key, doc.project_key, ''), coalesce(doc.slug, ''), item.`+column+`,
		       item.edited_at, item.deleted_at, item.deleted_by
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) with ordinality as p (low, high, issue_key, document, n)
		cross join lateral (
			select item.id, item.issue_key, item.artifact_id, item.`+column+`, `+ItemKind(kind).postMarkColumns("item")+`
			from `+table+` item
			where item.id >= p.low::uuid and item.id <= p.high::uuid
			  and (p.issue_key = '' or item.issue_key = p.issue_key)
			  and (p.document = '' or item.artifact_id = (select id from artifacts where ref_key = p.document))`+
		args.andVisible(who, kind, `item.id::text`)+`
			order by item.id limit `+prefixLimit+`
		) item (id, issue_key, artifact_id, `+column+`, edited_at, deleted_at, deleted_by)
		left join issues issue on issue.key = item.issue_key
		left join artifacts doc on doc.id = item.artifact_id
		order by p.n, item.id`, args.values...)
	if err != nil {
		return fmt.Errorf("read the %ss that %d short ids name: %w", kind, len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		var id, project, slug, body string
		var issueKey *string
		var marks postMarks
		if err := rows.Scan(append([]any{&ordinal, &id, &issueKey, &project, &slug, &body}, marks.targets()...)...); err != nil {
			return fmt.Errorf("scan the %ss that %d short ids name: %w", kind, len(ids), err)
		}
		candidate, err := itemCandidate(kind, id, issueKey, project, slug, body, marks)
		if err != nil {
			return err
		}
		short := ids[ordinal-1]
		found[short] = append(found[short], candidate)
	}
	return rows.Err()
}

// messageShortIDCandidates reads into found, under each of ids, the messages of its issue that
// its prefix names, oldest id first, as messagePrefixCandidates reads them for one id within an
// issue, each with its marks. Every id names an issue (ResolvePrefixes), so no direct message is
// read. A message who may not see is none of them.
func messageShortIDCandidates(ctx context.Context, q Queryer, who *model.Actor, ids []ShortID, found map[ShortID][]Candidate) error {
	if len(ids) == 0 {
		return nil
	}
	lows, highs, issues := shortIDRanges(ids)
	args := queryArgs{values: []any{lows, highs, issues}}
	rows, err := q.Query(ctx, `select p.n, m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body,
		       m.edited_at, m.deleted_at, m.deleted_by
		from unnest($1::text[], $2::text[], $3::text[]) with ordinality as p (low, high, issue_key, n)
		cross join lateral (
			select m.id, m.issue_key, m.body, `+ItemMessage.postMarkColumns("m")+`
			from messages m
			where m.id >= p.low::uuid and m.id <= p.high::uuid and m.issue_key = p.issue_key`+
		args.andVisible(who, "message", `m.id::text`)+`
			order by m.id limit `+prefixLimit+`
		) m (id, issue_key, body, edited_at, deleted_at, deleted_by)
		left join issues i on i.key = m.issue_key
		order by p.n, m.id`, args.values...)
	if err != nil {
		return fmt.Errorf("read the messages that %d short ids name: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		var id, project, body string
		var issueKey *string
		var marks postMarks
		if err := rows.Scan(append([]any{&ordinal, &id, &issueKey, &project, &body}, marks.targets()...)...); err != nil {
			return fmt.Errorf("scan the messages that %d short ids name: %w", len(ids), err)
		}
		candidate, err := itemCandidate("message", id, issueKey, project, "", body, marks)
		if err != nil {
			return err
		}
		short := ids[ordinal-1]
		found[short] = append(found[short], candidate)
	}
	return rows.Err()
}

// artifactShortIDCandidates reads into found, under each of ids, the documents and the files
// that its prefix names, oldest id first, as artifactPrefixCandidates reads them for one id:
// within the issue that its scope names, or among every document where its scope names none. A
// document who may not see is none of them.
func artifactShortIDCandidates(ctx context.Context, q Queryer, who *model.Actor, ids []ShortID, found map[ShortID][]Candidate) error {
	if len(ids) == 0 {
		return nil
	}
	lows, highs, issues := shortIDRanges(ids)
	args := queryArgs{values: []any{lows, highs, issues}}
	rows, err := q.Query(ctx, `select p.n, `+documentColumns("a")+`, a.name
		from unnest($1::text[], $2::text[], $3::text[]) with ordinality as p (low, high, issue_key, n)
		cross join lateral (
			select a.id
			from artifacts a
			where a.id >= p.low::uuid and a.id <= p.high::uuid and (p.issue_key = '' or a.issue_key = p.issue_key)`+
		args.andVisible(who, "artifact", `a.ref_key`)+`
			order by a.id limit `+prefixLimit+`
		) hit
		join artifacts a on a.id = hit.id
		order by p.n, a.id`, args.values...)
	if err != nil {
		return fmt.Errorf("read the documents that %d short ids name: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		var document documentParent
		var name string
		if err := rows.Scan(append(append([]any{&ordinal}, document.targets()...), &name)...); err != nil {
			return fmt.Errorf("scan the documents that %d short ids name: %w", len(ids), err)
		}
		short := ids[ordinal-1]
		found[short] = append(found[short], artifactCandidate(document, name))
	}
	return rows.Err()
}

// blockShortIDCandidates reads into found, under each of ids, the blocks of its document that
// its prefix names, in block id order, as blockPrefixCandidates reads them for one id: where the
// block index keeps them, in the room the document is edited in (DocumentRoomBlocksSQL,
// DocumentRoomTextsSQL). Without pinned, every id is of the latest indexed version of its
// document, whose blocks document_blocks holds. With pinned, every id is of the version that its
// reference pins, whose blocks are the ids that document_block_texts records a text of at or
// before that version, the latest of those texts each, and none at all unless a block-aware build
// indexed that version. A block who may not see is none of them, at its node's id, the room's
// ref_key, '#' and the block id, whichever version names it.
func blockShortIDCandidates(ctx context.Context, q Queryer, who *model.Actor, ids []ShortID, pinned bool, found map[ShortID][]Candidate) error {
	if len(ids) == 0 {
		return nil
	}
	documents, prefixes := make([]string, len(ids)), make([]string, len(ids))
	for index, id := range ids {
		documents[index], prefixes[index] = id.Scope.Document, strings.ToLower(id.Prefix)
	}
	args := queryArgs{values: []any{documents, prefixes}}
	var query string
	if !pinned {
		query = `select p.n, d.block_id, d.excerpt, ` + documentColumns("a") + `
		from unnest($1::text[], $2::text[]) with ordinality as p (document, prefix, n)
		join artifacts a on a.ref_key = p.document
		cross join lateral (
			select d.block_id, d.excerpt
			from document_blocks d
			where ` + DocumentRoomBlocksSQL("d", "a.id") + ` and starts_with(lower(d.block_id), p.prefix)` +
			args.andVisible(who, "block", `d.node_id`) + `
			order by d.block_id limit ` + prefixLimit + `
		) d
		order by p.n, d.block_id`
	} else {
		pins := make([]int64, len(ids))
		for index, id := range ids {
			pins[index] = int64(id.Scope.Version)
		}
		args.values = append(args.values, pins)
		query = `select p.n, t.block_id, t.text, ` + documentColumns("a") + `
			from unnest($1::text[], $2::text[], $3::bigint[]) with ordinality as p (document, prefix, pin, n)
			join artifacts a on a.ref_key = p.document
			cross join lateral (
				select t.block_id, t.text
				from (
					select distinct on (b.block_id) b.block_id, b.text
					from document_block_texts b
					where ` + DocumentRoomTextsSQL("b", "a.id") + ` and b.version <= p.pin and starts_with(lower(b.block_id), p.prefix)
					order by b.block_id, b.version desc
				) t
				where t.text is not null` +
			args.andVisible(who, "block", documentRoomBlockAddressSQL("a.id", "t.block_id")) + `
				order by t.block_id limit ` + prefixLimit + `
			) t
			where exists (select 1 from document_indexed_versions i where i.artifact_id = a.id and i.version = p.pin)
			order by p.n, t.block_id`
	}
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return fmt.Errorf("read the blocks that %d short ids name: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		var blockID, body string
		var document documentParent
		if err := rows.Scan(append([]any{&ordinal, &blockID, &body}, document.targets()...)...); err != nil {
			return fmt.Errorf("scan the blocks that %d short ids name: %w", len(ids), err)
		}
		short := ids[ordinal-1]
		found[short] = append(found[short], blockCandidate(blockID, body, document))
	}
	return rows.Err()
}

// shortIDRanges is, for each of ids, the uuid range that its prefix names (uuidRange) and the
// issue that its scope names, "" for none. Every prefix of ids can start a uuid
// (ResolvePrefixes).
func shortIDRanges(ids []ShortID) (lows, highs, issues []string) {
	lows, highs, issues = make([]string, len(ids)), make([]string, len(ids)), make([]string, len(ids))
	for index, id := range ids {
		lows[index], highs[index], _ = uuidRange(id.Prefix)
		issues[index] = id.Scope.IssueKey
	}
	return lows, highs, issues
}
