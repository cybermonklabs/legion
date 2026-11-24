package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The rows of this file read each version through the four version routes and nothing else. They
// call none of the functions that name a download, so applied onto a tree without them they still
// build. There each row that reads a name or download=true fails on the header it reads, and the
// OpenAPI row on the parameter the committed document does not declare; the row that reads a
// document without download=true (TestADocumentVersionAnswersItsJSONUnlessDownloadIsExactlyTrue)
// passes there, as it must.

// The two cache answers of a file's or an image's version. The version address keeps the bytes a
// year, since they never change. The download address asks again before each use, since the name
// it saves under does change: an older version gains its -v<number> once a newer one arrives.
const (
	versionAddressCache  = "private, max-age=31536000, immutable"
	downloadAddressCache = "private, no-cache"
)

// downloadRoute is one of the four version routes, beside the owner whose /artifacts takes the
// uploads a row reads there.
type downloadRoute struct {
	name  string
	owner string
	// readsIssue is a route that reaches an issue's documents, its spec among them. The project's
	// route reaches only the project's own documents, and a conversation holds no document.
	readsIssue bool
	// version is the address of the artifact's version number on the route.
	version func(artifact model.Artifact, number int) string
}

// downloadRoutes builds a server with a file store and answers it with the four version routes:
// by id, by issue and slug, by project and slug, and by conversation and slug, in that order. Each
// route reads an owner of its own, so a name a row uploads on one route takes its own version
// numbers there.
func downloadRoutes(t *testing.T) (http.Handler, []downloadRoute) {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	readBySlug := fileIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "FILES", "title": "Read by id", "spec": "# Read by id",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue read by id: status=%d body=%s", created.Code, created.Body.String())
	}
	readByID := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	bySlug := func(owner string) func(model.Artifact, int) string {
		return func(artifact model.Artifact, number int) string {
			return fmt.Sprintf("%s/artifacts/%s/versions/%d", owner, artifact.Slug, number)
		}
	}
	return handler, []downloadRoute{
		{name: "by id", owner: "/api/v1/issues/" + readByID, readsIssue: true, version: func(artifact model.Artifact, number int) string {
			return fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifact.ID, number)
		}},
		{name: "by issue and slug", owner: "/api/v1/issues/" + readBySlug, readsIssue: true, version: bySlug("/api/v1/issues/" + readBySlug)},
		{name: "by project and slug", owner: "/api/v1/projects/FILES", version: bySlug("/api/v1/projects/FILES")},
		{name: "by conversation and slug", owner: "/api/v1/agents/" + agentSession, version: bySlug("/api/v1/agents/" + agentSession)},
	}
}

// uploadVersions uploads each of contents, in order, as versions 1, 2, … of a new artifact named
// name under owner, and answers the artifact. The name goes in the form's name field, which is
// what names the artifact. The file part's own filename is written raw into its header
// (apitest.MultipartUpload), so it is a plain stand-in rather than a name that holds a quote or a
// line break. A version after the first of a Markdown upload replaces a document the owner
// already holds, so it names the token of that version (uploadBase on the artifact the upload
// before it answered); a file or an image has no document tree, is asked no token, and answers
// that read NOT_DOCUMENT, so only a Markdown upload after the first reads one.
func uploadVersions(t *testing.T, handler http.Handler, owner, name, contentType string, contents ...[]byte) model.Artifact {
	t.Helper()
	var artifact model.Artifact
	for index, content := range contents {
		fields := map[string]string{"name": name}
		if index > 0 && contentType == "text/markdown" {
			fields["precondition"] = uploadBaseField(t, handler, artifact.ID)
		}
		response := multipartRequest(t, handler, owner+"/artifacts", fields, "upload.bin", contentType, content, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %q to %s: status=%d body=%s", name, owner, response.Code, response.Body.String())
		}
		uploaded := decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
			Version  model.Version  `json:"version"`
		}](t, response)
		if uploaded.Artifact.Name != name || uploaded.Version.Number != index+1 || (index > 0 && uploaded.Artifact.ID != artifact.ID) {
			t.Fatalf("upload %q to %s stored %q as version %d of %s, want version %d of one artifact under that name",
				name, owner, uploaded.Artifact.Name, uploaded.Version.Number, uploaded.Artifact.ID, index+1)
		}
		artifact = uploaded.Artifact
	}
	return artifact
}

// requireHeaders fails for each header of want whose value in got is not, whole, the value want
// gives it; "" is a header the answer must not carry.
func requireHeaders(t *testing.T, request string, got http.Header, want map[string]string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if value := got.Get(name); value != want[name] {
			t.Errorf("%s: %s %q, want %q", request, name, value, want[name])
		}
	}
}

