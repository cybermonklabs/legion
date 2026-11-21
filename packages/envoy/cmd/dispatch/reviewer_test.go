package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The reviewer is built whenever its model group is set, whatever the kinds say, so the draft
// route answers; with the group unset it is nil, and a kind other than off then refuses the boot.
func TestTheReviewerIsBuiltWheneverItsModelGroupIsSet(t *testing.T) {
	pacer := modelpace.New(modelpace.Config{})
	unset, err := newReviewer(resolveWith(t, map[string]string{}).Writing, pacer)
	if err != nil || unset != nil {
		t.Fatalf("no group: reviewer %v (%v), want none", unset, err)
	}
	for _, settings := range []map[string]string{
		standInGroup(reviewerModel, "anthropic", nil),
		standInGroup(reviewerModel, "azure", map[string]string{"DISPATCH_REVIEWER_COMMENTS": "hold", "DISPATCH_REVIEWER_ASKS": "after"}),
		anthropicGroup(reviewerModel, nil),
	} {
		built, err := newReviewer(resolveWith(t, settings).Writing, pacer)
		if err != nil || built == nil {
			t.Fatalf("%v: reviewer %v (%v), want one", settings, built, err)
		}
	}
	refusedWith(t, map[string]string{"DISPATCH_REVIEWER_COMMENTS": "hold"}, "needs the reviewer's model group")
}

// The Dispatch-Test-Model header picks a test model by its whole name, as main builds the server:
// beside Kimi-K3.1-Pro, which sends no reasoning effort as an azure group does, a draft that names
// Kimi-K3.1-Pro:off calls the same deployment with reasoning_effort none, agentloop's word for off
// on Chat Completions. The reviewer's own group answers a draft with no header.
func TestTheTestModelHeaderPicksAVariantByItsWholeName(t *testing.T) {
	// A review on openai-chat sends its schema strict, which requires the rewrite, null for
	// none; Anthropic's protocol sends the schema that leaves it out.
	stand := modeltest.New(t,
		modeltest.Rule{Match: func(r modeltest.Request) bool { return r.Protocol == modeltest.Chat }, Reply: modeltest.Reply{Text: `{"findings":[],"rewrite":null}`}},
		modeltest.Rule{Reply: modeltest.Reply{Text: `{"findings":[]}`}})
	boot := resolveWith(t, standInGroup(reviewerModel, "anthropic", map[string]string{
		"DISPATCH_TEST_MODEL_URL":                      stand.URL,
		"DISPATCH_TEST_MODELS":                         "Kimi-K3.1-Pro=azure,Kimi-K3.1-Pro:off=azure",
		"DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE": "1000000",
	}))
	pacer := modelpace.New(modelpace.Config{Quotas: boot.Writing.DeploymentQuotas})
	textReviewer, err := newReviewer(boot.Writing, pacer)
	if err != nil {
		t.Fatal(err)
	}
	testModels, err := newTestModels(boot.Writing, pacer)
	if err != nil {
		t.Fatal(err)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{Reviewer: textReviewer, TestModels: testModels, Store: storetest.Open(t)})
	for _, c := range []struct {
		header, protocol, model string
		effort                  any
	}{
		{"Kimi-K3.1-Pro:off", modeltest.Chat, "Kimi-K3.1-Pro", "none"},
		{"Kimi-K3.1-Pro", modeltest.Chat, "Kimi-K3.1-Pro", nil},
		{"", modeltest.Anthropic, "stand-in-reviewer", nil},
	} {
		before := len(stand.Requests())
		request := httptest.NewRequest(http.MethodPost, "/api/v1/reviewer-drafts", strings.NewReader(`{"kind":"comment","text":"Status\nThe change lands today."}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer agent-token")
		if c.header != "" {
			request.Header.Set(api.TestModelHeader, c.header)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %q: POST /api/v1/reviewer-drafts answered %d %s, want 200", api.TestModelHeader, c.header, response.Code, response.Body.String())
		}
		calls := stand.Requests()[before:]
		if len(calls) != 1 {
			t.Fatalf("%s %q: the stand-in got %d requests, want the review's one", api.TestModelHeader, c.header, len(calls))
		}
		var body map[string]any
		if err := json.Unmarshal(calls[0].Body, &body); err != nil {
			t.Fatal(err)
		}
		if calls[0].Protocol != c.protocol || calls[0].Model != c.model || body["reasoning_effort"] != c.effort {
			t.Errorf("%s %q: the review called %s %q with reasoning_effort %v, want %s %q with %v",
				api.TestModelHeader, c.header, calls[0].Protocol, calls[0].Model, body["reasoning_effort"], c.protocol, c.model, c.effort)
		}
	}
}
