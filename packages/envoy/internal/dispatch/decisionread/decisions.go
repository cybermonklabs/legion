// Package decisionread answers a decision node of the store in model.Decision's shape. A
// decision is one node: an agent's is a
// `decision` block node, and a person's is the answered ask's own `ask` node. This package is the
// one reader of what every read of a decision answers about it - its words, their text sum, the
// spec nodes it shaped with each link's `stale`, its review state, and the version it last changed
// in - so the reads that answer a decision answer the same values:
//
//   - the Decisions read's `entries` (api's getIssueDecisions), which takes each member below from
//     here and adds what only an entry carries;
//   - the Keep (api's keepDecision), which compares the posted `text_sha256` with TextSum of the
//     words this package reads, so a Keep holds the sum the read served;
//   - the Decisions read's old `decisions` member once it answers from the store;
//   - the tree read a later change adds, which calls Decisions for each decision node it draws.
//
// It is a package of its own because api's readers are unexported and neither docs nor refs may
// import api. It imports graphstore, refs and model, and neither api nor docs.
package decisionread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Node names one node Decisions reads: Kind and ID as graph_nodes keys it. A `decision` block
// node is {refs.BlockNodeKind, "<ref_key>#<block id>"}, and an ask's node is
// {graphstore.KindAsk, "<ask id>"}.
type Node struct{ Kind, ID string }

