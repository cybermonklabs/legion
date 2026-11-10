package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// treesRequest is a GET as the dashboard makes it when trees is true (`Dispatch-Trees: 1`) and as
// every other client makes it when it is false.
func treesRequest(t *testing.T, handler http.Handler, target string, trees bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if trees {
		request.Header.Set("Dispatch-Trees", "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s (trees=%v): status=%d body=%s", target, trees, response.Code, response.Body.String())
	}
	return response
}

// valueAt is the JSON value standing at a dotted path of the response body, and whether the body
// holds it at all. A number in the path indexes a list.
func valueAt(t *testing.T, response *httptest.ResponseRecorder, path string) (json.RawMessage, bool) {
	t.Helper()
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode the answer %s: %v", response.Body.String(), err)
	}
	for _, step := range strings.Split(path, ".") {
		switch held := value.(type) {
		case map[string]any:
			next, ok := held[step]
			if !ok {
				return nil, false
			}
			value = next
		case []any:
			index := 0
			for _, digit := range step {
				index = index*10 + int(digit-'0')
			}
			if index >= len(held) {
				return nil, false
			}
			value = held[index]
		default:
			return nil, false
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the value at %s: %v", path, err)
	}
	return encoded, true
}

// requireDisplayTree fails unless the answer holds no tree at treePath without the header, and
// the tree ReadForDisplay makes of the text standing at textPath with it. The text is read from
// the answer itself, so the check is against what Dispatch stored rather than what the test sent.
func requireDisplayTree(t *testing.T, handler http.Handler, target, textPath, treePath string) {
	t.Helper()
	bare := treesRequest(t, handler, target, false)
	if _, held := valueAt(t, bare, treePath); held {
		t.Errorf("GET %s without the header carries %s", target, treePath)
	}
	raw, held := valueAt(t, bare, textPath)
	if !held {
		t.Fatalf("GET %s carries no %s", target, textPath)
	}
	var markdown string
	if err := json.Unmarshal(raw, &markdown); err != nil {
		t.Fatalf("GET %s: %s is %s, not a text", target, textPath, raw)
	}
	got, held := valueAt(t, treesRequest(t, handler, target, true), treePath)
	if !held {
		t.Fatalf("GET %s with the header carries no %s", target, treePath)
	}
	want := sameShape(t, pmdoc.DisplayJSON(markdown, pmdoc.DisplayLatest))
	if !bytes.Equal(got, want) {
		t.Errorf("GET %s: %s is\n\t%s\nwant ReadForDisplay's tree of %q\n\t%s", target, treePath, got, markdown, want)
	}
}

