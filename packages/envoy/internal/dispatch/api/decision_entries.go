package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// decisionEntry is one entry of the Decisions read's `entries`: the
// node itself. node is the node that holds it - an ask's own node (kind ask), or a `decision`
// block's node (kind block) - and ref that node's dispatch:// address, which is where
// the entry links; document is the document whose block shows it, null for an ask on the issue
// itself. kind is the node's shape, and never says who decided: `person` for an ask, which a
// person answers, `agent` for a `decision` block, whoever decided it, and `declined` for an ask
// the person declined to answer, which records no decision. Who decided is decided_by below.
// state is the node state of its node, which is any state but gone (decisionEntriesRead), so a
// proposed, settled or overtaken entry is told from a live one (the spec's R5).
//
// An ask carries its wording from its row (question, options, urgency, multiple) and its answer
// from its node (answer_state, answer, selected, answered_by, answered_at, resolution), each null
// on a decision. A declined ask (resolution.kind = "declined") is marked declined, carries the
// person's note as declined_note, and carries no answer and no decided_by. A decision carries
// uncertainty, uncertainty_reason, kept_by and kept_text_sum from its node, each null on an ask
// and where the node holds none. decided_by is the far end of the node's decided_by tuple: the
// person who answered, or the person or session whose credential wrote the decision. A session
// decider carries title, the newest title that session stamped on anything it wrote
// (decisionDeciderTitles), which outlives Envoy's registry; a person, and a session that never
// stamped one, carry none.
//
// text, text_sha256, replaced, kept, waiting_review, changed_version, changed_at and shaped are
// decisionread.Decisions' for the node, so they read as the Keep and every other read of a
// decision reads them: an ask's text is its question, and a decision's text its own words.
// shaped is one link for each spec node the node's current `shaped` tuples reach, each with the
// tuple's own event, the event that wrote the version the node's text last changed in
// (text_changed_event), stale, true when the decision's own words differ from those at the version
// the tuple was written in and from those at the version that stood when the node's text last
// changed, and changed_since_tie, true when the node's text changed after the tuple
// (model.DecisionLink). readings are the `reading` nodes, at any state but gone, whose part_of
// reaches this node, each with its node state and the far end of its authored_by tuple, null where
// it holds none.
type decisionEntry struct {
	Node     decisionEntryNode      `json:"node"`
	Ref      string                 `json:"ref"`
	Document *decisionEntryDocument `json:"document"`
	Kind     string                 `json:"kind"`
	// State is the node state of Node. Every entry of this build carries it, and a server of the
	// release before answers the read without it during a roll, so the document marks it optional
	// (`required:"false"`) and a reader takes an entry that carries none as live.
	State string `json:"state" enum:"live,gone,proposed,settled,overtaken" required:"false"`

	Text string `json:"text"`
	// TextTree is Text read as the tree a reader is shown, present only for a caller that sent
	// `Dispatch-Trees: 1`.
	TextTree   model.DisplayTree `json:"text_tree,omitempty"`
	TextSHA256 string            `json:"text_sha256"`

	Question    *string           `json:"question"`
	Options     []model.AskOption `json:"options"`
	Urgency     *string           `json:"urgency"`
	Multiple    *bool             `json:"multiple"`
	AnswerState *string           `json:"answer_state"`
	Answer      *string           `json:"answer"`
	// AnswerTree is Answer read as the tree a reader is shown, present only for a caller that
	// sent `Dispatch-Trees: 1` and only where the ask holds an answer's text: the Decisions tab
	// draws a person's decision by that text.
	AnswerTree   model.DisplayTree    `json:"answer_tree,omitempty"`
	Selected     []string             `json:"selected"`
	AnsweredBy   *string              `json:"answered_by"`
	AnsweredAt   *time.Time           `json:"answered_at"`
	Resolution   *model.AskResolution `json:"resolution"`
	Declined     bool                 `json:"declined"`
	DeclinedNote *string              `json:"declined_note"`

	Uncertainty       *string          `json:"uncertainty"`
	UncertaintyReason *string          `json:"uncertainty_reason"`
	DecidedBy         *decisionDecider `json:"decided_by"`
	KeptBy            *string          `json:"kept_by"`
	KeptTextSum       *string          `json:"kept_text_sum"`

	Replaced       bool                 `json:"replaced"`
	Kept           *model.DecisionKeep  `json:"kept"`
	WaitingReview  bool                 `json:"waiting_review"`
	ChangedVersion int                  `json:"changed_version"`
	ChangedAt      *time.Time           `json:"changed_at"`
	Shaped         []model.DecisionLink `json:"shaped"`
	// FromAsk is, for a decision node, the id of each ask its from_ask tuples reach (the questions
	// it settles, decisionread's Decision.Asks), and [] for an ask's own entry.
	FromAsk  []string          `json:"from_ask"`
	Readings []decisionReading `json:"readings"`

	// openedAt is the ask row's created_at, which orders the open asks among themselves.
	openedAt time.Time
	// foundIn is the document the read found a decision's node in (decisionEntriesRead), which
	// decisionread takes for a block the room's index names no document of yet; "" for an ask.
	foundIn string
}

