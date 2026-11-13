package api

// going_undo.go undoes one turn of the command bar (R4): POST /api/v1/going/runs/{id}/undo walks
// the run's writes that went through and that no undo has reverted, newest first, and undoes each
// by the undo its catalogue row states (GoingUndo), in-process as the caller, as the write itself
// ran (going_writes.go):
//
//   - write_back writes the value read before the write over the value the write left: an
//     issue's fields, an ask's text, a document's content;
//   - reverse runs the write's reverse: a claim released, a release taken back (only where the
//     caller held the claim before), a follow unfollowed and an unfollow followed, a
//     resolved comment reopened, an opened ask or approval request retracted;
//   - close_created posts the undo's reason on an issue the turn created, then closes it, the
//     order a reasoned close takes;
//   - trace records the write undone and erases nothing.
//
// A write_back or a reverse whose item no longer holds what the write left is refused for that
// field, naming what is there now, and the rest of the turn is still undone. Each undo is
// claimed on the run before it is made (claimGoingRunUndos), recorded in the place of its claim
// (recordGoingRunUndo) and answered as one line.

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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingUndoReasonByDefault is the reason an undo gives where the person gave none: the message it
// posts on an issue it closes, an ask's retraction and a document's restoring version.
const goingUndoReasonByDefault = "Undone from the command bar."

// goingUndoClaimTries is how often one undo reads its run and claims its writes again when another
// request's claim or verdict lands between the two, before it answers the run busy.
const goingUndoClaimTries = 3

// The undo's own refusals of one write.
const (
	// codeGoingUndoChanged is a field another writer changed since the write: it is left as it
	// is now, and the rest is undone.
	codeGoingUndoChanged = "GOING_UNDO_CHANGED"
	// codeGoingUndoNotCallers is a release of a claim the undo cannot take back for its holder:
	// the claim it would make is recorded as its own claimant (goingUndoer.claimant), not as the
	// holder the release released, so taking it would give the claim to one who never held it.
	codeGoingUndoNotCallers = "GOING_UNDO_NOT_CALLERS_CLAIM"
	// codeGoingUndoUnknown is a write whose action the catalogue no longer holds: no row states
	// its undo, so none is tried.
	codeGoingUndoUnknown = "GOING_UNDO_UNKNOWN"
	// codeGoingUndoUnserved is a write whose row states an undo this server has no way to run
	// for its action: 501, which leaves the write undoable for a server that has one.
	codeGoingUndoUnserved = "GOING_UNDO_UNSERVED"
	// codeGoingUndoAfterUnread is a write that landed and whose value after was never read
	// (goingRunWrite.AfterUnread): what it left is unknown, so nothing is put back, and a retry
	// would only answer the same, which settles it.
	codeGoingUndoAfterUnread = "GOING_UNDO_AFTER_UNREAD"
	// codeGoingUndoInProgress is a run another undo of is under way: 429 with a Retry-After, so
	// the retry reads what that undo recorded.
	codeGoingUndoInProgress = "GOING_UNDO_IN_PROGRESS"
	// codeGoingUndoUnsettled is a write whose undo was claimed longer ago than any request making
	// it can still run (goingClaimBound) and never recorded: it may have been undone, so it is not
	// undone again, and the person reads the item to see, as codeGoingWriteUnsettled answers a
	// write.
	codeGoingUndoUnsettled = "GOING_UNDO_UNSETTLED"
	// codeClaimContended is the claim routes' refusal of a claim whose holder changed twice while
	// the request ran (issue_claim.go), which asks the caller to read the issue and try again.
	codeClaimContended = "CLAIM_CONTENDED"
	// codeGoingLandingUnread is an undo request whose landing could not be read back
	// (goingLandingUnread): the undo is answered the server's own failure and never recorded, so
	// its claim stays and it is never made twice.
	codeGoingLandingUnread = "GOING_LANDING_UNREAD"
)

// goingUndoInput is POST /api/v1/going/runs/{id}/undo's optional body: the reason an issue the
// turn created is closed with.
type goingUndoInput struct {
	Reason string `json:"reason"`
}

// goingUndoLine is how one write's undo went: the write, its item, the undo its row states, the
// value the item holds now where the undo read it, the outcome, and how each field of the write
// went, by the field's name. requests is how each request of an undo that sends more than one
// went (closeCreated), which the run records and a later undo of the write resumes after.
type goingUndoLine struct {
	WriteID  string            `json:"write_id"`
	Action   string            `json:"action"`
	Item     string            `json:"item"`
	Undo     goingUndoKind     `json:"undo" enum:"reverse,write_back,close_created,trace"`
	Now      json.RawMessage   `json:"now"`
	Outcome  goingWriteOutcome `json:"outcome"`
	Fields   []goingUndoField  `json:"fields"`
	requests []goingWriteRequest
}

// goingUndoAnswer is what an undo answers: one line per write it undid, newest first.
type goingUndoAnswer struct {
	Lines []goingUndoLine `json:"lines"`
}

