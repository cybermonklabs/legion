// project_scope_test.go drives the real server with an access token the broker scoped to one
// project, through the same stub broker and the same proof producer broker_token_test.go uses,
// and walks the route table for a mutating row the check cannot decide and for an address of a
// project's write that no write of this file drives.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestEveryMutatingRouteIsClassed: every mutating row of the table declares what its write
// belongs to, and every class names a row that is served. The sub-tests are the negative
// control: the same walk over a fixture holding an unclassed mutating row, and over one classed
// as a project's write whose address names no object, fails on each.
func TestEveryMutatingRouteIsClassed(t *testing.T) {
	t.Parallel()
	served := &server{deps: Deps{TestHooksEnabled: true}}
	routes := served.routes()
	if undecidable := served.undecidableWrites(routes); len(undecidable) > 0 {
		t.Fatalf("%d mutating row(s) the project check cannot decide:\n%s", len(undecidable), strings.Join(undecidable, "\n"))
	}
	mounted := make(map[string]bool, len(routes))
	for _, route := range routes {
		mounted[route.Method+" "+route.Pattern] = true
	}
	for name := range writeClasses {
		if !mounted[name] {
			t.Errorf("writeClasses names %q, which the table does not serve", name)
		}
	}

	t.Run("a mutating row with no class fails this walk", func(t *testing.T) {
		undecidable := served.undecidableWrites([]apiRoute{
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets", Auth: authAny}},
			{routeRow: routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets", Auth: authAny}},
		})
		if len(undecidable) != 1 || !strings.HasPrefix(undecidable[0], "POST /api/v1/widgets:") {
			t.Fatalf("the walk answered %q, want the unclassed POST named and the GET left alone", undecidable)
		}
	})

	t.Run("a project row whose address names no object fails it too", func(t *testing.T) {
		undecidable := served.undecidableWrites([]apiRoute{
			{routeRow: routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets/{id}/paint", Auth: authAny}, Scope: scopeProject},
		})
		if len(undecidable) != 1 || !strings.Contains(undecidable[0], "names no object") {
			t.Fatalf("the walk answered %q, want the unresolvable project row named", undecidable)
		}
	})
}

// projectFixture is one project with an object of every addressing kind the check resolves. A
// check result is recorded only by seedCheckResult, since recording one takes a Verification
// document that no other test of a fixture needs, so result is empty until then.
type projectFixture struct {
	key      string
	issue    string
	document string
	ask      string
	comment  string
	message  string
	result   string
}

// asPerson serves a request signed in as the test's person, the credential that carries no
// project.
func (r *brokerTokenRig) asPerson(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return r.serve(withPerson(brokerRequest(t, method, path, body, "", ""), brokerTestPerson))
}

// seedProject builds a project and, in it, an issue, a project document, an ask, a comment and
// an issue message: one object of each kind a resolver reads. A person writes them all, since a
// person's cookie carries no project and this check is a no-op for it.
func seedProject(t *testing.T, rig *brokerTokenRig, key, name string) projectFixture {
	t.Helper()
	created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": name})
	if created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, created.Code, created.Body.String())
	}
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": key, "title": name + " work", "spec": "# Spec\n\nA passage.",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create an issue in %s: status=%d body=%s", key, issue.Code, issue.Body.String())
	}
	fixture := projectFixture{key: key, issue: decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key}

	document := rig.asPerson(t, http.MethodPost, "/api/v1/projects/"+key+"/artifacts", map[string]any{
		"name": "notes.md", "content": "# Notes\n\nA project document.",
	})
	if document.Code != http.StatusCreated {
		t.Fatalf("upload a project document to %s: status=%d body=%s", key, document.Code, document.Body.String())
	}
	fixture.document = decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, document).Artifact.ID
	if fixture.document == "" {
		t.Fatalf("the uploaded project document has no id: %s", document.Body.String())
	}

	fixture.ask = seededID(t, rig, "ask", "/api/v1/issues/"+fixture.issue+"/asks", map[string]any{
		"question": "Which way for " + key + "?",
	})
	fixture.comment = seededID(t, rig, "comment", "/api/v1/issues/"+fixture.issue+"/comments", map[string]any{
		"body": "A remark on " + key + ".",
	})
	fixture.message = seededID(t, rig, "message", "/api/v1/issues/"+fixture.issue+"/messages", map[string]any{
		"body": "A note on " + key + ".",
	})
	return fixture
}

