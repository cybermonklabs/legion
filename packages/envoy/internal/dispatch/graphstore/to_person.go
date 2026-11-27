package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The three relations that tie a thing to a person by what reached them (the spec's R2
// and R4). TupleDeliveredTo (people.go) points from a write at each person it reached;
// TupleMentions points from a thing at the person a mention of it names, as it points at a
// document or an issue the thing cites; TupleParticipatesIn points from a person, or a session,
// at the root of a thread they took part in. Each of the three that names a person is appended by
// ToPerson and by nothing else. What a person has read, cleared, dismissed or snoozed is the other
// direction, a tuple from the person, and FromPerson's (from_person.go).
const (
	TupleMentions       = "mentions"
	TupleParticipatesIn = "participates_in"
)

// ErrReservedForAuthor refuses a delivered_to, a mentions or a participates_in tuple between a
// node reserved for its author (VisibilityAuthor) and a person who is not that author: the read
// of a person's own node (R1) answers only what that person may see, so such a tuple would be
// mail nobody can read (R2).
var ErrReservedForAuthor = errors.New("a node reserved for its author reaches no other person")

// ErrPersonTupleOutsideToPerson refuses one of the three kinds ToPerson appends, naming a person,
// handed to any other writer of this store: ToPerson is the one place that holds such a tuple to
// the rule of ErrReservedForAuthor, so a writer that went around it would be a second rule.
var ErrPersonTupleOutsideToPerson = errors.New("a delivered_to, mentions or participates_in tuple naming a person is appended by ToPerson alone")

// ToPerson appends tuples that tie a thing to a person: a delivered_to or a mentions from the
// thing to the person, or a participates_in from the person to the root of the thread. It is
// the only way those three kinds reach a person's node (ErrPersonTupleOutsideToPerson).
//
// The person of each tuple is graphstore.PersonNode's canonical login (model.CanonicalLogin), so
// the three spellings a record can hold of one person meet at one node. Each person's node is
// written beside the tuples, `do nothing` where the store holds it already, so a person reached
// before they wrote anything has a node their page is read from (api's listPersonNodes): the
// node is made by the tuple's writer at its moment and under its event, and its base attributes
// with it, as a write that first records a person makes one (AuthorFacts).
//
// A thing reserved for its author reaches no person but that author: a tuple whose other end is
// a node the store holds at VisibilityAuthor is refused with ErrReservedForAuthor, naming the
// node and its visibility, unless the person is the author that node's made_by proved (the one
// rule, AuthorIsPerson), and nothing of the call is appended. A node the store holds no row for is
// visible, as the read's own predicate has it (refs.VisibleKind). An end past the store's address
// bound is no tuple, and the rest of the call is appended.
//
// It is QueueToPerson run at once, as a batch of its own, for a writer with no write phase. Its
// error is the batch's: a refusal, a statement that fails, or a tuple QueueToPerson cannot take.
func ToPerson(ctx context.Context, tx pgx.Tx, tuples []Tuple) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		QueueToPerson(b, tuples)
		return nil
	})
}

