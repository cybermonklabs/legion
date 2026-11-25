package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// keepSumSigningKey is the one cookie key every process of the Keep pass's test takes, as every
// process of a deployment does, so no process waits out a live window of the one before it
// (processes.Register).
const keepSumSigningKey = "keep-sum-key"

// keepSumPassMatch is the Keep pass's line (graphcensus.KeepPassLine) as a reader of the process's
// output matches it, and keepSumRewrote the count of Keeps it names as rewritten.
var (
	keepSumPassMatch = `msg="` + graphcensus.KeepPassLine + `"`
	keepSumRewrote   = regexp.MustCompile(` rewrote=(\d+)\b`)
)

// keepSumEntry is what this test reads of one entry of GET /api/v1/issues/{key}/decisions.
type keepSumEntry struct {
	Node struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"node"`
	TextSHA256    string              `json:"text_sha256"`
	Kept          *model.DecisionKeep `json:"kept"`
	WaitingReview bool                `json:"waiting_review"`
}

// keepSumDecisions is each decision entry of the issue key, as alice reads it, by its block id.
func keepSumDecisions(t *testing.T, p *dispatchProcess, key string) map[string]keepSumEntry {
	t.Helper()
	var read struct {
		Entries []keepSumEntry `json:"entries"`
	}
	answer := p.call(t, http.MethodGet, "/api/v1/issues/"+key+"/decisions", "", http.StatusOK)
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatalf("decode the Decisions read of %s: %v: %s", key, err, answer)
	}
	entries := map[string]keepSumEntry{}
	for _, entry := range read.Entries {
		if _, block, found := strings.Cut(entry.Node.ID, "#"); found && entry.Node.Kind == refs.BlockNodeKind {
			entries[block] = entry
		}
	}
	return entries
}

// keepSumEdit applies ops to the spec of the issue key as alice, through the edit route.
func keepSumEdit(t *testing.T, p *dispatchProcess, key string, ops ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ops": ops, "summary": "the Keep pass's test"})
	if err != nil {
		t.Fatal(err)
	}
	p.call(t, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", string(body), http.StatusOK)
}

// shapedSumOf is the sum a release before this one gave decision block of markdown: the decision
// rendered alone through pmdoc.Render, with its shaped attribute and without the readings it closes
// with, as refs.DecisionWords rendered it before it left shaped out.
func shapedSumOf(t *testing.T, markdown, block string) string {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}
	var decision *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == refs.DecisionSupertag && refs.TypedBlockID(node) == block {
			decision = node
		}
		return decision == nil
	})
	if decision == nil {
		t.Fatalf("the spec holds no decision %s:\n%s", block, markdown)
	}
	content, _ := pmdoc.TypedContent(decision)
	own := *decision
	own.Children = content
	words, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{&own}})
	if err != nil {
		t.Fatalf("render %s: %v", block, err)
	}
	if !strings.Contains(words, "shaped=") {
		t.Fatalf("%s renders %q, with no shaped, so this case proves nothing", block, words)
	}
	return decisionread.TextSum(words)
}

// keepSumCensus is the count the store census of this build, `envoy-dispatch census --store`,
// reads for the census kind keep_shaped_sum: the Keeps the Keep pass still owes a write. The census
// may refuse a database this small for kinds it holds no row of, so its exit code is not read.
func keepSumCensus(t *testing.T, databaseURL string) int64 {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(cmdtest.Build(t, "envoy-dispatch"), "census", "--store")
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL, "DISPATCH_NATS_DISABLED=1"}
	sort.Strings(cmd.Env)
	out, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		t.Fatalf("run the store census: %v\n%s", err, out)
	}
	owed := regexp.MustCompile(`(?m)^census: store kind=keep_shaped_sum owed_rows=([0-9,]+) `).FindSubmatch(out)
	if owed == nil {
		t.Fatalf("the store census printed no owed_rows of kind keep_shaped_sum:\n%s", out)
	}
	count, err := strconv.ParseInt(strings.ReplaceAll(string(owed[1]), ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("read the census's count %q: %v", owed[1], err)
	}
	return count
}

// The boot's backfill rewrites the stored sum of each Keep a release before this one made on a
// decision that carries shaped, where that sum is the sum of the decision's words with shaped,
// which this build leaves out of them (R32 of dispatch://INFRA-1253's spec): the check
// c-keep-sum-pass.
//
// A process of this build writes, as alice, an issue whose spec holds requirement r1 and decisions
// D1 and D2, writes the shaped attribute of each through the edit route's retype, keeps both
// through the Keep route, and then changes D2's words, so D2's Keep no longer matches. With the
// process stopped, the test writes each Keep's sum by SQL as the release before summed it, over the
// words with shaped: over D1's current words, and over D2's words before alice's change, into the
// decision_keeps row and as the node's kept_text_sum. The store census then reads 1 for the kind
// keep_shaped_sum, D1's Keep.
//
// A second process over the same database logs the Keep pass's line naming 1 Keep rewritten
// before its boot backfill's done line. D1's row and its kept_text_sum then hold the text_sha256
// the Decisions read answers for D1, kept by alice and waiting for no review; D2's hold the sum the
// test wrote, and D2 waits for review. The census reads 0. A third process's Keep pass rewrites 0
// Keeps, and neither Keep's stored sums move.
func TestTheBootBackfillRecomputesTheKeepSumsOfShapedDecisions(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	ctx := context.Background()
	start := func() *dispatchProcess {
		t.Helper()
		p := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: keepSumSigningKey,
			Env: map[string]string{"DISPATCH_TEST_HOOKS": "1"}})
		p.waitHealthy(t)
		return p
	}
	stop := func(p *dispatchProcess) {
		t.Helper()
		p.Terminate(t)
		p.WaitExit(t, "SIGTERM")
	}
	backfilled := func(p *dispatchProcess) int {
		t.Helper()
		for deadline := time.Now().Add(300 * time.Second); !p.logged(bootBackfillDoneMatch); time.Sleep(2 * time.Second) {
			if time.Now().After(deadline) {
				t.Fatalf("the boot backfill logged no done line within 300 s:\n%s", p.Output.String())
			}
		}
		lines := p.logLines(keepSumPassMatch)
		if len(lines) == 0 {
			t.Fatalf("the boot backfill logged no line of the Keep pass:\n%s", p.Output.String())
		}
		named := keepSumRewrote.FindStringSubmatch(lines[0])
		if named == nil {
			t.Fatalf("the Keep pass's line names no count of Keeps it rewrote: %s", lines[0])
		}
		rewrote, _ := strconv.Atoi(named[1])
		t.Logf("the Keep pass: %s", lines[0])
		return rewrote
	}

	first := start()
	first.call(t, http.MethodPost, "/api/v1/projects", `{"key":"KEEP","name":"Keep sums"}`, http.StatusCreated)
	key, spec := first.createIssueIn(t, "KEEP", "Kept decisions", ":::requirement{#r1}\nThe store keeps every decision.\n:::\n\n"+
		":::decision{#d1}\nWe keep the node store.\n:::\n\n:::decision{#d2}\nWe shard the log.\n:::\n")
	pointer := []string{"dispatch://" + key + "/spec#b-r1"}
	keepSumEdit(t, first, key,
		map[string]any{"op": "retype", "block": "d1", "type": "decision", "attributes": map[string]any{"shaped": pointer}},
		map[string]any{"op": "retype", "block": "d2", "type": "decision", "attributes": map[string]any{"shaped": pointer}})
	for block, entry := range keepSumDecisions(t, first, key) {
		first.call(t, http.MethodPost, "/api/v1/artifacts/"+spec+"/blocks/"+block+"/keep",
			fmt.Sprintf(`{"text_sha256":%q}`, entry.TextSHA256), http.StatusOK)
	}
	d2Before := shapedSumOf(t, first.documentText(t, spec), "d2")
	keepSumEdit(t, first, key, map[string]any{"op": "replace", "find": "We shard the log.", "with": "We split the log."})
	text := first.documentText(t, spec)
	d1Old := shapedSumOf(t, text, "d1")
	if d2Now := shapedSumOf(t, text, "d2"); d2Now == d2Before {
		t.Fatalf("D2's words with shaped sum to %s before and after alice's change, so this case proves nothing", d2Now)
	}
	entries := keepSumDecisions(t, first, key)
	d1, d2 := entries["d1"], entries["d2"]
	if d1.Node.ID == "" || d2.Node.ID == "" || d1.TextSHA256 == d1Old {
		t.Fatalf("the Decisions read answers D1 %+v and D2 %+v, and D1's words with shaped sum to %s: this case proves nothing", d1, d2, d1Old)
	}
	stop(first)

	// The release before's Keeps, each over the words with shaped.
	for _, kept := range []struct {
		block, node, sum string
	}{{"d1", d1.Node.ID, d1Old}, {"d2", d2.Node.ID, d2Before}} {
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `update decision_keeps set text_sha256 = $3 where artifact_id = $1::uuid and block_id = $2`,
				spec, kept.block, kept.sum)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%d decision_keeps rows of %s, want alice's one", tag.RowsAffected(), kept.block)
			}
			return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
				FromKind: refs.BlockNodeKind, FromID: kept.node, Kind: refs.ValueKeptTextSum,
				ToKind: graphstore.KindValue, ToID: kept.sum, At: time.Now().UTC(),
				Who: model.Actor{Kind: "user", ID: dispatchTestLogin}, SourceEvent: graphstore.NoEvent, Asserted: true,
			}})
		}); err != nil {
			t.Fatalf("write the Keep of %s as the release before summed it: %v", kept.block, err)
		}
	}
	if owed := keepSumCensus(t, databaseURL); owed != 1 {
		t.Errorf("census kind keep_shaped_sum reads %d, want %d", owed, 1)
	}

	stored := func(block, node string) (row, value string) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `select text_sha256 from decision_keeps where artifact_id = $1::uuid and block_id = $2`,
			spec, block).Scan(&row); err != nil {
			t.Fatalf("read the Keep of %s: %v", block, err)
		}
		value, _, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, node, refs.ValueKeptTextSum)
		if err != nil {
			t.Fatal(err)
		}
		return row, value
	}
	second := start()
	if rewrote := backfilled(second); rewrote != 1 {
		t.Errorf("the Keep pass rewrote %d Keeps, want %d", rewrote, 1)
	}
	entries = keepSumDecisions(t, second, key)
	d1Row, d1Value := stored("d1", d1.Node.ID)
	if read := entries["d1"]; read.WaitingReview || read.Kept == nil || read.Kept.KeptBy != dispatchTestLogin || d1Row != read.TextSHA256 {
		t.Errorf("the Keep of %s reads waiting_review %t after the boot backfill, stored %s, want %s (its Keep %+v)",
			d1.Node.ID, read.WaitingReview, d1Row, read.TextSHA256, read.Kept)
	}
	if d1Value != entries["d1"].TextSHA256 {
		t.Errorf("the kept_text_sum of %s reads %s after the boot backfill, want %s", d1.Node.ID, d1Value, entries["d1"].TextSHA256)
	}
	d2Row, d2Value := stored("d2", d2.Node.ID)
	if d2Row != d2Before || d2Value != d2Before {
		t.Errorf("the Keep pass rewrote the Keep of %s, which did not match: %s to %s and %s", d2.Node.ID, d2Before, d2Row, d2Value)
	}
	if !entries["d2"].WaitingReview {
		t.Errorf("%s reads waiting_review false after the boot backfill, want true: its Keep did not match", d2.Node.ID)
	}
	stop(second)
	if owed := keepSumCensus(t, databaseURL); owed != 0 {
		t.Errorf("census kind keep_shaped_sum reads %d, want %d", owed, 0)
	}

	third := start()
	if rewrote := backfilled(third); rewrote != 0 {
		t.Errorf("the Keep pass rewrote %d Keeps, want %d", rewrote, 0)
	}
	stop(third)
	for _, kept := range []struct{ block, node, row, value string }{
		{"d1", d1.Node.ID, d1Row, d1Value}, {"d2", d2.Node.ID, d2Row, d2Value},
	} {
		if row, value := stored(kept.block, kept.node); row != kept.row || value != kept.value {
			t.Errorf("the third boot's Keep pass moved the Keep of %s from %s and %s to %s and %s", kept.node, kept.row, kept.value, row, value)
		}
	}
}
