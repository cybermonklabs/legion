package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// goingSeveralRequestRow is a row of the catalogue whose write sends a request before the row's
// own, found by goingSeveralRequestRows, and arguments that make it send one. issue is the
// fixture issue the arguments name, which each case of a test replaces with an issue of its own,
// since the write closes the issue it names.
type goingSeveralRequestRow struct {
	action    string
	arguments map[string]any
	issue     string
}

// goingSeveralRequestRows is every row of the catalogue whose write sends more than one request,
// derived from the catalogue rather than written out: each row is prepared, as the writes route
// prepares it, with its row case's arguments and with each variation of them - one more of the
// action's own arguments the case leaves out, under each value of every enum argument it names -
// and a row any of whose calls translates to more than one request is one of them, with the
// fewest of those arguments that still make it send more than one, so no argument the case
// carries for another reason (a link an issue alone may hold) rides along.
func goingSeveralRequestRows(t *testing.T, rig goingWritesRig) []goingSeveralRequestRow {
	t.Helper()
	fixtures := newGoingRowFixtures(t, rig)
	cases := goingRowCases(fixtures)
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	caller := GoingCaller{Catalogue: catalogue, Doer: rig.served.goingDoer(goingCallerOf(t, rig, false))}
	several := func(action string, route GoingRoute, arguments map[string]any) bool {
		prepared, err := caller.prepare(context.Background(), action, arguments)
		if err != nil || prepared.route.Method != route.Method || prepared.route.Pattern != route.Pattern {
			return false
		}
		requests, err := prepared.call.requests(prepared.route)
		return err == nil && len(requests) > 1
	}
	var rows []goingSeveralRequestRow
	for _, action := range catalogue.Actions() {
		for _, route := range action.Routes {
			row, held := cases[action.Action+" "+route.Method+" "+route.Pattern]
			if !held || row.bearer || row.session != "" {
				continue
			}
			for _, arguments := range goingArgumentVariants(action, row.arguments) {
				if !several(action.Action, route, arguments) {
					continue
				}
				for _, name := range slices.Sorted(maps.Keys(arguments)) {
					fewer := maps.Clone(arguments)
					delete(fewer, name)
					if several(action.Action, route, fewer) {
						arguments = fewer
					}
				}
				rows = append(rows, goingSeveralRequestRow{action: action.Action, arguments: arguments, issue: fixtures.issue})
				break
			}
		}
	}
	if len(rows) == 0 {
		t.Fatal("no row of the catalogue sends more than one request, which a reasoned close does")
	}
	return rows
}

// goingArgumentVariants is arguments, and each of them with one more argument of the action's
// own that arguments leave out, under each value of every enum argument arguments name.
func goingArgumentVariants(action GoingAction, arguments map[string]any) []map[string]any {
	variants := []map[string]any{arguments}
	var added []map[string]any
	for _, field := range action.Request {
		if _, named := arguments[field.Name]; named {
			continue
		}
		var value any
		switch field.Kind {
		case contracts.ToolFieldString:
			value = "A note the derivation adds."
		case contracts.ToolFieldBoolean:
			value = true
		case contracts.ToolFieldNumber:
			value = 1
		case contracts.ToolFieldEnum:
			for _, each := range field.Values {
				added = append(added, goingWith(arguments, field.Name, each))
			}
			continue
		default:
			continue
		}
		added = append(added, goingWith(arguments, field.Name, value))
	}
	for _, variant := range added {
		variants = append(variants, variant)
		for _, field := range action.Request {
			if _, named := arguments[field.Name]; !named || field.Kind != contracts.ToolFieldEnum {
				continue
			}
			for _, each := range field.Values {
				variants = append(variants, goingWith(variant, field.Name, each))
			}
		}
	}
	return variants
}

func goingWith(arguments map[string]any, name string, value any) map[string]any {
	with := make(map[string]any, len(arguments)+1)
	for key, held := range arguments {
		with[key] = held
	}
	with[name] = value
	return with
}

// on is the row's arguments naming issue instead of the fixture issue, and the request the write
// sends first and the row's own, as method and path.
func (row goingSeveralRequestRow) on(t *testing.T, rig goingWritesRig, issue string) (map[string]any, string, string) {
	t.Helper()
	arguments := make(map[string]any, len(row.arguments))
	for key, value := range row.arguments {
		if value == row.issue {
			value = issue
		}
		arguments[key] = value
	}
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	caller := GoingCaller{Catalogue: catalogue, Doer: rig.served.goingDoer(goingCallerOf(t, rig, false))}
	prepared, err := caller.prepare(context.Background(), row.action, arguments)
	if err != nil {
		t.Fatalf("prepare %s on %s: %v", row.action, issue, err)
	}
	requests, err := prepared.call.requests(prepared.route)
	if err != nil || len(requests) < 2 {
		t.Fatalf("%s on %s sends %d requests (%v), want more than one", row.action, issue, len(requests), err)
	}
	first, own := requests[0], requests[len(requests)-1]
	return arguments, first.method + " " + first.path, own.method + " " + own.path
}

