// Package refs indexes Dispatch links and reads the reference graph. Mentions are derived from
// source text into refs; structural relations stay in the columns that own them and the
// graph_edges view unions both into one typed edge relation. Artifact targets use ref_key
// (`<issue-or-project>/<slug>`, or `agent/<session id>/<slug>` for an agent's conversation) so
// every owner's documents and files share one address; artifact sources are the artifact uuid.
// Closure reads an issue's references one hop in each direction: every node the issue's own
// content links to, and every node that links to the issue or to its own content.
// References whose target is absent stay stored but are absent from closures and referenced-by
// results.
package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Queryer is the query surface shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Target is one end of a reference edge: the kind and durable id `refs` stores.
type Target struct {
	Kind string
	ID   string
}

// EditedTargets is what an edit of an ask's question or a comment's body from previous to body
// moves beside the edges it inserts or deletes, found being body's references. Every edge such a
// source writes shows the head of its text as its excerpt (Edges, excerptRunes), so when the edit
// moves that head, the panel of every target the source still cites changes although its edge
// stays: those targets are named too. An edit that keeps the head moves none.
func EditedTargets(previous, body string, found []text.Ref) []Target {
	if text.HeadRunes(previous, excerptRunes) == text.HeadRunes(body, excerptRunes) {
		return nil
	}
	kinds, ids := refTargets(found)
	targets := make([]Target, len(kinds))
	for index := range kinds {
		targets[index] = Target{Kind: kinds[index], ID: ids[index]}
	}
	return targets
}

// replaceRefs reconciles the Dispatch references written by one source with found: targets no
// longer mentioned are deleted, new ones inserted with insertedAt as created_at (nil: now()), and
// an edge that persists keeps its created_at and source_seq. It returns the targets whose inbound
// edges moved — deleted or inserted, oldest state first — so its caller can resolve the ones
// a reader refreshes by name (CountedChanges). It is PlanReplace and Queue back to back, the
// queued statements run at once as a batch of their own. Only targets of a node kind this
// build knows (nodeKinds) are deleted: during a release a newer build writes the same sources, and
// an edge to a node kind it added is one this build cannot read out of the body, so it keeps that
// edge.
//
// What it queues settles what the source owes the re-index queue (Replacement.Queue): this text
// has now been read with the whole grammar, so a write this build makes costs the drain nothing
// and only an older build's write stays owed.
func replaceRefs(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) ([]Target, error) {
	replacement, err := PlanReplace(ctx, tx, fromKind, fromID, found, insertedAt)
	if err != nil {
		return nil, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}); err != nil {
		return nil, err
	}
	return replacement.Changed(), nil
}

// Replacement is one source's references reconciled with found, the references its text now
// cites, as replaceRefs reconciles them. It is planned against the edges the source holds
// (PlanReplace, PlanReplaceFrom), so the targets it moves are known before anything is written,
// and written by Queue, which puts its statements on a write phase's batch: a writer that decides
// everything first and writes last plans in its read phase and queues in its write phase.
type Replacement struct {
	fromKind, fromID string
	kinds, ids       []string
	insertedAt       *time.Time
	changed          []Target
	held             []Target
	// owed is the source's id as the re-index queue keys it, "" for a from_id that is no uuid
	// (a test's synthetic source), which owes nothing: what queueSettleRefsGrammarOwed deletes
	// the row of.
	owed string
}

// PlanReplace plans the reconciliation of fromKind/fromID's references with found against the
// edges q holds for the source (HeldTargets).
func PlanReplace(ctx context.Context, q Queryer, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) (Replacement, error) {
	held, err := HeldTargets(ctx, q, fromKind, fromID)
	if err != nil {
		return Replacement{}, err
	}
	return PlanReplaceFrom(held, fromKind, fromID, found, insertedAt), nil
}

// HeldTargets is the targets of the edges q holds for one source, read with the predicate the
// reconciling delete visits them by: an edge to a node kind this build does not know (nodeKinds)
// is one that delete keeps, so it is neither held nor moved here (replaceRefs).
func HeldTargets(ctx context.Context, q Queryer, fromKind, fromID string) ([]Target, error) {
	held, err := scanTargets(q.Query(ctx, `
		select to_kind, to_id from refs where from_kind = $1 and from_id = $2 and to_kind = any($3)
	`, fromKind, fromID, nodeKinds))
	if err != nil {
		return nil, fmt.Errorf("read references: %w", err)
	}
	return held, nil
}

