package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The read state of a person (the spec's R6): what they have read, cleared, dismissed
// or snoozed is a tuple from their node and never a node, its far end is what the mark is about,
// and the tuple's own moment is the mark it stands for. TupleReadThrough runs to the root of a
// thread, or to a session, the person has read through that moment; TupleDismissed to the root of
// a thread, or the root message of an Agent thread, they took out of their Inbox at that moment;
// TupleCleared to a session whose conversation they cleared before that moment; TupleReplyRead to
// a reply they read by its id; and TupleSnoozed to an ask they will deal with at that moment, its
// snoozed_until (decision 13). A mark that ends - a restore, an unsnooze, a dismissal a later reply
// takes back, a reply-read row the session's read mark passed - is a retraction of its tuple, so
// its history reads back. Each of the five from a person is appended by FromPerson and by nothing
// else (ErrMarkOutsideFromPerson).
const (
	TupleReadThrough = "read_through"
	TupleDismissed   = "dismissed"
	TupleCleared     = "cleared"
	TupleReplyRead   = "reply_read"
	TupleSnoozed     = "snoozed"
)

// markKinds is the five, held once for the checks below.
var markKinds = [...]string{TupleReadThrough, TupleDismissed, TupleCleared, TupleReplyRead, TupleSnoozed}

// ErrMarkOutsideFromPerson refuses a read-state tuple from a person handed to any other writer of
// this store: FromPerson is the one place that keys such a tuple on the person's node, so a writer
// that went around it could state one person's marks under two spellings of their login.
var ErrMarkOutsideFromPerson = errors.New("a read_through, dismissed, cleared, reply_read or snoozed tuple from a person is appended by FromPerson alone")

// FromPerson appends the read state one write of a person's own states: tuples from the person
// (FromKind KindPerson) under the five kinds above, each to the node the mark is about, asserted
// for a mark the write made and retracted for one it ended, every one under the write's event.
//
// The person of each tuple is PersonNode's canonical login (model.CanonicalLogin), so every
// spelling a route's actor or a projection row holds of one person - the raw actor id
// user_agent_state keys its Clear on, the canonical login the other read-state tables key on -
// meets at one node (decision 9). Each person's node is written beside the tuples, `do nothing`
// where the store holds it already, made by the tuple's writer under its event at made, the moment
// of the write: a mark's own moment can lie ahead of the write that states it (a snooze's
// snoozed_until), and a node is made when a write first records it.
//
// A tuple whose ends lie past the store's address bound is no tuple, as ToPerson has it, and the
// rest of the call is appended; a writer that marks its projection row asks AddressFits of the
// same ends first, so such a row stays its old arm's.
//
// It is QueueFromPerson run at once, as a batch of its own, for a writer with no write phase. Its
// error is the batch's: a statement that fails, or a tuple QueueFromPerson cannot take.
func FromPerson(ctx context.Context, tx pgx.Tx, made time.Time, tuples []Tuple) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueFromPerson(b, made, tuples)
		return nil
	})
}

