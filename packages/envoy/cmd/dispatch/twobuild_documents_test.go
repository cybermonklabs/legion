//go:build twobuild

// The stored-document half of the two-build suite: each build reading, editing and keeping the
// references of the other's documents, over the API and the document socket
// (twobuild_harness_test.go holds the harness both halves start their builds with).

package main

// The two-build suite's stored-document half reads this tree's side of what each build holds
// (documentSides, tree ../../internal/dispatch at :55); the previous build's side is the copy
// packages/envoy/scripts/build-dispatch-build.sh writes outside the checkout.
//ci:reads packages/envoy/internal/dispatch/pmdoc/schema/blocks.json documentSides readManifest (os.ReadFile) of the current build's block manifest
//ci:reads packages/envoy/internal/dispatch/pmdoc/testdata/corpus/*.md documentSides the current build's corpus: corpusFiles os.ReadDir, and requireCorpusCrossesBuilds os.ReadFile of each .md file it lists
//ci:reads packages/envoy/internal/dispatch/text/testdata/dispatch-text-references.json documentSides the current build's reference fixtures, read by referencesMarkdown os.ReadFile
//ci:reads packages/envoy/internal/dispatch/text/testdata/dispatch-href-references.json documentSides the current build's reference fixtures, read by referencesMarkdown os.ReadFile

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
	"os/exec"
	"path/filepath"
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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/heavybodies"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// ───── stored documents ─────────────────────────────────────────────────────

// referenceServerURL is DISPATCH_SERVER_URL for the document rows: the origin the reference
// fixtures' dashboard links name (text/testdata), so both builds read those links as references.
const referenceServerURL = "https://dispatch.test"