// undoGoingRun is POST /api/v1/going/runs/{id}/undo. A run that is not the caller's, that the
// record does not hold, or that is older than thirty days - a run gone with its record - is 404
// GOING_RUN_NOT_FOUND. A run with nothing left to undo answers no line: a write already undone,
// or whose undo was refused with a verdict that would only repeat (goingUndoSettles), is not tried
// again. The undo claims every write it is about to undo on the run in one statement
// (claimGoingRunUndos) before it undoes any, holding no lock and no connection while it works, so
// a second undo of the run while the first is under way is 429 GOING_UNDO_IN_PROGRESS with a
// Retry-After, and its retry reads what the first recorded and undoes nothing twice. A write whose
// undo was claimed and never recorded, once no request can still be making it, is answered
// unsettled, 409 GOING_UNDO_UNSETTLED, and never undone again: that undo may have been made.
func (s *server) undoGoingRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input goingUndoInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	// Bounded as a keyed request's handler is, keyed or not, so a claim older than
	// goingClaimBound - this bound and the bookkeeping that records each undo after it - is one no
	// request is still making.
	ctx, cancel := context.WithTimeout(r.Context(), s.keyedRunLimit())
	defer cancel()
	claimedAt := time.Now().UTC().Truncate(time.Microsecond)
	var run goingRun
	var writes []goingRunWrite
	var unsettled map[string]time.Time
	for tries := 0; ; tries++ {
		if run, ok = s.readCallersGoingRun(w, r, caller.ID); !ok {
			return
		}
		// Each refused claim is another request's claim or verdict landing since the read; past
		// a few, the run is busy with undos, which a retry of this one waits out.
		if s.goingUndoUnderWay(run) || tries == goingUndoClaimTries {
			w.Header().Set("Retry-After", goingClaimedRetryAfter)
			writeError(w, codeGoingUndoInProgress, http.StatusTooManyRequests,
				"another undo of this run is under way: ask again shortly, and this undo reads what it did")
			return
		}
		writes, unsettled = goingUndoable(run)
		ids := goingUndoIDs(writes, unsettled)
		if len(ids) == 0 {
			break
		}
		landed := make(map[string][]goingWriteRequest, len(ids))
		for _, id := range ids {
			landed[id] = goingUndoLandedBefore(run, id)
		}
		err := s.claimGoingRunUndos(ctx, run.ID, caller.ID, ids, landed, claimedAt, len(run.Undos))
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			s.writeHandlerError(w, fmt.Errorf("claim the going undo: %w", err))
			return
		}
		// Refused: another undo claimed the run, settled one of these writes or recorded an undo
		// since the read, or the run is gone. The next read says which, and holds what any undo
		// of these writes landed (goingUndoLandedBefore) as the claim finds it.
	}
	at := goingUndoClaimedAt(claimedAt)
	// The writes the loop below has not reached yet, whose claims are given up when the handler
	// returns. An undo that was made and whose record failed keeps its claim, as a write whose
	// record failed does, so no undo of the run makes it a second time: past the bound it is
	// answered unsettled.
	unreached := goingUndoIDs(writes, unsettled)
	defer func() {
		if len(unreached) == 0 {
			return
		}
		bookkeeping, done := goingBookkeeping(ctx)
		defer done()
		if err := s.releaseGoingRunUndos(bookkeeping, run.ID, caller.ID, at, unreached); err != nil {
			slog.Error("dispatch: give up the claims of a going undo that recorded no line", "error", err)
		}
	}()
	undoer := goingUndoer{
		s: s, doer: s.goingDoer(r), login: caller.ID, session: goingCredentialOf(r, actor, false).session(),
		reason: cmp.Or(strings.TrimSpace(input.Reason), goingUndoReasonByDefault),
	}
	undoer.observer = goingObserver{read: goingReader{doer: undoer.doer}, s: s}
	lines := []goingUndoLine{}
	for _, write := range writes {
		if claimed, pending := unsettled[write.ID]; pending {
			lines = append(lines, goingUndoUnsettledLine(write, claimed))
			continue
		}
		unreached = unreached[1:]
		undoer.landings = &goingLandings{s: s, run: run.ID, login: caller.ID, write: write.ID, undo: at}
		line := undoer.undo(ctx, write, goingUndoLandedBefore(run, write.ID))
		if line.Outcome.Code == codeGoingLandingUnread {
			// Whether a request of the undo landed is unknown: its claim stays, so no undo of the
			// run makes it a second time, and past the bound it is answered unsettled.
			s.writeHandlerError(w, errors.New("read whether an undo's request landed"))
			return
		}
		bookkeeping, done := goingBookkeeping(ctx)
		err := s.recordGoingRunUndo(bookkeeping, run.ID, caller.ID, goingRunUndo{
			WriteID: write.ID, Outcome: line.Outcome, Fields: line.Fields, At: time.Now().UTC(),
			Settled: goingUndoSettles(line.Outcome), Requests: line.requests,
		}, at)
		done()
		if err != nil {
			s.writeGoingRunAppendError(w, err)
			return
		}
		lines = append(lines, line)
	}
	WriteJSON(w, http.StatusOK, goingUndoAnswer{Lines: lines})
}

