// issue_relations.go is an issue's relations to other nodes as POST /api/v1/issues and PATCH
// /api/v1/issues/{key} write them (R9, R10 of the issue-tree spec): its parent (child_of), the
// issues it waits on (needs, which the request may also name blocked_by), the issue of its project
// it repeats (duplicate_of) and the ask, comment or message it was filed out of (filed_from). Each
// is a tuple out of the issue's node, which the write appends in its own transaction, caused by
// the issue's own event, once that event is appended (stateIssueRelations); issues.parent_key and
// the issue_links rows, which project child_of and needs, are written before that event in the
// same transaction. No row projects duplicate_of or filed_from, so the issue read reads both off
// the store (issueDuplicateOfColumn, issueFiledFromColumn).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The codes an issue's relations are refused by, each a 400 naming the field and the value it
// refuses. A wait's refusal takes the code of the member the request used (R10): blocked_by's for
// a request that names blocked_by alone, needs' for one that names needs.
const (
	codeNeedsInput       = "NEEDS_INPUT"
	codeBlockedByInput   = "BLOCKED_BY_INPUT"
	codeDuplicateOfInput = "DUPLICATE_OF_INPUT"
	codeFiledFromInput   = "FILED_FROM_INPUT"
)

// blockedByMember is the spelling of an issue's wait set a client of the release before reads and
// writes, which this release takes beside needs (model.EdgeKindNeeds) and answers beside it.
const blockedByMember = "blocked_by"

// issueRelationsRule is what the issue create and the issue PATCH say of the relations they write,
// in the route table's words.
const issueRelationsRule = "needs is the issues the issue waits on, of any project, sorted; blocked_by is the same list under " +
	"the spelling a client of the release before sends, and a body naming both names the same issues in each, else 400 " +
	"NEEDS_INPUT naming both. A list refused for its count or its keys is 400 NEEDS_INPUT, or BLOCKED_BY_INPUT where the " +
	"body named blocked_by alone; a wait that closes a ring through waits and parents is 409 DEPENDENCY_CYCLE; and a " +
	"credential that names one project is refused a target outside it with 403 PROJECT_SCOPE. duplicate_of is the key of " +
	"another issue of the issue's project, else 400 DUPLICATE_OF_INPUT; filed_from is the canonical reference of a public " +
	"ask, comment or message of the issue's project, dispatch://<KEY>/<ask|comment|message>/<uuid> or " +
	"dispatch://<PROJECT>/artifact/<slug>/<ask|comment>/<uuid>, else 400 FILED_FROM_INPUT, one text for an item of " +
	"another project, one no row holds and one reserved for its author; a refused relation writes nothing."

// issueWaits is the set of issues one create or PATCH says an issue waits on, sorted and each
// once, with the member the request named it by and the code a refusal of it carries.
type issueWaits struct {
	targets []string
	member  string
	code    string
}

// resolveIssueWaits reads the two members a write names an issue's waits by, each nil where the
// request leaves it out. provided is false where it names neither. A request that names needs
// alone is refused, where it is, under NEEDS_INPUT, and one that names blocked_by alone under
// BLOCKED_BY_INPUT; one that names both with the same issues, which a CLI of this release sends so
// that a Dispatch of the release before, which reads blocked_by alone, is served too, is served
// from them, and one whose two lists name different issues is refused NEEDS_INPUT, naming both.
// The cap counts what the request sent, before a key named twice is taken once, as the count the
// field has always been held to.
func resolveIssueWaits(needs, blockedBy *[]string) (issueWaits, bool, error) {
	if needs == nil && blockedBy == nil {
		return issueWaits{}, false, nil
	}
	waits := issueWaits{member: model.EdgeKindNeeds, code: codeNeedsInput}
	named := needs
	if needs == nil {
		waits.member, waits.code, named = blockedByMember, codeBlockedByInput, blockedBy
	}
	if needs != nil && blockedBy != nil && !slices.Equal(onceEach(*needs), onceEach(*blockedBy)) {
		return issueWaits{}, true, errorf(http.StatusBadRequest, codeNeedsInput,
			"needs [%s] and blocked_by [%s] name different issues: a request that names both names the same issues in each",
			strings.Join(*needs, ", "), strings.Join(*blockedBy, ", "))
	}
	if len(*named) > contracts.MaxIssueBlockers {
		return issueWaits{}, true, countExceededError(waits.code, waits.member, len(*named), contracts.MaxIssueBlockers)
	}
	waits.targets = onceEach(*named)
	return waits, true, nil
}

