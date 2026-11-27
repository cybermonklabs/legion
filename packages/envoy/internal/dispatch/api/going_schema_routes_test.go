// going_schema_routes_test.go holds every write tool's schema to the route it writes. Each tool's
// own schema builds the calls, each call goes through the tool's own translation to its route on
// the server's route table called in-process, as a person's run sends its approved rows, and no
// route refuses one of them for its shape. A call the schema takes and the route refuses for its
// shape is a plan row the agent proposes, the person approves and the route answers 400.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// goingSchemaSet is one call a tool's schema takes: what it varies from the call of the required
// fields alone, and its arguments. byID is a call naming its document by id, built whole rather
// than from the schema (goingSchemaItemsOf.byID), which the test requires to write.
type goingSchemaSet struct {
	name      string
	arguments map[string]any
	byID      bool
}

// goingSchemaValue gives one field its value in a call. path names the field from the call's top
// (components.ids, ops[].op, labels[]); length is the length a text must have where the call sits
// at a bound's edge, -1 for none; n tells apart the items of one list. A field whose value is no
// free text (an id, a time, a URL) answers its one real value whatever the length, and so has no
// edge call.
type goingSchemaValue func(path string, field contracts.ToolField, length, n int) any

// goingSchemaSets is every call fields, a tool's arguments, take as the test builds them: the
// required fields alone; that call with each optional field added; with each value of each enum
// and each value of each boolean; with null for each nullable field; and with each bounded field
// at each edge its schema states (a text's length, a list's count of items, a number's value),
// for the fields inside an object and inside a list's item too. A text or a list takes its value
// from value, a number its lowest allowed value (0 where the schema states none), an enum its
// first value, a list as many items as its lowest bound and one at least, and an object its
// required fields.
func goingSchemaSets(fields []contracts.ToolField, value goingSchemaValue) []goingSchemaSet {
	base := goingSchemaObject("", fields, value, 0)
	sets := []goingSchemaSet{{name: "the required fields alone", arguments: base}}
	vary := func(name string, change func(arguments map[string]any)) {
		arguments := goingSchemaClone(base)
		change(arguments)
		sets = append(sets, goingSchemaSet{name: name, arguments: arguments})
	}
	var walk func(prefix string, fields []contracts.ToolField, at func(map[string]any) map[string]any)
	walk = func(prefix string, fields []contracts.ToolField, at func(map[string]any) map[string]any) {
		for _, field := range fields {
			path := prefix + field.Name
			set := func(value any) func(map[string]any) {
				return func(arguments map[string]any) { at(arguments)[field.Name] = value }
			}
			switch field.Kind {
			case contracts.ToolFieldBoolean:
				vary(path+" true", set(true))
				vary(path+" false", set(false))
			case contracts.ToolFieldEnum:
				for _, choice := range field.Values {
					vary(path+" "+choice, set(choice))
				}
			default:
				if !field.Required {
					vary("with "+path, set(goingSchemaNatural(path, field, value, 0)))
				}
			}
			if field.Nullable {
				vary(path+" null", set(nil))
			}
			for _, edge := range goingSchemaEdges(field) {
				switch field.Kind {
				case contracts.ToolFieldNumber:
					vary(fmt.Sprintf("%s at %d", path, edge), set(float64(edge)))
				case contracts.ToolFieldString:
					if text, isText := value(path, field, edge, 0).(string); isText && len(text) == edge {
						vary(fmt.Sprintf("%s of %d characters", path, edge), set(text))
					}
				case contracts.ToolFieldArray:
					vary(fmt.Sprintf("%s of %d items", path, edge), set(goingSchemaItems(path, *field.Item, value, edge)))
				}
			}
			switch field.Kind {
			case contracts.ToolFieldObject:
				walk(path+".", field.Fields, func(arguments map[string]any) map[string]any {
					container := at(arguments)
					object, held := container[field.Name].(map[string]any)
					if !held {
						object = goingSchemaObject(path+".", field.Fields, value, 0)
						container[field.Name] = object
					}
					return object
				})
			case contracts.ToolFieldArray:
				item := *field.Item
				itemPath := path + "[]"
				switch item.Kind {
				case contracts.ToolFieldEnum:
					for _, choice := range item.Values {
						vary(itemPath+" "+choice, set([]any{choice}))
					}
				case contracts.ToolFieldString:
					for _, edge := range goingSchemaEdges(item) {
						if text, isText := value(itemPath, item, edge, 0).(string); isText && len(text) == edge {
							vary(fmt.Sprintf("%s of %d characters", itemPath, edge), set([]any{text}))
						}
					}
				case contracts.ToolFieldObject:
					walk(itemPath+".", item.Fields, func(arguments map[string]any) map[string]any {
						container := at(arguments)
						items, held := container[field.Name].([]any)
						if !held || len(items) == 0 {
							items = []any{goingSchemaObject(itemPath+".", item.Fields, value, 0)}
							container[field.Name] = items
						}
						return items[0].(map[string]any)
					})
				}
			}
		}
	}
	walk("", fields, func(arguments map[string]any) map[string]any { return arguments })
	return sets
}

// goingSchemaEdges is each edge of field's bounds, low then high, as whole numbers.
func goingSchemaEdges(field contracts.ToolField) []int {
	var edges []int
	for _, bound := range []*float64{field.Min, field.Max} {
		if bound != nil {
			edges = append(edges, int(*bound))
		}
	}
	return edges
}

// goingSchemaObject is an object of the required fields alone, each at its natural value.
func goingSchemaObject(prefix string, fields []contracts.ToolField, value goingSchemaValue, n int) map[string]any {
	object := map[string]any{}
	for _, field := range fields {
		if field.Required {
			object[field.Name] = goingSchemaNatural(prefix+field.Name, field, value, n)
		}
	}
	return object
}

// goingSchemaNatural is the value a field takes where nothing varies it: value's for a text, the
// lowest allowed for a number, false, the first choice, a list of as many items as its lowest
// bound and one at least, and an object of its required fields.
func goingSchemaNatural(path string, field contracts.ToolField, value goingSchemaValue, n int) any {
	switch field.Kind {
	case contracts.ToolFieldNumber:
		if field.Min != nil {
			return *field.Min
		}
		return 0.0
	case contracts.ToolFieldBoolean:
		return false
	case contracts.ToolFieldEnum:
		return field.Values[0]
	case contracts.ToolFieldArray:
		count := 1
		if field.Min != nil && int(*field.Min) > count {
			count = int(*field.Min)
		}
		return goingSchemaItems(path, *field.Item, value, count)
	case contracts.ToolFieldObject:
		return goingSchemaObject(path+".", field.Fields, value, n)
	}
	return value(path, field, -1, n)
}