// goingUndoUnderWay reports whether another undo of the run is under way: an undo it claimed
// within goingClaimBound is still pending.
func (s *server) goingUndoUnderWay(run goingRun) bool {
	return slices.ContainsFunc(run.Undos, func(undo goingRunUndo) bool {
		return undo.Pending && time.Since(undo.At) < s.goingClaimBound()
	})
}

// goingUndoLandedBefore is the requests the latest recorded undo of the write landed before the
// first that did not, whatever key that undo was sent under: an undo of the write left undoable
// after part of it landed resumes after them (closeCreated), sending none of them again.
func goingUndoLandedBefore(run goingRun, writeID string) []goingWriteRequest {
	for _, undo := range slices.Backward(run.Undos) {
		if undo.WriteID == writeID && !undo.Pending {
			return goingLanded(undo.Requests)
		}
	}
	return nil
}

// goingUndoable is every write of the run an undo answers for, newest first: each that went
// through and that no undo has undone or settled. unsettled holds, by write, when the undo of
// each among them that an undo claimed and never recorded was claimed: such a write is answered
// unsettled rather than undone, since that undo may have been made.
func goingUndoable(run goingRun) (writes []goingRunWrite, unsettled map[string]time.Time) {
	settled := map[string]bool{}
	unsettled = map[string]time.Time{}
	for _, undo := range run.Undos {
		switch {
		case undo.Outcome.OK || undo.Settled:
			settled[undo.WriteID] = true
		case undo.Pending:
			unsettled[undo.WriteID] = undo.At
		}
	}
	for _, write := range slices.Backward(run.Writes) {
		if write.Outcome.OK && !settled[write.ID] {
			writes = append(writes, write)
		}
	}
	return writes, unsettled
}

// goingUndoIDs is the id of each of writes an undo makes, in order: every one but those unsettled.
func goingUndoIDs(writes []goingRunWrite, unsettled map[string]time.Time) []string {
	ids := make([]string, 0, len(writes))
	for _, write := range writes {
		if _, pending := unsettled[write.ID]; !pending {
			ids = append(ids, write.ID)
		}
	}
	return ids
}

// goingUndoUnsettledLine is the line of a write whose undo was claimed at claimed and never
// recorded: 409 GOING_UNDO_UNSETTLED for it and for each field it changed, with nothing undone.
func goingUndoUnsettledLine(write goingRunWrite, claimed time.Time) goingUndoLine {
	outcome := goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoUnsettled, Message: fmt.Sprintf(
		"this write's undo was made at %s and its outcome was never recorded, so it may have been undone: it is not undone again; read the item to see what it holds",
		claimed.Format(time.RFC3339))}
	names := write.Fields
	if len(names) == 0 {
		names = []string{""}
	}
	fields := make([]goingUndoField, 0, len(names))
	for _, field := range names {
		fields = append(fields, goingUndoField{Field: field, Message: outcome.Message})
	}
	return goingUndoLine{WriteID: write.ID, Action: write.Action, Item: write.Target, Undo: write.Undo,
		Now: json.RawMessage("null"), Outcome: outcome, Fields: fields}
}

// goingUndoSettles reports whether an undo's outcome settles its write: ok, or a refusal a retry
// would only repeat. The server's own failure (5xx, or no status at all) settles nothing, and
// neither does a refusal that says only that this attempt was not heard - the statuses the keyed
// layer keeps no answer for (storedStatus: 401, 408, 429) - or a claim contended while the undo
// ran (CLAIM_CONTENDED), which asks the caller to try again.
func goingUndoSettles(outcome goingWriteOutcome) bool {
	switch {
	case outcome.OK:
		return true
	case outcome.Status <= 0 || !storedStatus(outcome.Status):
		return false
	}
	return outcome.Code != codeClaimContended
}

// goingUndoer undoes the writes of one run as its caller.
type goingUndoer struct {
	s        *server
	doer     GoingDoer
	observer goingObserver
	login    string
	session  string
	reason   string
	// landings is where the requests of the write's undo under way record that they landed.
	landings *goingLandings
}