// QueueFromPerson queues FromPerson on a write phase's batch, as one statement: the person nodes,
// in ascending (kind, id) as every write of this store takes them (WriteArms), then the tuples,
// which take the relation repeat key, so one event states each mark once. A tuple's SourceEvent
// may be LatestEvent, for a write whose event the batch appends right before it. A tuple that is
// no mark from a person, or one that cannot be encoded, fails Send rather than this call, which
// answers nothing (QueueWrite).
func QueueFromPerson(b *store.WriteBatch, made time.Time, tuples []Tuple) {
	if len(tuples) == 0 {
		return
	}
	stated := make([]Tuple, 0, len(tuples))
	var persons []Node
	written := make(map[string]struct{}, len(tuples))
	for _, tuple := range tuples {
		if tuple.FromKind != KindPerson || !isMark(tuple.Kind) || tuple.ToKind == KindValue {
			queueRefused(b, fmt.Errorf("FromPerson appends %s from a person to a node, not %s %s/%s -> %s/%s",
				strings.Join(markKinds[:], ", "), tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID))
			return
		}
		tuple.FromID = model.CanonicalLogin(tuple.FromID)
		if tuple.FromID == "" {
			queueRefused(b, fmt.Errorf("%s -> %s/%s names no person", tuple.Kind, tuple.ToKind, tuple.ToID))
			return
		}
		if !AddressFits(tuple.FromID) || !AddressFits(tuple.ToID) {
			continue
		}
		stated = append(stated, tuple)
		if _, held := written[tuple.FromID]; !held {
			written[tuple.FromID] = struct{}{}
			persons = append(persons, PersonNode(tuple.FromID, tuple.Who, made, tuple.SourceEvent))
		}
	}
	if len(stated) == 0 {
		return
	}
	arms, params, _, err := writeArms(1, persons, stated, nil, true)
	if err != nil {
		queueRefused(b, err)
		return
	}
	b.Queue(fmt.Sprintf("append %d mark from a person", len(stated)), "with "+arms+"\nselect 1", params...)
}

// Marked names the persons whose marks QueueMarksEnded ends: a list of logins its caller knows
// (MarkedBy), or the logins a select reads as the statement runs (MarkedIn).
type Marked struct {
	logins []string
	query  string
	arg    any
}

// MarkedBy is the persons of logins, each read as its canonical login.
func MarkedBy(logins []string) Marked {
	return Marked{logins: logins}
}

// MarkedIn is the persons whose canonical logins query answers as the statement that ends their
// marks runs: a select of one text column over one parameter, written $4, which takes arg. It is
// for a write whose persons are the rows of a table another writer changes, read in the statement
// that ends their marks rather than before the write phase, so a row that writer committed while
// this write waited on a lock both take is read: a session's reply ends the Agent dismissals of its
// thread's root that stand once it holds that root (inboxwrite's queueAgentDismissalsClear).
func MarkedIn(query string, arg any) Marked {
	return Marked{query: query, arg: arg}
}

// QueueMarksEnded queues FromPerson's retraction of the mark of kind from each person of people
// into toKind/toID whose newest tuple asserts it as the statement runs, made by who at at under
// event, and of no other: the standing of the mark is read by the statement that ends it (the way
// CurrentOut reads a relation, CurrentTupleRead), not in Go before the write phase, so a write
// whose earlier statements took the rows the mark's own writer locks first - a reply's lock of its
// thread's user_threads rows, which the dismiss route takes too - ends the mark that writer
// committed while it waited; people read by the statement (MarkedIn) are read then too. Every
// person a standing mark names holds a node already, which FromPerson wrote beside the mark, so
// the statement writes no node. A login past the store's address bound states nothing, as
// FromPerson has it. event may be LatestEvent. A kind that is no mark, or a writer that cannot be
// encoded, fails Send rather than this call, which answers nothing (QueueWrite).
func QueueMarksEnded(b *store.WriteBatch, kind, toKind, toID string, people Marked, at time.Time, who model.Actor, event int64) {
	if !isMark(kind) || toKind == KindValue {
		queueRefused(b, fmt.Errorf("QueueMarksEnded retracts %s from a person to a node, not %s -> %s/%s",
			strings.Join(markKinds[:], ", "), kind, toKind, toID))
		return
	}
	if !AddressFits(toID) {
		return
	}
	persons, named, arg := `any($4::text[])`, "", any(nil)
	if people.query != "" {
		persons, named, arg = `any(array(`+people.query+`))`, "the persons its statement reads", people.arg
	} else {
		logins := make([]string, 0, len(people.logins))
		seen := make(map[string]struct{}, len(people.logins))
		for _, login := range people.logins {
			person := model.CanonicalLogin(login)
			if person == "" || !AddressFits(person) {
				continue
			}
			if _, held := seen[person]; held {
				continue
			}
			seen[person] = struct{}{}
			logins = append(logins, person)
		}
		if len(logins) == 0 {
			return
		}
		named, arg = fmt.Sprintf("%d person", len(logins)), logins
	}
	b.QueueBuilt(fmt.Sprintf("end the %s marks of %s on %s/%s", kind, named, toKind, toID), `
		insert into graph_tuples (`+tupleWriteColumns+`)
		select '`+KindPerson+`', standing.from_id, $1, $2, $3, $5::timestamptz, $6::jsonb, `+causeOf("$7::bigint")+`, null, false
		from (
			`+CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
		// toKind is bound, and never KindValue (refused above), so its `<> 'value'` is written out:
		// graph_tuples_as_of_in's predicate, which a generic plan cannot prove from the bound kind.
		`t.to_kind = $2 and t.to_id = $3 and t.kind = $1 and t.from_kind = '`+KindPerson+`' and t.from_id = `+persons+`
			and t.to_kind <> '`+KindValue+`'`)+`
		) standing
		where standing.asserted
		order by standing.from_id`+relationRepeatKey,
		func() ([]any, error) {
			writer, err := json.Marshal(who.Recorded())
			if err != nil {
				return nil, fmt.Errorf("encode the writer of the %s marks ended: %w", kind, err)
			}
			return []any{kind, toKind, toID, arg, at, writer, event}, nil
		})
}

