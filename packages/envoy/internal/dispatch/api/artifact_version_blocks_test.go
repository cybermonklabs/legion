package api

//ci:reads packages/contracts/fixtures/dispatch-version-blocks/* TestAVersionsBlocksCarryTheParentOfATaskBlock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// versionBlocksAnswer is what a version read with ?blocks=true answers, as a reader of the plan
// at a version reads it: the version's markdown and the blocks whose byte ranges index it.
type versionBlocksAnswer struct {
	Markdown string                 `json:"markdown"`
	Number   int                    `json:"number"`
	SHA256   *string                `json:"sha256"`
	Size     *int                   `json:"size"`
	Blocks   *[]model.ArtifactBlock `json:"blocks"`
}

// A version read with ?blocks=true carries the blocks of that version's own text, not the live
// document's: each typed block under the id its opening line writes, with its attributes and the
// byte range of its rendering in the markdown the same answer carries. Two versions of a plan
// task therefore compare by the id of its block, the comparison a plan's later version rests on.
func TestAVersionReadCarriesThatVersionsBlocks(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)

	plan := createIssueDocument(t, handler, issue.Key, "plan.md", ":::plan_task{#a steps=\"implementer reviewer\"}\nDo the change.\n:::\n")
	editToVersion(t, handler, plan.ID, 2, map[string]any{
		"op": "replace", "find": "Do the change.", "with": ":::plan_task{#a steps=\"planner implementer\"}\nDo it again.\n:::",
	})
	v1 := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true")
	v2 := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/2?blocks=true")
	assertPlanTask(t, v1, "a", "implementer reviewer", "Do the change.")
	assertPlanTask(t, v2, "a", "planner implementer", "Do it again.")
	bySlug := readVersionBlocks(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts/plan-md/versions/1?blocks=true")
	assertPlanTask(t, bySlug, "a", "implementer reviewer", "Do the change.")
	// Only the typed block is listed: the paragraph inside it writes no id into the stored text, so
	// a read of the version would give it a new id each time.
	for _, answer := range []versionBlocksAnswer{v1, v2} {
		if len(*answer.Blocks) != 1 {
			t.Fatalf("version %d lists %+v, want its one plan task alone", answer.Number, *answer.Blocks)
		}
	}

	// A plan task written with no id gets one when it is first stored, and keeps it in each later
	// version: the stored markdown names it, so every read of every version finds the same one.
	unnamed := createIssueDocument(t, handler, issue.Key, "unnamed.md", ":::plan_task{steps=\"implementer\"}\nX\n:::\n")
	first := readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/1?blocks=true")
	minted := onlyPlanTask(t, first).ID
	if minted == "" {
		t.Fatalf("version 1 of a plan task written with no id lists it with none: %+v", first)
	}
	if again := onlyPlanTask(t, readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/1?blocks=true")).ID; again != minted {
		t.Fatalf("version 1 read twice names its plan task %q, then %q", minted, again)
	}
	editToVersion(t, handler, unnamed.ID, 2, map[string]any{
		"op": "retype", "block": minted, "type": "plan_task", "attributes": map[string]any{"steps": "implementer reviewer"},
	})
	second := readVersionBlocks(t, handler, "/api/v1/artifacts/"+unnamed.ID+"/versions/2?blocks=true")
	if got := onlyPlanTask(t, second).ID; got != minted {
		t.Fatalf("a task block changed its id between versions: %q in version 1, %q in version 2\nversion 2's markdown: %q", minted, got, second.Markdown)
	}
	assertPlanTask(t, first, minted, "implementer", "X")
	assertPlanTask(t, second, minted, "implementer reviewer", "X")

	// Control: a read that asks for no blocks has no "blocks" key, whatever else it sends, and the
	// live text's blocks are version 2's, so the version reads above did not read the live tree.
	for _, query := range []string{"", "?blocks=false", "?blocks=1", "?blocks="} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1"+query, nil, "alice")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"blocks"`) {
			t.Fatalf("GET version 1%s: status=%d body=%s, want the version with no blocks key", query, response.Code, response.Body.String())
		}
	}
	live := decodeBody[versionBlocksAnswer](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/text?blocks=true", nil, "alice"))
	assertPlanTask(t, live, "a", "planner implementer", "Do it again.")
}