func seededID(t *testing.T, rig *brokerTokenRig, kind, path string, body any) string {
	t.Helper()
	response := rig.asPerson(t, http.MethodPost, path, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("seed a %s at %s: status=%d body=%s", kind, path, response.Code, response.Body.String())
	}
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
	if id == "" {
		t.Fatalf("the seeded %s has no id: %s", kind, response.Body.String())
	}
	return id
}

// seedCheckResult records one check result on the fixture's issue, against a Verification
// document a person uploads, and answers the fixture holding that result. The result is recorded
// as the broker's test session over the shared token, which proves no session and so records the
// one its body names: the session that recorded a result is one of the three callers its
// retraction admits (refuseRetractionBy), so a retraction by a token proving that session is the
// handler's to take.
func seedCheckResult(t *testing.T, rig *brokerTokenRig, fixture projectFixture) projectFixture {
	t.Helper()
	uploadDocument(t, rig.handler, "/api/v1/issues/"+fixture.issue, "verification.md", twoChecks)
	fixture.result = recordResultAsSession(t, rig.handler, fixture.issue, brokerTestSession,
		resultBody("k1", "pass", headCommit, nil)).ID
	return fixture
}

// addressedWrite is one write of one addressing kind, built against whichever project's objects
// it is pointed at, each id written in whichever spelling the matrix is running.
type addressedWrite struct {
	kind   string
	method string
	// prefix is the address projectResolverFor resolves this write's project through: the
	// object whose project the check reads. TestEveryProjectAddressHasAnAddressedWrite holds the
	// rows to every such address a mounted write has.
	prefix string
	path   func(p projectFixture, spell func(string) string) string
	body   any
	// handlerAnswer is the status the handler itself answers where this check lets the request
	// through, in the token's own project and in any other. 0 means any answer that is not this
	// check's refusal.
	handlerAnswer int
	// uuidAddressed is whether the path names its object by uuid, which is what gives the
	// matrix a second spelling to run (uuidSpellings).
	uuidAddressed bool
}

func addressedWrites() []addressedWrite {
	return []addressedWrite{
		{
			kind: "issue key", method: http.MethodPatch, prefix: "/api/v1/issues/{key}",
			path:          func(p projectFixture, _ func(string) string) string { return "/api/v1/issues/" + p.issue },
			body:          map[string]any{"title": "Retitled by a scoped token"},
			handlerAnswer: http.StatusOK,
		},
		{
			kind: "project key", method: http.MethodPost, prefix: "/api/v1/projects/{key}",
			path: func(p projectFixture, _ func(string) string) string {
				return "/api/v1/projects/" + p.key + "/artifacts"
			},
			body:          map[string]any{"name": "scoped.md", "content": "# Written by a scoped token"},
			handlerAnswer: http.StatusCreated,
		},
		{
			kind: "artifact id", method: http.MethodPost, prefix: "/api/v1/artifacts/{id}",
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/artifacts/" + spell(p.document) + "/asks"
			},
			body:          map[string]any{"question": "Is this document right?"},
			handlerAnswer: http.StatusCreated,
			uuidAddressed: true,
		},
		{
			kind: "ask id", method: http.MethodPost, prefix: "/api/v1/asks/{id}",
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/asks/" + spell(p.ask) + "/resolve"
			},
			body:          map[string]any{"kind": "resolved", "reason": "Answered elsewhere."},
			handlerAnswer: http.StatusOK,
			uuidAddressed: true,
		},
		{
			kind: "comment id", method: http.MethodPost, prefix: "/api/v1/comments/{id}",
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/comments/" + spell(p.comment) + "/resolve"
			},
			body:          map[string]any{},
			uuidAddressed: true,
		},
		{
			// A message on an issue that targets nobody: the handler's own 404 proves the request
			// reached it, where the project check's refusal is a 403.
			kind: "message id", method: http.MethodPost, prefix: "/api/v1/messages/{id}",
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/messages/" + spell(p.message) + "/deliveries"
			},
			body:          map[string]any{"delivery": "btw"},
			handlerAnswer: http.StatusNotFound,
			uuidAddressed: true,
		},
		{
			// The retraction of a result the token's own session recorded (seedCheckResult), one
			// of the three callers the route admits, so the handler takes it.
			kind: "check result id", method: http.MethodPost, prefix: "/api/v1/results/{id}",
			path: func(p projectFixture, spell func(string) string) string {
				return "/api/v1/results/" + spell(p.result) + "/retract"
			},
			body:          map[string]any{"reason": "The run was against the wrong commit."},
			handlerAnswer: http.StatusCreated,
			uuidAddressed: true,
		},
	}
}

