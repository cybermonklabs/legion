package api

// going_undo.go undoes one run of the command bar (R6): POST /api/v1/going/runs/{id}/undo walks
// the run's landed calls - each call of the run that landed a request (going_landings, phase do),
// with the row of the run's plan it ran - newest first, and undoes each as the credential that
// pressed Undo, through the server's own route table in-process (going_writes.go), by the undo
// the row's tool states (goingWriteTool.Undo):
//
//   - write_back reads the item's value now (going_observe.go) and writes back the value the call
//     found, field by field, only where the value now is the one the call left; a later value
//     another writer left is kept and named, with who wrote the item's newest event;
//   - reverse calls the row's reverse route (goingToolRoute.UndoRoutes): a claim released or taken
//     back, a follow unfollowed, a resolved thread reopened and a reopened one resolved again, an
//     opened ask retracted, and a suggestion of the run that is still open rejected;
//   - close_created closes the issue the call created, with no message and no comment;
//   - not_undone names the call "sent, not undone": a message, a broadcast, a review.
//
// What a call found and left is read off what its route recorded beside its landing, in the
// transaction of the change (goingRecordAnswer), never off a read made since, so a run whose
// process ended after a write landed is undone from that record. An undo sends one request at the
// most, and that request lands in going_landings under the key of the call it undoes, phase undo,
// in its route's own transaction, with the line the undo answers (goingUndoLanding): a second
// press, after a crash or beside the first, answers each call whose undo landed from that landing
// and sends nothing twice.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingUndoReason is the note an undo's retraction of an ask and a document's restoring version
// carry.
const goingUndoReason = "Undone from the command bar."

// goingSentNotUndone is what an undo says of a call it does not undo: a message, a broadcast, a
// review, or any other write that reached a person or a session, or that no route takes back.
const goingSentNotUndone = "sent, not undone"

// The undo's own refusals of one call.
const (
	// codeGoingUndoChanged is a value another writer left after the run: it is kept, and named.
	codeGoingUndoChanged = "GOING_UNDO_CHANGED"
	// codeGoingUndoNotCallers is a release of a claim the undo cannot take back for its holder:
	// the claim it would make is recorded as its own claimant (goingUndoer.claimant), not as the
	// holder the release released, so taking it would give the claim to one who never held it.
	codeGoingUndoNotCallers = "GOING_UNDO_NOT_CALLERS_CLAIM"
	// codeGoingUndoUnknown is a landed call that names no row of the run's plan, or whose tool
	// the table no longer holds: no row states its undo, so none is tried.
	codeGoingUndoUnknown = "GOING_UNDO_UNKNOWN"
	// codeGoingUndoUnserved is a call whose tool states an undo this server has no way to run.
	codeGoingUndoUnserved = "GOING_UNDO_UNSERVED"
	// codeGoingUndoAfterUnread is a call whose route recorded nothing that says what it left:
	// there is nothing to hold the value now to, so nothing is put back.
	codeGoingUndoAfterUnread = "GOING_UNDO_AFTER_UNREAD"
	// codeGoingUndoBeforeUnread is a call whose route answered that it found a value the read its
	// row was approved over did not see: what the call replaced is unknown, so nothing is put back.
	codeGoingUndoBeforeUnread = "GOING_UNDO_BEFORE_UNREAD"
)

// goingUndoLine is how the undo of one landed call went: the plan and row of the run the call ran,
// its tool, its item, the undo its tool states, the outcome, and how each field of the call went,
// by the field's name.
type goingUndoLine struct {
	Plan    int               `json:"plan"`
	Row     int               `json:"row"`
	Tool    string            `json:"tool"`
	Item    string            `json:"item"`
	Undo    goingUndoKind     `json:"undo" enum:"reverse,write_back,close_created,not_undone"`
	Outcome goingWriteOutcome `json:"outcome"`
	Fields  []goingUndoField  `json:"fields"`
}

// goingUndoField is how the undo of one field of a call went: put back (ok), or kept as another
// writer left it, with what it holds now and who wrote it named in message.
type goingUndoField struct {
	Field   string `json:"field"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// goingUndoAnswer is what an undo answers: one line for each landed call of the run, newest first.
type goingUndoAnswer struct {
	Lines []goingUndoLine `json:"lines"`
}

// undoGoingRun is POST /api/v1/going/runs/{id}/undo, made as the credential that pressed it. A
// run that is not the caller's, that the store does not hold, or that History no longer lists -
// its conversation's newest run is older than goingRunsKept - is 404 GOING_RUN_NOT_FOUND. Each
// landed call of the run is undone in turn, newest first (goingUndoer.undo), and the answer holds
// one line for each.
func (s *server) undoGoingRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeGoingRunNotFound(w)
		return
	}
	run := id.String()
	listed, err := s.goingRunInHistory(r.Context(), run, caller.ID)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the going run: %w", err))
		return
	}
	if !listed {
		writeGoingRunNotFound(w)
		return
	}
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	writes, err := s.goingLandedWrites(r.Context(), run)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	doer := s.goingDoer(goingBarCaller(actor, human, run), PublicOrigin(s.deps.ServerURL, r))
	undoer := goingUndoer{s: s, doer: doer, observer: goingObserver{read: goingReader{doer: doer}, s: s},
		login: caller.ID, session: goingCredentialOf(r, actor, false).session()}
	lines := make([]goingUndoLine, 0, len(writes))
	for _, write := range writes {
		lines = append(lines, undoer.undo(r.Context(), write))
	}
	WriteJSON(w, http.StatusOK, goingUndoAnswer{Lines: lines})
}

// goingRunInHistory reports whether run is login's and History lists it: its conversation's newest
// run is younger than goingRunsKept (goingConversationListed).
func (s *server) goingRunInHistory(ctx context.Context, run, login string) (bool, error) {
	var listed bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select exists (select 1 from going_runs r where r.id = $1::uuid and r.author_login = $2 and `+goingConversationListed(3)+`)
	`, run, login, goingRunsKept.Seconds()).Scan(&listed)
	return listed, err
}

// goingLandedWrite is one call of a run that landed a request, as the undo reads it: the call's
// key; the row of the run's plan it ran - its plan, its number, its tool and arguments, and the
// read of its item the row was approved over (goingRowApproval.Seen), nil for none; and each
// request it landed, in order.
type goingLandedWrite struct {
	key       goingCallKey
	plan, row int
	tool      string
	arguments map[string]any
	seen      *goingRowShown
	requests  []goingLandedRequest
}