// The blocks of a version index the markdown its answer carries. A version an earlier renderer
// stored can render otherwise today; its answer then carries the rendering the blocks were read
// from, and the digest and size of that text, so a reader slicing the markdown at a block's range
// reads that block.
func TestAVersionsBlocksIndexTheMarkdownItsAnswerCarries(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createArtifactIssue(t, handler)
	plan := createIssueDocument(t, handler, issue.Key, "plan.md", "Intro.\n\n:::plan_task{#a steps=\"implementer\"}\nBody.\n:::\n")
	// The stored text as a renderer that kept the blank lines between blocks would have written it.
	stored := "Intro.\n\n\n\n:::plan_task{#a steps=\"implementer\"}\nBody.\n:::\n"
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1`, plan.ID, stored); err != nil {
		t.Fatalf("store version 1's older rendering: %v", err)
	}
	plain := decodeBody[versionBlocksAnswer](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1", nil, "alice"))
	if plain.Markdown != stored {
		t.Fatalf("version 1 without blocks = %q, want the stored %q", plain.Markdown, stored)
	}
	answer := readVersionBlocks(t, handler, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true")
	if answer.Markdown == stored {
		t.Fatalf("version 1 with blocks answers the stored %q, which no block indexes", stored)
	}
	assertPlanTask(t, answer, "a", "implementer", "Body.")
	digest := sha256.Sum256([]byte(answer.Markdown))
	if answer.SHA256 == nil || *answer.SHA256 != hex.EncodeToString(digest[:]) || answer.Size == nil || *answer.Size != len(answer.Markdown) {
		t.Fatalf("version 1 with blocks: sha256=%v size=%v, want those of the markdown it carries (%q)", answer.SHA256, answer.Size, answer.Markdown)
	}
}

// createIssueDocument uploads a markdown document onto an issue as version 1.
func createIssueDocument(t *testing.T, handler http.Handler, issueKey, name, content string) model.Artifact {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]string{
		"name": name, "content": content,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", name, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
}

// editToVersion applies one edit operation to a document and requires that it wrote version.
func editToVersion(t *testing.T, handler http.Handler, artifactID string, version int, op map[string]any) {
	t.Helper()
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]any{op},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit %+v: status=%d body=%s", op, edited.Code, edited.Body.String())
	}
	written := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(version), nil, "alice")
	if written.Code != http.StatusOK {
		t.Fatalf("the edit %+v wrote no version %d: status=%d body=%s", op, version, written.Code, written.Body.String())
	}
}

// readVersionBlocks reads a version with its blocks and requires the key.
func readVersionBlocks(t *testing.T, handler http.Handler, target string) versionBlocksAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	answer := decodeBody[versionBlocksAnswer](t, response)
	if answer.Blocks == nil {
		t.Fatalf("GET %s answers no blocks key: %s", target, response.Body.String())
	}
	return answer
}

// onlyPlanTask is the one plan_task block an answer lists.
func onlyPlanTask(t *testing.T, answer versionBlocksAnswer) model.ArtifactBlock {
	t.Helper()
	var found []model.ArtifactBlock
	for _, block := range *answer.Blocks {
		if block.Type == "plan_task" {
			found = append(found, block)
		}
	}
	if len(found) != 1 {
		t.Fatalf("blocks %+v hold %d plan tasks, want one", *answer.Blocks, len(found))
	}
	return found[0]
}

// assertPlanTask requires the answer to list the plan_task block id with that steps attribute,
// its range to be its rendering in the answer's markdown (the opening line, the body, the closing
// fence), and its body to be body.
func assertPlanTask(t *testing.T, answer versionBlocksAnswer, id, steps, body string) {
	t.Helper()
	for _, block := range *answer.Blocks {
		if block.ID != id {
			continue
		}
		if block.Type != "plan_task" {
			t.Fatalf("block %s is a %s, want a plan_task", id, block.Type)
		}
		attributes := map[string]string{}
		for _, attribute := range block.Attributes {
			attributes[attribute.Name] = attribute.Value
		}
		if attributes["steps"] != steps {
			t.Fatalf("block %s carries attributes %+v, want steps %q", id, block.Attributes, steps)
		}
		if block.From < 0 || block.To > len(answer.Markdown) || block.To < block.From {
			t.Fatalf("block %+v does not index the markdown it came with (%q)", block, answer.Markdown)
		}
		rendering := answer.Markdown[block.From:block.To]
		opening, rest, cut := strings.Cut(rendering, "\n")
		closing := strings.LastIndex(rest, "\n")
		if !cut || !strings.HasPrefix(opening, ":::plan_task{") || closing < 0 || rest[closing+1:] != ":::" {
			t.Fatalf("block %s spans %q, want its opening line, its body and its closing fence", id, rendering)
		}
		if got := rest[:closing]; got != body {
			t.Fatalf("block %s's body = %q, want %q", id, got, body)
		}
		return
	}
	t.Fatalf("no block %s among %+v (markdown %q)", id, *answer.Blocks, answer.Markdown)
}

// versionBlocksFixtures holds, from this package's directory, the two answers
// TestAVersionsBlocksCarryTheParentOfATaskBlock commits, which -update (updateFixtures) rewrites:
// a version read that lists each task block's parent, and an older build's refusal of it. Nothing
// reads them yet; the daemon's Dispatch client of cybermonklabs/legion#5204 is to read them.
var versionBlocksFixtures = filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "dispatch-version-blocks")

// A plan's task blocks stand under their task by parent, which one release reads and the next one
// creates. N is the version that first reads parent, read off the manifest, and each build here is
// the embedded manifest as of its version (pmdoc.ManifestAsOfForTest), so the test holds whether
// this build is N or later. Three Dispatch processes share one database, one after another, each
// with a document service of its own, so no service runs while another manifest is installed:
// pmdoc's readers do not lock the manifest. The build at N+1 writes plan.md of ACME-1, default and
// t1, t2 and t3 under it. The build at N reads that version and lists each task block with its
// parent. It writes a later version of the plan, a fourth block under default included, since the
// plan holds parent already. It refuses a version that puts parent into a plan that holds none,
// and writes no version of that plan. The build at N-1, whose manifest declares no parent, answers
// the plan's version 409 DOC_NEWER_CONTENT. The test changes the manifest, a package variable, so
// it stays serial.
func TestAVersionsBlocksCarryTheParentOfATaskBlock(t *testing.T) {
	database := storetest.Open(t)
	seedPeople(t, database, "alice")
	const parent = "dispatch://ACME-1/artifact/plan-md#b-default"
	under := func(id, text string) string {
		return ":::plan_task{#" + id + " parent=\"" + parent + "\"}\n" + text + "\n:::\n"
	}
	planText := ":::plan_task{#default}\nShip the parser.\n:::\n\n" + under("t1", "Read the grammar.") + "\n" +
		under("t2", "Write the parser.") + "\n" + under("t3", "Test the parser.")
	placed := map[string]string{"default": "", "t1": parent, "t2": parent, "t3": parent}
	versions, declared := pmdoc.Element{Kind: pmdoc.AttributeElement, Block: "plan_task", Name: "parent"}.Versions()
	if !declared || versions.CreatableSince != versions.Since+1 {
		t.Fatalf("plan_task.parent = %+v, %t; want it declared, created one version after it is read", versions, declared)
	}
	n := versions.Since
	asOf := func(version int) []byte {
		t.Helper()
		manifest, err := pmdoc.ManifestAsOfForTest(version)
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}

	restoreNewer, err := pmdoc.UseManifestForTest(asOf(n + 1))
	if err != nil {
		t.Fatalf("install the manifest of the build at version %d: %v", n+1, err)
	}
	t.Cleanup(restoreNewer)
	newer, stopNewer := versionBlocksProcess(t, database)
	if created := dispatchRequest(t, newer, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACME", "name": "Acme"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project ACME: status=%d body=%s", created.Code, created.Body.String())
	}
	filed := dispatchRequest(t, newer, http.MethodPost, "/api/v1/issues", map[string]string{"project": "ACME", "title": "A plan under its task"}, "alice")
	if filed.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", filed.Code, filed.Body.String())
	}
	issue := decodeBody[model.Issue](t, filed)
	if issue.Key != "ACME-1" {
		t.Fatalf("the issue is %s, want ACME-1, which the plan's parents name", issue.Key)
	}
	plan := createIssueDocument(t, newer, issue.Key, "plan.md", planText)
	stopNewer()
	restoreNewer()

	restoreCurrent, err := pmdoc.UseManifestForTest(asOf(n))
	if err != nil {
		t.Fatalf("install the manifest of the build at version %d: %v", n, err)
	}
	t.Cleanup(restoreCurrent)
	current, stopCurrent := versionBlocksProcess(t, database)
	read := dispatchRequest(t, current, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("this build reading version 1 of the plan the newer build wrote: status=%d body=%s", read.Code, read.Body.String())
	}
	requireParents(t, "version 1, read by this build", decodeBody[versionBlocksAnswer](t, read), placed)
	uploadIssueDocument(t, current, issue.Key, "plan.md", planText+"\n"+under("t4", "Ship it."))
	placed["t4"] = parent
	requireParents(t, "version 2, which this build wrote beside them", readVersionBlocks(t, current, "/api/v1/artifacts/"+plan.ID+"/versions/2?blocks=true"), placed)

	const notesText = ":::plan_task{#n1}\nNote it.\n:::\n"
	notes := createIssueDocument(t, current, issue.Key, "notes.md", notesText)
	owner := "/api/v1/issues/" + issue.Key + "/artifacts"
	refused := dispatchRequest(t, current, http.MethodPost, owner, map[string]any{
		"name":         "notes.md",
		"content":      notesText + "\n:::plan_task{#n2 parent=\"dispatch://ACME-1/artifact/notes-md#b-n1\"}\nUnder it.\n:::\n",
		"precondition": uploadBaseNamed(t, current, owner, "notes.md"),
	}, "alice")
	expectRefusal(t, "this build putting parent into a plan that holds none", refused, http.StatusBadRequest, "INVALID_MARKDOWN",
		`this Dispatch build reads attribute "parent" of typed block "plan_task" but cannot write it yet`)
	if second := dispatchRequest(t, current, http.MethodGet, "/api/v1/artifacts/"+notes.ID+"/versions/2", nil, "alice"); second.Code != http.StatusNotFound {
		t.Fatalf("the refused write left version 2 of the plan that holds no parent: status=%d body=%s", second.Code, second.Body.String())
	}
	stopCurrent()
	restoreCurrent()

	restoreOlder, err := pmdoc.UseManifestForTest(asOf(n - 1))
	if err != nil {
		t.Fatalf("install the manifest of the build at version %d: %v", n-1, err)
	}
	t.Cleanup(restoreOlder)
	older, _ := versionBlocksProcess(t, database)
	newerContent := dispatchRequest(t, older, http.MethodGet, "/api/v1/artifacts/"+plan.ID+"/versions/1?blocks=true", nil, "alice")
	expectRefusal(t, "a build of the release before reading version 1 of the plan", newerContent, http.StatusConflict, "DOC_NEWER_CONTENT", "parent")

	requireVersionBlocksFixture(t, "plan-with-parent.json", read.Body.Bytes(), func(fixture, answer []byte) bool {
		var held, served versionBlocksAnswer
		if json.Unmarshal(fixture, &held) != nil || json.Unmarshal(answer, &served) != nil || held.Blocks == nil || served.Blocks == nil {
			return false
		}
		return held.Markdown == served.Markdown && reflect.DeepEqual(blocksBesideTheirTokens(*held.Blocks), blocksBesideTheirTokens(*served.Blocks))
	})
	requireVersionBlocksFixture(t, "older-build-newer-content.json", newerContent.Body.Bytes(), func(fixture, answer []byte) bool {
		var held, served any
		return json.Unmarshal(fixture, &held) == nil && json.Unmarshal(answer, &served) == nil && reflect.DeepEqual(held, served)
	})
}

// versionBlocksProcess is one Dispatch process on database, with a document service of its own,
// as one build of a release serves. stop ends its routes' background work and shuts its service
// down, so nothing of it reads the manifest once stop returns, and the caller may install another
// (pmdoc.UseManifestForTest). The test's end stops a process the test did not.
func versionBlocksProcess(t *testing.T, database *store.Store) (http.Handler, func()) {
	t.Helper()
	service := docs.New(docs.Deps{Store: database, ServerURL: testServerURL, Settle: 20 * time.Millisecond})
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs:        service,
		ServerURL:   testServerURL,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	lifetime, end := context.WithCancel(t.Context())
	mux := http.NewServeMux()
	background := Register(mux, lifetime, deps)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			end()
			background.Wait()
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shut down the process's document service: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return mux, stop
}

// requireParents requires answer to list exactly the task blocks want names, each with the parent
// want gives it as a block[] attribute, and with none where want gives "".
func requireParents(t *testing.T, what string, answer versionBlocksAnswer, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, block := range *answer.Blocks {
		if block.Type != "plan_task" {
			continue
		}
		got[block.ID] = ""
		for _, attribute := range block.Attributes {
			if attribute.Name != "parent" {
				continue
			}
			if attribute.Kind != string(pmdoc.BlockAttributeBlockList) {
				t.Errorf("%s: block %s carries parent as a %q attribute, want block[]", what, block.ID, attribute.Kind)
			}
			got[block.ID] = attribute.Value
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("%s lists the task blocks and their parents %v, want %v (markdown %q)", what, got, want, answer.Markdown)
	}
}

// blocksBesideTheirTokens is blocks with no token: what a fixture of a version read holds to
// this build's answer, beside the markdown.
func blocksBesideTheirTokens(blocks []model.ArtifactBlock) []model.ArtifactBlock {
	stripped := make([]model.ArtifactBlock, len(blocks))
	for index, block := range blocks {
		block.Token = ""
		stripped[index] = block
	}
	return stripped
}

// requireVersionBlocksFixture writes answer to the fixture name under -update (updateFixtures),
// indented, and otherwise requires the committed fixture and answer to be the same answer by same.
func requireVersionBlocksFixture(t *testing.T, name string, answer []byte, same func(fixture, answer []byte) bool) {
	t.Helper()
	path := filepath.Join(versionBlocksFixtures, name)
	if *updateFixtures {
		var indented bytes.Buffer
		if err := json.Indent(&indented, answer, "", "  "); err != nil {
			t.Fatalf("indent the answer for %s: %v", name, err)
		}
		indented.WriteByte('\n')
		if err := os.MkdirAll(versionBlocksFixtures, 0o755); err != nil {
			t.Fatalf("make the fixture directory: %v", err)
		}
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
		return
	}
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, which -update writes: %v", path, err)
	}
	if !same(fixture, answer) {
		t.Fatalf("%s is not what this build answers. Write it again with -update.\nfixture: %s\nanswer:  %s", path, fixture, answer)
	}
}
