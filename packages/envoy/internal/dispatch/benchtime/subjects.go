package benchtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/answerswindow"
)

// Subjects are the rows the measured statements read: the largest project and its most heavily
// linked issue, the document most anchors point at, the sessions the agents list reads, the
// embedding a meaning arm searches by, three words of three frequencies, and the window and the
// three sessions of the agents' answers read.
//
// Each is read from the database when the run starts rather than written here as the key the
// seed printed, so a re-seed, a scale flag or a second seeded database moves the subjects with
// it and the harness still measures the heaviest row rather than one that no longer exists. On
// the 13 GB database seeded on 31 October 2026 they resolve to SEED1, SEED1-1 (104 children, 99
// blockers), the artifact behind SEED1-401/spec (10,581 anchored comments), seed-session-00001
// to seed-session-00005, seed-embed-v1, and words of the seed's Zipf vocabulary.
type Subjects struct {
	// Project is the project holding the most issues: the listing's and the search's scope.
	Project string
	// Issue is that project's most heavily linked issue, children and blockers counted
	// together: the subject of the backlink count, the closure, the children read and the ask,
	// message and comment lists.
	Issue string
	// AnchoredDocument is the artifact id carrying the most anchored comments: the subject of
	// every anchor read and of the artifact ask lists.
	AnchoredDocument string
	// Sessions are five session ids the agents list reads the last activity of.
	Sessions []string
	// EmbeddingModel and QueryVector are the model of Issue's embedding and that embedding as a
	// pgvector literal, which is the query vector of every meaning arm: a vector drawn around
	// one of the seed's cluster centres scores above searchMeaningFloor, where a random one does
	// not and the meaning arms would measure a search that found nothing.
	EmbeddingModel string
	QueryVector    string
	// RareWord, MediumWord and CommonWord are words of the project at three frequencies, read
	// from its term statistics: the rarest word still in at least rareWordFloor of its items,
	// the rarest still in at least mediumWordFloor, and the word in most of them.
	RareWord   string
	MediumWord string
	CommonWord string
	// ProbeLexemes are the project's probeLexemeCount rarest words at or above rareWordFloor,
	// the subject the related-terms probe is measured on. That is the class production probes:
	// the picker hands the probe the words of an item's own text that its statistics do not
	// hold, after dropping the noise and the words in more than a tenth of the project
	// (pickRelatedTerms), so a probed word is one a few hundred items hold, never one of the
	// project's commonest. On the commonest the measurement says nothing: every arm matches
	// its first row at once, the probe reads tens of milliseconds whatever its plan is, and a
	// document arm that reads the whole table once per lexeme is invisible.
	ProbeLexemes []string
	// AnswersSince is the start of the seed's answers window by its one rule
	// (answerswindow.StartQuery): the since that every measurement of the agents' answers read
	// binds. It is read from the database, never now() less 72 hours, since the seed dates every
	// row in its own year.
	AnswersSince time.Time
	// AnswerSessions are the three sessions the answers read is measured for, chosen as the
	// seed's report chooses them (answerswindow.Read), in this order: the session with the most
	// deliveries before the window, and of the others the session with the most deliveries in the
	// window and the one with the fewest there, at least one.
	AnswerSessions []AnswerSession
}

// AnswerSession is one of the three sessions the agents' answers read is measured for, with the
// name its measurement carries.
type AnswerSession struct {
	// Name says which of the three the session is, as a pair's name says it.
	Name string
	answerswindow.Subject
}

// String is the session as the subjects' line records it: its name, its id and its deliveries.
func (s AnswerSession) String() string {
	return s.Name + ": " + s.Subject.String()
}

// rareWordFloor and mediumWordFloor are the item counts the two scarcer words, and rareWordFloor
// the probe's subject too, are taken above. A word in one or two items measures an arm that
// returns almost nothing, and a search's cost is in the rows it ranks; the floors keep each word
// in the class the measurement is about.
// probeLexemeCount is how many of the rarest words the probe's own subject holds.
const (
	rareWordFloor    = 100
	mediumWordFloor  = 1000
	probeLexemeCount = 20
)

// String is the one line a run records its subjects as. The query vector is given by its length
// alone: it is 1,536 numbers, and what identifies it is the issue it is the embedding of.
func (s Subjects) String() string {
	sessions := make([]string, 0, len(s.AnswerSessions))
	for _, session := range s.AnswerSessions {
		sessions = append(sessions, session.String())
	}
	return fmt.Sprintf(
		"project=%s issue=%s anchored_document=%s sessions=%s model=%s query_vector=%d bytes of %s's embedding words=%s/%s/%s probe_lexemes=%d answers_since=%s answer_sessions=[%s]",
		s.Project, s.Issue, s.AnchoredDocument, strings.Join(s.Sessions, ","),
		s.EmbeddingModel, len(s.QueryVector), s.Issue, s.RareWord, s.MediumWord, s.CommonWord,
		len(s.ProbeLexemes), s.AnswersSince.Format(time.RFC3339Nano), strings.Join(sessions, "; "))
}

