package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An issue's decisions are its document `decisions.md`, and this file is the server's half of the
// Decisions view: the view itself, Keep, and each person's last read version of a document.
//
// Each decision is one `##` section of that document: its heading block and every block after it
// up to the next heading of its level or higher. Its heading names the choice, its blocks give the
// reason and the options weighed, and each spec block it shaped is linked
// `dispatch://KEY/spec#b-<id>`, which the block index records as a link from the decision's own
// block to that spec block. The section headed exactly `## Replaced` holds replaced decisions as
// `###` sections, a paragraph that starts `Unsure:` marks a decision unsure, and a person's
// decision links its answered ask.
//
// A spec block X linked from decision D by link L is out of date exactly when D's own text
// changed after it first linked X and X has not changed since D did, both read from the
// document's text at a version (markStaleLinks) rather than from D's changed version, which is
// the greatest of the blocks D holds now and so does not move when a block leaves D or joins it.
// A new decision therefore marks nothing, and neither does the block index's first pass over a
// document, which gives every block and every pair one version.
//
// The links are read through refs.BlockLinks, the read GET /api/v1/block-links already serves, and
// each block's own changed version through refs.BlockChanges: this view writes no query for a link
// the block graph already answers.

const (
	// decisionsDocumentName is the document an issue's decisions live in, and
	// decisionsDocumentSlug the slug its reference names
	// (`dispatch://KEY/artifact/decisions-md`). The view reads the slug first and falls back to
	// the filename, as every document reference resolves.
	decisionsDocumentName = "decisions.md"
	decisionsDocumentSlug = "decisions-md"
	// replacedHeading is the section that holds replaced decisions; its own `###` sections are
	// the replaced ones, and it is no decision itself.
	replacedHeading = "Replaced"
	// unsurePrefix opens the paragraph that marks a decision unsure and says what would change
	// the agent's mind.
	unsurePrefix = "Unsure:"
)

// issueDecisions is GET /api/v1/issues/{key}/decisions. entries is the read of the store
// alone: one entry for each node that holds a question the issue asked or a
// decision of any of its documents (decisionEntries). document, open_questions and decisions are
// the members the release before answers, read as it reads them: the issue's decisions document,
// the open questions its spec still holds, and each decision of that document in document order.
// document is null, with no decisions, when the issue has no decisions.md.
type issueDecisions struct {
	Entries       []decisionEntry  `json:"entries"`
	Document      *string          `json:"document"`
	OpenQuestions []issueAskRow    `json:"open_questions"`
	Decisions     []model.Decision `json:"decisions"`
}

// decisionSection is one decision as the document's own blocks give it, before the links, asks and
// keeps that complete it are read. Start and End are its first and last block's bounds in the
// document's canonical markdown.
type decisionSection struct {
	headingBlock string
	heading      string
	level        int
	replaced     bool
	unsure       bool
	blockIDs     []string
	start        int
	end          int
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
	answer := issueDecisions{OpenQuestions: []issueAskRow{}, Decisions: []model.Decision{}}
	var err error
	if answer.Entries, err = s.decisionEntries(r.Context(), who, key, reader); err != nil {
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
	document, found, err := s.issueDecisionsDocument(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		reader.resolve()
		WriteJSON(w, http.StatusOK, answer)
		return
	}
	reference := artifactReference(document)
	answer.Document = &reference
	specID := ""
	if specFound {
		specID = spec.ID
	}
	if answer.Decisions, err = s.readDecisions(r.Context(), who, document, reference, specID, reader); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, answer)
}

