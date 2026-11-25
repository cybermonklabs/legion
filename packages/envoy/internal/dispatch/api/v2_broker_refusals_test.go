package api

// v2_broker_refusals_test.go holds the refusals the /api/v2 credential routes declare for the
// secrets broker (brokerCall) to the refusals the broker's own source answers on the routes they
// call, and drives a refusal of each through both versions: /api/v2 answers it under the code it
// declares, with the broker's message and the members its refusal carries beside it, and /api/v1
// relays the broker's bytes (dispatch://INFRA-4071/spec R6 and R7; R3.1 for /api/v1).
//ci:builds packages/envoy ./cmd/cipher-refgen readBrokerReference runs `go run ./cmd/cipher-refgen` in the module
//ci:reads-set refgen_api readBrokerReference's cipher-refgen parses the broker's API package and every package its routes and bodies name (readAPI, apiPage)
//ci:reads packages/envoy/internal/cipher/config/*.go !**/*_test.go readBrokerReference's cipher-refgen parses it (configPage)
//ci:reads packages/envoy/cmd/cipherd/*.go !**/*_test.go readBrokerReference's cipher-refgen parses it (configPage)
//ci:reads packages/envoy/internal/cipher/helper/*.go !**/*_test.go readBrokerReference's cipher-refgen parses it (errorsPage, docConsts)
//ci:reads packages/envoy/cmd/cipher/*.go !**/*_test.go readBrokerReference's cipher-refgen parses it (errorsPage, docConsts)
//ci:reads packages/envoy/internal/cipher/proxy/*.go !**/*_test.go readBrokerReference's cipher-refgen parses it (errorsPage, checkRefusalWrites)

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// v2BrokerCalls is each /api/v2 credential route, "<method> <pattern>", with the calls of the
// secrets broker its handler makes.
var v2BrokerCalls = map[string][]brokerCall{
	"GET /api/v2/credential-grants":              {brokerGrantsCall},
	"DELETE /api/v2/credential-grants/{id}":      {brokerRevokeGrantCall},
	"GET /api/v2/credential-requests":            {brokerPendingCall},
	"PUT /api/v2/credential-requests/{id}/state": {brokerDecisionCall, brokerDismissalCall, brokerLookupCall},
	"POST /api/v2/machine-login-lookups":         {brokerLookupCall},
	"GET /api/v2/machine-logins":                 {brokerMachineLoginsCall},
	"DELETE /api/v2/machine-logins/{id}":         {brokerRevokeMachineLoginCall},
	"GET /api/v2/me/signins":                     {brokerSignInsCall},
	"DELETE /api/v2/me/signins/{id}":             {brokerRevokeSignInCall},
}