// goingCommittedThenRefused is a route table that serves the first request of `refused` whole,
// so its transaction commits, and then answers 503 in its place, as a route does whose change
// committed and whose read after it failed (createStoredMessage, messages.go); every request is
// counted by method and path.
func goingCommittedThenRefused(inner http.Handler, refused string, sent map[string]int) http.Handler {
	answered := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Method + " " + r.URL.Path
		sent[request]++
		if request == refused && !answered {
			answered = true
			inner.ServeHTTP(httptest.NewRecorder(), r)
			writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "the change committed and its answer failed")
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// TestGoingWriteWhoseRequestCommittedAndAnswered5xxIsNeverSentAgain (review round 4, finding 1;
// check c21): a request whose handler committed its change and then answered 503 landed, so
// neither the write store's retry under the write's key nor a press under a key of its own sends
// it a second time: a comment is made once, and a reasoned close posts its reason once and then
// closes.
func TestGoingWriteWhoseRequestCommittedAndAnswered5xxIsNeverSentAgain(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })

	t.Run("dispatch_comment", func(t *testing.T) {
		issue := rig.issue(t, "Commented once")
		const body = "Committed, then answered 503."
		run := rig.proposed(t, "alice", false, "dispatch_comment", map[string]any{"issue": issue, "body": body})
		comments := http.MethodPost + " /api/v1/issues/" + issue + "/comments"
		sent := map[string]int{}
		rig.served.inProcess = goingCommittedThenRefused(inner, comments, sent)
		write := map[string]any{"offer": "dispatch_comment", "invocation": 0, "approved": goingApprovedPerson}
		rig.keyedWrite(t, run, "press-comment", write)
		retry := rig.keyedWrite(t, run, "press-comment", write)
		again := rig.keyedWrite(t, run, "press-comment-again", write)
		if sent[comments] != 1 || rig.goingCommentsSaying(t, issue, body) != 1 {
			t.Fatalf("%s was sent %d times and the issue holds %d comments saying %q, want once: a retry resent a request that landed",
				comments, sent[comments], rig.goingCommentsSaying(t, issue, body), body)
		}
		if !goingLineOf(t, retry).Outcome.OK || !goingLineOf(t, again).Outcome.OK {
			t.Fatalf("the retries answered %s and %s, want the write's line, landed", retry.Body.String(), again.Body.String())
		}
	})

	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Reason committed, then 503")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			sent := map[string]int{}
			rig.served.inProcess = goingCommittedThenRefused(inner, first, sent)
			write := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.keyedWrite(t, run, "press-"+row.action, write)
			rig.keyedWrite(t, run, "press-"+row.action, write)
			line := goingLineOf(t, rig.keyedWrite(t, run, "press-"+row.action+"-again", write))
			if sent[first] != 1 {
				t.Fatalf("%s was sent %d times, want once: it committed, so it landed", first, sent[first])
			}
			if !line.Outcome.OK || sent[own] != 1 || rig.issueRead(t, issue).Status != "done" {
				t.Fatalf("the write answered %+v with %s sent %d times, want it landed once", line, own, sent[own])
			}
		})
		// The row's own request, the last the write sends, commits and then answers 503: the write
		// went through, so neither the write store's retry under its key nor a new press sends it
		// again, and the issue is done with its reason posted once.
		t.Run(row.action+" its own request", func(t *testing.T) {
			issue := rig.issue(t, "Closed, then 503")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			sent := map[string]int{}
			rig.served.inProcess = goingCommittedThenRefused(inner, own, sent)
			write := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			first1 := rig.keyedWrite(t, run, "press-own-"+row.action, write)
			retry := rig.keyedWrite(t, run, "press-own-"+row.action, write)
			again := rig.keyedWrite(t, run, "press-own-"+row.action+"-again", write)
			if sent[first] != 1 || sent[own] != 1 || rig.issueRead(t, issue).Status != "done" {
				t.Fatalf("%s was sent %d times and %s %d, want each once and the issue done", first, sent[first], own, sent[own])
			}
			for _, answer := range []*httptest.ResponseRecorder{first1, retry, again} {
				if !goingLineOf(t, answer).Outcome.OK {
					t.Fatalf("a press answered %s, want the write's line, landed", answer.Body.String())
				}
			}
		})
	}
}