// decisionEntryNode names a node of the store: its kind and its id as graph_nodes keys it.
type decisionEntryNode struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// decisionDecider is an entry's decided_by: the node its decided_by tuple reaches, named as
// decisionEntryNode names a node, and for a session the title it stamped, empty and left out of
// the answer for a person and for a session that never stamped one.
type decisionDecider struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// decisionEntryDocument is the document whose block shows an entry: its id and dispatch://
// address, and that block's id and node address (`<ref_key>#<block id>`, under the ref_key of the
// artifact keying the block's rows: the document's own, or for a document converted into its
// issue's room the room's tree's). A converted document's entry names that document, never the
// tree, as an entry of a document that is its own room does.
type decisionEntryDocument struct {
	ArtifactID string `json:"artifact_id"`
	Ref        string `json:"ref"`
	BlockID    string `json:"block_id"`
	Block      string `json:"block"`
}

// decisionReading is one reading of an entry: its block node and that node's state, the document
// that holds it, its text as the block index recorded it last (null where its document still owes
// that index), and the person or session its authored_by names, null where the node holds no such
// tuple - a reading the read neither leaves out nor gives an author.
type decisionReading struct {
	Node decisionEntryNode `json:"node"`
	// State is the node state of Node, optional in the document as decisionEntry's State is.
	State      string             `json:"state" enum:"live,gone,proposed,settled,overtaken" required:"false"`
	ArtifactID *string            `json:"artifact_id"`
	BlockID    string             `json:"block_id"`
	Text       *string            `json:"text"`
	AuthoredBy *decisionEntryNode `json:"authored_by"`
}

// entryFactKinds are the values and relations of an entry's node the entries read takes itself;
// the rest are decisionread's, kept_by and kept_text_sum among them (decisionEntries).
var entryFactKinds = []string{
	graphstore.TupleAnswerState, graphstore.TupleAnswer, graphstore.TupleSelected, graphstore.TupleAnsweredAt,
	graphstore.TupleResolution, graphstore.TupleAnsweredBy, graphstore.TupleDecidedBy,
	refs.ValueUncertainty, refs.ValueUncertaintyReason,
}

