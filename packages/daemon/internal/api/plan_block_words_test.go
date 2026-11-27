package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// The bearers the daemon's read of a plan carries in these rows. echoedBearer is lower-case
// letters, digits and hyphens, so an id that is it whole, or a run of it, is in the task-id grammar
// and only the check for the read's credential refuses it; it holds the probe's token, a marker
// the redaction gate reads its evidence by. plainBearer shares no run with the probe's URL, so a
// block quoting that URL is refused by no check for the read's credential.
const (
	echoedBearer = "q7v2-" + oauth2calltest.Token + "-x9k3"
	plainBearer  = "zq7v2x9k3m4p8w1r5t0j"
)

// standInPlanReads is the architect harness's Dispatch fake with its plan reads made by the
// daemon's own client (dispatch.HTTPClient.PlanDocument) of a stand-in in Dispatch's place.
type standInPlanReads struct {
	*statusRecorder
	client *dispatch.HTTPClient
}

func (s standInPlanReads) PlanDocument(ctx context.Context, artifactID string, version int) (dispatch.DocumentVersion, error) {
	return s.client.PlanDocument(ctx, artifactID, version)
}

// planBlockStandIn answers in Dispatch's place: version 1 of planDoc, on LEGION-208, holding one
// plan_task block whose id and attributes block writes from the bearer the read carried, and the
// document's own read, which names its issue.
func planBlockStandIn(t *testing.T, block func(bearer string) (id string, attributes map[string]string)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artifacts/"+planDoc {
			writeStandInJSON(t, w, map[string]any{"id": planDoc, "issue_key": "LEGION-208", "slug": "plan-md"})
			return
		}
		id, attributes := block(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		opening := ":::plan_task{#" + id
		answered := []map[string]string{}
		for name, value := range attributes {
			opening += " " + name + "=" + strconv.Quote(value)
			answered = append(answered, map[string]string{"name": name, "value": value, "kind": "string"})
		}
		markdown := opening + "}\nDo the change.\n:::"
		writeStandInJSON(t, w, map[string]any{"markdown": markdown, "number": 1, "blocks": []map[string]any{
			{"id": id, "type": "plan_task", "from": 0, "to": len(markdown), "attributes": answered},
		}})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func writeStandInJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("write the stand-in's answer: %v", err)
	}
}

// standInPlanHarness is planHarness with its Dispatch's plan reads made of standIn with bearer,
// and the grant of LEGION-208's architect.
func standInPlanHarness(t *testing.T, standIn, bearer string) (*harness, string) {
	t.Helper()
	h, facts, statuses := planHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{facts},
		Dispatch: standInPlanReads{statusRecorder: statuses, client: dispatch.New(standIn, dispatch.BearerTransport(bearer, nil))},
	}).Handler
	return h, newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect).grant(t)
}

