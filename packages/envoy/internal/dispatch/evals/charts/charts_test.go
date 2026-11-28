package charts

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// platformCommit is the commit of the platform whose answers testdata/corpus.json holds.
const platformCommit = "4f9223b9dfa9ce5721f574cc234dd9c1becc2369"

// declaredKinds are the 15 kinds the platform takes, written out here rather than read from the
// package, so a kind the port loses still fails the check.
var declaredKinds = []string{"xy", "band", "bar", "interval", "stacked-bar", "grid", "lanes", "network", "architecture",
	"module", "number", "pending", "runs", "text", "prose"}

// corpusCase is one case of testdata/corpus.json: a list of declarations, the platform's answer,
// and, where Dispatch's spec makes it answer otherwise, Dispatch's own answer and the difference
// that makes it.
type corpusCase struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Difference string          `json:"difference"`
	Charts     json.RawMessage `json:"charts"`
	ChartsJSON string          `json:"charts_json"`
	Platform   []Refusal       `json:"platform"`
	Dispatch   *[]Refusal      `json:"dispatch"`
}

// declared is the case's list as the request body holds it.
func (c corpusCase) declared() json.RawMessage {
	if c.ChartsJSON != "" {
		return json.RawMessage(c.ChartsJSON)
	}
	return c.Charts
}

// expected is Dispatch's answer to the case: its own where it has one, else the platform's.
func (c corpusCase) expected() []Refusal {
	if c.Dispatch != nil {
		return *c.Dispatch
	}
	return c.Platform
}

func readCorpus(t *testing.T) []corpusCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var corpus struct {
		PlatformCommit string       `json:"platform_commit"`
		Cases          []corpusCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decode the corpus: %v", err)
	}
	if corpus.PlatformCommit != platformCommit {
		t.Fatalf("the corpus holds the answers of the platform at %s, and the port is of %s", corpus.PlatformCommit, platformCommit)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("the corpus holds no case, and a check of none proves nothing")
	}
	return corpus.Cases
}

func validate(t *testing.T, charts string) []Refusal {
	t.Helper()
	refusals, err := Validate(json.RawMessage(charts))
	if err != nil {
		t.Fatalf("validate %s: %v", charts, err)
	}
	return refusals
}

// refusalAt is the refusal of field, if the answer holds one.
func refusalAt(refusals []Refusal, field string) (Refusal, bool) {
	index := slices.IndexFunc(refusals, func(refusal Refusal) bool { return refusal.Field == field })
	if index < 0 {
		return Refusal{}, false
	}
	return refusals[index], true
}

func holdsMessage(refusals []Refusal, part string) bool {
	return slices.ContainsFunc(refusals, func(refusal Refusal) bool { return strings.Contains(refusal.Message, part) })
}

// TestEveryKindIsAcceptedOrRefusedAsThePlatformAnswers runs every corpus case through Validate:
// each is accepted or refused as the platform answered it, with the same fields and messages in
// the same order, except where the case gives Dispatch's own answer. Every one of the 15 kinds
// has a case, and each kind Dispatch draws has one it accepts and one it refuses.
func TestEveryKindIsAcceptedOrRefusedAsThePlatformAnswers(t *testing.T) {
	cases := readCorpus(t)
	accepted, refused := map[string]int{}, map[string]int{}
	for _, c := range cases {
		if (c.Difference == "") != (c.Dispatch == nil) {
			t.Errorf("%s: a case gives Dispatch's answer exactly when it names the difference that makes it; it names %q", c.Name, c.Difference)
		}
		if len(c.declared()) == 0 {
			t.Errorf("%s: the case declares no charts", c.Name)
			continue
		}
		got, err := Validate(c.declared())
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		want := c.expected()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %d refusals: %s\nwant %d refusals: %s", c.Name, len(got), show(got), len(want), show(want))
		}
		if len(want) == 0 {
			accepted[c.Kind]++
		} else {
			refused[c.Kind]++
		}
	}
	if len(declaredKinds) != 15 {
		t.Fatalf("the platform takes 15 kinds, and the check lists %d", len(declaredKinds))
	}
	for _, kind := range declaredKinds {
		t.Logf("%s: %d accepted, %d refused", kind, accepted[kind], refused[kind])
		switch {
		case accepted[kind]+refused[kind] == 0:
			t.Errorf("kind %s has no case in the corpus", kind)
		case kind == "module":
			if accepted[kind] > 0 {
				t.Errorf("Dispatch accepts %d module cases, and it draws no module", accepted[kind])
			}
		case accepted[kind] == 0 || refused[kind] == 0:
			t.Errorf("kind %s has %d accepted and %d refused cases; it needs one of each", kind, accepted[kind], refused[kind])
		}
	}
	t.Logf("list: %d accepted, %d refused; %d cases in all", accepted["list"], refused["list"], len(cases))
}

