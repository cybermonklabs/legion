package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A child issue is a view on its ancestor's spec (R23 of the issue-tree spec): its checks serve
// the requirements of the nearest issue above it that holds requirement nodes, is no view itself
// and is not done, and a requirement written on it is refused with that ancestor's name, so the
// work it does is the work that ancestor states. Whether an issue is a view is read from its own
// row of issue_views and from the issues above it, and from nothing else: never from whether it
// holds an issue_trees row, which an issue born converted and one the conversion reached holding
// requirements of its own both hold, and never from a requirement node of its own, since one that
// reached a view through its room is a gap the coverage read names and does not end the view.
//
// ViewOf is the one reader of whether an issue is a view: the tree read's view_of member, the
// coverage reads (through QueueViewOf, which queues ViewOf's one statement on a batch of the
// reader's), the spec query, the refusal of a requirement written on such a child
// (RefuseRequirementOnView) and the block index's own-spec mark (markOwnSpecIndexed, which reads
// the row's mark alone first) read it. An issue create reads ViewUnder for the issue it makes,
// whose own row is not written yet (RefuseRequirementUnder). Each reads the chain in one statement
// (viewChainStatement) and judges it by one rule (viewOfChain), so none of them disagree.

// requirementType is the typed block a requirement is.
const requirementType = "requirement"

// viewChainStatement reads the issue $1 and each issue above it up to the root, at most
// refs.IssueDepthCap above it, as refs.IssueAncestors climbs, in that order: each one's key,
// whether it is done, whether its row of issue_views lets it be a view - it says born, or t19b
// with the conversion's view mark, and holds no own-spec mark; an issue with no row may not be one
// - and, from the depth $3 up, whether it holds a requirement node: a document_blocks row of type
// requirement of one of its artifacts, its tree's among them, which the block index writes for
// every current requirement block and deletes once a version drops it. Below that depth it reads
// no requirement node, so ViewOf reads none of the issue's own.
var viewChainStatement = `
	with recursive chain as (
		select i.key, i.parent_key, 0 as depth from issues i where i.key = $1
		union
		select i.key, i.parent_key, c.depth + 1
		from issues i join chain c on i.key = c.parent_key
		where c.depth < $2
	)
	select c.key, i.status = 'done',
	       coalesce((v.converted = 'born' or v.converted = 't19b' and v.view_marked) and v.own_spec_at is null, false),
	       c.depth >= $3 and exists (
	           select 1 from document_blocks d join artifacts doc on doc.id = d.artifact_id
	           where doc.issue_key = c.key and d.type = '` + requirementType + `')
	from chain c
	join issues i on i.key = c.key
	left join issue_views v on v.issue_key = c.key
	order by c.depth`

// chainIssue is what viewChainStatement reads of one issue of a chain.
type chainIssue struct {
	key                         string
	done, viewable, requirement bool
}

// readViewChain is the chain viewChainStatement reads from the issue key up, through q, with
// the requirement nodes of each issue read from depth up: none of key's own where depth is 1.
// An issue the store holds no row of has an empty chain.
func readViewChain(ctx context.Context, q Queryer, key string, depth int) ([]chainIssue, error) {
	rows, err := q.Query(ctx, viewChainStatement, key, refs.IssueDepthCap, depth)
	if err != nil {
		return nil, fmt.Errorf("read the issues from %s up: %w", key, err)
	}
	return collectViewChain(rows, key)
}

// collectViewChain is the chain the rows of viewChainStatement hold, read from the issue key up.
func collectViewChain(rows pgx.Rows, key string) ([]chainIssue, error) {
	chain, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (chainIssue, error) {
		var issue chainIssue
		return issue, row.Scan(&issue.key, &issue.done, &issue.viewable, &issue.requirement)
	})
	if err != nil {
		return nil, fmt.Errorf("read the issues from %s up: %w", key, err)
	}
	return chain, nil
}

// chainAncestors is the keys of the issues above the issue a chain read from it up (readViewChain)
// holds, parent first: refs.IssueAncestors' answer, which climbs the same parent_key links to the
// same depth (refs.IssueDepthCap) in the same order, and [] where the store holds no row of the
// issue.
func chainAncestors(chain []chainIssue) []string {
	ancestors := make([]string, 0, max(len(chain)-1, 0))
	for index := 1; index < len(chain); index++ {
		ancestors = append(ancestors, chain[index].key)
	}
	return ancestors
}

