//go:build twobuild

// The node half of the two-build suite: what a document's node is left as when the build before
// this one creates or removes a document beside this build during a roll, whichever of the two
// writers of document nodes that build is (twobuild_harness_test.go holds the harness every half
// starts its builds with).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

// A document's node is addressed by its ref_key. The test first reads, before this build starts,
// which of two writers the previous build is (previousWritesDocumentNodes), and holds each order
// to what that writer leaves. A build from before document nodes (#5491), such as the newest
// release CI's dispatch-two-builds job pairs this tree with, writes no document node and states
// none gone. A build that writes them, such as this tree built as the previous build in the merge
// group's dispatch-two-builds-same, writes a document's node in the transaction that makes its row
// and states it gone in the one that removes it.
//
// Beside a build that writes no node, the orders a roll can mix leave a node the census has to
// name, each with its cure:
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
// Beside a build that writes document nodes, no order leaves this build anything to cure: the
// previous build's removal states the node gone and its create states it live under the row's own
// event, as this build's do. Each order reads its node and the census's lines naming its row right
// after the order's last write, with no backfill run and neither of this build's passes waited
// for: the first order's node is live, the second's gone and named orphaned by no census, and the
// third's and fourth's live, and once the previous build stops the census names no project row or
// node at all.
//
// Each order beside a build that writes no node has its control. The first and fourth orders' is
// both of this build's passes taken out - the tick's object pass out of docs' ReconcileBlockIndex
// and backfillObjectNodes out of graphcensus's BackfillAsks - after which the census names both
// rows gone for as long as the test waits. The second's is this file run in main's tree before
// #5491, where this build's removal states nothing gone and the census prints no orphaned field,
// so it fails at its first census. The third's is the gone step taken out of this build's create
// (api's storeArtifact): the node then keeps the removed row's live statement, and the census
// names the new row gone.
func TestTwoBuildsLeaveADocumentsNodeTheCensusNamesAndItsCureClears(t *testing.T) {
	builds := newTwoBuilds(t)
	builds.startPrevious(t, nil)
	writesNodes := previousWritesDocumentNodes(t, builds)
	t.Logf("the %s writes document nodes: %t", builds.previousName(), writesNodes)
	builds.startCurrent(t, nil)
	previous, current := builds.previous(), builds.current()
	t.Logf("the %s serves beside the %s", builds.previousName(), builds.currentName())
	project := uniqueProjectKey(t, "NODE")
	current.createProject(t, project, "Two builds' document nodes")
	issue, _ := createDocument(t, current, project, "Documents two builds remove and make", "The spec.\n")
	// The rows this build cures, read once the previous build has stopped.
	var forTheTick []string
	// leaveForTheTick ends the first and fourth orders at the row at ref: beside a build that
	// writes document nodes, its node is live and the census names nothing of it; beside one that
	// writes none, the row is left for this build to cure.
	leaveForTheTick := func(t *testing.T, ref string) {
		t.Helper()
		if writesNodes {
			requireDocumentNodeState(t, builds, ref, graphstore.StateLive)
			requireIncompleteLinesNaming(t, runTwoBuildStoreCensus(t, builds), ref)
			return
		}
		forTheTick = append(forTheTick, ref)
	}

	t.Run("this build makes a document and the previous build removes it", func(t *testing.T) {
		document := makeUntouchedDocument(t, current, issue, "Removed by the previous build.md")
		requireDocumentNodeState(t, builds, document.ref, graphstore.StateLive)
		removeUntouchedDocument(t, previous, document)
		if writesNodes {
			requireDocumentNodeState(t, builds, document.ref, graphstore.StateGone)
			census := runTwoBuildStoreCensus(t, builds)
			requireProjectLine(t, census, "artifact", "orphaned=0")
			requireIncompleteLinesNaming(t, census, document.ref)
			return
		}
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
		removed := makeUntouchedDocument(t, current, issue, "Made again.md")
		removeUntouchedDocument(t, current, removed)
		requireDocumentNodeState(t, builds, removed.ref, graphstore.StateGone)
		again := makeUntouchedDocument(t, previous, issue, "Made again.md")
		if again.ref != removed.ref {
			t.Fatalf("the %s made the document at %s, want the removed one's address %s, so this row proves nothing",
				builds.previousName(), again.ref, removed.ref)
		}
		leaveForTheTick(t, again.ref)
	})

	t.Run("the previous build removes this build's document and this build makes it again", func(t *testing.T) {
		document := makeUntouchedDocument(t, current, issue, "Made again here.md")
		removeUntouchedDocument(t, previous, document)
		again := makeUntouchedDocument(t, current, issue, "Made again here.md")
		if again.ref != document.ref {
			t.Fatalf("this build made the document at %s, want the removed one's address %s, so this row proves nothing", again.ref, document.ref)
		}
		requireDocumentNodeState(t, builds, again.ref, graphstore.StateLive)
		requireIncompleteLinesNaming(t, runTwoBuildStoreCensus(t, builds), again.ref)
	})

	t.Run("the previous build removes this build's document and makes it again", func(t *testing.T) {
		document := makeUntouchedDocument(t, current, issue, "Made again there.md")
		removeUntouchedDocument(t, previous, document)
		again := makeUntouchedDocument(t, previous, issue, "Made again there.md")
		if again.ref != document.ref {
			t.Fatalf("the %s made the document at %s, want the removed one's address %s, so this row proves nothing",
				builds.previousName(), again.ref, document.ref)
		}
		leaveForTheTick(t, again.ref)
	})

	t.Run("the previous build stops and this build cures what it left", func(t *testing.T) {
		// Beside a build that writes no node, the first and fourth orders each left a row for this
		// build to cure; beside one that writes them, none.
		want := 2
		if writesNodes {
			want = 0
		}
		if len(forTheTick) != want {
			t.Fatalf("the orders left %d row(s) for this build to cure, want %d", len(forTheTick), want)
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
		if writesNodes {
			if lines := incompleteLines(runTwoBuildStoreCensus(t, builds)); len(lines) > 0 {
				t.Errorf("once the %s, which writes document nodes, stopped, the census names %q, want nothing: no order beside it leaves this build a row to cure",
					builds.previousName(), lines)
			}
			return
		}
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

// previousWritesDocumentNodes answers whether the previous build writes document nodes. Through
// that build it makes a project and an issue of its own, makes an untouched document of the issue
// and reads the node at its address, then removes the document and reads the node again: a build
// from before document nodes leaves no node either time, and one that writes them leaves the node
// live, then gone. Any other pair is neither writer, and fails the test. It runs while the
// previous build's process is the only one live, before this build starts, so no pass of this
// build (its block-index tick, its decisions step's phase 1) can have written or stated the
// probe's node.
//
// The probe's document is removed by the time this answers, and its issue stays, since no route
// removes an issue. Beside a build that writes document nodes, the issue and its spec hold their
// nodes from their create, as the row's own issue does. Beside one that writes none, they hold
// none, and this tree's backfill-graph-store writes them before this build starts: otherwise this
// build's boot, whose decisions step runs phase 1 before it serves, would write the spec's node
// and log it on the step's kind=artifact line, whose rows= the last subtest counts as a pass's
// cure of the rows the orders left. Either way the census names nothing of the probe.
func previousWritesDocumentNodes(t *testing.T, builds *twoBuilds) bool {
	t.Helper()
	previous := builds.previous()
	project := uniqueProjectKey(t, "NWR")
	previous.createProject(t, project, "Which writer of document nodes the previous build is")
	issue, _ := createDocument(t, previous, project, "A document the previous build makes and removes alone", "The spec.\n")
	probe := makeUntouchedDocument(t, previous, issue, "Which writer the previous build is.md")
	made := documentNodeState(t, builds, probe.ref)
	removeUntouchedDocument(t, previous, probe)
	removed := documentNodeState(t, builds, probe.ref)
	switch {
	case made == noDocumentNode && removed == noDocumentNode:
		runCurrentCommand(t, builds, []int{0}, "backfill-graph-store")
		return false
	case made == graphstore.StateLive && removed == graphstore.StateGone:
		return true
	}
	t.Fatalf("the %s left the node of a document it made %s, and %s once it removed the document; want no node both times, from a build before document nodes, or live then gone, from one that writes them",
		builds.previousName(), made, removed)
	return false
}

// untouchedDocument is a document made by typing a title and never written in, the one kind the
// removal route removes: its id and the address its node is written under.
type untouchedDocument struct{ id, ref string }

// makeUntouchedDocument makes one of the issue's documents through p by typing its title.
func makeUntouchedDocument(t *testing.T, p *dispatchProcess, issue, name string) untouchedDocument {
	t.Helper()
	made := p.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", jsonBody(t, map[string]string{"name": name}), http.StatusCreated)
	var answer struct {
		Artifact struct {
			ID     string `json:"id"`
			RefKey string `json:"ref_key"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(made, &answer); err != nil || answer.Artifact.ID == "" || answer.Artifact.RefKey == "" {
		t.Fatalf("make %q on %s: %v: %s", name, issue, err, made)
	}
	return untouchedDocument{id: answer.Artifact.ID, ref: answer.Artifact.RefKey}
}

// removeUntouchedDocument removes the document through p.
func removeUntouchedDocument(t *testing.T, p *dispatchProcess, document untouchedDocument) {
	t.Helper()
	p.call(t, http.MethodDelete, "/api/v1/artifacts/"+document.id, "", http.StatusNoContent)
}

// noDocumentNode is what documentNodeState reads for an address the store holds no node at.
const noDocumentNode = "(no node)"

// documentNodeState is the state the store holds the document node at ref in, noDocumentNode
// where it holds none.
func documentNodeState(t *testing.T, builds *twoBuilds, ref string) string {
	t.Helper()
	var held string
	if err := builds.database.Pool.QueryRow(context.Background(), `
		select coalesce((select state from graph_nodes where kind = $1 and id = $2), $3)
	`, graphstore.KindArtifact, ref, noDocumentNode).Scan(&held); err != nil {
		t.Fatalf("read the node of %s: %v", ref, err)
	}
	return held
}

// requireDocumentNodeState fails unless the store holds the document node at ref in state, or
// holds none when state is noDocumentNode.
func requireDocumentNodeState(t *testing.T, builds *twoBuilds, ref, state string) {
	t.Helper()
	if held := documentNodeState(t, builds, ref); held != state {
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
		held := documentNodeState(t, builds, ref)
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