// fileVersionRead is one read of a file's or an image's version: its address, and the bytes, the
// type they were stored as and the Content-Disposition it must answer.
type fileVersionRead struct {
	address     string
	contentType string
	content     []byte
	disposition string
}

// requireFileVersion makes read and holds its answer to the version's bytes under every header of
// a version's answer, with Cache-Control cacheControl.
func requireFileVersion(t *testing.T, handler http.Handler, read fileVersionRead, cacheControl string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, read.address, nil, "alice")
	if response.Code != http.StatusOK {
		t.Errorf("GET %s: status=%d body=%s, want 200", read.address, response.Code, response.Body.String())
		return
	}
	if !bytes.Equal(response.Body.Bytes(), read.content) {
		t.Errorf("GET %s: body %q, want the version's bytes %q", read.address, response.Body.String(), read.content)
	}
	checksum := sha256.Sum256(read.content)
	requireHeaders(t, "GET "+read.address, response.Header(), map[string]string{
		"Cache-Control":          cacheControl,
		"Content-Disposition":    read.disposition,
		"Content-Type":           read.contentType,
		"ETag":                   hex.EncodeToString(checksum[:]),
		"X-Content-Type-Options": "nosniff",
	})
}

// documentVersionKeys are the members of a document version's JSON answer, which every read
// without download=true keeps.
var documentVersionKeys = []string{"authors", "created_at", "markdown", "named", "number", "sha256", "size", "summary"}

// documentVersionJSON reads a document's version at address, holds the answer to the JSON a read
// without download=true answers, with exactly documentVersionKeys and none of a download's
// headers, and answers its markdown.
func documentVersionJSON(t *testing.T, handler http.Handler, address string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, address, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s, want 200", address, response.Code, response.Body.String())
	}
	requireHeaders(t, "GET "+address, response.Header(), map[string]string{
		"Cache-Control":       "",
		"Content-Disposition": "",
		"Content-Type":        "application/json",
	})
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: decode the JSON answer %q: %v", address, response.Body.String(), err)
	}
	if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, documentVersionKeys) {
		t.Errorf("GET %s answers the members %v, want exactly %v", address, keys, documentVersionKeys)
	}
	var markdown string
	if err := json.Unmarshal(body["markdown"], &markdown); err != nil {
		t.Fatalf("GET %s: read the markdown member: %v", address, err)
	}
	return markdown
}

// requireMarkdownDownload reads a document's version at address as JSON, then at its download
// address, and holds the download to the Markdown file of the JSON's markdown, saved under
// disposition. It answers that markdown.
func requireMarkdownDownload(t *testing.T, handler http.Handler, address, disposition string) string {
	t.Helper()
	markdown := documentVersionJSON(t, handler, address)
	download := address + "?download=true"
	response := dispatchRequest(t, handler, http.MethodGet, download, nil, "alice")
	if response.Code != http.StatusOK {
		t.Errorf("GET %s: status=%d body=%s, want 200", download, response.Code, response.Body.String())
		return markdown
	}
	if response.Body.String() != markdown {
		t.Errorf("GET %s: body %q, want the version's markdown %q", download, response.Body.String(), markdown)
	}
	requireHeaders(t, "GET "+download, response.Header(), map[string]string{
		"Cache-Control":          downloadAddressCache,
		"Content-Disposition":    disposition,
		"Content-Type":           "text/markdown; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
	})
	return markdown
}

// documentAtTwoVersions answers the document a route's owner holds at versions 1 and 2. On a
// route that reads an issue it is the issue's spec, which issue creation stores as spec.md, and its
// second version is an edit named as a version, which writes the version at once: an upload of
// spec.md would replace the spec whole. The project's route reaches no issue's document, so there
// it is a project document uploaded twice.
func documentAtTwoVersions(t *testing.T, handler http.Handler, route downloadRoute) model.Artifact {
	t.Helper()
	if !route.readsIssue {
		return uploadVersions(t, handler, route.owner, "roadmap.md", "text/markdown", []byte("# Roadmap\n"), []byte("# Roadmap\n\nRevised.\n"))
	}
	read := dispatchRequest(t, handler, http.MethodGet, route.owner+"/artifacts/spec", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the spec of %s: status=%d body=%s", route.owner, read.Code, read.Body.String())
	}
	spec := decodeBody[model.Artifact](t, read)
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec.ID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "Revised."}},
		"summary": "Revise the spec",
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the spec of %s: status=%d body=%s", route.owner, edited.Code, edited.Body.String())
	}
	if version := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, edited).Version; version == nil || version.Number != 2 {
		t.Fatalf("the named edit of the spec of %s wrote %s, want version 2", route.owner, edited.Body.String())
	}
	return spec
}

