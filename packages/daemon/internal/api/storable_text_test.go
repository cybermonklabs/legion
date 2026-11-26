package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// nulText is a string whose second character is a NUL (U+0000), which PostgreSQL's text and jsonb
// cannot hold: the daemon's store keeps U+FFFD in its place.
const nulText = "a\x00b"

// nulError is the decoder's error for a string at place whose NUL is its at-th character, counted
// from 1 in code points.
func nulError(place string, at int) string {
	return fmt.Sprintf("%s holds a NUL character (U+0000) at character %d, which the daemon API refuses rather than store it as U+FFFD", place, at)
}

// The decoder of every request body refuses a string holding a NUL character, in strict mode and
// in lenient, with the daemon's own refusal: the string's place in the body, as Dispatch's walk
// finds it (dispatchbody.NulInJSON, whose own test holds every place it reads), and where in the
// string the NUL stands, counted in code points as the daemon's caps count, never the string
// itself. A character past the Basic Multilingual Plane counts once, where Dispatch's UTF-16 count
// would count two. Lenient mode reads a member it ignores, and a member's name, the name written
// with the six characters \u0000 so the refusal carries no NUL. The control is the same body with
// no NUL, taken in both modes.
func TestTheBodyDecoderRefusesAStringHoldingANul(t *testing.T) {
	type task struct {
		Text string `json:"text"`
	}
	type body struct {
		Tasks []task `json:"tasks"`
	}
	decode := func(raw string, strict bool) error {
		var into body
		_, err := decodeJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw)), &into, strict)
		return err
	}

	for _, row := range []struct {
		name, body, place string
		at                int
		lenientOnly       bool
	}{
		{name: "a member of an array's object", body: `{"tasks":[{"text":"a"},{"text":"\ud83d\ude00\u0000"}]}`, place: "tasks[1].text", at: 2},
		{name: "a member's name", body: `{"a\u0000b":"x"}`, place: `a\u0000b`, at: 2, lenientOnly: true},
	} {
		for _, strict := range []bool{true, false} {
			if strict && row.lenientOnly {
				continue
			}
			t.Run(fmt.Sprintf("%s, strict %t", row.name, strict), func(t *testing.T) {
				err := decode(row.body, strict)
				if want := nulError(row.place, row.at); err == nil || err.Error() != want {
					t.Fatalf("decode %s = %v, want %q", row.body, err, want)
				}
				if strings.Contains(err.Error(), "\x00") {
					t.Fatalf("the refusal %q carries the NUL itself", err)
				}
			})
		}
	}

	for _, strict := range []bool{true, false} {
		if err := decode(`{"tasks":[{"text":"a"},{"text":"\ud83d\ude00"}]}`, strict); err != nil {
			t.Fatalf("decode the body with no NUL, strict %t = %v, want it taken", strict, err)
		}
	}
}

// A completion whose summary holds a NUL character is refused as input naming summary, before
// GitHub is read, with nothing recorded and the issue where it was, so the agent sends it again
// corrected and that is taken. The workflow decides the fact here, writing the summary on the
// implementer's phase row as the whole daemon does: a summary holding a NUL that reached that
// write would be stored with U+FFFD in its place and the completion answered 200, so the agent
// would never learn that its summary was not kept as written.
func TestACompletionSummaryWithANulIsRefusedAsInput(t *testing.T) {
	github := newHandoffGitHub(t)
	h := newWorkflowHandoffHarness(t, github, io.Discard)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	written := implementer.writeHandoff("implement", 1)
	complete := func(summary string) *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: summary, Commit: headSHA, HandoffID: written, HandoffVersion: 1})
	}

	wantRefusal(t, complete("implemented"+nulText), http.StatusBadRequest, "invalid request body: "+nulError("summary", 13))
	if at, row, pr := handoffRecord(t, h, "LEGION-208"); at != phase.Implementing || row != nil || pr != nil {
		t.Fatalf("after the refusal LEGION-208 is in %s with implementer row %+v and pull request %+v, want implementing with neither", at, row, pr)
	}
	if served := github.serves(); len(served) != 0 {
		t.Fatalf("GitHub was asked for %v for a completion refused for its body, want nothing", served)
	}

	completion(t, complete("implemented"))
	if at, row, _ := handoffRecord(t, h, "LEGION-208"); at != phase.Testing || row == nil || row.Summary != "implemented" {
		t.Fatalf("after the corrected completion LEGION-208 is in %s with implementer row %+v, want testing with the summary recorded", at, row)
	}
}

