package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// definitions.go serves Define for a person (INFRA-3231's Decision 24, INFRA-3552's R4): what
// words the person selected mean where the text uses them.
//
//   - POST /api/v1/definitions answers at once with the definition read from Dispatch's record
//     (the component's Define, one model call). Words in an ask, a decision block's included, also
//     send the session that asked it one side question, what it means by those words, when Envoy
//     reads that session live and its host advertises btw: a listener frame from Dispatch whose
//     message id is the definition's own id, with no stored message and no event. The session's
//     answer comes to POST /api/v1/messages/{id}/reply under that id (answerDefinitionFrame),
//     which keeps it with the definition, writes no message row and no event, and takes it for
//     the side question's wait from the definition's time. Words in a document of an issue or a
//     project get the record's definition alone.
//   - GET /api/v1/definitions/{id} reads one of the caller's own definitions, the agent's answer
//     with it once it came; GET /api/v1/me/definitions lists the caller's own, one page of them
//     (list_page.go). No person reads another's: a definition is kept for the person who asked
//     for it.
//
// A definition changes nothing in the record: it appends no event on the issue or the document,
// whose events any signed-in caller reads. Each Define use is that person's question about those
// words in R8's records (store.RecordDefineUse).

// The bounds of a Define request: how many selections it names, and how long each may be.
const (
	defineSelectionsMax = 20
	defineSelectionMax  = 500
)

// The states of a definition's side question (definitionAgent.State).
const (
	// definitionAgentNone is a definition that sent no side question: words in a document, an ask
	// no session opened, or a session Envoy did not read live with a host that takes btw.
	definitionAgentNone = "none"
	// definitionAgentWaiting is a side question whose answer may still come, until Until.
	definitionAgentWaiting = "waiting"
	// definitionAgentAnswered is a side question the session answered in time.
	definitionAgentAnswered = "answered"
	// definitionAgentUnanswered is a side question no answer came to in time.
	definitionAgentUnanswered = "unanswered"
)

// defineInput is POST /api/v1/definitions's body: where the words are, an ask or a document, and
// the words.
type defineInput struct {
	// AskID is the ask the words are in, a decision block's included.
	AskID string `json:"ask_id"`
	// Document is the document the words are in, of an issue or a project, read at its latest
	// version; set it or AskID, never both.
	Document string `json:"document"`
	// Words are the person's selections, each the words as the text holds them.
	Words []string `json:"words" required:"true"`
}

