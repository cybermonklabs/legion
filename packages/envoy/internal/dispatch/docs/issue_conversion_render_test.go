package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// convertedSpec is the spec the converted issue of TestAnEditOfAConvertedSpecWritesOneVersionOfIt
// holds before its conversion: a paragraph and the requirement r1.
const convertedSpec = "The spec's opening paragraph.\n\n:::requirement{#r1}\nThe requirement.\n:::\n"

// requireTreeRendersAsStored fails unless each version of the document artifactID that renders from
// the tree - one whose version_renders row holds a time - renders, as of that time and as the
// server reads the store (RenderAt), as the markdown it stores, byte for byte, and answers how many
// such versions the document holds.
func requireTreeRendersAsStored(t *testing.T, database *store.Store, what, artifactID string) int {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select v.number, coalesce(v.markdown, ''), r.at
		from artifact_versions v
		join version_renders r on r.artifact_id = v.artifact_id and r.number = v.number
		where v.artifact_id = $1 and r.at is not null
		order by v.number`, artifactID)
	if err != nil {
		t.Fatalf("%s: read the versions of %s that render from the tree: %v", what, artifactID, err)
	}
	type rendered struct {
		number   int
		markdown string
		at       time.Time
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rendered, error) {
		var version rendered
		err := row.Scan(&version.number, &version.markdown, &version.at)
		return version, err
	})
	if err != nil {
		t.Fatalf("%s: read the versions of %s that render from the tree: %v", what, artifactID, err)
	}
	for _, version := range versions {
		got, err := RenderAt(ctx, database.Pool, model.ServerReader, artifactID, version.at)
		if err != nil {
			t.Fatalf("%s: render version %d of %s as of %s: %v", what, version.number, artifactID, version.at.Format(time.RFC3339Nano), err)
		}
		if got.Markdown != version.markdown {
			t.Errorf("%s: version %d of %s renders from the tree, as of the time its row records, as\n%s\nand stores\n%s\nwant the two equal, byte for byte",
				what, version.number, artifactID, got.Markdown, version.markdown)
		}
	}
	return len(versions)
}

// TestAnEditOfAConvertedSpecWritesOneVersionOfIt: the conversion of an issue leaves each block at
// its node in the room alone, at every time after it, so a version of a converted document renders
// from the tree as the bytes it stores, and an edit of the converted spec writes one version of it:
// the drain its settlement runs (DrainOwedQueries, planRevisions) finds the spec's render the
// markdown that version stores, and owes it nothing.
//
// The issue DOC-1 holds a spec of a paragraph and the requirement r1, written and indexed before
// its conversion. A person then moves r1 to proposed through the node route's write
// (graphstore.WriteState, as api's writeNodeState writes it), dated after the spec's version: the
// newest state the old node holds by its date. The conversion retires the spec's own block index
// (retireDocuments), which states each old node gone. Dated at the old version's time, that gone
// is older than the proposed state, so a read as of any later time (refs.TreeCut's Dated) takes the
// old node present beside its node in the room: the render of the spec prints r1 twice, the
// version the conversion wrote does not render as it stores, and the drain after the next edit
// writes that doubled rendering as a second version. Dated at the conversion's own time, the gone
// is the newest state by date and by seq alike.
//
// A peer then appends the requirement r2 to the spec's fragment of the room, and its settlement
// writes one version of the spec, holding r1 once, and no version of the decisions query, whose
// answer the edit did not move. A second edit appends the decision d1: its settlement writes one
// version of the spec again, and the drain writes the version of the decisions query that holds
// the decision's words (R15), since that edit moved the query's answer and wrote no version of it.
func TestAnEditOfAConvertedSpecWritesOneVersionOfIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	issue := createUnconvertedIssue(t, database, service, 1, "", convertedSpec, "The verification's paragraph.\n", "The plan's paragraph.\n")

	// The node route's write of a state, dated by the database's clock once the spec's version is
	// written.
	alice := model.Actor{Kind: "user", ID: "alice"}
	old := refs.BlockAddress(graphstore.ArtifactRefKey(issue.key, "DOC", "", "spec"), "r1")
	var proposedAt time.Time
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		node, held, err := graphstore.ReadNode(ctx, tx, refs.BlockNodeKind, old)
		if err != nil || !held {
			return fmt.Errorf("the requirement's node %s is not in the store (%t, %v)", old, held, err)
		}
		if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&proposedAt); err != nil {
			return err
		}
		_, err = graphstore.WriteState(ctx, tx, []graphstore.Node{node}, graphstore.StateProposed, proposedAt, alice)
		return err
	}); err != nil {
		t.Fatalf("write the requirement proposed: %v", err)
	}
	var versionedBefore bool
	if err := database.Pool.QueryRow(ctx, `select created_at < $2 from artifact_versions where artifact_id = $1 and number = 1`,
		issue.spec, proposedAt).Scan(&versionedBefore); err != nil || !versionedBefore {
		t.Fatalf("the spec's version 1 is written before the requirement's proposed state, dated %s: %t (%v); want it before, or this test proves nothing",
			proposedAt.Format(time.RFC3339Nano), versionedBefore, err)
	}

	result, err := service.ConvertIssue(ctx, issue.key, IssueRoomGate, false)
	if err != nil || result.Outcome != IssueConverted {
		t.Fatalf("convert %s: %+v, %v", issue.key, result, err)
	}
	tree, _ := treeOfIssue(t, database, issue.key)
	var decisions string
	if err := database.Pool.QueryRow(ctx, `select a.id::text from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a"),
		issue.key).Scan(&decisions); err != nil {
		t.Fatalf("read the decisions query of %s: %v", issue.key, err)
	}
	converted := savedVersion(t, database, issue.spec)
	if converted != 2 {
		t.Fatalf("the conversion left version %d of the spec newest, want its own version 2", converted)
	}
	if rendered := requireTreeRendersAsStored(t, database, "the conversion", issue.spec); rendered != 1 {
		t.Fatalf("%d versions of the converted spec render from the tree, want the one the conversion wrote", rendered)
	}

	peer := connectPeer(t, server.URL, issue.spec, "alice")
	if err := peer.WaitForText(fragmentMarkdown(issue.spec), issue.markdown[issue.spec], roomWait(t)); err != nil {
		t.Fatalf("a peer's copy of %s's room: %v", issue.key, err)
	}
	written := recordRoom{database: database, service: service, peer: peer,
		room: issueRoom{issue: issue.key, tree: tree, spec: issue.spec, verification: issue.verification, plan: issue.plan}}
	appendToSpec := func(markdown string) func(*pmdoc.Node) (*pmdoc.Node, error) {
		return func(held *pmdoc.Node) (*pmdoc.Node, error) {
			if held == nil {
				return nil, errors.New("the spec's fragment of the room holds no tree")
			}
			added, err := pmdoc.Parse(markdown)
			if err != nil {
				return nil, err
			}
			held.Children = append(held.Children, added.Children...)
			return held, nil
		}
	}
	// requireOneVersion fails unless the edit left the spec one version past before, holding each
	// of ids once, with every version of the spec that renders from the tree rendering as stored.
	requireOneVersion := func(what string, before int, ids ...string) {
		t.Helper()
		number := savedVersion(t, database, issue.spec)
		if number != before+1 {
			t.Errorf("%s left version %d of the spec newest, want %d: the one version its settlement wrote", what, number, before+1)
		}
		stored := storedVersionMarkdown(t, database, issue.spec, number)
		for _, id := range ids {
			if count := strings.Count(stored, "{#"+id+"}") + strings.Count(stored, "{#"+id+" "); count != 1 {
				t.Errorf("%s: version %d of the spec holds the block %s %d times, want once:\n%s", what, number, id, count, stored)
			}
		}
		requireTreeRendersAsStored(t, database, what, issue.spec)
	}

	written.settleSpecEdit(t, ctx, appendToSpec(":::requirement{#r2}\nA second requirement.\n:::\n"))
	requireOneVersion("the edit that adds the requirement r2", converted, "r1", "r2")
	if number := savedVersion(t, database, decisions); number != 1 {
		t.Errorf("the edit that adds the requirement r2 left version %d of the decisions query newest, want its version 1: the edit moved no answer of the query", number)
	}

	const decided = "We keep one store."
	written.settleSpecEdit(t, ctx, appendToSpec(":::decision{#d1}\n"+decided+"\n:::\n"))
	requireOneVersion("the edit that adds the decision d1", converted+1, "r1", "r2", "d1")
	if number := savedVersion(t, database, decisions); number != 2 {
		t.Errorf("the edit that adds the decision d1 left version %d of the decisions query newest, want the version 2 the drain writes of it", number)
	} else if stored := storedVersionMarkdown(t, database, decisions, number); !strings.Contains(stored, decided) {
		t.Errorf("version %d of the decisions query stores %q, want the decision's words %q in it", number, stored, decided)
	}
	requireTreeRendersAsStored(t, database, "the edit that adds the decision d1", decisions)
}
