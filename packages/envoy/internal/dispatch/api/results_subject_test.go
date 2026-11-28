package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// subjectChecks is a Verification document whose checks name what they read: k5 reads production
// and names the program it reads, k6 reads the graph and names no program, and k1 names neither.
const subjectChecks = ":::check{#k5 when=\"after_merge\" subject=\"production\" program=\"dispatch\"}\n" +
	"Production answers at the deploy's commit.\n" +
	":::\n\n" +
	":::check{#k6 when=\"default_branch\" subject=\"graph\"}\n" +
	"The graph answers at the default branch.\n" +
	":::\n\n" +
	":::check{#k1}\n" +
	"The server builds.\n" +
	":::\n"

// A check block names what it reads in `subject` (code, production or graph) and the deployed
// program it reads in `program`, and GET /api/v1/issues/{key}/results serves each as
// `checks[].subject` and `checks[].program`, as stored, leaving the key out where the block names
// none; a subject outside the three is refused as markdown outside the schema. This build writes
// both marks and serves them, so the document is written and read under the build's own schema.
// TestTheCheckMarksAreReadAtNAndWrittenFromNPlusOne (pmdoc/manifest_test.go) holds the version
// boundary: which version reads the marks and which writes them.
func TestACheckBlockServesItsSubjectAndProgram(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESSP", "Subject and program", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key

	uploadDocument(t, handler, owner, "verification.md", subjectChecks)
	staging := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts", map[string]any{
		"name": "staging.md", "content": ":::check{#k7 subject=\"staging\"}\nA subject outside the three.\n:::\n",
	}, "alice")
	requireMarkdownRefused(t, staging, "a subject outside the three", "must be one of code, production, graph, got \"staging\"")

	checks := servedCheckMembers(t, handler, issue.Key)
	requireCheckMember(t, checks, "k5", "subject", `"production"`)
	requireCheckMember(t, checks, "k5", "program", `"dispatch"`)
	requireCheckMember(t, checks, "k6", "subject", `"graph"`)
	requireCheckMember(t, checks, "k6", "program", "")
	requireCheckMember(t, checks, "k1", "subject", "")
	requireCheckMember(t, checks, "k1", "program", "")
}

// An author marks a check they wrote before: the Verification document uploaded again whole,
// naming the token it stood at after a pass of the check was recorded, with `subject` and
// `program` added to the check's opening line, is taken under this build's own schema, with no
// manifest raised. The results read then serves the check's subject and program, and its serves,
// surface, run, when and negative_control as they were before the edit. A check's digest covers
// its whole block, its opening line included, so the pass recorded before the edit reads
// check_words_moved true.
func TestAnAuthorWritesACheckSubjectAndProgram(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESAW", "An author's marks", twoRequirements)
	owner := "/api/v1/issues/" + issue.Key
	opening := `:::check{#k8 serves="dispatch://` + issue.Key + `/spec#b-r1" surface="cli"` +
		` run="go test ./internal/dispatch/api" when="after_merge" negative_control="Stop the server and watch the read fail."`
	words := "\nProduction answers the results read at the deploy's commit.\n:::\n"
	verification := uploadDocument(t, handler, owner, "verification.md", opening+"}"+words)
	kept := []struct{ name, want string }{
		{"serves", `["dispatch://` + issue.Key + `/spec#b-r1"]`},
		{"surface", `"cli"`},
		{"run", `"go test ./internal/dispatch/api"`},
		{"when", `"after_merge"`},
		{"negative_control", `"Stop the server and watch the read fail."`},
	}
	before := servedCheckMembers(t, handler, issue.Key)
	requireCheckMember(t, before, "k8", "subject", "")
	requireCheckMember(t, before, "k8", "program", "")
	for _, member := range kept {
		requireCheckMember(t, before, "k8", member.name, member.want)
	}

	recorded := recordResult(t, handler, issue.Key, resultBody("k8", "pass", mergeCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a pass of k8: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	pass := decodeBody[recordedResult](t, recorded).Result
	token := uploadBase(t, handler, verification.Artifact.ID)

	edited := postUpload(t, handler, issue.Key, map[string]any{
		"name":         "verification.md",
		"content":      opening + ` subject="production" program="dispatch"}` + words,
		"precondition": token,
	})
	if edited.Code != http.StatusCreated {
		t.Fatalf("the author's upload adding subject and program to k8 under the token %q: status=%d body=%s, want 201",
			token["document"], edited.Code, edited.Body.String())
	}

	after := servedCheckMembers(t, handler, issue.Key)
	requireCheckMember(t, after, "k8", "subject", `"production"`)
	requireCheckMember(t, after, "k8", "program", `"dispatch"`)
	for _, member := range kept {
		requireCheckMember(t, after, "k8", member.name, member.want)
	}
	switch moved := resultRead(t, readResults(t, handler, issue.Key), "k8", pass.ID).CheckWordsMoved; {
	case moved == nil:
		t.Errorf("the pass recorded at k8's version %d carries no check_words_moved after the edit, want true", pass.CheckVersion)
	case !*moved:
		t.Errorf("the pass recorded at k8's version %d reads check_words_moved false after the edit, want true: a check's digest covers its opening line",
			pass.CheckVersion)
	}
}

// requireMarkdownRefused holds a document write to 400 INVALID_MARKDOWN, the caller's markdown to
// fix, whose message says want.
func requireMarkdownRefused(t *testing.T, response *httptest.ResponseRecorder, what, want string) {
	t.Helper()
	answer := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusBadRequest || answer.Code != "INVALID_MARKDOWN" || !strings.Contains(answer.Error, want) {
		t.Errorf("%s: status=%d body=%s, want 400 INVALID_MARKDOWN saying %q", what, response.Code, response.Body.String(), want)
	}
}

// servedCheckMembers is each check of the issue's results read, by block id, as the route writes
// it: its members as raw JSON, so a key the route leaves out is told from an empty value.
func servedCheckMembers(t *testing.T, handler http.Handler, issue string) map[string]map[string]json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	read := decodeBody[struct {
		Checks []map[string]json.RawMessage `json:"checks"`
	}](t, response)
	checks := map[string]map[string]json.RawMessage{}
	for _, check := range read.Checks {
		var id string
		if err := json.Unmarshal(check["block_id"], &id); err != nil {
			t.Fatalf("read a check's block id from %s: %v", response.Body.String(), err)
		}
		checks[id] = check
	}
	return checks
}

// requireCheckMember holds check blockID's member name to want, its JSON as the route wrote it,
// where an empty want is the key left out.
func requireCheckMember(t *testing.T, checks map[string]map[string]json.RawMessage, blockID, name, want string) {
	t.Helper()
	check, ok := checks[blockID]
	if !ok {
		t.Fatalf("the results read serves no check %s", blockID)
	}
	held, present := check[name]
	switch {
	case want == "" && present:
		t.Errorf("check %s serves %s = %s, want the key left out", blockID, name, held)
	case want != "" && string(held) != want:
		t.Errorf("check %s serves %s = %s (present %t), want %s", blockID, name, held, present, want)
	}
}