// ViewOf is the issue whose requirements the issue issueKey is a view of, read through q, and ""
// where it is no view. An issue is a view where all three hold: its row of issue_views says it was
// born converted, or converted by the conversion with the conversion's view mark; its row holds no
// own-spec mark (QueueOwnSpec); and the nearest issue above it that holds requirement nodes and is
// not itself a view is not done. That issue is the one it is a view of. One statement reads the
// issue's row and the chain above it (viewChainStatement), and which issues of the chain are views
// is decided from the root down (viewOfChain), so the search passes over an ancestor that is
// itself a view, whose requirement is a gap and no part of a spec. It reads no requirement node of
// the issue itself: a requirement that reached a view through its room is a gap the coverage read
// names, not a requirement of its own. An issue no conversion reached is no view and writes as it
// always has; one whose spec is its own stands on its own whatever happens above it later; and one
// whose nearest such ancestor is done stands on its own, since that ancestor's spec can take no
// more work. An issue the store holds no row of is no view.
func ViewOf(ctx context.Context, q Queryer, issueKey string) (string, error) {
	chain, err := readViewChain(ctx, q, issueKey, 1)
	if err != nil {
		return "", err
	}
	return viewOfIssue(chain), nil
}

// QueueViewOf queues on batch the one statement ViewOf reads for the issue issueKey, so a reader
// that sends statements of its own in one round trip reads whether the issue is a view beside
// them (api's issueCoverage, which an approval request reads): once the batch is sent and closed
// without an error, *view holds what ViewOf answers.
func QueueViewOf(batch *pgx.Batch, issueKey string, view *string) {
	batch.Queue(viewChainStatement, issueKey, refs.IssueDepthCap, 1).Query(func(rows pgx.Rows) error {
		chain, err := collectViewChain(rows, issueKey)
		if err != nil {
			return err
		}
		*view = viewOfIssue(chain)
		return nil
	})
}

// viewOfIssue is ViewOf's answer from the chain viewChainStatement reads from the issue up with
// none of the issue's own requirement nodes: "" where the store holds no row of the issue or its
// row of issue_views lets it be no view, and otherwise the judgement of the chain above it.
func viewOfIssue(chain []chainIssue) string {
	if len(chain) == 0 || !chain[0].viewable {
		return ""
	}
	return viewOfChain(chain[1:])
}

// ViewUnder is ViewOf from a parent: the issue whose requirements an issue born converted with no
// own-spec mark, under the parent parentKey, is a view of, read through q, and "" where it would
// be none. The create reads it for the issue it makes, whose own row is not written yet. A root
// issue, whose parentKey is "", is no view.
func ViewUnder(ctx context.Context, q Queryer, parentKey string) (string, error) {
	if parentKey == "" {
		return "", nil
	}
	chain, err := readViewChain(ctx, q, parentKey, 0)
	if err != nil {
		return "", err
	}
	return viewOfChain(chain), nil
}

// viewOfChain is the issue an issue that may be a view is a view of, given the chain above it,
// its parent first: the nearest issue of the chain that holds requirement nodes and is not itself
// a view, where that issue is not done, and "" otherwise. Whether an issue of the chain is a view
// depends on the issues above it alone, so the chain is judged from the root down, each issue
// against the nearest such issue above it.
func viewOfChain(chain []chainIssue) string {
	var nearest *chainIssue
	for index := len(chain) - 1; index >= 0; index-- {
		issue := &chain[index]
		view := issue.viewable && nearest != nil && !nearest.done
		if issue.requirement && !view {
			nearest = issue
		}
	}
	if nearest == nil || nearest.done {
		return ""
	}
	return nearest.key
}

// QueueBornConverted queues on b the row of issue_views that says the issue issueKey was born
// converted, in the transaction of the create that makes it so (api's createIssueTree). It
// upserts on the issue's key, as every writer of the row does, so the writers meet in any order,
// and it moves no mark.
func QueueBornConverted(b *store.WriteBatch, issueKey string) {
	b.Queue("record that "+issueKey+" was born converted", `
		insert into issue_views (issue_key, converted) values ($1, 'born')
		on conflict (issue_key) do update set converted = excluded.converted`, issueKey)
}