// goingSchemaItems is a list of count items of item, each its own (n).
func goingSchemaItems(path string, item contracts.ToolField, value goingSchemaValue, count int) []any {
	items := make([]any, count)
	for n := range items {
		items[n] = goingSchemaNatural(path+"[]", item, value, n)
	}
	return items
}

// goingSchemaClone is a deep copy of a call's arguments.
func goingSchemaClone(arguments map[string]any) map[string]any {
	var clone func(any) any
	clone = func(value any) any {
		switch value := value.(type) {
		case map[string]any:
			copied := make(map[string]any, len(value))
			for key, item := range value {
				copied[key] = clone(item)
			}
			return copied
		case []any:
			copied := make([]any, len(value))
			for index, item := range value {
				copied[index] = clone(item)
			}
			return copied
		}
		return value
	}
	return clone(arguments).(map[string]any)
}

// goingSchemaText is a free text of exactly length characters, words where length is -1.
func goingSchemaText(words string, length int) string {
	if length < 0 {
		return words
	}
	return strings.Repeat("a", length)
}

// goingSchemaComponents is how many components the architecture model of the test's project
// holds: the most an issue's explicit attachment names.
const goingSchemaComponents = 50

// goingSchemaSessions is how many sessions a broadcast at its bound names: the stand-in listener
// holds one, and the rest are ids it knows nothing of.
const goingSchemaSessions = 100

// newGoingSchemaRig is the command-bar rig whose server holds a file store, so a result can name a
// picture, and whose GitHub stand-in serves acme/arch, the project's architecture source, with
// goingSchemaComponents components, and acme/wide, a second readable repository.
func newGoingSchemaRig(t *testing.T) goingWritesRig {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	people := goingCookieOrHeader{
		cookie: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		header: headerIdentity(database),
	}
	components := make(map[string]string, goingSchemaComponents)
	for index := range goingSchemaComponents {
		components[fmt.Sprintf("part-%02d.md", index)] = fmt.Sprintf("---\ntitle: Part %d\n---\nPart %d of the model.\n", index, index)
	}
	fake := &fakeGitHubApp{
		installations: map[string]string{"acme/arch": "read", "acme/wide": "read"},
		repos: map[string]*fakeArchitectureRepo{
			"acme/arch": {commit: shaInfra, files: components},
			"acme/wide": {commit: shaInfra, files: infraFiles()},
		},
	}
	github := githubtest.Server(t, githubtest.Spec{Handle: fake.handler(t).ServeHTTP})
	app := &auth.AppConfig{ClientID: "Iv1.test", PEM: goingFixtureAppPEM(t)}
	mux, _, _, served := newRegisteredTestServer(t, testServerOptions{store: database, identity: people, envoyURL: listener.URL,
		app: app, githubAPIBase: github, files: filestest.NewMemory()})
	return goingWritesRig{
		mux: mux, network: EnforceCookieOrigin(testServerURL, mux), served: served,
		database: database, sessions: sessions, sent: &sent,
	}
}

// goingSchemaItemsOf are the real items a call's identifier fields name: the issue and its
// documents, the project's document, the issues, priorities and components a list names, and the
// asks, comments, messages, results, groups and documents a write names. goingSchemaSeed makes a
// fresh one of each item a tool's write would spoil for the next call.
type goingSchemaItemsOf struct {
	issue                        string
	notesSlug, notesID           string
	planSlug                     string
	planVersion                  int
	guideSlug, guideID           string
	image                        string
	pool, priorities, components []string
	ask, optionsAsk              string
	comment, suggestion          string
	// mentioned is a comment that mentions the planner session, and mentionedBlock its first
	// block.
	mentioned, mentionedBlock string
	result                    string
	issueMessage, direct      string
	// sessionReply is the planner session's reply to direct, a message that session wrote.
	sessionReply    string
	group, priority string
	// document is a document of the issue that a call may rename, rebuild or remove. editable is
	// an issue whose spec is goingSchemaDocumentText, which an edit works on, with that spec's id,
	// token and blocks as read, and projectDocumentSlug and projectDocumentID a project's document
	// of the same text.
	document                 string
	editable                 string
	editableSpec             string
	documentToken            string
	blocks                   []model.ArtifactBlock
	projectDocumentSlug      string
	projectDocumentID        string
	decisions, decisionBlock string
	decisionSHA              string
}

// goingSchemaSeed makes the items the calls name through the routes, as alice.
type goingSchemaSeed struct {
	t      *testing.T
	rig    goingWritesRig
	n      int
	shared goingSchemaItemsOf
}

// next is a number no earlier call of the seed took.
func (s *goingSchemaSeed) next() int {
	s.n++
	return s.n
}

// write is one write of the seed, which must succeed, decoded into a value of T.
func goingSchemaWrite[T any](s *goingSchemaSeed, method, target string, body any) T {
	s.t.Helper()
	return decodeBody[T](s.t, s.rig.fixture(s.t, "alice", method, target, body))
}

// goingSchemaDocumentText is the text of a document a call edits: two paragraphs and a table, so
// every operation of an edit has a block to work on.
const goingSchemaDocumentText = "# Draft\n\nThe first line.\n\nThe second line.\n\n| Name | Size |\n| --- | --- |\n| one | 1 |\n| two | 2 |\n"