// onceEach is keys sorted, each once, in a slice of its own.
func onceEach(keys []string) []string {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

// optionalList is a create's list member as resolveIssueWaits reads one: nil where the body left
// it out or sent null, and the list otherwise, [] included.
func optionalList(list []string) *[]string {
	if list == nil {
		return nil
	}
	return &list
}

// parseNullableString decodes a write's member that holds one string, null, or nothing. Absent →
// (nil, false); JSON null → (nil, true); a string → its value, untrimmed; anything else → the
// refusal refuse words for the raw value.
func parseNullableString(raw json.RawMessage, refuse func(value string) error) (*string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, refuse(string(raw))
	}
	return &value, true, nil
}

// resolveDuplicateOf reads a write's duplicate_of against the store, in the write's transaction:
// one issue key of project, the writing issue's, that an issue holds, other than issue's own, or
// null. issue is "" for a create, whose key no issue holds yet. Any other value is refused 400
// DUPLICATE_OF_INPUT, naming the field and the value.
func resolveDuplicateOf(ctx context.Context, q queryer, issue, project string, raw json.RawMessage) (*string, bool, error) {
	refuse := func(value, why string) error {
		return errorf(http.StatusBadRequest, codeDuplicateOfInput,
			"duplicate_of %s %s: it names one issue of project %s by its key, other than the issue itself, or null", value, why, project)
	}
	target, provided, err := parseNullableString(raw, func(value string) error { return refuse(value, "is no issue key") })
	if err != nil || target == nil {
		return nil, provided, err
	}
	quoted := fmt.Sprintf("%q", *target)
	if *target == issue {
		return nil, true, refuse(quoted, "is the issue itself")
	}
	var targetProject string
	err = q.QueryRow(ctx, `select project_key from issues where key = $1`, *target).Scan(&targetProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, refuse(quoted, "is no issue Dispatch holds")
	}
	if err != nil {
		return nil, true, err
	}
	if targetProject != project {
		return nil, true, refuse(quoted, "is an issue of project "+targetProject)
	}
	return target, true, nil
}

// filedFromItem is the ask, comment or message a write's filed_from names: its node's kind and id,
// its uuid, and its canonical reference, which the issue read answers. node is what the write
// states of the item where its row is one the release before wrote and the store holds no node
// of it yet, nil where it holds one, and nil for a comment the store leaves out whole because an
// end of its relations is past the address bound (stateFiledFromNode).
type filedFromItem struct {
	kind, id, ref string
	node          *filedFromNode
}

// filedFromNode is the node of an item the store holds none of, and the facts its kind's writer
// states beside it: an ask's and a message's node (graphstore.AskNode, graphstore.MessageNode),
// and a comment's with the relations its row projects and who wrote it, as the backfill of the
// comments writes them (graphstore.CommentFacts, graphstore.AuthorFacts), whose row the write then
// names it on.
type filedFromNode struct {
	nodes  []graphstore.Node
	tuples []graphstore.Tuple
}

// filedFromKinds are the kinds of item a filed_from names: the conversation items of R9.
var filedFromKinds = []string{graphstore.KindAsk, graphstore.KindComment, graphstore.KindMessage}