// PlanReplaceFrom plans the reconciliation against held, the targets the source holds: those a
// caller read (HeldTargets), or the Held of its own earlier replacement of the source that is not
// written yet. The targets it moves are the held ones found no longer names, in held's order, then
// those found names that held lacks, in found's order: what the delete and the insert return.
func PlanReplaceFrom(held []Target, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) Replacement {
	kinds, ids := refTargets(found)
	cited := make(map[Target]struct{}, len(kinds))
	after := make([]Target, len(kinds))
	for index := range kinds {
		after[index] = Target{Kind: kinds[index], ID: ids[index]}
		cited[after[index]] = struct{}{}
	}
	stored := make(map[Target]struct{}, len(held))
	changed := []Target{}
	for _, target := range held {
		stored[target] = struct{}{}
		if _, kept := cited[target]; !kept {
			changed = append(changed, target)
		}
	}
	for _, target := range after {
		if _, kept := stored[target]; !kept {
			changed = append(changed, target)
		}
	}
	owed, _ := canonicalUUID(fromID)
	return Replacement{fromKind: fromKind, fromID: fromID, kinds: kinds, ids: ids, insertedAt: insertedAt, changed: changed, held: after, owed: owed}
}

// Changed is the targets whose inbound edges the replacement moves (PlanReplaceFrom).
func (r Replacement) Changed() []Target {
	return r.changed
}

// Held is the targets the source holds once the replacement is written.
func (r Replacement) Held() []Target {
	return r.held
}

// Queue queues the replacement's writes on a write phase's batch: the delete of the edges found no
// longer names, the insert of the new ones, and the settlement of what the source owes the
// re-index queue (queueSettleRefsGrammarOwed). A surviving edge keeps its created_at and
// source_seq.
func (r Replacement) Queue(b *store.WriteBatch) {
	b.Queue("clear references", `
		delete from refs
		where from_kind = $1 and from_id = $2 and to_kind = any($5)
		  and not exists (
		    select 1 from unnest($3::text[], $4::text[]) as target(kind, id)
		    where target.kind = refs.to_kind and target.id = refs.to_id
		  )
	`, r.fromKind, r.fromID, r.kinds, r.ids, nodeKinds)
	if len(r.kinds) > 0 {
		b.Queue("write reference", `
			insert into refs (from_kind, from_id, to_kind, to_id, created_at)
			select $1, $2, target.kind, target.id, coalesce($5::timestamptz, now())
			from unnest($3::text[], $4::text[]) as target(kind, id)
			on conflict (from_kind, from_id, to_kind, to_id) do nothing
		`, r.fromKind, r.fromID, r.kinds, r.ids, r.insertedAt)
	}
	r.queueSettleRefsGrammarOwed(b)
}

// queueSettleRefsGrammarOwed queues the delete of the row one source owes the re-index queue
// (refs_grammar_owed), in the write's own transaction, which holds that source's owner: every
// write of the text was committed before this transaction read it, so whatever enqueued_at the
// row holds, the text this write indexed is the text the row is owed for. It is the queue's four
// source kinds that are settled this way - artifact, ask, comment and message, which are the
// from_kind of every refs row too, so one delete serves them all. The fifth kind, `scope`, names
// a document whose sources are owed a re-index, not a source whose refs this writes, and only
// the scope drain settles it. A from_id that is no uuid owes nothing (owed).
func (r Replacement) queueSettleRefsGrammarOwed(b *store.WriteBatch) {
	if r.owed == "" {
		return
	}
	b.Queue(fmt.Sprintf("settle the index owed by %s %s", r.fromKind, r.fromID),
		`delete from refs_grammar_owed where kind = $1 and id = $2::uuid`, r.fromKind, r.owed)
}

