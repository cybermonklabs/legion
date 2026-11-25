//go:build memory

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The real-process harness the memory tests (memory_test.go) drive Dispatch through: a test
// binary that runs main as a Dispatch server on the shared test database (memoryTestServeEnv), the
// server's resident memory read from /proc, and requests over HTTP and the document websocket.
// Both files build only under the `memory` tag, which the dispatch-memory CI job sets: their rows
// run past go test's default deadline, so the unit run of every Go package leaves them out.
//
// The tests run at the same time (t.Parallel), each on a database of its own (newMemoryHarness).
// What a test measures is one server process's own memory - its resident peak above its own idle
// (peakAboveIdle) or the heap its own collector found live (liveHeapAboveIdle) - which other
// processes do not move while the machine has memory to spare: the most any test reads is about
// 400 MiB above a server's idle (TestColdReadCacheEvictionPlateaus), four servers at once on a
// 16 GB runner. One after another, the suite kept 0.92 of 4 CPUs busy on average (2,461 CPU
// seconds over 2,675 s, the whole suite pinned to 4 CPUs of a dev box). The two tests that
// measure this test process's own allocations (TestBlockPathsOfTheHeaviestTablesStayWithinTheMemoryBound
// and TestTableRowsOfSpannedTablesShareOneSpanBudget) do not call t.Parallel, so go test runs
// them before any parallel test starts. At most testlimits.MaxProcessesAtOnce servers run at once
// (dispatchSlots), the count the lane's Postgres is sized for, and a test that runs two at once
// reserves both before its first start (dispatchSlots.reserve).

const memoryTestToken = "memory-test-token"

// memoryHarness is the database and the issue the memory tests' servers share.
type memoryHarness struct {
	t        *testing.T
	database *store.Store
	issue    string
	// env is added to every server's environment.
	env []string
}

func newMemoryHarness(t *testing.T) *memoryHarness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the memory tests read a server's resident memory from /proc")
	}
	harness := &memoryHarness{t: t, database: storetest.Open(t)}
	server := harness.start(t)
	for _, request := range []struct{ path, body string }{
		{"/api/v1/projects", `{"key":"MEM","name":"Memory"}`},
		{"/api/v1/issues", `{"project":"MEM","title":"Memory bound"}`},
	} {
		response := server.send(t, http.MethodPost, request.path, "application/json", strings.NewReader(request.body), http.Header{"X-Dispatch-User": {"alice"}})
		if response.status != http.StatusCreated {
			t.Fatalf("POST %s: status %d body %s", request.path, response.status, response.body)
		}
		var created struct{ Key string }
		if err := json.Unmarshal(response.body, &created); err != nil {
			t.Fatalf("decode %s: %v", request.path, err)
		}
		harness.issue = created.Key
	}
	server.stop(t)
	return harness
}

// waitForSettlement waits until the document a stored upload wrote has settled.
func (h *memoryHarness) waitForSettlement(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status == http.StatusCreated {
		h.waitForSettled(t, upload.artifactID)
	}
}

// waitForSettled waits, through the shared wait, until a document has settled every write it has
// had: the settlement that has read every update deletes the document's doc_settlements_pending
// row as it commits. Its reading is when that row was marked, while it is there.
func (h *memoryHarness) waitForSettled(t *testing.T, artifactID string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("document %s to settle every write it has had (its doc_settlements_pending row gone)", artifactID), func() (string, bool) {
		var markedAt *time.Time
		if err := h.database.Pool.QueryRow(testwait.Context(t),
			`select max(marked_at) from doc_settlements_pending where artifact_id = $1`, artifactID,
		).Scan(&markedAt); err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if markedAt != nil {
			return "its settlement pending since " + markedAt.Format(time.RFC3339Nano), false
		}
		return "no settlement pending", true
	})
}

// memoryTestCollectEnv makes a memory test's server collect its heap each time it is sent SIGUSR1
// and then report that collection on its standard error (collectOnSignal).
const memoryTestCollectEnv = "DISPATCH_MEMORY_TEST_COLLECT_ON_SIGNAL"

// collectedLine is the line a server running with memoryTestCollectEnv writes once the collection
// a SIGUSR1 asked for has run: its number and the heap it found live, in bytes.
var collectedLine = regexp.MustCompile(`memory test: collection (\d+) found (\d+) bytes live\n`)

// memoryServer is a Dispatch server: this test binary running main.
type memoryServer struct {
	command *exec.Cmd
	base    string
	// output and exited are the process's (cmdtest.Process): its combined output, and closed
	// once it has exited and released its connections.
	output *cmdtest.LockedBuffer
	exited <-chan struct{}
	idle   int64
	// database is the harness's own handle on the database this server serves, which a measured
	// window takes the block-index lease on (holdBlockIndexLease).
	database *store.Store
}

func init() {
	if os.Getenv(memoryTestServeEnv) == "1" && os.Getenv(memoryTestCollectEnv) == "1" {
		collectOnSignal()
	}
}

