package api

//ci:reads packages/contracts/schemas/dispatch-openapi-v2.json newTourContract (the path built runtime-relative ../../../../.., then opened: the committed /api/v2 document the route tour's contract subtest holds every answer to)

// routes_v2_tour_test.go is the route tour of dispatch://INFRA-4071: one step per /api/v2 route
// the rules of /api/v2 bind - every route but the rows of the INFRA-979 tree and the test hooks -
// each a request the route answers with a status the step names, on a server of its own
// (newRouteTour), and subtests that send every step again in the ways R1, R4, R7 and R8 say every
// route answers alike.
//
// Each group of routes registers its steps in a file of its own, routes_v2_tour_<group>_test.go,
// with an init function:
//
//	func init() { addTourSteps("asks", asksTourSteps) }
//
//	func asksTourSteps() []tourStep {
//		return []tourStep{{
//			Method: http.MethodGet, Pattern: "/asks/{id}", Status: http.StatusOK,
//			Setup: func(t *testing.T, tour *routeTour) tourRequest {
//				ask := tour.ask(t, tour.issue(t))
//				return tourRequest{Path: map[string]string{"id": ask}, As: tourPerson("alice")}
//			},
//			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.AskNotFound}},
//		}}
//	}
//
// so a group adds a file and edits no shared literal. The tour fails while a mounted route has no
// step (every-route-has-a-step).

import (
	"bytes"
	"cmp"
	"encoding/json"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
)

// tourStep is one call of one /api/v2 route the tour makes: the route, what the call needs made
// first, the request, and the status it is answered with.
type tourStep struct {
	// Method and Pattern name the route as its row writes it: its method and its version-free
	// pattern, "/issues/{key}", and "" for the route index at /api/v2 itself.
	Method  string
	Pattern string
	// Name tells apart two steps of one route; empty for a route's only step.
	Name string
	// Setup makes what the request names on the tour's database - an issue, a document, an ask -
	// tells the tour's fakes what to answer, and answers the request. It runs once for each
	// subtest that sends the step, so each send has objects of its own to change.
	Setup func(t *testing.T, tour *routeTour) tourRequest
	// Status is the status the request is answered with.
	Status int
	// Missing are the path values that name an object Dispatch stores (decision 11), each with a
	// value no stored object has and the 404 code the route answers it with; empty for a route
	// whose path names none. Never a {session_id}, the {id} of an agent mark or the
	// app-installation read's {owner}/{repo}, which keep today's answer (ruling A).
	Missing []tourMissing
	// NoCredential is the 401 code the route answers a request with no credential, where it is
	// not the identity layer's NO_IDENTITY: a route only a bearer may call answers its own.
	NoCredential apicode.Code
	// ReplayMoves are the members a route whose Replay answers the resource afresh answers as they
	// stand at the repeat rather than as its first answer gave them, which the replays subtest
	// leaves out of its comparison: each a path of member names from the top, joined by dots, `*`
	// standing for every element of a list (`recipients.*.message.deliveries`, a broadcast's
	// attempts, which run behind its first answer and which the route describes as they stand).
	// Empty for every other step, whose repeat answers its first answer whole.
	ReplayMoves []string
}

// tourRequest is the request a step sends.
type tourRequest struct {
	// Path is the value of each wildcard of the route's pattern.
	Path map[string]string
	// Query is the query the request carries; nil for none.
	Query url.Values
	// Body is a JSON body, any value json.Marshal takes, or a tourUpload for a multipart upload;
	// nil sends none.
	Body any
	// As is the credential the request carries.
	As tourCaller
}

// tourUpload is a multipart upload's body: its fields, and its file.
type tourUpload struct {
	Fields      map[string]string
	Filename    string
	ContentType string
	Content     []byte
}

// tourMissing is one path value that names a stored object, with a value no object has, and the
// code its route answers that value with.
type tourMissing struct {
	Param string
	Value string
	Code  apicode.Code
}

// tourCaller is the credential a step's request carries: a person signed in at a browser
// (Login), a bearer token (Token: a personal token, the shared agent token), or none at all.
type tourCaller struct {
	Login string
	Token string
}