// TestThePlanRouteAnswersNoWordOfAPlanBlockItRefuses: whatever answers in Dispatch's place answers
// the plan route's read with a plan_task block whose id is the bearer the read carried (id-bearer)
// or a run of it (id-run), whose id holds none of it but is outside the task-id grammar
// (id-grammar: it holds the gate's marker -RDX-, so the capture can show it), whose steps echo the
// bearer (steps-bearer) or quote a URL with made-up credentials in a word outside the step grammar
// (steps-grammar), or whose needs echo the bearer (needs-bearer) or quote such a URL (needs-url).
// register_plan's answer holds none of those words: the route took the plan and answered its id,
// or refused it with a reason quoting the id, the step or the needs. Each refusal names the block
// by its place and the rule, or the step by its place, or quotes each needs value that names no
// task through the URL rule (plan.Validate), and the control, a block of id a, is taken.
// The twin shows the stand-in's own answer holds each word. Captures api.plan-route-words/<case>.
func TestThePlanRouteAnswersNoWordOfAPlanBlockItRefuses(t *testing.T) {
	steps := func(attributes map[string]string) map[string]string {
		attributes["steps"] = "implementer reviewer"
		return attributes
	}
	const upper = "Task-RDX-Echo"
	const refused = "typed block 1 of the document, a plan_task block, "
	// probeURL is the URL the probe's words quote, with its made-up user, password and token: one
	// word, so a step or a needs value can be it.
	probeURL := oauth2calltest.Words[strings.LastIndex(oauth2calltest.Words, " ")+1:]
	cases := []struct {
		name, bearer, secret, want string
		block                      func(bearer string) (string, map[string]string)
	}{
		{name: "id-bearer", bearer: echoedBearer, secret: echoedBearer, want: refused + "has an id holding a run of the credential",
			block: func(bearer string) (string, map[string]string) { return bearer, steps(map[string]string{}) }},
		{name: "id-run", bearer: echoedBearer, secret: echoedBearer, want: refused + "has an id holding a run of the credential",
			block: func(bearer string) (string, map[string]string) { return bearer[5:23], steps(map[string]string{}) }},
		{name: "id-grammar", bearer: echoedBearer, secret: upper, want: refused + "has an id outside the task-id grammar [a-z][a-z0-9-]*",
			block: func(string) (string, map[string]string) { return upper, steps(map[string]string{}) }},
		{name: "steps-bearer", bearer: echoedBearer, secret: echoedBearer, want: refused + "has steps holding a run of the credential",
			block: func(bearer string) (string, map[string]string) {
				return "a", map[string]string{"steps": "implementer " + bearer}
			}},
		{name: "steps-grammar", bearer: plainBearer, secret: oauth2calltest.Token, want: "task a: the 2nd word of steps is neither a role nor a role:key",
			block: func(string) (string, map[string]string) {
				return "a", map[string]string{"steps": "implementer " + probeURL}
			}},
		{name: "needs-bearer", bearer: echoedBearer, secret: echoedBearer, want: refused + "has value 1 of its needs holding a run of the credential",
			block: func(bearer string) (string, map[string]string) {
				return "a", steps(map[string]string{"needs": "dispatch://LEGION-208/artifact/plan-md#b-" + bearer})
			}},
		{name: "needs-url", bearer: plainBearer, secret: oauth2calltest.Token, want: "task a needs " + redact.URLCredentials(probeURL) + ", which names no task of this plan.",
			block: func(string) (string, map[string]string) {
				return "a", steps(map[string]string{"needs": "dispatch://LEGION-208/artifact/plan-md#b-b " + probeURL})
			}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			request, err := http.NewRequest(http.MethodGet, planBlockStandIn(t, c.block)+"/api/v1/artifacts/"+planDoc+"/versions/1?blocks=true", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+c.bearer)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("read the stand-in's %s answer: %v", c.name, err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || oauth2calltest.Piece(string(body), c.secret) == "" {
				t.Errorf("the stand-in's %s answer holds no run of %q (%v), so the row cannot see a leak", c.name, c.secret, err)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, grant := standInPlanHarness(t, planBlockStandIn(t, c.block), c.bearer)

			answer := registerPlan(h, grant, "LEGION-208", planDoc, 1)

			oauth2calltest.RequireNoPiece(t, "api.plan-route-words/"+c.name, answer.Body.String(), c.secret)
			if answer.Code != http.StatusConflict || !strings.Contains(answer.Body.String(), c.want) {
				t.Errorf("register_plan = %d %s, want 409 PLAN_REFUSED saying %q", answer.Code, answer.Body, c.want)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		h, grant := standInPlanHarness(t, planBlockStandIn(t, func(string) (string, map[string]string) {
			return "a", steps(map[string]string{})
		}), echoedBearer)

		taken := registerPlan(h, grant, "LEGION-208", planDoc, 1)

		if want := `{"plan":{"version":1,"tasks":[{"id":"a","steps":["implementer","reviewer"]}]}}`; taken.Code != http.StatusOK || strings.TrimSpace(taken.Body.String()) != want {
			t.Fatalf("register_plan of a block of id a = %d %s, want 200 %s", taken.Code, taken.Body, want)
		}
	})
}
