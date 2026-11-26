package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// PUT /api/v2/credential-requests/{id}/state answers the request as the broker records it once
// decided, read by the code the caller typed. A code the broker reads as another record - a code
// a later login took, which machine.LookupByCode answers newest first - is no answer for this
// request, and an answer of the broker's Dispatch cannot read is Dispatch's own failure: each is a
// logged 500, never the other record nor the 503 of a broker out of reach.
func TestTheCredentialStateAnswersItsOwnRecordAlone(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodPut, "/credential-requests/{id}/state")
	approve := func(t *testing.T, record string) (int, string) {
		t.Helper()
		tour.secrets.answer(http.MethodPost, "/v1/credential-requests/"+record+"/approve", http.StatusOK,
			map[string]any{"credential_id": "tour-credential", "state": "approved"})
		response := tour.send(t, route, tourRequest{
			Path: map[string]string{"id": record}, Body: map[string]any{"state": "approved", "code": "ABCD-1234"}, As: tourPerson("alice"),
		}, "")
		code, _ := refusalOf(t, response)
		return response.Code, code
	}

	t.Run("a code that reads another record", func(t *testing.T) {
		tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, tourLoginRecord("tour-record-"+uuid.NewString(), "pending"))
		if status, code := approve(t, "tour-record-"+uuid.NewString()); status != http.StatusInternalServerError || code != "INTERNAL" {
			t.Fatalf("an approval whose code reads another record answered %d %s, want 500 INTERNAL", status, code)
		}
	})

	t.Run("a lookup Dispatch cannot read", func(t *testing.T) {
		tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, []string{"no record"})
		if status, code := approve(t, "tour-record-"+uuid.NewString()); status != http.StatusInternalServerError || code != "INTERNAL" {
			t.Fatalf("an approval whose lookup answered no record Dispatch reads answered %d %s, want 500 INTERNAL", status, code)
		}
	})
}