// sameShape is tree's encoding with its keys in the order valueAt's own round trip leaves them,
// so two encodings of one tree compare as the one value they are.
func sameShape(t *testing.T, tree json.RawMessage) json.RawMessage {
	t.Helper()
	var value any
	if err := json.Unmarshal(tree, &value); err != nil {
		t.Fatalf("decode the tree %s: %v", tree, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the tree: %v", err)
	}
	return encoded
}

// Every route class the dashboard reads stored text from carries that text's display tree beside
// it when, and only when, the request holds `Dispatch-Trees: 1`, and the tree is the one
// pmdoc.ReadForDisplay makes of exactly that text, and the document the route table emits
// declares that tree where the answer carries it. The routes covered here are the issue's
// comment list and one comment's thread, the issue's ask list and one ask, the issue's message
// list, the issue's event log, a document's text, the caller's own answers, a project's
// architecture and one node's references; the search snippet, whose tree is cut and marked
// rather than read whole, has its own test below.
func TestEveryReadCarriesItsDisplayTreesOnlyUnderTheHeader(t *testing.T) {
	handler := newTestHandler(t)
	const (
		commentBody  = "Please check **this** before the deploy."
		messageBody  = "The *staging* run is green."
		askQuestion  = "Ship the ~~draft~~ **release** today?"
		optionLabel  = "Yes, `ship it`"
		answerText   = "Shipping **now**."
		documentText = "# Spec\n\nThe **plan** is written here.\n"
	)
	issue := createInteractionIssue(t, handler, "TREE", "Display trees", documentText)

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": commentBody}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": messageBody}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	messageID := decodeBody[model.Message](t, message).ID

	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": askQuestion,
		"options":  []map[string]string{{"label": optionLabel}},
	}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[model.Ask](t, ask).ID
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{optionLabel}, "text": answerText}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	// Each row is read twice: the live answer carries the tree, and the OpenAPI document's own
	// 200 schema for the route declares it there. The second reading is what holds a route row
	// to the shaped read its handler writes (routes_shapes.go), since a row naming the model
	// inside that read declares no tree at all. The architecture tree and one node's references
	// are rows of the schema reading alone - the trees they carry need an architecture source
	// and a stored reference this issue has neither of - and they are the two rows that were
	// found naming the model (INFRA-635). A decision's text is a row of the schema reading too:
	// its live reading is the decisions view's own test, which has a decisions.md to read
	// (decisions_test.go).
	node, _ := readOpenAPITree(t)
	document, _ := node.(map[string]any)
	for _, test := range []struct{ name, pattern, target, textPath, treePath string }{
		{"the issue's comment list", "/api/v1/issues/{key}/comments",
			"/api/v1/issues/" + issue.Key + "/comments", "0.body", "0.body_tree"},
		{"one comment's thread", "/api/v1/comments/{id}",
			"/api/v1/comments/" + commentID, "comment.body", "comment.body_tree"},
		{"the issue's message list", "/api/v1/issues/{key}/messages",
			"/api/v1/issues/" + issue.Key + "/messages", "0.message.body", "0.message.body_tree"},
		{"one message's thread", "/api/v1/messages/{id}",
			"/api/v1/messages/" + messageID, "message.body", "message.body_tree"},
		{"the issue's ask list", "/api/v1/issues/{key}/asks",
			"/api/v1/issues/" + issue.Key + "/asks", "0.question", "0.question_tree"},
		{"an ask option in the list", "/api/v1/issues/{key}/asks",
			"/api/v1/issues/" + issue.Key + "/asks", "0.options.0.label", "0.options.0.label_tree"},
		{"one ask", "/api/v1/asks/{id}", "/api/v1/asks/" + askID, "ask.question", "ask.question_tree"},
		{"one ask's answer", "/api/v1/asks/{id}", "/api/v1/asks/" + askID, "ask.answer.text", "ask.answer.text_tree"},
		{"the caller's answer history", "/api/v1/me/answers",
			"/api/v1/me/answers", "rows.0.question", "rows.0.question_tree"},
		{"a document's text", "/api/v1/artifacts/{id}/text",
			"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text", "markdown", "markdown_tree"},
		{"a project's architecture", "/api/v1/projects/{key}/architecture", "", "", "components.0.prose_tree"},
		{"a node's own text", "/api/v1/references", "", "", "node.text_tree"},
		{"the text a reference pinned", "/api/v1/references", "", "", "node.pin.text_tree"},
		{"a decision's text", "/api/v1/issues/{key}/decisions", "", "", "decisions.0.text_tree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.target != "" {
				requireDisplayTree(t, handler, test.target, test.textPath, test.treePath)
			}
			requireTreeInAnswerSchema(t, document, test.pattern, test.treePath)
		})
	}

	// The event log's trees ride in the envelope's own map, keyed by the payload field's path.
	t.Run("the issue's event log", func(t *testing.T) {
		target := "/api/v1/issues/" + issue.Key + "/events"
		bare := treesRequest(t, handler, target, false)
		if strings.Contains(bare.Body.String(), `"trees"`) {
			t.Errorf("GET %s without the header carries trees: %s", target, bare.Body.String())
		}
		var events []struct {
			Type  string                     `json:"type"`
			Trees map[string]json.RawMessage `json:"trees"`
		}
		if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &events); err != nil {
			t.Fatalf("decode the event log: %v", err)
		}
		want := map[string]json.RawMessage{
			"comment.created": pmdoc.DisplayJSON(commentBody, pmdoc.DisplayLatest),
			"message.created": pmdoc.DisplayJSON(messageBody, pmdoc.DisplayLatest),
		}
		seen := map[string]bool{}
		for _, event := range events {
			if tree, carried := want[event.Type]; carried {
				seen[event.Type] = true
				if !bytes.Equal(event.Trees["payload.body"], tree) {
					t.Errorf("%s trees[payload.body] = %s, want %s", event.Type, event.Trees["payload.body"], tree)
				}
			}
			if event.Type == "ask.opened" {
				if !bytes.Equal(event.Trees["payload.question"], pmdoc.DisplayJSON(askQuestion, pmdoc.DisplayLatest)) {
					t.Errorf("ask.opened trees[payload.question] = %s", event.Trees["payload.question"])
				}
				if !bytes.Equal(event.Trees["payload.options.0.label"], pmdoc.DisplayJSON(optionLabel, pmdoc.DisplayLatest)) {
					t.Errorf("ask.opened trees[payload.options.0.label] = %s", event.Trees["payload.options.0.label"])
				}
				seen["ask.opened"] = true
			}
		}
		for _, kind := range []string{"comment.created", "message.created", "ask.opened"} {
			if !seen[kind] {
				t.Errorf("the event log carried no %s", kind)
			}
		}
	})
}

