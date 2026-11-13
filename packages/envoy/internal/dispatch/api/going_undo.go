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
// appended to the run (appendGoingRunUndo) and answered as one line.

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
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingUndoReasonByDefault is the reason an undo gives where the person gave none: the message it
// posts on an issue it closes, an ask's retraction and a document's restoring version.
const goingUndoReasonByDefault = "Undone from the command bar."

// The undo's own refusals of one write.
const (
	// codeGoingUndoChanged is a field another writer changed since the write: it is left as it
	// is now, and the rest is undone.
	codeGoingUndoChanged = "GOING_UNDO_CHANGED"
	// codeGoingUndoNotCallers is a release of a claim the caller did not hold: taking it back
	// would make the caller its holder, which is not what it was.
	codeGoingUndoNotCallers = "GOING_UNDO_NOT_CALLERS_CLAIM"
	// codeGoingUndoUnknown is a write whose action the catalogue no longer holds: no row states
	// its undo, so none is tried.
	codeGoingUndoUnknown = "GOING_UNDO_UNKNOWN"
)

// goingUndoInput is POST /api/v1/going/runs/{id}/undo's optional body: the reason an issue the
// turn created is closed with.
type goingUndoInput struct {
	Reason string `json:"reason"`
}

// goingUndoLine is how one write's undo went: the write, its item and field, the undo its row
// states, the value the item holds now where the undo read it, the outcome, and how each field
// of the write went.
type goingUndoLine struct {
	WriteID string            `json:"write_id"`
	Action  string            `json:"action"`
	Item    string            `json:"item"`
	Field   string            `json:"field"`
	Undo    goingUndoKind     `json:"undo" enum:"reverse,write_back,close_created,trace"`
	Now     json.RawMessage   `json:"now"`
	Outcome goingWriteOutcome `json:"outcome"`
	Fields  []goingUndoField  `json:"fields"`
}

// goingUndoAnswer is what an undo answers: one line per write it undid, newest first.
type goingUndoAnswer struct {
	Lines []goingUndoLine `json:"lines"`
}