// undo undoes one write by the undo the catalogue row it went through states, as its record keeps
// it (goingRunWrite.Undo), after the requests an earlier undo of the write landed (landed). That
// kind chooses the path: a trace is recorded undone and erases
// nothing, and each other kind runs the undo of its own action. A write of an action the catalogue
// no longer holds, or whose record names no undo, is refused, 409 GOING_UNDO_UNKNOWN, and a kind
// that names an undo this server has none of for its action, 501 GOING_UNDO_UNSERVED, rather than
// recorded undone with nothing undone. A write whose value after was never read
// (goingRunWrite.AfterUnread) is refused, 409 GOING_UNDO_AFTER_UNREAD, for every kind but a
// trace: what it left is unknown, so there is nothing to compare what is there now with.
func (u goingUndoer) undo(ctx context.Context, write goingRunWrite, landed []goingWriteRequest) goingUndoLine {
	line := goingUndoLine{WriteID: write.ID, Action: write.Action, Item: write.Target, Now: json.RawMessage("null")}
	if _, held := u.s.going.Lookup(write.Action); !held || write.Undo == "" {
		line.Undo = goingUndoTrace
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoUnknown,
			Message: fmt.Sprintf("%s is no action of the catalogue now, or its write's record names no undo, so its undo is unknown", write.Action)}
		return line
	}
	line.Undo = write.Undo
	var now any
	var outcome goingWriteOutcome
	var fields []goingUndoField
	served := true
	switch kind := line.Undo; {
	case write.AfterUnread && kind != goingUndoTrace:
		outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoAfterUnread,
			Message: "the value this write left was never read, so what it changed is unknown and nothing was undone: read the item to see what it holds"}
	case kind == goingUndoTrace:
		outcome = goingWriteOutcome{OK: true, Status: http.StatusOK,
			Message: "recorded as undone: Dispatch serves no reverse of this write, so it stays as it is"}
	case kind == goingUndoWriteBack:
		switch write.Action {
		case "dispatch_issue_update":
			now, outcome, fields = u.issueFields(ctx, write)
		case "dispatch_edit_ask":
			now, outcome, fields = u.askFields(ctx, write)
		case "dispatch_doc_edit":
			now, outcome = u.document(ctx, write)
		default:
			served = false
		}
	case kind == goingUndoReverse:
		switch write.Action {
		case "dispatch_claim":
			now, outcome = u.claim(ctx, write)
		case "dispatch_follow":
			now, outcome = u.follow(ctx, write)
		case "dispatch_resolve_comment":
			now, outcome = u.comment(ctx, write)
		case "dispatch_ask", "dispatch_request_approval":
			now, outcome = u.retract(ctx, write)
		default:
			served = false
		}
	case kind == goingUndoCloseCreated:
		if write.Action == "dispatch_issue" {
			now, outcome, line.requests = u.closeCreated(ctx, write, landed)
		} else {
			served = false
		}
	default:
		served = false
	}
	if !served {
		outcome = goingWriteOutcome{Status: http.StatusNotImplemented, Code: codeGoingUndoUnserved,
			Message: fmt.Sprintf("the catalogue states a %s undo for %s, which this server has no way to run, so nothing was undone", line.Undo, write.Action)}
	}
	if fields == nil {
		// One outcome, the write's, for each field it changed.
		names := write.Fields
		if len(names) == 0 {
			names = []string{""}
		}
		fields = make([]goingUndoField, 0, len(names))
		for _, field := range names {
			fields = append(fields, goingUndoField{Field: field, OK: outcome.OK, Message: outcome.Message})
		}
	}
	line.Now, line.Outcome, line.Fields = goingJSONOrNull(now), outcome, fields
	return line
}