// TestTheBrokerCallsDeclareEveryRefusalTheBrokerAnswers holds every call the /api/v2 credential
// handlers make of the secrets broker to the refusals the broker answers on its routes, as its
// generated HTTP reference reads them from its source (readBrokerReference): each refusal the
// broker answers there is one the call declares, by the broker's code at the broker's status -
// its 404 NOT_FOUND as the object the path names, and its refusals of Dispatch's own token at
// the 502 or 503 of apicode's constants - and the call declares no other. A code the broker
// adds, renames or moves to another status fails it, until the call declares it as the broker
// answers it. Each member a refusal's body carries there beside code and error is one
// brokerRefusalV2 carries, so /api/v2 keeps it; answer_id, which the broker's answer middleware
// adds to every 4xx refusal (internal/cipher/answer) rather than its route's writer, is outside
// the reference and brokerRefusalV2 carries it too. Every /api/v2 route that relays to the broker
// is listed in v2BrokerCalls.
func TestTheBrokerCallsDeclareEveryRefusalTheBrokerAnswers(t *testing.T) {
	t.Parallel()
	reference := readBrokerReference(t)
	var calls []brokerCall
	for _, route := range slices.Sorted(maps.Keys(v2BrokerCalls)) {
		for _, call := range v2BrokerCalls[route] {
			if !slices.ContainsFunc(calls, func(listed brokerCall) bool { return slices.Equal(listed.Routes, call.Routes) }) {
				calls = append(calls, call)
			}
		}
	}
	for _, mismatch := range brokerCallMismatches(reference.Refusals, calls) {
		t.Error(mismatch)
	}
	kept := brokerRefusalMembers()
	for _, mismatch := range brokerMemberMismatches(reference.Members, calls, kept) {
		t.Error(mismatch)
	}
	for _, call := range calls {
		for _, route := range call.Routes {
			t.Logf("%s: a refusal's members beside code and error: %q, and answer_id on every 4xx refusal",
				route, slices.Sorted(maps.Keys(reference.Members[route])))
		}
	}
	if !kept["answer_id"] {
		t.Errorf("brokerRefusalV2 carries %q, and no answer_id, which the broker adds to every 4xx refusal", slices.Sorted(maps.Keys(kept)))
	}

	t.Run("every /api/v2 route that relays to the broker is listed", func(t *testing.T) {
		var relaying []string
		for _, route := range servedAt(apiV2, (&server{deps: Deps{TestHooksEnabled: true}}).routes()) {
			if slices.Contains(route.Refusals, brokerUnconfiguredRefusal) {
				relaying = append(relaying, route.Method+" "+route.Pattern)
			}
		}
		slices.Sort(relaying)
		if listed := slices.Sorted(maps.Keys(v2BrokerCalls)); !slices.Equal(relaying, listed) {
			t.Fatalf("the /api/v2 routes that relay to the broker are %q, and v2BrokerCalls lists %q", relaying, listed)
		}
	})

	t.Run("a code the broker adds, one it renames and one it moves fail it", func(t *testing.T) {
		route := brokerRevokeGrantCall.Routes[0]
		changed := map[string]bool{"418 GRANT_TEAPOT": true, "409 GRANT_SHIFTING": true, "403 ACCOUNT_TOKEN_EXPIRED": true}
		for refusal := range reference.Refusals[route] {
			if refusal != "409 GRANT_MOVING" && refusal != "401 ACCOUNT_TOKEN_EXPIRED" {
				changed[refusal] = true
			}
		}
		mismatches := brokerCallMismatches(map[string]map[string]bool{route: changed}, []brokerCall{brokerRevokeGrantCall})
		want := []string{"418 GRANT_TEAPOT", "409 GRANT_SHIFTING", "409 GRANT_MOVING", "403 ACCOUNT_TOKEN_EXPIRED", "401 ACCOUNT_TOKEN_EXPIRED"}
		for _, refusal := range want {
			if !slices.ContainsFunc(mismatches, func(mismatch string) bool { return strings.Contains(mismatch, refusal) }) {
				t.Errorf("mismatches %q name no %s", mismatches, refusal)
			}
		}
		if len(mismatches) != len(want) {
			t.Fatalf("a broker that adds 418 GRANT_TEAPOT, renames GRANT_MOVING to GRANT_SHIFTING and answers "+
				"ACCOUNT_TOKEN_EXPIRED at 403: mismatches %q, want one for each of %q", mismatches, want)
		}
	})

	t.Run("a member the broker adds to a refusal fails it", func(t *testing.T) {
		route := brokerDecisionCall.Routes[0]
		members := map[string]map[string]bool{route: {"DECIDE_ON_BROKER_PAGE approve_url": true, "DECIDE_ON_BROKER_PAGE page_title": true}}
		mismatches := brokerMemberMismatches(members, []brokerCall{brokerDecisionCall}, kept)
		if len(mismatches) != 1 || !strings.Contains(mismatches[0], "page_title") {
			t.Fatalf("a DECIDE_ON_BROKER_PAGE carrying page_title beside approve_url: mismatches %q, want page_title alone", mismatches)
		}
	})
}