// resolveFiledFrom reads a write's filed_from against the store, in the write's transaction: the
// canonical reference of one public ask, comment or message of project, the writing issue's, or
// null. The value is read as a request names a node (text.ParseRef) and taken only where it is
// that item's own canonical reference, whole, with its lower-case uuid:
// `dispatch://<KEY>/<ask|comment|message>/<uuid>` for an item of an issue, and
// `dispatch://<PROJECT>/artifact/<slug>/<ask|comment>/<uuid>` for one of a project document. The
// item's row is read by its id, and taken only where the row's owner is the issue or the project
// document the reference names, that owner is of project, and the item is public: its node is in
// project, not gone, and of visibility project rather than reserved for its author. A row the
// release before wrote, which carries no node yet, is all three by what its owner is, and the write
// states its node (filedFromItem.node). Every other value is refused 400 FILED_FROM_INPUT, naming
// the field and the value; an item of another project, an item no row holds and an item reserved
// for its author are refused with one text, so the refusal tells no caller that an item it may not
// read exists.
func resolveFiledFrom(ctx context.Context, q queryer, project string, raw json.RawMessage) (*filedFromItem, bool, error) {
	refuse := func(value, why string) error {
		return errorf(http.StatusBadRequest, codeFiledFromInput,
			"filed_from %s %s: it names one public ask, comment or message of project %s by its canonical reference, dispatch://<KEY>/<ask|comment|message>/<uuid> or dispatch://<PROJECT>/artifact/<slug>/<ask|comment>/<uuid>, or null",
			value, why, project)
	}
	value, provided, err := parseNullableString(raw, func(value string) error { return refuse(value, "is no reference") })
	if err != nil || value == nil {
		return nil, provided, err
	}
	quoted := fmt.Sprintf("%q", *value)
	ref, ok := text.ParseRef(*value, "")
	if !ok || !slices.Contains(filedFromKinds, ref.Kind) {
		return nil, true, refuse(quoted, "is no reference to an ask, a comment or a message")
	}
	canonical := ""
	if parsed, err := uuid.Parse(ref.ID); err == nil && parsed.String() == ref.ID {
		switch {
		case ref.IssueKey != "":
			canonical = "dispatch://" + ref.IssueKey + "/" + ref.Kind + "/" + ref.ID
		case ref.Project != "" && ref.Document != "":
			canonical = "dispatch://" + ref.Project + "/artifact/" + ref.Document + "/" + ref.Kind + "/" + ref.ID
		}
	}
	if canonical != *value {
		return nil, true, refuse(quoted, "is not the item's own canonical reference, written whole with its lower-case uuid")
	}
	item, public, err := readFiledFromItem(ctx, q, project, ref)
	if err != nil {
		return nil, true, err
	}
	if !public {
		return nil, true, refuse(quoted, "names no public ask, comment or message of project "+project)
	}
	item.ref = canonical
	return item, true, nil
}

// filedFromRow is what the read of an item's row answers: its owner, the issue it is on or the
// project document that holds it, that owner's project, who wrote it and when, and what its node
// is in the store, if it holds one. A comment's row answers too the addresses of the artifacts it
// is anchored to and owned by, and the artifact it is written on, whose events hold its
// comment.created where no issue owns it.
type filedFromRow struct {
	issue, project, document *string
	author                   []byte
	createdAt                time.Time
	nodeProject, visibility  *string
	state                    *string
	anchor, owner            *string
	ownerArtifact            *string
}

