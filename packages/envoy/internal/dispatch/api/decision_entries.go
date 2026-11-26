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
// (model.DecisionLink). readings are the live `reading` nodes whose
// part_of reaches this node, each with the far end of its authored_by tuple, null where it holds
// none.
type decisionEntry struct {
	Node     decisionEntryNode      `json:"node"`
	Ref      string                 `json:"ref"`
	Document *decisionEntryDocument `json:"document"`
	Kind     string                 `json:"kind"`

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
	AnswerTree model.DisplayTree `json:"answer_tree,omitempty"`
	Selected   []string          `json:"selected"`
	// SelectedTrees is the tree of each label of Selected at that label's own index, present only
	// for a caller that sent `Dispatch-Trees: 1` (chosenLabelTrees), matching every read of an
	// answer (registerChosenLabels) so callers can draw chosen labels without parsing.
	SelectedTrees []model.DisplayTree  `json:"selected_trees,omitempty"`
	AnsweredBy    *string              `json:"answered_by"`
	AnsweredAt    *time.Time           `json:"answered_at"`
	Resolution    *model.AskResolution `json:"resolution"`
	Declined      bool                 `json:"declined"`
	DeclinedNote  *string              `json:"declined_note"`

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
// address, and that block's id and node address (`<ref_key>#<block id>`).
type decisionEntryDocument struct {
	ArtifactID string `json:"artifact_id"`
	Ref        string `json:"ref"`
	BlockID    string `json:"block_id"`
	Block      string `json:"block"`
}

// decisionReading is one reading of an entry: its block node, the document that holds it, its
// text as the block index recorded it last (null where its document still owes that index), and
// the person or session its authored_by names, null where the node holds no such tuple - a
// reading the read neither leaves out nor gives an author.
type decisionReading struct {
	Node       decisionEntryNode  `json:"node"`
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

// decisionEntriesRead is the statement decisionEntries reads an issue's entries with, bound to
// the issue $1, the acting credential $2 (refs.VisibilityArg) and entryFactKinds $3. It reads no
// node of another issue: the asks by asks.issue_key, and the decisions from the issue's own
// documents (artifacts.issue_key), each document through its own rows. A document the block
// index has indexed lists its decisions in its document_blocks rows (type `decision`); one that
// owes that index (document_block_versions.version null) lists them as well in the nodes the
// typed-node pass placed in it in the write's own transaction (refs.PlanDecisionNodes, written
// by refs.DecisionIndexing.Queue): the current part_of tuples into the document's own node,
// through graph_tuples_as_of_in, and the nodes its block_refs rows name (a decision's
// decided_by, shaped and from_ask), keyed by the document. Each candidate is then read by its
// own key, the live nodes of the `decision` supertag kept.
func decisionEntriesRead() string {
	return `
		with documents as (
			select a.id, a.ref_key,
			       not exists (select 1 from document_block_versions v where v.artifact_id = a.id and v.version is not null) as owed
			from artifacts a where a.issue_key = $1 and a.kind = 'doc'
		),
		decision_nodes as (
			select held.id from documents doc
			cross join lateral (
				select d.node_address as id from document_blocks d
				where d.artifact_id = doc.id and d.type = '` + refs.DecisionSupertag + `' and d.node_kind = '` + refs.BlockNodeKind + `'
				union
				select part.from_id from (` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = 'artifact' and t.to_id = doc.ref_key and t.kind = '`+graphstore.TuplePartOf+`'`) + `) part
				where doc.owed and part.asserted and part.from_kind = '` + refs.BlockNodeKind + `'
				union
				select r.node_address from block_refs r
				where doc.owed and r.artifact_id = doc.id and r.node_kind = '` + refs.BlockNodeKind + `'
				offset 0
			) held
		),
		members as (
			select node.kind, node.id from asks q
			cross join lateral (
				select g.kind, g.id from graph_nodes g
				where g.kind = '` + graphstore.KindAsk + `' and g.id = q.id::text and g.state = '` + graphstore.StateLive + `'
				  and ` + refs.VisibleRow("g", "$2") + `
				offset 0
			) node
			where q.issue_key = $1 and q.kind = 'question'
			union all
			select node.kind, node.id from decision_nodes n
			cross join lateral (
				select g.kind, g.id from graph_nodes g
				where g.kind = '` + refs.BlockNodeKind + `' and g.id = n.id and g.supertag = '` + refs.DecisionSupertag + `'
				  and g.state = '` + graphstore.StateLive + `' and ` + refs.VisibleRow("g", "$2") + `
				offset 0
			) node
		)
		select m.kind, m.id, q.question, q.options, q.urgency, q.multiple, q.created_at, coalesce(q.block_id, ''),
		       doc.id is not null, coalesce(doc.id::text, ''), coalesce(doc.ref_key, ''), coalesce(doc.slug, ''),
		       coalesce(doc.kind, ''), doc.issue_key, coalesce(doc.project_key, ''), doc.session_id, coalesce(doc.is_primary, false),
		       doc.id is not null and (
		         exists (select 1 from document_blocks b
		                 where m.kind = '` + graphstore.KindAsk + `' and b.artifact_id = doc.id and b.block_id = q.block_id and b.type = 'ask' offset 0)
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
		                 'id', r.id, 'artifact_id', rd.artifact_id, 'block_id', rd.block_id,
		                 'text', (select x.text from document_block_texts x
		                          where x.artifact_id = rd.artifact_id and x.block_id = rd.block_id
		                          order by x.version desc limit 1),
		                 'author_kind', author.to_kind, 'author_id', author.to_id) order by r.id), '[]'::json)
		        from (` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = m.kind and t.to_id = m.id and t.kind = '`+graphstore.TuplePartOf+`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `) part
		        cross join lateral (
		        	select r.kind, r.id from graph_nodes r
		        	where r.kind = part.from_kind and r.id = part.from_id
		        	  and r.supertag = '` + refs.ReadingSupertag + `' and r.state = '` + graphstore.StateLive + `'
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
		        where part.asserted)
		from members m
		left join asks q on m.kind = '` + graphstore.KindAsk + `' and q.id = case when m.kind = '` + graphstore.KindAsk + `' then m.id::uuid end
		left join lateral (
			select a.* from artifacts a
			where m.kind = '` + graphstore.KindAsk + `' and a.id = q.block_artifact_id
			union all
			select a.* from artifacts a
			where m.kind = '` + refs.BlockNodeKind + `' and a.ref_key = split_part(m.id, '#', 1)
		) doc on true
		order by m.kind, m.id`
}

// decisionEntries is the Decisions read's entries for issue key, as who reads them, in one
// statement over graph_nodes, graph_tuples and the asks row for an ask's wording, and then
// decisionread.Decisions over the nodes it found. Both read in one read-only transaction at
// repeatable read, so the entries answer one state of the store: a Keep or an answer committed
// between the two reads is in both or in neither. Its members are the store's:
//
//   - each live `ask` node of an ask of kind question (asks.kind) the issue holds, on any of its
//     documents or on the issue itself. A copy of an answered ask on another document opens no
//     row of its own (its block's copied_from names the source), so it is no entry; a reading in
//     it reads the source's ask and is answered under the source's entry.
//   - each live block node of the `decision` supertag in any document of the issue, found from
//     that document alone (decisionEntriesRead), so a decision of a document that still owes its
//     block index is listed.
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
// each entry that is a decision node or an answered ask, in
// the order of the entries, so a page of the release before reads what the store holds.
func (s *server) decisionEntries(ctx context.Context, who model.Actor, key string, reader *displayTreeReader) ([]decisionEntry, []model.Decision, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("open the read of the decisions of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, decisionEntriesRead(), key, refs.VisibilityArg(who), entryFactKinds)
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
		var askBlock string
		var document, holds bool
		var artifactID, refKey, slug, artifactKind string
		var project string
		var issueKey, session *string
		var primary bool
		var facts, readings []byte
		if err := rows.Scan(&entry.Node.Kind, &entry.Node.ID, &question, &options, &urgency, &multiple, &opened, &askBlock,
			&document, &artifactID, &refKey, &slug, &artifactKind, &issueKey, &project, &session, &primary, &holds,
			&facts, &readings); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read a decision of %s: %w", key, err)
		}
		if document && holds {
			place := decisionEntryDocument{
				ArtifactID: artifactID,
				Ref:        refs.ArtifactRef(issueKey, project, session, slug, artifactKind, primary),
				BlockID:    askBlock,
			}
			if entry.Node.Kind == refs.BlockNodeKind {
				place.BlockID = strings.TrimPrefix(entry.Node.ID, refKey+"#")
			}
			place.Block = refs.BlockAddress(refKey, place.BlockID)
			if place.BlockID != "" {
				entry.Document = &place
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
		nodes[index] = decisionread.Node{Kind: entry.Node.Kind, ID: entry.Node.ID}
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
			decisions = append(decisions, byNode[entry.Node])
		}
	}
	for index := range entries {
		reader.text(&entries[index].TextTree, entries[index].Text)
		reader.textOf(&entries[index].AnswerTree, entries[index].Answer)
		entries[index].SelectedTrees = chosenLabelTrees(reader, entries[index].Selected)
	}
	// kept for one release: compat-objects
	for index := range decisions {
		reader.text(&decisions[index].TextTree, decisions[index].Text)
	}
	return entries, decisions, nil
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
		if held, err := answer.Read(fact.Kind, value); held {
			if err != nil {
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
	entry.AnswerState, entry.Answer, entry.Selected = answer.State, answer.Answer, answer.Selected
	entry.AnsweredAt, entry.AnsweredBy, entry.Resolution = answer.AnsweredAt, answer.AnsweredBy, answer.Resolution
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