// definitionAgent is what the asking session answered to a definition's side question.
type definitionAgent struct {
	// State is none, waiting, answered or unanswered.
	State string `json:"state"`
	// Session is the session the side question went to, "" for none.
	Session string `json:"session,omitempty"`
	// Question is the side question Dispatch sent, and Answer the session's answer in its own
	// words, at AnsweredAt.
	Question   string     `json:"question,omitempty"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
	// Until is when the side question's wait ends, for a side question sent.
	Until *time.Time `json:"until,omitempty"`
}

// definitionRead is one definition as its person reads it: where the words were (an ask, or a
// document at a version), the words, the parts of Dispatch's record it read, what the record
// says they mean (each {words, meaning, links}), why the model gave none where it gave none, the
// asking session's answer, and each model call.
type definitionRead struct {
	ID          string                `json:"id"`
	Person      string                `json:"person"`
	AskID       *string               `json:"ask_id"`
	ArtifactID  *string               `json:"artifact_id"`
	Version     *int                  `json:"version"`
	Words       []string              `json:"words"`
	Inputs      []string              `json:"inputs"`
	Definitions []reviewer.Definition `json:"definitions"`
	Failure     string                `json:"failure,omitempty"`
	Agent       definitionAgent       `json:"agent"`
	Calls       []reviewerCall        `json:"calls"`
	CreatedAt   time.Time             `json:"created_at"`
}

// definitionsPage is GET /api/v1/me/definitions's answer: one page of the caller's definitions,
// the newest first, and how many they have.
type definitionsPage struct {
	Items  []definitionRead `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// definitionAnswerRead is what POST /api/v1/messages/{id}/reply answers a session's answer to a
// definition's side question: the definition's id and the side question's state. The definition
// itself is its person's alone.
type definitionAnswerRead struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// defineSubject is where a Define's words are: the record's subject, the agent text its R8 record
// names ("ask:<id>", "version:<document id>:<number>"), its project, and the ask or the document.
type defineSubject struct {
	subject    reviewer.Subject
	answers    string
	project    string
	askID      *string
	artifactID *string
	version    *int
}

// createDefinition is POST /api/v1/definitions.
func (s *server) createDefinition(w http.ResponseWriter, r *http.Request) {
	person, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input defineInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.RewriteDefine == nil {
		writeError(w, "DEFINE_NO_MODEL", http.StatusServiceUnavailable,
			"this deployment sets no model group for Rewrite and Define (DISPATCH_REWRITE_DEFINE_MODEL_*)")
		return
	}
	rd := s.deps.RewriteDefine
	chosen, err := s.requestModel(r, rd.config.Model)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words, err := defineWords(input.Words)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx := r.Context()
	where, err := s.defineSubject(ctx, input, person)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	record, err := reviewer.BuildRecord(ctx, s.deps.Store, where.subject)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defined, err := reviewer.Define(ctx, chosen, rd.config.Rules, record, words, reviewer.Answer{})
	failure := ""
	var failed *reviewer.ModelError
	switch {
	case errors.As(err, &failed):
		failure = "the model gave no definition: " + modelFailure(failed)
	case err != nil:
		s.writeHandlerError(w, err)
		return
	}
	definitions := defined.Definitions
	if definitions == nil {
		definitions = []reviewer.Definition{}
	}
	id := uuid.NewString()
	writer, question := "", ""
	if where.askID != nil && s.sideQuestionSession(ctx, where.subject.Writer, true) {
		if question, err = reviewer.DefineQuestion(s.askAddress(ctx, *where.askID), where.subject.Text.Kind, words); err != nil {
			slog.Warn("dispatch: write a definition's side question", "ask", *where.askID, "error", err)
			question = ""
		} else {
			writer = where.subject.Writer
		}
	}
	calls := callsOf(defined.Calls)
	encodedDefinitions, err := encodeJSON(definitions)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	encodedCalls, err := encodeJSON(calls)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	inputs := recordInputs(record)
	// The definition is the request's own write, so it commits in a transaction of the shared
	// pool: the commit step marks the request's Idempotency-Key committed with it, and a repeat
	// after a crash is answered KEY_COMMITTED rather than defining the words a second time
	// (keyed_write_guard_test.go).
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into definitions (id, person, ask_id, artifact_id, version, project, words, inputs, definitions,
			failure, calls, writer, agent_question)
		values ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, nullif($10, ''), $11, nullif($12, ''), nullif($13, ''))`,
		id, person.ID, where.askID, where.artifactID, where.version, where.project, words, inputs, encodedDefinitions,
		failure, encodedCalls, writer, question); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.recordDefineUse(ctx, id, person.ID, where, words, defined.Calls)
	if writer != "" {
		frame := systemFrame{
			id: id, body: question, writer: writer, key: "definition:" + id, mode: "btw", expects: contracts.ExpectsReplyRequired,
		}
		if err := s.sendSystemFrame(ctx, frame); err != nil {
			slog.Warn("dispatch: send a definition's side question", "definition", id, "session", writer, "error", err)
			if err := s.recordSideQuestionNotSent(ctx, id); err != nil {
				slog.Warn("dispatch: record a side question not sent", "definition", id, "error", err)
			}
		}
	}
	read, err := s.loadDefinition(ctx, id, person.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, read)
}

// recordSideQuestionNotSent records that the definition id sent no side question after all: the
// frame did not reach the session. It is the request's own write, so it commits in a transaction
// of the shared pool, whose commit step marks the request's key with it, as the definition's
// insert did.
func (s *server) recordSideQuestionNotSent(ctx context.Context, id string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update definitions set writer = null, agent_question = null where id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// defineWords is a Define's selections, each trimmed, blank ones dropped, within the bounds.
func defineWords(selected []string) ([]string, error) {
	words := make([]string, 0, len(selected))
	for _, word := range selected {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		if utf8.RuneCountInString(word) > defineSelectionMax {
			return nil, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "each selection is at most %d characters", defineSelectionMax)
		}
		words = append(words, word)
	}
	switch {
	case len(words) == 0:
		return nil, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "words names no words to define")
	case len(words) > defineSelectionsMax:
		return nil, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "words names at most %d selections", defineSelectionsMax)
	}
	return words, nil
}

// defineSubject reads where input's words are: an ask, whose record a rewrite reads too
// (reviewer.AskSubject), or a document of an issue or a project at its latest version, whose
// record is the document and its issue or project. The person reads the definition, so their
// recorded words are the record's reader's.
func (s *server) defineSubject(ctx context.Context, input defineInput, person model.Actor) (defineSubject, error) {
	askID, document := strings.TrimSpace(input.AskID), strings.TrimSpace(input.Document)
	if (askID == "") == (document == "") {
		return defineSubject{}, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "name the words' ask in ask_id or their document in document, one of the two")
	}
	if askID != "" {
		if _, err := uuid.Parse(askID); err != nil {
			return defineSubject{}, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "ask_id is not an ask's id")
		}
		subject, err := reviewer.AskSubject(ctx, s.deps.Store, askID)
		if errors.Is(err, pgx.ErrNoRows) {
			return defineSubject{}, errorf(http.StatusNotFound, "NOT_FOUND", "no ask has the id %s", askID)
		}
		if err != nil {
			return defineSubject{}, err
		}
		subject.Reader = person.ID
		ask, err := s.loadAsk(ctx, s.deps.Store.Pool, askID)
		if err != nil {
			return defineSubject{}, err
		}
		return defineSubject{
			subject: subject, answers: "ask:" + askID, project: s.askProject(ctx, s.deps.Store.Pool, ask), askID: &askID,
		}, nil
	}
	if _, err := uuid.Parse(document); err != nil {
		return defineSubject{}, errorf(http.StatusBadRequest, "DEFINITION_INPUT", "document is not a document's id")
	}
	var (
		kind, project, markdown string
		agentOwned              bool
		version                 *int
	)
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select a.kind, a.session_id is not null,
			coalesce((select i.project_key from issues i where i.key = a.issue_key), a.project_key, ''),
			v.number, coalesce(v.markdown, '')
		from artifacts a
		left join lateral (
			select number, markdown from artifact_versions where artifact_id = a.id order by number desc limit 1
		) v on true
		where a.id = $1::uuid`, document).Scan(&kind, &agentOwned, &project, &version, &markdown)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return defineSubject{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	case err != nil:
		return defineSubject{}, err
	case kind != "doc" || agentOwned || version == nil:
		return defineSubject{}, errorf(http.StatusBadRequest, "DEFINITION_INPUT",
			"document is not a document of an issue or a project: a file, an image or an agent conversation's upload has no words to define")
	}
	subject := reviewer.Subject{
		Text:       reviewer.Text{Kind: reviewer.KindDocument, Body: markdown},
		ID:         document,
		DocumentID: document,
		Quote:      firstWords(input.Words),
		Reader:     person.ID,
	}
	return defineSubject{
		subject: subject, answers: fmt.Sprintf("version:%s:%d", document, *version), project: project,
		artifactID: &document, version: version,
	}, nil
}

