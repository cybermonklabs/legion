package api

// going_undo.go undoes one run of the command bar (R6): POST /api/v1/going/runs/{id}/undo walks
// the run's landed calls - each call of the run that landed a request (going_landings, phase do),
// with the row of the run's plan it ran - newest first, and undoes each as the credential that
// pressed Undo, through the server's own route table in-process (going_writes.go), by the undo
// the row's tool states (goingWriteTool.Undo):
//
//   - write_back reads the item's value now (going_observe.go) and writes back the value the call
//     found, field by field, only where the value now is the one the call left; a later value
//     another writer left is kept and named, with who changed that field after the call landed -
//     an issue's fields, an ask's parts, a document's content, a comment's or an issue message's
//     body, of which a post deleted since is kept, named deleted, with no word of its body;
//   - reverse calls the row's reverse route (goingToolRoute.UndoRoutes): a claim released or taken
//     back, a follow unfollowed, a resolved thread reopened and a reopened one resolved again, an
//     opened ask retracted, and a suggestion of the run that is still open rejected;
//   - close_created closes the issue the call created, with no message and no comment;
//   - not_undone names the call "sent, not undone": a message, a broadcast, a review; and a delete
//     of a comment or an issue message "deleted, not undone", since nothing restores a deleted
//     post.
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingUndoReason is the note an undo's retraction of an ask and a document's restoring version
// carry.
const goingUndoReason = "Undone from the command bar."

// goingSentNotUndone is what an undo says of a call it does not undo: a message, a broadcast, a
// review, or any other write that reached a person or a session, or that no route takes back.
const goingSentNotUndone = "sent, not undone"

// goingDeletedNotUndone is what an undo says of a delete of a comment or an issue message, which
// nothing restores (goingNotUndoneWords).
const goingDeletedNotUndone = "deleted, not undone"

// goingNotUndoneWords is what an undo says of a call of tool, whose undo is not_undone: a delete
// of a post is deleted, not undone, and every other write sent, not undone.
func goingNotUndoneWords(tool string) string {
	if tool == "dispatch_delete_comment" || tool == "dispatch_delete_message" {
		return goingDeletedNotUndone
	}
	return goingSentNotUndone
}

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
	// row was approved over did not see, or a document's edit whose approval holds no read of the
	// document it can put back: what the call replaced is unknown, so nothing is put back.
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
// writer left it, with what it holds now and who wrote it named in message. A field kept because
// another writer changed it after the run also carries that value as data (now, the field's JSON
// as the item's read answers it) and its writer (by: who wrote that field's change after the
// call's landing, absent where no event after the landing names one), so the command bar words
// the value as the item's page shows it; message stays for a bar deployed before now and by.
type goingUndoField struct {
	Field   string          `json:"field"`
	OK      bool            `json:"ok"`
	Message string          `json:"message"`
	Now     json.RawMessage `json:"now,omitempty"`
	By      *goingHolder    `json:"by,omitempty"`
}

// goingUndoAnswer is what an undo answers: one line for each landed call of the run, newest first.
type goingUndoAnswer struct {
	Lines []goingUndoLine `json:"lines"`
}

// undoGoingRun is POST /api/v1/going/runs/{id}/undo, made as the credential that pressed it. A
// run that is not the caller's, that the store does not hold, or that History no longer lists -
// its conversation's newest run is older than goingRunsKept - is 404 GOING_RUN_NOT_FOUND. Each
// landed call of the run is undone in turn, newest first (goingUndoer.undo), and the answer holds
// one line for each (walkGoingUndo). A keyed press first commits the run as its key's recovery
// point (bindGoingUndoRecovery), before the walk sends anything: a walk commits as it goes - each
// undo's landing, and an undo that landed nothing (recordUndo), which marks the key committed -
// so a press whose process ended mid-walk leaves its key committed, and a repeat under the key is
// answered by walking the run again (replayGoingUndo) rather than 409 KEY_COMMITTED.
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
	s.walkGoingUndo(w, r, caller, id.String())
}

// goingUndoRecovery is the recovery point of a keyed undo: the run it walks.
type goingUndoRecovery struct {
	Run string `json:"run"`
}

// replayGoingUndo is the undo route's keyedReplay: a repeat of a keyed press whose walk committed
// its recovery point walks the same run again as the repeat's caller - the key's own person - and
// answers its lines as the press does. The walk is idempotent by its landings: each call whose undo
// landed, or was recorded, answers from that record and sends nothing, so the repeat finishes what
// a press whose process ended left, and sends each remaining undo once. The first answer stored
// says nothing the walk does not say again.
func (s *server) replayGoingUndo(w http.ResponseWriter, r *http.Request, point goingUndoRecovery, _ []byte) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.walkGoingUndo(w, r, caller, point.Run)
}

// walkGoingUndo walks run's landed calls as caller, the person of r, and answers the lines: 404
// GOING_RUN_NOT_FOUND where the run is not the caller's or History no longer lists it. A keyed
// press binds the run as its key's recovery point before it sends anything; a replay holds no key
// (it answers a repeat the key layer did not let claim it), and binds nothing.
func (s *server) walkGoingUndo(w http.ResponseWriter, r *http.Request, caller model.Actor, run string) {
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
	if err := s.bindGoingUndoRecovery(r.Context(), run); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	doer := s.goingDoer(goingBarCaller(actor, human, run), PublicOrigin(s.deps.ServerURL, r))
	undoer := goingUndoer{s: s, doer: doer, observer: goingObserver{read: goingReader{doer: doer}, s: s}, catalogue: s.going,
		login: caller.ID, session: goingCredentialOf(r, actor, false).session()}
	lines := make([]goingUndoLine, 0, len(writes))
	for _, write := range writes {
		lines = append(lines, undoer.undo(r.Context(), write))
	}
	WriteJSON(w, http.StatusOK, goingUndoAnswer{Lines: lines})
}