// QueueOwnSpec queues on b the own-spec mark of the issue issueKey: its spec is its own, so it is
// never a view again (ViewOf). It is the one statement that sets the mark, which a caller queues
// only after ViewOf answers that the issue is no view in the same transaction: every write
// through the API that the refusal of a requirement on a view lets through, where the document it
// writes holds a requirement block (RefuseRequirementOnView, RefuseRequirementUnder), and every
// pass of the block index that states a requirement node of the issue (markOwnSpecIndexed). The
// mark keeps its first time (coalesce); setting it again changes nothing, and nothing clears it.
func QueueOwnSpec(b *store.WriteBatch, issueKey string) {
	b.Queue("mark the spec of "+issueKey+" its own", `
		insert into issue_views (issue_key, own_spec_at) values ($1, now())
		on conflict (issue_key) do update set own_spec_at = coalesce(issue_views.own_spec_at, excluded.own_spec_at)`, issueKey)
}

// ErrRequirementBelongsToAncestor refuses a write that adds a requirement node to an issue that
// is a view of Ancestor (ViewOf, ViewUnder): Issue's spec states no requirement of its own, so
// work Ancestor does not state goes into Ancestor's spec first. Blocks are the ids of the
// requirement blocks the write would add. Nothing of the write is written.
type ErrRequirementBelongsToAncestor struct {
	Issue, Ancestor string
	Blocks          []string
}

func (e *ErrRequirementBelongsToAncestor) Error() string {
	return fmt.Sprintf("%s is a view on the spec of %s and holds no requirement of its own, so this write's requirement %s belongs to %s: write it into the spec of %s (dispatch://%s/spec), and serve it from a check of %s",
		e.Issue, e.Ancestor, strings.Join(e.Blocks, ", "), e.Ancestor, e.Ancestor, e.Ancestor, e.Issue)
}

// requirementIDs is the id of each requirement block tree holds, in document order, an id held
// twice once.
func requirementIDs(tree *pmdoc.Node) []string {
	var ids []string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != requirementType {
			return true
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		return true
	})
	return ids
}

// holdsBlockType says whether tree holds a block of type typ anywhere.
func holdsBlockType(tree *pmdoc.Node, typ string) bool {
	held := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		held = held || node.Type == typ
		return !held
	})
	return held
}

