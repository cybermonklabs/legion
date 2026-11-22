package graphcensus_test

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
)

// The level the repair issue's drive imports, in the form of level one: the statement
// a-bill-is-sent, which its notes give a kind, the statement a-bill-has-no-owner, which they give
// none, so the import holds it, and the component billing; and the repository it is the level of.
const (
	sizedLevelRepo = "acme/level"
	sizedLevel     = "# A sized level\n\n## Statements\n\n### Billing\n\n" +
		":::requirement{#a-bill-is-sent}\nA bill is sent.\n:::\n\n" +
		":::requirement{#a-bill-has-no-owner}\nA bill has no owner of its own.\n:::\n\n" +
		"## Components\n\n| Id | Kind | Name | Purpose | Parent or joins |\n| --- | --- | --- | --- | --- |\n" +
		"| billing | component | Billing | It bills each order. | none |\n"
	sizedLevelNotes = "# Notes of the sized level\n\n" +
		"### a-bill-is-sent\n\n- Kind: behavior\n- About: billing\n- Supports: none\n\n" +
		"### a-bill-has-no-owner\n\n- About: billing\n- Supports: none\n"
)

// specImportDrives is the drive of the repair issue of a level's holds (api's repairIssueHook),
// over the rig the drives before it wrote. Its Ready writes alice's level of SIZE in acme/level
// with its notes, and her approval of it before acme/level is a spec source of SIZE, as level one
// was approved, which writes nothing of the approved spec, and then names acme/level a spec source.
// Its drive is the level's import (POST /api/v1/issues/{key}/spec-import-level), which writes the
// level into the approved spec (specstore.Import) and opens the child issue that repairs the
// statement it held, through the issue route's own create, whose hook names the child on the hold.
func specImportDrives(t *testing.T, r *writersRig) []graphcensus.StoreDrive {
	t.Helper()
	var level string
	return []graphcensus.StoreDrive{{
		// The import's first versions: a-bill-is-sent's, which the terms spec_node and spec_version
		// count, and billing's, which spec_component counts; its hold of a-bill-has-no-owner, which
		// spec_hold counts; the child issue's create, as a create of an issue born converted, beside
		// its child_of the level, which the term issue_relation counts, and the reference its spec
		// and the block that names the level make to it, which document_ref and block_ref count; and
		// the hook's repair_issue on the hold, which spec_repair_issue counts.
		Name: "api/spec_import.go#repairIssueHook", Database: r.database,
		Sites: []string{"api/spec_import.go#repairIssueHook", "specstore/import.go#Import", "api/issue_create.go#createIssueWith",
			"api/artifacts.go#createIssueTree", "api/issue_create.go#createDecisionsQuery", "docs/mutation.go#WriteVersionNode",
			"refs/blocks.go#Queue"},
		Ready: func(t *testing.T) {
			created := decoded[struct {
				Key     string `json:"key"`
				Primary string `json:"primary_artifact_id"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "A sized level", "spec": sizedLevel, "repo": sizedLevelRepo, "force": true,
			}, "alice", http.StatusCreated))
			level = created.Key
			r.request(t, http.MethodPost, "/api/v1/issues/"+level+"/artifacts", map[string]any{
				"name": "notes.md", "content": sizedLevelNotes,
			}, "alice", http.StatusCreated)
			ask := decoded[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, r.request(t, http.MethodPost, "/api/v1/artifacts/"+created.Primary+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_leveller")}, "", http.StatusCreated)).Ask.ID
			r.request(t, http.MethodPost, "/api/v1/asks/"+ask+"/answer",
				map[string]any{"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": nil}, "alice", http.StatusOK)
			r.request(t, http.MethodPut, "/api/v1/projects/SIZE/spec-sources/"+sizedLevelRepo, map[string]any{"branch": "main"},
				"alice", http.StatusOK)
		},
		Drive: func(t *testing.T) {
			imported := decoded[struct {
				RepairIssue *string `json:"repair_issue"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+level+"/spec-import-level",
				map[string]any{"actor": sessionActor("ses_leveller"), "notes": "notes.md"}, "", http.StatusOK))
			if imported.RepairIssue == nil {
				t.Fatalf("the import of %s opened no repair issue for the statement it held", level)
			}
		},
		Counts: merged(newBlockCounted, blockValuesCounted, roomBlockCounted, objectNodeCounted(graphstore.KindIssue),
			objectNodeCounted(graphstore.KindArtifact), documentPartOfCounted, versionCounted, issueRoomCounted, queryNodeCounted,
			queryVersionCounted, map[graphcensus.StoredKind][]string{
				{graphstore.KindIssue, graphstore.TupleChildOf}:      {"issue_relation"},
				{graphstore.KindArtifact, graphstore.TupleMentions}:  {"document_ref"},
				{refs.BlockNodeKind, graphstore.TupleMentions}:       {"block_ref"},
				{specstore.KindNode, graphstore.AttributeVisibility}: {"spec_node", "spec_component"},
				{specstore.KindNode, graphstore.AttributeState}:      {"spec_node", "spec_component"},
				{specstore.KindNode, graphstore.AttributeProject}:    {"spec_node", "spec_component"},
				{specstore.KindNode, specstore.AttributeVersion}:     {"spec_version", "spec_component"},
				{specstore.KindHold, graphstore.AttributeVisibility}: {"spec_hold"},
				{specstore.KindHold, graphstore.AttributeState}:      {"spec_hold"},
				{specstore.KindHold, graphstore.AttributeProject}:    {"spec_hold"},
				{specstore.KindHold, specstore.AttributeHold}:        {"spec_hold"},
				{specstore.KindHold, specstore.AttributeRepairIssue}: {"spec_repair_issue"},
			}),
	}}
}