func scanTargets(rows pgx.Rows, err error) ([]Target, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := []Target{}
	for rows.Next() {
		var target Target
		if err := rows.Scan(&target.Kind, &target.ID); err != nil {
			return nil, err
		}
		changed = append(changed, target)
	}
	return changed, rows.Err()
}

// TargetOf is the graph target a parsed reference names, the kind and id refs and graph_edges
// store an edge to it under; an external URL names none. A block of a comment, of a message or
// of an ask — a part of its question, its options and its answer — is stored under its item's
// block kind, comment_block, message_block or ask_block (ItemKind.BlockKind), not block: a build
// that predates item blocks skips a kind it does not know (resolveNodes), where it would count a
// block row and resolve it through document_blocks.
func TargetOf(ref text.Ref) (Target, bool) {
	if ref.Kind == "url" {
		return Target{}, false
	}
	kind := ref.Kind
	if kind == "block" && ref.Item != "" {
		kind = ItemKind(ref.Item).BlockKind()
	}
	return Target{Kind: kind, ID: text.TargetID(ref)}, true
}

// refTargets is the deduplicated (to_kind, to_id) set found names, as parallel arrays.
func refTargets(found []text.Ref) ([]string, []string) {
	kinds, ids := []string{}, []string{}
	seen := make(map[Target]struct{})
	for _, ref := range found {
		target, ok := TargetOf(ref)
		if !ok {
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		kinds = append(kinds, target.Kind)
		ids = append(ids, target.ID)
	}
	return kinds, ids
}

// Stamp records eventID as the write that introduced every edge of one source that has no
// provenance yet: the source's own refs and, for a document, a comment, a message or an ask, the
// mentions its blocks hold, an ask's parts included. It runs after the source's event is
// appended, in the same transaction, and leaves edges that survived a reconcile untouched. Its
// statements run at once, as a batch of their own.
func Stamp(ctx context.Context, tx pgx.Tx, fromKind, fromID string, eventID int64) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueStamp(b, fromKind, fromID, &eventID)
		return nil
	})
}

// QueueStamp queues Stamp on a write phase's batch, right after the source's event, whose id the
// statements take as store.LatestEventID: the id the event's insert took, which nothing in Go
// reads before they are sent.
func QueueStamp(b *store.WriteBatch, fromKind, fromID string) {
	queueStamp(b, fromKind, fromID, nil)
}

// queueStamp queues Stamp's statements with eventID, or with store.LatestEventID when it is nil.
func queueStamp(b *store.WriteBatch, fromKind, fromID string, eventID *int64) {
	refsID, refsArgs := store.LatestEventID, []any{fromKind, fromID}
	blocksID, blocksArgs := store.LatestEventID, []any{fromID}
	if eventID != nil {
		refsID, refsArgs = "$3", append(refsArgs, *eventID)
		blocksID, blocksArgs = "$2", append(blocksArgs, *eventID)
	}
	b.Queue("stamp references", `
		update refs set source_seq = `+refsID+`
		where from_kind = $1 and from_id = $2 and source_seq is null
	`, refsArgs...)
	var blockRefs string
	switch fromKind {
	case "artifact":
		blockRefs = `update block_refs set source_seq = ` + blocksID + ` where artifact_id = $1 and source_seq is null`
	case string(ItemComment), string(ItemMessage), string(ItemAsk):
		// An item's blocks are keyed by its uuid; an id that is none names no item and holds no
		// block mention.
		if uuid.Validate(fromID) != nil {
			return
		}
		tables, _ := ItemKind(fromKind).tables()
		blockRefs = `update ` + tables.refs + ` set source_seq = ` + blocksID + ` where ` + tables.item + ` = $1::uuid and source_seq is null`
	default:
		return
	}
	b.Queue("stamp block references", blockRefs, blocksArgs...)
}

// CountedChangeLimit bounds the targets of one kind (issue, ask, artifact, block) an event names,
// so an event carries at most four times as many. Each kind is capped on its own: an index whose
// one paragraph cites forty documents still names every one and every issue beside them, and a
// reader falls back only for the kind it was not told about. A hundred entries of the longest
// kind, a block (its node id, its reference and its document's uuid, about 200 bytes), is some
// 20 KB, so a full event of four kinds stays under 100 KB: about what an event at
// contracts.DispatchEventBodyMax already carries, and far under NATS's 1 MiB maximum payload
// that the outbox publishes it through. A body citing more is rare enough that carrying an
// unbounded array on every event is the wrong trade.
const CountedChangeLimit = 100