// artifactReference is a document's own dispatch:// address.
func artifactReference(document model.Artifact) string {
	return refs.ArtifactRef(document.IssueKey, document.Project, document.SessionID, document.Slug,
		document.Kind, document.Primary)
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

// issueDecisionsDocument is the issue's decisions.md: the document at the slug its reference
// names, else the one whose filename it is, as every document reference resolves.
func (s *server) issueDecisionsDocument(ctx context.Context, key string) (model.Artifact, bool, error) {
	target := artifactTarget{IssueKey: &key}
	document, err := s.loadArtifactByRefKey(ctx, s.deps.Store.Pool, target.refPrefix()+"/"+decisionsDocumentSlug)
	if err == nil {
		return document, true, nil
	}
	if !isArtifactNotFound(err) {
		return model.Artifact{}, false, err
	}
	document, err = s.loadArtifactByDocumentReference(ctx, s.deps.Store.Pool, target, decisionsDocumentName)
	if err != nil {
		if artifactMissing(err) {
			return model.Artifact{}, false, nil
		}
		return model.Artifact{}, false, err
	}
	return document, true, nil
}

// artifactMissing reports whether err says the issue holds no such document: no row, no document
// of that name, or more than one, each of which this view reads as an issue with no decisions.md
// rather than as a refusal of the read.
func artifactMissing(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.code == "ARTIFACT_NOT_FOUND" || apiErr.code == "ARTIFACT_REFERENCE_AMBIGUOUS"
}

// specOpenQuestions are the issue's open asks whose block is in its spec, oldest first, in the
// shape every ask read has (askReadColumns, with the newest reply beside the row). An ask is open
// as its node holds it (askNodeAnswer.applyTo), which every read of an answer reads (R3), so the
// statement reads each of the spec's asks and the scan keeps the open ones. An ask's block is in
// the spec where the spec's block index holds an ask block under its block id (document_blocks): a
// row that names the spec and a block it no longer holds - one a reopen restored with no block to
// write, which a later settlement retracts - is no question of it. reader is the answer's
// own, and each row registers its texts with it once the slice has stopped growing: a
// shaping registers a pointer into the row, and an append that moved the backing array would
// leave the reader filling trees into memory the answer no longer holds.
func (s *server) specOpenQuestions(
	ctx context.Context, who model.Actor, key, specID string, reader *displayTreeReader,
) ([]issueAskRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+askReadColumns+` `+askReadFrom+`
		where a.issue_key = $1 and a.block_artifact_id = $2::uuid
		  and exists (
			select 1 from document_blocks d
			where d.artifact_id = a.block_artifact_id and d.block_id = a.block_id and d.type = 'ask')
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
		if ask.State != "open" {
			continue
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

// decisionEntry is one entry of the Decisions read's `entries`: the
// node itself. node is the node that holds it - an ask's own node (kind ask), or an agent's
// decision's block node (kind block) - and ref that node's dispatch:// address, which is where
// the entry links; document is the document whose block shows it, null for an ask on the issue
// itself. kind says whose it is: `person` for an ask, which a person decides, `agent` for a
// decision, and `declined` for an ask the person declined to answer, which records no decision.
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
// (text_changed_event), stale, true when the decision's own words changed after the tuple and the
// node's text has not changed since they did, and changed_since_tie, true when the node's text
// changed after the tuple (model.DecisionLink). readings are the live `reading` nodes whose
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
// through graph_tuples_inbound, and the nodes its block_refs rows name (a decision's
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
func (s *server) decisionEntries(ctx context.Context, who model.Actor, key string, reader *displayTreeReader) ([]decisionEntry, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("open the read of the decisions of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, decisionEntriesRead(), key, refs.VisibilityArg(who), entryFactKinds)
	if err != nil {
		return nil, fmt.Errorf("read the decisions of %s: %w", key, err)
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
			return nil, fmt.Errorf("read a decision of %s: %w", key, err)
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
			return nil, fmt.Errorf("read the readings of %s %s: %w", entry.Node.Kind, entry.Node.ID, err)
		}
		entries = append(entries, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the decisions of %s: %w", key, err)
	}
	nodes := make([]decisionread.Node, len(entries))
	for index, entry := range entries {
		nodes[index] = decisionread.Node{Kind: entry.Node.Kind, ID: entry.Node.ID}
	}
	decided, err := decisionread.Decisions(ctx, tx, who, nodes)
	if err != nil {
		return nil, err
	}
	if err := decisionDeciderTitles(ctx, tx, entries); err != nil {
		return nil, fmt.Errorf("name the deciders of the decisions of %s: %w", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("close the read of the decisions of %s: %w", key, err)
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
	for index := range entries {
		reader.text(&entries[index].TextTree, entries[index].Text)
		reader.textOf(&entries[index].AnswerTree, entries[index].Answer)
	}
	return entries, nil
}

// before reports whether entry comes before other in the Decisions read: an open ask before every
// other entry, open asks oldest first, and every other entry by its time, oldest first, a node's
// id breaking a tie.
func (entry decisionEntry) before(other decisionEntry) bool {
	open, otherOpen := entry.isOpen(), other.isOpen()
	if open != otherOpen {
		return open
	}
	at, otherAt := entry.at(), other.at()
	if open {
		at, otherAt = entry.openedAt, other.openedAt
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
// answer state, as every ask read reads it (askNodeAnswer), and the decision's own. A declined ask
// carries its note and no answer, and an ask carries no Keep.
func (entry *decisionEntry) readFacts(encoded []byte) error {
	var facts []nodeFact
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return fmt.Errorf("the node's values: %w", err)
	}
	var answer askNodeAnswer
	for _, fact := range facts {
		value := fact.ToID
		if held, err := answer.read(fact.Kind, value); held {
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

// readDecisions builds every decision of the decisions document: its sections from the document's
// own blocks, its links from the block graph (refs.BlockLinks), its asks from the block mentions
// its blocks hold, and its keeps from decision_keeps. specID is "" when the issue has no spec, and
// every decision then has no links.
//
// reader is the answer's own, and each decision registers its text with it once the slice has
// stopped growing, as the open questions beside them do (specOpenQuestions): a shaping registers
// a pointer into the row, and an append that moved the backing array would leave the reader
// filling a tree into memory the answer no longer holds. Keep reads the decisions to compare one
// sum and answers none of them, so it passes no reader and no tree is read for it.
func (s *server) readDecisions(
	ctx context.Context, who model.Actor, document model.Artifact, reference, specID string, reader *displayTreeReader,
) ([]model.Decision, error) {
	markdown, sections, err := s.decisionSections(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	changes, err := refs.BlockChanges(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	var links []model.BlockLink
	if specID != "" {
		if links, err = refs.BlockLinks(ctx, s.deps.Store.Pool, who, document.ID, specID); err != nil {
			return nil, err
		}
	}
	asks, err := decisionBlockAsks(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	keeps, err := decisionKeeps(ctx, s.deps.Store.Pool, document.ID)
	if err != nil {
		return nil, err
	}
	decisions := make([]model.Decision, 0, len(sections))
	keys := make([]sectionKey, 0, len(sections))
	seen := map[string]int{}
	for _, section := range sections {
		keys = append(keys, sectionKey{heading: section.heading, occurrence: seen[section.heading]})
		seen[section.heading]++
		decision := model.Decision{
			BlockID:  section.headingBlock,
			Heading:  section.heading,
			Ref:      refs.BlockRef(reference, section.headingBlock),
			Text:     sliceMarkdown(markdown, section.start, section.end),
			Kind:     "agent",
			Unsure:   section.unsure,
			Replaced: section.replaced,
			Asks:     []string{},
			Links:    []model.DecisionLink{},
		}
		held := make(map[string]bool, len(section.blockIDs))
		for _, blockID := range section.blockIDs {
			held[blockID] = true
			if change, ok := changes[blockID]; ok && change.Version > decision.ChangedVersion {
				at := change.At
				decision.ChangedVersion, decision.ChangedAt = change.Version, &at
			}
			decision.Asks = append(decision.Asks, asks[blockID]...)
		}
		for _, link := range links {
			if !held[link.From.BlockID] {
				continue
			}
			decision.Links = append(decision.Links, model.DecisionLink{
				FromBlock:      link.From.BlockID,
				To:             link.To,
				CreatedVersion: link.CreatedVersion,
				CreatedAt:      link.CreatedAt,
			})
		}
		if len(decision.Asks) > 0 {
			decision.Kind = "person"
		}
		if keep, ok := keeps[section.headingBlock]; ok {
			decision.Kept = &keep
		}
		decision.TextSHA256 = decisionread.TextSum(decision.Text)
		decision.WaitingReview = waitsForReview(decision)
		decisions = append(decisions, decision)
	}
	for index := range decisions {
		reader.text(&decisions[index].TextTree, decisions[index].Text)
	}
	if err := s.markStaleLinks(ctx, document.ID, markdown, decisions, keys); err != nil {
		return nil, err
	}
	return decisions, nil
}

// markStaleLinks decides each link's `stale`: the decision's own text changed after it first
// linked the spec block, and that spec block has not changed since the decision did.
//
// Both halves are answered by the decision's text at a version, never by its changed version:
// that version is the greatest of the blocks the decision holds now, so a block removed from the
// decision, or moved into or out of it, changes the text and leaves the version where it was
// (the reviewer's measurement on #2626). The text at the version that created the link says
// whether the decision changed since; the text at the version current when the spec block last
// changed says whether it changed since that, which is what clears a link the spec answered.
//
// A version this document does not hold, and a spec block Dispatch records no change for, each
// leave the link as it is: the view marks nothing it cannot read, as the block index's first
// pass over a document marks nothing. Each decision is named across versions by its heading and
// its occurrence among the decisions of that heading (sectionKey), the same pair the view's own
// walk gives it, so a document that carries one heading twice still names each of them.
func (s *server) markStaleLinks(
	ctx context.Context, artifactID, markdown string, decisions []model.Decision, keys []sectionKey,
) error {
	times := map[int64]time.Time{}
	numbers := map[int]bool{}
	for _, decision := range decisions {
		for _, link := range decision.Links {
			numbers[link.CreatedVersion] = true
			if link.To.ChangedAt != nil {
				times[link.To.ChangedAt.UnixNano()] = *link.To.ChangedAt
			}
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	atTime, err := s.versionsAtTimes(ctx, artifactID, sortedTimes(times))
	if err != nil {
		return err
	}
	for _, number := range atTime {
		numbers[number] = true
	}
	sums, err := s.decisionSumsAt(ctx, artifactID, sortedNumbers(numbers))
	if err != nil {
		return err
	}
	current := decisionSums(markdown)
	for i := range decisions {
		decision := &decisions[i]
		now, ok := current[keys[i]]
		if !ok {
			continue
		}
		for j := range decision.Links {
			decision.Links[j].Stale = staleLink(now, keys[i], decision.Links[j], sums, atTime)
		}
	}
	return nil
}

// staleLink is that rule for one link, over the sums markStaleLinks read. A time keys those
// reads by its own instant (UnixNano), since a time read back from Postgres is the same instant
// in another `time.Time` value, which no map matches.
//
// A version this document does not hold leaves the link as it is. A version it holds that gives
// this decision no sum - its heading was renamed since, or that version carried fewer decisions
// of that heading - is text that differs from the text now, which it is.
func staleLink(
	now string,
	key sectionKey,
	link model.DecisionLink,
	sums map[int]map[sectionKey]string,
	atTime map[int64]int,
) bool {
	atCreated, read := sums[link.CreatedVersion]
	if !read || atCreated[key] == now {
		return false
	}
	if link.To.ChangedAt == nil {
		return false
	}
	number, asked := atTime[link.To.ChangedAt.UnixNano()]
	if !asked {
		return false
	}
	if number == 0 {
		// The spec block last changed before this document held a version of its own, so every
		// change of the decision came after it.
		return true
	}
	atChange, read := sums[number]
	return read && atChange[key] != now
}

// versionsAtTimes is, for each time, the document's greatest version written at or before it:
// the version of decisions.md that stood when a spec block changed, keyed by the time's own
// instant. A time before the document's first version answers 0.
func (s *server) versionsAtTimes(
	ctx context.Context, artifactID string, times []time.Time,
) (map[int64]int, error) {
	at := map[int64]int{}
	if len(times) == 0 {
		return at, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select asked.ord, version.number
		from unnest($2::timestamptz[]) with ordinality as asked(at, ord)
		left join lateral (
			select v.number from artifact_versions v
			where v.artifact_id = $1::uuid and v.created_at <= asked.at
			order by v.number desc limit 1
		) version on true
	`, artifactID, times)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var number *int
		if err := rows.Scan(&ordinal, &number); err != nil {
			return nil, err
		}
		if ordinal < 1 || ordinal > len(times) {
			continue
		}
		at[times[ordinal-1].UnixNano()] = 0
		if number != nil {
			at[times[ordinal-1].UnixNano()] = *number
		}
	}
	return at, rows.Err()
}