// unexercisedProjectAddresses names every mounted project write, a mutating row classed
// scopeProject, whose pattern addresses no prefix one of writes names: an address the check
// resolves a project through that no write drives.
func unexercisedProjectAddresses(routes []apiRoute, writes []addressedWrite) []string {
	var unexercised []string
	for _, route := range routes {
		if route.Method == http.MethodGet || route.Scope != scopeProject {
			continue
		}
		if !slices.ContainsFunc(writes, func(write addressedWrite) bool {
			return write.prefix != "" && addresses(route.Pattern, write.prefix)
		}) {
			unexercised = append(unexercised, route.Method+" "+route.Pattern)
		}
	}
	return unexercised
}

// TestEveryProjectAddressHasAnAddressedWrite: every address a mounted project write resolves its
// project through is named by a row of addressedWrites, so
// TestAProjectScopedTokenWritesAnotherProjectsObjects drives an object of each kind the check
// resolves, and a kind added later fails here until a row drives it too. Each row names a prefix
// the check resolves a project through, and its path addresses that prefix. The sub-test is the
// negative control: the same walk without the check result's row names the retraction route.
func TestEveryProjectAddressHasAnAddressedWrite(t *testing.T) {
	t.Parallel()
	served := &server{deps: Deps{TestHooksEnabled: true}}
	routes := served.routes()
	probe := projectFixture{
		key: "KEY", issue: "KEY-1", document: "document", ask: "ask", comment: "comment", message: "message", result: "result",
	}
	for _, write := range addressedWrites() {
		if _, resolved := served.projectResolverFor(write.prefix); !resolved {
			t.Errorf("the %s write names the prefix %q, which the project check resolves no project through", write.kind, write.prefix)
		}
		literal, _, _ := strings.Cut(write.prefix, "{")
		if path := write.path(probe, func(id string) string { return id }); !strings.HasPrefix(path, literal) {
			t.Errorf("the %s write's path %s does not address its prefix %s", write.kind, path, write.prefix)
		}
	}
	if unexercised := unexercisedProjectAddresses(routes, addressedWrites()); len(unexercised) > 0 {
		t.Fatalf("%d project write(s) whose address no row of addressedWrites names:\n%s",
			len(unexercised), strings.Join(unexercised, "\n"))
	}

	t.Run("a project address no write names fails this walk", func(t *testing.T) {
		without := slices.DeleteFunc(addressedWrites(), func(write addressedWrite) bool {
			return write.prefix == "/api/v1/results/{id}"
		})
		if unexercised := unexercisedProjectAddresses(routes, without); !slices.Equal(unexercised, []string{"POST /api/v1/results/{id}/retract"}) {
			t.Fatalf("the walk without the check result's row answered %q, want the retraction route named", unexercised)
		}
	})
}

// uuidSpelling is one way of writing a uuid in a path, and what the handler behind the check
// answers for it where the write is the caller's own to make.
type uuidSpelling struct {
	name string
	// spell writes the id as this spelling.
	spell func(string) string
	// handlerAnswered is whether the handler's own answer is the one the write declares. Only
	// the canonical spelling is every handler's: a route that parses its id in Go before it
	// reads the row (requireUUIDPath) refuses an alternate spelling 400, which is its answer to
	// make and not this check's.
	handlerAnswered bool
}

