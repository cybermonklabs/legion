package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// keptAskQuestion and keptAskAnswer are the question of the ask TestAnAgentCannotDropAnAnsweredAsk
// answers and the words its person answers it with.
const (
	keptAskQuestion = "Which name do we ship?"
	keptAskAnswer   = "Ship the name we have."
	keptAskContext  = "Context.\n"
)

// R6 of dispatch://INFRA-1150, as the middle paragraph of c-answers-restored states it: no agent
// takes a person's answered decision out of a spec or moves it. Each of an agent's three writes -
// an edit that drops the ask's block, a whole-document replacement that leaves the block out, and
// a retype of the block - is refused 409 ANSWERED_ASK_KEPT, quoting the ask's question and naming
// its node and its block's address. The document's version is unchanged, and the answer is still in
// the spec when it is read back. The same three writes by the person are taken, whether they are
// made at a browser or through the person's own token, which holds the person scope that the
// refusal reads (docs.WithPersonScope).
func TestAnAgentCannotDropAnAnsweredAsk(t *testing.T) {
	handler, database := blockAskHandler(t)
	personalToken := personalTokenHolding(t, database, model.ScopePerson)
	writes := []struct {
		name string
		// path is the route the write goes to, on the issue and its spec.
		path func(issueKey, artifactID string) string
		body map[string]any
		// refusal is what the refusal says after its ask's node and question, %s being the block's
		// address.
		refusal string
		// taken is the status the person's write is answered with, and holds what the spec holds
		// once it is.
		taken int
		holds string
	}{
		{
			name:    "an edit that drops the block",
			path:    func(_, artifactID string) string { return "/api/v1/artifacts/" + artifactID + "/edits" },
			body:    map[string]any{"ops": []map[string]any{{"op": "delete", "block": "chosen"}}},
			refusal: "an agent cannot remove its ask block %s from this document",
			taken:   http.StatusOK,
			holds:   keptAskContext,
		},
		{
			name:    "a whole-document replacement that leaves the block out",
			path:    func(issueKey, _ string) string { return "/api/v1/issues/" + issueKey + "/artifacts" },
			body:    map[string]any{"name": "spec.md", "content": keptAskContext},
			refusal: "an agent cannot remove its ask block %s from this document",
			taken:   http.StatusCreated,
			holds:   keptAskContext,
		},
		{
			name: "a retype of the block",
			path: func(_, artifactID string) string { return "/api/v1/artifacts/" + artifactID + "/edits" },
			body: map[string]any{"ops": []map[string]any{{
				"op": "retype", "block": "chosen", "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Old choice"},
			}}},
			refusal: "an agent cannot retype its ask block %s to callout",
			taken:   http.StatusOK,
			holds:   `:::callout{#chosen kind="note" title="Old choice"}`,
		},
	}
	people := []struct {
		name string
		send func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder
	}{
		{"at a browser", func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
		}},
		{"through their own token", func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, http.MethodPost, path, body, personalToken)
		}},
	}
	index := 0
	for _, write := range writes {
		for _, person := range people {
			project := fmt.Sprintf("KP%c", 'A'+index)
			index++
			t.Run(write.name+" "+person.name, func(t *testing.T) {
				issue := createInteractionIssue(t, handler, project, "An answered decision", keptAskContext+"\n"+
					":::ask{#chosen urgency=\"high\" multiple=\"false\"}\n"+keptAskQuestion+"\n\n- Keep: The name we have.\n- Change: A new one.\n:::\n")
				askID := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"chosen": keptAskQuestion})["chosen"]
				answerDecisionBlock(t, handler, askID, "alice", []string{"Keep"}, keptAskAnswer, nil)
				path := write.path(issue.Key, issue.PrimaryArtifactID)
				body := map[string]any{"actor": sessionActor()}
				for name, value := range write.body {
					body[name] = value
				}

				before := documentMarkdown(t, handler, issue.PrimaryArtifactID)
				requireAnswerInSpec(t, "before the agent's "+write.name, before)
				versions := len(documentVersions(t, handler, issue.PrimaryArtifactID))
				refused := sessionRequest(t, handler, http.MethodPost, path, body)
				if refused.Code != http.StatusConflict {
					t.Fatalf("the agent's %s was not refused: status=%d body=%s, want 409 %s", write.name, refused.Code, refused.Body.String(), codeAnsweredAskKept)
				}
				want := fmt.Sprintf("ask dispatch://%s/ask/%s, asking %q, is answered; ", issue.Key, askID, keptAskQuestion) +
					fmt.Sprintf(write.refusal, "dispatch://"+issue.Key+"/spec#b-chosen")
				if refusal := decodeBody[errorBody](t, refused); refusal.Code != codeAnsweredAskKept || refusal.Error != want {
					t.Fatalf("the agent's %s was refused %#v, want %s saying\n%s", write.name, refusal, codeAnsweredAskKept, want)
				}
				if after := len(documentVersions(t, handler, issue.PrimaryArtifactID)); after != versions {
					t.Fatalf("the agent's refused %s left the document %d versions, want the %d it had", write.name, after, versions)
				}
				after := documentMarkdown(t, handler, issue.PrimaryArtifactID)
				if after != before {
					t.Fatalf("the agent's refused %s changed the spec to %q, want it as it was, %q", write.name, after, before)
				}
				requireAnswerInSpec(t, "after the agent's refused "+write.name, after)
				if ask := readBlockAsk(t, handler, askID); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
					t.Fatalf("the ask after the agent's refused %s = state %q answer %#v, want alice's answer", write.name, ask.State, ask.Answer)
				}

				taken := person.send(t, path, body)
				if taken.Code != write.taken {
					t.Fatalf("the person's %s %s: status=%d body=%s, want %d", write.name, person.name, taken.Code, taken.Body.String(), write.taken)
				}
				spec := documentMarkdown(t, handler, issue.PrimaryArtifactID)
				if strings.Contains(spec, ":::ask{#chosen") || !strings.Contains(spec, write.holds) {
					t.Fatalf("the spec after the person's %s %s = %q, want no ask block chosen and %q", write.name, person.name, spec, write.holds)
				}
			})
		}
	}
}

// requireAnswerInSpec fails t unless the spec, as GET .../text reads it, holds the person's
// answer on its ask block.
func requireAnswerInSpec(t *testing.T, when, spec string) {
	t.Helper()
	for _, want := range []string{`:::ask{#chosen `, `state="answered"`, `answered_by="alice"`, `answer="` + keptAskAnswer + `"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("the spec %s = %q, want the answer on its ask block, holding %s", when, spec, want)
		}
	}
}