// undoGoingRun is POST /api/v1/going/runs/{id}/undo. A run that is not the caller's, that the
// record does not hold, or that is older than thirty days - a run gone with its record - is 404
// GOING_RUN_NOT_FOUND. A run with nothing left to undo answers no line: a write already undone,
// or whose undo was refused for what another writer changed, is not tried again. Two undos of one
// run take turns (lockGoing, keyed by the run), so the second reads what the first recorded and
// undoes nothing twice. The run is read once before the lock, so only its owner ever holds the
// lock and a connection for it, and once more under it, which is the read the undo goes by.
func (s *server) undoGoingRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input goingUndoInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	owned, ok := s.readCallersGoingRun(w, r, caller.ID)
	if !ok {
		return
	}
	release, err := s.lockGoing(r.Context(), "going-undo:"+owned.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer release()
	run, ok := s.readCallersGoingRun(w, r, caller.ID)
	if !ok {
		return
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	undoer := goingUndoer{
		s: s, doer: s.goingDoer(r), login: caller.ID, session: goingCredentialOf(actor, false).session(),
		reason: cmp.Or(strings.TrimSpace(input.Reason), goingUndoReasonByDefault),
	}
	undoer.observer = goingObserver{read: goingReader{doer: undoer.doer}, s: s}
	settled := map[string]bool{}
	for _, undo := range run.Undos {
		if undo.Outcome.OK || undo.Settled {
			settled[undo.WriteID] = true
		}
	}
	lines := []goingUndoLine{}
	for _, write := range slices.Backward(run.Writes) {
		if !write.Outcome.OK || settled[write.ID] {
			continue
		}
		line := undoer.undo(r.Context(), write)
		if err := s.appendGoingRunUndo(r.Context(), run.ID, caller.ID, goingRunUndo{
			WriteID: write.ID, Outcome: line.Outcome, Fields: line.Fields, At: time.Now().UTC(),
			// A verdict below the server's own failures would only repeat: the write is settled.
			Settled: line.Outcome.OK || (line.Outcome.Status > 0 && line.Outcome.Status < http.StatusInternalServerError),
		}); err != nil {
			s.writeGoingRunAppendError(w, err)
			return
		}
		lines = append(lines, line)
	}
	WriteJSON(w, http.StatusOK, goingUndoAnswer{Lines: lines})
}

// lockGoing holds the lock named key until release is called: a transaction-scoped advisory
// lock, on a connection of its own mark, so the in-process writes and the appends to the run made
// under it - each on connections of their own - are not held by it. An undo holds one keyed by
// its run, so a second undo of the same run waits for the first to finish before it reads the
// run; a write holds one keyed by its run, offer and invocation (createGoingWrite).
func (s *server) lockGoing(ctx context.Context, key string) (func(), error) {
	locking := store.ForConcurrentWorker(ctx)
	tx, err := s.begin(locking)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(locking, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		_ = tx.Rollback(context.WithoutCancel(locking))
		return nil, fmt.Errorf("take the lock %s: %w", key, err)
	}
	return func() { _ = tx.Rollback(context.WithoutCancel(locking)) }, nil
}

// goingUndoer undoes the writes of one run as its caller.
type goingUndoer struct {
	s        *server
	doer     GoingDoer
	observer goingObserver
	login    string
	session  string
	reason   string
}

// undo undoes one write by the undo its catalogue row states, and answers how it went.
func (u goingUndoer) undo(ctx context.Context, write goingRunWrite) goingUndoLine {
	line := goingUndoLine{WriteID: write.ID, Action: write.Action, Item: write.Target, Field: write.Field, Now: json.RawMessage("null")}
	action, held := u.s.going.Lookup(write.Action)
	if !held || len(action.Routes) == 0 {
		line.Undo = goingUndoTrace
		line.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoUnknown,
			Message: fmt.Sprintf("%s is no action of the catalogue now, so its undo is unknown", write.Action)}
		return line
	}
	line.Undo = action.Routes[0].Undo.Kind
	var now any
	var outcome goingWriteOutcome
	var fields []goingUndoField
	switch write.Action {
	case "dispatch_issue_update":
		now, outcome, fields = u.issueFields(ctx, write)
	case "dispatch_edit_ask":
		now, outcome, fields = u.askFields(ctx, write)
	case "dispatch_doc_edit":
		now, outcome = u.document(ctx, write)
	case "dispatch_claim":
		now, outcome = u.claim(ctx, write)
	case "dispatch_follow":
		now, outcome = u.follow(ctx, write)
	case "dispatch_resolve_comment":
		now, outcome = u.comment(ctx, write)
	case "dispatch_ask", "dispatch_request_approval":
		now, outcome = u.retract(ctx, write)
	case "dispatch_issue":
		now, outcome = u.closeCreated(ctx, write)
	default:
		outcome = goingWriteOutcome{OK: true, Status: http.StatusOK,
			Message: "recorded as undone: Dispatch serves no reverse of this write, so it stays as it is"}
	}
	if fields == nil {
		fields = []goingUndoField{{Field: write.Field, OK: outcome.OK, Message: outcome.Message}}
	}
	line.Now, line.Outcome, line.Fields = goingJSONOrNull(now), outcome, fields
	return line
}

