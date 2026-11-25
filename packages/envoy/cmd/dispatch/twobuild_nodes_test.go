//go:build twobuild

// The node half of the two-build suite: what a document's node is left as when the build before
// this one, which writes no node, creates or removes a document beside this build during a roll
// (twobuild_harness_test.go holds the harness every half starts its builds with).

package main

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// A document's node is addressed by its ref_key, and the build before this one writes none, so
// the orders a roll can mix leave a node the census has to name, each with its cure:
//
//   - This build removes a document, stating its node gone, and the previous build makes a
//     document of the same issue and name, which takes the same address: the row stands behind a
//     gone node. Whichever of this build's two runs of the backfill's object pass reaches the row
//     first makes the node live from the row's own content: the block-index tick's (docs'
//     writeObjectNodesAnotherBuildWrote) or the one that opens the decisions step's phase 1
//     (graphcensus's BackfillAsks). So the roll needs no second envoy-dispatch backfill-graph-store.
//   - This build makes a document, writing its node, and the previous build removes it: the node
//     stands live behind no row. The census names it orphaned, and the backfill, which reads no
//     node no row stands behind, leaves it as it is. This build's block-index tick, in the object
//     pass it runs once the previous build has stopped, states it gone as this build's own removal
//     would have (graphcensus's StateOrphanedGone), and the census then names it no more.
//   - The previous build removes this build's document and this build makes it again: the create
//     finds the orphaned node at the free address and states it gone, then live, under its own
//     event, and the census names nothing.
//   - The previous build removes this build's document and makes it again itself: the live node is
//     the first row's, by its create event, so the second row stands behind another row's node.
//     Whichever of this build's two object passes reaches the row first states the node gone, then
//     live, under the second row's own event, with no backfill run.
//
// The tick runs under the block-index lease, which both builds' processes compete for each period,
// and a period the previous build serves is one this build's tick waits out: while both serve, the
// tick cures the first and fourth orders' rows only in a period this build happens to win. The
// decisions step takes no block-index lease: it ticks every block-index interval under a lock of
// its own, and while a process of another build serves, each tick reads whether a row stands
// unmarked and runs phase 1 where one does, so it can cure those rows before the previous build
// stops. The tick and the decisions step start together at boot and each waits its interval after
// its own pass, so either can reach the rows first, and the other then finds no row left to write.
// So the test reads their cure once the previous build has stopped, as the end of a roll stops it:
// the decisions step may have cured them by then, and this build's next tick serves the period,
// finds the fleet alone after another build or joins its build's cursor, and runs the object pass.
// The second order runs first, since the backfill it runs would cure those two rows too, and every
// census read before the end reads only the lines that name its own row. The cure is read three
// ways: each row's node is live, the census names no row at all, and this build's two passes
// together wrote at least the two rows' nodes (the tick's object_nodes= and the decisions step's
// rows= of kind=artifact), with no backfill run after those rows were made.
//
// After the last cure the census names no project row or node at all. Each order has its control.
// The first and fourth orders' is both writers taken out - the tick's object pass out of docs'
// ReconcileBlockIndex and backfillObjectNodes out of graphcensus's BackfillAsks - after which the
// census names both rows gone for as long as the test waits. The second's is this file run in the
// tree at the branch's head before these classes and their cures, where this build's removal
// states nothing gone and the census prints no orphaned field, so it fails at its first census.
// The third's is the gone step taken out of this build's create (api's storeArtifact): the node
// then keeps the removed row's live statement, and the census names the new row gone.
func TestTwoBuildsLeaveADocumentsNodeTheCensusNamesAndItsCureClears(t *testing.T) {
	builds := newTwoBuilds(t)
	builds.startPrevious(t, nil)
	builds.startCurrent(t, nil)
	previous, current := builds.previous(), builds.current()
	t.Logf("the %s serves beside the %s", builds.previousName(), builds.currentName())
	project := uniqueProjectKey(t, "NODE")
	current.createProject(t, project, "Two builds' document nodes")
	issue, _ := createDocument(t, current, project, "Documents two builds remove and make", "The spec.\n")
	// The rows this build cures, read once the previous build has stopped.
	var forTheTick []string

	t.Run("this build makes a document and the previous build removes it", func(t *testing.T) {
		document := makeTypedDocument(t, current, builds.currentName(), issue, "Removed by the previous build.md")
		requireDocumentNodeState(t, builds, document.ref, graphstore.StateLive)
		removeTypedDocument(t, previous, builds.previousName(), document)
		orphaned := "artifact " + document.ref + ": orphaned; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone"
		census := runTwoBuildStoreCensus(t, builds)
		requireProjectLine(t, census, "artifact", "orphaned=1")
		if !requireIncompleteLinesNaming(t, census, document.ref, orphaned) {
			t.FailNow()
		}
		runCurrentCommand(t, builds, []int{0}, "backfill-graph-store")
		requireDocumentNodeState(t, builds, document.ref, graphstore.StateLive)
		requireIncompleteLinesNaming(t, runTwoBuildStoreCensus(t, builds), document.ref, orphaned)
	})

	t.Run("this build removes a document and the previous build makes it again", func(t *testing.T) {
		removed := makeTypedDocument(t, current, builds.currentName(), issue, "Made again.md")
		removeTypedDocument(t, current, builds.currentName(), removed)
		requireDocumentNodeState(t, builds, removed.ref, graphstore.StateGone)
		again := makeTypedDocument(t, previous, builds.previousName(), issue, "Made again.md")
		if again.ref != removed.ref {
			t.Fatalf("the %s made the document at %s, want the removed one's address %s, so this row proves nothing",
				builds.previousName(), again.ref, removed.ref)
		}
		forTheTick = append(forTheTick, again.ref)
	})

	t.Run("the previous build removes this build's document and this build makes it again", func(t *testing.T) {
		document := makeTypedDocument(t, current, builds.currentName(), issue, "Made again here.md")
		removeTypedDocument(t, previous, builds.previousName(), document)
		again := makeTypedDocument(t, current, builds.currentName(), issue, "Made again here.md")
		if again.ref != document.ref {
			t.Fatalf("this build made the document at %s, want the removed one's address %s, so this row proves nothing", again.ref, document.ref)
		}
		requireDocumentNodeState(t, builds, again.ref, graphstore.StateLive)
		requireIncompleteLinesNaming(t, runTwoBuildStoreCensus(t, builds), again.ref)
	})

	t.Run("the previous build removes this build's document and makes it again", func(t *testing.T) {
		document := makeTypedDocument(t, current, builds.currentName(), issue, "Made again there.md")
		removeTypedDocument(t, previous, builds.previousName(), document)
		again := makeTypedDocument(t, previous, builds.previousName(), issue, "Made again there.md")
		if again.ref != document.ref {
			t.Fatalf("the %s made the document at %s, want the removed one's address %s, so this row proves nothing",
				builds.previousName(), again.ref, document.ref)
		}
		forTheTick = append(forTheTick, again.ref)
	})

	t.Run("the previous build stops and this build cures what it left", func(t *testing.T) {
		if len(forTheTick) != 2 {
			t.Fatalf("the orders left %d row(s) for this build to cure, want the first and fourth orders' two", len(forTheTick))
		}
		t.Logf("before the previous build stops, the census names %q", incompleteLines(runTwoBuildStoreCensus(t, builds)))
		var commits []string
		if err := builds.database.Pool.QueryRow(context.Background(), `
			select coalesce(array_agg(commit || ' heartbeat ' || round(extract(epoch from now() - heartbeat_at))::text || 's ago' order by started_at), '{}')
			from server_processes
		`).Scan(&commits); err != nil {
			t.Fatalf("read the registry: %v", err)
		}
		t.Logf("the registry holds %q", commits)
		stop(t, previous)
		for _, ref := range forTheTick {
			awaitThisBuildsCure(t, builds, ref)
		}
		objectNodes := regexp.MustCompile(`\bobject_nodes=([0-9]+)\b`)
		documentRows := regexp.MustCompile(`\bkind=artifact\b.*\brows=([0-9]+)\b`)
		var ticks, steps []string
		written := 0
		for _, line := range strings.Split(current.Output.String(), "\n") {
			var field []string
			switch {
			case strings.Contains(line, `msg="dispatch: block index"`):
				ticks = append(ticks, line)
				field = objectNodes.FindStringSubmatch(line)
			case strings.Contains(line, `msg="dispatch: decisions step stated rows written before this release on the store"`):
				if field = documentRows.FindStringSubmatch(line); field != nil {
					steps = append(steps, line)
				}
			}
			if field != nil {
				count, _ := strconv.Atoi(field[1])
				written += count
			}
		}
		t.Logf("this build's block-index ticks: %q", ticks)
		t.Logf("this build's decisions step's document rows: %q", steps)
		if written < len(forTheTick) {
			t.Errorf("this build's block-index ticks (object_nodes=) and its decisions step's phase 1 (rows= of kind=artifact) wrote %d node(s) in all, want at least the %d the orders left: no envoy-dispatch backfill-graph-store ran after those rows were made, so this build's own passes have to be what cured them",
				written, len(forTheTick))
		}
	})
}