// newGoingSchemaSeed makes the project DEMO with its architecture source, its issue with a spec,
// notes, a plan, a Verification document, decisions and a picture, the project's guide, twenty
// more issues, twenty priorities and the items each write names.
func newGoingSchemaSeed(t *testing.T, rig goingWritesRig) *goingSchemaSeed {
	t.Helper()
	s := &goingSchemaSeed{t: t, rig: rig}
	f := &s.shared
	issue := createInteractionIssue(t, rig.mux, "DEMO", "The first issue", "The spec says hello to everyone.\n")
	f.issue = issue.Key
	rig.fixture(t, "alice", http.MethodPut, "/api/v1/projects/DEMO/architecture-sources/acme/arch", map[string]any{"branch": "main"})
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/projects/DEMO/architecture-sources/sync", map[string]any{})
	for index := range goingSchemaComponents {
		f.components = append(f.components, fmt.Sprintf("part-%02d", index))
	}
	owner := "/api/v1/issues/" + f.issue
	notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
	f.notesSlug, f.notesID = notes.Artifact.Slug, notes.Artifact.ID
	guide := uploadDocument(t, rig.mux, "/api/v1/projects/DEMO", "guide.md", "# Guide\n\nRead this first.\n")
	f.guideSlug, f.guideID = guide.Artifact.Slug, guide.Artifact.ID
	plan := uploadDocument(t, rig.mux, owner, "plan.md", "# Plan\n\nDo the work.\n")
	f.planSlug, f.planVersion = plan.Artifact.Slug, plan.Version.Number
	uploadDocument(t, rig.mux, owner, "verification.md", twoChecks)
	picture := uploadImage(t, rig.mux, f.issue, "screen.png", "pixels")
	f.image = "dispatch://" + f.issue + "/artifact/" + picture.Artifact.Slug
	decisions := uploadDocument(t, rig.mux, owner, "decisions.md", "## Keep the change small\n\nThe smallest change wins.\n")
	f.decisions = decisions.Artifact.ID
	kept := readIssueDecisions(t, rig.mux, f.issue)
	if len(kept.Decisions) == 0 {
		t.Fatalf("%s's decisions.md holds no decision", f.issue)
	}
	f.decisionBlock, f.decisionSHA = kept.Decisions[0].BlockID, kept.Decisions[0].TextSHA256
	for index := range 20 {
		f.pool = append(f.pool, rig.issue(t, fmt.Sprintf("Pool issue %d", index)))
		f.priorities = append(f.priorities, s.newPriority())
	}
	f.issueMessage = goingSchemaWrite[model.Message](s, http.MethodPost, owner+"/messages", map[string]any{"body": "Where are we?"}).ID
	f.direct = goingSchemaWrite[model.Message](s, http.MethodPost, "/api/v1/agents/"+goingPlanner+"/messages", map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	}).ID
	f.ask, f.optionsAsk = s.newAsk(false), s.newAsk(true)
	f.comment, f.suggestion = s.newComment(), s.newSuggestion()
	f.mentioned, f.mentionedBlock = s.newMentioned()
	reply := bearerRequest(t, rig.mux, http.MethodPost, "/api/v1/messages/"+f.direct+"/reply", map[string]any{
		"actor": goingActorSession(goingPlanner), "attempt": 1, "body": "On my way.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the planner's reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	f.sessionReply = decodeBody[model.Message](t, reply).ID
	f.result = s.newResult()
	f.group, f.priority = s.newGroup(), s.newPriority()
	f.document = s.newDocument()
	s.newEditable(f)
	return s
}

// newPriority makes one more priority and answers its id.
func (s *goingSchemaSeed) newPriority() string {
	id := fmt.Sprintf("seed-priority-%d", s.next())
	goingSchemaWrite[map[string]any](s, http.MethodPost, "/api/v1/priorities", map[string]any{"id": id, "title": "Priority " + id})
	return id
}

// newAsk opens one more ask on the issue, of two options where options is set.
func (s *goingSchemaSeed) newAsk(options bool) string {
	body := map[string]any{"question": fmt.Sprintf("Ship change %d on Friday?", s.next())}
	if options {
		body["options"] = []any{map[string]any{"label": "Ship"}, map[string]any{"label": "Hold"}}
	}
	return goingSchemaWrite[model.Ask](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/asks", body).ID
}

// newComment starts one more thread on the issue.
func (s *goingSchemaSeed) newComment() string {
	return goingSchemaWrite[model.Comment](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/comments", map[string]any{
		"body": fmt.Sprintf("A thread to work on, %d.", s.next()),
	}).ID
}

// newIssueMessage posts one more message on the issue.
func (s *goingSchemaSeed) newIssueMessage() string {
	return goingSchemaWrite[model.Message](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/messages", map[string]any{
		"body": fmt.Sprintf("A message to correct, %d.", s.next()),
	}).ID
}

// newResolvedComment starts one more thread on the issue and resolves it.
func (s *goingSchemaSeed) newResolvedComment() string {
	id := s.newComment()
	s.rig.fixture(s.t, "alice", http.MethodPost, "/api/v1/comments/"+id+"/resolve", map[string]any{})
	return id
}

// newSuggestion suggests one more change, to a document of the issue of its own, so a suggestion
// a call accepts changes no text another call quotes.
func (s *goingSchemaSeed) newSuggestion() string {
	notes := uploadDocument(s.t, s.rig.mux, "/api/v1/issues/"+s.shared.issue, fmt.Sprintf("suggest-%d.md", s.next()), "# Notes\n\nThe first line.\n")
	return goingSchemaWrite[model.Comment](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/comments", map[string]any{
		"anchor":     map[string]any{"artifact": notes.Artifact.ID, "quote": "first line"},
		"suggestion": map[string]any{"replace_with": "opening line"},
	}).ID
}

// newMentioned posts one more comment on the issue that mentions the planner session, and
// answers it with its first block.
func (s *goingSchemaSeed) newMentioned() (id, block string) {
	id = goingSchemaWrite[model.Comment](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/comments", map[string]any{
		"body": fmt.Sprintf("Have a look, %d.", s.next()), "mentions": []any{map[string]any{"target": "session:" + goingPlanner}},
	}).ID
	blocks := commentBlockIDs(s.t, s.rig.database, id)
	if len(blocks) == 0 {
		s.t.Fatalf("comment %s holds no block", id)
	}
	return id, blocks[0]
}

// newResult records one more result on the issue.
func (s *goingSchemaSeed) newResult() string {
	return goingSchemaWrite[recordedResult](s, http.MethodPost, "/api/v1/issues/"+s.shared.issue+"/results",
		resultBody("k1", "pass", headCommit, nil)).Result.ID
}

// newGroup makes one more session group, of a session of its own.
func (s *goingSchemaSeed) newGroup() string {
	n := s.next()
	return goingSchemaWrite[struct {
		ID string `json:"id"`
	}](s, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": fmt.Sprintf("Seed group %d", n), "sessions": []any{fmt.Sprintf("seed-session-%d", n)},
	}).ID
}

// newIssue makes one more issue of DEMO with spec.
func (s *goingSchemaSeed) newIssue(spec string) string {
	return goingSchemaWrite[model.Issue](s, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": fmt.Sprintf("An issue a call works on, %d", s.next()), "spec": spec, "force": true,
	}).Key
}

