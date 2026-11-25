package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// premortemCapturePath is the capture of the graph pre-mortem of cybermonklabs/legion#3142, copied
// unchanged from that pull request's branch, from the repository root.
const premortemCapturePath = "packages/dispatch/e2e/fixtures/acme-1098-premortem.json"

// premortemCapture is what the capture records: the document's markdown, and the verdict and each
// risk's heading and section as its own capture read them.
type premortemCapture struct {
	Document struct {
		Markdown string `json:"markdown"`
	} `json:"document"`
	Verdict string `json:"verdict"`
	Risks   []struct {
		Index    int    `json:"index"`
		Title    string `json:"title"`
		Markdown string `json:"markdown"`
	} `json:"risks"`
}

func readPremortemCapture(t *testing.T) premortemCapture {
	t.Helper()
	raw := readRepositoryFile(t, repositoryRoot(t), premortemCapturePath)
	var capture premortemCapture
	if err := json.Unmarshal([]byte(raw), &capture); err != nil {
		t.Fatalf("decode the capture: %v", err)
	}
	if capture.Document.Markdown == "" || len(capture.Risks) == 0 {
		t.Fatalf("the capture holds no markdown or no risk: %d bytes, %d risks", len(capture.Document.Markdown), len(capture.Risks))
	}
	return capture
}

// TestTheParserReadsTheCapturedPremortem reads the captured pre-mortem's markdown and finds what
// the capture recorded of it: the verdict word for word, eleven risks numbered 0 to 10, each with
// the heading the capture names and the early warning and prevention its section holds, and the
// two closing sections word for word, which the lens's verdict carries after the document's own.
func TestTheParserReadsTheCapturedPremortem(t *testing.T) {
	t.Parallel()
	capture := readPremortemCapture(t)
	document, err := parsePremortem(capture.Document.Markdown)
	if err != nil {
		t.Fatalf("parse the captured pre-mortem: %v", err)
	}
	if document.verdict != capture.Verdict {
		t.Errorf("the verdict reads %q, want the capture's %q", document.verdict, capture.Verdict)
	}
	if len(document.risks) != 11 || len(capture.Risks) != 11 {
		t.Fatalf("the parser reads %d risks and the capture records %d, want eleven each", len(document.risks), len(capture.Risks))
	}
	for index, risk := range document.risks {
		recorded := capture.Risks[index]
		t.Logf("risk %d: %s", risk.number, risk.heading)
		if risk.number != index || recorded.Index != index {
			t.Errorf("risk %d is numbered %d, and the capture's %d, want %d", index, risk.number, recorded.Index, index)
		}
		if risk.heading != recorded.Title {
			t.Errorf("risk %d's heading reads %q, want the capture's %q", index, risk.heading, recorded.Title)
		}
		if risk.earlyWarning == "" || !strings.Contains(recorded.Markdown, "- **Early warning:** "+risk.earlyWarning+"\n") {
			t.Errorf("risk %d's early warning %q is not its section's item word for word", index, risk.earlyWarning)
		}
		if risk.prevention == "" || !strings.HasSuffix(recorded.Markdown, "- **Prevention:** "+risk.prevention) {
			t.Errorf("risk %d's prevention %q is not its section's last item word for word", index, risk.prevention)
		}
	}
	worldClass := "## What a world-class implementation looks like\n\n" + document.worldClass +
		"\n\n## The smallest first slice that proves the model\n\n"
	if !strings.HasPrefix(document.worldClass, "The pattern that works is **tuples as the internal log") ||
		!strings.HasSuffix(document.worldClass, "and a small set of named queries, each authorized per kind.") ||
		!strings.Contains(capture.Document.Markdown, worldClass) {
		t.Errorf("the first closing section is not the document's word for word: %q", document.worldClass)
	}
	if !strings.HasPrefix(document.firstSlice, "Two queries, one shared foundation;") ||
		!strings.HasSuffix(document.firstSlice, "neither cost a single breaking change to an agent.") ||
		!strings.HasSuffix(capture.Document.Markdown, "\n\n"+document.firstSlice+"\n") {
		t.Errorf("the second closing section is not the document's word for word: %q", document.firstSlice)
	}
	if want := capture.Verdict + "\n\n" + worldClass + document.firstSlice; document.lensVerdict() != want {
		t.Errorf("the lens's verdict reads %q, want the verdict and then both closing sections under their headings", document.lensVerdict())
	}

	t.Run("a section the document lacks is named", func(t *testing.T) {
		for _, missing := range []struct {
			cut   string
			names []string
		}{
			{"## Verdict (confidence: high)", []string{"## Verdict"}},
			{"## What a world-class implementation looks like", []string{"## What a world-class implementation looks like"}},
			{"- **Prevention:** additive only.", []string{"10", "**Prevention:**"}},
		} {
			_, err := parsePremortem(strings.Replace(capture.Document.Markdown, missing.cut, "Some prose.", 1))
			if err == nil || !graphstoretest.NamesAsAWord(err.Error(), missing.names...) {
				t.Errorf("the document without %q is refused %v, want it naming %q", missing.cut, err, missing.names)
			}
		}
	})
	t.Run("a heading inside a fenced code block is no heading", func(t *testing.T) {
		fenced := strings.Replace(capture.Document.Markdown, "## The ten ways it goes wrong",
			"```\n## Risk 11, a heading in code\n```\n\n## The ten ways it goes wrong", 1)
		read, err := parsePremortem(fenced)
		if err != nil || len(read.risks) != 11 {
			t.Fatalf("the document with a fenced heading reads %d risks (%v), want eleven", len(read.risks), err)
		}
	})
}