// TestGoingUndoWhoseRequestCommittedAndAnswered5xxIsNeverSentAgain (review round 4, finding 1, in
// the undo): the undo of an issue the turn created posts its reason and closes it; a reason whose
// message committed and was then answered 503 landed, so the undo goes on to the close, and no
// later undo posts the reason a second time.
func TestGoingUndoWhoseRequestCommittedAndAnswered5xxIsNeverSentAgain(t *testing.T) {
	rig := newGoingWritesRig(t)
	createInteractionIssue(t, rig.mux, "DEMO", "An older issue", "The spec.\n")
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	run, line := rig.runOne(t, "dispatch_issue", map[string]any{"project": "DEMO", "title": "Made, then undone", "force": true}, nil)
	const reason = "Committed, then answered 503."
	messages := http.MethodPost + " /api/v1/issues/" + line.Item + "/messages"
	sent := map[string]int{}
	rig.served.inProcess = goingCommittedThenRefused(inner, messages, sent)
	rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", map[string]any{"reason": reason})
	rig.browser(t, "alice", http.MethodPost, "/api/v1/going/runs/"+run+"/undo", map[string]any{"reason": reason})
	rig.served.inProcess = inner
	var posted int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where issue_key = $1 and body = $2`, line.Item, reason).Scan(&posted); err != nil || posted != 1 || sent[messages] != 1 {
		t.Fatalf("the created issue holds %d messages saying %q (%v), with %s sent %d times, want the reason posted once", posted, reason, err, messages, sent[messages])
	}
	if status := rig.issueRead(t, line.Item).Status; status != "done" {
		t.Fatalf("the created issue is %s after the undos, want done", status)
	}
}

// goingDiesAfter is a route table that serves the first request of `dying` whole, so its
// transaction commits, and then ends the request making it, as a process ends between a change
// and its answer: nothing after the change - the answer, the write's record - happens.
func goingDiesAfter(inner http.Handler, dying string) http.Handler {
	died := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == dying && !died {
			died = true
			inner.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	})
}

// dieWriting sends one write of the run that the route table ends.
func (rig goingWritesRig) dieWriting(t *testing.T, run string, write map[string]any) {
	t.Helper()
	func() {
		defer func() {
			if ended := recover(); ended != nil && !errors.Is(asError(ended), http.ErrAbortHandler) {
				panic(ended)
			}
		}()
		rig.write(t, "alice", run, write)
	}()
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}

// ageGoingWrites puts every write of the run's claim past goingClaimBound, as a claim whose
// request ended long ago.
func (rig goingWritesRig) ageGoingWrites(t *testing.T, run string) {
	t.Helper()
	aged := time.Now().UTC().Add(-(rig.served.goingClaimBound() + time.Minute)).Format(time.RFC3339Nano)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update going_runs set writes = (
		  select jsonb_agg(jsonb_set(w, '{at}', to_jsonb($2::text)) order by n)
		  from jsonb_array_elements(writes) with ordinality e(w, n))
		where id = $1::uuid`, run, aged); err != nil {
		t.Fatalf("age the run's writes: %v", err)
	}
}

// TestGoingWriteWhoseProcessEndedAfterARequestLandedResumes (review round 4, finding 1; check c21):
// a write whose request committed and whose process then ended, before the request's answer and
// the write's record, leaves the record of that landing, made in the same transaction as the
// change: once no request can still be making it, the next press finds the record and resumes
// after what landed rather than answering the write unsettled, and sends nothing twice.
func TestGoingWriteWhoseProcessEndedAfterARequestLandedResumes(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })

	t.Run("dispatch_comment", func(t *testing.T) {
		issue := rig.issue(t, "Commented, then ended")
		const body = "Committed as the process ended."
		run := rig.proposed(t, "alice", false, "dispatch_comment", map[string]any{"issue": issue, "body": body})
		write := map[string]any{"offer": "dispatch_comment", "invocation": 0, "approved": goingApprovedPerson}
		rig.served.inProcess = goingDiesAfter(inner, http.MethodPost+" /api/v1/issues/"+issue+"/comments")
		rig.dieWriting(t, run, write)
		rig.served.inProcess = inner
		rig.ageGoingWrites(t, run)
		line := goingLineOf(t, rig.write(t, "alice", run, write))
		if !line.Outcome.OK || rig.goingCommentsSaying(t, issue, body) != 1 {
			t.Fatalf("the press after the process ended answered %+v with %d comments saying %q, want the write landed once",
				line, rig.goingCommentsSaying(t, issue, body), body)
		}
		if writes := rig.record(t, "alice", run).Writes; len(writes) != 1 || writes[0].Pending || !writes[0].Outcome.OK {
			t.Fatalf("the run keeps %+v, want the one write, recorded landed", writes)
		}
	})

	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Reason posted, then ended")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			write := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.served.inProcess = goingDiesAfter(inner, first)
			rig.dieWriting(t, run, write)
			sent := map[string]int{}
			rig.served.inProcess = goingCommittedThenRefused(inner, "", sent)
			rig.ageGoingWrites(t, run)
			line := goingLineOf(t, rig.write(t, "alice", run, write))
			if !line.Outcome.OK || sent[first] != 0 || sent[own] != 1 || rig.issueRead(t, issue).Status != "done" {
				t.Fatalf("the press after the process ended answered %+v with %s sent %d times and %s %d, want it resumed at %s and landed",
					line, first, sent[first], own, sent[own], own)
			}
		})
	}
}