// newDocument uploads one more document of goingSchemaDocumentText to the issue, and answers its
// id.
func (s *goingSchemaSeed) newDocument() string {
	return uploadDocument(s.t, s.rig.mux, "/api/v1/issues/"+s.shared.issue, fmt.Sprintf("draft-%d.md", s.next()), goingSchemaDocumentText).Artifact.ID
}

// newEditable makes one more issue whose spec is goingSchemaDocumentText, reads that spec's id,
// token and blocks, and uploads one more project document of the same text.
func (s *goingSchemaSeed) newEditable(items *goingSchemaItemsOf) {
	items.editable = s.newIssue(goingSchemaDocumentText)
	items.editableSpec = s.rig.issueRead(s.t, items.editable).PrimaryArtifactID
	items.documentToken = readDocumentPrecondition(s.t, s.rig.mux, items.editableSpec).Token
	items.blocks = goingSchemaWrite[[]model.ArtifactBlock](s, http.MethodGet, "/api/v1/artifacts/"+items.editableSpec+"/blocks", nil)
	project := uploadDocument(s.t, s.rig.mux, "/api/v1/projects/DEMO", fmt.Sprintf("draft-%d.md", s.next()), goingSchemaDocumentText)
	items.projectDocumentSlug, items.projectDocumentID = project.Artifact.Slug, project.Artifact.ID
}

// byID is the calls of tool that name its document by id beside the owner its rule requires: an
// edit of the issue's spec and one of the project's document. Each takes the document's own row,
// since the owner's slug routes read a slug or a filename and never an id; before, each took its
// owner's slug route and was 404, though the plan's row read the document by its id and showed it.
func (items goingSchemaItemsOf) byID(tool string) []goingSchemaSet {
	if tool != "dispatch_doc_edit" {
		return nil
	}
	ops := func() []any {
		return []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}}
	}
	return []goingSchemaSet{
		{name: "the issue's spec by id", byID: true, arguments: map[string]any{"issue": items.editable, "artifact": items.editableSpec, "ops": ops()}},
		{name: "the project's document by id", byID: true, arguments: map[string]any{"project": "DEMO", "artifact": items.projectDocumentID, "ops": ops()}},
	}
}

// goingSchemaFresh is the items each tool's write spoils for the next call, which the seed makes
// afresh before every call of that tool: an issue a call closes or claims, an ask it resolves or
// answers, a comment it resolves, a comment or a message it deletes, a document it edits or
// removes, a group or a priority it deletes, a source or a daemon it removes.
var goingSchemaFresh = map[string][]string{
	"dispatch_issue_update":               {"issue"},
	"dispatch_claim":                      {"issue"},
	"dispatch_edit_ask":                   {"ask"},
	"dispatch_resolve_ask":                {"ask"},
	"dispatch_resolve_comment":            {"comment"},
	"dispatch_follow":                     {"followedAsk"},
	"dispatch_doc_edit":                   {"editable"},
	"dispatch_request_approval":           {"issue"},
	"dispatch_plan_approve":               {"issue"},
	"dispatch_review":                     {"issue"},
	"dispatch_result":                     {"result"},
	"dispatch_remove_architecture_source": {"source"},
	"dispatch_unset_legion_daemon":        {"daemon"},
	"dispatch_edit_session_group":         {"group"},
	"dispatch_delete_session_group":       {"group"},
	"dispatch_edit_priority":              {"priority"},
	"dispatch_delete_priority":            {"priority"},
	"dispatch_reply_comment_delivery":     {"mentioned"},
	"dispatch_hand_ask":                   {"ask"},
	"dispatch_answer_ask":                 {"optionsAsk"},
	"dispatch_decline_ask":                {"ask"},
	"dispatch_reopen_comment":             {"resolvedComment"},
	"dispatch_delete_comment":             {"comment"},
	"dispatch_delete_message":             {"issueMessage"},
	"dispatch_accept_suggestion":          {"suggestion"},
	"dispatch_reject_suggestion":          {"suggestion"},
	"dispatch_remove_document":            {"untouched"},
	"dispatch_rebuild_document":           {"document"},
	"dispatch_rename_document":            {"document"},
	"dispatch_review_document":            {"issue"},
}

// items is the items a call of tool names: the shared ones, with a fresh one of each the tool
// spoils.
func (s *goingSchemaSeed) items(tool string) goingSchemaItemsOf {
	items := s.shared
	items.pool, items.priorities, items.components = slices.Clone(items.pool), slices.Clone(items.priorities), slices.Clone(items.components)
	for _, spoiled := range goingSchemaFresh[tool] {
		switch spoiled {
		case "issue":
			items.issue = s.newIssue("The spec says hello to everyone.\n")
			plan := uploadDocument(s.t, s.rig.mux, "/api/v1/issues/"+items.issue, "plan.md", "# Plan\n\nDo the work.\n")
			items.planSlug, items.planVersion = plan.Artifact.Slug, plan.Version.Number
			notes := uploadDocument(s.t, s.rig.mux, "/api/v1/issues/"+items.issue, "notes.md", "# Notes\n\nThe first line.\n")
			items.notesSlug, items.notesID = notes.Artifact.Slug, notes.Artifact.ID
			uploadDocument(s.t, s.rig.mux, "/api/v1/issues/"+items.issue, "verification.md", twoChecks)
		case "ask":
			items.ask = s.newAsk(false)
		case "followedAsk":
			// An ask the planner session follows, so an unfollow has a follower to take off.
			items.ask = s.newAsk(false)
			s.rig.fixture(s.t, "alice", http.MethodPut, "/api/v1/asks/"+items.ask+"/followers/"+goingPlanner, nil)
		case "untouched":
			// A document made by its name alone and never written in, which is the one a remove
			// takes.
			items.document = goingSchemaWrite[uploadedArtifact](s, http.MethodPost, "/api/v1/issues/"+items.issue+"/artifacts", map[string]any{
				"name": fmt.Sprintf("untouched-%d.md", s.next()),
			}).Artifact.ID
		case "optionsAsk":
			items.optionsAsk = s.newAsk(true)
		case "comment":
			items.comment = s.newComment()
		case "resolvedComment":
			items.comment = s.newResolvedComment()
		case "suggestion":
			items.suggestion = s.newSuggestion()
		case "issueMessage":
			items.issueMessage = s.newIssueMessage()
		case "mentioned":
			items.mentioned, items.mentionedBlock = s.newMentioned()
		case "result":
			items.result = s.newResult()
		case "group":
			items.group = s.newGroup()
		case "priority":
			items.priority = s.newPriority()
		case "document":
			items.document = s.newDocument()
		case "editable":
			s.newEditable(&items)
		case "source":
			s.rig.fixture(s.t, "alice", http.MethodPut, "/api/v1/projects/DEMO/architecture-sources/acme/wide", map[string]any{"branch": "main"})
		case "daemon":
			s.rig.fixture(s.t, "alice", http.MethodPut, "/api/v1/projects/DEMO/legion/daemon", map[string]any{"state_url": "https://legion.example.com"})
		default:
			s.t.Fatalf("goingSchemaFresh names %q, which the seed makes none of", spoiled)
		}
	}
	return items
}

