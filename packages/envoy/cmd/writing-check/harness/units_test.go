package harness

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

func TestTheCandidateListAndThePriceTable(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	roles := cfg.Models.Roles
	if roles.Grader.ID != "claude-opus-5" || roles.Writer.ID != "claude-opus-5-5" || roles.Reader.ID != "claude-sonnet-4-6" {
		t.Errorf("the roles of Decision 20: %+v", roles)
	}
	if len(cfg.Models.Candidates) != 9 || len(cfg.Models.Pending) != 4 {
		t.Errorf("nine candidates today, Kimi-K3.1-Pro at reasoning off and the writer's model among them, and INFRA-3483's four pending: %d, %d", len(cfg.Models.Candidates), len(cfg.Models.Pending))
	}
	for _, bad := range []struct{ name, candidates string }{
		{"a role of another family", strings.Replace(string(candidatesFile), "reader: {family: claude, id: claude-sonnet-4-6}", "reader: {family: foundry, id: grok-4.7}", 1)},
		{"the grader playing the writer", strings.Replace(string(candidatesFile), "writer: {family: claude, id: claude-opus-5-5}", "writer: {family: claude, id: claude-opus-5}", 1)},
		{"the grader playing the reader", strings.Replace(string(candidatesFile), "reader: {family: claude, id: claude-sonnet-4-6}", "reader: {family: claude, id: claude-opus-5}", 1)},
		{"a fallback grader playing the reader", strings.Replace(string(candidatesFile), "reader: {family: claude, id: claude-sonnet-4-6}", "reader: {family: claude, id: claude-opus-4-7}", 1)},
		{"a model with no price", strings.Replace(string(candidatesFile), "id: claude-haiku-5-5", "id: claude-haiku-9", 1)},
		{"an effort the local Dispatch refuses", strings.Replace(string(candidatesFile), `effort: "off"`, `effort: "max"`, 1)},
		{"a candidate at one effort named twice", strings.Replace(string(candidatesFile), "  - {family: foundry, id: grok-4.7}\n", "  - {family: foundry, id: grok-4.7}\n  - {family: foundry, id: Kimi-K3.1-Pro, effort: \"off\"}\n", 1)},
		{"the writer's model named twice as a candidate", strings.Replace(string(candidatesFile), "  - {family: foundry, id: grok-4.7}\n", "  - {family: foundry, id: grok-4.7}\n  - {family: claude, id: claude-opus-5-5}\n", 1)},
	} {
		if bad.candidates == string(candidatesFile) {
			t.Fatalf("%s: the replacement matched nothing in candidates.yaml", bad.name)
		}
		if _, err := parseConfig([]byte(bad.candidates), pricesFile); err == nil {
			t.Errorf("%s was let through", bad.name)
		}
	}
}

// R2 bars only the grader from being a candidate: the writer and the fresh reader may be. So
// candidates.yaml, whose writer claude-opus-5-5 is also a candidate, loads, and so does one whose
// fresh reader is a candidate; one that names the grader or a fallback grader as a candidate is
// refused. A model that plays a role and is a candidate is one model of AllModels, which the
// probe calls once, and its role names both.
func TestTheConfigBarsOnlyTheGraderFromTheCandidates(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	writer := cfg.Models.Roles.Writer
	if writer.ID != "claude-opus-5-5" || !cfg.IsCandidate(writer.ID) || !cfg.IsCandidate("claude-fable-5-2") {
		t.Errorf("the writer %s and claude-fable-5-2 are candidates of candidates.yaml: %+v", writer.ID, cfg.Models.Candidates)
	}
	if got := cfg.RoleOf(writer.ID); got != RoleWriter+", "+RoleCandidate {
		t.Errorf("the role of the writer = %q, want %q", got, RoleWriter+", "+RoleCandidate)
	}
	if got := cfg.RoleOf(cfg.Models.Roles.Grader.ID); got != RoleGrader {
		t.Errorf("the role of the grader = %q", got)
	}
	if got := cfg.RoleOf("claude-fable-5-2"); got != RoleCandidate {
		t.Errorf("the role of claude-fable-5-2 = %q", got)
	}
	n := 0
	for _, m := range cfg.AllModels() {
		if m.ID == writer.ID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("AllModels lists the writer %d times, want once", n)
	}
	replace := func(old, with string) []byte {
		t.Helper()
		out := strings.Replace(string(candidatesFile), old, with, 1)
		if out == string(candidatesFile) {
			t.Fatalf("%q matched nothing in candidates.yaml", old)
		}
		return []byte(out)
	}
	if _, err := parseConfig(replace("reader: {family: claude, id: claude-sonnet-4-6}", "reader: {family: claude, id: claude-fable-5-2}"), pricesFile); err != nil {
		t.Errorf("a fresh reader that is a candidate was refused: %v", err)
	}
	for _, bad := range []struct{ name, id string }{
		{"the grader", cfg.Models.Roles.Grader.ID},
		{"a fallback grader", cfg.Models.Roles.GraderFallbacks[1].ID},
	} {
		_, err := parseConfig(replace("  - {family: claude, id: claude-opus-4-8}\n", "  - {family: claude, id: "+bad.id+"}\n"), pricesFile)
		if err == nil || !strings.Contains(err.Error(), "is also a candidate") {
			t.Errorf("%s %s as a candidate: %v, want it refused", bad.name, bad.id, err)
		}
	}
}

