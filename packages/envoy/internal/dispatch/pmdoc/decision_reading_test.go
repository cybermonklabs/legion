package pmdoc

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// An `ask` attribute names one ask of an issue, written whole as every writer writes its address,
// and never a part of one: `from_ask` points a decision at the ask it settles, which a part of the
// ask's text is not.
func TestAnAskAttributeNamesOneAskOfAnIssue(t *testing.T) {
	attr := BlockAttributeSchema{Kind: BlockAttributeAsk}
	const ask = "dispatch://CORE-1/ask/0b6f4c0e-3d1a-4f5e-9a7b-2c8d1e0f3a4b"
	if err := validateAttributeValue("from_ask", attr, ask); err != nil {
		t.Fatalf("an ask of an issue %q is refused: %v", ask, err)
	}
	for value, reason := range map[string]string{
		ask + "#b-q1":                       "is a part of an ask",
		ask + "#b-q1@v2":                    "is a part of an ask",
		"dispatch://CORE-1/spec#b-x":        "is not an ask of an issue",
		"dispatch://CORE-1":                 "is not an ask of an issue",
		"dispatch://CORE/artifact/h/ask/a1": "is not an ask of an issue",
		ask + ".":                           "is not an ask of an issue",
		"see " + ask:                        "is not an ask of an issue",
		"":                                  "is not an ask of an issue",
	} {
		err := validateAttributeValue("from_ask", attr, value)
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("the ask attribute value %q = %v, want it refused as one that %s", value, err, reason)
		}
	}
	if err := validateAttributeValue("from_ask", attr, true); err == nil {
		t.Errorf("an ask attribute holding a bool is accepted")
	}

	written := ":::decision{#d1 from_ask=\"" + ask + "\"}\nWe ship on Friday.\n:::\n"
	doc, err := ParseForWrite(written, nil)
	if err != nil {
		t.Fatalf("a decision naming the ask it settles is refused: %v", err)
	}
	if got := doc.Children[0].Attrs["from_ask"]; got != ask {
		t.Fatalf("the decision's from_ask = %#v, want %q", got, ask)
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render the decision: %v", err)
	}
	if !strings.Contains(rendered, `from_ask="`+ask+`"`) {
		t.Fatalf("the decision renders as %q, want its from_ask written back", rendered)
	}
	_, err = ParseForWrite(":::decision{#d1 from_ask=\""+ask+"#b-q1\"}\nWe ship on Friday.\n:::\n", nil)
	if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "is a part of an ask") {
		t.Fatalf("a decision naming a part of an ask = %v, want the part refused", err)
	}
}

// An attribute whose `markdown` field is false lives on the block's node alone: the renderer
// never writes it, and markdown that names it is read as if it did not. The rule is the field's,
// not a name's: the renderer used to leave out `invalid` by its name.
func TestAnAttributeMarkdownNeverCarriesIsNeitherWrittenNorRead(t *testing.T) {
	never := []string{"decided_by", "kept_by", "kept_text_sum"}
	doc, err := ParseForWrite(":::decision{#d1 uncertainty=\"low\"}\nWe ship on Friday.\n:::\n", nil)
	if err != nil {
		t.Fatalf("parse a decision: %v", err)
	}
	decision := doc.Children[0]
	for _, name := range never {
		decision.Attrs[name] = "user:alice"
	}
	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render a decision holding the attributes markdown never carries: %v", err)
	}
	for _, name := range never {
		if strings.Contains(rendered, name) {
			t.Errorf("the decision renders as %q, which writes %s", rendered, name)
		}
	}
	if !strings.Contains(rendered, `uncertainty="low"`) {
		t.Errorf("the decision renders as %q, want its uncertainty written", rendered)
	}

	claimed := ":::decision{#d1 decided_by=\"user:mallory\" kept_by=\"user:mallory\" kept_text_sum=\"forged\"}\nWe ship on Friday.\n:::\n" +
		"\n:::ask{#a1 invalid=\"forged\"}\nWhich day?\n:::\n"
	parsed, err := ParseForWrite(claimed, nil)
	if err != nil {
		t.Fatalf("markdown naming attributes markdown never carries is refused: %v", err)
	}
	for _, name := range never {
		if value, held := parsed.Children[0].Attrs[name]; held {
			t.Errorf("markdown claiming %s gives the decision %s = %#v, want none", name, name, value)
		}
	}
	if value, held := parsed.Children[1].Attrs["invalid"]; held {
		t.Errorf("markdown claiming invalid gives the ask invalid = %#v, want none", value)
	}

	value := blockPointerTestSchema(BlockAttributes{
		{Name: "invalid", BlockAttributeSchema: BlockAttributeSchema{Kind: BlockAttributeString, Server: true}},
		{Name: "note", BlockAttributeSchema: BlockAttributeSchema{Kind: BlockAttributeString, Markdown: new(false)}},
	})
	useTestBlockSchema(t, value)
	claim, err := Parse(":::claim{#c1 invalid=\"kept\" note=\"dropped\"}\nA claim.\n:::\n")
	if err != nil {
		t.Fatalf("parse a claim: %v", err)
	}
	if got, held := claim.Children[0].Attrs["note"]; held {
		t.Errorf("the claim's note = %#v, want it dropped since markdown never carries it", got)
	}
	claim.Children[0].Attrs["note"] = "on the node"
	rendered, err = Render(claim)
	if err != nil {
		t.Fatalf("render a claim: %v", err)
	}
	if !strings.Contains(rendered, `invalid="kept"`) || strings.Contains(rendered, "note") {
		t.Errorf("the claim renders as %q, want invalid written, since markdown carries it, and note left out", rendered)
	}
}