// value is the value of each field of tool's calls: a real item of items for an identifier, chosen
// by the field's name; a time now, or an hour on for a snooze; words for a free text.
func (items goingSchemaItemsOf) value(s *goingSchemaSeed, tool string) goingSchemaValue {
	now := time.Now().UTC().Truncate(time.Second)
	return func(path string, field contracts.ToolField, length, n int) any {
		words := goingSchemaText(fmt.Sprintf("Words for %s %d.", path, n), length)
		switch path {
		case "issue":
			return items.issue
		case "title":
			// A title of its own: a new issue's title like an earlier one's is a possible
			// duplicate, and a priority's title gives its id.
			switch tool {
			case "dispatch_issue":
				return goingSchemaText(fmt.Sprintf("Quokka%d", s.next()), length)
			case "dispatch_create_priority":
				return goingSchemaText(fmt.Sprintf("Words for title %d.", s.next()), length)
			}
		case "project", "project[]":
			if tool == "dispatch_project" {
				break
			}
			return "DEMO"
		case "parent", "before", "after":
			if tool == "dispatch_issue" || tool == "dispatch_issue_update" {
				return items.pool[n%len(items.pool)]
			}
		case "blocked_by[]":
			return items.pool[n%len(items.pool)]
		case "advances[]":
			return items.priorities[n%len(items.priorities)]
		case "components.ids[]":
			return items.components[n%len(items.components)]
		case "labels[]", "labels_add[]":
			if length >= 0 {
				return goingSchemaText("", length)
			}
			return fmt.Sprintf("label-%d", n)
		case "external_links[]":
			return fmt.Sprintf("https://example.com/pull/%d", s.next())
		case "external":
			return fmt.Sprintf("acme/widgets#%d", s.next())
		case "repo":
			switch tool {
			case "dispatch_issue", "dispatch_issue_update", "dispatch_architecture_sync", "dispatch_set_default_repository", "dispatch_deploy":
				return "acme/arch"
			case "dispatch_save_architecture_source", "dispatch_remove_architecture_source":
				return "wide"
			}
			return "arch"
		case "owner":
			if tool == "dispatch_create_priority" || tool == "dispatch_edit_priority" {
				return "bob"
			}
			return "acme"
		case "assignee", "default_assignee", "asked_of":
			return "bob"
		case "route":
			return "role:legion-reviewer"
		case "ref":
			return "dispatch://" + items.issue
		case "artifact", "anchor.artifact":
			return items.notesSlug
		case "quote", "anchor.quote":
			return "first line"
		case "ask", "reply_to_ask":
			if tool == "dispatch_answer_ask" {
				return items.optionsAsk
			}
			return items.ask
		case "comment":
			switch tool {
			case "dispatch_accept_suggestion", "dispatch_reject_suggestion":
				return items.suggestion
			case "dispatch_redeliver_comment", "dispatch_reply_comment_delivery":
				return items.mentioned
			}
			return items.comment
		case "reply_to", "thread":
			return items.comment
		case "mentions[]", "target":
			if tool == "dispatch_redeliver_comment" || tool == "dispatch_reply_comment_delivery" {
				return "session:" + goingPlanner
			}
			return "role:legion-reviewer"
		case "in_reply_to":
			if tool == "dispatch_send_to_agent" {
				return items.direct
			}
			return items.issueMessage
		case "message":
			// The corrections of a message take an issue message the caller wrote; every other
			// tool's message is the direct one.
			if tool == "dispatch_edit_message" || tool == "dispatch_delete_message" {
				return items.issueMessage
			}
			return items.direct
		case "read_replies[]":
			return items.sessionReply
		case "session", "add[]", "remove[]":
			return goingPlanner
		case "sessions[]":
			return fmt.Sprintf("schema-session-%d", s.next())
		case "session_ids[]":
			if n == 0 {
				return goingPlanner
			}
			return fmt.Sprintf("absent-session-%d", n)
		case "check":
			return "k1"
		case "role":
			return goingSchemaText("implementer", length)
		case "commit":
			return headCommit
		case "program":
			return "worker"
		case "deployed_at":
			return now.Format(time.RFC3339)
		case "url":
			return "https://worker.example.com"
		case "screenshots[]":
			return items.image
		case "retract":
			return items.result
		case "name":
			switch tool {
			case "dispatch_artifact", "dispatch_rename_document":
				return fmt.Sprintf("upload-%d.md", s.next())
			case "dispatch_create_session_group", "dispatch_edit_session_group":
				return goingSchemaText(fmt.Sprintf("Group %d", s.next()), length)
			}
		case "path":
			return "notes.md"
		case "key":
			return fmt.Sprintf("NP%s", strings.ToUpper(strconv.FormatInt(int64(s.next()), 36)))
		case "id":
			id := fmt.Sprintf("schema-priority-%d", s.next())
			if length > len(id) {
				return strings.Repeat("p", length-len(id)) + id
			}
			return id
		case "priority":
			return items.priority
		case "rank.before", "rank.after":
			return items.priorities[0]
		case "group":
			return items.group
		case "phase":
			return "plan"
		case "body":
			if tool == "dispatch_handoff_write" {
				return `{"summary":"planned"}`
			}
		case "branch":
			return "main"
		case "deploy_repo", "excluded_repos[]":
			return "acme/arch"
		case "renamed_to":
			return "acme/renamed"
		case "reply_to_block":
			return items.mentionedBlock
		case "deploy_workflow_path", "pr_checks_workflow_path":
			return ".github/workflows/deploy.yml"
		case "production_job_names[]":
			return fmt.Sprintf("deploy-production-%d", n)
		case "population_authors[]":
			return fmt.Sprintf("author-%d", n)
		case "state_url":
			return "https://legion.example.com"
		case "attempt":
			return "1"
		case "read_through", "cleared_before", "dismiss_through", "expected_answer_at":
			return now.Format(time.RFC3339)
		case "expected_edited_at":
			// The ask a call answers was never edited.
			return nil
		case "snoozed_until":
			return now.Add(time.Hour).Format(time.RFC3339)
		case "threads[]":
			return "reply:" + items.comment
		case "seen_replies":
			return map[string]any{"reply:" + items.comment: map[string]any{"version": 1}}
		case "selected[]":
			return "Ship"
		case "document":
			return items.document
		case "block":
			return items.decisionBlock
		case "text_sha256":
			return items.decisionSHA
		case "dismissed[]":
			return "pinned_items:ask:" + items.ask
		case "precondition.document":
			return items.documentToken
		case "ops[].attributes":
			return map[string]any{"kind": "note"}
		case "options[].label":
			return goingSchemaText(fmt.Sprintf("Option %d", n), length)
		}
		if field.Kind == contracts.ToolFieldAny {
			return map[string]any{}
		}
		return words
	}
}

