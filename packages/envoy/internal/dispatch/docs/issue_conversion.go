package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The conversion of an issue into its room (R6 of this issue's spec): the three rooms an issue
// holds - its spec's, its verification-md's and its plan-md's - become the issue's one room, in
// one transaction for the issue, and never half of one. ConvertIssue is the one function that
// does it; the server's step (IssueRoomStep) and the command (`envoy-dispatch convert-issues`)
// both call it, each with a gate of its own (ConvertGate).
//
// The transaction holds the issue's owner lock, then the lock of each room it reads or writes:
// the three documents' and the new tree's. It reads each document's room at its stored head, after
// asking every process that holds one to store what it applied, renders each to markdown, and
// writes, at once and in this order, so each read of the transaction sees what it wrote before it
// (joinAtOnce, as the decisions step's restore of one issue writes):
//
//  1. the tree artifact, its node and its issue_trees row, the issue's row of issue_views saying the
//     conversion reached it (t19b, with the view mark where a child holds no requirement of its
//     own, R23), and the three documents' document_rooms rows, announced to every process
//     (AnnounceDocumentRooms), whose rooms of those documents close on the commit;
//  2. each document made a query node (graphstore.QueryNode), every role of the issue's
//     documents cleared, and the issue's decisions query node where it holds none;
//  3. the room's first update, one fragment for each document (encodeDocumentSeed), whose item
//     count the bound of R5 is held to before it is written, the tree's empty first version, and
//     a version of each document holding the text its room held;
//  4. one moved_to tuple from each block's old address to its node in the room, a block id the
//     room holds already in an earlier fragment taking a fresh id; each node's authors and Keeps
//     carried to its node in the room; and the asks whose blocks the documents hold moved into the
//     room;
//  5. the old index of each document retired (its blocks' nodes gone, their rows deleted), the
//     room indexed in one pass, and every pointer at a block that moved - a part_of, an
//     anchored_in, a prevented_by, a check result's pair, a refs row and a block_refs row - moved
//     to the block's node in the room;
//  6. for each document in the order verification-md, plan-md, spec (R21), the move of its open
//     approval request, the carry of its approval, its artifact.version, and what its block index
//     stated under that event; then the tree's own.
//
// The three old rooms keep every doc_updates, doc_snapshots and doc_checkpoints row they hold: no
// statement here touches those tables but the tree's own first update.

// ConversionActor is the writer the conversion names on the versions, the facts and the events it
// writes: a system actor of its own, as QueryDrainActor is a drain's. No person runs it.
var ConversionActor = model.Actor{Kind: "system", ID: "issue-conversion"}

// ConversionRollbackCost is what a rollback to a build before the issue room costs once an issue
// has converted (R6), which the command prints and the step logs.
const ConversionRollbackCost = "a rollback to a build before the issue room reads each converted document as it stood before its conversion, and loses every edit written into its issue's room since"

// ConversionOutcome is what the conversion did with one issue.
type ConversionOutcome string

// The three outcomes of an issue the conversion reaches without failing.
const (
	IssueConverted  ConversionOutcome = "converted"
	IssuePassedOver ConversionOutcome = "passed over"
	IssueDeferred   ConversionOutcome = "deferred"
)

// ConvertedIssue is what ConvertIssue answers of one issue: its outcome, the reason it was passed
// over or deferred, the document a deferral waits on, the item count of the room's first update
// where the conversion measured it (R5's bound), how long it took, and whether it was a dry run,
// whose transaction rolled back.
type ConvertedIssue struct {
	Issue    string
	Outcome  ConversionOutcome
	Reason   string
	Document string
	Items    uint64
	Took     time.Duration
	DryRun   bool
}

// ConvertGate runs first inside each issue's transaction, before any write: a ConvertRefusal ends
// that issue with nothing written and stops the caller's run.
type ConvertGate func(ctx context.Context, tx pgx.Tx) error

// ConvertRefusal is a gate's refusal, naming what it read.
type ConvertRefusal struct{ Reason string }

func (r *ConvertRefusal) Error() string { return "the conversion is refused: " + r.Reason }

// IssueRoomGate is the step's gate (R5, R6): every process processes.Live lists, read in tx on the
// database's clock, lists the issue room among its capabilities.
func IssueRoomGate(ctx context.Context, tx pgx.Tx) error {
	live, err := processes.Live(ctx, tx)
	if err != nil {
		return err
	}
	if blocker, blocked := withoutIssueRoom(live); blocked {
		return &ConvertRefusal{Reason: fmt.Sprintf("the live server process %s serves %s, which lists no %s",
			blocker.ID, blocker.Build(), processes.CapabilityIssueRoom)}
	}
	return nil
}

// withoutIssueRoom is the first of live that lists no issue room, and whether one does.
func withoutIssueRoom(live []processes.Process) (processes.Process, bool) {
	for _, process := range live {
		if !process.Lists(processes.CapabilityIssueRoom) {
			return process, true
		}
	}
	return processes.Process{}, false
}

// BuildGate is the command's gate (R6): the conversion runs against commit, the commit its binary
// was stamped with, only while every live process serves that commit and the newest
// server_build_announcements row names it. An unstamped binary is refused.
func BuildGate(commit string) ConvertGate {
	return func(ctx context.Context, tx pgx.Tx) error {
		if commit == "" {
			return &ConvertRefusal{Reason: `this envoy-dispatch carries no commit: build it with -ldflags "-X main.buildCommit=<commit>", the commit the conversion runs against`}
		}
		live, err := processes.Live(ctx, tx)
		if err != nil {
			return err
		}
		for _, process := range live {
			if process.Commit != commit {
				return &ConvertRefusal{Reason: fmt.Sprintf("the live server process %s serves %s, not commit %s, the one this conversion runs against",
					process.ID, process.Build(), commit)}
			}
		}
		var announced string
		err = tx.QueryRow(ctx, `select commit from server_build_announcements order by seq desc limit 1`).Scan(&announced)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return &ConvertRefusal{Reason: fmt.Sprintf("no server build announcement names a build yet; the conversion runs once the newest one names commit %s", commit)}
		case err != nil:
			return fmt.Errorf("read the newest server build announcement: %w", err)
		case announced != commit:
			return &ConvertRefusal{Reason: fmt.Sprintf("the newest server build announcement names commit %s, not commit %s, the one this conversion runs against",
				announced, commit)}
		}
		return nil
	}
}

// CheckConversionGate runs gate once in a read-only transaction of its own, so a run is refused
// while another build is live even where every issue it reaches is passed over before its gate.
func (s *Service) CheckConversionGate(ctx context.Context, gate ConvertGate) error {
	ctx = store.WithTransactionTracking(ctx)
	tx, err := s.store.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin the conversion's gate: %w", err)
	}
	defer tx.Rollback(context.Background())
	return gate(ctx, tx)
}

// ConvertProject converts the issues of project in key order, by number, each in a transaction of
// its own (ConvertIssue), and hands report each issue's answer and its failure. A gate's refusal
// stops the run and is its error; a failure of one issue is reported and the run goes on.
func (s *Service) ConvertProject(ctx context.Context, project string, gate ConvertGate, dryRun bool, report func(ConvertedIssue, error)) error {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, `select key from issues where project_key = $1 order by number, key`, project)
	if err != nil {
		return fmt.Errorf("list the issues of %s: %w", project, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the issues of %s: %w", project, err)
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := s.ConvertIssue(ctx, key, gate, dryRun)
		var refusal *ConvertRefusal
		if errors.As(err, &refusal) {
			return err
		}
		report(result, err)
	}
	return nil
}

// conversionIssueStatement reads what decides whether an issue converts: its status, its project,
// its parent, and whether it holds a tree already.
const conversionIssueStatement = `
	select i.status, i.project_key, coalesce(i.parent_key, ''),
	       exists (select 1 from issue_trees t where t.issue_key = i.key)
	from issues i where i.key = $1`

// conversionIssue is what conversionIssueStatement reads of an issue.
type conversionIssue struct {
	status, project, parent string
	converted               bool
}

// readConversionIssue reads issueKey through q (conversionIssueStatement).
func readConversionIssue(ctx context.Context, q Queryer, issueKey string) (conversionIssue, error) {
	var issue conversionIssue
	err := q.QueryRow(ctx, conversionIssueStatement, issueKey).Scan(&issue.status, &issue.project, &issue.parent, &issue.converted)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversionIssue{}, fmt.Errorf("the database holds no issue %s", issueKey)
	}
	if err != nil {
		return conversionIssue{}, fmt.Errorf("read the issue %s: %w", issueKey, err)
	}
	return issue, nil
}

