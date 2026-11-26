//go:build memory

package main

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// The live-heap reading of a cold edit, which TestTheHeaviestDocumentsWithAFullMarginStayWithinTheMemoryBound
// bounds a full margin's cost by: the most heap the server's collector found live while the edit
// ran, above what a collection found live just before it.
//
// That test compares two cold edits of one document, with a full margin and without, and a full
// margin adds a few MiB to what the edit holds, so the comparison needs a reading that moves by
// less than that from run to run. Resident memory above idle (peakAboveIdle) does not: read above
// an idle taken as /healthz first answered, with the boot's dead pg_query heap in it or not
// (memoryHarness.start), the two edits differed by -257 to +256 MiB on a 288-CPU host, against the
// test's 128 (INFRA-3904: rows read 132 and 150); read above an idle taken after a collection that
// gives the free heap back to the system, they still differed by -21 to +24 MiB, from where the
// collector's GOGC=25 margin fell and from pages the heap held free.
//
// Read by the collector instead, what counts is the objects the server can still reach, and not
// pages that are resident but free. Each measured server runs every collection with the world
// stopped (GODEBUG gcstoptheworld=1), so a collection finds live exactly what is reachable when it
// runs, with nothing allocated during a concurrent mark counted live as well; it runs one each time
// the heap grows 5% past what the last found live (GOGC=5); and it writes each collection's live
// heap to its standard error (GODEBUG gctrace=1), which the harness captures with the rest of its
// output. The heap holds every live object, and a collection runs before the heap passes what the
// last one found live by more than about 5%, so when the edit held the most it ever held live, L,
// the last collection had found at least about L/1.05; and none finds more than is reachable. Each
// reading is therefore at most about 5% under the most the edit held, and never over it. The idle
// the reading stands above is a collection the test asks for (collectOnSignal) once /healthz has
// answered, so the boot's dead heap is gone from it too, and a second one closes the window, so a
// window in which the heap never grew enough for a collection of its own still holds one, and what
// the edit left live counts.
//
// The two collections the test asks for report themselves (collectOnSignal), because the runtime
// wakes runtime.GC's caller (here debug.FreeOSMemory) before it writes that collection's trace
// line: the line can come after whatever the caller writes next, and did in 5 of about 80 windows.
// Every collection between the two is read from the trace, where each has written its line before
// the next one begins (a collection holds the runtime's gcsema from its start until after its
// line, and the next one takes gcsema to start), so all of them are there by the time the second
// reports itself.

// liveHeapServerEnv is the environment liveHeapAboveIdle reads a server under: collections with
// the world stopped, one each time the heap grows 5% past what the last found live, and each
// one's live heap on standard error. memoryHarness.start puts it after its own GOGC=25, so it
// wins, and every server it starts collects on SIGUSR1 (collectOnSignal).
var liveHeapServerEnv = []string{
	"GOGC=5",
	"GODEBUG=gctrace=1,gcstoptheworld=1",
}

// gctraceLine is one collection as GODEBUG gctrace=1 writes it, documented in the runtime
// package: its number, and the heap when it began, when it ended and what it found live, in
// mebibytes (the runtime writes "MB" for 1<<20 bytes).
var gctraceLine = regexp.MustCompile(`gc (\d+) @[^\n]*? (\d+)->(\d+)->(\d+) MB, \d+ MB goal`)

// logLine is one line the server's slog handler writes (main's slog.NewTextHandler): written
// whole, it can land between the runtime's writes of a collection's line, which collections drops
// it from before it reads that line.
var logLine = regexp.MustCompile(`time=\d{4}-\d\d-\d\dT[^\n]*\n`)

// collections are the collections output's trace lines report, in order.
func collections(t *testing.T, output string) []collection {
	t.Helper()
	var found []collection
	for _, match := range gctraceLine.FindAllStringSubmatch(logLine.ReplaceAllString(output, ""), -1) {
		number, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("read collection %q: %v", match[1], err)
		}
		live, err := strconv.ParseInt(match[4], 10, 64)
		if err != nil {
			t.Fatalf("read collection %d's live heap %q: %v", number, match[4], err)
		}
		found = append(found, collection{number: number, live: live << 20})
	}
	return found
}

// liveHeapAboveIdle runs during on a server started under liveHeapServerEnv and reports the most
// heap the server's collections found live while it ran, above what a collection found live just
// before it. A collection the test asks for after during returns closes the window. Every
// collection between the two must be read from the trace: one the trace lost would leave the most
// live heap unknown, which fails the test rather than reading lower.
func (p *memoryServer) liveHeapAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	release := p.holdBlockIndexLease(t)
	defer release()
	idle, opened := p.collect(t)
	during()
	last, _ := p.collect(t)
	window := p.outputFrom(opened)
	requireNoIndexPass(t, window)
	held := max(idle.live, last.live)
	next := idle.number + 1
	for _, cycle := range collections(t, window) {
		// The first collection's own line can follow its report, and the last's can still be on
		// its way; their reports already carry both.
		if cycle.number <= idle.number || cycle.number >= last.number {
			continue
		}
		if cycle.number != next {
			t.Fatalf("the server's trace reports collection %d after %d, so the most heap the window held live is unknown:\n%s", cycle.number, next-1, window)
		}
		held = max(held, cycle.live)
		next++
	}
	if next != last.number {
		t.Fatalf("the server's trace reports collections %d to %d of the window's %d to %d, so the most heap it held live is unknown: is GODEBUG gctrace=1 set, and is its line still %q?\n%s",
			idle.number+1, next-1, idle.number+1, last.number-1, gctraceLine, window)
	}
	return held - idle.live
}

// coldEditLiveHeap is coldEdit read by the server's live heap (liveHeapAboveIdle) rather than its
// resident memory: it sends edit to a document on a server that has not loaded it, fails the test
// unless it is taken, and logs and returns the most heap the server held live above idle.
func (h *memoryHarness) coldEditLiveHeap(t *testing.T, name, artifactID string, edit map[string]any) int64 {
	t.Helper()
	env := h.env
	h.env = append(slices.Clone(env), liveHeapServerEnv...)
	server := h.start(t)
	h.env = env
	var answer response
	held := server.liveHeapAboveIdle(t, func() { answer = server.edit(t, artifactID, edit) })
	server.stop(t)
	t.Logf("%s: a cold one-word edit answered %d, %d MiB of live heap above idle", name, answer.status, held>>20)
	if answer.status != http.StatusOK {
		t.Errorf("%s: a cold one-word edit answered %d %.300s, want 200", name, answer.status, answer.body)
	}
	return held
}