// readFiledFromItem reads the row of the item ref names, by its id, and answers whether it is a
// public item of project the reference names by its own owner (resolveFiledFrom), and the item.
// Only an item whose row carries no node yet reads more than its row and its node: a comment's
// comment.created event, within its owner (filedFromCommentEvent), which its node's facts carry.
func readFiledFromItem(ctx context.Context, q queryer, project string, ref text.Ref) (*filedFromItem, bool, error) {
	var statement string
	switch ref.Kind {
	case graphstore.KindAsk:
		statement = `
		select k.issue_key, coalesce(i.project_key, a.project_key), case when a.issue_key is null then a.slug end,
		       k.author, k.created_at, null::text, null::text, null::text
		from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
		where k.id = $1::uuid`
	case graphstore.KindComment:
		statement = `
		select c.issue_key, coalesce(i.project_key, owner.project_key), case when owner.issue_key is null then owner.slug end,
		       c.author, c.created_at, anchor.ref_key, owner.ref_key, c.artifact_id::text
		from comments c left join issues i on i.key = c.issue_key
		left join artifacts anchor on anchor.id = ` + store.CastableUUID(`c.anchor->>'artifact_id'`) + `
		left join artifacts owner on owner.id = c.artifact_id
		where c.id = $1::uuid`
	case graphstore.KindMessage:
		statement = `
		select m.issue_key, i.project_key, null::text, m.author, m.created_at, null::text, null::text, null::text
		from messages m left join issues i on i.key = m.issue_key
		where m.id = $1::uuid`
	}
	var row filedFromRow
	err := q.QueryRow(ctx, statement, ref.ID).Scan(&row.issue, &row.project, &row.document, &row.author, &row.createdAt,
		&row.anchor, &row.owner, &row.ownerArtifact)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the %s filed_from names: %w", ref.Kind, err)
	}
	ownedAsNamed := ref.IssueKey != "" && row.issue != nil && *row.issue == ref.IssueKey ||
		ref.IssueKey == "" && row.issue == nil && row.document != nil && *row.document == ref.Document &&
			row.project != nil && *row.project == ref.Project
	if !ownedAsNamed || row.project == nil || *row.project != project {
		return nil, false, nil
	}
	if err := q.QueryRow(ctx, `select project_key, visibility, state from graph_nodes where kind = $1 and id = $2`,
		ref.Kind, ref.ID).Scan(&row.nodeProject, &row.visibility, &row.state); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("read the node of the %s filed_from names: %w", ref.Kind, err)
	}
	item := &filedFromItem{kind: ref.Kind, id: ref.ID}
	if row.visibility != nil {
		public := row.nodeProject != nil && *row.nodeProject == project &&
			*row.visibility == graphstore.VisibilityProject && *row.state != graphstore.StateGone
		return item, public, nil
	}
	var author model.Actor
	if err := json.Unmarshal(row.author, &author); err != nil {
		return nil, false, fmt.Errorf("read the author of the %s filed_from names: %w", ref.Kind, err)
	}
	switch ref.Kind {
	case graphstore.KindAsk:
		item.node = &filedFromNode{nodes: []graphstore.Node{graphstore.AskNode(ref.ID, project, nil, author, row.createdAt)}}
	case graphstore.KindMessage:
		item.node = &filedFromNode{nodes: []graphstore.Node{graphstore.MessageNode(ref.ID, project, nil, author, row.createdAt)}}
	case graphstore.KindComment:
		// A comment any of whose relation ends is past the store's address bound is left whole
		// out of the store, as its own writer leaves it (commentStored) and as the backfill of the
		// comments never reads it (graphcensus' readCommentRows): no node, no fact and no stamp,
		// and the old comments arm answers both of its edges. The issue's filed_from still names
		// it, as it names any item.
		anchor, owner := derefString(row.anchor), derefString(row.owner)
		if !graphstore.AddressFits(anchor) || !graphstore.AddressFits(owner) {
			return item, true, nil
		}
		event, err := filedFromCommentEvent(ctx, q, row, ref.ID)
		if err != nil {
			return nil, false, err
		}
		node, tuples := graphstore.CommentFacts(nil, ref.ID, author, row.createdAt, event, anchor, owner, project, nil)
		item.node = &filedFromNode{nodes: []graphstore.Node{node}, tuples: tuples}
		if authors, facts, named := graphstore.AuthorFacts(graphstore.KindComment, ref.ID, author, row.createdAt, event); named {
			for _, written := range authors {
				written.Restates = true
				item.node.nodes = append(item.node.nodes, written)
			}
			item.node.tuples = append(item.node.tuples, facts...)
		}
	}
	return item, true, nil
}

// filedFromCommentEvent is the id of the comment.created event that stored the comment id whose
// row is row, or graphstore.NoEvent where its owner holds none. It reads the events of the
// comment's owner alone, its issue's or the artifact it is written on, through that owner's
// (issue_key, seq) or (artifact_id, seq) index, as loadCommentCreatedEvent reads the same event,
// rather than matching the payload id across the whole log, since it runs inside the write's
// transaction, under the issue's row and, with a parent or a wait, the deployment's dependency lock.
func filedFromCommentEvent(ctx context.Context, q queryer, row filedFromRow, id string) (int64, error) {
	ownerColumn, ownerID := commentEventOwner(model.Comment{IssueKey: row.issue, ArtifactID: row.ownerArtifact})
	var event int64
	err := q.QueryRow(ctx, `
		select id from events
		where `+ownerColumn+` = $1 and type = 'comment.created' and payload ->> 'id' = $2
		order by seq desc
		limit 1
	`, ownerID, id).Scan(&event)
	if errors.Is(err, pgx.ErrNoRows) {
		return graphstore.NoEvent, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the event that stored the comment filed_from names: %w", err)
	}
	return event, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// stateFiledFromNode states the node of the item a write's filed_from names where the store held
// none of it (filedFromItem.node), in the write's transaction, as its kind's own writer states it:
// an ask's or a message's node as the backfill of the nodes an older build wrote no node of writes
// it, at the sentinel, and a comment's node, its relations and who wrote it as the backfill of the
// comments writes them, under the event that stored the comment, with the comments row named on
// its node after both. A comment an end of whose relations is past the store's address bound is no
// node of the store, as readFiledFromItem reads it, so nothing is stated of it and its row names
// none. It runs after the issue's owner row and before the issue's own node, so the write takes
// every row of the store before its event; a backfill writing the same item meanwhile is the one
// other writer of it, and the second of the two writes moves nothing the first wrote.
func stateFiledFromNode(ctx context.Context, tx pgx.Tx, item *filedFromItem) error {
	if item == nil || item.node == nil {
		return nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, item.node.nodes, item.node.tuples)
		if item.kind == graphstore.KindComment {
			b.Queue("", `update comments set node_kind = $2, node_address = $1 where id = $1::uuid and node_kind is null and `+graphstore.KeptHeadRows,
				item.id, graphstore.KindComment)
		}
		graphstore.QueueHeads(b, graphstore.HeadsOf(item.node.nodes, item.node.tuples))
		return nil
	}); err != nil {
		return fmt.Errorf("state the node of the %s filed_from names: %w", item.kind, err)
	}
	return nil
}