// passedOver is the reason the conversion passes the issue over, and whether it does: a done
// issue converts not at all (R22), and an issue that holds a tree converted already - born
// converted, or converted by an earlier run.
func (issue conversionIssue) passedOver() (string, bool) {
	switch {
	case issue.status == "done":
		return "the issue is done", true
	case issue.converted:
		return "the issue holds its room already", true
	}
	return "", false
}

// conversionDocumentsStatement is the issue $1's documents the conversion takes into its room: its
// spec, its verification-md and its plan-md (the bands a query covers), not retired, in the order
// the room renders them (roomLayoutSQL).
var conversionDocumentsStatement = `
	select a.id::text, coalesce(a.ref_key, ''), a.slug, a.name, ` + refs.DocumentBandSQL("a") + `
	from artifacts a
	where a.issue_key = $1 and a.kind = 'doc' and a.retired_at is null
	  and ` + refs.DocumentBandSQL("a") + ` in ('` + string(refs.RankBandVerification) + `', '` +
	string(refs.RankBandPlan) + `', '` + string(refs.RankBandSpec) + `')
	order by ` + roomLayoutRankSQL(refs.DocumentBandSQL("a")) + `, a.created_at, a.id`

// conversionOwedStatement is one document of the issue $1 that still owes its block index, by
// either mark (document_block_versions' null version or store_owed_at), as the reconciliation
// lists a document: the conversion's census would find that document's blocks stale.
const conversionOwedStatement = `
	select a.id::text, coalesce(a.ref_key, ''), a.slug
	from artifacts a
	join document_block_versions b on b.artifact_id = a.id
	where a.issue_key = $1 and a.kind = 'doc' and a.retired_at is null
	  and (b.version is null or b.store_owed_at is not null)
	order by a.created_at, a.id
	limit 1`

// convertedDocument is one document the conversion takes into the issue's room.
type convertedDocument struct {
	id, refKey, slug, name, query string
	// tree is the document's room at its stored head, nil where the room holds no block, and
	// margin the room's margin records (marksMapName), which the issue's room takes.
	tree   *pmdoc.Node
	margin map[string]any
	// headMarkdown and headOffsets are the head's rendering as the room held it; markdown and
	// offsets the rendering once any block id the room holds already took a fresh one
	// (remint), whose offsets pair with the head's one for one.
	headMarkdown, markdown string
	headOffsets, offsets   []pmdoc.BlockOffset
	latest                 roomVersion
	version                model.Version
	versionNode, retired   graphstore.Uncaused
	renamed                map[string]string
}

// part is the document's fragment of the room as the room's pass reads it: its tree, or an empty
// document where the room holds no block.
func (d *convertedDocument) part() *pmdoc.Node {
	if d.tree == nil {
		return &pmdoc.Node{Type: "doc"}
	}
	return d.tree
}

// blockMove is one block of a converted document: its address in the document, its node in the
// room, and its type.
type blockMove struct {
	fromID, toID, from, to, typ string
}

// moves is each block of the document's head paired with its node in the room, the tree's ref_key
// tree, where both addresses fit the store's bound.
func (d *convertedDocument) moves(tree string) []blockMove {
	moves := make([]blockMove, 0, len(d.offsets))
	for index, offset := range d.offsets {
		move := blockMove{
			fromID: d.headOffsets[index].ID, toID: offset.ID, typ: offset.Type,
			from: refs.BlockAddress(d.refKey, d.headOffsets[index].ID), to: refs.BlockAddress(tree, offset.ID),
		}
		if graphstore.AddressFits(move.from) && graphstore.AddressFits(move.to) {
			moves = append(moves, move)
		}
	}
	return moves
}

// issueConversion is one issue's conversion, under the transaction tx that holds its locks.
type issueConversion struct {
	s               *Service
	tx              pgx.Tx
	issue           conversionIssue
	key             string
	at              time.Time
	tree, treeSlug  string
	treeRef         string
	documents       []*convertedDocument
	own             *pmdoc.Node
	ownMarkdown     string
	ownOffsets      []pmdoc.BlockOffset
	treeVersion     model.Version
	treeVersionNode graphstore.Uncaused
	index           roomIndex
}

// conversionStop is a conversion that ends with nothing written: an issue passed over or
// deferred, with the reason and the document a deferral waits on.
type conversionStop struct {
	outcome          ConversionOutcome
	reason, document string
}

// ConvertIssue converts the issue issueKey into its room in a transaction of its own, which it
// commits, or under dryRun rolls back once every write and the write phase's end have run, and
// answers what it did. A done issue, and one that holds its room already, is passed over before
// the gate runs, with nothing read but its row. Every document of the issue's room is flushed by
// every process that holds it first, holding no lock. Then, inside the transaction, gate runs
// before anything else: its refusal (ConvertRefusal) is answered as the error, with nothing
// written. Under the issue's owner lock the issue is read again, and it is deferred, with nothing
// written, while one of its documents owes its block index or a room's text reads otherwise than
// its latest version while it owes a settlement. A failure answers the error, with nothing
// written.
func (s *Service) ConvertIssue(ctx context.Context, issueKey string, gate ConvertGate, dryRun bool) (ConvertedIssue, error) {
	ctx = store.WithTransactionTracking(ctx)
	began := time.Now()
	result := ConvertedIssue{Issue: issueKey, DryRun: dryRun}
	answer := func(err error) (ConvertedIssue, error) {
		result.Took = time.Since(began)
		return result, err
	}
	issue, err := readConversionIssue(ctx, s.store.Pool, issueKey)
	if err != nil {
		return answer(err)
	}
	if reason, over := issue.passedOver(); over {
		result.Outcome, result.Reason = IssuePassedOver, reason
		return answer(nil)
	}
	documents, err := readConversionDocuments(ctx, s.store.Pool, issueKey)
	if err != nil {
		return answer(err)
	}
	for _, document := range documents {
		stop, err := s.flushConvertedRoom(ctx, document)
		if err != nil {
			return answer(err)
		}
		if stop != nil {
			result.Outcome, result.Reason, result.Document = stop.outcome, stop.reason, stop.document
			return answer(nil)
		}
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return answer(fmt.Errorf("begin the conversion of %s: %w", issueKey, err))
	}
	defer tx.Rollback(context.Background())
	joined, ledger := s.joinAtOnce(ctx, tx)
	defer ledger.Discard()
	if err := gate(joined, tx); err != nil {
		return answer(err)
	}
	conversion := &issueConversion{s: s, tx: tx, key: issueKey}
	stop, err := conversion.run(joined, &result)
	if err != nil {
		return answer(fmt.Errorf("the conversion of %s: %w", issueKey, err))
	}
	if stop != nil {
		result.Outcome, result.Reason, result.Document = stop.outcome, stop.reason, stop.document
		return answer(nil)
	}
	if dryRun {
		if err := ledger.runQueue(ctx, ledger.commitWrites); err != nil {
			return answer(fmt.Errorf("the write phase of %s: %w", issueKey, err))
		}
		if err := ledger.runAfterWrites(ctx); err != nil {
			return answer(fmt.Errorf("the write phase of %s: %w", issueKey, err))
		}
		if err := tx.Rollback(ctx); err != nil {
			return answer(fmt.Errorf("roll the dry run of %s back: %w", issueKey, err))
		}
	} else if err := ledger.Commit(ctx); err != nil {
		return answer(fmt.Errorf("commit the conversion of %s: %w", issueKey, err))
	}
	result.Outcome = IssueConverted
	return answer(nil)
}

// readConversionDocuments reads, through q, the documents of issueKey the conversion takes into its
// room (conversionDocumentsStatement), each with the query whose record covers it.
func readConversionDocuments(ctx context.Context, q Queryer, issueKey string) ([]*convertedDocument, error) {
	rows, err := q.Query(ctx, conversionDocumentsStatement, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read the documents of %s: %w", issueKey, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*convertedDocument, error) {
		var document convertedDocument
		var band string
		if err := row.Scan(&document.id, &document.refKey, &document.slug, &document.name, &band); err != nil {
			return nil, fmt.Errorf("read the documents of %s: %w", issueKey, err)
		}
		document.query = queryOfBand[band]
		return &document, nil
	})
}

// flushConvertedRoom has every process that holds a room of the document make every update that
// room applied durable before the conversion reads it from the store: every other process through
// a flush query (flushOtherRooms), and this process's own room, where it holds one, as a member
// answers that query (roomQueries.query). It holds no lock. This process's room that does not
// store its updates in the query's wait defers the issue: the store does not hold that room's
// text yet.
func (s *Service) flushConvertedRoom(ctx context.Context, document *convertedDocument) (*conversionStop, error) {
	if err := s.flushOtherRooms(ctx, s.store.Pool, document.id); err != nil {
		return nil, err
	}
	live := s.srv.GetDoc(document.id)
	if live == nil {
		return nil, nil
	}
	classes := s.unstoredSnapshot(document.id, live)
	done := s.askRoomAppends(document.id)
	defer done()
	flushed, cancel := context.WithTimeout(ctx, s.roomQueries.answerWait())
	defer cancel()
	if err := awaitStored(flushed, s.instanceOf(live), classes); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &conversionStop{outcome: IssueDeferred, document: document.id,
			reason: fmt.Sprintf("its document %s (%s, %s) holds edits its room has not stored yet: %v", document.slug, document.refKey, document.id, err)}, nil
	}
	return nil, nil
}

