//go:build twobuild

// The stored-document half of the two-build suite: each build reading, editing and keeping the
// references of the other's documents, over the API and the document socket
// (twobuild_harness_test.go holds the harness both halves start their builds with).

package main

// The two-build suite's stored-document half reads this tree's side of what each build holds
// (documentSides, tree ../../internal/dispatch); the previous build's side is the copy
// packages/envoy/scripts/build-dispatch-build.sh writes outside the checkout.
//ci:reads packages/envoy/internal/dispatch/pmdoc/schema/blocks.json documentSides readManifest (os.ReadFile) of the current build's block manifest
//ci:reads packages/envoy/internal/dispatch/pmdoc/testdata/corpus/*.md documentSides the current build's corpus: corpusFiles os.ReadDir, and requireCorpusCrossesBuilds os.ReadFile of each .md file it lists
//ci:reads packages/envoy/internal/dispatch/text/testdata/dispatch-text-references.json documentSides the current build's reference fixtures, read by referencesMarkdown through readFixture os.ReadFile
//ci:reads packages/envoy/internal/dispatch/text/testdata/dispatch-href-references.json documentSides the current build's reference fixtures, read by referencesMarkdown and hrefsReadApart through hrefTable and readFixture os.ReadFile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// ───── stored documents ─────────────────────────────────────────────────────

// referenceServerURL is DISPATCH_SERVER_URL for the document rows: the origin the reference
// fixtures' dashboard links name (text/testdata), so both builds read those links as references.
const referenceServerURL = "https://dispatch.test"

// documentSide is one build as a writer or a reader of documents: its process, the corpus and
// reference fixtures of its own tree, its block manifest, and what its page presents on a
// document socket.
type documentSide struct {
	name     string
	process  *dispatchProcess
	corpus   string
	fixtures string
	manifest blockManifest
	page     pageParameters
}

// documentSides is the previous build's side and the current build's, each from its own tree.
func (b *twoBuilds) documentSides(t *testing.T) []documentSide {
	t.Helper()
	tree := filepath.Join("..", "..", "internal", "dispatch")
	previousManifest := readManifest(t, b.build.path("pmdoc", "schema", "blocks.json"))
	currentManifest := readManifest(t, filepath.Join(tree, "pmdoc", "schema", "blocks.json"))
	return []documentSide{
		{
			name:     b.buildName(b.previous()),
			process:  b.previous(),
			corpus:   b.build.path("pmdoc", "testdata", "corpus"),
			fixtures: b.build.path("text", "testdata"),
			manifest: previousManifest,
			page:     previousManifest.page(pageSendsCreatable(t, b.build.path("web"))),
		},
		{
			name:     b.buildName(b.current()),
			process:  b.current(),
			corpus:   filepath.Join(tree, "pmdoc", "testdata", "corpus"),
			fixtures: filepath.Join(tree, "text", "testdata"),
			manifest: currentManifest,
			// The current page sends schema_creatable (packages/dispatch/web/src/features/doc/connection.ts).
			page: currentManifest.page(true),
		},
	}
}

// elementVersions are the two numbers the manifest may give an element: the schema version that
// first reads it and the one that first creates it, each 1 where it names none.
type elementVersions struct {
	Since          *int `json:"since"`
	CreatableSince *int `json:"creatable_since"`
}

type manifestAttribute struct {
	elementVersions
	Kind           string                     `json:"kind"`
	Choices        []string                   `json:"choices"`
	ChoiceVersions map[string]elementVersions `json:"choice_versions"`
	Default        json.RawMessage            `json:"default"`
	Server         bool                       `json:"server"`
}

type manifestType struct {
	elementVersions
	Name       string                       `json:"name"`
	Content    string                       `json:"content"`
	Attributes map[string]manifestAttribute `json:"attributes"`
	// Parents are the typed blocks this type stands in, at the end of one's own content; a type
	// that names none is a block of its own wherever a block stands.
	Parents []string `json:"parents"`
}

// blockManifest is a build's pmdoc/schema/blocks.json.
type blockManifest struct {
	Version int               `json:"version"`
	Nodes   []elementVersions `json:"nodes"`
	Marks   []elementVersions `json:"marks"`
	Types   []manifestType    `json:"types"`
}

func readManifest(t *testing.T, path string) blockManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the block manifest: %v", err)
	}
	var manifest blockManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode the block manifest %s: %v", path, err)
	}
	return manifest
}

// creatable is R for the manifest: the highest since among the elements an editor of its version
// may create, by the rule the page states (schemaCreatable in
// packages/dispatch/web/src/features/doc/schema-creatable.ts) and pmdoc.SchemaCreatable computes.
func (m blockManifest) creatable() int {
	creatable := 0
	count := func(element elementVersions) {
		since, creatableSince := 1, 1
		if element.Since != nil {
			since = *element.Since
		}
		if element.CreatableSince != nil {
			creatableSince = *element.CreatableSince
		}
		if creatableSince <= m.Version {
			creatable = max(creatable, since)
		}
	}
	for _, element := range slices.Concat(m.Nodes, m.Marks) {
		count(element)
	}
	for _, typed := range m.Types {
		count(typed.elementVersions)
		for _, attribute := range typed.Attributes {
			count(attribute.elementVersions)
			for _, choice := range attribute.Choices {
				count(attribute.ChoiceVersions[choice])
			}
		}
	}
	return creatable
}