// goingDiesBefore is a route table that ends the request making `dying`, the first time it is
// sent, before its route sees it: the process ends after every earlier request of the write was
// answered and before the write's record.
func goingDiesBefore(inner http.Handler, dying string) http.Handler {
	died := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == dying && !died {
			died = true
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	})
}

// TestGoingWriteTakenOverAnswersAStatusAndWhatItsLandingsMade (review round 5, finding 4): a
// write taken over after its process ended is answered with a status, never ok with status 0,
// and a request of it that was answered before the process ended keeps, in the record of its
// landing, the status it was answered and the item it made, so the taken-over write names them.
func TestGoingWriteTakenOverAnswersAStatusAndWhatItsLandingsMade(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })

	t.Run("every request landed before the process ended", func(t *testing.T) {
		issue := rig.issue(t, "Commented, then ended unanswered")
		run := rig.proposed(t, "alice", false, "dispatch_comment", map[string]any{"issue": issue, "body": "Landed unanswered."})
		write := map[string]any{"offer": "dispatch_comment", "invocation": 0, "approved": goingApprovedPerson}
		rig.served.inProcess = goingDiesAfter(inner, http.MethodPost+" /api/v1/issues/"+issue+"/comments")
		rig.dieWriting(t, run, write)
		rig.served.inProcess = inner
		rig.ageGoingWrites(t, run)
		line := goingLineOf(t, rig.write(t, "alice", run, write))
		if !line.Outcome.OK || line.Outcome.Status == 0 {
			t.Fatalf("the taken-over write answered %+v, want it landed with a status", line.Outcome)
		}
	})

	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Reason answered, then ended")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			write := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.served.inProcess = goingDiesBefore(inner, own)
			rig.dieWriting(t, run, write)
			rig.served.inProcess = inner
			rig.ageGoingWrites(t, run)
			goingLineOf(t, rig.write(t, "alice", run, write))
			recorded := rig.requestsRecorded(t, run)
			if len(recorded) != 1 || len(recorded[0].Requests) != 2 {
				t.Fatalf("the run keeps %+v, want the one write with its two requests", recorded)
			}
			if landed := recorded[0].Requests[0]; landed.Method+" "+landed.Path != first || landed.Outcome.Status != http.StatusCreated || landed.Made == "" {
				t.Fatalf("the taken-over write records %s as %+v, want it landed 201 naming the message it made", first, landed)
			}
		})
	}
}

// TestGoingWriteResumedAfterItsReasonLandedSaysSoWhenRefused (review round 4, finding 7): a
// reasoned close whose reason landed and whose close was refused 503 resumes at the close; where
// someone closed the issue in between, the refusal says the reason stands, and does not say it
// was not posted.
func TestGoingWriteResumedAfterItsReasonLandedSaysSoWhenRefused(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Closed in between")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			refused := false
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path == own && !refused {
					refused = true
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "the close was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			write := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			if half := rig.write(t, "alice", run, write); half.Code != http.StatusServiceUnavailable {
				t.Fatalf("the half-made write answered %d %s, want 503", half.Code, half.Body.String())
			}
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"})
			resumed := rig.write(t, "alice", run, write)
			refusal := goingRefusalSeenOf(t, resumed)
			_, path, _ := strings.Cut(first, " ")
			if resumed.Code != http.StatusConflict || strings.Contains(refusal.Error, "not posted") || !strings.Contains(refusal.Error, path) {
				t.Fatalf("the resumed write answered %d %+v, want its refusal naming %s, which landed, and never saying the reason was not posted",
					resumed.Code, refusal, first)
			}
		})
	}
}

