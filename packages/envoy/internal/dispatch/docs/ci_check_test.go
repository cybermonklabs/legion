package docs

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ciCheckKept is the attributes of a check CI proves that keeps every rule while no result is
// recorded against it.
const ciCheckKept = `negative_control="Break the build and watch the run fail." ci_check="go-test"`

// ciCheckOf is check block id carrying attributes as markdown writes them.
func ciCheckOf(id, attributes string) string {
	return ":::check{#" + id + " " + attributes + "}\nThe server builds at the head.\n:::\n"
}

// invalidCheck is one block.invalid event, the members a check's refusal fills in.
type invalidCheck struct {
	BlockID, Reason, BlockType string
	Version                    int
}

// invalidChecks is every block.invalid event of CORE-1, oldest first.
func invalidChecks(t *testing.T, database *store.Store) []invalidCheck {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select payload->>'block_id', payload->>'reason', coalesce(payload->>'block_type', ''), (payload->>'version')::int
		from events where type = 'block.invalid' and issue_key = 'CORE-1' order by id
	`)
	if err != nil {
		t.Fatalf("read the block.invalid events: %v", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (invalidCheck, error) {
		var event invalidCheck
		err := row.Scan(&event.BlockID, &event.Reason, &event.BlockType, &event.Version)
		return event, err
	})
	if err != nil {
		t.Fatalf("scan the block.invalid events: %v", err)
	}
	return found
}

// settleBrowserWrite settles artifactID's room once every update the room holds is durable, as the
// room's settle timer does after a browser's write.
func settleBrowserWrite(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	waitForLandedAppends(t, service, artifactID)
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	state.mu.Lock()
	failed, failures := state.failed, state.settleFailures
	state.mu.Unlock()
	if failed != nil || failures != 0 {
		t.Fatalf("settlement failed (%v, %d failures), want it settled", failed, failures)
	}
}

// liveCIChecks is the ci_check of each check block of the live document by its id, "" for one
// carrying none.
func liveCIChecks(t *testing.T, service *Service, artifactID string) map[string]string {
	t.Helper()
	checks := map[string]string{}
	for _, node := range checkBlockNodes(liveTree(t, service, artifactID)) {
		check := CheckBlockOf(node)
		checks[check.BlockID] = check.CICheck
	}
	return checks
}

// A browser's write is never refused, so settlement holds the check blocks it leaves to the four
// rules: each block carrying a ci_check that breaks one - a block holding a recorded result, a run,
// a when of after_merge, no negative control - loses its ci_check in the live document and in the
// version settlement writes, and one block.invalid names the block, the rule and block_type check
// at that version. A block keeping every rule keeps its ci_check. A later settlement of the same
// document repairs nothing again and adds no event.
func TestSettlementDeletesTheCICheckOfEachBrokenCheckABrowserWrote(t *testing.T) {
	database, service := blockIndexService(t)
	checks := writeIndexedDocument(t, database, service, "verification", ciCheckOf("c1", `negative_control="Break the build and watch the run fail."`))
	if _, err := database.Pool.Exec(context.Background(), `
		insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, role, commit_sha, verdict, actor)
		values ('CORE-1', 'CORE-1', $1, 'c1', 1, 'digest', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"alice"}')
	`, checks); err != nil {
		t.Fatalf("record a result against c1: %v", err)
	}

	browserReplaceText(t, service, checks, strings.Join([]string{
		ciCheckOf("c1", ciCheckKept),
		ciCheckOf("c2", `run="go test ./..." `+ciCheckKept),
		ciCheckOf("c3", `when="after_merge" `+ciCheckKept),
		ciCheckOf("c4", `ci_check="go-test"`),
		ciCheckOf("c5", ciCheckKept),
	}, "\n"), model.Actor{Kind: "user", ID: "bob"})
	settleBrowserWrite(t, service, checks)

	want := []invalidCheck{
		{BlockID: "c1", Reason: ciCheckRuleRecorded, BlockType: "check", Version: 2},
		{BlockID: "c2", Reason: ciCheckRuleRun, BlockType: "check", Version: 2},
		{BlockID: "c3", Reason: ciCheckRuleWhen, BlockType: "check", Version: 2},
		{BlockID: "c4", Reason: ciCheckRuleNegativeControl, BlockType: "check", Version: 2},
	}
	if got := invalidChecks(t, database); !reflect.DeepEqual(got, want) {
		t.Fatalf("block.invalid events = %+v, want %+v", got, want)
	}
	wantLive := map[string]string{"c1": "", "c2": "", "c3": "", "c4": "", "c5": "go-test"}
	if got := liveCIChecks(t, service, checks); !reflect.DeepEqual(got, wantLive) {
		t.Fatalf("the live document's ci_checks = %v, want %v", got, wantLive)
	}
	var version int
	var markdown string
	if err := database.Pool.QueryRow(context.Background(), `
		select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, checks).Scan(&version, &markdown); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	if version != 2 || strings.Count(markdown, `ci_check=`) != 1 || !strings.Contains(markdown, `:::check{#c5 `) {
		t.Fatalf("version %d stores %q, want version 2 holding c5's ci_check alone", version, markdown)
	}
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		if !strings.Contains(markdown, ":::check{#"+id) {
			t.Fatalf("version 2 stores %q, want check %s kept", markdown, id)
		}
	}

	// A later browser edit elsewhere settles again: nothing breaks a rule now, so no block is
	// repaired and no event is added.
	connection := service.nextConnection.Add(1)
	service.addConnection(checks, connection, model.Actor{Kind: "user", ID: "bob"})
	editLiveTree(t, service, checks, func(tree *pmdoc.Node) *pmdoc.Node {
		later, err := pmdoc.Parse("Later.\n")
		if err != nil {
			t.Fatalf("parse the browser's paragraph: %v", err)
		}
		tree.Children = append(tree.Children, later.Children...)
		return tree
	})
	service.removeConnection(checks, connection)
	settleBrowserWrite(t, service, checks)
	if got := invalidChecks(t, database); !reflect.DeepEqual(got, want) {
		t.Fatalf("block.invalid events after a second settlement = %+v, want the first settlement's %+v alone", got, want)
	}
	if got := liveCIChecks(t, service, checks); !reflect.DeepEqual(got, wantLive) {
		t.Fatalf("the live document's ci_checks after a second settlement = %v, want %v", got, wantLive)
	}
}

