package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What the page a reader opens a document at carries, and what it does not (dispatch://INFRA-341
// R11, acceptance A11). A reader with a session that may read the document is sent its opening
// blocks in the page itself, before the page's own scripts; a reader with no session, or one
// whose session has been revoked, or one asking for a document that is not theirs to read, is
// sent the build's own shell with no word of the document in it. A page carrying a document is
// nobody's to keep: no store, no validator, and no 304 for a browser that asks with one.

// firstScreenPage is the dashboard page these rows serve: the shell's own shape - the build's
// inline bootstrap and its entry chunk in the head, as Vite writes them, and the marker the
// server replaces inside the element that holds a document's opening blocks, after `#root`
// because `createRoot` clears what it mounts into.
const firstScreenPage = `<!doctype html>
<html lang="en">
  <head>
    <title>Dispatch</title>
    <script>window.__dispatchEarlyDocumentRead = undefined;</script>
    <script type="module" crossorigin src="/assets/index-abc.js"></script>
    <link rel="stylesheet" crossorigin href="/assets/index-abc.css">
  </head>
  <body>
    <div id="root"></div>
    <div id="first-screen" class="dispatch-markdown prose"><!--first-screen--></div>
  </body>
</html>
`

// firstScreenRig is a router on a real database and a cookie identity, serving a dashboard build
// of one page, so a row can revoke the reader's session and ask again.
type firstScreenRig struct {
	cookie    *http.Cookie
	database  *store.Store
	documents *docs.Service
	handler   http.Handler
	sessions  *memorySessionStore
}

// unreachableFirstScreenBudget is what every row below but the fallback's gives the page to draw
// a document in. Its rows read what the page CARRIES, and production's 300 ms made that verdict a
// race against whatever else the machine was doing: four Go packages at once on two processors is
// enough to lose it, and the server then sends the shell, which is right of the server and read
// by the row as a failure. Two minutes is longer than any of these documents takes anywhere and
// shorter than the go test timeout, so a row that would have hung still fails rather than
// hanging (dispatch://INFRA-341).
const unreachableFirstScreenBudget = 2 * time.Minute

// beatenFirstScreenBudget is the fallback row's, and nothing can be drawn inside it: the
// clause it proves is then the test's own doing rather than the machine's.
const beatenFirstScreenBudget = time.Nanosecond

func newFirstScreenRig(t *testing.T, budget ...time.Duration) *firstScreenRig {
	t.Helper()
	given := unreachableFirstScreenBudget
	if len(budget) == 1 {
		given = budget[0]
	}
	database := storetest.Open(t)
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte(firstScreenPage), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	people := newMemoryPeople()
	if err := people.SignIn(context.Background(), "sami@d.example", "refresh-token"); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	sessions := &memorySessionStore{generations: map[string]int64{"sami@d.example": 0}}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Sessions:   sessions,
		Identity:   identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		Store:      database,
		Docs:       documents,
		Events:     broker,
		WebDistDir: webDist,
		Lifetime:   t.Context(),

		FirstScreenBudget: given,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("sami@d.example", 0, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return &firstScreenRig{cookie: cookie, database: database, documents: documents, handler: handler, sessions: sessions}
}

// call makes one request as the signed-in reader, unless signedIn is false.
func (r *firstScreenRig) call(t *testing.T, method, path string, signedIn bool, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://dispatch.test"+path, nil)
	if signedIn {
		request.AddCookie(r.cookie)
	}
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// write posts body as the signed-in reader and answers the response's JSON.
func (r *firstScreenRig) write(t *testing.T, path, body string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test"+path, strings.NewReader(body))
	request.AddCookie(r.cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://dispatch.test")
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode POST %s %s: %v", path, response.Body.String(), err)
	}
	return answer
}

// seedDocument writes an issue holding one document and answers the issue's key and the
// document's slug.
func (r *firstScreenRig) seedDocument(t *testing.T, markdown string) (string, string) {
	t.Helper()
	r.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := r.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		key, _ = issue["issue"].(map[string]any)["key"].(string)
	}
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}
	body, err := json.Marshal(map[string]string{"content": markdown, "name": "notes.md"})
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}
	created := r.write(t, "/api/v1/issues/"+key+"/artifacts", string(body))
	artifact, _ := created["artifact"].(map[string]any)
	slug, _ := artifact["slug"].(string)
	if slug == "" {
		t.Fatalf("the document answered no slug: %v", created)
	}
	return key, slug
}