// decisionSumsAt is each named version's decisions, with the sum of each one's text: the
// version's own stored markdown, cut by the same rule as the document as it stands.
func (s *server) decisionSumsAt(
	ctx context.Context, artifactID string, numbers []int,
) (map[int]map[sectionKey]string, error) {
	sums := map[int]map[sectionKey]string{}
	if len(numbers) == 0 {
		return sums, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select number, coalesce(markdown, '') from artifact_versions
		where artifact_id = $1::uuid and number = any($2::int[])
	`, artifactID, numbers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var number int
		var markdown string
		if err := rows.Scan(&number, &markdown); err != nil {
			return nil, err
		}
		sums[number] = decisionSums(markdown)
	}
	return sums, rows.Err()
}

// decisionSums cuts a document's markdown into its decisions and sums each one's text. It cuts
// by the rule the block walk cuts the document that stands by (decisionSections): a `##` heading
// outside `## Replaced` and a `###` heading under it opens a decision, a heading of its own
// level or higher closes it - `#` included, which opens none - and a deeper heading is text of
// the decision it stands in.
//
// A stored version holds no block id, so a decision is keyed by its heading and by how many
// decisions before it carry that heading (sectionKey): a document that carries one heading twice,
// which a replaced decision made again does, is two decisions here as it is two in the view,
// rather than one the sum cannot name. Its sum is taken over the section's text with the blank
// lines at its end removed, so the same section of two versions sums alike whatever follows it.
func decisionSums(markdown string) map[sectionKey]string {
	sums := map[sectionKey]string{}
	seen := map[string]int{}
	type section struct {
		key   sectionKey
		level int
		start int
	}
	var open *section
	inReplaced := false
	fenced := ""
	offset := 0
	closeAt := func(end int) {
		if open == nil {
			return
		}
		sums[open.key] = decisionread.TextSum(strings.TrimRight(markdown[open.start:end], "\n"))
		open = nil
	}
	for _, line := range strings.SplitAfter(markdown, "\n") {
		if line == "" {
			break
		}
		trimmed := strings.TrimRight(line, "\n")
		if fence := codeFence(trimmed); fence != "" {
			if fenced == "" {
				fenced = fence
			} else if strings.HasPrefix(fence, fenced[:1]) && len(fence) >= len(fenced) {
				fenced = ""
			}
			offset += len(line)
			continue
		}
		if fenced == "" {
			if level, heading := markdownHeading(trimmed); level > 0 {
				if open != nil && level <= open.level {
					closeAt(offset)
				}
				if level <= 2 {
					inReplaced = level == 2 && heading == replacedHeading
				}
				if open == nil && isDecisionHeading(level, inReplaced) {
					occurrence := seen[heading]
					seen[heading] = occurrence + 1
					open = &section{
						key:   sectionKey{heading: heading, occurrence: occurrence},
						level: level,
						start: offset,
					}
				}
			}
		}
		offset += len(line)
	}
	closeAt(len(markdown))
	return sums
}

// sectionKey names one decision of a document across its versions: its heading, and how many
// decisions before it carry that heading.
type sectionKey struct {
	heading    string
	occurrence int
}

// codeFence is the fence a line opens or closes a fenced code block with (three or more
// backticks or tildes, at most three spaces in), else "". A heading inside such a block is the
// code's own text.
func codeFence(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			run := 0
			for run < len(trimmed) && trimmed[run] == marker[0] {
				run++
			}
			return trimmed[:run]
		}
	}
	return ""
}

