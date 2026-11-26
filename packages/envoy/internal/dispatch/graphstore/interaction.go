package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A run of the command bar (api/going.go) is an interaction node of this store
// (spec requirement r-interaction), addressed `dispatch://interaction/<uuid>` by the
// run's own uuid, and `going_requests` and `going_events` are its projections: the row of the
// first projects the node and its made_by, and each row of the second one frame value of it.
// These are the facts of one run in the one shape every writer of a run states them in: the
// going POST's record, and the writer of one run an earlier build recorded with no node
// (WriteRun), which the frame path and the backfill share.

// The attributes of an interaction beside its words (AttributeWords), as the tuple that states
// each value spells it and as migrations 20261123174710 and 20261122230406 declare them on the
// interaction type: the moment the run is over, each frame of its stream, and the table that
// projects the node. Its project is the base attribute project, which no attribute node declares.
const (
	AttributeEndsAt = "ends_at"
	AttributeFrame  = "frame"
	AttributeSource = "source"
)

// SourceGoingRequests is the source every interaction node this build writes holds: a run of the
// command bar, which going_requests projects. The sweep states such a node gone once its run is
// more than an hour old (api's sweepGoingRequests), and reads the value to leave alone an
// interaction node of any other source, which a later release writes.
const SourceGoingRequests = "going_requests"

// RowlessRunSQL is true of the graph_nodes row n that is an interaction node of the source
// going_requests (SourceGoingRequests), not gone, whose run's going_requests row is no longer
// there (RunRowGoneSQL): a run whose rows a process of the release before deleted an hour old,
// during a roll or after a rollback, which leaves the node live. It is the one test of such a
// node: the sweep reads those an hour old (api's goingRowlessRuns) and the backfill every one
// (graphcensus' backfillGoneRuns), and each states the node gone. The kind, the state and the
// source are the literals graph_nodes_interaction_live is partial on (migration 20261122230407),
// so a read bound by it can read that index. Every writer of an interaction keys it by its run's
// uuid as Postgres writes it, and an id of any other shape is read as a run with no row rather
// than cast (store.CanonicalUUID), so no node can fail the read.
var RowlessRunSQL = `n.kind = '` + KindInteraction + `' and n.state <> '` + StateGone + `'
		  and n.attributes->>'` + AttributeSource + `' = '` + SourceGoingRequests + `'
		  and ` + RunRowGoneSQL(store.CanonicalUUID("n.id"))

// RunRowGoneSQL is true of the run whose uuid is the SQL expression run once its going_requests
// row is no longer there. The sweep removes that row an hour after the run was asked, in the
// transaction that states its node gone (api's goRunGone). A process of the release before
// removes it too, and leaves the node live. The run's going_request_posts and going_events rows
// go with the row. It is the one test of a run with no row. RowlessRunSQL reads it of a node, and
// the graph read of the run's own address reads it to answer no frame of such a run (refs'
// interactionFrames). Where run is null, no row answers it, and the test is true.
func RunRowGoneSQL(run string) string {
	return `not exists (select 1 from going_requests r where r.id = ` + run + `)`
}

// Interaction is one run of the command bar as the store holds it: ID, the run's own uuid in the
// lowercase form Postgres writes it; Author, the whole credential the POST proved, recorded
// (model.Actor.Recorded), which is the node's made_by and which going_requests.author projects;
// the project the run was asked under, the empty string for every project, which states no
// project; the words of the question; At, when it was asked; and EndsAt, when its window closes.
type Interaction struct {
	ID      string
	Author  model.Actor
	Project string
	Words   string
	At      time.Time
	EndsAt  time.Time
}