// A file's or an image's version read at its version address, with no parameter, saves under its
// artifact's name on each of the four version routes: the latest version under the name itself,
// an older one with -v<number> before its extension. The name goes in twice: as filename*, the
// UTF-8 name a browser reads, and as filename, an ASCII form for a client that reads only that,
// with each character outside printable ASCII, each `"` and each `\` written as `_`. A path
// separator is `_` in both. Every other header of the version address stays as it was.
func TestAVersionDownloadIsNamedAfterItsArtifactOnEveryRoute(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	// %PDF- opens what http.DetectContentType reads as a PDF, which is no picture, so each upload
	// is a file; a PNG's signature makes the one image.
	reports := [][]byte{[]byte("%PDF-1.7\nreport, version 1"), []byte("%PDF-1.7\nreport, version 2"), []byte("%PDF-1.7\nreport, version 3")}
	quarterly := []byte("%PDF-1.7\nquarterly résumé")
	separated := []byte("a name with both path separators")
	image := pngBytes("pixels")
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			report := uploadVersions(t, handler, route.owner, "report.pdf", "application/pdf", reports...)
			shot := uploadVersions(t, handler, route.owner, "shot.png", "image/png", image)
			if report.Kind != "file" || shot.Kind != "image" {
				t.Fatalf("report.pdf is a %s and shot.png a %s, want a file and an image", report.Kind, shot.Kind)
			}
			quoted := uploadVersions(t, handler, route.owner, `Q3 "final" résumé.pdf`, "application/pdf", quarterly)
			paths := uploadVersions(t, handler, route.owner, `a/b\c.txt`, "text/plain", separated)
			for _, read := range []fileVersionRead{
				{route.version(report, 3), "application/pdf", reports[2], `attachment; filename="report.pdf"; filename*=UTF-8''report.pdf`},
				{route.version(report, 2), "application/pdf", reports[1], `attachment; filename="report-v2.pdf"; filename*=UTF-8''report-v2.pdf`},
				{route.version(report, 1), "application/pdf", reports[0], `attachment; filename="report-v1.pdf"; filename*=UTF-8''report-v1.pdf`},
				{route.version(shot, 1), "image/png", image, `attachment; filename="shot.png"; filename*=UTF-8''shot.png`},
				{route.version(quoted, 1), "application/pdf", quarterly, `attachment; filename="Q3 _final_ r_sum_.pdf"; filename*=UTF-8''Q3%20%22final%22%20r%C3%A9sum%C3%A9.pdf`},
				{route.version(paths, 1), "text/plain", separated, `attachment; filename="a_b_c.txt"; filename*=UTF-8''a_b_c.txt`},
			} {
				requireFileVersion(t, handler, read, versionAddressCache)
			}
		})
	}
}

// An older version's number goes before the extension, which starts at the last `.` after the
// name's first character: a name with no `.`, or whose only `.` opens it, takes the number at its
// end, and of two extensions only the last follows the number.
func TestAnOlderVersionDownloadTakesItsNumberBeforeTheExtension(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	byID := routes[0]
	makefiles := [][]byte{[]byte("all:\n\tgo build\n"), []byte("all:\n\tgo build ./...\n")}
	environments := [][]byte{[]byte("MODE=one\n"), []byte("MODE=two\n")}
	archives := [][]byte{[]byte("archive, version 1"), []byte("archive, version 2")}
	makefile := uploadVersions(t, handler, byID.owner, "Makefile", "text/plain", makefiles...)
	environment := uploadVersions(t, handler, byID.owner, ".env", "text/plain", environments...)
	archive := uploadVersions(t, handler, byID.owner, "archive.tar.gz", "application/gzip", archives...)
	for _, read := range []fileVersionRead{
		{byID.version(makefile, 1), "text/plain", makefiles[0], `attachment; filename="Makefile-v1"; filename*=UTF-8''Makefile-v1`},
		{byID.version(environment, 1), "text/plain", environments[0], `attachment; filename=".env-v1"; filename*=UTF-8''.env-v1`},
		{byID.version(archive, 1), "application/gzip", archives[0], `attachment; filename="archive.tar-v1.gz"; filename*=UTF-8''archive.tar-v1.gz`},
	} {
		requireFileVersion(t, handler, read, versionAddressCache)
	}
}