// documentID is the id of the issue document at slug, read through the API the page reads it by.
func (r *firstScreenRig) documentID(t *testing.T, key, slug string) string {
	t.Helper()
	response := r.call(t, http.MethodGet, "/api/v1/issues/"+key+"/artifacts/"+slug, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read the document: status=%d body=%s", response.Code, response.Body.String())
	}
	var artifact struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &artifact); err != nil {
		t.Fatalf("decode the document %s: %v", response.Body.String(), err)
	}
	if artifact.ID == "" {
		t.Fatalf("the document answered no id: %s", response.Body.String())
	}
	return artifact.ID
}

// theDocument is the markdown these rows seed: a heading and two paragraphs, each a block of its
// own, so a page carrying the document carries three block ids and their text.
const theDocument = "# The lease window\n\nThe window opens on the first lease.\n\nIt closes on the last.\n"

// blockIDsIn is every `data-block-id` value in page, in the order the page carries them.
func blockIDsIn(page string) []string {
	ids := []string{}
	rest := page
	for {
		at := strings.Index(rest, `data-block-id="`)
		if at < 0 {
			return ids
		}
		rest = rest[at+len(`data-block-id="`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return ids
		}
		ids = append(ids, rest[:end])
		rest = rest[end:]
	}
}

// scriptTagsIn is every script start tag in page, in the order the page carries them.
func scriptTagsIn(page string) []string {
	tags := []string{}
	rest := page
	for {
		at := strings.Index(rest, "<script")
		if at < 0 {
			return tags
		}
		rest = rest[at:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return append(tags, rest)
		}
		tags = append(tags, rest[:end+1])
		rest = rest[end+1:]
	}
}

func TestDocumentPageCarriesItsOpeningBlocks(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the document page: status=%d body=%s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	for _, text := range []string{"The lease window", "The window opens on the first lease.", "It closes on the last."} {
		if !strings.Contains(page, text) {
			t.Fatalf("the page carries no %q:\n%s", text, page)
		}
	}
	ids := blockIDsIn(page)
	if len(ids) != 3 {
		t.Fatalf("the page carries %d block ids, want the document's three: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id == "" {
			t.Fatalf("the page carries an empty block id: %v", ids)
		}
	}
	// Nothing the browser has to fetch and run stands between the page's first byte and the
	// reader's first block. Every script the page carries before the blocks is the build's own
	// bootstrap: inline, so it costs no request, or `type="module"`, which is deferred until the
	// document has been parsed. So the blocks are painted from the page's own bytes, with no app
	// code downloaded, evaluated or run - which is what the browser row with the app's code
	// blocked proves from the other side.
	blocks := strings.Index(page, `data-block-id="`)
	if blocks < 0 {
		t.Fatalf("the page carries no block at all:\n%s", page)
	}
	for _, tag := range scriptTagsIn(page[:blocks]) {
		if strings.Contains(tag, " src=") && !strings.Contains(tag, `type="module"`) {
			t.Fatalf("a script the browser must fetch and run stands before the document: %s", tag)
		}
	}
	if label := strings.Index(page, `aria-label="Document text"`); label < 0 || label > blocks {
		t.Fatalf("the document is not named as the page's own preview names it:\n%s", page)
	}
	if root := strings.Index(page, `id="root"`); root < 0 || root > blocks {
		t.Fatalf("the document's blocks do not sit after the app's own root:\n%s", page)
	}
	// Nobody's to keep, and no validator for a browser to revalidate with.
	if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if got := response.Header().Get("ETag"); got != "" {
		t.Fatalf("the document page carries an ETag %q", got)
	}
}

func TestDocumentPageCarriesTheIssuesSpec(t *testing.T) {
	rig := newFirstScreenRig(t)
	rig.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := rig.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document","spec":`+
		`"# The lease window\n\nThe window opens on the first lease.\n"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}

	page := rig.call(t, http.MethodGet, "/issues/"+key, true, nil).Body.String()
	if !strings.Contains(page, "The window opens on the first lease.") {
		t.Fatalf("the Spec tab's page carries no spec:\n%s", page)
	}
	if len(blockIDsIn(page)) == 0 {
		t.Fatalf("the Spec tab's page carries no block ids:\n%s", page)
	}
}

func TestDocumentPageWithoutASessionCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, false, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the page without a session: status=%d body=%s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	if page != firstScreenPage {
		t.Fatalf("the page without a session is not the build's own shell:\n%s", page)
	}
	for _, text := range []string{"The lease window", "data-block-id"} {
		if strings.Contains(page, text) {
			t.Fatalf("the page without a session carries %q:\n%s", text, page)
		}
	}
	if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
		t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
	}
}

func TestDocumentPageWithARevokedSessionCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	if !strings.Contains(rig.call(t, http.MethodGet, address, true, nil).Body.String(), "The lease window") {
		t.Fatal("the reader's own session read no document to begin with")
	}

	if err := rig.sessions.RevokeSessions(context.Background(), "sami@d.example"); err != nil {
		t.Fatalf("revoke the session: %v", err)
	}

	page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a revoked session was sent a document:\n%s", page)
	}
}

func TestDocumentPageOfAnAddressNamingNoDocumentCarriesNothing(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, _ := rig.seedDocument(t, theDocument)

	for _, address := range []string{
		"/issues/" + key + "/artifacts/no-such-document",
		"/issues/NOSUCH-9/artifacts/notes-md",
		"/issues/NOSUCH-9",
	} {
		page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
		if page != firstScreenPage {
			t.Fatalf("%s was sent a document:\n%s", address, page)
		}
	}
}

// A file is not a document, and `/text` answers NOT_DOCUMENT for it. The page draws nothing
// rather than inventing a reading of bytes nobody asked it to read.
func TestPageOfAnAddressNamingAFileCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, _ := rig.seedDocument(t, theDocument)
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("an image"))
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	upload := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/issues/"+key+"/artifacts", body)
	upload.AddCookie(rig.cookie)
	upload.Header.Set("Content-Type", bodyType)
	upload.Header.Set("Origin", "http://dispatch.test")
	uploaded := httptest.NewRecorder()
	rig.handler.ServeHTTP(uploaded, upload)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a file: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	var created struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(uploaded.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the upload %s: %v", uploaded.Body.String(), err)
	}

	page := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+created.Artifact.Slug, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a file's address was sent a document:\n%s", page)
	}
}

// A document whose tree this server cannot read is one the page cannot draw, and the reader's
// own editor meets that refusal with its reason a moment later. The page is the shell.
func TestPageOfADocumentThatWillNotLoadCarriesNoDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	if !strings.Contains(rig.call(t, http.MethodGet, address, true, nil).Body.String(), "The lease window") {
		t.Fatal("the document did not draw to begin with")
	}
	artifact := rig.documentID(t, key, slug)
	if err := rig.documents.InjectSchemaInvalidForTest(context.Background(), artifact); err != nil {
		t.Fatalf("make the document unreadable: %v", err)
	}

	page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
	if page != firstScreenPage {
		t.Fatalf("a document that will not load was drawn:\n%s", page)
	}
}

// A pinned version is a different document than the one this draws, and every other address
// opens no document at all, so each is the build's own shell with its own caching.
func TestOnlyADocumentAddressCarriesADocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	for _, address := range []string{
		"/issues/" + key + "/artifacts/" + slug + "?v=1",
		"/issues/" + key + "/activity",
		"/issues/" + key + "/artifacts/" + slug + "/extra",
		"/inbox",
		"/",
	} {
		response := rig.call(t, http.MethodGet, address, true, nil)
		if response.Body.String() != firstScreenPage {
			t.Fatalf("%s was sent a document:\n%s", address, response.Body.String())
		}
		if got := response.Header().Get("ETag"); got == "" {
			t.Fatalf("%s is a shell and carries no ETag", address)
		}
	}
}

// Which words of a path are an issue key is text.IsIssueKey's to say, the grammar a reference is
// parsed by (`text/refs.go`'s issueKeyPattern), so a path this handler opens a document at and a
// reference naming that document agree on what an issue key is.
func TestADocumentAddressNamesAnIssueKeyAsDispatchMintsOne(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{"/issues/CORE-1", true},
		{"/issues/C0RE2-10/spec", true},
		{"/issues/AB-1/artifacts/notes-md", true},
		{"/issues/C-1", false},
		{"/issues/CORE-01", false},
		{"/issues/CORE-0", false},
		{"/issues/core-1", false},
		{"/issues/VERYLONGKEY-1", false},
		{"/issues/CORE-1x", false},
		{"/issues/CORE", false},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+test.path, nil)
		if _, ok := documentAddress(request, test.path); ok != test.want {
			t.Fatalf("%s opened a document: %v, want %v", test.path, ok, test.want)
		}
	}
}

// A browser that kept a page it was sent before - another reader's document, or one this reader
// may no longer read - must never be told to use it again. The document page carries no
// validator, so there is nothing for an If-None-Match to match, and the whole page is answered.
func TestDocumentPageNeverAnswers304(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug

	first := rig.call(t, http.MethodGet, address, true, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("the first read: status=%d", first.Code)
	}
	shell := rig.call(t, http.MethodGet, "/inbox", true, nil)
	tag := shell.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the shell carries no ETag to ask with")
	}

	for _, condition := range []string{tag, "*", `"anything"`} {
		again := rig.call(t, http.MethodGet, address, true, map[string]string{"If-None-Match": condition})
		if again.Code != http.StatusOK {
			t.Fatalf("If-None-Match %s: status=%d, want 200", condition, again.Code)
		}
		if !strings.Contains(again.Body.String(), "The lease window") {
			t.Fatalf("If-None-Match %s answered no document:\n%s", condition, again.Body.String())
		}
	}
}

// All four addresses a document opens at carry it: the issue's own spec at `/issues/{key}` and
// at `/issues/{key}/spec`, which a link to a block of the spec names, and the issue document
// under both spellings of its artifact path, which are both written.
func TestEveryDocumentAddressCarriesItsDocument(t *testing.T) {
	rig := newFirstScreenRig(t)
	rig.write(t, "/api/v1/projects", `{"key":"BIG","name":"Big documents"}`)
	issue := rig.write(t, "/api/v1/issues", `{"project":"BIG","title":"A document","spec":`+
		`"# The lease window\n\nThe window opens on the first lease.\n"}`)
	key, _ := issue["key"].(string)
	if key == "" {
		t.Fatalf("the issue answered no key: %v", issue)
	}
	body, err := json.Marshal(map[string]string{"content": theDocument, "name": "notes.md"})
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}
	created := rig.write(t, "/api/v1/issues/"+key+"/artifacts", string(body))
	slug, _ := created["artifact"].(map[string]any)["slug"].(string)
	if slug == "" {
		t.Fatalf("the document answered no slug: %v", created)
	}

	for _, address := range []string{
		"/issues/" + key,
		"/issues/" + key + "/spec",
		"/issues/" + key + "/artifacts/" + slug,
		"/issues/" + key + "/artifact/" + slug,
	} {
		page := rig.call(t, http.MethodGet, address, true, nil).Body.String()
		if !strings.Contains(page, "The window opens on the first lease.") {
			t.Fatalf("%s carries no document:\n%s", address, page)
		}
		if len(blockIDsIn(page)) == 0 {
			t.Fatalf("%s carries no block ids:\n%s", address, page)
		}
	}
}

// The page is what the reader is waiting for. A document the server cannot draw inside its
// budget is one the page's own read draws for them a moment later, so the page goes without it
// rather than holding every reader of that document for the wait.
//
// Two rigs, because the budget is the context's: the first proves this document draws when the
// page waits for it, so the second's shell is the budget's doing and not an undrawable document's,
// and the second gives the read a nanosecond, which nothing on any machine beats. Neither half
// waits on the clock or on how busy the machine is.
func TestADocumentReadPastItsBudgetSendsTheShell(t *testing.T) {
	drawing := newFirstScreenRig(t)
	drawnKey, drawnSlug := drawing.seedDocument(t, theDocument)
	drawnAddress := "/issues/" + drawnKey + "/artifacts/" + drawnSlug
	if !strings.Contains(drawing.call(t, http.MethodGet, drawnAddress, true, nil).Body.String(), "The lease window") {
		t.Fatal("the document did not draw to begin with")
	}

	rig := newFirstScreenRig(t, beatenFirstScreenBudget)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug
	response := rig.call(t, http.MethodGet, address, true, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the page past its budget: status=%d", response.Code)
	}
	if response.Body.String() != firstScreenPage {
		t.Fatalf("a read past its budget still drew:\n%s", response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
		t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
	}
}

// pageWithADraw is a page handler whose draw is draw's, over a dashboard build of one page, with
// nothing but the budget between the reader and it. The draw is given rather than seeded,
// because what these two rows read is what the page does with a draw that takes its own time -
// a caller check waiting on the sign-in issuer, a fold that watches no context - and no document
// can be made to do that on purpose.
func pageWithADraw(
	t *testing.T,
	budget time.Duration,
	draw func(context.Context, *http.Request, api.FirstScreenAddress) api.FirstScreen,
) (*router, string) {
	t.Helper()
	webDist := t.TempDir()
	page := filepath.Join(webDist, "index.html")
	if err := os.WriteFile(page, []byte(firstScreenPage), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	_, appContext := newTestRouter(t)
	appContext.WebDistDir = webDist
	appContext.firstScreenBudget = budget
	return &router{ctx: appContext, firstScreen: draw}, page
}

// theDocumentAddress is the address these two rows open.
const theDocumentAddress = "/issues/CORE-1/artifacts/notes-md"

// A draw that is still out when the budget is spent costs the reader nothing but the document:
// the page goes out, as the shell, there and then.
//
// The budget has to bound the draw from outside it, because nothing inside watches it all the
// way: the caller check can wait on the sign-in issuer for up to the 15 s of
// identity.Membership's refresh, and the fold of a cold document (docs/fold.go) takes no context
// at all. The draw here takes none either, which is how it stands for both. Before the budget
// was spent outside the draw this row did not fail - it hung, which is what a reader got.
func TestADrawStillOutWhenTheBudgetIsSpentSendsTheShell(t *testing.T) {
	drawing := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	page, path := pageWithADraw(t, beatenFirstScreenBudget,
		func(context.Context, *http.Request, api.FirstScreenAddress) api.FirstScreen {
			close(drawing)
			<-release
			return api.FirstScreen{Drawn: true, HTML: `<p data-block-id="b-1">The document</p>`}
		})

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
		page.servePageForDocument(response, request, path, theDocumentAddress)
		answered <- response
	}()

	// Not a verdict on the clock: the draw above never ends on its own, so a page that waits for
	// it waits for ever, and this bound is only what turns that into a failure rather than a
	// hung test.
	select {
	case response := <-answered:
		if response.Code != http.StatusOK {
			t.Fatalf("the page past its budget: status=%d", response.Code)
		}
		if response.Body.String() != firstScreenPage {
			t.Fatalf("a draw still out past the budget reached the page:\n%s", response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
			t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the page waited on a draw that watches no context: the budget bounded nothing")
	}
	<-drawing
}

// The budget is the page's, not a deadline on the read: a context cancelled under a running
// query makes pgx close the connection it was on (outbox/lease.go), so a database slow enough to
// cost pages their documents would also cost the pool a connection for every page view. The draw
// is handed a detached context and the page stops waiting for it instead.
func TestTheDrawTheBudgetBeatIsNotCancelledUnderIt(t *testing.T) {
	drawn := make(chan error, 1)
	page, path := pageWithADraw(t, beatenFirstScreenBudget,
		func(ctx context.Context, _ *http.Request, _ api.FirstScreenAddress) api.FirstScreen {
			drawn <- ctx.Err()
			return api.FirstScreen{}
		})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
	page.servePageForDocument(response, request, path, theDocumentAddress)

	if response.Body.String() != firstScreenPage {
		t.Fatalf("the page past its budget drew:\n%s", response.Body.String())
	}
	if err := <-drawn; err != nil {
		t.Fatalf("the draw ran under a context the budget had already ended (%v): a query on it would cost the pool its connection", err)
	}
}

// A draw the page has stopped waiting for still ends. It is detached from the request on
// purpose, so neither the reader leaving nor the budget nor shutdown can end it - these
// goroutines are registered with nothing that waits for them - and its own deadline
// (firstScreenDrawCap) is all there is. Without one, a draw that parks - on a failed room's
// recovery, on the rooms pool's four connections, on the sign-in issuer - holds a goroutine and
// its slot for the life of the process, and this row holds the draw open to read that it ends
// anyway.
func TestADetachedDrawEndsByItself(t *testing.T) {
	ended := make(chan error, 1)
	page, path := pageWithADraw(t, beatenFirstScreenBudget,
		func(ctx context.Context, _ *http.Request, _ api.FirstScreenAddress) api.FirstScreen {
			<-ctx.Done()
			ended <- ctx.Err()
			return api.FirstScreen{}
		})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
	page.servePageForDocument(response, request, path, theDocumentAddress)
	if response.Body.String() != firstScreenPage {
		t.Fatalf("the page past its budget drew:\n%s", response.Body.String())
	}

	// Not a verdict on the clock: the draw above ends when its context does and never otherwise,
	// so this bound only turns a draw nothing can end into a failure rather than a hung test.
	select {
	case err := <-ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the detached draw ended with %v, want the draw cap's own deadline", err)
		}
	case <-time.After(firstScreenDrawCap + 30*time.Second):
		t.Fatalf("a draw the page gave up on was still running %s later: nothing can end it, and it holds its slot and a rooms-pool connection for the life of the process",
			firstScreenDrawCap+30*time.Second)
	}
}

// theDrawnDocument is what the draws of the row below hand the page when they do not panic.
const theDrawnDocument = `<p data-block-id="b-1">The document</p>`

// aBudgetADrawWins is what the row below gives a draw that answers at once: long enough that a
// draw which returns an answer always reaches the page, so the shell the panicking draw leaves
// is the panic's doing and not a budget no draw can win. The row waits it out once, for the one
// page view whose draw panics.
const aBudgetADrawWins = time.Second

// A draw that panics costs this page view its document and nothing else. The draw runs on a
// goroutine of its own, where an unrecovered panic takes the process down - every reader of this
// server, not the one reader whose document could not be drawn - and the draw recovers none of
// its own on the way: it reaches ygo's ApplyUpdateV1 and pmdoc.FirstScreenHTML, and neither
// recovers, where every other off-request decode in the documents package does
// (docs/read_cache.go's foldDocument, docs/websocket.go's LoadDoc, pmdoc/panic.go).
//
// So this row panics the draw and reads what is left: the page goes out as the shell when the
// budget is spent, the draw's slot comes back, and the server answers the next reader with a
// document. A control draws first under the same budget, so the shell below is the panic's and
// not the budget's. Without the recover the panic ends the test binary, and every row of this
// package goes with it, which is the process this row is about.
func TestADrawThatPanicsSendsTheShellAndTheServerLivesOn(t *testing.T) {
	var panicking atomic.Bool
	panicked := make(chan struct{}, 1)
	page, path := pageWithADraw(t, aBudgetADrawWins,
		func(context.Context, *http.Request, api.FirstScreenAddress) api.FirstScreen {
			if panicking.Load() {
				panicked <- struct{}{}
				panic("the document's history is not a document")
			}
			return api.FirstScreen{Drawn: true, HTML: theDrawnDocument}
		})
	servePage := func() *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
		page.servePageForDocument(response, request, path, theDocumentAddress)
		return response
	}

	// The slots held before either draw of this row begins. Read here and not after the control
	// page returns: the control's goroutine hands its answer over on `drawn` before its deferred
	// release runs (startDraw), so a count read once that page is out can still hold the
	// control's own slot, and a panicking draw that kept its slot would then leave exactly that
	// count behind.
	held := len(firstScreenDrawSlots)
	if drew := servePage().Body.String(); !strings.Contains(drew, theDrawnDocument) {
		t.Fatalf("a draw that answers did not reach the page to begin with, so this budget proves nothing:\n%s", drew)
	}

	panicking.Store(true)
	response := servePage()
	select {
	case <-panicked:
	default:
		t.Fatal("the draw never panicked: this row read a page view of nothing")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("the page whose draw panicked: status=%d", response.Code)
	}
	if response.Body.String() != firstScreenPage {
		t.Fatalf("a page whose draw panicked carried something other than the shell:\n%s", response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
		t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
	}

	// And the draw's slot came back with it: the release is deferred, so it runs as the panic
	// unwinds, and a draw that kept its slot would take one of the few this process has out of it
	// for good. Both draws of this row end, so the count goes back to what it was before the
	// control began and no higher. Not a verdict on the clock - each goroutine releases its slot
	// and ends - so this bound only turns a slot that never comes back into a failure rather
	// than a hung row.
	for begun := time.Now(); len(firstScreenDrawSlots) > held; {
		if time.Since(begun) > 30*time.Second {
			t.Fatalf("the slot of a draw that panicked never came back: %d of %d are held, against %d before this row's draws began",
				len(firstScreenDrawSlots), cap(firstScreenDrawSlots), held)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the process is still serving: the next reader of the same address gets their document,
	// which is what an unrecovered panic on that goroutine would have taken from them.
	panicking.Store(false)
	if drew := servePage().Body.String(); !strings.Contains(drew, theDrawnDocument) {
		t.Fatalf("the page view after a draw panicked lost its document:\n%s", drew)
	}
}

// withoutRecoveryWaitMark is how a context spells the mark docs.WithoutRecoveryWait puts on one.
// It is read off a context this row marks itself rather than written out here, so what the row
// looks for is the mark the docs package actually sets.
func withoutRecoveryWaitMark(t *testing.T) string {
	t.Helper()
	mark := strings.TrimPrefix(
		fmt.Sprint(docs.WithoutRecoveryWait(context.Background())), fmt.Sprint(context.Background()))
	if mark == "" {
		t.Fatal("docs.WithoutRecoveryWait leaves no mark a context spells")
	}
	return mark
}

// A failed document room refuses the draw at once instead of holding it for the room's recovery:
// the eviction that recovery waits for compacts under the document's advisory lock, which any
// transaction may hold, so the wait has no bound of its own and the page has no use for it - the
// reader is sent the shell and their own editor meets the room's refusal with its reason.
func TestADetachedDrawWaitsForNoFailedRoomsRecovery(t *testing.T) {
	drawing := make(chan string, 1)
	page, path := pageWithADraw(t, unreachableFirstScreenBudget,
		func(ctx context.Context, _ *http.Request, _ api.FirstScreenAddress) api.FirstScreen {
			drawing <- fmt.Sprint(ctx)
			return api.FirstScreen{}
		})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
	page.servePageForDocument(response, request, path, theDocumentAddress)

	if got, mark := <-drawing, withoutRecoveryWaitMark(t); !strings.Contains(got, mark) {
		t.Fatalf("the draw ran under %s, which carries no %s: a failed room would hold it for its recovery", got, mark)
	}
}

// The draws in flight are capped (firstScreenDrawSlots), because a draw past the budget is work
// nobody is waiting for and its heaviest step takes one of the four connections document loads
// share. A page view that finds no slot free sends the shell there and then, which is what the
// budget sends and what a refused draw sends; it does not queue behind the draws in flight.
func TestAPageViewWithNoDrawSlotSendsTheShell(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, cap(firstScreenDrawSlots)+1)
	// A budget no draw loses, so what this row reads is the cap on the draws in flight and never
	// the clock: a page that answers at all here answered without waiting for a draw.
	page, path := pageWithADraw(t, unreachableFirstScreenBudget,
		func(context.Context, *http.Request, api.FirstScreenAddress) api.FirstScreen {
			started <- struct{}{}
			<-release
			return api.FirstScreen{Drawn: true, HTML: `<p data-block-id="b-1">The document</p>`}
		})
	for range cap(firstScreenDrawSlots) {
		go func() {
			request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
			page.servePageForDocument(httptest.NewRecorder(), request, path, theDocumentAddress)
		}()
		// Every slot taken before the row asks for one more: the slot is taken before the draw
		// runs, so a draw that has started holds one.
		<-started
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+theDocumentAddress, nil)
		page.servePageForDocument(response, request, path, theDocumentAddress)
		answered <- response
	}()
	select {
	case response := <-answered:
		if response.Body.String() != firstScreenPage {
			t.Fatalf("a page view with no draw slot free drew the document:\n%s", response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != pageCacheControl {
			t.Fatalf("the shell's Cache-Control = %q, want %q", got, pageCacheControl)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a page view with no draw slot free waited on a draw of its own instead of sending the shell: the draws in flight are capped by nothing")
	}
	select {
	case <-started:
		t.Fatal("a page view with no draw slot free started a draw anyway: the draws in flight are capped by nothing")
	default:
	}
}

// A HEAD of a drawn document page answers how long its body is. The drawn page writes its own
// bytes rather than going through http.ServeContent, which is what sets the length for the shell,
// so a HEAD that answered without one would leave a browser - or a proxy, or a monitor - reading
// a page whose length the server never told it, while the same address under GET sends the bytes.
func TestAHeadOfADocumentPageAnswersItsLength(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)
	address := "/issues/" + key + "/artifacts/" + slug

	drawn := rig.call(t, http.MethodGet, address, true, nil)
	if !strings.Contains(drawn.Body.String(), "data-first-screen-address") {
		t.Fatalf("the page carries no document to measure:\n%s", drawn.Body.String())
	}
	if got := drawn.Header().Get("Content-Length"); got != strconv.Itoa(drawn.Body.Len()) {
		t.Fatalf("the drawn page's Content-Length = %q, want %d, the bytes it sent", got, drawn.Body.Len())
	}

	head := rig.call(t, http.MethodHead, address, true, nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD %s: status=%d", address, head.Code)
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(drawn.Body.Len()) {
		t.Fatalf("a HEAD of the drawn page answers Content-Length %q, want %d, what its GET sends", got, drawn.Body.Len())
	}
	if head.Body.Len() != 0 {
		t.Fatalf("a HEAD of the drawn page carries %d bytes of body", head.Body.Len())
	}
}

// The bytes are one reader's document: no shared cache may hold them, no browser may keep them,
// and there is no validator for either to revalidate with.
func TestADocumentPageIsNobodysToKeep(t *testing.T) {
	rig := newFirstScreenRig(t)
	key, slug := rig.seedDocument(t, theDocument)

	response := rig.call(t, http.MethodGet, "/issues/"+key+"/artifacts/"+slug, true, nil)
	if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if got := response.Header().Get("ETag"); got != "" {
		t.Fatalf("the document page carries an ETag %q", got)
	}
	if got := response.Header().Get("Last-Modified"); got != "" {
		t.Fatalf("the document page carries a Last-Modified %q", got)
	}
}