// countedKinds is the order CountedChanges lists the kinds it truncated in.
var countedKinds = [...]string{"issue", "ask", "artifact", "block"}

// CountedChanges names the changed targets a reader refreshes by name — an issue (its detail, its
// count and its references read), an ask (every list that carries the ask row, plus the human
// Inbox), a document or file (its Referenced by panel) and a block of a document (its own panel
// and every link to it, which read one graph read, and its document's panel, which lists the
// edges at its blocks) — in the shape those readers are keyed by. A part of an ask is named as its
// ask, whose cards and panel are what a link to one of its parts moves. Comments, messages, their
// blocks and components are not named themselves: nothing counts them, and their panels refresh
// on the graph itself.
//
// A target that is an issue's own content — a document of the issue or a block of one, an ask of
// it or a part of one, a comment or a message on it or a block of either — also names that issue:
// the issue's references read lists what links into its content, so a link added to or removed
// from that content changes the read, and a page holding it refreshes it on a target of kind issue
// (dispatch://INFRA-4704, R6). Each such issue is named once, after every target named above, in
// the order of its first content target, and not where the write names it already. Content no
// issue owns (a project's or an agent's document, the ask or the comment of a project document, a
// direct message) names no issue, and nor does a document this transaction has only queued: the
// write that queues it appends an event of the document's own issue, which refreshes that issue's
// read. Each target's issue is read by key: a document's and an ask's from the row the target is
// resolved by, and a comment's and a message's, a block's through the item its node id names, by
// one primary key read of each kind.
//
// A kind with more than CountedChangeLimit named targets, the issues of content targets counted
// among the issues, names none of them and is listed in TruncatedKinds, and the reader falls back
// to refreshing every read of that kind; the other kinds are still named.
//
// A reference the grammar accepts need not address anything: `dispatch://CORE-1/ask/hello`
// parses and is stored, so every lookup here compares text and a target that resolves to no
// row is simply not named. A write must never fail because of what its body cites, with one
// exception: a typed block's `block[]` attribute is a schema field, not prose, and an agent's or
// API write introducing a pointer is refused when it names a block of a type the attribute's
// targets do not allow, a block no document holds, or a block outside the issues and documents
// the attribute may point into (pmdoc.CheckBlockPointers). A browser's edit is never refused, so
// a pointer it leaves at a block no document holds stays unresolved.
func CountedChanges(ctx context.Context, q Queryer, changed []Target) (model.ReferenceChanges, error) {
	// counted is a target to name and the target it is resolved by: a block is resolved through
	// its document, the ref_key before the first '#', and any other target as itself.
	type counted struct{ target, lookup Target }
	// lookups is the ids of each kind of target to resolve, in the targets' order.
	lookups := map[string][]string{}
	ordered := make([]counted, 0, len(changed))
	seen := make(map[[2]string]struct{}, len(changed))
	for _, target := range changed {
		// A block of an ask, a comment or a message is counted as its item, the one its node id
		// names, so two blocks of one item, or a block and the item's own row, count that item
		// once.
		if kind, itemID, _, ok := ParseItemBlockNode(target.ID); ok && target.Kind == kind.BlockKind() {
			target = Target{Kind: string(kind), ID: itemID}
		}
		if _, duplicate := seen[[2]string{target.Kind, target.ID}]; duplicate {
			continue
		}
		seen[[2]string{target.Kind, target.ID}] = struct{}{}
		lookup := target
		if target.Kind == "block" {
			document, _, _ := strings.Cut(target.ID, "#")
			lookup = Target{Kind: "artifact", ID: document}
		}
		lookups[lookup.Kind] = append(lookups[lookup.Kind], lookup.ID)
		ordered = append(ordered, counted{target: target, lookup: lookup})
	}
	issueKeys, err := resolveIssueTargets(ctx, q, lookups["issue"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	// resolveIssueTargets answers which keys name an issue, the set Reader.Issues reads as well;
	// an issue is named as itself, and is no issue's content.
	issues := make(map[string]resolvedTarget, len(issueKeys))
	for key := range issueKeys {
		issues[key] = resolvedTarget{reference: &model.ChangedReference{Kind: "issue", ID: key}}
	}
	asks, err := resolveAskTargets(ctx, q, lookups["ask"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	artifacts, err := resolveArtifactTargets(ctx, q, lookups["artifact"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	comments, err := resolveItemIssues(ctx, q, ItemComment, lookups["comment"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	messages, err := resolveItemIssues(ctx, q, ItemMessage, lookups["message"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	// resolved is each kind's resolved targets, keyed as the targets of that kind are looked up.
	// A target of a kind none resolves names nothing.
	resolved := map[string]map[string]resolvedTarget{
		"issue": issues, "ask": asks, "artifact": artifacts, "comment": comments, "message": messages,
	}
	references := make([]model.ChangedReference, 0, len(ordered))
	// owners is the issue of each content target, in the targets' order, an issue as often as
	// its content is changed.
	owners := make([]string, 0, len(ordered))
	for _, change := range ordered {
		entry, found := resolved[change.lookup.Kind][change.lookup.ID]
		if !found {
			continue
		}
		if change.target.Kind == "block" {
			entry = blockTarget(entry, change.target.ID)
		}
		if entry.reference != nil {
			references = append(references, *entry.reference)
		}
		if entry.issue != "" {
			owners = append(owners, entry.issue)
		}
	}
	issuesNamed := make(map[string]struct{}, len(references)+len(owners))
	for _, reference := range references {
		if reference.Kind == "issue" {
			issuesNamed[reference.ID] = struct{}{}
		}
	}
	for _, owner := range owners {
		if _, named := issuesNamed[owner]; named {
			continue
		}
		issuesNamed[owner] = struct{}{}
		references = append(references, model.ChangedReference{Kind: "issue", ID: owner})
	}
	named := make(map[string]int, len(countedKinds))
	for _, reference := range references {
		named[reference.Kind]++
	}
	changes := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if named[kind] > CountedChangeLimit {
			changes.TruncatedKinds = append(changes.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			changes.Targets = append(changes.Targets, reference)
		}
	}
	return changes, nil
}

// resolvedTarget is a changed target as its resolver answers it for CountedChanges: reference,
// the entry its readers are keyed by, nil for a target named by its issue alone (a comment, a
// message), and issue, the key of the issue whose own content the target is, empty for a target
// that is no issue's content (an issue itself, and content no issue owns) and for a document the
// reading transaction has only queued.
type resolvedTarget struct {
	reference *model.ChangedReference
	issue     string
}

// blockTarget is block nodeID of the document resolved as document, as CountedChanges names it:
// its node id, its document's uuid and its reference, the block's id after the first '#' under
// its document's reference (BlockRef), and the issue that owns the document.
func blockTarget(document resolvedTarget, nodeID string) resolvedTarget {
	_, blockID, _ := strings.Cut(nodeID, "#")
	artifactID := document.reference.ID
	return resolvedTarget{
		reference: &model.ChangedReference{
			Kind:       "block",
			ID:         nodeID,
			ArtifactID: &artifactID,
			Ref:        BlockRef(document.reference.Ref, blockID),
		},
		issue: document.issue,
	}
}

// MergeChanges is what two counted parts of one write moved, for an event that reports both: each
// target once, first's before second's. A kind either part truncated stays truncated and names
// none of its targets, and a kind whose merged targets pass CountedChangeLimit is truncated, as
// CountedChanges would have left it.
func MergeChanges(first, second model.ReferenceChanges) model.ReferenceChanges {
	truncated := make(map[string]bool, len(countedKinds))
	for _, kind := range slices.Concat(first.TruncatedKinds, second.TruncatedKinds) {
		truncated[kind] = true
	}
	references := make([]model.ChangedReference, 0, len(first.Targets)+len(second.Targets))
	seen := make(map[[2]string]struct{}, len(first.Targets)+len(second.Targets))
	named := make(map[string]int, len(countedKinds))
	for _, reference := range slices.Concat(first.Targets, second.Targets) {
		key := [2]string{reference.Kind, reference.ID}
		if _, duplicate := seen[key]; duplicate || truncated[reference.Kind] {
			continue
		}
		seen[key] = struct{}{}
		references = append(references, reference)
		named[reference.Kind]++
	}
	merged := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if truncated[kind] || named[kind] > CountedChangeLimit {
			merged.TruncatedKinds = append(merged.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			merged.Targets = append(merged.Targets, reference)
		}
	}
	return merged
}

// resolveIssueTargets is which of keys names an issue: one whose row is stored, and one whose
// row the reading transaction has queued and not written yet (Pending), which a new issue's
// spec writing its own key bare names.
func resolveIssueTargets(ctx context.Context, q Queryer, keys []string) (map[string]struct{}, error) {
	found := map[string]struct{}{}
	if len(keys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `select key from issues where key = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed issue references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan changed issue reference: %w", err)
		}
		found[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, keys, func(pending Pending, key string) (struct{}, bool) {
		return struct{}{}, pending.PendingIssue(key)
	})
	return found, nil
}

// resolveAskTargets skips a citation that is not a uuid rather than casting the column to text:
// `dispatch://CORE-1/ask/hello` parses and must never fail the write, and a target nothing can
// address resolves to nothing either way — while `id = any(...)` keeps the asks primary key,
// which matters because this runs inside the write transaction of every body that cites an ask.
//
// An ask whose row the reading transaction has queued and not written yet is named from what
// that insert writes (Pending), as the stored ones are: settlement opens the ask of every new
// question block of an edit in one transaction, so a `#id` naming another new block of that
// same edit is stored as the edge of an ask no read of the table answers for.
func resolveAskTargets(ctx context.Context, q Queryer, ids []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	addressable := uuidsOf(ids)
	if len(addressable) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, issue_key, artifact_id::text
		from asks
		where id = any($1)
	`, addressable)
	if err != nil {
		return nil, fmt.Errorf("resolve changed ask references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		reference := model.ChangedReference{Kind: "ask"}
		if err := rows.Scan(&reference.ID, &reference.IssueKey, &reference.ArtifactID); err != nil {
			return nil, fmt.Errorf("scan changed ask reference: %w", err)
		}
		found[reference.ID] = askTarget(reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, ids, func(pending Pending, id string) (resolvedTarget, bool) {
		ask, queued := pending.PendingAsk(id)
		if !queued {
			return resolvedTarget{}, false
		}
		return askTarget(model.ChangedReference{Kind: "ask", ID: ask.ID, IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID}), true
	})
	return found, nil
}

// askTarget is the ask whose entry is reference as CountedChanges names it: that entry, and the
// issue it is on, the one whose content it is.
func askTarget(reference model.ChangedReference) resolvedTarget {
	target := resolvedTarget{reference: &reference}
	if reference.IssueKey != nil {
		target.issue = *reference.IssueKey
	}
	return target
}

// uuidsOf is each of ids that parses as a uuid, the rest skipped: a citation the grammar accepts
// need not be a uuid (`dispatch://CORE-1/comment/hello`), and a resolver that compares the uuid
// key column with these, rather than casting the column to text, keeps its primary key.
func uuidsOf(ids []string) []uuid.UUID {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if value, err := uuid.Parse(id); err == nil {
			parsed = append(parsed, value)
		}
	}
	return parsed
}

// resolveArtifactTargets resolves cited artifacts by the ref_key `refs` stores for them, the
// address GET /api/v1/references?to= resolves too, into the entry a Referenced by panel is
// matched by: the artifact's uuid (its node id in that read) and its dispatch:// address (the
// reference the dashboard opens the panel on, refs.ArtifactRef), beside the issue that owns it
// (CountedChanges names that issue too), none for an artifact no issue owns (a project's, an
// agent's). Keyed by ref_key. A document whose artifacts row the reading transaction has queued
// and not written yet is resolved from what that insert writes (Pending), which names no issue:
// a new issue's spec citing its own address names a row this transaction has only queued.
func resolveArtifactTargets(ctx context.Context, q Queryer, refKeys []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	if len(refKeys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, ref_key, issue_key, coalesce(project_key, ''), session_id, slug, kind, is_primary
		from artifacts
		where ref_key = any($1)
	`, refKeys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed artifact references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, kind string
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan changed artifact reference: %w", err)
		}
		artifact := resolvedTarget{reference: &model.ChangedReference{
			Kind: "artifact",
			ID:   id,
			Ref:  ArtifactRef(issueKey, project, session, slug, kind, primary),
		}}
		if issueKey != nil {
			artifact.issue = *issueKey
		}
		found[refKey] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, refKeys, func(pending Pending, refKey string) (resolvedTarget, bool) {
		id, ref, queued := pending.PendingArtifact(refKey)
		if !queued {
			return resolvedTarget{}, false
		}
		return resolvedTarget{reference: &model.ChangedReference{Kind: "artifact", ID: id, Ref: ref}}, true
	})
	return found, nil
}

// resolveItemIssues resolves each of ids that names a comment or a message of kind into the issue
// it is on, keyed by id, with no entry of its own (resolvedTarget): nothing counts a comment or a
// message. It reads the items table by its primary key in one statement. An id that is no uuid
// (uuidsOf), names no row, or names a row no issue owns - a comment of a project document, a
// direct message - answers nothing.
func resolveItemIssues(ctx context.Context, q Queryer, kind ItemKind, ids []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	addressable := uuidsOf(ids)
	if len(addressable) == 0 {
		return found, nil
	}
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		select id::text, issue_key
		from `+tables.items+`
		where id = any($1) and issue_key is not null
	`, addressable)
	if err != nil {
		return nil, fmt.Errorf("resolve the issues of changed %s references: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, issueKey string
		if err := rows.Scan(&id, &issueKey); err != nil {
			return nil, fmt.Errorf("scan the issue of a changed %s reference: %w", kind, err)
		}
		found[id] = resolvedTarget{issue: issueKey}
	}
	return found, rows.Err()
}

// mergePending fills the gaps a read of the stored rows leaves. found is what that read
// answered and keys is what it was asked about: for each key found holds no target for, queued
// says whether the reading transaction has that row queued and not written yet (Pending), and
// what it answers goes into found. A stored row wins over a queued one of the same key, since
// the read already holds what the row says, and a transaction with nothing queued - every read
// outside a write's two phases - adds nothing.
//
// Every resolver that answers for a queued row merges through this one call, so a kind added to
// Pending is merged the way the kinds before it are.
func mergePending[Key comparable, Target any](
	ctx context.Context,
	found map[Key]Target,
	keys []Key,
	queued func(pending Pending, key Key) (Target, bool),
) {
	pending := pendingOf(ctx)
	if pending == nil {
		return
	}
	for _, key := range keys {
		if _, stored := found[key]; stored {
			continue
		}
		if target, ok := queued(pending, key); ok {
			found[key] = target
		}
	}
}

// ReferencedBy lists Dispatch items and artifacts that mention an artifact reference key or one
// of its current blocks, once per source, in the shape the document surfaces read (kind, id
// order). A citing document, comment, message or ask is listed itself, not by the block holding
// its link: an ask that cites the document from one of its parts alone — an option, or the
// answer it was given — is listed as that ask, once. A source who may not see is listed to
// nobody but its author.
func ReferencedBy(ctx context.Context, q Queryer, who model.Actor, refKey string) ([]model.ReferencedBy, error) {
	edges, err := resolveEdges(ctx, q, who, Query{Direction: "in", Kind: "artifact", ID: refKey, Kinds: []string{"mentions"}, WholeDocuments: true})
	if err != nil {
		return nil, err
	}
	references := make([]model.ReferencedBy, 0, len(edges))
	// Every edge of one source carries that source's node and excerpt, so the entry kept is
	// the same whichever of its edges comes first.
	seen := make(map[[2]string]struct{}, len(edges))
	for _, edge := range edges {
		source := [2]string{edge.Node.Kind, edge.Node.ID}
		if _, listed := seen[source]; listed {
			continue
		}
		seen[source] = struct{}{}
		reference := model.ReferencedBy{
			Kind:     edge.Node.Kind,
			ID:       edge.Node.ID,
			IssueKey: edge.Node.IssueKey,
			Project:  edge.Node.Project,
			RefKey:   edge.refKey,
		}
		if edge.Excerpt != nil {
			reference.Excerpt = edge.Excerpt.Text
		}
		references = append(references, reference)
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].Kind != references[right].Kind {
			return references[left].Kind < references[right].Kind
		}
		return references[left].ID < references[right].ID
	})
	return references, nil
}

// Outgoing lists Dispatch mentions written by an artifact, resolving artifact targets. A block
// target keeps its own kind and id and carries its document as the artifact, while a current
// version holds the block; a block none holds is omitted, as is a target of a node kind this
// build does not know. A block of an ask, a comment or a message, which this shape has no field
// for, is listed by the graph read (Edges) alone. Any other unresolved target is listed.
//
// It answers for who. A document who may not see writes no mention here, so its mentions say
// nothing of it, and a target who may not see is no mention at all - for a block, neither the
// block nor the document it carries (VisibleKind, VisibleEnd). The document is read by its uuid
// through artifacts' primary key, since the store keys it by ref_key and the registry's own
// rewrite compares the uuid as text, which no index serves.
func Outgoing(ctx context.Context, q Queryer, who model.Actor, artifactID string) ([]model.OutgoingReference, error) {
	rows, err := q.Query(ctx, `
		select e.to_kind, e.to_id,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, coalesce(a.ref_key, ''), coalesce(a.slug, ''), coalesce(a.name, ''), coalesce(a.kind, ''), coalesce(a.is_primary, false),
		       coalesce(a.created_by, '{}'::jsonb), coalesce(a.created_at, timestamptz 'epoch'),
		       coalesce(v.number, 0), coalesce(v.named, false), v.summary, coalesce(v.authors, '[]'::jsonb), coalesce(v.created_at, timestamptz 'epoch'), `+model.VersionSize+`, v.mime, v.sha256
		from graph_edges e
		left join document_blocks d on e.to_kind = 'block' and d.node_id = e.to_id
		left join artifacts a on a.ref_key = case e.to_kind when 'artifact' then e.to_id when 'block' then split_part(d.node_id, '#', 1) end
		left join artifact_versions v on v.artifact_id = a.id
		where e.from_kind = 'artifact' and e.from_id = $1 and e.kind = 'mentions' and e.to_kind = any($2)
		  and e.to_kind not in `+itemBlockKinds+`
		  and (e.to_kind <> 'block' or d.node_id is not null)
		  and `+VisibleKind("artifact", artifactRefKeyOf(`$1::text::uuid`), "$3")+`
		  and `+VisibleEnd("e.to_kind", "e.to_id", true, "$3")+`
		  and (e.to_kind <> 'block' or `+VisibleKind("artifact", "a.ref_key", "$3")+`)
		order by e.to_kind, e.to_id, v.number
	`, artifactID, nodeKinds, VisibilityArg(who))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	outgoing := []model.OutgoingReference{}
	positions := make(map[string]int)
	for rows.Next() {
		var reference model.OutgoingReference
		var artifactIDValue *string
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&reference.Kind,
			&reference.ToID,
			&artifactIDValue,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.SessionID,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&createdBy,
			&artifact.CreatedAt,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		if artifactIDValue == nil {
			outgoing = append(outgoing, reference)
			continue
		}
		artifact.ID = *artifactIDValue
		if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
			return nil, fmt.Errorf("decode artifact author: %w", err)
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode artifact version authors: %w", err)
		}
		if position, found := positions[reference.ToID]; found {
			outgoing[position].Artifact.Versions = append(outgoing[position].Artifact.Versions, version)
			continue
		}
		artifact.Versions = []model.Version{version}
		reference.Artifact = &artifact
		positions[reference.ToID] = len(outgoing)
		outgoing = append(outgoing, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, reference := range outgoing {
		if reference.Artifact != nil {
			model.StateFileTokens(reference.Artifact.Kind, reference.Artifact.Versions)
		}
	}
	return outgoing, nil
}
