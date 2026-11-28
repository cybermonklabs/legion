package api

//ci:builds packages/envoy ./cmd/dispatch convertIssues builds envoy-dispatch, whose convert-issues TestEveryPointerAtAMovedNodeIsATupleAndItsProjection runs as the conversion of R6 of the issue-tree spec

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// conversionCommit is the commit the conversion checks stamp the envoy-dispatch they convert with,
// and name in the server_build_announcements row they write, so the fleet gate of R6 of the
// issue-tree spec passes.
const conversionCommit = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"

// convertIssues runs `envoy-dispatch convert-issues --project <project>` over database, the
// conversion of R6 of the issue-tree spec by hand, which the checks take as its entry point: with
// dryRun, `--dry-run`, which writes as a real run does and rolls each issue's transaction back. It
// fails t unless the command exits 0.
func convertIssues(t *testing.T, database *store.Store, project string, dryRun bool) {
	t.Helper()
	binary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "envoy-dispatch", Dir: "../../../cmd/dispatch",
		Flags: []string{"-ldflags", "-X main.buildCommit=" + conversionCommit},
	})
	args := []string{"convert-issues", "--project", project}
	if dryRun {
		args = append(args, "--dry-run")
	}
	home := t.TempDir()
	command := exec.Command(binary, args...)
	command.Dir = home
	command.Env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + database.Pool.Config().ConnString(), "DISPATCH_NATS_DISABLED=1",
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy-dispatch %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	t.Logf("envoy-dispatch %s printed:\n%s", strings.Join(args, " "), out)
}

// pointersRow is one SQL statement reading one row as text, and its arguments: what the test
// compares before and after a run of the conversion.
type pointersRow struct {
	what      string
	statement string
	args      []any
}

func (row pointersRow) read(t *testing.T, database *store.Store) string {
	t.Helper()
	var text string
	if err := database.Pool.QueryRow(context.Background(), row.statement, row.args...).Scan(&text); err != nil {
		t.Fatalf("read %s: %v", row.what, err)
	}
	return text
}