// goingSchemaOpFields is what each operation of a document edit takes, beside op: the fields it
// needs first, then the ones it may carry.
var goingSchemaOpFields = map[string][]string{
	"replace":       {"find", "with", "occurrence"},
	"delete":        {"find", "occurrence"},
	"insert":        {"markdown", "after", "before"},
	"retype":        {"block", "type", "attributes"},
	"move":          {"block", "after", "before"},
	"delete_row":    {"block", "index"},
	"delete_column": {"block", "index"},
}

// goingSchemaOpHome is the operation each field of an edit's operation goes with where the
// operation the call names takes no such field.
var goingSchemaOpHome = map[string]string{
	"find": "replace", "with": "replace", "occurrence": "replace", "markdown": "insert", "after": "insert",
	"before": "insert", "block": "retype", "type": "retype", "attributes": "retype", "index": "delete_row",
}

// repairEdit makes each operation of a document edit what its op takes, and its precondition
// select exactly one of document and blocks, as dispatch_doc_edit's validation and its
// operations' descriptions state: an operation carrying a field its op does not take takes the op
// the field goes with, and each op names the block or the text of the call's document it works
// on.
func (items goingSchemaItemsOf) repairEdit(arguments map[string]any) {
	paragraph, second, table := "", "", ""
	for _, block := range items.blocks {
		switch {
		case block.Type == "paragraph" && paragraph == "":
			paragraph = block.ID
		case block.Type == "paragraph" && second == "":
			second = block.ID
		case block.Type == "table" && table == "":
			table = block.ID
		}
	}
	ops, _ := arguments["ops"].([]any)
	for _, raw := range ops {
		op := raw.(map[string]any)
		kind, _ := op["op"].(string)
		for name := range op {
			if name != "op" && !slices.Contains(goingSchemaOpFields[kind], name) {
				kind = goingSchemaOpHome[name]
			}
		}
		op["op"] = kind
		anchored := op["after"] != nil || op["before"] != nil
		switch kind {
		case "replace":
			op["find"], op["with"] = "first line", "opening line"
		case "delete":
			op["find"] = "The second line."
		case "insert":
			op["markdown"] = "A new paragraph."
			if !anchored {
				op["after"] = "The first line."
			}
		case "retype":
			op["block"], op["type"] = paragraph, "callout"
		case "move":
			op["block"] = second
			if !anchored {
				op["before"] = "The first line."
			}
		case "delete_row", "delete_column":
			op["block"] = table
			if _, indexed := op["index"]; !indexed {
				op["index"] = 1.0
			}
		}
		for _, anchor := range []string{"after", "before"} {
			if _, held := op[anchor]; held && (kind == "insert" || kind == "move") {
				op[anchor] = "The first line."
			}
		}
	}
	if precondition, held := arguments["precondition"].(map[string]any); held {
		if _, blocks := precondition["blocks"]; blocks {
			delete(precondition, "document")
			named := []any{}
			for _, block := range items.blocks {
				named = append(named, map[string]any{"id": block.ID, "token": block.Token})
			}
			precondition["blocks"] = named
		} else {
			precondition["document"] = items.documentToken
		}
	}
}

// goingSchemaOwner names the owner of a document call as the tools that write one take exactly
// one of them: the project with projectDocument where the call names the project, and otherwise
// the issue, with issueDocument where the call names a document.
func goingSchemaOwner(arguments map[string]any, issue, issueDocument, projectDocument string) {
	if _, project := arguments["project"]; project {
		delete(arguments, "issue")
		arguments["artifact"] = projectDocument
		return
	}
	arguments["issue"] = issue
	if _, named := arguments["artifact"]; named {
		arguments["artifact"] = issueDocument
	}
}