// goingLandedWrites is every call of run that landed a request (going_landings, phase do), newest
// first, each with the row of the run's plan whose key it is (going_plan_rows.result): the row
// records the key that runs it before its first request is sent, so a call whose process ended
// after it landed is among them, whatever its row says of how it went; and it keeps the key of an
// earlier try whose call landed partly before the row waited again (goingRowRecord.Resumes), so
// that try's landings stay the row's whatever the person answered the row with - unticked, or
// approved again with other values, which the next try sends anew - until a try of the row takes
// them over (adoptLandings).
func (s *server) goingLandedWrites(ctx context.Context, run string) ([]goingLandedWrite, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select l.step, l.call, l.method, l.path, l.body_digest, coalesce((l.answer->>'status')::int, 0), l.answer->'body',
		  l.recovery, p.plan, p.row, coalesce(p.change->>'tool', ''), p.change->'arguments', p.approval
		from going_landings l
		left join going_plan_rows p on p.run_id = l.run_id
		  and ((p.result->>'step')::int = l.step and (p.result->>'call')::int = l.call
		       or p.result->'resumes' @> jsonb_build_array(jsonb_build_object('step', l.step, 'call', l.call)))
		where l.run_id = $1::uuid and l.phase = 'do'
		order by l.step desc, l.call desc, l.request
	`, run)
	if err != nil {
		return nil, fmt.Errorf("read the landings of run %s: %w", run, err)
	}
	defer rows.Close()
	var writes []goingLandedWrite
	for rows.Next() {
		key := goingCallKey{run: run}
		var request goingLandedRequest
		var body, recovery, arguments, approval []byte
		var plan, row *int
		var tool string
		if err := rows.Scan(&key.step, &key.call, &request.method, &request.path, &request.digest, &request.status, &body,
			&recovery, &plan, &row, &tool, &arguments, &approval); err != nil {
			return nil, fmt.Errorf("read the landings of run %s: %w", run, err)
		}
		request.body, request.recovery = body, recovery
		if len(writes) == 0 || writes[len(writes)-1].key != key {
			write := goingLandedWrite{key: key, tool: tool}
			if plan != nil && row != nil {
				write.plan, write.row = *plan, *row
			}
			if write.arguments, err = goingRowArguments(arguments); err != nil {
				return nil, fmt.Errorf("read row %d of plan %d of run %s: %w", write.row, write.plan, run, err)
			}
			if approval != nil {
				var approved goingRowApproval
				if err := json.Unmarshal(approval, &approved); err != nil {
					return nil, fmt.Errorf("read the approval of row %d of plan %d of run %s: %w", write.row, write.plan, run, err)
				}
				write.seen = &approved.Seen
			}
			writes = append(writes, write)
		}
		writes[len(writes)-1].requests = append(writes[len(writes)-1].requests, request)
	}
	return writes, rows.Err()
}

// goingUndoer undoes the landed calls of one run as the credential that pressed Undo: its person
// (login), and the session that credential proved, "" for none.
type goingUndoer struct {
	s        *server
	doer     GoingDoer
	observer goingObserver
	login    string
	session  string
}

// goingUndoSend is the one request an undo of a call sends: through route, its placeholders filled
// from values, with body (nil for none) and condition (nil for none), and the line the undo
// answers once it lands (landed). changed are the refusals of the route that say another writer
// changed the item between the undo's read and its request.
type goingUndoSend struct {
	route     string
	values    map[string]string
	body      any
	condition *goingCondition
	landed    goingUndoLine
	changed   []string
}

// undo undoes one landed call by the undo its tool states. A call whose undo landed before - an
// earlier press of Undo, or one made beside this one - is answered from that landing, and nothing
// is sent. A call that names no row of the run's plan, or whose tool the table no longer holds, is
// refused, 409 GOING_UNDO_UNKNOWN, and a kind this server has no way to run for the tool, 501
// GOING_UNDO_UNSERVED. A call of which only the requests it sends before its own landed - a close's
// reason, a message - is named sent, not undone: no undo takes a message back, and nothing else of
// the change stands. A call whose route recorded nothing that says what it found and left is
// refused, 409 GOING_UNDO_AFTER_UNREAD, and one whose route found a value the row's read did not
// see, 409 GOING_UNDO_BEFORE_UNREAD: nothing is put back.
func (u goingUndoer) undo(ctx context.Context, write goingLandedWrite) goingUndoLine {
	line := goingUndoLine{Plan: write.plan, Row: write.row, Tool: write.tool, Undo: goingUndoNotUndone, Item: u.landedItem(write)}
	tool, held := goingWriteTools[write.tool]
	if !held {
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoUnknown,
			Message: "this call names no write tool of the run's plan, so its undo is unknown and nothing was undone"}
		return goingLineOf(line, nil)
	}
	line.Undo = tool.Undo
	if landed, err := u.s.goingUndoLanded(ctx, write.key); err != nil || landed != nil {
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, nil)
		}
		return *landed
	}
	if tool.Undo == goingUndoNotUndone {
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingSentNotUndone}
		return goingLineOf(line, nil)
	}
	if last := write.requests[len(write.requests)-1]; !goingOwnLanded(u.s.going, write.tool, last) {
		line.Undo = goingUndoNotUndone
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingSentNotUndone}
		// The line names the row's item, not the message that landed.
		if write.seen != nil && write.seen.Item != "" {
			line.Item = write.seen.Item
		} else if issue := goingIssueOfPath(last.path); issue != "" {
			line.Item = issue
		}
		return goingLineOf(line, nil)
	}
	var send *goingUndoSend
	switch {
	case write.tool == "dispatch_suggest":
		line, send = u.suggestion(ctx, write, line)
	case write.tool == "dispatch_doc_edit":
		line, send = u.document(ctx, write, line)
	default:
		undone, err := u.recorded(ctx, write)
		line.Item = cmp.Or(undone.target, line.Item)
		switch {
		case err != nil:
			slog.Warn("dispatch: read what a command-bar change left off its record", "run", write.key.run, "tool", write.tool, "error", err)
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
				Message: "what this change left cannot be read off its record, so nothing was put back: read the item, and change it by hand to undo this change"}
			return goingLineOf(line, undone.fields)
		case undone.beforeUnread:
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoBeforeUnread,
				Message: "another writer changed the item between the read the change was approved over and the change, so what it replaced is unknown and nothing was put back: read the item to see what it holds"}
			return goingLineOf(line, undone.fields)
		}
		line, send = u.kind(ctx, line, undone)
	}
	if send == nil {
		return line
	}
	return u.send(ctx, write.key, *send)
}

// kind is the undo of a call by the undo its tool states, from what the call found and left: the
// line it answers where it sends nothing, or the request it sends.
func (u goingUndoer) kind(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	switch {
	case line.Undo == goingUndoWriteBack && undone.tool == "dispatch_issue_update":
		return u.issueFields(ctx, line, undone)
	case line.Undo == goingUndoWriteBack && undone.tool == "dispatch_edit_ask":
		return u.askFields(ctx, line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_claim":
		return u.claim(ctx, line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_follow":
		return u.follow(ctx, line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_resolve_comment":
		return u.comment(ctx, line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_reopen_comment":
		return u.reopened(ctx, line, undone)
	case line.Undo == goingUndoReverse && (undone.tool == "dispatch_ask" || undone.tool == "dispatch_request_approval"):
		return u.retract(ctx, line, undone)
	case line.Undo == goingUndoCloseCreated && undone.tool == "dispatch_issue":
		return u.closeCreated(ctx, line, undone)
	}
	line.Outcome = goingWriteOutcome{Status: http.StatusNotImplemented, Code: codeGoingUndoUnserved,
		Message: "Dispatch has no way to run this change's undo, so nothing was undone"}
	return goingLineOf(line, undone.fields), nil
}

// goingLineOf is line with one field outcome, the line's own, for each of fields, or for the one
// unnamed field of a call that names none.
func goingLineOf(line goingUndoLine, fields []string) goingUndoLine {
	if len(fields) == 0 {
		fields = []string{""}
	}
	line.Fields = make([]goingUndoField, 0, len(fields))
	for _, field := range fields {
		line.Fields = append(line.Fields, goingUndoField{Field: field, OK: line.Outcome.OK, Message: line.Outcome.Message})
	}
	return line
}

// landedItem is the item a landed call names before anything is read of it: what its last request
// made, as its route answered it, else the last item its path names.
func (u goingUndoer) landedItem(write goingLandedWrite) string {
	last := write.requests[len(write.requests)-1]
	if made := goingMadeID(last.body); made != "" {
		return made
	}
	if prepared, held := goingRecordedCall(u.s.going, write.tool, goingWriteRequest{Method: last.method, Path: last.path}); held {
		return goingTargetOfPath(prepared.call.path)
	}
	return ""
}

// send makes the one request of an undo, in-process as the caller, carrying its landing
// (goingUndoLanding), which records it and the line it answers in the route's own transaction. A
// request answered below 300 landed, and answers that line. One refused may have landed all the
// same - its route committed and then answered 5xx, or another press's request of the same undo
// landed first, which rolls this one back - as the call's undo landing says: then it answers that
// landing's line. Otherwise nothing of it landed: the line is the route's refusal, a refusal
// `changed` names being another writer's change since the undo's read, and every field the undo
// would have put back is left with it.
func (u goingUndoer) send(ctx context.Context, key goingCallKey, send goingUndoSend) goingUndoLine {
	line := send.landed
	method, pattern, _ := strings.Cut(send.route, " ")
	path := goingPath(pattern, send.values)
	spec := goingSend{method: method, path: path}
	if send.body != nil {
		encoded, err := goingJSONRequest(method, path, send.body)
		if err != nil {
			return goingRefusedLine(line, goingFailed(err))
		}
		spec = encoded
	}
	spec.condition = send.condition
	landing := goingUndoLanding{key: key, method: spec.method, path: spec.path, digest: spec.digest(), line: line}
	request, err := spec.build(context.WithValue(ctx, goingLandingKey{}, landing), "")
	if err != nil {
		return goingRefusedLine(line, goingFailed(err))
	}
	response, err := u.doer.Do(request)
	if err != nil {
		return goingRefusedLine(line, goingFailed(err))
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode < http.StatusMultipleChoices {
		if line.Outcome.OK {
			line.Outcome.Status = response.StatusCode
		}
		return line
	}
	if landed, err := u.s.goingUndoLanded(ctx, key); err != nil {
		return goingRefusedLine(line, goingFailed(err))
	} else if landed != nil {
		return *landed
	}
	outcome := goingRefusalOf(response, answer).outcome()
	if slices.Contains(send.changed, outcome.Code) {
		outcome = goingChanged([]string{outcome.Message}, nil)
	}
	return goingRefusedLine(line, outcome)
}

// goingRefusedLine is line with outcome in place of the one it would have answered had its request
// landed: every field it would have put back is left with that outcome, and every field it kept
// stays kept.
func goingRefusedLine(line goingUndoLine, outcome goingWriteOutcome) goingUndoLine {
	line.Outcome = outcome
	line.Fields = slices.Clone(line.Fields)
	for index, field := range line.Fields {
		if field.OK {
			line.Fields[index] = goingUndoField{Field: field.Field, Message: outcome.Message}
		}
	}
	return line
}

// goingUndoLanding is the landing of the one request the undo of a call sends: the call's key, the
// request by method, path and body digest, and the line the undo answers once the request lands.
type goingUndoLanding struct {
	key                  goingCallKey
	method, path, digest string
	line                 goingUndoLine
}

// step is the commit step a route's transaction takes while the undo's request runs (running): a
// transaction that wrote (goingTransactionWrote) writes the request's going_landings row - the
// call's key, phase undo, request 0 - with what its route recorded it answers (goingRecordAnswer)
// and the line the undo answers, in that same transaction, so the undo and the record of it
// commit together or not at all. A second press whose request of the same undo commits after this
// one waits on its row and fails, which rolls its change back. A route that commits several
// transactions writes the row with the first.
func (l goingUndoLanding) step(running *atomic.Bool, sink *goingAnswerSink) store.BeforeCommit {
	var recorded atomic.Bool
	return func(ctx context.Context, tx pgx.Tx) error {
		if !running.Load() || recorded.Load() {
			return nil
		}
		wrote, err := goingTransactionWrote(ctx, tx)
		if err != nil || !wrote {
			return err
		}
		status, body, _ := sink.recorded()
		line := l.line
		if line.Outcome.OK {
			line.Outcome.Status = cmp.Or(status, line.Outcome.Status)
		}
		answer, err := json.Marshal(struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
			Line   goingUndoLine   `json:"line"`
		}{status, goingJSONOrNullRaw(body), line})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
			values ($1::uuid, 'undo', $2, $3, 0, $4, $5, $6, $7::jsonb)
		`, l.key.run, l.key.step, l.key.call, l.method, l.path, l.digest, string(answer)); err != nil {
			return fmt.Errorf("record an undo's request landed: %w", err)
		}
		recorded.Store(true)
		return nil
	}
}