// collectOnSignal collects the heap and gives its free memory back to the system
// (debug.FreeOSMemory) each time the process is sent SIGUSR1, and then writes the number of the
// last collection the runtime completed and the heap it found live (collectedLine), read again
// until no collection completed between two reads of the number, so the heap is that collection's.
// It is installed before main runs, so the server never meets a SIGUSR1 it would die of.
func collectOnSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/gc/heap/live:bytes"}}
		for range signals {
			debug.FreeOSMemory()
			for {
				metrics.Read(samples)
				number, live := samples[0].Value.Uint64(), samples[1].Value.Uint64()
				metrics.Read(samples[:1])
				if samples[0].Value.Uint64() == number {
					fmt.Fprintf(os.Stderr, "memory test: collection %d found %d bytes live\n", number, live)
					break
				}
			}
		}
	}()
}

// start starts a Dispatch server on a loopback port the kernel chose and the test holds from that
// moment, which the server inherits as DISPATCH_LISTEN_FD and serves on, so no other program can
// take the port before it serves. It takes the server's slot before the drain, so what the drain
// leaves owed is what the server meets.
func (h *memoryHarness) start(t *testing.T) *memoryServer {
	t.Helper()
	releaseSlot := dispatchSlots.take(t)
	held := cmdtest.HoldPort(t)
	port := strconv.Itoa(held.Port)
	h.drainBeforeStart(t, port)
	command := exec.Command(os.Args[0])
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		memoryTestServeEnv + "=1",
		"DATABASE_URL=" + h.database.Pool.Config().ConnString(),
		"DISPATCH_AGENT_TOKEN=" + memoryTestToken,
		"DISPATCH_SIGNING_KEY=memory-test-signing-key",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + port,
		"DISPATCH_WEB_DIST=" + t.TempDir(),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + port,
		// Every measured server collects at GOGC=25, so a peak above idle reads the memory a
		// request holds plus the collector's margin at that setting, not the resident memory
		// production shows at Go's default GOGC=100, where the collector lets the heap grow to
		// twice the live set before it collects and an edit can raise resident memory by up to
		// about twice what it holds. A test that needs another value sets GOGC in
		// memoryHarness.env, which is appended after this and wins.
		"GOGC=25",
		// Every server collects and gives its free memory back to the system when it is sent
		// SIGUSR1 (collectOnSignal), which start asks for before it reads the server's idle.
		memoryTestCollectEnv + "=1",
	}
	command.Env = append(command.Env, h.env...)
	held.Pass(command, "DISPATCH_LISTEN_FD")
	// This server is a Dispatch process like any other a test starts, so the connection guard
	// counts it, and releases it before exited closes, so a test that sees the exit sees the
	// count fall (requireConnectionCeiling, cmdtest.StartThen); its slot goes back with it.
	// StartThen closes the test's copy of the held socket once the server has started, so the
	// server releases the port when it exits.
	releaseConnections := requireConnectionCeiling(t, h.database.Pool.Config().ConnString())
	process := cmdtest.StartThen(t, "the Dispatch server", command, func() {
		releaseConnections()
		releaseSlot()
	})
	server := &memoryServer{command: command, base: "http://127.0.0.1:" + port, output: process.Output, exited: process.Exited, database: h.database}
	// Registered after StartThen's own cleanup, so it runs first: the server is stopped with
	// SIGTERM as a deploy stops it, and StartThen's kill finds it gone.
	t.Cleanup(func() { server.stop(t) })
	// The kernel queues a request on the held socket until the server serves it, so each one
	// carries the shared wait's context, which ends where the wait gives up.
	ctx := testwait.Context(t)
	testwait.Until(t, "the Dispatch server to answer /healthz with 200", func() (string, bool) {
		select {
		case <-server.exited:
			t.Fatalf("the Dispatch server exited while starting:\n%s", server.outputTail())
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.base+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return fmt.Sprintf("GET /healthz: %v; the server's output ends:\n%s", err, server.outputTail()), false
		}
		_ = response.Body.Close()
		return fmt.Sprintf("GET /healthz: %d; the server's output ends:\n%s", response.StatusCode, server.outputTail()), response.StatusCode == http.StatusOK
	})
	// The idle is read after a collection that gives the heap's free memory back to the system.
	// Whether a collection had freed what a boot left dead before /healthz answered was the luck of
	// where the boot's collections fell: read at once, the idle was about 90 MiB on one server and
	// about 330 MiB on the next, and a request then reused the dead pages, so its peak above idle
	// read up to about 240 MiB low, and below zero (INFRA-3904). Most of that dead heap was the
	// migrations' libpg_query parse, which a start no longer runs (pgmigrate.LoadBuilt).
	server.collect(t)
	server.idle = server.resident(t, "VmRSS")
	return server
}

// collection is one collection the server reports: its number and the heap it found live.
type collection struct {
	number int
	live   int64
}

// owedBeforeStart is what the test's database owes when a server is about to start: the documents
// owed a block index, the items owed an index with the block grammar, the comments, messages and
// asks owed a block index of their own blocks, the rows of the re-index queue refs_grammar_owed -
// a comment, an ask or an issue message owed an index with this build's whole grammar, a document
// owed that index for the documents batch to write, or one document's whole scope - and the
// documents owed a settlement.
type owedBeforeStart struct {
	documents   int
	items       int
	itemBlocks  int
	texts       int
	settlements int
}