// tourPerson is login signed in at a browser, holding every scope.
func tourPerson(login string) tourCaller { return tourCaller{Login: login} }

// tourBearer is a bearer token.
func tourBearer(token string) tourCaller { return tourCaller{Token: token} }

// tourGroups are the tour's steps by group, each group's added by its own file (addTourSteps).
var tourGroups = map[string]func() []tourStep{}

// addTourSteps adds a group's steps to the tour. A group added twice is a mistake in the files
// that name it, and panics naming it.
func addTourSteps(group string, steps func() []tourStep) {
	if _, added := tourGroups[group]; added {
		panic("the route tour's group " + group + " is added twice")
	}
	tourGroups[group] = steps
}

// route is the "<method> <pattern>" of the /api/v2 route the step calls.
func (step tourStep) route() string {
	return step.Method + " /api/v2" + step.Pattern
}

// name is the step's subtest name: its route, and its own name where it has one.
func (step tourStep) name() string {
	if step.Name == "" {
		return step.route()
	}
	return step.route() + ": " + step.Name
}

// tourSteps is every group's steps, sorted by route and name.
func tourSteps() []tourStep {
	var steps []tourStep
	for _, group := range slices.Sorted(maps.Keys(tourGroups)) {
		steps = append(steps, tourGroups[group]()...)
	}
	slices.SortStableFunc(steps, func(left, right tourStep) int { return strings.Compare(left.name(), right.name()) })
	return steps
}

