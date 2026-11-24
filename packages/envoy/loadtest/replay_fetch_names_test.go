package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of the files replay-fetch names after what Dispatch's answers name: a document id of a
// listing (docs/ and raw/versions/), a document id of a window's artifact.created (created-docs/ and
// raw/versions/), and an issue key of a window's issue.created (raw/artifacts/). Whatever answers in
// Dispatch's place can name one that climbs out of the bundle with "..", or one longer than a file
// name may be, whose error printed the path; each echoes the fetch's bearer
// (answer_members_test.go), so a file named after it, or an error that prints it, holds the bearer.
// replay-fetch refuses such an id or key where it decodes the answer naming it (listingInShape,
// eventInShape), before it names a file after it, and names every file after an id or key in
// Dispatch's shape as it is.

// namedShapes are a document id, a created document's id and a created issue's key in Dispatch's
// shape, which a row's hostile member stands in for, one at a time.
var namedShapes = [3]string{"d0c00000-0000-4000-8000-000000000001", "c4ea7ed0-0000-4000-8000-000000000002", "PRJ-2"}

// requireHostileNamesNone runs one replay-fetch for each of the three members, with hostile in its
// place and the other two in Dispatch's shape, and requires of each run what requireNamesNone does;
// then, as the control, runs one with all three in Dispatch's shape and requires its bundle to name
// each file after its id or key.
func requireHostileNamesNone(t *testing.T, capture, hostile string) {
	t.Helper()
	for i, member := range []string{"listed-document", "created-document", "created-issue"} {
		t.Run(member, func(t *testing.T) {
			ids := namedShapes
			ids[i] = hostile + "-" + member
			runNamedFetch(t, ids[0], ids[1], ids[2]).requireNamesNone(t, capture)
		})
	}
	runNamedFetch(t, namedShapes[0], namedShapes[1], namedShapes[2]).requireNamedAsTheyAre(t)
}

// namedFetch is one replay-fetch of a stand-in Dispatch whose PRJ-1 listing names docID, whose
// window creates the document createdID and the issue createdKey (with no primary document, so its
// listing is read for its spec), into a bundle nested four directories into root, so a file named
// after an id that climbs out with ".." still lands inside root.
type namedFetch struct {
	root, out string
	counts    replayCounts
	err       error
	logged    bytes.Buffer
}