// RefuseRequirementOnView refuses, with ErrRequirementBelongsToAncestor, a write to a document of
// the issue issueKey that leaves after holding a requirement block before did not hold, where that
// issue is a view (ViewOf), read through q. before is the document the write replaces, nil for a
// new document. Wherever after holds a requirement block it reads ViewOf once, for a write that
// adds none too: a write to a view that adds a requirement id is refused, one that adds none - a
// requirement a client brought in through the room, kept - is let through, and a write to an
// issue that is no view marks the issue's spec its own (QueueOwnSpec) on the write of ctx, the
// document write it is called in, so its transaction sets the mark. It is the one place each
// write through the API that writes a document sets the mark: the upload of a new document, the
// upload of a new version (ReplaceText) and both edit routes (finishBatch). A write whose after
// holds no requirement block reads nothing.
func RefuseRequirementOnView(ctx context.Context, q Queryer, issueKey string, before, after *pmdoc.Node) error {
	if issueKey == "" || !holdsBlockType(after, requirementType) {
		return nil
	}
	ancestor, err := ViewOf(ctx, q, issueKey)
	if err != nil {
		return err
	}
	if ancestor == "" {
		return queueOwnSpecIn(ctx, issueKey)
	}
	held := requirementIDs(before)
	var added []string
	for _, id := range requirementIDs(after) {
		if !slices.Contains(held, id) {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return requirementBelongsTo(issueKey, ancestor, added)
}

// RefuseRequirementUnder refuses, with ErrRequirementBelongsToAncestor, the spec of the issue
// issueKey that an issue create makes under the parent parentKey, born converted where born is
// set, where spec holds a requirement block and the new issue would be a view, read through q.
// The new issue's own row is not written yet: an issue born converted holds no own-spec mark, so
// it is a view where ViewUnder of its parent names an ancestor, and an issue created unconverted
// is no view. A spec let through holding a requirement block - of a root issue, an issue created
// unconverted, or a child born converted that ViewUnder names no ancestor for - marks the new
// issue's spec its own (QueueOwnSpec) on the write of ctx, the create's document write, so a
// create whose blocks the block index takes up later keeps the mark. A spec that holds no
// requirement block reads nothing.
func RefuseRequirementUnder(ctx context.Context, q Queryer, issueKey, parentKey string, born bool, spec *pmdoc.Node) error {
	if !holdsBlockType(spec, requirementType) {
		return nil
	}
	if born {
		ancestor, err := ViewUnder(ctx, q, parentKey)
		if err != nil {
			return err
		}
		if ancestor != "" {
			return requirementBelongsTo(issueKey, ancestor, requirementIDs(spec))
		}
	}
	return queueOwnSpecIn(ctx, issueKey)
}

// requirementBelongsTo is the refusal of the requirement blocks added to the issue, a view of
// ancestor, naming the blocks in sorted order.
func requirementBelongsTo(issue, ancestor string, added []string) error {
	slices.Sort(added)
	return &ErrRequirementBelongsToAncestor{Issue: issue, Ancestor: ancestor, Blocks: added}
}

// refuseRequirementOnViewOf is RefuseRequirementOnView for a write to the document artifactID,
// whose issue its row names. A document no issue holds is never refused and marks nothing.
func refuseRequirementOnViewOf(ctx context.Context, q Queryer, artifactID string, before, after *pmdoc.Node) error {
	if !holdsBlockType(after, requirementType) {
		return nil
	}
	var issue *string
	err := q.QueryRow(ctx, `select issue_key from artifacts where id = $1::uuid`, artifactID).Scan(&issue)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && issue == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the issue of document %s: %w", artifactID, err)
	}
	return RefuseRequirementOnView(ctx, q, *issue, before, after)
}

// queueOwnSpecIn queues the own-spec mark of issueKey (QueueOwnSpec) on the write of ctx's
// transaction (write): its ledger's write phase, where a ledger queues it. ctx is a document
// write's, joined to its transaction (Service.Join).
func queueOwnSpecIn(ctx context.Context, issueKey string) error {
	tx, joined := txFromContext(ctx)
	if !joined {
		return errUnjoined
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		QueueOwnSpec(b, issueKey)
		return nil
	})
}

// markOwnSpecIndexed marks, in tx, the spec of the issue issueKey its own where a pass of the
// block index over its documents states a requirement node (blocks, the blocks the pass indexes,
// hold a block of type requirement) and ViewOf answers in tx that the issue is no view, whatever
// wrote the requirement: a write through the API, a settlement of the issue's room, or the
// block-index reconciliation. The passes are the writer's (indexRenderedVersion), a settlement's
// room pass, the reconciliation's, and the typed passes of a write that leaves its other blocks to
// the reconciliation, which write its requirement nodes in its own transaction (planTypedPasses).
// It reads the issue's row of issue_views first, and a pass whose issue holds the mark already
// reads nothing more. A requirement the pass indexes on a view marks nothing, so the view stands
// and the requirement reads as its gap. A document no issue holds marks nothing.
func markOwnSpecIndexed(ctx context.Context, tx pgx.Tx, issueKey string, blocks []model.ArtifactBlock) error {
	if issueKey == "" || !slices.ContainsFunc(blocks, func(block model.ArtifactBlock) bool { return block.Type == requirementType }) {
		return nil
	}
	var marked bool
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from issue_views where issue_key = $1 and own_spec_at is not null)`, issueKey).Scan(&marked); err != nil {
		return fmt.Errorf("read whether the spec of %s is its own: %w", issueKey, err)
	}
	if marked {
		return nil
	}
	ancestor, err := ViewOf(ctx, tx, issueKey)
	if err != nil || ancestor != "" {
		return err
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		QueueOwnSpec(b, issueKey)
		return nil
	})
}
