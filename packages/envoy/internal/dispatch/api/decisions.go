package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The server's half of the Decisions view: the Decisions read, Keep, and each person's last read
// version of a document. A decision is one node of the store (dispatch://INFRA-1150 R1): an
// agent's is a `decision` block node of any document of the issue, and a person's is the answered
// ask's own node. Every read of a decision answers from those nodes, through decisionread, and no
// read answers from the text of an issue's decisions document (R11).

// issueDecisions is GET /api/v1/issues/{key}/decisions. entries is the read of the store
// (dispatch://INFRA-1150 R4): one entry for each node that holds a question the issue asked or a
// decision of any of its documents (decisionEntries). open_questions are the open asks its spec
// still holds. document and decisions are the members the release before answers, and they
// answer from the store (R11, ruling F4): decisions is each decision node and each answered ask
// in model.Decision's shape, in the order of entries, and document is the address of the issue's
// decisions document while it stands (decisions_documents()), null once it is retired or where
// the issue holds none.
//
// kept for one release: compat-objects
type issueDecisions struct {
	Entries       []decisionEntry  `json:"entries"`
	Document      *string          `json:"document"`
	OpenQuestions []issueAskRow    `json:"open_questions"`
	Decisions     []model.Decision `json:"decisions"`
}

func (s *server) getIssueDecisions(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	// One reader for the whole answer: the open questions are the same ask rows the ask list
	// answers, so a caller that asked for the trees is given each question's here too. The
	// reader is nil for a caller that did not ask, and every call on it then does nothing.
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	answer := issueDecisions{OpenQuestions: []issueAskRow{}}
	var err error
	if answer.Entries, answer.Decisions, err = s.decisionEntries(r.Context(), who, key, reader); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	spec, specFound, err := s.issueSpecDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if specFound {
		if answer.OpenQuestions, err = s.specOpenQuestions(r.Context(), who, key, spec.ID, reader); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if answer.Document, err = s.standingDecisionsDocument(r.Context(), key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, answer)
}

// standingDecisionsDocument is the dispatch:// address of issue key's decisions document while it
// stands (decisions_documents()), and nil once it is retired or where the issue holds none: the
// Decisions read's old `document` member (dispatch://INFRA-1150 R11, ruling F4). Nothing reads
// the document's text.
func (s *server) standingDecisionsDocument(ctx context.Context, key string) (*string, error) {
	var issueKey, session *string
	var project, slug, kind string
	var primary bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary
		from decisions_documents() d join artifacts a on a.id = d.id
		where d.issue_key = $1 order by a.created_at, a.id limit 1
	`, key).Scan(&issueKey, &project, &session, &slug, &kind, &primary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the decisions document of %s: %w", key, err)
	}
	reference := refs.ArtifactRef(issueKey, project, session, slug, kind, primary)
	return &reference, nil
}

// issueSpecDocument is the issue's primary document, the spec a decision's links point into. An
// issue with none, and an issue that does not exist, answer found false: the view is a read, and
// an issue with no spec simply holds no decision link.
func (s *server) issueSpecDocument(ctx context.Context, key string) (model.Artifact, bool, error) {
	artifact, err := s.loadArtifactRow(ctx, s.deps.Store.Pool, s.deps.Store.Pool.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where issue_key = $1 and is_primary and kind = 'doc'
	`, key))
	if err != nil {
		if artifactMissing(err) {
			return model.Artifact{}, false, nil
		}
		return model.Artifact{}, false, err
	}
	return artifact, true, nil
}

// artifactMissing reports whether err says the issue holds no such document: no row, no document
// of that name, or more than one, each of which the Decisions read takes as an issue with no spec
// rather than as a refusal of the read.
func artifactMissing(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.code == "ARTIFACT_NOT_FOUND" || apiErr.code == "ARTIFACT_REFERENCE_AMBIGUOUS"
}