// Each control character (Cc) and each bidirectional control (Bidi_Control) of a name is one `_` in
// both forms of the saved name: a control character has no place in a file name, and a
// bidirectional control can draw a name's extension in a false order, so a program reads as a
// picture. Every other character stays in filename*, the zero-width joiner inside an emoji
// included, and is one `_` per character in the ASCII filename. The upload trims space at the two
// ends of a name, so each control character here sits inside it.
func TestAVersionDownloadNameWritesEveryUnsafeCharacterAsAnUnderscore(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	byID := routes[0]
	content := []byte("a name to make safe")
	// A tab, a line feed, DEL (U+007F) and NEL (U+0085).
	controls := uploadVersions(t, handler, byID.owner, "a\tb\nc\x7fd\u0085e.txt", "text/plain", content)
	// Each of the twelve: U+061C, U+200E, U+200F, U+202A to U+202E and U+2066 to U+2069.
	bidirectional := uploadVersions(t, handler, byID.owner,
		"b\u061cc\u200ed\u200fe\u202af\u202bg\u202ch\u202di\u202ej\u2066k\u2067l\u2068m\u2069n.txt", "text/plain", content)
	// A woman (U+1F469), the zero-width joiner (U+200D) and a laptop (U+1F4BB): one emoji.
	emoji := uploadVersions(t, handler, byID.owner, "\U0001F469\u200d\U0001F4BB.txt", "text/plain", content)
	for _, read := range []fileVersionRead{
		{byID.version(controls, 1), "text/plain", content, `attachment; filename="a_b_c_d_e.txt"; filename*=UTF-8''a_b_c_d_e.txt`},
		{byID.version(bidirectional, 1), "text/plain", content, `attachment; filename="b_c_d_e_f_g_h_i_j_k_l_m_n.txt"; filename*=UTF-8''b_c_d_e_f_g_h_i_j_k_l_m_n.txt`},
		{byID.version(emoji, 1), "text/plain", content, `attachment; filename="___.txt"; filename*=UTF-8''%F0%9F%91%A9%E2%80%8D%F0%9F%92%BB.txt`},
	} {
		requireFileVersion(t, handler, read, versionAddressCache)
	}
}

// The download address, ?download=true, names a file's and an image's version as the version
// address does, on each of the four routes, and has a browser ask again before it uses a copy it
// holds: the name an older version saves under changes when a newer version arrives.
func TestTheDownloadAddressNamesAFileAndAsksAgainOnEveryRoute(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	reports := [][]byte{[]byte("%PDF-1.7\nreport, version 1"), []byte("%PDF-1.7\nreport, version 2")}
	images := [][]byte{pngBytes("first pixels"), pngBytes("second pixels")}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			report := uploadVersions(t, handler, route.owner, "report.pdf", "application/pdf", reports...)
			shot := uploadVersions(t, handler, route.owner, "shot.png", "image/png", images...)
			if report.Kind != "file" || shot.Kind != "image" {
				t.Fatalf("report.pdf is a %s and shot.png a %s, want a file and an image", report.Kind, shot.Kind)
			}
			for _, read := range []fileVersionRead{
				{route.version(report, 2) + "?download=true", "application/pdf", reports[1], `attachment; filename="report.pdf"; filename*=UTF-8''report.pdf`},
				{route.version(report, 1) + "?download=true", "application/pdf", reports[0], `attachment; filename="report-v1.pdf"; filename*=UTF-8''report-v1.pdf`},
				{route.version(shot, 2) + "?download=true", "image/png", images[1], `attachment; filename="shot.png"; filename*=UTF-8''shot.png`},
				{route.version(shot, 1) + "?download=true", "image/png", images[0], `attachment; filename="shot-v1.png"; filename*=UTF-8''shot-v1.png`},
			} {
				requireFileVersion(t, handler, read, downloadAddressCache)
			}
		})
	}
}

