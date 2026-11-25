//go:build !race

package docs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/docwork"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/collabtest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The document service's own write transactions under storetest's xid rig (INFRA-636, plan test
// T1): settlement's version write, the edit-notice sweep and the task-progress reconciliation run
// no HTTP route, so these tests run them directly under a marked context (storetest.MarkXid) on
// documents the size of the load harness's large specs, and judge each transaction as the API's
// xid window tests judge a route's.
//
// Two background transactions of the plan's scope have no test here. The block-index backfill's
// owed arms (ReconcileBlockIndex) keep D6 at base on this fixture - their largest gap is about
// 3 ms and their gaps a fraction of their statements, even with hundreds of references - so a
// test of them could not be red now; the tick's window arm, which writes in transactions of its
// own, is judged below. RestoreDecidedWordings does no document work under its xid (only the
// reconciliation of its asks' own references on short questions, planAskReferences), and
// reaching it needs the reworded state an earlier release leaves.
//
// This file and anchor_blocks_xid_window_test.go build without the race detector (`//go:build
// !race`), as the API's and delivery's window tests do. Under it the rig judges no gap
// (storetest.XidJudgesGaps) and the shape is the same rule in either build, so a race run of
// these tests proved nothing the plain run does not: envoy-go's step "Test what the race build
// cannot run at all or at full size" runs them, judging shape and gaps both.

// xidService is a document service on a traced pool of a fresh database, settling after a day so
// only the settlement a test runs itself settles. Its document store and its identity check every
// call (checkedVersioned, collabtest.CheckedIdentity): none may run inside a transaction's xid
// window, from its first write to its end (store.InXidWindow); and the tracer fails the test for
// document work inside one (storetest.Traced).
func xidService(t *testing.T) (*Service, *store.Store, *storetest.XidTracer) {
	t.Helper()
	tracer := storetest.NewXidTracer()
	database := &store.Store{Pool: storetest.Traced(t, storetest.Open(t).Pool, tracer, nil)}
	service := New(Deps{
		Store:       database,
		Persistence: checkedVersioned(t, NewPgVersioned(database)),
		Events:      events.NewBroker(),
		Identity:    collabtest.CheckedIdentity(t, headerIdentity(database)),
		ServerURL:   "https://dispatch.example",
		Settle:      stagedAhead,
	})
	testcleanup.Add(t, func() {
		if err := docstest.Shutdown(t, context.Background(), service); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database, tracer
}

// The rig is not vacuous: a transaction whose statements run back to back after its first write
// keeps D6 even when it spent long in Go before that write, since a transaction holds no xid until
// a statement gives it one. A run that judges no gap holds the window's shape here and says so
// (AssertXidWindows).
func TestXidWindowRigPassesBackToBackStatementsAfterTheFirstWrite(t *testing.T) {
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransaction(t, database, 0)
	})
	storetest.AssertXidWindows(t, report, xidRigWindow)
	if len(report.Windows) != 1 {
		t.Fatalf("windows = %d, want the one transaction", len(report.Windows))
	}
	window := report.Windows[0]
	if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
		t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s", window.Statements[0].SQL, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// xidRigWindow is the rig transaction's window: its insert, its two reads and its COMMIT.
var xidRigWindow = storetest.XidWindowOf(4, "the rig transaction's insert, its two reads and its COMMIT")

// rigInjectedGap is the statement-free gap the deliberate-fault self-tests put inside the window
// (storetest.XidFaultGap), four times the bound for a window whose statements are as short as
// this transaction's. A sleep cannot run shorter than it asks for, so it is also the floor that
// bound must stay under: were XidMaxGap ever raised to it, these self-tests would report no
// violation and would say so.
const rigInjectedGap = storetest.XidFaultGap

// The rig's own work on a statement - classifying its text, reading whether it wrote - is done
// inside the statement, between its Start and its End, and none of it in the gaps around it
// (storetest's XidTracer.begin and XidTracer.end). The statement here carries a comment long
// enough that that work takes many times the bound of CPU: its text lowered to classify it, and
// the two expressions that read whether a statement wrote run over it. Put back in a gap, as
// before this rule, that CPU is the gap's and the window breaks D6; inside the statement, the
// window keeps it. The test first holds that the work did take that long, so a machine that ran it
// in no time cannot pass for one that kept it out of the gaps.
func TestXidWindowRigKeepsItsOwnWorkOutOfTheGaps(t *testing.T) {
	// Not parallel: it measures a window's statement-free gaps in CPU time on this machine, which
	// other tests' work on the same cores would stretch.
	storetest.RequireXidGapJudgment(t)
	_, database, tracer := xidService(t)
	long := "SELECT 1 /* " + strings.Repeat("Rig Work ", 1<<19) + "*/"
	report := tracer.Measure(t, database.Pool, func() {
		ctx := storetest.MarkXid(context.Background())
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
			t.Fatalf("take a transaction id: %v", err)
		}
		if _, err := tx.Exec(ctx, long); err != nil {
			t.Fatalf("run the long statement: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the transaction: %v", err)
		}
	})
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	if len(window.Statements) != 3 {
		t.Fatalf("the window holds %d statements, want the forced xid, the long statement and the commit:\n%s", len(window.Statements), window.Detail())
	}
	ran := window.Statements[1].Ran()
	if ran <= 4*storetest.XidMaxGap {
		t.Fatalf("the long statement's thread ran %s of CPU with the rig's work on it, not past %s: the rig's work was too quick to tell where it fell\n%s",
			storetest.XidMillis(ran), storetest.XidMillis(4*storetest.XidMaxGap), window.Detail())
	}
	if err := report.Err(); err != nil {
		t.Fatalf("the rig's own work on a long statement broke D6, so it ran in a gap:\n%v", err)
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// The rig sees a gap inside the window: the same transaction with Go work between its write and
// its commit breaks D6, and the report names the statement the gap follows. The gap is four
// times the bound and is spent in Go, so this test is a reading of the bound alone.
func TestXidWindowRigCatchesAGapAfterTheFirstWrite(t *testing.T) {
	storetest.RequireXidGapJudgment(t)
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransaction(t, database, rigInjectedGap)
	})
	err := report.Err()
	if err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a %s gap after the write: verdict %v, want a D6 violation after the insert", rigInjectedGap, err)
	}
	t.Logf("the rig's verdict on a %s gap after the write:\n%v", rigInjectedGap, err)
}