// bindGoingUndoRecovery commits run as the recovery point of the keyed press ctx carries
// (bindRequestKeyRecovery), in a transaction of its own that writes nothing else, so it marks no
// key committed (markRequestKeyCommitted counts no write of request_keys): a press whose process
// ends before it sends anything is answered by its replay all the same. A press without a key binds
// nothing. A press whose key a repeat took over is refused (errRequestKeyTakenOver), and walks
// nothing: the repeat walks instead.
func (s *server) bindGoingUndoRecovery(ctx context.Context, run string) error {
	if _, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim); !keyed {
		return nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bindRequestKeyRecovery(ctx, tx, goingUndoRecovery{Run: run}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit an undo's recovery point: %w", err)
	}
	return nil
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
// read of its item the row was approved over (goingRowApproval.Seen), nil for none; each request
// it landed, in order; and when its last request landed (going_landings.landed_at), the now() of
// the transaction that committed it, which every event that transaction wrote carries as its
// created_at.
type goingLandedWrite struct {
	key       goingCallKey
	plan, row int
	tool      string
	arguments map[string]any
	seen      *goingRowShown
	requests  []goingLandedRequest
	landedAt  time.Time
}

// goingLandedWriteOf is a call of run as the undo reads it before its requests: its key and the
// row of the run's plan whose key it is - its plan and number (nil where no row is), its tool, and
// the arguments and approval the row records (nil for none). Arguments or an approval that do not
// decode are an error: the undo cannot read the call.
func goingLandedWriteOf(key goingCallKey, plan, row *int, tool string, arguments, approval []byte) (goingLandedWrite, error) {
	write := goingLandedWrite{key: key, tool: tool}
	if plan != nil && row != nil {
		write.plan, write.row = *plan, *row
	}
	var err error
	if write.arguments, err = goingRowArguments(arguments); err != nil {
		return write, fmt.Errorf("read row %d of plan %d of run %s: %w", write.row, write.plan, key.run, err)
	}
	if approval != nil {
		var approved goingRowApproval
		if err := json.Unmarshal(approval, &approved); err != nil {
			return write, fmt.Errorf("read the approval of row %d of plan %d of run %s: %w", write.row, write.plan, key.run, err)
		}
		write.seen = &approved.Seen
	}
	return write, nil
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
		  l.recovery, l.landed_at, p.plan, p.row, coalesce(p.change->>'tool', ''), p.change->'arguments', p.approval
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
		var landed time.Time
		var plan, row *int
		var tool string
		if err := rows.Scan(&key.step, &key.call, &request.method, &request.path, &request.digest, &request.status, &body,
			&recovery, &landed, &plan, &row, &tool, &arguments, &approval); err != nil {
			return nil, fmt.Errorf("read the landings of run %s: %w", run, err)
		}
		request.body, request.recovery = body, recovery
		if len(writes) == 0 || writes[len(writes)-1].key != key {
			write, err := goingLandedWriteOf(key, plan, row, tool, arguments, approval)
			if err != nil {
				return nil, err
			}
			writes = append(writes, write)
		}
		writes[len(writes)-1].requests = append(writes[len(writes)-1].requests, request)
		writes[len(writes)-1].landedAt = landed
	}
	return writes, rows.Err()
}

// goingUndoer undoes the landed calls of one run as the credential that pressed Undo: its person
// (login), and the session that credential proved, "" for none; catalogue is the server's going
// catalogue, whose rows name which request of a call is its own (goingRecordedCall).
type goingUndoer struct {
	s         *server
	doer      GoingDoer
	observer  goingObserver
	catalogue GoingCatalogue
	login     string
	session   string
}

// goingUndoSend is the one request an undo of a call sends: through route, its placeholders filled
// from values, with body (nil for none) and condition (nil for none), and the line the undo
// answers once it lands (landed). changed are the refusals of the route that say another writer
// changed the item between the undo's read and its request. changedMessage is the words of the
// kept line such a refusal answers, in place of the refusal's own, "" to keep the route's words.
type goingUndoSend struct {
	route          string
	values         map[string]string
	body           any
	condition      *goingCondition
	landed         goingUndoLine
	changed        []string
	changedMessage string
}

// goingUndoPlan is what the undo of one landed call makes of the call's record alone, before it
// reads the item now or sends anything (goingUndoer.plan). Exactly one of the two is set: settled
// builds the line the undo answers where the record settles the call - it left nothing Undo can
// give back, or its record does not say what it left - and next reads the item now and answers
// the line, with the one request the undo sends where it sends one. The settled line is built only
// when the undo answers it, so a read of the run that asks only what Undo does with the call
// (goingCallUndo) builds and logs nothing; stands says, beside settled, whether that line refuses
// the undo, so the call's change stands though Undo would take it back, or whether Undo tries
// nothing of the call by design: it changed nothing, or it is sent, not undone.
type goingUndoPlan struct {
	settled func() goingUndoLine
	stands  bool
	next    func(ctx context.Context) (goingUndoLine, *goingUndoSend)
}

// goingStanding is the plan of a call whose undo its record refuses with nothing sent, so its
// change stands: its undo is unknown or unserved, or its record does not say what it left or what
// it replaced, or names a claim the bar cannot take back. line builds the line.
func goingStanding(line func() goingUndoLine) goingUndoPlan {
	return goingUndoPlan{settled: line, stands: true}
}

// goingNotTried is the plan of a call Undo tries nothing of: one its record says changed nothing,
// and one that is sent, not undone by design - a message, a broadcast, a review, a write no route
// takes back. line builds the line.
func goingNotTried(line func() goingUndoLine) goingUndoPlan {
	return goingUndoPlan{settled: line}
}

// goingSending is the plan of a call whose undo sends send with nothing read first: the record
// alone says what to send, and the route's own condition decides whether the item still allows it.
func goingSending(line goingUndoLine, send *goingUndoSend) goingUndoPlan {
	return goingUndoPlan{next: func(context.Context) (goingUndoLine, *goingUndoSend) { return line, send }}
}

// undo undoes one landed call by the undo its tool states. A call whose undo landed before - an
// earlier press of Undo, or one made beside this one - is answered from that landing, and nothing
// is sent. Any other is decided first by its record (plan): a call the record settles is answered
// with nothing read or sent, and every other goes on to its tool's undo, which reads the item now
// and sends the one request that puts it back, where the item still holds what the call left. An
// undo whose read finds a change another writer made since the run keeps it with nothing sent,
// and that is recorded as the call's undo landing (recordUndo), as a refusal of the request is.
func (u goingUndoer) undo(ctx context.Context, write goingLandedWrite) goingUndoLine {
	if tool, held := goingWriteTools[write.tool]; held {
		landed, err := u.s.goingUndoLanded(ctx, write.key)
		if err != nil {
			line := goingUndoLine{Plan: write.plan, Row: write.row, Tool: write.tool, Undo: tool.Undo, Item: u.landedItem(write), Outcome: goingFailed(err)}
			return goingLineOf(line, nil)
		}
		if landed != nil {
			return *landed
		}
	}
	plan := u.plan(ctx, write)
	if plan.next == nil {
		return plan.settled()
	}
	line, send := plan.next(ctx)
	if send != nil {
		return u.send(ctx, write.key, *send)
	}
	if line.Outcome.Code == codeGoingUndoChanged {
		// Nothing was sent, so the landing names no request.
		return u.s.recordUndo(ctx, goingUndoLanding{key: write.key}, line, false)
	}
	return line
}

