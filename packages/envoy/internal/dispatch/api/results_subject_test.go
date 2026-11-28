package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
// none; a subject outside the three is refused as markdown outside the schema. The document is
// written under a manifest that creates both marks, this build's raised one version
// (pmdoc.UseManifestForTest), and read after the restore, so the test asserts nothing of the
// build's own version: the build that reads the marks and does not create them yet, and the build
// that creates them, each pass it. TestTheCheckMarksAreReadAtNAndWrittenFromNPlusOne holds which
// version reads them and which writes them.
//
// It stays serial: the manifest is the package's, and every other test reads it.
func TestACheckBlockServesItsSubjectAndProgram(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESSP", "Subject and program", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key

	restore := useTheNextReleasesManifest(t)
	uploadDocument(t, handler, owner, "verification.md", subjectChecks)
	staging := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts", map[string]any{
		"name": "staging.md", "content": ":::check{#k7 subject=\"staging\"}\nA subject outside the three.\n:::\n",
	}, "alice")
	requireMarkdownRefused(t, staging, "a subject outside the three", "must be one of code, production, graph, got \"staging\"")
	restore()

	checks := servedCheckMembers(t, handler, issue.Key)
	requireCheckMember(t, checks, "k5", "subject", `"production"`)
	requireCheckMember(t, checks, "k5", "program", `"dispatch"`)
	requireCheckMember(t, checks, "k6", "subject", `"graph"`)
	requireCheckMember(t, checks, "k6", "program", "")
	requireCheckMember(t, checks, "k1", "subject", "")
	requireCheckMember(t, checks, "k1", "program", "")
}

// useTheNextReleasesManifest makes the package read and write by the embedded manifest with its
// version raised by one, as the next release's build does: each element this build reads and
// creates only from that version is one it creates. restore puts this build's manifest back, and
// the test's cleanup does too should it end before.
func useTheNextReleasesManifest(t *testing.T) (restore func()) {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	version, ok := manifest["version"].(float64)
	if !ok {
		t.Fatalf("the manifest declares no version")
	}
	manifest["version"] = int(version) + 1
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
	restore, err = pmdoc.UseManifestForTest(encoded)
	if err != nil {
		t.Fatalf("install the manifest of version %d: %v", int(version)+1, err)
	}
	t.Cleanup(restore)
	return restore
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
