package textversions

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestEveryEventTypeIsSortedForTheKeywordFill holds the rules to every event type there is: each
// member of contracts.DispatchEventTypes, the DispatchEvent union of packages/contracts, either
// can start a version (versionRules) or changes no searchable text (unchangingTypes), and is in
// exactly one of the two; neither list names a type the union lacks; and each type production's
// log holds (testdata/production_event_types.txt, written by `testdata/capture.sh types`) is a
// member of the union, so the fill meets no event of a type the rules were never sorted for.
func TestEveryEventTypeIsSortedForTheKeywordFill(t *testing.T) {
	members := make(map[string]bool, len(contracts.DispatchEventTypes))
	var neither, both []string
	for _, typ := range contracts.DispatchEventTypes {
		members[typ] = true
		_, starts := versionRules[typ]
		_, unchanging := unchangingTypes[typ]
		switch {
		case starts && unchanging:
			both = append(both, typ)
		case !starts && !unchanging:
			neither = append(neither, typ)
		}
	}
	var strays []string
	for typ := range versionRules {
		if !members[typ] {
			strays = append(strays, typ)
		}
	}
	for typ := range unchangingTypes {
		if !members[typ] && !slices.Contains(strays, typ) {
			strays = append(strays, typ)
		}
	}
	slices.Sort(strays)
	var unknown []string
	for _, typ := range productionEventTypes(t) {
		if !members[typ] {
			unknown = append(unknown, typ)
		}
	}

	if len(neither) > 0 {
		t.Errorf("%d of the %d event types are in neither versionRules nor unchangingTypes: %s",
			len(neither), len(contracts.DispatchEventTypes), strings.Join(neither, ", "))
	}
	if len(both) > 0 {
		t.Errorf("%d event types are in both versionRules and unchangingTypes: %s",
			len(both), strings.Join(both, ", "))
	}
	if len(strays) > 0 {
		t.Errorf("the rules name %d types that are no member of contracts.DispatchEventTypes: %s",
			len(strays), strings.Join(strays, ", "))
	}
	if len(unknown) > 0 {
		t.Errorf("production's log holds %d event types that contracts.DispatchEventTypes lacks: %s",
			len(unknown), strings.Join(unknown, ", "))
	}
}

// productionEventTypes reads the types of testdata/production_event_types.txt: one line per type,
// `<type><TAB><count>`, under comment lines that start with #.
func productionEventTypes(t *testing.T) []string {
	t.Helper()
	const path = "testdata/production_event_types.txt"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the types of production's log: %v", err)
	}
	var types []string
	for number, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		typ, count, ok := strings.Cut(line, "\t")
		if _, err := strconv.Atoi(count); !ok || typ == "" || err != nil {
			t.Fatalf("%s:%d is not <type><TAB><count>: %q", path, number+1, line)
		}
		types = append(types, typ)
	}
	if len(types) == 0 {
		t.Fatalf("%s names no event type", path)
	}
	return types
}

// TestEveryProductionPayloadShapeGivesItsVersion decodes the first event of each payload shape
// production's log holds of the types the fill reads (testdata/payloads/<type>-<n>.json, captured
// and redacted by `testdata/capture.sh shapes`) through the rules, as the first event of its item,
// and holds the version it starts to <type>-<n>.expected.json, which capture.sh's jq read from the
// same redacted event and never from this package's code: null there is an event that starts no
// version. It names each shape whose type has no rule.
//
// An ask's event is given the body readEventsQuery reads of it, askBodySQL run over its payload
// in a test database, and the version's body is held to the one ask_search_suffix makes of the
// expected question, options and answer, so what the fill's SQL reads of an ask's payload is held
// to what jq read of it. That body is where a version holds an ask's question, so the question is
// held through it alone.
func TestEveryProductionPayloadShapeGivesItsVersion(t *testing.T) {
	names := payloadFixtures(t)
	database := storetest.Open(t)
	var noRule []string
	for _, name := range names {
		fixture := readFixtureEvent(t, name)
		want := readExpectedVersion(t, name)
		if slices.Contains(askTypes, fixture.Type) {
			fixture.AskBody = askBodyOfPayload(t, database, name, fixture.Payload)
		}
		if want != nil && want.Kind == kindAsk {
			want.TitleOrBody, want.AskBody = nil, expectedAskBody(t, database, name, *want)
		}
		got, started, err := versionOf(event(fixture))
		switch {
		case errors.Is(err, errNoRule):
			noRule = append(noRule, name)
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case want == nil && started:
			t.Errorf("%s starts the version %s; its payload starts none", name, expectedOf(got))
		case want == nil:
		case !started:
			t.Errorf("%s starts no version; its payload gives %s", name, *want)
		default:
			if differ := want.differences(expectedOf(got)); len(differ) > 0 {
				t.Errorf("%s starts a version whose %s", name, strings.Join(differ, "; "))
			}
		}
	}
	if len(noRule) > 0 {
		t.Errorf("%d of the %d payload shapes of production's log have no rule:\n%s",
			len(noRule), len(names), strings.Join(noRule, "\n"))
	}
}