// premortemRig is the reasoning routes' issue (reasoningRig), its requirement r1 and its
// Verification document's checks k1 and k2, with a second issue of its project holding a
// Verification document of its own, and a pre-mortem document holding the capture's markdown on
// the first: what the conversion reads and points at.
type premortemRig struct {
	reasoningRig
	capture premortemCapture
	// otherIssue is the second issue's key.
	otherIssue string
	// slug is the pre-mortem's slug, refKey its store address, address its dispatch:// one and
	// documentID the id of its artifacts row.
	slug, refKey, address, documentID string
	// otherIssueCheck is a check of the second issue, a dispatch:// address, and each node id the
	// id a prevented_by tuple names its check by: the node its address resolves to, its issue's
	// room's node, every issue being born converted (nodeOf); otherIssueDocumentID is the id of the
	// artifacts row of the second issue's Verification document, which holds that check.
	otherIssueCheck, otherIssueDocumentID           string
	checkNode, secondCheckNode, otherIssueCheckNode string
}

// nodeOf is the node id the check at the dispatch:// address resolves to (graphstore.MovedTo: on
// an issue born converted, its room's node, `<tree ref_key>#<block>`), which the conversion's and
// the pointer route's prevented_by name it by.
func (r premortemRig) nodeOf(address string) string {
	r.t.Helper()
	_, id, ok := decodeNodeAddress(address)
	if !ok {
		r.t.Fatalf("%s decodes to no node", address)
	}
	node, resolved, err := graphstore.MovedTo(context.Background(), r.database.Pool, id)
	if err != nil || !resolved {
		r.t.Fatalf("resolve the check %s: resolved %t, %v", address, resolved, err)
	}
	return node.Address
}

func newPremortemRig(t *testing.T) premortemRig {
	t.Helper()
	return premortemRigOn(t, newReasoningRig(t))
}