// A candidate at an effort is a candidate of its own, named <id>:<effort>: candidates.yaml's
// Kimi-K3.1-Pro:off is a test model of `local --test-models` beside Kimi-K3.1-Pro, `score --model`
// and `--models` take its name, and its lane calls deployment Kimi-K3.1-Pro at effort off, at
// Kimi-K3.1-Pro's price, and names the candidate in the Dispatch-Test-Model header. Kimi-K3.1-Pro
// alone sends no effort, as before; a name candidates.yaml does not hold is refused.
func TestACandidateAtAnEffortIsACandidateOfItsOwn(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cfg.Candidate("Kimi-K3.1-Pro:off"); !ok || got != (ModelEntry{Family: llm.FamilyFoundry, ID: "Kimi-K3.1-Pro", Effort: "off"}) {
		t.Errorf("the candidate Kimi-K3.1-Pro:off = %+v (%t)", got, ok)
	}
	if got, ok := cfg.Candidate("Kimi-K3.1-Pro"); !ok || got.Effort != "" || got.Name() != "Kimi-K3.1-Pro" {
		t.Errorf("the candidate Kimi-K3.1-Pro = %+v (%t), want it at no effort", got, ok)
	}

	mf := &modelFlags{gateway: "http://gateway.example:4000", keyFile: "/var/run/model-gateway-key/key",
		foundry: "https://foundry.example/models", apiVersion: defaultAPIVersion, tenant: "tenant-id", clientID: "client-id", tokenFile: defaultTokenFile, scope: "scope"}
	set, err := testModelSettings(cfg, mf)
	if err != nil {
		t.Fatal(err)
	}
	listed := strings.Split(set["DISPATCH_TEST_MODELS"], ",")
	if len(listed) != len(cfg.Models.Candidates) || !slices.Contains(listed, "Kimi-K3.1-Pro:off=azure") || !slices.Contains(listed, "Kimi-K3.1-Pro=azure") {
		t.Errorf("DISPATCH_TEST_MODELS = %q, want each candidate by its name, Kimi-K3.1-Pro:off=azure and Kimi-K3.1-Pro=azure among them", set["DISPATCH_TEST_MODELS"])
	}

	run := &changeRun{cfg: cfg, runner: &llm.Runner{}}
	lanes, err := run.withCandidates(mf, []string{"Kimi-K3.1-Pro:off", "Kimi-K3.1-Pro"}, true)
	if err != nil {
		t.Fatal(err)
	}
	price := cfg.Prices.Prices["Kimi-K3.1-Pro"].agentloop()
	for i, want := range []struct{ name, effort string }{{"Kimi-K3.1-Pro:off", "off"}, {"Kimi-K3.1-Pro", ""}} {
		lane := lanes[i].(*changeRun)
		if m := lane.candModel; m.ID != "Kimi-K3.1-Pro" || m.Effort != want.effort || m.Price != price {
			t.Errorf("the lane of %s calls %q at effort %q, priced %+v; want Kimi-K3.1-Pro at %q, priced %+v", want.name, m.ID, m.Effort, m.Price, want.effort, price)
		}
		if got := lane.testModel().Get("Dispatch-Test-Model"); got != want.name {
			t.Errorf("the lane of %s names %q in the Dispatch-Test-Model header", want.name, got)
		}
	}
	if _, err := run.withCandidates(mf, []string{"Kimi-K3.1-Pro:low"}, false); err == nil {
		t.Error("Kimi-K3.1-Pro:low, which candidates.yaml does not hold, was let through")
	}
}