// pageParameters is what a build's page presents when it opens a document socket
// (features/doc/connection.ts): the query its provider's websocket carries, and the token its
// provider authenticates with, which is the schema version alone.
type pageParameters struct {
	query url.Values
	token string
	// version is the schema_version the page presents and creatable the schema_creatable, 1 for
	// a page that sends none: every element of a manifest before them has since 1.
	version   int
	creatable int
}

func (p pageParameters) String() string { return "?" + p.query.Encode() + " with token " + p.token }

// page is what a page built with this manifest presents: schema_version, and schema_creatable
// when that build's page sends it.
func (m blockManifest) page(sendsCreatable bool) pageParameters {
	query := url.Values{"schema_version": {strconv.Itoa(m.Version)}}
	creatable := 1
	if sendsCreatable {
		creatable = m.creatable()
		query.Set("schema_creatable", strconv.Itoa(creatable))
	}
	return pageParameters{query: query, token: strconv.Itoa(m.Version), version: m.Version, creatable: creatable}
}

// pageSendsCreatable reports whether a built web app's scripts name schema_creatable, the
// parameter a page sends from the build that adds the fleet columns on. A page of a build that
// predates them sends schema_version alone.
func pageSendsCreatable(t *testing.T, web string) bool {
	t.Helper()
	scripts, err := filepath.Glob(filepath.Join(web, "assets", "*.js"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("the previous build's web app at %s holds no assets/*.js: %v", web, err)
	}
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if bytes.Contains(raw, []byte("schema_creatable")) {
			return true
		}
	}
	return false
}