// TestGoingWriteOfOneInvocationClaimedAfterAnotherRecordedResumesFromThatRecord (review round 4,
// finding 3): a request for an invocation reads the run, and before its claim lands another
// request of the same invocation claims it, posts the reason and records the write refused after
// that landing. The first request's claim must not resume from the run it read, which records
// nothing landed: it resumes after the reason the other recorded, and sends no second reason.
func TestGoingWriteOfOneInvocationClaimedAfterAnotherRecordedResumesFromThatRecord(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Pressed in two tabs")
			arguments, first, own := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			sent := map[string]int{}
			rig.served.inProcess = goingCommittedThenRefused(inner, "", sent)
			method, path, _ := strings.Cut(first, " ")
			rig.holdGoingRun(t, run, func(held pgx.Tx) {
				// What the other request records once its reason landed and its close was refused
				// 503: a write of the invocation, refused, its reason landed.
				if _, err := held.Exec(context.Background(), `
					update going_runs set writes = writes || jsonb_build_array(jsonb_build_object(
					  'id', gen_random_uuid()::text, 'action', $2::text, 'invocation', 0, 'target', $3::text,
					  'fields', '[]'::jsonb, 'before', null, 'after', null, 'approved', 'person', 'pending', false,
					  'at', now(), 'outcome', jsonb_build_object('ok', false, 'status', 503, 'code', 'SERVICE_UNAVAILABLE', 'message', 'refused'),
					  'requests', jsonb_build_array(
					    jsonb_build_object('method', $4::text, 'path', $5::text, 'outcome', jsonb_build_object('ok', true, 'status', 201)),
					    jsonb_build_object('method', 'PATCH', 'path', '/api/v1/issues/' || $3::text,
					      'outcome', jsonb_build_object('ok', false, 'status', 503, 'code', 'SERVICE_UNAVAILABLE', 'message', 'refused')))))
					where id = $1::uuid`, run, row.action, issue, method, path); err != nil {
					t.Fatalf("record the other request's write: %v", err)
				}
			}, func() {
				rig.write(t, "alice", run, map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson})
			})
			if sent[first] != 0 || sent[own] != 1 {
				t.Fatalf("the request claimed after the other recorded sent %s %d times and %s %d, want it resumed after the reason the other posted",
					first, sent[first], own, sent[own])
			}
		})
	}
}

// holdGoingRun runs request while the test's own transaction holds the run's row, so request
// reads the run and then waits at its claim; then, in that transaction, change writes what
// another request records in the meantime, and commits, so the claim lands on what change left.
func (rig goingWritesRig) holdGoingRun(t *testing.T, run string, change func(pgx.Tx), request func()) {
	t.Helper()
	ctx := context.Background()
	lock, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `select 1 from going_runs where id = $1::uuid for update`, run); err != nil {
		t.Fatalf("hold the run: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		request()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where wait_event_type = 'Lock' and query like '%update going_runs%' and pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("read who waits on the run: %v", err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-done:
			t.Fatal("the request ended without waiting on the run it read")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never reached its claim of the run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The change is the other request's, made as the held transaction's own write.
	change(lock)
	if err := lock.Commit(ctx); err != nil {
		t.Fatalf("let the run go: %v", err)
	}
	<-done
}

// TestGoingWriteUploadWhoseOwnerCannotBeNamedIsNeverAutomatic (review round 4, finding 5): an
// upload sent at once whose route names no owner the write can read is held to the documents an
// owner it cannot name may hold, so it is refused, as the proposal treats an owner it could not
// read; one whose words write its name still runs at once.
func TestGoingWriteUploadWhoseOwnerCannotBeNamedIsNeverAutomatic(t *testing.T) {
	prepared := goingPrepared{
		action: "dispatch_artifact", route: GoingRoute{Method: http.MethodPost, Pattern: "/api/v1/somewhere/{key}/artifacts"},
		arguments: map[string]any{"name": "notes", "content": "# Notes\n"}, call: goingCall{path: map[string]string{"key": "DEMO-1"}},
	}
	before, err := (goingObserver{}).uploadedOver(context.Background(), prepared, goingFields("artifact"), "")
	if err != nil {
		t.Fatalf("read what the upload writes over: %v", err)
	}
	if !goingUploadsOver(prepared, before, "save this as a doc") {
		t.Error("an upload whose owner the write cannot name runs at once, want it refused as one that may write over a document")
	}
	if goingUploadsOver(prepared, before, "save this as notes") {
		t.Error("an upload whose words write its name is refused, want it run at once")
	}
}

// TestGoingWriteResumedOntoOtherArgumentsCountsNoStaleLandingAsItsOwn (review round 6, finding 1;
// check c21): a reasoned close of issue A posts A's reason and its close is refused 503 before it
// commits; the next press replaces the invocation's arguments to close issue B instead, so it
// resumes nothing, and B's reason is refused 403 before it commits. The landing the claim kept
// from A's attempt is no landing of B's request: the write is refused, B is not closed, and the
// record does not say B's reason landed.
func TestGoingWriteResumedOntoOtherArgumentsCountsNoStaleLandingAsItsOwn(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			a, b := rig.issue(t, "Closed first"), rig.issue(t, "Closed instead")
			argumentsA, _, ownA := row.on(t, rig, a)
			argumentsB, firstB, ownB := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			sent := map[string]int{}
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := r.Method + " " + r.URL.Path
				sent[request]++
				switch request {
				case ownA:
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "A's close was refused")
					return
				case firstB:
					writeError(w, codeScopeRequired, http.StatusForbidden, "B's reason was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			if half := rig.write(t, "alice", run, press); half.Code != http.StatusServiceUnavailable {
				t.Fatalf("the press on %s answered %d %s, want its close's 503", a, half.Code, half.Body.String())
			}
			press["arguments"] = argumentsB
			refused := rig.write(t, "alice", run, press)
			if refused.Code != http.StatusForbidden || sent[ownB] != 0 || rig.issueRead(t, b).Status == "done" {
				t.Fatalf("the press on %s answered %d %s with %s sent %d times and %s %s, want its reason's 403 and %s left open",
					b, refused.Code, refused.Body.String(), ownB, sent[ownB], b, rig.issueRead(t, b).Status, b)
			}
			recorded := rig.requestsRecorded(t, run)
			latest := recorded[len(recorded)-1]
			if latest.Outcome.OK || len(latest.Requests) == 0 || latest.Requests[0].Method+" "+latest.Requests[0].Path != firstB || latest.Requests[0].Outcome.OK {
				t.Fatalf("the press on %s is recorded %+v, want it refused with %s recorded not landed", b, latest, firstB)
			}
		})
	}
}