// InteractionFacts is what the store holds of one run: its interaction node, then the author's
// own nodes, and its tuples. The node is live and reserved for its author whoever that is, since
// a run is read by the credential that asked it and by nobody else, and a run whose author names
// no credential (NamesCredential) - the shared bearer's empty author - is read by nobody, as the
// stream reads it (api's goingRunIsTheCallers). It carries its words, the moment it ends and its
// source, SourceGoingRequests, each also stated as a value tuple, and the run's project where it
// names one. Where AuthorOf names somebody of the author, the run also states its authored_by
// tuple, with that author's own person or session node and the aliases the server proved of them
// (AuthorFacts).
//
// ends_at is written in UTC as RFC 3339 with its fraction of a second, a timestamp attribute's
// text, so the node's attribute and its value tuple read the same instant the run's row holds.
// Every fact is made by the author, at the moment the run was asked, and caused by no event: a
// run appends none.
func InteractionFacts(run Interaction) ([]Node, []Tuple) {
	authors, authored, _ := AuthorFacts(KindInteraction, run.ID, run.Author, run.At, NoEvent)
	node := Node{
		Kind: KindInteraction, ID: run.ID, Supertag: KindInteraction,
		Visibility: VisibilityAuthor, State: StateLive, Project: run.Project,
		Attributes: map[string]any{
			AttributeWords: run.Words, AttributeEndsAt: run.EndsAt.UTC().Format(time.RFC3339Nano),
			AttributeSource: SourceGoingRequests,
		},
		MadeAt: run.At, MadeBy: run.Author, SourceEvent: NoEvent,
	}
	return append([]Node{node}, authors...),
		append(AttributeTuples(node, AttributeWords, AttributeEndsAt, AttributeSource), authored...)
}

// FrameWriter is who every frame value tuple is stated by: the server answering the run - the
// answer, a reader that ends the run at its deadline, or the sweep - which acts for no person, as
// the command that adds a lens kind does (lensKindWriter). The run's author asked the question;
// no frame is their writing.
var FrameWriter = model.Actor{Kind: "system", ID: "going"}

// NodeRowLockSQL takes one node's graph_nodes row `for no key update`, $1 its kind and $2 its id:
// the lock a writer takes of the one node whose own facts it reads and then appends to, so two
// such writes of one node queue at its row and the second reads what the first appended. Every
// frame of a run takes its interaction's row so first, as do the sweep that states a run gone an
// hour on and every backfill pass that writes a run's node (LockInteraction), and Retarget takes
// the row of the node whose pointer it moves.
const NodeRowLockSQL = `select 1 from graph_nodes where kind = $1 and id = $2 for no key update`

// LockInteraction takes the interaction node row of the run whose id is run (NodeRowLockSQL), and
// answers whether the store held that row to take: the first lock of every frame's transaction
// and of the sweep's of a run an hour old (api's lockGoingRun), and of every backfill pass that
// writes a run's node (graphcensus), so frames, the sweep and a pass of one run queue there.
func LockInteraction(ctx context.Context, tx pgx.Tx, run string) (bool, error) {
	tag, err := tx.Exec(ctx, NodeRowLockSQL, KindInteraction, run)
	if err != nil {
		return false, fmt.Errorf("lock the interaction node of run %s: %w", run, err)
	}
	return tag.RowsAffected() == 1, nil
}

// FrameValueSQL is the value of one frame tuple as SQL over the frame's sequence, name and data,
// each an SQL expression: the JSON object {"sequence": <integer>, "name": <text>, "data": <the
// frame's payload>}, written as Postgres writes jsonb as text, so its keys stand in jsonb's own
// order ("data", "name", "sequence") and its spacing is jsonb's. A frame's value is computed in
// SQL rather than in Go so that the frame path, which reads the sequence it takes in the same
// statement, and a writer that states a frame from a going_events row an earlier build wrote
// (WriteRun, ReadRunFrames) write the same text for the same frame, byte for byte. The text is the
// value tuple's far end, which no repeat key or read index bounds; a reader orders a run's frames
// by `(to_id::jsonb ->> 'sequence')::integer`.
func FrameValueSQL(sequence, name, data string) string {
	return `(jsonb_build_object('sequence', ` + sequence + `, 'name', ` + name + `, 'data', ` + data + `)::text)`
}

// FrameTuple is one frame of the run whose id is run, stated as a value tuple of its interaction
// node: value is the frame's text (FrameValueSQL), and at the moment its going_events row is
// written, which that row's created_at holds. It is stated by FrameWriter and caused by no event.
func FrameTuple(run, value string, at time.Time) Tuple {
	return Tuple{
		FromKind: KindInteraction, FromID: run, Kind: AttributeFrame, ToKind: KindValue, ToID: value,
		At: at, Who: FrameWriter, SourceEvent: NoEvent, Asserted: true,
	}
}