// noDocumentNode is what requireDocumentNodeState reads for an address the store holds no node at.
const noDocumentNode = "(no node)"

// requireDocumentNodeState fails unless the store holds the document node at ref in state, or
// holds none when state is noDocumentNode.
func requireDocumentNodeState(t *testing.T, builds *twoBuilds, ref, state string) {
	t.Helper()
	var held string
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select coalesce((select state from graph_nodes where kind = $1 and id = $2), $3)
	`, graphstore.KindArtifact, ref, noDocumentNode).Scan(&held); err != nil {
		t.Fatalf("read the node of %s: %v", ref, err)
	}
	if held != state {
		t.Errorf("the node of %s is %s, want %s", ref, held, state)
	}
}

// awaitThisBuildsCure waits, for at most three block-index intervals and a margin, until the store
// holds the live node of the row at ref and the census names no project row or node at all. The
// test calls it once the previous build has stopped. No backfill is run: this build's own passes,
// the block-index tick's object pass and the decisions step's phase 1, are the cure, and the
// decisions step's can land before the stop. It fails naming what the census still names once the
// wait is over.
func awaitThisBuildsCure(t *testing.T, builds *twoBuilds, ref string) {
	t.Helper()
	deadline := time.Now().Add(3*docs.BlockIndexReconcileInterval + 30*time.Second)
	for {
		census := runTwoBuildStoreCensus(t, builds)
		var held string
		if err := builds.database.Pool.QueryRow(context.Background(), `
			select coalesce((select state from graph_nodes where kind = $1 and id = $2), $3)
		`, graphstore.KindArtifact, ref, noDocumentNode).Scan(&held); err != nil {
			t.Fatalf("read the node of %s: %v", ref, err)
		}
		if held == graphstore.StateLive && len(incompleteLines(census)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("this build left the node of %s %s and the census naming %q after a wait of %s, with no backfill run: neither the block-index tick's object pass nor the decisions step's phase 1 cured it",
				ref, held, incompleteLines(census), 3*docs.BlockIndexReconcileInterval+30*time.Second)
		}
		time.Sleep(5 * time.Second)
	}
}

// incompleteLinesNaming is every `census: project incomplete` line of the census that names the
// document at ref, without that head.
func incompleteLinesNaming(census, ref string) []string {
	var lines []string
	for _, line := range incompleteLines(census) {
		if strings.HasPrefix(line, "artifact "+ref+": ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// requireIncompleteLinesNaming fails the test, and answers false, unless the census named exactly
// want as incomplete of the document at ref, in that order; lines naming other rows are not read.
func requireIncompleteLinesNaming(t *testing.T, census, ref string, want ...string) bool {
	t.Helper()
	got := incompleteLinesNaming(census, ref)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the census named %q as incomplete of %s, want %q:\n%s", got, ref, want, census)
		return false
	}
	return true
}

// runTwoBuildStoreCensus runs this tree's `envoy-dispatch census --store` and answers what it printed. It
// takes exit 0, a census that passed, and exit 1, one that refused: on a database holding no role,
// page or person the store census refuses those kinds for having no row to check, which says
// nothing of a document's node. Exit 2 is a census that could not be taken and printed no report,
// and fails the test, as does a report holding no line for the document kind: a cure is read from
// the report alone.
func runTwoBuildStoreCensus(t *testing.T, builds *twoBuilds) string {
	t.Helper()
	census := runCurrentCommand(t, builds, []int{0, 1}, "census", "--store")
	if !strings.Contains(census, "\ncensus: project kind=artifact ") && !strings.HasPrefix(census, "census: project kind=artifact ") {
		t.Fatalf("the census printed no project line for documents, so it read no document node:\n%s", census)
	}
	return census
}

// runCurrentCommand runs this tree's envoy-dispatch with args against the pair's database and
// answers its output, failing the test unless it exits with one of wants.
func runCurrentCommand(t *testing.T, builds *twoBuilds, wants []int, args ...string) string {
	t.Helper()
	return runCommand(t, builds, wants, strings.Join(args, " "), exec.Command(builds.binary, args...))
}

func runCommand(t *testing.T, builds *twoBuilds, wants []int, shown string, command *exec.Cmd) string {
	t.Helper()
	command.Env = append(command.Environ(), "DATABASE_URL="+builds.databaseURL(t))
	output, err := command.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("run %s: %v", shown, err)
	}
	if !slices.Contains(wants, code) {
		t.Fatalf("%s exited %d, want one of %v:\n%s", shown, code, wants, output)
	}
	return string(output)
}

// requireProjectLine fails unless the census printed a `census: project kind=<kind>` line holding
// every field of want.
func requireProjectLine(t *testing.T, census, kind string, want ...string) {
	t.Helper()
	for _, line := range strings.Split(census, "\n") {
		if !strings.HasPrefix(line, "census: project kind="+kind+" ") {
			continue
		}
		fields := strings.Fields(line)
		for _, field := range want {
			if !slices.Contains(fields, field) {
				t.Errorf("the census's %s line reads %q, want %s", kind, line, field)
			}
		}
		return
	}
	t.Errorf("the census printed no line for the project kind %s:\n%s", kind, census)
}

// incompleteLines is every `census: project incomplete` line of the census, without that head.
func incompleteLines(census string) []string {
	var lines []string
	for _, line := range strings.Split(census, "\n") {
		if rest, ok := strings.CutPrefix(line, "census: project incomplete "); ok {
			lines = append(lines, rest)
		}
	}
	return lines
}