// issueRelationFacts is what one create or PATCH states of an issue's relations, held until the
// issue's own event is appended, which causes every one of them (queueIssueRelations): the claims
// of its single-valued relations - child_of, duplicate_of and filed_from - and the tuples of its
// waits, of its affects to the components of its own and of the conversation words it no longer
// hangs from.
type issueRelationFacts struct {
	claims []graphstore.Claim
	tuples []graphstore.Tuple
}

// claim holds the issue at the one far end of kind toKind/toID names, or at none where toID is
// "", which retracts whatever far end it holds under kind.
func (f *issueRelationFacts) claim(issue, kind, toKind, toID string, at time.Time) {
	f.claims = append(f.claims, graphstore.Claim{
		FromKind: graphstore.KindIssue, FromID: issue, Kind: kind, ToKind: toKind, ToID: toID, At: at,
	})
}

// queueIssueRelations queues on b the append of what facts holds, written by who and caused by the
// issue's own event, which the write phase appends right before it (graphstore.LatestEvent): the
// claims as single-valued relations, which append only what moved (graphstore.Claim), and the
// tuples as they are, in one statement of the store's arms (graphstore.WriteArms). The projection
// rows - issues.parent_key, the issue_links rows and the issue_component_members rows - were
// written before the event, in the same transaction, so the two commit together. Its callers queue
// the head write of the nodes it states the relations of (heads) right after it, beside their own.
func queueIssueRelations(b *store.WriteBatch, facts issueRelationFacts, who model.Actor) error {
	if len(facts.claims) == 0 && len(facts.tuples) == 0 {
		return nil
	}
	for index := range facts.claims {
		facts.claims[index].Who, facts.claims[index].SourceEvent = who, graphstore.LatestEvent
	}
	for index := range facts.tuples {
		facts.tuples[index].Who, facts.tuples[index].SourceEvent = who, graphstore.LatestEvent
	}
	arms, params, err := graphstore.WriteArms(1, nil, facts.tuples, facts.claims)
	if err != nil {
		return err
	}
	b.Queue("state the relations of "+facts.issue(), "with "+arms+"\nselect 1", params...)
	return nil
}

// issue is the issue the facts are about, for an error that names it.
func (f issueRelationFacts) issue() string {
	if len(f.claims) > 0 {
		return f.claims[0].FromID
	}
	return f.tuples[0].FromID
}

// heads is the nodes the facts state relations out of, whose head rows they move.
func (f issueRelationFacts) heads() []graphstore.NodeKey {
	keys := make([]graphstore.NodeKey, 0, len(f.claims)+len(f.tuples))
	for _, claim := range f.claims {
		keys = append(keys, graphstore.NodeKey{Kind: claim.FromKind, ID: claim.FromID})
	}
	return append(keys, graphstore.HeadsOf(nil, f.tuples)...)
}