// firstWords is the first selection that is not blank, the place in a document its record reads
// around.
func firstWords(selected []string) string {
	for _, word := range selected {
		if word = strings.TrimSpace(word); word != "" {
			return word
		}
	}
	return ""
}

// recordInputs names the parts of Dispatch's record a definition read: the text, and each other
// part the record held.
func recordInputs(record reviewer.Record) []string {
	inputs := []string{"text"}
	if len(record.Thread) > 0 {
		inputs = append(inputs, "thread")
	}
	if record.Document != nil && !(record.Document.Holds && record.Text.Kind == reviewer.KindDocument) {
		inputs = append(inputs, "document:"+record.Document.Name)
	}
	if record.IssueKey != "" {
		inputs = append(inputs, "issue:"+record.IssueKey)
	}
	if record.Project != "" {
		inputs = append(inputs, "project:"+record.Project)
	}
	if record.Decisions != "" {
		inputs = append(inputs, "decisions")
	}
	if len(record.WriterTexts) > 0 {
		inputs = append(inputs, "writer_texts")
	}
	if len(record.ShownReaderWords()) > 0 {
		inputs = append(inputs, "reader_words")
	}
	return inputs
}

// recordDefineUse records the Define use as person's question about words (R8): one
// word_questions row with source define, the model that served it named.
func (s *server) recordDefineUse(ctx context.Context, id, person string, where defineSubject, words []string, calls []reviewer.Call) {
	served := ""
	for _, call := range calls {
		if call.Purpose == reviewer.PurposeDefine {
			served = call.Served
			if served == "" {
				served = call.Deployment
			}
		}
	}
	if err := s.deps.Store.RecordDefineUse(ctx, store.DefineUse{
		DefinitionID: id, Person: person, Project: where.project, Words: words, Answers: where.answers, Model: served,
	}); err != nil {
		slog.Warn("dispatch: record a Define use", "definition", id, "error", err)
	}
}

