package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// uploadBase is the token an upload replacing a document names: the one the document carries
// now, read the way `dispatch doc-read` reads it (GET /api/v1/artifacts/{id}/text). A JSON
// upload sends the object this returns under `precondition`, and a multipart upload its JSON
// text in the form field of that name (uploadBaseField).
func uploadBase(t *testing.T, handler http.Handler, artifactID string) map[string]string {
	t.Helper()
	return map[string]string{"document": readDocumentPrecondition(t, handler, artifactID).Token}
}

// encodeUploadBase is a token object written as a multipart form field's value.
func encodeUploadBase(t *testing.T, base map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("encode the token the upload replaces: %v", err)
	}
	return string(encoded)
}

// uploadBaseField is uploadBase written as a multipart form field's value.
func uploadBaseField(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	return encodeUploadBase(t, uploadBase(t, handler, artifactID))
}

// uploadBaseNamed is uploadBase for a document named the way an upload names it - its owner's
// artifacts path and its name or slug - rather than by id, for a test that holds no id. It
// answers nil when the owner holds nothing under that name, which is a first upload and names
// no token.
func uploadBaseNamed(t *testing.T, handler http.Handler, ownerPath, name string) map[string]string {
	t.Helper()
	artifactID := artifactIDNamed(t, handler, ownerPath, name)
	if artifactID == "" {
		return nil
	}
	return uploadBase(t, handler, artifactID)
}

// artifactIDNamed is the id of the artifact the owner holds under name - its name or its slug -
// read from the owner's listing, and the empty string when it holds none. ownerPath is the
// artifacts path an upload to that owner posts to. A project's listing carries its issues'
// documents too, so only an artifact of the owner the path names is read.
func artifactIDNamed(tb testing.TB, handler http.Handler, ownerPath, name string) string {
	tb.Helper()
	listed := dispatchRequest(tb, handler, http.MethodGet, ownerPath, nil, "alice")
	if listed.Code != http.StatusOK {
		tb.Fatalf("list %s: status=%d body=%s", ownerPath, listed.Code, listed.Body.String())
	}
	ofTheProject := strings.Contains(ownerPath, "/projects/")
	for _, artifact := range decodeBody[[]model.Artifact](tb, listed) {
		if ofTheProject == (artifact.IssueKey != nil) {
			continue
		}
		if artifact.Name == name || artifact.Slug == name {
			return artifact.ID
		}
	}
	return ""
}

// uploadBaseHandler is a server whose settlement is held back for the test's duration.
func uploadBaseHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	return handler, database
}

// uploadPreconditionConflict is the 409 an upload naming a token the document has moved past is
// refused with: the document token and version number the document stands at, and the mismatch
// that named them.
type uploadPreconditionConflict struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Current struct {
		Document string `json:"document"`
		Version  *int   `json:"version"`
	} `json:"current"`
	Mismatches []documentPreconditionMismatch `json:"mismatches"`
}

// uploadedVersionCount is how many versions the artifact holds, which a refused upload must leave
// where it was.
func uploadedVersionCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	_, versions, _ := documentMutationCounts(t, database, artifactID)
	return versions
}

// postUpload is one upload to the issue, with the body given.
func postUpload(t *testing.T, handler http.Handler, issueKey string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", body, "alice")
}

// seedUploadedDocument uploads a document under a name no artifact holds, and answers its id.
func seedUploadedDocument(t *testing.T, handler http.Handler, issueKey, name, markdown string) string {
	t.Helper()
	created := postUpload(t, handler, issueKey, map[string]any{"name": name, "content": markdown})
	if created.Code != http.StatusCreated {
		t.Fatalf("seed %s: status=%d want 201, body=%s", name, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact.ID
}

// A token that matches the live document is taken, and the version is written.
func TestUploadReplacingADocumentTakesTheBaseItNames(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")

	base := uploadBase(t, handler, artifactID)
	before := uploadedVersionCount(t, database, artifactID)
	replaced := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nafter\n", "precondition": base,
	})
	if replaced.Code != http.StatusCreated {
		t.Fatalf("upload naming the live base = %d (want 201) sending %q, body=%s", replaced.Code, base["document"], replaced.Body.String())
	}
	version := decodeBody[struct {
		Version model.Version `json:"version"`
	}](t, replaced).Version
	if version.Number != 2 {
		t.Fatalf("upload naming the live base wrote version %d, want 2", version.Number)
	}
	if after := uploadedVersionCount(t, database, artifactID); after != before+1 {
		t.Fatalf("upload naming the live base left %d versions, want %d", after, before+1)
	}
	if read := readDocumentPrecondition(t, handler, artifactID); read.Markdown != "# Notes\n\nafter\n" {
		t.Fatalf("document after the upload = %q, want the uploaded text", read.Markdown)
	}
}