func TestPlantedOutputsAreMarked(t *testing.T) {
	yes, no := true, false
	five, minus := 5, -1
	for _, tc := range []struct {
		name  string
		p     Planted
		grade Grade
		want  bool
	}{
		{"a wrong answer the grader fails", Planted{Judgement: JAnswered, Planted: PlantedWrong}, Grade{Verdict: &no}, true},
		{"a wrong answer the grader passes", Planted{Judgement: JAnswered, Planted: PlantedWrong}, Grade{Verdict: &yes}, false},
		{"a right definition the grader passes", Planted{Judgement: JDefine, Planted: PlantedRight}, Grade{Verdict: &yes}, true},
		{"a harder text the grader finds harder", Planted{Judgement: JHarder, Planted: PlantedWrong}, Grade{Verdict: &yes}, true},
		{"a lost fact the grader names", Planted{Judgement: JLost, Planted: PlantedWrong, Marker: "noon"}, Grade{Count: 1, Items: []Item{{Quote: "at Noon", Note: "the time"}}}, true},
		{"a lost fact the grader misses", Planted{Judgement: JLost, Planted: PlantedWrong, Marker: "noon"}, Grade{Count: 1, Items: []Item{{Quote: "the date"}}}, false},
		{"a right text the grader lists nothing in", Planted{Judgement: JPhrases, Planted: PlantedRight}, Grade{}, true},
		{"a right text the grader lists a phrase in", Planted{Judgement: JPhrases, Planted: PlantedRight}, Grade{Count: 1}, false},
		{"an answer start near the planted one", Planted{Judgement: JAnswerStart, Planted: PlantedWrong, WordsBefore: &five}, Grade{Found: &yes, WordsBefore: &[]int{8}[0]}, true},
		{"an answer start far from it", Planted{Judgement: JAnswerStart, Planted: PlantedWrong, WordsBefore: &five}, Grade{Found: &yes, WordsBefore: &[]int{40}[0]}, false},
		{"an answer start not found in the text", Planted{Judgement: JAnswerStart, Planted: PlantedRight, WordsBefore: &five}, Grade{Found: &yes, WordsBefore: &minus}, false},
		{"a grade that failed", Planted{Judgement: JAnswered, Planted: PlantedRight}, Grade{Verdict: &yes, Error: "the call failed"}, false},
	} {
		if got := tc.p.correct(tc.grade); got != tc.want {
			t.Errorf("%s: correct = %t, want %t", tc.name, got, tc.want)
		}
	}
	if err := (Planted{ID: "x", Judgement: JLost, Planted: PlantedWrong, Output: "text"}).check(); err == nil {
		t.Error("a wrong list output with no marker was let through")
	}
}

func TestProbeRows(t *testing.T) {
	m := ModelEntry{Family: "foundry", ID: "grok-4.7"}
	ok := probeCall{Status: http.StatusOK, Served: "grok-4.7", Seconds: 1}
	if row := probeRow(m, "candidate", []probeCall{ok, ok}); !row.Pass || row.OK != 2 {
		t.Errorf("two answers of the id asked for: %+v", row)
	}
	notFound := probeCall{Status: http.StatusNotFound, Reason: routeNotFound}
	if row := probeRow(m, "candidate", []probeCall{ok, notFound}); row.Pass || row.RouteNotFound != 1 {
		t.Errorf("a route_not_found passed: %+v", row)
	}
	other := probeCall{Status: http.StatusOK, Served: "grok-4.6"}
	if row := probeRow(m, "candidate", []probeCall{ok, other}); row.Pass {
		t.Errorf("an answer by another model passed: %+v", row)
	}
	busy := probeCall{Status: http.StatusTooManyRequests, Error: "slow down"}
	if row := probeRow(m, "candidate", []probeCall{ok, busy}); !row.Pass {
		t.Errorf("a 429 among answers failed the probe: %+v", row)
	}
	for _, tc := range []struct {
		requested, served string
		want              bool
	}{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", true},
		{"claude-opus-5", "claude-opus-5-20261101", true},
		{"claude-opus-5", "anthropic/claude-opus-5", true},
		{"Kimi-K3.1-Pro", "kimi-k3-1-pro", true},
		{"claude-opus-5", "claude-sonnet-5-5", false},
	} {
		if got := servedMatches(tc.requested, tc.served); got != tc.want {
			t.Errorf("servedMatches(%q, %q) = %t", tc.requested, tc.served, got)
		}
	}
}

