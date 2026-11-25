package claim

//ci:reads packages/contracts/fixtures/project-tokens.json TestProjectTokenIsTheShippedSpelling

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The six words a role travels as: the pane's `LEGION_ROLE`, the register response's `role`, the
// `workers` key in the state the plugin reads. A renamed word is a cross-language break, so the
// list is pinned here rather than left to the constant declarations.
func TestRoleConstantsAreTheWireWords(t *testing.T) {
	roles := []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}
	want := []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}
	if len(roles) != len(want) {
		t.Fatalf("roles: got %d constants, want %d", len(roles), len(want))
	}
	for i, role := range roles {
		if string(role) != want[i] {
			t.Errorf("role %d: got %q, want %q", i, role, want[i])
		}
	}
}

// The register/ready/exit wire is exchanged with the Oh My Pi plugin and mirrored by
// `packages/contracts/src/legion-api.ts`, so every member name here is a contract with another
// language. Marshalling is what pins them: a renamed Go field silently changes the wire unless a
// test reads the bytes.
func TestWireMemberNames(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "register request",
			value: RegisterRequest{
				BootToken:      "boot-1",
				SessionID:      "ses_architect_208",
				OmpSessionFile: "/state/sessions/ses_architect_208.json",
				AgentID:        "agent-1",
				PluginContract: 1,
			},
			want: `{"bootToken":"boot-1","sessionId":"ses_architect_208",` +
				`"ompSessionFile":"/state/sessions/ses_architect_208.json","agentId":"agent-1",` +
				`"pluginContract":1}`,
		},
		{
			name: "register response",
			value: RegisterResponse{
				ClaimToken:   "legion-omp-LEGION-208-architect",
				Tree:         "LEGION-208",
				Issue:        "LEGION-208",
				Role:         RoleArchitect,
				Generation:   3,
				Secret:       "sec-1",
				PromptAgents: []string{"plan-gap-analyst", "scout"},
			},
			want: `{"claimToken":"legion-omp-LEGION-208-architect","tree":"LEGION-208",` +
				`"issue":"LEGION-208","role":"architect","generation":3,"secret":"sec-1",` +
				`"promptAgents":["plan-gap-analyst","scout"]}`,
		},
		{
			name: "register response with no prompt agents",
			value: RegisterResponse{
				ClaimToken:   "legion-omp-LEGION-208-merger",
				Tree:         "LEGION-208",
				Issue:        "LEGION-208",
				Role:         RoleMerger,
				Generation:   1,
				Secret:       "sec-3",
				PromptAgents: []string{},
			},
			want: `{"claimToken":"legion-omp-LEGION-208-merger","tree":"LEGION-208",` +
				`"issue":"LEGION-208","role":"merger","generation":1,"secret":"sec-3",` +
				`"promptAgents":[]}`,
		},
		{
			name: "ready request",
			value: ReadyRequest{
				ClaimToken: "legion-omp-LEGION-208-tester",
				SessionID:  "ses_tester_208",
				Secret:     "sec-2",
				Generation: 4,
			},
			want: `{"claimToken":"legion-omp-LEGION-208-tester","sessionId":"ses_tester_208",` +
				`"secret":"sec-2","generation":4}`,
		},
		{
			name: "ready request with a capability report",
			value: ReadyRequest{
				ClaimToken: "legion-omp-LEGION-208-tester",
				SessionID:  "ses_tester_208",
				Secret:     "sec-2",
				Generation: 4,
				Capabilities: &CapabilityReport{
					MeasuredAt: time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC),
					ElapsedMs:  1200,
					Rows: []CapabilityRow{
						{Name: "subagents", OK: true, Detail: "4 agents"},
						{Name: "web-search", OK: false, Detail: "web_search is not among the active tools"},
					},
				},
			},
			want: `{"claimToken":"legion-omp-LEGION-208-tester","sessionId":"ses_tester_208",` +
				`"secret":"sec-2","generation":4,"capabilities":{"measuredAt":"2026-10-10T02:18:59Z",` +
				`"elapsedMs":1200,"rows":[{"name":"subagents","ok":true,"detail":"4 agents"},` +
				`{"name":"web-search","ok":false,"detail":"web_search is not among the active tools"}]}}`,
		},
		{
			name: "exit request",
			value: ExitRequest{
				ClaimToken: "legion-omp-LEGION-208-tester",
				SessionID:  "ses_tester_208",
				Secret:     "sec-2",
				Generation: 4,
				Reason:     "phase complete",
			},
			want: `{"claimToken":"legion-omp-LEGION-208-tester","sessionId":"ses_tester_208",` +
				`"secret":"sec-2","generation":4,"reason":"phase complete"}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(testCase.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("wire shape\n got: %s\nwant: %s", encoded, testCase.want)
			}
		})
	}
}

// A refusal read back off the wire is the same value the route answered: the plugin decides
// whether to exit from the status, and an operator reads the sentence.
func TestRefusalsCarryTheStatusAndSentenceTheRoutesAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		refusal     Refusal
		wantStatus  int
		wantMessage string
	}{
		{"invalid boot token", InvalidBootToken, http.StatusForbidden, "Invalid boot token"},
		{"stale generation", StaleGeneration, http.StatusConflict, "Stale generation"},
		{
			name:        "same agent refusal",
			refusal:     SameAgentRefusal,
			wantStatus:  http.StatusConflict,
			wantMessage: "Worker respawn must resume the same agent session",
		},
		{"invalid secret", InvalidSecret, http.StatusForbidden, "Invalid session secret"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.refusal.Status != testCase.wantStatus {
				t.Errorf("status: got %d, want %d", testCase.refusal.Status, testCase.wantStatus)
			}
			if testCase.refusal.Error() != testCase.wantMessage {
				t.Errorf("message: got %q, want %q", testCase.refusal.Error(), testCase.wantMessage)
			}
			encoded, err := json.Marshal(testCase.refusal)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			want := `{"error":"` + testCase.wantMessage + `"}`
			if string(encoded) != want {
				t.Fatalf("body\n got: %s\nwant: %s", encoded, want)
			}
		})
	}
}

// A handler wraps a refusal with the context of the request it refused; the caller still has to
// be able to ask which refusal it was, because the status it answers depends on it.
func TestAWrappedRefusalIsStillIdentifiable(t *testing.T) {
	wrapped := errors.Join(errors.New("register LEGION-208/tester"), SameAgentRefusal)
	if !errors.Is(wrapped, SameAgentRefusal) {
		t.Fatalf("errors.Is(wrapped, SameAgentRefusal): got false, want true")
	}
	if errors.Is(wrapped, StaleGeneration) {
		t.Fatalf("errors.Is(wrapped, StaleGeneration): got true, want false")
	}
}

// The project word every token and the private tmux server carry is the shipped daemon's
// (`legionProjectToken`, packages/contracts/src/legion-roles.ts): the operator's `project`
// lowercased, with everything outside [a-z0-9] dropped, so `sjawhar/legion` and `LEGION` name the
// same server either daemon would. Both read packages/contracts/fixtures/project-tokens.json, so the
// plugin, which compares its LEGION_PROJECT with the project this daemon reports, agrees with both.
func TestProjectTokenIsTheShippedSpelling(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "project-tokens.json"))
	if err != nil {
		t.Fatalf("read the project-token fixture: %v", err)
	}
	var fixture struct {
		Tokens  map[string]string `json:"tokens"`
		Refused []string          `json:"refused"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the project-token fixture: %v", err)
	}
	for project, want := range fixture.Tokens {
		got, err := ProjectToken(project)
		if err != nil || got != want {
			t.Errorf("ProjectToken(%q) = %q, %v; want %q", project, got, err, want)
		}
	}
	for _, project := range fixture.Refused {
		if _, err := ProjectToken(project); err == nil || !strings.Contains(err.Error(), "project") {
			t.Errorf("ProjectToken(%q) = %v, want a refusal naming project", project, err)
		}
	}
}