// TextSum is a decision's text sum: the sha256, in hex, of its words. A Keep records it as the
// node's kept_text_sum and posts it back as `text_sha256`, and an agent's decision waits for review
// until a Keep's sum is its sum as it stands. It is the one sum of a decision's text: this
// package's reads, the Keep and the tree read a later change adds all call it.
func TextSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Decisions answers each of nodes the store holds as a decision, in model.Decision's shape and in
// the order of nodes, reading for the person or session who: every statement binds the
// visibility predicate of the registry (refs.VisibleRow, refs.VisibleKind), so a node, or a spec
// node a decision shaped, that who may not see is absent. Each answer names its node in NodeKind
// and NodeID, which no wire shape carries, so a caller that asked for several finds each one.
//
// A node is read as a decision when it is a live block node of the `decision` supertag, or an ask
// node with an `asks` row. A caller lists as decisions the asks a person answered; an ask in any
// other answer state reads the same way, and the Decisions read takes the links of an open and a
// declined ask from here too. Any other node, and a node the store does not hold, is left out
// with no log (isDecision): the Keep reads the node at whatever block it was posted for.
//
// Each decision answers:
//
//   - Text, its words, and TextSHA256, their sum (TextSum). An agent's words are the decision's
//     own (refs.DecisionWords), read off the block's text as the block index recorded it last
//     (document_block_texts), so a reading it closes with is none of them. A decision whose
//     document owes its block index is read off the document's newest version, whose markdown
//     holds it as it stands (walker.changed). A person's words are the ask's question, from its
//     row: the answer is the ask node's, and no other field restates it.
//   - Kind, `person` for an ask and `agent` for a decision; Heading, the ask's question or the
//     decision's first paragraph; Ref, the node's own address; Asks, the ask an agent's decision
//     settles (from_ask) or the ask itself; Unsure, true where its uncertainty is `high`.
//   - Replaced, Kept and WaitingReview from the node: the newest kept_by tuple and the
//     kept_text_sum value, beside decision_keeps' changed_version,
//     and waiting_review true for an agent's decision unless it is replaced or the kept sum is its
//     sum as it stands.
//   - ChangedVersion, the version that last changed the decision's own words (refs.DecisionWords),
//     which a reading added, changed or removed under it does not move: the block's
//     document_blocks.changed_version, walked back over its
//     stored texts while the words' sum is unchanged (an ask's, its block's row); ChangedAt is that
//     version's created_at. A decision whose document owes its block index is walked over the
//     markdown of the versions the index has not recorded, newest first (walker.changed), and an
//     ask's words are its question and its options alone, no attribute of its block (ownWords).
//   - Links, one for each spec node a current `shaped` tuple reaches, FromBlock the node's own
//     block (the decision's, or the ask's). Event is the tuple's
//     source_event and CreatedAt its at; TextChangedEvent is the event that wrote the version the
//     target's text last changed in (its document_blocks.changed_version) and TextChangedAt that
//     version's created_at. Stale is true when the target's text changed after the tuple was
//     written: by the two events where both are held, else by the database's clock (R5). None of
//     the three computed members is stored.
//
// A node whose stored value does not parse - an uncertainty or a replaced outside its type, or
// words that do not read back - is left out and logged, and the read goes on with the others. A
// failure of the store fails the read. TextTree is left for the caller's own reader.
func Decisions(ctx context.Context, q graphstore.Queryer, who model.Actor, nodes []Node) ([]model.Decision, error) {
	decisions := []model.Decision{}
	if len(nodes) == 0 {
		return decisions, nil
	}
	kinds, ids := make([]string, len(nodes)), make([]string, len(nodes))
	for index, node := range nodes {
		kinds[index], ids[index] = node.Kind, node.ID
	}
	held, err := readNodes(ctx, q, who, kinds, ids)
	if err != nil {
		return nil, err
	}
	facts, err := readFacts(ctx, q, kinds, ids)
	if err != nil {
		return nil, err
	}
	links, err := readLinks(ctx, q, who, kinds, ids)
	if err != nil {
		return nil, err
	}
	versions := &walker{q: q, versions: map[versionKey]versionRead{}}
	for index := range nodes {
		node, found := held[index]
		// A node that is no decision is left out with no log: the Keep reads the node at whatever
		// address it was posted, a heading of a decisions document's section among them.
		if !found || !node.isDecision() {
			continue
		}
		decision, err := node.decision(ctx, versions, facts[index], links[index])
		if versions.err != nil {
			return nil, versions.err
		}
		if err != nil {
			slog.Warn("dispatch: a decision whose stored value does not parse is left out of the read",
				"kind", node.kind, "node", node.id, "err", err)
			continue
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

// heldNode is what the store holds of one node Decisions reads: the node row, the document that
// holds its block, an ask's row, its Keep's version, and what tells the version its words last
// changed in (walker.changed): its block's document_blocks row, every text the block index
// recorded of the block, the versions of the document that index recorded, the document's newest
// version, and whether the document owes its block index.
type heldNode struct {
	kind, id, supertag, state string

	document                               bool
	artifactID, refKey, slug, artifactKind string
	issueKey, project, session             *string
	primary                                bool
	question, askIssue                     *string
	blockID                                string
	rowVersion                             *int
	texts                                  []storedText
	indexed                                []int32
	latest                                 int
	owed                                   bool
	keptVersion                            *int
}

// nodesRead reads each named node, with the document holding its block and that block's rows,
// in one statement, for the person or session bound at $3, each table read by its own key. A
// block node's document is the one whose ref_key opens its address (a ref_key holds no '#'), and
// an ask's the document holding its block (asks.block_artifact_id). The block's texts are every
// text the block index recorded of it (document_block_texts), newest first, each beside its
// version's created_at; the document's indexed versions are document_indexed_versions', newest
// first; and the document owes its block index where document_block_versions holds no version.
func nodesRead() string {
	return `
	select w.n, g.kind, g.id, g.supertag, g.state,
	       doc.id is not null, coalesce(doc.id::text, ''), coalesce(doc.ref_key, ''), coalesce(doc.slug, ''),
	       coalesce(doc.kind, ''), doc.issue_key, doc.project_key, doc.session_id, coalesce(doc.is_primary, false),
	       q.question, q.issue_key, coalesce(place.block_id, ''),
	       row.changed_version,
	       (select json_agg(json_build_object('version', x.version, 'text', x.text, 'at', v.created_at) order by x.version desc)
	        from document_block_texts x
	        left join artifact_versions v on v.artifact_id = x.artifact_id and v.number = x.version
	        where x.artifact_id = doc.id and x.block_id = place.block_id),
	       (select array_agg(i.version order by i.version desc) from document_indexed_versions i where i.artifact_id = doc.id),
	       coalesce((select max(v.number) from artifact_versions v where v.artifact_id = doc.id), 0),
	       doc.id is not null and not exists (
	         select 1 from document_block_versions b where b.artifact_id = doc.id and b.version is not null),
	       (select k.changed_version from decision_keeps k
	        where g.kind = '` + refs.BlockNodeKind + `' and k.artifact_id = doc.id and k.block_id = place.block_id)
	from unnest($1::text[], $2::text[]) with ordinality as w (kind, id, n)
	join graph_nodes g on g.kind = w.kind and g.id = w.id and ` + refs.VisibleRow("g", "$3") + `
	left join asks q on g.kind = '` + graphstore.KindAsk + `'
		and q.id = case when g.id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then g.id::uuid end
	left join lateral (
		select a.* from artifacts a where g.kind = '` + graphstore.KindAsk + `' and a.id = q.block_artifact_id
		union all
		select a.* from artifacts a where g.kind = '` + refs.BlockNodeKind + `' and a.ref_key = split_part(g.id, '#', 1)
	) doc on true
	left join lateral (
		select b.artifact_id, b.block_id, b.changed_version from document_blocks b
		where g.kind = '` + refs.BlockNodeKind + `' and b.node_kind = '` + refs.BlockNodeKind + `' and b.node_address = g.id
		union all
		select b.artifact_id, b.block_id, b.changed_version from document_blocks b
		where g.kind = '` + graphstore.KindAsk + `' and b.artifact_id = q.block_artifact_id and b.block_id = q.block_id and b.type = 'ask'
	) row on true
	left join lateral (
		select coalesce(row.block_id, case when g.kind = '` + graphstore.KindAsk + `' then q.block_id
		                                   else substr(g.id, length(doc.ref_key) + 2) end) as block_id
	) place on doc.id is not null
	order by w.n`
}

// storedText is one text the block index recorded of a block: the version it stands from, the
// block's text (nil where that version deleted the block), and when that version was written.
type storedText struct {
	Version int        `json:"version"`
	Text    *string    `json:"text"`
	At      *time.Time `json:"at"`
}

// readNodes reads each node of the two arrays (nodesRead), keyed by its index among them, for who.
func readNodes(ctx context.Context, q graphstore.Queryer, who model.Actor, kinds, ids []string) (map[int]heldNode, error) {
	rows, err := q.Query(ctx, nodesRead(), kinds, ids, refs.VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the decision nodes: %w", err)
	}
	defer rows.Close()
	held := map[int]heldNode{}
	for rows.Next() {
		var n int64
		var node heldNode
		var texts []byte
		if err := rows.Scan(&n, &node.kind, &node.id, &node.supertag, &node.state,
			&node.document, &node.artifactID, &node.refKey, &node.slug, &node.artifactKind,
			&node.issueKey, &node.project, &node.session, &node.primary,
			&node.question, &node.askIssue, &node.blockID, &node.rowVersion, &texts,
			&node.indexed, &node.latest, &node.owed, &node.keptVersion); err != nil {
			return nil, fmt.Errorf("read a decision node: %w", err)
		}
		if texts != nil {
			if err := json.Unmarshal(texts, &node.texts); err != nil {
				return nil, fmt.Errorf("read the recorded texts of %s %s: %w", node.kind, node.id, err)
			}
		}
		held[int(n)-1] = node
	}
	return held, rows.Err()
}

// fact is one relation or value a node holds now: its kind, its far end's kind and id (the value
// itself, for a value), and when its tuple was written.
type fact struct {
	kind, toKind, toID string
	at                 time.Time
	seq                int64
}

// factKinds are the relations and values of a decision node the read takes beside its links.
var factKinds = []string{refs.TupleFromAsk, refs.TupleKeptBy, refs.ValueKeptTextSum, refs.ValueReplaced, refs.ValueUncertainty}

// readFacts reads, for each node, the relations and values of factKinds it holds now, in one
// statement: the newest tuple of each relation (kind and far end) and of each value (kind alone),
// answered while it asserts (graphstore.CurrentTupleRead's rule, CurrentOut's and CurrentValue's).
func readFacts(ctx context.Context, q graphstore.Queryer, kinds, ids []string) (map[int][]fact, error) {
	rows, err := q.Query(ctx, `
		select w.n, f.kind, f.to_kind, f.to_id, f.at, f.seq
		from unnest($1::text[], $2::text[]) with ordinality as w (kind, id, n)
		cross join lateral (`+graphstore.CurrentTupleRead(
		`t.kind, t.to_kind, case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_id end`,
		`t.kind, t.to_kind, t.to_id, t.at, t.seq, t.asserted`,
		`t.from_kind = w.kind and t.from_id = w.id and t.kind = any($3::text[])`)+`
		) f
		where f.asserted
		order by w.n, f.kind, f.seq
	`, kinds, ids, factKinds)
	if err != nil {
		return nil, fmt.Errorf("read the decision nodes' values: %w", err)
	}
	defer rows.Close()
	facts := map[int][]fact{}
	for rows.Next() {
		var n int64
		var held fact
		if err := rows.Scan(&n, &held.kind, &held.toKind, &held.toID, &held.at, &held.seq); err != nil {
			return nil, fmt.Errorf("read a decision node's value: %w", err)
		}
		facts[int(n)-1] = append(facts[int(n)-1], held)
	}
	return facts, rows.Err()
}

// readLinks reads, for each node, every spec node a current `shaped` tuple of it reaches that who
// may see (refs.VisibleKind), in one statement, with the two times R5 compares: the tuple's own
// event and time, and the event and time that wrote the version the target's text last changed
// in. The event of a version is docs' versionEventID's: its artifact.version, or for version 1 the
// document's artifact.created or the issue.created that seeded the issue's spec. A target whose
// document holds no row for it - one that still owes its block index - has no such version.
func readLinks(ctx context.Context, q graphstore.Queryer, who model.Actor, kinds, ids []string) (map[int][]model.DecisionLink, error) {
	rows, err := q.Query(ctx, `
		select w.n, s.to_id, s.at, s.source_event,
		       coalesce(target.block_id, ''), coalesce(target.excerpt, ''), target.changed_version, target_version.created_at,
		       (select e.id from events e
		        where (e.issue_key = doc.issue_key or e.artifact_id = doc.id)
		          and (e.type = 'artifact.version' and e.payload->>'artifact_id' = doc.id::text
		               and e.payload->'version'->>'number' = target.changed_version::text
		               or target.changed_version = 1 and e.type = 'artifact.created' and e.payload->'artifact'->>'id' = doc.id::text
		               or target.changed_version = 1 and e.type = 'issue.created' and e.issue_key = doc.issue_key and doc.is_primary)
		        order by e.id desc limit 1),
		       doc.id is not null, coalesce(doc.ref_key, ''), coalesce(doc.slug, ''), coalesce(doc.kind, ''),
		       doc.issue_key, doc.project_key, doc.session_id, coalesce(doc.is_primary, false),
		       coalesce((select r.created_version from block_refs r
		                 where r.node_kind = w.kind and r.node_address = w.id and r.kind = '`+refs.TupleShaped+`'
		                   and r.to_kind = '`+refs.BlockNodeKind+`' and r.to_id = s.to_id
		                 order by r.created_version limit 1), 0)
		from unnest($1::text[], $2::text[]) with ordinality as w (kind, id, n)
		cross join lateral (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`,
		`t.to_kind, t.to_id, t.at, t.source_event, t.asserted`,
		`t.from_kind = w.kind and t.from_id = w.id and t.kind = '`+refs.TupleShaped+`' and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) s
		left join artifacts doc on doc.ref_key = split_part(s.to_id, '#', 1)
		left join document_blocks target on target.node_kind = '`+refs.BlockNodeKind+`' and target.node_address = s.to_id
		left join artifact_versions target_version on target_version.artifact_id = target.artifact_id
			and target_version.number = target.changed_version
		where s.asserted and s.to_kind = '`+refs.BlockNodeKind+`' and `+refs.VisibleKind(refs.BlockNodeKind, "s.to_id", "$3")+`
		order by w.n, s.to_id
	`, kinds, ids, refs.VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("read the decision nodes' shaped links: %w", err)
	}
	defer rows.Close()
	links := map[int][]model.DecisionLink{}
	for rows.Next() {
		var n int64
		var address string
		var event int64
		var link model.DecisionLink
		var changedVersion *int
		var changedEvent *int64
		var document bool
		var refKey, slug, artifactKind string
		var issueKey, project, session *string
		var primary bool
		if err := rows.Scan(&n, &address, &link.CreatedAt, &event,
			&link.To.BlockID, &link.To.Text, &changedVersion, &link.TextChangedAt, &changedEvent,
			&document, &refKey, &slug, &artifactKind, &issueKey, &project, &session, &primary,
			&link.CreatedVersion); err != nil {
			return nil, fmt.Errorf("read a decision node's shaped link: %w", err)
		}
		link.To.Kind, link.To.ID = refs.BlockNodeKind, address
		if link.To.BlockID == "" && document {
			link.To.BlockID = strings.TrimPrefix(address, refKey+"#")
		}
		if document {
			link.To.Document = refs.ArtifactRef(issueKey, stringOf(project), session, slug, artifactKind, primary)
			link.To.Ref = refs.BlockRef(link.To.Document, link.To.BlockID)
		}
		if changedVersion != nil {
			link.To.ChangedVersion = *changedVersion
		}
		if event != graphstore.NoEvent {
			link.Event = &event
		}
		if changedEvent != nil && *changedEvent != graphstore.NoEvent {
			link.TextChangedEvent = changedEvent
		}
		link.Stale = textChangedAfterTie(link)
		links[int(n)-1] = append(links[int(n)-1], link)
	}
	return links, rows.Err()
}

// textChangedAfterTie is R5's rule for one link (H1): the target's text changed after the
// decision tied it. The two times are the store's: the events that wrote each where both are
// held, else the database's clock - the version's created_at against the tuple's at. A target
// with no recorded change, one whose document still owes its block index, has not changed.
func textChangedAfterTie(link model.DecisionLink) bool {
	if link.TextChangedAt == nil {
		return false
	}
	if link.Event != nil && link.TextChangedEvent != nil {
		return *link.TextChangedEvent > *link.Event
	}
	return link.TextChangedAt.After(link.CreatedAt)
}

// isDecision reports whether node is read as a decision: an ask node with an `asks` row, or a live
// block node of the `decision` supertag.
func (node heldNode) isDecision() bool {
	return node.kind == graphstore.KindAsk && node.question != nil ||
		node.kind == refs.BlockNodeKind && node.supertag == refs.DecisionSupertag && node.state == graphstore.StateLive
}

// decision is node, a decision (isDecision), in model.Decision's shape, with the values it holds
// (facts) and its links, or the error that says which stored value of it does not parse. Its
// words and the version they last changed in come from one walk (walker.changed).
func (node heldNode) decision(ctx context.Context, versions *walker, facts []fact, links []model.DecisionLink) (model.Decision, error) {
	decision := model.Decision{
		NodeKind: node.kind, NodeID: node.id,
		BlockID: node.blockID, Asks: []string{}, Links: links,
	}
	if decision.Links == nil {
		decision.Links = []model.DecisionLink{}
	}
	// A link's FromBlock is the block of the decision that holds it, as a section's link named
	// the section's block it was written in: here the decision's own block, or the ask's.
	for index := range decision.Links {
		decision.Links[index].FromBlock = node.blockID
	}
	documentRef := ""
	if node.document {
		documentRef = refs.ArtifactRef(node.issueKey, stringOf(node.project), node.session, node.slug, node.artifactKind, node.primary)
	}
	if node.kind == graphstore.KindAsk {
		decision.Kind = "person"
		decision.Text, decision.Heading = *node.question, *node.question
		decision.Asks = []string{node.id}
		switch {
		case node.askIssue != nil:
			decision.Ref = "dispatch://" + *node.askIssue + "/ask/" + node.id
		case documentRef != "":
			decision.Ref = documentRef + "/ask/" + node.id
		}
		// An ask whose block does not read back is still an ask, which settlement flags
		// `invalid`: its block's row's own version is the one it answers.
		changed, err := versions.changed(ctx, node, askType)
		switch {
		case err != nil && !errors.Is(err, errUnreadable):
			return model.Decision{}, err
		case err != nil && node.rowVersion != nil:
			decision.ChangedVersion = *node.rowVersion
		case err == nil:
			decision.ChangedVersion, decision.ChangedAt = changed.version, changed.at
		}
	} else {
		decision.Kind = "agent"
		decision.Ref = refs.BlockRef(documentRef, node.blockID)
		changed, err := versions.changed(ctx, node, refs.DecisionSupertag)
		if err != nil {
			return model.Decision{}, err
		}
		if changed.block != nil {
			decision.Text = changed.words
			content, _ := pmdoc.TypedContent(changed.block)
			for _, child := range content {
				if heading := plainText(child); heading != "" {
					decision.Heading = heading
					break
				}
			}
		}
		decision.ChangedVersion, decision.ChangedAt = changed.version, changed.at
	}
	decision.TextSHA256 = TextSum(decision.Text)
	var kept *fact
	keptSum, keptSumHeld := "", false
	for index, held := range facts {
		switch held.kind {
		case refs.TupleFromAsk:
			if held.toKind == graphstore.KindAsk {
				decision.Asks = append(decision.Asks, held.toID)
			}
		case refs.TupleKeptBy:
			if kept == nil || held.seq > kept.seq {
				kept = &facts[index]
			}
		case refs.ValueKeptTextSum:
			keptSum, keptSumHeld = held.toID, true
		case refs.ValueReplaced:
			switch held.toID {
			case "true":
				decision.Replaced = true
			case "false":
			default:
				return model.Decision{}, fmt.Errorf("replaced holds %q, which is no bool", held.toID)
			}
		case refs.ValueUncertainty:
			switch held.toID {
			case "low", "medium":
			case "high":
				decision.Unsure = true
			default:
				return model.Decision{}, fmt.Errorf("uncertainty holds %q, which is none of low, medium and high", held.toID)
			}
		}
	}
	if decision.Kind == "agent" && kept != nil && keptSumHeld {
		decision.Kept = &model.DecisionKeep{
			ArtifactID: node.artifactID, BlockID: node.blockID, KeptBy: kept.toID, KeptAt: kept.at, TextSHA256: keptSum,
		}
		if node.keptVersion != nil {
			decision.Kept.ChangedVersion = *node.keptVersion
		}
	}
	decision.WaitingReview = decision.Kind == "agent" && !decision.Replaced &&
		(decision.Kept == nil || decision.Kept.TextSHA256 != decision.TextSHA256)
	return decision, nil
}

// askType is the block type of an ask's block, whose own words - its question and its options,
// and none of the readings it closes with - say which version last changed the ask.
const askType = "ask"

// errUnreadable is the error of a block whose text as it stands does not read back as its type.
var errUnreadable = errors.New("the text does not read back")

// walker reads the versions of the documents one call of Decisions walks, each version read and
// parsed once however many of its blocks the call reads, through the call's own Queryer, so a
// caller holding one transaction reads one snapshot. err is the first failure of the store,
// which ends the call.
type walker struct {
	q        graphstore.Queryer
	versions map[versionKey]versionRead
	err      error
}

// versionKey names one version of one document.
type versionKey struct {
	artifactID string
	number     int
}

// versionRead is one version as the walk reads it: its tree, nil where the document holds no
// such version, holds no markdown at it or where that markdown does not read back (unreadable),
// and when the version was written.
type versionRead struct {
	tree       *pmdoc.Node
	at         *time.Time
	unreadable bool
}

// version is version number of document artifactID, read once.
func (w *walker) version(ctx context.Context, artifactID string, number int) versionRead {
	key := versionKey{artifactID: artifactID, number: number}
	if read, held := w.versions[key]; held || w.err != nil {
		return read
	}
	var markdown *string
	var at time.Time
	read := versionRead{}
	err := w.q.QueryRow(ctx, `select markdown, created_at from artifact_versions where artifact_id = $1::uuid and number = $2`,
		artifactID, number).Scan(&markdown, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		w.err = fmt.Errorf("read version %d of %s: %w", number, artifactID, err)
		return read
	default:
		read.at = &at
		if markdown != nil {
			if tree, err := pmdoc.ParseRendering(*markdown); err == nil {
				read.tree = tree
			} else {
				read.unreadable = true
			}
		}
	}
	w.versions[key] = read
	return read
}

// changedWords is what the walk answers of a block: its own words as it stands, the block they
// were read off (nil where the document holds no such block now), and the version those words
// were first held in with that version's time.
type changedWords struct {
	words   string
	block   *pmdoc.Node
	version int
	at      *time.Time
}

// changed is the walk that answers node's block - a decision, or the ask block of an ask (typ) -
// as it stands: its own words (ownWords) and the version of its document that last changed them,
// which a reading written, edited or removed under it, and for an ask a write of its block's
// attributes alone - the server's, or the `shaped` pointers - never moves.
//
// It reads the document as the block index left it and as it stands past that index. A document
// that owes its block index (document_block_versions holds no version) stands at its newest
// version, whose markdown gives the words, and each version the index has not recorded
// (document_indexed_versions) is read off its own markdown, newest first, as the index itself
// reads the versions it did not record (refs' textSince). A version the index recorded holds the
// block's text at its latest document_block_texts row at or before it. A row stands from its
// version on, so the walk steps from row to row, and reads the unrecorded versions just below
// each row's version off their markdown, which a pass that indexed several versions at once
// covered without recording: their block can hold the row's words with another text around them.
// The walk goes back while the words read the same, and answers the earliest version it reached.
//
// A block whose text as it stands does not read back is errUnreadable; an earlier version that
// does not, or that holds no such block, ends the walk there.
func (w *walker) changed(ctx context.Context, node heldNode, typ string) (changedWords, error) {
	var answer changedWords
	if !node.document || node.latest == 0 {
		return answer, nil
	}
	// The versions the block index recorded come newest first.
	lastRecorded := 0
	if len(node.indexed) > 0 {
		lastRecorded = int(node.indexed[0])
	}
	// rowAt is the index of the latest row at or before version n, or -1.
	rowAt := func(n int) int {
		for index, text := range node.texts {
			if text.Version <= n {
				return index
			}
		}
		return -1
	}
	// fromMarkdown is the block's words at version n read off n's markdown, and whether n holds
	// the block.
	fromMarkdown := func(n int) (string, *pmdoc.Node, bool, *time.Time, bool) {
		read := w.version(ctx, node.artifactID, n)
		if read.tree == nil {
			return "", nil, false, read.at, read.unreadable
		}
		words, block, held := blockWords(read.tree, typ, node.blockID)
		return words, block, held, read.at, false
	}
	// fromRow is the block's words in the text row records, and whether it holds the block.
	fromRow := func(row int) (string, *pmdoc.Node, bool, bool) {
		text := node.texts[row].Text
		if text == nil {
			return "", nil, false, false
		}
		tree, err := pmdoc.ParseRendering(*text)
		if err != nil {
			return "", nil, false, true
		}
		words, block, held := blockWords(tree, typ, node.blockID)
		return words, block, held, false
	}
	// back walks the unrecorded versions below version, down to below, off their markdown, and
	// reports whether the words still read the same at the last of them.
	back := func(version, below int) bool {
		for n := version - 1; n > below; n-- {
			words, _, held, at, _ := fromMarkdown(n)
			if w.err != nil || !held || words != answer.words {
				return false
			}
			answer.version, answer.at = n, at
		}
		return true
	}
	row := -1
	if node.owed {
		words, block, held, at, unreadable := fromMarkdown(node.latest)
		if unreadable {
			return answer, fmt.Errorf("version %d of %s %q: %w", node.latest, typ, node.blockID, errUnreadable)
		}
		if w.err != nil || !held {
			return answer, w.err
		}
		answer = changedWords{words: words, block: block, version: node.latest, at: at}
		if !back(node.latest, lastRecorded) || lastRecorded == 0 {
			return answer, w.err
		}
		if row = rowAt(lastRecorded); row < 0 {
			return answer, nil
		}
		if words, _, held, _ := fromRow(row); !held || words != answer.words {
			return answer, nil
		}
		answer.version, answer.at = node.texts[row].Version, node.texts[row].At
	} else {
		if len(node.texts) == 0 {
			return answer, nil
		}
		row = 0
		words, block, held, unreadable := fromRow(row)
		if unreadable {
			return answer, fmt.Errorf("the recorded text of %s %q: %w", typ, node.blockID, errUnreadable)
		}
		if !held {
			return answer, nil
		}
		answer = changedWords{words: words, block: block, version: node.texts[row].Version, at: node.texts[row].At}
	}
	for {
		version := node.texts[row].Version
		below := 0
		for _, recordedVersion := range node.indexed {
			if int(recordedVersion) < version {
				below = int(recordedVersion)
				break
			}
		}
		if !back(version, below) || below == 0 {
			return answer, w.err
		}
		next := rowAt(below)
		if next < 0 {
			return answer, nil
		}
		if words, _, held, _ := fromRow(next); !held || words != answer.words {
			return answer, nil
		}
		answer.version, answer.at = node.texts[next].Version, node.texts[next].At
		row = next
	}
}

// blockWords is the own words of the block of type typ and id blockID that tree holds (ownWords),
// that block, and whether tree holds it and its words render.
func blockWords(tree *pmdoc.Node, typ, blockID string) (string, *pmdoc.Node, bool) {
	var found *pmdoc.Node
	pmdoc.Walk(tree, func(block *pmdoc.Node) bool {
		if block.Type == typ && refs.TypedBlockID(block) == blockID {
			found = block
		}
		return found == nil
	})
	if found == nil {
		return "", nil, false
	}
	words, err := ownWords(found)
	if err != nil {
		return "", nil, false
	}
	return words, found, true
}

// ownWords is a typed block's own words. An ask's are its question and its options (plan v17,
// T8): the blocks of its content rendered alone, with no attribute of the block at all - neither
// the server's state, answered_by, answered_at, selected, answer, copied_from and
// copied_from_document, which the roll window and the first write on one build write or drop, nor
// the `shaped` pointers the conversion gives it, nor its urgency - and none of the readings it
// closes with. So a version that changes none of those words changes no ask's words. A decision's
// are the block rendered alone with the readings it closes with left out (refs.DecisionWords); its
// markdown carries none of its server attributes (decided_by, kept_by and kept_text_sum are
// `markdown: false`), so they are the words a Keep sums.
func ownWords(block *pmdoc.Node) (string, error) {
	if block.Type == askType {
		content, _ := pmdoc.TypedContent(block)
		return pmdoc.Render(&pmdoc.Node{Type: "doc", Children: content})
	}
	attributes, _ := pmdoc.BlockTypeAttributes(block.Type)
	own := *block
	own.Attrs = make(pmdoc.Attrs, len(block.Attrs))
	for name, value := range block.Attrs {
		if attribute := attributes.Get(name); attribute.Server && attribute.InMarkdown() {
			continue
		}
		own.Attrs[name] = value
	}
	return refs.DecisionWords(&own)
}

// plainText is the text node holds, its children's included, with its lines joined by a space.
func plainText(node *pmdoc.Node) string {
	var words []string
	pmdoc.Walk(node, func(child *pmdoc.Node) bool {
		if child.Text != "" {
			words = append(words, child.Text)
		}
		return true
	})
	return strings.Join(strings.Fields(strings.Join(words, " ")), " ")
}

// stringOf is *value, or "" for nil.
func stringOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