// At its download address a document's version is its Markdown file, on each of the three routes
// that reach a document (a conversation holds files and images alone): the text the JSON answer's
// markdown member holds, an older version's own, saved under the name the document holds at the
// moment of the read, with .md added where that name ends in no Markdown extension.
func TestTheDownloadAddressAnswersADocumentAsItsMarkdownFile(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	for _, route := range routes[:3] {
		t.Run(route.name, func(t *testing.T) {
			document := documentAtTwoVersions(t, handler, route)
			latest, older := `attachment; filename="spec.md"; filename*=UTF-8''spec.md`, `attachment; filename="spec-v1.md"; filename*=UTF-8''spec-v1.md`
			if !route.readsIssue {
				latest, older = `attachment; filename="roadmap.md"; filename*=UTF-8''roadmap.md`, `attachment; filename="roadmap-v1.md"; filename*=UTF-8''roadmap-v1.md`
			}
			second := requireMarkdownDownload(t, handler, route.version(document, 2), latest)
			if first := requireMarkdownDownload(t, handler, route.version(document, 1), older); first == second {
				t.Errorf("versions 1 and 2 of %s hold the same markdown %q, want each its own", document.Name, first)
			}

			notes := uploadVersions(t, handler, route.owner, "Launch notes", "text/markdown",
				[]byte("# Launch\n"), []byte("# Launch\n\nDraft two.\n"), []byte("# Launch\n\nDraft three.\n"))
			requireMarkdownDownload(t, handler, route.version(notes, 3), `attachment; filename="Launch notes.md"; filename*=UTF-8''Launch%20notes.md`)
			requireMarkdownDownload(t, handler, route.version(notes, 1), `attachment; filename="Launch notes-v1.md"; filename*=UTF-8''Launch%20notes-v1.md`)

			upper := uploadVersions(t, handler, route.owner, "NOTES.MD", "text/markdown", []byte("# Notes\n"))
			requireMarkdownDownload(t, handler, route.version(upper, 1), `attachment; filename="NOTES.MD"; filename*=UTF-8''NOTES.MD`)
			design := uploadVersions(t, handler, route.owner, "design.markdown", "text/markdown", []byte("# Design\n"))
			requireMarkdownDownload(t, handler, route.version(design, 1), `attachment; filename="design.markdown"; filename*=UTF-8''design.markdown`)

			// A rename writes the name alone and keeps the slug, so the same address saves under the
			// new name.
			draft := uploadVersions(t, handler, route.owner, "Draft", "text/markdown", []byte("# Draft\n"))
			requireMarkdownDownload(t, handler, route.version(draft, 1), `attachment; filename="Draft.md"; filename*=UTF-8''Draft.md`)
			correctedTitle(t, handler, draft.ID, "Final plan")
			requireMarkdownDownload(t, handler, route.version(draft, 1), `attachment; filename="Final plan.md"; filename*=UTF-8''Final%20plan.md`)
		})
	}
}

// A document's version read without download=true answers the JSON it answered before the
// download address existed, whatever else the parameter says: only the exact value true asks for
// the file, so download=false, download=1 and an empty download= each read as no parameter.
func TestADocumentVersionAnswersItsJSONUnlessDownloadIsExactlyTrue(t *testing.T) {
	t.Parallel()
	handler, routes := downloadRoutes(t)
	for _, route := range routes[:3] {
		t.Run(route.name, func(t *testing.T) {
			document := documentAtTwoVersions(t, handler, route)
			for _, number := range []int{1, 2} {
				address := route.version(document, number)
				markdown := documentVersionJSON(t, handler, address)
				for _, query := range []string{"?download=false", "?download=1", "?download="} {
					if got := documentVersionJSON(t, handler, address+query); got != markdown {
						t.Errorf("GET %s%s answers the markdown %q, want %q, the version's own", address, query, got, markdown)
					}
				}
			}
		})
	}
}

// Each of the four version operations of the committed OpenAPI document, which every generated
// client is written from, declares the download query parameter. The query-key guard holds only
// that some route of artifacts.go names the key, which one operation would satisfy.
func TestEveryVersionOperationDeclaresTheDownloadParameterInTheOpenAPIDocument(t *testing.T) {
	t.Parallel()
	type parameter struct {
		Name string `json:"name"`
		In   string `json:"in"`
	}
	var document struct {
		Paths map[string]struct {
			Get *struct {
				Parameters []parameter `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(committedOpenAPI(t), &document); err != nil {
		t.Fatalf("decode the committed document: %v", err)
	}
	for _, pattern := range []string{
		"/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
		"/api/v1/artifacts/{id}/versions/{number}",
		"/api/v1/issues/{key}/artifacts/{slug}/versions/{number}",
		"/api/v1/projects/{key}/artifacts/{slug}/versions/{number}",
	} {
		operation := document.Paths[pattern].Get
		if operation == nil {
			t.Errorf("the document has no GET %s", pattern)
			continue
		}
		if !slices.Contains(operation.Parameters, parameter{Name: "download", In: "query"}) {
			t.Errorf("GET %s declares the parameters %+v, want a query parameter named download", pattern, operation.Parameters)
		}
	}
}
