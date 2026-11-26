package docs

//ci:builds packages/envoy ./cmd/dispatch blockTypeSupertagConvert builds envoy-dispatch, whose convert-issues TestEveryTypedBlockNodeCarriesItsTypesSupertag runs as the conversion of this issue's spec R6

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockTypeSupertagCommit is the commit the test stamps the envoy-dispatch it converts with, and
// names in the server_build_announcements row it writes, so the fleet gate of this issue's spec R6
// passes.
const blockTypeSupertagCommit = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"

// blockTypeSupertagUnanswered is the id of the reading in the ask nobody answers: no reading yet,
// so its node keeps the supertag block (this issue's spec R26).
const blockTypeSupertagUnanswered = "g2"

// The three documents of issue CORE-1. Together they hold one block of each type of
// pmdoc/schema/blocks.json - a callout, an ask, a requirement, a plan task, a check, a decision, a
// reading and a criterion - and a paragraph, a heading, a list and a code block, a reading in an
// ask that nobody has answered among them (blockTypeSupertagUnansweredReading, which a browser's
// write puts in the spec's ask a1). d2 is a decision with no reading and vp1 a paragraph of the
// Verification document, which the retypes below turn into a check; c2 is a check they turn into a
// requirement.
const (
	blockTypeSupertagSpec = "## The spec heading\n\n" +
		"A plain paragraph of the spec.\n\n" +
		"- A list item of the spec.\n\n" +
		"```go\nfmt.Println(\"a code block\")\n```\n\n" +
		":::criterion{#cr1}\nThe criterion.\n:::\n\n" +
		":::requirement{#r1 serves=\"dispatch://CORE-1/spec#b-cr1\"}\nThe requirement.\n:::\n\n" +
		":::callout{#co1 kind=\"note\" title=\"\"}\nA callout.\n:::\n\n" +
		"::::decision{#d1}\nThe decision with its reading.\n\n:::reading{#g1}\nThe agent reads the decision.\n:::\n::::\n\n" +
		":::decision{#d2}\nA decision with no reading.\n:::\n\n" +
		":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich way?\n\n- Left: Go left\n- Right: Go right\n:::\n"
	blockTypeSupertagVerification = "A paragraph of the Verification document.\n\n" +
		":::check{#c1 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe first check.\n:::\n\n" +
		":::check{#c2 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe second check.\n:::\n\n" +
		":::check{#c3 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe third check.\n:::\n"
	blockTypeSupertagPlan = ":::plan_task{#t1 serves=\"dispatch://CORE-1/spec#b-r1\"}\nThe task.\n:::\n"
)

// blockTypeSupertagUnansweredReading appends the reading blockTypeSupertagUnanswered to the end of
// the document's first ask, the spec's a1, which nobody has answered. A write that takes markdown
// refuses a reading it introduces into an ask that holds no answer, the seed of a document's first
// text among them (refuseReadingsWithoutAnswers, TestAReadingInAnAskWithNoAnswerIsRefused); a
// browser's write is never refused, so this is how such a reading comes to stand in a document,
// and the edit is a browser's (editLiveTree, appendToAsk).
func blockTypeSupertagUnansweredReading(tree *pmdoc.Node) *pmdoc.Node {
	return appendToAsk(&pmdoc.Node{
		Type:     "reading",
		Attrs:    pmdoc.Attrs{pmdoc.BlockIDAttr: blockTypeSupertagUnanswered},
		Children: []*pmdoc.Node{{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "The agent reads the question."}}}},
	})(tree)
}

// blockTypeSupertagNode is one document_blocks row beside the supertag its node carries, and
// whether the node stands.
type blockTypeSupertagNode struct {
	address, typ, supertag string
	present                bool
}

// blockTypeSupertagWant is the supertag the node of a block of type typ and id id carries: its
// type where the type is a supertag node of the block schema, except the reading of an ask nobody
// answered; block for every other block.
func blockTypeSupertagWant(typ, id string) string {
	if typ == "reading" && id == blockTypeSupertagUnanswered {
		return refs.BlockSupertag
	}
	if pmdoc.IsTypedBlock(typ) {
		return typ
	}
	return refs.BlockSupertag
}