// plan is what the undo of write makes of the call's record alone - its row's tool, its last
// landed request and what that request's route recorded beside its landing - reading nothing
// else, so the run's read decides from it what Undo does with the call (goingCallUndo) and the
// two never differ. A call that names no row of the run's plan, or whose tool the table no longer
// holds, is refused, 409 GOING_UNDO_UNKNOWN. A call whose tool states no undo, or of which only
// the requests it sends before its own landed - a close's reason, a message - is named sent, not
// undone (a delete of a post deleted, not undone, goingNotUndoneWords): no undo takes a message
// back or restores a deleted post, and nothing else of the change stands. A call whose route
// recorded nothing that says what it found and left is refused, 409 GOING_UNDO_AFTER_UNREAD, and
// one whose route found a value the row's read did not see, 409 GOING_UNDO_BEFORE_UNREAD: nothing
// is put back. A call its record says changed nothing is undone by nothing (kind), and a kind this
// server has no way to run for the tool is refused, 501 GOING_UNDO_UNSERVED. Every other call goes
// on to its tool's undo. Each refusal here leaves the call's change standing (goingStanding); a
// call named sent, not undone, and one that changed nothing, Undo tries nothing of (goingNotTried).
func (u goingUndoer) plan(ctx context.Context, write goingLandedWrite) goingUndoPlan {
	line := goingUndoLine{Plan: write.plan, Row: write.row, Tool: write.tool, Undo: goingUndoNotUndone, Item: u.landedItem(write)}
	tool, held := goingWriteTools[write.tool]
	if !held {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoUnknown,
				Message: "this call names no write tool of the run's plan, so its undo is unknown and nothing was undone"}
			return goingLineOf(line, nil)
		})
	}
	line.Undo = tool.Undo
	if tool.Undo == goingUndoNotUndone {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingNotUndoneWords(write.tool)}
			return goingLineOf(line, nil)
		})
	}
	last := write.requests[len(write.requests)-1]
	prepared, own := goingRecordedCall(u.catalogue, write.tool, goingWriteRequest{Method: last.method, Path: last.path})
	if !own {
		return goingNotTried(func() goingUndoLine {
			line.Undo = goingUndoNotUndone
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: goingSentNotUndone}
			// The line names the row's item, not the message that landed.
			if write.seen != nil && write.seen.Item != "" {
				line.Item = write.seen.Item
			} else if issue := goingIssueOfPath(last.path); issue != "" {
				line.Item = issue
			}
			return goingLineOf(line, nil)
		})
	}
	switch write.tool {
	case "dispatch_suggest":
		return u.suggestion(write, line)
	case "dispatch_doc_edit":
		return u.document(write, prepared, line)
	}
	undone, err := u.recorded(ctx, write, prepared)
	line.Item = cmp.Or(undone.target, line.Item)
	switch {
	case err != nil:
		return goingStanding(func() goingUndoLine {
			slog.Warn("dispatch: read what a command-bar change left off its record", "run", write.key.run, "tool", write.tool, "error", err)
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
				Message: "what this change left cannot be read off its record, so nothing was put back: read the item, and change it by hand to undo this change"}
			return goingLineOf(line, undone.fields)
		})
	case undone.beforeUnread:
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoBeforeUnread,
				Message: "another writer changed the item between the read the change was approved over and the change, so what it replaced is unknown and nothing was put back: read the item to see what it holds"}
			return goingLineOf(line, undone.fields)
		})
	}
	return u.kind(line, undone)
}