// requireTreeInAnswerSchema fails unless the emitted document declares the display tree at
// treePath of the 200 answer the GET of pattern gives. Every path a reader walks here is one a
// live answer carries, so the two readings of a row hold the document to what the handler
// writes.
func requireTreeInAnswerSchema(t *testing.T, document map[string]any, pattern, treePath string) {
	t.Helper()
	schema := openAPIAnswerSchema(t, document, pattern)
	for _, step := range strings.Split(treePath, ".") {
		child := openAPISchemaChild(document, schema, step)
		if child == nil {
			t.Fatalf("the 200 schema of GET %s reaches no %q on the way to %s", pattern, step, treePath)
		}
		schema = child
	}
	want := displayTreeSchema(t)
	if !reflect.DeepEqual(schema, want) {
		t.Errorf("GET %s declares %s as %v, and the display tree is %v", pattern, treePath, schema, want)
	}
}

// openAPIAnswerSchema is the JSON body the document says the GET of pattern answers 200 with.
func openAPIAnswerSchema(t *testing.T, document map[string]any, pattern string) map[string]any {
	t.Helper()
	paths, _ := document["paths"].(map[string]any)
	item, _ := paths[pattern].(map[string]any)
	get, _ := item["get"].(map[string]any)
	responses, _ := get["responses"].(map[string]any)
	answer, _ := responses["200"].(map[string]any)
	content, _ := answer["content"].(map[string]any)
	body, _ := content["application/json"].(map[string]any)
	schema, declared := body["schema"].(map[string]any)
	if !declared {
		t.Fatalf("the document declares no JSON 200 answer for GET %s", pattern)
	}
	return schema
}