// definitionColumns are the columns scanDefinition reads, in its order; $2 is the side
// question's wait.
const definitionColumns = `id::text, person, ask_id::text, artifact_id::text, version, words, inputs, definitions,
	coalesce(failure, ''), calls, coalesce(writer, ''), coalesce(agent_question, ''), coalesce(agent_answer, ''),
	agent_answered_at, created_at, created_at + $2::interval, created_at + $2::interval > now()`

// scanDefinition reads one row of definitionColumns, and into after each column the select names
// past them.
func scanDefinition(row pgx.Row, after ...any) (definitionRead, error) {
	var (
		read               definitionRead
		definitions, calls []byte
		until              time.Time
		open               bool
	)
	columns := []any{&read.ID, &read.Person, &read.AskID, &read.ArtifactID, &read.Version, &read.Words, &read.Inputs,
		&definitions, &read.Failure, &calls, &read.Agent.Session, &read.Agent.Question, &read.Agent.Answer,
		&read.Agent.AnsweredAt, &read.CreatedAt, &until, &open}
	if err := row.Scan(append(columns, after...)...); err != nil {
		return definitionRead{}, err
	}
	if err := json.Unmarshal(definitions, &read.Definitions); err != nil {
		return definitionRead{}, fmt.Errorf("read definition %s: %w", read.ID, err)
	}
	if err := json.Unmarshal(calls, &read.Calls); err != nil {
		return definitionRead{}, fmt.Errorf("read definition %s's calls: %w", read.ID, err)
	}
	switch {
	case read.Agent.Question == "":
		read.Agent.State = definitionAgentNone
	case read.Agent.Answer != "":
		read.Agent.State = definitionAgentAnswered
	case open:
		read.Agent.State = definitionAgentWaiting
	default:
		read.Agent.State = definitionAgentUnanswered
	}
	if read.Agent.Question != "" {
		until = until.UTC()
		read.Agent.Until = &until
	}
	return read, nil
}

// loadDefinition reads person's definition id; another person's, or none, is 404
// DEFINITION_NOT_FOUND.
func (s *server) loadDefinition(ctx context.Context, id, person string) (definitionRead, error) {
	read, err := scanDefinition(s.deps.Store.Pool.QueryRow(ctx, `
		select `+definitionColumns+` from definitions where id = $1::uuid and person = $3`,
		id, intervalOf(s.deps.RewriteDefine.sideQuestionWait()), person))
	if errors.Is(err, pgx.ErrNoRows) {
		return definitionRead{}, errorf(http.StatusNotFound, "DEFINITION_NOT_FOUND", "you have no definition with the id %s", id)
	}
	return read, err
}