// createDocument creates an issue in project whose spec is markdown, through p, and returns its key
// and its spec's id. It is forced past the duplicate check: the rows create many issues of one
// shape on purpose.
func createDocument(t *testing.T, p *dispatchProcess, project, title, markdown string) (string, string) {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		jsonBody(t, map[string]any{"project": project, "title": title, "spec": markdown, "force": true}), http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the document %q: %v: %s", title, err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// appendParagraph appends paragraph at the end of the document through p, an edit that versions
// the document in the request's own transaction.
func appendParagraph(t *testing.T, p *dispatchProcess, artifactID, paragraph string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		jsonBody(t, map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": paragraph + "\n"}}}), http.StatusOK)
}

// withParagraph is render with paragraph appended as the document's last block.
func withParagraph(render, paragraph string) string {
	switch {
	case render == "":
		return paragraph + "\n"
	case strings.HasSuffix(render, "\n"):
		return render + "\n" + paragraph + "\n"
	default:
		return render + "\n\n" + paragraph + "\n"
	}
}

// The Hocuspocus frames a document socket's admission is read from (ygo provider/websocket).
const (
	hocuspocusAuth          = 2
	hocuspocusAuthToken     = 0
	hocuspocusAuthDenied    = 1
	hocuspocusAuthenticated = 2
	// writableScope is the scope an Authenticated frame names for a socket that may write, and
	// readOnlyScope the one it names for a socket that may not.
	writableScope = "read-write"
	readOnlyScope = "readonly"
)

// fleetNumbers are the numbers the document socket's admission reads off the live registry rows
// (docs.readSchemaFleet): the lowest schema_version and the highest schema_creatable among the
// live server processes. A process of a build that predates those columns registers neither, and
// the zeros its row holds are what it counts as, so one live process of such a build puts the
// lowest version at 0. live is how many live rows the numbers were read from.
type fleetNumbers struct {
	minVersion, maxCreatable int
	live                     int
}

// fleet reads the live rows' numbers now. It fails when the registry holds no live row: the
// processes a row of this suite started are in it, and numbers read off an empty registry would
// say every page is read-only for a reason no build has.
func (b *twoBuilds) fleet(t *testing.T) fleetNumbers {
	t.Helper()
	var numbers fleetNumbers
	if err := b.database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(min(schema_version), 0), coalesce(max(schema_creatable), 0)
		from server_processes
		where heartbeat_at > now() - make_interval(secs => $1)`,
		processes.LiveWindow.Seconds()).Scan(&numbers.live, &numbers.minVersion, &numbers.maxCreatable); err != nil {
		t.Fatalf("read the live fleet's schema numbers: %v", err)
	}
	if numbers.live == 0 {
		t.Fatal("the registry holds no live server process, so there is no fleet to admit a page by")
	}
	return numbers
}

// admittedScope is the scope reader admits a socket with writer's page parameters in, by the rule
// the page admission states (docs.schemaFleet.admits): the page creates no more than every live
// process reads and no more than the highest any of them creates, and it reads at least what they
// create and what the document holds. fleet is what those live processes register. The document
// is one the writer created, so the highest since it holds is at most the writer's own schema
// version.
//
// While a process of a build that predates the fleet columns is live, the lowest version in the
// fleet is 0, which no page's schema_creatable is at or under, so a reader of this build admits
// every page read-only until that process is gone.
//
// A reader that predates the admission - its manifest names no nodes - knows no fleet: it
// compares the page's token, which is the page's schema version alone, with its own version, and
// answers read-only when they differ (the socket's token in
// packages/dispatch/web/src/features/doc/connection.ts).
// So a release that raises the manifest version makes every page of the newer build read-only on
// a process of the older one until the older build is gone, whatever the elements are creatable
// from.
func admittedScope(writer, reader documentSide, fleet fleetNumbers) string {
	if len(reader.manifest.Nodes) == 0 {
		if writer.page.token == strconv.Itoa(reader.manifest.Version) {
			return writableScope
		}
		return readOnlyScope
	}
	if writer.page.creatable <= min(fleet.minVersion, fleet.maxCreatable) &&
		writer.page.version >= max(fleet.maxCreatable, writer.manifest.Version) {
		return writableScope
	}
	return readOnlyScope
}

// documentSocket is a document socket as a page's provider holds it: authenticated with the
// page's token, reading every frame the room sends until the connection ends.
type documentSocket struct {
	artifactID string
	scope      string
	ended      chan struct{}
	endErr     error
}

// openDocumentSocket opens server's socket for the document with page's parameters, authenticates
// as the page's provider does, and returns once the server has answered with the socket's scope.
func openDocumentSocket(t *testing.T, server *dispatchProcess, artifactID string, page pageParameters) *documentSocket {
	t.Helper()
	address := "ws://127.0.0.1:" + strconv.Itoa(server.port) + "/ws/doc/" + artifactID + "?" + page.query.Encode()
	connection, response, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {dispatchTestLogin}})
	if err != nil {
		status := 0
		var body []byte
		if response != nil {
			status = response.StatusCode
			body, _ = io.ReadAll(response.Body)
		}
		t.Fatalf("open the document socket %s: status %d %s: %v", address, status, body, err)
	}
	socket := &documentSocket{artifactID: artifactID, ended: make(chan struct{})}
	t.Cleanup(func() {
		_ = connection.Close()
		<-socket.ended
	})
	auth := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(hocuspocusAuth)
		encoder.WriteVarUint(hocuspocusAuthToken)
		encoder.WriteVarString(page.token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, auth); err != nil {
		t.Fatalf("authenticate the document socket: %v", err)
	}
	scopes := make(chan string, 1)
	go func() {
		defer close(socket.ended)
		for {
			_, message, err := connection.ReadMessage()
			if err != nil {
				socket.endErr = err
				return
			}
			decoder := encoding.NewDecoder(message)
			if _, err := decoder.ReadVarString(); err != nil {
				continue
			}
			if kind, err := decoder.ReadVarUint(); err != nil || kind != hocuspocusAuth {
				continue
			}
			answer, err := decoder.ReadVarUint()
			if err != nil {
				continue
			}
			text, _ := decoder.ReadVarString()
			switch answer {
			case hocuspocusAuthenticated:
				text = "scope " + text
			case hocuspocusAuthDenied:
				text = "denied: " + text
			default:
				continue
			}
			select {
			case scopes <- text:
			default:
			}
		}
	}()
	select {
	case answer := <-scopes:
		socket.scope = strings.TrimPrefix(answer, "scope ")
	case <-socket.ended:
		t.Fatalf("the document socket for %s closed before the server answered its authentication: %v", artifactID, socket.endErr)
	case <-time.After(10 * time.Second):
		t.Fatalf("the server never answered the document socket's authentication for %s", artifactID)
	}
	return socket
}

func (s *documentSocket) closed() bool {
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}

// corpusFiles is every markdown file of a corpus directory, by name.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the corpus %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, entry.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("the corpus %s holds no markdown file", dir)
	}
	return files
}

// A document either build wrote reads and edits on the other. For each writer X and each file of
// X's own corpus: a document created on X and settled answers /text on Y and takes an append edit
// there, and X then renders it as its first render with the appended paragraph after it; on Y, a
// document socket opened with X's page parameters is admitted writable and stays open. A version X
// wrote holding every typed block of X's manifest is approved, the answer to its ask block
// makes Y write the next version, and the approval is carried to it. A body holding every
// reference form of X's text/testdata keeps its refs rows after Y rewrites it, but for the edges a
// Y from before the unknown-kind guard drops and the rows D10's grammar change makes Y add, each
// of which it logs (requireRefsKept).
func TestTwoBuildsReadAndEditEachOthersDocuments(t *testing.T) {
	builds := newTwoBuilds(t)
	env := map[string]string{"DISPATCH_SERVER_URL": referenceServerURL}
	builds.startPrevious(t, env)
	builds.startCurrent(t, env)
	sides := builds.documentSides(t)
	for index, writer := range sides {
		reader := sides[1-index]
		t.Run(writer.name+" writes, "+reader.name+" reads", func(t *testing.T) {
			project := uniqueProjectKey(t, "DOC")
			writer.process.createProject(t, project, "Two builds")
			t.Run("corpus", func(t *testing.T) {
				requireCorpusCrossesBuilds(t, builds, writer, reader, project)
			})
			t.Run("approval carry", func(t *testing.T) {
				requireApprovalCarriedAcrossBuilds(t, writer, reader, project)
			})
			t.Run("references", func(t *testing.T) {
				requireReferencesKeptAcrossBuilds(t, builds, writer, reader, project)
			})
		})
	}
}

// socketOpenFor is how long every socket the corpus row opened must stay open after the last one
// was admitted: long enough for a server that closes a socket it admitted to have done so.
const socketOpenFor = 3 * time.Second

// corpusSkipped are the corpus files this row leaves out, each with why. A corpus file is
// markdown for the parsers' fixtures, which a server does not have to be able to store.
var corpusSkipped = map[string]string{
	"requirements-tasks-and-checks.md":               "its plan tasks and checks point at blocks of a document no project this row creates holds, and a stored block pointer is held to a block the database has (CheckBlockPointers)",
	"ask-opening-with-a-list.md":                     "its ask writes its options and no question, which a whole write refuses, whichever build takes it (pmdoc.AskWrittenContentError)",
	"footnote-definition-ask-opening-with-a-list.md": "its ask writes its options and no question, which a whole write refuses, whichever build takes it (pmdoc.AskWrittenContentError)",
}

// corpusSkips reports whether this row leaves a corpus document out: by name, for the three
// above, or by what the document HOLDS, for the one class a name cannot be trusted for. A
// document carrying U+0000 is refused by either build at the create (NUL_CHARACTER, decodeJSON),
// and the corpus gains such documents as the parsers gain fixtures, so this reads the markdown
// rather than its file name: a fixture added under any name is covered, and one named nul-
// something that holds no NUL is written through both builds like any other.
func corpusSkips(file string, markdown []byte) bool {
	if _, skipped := corpusSkipped[file]; skipped {
		return true
	}
	return bytes.ContainsRune(markdown, 0)
}

// requireCorpusCrossesBuilds creates every corpus document of the writer's on the writer, then
// waits for each to settle before any build reads it: while two builds serve, an ask block's state
// reaches its markdown at its document's settlement alone, which keeps it in step with the ask's
// row for the earlier build's pages (docs' rollWindowAskAttributes), since this build gives a
// server attribute no written default (pmdoc's writtenDefault) and the earlier build writes the
// default `state="open"` into every ask block it renders. So the writer's first render, taken once
// the document has settled, is the document as the roll window keeps it, and the reader's append
// changes nothing else in it. The documents are all created before the first wait, so their
// settle delays run together.
func requireCorpusCrossesBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	fleet := builds.fleet(t)
	type corpusDocument struct{ file, artifactID string }
	var created []corpusDocument
	files := corpusFiles(t, writer.corpus)
	for _, file := range files {
		markdown, err := os.ReadFile(filepath.Join(writer.corpus, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if corpusSkips(file, markdown) {
			continue
		}
		_, artifactID := createDocument(t, writer.process, project, "Corpus "+file, string(markdown))
		created = append(created, corpusDocument{file: file, artifactID: artifactID})
	}
	for _, document := range created {
		waitForSettlementOwed(t, builds.database, document.artifactID, false)
	}
	var sockets []*documentSocket
	for _, document := range created {
		file, artifactID := document.file, document.artifactID
		first := writer.process.documentText(t, artifactID)
		reader.process.documentText(t, artifactID)

		paragraph := "Appended on the " + reader.name + " to " + file + "."
		appendParagraph(t, reader.process, artifactID, paragraph)
		if got, want := writer.process.documentText(t, artifactID), withParagraph(first, paragraph); got != want {
			t.Errorf("%s: after the %s appended a paragraph, the %s renders\n%q\nwant its first render with the paragraph after it\n%q",
				file, reader.name, writer.name, got, want)
		}

		socket := openDocumentSocket(t, reader.process, artifactID, writer.page)
		if want := admittedScope(writer, reader, fleet); socket.scope != want {
			t.Errorf("%s: the %s admits a socket with the %s page's parameters (%s) as %q, want %q",
				file, reader.name, writer.name, writer.page, socket.scope, want)
		}
		sockets = append(sockets, socket)
	}
	time.Sleep(socketOpenFor)
	for _, socket := range sockets {
		if socket.closed() {
			t.Errorf("the %s closed the socket it admitted for document %s: %v", reader.name, socket.artifactID, socket.endErr)
		}
	}
	t.Logf("%d corpus documents of the %s read, took an edit and kept a socket open on the %s, admitted %s by a fleet reading version %d and creating up to %d",
		len(files), writer.name, reader.name, admittedScope(writer, reader, fleet), fleet.minVersion, fleet.maxCreatable)
}

// typedBlockID is the block id the typed-block document gives each typed block: tb-<type name>.
func typedBlockID(name string) string { return "tb-" + name }

// typedBlocksMarkdown is a document holding one block of every typed block both manifests
// declare, each with its id and every attribute a writer sets at its default. An ask block
// (the ask type) is given two choices, First and Second. A type only the writer's manifest
// declares is left out: the reader could not read such a document at all, which is the point of
// the manifest's own staging tests and not of this row, which is about what both builds read.
//
// Where each block stands is the manifest's to say. A type that names no parents is a block of
// the document's own, in manifest order. A type that names parents, as the reading names the
// decision and the ask, stands only at the end of a block of one of them, after that block's own
// content, so it is written there: in the block of the first of its parents the document writes,
// after that parent's content, in manifest order among that parent's other such types. A parent's
// fence is one colon longer than the longest fence inside it, which is how markdown closes the
// inner block before the outer one. A type none of whose parents the document writes fails the
// test, as does one the document reaches through no block of its own. So a document of types
// that name no parents, as every manifest before the reading's is, is the one this helper wrote
// before it read parents, byte for byte.
func typedBlocksMarkdown(t *testing.T, manifest, reader blockManifest) string {
	t.Helper()
	var written []manifestType
	for _, typed := range manifest.Types {
		if slices.ContainsFunc(reader.Types, func(other manifestType) bool { return other.Name == typed.Name }) {
			written = append(written, typed)
		}
	}
	writes := func(name string) bool {
		return slices.ContainsFunc(written, func(typed manifestType) bool { return typed.Name == name })
	}
	// own are the types written as blocks of the document's own, and inside the types written at
	// the end of each parent's block.
	var own []manifestType
	inside := map[string][]manifestType{}
	for _, typed := range written {
		if len(typed.Parents) == 0 {
			own = append(own, typed)
			continue
		}
		at := slices.IndexFunc(typed.Parents, writes)
		if at < 0 {
			t.Fatalf("the %s block stands only at the end of a block of %q, none of which both manifests declare; teach typedBlocksMarkdown where to write it", typed.Name, typed.Parents)
		}
		inside[typed.Parents[at]] = append(inside[typed.Parents[at]], typed)
	}
	placed := 0
	// block is typed's block, the blocks inside it included, and the length of its fence.
	var block func(typed manifestType) (string, int)
	block = func(typed manifestType) (string, int) {
		placed++
		attributes := []string{"#" + typedBlockID(typed.Name)}
		for _, name := range slices.Sorted(maps.Keys(typed.Attributes)) {
			attribute := typed.Attributes[name]
			if attribute.Server || len(attribute.Default) == 0 {
				continue
			}
			var value any
			if err := json.Unmarshal(attribute.Default, &value); err != nil {
				t.Fatalf("decode the default of %s.%s: %v", typed.Name, name, err)
			}
			switch value := value.(type) {
			case string:
				if value == "" {
					attributes = append(attributes, name)
				} else {
					attributes = append(attributes, fmt.Sprintf("%s=%q", name, value))
				}
			case bool:
				attributes = append(attributes, fmt.Sprintf("%s=%q", name, strconv.FormatBool(value)))
			default:
				t.Fatalf("the harness writes no %T default, as %s.%s has; teach typedBlocksMarkdown to write it", value, typed.Name, name)
			}
		}
		var content []string
		for _, token := range strings.Fields(typed.Content) {
			switch strings.TrimRight(token, "+*?") {
			case "block", "paragraph":
				content = append(content, "The "+typed.Name+" block says one thing.")
			case "bullet_list":
				content = append(content, "- First: The first choice.\n- Second: The second choice.")
			default:
				t.Fatalf("the harness writes no %q content, as the %s block takes; teach typedBlocksMarkdown to write it", token, typed.Name)
			}
		}
		fence := 3
		for _, child := range inside[typed.Name] {
			markdown, childFence := block(child)
			content = append(content, strings.TrimSuffix(markdown, "\n"))
			fence = max(fence, childFence+1)
		}
		colons := strings.Repeat(":", fence)
		return colons + typed.Name + "{" + strings.Join(attributes, " ") + "}\n" + strings.Join(content, "\n\n") + "\n" + colons + "\n", fence
	}
	blocks := []string{"# Every typed block\n"}
	for _, typed := range own {
		markdown, _ := block(typed)
		blocks = append(blocks, markdown)
	}
	if placed != len(written) {
		t.Fatalf("the document reaches %d of the %d typed blocks both manifests declare: a type whose parents stand only inside one another is in no block of the document's own; teach typedBlocksMarkdown where to write it", placed, len(written))
	}
	return strings.Join(blocks, "\n")
}

// servedReview is a review as GET /api/v1/artifacts/{id}/reviews lists it, newest first.
type servedReview struct {
	Version int         `json:"version"`
	State   string      `json:"state"`
	Actor   model.Actor `json:"actor"`
}

func requireApprovalCarriedAcrossBuilds(t *testing.T, writer, reader documentSide, project string) {
	if !slices.ContainsFunc(writer.manifest.Types, func(typed manifestType) bool { return typed.Name == "ask" }) {
		t.Fatalf("the %s's manifest declares no ask block, the block an approval is carried by", writer.name)
	}
	issueKey, artifactID := createDocument(t, writer.process, project, "Every typed block", typedBlocksMarkdown(t, writer.manifest, reader.manifest))
	askID := waitForBlockAsk(t, writer.process, issueKey, typedBlockID("ask"))

	var approval servedReview
	approved := writer.process.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	if err := json.Unmarshal(approved, &approval); err != nil || approval.State != "approved" {
		t.Fatalf("approve the typed-block document on the %s: %v: %s", writer.name, err, approved)
	}
	reader.process.call(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		`{"selected":["First"],"expected_edited_at":null}`, http.StatusOK)

	// The carried approval is in the approver's name, and its whole actor crosses the builds, the
	// proven flag included: neither build may lose the mark when it carries the other's review.
	// Every release from legion-envoy-v30.44.0 on stores model.Actor.Proven (main's #4281), and
	// no pair the harness makes holds a build older than that: dispatch-two-builds pairs this tree
	// with the newest release before it, which only moves forward, and dispatch-two-builds-same
	// pairs it with its own build. So the comparison holds every field of the actor, the flag too.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var reviews []servedReview
		getJSON(t, writer.process, "/api/v1/artifacts/"+artifactID+"/reviews", &reviews)
		if len(reviews) > 1 && reviews[0].State == "approved" && reviews[0].Version > approval.Version &&
			reviews[0].Actor == (model.Actor{Kind: "user", ID: dispatchTestLogin, Proven: true}) {
			t.Logf("the %s carried the approval of version %d to version %d", reader.name, approval.Version, reviews[0].Version)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer on the %s to the ask block of a version the %s wrote and %s approved (version %d) carried no approval within 30s; reviews: %+v\n%s:\n%s",
				reader.name, writer.name, dispatchTestLogin, approval.Version, reviews, reader.name, reader.process.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForBlockAsk waits for the ask settlement indexed from the ask block blockID of the
// issue's documents, and returns its id.
func waitForBlockAsk(t *testing.T, p *dispatchProcess, issueKey, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		getJSON(t, p, "/api/v1/issues/"+issueKey+"/asks?state=all", &asks)
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ask was indexed from the block %s of %s within 20s: %+v", blockID, issueKey, asks)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// referencesMarkdown is a body holding every reference form of a build's text/testdata that stands
// alone as a document body: each body of its text table, and each link of its href table as the
// dashboard writes it and as its dispatch:// form, a paragraph each. Two kinds of text row are left
// out, each logged:
//   - A row that names an origin of its own: its links are references only to a server at that
//     origin, and both builds serve referenceServerURL here.
//   - A row that opens a typed block (a line that starts with `:::`): the table holds the opening
//     line alone, so the text reader is tested on an attribute's value. A document must close
//     the block, and the writer's manifest must declare the attribute. The row carries neither,
//     so the writer refuses it as a document (`typed block "callout" is unclosed at document
//     level`).
//
// Every other row must stand alone. A writer that refuses one fails the row, so a refused row is
// never dropped without a trace.
func referencesMarkdown(t *testing.T, fixtures string) string {
	t.Helper()
	var texts []struct {
		Body   string `json:"body"`
		Origin string `json:"origin"`
	}
	readFixture(t, fixtures, "dispatch-text-references.json", &texts)
	hrefs := hrefTable(t, fixtures)
	paragraphs := []string{"# Every reference form"}
	for _, row := range texts {
		switch {
		case row.Origin != "" && row.Origin != referenceServerURL:
			t.Logf("left out the text row %q: it names the origin %s", row.Body, row.Origin)
		case opensTypedBlock(row.Body):
			t.Logf("left out the text row %q: it opens a typed block it does not close", row.Body)
		default:
			paragraphs = append(paragraphs, row.Body)
		}
	}
	for _, row := range hrefs {
		paragraphs = append(paragraphs, referenceServerURL+row.Href, row.Ref)
	}
	return strings.Join(paragraphs, "\n\n") + "\n"
}

// hrefRow is a row of a build's text/testdata/dispatch-href-references.json: a dashboard link's
// path and the reference that build reads it as.
type hrefRow struct {
	Href string `json:"href"`
	Ref  string `json:"ref"`
}

// hrefTable is the href table of a build's text/testdata.
func hrefTable(t *testing.T, fixtures string) []hrefRow {
	t.Helper()
	var hrefs []hrefRow
	readFixture(t, fixtures, "dispatch-href-references.json", &hrefs)
	return hrefs
}

// readFixture decodes the JSON file name of a build's text/testdata into into.
func readFixture(t *testing.T, fixtures, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// hrefReadApart is an href the writer's and the reader's href tables both hold and map to
// different references: the reference grammar changed between the two builds. writerRow and
// readerRow are the refs rows the two references name (`kind id`, as documentRefs reads them), and
// d10 is whether the two readings are D10's (decisionsReadings).
type hrefReadApart struct {
	href, writerRef, readerRef string
	writerRow, readerRow       string
	d10                        bool
}

// decisionsPage is D10's href form, the Decisions tab of an issue: `/issues/KEY/decisions`, with
// or without a fragment.
var decisionsPage = regexp.MustCompile(`^/issues/([A-Z][A-Z0-9]*-[1-9][0-9]*)/decisions(#.*)?$`)

// decisionsReadings reports whether href is the Decisions tab of an issue (decisionsPage) and
// readings are exactly the two readings of it D10 names, in either order: the issue KEY,
// `dispatch://KEY/decisions`, in a build that carries D10, and the issue's decisions document,
// `dispatch://KEY/artifact/decisions-md`, in a build before it.
func decisionsReadings(href string, readings ...string) bool {
	match := decisionsPage.FindStringSubmatch(href)
	if match == nil || len(readings) != 2 {
		return false
	}
	issue, document := "dispatch://"+match[1]+"/decisions", "dispatch://"+match[1]+"/artifact/"+refs.RetiredDecisionsSlug
	return (readings[0] == issue && readings[1] == document) || (readings[0] == document && readings[1] == issue)
}

// hrefsReadApart is each href the reader's own href table maps to another reference than the
// writer's table does, keyed by both refs rows it reads as: the writer's reading and the reader's.
// Either build rewrites a body holding that href with its own reading, and so does either build's
// block-index pass that re-indexes it (awaitReferencesSettled). The one such href is D10's grammar
// change: the Decisions tab's `/issues/KEY/decisions` reads as the issue KEY in a build that
// carries it, and as the document KEY/decisions-md in a build before it.
func hrefsReadApart(t *testing.T, writer, reader documentSide) map[string]hrefReadApart {
	t.Helper()
	writerRefs := map[string]string{}
	for _, row := range hrefTable(t, writer.fixtures) {
		writerRefs[row.Href] = row.Ref
	}
	apart := map[string]hrefReadApart{}
	for _, row := range hrefTable(t, reader.fixtures) {
		writerRef, both := writerRefs[row.Href]
		if !both || writerRef == row.Ref {
			continue
		}
		href := hrefReadApart{
			href: row.Href, writerRef: writerRef, readerRef: row.Ref,
			writerRow: refRow(t, writer, row.Href, writerRef), readerRow: refRow(t, reader, row.Href, row.Ref),
			d10: decisionsReadings(row.Href, writerRef, row.Ref),
		}
		apart[href.writerRow], apart[href.readerRow] = href, href
	}
	return apart
}

// refRow is the refs row the reference ref names, `kind id`, as documentRefs reads it; side's href
// table maps href to ref.
func refRow(t *testing.T, side documentSide, href, ref string) string {
	t.Helper()
	parsed, ok := text.ParseRef(ref, "")
	if !ok {
		t.Fatalf("the %s's href table maps %s to %s, which names no reference", side.name, href, ref)
	}
	return parsed.Kind + " " + text.TargetID(parsed)
}

// documentReading is, for D10's href, the refs row of the reading that names the decisions
// document: the reading of a build before D10, which only that reading of the href writes.
func (href hrefReadApart) documentReading() string {
	if strings.HasPrefix(href.writerRow, "artifact ") {
		return href.writerRow
	}
	return href.readerRow
}

// otherReading is, for a refs row that is one build's reading of an href the two builds read
// apart, the row the other build reads that href as.
func (href hrefReadApart) otherReading(row string) string {
	if row == href.writerRow {
		return href.readerRow
	}
	return href.writerRow
}

// opensTypedBlock reports whether a line of body starts with the `:::` of a typed block's fence.
func opensTypedBlock(body string) bool {
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, ":::") {
			return true
		}
	}
	return false
}

// documentRefs is every refs row the document is the source of, as `kind id`, sorted.
func documentRefs(t *testing.T, builds *twoBuilds, artifactID string) []string {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), `
		select to_kind || ' ' || to_id from refs
		where from_kind = 'artifact' and from_id = $1
		order by 1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	defer rows.Close()
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the document's refs: %v", err)
	}
	return held
}

// referencesSettleWait bounds awaitReferencesSettled: the block-index tick of each build comes
// every docs.BlockIndexReconcileInterval, and the re-index a version owes takes two of them, the
// one that drains the document's scope row and the one whose batch indexes the document it owed.
const referencesSettleWait = 3*docs.BlockIndexReconcileInterval + 30*time.Second

// awaitReferencesSettled waits until the document owes no pass that rewrites its refs rows, and
// fails once referencesSettleWait passes. Every version either build writes owes the document's
// scope a re-index (refs_grammar_owed, migration 20261103205041). The block-index tick of
// whichever build takes it next drains that row by owing the document a block index (docs'
// reindexScope), and a batch of the same build indexes the document's latest version whole,
// its references with that build's own grammar (indexOwedDocument). So the rows after a write
// stand only once that queue holds no row naming the document and its block index is at its
// latest version with no store mark owed (document_block_versions). A wait of fixed length reads
// the rows before a tick that is due, or between a tick of the one build and one of the other.
func awaitReferencesSettled(t *testing.T, builds *twoBuilds, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(referencesSettleWait)
	for {
		var queued, indexed bool
		var owed string
		if err := builds.database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from refs_grammar_owed where id = $1::uuid),
			       coalesce((select b.version = (select max(v.number) from artifact_versions v where v.artifact_id = $1::uuid)
			                   and b.store_owed_at is null
			                 from document_block_versions b where b.artifact_id = $1::uuid), false),
			       coalesce((select string_agg(kind, ', ' order by kind) from refs_grammar_owed where id = $1::uuid), '')
		`, artifactID).Scan(&queued, &indexed, &owed); err != nil {
			t.Fatalf("read what the document %s owes: %v", artifactID, err)
		}
		if !queued && indexed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the document %s still owes a re-index: re-index queue rows %q, block index at its latest version with no store mark %v",
				referencesSettleWait, artifactID, owed, indexed)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// requireReferencesKeptAcrossBuilds creates the body holding every reference form of X's