// pointersFarEnds is the far end of every tuple of kind out of the node fromKind/fromID to a node
// of kind toKind that stands now: the newest row of each (kind, far end) is an assertion.
func pointersFarEnds(t *testing.T, database *store.Store, fromKind, fromID, kind, toKind string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_id from (
			select distinct on (kind, to_kind, to_id) to_id, asserted
			from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
			order by kind, to_kind, to_id, seq desc
		) latest
		where asserted order by to_id
	`, fromKind, fromID, kind, toKind)
	if err != nil {
		t.Fatalf("read the %s tuples out of %s %s: %v", kind, fromKind, fromID, err)
	}
	ends, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the %s tuples out of %s %s: %v", kind, fromKind, fromID, err)
	}
	return ends
}

// Every structured pointer at a node that moves is a tuple, and its row a projection of it (this
// issue's spec R13): the refs row's to_id, the block_refs row's to_id, the ask's
// block_artifact_id, the comment's anchor and the check_results pair of check_artifact_id and
// check_block_id. An issue the release before stored - no document_rooms row, its three documents
// each a room of its own - converted through the conversion of R6 moves its nodes into its tree,
// and each pointer then reads back as a tuple of the store at the node's new home, its projection
// equal to it. The moving transaction rolled back leaves every pointer as it was. A #b- citation in
// the prose of a comment or a message keeps its bytes and resolves through the moved_to tuple, and
// a pointer at a block that did not move is untouched.
func TestEveryPointerAtAMovedNodeIsATupleAndItsProjection(t *testing.T) {
	t.Parallel()
	const project = "PTR"
	const cited = "The cited paragraph, which every structured pointer names."
	const stays = "The paragraph of notes.md, which does not move."
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})

	// The issue as the release before stores it: a live process lists no issue room, so the issue
	// is created with its three documents each a room of its own and no document_rooms row.
	holdIssuesUnconverted(t, database)
	issue := createInteractionIssue(t, handler, project, "Pointers that move", "A placeholder spec.\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "spec", cited+"\n\n"+typedBlockMarkdown(issue.Key, acceptedRequirement("r1"))+
		"\n:::ask{#k1}\nDoes the cited paragraph hold?\n:::\n")
	verification := uploadDocument(t, handler, owner, "verification.md", typedBlockMarkdown(issue.Key, acceptedCheck("c1", "r1"))).Artifact
	uploadDocument(t, handler, owner, "plan.md", typedBlockMarkdown(issue.Key, acceptedPlanTask("t1", "r1")))
	notes := uploadDocument(t, handler, owner, "notes.md", stays+"\n").Artifact
	var rooms int
	if err := database.Pool.QueryRow(ctx, `select count(*)::int from document_rooms d join artifacts a on a.id = d.artifact_id where a.issue_key = $1`, issue.Key).Scan(&rooms); err != nil {
		t.Fatal(err)
	}
	if rooms != 0 {
		t.Fatalf("%s holds %d document_rooms rows before its conversion, want none: the release before stores none", issue.Key, rooms)
	}
	citing := createProjectIssue(t, handler, project, "The citing issue", "The citing issue's spec.\n")
	cites := uploadDocument(t, handler, "/api/v1/issues/"+citing, "cites.md", "The citing paragraph.\n").Artifact

	specKey := refKeyOf(t, database, issue.PrimaryArtifactID)
	paragraph := blockIDsByText(t, handler, issue.PrimaryArtifactID)[cited]
	stayed := blockIDsByText(t, handler, notes.ID)[stays]
	citingBlock := blockIDsByText(t, handler, cites.ID)["The citing paragraph."]
	if paragraph == "" || stayed == "" || citingBlock == "" {
		t.Fatalf("the blocks of the seeded documents are not listed: cited %q, stays %q, citing %q", paragraph, stayed, citingBlock)
	}
	oldAddress := specKey + "#" + paragraph
	notesAddress := refKeyOf(t, database, notes.ID) + "#" + stayed
	citesKey := refKeyOf(t, database, cites.ID)
	fromNode := citesKey + "#" + citingBlock

	// The five structured pointers, written by SQL as the release before stores them: rows that
	// name no node. The ask is the k1 block's row the spec's write opened, restated with no node.
	for _, statement := range []struct {
		what, sql string
		args      []any
	}{
		{"the refs row", `insert into refs (from_kind, from_id, to_kind, to_id, created_at) values ('artifact', $1, 'block', $2, now())`,
			[]any{cites.ID, oldAddress}},
		{"the refs row at a block that does not move", `insert into refs (from_kind, from_id, to_kind, to_id, created_at) values ('artifact', $1, 'block', $2, now())`,
			[]any{cites.ID, notesAddress}},
		{"the block_refs row", `insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version) values ($1::uuid, $2, $3, 'block', $4, 'mentions', 1)`,
			[]any{cites.ID, citingBlock, fromNode, oldAddress}},
		{"the block_refs row at a block that does not move", `insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version) values ($1::uuid, $2, $3, 'block', $4, 'mentions', 1)`,
			[]any{cites.ID, citingBlock, fromNode, notesAddress}},
		{"the ask", `update asks set node_kind = null, node_address = null where block_artifact_id = $1::uuid and block_id = 'k1'`,
			[]any{issue.PrimaryArtifactID}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write %s as the release before stores it: %v", statement.what, err)
		}
	}
	var askID string
	if err := database.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1::uuid and block_id = 'k1'`, issue.PrimaryArtifactID).Scan(&askID); err != nil {
		t.Fatalf("read the ask of the spec's k1 block: %v", err)
	}
	if ends := pointersFarEnds(t, database, graphstore.KindAsk, askID, graphstore.TupleAskedOn, refs.BlockNodeKind); !slices.Equal(ends, []string{specKey + "#k1"}) {
		t.Errorf("new ask %s states asked_on to %v, want its block %s#k1", askID, ends, specKey)
	}
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "The cited paragraph"},
		"body":   "A comment anchored on the cited paragraph.",
	}, "alice")
	if comment.Anchor == nil || comment.Anchor.BlockID == nil || *comment.Anchor.BlockID != paragraph || comment.Anchor.ArtifactID != issue.PrimaryArtifactID {
		t.Fatalf("the comment's anchor is %+v, want the cited paragraph %s of the spec %s", comment.Anchor, paragraph, issue.PrimaryArtifactID)
	}
	if _, err := database.Pool.Exec(ctx, `update comments set node_kind = null, node_address = null where id = $1::uuid`, comment.ID); err != nil {
		t.Fatalf("restate the comment as the release before stores it: %v", err)
	}
	var resultID string
	if err := database.Pool.QueryRow(ctx, `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, role, commit_sha, verdict, actor)
		values ($1, $1, $2::uuid, 'c1', $3, $4, 'implementer', $5, 'fail', '{"kind":"person","id":"alice"}')
		returning id::text
	`, issue.Key, verification.ID, latestVersion(t, database, verification.ID).Number, strings.Repeat("ab", 32), conversionCommit).Scan(&resultID); err != nil {
		t.Fatalf("write the check_results row as the release before stores it: %v", err)
	}

	// The prose citations, whose bytes the move never rewrites.
	prose := "See dispatch://" + issue.Key + "/spec#b-" + paragraph + " for the cited paragraph."
	proseComment := createThreadComment(t, handler, citing, map[string]any{"body": prose}, "alice")
	proseMessage := createIssueMessage(t, handler, citing, map[string]any{"body": prose}, "alice")
	bodies := []pointersRow{
		{"the body of the comment citing the paragraph in prose", `select body from comments where id = $1::uuid`, []any{proseComment.ID}},
		{"the body of the message citing the paragraph in prose", `select body from messages where id = $1::uuid`, []any{proseMessage.ID}},
	}
	pointers := []pointersRow{
		{"the refs row", `select coalesce(string_agg(to_jsonb(r)::text, ',' order by r.to_id), '') from refs r where r.from_kind = 'artifact' and r.from_id = $1 and r.to_kind = 'block'`, []any{cites.ID}},
		{"the block_refs row", `select coalesce(string_agg(to_jsonb(r)::text, ',' order by r.to_id), '') from block_refs r where r.from_node = $1 and r.to_kind = 'block'`, []any{fromNode}},
		{"the ask", `select to_jsonb(k)::text from asks k where k.id = $1::uuid`, []any{askID}},
		{"the comment", `select to_jsonb(c)::text from comments c where c.id = $1::uuid`, []any{comment.ID}},
		{"the check result", `select to_jsonb(r)::text from check_results r where r.id = $1::uuid`, []any{resultID}},
	}
	untouched := []pointersRow{
		{"the refs row at the block that does not move", `select to_jsonb(r)::text from refs r where r.from_kind = 'artifact' and r.from_id = $1 and r.to_id = $2`, []any{cites.ID, notesAddress}},
		{"the block_refs row at the block that does not move", `select to_jsonb(r)::text from block_refs r where r.from_node = $1 and r.to_id = $2`, []any{fromNode, notesAddress}},
	}
	read := func(rows []pointersRow) []string {
		texts := make([]string, len(rows))
		for index, row := range rows {
			texts[index] = row.read(t, database)
		}
		return texts
	}
	before := read(slices.Concat(pointers, bodies, untouched))

	openTheConversionFleet(t, database)

	// The moving transaction rolled back: every pointer stands as it was.
	convertIssues(t, database, project, true)
	for index, now := range read(slices.Concat(pointers, bodies, untouched)) {
		if now != before[index] {
			t.Errorf("the conversion rolled back left %s as %s, want it as it was: %s", slices.Concat(pointers, bodies, untouched)[index].what, now, before[index])
		}
	}

	// The moving transaction committed: each pointer is a tuple at the node's new home and its row
	// projects it.
	convertIssues(t, database, project, false)
	tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if tree == "" {
		t.Fatalf("after its conversion %s holds no tree", issue.Key)
	}
	treeKey := refKeyOf(t, database, tree)
	newAddress := treeKey + "#" + paragraph
	checkAddress := treeKey + "#c1"

	projections := []struct {
		row                        pointersRow
		fromKind, fromID, kind, to string
		want                       string
	}{
		{pointersRow{"the refs row's to_id", `select coalesce(string_agg(r.to_id, ',' order by r.to_id), '') from refs r where r.from_kind = 'artifact' and r.from_id = $1 and r.to_kind = 'block' and r.to_id <> $2`, []any{cites.ID, notesAddress}},
			"artifact", citesKey, "mentions", "block", newAddress},
		{pointersRow{"the block_refs row's to_id", `select coalesce(string_agg(r.to_id, ',' order by r.to_id), '') from block_refs r where r.from_node = $1 and r.to_kind = 'block' and r.to_id <> $2`, []any{fromNode, notesAddress}},
			"block", fromNode, "mentions", "block", newAddress},
		{pointersRow{"the ask's block_artifact_id", `select coalesce((select ref_key from artifacts where id = k.block_artifact_id), '') from asks k where k.id = $1::uuid`, []any{askID}},
			graphstore.KindAsk, askID, graphstore.TuplePartOf, "artifact", treeKey},
		{pointersRow{"the ask's block", `select coalesce((select ref_key from artifacts where id = k.block_artifact_id) || '#' || k.block_id, '') from asks k where k.id = $1::uuid`, []any{askID}},
			graphstore.KindAsk, askID, graphstore.TupleAskedOn, refs.BlockNodeKind, treeKey + "#k1"},
		{pointersRow{"the comment's anchor", `select coalesce(` + graphstore.CommentAnchoredInSQL("c") + `, '') from comments c where c.id = $1::uuid`, []any{comment.ID}},
			graphstore.KindComment, comment.ID, graphstore.TupleAnchoredIn, "block", newAddress},
		{pointersRow{"the check_results pair", `select coalesce((select ref_key from artifacts where id = r.check_artifact_id) || '#' || r.check_block_id, '') from check_results r where r.id = $1::uuid`, []any{resultID}},
			graphstore.KindResult, resultID, graphstore.TupleResultOf, "block", checkAddress},
	}
	for _, projection := range projections {
		row := projection.row.read(t, database)
		ends := pointersFarEnds(t, database, projection.fromKind, projection.fromID, projection.kind, projection.to)
		if !slices.Equal(ends, []string{projection.want}) {
			t.Errorf("%s: the store holds the %s tuples %v out of %s %s, want one to the node's new home %s", projection.row.what, projection.kind, ends, projection.fromKind, projection.fromID, projection.want)
		}
		if row != projection.want {
			t.Errorf("%s projects %q, want %q, its tuple's far end at the node's new home", projection.row.what, row, projection.want)
		}
	}
	var checkKeys string
	if err := database.Pool.QueryRow(ctx, `select coalesce(r.check_artifact_id::text, '') || ' ' || r.check_block_id from check_results r where r.id = $1::uuid`, resultID).Scan(&checkKeys); err != nil {
		t.Fatal(err)
	}
	if checkKeys != tree+" c1" {
		t.Errorf("the check_results pair reads %q, want %q: the check_results_check index serves the pair at the check's new home", checkKeys, tree+" c1")
	}
	found := false
	for _, result := range checkOf(t, readResults(t, handler, issue.Key), "c1").Results {
		found = found || result.ID == resultID
	}
	if !found {
		t.Errorf("the result %s reads back through no check c1 of %s: it belongs to no check node the issue holds", resultID, issue.Key)
	}

	// Prose keeps its bytes and resolves through the moved_to tuple.
	for index, now := range read(bodies) {
		if want := before[len(pointers)+index]; now != want {
			t.Errorf("the conversion rewrote %s to %q, want its bytes %q unchanged", bodies[index].what, now, want)
		}
	}
	if moved := pointersFarEnds(t, database, "block", oldAddress, graphstore.TupleMovedTo, "block"); !slices.Equal(moved, []string{newAddress}) {
		t.Errorf("the old address %s states the moved_to tuples %v, want one to %s", oldAddress, moved, newAddress)
	}
	resolved, ok, err := graphstore.MovedTo(ctx, database.Pool, oldAddress)
	if err != nil {
		t.Fatalf("resolve %s: %v", oldAddress, err)
	}
	if !ok || resolved.Address != newAddress {
		t.Errorf("the prose citation %s resolves to %+v (%t), want %s through its moved_to tuple", oldAddress, resolved, ok, newAddress)
	}

	// A pointer at a block that did not move is untouched, row for row.
	for index, now := range read(untouched) {
		if want := before[len(pointers)+len(bodies)+index]; now != want {
			t.Errorf("the conversion changed %s to %s, want it untouched: %s", untouched[index].what, now, want)
		}
	}
	if moved := pointersFarEnds(t, database, "block", notesAddress, graphstore.TupleMovedTo, "block"); len(moved) != 0 {
		t.Errorf("the block %s, which did not move, states the moved_to tuples %v, want none", notesAddress, moved)
	}
}

// openTheConversionFleet takes away the live process holdIssuesUnconverted added, which lists no
// issue room, and writes the announcement of the build the conversion runs as (conversionCommit),
// so the conversion's fleet gate passes (R6 of the issue-tree spec): no live process serves another
// build, and the newest announcement names the commit the conversion runs against.
func openTheConversionFleet(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where heartbeat_at > now() + interval '1 hour'`); err != nil {
		t.Fatalf("take away the live process that lists no issue room: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, conversionCommit); err != nil {
		t.Fatalf("announce the build the conversion runs as: %v", err)
	}
}