// askBodyOfPayload is the body readEventsQuery reads of the ask event name whose payload is
// payload: askBodySQL, run over it in database.
func askBodyOfPayload(t *testing.T, database *store.Store, name string, payload json.RawMessage) *string {
	t.Helper()
	var body *string
	if err := database.Pool.QueryRow(t.Context(), `select `+askBodySQL+` from (select $1::jsonb) as e (payload)`, payload).Scan(&body); err != nil {
		t.Fatalf("read the ask body of %s: %v", name, err)
	}
	return body
}

// expectedAskBody is the body of the ask want, the expected version of name: its question, a
// space and ask_search_suffix of its options and answer, made in database.
func expectedAskBody(t *testing.T, database *store.Store, name string, want expectedVersion) *string {
	t.Helper()
	var body *string
	if err := database.Pool.QueryRow(t.Context(), `select $1 || ' ' || ask_search_suffix($2::jsonb, $3::jsonb)`,
		want.TitleOrBody, want.Options, want.Answer).Scan(&body); err != nil {
		t.Fatalf("make the expected ask body of %s: %v", name, err)
	}
	return body
}

// TestAnAskEventReadWithNoBodyIsAFillBug holds askVersion's answer for an ask's event read with
// no body (event.AskBody) while its payload holds a question: an error that names a bug of the
// fill, and not errUnreadablePayload, which the fill takes for a payload a person wrote and stops
// at.
func TestAnAskEventReadWithNoBodyIsAFillBug(t *testing.T) {
	_, started, err := versionOf(event(readFixtureEvent(t, "ask.opened-1")))
	switch {
	case err == nil:
		t.Fatalf("an ask's event read with no body answers no error (started %v), want one naming a bug of the fill", started)
	case errors.Is(err, errUnreadablePayload):
		t.Fatalf("an ask's event read with no body answers %q, an unreadable payload; want an error naming a bug of the fill", err)
	case !strings.Contains(err.Error(), "a bug of the fill"):
		t.Fatalf("an ask's event read with no body answers %q, want an error naming a bug of the fill", err)
	}
}

// errNoRule is versionOf's answer for an event of a type versionRules holds no rule for.
var errNoRule = errors.New("no keyword version rule reads this event type")

// versionOf decodes e through the rule of its type as the first event of its item, which has no
// version before it (noLatest): the version e starts, or started false where it starts none.
func versionOf(e event) (v version, started bool, err error) {
	decide, ok := versionRules[e.Type]
	if !ok {
		return version{}, false, fmt.Errorf("%w: %s", errNoRule, e.Type)
	}
	return decide(e, noLatest)
}

// captureScript is the script that captures this package's testdata.
const captureScript = "testdata/capture.sh"

// fillTypesLine is the one assignment of capture.sh's fill_types, the event types whose payload
// shapes `capture.sh shapes` captures.
var fillTypesLine = regexp.MustCompile(`(?m)^fill_types="([^"]*)"$`)

