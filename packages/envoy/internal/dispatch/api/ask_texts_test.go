package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// The stored texts of one ask carrying every kind of them, each under the path an event's trees
// map is keyed by. It is written out rather than walked, because these keys are what the
// dashboard reads a payload's tree by: a change to one of them is a change every reader of the
// feed sees, and the tests below hold the three paths over an ask's texts to it.
var askTextsByPath = map[string]string{
	"payload.question":                       "Ship the **release** today?",
	"payload.options.0.label":                "Yes, `ship it`",
	"payload.options.0.description":          "The rollback is *rehearsed*.",
	"payload.options.1.label":                "No, hold it",
	"payload.options.1.description":          "Wait for the [rehearsal](https://example.test).",
	"payload.answer.text":                    "Shipping, with the **flag** off.",
	"payload.resolution.reason":              "Answered in the *standup* instead.",
	"payload.previous.question":              "Ship the ~~draft~~ release today?",
	"payload.previous.options.0.label":       "Yes, ship the draft",
	"payload.previous.options.0.description": "The draft is **signed off**.",
	"payload.previous.options.1.label":       "No, hold the draft",
	"payload.previous.options.1.description": "Hold it for the `rehearsal`.",
}

// askWithEveryText is an ask holding every stored text an ask can hold, and the content one edit
// carried away: the pair askTextsByPath names.
func askWithEveryText() (model.Ask, model.AskEditPrevious) {
	answer := askTextsByPath["payload.answer.text"]
	ask := model.Ask{
		ID:       "11111111-1111-4111-8111-111111111111",
		Kind:     "question",
		State:    "resolved",
		Question: askTextsByPath["payload.question"],
		Options: []model.AskOption{
			{
				Label:       askTextsByPath["payload.options.0.label"],
				Description: askTextsByPath["payload.options.0.description"],
			},
			{
				Label:       askTextsByPath["payload.options.1.label"],
				Description: askTextsByPath["payload.options.1.description"],
			},
		},
		Answer:     &model.AskAnswer{User: "alice", Selected: []string{}, Text: &answer},
		Resolution: &model.AskResolution{Kind: "withdrawn", Reason: askTextsByPath["payload.resolution.reason"]},
	}
	previous := model.AskEditPrevious{
		Question: askTextsByPath["payload.previous.question"],
		Options: []model.AskOption{
			{
				Label:       askTextsByPath["payload.previous.options.0.label"],
				Description: askTextsByPath["payload.previous.options.0.description"],
			},
			{
				Label:       askTextsByPath["payload.previous.options.1.label"],
				Description: askTextsByPath["payload.previous.options.1.description"],
			},
		},
	}
	return ask, previous
}

// The one walk of an ask's stored texts answers exactly the texts askTextsByPath names, each
// under its own path. Every path that touches an ask's texts makes this walk, so a text added to
// it and missed by one of them is caught by the two tests below rather than costing a parse on
// every read or storing a tree no reader takes.
func TestTheWalkOverAnAsksTextsAnswersEveryOneOfThem(t *testing.T) {
	ask, previous := askWithEveryText()
	walked := map[string]string{}
	texttrees.VisitAskTexts(texttrees.AskContentOf(ask), func(text texttrees.AskText) {
		walked[text.Path(eventPayloadPrefix)] = text.Text
	})
	texttrees.VisitAskTexts(texttrees.AskEditPreviousContent(previous), func(text texttrees.AskText) {
		walked[text.Path(eventPayloadPrefix+".previous")] = text.Text
	})
	for path, want := range askTextsByPath {
		if walked[path] != want {
			t.Errorf("the walk answered %q at %q, want %q", walked[path], path, want)
		}
	}
	for path := range walked {
		if _, named := askTextsByPath[path]; !named {
			t.Errorf("the walk answered a text at %q, which no path of an ask's texts names", path)
		}
	}
	// And the texts alone are what the background fill stores the trees of, in the same walk.
	texts := texttrees.AppendAskTexts(nil, texttrees.AskContentOf(ask))
	texts = texttrees.AppendAskTexts(texts, texttrees.AskEditPreviousContent(previous))
	slices.Sort(texts)
	want := slices.Sorted(maps.Values(askTextsByPath))
	if !slices.Equal(texts, want) {
		t.Errorf("the fill's texts are %q, want %q", texts, want)
	}
}