// PUT /api/v2/credential-requests/{id}/state answers a machine login the broker already holds in
// the state sent as it stands, relaying nothing, to the callers the broker would let decide it and
// to no other (recordSetter, record.MayDecide): anyone signed in for a service's login, whatever
// approver it stored, and otherwise the person its approver names. A person's machine login whose
// approver is anyone, which only an older broker opened, is the broker's to refuse to everybody,
// so its state sent again by any person is relayed, and the broker's 403 NOT_APPROVER answered.
// A login whose approver is a group is relayed the same way: Dispatch reads no group's members, so
// the broker, which does, decides it. A deny of a service's login is relayed too, since the
// broker refuses it to a person Dispatch calls a restricted member (#4848), which Dispatch cannot
// tell: the broker's 409 RECORD_TERMINAL for a login already denied, which it answers only a
// caller it admitted, is answered 200 the login as it stands, and its 403 NOT_APPROVER relayed.
// No broker-issued token holds approve (brokerTokenActor), so the approver's own broker token
// sending the state again is refused 403 OAUTH_SCOPE_REQUIRED naming approve at the state's rule,
// before the compare reads the record, and nothing is relayed or looked up, as /api/v1 refuses
// that token its approve.
func TestTheCredentialStateNoOpAdmitsWhomTheBrokerLetsDecide(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodPut, "/credential-requests/{id}/state")
	service := "legion-daemon"
	for _, test := range []struct {
		name, approver string
		service        *string
		caller         string
		unchanged      bool
	}{
		{"a person's login whose approver is anyone, sent by a person", "anyone", nil, "bob", false},
		{"a service's login, sent by a person other than its stored approver", "alice", &service, "bob", true},
		{"a person's login, sent by its approver", "alice", nil, "alice", true},
		{"a person's login whose approver is a group, sent by a person", "group:eng@example.com", nil, "alice", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := "tour-record-" + uuid.NewString()
			stored := tourLoginRecord(id, "approved")
			stored.Approver, stored.Service = test.approver, test.service
			tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, stored)
			approve := "/v1/credential-requests/" + id + "/approve"
			tour.secrets.answer(http.MethodPost, approve, http.StatusForbidden, map[string]string{
				"code": "NOT_APPROVER", "error": "not an approver of this record",
			})
			response := tour.send(t, route, tourRequest{
				Path: map[string]string{"id": id}, Body: map[string]any{"state": "approved", "code": "ABCD-1234"}, As: tourPerson(test.caller),
			}, "")
			relayed := tour.secrets.callsTo(http.MethodPost, approve)
			if test.unchanged {
				if answered := decodeBody[wire.RecordResponse](t, response); response.Code != http.StatusOK || answered.RecordID != id || relayed != 0 {
					t.Fatalf("%s's approval answered %d %s and was relayed %d times, want 200 with the request as it stands, relayed never",
						test.caller, response.Code, response.Body.String(), relayed)
				}
				return
			}
			if code, _ := refusalOf(t, response); response.Code != http.StatusForbidden || code != "NOT_APPROVER" || relayed != 1 {
				t.Fatalf("%s's approval answered %d %s and was relayed %d times, want the broker's 403 NOT_APPROVER, relayed once",
					test.caller, response.Code, response.Body.String(), relayed)
			}
		})
	}

	t.Run("a person's login, sent by its approver's broker token", func(t *testing.T) {
		caller := newBrokerCaller(t)
		const token = "a-tour-token-that-would-approve-again"
		tour.brokerTokens.Says(token, personScopedTokenAnswer(caller.jkt))
		id := "tour-record-" + uuid.NewString()
		stored := tourLoginRecord(id, "approved")
		stored.Approver = brokerTestPerson
		tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, stored)
		approve, lookups := "/v1/credential-requests/"+id+"/approve", tour.secrets.callsTo(http.MethodPost, "/v1/machine-logins/lookup")
		response := tour.serve(caller.presenting(t, route.Method, "/api/v2/credential-requests/"+id+"/state", token,
			map[string]any{"state": "approved", "code": "ABCD-1234"}))
		assertGateRefused(t, "the approval sent again by its approver's broker token", response, model.ScopeApprove, "state approved")
		if relayed, looked := tour.secrets.callsTo(http.MethodPost, approve),
			tour.secrets.callsTo(http.MethodPost, "/v1/machine-logins/lookup")-lookups; relayed != 0 || looked != 0 {
			t.Fatalf("the refused approval was relayed %d times and looked up %d times, want never", relayed, looked)
		}
	})

	t.Run("a service's login denied again", func(t *testing.T) {
		for _, broker := range []struct {
			name, code string
			status     int
		}{
			{"by a caller the broker admits", "RECORD_TERMINAL", http.StatusConflict},
			{"by a restricted member", "NOT_APPROVER", http.StatusForbidden},
		} {
			t.Run(broker.name, func(t *testing.T) {
				id := "tour-record-" + uuid.NewString()
				stored := tourLoginRecord(id, "denied")
				stored.Approver, stored.Service = "alice", &service
				tour.secrets.answer(http.MethodPost, "/v1/machine-logins/lookup", http.StatusOK, stored)
				deny := "/v1/credential-requests/" + id + "/deny"
				tour.secrets.answer(http.MethodPost, deny, broker.status, map[string]string{
					"code": broker.code, "error": "the broker's refusal",
				})
				response := tour.send(t, route, tourRequest{
					Path: map[string]string{"id": id}, Body: map[string]any{"state": "denied", "code": "ABCD-1234"}, As: tourPerson("bob"),
				}, "")
				relayed := tour.secrets.callsTo(http.MethodPost, deny)
				if broker.status == http.StatusConflict {
					if answered := decodeBody[wire.RecordResponse](t, response); response.Code != http.StatusOK || answered.RecordID != id || answered.State != "denied" || relayed != 1 {
						t.Fatalf("the deny sent again answered %d %s and was relayed %d times, want 200 with the login as it stands, relayed once",
							response.Code, response.Body.String(), relayed)
					}
					return
				}
				if code, _ := refusalOf(t, response); response.Code != http.StatusForbidden || code != "NOT_APPROVER" || relayed != 1 {
					t.Fatalf("the deny sent again answered %d %s and was relayed %d times, want the broker's 403 NOT_APPROVER, relayed once",
						response.Code, response.Body.String(), relayed)
				}
			})
		}
	})
}

