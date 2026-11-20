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

// The read state of a person (dispatch://INFRA-1152 R6): what they have read, cleared, dismissed
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
// It is QueueFromPerson run at once, as a batch of its own, for a writer with no write phase.
func FromPerson(ctx context.Context, tx pgx.Tx, made time.Time, tuples []Tuple) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return QueueFromPerson(b, made, tuples) })
}

// QueueFromPerson queues FromPerson on a write phase's batch, as one statement: the person nodes,
// in ascending (kind, id) as every write of this store takes them (WriteArms), then the tuples,
// which take the relation repeat key, so one event states each mark once. A tuple's SourceEvent
// may be LatestEvent, for a write whose event the batch appends right before it.
func QueueFromPerson(b *store.WriteBatch, made time.Time, tuples []Tuple) error {
	if len(tuples) == 0 {
		return nil
	}
	stated := make([]Tuple, 0, len(tuples))
	var persons []Node
	for _, tuple := range tuples {
		if tuple.FromKind != KindPerson || !isMark(tuple.Kind) || tuple.ToKind == KindValue {
			return fmt.Errorf("FromPerson appends %s from a person to a node, not %s %s/%s -> %s/%s",
				strings.Join(markKinds[:], ", "), tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID)
		}
		tuple.FromID = model.CanonicalLogin(tuple.FromID)
		if tuple.FromID == "" {
			return fmt.Errorf("%s -> %s/%s names no person", tuple.Kind, tuple.ToKind, tuple.ToID)
		}
		if !AddressFits(tuple.FromID) || !AddressFits(tuple.ToID) {
			continue
		}
		stated = append(stated, tuple)
		if !slices.ContainsFunc(persons, func(person Node) bool { return person.ID == tuple.FromID }) {
			persons = append(persons, PersonNode(tuple.FromID, tuple.Who, made, tuple.SourceEvent))
		}
	}
	if len(stated) == 0 {
		return nil
	}
	arms, params, _, err := writeArms(1, persons, stated, nil, true)
	if err != nil {
		return err
	}
	b.Queue(fmt.Sprintf("append %d mark from a person", len(stated)), "with "+arms+"\nselect 1", params...)
	return nil
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
// FromPerson has it. event may be LatestEvent.
func QueueMarksEnded(b *store.WriteBatch, kind, toKind, toID string, people Marked, at time.Time, who model.Actor, event int64) error {
	if !isMark(kind) || toKind == KindValue {
		return fmt.Errorf("QueueMarksEnded retracts %s from a person to a node, not %s -> %s/%s",
			strings.Join(markKinds[:], ", "), kind, toKind, toID)
	}
	if !AddressFits(toID) {
		return nil
	}
	persons, named, arg := `any($4::text[])`, "", any(nil)
	if people.query != "" {
		persons, named, arg = `any(array(`+people.query+`))`, "the persons its statement reads", people.arg
	} else {
		logins := make([]string, 0, len(people.logins))
		for _, login := range people.logins {
			if person := model.CanonicalLogin(login); person != "" && AddressFits(person) && !slices.Contains(logins, person) {
				logins = append(logins, person)
			}
		}
		if len(logins) == 0 {
			return nil
		}
		named, arg = fmt.Sprintf("%d person", len(logins)), logins
	}
	writer, err := json.Marshal(who.Recorded())
	if err != nil {
		return fmt.Errorf("encode the writer of the %s marks ended: %w", kind, err)
	}
	b.Queue(fmt.Sprintf("end the %s marks of %s on %s/%s", kind, named, toKind, toID), `
		insert into graph_tuples (`+tupleWriteColumns+`)
		select '`+KindPerson+`', standing.from_id, $1, $2, $3, $5::timestamptz, $6::jsonb, `+causeOf("$7::bigint")+`, null, false
		from (
			`+CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
		`t.to_kind = $2 and t.to_id = $3 and t.kind = $1 and t.from_kind = '`+KindPerson+`' and t.from_id = `+persons)+`
		) standing
		where standing.asserted
		order by standing.from_id`+relationRepeatKey,
		kind, toKind, toID, arg, at, writer, event)
	return nil
}

// MarkArm is the arm of a caller's statement that appends, for each row of rows - a common-table
// expression of that statement answering (login, at): a person's canonical login and the moment
// of their mark - FromPerson's mark of kind from that person into toKind/toID, asserted at the
// row's moment, made by who under event: for a write whose mark is the answer of a statement's
// own write, a row whose upsert moved a read mark (inboxwrite's keep of a writer's own row), so
// the mark is stated where the row moved and nowhere else without the answer being read in Go.
// Its parameters are bound from $first on, after the caller's own, and it ends with no comma. The
// person of every row holds a node already - the caller writes it earlier in the same write
// phase (ToPerson) - so the arm writes no node. event may be LatestEvent.
func MarkArm(first int, rows, kind, toKind, toID string, who model.Actor, event int64) (string, []any, error) {
	if !isMark(kind) || toKind == KindValue {
		return "", nil, fmt.Errorf("MarkArm appends %s from a person to a node, not %s -> %s/%s",
			strings.Join(markKinds[:], ", "), kind, toKind, toID)
	}
	writer, err := json.Marshal(who.Recorded())
	if err != nil {
		return "", nil, fmt.Errorf("encode the writer of the %s mark: %w", kind, err)
	}
	param := func(offset int) string { return "$" + strconv.Itoa(first+offset) }
	return `graph_marks_stated as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select '` + KindPerson + `', marked.login, ` + param(0) + `::text, ` + param(1) + `::text, ` + param(2) + `::text, marked.at, ` + param(3) + `::jsonb, ` + causeOf(param(4)+`::bigint`) + `, null, true
	from ` + rows + ` marked
	where octet_length(marked.login) <= ` + strconv.Itoa(MaxAddressBytes) + `
	order by marked.login` + relationRepeatKey + `
)`, []any{kind, toKind, toID, writer, event}, nil
}

// isMark is whether kind is one of the five read-state kinds FromPerson appends.
func isMark(kind string) bool { return slices.Contains(markKinds[:], kind) }

// marksFromPerson is whether tuple is a mark from a person, which FromPerson alone appends.
func marksFromPerson(tuple Tuple) bool {
	return tuple.FromKind == KindPerson && isMark(tuple.Kind)
}