// run is the conversion inside its transaction, once the gate passed. It answers a stop where it
// writes nothing, and fills result's item count where it measured the room's first update.
func (c *issueConversion) run(ctx context.Context, result *ConvertedIssue) (*conversionStop, error) {
	if err := store.LockOwner(ctx, c.tx, store.IssueOwner(c.key)); err != nil {
		return nil, err
	}
	issue, err := readConversionIssue(ctx, c.tx, c.key)
	if err != nil {
		return nil, err
	}
	if reason, over := issue.passedOver(); over {
		return &conversionStop{outcome: IssuePassedOver, reason: reason}, nil
	}
	c.issue = issue
	var owedID, owedRef, owedSlug string
	switch err := c.tx.QueryRow(ctx, conversionOwedStatement, c.key).Scan(&owedID, &owedRef, &owedSlug); {
	case err == nil:
		return &conversionStop{outcome: IssueDeferred, document: owedID,
			reason: fmt.Sprintf("its document %s (%s, %s) owes its block index", owedSlug, owedRef, owedID)}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("read what the documents of %s owe: %w", c.key, err)
	}
	if c.documents, err = readConversionDocuments(ctx, c.tx, c.key); err != nil {
		return nil, err
	}
	if len(c.documents) == 0 {
		return &conversionStop{outcome: IssuePassedOver, reason: "the issue holds no spec, verification-md or plan-md"}, nil
	}
	if err := c.tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&c.tree); err != nil {
		return nil, fmt.Errorf("draw the id of the tree of %s: %w", c.key, err)
	}
	rooms := []string{c.tree}
	for _, document := range c.documents {
		rooms = append(rooms, document.id)
	}
	slices.Sort(rooms)
	for _, room := range rooms {
		if err := lockDocumentRoom(ctx, c.tx, room); err != nil {
			return nil, err
		}
	}
	if stop, err := c.readHeads(ctx); err != nil || stop != nil {
		return stop, err
	}
	if err := c.remint(); err != nil {
		return nil, err
	}
	seed, err := c.seed()
	if err != nil {
		return nil, err
	}
	items, counted := updateItems(seed)
	if !counted {
		return nil, fmt.Errorf("the first update of the room of %s names more than one client", c.key)
	}
	result.Items = items
	if items > maxUpdateItems {
		return &conversionStop{outcome: IssuePassedOver,
			reason: fmt.Sprintf("its room's first update would hold %d items, past the %d one update decodes", items, maxUpdateItems)}, nil
	}
	if c.at, err = readNow(ctx, c.tx); err != nil {
		return nil, err
	}
	for _, step := range []func(context.Context) error{
		c.writeTree, c.writeQueries,
		func(ctx context.Context) error { return c.writeSeed(ctx, seed) },
		c.writeVersions, c.writeMoves, c.moveAsks, c.retireDocuments, c.indexTheRoom, c.movePointers,
		c.finish,
	} {
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// readHeads reads each document's room at its stored head, under the room locks the transaction
// holds, so no append lands between this read and the commit, and renders it. A room or a version
// that does not read back passes the issue over with the reader's error; a room whose text reads
// otherwise than its latest version while it owes a settlement defers it, since that settlement
// is what versions the room's last edits and credits their authors.
func (c *issueConversion) readHeads(ctx context.Context) (*conversionStop, error) {
	ids := make([]string, len(c.documents))
	for index, document := range c.documents {
		ids[index] = document.id
	}
	latest, err := readRoomVersions(ctx, c.tx, ids)
	if err != nil {
		return nil, err
	}
	passOver := func(format string, args ...any) (*conversionStop, error) {
		return &conversionStop{outcome: IssuePassedOver, reason: fmt.Sprintf(format, args...)}, nil
	}
	for index, document := range c.documents {
		document.latest = latest[index]
		loaded, err := c.s.persistence.Load(ctx, document.id)
		if errors.Is(err, ErrDocumentUnloadable) {
			return passOver("the stored history of its document %s does not load: %v", document.slug, err)
		}
		if err != nil {
			return nil, fmt.Errorf("load the room of %s: %w", document.slug, err)
		}
		room, err := buildFork(crdt.NewClientID(), loaded.Update)
		if err != nil {
			return passOver("the stored history of its document %s does not decode: %v", document.slug, err)
		}
		if room.GetXmlFragment(fragmentName).Len() > 0 {
			valid, err := treeOfFragmentUnshared(room, fragmentName)
			if errors.Is(err, ErrDocOutsideSchema) || errors.Is(err, ErrDocNewerContent) {
				return passOver("its document %s does not read: %v", document.slug, err)
			}
			if err != nil {
				return nil, fmt.Errorf("read the room of %s: %w", document.slug, err)
			}
			document.tree = valid.Node()
		}
		document.margin = room.GetMap(marksMapName).Entries()
		if document.headMarkdown, document.headOffsets, err = renderFragment(document.part()); err != nil {
			return passOver("its document %s does not render: %v", document.slug, err)
		}
		if document.headMarkdown != "" {
			if _, err := pmdoc.ParseRendering(document.headMarkdown); err != nil {
				return passOver("the head of its document %s does not read back: %v", document.slug, err)
			}
		}
		if document.latest.markdown != "" {
			if _, err := pmdoc.ParseRendering(document.latest.markdown); err != nil {
				return passOver("version %d of its document %s does not read back: %v", document.latest.number, document.slug, err)
			}
		}
		if document.headMarkdown != document.latest.markdown {
			owed, err := settlementPending(ctx, c.tx, document.id)
			if err != nil {
				return nil, err
			}
			if owed {
				return &conversionStop{outcome: IssueDeferred, document: document.id,
					reason: fmt.Sprintf("its document %s (%s, %s) owes a settlement of its room, whose text reads otherwise than its version %d",
						document.slug, document.refKey, document.id, document.latest.number)}, nil
			}
		}
	}
	return nil, nil
}

// remint gives each block a fresh id where a fragment before its own in the room holds its id
// already (the tree's own fragment, then verification-md, plan-md and the spec), so no id names two
// blocks of the room, and renders each document whose ids moved again. The ids a document repeats
// within itself are its own, and stay.
func (c *issueConversion) remint() error {
	own, _, err := parseWrittenReplacing(nil, "")
	if err != nil {
		return fmt.Errorf("read the tree's own fragment: %w", err)
	}
	c.own = own
	if c.ownMarkdown, c.ownOffsets, err = renderFragment(own); err != nil {
		return fmt.Errorf("render the tree's own fragment: %w", err)
	}
	held := map[string]struct{}{}
	idOf := func(node *pmdoc.Node) string {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		return id
	}
	pmdoc.Walk(own, func(node *pmdoc.Node) bool {
		if id := idOf(node); id != "" {
			held[id] = struct{}{}
		}
		return true
	})
	for _, document := range c.documents {
		document.markdown, document.offsets = document.headMarkdown, document.headOffsets
		if document.tree == nil {
			continue
		}
		kept := map[string]struct{}{}
		renamed := false
		pmdoc.Walk(document.tree, func(node *pmdoc.Node) bool {
			id := idOf(node)
			if id == "" || node.Type == "doc" {
				return true
			}
			if _, taken := held[id]; taken {
				fresh := uuid.NewString()
				attrs := maps.Clone(node.Attrs)
				attrs[pmdoc.BlockIDAttr] = fresh
				node.Attrs = attrs
				id, renamed = fresh, true
			}
			kept[id] = struct{}{}
			return true
		})
		maps.Copy(held, kept)
		if !renamed {
			continue
		}
		markdown, offsets, err := renderFragment(document.tree)
		if err != nil {
			return fmt.Errorf("render %s with its fresh block ids: %w", document.slug, err)
		}
		if len(offsets) != len(document.headOffsets) {
			return fmt.Errorf("%s renders %d blocks with its fresh block ids and %d without", document.slug, len(offsets), len(document.headOffsets))
		}
		document.markdown, document.offsets = markdown, offsets
		document.renamed = map[string]string{}
		for index, offset := range offsets {
			if old := document.headOffsets[index].ID; old != offset.ID {
				document.renamed[old] = offset.ID
			}
		}
	}
	return nil
}

// seed is the room's first update: the tree's own fragment and each document's, in the room's
// order, with every document's margin records.
func (c *issueConversion) seed() ([]byte, error) {
	layout := c.layout()
	trees := []*pmdoc.Node{c.own}
	held := []*pmdoc.Node{nil}
	margin := map[string]any{}
	for _, document := range c.documents {
		trees = append(trees, document.tree)
		held = append(held, document.tree)
		maps.Copy(margin, document.margin)
	}
	seed, err := encodeDocumentSeed(layout.fragments(), trees, held, margin)
	if err != nil {
		return nil, fmt.Errorf("encode the first update of the room of %s: %w", c.key, err)
	}
	return seed, nil
}

// layout is the room's layout: its documents in the order the room renders them.
func (c *issueConversion) layout() roomLayout {
	layout := make(roomLayout, len(c.documents))
	for index, document := range c.documents {
		layout[index] = document.id
	}
	return layout
}

// ids is the ids of the converted documents, in the room's order.
func (c *issueConversion) ids() []string {
	return []string(c.layout())
}

// writeTree writes the tree artifact - its node with its part_of the issue, then its row - its
// issue_trees row, the issue's row of issue_views, and the documents' document_rooms rows naming
// the tree, announced to every process.
func (c *issueConversion) writeTree(ctx context.Context) error {
	actorJSON, err := json.Marshal(ConversionActor)
	if err != nil {
		return err
	}
	if c.treeSlug, err = freeIssueSlug(ctx, c.tx, c.key, issueTreeArtifactName); err != nil {
		return err
	}
	c.treeRef = graphstore.ArtifactRefKey(c.key, c.issue.project, "", c.treeSlug)
	at, err := graphstore.StatementTime(ctx, c.tx, graphstore.KindArtifact, c.treeRef)
	if err != nil {
		return err
	}
	node := graphstore.Created(graphstore.ArtifactNode(c.treeRef, c.issue.project, "", ConversionActor, at))
	treeTuples := []graphstore.Tuple{graphstore.ArtifactPartOf(c.treeRef, c.key, ConversionActor, at)}
	if err := graphstore.Write(ctx, c.tx, []graphstore.Node{node}, treeTuples); err != nil {
		return fmt.Errorf("write the node of the tree of %s: %w", c.key, err)
	}
	if err := writeExec(ctx, c.tx, "create the issue's tree", `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, node_kind, node_address)
		values ($1, $2, $3, $4, $5, 'tree', false, $6, $7, $8, $9)
	`, c.tree, c.key, c.issue.project, c.treeSlug, issueTreeArtifactName, actorJSON, at, graphstore.KindArtifact, c.treeRef); err != nil {
		return err
	}
	if err := writeExec(ctx, c.tx, "record the issue's tree", `insert into issue_trees (issue_key, artifact_id) values ($1, $2)`, c.key, c.tree); err != nil {
		return err
	}
	// The tree's head row, written once its artifacts row names it, as every other writer of a
	// node of an issue's tree writes its own.
	if err := graphstore.WriteHeads(ctx, c.tx, graphstore.HeadsOf([]graphstore.Node{node}, treeTuples)); err != nil {
		return err
	}
	// A child holding no requirement of its own is a view on its ancestor's spec (R23); the
	// mark an earlier write left on the row stays.
	viewMarked := false
	if c.issue.parent != "" {
		viewMarked = true
		for _, document := range c.documents {
			viewMarked = viewMarked && !holdsRequirement(document.tree)
		}
	}
	if err := writeExec(ctx, c.tx, "record how the issue was converted", `
		insert into issue_views (issue_key, converted, view_marked) values ($1, 't19b', $2)
		on conflict (issue_key) do update set converted = excluded.converted, view_marked = excluded.view_marked
	`, c.key, viewMarked); err != nil {
		return err
	}
	if err := writeExec(ctx, c.tx, "join the issue's documents to its room", `
		insert into document_rooms (artifact_id, room)
		select joined.document, $2::uuid from unnest($1::uuid[]) as joined (document)
	`, c.ids(), c.tree); err != nil {
		return err
	}
	return AnnounceDocumentRooms(ctx, c.tx, c.tree, c.ids()...)
}

// writeQueries makes each converted document a query node (graphstore.QueryNode), clears the role
// of every document of the issue (R16: a converted issue's rows hold none), and writes the issue's
// decisions query node where it holds none, with its empty first version, that version's node and
// its index, as an issue born converted is created with.
func (c *issueConversion) writeQueries(ctx context.Context) error {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	for _, document := range c.documents {
		at, err := graphstore.StatementTime(ctx, c.tx, graphstore.KindArtifact, document.refKey)
		if err != nil {
			return err
		}
		node := graphstore.QueryNode(document.refKey, c.issue.project, document.query, ConversionActor, at)
		nodes = append(nodes, node)
		tuples = graphstore.AttributeFacts(append(tuples, graphstore.ArtifactPartOf(document.refKey, c.key, ConversionActor, at)), node)
	}
	if err := graphstore.Write(ctx, c.tx, nodes, tuples); err != nil {
		return fmt.Errorf("make the documents of %s query nodes: %w", c.key, err)
	}
	if err := graphstore.WriteHeads(ctx, c.tx, graphstore.HeadsOf(nodes, tuples)); err != nil {
		return err
	}
	if err := writeExec(ctx, c.tx, "clear the roles of the issue's documents",
		`update artifacts set role = null where issue_key = $1 and role is not null`, c.key); err != nil {
		return err
	}
	var held bool
	if err := c.tx.QueryRow(ctx, `select exists (select 1 from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a")+`)`,
		c.key).Scan(&held); err != nil {
		return fmt.Errorf("read the decisions query of %s: %w", c.key, err)
	}
	if held {
		return nil
	}
	return c.writeDecisionsQuery(ctx)
}

// writeDecisionsQuery writes the issue's decisions query node and its row, a doc no fragment holds,
// its empty first version with that version's node, and the version's index, caused at the
// sentinel: no event records the conversion's own documents.
func (c *issueConversion) writeDecisionsQuery(ctx context.Context) error {
	actorJSON, err := json.Marshal(ConversionActor)
	if err != nil {
		return err
	}
	var id string
	if err := c.tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		return fmt.Errorf("draw the id of the decisions query of %s: %w", c.key, err)
	}
	slug, err := freeIssueSlug(ctx, c.tx, c.key, decisionsQueryName)
	if err != nil {
		return err
	}
	refKey := graphstore.ArtifactRefKey(c.key, c.issue.project, "", slug)
	at, err := graphstore.StatementTime(ctx, c.tx, graphstore.KindArtifact, refKey)
	if err != nil {
		return err
	}
	node := graphstore.Created(graphstore.QueryNode(refKey, c.issue.project, graphstore.QueryDecisions, ConversionActor, at))
	queryTuples := graphstore.AttributeFacts([]graphstore.Tuple{graphstore.ArtifactPartOf(refKey, c.key, ConversionActor, at)}, node)
	if err := graphstore.Write(ctx, c.tx, []graphstore.Node{node}, queryTuples); err != nil {
		return fmt.Errorf("write the decisions query of %s: %w", c.key, err)
	}
	if err := writeExec(ctx, c.tx, "create the decisions query", `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, node_kind, node_address)
		values ($1, $2, $3, $4, $5, 'doc', false, $6, $7, $8, $9)
	`, id, c.key, c.issue.project, slug, decisionsQueryName, actorJSON, at, graphstore.KindArtifact, refKey); err != nil {
		return err
	}
	version, err := queueVersionInsert(ctx, c.tx, id, "", []model.Actor{ConversionActor}, false, nil, nil, RenderStamp{})
	if err != nil {
		return err
	}
	owner, err := ReadVersionOwner(ctx, c.tx, id)
	if err != nil {
		return err
	}
	versionNode, err := WriteVersionNode(ctx, c.tx, owner, version.Number, ConversionActor, version.CreatedAt)
	if err != nil {
		return err
	}
	versions := refs.OneVersion(id, version.Number, version.CreatedAt)
	_, indexed, err := c.s.indexDocument(ctx, c.tx, id, versions, "", renderedTree{tree: &pmdoc.Node{Type: "doc"}}, nil)
	if err != nil {
		return fmt.Errorf("index the decisions query of %s: %w", c.key, err)
	}
	stated := graphstore.Joined(versionNode, indexed)
	if err := graphstore.Caused(ctx, c.tx, stated, graphstore.NoEvent); err != nil {
		return err
	}
	// The query's head row, written once its artifacts row names it, and those of what its
	// version states.
	return graphstore.WriteHeads(ctx, c.tx, append(graphstore.HeadsOf([]graphstore.Node{node}, queryTuples), graphstore.HeldHeads(stated)...))
}

// writeSeed appends the room's first update, its version 1, under the tree's own key.
func (c *issueConversion) writeSeed(ctx context.Context, seed []byte) error {
	if _, err := c.s.persistence.AppendUpdateTx(ctx, c.tx, c.tree, seed, true); err != nil {
		return fmt.Errorf("write the first update of the room of %s: %w", c.key, err)
	}
	return nil
}

// writeVersions writes the tree's first version, its own fragment's text, and a version of each
// document holding the text its room held, each with its node, a query version for each document
// (which writeQueries made a query node).
func (c *issueConversion) writeVersions(ctx context.Context) error {
	var err error
	if c.treeVersion, c.treeVersionNode, err = c.writeVersion(ctx, c.tree, c.ownMarkdown); err != nil {
		return err
	}
	for _, document := range c.documents {
		if document.version, document.versionNode, err = c.writeVersion(ctx, document.id, document.markdown); err != nil {
			return err
		}
	}
	return nil
}

// writeVersion writes one version of artifactID holding markdown, by the conversion, and its node.
func (c *issueConversion) writeVersion(ctx context.Context, artifactID, markdown string) (model.Version, graphstore.Uncaused, error) {
	version, err := queueVersionInsert(ctx, c.tx, artifactID, markdown, []model.Actor{ConversionActor}, false, nil, nil, RenderStamp{})
	if err != nil {
		return model.Version{}, graphstore.Uncaused{}, err
	}
	collectVersionText(ctx, artifactID, markdown)
	owner, err := ReadVersionOwner(ctx, c.tx, artifactID)
	if err != nil {
		return model.Version{}, graphstore.Uncaused{}, err
	}
	node, err := WriteVersionNode(ctx, c.tx, owner, version.Number, ConversionActor, version.CreatedAt)
	if err != nil {
		return model.Version{}, graphstore.Uncaused{}, err
	}
	return version, node, nil
}

// conversionHeldOutStatement is every tuple of the kinds $2 that the block nodes $1 assert now,
// the newest of each relation - and of each attribute, whatever its value - in the order they
// were appended: what the conversion carries from a block's old node to its node in the room.
var conversionHeldOutStatement = `
	select held.from_id, held.kind, held.to_kind, held.to_id, held.at, held.who
	from (` + graphstore.CurrentTupleRead(
	`t.from_id, t.kind, (case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_kind || ':' || t.to_id end)`,
	`t.from_id, t.kind, t.to_kind, t.to_id, t.at, t.who, t.asserted, t.seq`,
	`t.from_kind = '`+refs.BlockNodeKind+`' and t.from_id = any($1::text[]) and t.kind = any($2::text[])`) + `) held
	where held.asserted
	order by held.seq`

// conversionFirstAuthorsStatement is, of each block $2 of the document $1, the authors of the
// version that first held it, where Dispatch knows that version: the lowest version
// document_block_texts records a text of the block in, which is version 1 or one whose version
// before a block-aware build indexed, as the reader of a block's maker reads it (refs'
// blockFirstVersion).
const conversionFirstAuthorsStatement = `
	select block.id, first.authors
	from unnest($2::text[]) as block (id)
	cross join lateral (
		select v.authors,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = t.artifact_id and i.version = t.version - 1
		       ) as known
		from document_block_texts t
		join artifact_versions v on v.artifact_id = t.artifact_id and v.number = t.version
		where t.artifact_id = $1::uuid and t.block_id = block.id and t.text is not null
		order by t.version limit 1
	) first
	where first.known`

// heldTuple is one tuple conversionHeldOutStatement reads.
type heldTuple struct {
	from, kind, toKind, toID string
	at                       time.Time
	who                      model.Actor
}

// writeMoves states, before the room's pass, what the pass reads of each block's node in the room:
// one moved_to tuple from each block's old address to that node, so the pass resolves a pointer
// at the old address there; the node's authors, its old node's authored_by in their order, or
// where that states none the authors of the version that first held the block, so the pass, which
// states authors only for a node that states none, keeps the authors Dispatch showed before the
// move (R22); and the old node's Keep, its kept_by and its kept_text_sum. A decision's and a
// reading's authors are their typed pass's, which states them from their words.
func (c *issueConversion) writeMoves(ctx context.Context) error {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	made := map[graphstore.NodeKey]bool{}
	aliased := map[graphstore.NodeKey]bool{}
	for _, document := range c.documents {
		moves := document.moves(c.treeRef)
		if len(moves) == 0 {
			continue
		}
		from := make([]string, len(moves))
		for index, move := range moves {
			from[index] = move.from
		}
		held, err := c.readHeldOut(ctx, from, []string{graphstore.TupleAuthoredBy, refs.TupleKeptBy, refs.ValueKeptTextSum})
		if err != nil {
			return err
		}
		authored := map[string]bool{}
		for _, tuple := range held {
			authored[tuple.from] = authored[tuple.from] || tuple.kind == graphstore.TupleAuthoredBy
		}
		var unauthored []string
		for _, move := range moves {
			if !authored[move.from] && move.typ != decisionType && move.typ != readingType {
				unauthored = append(unauthored, move.fromID)
			}
		}
		first, err := c.readFirstAuthors(ctx, document.id, unauthored)
		if err != nil {
			return err
		}
		to := make(map[string]string, len(moves))
		for _, move := range moves {
			to[move.from] = move.to
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: move.from, Kind: graphstore.TupleMovedTo,
				ToKind: refs.BlockNodeKind, ToID: move.to, At: c.at, Who: ConversionActor, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
			if move.typ == decisionType || move.typ == readingType || authored[move.from] {
				continue
			}
			for _, author := range first[move.fromID] {
				authorNodes, facts, named := graphstore.AuthorFacts(refs.BlockNodeKind, move.to, author, c.at, graphstore.NoEvent)
				if !named {
					continue
				}
				tuples = append(tuples, facts[0])
				key := graphstore.NodeKey{Kind: authorNodes[0].Kind, ID: authorNodes[0].ID}
				if !aliased[key] {
					aliased[key] = true
					tuples = append(tuples, facts[1:]...)
				}
				for _, node := range authorNodes {
					if key := (graphstore.NodeKey{Kind: node.Kind, ID: node.ID}); !made[key] {
						made[key] = true
						nodes = append(nodes, node)
					}
				}
			}
		}
		for _, tuple := range held {
			if tuple.kind == graphstore.TupleAuthoredBy && (documentMoveType(moves, tuple.from) == decisionType || documentMoveType(moves, tuple.from) == readingType) {
				continue
			}
			tuples = append(tuples, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: to[tuple.from], Kind: tuple.kind, ToKind: tuple.toKind, ToID: tuple.toID,
				At: tuple.at, Who: tuple.who, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
		}
	}
	if len(nodes)+len(tuples) == 0 {
		return nil
	}
	if err := graphstore.Write(ctx, c.tx, nodes, tuples); err != nil {
		return fmt.Errorf("state the moves of the blocks of %s: %w", c.key, err)
	}
	return graphstore.WriteHeads(ctx, c.tx, graphstore.HeadsOf(nodes, tuples))
}

// documentMoveType is the type of the block whose old address is from among moves, "" for none.
func documentMoveType(moves []blockMove, from string) string {
	for _, move := range moves {
		if move.from == from {
			return move.typ
		}
	}
	return ""
}

// readHeldOut reads the tuples of kinds the block nodes from assert now
// (conversionHeldOutStatement).
func (c *issueConversion) readHeldOut(ctx context.Context, from, kinds []string) ([]heldTuple, error) {
	rows, err := c.tx.Query(ctx, conversionHeldOutStatement, from, kinds)
	if err != nil {
		return nil, fmt.Errorf("read what the blocks of %s state: %w", c.key, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (heldTuple, error) {
		var tuple heldTuple
		var who []byte
		if err := row.Scan(&tuple.from, &tuple.kind, &tuple.toKind, &tuple.toID, &tuple.at, &who); err != nil {
			return heldTuple{}, err
		}
		if err := json.Unmarshal(who, &tuple.who); err != nil {
			return heldTuple{}, fmt.Errorf("decode who stated %s of %s: %w", tuple.kind, tuple.from, err)
		}
		return tuple, nil
	})
}

// readFirstAuthors reads, of each of blocks of the document artifactID, the authors of the version
// that first held it (conversionFirstAuthorsStatement), by block id.
func (c *issueConversion) readFirstAuthors(ctx context.Context, artifactID string, blocks []string) (map[string][]model.Actor, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	rows, err := c.tx.Query(ctx, conversionFirstAuthorsStatement, artifactID, blocks)
	if err != nil {
		return nil, fmt.Errorf("read who wrote the first versions of the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	first := map[string][]model.Actor{}
	for rows.Next() {
		var block string
		var raw []byte
		if err := rows.Scan(&block, &raw); err != nil {
			return nil, fmt.Errorf("read who wrote the first version of a block of %s: %w", artifactID, err)
		}
		var authors []model.Actor
		if err := json.Unmarshal(raw, &authors); err != nil {
			return nil, fmt.Errorf("decode who wrote the first version of block %s of %s: %w", block, artifactID, err)
		}
		first[block] = authors
	}
	return first, rows.Err()
}

// renamedPairs is each block id a document's conversion gave a fresh one, as three arrays a
// statement binds: the document, the old id and the new one.
func (c *issueConversion) renamedPairs() (documents, old, fresh []string) {
	for _, document := range c.documents {
		for _, id := range slices.Sorted(maps.Keys(document.renamed)) {
			documents, old, fresh = append(documents, document.id), append(old, id), append(fresh, document.renamed[id])
		}
	}
	return documents, old, fresh
}

// moveAsks moves into the room every ask whose block a converted document holds: its row names
// the tree as the document holding its block (asks of an issue room are its tree's), under the
// block's id in the room, and its node's relations to documents are claimed again there
// (graphstore.AskClaims); the authors of those asks' blocks are recorded under the room as well,
// and the anchor of an ask or a comment whose block took a fresh id names that id.
func (c *issueConversion) moveAsks(ctx context.Context) error {
	documents, old, fresh := c.renamedPairs()
	rows, err := c.tx.Query(ctx, `
		select k.id::text, k.artifact_id::text, coalesce(moved.fresh, k.block_id)
		from asks k
		left join unnest($2::text[], $3::text[], $4::text[]) as moved (document, old, fresh)
		  on moved.document = k.block_artifact_id::text and moved.old = k.block_id
		where k.block_artifact_id = any($1::uuid[])
		order by k.id
	`, c.ids(), documents, old, fresh)
	if err != nil {
		return fmt.Errorf("read the asks of the documents of %s: %w", c.key, err)
	}
	type movedAsk struct {
		id    string
		owner *string
		block string
	}
	asks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (movedAsk, error) {
		var ask movedAsk
		err := row.Scan(&ask.id, &ask.owner, &ask.block)
		return ask, err
	})
	if err != nil {
		return fmt.Errorf("read the asks of the documents of %s: %w", c.key, err)
	}
	if len(asks) > 0 {
		ids, blocks := make([]string, len(asks)), make([]string, len(asks))
		var claims []graphstore.Claim
		for index, ask := range asks {
			ids[index], blocks[index] = ask.id, ask.block
			places, err := graphstore.ReadAskPlaces(ctx, c.tx, &c.tree, ask.owner, &c.tree)
			if err != nil {
				return err
			}
			claims = append(claims, graphstore.AskClaims(ask.id, places, ConversionActor, c.at, graphstore.NoEvent)...)
		}
		if err := writeExec(ctx, c.tx, "move the asks of the issue's documents into its room", `
			update asks k set block_artifact_id = $1::uuid, block_id = moved.block
			from unnest($2::uuid[], $3::text[]) as moved (id, block)
			where k.id = moved.id
		`, c.tree, ids, blocks); err != nil {
			return err
		}
		arms, params, err := graphstore.WriteArms(1, nil, nil, claims)
		if err != nil {
			return err
		}
		if _, err := c.tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
			return fmt.Errorf("claim the moved asks of %s in its room: %w", c.key, err)
		}
		if err := graphstore.WriteHeads(ctx, c.tx, graphstore.HeadsOfClaims(claims)); err != nil {
			return err
		}
	}
	if err := writeExec(ctx, c.tx, "record the authors of the issue's ask blocks under its room", `
		insert into doc_ask_block_authors (artifact_id, block_id, actor, recorded_at)
		select $1::uuid, coalesce(moved.fresh, a.block_id), a.actor, a.recorded_at
		from doc_ask_block_authors a
		left join unnest($3::text[], $4::text[], $5::text[]) as moved (document, old, fresh)
		  on moved.document = a.artifact_id::text and moved.old = a.block_id
		where a.artifact_id = any($2::uuid[])
		on conflict (artifact_id, block_id_key(block_id)) do nothing
	`, c.tree, c.ids(), documents, old, fresh); err != nil {
		return err
	}
	if len(old) == 0 {
		return nil
	}
	for _, table := range []string{"asks", "comments"} {
		if err := writeExec(ctx, c.tx, "name the fresh block id in the anchors of the "+table+" of the issue's documents", `
			update `+table+` held set anchor = jsonb_set(held.anchor, '{block_id}', to_jsonb(moved.fresh))
			from unnest($1::text[], $2::text[], $3::text[]) as moved (document, old, fresh)
			where held.anchor->>'artifact_id' = moved.document and held.anchor->>'block_id' = moved.old
		`, documents, old, fresh); err != nil {
			return err
		}
	}
	return nil
}

// retireDocuments retires each converted document's own block index: a pass over its latest
// version holding no block, which states each block's node gone with its pointers, deletes its
// rows and reconciles the document's own references to none (indexRenderedVersion), so no walk
// reaches a block at both its old address and its node in the room. What each pass states is
// caused with that document's version (finish).
func (c *issueConversion) retireDocuments(ctx context.Context) error {
	for _, document := range c.documents {
		if document.latest.number == 0 {
			continue
		}
		versions := refs.OneVersion(document.id, document.latest.number, document.latest.at)
		_, retired, err := c.s.indexRenderedVersion(ctx, c.tx, document.id, versions, nil, false, "", nil, &pmdoc.Node{Type: "doc"}, nil)
		if err != nil {
			return fmt.Errorf("retire the block index of %s: %w", document.slug, err)
		}
		document.retired = retired
	}
	return nil
}

// indexTheRoom runs the room's one pass under its tree (indexRoom), over every fragment at the
// version the conversion wrote of it, each fragment's own rendering laid end to end in the room's
// order (fragmentsText).
func (c *issueConversion) indexTheRoom(ctx context.Context) error {
	versions := refs.Versions{c.tree: {Number: c.treeVersion.Number, At: c.treeVersion.CreatedAt}}
	parts := []*pmdoc.Node{c.own}
	markdowns := []string{c.ownMarkdown}
	offsets := [][]pmdoc.BlockOffset{c.ownOffsets}
	for _, document := range c.documents {
		versions[document.id] = refs.Version{Number: document.version.Number, At: document.version.CreatedAt}
		parts = append(parts, document.part())
		markdowns = append(markdowns, document.markdown)
		offsets = append(offsets, document.offsets)
	}
	joined, counts := joinFragments(parts)
	markdown, laid := fragmentsText(markdowns, offsets)
	blocks := offsetBlocks(laid, c.layout().blockDocuments(c.tree, joined, counts))
	index, err := c.s.indexRoom(ctx, c.tx, c.tree, versions, versions, false, markdown, blocks, joined, nil)
	if err != nil {
		return fmt.Errorf("index the room of %s: %w", c.key, err)
	}
	c.index = index
	return nil
}

// conversionPointersStatement is every tuple of the kinds $2 at one of the block nodes $1 that
// stands now, out of a node that is no block of $1 and no result: the place of a node under a
// block that moved, a comment's anchor block and a risk's prevention, each moved to the block's
// node in the room (movePointers). A result's are its row's, moved with it.
var conversionPointersStatement = `
	select held.from_kind, held.from_id, held.kind, held.to_id
	from (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_id`, `t.from_kind, t.from_id, t.kind, t.to_id, t.asserted`,
	`t.to_kind = '`+refs.BlockNodeKind+`' and t.to_id = any($1::text[]) and t.kind = any($2::text[])`) + `) held
	where held.asserted and held.from_kind <> '` + graphstore.KindResult + `'
	  and not (held.from_kind = '` + refs.BlockNodeKind + `' and held.from_id = any($1::text[]))`

// movePointers moves, after the room's pass, every structured pointer at a block that moved to the
// block's node in the room (R13): each pointer tuple the store holds at the old address
// (conversionPointersStatement), retracted there and stated at the node; each check result's row,
// whose pair names the tree and the block's id in the room, with its result_of and its part_of
// claimed there; each refs row of a document citing the block, and each block_refs row of a block
// of another document pointing at it, rewritten to the node, with the tuple each projects
// retracted at the old address and stated at the node.
func (c *issueConversion) movePointers(ctx context.Context) error {
	to := map[string]string{}
	var old, fresh []string
	for _, document := range c.documents {
		for _, move := range document.moves(c.treeRef) {
			to[move.from] = move.to
			old, fresh = append(old, move.from), append(fresh, move.to)
		}
	}
	if len(old) == 0 {
		return nil
	}
	var tuples []graphstore.Tuple
	pointer := func(fromKind, fromID, kind, from string) {
		tuples = append(tuples,
			graphstore.Tuple{FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: refs.BlockNodeKind, ToID: from,
				At: c.at, Who: ConversionActor, SourceEvent: graphstore.NoEvent, Asserted: false},
			graphstore.Tuple{FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: refs.BlockNodeKind, ToID: to[from],
				At: c.at, Who: ConversionActor, SourceEvent: graphstore.NoEvent, Asserted: true})
	}
	rows, err := c.tx.Query(ctx, conversionPointersStatement, old,
		[]string{graphstore.TuplePartOf, graphstore.TupleAnchoredIn, graphstore.TuplePreventedBy})
	if err != nil {
		return fmt.Errorf("read the pointers at the blocks of %s: %w", c.key, err)
	}
	for rows.Next() {
		var fromKind, fromID, kind, from string
		if err := rows.Scan(&fromKind, &fromID, &kind, &from); err != nil {
			rows.Close()
			return fmt.Errorf("read a pointer at a block of %s: %w", c.key, err)
		}
		pointer(fromKind, fromID, kind, from)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the pointers at the blocks of %s: %w", c.key, err)
	}
	claims, err := c.moveResults(ctx)
	if err != nil {
		return err
	}
	refsRows, err := c.tx.Query(ctx, `
		select coalesce(a.node_kind, ''), coalesce(a.node_address, ''), r.to_id
		from refs r left join artifacts a on a.id::text = r.from_id
		where r.from_kind = '`+graphstore.KindArtifact+`' and r.to_kind = '`+refs.BlockNodeKind+`' and r.to_id = any($1::text[])
	`, old)
	if err != nil {
		return fmt.Errorf("read the references at the blocks of %s: %w", c.key, err)
	}
	for refsRows.Next() {
		var nodeKind, nodeAddress, from string
		if err := refsRows.Scan(&nodeKind, &nodeAddress, &from); err != nil {
			refsRows.Close()
			return fmt.Errorf("read a reference at a block of %s: %w", c.key, err)
		}
		if nodeKind != "" && nodeAddress != "" {
			pointer(nodeKind, nodeAddress, graphstore.TupleMentions, from)
		}
	}
	if err := refsRows.Err(); err != nil {
		return fmt.Errorf("read the references at the blocks of %s: %w", c.key, err)
	}
	linkRows, err := c.tx.Query(ctx, `
		select r.from_node, r.kind, coalesce(r.node_kind, ''), coalesce(r.node_address, ''), r.to_id
		from block_refs r
		where r.to_kind = '`+refs.BlockNodeKind+`' and r.to_id = any($1::text[]) and r.artifact_id <> all($2::uuid[])
	`, old, c.ids())
	if err != nil {
		return fmt.Errorf("read the links at the blocks of %s: %w", c.key, err)
	}
	for linkRows.Next() {
		var fromNode, kind, nodeKind, nodeAddress, from string
		if err := linkRows.Scan(&fromNode, &kind, &nodeKind, &nodeAddress, &from); err != nil {
			linkRows.Close()
			return fmt.Errorf("read a link at a block of %s: %w", c.key, err)
		}
		if nodeKind != "" && nodeAddress != "" {
			pointer(nodeKind, nodeAddress, kind, from)
		} else {
			pointer(refs.BlockNodeKind, fromNode, kind, from)
		}
	}
	if err := linkRows.Err(); err != nil {
		return fmt.Errorf("read the links at the blocks of %s: %w", c.key, err)
	}
	for _, statement := range []struct{ what, sql string }{
		{"point the references at the moved blocks of the issue at their nodes in its room", `
			update refs r set to_id = moved.fresh
			from unnest($1::text[], $2::text[]) as moved (old, fresh)
			where r.from_kind = '` + graphstore.KindArtifact + `' and r.to_kind = '` + refs.BlockNodeKind + `' and r.to_id = moved.old
			  and not exists (
				select 1 from refs held
				where held.from_kind = r.from_kind and held.from_id = r.from_id and held.to_kind = r.to_kind and held.to_id = moved.fresh)`},
		{"delete the references the issue's room already holds at the moved blocks", `
			delete from refs r using unnest($1::text[], $2::text[]) as moved (old, fresh)
			where r.from_kind = '` + graphstore.KindArtifact + `' and r.to_kind = '` + refs.BlockNodeKind + `' and r.to_id = moved.old`},
	} {
		if err := writeExec(ctx, c.tx, statement.what, statement.sql, old, fresh); err != nil {
			return err
		}
	}
	for _, statement := range []struct{ what, sql string }{
		{"point the links at the moved blocks of the issue at their nodes in its room", `
			update block_refs r set to_id = moved.fresh
			from unnest($1::text[], $2::text[]) as moved (old, fresh)
			where r.to_kind = '` + refs.BlockNodeKind + `' and r.to_id = moved.old and r.artifact_id <> all($3::uuid[])
			  and not exists (
				select 1 from block_refs held
				where held.artifact_id = r.artifact_id and held.block_id = r.block_id and held.kind = r.kind
				  and held.to_kind = r.to_kind and held.to_id = moved.fresh)`},
		{"delete the links a block already holds at the moved blocks' nodes", `
			delete from block_refs r using unnest($1::text[], $2::text[]) as moved (old, fresh)
			where r.to_kind = '` + refs.BlockNodeKind + `' and r.to_id = moved.old and r.artifact_id <> all($3::uuid[])`},
	} {
		if err := writeExec(ctx, c.tx, statement.what, statement.sql, old, fresh, c.ids()); err != nil {
			return err
		}
	}
	if len(tuples) > 0 {
		if err := graphstore.Write(ctx, c.tx, nil, tuples); err != nil {
			return fmt.Errorf("move the pointers at the blocks of %s: %w", c.key, err)
		}
	}
	if len(claims) > 0 {
		arms, params, err := graphstore.WriteArms(1, nil, nil, claims)
		if err != nil {
			return err
		}
		if _, err := c.tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
			return fmt.Errorf("claim the moved results of %s at their checks: %w", c.key, err)
		}
		if err := graphstore.WriteHeads(ctx, c.tx, graphstore.HeadsOfClaims(claims)); err != nil {
			return err
		}
	}
	return writeExec(ctx, c.tx, "clear the settlements the issue's old rooms owe, whose text their versions hold",
		`delete from doc_settlements_pending where artifact_id = any($1::uuid[])`, c.ids())
}

// moveResults moves each check result recorded against a check of a converted document to that
// check's node in the room: its row's pair names the tree and the check's id in the room, and its
// result_of and its part_of are claimed there, which a result whose node the backfill has not
// written yet states for the node it gets.
func (c *issueConversion) moveResults(ctx context.Context) ([]graphstore.Claim, error) {
	documents, old, fresh := c.renamedPairs()
	rows, err := c.tx.Query(ctx, `
		select r.id::text, coalesce(moved.fresh, r.check_block_id)
		from check_results r
		left join unnest($2::text[], $3::text[], $4::text[]) as moved (document, old, fresh)
		  on moved.document = r.check_artifact_id::text and moved.old = r.check_block_id
		where r.check_artifact_id = any($1::uuid[])
		order by r.id
	`, c.ids(), documents, old, fresh)
	if err != nil {
		return nil, fmt.Errorf("read the results of the checks of %s: %w", c.key, err)
	}
	type movedResult struct{ id, block string }
	results, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (movedResult, error) {
		var result movedResult
		err := row.Scan(&result.id, &result.block)
		return result, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the results of the checks of %s: %w", c.key, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	ids, blocks := make([]string, len(results)), make([]string, len(results))
	var claims []graphstore.Claim
	for index, result := range results {
		ids[index], blocks[index] = result.id, result.block
		check := refs.BlockAddress(c.treeRef, result.block)
		if !graphstore.AddressFits(check) {
			continue
		}
		for _, kind := range []string{graphstore.TupleResultOf, graphstore.TuplePartOf} {
			claims = append(claims, graphstore.Claim{
				FromKind: graphstore.KindResult, FromID: result.id, Kind: kind, ToKind: refs.BlockNodeKind, ToID: check,
				At: c.at, Who: ConversionActor, SourceEvent: graphstore.NoEvent,
			})
		}
	}
	if err := writeExec(ctx, c.tx, "move the results of the issue's checks into its room", `
		update check_results r set check_artifact_id = $1::uuid, check_block_id = moved.block
		from unnest($2::uuid[], $3::text[]) as moved (id, block)
		where r.id = moved.id
	`, c.tree, ids, blocks); err != nil {
		return nil, err
	}
	return claims, nil
}

// finish writes, for each document in the room's order - verification-md, plan-md, then the spec
// (R21) - the move of its open approval request to the version the conversion wrote, the carry of
// its standing approval to that version (carryStandingApproval: a version whose text is the
// approved text carries it, as R22 has the person's approval carry across a change only of where
// a block sits), its artifact.version, and, under that event, what retiring its old index and the
// room's pass stated of its blocks and its version's node, with the stamp of its links; and then
// what the pass stated of the tree's own fragment and the room's references, with the tree's
// version's node, at the sentinel, since no event records the tree's own version.
func (c *issueConversion) finish(ctx context.Context) error {
	sink := eventSink(ctx)
	caused := map[string]bool{}
	for _, document := range c.documents {
		if err := moveApprovalAsk(ctx, c.tx, c.s.events, document.id, document.version, document.markdown, document.tree, ConversionActor, nil, false, c.s.serverURL); err != nil {
			return err
		}
		if err := refuseSecondCarry(ctx, c.tx, document.id); err != nil {
			return err
		}
		newest, err := NewestReview(ctx, c.tx, document.id)
		if err != nil {
			return err
		}
		if newest != nil && newest.State == "approved" {
			rendered := renderedTree{tree: document.part(), markdown: document.markdown}
			if err := c.s.carryStandingApproval(ctx, c.tx, document.id, document.version.Number, rendered, *newest, decisionStepCases{}, sink); err != nil {
				return err
			}
		}
		issue := c.key
		event := model.Event{
			IssueKey: &issue, Type: "artifact.version", Actor: ConversionActor,
			Payload: ArtifactVersionEventPayload(document.id, document.name, document.version, nil, model.ReferenceChanges{}, nil),
		}
		stated := graphstore.Joined(document.retired, c.index.facts[document.id], document.versionNode)
		caused[document.id] = true
		if err := write(ctx, c.tx, func(b *store.WriteBatch) error {
			if err := c.s.events.QueueAppend(b, event, sink); err != nil {
				return err
			}
			graphstore.QueueCaused(b, stated, graphstore.LatestEvent)
			stampVersionEvent(b, c.tree, document.id, document.version.Number, document.version.CreatedAt, graphstore.LatestEvent)
			graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
			return nil
		}); err != nil {
			return err
		}
	}
	rest := []graphstore.Uncaused{c.treeVersionNode, c.index.references}
	for _, part := range slices.Sorted(maps.Keys(c.index.facts)) {
		if !caused[part] {
			rest = append(rest, c.index.facts[part])
		}
	}
	stated := graphstore.Joined(rest...)
	return write(ctx, c.tx, func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, stated, graphstore.NoEvent)
		graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
		return nil
	})
}

// issueTreeArtifactName and decisionsQueryName are the names an issue's tree artifact and its
// decisions query are created under, from which their slugs are taken (freeIssueSlug), as api
// names them for an issue born converted (issueTreeName, issueDecisionsName).
const (
	issueTreeArtifactName = "tree"
	decisionsQueryName    = "decisions"
)

// freeIssueSlug is the first slug of base, base-2, base-3 and so on that no document of the issue
// issueKey holds, read in tx under the issue's owner lock, as api's nextArtifactSlug takes one.
// base is a slug already: a lower-case word.
func freeIssueSlug(ctx context.Context, tx pgx.Tx, issueKey, base string) (string, error) {
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from artifacts where issue_key = $1 and slug = $2)`,
			issueKey, candidate).Scan(&taken); err != nil {
			return "", fmt.Errorf("read whether %s holds the slug %s: %w", issueKey, candidate, err)
		}
		if !taken {
			return candidate, nil
		}
	}
}

