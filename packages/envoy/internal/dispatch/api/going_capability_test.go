package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// goingCapabilityUnset is the capability of a deployment that configured nothing the bar uses.
const goingCapabilityUnset = `{"answer":{"configured":false,"model":"","blocks":"unknown"},` +
	`"decision":{"configured":false,"model":"","reachable":false},` +
	`"session":{"configured":false},"label":{"configured":false}}`

// readGoingCapability reads GET /api/v1/going/capability as a signed-in person and answers its
// body, trimmed.
func readGoingCapability(t *testing.T, handler http.Handler) string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/going/capability", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("capability: status=%d body=%s", response.Code, response.Body.String())
	}
	return strings.TrimSpace(response.Body.String())
}

// systemOneCalls is a System One endpoint that counts the calls it is made and answers each with
// the three answers. It calls nothing and holds no credential.
type systemOneCalls struct {
	url   string
	calls atomic.Int32
}

func newSystemOneCalls(t *testing.T) *systemOneCalls {
	t.Helper()
	standIn := &systemOneCalls{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		standIn.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-latest","answers":{`+
			`"action":{"type":"choice","choice":"none","confidence":0.9},`+
			`"context":{"type":"choice","choice":"none","confidence":0.8},`+
			`"display":{"type":"choice","choice":"prose","confidence":0.9}},`+
			`"usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	t.Cleanup(server.Close)
	standIn.url = server.URL
	return standIn
}

// systemOneModel is name at the stand-in's endpoint, whose token is what token answers.
func (s *systemOneCalls) model(name string, token func(context.Context) (string, error)) agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.SystemOne, BaseURL: s.url, Token: token},
		ID:       name,
	}
}

// staticToken is a token that always reads.
func staticToken(context.Context) (string, error) { return "decision-bearer", nil }

// A deployment that configured nothing reads every part unset, blocks unknown, to any caller
// Dispatch admits and to nobody else.
func TestGoingCapabilityWithNothingConfiguredReadsEveryPartUnset(t *testing.T) {
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{})
	if got := readGoingCapability(t, handler); got != goingCapabilityUnset {
		t.Fatalf("with nothing configured the capability reads %s, want %s", got, goingCapabilityUnset)
	}
	if unauthenticated := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/capability", nil, ""); unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("with no credential: status=%d body=%s, want 401", unauthenticated.Code, unauthenticated.Body.String())
	}
}

// The four parts read apart: each one configured alone reads configured and leaves the other
// three unset, so a deployment that answers and cannot decide is told from one that does
// neither, and the session from the label. The models are named; their endpoints never are.
func TestGoingCapabilityReadsTheFourPartsApart(t *testing.T) {
	standIn := newSystemOneCalls(t)
	decider, measured := NewDecider(standIn.model("jev-latest", staticToken))
	if !measured {
		t.Fatal("NewDecider refused jev-latest, which the record holds")
	}
	for name, tc := range map[string]struct {
		options testServerOptions
		want    string
	}{
		"the answering model alone": {
			options: testServerOptions{answerer: &fakeAnswerer{}, answerModel: "chat-model"},
			want: `{"answer":{"configured":true,"model":"chat-model","blocks":"unknown"},` +
				`"decision":{"configured":false,"model":"","reachable":false},` +
				`"session":{"configured":false},"label":{"configured":false}}`,
		},
		"the decision model alone": {
			options: testServerOptions{decider: decider},
			want: `{"answer":{"configured":false,"model":"","blocks":"unknown"},` +
				`"decision":{"configured":true,"model":"jev-latest","reachable":true},` +
				`"session":{"configured":false},"label":{"configured":false}}`,
		},
		"the session alone": {
			options: testServerOptions{newIssueSession: "0199e2a0-1111-7000-8000-000000000001"},
			want: `{"answer":{"configured":false,"model":"","blocks":"unknown"},` +
				`"decision":{"configured":false,"model":"","reachable":false},` +
				`"session":{"configured":true},"label":{"configured":false}}`,
		},
		"the label alone": {
			options: testServerOptions{newIssueLabel: "from-the-bar"},
			want: `{"answer":{"configured":false,"model":"","blocks":"unknown"},` +
				`"decision":{"configured":false,"model":"","reachable":false},` +
				`"session":{"configured":false},"label":{"configured":true}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler, _, _, _ := newRegisteredTestServer(t, tc.options)
			got := readGoingCapability(t, handler)
			if got != tc.want {
				t.Fatalf("the capability reads %s, want %s", got, tc.want)
			}
			if strings.Contains(got, standIn.url) || strings.Contains(got, "decision-bearer") {
				t.Fatalf("the capability names an endpoint or a credential: %s", got)
			}
		})
	}
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("reading the capability made %d decision calls, want none", calls)
	}
}

