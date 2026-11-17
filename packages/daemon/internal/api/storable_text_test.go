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
// cannot hold: a statement that stores it fails.
const nulText = "a\x00b"

// nulError is the decoder's error for a string at place whose NUL is its at-th character, counted
// from 1 in code points.
func nulError(place string, at int) string {
	return fmt.Sprintf("%s holds a NUL character (U+0000) at character %d, which the daemon cannot store", place, at)
}

// The decoder of every request body refuses a string holding a NUL character wherever it stands:
// a member at the top, a member of a nested object, an element of an array and a member of an
// array's object, in strict mode and in lenient. The refusal names the string's place in the body
// and where in it the NUL stands, counted in code points, and never the string. Lenient mode reads
// every string the body holds, a member it ignores and a member's name among them, the name written
// with the six characters \u0000 so the refusal carries no NUL. The controls are taken in both
// modes: the same members with no NUL, and a string holding the six characters \u0000 as text.
func TestTheBodyDecoderRefusesAStringHoldingANul(t *testing.T) {
	type task struct {
		Text string `json:"text"`
	}
	type body struct {
		PlanReason string `json:"planReason"`
		Plan       struct {
			Source string `json:"source"`
		} `json:"plan"`
		Labels []string `json:"labels"`
		Tasks  []task   `json:"tasks"`
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
		{name: "a top-level member", body: `{"planReason":"fit\u0000"}`, place: "planReason", at: 4},
		{name: "a nested object's member", body: `{"plan":{"source":"own\u0000er"}}`, place: "plan.source", at: 4},
		{name: "an array element", body: `{"labels":["ok","b\u0000"]}`, place: "labels[1]", at: 2},
		{name: "a member of an array's object", body: `{"tasks":[{"text":"a"},{"text":"é\u0000"}]}`, place: "tasks[1].text", at: 2},
		{name: "a member the route does not read", body: `{"ignored":"x\u0000"}`, place: "ignored", at: 2, lenientOnly: true},
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

	for _, taken := range []string{
		`{"planReason":"fit","plan":{"source":"owner"},"labels":["ok","b"],"tasks":[{"text":"a"},{"text":"é"}]}`,
		`{"planReason":"fit\\u0000"}`,
	} {
		for _, strict := range []bool{true, false} {
			if err := decode(taken, strict); err != nil {
				t.Fatalf("decode %s, strict %t = %v, want it taken", taken, strict, err)
			}
		}
	}
}

// A completion whose summary holds a NUL character is refused as input naming summary, before
// GitHub is read, with nothing recorded and the issue where it was, so the agent sends it again
// corrected and that is taken. The workflow decides the fact here, writing the summary on the
// implementer's phase row as the whole daemon does, which is where PostgreSQL refused it and the
// completion was answered 500 FACT_APPLY_FAILED, naming no field.
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

// Every route of the daemon's API that reads a JSON body refuses a NUL character in a string of
// it, 400 naming the string's place, and no route answers such a body with a 5xx. The walk is over
// the routes NewServer mounts (routes), so a route added without a row here fails the test until
// its row says what it reads: a string member of its body, which carries the NUL; a body of no
// string member, whose strict decoder refuses the member the NUL rides before it reads a string;
// or no body, when its answer must be no decoder refusal at all, which would mean it reads the body
// after all. A route whose pattern names a claim is sent a claim of its own, launched for it.
func TestNoRouteTakesABodyStringHoldingANul(t *testing.T) {
	h, _ := newStaleHarness(t) // newHarness serving the state route too
	type nulRow struct {
		body any
		// refusal is the decoder's error the body is refused with; "" for a route that reads no body.
		refusal string
	}
	reads := func(member string) nulRow { return nulRow{map[string]any{member: nulText}, nulError(member, 2)} }
	noString := nulRow{map[string]any{"note": nulText}, `json: unknown field "note"`}
	noBody := nulRow{map[string]any{"note": nulText}, ""}
	rows := map[string]nulRow{
		"GET /legion/v1/state": noBody,
		"GET /healthz":         noBody,
		"GET /readyz":          noBody,

		"POST /legion/v1/claims/register":                        reads("bootToken"),
		"POST /legion/v1/claims/ready":                           reads("sessionId"),
		"POST /legion/v1/claims/ready-for-update":                reads("sessionId"),
		"POST /legion/v1/claims/exit":                            reads("reason"),
		"POST /legion/v1/claims/turn-outcome":                    reads("message"),
		"POST /legion/v1/claims/activity":                        reads("sessionId"),
		"POST /legion/v1/claims/release-records":                 reads("since"),
		"POST /legion/v1/grants":                                 reads("sessionId"),
		"POST /legion/v1/controller/secret":                      noString,
		"POST /legion/v1/controller/github-credential":           reads("secret"),
		"POST /legion/v1/handoff/complete":                       reads("summary"),
		"POST /legion/v1/issues/status":                          reads("status"),
		"POST /legion/v1/claims/fresh-session":                   reads("issue"),
		"POST /legion/v1/claims/compact":                         reads("issue"),
		"POST /legion/v1/gates/register":                         reads("artifactId"),
		"POST /legion/v1/waves/release":                          {map[string]any{"issues": []string{nulText}}, nulError("issues[0]", 2)},
		"POST /legion/v1/phase/backward":                         reads("reason"),
		"POST /legion/v1/phase/retry":                            reads("issue"),
		"POST /legion/v1/signoff":                                reads("issue"),
		"POST /legion/v1/roots/close":                            reads("reason"),
		"POST /legion/v1/children/park":                          reads("issue"),
		"POST /legion/v1/children/rerun":                         reads("issue"),
		"POST /legion/v1/operator/claims":                        reads("tree"),
		"GET /legion/v1/operator/claims":                         noBody,
		"POST /legion/v1/operator/claims/{token}/deliver":        reads("task"),
		"POST /legion/v1/operator/claims/{token}/suspend":        noBody,
		"POST /legion/v1/operator/claims/{token}/resume":         noBody,
		"POST /legion/v1/operator/claims/{token}/stop":           noBody,
		"POST /legion/v1/operator/claims/{token}/retry":          noString,
		"POST /legion/v1/operator/claims/{token}/compact":        noBody,
		"POST /legion/v1/operator/claims/{token}/forget-session": reads("session"),
		"POST /legion/v1/operator/claims/{token}/close":          noBody,
	}

	walked := map[string]bool{}
	for i, route := range (&server{}).routes() {
		walked[route.pattern] = true
		row, ok := rows[route.pattern]
		if !ok {
			t.Errorf("%s has no row: say which string of its body it reads, or that it reads none", route.pattern)
			continue
		}
		t.Run(route.pattern, func(t *testing.T) {
			method, target, _ := strings.Cut(route.pattern, " ")
			if strings.Contains(target, "{token}") {
				token, _ := h.launch(fmt.Sprintf("LEGION-%d", 300+i), claim.RoleImplementer)
				target = strings.Replace(target, "{token}", string(token), 1)
			}
			recorder := h.operator(method, target, row.body)
			if recorder.Code >= http.StatusInternalServerError {
				t.Fatalf("%s answered a body holding a NUL %d %s, want no 5xx", route.pattern, recorder.Code, recorder.Body)
			}
			if row.refusal != "" {
				wantRefusal(t, recorder, http.StatusBadRequest, "invalid request body: "+row.refusal)
				return
			}
			if strings.Contains(recorder.Body.String(), "invalid request body") {
				t.Fatalf("%s, which its row says reads no body, answered %d %s: give it the member it reads", route.pattern, recorder.Code, recorder.Body)
			}
		})
	}
	for pattern := range rows {
		if !walked[pattern] {
			t.Errorf("the row for %s names no route the server mounts", pattern)
		}
	}
}