// MarkArm is the arm of a caller's statement that appends, for each row of rows - a common-table
// expression of that statement answering (login, at, to_id): a person's canonical login, the moment
// of their mark and the id of the toKind node it is about - FromPerson's mark of kind from that
// person into that node, asserted at the row's moment, made by who under event: for a write whose
// mark is the answer of a statement's own write, a row whose upsert moved a read mark (inboxwrite's
// keep of a writer's own row) or a row its insert wrote (inboxwrite's replies a person's own reply
// under an Agent thread reads by id), so the mark is stated where the row was written and nowhere
// else without the answer being read in Go. Its parameters are bound from $first on, after the
// caller's own, and it ends with no comma. The person of every row holds a node already - the
// caller writes it earlier in the same write phase (ToPerson, QueueFromPerson) - so the arm writes
// no node. A row whose login or far end lies past the store's address bound states nothing, as
// FromPerson has it. event may be LatestEvent.
func MarkArm(first int, rows, kind, toKind string, who model.Actor, event int64) (string, []any, error) {
	if !isMark(kind) || toKind == KindValue {
		return "", nil, fmt.Errorf("MarkArm appends %s from a person to a node, not %s -> %s",
			strings.Join(markKinds[:], ", "), kind, toKind)
	}
	writer, err := json.Marshal(who.Recorded())
	if err != nil {
		return "", nil, fmt.Errorf("encode the writer of the %s mark: %w", kind, err)
	}
	param := func(offset int) string { return "$" + strconv.Itoa(first+offset) }
	return `graph_marks_stated as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select '` + KindPerson + `', marked.login, ` + param(0) + `::text, ` + param(1) + `::text, marked.to_id, marked.at, ` + param(2) + `::jsonb, ` + causeOf(param(3)+`::bigint`) + `, null, true
	from ` + rows + ` marked
	where ` + AddressFitsSQL(`marked.login`) + ` and ` + AddressFitsSQL(`marked.to_id`) + `
	order by marked.login, marked.to_id` + relationRepeatKey + `
)`, []any{kind, toKind, writer, event}, nil
}

// isMark is whether kind is one of the five read-state kinds FromPerson appends.
func isMark(kind string) bool { return slices.Contains(markKinds[:], kind) }

// marksFromPerson is whether tuple is a mark from a person, which FromPerson alone appends.
func marksFromPerson(tuple Tuple) bool {
	return tuple.FromKind == KindPerson && isMark(tuple.Kind)
}