// A decision model the committed record does not hold is no decision model: NewDecider builds
// none, the capability reads the decision unset, and a question asked is answered with no call to
// the decision endpoint. One the record holds reads configured, named, with the record's own
// thresholds, and unreachable while its token does not read.
func TestGoingCapabilityReadsAnUnmeasuredDecisionModelUnset(t *testing.T) {
	standIn := newSystemOneCalls(t)
	if decider, measured := NewDecider(standIn.model("unmeasured-model", staticToken)); measured || decider != nil {
		t.Fatalf("NewDecider(unmeasured-model) = %v, %v, want no decider", decider, measured)
	}
	answerer := &fakeAnswerer{frames: []going.Event{going.Text("An answer."), going.End(0, 0)}}
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, answerModel: "chat-model"})
	want := `{"answer":{"configured":true,"model":"chat-model","blocks":"unknown"},` +
		`"decision":{"configured":false,"model":"","reachable":false},` +
		`"session":{"configured":false},"label":{"configured":false}}`
	if got := readGoingCapability(t, handler); got != want {
		t.Fatalf("with an unmeasured decision model the capability reads %s, want %s", got, want)
	}
	rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
	asked := rig.ask(t, "alice", map[string]any{"words": "what is open?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	rig.awaitTerminal(t, decodeBody[askedRun](t, asked).ID)
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("an unmeasured decision model was called %d times, want none", calls)
	}

	measured, ok := going.DecisionRecord("jev-latest")
	if !ok {
		t.Fatal("the record holds no jev-latest")
	}
	var unreadable atomic.Bool
	decider, built := NewDecider(standIn.model("jev-latest", func(ctx context.Context) (string, error) {
		if unreadable.Load() {
			return "", errors.New("the token file is empty")
		}
		return staticToken(ctx)
	}))
	if !built {
		t.Fatal("NewDecider refused jev-latest, which the record holds")
	}
	if record := decider.Record(); record.ActionThreshold == nil || measured.ActionThreshold == nil ||
		*record.ActionThreshold != *measured.ActionThreshold || record.ChartShare != measured.ChartShare {
		t.Fatalf("the decider carries %+v, want the record's own entry %+v", record, measured)
	}
	decided, _, _, _ := newRegisteredTestServer(t, testServerOptions{decider: decider})
	reachable := `{"answer":{"configured":false,"model":"","blocks":"unknown"},` +
		`"decision":{"configured":true,"model":"jev-latest","reachable":true},` +
		`"session":{"configured":false},"label":{"configured":false}}`
	if got := readGoingCapability(t, decided); got != reachable {
		t.Fatalf("with jev-latest the capability reads %s, want %s", got, reachable)
	}
	unreadable.Store(true)
	unreachable := strings.Replace(reachable, `"reachable":true`, `"reachable":false`, 1)
	if got := readGoingCapability(t, decided); got != unreachable {
		t.Fatalf("with jev-latest's token unreadable the capability reads %s, want %s", got, unreachable)
	}
	if calls := standIn.calls.Load(); calls != 0 {
		t.Fatalf("reading the capability made %d decision calls, want none", calls)
	}
}

// answer.blocks follows the answerer: unknown while it knows nothing, true once a structured
// reply was accepted, false once its service refused one.
func TestGoingCapabilityBlocksFollowTheAnswerer(t *testing.T) {
	answerer := &fakeAnswerer{}
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, answerModel: "chat-model"})
	for _, tc := range []struct {
		blocks, known bool
		want          string
	}{
		{false, false, `"blocks":"unknown"`},
		{true, true, `"blocks":true`},
		{false, true, `"blocks":false`},
	} {
		answerer.blocks, answerer.known = tc.blocks, tc.known
		if got := readGoingCapability(t, handler); !strings.Contains(got, `"answer":{"configured":true,"model":"chat-model",`+tc.want+`}`) {
			t.Errorf("Blocks() = %v, %v: the capability reads %s, want %s", tc.blocks, tc.known, got, tc.want)
		}
	}
}

// TestStructuredOutputRefusedTellsTheGoingCapabilityNoBlocks: with the answerer a deployment runs
// against a chat service that answers 400 to the structured request, the answer still streams as
// prose and the capability then reads blocks false; against one that takes the schema it reads
// true. Before either run it reads unknown (c20's capability leg).
func TestStructuredOutputRefusedTellsTheGoingCapabilityNoBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		reply goingtest.Reply
		want  string
	}{
		"refused": {
			reply: goingtest.Reply{RefuseFormat: true, Final: &goingtest.Final{Answer: "Nothing is open.", Arguments: "{}"}},
			want:  `"blocks":false`,
		},
		"accepted": {
			reply: goingtest.Reply{Final: &goingtest.Final{Answer: "Nothing is open.", Arguments: "{}"}},
			want:  `"blocks":true`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			answerer, _ := newGoingModelAnswerer(t, tc.reply)
			handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: answerer, answerModel: "going-test-deployment"})
			if got := readGoingCapability(t, handler); !strings.Contains(got, `"blocks":"unknown"`) {
				t.Fatalf("before any run the capability reads %s, want blocks unknown", got)
			}
			rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
			asked := rig.ask(t, "alice", map[string]any{"words": "what is open?"})
			if asked.Code != http.StatusCreated {
				t.Fatalf("ask: status=%d body=%s", asked.Code, asked.Body.String())
			}
			frames := rig.awaitTerminal(t, decodeBody[askedRun](t, asked).ID)
			texts := 0
			for _, frame := range frames {
				if strings.Contains(frame, " "+contracts.GoingEventText+" ") {
					texts++
				}
			}
			if last := frames[len(frames)-1]; texts == 0 || !strings.Contains(last, " "+contracts.GoingEventEnd+" ") {
				t.Fatalf("the run stored %v, want the answer's text and an end", frames)
			}
			if got := readGoingCapability(t, handler); !strings.Contains(got, `"answer":{"configured":true,"model":"going-test-deployment",`+tc.want+`}`) {
				t.Fatalf("after the run the capability reads %s, want %s", got, tc.want)
			}
		})
	}
}