// A repair lands only on a block of the type it was made on: a check block's repair takes no
// effect on a block retyped since settlement read the tree, though it still holds the judged
// attributes, nor on a check block a peer changed since, which the settlement that change
// schedules judges again.
func TestACICheckRepairLandsOnlyOnTheCheckItJudged(t *testing.T) {
	broken := ciCheckOf("c1", `run="go test ./..." `+ciCheckKept)
	read, err := pmdoc.Parse(broken)
	if err != nil {
		t.Fatalf("parse the settled document: %v", err)
	}
	judged := CheckAttributesOf(checkBlockNodes(read)[0])
	reconciled := settlementReconciliation{repairs: []blockRepair{{blockID: "c1", blockType: "check", set: deleteJudgedCICheck(judged)}}}

	for _, test := range []struct {
		name, live string
		retyped    bool
		changed    bool
	}{
		{name: "the judged check", live: broken, changed: true},
		{name: "a check whose run a peer removed", live: ciCheckOf("c1", ciCheckKept)},
		{name: "the check retyped to a requirement", live: broken, retyped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			live, err := pmdoc.Parse(test.live)
			if err != nil {
				t.Fatalf("parse the live document: %v", err)
			}
			if test.retyped {
				live.Children[0].Type = "requirement"
			}
			if changed := reconciled.repairLive(live); changed != test.changed {
				t.Fatalf("repairLive changed the document: %v, want %v", changed, test.changed)
			}
			if _, carries := live.Children[0].Attrs["ci_check"]; carries == test.changed {
				t.Fatalf("the live block carries ci_check: %v, want %v", carries, !test.changed)
			}
		})
	}
}