// TestTheCaptureScriptCapturesEveryRuleType holds capture.sh's fill_types to the types a rule
// reads (versionRules). The script cannot read Go, so it writes them out itself; a type a rule
// reads that it left out would get no fixture under testdata/payloads, and
// TestEveryProductionPayloadShapeGivesItsVersion would pass with that type's shapes untested.
func TestTheCaptureScriptCapturesEveryRuleType(t *testing.T) {
	script, err := os.ReadFile(captureScript)
	if err != nil {
		t.Fatalf("read %s: %v", captureScript, err)
	}
	want := strings.Join(ruleTypes, " ")
	matches := fillTypesLine.FindAllStringSubmatch(string(script), -1)
	if len(matches) != 1 {
		t.Fatalf("%s assigns fill_types %d times; want one line fill_types=%q", captureScript, len(matches), want)
	}
	captured := strings.Fields(matches[0][1])
	slices.Sort(captured)
	if slices.Equal(captured, ruleTypes) {
		return
	}
	var lacks, besides []string
	for _, typ := range ruleTypes {
		if !slices.Contains(captured, typ) {
			lacks = append(lacks, typ)
		}
	}
	for _, typ := range captured {
		if _, ok := versionRules[typ]; !ok {
			besides = append(besides, typ)
		}
	}
	t.Errorf("%s's fill_types, sorted, is %q, which lacks %q of the types a rule reads and names %q besides; write fill_types=%q",
		captureScript, strings.Join(captured, " "), lacks, besides, want)
}

// payloadFixtures names each captured event of testdata/payloads, <type>-<n> for
// <type>-<n>.json, by type and then n, and fails for one with no <type>-<n>.expected.json or the
// other way round.
func payloadFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "payloads", "*.json"))
	if err != nil {
		t.Fatalf("list the captured payloads: %v", err)
	}
	events, expected := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		base := filepath.Base(path)
		if name, ok := strings.CutSuffix(base, ".expected.json"); ok {
			expected[name] = true
		} else {
			events[strings.TrimSuffix(base, ".json")] = true
		}
	}
	var names []string
	for name := range events {
		if !expected[name] {
			t.Errorf("testdata/payloads/%s.json has no %s.expected.json", name, name)
		}
		names = append(names, name)
	}
	for name := range expected {
		if !events[name] {
			t.Errorf("testdata/payloads/%s.expected.json has no %s.json", name, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("testdata/payloads holds no captured event")
	}
	slices.SortFunc(names, func(a, b string) int {
		aType, aNumber := fixtureOrder(a)
		bType, bNumber := fixtureOrder(b)
		return cmp.Or(strings.Compare(aType, bType), cmp.Compare(aNumber, bNumber))
	})
	return names
}

// fixtureOrder splits <type>-<n> into its type and n.
func fixtureOrder(name string) (string, int) {
	at := strings.LastIndex(name, "-")
	if at < 0 {
		return name, 0
	}
	number, _ := strconv.Atoi(name[at+1:])
	return name[:at], number
}

// fixtureEvent is a captured event as GET /api/v1/events/log answers it: of its envelope, the
// columns of the event's row the rules read, and its payload; and of an ask's event the body the
// fill reads of it, which the envelope does not hold and the test makes (askBodyOfPayload). Its
// fields are event's, in the same order, so it converts to one; the bytes the fill's read counts
// of an event, which no rule reads, stay 0.
type fixtureEvent struct {
	ID                         int64           `json:"id"`
	Type                       string          `json:"type"`
	IssueKey                   *string         `json:"issue_key"`
	ArtifactID                 *string         `json:"artifact_id"`
	Project                    string          `json:"project"`
	Seq                        int32           `json:"seq"`
	CreatedAt                  time.Time       `json:"created_at"`
	Payload                    json.RawMessage `json:"payload"`
	AskBody                    *string         `json:"-"`
	PayloadBytes, RowBodyBytes int64           `json:"-"`
}

func readFixtureEvent(t *testing.T, name string) fixtureEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "payloads", name+".json"))
	if err != nil {
		t.Fatalf("read the captured event %s: %v", name, err)
	}
	var fixture fixtureEvent
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the captured event %s: %v", name, err)
	}
	if fixture.Type == "" || !strings.HasPrefix(name, fixture.Type+"-") {
		t.Fatalf("the captured event %s is of type %q", name, fixture.Type)
	}
	return fixture
}