func show(refusals []Refusal) string {
	var lines []string
	for _, refusal := range refusals {
		lines = append(lines, "\n\t"+refusal.Field+" | "+refusal.Message)
	}
	return strings.Join(lines, "")
}

// differences are the corpus cases of one of the differences Dispatch's spec makes.
func differences(t *testing.T, difference string) []corpusCase {
	t.Helper()
	var found []corpusCase
	for _, c := range readCorpus(t) {
		if c.Difference == difference {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		t.Fatalf("the corpus holds no case of the difference %s", difference)
	}
	return found
}

// TestAModuleSpecIsRefused checks that kind module is refused with its field named, even where the
// platform accepts the declaration because it carries a retired module forward.
func TestAModuleSpecIsRefused(t *testing.T) {
	platformAccepted := 0
	for _, c := range differences(t, "module") {
		got, err := Validate(c.declared())
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if len(c.Platform) == 0 {
			platformAccepted++
		}
		if holdsMessage(c.Platform, "Dispatch draws no module") {
			t.Errorf("%s: the platform's answer holds Dispatch's refusal: %s", c.Name, show(c.Platform))
		}
		if !holdsMessage(got, "must not be module: Dispatch draws no module") || !slices.Equal(got, c.expected()) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, show(got), show(c.expected()))
		}
	}
	if platformAccepted == 0 {
		t.Error("no module case is one the platform accepts, so none shows that Dispatch refuses what the platform takes")
	}
	got := validate(t, `[{"id": "risk", "title": "Grader risk", "kind": "module", "module": "any-module"}]`)
	want := []Refusal{{Field: "charts[0].kind", Message: "charts[0].kind must not be module: Dispatch draws no module"}}
	if !slices.Equal(got, want) {
		t.Errorf("a module spec:\n got %s\nwant %s", show(got), show(want))
	}
	got = validate(t, `[{"id": "a", "title": "A", "kind": "prose", "paragraphs": ["x"]}, {"id": "b", "title": "B", "kind": "module"}]`)
	if refusal, ok := refusalAt(got, "charts[1].kind"); !ok || len(got) != 1 {
		t.Errorf("a module second in the list: got %s, want one refusal at charts[1].kind", show(got))
	} else if !strings.Contains(refusal.Message, "Dispatch draws no module") {
		t.Errorf("the refusal of a module says %q, which does not say that Dispatch draws no module", refusal.Message)
	}
}

// TestApprovedAndKeptAreNeitherRequiredNorOrdered checks that no kind requires approved and that
// approved, kept and other declarations stand in any order, while approved and kept are still
// checked as the platform shapes them.
func TestApprovedAndKeptAreNeitherRequiredNorOrdered(t *testing.T) {
	for _, c := range differences(t, "approved") {
		got, err := Validate(c.declared())
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !holdsMessage(c.Platform, ".approved is required for") && !holdsMessage(c.Platform, " is out of order") {
			t.Errorf("%s: the platform's answer shows no rule of approved that Dispatch drops: %s", c.Name, show(c.Platform))
		}
		if holdsMessage(got, ".approved is required for") || holdsMessage(got, " is out of order") || !slices.Equal(got, c.expected()) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, show(got), show(c.expected()))
		}
	}
	for name, charts := range map[string]string{
		"a number with no approved":  `[{"id": "n", "title": "N", "kind": "number", "series": ["total"]}]`,
		"a pending with no approved": `[{"id": "p", "title": "P", "kind": "pending", "builder": "dispatch://DEMO-9"}]`,
		"plain, kept, then approved": `[{"id": "a", "title": "A", "kind": "prose", "paragraphs": ["x"]},
			{"id": "b", "title": "B", "kind": "bar", "y": {"series": "s"}, "kept": "dispatch://DEMO-1/ask/3"},
			{"id": "c", "title": "C", "kind": "xy", "y": {"series": "s"}, "approved": "https://example.com/approved"}]`,
	} {
		if got := validate(t, charts); len(got) != 0 {
			t.Errorf("%s is refused: %s", name, show(got))
		}
	}
	got := validate(t, `[{"id": "a", "title": "A", "kind": "xy", "y": {"series": "s"}, "approved": "http://example.com"},
		{"id": "b", "title": "B", "kind": "xy", "y": {"series": "s"}, "approved": "dispatch://DEMO-1", "kept": "dispatch://DEMO-2"},
		{"id": "c", "title": "C", "kind": "xy", "y": {"series": "s"}, "kept": "https://example.com"}]`)
	want := []Refusal{
		{Field: "charts[0].approved", Message: "charts[0].approved must be a dispatch:// or https:// link to the approved definition"},
		{Field: "charts[1].kept", Message: "charts[1].kept must not be set with approved: a declaration is approved or kept, not both"},
		{Field: "charts[2].kept", Message: "charts[2].kept must be a dispatch:// link to the owner's answer"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("approved and kept shaped wrongly:\n got %s\nwant %s", show(got), show(want))
	}
}