// send makes request index of an undo through route, one of the routes the catalogue declares
// for the undos (goingIssuePatchRoute and the rest), its placeholders filled from values,
// in-process as the caller, and answers how it went. The request landed where it was answered
// 2xx, or where its route's transaction committed, as the record it wrote there says
// (goingLandings.landed), whatever its answer's status: a write back that committed and then
// answered 5xx is undone and never made a second time. One whose landing could not be read back
// is codeGoingLandingUnread.
func (u goingUndoer) send(ctx context.Context, index int, route string, values map[string]string, body any) goingWriteOutcome {
	method, pattern, _ := strings.Cut(route, " ")
	path := goingPath(pattern, values)
	spec := goingSend{method: method, path: path}
	if body != nil {
		encoded, err := goingJSONRequest(method, path, body)
		if err != nil {
			return goingFailed(err)
		}
		spec = encoded
	}
	request, err := spec.build(u.landings.on(ctx, index, spec), "")
	if err != nil {
		return goingFailed(err)
	}
	response, err := u.doer.Do(request)
	if err != nil {
		return goingFailed(err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	landed := response.StatusCode < http.StatusMultipleChoices
	if !landed && u.landings != nil {
		if landed, err = u.landings.landed(ctx, index, spec); err != nil {
			slog.Error("dispatch: read whether a going undo's request landed", "error", err)
			return goingWriteOutcome{Status: http.StatusInternalServerError, Code: codeGoingLandingUnread, Message: "internal server error"}
		}
	}
	if !landed {
		return goingRefusalOf(response, answer).outcome()
	}
	return goingWriteOutcome{OK: true, Status: response.StatusCode}
}

// goingFailed is the outcome of an undo that could not be made at all, or of a write's or an
// undo's read that failed: a route's refusal as that route answered it, and anything else as the
// server's own failure, its detail logged and its message the fixed one writeHandlerError
// answers, since the outcome is both answered and stored on the run.
func goingFailed(err error) goingWriteOutcome {
	var refusal goingRefusal
	if errors.As(err, &refusal) {
		return refusal.outcome()
	}
	slog.Error("dispatch: a going write or undo failed", "error", err)
	return goingWriteOutcome{Status: http.StatusInternalServerError, Code: codeInternal, Message: "internal server error"}
}

// goingChanged is the outcome of an undo refused, whole or for some fields, because another writer
// changed what the write left: each change named, with what was undone beside it.
func goingChanged(changes []string, restored []string) goingWriteOutcome {
	message := strings.Join(changes, "; ")
	if len(restored) > 0 {
		message += "; the rest was undone: " + strings.Join(restored, ", ")
	}
	return goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoChanged, Message: message}
}

// goingRank is the field an issue's rank is recorded under. Its value is the issue's place
// (goingRankPlace): its rank and the neighbours it stood between, since a rank is written by
// naming neighbours.
const goingRank = "rank"

// issueFields writes back each field of the issue the write changed that still holds what the
// write left, and names each one another writer changed since. The rank is put back between the
// neighbours the issue stood between before the write (rankRestore), while the issue's rank is
// still the one the write left; a neighbour that is no longer an issue of the project is passed
// over, and with neither left the rank is refused, naming why. A failed read of the neighbours
// fails the undo of the write, which leaves it undoable. The update expects each field it writes
// back to hold what the write left (updateIssueInput.Expect), its rank the rank string, so a
// change another writer makes after the read is refused by the update itself.
func (u goingUndoer) issueFields(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome, []goingUndoField) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.issueFields(ctx, write.Target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(field string, held, before, after json.RawMessage) (any, string, error) {
		if field != goingRank {
			if !goingSameJSON(held, after) {
				return nil, goingChangedField(field, held), nil
			}
			return goingWriteBackValue(field, before), "", nil
		}
		var heldPlace, beforePlace, afterPlace goingRankPlace
		if json.Unmarshal(held, &heldPlace) != nil || json.Unmarshal(before, &beforePlace) != nil || json.Unmarshal(after, &afterPlace) != nil {
			return nil, "rank: the write's record holds no place to put the issue back in", nil
		}
		// Its neighbours move as other issues are ranked; only its own rank says whether another
		// writer moved this issue.
		if heldPlace.Rank != afterPlace.Rank {
			return nil, fmt.Sprintf("rank: the issue was moved again since this write, between %s and %s",
				goingNeighbourText(heldPlace.After), goingNeighbourText(heldPlace.Before)), nil
		}
		return u.observer.rankRestore(ctx, write.Target, beforePlace)
	}
	expectOf := func(field string, after json.RawMessage) any {
		var place goingRankPlace
		if field == goingRank && json.Unmarshal(after, &place) == nil {
			return place.Rank
		}
		return goingJSONOrNullRaw(after)
	}
	return u.writeBack(ctx, write, read, goingIssuePatchRoute, map[string]string{"key": write.Target}, restoreOf, expectOf, "ISSUE_CHANGED")
}

// rankRestore is how a write back puts an issue at the place it held: between the two neighbours
// it had, those of them that are still issues of its project, or both where they still stand in
// order, and why it cannot where neither is. A read of the neighbours that failed is its error,
// never a reason: the undo answers it as the server's own failure, which leaves the write
// undoable.
func (o goingObserver) rankRestore(ctx context.Context, key string, place goingRankPlace) (map[string]any, string, error) {
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
		return nil, "rank: neither issue it stood between is an issue of its project now, so its place cannot be named back", nil
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

// goingChangedField is the refusal of a field another writer changed since the write.
func goingChangedField(field string, held json.RawMessage) string {
	return fmt.Sprintf("%s holds %s now, which another write left", field, held)
}

// goingWriteBackValue is a field's value before a write as an update writes it back: a route the
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

// askFields writes back each part of the ask the edit changed that still holds what the edit
// left, and names each one another writer changed since; the edit expects each part it writes
// back to hold what the edit left (editAskInput.Expect).
func (u goingUndoer) askFields(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome, []goingUndoField) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.askFields(ctx, write.Target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(field string, held, before, after json.RawMessage) (any, string, error) {
		if !goingSameJSON(held, after) {
			return nil, goingChangedField(field, held), nil
		}
		return goingJSONOrNullRaw(before), "", nil
	}
	expectOf := func(_ string, after json.RawMessage) any { return goingJSONOrNullRaw(after) }
	return u.writeBack(ctx, write, read, goingAskEditRoute, map[string]string{"id": write.Target}, restoreOf, expectOf, "ASK_CHANGED")
}

// writeBack is a write back of the fields one write changed, by one PATCH through route, its
// placeholders filled from values: restoreOf says, from what each field holds now, held before
// the write and was left by it, the value the PATCH writes back, or why the field is left as it
// is - another writer changed it since - or fails, which fails the whole write back before
// anything is sent. The PATCH expects each field it writes back to hold what the write left
// (expectOf, the route's expect), decided under the lock the route takes, so a field another
// writer changes between the read and the PATCH is never written over: the route's refusal of
// it (changedCode) is the field changed, and nothing is written back. Every field's own outcome
// is answered beside the write's.
func (u goingUndoer) writeBack(
	ctx context.Context, write goingRunWrite, read func([]string) (map[string]any, error),
	route string, values map[string]string,
	restoreOf func(field string, held, before, after json.RawMessage) (any, string, error),
	expectOf func(field string, after json.RawMessage) any, changedCode string,
) (any, goingWriteOutcome, []goingUndoField) {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no field values to write back")), nil
	}
	fields := slices.Sorted(maps.Keys(after))
	now, err := read(fields)
	if err != nil {
		return nil, goingFailed(err), nil
	}
	restore, expect := map[string]any{}, map[string]any{}
	var changes, restored []string
	outcomes := make([]goingUndoField, 0, len(fields))
	for _, field := range fields {
		value, why, err := restoreOf(field, goingJSONOrNull(now[field]), goingJSONOrNullRaw(before[field]), after[field])
		if err != nil {
			return now, goingFailed(err), nil
		}
		if why != "" {
			changes = append(changes, why)
			outcomes = append(outcomes, goingUndoField{Field: field, Message: why})
			continue
		}
		restore[field], expect[field] = value, expectOf(field, after[field])
		restored = append(restored, field)
		outcomes = append(outcomes, goingUndoField{Field: field, OK: true})
	}
	if len(restore) > 0 {
		restore["expect"] = expect
		if outcome := u.send(ctx, 0, route, values, restore); !outcome.OK {
			if outcome.Code == changedCode {
				// Another writer changed a field after the read: the route wrote nothing back.
				outcome = goingChanged([]string{outcome.Message}, nil)
			}
			// Nothing was written back: every field is left with the route's refusal.
			for index := range outcomes {
				if outcomes[index].OK {
					outcomes[index] = goingUndoField{Field: outcomes[index].Field, Message: outcome.Message}
				}
			}
			if reread, err := read(fields); err == nil {
				now = reread
			}
			return now, outcome, outcomes
		}
		if reread, err := read(fields); err == nil {
			now = reread
		}
	}
	if len(changes) > 0 {
		return now, goingChanged(changes, restored), outcomes
	}
	return now, goingWriteOutcome{OK: true, Status: http.StatusOK}, outcomes
}

// document writes the content the document held before the edit back over it, as a further
// version uploaded under the document's own name to its owner's route, where its content is still
// what the edit left: the upload expects it (jsonArtifactUpload.ExpectSHA256), decided under the
// document's own lock, so an edit another writer makes after the read is never written over.
func (u goingUndoer) document(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after goingDocumentValue
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no content to write back"))
	}
	var artifact goingDocumentRead
	if err := u.observer.read.get(ctx, goingArtifactPath(write.Target), &artifact); err != nil {
		return nil, goingFailed(err)
	}
	now, err := u.observer.documentValue(ctx, artifact.ID)
	if err != nil {
		return nil, goingFailed(err)
	}
	changed := func(now goingDocumentValue) goingWriteOutcome {
		return goingChanged([]string{fmt.Sprintf("content is at version %s now, written after this edit", goingVersionText(now.Version))}, nil)
	}
	if now.Content != after.Content {
		return now, changed(now)
	}
	route, values := goingProjectUploadRoute, map[string]string{"key": artifact.Project}
	if artifact.IssueKey != nil {
		route, values = goingIssueUploadRoute, map[string]string{"key": *artifact.IssueKey}
	}
	outcome := u.send(ctx, 0, route, values, map[string]any{
		"name": artifact.Name, "content": before.Content, "summary": u.reason,
		"expect_sha256": docs.MarkdownSHA256(after.Content),
	})
	if read, err := u.observer.documentValue(ctx, artifact.ID); err == nil {
		now = read
	}
	if outcome.Code == "DOCUMENT_CHANGED" {
		return now, changed(now)
	}
	return now, outcome
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

// claim undoes a claim by releasing it and a release by taking the claim back, each decided by
// the claim route in its own transaction, never from a read another writer may have overtaken. A
// claim is released only while the holder it left - as its route answered it, in the claim's own
// transaction - still holds it (releaseClaimInput.ExpectHolder); a release is taken back only for
// the holder it released - as its route answered it, previous_claim - and only where the claim
// the undo makes would be recorded as that very holder (claimsAs) and nobody holds the issue
// (claimIssueInput.ExpectUnclaimed). A release of another's claim - a session's the person
// released, another person's, or the person's own undone through a credential bound to a
// session - is refused, 409 GOING_UNDO_NOT_CALLERS_CLAIM, settled, with nothing written: the bar
// cannot claim for that holder, and claiming as the caller would give the caller a claim it never
// held. Either route refusing its condition, 409 CLAIM_HOLDER_CHANGED, is the claim changed by
// another writer since, and nothing is undone. A claim or a release its route answered changed
// nothing (goingRunWrite.Unchanged) - a claim of an issue its caller already held, a release of
// one nobody held - is undone by nothing.
func (u goingUndoer) claim(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after *goingHolder
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no claim to undo"))
	}
	values := map[string]string{"key": write.Target}
	// The claim as it stands once the undo is decided, for the line to show.
	now := func() any {
		current, _, err := u.observer.claimHolder(ctx, write.Target)
		if err != nil || current == nil {
			return nil
		}
		return current
	}
	changed := func(outcome goingWriteOutcome) goingWriteOutcome {
		if outcome.Code == codeClaimHolderChanged {
			return goingChanged([]string{outcome.Message}, nil)
		}
		return outcome
	}
	switch {
	case write.Unchanged:
		return now(), goingWriteOutcome{OK: true, Status: http.StatusOK,
			Message: "the claim route answered that this write changed nothing - the claim already stood as it asked - so there is nothing to undo"}
	case after != nil:
		outcome := u.send(ctx, 0, goingReleaseRoute, values, map[string]any{"expect_holder": after})
		return now(), changed(outcome)
	case before == nil:
		return now(), goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "nobody held the claim before this release, so there is nothing to take back"}
	case !u.claimsAs(before):
		claimant := u.claimant()
		return now(), goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoNotCallers, Message: fmt.Sprintf(
			"this release released the claim of %s, which the bar cannot claim for them: its claim would be recorded as %s, which never held it, so nothing was taken back",
			goingHolderText(before), goingHolderText(&claimant))}
	}
	outcome := u.send(ctx, 0, goingClaimRoute, values, map[string]any{"expect_unclaimed": true})
	return now(), changed(outcome)
}