// The control on the bound: the fault this change removed - a document call inside the window -
// still breaks D6. The call is the parse every write route does on the text it writes
// (parseInput), on a document the size of the load harness's large specs, and the sleep it
// replaces is gone: nothing here waits. A build whose bound reached what that parse costs would
// let the real fault through, so this is the test that would go red first were XidMaxGap ever
// widened too far. The parse runs no statement of its own, so the window's shape is the same
// with it as without it, and of the window's verdicts only the bound sees it. The tracer's
// document-work check sees the parse too, and TestXidWindowCatchesDocumentWorkInsideTheWindow
// holds that check, so here the pool reports to a recorder: its report of the parse is no failure
// of this row, and any other report is. A document call that does run a statement inside the
// window is caught by the shape each window test asserts as well.
func TestXidWindowCatchesADocumentCallInsideTheWindow(t *testing.T) {
	storetest.RequireXidGapJudgment(t)
	tracer := storetest.NewXidTracer()
	caught := &collabtest.RecordedFailures{TB: t}
	database := &store.Store{Pool: storetest.Traced(caught, storetest.Open(t).Pool, tracer, nil)}
	spec, _ := storetest.XidSpec("DOC", 1)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func() {
			if _, err := parseInput(spec); err != nil {
				t.Errorf("parse the %d-byte spec: %v", len(spec), err)
			}
		})
	})
	for _, failure := range caught.Reported() {
		if !strings.HasPrefix(failure, "document work (") {
			t.Errorf("the rig reported a failure that is not the document work it was shown:\n%s", failure)
		}
	}
	err := report.Err()
	if err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
		!strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a parse of the %d-byte spec after the write: verdict %v, want a D6 violation after the insert",
			len(spec), err)
	}
	t.Logf("the rig's verdict on a parse of the %d-byte spec after the write:\n%v", len(spec), err)
}