// TestV2RouteTour walks every /api/v2 route the rules of /api/v2 bind, one step at least each,
// and holds each step to what every route answers alike:
//
//   - every-route-has-a-step: a mounted route with no step fails, so the tour covers the table.
//   - steps: each step is answered its status.
//   - contract: each step's answer holds to the response the committed /api/v2 document declares
//     for its operation at the status it was answered - every required member present, no member
//     the schema does not declare, a content type the document names - and a route no step
//     recorded an answer of fails (R6).
//   - unknown-keys: each step sent again with ?zz_undeclared=1 is answered 400 QUERY_KEY_UNKNOWN,
//     naming the key and every key the route declares, and writes nothing (R4).
//   - undeclared-members: each write step sent with a body member, or an upload's field, the
//     route does not declare is answered the status the step names, as it is without it (R4).
//   - missing-objects: each step whose path names a stored object, sent naming one that does not
//     exist, is answered 404 with that object's code (decision 11, R7).
//   - unauthenticated: each step of a route that is not public, sent with no credential, is
//     answered 401 with the identity layer's code.
//   - replays: each write step sent twice under one Idempotency-Key is answered the same body the
//     second time (the same JSON value, for a route whose Replay or Reread answers the resource
//     afresh, but the members its step names as answered as they stand, ReplayMoves), marked a
//     replay, with no second event; a route whose answer holds a secret answers the repeat 409
//     KEY_COMMITTED (R8).
func TestV2RouteTour(t *testing.T) {
	tour := newRouteTour(t)
	steps := tourSteps()

	t.Run("every-route-has-a-step", func(t *testing.T) {
		stepped := map[string]bool{}
		for _, step := range steps {
			stepped[step.route()] = true
		}
		var missing []string
		for _, route := range slices.Sorted(maps.Keys(tour.routes)) {
			if !stepped[route] {
				missing = append(missing, route)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%d of the %d /api/v2 routes have no step of the route tour:\n\t%s\n"+
				"Add each its step in its group's routes_v2_tour_<group>_test.go (addTourSteps).",
				len(missing), len(tour.routes), strings.Join(missing, "\n\t"))
		}
	})

	t.Run("steps", func(t *testing.T) {
		for _, step := range steps {
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := step.Setup(t, tour)
				if response := tour.send(t, route, request, ""); response.Code != step.Status {
					t.Fatalf("answered %d %s, want %d", response.Code, response.Body.String(), step.Status)
				}
			})
		}
	})

	t.Run("contract", func(t *testing.T) {
		contract := newTourContract(t)
		recorded := map[string]bool{}
		for _, step := range steps {
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := step.Setup(t, tour)
				response := tour.send(t, route, request, "")
				if response.Code != step.Status {
					t.Fatalf("answered %d %s, want %d", response.Code, response.Body.String(), step.Status)
				}
				recorded[step.route()] = true
				contract.hold(t, route, response)
			})
		}
		var unrecorded []string
		for _, route := range slices.Sorted(maps.Keys(tour.routes)) {
			if !recorded[route] {
				unrecorded = append(unrecorded, route)
			}
		}
		if len(unrecorded) > 0 {
			t.Errorf("%d of the %d /api/v2 routes recorded no answer to hold to %s:\n\t%s", len(unrecorded),
				len(tour.routes), OpenAPIV2DocumentPath, strings.Join(unrecorded, "\n\t"))
		}
	})

	t.Run("unknown-keys", func(t *testing.T) {
		for _, step := range steps {
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := step.Setup(t, tour)
				write := route.Method != http.MethodGet
				var before map[string]string
				if write {
					awaitBackgroundWrites(t, tour.served)
					before = databaseFingerprint(t, tour.database)
				}
				request.Query = withQueryValue(request.Query, "zz_undeclared", "1")
				response := tour.send(t, route, request, "")
				refusal := decodeBody[queryKeyUnknownBody](t, response)
				if want := declaredQueryKeys(route.Query); response.Code != http.StatusBadRequest || refusal.Code != "QUERY_KEY_UNKNOWN" ||
					refusal.Key != "zz_undeclared" || !slices.Equal(refusal.Declared, want) {
					t.Fatalf("answered %d %s, want 400 QUERY_KEY_UNKNOWN naming zz_undeclared and %v", response.Code, response.Body.String(), want)
				}
				if write {
					awaitBackgroundWrites(t, tour.served)
					assertUnchanged(t, step.name()+" with an undeclared key", before, databaseFingerprint(t, tour.database))
				}
			})
		}
	})

	t.Run("undeclared-members", func(t *testing.T) {
		for _, step := range steps {
			if step.Method == http.MethodGet {
				continue
			}
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := step.Setup(t, tour)
				if request.Body == nil {
					t.Skip("the step sends no body to add a member to")
				}
				request.Body = withUndeclaredMember(t, request.Body)
				if response := tour.send(t, route, request, ""); response.Code != step.Status {
					t.Fatalf("with a member the route does not declare: answered %d %s, want %d as without it",
						response.Code, response.Body.String(), step.Status)
				}
			})
		}
	})

	t.Run("missing-objects", func(t *testing.T) {
		for _, step := range steps {
			for _, missing := range step.Missing {
				t.Run(step.name()+" "+missing.Param, func(t *testing.T) {
					route := tour.stepRoute(t, step)
					if !slices.Contains(patternWildcards(route.Pattern), missing.Param) {
						t.Fatalf("names {%s}, which is no wildcard of %s", missing.Param, route.Pattern)
					}
					request := step.Setup(t, tour)
					if keepsItsAnswer(route, request, missing.Param) {
						t.Fatalf("names {%s} of %s, which names no object Dispatch stores: it keeps today's answer (ruling A)",
							missing.Param, route.Pattern)
					}
					request.Path = maps.Clone(request.Path)
					request.Path[missing.Param] = missing.Value
					response := tour.send(t, route, request, "")
					if code, _ := refusalOf(t, response); response.Code != http.StatusNotFound || code != missing.Code.Name() {
						t.Fatalf("naming {%s} %q: answered %d %s, want 404 %s", missing.Param, missing.Value,
							response.Code, response.Body.String(), missing.Code.Name())
					}
				})
			}
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		for _, step := range steps {
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				if route.Auth == authPublic {
					t.Skip("a public route answers a caller with no credential")
				}
				request := step.Setup(t, tour)
				request.As = tourCaller{}
				response := tour.send(t, route, request, "")
				want := cmp.Or(step.NoCredential, apicode.NoIdentity)
				if code, _ := refusalOf(t, response); response.Code != http.StatusUnauthorized || code != want.Name() {
					t.Fatalf("with no credential: answered %d %s, want 401 %s", response.Code, response.Body.String(), want.Name())
				}
			})
		}
	})

	t.Run("replays", func(t *testing.T) {
		for _, step := range steps {
			if step.Method == http.MethodGet {
				continue
			}
			t.Run(step.name(), func(t *testing.T) {
				route := tour.stepRoute(t, step)
				request := step.Setup(t, tour)
				key := uuid.NewString()
				first := tour.send(t, route, request, key)
				if first.Code != step.Status {
					t.Fatalf("the first send answered %d %s, want %d", first.Code, first.Body.String(), step.Status)
				}
				events := tour.eventCount(t, "", "")
				again := tour.send(t, route, request, key)
				sameAnswer := again.Body.String() == first.Body.String()
				if route.Replay != nil || route.Reread != nil {
					// A route with a Replay or a Reread answers a repeat with the resource as it
					// stands now (writeRefreshed), re-encoded, so its members may come back in
					// another order, and those the step names as moving since may hold what they
					// hold now.
					sameAnswer = jsonEqual(t, withoutMembers(t, again.Body.Bytes(), step.ReplayMoves),
						string(withoutMembers(t, first.Body.Bytes(), step.ReplayMoves)))
				}
				if route.AnswerHoldsSecret {
					if code, _ := refusalOf(t, again); again.Code != http.StatusConflict || code != "KEY_COMMITTED" {
						t.Fatalf("a repeat of a write whose answer holds a secret answered %d %s, want 409 KEY_COMMITTED",
							again.Code, again.Body.String())
					}
				} else if again.Code != first.Code || !replayed(again) || !sameAnswer {
					t.Fatalf("the repeat answered %d %s (replayed %v), want the first answer %d %s, replayed",
						again.Code, again.Body.String(), replayed(again), first.Code, first.Body.String())
				}
				if after := tour.eventCount(t, "", ""); after != events {
					t.Fatalf("the repeat appended %d events, want none", after-events)
				}
			})
		}
	})
}