// issueRoomStepLock is the session advisory lock the issue room step takes ("DISSROOM"), a single
// 64-bit key apart from every other single-key advisory lock of the server.
const issueRoomStepLock int64 = 0x44495353524F4F4D

// IssueRoomStep is the server's step that converts the issues in flight into their rooms (R6): it
// runs once at once and then on a tick of its own, every BlockIndexReconcileInterval, for as long
// as the process runs. A tick converts only while every process processes.Live lists the issue
// room; where one lists none, it converts nothing and logs one line naming that process. Otherwise,
// where it has work, it takes its own session lock (issueRoomStepLock), skipping where another
// process holds it, and gives each issue that holds no tree, the projects in key order and the
// issues of a project by number, to ConvertIssue in a transaction of its own under IssueRoomGate,
// logging what the command prints of it. An issue converted, passed over or whose conversion
// failed is settled for the life of the process; a deferred one is read again each tick until it
// converts. The work is owed on the first tick, after a tick a live process without the issue room
// held back, after a tick that converted an issue, and while an issue is deferred.
type IssueRoomStep struct {
	service  *Service
	database *store.Store
	logger   *slog.Logger
	interval time.Duration
	owed     bool
	settled  map[string]bool
	deferred map[string]string
}

// NewIssueRoomStep is the step of a process over database, converting through service and logging
// through logger (slog.Default when nil).
func NewIssueRoomStep(service *Service, database *store.Store, logger *slog.Logger) *IssueRoomStep {
	if logger == nil {
		logger = slog.Default()
	}
	return &IssueRoomStep{
		service: service, database: database, logger: logger, interval: BlockIndexReconcileInterval,
		owed: true, settled: map[string]bool{}, deferred: map[string]string{},
	}
}