// A multipart upload names the token of the version it replaces in the form field
// `precondition`, and one whose field is not the JSON object that token is written as is refused
// naming the field.
func TestMultipartUploadNamesItsBaseInItsForm(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	target := "/api/v1/issues/" + issue.Key + "/artifacts"
	versionsBefore := uploadedVersionCount(t, database, artifactID)

	malformed := multipartRequest(t, handler, target, map[string]string{"name": "notes.md", "precondition": "not json"},
		"notes.md", "text/markdown", []byte("# Notes\n\nafter\n"), "alice")
	if code, _ := refusalOf(t, malformed); malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), "precondition") {
		t.Fatalf("multipart upload whose base is not JSON = %d %q, want 400 naming the precondition field: %s", malformed.Code, code, malformed.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
		t.Fatalf("the refused multipart upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
	}

	replaced := multipartRequest(t, handler, target,
		map[string]string{"name": "notes.md", "precondition": uploadBaseField(t, handler, artifactID)},
		"notes.md", "text/markdown", []byte("# Notes\n\nafter\n"), "alice")
	if replaced.Code != http.StatusCreated {
		t.Fatalf("multipart upload naming the live base = %d (want 201), body=%s", replaced.Code, replaced.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
		t.Fatalf("multipart upload naming the live base left %d versions, want %d", after, versionsBefore+1)
	}
}

// A member neither shape declares is ignored by both, as every Dispatch route reads a body since
// #2461, so one build of a fleet takes an upload another build's member rides in. What a
// misspelled key cannot do is pass as the key it misspells: a precondition left naming nothing is
// refused by its own shape, in a form field as in a body, and that upload writes nothing.
func TestAMemberNeitherUploadShapeDeclaresIsIgnoredAndAMisspelledTokenIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	target := "/api/v1/issues/" + issue.Key + "/artifacts"

	// The form's own base, with a member no field of the object names beside the token it does
	// name: the upload is taken, and the version it writes is the one the token allowed.
	base := uploadBase(t, handler, artifactID)
	versionsBefore := uploadedVersionCount(t, database, artifactID)
	strayBase := fmt.Sprintf(`{"document":%q,"blocks_":[]}`, base["document"])
	taken := multipartRequest(t, handler, target, map[string]string{"name": "notes.md", "precondition": strayBase},
		"notes.md", "text/markdown", []byte("# Notes\n\nafter\n"), "alice")
	if taken.Code != http.StatusCreated {
		t.Fatalf("multipart base holding a member it does not name = %d, want 201: %s", taken.Code, taken.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
		t.Fatalf("the taken multipart upload wrote: %d versions, want %d", after, versionsBefore+1)
	}

	// The same object in the JSON body is taken too, which is the parity this holds.
	body := uploadBase(t, handler, artifactID)
	versionsBefore = uploadedVersionCount(t, database, artifactID)
	stored := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nagain\n",
		"precondition": map[string]any{"document": body["document"], "blocks_": []any{}},
	})
	if stored.Code != http.StatusCreated {
		t.Fatalf("a JSON body whose base holds a member it does not name = %d, want 201: %s", stored.Code, stored.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
		t.Fatalf("the taken JSON upload wrote: %d versions, want %d", after, versionsBefore+1)
	}

	// A token whose key is misspelled names nothing, so neither shape reads it as a token: each
	// upload is refused by the precondition's own shape and writes nothing.
	misspelled := uploadBase(t, handler, artifactID)
	versionsBefore = uploadedVersionCount(t, database, artifactID)
	strayKey := fmt.Sprintf(`{"document_":%q}`, misspelled["document"])
	refusedForm := multipartRequest(t, handler, target, map[string]string{"name": "notes.md", "precondition": strayKey},
		"notes.md", "text/markdown", []byte("# Notes\n\nnever\n"), "alice")
	if code, text := refusalOf(t, refusedForm); refusedForm.Code != http.StatusBadRequest || code != "INVALID_PRECONDITION" {
		t.Fatalf("multipart base naming no token = %d %q %q, want 400 INVALID_PRECONDITION: %s",
			refusedForm.Code, code, text, refusedForm.Body.String())
	}
	refusedBody := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nnever\n",
		"precondition": map[string]any{"document_": misspelled["document"]},
	})
	if code, text := refusalOf(t, refusedBody); refusedBody.Code != http.StatusBadRequest || code != "INVALID_PRECONDITION" {
		t.Fatalf("a JSON base naming no token = %d %q %q, want 400 INVALID_PRECONDITION: %s",
			refusedBody.Code, code, text, refusedBody.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
		t.Fatalf("a refused upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
	}
}

// A token the document has moved past is refused, and the refusal says where the document stands
// and how to read its token again. It names the field the token goes in and no flag of a command,
// since the caller may have sent its token straight to the route from a `dispatch` whose
// `artifact` has no flag for one (INFRA-4980). This is the control of the no-token replacement
// below: a token that is named is still checked, inside the write's lock, and refused 409.
func TestUploadNamingABaseBehindTheDocumentIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")

	stale := uploadBase(t, handler, artifactID)
	if first := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nfirst\n", "precondition": stale,
	}); first.Code != http.StatusCreated {
		t.Fatalf("the upload that moves the document on = %d (want 201), body=%s", first.Code, first.Body.String())
	}
	current := readDocumentPrecondition(t, handler, artifactID)
	versionsBefore := uploadedVersionCount(t, database, artifactID)

	refused := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nsecond\n", "precondition": stale,
	})
	if refused.Code != http.StatusConflict {
		t.Fatalf("upload naming a base behind the document = %d (want 409) sending %q, body=%s", refused.Code, stale["document"], refused.Body.String())
	}
	conflict := decodeBody[uploadPreconditionConflict](t, refused)
	if conflict.Code != "PRECONDITION_FAILED" {
		t.Fatalf("upload naming a stale base answered code %q, want PRECONDITION_FAILED: %s", conflict.Code, refused.Body.String())
	}
	if conflict.Current.Document != current.Token {
		t.Fatalf("the refusal's current.document = %q, want the token the document has now, %q (it was sent %q)",
			conflict.Current.Document, current.Token, stale["document"])
	}
	if conflict.Current.Version == nil || *conflict.Current.Version != versionsBefore {
		t.Fatalf("the refusal's current.version = %v, want the document's version %d: %s", conflict.Current.Version, versionsBefore, refused.Body.String())
	}
	if !strings.Contains(conflict.Error, "dispatch doc-read") || !strings.Contains(conflict.Error, "precondition") {
		t.Fatalf("the refusal = %q, want it to say the current token comes from `dispatch doc-read` and to upload again naming it as the precondition", conflict.Error)
	}
	if strings.Contains(conflict.Error, "--precondition-json") {
		t.Fatalf("the refusal = %q, want it to name no flag of a command", conflict.Error)
	}
	if len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "document" ||
		conflict.Mismatches[0].Expected != stale["document"] ||
		conflict.Mismatches[0].Current == nil || *conflict.Mismatches[0].Current != current.Token {
		t.Fatalf("the refusal's mismatches = %#v, want one document mismatch of %q against %q", conflict.Mismatches, stale["document"], current.Token)
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
		t.Fatalf("the refused upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
	}
	if read := readDocumentPrecondition(t, handler, artifactID); read.Markdown != current.Markdown || read.Token != current.Token {
		t.Fatalf("document after the refused upload = %q (%s), want it as it was, %q (%s)", read.Markdown, read.Token, current.Markdown, current.Token)
	}
}