// stepRoute is the mounted route step calls, looked up as every test of the tour looks one up
// (requireMounted).
func (tour *routeTour) stepRoute(t *testing.T, step tourStep) apiRoute {
	t.Helper()
	return tour.requireMounted(t, step.Method, step.Pattern)
}

// keepsItsAnswer reports whether param, a wildcard of route that request fills, names no object
// Dispatch stores, so a value no object has keeps today's answer rather than a 404 (ruling A,
// tourStep.Missing): a {session_id}, the {id} of an agent mark, and the app-installation read's
// {owner} and {repo}.
func keepsItsAnswer(route apiRoute, request tourRequest, param string) bool {
	switch {
	case param == "session_id":
		return true
	case param == "id" && route.Pattern == "/api/v2/me/marks/{kind}/{id}":
		return request.Path["kind"] == "agent"
	case param == "owner" || param == "repo":
		return strings.HasSuffix(route.Pattern, "/app-installation")
	}
	return false
}

// send carries request to route through the tour's server, under the Idempotency-Key key where
// one is given.
func (tour *routeTour) send(t *testing.T, route apiRoute, request tourRequest, key string) *httptest.ResponseRecorder {
	t.Helper()
	return tour.serve(tour.request(t, route, request, key))
}

// request is the HTTP request send carries: request to route, under the Idempotency-Key key where
// one is given. It fails t for a request it cannot build, so a test that sends on a goroutine of
// its own builds the request here, on the test's goroutine, and serves it there (serve).
func (tour *routeTour) request(t *testing.T, route apiRoute, request tourRequest, key string) *http.Request {
	t.Helper()
	target := route.Pattern
	for _, name := range patternWildcards(route.Pattern) {
		value, named := request.Path[name]
		if !named {
			t.Fatalf("the request names no {%s} of %s", name, route.Pattern)
		}
		target = strings.Replace(target, "{"+name+"}", url.PathEscape(value), 1)
		target = strings.Replace(target, "{"+name+"...}", value, 1)
	}
	if len(request.Query) > 0 {
		target += "?" + request.Query.Encode()
	}
	var body *bytes.Buffer
	contentType := "application/json"
	switch sent := request.Body.(type) {
	case nil:
		body = &bytes.Buffer{}
	case tourUpload:
		upload, uploadType, err := apitest.MultipartUpload(sent.Fields, sent.Filename, sent.ContentType, sent.Content)
		if err != nil {
			t.Fatal(err)
		}
		body, contentType = upload, uploadType
	default:
		encoded, err := json.Marshal(sent)
		if err != nil {
			t.Fatalf("encode the request's body: %v", err)
		}
		body = bytes.NewBuffer(encoded)
	}
	sending := httptest.NewRequest(route.Method, target, body)
	sending.Header.Set("Content-Type", contentType)
	if request.As.Login != "" {
		sending.Header.Set("X-Dispatch-User", request.As.Login)
	}
	if request.As.Token != "" {
		sending.Header.Set("Authorization", "Bearer "+request.As.Token)
	}
	if key != "" {
		sending.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	return sending
}

// serve carries sending through the tour's server. It fails no test, so it may run on any
// goroutine.
func (tour *routeTour) serve(sending *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	tour.handler.ServeHTTP(response, sending)
	return response
}

// withQueryValue is query with one more key and value, query itself left as it was.
func withQueryValue(query url.Values, key, value string) url.Values {
	with := url.Values{}
	for name, values := range query {
		with[name] = slices.Clone(values)
	}
	with.Add(key, value)
	return with
}

// refusalBody is a tourRefusal's change that sets the member name of its step's JSON body to
// value, the step's own body left as it was.
func refusalBody(name string, value any) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(_ *testing.T, _ *routeTour, request tourRequest) tourRequest {
		body := maps.Clone(request.Body.(map[string]any))
		body[name] = value
		request.Body = body
		return request
	}
}