func runNamedFetch(t *testing.T, docID, createdID, createdKey string) *namedFetch {
	t.Helper()
	const spec = "5c000000-0000-4000-8000-00000000000c"
	listing := func(id string) []map[string]any {
		return []map[string]any{{"id": id, "name": "spec", "kind": "doc", "primary": true,
			"created_at": mins(-100), "versions": []map[string]any{{"number": 1, "created_at": mins(-100)}}}}
	}
	var events []json.RawMessage
	for _, ev := range []replayEvent{
		mkEvent(t, 1, "PRJ-1", evIssueCreated, mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", map[string]any{"primary_artifact_id": nil})),
		mkEvent(t, 2, "PRJ-1", evArtifactCreated, mins(6), sess("s0"), map[string]any{"artifact": map[string]any{"id": createdID, "kind": "doc"}}),
		mkEvent(t, 3, createdKey, evIssueCreated, mins(8), sess("s0"), issueSnap(createdKey, "t", "open", map[string]any{"primary_artifact_id": nil})),
	} {
		events = append(events, ev.Raw)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/api/v1/issues":
			writeJSONAnswer(w, http.StatusOK, []map[string]string{{"key": "PRJ-1"}})
		case p == "/api/v1/issues/PRJ-1/events":
			writeJSONAnswer(w, http.StatusOK, events)
		case p == "/api/v1/issues/PRJ-1/artifacts":
			writeJSONAnswer(w, http.StatusOK, listing(docID))
		case strings.HasSuffix(p, "/artifacts"):
			writeJSONAnswer(w, http.StatusOK, listing(spec))
		case strings.Contains(p, "/versions/"):
			writeJSONAnswer(w, http.StatusOK, map[string]string{"markdown": "A document's text.\n"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	run := &namedFetch{root: t.TempDir()}
	run.out = filepath.Join(run.root, "a", "b", "c", "bundle")
	f := &replayFetcher{api: newAPI(srv.URL, memberBearer, 2), raw: filepath.Join(run.out, bundleRawDir), conc: 2, lg: log.New(&run.logged, "", 0)}
	run.counts, run.err = f.run(context.Background(), "PRJ", tAt, tUntil, run.out, nil)
	return run
}

// files is every file under the run's root, as slash paths from root, and how many of them are
// outside the bundle.
func (run *namedFetch) files(t *testing.T) (all []string, outside int) {
	t.Helper()
	bundle, err := filepath.Rel(run.root, run.out)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(run.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(run.root, p)
		if err != nil {
			return err
		}
		all = append(all, filepath.ToSlash(rel))
		if !strings.HasPrefix(rel, bundle+string(filepath.Separator)) {
			outside++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("the walk found no file at all, so it cannot say where replay-fetch wrote")
	}
	return all, outside
}

// requireNamesNone captures the run's error, its log and the name of every file it wrote under
// capture, and fails where they hold a credential of the probe or a run of the bearer, or where a
// file lies outside the bundle; then, as the control, requires replay-fetch to have refused the
// answer that named the hostile member where it decoded it (PRJ-1's listing, or a page of PRJ-1's
// events) and to have written no bundle.
func (run *namedFetch) requireNamesNone(t *testing.T, capture string) {
	t.Helper()
	files, outside := run.files(t)
	redacttest.RequireNoPiece(t, capture, errorText(run.err)+"\n"+run.logged.String()+"\n"+strings.Join(files, "\n"), memberBearer)
	if outside > 0 {
		t.Errorf("replay-fetch wrote %d files outside the bundle %s", outside, run.out)
	}
	if msg := errorText(run.err); !strings.Contains(msg, "PRJ-1 artifacts: the listing names an artifact by an id that is not in Dispatch's shape") &&
		!strings.Contains(msg, "PRJ-1 events after 0: event ") {
		t.Errorf("replay-fetch = %v, want the refusal of the answer naming the member", run.err)
	}
	if _, err := os.Stat(filepath.Join(run.out, bundleMetaFile)); err == nil {
		t.Errorf("replay-fetch wrote a bundle over an answer it refused")
	}
}

// requireNamedAsTheyAre requires a run over members in Dispatch's shape to have read every document
// and its bundle to verify and hold each one's text, each file named after its id as it is: the
// state's document at docs/<id>.md, and the created document and the created spec under
// created-docs/<id>.md, where the plan's rows (TestReplayDocCreated) read them (bundleCreatedRel).
func (run *namedFetch) requireNamedAsTheyAre(t *testing.T) {
	t.Helper()
	if run.err != nil {
		t.Fatalf("replay-fetch over members in Dispatch's shape = %v, want a bundle", run.err)
	}
	if run.counts.DocsUnreadable != 0 || run.counts.CreatedDocs != 1 || run.counts.CreatedSpecs != 1 {
		t.Fatalf("counts %+v, want the state's document, the created document and the created spec read", run.counts)
	}
	b, err := readBundle(run.out)
	if err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	doc := b.State.Issues[0].Artifacts[0]
	texts := []string{doc.TextPath}
	for rel := range b.Meta.Files {
		if strings.HasPrefix(rel, bundleCreatedDir+"/") {
			texts = append(texts, rel)
		}
	}
	slices.Sort(texts[1:])
	want := []string{bundleDocsDir + "/" + namedShapes[0] + ".md", bundleCreatedDir + "/5c000000-0000-4000-8000-00000000000c.md", bundleCreatedDir + "/" + namedShapes[1] + ".md"}
	if doc.ID != namedShapes[0] || !slices.Equal(texts, want) {
		t.Fatalf("the state's document %s and the bundle's texts %v, want %s and %v", doc.ID, texts, namedShapes[0], want)
	}
	for _, rel := range texts {
		if text, err := b.readFile(rel); err != nil || string(text) != "A document's text.\n" {
			t.Errorf("%s reads %q (%v); want the document's text", rel, text, err)
		}
	}
}

// TestAnIDThatClimbsOutOfTheBundleNamesNoFileOutsideIt: Dispatch's listing, or a stand-in's in
// its place, names a document id that climbs out of the bundle with "../", or a window creates a
// document or an issue named so, and replay-fetch wrote each one's cache file, text and listing
// outside raw/ and the bundle, under names that hold the bearer the id echoes. replay-fetch now
// refuses the answer naming it where it decodes it, before any file is named after it. The
// controls: each refusal names that answer, and the same fetch over members in Dispatch's shape
// names each file after its member. Captures loadtest.answer-members/file-names-dotdot.
func TestAnIDThatClimbsOutOfTheBundleNamesNoFileOutsideIt(t *testing.T) {
	requireHostileNamesNone(t, "loadtest.answer-members/file-names-dotdot", "../../../"+memberBearer)
}

// TestAnIDLongerThanAFileNameNamesNoneOfIt: Dispatch's listing, or a stand-in's in its place,
// names a document id of 320 bytes, or a window creates a document or an issue named so, and
// replay-fetch's read of its cache failed with the *os.PathError of a name too long, which printed
// the path and the id in it. replay-fetch now refuses the answer naming it where it decodes it,
// before any file is named after it. The controls: each refusal names that answer, and the same
// fetch over members in Dispatch's shape names each file after its member. Captures
// loadtest.answer-members/file-names-long.
func TestAnIDLongerThanAFileNameNamesNoneOfIt(t *testing.T) {
	requireHostileNamesNone(t, "loadtest.answer-members/file-names-long", strings.Repeat(memberBearer, 8))
}