// TestGoingWritePressedBackToItsFirstArgumentsResumesAfterWhatTheyLanded (review round 7; check
// c21): a reasoned close of A posts A's reason and its close is refused 503 before it commits; a
// press with arguments for B is refused before anything of it lands; a press back to A's own
// arguments resumes after A's reason, which landed in the first press, though the write between
// landed nothing: A holds one reason in all, and ends done.
func TestGoingWritePressedBackToItsFirstArgumentsResumesAfterWhatTheyLanded(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			a, b := rig.issue(t, "Closed first and last"), rig.issue(t, "Pressed between")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			argumentsB, firstB, _ := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			sent := map[string]int{}
			refusedA := false
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := r.Method + " " + r.URL.Path
				sent[request]++
				switch {
				case request == ownA && !refusedA:
					refusedA = true
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "A's close was refused")
					return
				case request == firstB:
					writeError(w, codeScopeRequired, http.StatusForbidden, "B's reason was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			if half := rig.write(t, "alice", run, press); half.Code != http.StatusServiceUnavailable {
				t.Fatalf("the press on %s answered %d %s, want its close's 503", a, half.Code, half.Body.String())
			}
			press["arguments"] = argumentsB
			if refused := rig.write(t, "alice", run, press); refused.Code != http.StatusForbidden {
				t.Fatalf("the press on %s answered %d %s, want its reason's 403", b, refused.Code, refused.Body.String())
			}
			press["arguments"] = argumentsA
			line := goingLineOf(t, rig.write(t, "alice", run, press))
			var reasons int
			if err := rig.database.Pool.QueryRow(context.Background(),
				`select count(*) from messages where issue_key = $1 and body = $2`, a, argumentsA["reason"]).Scan(&reasons); err != nil {
				t.Fatalf("count %s's reasons: %v", a, err)
			}
			if !line.Outcome.OK || reasons != 1 || sent[firstA] != 1 || rig.issueRead(t, a).Status != "done" {
				t.Fatalf("the press back on %s answered %+v with %s sent %d times and %d reasons on %s, want it resumed after the reason, which is posted once, and %s done",
					a, line.Outcome, firstA, sent[firstA], reasons, a, a)
			}
		})
	}
}

