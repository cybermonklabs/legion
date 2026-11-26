package api

//ci:builds packages/envoy ./cmd/dispatch blockTypeSupertagBoot builds envoy-dispatch, which TestTheTupleArmReadsABlocksTypeFromItsNode boots over four databases for the boot step of this issue's spec R26
//ci:reads packages/envoy/internal/dispatch/**/*.go !**/*_test.go blockTypeSupertagScan walks internal/dispatch and reads every non-test file as text

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// blockTypeSupertagCommit is the commit the test stamps the envoy-dispatch it boots with, and
// blockTypeSupertagEarlier the commit of the announcement it writes first, so that announcement is
// the newest one that names another commit than the booting build's.
const (
	blockTypeSupertagCommit  = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	blockTypeSupertagEarlier = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
)

// blockTypeSupertagCapability is the capability a build lists that moves the two reads of a
// block's type onto its node (processes.Capabilities), which its announcement lists beside the
// tuple kinds.
const blockTypeSupertagCapability = "typed_block_supertags"

// blockTypeSupertagIssue is one issue of a database the test seeds: a criterion, a requirement
// whose serves names that criterion, and a check whose serves names that requirement, by node
// address and by the ref a reader names the block with.
type blockTypeSupertagIssue struct {
	key                           string
	criterion, requirement, check string
	requirementRef, checkRef      string
}

// blockTypeSupertagPointer is one serves pointer of the issue: the block it leaves, as a node and
// as a ref, and the node it points at.
type blockTypeSupertagPointer struct {
	from, fromRef, to string
}

func (i blockTypeSupertagIssue) pointers() []blockTypeSupertagPointer {
	return []blockTypeSupertagPointer{
		{from: i.requirement, fromRef: i.requirementRef, to: i.criterion},
		{from: i.check, fromRef: i.checkRef, to: i.requirement},
	}
}

// blockTypeSupertagSeed seeds the issue of a database: a spec holding a criterion and a
// requirement that serves it, and a Verification document holding a check that serves the
// requirement, each written through the upload route.
func blockTypeSupertagSeed(t *testing.T, handler http.Handler, database *store.Store, deps Deps, project string) blockTypeSupertagIssue {
	t.Helper()
	issue := createInteractionIssue(t, handler, project, "Typed blocks", "A spec.\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "spec", "A spec.\n\n:::criterion{#cr1}\nThe criterion.\n:::\n\n"+
		":::requirement{#r1 serves=\"dispatch://"+issue.Key+"/spec#b-cr1\"}\nThe requirement.\n:::\n")
	verification := uploadDocument(t, handler, owner, "verification.md",
		":::check{#c1 serves=\"dispatch://"+issue.Key+"/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe check.\n:::\n").Artifact
	reconcileUntilNothingOwed(t, handler, deps, issue.Key)
	specKey := blockKeyOf(t, database, issue.PrimaryArtifactID)
	verificationKey := blockKeyOf(t, database, verification.ID)
	return blockTypeSupertagIssue{
		key:            issue.Key,
		criterion:      specKey + "#cr1",
		requirement:    specKey + "#r1",
		check:          verificationKey + "#c1",
		requirementRef: "dispatch://" + issue.Key + "/spec#b-r1",
		checkRef:       "dispatch://" + issue.Key + "/artifact/verification-md#b-c1",
	}
}

// blockTypeSupertagListed reads whether the pointer is listed through each of the three reads of
// the tuple arm: graph_edges, refs.BacklinkCounts and GET /api/v1/references?from=<the citing
// block's ref>.
func blockTypeSupertagListed(t *testing.T, handler http.Handler, database *store.Store, pointer blockTypeSupertagPointer) map[string]bool {
	t.Helper()
	ctx := context.Background()
	var edges int
	if err := database.Pool.QueryRow(ctx,
		`select count(*)::int from graph_edges where from_id = $1 and kind = 'serves' and to_id = $2`, pointer.from, pointer.to).Scan(&edges); err != nil {
		t.Fatalf("read graph_edges for the pointer from %s to %s: %v", pointer.from, pointer.to, err)
	}
	counts, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, refs.BlockNodeKind, []string{pointer.to}, nil)
	if err != nil {
		t.Fatalf("count the backlinks of %s: %v", pointer.to, err)
	}
	referenced := false
	for _, edge := range graphEdges(t, handler, url.Values{"from": {pointer.fromRef}}).Edges {
		referenced = referenced || edge.Kind == "serves"
	}
	return map[string]bool{"graph_edges": edges > 0, "refs.BacklinkCounts": counts[pointer.to] > 0, "GET /api/v1/references": referenced}
}