// brokerCallMismatches names each refusal answered holds for a route of calls that its call does
// not declare, and each refusal a call declares that answered does not hold for its route.
// answered is the broker's refusals by route and then "<status> <code>" (readBrokerReference).
func brokerCallMismatches(answered map[string]map[string]bool, calls []brokerCall) []string {
	var mismatches []string
	for _, call := range calls {
		declared := map[string]bool{}
		for _, refusal := range call.Refusals {
			declared[fmt.Sprintf("%d %s", refusal.Code.Status(), refusal.Code.Name())] = true
		}
		for _, account := range brokerAccountRefusals {
			declared[fmt.Sprintf("%d %s", account.BrokerStatus, account.Refusal.Code.Name())] = true
		}
		if !call.Missing.Code.IsZero() {
			declared[fmt.Sprintf("%d %s", http.StatusNotFound, brokerNotFoundCode)] = true
		}
		for _, route := range call.Routes {
			refusals, served := answered[route]
			if !served {
				mismatches = append(mismatches, route+": the broker's reference lists no such route")
				continue
			}
			for _, refusal := range slices.Sorted(maps.Keys(refusals)) {
				if !declared[refusal] {
					mismatches = append(mismatches, fmt.Sprintf("%s answers %s, which its call does not declare", route, refusal))
				}
			}
			for _, refusal := range slices.Sorted(maps.Keys(declared)) {
				if !refusals[refusal] {
					mismatches = append(mismatches, fmt.Sprintf("%s: its call declares %s, which the broker does not answer there", route, refusal))
				}
			}
		}
	}
	return mismatches
}

// brokerMemberMismatches names each member a refusal's body carries on a route of calls, beside
// code and error, that kept does not hold. members is each route's "<code> <member>"
// (readBrokerReference); kept is brokerRefusalMembers.
func brokerMemberMismatches(members map[string]map[string]bool, calls []brokerCall, kept map[string]bool) []string {
	var mismatches []string
	for _, call := range calls {
		for _, route := range call.Routes {
			for _, carried := range slices.Sorted(maps.Keys(members[route])) {
				code, member, _ := strings.Cut(carried, " ")
				if !kept[member] {
					mismatches = append(mismatches, fmt.Sprintf("%s answers %s with the member %s, which brokerRefusalV2 does not carry", route, code, member))
				}
			}
		}
	}
	return mismatches
}

// brokerRefusalMembers are the members brokerRefusalV2 carries beside code and error, by their
// JSON names.
func brokerRefusalMembers() map[string]bool {
	members := map[string]bool{}
	typ := reflect.TypeFor[brokerRefusalV2]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "code" && name != "error" {
			members[name] = true
		}
	}
	return members
}

// brokerReference is what the secrets broker's generated HTTP reference says its routes refuse, by
// route, "<method> <pattern>" as its route table writes them: Refusals each "<status> <code>" it
// answers there, those its route lists and those its credential's class lists, which the broker
// answers before the route runs; and Members each "<code> <member>" of a refusal whose body
// carries more than code and error, by each member beyond those two.
type brokerReference struct {
	Refusals map[string]map[string]bool
	Members  map[string]map[string]bool
}

// readBrokerReference reads the broker's generated HTTP reference: cmd/cipher-refgen's api.md,
// which reads every refusal and its body from the broker's source (internal/cipher/api) with
// go/ast and runs none of it, so no broker has to run to say what it answers.
func readBrokerReference(t *testing.T) brokerReference {
	t.Helper()
	module, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	command := exec.Command("go", "run", "./cmd/cipher-refgen", filepath.Join(module, "..", ".."), out)
	command.Dir = module
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write the broker's reference with cmd/cipher-refgen: %v\n%s", err, output)
	}
	page, err := os.ReadFile(filepath.Join(out, "api.md"))
	if err != nil {
		t.Fatalf("read the broker's reference: %v", err)
	}
	reference, err := parseBrokerReference(string(page))
	if err != nil {
		t.Fatalf("read the broker's reference: %v", err)
	}
	return reference
}