// QueueFrameTuple queues on batch the append Write makes of tuple, one frame value of a run
// (FrameTuple): one insert of a value tuple under its repeat key (queueAppend). It is Write for
// that one tuple, for the frame path, which sends it in one round trip beside a statement of its
// own (api's appendGoingEvent): a frame value is no base attribute a value is refused for, no
// pointer a type rule holds and no change of its node (Tuple.Changes), so Write reads nothing and
// appends nothing else for it, whatever run its write phase is made for. Any other tuple is
// refused and nothing is queued: it goes through Write, which holds it to whatever applies.
func QueueFrameTuple(batch *pgx.Batch, tuple Tuple) error {
	if tuple.FromKind != KindInteraction || tuple.Kind != AttributeFrame || tuple.ToKind != KindValue || tuple.Changes {
		return fmt.Errorf("the %s tuple out of %s %s is no frame value of a run, which is all a queued append takes: append it through Write",
			tuple.Kind, tuple.FromKind, tuple.FromID)
	}
	args, err := tupleParams([]Tuple{tuple})
	if err != nil {
		return err
	}
	batch.Queue(tupleInsert(1, "")+valueRepeatKey, args...)
	return nil
}

// FrameHeldSQL is true of a going_events row e whose run's interaction node holds a frame value of
// e's sequence: the anti-join a frame a process of the release before appended with no value is
// found by (graphcensus' backfillRunFrames), and the frames ReadRunFrames leaves out when it reads
// only those owed.
const FrameHeldSQL = `exists (
		select 1 from graph_tuples t
		where t.from_kind = '` + KindInteraction + `' and t.from_id = e.request_id::text
		  and t.kind = '` + AttributeFrame + `' and t.to_kind = '` + KindValue + `' and t.asserted
		  and (t.to_id::jsonb->>'sequence')::integer = e.sequence)`

// terminalFrameNames are the two frame names that end a run's stream (going.Event.Terminal), the
// pair the going_events_terminal index holds as literals: every read of a run's ending names them
// as TerminalFrameNamesSQL writes them (RunEndingSQL).
var terminalFrameNames = []string{contracts.GoingEventEnd, contracts.GoingEventError}

// TerminalFrameNamesSQL is terminalFrameNames as the SQL list the going_events_terminal index's
// predicate writes, `'end', 'error'`: a read that names the two as literals is one the planner
// proves that partial index answers, where a bound array leaves a prepared statement's generic
// plan nothing to prove it by. The stream (api/going.go) and the graph read of a run's own address
// (refs' interactionFrames) read the run's ending by it.
var TerminalFrameNamesSQL = store.QuoteLiterals(terminalFrameNames)

// RunEndingSQL is the sequence of the one terminal frame the run whose uuid is the SQL expression
// run has stored, null while it has stored none: one probe of going_events_terminal, which holds
// that a run stores at most one (TerminalFrameNamesSQL). The stream reads it as the run's state
// (api's goingRunStateOf), and the graph read of a withdrawn run answers the withdrawal at it, as
// the stream writes it there (refs' interactionFrames, api's goingWithdrawal).
func RunEndingSQL(run string) string {
	return `(select e.sequence from going_events e where e.request_id = ` + run + ` and e.name in (` + TerminalFrameNamesSQL + `))`
}

// RunNextSequenceSQL is the sequence the next frame of the run whose uuid is the SQL expression run
// takes: one past its newest, read backward on the (request_id, sequence) key, and 1 for a run
// that has stored none. The frame path reads it once it holds the run's node (api's
// goingNextFrame), so it is the sequence the run's ending takes where nothing is stored before it
// (api's endGoingRun); the graph read of a run withdrawn with no ending stored answers the
// withdrawal at it, and writes nothing (refs' interactionFrames).
func RunNextSequenceSQL(run string) string {
	return `coalesce((select max(e.sequence) from going_events e where e.request_id = ` + run + `), 0) + 1`
}