// blockTypeSupertagRequireListed fails t for each read of each pointer that does not answer want
// (listed where want is true, hidden where it is false), with message as its format over the
// pointer's source, its target, the read and the answer.
func blockTypeSupertagRequireListed(t *testing.T, handler http.Handler, database *store.Store, issue blockTypeSupertagIssue, want bool, message string) {
	t.Helper()
	answer := func(listed bool) string {
		if listed {
			return "listed"
		}
		return "hidden"
	}
	for _, pointer := range issue.pointers() {
		reads := blockTypeSupertagListed(t, handler, database, pointer)
		for _, read := range []string{"graph_edges", "refs.BacklinkCounts", "GET /api/v1/references"} {
			if reads[read] != want {
				t.Errorf(message, pointer.from, pointer.to, read, answer(reads[read]), answer(want))
			}
		}
	}
}

// blockTypeSupertagFunction is the text of the function name, as pg_proc holds it.
func blockTypeSupertagFunction(t *testing.T, database *store.Store, name string) string {
	t.Helper()
	var text string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(string_agg(prosrc, E'\n'), '') from pg_proc where proname = $1`, name).Scan(&text); err != nil {
		t.Fatalf("read %s from pg_proc: %v", name, err)
	}
	if text == "" {
		t.Fatalf("pg_proc holds no function %s", name)
	}
	return text
}

// blockTypeSupertagReadsRow is whether the text of function name names document_blocks.
func blockTypeSupertagReadsRow(t *testing.T, database *store.Store, name string) bool {
	t.Helper()
	return strings.Contains(blockTypeSupertagFunction(t, database, name), "document_blocks")
}

// blockTypeSupertagBoot boots this head's envoy-dispatch, stamped with blockTypeSupertagCommit,
// over the database databaseURL names, on a loopback port the test holds until the process serves
// on it, and answers once it logs that it serves. It inherits nothing from the test's environment.
// The server reads its two test settings, which turn the embedder off and put the maintenance
// sweep a day apart, only beside DISPATCH_TEST_HOOKS=1, and refuses to boot on either without it;
// it sets no hold, so nothing holds the boot backfill or the step that follows it.
func blockTypeSupertagBoot(t *testing.T, databaseURL string) *cmdtest.Process {
	t.Helper()
	binary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "envoy-dispatch", Dir: "../../../cmd/dispatch",
		Flags: []string{"-ldflags", "-X main.buildCommit=" + blockTypeSupertagCommit},
	})
	home := t.TempDir()
	held := cmdtest.HoldPort(t)
	environment := map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": home, "XDG_DATA_HOME": home,
		"DATABASE_URL":                             databaseURL,
		"DISPATCH_AGENT_TOKEN":                     "block-type-supertag-token",
		"DISPATCH_IDENTITY":                        "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED":         "1",
		"DISPATCH_LISTEN_HOST":                     "127.0.0.1",
		"DISPATCH_PORT":                            strconv.Itoa(held.Port),
		"DISPATCH_WEB_DIST":                        home,
		"DISPATCH_NATS_DISABLED":                   "1",
		"DISPATCH_SIGNING_KEY":                     "block-type-supertag-signing-key",
		"DISPATCH_TEST_HOOKS":                      "1",
		"DISPATCH_TEST_EMBEDDER_OFF":               "1",
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "24h",
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	for name, value := range environment {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	slices.Sort(cmd.Env)
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	process := cmdtest.Start(t, "envoy-dispatch", cmd)
	process.WaitForOutput(t, `msg="dispatch: listening" addr=127.0.0.1:`+strconv.Itoa(held.Port))
	return process
}

// blockTypeSupertagStop stops the process with SIGTERM, as a deploy does, and answers what it
// wrote.
func blockTypeSupertagStop(t *testing.T, process *cmdtest.Process) string {
	t.Helper()
	process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
	return process.Output.String()
}

// blockTypeSupertagStepEnded are the lines the boot's typed block supertags step ends with
// (graphcensus.RunTypedBlockSupertagsStep): it restated both functions, it leaves them reading the
// rows and says why, or it failed, which cmd/dispatch's startBootBackfill logs. A step that finds
// neither function reading the rows ends with no line, which a boot over a database the step has
// not restated never meets.
var blockTypeSupertagStepEnded = []string{
	`msg="dispatch: the typed block supertags step restated `,
	`msg="dispatch: the typed block supertags step leaves `,
	`msg="dispatch: the typed block supertags step failed"`,
}

