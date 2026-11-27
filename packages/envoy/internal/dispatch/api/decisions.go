package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The server's half of the Decisions view: the Decisions read, Keep, and each person's last read
// version of a document. A decision is one node of the store: an
// agent's is a `decision` block node of any document of the issue, and a person's is the answered
// ask's own node. Every read of a decision answers from those nodes, through decisionread, and no
// read answers from the text of an issue's decisions document (R11).

// issueDecisions is GET /api/v1/issues/{key}/decisions. entries is the read of the store
// alone: one entry for each node that holds a question the issue asked or a
// decision of any of its documents (decisionEntries). open_questions are the open asks its spec
// still holds. document and decisions are the members the release before answers, and they
// answer from the store (R11, ruling F4): decisions is each decision node and each answered ask
// in model.Decision's shape, in the order of entries, and document is the address of the issue's
// decisions document while it stands (decisions_documents()), null once it is retired or where
// the issue holds none. A link of an entry's shaped and of a decision's links is one value
// (model.DecisionLink): its stale is the release before's rule, which compares text sums - the
// decision's own words differ from those at the version the tie was written in and from those at
// the version that stood when the spec node last changed, as the page of the release before reads
// it too - and its changed_since_tie is R5's, the spec node's text changed after the tie.
// entries and open_questions are the route's own, and the whole route is kept for one release
// for a page of the release before (decisions-route): a page of this release reads the tree's
// decisions view, which answers the same nodes (R21 of the issue-tree spec). document and
// decisions go once no page of the release before reads them (compat-objects).
type issueDecisions struct {
	Entries []decisionEntry `json:"entries"`
	// kept for one release: compat-objects
	Document      *string       `json:"document"`
	OpenQuestions []issueAskRow `json:"open_questions"`
	// kept for one release: compat-objects
	Decisions []model.Decision `json:"decisions"`
}

// kept for one release: decisions-route
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
	// kept for one release: compat-objects
	if answer.Document, err = s.standingDecisionsDocument(r.Context(), key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, answer)
}

