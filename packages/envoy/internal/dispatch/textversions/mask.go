package textversions

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A deleted pending insert's words were its comment's own and never document text, so no read of
// a stored document version gives them (R3 of dispatch://INFRA-1523, docs.MaskedVersionText). A
// vector cannot be masked at a read, and an issue's keyword row holds the vector of the primary
// document version it names, so the table holds the vector of the masked text. A version's text
// can be masked only where the version stores an insert tree (artifact_versions.insert_tree,
// written once with the version), so only the rows of such versions can change under a delete.
//
// The fill and the delete meet on the issue's owner lock (store.LockOwner), which the delete
// takes before anything it reads and holds to its commit (api's deleteComment, through its
// owner's lock), as every writer of a comment does. The fill reads and renders everything a row of
// such a version needs outside any lock (readMaskPlan): the state of the comments on each document
// (maskStateQuery), then each version's text as every read gives it. Then, as the last thing its
// transaction does, it takes the owner locks of those rows' issues in one statement
// (store.LockOwners), reads the documents' state again, and renders again the text of each
// version whose document changed since (maskPlan.recheck), before it writes the rows and commits:
//
//   - A delete that holds the lock when the fill asks for it commits before the fill's second read,
//     which sees the comment deleted and renders the text without the words.
//   - A delete that asks for the lock while the fill holds it waits for the fill's commit, and its
//     rewrite of the issue's rows (store.QueueTextVersionsMask), a statement after that commit,
//     reads and rewrites the rows the fill wrote.
//   - A delete that committed before the fill's first read is read by it.
//
// So no row of an issue keeps a deleted insert's words, and the fill holds an issue's lock only for
// the second read, any text it must render again, the write and the commit. A row of a version
// that stores no tree needs no lock. The fill takes no other owner's lock and holds its cursor row
// first; no writer of an issue waits for the cursor row, so no cycle of waits forms.

// treeVersion is a stored document version that holds an insert tree: its document and number,
// the issue whose primary document it is, its markdown and its tree.
type treeVersion struct {
	key      specVersion
	issue    string
	markdown string
	tree     []byte
}

// specTreesQuery reads, of the primary document versions $1 (their documents' ids) and $2 (their
// numbers), each that stores an insert tree, with its document's issue, its markdown and its tree:
// the versions whose text a read can mask (docs.MaskedVersionText). A version that stores none is
// read as stored.
const specTreesQuery = `
	select v.artifact_id::text, v.number, coalesce(a.issue_key, ''), coalesce(v.markdown, ''), v.insert_tree
	  from unnest($1::text[], $2::integer[]) as s (artifact_id, number)
	  join public.artifact_versions v on v.artifact_id = s.artifact_id::uuid and v.number = s.number
	  join public.artifacts a on a.id = v.artifact_id
	 where v.insert_tree is not null`

// readSpecTrees reads, of the primary document versions the issue versions of versions name, those
// that store an insert tree (specTreesQuery).
func readSpecTrees(ctx context.Context, tx pgx.Tx, versions []version) ([]treeVersion, error) {
	var artifacts []string
	var numbers []int32
	seen := map[specVersion]bool{}
	for index := range versions {
		key, ok := versions[index].spec()
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		artifacts, numbers = append(artifacts, key.artifact), append(numbers, key.number)
	}
	if len(artifacts) == 0 {
		return nil, nil
	}
	return scanTreeVersions(ctx, tx, specTreesQuery, artifacts, numbers)
}

// scanTreeVersions runs query, which answers versions' documents, numbers, issues, markdowns and
// trees, with args.
func scanTreeVersions(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]treeVersion, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the document versions holding an insert tree: %w", err)
	}
	defer rows.Close()
	var trees []treeVersion
	for rows.Next() {
		var one treeVersion
		if err := rows.Scan(&one.key.artifact, &one.key.number, &one.issue, &one.markdown, &one.tree); err != nil {
			return nil, fmt.Errorf("read a document version holding an insert tree: %w", err)
		}
		trees = append(trees, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the document versions holding an insert tree: %w", err)
	}
	return trees, nil
}

// maskStateQuery reads, of each document $1, the state of the comments anchored on it that a
// version's mask reads (docs.MaskedVersionText): each comment's id, its mark, whether it is deleted
// and whether its suggestion was accepted, as one digest. A delete, an accept and a reject of a
// suggestion each write the comment's row, so each changes the digest. The comments are read
// through the comments' anchor index (comments_anchor_artifact).
const maskStateQuery = `
	select d.id::text,
	       md5(coalesce(string_agg(c.id::text || ' ' || coalesce(c.anchor->>'mark_id', '') || ' '
	                               || (c.deleted_at is not null)::text || ' ' || coalesce(c.suggestion->>'accepted', ''),
	                               ',' order by c.id), ''))
	  from unnest($1::uuid[]) as d (id)
	  left join public.comments c on (c.anchor->>'artifact_id')::uuid = d.id
	 group by d.id`