// QueueToPerson queues ToPerson on a write phase's batch, as one statement: the write of the person
// nodes and the tuples, the nodes first in ascending (kind, id), as every write of this store takes
// them (WriteArms), whose own query is the read of the nodes reserved for their author among the
// tuples' other ends, which locks nothing and whose callback refuses a tuple one of them forbids
// (ErrReservedForAuthor). The tuples take the relation repeat key, so one event states each
// relation once. The read runs where the batch reaches it, so it sees every node the statements
// queued before it wrote, as the read ToPerson ran after those writes saw them; the statement's
// arms run to their end whatever it answers, and a refusal stops the batch's reading, and the
// transaction it ends commits nothing of the call. A tuple's SourceEvent may be LatestEvent, for a
// write whose event the batch appends right before it. A tuple of none of the three kinds, or one
// that cannot be encoded, fails Send rather than this call, which answers nothing (QueueWrite).
func QueueToPerson(b *store.WriteBatch, tuples []Tuple) {
	if len(tuples) == 0 {
		return
	}
	stated := make([]Tuple, 0, len(tuples))
	// reaches is, beside each stated tuple, the person it names and the node at its other end.
	reaches := make([]personReach, 0, len(tuples))
	persons := make([]Node, 0, len(tuples))
	made := make(map[string]struct{}, len(tuples))
	ends := make(map[[2]string]struct{}, len(tuples))
	for _, tuple := range tuples {
		person, end, err := personEnds(&tuple)
		if err != nil {
			queueRefused(b, err)
			return
		}
		if !AddressFits(person) || !AddressFits(end[1]) {
			continue
		}
		stated = append(stated, tuple)
		reaches = append(reaches, personReach{person: person, end: end})
		ends[end] = struct{}{}
		if _, held := made[person]; !held {
			made[person] = struct{}{}
			persons = append(persons, PersonNode(person, tuple.Who, tuple.At, tuple.SourceEvent))
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
	read, args := reservedForAuthorRead(len(params)+1, ends)
	b.Queue(fmt.Sprintf("append %d tuple naming a person", len(stated)), "with "+arms+read, append(params, args...)...).
		Query(reservedForAuthor(len(ends), func(reserved map[[2]string]model.Actor) error {
			for index, reach := range reaches {
				author, held := reserved[reach.end]
				if !held {
					continue
				}
				if AuthorIsPerson(author, reach.person) {
					continue
				}
				return reservedRefusal(stated[index], reach.person, reach.end)
			}
			return nil
		}))
}

// ToPersonRows is the select list a caller's common-table expression answers for ToPersonArms and
// ReservedForAuthorRead, one row per tuple, in the order the tuples are appended: position, and the
// tuple's from_kind, from_id, kind, to_kind, to_id and asserted, each a delivered_to or a mentions
// to a person or a participates_in from one, the person's end a canonical login
// (user_threads_login, model.CanonicalLogin in SQL).
const ToPersonRows = `position, from_kind, from_id, kind, to_kind, to_id, asserted`

// ToPersonArms is the arms of a caller's statement that appends the tuples ToPerson appends whose
// rows that statement derives itself, read off rows, a common-table expression of it answering
// ToPersonRows: for a writer whose tuples hang on what its own write phase's earlier statements
// leave, which no read in Go can answer before the batch is sent (inboxwrite's move of who an ask
// waits on, after the asks rows it moves are locked). Every tuple is dated at, made by who and
// caused by event (LatestEvent for the event the batch appends right before). The person each
// asserted tuple names gets their node, as ToPerson writes it - project-visible, live, made by who
// at at under event, `do nothing` where the store holds it already, with its base attributes for a
// node it makes - before any tuple is appended, in ascending (kind, id), as every write of this
// store takes them (WriteArms). A row whose person or other end is past the store's address bound
// is no tuple, as ToPerson has it. The rule of ErrReservedForAuthor is the caller's to hold, by
// the query of the same statement over the same rows, written after these arms
// (ReservedForAuthorRead). Its parameters are bound from $first on, after the caller's own, and
// the arms end with no comma.
func ToPersonArms(first int, rows string, at time.Time, who model.Actor, event int64) (string, []any, error) {
	writer, err := json.Marshal(who.Recorded())
	if err != nil {
		return "", nil, fmt.Errorf("encode the writer of the tuples naming a person: %w", err)
	}
	param := func(offset int) string { return "$" + strconv.Itoa(first+offset) }
	at0, who1, event2 := param(0)+`::timestamptz`, param(1)+`::jsonb`, param(2)+`::bigint`
	person := `(case when stated.kind = '` + TupleParticipatesIn + `' then stated.from_id else stated.to_id end)`
	return `graph_people_stated as (
	select ` + ToPersonRows + `, ` + person + ` as person
	from ` + rows + ` stated
	where ` + AddressFitsSQL(`stated.from_id`) + ` and ` + AddressFitsSQL(`stated.to_id`) + `
),
graph_nodes_written as (
	insert into graph_nodes (` + nodeColumns + `)
	select distinct '` + KindPerson + `', people.person, '` + KindPerson + `', '` + VisibilityProject + `', '` + StateLive + `', '{}'::jsonb,
	       ` + at0 + `, ` + who1 + `, ` + causeOf(event2) + `, null::text, null::text[]
	from graph_people_stated people
	where people.asserted
	order by 2
	on conflict (kind, id) do nothing
	returning ` + nodeWrittenColumns + `
),
` + nodeAttributesArm + `,
graph_relations_appended as (
	insert into graph_tuples (` + tupleWriteColumns + `)
	select people.from_kind, people.from_id, people.kind, people.to_kind, people.to_id,
	       ` + at0 + `, ` + who1 + `, ` + causeOf(event2) + `, null, people.asserted
	from graph_people_stated people
	where ` + nodesWrittenFirst + `
	order by people.position` + relationRepeatKey + `
)`, []any{at, writer, event}, nil
}

// ReservedForAuthorRead is a read over rows, a common-table expression of the caller's statement
// answering ToPersonRows, of the first tuple ToPerson would refuse (ErrReservedForAuthor): one
// whose other end is a node the store holds at VisibilityAuthor and whose person is not the author
// that node's made_by proved (the one rule in SQL, AuthorIsSQL, as ToPerson asks it in Go through
// AuthorIsPerson). It answers (kind, from_kind, from_id, to_kind, to_id, person), no row where
// none would be refused, for the caller's callback to refuse with RefuseReservedForAuthor; the
// caller makes it the query of the statement whose arms append the same rows (ToPersonArms),
// written after them, so the rows are worked out once: those arms run to their end whatever it
// answers, and a refusal ends the transaction, which commits nothing of them.
func ReservedForAuthorRead(rows string) string {
	return `
		select stated.kind, stated.from_kind, stated.from_id, stated.to_kind, stated.to_id, person.login
		from ` + rows + ` stated
		cross join lateral (select case when stated.kind = '` + TupleParticipatesIn + `' then stated.from_id else stated.to_id end as login) person
		cross join lateral (select case when stated.kind = '` + TupleParticipatesIn + `' then stated.to_kind else stated.from_kind end as kind,
		                           case when stated.kind = '` + TupleParticipatesIn + `' then stated.to_id else stated.from_id end as id) other
		join graph_nodes n on n.kind = other.kind and n.id = other.id
		where n.visibility = '` + VisibilityAuthor + `'
		  and not ` + AuthorIsSQL(`n.made_by`, `jsonb_build_object('kind', '`+authorPerson+`', 'id', person.login)`) + `
		limit 1`
}

// PersonOfSQL is the person AuthorOf reads off an actor, in SQL, over an actor stored as jsonb at
// column: the canonical login of the user it names, or of a personal token's owner, through
// user_threads_login (model.CanonicalLogin in SQL), and null for a session or nobody.
func PersonOfSQL(column string) string {
	return `(case
		      when ` + column + `->>'kind' = 'user' and coalesce(` + column + `->>'id', '') <> '' then user_threads_login(` + column + `->>'id')
		      when ` + column + `->>'kind' = 'session' and coalesce(` + column + `->>'id', '') <> '' then null
		      when coalesce(` + column + `->>'owner', '') <> '' then user_threads_login(` + column + `->>'owner')
		  end)`
}

// RefuseReservedForAuthor is the callback of ReservedForAuthorRead: the refusal ToPerson answers
// for the row it read, naming the tuple, its person and the node visible to its author alone, and
// nil where it read none.
func RefuseReservedForAuthor(row pgx.Row) error {
	var kind, fromKind, fromID, toKind, toID, person string
	switch err := row.Scan(&kind, &fromKind, &fromID, &toKind, &toID, &person); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	end := [2]string{fromKind, fromID}
	if kind == TupleParticipatesIn {
		end = [2]string{toKind, toID}
	}
	return reservedRefusal(Tuple{Kind: kind, FromKind: fromKind, FromID: fromID, ToKind: toKind, ToID: toID}, person, end)
}

// reservedRefusal is ToPerson's refusal of tuple, which names person and whose other end, end, is
// a node the store holds visible to its author alone (ErrReservedForAuthor).
func reservedRefusal(tuple Tuple, person string, end [2]string) error {
	return fmt.Errorf("%s %s/%s -> %s/%s names person %s, and %s/%s is visible to its author alone (visibility %s): %w",
		tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID, person, end[0], end[1], VisibilityAuthor, ErrReservedForAuthor)
}

// personReach is what one tuple ToPerson appends ties: the person, by their canonical login, and
// the node at the tuple's other end, whose visibility decides whether the person may be reached.
type personReach struct {
	person string
	end    [2]string
}

// personEnds reads one tuple ToPerson is handed: the person it names, canonicalised on the tuple
// itself, and the node at its other end, which is the thing a delivered_to or a mentions points
// from and the thread root a participates_in points at. A tuple of another kind, or of one of the
// three with no person at the end its kind puts one, is no tuple of ToPerson's.
func personEnds(tuple *Tuple) (string, [2]string, error) {
	switch tuple.Kind {
	case TupleDeliveredTo, TupleMentions:
		if tuple.ToKind == KindPerson {
			tuple.ToID = model.CanonicalLogin(tuple.ToID)
			return tuple.ToID, [2]string{tuple.FromKind, tuple.FromID}, nil
		}
	case TupleParticipatesIn:
		if tuple.FromKind == KindPerson {
			tuple.FromID = model.CanonicalLogin(tuple.FromID)
			return tuple.FromID, [2]string{tuple.ToKind, tuple.ToID}, nil
		}
	}
	return "", [2]string{}, fmt.Errorf("ToPerson appends %s to a person and %s from one, not %s %s/%s -> %s/%s",
		TupleDeliveredTo+" and "+TupleMentions, TupleParticipatesIn, tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID)
}

// reservedForAuthorRead is the read that answers, of the nodes ends names, each the store holds at
// VisibilityAuthor, with who made it, in one read of graph_nodes by its key: the query of
// QueueToPerson's statement, written after its arms, its parameters bound from $first on.
func reservedForAuthorRead(first int, ends map[[2]string]struct{}) (string, []any) {
	kinds := make([]string, 0, len(ends))
	ids := make([]string, 0, len(ends))
	for end := range ends {
		kinds, ids = append(kinds, end[0]), append(ids, end[1])
	}
	param := func(offset int) string { return "$" + strconv.Itoa(first+offset) }
	return `
		select n.kind, n.id, n.made_by
		from unnest(` + param(0) + `::text[], ` + param(1) + `::text[]) as named(kind, id)
		join graph_nodes n on n.kind = named.kind and n.id = named.id
		where n.visibility = ` + param(2), []any{kinds, ids, VisibilityAuthor}
}

// reservedForAuthor reads the answer of reservedForAuthorRead over ends nodes once the statement
// has run and hands it to check; check's error stops the batch.
func reservedForAuthor(ends int, check func(map[[2]string]model.Actor) error) func(pgx.Rows) error {
	return func(rows pgx.Rows) error {
		reserved := map[[2]string]model.Actor{}
		for rows.Next() {
			var kind, id string
			var madeBy []byte
			if err := rows.Scan(&kind, &id, &madeBy); err != nil {
				return fmt.Errorf("read a node reserved for its author: %w", err)
			}
			var author model.Actor
			if err := json.Unmarshal(madeBy, &author); err != nil {
				return fmt.Errorf("decode the author of %s/%s: %w", kind, id, err)
			}
			reserved[[2]string{kind, id}] = author
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read which of %d node is reserved for its author: %w", ends, err)
		}
		return check(reserved)
	}
}

// NamesPerson is whether tuple is one of the three kinds ToPerson alone appends, naming a person
// at the end its kind puts one: a writer that hands the store tuples of every kind parts them by
// it, those to ToPerson and every other to Write.
func NamesPerson(tuple Tuple) bool {
	switch tuple.Kind {
	case TupleDeliveredTo, TupleMentions:
		return tuple.ToKind == KindPerson
	case TupleParticipatesIn:
		return tuple.FromKind == KindPerson
	}
	return false
}

// refusePersonTuples is the check every writer of this store but ToPerson and FromPerson makes of
// the tuples and the claims it is handed: a tuple of ToPerson's three kinds naming a person is
// ToPerson's (ErrPersonTupleOutsideToPerson), and a mark from a person is FromPerson's
// (ErrMarkOutsideFromPerson).
func refusePersonTuples(tuples []Tuple, claims []Claim) error {
	for _, tuple := range tuples {
		if err := refusePersonTuple("", tuple); err != nil {
			return err
		}
	}
	for _, claim := range claims {
		claimed := Tuple{FromKind: claim.FromKind, FromID: claim.FromID, Kind: claim.Kind, ToKind: claim.ToKind, ToID: claim.ToID}
		if err := refusePersonTuple("claim ", claimed); err != nil {
			return err
		}
	}
	return nil
}

// refusePersonTuple is that check of one tuple, or of one claim spelled as its tuple, which what
// names as such in the refusal.
func refusePersonTuple(what string, tuple Tuple) error {
	switch {
	case NamesPerson(tuple):
		return fmt.Errorf("%s%s %s/%s -> %s/%s: %w", what, tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID, ErrPersonTupleOutsideToPerson)
	case marksFromPerson(tuple):
		return fmt.Errorf("%s%s %s/%s -> %s/%s: %w", what, tuple.Kind, tuple.FromKind, tuple.FromID, tuple.ToKind, tuple.ToID, ErrMarkOutsideFromPerson)
	}
	return nil
}
