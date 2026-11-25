package texttrees_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m.Run)) }

// A stored tree is read back in an array of its own grown by append, whose capacity is the whole
// allocation the runtime rounded the tree's length up to. The display cache charges a tree its
// array's capacity (pmdoc.PutStoredDisplayTree), so an array made exactly the tree's length, as a
// scan into a []byte makes it, would hide that rounding from the charge while the heap still held
// it.
func TestAStoredTreeIsReadBackInAnArrayAsLongAsItsAllocation(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	// A tree of 1,000 bytes: no size the runtime allocates, so its allocation is longer than it.
	const frame = `{"type":"doc","text":""}`
	tree := json.RawMessage(`{"type":"doc","text":"` + strings.Repeat("a", 1000-len(frame)) + `"}`)
	sum := pmdoc.TextSum("A text whose tree this test stores itself.")
	if err := table.StoreTrees(ctx, pmdoc.DisplayLatest, []texttrees.Tree{{Sum: sum, Tree: tree}}); err != nil {
		t.Fatalf("store the tree: %v", err)
	}
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{sum})
	if err != nil {
		t.Fatalf("read the tree: %v", err)
	}
	got := stored[sum]
	if !bytes.Equal(got, tree) {
		t.Fatalf("the stored tree is read back as\n\t%s\nwant\n\t%s", got, tree)
	}
	allocated := cap(append(json.RawMessage(nil), tree...))
	if allocated == len(tree) {
		t.Fatalf("a tree of %d bytes is allocated exactly its length, so this test shows nothing: choose another length", len(tree))
	}
	if cap(got) != allocated {
		t.Errorf("the tree of %d bytes is read back in an array of %d, want the %d its allocation holds", len(got), cap(got), allocated)
	}
}

// A text's tree is stored once and read back as the bytes pmdoc.DisplayJSON answers, which is what
// a response carries: a read serves the stored bytes rather than reading the markdown again.
func TestAStoredTreeIsReadBackAsTheTreeTheTextIsShownAs(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	const body = "Check **this** before the deploy, in [staging](https://example.test)."
	const question = "Ship the ~~draft~~ release today?"
	if err := table.Store(ctx, pmdoc.DisplayLatest, body, question); err != nil {
		t.Fatalf("store two texts: %v", err)
	}
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(body), pmdoc.TextSum(question)})
	if err != nil {
		t.Fatalf("read the two trees: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("the read answered %d rows, want the two stored", len(stored))
	}
	for _, text := range []string{body, question} {
		want := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest)
		if got := stored[pmdoc.TextSum(text)]; !bytes.Equal(got, want) {
			t.Errorf("the stored tree of %q is\n\t%s\nwant\n\t%s", text, got, want)
		}
	}
	// A sum nothing stored is simply absent, which is how its caller knows to read the text.
	unstored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum("Nothing stored this.")})
	if err != nil {
		t.Fatalf("read an unstored sum: %v", err)
	}
	if len(unstored) != 0 {
		t.Errorf("the read answered %d rows for a text nothing stored, want none", len(unstored))
	}
}

// A text stored again keeps the row it has: the key is the reading, so a second store of the same
// text writes nothing and refuses nothing.
func TestStoringATextAgainKeepsTheRowItHas(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	const body = "The *staging* run is green."
	if err := table.Store(ctx, pmdoc.DisplayLatest, body); err != nil {
		t.Fatalf("store the text: %v", err)
	}
	// The sum is handed to a query of the test's own as the bytes the column holds: pgx encodes a
	// byte slice as bytea and knows no plan for the array pmdoc.TextSum answers.
	sum := pmdoc.TextSum(body)
	var created string
	if err := database.Pool.QueryRow(ctx,
		`select created_at::text from text_trees where sum = $1`, sum[:]).Scan(&created); err != nil {
		t.Fatalf("read the row's time: %v", err)
	}
	// The same text twice in one batch, and then a batch of its own: neither writes a second row.
	if err := table.Store(ctx, pmdoc.DisplayLatest, body, body); err != nil {
		t.Fatalf("store the text twice over: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayLatest, body); err != nil {
		t.Fatalf("store the text again: %v", err)
	}
	var rows int
	var stillCreated string
	if err := database.Pool.QueryRow(ctx,
		`select count(*), min(created_at)::text from text_trees where sum = $1`, sum[:]).Scan(&rows, &stillCreated); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("the table holds %d rows for one text, want 1", rows)
	}
	if stillCreated != created {
		t.Errorf("the row was written again at %s, want the %s it had", stillCreated, created)
	}
}