// kind is the undo of a call by the undo its tool states, from what the call found and left: the
// line its record settles it with, or the undo that reads the item now and answers the line, with
// the request it sends.
func (u goingUndoer) kind(line goingUndoLine, undone goingUndone) goingUndoPlan {
	switch {
	case line.Undo == goingUndoWriteBack && undone.tool == "dispatch_issue_update":
		return u.issueFields(line, undone)
	case line.Undo == goingUndoWriteBack && undone.tool == "dispatch_edit_ask":
		return u.askFields(line, undone)
	case line.Undo == goingUndoWriteBack && (undone.tool == "dispatch_edit_comment" || undone.tool == "dispatch_edit_message"):
		return u.postBody(line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_claim":
		return u.claim(line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_follow":
		return u.follow(line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_resolve_comment":
		return u.comment(line, undone)
	case line.Undo == goingUndoReverse && undone.tool == "dispatch_reopen_comment":
		return u.reopened(line, undone)
	case line.Undo == goingUndoReverse && (undone.tool == "dispatch_ask" || undone.tool == "dispatch_request_approval"):
		return u.retract(line, undone)
	case line.Undo == goingUndoCloseCreated && undone.tool == "dispatch_issue":
		return u.closeCreated(line, undone)
	}
	return goingStanding(func() goingUndoLine {
		line.Outcome = goingWriteOutcome{Status: http.StatusNotImplemented, Code: codeGoingUndoUnserved,
			Message: "Dispatch has no way to run this change's undo, so nothing was undone"}
		return goingLineOf(line, undone.fields)
	})
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

// goingLineKeptBy adds the writer to a kept line with no structured value to show.
func goingLineKeptBy(line goingUndoLine, fields []string, writer *model.Actor) goingUndoLine {
	line = goingLineOf(line, fields)
	if writer != nil {
		by := &goingHolder{Kind: writer.Kind, ID: writer.ID}
		for i := range line.Fields {
			line.Fields[i].By = by
		}
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
	if prepared, held := goingRecordedCall(u.catalogue, write.tool, goingWriteRequest{Method: last.method, Path: last.path}); held {
		return goingTargetOfPath(prepared.call.path)
	}
	return ""
}

// send makes the one request of an undo, in-process as the caller, carrying its landing
// (goingUndoLanding), which records it and the line it answers in the route's own transaction.
// Whether it landed is read from that landing, never from the status the route answered. Where
// the call's undo landed, the request answers that landing's line: its own, whatever its route
// answered (a route may commit and then answer 5xx), or another press's, whose request of the same
// undo landed first and rolled this one back. A request answered below 300 where nothing landed
// changed nothing - its route found the item already as the undo asked, as a follow of a session
// that follows the ask already answers 204 - and says so; a refusal `changed` names is another
// writer's change since the undo's read, which is kept, in the send's own words where it names
// them (changedMessage). Either is recorded as the call's undo landing in a statement of its own
// (recordUndo), since the route's transaction recorded none.
// Otherwise nothing of it landed: the line is the route's refusal, and every field the undo would
// have put back is left with it, for a later press to try again.
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
	landed, err := u.s.goingUndoLanded(ctx, key)
	if err != nil {
		return goingRefusedLine(line, goingFailed(err))
	}
	if landed != nil {
		return *landed
	}
	if response.StatusCode < http.StatusMultipleChoices {
		return u.s.recordUndo(ctx, landing, goingUnchangedLine(line, response.StatusCode), true)
	}
	outcome := goingRefusalOf(response, answer).outcome()
	if slices.Contains(send.changed, outcome.Code) {
		message := outcome.Message
		if send.changedMessage != "" {
			message = send.changedMessage
		}
		return u.s.recordUndo(ctx, landing, goingRefusedLine(line, goingChanged([]string{message}, nil)), false)
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

// goingUndoChangedNothing is what an undo says of a call whose one request its route answered as
// done while it committed nothing: the item already stood as the undo asked, so nothing of the
// call's change was left to undo. It is worded as the undos a record settles word a change that
// changed nothing.
const goingUndoChangedNothing = "the undo's route answered that it changed nothing - the item already stood as the undo asked - so there is nothing to undo"

// goingUnchangedLine is line where its request was answered status, below 300, and committed
// nothing: the line and every field it would have put back say the undo changed nothing, and every
// field it kept stays kept.
func goingUnchangedLine(line goingUndoLine, status int) goingUndoLine {
	line.Outcome = goingWriteOutcome{OK: true, Status: status, Message: goingUndoChangedNothing}
	line.Fields = slices.Clone(line.Fields)
	for index, field := range line.Fields {
		if field.OK {
			line.Fields[index] = goingUndoField{Field: field.Field, OK: true, Message: goingUndoChangedNothing}
		}
	}
	return line
}

// recordUndo records line as the undo landing of landing's call where nothing of its undo landed
// in a route's transaction, in a transaction of its own (insertUndoLanding), under the call's key,
// phase undo, request 0: an undo whose request its route answered as done while it committed
// nothing (unchanged, the line goingUnchangedLine), and an undo that keeps a change another writer
// made since the run (GOING_UNDO_CHANGED), found by the undo's read with nothing sent - its
// landing then names no request - or by its route's refusal. Both are settled: a second press
// answers the call from the landing, as from any undo landing, and sends nothing again, and the
// run's read counts a kept change as one Undo leaves standing and one that changed nothing in no
// count (goingRunUndoColumns). Where another press's request of the same undo landed first, that
// landing stands and its line is answered. A record that fails is logged and the line still
// answered: a later press then decides the call again.
func (s *server) recordUndo(ctx context.Context, landing goingUndoLanding, line goingUndoLine, unchanged bool) goingUndoLine {
	recorded, err := s.insertUndoLanding(ctx, landing, line, unchanged)
	if err == nil && !recorded {
		// Another press's request of the same undo landed first.
		var landed *goingUndoLine
		if landed, err = s.goingUndoLanded(ctx, landing.key); landed != nil {
			return *landed
		}
	}
	if err != nil {
		slog.Error("dispatch: record a command-bar undo that landed nothing", "run", landing.key.run, "error", err)
	}
	return line
}

// insertUndoLanding writes the undo landing recordUndo records, and reports whether it wrote it:
// false where the call holds an undo landing already. It writes in a transaction of the shared pool
// (s.begin), so the undo request's commit step marks its Idempotency-Key committed with the
// landing (markRequestKeyCommitted), and a repeat after a crash answers KEY_COMMITTED rather than
// writing it again.
func (s *server) insertUndoLanding(ctx context.Context, landing goingUndoLanding, line goingUndoLine, unchanged bool) (bool, error) {
	answer, err := json.Marshal(goingUndoLandingAnswer{Status: line.Outcome.Status, Body: json.RawMessage("null"), Line: line, Unchanged: unchanged})
	if err != nil {
		return false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var wrote bool
	err = tx.QueryRow(ctx, `
		insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
		values ($1::uuid, 'undo', $2, $3, 0, $4, $5, $6, $7::jsonb)
		on conflict do nothing
		returning true
	`, landing.key.run, landing.key.step, landing.key.call, landing.method, landing.path, landing.digest, string(answer)).Scan(&wrote)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record an undo that landed nothing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit an undo that landed nothing: %w", err)
	}
	return true, nil
}

// goingUndoLandingAnswer is what an undo landing records beside the request: the status and body
// its route answered of its transaction (goingRecordAnswer), the line the undo answers, and
// unchanged for a request its route answered as done while it committed nothing (recordUndo).
type goingUndoLandingAnswer struct {
	Status    int             `json:"status"`
	Body      json.RawMessage `json:"body"`
	Line      goingUndoLine   `json:"line"`
	Unchanged bool            `json:"unchanged,omitempty"`
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
		answer, err := json.Marshal(goingUndoLandingAnswer{Status: status, Body: goingJSONOrNullRaw(body), Line: line})
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
// that it found a value the read its row was approved over did not see (beforeUnread). seq is the
// issue's last_seq the route of an issue's update or create answered it left - the sequence of the
// newest event the call wrote on the issue, 0 where the answer names none; an answer whose last_seq
// does not decode as a sequence is an error (recorded) - and landedAt when the call landed
// (goingLandedWrite.landedAt): what came after the call is read from them, never from the item's
// newest event, which may be anything of anyone's.
type goingUndone struct {
	tool         string
	target       string
	fields       []string
	before       json.RawMessage
	after        json.RawMessage
	unchanged    bool
	beforeUnread bool
	seq          int
	landedAt     time.Time
}

// recorded is what write found and left, from what the route of its last request recorded beside
// that request's landing, in the transaction of the change (goingRecordAnswer): the call's row is
// prepared, the one of its tool that request names (goingRecordedCall), which plan found before it
// came here, and nothing of the call is translated or read before again. The value it found is
// what its route answered it found (goingObservation.stood), else the value of the read its row
// was approved over; whether the call changed anything is its landing, which it has. A last
// request that recorded nothing, or whose record observe cannot read or whose last_seq does not
// decode as a sequence, is an error: what the call left, or the event it left it at, is unknown.
func (u goingUndoer) recorded(ctx context.Context, write goingLandedWrite, prepared goingPrepared) (goingUndone, error) {
	last := write.requests[len(write.requests)-1]
	undone := goingUndone{tool: write.tool, fields: goingChangedFields(write), landedAt: write.landedAt}
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
	var issue struct {
		LastSeq int `json:"last_seq"`
	}
	if err := json.Unmarshal(last.body, &issue); err != nil {
		return undone, fmt.Errorf("read the last_seq the route of %s %s recorded beside its landing: %w", last.method, last.path, err)
	}
	undone.seq = issue.LastSeq
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

// issueFieldWriter is who changed field of the issue key after the call that left the issue at
// seq (goingUndone.seq): the actor of the issue's newest event after seq whose changed list names
// field (issueChangedFields). nil where the record names no seq or no event after it names the
// field, so a comment, a claim or a change of another field made since never names this field's
// writer.
func (u goingUndoer) issueFieldWriter(ctx context.Context, key, field string, seq int) *model.Actor {
	if seq <= 0 {
		return nil
	}
	return u.eventActor(ctx, `
		select actor from events
		where issue_key = $1 and seq > $2 and payload->'changed' @> jsonb_build_array($3::text)
		order by seq desc limit 1
	`, key, seq, field)
}

// askPartWriter is who changed part of the ask id after the edit that landed at landed
// (goingUndone.landedAt): the actor of the ask's newest ask.edited whose part differs from the
// part it replaced (previous), written by a transaction begun after the landing's - its
// created_at, that transaction's now(), is later; the edit's own event carries the landing's
// moment and is never one. An edit that began before the landing's and waited on the ask's owner
// lock is not read, which leaves the part unnamed rather than misnamed. nil where none is.
func (u goingUndoer) askPartWriter(ctx context.Context, id, part string, landed time.Time) *model.Actor {
	return u.eventActor(ctx, `
		select actor from events
		where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited') and type = 'ask.edited'
		  and payload->>'id' = $1 and created_at > $3
		  and payload->$2::text is distinct from payload->'previous'->$2::text
		order by id desc limit 1
	`, id, part, landed)
}

// postBodyWriter is who wrote the body the comment or the issue message id holds after the edit
// that landed at landed (goingUndone.landedAt): the actor of the post's newest edit (kind
// "comment" or "message", its comment.edited or message.edited) written by a transaction begun
// after the landing's - its created_at, that transaction's now(), is later; the edit's own event
// carries the landing's moment and is never one. nil where none is. The type list is the one
// events_post_text_changes indexes, so the planner reads that index.
func (u goingUndoer) postBodyWriter(ctx context.Context, kind, id string, landed time.Time) *model.Actor {
	return u.eventActor(ctx, `
		select actor from events
		where type in ('comment.deleted', 'comment.edited', 'message.deleted', 'message.edited') and type = $1
		  and payload->>'id' = $2 and created_at > $3
		order by id desc limit 1
	`, kind+".edited", id, landed)
}

// documentVersionWriter is who wrote version, the version the document is at now, after the edit
// that landed at landed: the actor of the document's artifact.version of that number written by a
// transaction begun after the landing's (created_at later), read on the document's owner - its
// issue, or the document itself for a project document (ownerForArtifact). nil where version is
// none or was written before, as the edit's own version was.
func (u goingUndoer) documentVersionWriter(ctx context.Context, document goingDocumentRead, version *int, landed time.Time) *model.Actor {
	if version == nil {
		return nil
	}
	owner, key := "artifact_id = $1::uuid", document.ID
	if document.IssueKey != nil {
		owner, key = "issue_key = $1", *document.IssueKey
	}
	return u.eventActor(ctx, `
		select actor from events
		where `+owner+` and type = 'artifact.version' and payload->>'artifact_id' = $2
		  and payload->'version'->>'number' = $3 and created_at > $4
		order by id desc limit 1
	`, key, document.ID, strconv.Itoa(*version), landed)
}

// eventActor is the actor of the one event query answers, nil where it answers none. A read that
// fails is logged and names nobody: the undo then says "another writer" rather than a wrong name.
func (u goingUndoer) eventActor(ctx context.Context, query string, arguments ...any) *model.Actor {
	var actor model.Actor
	err := u.s.deps.Store.Pool.QueryRow(ctx, query, arguments...).Scan(&actor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("dispatch: read who changed a value an undo keeps", "error", err)
	}
	if err != nil || actor.ID == "" {
		return nil
	}
	return &actor
}

// goingWriterText names a writer as a claim refusal names a holder (claimHolder): "another
// writer" for none.
func goingWriterText(actor *model.Actor) string {
	if actor == nil {
		return "another writer"
	}
	return claimHolder(*actor, "")
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

// goingKeptField is the outcome of a field another writer changed after the run: what it holds now
// (held) and who changed it after the call landed (by, nil where no event after the landing names
// one), as data and in words, the words without naming the field (goingFieldChange).
func goingKeptField(held json.RawMessage, by *model.Actor) goingUndoField {
	kept := goingUndoField{
		Message: fmt.Sprintf("holds %s now, which %s wrote after this run, so it is kept", held, goingWriterText(by)),
		Now:     held,
	}
	if by != nil {
		kept.By = &goingHolder{Kind: by.Kind, ID: by.ID}
	}
	return kept
}

// goingRank is the field an issue's rank is recorded under. Its value is the issue's place
// (issueRankPlace): its rank and the neighbours it stood between, since a rank is written by
// naming neighbours.
const goingRank = "rank"

// issueFields writes back each field of the issue the call changed that still holds what the call
// left, and keeps each one another writer changed since, naming who changed that field after the
// call (issueFieldWriter). The rank is put back between the neighbours the issue stood between
// before the call (rankRestore), while the issue's rank is still the one the call left; with
// neither neighbour an issue of the project now, the rank is left, naming why. The advances
// relation is put back as the issue's own priority ids before the call, carrying the revision the
// call left as the route's own compare-and-set (advances_revision): a writer who changed the
// relation since moved its revision. Every other field the update expects to hold what the call
// left (goingCondition.Expect), so a change another writer makes after the read is refused by the
// update itself.
func (u goingUndoer) issueFields(line goingUndoLine, undone goingUndone) goingUndoPlan {
	read := func(ctx context.Context, fields []string) (map[string]any, error) {
		current, err := u.observer.issueFields(ctx, undone.target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(ctx context.Context, field string, held, before, after json.RawMessage) (any, goingUndoField, error) {
		who := func() *model.Actor { return u.issueFieldWriter(ctx, undone.target, field, undone.seq) }
		switch field {
		case goingAdvances:
			var heldValue, afterValue goingAdvancesValue
			if json.Unmarshal(held, &heldValue) != nil || json.Unmarshal(after, &afterValue) != nil {
				return nil, goingUndoField{Message: "the change's record holds no revision to write back against, so it is kept"}, nil
			}
			if heldValue.Revision != afterValue.Revision {
				return nil, goingUndoField{Message: fmt.Sprintf("holds revision %d now, which %s wrote after this run, so it is kept",
					heldValue.Revision, goingWriterText(who()))}, nil
			}
			var beforeValue goingAdvancesValue
			if json.Unmarshal(before, &beforeValue) != nil || beforeValue.IDs == nil {
				return nil, goingUndoField{Message: "the change's record holds no priority ids to write back, so it is kept"}, nil
			}
			return beforeValue.IDs, goingUndoField{}, nil
		case goingRank:
			var heldPlace, beforePlace, afterPlace issueRankPlace
			if json.Unmarshal(held, &heldPlace) != nil || json.Unmarshal(before, &beforePlace) != nil || json.Unmarshal(after, &afterPlace) != nil {
				return nil, goingUndoField{Message: "the change's record holds no place to put the issue back in, so it is kept"}, nil
			}
			// Its neighbours move as other issues are ranked; only its own rank says whether another
			// writer moved this issue.
			if heldPlace.Rank != afterPlace.Rank {
				return nil, goingUndoField{Message: fmt.Sprintf("the issue stands between %s and %s now, where %s moved it after this run, so it is kept",
					goingNeighbourText(heldPlace.After), goingNeighbourText(heldPlace.Before), goingWriterText(who()))}, nil
			}
			restore, why, err := u.observer.rankRestore(ctx, undone.target, beforePlace)
			return restore, goingUndoField{Message: why}, err
		default:
			if !goingSameJSON(held, after) {
				return nil, goingKeptField(held, who()), nil
			}
			return goingWriteBackValue(field, before), goingUndoField{}, nil
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
// and keeps each one another writer changed since, naming who changed that part after the edit
// (askPartWriter); the edit expects each part it writes back to hold what the edit left
// (goingCondition.Expect).
func (u goingUndoer) askFields(line goingUndoLine, undone goingUndone) goingUndoPlan {
	read := func(ctx context.Context, fields []string) (map[string]any, error) {
		current, err := u.observer.askFields(ctx, undone.target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(ctx context.Context, field string, held, before, after json.RawMessage) (any, goingUndoField, error) {
		if !goingSameJSON(held, after) {
			return nil, goingKeptField(held, u.askPartWriter(ctx, undone.target, field, undone.landedAt)), nil
		}
		return goingJSONOrNullRaw(before), goingUndoField{}, nil
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

// postBody writes back the body an edit of a comment or an issue message replaced, through the
// edit route, while the post still holds the body the edit left: the edit expects that body
// (goingCondition.ExpectBody), decided under the post's owner lock, so words written since are
// kept and named with who wrote them after the edit (postBodyWriter), and the route's 409
// COMMENT_CHANGED or MESSAGE_CHANGED is the post changed between the undo's read and its write. A
// post its author deleted since is kept as the delete left it, named deleted and by whom, with no
// word of its body: nothing restores a deleted post, and no read of one gives its words again; the
// route's 409 POST_DELETED is a delete made between the read and the write.
func (u goingUndoer) postBody(line goingUndoLine, undone goingUndone) goingUndoPlan {
	kind := goingPostTools[undone.tool]
	route, changed := goingCommentEditRoute, codeCommentChanged
	if kind == "message" {
		route, changed = goingMessageEditRoute, codeMessageChanged
	}
	// deleted is the delete's mark where the read below found the post deleted.
	var deleted *goingPostNow
	read := func(ctx context.Context, _ []string) (map[string]any, error) {
		current, err := u.observer.post(ctx, undone.tool, undone.target)
		if err != nil {
			return nil, err
		}
		now, _ := current.value.(goingPostNow)
		if current.deleted || now.Body == nil {
			deleted = &now
			return map[string]any{}, nil
		}
		return map[string]any{"body": *now.Body}, nil
	}
	restoreOf := func(ctx context.Context, _ string, held, before, after json.RawMessage) (any, goingUndoField, error) {
		switch {
		case deleted != nil:
			var by *model.Actor
			if deleted.DeletedBy != nil {
				by = &model.Actor{Kind: deleted.DeletedBy.Kind, ID: deleted.DeletedBy.ID}
			}
			return nil, goingUndoField{By: deleted.DeletedBy, Message: fmt.Sprintf("the %s was deleted after this run, by %s, so no words were put back",
				kind, goingWriterText(by))}, nil
		case !goingSameJSON(held, after):
			return nil, goingKeptField(held, u.postBodyWriter(ctx, kind, undone.target, undone.landedAt)), nil
		}
		return goingJSONOrNullRaw(before), goingUndoField{}, nil
	}
	conditionOf := func(restored map[string]json.RawMessage) (map[string]any, goingCondition) {
		// A body the record does not hold as text leaves the write back with no condition, which
		// its route refuses with nothing written (GOING_CONDITION_MISSING).
		var body string
		if json.Unmarshal(restored["body"], &body) != nil {
			return nil, goingCondition{}
		}
		return nil, goingCondition{ExpectBody: &body}
	}
	return u.writeBack(line, undone, read, route, map[string]string{"id": undone.target}, restoreOf, conditionOf,
		[]string{changed, codePostDeleted})
}

// writeBack is a write back of the fields one call changed, by one PATCH through route, its
// placeholders filled from values. A record that holds no value the call found and left of its
// fields settles the call: what it left is unknown, so nothing is written back. Otherwise read
// reads what each field holds now, and restoreOf says, from that, what it held before the call and
// what the call left it, the value the PATCH writes back, or why the field is kept - another
// writer changed it since - as that field's outcome with a message (goingKeptField), or fails,
// which fails the whole write back before anything is sent.
// conditionOf says what guards the write back, from the fields restored and what the call left
// them: the condition the PATCH carries on its context, marked as a write back (Expect,
// going_conditions.go), and the body keys of a precondition token a field carries instead
// (advances_revision), each decided under the lock the route takes. changed are the route's
// refusals that say another writer changed a field between the read and the PATCH. Each field's
// own outcome is answered beside the call's; a call whose every field is kept sends nothing.
func (u goingUndoer) writeBack(
	line goingUndoLine, undone goingUndone, read func(context.Context, []string) (map[string]any, error),
	route string, values map[string]string,
	restoreOf func(ctx context.Context, field string, held, before, after json.RawMessage) (any, goingUndoField, error),
	conditionOf func(restored map[string]json.RawMessage) (map[string]any, goingCondition), changed []string,
) goingUndoPlan {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil || len(after) == 0 {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the change's record holds no field values to write back"))
			return goingLineOf(line, undone.fields)
		})
	}
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
		fields := slices.Sorted(maps.Keys(after))
		now, err := read(ctx, fields)
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, fields), nil
		}
		restore := map[string]any{}
		restoredAfter := map[string]json.RawMessage{}
		var changes, restored []string
		line.Fields = make([]goingUndoField, 0, len(fields))
		for _, field := range fields {
			value, kept, err := restoreOf(ctx, field, goingJSONOrNull(now[field]), goingJSONOrNullRaw(before[field]), after[field])
			if err != nil {
				line.Outcome = goingFailed(err)
				return goingLineOf(line, fields), nil
			}
			if kept.Message != "" {
				changes = append(changes, goingFieldChange(field, kept.Message))
				kept.Field = field
				line.Fields = append(line.Fields, kept)
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
	}}
}

// document writes the content the document held before the edit back over it, as a further
// version uploaded to its owner's route under its slug, only while the document is what the edit
// left: its token now is the one the edit's route answered it left (editArtifactResponse), and the
// upload names that token, the one its read returned, as its precondition, as `dispatch artifact
// --precondition-json` names one, and that very document by its id (ExpectArtifact of
// goingCondition). The upload route decides the id under the owner's lock and the document write
// checks the token under the document's own locks, so an edit another writer makes after the
// read is never written over (409 PRECONDITION_FAILED), nor a document made under the slug since
// (409 DOCUMENT_CHANGED), and either is kept, in the undo's own words (goingUndoDocumentChanged).
// A document another writer changed since the edit is kept, naming its version and who wrote that
// version after the edit (documentVersionWriter).
// What the edit replaced is the content of the read its row was approved over, where the approval
// holds that read, the read's token is the one the edit's route answered it found, and the read
// holds the document's content; otherwise what it replaced is unknown, the words saying which of
// the three it is, and nothing is written back: that, and a record that does not name the tokens,
// the record settles. prepared is the edit's row that its last request names (goingRecordedCall),
// which plan found before it came here.
func (u goingUndoer) document(write goingLandedWrite, prepared goingPrepared, line goingUndoLine) goingUndoPlan {
	fields := goingChangedFields(write)
	last := write.requests[len(write.requests)-1]
	var edited struct {
		Token         string `json:"token"`
		PreviousToken string `json:"previous_token"`
	}
	if json.Unmarshal(last.body, &edited) != nil || edited.Token == "" || edited.PreviousToken == "" {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
				Message: "the edit's record does not name the document it found and the one it left, so nothing was written back: read the document, and change it by hand to undo this edit"}
			return goingLineOf(line, fields)
		})
	}
	var before goingDocumentValue
	var unread string
	var unreadable error
	switch {
	case write.seen == nil:
		unread = "the edit's approval holds no read of the document"
	case write.seen.Token != edited.PreviousToken:
		unread = "another writer changed the document between the read the edit was approved over and the edit"
	default:
		if err := json.Unmarshal(write.seen.Now, &before); err != nil {
			unread, unreadable = "the read the edit was approved over does not hold the document's content", err
		}
	}
	if unread != "" {
		return goingStanding(func() goingUndoLine {
			if unreadable != nil {
				slog.Warn("dispatch: read the document an edit was approved over off its record", "run", write.key.run, "error", unreadable)
			}
			if write.seen != nil && write.seen.Item != "" {
				line.Item = write.seen.Item
			}
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoBeforeUnread,
				Message: unread + ", so what it replaced is unknown and nothing was written back: read the document to see what it holds"}
			return goingLineOf(line, fields)
		})
	}
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
		document := write.seen.Item
		if document == "" {
			prepared.arguments = write.arguments
			resolved, err := u.observer.editedDocument(ctx, prepared)
			if err != nil {
				line.Outcome = goingFailed(err)
				return goingLineOf(line, fields), nil
			}
			document = resolved
		}
		// The slug is read beside the rest, since it is the name the write back below sends.
		var artifact struct {
			goingDocumentRead
			Slug string `json:"slug"`
		}
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
			writer := u.documentVersionWriter(ctx, artifact.goingDocumentRead, now.Version, write.landedAt)
			line.Outcome = goingChanged([]string{fmt.Sprintf("the document is at version %s now, which %s wrote after this run, so it is kept",
				goingVersionText(now.Version), goingWriterText(writer))}, nil)
			return goingLineKeptBy(line, fields, writer), nil
		}
		route, values := goingProjectUploadRoute, map[string]string{"key": artifact.Project}
		if artifact.IssueKey != nil {
			route, values = goingIssueUploadRoute, map[string]string{"key": *artifact.IssueKey}
		}
		line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "written back"}
		// The upload names the document by its slug, not its name. The route takes the artifact
		// whose name or slug the name is, the one whose slug it is first (findArtifactByNameOrSlug),
		// and an owner may hold another document whose slug is this one's name (artifacts.go,
		// artifactByNameOrSlugSQL), so the name could reach that other document. A slug is one
		// artifact's alone within its owner (artifacts_ref_key_key) and never changes, so the slug
		// reaches the document read above while it stands; the condition names that document by
		// its id too, so one made under its slug after it was removed is never written over.
		return goingLineOf(line, fields), &goingUndoSend{route: route, values: values,
			body: map[string]any{"name": artifact.Slug, "content": before.Content, "summary": goingUndoReason,
				"precondition": map[string]any{"document": token}},
			condition: &goingCondition{WriteBack: true, ExpectArtifact: artifact.ID},
			landed:    goingLineOf(line, fields), changed: []string{"DOCUMENT_CHANGED", "PRECONDITION_FAILED"},
			changedMessage: goingUndoDocumentChanged}
	}}
}

// goingUndoDocumentChanged is what the undo of a document edit says where the upload's route
// refuses its write back as another writer's change since the undo's read (DOCUMENT_CHANGED,
// PRECONDITION_FAILED). The route's own words tell an uploader to read a fresh token and upload
// again, which is no step for the person reading the bar.
const goingUndoDocumentChanged = "another writer changed the document after this undo read it, so it is kept and nothing was written back"

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
// claim route in its own transaction; what to send is decided from the record alone. A claim is
// released only while the holder it left - as its route answered it - still holds it
// (goingCondition.ExpectHolder); a release is taken back only for the holder it released - as its
// route answered it, previous_claim - and only where the claim the undo makes would be recorded as
// that very holder (claimsAs) and nobody holds the issue (goingCondition.ExpectUnclaimed). A
// release of another's claim is refused, 409 GOING_UNDO_NOT_CALLERS_CLAIM, with nothing written:
// the bar cannot claim for that holder. Either route refusing its condition, 409
// CLAIM_HOLDER_CHANGED, is the claim changed by another writer since, and it is kept. A claim or a
// release its route answered changed nothing is undone by nothing.
func (u goingUndoer) claim(line goingUndoLine, undone goingUndone) goingUndoPlan {
	var before, after *goingHolder
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the change's record holds no claim to undo"))
			return goingLineOf(line, undone.fields)
		})
	}
	values := map[string]string{"key": undone.target}
	reversed := line
	reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
	reversed = goingLineOf(reversed, undone.fields)
	changed := []string{codeClaimHolderChanged}
	switch {
	case undone.unchanged:
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK,
				Message: "the claim route answered that this change changed nothing - the claim already stood as it asked - so there is nothing to undo"}
			return goingLineOf(line, undone.fields)
		})
	case after != nil:
		// The body is empty: a bearer's release reads its actor from one, and a person's none.
		return goingSending(line, &goingUndoSend{route: goingReleaseRoute, values: values, body: map[string]any{},
			condition: &goingCondition{WriteBack: true, ExpectHolder: &model.Actor{Kind: after.Kind, ID: after.ID}}, landed: reversed, changed: changed})
	case before == nil:
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "nobody held the claim before this release, so there is nothing to take back"}
			return goingLineOf(line, undone.fields)
		})
	case !u.claimsAs(before):
		return goingStanding(func() goingUndoLine {
			claimant := u.claimant()
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoNotCallers, Message: fmt.Sprintf(
				"this release released the claim of %s, which the bar cannot claim for them: its claim would be recorded as %s, which never held it, so nothing was taken back",
				goingHolderText(before), goingHolderText(&claimant))}
			return goingLineOf(line, undone.fields)
		})
	}
	return goingSending(line, &goingUndoSend{route: goingClaimRoute, values: values, body: map[string]any{},
		condition: &goingCondition{WriteBack: true, ExpectUnclaimed: true}, landed: reversed, changed: changed})
}

