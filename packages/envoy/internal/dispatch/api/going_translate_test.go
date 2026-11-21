// going_translate_test.go guards the hand-written translators of going_translate.go against the
// class of defect the merge introduced in goingTranslateIssueUpdate: a catalogue argument a
// translator silently drops, so the bar offers an argument that writes nothing and main adding an
// argument to a tool spec reintroduces the bug with no other test failing.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingResolvingDoer answers the reads a translator makes to resolve a document slug or a session
// name, so a reference the call carries reaches the built request as the id it resolves to rather
// than vanishing: an artifact read answers an id built from the whole URL, so the owner (the issue
// or the project) and the slug both change it; the session roster answers nothing, so session()
// keeps the id it was given (a whole uuid).
type goingResolvingDoer struct{}

func (goingResolvingDoer) Do(request *http.Request) (*http.Response, error) {
	path := request.URL.Path
	body := "{}"
	switch {
	case strings.HasPrefix(path, "/api/v1/agents"):
		// Empty body: the roster read fails to decode, so session() falls back to the id given.
		body = ""
	case strings.Contains(path, "/artifacts/"):
		body = fmt.Sprintf(`{"id":%q}`, "resolved:"+path)
	case strings.HasPrefix(path, "/api/v1/issues/"):
		body = fmt.Sprintf(`{"primary_artifact_id":%q}`, "spec:"+path)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

// goingTranslatorConsumes names, per action, each argument a translator consumes by design rather
// than forwarding to the request it builds, with why. Everything else an action takes must reach
// the built request - its path, its body, a pre-request such as a reasoned close's message, or a
// resolved document id - or refuse the call before or at its translation, or
// TestEveryTranslatorForwardsOrConsumesEveryArgument fails.
var goingTranslatorConsumes = map[string]map[string]string{
	// release and action pick which row of their tool the call performs; the row chosen is the
	// request, so the value is read by chooseGoingRoute, not sent.
	"dispatch_claim":  {"release": "picks the release row from the claim row"},
	"dispatch_follow": {"action": "picks the follow or the unfollow row"},
	// A plan's upload row is sent as the plan's runner decides from its read before (rowUpload):
	// a document of the name found is written as its next version, under the token that read
	// gave, since the person's approval of the row, which shows that document, is the consent
	// force gives on the command line; and the token sent is the one read, not one the model wrote.
	"dispatch_artifact": {
		"force":        "the approval of the row that shows the document is the consent (rowUpload)",
		"precondition": "the runner sends the token its own read before found (rowUpload)",
	},
}

func goingRouteKey(r GoingRoute) string { return r.Method + " " + r.Pattern }

// goingGuardIsArgumentsAsBody reports whether a row's translator is goingArgumentsAsBody, which
// forwards the whole argument map and so can drop nothing.
func goingGuardIsArgumentsAsBody(r GoingRoute) bool {
	return reflect.ValueOf(r.translate).Pointer() == reflect.ValueOf(goingArgumentsAsBody).Pointer()
}

// goingGuardUUID is a whole uuid a resolver echoes rather than reading, distinctive by n.
func goingGuardUUID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// goingGuardValue is a distinctive value of field's own kind: a slug for a document reference and a
// uuid for a session (so a resolver carries it through), a sentinel string, number, bool, list or
// object otherwise. n keeps each value distinct from the base's and from one another.
func goingGuardValue(field contracts.ToolField, n int) any {
	switch field.Name {
	case "artifact":
		return "guard-slug-" + strconv.Itoa(n)
	case "session":
		return goingGuardUUID(n)
	case "repo":
		return "guardowner/guardname-" + strconv.Itoa(n)
	}
	switch field.Kind {
	case contracts.ToolFieldNumber:
		return 700000 + n
	case contracts.ToolFieldBoolean:
		return true
	case contracts.ToolFieldArray:
		item := contracts.ToolField{Kind: contracts.ToolFieldString}
		if field.Item != nil {
			item = *field.Item
		}
		return []any{goingGuardValue(item, n)}
	case contracts.ToolFieldObject:
		object := map[string]any{}
		for index, sub := range field.Fields {
			object[sub.Name] = goingGuardValue(sub, n*100+index+1)
		}
		return object
	default:
		return "guardz" + field.Name + "z" + strconv.Itoa(n)
	}
}

// goingGuardBase is an argument map that makes chooseGoingRoute pick route and that route's
// translator build a request without error: the row's own path arguments, its guard, and a few
// enablers (content for an upload, a quote and a document for an anchor, a done status for a
// reason) added only where they do not divert the row. The session the caller acts as is returned
// beside it.
func goingGuardBase(entry GoingAction, route GoingRoute) (map[string]any, string, bool) {
	const session = "11111111-0000-4000-8000-000000000000"
	args := map[string]any{}
	n := 1
	for _, arg := range route.path {
		if arg == goingCallerSession {
			continue
		}
		switch arg {
		case "artifact":
			args[arg] = "guard-slug-base"
		case "session":
			args[arg] = goingGuardUUID(0)
		case "repo":
			args[arg] = "guardowner/guardname-base"
		default:
			args[arg] = "guardz-path-" + arg
		}
		n++
	}
	if route.whenArgument != "" {
		args[route.whenArgument] = route.whenValue
	}
	selects := func(candidate map[string]any) bool {
		got, _, ok := chooseGoingRoute(entry, candidate, session)
		return ok && goingRouteKey(got) == goingRouteKey(route)
	}
	for name, value := range map[string]any{
		"content":  "guardz-content-base",
		"quote":    "guardz-quote-base",
		"status":   "done",
		"artifact": "guard-slug-base",
	} {
		if _, set := args[name]; set {
			continue
		}
		candidate := goingCloneArgs(args)
		candidate[name] = value
		if selects(candidate) {
			args = candidate
		}
	}
	if !selects(args) {
		return nil, "", false
	}
	return args, session, true
}

func goingCloneArgs(args map[string]any) map[string]any {
	clone := make(map[string]any, len(args))
	for key, value := range args {
		clone[key] = value
	}
	return clone
}

// goingGuardHeld is the read before a write of action makes where its call writes to an owner that
// holds a document of the name it uploads under: a document of one version, with its name and
// slug the upload's. A write of any other action reads nothing here.
func goingGuardHeld(action string, args map[string]any) goingObservation {
	argument, uploads := goingUploadNames[action]
	if !uploads {
		return goingObservation{}
	}
	name := goingText(args, argument)
	held := model.Artifact{ID: goingGuardUUID(9), Name: name, Slug: name, Kind: "doc",
		Versions: []model.Version{{Number: 1, Authors: []model.Actor{{Kind: "user", ID: "alice"}}}}}
	return goingObservation{target: held.ID, value: goingUploadBefore{ID: held.ID, Name: name, Version: 1}, held: &held}
}

// goingGuardWrite is what a write of action through route makes of call, approved as approved:
// the requests its translation builds, serialised - each request's method, path, query, body (its
// boundary-independent form for a multipart upload) and the condition it carries on its context
// (goingSend.condition) - so two calls make the same write exactly where they build the same
// requests. A call the bar can never send as written is refused before its translation, as
// GoingCaller.prepare refuses it (goingUnsendable).
func goingGuardWrite(action string, route GoingRoute, args map[string]any, values map[string]string, session, approved string) (string, error) {
	if why := goingUnsendable(action, args); why != "" {
		return "", GoingArgumentError{Action: action, Reason: why}
	}
	reader := goingReader{doer: goingResolvingDoer{}, base: ""}
	call, err := route.translate(context.Background(), reader, goingCallInput{arguments: args, path: values, session: session})
	if err != nil {
		return "", err
	}
	sends, err := call.requests(route)
	if err != nil {
		return "", err
	}
	var built strings.Builder
	for _, send := range sends {
		built.WriteString(send.method)
		built.WriteByte(' ')
		built.WriteString(send.path)
		if len(send.query) > 0 {
			built.WriteByte('?')
			built.WriteString(send.query.Encode())
		}
		built.WriteByte('\n')
		body := send.body
		if send.stable != nil {
			body = send.stable
		}
		built.Write(body)
		built.WriteByte('\n')
		if send.condition != nil {
			condition, err := json.Marshal(send.condition)
			if err != nil {
				return "", err
			}
			built.WriteString("condition: ")
			built.Write(condition)
			built.WriteByte('\n')
		}
	}
	return built.String(), nil
}

// TestEveryTranslatorForwardsOrConsumesEveryArgument (merge guard): for every catalogue action
// whose rows are translated by a hand-written translator (not goingArgumentsAsBody, which forwards
// the whole argument map), every argument the action takes reaches the write - the request some
// row's translator builds, its path, its body, a pre-request such as a reasoned close's message,
// or a resolved document id - or is one goingTranslatorConsumes names for that action, with why.
// An argument no row forwards and no reason names is a silent drop, as goingTranslateIssueUpdate
// dropped `advances` after the merge added it.
// Actions are read from BuildGoingCatalogue, so a tool spec's new argument is held to this at
// once. An argument is tested against each row it routes to in turn, and passes as soon as one
// forwards it (a result's `check` is the record row's, not the retract row's); an argument whose
// presence refuses the write before its translation (a picture, a file path, a ref a comment
// cannot take: goingUnsendable) or refuses the translation is visibly consumed, not silently
// dropped, and passes.
func TestEveryTranslatorForwardsOrConsumesEveryArgument(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	for _, approved := range []string{goingApprovedByPerson} {
		for _, action := range catalogue.Actions() {
			var rows []GoingRoute
			for _, route := range action.Routes {
				if !goingGuardIsArgumentsAsBody(route) {
					rows = append(rows, route)
				}
			}
			if len(rows) == 0 {
				continue
			}
			for index, field := range action.Request {
				forwarded, routed := false, false
				for _, route := range rows {
					base, session, ok := goingGuardBase(action, route)
					if !ok {
						t.Errorf("%s %s: no base call selects this row", action.Action, goingRouteKey(route))
						continue
					}
					args := goingCloneArgs(base)
					args[field.Name] = goingGuardValue(field, 1000+index)
					chosen, values, ok := chooseGoingRoute(action, args, session)
					if !ok || goingRouteKey(chosen) != goingRouteKey(route) {
						// Setting this argument picks another row; it is tested there.
						continue
					}
					routed = true
					_, baseValues, _ := chooseGoingRoute(action, base, session)
					baseline, err := goingGuardWrite(action.Action, route, base, baseValues, session, approved)
					if err != nil {
						t.Errorf("%s %s: the base call does not translate: %v", action.Action, goingRouteKey(route), err)
						continue
					}
					built, err := goingGuardWrite(action.Action, route, args, values, session, approved)
					if err != nil {
						// The argument's presence refuses the translation, which the bar surfaces.
						forwarded = true
						break
					}
					if built != baseline {
						forwarded = true
						break
					}
				}
				if forwarded || !routed {
					continue
				}
				if _, consumed := goingTranslatorConsumes[action.Action][field.Name]; consumed {
					continue
				}
				t.Errorf("%s drops argument %q, approved %s: no row's translator forwards it to the request, no check before the write is sent reads it, and goingTranslatorConsumes names no reason",
					action.Action, field.Name, approved)
			}
		}
	}
}

// TestGoingArchitectureSyncRoutesOneSourceByRepo: a sync that names a repo takes the one-source
// route, its repository split into the {owner} and {repo} path segments, and a sync that names none
// takes the every-source route. Before the one-source row existed, a repo was dropped and both
// calls took the every-source route.
func TestGoingArchitectureSyncRoutesOneSourceByRepo(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	action, ok := catalogue.Lookup("dispatch_architecture_sync")
	if !ok {
		t.Fatal("the catalogue holds no dispatch_architecture_sync")
	}
	built := func(args map[string]any) string {
		route, values, ok := chooseGoingRoute(action, args, "")
		if !ok {
			t.Fatalf("%v routes to no sync row", args)
		}
		call, err := route.translate(context.Background(), goingReader{doer: goingResolvingDoer{}}, goingCallInput{arguments: args, path: values})
		if err != nil {
			t.Fatalf("translate %v: %v", args, err)
		}
		sends, err := call.requests(route)
		if err != nil {
			t.Fatal(err)
		}
		last := sends[len(sends)-1]
		return last.method + " " + last.path
	}
	if got, want := built(map[string]any{"project": "CORE", "repo": "acme/arch"}),
		"POST /api/v1/projects/CORE/architecture-sources/acme/arch/sync"; got != want {
		t.Errorf("a sync of acme/arch built %q, want the one-source route %q", got, want)
	}
	if got, want := built(map[string]any{"project": "CORE"}),
		"POST /api/v1/projects/CORE/architecture-sources/sync"; got != want {
		t.Errorf("a sync with no repo built %q, want the every-source route %q", got, want)
	}
}