// TestANetworkAndAnArchitectureTakeNoBucket checks that a network and an architecture chart need
// no graph, the platform's folder of graph files, and that a graph given is checked as the
// platform shapes it.
func TestANetworkAndAnArchitectureTakeNoBucket(t *testing.T) {
	for _, c := range differences(t, "graph") {
		got, err := Validate(c.declared())
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !slices.ContainsFunc(c.Platform, func(refusal Refusal) bool { return strings.HasSuffix(refusal.Field, ".graph") }) {
			t.Errorf("%s: the platform's answer does not require the graph: %s", c.Name, show(c.Platform))
		}
		if !slices.Equal(got, c.expected()) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, show(got), show(c.expected()))
		}
	}
	network := `{"id": "n", "title": "N", "kind": "network", "size": "weight", "color": "status"`
	architecture := `{"id": "a", "title": "A", "kind": "architecture", "value_series": "risk", "bins": [1, 2]`
	if got := validate(t, "["+network+"}, "+architecture+"}]"); len(got) != 0 {
		t.Errorf("a network and an architecture with no graph are refused: %s", show(got))
	}
	graph := `, "graph": {"team": "demo", "source_prefix": "graphs/"}}`
	if got := validate(t, "["+network+graph+", "+architecture+graph+"]"); len(got) != 0 {
		t.Errorf("a network and an architecture with a graph are refused: %s", show(got))
	}
	got := validate(t, "["+network+`, "graph": null}, `+architecture+`, "graph": {"team": "demo", "source_prefix": "graphs"}}]`)
	want := []Refusal{
		{Field: "charts[0].graph", Message: "charts[0].graph must be an object"},
		{Field: "charts[1].graph.source_prefix", Message: "charts[1].graph.source_prefix must be a relative folder prefix ending in /"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("a graph shaped wrongly:\n got %s\nwant %s", show(got), show(want))
	}
}

// TestANumberTakesAnAdapterOfThePlatformsList checks that a number's adapter is one of the
// platform's two, accepted with no approved, and that any other is refused in the platform's words.
func TestANumberTakesAnAdapterOfThePlatformsList(t *testing.T) {
	for _, c := range differences(t, "adapter") {
		got, err := Validate(c.declared())
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !slices.Equal(got, c.expected()) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, show(got), show(c.expected()))
		}
	}
	for _, adapter := range []string{"graph-headline", "run-headline"} {
		if got := validate(t, `[{"id": "n", "title": "N", "kind": "number", "series": ["total"], "adapter": "`+adapter+`"}]`); len(got) != 0 {
			t.Errorf("adapter %s is refused: %s", adapter, show(got))
		}
	}
	got := validate(t, `[{"id": "n", "title": "N", "kind": "number", "series": ["total"], "adapter": "headline"}]`)
	want := []Refusal{{Field: "charts[0].adapter",
		Message: `charts[0].adapter must name an adapter of loaders/adapters.json (graph-headline, run-headline); "headline" is not one`}}
	if !slices.Equal(got, want) {
		t.Errorf("an unknown adapter:\n got %s\nwant %s", show(got), show(want))
	}
}

// TestChartsThatAreNoJSONValueAreAnError checks that Validate answers an error, not a refusal, for
// bytes that are no single JSON value: no request body that decoded holds them.
func TestChartsThatAreNoJSONValueAreAnError(t *testing.T) {
	for _, charts := range []string{"", "[", `[{"id": }]`, "[1,]", "[] []", "NaN", `{"a" 1}`, `[{1: 2}]`} {
		if refusals, err := Validate(json.RawMessage(charts)); err == nil {
			t.Errorf("%q answers no error but %s", charts, show(refusals))
		}
	}
}