// TestAReplysOwnChecksDecide: a check the grader marks true fails the verdict whatever the
// verdict field says; an item the grader marks defined, an item the text does not hold and a
// second item of one quote are no findings.
func TestAReplysOwnChecksDecide(t *testing.T) {
	var g Grade
	reply := `{"1_asked":"what it means","2_given":"the words again","3_restates_question":true,"4_points_elsewhere":false,` +
		`"5_other_question":false,"6_contradicts_record":false,"7_not_plain":false,"8_reason":"r","9_answers":true}`
	if err := judgementSpecs[JAnswered].read([]byte(reply), &g, ""); err != nil {
		t.Fatal(err)
	}
	if g.Verdict == nil || *g.Verdict || len(g.Failed) != 1 || g.Failed[0] != "restates_question" || g.Notes["given"] != "the words again" {
		t.Errorf("an answer that restates the question: %+v", g)
	}
	g = Grade{}
	item := func(word string, common, reference bool) string {
		return fmt.Sprintf(`{"1_word":%q,"2_defined_in_text":false,"3_defined_in_record":false,"4_common_meaning":%t,"5_reference_only":%t,"6_why":"w"}`, word, common, reference)
	}
	reply = `{"undefined":[` + strings.Join([]string{item("session", true, false), item("Zork", false, false), item("zork", false, false),
		item("run.sh", false, true), item("quux", false, false)}, ",") + `]}`
	if err := judgementSpecs[JUndefined].read([]byte(reply), &g, "The session built the **zork** with `run.sh`."); err != nil {
		t.Fatal(err)
	}
	if g.Count != 1 || g.Items[0].Quote != "Zork" || g.Items[0].Note != "w" || g.Dropped != 3 || g.NotInText != 1 {
		t.Errorf("the undefined words: %+v", g)
	}
	// The phrases judgement's candidates: the sentences that hold a verb of the definition,
	// outside code, with the sentences next to them; a table cell and a list item are sentences.
	text := "The run is green. This lands the change today.\n\n- The page lists `names` only.\n\n| Step | It shows the gap |\n\n```\nit names nothing\n```\n> The quote names it."
	cands := phraseCandidates(text)
	if len(cands) != 2 || cands[0].Sentence != "This lands the change today." || cands[0].Before != "The run is green." ||
		cands[1].Sentence != "It shows the gap" {
		t.Errorf("the candidates: %+v", cands)
	}
	// A reply missing a check is no reply of the schema.
	if err := judgementSpecs[JAnswered].read([]byte(`{"8_reason":"r","9_answers":true}`), &Grade{}, ""); err == nil {
		t.Error("a reply with no checks was read")
	}
	// The schema asks for the fields in the prompt's order, the verdict last.
	schema := string(judgementSpecs[JAnswered].schema())
	if i, j := strings.Index(schema, `"1_asked"`), strings.Index(schema, `"9_answers"`); i < 0 || j < i {
		t.Errorf("the answered schema: %s", schema)
	}
}

// The lost judgement drops an item its own checks rule out: one whose words state no fact, and
// one whose fact the new text states. The prompt and the schema ask for both checks.
func TestALostItemWithEitherCheckTrueIsDropped(t *testing.T) {
	item := func(quote string, noFact, inNewText bool) string {
		return fmt.Sprintf(`{"1_quote":%q,"2_states_no_fact":%t,"3_in_new_text":%t,"4_fact":"f"}`, quote, noFact, inNewText)
	}
	reply := `{"lost":[` + strings.Join([]string{item("**The choice:**", true, false), item("The run took 40 s.", false, true),
		item("Agreed.", true, true), item("The test fails on node 7.", false, false)}, ",") + `]}`
	var g Grade
	if err := judgementSpecs[JLost].read([]byte(reply), &g, "The new text."); err != nil {
		t.Fatal(err)
	}
	if g.Count != 1 || len(g.Items) != 1 || g.Items[0] != (Item{Quote: "The test fails on node 7.", Note: "f"}) || g.Dropped != 3 || g.NotInText != 0 {
		t.Errorf("the lost facts: %+v, want the one item with both checks false, and 3 dropped", g)
	}
	// A lost fact's quote is of the original text, so no item is dropped for words the new
	// text does not hold.
	if judgementSpecs[JLost].inText {
		t.Error("the lost judgement looks its quotes up in the new text")
	}
	schema := string(judgementSpecs[JLost].schema())
	for _, want := range []string{`"1_quote":{"type":"string"}`, `"2_states_no_fact":{"type":"boolean"}`, `"3_in_new_text":{"type":"boolean"}`, `"4_fact":{"type":"string"}`} {
		if !strings.Contains(schema, want) {
			t.Errorf("the lost schema does not hold %s: %s", want, schema)
		}
	}
	prompt, err := render("grade-lost", promptData{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"2_states_no_fact"`, `"3_in_new_text"`, "- A heading", "- A label", "- A word of agreement", "says again what another sentence", "The reader's own words, quoted back"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the lost prompt does not hold %q", want)
		}
	}
	// The added judgement has no such check: each of its items is a finding.
	g = Grade{}
	if err := judgementSpecs[JAdded].read([]byte(`{"added":[{"1_quote":"at noon","2_fact":"f"}]}`), &g, ""); err != nil || g.Count != 1 || g.Dropped != 0 {
		t.Errorf("the added facts: %+v, %v", g, err)
	}
}