// GET /api/v2/machine-logins answers each machine login's person as the secrets broker lists it
// (#4452): a person's pod login, the login of a pod whose Kubernetes account the broker's
// BROKER_POD_OPERATORS maps to a person, names that person, and every other login, a service's
// among them, answers person null. The page reads the broker's rows into its own type before it
// slices them, so a member that type lacked would never reach a client of /api/v2.
func TestAPersonsPodLoginNamesItsPersonOnV2(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodGet, "/machine-logins")
	tour.secrets.answer(http.MethodGet, "/v1/launcher-credentials", http.StatusOK,
		wire.LauncherCredentialsResponse{Credentials: tourMachineLogins("alice")})
	response := tour.send(t, route, tourRequest{As: tourPerson("alice")}, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v2/machine-logins answered %d %s, want 200", response.Code, response.Body.String())
	}
	persons := map[any]any{}
	for _, login := range decodeBody[page[map[string]any]](t, response).Items {
		person, named := login["person"]
		if !named {
			t.Errorf("GET /api/v2/machine-logins answered a login with no person: %v", login)
		}
		persons[login["credential_id"]] = person
	}
	if want := map[any]any{"tour-person-pod-login": "alice", "tour-service-login": nil}; !reflect.DeepEqual(persons, want) {
		t.Errorf("GET /api/v2/machine-logins answered the persons %v, want %v: %s", persons, want, response.Body.String())
	}
}

// GET /api/v2/asks?source=<kind>:<id> names its source by a whole uuid or a prefix of 8 characters
// or more, and any other source is 400 INVALID_QUERY, as its description says, rather than the 404
// of a source on no document.
func TestAnAskSourceThatIsNoIDIsRefused(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.requireMounted(t, http.MethodGet, "/asks")
	for _, source := range []string{"ask:abc", "comment:not-an-id"} {
		response := tour.send(t, route, tourRequest{Query: url.Values{"source": {source}}, As: tourPerson("alice")}, "")
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_QUERY" {
			t.Errorf("GET /api/v2/asks?source=%s answered %d %s, want 400 INVALID_QUERY", source, response.Code, response.Body.String())
		}
	}
}

// GET /api/v2/priorities carries each listed description's display tree under `Dispatch-Trees: 1`,
// as GET /api/v1/priorities does since #4835
// (TestAPriorityDescriptionNested4000DeepReadsBackAsATreeOfItsOwnText), so a page that reads the
// /api/v2 list parses no description either, and a read without the header carries none (treeAt).
// The description is emphasis nested 4,000 deep, the shape that cost the browser's parser the
// square of its depth: its tree is every character of it as text.
func TestAV2PriorityListCarriesEachDescriptionsTree(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	written := strings.Repeat("*a ", 4_000) + "b" + strings.Repeat(" c*", 4_000)
	priorityTreesRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": "nested-deep", "title": "Nested deep", "description": written}, http.StatusCreated)

	// The store starts with no priority (newTestHandlerWithStore), so the one created is item 0.
	requireTextTree(t, "the /api/v2 listing",
		model.DisplayTree(treeAt(t, handler, "/api/v2/priorities", "items.0.description_tree")), written)
}