// text/testdata on X, appends a paragraph on Y, and checks the refs rows after the rewrite and
// after every pass it owed has rewritten them (awaitReferencesSettled, requireRefsKept).
func requireReferencesKeptAcrossBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	_, artifactID := createDocument(t, writer.process, project, "Every reference form", referencesMarkdown(t, writer.fixtures))
	written := documentRefs(t, builds, artifactID)
	if len(written) == 0 {
		t.Fatalf("the %s wrote no refs rows for the body holding every reference form of its text/testdata", writer.name)
	}
	apart := hrefsReadApart(t, writer, reader)
	appendParagraph(t, reader.process, artifactID, "Rewritten on the "+reader.name+".")
	requireRefsKept(t, documentRefs(t, builds, artifactID), written, apart, "after the "+reader.name+" rewrote the body", writer.name, reader)
	awaitReferencesSettled(t, builds, artifactID)
	dropped := requireRefsKept(t, documentRefs(t, builds, artifactID), written, apart, "after the "+reader.name+"'s rewrite settled", writer.name, reader)
	t.Logf("%d of the %d refs rows the %s wrote survived the %s's rewrite", len(written)-len(dropped), len(written), writer.name, reader.name)
}

// requireRefsKept fails unless got holds every row of written that the reader owes it: every edge
// of a kind it extracts from the body, and, from a reader that keeps edges to the node kinds it
// does not know, those edges too. Whether it keeps them is read from its manifest: the guard that
// keeps one (refs.replaceRefs) ships with the node list this release added to the block manifest,
// so a build whose manifest names its nodes carries it. A reader from before that guard deletes
// every edge of the source it rewrites, so each edge of a kind it extracts none of is logged as
// dropped rather than failed: the protection binds from the release after the one that carries it
// (see "Two builds at once" in the README), and until a build that knows those kinds rewrites the
// body the edges are gone. The kinds the reader extracts are the kinds its rewrite left a row of.
// It may add rows, but only of a kind that written holds no row of: a kind it extracts and the
// writer does not, such as `block` from a `#b-` link when the writer is a build before the block
// graph. A row it adds of a kind the writer wrote means the two builds read one reference form as
// different targets, unless that row is one build's reading of an href the two builds read apart
// (apart, hrefsReadApart): that is D10's grammar change, which is logged. D10's href stands in the
// rows by the reading of whichever build last indexed the body - the writer's own write, the
// reader's rewrite, or the block-index pass of either build that re-indexes it after each - so the
// row of written that is its document reading (`artifact KEY/decisions-md`, which only a build
// before D10 writes from it, documentReading) is kept while the issue reading's row is held. No
// other row of written is kept by another row. It returns the edges the reader dropped.
func requireRefsKept(t *testing.T, got, written []string, apart map[string]hrefReadApart, when, writer string, reader documentSide) []string {
	t.Helper()
	keepsUnknownKinds := len(reader.manifest.Nodes) > 0
	kinds := map[string]bool{}
	for _, row := range written {
		kind, _, _ := strings.Cut(row, " ")
		kinds[kind] = true
	}
	extracted := map[string]bool{}
	for _, row := range got {
		kind, _, _ := strings.Cut(row, " ")
		extracted[kind] = true
	}
	regrammar := func(row string, href hrefReadApart) string {
		return fmt.Sprintf("\t%s: %s reads as %s in the %s and as %s in the %s\n",
			row, href.href, href.writerRef, writer, href.readerRef, reader.name)
	}
	var lost, dropped, added, regrammared []string
	for _, row := range written {
		if slices.Contains(got, row) {
			continue
		}
		if href, ok := apart[row]; ok && href.d10 && row == href.documentReading() && slices.Contains(got, href.otherReading(row)) {
			regrammared = append(regrammared, regrammar(href.otherReading(row), href))
			continue
		}
		if kind, _, _ := strings.Cut(row, " "); !keepsUnknownKinds && !extracted[kind] {
			dropped = append(dropped, row)
			continue
		}
		lost = append(lost, row)
	}
	for _, row := range got {
		if kind, _, _ := strings.Cut(row, " "); !kinds[kind] || slices.Contains(written, row) {
			continue
		}
		if href, ok := apart[row]; ok {
			if !slices.Contains(regrammared, regrammar(row, href)) {
				regrammared = append(regrammared, regrammar(row, href))
			}
			continue
		}
		added = append(added, row)
	}
	if len(dropped) > 0 {
		t.Logf("%s, the %s dropped %d of the %d edges the %s wrote, each to a node kind it extracts none of and keeps no edge to:\n%s",
			when, reader.name, len(dropped), len(written), writer, refRowsText(dropped))
	}
	if len(regrammared) > 0 {
		t.Logf("%s, %d rows are the reading of an href the two builds read apart by D10's grammar change, other than the one written holds:\n%s",
			when, len(regrammared), strings.Join(regrammared, ""))
	}
	if len(lost) > 0 || len(added) > 0 {
		t.Fatalf("%s, the body's refs rows lack %d of the %d the %s wrote\n%s\nand hold %d rows of kinds the %s wrote that it did not write\n%s\nall rows now:\n%s",
			when, len(lost), len(written), writer, refRowsText(lost), len(added), writer, refRowsText(added), refRowsText(got))
	}
	return dropped
}

// refRowsText is rows one per line, each row longer than 120 bytes cut to its first 120 and its
// length: the text tables hold target ids of about 2 KB.
func refRowsText(rows []string) string {
	var text strings.Builder
	for _, row := range rows {
		if len(row) > 120 {
			row = fmt.Sprintf("%s… (%d bytes)", row[:120], len(row))
		}
		text.WriteString("\t" + row + "\n")
	}
	return text.String()
}