// repair makes a call of tool satisfy the cross-field rules the tool's spec states beside its
// schema (DispatchToolSpec.validation in dispatch-tools.ts, and the rules each field's description
// states): the field a call must name besides the required ones, the owner a document write names
// exactly one of, the fields that go together and those that exclude each other. It adds what a
// rule needs and drops what a rule refuses, keeping the field the call varies.
func (items goingSchemaItemsOf) repair(tool string, arguments map[string]any) {
	has := func(name string) bool {
		_, held := arguments[name]
		return held
	}
	if components, held := arguments["components"].(map[string]any); held {
		// The rule of an attachment's parts: mode explicit names ids, and mode none a reason.
		_, ids := components["ids"]
		_, reason := components["reason"]
		switch {
		case ids:
			components["mode"] = "explicit"
		case reason:
			components["mode"] = "none"
		}
		switch components["mode"] {
		case "explicit":
			if !ids {
				components["ids"] = []any{items.components[0]}
			}
		case "none":
			if !reason {
				components["reason"] = "Process work."
			}
		}
	}
	switch tool {
	case "dispatch_issue_update":
		// An option goes with a status and no other field (the tool's rule), and writes no field
		// of its own, so a call that names one gets a status. One set to true is never sent, by
		// design: a plan writes it out as the rows of the issues it changes.
		options, named := false, false
		for name := range arguments {
			switch name {
			case optionIncludeChildren, optionUnlinkChildren, optionIncludeParent:
				options = true
			case "issue", "reason", "advances_revision":
			default:
				named = true
			}
		}
		if options && !has("status") {
			arguments["status"] = "todo"
		}
		if !named && !options {
			arguments["title"] = "A title the update writes"
		}
		if has("reason") {
			arguments["status"] = "done"
		}
		if arguments["status"] == "done" && !has("reason") {
			arguments["reason"] = "The work is done."
		}
	case "dispatch_ask":
		goingSchemaOwner(arguments, items.issue, items.notesSlug, items.guideSlug)
		if anchor, held := arguments["anchor"].(map[string]any); held {
			anchor["artifact"], anchor["quote"] = items.notesSlug, "first line"
			if has("project") {
				anchor["artifact"], anchor["quote"] = items.guideSlug, "Read this"
			}
		}
	case "dispatch_edit_ask":
		if len(arguments) == 1 {
			arguments["question"] = "Ship it on Monday?"
		}
	case "dispatch_comment":
		// A quote needs the document it is a quote of.
		quoted := has("quote") || has("occurrence")
		if quoted {
			arguments["artifact"] = ""
		}
		goingSchemaOwner(arguments, items.issue, items.notesSlug, items.guideSlug)
		if quoted {
			arguments["quote"] = "first line"
			if has("project") {
				arguments["quote"] = "Read this"
			}
		}
		if has("turn") {
			arguments["reply_to_ask"] = items.ask
		}
	case "dispatch_suggest":
		// Its validation: artifact or ref names the document.
		arguments["artifact"] = ""
		goingSchemaOwner(arguments, items.issue, items.notesSlug, items.guideSlug)
		arguments["quote"] = "first line"
		if has("project") {
			arguments["quote"] = "Read this"
		}
	case "dispatch_message":
		arguments["issue"] = items.issue
	case "dispatch_doc_edit":
		goingSchemaOwner(arguments, items.editable, "spec", items.projectDocumentSlug)
		items.repairEdit(arguments)
	case "dispatch_request_approval":
		goingSchemaOwner(arguments, items.issue, items.notesSlug, items.guideSlug)
	case "dispatch_plan_approve":
		arguments["version"] = float64(items.planVersion)
		if has("artifact") {
			arguments["artifact"] = items.planSlug
		}
	case "dispatch_review", "dispatch_review_document":
		verdict := "verdict"
		if tool == "dispatch_review_document" {
			verdict = "state"
		}
		if has("reason") {
			arguments[verdict] = "changes_requested"
		}
		if arguments[verdict] == "changes_requested" && !has("reason") {
			arguments["reason"] = "The second section must name its owner."
		}
		if tool == "dispatch_review" && has("artifact") {
			arguments["artifact"] = "spec"
		}
	case "dispatch_result":
		if has("retract") || has("reason") {
			maps.DeleteFunc(arguments, func(name string, _ any) bool { return name != "retract" && name != "reason" })
			arguments["retract"] = items.result
			if !has("reason") {
				arguments["reason"] = "Recorded at the wrong commit."
			}
			break
		}
		for name, value := range map[string]any{"issue": items.issue, "check": "k1", "role": "implementer", "verdict": "pass", "commit": headCommit} {
			if !has(name) {
				arguments[name] = value
			}
		}
	case "dispatch_artifact":
		goingSchemaOwner(arguments, items.issue, "", "")
		delete(arguments, "artifact")
		if !has("path") {
			if _, text := arguments["content"].(string); !text {
				arguments["content"] = "# Upload\n\nWords to keep.\n"
			}
		} else {
			delete(arguments, "content")
		}
	case "dispatch_create_priority":
		if title, _ := arguments["title"].(string); len(title) > 200 && !has("id") {
			arguments["id"] = fmt.Sprintf("long-title-%d", len(title))
		}
	case "dispatch_edit_priority":
		if rank, held := arguments["rank"].(map[string]any); held && len(rank) == 0 {
			rank["before"] = items.priorities[0]
		}
		if len(arguments) == 1 {
			arguments["title"] = "A title the edit writes"
		}
	case "dispatch_mark_inbox_read", "dispatch_dismiss_inbox":
		if has("artifact") {
			arguments["artifact"] = items.notesID
		}
		if tool == "dispatch_mark_inbox_read" {
			break
		}
		narrowed := false
		for _, name := range []string{"dismiss_through", "kind", "priority", "urgency", "project", "issue", "artifact"} {
			narrowed = narrowed || has(name)
		}
		if narrowed {
			delete(arguments, "threads")
			delete(arguments, "seen_replies")
			if !has("dismiss_through") {
				arguments["dismiss_through"] = time.Now().UTC().Format(time.RFC3339)
			}
			break
		}
		arguments["threads"] = []any{"reply:" + items.comment}
		if arguments["restore"] == true {
			delete(arguments, "seen_replies")
		} else if !has("seen_replies") {
			arguments["seen_replies"] = map[string]any{"reply:" + items.comment: map[string]any{"version": 1}}
		}
	case "dispatch_answer_ask":
		if !has("selected") && !has("text") {
			arguments["text"] = "Ship it."
		}
	case "dispatch_reply_comment_delivery":
		if has("error") && !has("reply_to_block") {
			delete(arguments, "body")
		} else {
			delete(arguments, "error")
			if !has("body") {
				arguments["body"] = "On it."
			}
		}
	case "dispatch_set_agent_state":
		if len(arguments) == 1 {
			arguments["pinned"] = true
		}
	case "dispatch_name_version":
		if has("issue") {
			delete(arguments, "project")
			arguments["document"] = items.notesSlug
		} else if has("project") {
			arguments["document"] = items.guideSlug
		}
	case "dispatch_unsubscribe_document", "dispatch_keep_decision":
		if tool == "dispatch_keep_decision" {
			arguments["document"] = items.decisions
		} else {
			arguments["document"] = items.guideID
		}
	case "dispatch_mark_document_read":
		arguments["document"] = items.notesID
	}
}

// goingSchemaShapeRefusal reports whether a route's answer refuses a call for its shape: a 400,
// a 413 or a 422 by its status, or a code that names the input as wrong (INVALID_*, *_INPUT,
// *_REQUIRED, *VALIDATION*) at any status.
func goingSchemaShapeRefusal(status int, code string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return strings.HasPrefix(code, "INVALID_") || strings.HasSuffix(code, "_INPUT") ||
		strings.HasSuffix(code, "_REQUIRED") || strings.Contains(code, "VALIDATION")
}