// blockTypeSupertagBackfillEnded are the lines with which the boot's run of the backfill ends
// with no done line, skipped at its bound or failed (cmd/dispatch's runBootBackfill): a run that
// logs no done line never reaches the step.
var blockTypeSupertagBackfillEnded = []string{`msg="dispatch: boot backfill skipped`, `msg="dispatch: boot backfill failed`}

// blockTypeSupertagWaitStep waits until the process, booted over a database whose two functions
// still read the rows, logs how its boot's typed block supertags step ended, and fails t where the
// boot's run of the backfill ended with no done line, so the step never ran, or where the process
// exited first. The step logs each block it names while its transaction holds a connection of the
// pool, and a stopping process exits only once its connections are back, so the log the stop then
// answers holds every line the step wrote.
func blockTypeSupertagWaitStep(t *testing.T, process *cmdtest.Process) {
	t.Helper()
	logged := func(output string, lines []string) bool {
		return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(output, line) })
	}
	exited := false
	output := testwait.Until(t, "the boot's typed block supertags step to log how it ended", func() (string, bool) {
		select {
		case <-process.Exited:
			exited = true
		default:
		}
		output := process.Output.String()
		return output, exited || logged(output, blockTypeSupertagStepEnded) || logged(output, blockTypeSupertagBackfillEnded)
	})
	switch {
	case logged(output, blockTypeSupertagStepEnded):
	case exited:
		t.Fatalf("envoy-dispatch exited before its typed block supertags step ended:\n%s", output)
	default:
		t.Fatalf("the boot's run of the backfill ended with no done line, so the typed block supertags step never ran:\n%s", output)
	}
}

// blockTypeSupertagAnnounce writes the announcement of another build than the one the test boots,
// listing tupleKinds, which is what the step reads of the fleet before it (this issue's spec R26).
func blockTypeSupertagAnnounce(t *testing.T, database *store.Store, tupleKinds []string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`, blockTypeSupertagEarlier, tupleKinds); err != nil {
		t.Fatalf("announce another build listing %v: %v", tupleKinds, err)
	}
}

// blockTypeSupertagSetRow sets the type of the document_blocks row of the node address to typ,
// leaving its node as it was.
func blockTypeSupertagSetRow(t *testing.T, database *store.Store, address, typ string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update document_blocks set type = $2 where node_id = $1`, address, typ); err != nil {
		t.Fatalf("set the type of the row of %s to %s: %v", address, typ, err)
	}
}