// readOwed reads in one statement everything a starting server would find owed.
func (h *memoryHarness) readOwed(t *testing.T) owedBeforeStart {
	t.Helper()
	var owed owedBeforeStart
	if err := h.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_block_versions where version is null),
		       (select count(*) from block_grammar_owed),
		       (select count(*) from comment_block_state where owed_at is not null)
		         + (select count(*) from message_block_state where owed_at is not null)
		         + (select count(*) from ask_block_state where owed_at is not null),
		       (select count(*) from refs_grammar_owed),
		       (select count(*) from doc_settlements_pending)
	`).Scan(&owed.documents, &owed.items, &owed.itemBlocks, &owed.texts, &owed.settlements); err != nil {
		t.Fatalf("read what a starting server would find owed: %v", err)
	}
	return owed
}

// drainBeforeStart leaves the database owing nothing the server about to start would do inside a
// measured window. A fresh server arms the settlement of every row older than a minute at start
// and every minute after (docs.SettlementResumeInterval and settlementResumeAge) and indexes the
// blocks of every document owed one, both of which read whole documents: left owed, either runs
// inside the next window, and the peak measured there is that work's memory, not the request's.
//
// It does that work itself rather than waiting for it. Between one server's stop and the next
// server's start no process in this test is settling anything, so a settlement a write left on a
// server that has since stopped would stand for ever: a guard that waited for such a row to
// settle would wait out its whole timeout and fail, which is how every memory test of this
// package went red on CI (INFRA-985, run 42013424869).
func (h *memoryHarness) drainBeforeStart(t *testing.T, port string) {
	t.Helper()
	owed := h.readOwed(t)
	if total := owed.settlements + owed.documents + owed.items + owed.itemBlocks + owed.texts; total == 0 {
		t.Logf("block index before start (%s): indexed %d documents, %d items, %d item blocks, %d texts; owed %d",
			t.Name(), 0, 0, 0, 0, total)
		return
	}
	// One service for both pieces of work, shut down before the server starts, so neither its
	// compaction queue nor a lease of its own outlives this step. ServerURL is the port the test
	// already holds, the one refs.ReindexOwed hands text.ExtractAt, so the index this writes
	// is the index the server would have written.
	service := docs.New(docs.Deps{Store: h.database, Events: events.NewBroker(), ServerURL: "http://127.0.0.1:" + port})
	defer docstest.Shutdown(t, context.Background(), service)
	if owed.settlements > 0 {
		h.settleOwed(t, service)
	}
	var documents, items, itemBlocks, texts int
	if owed.documents+owed.items+owed.itemBlocks+owed.texts > 0 {
		documents, items, itemBlocks, texts = h.writeOwedBlockIndex(t, service)
	}
	// What the line reports is a reading taken after the work, not an assumption that the work
	// cleared everything: a row still owed here is one nothing in this process can clear, and it
	// would be measured inside the window as this guard exists to prevent.
	left := h.readOwed(t)
	remaining := left.settlements + left.documents + left.items + left.itemBlocks + left.texts
	if remaining != 0 {
		t.Fatalf("after the drain (%s) the database still owes %d documents a block index, %d items, %d item blocks, %d texts and %d settlements, which nothing in this process can clear",
			t.Name(), left.documents, left.items, left.itemBlocks, left.texts, left.settlements)
	}
	t.Logf("block index before start (%s): indexed %d documents, %d items, %d item blocks, %d texts; owed %d",
		t.Name(), documents, items, itemBlocks, texts, remaining)
}

// settleOwed settles, on this goroutine, every document owed a settlement, by the issue each
// belongs to: SettleIssueDocuments is the only exported synchronous settle, and it returns when
// the settlements it ran have committed. A row still owed after it is one nothing here can clear,
// and the failure names it.
func (h *memoryHarness) settleOwed(t *testing.T, service *docs.Service) {
	t.Helper()
	ctx := context.Background()
	rows, err := h.database.Pool.Query(ctx, `
		select distinct coalesce(artifacts.issue_key, '')
		from doc_settlements_pending pending join artifacts on artifacts.id = pending.artifact_id
	`)
	if err != nil {
		t.Fatalf("read the issues of the documents owed a settlement: %v", err)
	}
	var issues []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			t.Fatalf("read the issue of a document owed a settlement: %v", err)
		}
		if key != "" {
			issues = append(issues, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issues of the documents owed a settlement: %v", err)
	}
	for _, issue := range issues {
		if err := service.SettleIssueDocuments(ctx, issue); err != nil {
			t.Fatalf("settle the documents of issue %s before a measured server starts: %v", issue, err)
		}
	}
	if pending := h.pendingSettlements(t); len(pending) > 0 {
		t.Fatalf("a settlement was still owed after this harness settled every issue that owed one (%s); a document that belongs to no issue cannot be settled from here",
			strings.Join(pending, "; "))
	}
}

// pendingSettlements names each document owed a settlement, with the marked_at of its row.
func (h *memoryHarness) pendingSettlements(t *testing.T) []string {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(),
		`select artifact_id::text, marked_at from doc_settlements_pending order by marked_at`)
	if err != nil {
		t.Fatalf("read the documents owed a settlement: %v", err)
	}
	defer rows.Close()
	var pending []string
	for rows.Next() {
		var artifactID string
		var markedAt time.Time
		if err := rows.Scan(&artifactID, &markedAt); err != nil {
			t.Fatalf("read a document owed a settlement: %v", err)
		}
		pending = append(pending, fmt.Sprintf("%s marked at %s", artifactID, markedAt.Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents owed a settlement: %v", err)
	}
	return pending
}

// writeOwedBlockIndex writes, through service, the block index the server would have written, and
// reports what it indexed.
func (h *memoryHarness) writeOwedBlockIndex(t *testing.T, service *docs.Service) (documents, items, itemBlocks, texts int) {
	t.Helper()
	ctx := testwait.Context(t)
	var cursors docs.BlockIndexCursors
	// liveAheadTicks counts the ticks in a row that indexed nothing and found a document live
	// ahead: a document whose stored text is ahead of its latest version is moved on by a
	// settlement, which settleOwed has already run for every issue that owed one, so two such
	// ticks say the drain cannot finish.
	liveAheadTicks := 0
	// Each ask of the shared wait is one tick of the reconcile; a document a tick could not index is
	// logged and left owed, and the wait's reading counts what is still owed.
	testwait.Until(t, "the block index a starting server would write to be written", func() (string, bool) {
		tick, err := service.ReconcileBlockIndex(ctx, cursors, nil)
		if err != nil {
			t.Fatalf("write the block index a starting server would write: %v", err)
		}
		cursors = tick.Cursor
		documents += tick.Documents
		items += tick.Items
		itemBlocks += tick.ItemBlocks
		texts += tick.Texts
		indexed := tick.Documents + tick.Items + tick.ItemBlocks + tick.Texts
		if tick.OwedDocuments+tick.OwedItems+tick.OwedItemBlocks+tick.OwedTexts == 0 {
			return "nothing owed", true
		}
		if indexed == 0 && tick.LiveAhead > 0 {
			liveAheadTicks++
			if liveAheadTicks > 1 {
				t.Fatalf("the block index is owed documents whose stored text is ahead of their latest version, which nothing in this test process settles: %s",
					strings.Join(h.owedDocumentNames(t), "; "))
			}
		} else {
			liveAheadTicks = 0
		}
		return fmt.Sprintf("still owed %d documents, %d items, %d item blocks and %d texts after a tick that indexed %d",
			tick.OwedDocuments, tick.OwedItems, tick.OwedItemBlocks, tick.OwedTexts, indexed), false
	})
	return documents, items, itemBlocks, texts
}

// owedDocumentNames names each document still owed a block index, with the marked_at of its
// pending settlement where it has one: a document live ahead owes one.
func (h *memoryHarness) owedDocumentNames(t *testing.T) []string {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(), `
		select owed.artifact_id::text, pending.marked_at
		from document_block_versions owed
		left join doc_settlements_pending pending on pending.artifact_id = owed.artifact_id
		where owed.version is null
		order by owed.artifact_id
	`)
	if err != nil {
		t.Fatalf("read the documents owed a block index: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var artifactID string
		var markedAt *time.Time
		if err := rows.Scan(&artifactID, &markedAt); err != nil {
			t.Fatalf("read a document owed a block index: %v", err)
		}
		if markedAt == nil {
			names = append(names, artifactID+" owing no settlement")
			continue
		}
		names = append(names, fmt.Sprintf("%s marked at %s", artifactID, markedAt.Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents owed a block index: %v", err)
	}
	return names
}

// stop stops the server with SIGTERM, as a deploy does, and waits through the shared wait for it to
// exit: what its graceful stop writes (a compaction, history_memory_test.go) is read after it, and
// StartThen's cleanup kill stays the last resort.
func (p *memoryServer) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.command.Process.Signal(syscall.SIGTERM)
	testwait.Receive(t, "the Dispatch server to exit on SIGTERM", p.exited)
}

// collect asks the server for a collection (collectOnSignal), waits until the server has reported
// it, and returns it with the length of the server's output up to the end of its report, where
// whatever the server writes after it begins.
func (p *memoryServer) collect(t *testing.T) (collection, int) {
	t.Helper()
	offset := p.output.Len()
	if err := p.command.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("ask the server for a collection: %v\n%s", err, p.outputTail())
	}
	var (
		reported collection
		end      int
	)
	testwait.Until(t, "the server to report the collection it was asked for (is "+memoryTestCollectEnv+"=1 set?)", func() (string, bool) {
		logged := p.outputFrom(offset)
		if at := collectedLine.FindStringSubmatchIndex(logged); at != nil {
			number, err := strconv.Atoi(logged[at[2]:at[3]])
			if err != nil {
				t.Fatalf("read the collection's number %q: %v", logged[at[2]:at[3]], err)
			}
			live, err := strconv.ParseInt(logged[at[4]:at[5]], 10, 64)
			if err != nil {
				t.Fatalf("read the collection's live heap %q: %v", logged[at[4]:at[5]], err)
			}
			reported, end = collection{number: number, live: live}, offset+at[1]
			return logged[at[0]:at[1]], true
		}
		select {
		case <-p.exited:
			t.Fatalf("the server exited before it reported the collection it was asked for:\n%s", p.outputTail())
		default:
		}
		if len(logged) > 4000 {
			logged = logged[len(logged)-4000:]
		}
		return "what the server wrote since the ask: " + logged, false
	})
	return reported, end
}

// resident is the server's resident memory in bytes, VmRSS now or VmHWM its peak.
func (p *memoryServer) resident(t *testing.T, field string) int64 {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.command.Process.Pid))
	if err != nil {
		t.Fatalf("read the server's memory (has it died?): %v\n%s", err, p.outputTail())
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, field+":"); ok {
			kibibytes, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				t.Fatalf("parse %s %q: %v", field, value, err)
			}
			return kibibytes << 10
		}
	}
	t.Fatalf("no %s in the server's status", field)
	return 0
}

// peakAboveIdle runs during and reports the most the server's resident memory rose above what it
// held idle, freshly started, while it ran.
func (p *memoryServer) peakAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	// Every measured window goes through here, so the lease is taken here: from before the window
	// begins to after the peak has been read, which is the whole of what the guard below watches.
	// The release is deferred, so a window whose own failure ends the test gives the lease back on
	// the way out and the rows after it measure their windows too.
	release := p.holdBlockIndexLease(t)
	defer release()
	// Writing 5 to clear_refs resets the peak (VmHWM) to the memory resident now.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", p.command.Process.Pid), []byte("5"), 0); err != nil {
		t.Fatalf("reset the server's peak memory: %v", err)
	}
	written := p.output.Len()
	during()
	requireNoBlockIndexPass(t, p.outputFrom(written))
	return p.resident(t, "VmHWM") - p.idle
}

// requireNoBlockIndexPass fails the test where window, what the server logged during a measured
// window, holds a block-index pass. A block-index pass reads whole documents of its own, so one
// inside the window would make what the window measured that pass's memory rather than the
// request's. start leaves nothing owed, and a pass that finds nothing owed logs nothing, so a
// clean window holds no such line. What a drain taken before the server started cannot cover is a
// window that fills an owed queue itself - any window that stores a document - since that row is
// owed while the measurement runs and the server's next tick would index it there; the pass is
// held off for the window's length by its own lease (holdBlockIndexLease) instead, and this guard
// stays as the proof that it was. The handler is the slog.NewTextHandler main installs with
// slog.SetDefault in cmd/dispatch/main.go, which writes the message mid-line as
// msg="dispatch: block index", so the substring is the match and not a line's prefix.
func requireNoBlockIndexPass(t *testing.T, window string) {
	t.Helper()
	for _, line := range strings.Split(window, "\n") {
		if strings.Contains(line, "dispatch: block index") {
			t.Fatalf("the server indexed blocks inside the measured window, so what it measured is that pass's memory: %s", line)
		}
	}
}

// blockIndexLeaseNamespace is the first key every loop lease is taken under - duty's
// leaseNamespace, which stays unexported - the four bytes of "LOOP". The second key is the
// lease's own name, which Postgres hashes with hashtext.
const blockIndexLeaseNamespace int32 = 0x4C4F4F50

// holdBlockIndexLease takes the block-index pass's own lease (docs.BlockIndexLease) and answers
// the call that gives it back. duty.Exclusive takes that lease before every pass and skips the
// pass where another session holds it, so no block-index pass runs while this test holds it:
// a window that owes the index of what it just wrote leaves that work to a tick after the lease
// is given back. It does not replace drainBeforeStart, which clears what was owed before the
// server started, work the lease only postpones; the two together leave the window clean.
//
// The lock belongs to the session that took it, so one connection of the harness's pool takes it,
// holds it for the window and gives it back (duty.FreeSessionLock, which destroys rather than
// returns a connection whose unlock failed, since what such a session holds is unknown). A pass
// reads one batch of each queue it serves and gives the lease back, so the wait for it goes
// through the shared wait: a pass that never gives it back fails the test naming the lease.
func (p *memoryServer) holdBlockIndexLease(t *testing.T) func() {
	t.Helper()
	ctx := testwait.Context(t)
	conn, err := p.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the block-index lease: %v", err)
	}
	held := false
	// Deferred, so a wait that gives up gives the connection back on its way out.
	defer func() {
		if !held {
			conn.Release()
		}
	}()
	// pg_try_advisory_lock answers false rather than queueing behind the holder, so a pass running
	// at this moment is waited out here: it gives the lease back when it ends.
	testwait.Until(t, fmt.Sprintf("the block-index pass to give the %q lease back, so this window is measured with the pass held off", docs.BlockIndexLease), func() (string, bool) {
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease).Scan(&held); err != nil {
			t.Fatalf("take the block-index lease: %v", err)
		}
		if held {
			return "the lease taken", true
		}
		return "the lease held by another session; the server's output ends:\n" + p.outputTail(), false
	})
	return func() {
		if err := duty.FreeSessionLock(clock.Real(), conn, `select pg_advisory_unlock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease); err != nil {
			t.Errorf("give the block-index lease back: %v", err)
		}
	}
}