// premortemRigOn is newPremortemRig over the reasoning rig reasoning, which the caller built.
func premortemRigOn(t *testing.T, reasoning reasoningRig) premortemRig {
	t.Helper()
	rig := premortemRig{reasoningRig: reasoning, capture: readPremortemCapture(t)}
	project := strings.Split(rig.issue, "-")[0]
	other := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "The issue whose check a risk names",
		"spec": ":::requirement{#r9}\nEvery node kind has a predicate.\n:::\n",
	}, "alice")
	if other.Code != http.StatusCreated {
		t.Fatalf("create the second issue: status=%d body=%s", other.Code, other.Body.String())
	}
	rig.otherIssue = decodeBody[struct {
		Key string `json:"key"`
	}](t, other).Key
	otherVerification := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.otherIssue, "verification.md",
		checkServing("k9", refs.BlockRef("dispatch://"+rig.otherIssue+"/spec", "r9")))
	rig.otherIssueCheck = refs.BlockRef("dispatch://"+rig.otherIssue+"/artifact/"+otherVerification.Artifact.Slug, "k9")
	rig.checkNode, rig.secondCheckNode, rig.otherIssueCheckNode = rig.nodeOf(rig.check), rig.nodeOf(rig.secondCheck), rig.nodeOf(rig.otherIssueCheck)
	rig.otherIssueDocumentID = otherVerification.Artifact.ID
	premortem := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "graph-premortem.md", rig.capture.Document.Markdown)
	rig.slug, rig.refKey, rig.documentID = premortem.Artifact.Slug, premortem.Artifact.RefKey, premortem.Artifact.ID
	rig.address = "dispatch://" + rig.issue + "/artifact/" + rig.slug
	return rig
}

// otherProjectCheck is a check of an issue of a second project, FAR, in a Verification document
// of its own: its dispatch:// address, and the id a prevented_by tuple would name it by.
func (r premortemRig) otherProjectCheck() (address, node string) {
	r.t.Helper()
	issue := createInteractionIssue(r.t, r.handler, "FAR", "Another project's work",
		":::requirement{#r7}\nEvery project keeps its own checks.\n:::\n")
	verification := uploadDocument(r.t, r.handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checkServing("k7", refs.BlockRef("dispatch://"+issue.Key+"/spec", "r7")))
	address = refs.BlockRef("dispatch://"+issue.Key+"/artifact/"+verification.Artifact.Slug, "k7")
	return address, r.nodeOf(address)
}

// premortemMap is a map of each of the document's eleven risks: risk 0 to both checks of the
// issue, risk 8 to the check of the second issue, and every other risk, risk 3 among them, to none.
func (r premortemRig) premortemMap() []PremortemEntry {
	entries := make([]PremortemEntry, 0, 11)
	for risk := range 11 {
		entry := PremortemEntry{Risk: risk, Checks: []string{}, Reason: fmt.Sprintf("No check of this tree covers risk %d.", risk)}
		switch risk {
		case 0:
			entry.Checks, entry.Reason = []string{r.check, r.secondCheck}, "Both checks fail a slice that ships nothing visible."
		case 8:
			entry.Checks, entry.Reason = []string{r.otherIssueCheck}, "The kind census fails a kind with no predicate."
		}
		entries = append(entries, entry)
	}
	return entries
}

// convert runs ConvertPremortem in a transaction of its own as the server's own credential, and
// commits it where it answered no error.
func (r premortemRig) convert(entries []PremortemEntry, dryRun bool) (PremortemReport, error) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		r.t.Fatalf("begin the conversion: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	report, events, err := ConvertPremortem(ctx, tx, PremortemConverter, PremortemConverter, r.issue, r.slug, entries, dryRun)
	if err != nil {
		return PremortemReport{}, err
	}
	retired := report.Lens.Address != nil && !report.AlreadyConverted
	if (!retired && len(events) != 0) || (retired && (len(events) != 1 || events[0].Type != model.DocumentRetiredEventType)) {
		r.t.Fatalf("the conversion answers the events %+v beside %+v, want the one document.retired of its retirement, and none where it retired nothing",
			events, report)
	}
	if err := tx.Commit(ctx); err != nil {
		r.t.Fatalf("commit the conversion: %v", err)
	}
	return report, nil
}