// brokerRefuses makes the tour's secrets broker refuse method and path with code at status, as the
// broker writes a refusal: {code, error}, and the answer_id its answer middleware adds to a 4xx
// one (internal/cipher/answer). brokerAnswers says where and for how long.
func brokerRefuses(method, path, code string, status int) func(*testing.T, *routeTour, tourRequest) tourRequest {
	body := map[string]string{"code": code, "error": "the secrets broker's refusal"}
	if status < http.StatusInternalServerError {
		body["answer_id"] = "tour-broker-answer"
	}
	return brokerAnswers(method, path, status, body)
}

// brokerAnswers makes the tour's secrets broker answer method and path, its {id} the request's own
// id, with status and body, for the length of the subtest: what it answered there before is put
// back after, as withoutDeliverySettings puts the delivery settings back.
func brokerAnswers(method, path string, status int, body map[string]string) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
		called := strings.ReplaceAll(path, "{id}", request.Path["id"])
		secrets, route := tour.secrets, method+" "+called
		secrets.mu.Lock()
		before, answered := secrets.answers[route]
		secrets.mu.Unlock()
		secrets.answer(method, called, status, body)
		t.Cleanup(func() {
			secrets.mu.Lock()
			defer secrets.mu.Unlock()
			if answered {
				secrets.answers[route] = before
				return
			}
			delete(secrets.answers, route)
		})
		return request
	}
}

// The markdown of the tour's refusals of what a write puts into a document (tourRefusals): a check
// block carrying ci_check with no negative control, which breaks a rule of a check CI proves; an
// ask whose question holds a code block, which breaks an ask's content rule; and HTML, which is no
// Proof document.
const (
	tourCICheckBroken   = ":::check{#c1 ci_check=\"go-test\"}\nThe server builds at the head.\n:::\n"
	tourAskBlockBroken  = "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n```\ncode\n```\n:::\n\nAfter.\n"
	tourMarkdownRefused = "<details>x</details>"
)