// A completion that hides a NUL character in the first copy of a member it repeats, followed by a
// null that leaves the field as that copy set it, is refused as input naming the member, before
// anything is recorded, and the same completion written once and clean is taken: the route acts
// on what the typed decode kept, so the decoder's check reads every copy the body writes.
func TestACompletionHidesNoNulBehindARepeatedMember(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	// Each row at a commit of its own, the commit being in the completion's key: a row the decoder
	// let through is a completion of its own, and never answered as the row before it.
	for _, tc := range []struct {
		name, commit, members, place string
		at                           int
	}{
		{name: "planReason", commit: "c0ffee01", members: `"summary":"retro done","planRating":4,"planReason":"fit\u0000","planReason":null`, place: "planReason", at: 4},
		{name: "summary", commit: "c0ffee02", members: `"summary":"retro\u0000 done","summary":null,"planRating":4,"planReason":"fit"`, place: "summary", at: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRefusal(t, h.completeRaw(implementer.grant(t), fmt.Sprintf(`,"commit":%q,%s`, tc.commit, tc.members)), http.StatusBadRequest,
				"invalid request body: "+nulError(tc.place, tc.at))
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts after the refusal = %#v, want none applied", got)
			}
		})
	}

	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee01",
		PlanRating: new(4), PlanReason: new("fit")}))
	if fact := lastCompletion(t, facts); fact.Summary != "retro done" || !ratedAs(fact, 4, "fit") {
		t.Fatalf("the corrected completion's fact = %+v, want its summary and reason as sent", fact)
	}
}