// A reading stands only where a type its parents name admits it: at the end of a decision or an
// ask, after that block's own content. It is in no node group, so the document, a quote, a list
// item, a callout and every other block's content refuse it, and so does a parent that holds it
// anywhere but at its end.
func TestAReadingStandsOnlyAfterTheContentOfItsParent(t *testing.T) {
	reading := ":::reading{#r1}\nThe agent reads it as Friday.\n:::\n"
	for name, markdown := range map[string]string{
		"after a decision's content": "::::decision{#d1}\nWe ship on Friday.\n\n" + reading + "::::\n",
		"after an ask's question":    "::::ask{#a1}\nWhich day?\n\n" + reading + "::::\n",
		"after an ask's options":     "::::ask{#a1}\nWhich day?\n\n- Friday\n- Monday\n\n" + reading + "::::\n",
	} {
		doc, err := ParseForWrite(markdown, nil)
		if err != nil {
			t.Errorf("%s: %v, want the reading admitted", name, err)
			continue
		}
		parent := doc.Children[0]
		content, admitted := TypedContent(parent)
		if len(admitted) != 1 || admitted[0].Type != "reading" || len(content) == 0 || slices.ContainsFunc(content, func(child *Node) bool { return child.Type == "reading" }) {
			t.Errorf("%s: the %s holds content %v and admits %v, want its content and then the reading", name, parent.Type, BlockNames(content), BlockNames(admitted))
		}
		if err := AskContentError(doc); err != nil {
			t.Errorf("%s: the ask's content rule refuses it: %v", name, err)
		}
		rendered, err := Render(doc)
		if err != nil {
			t.Errorf("%s: render: %v", name, err)
			continue
		}
		if back, err := Parse(rendered); err != nil || !back.Equal(doc) {
			t.Errorf("%s: the rendering %q reads back as %v (%v), want the document it was rendered from", name, rendered, back, err)
		}
	}
	for name, markdown := range map[string]string{
		"at the document's level":       reading,
		"in a quote":                    "> " + strings.ReplaceAll(strings.TrimSuffix(reading, "\n"), "\n", "\n> ") + "\n",
		"in a list item":                "- An item.\n\n  " + strings.ReplaceAll(strings.TrimSuffix(reading, "\n"), "\n", "\n  ") + "\n",
		"in a callout":                  "::::callout{#c1}\nA note.\n\n" + reading + "::::\n",
		"in a requirement":              "::::requirement{#q1}\nA requirement.\n\n" + reading + "::::\n",
		"in a reading":                  ":::::decision{#d1}\nWe ship.\n\n::::reading{#r0}\nA reading.\n\n" + reading + "::::\n:::::\n",
		"before the rest of a decision": "::::decision{#d1}\nWe ship on Friday.\n\n" + reading + "\nAnd say so.\n::::\n",
	} {
		_, err := ParseForWrite(markdown, nil)
		if err == nil {
			t.Errorf("%s: the reading is admitted, want it refused", name)
			continue
		}
		if !errors.Is(err, ErrSchema) {
			t.Errorf("%s: %v, want a schema refusal", name, err)
		}
		if !strings.Contains(err.Error(), "reading") {
			t.Errorf("%s: %v, want the refusal to name the reading", name, err)
		}
	}
	// An ask is kept whatever order a browser leaves its blocks and readings in, for settlement to
	// flag (validateTypedBlock), so markdown written in whole holding a reading before the ask's
	// options is refused by the ask's content rule, which names the reading.
	midAsk, err := ParseForWrite("::::ask{#a1}\nWhich day?\n\n"+reading+"\n- Friday\n- Monday\n::::\n", nil)
	if err != nil {
		t.Fatalf("an ask holding a reading before its options is not kept: %v", err)
	}
	if err := AskWrittenContentError(midAsk); err == nil || !strings.Contains(err.Error(), "holds a reading") {
		t.Errorf("an ask holding a reading before its options = %v, want its content rule to refuse the reading", err)
	}
	_, err = ParseForWrite(reading, nil)
	if want := `typed block "reading" "r1" stands only at the end of a decision or an ask, after that block's own content`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a reading at the document's level = %v, want %q", err, want)
	}
	if IsTypedBlock("reading") && isBlockNodeType("reading") {
		t.Errorf("the reading is in the block group, want it in none")
	}
}