// goingUndoLanded is the line the undo of the call key answered where its request landed
// (goingUndoLanding), nil where none did.
func (s *server) goingUndoLanded(ctx context.Context, key goingCallKey) (*goingUndoLine, error) {
	var encoded []byte
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select answer->'line' from going_landings where run_id = $1::uuid and phase = 'undo' and step = $2 and call = $3 and request = 0
	`, key.run, key.step, key.call).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the undo of call %d of step %d: %w", key.call, key.step, err)
	}
	var line goingUndoLine
	if err := json.Unmarshal(encoded, &line); err != nil {
		return nil, fmt.Errorf("read the undo of call %d of step %d: %w", key.call, key.step, err)
	}
	return &line, nil
}

// goingUndone is what one landed call found and left, read off its record (goingUndoer.recorded):
// its tool, its item, the fields of it the call changed, the value it found there (before) and
// the one it left (after), and whether its route answered that it changed nothing (unchanged) or
// that it found a value the read its row was approved over did not see (beforeUnread).
type goingUndone struct {
	tool         string
	target       string
	fields       []string
	before       json.RawMessage
	after        json.RawMessage
	unchanged    bool
	beforeUnread bool
}

// recorded is what write found and left, from what the route of its last request recorded beside
// that request's landing, in the transaction of the change (goingRecordAnswer): the call's row is
// the one of its tool the request names (goingRecordedCall), and nothing of the call is
// translated or read before again. The value it found is what its route answered it found
// (goingObservation.stood), else the value of the read its row was approved over; whether the call
// changed anything is its landing, which it has. A last request that recorded nothing, or whose
// row the tool no longer holds, is an error: what the call left is unknown.
func (u goingUndoer) recorded(ctx context.Context, write goingLandedWrite) (goingUndone, error) {
	last := write.requests[len(write.requests)-1]
	undone := goingUndone{tool: write.tool, fields: goingChangedFields(write)}
	prepared, held := goingRecordedCall(u.s.going, write.tool, goingWriteRequest{Method: last.method, Path: last.path})
	if !held {
		return undone, fmt.Errorf("no row of %s sends %s %s", write.tool, last.method, last.path)
	}
	prepared.arguments = write.arguments
	before := goingObservation{target: goingTargetOfPath(prepared.call.path), fields: undone.fields, issue: goingText(write.arguments, "issue")}
	var seen json.RawMessage
	if write.seen != nil {
		before.target = cmp.Or(write.seen.Item, before.target)
		before.issue = cmp.Or(write.seen.Issue, before.issue)
		before.token = write.seen.Token
		seen = write.seen.Now
	}
	undone.target = before.target
	if len(last.body) == 0 || string(last.body) == "null" {
		return undone, fmt.Errorf("the route of %s %s recorded nothing of what it found and left beside its landing", last.method, last.path)
	}
	observer := u.observer
	observer.landed = func(context.Context) (bool, error) { return true, nil }
	observed, err := observer.observe(ctx, prepared, last.body, last.status, before)
	if err != nil {
		return undone, err
	}
	undone.target = cmp.Or(observed.target, undone.target)
	if len(observed.fields) > 0 {
		undone.fields = observed.fields
	}
	undone.before = goingJSONOrNullRaw(seen)
	if len(observed.stood) > 0 {
		undone.before = observed.stood
	}
	undone.after = goingJSONOrNull(observed.value)
	undone.unchanged, undone.beforeUnread = observed.unchanged, observed.beforeUnread
	return undone, nil
}

// goingChangedFields is the fields of its item a landed call changes: those the read its row was
// approved over names, else those its tool and arguments write.
func goingChangedFields(write goingLandedWrite) []string {
	if write.seen != nil && len(write.seen.Fields) > 0 {
		return write.seen.Fields
	}
	switch write.tool {
	case "dispatch_issue_update":
		return goingIssueFieldsOf(write.arguments)
	case "dispatch_edit_ask":
		return goingAskFieldsOf(write.arguments)
	}
	return goingFields(goingWriteFields[write.tool])
}

// writer names who wrote the newest event of an item, as a claim refusal names a holder
// (claimHolder), read from the store: the newest event of issue where document is "", else the
// newest event of the owner - issue, or project where issue is "" - that names document; "another
// writer" where none is found.
func (u goingUndoer) writer(ctx context.Context, issue, project, document string) string {
	var actor model.Actor
	var err error
	switch {
	case document == "":
		err = u.s.deps.Store.Pool.QueryRow(ctx, `select actor from events where issue_key = $1 order by seq desc limit 1`, issue).Scan(&actor)
	case issue != "":
		err = u.s.deps.Store.Pool.QueryRow(ctx, `
			select actor from events where issue_key = $1 and payload->>'artifact_id' = $2 order by seq desc limit 1
		`, issue, document).Scan(&actor)
	default:
		err = u.s.deps.Store.Pool.QueryRow(ctx, `
			select actor from events where project_key = $1 and payload->>'artifact_id' = $2 order by seq desc limit 1
		`, project, document).Scan(&actor)
	}
	if err != nil || actor.ID == "" {
		return "another writer"
	}
	return claimHolder(actor, "")
}

// askWriter names who wrote the newest event of the ask id (writer).
func (u goingUndoer) askWriter(ctx context.Context, id string) string {
	var actor model.Actor
	if err := u.s.deps.Store.Pool.QueryRow(ctx, `select actor from events where payload->>'id' = $1 order by id desc limit 1`, id).Scan(&actor); err != nil || actor.ID == "" {
		return "another writer"
	}
	return claimHolder(actor, "")
}

// goingFailed is the outcome of an undo that could not be made at all, or of a read it made that
// failed: a route's refusal as that route answered it, and anything else as the server's own
// failure, its detail logged and its message the fixed one writeHandlerError answers.
func goingFailed(err error) goingWriteOutcome {
	var refusal goingRefusal
	if errors.As(err, &refusal) {
		return refusal.outcome()
	}
	slog.Error("dispatch: a command-bar undo failed", "error", err)
	return goingWriteOutcome{Status: http.StatusInternalServerError, Code: codeInternal, Message: "internal server error"}
}

// goingChanged is the outcome of an undo that kept, whole or for some fields, what another writer
// left: each change named, with what was undone beside it.
func goingChanged(changes []string, restored []string) goingWriteOutcome {
	message := strings.Join(changes, "; ")
	if len(restored) > 0 {
		message += "; the rest was undone: " + strings.Join(restored, ", ")
	}
	return goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoChanged, Message: message}
}

// goingFieldChange is a field's outcome as the line's whole outcome says it among the other
// fields' (goingChanged): the field's name, then the field's own message, which never names the
// field, since every reader prints the field beside it. A field with no name is its message alone.
func goingFieldChange(field, why string) string {
	if field == "" {
		return why
	}
	return field + ": " + why
}

// goingKeptField is the message of a field another writer changed after the run: what it holds now
// (held) and who wrote it, without naming the field (goingFieldChange).
func goingKeptField(held json.RawMessage, who string) string {
	return fmt.Sprintf("holds %s now, which %s wrote after this run, so it is kept", held, who)
}

// goingRank is the field an issue's rank is recorded under. Its value is the issue's place
// (issueRankPlace): its rank and the neighbours it stood between, since a rank is written by
// naming neighbours.
const goingRank = "rank"

// issueFields writes back each field of the issue the call changed that still holds what the call
// left, and keeps each one another writer changed since, naming who. The rank is put back between
// the neighbours the issue stood between before the call (rankRestore), while the issue's rank is
// still the one the call left; with neither neighbour an issue of the project now, the rank is
// left, naming why. The advances relation is put back as the issue's own priority ids before the
// call, carrying the revision the call left as the route's own compare-and-set
// (advances_revision): a writer who changed the relation since moved its revision. Every other
// field the update expects to hold what the call left (goingCondition.Expect), so a change another
// writer makes after the read is refused by the update itself.
func (u goingUndoer) issueFields(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.issueFields(ctx, undone.target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	who := sync.OnceValue(func() string { return u.writer(ctx, undone.target, "", "") })
	restoreOf := func(field string, held, before, after json.RawMessage) (any, string, error) {
		switch field {
		case goingAdvances:
			var heldValue, afterValue goingAdvancesValue
			if json.Unmarshal(held, &heldValue) != nil || json.Unmarshal(after, &afterValue) != nil {
				return nil, "the change's record holds no revision to write back against, so it is kept", nil
			}
			if heldValue.Revision != afterValue.Revision {
				return nil, fmt.Sprintf("holds revision %d now, which %s wrote after this run, so it is kept", heldValue.Revision, who()), nil
			}
			var beforeValue goingAdvancesValue
			if json.Unmarshal(before, &beforeValue) != nil || beforeValue.IDs == nil {
				return nil, "the change's record holds no priority ids to write back, so it is kept", nil
			}
			return beforeValue.IDs, "", nil
		case goingRank:
			var heldPlace, beforePlace, afterPlace issueRankPlace
			if json.Unmarshal(held, &heldPlace) != nil || json.Unmarshal(before, &beforePlace) != nil || json.Unmarshal(after, &afterPlace) != nil {
				return nil, "the change's record holds no place to put the issue back in, so it is kept", nil
			}
			// Its neighbours move as other issues are ranked; only its own rank says whether another
			// writer moved this issue.
			if heldPlace.Rank != afterPlace.Rank {
				return nil, fmt.Sprintf("the issue stands between %s and %s now, where %s moved it after this run, so it is kept",
					goingNeighbourText(heldPlace.After), goingNeighbourText(heldPlace.Before), who()), nil
			}
			return u.observer.rankRestore(ctx, undone.target, beforePlace)
		default:
			if !goingSameJSON(held, after) {
				return nil, goingKeptField(held, who()), nil
			}
			return goingWriteBackValue(field, before), "", nil
		}
	}
	conditionOf := func(restored map[string]json.RawMessage) (map[string]any, goingCondition) {
		body, expect := map[string]any{}, map[string]json.RawMessage{}
		for field, after := range restored {
			switch field {
			case goingAdvances:
				var value goingAdvancesValue
				if json.Unmarshal(after, &value) == nil {
					body["advances_revision"] = value.Revision
				}
			case goingRank:
				var place issueRankPlace
				if json.Unmarshal(after, &place) == nil {
					expect[field], _ = json.Marshal(place.Rank)
				} else {
					expect[field] = goingJSONOrNullRaw(after)
				}
			default:
				expect[field] = goingJSONOrNullRaw(after)
			}
		}
		return body, goingCondition{Expect: expect}
	}
	return u.writeBack(line, undone, read, goingIssuePatchRoute, map[string]string{"key": undone.target}, restoreOf, conditionOf,
		[]string{"ISSUE_CHANGED", codeAdvancesRevision})
}

// rankRestore is how a write back puts an issue at the place it held: between the two neighbours
// it had, those of them that are still issues of its project, or both where they still stand in
// order, and why it cannot where neither is. A read of the neighbours that failed is its error,
// never a reason: the undo answers it as the server's own failure.
func (o goingObserver) rankRestore(ctx context.Context, key string, place issueRankPlace) (map[string]any, string, error) {
	names := make([]string, 0, 2)
	for _, neighbour := range []*string{place.After, place.Before} {
		if neighbour != nil {
			names = append(names, *neighbour)
		}
	}
	// A neighbour that is no longer an issue of the project is passed over: it leaves that side
	// of the place unnamed.
	ranks := map[string]string{}
	if len(names) > 0 {
		rows, err := o.s.deps.Store.Pool.Query(ctx, `
			select n.key, n.rank from issues n join issues i on i.key = $2 and i.project_key = n.project_key
			where n.key = any($1)
		`, names, key)
		if err != nil {
			return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, rank string
			if err := rows.Scan(&name, &rank); err != nil {
				return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
			}
			ranks[name] = rank
		}
		if err := rows.Err(); err != nil {
			return nil, "", fmt.Errorf("read the ranks of the issues %s stood between: %w", key, err)
		}
	}
	restore := map[string]any{}
	if place.After != nil && ranks[*place.After] != "" {
		restore["after"] = *place.After
	}
	if place.Before != nil && ranks[*place.Before] != "" {
		if after, both := restore["after"].(string); !both || ranks[after] < ranks[*place.Before] {
			restore["before"] = *place.Before
		}
	}
	if len(restore) == 0 {
		return nil, "neither issue it stood between is an issue of its project now, so its place cannot be named back", nil
	}
	return restore, "", nil
}

// goingNeighbourText names a rank neighbour, or says there is none.
func goingNeighbourText(key *string) string {
	if key == nil {
		return "nothing"
	}
	return *key
}

// goingWriteBackValue is a field's value before a call as an update writes it back: a route the
// issue had none of is cleared by "", which is how an update clears one.
func goingWriteBackValue(field string, before json.RawMessage) any {
	if field == "route" && string(goingJSONOrNullRaw(before)) == "null" {
		return ""
	}
	return goingJSONOrNullRaw(before)
}

// goingSameJSON reports whether two JSON values are the same value, whatever their spelling.
func goingSameJSON(left, right json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(goingJSONOrNullRaw(left), &a) != nil || json.Unmarshal(goingJSONOrNullRaw(right), &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func goingJSONOrNullRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// askFields writes back each part of the ask the edit changed that still holds what the edit left,
// and keeps each one another writer changed since, naming who; the edit expects each part it
// writes back to hold what the edit left (goingCondition.Expect).
func (u goingUndoer) askFields(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.askFields(ctx, undone.target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	who := sync.OnceValue(func() string { return u.askWriter(ctx, undone.target) })
	restoreOf := func(_ string, held, before, after json.RawMessage) (any, string, error) {
		if !goingSameJSON(held, after) {
			return nil, goingKeptField(held, who()), nil
		}
		return goingJSONOrNullRaw(before), "", nil
	}
	conditionOf := func(restored map[string]json.RawMessage) (map[string]any, goingCondition) {
		expect := map[string]json.RawMessage{}
		for field, after := range restored {
			expect[field] = goingJSONOrNullRaw(after)
		}
		return nil, goingCondition{Expect: expect}
	}
	return u.writeBack(line, undone, read, goingAskEditRoute, map[string]string{"id": undone.target}, restoreOf, conditionOf, []string{"ASK_CHANGED"})
}

// writeBack is a write back of the fields one call changed, by one PATCH through route, its
// placeholders filled from values: restoreOf says, from what each field holds now, held before the
// call and was left by it, the value the PATCH writes back, or why the field is kept - another
// writer changed it since - or fails, which fails the whole write back before anything is sent.
// conditionOf says what guards the write back, from the fields restored and what the call left
// them: the condition the PATCH carries on its context, marked as a write back (Expect,
// going_conditions.go), and the body keys of a precondition token a field carries instead
// (advances_revision), each decided under the lock the route takes. changed are the route's
// refusals that say another writer changed a field between the read and the PATCH. Each field's
// own outcome is answered beside the call's; a call whose every field is kept sends nothing.
func (u goingUndoer) writeBack(
	line goingUndoLine, undone goingUndone, read func([]string) (map[string]any, error),
	route string, values map[string]string,
	restoreOf func(field string, held, before, after json.RawMessage) (any, string, error),
	conditionOf func(restored map[string]json.RawMessage) (map[string]any, goingCondition), changed []string,
) (goingUndoLine, *goingUndoSend) {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil || len(after) == 0 {
		line.Outcome = goingFailed(errors.New("the change's record holds no field values to write back"))
		return goingLineOf(line, undone.fields), nil
	}
	fields := slices.Sorted(maps.Keys(after))
	now, err := read(fields)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, fields), nil
	}
	restore := map[string]any{}
	restoredAfter := map[string]json.RawMessage{}
	var changes, restored []string
	line.Fields = make([]goingUndoField, 0, len(fields))
	for _, field := range fields {
		value, why, err := restoreOf(field, goingJSONOrNull(now[field]), goingJSONOrNullRaw(before[field]), after[field])
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, fields), nil
		}
		if why != "" {
			changes = append(changes, goingFieldChange(field, why))
			line.Fields = append(line.Fields, goingUndoField{Field: field, Message: why})
			continue
		}
		restore[field], restoredAfter[field] = value, after[field]
		restored = append(restored, field)
		line.Fields = append(line.Fields, goingUndoField{Field: field, OK: true, Message: "written back"})
	}
	line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "written back"}
	if len(changes) > 0 {
		line.Outcome = goingChanged(changes, restored)
	}
	if len(restore) == 0 {
		return line, nil
	}
	tokens, condition := conditionOf(restoredAfter)
	maps.Copy(restore, tokens)
	condition.WriteBack = true
	return line, &goingUndoSend{route: route, values: values, body: restore, condition: &condition, landed: line, changed: changed}
}

// document writes the content the document held before the edit back over it, as a further
// version uploaded under its own name to its owner's route, only while the document is what the
// edit left: its token now is the one the edit's route answered it left (editArtifactResponse),
// and the upload expects the content read with that token (goingCondition.ExpectSHA256), decided
// under the document's own lock, so an edit another writer makes after the read is never written
// over. A document another writer changed since the edit is kept, naming its version and who
// wrote it. What the edit replaced is the content of the read its row was approved over, where
// that read's token is the one the edit's route answered it found; any other is a document another
// writer changed before the edit, and nothing is written back.
func (u goingUndoer) document(ctx context.Context, write goingLandedWrite, line goingUndoLine) (goingUndoLine, *goingUndoSend) {
	fields := goingChangedFields(write)
	last := write.requests[len(write.requests)-1]
	var edited struct {
		Token         string `json:"token"`
		PreviousToken string `json:"previous_token"`
	}
	if json.Unmarshal(last.body, &edited) != nil || edited.Token == "" || edited.PreviousToken == "" {
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
			Message: "the edit's record does not name the document it found and the one it left, so nothing was written back: read the document, and change it by hand to undo this edit"}
		return goingLineOf(line, fields), nil
	}
	document := ""
	if write.seen != nil {
		document = write.seen.Item
	}
	if document == "" {
		prepared, held := goingRecordedCall(u.s.going, write.tool, goingWriteRequest{Method: last.method, Path: last.path})
		if !held {
			line.Outcome = goingFailed(fmt.Errorf("no row of %s sends %s %s", write.tool, last.method, last.path))
			return goingLineOf(line, fields), nil
		}
		prepared.arguments = write.arguments
		resolved, err := u.observer.editedDocument(ctx, prepared)
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, fields), nil
		}
		document = resolved
	}
	var artifact goingDocumentRead
	if err := u.observer.read.get(ctx, goingArtifactPath(document), &artifact); err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, fields), nil
	}
	line.Item = artifact.ID
	now, token, err := u.observer.documentValue(ctx, artifact.ID)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, fields), nil
	}
	if token != edited.Token {
		who := u.writer(ctx, deref(artifact.IssueKey), artifact.Project, artifact.ID)
		line.Outcome = goingChanged([]string{fmt.Sprintf("the document is at version %s now, which %s wrote after this run, so it is kept",
			goingVersionText(now.Version), who)}, nil)
		return goingLineOf(line, fields), nil
	}
	var before goingDocumentValue
	if write.seen == nil || write.seen.Token != edited.PreviousToken || json.Unmarshal(write.seen.Now, &before) != nil {
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoBeforeUnread,
			Message: "another writer changed the document between the read the edit was approved over and the edit, so what it replaced is unknown and nothing was written back: read the document to see what it holds"}
		return goingLineOf(line, fields), nil
	}
	route, values := goingProjectUploadRoute, map[string]string{"key": artifact.Project}
	if artifact.IssueKey != nil {
		route, values = goingIssueUploadRoute, map[string]string{"key": *artifact.IssueKey}
	}
	line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "written back"}
	return goingLineOf(line, fields), &goingUndoSend{route: route, values: values,
		body:      map[string]any{"name": artifact.Name, "content": before.Content, "summary": goingUndoReason},
		condition: &goingCondition{WriteBack: true, ExpectSHA256: docs.MarkdownSHA256(now.Content)},
		landed:    goingLineOf(line, fields), changed: []string{"DOCUMENT_CHANGED"}}
}

// goingVersionText is a document's version as a person reads it.
func goingVersionText(version *int) string {
	if version == nil {
		return "none"
	}
	return fmt.Sprint(*version)
}

// claimant is the holder the claim route records the undo's own claim as: the session the
// caller's credential proved, where it proved one, else the person.
func (u goingUndoer) claimant() goingHolder {
	if u.session != "" {
		return goingHolder{Kind: "session", ID: u.session}
	}
	return goingHolder{Kind: "user", ID: u.login}
}

// claimsAs reports whether holder is the undo's claimant (claimant), so a claim the undo makes
// puts back holder's own claim rather than handing the issue to another.
func (u goingUndoer) claimsAs(holder *goingHolder) bool {
	if holder == nil {
		return false
	}
	claimant := u.claimant()
	if holder.Kind != claimant.Kind {
		return false
	}
	if holder.Kind == "user" {
		return model.CanonicalLogin(holder.ID) == model.CanonicalLogin(claimant.ID)
	}
	return holder.ID == claimant.ID
}

// goingHolderText names a claim's holder as a claim refusal names one (claimHolder), or says
// nobody holds it.
func goingHolderText(holder *goingHolder) string {
	if holder == nil {
		return "nobody"
	}
	return claimHolder(model.Actor{Kind: holder.Kind, ID: holder.ID}, "")
}

// claim undoes a claim by releasing it and a release by taking the claim back, each decided by the
// claim route in its own transaction. A claim is released only while the holder it left - as its
// route answered it - still holds it (goingCondition.ExpectHolder); a release is taken back only
// for the holder it released - as its route answered it, previous_claim - and only where the claim
// the undo makes would be recorded as that very holder (claimsAs) and nobody holds the issue
// (goingCondition.ExpectUnclaimed). A release of another's claim is refused, 409
// GOING_UNDO_NOT_CALLERS_CLAIM, with nothing written: the bar cannot claim for that holder. Either
// route refusing its condition, 409 CLAIM_HOLDER_CHANGED, is the claim changed by another writer
// since, and it is kept. A claim or a release its route answered changed nothing is undone by
// nothing.
func (u goingUndoer) claim(_ context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	var before, after *goingHolder
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		line.Outcome = goingFailed(errors.New("the change's record holds no claim to undo"))
		return goingLineOf(line, undone.fields), nil
	}
	values := map[string]string{"key": undone.target}
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	reversed = goingLineOf(reversed, undone.fields)
	changed := []string{codeClaimHolderChanged}
	switch {
	case undone.unchanged:
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK,
			Message: "the claim route answered that this change changed nothing - the claim already stood as it asked - so there is nothing to undo"}
		return goingLineOf(line, undone.fields), nil
	case after != nil:
		// The body is empty: a bearer's release reads its actor from one, and a person's none.
		return line, &goingUndoSend{route: goingReleaseRoute, values: values, body: map[string]any{},
			condition: &goingCondition{WriteBack: true, ExpectHolder: &model.Actor{Kind: after.Kind, ID: after.ID}}, landed: reversed, changed: changed}
	case before == nil:
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "nobody held the claim before this release, so there is nothing to take back"}
		return goingLineOf(line, undone.fields), nil
	case !u.claimsAs(before):
		claimant := u.claimant()
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoNotCallers, Message: fmt.Sprintf(
			"this release released the claim of %s, which the bar cannot claim for them: its claim would be recorded as %s, which never held it, so nothing was taken back",
			goingHolderText(before), goingHolderText(&claimant))}
		return goingLineOf(line, undone.fields), nil
	}
	return line, &goingUndoSend{route: goingClaimRoute, values: values, body: map[string]any{},
		condition: &goingCondition{WriteBack: true, ExpectUnclaimed: true}, landed: reversed, changed: changed}
}

// follow undoes a follow by unfollowing and an unfollow by following again, while the session
// still follows as the change left it.
func (u goingUndoer) follow(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	var before, after goingFollowing
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		line.Outcome = goingFailed(errors.New("the change's record holds no follow to undo"))
		return goingLineOf(line, undone.fields), nil
	}
	following, err := u.observer.follows(ctx, undone.target, after.Session)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, undone.fields), nil
	}
	switch {
	case following != after.Following:
		state := "does not follow the ask now"
		if following {
			state = "follows the ask now"
		}
		line.Outcome = goingChanged([]string{fmt.Sprintf("session %s %s, which another writer left after this run, so it is kept", after.Session, state)}, nil)
		return goingLineOf(line, undone.fields), nil
	case before.Following == after.Following:
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the change changed nothing, so there is nothing to undo"}
		return goingLineOf(line, undone.fields), nil
	}
	route := goingUnfollowRoute
	if before.Following {
		route = goingFollowRoute
	}
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	return line, &goingUndoSend{route: route, values: map[string]string{"id": undone.target, "session_id": after.Session},
		landed: goingLineOf(reversed, undone.fields)}
}

// goingCommentResolution is a resolve's value: whether the thread is resolved, and the resolution
// as the comment's read answers it, who resolved it and when, null for an open thread.
type goingCommentResolution struct {
	Resolved   bool            `json:"resolved"`
	ResolvedBy json.RawMessage `json:"resolved_by"`
	ResolvedAt *string         `json:"resolved_at"`
}

// comment undoes a resolve by reopening the thread, while it still holds the resolution the run
// left: the reopen expects that resolution's moment (goingCondition.ExpectResolvedAt), decided
// under the comment's lock, so a thread another writer reopened and resolved again keeps their
// resolution, the route's 409 COMMENT_CHANGED being the thread changed.
func (u goingUndoer) comment(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	var before, after goingCommentResolution
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		line.Outcome = goingFailed(errors.New("the change's record holds no resolve to undo"))
		return goingLineOf(line, undone.fields), nil
	}
	current, err := u.observer.commentResolved(ctx, undone.target)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, undone.fields), nil
	}
	now, _ := current.value.(goingCommentResolution)
	switch {
	case before.Resolved || !after.Resolved:
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was resolved before this change, so there is nothing to reopen"}
		return goingLineOf(line, undone.fields), nil
	case !now.Resolved:
		line.Outcome = goingChanged([]string{"the thread is open now, reopened by another writer after this run"}, nil)
		return goingLineOf(line, undone.fields), nil
	case after.ResolvedAt == nil || now.ResolvedAt == nil || *now.ResolvedAt != *after.ResolvedAt:
		line.Outcome = goingChanged([]string{"the thread was resolved again after this run, by another writer, so it is kept"}, nil)
		return goingLineOf(line, undone.fields), nil
	}
	resolvedAt := *after.ResolvedAt
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	return line, &goingUndoSend{route: goingCommentReopenRoute, values: map[string]string{"id": undone.target},
		condition: &goingCondition{WriteBack: true, ExpectResolvedAt: &resolvedAt}, landed: goingLineOf(reversed, undone.fields),
		changed: []string{codeCommentChanged}}
}

// reopened undoes a reopen by resolving the thread again, while it is still open as the run left
// it: the resolve expects an open thread (goingCondition.ExpectOpen), decided under the comment's
// lock, so a thread another writer resolved since keeps their resolution, the route's 409
// COMMENT_CHANGED being the thread changed. A reopen that found the thread open changed nothing,
// and is undone by nothing.
func (u goingUndoer) reopened(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	var before, after goingCommentResolution
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		line.Outcome = goingFailed(errors.New("the change's record holds no reopen to undo"))
		return goingLineOf(line, undone.fields), nil
	}
	if !before.Resolved || after.Resolved {
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was open before this change, so there is nothing to resolve"}
		return goingLineOf(line, undone.fields), nil
	}
	current, err := u.observer.commentResolved(ctx, undone.target)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, undone.fields), nil
	}
	if now, _ := current.value.(goingCommentResolution); now.Resolved {
		line.Outcome = goingChanged([]string{"the thread is resolved now, by another writer after this run, so it is kept"}, nil)
		return goingLineOf(line, undone.fields), nil
	}
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	return line, &goingUndoSend{route: goingCommentResolve, values: map[string]string{"id": undone.target},
		condition: &goingCondition{WriteBack: true, ExpectOpen: true}, landed: goingLineOf(reversed, undone.fields),
		changed: []string{codeCommentChanged}}
}

// retract undoes an ask the run opened, an approval request's among them, by retracting it while
// it is still open. An approval request its route answered opened no ask - it renewed one already
// open - is undone by nothing. A record that does not say whether an approval request opened its
// ask, or says it opened one and does not name it, is the server's own failure, and retracts
// nothing.
func (u goingUndoer) retract(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	var after struct {
		ID     string `json:"id"`
		Ask    any    `json:"ask"`
		Opened *bool  `json:"opened"`
	}
	if err := json.Unmarshal(undone.after, &after); err != nil {
		line.Outcome = goingFailed(fmt.Errorf("the change's record holds no ask to retract: %w", err))
		return goingLineOf(line, undone.fields), nil
	}
	ask := after.ID
	if undone.tool == "dispatch_request_approval" {
		if after.Opened == nil {
			line.Outcome = goingFailed(errors.New("the change's record does not say whether the approval request opened its ask"))
			return goingLineOf(line, undone.fields), nil
		}
		if !*after.Opened {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK,
				Message: "the approval route answered that this change opened no ask - it renewed one already open - so there is none to retract"}
			return goingLineOf(line, undone.fields), nil
		}
		opened, named := after.Ask.(string)
		if !named || opened == "" {
			line.Outcome = goingFailed(errors.New("the change's record says the approval request opened an ask and does not name it"))
			return goingLineOf(line, undone.fields), nil
		}
		ask = opened
	}
	if ask == "" {
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "this change opened no ask, so there is none to retract"}
		return goingLineOf(line, undone.fields), nil
	}
	line.Item = ask
	current, err := u.observer.askState(ctx, ask)
	if err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, undone.fields), nil
	}
	if current.value != "open" {
		line.Outcome = goingChanged([]string{fmt.Sprintf("the ask is %v now, so it is no longer open to retract", current.value)}, nil)
		return goingLineOf(line, undone.fields), nil
	}
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	return line, &goingUndoSend{route: goingAskResolveRoute, values: map[string]string{"id": ask},
		body: map[string]any{"kind": "retracted", "reason": goingUndoReason}, landed: goingLineOf(reversed, undone.fields)}
}

// closeCreated closes an issue the run created, posting nothing on it: its status set done, while
// it still holds the status the create left it in, as the create's route answered it. An issue
// someone closed or moved since is kept, with nothing written, and the close expects that status
// too (goingCondition.Expect), decided under the issue's lock, so a move made after the undo's read
// is never closed over either: that refusal, 409 ISSUE_CHANGED, is the issue changed. A create its
// route answered made nothing - it named an external issue Dispatch already held - is undone by
// nothing: that issue is no issue of the run's.
func (u goingUndoer) closeCreated(ctx context.Context, line goingUndoLine, undone goingUndone) (goingUndoLine, *goingUndoSend) {
	if undone.unchanged {
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: fmt.Sprintf(
			"this create made no issue: Dispatch already held %s for the external issue it named, so nothing was closed", undone.target)}
		return goingLineOf(line, undone.fields), nil
	}
	var made struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(undone.after, &made); err != nil || made.Status == "" {
		line.Outcome = goingFailed(errors.New("the create's record names no status the issue was left in, so no close can expect it"))
		return goingLineOf(line, undone.fields), nil
	}
	var issue struct {
		Status   string  `json:"status"`
		ClosedAt *string `json:"closed_at"`
	}
	if err := u.observer.read.get(ctx, goingIssuePath(undone.target), &issue); err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, undone.fields), nil
	}
	switch {
	case issue.ClosedAt != nil:
		line.Outcome = goingChanged([]string{fmt.Sprintf("the issue was closed at %s, by %s, so it is kept", *issue.ClosedAt, u.writer(ctx, undone.target, "", ""))}, nil)
		return goingLineOf(line, undone.fields), nil
	case issue.Status != made.Status:
		line.Outcome = goingChanged([]string{fmt.Sprintf("the issue is %s now, where %s moved it after this run left it %s, so it is kept",
			issue.Status, u.writer(ctx, undone.target, "", ""), made.Status)}, nil)
		return goingLineOf(line, undone.fields), nil
	}
	expected, _ := json.Marshal(made.Status)
	closed := line
	closed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "closed"}
	return line, &goingUndoSend{route: goingIssuePatchRoute, values: map[string]string{"key": undone.target}, body: map[string]any{"status": "done"},
		condition: &goingCondition{WriteBack: true, Expect: map[string]json.RawMessage{"status": expected}},
		landed:    goingLineOf(closed, undone.fields), changed: []string{"ISSUE_CHANGED"}}
}

// suggestion undoes a suggestion the run posted by rejecting it while it is still open: the
// suggestion's comment is the one its landing's recovery point names (commentRecovery), which its
// route committed with it. A suggestion someone accepted or rejected since is kept, naming who,
// and the reject's own refusal of one decided meanwhile, 409 ALREADY_ACTIONED, is the same.
func (u goingUndoer) suggestion(ctx context.Context, write goingLandedWrite, line goingUndoLine) (goingUndoLine, *goingUndoSend) {
	fields := goingChangedFields(write)
	var posted commentRecovery
	last := write.requests[len(write.requests)-1]
	if json.Unmarshal(last.recovery, &posted) != nil || posted.CommentID == "" {
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
			Message: "the suggestion's record does not name the comment it posted, so nothing was rejected: read the document's suggestions, and reject it by hand"}
		return goingLineOf(line, fields), nil
	}
	line.Item = posted.CommentID
	var read struct {
		Comment struct {
			ResolvedBy *model.Actor `json:"resolved_by"`
			Suggestion *struct {
				Accepted *bool `json:"accepted"`
			} `json:"suggestion"`
		} `json:"comment"`
	}
	if err := u.observer.read.get(ctx, goingCommentPath(posted.CommentID), &read); err != nil {
		line.Outcome = goingFailed(err)
		return goingLineOf(line, fields), nil
	}
	if read.Comment.Suggestion != nil && read.Comment.Suggestion.Accepted != nil {
		decided, who := "rejected", "another writer"
		if *read.Comment.Suggestion.Accepted {
			decided = "accepted"
		}
		if read.Comment.ResolvedBy != nil && read.Comment.ResolvedBy.ID != "" {
			who = claimHolder(*read.Comment.ResolvedBy, "")
		}
		line.Outcome = goingChanged([]string{fmt.Sprintf("the suggestion was %s by %s after this run, so it is kept", decided, who)}, nil)
		return goingLineOf(line, fields), nil
	}
	rejected := line
	rejected.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "rejected"}
	return line, &goingUndoSend{route: goingCommentRejectRoute, values: map[string]string{"id": posted.CommentID},
		landed: goingLineOf(rejected, fields), changed: []string{"ALREADY_ACTIONED"}}
}