// goingReasonsOn is how many messages of issue say body.
func (rig goingWritesRig) goingReasonsOn(t *testing.T, issue string, body any) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from messages where issue_key = $1 and body = $2`, issue, body).Scan(&count); err != nil {
		t.Fatalf("count %s's messages saying %v: %v", issue, body, err)
	}
	return count
}

// TestGoingWriteTakenOverWithOtherArgumentsKeepsTheDeadWritesRecord (review round 8, finding 1;
// check c21): a write whose process ended is taken over by a press whose arguments name another
// issue. The taken-over write's landings are never rewritten: it is recorded as it stands, naming
// what it landed. Where all of it landed, it went through: the press is answered that write's line
// and sends nothing for B, and, the value it left never read, the turn's undo leaves A as it is,
// as for every write taken over after all of it landed. Where only A's reason landed, it is
// recorded refused naming the reason, the press runs as a write of its own, and a later press back
// to A resumes after A's reason and posts it once.
func TestGoingWriteTakenOverWithOtherArgumentsKeepsTheDeadWritesRecord(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action+" every request landed", func(t *testing.T) {
			a, b := rig.issue(t, "Closed as the process ended"), rig.issue(t, "Pressed after")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			argumentsB, _, _ := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.served.inProcess = goingDiesAfter(inner, ownA)
			rig.dieWriting(t, run, press)
			rig.served.inProcess = inner
			rig.ageGoingWrites(t, run)
			press["arguments"] = argumentsB
			line := goingLineOf(t, rig.write(t, "alice", run, press))
			if line.Item != a || rig.issueRead(t, b).Status == "done" {
				t.Fatalf("the press on %s answered %+v and left %s %s, want the line of the write that closed %s and %s left open",
					b, line, b, rig.issueRead(t, b).Status, a, b)
			}
			recorded := rig.requestsRecorded(t, run)
			if len(recorded) != 1 || recorded[0].Pending || !recorded[0].Outcome.OK || len(recorded[0].Requests) != 2 ||
				recorded[0].Requests[0].Method+" "+recorded[0].Requests[0].Path != firstA || recorded[0].Requests[1].Method+" "+recorded[0].Requests[1].Path != ownA {
				t.Fatalf("the run keeps %+v, want the one write, through, naming %s and %s", recorded, firstA, ownA)
			}
			undone := oneLine(t, rig.undo(t, "alice", run, nil), false)
			if undone.Outcome.Code != codeGoingUndoAfterUnread || rig.issueRead(t, a).Status != "done" {
				t.Fatalf("the turn's undo answered %+v and left %s %s, want %s and %s done", undone.Outcome, a, rig.issueRead(t, a).Status, codeGoingUndoAfterUnread, a)
			}
		})
		t.Run(row.action+" its reason landed", func(t *testing.T) {
			a, b := rig.issue(t, "Reasoned as the process ended"), rig.issue(t, "Refused after")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			argumentsB, firstB, _ := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.served.inProcess = goingDiesAfter(inner, firstA)
			rig.dieWriting(t, run, press)
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path == firstB {
					writeError(w, codeScopeRequired, http.StatusForbidden, "B's reason was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			rig.ageGoingWrites(t, run)
			press["arguments"] = argumentsB
			if refused := rig.write(t, "alice", run, press); refused.Code != http.StatusForbidden {
				t.Fatalf("the press on %s answered %d %s, want its reason's 403", b, refused.Code, refused.Body.String())
			}
			recorded := rig.requestsRecorded(t, run)
			if len(recorded) != 2 || recorded[0].Pending || recorded[0].Outcome.OK || len(recorded[0].Requests) != 1 ||
				recorded[0].Requests[0].Method+" "+recorded[0].Requests[0].Path != firstA || !recorded[0].Requests[0].Outcome.OK ||
				recorded[1].Outcome.OK || len(recorded[1].Requests) == 0 || recorded[1].Requests[0].Method+" "+recorded[1].Requests[0].Path != firstB {
				t.Fatalf("the run keeps %+v, want the taken-over write refused naming %s, which landed, and a write of its own for %s", recorded, firstA, b)
			}
			rig.served.inProcess = inner
			press["arguments"] = argumentsA
			line := goingLineOf(t, rig.write(t, "alice", run, press))
			if !line.Outcome.OK || rig.goingReasonsOn(t, a, argumentsA["reason"]) != 1 || rig.issueRead(t, a).Status != "done" {
				t.Fatalf("the press back on %s answered %+v with %d reasons on it and %s %s, want %s resumed after its one reason and done",
					a, line.Outcome, rig.goingReasonsOn(t, a, argumentsA["reason"]), a, rig.issueRead(t, a).Status, ownA)
			}
		})
	}
}

// TestGoingWriteRefusedOnAnotherIssueSaysNothingOfItWasSent (review round 8, finding 2): A's
// reason landed and its close was refused 503; a press with arguments for B, which someone closed,
// is refused before anything of it is sent, and says that neither B's reason nor its close was
// sent, and names nothing of A's as its own.
func TestGoingWriteRefusedOnAnotherIssueSaysNothingOfItWasSent(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			a, b := rig.issue(t, "Half closed"), rig.issue(t, "Closed by bob")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			argumentsB, _, _ := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path == ownA {
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "A's close was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.write(t, "alice", run, press)
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+b, map[string]any{"status": "done"})
			press["arguments"] = argumentsB
			refused := rig.write(t, "alice", run, press)
			refusal := goingRefusalSeenOf(t, refused)
			_, pathA, _ := strings.Cut(firstA, " ")
			if refused.Code != http.StatusConflict || !strings.Contains(refusal.Error, "the reason was not posted") || strings.Contains(refusal.Error, pathA) {
				t.Fatalf("the press on %s answered %d %+v, want it refused saying its reason was not posted, naming nothing of %s", b, refused.Code, refusal, a)
			}
		})
	}
}

// TestGoingWriteWithAnotherReasonPostsItsOwn (review round 8, finding 3): A's reason landed and its
// close was refused 503; a press that changes only the reason resumes after nothing, since a
// landing counts only for the request of its own body: it posts its own reason and closes, and the
// first reason stays posted, named on the first write's record.
func TestGoingWriteWithAnotherReasonPostsItsOwn(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			a := rig.issue(t, "Reasoned twice")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			refusedA := false
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path == ownA && !refusedA {
					refusedA = true
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "A's close was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.write(t, "alice", run, press)
			other := maps.Clone(argumentsA)
			other["reason"] = "Shipped in the release after all."
			press["arguments"] = other
			line := goingLineOf(t, rig.write(t, "alice", run, press))
			if !line.Outcome.OK || rig.goingReasonsOn(t, a, other["reason"]) != 1 || rig.goingReasonsOn(t, a, argumentsA["reason"]) != 1 || rig.issueRead(t, a).Status != "done" {
				t.Fatalf("the press with another reason answered %+v with %d of its reasons and %d of the first on %s, want each once and %s done",
					line.Outcome, rig.goingReasonsOn(t, a, other["reason"]), rig.goingReasonsOn(t, a, argumentsA["reason"]), a, a)
			}
			if recorded := rig.requestsRecorded(t, run); len(recorded) != 2 || len(recorded[0].Requests) == 0 ||
				recorded[0].Requests[0].Method+" "+recorded[0].Requests[0].Path != firstA || !recorded[0].Requests[0].Outcome.OK {
				t.Fatalf("the run keeps %+v, want the first write naming its reason landed", recorded)
			}
		})
	}
}

// TestGoingWriteTakenOverAndRefusedByItsTranslationNamesItsLandingsOnce (review round 9,
// finding 1): a reasoned close whose process ended after its reason landed is taken over by a
// press with the same arguments after someone closed the issue; the translation refuses it. The
// refusal names the landed reason once, and the record keeps the item and fields the write
// planned.
func TestGoingWriteTakenOverAndRefusedByItsTranslationNamesItsLandingsOnce(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			issue := rig.issue(t, "Reasoned, ended, then closed")
			arguments, first, _ := row.on(t, rig, issue)
			run := rig.proposed(t, "alice", false, row.action, arguments)
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.served.inProcess = goingDiesAfter(inner, first)
			rig.dieWriting(t, run, press)
			rig.served.inProcess = inner
			rig.fixture(t, "bob", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": "done"})
			rig.ageGoingWrites(t, run)
			refused := rig.write(t, "alice", run, press)
			refusal := goingRefusalSeenOf(t, refused)
			_, path, _ := strings.Cut(first, " ")
			if refused.Code != http.StatusConflict || strings.Count(refusal.Error, path) != 1 {
				t.Fatalf("the taken-over press answered %d %+v, want it refused naming %s once", refused.Code, refusal, first)
			}
			writes := rig.record(t, "alice", run).Writes
			if len(writes) != 1 || writes[0].Pending || writes[0].Target != issue || len(writes[0].Fields) == 0 {
				t.Fatalf("the run keeps %+v, want the one write, refused, keeping its item %s and the fields it planned", writes, issue)
			}
		})
	}
}

// TestGoingWriteTakenOverBeforeItsPlanNamesNothingItInherited (review round 9, finding 2): a
// reasoned close of A lands its reason and is refused 503; the next press's claim, which starts
// out holding that landed reason, ends before its call is planned; a press for B takes it over.
// The taken-over write is settled naming nothing it did not land itself, and B is closed.
func TestGoingWriteTakenOverBeforeItsPlanNamesNothingItInherited(t *testing.T) {
	rig := newGoingWritesRig(t)
	rows := goingSeveralRequestRows(t, rig)
	inner := rig.served.inProcess
	t.Cleanup(func() { rig.served.inProcess = inner })
	for _, row := range rows {
		t.Run(row.action, func(t *testing.T) {
			a, b := rig.issue(t, "Half closed, then pressed again"), rig.issue(t, "Pressed after")
			argumentsA, firstA, ownA := row.on(t, rig, a)
			argumentsB, _, _ := row.on(t, rig, b)
			run := rig.proposed(t, "alice", false, row.action, argumentsA)
			rig.served.inProcess = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path == ownA {
					writeError(w, "SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "A's close was refused")
					return
				}
				inner.ServeHTTP(w, r)
			})
			press := map[string]any{"offer": row.action, "invocation": 0, "approved": goingApprovedPerson}
			rig.write(t, "alice", run, press)
			rig.served.inProcess = goingDiesBefore(inner, http.MethodGet+" /api/v1/issues/"+a)
			rig.dieWriting(t, run, press)
			rig.served.inProcess = inner
			rig.ageGoingWrites(t, run)
			press["arguments"] = argumentsB
			line := goingLineOf(t, rig.write(t, "alice", run, press))
			_, pathA, _ := strings.Cut(firstA, " ")
			recorded := rig.requestsRecorded(t, run)
			if !line.Outcome.OK || rig.issueRead(t, b).Status != "done" || len(recorded) != 3 || recorded[1].Pending ||
				recorded[1].Outcome.OK || strings.Contains(recorded[1].Outcome.Message, pathA) {
				t.Fatalf("the press on %s answered %+v and the run keeps %+v, want the taken-over write settled naming nothing of %s and %s done",
					b, line.Outcome, recorded, firstA, b)
			}
		})
	}
}