// The two display modes of one text are two trees (pmdoc.ReadForDisplay), so they are two rows,
// and a read in one mode never answers the other's.
func TestTheTwoModesOfOneTextAreTwoRows(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	const text = ":::ask{state=\"answered\"}\nIs it ready?\n:::\n"
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store the latest reading: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayVersion, text); err != nil {
		t.Fatalf("store the version reading: %v", err)
	}
	sum := pmdoc.TextSum(text)
	var rows int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from text_trees where sum = $1`, sum[:]).Scan(&rows); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the table holds %d rows for one text in two modes, want 2", rows)
	}
	for _, mode := range []pmdoc.DisplayMode{pmdoc.DisplayLatest, pmdoc.DisplayVersion} {
		stored, err := table.Read(ctx, mode, [][32]byte{pmdoc.TextSum(text)})
		if err != nil {
			t.Fatalf("read mode %d: %v", mode, err)
		}
		want := pmdoc.DisplayJSON(text, mode)
		if got := stored[pmdoc.TextSum(text)]; !bytes.Equal(got, want) {
			t.Errorf("mode %d answered\n\t%s\nwant\n\t%s", mode, got, want)
		}
	}
}

// A tree stored at one display parser version is not read at another: a build whose reading of a
// text has changed reads every text again rather than serving a tree the build before it made.
func TestATreeStoredAtOneVersionIsNotReadAtAnother(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	const text = "A tree the version before this one stored."
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store at this version: %v", err)
	}
	restore := pmdoc.SetDisplayTreeVersionForTest(pmdoc.DisplayTreeVersion + 1)
	defer restore()
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(text)})
	if err != nil {
		t.Fatalf("read at the next version: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("the read at version %d answered the tree stored at %d",
			pmdoc.DisplayTreeVersionNow(), pmdoc.DisplayTreeVersion)
	}
	// The next version stores its own reading beside the one before it, which stays where it is.
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store at the next version: %v", err)
	}
	if stored, err = table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(text)}); err != nil {
		t.Fatalf("read the next version's row: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("the read at version %d answered %d rows, want its own", pmdoc.DisplayTreeVersionNow(), len(stored))
	}
	restore()
	if stored, err = table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum(text)}); err != nil {
		t.Fatalf("read this version's row again: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("the row stored at version %d is gone after the next version stored its own", pmdoc.DisplayTreeVersion)
	}
}

// A tree stored at display parser version 4, which read an address written after `](` on past its
// `)` and so held an email typed straight after that `)` as text, is not served at this build's
// version: the text is read again, and the email is the link this build's reading makes of it
// (text.DispatchSpans ends such an address at its `)`).
func TestATreeStoredBeforeALinkAddressEndedAtItsParenthesisIsReadAgain(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(storetest.Open(t).Pool)
	const text = `Mail \[w](dispatch://ACME-1)me@example.com now.`
	sum := pmdoc.TextSum(text)
	old := json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[` +
		`{"type":"text","text":"Mail [w](dispatch://ACME-1)me@example.com now."}]}]}`)
	restore := pmdoc.SetDisplayTreeVersionForTest(4)
	err := table.StoreTrees(ctx, pmdoc.DisplayLatest, []texttrees.Tree{{Sum: sum, Tree: old}})
	restore()
	if err != nil {
		t.Fatalf("store the tree version 4 read: %v", err)
	}
	stored, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{sum})
	if err != nil {
		t.Fatalf("read at version %d: %v", pmdoc.DisplayTreeVersion, err)
	}
	if len(stored) != 0 {
		t.Fatalf("the read at version %d answered the tree stored at version 4: %s", pmdoc.DisplayTreeVersion, stored[sum])
	}
	reading := pmdoc.DisplayJSON(text, pmdoc.DisplayLatest)
	if !bytes.Contains(reading, []byte(`"href":"mailto:me@example.com"`)) {
		t.Fatalf("this build reads %q as\n\t%s\nwhich links no email", text, reading)
	}
	if err := table.Store(ctx, pmdoc.DisplayLatest, text); err != nil {
		t.Fatalf("store at version %d: %v", pmdoc.DisplayTreeVersion, err)
	}
	if stored, err = table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{sum}); err != nil {
		t.Fatalf("read this version's row: %v", err)
	}
	if got := stored[sum]; !bytes.Equal(got, reading) {
		t.Errorf("the tree stored at version %d is\n\t%s\nwant\n\t%s", pmdoc.DisplayTreeVersion, got, reading)
	}
}

// An empty text has no tree and no row: a write that stored nothing leaves the table as it was.
func TestAnEmptyTextStoresNothing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	table := texttrees.New(database.Pool)
	if err := table.Store(ctx, pmdoc.DisplayLatest, ""); err != nil {
		t.Fatalf("store an empty text: %v", err)
	}
	if err := table.Store(ctx, pmdoc.DisplayLatest); err != nil {
		t.Fatalf("store no text at all: %v", err)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from text_trees`).Scan(&rows); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("the table holds %d rows after storing an empty text, want none", rows)
	}
}

// Every method of a table with no pool behind it answers an error and none of them panics: storing
// a tree is never allowed to fail the write it belongs to, and a server with no database refuses
// that write itself.
func TestATableWithNoPoolAnswersAnError(t *testing.T) {
	ctx := context.Background()
	table := texttrees.New(nil)
	if err := table.Store(ctx, pmdoc.DisplayLatest, "A text."); err == nil {
		t.Error("the store on a table with no pool answered no error")
	}
	if _, err := table.Read(ctx, pmdoc.DisplayLatest, [][32]byte{pmdoc.TextSum("A text.")}); err == nil {
		t.Error("the read on a table with no pool answered no error")
	}
}
