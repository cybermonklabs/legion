package main

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheApprovalPagesRoutesAreReadInTheirOwnPackage reads the broker's own source, as the docs
// build does. A route whose handler is a method of *approval.Page is read in the approval package,
// where it answers HTML pages and redirects by status and no JSON; and the account approve route's
// DECIDE_ON_BROKER_PAGE refusal names the body it answers, which carries approve_url.
func TestTheApprovalPagesRoutesAreReadInTheirOwnPackage(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	route := func(method, pattern string) int {
		t.Helper()
		i := slices.IndexFunc(a.routes, func(r route) bool { return r.Method == method && r.Pattern == pattern })
		if i < 0 {
			t.Fatalf("no route %s %s", method, pattern)
		}
		return i
	}

	decide := route(http.MethodPost, "/approve")
	if got, want := a.routes[decide].Dir, envoyDir+"/internal/cipher/approval"; got != want {
		t.Errorf("POST /approve's handler is read in %s, want %s", got, want)
	}
	pages := map[int]bool{}
	for _, o := range a.outcomes[decide] {
		if o.Code != "" || o.RespType != "" {
			t.Errorf("POST /approve answers %d %q with a JSON body %q, want an HTML page", o.Status, o.Code, o.RespType)
		}
		if o.Page {
			pages[o.Status] = true
		}
	}
	if !pages[http.StatusSeeOther] || !pages[http.StatusForbidden] {
		t.Errorf("POST /approve answers %v, want its 303 back to the page and its 403 refusals", pages)
	}

	approve := route(http.MethodPost, "/v1/credential-requests/{record}/approve")
	var refusal *outcome
	for _, o := range refusals(a.outcomes[approve]) {
		if o.Code == "DECIDE_ON_BROKER_PAGE" {
			refusal = &o
		}
	}
	if refusal == nil || refusal.Status != http.StatusForbidden || refusal.RespType == "" {
		t.Fatalf("the approve route's DECIDE_ON_BROKER_PAGE refusal = %+v, want a 403 naming its body", refusal)
	}
	ref, err := a.g.localStruct(refusal.RespType)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := a.g.fields(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(fields, func(f jsonField) bool { return f.Name == "approve_url" }) {
		t.Errorf("the refusal's body %s = %+v, want an approve_url field", refusal.RespType, fields)
	}
}

// TestAVerbatimJSONFieldIsNamedAsTheJSONItIs reads the broker's own source, as the docs build
// does. The grant-log route answers store.GrantLogEvent, whose Raw field is a json.RawMessage:
// an audit row's own detail object, passed through exactly as the row holds it, whose keys
// belong to that row's kind rather than to the Go type carrying it. refgen has to name such a
// field as the JSON it is; naming it by walking its fields is impossible, and refusing it would
// leave the one route that answers the broker's audit trail undocumentable.
func TestAVerbatimJSONFieldIsNamedAsTheJSONItIs(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(a.routes, func(r route) bool { return r.Method == http.MethodGet && r.Pattern == "/v1/grant-log" })
	if i < 0 {
		t.Fatal("no route GET /v1/grant-log")
	}
	var answer *outcome
	for _, o := range a.outcomes[i] {
		if o.Code == "" && o.RespType != "" {
			answer = &o
		}
	}
	if answer == nil {
		t.Fatalf("GET /v1/grant-log answers %+v, want a success naming the body it encodes", a.outcomes[i])
	}
	ref, err := a.g.localStruct(answer.RespType)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := a.g.fields(ref)
	if err != nil {
		t.Fatal(err)
	}
	events := slices.IndexFunc(fields, func(f jsonField) bool { return f.Name == "events" })
	if events < 0 || fields[events].Object == nil {
		t.Fatalf("the grant log's body %s = %+v, want an events field holding the event object", answer.RespType, fields)
	}
	event, err := a.g.fields(*fields[events].Object)
	if err != nil {
		t.Fatal(err)
	}
	raw := slices.IndexFunc(event, func(f jsonField) bool { return f.Name == "raw" })
	if raw < 0 {
		t.Fatalf("the event object = %+v, want the audit row's own detail as raw", event)
	}
	if got, want := event[raw].Type, "any JSON value"; got != want {
		t.Errorf("the event's raw field is named %q, want %q: a document the broker passes through verbatim is named as the JSON it is", got, want)
	}
	if event[raw].Object != nil {
		t.Errorf("the event's raw field was read as an object of its own, whose fields the page would then list: its keys are the audit row kind's, not a Go type's")
	}
}

// TestTheSignInClassRefusesPastTheRateLimit reads the broker's own source, as the docs build
// does. authenticate's authSignIn case spends the source address's bucket before any route of
// the approval page runs, and refuses past it with the page's own 429, written by a method of
// the approval package whose receiver is a field expression. refgen has to read that refusal
// through to the page it generates, or the class reads as refusing nothing while its own doc
// text says every one of its routes spends that bucket first.
func TestTheSignInClassRefusesPastTheRateLimit(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(a.classes, func(c authClass) bool { return c.Wrapper == "signInAuth" })
	if i < 0 {
		t.Fatalf("no signInAuth class among %+v", a.classes)
	}
	class := a.classes[i]
	if !slices.ContainsFunc(class.Refusals, func(o outcome) bool {
		return o.Status == http.StatusTooManyRequests && o.Page
	}) {
		t.Errorf("the %s class refuses %+v, want the approval page's 429", class.Label, class.Refusals)
	}

	page, err := apiPage(a)
	if err != nil {
		t.Fatal(err)
	}
	row := ""
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| "+class.Label+" |") {
			row = line
		}
	}
	if !strings.Contains(row, "429") {
		t.Errorf("the %s row of the Authentication table is %q, want its 429 in the refusals column", class.Label, row)
	}
}