// markdownHeading is a line's ATX heading level and title, or level 0 for a line that is no
// heading. It reads a title as `headingOf` reads one off a heading block's own markdown, closing
// hashes and all, so the heading a version is keyed by is the heading the view names.
func markdownHeading(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}

// sortedNumbers and sortedTimes are the keys of a set, in order, so one read asks for each once.
func sortedNumbers(set map[int]bool) []int {
	numbers := make([]int, 0, len(set))
	for number := range set {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

func sortedTimes(set map[int64]time.Time) []time.Time {
	times := make([]time.Time, 0, len(set))
	for _, at := range set {
		times = append(times, at)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return times
}

// waitsForReview is the rule a decision's review state reads, for the
// decisions document's sections: an agent's decision waits for a person's review unless it is
// marked replaced, or the text sum the Keep recorded on it is its text sum as it stands. A
// person's decision, an answered ask, never waits: the person decided it. A decision node's
// review state is decisionread's, by the same rule.
func waitsForReview(decision model.Decision) bool {
	return decision.Kind == "agent" && !decision.Replaced &&
		(decision.Kept == nil || decision.Kept.TextSHA256 != decision.TextSHA256)
}

// decisionSections cuts the decisions document into its sections: each `##` heading outside
// `## Replaced`, and each `###` heading under it, with every block after the heading up to the
// next heading of its level or higher.
func (s *server) decisionSections(ctx context.Context, artifactID string) (string, []decisionSection, error) {
	markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, artifactID)
	if err != nil {
		return "", nil, err
	}
	sort.SliceStable(blocks, func(a, b int) bool { return blocks[a].From < blocks[b].From })
	var sections []decisionSection
	var current *decisionSection
	inReplaced := false
	for _, block := range blocks {
		level, title := headingOf(markdown, block)
		if level > 0 {
			if current != nil && level <= current.level {
				sections = append(sections, *current)
				current = nil
			}
			if level <= 2 {
				inReplaced = level == 2 && title == replacedHeading
			}
			if current == nil {
				if !isDecisionHeading(level, inReplaced) {
					continue
				}
				current = &decisionSection{
					headingBlock: block.ID,
					heading:      title,
					level:        level,
					replaced:     inReplaced,
					blockIDs:     []string{block.ID},
					start:        block.From,
					end:          block.To,
				}
				continue
			}
		}
		if current == nil {
			continue
		}
		current.blockIDs = append(current.blockIDs, block.ID)
		if block.To > current.end {
			current.end = block.To
		}
		if block.Type == "paragraph" &&
			strings.HasPrefix(strings.TrimSpace(sliceMarkdown(markdown, block.From, block.To)), unsurePrefix) {
			current.unsure = true
		}
	}
	if current != nil {
		sections = append(sections, *current)
	}
	return markdown, sections, nil
}

// isDecisionHeading reports whether a heading of this level opens a decision: a `##` section
// outside `## Replaced`, or a `###` section under it.
func isDecisionHeading(level int, inReplaced bool) bool {
	if inReplaced {
		return level == 3
	}
	return level == 2
}

// headingOf reads a block's heading level and title from its own canonical markdown: `## Title` is
// level 2 with the title "Title". A block that is no heading answers level 0.
func headingOf(markdown string, block model.ArtifactBlock) (int, string) {
	if block.Type != "heading" {
		return 0, ""
	}
	text := strings.TrimSpace(sliceMarkdown(markdown, block.From, block.To))
	level := 0
	for level < len(text) && text[level] == '#' {
		level++
	}
	if level == 0 {
		return 0, ""
	}
	return level, strings.TrimSpace(text[level:])
}

// decisionBlockAsks is each block of the decisions document that mentions an ask, with the asks it
// mentions: the block index's own record of what the document's text cites.
func decisionBlockAsks(ctx context.Context, q queryer, artifactID string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `
		select block_id, to_id from block_refs
		where artifact_id = $1::uuid and to_kind = 'ask' and kind = 'mentions'
		order by block_id, to_id
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	asks := map[string][]string{}
	for rows.Next() {
		var blockID, askID string
		if err := rows.Scan(&blockID, &askID); err != nil {
			return nil, err
		}
		asks[blockID] = append(asks[blockID], askID)
	}
	return asks, rows.Err()
}

// decisionKeeps is every decision of the document a person has kept, by heading block.
func decisionKeeps(ctx context.Context, q queryer, artifactID string) (map[string]model.DecisionKeep, error) {
	rows, err := q.Query(ctx, `
		select block_id, text_sha256, changed_version, kept_by, kept_at
		from decision_keeps where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keeps := map[string]model.DecisionKeep{}
	for rows.Next() {
		keep := model.DecisionKeep{ArtifactID: artifactID}
		if err := rows.Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion,
			&keep.KeptBy, &keep.KeptAt); err != nil {
			return nil, err
		}
		keeps[keep.BlockID] = keep
	}
	return keeps, rows.Err()
}

// keepDecisionInput is the body of POST /api/v1/artifacts/{id}/blocks/{block_id}/keep.
type keepDecisionInput struct {
	TextSHA256 string `json:"text_sha256" required:"true"`
}

// keepDecision is POST /api/v1/artifacts/{id}/blocks/{block_id}/keep {"text_sha256": "<sum>"}:
// the caller agrees with a decision, as the text they read it at. The block is an agent's
// decision, a `decision` block of any document of the issue, whose node the Keep acts on
// (keepDecisionNode); or, in an issue's decisions.md, the heading block a decision's section
// opens with, kept on the decision_keeps row alone as it always was. The sum is the
// `text_sha256` the read served them, and a decision whose text has changed since is 409
// DECISION_CHANGED with nothing written: a Keep over text nobody read is the one thing Keep
// exists to prevent, as `409 ASK_EDITED` holds an answer to the wording it was given on. Any other
// block - one that is neither, or a decision marked replaced, which is no longer the design - is
// 404 TARGET_NOT_FOUND. The route is the `person` scope's (routeScopes): a Keep is a person's.
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
	} else { // kept for one release: heading-keep
		// kept for one release: heading-keep
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

// keepDecisionHeading is the Keep of decision, a section of the issue's decisions.md opened by
// its heading block, in tx: the decision_keeps row of that block, which no node projects, and the
// decision.kept that records it.
func (s *server) keepDecisionHeading(ctx context.Context, tx pgx.Tx, actor model.Actor, document model.Artifact, decision model.Decision) (model.DecisionKeep, model.Event, error) {
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
	event, err := s.appendEvent(ctx, tx, issueOwner(*document.IssueKey).event("decision.kept", actor, map[string]any{
		"artifact_id":     document.ID,
		"block_id":        decision.BlockID,
		"changed_version": keep.ChangedVersion,
	}))
	return keep, event, err
}

// keepDecisionNode is the Keep of decision, an agent's decision node of document (readDecisionNode),
// in tx, as two appends on the node: a kept_by tuple to the person who
// kept it and a kept_text_sum value holding the text sum they read it at, with decision_keeps, the
// row the decision's block keys, as their projection, written in the same transaction. The Keep
// records that the decision has been reviewed and not who else did, so a kept_by to a person
// another Keep named is retracted, and the node holds one. The row's changed_version is the
// version that last changed the decision's text (readDecisionNode).
//
// It writes as the store's writers do: the issue's owner lock first, under which it reads the
// decision again and the node's current keepers, and then one write-phase batch (store.RunWrite),
// so the transaction holds its id only for that batch: the person's node (one the store already
// holds moves nothing), the projection row, the decision.kept event, and last the tuples, under
// that event (graphstore.QueueNodes, graphstore.QueueCaused over graphstore.LatestEvent). The
// event's changed_version is the one the row is written with, which no statement of the batch
// reads back.
//
// keepDecision read decision on the pool, holding no lock, and an agent's write of the document
// takes the same owner lock: one that committed while the Keep waited for it would leave the Keep
// recording a sum the decision no longer has, or keeping a decision now replaced. So the decision
// is read again under the lock, as keepDecisionHeading reads its section: one the store no longer
// holds, or holds replaced, is 404 TARGET_NOT_FOUND, and one whose text sum is no longer the one
// it was read at is 409 DECISION_CHANGED, each with nothing written.
func (s *server) keepDecisionNode(ctx context.Context, tx pgx.Tx, actor model.Actor, document model.Artifact, decision model.Decision) (model.DecisionKeep, model.Event, error) {
	own := issueOwner(*document.IssueKey)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	current, found, err := s.readDecisionNode(ctx, tx, actor, document, decision.BlockID)
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	if !found || current.Replaced {
		return model.DecisionKeep{}, model.Event{}, errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", decision.BlockID, document.Name)
	}
	if current.TextSHA256 != decision.TextSHA256 {
		return model.DecisionKeep{}, model.Event{}, errorf(http.StatusConflict, "DECISION_CHANGED",
			"the decision changed since it was read; read it again and keep what it says now")
	}
	decision = current
	login := model.CanonicalLogin(actor.ID)
	address := refs.BlockAddress(document.RefKey, decision.BlockID)
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
	held := graphstore.PlanUncaused([]graphstore.Node{graphstore.PersonNode(login, actor, at, graphstore.NoEvent)}, tuples)
	keep := model.DecisionKeep{ArtifactID: document.ID}
	var event model.Event
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, held)
		b.Queue("", `
			insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by, kept_at)
			values ($1::uuid, $2, $3, $4, $5, $6)
			on conflict (artifact_id, block_id) do update
				set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
					kept_by = excluded.kept_by, kept_at = excluded.kept_at
			returning block_id, text_sha256, changed_version, kept_by, kept_at
		`, document.ID, decision.BlockID, decision.TextSHA256, decision.ChangedVersion, login, at).QueryRow(func(row pgx.Row) error {
			return row.Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt)
		})
		if err := s.deps.Events.QueueAppend(b, own.event("decision.kept", actor, map[string]any{
			"artifact_id":     document.ID,
			"block_id":        decision.BlockID,
			"changed_version": decision.ChangedVersion,
		}), func(written model.Event) { event = written }); err != nil {
			return err
		}
		graphstore.QueueCaused(b, held, graphstore.LatestEvent)
		return nil
	}); err != nil {
		return model.DecisionKeep{}, model.Event{}, err
	}
	return keep, event, nil
}