// follow undoes a follow by unfollowing and an unfollow by following again, while the session
// still follows as the write left it.
func (u goingUndoer) follow(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after goingFollowing
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no follow to undo"))
	}
	following, err := u.observer.follows(ctx, write.Target, after.Session)
	if err != nil {
		return nil, goingFailed(err)
	}
	now := goingFollowing{Session: after.Session, Following: following}
	switch {
	case following != after.Following:
		return now, goingChanged([]string{fmt.Sprintf("following is %t now, which another write left", following)}, nil)
	case before.Following == after.Following:
		return now, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the write changed nothing, so there is nothing to undo"}
	}
	route := goingUnfollowRoute
	if before.Following {
		route = goingFollowRoute
	}
	outcome := u.send(ctx, 0, route, map[string]string{"id": write.Target, "session_id": after.Session}, nil)
	if outcome.OK {
		now.Following = before.Following
	}
	return now, outcome
}

// goingCommentResolution is a resolve's value: whether the thread is resolved, and the resolution
// as the comment's read answers it, who resolved it and when, null for an open thread.
type goingCommentResolution struct {
	Resolved   bool            `json:"resolved"`
	ResolvedBy json.RawMessage `json:"resolved_by"`
	ResolvedAt *string         `json:"resolved_at"`
}