// Every route of the daemon's API that reads a JSON body refuses a NUL character in a string of
// it, 400 naming the string's place, and no route answers such a body with a 5xx. The walk is over
// the server's own table (routes), which NewServer mounts whole and alone, the catch-all among
// them, so a route added without a row here fails the test until its row says what it reads: a
// string member of its body, which carries the NUL; a body of no string member, whose strict
// decoder refuses the member the NUL rides before it reads a string; or no body, when its answer
// must be no decoder refusal at all, which would mean it reads the body after all. A pattern that
// names no method (the catch-all) is sent a POST, and a route whose pattern names a claim is sent
// a claim of its own, launched for it.
func TestNoRouteTakesABodyStringHoldingANul(t *testing.T) {
	h, _ := newStaleHarness(t) // newHarness serving the state route too
	type nulRow struct {
		body any
		// refusal is the decoder's error the body is refused with; "" for a route that reads no body.
		refusal string
		// code is the code a route that refuses in the coded shape (Failure) names beside that
		// error, the task routes' INVALID_BODY; "" for a route that answers the sentence alone.
		code string
	}
	reads := func(member string) nulRow {
		return nulRow{body: map[string]any{member: nulText}, refusal: nulError(member, 2)}
	}
	// coded is reads for a task route, whose undecodable body is refused in the coded shape
	// (readTaskBody).
	coded := func(member string) nulRow { row := reads(member); row.code = "INVALID_BODY"; return row }
	// unread is a body whose one member, note, no route reads: a strict decoder refuses the member
	// before it reads the string, and a route that reads no body answers nothing about it.
	unread := map[string]any{"note": nulText}
	noString := nulRow{body: unread, refusal: `json: unknown field "note"`}
	noBody := nulRow{body: unread}
	rows := map[string]nulRow{
		"/":                                  noBody,
		"GET /legion/v1/state":               noBody,
		"GET /legion/v1/issues/{key}/record": noBody,
		"GET /healthz":                       noBody,
		"GET /readyz":                        noBody,

		"POST /legion/v1/claims/register":                        reads("bootToken"),
		"POST /legion/v1/claims/ready":                           reads("sessionId"),
		"POST /legion/v1/claims/ready-for-update":                reads("sessionId"),
		"POST /legion/v1/claims/exit":                            reads("reason"),
		"POST /legion/v1/claims/turn-outcome":                    reads("message"),
		"POST /legion/v1/claims/activity":                        reads("sessionId"),
		"POST /legion/v1/claims/release-records":                 reads("since"),
		"POST /legion/v1/claims/task":                            coded("sessionId"),
		"POST /legion/v1/claims/task-seen":                       coded("epoch"),
		"POST /legion/v1/claims/take":                            coded("epoch"),
		"POST /legion/v1/grants":                                 reads("sessionId"),
		"POST /legion/v1/controller/secret":                      noString,
		"POST /legion/v1/controller/github-credential":           reads("secret"),
		"POST /legion/v1/handoff/complete":                       reads("summary"),
		"POST /legion/v1/issues/status":                          reads("status"),
		"POST /legion/v1/claims/fresh-session":                   reads("issue"),
		"POST /legion/v1/claims/compact":                         reads("issue"),
		"POST /legion/v1/agents/stop":                            reads("issue"),
		"POST /legion/v1/agents/retry":                           reads("issue"),
		"POST /legion/v1/agents/suspend":                         reads("issue"),
		"POST /legion/v1/agents/resume":                          reads("issue"),
		"POST /legion/v1/issues/pod-update":                      reads("issue"),
		"POST /legion/v1/project/pause":                          reads("reason"),
		"POST /legion/v1/project/resume":                         reads("grantId"),
		"POST /legion/v1/gates/register":                         reads("artifactId"),
		"POST /legion/v1/plans/register":                         reads("artifactId"),
		"POST /legion/v1/tasks/stop":                             reads("task"),
		"POST /legion/v1/waves/release":                          {body: map[string]any{"issues": []string{nulText}}, refusal: nulError("issues[0]", 2)},
		"POST /legion/v1/phase/backward":                         reads("reason"),
		"POST /legion/v1/phase/retry":                            reads("issue"),
		"POST /legion/v1/signoff":                                reads("issue"),
		"POST /legion/v1/roots/close":                            reads("reason"),
		"POST /legion/v1/children/park":                          noBody,
		"POST /legion/v1/children/rerun":                         reads("issue"),
		"POST /legion/v1/intake/replay":                          reads("consumer"),
		"POST /legion/v1/intake/discard":                         reads("reason"),
		"POST /legion/v1/operator/claims":                        reads("tree"),
		"GET /legion/v1/operator/claims":                         noBody,
		"POST /legion/v1/operator/claims/{token}/deliver":        reads("task"),
		"POST /legion/v1/operator/claims/{token}/suspend":        noBody,
		"POST /legion/v1/operator/claims/{token}/resume":         noBody,
		"POST /legion/v1/operator/claims/{token}/stop":           noBody,
		"POST /legion/v1/operator/claims/{token}/retry":          noString,
		"POST /legion/v1/operator/claims/{token}/compact":        noBody,
		"POST /legion/v1/operator/claims/{token}/forget-session": reads("session"),
		"POST /legion/v1/operator/claims/{token}/release-names":  noBody,
		"POST /legion/v1/operator/claims/{token}/close":          noBody,
		"POST /legion/v1/operator/intake/replay":                 reads("consumer"),
		"POST /legion/v1/operator/intake/discard":                reads("reason"),
		"POST /legion/v1/operator/project/pause":                 reads("reason"),
		"POST /legion/v1/operator/project/resume":                noString,
	}

	served := (&server{}).routes()
	if len(served) == 0 {
		t.Fatal("the server's table holds no route: the test read nothing")
	}
	walked := map[string]bool{}
	for i, route := range served {
		pattern := route.pattern
		walked[pattern] = true
		row, ok := rows[pattern]
		if !ok {
			t.Errorf("%s has no row: say which string of its body it reads, or that it reads none", pattern)
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			method, target, named := strings.Cut(pattern, " ")
			if !named {
				method, target = http.MethodPost, pattern
			}
			if strings.Contains(target, "{token}") {
				token, _ := h.launch(fmt.Sprintf("LEGION-%d", 300+i), claim.RoleImplementer)
				target = strings.Replace(target, "{token}", string(token), 1)
			}
			recorder := h.operator(method, target, row.body)
			if recorder.Code >= http.StatusInternalServerError {
				t.Fatalf("%s answered a body holding a NUL %d %s, want no 5xx", pattern, recorder.Code, recorder.Body)
			}
			if row.refusal != "" {
				if row.code != "" {
					refusedSaying(t, recorder, http.StatusBadRequest, row.code, "invalid request body: "+row.refusal)
				} else {
					wantRefusal(t, recorder, http.StatusBadRequest, "invalid request body: "+row.refusal)
				}
				return
			}
			if strings.Contains(recorder.Body.String(), "invalid request body") {
				t.Fatalf("%s, which its row says reads no body, answered %d %s: give it the member it reads", pattern, recorder.Code, recorder.Body)
			}
		})
	}
	for pattern := range rows {
		if !walked[pattern] {
			t.Errorf("the row for %s names no route the server mounts", pattern)
		}
	}
}