// The lines of the broker's generated HTTP reference this reads: a row of a route's refusal
// table, "| 404 | `NOT_FOUND` | …"; an entry of a credential's refusal cell, "401 `PROOF_INVALID`";
// the heading of a refusal's richer body, "The `DECIDE_ON_BROKER_PAGE` refusal's body (JSON):";
// and a row of the table of that body's fields, "| `approve_url` | `string` | …".
var (
	brokerReferenceRow   = regexp.MustCompile("^\\| ([0-9]{3}) \\| `([A-Z0-9_]+)` \\|")
	brokerReferenceEntry = regexp.MustCompile("([0-9]{3}) `([A-Z0-9_]+)`")
	brokerReferenceBody  = regexp.MustCompile("^The `([A-Z0-9_]+)` refusal's body \\(JSON\\):$")
	brokerReferenceField = regexp.MustCompile("^\\| `([A-Za-z0-9_]+)` \\|")
)

// parseBrokerReference reads api.md (readBrokerReference): its Authentication table, a row a
// credential with its refusals in the last cell, and a section "### <method> <pattern>" for each
// route, naming its credential ("- Credential: …"), listing its refusals, and listing the fields
// of each refusal whose body carries more than code and error.
func parseBrokerReference(page string) (brokerReference, error) {
	credentials := map[string][]string{}
	reference := brokerReference{Refusals: map[string]map[string]bool{}, Members: map[string]map[string]bool{}}
	credentialOf := map[string]string{}
	section, route, richer := "", "", ""
	for line := range strings.SplitSeq(page, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			section, route = line, ""
		case strings.HasPrefix(line, "### "):
			route, richer = strings.TrimPrefix(line, "### "), ""
			reference.Refusals[route], reference.Members[route] = map[string]bool{}, map[string]bool{}
		case route != "":
			if label, named := strings.CutPrefix(line, "- Credential: "); named {
				credentialOf[route] = label
			} else if row := brokerReferenceRow.FindStringSubmatch(line); row != nil {
				reference.Refusals[route][row[1]+" "+row[2]] = true
			} else if body := brokerReferenceBody.FindStringSubmatch(line); body != nil {
				richer = body[1]
			} else if field := brokerReferenceField.FindStringSubmatch(line); field != nil && richer != "" {
				if field[1] != "code" && field[1] != "error" {
					reference.Members[route][richer+" "+field[1]] = true
				}
			} else if line != "" && !strings.HasPrefix(line, "|") {
				// Any other line ends the richer body's table: a request's or an answer's body, or
				// the fields of a nested object, whose members are none of the refusal's own.
				richer = ""
			}
		case section == "## Authentication" && strings.HasPrefix(line, "| ") && strings.HasSuffix(line, " |"):
			cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
			credentials[cells[0]] = []string{}
			for _, entry := range brokerReferenceEntry.FindAllStringSubmatch(cells[len(cells)-1], -1) {
				credentials[cells[0]] = append(credentials[cells[0]], entry[1]+" "+entry[2])
			}
		}
	}
	if len(reference.Refusals) == 0 {
		return brokerReference{}, errors.New("it lists no route")
	}
	for route, refusals := range reference.Refusals {
		refusedFirst, listed := credentials[credentialOf[route]]
		if !listed {
			return brokerReference{}, fmt.Errorf("%s takes the credential %q, which its Authentication table does not list", route, credentialOf[route])
		}
		for _, refusal := range refusedFirst {
			refusals[refusal] = true
		}
	}
	return reference, nil
}

// refusingBroker is a secrets broker that answers every call with the one refusal it was last
// told to, and records the route of each call it was sent since.
type refusingBroker struct {
	server     *httptest.Server
	mu         sync.Mutex
	status     int
	body       string
	retryAfter string
	called     []string
}

