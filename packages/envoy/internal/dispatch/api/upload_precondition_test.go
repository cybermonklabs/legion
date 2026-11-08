package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// uploadBase is the base an upload replacing a document names: the token the document carries
// now, read the way `dispatch doc-read` reads it (GET /api/v1/artifacts/{id}/text). A JSON upload
// sends the object this returns under `precondition`, a multipart upload its JSON text in the
// form field of that name (uploadBaseField).
func uploadBase(t *testing.T, handler http.Handler, artifactID string) map[string]string {
	t.Helper()
	return map[string]string{"document": readDocumentPrecondition(t, handler, artifactID).Token}
}

// uploadBaseField is uploadBase written as a multipart form field's value.
func uploadBaseField(t *testing.T, handler http.Handler, artifactID string) string {
	t.Helper()
	encoded, err := json.Marshal(uploadBase(t, handler, artifactID))
	if err != nil {
		t.Fatalf("encode the upload's base: %v", err)
	}
	return string(encoded)
}

// uploadBaseHandler is a server whose settlement is held back for the test's duration, asking
// every replacing upload for a base or not as DISPATCH_UPLOAD_BASE_REQUIRED does
// (Deps.UploadBaseRequired).
func uploadBaseHandler(t *testing.T, required bool) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour, uploadBaseRequired: required})
	return handler, database
}

// uploadPreconditionConflict is the 409 an upload naming a stale base is refused with: the
// document token and version number the document stands at, and the mismatch that named them.
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

// A base that matches the live document is taken, and the version is written. The base is checked
// whatever DISPATCH_UPLOAD_BASE_REQUIRED says, so this drives the default server.
func TestUploadReplacingADocumentTakesTheBaseItNames(t *testing.T) {
	handler, database := uploadBaseHandler(t, false)
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

// A multipart upload names its base in the form field `precondition`, and one whose field is not
// the JSON object a base is written as is refused naming the field.
func TestMultipartUploadNamesItsBaseInItsForm(t *testing.T) {
	handler, database := uploadBaseHandler(t, false)
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

// A base the document has moved past is refused, whatever DISPATCH_UPLOAD_BASE_REQUIRED says, and
// the refusal says where the document stands and how to read its token again.
func TestUploadNamingABaseBehindTheDocumentIsRefusedAndWritesNothing(t *testing.T) {
	handler, database := uploadBaseHandler(t, false)
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
	if !strings.Contains(conflict.Error, "dispatch doc-read") || !strings.Contains(conflict.Error, "--precondition-json") {
		t.Fatalf("the refusal = %q, want it to say the current token comes from `dispatch doc-read` and to upload again with it", conflict.Error)
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

// A replacement that names no base at all is what DISPATCH_UPLOAD_BASE_REQUIRED decides: with the
// setting off, which is the default while the fleet's pods still run a `dispatch` that sends no
// base, the upload is taken as it always was; with it on it is refused and writes nothing.
func TestUploadReplacingADocumentWithNoBaseFollowsTheSetting(t *testing.T) {
	for _, test := range []struct {
		name     string
		required bool
	}{
		{"the setting off", false},
		{"the setting on", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database := uploadBaseHandler(t, test.required)
			issue := createArtifactIssue(t, handler)
			artifactID := seedUploadedDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nbefore\n")
			current := readDocumentPrecondition(t, handler, artifactID)
			versionsBefore := uploadedVersionCount(t, database, artifactID)

			answer := postUpload(t, handler, issue.Key, map[string]any{"name": "notes.md", "content": "# Notes\n\nafter\n"})
			if !test.required {
				if answer.Code != http.StatusCreated {
					t.Fatalf("with the setting off, an upload naming no base = %d (want 201), body=%s", answer.Code, answer.Body.String())
				}
				if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore+1 {
					t.Fatalf("with the setting off, the upload left %d versions, want %d", after, versionsBefore+1)
				}
				return
			}
			if answer.Code != http.StatusBadRequest {
				t.Fatalf("with the setting on, an upload naming no base = %d (want 400), body=%s", answer.Code, answer.Body.String())
			}
			code, text := refusalOf(t, answer)
			if code != "PRECONDITION_REQUIRED" {
				t.Fatalf("with the setting on, an upload naming no base answered code %q, want PRECONDITION_REQUIRED: %s", code, answer.Body.String())
			}
			if !strings.Contains(text, "dispatch doc-read") || !strings.Contains(text, "--precondition-json") {
				t.Fatalf("the refusal = %q, want it to say the token comes from `dispatch doc-read` and to upload again with it", text)
			}
			if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
				t.Fatalf("the refused upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
			}
			if read := readDocumentPrecondition(t, handler, artifactID); read.Token != current.Token {
				t.Fatalf("document after the refused upload = %s, want the token it had, %s", read.Token, current.Token)
			}
		})
	}
}

// A first upload has no base to name, so the setting asks it for none even when it is on.
func TestFirstUploadOfANameNoArtifactHoldsNeedsNoBase(t *testing.T) {
	handler, _ := uploadBaseHandler(t, true)
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

// A file is not a document: it is replaced with no base whatever the setting says.
func TestFileUploadNeedsNoBase(t *testing.T) {
	handler, _ := uploadBaseHandler(t, true)
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
// so no caller could name a base for it, and the upload is the only way to repair it: with the
// setting on it is taken as it is.
func TestRepairingUploadNeedsNoBase(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.Name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{uploadBaseRequired: true})
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

// Two uploads race for one new name. Once the first has committed, the second is a replacement of
// the document the first created, which is what the race resolves to: the handler that loses sees
// a document that exists, so with the setting on it must name the base it replaces. They are
// driven in sequence through the one handler, which is that outcome.
func TestSecondUploadOfARacedNewNameMustNameItsBase(t *testing.T) {
	handler, database := uploadBaseHandler(t, true)
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
	if second.Code != http.StatusBadRequest {
		t.Fatalf("the upload that lost the race = %d (want 400), body=%s", second.Code, second.Body.String())
	}
	if code, _ := refusalOf(t, second); code != "PRECONDITION_REQUIRED" {
		t.Fatalf("the upload that lost the race answered code %q, want PRECONDITION_REQUIRED: %s", code, second.Body.String())
	}
	if after := uploadedVersionCount(t, database, artifactID); after != versionsBefore {
		t.Fatalf("the refused upload wrote: %d versions, want the %d standing before it", after, versionsBefore)
	}
}

// An upload replaces the whole document, so a blocks precondition says nothing about what it
// replaces and is refused whatever the setting says.
func TestUploadNamingBlocksAsItsBaseIsRefused(t *testing.T) {
	handler, database := uploadBaseHandler(t, false)
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
// answer, 201 and all, and writes nothing more. That is right even though the base the body names
// is behind the document by then: the base the client sent is the one its own call replaced, and
// the repeat is that same call, not a second one.
func TestRepeatedUploadUnderOneKeyIsAnsweredByItsFirstCall(t *testing.T) {
	handler, database := uploadBaseHandler(t, true)
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