// bindSubjects reads every subject. A subject the database does not hold fails the run with what
// was asked for: a measurement bound to a row that is not there measures nothing, and the
// failure must say which part of the seed is missing rather than surface later as an empty plan.
func bindSubjects(t *testing.T, conn *pgx.Conn) Subjects {
	t.Helper()
	ctx := context.Background()
	var subjects Subjects
	subjects.Project = one[string](t, conn, "the project holding the most issues", `
		select i.project_key from issues i group by i.project_key order by count(*) desc, i.project_key limit 1`)
	// Children and blockers together, which is how #2177 picked the subject its table measured.
	subjects.Issue = one[string](t, conn, "the most heavily linked issue of "+subjects.Project, `
		select i.key from issues i
		where i.project_key = $1
		order by (select count(*) from issues c where c.parent_key = i.key)
		       + (select count(*) from issue_links l where l.issue_key = i.key) desc, i.key
		limit 1`, subjects.Project)
	subjects.AnchoredDocument = one[string](t, conn, "the document carrying the most anchored comments", `
		select (c.anchor->>'artifact_id')::uuid::text
		from comments c
		where c.anchor is not null and c.anchor->>'artifact_id' is not null
		group by 1 order by count(*) desc limit 1`)
	rows, err := conn.Query(ctx, `
		select distinct e.actor->>'id'
		from events e where e.actor->>'kind' = 'session'
		order by 1 limit 5`)
	if err != nil {
		t.Fatalf("read the sessions the agents list reads: %v", err)
	}
	subjects.Sessions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the sessions the agents list reads: %v", err)
	}
	if len(subjects.Sessions) == 0 {
		t.Fatal("the database holds no event written by a session, so the agents list has no subject")
	}
	// The vector and its model come from the one embeddings row, read by its primary key, so the
	// model is the one that embedded this vector rather than whichever row a scan met first.
	if err := conn.QueryRow(ctx, `
		select e.embedding::text, e.model
		from embeddings e
		where e.kind = 'issue' and e.id = $1 and e.embedding is not null and e.model is not null`,
		subjects.Issue).Scan(&subjects.QueryVector, &subjects.EmbeddingModel); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s has no embedding, so no meaning arm has a query vector", subjects.Issue)
		}
		t.Fatalf("read %s's embedding: %v", subjects.Issue, err)
	}
	subjects.RareWord = word(t, conn, subjects.Project, "the rarest", `
		select s.lexeme from search_term_stats s
		where s.project_key = $1 and s.ndoc >= $2 order by s.ndoc, s.lexeme limit 1`, rareWordFloor)
	subjects.MediumWord = word(t, conn, subjects.Project, "a middling", `
		select s.lexeme from search_term_stats s
		where s.project_key = $1 and s.ndoc >= $2 order by s.ndoc, s.lexeme limit 1`, mediumWordFloor)
	subjects.CommonWord = word(t, conn, subjects.Project, "the commonest", `
		select s.lexeme from search_term_stats s
		where s.project_key = $1 and s.ndoc >= $2 order by s.ndoc desc, s.lexeme limit 1`, 0)
	probeRows, err := conn.Query(ctx, `
		select s.lexeme from search_term_stats s
		where s.project_key = $1 and s.ndoc >= $2 order by s.ndoc, s.lexeme limit $3`,
		subjects.Project, rareWordFloor, probeLexemeCount)
	if err != nil {
		t.Fatalf("read %s's rarest words: %v", subjects.Project, err)
	}
	subjects.ProbeLexemes, err = pgx.CollectRows(probeRows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s's rarest words: %v", subjects.Project, err)
	}
	if len(subjects.ProbeLexemes) < probeLexemeCount {
		t.Fatalf("%s holds %d words in at least %d of its items, and the probe's subject is %d: the term statistics of this database are not the seed's",
			subjects.Project, len(subjects.ProbeLexemes), rareWordFloor, probeLexemeCount)
	}
	subjects.AnswersSince, subjects.AnswerSessions = answerSubjects(t, conn)
	return subjects
}

// answerSubjects reads the agents' answers read's subjects by the rule the seed's report prints
// them by (answerswindow.Read): the start of the window and its three sessions. A database that
// holds no delivery, or too few sessions for the three, is one seeded before the seed wrote
// deliveries, and the run fails, as for every other subject, rather than measure a read that
// lists nothing.
func answerSubjects(t *testing.T, conn *pgx.Conn) (time.Time, []AnswerSession) {
	t.Helper()
	answers, err := answerswindow.Read(context.Background(), conn)
	if err != nil {
		t.Fatalf("read the agents' answers read's window and sessions: %v", err)
	}
	if answers.Since.IsZero() {
		t.Fatal("the database holds no event that delivers a person's words to a session, so the agents' answers read has no window: " +
			"it was seeded before cmd/dispatch-seed wrote deliveries, or by another tool; seed it again with this checkout's cmd/dispatch-seed")
	}
	sessions := []AnswerSession{
		{Name: "the session with old deliveries", Subject: answers.Old},
		{Name: "the session with the most deliveries in the window", Subject: answers.Most},
		{Name: "the session with the fewest deliveries in the window", Subject: answers.Fewest},
	}
	for _, session := range sessions {
		if session.Session == "" {
			t.Fatalf("%s for the agents' answers read is missing: the database was seeded before cmd/dispatch-seed wrote deliveries, or at a scale too small for three sessions; seed it again with this checkout's cmd/dispatch-seed",
				session.Name)
		}
	}
	return answers.Since, sessions
}

// word reads one of the three frequencies. A project whose term statistics are not built has no
// row at any floor, which is a seeded database the refresh never ran on: the measurement would
// then search a word of its own choosing, so the run fails instead.
func word(t *testing.T, conn *pgx.Conn, project, which, statement string, floor int) string {
	t.Helper()
	return one[string](t, conn,
		fmt.Sprintf("%s word of %s, one in at least %d of its items", which, project, floor),
		statement, project, floor)
}

// one reads a subject that is exactly one value. A statement answering no row names the subject
// it was looking for.
func one[T any](t *testing.T, conn *pgx.Conn, what, statement string, args ...any) T {
	t.Helper()
	var value T
	if err := conn.QueryRow(context.Background(), statement, args...).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("the database answers no row for %s; is it the database cmd/dispatch-seed filled?", what)
		}
		t.Fatalf("read %s: %v", what, err)
	}
	return value
}