// GET /api/v2/handoffs/{id}, and GET /api/v2/issues/{key}/handoffs read as the latest handoff of a
// phase (phase=, sort=-version, limit=1, include=body), answer under `Dispatch-Trees: 1` the
// field_trees GET /api/v1/handoffs/{id} and GET /api/v1/issues/{key}/handoffs/{phase}/latest answer
// (writeHandoff, handoffRead): the tree of each string the body's card draws, under its JSON
// Pointer, so a client that moves off /api/v1 to the route that answers for it loses no member.
// Each answer holds to the committed /api/v2 document (tourContract.hold), with the header and
// without it, and a read without the header carries no field_trees. On an issue with two versions
// of one phase and a handoff of another, the list with include=body carries on every row the
// field_trees of that handoff's /api/v1 read, and the phase's latest answers version 2 alone, with
// version 2's trees.
func TestAV2HandoffReadCarriesTheFieldTreesOfItsV1Read(t *testing.T) {
	tour := newRouteTour(t)
	contract := newTourContract(t)
	issue := tour.issue(t)
	body, err := json.Marshal(map[string]any{
		"summary": "Shipped the **parser**.", "count": 3,
		"proof": []any{"`go test ./...` passes.", map[string]string{"command": "go test"}},
	})
	if err != nil {
		t.Fatalf("encode the handoff body: %v", err)
	}
	created := sessionRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/handoffs/implement", map[string]any{
		"actor": map[string]any{"kind": "session", "id": tourRecipient}, "body": string(body),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("record the handoff: %d %s", created.Code, created.Body.String())
	}
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	alice := tourPerson("alice")
	for _, read := range []struct {
		name, pattern, v1, trees string
		request                  tourRequest
	}{
		{
			name: "one handoff by id", pattern: "/handoffs/{id}", v1: "/api/v1/handoffs/" + id, trees: "field_trees",
			request: tourRequest{Path: map[string]string{"id": id}, As: alice},
		},
		{
			name: "a phase's latest, with its body", pattern: "/issues/{key}/handoffs",
			v1: "/api/v1/issues/" + issue.Key + "/handoffs/implement/latest", trees: "items.0.field_trees",
			request: tourRequest{
				Path:  map[string]string{"key": issue.Key},
				Query: url.Values{"phase": {"implement"}, "sort": {"-version"}, "limit": {"1"}, "include": {"body"}}, As: alice,
			},
		},
	} {
		t.Run(read.name, func(t *testing.T) {
			route := tour.requireMounted(t, http.MethodGet, read.pattern)
			answered := func(trees bool) *httptest.ResponseRecorder {
				t.Helper()
				sending := tour.request(t, route, read.request, "")
				if trees {
					sending.Header.Set(displayTreesHeader, "1")
				}
				response := tour.serve(sending)
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s (trees=%v) answered %d %s, want 200", route.Pattern, trees, response.Code, response.Body.String())
				}
				contract.hold(t, route, response)
				return response
			}
			if bare := answered(false); strings.Contains(bare.Body.String(), "field_trees") {
				t.Errorf("GET %s without the header carries field_trees: %s", route.Pattern, bare.Body.String())
			}
			got, held := valueAt(t, answered(true), read.trees)
			if !held {
				t.Fatalf("GET %s with the header carries no %s", route.Pattern, read.trees)
			}
			want, _ := valueAt(t, treesRequest(t, tour.handler, read.v1, true), "field_trees")
			var gotTrees, wantTrees map[string]any
			if err := json.Unmarshal(got, &gotTrees); err != nil {
				t.Fatalf("decode the /api/v2 field_trees %s: %v", got, err)
			}
			if err := json.Unmarshal(want, &wantTrees); err != nil {
				t.Fatalf("decode the /api/v1 field_trees %s: %v", want, err)
			}
			if _, summary := wantTrees["/summary"]; !summary || len(wantTrees) != 2 {
				t.Fatalf("GET %s answers the field_trees %s, want the trees of /summary and /proof/0 alone", read.v1, want)
			}
			if !reflect.DeepEqual(gotTrees, wantTrees) {
				t.Errorf("GET %s answers the field_trees\n\t%s\nwant those GET %s answers\n\t%s", route.Pattern, got, read.v1, want)
			}
		})
	}

	// An issue with two versions of one phase and a handoff of another: the whole listing carries
	// each row's own trees, and the phase's latest is version 2 alone, with version 2's trees.
	t.Run("two versions of one phase and a handoff of another", func(t *testing.T) {
		issue := tour.issue(t)
		record := func(phase, summary string) string {
			t.Helper()
			body, err := json.Marshal(map[string]any{"summary": summary})
			if err != nil {
				t.Fatalf("encode the handoff body: %v", err)
			}
			created := sessionRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/handoffs/"+phase, map[string]any{
				"actor": map[string]any{"kind": "session", "id": tourRecipient}, "body": string(body),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("record the %s handoff: %d %s", phase, created.Code, created.Body.String())
			}
			return decodeBody[struct {
				ID string `json:"id"`
			}](t, created).ID
		}
		first := record("implement", "Wrote the **parser**.")
		second := record("implement", "Shipped the _parser_.")
		review := record("review", "Read the `parser`.")
		trees := func(raw json.RawMessage) map[string]any {
			t.Helper()
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode the field_trees %s: %v", raw, err)
			}
			return decoded
		}
		v1Trees := map[string]map[string]any{}
		for _, id := range []string{first, second, review} {
			raw, _ := valueAt(t, treesRequest(t, tour.handler, "/api/v1/handoffs/"+id, true), "field_trees")
			v1Trees[id] = trees(raw)
			if _, summary := v1Trees[id]["/summary"]; !summary {
				t.Fatalf("GET /api/v1/handoffs/%s answers the field_trees %s, want the tree of /summary", id, raw)
			}
		}
		if reflect.DeepEqual(v1Trees[first], v1Trees[second]) || reflect.DeepEqual(v1Trees[second], v1Trees[review]) {
			t.Fatalf("the three handoffs' /api/v1 field_trees are not each their own: %v", v1Trees)
		}
		route := tour.requireMounted(t, http.MethodGet, "/issues/{key}/handoffs")
		type listedHandoff struct {
			ID         string          `json:"id"`
			Version    int             `json:"version"`
			FieldTrees json.RawMessage `json:"field_trees"`
		}
		list := func(query url.Values) []listedHandoff {
			t.Helper()
			sending := tour.request(t, route, tourRequest{Path: map[string]string{"key": issue.Key}, Query: query, As: alice}, "")
			sending.Header.Set(displayTreesHeader, "1")
			response := tour.serve(sending)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s?%s answered %d %s, want 200", route.Pattern, query.Encode(), response.Code, response.Body.String())
			}
			contract.hold(t, route, response)
			return decodeBody[page[listedHandoff]](t, response).Items
		}

		listed := map[string]bool{}
		for _, item := range list(url.Values{"include": {"body"}}) {
			want, known := v1Trees[item.ID]
			if !known || listed[item.ID] {
				t.Fatalf("GET %s?include=body lists the handoff %s, want each of %s, %s and %s once", route.Pattern, item.ID, first, second, review)
			}
			listed[item.ID] = true
			if got := trees(item.FieldTrees); !reflect.DeepEqual(got, want) {
				t.Errorf("GET %s?include=body answers handoff %s's field_trees %s, want those GET /api/v1/handoffs/%s answers: %v",
					route.Pattern, item.ID, item.FieldTrees, item.ID, want)
			}
		}
		if len(listed) != 3 {
			t.Fatalf("GET %s?include=body lists %d of the issue's 3 handoffs", route.Pattern, len(listed))
		}

		latest := list(url.Values{"phase": {"implement"}, "sort": {"-version"}, "limit": {"1"}, "include": {"body"}})
		if len(latest) != 1 || latest[0].ID != second || latest[0].Version != 2 {
			t.Fatalf("GET %s?phase=implement&sort=-version&limit=1&include=body answers %+v, want version 2 (%s) alone", route.Pattern, latest, second)
		}
		raw, _ := valueAt(t, treesRequest(t, tour.handler, "/api/v1/issues/"+issue.Key+"/handoffs/implement/latest", true), "field_trees")
		if got, want := trees(latest[0].FieldTrees), trees(raw); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(want, v1Trees[second]) {
			t.Errorf("the phase's latest answers the field_trees %s, want version 2's, those GET /api/v1/issues/%s/handoffs/implement/latest answers: %s",
				latest[0].FieldTrees, issue.Key, raw)
		}
	})
}
