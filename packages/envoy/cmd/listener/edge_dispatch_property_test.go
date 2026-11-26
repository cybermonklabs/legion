package main

//ci:reads packages/contracts/fixtures/dispatch-frame-property.json TestEdgeFrameVerdictOnGeneratedDispatchFrames os.ReadFile (dispatchPropertyFixture, ../../../contracts/fixtures): the generated frames the edge passes and withholds must be the ones the fixture records for the plugin's side of the property

import (
	"encoding/json"
	"flag"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// updateDispatchProperty rewrites dispatchPropertyFixture from this tree's generator and
// edgeFrameVerdict.
var updateDispatchProperty = flag.Bool("update-dispatch-property", false,
	"rewrite packages/contracts/fixtures/dispatch-frame-property.json")

// dispatchPropertyFixture is where the generated frames the edge passes, and a sample of those it
// withholds, are recorded for the plugin's side of the property
// (packages/envoy-client/src/__tests__/dispatch-frame-property.test.ts), which renders them.
const dispatchPropertyFixture = "../../../contracts/fixtures/dispatch-frame-property.json"

// dispatchPropertyFrames is how many frames generatedDispatchFrames writes, and
// dispatchWithheldSample how many of those the edge withholds the fixture records, in order.
const (
	dispatchPropertyFrames = 100_000
	dispatchWithheldSample = 200
)

// dispatchPropertyRecord is the fixture: the generated frames the edge passes on the session's own
// agent subject for a token of PRIV, and the first dispatchWithheldSample frames it withholds there.
type dispatchPropertyRecord struct {
	Passed   []string `json:"passed"`
	Withheld []string `json:"withheld"`
}

// generatedDispatchFrames is 100,000 Dispatch frames drawn from PCG(5192, 11): each on the
// session's own agent subject, its role lane or the token's project's issue topic, its payload an
// event or a targeted frame whose project and issue key are drawn to split a reader of the
// payload's fields from what the plugin shows - case variants, escapes, repeats, a "__proto__" key,
// the token's project beside another project's nested event, a misspelt delivery line. It is the
// one generator of the property: the plugin's side reads the frames it judges from the fixture.
func generatedDispatchFrames() []string {
	r := rand.New(rand.NewPCG(5192, 11))
	pick := func(pool []string) string { return pool[r.IntN(len(pool))] }
	own := contracts.LegacyAgentSubject("ses_v")
	frames := make([]string, 0, dispatchPropertyFrames)
	for range dispatchPropertyFrames {
		projects := []string{`"PRIV"`, `"PUB"`, `""`, `"priv"`, `" PRIV"`, `"P\u0052IV"`, `"P\u0055B"`, `null`, `5`}
		issues := []string{`"PRIV-1"`, `"PUB-1"`, `"priv-1"`, `" PUB-1"`, `"PUB-01"`, `"PUB-1 "`, `"P\u0055B-1"`, `"P\u0052IV-1"`, `null`, `"PUB"`, `"PRIV-x"`, `"PRIV-1-2"`}
		var fields []string
		add := func(k, v string) { fields = append(fields, `"`+k+`":`+v) }
		switch r.IntN(5) {
		case 0:
		case 1, 2:
			add(pick([]string{"project", "project", "project", "Project", `pr\u006fject`}), pick(projects))
		default:
			add("project", pick(projects))
			if r.IntN(4) == 0 {
				add(pick([]string{"project", "PROJECT"}), pick(projects))
			}
		}
		switch r.IntN(5) {
		case 0:
		case 1, 2:
			add(pick([]string{"issue_key", "issue_key", "issue_key", "Issue_Key", `issue_k\u0065y`}), pick(issues))
		default:
			add("issue_key", pick(issues))
			if r.IntN(4) == 0 {
				add(pick([]string{"issue_key", "ISSUE_KEY"}), pick(issues))
			}
		}
		add("type", `"comment.created"`)
		add("actor", `{"kind":"person","id":"owner@example.com"}`)
		add("notify", `true`)
		add("payload", `{"id":"c-1","body":"BODY"}`)
		if r.IntN(12) == 0 {
			add("__proto__", `{"project":"PUB"}`)
		}
		r.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
		event := "{" + strings.Join(fields, ",") + "}"
		var payload string
		switch r.IntN(6) {
		case 0, 1:
			payload = event
		case 2, 3:
			payload = `{"event":` + event + `,"delivery":{"attempt":1,"mode":"steer"}}`
		case 4:
			payload = `{"delivery":{"attempt":1},"event":` + event + `,"project":` + pick(projects) + `}`
		default:
			payload = `{"event":` + event + `,"project":` + pick(projects) + `,"issue_key":` + pick(issues) + `}`
		}
		source := pick([]string{`"dispatch"`, `"dispatch"`, `"dispatch"`, `"d\u0069spatch"`})
		topic := pick([]string{own, own, contracts.RoleTopicPrefix + "legion-priv-priv-1-tester", "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"})
		quoted, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		frames = append(frames, `{"source":`+source+`,"event_id":"g","topic":"`+topic+`","payload":`+string(quoted)+`}`)
	}
	return frames
}

// TestEdgeFrameVerdictOnGeneratedDispatchFrames records the generated Dispatch frames
// (generatedDispatchFrames) the edge passes on the session's own agent subject for a token of
// PRIV, and the first frames it withholds there, for the plugin's side of the property to render:
// no card of a passed frame may show another project's event, and the withheld sample is that
// side's control that its oracle finds such cards (dispatch-frame-property.test.ts). The fixture
// must hold this tree's frames and verdicts; `go test ./cmd/listener -run
// TestEdgeFrameVerdictOnGeneratedDispatchFrames -update-dispatch-property` rewrites it.
func TestEdgeFrameVerdictOnGeneratedDispatchFrames(t *testing.T) {
	own := contracts.LegacyAgentSubject("ses_v")
	computed := dispatchPropertyRecord{Passed: []string{}, Withheld: []string{}}
	for _, frame := range generatedDispatchFrames() {
		if edgeFrameVerdict(nil, "ses_v", []string{"PRIV"}, own, []byte(frame)) == framePasses() {
			computed.Passed = append(computed.Passed, frame)
		} else if len(computed.Withheld) < dispatchWithheldSample {
			computed.Withheld = append(computed.Withheld, frame)
		}
	}
	if len(computed.Passed) == 0 || len(computed.Withheld) < dispatchWithheldSample {
		t.Fatalf("the edge passes %d of the %d generated frames and withholds %d of the %d sampled: the property would judge too little",
			len(computed.Passed), dispatchPropertyFrames, len(computed.Withheld), dispatchWithheldSample)
	}
	if *updateDispatchProperty {
		out, err := json.MarshalIndent(computed, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dispatchPropertyFixture, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: %d passed, %d withheld", dispatchPropertyFixture, len(computed.Passed), len(computed.Withheld))
		return
	}
	data, err := os.ReadFile(dispatchPropertyFixture)
	if err != nil {
		t.Fatalf("read the property fixture: %v", err)
	}
	var recorded dispatchPropertyRecord
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatalf("decode the property fixture: %v", err)
	}
	if !slices.Equal(recorded.Passed, computed.Passed) || !slices.Equal(recorded.Withheld, computed.Withheld) {
		t.Fatalf("the edge passes %d generated frames and the fixture records %d (withheld sample: %d computed, %d recorded, equal %t): the generator or the rule changed without the fixture; rerun with -update-dispatch-property",
			len(computed.Passed), len(recorded.Passed), len(computed.Withheld), len(recorded.Withheld), slices.Equal(recorded.Withheld, computed.Withheld))
	}
}