// refusalDocumentAnchor is a tourRefusal's change that anchors its step's comment or ask on the
// document the path's {id} names, the anchor's member name set to value: a quote or a mark_id.
func refusalDocumentAnchor(name string, value any) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
		return refusalBody("anchor", map[string]any{"artifact": request.Path["id"], name: value})(t, tour, request)
	}
}

// refusalStaleIssueUpload and refusalStaleProjectUpload are a tourRefusal's change that first
// stores the document its step's JSON upload names, under the issue or the project the path's
// {key} names, and then names as the upload's precondition a token that document never had, so
// the upload replaces a document whose token it does not hold.
var (
	refusalStaleIssueUpload = refusalStaleUpload(func(t *testing.T, tour *routeTour, key, name string) {
		tour.issueDocument(t, key, name, "# Findings\n\nWhat the tour found first.\n")
	})
	refusalStaleProjectUpload = refusalStaleUpload(func(t *testing.T, tour *routeTour, key, name string) {
		createProjectDocument(t, tour.handler, key, name, "# Design\n\nThe tour's first design.\n")
	})
)

// refusalStaleUpload is the change of refusalStaleIssueUpload and refusalStaleProjectUpload, the
// document stored through store.
func refusalStaleUpload(store func(t *testing.T, tour *routeTour, key, name string)) func(*testing.T, *routeTour, tourRequest) tourRequest {
	return func(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
		t.Helper()
		store(t, tour, request.Path["key"], request.Body.(map[string]any)["name"].(string))
		return refusalBody("precondition", map[string]any{"document": "sha256:" + strings.Repeat("0", 64)})(t, tour, request)
	}
}