// storeCounts is every node and every tuple the store holds, which a conversion that writes nothing
// leaves as it found them.
func (r premortemRig) storeCounts() [2]int {
	r.t.Helper()
	var counts [2]int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select (select count(*) from graph_nodes), (select count(*) from graph_tuples)`).Scan(&counts[0], &counts[1]); err != nil {
		r.t.Fatalf("count the store's nodes and tuples: %v", err)
	}
	return counts
}

// retirements is the reason of each document.retired event of the document artifactID, in order.
func (r premortemRig) retirements(artifactID string) []string {
	r.t.Helper()
	reasons, err := readStrings(context.Background(), r.database.Pool, `
		select payload->>'reason' from events where type = $1 and payload->>'artifact_id' = $2 order by id
	`, model.DocumentRetiredEventType, artifactID)
	if err != nil {
		r.t.Fatalf("read the document.retired events of %s: %v", artifactID, err)
	}
	return reasons
}

// TestConvertPremortemWritesTheLensItsRisksAndTheirChecksOnce converts the captured pre-mortem.
// Every refusal of the map writes nothing; a dry run answers the lens, the eleven risks and their
// checks and writes nothing, the retirement among it; the conversion writes one lens of kind
// pre-mortem under the issue carrying the verdict and both closing sections, eleven risks flat
// under it in the order of their numbers with their headings, early warnings and preventions, the
// prevented_by tuples the map names - a check of another issue among them, and none for risk 3 -
// and one moved_to tuple from the document's address to the lens, and retires the document: it
// leaves the issue's document list, refuses new text 409 DOCUMENT_RETIRED naming its slug and its
// id, and holds one document.retired event whose reason names the lens. Its old address then
// answers the lens through GET /api/v1/references?to=, the closure of the issue whose comment
// cites that address holds the lens as its member, and the store census reads no difference and
// no unresolved far end. A second call finds that lens and writes nothing.
func TestConvertPremortemWritesTheLensItsRisksAndTheirChecksOnce(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	document, err := parsePremortem(rig.capture.Document.Markdown)
	if err != nil {
		t.Fatalf("parse the capture: %v", err)
	}
	before := rig.storeCounts()

	t.Run("each refusal of the map names its entry and writes nothing", func(t *testing.T) {
		withEntry := func(change func([]PremortemEntry) []PremortemEntry) []PremortemEntry {
			return change(rig.premortemMap())
		}
		unheld := "dispatch://" + rig.issue + "/artifact/verification-md#b-k404"
		// Each refusal is told by its entry and risk, and by what it names: the risks the document
		// holds, the entry that named a risk first, the check, the field. The refusal of a risk no
		// entry names is entry 0.
		for _, refused := range []struct {
			name        string
			entries     []PremortemEntry
			entry, risk int
			names       []string
		}{
			{"a risk the document does not hold", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				return append(entries, PremortemEntry{Risk: 11, Checks: []string{}, Reason: "A risk the document lacks."})
			}), 12, 11, []string{"11", "0", "10"}},
			{"a number twice", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				return append(entries, PremortemEntry{Risk: 4, Checks: []string{}, Reason: "Risk 4 again."})
			}), 12, 4, []string{"4", "5"}},
			{"a risk with no entry", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				return slices.Delete(entries, 10, 11)
			}), 0, 10, nil},
			{"an address that names no block", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				entries[2].Checks = []string{unheld}
				return entries
			}), 3, 2, []string{unheld}},
			{"a block that is no check", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				entries[5].Checks = []string{rig.requirement}
				return entries
			}), 6, 5, []string{rig.requirement}},
			{"a blank reason", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				entries[7].Reason = " "
				return entries
			}), 8, 7, []string{"reason"}},
			{"a check named twice", withEntry(func(entries []PremortemEntry) []PremortemEntry {
				entries[0].Checks = []string{rig.check, rig.check}
				return entries
			}), 1, 0, []string{rig.check}},
		} {
			_, err := rig.convert(refused.entries, false)
			t.Logf("%s: %v", refused.name, err)
			var mapError *PremortemMapError
			if !errors.As(err, &mapError) || mapError.Entry != refused.entry || mapError.Risk != refused.risk ||
				!graphstoretest.NamesAsAWord(mapError.Problem, refused.names...) {
				t.Errorf("%s is refused %v, want entry %d, risk %d, naming %q", refused.name, err, refused.entry, refused.risk, refused.names)
			}
			if after := rig.storeCounts(); after != before {
				t.Errorf("%s left %v nodes and tuples, want the %v it found", refused.name, after, before)
			}
		}
	})

	t.Run("a dry run answers what it would write and writes nothing", func(t *testing.T) {
		report, err := rig.convert(rig.premortemMap(), true)
		if err != nil {
			t.Fatalf("the dry run: %v", err)
		}
		t.Logf("the dry run: %s", report.Summary)
		if !report.DryRun || report.AlreadyConverted || report.Lens.Address != nil || report.Document.MovedTo != nil ||
			report.Document.Address != rig.address || report.Lens.Kind != graphstore.LensKindPreMortem ||
			report.Lens.Verdict == nil || *report.Lens.Verdict != document.lensVerdict() || len(report.Risks) != 11 {
			t.Fatalf("the dry run answers %+v", report)
		}
		if got := report.Risks[0].PreventedBy; !slices.Equal(got, []string{rig.check, rig.secondCheck}) {
			t.Errorf("the dry run's risk 0 is prevented by %v, want both checks", got)
		}
		if got := report.Risks[8].PreventedBy; !slices.Equal(got, []string{rig.otherIssueCheck}) {
			t.Errorf("the dry run's risk 8 is prevented by %v, want the second issue's check", got)
		}
		for _, risk := range report.Risks {
			if risk.Address != nil {
				t.Errorf("the dry run names risk %d at %s, an address nothing holds", risk.Number, *risk.Address)
			}
		}
		if after := rig.storeCounts(); after != before {
			t.Errorf("the dry run left %v nodes and tuples, want the %v it found", after, before)
		}
		if retirements := rig.retirements(report.Document.ID); len(retirements) != 0 ||
			!slices.Contains(issueDocumentIDs(t, rig.handler, rig.issue), report.Document.ID) {
			t.Errorf("after the dry run the document is listed %t and holds the document.retired events %v, want it listed and none",
				slices.Contains(issueDocumentIDs(t, rig.handler, rig.issue), report.Document.ID), retirements)
		}
	})

	createReferenceAPIComment(t, rig.handler, rig.otherIssue, "As "+rig.address+" warns.")
	report, err := rig.convert(rig.premortemMap(), false)
	if err != nil {
		t.Fatalf("convert the pre-mortem: %v", err)
	}
	t.Logf("the conversion: %s", report.Summary)
	if report.DryRun || report.AlreadyConverted || report.Lens.Address == nil || report.Document.MovedTo == nil ||
		*report.Document.MovedTo != *report.Lens.Address || len(report.Risks) != 11 {
		t.Fatalf("the conversion answers %+v", report)
	}
	lens := strings.TrimPrefix(*report.Lens.Address, "dispatch://"+graphstore.KindLens+"/")
	var verdict, visibility, state string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select attributes->>'`+graphstore.AttributeVerdict+`', visibility, state from graph_nodes where kind = $1 and id = $2
	`, graphstore.KindLens, lens).Scan(&verdict, &visibility, &state); err != nil {
		t.Fatalf("read the lens %s: %v", lens, err)
	}
	if verdict != document.lensVerdict() || !strings.HasPrefix(verdict, rig.capture.Verdict+"\n\n") ||
		!strings.Contains(verdict, "## What a world-class implementation looks like") ||
		!strings.Contains(verdict, "## The smallest first slice that proves the model") {
		t.Errorf("the lens's verdict reads %q, want the verdict and both closing sections", verdict)
	}
	if visibility != graphstore.VisibilityProject || state != graphstore.StateLive {
		t.Errorf("the lens is %s and %s, want project and live", visibility, state)
	}
	if got := rig.current(graphstore.KindLens, lens, graphstore.TupleHasKind); !slices.Equal(got, []string{"lens_kind/" + graphstore.LensKindPreMortem}) {
		t.Errorf("the lens has kind %v, want lens_kind/pre-mortem", got)
	}
	if got := rig.current(graphstore.KindLens, lens, graphstore.TuplePartOf); !slices.Equal(got, []string{"issue/" + rig.issue}) {
		t.Errorf("the lens stands under %v, want the issue", got)
	}

	risks, err := readStrings(context.Background(), rig.database.Pool, `
		select from_id from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3 and from_kind = $4 order by seq
	`, graphstore.TuplePartOf, graphstore.KindLens, lens, graphstore.KindRisk)
	if err != nil {
		t.Fatalf("read the risks under the lens: %v", err)
	}
	if len(risks) != 11 {
		t.Fatalf("%d risks stand under the lens, want eleven", len(risks))
	}
	wantChecks := map[int][]string{
		0: {"block/" + rig.checkNode, "block/" + rig.secondCheckNode},
		8: {"block/" + rig.otherIssueCheckNode},
	}
	for index, id := range risks {
		parsed, answered := document.risks[index], report.Risks[index]
		if answered.Address == nil || *answered.Address != "dispatch://"+graphstore.KindRisk+"/"+id || answered.Number != index {
			t.Errorf("risk %d stands under the lens at %s, and the report names %+v", index, id, answered)
		}
		var heading, earlyWarning, prevention string
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select attributes->>'`+graphstore.AttributeHeading+`', attributes->>'`+graphstore.AttributeEarlyWarning+`',
			       attributes->>'`+graphstore.AttributePrevention+`'
			from graph_nodes where kind = $1 and id = $2
		`, graphstore.KindRisk, id).Scan(&heading, &earlyWarning, &prevention); err != nil {
			t.Fatalf("read risk %d: %v", index, err)
		}
		if heading != rig.capture.Risks[index].Title || earlyWarning != parsed.earlyWarning || prevention != parsed.prevention {
			t.Errorf("risk %d holds %q, %q, %q, want its section's heading, early warning and prevention", index, heading, earlyWarning, prevention)
		}
		want := wantChecks[index]
		if want == nil {
			want = []string{}
		}
		slices.Sort(want)
		if got := rig.current(graphstore.KindRisk, id, graphstore.TuplePreventedBy); !slices.Equal(got, want) {
			t.Errorf("risk %d is prevented by %v, want %v", index, got, want)
		}
		if got := rig.current(graphstore.KindRisk, id, graphstore.TuplePartOf); !slices.Equal(got, []string{"lens/" + lens}) {
			t.Errorf("risk %d stands under %v, want the lens alone", index, got)
		}
	}
	if got := rig.current(graphstore.KindArtifact, rig.refKey, graphstore.TupleMovedTo); !slices.Equal(got, []string{"lens/" + lens}) {
		t.Errorf("the document's address moved to %v, want the lens", got)
	}

	if ids := issueDocumentIDs(t, rig.handler, rig.issue); slices.Contains(ids, report.Document.ID) {
		t.Errorf("the issue's document list is %v, which holds the converted document %s", ids, report.Document.ID)
	}
	ops := map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "More."}}, "summary": "edit"}
	requireRefusedNaming(t, "an edit of the converted document", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+rig.issue+"/artifacts/"+rig.slug+"/edits", ops, "alice"), http.StatusConflict, "DOCUMENT_RETIRED", rig.slug, report.Document.ID)
	if retirements, want := rig.retirements(report.Document.ID), "converted into the pre-mortem lens "+*report.Lens.Address; len(retirements) != 1 || retirements[0] != want {
		t.Errorf("the converted document holds the document.retired events %q, want one whose reason is %q", retirements, want)
	}
	redirected := dispatchReferences(t, rig.handler, rig.address, "alice")
	if redirected.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references?to=%s: status=%d body=%s", rig.address, redirected.Code, redirected.Body.String())
	}
	if node := decodeBody[model.GraphReferences](t, redirected).Node; node.Kind != graphstore.KindLens || node.ID != lens || node.Ref != *report.Lens.Address {
		t.Errorf("GET /api/v1/references?to=%s names %s %s (%s), want the lens %s", rig.address, node.Kind, node.ID, node.Ref, *report.Lens.Address)
	}
	closure := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.otherIssue+"/references", nil, "alice")
	if closure.Code != http.StatusOK {
		t.Fatalf("GET the closure of %s: status=%d body=%s", rig.otherIssue, closure.Code, closure.Body.String())
	}
	if members := decodeBody[model.IssueReferences](t, closure).Members; len(members) != 1 || members[0].Kind != graphstore.KindLens ||
		members[0].Address != *report.Lens.Address || members[0].Artifact.ID != report.Document.ID {
		t.Errorf("the closure of %s, whose comment cites %s, holds %+v; want the lens %s, the retired document beside it",
			rig.otherIssue, rig.address, members, *report.Lens.Address)
	}
	requireTheCensusAgreesOver(t, rig.database, "after the conversion")

	converted := rig.storeCounts()
	again, err := rig.convert(rig.premortemMap(), false)
	if err != nil {
		t.Fatalf("convert the pre-mortem again: %v", err)
	}
	t.Logf("the second call: %s", again.Summary)
	if !again.AlreadyConverted || again.Lens.Address == nil || *again.Lens.Address != *report.Lens.Address || len(again.Risks) != 0 {
		t.Errorf("the second call answers %+v, want the document already converted into %s", again, *report.Lens.Address)
	}
	if after := rig.storeCounts(); after != converted {
		t.Errorf("the second call left %v nodes and tuples, want the %v the conversion left", after, converted)
	}
}

// TestThePremortemRouteRefusesAnEntryNamingItsPosition calls the route with the eleven entries and
// one more for risk 11, which the document does not hold: 400 ARTIFACT_INPUT naming prevented_by,
// that entry by its position and its risk, and nothing written. An entry of the wrong type is
// refused naming its position and the member of the wrong type, an entry that leaves out a member
// naming its position, its risk and that member, and a body that is not JSON keeps 400
// INVALID_JSON. With the eleven entries and dry_run it answers the report and writes nothing.
func TestThePremortemRouteRefusesAnEntryNamingItsPosition(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	path := "/api/v1/issues/" + rig.issue + "/artifacts/" + rig.slug + "/premortem-convert"
	before := rig.storeCounts()

	withRisk11 := append(rig.premortemMap(), PremortemEntry{Risk: 11, Checks: []string{}, Reason: "A risk the document lacks."})
	requireRefusedNaming(t, "an entry for risk 11", dispatchRequest(t, rig.handler, http.MethodPost, path,
		map[string]any{"prevented_by": withRisk11}, "alice"),
		http.StatusBadRequest, "ARTIFACT_INPUT", "prevented_by", "12", "11")
	// Every refusal of an entry's shape holds the shape (premortemEntryShape), which names every
	// member, so each test below reads the member the refusal names outside the shape: a mistyped
	// member after it, a member left out before it.
	mistyped := []any{map[string]any{"risk": 0, "checks": []string{}, "reason": "None."}, map[string]any{"risk": "x", "checks": []string{}, "reason": "None."}}
	wrongType := requireRefusedNaming(t, "an entry whose risk is no number", dispatchRequest(t, rig.handler, http.MethodPost, path,
		map[string]any{"prevented_by": mistyped}, "alice"),
		http.StatusBadRequest, "ARTIFACT_INPUT", "prevented_by", "2", "risk")
	if named, cause, found := strings.Cut(wrongType.Error, premortemEntryShape); !found ||
		!graphstoretest.NamesAsAWord(named, "2") || !graphstoretest.NamesAsAWord(cause, "risk") {
		t.Fatalf("an entry whose risk is no number is refused %q, want the entry 2 named before the entry's shape and the member risk after it", wrongType.Error)
	}
	// An entry without its checks is refused naming the entry by its position and its risk, and the
	// member it leaves out.
	missing := requireRefusedNaming(t, "an entry without its checks", dispatchRequest(t, rig.handler, http.MethodPost, path,
		map[string]any{"prevented_by": []any{map[string]any{"risk": 3, "reason": "None."}}}, "alice"),
		http.StatusBadRequest, "ARTIFACT_INPUT", "prevented_by", "1", "3", "checks")
	if named, _, found := strings.Cut(missing.Error, premortemEntryShape); !found || !graphstoretest.NamesAsAWord(named, "1", "3", "checks") {
		t.Fatalf("an entry without its checks is refused %q, want the entry 1, its risk 3 and the member checks named before the entry's shape", missing.Error)
	}
	notJSON := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prevented_by": [`))
	notJSON.Header.Set("Content-Type", "application/json")
	notJSON.Header.Set("X-Dispatch-User", "alice")
	answered := httptest.NewRecorder()
	rig.handler.ServeHTTP(answered, notJSON)
	requireRefusedNaming(t, "a body that is not JSON", answered, http.StatusBadRequest, "INVALID_JSON")
	if after := rig.storeCounts(); after != before {
		t.Errorf("the refused calls left %v nodes and tuples, want the %v they found", after, before)
	}

	previewed := dispatchRequest(t, rig.handler, http.MethodPost, path,
		map[string]any{"prevented_by": rig.premortemMap(), "dry_run": true}, "alice")
	if previewed.Code != http.StatusOK {
		t.Fatalf("the dry run: status=%d body=%s", previewed.Code, previewed.Body.String())
	}
	report := decodeBody[PremortemReport](t, previewed)
	if !report.DryRun || len(report.Risks) != 11 || report.Lens.Address != nil ||
		!slices.Equal(report.Risks[8].PreventedBy, []string{rig.otherIssueCheck}) || len(report.Risks[3].PreventedBy) != 0 {
		t.Errorf("the dry run answers %+v", report)
	}
	if after := rig.storeCounts(); after != before {
		t.Errorf("the dry run left %v nodes and tuples, want the %v it found", after, before)
	}
}