// blockTypeSupertagSetNode sets the supertag of the node of address to supertag in the store
// alone, leaving the row of the block as it was.
func blockTypeSupertagSetNode(t *testing.T, database *store.Store, address, supertag string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update graph_nodes set supertag = $2 where kind = $3 and id = $1`, address, supertag, refs.BlockNodeKind); err != nil {
		t.Fatalf("set the supertag of the node of %s to %s: %v", address, supertag, err)
	}
}

// blockTypeSupertagPlan is the plan of the text of function name, prepared with its argument types
// and run with arguments, as the planner plans it generically with sequential scans off, one line
// a plan node. A prepared statement belongs to the session of the connection that prepared it, and
// the transaction's rollback leaves it there, so the plan holds one connection of the pool from its
// transaction to the deallocate of the statement on that connection, once the rollback has ended
// the transaction and its settings: the connection goes back to the pool holding no statement the
// next plan prepares under the same name.
func blockTypeSupertagPlan(t *testing.T, database *store.Store, name, types, arguments string) string {
	t.Helper()
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the plan of %s: %v", name, err)
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan of %s: %v", name, err)
	}
	prepared := false
	defer func() {
		_ = tx.Rollback(ctx)
		if !prepared {
			return
		}
		if _, err := connection.Exec(ctx, `deallocate blocktype_supertag_plan`); err != nil {
			t.Errorf("deallocate the plan of %s: %v", name, err)
		}
	}()
	for _, statement := range []string{
		`set local enable_seqscan = off`,
		`set local plan_cache_mode = force_generic_plan`,
		`prepare blocktype_supertag_plan (` + types + `) as ` + blockTypeSupertagFunction(t, database, name),
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("plan %s: %s: %v", name, statement, err)
		}
	}
	prepared = true
	rows, err := tx.Query(ctx, `explain (costs off) execute blocktype_supertag_plan (`+arguments+`)`)
	if err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan the plan of %s: %v", name, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the plan of %s: %v", name, err)
	}
	return strings.Join(lines, "\n")
}

// The tuple arm of graph_edges reads a block's type from the block's node, where it read it from
// document_blocks.type (this issue's spec R26): block_pointer_targets reads the type of a
// pointer's source and block_node_listed the type of its target, and a step of every boot moves
// both onto the node's supertag in four databases, each as a deployment meets it.
func TestTheTupleArmReadsABlocksTypeFromItsNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The first database is migrated and seeded, and its newest announcement that names another
	// commit lists no typed_block_supertags: a fleet whose build before moves nothing. The step
	// leaves both functions as the release before has them, so a row that says check leaves both
	// pointers hidden.
	t.Run("a fleet that does not list the capability", func(t *testing.T) {
		database := storetest.Open(t)
		handler, _, deps := newTestServer(t, testServerOptions{store: database})
		issue := blockTypeSupertagSeed(t, handler, database, deps, "TYPEA")
		before := map[string]string{
			"block_pointer_targets": blockTypeSupertagFunction(t, database, "block_pointer_targets"),
			"block_node_listed":     blockTypeSupertagFunction(t, database, "block_node_listed"),
		}
		blockTypeSupertagAnnounce(t, database, []string{"part_of"})
		process := blockTypeSupertagBoot(t, database.Pool.Config().ConnString())
		blockTypeSupertagWaitStep(t, process)
		boot := blockTypeSupertagStop(t, process)
		for name, text := range before {
			if after := blockTypeSupertagFunction(t, database, name); after != text {
				t.Errorf("the boot step restated %s over a fleet that lists no %s:\n%s\nserver log:\n%s", name, blockTypeSupertagCapability, after, boot)
			}
		}
		blockTypeSupertagSetRow(t, database, issue.requirement, "check")
		blockTypeSupertagRequireListed(t, handler, database, issue, false,
			"the pointer from %s to %s: %s answers it %s, want %[5]s: the functions read the row, which says check")
	})

	// The second database is one the test makes with storetest.Empty and boots, so the migrations
	// and the step run over a database the boot created from empty. The step restates both
	// functions, which then read no document_blocks, while block_ref_listed keeps the row.
	t.Run("a database the boot created from empty", func(t *testing.T) {
		url := storetest.Empty(t)
		process := blockTypeSupertagBoot(t, url)
		database, err := store.Open(ctx, url)
		if err != nil {
			t.Fatalf("open the database the boot created: %v", err)
		}
		t.Cleanup(database.Pool.Close)
		testwait.Eventually(t, "the boot step to restate block_pointer_targets and block_node_listed", func() bool {
			return !blockTypeSupertagReadsRow(t, database, "block_pointer_targets") && !blockTypeSupertagReadsRow(t, database, "block_node_listed")
		})
		boot := blockTypeSupertagStop(t, process)
		for _, name := range []string{"block_pointer_targets", "block_node_listed"} {
			if blockTypeSupertagReadsRow(t, database, name) {
				t.Errorf("%s reads document_blocks after the boot step:\n%s\nserver log:\n%s", name, blockTypeSupertagFunction(t, database, name), boot)
			}
			t.Logf("%s after the boot step reads document_blocks: %t\n%s", name, blockTypeSupertagReadsRow(t, database, name), blockTypeSupertagFunction(t, database, name))
		}
		if !blockTypeSupertagReadsRow(t, database, "block_ref_listed") {
			t.Errorf("block_ref_listed does not name document_blocks, which it keeps the row for:\n%s", blockTypeSupertagFunction(t, database, "block_ref_listed"))
		}
		t.Logf("block_ref_listed reads document_blocks: %t\n%s", blockTypeSupertagReadsRow(t, database, "block_ref_listed"), blockTypeSupertagFunction(t, database, "block_ref_listed"))

		handler, _, deps := newTestServer(t, testServerOptions{store: database})
		issue := blockTypeSupertagSeed(t, handler, database, deps, "TYPEB")
		message := "the pointer from %s to %s follows the row's type, not the node's supertag: %s answers it %s"
		// The row says check and the node says requirement: both functions read the node, so both
		// pointers are listed.
		blockTypeSupertagSetRow(t, database, issue.requirement, "check")
		blockTypeSupertagRequireListed(t, handler, database, issue, true, message+" (want %[5]s)")
		// The row says requirement again and the node, in the store alone, says check: both pointers
		// are hidden.
		blockTypeSupertagSetRow(t, database, issue.requirement, "requirement")
		blockTypeSupertagSetNode(t, database, issue.requirement, "check")
		blockTypeSupertagRequireListed(t, handler, database, issue, false, message+" (want %[5]s)")

		// No read gets slower: over 50,000 seeded blocks, after analyze, each restated function
		// reads graph_nodes through its primary key and holds no Seq Scan.
		if _, err := database.Pool.Exec(ctx, `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select 'block', 'SEEDED/doc#b' || g, 'block', 'project', 'live', '{}', now(), '{"kind":"user","id":"alice"}', 0
			from generate_series(1, 50000) g
		`); err != nil {
			t.Fatalf("seed 50,000 blocks: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `analyze graph_nodes`); err != nil {
			t.Fatalf("analyze graph_nodes: %v", err)
		}
		for _, function := range []struct{ name, types, arguments string }{
			{"block_pointer_targets", "text, text", `'SEEDED/doc#b25000', 'serves'`},
			{"block_node_listed", "text[], text", `array['requirement'], 'SEEDED/doc#b25000'`},
		} {
			plan := blockTypeSupertagPlan(t, database, function.name, function.types, function.arguments)
			t.Logf("the plan of %s:\n%s", function.name, plan)
			if strings.Contains(plan, "Seq Scan") {
				t.Errorf("%s reads a block's type by a Seq Scan:\n%s", function.name, plan)
			}
			if !strings.Contains(plan, "graph_nodes_pkey") {
				t.Errorf("%s reads a block's type by no scan of graph_nodes_pkey:\n%s", function.name, plan)
			}
		}
	})

	// The third database is migrated and seeded, its census reads zero, nothing is owed, and its
	// newest announcement that names another commit lists typed_block_supertags: the case a
	// deployment meets. The step restates both functions, and a second boot finds both done and
	// runs no census.
	t.Run("a fleet that lists the capability", func(t *testing.T) {
		database := storetest.Open(t)
		handler, _, deps := newTestServer(t, testServerOptions{store: database})
		blockTypeSupertagSeed(t, handler, database, deps, "TYPEC")
		blockTypeSupertagAnnounce(t, database, []string{blockTypeSupertagCapability})
		process := blockTypeSupertagBoot(t, database.Pool.Config().ConnString())
		testwait.Eventually(t, "the boot step to restate block_pointer_targets and block_node_listed", func() bool {
			return !blockTypeSupertagReadsRow(t, database, "block_pointer_targets") && !blockTypeSupertagReadsRow(t, database, "block_node_listed")
		})
		first := blockTypeSupertagStop(t, process)
		t.Logf("the first boot's log:\n%s", first)
		second := blockTypeSupertagStop(t, blockTypeSupertagBoot(t, database.Pool.Config().ConnString()))
		t.Logf("the second boot's log:\n%s", second)
		for _, name := range []string{"block_pointer_targets", "block_node_listed"} {
			if blockTypeSupertagReadsRow(t, database, name) {
				t.Errorf("%s reads document_blocks after a second boot", name)
			}
		}
		if strings.Contains(second, "block_type") {
			t.Errorf("the second boot, which finds both functions done, names the census of kind block_type in its log:\n%s", second)
		}
	})

	// The fourth database is like the third, but one check's node carries block. Once the step has
	// ended, it has left both functions as they are, and the server log names that block.
	t.Run("a database where one check's node carries block", func(t *testing.T) {
		database := storetest.Open(t)
		handler, _, deps := newTestServer(t, testServerOptions{store: database})
		issue := blockTypeSupertagSeed(t, handler, database, deps, "TYPED")
		blockTypeSupertagSetNode(t, database, issue.check, "block")
		blockTypeSupertagAnnounce(t, database, []string{blockTypeSupertagCapability})
		process := blockTypeSupertagBoot(t, database.Pool.Config().ConnString())
		blockTypeSupertagWaitStep(t, process)
		boot := blockTypeSupertagStop(t, process)
		for _, name := range []string{"block_pointer_targets", "block_node_listed"} {
			if !blockTypeSupertagReadsRow(t, database, name) {
				t.Errorf("the boot step restated %s over a database where the node of %s carries block", name, issue.check)
			}
		}
		if !strings.Contains(boot, issue.check) {
			t.Errorf("the server log does not name the block %s whose node carries block:\n%s", issue.check, boot)
		}
	})

	// No read selects block nodes by the supertag block.
	t.Run("no read selects block nodes by the supertag block", blockTypeSupertagRequireNoBlockRead)
}

// blockTypeSupertagReadPatterns are the forms of a read that compares a supertag with block: SQL
// text or Go over the literal or over refs.BlockSupertag.
var blockTypeSupertagReadPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)supertag\s*(?:=|<>|!=)\s*\(?\s*'block'`),
	regexp.MustCompile(`(?i)supertag\s+(?:not\s+)?in\s*\(\s*'block'`),
	regexp.MustCompile("(?i)supertag\\s*(?:=|<>|!=)\\s*'[`\"]\\s*\\+\\s*(?:refs\\.)?BlockSupertag"),
	regexp.MustCompile(`[Ss]upertag\s*(?:==|!=)\s*(?:(?:refs\.)?BlockSupertag\b|"block")`),
}