// Run ticks at once and then every interval until ctx ends. A tick that fails is logged and tried
// again at the next one.
func (s *IssueRoomStep) Run(ctx context.Context) {
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("dispatch: issue room step tick; the next tick tries again", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.interval):
		}
	}
}

// unconvertedIssuesStatement is every issue that holds no tree, the projects in key order and the
// issues of a project by number.
const unconvertedIssuesStatement = `
	select i.key from issues i
	where not exists (select 1 from issue_trees t where t.issue_key = i.key)
	order by i.project_key, i.number, i.key`

// Tick is one pass of the step (IssueRoomStep).
func (s *IssueRoomStep) Tick(ctx context.Context) error {
	ctx = store.WithTransactionTracking(ctx)
	live, err := processes.Live(ctx, s.database.Pool)
	if err != nil {
		return err
	}
	if blocker, blocked := withoutIssueRoom(live); blocked {
		s.logger.Info("dispatch: the issue room step converts no issue while a live server process lists no issue_room",
			"instance", blocker.ID, "commit", blocker.Commit)
		s.owed = true
		return nil
	}
	if !s.owed && len(s.deferred) == 0 {
		return nil
	}
	conn, err := s.database.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("take the issue room step's connection: %w", err)
	}
	held := true
	defer func() { s.release(conn, held) }()
	var locked bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, issueRoomStepLock).Scan(&locked); err != nil {
		return fmt.Errorf("take the issue room step's lock: %w", err)
	}
	if !locked {
		held = false
		return nil
	}
	rows, err := s.database.Pool.Query(ctx, unconvertedIssuesStatement)
	if err != nil {
		return fmt.Errorf("list the issues that hold no room: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list the issues that hold no room: %w", err)
	}
	converted := false
	for _, key := range keys {
		if s.settled[key] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := s.service.ConvertIssue(ctx, key, IssueRoomGate, false)
		var refusal *ConvertRefusal
		switch {
		case errors.As(err, &refusal):
			s.logger.Info("dispatch: the issue room step converts no issue while a live server process lists no issue_room",
				"reason", refusal.Reason)
			s.owed = true
			return nil
		case err != nil:
			s.logger.Error("dispatch: the issue room step failed on an issue; the next boot tries it again", "issue", key, "error", err)
		case result.Outcome == IssueDeferred:
			if s.deferred[key] != result.Reason {
				s.logger.Info("dispatch: the issue room step deferred an issue", "issue", key, "document", result.Document, "reason", result.Reason)
				s.deferred[key] = result.Reason
			}
			continue
		case result.Outcome == IssuePassedOver:
			s.logger.Info("dispatch: the issue room step passed over an issue", "issue", key, "reason", result.Reason, "items", result.Items)
		default:
			s.logger.Info("dispatch: the issue room step converted an issue", "issue", key, "took", result.Took, "items", result.Items,
				"rollback", ConversionRollbackCost)
			converted = true
		}
		delete(s.deferred, key)
		s.settled[key] = true
	}
	s.owed = converted
	return nil
}

// release gives the step's connection back: at once where it holds no lock, and otherwise through
// duty.FreeSessionLock, which returns it only once Postgres answers that the unlock freed the lock
// and closes it otherwise.
func (s *IssueRoomStep) release(conn *pgxpool.Conn, held bool) {
	if !held {
		conn.Release()
		return
	}
	err := duty.FreeSessionLock(conn, `select pg_advisory_unlock($1)`, issueRoomStepLock)
	switch {
	case err == nil:
	case errors.Is(err, duty.ErrSessionClosed):
		s.logger.Info("dispatch: the issue room step's connection was closed before its unlock; its session ended the lock", "error", err)
	default:
		s.logger.Error("dispatch: the issue room step's lock was not released; its connection leaves the pool", "error", err)
	}
}