func newRefusingBroker(t *testing.T) *refusingBroker {
	t.Helper()
	broker := &refusingBroker{}
	broker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broker.mu.Lock()
		broker.called = append(broker.called, r.Method+" "+r.URL.Path)
		status, body, retryAfter := broker.status, broker.body, broker.retryAfter
		broker.mu.Unlock()
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(broker.server.Close)
	return broker
}

// refuse makes the broker answer every later call status and body, with retryAfter in its
// Retry-After where it is not empty, and forgets the calls it was sent so far.
func (b *refusingBroker) refuse(status int, body, retryAfter string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status, b.body, b.retryAfter, b.called = status, body, retryAfter, nil
}

// calls is the route of each call the broker was sent since refuse, in order.
func (b *refusingBroker) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.called)
}

// brokerRouteOf reports whether called, "<method> <path>", is a call of one of patterns, the
// broker's routes as its route table writes them, where a {wildcard} is any one segment.
func brokerRouteOf(patterns []string, called string) bool {
	method, path, _ := strings.Cut(called, " ")
	segments := strings.Split(path, "/")
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		patternMethod, patternPath, _ := strings.Cut(pattern, " ")
		patternSegments := strings.Split(patternPath, "/")
		if patternMethod != method || len(patternSegments) != len(segments) {
			return false
		}
		for i, segment := range patternSegments {
			if !strings.HasPrefix(segment, "{") && segment != segments[i] {
				return false
			}
		}
		return true
	})
}