// strandedWords answers the retraction, dated at, of each part_of out of issue child's node to a
// conversation node - an ask, a comment or a message, or a block of one - that is no longer on its
// parent or above it once child_of names parent, nil for no parent (R2): the words that filed a
// child under one parent do not place it under another, and the child hangs from its new parent
// issue until someone places it again. Words of an item no issue holds, a project document's, are
// on no parent either. A part_of to any other node is no conversation word's, and stays.
func strandedWords(ctx context.Context, tx pgx.Tx, child string, parent *string, at time.Time) ([]graphstore.Tuple, error) {
	rows, err := tx.Query(ctx, `
		select held.to_kind, held.to_id from (
			`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindIssue+`' and t.from_id = $1 and t.kind = '`+graphstore.TuplePartOf+`'
				and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) held where held.asserted
	`, child)
	if err != nil {
		return nil, fmt.Errorf("read the words %s hangs from: %w", child, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var end [2]string
		err := row.Scan(&end[0], &end[1])
		return end, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the words %s hangs from: %w", child, err)
	}
	if len(held) == 0 {
		return nil, nil
	}
	chain := map[string]bool{}
	if parent != nil {
		above, err := refs.IssueAncestors(ctx, tx, *parent)
		if err != nil {
			return nil, err
		}
		for _, key := range append(above, *parent) {
			chain[key] = true
		}
	}
	var stranded []graphstore.Tuple
	for _, end := range held {
		item, id := end[0], end[1]
		if !slices.Contains(filedFromKinds, item) {
			kind, itemID, _, ok := refs.ParseItemBlockNode(id)
			if !ok {
				continue
			}
			item, id = string(kind), itemID
		}
		issue, err := conversationItemIssue(ctx, tx, item, id)
		if err != nil {
			return nil, err
		}
		if issue != "" && chain[issue] {
			continue
		}
		stranded = append(stranded, graphstore.Tuple{
			FromKind: graphstore.KindIssue, FromID: child, Kind: graphstore.TuplePartOf,
			ToKind: end[0], ToID: end[1], At: at, Asserted: false,
		})
	}
	return stranded, nil
}

// conversationItemIssue is the issue the ask, comment or message kind/id is on, its own or that of
// the document it is written on, "" for one no issue holds or no row holds.
func conversationItemIssue(ctx context.Context, q queryer, kind, id string) (string, error) {
	var statement string
	switch kind {
	case graphstore.KindAsk:
		statement = `select coalesce(k.issue_key, a.issue_key) from asks k left join artifacts a on a.id = k.artifact_id where k.id = $1::uuid`
	case graphstore.KindComment:
		statement = `select coalesce(c.issue_key, a.issue_key) from comments c left join artifacts a on a.id = c.artifact_id where c.id = $1::uuid`
	case graphstore.KindMessage:
		statement = `select m.issue_key from messages m where m.id = $1::uuid`
	default:
		return "", nil
	}
	var issue *string
	err := q.QueryRow(ctx, statement, id).Scan(&issue)
	if errors.Is(err, pgx.ErrNoRows) || issue == nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the issue the %s is on: %w", kind, err)
	}
	return *issue, nil
}

// issueDuplicateOfColumn is the key the duplicate_of tuple of the issue aliased `i` names now, null
// where it states none.
var issueDuplicateOfColumn = `(select held.to_id from (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
	`t.from_kind = '`+graphstore.KindIssue+`' and t.from_id = i.key and t.kind = '`+graphstore.TupleDuplicateOf+`'
			  and t.to_kind = '`+graphstore.KindIssue+`'`) + `
		) held where held.asserted order by held.to_id limit 1)`

// issueFiledFromColumn is the canonical reference of the item the filed_from tuple of the issue
// aliased `i` names now, written as resolveFiledFrom takes it, null where it states none or the
// item's row is gone.
var issueFiledFromColumn = `(select case held.to_kind
			when '` + graphstore.KindAsk + `' then ` + filedFromRef("asks", graphstore.KindAsk) + `
			when '` + graphstore.KindComment + `' then ` + filedFromRef("comments", graphstore.KindComment) + `
			when '` + graphstore.KindMessage + `' then (select 'dispatch://' || m.issue_key || '/message/' || m.id::text
				from messages m where m.id = ` + store.CastableUUID(`held.to_id`) + `)
		end from (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = '`+graphstore.KindIssue+`' and t.from_id = i.key and t.kind = '`+graphstore.TupleFiledFrom+`'
			  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
		) held where held.asserted order by held.to_kind, held.to_id limit 1)`

// filedFromRef is the canonical reference of the ask or comment of table whose id held.to_id names:
// under its issue, or under the project document that holds it.
func filedFromRef(table, kind string) string {
	return `(select coalesce('dispatch://' || item.issue_key || '/` + kind + `/' || item.id::text,
				'dispatch://' || document.project_key || '/artifact/' || document.slug || '/` + kind + `/' || item.id::text)
				from ` + table + ` item left join artifacts document on document.id = item.artifact_id and document.issue_key is null
				where item.id = ` + store.CastableUUID(`held.to_id`) + `)`
}