// TestARefusalWrittenWithAnotherPackagesConstantsIsRead reads the broker's own source, as the
// docs build does. authenticate's authService case answers a token whose issuer the broker cannot
// read through writeIssuerUnavailable, which writes its code and its sentence as two constants of
// package oidc, the pair Dispatch answers the same fault with. refgen has to read both as the
// text they hold, or it refuses the whole build for a refusal whose code it cannot read.
func TestARefusalWrittenWithAnotherPackagesConstantsIsRead(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(a.classes, func(c authClass) bool { return c.Wrapper == "serviceAuth" })
	if i < 0 {
		t.Fatalf("no serviceAuth class among %+v", a.classes)
	}
	class := a.classes[i]
	const code, sentence = "OIDC_ISSUER_UNAVAILABLE", "the service-account token issuer cannot be reached; try again in a minute"
	if !slices.ContainsFunc(class.Refusals, func(o outcome) bool {
		return o.Status == http.StatusServiceUnavailable && o.Code == code && slices.Equal(o.Messages, []string{sentence})
	}) {
		t.Errorf("the %s class refuses %+v, want a 503 %s saying %q", class.Label, class.Refusals, code, sentence)
	}
}

// TestACallOfANameTwoDeclarationsShareIsRefused: refgen runs no type checker and resolves a call
// by its name alone, so a package may declare one name on two types, as the API package declares
// UnmarshalJSON on two request bodies, and refgen reads that package while it resolves no call of
// that name. A handler calling a name declared once is read as before; a call of the shared name,
// and a route row whose handler is that name, are each refused, naming both declarations, where
// reading them would follow whichever declaration came first.
func TestACallOfANameTwoDeclarationsShareIsRefused(t *testing.T) {
	tree := map[string]string{
		apiDir + "/bodies.go": `package api

type first struct{}

func (b *first) UnmarshalJSON(data []byte) error { return nil }

type second struct{}

func (b *second) UnmarshalJSON(data []byte) error { return nil }
`,
		apiDir + "/handlers.go": `package api

import "net/http"

type server struct{}

func writeError(w http.ResponseWriter, status int, code, message string) {}

func (s *server) refuse(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusBadRequest, "REFUSED", "refused")
}

func (s *server) decode(w http.ResponseWriter, r *http.Request) {
	var body first
	if err := body.UnmarshalJSON(nil); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "the body does not decode")
	}
}
`,
	}
	g, err := newGraph(writeTree(t, tree))
	if err != nil {
		t.Fatalf("a package declaring UnmarshalJSON on two types is refused before any call of it: %v", err)
	}
	if outs, err := g.handlerOutcomes(route{Handler: "refuse", Dir: apiDir, At: "the route row of refuse"}); err != nil || len(outs) != 1 || outs[0].Code != "REFUSED" {
		t.Errorf("refuse, which calls writeError, a name declared once, is read as %+v, %v; want its one REFUSED", outs, err)
	}
	at := func(file, text string) string {
		return strings.TrimSuffix(lineOf(t, tree, apiDir+"/"+file, text), ": ")
	}
	both := at("bodies.go", "(b *first)") + " and " + at("bodies.go", "(b *second)") + " each declare UnmarshalJSON"
	for _, c := range []struct{ what, handler, at string }{
		{"a handler's call of UnmarshalJSON", "decode", at("handlers.go", "body.UnmarshalJSON(")},
		{"a route row whose handler is UnmarshalJSON", "UnmarshalJSON", "the route row of UnmarshalJSON"},
	} {
		outs, err := g.handlerOutcomes(route{Handler: c.handler, Dir: apiDir, At: "the route row of " + c.handler})
		if want := c.at + ": " + both; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s is read as %+v, %v; want it refused with an error starting %q", c.what, outs, err, want)
		}
	}
}

// TestTheCarriedRouteNamesItsBodyAndItsRefusals reads the broker's own source, as the docs build
// does. POST /v1/enrollments/carried reads its body through readCarriedBody, which decodes the
// list one id at a time rather than through readJSON, so refgen has to know that reader to name
// the route's request body, carriedBody; and its 403 and 400 are written from sentinels, so their
// messages are the sentinels' own words, not "(varies)" or a format with its verbs elided (review
// of #4355).
//
// The controls (the pull request records each run): refgen's list without readCarriedBody names no
// body; the caller checked by `err != nil` rather than by its sentinel answers the 403 "(varies)".
func TestTheCarriedRouteNamesItsBodyAndItsRefusals(t *testing.T) {
	a, err := readAPI(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(a.routes, func(r route) bool {
		return r.Method == http.MethodPost && r.Pattern == "/v1/enrollments/carried"
	})
	if i < 0 {
		t.Fatal("no route POST /v1/enrollments/carried")
	}
	if !slices.ContainsFunc(a.outcomes[i], func(o outcome) bool { return o.BodyType == "carriedBody" }) {
		t.Errorf("POST /v1/enrollments/carried reads no request body refgen can name: %+v", a.outcomes[i])
	}
	want := map[string]string{
		"SERVICE_LOGIN_REQUIRED": "only a launcher credential whose login names an account may ask",
		"INVALID_SESSION_IDS":    "session_ids must name 1 to 4096 session ids, each 1 to 255 bytes of text",
	}
	for _, o := range refusals(a.outcomes[i]) {
		part, ok := want[o.Code]
		if !ok {
			continue
		}
		if !slices.ContainsFunc(o.Messages, func(m string) bool { return strings.Contains(m, part) }) {
			t.Errorf("the route's %d %s says %q, want a message holding %q", o.Status, o.Code, o.Messages, part)
		}
		delete(want, o.Code)
	}
	for code := range want {
		t.Errorf("the route answers no %s refusal", code)
	}
}
