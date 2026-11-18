package graphstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A run of the command bar (api/going.go) is an interaction node of this store
// (dispatch://INFRA-1950/spec#b-r-interaction), addressed `dispatch://interaction/<uuid>` by the
// run's own uuid, and `going_requests` and `going_events` are its projections: the row of the
// first projects the node and its made_by, and each row of the second one frame value of it.
// These are the facts of one run in the one shape every writer of a run states them in: the
// going POST's record, and the backfill's writer for one run an earlier build recorded with no
// node (graphcensus.WriteRun).

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
// nobody - a credential that proves no author (ProvesAuthor) - is read by nobody, as the stream
// reads it (api.streamGoingAnswer). It carries its words and the moment it ends, each also stated
// as a value tuple, and the run's project where it names one. Where AuthorOf names somebody of the
// author, the run also states its authored_by tuple, with that author's own person or session node
// and the aliases the server proved of them (AuthorFacts).
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
// (graphcensus.WriteRun) write the same text for the same frame, byte for byte. The text is the
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

// QueueFrameTuple queues on batch the one statement Write sends for tuple, one frame value of a run
// (FrameTuple): the append of value tuples under their repeat key (appendTuples). It is Write for
// that one tuple, for the frame path, which sends it in one round trip beside a statement of its
// own (api's appendGoingEvent): a frame value is no base attribute a value is refused for, no
// pointer a type rule holds and no change of its node (Tuple.Changes), so Write reads nothing and
// appends nothing else for it. Any other tuple is refused and nothing is queued: it goes through
// Write, which holds it to whatever applies.
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
