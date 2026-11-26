package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The labels of one ask's options, each holding markdown of its own, so a tree read from one
// label is never the tree of another and a list in the wrong order fails.
const (
	chosenBrass  = "The **brass** sextant"
	chosenCopper = "The `copper` astrolabe"
	chosenBronze = "The *bronze* quadrant"
	// renamedBronze is what the ask's third option is called once the answer that chose
	// chosenBronze has been replaced: that earlier answer names an option the ask no longer
	// holds.
	renamedBronze = "The ~~bronze~~ iron quadrant"
)

// chosenLabelAsk is an ask answered twice: first with chosenBronze, then with chosenBrass and
// chosenCopper, after which its bronze option is renamed. It answers the issue's key and the
// ask's id.
func chosenLabelAsk(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "CHSN", "Navigation instruments", "# Spec\n\nThe plan.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which navigation instrument takes the altitude?",
		"multiple": true,
		"options": []map[string]string{
			{"label": chosenBrass}, {"label": chosenCopper}, {"label": chosenBronze},
		},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[model.Ask](t, created).ID
	first := answerSearchAsk(t, handler, askID, map[string]any{"selected": []string{chosenBronze}})
	answerSearchAsk(t, handler, askID, map[string]any{
		"selected":           []string{chosenBrass, chosenCopper},
		"text":               "Both, **checked** against each other.",
		"expected_answer_at": timestampValue(first.At),
	})
	// An answered ask cannot be reworded through the API, while a document's ask block can
	// change under its answers; the row is renamed in place to stand for that.
	if _, err := database.Pool.Exec(context.Background(),
		`update asks set options = jsonb_set(options, '{2,label}', to_jsonb($2::text)) where id = $1`,
		askID, renamedBronze,
	); err != nil {
		t.Fatalf("rename the bronze option: %v", err)
	}
	return handler, issue.Key, askID
}

// Every read that answers a chosen option's label to the dashboard answers, beside the answer's
// `selected`, a `selected_trees` list holding the tree of the label at the same index: the
// tree pmdoc.ReadForDisplay makes of exactly that label, present under `Dispatch-Trees: 1` and
// absent without it. The reads are the caller's answer history (the Answers page), one ask with
// its thread of answers (an ask card's earlier answers, one of which chose an option the ask no
// longer holds), an issue's ask list, and a search hit's ask (the palette's `Chose …` line). The
// route table's own 200 schema declares each list where the answer carries it.
func TestEveryReadOfAnAnswerCarriesTheTreeOfEachLabelItChose(t *testing.T) {
	t.Parallel()
	handler, issueKey, askID := chosenLabelAsk(t)
	node, _ := readOpenAPITree(t)
	document, _ := node.(map[string]any)

	for _, test := range []struct{ name, pattern, target, answer string }{
		// Newest first: the second answer, then the first.
		{"the caller's answer history, the current answer", "/api/v1/me/answers", "/api/v1/me/answers",
			"rows.0.answer"},
		{"the caller's answer history, an answer that chose a renamed option", "/api/v1/me/answers",
			"/api/v1/me/answers", "rows.1.answer"},
		// Oldest first: the earlier answer, whose option the ask no longer holds, then the current.
		{"one ask's earlier answer", "/api/v1/asks/{id}", "/api/v1/asks/" + askID, "answers.0"},
		{"one ask's current answer in its thread", "/api/v1/asks/{id}", "/api/v1/asks/" + askID, "answers.1"},
		{"one ask's own answer", "/api/v1/asks/{id}", "/api/v1/asks/" + askID, "ask.answer"},
		{"the issue's ask list", "/api/v1/issues/{key}/asks", "/api/v1/issues/" + issueKey + "/asks",
			"0.answer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireChosenLabelTrees(t, handler, test.target, test.answer)
			requireTreeInAnswerSchema(t, document, test.pattern, test.answer+".selected_trees.0")
		})
	}

	t.Run("a search hit's ask", func(t *testing.T) {
		target := "/api/v1/search?q=" + "altitude&limit=50"
		hits := rawSearchResults(t, treesRequest(t, handler, target, false))
		at := -1
		for index, hit := range hits {
			if rawString(t, hit["kind"]) == "ask" && rawString(t, hit["id"]) == askID {
				at = index
			}
		}
		if at < 0 {
			t.Fatalf("GET %s answered no hit of ask %s: %v", target, askID, hits)
		}
		requireChosenLabelTrees(t, handler, target, "results."+strconv.Itoa(at)+".ask.answer")
		requireTreeInAnswerSchema(t, document, "/api/v1/search", "results.0.ask.answer.selected_trees.0")
	})
}