// openAPISchemaChild is the schema one step of an answer path stands at: a name is a property of
// an object, a number an item of a list. A reference is followed to what it names, and a schema
// written as a choice of several - a nilable field is its own shape or null - is searched
// through, since the path stands on whichever arm declares it.
func openAPISchemaChild(document, schema map[string]any, step string) map[string]any {
	if reference, names := schema["$ref"].(string); names {
		components, _ := document["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		declared, found := schemas[strings.TrimPrefix(reference, "#/components/schemas/")].(map[string]any)
		if !found {
			return nil
		}
		return openAPISchemaChild(document, declared, step)
	}
	if _, isIndex := strconv.Atoi(step); isIndex == nil {
		if items, holds := schema["items"].(map[string]any); holds {
			return items
		}
	} else if properties, holds := schema["properties"].(map[string]any); holds {
		if child, declared := properties[step].(map[string]any); declared {
			return child
		}
	}
	for _, choice := range []string{"anyOf", "allOf", "oneOf"} {
		arms, _ := schema[choice].([]any)
		for _, arm := range arms {
			armSchema, isObject := arm.(map[string]any)
			if !isObject {
				continue
			}
			if child := openAPISchemaChild(document, armSchema, step); child != nil {
				return child
			}
		}
	}
	return nil
}

// displayTreeSchema is what model.DisplayTree tells the emitter its bytes are, which is the
// schema every tree field of the document carries.
func displayTreeSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := model.DisplayTree{}.JSONSchemaBytes()
	if err != nil {
		t.Fatalf("the display tree's own schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("read the display tree's own schema: %v", err)
	}
	return schema
}

// displayTreeFields names every field of row, and of every row it holds, whose JSON name ends
// in `_tree`, each under the path it stands at. A type is walked once, which is enough to say
// whether any tree stands on it.
func displayTreeFields(row reflect.Type, path string, seen map[reflect.Type]bool) []string {
	for row.Kind() == reflect.Pointer || row.Kind() == reflect.Slice || row.Kind() == reflect.Array {
		row = row.Elem()
	}
	if row.Kind() != reflect.Struct || seen[row] {
		return nil
	}
	seen[row] = true
	var found []string
	for index := range row.NumField() {
		field := row.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		at := name
		if path != "" {
			at = path + "." + name
		}
		if strings.HasSuffix(name, "_tree") {
			found = append(found, at)
		}
		found = append(found, displayTreeFields(field.Type, at, seen)...)
	}
	return found
}

// A display tree stands on the shape a read answers and never on the row the whole deployment
// shares, because an event payload carries that row (the spec's R6: the trees "are not part of
// the message-bus schemas"). A document's approval is the case that was got wrong: the
// `artifact.created` payload is the document row itself (artifactCreatedEventPayload), so a
// `reason_tree` on model.ArtifactApproval would be a response field published on the bus. Here
// one document is reviewed, its read answers the tree of the reason, and the payload of its own
// creation - and of every other event of its issue - carries none.
func TestAnArtifactPayloadCarriesNoApprovalTreeThoughItsReadDoes(t *testing.T) {
	for _, row := range []any{
		model.Artifact{}, model.ArtifactApproval{},
		model.GraphNode{}, model.BlockPin{}, model.ArchitectureTreeComponent{},
	} {
		shared := reflect.TypeOf(row)
		if fields := displayTreeFields(shared, "", map[reflect.Type]bool{}); len(fields) > 0 {
			t.Errorf("model.%s carries the display tree %v; a read's own shape holds it (display_trees.go)",
				shared.Name(), fields)
		}
	}

	handler := newTestHandler(t)
	const reason = "The **rollback** step is missing."
	issue := createInteractionIssue(t, handler, "PAYL", "Payload trees", "# Spec\n\nThe plan.\n")
	document := uploadIssueDocument(t, handler, issue.Key, "Rollout plan.md", "# Rollout\n\nFirst.\n")
	review := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/reviews",
		map[string]any{"state": "changes_requested", "reason": reason}, "alice")
	if review.Code != http.StatusCreated && review.Code != http.StatusOK {
		t.Fatalf("request changes: status=%d body=%s", review.Code, review.Body.String())
	}

	requireDisplayTree(t, handler, "/api/v1/artifacts/"+document.ID,
		"approval.reason", "approval.reason_tree")

	target := "/api/v1/issues/" + issue.Key + "/events"
	var events []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &events); err != nil {
		t.Fatalf("decode the event log: %v", err)
	}
	carried := false
	for _, event := range events {
		payload := string(event.Payload)
		if event.Type == "artifact.created" && strings.Contains(payload, document.ID) {
			carried = true
		}
		if strings.Contains(payload, "_tree") {
			t.Errorf("the %s payload carries a display tree: %s", event.Type, payload)
		}
	}
	if !carried {
		t.Error("the issue's event log carried no artifact.created of the reviewed document, whose payload is its row")
	}
}