type response struct {
	status int
	body   []byte
}

func (p *memoryServer) send(t *testing.T, method, path, contentType string, body io.Reader, header http.Header) response {
	t.Helper()
	answer, err := p.trySend(method, path, contentType, body, header)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// trySend is send for a goroutine other than the test's, which may not end the test.
func (p *memoryServer) trySend(method, path, contentType string, body io.Reader, header http.Header) (response, error) {
	request, err := http.NewRequest(method, p.base+path, body)
	if err != nil {
		return response{}, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	request.Header = header
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	answer, err := (&http.Client{Timeout: 20 * time.Minute}).Do(request)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w\n%s", method, path, err, p.outputTail())
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return response{status: answer.StatusCode, body: read}, nil
}

func (p *memoryServer) get(t *testing.T, path string) {
	t.Helper()
	if answer := p.send(t, http.MethodGet, path, "", nil, http.Header{"X-Dispatch-User": {"alice"}}); answer.status != http.StatusOK {
		t.Fatalf("GET %s: status %d body %.300s", path, answer.status, answer.body)
	}
}

type uploadResult struct {
	status     int
	body       []byte
	artifactID string
	elapsed    time.Duration
}

// upload sends markdown as a new document of issue through POST /api/v1/issues/{key}/artifacts,
// as a JSON body or a multipart file, with an agent's bearer token as production's checks do.
func (p *memoryServer) upload(t *testing.T, mode, issue, markdown string) uploadResult {
	t.Helper()
	result, err := p.tryUpload(mode, issue, markdown)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// tryUpload is upload for a goroutine other than the test's.
func (p *memoryServer) tryUpload(mode, issue, markdown string) (uploadResult, error) {
	return p.tryUploadNamed(mode, issue, fmt.Sprintf("memory-%d.md", time.Now().UnixNano()), markdown)
}

// tryUploadNamed is tryUpload of a document named name: a new version of the issue's document of
// that name where it has one. A version after the first replaces a document that already exists,
// so it names the token of the version it replaces, which uploadBase reads.
func (p *memoryServer) tryUploadNamed(mode, issue, name, markdown string) (uploadResult, error) {
	base, err := p.uploadBase(issue, name)
	if err != nil {
		return uploadResult{}, err
	}
	actor := map[string]string{"kind": "session", "id": "memory-test"}
	var body *bytes.Buffer
	var contentType string
	switch mode {
	case "json":
		body = &bytes.Buffer{}
		upload := map[string]any{"name": name, "content": markdown, "actor": actor}
		if base != nil {
			upload["precondition"] = base
		}
		if err := json.NewEncoder(body).Encode(upload); err != nil {
			return uploadResult{}, fmt.Errorf("encode the upload: %w", err)
		}
		contentType = "application/json"
	case "multipart":
		encodedActor, err := json.Marshal(actor)
		if err != nil {
			return uploadResult{}, fmt.Errorf("encode the actor: %w", err)
		}
		fields := map[string]string{"name": name, "actor": string(encodedActor)}
		if base != nil {
			encodedBase, err := json.Marshal(base)
			if err != nil {
				return uploadResult{}, fmt.Errorf("encode the token the upload replaces: %w", err)
			}
			fields["precondition"] = string(encodedBase)
		}
		body, contentType, err = apitest.MultipartUpload(fields, name, "text/markdown", []byte(markdown))
		if err != nil {
			return uploadResult{}, err
		}
	default:
		return uploadResult{}, fmt.Errorf("unknown upload mode %q", mode)
	}
	started := time.Now()
	answer, err := p.trySend(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", contentType, body, http.Header{"Authorization": {"Bearer " + memoryTestToken}})
	if err != nil {
		return uploadResult{}, err
	}
	result := uploadResult{status: answer.status, body: answer.body, elapsed: time.Since(started).Round(time.Millisecond)}
	if answer.status == http.StatusCreated {
		var created struct {
			Artifact struct{ ID string } `json:"artifact"`
		}
		if err := json.Unmarshal(answer.body, &created); err != nil {
			return uploadResult{}, fmt.Errorf("decode the upload's answer: %w", err)
		}
		result.artifactID = created.Artifact.ID
	}
	return result, nil
}

// uploadBase is the token an upload of the document the issue holds under name must name: the one
// that document carries now, read as every reader of it reads it. It answers nothing where the
// issue holds no document under that name, since such an upload is the document's first version
// and replaces nothing. Any other answer is this reader's error: it cannot know what the upload
// should name.
func (p *memoryServer) uploadBase(issue, name string) (map[string]string, error) {
	answer, err := p.trySend(http.MethodGet, "/api/v1/issues/"+issue+"/artifacts/"+name+"/text", "", nil,
		http.Header{"Authorization": {"Bearer " + memoryTestToken}})
	if err != nil {
		return nil, err
	}
	if answer.status == http.StatusNotFound {
		return nil, nil
	}
	if answer.status != http.StatusOK {
		return nil, fmt.Errorf("read the document %q of %s: status %d body %.300s", name, issue, answer.status, answer.body)
	}
	var document struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(answer.body, &document); err != nil {
		return nil, fmt.Errorf("decode the document %q of %s: %w", name, issue, err)
	}
	return map[string]string{"document": document.Token}, nil
}

// requireAnswered fails the test unless the upload was stored or refused with a 4xx naming its
// code and why.
func requireAnswered(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status == http.StatusCreated {
		return
	}
	var refusal struct{ Code, Error string }
	if upload.status < 400 || upload.status >= 500 || json.Unmarshal(upload.body, &refusal) != nil || refusal.Code == "" || refusal.Error == "" {
		t.Fatalf("upload answered %d %.300s, want 201 or a 4xx naming its code and reason", upload.status, upload.body)
	}
}

// loadOverWebsocket opens the document's room as a browser does - it connects and asks for the
// whole document - closes once the room has sent it, and returns once the server has ended the
// socket, the settlement attempt its departure makes included.
func (p *memoryServer) loadOverWebsocket(t *testing.T, artifactID string) {
	t.Helper()
	written := p.output.Len()
	address := "ws" + strings.TrimPrefix(p.base, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, answer, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		t.Fatalf("connect to the document's room: %v (%#v)", err, answer)
	}
	defer connection.Close()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, append(frame, ygsync.EncodeSyncStep1(crdt.New())...)); err != nil {
		t.Fatalf("ask the room for the document: %v", err)
	}
	// The room's answer is read under the shared wait's give-up, which is every wait's one bound.
	if deadline, bounded := testwait.GiveUpAt(t); bounded {
		if err := connection.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set the read deadline: %v", err)
		}
	}
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the room's answer: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
			continue
		}
		if kind, _, err := ygsync.ReadSyncMessage(decoder.RemainingBytes()); err == nil && kind == ygsync.MsgSyncStep2 {
			break
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}
	// The room's last peer leaving runs the settlement it owes on the socket's own handler: ygo's
	// ServeHTTP runs the departure (handleDisconnect, which calls OnLastPeer, the service's
	// settleLastPeer, and waits for it) before it returns, and the access log writes the request's
	// line once the handler has returned (accesslog.Handler). So the socket's line is the moment
	// the departure, and the settlement attempt it made, have ended, and the peak read after it
	// counts them. A settlement whose timer fired before the departure, or one that attempt arms
	// again, runs on its own and can end after the line; this load sends no update, and the
	// server it meets started owing nothing (drainBeforeStart), so it leaves neither.
	// A fixed 3 s stood here before, which counted no settlement that ran past it: across nine
	// cold loads of heavy documents the server spent at most 40 ms of CPU in those 3 s.
	p.waitForRequestLine(t, written, "route=/ws/doc/{room}", "path=/ws/doc/"+artifactID)
}

// waitForRequestLine waits until the server's output past offset holds the access log line of a
// request whose line carries every one of fields, and fails the test if the server exits first.
// The shared wait (testwait.Until) is its one bound, and a give-up prints what the server wrote
// past offset.
func (p *memoryServer) waitForRequestLine(t *testing.T, offset int, fields ...string) {
	t.Helper()
	want := append([]string{`msg="` + accesslog.Message + `"`}, fields...)
	testwait.Until(t, fmt.Sprintf("the server to log the request %q", fields), func() (string, bool) {
		logged := p.outputFrom(offset)
		for line := range strings.Lines(logged) {
			if !slices.ContainsFunc(want, func(field string) bool { return !strings.Contains(line, field) }) {
				return line, true
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("the server exited before it logged the request %q:\n%s", fields, p.outputTail())
		default:
		}
		if len(logged) > 4000 {
			logged = logged[len(logged)-4000:]
		}
		return "what the server wrote since the request began: " + logged, false
	})
}

// outputTail is the last 4,000 bytes the server has written, for a failure to show.
func (p *memoryServer) outputTail() string {
	output := p.output.String()
	if len(output) > 4000 {
		output = output[len(output)-4000:]
	}
	return output
}

// outputFrom is everything the server wrote past offset, a length of its output read earlier:
// what one measured window logged (peakAboveIdle), which outputTail's last 4,000 bytes do not
// answer for.
func (p *memoryServer) outputFrom(offset int) string {
	output := p.output.String()
	if offset >= len(output) {
		return ""
	}
	return output[offset:]
}

// coldPeaks reads and edits a document on servers that have not loaded it (coldReads, coldEdit)
// and fails the test where a read or the edit holds more than the bound above idle.
func (h *memoryHarness) coldPeaks(t *testing.T, name, artifactID string, edit map[string]any) {
	t.Helper()
	h.coldReads(t, name, artifactID)
	if peak := h.coldEdit(t, name, artifactID, edit); peak > requestMemoryBound {
		t.Errorf("%s: a cold one-word edit held %d MiB above idle, want at most %d MiB", name, peak>>20, requestMemoryBound>>20)
	}
}

// coldReads reads a document on servers that have not loaded it - its text and a websocket load of
// its room, each on a fresh server, then four reads of its text at once - and fails the test where
// one of the first two holds more than the bound above idle. Every peak is logged under name; the
// four reads at once are logged beside the production task's 4,096 MiB.
func (h *memoryHarness) coldReads(t *testing.T, name, artifactID string) {
	t.Helper()
	for _, cold := range []struct {
		name string
		run  func(server *memoryServer)
	}{
		{"text read", func(server *memoryServer) { server.get(t, "/api/v1/artifacts/"+artifactID+"/text") }},
		{"websocket load", func(server *memoryServer) { server.loadOverWebsocket(t, artifactID) }},
	} {
		server := h.start(t)
		peak := server.peakAboveIdle(t, func() { cold.run(server) })
		server.stop(t)
		t.Logf("%s: a cold %s, %d MiB above idle", name, cold.name, peak>>20)
		if peak > requestMemoryBound {
			t.Errorf("%s: a cold %s held %d MiB above idle, want at most %d MiB", name, cold.name, peak>>20, requestMemoryBound>>20)
		}
	}
	server := h.start(t)
	peak := server.peakAboveIdle(t, func() { server.coldTextReads(t, artifactID, 4) })
	server.stop(t)
	t.Logf("%s: four cold text reads at once, %d MiB above idle", name, peak>>20)
}

// coldTextReads reads a document's text readers times at once, failing unless every read answers
// 200. Its caller measures the fresh server's peak around it. Each read carries trySend's
// 20-minute client timeout, which can outlast the test binary's deadline, so the join on them goes
// through the shared wait (testwait.Returns), which is their bound.
func (p *memoryServer) coldTextReads(t *testing.T, artifactID string, readers int) {
	t.Helper()
	failures := make([]error, readers)
	var group sync.WaitGroup
	for index := range readers {
		group.Go(func() {
			answer, err := p.trySend(http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
			if err == nil && answer.status != http.StatusOK {
				err = fmt.Errorf("read %d: status %d body %.300s", index, answer.status, answer.body)
			}
			failures[index] = err
		})
	}
	testwait.Returns(t, fmt.Sprintf("the %d cold text reads at once to return", readers), group.Wait)
	if err := errors.Join(failures...); err != nil {
		t.Fatal(err)
	}
}

// coldEdit sends edit, a one-word edit, to a document on a server that has not loaded it, fails the
// test unless it is taken, and logs and returns its peak above the server's idle memory.
func (h *memoryHarness) coldEdit(t *testing.T, name, artifactID string, edit map[string]any) int64 {
	t.Helper()
	server := h.start(t)
	var answer response
	peak := server.peakAboveIdle(t, func() { answer = server.edit(t, artifactID, edit) })
	server.stop(t)
	t.Logf("%s: a cold one-word edit answered %d, %d MiB above idle", name, answer.status, peak>>20)
	if answer.status != http.StatusOK {
		t.Errorf("%s: a cold one-word edit answered %d %.300s, want 200", name, answer.status, answer.body)
	}
	return peak
}

// blockAsks are the ids of the asks settlement indexes from the blocks ask-0 to ask-<count-1> of
// the issue's spec, in that order, once it has indexed every one.
func (p *memoryServer) blockAsks(t *testing.T, issueKey string, count int) []string {
	t.Helper()
	var ids []string
	testwait.Until(t, fmt.Sprintf("settlement to index the %d asks of %s's spec", count, issueKey), func() (string, bool) {
		answer := p.send(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?state=all", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		if answer.status != http.StatusOK || json.Unmarshal(answer.body, &asks) != nil {
			t.Fatalf("list the issue's asks: status %d body %.300s", answer.status, answer.body)
		}
		byBlock := map[string]string{}
		for _, ask := range asks {
			if ask.BlockID != nil {
				byBlock[*ask.BlockID] = ask.ID
			}
		}
		ids = make([]string, 0, count)
		for index := range count {
			if id, found := byBlock[fmt.Sprintf("ask-%d", index)]; found {
				ids = append(ids, id)
			}
		}
		return fmt.Sprintf("settlement indexed %d of the spec's %d asks", len(ids), count), len(ids) == count
	})
	return ids
}

// createdIssue is an issue a memory test created, and its primary document.
type createdIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// createIssue creates an issue of the memory harness's project whose spec is spec, past the
// near-duplicate check its title would meet beside the others a test creates.
func (p *memoryServer) createIssue(t *testing.T, title, spec string) createdIssue {
	t.Helper()
	body, err := json.Marshal(map[string]any{"project": "MEM", "title": title, "spec": spec, "force": true})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := p.send(t, http.MethodPost, "/api/v1/issues", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
	var issue createdIssue
	if created.status != http.StatusCreated || json.Unmarshal(created.body, &issue) != nil || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: status %d body %.300s", title, created.status, created.body)
	}
	return issue
}

// edit sends one edit operation to a document as a person does.
func (p *memoryServer) edit(t *testing.T, artifactID string, op map[string]any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ops": []map[string]any{op}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
}

// text is a document's markdown, as GET .../text answers it.
func (p *memoryServer) text(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var text struct{ Markdown string }
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &text) != nil {
		t.Fatalf("read the document's text: status %d body %.300s", answer.status, answer.body)
	}
	return text.Markdown
}