// The control on the document-work check, in INFRA-636's own shape: document work inside the
// window, once for each entry point that announces its work (docwork.Enter), each in a transaction
// of its own, made after that transaction's first write and before its commit, and the sleep it
// replaces is gone: nothing here waits. No entry point runs a statement of its own or calls a
// collaborator, so the window's shape is the same with it as without it; what sees it in every run
// is the tracer's document-work check (docwork), which must name the work's kind and, as the frame
// that announced it, the entry point itself: one whose Enter were deleted would be seen, if at all,
// through another entry point of its kind beneath it, and its case fails by name. The parse is the
// one every write route does on the text it writes (parseInput), on a document the size of the load
// harness's large specs; the rest run on a small document read and written outside the window.
// Nothing else may fail. The pool reports to a recorder rather than to t, so the check firing is
// what this test asserts.
func TestXidWindowCatchesDocumentWorkInsideTheWindow(t *testing.T) {
	spec, _ := storetest.XidSpec("DOC", 1)
	tree, err := pmdoc.Parse("Intro with a quote to find.\n\nOutro.\n")
	if err != nil {
		t.Fatalf("parse the small document: %v", err)
	}
	markdown, _, layout, err := pmdoc.RenderWithLayout(tree)
	if err != nil {
		t.Fatalf("render the small document: %v", err)
	}
	stored := crdt.New()
	storedFragment := stored.GetXmlFragment(fragmentName)
	var stores error
	stored.Transact(func(txn *crdt.Transaction) { stores = pmdoc.Update(txn, storedFragment, tree) }, nil)
	if stores != nil {
		t.Fatalf("write the small document into a fragment: %v", stores)
	}
	for _, entry := range []struct {
		// kind is the work the entry point announces, and enteredBy the frame that calls
		// docwork.Enter for it: the entry point, as the stack names it.
		name, kind, enteredBy string
		work                  func() error
	}{
		{"a parse of the large spec", docwork.Parse, "pmdoc.readContext(", func() error {
			_, err := parseInput(spec)
			return err
		}},
		// ParseInline reads with a parser context of its own, not through readContext, so it
		// reaches the Enter of pmdoc.readInline alone, as an inline replacement's text
		// (inlineReplacement) and the renderer's read-backs (parseInlineWithDefinitions,
		// altReadsBack) do.
		{"an inline parse", docwork.Parse, "pmdoc.readInline(", func() error {
			_, err := pmdoc.ParseInline("**x**", nil)
			return err
		}},
		{"a render", docwork.Render, "pmdoc.renderDocument(", func() error {
			_, err := pmdoc.Render(tree)
			return err
		}},
		{"a layout's splice", docwork.Render, "pmdoc.(*Layout).Splice(", func() error {
			if _, _, ok := layout.Splice(markdown, []int{0}, func(index int) (*pmdoc.Node, error) { return tree.Children[index], nil }); !ok {
				return fmt.Errorf("the splice of block 0 was refused")
			}
			return nil
		}},
		{"a read of a fragment", docwork.Read, "pmdoc.readDocument(", func() error {
			_, err := pmdoc.Read(storedFragment)
			return err
		}},
		{"a write into a fragment", docwork.Encode, "pmdoc.updateShape(", func() error {
			written := crdt.New()
			fragment := written.GetXmlFragment(fragmentName)
			var err error
			written.Transact(func(txn *crdt.Transaction) { err = pmdoc.Update(txn, fragment, tree) }, nil)
			return err
		}},
		{"a quote's walk", docwork.AnchorWalk, "pmdoc.(*QuoteFinder).matches(", func() error {
			_, err := pmdoc.NewQuoteFinder(tree).Find("a quote to find", nil, nil)
			return err
		}},
		{"a walk for marks", docwork.AnchorWalk, "pmdoc.FindMarks(", func() error {
			pmdoc.FindMarks(tree, []pmdoc.MarkRef{{Type: string(MarkComment), ID: "c1"}})
			return nil
		}},
		{"a reference extraction", docwork.References, "text.ExtractAt(", func() error {
			text.ExtractAt("See dispatch://DOC-1 for it.", "https://dispatch.example")
			return nil
		}},
		{"the mentions of a text", docwork.References, "text.Mentions(", func() error {
			_, err := text.Mentions("See DOC-1 for it.", "https://dispatch.example", noReferences{})
			return err
		}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			tracer := storetest.NewXidTracer()
			caught := &collabtest.RecordedFailures{TB: t}
			database := &store.Store{Pool: storetest.Traced(caught, storetest.Open(t).Pool, tracer, nil)}
			report := tracer.Measure(t, database.Pool, func() {
				runRigTransactionDoing(t, database, func() {
					if err := entry.work(); err != nil {
						t.Errorf("%s: %v", entry.name, err)
					}
				})
			})
			failures := caught.Reported()
			prefix := "document work (" + entry.kind + ") ran inside a transaction's xid window"
			named := slices.IndexFunc(failures, func(failure string) bool { return strings.HasPrefix(failure, prefix) })
			switch {
			case named < 0:
				t.Fatalf("%s after the write: the document-work check reported %q, want its work, %s, named", entry.name, failures, entry.kind)
			case !strings.Contains(enteredBy(failures[named]), entry.enteredBy):
				t.Fatalf("%s after the write: the document-work check named its %s as entered by %q, want %s, the entry point itself:\n%s",
					entry.name, entry.kind, enteredBy(failures[named]), entry.enteredBy, failures[named])
			}
			for _, failure := range failures {
				if !strings.HasPrefix(failure, "document work (") {
					t.Errorf("the rig reported a failure that is not the document work it was shown:\n%s", failure)
				}
			}
			t.Logf("the document-work check on %s after the write:\n%s", entry.name, failures[named])
			// Where the build judges gaps, D6 sees the parse of the large spec too
			// (TestXidWindowCatchesADocumentCallInsideTheWindow); this row holds the document-work
			// check, so a D6 violation is no failure of it, and a rig problem is.
			if err := report.Err(); err != nil && (len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION")) {
				t.Fatalf("the rig could not judge the transaction: %v", err)
			}
		})
	}
}