// unguardedReplacementSample is the sample line of the no-token replacement counter in a /metrics
// body, "" when the body holds none: the counter carries no label, so it has one series, which
// appears with the first replacement it counts.
func unguardedReplacementSample(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	for line := range strings.Lines(scrape(t, registry)) {
		if strings.HasPrefix(line, unguardedReplacementsMetric+" ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// uploadFromClient posts one upload to the issue as alice: the inline body `dispatch artifact
// --content-file` sends when form is nil, else the multipart form `--path` sends, with form as
// its fields and content as its file.
func uploadFromClient(t *testing.T, handler http.Handler, issueKey string, body map[string]any, form map[string]string, content string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if form == nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the upload: %v", err)
		}
		request = httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
	} else {
		encoded, bodyType, err := apitest.MultipartUpload(form, form["name"], "text/markdown", []byte(content))
		if err != nil {
			t.Fatalf("encode the upload: %v", err)
		}
		request = httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", encoded)
		request.Header.Set("Content-Type", bodyType)
	}
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// INFRA-4980: a replacement that names no token is taken, as every replacement was before #3000,
// because no deployed `dispatch` client can name a token on `dispatch artifact`; #3000 refused it
// 400 PRECONDITION_REQUIRED and wrote nothing, which refused every agent's replacement of a
// document. Both shapes a client uploads in are driven, the inline body and the multipart form.
// Each one stored is counted on /metrics and logged with the document it replaced, so the refusal
// can come back once no client sends one without a token. A replacement
// naming the live token is checked and taken and is not counted, and a first upload replaces
// nothing and is not counted either.
func TestUploadReplacingADocumentWithNoTokenIsTakenCountedAndLogged(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	registry := metrics.New()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour, metrics: registry})
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	if sample := unguardedReplacementSample(t, registry); sample != "" {
		t.Fatalf("after a first upload the counter reads %q, want no replacement counted", sample)
	}

	for _, upload := range []struct {
		shape, markdown string
		form            map[string]string
	}{
		{shape: "inline", markdown: "# Notes\n\ninline\n"},
		{shape: "multipart", markdown: "# Notes\n\nform\n", form: map[string]string{"name": "notes.md"}},
	} {
		versionsBefore := uploadedVersionCount(t, database, artifactID)
		answer := uploadFromClient(t, handler, issue.Key,
			map[string]any{"name": "notes.md", "content": upload.markdown}, upload.form, upload.markdown)
		if answer.Code != http.StatusCreated {
			t.Fatalf("the %s replacement naming no token = %d (want 201), body=%s", upload.shape, answer.Code, answer.Body.String())
		}
		if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
			t.Fatalf("the %s replacement naming no token left %d versions, want %d", upload.shape, after, versionsBefore+1)
		}
		if read := readDocumentPrecondition(t, handler, artifactID); read.Markdown != upload.markdown {
			t.Fatalf("the document after the %s replacement naming no token = %q, want %q", upload.shape, read.Markdown, upload.markdown)
		}
	}

	guarded := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nguarded\n", "precondition": uploadBase(t, handler, artifactID),
	})
	if guarded.Code != http.StatusCreated {
		t.Fatalf("the replacement naming the live token = %d (want 201), body=%s", guarded.Code, guarded.Body.String())
	}

	if sample, want := unguardedReplacementSample(t, registry), unguardedReplacementsMetric+" 2"; sample != want {
		t.Fatalf("the counter reads %q, want %q: the two replacements naming no token, and not the one naming a token", sample, want)
	}
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, unguardedReplacementMessage) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d lines logged the replacements naming no token, want 2: %s", len(lines), logs.String())
	}
	for index, line := range lines {
		for _, want := range []string{"artifact=" + artifactID, "document=" + issue.Key + "/notes-md"} {
			if !strings.Contains(line, want) {
				t.Fatalf("the log line of replacement %d = %q, want it to hold %q", index+1, line, want)
			}
		}
		for _, field := range []string{"caller=", "caller_kind=", "user_agent="} {
			if strings.Contains(line, field) {
				t.Fatalf("the log line of replacement %d = %q, want no %s in it: the access log's line names the caller", index+1, line, field)
			}
		}
	}
}