// requireChosenLabelTrees states that the answer standing at answerPath of target's body carries,
// under the header and only there, the tree of each label it chose at that label's own index,
// and no tree beyond its labels. The labels are read from the answer itself.
func requireChosenLabelTrees(t *testing.T, handler http.Handler, target, answerPath string) {
	t.Helper()
	raw, held := valueAt(t, treesRequest(t, handler, target, false), answerPath+".selected")
	if !held {
		t.Fatalf("GET %s carries no %s.selected", target, answerPath)
	}
	var labels []string
	if err := json.Unmarshal(raw, &labels); err != nil || len(labels) == 0 {
		t.Fatalf("GET %s: %s.selected is %s, want the labels the answer chose", target, answerPath, raw)
	}
	for index := range labels {
		at := strconv.Itoa(index)
		requireDisplayTree(t, handler, target, answerPath+".selected."+at, answerPath+".selected_trees."+at)
	}
	trees, _ := valueAt(t, treesRequest(t, handler, target, true), answerPath+".selected_trees")
	var list []json.RawMessage
	if err := json.Unmarshal(trees, &list); err != nil || len(list) != len(labels) {
		t.Errorf("GET %s: %s.selected_trees is %s, want one tree for each of %q", target, answerPath, trees, labels)
	}
}

// An ask.answered event that replaced an answer carries the tree of each label the replaced
// answer chose in its trees map, under `payload.previous_answer.selected.<n>`: the Activity tab's
// `was:` line draws them, and one of them names an option the ask no longer holds, so no option
// in the payload stands for it. The REST log carries them under the header alone, and a live
// frame's own reading of the same payload answers the same keys.
func TestAReplacedAnswersEventCarriesTheTreeOfEachLabelItChose(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "CHEV", "Navigation instruments", "# Spec\n\nThe plan.\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which navigation instrument takes the altitude?",
		"multiple": true,
		"options":  []map[string]string{{"label": chosenBrass}, {"label": chosenCopper}, {"label": chosenBronze}},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	askID := decodeBody[model.Ask](t, created).ID
	first := answerSearchAsk(t, handler, askID, map[string]any{"selected": []string{chosenBronze, chosenCopper}})
	answerSearchAsk(t, handler, askID, map[string]any{
		"selected": []string{chosenBrass}, "expected_answer_at": timestampValue(first.At),
	})
	if _, err := database.Pool.Exec(context.Background(),
		`update asks set options = jsonb_set(options, '{2,label}', to_jsonb($2::text)) where id = $1`,
		askID, renamedBronze,
	); err != nil {
		t.Fatalf("rename the bronze option: %v", err)
	}
	want := map[string]json.RawMessage{
		"payload.previous_answer.selected.0": pmdoc.DisplayJSON(chosenBronze, pmdoc.DisplayLatest),
		"payload.previous_answer.selected.1": pmdoc.DisplayJSON(chosenCopper, pmdoc.DisplayLatest),
	}

	target := "/api/v1/issues/" + issue.Key + "/events"
	if bare := treesRequest(t, handler, target, false); strings.Contains(bare.Body.String(), `"trees"`) {
		t.Errorf("GET %s without the header carries trees: %s", target, bare.Body.String())
	}
	var events []model.Event
	if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &events); err != nil {
		t.Fatalf("decode the event log: %v", err)
	}
	replaced := 0
	for _, event := range events {
		var payload struct {
			PreviousAnswer *model.AskAnswer `json:"previous_answer"`
		}
		if event.Type != "ask.answered" || json.Unmarshal(eventPayloadJSON(event), &payload) != nil ||
			payload.PreviousAnswer == nil {
			continue
		}
		replaced++
		requireChosenLabelEventTrees(t, "the event log", event.Trees, want)
		// The live frame reads the payload afresh, so it is handed the event without the map the
		// log's read filled: the two share that map, and a reading that answered nothing would
		// otherwise pass on the log's trees.
		live := event
		live.Trees = nil
		requireChosenLabelEventTrees(t, "a live frame", eventDisplayTrees(live), want)
	}
	if replaced != 1 {
		t.Fatalf("the event log carried %d ask.answered events that replaced an answer, want 1", replaced)
	}
}

// requireChosenLabelEventTrees states that trees holds want's tree under each of want's keys, and
// no tree of a label beyond them: the current answer's labels are drawn from their options.
func requireChosenLabelEventTrees(t *testing.T, what string, trees, want map[string]json.RawMessage) {
	t.Helper()
	for path, tree := range want {
		if !bytes.Equal(trees[path], tree) {
			t.Errorf("%s: trees[%s] = %s, want %s", what, path, trees[path], tree)
		}
	}
	for path := range trees {
		if _, wanted := want[path]; !wanted && strings.Contains(path, ".selected.") {
			t.Errorf("%s: trees[%s] is the tree of a label no draw reads from the payload", what, path)
		}
	}
}
