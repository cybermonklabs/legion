// going_in_process_test.go holds what a request of the bar's in-process caller is held to beyond a
// request off the network: a member its route does not declare is refused by name rather than
// ignored, since the bar and the route it calls are always one build.
package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// goingInProcessSend sends spec through doer, the route table called in-process, and answers the
// status and the body of its answer.
func goingInProcessSend(t *testing.T, doer GoingDoer, spec goingSend) (int, string) {
	t.Helper()
	request, err := spec.build(context.Background(), "")
	if err != nil {
		t.Fatalf("build %s %s: %v", spec.method, spec.path, err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatalf("send %s %s: %v", spec.method, spec.path, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

// TestGoingInProcessRequestsRefuseAMemberTheirRouteDoesNotDeclare is c8's negative control under
// main's lenient decoding (decodeJSON): a request the bar sends in-process naming a member its
// route does not declare is refused, 400 INVALID_JSON naming the member, with nothing written,
// where the same body off the network is taken with the member ignored. A translator that sent
// the ask a reply answers under `reply_to_ask` rather than the route's `ask_id` would otherwise
// post the reply as a plain comment; a row whose translation is removed, so that it sends the
// tool's own arguments, is refused by its route; and an upload's JSON member and multipart field
// are held to the same.
func TestGoingInProcessRequestsRefuseAMemberTheirRouteDoesNotDeclare(t *testing.T) {
	rig := newGoingWritesRig(t)
	issue := createInteractionIssue(t, rig.mux, "DEMO", "An issue to reply on", "The spec.\n").Key
	ask := rig.ask(t, issue, "Ship it on Friday?")
	doer := rig.served.goingDoer(goingCallerOf(t, rig, false), testServerURL)
	comments := func(t *testing.T, body string) (all, underTheAsk int) {
		t.Helper()
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select count(*), count(*) filter (where ask_id is not null) from comments where issue_key = $1 and body = $2`,
			issue, body).Scan(&all, &underTheAsk); err != nil {
			t.Fatalf("count the comments: %v", err)
		}
		return all, underTheAsk
	}

	t.Run("a member under a name the route does not declare", func(t *testing.T) {
		const words = "A reply under a misnamed member."
		spec, err := goingJSONRequest(http.MethodPost, goingIssuePath(issue, "comments"), map[string]any{"body": words, "reply_to_ask": ask})
		if err != nil {
			t.Fatal(err)
		}
		status, answer := goingInProcessSend(t, doer, spec)
		if status != http.StatusBadRequest || !strings.Contains(answer, "INVALID_JSON") || !strings.Contains(answer, "reply_to_ask") {
			t.Fatalf("the in-process comment naming reply_to_ask answered %d %s, want 400 INVALID_JSON naming reply_to_ask", status, answer)
		}
		if all, _ := comments(t, words); all != 0 {
			t.Fatalf("the refused in-process comment wrote %d comments, want none", all)
		}
		// Off the network the member is ignored, as decodeJSON reads every body: the refusal is
		// the in-process request's alone.
		if response := rig.browser(t, "alice", http.MethodPost, goingIssuePath(issue, "comments"), map[string]any{"body": words, "reply_to_ask": ask}); response.Code != http.StatusCreated {
			t.Fatalf("the same comment from the browser answered %d %s, want 201 with the member ignored", response.Code, response.Body.String())
		}
		if all, underTheAsk := comments(t, words); all != 1 || underTheAsk != 0 {
			t.Fatalf("the browser's comment wrote %d comments, %d under the ask, want one plain comment", all, underTheAsk)
		}
	})

	t.Run("a row whose translation is removed", func(t *testing.T) {
		catalogue, err := BuildGoingCatalogue()
		if err != nil {
			t.Fatal(err)
		}
		arguments := func(words string) map[string]any {
			return map[string]any{"issue": issue, "body": words, "reply_to_ask": ask}
		}
		// dispatch_comment's translation only renames: reply_to_ask is the route's ask_id.
		const translated = "A reply the translation sends under the ask."
		response, err := GoingCaller{Catalogue: catalogue, Doer: doer}.Do(context.Background(), "dispatch_comment", arguments(translated))
		if err != nil || response.StatusCode != http.StatusCreated {
			t.Fatalf("the translated comment answered %v, %v, want 201", response, err)
		}
		if all, underTheAsk := comments(t, translated); all != 1 || underTheAsk != 1 {
			t.Fatalf("the translated comment wrote %d comments, %d under the ask, want one reply under the ask", all, underTheAsk)
		}
		index := catalogue.byAction["dispatch_comment"]
		for row := range catalogue.actions[index].Routes {
			catalogue.actions[index].Routes[row].translate = goingArgumentsAsBody
		}
		const raw = "A reply sent as the tool's own arguments."
		response, err = GoingCaller{Catalogue: catalogue, Doer: doer}.Do(context.Background(), "dispatch_comment", arguments(raw))
		if err != nil {
			t.Fatalf("send the untranslated comment: %v", err)
		}
		answer, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(answer), "INVALID_JSON") || !strings.Contains(string(answer), "reply_to_ask") {
			t.Fatalf("the untranslated comment answered %d %s, want its route's 400 INVALID_JSON naming reply_to_ask", response.StatusCode, answer)
		}
		if all, _ := comments(t, raw); all != 0 {
			t.Fatalf("the untranslated comment wrote %d comments, want none", all)
		}
	})

	t.Run("an upload's JSON member and multipart field", func(t *testing.T) {
		documents := func(t *testing.T, name string) int {
			t.Helper()
			var count int
			if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from artifacts where name = $1`, name).Scan(&count); err != nil {
				t.Fatalf("count the documents: %v", err)
			}
			return count
		}
		spec, err := goingJSONRequest(http.MethodPost, goingIssuePath(issue, "artifacts"), map[string]any{
			"name": "misnamed.md", "content": "# Misnamed\n", "sumary": "A summary under a misspelt member.",
		})
		if err != nil {
			t.Fatal(err)
		}
		status, answer := goingInProcessSend(t, doer, spec)
		if status != http.StatusBadRequest || !strings.Contains(answer, "INVALID_JSON") || !strings.Contains(answer, "sumary") {
			t.Fatalf("the in-process JSON upload naming sumary answered %d %s, want 400 INVALID_JSON naming sumary", status, answer)
		}
		if count := documents(t, "misnamed.md"); count != 0 {
			t.Fatalf("the refused JSON upload stored %d documents, want none", count)
		}
		form := goingUpload{fields: map[string]string{"name": "misnamed.txt", "sumary": "A summary under a misspelt field."},
			filename: "misnamed.txt", contentType: "text/plain; charset=utf-8", content: []byte("Plain words.")}
		body, contentType, err := form.encode()
		if err != nil {
			t.Fatal(err)
		}
		status, answer = goingInProcessSend(t, doer, goingSend{method: http.MethodPost,
			path: "/api/v1/agents/" + goingPlanner + "/artifacts", contentType: contentType, body: body})
		if status != http.StatusBadRequest || !strings.Contains(answer, "INVALID_JSON") || !strings.Contains(answer, "sumary") {
			t.Fatalf("the in-process multipart upload naming sumary answered %d %s, want 400 INVALID_JSON naming sumary", status, answer)
		}
		if count := documents(t, "misnamed.txt"); count != 0 {
			t.Fatalf("the refused multipart upload stored %d files, want none", count)
		}
		// The same form declared whole is taken, so the refusal above is the field's alone.
		form.fields = map[string]string{"name": "declared.txt", "summary": "A summary under its own field."}
		form.filename = "declared.txt"
		if body, contentType, err = form.encode(); err != nil {
			t.Fatal(err)
		}
		status, answer = goingInProcessSend(t, doer, goingSend{method: http.MethodPost,
			path: "/api/v1/agents/" + goingPlanner + "/artifacts", contentType: contentType, body: body})
		if status != http.StatusCreated {
			t.Fatalf("the in-process multipart upload with its own fields answered %d %s, want 201", status, answer)
		}
	})
}