// withoutMembers is the JSON value body with each member paths names taken out (tourStep's
// ReplayMoves), and body itself where paths names none.
func withoutMembers(t *testing.T, body []byte, paths []string) json.RawMessage {
	t.Helper()
	if len(paths) == 0 {
		return body
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode the answer %s: %v", body, err)
	}
	for _, path := range paths {
		value = withoutMember(value, strings.Split(path, "."))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	return encoded
}

// withoutMember is value with the member path names taken out, every element of a list where a
// name is `*`; a path that names nothing value holds leaves it as it is.
func withoutMember(value any, path []string) any {
	switch node := value.(type) {
	case map[string]any:
		if len(path) == 1 {
			delete(node, path[0])
		} else if child, held := node[path[0]]; held {
			node[path[0]] = withoutMember(child, path[1:])
		}
	case []any:
		if path[0] == "*" && len(path) > 1 {
			for index := range node {
				node[index] = withoutMember(node[index], path[1:])
			}
		}
	}
	return value
}

// withUndeclaredMember is body with a member no route declares, zz_undeclared: a JSON object's
// member, or an upload's field.
func withUndeclaredMember(t *testing.T, body any) any {
	t.Helper()
	if upload, ok := body.(tourUpload); ok {
		upload.Fields = maps.Clone(upload.Fields)
		if upload.Fields == nil {
			upload.Fields = map[string]string{}
		}
		upload.Fields["zz_undeclared"] = "1"
		return upload
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request's body: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatalf("the step's body %s is no JSON object to add a member to", encoded)
	}
	members["zz_undeclared"] = json.RawMessage(`{"deep":[1]}`)
	return members
}

// declaredQueryKeys is every key a route whose query is query declares, read off its query tags
// (an embedded struct's own included), and session, which every /api/v2 route declares, sorted:
// what a refusal of an undeclared key names.
func declaredQueryKeys(query any) []string {
	keys := []string{"session"}
	var collect func(typ reflect.Type)
	collect = func(typ reflect.Type) {
		for index := range typ.NumField() {
			field := typ.Field(index)
			tag, tagged := field.Tag.Lookup("query")
			if field.Anonymous && !tagged && field.Type.Kind() == reflect.Struct {
				collect(field.Type)
				continue
			}
			if key, _, _ := strings.Cut(tag, ","); tagged {
				keys = append(keys, key)
			}
		}
	}
	if query != nil {
		collect(reflect.TypeOf(query))
	}
	slices.Sort(keys)
	return keys
}

// tourContract is the committed /api/v2 document (OpenAPIV2DocumentPath), compiled to hold
// answers to: an OpenAPI 3.1 document's schemas are JSON Schema 2020-12, so each response
// schema is compiled where it stands in the document, its references resolved within it.
type tourContract struct {
	location string
	document map[string]any
	compiler *jsonschema.Compiler
	compiled map[string]*jsonschema.Schema
}

// newTourContract reads the committed /api/v2 document, which the emitter's own test holds to what
// the emitter writes, so an answer is held to what the clients' types say.
func newTourContract(t *testing.T) *tourContract {
	t.Helper()
	location, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "..", filepath.FromSlash(OpenAPIV2DocumentPath)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(location)
	if err != nil {
		t.Fatalf("open the committed /api/v2 document: %v", err)
	}
	defer file.Close()
	read, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatalf("read the committed /api/v2 document: %v", err)
	}
	document, isObject := read.(map[string]any)
	if !isObject {
		t.Fatalf("the committed /api/v2 document is no JSON object")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(location, document); err != nil {
		t.Fatalf("load the committed /api/v2 document: %v", err)
	}
	return &tourContract{location: location, document: document, compiler: compiler, compiled: map[string]*jsonschema.Schema{}}
}

// hold holds one answer of route to the response its operation declares at the answer's status:
// a status the operation declares, no body where that response has no content, a content type it
// names, and for a body the document models, the schema that content type carries.
func (contract *tourContract) hold(t *testing.T, route apiRoute, response *httptest.ResponseRecorder) {
	t.Helper()
	method, status := strings.ToLower(route.Method), strconv.Itoa(response.Code)
	declared := openAPIAt(contract.document, "paths", route.Pattern, method, "responses", status)
	if declared == nil {
		t.Fatalf("answered %s, a status the document declares no response of %s %s at", status, route.Method, route.Pattern)
	}
	body := response.Body.Bytes()
	content, _ := declared["content"].(map[string]any)
	if content == nil {
		if len(body) > 0 {
			t.Fatalf("answered %s with the body %s, and the document declares none at that status", status, body)
		}
		return
	}
	mediaType, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil {
		t.Fatalf("answered %s under the content type %q, which is no media type: %v", status, response.Header().Get("Content-Type"), err)
	}
	// The most specific key the answer's media type matches, as OpenAPI reads a response's content:
	// the type itself, then its range (text/*), then any type (*/*).
	key := ""
	for _, candidate := range []string{mediaType, strings.SplitN(mediaType, "/", 2)[0] + "/*", "*/*"} {
		if _, declared := content[candidate]; declared {
			key = candidate
			break
		}
	}
	named, held := content[key].(map[string]any)
	if !held {
		t.Fatalf("answered %s as %s, and the document declares %v at that status", status, mediaType, slices.Sorted(maps.Keys(content)))
	}
	if _, modelled := named["schema"]; !modelled {
		// Bytes Dispatch does not model: the document names their content type and nothing more.
		return
	}
	schema := contract.schemaAt(t, "paths", route.Pattern, method, "responses", status, "content", key, "schema")
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("answered %s %s, which is no JSON: %v", status, body, err)
	}
	if err := schema.Validate(value); err != nil {
		t.Errorf("the answer of %s %s at %s holds to no schema the document declares there:\n%v\nthe answer: %s",
			route.Method, route.Pattern, status, err, body)
	}
}

// schemaAt is the schema at path in the document, compiled once.
func (contract *tourContract) schemaAt(t *testing.T, path ...string) *jsonschema.Schema {
	t.Helper()
	var pointer strings.Builder
	for _, token := range path {
		pointer.WriteString("/" + url.PathEscape(strings.NewReplacer("~", "~0", "/", "~1").Replace(token)))
	}
	location := contract.location + "#" + pointer.String()
	if schema, compiled := contract.compiled[location]; compiled {
		return schema
	}
	schema, err := contract.compiler.Compile(location)
	if err != nil {
		t.Fatalf("compile the document's schema at %s: %v", pointer.String(), err)
	}
	contract.compiled[location] = schema
	return schema
}