// A first upload replaces no version, so it names no token.
func TestFirstUploadOfANameNoArtifactHoldsNeedsNoToken(t *testing.T) {
	t.Parallel()
	handler, _ := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)

	created := postUpload(t, handler, issue.Key, map[string]any{"name": "fresh.md", "content": "# Fresh\n"})
	if created.Code != http.StatusCreated {
		t.Fatalf("first upload of a name no artifact holds = %d (want 201), body=%s", created.Code, created.Body.String())
	}
	upload := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
		Version  model.Version  `json:"version"`
	}](t, created)
	if upload.Version.Number != 1 {
		t.Fatalf("first upload wrote version %d, want 1", upload.Version.Number)
	}
	if read := readDocumentPrecondition(t, handler, upload.Artifact.ID); read.Markdown != "# Fresh\n" {
		t.Fatalf("the created document = %q, want the uploaded text", read.Markdown)
	}
}

// A first upload replaces nothing, so a token names a version the owner does not hold: the
// caller's belief about what it is replacing is false, which is a failed precondition, and
// nothing is written.
func TestFirstUploadNamingABaseIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	// The issue's create writes its spec and, for an issue born converted, the documents of its
	// room beside it: the refused upload leaves exactly those.
	created := issueArtifactIDs(t, database, issue.Key)
	if !slices.Contains(created, issue.PrimaryArtifactID) {
		t.Fatalf("the issue was created holding the artifacts %v, want its spec %s among them", created, issue.PrimaryArtifactID)
	}

	refused := postUpload(t, handler, issue.Key, map[string]any{
		"name": "fresh.md", "content": "# Fresh\n",
		"precondition": map[string]string{"document": "sha256:" + strings.Repeat("a", 64)},
	})
	if refused.Code != http.StatusConflict {
		t.Fatalf("a first upload naming a base = %d (want 409), body=%s", refused.Code, refused.Body.String())
	}
	conflict := decodeBody[uploadPreconditionConflict](t, refused)
	if conflict.Code != "PRECONDITION_FAILED" {
		t.Fatalf("the refusal's code = %q, want PRECONDITION_FAILED: %s", conflict.Code, refused.Body.String())
	}
	// The refusal of a token the document has moved past sends the caller to dispatch doc-read
	// for a fresh one. This one must not: the owner holds no document under that name, so there
	// is no token to read, and the caller's upload creates the document rather than replacing
	// one.
	if !strings.Contains(conflict.Error, "has no token for an upload to name: send it again naming no token") {
		t.Fatalf("the refusal reads %q, want it to say the name has no token for an upload to name and to ask for one naming none", conflict.Error)
	}
	if strings.Contains(conflict.Error, "dispatch doc-read") {
		t.Fatalf("the refusal reads %q, want it not to send the caller after a token no document has", conflict.Error)
	}
	// The refusal is its own and not a stale token's, borrowed: it asserts no mismatch with a
	// live token it does not hold, and sends no empty `current` where a reader expects a token
	// its next upload can name.
	if strings.Contains(refused.Body.String(), `"current"`) || strings.Contains(refused.Body.String(), `"mismatches"`) {
		t.Fatalf("the refusal body = %s, want neither a current token nor a mismatch: this owner holds no document under the name", refused.Body.String())
	}
	if strings.Contains(conflict.Error, "document edit precondition failed") {
		t.Fatalf("the refusal reads %q, want a sentence of its own: no document of this owner changed", conflict.Error)
	}
	if after := issueArtifactIDs(t, database, issue.Key); !slices.Equal(after, created) {
		t.Fatalf("the issue holds the artifacts %v after the refused first upload, want only the %v it was created with", after, created)
	}
	var named int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from artifacts where issue_key = $1 and name = 'fresh.md'`, issue.Key).Scan(&named); err != nil {
		t.Fatalf("count the issue's artifacts named fresh.md: %v", err)
	}
	if named != 0 {
		t.Fatalf("the issue holds %d artifacts named fresh.md after the refused first upload, want none", named)
	}
}

// issueArtifactIDs is the id of each artifact the issue issueKey holds, in id order.
func issueArtifactIDs(t *testing.T, database *store.Store, issueKey string) []string {
	t.Helper()
	var ids []string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(array_agg(id::text order by id), '{}') from artifacts where issue_key = $1`, issueKey).Scan(&ids); err != nil {
		t.Fatalf("read the artifacts of %s: %v", issueKey, err)
	}
	return ids
}