// Every answer says its body depends on the trees header and on the encoding the caller takes, so
// no cache between a browser and Dispatch hands a reader with trees an answer written for a reader
// without them, or a caller that takes no gzip one that was compressed.
func TestEveryAnswerVariesOnTheTreesHeaderAndTheEncoding(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "VARY", "Vary", "A spec")
	for _, target := range []string{"/api/v1/issues/" + issue.Key, "/api/v1/issues/" + issue.Key + "/comments", "/api/v1/projects"} {
		response := treesRequest(t, handler, target, false)
		got := slices.Sorted(slices.Values(response.Header().Values("Vary")))
		if !slices.Equal(got, []string{"Accept-Encoding", "Dispatch-Trees"}) {
			t.Errorf("GET %s: Vary = %v, want Dispatch-Trees and Accept-Encoding", target, got)
		}
	}
}

// A search hit's snippet carries the tree of the headline itself, balanced as the cut left it and
// with the match marked, and carries it only under the header.
func TestASearchHitCarriesTheTreeOfItsSnippet(t *testing.T) {
	handler := newTestHandler(t)
	createInteractionIssue(t, handler, "SNIP", "Astrolabe", "The **astrolabe** measures altitude.")
	const target = "/api/v1/search?q=astrolabe"

	var bare model.SearchResponse
	if err := json.Unmarshal(treesRequest(t, handler, target, false).Body.Bytes(), &bare); err != nil {
		t.Fatalf("decode the search answer: %v", err)
	}
	if len(bare.Results) == 0 {
		t.Fatalf("the search found nothing: %s", treesRequest(t, handler, target, false).Body.String())
	}
	for _, result := range bare.Results {
		if result.SnippetTree != nil {
			t.Errorf("a hit carries snippet_tree without the header: %s", result.SnippetTree)
		}
	}

	var marked model.SearchResponse
	if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &marked); err != nil {
		t.Fatalf("decode the search answer: %v", err)
	}
	for _, result := range marked.Results {
		if result.SnippetTree == nil {
			t.Fatalf("hit %s carries no snippet_tree", result.ID)
		}
		highlighted := strings.Join(highlightedText(t, json.RawMessage(result.SnippetTree)), " ")
		if !strings.Contains(strings.ToLower(highlighted), "astrolabe") {
			t.Errorf("hit %s highlights %q, want the matched word", result.ID, highlighted)
		}
		if strings.Contains(treeText(t, json.RawMessage(result.SnippetTree)), markStart) {
			t.Errorf("hit %s keeps a sentinel in its tree: %s", result.ID, result.SnippetTree)
		}
	}
}

// snippetNode is a display tree as a test reads it: enough of a ProseMirror node to walk its text.
type snippetNode struct {
	Type    string        `json:"type"`
	Text    string        `json:"text"`
	Marks   []snippetMark `json:"marks"`
	Content []snippetNode `json:"content"`
}

type snippetMark struct {
	Type string `json:"type"`
}

