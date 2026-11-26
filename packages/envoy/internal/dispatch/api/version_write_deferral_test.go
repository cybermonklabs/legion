package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The routes that write a version through the document service with nothing in their transaction
// reading the version's block index or its record after it - the edit route, by either of its
// writes, an anchored comment's and an anchored ask's pin, the accept of a suggestion, the
// named-version route and the rebuild - mark their write (docs.WithDeferredBlockIndex), so a version
// above the bound leaves its blocks to the block-index reconciliation, as an upload's does. A
// settlement's version and the decisions step's are written unmarked and index their blocks in
// their own transaction.

// deferralPath is one of the two ways a document's blocks are indexed: a document that is its own
// room, the spec of an issue that has not converted, and a fragment of an issue room, the spec of a
// converted issue.
type deferralPath struct {
	name string
	room bool
}

var deferralPaths = []deferralPath{
	{name: "a document that is its own room"},
	{name: "a document of an issue room", room: true},
}

// itemsUnderTheBoundWithItsWrites is a list that keeps the spec under the bound with every version
// the test writes. The bound counts the blocks the render records (pmdoc.RenderWithBlockOffsets),
// and a list of n items is 2n+1 of them - the list, each item and each item's paragraph - so the
// spec of 970 items is 1,946 blocks with its heading and its four paragraphs, and six of the seven
// writes add a paragraph each, well inside deferralWritesSlack (requireRoomForTheWrites).
const itemsUnderTheBoundWithItsWrites = 970

// deferralWritesSlack is the room under the bound the spec under it keeps for the blocks the test's
// writes add to it: a few dozen.
const deferralWritesSlack = 40

// deferralBound is docs' deferBlockIndexBlocks: a version of more blocks than this leaves its
// blocks to the block-index reconciliation.
const deferralBound = 2_000

// deferralSpec is the spec the test writes versions of: a list past the bound where above, else one
// that stays under it, and a paragraph for each anchor and the suggestion the writes quote.
func deferralSpec(above bool) string {
	items := itemsUnderTheBoundWithItsWrites
	if above {
		items = itemsOverTheBlockBound
	}
	var spec strings.Builder
	spec.WriteString("# Spec\n\n")
	for _, quote := range []string{"The comment's paragraph holds.", "The ask's paragraph holds.", "The suggestion's paragraph holds.", "The carry's paragraph holds."} {
		spec.WriteString(quote + "\n\n")
	}
	spec.WriteString(deferralList(items, "end"))
	return spec.String()
}

// requireRoomForTheWrites fails t unless spec, its blocks counted as the render records them - the
// offsets whose number docs' version writes hold to the bound - keeps deferralWritesSlack blocks
// under the bound, so each write of the rows under it writes a version its own write indexes.
func requireRoomForTheWrites(t *testing.T, spec string) {
	t.Helper()
	if blocks := renderedBlocks(t, spec); blocks+deferralWritesSlack > deferralBound {
		t.Fatalf("the spec under the bound holds %d blocks as the render records them, so with the %d its writes may add it passes the bound of %d blocks a write indexes in its own request: the rows under the bound would write deferred versions", blocks, deferralWritesSlack, deferralBound)
	}
}

// deferralDocument is the spec of one issue on one path, on one side of the bound, whose versions
// the test writes, with a browser editing it where a write needs an edit no settlement has
// versioned, and the document store whose next load of the spec's room a rebuild fails.
type deferralDocument struct {
	handler   http.Handler
	database  *store.Store
	deps      Deps
	documents *docs.Service
	broken    *invalidLoadOnceStore
	issueKey  string
	spec      string
	above     bool
	peer      *syncedPeer
}

// newDeferralDocument creates the issue on path holding deferralSpec, and runs the reconciliation
// until its spec is indexed, so each write starts from an indexed document.
func newDeferralDocument(t *testing.T, path deferralPath, above bool) *deferralDocument {
	t.Helper()
	spec := deferralSpec(above)
	if !above {
		requireRoomForTheWrites(t, spec)
	}
	d := &deferralDocument{above: above}
	d.handler, d.database, d.deps = newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true,
		persistence: func(database *store.Store) docs.VersionedStore {
			d.broken = newInvalidLoadOnceStore(database)
			return d.broken
		}})
	d.documents = d.deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, d.handler, "DEFER")
	issue := createDigestIssue(t, d.handler, d.database, "DEFER", "The version writes of "+path.name, spec, !path.room)
	d.issueKey, d.spec = issue.Key, issue.PrimaryArtifactID
	reconcileUntilNothingOwed(t, d.handler, d.deps, d.issueKey)
	requireIndexedAt(t, d.database, d.spec, 1)
	return d
}