// uuidSpellings are the spellings of one uuid a path may carry, which this check and the
// handler behind it must read the same way: both read the value the prefix layer resolved
// (resolveIDPrefixes), which for a spelling that is no id written short - every spelling here -
// is the path value as written. Postgres's uuid parser takes a hyphen after any group of four
// digits, so `a061-e3f2-…` is the same id as `a061e3f2-…` to every query a handler makes, while
// Go's uuid.Parse refuses it: a check that read ids with the second admitted a write to an
// object it had resolved to nothing (INFRA-264). The prefix layer reads neither parser and
// resolves nothing for such a spelling - no uuid starts with a hyphen after four digits - so it
// leaves the two reading one value, exactly as they did.
func uuidSpellings(write addressedWrite) []uuidSpelling {
	canonical := uuidSpelling{name: "canonical", spell: func(id string) string { return id }, handlerAnswered: true}
	if !write.uuidAddressed {
		return []uuidSpelling{canonical}
	}
	return []uuidSpelling{canonical, {name: "hyphens every four digits", spell: hyphenatedEveryFour}}
}

// hyphenatedEveryFour writes a uuid with a hyphen after each group of four digits, one of the
// spellings Postgres reads as that uuid and Go's parser does not.
func hyphenatedEveryFour(id string) string {
	digits := strings.ReplaceAll(id, "-", "")
	var spelled strings.Builder
	for group := 0; group*4 < len(digits); group++ {
		if group > 0 {
			spelled.WriteByte('-')
		}
		spelled.WriteString(digits[group*4 : min(group*4+4, len(digits))])
	}
	return spelled.String()
}

// TestAProjectScopedTokenWritesAnotherProjectsObjects: one real token, two projects. A credential
// that names one project writes another project's objects as a credential that names none does
// (INFRA-4799), so every addressing kind the check resolves reaches the handler in the other
// project and gets the handler's own answer, the one the same write gets in the token's own
// project, and never the project check's refusal. Each id-addressed kind runs under both
// spellings of its uuid, since the check and the handler have to read a path the same way.
func TestAProjectScopedTokenWritesAnotherProjectsObjects(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedCheckResult(t, rig, seedProject(t, rig, brokerTestProject, "Broker"))
	other := seedCheckResult(t, rig, seedProject(t, rig, "OTH", "Other"))

	for _, write := range addressedWrites() {
		for _, spelling := range uuidSpellings(write) {
			t.Run(write.kind+", "+spelling.name, func(t *testing.T) {
				for _, project := range []projectFixture{other, own} {
					answered := rig.serve(caller.presenting(t, write.method, write.path(project, spelling.spell), token, write.body))
					if answered.Code == http.StatusForbidden {
						if refusal := decodeBody[refusalAnswer](t, answered); refusal.Code == codeProjectScope {
							t.Fatalf("a write to %s was refused by the project check: %+v", project.key, refusal)
						}
					}
					if spelling.handlerAnswered && write.handlerAnswer != 0 && answered.Code != write.handlerAnswer {
						t.Fatalf("a write to %s: status=%d body=%s, want %d from the handler",
							project.key, answered.Code, answered.Body.String(), write.handlerAnswer)
					}
				}
			})
		}
	}
}