// blockTypeSupertagScan is the symbols of the Go source named name that hold a read comparing a
// supertag with block, as `<name>: <symbol> selects block nodes by the supertag block`. The symbol
// is the function holding the read, or the package level where none does.
func blockTypeSupertagScan(t *testing.T, name string, source []byte) []string {
	t.Helper()
	var offsets []int
	for _, pattern := range blockTypeSupertagReadPatterns {
		for _, found := range pattern.FindAllIndex(source, -1) {
			offsets = append(offsets, found[0])
		}
	}
	if len(offsets) == 0 {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var found []string
	for _, offset := range offsets {
		symbol := "(package level)"
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok &&
				fset.Position(function.Pos()).Offset <= offset && offset < fset.Position(function.End()).Offset {
				symbol = function.Name.Name
			}
		}
		found = append(found, fmt.Sprintf("%s: %s selects block nodes by the supertag block", name, symbol))
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// blockTypeSupertagRequireNoBlockRead proves that no read selects block nodes by the supertag
// block (this issue's spec R26): every read that finds block nodes selects them by their kind,
// refs.BlockNodeKind. It scans the Go source of internal/dispatch for a read that compares a
// supertag with block, through refs.BlockSupertag or the literal, and finds none; the same scan
// over a copy of one file with such a read planted finds it, and both readings go into the log.
func blockTypeSupertagRequireNoBlockRead(t *testing.T) {
	var found []string
	scanned := 0
	if err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		found = append(found, blockTypeSupertagScan(t, strings.TrimPrefix(filepath.ToSlash(path), "../"), source)...)
		return nil
	}); err != nil {
		t.Fatalf("walk internal/dispatch: %v", err)
	}
	t.Logf("the scan read %d non-test files of internal/dispatch and found %d reads that select block nodes by the supertag block", scanned, len(found))
	for _, finding := range found {
		t.Error(finding)
	}

	source, err := os.ReadFile("../refs/block_nodes.go")
	if err != nil {
		t.Fatalf("read a file to plant a read in: %v", err)
	}
	planted := append(slices.Clone(source), []byte("\n// plantedBlockRead selects block nodes by the supertag block.\n"+
		"func plantedBlockRead() string {\n\treturn \"select id from graph_nodes where kind = 'x' and supertag = '\" + BlockSupertag + \"'\"\n}\n")...)
	reading := blockTypeSupertagScan(t, "refs/block_nodes.go", planted)
	t.Logf("the scan over a copy of refs/block_nodes.go with a read planted finds: %v", reading)
	if !slices.Contains(reading, "refs/block_nodes.go: plantedBlockRead selects block nodes by the supertag block") {
		t.Errorf("the scan finds no read in a copy of refs/block_nodes.go with one planted: %v", reading)
	}
}