// send makes one write of an undo through route, one of the routes the catalogue declares for the
// undos (goingIssuePatchRoute and the rest), its placeholders filled from values, in-process as
// the caller, and answers how it went.
func (u goingUndoer) send(ctx context.Context, route string, values map[string]string, body any) goingWriteOutcome {
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
	request, err := spec.build(ctx, "")
	if err != nil {
		return goingFailed(err)
	}
	response, err := u.doer.Do(request)
	if err != nil {
		return goingFailed(err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode >= http.StatusMultipleChoices {
		refusal := goingRefusalOf(response, answer)
		return goingWriteOutcome{Status: refusal.Status, Code: refusal.Code, Message: refusal.Message}
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
		return goingWriteOutcome{Status: refusal.Status, Code: refusal.Code, Message: refusal.Message}
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
// over, and with neither left the rank is refused, naming why.
func (u goingUndoer) issueFields(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome, []goingUndoField) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.issueFields(ctx, write.Target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(field string, held, before, after json.RawMessage) (any, string) {
		if field != goingRank {
			if !goingSameJSON(held, after) {
				return nil, goingChangedField(field, held)
			}
			return goingWriteBackValue(field, before), ""
		}
		var heldPlace, beforePlace, afterPlace goingRankPlace
		if json.Unmarshal(held, &heldPlace) != nil || json.Unmarshal(before, &beforePlace) != nil || json.Unmarshal(after, &afterPlace) != nil {
			return nil, "rank: the write's record holds no place to put the issue back in"
		}
		// Its neighbours move as other issues are ranked; only its own rank says whether another
		// writer moved this issue.
		if heldPlace.Rank != afterPlace.Rank {
			return nil, fmt.Sprintf("rank: the issue was moved again since this write, between %s and %s",
				goingNeighbourText(heldPlace.After), goingNeighbourText(heldPlace.Before))
		}
		restore, why := u.observer.rankRestore(ctx, write.Target, beforePlace)
		if why != "" {
			return nil, why
		}
		return restore, ""
	}
	return u.writeBack(ctx, write, read, goingIssuePatchRoute, map[string]string{"key": write.Target}, restoreOf)
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

// askFields writes back each part of the ask the edit changed that still holds what the edit
// left, and names each one another writer changed since.
func (u goingUndoer) askFields(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome, []goingUndoField) {
	read := func(fields []string) (map[string]any, error) {
		current, err := u.observer.askFields(ctx, write.Target, fields)
		now, _ := current.value.(map[string]any)
		return now, err
	}
	restoreOf := func(field string, held, before, after json.RawMessage) (any, string) {
		if !goingSameJSON(held, after) {
			return nil, goingChangedField(field, held)
		}
		return goingJSONOrNullRaw(before), ""
	}
	return u.writeBack(ctx, write, read, goingAskEditRoute, map[string]string{"id": write.Target}, restoreOf)
}

// writeBack is a write back of the fields one write changed, by one PATCH through route, its
// placeholders filled from values: restoreOf says, from what each field holds now, held before
// the write and was left by it, the value the PATCH writes back, or why the field is left as it
// is - another writer changed it since. Every field's own outcome is answered beside the write's.
func (u goingUndoer) writeBack(
	ctx context.Context, write goingRunWrite, read func([]string) (map[string]any, error),
	route string, values map[string]string,
	restoreOf func(field string, held, before, after json.RawMessage) (any, string),
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
	restore := map[string]any{}
	var changes, restored []string
	outcomes := make([]goingUndoField, 0, len(fields))
	for _, field := range fields {
		value, why := restoreOf(field, goingJSONOrNull(now[field]), goingJSONOrNullRaw(before[field]), after[field])
		if why != "" {
			changes = append(changes, why)
			outcomes = append(outcomes, goingUndoField{Field: field, Message: why})
			continue
		}
		restore[field] = value
		restored = append(restored, field)
		outcomes = append(outcomes, goingUndoField{Field: field, OK: true})
	}
	if len(restore) > 0 {
		if outcome := u.send(ctx, route, values, restore); !outcome.OK {
			// Nothing was written back: every field is left with the route's refusal.
			for index := range outcomes {
				if outcomes[index].OK {
					outcomes[index] = goingUndoField{Field: outcomes[index].Field, Message: outcome.Message}
				}
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
// what the edit left.
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
	if now.Content != after.Content {
		return now, goingChanged([]string{fmt.Sprintf("content is at version %s now, written after this edit", goingVersionText(now.Version))}, nil)
	}
	route, values := goingProjectUploadRoute, map[string]string{"key": artifact.Project}
	if artifact.IssueKey != nil {
		route, values = goingIssueUploadRoute, map[string]string{"key": *artifact.IssueKey}
	}
	outcome := u.send(ctx, route, values, map[string]any{
		"name": artifact.Name, "content": before.Content, "summary": u.reason,
	})
	if !outcome.OK {
		return now, outcome
	}
	if read, err := u.observer.documentValue(ctx, artifact.ID); err == nil {
		now = read
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

// holds reports whether holder is the caller: the person themselves, or the session their
// credential proved.
func (u goingUndoer) holds(holder *goingHolder) bool {
	if holder == nil {
		return false
	}
	switch holder.Kind {
	case "user":
		return model.CanonicalLogin(holder.ID) == model.CanonicalLogin(u.login)
	case "session":
		return u.session != "" && holder.ID == u.session
	}
	return false
}

// goingHolderText names a claim's holder, or says nobody holds it.
func goingHolderText(holder *goingHolder) string {
	if holder == nil {
		return "nobody"
	}
	return holder.Kind + " " + holder.ID
}

// claim undoes a claim by releasing it, while the holder the claim left still holds it, and a
// release by taking the claim back, only where the caller held it before the release and nobody
// has taken it since.
func (u goingUndoer) claim(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after *goingHolder
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no claim to undo"))
	}
	current, _, err := u.observer.claimHolder(ctx, write.Target)
	if err != nil {
		return nil, goingFailed(err)
	}
	switch {
	case after != nil:
		if current == nil || *current != *after {
			return current, goingChanged([]string{"the claim is held by " + goingHolderText(current) + " now, not by the holder this claim left"}, nil)
		}
		outcome := u.send(ctx, goingReleaseRoute, map[string]string{"key": write.Target}, map[string]any{})
		if outcome.OK {
			current = nil
		}
		return current, outcome
	case before == nil:
		return current, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "nobody held the claim before this release, so there is nothing to take back"}
	case !u.holds(before):
		return current, goingWriteOutcome{Status: http.StatusConflict, Code: codeGoingUndoNotCallers, Message: fmt.Sprintf(
			"the claim was held by %s before this release, and taking it back would make you its holder, which it never was", goingHolderText(before))}
	case current != nil:
		return current, goingChanged([]string{"the claim is held by " + goingHolderText(current) + " now, taken after this release"}, nil)
	}
	outcome := u.send(ctx, goingClaimRoute, map[string]string{"key": write.Target}, map[string]any{})
	if outcome.OK {
		current, _, _ = u.observer.claimHolder(ctx, write.Target)
	}
	return current, outcome
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
	outcome := u.send(ctx, route, map[string]string{"id": write.Target, "session_id": after.Session}, nil)
	if outcome.OK {
		now.Following = before.Following
	}
	return now, outcome
}

// comment undoes a resolve by reopening the thread, while it is still resolved.
func (u goingUndoer) comment(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var before, after bool
	if json.Unmarshal(write.Before, &before) != nil || json.Unmarshal(write.After, &after) != nil {
		return nil, goingFailed(fmt.Errorf("the write's record holds no resolve to undo"))
	}
	current, err := u.observer.commentResolved(ctx, write.Target)
	if err != nil {
		return nil, goingFailed(err)
	}
	resolved, _ := current.value.(bool)
	switch {
	case resolved != after:
		return resolved, goingChanged([]string{"the thread is open now, reopened by another write"}, nil)
	case before == after:
		return resolved, goingWriteOutcome{OK: true, Status: http.StatusOK, Message: "the thread was resolved before this write, so there is nothing to reopen"}
	}
	outcome := u.send(ctx, goingCommentReopenRoute, map[string]string{"id": write.Target}, nil)
	if outcome.OK {
		return false, outcome
	}
	return resolved, outcome
}

// retract undoes an ask the turn opened, an approval request's among them, by retracting it while
// it is still open.
func (u goingUndoer) retract(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var after struct {
		ID  string `json:"id"`
		Ask any    `json:"ask"`
	}
	_ = json.Unmarshal(write.After, &after)
	ask := after.ID
	if write.Action == "dispatch_request_approval" {
		ask, _ = after.Ask.(string)
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
	outcome := u.send(ctx, goingAskResolveRoute, map[string]string{"id": ask}, map[string]any{"kind": "retracted", "reason": u.reason})
	if outcome.OK {
		return "resolved", outcome
	}
	return current.value, outcome
}

// closeCreated closes an issue the turn created: the undo's reason posted on it as a message,
// then its status set done, while it is still open.
func (u goingUndoer) closeCreated(ctx context.Context, write goingRunWrite) (any, goingWriteOutcome) {
	var issue struct {
		Status   string  `json:"status"`
		ClosedAt *string `json:"closed_at"`
	}
	if err := u.observer.read.get(ctx, goingIssuePath(write.Target), &issue); err != nil {
		return nil, goingFailed(err)
	}
	if issue.ClosedAt != nil {
		return issue.Status, goingChanged([]string{"the issue is closed already, at " + *issue.ClosedAt}, nil)
	}
	if outcome := u.send(ctx, goingIssueMessageRoute, map[string]string{"key": write.Target}, map[string]any{"body": u.reason}); !outcome.OK {
		return issue.Status, outcome
	}
	outcome := u.send(ctx, goingIssuePatchRoute, map[string]string{"key": write.Target}, map[string]any{"status": "done"})
	if outcome.OK {
		return "done", outcome
	}
	return issue.Status, outcome
}