// getDefinition is GET /api/v1/definitions/{id}: one of the caller's own definitions.
func (s *server) getDefinition(w http.ResponseWriter, r *http.Request) {
	person, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		writeError(w, "DEFINITION_NOT_FOUND", http.StatusNotFound, "a definition's id is a uuid")
		return
	}
	read, err := s.loadDefinition(r.Context(), r.PathValue("id"), person.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// listMyDefinitions is GET /api/v1/me/definitions: one page of the caller's own definitions, the
// newest first, and how many they have.
func (s *server) listMyDefinitions(w http.ResponseWriter, r *http.Request) {
	person, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	page, err := parseListPage(r.URL.Query())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx := r.Context()
	// The count over every definition rides on each row of the page, so the page and its total
	// are one reading. A page past the last definition holds no row to carry it, and is counted
	// on its own.
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+definitionColumns+`, count(*) over () from definitions where person = $1
		order by created_at desc, id desc limit $3 offset $4`,
		person.ID, intervalOf(s.deps.RewriteDefine.sideQuestionWait()), page.limit, page.offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	answer := definitionsPage{Items: []definitionRead{}, Limit: page.limit, Offset: page.offset}
	for rows.Next() {
		read, err := scanDefinition(rows, &answer.Total)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		answer.Items = append(answer.Items, read)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(answer.Items) == 0 && page.offset > 0 {
		if err := s.deps.Store.Pool.QueryRow(ctx, `select count(*) from definitions where person = $1`,
			person.ID).Scan(&answer.Total); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, answer)
}

// answerDefinitionFrame takes a reply to a definition's side question (createDefinition), which
// names the definition's id where a delivered message's id stands: the reply is the asking
// session's answer, kept with the definition, and writes no message row and no event. It reports
// whether id is a definition's, having answered the request. Only the session the side question
// went to answers it, once, within the side question's wait; an error report records nothing.
func (s *server) answerDefinitionFrame(w http.ResponseWriter, r *http.Request, actor model.Actor, input messageReplyInput) bool {
	if s.deps.Store == nil {
		return false
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		return false
	}
	ctx := r.Context()
	var (
		writer, question, answer string
		open                     bool
	)
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(writer, ''), coalesce(agent_question, ''), coalesce(agent_answer, ''),
			created_at + $2::interval > now()
		from definitions where id = $1::uuid`, id, intervalOf(s.deps.RewriteDefine.sideQuestionWait())).Scan(&writer, &question, &answer, &open)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	switch {
	case question == "" || writer != actor.ID:
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "only the session a definition's side question went to answers it")
		return true
	case answer != "":
		WriteJSON(w, http.StatusAccepted, definitionAnswerRead{ID: id, State: definitionAgentAnswered})
		return true
	case !open:
		writeError(w, "DEFINITION_ANSWER_LATE", http.StatusConflict,
			"the side question's wait has ended: a definition takes the agent's answer for "+s.deps.RewriteDefine.sideQuestionWait().String())
		return true
	case input.Body == nil:
		// An error report is no answer: the definition stays with the record's alone.
		WriteJSON(w, http.StatusAccepted, definitionAnswerRead{ID: id, State: definitionAgentWaiting})
		return true
	}
	// The answer is the request's own write: it commits in a transaction of the shared pool, whose
	// commit step marks the request's key with it (keyed_write_guard_test.go).
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		update definitions set agent_answer = $2, agent_answered_at = now()
		where id = $1::uuid and agent_answer is null`, id, *input.Body); err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return true
	}
	WriteJSON(w, http.StatusAccepted, definitionAnswerRead{ID: id, State: definitionAgentAnswered})
	return true
}