// referenceEnv is what both builds' processes start with in the document rows, and a restart of
// either takes again: DISPATCH_SERVER_URL at referenceServerURL.
func referenceEnv() map[string]string {
	return map[string]string{"DISPATCH_SERVER_URL": referenceServerURL}
}

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
	// Parents are the typed blocks this type stands in, at the end of that block's own content; a
	// type that names none is a block of its own wherever a block stands.
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
// X's own corpus: a document created on X answers /text on Y and takes an append edit there, and X
// then renders it as its first render with the appended paragraph after it; on Y, a document socket
// opened with X's page parameters is admitted writable and stays open. A version X wrote holding
// every typed block of X's manifest is approved, the answer to its decision block makes Y write the
// next version, and the approval is carried to it. A body holding every reference form of X's
// text/testdata that Y's own table holds too keeps its refs rows after Y rewrites it, but for the
// edges a Y from before the unknown-kind guard drops, which it logs (requireRefsKept); a body
// holding the forms only X's table holds has every row X wrote back, its blocks' edges included,
// within two block-index intervals of Y's rewrite (requireWriterOnlyReferencesBack). Where Y reads
// no tab address and X does, each trigger that owes X a document Y re-indexed is shown needed on
// its own: a version Y writes, Y's rebuild-refs' delete from refs, and Y's block index's delete from
// block_refs (requireEachTriggerOwesTheDocument), a row that stops and restarts both processes and
// so runs last.
func TestTwoBuildsReadAndEditEachOthersDocuments(t *testing.T) {
	builds := newTwoBuilds(t)
	env := referenceEnv()
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

func requireCorpusCrossesBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	fleet := builds.fleet(t)
	var sockets []*documentSocket
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
// declare, each with its id and every attribute a writer sets at its default. A decision block
// (the ask type) is given two choices, First and Second. A type only the writer's manifest
// declares is left out: the reader could not read such a document at all, which is the point of
// the manifest's own staging tests and not of this row, which is about what both builds read.
//
// Where each block stands is the manifest's to say. A type that names no parents is a block of
// the document's own, in manifest order. A type that names parents, as the reading names the
// decision and the ask, stands only at the end of a block of one of them, so it is written there:
// in the block of the first of its parents the document writes, after that block's own content,
// in manifest order among that parent's other such types. A parent's fence is one colon longer
// than the longest fence inside it, which is how markdown closes the inner block before the outer
// one. A type none of whose parents the document writes fails the test, as does one the document
// reaches through no block of its own. So a document of types that name no parents, as every
// manifest before the reading's is, is each type's three-colon block in manifest order, under the
// document's heading.
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
		t.Fatalf("the %s's manifest declares no ask block, the decision block an approval is carried by", writer.name)
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
			t.Fatalf("the answer on the %s to the decision block of a version the %s wrote and %s approved (version %d) carried no approval within 30s; reviews: %+v\n%s:\n%s",
				reader.name, writer.name, dispatchTestLogin, approval.Version, reviews, reader.name, reader.process.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForBlockAsk waits for the ask settlement indexed from the decision block blockID of the
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

// referenceTables is a build's two reference tables of text/testdata: the bodies of its text table
// and the links of its href table.
type referenceTables struct {
	texts []struct {
		Body   string `json:"body"`
		Origin string `json:"origin"`
	}
	hrefs []struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}
}

func readReferenceTables(t *testing.T, fixtures string) referenceTables {
	t.Helper()
	var tables referenceTables
	for name, into := range map[string]any{"dispatch-text-references.json": &tables.texts, "dispatch-href-references.json": &tables.hrefs} {
		raw, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	return tables
}

// referencesMarkdown is two bodies holding every reference form of the writer's text/testdata that
// stands alone as a document body: each body of its text table, and each link of its href table as
// the dashboard writes it and as its dispatch:// form, a paragraph each. shared holds the forms the
// reader's own tables hold too - a text row whose body the reader's text table holds, a link whose
// href its href table holds - and writerOnly the rest, the forms only the writer reads, or nothing
// where the reader's tables hold every one. Two kinds of text row are left out, each logged:
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
func referencesMarkdown(t *testing.T, writer, reader string) (shared, writerOnly string) {
	t.Helper()
	written, read := readReferenceTables(t, writer), readReferenceTables(t, reader)
	bodies := map[string]bool{}
	for _, row := range read.texts {
		bodies[row.Body] = true
	}
	hrefs := map[string]bool{}
	for _, row := range read.hrefs {
		hrefs[row.Href] = true
	}
	both := []string{"# Every reference form"}
	only := []string{"# Every reference form only the writer reads"}
	for _, row := range written.texts {
		switch {
		case row.Origin != "" && row.Origin != referenceServerURL:
			t.Logf("left out the text row %q: it names the origin %s", row.Body, row.Origin)
		case opensTypedBlock(row.Body):
			t.Logf("left out the text row %q: it opens a typed block it does not close", row.Body)
		case nestsPastTheBound(row.Body):
			t.Logf("left out the text row %q: it nests blocks past the bound a document is held to", row.Body)
		case bodies[row.Body]:
			both = append(both, row.Body)
		default:
			only = append(only, row.Body)
		}
	}
	for _, row := range written.hrefs {
		if hrefs[row.Href] {
			both = append(both, referenceServerURL+row.Href, row.Ref)
		} else {
			only = append(only, referenceServerURL+row.Href, row.Ref)
		}
	}
	shared = strings.Join(both, "\n\n") + "\n"
	if len(only) > 1 {
		writerOnly = strings.Join(only, "\n\n") + "\n"
	}
	return shared, writerOnly
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

// nestsPastTheBound reports whether a line of body opens more blocks inside one another than a
// document may hold (heavybodies.MaxNesting, pmdoc's own bound): a quote marker opens one, and a
// list marker opens a list and its first item. The server refuses such a document whole
// (`opens a block inside 100 blocks`), where a comment, an ask and a message store the row's text
// as it is, so the table holds rows that nest past the bound on purpose.
func nestsPastTheBound(body string) bool {
	for line := range strings.Lines(body) {
		opened := 0
		for rest := strings.TrimLeft(line, " \t"); ; rest = strings.TrimLeft(rest, " \t") {
			if quoted, ok := strings.CutPrefix(rest, ">"); ok {
				opened++
				rest = quoted
				continue
			}
			marker := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
			switch {
			case marker > 0 && marker <= 9 && strings.ContainsRune(".)", rune(rest[marker])):
				marker++
			case marker == 0 && strings.ContainsRune("-*+", rune(rest[0])):
				marker = 1
			default:
				marker = -1
			}
			if marker < 0 || marker >= len(rest) || (rest[marker] != ' ' && rest[marker] != '\t') {
				break
			}
			opened += 2
			rest = rest[marker:]
		}
		if opened > heavybodies.MaxNesting {
			return true
		}
	}
	return false
}

// documentRefs is every refs row the document is the source of, as `kind id`, sorted.
func documentRefs(t *testing.T, builds *twoBuilds, artifactID string) []string {
	t.Helper()
	return queryRows(t, builds, "read the document's refs", `
		select to_kind || ' ' || to_id from refs
		where from_kind = 'artifact' and from_id = $1
		order by 1
	`, artifactID)
}

// documentBlockEdges is the target of every edge the document's blocks hold (block_refs), as
// `kind id`, once per edge, sorted. A block's id is left out, so the list reads the same however a
// build numbers the blocks of one text, and still counts an edge one block lost while another
// kept its own to the same target.
func documentBlockEdges(t *testing.T, builds *twoBuilds, artifactID string) []string {
	t.Helper()
	return queryRows(t, builds, "read the document's block edges", `
		select to_kind || ' ' || to_id from block_refs
		where artifact_id = $1::uuid
		order by 1
	`, artifactID)
}

// queryRows is the one text column query answers for args, in the order it answers them.
func queryRows(t *testing.T, builds *twoBuilds, what, query string, args ...any) []string {
	t.Helper()
	rows, err := builds.database.Pool.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer rows.Close()
	read, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return read
}

// referencesSettle is how long the references row waits after Y's edit before it reads the refs
// rows again: past the default two-second settle delay, so a settlement that rewrites the document
// after the edit has rewritten it too.
const referencesSettle = 4 * time.Second

// requireReferencesKeptAcrossBuilds creates the reference forms of X's text/testdata on X as two
// bodies, split by Y's own tables (referencesMarkdown), appends a paragraph to each on Y, and
// checks each body's refs rows:
//   - the forms Y's tables hold too, after the rewrite and after it settled, as this row always
//     has (requireRefsKept);
//   - the forms only X's table holds, every row X wrote, its blocks' edges included, back within
//     two block-index intervals of Y's rewrite (requireWriterOnlyReferencesBack), where Y may drop
//     the rows of those forms on its rewrite. When Y's tables hold every form of X's, as the
//     current build's hold the previous build's, that body is empty and skipped.
//
// Then, where Y is the previous build and reads no tab address while X does, it shows each trigger
// that owes X a document Y re-indexed needed on its own (requireEachTriggerOwesTheDocument).
//
// This changes main's row, which held one body of every form to requireRefsKept. No old binary can
// keep an edge of a form it cannot parse: its rewrite re-extracts the body and deletes every edge
// of a kind it knows that it did not find, so the current build's new forms of the issue and
// artifact kinds the previous build knows were 8 of 102 rows lost on CI. The guarantee for the
// forms the reader reads is unchanged, and a new one is added for the rest: the writer's forms
// drain (docs.Service.RunRefsFormsReconciliation) indexes the text again and writes them back,
// read here while both builds' loops run, with no hook a real deploy lacks.
func requireReferencesKeptAcrossBuilds(t *testing.T, builds *twoBuilds, writer, reader documentSide, project string) {
	shared, writerOnly := referencesMarkdown(t, writer.fixtures, reader.fixtures)
	key, artifactID := createDocument(t, writer.process, project, "Every reference form", shared)
	written := documentRefs(t, builds, artifactID)
	if len(written) == 0 {
		t.Fatalf("the %s wrote no refs rows for the body holding every reference form of its text/testdata", writer.name)
	}
	appendParagraph(t, reader.process, artifactID, "Rewritten on the "+reader.name+".")
	requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+" rewrote the body", writer.name, reader)
	time.Sleep(referencesSettle)
	dropped := requireRefsKept(t, documentRefs(t, builds, artifactID), written, "after the "+reader.name+"'s rewrite settled", writer.name, reader)
	t.Logf("%d of the %d refs rows the %s wrote survived the %s's rewrite", len(written)-len(dropped), len(written), writer.name, reader.name)
	if writerOnly == "" {
		t.Logf("the %s's tables hold every reference form of the %s's: no body of forms only the %s reads", reader.name, writer.name, writer.name)
		return
	}
	requireWriterOnlyReferencesBack(t, builds, writer, reader, project, writerOnly)
	switch {
	case reader.process != builds.previous():
		t.Logf("the %s is not the previous build, whose rebuild-refs the trigger rows run", reader.name)
	case !readsTabAddresses(t, writer.fixtures) || readsTabAddresses(t, reader.fixtures):
		t.Logf("the tab addresses are not a form only the %s reads: no trigger row", writer.name)
	default:
		requireEachTriggerOwesTheDocument(t, builds, writer, reader, project, key)
	}
}

// writerOnlyReferencesBack is how long after the reader's rewrite every row of the forms only the
// writer reads must be back: two intervals of the writer's forms drain, the wait between its
// passes once no batch is full, so a pass that listed the queue just before the rewrite owed the
// document is followed by one that drains it.
const writerOnlyReferencesBack = 2 * docs.BlockIndexReconcileInterval

// requireWriterOnlyReferencesBack creates markdown, the forms only the writer's table holds, on the
// writer, appends a paragraph on the reader, and fails unless the document's edges are the ones
// the writer wrote again within writerOnlyReferencesBack of that rewrite: every refs row back, and
// no row of a kind the writer wrote that it did not write, which would mean the writer's index of
// the reader's text read one form as another target, and its blocks' edges as the writer wrote
// them. Until then the reader's rewrite may have dropped rows of the kinds it extracts, which is
// logged; the first read is taken after referencesSettle, so a settlement that rewrites the
// document after the edit has dropped them too.
func requireWriterOnlyReferencesBack(t *testing.T, builds *twoBuilds, writer, reader documentSide, project, markdown string) {
	_, artifactID := createDocument(t, writer.process, project, "Every reference form only the writer reads", markdown)
	written, writtenBlocks := documentRefs(t, builds, artifactID), documentBlockEdges(t, builds, artifactID)
	rewritten := time.Now()
	appendParagraph(t, reader.process, artifactID, "Rewritten on the "+reader.name+".")
	if lost, _ := refRowsMoved(documentRefs(t, builds, artifactID), written); len(lost) > 0 {
		t.Logf("after the %s rewrote the body of the forms only the %s reads, it dropped %d of the %d rows the %s wrote:\n%s",
			reader.name, writer.name, len(lost), len(written), writer.name, refRowsText(lost))
	}
	time.Sleep(referencesSettle)
	deadline := rewritten.Add(writerOnlyReferencesBack)
	for {
		got, blocks := documentRefs(t, builds, artifactID), documentBlockEdges(t, builds, artifactID)
		lost, added := refRowsMoved(got, written)
		if len(lost) == 0 && len(added) == 0 && slices.Equal(blocks, writtenBlocks) {
			t.Logf("all %d refs rows and %d block edges the %s wrote of the forms only it reads were back %s after the %s's rewrite",
				len(written), len(writtenBlocks), writer.name, time.Since(rewritten).Round(time.Second), reader.name)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s after the %s rewrote the body of the forms only the %s reads, its refs rows lack %d of the %d the %s wrote\n%s\nand hold %d rows of kinds the %s wrote that it did not write\n%s\nall rows now:\n%s\nits blocks hold the edges\n%s\nwhere the %s wrote\n%s",
				writerOnlyReferencesBack, reader.name, writer.name, len(lost), len(written), writer.name, refRowsText(lost),
				len(added), writer.name, refRowsText(added), refRowsText(got), refRowsText(blocks), writer.name, refRowsText(writtenBlocks))
		}
		time.Sleep(time.Second)
	}
}

// readsTabAddresses reports whether a build's text table holds the row naming an issue's plan tab,
// the form the trigger rows write (formsOnlyMarkdown): a build whose table holds it reads it.
func readsTabAddresses(t *testing.T, fixtures string) bool {
	t.Helper()
	for _, row := range readReferenceTables(t, fixtures).texts {
		if strings.Contains(row.Body, "dispatch://CORE-45/plan") {
			return true
		}
	}
	return false
}

// formsOnlyMarkdown is a document whose every reference is a form a build that reads no tab
// address cannot read: issue key's plan tab and its verification tab as dispatch:// addresses, a
// paragraph each. Such a build writes no edge for it, and its re-index of a text the writer indexed
// deletes every edge the writer wrote.
func formsOnlyMarkdown(key string) string {
	return "# Forms only the writer reads\n\nThe plan: dispatch://" + key + "/plan.\n\nThe verification: dispatch://" + key + "/verification.\n"
}

// issueTwiceMarkdown names issue key twice, as a bare key in one paragraph and as its plan tab in
// another. A build that reads no tab address keeps the document's refs row to the issue, which the
// bare key still finds, and its block index drops the second paragraph's edge alone.
func issueTwiceMarkdown(key string) string {
	return "# The issue twice\n\nThe issue is " + key + ".\n\nIts plan: dispatch://" + key + "/plan.\n"
}

// documentEdgesWritten is a document's refs rows and its blocks' edges as the writer indexed them.
type documentEdgesWritten struct {
	id           string
	refs, blocks []string
}

// writtenEdges reads document artifactID's edges as they stand, which the caller has just had the
// writer index.
func writtenEdges(t *testing.T, builds *twoBuilds, artifactID string) documentEdgesWritten {
	t.Helper()
	written := documentEdgesWritten{id: artifactID, refs: documentRefs(t, builds, artifactID), blocks: documentBlockEdges(t, builds, artifactID)}
	if len(written.refs) == 0 || len(written.blocks) == 0 {
		t.Fatalf("the writer indexed document %s to refs rows %v and block edges %v, want both", artifactID, written.refs, written.blocks)
	}
	return written
}

// back reports whether the document's edges are the ones the writer wrote.
func (written documentEdgesWritten) back(t *testing.T, builds *twoBuilds) bool {
	t.Helper()
	return slices.Equal(documentRefs(t, builds, written.id), written.refs) &&
		slices.Equal(documentBlockEdges(t, builds, written.id), written.blocks)
}

// formsOwed reports whether refs_forms_owed holds document artifactID.
func formsOwed(t *testing.T, builds *twoBuilds, artifactID string) bool {
	t.Helper()
	var owed bool
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from refs_forms_owed where kind = 'artifact' and id = $1::uuid)
	`, artifactID).Scan(&owed); err != nil {
		t.Fatalf("read whether %s is owed: %v", artifactID, err)
	}
	return owed
}

// triggerRowsBack is how long the trigger rows give the writer to write a document's edges back:
// the rewrite row's two drain intervals and two more, since after the previous build's rebuild-refs
// the writer's first passes take every text that rebuild re-indexed and every document it marked
// owed a block index.
const triggerRowsBack = 4 * docs.BlockIndexReconcileInterval

// requireEachTriggerOwesTheDocument shows each trigger that owes the writer (X, this build) a
// document the reader (Y, the previous build, which reads no tab address) re-indexed needed on its
// own, each through a re-index the other two do not see:
//   - A version Y writes (migration 20261117025904): Y creates a document holding forms only X
//     reads, so no edge of them exists for Y to delete, and only its version owes it. X's edges for
//     that text, its own and its blocks', must appear within triggerRowsBack.
//   - Y's rebuild-refs' delete from refs (20261117025906): with both processes stopped, so nothing
//     else re-indexes or drains, Y's rebuild-refs re-indexes every source's references alone,
//     writing no version and no block edge, and the document holding forms only X reads must be
//     owed when it returns.
//   - Y's block index's delete from block_refs (20261117025907): a document naming one issue as a
//     bare key in one block and as its plan tab in another keeps its refs row through that
//     rebuild, so it is not owed, and Y alone, started again, indexes the blocks rebuild-refs
//     marked owed with no version, dropping the second block's edge; the document must then be
//     owed.
//
// X, started again, must then write both documents' edges back within triggerRowsBack. A run with
// any one of the three triggers removed fails its own step, where today's rewrite row passes with
// either of the first two removed. The rows stop and restart both builds' processes, and so run
// last.
func requireEachTriggerOwesTheDocument(t *testing.T, builds *twoBuilds, writer, reader documentSide, project, key string) {
	_, twin := createDocument(t, writer.process, project, "Forms only the writer reads, written by it", formsOnlyMarkdown(key))
	onlyForms := writtenEdges(t, builds, twin)
	_, issueTwice := createDocument(t, writer.process, project, "The issue twice", issueTwiceMarkdown(key))
	twice := writtenEdges(t, builds, issueTwice)

	// A version alone: the reader creates the same text the writer indexed in twin.
	_, readerWrote := createDocument(t, reader.process, project, "Forms only the writer reads, written by the reader", formsOnlyMarkdown(key))
	versionOwed := documentEdgesWritten{id: readerWrote, refs: onlyForms.refs, blocks: onlyForms.blocks}
	waitUntil(t, triggerRowsBack, "the "+writer.name+" to write its edges of the forms only it reads for the document the "+reader.name+" created holding them, owed by its version alone",
		func() bool { return versionOwed.back(t, builds) })

	// Nothing runs from here until the reader starts again: the settlements of the writer's two
	// documents have written their versions, and the reader holds no room of them.
	time.Sleep(referencesSettle)
	waitUntil(t, writerOnlyReferencesBack, "the "+writer.name+"'s documents to owe nothing",
		func() bool { return !formsOwed(t, builds, twin) && !formsOwed(t, builds, issueTwice) })
	stop(t, reader.process)
	stop(t, writer.process)
	if formsOwed(t, builds, twin) || formsOwed(t, builds, issueTwice) {
		t.Fatalf("a document the %s wrote was owed once both processes stopped", writer.name)
	}

	output := builds.rebuildRefsOnPrevious(t)
	t.Logf("the %s's rebuild-refs: %s", reader.name, strings.TrimSpace(output))
	if !formsOwed(t, builds, twin) {
		t.Fatalf("after the %s's rebuild-refs deleted the edges of the forms only the %s reads, the document is not owed: its refs rows are %v",
			reader.name, writer.name, documentRefs(t, builds, twin))
	}
	if formsOwed(t, builds, issueTwice) {
		t.Fatalf("after the %s's rebuild-refs the document naming its issue twice is owed, though its refs rows %v keep the edge the bare key finds",
			reader.name, documentRefs(t, builds, issueTwice))
	}

	reader.process = builds.startPrevious(t, referenceEnv())
	waitUntil(t, triggerRowsBack, "the "+reader.name+"'s block index to index the document naming its issue twice", func() bool {
		var version *int
		if err := builds.database.Pool.QueryRow(context.Background(),
			`select version from document_block_versions where artifact_id = $1::uuid`, issueTwice).Scan(&version); err != nil {
			t.Fatalf("read the block index of %s: %v", issueTwice, err)
		}
		return version != nil
	})
	if !formsOwed(t, builds, issueTwice) {
		t.Fatalf("after the %s's block index dropped a block's edge of the document naming its issue twice, the document is not owed: its blocks hold %v",
			reader.name, documentBlockEdges(t, builds, issueTwice))
	}

	writer.process = builds.startCurrent(t, referenceEnv())
	waitUntil(t, triggerRowsBack, "the "+writer.name+" to write back the edges of the documents the "+reader.name+"'s rebuild-refs and block index re-indexed",
		func() bool { return onlyForms.back(t, builds) && twice.back(t, builds) })
}

// rebuildRefsOnPrevious runs the previous build's `envoy-dispatch rebuild-refs` against the pair's
// database, as an operator runs it during a roll, and returns what it printed.
func (b *twoBuilds) rebuildRefsOnPrevious(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, b.build.path("envoy-dispatch"), "rebuild-refs")
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + b.databaseURL(t), "DISPATCH_SERVER_URL=" + referenceServerURL,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the previous build's rebuild-refs: %v\n%s", err, output)
	}
	return string(output)
}

// refRowsMoved is what got lacks of written, and what it holds of a kind written holds a row of
// that written does not.
func refRowsMoved(got, written []string) (lost, added []string) {
	kinds := map[string]bool{}
	for _, row := range written {
		kind, _, _ := strings.Cut(row, " ")
		kinds[kind] = true
		if !slices.Contains(got, row) {
			lost = append(lost, row)
		}
	}
	for _, row := range got {
		if kind, _, _ := strings.Cut(row, " "); kinds[kind] && !slices.Contains(written, row) {
			added = append(added, row)
		}
	}
	return lost, added
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
// different targets. It returns the edges the reader dropped.
func requireRefsKept(t *testing.T, got, written []string, when, writer string, reader documentSide) []string {
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
	var lost, dropped, added []string
	for _, row := range written {
		if slices.Contains(got, row) {
			continue
		}
		if kind, _, _ := strings.Cut(row, " "); !keepsUnknownKinds && !extracted[kind] {
			dropped = append(dropped, row)
			continue
		}
		lost = append(lost, row)
	}
	for _, row := range got {
		if kind, _, _ := strings.Cut(row, " "); kinds[kind] && !slices.Contains(written, row) {
			added = append(added, row)
		}
	}
	if len(dropped) > 0 {
		t.Logf("%s, the %s dropped %d of the %d edges the %s wrote, each to a node kind it extracts none of and keeps no edge to:\n%s",
			when, reader.name, len(dropped), len(written), writer, refRowsText(dropped))
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