// readMaskState reads the state of the comments on each of documents (maskStateQuery).
func readMaskState(ctx context.Context, tx pgx.Tx, documents []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, maskStateQuery, documents)
	if err != nil {
		return nil, fmt.Errorf("read the comments of %d documents holding an insert tree: %w", len(documents), err)
	}
	defer rows.Close()
	state := make(map[string]string, len(documents))
	for rows.Next() {
		var document, digest string
		if err := rows.Scan(&document, &digest); err != nil {
			return nil, fmt.Errorf("read the comments of %d documents holding an insert tree: %w", len(documents), err)
		}
		state[document] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the comments of %d documents holding an insert tree: %w", len(documents), err)
	}
	return state, nil
}

// maskPlan is what a batch reads, outside any lock, of the document versions holding an insert
// tree whose issue rows it writes: the versions, the state of each one's document's comments,
// read before the texts, and each version's text as every read gives it, by version.
type maskPlan struct {
	versions []treeVersion
	state    map[string]string
	texts    map[specVersion]string
}

// readMaskPlan reads the plan of versions (maskPlan): the documents' state first, then the texts,
// so a delete or an accept that commits after the state was read changes the state the recheck
// reads under the lock, whichever of the texts it reached.
func readMaskPlan(ctx context.Context, tx pgx.Tx, versions []treeVersion) (maskPlan, error) {
	plan := maskPlan{versions: versions, texts: make(map[specVersion]string, len(versions))}
	if len(versions) == 0 {
		return plan, nil
	}
	var err error
	if plan.state, err = readMaskState(ctx, tx, plan.documents()); err != nil {
		return maskPlan{}, err
	}
	for _, one := range versions {
		if plan.texts[one.key], err = docs.MaskedVersionText(ctx, tx, one.key.artifact, one.markdown, one.tree); err != nil {
			return maskPlan{}, err
		}
	}
	return plan, nil
}

// documents is the plan's documents, each once, sorted.
func (p maskPlan) documents() []string {
	documents := make([]string, 0, len(p.versions))
	for _, one := range p.versions {
		documents = append(documents, one.key.artifact)
	}
	slices.Sort(documents)
	return slices.Compact(documents)
}

// owners is the owners of the plan's versions' issues: the locks a delete of a comment on one of
// their documents holds.
func (p maskPlan) owners() []store.Owner {
	owners := make([]store.Owner, 0, len(p.versions))
	for _, one := range p.versions {
		if one.issue != "" {
			owners = append(owners, store.IssueOwner(one.issue))
		}
	}
	return owners
}

// lockAndRecheck takes, in tx, the owner locks of the plan's issues (store.LockOwners), reads the
// documents' state again, and renders again the text of each version whose document changed
// since the plan read it, so the plan's texts are those every read gives while tx holds the
// locks, to its commit. It answers how many versions it rendered again.
func (p *maskPlan) lockAndRecheck(ctx context.Context, tx pgx.Tx) (int, error) {
	if len(p.versions) == 0 {
		return 0, nil
	}
	if err := store.LockOwners(ctx, tx, p.owners()...); err != nil {
		return 0, fmt.Errorf("lock the issues of %d document versions holding an insert tree: %w", len(p.versions), err)
	}
	now, err := readMaskState(ctx, tx, p.documents())
	if err != nil {
		return 0, err
	}
	again := 0
	for _, one := range p.versions {
		if now[one.key.artifact] == p.state[one.key.artifact] {
			continue
		}
		if p.texts[one.key], err = docs.MaskedVersionText(ctx, tx, one.key.artifact, one.markdown, one.tree); err != nil {
			return 0, err
		}
		again++
	}
	p.state = now
	return again, nil
}

// masked is the text of each of the plan's versions that a read masks, by version: the texts
// the insert makes an issue row's vector from in place of the stored markdown
// (insertVersionsQuery).
func (p maskPlan) masked() map[specVersion]string {
	masked := map[specVersion]string{}
	for _, one := range p.versions {
		if text := p.texts[one.key]; text != one.markdown {
			masked[one.key] = text
		}
	}
	return masked
}

// holds is whether v is an issue row naming one of the plan's versions.
func (p maskPlan) holds(v version) bool {
	key, ok := v.spec()
	if !ok {
		return false
	}
	_, held := p.texts[key]
	return held
}

// maskedDocument is, of one document, the versions whose text a read masks and those texts, in
// the same order: the rows the re-read rewrites (store.MaskTextVersions).
type maskedDocument struct {
	artifact string
	numbers  []int32
	texts    []string
}

// maskedByDocument is the plan's versions whose text a read masks, by document, in the order of
// the plan's versions, which a re-read reads by document and number (deletedInsertVersionsQuery).
func (p maskPlan) maskedByDocument() []maskedDocument {
	var out []maskedDocument
	for _, one := range p.versions {
		text := p.texts[one.key]
		if text == one.markdown {
			continue
		}
		if len(out) == 0 || out[len(out)-1].artifact != one.key.artifact {
			out = append(out, maskedDocument{artifact: one.key.artifact})
		}
		document := &out[len(out)-1]
		document.numbers, document.texts = append(document.numbers, one.key.number), append(document.texts, text)
	}
	return out
}
