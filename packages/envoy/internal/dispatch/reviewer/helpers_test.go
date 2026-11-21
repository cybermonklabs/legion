package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// standInToken is the credential every call to the stand-in model presents, which reads none.
func standInToken(context.Context) (string, error) { return "stand-in", nil }

// chatModel is a model group of kind azure at url: a Chat Completions deployment.
func chatModel(url, deployment string) Model {
	return Model{
		Model:   agentloop.Model{Endpoint: agentloop.Endpoint{Kind: agentloop.OpenAIChat, BaseURL: url, Token: standInToken}, ID: deployment, Effort: "off"},
		Timeout: 20 * time.Second,
	}
}

// anthropicModel is a model group of kind anthropic at url: an Anthropic Messages service.
func anthropicModel(url, id string) Model {
	return Model{
		Model:   agentloop.Model{Endpoint: agentloop.Endpoint{Kind: agentloop.AnthropicMessages, BaseURL: url, Token: standInToken}, ID: id, Effort: "off"},
		Timeout: 20 * time.Second,
	}
}

// asJSON is v as the JSON text a stand-in model answers with.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The markers of the prompts' own text, by which a stand-in's rule tells the calls apart.
const (
	reviewMarker   = "You review a text for its reader."
	rewriteMarker  = "You write a text again for its reader."
	defineMarker   = "You define words for a reader."
	detectorMarker = "decide if its verb phrase is vacuous"
)

// noFindings is a review's reply that found nothing as strict mode writes it on openai-chat:
// every property of the schema, the rewrite null.
const noFindings = `{"findings":[],"rewrite":null}`

// askRecord is the record of an ask with two options, a thread, the document it stands in, the
// issue's decisions document, the writer's texts and the words its reader asked about before. Its
// options carry no id, as the ask's row holds them (AskSubject).
func askRecord() Record {
	return Record{
		Text: Text{
			Kind: KindAsk,
			Body: "Finding from tester\nWe spin up the node. This names the problem.\nRef: dispatch://PRJ-1/comment/c1",
			Options: []model.AskOption{
				{Label: "Ship", Description: "Release it today"},
				{Label: "Hold", Description: "Wait for the store"},
			},
		},
		Thread: []Entry{{Kind: KindComment, Author: "person sami", At: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), Text: "What is a store?"}},
		Document: &Document{
			Name: "spec", Holds: true, BlockID: "blk",
			Markdown: "# Spec\n\nThe store keeps rows.\n\n:::ask{#blk urgency=\"med\" multiple=\"false\"}\nWhich store?\n\n- A: one\n:::\n",
		},
		IssueKey: "PRJ-1", IssueTitle: "Ship the reviewer",
		Decisions:   "1. We keep one store.",
		WriterTexts: []Entry{{Kind: KindComment, Author: authorWriter, At: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), Text: "I moved the store."}},
		Reader:      "sami",
		ReaderWords: []string{"store"},
	}
}

// servedAs is a proxy in front of the stand-in that answers each reply as served by served: it
// writes served in place of the model the request named, as an Azure deployment or a gateway
// alias answers with the model behind it.
func servedAs(t *testing.T, stand *modeltest.Model, named, served string) string {
	t.Helper()
	target, err := url.Parse(stand.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request, _ := http.NewRequestWithContext(r.Context(), r.Method, target.JoinPath(r.URL.Path).String()+"?"+r.URL.RawQuery, bytes.NewReader(body))
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		answer, _ := io.ReadAll(response.Body)
		for key, values := range response.Header {
			if key != "Content-Length" {
				w.Header()[key] = values
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(bytes.ReplaceAll(answer, []byte(`"model":"`+named+`"`), []byte(`"model":"`+served+`"`)))
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// requestsHolding are the requests the stand-in got whose text holds marker.
func requestsHolding(stand *modeltest.Model, marker string) []modeltest.Request {
	var held []modeltest.Request
	for _, request := range stand.Requests() {
		if strings.Contains(request.Text, marker) {
			held = append(held, request)
		}
	}
	return held
}