// standingDecisionsDocument is the dispatch:// address of issue key's decisions document while it
// stands (decisions_documents()), and nil once it is retired or where the issue holds none: the
// Decisions read's old `document` member. Nothing reads
// the document's text.
//
// kept for one release: compat-objects
func (s *server) standingDecisionsDocument(ctx context.Context, key string) (*string, error) {
	var issueKey, session *string
	var project, slug, kind string
	var primary bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary
		`+refs.DecisionsDocumentOf("$1"), key).Scan(&issueKey, &project, &session, &slug, &kind, &primary)
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

// specOpenQuestions are the issue's open asks whose block is in its spec (refs.AsksOfDocumentSQL),
// oldest first, in the shape every ask read has (askReadColumns, with the newest reply beside the
// row). An ask is open as its node holds it (asks.NodeAnswer.ApplyTo), which every read of an
// answer reads (R3), so the statement reads each of the spec's asks and the scan keeps the open
// ones. An ask's block is in the spec where the spec's block index holds an ask block under its
// block id (document_blocks, keyed as the ask is): a row that names the spec and a block it no
// longer holds - one a reopen restored with no block to write, which a later settlement retracts -
// is no question of it. An ask a stored value of which does not decode - one its node holds, or
// its row's author, options, anchor, answer, resolution or approval, or its newest reply's author
// (docs.UndecodableAskError) - is left out and logged with its id, as decisionEntries leaves out
// its entry, and the read goes on (R4, scanOpenQuestions); a failure of the query or of the scan, a
// column pgx cannot scan among them, fails the read.
//
// That index cannot say so while the spec owes it - while the spec is among the documents the
// coverage read names as owing their block index (queueOwedDocuments), read in the same batch: a
// version above the bound leaves its blocks to the block-index reconciliation, so an ask block it
// added is in no document_blocks row yet, and for a spec converted into its issue's room no row
// says yet which fragment holds the block (refs.AskBlockDocumentSQL). Then the spec's asks are the
// asks rows of the ask blocks its fragment holds as a read of the document reads it, out of the
// issue's room for a converted spec (docs.API.Blocks), found by the room and those block ids
// (refs.AsksOfFragmentBlocksSQL), whether or not the index holds them: a question a person can
// see does not drop out of the read because the index has not caught up, and no ask whose block
// the spec no longer holds is listed from an index that is behind. The block ids are all that read
// answers: it decodes no ask's answer, as the text routes' read (docs.API.TextWithAsks) does and
// fails on a node that holds a value that does not parse, so such an ask reaches the scan, which
// leaves it out. Once the index states the spec's version, its rows decide again. Either way one
// statement reads each ask once, so none is listed twice. reader is the answer's own, and each row
// registers its texts with it once the slice has stopped growing: a shaping registers a pointer
// into the row, and an append that moved the backing array would leave the reader filling trees
// into memory the answer no longer holds.
func (s *server) specOpenQuestions(
	ctx context.Context, who model.Actor, key, specID string, reader *displayTreeReader,
) ([]issueAskRow, error) {
	var owed []model.OwedDocument
	questions := []issueAskRow{}
	batch := &pgx.Batch{}
	queueOwedDocuments(batch, key, &owed)
	batch.Queue(`
		select `+askReadColumns+` `+askReadFrom+`
		where a.issue_key = $1 and `+refs.AsksOfDocumentSQL("a", "$2::uuid")+`
		  and exists (
			select 1 from document_blocks d
			where d.artifact_id = a.block_artifact_id and d.block_id = a.block_id and d.type = 'ask')
		order by a.created_at, a.id
	`, key, specID).Query(func(rows pgx.Rows) error {
		return scanOpenQuestions(rows, key, &questions)
	})
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, err
	}
	if slices.ContainsFunc(owed, func(document model.OwedDocument) bool { return document.ArtifactID == specID }) {
		held, err := s.deps.Docs.Blocks(ctx, specID)
		if err != nil {
			return nil, fmt.Errorf("read the ask blocks of the spec %s, which owes its block index: %w", specID, err)
		}
		blocks := make([]string, 0, len(held))
		for _, block := range held {
			if block.Type == "ask" && block.ID != "" {
				blocks = append(blocks, block.ID)
			}
		}
		rows, err := s.deps.Store.Pool.Query(ctx, `
			select `+askReadColumns+` `+askReadFrom+`
			where a.issue_key = $1 and `+refs.AsksOfFragmentBlocksSQL("a", "$2::uuid", "$3::text[]")+`
			order by a.created_at, a.id
		`, key, specID, blocks)
		if err != nil {
			return nil, err
		}
		questions = questions[:0]
		err = scanOpenQuestions(rows, key, &questions)
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	listed := make([]*model.Ask, len(questions))
	for index := range questions {
		listed[index] = &questions[index].Ask
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, listed); err != nil {
		return nil, err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, who, listed); err != nil {
		return nil, err
	}
	for index := range questions {
		questions[index].shape(reader)
	}
	return questions, nil
}

// scanOpenQuestions appends to questions each ask rows reads (askReadColumns) that is open as its
// node holds it, in the order rows reads them. An ask of issue key a stored value of which does
// not decode (scanAskRead's docs.UndecodableAskError) is left out and logged with its id, and the
// scan goes on; any other failure of the scan, a column pgx cannot scan among them, fails it.
func scanOpenQuestions(rows pgx.Rows, key string, questions *[]issueAskRow) error {
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if errors.Is(err, docs.ErrUndecodableAsk) {
			undecodable, _ := errors.AsType[*docs.UndecodableAskError](err)
			slog.Warn("dispatch: an ask whose stored value does not decode is left out of the issue's open questions",
				"ask", undecodable.AskID, "issue", key, "err", err)
			continue
		}
		if err != nil {
			return err
		}
		if ask.State == "open" {
			*questions = append(*questions, issueAskRow{Ask: ask, LastReply: reply})
		}
	}
	return rows.Err()
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
// conversion carries onto the section's decision node. The
// sum is the `text_sha256` the read served them, and a decision whose text has changed since is
// 409 DECISION_CHANGED with nothing written: a Keep over text nobody read is the one thing Keep
// exists to prevent, as `409 ASK_EDITED` holds an answer to the wording it was given on. Any other
// block - one that is neither, a heading of a retired decisions document, or a decision marked
// replaced, which is no longer the design - is 404 TARGET_NOT_FOUND. The route needs both the
// `person` and the `approve` scope (its route file's OAuthScopes): a Keep is a person's.
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
	marked := false
	if onNode {
		keep, event, marked, err = s.keepDecisionNode(r.Context(), tx, actor, document, decision)
	} else {
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
	published := []model.Event{event}
	if marked {
		published = append(published, s.deps.Docs.DrainOwedQueries(r.Context(), *document.IssueKey)...)
	}
	s.publish(r.Context(), published...)
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
// too; one whose section's text changed while it waited is 409
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
// holds moves nothing), the projection row, the decision.kept event, the tuples, under that event
// (graphstore.QueueNodes, graphstore.QueueCaused over graphstore.LatestEvent), and then the mark
// of the issue's queries owed (docs.QueueQueryMark): the decisions query takes the decision, and
// its record holds the kept_by and the kept_text_sum out of its node, so R14 owes that query a
// version, which the route drains once the batch has committed. marked says whether the issue has
// converted and took the mark. The event's changed_version is the one the row is written with,
// which no statement of the batch reads back.
//
// keepDecision read decision on the pool, holding no lock, and an agent's write of the document
// takes the same owner lock: one that committed while the Keep waited for it would leave the Keep
// recording a sum the decision no longer has, or keeping a decision now replaced. So the decision
// is read again under the lock, as keepDecisionHeading reads its section: one the store no longer
// holds, or holds replaced, is 404 TARGET_NOT_FOUND, and one whose text sum is no longer the one
// it was read at is 409 DECISION_CHANGED, each with nothing written.
func (s *server) keepDecisionNode(ctx context.Context, tx pgx.Tx, actor model.Actor, document model.Artifact, decision model.Decision) (model.DecisionKeep, model.Event, bool, error) {
	own := issueOwner(*document.IssueKey)
	if err := s.deps.Events.LockOwners(ctx, tx, own.event("", actor, nil)); err != nil {
		return model.DecisionKeep{}, model.Event{}, false, err
	}
	current, found, err := s.readDecisionNode(ctx, tx, actor, document, decision.BlockID)
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, false, err
	}
	if !found || current.Replaced {
		return model.DecisionKeep{}, model.Event{}, false, errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", decision.BlockID, document.Name)
	}
	if current.TextSHA256 != decision.TextSHA256 {
		return model.DecisionKeep{}, model.Event{}, false, errorf(http.StatusConflict, "DECISION_CHANGED",
			"the decision changed since it was read; read it again and keep what it says now")
	}
	decision = current
	login := model.CanonicalLogin(actor.ID)
	// The node readDecisionNode read the decision at under the lock (decisionNodeAddress).
	address := decision.NodeID
	// The tuple's time is the projection row's, to the microsecond a timestamptz holds.
	at := time.Now().UTC().Truncate(time.Microsecond)
	earlier, err := graphstore.CurrentOut(ctx, tx, refs.BlockNodeKind, address, []string{refs.TupleKeptBy})
	if err != nil {
		return model.DecisionKeep{}, model.Event{}, false, err
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
	marked := false
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
		docs.QueueQueryMark(b, *document.IssueKey, graphstore.LatestEvent, &marked)
		graphstore.QueueHeads(b, graphstore.HeldHeads(held))
		return nil
	}); err != nil {
		return model.DecisionKeep{}, model.Event{}, false, err
	}
	return keep, event, marked, nil
}

// keptDecision is the decision Keep acts on at blockID of document, and whether it is a node's:
// an agent's decision node of a document of the issue (readDecisionNode), else the section a
// heading block of the issue's decisions document opens while that document stands, read through
// the decisions step's section reader (docs' DecisionHeading over decisions_documents()).
// A decision marked replaced, a heading under `## Replaced`
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
// index keys it (refs.BlockAddress), read through q: under the document's ref_key for a document
// that is its own room, and for a document converted into its issue's room (model.Artifact.Room,
// refs.DocumentRoomSQL) under the ref_key of the room's tree, where the room's index records the
// block in this document's fragment (issue_room_blocks). A U+0000 reads as U+FFFD, as the
// rendering writes it. found is false for a block the room records in no fragment of document.
func decisionNodeAddress(ctx context.Context, q queryer, document model.Artifact, blockID string) (string, bool, error) {
	if document.Room == "" || document.Room == document.ID {
		return refs.BlockAddress(document.RefKey, blockID), true, nil
	}
	var key string
	err := q.QueryRow(ctx, `
		select room.ref_key from issue_room_blocks b join artifacts room on room.id = b.tree_artifact_id
		where b.tree_artifact_id = $1::uuid and block_id_key(b.block_id) = block_id_key($2::text) and b.block_id = $2
		  and b.document_id = $3::uuid
	`, document.Room, strings.ReplaceAll(blockID, "\x00", "\uFFFD"), document.ID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the node of block %q of %s: %w", blockID, document.ID, err)
	}
	return refs.BlockAddress(key, blockID), true, nil
}

// readDecisionNode is the agent's decision whose node is block blockID of document, a document
// of an issue, as who reads it, and whether the store holds such a node: a node of the `decision`
// supertag at that block's address (decisionNodeAddress), at any state but gone. It is read
// through decisionread.Decisions, the one reader every read of a decision node answers from, so
// the Keep compares a posted sum with the sum the Decisions read served, and writes the version
// its words last changed in: its words are its own (refs.DecisionWords), and no reading it closes
// with moves its text, its sum or that version. The answer names the node (NodeID), which the
// Keep appends on.
func (s *server) readDecisionNode(ctx context.Context, q queryer, who model.Actor, document model.Artifact, blockID string) (model.Decision, bool, error) {
	if document.IssueKey == nil || document.Kind != "doc" {
		return model.Decision{}, false, nil
	}
	address, found, err := decisionNodeAddress(ctx, q, document, blockID)
	if err != nil || !found {
		return model.Decision{}, false, err
	}
	decisions, err := decisionread.Decisions(ctx, q, who, []decisionread.Node{{Kind: refs.BlockNodeKind, ID: address}})
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