// keptDecision is the decision Keep acts on at blockID of document, and whether it is a node's:
// an agent's decision node of a document of the issue (readDecisionNode), else the decision a
// heading block of the issue's decisions.md opens - that document resolved as the view resolves
// it, by slug and then by filename, so Keep and the view never disagree about which document
// holds the decisions. A decision marked replaced, a heading under `## Replaced` among them, is no
// decision Keep acts on.
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
	decisionsDocument, found, err := s.issueDecisionsDocument(ctx, *document.IssueKey)
	if err != nil {
		return model.Decision{}, false, err
	}
	if !found || decisionsDocument.ID != document.ID {
		return model.Decision{}, false, notFound
	}
	decisions, err := s.readDecisions(ctx, who, document, artifactReference(document), "", nil)
	if err != nil {
		return model.Decision{}, false, err
	}
	for _, decision := range decisions {
		if decision.BlockID == blockID && !decision.Replaced {
			return decision, false, nil
		}
	}
	return model.Decision{}, false, notFound
}

// readDecisionNode is the agent's decision whose node is block blockID of document, a document
// of an issue, as who reads it, and whether the store holds such a node: a live node of the
// `decision` supertag at that block's address. It is read through decisionread.Decisions, the one
// reader every read of a decision node answers from, so the Keep compares a posted sum with the
// sum the Decisions read served, and writes the version its words last changed in: its words
// are its own (refs.DecisionWords), and no reading it closes with moves its text, its sum or that
// version.
func (s *server) readDecisionNode(ctx context.Context, q queryer, who model.Actor, document model.Artifact, blockID string) (model.Decision, bool, error) {
	if document.IssueKey == nil || document.Kind != "doc" {
		return model.Decision{}, false, nil
	}
	decisions, err := decisionread.Decisions(ctx, q, who,
		[]decisionread.Node{{Kind: refs.BlockNodeKind, ID: refs.BlockAddress(document.RefKey, blockID)}})
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
// /api/v1/me/issues/{key}/state is, and announces nothing. The version is 0 or more and fits
// user_document_reads.version, an integer column (fitsIntegerColumn); anything else is refused
// 400 INVALID_STATE before the write.
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
	if input.Version == nil || *input.Version < 0 || !fitsIntegerColumn(*input.Version) {
		writeError(w, "INVALID_STATE", http.StatusBadRequest,
			fmt.Sprintf("version must be a whole number from 0 to %d", maxIntegerColumn))
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