// blockTypeSupertagNodes is every document_blocks row whose node address starts with refKey and
// '#', each with the supertag its node in graph_nodes carries.
func blockTypeSupertagNodes(t *testing.T, database *store.Store, refKey string) []blockTypeSupertagNode {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select d.node_id, d.type, coalesce(n.supertag, ''), n.id is not null
		from document_blocks d left join graph_nodes n on n.kind = $2 and n.id = d.node_id
		where starts_with(d.node_id, $1) order by d.node_id
	`, refKey+"#", refs.BlockNodeKind)
	if err != nil {
		t.Fatalf("read the blocks of %s with their nodes: %v", refKey, err)
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (blockTypeSupertagNode, error) {
		var node blockTypeSupertagNode
		err := row.Scan(&node.address, &node.typ, &node.supertag, &node.present)
		return node, err
	})
	if err != nil {
		t.Fatalf("read the blocks of %s with their nodes: %v", refKey, err)
	}
	return nodes
}

// blockTypeSupertagRequire fails t for each node that carries another supertag than its block's
// type gives it, or none.
func blockTypeSupertagRequire(t *testing.T, nodes []blockTypeSupertagNode) {
	t.Helper()
	for _, node := range nodes {
		want := blockTypeSupertagWant(node.typ, node.address[strings.LastIndex(node.address, "#")+1:])
		switch {
		case !node.present:
			t.Errorf("block %s of type %s carries the supertag (it has no node), want %s", node.address, node.typ, want)
		case node.supertag != want:
			t.Errorf("block %s of type %s carries the supertag %s", node.address, node.typ, node.supertag)
		}
	}
}

// blockTypeSupertagDocument creates document slug of issue, of project, its role role where it
// names one, its live text seeded with markdown and then edited by browser where it is not nil, a
// browser's write (editLiveTree), and writes its first version through the real version write, as
// a route marks its context where deferred is set (WithDeferredBlockIndex), so a document of more
// blocks than deferBlockIndexBlocks leaves its blocks to the reconciliation.
func blockTypeSupertagDocument(t *testing.T, database *store.Store, service *Service, issue, project, slug string, primary bool, role, markdown string, browser func(*pmdoc.Node) *pmdoc.Node, deferred bool) (string, VersionResult) {
	t.Helper()
	var stored *string
	if role != "" {
		stored = &role
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($1, $2, $3, $3 || '.md', 'doc', $4, $5, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue, project, slug, primary, stored).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	seedServiceText(t, service, id, markdown)
	if browser != nil {
		editLiveTree(t, service, id, browser)
	}
	ctx := context.Background()
	if deferred {
		ctx = WithDeferredBlockIndex(ctx)
	}
	result, err := blockTypeSupertagWrite(ctx, service, id, nil, "The first version")
	if err != nil {
		t.Fatalf("write the first version of %s: %v", slug, err)
	}
	settle(t, database, id)
	return id, result
}

// blockTypeSupertagWrite writes the next version of document id after ops, in a transaction of its
// own (writeVersion), and answers the error the write failed with, its transaction rolled back.
func blockTypeSupertagWrite(ctx context.Context, service *Service, id string, ops []model.EditOp, summary string) (VersionResult, error) {
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		return VersionResult{}, err
	}
	defer tx.Rollback(ctx)
	result, _, err := writeVersion(ctx, service, tx, id, ops, summary)
	return result, err
}

// blockTypeSupertagBlockID is the id of the block of document id whose text holds words.
func blockTypeSupertagBlockID(t *testing.T, service *Service, id, words string) string {
	t.Helper()
	markdown, blocks, err := service.TextWithBlocks(context.Background(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	for _, block := range blocks {
		if strings.Contains(markdown[block.From:block.To], words) && block.ID != "" && (block.Type == "paragraph" || block.Type == "heading") {
			return block.ID
		}
	}
	t.Fatalf("no block of %s holds %q: %q", id, words, markdown)
	return ""
}

// blockTypeSupertagConvert runs `envoy-dispatch convert-issues --project <project>` over database,
// the conversion of this issue's spec R6 by hand, which the checks take as its entry point. The
// binary is built here and stamped with blockTypeSupertagCommit, and the announcement of that
// commit is the newest one, with no live process, so the conversion's fleet gate passes.
func blockTypeSupertagConvert(t *testing.T, database *store.Store, project string) {
	t.Helper()
	announceBuild(t, database, blockTypeSupertagCommit)
	binary := filepath.Join(t.TempDir(), "envoy-dispatch")
	build := exec.Command("go", "build", "-buildvcs=false", "-ldflags", "-X main.buildCommit="+blockTypeSupertagCommit, "-o", binary, "../../../cmd/dispatch")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build envoy-dispatch: %v\n%s", err, out)
	}
	home := t.TempDir()
	command := exec.Command(binary, "convert-issues", "--project", project)
	command.Dir = home
	command.Env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + database.Pool.Config().ConnString(), "DISPATCH_NATS_DISABLED=1",
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy-dispatch convert-issues --project %s: %v\n%s", project, err, out)
	}
	t.Logf("envoy-dispatch convert-issues --project %s printed:\n%s", project, out)
}

// blockTypeSupertagCensus takes the store census, writes its report into the test's output, and
// answers the kind block_type of it.
func blockTypeSupertagCensus(t *testing.T, database *store.Store, what string) graphcensus.KindReport {
	t.Helper()
	report := takeStoreCensus(t, database)
	var written bytes.Buffer
	report.Write(&written)
	t.Logf("the store census %s:\n%s", what, written.String())
	return treeCensusKind(t, report, "block_type")
}

// blockTypeSupertagDifferences is how many differences of kind block_type kind holds, and whether
// one of them names address.
func blockTypeSupertagDifferences(kind graphcensus.KindReport, address string) (int, bool) {
	named := false
	for _, difference := range slices.Concat(kind.ReportedRows, kind.ReportedNodes) {
		named = named || strings.Contains(difference.At, address) || strings.Contains(difference.Cause, address)
	}
	return int(kind.RowDifferences + kind.NodeDifferences), named
}

// The node of each typed block carries the supertag of its type, and a census of kind block_type
// holds each document_blocks row that names its node against that node's supertag (this issue's
// spec R26). It proves every writer of a block node: the version write, the retype, the backfill's
// block batch, the reconciliation and the conversion of R6. The node of a paragraph, a heading, a
// list and a code block, and of the reading in an ask nobody has answered, carries block.
func TestEveryTypedBlockNodeCarriesItsTypesSupertag(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('BIG', 'Big', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('BIG-1', 'BIG', 1, 'Big', '{"kind":"user","id":"alice"}', 'W');
	`); err != nil {
		t.Fatalf("seed project BIG: %v", err)
	}

	// The writers: a spec, a Verification document and a plan through the real version write, the
	// spec's reading in the ask nobody answered put there by a browser's write.
	spec, _ := blockTypeSupertagDocument(t, database, service, "CORE-1", "CORE", "spec", true, "", blockTypeSupertagSpec, blockTypeSupertagUnansweredReading, false)
	verification, _ := blockTypeSupertagDocument(t, database, service, "CORE-1", "CORE", "verification-md", false, model.ArtifactRoleVerification, blockTypeSupertagVerification, nil, false)
	plan, _ := blockTypeSupertagDocument(t, database, service, "CORE-1", "CORE", "plan-md", false, model.ArtifactRolePlan, blockTypeSupertagPlan, nil, false)
	documents := map[string]string{"spec": spec, "verification-md": verification, "plan-md": plan}
	refKeys := map[string]string{}
	var written []blockTypeSupertagNode
	for slug, id := range documents {
		refKeys[slug] = refKeyOf(t, database, id)
		written = append(written, blockTypeSupertagNodes(t, database, refKeys[slug])...)
	}
	types := map[string]bool{}
	untyped := map[string]bool{}
	for _, node := range written {
		types[node.typ] = true
		if !pmdoc.IsTypedBlock(node.typ) {
			untyped[node.typ] = true
		}
	}
	for _, name := range pmdoc.TypedBlockNames() {
		if !types[name] {
			t.Fatalf("the three documents hold no block of type %s, so this test would prove nothing of it: %v", name, types)
		}
	}
	if len(untyped) < 4 {
		t.Fatalf("the three documents hold blocks of the untyped types %v, want a paragraph, a heading, a list and a code block", untyped)
	}
	blockTypeSupertagRequire(t, written)

	// A Verification document of more blocks than deferBlockIndexBlocks reads its check nodes with
	// the supertag check right after the write, before the reconciliation runs, and every block of
	// it gives the same answer once the reconciliation has indexed it.
	var big strings.Builder
	big.WriteString("## The big Verification document\n\n")
	for range deferBlockIndexBlocks {
		big.WriteString("- An item.\n")
	}
	big.WriteString("\n:::check{#bc1 surface=\"api\" run=\"go test ./x\"}\nA check of the big document.\n:::\n\n")
	big.WriteString(":::check{#bc2 surface=\"api\" run=\"go test ./x\"}\nAnother check of the big document.\n:::\n")
	bigID, _ := blockTypeSupertagDocument(t, database, service, "BIG-1", "BIG", "verification-md", false, model.ArtifactRoleVerification, big.String(), nil, true)
	bigKey := refKeyOf(t, database, bigID)
	if blockIndexAt(t, database, bigID) != nil {
		t.Fatalf("the Verification document of more than %d blocks is indexed by its own write, want its blocks left to the reconciliation", deferBlockIndexBlocks)
	}
	checks := 0
	for _, node := range blockTypeSupertagNodes(t, database, bigKey) {
		if node.typ != "check" {
			continue
		}
		checks++
		if !node.present || node.supertag != "check" {
			t.Errorf("block %s of type %s carries the supertag %s right after the write, want check", node.address, node.typ, node.supertag)
		}
	}
	if checks != 2 {
		t.Errorf("the deferred write left %d check blocks with rows, want 2", checks)
	}
	drainBlockIndex(t, service, "the deferred Verification document")
	indexed := blockTypeSupertagNodes(t, database, bigKey)
	if len(indexed) <= deferBlockIndexBlocks {
		t.Errorf("the reconciliation indexed %d blocks of the big document, want more than %d", len(indexed), deferBlockIndexBlocks)
	}
	blockTypeSupertagRequire(t, indexed)

	// A write that states a check's block apart from its node fails whole. The probe trigger
	// raises on a write of a graph_nodes row whose supertag is check, so a version that adds a
	// check and states its node fails, and neither the node nor the row of the check stands.
	probe, _ := blockTypeSupertagDocument(t, database, service, "BIG-1", "BIG", "probe-md", false, "", "A paragraph of the probe document.\n", nil, false)
	probeKey := refKeyOf(t, database, probe)
	if _, err := database.Pool.Exec(ctx, `
		create function blocktype_supertag_probe() returns trigger language plpgsql as $$
		begin
		  if new.supertag = 'check' then raise exception 'a node with the supertag check'; end if;
		  return new;
		end
		$$;
		create trigger blocktype_supertag_probe before insert or update on graph_nodes
		  for each row execute function blocktype_supertag_probe();
	`); err != nil {
		t.Fatalf("add the trigger that raises on a check's node: %v", err)
	}
	_, probeErr := blockTypeSupertagWrite(ctx, service, probe, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::check{#pc1 surface=\"api\" run=\"go test ./x\"}\nA check the write cannot state.\n:::",
	}}, "A version that adds a check")
	if probeErr == nil {
		t.Error("the version that adds a check wrote, with a trigger that raises on the node of a check")
	}
	var node, row int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*)::int from graph_nodes where kind = 'block' and id = $1),
		       (select count(*)::int from document_blocks where node_id = $1)
	`, probeKey+"#pc1").Scan(&node, &row); err != nil {
		t.Fatalf("read the check the failed write added: %v", err)
	}
	if node != 0 || row != 0 {
		t.Errorf("the version write stated block %s apart from its node: %d nodes and %d document_blocks rows stand", probeKey+"#pc1", node, row)
	}
	if _, err := database.Pool.Exec(ctx, `
		drop trigger blocktype_supertag_probe on graph_nodes;
		drop function blocktype_supertag_probe();
	`); err != nil {
		t.Fatalf("drop the trigger that raises on a check's node: %v", err)
	}

	// The retype: a paragraph becomes a check and a check a requirement through the retype op, and
	// a later version turns a decision into a check. The node and the row of each carry the new
	// type, and a block whose type no version changed keeps its changed_version and its text rows.
	heading := refKeys["spec"] + "#" + blockTypeSupertagBlockID(t, service, spec, "The spec heading")
	untouchedState := func() string {
		var state string
		if err := database.Pool.QueryRow(ctx, `
			select d.changed_version::text || '/' || (select count(*) from document_block_texts t
			                                          where t.artifact_id = d.artifact_id and t.block_id = d.block_id)::text
			from document_blocks d where d.node_id = $1
		`, heading).Scan(&state); err != nil {
			t.Fatalf("read the changed_version and the text rows of %s: %v", heading, err)
		}
		return state
	}
	untouchedBefore := untouchedState()
	paragraphBlock := blockTypeSupertagBlockID(t, service, verification, "A paragraph of the Verification document")
	retypedParagraph := refKeys["verification-md"] + "#" + paragraphBlock
	checkAttributes := map[string]any{"surface": "api", "run": "go test ./x"}
	retypes := []struct {
		document, address, from, to string
		ops                         []model.EditOp
	}{
		{verification, retypedParagraph, "paragraph", "check", nil},
		{verification, refKeys["verification-md"] + "#c2", "check", "requirement", nil},
		{spec, refKeys["spec"] + "#d2", "decision", "check", nil},
	}
	retypes[0].ops = []model.EditOp{
		{Op: "retype", Block: paragraphBlock, Type: "check", Attributes: checkAttributes},
		{Op: "retype", Block: "c2", Type: "requirement"},
	}
	retypes[2].ops = []model.EditOp{{Op: "retype", Block: "d2", Type: "check", Attributes: checkAttributes}}
	for _, step := range []int{0, 2} {
		if _, err := blockTypeSupertagWrite(ctx, service, retypes[step].document, retypes[step].ops, "A version that retypes blocks"); err != nil {
			t.Fatalf("write the version that retypes %s: %v", retypes[step].address, err)
		}
	}
	for _, retype := range retypes {
		var typ, supertag string
		if err := database.Pool.QueryRow(ctx, `
			select d.type, coalesce(n.supertag, '') from document_blocks d
			left join graph_nodes n on n.kind = 'block' and n.id = d.node_id where d.node_id = $1
		`, retype.address).Scan(&typ, &supertag); err != nil {
			t.Fatalf("read the retyped block %s: %v", retype.address, err)
		}
		if typ != retype.to || supertag != retype.to {
			t.Errorf("the retype of block %s from %s to %s left its node with the supertag %s (its row holds the type %s)",
				retype.address, retype.from, retype.to, supertag, typ)
		}
	}
	if after := untouchedState(); after != untouchedBefore {
		t.Errorf("the retypes moved the changed_version/text rows of %s, whose type no version changed, from %s to %s", heading, untouchedBefore, after)
	}

	// The backfill: the three documents put back as the release before left them - no block names
	// a node, and the store holds none of a block. Right after the backfill, and before the
	// reconciliation indexes the documents it marked, the node of each block carries the supertag of
	// its type.
	drainBlockIndex(t, service, "the retypes")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for slug := range documents {
		blockTypeSupertagRequire(t, blockTypeSupertagNodes(t, database, refKeys[slug]))
	}

	// A build of the release before restates a check's node with the supertag block and its
	// document is owed, and the reconciliation gives the node its type's supertag again.
	restated := refKeys["verification-md"] + "#c3"
	if _, err := database.Pool.Exec(ctx, `update graph_nodes set supertag = $2 where kind = $3 and id = $1`, restated, refs.BlockSupertag, refs.BlockNodeKind); err != nil {
		t.Fatalf("restate the node of %s with the supertag block: %v", restated, err)
	}
	if _, err := database.Pool.Exec(ctx, `update document_block_versions set store_owed_at = coalesce(store_owed_at, now()) where artifact_id = $1::uuid`, verification); err != nil {
		t.Fatalf("mark the Verification document owed: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	for slug := range documents {
		blockTypeSupertagRequire(t, blockTypeSupertagNodes(t, database, refKeys[slug]))
	}
	if after := blockTypeSupertagNodes(t, database, refKeys["verification-md"]); !slices.ContainsFunc(after, func(node blockTypeSupertagNode) bool {
		return node.address == restated && node.supertag == "check"
	}) {
		t.Errorf("after the reconciliation the node of %s does not carry check again: %v", restated, after)
	}

	// The conversion of R6 moves the issue's three documents into its room, and each block of the
	// room carries the supertag of its type.
	blockTypeSupertagConvert(t, database, "CORE")
	var tree string
	if err := database.Pool.QueryRow(ctx, `select artifact_id::text from issue_trees where issue_key = 'CORE-1'`).Scan(&tree); err != nil {
		t.Fatalf("read the tree CORE-1 holds after its conversion: %v", err)
	}
	treeKey := refKeyOf(t, database, tree)
	room := blockTypeSupertagNodes(t, database, treeKey)
	if len(room) < len(written) {
		t.Fatalf("the room of CORE-1 holds %d blocks after its conversion, want the %d its three documents held", len(room), len(written))
	}
	blockTypeSupertagRequire(t, room)

	// A version that drops one check states its node gone, and the node keeps the supertag check,
	// as heldSupertags in refs/blocks.go keeps it. The version is written by a service that holds
	// no room of the old shape.
	later := blockIndexProcess(t, database, "")
	dropped := treeKey + "#c1"
	version, err := blockTypeSupertagWrite(ctx, later, verification, []model.EditOp{{Op: "delete", Block: "c1"}}, "A version that drops a check")
	if err != nil {
		t.Fatalf("write the version that drops the check c1: %v", err)
	}
	var state, supertag string
	if err := database.Pool.QueryRow(ctx, `select state, supertag from graph_nodes where kind = 'block' and id = $1`, dropped).Scan(&state, &supertag); err != nil {
		t.Fatalf("read the node of the dropped check %s: %v", dropped, err)
	}
	if state != "gone" {
		t.Errorf("block %s, which version %d dropped, reads the state %s, want gone", dropped, version.Version.Number, state)
	}
	if supertag != "check" {
		t.Errorf("block %s, which version %d dropped, carries the supertag %s, want check", dropped, version.Version.Number, supertag)
	}

	// The census: once nothing is owed, the kind block_type reads rows and nodes and no difference.
	drainBlockIndex(t, later, "the dropped check")
	clean := blockTypeSupertagCensus(t, database, "once nothing is owed")
	if clean.Rows == 0 || clean.Nodes == 0 || clean.RowDifferences != 0 || clean.NodeDifferences != 0 {
		t.Errorf("the census of kind block_type reads %d rows, %d nodes, %d row and %d node difference(s), want rows and nodes above zero and no difference",
			clean.Rows, clean.Nodes, clean.RowDifferences, clean.NodeDifferences)
	}
	var plainParagraph string
	for _, node := range blockTypeSupertagNodes(t, database, treeKey) {
		if node.typ == "paragraph" {
			plainParagraph = node.address
			break
		}
	}
	if plainParagraph == "" {
		t.Fatalf("the room of CORE-1 holds no paragraph to break the census with")
	}
	checkAddress := treeKey + "#c3"
	for _, control := range []struct{ what, address, break_, mend string }{
		{"the supertag of a check's node set to block", checkAddress,
			`update graph_nodes set supertag = 'block' where kind = 'block' and id = $1`,
			`update graph_nodes set supertag = 'check' where kind = 'block' and id = $1`},
		{"the supertag of a paragraph's node set to check", plainParagraph,
			`update graph_nodes set supertag = 'check' where kind = 'block' and id = $1`,
			`update graph_nodes set supertag = 'block' where kind = 'block' and id = $1`},
		{"the type of a check's row set to requirement", checkAddress,
			`update document_blocks set type = 'requirement' where node_id = $1`,
			`update document_blocks set type = 'check' where node_id = $1`},
	} {
		if _, err := database.Pool.Exec(ctx, control.break_, control.address); err != nil {
			t.Fatalf("break the census with %s: %v", control.what, err)
		}
		broken := blockTypeSupertagCensus(t, database, "with "+control.what)
		if count, named := blockTypeSupertagDifferences(broken, control.address); count != 1 || !named {
			t.Errorf("with block %s broken the census reads %d difference(s) of kind block_type, want one naming the block", control.address, count)
		}
		if _, err := database.Pool.Exec(ctx, control.mend, control.address); err != nil {
			t.Fatalf("mend %s: %v", control.what, err)
		}
	}
}