// TestATargetThatResolvesToNoProjectIsRefusedToAProjectScopedToken: the check fails closed, so
// a target it cannot resolve to a project is refused to a project-scoped token. No project holds
// such a target, or nothing does — an id nobody has, and an id that is no uuid in any spelling.
// A refusal names the project the credential names and no other, so it invents no project. It
// does not close the existence oracle. A write to an object that exists reaches its handler,
// and this refusal reads differently, so a scoped caller can tell an object that exists from an
// id nobody holds. Nothing is read out that way, since no GET route is project-scoped. What it
// learns is which of its own writes addressed something that exists.
//
// This replaces the assertion that such a target kept its handler's answer for every caller.
// It still does for a caller that names no project, which is every credential but a
// project-scoped broker token, and the second half holds that.
func TestATargetThatResolvesToNoProjectIsRefusedToAProjectScopedToken(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-addressing-nothing"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")

	const unknownUUID = "5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"an issue nobody has":   {http.MethodPatch, "/api/v1/issues/NONE-1", map[string]any{"title": "x"}},
		"an ask nobody has":     {http.MethodPost, "/api/v1/asks/" + unknownUUID + "/resolve", map[string]any{"kind": "resolved", "reason": "x"}},
		"an id that is no uuid": {http.MethodPost, "/api/v1/comments/not-a-uuid/resolve", map[string]any{}},
		"a project nobody has":  {http.MethodPost, "/api/v1/projects/NONE/artifacts", map[string]any{"name": "n.md", "content": "# N"}},
		"a document nobody has": {http.MethodPost, "/api/v1/artifacts/" + unknownUUID + "/asks", map[string]any{"question": "?"}},
		"a message nobody has":  {http.MethodPost, "/api/v1/messages/" + unknownUUID + "/deliveries", map[string]any{"delivery": "btw"}},
		"a comment nobody has, spelled with hyphens every four digits": {
			http.MethodPost, "/api/v1/comments/" + hyphenatedEveryFour(unknownUUID) + "/resolve", map[string]any{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			refused := rig.serve(caller.presenting(t, request.method, request.path, token, request.body))
			if refused.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403 from the project check", refused.Code, refused.Body.String())
			}
			refusal := decodeBody[refusalAnswer](t, refused)
			if refusal.Code != codeProjectScope {
				t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScope)
			}
			if !strings.Contains(refusal.Error, brokerTestProject) {
				t.Fatalf("refusal %q does not name the project the credential covers (%s)", refusal.Error, brokerTestProject)
			}
			if strings.Contains(refusal.Error, token) {
				t.Fatalf("the refusal repeats the token: %q", refusal.Error)
			}

			// A caller that names no project is untouched: the handler answers for itself, as it
			// always has.
			answered := rig.asPerson(t, request.method, request.path, request.body)
			if answered.Code < http.StatusBadRequest {
				t.Fatalf("a person's same call: status=%d body=%s, want the handler's own refusal",
					answered.Code, answered.Body.String())
			}
			if answered.Code == http.StatusForbidden {
				if blocked := decodeBody[refusalAnswer](t, answered); blocked.Code == codeProjectScope {
					t.Fatalf("a caller that names no project was refused by project scope: %+v", blocked)
				}
			}
		})
	}
}

// TestADeploymentWriteIsRefusedToAProjectScopedToken: a credential that names one project
// may not create another, and the refusal names the route rather than leaving the caller to
// guess. The same write by a person and by a personal token, neither of which names a project,
// is untouched.
func TestADeploymentWriteIsRefusedToAProjectScopedToken(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-create-a-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))

	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/projects", token, map[string]any{
		"key": "NEW", "name": "New",
	}))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a project create under a project-scoped token: status=%d body=%s, want 403", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[refusalAnswer](t, refused)
	if refusal.Code != codeProjectScopeDeployment {
		t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScopeDeployment)
	}
	if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "POST /api/v1/projects") {
		t.Fatalf("refusal %q names neither the project the credential covers nor the route", refusal.Error)
	}

	person := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "PRS", "name": "Person"})
	if person.Code != http.StatusCreated {
		t.Fatalf("a person creating a project: status=%d body=%s", person.Code, person.Body.String())
	}
}