// wrote runs write, which writes the spec's next version through what, and fails t unless, above
// the bound, the version leaves the spec owed - its document_block_versions version null, the
// coverage read and the tree read naming it at that version, which holds no stated record - and the
// reconciliation then indexes it at that version; under the bound, unless the write indexed it at
// that version itself. It answers the version's number.
func (d *deferralDocument) wrote(t *testing.T, what string, write func()) int {
	t.Helper()
	before := latestVersion(t, d.database, d.spec).Number
	write()
	version := latestVersion(t, d.database, d.spec).Number
	if version != before+1 {
		t.Fatalf("%s wrote version %d of the spec, want %d", what, version, before+1)
	}
	indexed := blockIndexVersion(t, d.database, d.spec)
	if !d.above {
		if indexed == nil || *indexed != version {
			t.Errorf("%s under the bound left the spec's blocks indexed at %s, want its version %d indexed by its own write", what, indexedAt(indexed), version)
		}
		return version
	}
	if indexed != nil {
		t.Errorf("%s indexed the spec's version %d inline (its blocks indexed at %d), want it owed to the block-index reconciliation", what, version, *indexed)
		return version
	}
	requireOwedNamed(t, d.handler, d.issueKey, d.spec, version, what)
	requireUnstatedVersion(t, d.database, d.spec, version)
	reconcileUntilNothingOwed(t, d.handler, d.deps, d.issueKey)
	requireIndexedAt(t, d.database, d.spec, version)
	return version
}

// indexedAt is the version a block index stands at, as a failure names it.
func indexedAt(indexed *int) string {
	if indexed == nil {
		return "no version (owed)"
	}
	return fmt.Sprint(*indexed)
}

// browserEdit has a browser write change into the spec with no settlement versioning it, so the
// next version a route writes takes it.
func (d *deferralDocument) browserEdit(t *testing.T, change func(*pmdoc.Node) error) {
	t.Helper()
	if d.peer == nil {
		d.peer = connectBrowserPeer(t, d.handler, d.documents, d.spec)
		t.Cleanup(d.peer.close)
	}
	d.peer.edit(t, change)
	d.peer.barrier(t)
}

// browserParagraph has the browser append a paragraph holding words (browserEdit).
func (d *deferralDocument) browserParagraph(t *testing.T, words string) {
	t.Helper()
	d.browserEdit(t, func(tree *pmdoc.Node) error {
		typed, err := pmdoc.Parse(words + "\n")
		if err != nil {
			return err
		}
		tree.Children = append(tree.Children, typed.Children...)
		return nil
	})
}

// closeBrowser closes the browser and evicts the room the spec is edited in (artifactRoom), its
// issue's room for a fragment, which a rebuild and the decisions step each take on a room no
// process holds.
func (d *deferralDocument) closeBrowser(t *testing.T) {
	t.Helper()
	if d.peer != nil {
		d.peer.closeAndWait(t)
		d.peer = nil
	}
	if err := d.documents.Evict(context.Background(), artifactRoom(t, d.handler, d.spec)); err != nil {
		t.Fatalf("evict the spec's room: %v", err)
	}
}

// edit writes the spec's next version through POST /api/v1/artifacts/{id}/edits, inserting words
// at its end, with summary where it is not "" (NamedVersion), else with none (SnapshotVersion).
func (d *deferralDocument) edit(t *testing.T, words, summary string) {
	t.Helper()
	body := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": words + "\n"}}}
	if summary != "" {
		body["summary"] = summary
	}
	answer := dispatchRequest(t, d.handler, http.MethodPost, "/api/v1/artifacts/"+d.spec+"/edits", body, "alice")
	succeeded(t, "the edit inserting "+words, answer)
	if written := decodeBody[editArtifactResponse](t, answer).Version; written == nil || written.Named != (summary != "") {
		t.Fatalf("the edit inserting %q wrote the version %+v, want one named %t", words, written, summary != "")
	}
}