// enteredBy is the frame below docwork.Enter in a document-work failure's stack: the function that
// announced the work. A stack names each frame's function on one line and its file on the next.
func enteredBy(failure string) string {
	lines := strings.Split(failure, "\n")
	for index, line := range lines {
		if strings.Contains(line, "/docwork.Enter(") && index+2 < len(lines) {
			return lines[index+2]
		}
	}
	return ""
}

// noReferences is a text.Resolver that resolves nothing: no issue exists, the text belongs to no
// document, and no short id names an item.
type noReferences struct{}

func (noReferences) Issues([]string) (map[string]bool, error)    { return nil, nil }
func (noReferences) Questions() (map[string]text.Ref, error)     { return nil, nil }
func (noReferences) Items([]text.Prefix) (map[int]string, error) { return nil, nil }

// The collaborator check's control through this package's rig, in every run: the tracer opens a
// transaction's xid window at its first write (store.OpenXidWindow), and a call to a collaborator
// the rig decorates (collabtest.CheckedIdentity, through collabtest.Check) is then named, where
// the same call before the write is not. A tracer that stopped opening the window, a decorator
// that stopped checking or a check that stopped reporting would leave every rig row passing, and
// fails this one.
func TestXidWindowCatchesACollaboratorCallInsideTheWindow(t *testing.T) {
	tracer := storetest.NewXidTracer()
	database := &store.Store{Pool: storetest.Traced(t, storetest.Open(t).Pool, tracer, nil)}
	caught := &collabtest.RecordedFailures{TB: t}
	people := collabtest.CheckedIdentity(caught, signedIn("alice@example.com"))
	ctx := storetest.MarkXid(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	login := func(when string) {
		t.Helper()
		if _, err := people.Login(httptest.NewRequest(http.MethodGet, "/ws/doc/rig", nil).WithContext(ctx)); err != nil {
			t.Fatalf("%s: resolve the socket's person: %v", when, err)
		}
	}
	login("before the write")
	if failures := caught.Reported(); len(failures) != 0 {
		t.Fatalf("a collaborator call before the transaction's first write was reported: %q", failures)
	}
	if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('RIG', 'Rig', 'alice')`); err != nil {
		t.Fatalf("write: %v", err)
	}
	login("after the write")
	failures := caught.Reported()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "identity.Identity.Login was called while its caller holds a connection of the pool") {
		t.Fatalf("a collaborator call after the transaction's first write was reported as %q, want one failure naming identity.Identity.Login", failures)
	}
}

// signedIn is an identity that resolves every request to one person, reading nothing.
type signedIn string

func (s signedIn) Login(*http.Request) (string, error) { return string(s), nil }

// The document-work check's one gap, held in place: the same parse as the control above, handed
// to another goroutine while the window is open and waited for, holds the transaction id as long,
// but runs on a goroutine that wrote in no window, so the check reports nothing (the gap
// storetest's package doc names). Where the build judges gaps, D6 sees it, since the window's
// goroutine waits on the parse in its gap, which is the code's time; a build that judges none sees
// it nowhere. Were the check ever to follow work across goroutines, this row would fail and its
// doc and that gap would be rewritten.
func TestXidWindowLeavesDocumentWorkHandedToAnotherGoroutineUnseen(t *testing.T) {
	tracer := storetest.NewXidTracer()
	caught := &collabtest.RecordedFailures{TB: t}
	database := &store.Store{Pool: storetest.Traced(caught, storetest.Open(t).Pool, tracer, nil)}
	spec, _ := storetest.XidSpec("DOC", 1)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func() {
			parsed := make(chan error, 1)
			go func() {
				_, err := parseInput(spec)
				parsed <- err
			}()
			if err := testwait.Receive(t, "the handed-off parse", parsed); err != nil {
				t.Errorf("parse the %d-byte spec: %v", len(spec), err)
			}
		})
	})
	if failures := caught.Reported(); len(failures) != 0 {
		t.Fatalf("the document-work check reported a parse it cannot see on another goroutine: %q; "+
			"the gap storetest's package doc names is closed, so rewrite it and this row", failures)
	}
	err := report.Err()
	switch judged := storetest.XidJudgesGaps(); {
	case judged && (err == nil || len(report.Problems) > 0 || !strings.Contains(err.Error(), "XID WINDOW VIOLATION")):
		t.Fatalf("a parse of the %d-byte spec handed to another goroutine after the write: verdict %v, want a D6 violation, the one check that sees it", len(spec), err)
	case !judged && err != nil:
		t.Fatalf("the rig could not judge the transaction: %v", err)
	}
}

// The tracer opens a transaction's xid window, which the collaborator and document-work checks
// read, at the first statement whose own result says it wrote, whether that statement ran on its
// own or in a batch (store.OpenXidWindow), and the window closes when the transaction ends. A read
// before the write leaves it shut, and so does an update that matched no row, which took no
// transaction id. No measurement runs: the tracer opens windows whenever it traces.
func TestXidWindowOpensAtATransactionsFirstWriteInlineOrBatched(t *testing.T) {
	_, database, _ := xidService(t)
	for _, write := range []struct {
		name string
		key  string
		run  func(ctx context.Context, tx pgx.Tx, insert string) error
	}{
		{name: "inline", key: "INL", run: func(ctx context.Context, tx pgx.Tx, insert string) error {
			_, err := tx.Exec(ctx, insert)
			return err
		}},
		{name: "batched", key: "BAT", run: func(ctx context.Context, tx pgx.Tx, insert string) error {
			return store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
				batch.Queue("insert the project", insert)
				return nil
			})
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			ctx := storetest.MarkXid(context.Background())
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			var count int
			if err := tx.QueryRow(ctx, `select count(*) from projects`).Scan(&count); err != nil {
				t.Fatalf("read before the write: %v", err)
			}
			if _, err := tx.Exec(ctx, `update projects set name = name where key = 'NONE'`); err != nil {
				t.Fatalf("update no row: %v", err)
			}
			if store.InXidWindow(ctx) {
				t.Fatal("a transaction that read and updated no row is inside its xid window")
			}
			insert := fmt.Sprintf(`insert into projects (key, name, default_assignee) values ('%s', 'Rig', 'alice')`, write.key)
			if err := write.run(ctx, tx, insert); err != nil {
				t.Fatalf("write: %v", err)
			}
			if !store.InXidWindow(ctx) {
				t.Fatalf("a transaction that wrote %s is outside its xid window", write.name)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if store.InXidWindow(ctx) {
				t.Fatal("a transaction that committed is still inside its xid window")
			}
		})
	}
}

// A write phase that is one batch can hold its xid for less than the poller's interval, which on a
// loaded two-core box reaches tens of milliseconds, so the poller can miss the transaction
// altogether. Such a transaction is still judged, from the window its own statements derive - the
// first one whose result shows it wrote - and never reported as a rig problem; a sleep after that
// write is caught there as it is under a poller that saw the xid.
func TestXidWindowRigJudgesAWriteThePollerNeverSaw(t *testing.T) {
	for _, probe := range []struct {
		name string
		gap  time.Duration
	}{
		{name: "back to back", gap: 0},
		{name: fmt.Sprintf("a %s sleep after the write", rigInjectedGap), gap: rigInjectedGap},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if probe.gap > 0 {
				storetest.RequireXidGapJudgment(t)
			}
			_, database, tracer := xidService(t)
			// Blind for longer than the whole operation: no poll is sent while the transaction is
			// open, so nothing but its own statements says it held an xid.
			tracer.BlindPoller(time.Minute)
			report := tracer.Measure(t, database.Pool, func() {
				runRigTransaction(t, database, probe.gap)
			})
			if len(report.Problems) > 0 {
				t.Fatalf("the rig reported a problem about a transaction no poll saw: %v", report.Problems)
			}
			if len(report.Windows) != 1 {
				t.Fatalf("windows = %d, unsighted = %d, want the one transaction judged",
					len(report.Windows), len(report.Unsighted))
			}
			window := report.Windows[0]
			if !window.Transaction.Sighted.IsZero() {
				t.Fatalf("a poll saw the xid at %s, so this run did not test an unsighted transaction",
					window.Transaction.Sighted)
			}
			if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
				t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s",
					window.Statements[0].SQL, window.Detail())
			}
			err := report.Err()
			switch {
			case probe.gap == 0 && err != nil:
				t.Fatalf("a back-to-back transaction the poller never saw broke D6: %v", err)
			case probe.gap > 0 && (err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") ||
				!strings.Contains(err.Error(), "follows statement 0: insert into projects")):
				t.Fatalf("a %s gap after the write: verdict %v, want a D6 violation after the insert", probe.gap, err)
			}
			t.Logf("%s\n%s", window.Summary(), window.Detail())
		})
	}
}

// runRigTransaction runs, under a marked context, a transaction that reads, spends 20 ms in Go
// before its first write, writes, waits gap, reads twice more and commits.
func runRigTransaction(t *testing.T, database *store.Store, gap time.Duration) {
	t.Helper()
	runRigTransactionDoing(t, database, func() { time.Sleep(gap) })
}

// runRigTransactionDoing is runRigTransaction with between run after the write instead of a
// sleep: what the transaction does in Go while it holds its transaction id.
func runRigTransactionDoing(t *testing.T, database *store.Store, between func()) {
	t.Helper()
	if err := rigTransactionDoing(database, func() error { between(); return nil }); err != nil {
		t.Fatal(err)
	}
}

// rigTransactionDoing is runRigTransactionDoing answering its failure, for a goroutine other than
// the test's, which may not stop the test; between's error fails it too.
func rigTransactionDoing(database *store.Store, between func() error) error {
	ctx := storetest.MarkXid(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	var count int
	if err := tx.QueryRow(ctx, `select count(*) from projects`).Scan(&count); err != nil {
		return fmt.Errorf("read before the write: %w", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('RIG', 'Rig', 'alice')`); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := between(); err != nil {
		return err
	}
	for range 2 {
		if err := tx.QueryRow(ctx, `select count(*) from projects`).Scan(&count); err != nil {
			return fmt.Errorf("read after the write: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Settlement versions a document a person edited live: it renders the room, refreshes every open
// anchor against the new tree and writes the version, its anchors and its events in one
// transaction (settleRoomWithin).
func TestXidWindowSettlementVersionWrite(t *testing.T) {
	service, database, tracer := xidService(t)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	artifactID, before, generation := settlementFixture(t, service, database, alice)

	report := tracer.Measure(t, database.Pool, func() {
		service.settleRoomWithin(storetest.MarkXid(context.Background()), artifactID, generation, false)
	})
	if after := latestVersionNumber(t, service, artifactID); after != before+1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): settlement left the document at version %d, want %d", after, before+1)
	}
	storetest.AssertXidWindows(t, report, storetest.XidBatchedWindow)
}

// A closed issue's settlement versions what a person's browser stored before the close, and its
// write phase holds the closed path's own writes beside the version and its anchors
// (settlement.commitVersion): the delete of the pending row keeping the editor it credits as the
// document's closed edit source (clearSettlementPending), and the delete of the document's edit
// notice (dropEditNotice), here the held notice a person's version opened before the close. The
// issue closes in the database alone once alice's edit is durable, as a close this process has not
// heard of yet: the room stays resident, and the settlement reads the close off the issue row
// under the owner's lock (lockArtifactOwner).
func TestXidWindowClosedIssueSettlementVersionWrite(t *testing.T) {
	service, database, tracer := xidService(t)
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	artifactID, before, generation := settlementFixture(t, service, database, alice)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, $2::integer - 1, $2, '["alice@example.com"]', null, null)
	`, artifactID, before); err != nil {
		t.Fatalf("hold the edit notice alice's version opened: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the issue: %v", err)
	}

	report := tracer.Measure(t, database.Pool, func() {
		service.settleRoomWithin(storetest.MarkXid(ctx), artifactID, generation, false)
	})
	if after := latestVersionNumber(t, service, artifactID); after != before+1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): settlement left the document at version %d, want %d", after, before+1)
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept == nil || !kept.SameAs(alice) {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the settlement kept %+v as the document's closed edit source, want %s: it did not settle the closed issue's document", kept, alice.ID)
	}
	var notices int
	if err := database.Pool.QueryRow(ctx, `select count(*) from document_edit_notices where artifact_id = $1`, artifactID).Scan(&notices); err != nil {
		t.Fatalf("count the document's edit notices: %v", err)
	}
	if notices != 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the closed issue's settlement left the document's edit notice")
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(2,
		"the write phase's one batch - the pending row's delete keeping the closed edit source, the version and its anchors, the edit notice's delete and the version's event - and its COMMIT"))
}

// settlementFixture is the document the settlement window tests settle: one of the load harness's
// large specs on issue DOC-1 with 20 open anchored asks, live in its room, where editor's browser
// has bumped the revision token and the room has stored that update durably. It answers the
// document, its latest version and the room generation a settlement of it runs at.
func settlementFixture(t *testing.T, service *Service, database *store.Store, editor model.Actor) (string, int, uint64) {
	t.Helper()
	spec, quotes := storetest.XidSpec("DOC", 1)
	artifactID := createDocument(t, database, "# First")
	seedServiceText(t, service, artifactID, spec)
	step := len(quotes) / 24
	for i := range 20 {
		seedAnchoredAsk(t, service, artifactID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), quotes[i*step], quotes[i*step])
	}
	editAsConnectedPeer(t, service, artifactID, 1, editor, replaceRun("Revision token r0", "Revision token r1"))
	before := latestVersionNumber(t, service, artifactID)

	ctx := testwait.Context(t)
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	t.Logf("xid fixture: document of %d bytes with 20 open anchored asks", len(spec))
	return artifactID, before, generation
}

// The edit-notice sweep claims a due notice, then reads both versions it names, parses them to
// tell a server-state-only change from a person's edit and diffs them (sendEditNotice).
func TestXidWindowEditNoticeSweep(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, quotes := storetest.XidSpec("DOC", 1)
	document := createIssueDocument(t, database, 2, spec)
	revised := versionMarkdown(t, database, document, 1)
	revised = strings.Replace(revised, "Revision token r0", "Revision token r1", 1)
	for _, quote := range []string{quotes[10], quotes[len(quotes)/2], quotes[len(quotes)-10]} {
		revised = strings.Replace(revised, quote, strings.Replace(quote, "holds", "stands", 1), 1)
	}
	addPersonsVersion(t, database, document, 2, revised, model.Actor{Kind: "user", ID: "alice@example.com"})
	t.Logf("xid fixture: two versions of %d and %d bytes", len(spec), len(revised))

	var sent int
	report := tracer.Measure(t, database.Pool, func() {
		var err error
		sent, err = service.sendDueEditNotices(storetest.MarkXid(context.Background()))
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
	})
	if sent != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the sweep sent %d notices, want 1", sent)
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(2, "the notice's claiming delete and its COMMIT"))
}

// The task-progress reconciliation counts the tasks of each issue whose recorded count is not of
// its spec's latest version, one batch of issues per transaction, holding their rows: twenty issues
// with large specs, as a deploy whose overlap wrote versions through code that records no count
// leaves them.
func TestXidWindowReconcileTaskProgress(t *testing.T) {
	service, database, tracer := xidService(t)
	spec, _ := storetest.XidSpec("DOC", 1)
	const issues = 20
	for number := 1; number <= issues; number++ {
		document := createIssueDocument(t, database, number, spec)
		if _, err := database.Pool.Exec(context.Background(), `update artifacts set is_primary = true where id = $1`, document); err != nil {
			t.Fatalf("make document %d its issue's spec: %v", number, err)
		}
	}
	t.Logf("xid fixture: %d issues whose specs of %d bytes have no task count", issues, len(spec))

	report := tracer.Measure(t, database.Pool, func() {
		service.ReconcileTaskProgress(storetest.MarkXid(context.Background()))
	})
	var drifted int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from issues i where `+taskProgressDrift).Scan(&drifted); err != nil {
		t.Fatalf("count drifted issues: %v", err)
	}
	if drifted != 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %d issues still have no count of their spec's latest version", drifted)
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(issues+1,
		"one task-count update per issue of the batch and its COMMIT"))
}