// TestCredentialsThatNameNoProjectAreUnaffected: the check is a no-op for every credential that
// exists today. A person and a personal token write both projects, and the deployment-wide
// create too.
func TestCredentialsThatNameNoProjectAreUnaffected(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	for _, project := range []projectFixture{own, other} {
		comment := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+project.issue+"/comments", map[string]any{
			"body": "A person writes here.",
		})
		if comment.Code != http.StatusCreated {
			t.Fatalf("a person commenting on %s: status=%d body=%s", project.key, comment.Code, comment.Body.String())
		}
	}

	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[struct {
		Token string `json:"token"`
	}](t, minted).Token
	asToken := func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": "ses_personal_token"}
		return rig.serve(brokerRequest(t, method, path, body, "Bearer "+personal, ""))
	}

	for _, project := range []projectFixture{own, other} {
		comment := asToken(t, http.MethodPost, "/api/v1/issues/"+project.issue+"/comments", map[string]any{
			"body": fmt.Sprintf("A personal token writes on %s.", project.key),
		})
		if comment.Code != http.StatusCreated {
			t.Fatalf("a personal token commenting on %s: status=%d body=%s", project.key, comment.Code, comment.Body.String())
		}
	}
	created := asToken(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "TOK", "name": "Token"})
	if created.Code != http.StatusCreated {
		t.Fatalf("a personal token creating a project: status=%d body=%s", created.Code, created.Body.String())
	}
}

// TestAProjectScopedTokenCreatesAnIssueInAnotherProject: the one write whose project its address
// does not name, so its handler reads the project from the body. A credential that names one
// project creates an issue in another project as a credential that names none does
// (INFRA-4799), and in its own project as before.
func TestAProjectScopedTokenCreatesAnIssueInAnotherProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-creating-issues"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	seedProject(t, rig, "OTH", "Other")

	for _, project := range []string{"OTH", brokerTestProject} {
		created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
			"project": project, "title": "Filed into " + project, "spec": "# Spec",
		}))
		if created.Code != http.StatusCreated {
			t.Fatalf("creating an issue in %s: status=%d body=%s, want 201", project, created.Code, created.Body.String())
		}
		if issue := decodeBody[model.Issue](t, created); issue.Project != project || !strings.HasPrefix(issue.Key, project+"-") {
			t.Fatalf("the created issue is %s of project %q, want an issue of %s", issue.Key, issue.Project, project)
		}
	}

	// A project-scoped token's import that names no project is refused 400 INVALID_ISSUE, as a
	// person's is (TestExternalIssueRejectsAMissingProject): the create refuses it before the
	// scope check runs.
	unnamed := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"external": "owner/repo#1",
	}))
	if unnamed.Code != http.StatusBadRequest {
		t.Fatalf("importing an issue with no project: status=%d body=%s, want 400", unnamed.Code, unnamed.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, unnamed); refusal.Code != "INVALID_ISSUE" || !strings.Contains(refusal.Error, "project is required when external is set") {
		t.Fatalf("refusal = %+v, want INVALID_ISSUE naming project", refusal)
	}
}

// TestAProjectScopedTokenCommentsOnAnotherProjectsIssue: a comment on another project's issue is
// written as a credential that names no project writes it (INFRA-4799). The answer is the
// comment on that issue, and a person reading that issue's comments reads it, written by the
// session the token proved.
func TestAProjectScopedTokenCommentsOnAnotherProjectsIssue(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-commenting-across-projects"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTH", "Other")

	const body = "A remark from a token of the BRK project."
	created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+other.issue+"/comments", token, map[string]any{
		"body": body,
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("a comment on %s: status=%d body=%s, want 201", other.issue, created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	if comment.IssueKey == nil || *comment.IssueKey != other.issue {
		t.Fatalf("the comment answered is on %v, want %s", comment.IssueKey, other.issue)
	}

	listed := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+other.issue+"/comments", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("read the comments of %s: status=%d body=%s", other.issue, listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]model.Comment](t, listed)
	index := slices.IndexFunc(comments, func(read model.Comment) bool { return read.ID == comment.ID })
	if index < 0 {
		t.Fatalf("the comments of %s do not list %s: %s", other.issue, comment.ID, listed.Body.String())
	}
	if read := comments[index]; read.Body != body || read.Author.Kind != "session" || read.Author.ID != brokerTestSession {
		t.Fatalf("the comment read back is %q by %+v, want %q by session %s", read.Body, read.Author, body, brokerTestSession)
	}
}