// RunWithdrawnSQL is true of the run whose uuid is the SQL expression run once a comment or an
// issue message whose text it gave its model no longer stands: a going_request_posts row of the
// run (api's recordGoingPosts) that no comments or messages row answers with deleted_at unset,
// which is a deleted post and a post whose row is gone alike. It is the one test of a withdrawn
// run (api's goingWithdrawal): the stream reads it on every poll (api's goingRunStateOf), and the
// graph read of the run's own address reads it to answer what the stream answers (refs'
// interactionFrames). It reads no verdict of a run whose going_requests row is gone
// (RunRowGoneSQL): the run's going_request_posts rows went with the row. The graph read answers
// no frame of such a run, and reads this only of a run whose row is there.
func RunWithdrawnSQL(run string) string {
	return `exists (
	         select 1
	         from going_request_posts p
	         left join comments c on c.id = p.comment_id and c.deleted_at is null
	         left join messages m on m.id = p.message_id and m.deleted_at is null
	         where p.request_id = ` + run + ` and c.id is null and m.id is null
	       )`
}

// RunWithdrawal is the one frame a reader of a withdrawn run (RunWithdrawnSQL) is written, and all
// it is written from then on: an error frame carrying contracts.GoingErrorAnswerWithdrawn, at the
// sequence of the run's ending. The stream writes it, and stores it as the run's ending where the
// run has stored none (api's goingWithdrawal); the graph read of the run's own address answers it
// in place of every frame the run stored (refs' interactionFrames). Both read this one value, so
// the two views of a withdrawn run answer one frame.
var RunWithdrawal = going.Fault(contracts.GoingErrorAnswerWithdrawn)

// RunWithdrawalData is RunWithdrawal's payload as the frame path encodes every frame's for
// FrameValueSQL (api's appendGoingEvent): the JSON json.Marshal writes of it, encoded once, since
// going.ErrorData is one string, which json.Marshal cannot fail on. The graph read of a withdrawn
// run writes the frame's value from it (refs' interactionFrames), so the text it answers is the
// text the frame path writes of the withdrawal where the stream stores it as the run's ending.
var RunWithdrawalData = func() []byte {
	data, err := json.Marshal(RunWithdrawal.Data)
	if err != nil {
		panic(fmt.Sprintf("encode the payload of the withdrawal frame: %v", err))
	}
	return data
}()

// RunWritten is what WriteRun did for one run: Held, whether the record holds the run, and Nodes
// and Tuples, the nodes and tuples it stated of it, none where the store held its node already.
// The counts are the upper bound the backfill's report states (graphcensus' backfillRuns): an
// author's node another write already made moves nothing, and its base attributes are stated only
// by the write that made it.
type RunWritten struct {
	Held          bool
	Nodes, Tuples int64
}