// A live document keeps a decision and an ask whatever order their blocks and readings stand in,
// as the browser editor builds them (proof-editor's relaxAskContentCheck): a person's paragraph
// added at the end of a decision and an agent's first reading added there at once land in an
// order the shared document decides, [paragraph, reading, paragraph] among them, and a tree the
// server refused would leave the document unreadable while every browser keeps it. Its rendering
// reads back as the tree it was rendered from. Markdown a caller writes in whole is still held to
// the decision's rule, which names the reading, and a reading anywhere a parent does not admit
// one is refused as before.
func TestALiveDecisionKeepsItsReadingWhateverOrderItsBlocksStandIn(t *testing.T) {
	paragraph := func(id, text string) *Node {
		return &Node{Type: "paragraph", Attrs: Attrs{BlockIDAttr: id}, Children: []*Node{{Type: "text", Text: text}}}
	}
	reading := func() *Node {
		return &Node{Type: "reading", Attrs: Attrs{BlockIDAttr: "r1"}, Children: []*Node{paragraph("rp", "The agent reads it as Friday.")}}
	}
	decision := &Node{Type: "decision", Attrs: Attrs{BlockIDAttr: "d1", "replaced": false}, Children: []*Node{
		paragraph("p1", "We ship on Friday."), reading(), paragraph("p2", "And we say so."),
	}}
	doc := &Node{Type: "doc", Children: []*Node{decision}}
	if err := doc.Validate(); err != nil {
		t.Fatalf("a decision holding a paragraph after its reading is refused: %v", err)
	}
	list := &Node{Type: "bullet_list", Children: []*Node{{Type: "list_item", Children: []*Node{paragraph("o1", "Friday")}}}}
	ask := &Node{Type: "ask", Attrs: Attrs{BlockIDAttr: "a1"}, Children: []*Node{
		paragraph("q1", "Which day?"), list, reading(), paragraph("p3", "An afterthought."),
	}}
	if err := (&Node{Type: "doc", Children: []*Node{ask}}).Validate(); err != nil {
		t.Fatalf("an ask holding a paragraph after its reading is refused: %v", err)
	}

	rendered, err := Render(doc)
	if err != nil {
		t.Fatalf("render the decision: %v", err)
	}
	back, err := ParseRendering(rendered)
	if err != nil {
		t.Fatalf("the decision's rendering %q does not read back: %v", rendered, err)
	}
	// A paragraph's id is not written in the markdown, so the read-back is compared without block
	// ids, and the typed blocks' own ids, which are, one by one.
	held := back.Children[0]
	if !back.Equal(doc) || held.Attrs[BlockIDAttr] != "d1" || len(held.Children) != 3 || held.Children[1].Attrs[BlockIDAttr] != "r1" {
		t.Fatalf("the decision's rendering %q reads back as %s, want the tree it was rendered from", rendered, BlockNames(held.Children))
	}
	_, err = ParseForWrite(rendered, nil)
	if want := `typed block "reading" "r1" stands only at the end of a decision or an ask, after that block's own content`; !errors.Is(err, ErrSchema) || err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("markdown written in whole holding a paragraph after a decision's reading = %v, want it refused with %q", err, want)
	}

	callout := &Node{Type: "doc", Children: []*Node{{Type: "callout", Attrs: Attrs{BlockIDAttr: "k1", "kind": "note", "title": ""}, Children: []*Node{
		paragraph("p4", "A note."), reading(),
	}}}}
	if err := callout.Validate(); err == nil || !strings.Contains(err.Error(), `typed block "reading" "r1" stands only`) {
		t.Fatalf("a callout holding a reading = %v, want the reading refused", err)
	}
}