// An ask event's payload carries the tree of each of those texts under that same path: the key
// the dashboard reads it by. The payload is the one a write publishes, read as the event list and
// a live frame read it.
func TestAnAskEventCarriesEveryAskTextUnderItsPath(t *testing.T) {
	ask, previous := askWithEveryText()
	trees := eventDisplayTrees(model.Event{
		Type:    "ask.edited",
		Payload: model.NewAskEditEventPayload(ask, previous, model.Actor{}, model.ReferenceChanges{}),
	})
	requireTreesOfAskTexts(t, "the event's trees map", trees)
}

// The ask read shapes register exactly those texts with the request's reader, each in the field
// the dashboard reads it from. The fields are found in the shaped JSON rather than named here,
// because a read names a text's tree `<field>_tree` beside the field itself: the read's
// `options.0.label_tree` is the event's `payload.options.0.label`, and a text the shapes
// registered nowhere has no such member at all.
func TestTheAskReadShapesRegisterEveryAskText(t *testing.T) {
	ask, previous := askWithEveryText()
	reader := builtDisplayTrees(true)
	asks := shapedAsks([]model.Ask{ask}, reader)
	edits := askEditReads([]model.AskEdit{{Previous: previous}}, reader)
	reader.resolve()
	trees := map[string]json.RawMessage{}
	collectTreeFields(t, marshalRead(t, asks[0]), eventPayloadPrefix, trees)
	collectTreeFields(t, marshalRead(t, edits[0]), eventPayloadPrefix, trees)
	requireTreesOfAskTexts(t, "the ask read shapes", trees)
}

// requireTreesOfAskTexts states that trees holds the tree of every text askTextsByPath names,
// under that text's path, and nothing else. The tree wanted is the reading a request's reader
// makes of a text the table held no row for (builtDisplayTree), which is what every one of the
// three paths above answers.
func requireTreesOfAskTexts(t *testing.T, what string, trees map[string]json.RawMessage) {
	t.Helper()
	for path, text := range askTextsByPath {
		tree, held := trees[path]
		if !held {
			t.Errorf("%s holds no tree at %q, the path of %q", what, path, text)
			continue
		}
		if want := builtDisplayTree(pmdoc.TextSum(text), text, pmdoc.DisplayLatest); !bytes.Equal(tree, want) {
			t.Errorf("%s holds %s at %q, want the tree of %q, %s", what, tree, path, text, want)
		}
	}
	for path := range trees {
		if _, named := askTextsByPath[path]; !named {
			t.Errorf("%s holds a tree at %q, which is no path of an ask's texts", what, path)
		}
	}
}

// marshalRead is the JSON one shaped read answers with.
func marshalRead(t *testing.T, read any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("encode a shaped read: %v", err)
	}
	return encoded
}

// collectTreeFields puts every `<field>_tree` member raw holds, however deep, into trees under
// the path of the field it is the tree of, rooted at prefix. A member a shape left out - the
// tree of a text nothing registered - is simply not there.
func collectTreeFields(t *testing.T, raw json.RawMessage, prefix string, trees map[string]json.RawMessage) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err == nil {
		for name, value := range members {
			if field, isTree := strings.CutSuffix(name, "_tree"); isTree {
				if string(value) != "null" {
					trees[prefix+"."+field] = value
				}
				continue
			}
			collectTreeFields(t, value, prefix+"."+name, trees)
		}
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return
	}
	for index, item := range items {
		collectTreeFields(t, item, prefix+"."+strconv.Itoa(index), trees)
	}
}