// expectedVersion is <type>-<n>.expected.json: the version the payload of <type>-<n>.json
// starts, null where the payload gives no value. A body_truncated of null reads as false. An
// ask's question, options and answer are no field of a version: they are held through the ask's
// body.
type expectedVersion struct {
	Kind           string          `json:"kind"`
	ID             string          `json:"id"`
	IssueKey       *string         `json:"issue_key"`
	TitleOrBody    *string         `json:"title_or_body"`
	Options        json.RawMessage `json:"options"`
	Answer         json.RawMessage `json:"answer"`
	SpecArtifactID *string         `json:"spec_artifact_id"`
	SpecVersion    *int32          `json:"spec_version"`
	BodyTruncated  bool            `json:"body_truncated"`
	// AskBody is an ask's body, which no expected file holds: for an expected version, the one
	// ask_search_suffix makes of its question, options and answer (expectedAskBody), which then
	// holds the expected question in place of TitleOrBody; for a version the rules started, its
	// Body. It is nil for any other kind.
	AskBody *string `json:"-"`
}

// readExpectedVersion reads <name>.expected.json: nil where it is null, for an event that starts
// no version.
func readExpectedVersion(t *testing.T, name string) *expectedVersion {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "payloads", name+".expected.json"))
	if err != nil {
		t.Fatalf("read the expected version of %s: %v", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var want *expectedVersion
	if err := decoder.Decode(&want); err != nil {
		t.Fatalf("decode the expected version of %s: %v", name, err)
	}
	return want
}

// expectedOf is the version v in the form of an expected one: v's Body is the title_or_body of
// an issue or a post, null where it is empty, as for an artifact.version of an issue with no
// version yet, whose title no event gives, and the AskBody of an ask.
func expectedOf(v version) expectedVersion {
	got := expectedVersion{
		Kind:           v.Kind,
		ID:             v.ID,
		IssueKey:       v.IssueKey,
		SpecArtifactID: v.SpecArtifactID,
		SpecVersion:    v.SpecVersion,
		BodyTruncated:  v.BodyTruncated,
	}
	switch {
	case v.Kind == kindAsk:
		got.AskBody = &v.Body
	case v.Body != "":
		got.TitleOrBody = &v.Body
	}
	return got
}

// fields is each field a version holds with its value as canonical JSON, which is how two
// versions are compared: an ask's question, options and answer through its body (AskBody).
func (e expectedVersion) fields() [][2]string {
	return [][2]string{
		{"kind", canonicalJSON(e.Kind)},
		{"id", canonicalJSON(e.ID)},
		{"issue_key", canonicalJSON(e.IssueKey)},
		{"title_or_body", canonicalJSON(e.TitleOrBody)},
		{"ask_body", canonicalJSON(e.AskBody)},
		{"spec_artifact_id", canonicalJSON(e.SpecArtifactID)},
		{"spec_version", canonicalJSON(e.SpecVersion)},
		{"body_truncated", canonicalJSON(e.BodyTruncated)},
	}
}

// differences names each field in which got differs from e, with both values abridged to 200
// bytes.
func (e expectedVersion) differences(got expectedVersion) []string {
	var differ []string
	gotFields := got.fields()
	for i, field := range e.fields() {
		if field[1] != gotFields[i][1] {
			differ = append(differ, fmt.Sprintf("%s is %s, and its payload gives %s",
				field[0], abridged(gotFields[i][1], 200), abridged(field[1], 200)))
		}
	}
	return differ
}

func (e expectedVersion) String() string {
	fields := e.fields()
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field[0]+": "+abridged(field[1], 200))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func canonicalJSON(value any) string {
	out, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return string(out)
}

// abridged is text for a failure message: its first length bytes, and "..." after them where
// text is longer.
func abridged(text string, length int) string {
	if len(text) <= length {
		return text
	}
	return text[:length] + "..."
}
