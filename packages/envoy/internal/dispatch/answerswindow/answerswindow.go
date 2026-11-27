// Package answerswindow is the one rule for the window of the agents' answers read
// (sessionanswers.List) on a database that cmd/dispatch-seed filled, and for the three sessions
// that read is measured for. The seed's report prints them, the seed's test lists the answers over
// them, and benchtime binds them for every measurement of the read. All three read them here, so
// the seed and a measurement never disagree about the window or the sessions. It is an ordinary
// package rather than a part of the seed because Go cannot import a command.
package answerswindow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// personWordsEvent is the SQL predicate, on the event aliased e, that holds for a person's event
// of the five types that the answers read reads (sessionanswers.query, reached_by_destination;
// model.PersonWordsEventTypesSQL): an answer, a comment, a new message, a reply to a message, and
// every ask.resolved. It tests the type alone, so it holds for every resolution of an ask, where
// the read lists only the declines among them (an ask.resolved whose resolution is of kind
// declined). The seed writes declines alone, so on a seeded database the two agree.
const personWordsEvent = `e.actor->>'kind' = 'user' and e.type in ` + model.PersonWordsEventTypesSQL

// DeliveredPersonEvent is the SQL predicate, on the event aliased e, that holds for a person's
// event of the five types (personWordsEvent) whose published_destinations name at least one
// session's topic (contracts.AgentTopicPrefix): a delivery of a person's words to a session, the
// events the answers read reads. It is no count of the list: the list holds one item for each
// ask, comment and message at its current words and only where those are in the window, so an
// ask answered twice is one item; it lists no resolution that is not a decline; and it adds the
// session's message and comment deliveries, which this predicate never holds. The seed answers
// each ask at most once and resolves one only by a decline.
const DeliveredPersonEvent = personWordsEvent + `
	  and exists (select 1 from unnest(e.published_destinations) reached (topic)
	              where reached.topic like '` + contracts.AgentTopicPrefix + `%')`

// Length is the window that the answers read lists when its request names no start: the 72 hours
// that every agent host carries on each model request (api's sessionAnswersWindow). StartQuery
// takes the same 72 hours from the newest delivery.
const Length = 72 * time.Hour

// StartQuery is the one rule for the start of the seed's answers window: the newest created_at of
// a person's event of the five types whose destinations name a session, less the 72 hours of
// Length. Nothing else decides the window. Read reads the subjects over it, the seed's test lists
// the answers over it, and a measurement of the read binds the start it answers as its since:
// read from the database, never now() less 72 hours, since the seed dates every row in its own
// year, which is in the past of any run. It answers null on a database that holds no delivery.
//
// The seed's writer times the deliveries so that the rule's start falls between them whatever the
// newest one's exact time (cmd/dispatch-seed, planDeliveries): the deliveries in the window are in
// the last 72 hours of the seed's span, and every older one is at least a day before the earliest
// start that the rule can then answer.
var StartQuery = `
	select max(e.created_at) - ` + fmt.Sprintf("interval '%d seconds'", int64(Length/time.Second)) + `
	from events e
	where ` + DeliveredPersonEvent

// SessionsQuery is, for each session that a delivery of a person's words names, how many of those
// deliveries are inside the window that starts at $1, the start StartQuery answers, and how many
// are before it, in the order of the session ids: what Read chooses the read's three subjects
// from.
const SessionsQuery = `
	select substr(reached.topic, length('` + contracts.AgentTopicPrefix + `') + 1),
	       count(*) filter (where e.created_at >= $1),
	       count(*) filter (where e.created_at < $1)
	from events e
	cross join lateral unnest(e.published_destinations) reached (topic)
	where ` + personWordsEvent + `
	  and reached.topic like '` + contracts.AgentTopicPrefix + `%'
	group by 1
	order by 1`

// Subject is one session that the answers read is measured for, with how many deliveries of a
// person's words reached it inside the window and how many before it. Session is empty where the
// seed holds no session for the subject: too few sessions at a very small scale.
type Subject struct {
	Session      string
	InWindow     int
	BeforeWindow int
}

// String is the subject as the seed's report prints it.
func (s Subject) String() string {
	if s.Session == "" {
		return "none: the seed holds no session for it at this scale"
	}
	return fmt.Sprintf("%s with %d deliveries in the window and %d before it", s.Session, s.InWindow, s.BeforeWindow)
}

// Subjects is the start of the answers window and the three sessions that the read is measured
// for. Old is the session with the most deliveries before the window. Most and Fewest are, of the
// other sessions, the one with the most deliveries in the window and the one with the fewest
// there, at least one, Fewest being another session than Most. A tie goes to the lower session
// id. Since is the zero time on a database that holds no delivery.
type Subjects struct {
	Since             time.Time
	Old, Most, Fewest Subject
}

// Queryer is the query surface that Read reads through: a pool (*store.Pool), which the seed
// holds, or one connection (*pgx.Conn), which benchtime holds.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Read reads the answers window's start by its one rule (StartQuery) and chooses the three
// subjects over the deliveries that each session holds on either side of that start
// (SessionsQuery), as Subjects states them.
func Read(ctx context.Context, q Queryer) (Subjects, error) {
	var subjects Subjects
	var since *time.Time
	if err := q.QueryRow(ctx, StartQuery).Scan(&since); err != nil {
		return Subjects{}, fmt.Errorf("read the answers window's start: %w", err)
	}
	if since == nil {
		return subjects, nil
	}
	subjects.Since = since.UTC()
	rows, err := q.Query(ctx, SessionsQuery, subjects.Since)
	if err != nil {
		return Subjects{}, fmt.Errorf("count the deliveries of each session: %w", err)
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subject, error) {
		var subject Subject
		return subject, row.Scan(&subject.Session, &subject.InWindow, &subject.BeforeWindow)
	})
	if err != nil {
		return Subjects{}, fmt.Errorf("count the deliveries of each session: %w", err)
	}
	// The rows come in session id order and each choice takes a later row only on a strictly
	// better count, so a tie goes to the lower id.
	for _, session := range sessions {
		if session.BeforeWindow > subjects.Old.BeforeWindow {
			subjects.Old = session
		}
	}
	for _, session := range sessions {
		if session.Session != subjects.Old.Session && session.InWindow > subjects.Most.InWindow {
			subjects.Most = session
		}
	}
	for _, session := range sessions {
		if session.Session == subjects.Old.Session || session.Session == subjects.Most.Session || session.InWindow == 0 {
			continue
		}
		if subjects.Fewest.Session == "" || session.InWindow < subjects.Fewest.InWindow {
			subjects.Fewest = session
		}
	}
	return subjects, nil
}