// WriteRun is the one writer of a run's node from its row: it states in the store, in the caller's
// transaction, the run a going_requests row holds where the store holds no interaction node of it,
// which a pod of the release before writes during a roll, recording a run with no node and
// appending its frames with no frame value. The frame path calls it for the first frame this build
// writes of such a run (api's lockGoingRun), as does the sweep for such a run an hour old, whose
// node it then states gone, and the backfill for each such run it finds (graphcensus' backfillRuns,
// backfillRunFrames). It writes, from the row and the run's going_events rows, exactly what the
// going POST and the frame path write of a run (InteractionFacts, FrameTuple): the interaction
// node, its words, its ends_at and its source, its authored_by tuple with its author's own node
// where the row's author names somebody, and one frame value for each going_events row of the run,
// in sequence, in one Write. A row whose author names nobody gets a node reserved for an author
// nobody proves, which nobody reads, as the stream reads that row.
//
// id names the run as going_requests.id reads it; the node is keyed by the row's own id::text, the
// lowercase form the POST writes. It answers what it wrote (RunWritten): Held is false where the
// record holds no such row, and nothing is written then. A run whose node the store holds already
// is left as it is, answering Held and nothing written: its node and every frame written with it
// are another write's, and this writer mends nothing. Its reads - the row, the node, the frames -
// are its read phase, and that one Write, after all of them, its write phase.
//
// It takes the run's going_requests row `for no key update` before it reads whether the node is
// there, so two writers of one run - two frames this build writes of a run an older pod recorded,
// or the backfill beside one - queue at that row, and the second, reading in a statement of its
// own once the first has committed, finds the node there and writes nothing. The lock conflicts
// with no insert of a frame: a going_events row's foreign key takes the run's row `for key share`.
// It is the step the frame path (api's lockGoingRun) takes for a run with no node, before it
// takes the node's row. The node this writes is its transaction's own, which no other transaction
// can see until it commits, let alone hold; a node another write committed first, which this
// writes nothing of, may be held by then - by a frame of the run, by the backfill's pass over its
// frames, or by the sweep, which holds the node and then deletes the run's row an hour after it was
// asked (api's goRunGone) - so the frame takes that node's row without waiting and, where another
// holds it, lets its transaction go and begins again, taking the node's row first. The sweep waits
// on the run's row the frame holds, so a frame that waited on the node while it held the row would
// wait on the sweep as the sweep waits on it; letting its transaction go keeps that from happening.
func WriteRun(ctx context.Context, tx pgx.Tx, id string) (RunWritten, error) {
	var run Interaction
	var author []byte
	err := tx.QueryRow(ctx, `
		select id::text, author, project, words, created_at, ends_at
		from going_requests
		where id = $1::uuid
		for no key update
	`, id).Scan(&run.ID, &author, &run.Project, &run.Words, &run.At, &run.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunWritten{}, nil
	}
	if err != nil {
		return RunWritten{}, fmt.Errorf("read run %s: %w", id, err)
	}
	_, stored, err := ReadNode(ctx, tx, KindInteraction, run.ID)
	if err != nil {
		return RunWritten{}, err
	}
	if stored {
		return RunWritten{Held: true}, nil
	}
	if err := json.Unmarshal(author, &run.Author); err != nil {
		return RunWritten{}, fmt.Errorf("decode the author of run %s: %w", run.ID, err)
	}
	nodes, tuples := InteractionFacts(run)
	frames, err := ReadRunFrames(ctx, tx, run.ID, false)
	if err != nil {
		return RunWritten{}, err
	}
	if err := Write(ctx, tx, nodes, append(tuples, frames...)); err != nil {
		return RunWritten{}, fmt.Errorf("write the interaction node of run %s: %w", run.ID, err)
	}
	written := RunWritten{Held: true, Nodes: int64(len(nodes)), Tuples: int64(len(tuples) + len(frames))}
	for _, node := range nodes {
		// The visibility and the state of each node it made, and its project where it names one.
		written.Tuples += 2
		if node.Project != "" {
			written.Tuples++
		}
	}
	return written, nil
}

// ReadRunFrames reads a frame value of each going_events row of run, in sequence, as the frame
// path states one (FrameTuple): its text read off the row in SQL (FrameValueSQL), dated at the
// row's created_at. owedOnly leaves out each frame the run's interaction holds a value of already
// (FrameHeldSQL), the frames the backfill owes (graphcensus' writeRunFrames); WriteRun reads them
// all, for a run whose node it makes.
func ReadRunFrames(ctx context.Context, tx pgx.Tx, run string, owedOnly bool) ([]Tuple, error) {
	owed, problem := "", "read the frames of run %s: %w"
	if owedOnly {
		owed, problem = ` and not `+FrameHeldSQL, "read the frames of run %s the store holds no value of: %w"
	}
	rows, err := tx.Query(ctx, `
		select `+FrameValueSQL("e.sequence", "e.name", "e.data")+`, e.created_at
		from going_events e
		where e.request_id = $1::uuid`+owed+`
		order by e.sequence
	`, run)
	if err != nil {
		return nil, fmt.Errorf(problem, run, err)
	}
	frames, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tuple, error) {
		var value string
		var at time.Time
		if err := row.Scan(&value, &at); err != nil {
			return Tuple{}, err
		}
		return FrameTuple(run, value, at), nil
	})
	if err != nil {
		return nil, fmt.Errorf(problem, run, err)
	}
	return frames, nil
}