// TestAConversionMapNamingACheckOfAnotherProjectWritesNothing maps risk 5 to a check of another
// project than the issue's (spec requirement r-premortem-nodes): the conversion is
// refused, in a dry run as in a real one, through the function and through its route, with 400
// INVALID_POINTER naming the entry, risk 5, the check by its address and both projects, and the
// store holds no node or tuple of it - no lens, no risk, no prevented_by, no moved_to - and the
// document stays listed and unretired.
func TestAConversionMapNamingACheckOfAnotherProjectWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	foreign, _ := rig.otherProjectCheck()
	entries := rig.premortemMap()
	entries[5].Checks, entries[5].Reason = []string{foreign}, "A check of another project."
	names := []string{graphstore.TuplePreventedBy, foreign, "RSN", "FAR"}
	path := "/api/v1/issues/" + rig.issue + "/artifacts/" + rig.slug + "/premortem-convert"
	before := rig.storeCounts()
	unchanged := func(name string) {
		t.Helper()
		if after := rig.storeCounts(); after != before {
			t.Errorf("%s left %v nodes and tuples, want the %v it found", name, after, before)
		}
		if moved := rig.current(graphstore.KindArtifact, rig.refKey, graphstore.TupleMovedTo); len(moved) != 0 {
			t.Errorf("after %s the document's address moved to %v, want nowhere", name, moved)
		}
		if retirements := rig.retirements(rig.documentID); len(retirements) != 0 ||
			!slices.Contains(issueDocumentIDs(t, rig.handler, rig.issue), rig.documentID) {
			t.Errorf("after %s the document holds the document.retired events %v, or left the issue's list; want it listed and none", name, retirements)
		}
	}

	for _, dryRun := range []bool{true, false} {
		name := fmt.Sprintf("the conversion with dry run %t", dryRun)
		_, err := rig.convert(entries, dryRun)
		t.Logf("%s: %v", name, err)
		var mapError *PremortemMapError
		if !errors.As(err, &mapError) || !errors.Is(err, graphstore.ErrInvalidPointer) || mapError.Entry != 6 || mapError.Risk != 5 ||
			!graphstoretest.NamesAsAWord(err.Error(), names...) {
			t.Errorf("%s is refused %v, want the store's refusal of entry 6, risk 5, naming %q", name, err, names)
		}
		unchanged(name)

		name = fmt.Sprintf("the route with dry_run %t", dryRun)
		requireRefusedNaming(t, name, dispatchRequest(t, rig.handler, http.MethodPost, path,
			map[string]any{"prevented_by": entries, "dry_run": dryRun}, "alice"), http.StatusBadRequest, "INVALID_POINTER",
			append([]string{"6", "5"}, names...)...)
		unchanged(name)
	}
}
