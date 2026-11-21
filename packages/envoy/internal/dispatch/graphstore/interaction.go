package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A run of the command bar (api/going.go) is an interaction node of this store
// (spec requirement r-interaction), addressed `dispatch://interaction/<uuid>` by the
// run's own uuid, and `going_requests` and `going_events` are its projections: the row of the
// first projects the node and its made_by, and each row of the second one frame value of it.
// These are the facts of one run in the one shape every writer of a run states them in: the
// going POST's record, and the writer of one run an earlier build recorded with no node
// (WriteRun), which the frame path and the backfill share.

// The attributes of an interaction beside its words (AttributeWords), as the tuple that states
// each value spells it and as migration 20261116123354 declares them on the interaction type: the
// moment the run is over, and each frame of its stream. Its project is the base attribute
// project, which no attribute node declares.
const (
	AttributeEndsAt = "ends_at"
	AttributeFrame  = "frame"
)

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
// stream reads it (api's goingRunIsTheCallers). It carries its words and the moment it ends, each
// also stated as a value tuple, and the run's project where it names one. Where AuthorOf names
// somebody of the author, the run also states its authored_by tuple, with that author's own person
// or session node and the aliases the server proved of them (AuthorFacts).
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
		Attributes: map[string]any{AttributeWords: run.Words, AttributeEndsAt: run.EndsAt.UTC().Format(time.RFC3339Nano)},
		MadeAt:     run.At, MadeBy: run.Author, SourceEvent: NoEvent,
	}
	return append([]Node{node}, authors...), append(AttributeTuples(node, AttributeWords, AttributeEndsAt), authored...)
}

// FrameWriter is who every frame value tuple is stated by: the server answering the run - the
// answer, a reader that ends the run at its deadline, or the sweep - which acts for no person, as
// the command that adds a lens kind does (lensKindWriter). The run's author asked the question;
// no frame is their writing.
var FrameWriter = model.Actor{Kind: "system", ID: "going"}

// NodeRowLockSQL takes one node's graph_nodes row `for no key update`, $1 its kind and $2 its id:
// the lock a writer takes of the one node whose own facts it reads and then appends to, so two
// such writes of one node queue at its row and the second reads what the first appended. Every
// frame of a run takes its interaction's row so first, as does every backfill pass that writes a
// run's node (LockInteraction), and Retarget takes the row of the node whose pointer it moves.
const NodeRowLockSQL = `select 1 from graph_nodes where kind = $1 and id = $2 for no key update`

// LockInteraction takes the interaction node row of the run whose id is run (NodeRowLockSQL), and
// answers whether the store held that row to take: the first lock of every frame's transaction
// (api's lockGoingRun) and of every backfill pass that writes a run's node (graphcensus), so a
// frame and a pass of one run queue there.
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
// writes of such a run (api's lockGoingRun), and the backfill for each such run it finds
// (graphcensus' backfillRuns, backfillRunFrames). It writes, from the row and the run's
// going_events rows, exactly what the going POST and the frame path write of a run
// (InteractionFacts, FrameTuple): the interaction node, its words and its ends_at, its authored_by
// tuple with its author's own node where the row's author names somebody, and one frame value for
// each going_events row of the run, in sequence, in one Write. A row whose author names nobody gets
// a node reserved for an author nobody proves, which nobody reads, as the stream reads that row.
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
// writes nothing of, may be held by then - an erase of the run takes the node's row and then the
// run's - so the frame takes that node's row without waiting and, where another holds it, lets its
// transaction go and begins again, taking the node's row first.
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
