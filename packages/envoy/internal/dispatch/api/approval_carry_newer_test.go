package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newerAskAttribute is an attribute of the ask block that this build's manifest does not
// declare and the newer build's manifest (useNewerAskManifest) does.
const newerAskAttribute = "test_newer_attr"

// useNewerAskManifest makes this process read and write by the embedded manifest with one more
// attribute on the ask block, newerAskAttribute, as a newer build does, until the test ends.
// The manifest is package-global, so no other build of this test may run pmdoc while it is
// installed or restored.
func useNewerAskManifest(t *testing.T) {
	t.Helper()
	t.Cleanup(installNewerAskManifest(t))
}

// installNewerAskManifest installs the newer build's manifest (useNewerAskManifest) and answers
// the function that restores this build's.
func installNewerAskManifest(t *testing.T) func() {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	for _, typ := range manifest["types"].([]any) {
		block := typ.(map[string]any)
		if block["name"] == "ask" {
			block["attributes"].(map[string]any)[newerAskAttribute] = map[string]any{"kind": "string"}
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
	restore, err := pmdoc.UseManifestForTest(encoded)
	if err != nil {
		t.Fatalf("install the manifest: %v", err)
	}
	return restore
}

// newerRendering is markdown as the newer build renders it with newerAskAttribute set to value on
// the ask block blockID: the bytes that build stores for a version whose block holds the attribute.
func newerRendering(t *testing.T, markdown, blockID, value string) string {
	t.Helper()
	restore := installNewerAskManifest(t)
	defer restore()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	held := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && id == blockID {
			node.Attrs[newerAskAttribute] = value
			held = true
		}
		return true
	})
	if !held {
		t.Fatalf("%q holds no ask block %q", markdown, blockID)
	}
	rendered, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render %q with the newer attribute: %v", markdown, err)
	}
	return rendered
}

// newerBuildDocuments is a second Dispatch process on database, with its own document service and
// routes, as a newer build serving the same database during a release runs. Its document service
// admits a browser's socket (connectBrowserPeer) as the first process's does.
func newerBuildDocuments(t *testing.T, database *store.Store, settle time.Duration) (http.Handler, *docs.Service) {
	t.Helper()
	service := docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: settle})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs:        service,
		ServerURL:   "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new API dependencies of the newer build: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux, service
}

// Two builds serve one database during a release, and the version a human approved can hold an
// element only the newer one knows: here an attribute on the ask block, which the newer
// build's editor keeps on the block. While two builds serve, his answer is written into the block
// for the release before's pages (docs' rollWindowAskAttributes), so settlement writes a version
// of it, whose approval the attribute case would carry; but this build
// cannot read the approved version, so it cannot decide whether the approval carries. Its
// settlement writes no version, leaves his approval as it was and logs that a build that reads the
// element must write the version. The newer build then writes the version, with the attribute its
// editor keeps on the block, and carries his approval to it: the two differ in the block's
// server-owned attributes alone.
func TestAVersionWhoseApprovedVersionHoldsNewerContentIsWrittenByABuildThatReadsIt(t *testing.T) {
	logged := captureLog(t)
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	registerTwoBuilds(t, database)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "TEST", "Approved newer content", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	awaitVersionText(t, handler, issue.PrimaryArtifactID, "the block written in step with its open ask",
		contains(`:::ask{#region urgency="low" multiple="false" state="open"}`))
	awaitSettled(t, database, issue.PrimaryArtifactID)
	approved := latestVersion(t, database, issue.PrimaryArtifactID).Number
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", approved)

	// The newer build's rendering of the approved version: its ask block holds the attribute.
	written, err := database.Pool.Exec(ctx, `
		update artifact_versions set markdown = $3 where artifact_id = $1 and number = $2
	`, issue.PrimaryArtifactID, approved, newerRendering(t, versionMarkdown(t, handler, issue.PrimaryArtifactID, approved), "region", "first"))
	if err != nil || written.RowsAffected() != 1 {
		t.Fatalf("store the newer build's rendering of version %d: %v, %d rows; want one", approved, err, written.RowsAffected())
	}

	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	// The settlement's log line, read as it lands; the last reading is the whole log so far.
	refusal := testwait.UntilPolling(t, 10*time.Millisecond, "a settlement to meet the approved version's attribute", func() (string, bool) {
		log := logged.String()
		for line := range strings.Lines(log) {
			if strings.Contains(line, newerAskAttribute) {
				return line, true
			}
		}
		return log, false
	})
	if !strings.Contains(refusal, "no version written") || !strings.Contains(refusal, "a build that reads the element writes it") || !strings.Contains(refusal, fmt.Sprintf("version %d", approved)) {
		t.Fatalf("settlement log = %q, want it to say it wrote no version, that a build that reads the element writes it, and which version holds the element", refusal)
	}
	var latest int
	if err := database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, issue.PrimaryArtifactID).Scan(&latest); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	if latest != approved {
		t.Fatalf("this build wrote version %d over an approved version it cannot read, want no version past %d", latest, approved)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "approved")

	// The document still owes its settlement. The newer build's resumption arms a settlement owed
	// for a minute, so the row is aged past that.
	owed, err := database.Pool.Exec(ctx, `
		update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
	`, issue.PrimaryArtifactID)
	if err != nil || owed.RowsAffected() != 1 {
		t.Fatalf("age the owed settlement: %v, %d rows; want the document's one", err, owed.RowsAffected())
	}
	useNewerAskManifest(t)
	newer, service := newerBuildDocuments(t, database, 20*time.Millisecond)
	// The newer build's editor keeps the attribute on the live block, as the approved version
	// shows it.
	setLiveBlockAttributes(t, handler, service, issue.PrimaryArtifactID, "region", map[string]any{newerAskAttribute: "first"})
	leases := storetest.Leases(t, database.Pool)
	resumption, stop := context.WithCancel(ctx)
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		service.RunSettlementResumption(resumption, leases)
	}()
	t.Cleanup(func() {
		stop()
		testwait.Receive(t, "the settlement resumption's return once stopped", resumed)
	})
	waitForArtifactVersion(t, newer, issue.PrimaryArtifactID, approved+1)
	requireReviewCarriedWith(t, newer, issue.PrimaryArtifactID, approved+1, approved, attributeRemovalSentence(approved), nil)
}