// An outbox key belongs to the project its part before the first `-` names, the whole key where it
// has none (KeyOfProject), read with either spelling of the project: the Dispatch key or the
// claim token's. A key of another project is not the project's, and neither is one whose project
// merely starts or ends with the same letters.
func TestKeyOfProjectAnswersNoForAKeyOfAnotherProject(t *testing.T) {
	for _, c := range []struct {
		project, key string
		want         bool
	}{
		{"LEGION", "LEGION-208", true},
		{"legion", "LEGION-208", true},
		{"LEGION", "LEGION", true},
		{"legion", "LEGION", true},
		{"LEGION", "OTHERPROJECT-9", false},
		{"legion", "OTHERPROJECT-9", false},
		{"LEGION", "OTHERPROJECT", false},
		{"LEGION", "LEGIONX-1", false},
		{"LEGION", "XLEGION-1", false},
		{"LEGION", "LEG-1", false},
		{"LEGION", "", false},
	} {
		if got := KeyOfProject(c.project, c.key); got != c.want {
			t.Errorf("KeyOfProject(%q, %q) = %t, want %t", c.project, c.key, got, c.want)
		}
	}
}

// A claim token is the Envoy role the agent claims and the name every file of its pane is filed
// under, so it is the shipped roleToken byte for byte (legion-roles.ts:48-55) — a token Envoy
// would refuse, or one the TypeScript plugin would spell differently, is a role nobody reaches.
func TestNewTokenIsTheShippedRoleToken(t *testing.T) {
	got, err := NewToken("legion", "LEGION-208", RoleTester)
	if err != nil || got != "legion-legion-legion-208-tester" {
		t.Fatalf("NewToken = %q, %v; want legion-legion-legion-208-tester", got, err)
	}
	for _, testCase := range []struct {
		name, project, issue string
		role                 Role
		wantFragment         string
	}{
		{"a lowercase issue key", "legion", "legion-208", RoleTester, `"legion-208"`},
		{"an issue key with no number", "legion", "LEGION", RoleTester, `"LEGION"`},
		{"a role outside the grammar", "legion", "LEGION-208", Role("Operator"), `"Operator"`},
		{"a project that is not a token", "Legion", "LEGION-208", RoleTester, `"Legion"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := NewToken(testCase.project, testCase.issue, testCase.role)
			if err == nil {
				t.Fatalf("NewToken = %q, want a refusal", token)
			}
			if !strings.Contains(err.Error(), testCase.wantFragment) {
				t.Errorf("the refusal does not name %s: %v", testCase.wantFragment, err)
			}
		})
	}
}

// A deployment defines roles of its own, so a claim can be on a role outside Legion's six: a name
// of Dispatch's role grammar for a step, `^[a-z][a-z0-9-]{0,31}$`. Its token has the shape of every
// role token, and the agent after the issue's part, the role's name, may hold a "-" of its own. The
// controller is no such role, and neither is a name outside the grammar.
func TestATokenOfADeploymentRole(t *testing.T) {
	if token, err := NewToken("acme", "ACME-1", Role("checker")); err != nil || token != "legion-acme-acme-1-checker" {
		t.Errorf("NewToken(acme, ACME-1, checker) = %q, %v; want legion-acme-acme-1-checker", token, err)
	}
	for token, want := range map[Token]struct {
		issue string
		agent Agent
	}{
		"legion-acme-acme-1-checker":      {"legion-acme-acme-1", "checker"},
		"legion-acme-acme-1-code-checker": {"legion-acme-acme-1", "code-checker"},
		"legion-acme-acme-1-tester":       {"legion-acme-acme-1", Agent(RoleTester)},
	} {
		if issue, agent, ok := token.Cut(); !ok || issue != want.issue || agent != want.agent {
			t.Errorf("%s.Cut() = %q, %q, %t; want %q, %q, true", token, issue, agent, ok, want.issue, want.agent)
		}
	}
	for _, token := range []Token{"legion-acme-acme-1-controller", "legion-acme-acme-1-Checker", "legion-acme-acme-1", "legion-acme-acme-1-"} {
		if issue, agent, ok := token.Cut(); ok {
			t.Errorf("%s.Cut() = %q, %q; want no agent", token, issue, agent)
		}
	}
	if !Token("legion-acme-controller").IsController() {
		t.Errorf("legion-acme-controller.IsController() = false, want the controller")
	}
	for _, role := range []Role{RoleController, "Checker", "a::b", "", "1checker", Role("c" + strings.Repeat("x", 32))} {
		if token, err := NewToken("acme", "ACME-1", role); err == nil {
			t.Errorf("NewToken(acme, ACME-1, %q) = %q, want a refusal", role, token)
		}
	}
}

// A claim can be on one of Legion's six or on any name of the role grammar but the controller's;
// IsBuiltin takes the six alone.
func TestARoleIsLegionsOrANameOfTheRoleGrammar(t *testing.T) {
	for _, role := range append(slices.Clone(Roles), "checker", "code-checker", "c", Role("c"+strings.Repeat("x", 31))) {
		if !IsRole(role) {
			t.Errorf("IsRole(%q) = false, want true", role)
		}
		if want := slices.Contains(Roles, role); IsBuiltin(role) != want {
			t.Errorf("IsBuiltin(%q) = %t, want %t", role, !want, want)
		}
	}
	for _, role := range []Role{RoleController, "", "Checker", "1checker", "-checker", "a::b", "code_checker", Role("c" + strings.Repeat("x", 32))} {
		if IsRole(role) {
			t.Errorf("IsRole(%q) = true, want false", role)
		}
		if IsBuiltin(role) {
			t.Errorf("IsBuiltin(%q) = true, want false", role)
		}
	}
}

// The project controller's claim, which the daemon launches under `controller: daemon`, is on the
// role `controller`: the word the operator's controller already registers as and its pane is told
// as LEGION_ROLE. It is no workflow role — it holds no issue — so IsRole refuses it, no NewToken
// builds a token of it on an issue, and its one token is ControllerToken.
func TestTheControllerRoleIsTheControllersOwnAndNoWorkflowRole(t *testing.T) {
	if RoleController != "controller" {
		t.Fatalf("RoleController = %q, want the wire word controller", RoleController)
	}
	if IsRole(RoleController) {
		t.Fatal("IsRole(RoleController) = true; the controller holds no issue, so it is no workflow role")
	}
	if token, err := NewToken("legion", "LEGION-208", RoleController); err == nil {
		t.Fatalf("NewToken on an issue as the controller = %q, want a refusal", token)
	}
	if !IsController(RoleController, "", "") {
		t.Fatal("IsController(controller, no issue, no tree) = false, want true")
	}
	for _, place := range []struct{ issue, tree string }{{"LEGION-208", ""}, {"", "LEGION-208"}, {"LEGION-208", "LEGION-208"}} {
		if IsController(RoleController, place.issue, place.tree) {
			t.Errorf("IsController(controller, %q, %q) = true; the controller's claim is on no issue", place.issue, place.tree)
		}
	}
	if IsController(RoleArchitect, "", "") {
		t.Error("IsController(architect, no issue, no tree) = true, want false")
	}
}

// A token names the controller or an agent: the controller's token is the controller's, which Cut
// does not split, and a workflow token's agent is the name it ends in. A token that only looks like
// the controller's is neither.
func TestATokenNamesTheControllerOrItsAgent(t *testing.T) {
	for token, want := range map[Token]Agent{
		"legion-legion-legion-208-tester":    "tester",
		"legion-legion-legion-208-architect": "architect",
	} {
		if _, agent, ok := token.Cut(); !ok || agent != want || token.IsController() {
			t.Errorf("%s names the agent %q (%t), controller %t; want the agent %q", token, agent, ok, token.IsController(), want)
		}
	}
	if !ControllerToken("legion").IsController() {
		t.Errorf("%s is not the controller's", ControllerToken("legion"))
	}
	for _, token := range []Token{"legion--controller", "legion-a-b-controller", "other-legion-controller", "legion-Legion-controller"} {
		if token.IsController() {
			t.Errorf("%s is the controller's, want none", token)
		}
	}
}

// A claim is one agent of an issue, and its token ends in the agent's name, which can be a name no
// role has (`implementer-2`). Cut gives that name whole, with the issue's part before it, and the
// token of a role is the token of the agent named for that role.
func TestATokenEndsInItsAgent(t *testing.T) {
	token, err := AgentToken("legion", "INFRA-12", Agent("implementer"))
	if err != nil || token != "legion-legion-infra-12-implementer" {
		t.Fatalf("AgentToken = %q, %v", token, err)
	}
	issue, agent, ok := Token("legion-legion-infra-12-implementer-2").Cut()
	if !ok || issue != "legion-legion-infra-12" || agent != "implementer-2" {
		t.Fatalf("Cut = %q %q %v, want the issue part and implementer-2", issue, agent, ok)
	}
	same, _ := NewToken("legion", "INFRA-12", RoleTester)
	if byAgent, _ := AgentToken("legion", "INFRA-12", Agent(RoleTester)); same != byAgent {
		t.Fatalf("NewToken %q and AgentToken %q differ for a role", same, byAgent)
	}
	if _, agent, ok := ControllerToken("legion").Cut(); ok {
		t.Errorf("Cut of the controller's token = %q, want none: it names no issue", agent)
	}
}

// NewToken is the token of a role's agent, and it still refuses a name that is no role: its callers
// read that refusal as "no claim of this key". A deployment's role can be any name of the role
// grammar, an agent's name such as `implementer-2` among them, so the controller's name is the one
// name of the grammar that is no role. AgentToken takes any name of the agent grammar but that one.
// The controller's token is told apart from an issue's by IsController, which reads no role.
func TestNewTokenKeepsItsRefusalOfANameThatIsNoRole(t *testing.T) {
	if token, err := NewToken("legion", "INFRA-12", RoleController); err == nil || err.Error() != `claim token: "controller" is not a role` {
		t.Errorf("NewToken(controller) = %q, %v; want the refusal of a name that is no role", token, err)
	}
	if token, err := AgentToken("legion", "INFRA-12", Agent(RoleController)); err == nil {
		t.Errorf("AgentToken(controller) = %q, want a refusal: the controller names no agent of an issue", token)
	}
	if token, err := AgentToken("legion", "INFRA-12", Agent("implementer-2")); err != nil || token != "legion-legion-infra-12-implementer-2" {
		t.Errorf("AgentToken(implementer-2) = %q, %v; want legion-legion-infra-12-implementer-2", token, err)
	}
	if !ControllerToken("legion").IsController() {
		t.Error("ControllerToken(legion).IsController() = false, want true")
	}
	if Token("legion-legion-infra-12-implementer").IsController() {
		t.Error("an issue's token IsController() = true, want false")
	}
}

// An agent's name has the role grammar of a deployment: a lowercase letter, then at most 31
// lowercase letters, digits or dashes. AgentToken refuses any other name, and the issue and the
// project as NewToken does.
func TestAgentTokenRefusesANameOutsideTheAgentGrammar(t *testing.T) {
	for _, testCase := range []struct {
		name, project, issue string
		agent                Agent
		wantFragment         string
	}{
		{"no name", "legion", "INFRA-12", "", `""`},
		{"a name that starts with a digit", "legion", "INFRA-12", "2-implementer", `"2-implementer"`},
		{"an uppercase name", "legion", "INFRA-12", "Implementer", `"Implementer"`},
		{"a name of 33 bytes", "legion", "INFRA-12", Agent("a" + strings.Repeat("b", 32)), `"a` + strings.Repeat("b", 32) + `"`},
		{"a lowercase issue key", "legion", "infra-12", "implementer", `"infra-12"`},
		{"a project that is not a token", "Legion", "INFRA-12", "implementer", `"Legion"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := AgentToken(testCase.project, testCase.issue, testCase.agent)
			if err == nil {
				t.Fatalf("AgentToken = %q, want a refusal", token)
			}
			if !strings.Contains(err.Error(), testCase.wantFragment) {
				t.Errorf("the refusal does not name %s: %v", testCase.wantFragment, err)
			}
		})
	}
	if token, err := AgentToken("legion", "INFRA-12", Agent("a"+strings.Repeat("b", 31))); err != nil {
		t.Errorf("AgentToken of a name of 32 bytes = %q, %v; want it taken", token, err)
	}
}