// specOpenQuestions are the issue's open asks whose block is in its spec, oldest first, in the
// shape every ask read has (askReadColumns, with the newest reply beside the row). reader is the
// answer's own, and each row registers its texts with it once the slice has stopped growing: a
// shaping registers a pointer into the row, and an append that moved the backing array would
// leave the reader filling trees into memory the answer no longer holds.
func (s *server) specOpenQuestions(
	ctx context.Context, who model.Actor, key, specID string, reader *displayTreeReader,
) ([]issueAskRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+askReadColumns+` `+askReadFrom+`
		where a.issue_key = $1 and a.state = 'open' and a.block_artifact_id = $2::uuid
		order by a.created_at, a.id
	`, key, specID)
	if err != nil {
		return nil, err
	}
	questions := []issueAskRow{}
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		questions = append(questions, issueAskRow{Ask: ask, LastReply: reply})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	asks := make([]*model.Ask, len(questions))
	for index := range questions {
		asks[index] = &questions[index].Ask
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, who, asks); err != nil {
		return nil, err
	}
	for index := range questions {
		questions[index].shape(reader)
	}
	return questions, nil
}

// decisionEntry is one entry of the Decisions read's `entries` (dispatch://INFRA-1150 R4): the
// node itself. node is the node that holds it - an ask's own node (kind ask), or an agent's
// decision's block node (kind block) - and ref that node's dispatch:// address, which is where
// the entry links; document is the document whose block shows it, null for an ask on the issue
// itself. kind says whose it is: `person` for an ask, which a person decides, `agent` for a
// decision, and `declined` for an ask the person declined to answer, which records no decision.
// state is the node state of its node, which is any state but gone (decisionEntriesRead), so a
// proposed, settled or overtaken entry is told from a live one (dispatch://INFRA-1950's R5).
//
// An ask carries its wording from its row (question, options, urgency, multiple) and its answer
// from its node (answer_state, answer, selected, answered_by, answered_at, resolution), each null
// on a decision. A declined ask (resolution.kind = "declined") is marked declined, carries the
// person's note as declined_note, and carries no answer and no decided_by. A decision carries
// uncertainty, uncertainty_reason, kept_by and kept_text_sum from its node, each null on an ask
// and where the node holds none. decided_by is the far end of the node's decided_by tuple: the
// person who answered, or the person or session whose credential wrote the decision.
//
// text, text_sha256, replaced, kept, waiting_review, changed_version, changed_at and shaped are
// decisionread.Decisions' for the node, so they read as the Keep and every other read of a
// decision reads them: an ask's text is its question, and a decision's text its own words.
// shaped is one link for each spec node the node's current `shaped` tuples reach, each with the
// tuple's own event, the event that wrote the version the node's text last changed in
// (text_changed_event), and stale. readings are the `reading` nodes, at any state but gone, whose
// part_of reaches this node, each with its node state and the far end of its authored_by tuple,
// null where it holds none.
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

	Question     *string              `json:"question"`
	Options      []model.AskOption    `json:"options"`
	Urgency      *string              `json:"urgency"`
	Multiple     *bool                `json:"multiple"`
	AnswerState  *string              `json:"answer_state"`
	Answer       *string              `json:"answer"`
	Selected     []string             `json:"selected"`
	AnsweredBy   *string              `json:"answered_by"`
	AnsweredAt   *time.Time           `json:"answered_at"`
	Resolution   *model.AskResolution `json:"resolution"`
	Declined     bool                 `json:"declined"`
	DeclinedNote *string              `json:"declined_note"`

	Uncertainty       *string            `json:"uncertainty"`
	UncertaintyReason *string            `json:"uncertainty_reason"`
	DecidedBy         *decisionEntryNode `json:"decided_by"`
	KeptBy            *string            `json:"kept_by"`
	KeptTextSum       *string            `json:"kept_text_sum"`

	Replaced       bool                 `json:"replaced"`
	Kept           *model.DecisionKeep  `json:"kept"`
	WaitingReview  bool                 `json:"waiting_review"`
	ChangedVersion int                  `json:"changed_version"`
	ChangedAt      *time.Time           `json:"changed_at"`
	Shaped         []model.DecisionLink `json:"shaped"`
	Readings       []decisionReading    `json:"readings"`

	// openedAt is the ask row's created_at, which orders the open asks among themselves.
	openedAt time.Time
}

// decisionEntryNode names a node of the store: its kind and its id as graph_nodes keys it.
type decisionEntryNode struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// decisionEntryDocument is the document whose block shows an entry: its id and dispatch://
// address, and that block's id and node address (`<ref_key>#<block id>`).
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