// treeText is every character of text the tree holds, in document order.
func treeText(t *testing.T, tree json.RawMessage) string {
	t.Helper()
	var root snippetNode
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	var out strings.Builder
	var walk func(snippetNode)
	walk = func(node snippetNode) {
		out.WriteString(node.Text)
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	return out.String()
}

// highlightedText is the text of every node the snippet tree marks as what the query matched.
func highlightedText(t *testing.T, tree json.RawMessage) []string {
	t.Helper()
	var root snippetNode
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	var found []string
	var walk func(snippetNode)
	walk = func(node snippetNode) {
		for _, mark := range node.Marks {
			if mark.Type == pmdoc.DisplayHighlightMark {
				found = append(found, node.Text)
				break
			}
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	return found
}

// What the trees cost a reader, measured as a proxy sees it: one issue's comment list gzipped
// with and without the header. The numbers are logged rather than bounded - a tree's size is the
// text's - and the check is only that the trees are what grew the answer.
func TestTheTreesCostOfACommentList(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SIZE", "Comment list size", "A spec")
	for index := range 20 {
		body := "Comment " + strconv.Itoa(index) + ": the **deploy** is `green`, see [the run](https://example.com/run) " +
			"and the *staging* check.\n\n- one item\n- another item\n"
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": body}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create comment %d: status=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	target := "/api/v1/issues/" + issue.Key + "/comments"
	gzipped := func(trees bool) int {
		body := treesRequest(t, handler, target, trees).Body.Bytes()
		var squeezed bytes.Buffer
		writer := gzip.NewWriter(&squeezed)
		if _, err := writer.Write(body); err != nil {
			t.Fatalf("gzip the answer: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close the gzip writer: %v", err)
		}
		t.Logf("%s trees=%v: %d bytes, %d gzipped", target, trees, len(body), squeezed.Len())
		return squeezed.Len()
	}
	without, with := gzipped(false), gzipped(true)
	if with <= without {
		t.Errorf("the answer with trees gzips to %d bytes, not over the %d without them", with, without)
	}
}

// commentsToFill is a list long enough that buildDisplayTrees reads it on every processor, each
// body its own text so every tree is a build of its own.
func commentsToFill() []model.Comment {
	comments := make([]model.Comment, 64)
	for index := range comments {
		number := strconv.Itoa(index)
		comments[index] = model.Comment{
			ID:   "comment-" + number,
			Body: "Comment **" + number + "** of the list.\n\n- an item\n\n> a quoted line\n",
		}
	}
	return comments
}

// builtShapedComments is a comment list shaped over a reader that reads every text where it
// stands, which is the list route's shaping with no table behind it.
func builtShapedComments(comments []model.Comment, trees bool) []shapedComment {
	reader := builtDisplayTrees(trees)
	reads := shapedComments(comments, reader)
	reader.resolve()
	return reads
}

// The parallel reading answers exactly what reading each text where it stands answered: the same
// trees, each against its own row, in the order the rows came in (buildDisplayTrees).
func TestTheParallelFillAnswersTheTreeOfEveryRowInOrder(t *testing.T) {
	comments := commentsToFill()
	reads := builtShapedComments(comments, true)
	if len(reads) != len(comments) {
		t.Fatalf("the read list holds %d rows, want %d", len(reads), len(comments))
	}
	for index, read := range reads {
		if read.ID != comments[index].ID {
			t.Fatalf("row %d is %s, want %s", index, read.ID, comments[index].ID)
		}
		want := pmdoc.DisplayJSON(comments[index].Body, pmdoc.DisplayLatest)
		if !bytes.Equal(read.BodyTree, want) {
			t.Errorf("row %d reads %s, want %s", index, read.BodyTree, want)
		}
	}
}

// A list with no trees asked for has no reader at all and carries none.
func TestAListWithoutTheHeaderCarriesNoTree(t *testing.T) {
	for index, read := range builtShapedComments(commentsToFill(), false) {
		if read.BodyTree != nil {
			t.Fatalf("row %d carries a tree without the header: %s", index, read.BodyTree)
		}
	}
}

// Several readers of one list at once are answered the same trees: each reader's goroutines
// write only their own text's slot, and one text is built once however many readers want it
// (displayTreeBuilds). Run under -race, this is the check that the reading is safe.
func TestReadersOfOneListAtOnceAreAnsweredTheSameTrees(t *testing.T) {
	comments := commentsToFill()
	want := builtShapedComments(comments, true)
	answers := make([][]shapedComment, 8)
	var readers sync.WaitGroup
	for reader := range answers {
		readers.Add(1)
		go func() {
			defer readers.Done()
			answers[reader] = builtShapedComments(comments, true)
		}()
	}
	readers.Wait()
	for reader, answer := range answers {
		for index, read := range answer {
			if !bytes.Equal(read.BodyTree, want[index].BodyTree) {
				t.Fatalf("reader %d reads row %d as %s, want %s", reader, index, read.BodyTree, want[index].BodyTree)
			}
		}
	}
}