// The block-index tick's window arm gives windows to the owners whose blocks carry none: a
// document in a transaction of its own, which reads the document the server serves before it
// takes the owner row (windowDocument), and a comment in one of its own, which reads the body
// its blocks were split from under the owner's lock and takes no row lock (windowItem). Either
// way the whole write is the one statement that writes the windows, so the window is that
// statement and the commit after it.
func TestXidWindowBlockIndexWindowArm(t *testing.T) {
	service, database, tracer := xidService(t)
	ctx := context.Background()
	spec, _ := storetest.XidSpec("DOC", 1)
	document := createIssueDocument(t, database, 1, spec)
	// The live text, which the index and the window arm both read: a document whose room holds
	// none is indexed with no block, and a block index with no block owes no window.
	seedServiceText(t, service, document, spec)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values ('DOC-1', '{"kind":"user","id":"alice"}', $1)
	`, spec); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	// Ticks until nothing is owed index the blocks of both and write their windows. Owedness is
	// the marker the write of the windows cleared (windows_owed_at, INFRA-988), so dropping the
	// window rows and marking each owner again leaves each owed its windows and nothing else,
	// which is what the arm repairs.
	for range 5 {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Fatalf("index the blocks of the document and the comment: %v", err)
		}
		if tick.Documents == 0 && tick.ItemBlocks == 0 {
			break
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		delete from embedding_window_states;
		delete from embeddings;
		update document_block_versions set windows_owed_at = now();
		update comment_block_state set windows_owed_at = now();
	`); err != nil {
		t.Fatalf("drop the windows: %v", err)
	}
	owedDocuments, err := refs.DocumentsOwedWindows(ctx, database.Pool, "", 10)
	if err != nil {
		t.Fatalf("list the documents owed their windows: %v", err)
	}
	owedComments, err := refs.ItemsOwedWindows(ctx, database.Pool, refs.ItemComment, "", 10)
	if err != nil {
		t.Fatalf("list the comments owed their windows: %v", err)
	}
	if len(owedDocuments) != 1 || len(owedComments) != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %d documents and %d comments are owed their windows, want one of each",
			len(owedDocuments), len(owedComments))
	}
	t.Logf("xid fixture: a document and a comment of %d bytes whose blocks hold no window", len(spec))

	var tick BlockIndexTick
	report := tracer.Measure(t, database.Pool, func() {
		var err error
		tick, err = service.ReconcileBlockIndex(storetest.MarkXid(ctx), BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Errorf("window the owners whose blocks hold none: %v", err)
		}
	})
	if tick.Windows != 2 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the tick windowed %d owners, want the document and the comment", tick.Windows)
	}
	var windows int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from embeddings where split_part(id, '#', 1) = $1`, document).Scan(&windows); err != nil {
		t.Fatalf("count the document's windows: %v", err)
	}
	if windows == 0 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the arm left the document with no window")
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(2,
		"the one statement that writes the owner's windows and its COMMIT"))
}