// comment undoes a resolve by reopening the thread, while it still holds the resolution the turn
// left: the reopen expects that resolution's moment (reopenCommentInput.ExpectResolvedAt), decided
// under the comment's lock, so a thread another writer reopened and resolved again after the
// undo's read keeps their resolution, the route's 409 COMMENT_CHANGED being the thread changed.
func (u goingUndoer) comment(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after goingCommentResolution
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no resolve to undo"))
	}
	current, err := u.observer.commentResolved(ctx, write.Target)
	if err != nil {
		return nil, goingFailed(err)
	}
	now, _ := current.value.(goingCommentResolution)
	switch {
	case before.Resolved || !after.Resolved:
		return now, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was resolved before this write, so there is nothing to reopen"}
	case !now.Resolved:
		return now, goingChanged([]string{"the thread is open now, reopened by another write"}, nil)
	case after.ResolvedAt == nil || now.ResolvedAt == nil || *now.ResolvedAt != *after.ResolvedAt:
		return now, goingChanged([]string{"the thread was resolved again since this write, by another write"}, nil)
	}
	outcome := u.send(ctx, 0, goingCommentReopenRoute, map[string]string{"id": write.Target}, map[string]any{"expect_resolved_at": *after.ResolvedAt})
	if read, err := u.observer.commentResolved(ctx, write.Target); err == nil {
		now, _ = read.value.(goingCommentResolution)
	}
	if outcome.Code == codeCommentChanged {
		return now, goingChanged([]string{outcome.Message}, nil)
	}
	return now, outcome
}

