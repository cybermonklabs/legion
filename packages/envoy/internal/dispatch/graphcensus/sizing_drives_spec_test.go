package graphcensus

import (
	"context"
	"fmt"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The drives of the two writers of the approved spec of a spec source (specstore), and the
// project and repository whose approved spec they write.
const (
	specImportDrive   = "specstore/import.go#Import"
	specApprovalDrive = "specstore/apply.go#Apply"
	specProject       = "SPEC"
	specRepo          = "drive/spec"
)

// specNodeCounted is what a statement's or a check's first version appends of its spec_node,
// which the term spec_node counts: its visibility, its state and its project. Each version beside
// it is a value the term spec_version counts.
var specNodeCounted = map[StoredKind][]string{
	{specstore.KindNode, graphstore.AttributeVisibility}: {"spec_node"},
	{specstore.KindNode, graphstore.AttributeState}:      {"spec_node"},
	{specstore.KindNode, graphstore.AttributeProject}:    {"spec_node"},
	{specstore.KindNode, specstore.AttributeVersion}:     {"spec_version"},
}

// specLevelCounted is what an import of a level appends of the first versions it writes: a
// statement's, as specNodeCounted, and a component's or an interface's, its node's three base
// attributes and its version, which the term spec_component counts.
var specLevelCounted = map[StoredKind][]string{
	{specstore.KindNode, graphstore.AttributeVisibility}: {"spec_node", "spec_component"},
	{specstore.KindNode, graphstore.AttributeState}:      {"spec_node", "spec_component"},
	{specstore.KindNode, graphstore.AttributeProject}:    {"spec_node", "spec_component"},
	{specstore.KindNode, specstore.AttributeVersion}:     {"spec_version", "spec_component"},
}

// specCounted is counted, a drive's first versions, with the kinds it appends beyond them.
func specCounted(counted, beyond map[StoredKind][]string) map[StoredKind][]string {
	merged := maps.Clone(counted)
	maps.Copy(merged, beyond)
	return merged
}

// specRequestBlock is the requirement block id of a change request of issue, about billing, with
// attributes beside its kind, holding text, as a change request's spec holds it.
func specRequestBlock(issue, id, attributes, text string) specchange.Block {
	return specchange.Block{
		Address: issue + "/spec#" + id,
		Text:    ":::requirement{#" + id + " kind=\"behavior\" about=\"billing\"" + attributes + "}\n" + text + "\n:::\n",
	}
}

// specDrives are the drives of the writers of the approved spec of drive/spec in SPEC, over a
// database of their own, each in a transaction of its own that runs its write at once, as an
// approval's write phase runs what it queued (specstore.RunNow): the import of an approved level
// (specstore.Import), which writes the component billing and the statements a-bill-is-sent and
// a-bill-is-kept, each as its node's first version, and holds a-bill-is-owned, which gives no kind;
// and then an approval of a change request (specstore.Apply), which writes a second version of
// a-bill-is-sent, removes a-bill-is-kept, and writes a-bill-is-owned with its kind, clearing its
// hold.
func specDrives(t *testing.T) []StoreDrive {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	return []StoreDrive{{
		// The import: billing's first version, which the term spec_component counts, the two
		// statements', which spec_node and spec_version count, and the hold's node with its
		// visibility, state and project and its hold value, which the term spec_hold counts.
		Name: specImportDrive, Database: database, Sites: []string{specImportDrive},
		Drive: func(t *testing.T) {
			drivenTx(t, database, "import the level of drive/spec", func(tx pgx.Tx) error {
				const level = "<level>"
				imported, err := specstore.Import(ctx, tx, specstore.ImportInput{
					Project: specProject, Repo: specRepo, Issue: level, Who: drivenAuthor,
					Provenance: specstore.Provenance{Act: specstore.ActImport, Issue: level, Person: drivenAuthor},
					Level: specstore.Level{
						Statements: []specstore.LevelStatement{
							{ID: "a-bill-is-sent", Text: "A bill is sent.", Kind: "behavior", About: []string{"billing"}, Noted: true},
							{ID: "a-bill-is-kept", Text: "A bill is kept.", Kind: "behavior", About: []string{"billing"}, Noted: true},
							{ID: "a-bill-is-owned", Text: "A bill is owned."},
						},
						Components: []specgraph.Component{{ID: "billing", Name: "Billing", Purpose: "It bills each order."}},
					},
				})
				if err != nil {
					return err
				}
				if len(imported.Written) != 3 || len(imported.Held) != 1 {
					return fmt.Errorf("the import wrote %+v and held %+v, want billing and two statements written and a-bill-is-owned held",
						imported.Written, imported.Held)
				}
				return nil
			})
		},
		Counts: specCounted(specLevelCounted, map[StoredKind][]string{
			{specstore.KindHold, graphstore.AttributeVisibility}: {"spec_hold"},
			{specstore.KindHold, graphstore.AttributeState}:      {"spec_hold"},
			{specstore.KindHold, graphstore.AttributeProject}:    {"spec_hold"},
			{specstore.KindHold, specstore.AttributeHold}:        {"spec_hold"},
		}),
	}, {
		// The approval: a-bill-is-owned's first version, a-bill-is-sent's second, which the term
		// spec_version counts, a-bill-is-kept's removed value, which spec_removal counts, and the hold
		// of a-bill-is-owned stated gone, which spec_hold_cleared counts.
		Name: specApprovalDrive, Database: database, Sites: []string{specApprovalDrive},
		Drive: func(t *testing.T) {
			drivenTx(t, database, "approve the change request of drive/spec", func(tx pgx.Tx) error {
				const issue = "<request>"
				applied, err := specstore.Apply(ctx, tx, specstore.Input{
					Project: specProject, Repo: specRepo, Issue: issue, ChecksIncluded: true, Who: drivenAuthor,
					Provenance: specstore.Provenance{Act: specstore.ActApproval, Issue: issue, Person: drivenAuthor},
					Request: specchange.FromBlocks(issue, []specchange.Block{
						specRequestBlock(issue, "a-bill-is-sent", ` base="1"`, "A bill is sent once."),
						specRequestBlock(issue, "a-bill-is-kept", ` base="1" removes="true"`, "A bill is kept."),
						specRequestBlock(issue, "a-bill-is-owned", "", "A bill is owned."),
					}),
				}, specstore.RunNow(ctx, tx))
				if err != nil {
					return err
				}
				if len(applied.Written) != 3 {
					return fmt.Errorf("the approval wrote %+v, want a-bill-is-sent, a-bill-is-kept's removal and a-bill-is-owned", applied.Written)
				}
				return nil
			})
		},
		Counts: specCounted(specNodeCounted, map[StoredKind][]string{
			{specstore.KindNode, specstore.AttributeRemoved}: {"spec_removal"},
			{specstore.KindHold, graphstore.AttributeState}:  {"spec_hold_cleared"},
		}),
	}}
}