// anchoredWrites are the writes of a version the routes mark, each through what it is named by.
func (d *deferralDocument) anchoredWrites(t *testing.T) {
	t.Helper()
	d.wrote(t, "the edit with no summary", func() { d.edit(t, "An edit with no summary.", "") })
	d.wrote(t, "the edit with a summary", func() { d.edit(t, "An edit with a summary.", "A summary") })
	commentVersion := d.wrote(t, "the anchored comment's pin", func() {
		d.browserParagraph(t, "A browser's words the comment's pin versions.")
		answer := dispatchRequest(t, d.handler, http.MethodPost, "/api/v1/issues/"+d.issueKey+"/comments", map[string]any{
			"body": "Does this hold?", "anchor": map[string]any{"artifact": "spec", "quote": "The comment's paragraph holds."},
		}, "alice")
		if answer.Code != http.StatusCreated {
			t.Fatalf("the anchored comment: status=%d body=%s", answer.Code, answer.Body.String())
		}
		if comment := decodeBody[model.Comment](t, answer); comment.Anchor == nil || comment.Anchor.Version != latestVersion(t, d.database, d.spec).Number {
			t.Fatalf("the anchored comment is pinned to %+v, want the version its pin wrote", comment.Anchor)
		}
	})
	d.wrote(t, "the anchored ask's pin", func() {
		d.browserParagraph(t, "A browser's words the ask's pin versions.")
		answer := sessionRequest(t, d.handler, http.MethodPost, "/api/v1/issues/"+d.issueKey+"/asks", map[string]any{
			"question": "Which team signs off on this paragraph?",
			"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
			"anchor":   map[string]any{"artifact": "spec", "quote": "The ask's paragraph holds."}, "actor": sessionActor(),
		})
		if answer.Code != http.StatusCreated {
			t.Fatalf("the anchored ask: status=%d body=%s", answer.Code, answer.Body.String())
		}
		if ask := decodeBody[model.Ask](t, answer); ask.Anchor == nil || ask.Anchor.Version != commentVersion+1 {
			t.Fatalf("the anchored ask is pinned to %+v, want version %d, the version its pin wrote", ask.Anchor, commentVersion+1)
		}
	})
	suggested := sessionRequest(t, d.handler, http.MethodPost, "/api/v1/issues/"+d.issueKey+"/comments", map[string]any{
		"body": "This paragraph no longer holds as worded.", "anchor": map[string]any{"artifact": "spec", "quote": "The suggestion's paragraph holds."},
		"suggestion": map[string]string{"replace_with": "The suggestion's paragraph stands."}, "actor": sessionActor(),
	})
	if suggested.Code != http.StatusCreated {
		t.Fatalf("open the suggestion: status=%d body=%s", suggested.Code, suggested.Body.String())
	}
	suggestion := decodeBody[model.Comment](t, suggested)
	accepted := d.wrote(t, "the accept of a suggestion", func() {
		succeeded(t, "accept the suggestion", dispatchRequest(t, d.handler, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice"))
	})
	if markdown := versionMarkdownAt(t, d.database, d.spec, accepted); !strings.Contains(markdown, "The suggestion's paragraph stands.") {
		t.Errorf("the accept's version %d does not hold the suggestion applied:\n%s", accepted, markdown)
	}
	d.wrote(t, "the named-version route", func() {
		d.browserParagraph(t, "A browser's words the named version takes.")
		answer := sessionRequest(t, d.handler, http.MethodPost, "/api/v1/artifacts/"+d.spec+"/versions", map[string]any{
			"summary": "A named version", "actor": sessionActor(),
		})
		if answer.Code != http.StatusCreated {
			t.Fatalf("the named version: status=%d body=%s", answer.Code, answer.Body.String())
		}
	})
	d.closeBrowser(t)
	d.wrote(t, "the rebuild with its markdown supplied", func() {
		markdown := versionMarkdownAt(t, d.database, d.spec, latestVersion(t, d.database, d.spec).Number)
		d.broken.failNextLoad(t, d.spec)
		succeeded(t, "rebuild the spec", dispatchRequest(t, d.handler, http.MethodPost, "/api/v1/artifacts/"+d.spec+"/rebuild",
			map[string]any{"markdown": markdown + "\nA paragraph the rebuild writes.\n"}, "alice"))
	})
}

// carriedPin has alice approve the spec's latest version, an edit stale that approval and a browser
// take the edit back out, so the anchored comment's pin writes the approved text again and carries
// her approval to it: the version is deferred, so no pass of the pin's transaction records its
// version_renders row, and the review the carry writes pins the sha256 of the version's stored
// markdown. It fails t unless that review - the first review of the version by the order their
// events committed - holds that digest. The reconciliation that states the version afterwards
// carries again where its text is the approved text (R14: the newest review is an approval of that
// same version), and that second review pins the record's digest, whose own check is
// TestTheTreeRendersAPostCutoverVersionFromItsTuples.
func (d *deferralDocument) carriedPin(t *testing.T) {
	t.Helper()
	approval := reviewDocument(t, d.handler, d.spec, "alice", "approved", "")
	d.wrote(t, "the edit that stales the approval", func() { d.edit(t, "An edit the browser takes back out.", "") })
	pinned := d.wrote(t, "the anchored pin that restores the approved text", func() {
		d.browserEdit(t, func(tree *pmdoc.Node) error {
			tree.Children = tree.Children[:len(tree.Children)-1]
			return nil
		})
		answer := dispatchRequest(t, d.handler, http.MethodPost, "/api/v1/issues/"+d.issueKey+"/comments", map[string]any{
			"body": "Is this the approved text?", "anchor": map[string]any{"artifact": "spec", "quote": "The carry's paragraph holds."},
		}, "alice")
		if answer.Code != http.StatusCreated {
			t.Fatalf("the anchored comment restoring the approved text: status=%d body=%s", answer.Code, answer.Body.String())
		}
	})
	markdown := versionMarkdownAt(t, d.database, d.spec, pinned)
	if approved := versionMarkdownAt(t, d.database, d.spec, approval.Version); markdown != approved {
		t.Fatalf("the pin's version %d does not hold the text approved at version %d, so it carries nothing:\n%s", pinned, approval.Version, markdown)
	}
	want := contentHash(markdown)
	var digest *string
	if err := d.database.Pool.QueryRow(context.Background(), `
		select content_sha256 from artifact_reviews where artifact_id = $1 and version = $2 and state = 'approved'
		order by event_seq asc nulls last, created_at asc, id asc limit 1`,
		d.spec, pinned).Scan(&digest); err != nil {
		t.Fatalf("read the review the pin's carry wrote to version %d: %v", pinned, err)
	}
	if digest == nil || *digest != want {
		t.Errorf("the review the pin's carry wrote to version %d pins the digest %s, want the sha256 of the version's stored markdown, %s", pinned, valueOrNone(digest), want)
	}
}

// inlineWrites are the two writes of a version left unmarked, which index their blocks in their own
// transaction whatever the size: a settlement's version of a browser's edit, and the decisions
// step's version of the spec, which moves an ask block of the issue's decisions document into it
// and states the block's moved_to, which resolves through the spec's document_blocks rows.
func (d *deferralDocument) inlineWrites(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	d.browserParagraph(t, "A browser's words the settlement versions.")
	before := latestVersion(t, d.database, d.spec).Number
	settleIssue(t, d.documents, d.issueKey)
	if settled := latestVersion(t, d.database, d.spec).Number; settled != before+1 {
		t.Fatalf("the settlement wrote version %d of the spec, want %d", settled, before+1)
	} else if indexed := blockIndexVersion(t, d.database, d.spec); indexed == nil || *indexed != settled {
		t.Errorf("the settlement's version %d left the spec's blocks indexed at %s, want them indexed by the settlement's own transaction", settled, indexedAt(indexed))
	}
	d.closeBrowser(t)
	decisions := seedDecisionsDocument(t, d.database, d.documents, d.issueKey, "# Decisions\n\n## Which region goes first\n\n"+
		":::ask{#q-open urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n:::\n")
	step := docs.NewDecisionStep(d.database, "", nil)
	step.UseDocuments(d.documents)
	done, failed, err := step.RestoreAndConvert(ctx, "DEFER", true, false)
	if err != nil || len(failed) > 0 || len(done) != 1 || done[0].SpecVersion == 0 {
		t.Fatalf("the decisions step answered %+v, %+v, %v, want the issue's spec versioned", done, failed, err)
	}
	if indexed := blockIndexVersion(t, d.database, d.spec); indexed == nil || *indexed != done[0].SpecVersion {
		t.Errorf("the decisions step's version %d left the spec's blocks indexed at %s, want them indexed by the step's own transaction", done[0].SpecVersion, indexedAt(indexed))
	}
	var moved, reach string
	if err := d.database.Pool.QueryRow(ctx, `
		select a.ref_key || '#q-open', coalesce((select reach.kind from moved_to_reach(a.ref_key || '#q-open') reach), '')
		from artifacts a where a.id = $1::uuid`, decisions).Scan(&moved, &reach); err != nil {
		t.Fatalf("resolve the moved_to of the decisions document's ask block: %v", err)
	}
	if reach != "block" {
		t.Errorf("the decisions step's moved_to from %s dangles (it reaches %q), want it resolved to the spec's block through its document_blocks row", moved, reach)
	}
}

// TestAnEditAboveTheBoundDefersItsIndex: on each path, of a spec above the bound, an edit with no
// summary and one with a summary, an anchored comment's and an anchored ask's pin, the accept of a
// suggestion, the named-version route and the rebuild each leave the spec owed its block index, and
// the reconciliation then indexes it; under the bound each indexes it in its own write. Above the
// bound an anchored pin whose version restores the approved text carries the approval with the
// digest of the version's stored markdown, and a settlement's version and the decisions step's each
// index the spec in their own transaction, the step's moved_to resolving through its rows.
func TestAnEditAboveTheBoundDefersItsIndex(t *testing.T) {
	for _, path := range deferralPaths {
		for _, above := range []bool{true, false} {
			side := "under the bound"
			if above {
				side = "above the bound"
			}
			t.Run(path.name+" "+side, func(t *testing.T) {
				d := newDeferralDocument(t, path, above)
				d.anchoredWrites(t)
				if !above || t.Failed() {
					return
				}
				d.carriedPin(t)
				d.inlineWrites(t)
			})
		}
	}
}