// follow undoes a follow by unfollowing and an unfollow by following again, while the session
// still follows as the change left it. A change that left the session as it found it is undone by
// nothing.
func (u goingUndoer) follow(line goingUndoLine, undone goingUndone) goingUndoPlan {
	var before, after goingFollowing
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the change's record holds no follow to undo"))
			return goingLineOf(line, undone.fields)
		})
	}
	if before.Following == after.Following {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the change changed nothing, so there is nothing to undo"}
			return goingLineOf(line, undone.fields)
		})
	}
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
		following, err := u.observer.follows(ctx, undone.target, after.Session)
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, undone.fields), nil
		}
		if following != after.Following {
			state := "does not follow the ask now"
			if following {
				state = "follows the ask now"
			}
			line.Outcome = goingChanged([]string{fmt.Sprintf("session %s %s, which another writer left after this run, so it is kept", after.Session, state)}, nil)
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
	}}
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
// resolution, the route's 409 COMMENT_CHANGED being the thread changed. A resolve that found the
// thread resolved changed nothing, and is undone by nothing; a record of a resolve that names no
// moment of it holds nothing to reopen against.
func (u goingUndoer) comment(line goingUndoLine, undone goingUndone) goingUndoPlan {
	var before, after goingCommentResolution
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil ||
		after.Resolved && !before.Resolved && after.ResolvedAt == nil {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the change's record holds no resolve to undo"))
			return goingLineOf(line, undone.fields)
		})
	}
	if before.Resolved || !after.Resolved {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was resolved before this change, so there is nothing to reopen"}
			return goingLineOf(line, undone.fields)
		})
	}
	resolvedAt := *after.ResolvedAt
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
		current, err := u.observer.commentResolved(ctx, undone.target)
		if err != nil {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, undone.fields), nil
		}
		now, _ := current.value.(goingCommentResolution)
		switch {
		case !now.Resolved:
			line.Outcome = goingChanged([]string{"the thread is open now, reopened by another writer after this run"}, nil)
			return goingLineOf(line, undone.fields), nil
		case now.ResolvedAt == nil || *now.ResolvedAt != resolvedAt:
			line.Outcome = goingChanged([]string{"the thread was resolved again after this run, by another writer, so it is kept"}, nil)
			return goingLineOf(line, undone.fields), nil
		}
		reversed := line
		reversed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "reversed"}
		return line, &goingUndoSend{route: goingCommentReopenRoute, values: map[string]string{"id": undone.target},
			condition: &goingCondition{WriteBack: true, ExpectResolvedAt: &resolvedAt}, landed: goingLineOf(reversed, undone.fields),
			changed: []string{codeCommentChanged}}
	}}
}