// A file replaced naming no token is taken unchecked, as a document replaced naming none is.
func TestFileUploadNeedsNoBase(t *testing.T) {
	t.Parallel()
	handler, _ := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	path := "/api/v1/issues/" + issue.Key + "/artifacts"
	fields := map[string]string{"name": "diagram.png"}

	created := multipartRequest(t, handler, path, fields, "diagram.png", "image/png", []byte("\x89PNG\r\n\x1a\nfirst"), "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("first upload of the file = %d (want 201), body=%s", created.Code, created.Body.String())
	}
	replaced := multipartRequest(t, handler, path, fields, "diagram.png", "image/png", []byte("\x89PNG\r\n\x1a\nsecond"), "alice")
	if replaced.Code != http.StatusCreated {
		t.Fatalf("a file replaced naming no base = %d (want 201), body=%s", replaced.Code, replaced.Body.String())
	}
	if version := decodeBody[struct {
		Version model.Version `json:"version"`
	}](t, replaced).Version; version.Number != 2 {
		t.Fatalf("the replacing file upload wrote version %d, want 2", version.Number)
	}
}

// A document whose stored tree is outside the Proof schema renders no markdown and has no token,
// so no caller could name one for it, and the upload is the only way to repair it: it is taken
// as it is.
func TestRepairingUploadNeedsNoToken(t *testing.T) {
	t.Parallel()
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.Name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "repair.md", test.Corrupt)

			repaired := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "repair.md"},
				"repair.md", "text/markdown", []byte("repaired\n"), "alice")
			if repaired.Code != http.StatusCreated {
				t.Fatalf("a repairing upload naming no base = %d (want 201), body=%s", repaired.Code, repaired.Body.String())
			}
			text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
			if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"repaired\n"`) {
				t.Fatalf("read the repaired document: status=%d body=%s", text.Code, text.Body.String())
			}
		})
	}
}

// A document outside the Proof schema has no token for anybody to name, so an upload naming one
// on it has named a token the document has never had: it is refused with a sentence of its own,
// nothing is written, and the repair that names none is taken after it.
func TestRepairingUploadNamingABaseIsRefused(t *testing.T) {
	t.Parallel()
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.Name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "repair.md", test.Corrupt)
			versionsBefore := uploadedVersionCount(t, database, artifact.ID)

			refused := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts",
				map[string]string{"name": "repair.md", "precondition": `{"document":"sha256:` + strings.Repeat("a", 64) + `"}`},
				"repair.md", "text/markdown", []byte("repaired\n"), "alice")
			if refused.Code != http.StatusConflict {
				t.Fatalf("a repairing upload naming a base = %d (want 409), body=%s", refused.Code, refused.Body.String())
			}
			conflict := decodeBody[uploadPreconditionConflict](t, refused)
			if conflict.Code != "PRECONDITION_FAILED" {
				t.Fatalf("the refusal's code = %q, want PRECONDITION_FAILED: %s", conflict.Code, refused.Body.String())
			}
			// The document holds no token, so the refusal says so and asks for an upload
			// naming none, rather than sending the caller to `dispatch doc-read` for a token
			// nothing can print.
			if !strings.Contains(conflict.Error, "send it again naming no token") {
				t.Fatalf("the refusal reads %q, want it to say the document has no token and the repair names none", conflict.Error)
			}
			if strings.Contains(conflict.Error, "dispatch doc-read") {
				t.Fatalf("the refusal reads %q, want it not to send the caller after a token no document has", conflict.Error)
			}
			if after := uploadedVersionCount(t, database, artifact.ID); after != versionsBefore {
				t.Fatalf("the refused repair wrote: %d versions, want the %d standing before it", after, versionsBefore)
			}
			repaired := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "repair.md"},
				"repair.md", "text/markdown", []byte("repaired\n"), "alice")
			if repaired.Code != http.StatusCreated {
				t.Fatalf("the repair naming no base after the refusal = %d (want 201), body=%s", repaired.Code, repaired.Body.String())
			}
		})
	}
}

// Two uploads race for one new name. Once the first has committed, the second is a replacement of
// the document the first created, which is what the race resolves to. It names no token, so it is
// taken unchecked and counted, as every replacement naming none is until every deployed client
// sends one (INFRA-4980). They are driven in sequence through the one handler, which is that
// outcome.
func TestSecondUploadOfARacedNewNameIsAReplacement(t *testing.T) {
	t.Parallel()
	registry := metrics.New()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour, metrics: registry})
	issue := createArtifactIssue(t, handler)

	first := postUpload(t, handler, issue.Key, map[string]any{"name": "raced.md", "content": "# Raced\n\nfirst\n"})
	if first.Code != http.StatusCreated {
		t.Fatalf("the upload that won the race = %d (want 201), body=%s", first.Code, first.Body.String())
	}
	artifactID := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, first).Artifact.ID
	versionsBefore := uploadedVersionCount(t, database, artifactID)

	second := postUpload(t, handler, issue.Key, map[string]any{"name": "raced.md", "content": "# Raced\n\nsecond\n"})
	if second.Code != http.StatusCreated {
		t.Fatalf("the upload that lost the race = %d (want 201), body=%s", second.Code, second.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
		t.Fatalf("the upload that lost the race left %d versions, want %d", after, versionsBefore+1)
	}
	if sample, want := unguardedReplacementSample(t, registry), unguardedReplacementsMetric+" 1"; sample != want {
		t.Fatalf("the counter reads %q, want %q: the second upload is a replacement naming no token", sample, want)
	}
}

// An upload replaces the whole document, so a blocks precondition says nothing about what it
// replaces and is refused.
func TestUploadNamingBlocksAsItsBaseIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
	blocks := readDocumentPreconditionBlocks(t, handler, artifactID)
	versionsBefore := uploadedVersionCount(t, database, artifactID)

	refused := postUpload(t, handler, issue.Key, map[string]any{
		"name": "notes.md", "content": "# Notes\n\nafter\n",
		"precondition": map[string]any{"blocks": []map[string]string{{"id": blocks[0].ID, "token": blocks[0].Token}}},
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("upload naming a blocks base = %d (want 400) sending block %q, body=%s", refused.Code, blocks[0].ID, refused.Body.String())
	}
	code, text := refusalOf(t, refused)
	if code != "INVALID_PRECONDITION" {
		t.Fatalf("upload naming a blocks base answered code %q, want INVALID_PRECONDITION: %s", code, refused.Body.String())
	}
	if !strings.Contains(text, "document token and nothing else") {
		t.Fatalf("the refusal = %q, want it to say an upload's base is the document token alone", text)
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
		t.Fatalf("the refused upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
	}
}

// A repeat of an upload under the key its first call used is answered by that call's own stored
// answer, 201 and all, and writes nothing more. That is right even though the token the body
// names is behind the document by then: the version the client named is the one its own call
// replaced, and the repeat is that same call, not a second one.
func TestRepeatedUploadUnderOneKeyIsAnsweredByItsFirstCall(t *testing.T) {
	t.Parallel()
	handler, database := uploadBaseHandler(t)
	issue := createArtifactIssue(t, handler)
	artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")

	body := map[string]any{
		"name": "notes.md", "content": "# Notes\n\nafter\n", "precondition": uploadBase(t, handler, artifactID),
	}
	target := "/api/v1/issues/" + issue.Key + "/artifacts"
	first := keyedRequest(t, handler, http.MethodPost, target, body, "alice", "upload-notes-2")
	if first.Code != http.StatusCreated {
		t.Fatalf("the keyed upload = %d (want 201), body=%s", first.Code, first.Body.String())
	}
	versionsAfterFirst := uploadedVersionCount(t, database, artifactID)

	repeat := keyedRequest(t, handler, http.MethodPost, target, body, "alice", "upload-notes-2")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat under the same key = %d (replayed %t), want the first call's stored 201, body=%s",
			repeat.Code, replayed(repeat), repeat.Body.String())
	}
	if repeat.Body.String() != first.Body.String() {
		t.Fatalf("the repeat answered %s, want the first call's own answer %s", repeat.Body.String(), first.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsAfterFirst {
		t.Fatalf("the repeat wrote: %d versions, want the %d its first call left", after, versionsAfterFirst)
	}
}

// projectUploadBaseHandler is uploadBaseHandler with a project to upload documents into.
func projectUploadBaseHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database := uploadBaseHandler(t)
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create the project the documents belong to: status=%d body=%s", created.Code, created.Body.String())
	}
	return handler, database
}

// A project document is replaced through a route of its own, and the token holds there as it
// holds on the issue route, in the JSON body and in the multipart form alike: one that matches
// the live document is taken, and one the document has moved past is refused 409
// PRECONDITION_FAILED naming the token and the version number the document stands at, with
// nothing written.
func TestUploadReplacingAProjectDocumentHoldsTheBaseItNames(t *testing.T) {
	t.Parallel()
	const target = "/api/v1/projects/TEST/artifacts"
	for _, test := range []struct {
		name   string
		upload func(t *testing.T, handler http.Handler, base map[string]string, content string) *httptest.ResponseRecorder
	}{
		{
			name: "the JSON body",
			upload: func(t *testing.T, handler http.Handler, base map[string]string, content string) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, target, map[string]any{
					"name": "notes.md", "content": content, "precondition": base,
				}, "alice")
			},
		},
		{
			name: "the multipart form",
			upload: func(t *testing.T, handler http.Handler, base map[string]string, content string) *httptest.ResponseRecorder {
				t.Helper()
				encoded, err := json.Marshal(base)
				if err != nil {
					t.Fatalf("encode the upload's base: %v", err)
				}
				return multipartRequest(t, handler, target,
					map[string]string{"name": "notes.md", "precondition": string(encoded)},
					"notes.md", "text/markdown", []byte(content), "alice")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database := projectUploadBaseHandler(t)
			artifactID := createProjectDocument(t, handler, "TEST", "notes.md", "# Notes\n\nbefore\n").ID

			base := uploadBase(t, handler, artifactID)
			replaced := test.upload(t, handler, base, "# Notes\n\nfirst\n")
			if replaced.Code != http.StatusCreated {
				t.Fatalf("project upload naming the live base = %d (want 201) sending %q, body=%s", replaced.Code, base["document"], replaced.Body.String())
			}
			current := readDocumentPrecondition(t, handler, artifactID)
			if current.Markdown != "# Notes\n\nfirst\n" {
				t.Fatalf("the project document after the upload reads %q, want the text it named its base for", current.Markdown)
			}
			versionsBefore := uploadedVersionCount(t, database, artifactID)

			refused := test.upload(t, handler, base, "# Notes\n\nsecond\n")
			if refused.Code != http.StatusConflict {
				t.Fatalf("project upload naming a base behind the document = %d (want 409) sending %q, body=%s", refused.Code, base["document"], refused.Body.String())
			}
			conflict := decodeBody[uploadPreconditionConflict](t, refused)
			if conflict.Code != "PRECONDITION_FAILED" {
				t.Fatalf("the project upload's refusal answered code %q, want PRECONDITION_FAILED: %s", conflict.Code, refused.Body.String())
			}
			if conflict.Current.Document != current.Token {
				t.Fatalf("the refusal's current.document = %q, want the token the project document has now, %q (it was sent %q)",
					conflict.Current.Document, current.Token, base["document"])
			}
			if conflict.Current.Version == nil || *conflict.Current.Version != versionsBefore {
				t.Fatalf("the refusal's current.version = %v, want the project document's version %d: %s", conflict.Current.Version, versionsBefore, refused.Body.String())
			}
			if len(conflict.Mismatches) != 1 || conflict.Mismatches[0].Scope != "document" ||
				conflict.Mismatches[0].Expected != base["document"] ||
				conflict.Mismatches[0].Current == nil || *conflict.Mismatches[0].Current != current.Token {
				t.Fatalf("the refusal's mismatches = %#v, want one document mismatch of %q against %q", conflict.Mismatches, base["document"], current.Token)
			}
			if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
				t.Fatalf("the refused project upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
			}
			if read := readDocumentPrecondition(t, handler, artifactID); read.Markdown != current.Markdown || read.Token != current.Token {
				t.Fatalf("the project document after the refused upload = %q (%s), want it as it was, %q (%s)", read.Markdown, read.Token, current.Markdown, current.Token)
			}
		})
	}
}

// Two uploads of one project document read the same token, and both are in the server at once:
// the first is held inside its transactional append, after its own check and before it
// commits, while the second reaches the handler and queues on the project's document lock the
// first holds. Releasing the first commits it, and the second then runs against the document the
// first wrote and is refused 409, though the token it named was live when it was sent and when it
// entered the handler. That is what places the check inside the write's own lock: a check made
// before the lock would have read the document as it stood while the first upload was still
// uncommitted, found the token live, and taken the second upload, losing the first's text.
func TestConcurrentUploadsOfOneBaseLandTheFirstAndRefuseTheSecond(t *testing.T) {
	t.Parallel()
	f := newUploadRaceFixture(t)
	base := uploadBase(t, f.handler, f.document.ID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	f.persistence.holdNextAppend()
	firsts := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firsts <- f.request(ctx, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]any{
			"name": f.document.Name, "content": "before\n\nfirst", "precondition": base,
		})
	}()
	f.persistence.waitHeld(t)
	seconds := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		seconds <- f.request(ctx, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]any{
			"name": f.document.Name, "content": "before\n\nsecond", "precondition": base,
		})
	}()
	// The second upload holds no document operation of its own yet: it waits on the lock the
	// first took, which is what this reads.
	f.waitForWriteToEngage(t)
	f.persistence.release()

	first := awaitWithin(t, firsts, cancel, "the first upload of the base")
	second := awaitWithin(t, seconds, cancel, "the second upload of the base")
	if first.Code != http.StatusCreated {
		t.Fatalf("the first upload of the base = %d (want 201), body=%s", first.Code, first.Body.String())
	}
	if second.Code != http.StatusConflict {
		t.Fatalf("the second upload of the same base = %d (want 409) sending %q, body=%s", second.Code, base["document"], second.Body.String())
	}
	conflict := decodeBody[uploadPreconditionConflict](t, second)
	current := readDocumentPrecondition(t, f.handler, f.document.ID)
	if conflict.Code != "PRECONDITION_FAILED" || conflict.Current.Document != current.Token {
		t.Fatalf("the second upload's refusal = %q on %q, want PRECONDITION_FAILED on the token the first upload left, %q: %s",
			conflict.Code, conflict.Current.Document, current.Token, second.Body.String())
	}
	if current.Markdown != "before\n\nfirst\n" {
		t.Fatalf("the document after the race reads %q, want the first upload's text, which the second must not have replaced", current.Markdown)
	}
	if versions := uploadedVersionCount(t, f.database, f.document.ID); versions != 2 {
		t.Fatalf("the document holds %d versions after the race, want the 2 the seed and the first upload wrote", versions)
	}
}