// decisionEntriesRead shapes the Decisions query's entries for the previous release's route.
// $4 and $5 are the node kinds and ids the query takes. The document lookup supplies a fragment
// hint for decisions the block index has not reached; it does not select the entries.
func decisionEntriesRead() string {
	return `
		with documents as (
			select a.id, a.ref_key, ` + refs.DocumentRoomSQL("a.id") + ` as room,
			       not exists (select 1 from document_block_versions v where v.artifact_id = a.id and v.version is not null) as owed
			from artifacts a where a.issue_key = $1 and a.kind = 'doc'
		),
		decision_nodes as (
			select held.id, doc.id as document from documents doc
			cross join lateral (
				select d.node_address as id from document_blocks d
				where d.artifact_id = doc.room and d.type = '` + refs.DecisionSupertag + `' and d.node_kind = '` + refs.BlockNodeKind + `'
				  and ` + refs.BlockDocumentSQL("d") + ` = doc.id
				union
				select part.from_id from (` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = 'artifact' and t.to_id = doc.ref_key and t.kind = '`+graphstore.TuplePartOf+`'`) + `) part
				where doc.owed and part.asserted and part.from_kind = '` + refs.BlockNodeKind + `'
				union
				select r.node_address from block_refs r
				where doc.owed and r.artifact_id = doc.room and r.node_kind = '` + refs.BlockNodeKind + `'
				  and ` + refs.BlockDocumentSQL("r") + ` = doc.id
				offset 0
			) held
		),
		members as (
			select node.kind, node.id, node.state, source.document
			from unnest($4::text[], $5::text[]) member(kind, id)
			cross join lateral (
				select g.kind, g.id, g.state from graph_nodes g
				where g.kind = member.kind and g.id = member.id and g.state <> '` + graphstore.StateGone + `'
				  and ` + refs.VisibleRow("g", "$2") + `
				offset 0
			) node
			left join lateral (
				select d.document from decision_nodes d where d.id = member.id and member.kind = '` + refs.BlockNodeKind + `'
				order by d.document limit 1
			) source on true
		)
		select m.kind, m.id, m.state, q.question, q.options, q.urgency, q.multiple, q.created_at, coalesce(place.block_id, ''),
		       doc.id is not null, coalesce(doc.id::text, ''), coalesce(keyed.ref_key, ''), coalesce(doc.slug, ''),
		       coalesce(doc.kind, ''), doc.issue_key, coalesce(doc.project_key, ''), doc.session_id, coalesce(doc.is_primary, false),
		       doc.id is not null and (
		         exists (select 1 from document_blocks b
		                 where m.kind = '` + graphstore.KindAsk + `' and b.artifact_id = place.artifact_id and b.block_id = place.block_id and b.type = 'ask' offset 0)
		         or exists (select 1 from document_blocks b
		                    where m.kind = '` + refs.BlockNodeKind + `' and b.node_kind = m.kind and b.node_address = m.id offset 0)
		         or not exists (select 1 from document_block_versions v
		                        where v.artifact_id = doc.id and v.version is not null offset 0)),
		       (select coalesce(json_agg(json_build_object('kind', f.kind, 'to_kind', f.to_kind, 'to_id', f.to_id)), '[]'::json)
		        from (` + graphstore.CurrentTupleRead(
		`t.kind, t.to_kind, case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_id end`,
		`t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = m.kind and t.from_id = m.id and t.kind = any($3::text[])`) + `) f
		        where f.asserted),
		       (select coalesce(json_agg(json_build_object(
		                 'id', r.id, 'state', r.state, 'artifact_id', ` + refs.BlockDocumentSQL("rd") + `, 'block_id', rd.block_id,
		                 'text', (select x.text from document_block_texts x
		                          where x.artifact_id = rd.artifact_id and x.block_id = rd.block_id
		                          order by x.version desc limit 1),
		                 'author_kind', author.to_kind, 'author_id', author.to_id) order by r.id), '[]'::json)
		        from (` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = m.kind and t.to_id = m.id and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `) part
		        cross join lateral (
		        	select r.kind, r.id, r.state from graph_nodes r
		        	where r.kind = part.from_kind and r.id = part.from_id
		        	  and r.supertag = '` + refs.ReadingSupertag + `' and r.state <> '` + graphstore.StateGone + `'
		        	  and ` + refs.VisibleRow("r", "$2") + `
		        	offset 0
		        ) r
		        left join document_blocks rd on rd.node_kind = r.kind and rd.node_address = r.id
		        left join lateral (
		        	select held.to_kind, held.to_id from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`,
		`t.to_kind, t.to_id, t.seq, t.asserted`,
		`t.from_kind = r.kind and t.from_id = r.id and t.kind = '`+graphstore.TupleAuthoredBy+`'`) + `) held
		        	where held.asserted order by held.seq desc limit 1
		        ) author on true
		        where part.asserted),
		       coalesce(m.document::text, '')
		from members m
		left join asks q on m.kind = '` + graphstore.KindAsk + `' and q.id = case when m.kind = '` + graphstore.KindAsk + `' then m.id::uuid end
		left join lateral (
			select a.id, a.ref_key from artifacts a
			where m.kind = '` + graphstore.KindAsk + `' and a.id = q.block_artifact_id
			union all
			select a.id, a.ref_key from artifacts a
			where m.kind = '` + refs.BlockNodeKind + `' and a.ref_key = split_part(m.id, '#', 1)
		) keyed on true
		left join lateral (
			select keyed.id as artifact_id,
			       case when m.kind = '` + graphstore.KindAsk + `' then q.block_id else substr(m.id, length(keyed.ref_key) + 2) end as block_id
		) place on keyed.id is not null
		left join artifacts doc on doc.id = ` + refs.BlockDocumentOrSQL("place", "coalesce(m.document, place.artifact_id)") + `
		order by m.kind, m.id`
}

// decisionEntryTargets takes the primary entries from the same query definition as the tree
// view. The compatibility route nests readings under their decision or ask.
func decisionEntryTargets(ctx context.Context, q queryer, key string) (kinds, ids []string, err error) {
	if err := refs.TurnJITOff(ctx, q); err != nil {
		return nil, nil, err
	}
	tree, err := docs.WalkIssueTree(ctx, q, key, refs.TreeCut{})
	if err != nil {
		return nil, nil, err
	}
	taken, _ := tree.Takes(graphstore.QueryDecisions)
	for _, index := range taken {
		node := tree.Nodes[index]
		if node.Kind == graphstore.KindAsk || node.Type == refs.DecisionSupertag {
			kinds = append(kinds, node.Kind)
			ids = append(ids, node.ID)
		}
	}
	return kinds, ids, nil
}

// decisionEntries reads the Decisions query's nodes (decisionEntryTargets) and shapes them for
// the caller (decisionEntriesRead and decisionread.Decisions). The walk and those reads share one
// read-only, repeatable-read transaction, so a Keep or answer committed during the read is in
// all of them or in none. The route still nests each reading under its decision or ask.
//
// An entry names the document that holds its node's block where a document_blocks row holds that
// block - for an ask, a row of type ask at the ask's block id - or where the document owes its block
// index and no row can say so yet: an answered ask whose block left its document keeps its row,
// which still names that document, and names none, and so does one whose block id a block of
// another type took, since that block is not the ask's.
// kept_by and kept_text_sum are the Keep decisionread read (model.Decision's Kept), so they and
// `kept` are one read of the node.
//
// Open asks come first, oldest first, then every other entry by its time: an answer's, else a
// resolution's, else the version the decision's words last changed in, oldest first. An entry
// whose stored value does not parse - an answer state or an uncertainty outside its type, a
// selection or a resolution that is no JSON of its shape, a time that is none, or a value
// decisionread refuses - is left out and logged, and the read goes on. reader is the answer's
// own, and each entry registers its text with it once the slice has stopped growing.
//
// It answers too the Decisions read's old `decisions` member: decisionread's model.Decision of
// each entry that is a decision node or an answered ask, in the order of the entries, a `decision`
// block a person decided and nobody kept answered kind `person` (compatDecision), so a page of the
// release before reads what the store holds.
func (s *server) decisionEntries(ctx context.Context, who model.Actor, key string, reader *displayTreeReader) ([]decisionEntry, []model.Decision, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("open the read of the decisions of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	kinds, ids, err := decisionEntryTargets(ctx, tx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("walk the decisions query of %s: %w", key, err)
	}
	rows, err := tx.Query(ctx, decisionEntriesRead(), key, refs.VisibilityArg(who), entryFactKinds, kinds, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read the decisions of %s: %w", key, err)
	}
	entries := []decisionEntry{}
	for rows.Next() {
		var entry decisionEntry
		var options []byte
		var question, urgency *string
		var multiple *bool
		var opened *time.Time
		var blockID string
		var document, holds bool
		var artifactID, nodeKey, slug, artifactKind string
		var project string
		var issueKey, session *string
		var primary bool
		var facts, readings []byte
		if err := rows.Scan(&entry.Node.Kind, &entry.Node.ID, &entry.State, &question, &options, &urgency, &multiple, &opened, &blockID,
			&document, &artifactID, &nodeKey, &slug, &artifactKind, &issueKey, &project, &session, &primary, &holds,
			&facts, &readings, &entry.foundIn); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read a decision of %s: %w", key, err)
		}
		if document && holds && blockID != "" {
			entry.Document = &decisionEntryDocument{
				ArtifactID: artifactID,
				Ref:        refs.ArtifactRef(issueKey, project, session, slug, artifactKind, primary),
				BlockID:    blockID,
				Block:      refs.BlockAddress(nodeKey, blockID),
			}
		}
		if entry.Node.Kind == graphstore.KindAsk {
			entry.Kind, entry.Question, entry.Urgency, entry.Multiple = "person", question, urgency, multiple
			if opened != nil {
				entry.openedAt = *opened
			}
			entry.Options = []model.AskOption{}
			if options != nil {
				if err := json.Unmarshal(options, &entry.Options); err != nil {
					slog.Warn("dispatch: a decision whose stored value does not parse is left out of the read",
						"issue", key, "node", entry.Node.ID, "err", fmt.Errorf("the ask's options: %w", err))
					continue
				}
			}
		} else {
			entry.Kind = "agent"
		}
		if err := entry.readFacts(facts); err != nil {
			slog.Warn("dispatch: a decision whose stored value does not parse is left out of the read",
				"issue", key, "kind", entry.Node.Kind, "node", entry.Node.ID, "err", err)
			continue
		}
		if entry.Node.Kind == graphstore.KindAsk && entry.AnswerState == nil {
			// An ask's node states its answer state from its first settlement on (asks.Settle):
			// one that holds none has never been answered or closed, and is open.
			open := "open"
			entry.AnswerState = &open
		}
		if err := entry.readReadings(readings); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read the readings of %s %s: %w", entry.Node.Kind, entry.Node.ID, err)
		}
		entries = append(entries, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the decisions of %s: %w", key, err)
	}
	nodes := make([]decisionread.Node, len(entries))
	for index, entry := range entries {
		nodes[index] = decisionread.Node{Kind: entry.Node.Kind, ID: entry.Node.ID, Document: entry.foundIn}
	}
	decided, err := decisionread.Decisions(ctx, tx, who, nodes)
	if err != nil {
		return nil, nil, err
	}
	if err := decisionDeciderTitles(ctx, tx, entries); err != nil {
		return nil, nil, fmt.Errorf("name the deciders of the decisions of %s: %w", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("close the read of the decisions of %s: %w", key, err)
	}
	byNode := make(map[decisionEntryNode]model.Decision, len(decided))
	for _, decision := range decided {
		byNode[decisionEntryNode{Kind: decision.NodeKind, ID: decision.NodeID}] = decision
	}
	read := entries[:0]
	for _, entry := range entries {
		// A node decisionread left out is one whose stored value it refused, and logged.
		decision, found := byNode[entry.Node]
		if !found {
			continue
		}
		entry.Ref, entry.Text, entry.TextSHA256 = decision.Ref, decision.Text, decision.TextSHA256
		entry.Replaced, entry.Kept, entry.WaitingReview = decision.Replaced, decision.Kept, decision.WaitingReview
		if decision.Kept != nil {
			keptBy, keptSum := decision.Kept.KeptBy, decision.Kept.TextSHA256
			entry.KeptBy, entry.KeptTextSum = &keptBy, &keptSum
		}
		entry.ChangedVersion, entry.ChangedAt, entry.Shaped = decision.ChangedVersion, decision.ChangedAt, decision.Links
		entry.FromAsk = []string{}
		if entry.Node.Kind != graphstore.KindAsk {
			entry.FromAsk = append(entry.FromAsk, decision.Asks...)
		}
		read = append(read, entry)
	}
	entries = read
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].before(entries[j]) })
	// kept for one release: compat-objects
	decisions := []model.Decision{}
	// kept for one release: compat-objects
	for _, entry := range entries {
		if entry.Kind == "agent" || (entry.Kind == "person" && entry.AnswerState != nil && *entry.AnswerState == "answered") {
			decisions = append(decisions, compatDecision(entry, byNode[entry.Node]))
		}
	}
	for index := range entries {
		reader.text(&entries[index].TextTree, entries[index].Text)
		reader.textOf(&entries[index].AnswerTree, entries[index].Answer)
	}
	// kept for one release: compat-objects
	for index := range decisions {
		reader.text(&decisions[index].TextTree, decisions[index].Text)
	}
	return entries, decisions, nil
}

// compatDecision is decision, entry's node as decisionread answers it, as the old `decisions`
// member answers it to a page of the release before. That page lists a decision under "Waiting
// for your review" (waiting_review), under "Your decisions" (kind `person`, or a Keep) or under
// "Replaced", and draws a decision of kind `person` as a person's. A `decision` block whose
// decided_by names a person, which nobody kept, is kind `agent` with no Keep and waits for nobody
// (decisionread), so that page would list it nowhere: this member answers it kind `person`. The
// entry keeps kind `agent`, the node's shape, with its decided_by beside it.
//
// kept for one release: compat-objects
func compatDecision(entry decisionEntry, decision model.Decision) model.Decision {
	if entry.Node.Kind == refs.BlockNodeKind && entry.DecidedBy != nil && entry.DecidedBy.Kind == graphstore.KindPerson && decision.Kept == nil {
		decision.Kind = "person"
	}
	return decision
}

// before reports whether entry comes before other in the Decisions read: an open ask before every
// other entry, open asks oldest first, and every other entry by its time, oldest first, a node's
// id breaking a tie.
func (entry decisionEntry) before(other decisionEntry) bool {
	open, otherOpen := entry.isOpen(), other.isOpen()
	if open != otherOpen {
		return open
	}
	at, otherAt := entry.openedAt, other.openedAt
	if !open {
		at, otherAt = entry.at(), other.at()
	}
	if !at.Equal(otherAt) {
		return at.Before(otherAt)
	}
	if entry.Node.Kind != other.Node.Kind {
		return entry.Node.Kind < other.Node.Kind
	}
	return entry.Node.ID < other.Node.ID
}

// isOpen reports whether entry is an open ask, one no person has answered or closed.
func (entry decisionEntry) isOpen() bool {
	return entry.AnswerState != nil && *entry.AnswerState == "open"
}

// at is entry's time: when its answer was given, else when it was resolved, else when the version
// its words last changed in was written, else when the ask was opened.
func (entry decisionEntry) at() time.Time {
	switch {
	case entry.AnsweredAt != nil:
		return *entry.AnsweredAt
	case entry.Resolution != nil:
		return entry.Resolution.At
	case entry.ChangedAt != nil:
		return *entry.ChangedAt
	}
	return entry.openedAt
}

// readFacts sets the values and relations of entry's node the entries read holds (entryFactKinds),
// from the JSON list the statement aggregated, or answers which of them does not parse: the
// answer state, as every ask read reads it (asks.NodeAnswer), and the decision's own. A declined ask
// carries its note and no answer, and an ask carries no Keep.
func (entry *decisionEntry) readFacts(encoded []byte) error {
	var facts []asks.NodeFact
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return fmt.Errorf("the node's values: %w", err)
	}
	var answer asks.NodeAnswer
	for _, fact := range facts {
		value := fact.ToID
		if asks.IsAnswerKind(fact.Kind) {
			if err := answer.Read(fact.Kind, value); err != nil {
				return err
			}
			continue
		}
		switch fact.Kind {
		case graphstore.TupleDecidedBy:
			entry.DecidedBy = &decisionDecider{Kind: fact.ToKind, ID: value}
		case refs.ValueUncertainty:
			switch value {
			case "low", "medium", "high":
				entry.Uncertainty = &value
			default:
				return fmt.Errorf("uncertainty holds %q, which is none of low, medium and high", value)
			}
		case refs.ValueUncertaintyReason:
			entry.UncertaintyReason = &value
		}
	}
	// The node's answer holds "" for an answer state and an answerer it states none of; the entry
	// answers null for each.
	entry.AnswerState, entry.AnsweredBy = nil, nil
	if answer.State != "" {
		entry.AnswerState = &answer.State
	}
	if answer.AnsweredBy != "" {
		entry.AnsweredBy = &answer.AnsweredBy
	}
	entry.Answer, entry.Selected = answer.Answer, answer.Selected
	entry.AnsweredAt, entry.Resolution = answer.AnsweredAt, answer.Resolution
	if entry.Resolution != nil && entry.Resolution.Kind == model.AskResolutionDeclined {
		// D9: a declined ask is an entry of its own, with the person's note, and never an answer.
		entry.Declined, entry.Kind = true, "declined"
		if note := entry.Resolution.Reason; note != "" {
			entry.DeclinedNote = &note
		}
		entry.Answer, entry.Selected, entry.AnsweredBy, entry.AnsweredAt, entry.DecidedBy = nil, nil, nil, nil, nil
	}
	return nil
}

// decisionDeciderTitles gives each session decider of entries the newest title that session
// stamped on anything it wrote (delivery.ResolveSessionTitles), in one read in tx for every
// session the entries name, as the issue's collaborators and a person's agent threads name a
// session the registry no longer lists. A person decider, and a session that stamped no title,
// are left with none.
func decisionDeciderTitles(ctx context.Context, tx pgx.Tx, entries []decisionEntry) error {
	sessions := []string{}
	for _, entry := range entries {
		if entry.DecidedBy != nil && entry.DecidedBy.Kind == graphstore.KindSession {
			sessions = append(sessions, entry.DecidedBy.ID)
		}
	}
	slices.Sort(sessions)
	titles, err := delivery.ResolveSessionTitles(ctx, tx, slices.Compact(sessions))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.DecidedBy != nil && entry.DecidedBy.Kind == graphstore.KindSession {
			entry.DecidedBy.Title = titles[entry.DecidedBy.ID]
		}
	}
	return nil
}

// readReadings sets entry's readings from the JSON list the statement aggregated.
func (entry *decisionEntry) readReadings(encoded []byte) error {
	var readings []struct {
		ID         string  `json:"id"`
		State      string  `json:"state"`
		ArtifactID *string `json:"artifact_id"`
		BlockID    *string `json:"block_id"`
		Text       *string `json:"text"`
		AuthorKind *string `json:"author_kind"`
		AuthorID   *string `json:"author_id"`
	}
	if err := json.Unmarshal(encoded, &readings); err != nil {
		return err
	}
	entry.Readings = make([]decisionReading, 0, len(readings))
	for _, held := range readings {
		reading := decisionReading{
			Node:       decisionEntryNode{Kind: refs.BlockNodeKind, ID: held.ID},
			State:      held.State,
			ArtifactID: held.ArtifactID, Text: held.Text,
		}
		if held.BlockID != nil {
			reading.BlockID = *held.BlockID
		} else if _, blockID, found := strings.Cut(held.ID, "#"); found {
			reading.BlockID = blockID
		}
		if held.AuthorKind != nil && held.AuthorID != nil {
			reading.AuthoredBy = &decisionEntryNode{Kind: *held.AuthorKind, ID: *held.AuthorID}
		}
		entry.Readings = append(entry.Readings, reading)
	}
	return nil
}