// decisionEntriesRead is the statement decisionEntries reads an issue's entries with, bound to
// the issue $1, the acting credential $2 (refs.VisibilityArg) and entryFactKinds $3. It reads no
// node of another issue: the asks by asks.issue_key, and the decisions from the issue's own
// documents (artifacts.issue_key), each document through its own rows. A document the block
// index has indexed lists its decisions in its document_blocks rows (type `decision`); one that
// owes that index (document_block_versions.version null) lists them as well in the nodes the
// typed-node pass placed in it in the write's own transaction (refs.IndexDecisionNodes): the
// current part_of tuples into the document's own node, through graph_tuples_inbound, and the
// nodes its block_refs rows name (a decision's decided_by, shaped and from_ask), keyed by the
// document. Each candidate is then read by its own key, the present nodes of the `decision`
// supertag kept: a node at any state but gone, so a proposed, settled or overtaken decision, ask
// or reading is drawn wherever its node is (dispatch://INFRA-1950's R5 and R12).
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
			select node.kind, node.id, node.state from asks q
			cross join lateral (
				select g.kind, g.id, g.state from graph_nodes g
				where g.kind = '` + graphstore.KindAsk + `' and g.id = q.id::text and g.state <> '` + graphstore.StateGone + `'
				  and ` + refs.VisibleRow("g", "$2") + `
				offset 0
			) node
			where q.issue_key = $1 and q.kind = 'question'
			union all
			select node.kind, node.id, node.state from decision_nodes n
			cross join lateral (
				select g.kind, g.id, g.state from graph_nodes g
				where g.kind = '` + refs.BlockNodeKind + `' and g.id = n.id and g.supertag = '` + refs.DecisionSupertag + `'
				  and g.state <> '` + graphstore.StateGone + `' and ` + refs.VisibleRow("g", "$2") + `
				offset 0
			) node
		)
		select m.kind, m.id, m.state, q.question, q.options, q.urgency, q.multiple, q.created_at, coalesce(q.block_id, ''),
		       doc.id is not null, coalesce(doc.id::text, ''), coalesce(doc.ref_key, ''), coalesce(doc.slug, ''),
		       coalesce(doc.kind, ''), doc.issue_key, coalesce(doc.project_key, ''), doc.session_id, coalesce(doc.is_primary, false),
		       doc.id is not null and (
		         exists (select 1 from document_blocks b
		                 where m.kind = '` + graphstore.KindAsk + `' and b.artifact_id = doc.id and b.block_id = q.block_id offset 0)
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
		                 'id', r.id, 'state', r.state, 'artifact_id', rd.artifact_id, 'block_id', rd.block_id,
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
//   - each `ask` node, at any state but gone, of an ask of kind question (asks.kind) the issue
//     holds, on any of its documents or on the issue itself. A copy of an answered ask on another
//     document opens no row of its own (its block's copied_from names the source), so it is no
//     entry; a reading in it reads the source's ask and is answered under the source's entry.
//   - each block node of the `decision` supertag, at any state but gone, in any document of the
//     issue, found from that document alone (decisionEntriesRead), so a decision of a document that
//     still owes its block index is listed.
//
// An entry names the document that holds its node's block where a document_blocks row holds that
// block, or where the document owes its block index and no row can say so yet: an answered ask
// whose block left its document keeps its row, which still names that document, and names none.
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
// It answers too the Decisions read's old `decisions` member (dispatch://INFRA-1150 R11, ruling
// F4): decisionread's model.Decision of each entry that is a decision node or an answered ask, in
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
		if err := rows.Scan(&entry.Node.Kind, &entry.Node.ID, &entry.State, &question, &options, &urgency, &multiple, &opened, &askBlock,
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
			place.Block = refKey + "#" + place.BlockID
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
		read = append(read, entry)
	}
	entries = read
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].before(entries[j]) })
	decisions := []model.Decision{}
	for _, entry := range entries {
		if entry.Kind == "agent" || (entry.Kind == "person" && entry.AnswerState != nil && *entry.AnswerState == "answered") {
			decisions = append(decisions, byNode[entry.Node])
		}
	}
	for index := range entries {
		reader.text(&entries[index].TextTree, entries[index].Text)
	}
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
// from the JSON list the statement aggregated, or answers which of them does not parse. A
// declined ask carries its note and no answer, and an ask carries no Keep.
func (entry *decisionEntry) readFacts(encoded []byte) error {
	var facts []struct {
		Kind   string `json:"kind"`
		ToKind string `json:"to_kind"`
		ToID   string `json:"to_id"`
	}
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return fmt.Errorf("the node's values: %w", err)
	}
	for _, fact := range facts {
		value := fact.ToID
		switch fact.Kind {
		case graphstore.TupleAnswerState:
			switch value {
			case "open", "answered", "resolved":
				entry.AnswerState = &value
			default:
				return fmt.Errorf("answer_state holds %q, which is none of open, answered and resolved", value)
			}
		case graphstore.TupleAnswer:
			entry.Answer = &value
		case graphstore.TupleSelected:
			selected := []string{}
			if err := json.Unmarshal([]byte(value), &selected); err != nil {
				return fmt.Errorf("selected holds %q, which is no list of labels: %w", value, err)
			}
			entry.Selected = selected
		case graphstore.TupleAnsweredAt:
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return fmt.Errorf("answered_at holds %q, which is no time: %w", value, err)
			}
			entry.AnsweredAt = &at
		case graphstore.TupleResolution:
			var resolution model.AskResolution
			if err := json.Unmarshal([]byte(value), &resolution); err != nil {
				return fmt.Errorf("resolution holds %q, which is no resolution: %w", value, err)
			}
			entry.Resolution = &resolution
		case graphstore.TupleAnsweredBy:
			entry.AnsweredBy = &value
		case graphstore.TupleDecidedBy:
			entry.DecidedBy = &decisionEntryNode{Kind: fact.ToKind, ID: value}
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

// keepDecisionInput is the body of POST /api/v1/artifacts/{id}/blocks/{block_id}/keep.
type keepDecisionInput struct {
	TextSHA256 string `json:"text_sha256" required:"true"`
}

// keepDecision is POST /api/v1/artifacts/{id}/blocks/{block_id}/keep {"text_sha256": "<sum>"}:
// the caller agrees with a decision, as the text they read it at. The block is an agent's
// decision, a `decision` block of any document of the issue, whose node the Keep acts on
// (keepDecisionNode); or, until the decisions step retires it, the heading block a section of the
// issue's decisions document opens with, kept on the decision_keeps row alone, which the
// conversion carries onto the section's decision node (dispatch://INFRA-1150 R11, ruling F3). The
// sum is the `text_sha256` the read served them, and a decision whose text has changed since is
// 409 DECISION_CHANGED with nothing written: a Keep over text nobody read is the one thing Keep
// exists to prevent, as `409 ASK_EDITED` holds an answer to the wording it was given on. Any other
// block - one that is neither, a heading of a retired decisions document, or a decision marked
// replaced, which is no longer the design - is 404 TARGET_NOT_FOUND. The route is the `person`
// scope's (routeScopes): a Keep is a person's.
func (s *server) keepDecision(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input keepDecisionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.TextSHA256 == "" {
		writeError(w, "INVALID_KEEP", http.StatusBadRequest,
			"text_sha256 is the text_sha256 the decision was read at")
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	blockID := r.PathValue("block_id")
	decision, onNode, err := s.keptDecision(r.Context(), actor, document, blockID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if decision.TextSHA256 != input.TextSHA256 {
		writeError(w, "DECISION_CHANGED", http.StatusConflict,
			"the decision changed since it was read; read it again and keep what it says now")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var keep model.DecisionKeep
	var event model.Event
	if onNode {
		keep, event, err = s.keepDecisionNode(r.Context(), tx, actor, document, decision)
	} else {
		keep, event, err = s.keepDecisionHeading(r.Context(), tx, actor, document, decision)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	WriteJSON(w, http.StatusOK, keep)
}

// keepDecisionHeading is the Keep of decision, a section of the issue's decisions document opened
// by its heading block, in tx: the decision_keeps row of that block, which no node projects, and
// the decision.kept that records it. It takes the issue's owner row first, as keepDecisionNode does
// and as the decisions step takes it for the whole of its write of the issue, and reads the
// heading's section again under it, as keptDecision read it (docs' DecisionHeading): a Keep that
// waited behind the step's conversion of the section, or of the whole document and its
// retirement, would otherwise write its row on a heading the step carried onto no node, and
// answer 200. Such a Keep - the heading opening no section of a decisions document that stands -
// is 404 TARGET_NOT_FOUND, with nothing written, as a Keep of a heading of a retired document is
// (dispatch://INFRA-1150 F3); one whose section's text changed while it waited is 409
// DECISION_CHANGED, as keepDecision answers one posted over text that changed before.
// kept for one release: heading-keep
func (s *server) keepDecisionHeading(ctx context.Context, tx pgx.Tx, actor model.Actor, document model.Artifact, decision model.Decision) (model.DecisionKeep, model.Event, error) {
	own := issueOwner(*document.IssueKey)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	heading, found, err := s.deps.Docs.DecisionHeading(ctx, tx, document.ID, decision.BlockID)
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	if !found {
		return model.DecisionKeep{}, model.Event{}, errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", decision.BlockID, document.Name)
	}
	if heading.TextSHA256 != decision.TextSHA256 {
		return model.DecisionKeep{}, model.Event{}, errorf(http.StatusConflict, "DECISION_CHANGED",
			"the decision changed since it was read; read it again and keep what it says now")
	}
	keep := model.DecisionKeep{ArtifactID: document.ID}
	if err := tx.QueryRow(ctx, `
		insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
		values ($1::uuid, $2, $3, $4, $5)
		on conflict (artifact_id, block_id) do update
			set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
				kept_by = excluded.kept_by, kept_at = now()
		returning block_id, text_sha256, changed_version, kept_by, kept_at
	`, document.ID, decision.BlockID, decision.TextSHA256, decision.ChangedVersion,
		model.CanonicalLogin(actor.ID)).
		Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	event, err := s.appendEvent(ctx, tx, own.event("decision.kept", actor, map[string]any{
		"artifact_id":     document.ID,
		"block_id":        decision.BlockID,
		"changed_version": keep.ChangedVersion,
	}))
	return keep, event, err
}

// keepDecisionNode is the Keep of decision, an agent's decision node of document (readDecisionNode),
// in tx, as two appends on the node (dispatch://INFRA-1150 R2): a kept_by tuple to the person who
// kept it and a kept_text_sum value holding the text sum they read it at, with decision_keeps, the
// row the decision's block keys, as their projection, written in the same transaction. The Keep
// records that the decision has been reviewed and not who else did, so a kept_by to a person
// another Keep named is retracted, and the node holds one. The row's changed_version is the
// version that last changed the decision's text (readDecisionNode).
//
// It writes as the store's writers do: the issue's owner row first, then the person's node (one
// the store already holds moves nothing), the projection row, the decision.kept event, and last
// the tuples, under that event (graphstore.WriteUncaused, graphstore.Caused).
func (s *server) keepDecisionNode(ctx context.Context, tx pgx.Tx, actor model.Actor, document model.Artifact, decision model.Decision) (model.DecisionKeep, model.Event, error) {
	own := issueOwner(*document.IssueKey)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	login := model.CanonicalLogin(actor.ID)
	address := decisionNodeAddress(document, decision.BlockID)
	// The tuple's time is the projection row's, to the microsecond a timestamptz holds.
	at := time.Now().UTC().Truncate(time.Microsecond)
	earlier, err := graphstore.CurrentOut(ctx, tx, refs.BlockNodeKind, address, []string{refs.TupleKeptBy})
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	var tuples []graphstore.Tuple
	for _, kept := range earlier {
		if kept.ToKind != graphstore.KindPerson || kept.ToID != login {
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.TupleKeptBy,
				ToKind: kept.ToKind, ToID: kept.ToID, At: at, Who: actor, Asserted: false,
			})
		}
	}
	tuples = append(tuples,
		graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.TupleKeptBy,
			ToKind: graphstore.KindPerson, ToID: login, At: at, Who: actor, Asserted: true,
		},
		graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: refs.ValueKeptTextSum,
			ToKind: graphstore.KindValue, ToID: decision.TextSHA256, At: at, Who: actor, Asserted: true,
		},
	)
	held, err := graphstore.WriteUncaused(ctx, tx, []graphstore.Node{graphstore.PersonNode(login, actor, at, graphstore.NoEvent)}, tuples)
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	keep := model.DecisionKeep{ArtifactID: document.ID}
	if err := tx.QueryRow(ctx, `
		insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by, kept_at)
		values ($1::uuid, $2, $3, $4, $5, $6)
		on conflict (artifact_id, block_id) do update
			set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
				kept_by = excluded.kept_by, kept_at = excluded.kept_at
		returning block_id, text_sha256, changed_version, kept_by, kept_at
	`, document.ID, decision.BlockID, decision.TextSHA256, decision.ChangedVersion, login, at).
		Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	event, err := s.appendEvent(ctx, tx, own.event("decision.kept", actor, map[string]any{
		"artifact_id":     document.ID,
		"block_id":        decision.BlockID,
		"changed_version": keep.ChangedVersion,
	}))
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	if err := graphstore.Caused(ctx, tx, held, event.ID); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	return keep, event, nil
}

// keptDecision is the decision Keep acts on at blockID of document, and whether it is a node's:
// an agent's decision node of a document of the issue (readDecisionNode), else the section a
// heading block of the issue's decisions document opens while that document stands, read through
// the decisions step's section reader (docs' DecisionHeading over decisions_documents(),
// dispatch://INFRA-1150 R11, ruling F3). A decision marked replaced, a heading under `## Replaced`
// among them, and a heading of a retired decisions document are no decision Keep acts on.
func (s *server) keptDecision(ctx context.Context, who model.Actor, document model.Artifact, blockID string) (model.Decision, bool, error) {
	notFound := errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", blockID, document.Name)
	if document.IssueKey == nil {
		return model.Decision{}, false, notFound
	}
	node, found, err := s.readDecisionNode(ctx, s.deps.Store.Pool, who, document, blockID)
	if err != nil {
		return model.Decision{}, false, err
	}
	if found {
		if node.Replaced {
			return model.Decision{}, false, notFound
		}
		return node, true, nil
	}
	// kept for one release: heading-keep
	heading, found, err := s.deps.Docs.DecisionHeading(ctx, s.deps.Store.Pool, document.ID, blockID)
	if err != nil {
		return model.Decision{}, false, err
	}
	if !found {
		return model.Decision{}, false, notFound
	}
	return model.Decision{
		BlockID: blockID, Text: heading.Text, TextSHA256: heading.TextSHA256,
		ChangedVersion: heading.ChangedVersion, Kind: "agent",
	}, false, nil
}

// decisionNodeAddress is the address of the node of block blockID of document, as the block
// index keys it: a U+0000 reads as U+FFFD, as the rendering writes it.
func decisionNodeAddress(document model.Artifact, blockID string) string {
	return document.RefKey + "#" + strings.ReplaceAll(blockID, "\x00", "\uFFFD")
}

// readDecisionNode is the agent's decision whose node is block blockID of document, a document
// of an issue, as who reads it, and whether the store holds such a node: a node of the `decision`
// supertag at that block's address, at any state but gone. It is read through
// decisionread.Decisions, the one reader every read of a decision node answers from, so the Keep
// compares a posted sum with the sum the Decisions read served, and writes the version its words
// last changed in: its words are its own (refs.DecisionWords), and no reading it closes with
// moves its text, its sum or that version.
func (s *server) readDecisionNode(ctx context.Context, q queryer, who model.Actor, document model.Artifact, blockID string) (model.Decision, bool, error) {
	if document.IssueKey == nil || document.Kind != "doc" {
		return model.Decision{}, false, nil
	}
	decisions, err := decisionread.Decisions(ctx, q, who,
		[]decisionread.Node{{Kind: refs.BlockNodeKind, ID: decisionNodeAddress(document, blockID)}})
	if err != nil || len(decisions) == 0 || decisions[0].Kind != "agent" {
		return model.Decision{}, false, err
	}
	return decisions[0], true, nil
}

// getDocumentRead is GET /api/v1/me/documents/{id}/read: the caller's last read version of the
// document, null when they have read none. It is per-person state, as GET /api/v1/me/state is.
func (s *server) getDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version *int
	var at *time.Time
	err = s.deps.Store.Pool.QueryRow(r.Context(), `
		select version, read_at from user_document_reads where login = $1 and artifact_id = $2::uuid
	`, model.CanonicalLogin(actor.ID), document.ID).Scan(&version, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = version, at
	WriteJSON(w, http.StatusOK, read)
}

// documentReadInput is the body of PUT /api/v1/me/documents/{id}/read.
type documentReadInput struct {
	Version *int `json:"version" required:"true"`
}

// putDocumentRead is PUT /api/v1/me/documents/{id}/read {"version": n}: the caller has read the
// document through that version. It is the caller's own view state, written as PUT
// /api/v1/me/issues/{key}/state is, and announces nothing.
func (s *server) putDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input documentReadInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Version == nil || *input.Version < 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "version must be a whole number that is not negative")
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version int
	var at time.Time
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_document_reads (login, artifact_id, version) values ($1, $2::uuid, $3)
		on conflict (login, artifact_id) do update set version = excluded.version, read_at = now()
		returning version, read_at
	`, model.CanonicalLogin(actor.ID), document.ID, *input.Version).Scan(&version, &at); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = &version, &at
	WriteJSON(w, http.StatusOK, read)
}