// reopened undoes a reopen by resolving the thread again, while it is still open as the run left
// it: the resolve expects an open thread (goingCondition.ExpectOpen), decided under the comment's
// lock, so a thread another writer resolved since keeps their resolution, the route's 409
// COMMENT_CHANGED being the thread changed. A reopen that found the thread open changed nothing,
// and is undone by nothing.
func (u goingUndoer) reopened(line goingUndoLine, undone goingUndone) goingUndoPlan {
	var before, after goingCommentResolution
	if json.Unmarshal(undone.before, &before) != nil || json.Unmarshal(undone.after, &after) != nil {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the change's record holds no reopen to undo"))
			return goingLineOf(line, undone.fields)
		})
	}
	if !before.Resolved || after.Resolved {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was open before this change, so there is nothing to resolve"}
			return goingLineOf(line, undone.fields)
		})
	}
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
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
	}}
}

// retract undoes an ask the run opened, an approval request's among them, by retracting it while
// it is still open. An approval request its route answered opened no ask - it renewed one already
// open - is undone by nothing. A record that does not say whether an approval request opened its
// ask, or says it opened one and does not name it, is the server's own failure, and retracts
// nothing.
func (u goingUndoer) retract(line goingUndoLine, undone goingUndone) goingUndoPlan {
	failed := func(err error) goingUndoPlan {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(err)
			return goingLineOf(line, undone.fields)
		})
	}
	nothing := func(message string) goingUndoPlan {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: message}
			return goingLineOf(line, undone.fields)
		})
	}
	var after struct {
		ID     string `json:"id"`
		Ask    any    `json:"ask"`
		Opened *bool  `json:"opened"`
	}
	if err := json.Unmarshal(undone.after, &after); err != nil {
		return failed(fmt.Errorf("the change's record holds no ask to retract: %w", err))
	}
	ask := after.ID
	if undone.tool == "dispatch_request_approval" {
		if after.Opened == nil {
			return failed(errors.New("the change's record does not say whether the approval request opened its ask"))
		}
		if !*after.Opened {
			return nothing("the approval route answered that this change opened no ask - it renewed one already open - so there is none to retract")
		}
		opened, named := after.Ask.(string)
		if !named || opened == "" {
			return failed(errors.New("the change's record says the approval request opened an ask and does not name it"))
		}
		ask = opened
	}
	if ask == "" {
		return nothing("this change opened no ask, so there is none to retract")
	}
	line.Item = ask
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
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
	}}
}