// retract undoes an ask the turn opened, an approval request's among them, by retracting it while
// it is still open. An approval request its route answered opened no ask (opened false in the
// record's value after) - it renewed one already open, or the documents stood approved - is
// undone by nothing. A record whose value after does not decode, or does not say whether an
// approval request opened its ask, is the server's own failure, as claim answers one, and
// retracts nothing: it leaves the write undoable.
func (u goingUndoer) retract(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var after struct {
		ID     string `json:"id"`
		Ask    any    `json:"ask"`
		Opened *bool  `json:"opened"`
	}
	if err := json.Unmarshal(write.After, &after); err != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no ask to retract: %w", err))
	}
	ask := after.ID
	if write.Action == "dispatch_request_approval" {
		ask, _ = after.Ask.(string)
		if after.Opened == nil {
			return nil, goingFailed(errors.New("the write's record does not say whether the approval request opened its ask"))
		}
		if !*after.Opened {
			return nil, goingWriteOutcome{OK: true, Status: http.StatusOK,
				Message: "the approval route answered that this write opened no ask - it renewed one already open - so there is none to retract"}
		}
	}
	if ask == "" {
		return nil, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "this write opened no ask, so there is none to retract"}
	}
	current, err := u.observer.askState(ctx, ask)
	if err != nil {
		return nil, goingFailed(err)
	}
	if current.value != "open" {
		return current.value, goingChanged([]string{fmt.Sprintf("the ask is %v now, so it is no longer open to retract", current.value)}, nil)
	}
	outcome := u.send(ctx, 0, goingAskResolveRoute, map[string]string{"id": ask}, map[string]any{"kind": "retracted", "reason": u.reason})
	if outcome.OK {
		return "resolved", outcome
	}
	return current.value, outcome
}

// closeCreated closes an issue the turn created: the undo's reason posted on it as a message,
// then its status set done, while it still holds the status the turn left it in, answering how
// each request went. That status is the one the write recorded in its value after (the created
// issue's key and status, observe); an issue someone moved since is refused, 409
// GOING_UNDO_CHANGED, before anything is posted, and the close expects that status too
// (updateIssueInput.Expect), decided under the issue's lock, so a move made after the undo's read
// is never closed over either: that refusal, 409 ISSUE_CHANGED, is the issue changed, and settles.
// Where an earlier undo of the write posted the reason (landed) and its close was refused, the
// reason is not posted again: the undo resumes at the close. A close refused after the reason
// landed is answered naming the reason that stands (goingRefusal.after), as a write's refused line
// names what of it landed. It is the one undo that sends more than one request; every other kind
// sends one, so a retry of it has nothing landed to skip.
func (u goingUndoer) closeCreated(ctx context.Context, write goingRunWrite, landed []goingWriteRequest) (any, goingWriteOutcome, []goingWriteRequest) {
	var issue struct {
		Status   string  `json:"status"`
		ClosedAt *string `json:"closed_at"`
	}
	if err := u.observer.read.get(ctx, goingIssuePath(write.Target), &issue); err != nil {
		return nil, goingFailed(err), nil
	}
	if issue.ClosedAt != nil {
		return issue.Status, goingChanged([]string{"the issue is closed already, at " + *issue.ClosedAt}, nil), nil
	}
	var made struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(write.After, &made)
	left := made.Status
	if left == "" {
		// A record whose value after names no status - one written before the write recorded
		// it - is held to the status the undo reads, as before.
		left = issue.Status
	}
	if issue.Status != left {
		return issue.Status, goingChanged([]string{fmt.Sprintf("the issue is %s now, moved since this write left it %s", issue.Status, left)}, nil), nil
	}
	values := map[string]string{"key": write.Target}
	reason := goingUndoRequest(goingIssueMessageRoute, values)
	if len(landed) == 0 || landed[0].Method != reason.Method || landed[0].Path != reason.Path {
		reason.Outcome = u.send(ctx, 0, goingIssueMessageRoute, values, map[string]any{"body": u.reason})
	} else {
		reason = landed[0]
	}
	requests := []goingWriteRequest{reason}
	if !reason.Outcome.OK {
		return issue.Status, reason.Outcome, requests
	}
	closing := goingUndoRequest(goingIssuePatchRoute, values)
	closing.Outcome = u.send(ctx, 1, goingIssuePatchRoute, values, map[string]any{
		"status": "done", "expect": map[string]any{"status": left},
	})
	requests = append(requests, closing)
	if closing.Outcome.OK {
		return "done", closing.Outcome, requests
	}
	if closing.Outcome.Code == codeGoingLandingUnread {
		return issue.Status, closing.Outcome, requests
	}
	refused := goingRefusal{Status: closing.Outcome.Status, Code: closing.Outcome.Code, Message: closing.Outcome.Message}.after(requests[:1])
	if closing.Outcome.Code == "ISSUE_CHANGED" {
		return issue.Status, goingChanged([]string{refused.Message}, nil), requests
	}
	return issue.Status, refused.outcome(), requests
}

// goingUndoRequest is a request an undo sends through route, its placeholders filled from values,
// as the run records it, its outcome not yet set.
func goingUndoRequest(route string, values map[string]string) goingWriteRequest {
	method, pattern, _ := strings.Cut(route, " ")
	return goingWriteRequest{Method: method, Path: goingPath(pattern, values)}
}