// TestTheBrokersRefusalIsTypedOnV2AndRelayedVerbatimOnV1 drives one refusal of the secrets broker
// through each /api/v2 credential route, each arm of the state route's, and the /api/v1 route it
// stands beside. /api/v2 answers it brokerRefusalV2: under the code the route's call declares for
// the broker's status and code - the broker's own, for its 404 NOT_FOUND the object's - at that
// code's status, which is the broker's but for its refusals of Dispatch's own token (502, and 503
// for RATE_LIMITED, carrying the broker's Retry-After), a code the route's operation lists in
// x-refusal-codes; with the broker's message, and the broker's answer_id and approve_url where its
// refusal carries them, and no other member. A code, or a status, the call does not declare, and
// an answer that names no code, are 502 AGENT_SECRETS_REFUSAL_UNDECLARED naming what the broker
// answered. /api/v1 answers the broker's status and bytes, as it always did.
func TestTheBrokersRefusalIsTypedOnV2AndRelayedVerbatimOnV1(t *testing.T) {
	t.Parallel()
	broker := newRefusingBroker(t)
	handler := newCredentialTestHandler(t, broker.server.URL)
	operations := map[string]apiRoute{}
	for _, route := range servedAt(apiV2, (&server{deps: Deps{TestHooksEnabled: true}}).routes()) {
		operations[route.Method+" "+route.Pattern] = route
	}
	code := "ABCD-1234"
	for _, test := range []struct {
		name           string
		v2Route        string
		v2Path, v1Path string
		v2Body, v1Body any
		v1Method       string
		call           brokerCall
		status         int
		body           string
		retryAfter     string
		want           apicode.Code
		message        string
		members        map[string]any
	}{
		{
			name: "a grant list the broker refuses", v2Route: "GET /api/v2/credential-grants",
			v2Path: "/api/v2/credential-grants", v1Method: http.MethodGet, v1Path: "/api/v1/credential-grants?approver=me",
			call: brokerGrantsCall, status: http.StatusBadRequest,
			body: `{"code":"APPROVER_IS_GROUP","error":"approver names a person, not a group","answer_id":"answer-1"}`,
			want: apicode.ApproverIsGroup, message: "approver names a person, not a group", members: map[string]any{"answer_id": "answer-1"},
		},
		{
			name: "Dispatch's own token, expired", v2Route: "GET /api/v2/credential-grants",
			v2Path: "/api/v2/credential-grants", v1Method: http.MethodGet, v1Path: "/api/v1/credential-grants?approver=me",
			call: brokerGrantsCall, status: http.StatusUnauthorized,
			body: `{"code":"ACCOUNT_TOKEN_EXPIRED","error":"the caller's identity token has expired","answer_id":"answer-2"}`,
			want: apicode.AccountTokenExpired, message: "the caller's identity token has expired", members: map[string]any{"answer_id": "answer-2"},
		},
		{
			name: "a grant that keeps moving", v2Route: "DELETE /api/v2/credential-grants/{id}",
			v2Path: "/api/v2/credential-grants/grant1", v1Method: http.MethodPost, v1Path: "/api/v1/credential-grants/grant1/revoke",
			call: brokerRevokeGrantCall, status: http.StatusConflict,
			body: `{"code":"GRANT_MOVING","error":"the grant moved while it was revoked; send it again","answer_id":"answer-3"}`,
			want: apicode.GrantMoving, message: "the grant moved while it was revoked; send it again", members: map[string]any{"answer_id": "answer-3"},
		},
		{
			name: "a request list the broker refuses", v2Route: "GET /api/v2/credential-requests",
			v2Path: "/api/v2/credential-requests", v1Method: http.MethodGet, v1Path: "/api/v1/credential-requests?approver=me",
			call: brokerPendingCall, status: http.StatusBadRequest,
			body: `{"code":"STATE_INPUT","error":"state is \"pending\" or \"expired\"","answer_id":"answer-4"}`,
			want: apicode.StateInput, message: `state is "pending" or "expired"`, members: map[string]any{"answer_id": "answer-4"},
		},
		{
			name: "an approval of a secret request, its page kept", v2Route: "PUT /api/v2/credential-requests/{id}/state",
			v2Path: "/api/v2/credential-requests/rec1/state", v2Body: map[string]any{"state": "approved"},
			v1Method: http.MethodPost, v1Path: "/api/v1/credential-requests/rec1/approve",
			call: brokerDecisionCall, status: http.StatusForbidden,
			body: `{"code":"DECIDE_ON_BROKER_PAGE","error":"decide it on the broker's page: https://secrets.example.com/approve/rec1",` +
				`"approve_url":"https://secrets.example.com/approve/rec1","answer_id":"answer-5"}`,
			want: apicode.DecideOnBrokerPage, message: "decide it on the broker's page: https://secrets.example.com/approve/rec1",
			members: map[string]any{"approve_url": "https://secrets.example.com/approve/rec1", "answer_id": "answer-5"},
		},
		{
			name: "a deny whose code's lookup the broker refuses", v2Route: "PUT /api/v2/credential-requests/{id}/state",
			v2Path: "/api/v2/credential-requests/rec1/state", v2Body: map[string]any{"state": "denied", "code": code},
			v1Method: http.MethodPost, v1Path: "/api/v1/credential-requests/rec1/deny", v1Body: map[string]any{"code": code},
			call: brokerLookupCall, status: http.StatusBadRequest,
			body: `{"code":"INVALID_LOOKUP","error":"body must be one valid JSON object","answer_id":"answer-6"}`,
			want: apicode.InvalidLookup, message: "body must be one valid JSON object", members: map[string]any{"answer_id": "answer-6"},
		},
		{
			name: "a dismissal of a request still pending", v2Route: "PUT /api/v2/credential-requests/{id}/state",
			v2Path: "/api/v2/credential-requests/rec1/state", v2Body: map[string]any{"state": "dismissed"},
			v1Method: http.MethodPost, v1Path: "/api/v1/credential-requests/rec1/dismiss",
			call: brokerDismissalCall, status: http.StatusConflict,
			body: `{"code":"RECORD_PENDING","error":"the request can still be decided","answer_id":"answer-7"}`,
			want: apicode.RecordPending, message: "the request can still be decided", members: map[string]any{"answer_id": "answer-7"},
		},
		{
			name: "a code no machine login has", v2Route: "POST /api/v2/machine-login-lookups",
			v2Path: "/api/v2/machine-login-lookups", v2Body: map[string]any{"code": code},
			v1Method: http.MethodPost, v1Path: "/api/v1/credential-requests/machine-lookup", v1Body: map[string]any{"code": code},
			call: brokerLookupCall, status: http.StatusNotFound,
			body: `{"code":"NO_SUCH_CODE","error":"no pending machine login has this code","answer_id":"answer-8"}`,
			want: apicode.NoSuchCode, message: "no pending machine login has this code", members: map[string]any{"answer_id": "answer-8"},
		},
		{
			name: "a machine-login list the broker fails, a 5xx carrying no answer_id", v2Route: "GET /api/v2/machine-logins",
			v2Path: "/api/v2/machine-logins", v1Method: http.MethodGet, v1Path: "/api/v1/machine-logins",
			call: brokerMachineLoginsCall, status: http.StatusInternalServerError,
			body: `{"code":"INTERNAL","error":"list launcher credentials failed"}`,
			want: apicode.InternalError, message: "list launcher credentials failed",
		},
		{
			name: "a machine login the caller did not approve", v2Route: "DELETE /api/v2/machine-logins/{id}",
			v2Path: "/api/v2/machine-logins/cred1", v1Method: http.MethodPost, v1Path: "/api/v1/machine-logins/cred1/revoke",
			call: brokerRevokeMachineLoginCall, status: http.StatusForbidden,
			body: `{"code":"NOT_APPROVER","error":"not the approver of this machine login","answer_id":"answer-9"}`,
			want: apicode.NotApprover, message: "not the approver of this machine login", members: map[string]any{"answer_id": "answer-9"},
		},
		{
			name: "Dispatch's own account, past the broker's rate", v2Route: "GET /api/v2/me/signins",
			v2Path: "/api/v2/me/signins", v1Method: http.MethodGet, v1Path: "/api/v1/signins",
			call: brokerSignInsCall, status: http.StatusTooManyRequests, retryAfter: "7",
			body: `{"code":"RATE_LIMITED","error":"too many requests","answer_id":"answer-10"}`,
			want: apicode.RateLimited, message: "too many requests", members: map[string]any{"answer_id": "answer-10"},
		},
		{
			name: "a sign-in of anyone else's", v2Route: "DELETE /api/v2/me/signins/{id}",
			v2Path: "/api/v2/me/signins/signin1", v1Method: http.MethodPost, v1Path: "/api/v1/signins/signin1/revoke",
			call: brokerRevokeSignInCall, status: http.StatusNotFound,
			body: `{"code":"NOT_FOUND","error":"no sign-in of that person has that id","answer_id":"answer-11"}`,
			want: apicode.SigninNotFound, message: "no sign-in of that person has that id", members: map[string]any{"answer_id": "answer-11"},
		},
		{
			name: "a code the call does not declare", v2Route: "DELETE /api/v2/credential-grants/{id}",
			v2Path: "/api/v2/credential-grants/grant1", v1Method: http.MethodPost, v1Path: "/api/v1/credential-grants/grant1/revoke",
			call: brokerRevokeGrantCall, status: http.StatusConflict,
			body:    `{"code":"GRANT_SHIFTING","error":"a code a newer broker answers","answer_id":"answer-12"}`,
			want:    apicode.AgentSecretsRefusalUndeclared,
			message: "the secrets broker answered 409 GRANT_SHIFTING: a code a newer broker answers", members: map[string]any{"answer_id": "answer-12"},
		},
		{
			name: "a declared code at a status the call does not declare", v2Route: "DELETE /api/v2/machine-logins/{id}",
			v2Path: "/api/v2/machine-logins/cred1", v1Method: http.MethodPost, v1Path: "/api/v1/machine-logins/cred1/revoke",
			call: brokerRevokeMachineLoginCall, status: http.StatusConflict,
			body: `{"code":"NOT_APPROVER","error":"not the approver","answer_id":"answer-13"}`,
			want: apicode.AgentSecretsRefusalUndeclared, message: "the secrets broker answered 409 NOT_APPROVER: not the approver",
			members: map[string]any{"answer_id": "answer-13"},
		},
		{
			name: "a refusal of Dispatch's token at a status the call does not declare", v2Route: "POST /api/v2/machine-login-lookups",
			v2Path: "/api/v2/machine-login-lookups", v2Body: map[string]any{"code": code},
			v1Method: http.MethodPost, v1Path: "/api/v1/credential-requests/machine-lookup", v1Body: map[string]any{"code": code},
			call: brokerLookupCall, status: http.StatusUnauthorized,
			body:    `{"code":"ACCOUNT_NOT_LISTED","error":"this account is not in BROKER_API_ACCOUNTS","answer_id":"answer-14"}`,
			want:    apicode.AgentSecretsRefusalUndeclared,
			message: "the secrets broker answered 401 ACCOUNT_NOT_LISTED: this account is not in BROKER_API_ACCOUNTS",
			members: map[string]any{"answer_id": "answer-14"},
		},
		{
			name: "an answer that names no code", v2Route: "GET /api/v2/me/signins",
			v2Path: "/api/v2/me/signins", v1Method: http.MethodGet, v1Path: "/api/v1/signins",
			call: brokerSignInsCall, status: http.StatusBadGateway,
			body: `<html>502 Bad Gateway</html>`,
			want: apicode.AgentSecretsRefusalUndeclared, message: "the secrets broker answered 502 naming no code",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, _, _ := strings.Cut(test.v2Route, " ")
			broker.refuse(test.status, test.body, test.retryAfter)
			v2 := dispatchRequest(t, handler, method, test.v2Path, bodyOrEmpty(test.v2Body), "alice")
			if calls := broker.calls(); len(calls) == 0 || !brokerRouteOf(test.call.Routes, calls[0]) {
				t.Fatalf("%s %s called the broker at %q, want first a call of %q", method, test.v2Path, calls, test.call.Routes)
			}
			var answered map[string]any
			if err := json.Unmarshal(v2.Body.Bytes(), &answered); err != nil {
				t.Fatalf("%s %s answered %d %s, which is no JSON object: %v", method, test.v2Path, v2.Code, v2.Body.String(), err)
			}
			want := map[string]any{"code": test.want.Name(), "error": test.message}
			maps.Copy(want, test.members)
			if v2.Code != test.want.Status() || !maps.Equal(answered, want) {
				t.Fatalf("%s %s answered %d %s, want %d %v", method, test.v2Path, v2.Code, v2.Body.String(), test.want.Status(), want)
			}
			if retryAfter := v2.Header().Get("Retry-After"); retryAfter != test.retryAfter {
				t.Errorf("%s %s answered Retry-After %q, want the broker's %q", method, test.v2Path, retryAfter, test.retryAfter)
			}
			if listed := openAPIOperationCodes(operations[test.v2Route])[test.want.Name()]; listed != any(test.want.Status()) {
				t.Errorf("%s answered %d %s, which its x-refusal-codes lists at %v", test.v2Route, v2.Code, test.want.Name(), listed)
			}

			broker.refuse(test.status, test.body, test.retryAfter)
			v1 := dispatchRequest(t, handler, test.v1Method, test.v1Path, bodyOrEmpty(test.v1Body), "alice")
			if v1.Code != test.status || v1.Body.String() != test.body {
				t.Fatalf("%s %s answered %d %s, want the broker's %d %s byte for byte", test.v1Method, test.v1Path, v1.Code, v1.Body.String(), test.status, test.body)
			}
		})
	}
}

// bodyOrEmpty is body, or the empty JSON object where it is nil, which every credential route
// takes (featureOffRoutes).
func bodyOrEmpty(body any) any {
	if body == nil {
		return map[string]any{}
	}
	return body
}