// closeCreated closes an issue the run created, posting nothing on it: its status set done, while
// it still holds the status the create left it in, as the create's route answered it. An issue
// someone closed or moved since is kept, with nothing written, naming who closed or moved it after
// the create (issueFieldWriter), and the close expects that status too (goingCondition.Expect),
// decided under the issue's lock, so a move made after the undo's read is never closed over
// either: that refusal, 409 ISSUE_CHANGED, is the issue changed. A create its route answered made
// nothing - it named an external issue Dispatch already held - is undone by nothing: that issue is
// no issue of the run's.
func (u goingUndoer) closeCreated(line goingUndoLine, undone goingUndone) goingUndoPlan {
	if undone.unchanged {
		return goingNotTried(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: fmt.Sprintf(
				"this create made no issue: Dispatch already held %s for the external issue it named, so nothing was closed", undone.target)}
			return goingLineOf(line, undone.fields)
		})
	}
	var made struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(undone.after, &made); err != nil || made.Status == "" {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingFailed(errors.New("the create's record names no status the issue was left in, so no close can expect it"))
			return goingLineOf(line, undone.fields)
		})
	}
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
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
			closer := goingWriterText(u.issueFieldWriter(ctx, undone.target, "closed_at", undone.seq))
			line.Outcome = goingChanged([]string{fmt.Sprintf("the issue was closed at %s, by %s, so it is kept", *issue.ClosedAt, closer)}, nil)
			return goingLineOf(line, undone.fields), nil
		case issue.Status != made.Status:
			mover := u.issueFieldWriter(ctx, undone.target, "status", undone.seq)
			line.Outcome = goingChanged([]string{fmt.Sprintf("the issue is %s now, where %s moved it after this run left it %s, so it is kept",
				issue.Status, goingWriterText(mover), made.Status)}, nil)
			return goingLineKeptBy(line, undone.fields, mover), nil
		}
		expected, _ := json.Marshal(made.Status)
		closed := line
		closed.Outcome = goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "closed"}
		return line, &goingUndoSend{route: goingIssuePatchRoute, values: map[string]string{"key": undone.target}, body: map[string]any{"status": "done"},
			condition: &goingCondition{WriteBack: true, Expect: map[string]json.RawMessage{"status": expected}},
			landed:    goingLineOf(closed, undone.fields), changed: []string{"ISSUE_CHANGED"}}
	}}
}

// suggestion undoes a suggestion the run posted by rejecting it while it is still open: the
// suggestion's comment is the one its landing's recovery point names (commentRecovery), which its
// route committed with it. A suggestion someone accepted or rejected since is kept, naming who,
// and the reject's own refusal of one decided meanwhile, 409 ALREADY_ACTIONED, is the same. A
// record that names no comment settles the call: nothing is rejected.
func (u goingUndoer) suggestion(write goingLandedWrite, line goingUndoLine) goingUndoPlan {
	fields := goingChangedFields(write)
	var posted commentRecovery
	last := write.requests[len(write.requests)-1]
	if json.Unmarshal(last.recovery, &posted) != nil || posted.CommentID == "" {
		return goingStanding(func() goingUndoLine {
			line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
				Message: "the suggestion's record does not name the comment it posted, so nothing was rejected: read the document's suggestions, and reject it by hand"}
			return goingLineOf(line, fields)
		})
	}
	line.Item = posted.CommentID
	return goingUndoPlan{next: func(ctx context.Context) (goingUndoLine, *goingUndoSend) {
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
	}}
}