// goingSchemaStateOnly are the write tools no call of which the seed can bring past a refusal of
// state, each with why. Their calls still reach their routes, and a refusal of shape still fails
// the test; every other tool writes at least one of its calls, so a seed that stops making what
// the calls name fails the test rather than leaving it to pass on 404s.
var goingSchemaStateOnly = map[string]string{
	"dispatch_unsubscribe_issue":       "a session's subscription is an interest the Envoy listener holds, and the stand-in listener holds none (404 SUBSCRIBER_NOT_FOUND)",
	"dispatch_unsubscribe_document":    "a session's subscription is an interest the Envoy listener holds, and the stand-in listener holds none (404 SUBSCRIBER_NOT_FOUND)",
	"dispatch_accept_message_delivery": "an accept takes the latest attempt of a direct message whose person asked the session to take it as its own turn, a request no route of the seed makes (409 ACCEPT_SUPERSEDED)",
	"dispatch_rebuild_document":        "a rebuild takes a document whose history does not load, which no route makes (409 DOCUMENT_LOADS)",
}

// TestEveryWriteToolsSchemaTakesOnlyWhatItsRouteTakes: every call each write tool's own schema
// takes (goingSchemaSets), made to satisfy the rules its spec states beside the schema
// (goingSchemaItemsOf.repair) and naming real items, goes through the tool's own translation to
// its route on the server's route table called in-process (GoingCaller.Do, as a run sends an
// approved row), as the person, or as a session for a tool only a session's bearer may call; and
// no route refuses one for its shape (goingSchemaShapeRefusal). Any other answer passes: a 404 for
// an item the call did not make, a 409 for a state the seed did not set up, a 403 for a person
// who may not. A call the bar never sends as written (goingUnsendable: a picture, a file, a link
// where the bar names the place, an option of a status change set to true, which a plan writes out
// as the rows of the issues it changes) is refused before any request, by design, and passes. A call
// naming its document by id (goingSchemaItemsOf.byID) writes, or fails the test. Every tool but
// goingSchemaStateOnly's writes at least one of its calls, and the log tallies what each tool's
// calls were answered.
func TestEveryWriteToolsSchemaTakesOnlyWhatItsRouteTakes(t *testing.T) {
	t.Parallel()
	rig := newGoingSchemaRig(t)
	seed := newGoingSchemaSeed(t, rig)
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	person, bearer := goingCallerOf(t, rig, false), goingCallerOf(t, rig, true)
	ctx := context.Background()
	calls := 0
	for _, spec := range contracts.DispatchTools {
		if _, writes := goingWriteTools[spec.Name]; !writes {
			continue
		}
		action, held := catalogue.Lookup(spec.Name)
		if !held {
			t.Fatalf("the catalogue holds no %s", spec.Name)
		}
		caller, session, bearerOnly := person, "", true
		for _, route := range action.Routes {
			bearerOnly = bearerOnly && route.auth == authBearer
			if slices.Contains(slices.Collect(maps.Values(route.path)), goingCallerSession) {
				// The calling session fills the row, which a person's run acts as none of: the
				// person calls it naming the planner session, as the catalogue's own rows are
				// executed (TestEveryGoingRowExecutesThroughTheCallerAgainstTheServedTable).
				session = goingPlanner
			}
		}
		if bearerOnly {
			caller, session = bearer, goingPlanner
		}
		t.Run(spec.Name, func(t *testing.T) {
			// The seed makes the tool's fresh items under the subtest, which a fixture's failure
			// ends alone.
			seed.t = t
			answered := map[string]int{}
			for index := 0; ; index++ {
				items := seed.items(spec.Name)
				sets := append(goingSchemaSets(spec.Arguments, items.value(seed, spec.Name)), items.byID(spec.Name)...)
				if index >= len(sets) {
					break
				}
				set := sets[index]
				if !set.byID {
					items.repair(spec.Name, set.arguments)
				}
				calls++
				sent, _ := json.Marshal(set.arguments)
				doer := rig.served.goingDoer(caller, testServerURL)
				response, err := GoingCaller{Catalogue: catalogue, Session: session, Doer: doer,
					knownSession: rig.served.goingKnownSession}.Do(ctx, spec.Name, set.arguments)
				var refused GoingArgumentError
				var read goingRefusal
				switch {
				case errors.As(err, &refused) && goingUnsendable(spec.Name, set.arguments) != "":
					answered["never sent, by design"]++
					continue
				case errors.As(err, &read):
					answered[fmt.Sprintf("a read refused %d %s", read.Status, read.Code)]++
					if set.byID || goingSchemaShapeRefusal(read.Status, read.Code) {
						t.Errorf("%s (%s) %s: a read its translation made was refused: status=%d body=%s", spec.Name, set.name, sent, read.Status, read.body)
					}
					continue
				case err != nil:
					answered["refused before its route"]++
					t.Errorf("%s (%s) %s: the call was refused before its route: %v", spec.Name, set.name, sent, err)
					continue
				}
				answer := new(bytes.Buffer)
				_, _ = answer.ReadFrom(response.Body)
				_ = response.Body.Close()
				var refusal errorBody
				_ = json.Unmarshal(answer.Bytes(), &refusal)
				if response.StatusCode < http.StatusMultipleChoices {
					answered["2xx"]++
					continue
				}
				if set.byID {
					t.Errorf("%s (%s) %s: status=%d body=%s, want the document its id names written", spec.Name, set.name, sent, response.StatusCode, answer.String())
				}
				answered[fmt.Sprintf("%d %s", response.StatusCode, refusal.Code)]++
				if goingSchemaShapeRefusal(response.StatusCode, refusal.Code) {
					t.Errorf("%s (%s) %s: status=%d body=%s", spec.Name, set.name, sent, response.StatusCode, answer.String())
				}
			}
			tally := make([]string, 0, len(answered))
			for outcome, count := range answered {
				tally = append(tally, fmt.Sprintf("%s ×%d", outcome, count))
			}
			slices.Sort(tally)
			t.Logf("%s: %s", spec.Name, strings.Join(tally, "; "))
			_, stateOnly := goingSchemaStateOnly[spec.Name]
			switch {
			case answered["2xx"] == 0 && !stateOnly:
				t.Errorf("%s wrote none of its calls, so none reached past a refusal of state: %s", spec.Name, strings.Join(tally, "; "))
			case answered["2xx"] > 0 && stateOnly:
				t.Errorf("%s wrote %d of its calls, so goingSchemaStateOnly names it no more", spec.Name, answered["2xx"])
			}
		})
	}
	t.Logf("%d calls", calls)
}
