package api

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// itemRowTables are the rows an item's owner lock serialises: a comment, a message and an ask,
// and the block-state row each of them owns, whose owed_at and windows_owed_at say what the
// index still owes that item. They are the rows main's `select ... for no key update of t` held
// and the advisory lock at refs.LockItemOwner does not (itemOwnerLockRule).
var itemRowTables = []string{
	"comments", "messages", "asks",
	"comment_block_state", "message_block_state", "ask_block_state",
}

var (
	// itemRowWrite matches a statement that writes an item row by name, wherever in the
	// statement it sits: a data-modifying common table expression writes one as readily as a
	// top-level statement, so the match is over the whole statement rather than its opening.
	itemRowWrite = regexp.MustCompile(
		`\b(?:insert into|update|delete from) (?:` + strings.Join(itemRowTables, "|") + `)\b`)
	// formattedTableWrite matches a write whose table is a format verb (`update %s as target`,
	// docs/anchor_refresh.go): the statement writes a table this scan cannot read, which may be
	// an item row, so its function is on the census like any other writer. The verb must be a
	// string's, and an update must reach its `set` with no second verb in between, so that an
	// error message counting a document's updates ("the room took the update %s ago",
	// docs/append_gate.go) is not read as one.
	formattedTableWrite = regexp.MustCompile(
		`\b(?:insert into|delete from) %[sq]\b|\bupdate %[sq]\b[^%]*\bset\b`)
	// writeKeywordTail matches the left half of a concatenation whose next operand is the table:
	// `update ` + tables.state + ` set ...` (refs/item_blocks.go). An operand this scan cannot
	// spell leaves no table word in the assembled statement at all, so the shape is read off the
	// expression rather than off the text (unspelledTableWrites).
	writeKeywordTail = regexp.MustCompile(`\b(?:insert into|update|delete from)\s+$`)
)

// itemOwnerLockRule is what an unlisted writer is told: the rule it broke, and where the rule is
// proved, since this census proves only that the writer was named.
const itemOwnerLockRule = "Every writer of a comment, a message, an ask or one of their block-state rows takes that item's\n" +
	"owner lock first - its issue's, or the document a comment or an ask on a document hangs on\n" +
	"(store.LockOwner, through refs.LockItemOwner, api's lockedComment and lockCommentOwner, docs'\n" +
	"lockArtifactOwner) - and reads what it writes in a statement after that lock, never in the same\n" +
	"one. The lock is advisory, so it excludes exactly the writers that take it: main's row lock on\n" +
	"the item row also excluded a writer that took no owner lock, and this one does not.\n" +
	"Take the owner's lock, then add the function below with the lock it takes - and, where the\n" +
	"writer is one a server runs, add a row to the table that holds it behind a lock somebody else\n" +
	"holds (TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites,\n" +
	"item_row_owner_lock_test.go). This census reads names; that test reads locks."

// itemRowWriters is the census: every function of this module that writes an item row, or that
// writes a table this scan cannot read, with the owner lock it takes before the write. A writer
// the scan finds and this map does not name reds the test; an entry no writer answers reds it
// too, since a census that keeps a function it no longer covers stops saying anything.
//
// The reason beside each entry is for the reader, not for the test: nothing here compares it
// with the lock the function takes, or with any lock at all. It is how one writer of this module
// tells the next why their write is safe, and whoever adds an entry writes the reason the entry
// above theirs is written in - the lock named, and the caller or helper that takes it.
//
// Six entries take no owner lock - four exceptions, since cmd/dispatch-seed's three loaders share
// one - and each says why. Everything else on the list holds the item's owner, which is what
// makes the advisory lock at refs.LockItemOwner enough: those that take it in a transaction they
// open are rows of the lock table, which proves it by holding it (item_row_owner_lock_test.go),
// and those that write inside a transaction their caller opened rest on the lock that caller
// took, which no test here or there drives; the gap map (itemRowLockNoRow) names each with why,
// and TestTheLockTablePlacesEveryWriterTheCensusNames keeps every entry in one of the two. That
// any of them still takes or holds it is read off no entry here.
var itemRowWriters = map[string]map[string]string{
	"cmd/dispatch-seed/write_items.go": {
		"writeAsks":     seedLoaderException,
		"writeComments": seedLoaderException,
		"writeMessages": seedLoaderException,
	},
	"internal/dispatch/api/ask_asked_of.go": {
		"putAskAskedOf": "the ask's owner lock, taken with the read of the owner it guards " +
			"(requireOpenOwner, store.LockOwnerAndRead)",
	},
	"internal/dispatch/api/ask_transitions.go": {
		"answerTransition": "the ask's owner lock, which transitionAskTx takes before the " +
			"transition reads the ask (requireOpenOwner)",
	},
	"internal/dispatch/api/asks.go": {
		"createAskFor": "the owner's lock (requireOpenOwnerStatus)",
		"editAsk":      "the ask's owner lock, taken before the document's (requireOpenOwner)",
	},
	"internal/dispatch/api/broadcasts.go": {
		"writeBroadcast": "none, and none to take: each copy is a direct message, whose " +
			"conversation is no owner (store.OwnerOf), and nothing rewrites a message's body",
	},
	"internal/dispatch/api/comment_actions.go": {
		"commentAction": "the comment's owner lock (lockedComment, lockOwnerOfComment)",
		"editComment":   "the comment's owner lock (lockedComment, lockOwnerOfComment)",
		"reopenComment": "the comment's owner lock (lockedComment, lockOwnerOfComment)",
	},
	"internal/dispatch/api/comment_create.go": {
		"createCommentFor": "the owner's lock, taken inside the comment's transaction " +
			"(lockCommentOwner)",
	},
	"internal/dispatch/api/comment_delivery.go": {
		"replyComment": "the comment's owner lock (lockedCommentForDelivery, lockOwnerOfComment)",
	},
	"internal/dispatch/api/message_delivery.go": {
		"insertSessionReply": "the issue's owner lock where the conversation names an issue " +
			"(replyMessage's lockIssue); a direct message has no owner and needs none",
	},
	"internal/dispatch/api/messages.go": {
		"createStoredMessage": "the issue's owner lock where the message names an issue " +
			"(lockIssue); a direct message has no owner and needs none",
	},
	"internal/dispatch/api/reviews.go": {
		"handBackApprovalAsk": "the document's owner lock (requireOpenOwner)",
		"openApprovalAsk":     "the document's owner lock (requireOpenOwner)",
	},
	"internal/dispatch/docs/anchor_blocks.go": {
		"backfillAnchorBlock": "the row's owner lock, taken in the row's own transaction before " +
			"the row is read (refs.LockItemOwner)",
	},
	"internal/dispatch/docs/anchor_refresh.go": {
		"refreshAnchors": "the document's owner lock, which the version write that calls it " +
			"holds (lockArtifactOwner, writeVersionTx)",
	},
	"internal/dispatch/docs/approval_ask.go": {
		"RewriteApprovalAsk": "the document's owner lock, held by the version write that moves " +
			"the request or by the route that hands it back (lockArtifactOwner, requireOpenOwner)",
	},
	"internal/dispatch/docs/approval_carry.go": {
		"answerApprovalAsk": "the document's owner lock, which the version write holds " +
			"(lockArtifactOwner)",
	},
	"internal/dispatch/docs/ask_blocks.go": {
		"createAskBlock":      "the document's owner lock, which settlement holds (lockArtifactOwner)",
		"reconcileAskBlocks":  "the document's owner lock, which settlement holds (lockArtifactOwner)",
		"restoreRetractedAsk": "the document's owner lock, which settlement holds (lockArtifactOwner)",
	},
	"internal/dispatch/docs/ask_row.go": {
		"queueAskResolution": "the ask's owner lock, which every caller holds: the route that " +
			"resolves it (WriteAskResolution) and settlement (lockArtifactOwner)",
		"WriteRestoredAsks": "the issue's owner lock, which the reopen holds " +
			"(api's prepareRestoration, under lockIssue)",
	},
	"internal/dispatch/docs/decided_wording.go": {
		"restoreWordings": "none at boot, which is the boot restoration's exception: " +
			"RestoreDecidedWordings runs it once at boot under the copy locks alone, holding no " +
			"owner lock and no ask's row, and refuses the restoration when the rows moved under " +
			"it (errDecidedWordingsMoved). Settlement's own call holds the document's owner lock " +
			"(restoreDocumentDecidedWordings, lockArtifactOwner)",
	},
	"internal/dispatch/refs/item_blocks.go": {
		"clearItemWindowsOwed": "the item's owner lock, which the window arm takes (LockItemOwner)",
		"markItemBlocksOwed":   "the item's owner lock, which the window arm takes (LockItemOwner)",
		"queueBlockRefs": "the item's owner lock, which the caller holds (LockItemOwner, " +
			"readSourceUnderOwner, or the writing route's own); its tables are the item's block " +
			"and block-ref tables, no item row",
		"queueBlocks": "the item's owner lock, which the caller holds (LockItemOwner, " +
			"readSourceUnderOwner, or the writing route's own); its tables are the item's block " +
			"and block-text tables, no item row",
		"queueRecordIndexed": "the item's owner lock, which the caller holds (LockItemOwner, " +
			"readSourceUnderOwner, or the writing route's own)",
		"queueStateLock": "the item's owner lock, which the caller holds (LockItemOwner, " +
			"readSourceUnderOwner, or the writing route's own)",
	},
	"internal/dispatch/refs/rebuild.go": {
		"RebuildAll": "none, which is rebuild-refs' exception: it raises the owed markers " +
			"of every comment, message and ask so the index re-derives them, and writes no item " +
			"row and no other column of a state row. It is an operator's command over the whole " +
			"table, which no owner lock covers",
		"settleOwedStatement": "the source's owner lock, which the drain holds when it settles " +
			"a row (stillOwed); its table is an owed queue, no item row",
	},
	"internal/dispatch/refs/refs.go": {
		"queueStamp": "the source's owner lock, which every caller holds before its write phase; " +
			"its tables are refs and the item block-ref tables, no item row",
	},
}

// seedLoaderException is why the seed's three loaders are on the census with no lock, written so
// that a reader of the allowlist can weigh the exception without leaving the file. An owner lock
// serialises writers of one owner against each other, and cmd/dispatch-seed is a command a person
// runs from a shell against an empty, migrated database, filling it before any server is started
// on it (cmd/dispatch-seed/main.go): while it writes there is no second writer for a lock to
// serialise it against. Run instead against a database a server serves, these three would write
// comment, ask and message rows beside the routes, and each would need the owner lock every
// serving writer takes; that is the line this exception sits on.
const seedLoaderException = "none, and none to take: cmd/dispatch-seed is a command run from a " +
	"shell, filling an empty database before any server is started on it, so while it writes " +
	"there is no other writer for a lock to serialise it against; pointed at a database a " +
	"server serves it would take the owner lock like every serving writer"

// TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus holds the names half of the invariant the
// advisory owner lock rests on: every writer of a comment, a message, an ask or one of their
// block-state rows takes that item's owner lock (itemOwnerLockRule), and this half is that every
// such writer is named.
//
// main held it with a row lock. A writer of the item took the item's row `for no key update`, so
// it excluded every other writer of that row, whether or not that writer took the owner's lock.
// The advisory lock excludes only the writers that take it (store.LockOwner), so the one thing
// the row lock covered and this lock does not is a writer that changes or deletes the item row
// without taking its owner. No writer a server runs is one: the lock table holds those that open
// their own transaction behind a held owner lock (item_row_owner_lock_test.go), those that write
// inside a transaction their caller opened rest on the lock that caller took, which that table
// does not drive (itemRowLockNoRow names each with why), and the six entries here that take no
// owner lock each say why they take none. What this test keeps out is a writer arriving unnamed:
// a `delete from comments` added tomorrow (dispatch://INFRA-1523) reds here until its function is
// on the census with the lock it takes.
//
// What it catches is a missing name, in either direction: a writer the scan finds that no entry
// names, and an entry whose function no longer exists. What it does not catch is the lock. The
// reason beside an entry is prose, and nothing here reads it: an entry may name a helper the
// module renamed long ago, or a route whose lock was taken out and whose read moved to the pool,
// and this test stays green either way, because the function is still there and still writes the
// row. A green census says who writes an item row. It does not say that any of them still locks,
// and reading it as if it did is how a change that dropped a lock would walk through this gate.
//
// The lock is proved by holding it. In item_row_owner_lock_test.go,
// TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites takes an item's owner lock in
// a transaction of its own and requires the writer to wait for it, over a table of the serving
// writers of these rows. That table is the other half of this one - names here, locks there -
// and every entry of this map is placed in it: a row that drives the writer, or a named reason
// why it has none. An entry added here and placed nowhere reds
// TestTheLockTablePlacesEveryWriterTheCensusNames, so what that table leaves out stays written
// down rather than going quietly out of date.
//
// Its reach is the statement as this scan can assemble it, which is the reach the lock-level
// guard documents (TestNoForUpdateOnOwnerTables): a literal, or a chain of literals and
// package-level constants resolved to a fixpoint. Two shapes of this module write a table the
// scan cannot read, and both are refused rather than passed over, since either may be an item
// row:
//
//   - a table a format verb fills (`update %s as target`, docs/anchor_refresh.go), read off the
//     statement text (formattedTableWrite);
//   - a table a runtime value fills (`update ` + tables.state + ` set ...`,
//     refs/item_blocks.go), which leaves no table word in the assembled statement and is read
//     off the expression instead (unspelledTableWrites).
//
// The second is deliberately loose: a batch label built the same way ("update "+table+" anchor")
// reds beside the statement it labels. The tell is the entry itself - the function named is a
// writer whatever its label does - and a function on the census for a table that is never an
// item row says so in its reason.
//
// A statement declared outside any function - a package-level const holding SQL - is not a
// writer, and is passed over: the function that runs it resolves that constant through the same
// map and is found there, which is how docs/decided_wording.go's restore is found in
// restoreWordings.
func TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus(t *testing.T) {
	sources := moduleSources(t, "../../..")
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var unlisted []string
	answered := map[string]map[string]bool{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		account := func(located locatedStatement) {
			// A statement outside any function is a declaration, not a writer.
			if located.function == "" {
				return
			}
			file, function := censusEntry(path, located.function)
			if file == "" {
				unlisted = append(unlisted, fmt.Sprintf("%s %s: %s", path, located.function, located.statement))
				return
			}
			if answered[file] == nil {
				answered[file] = map[string]bool{}
			}
			answered[file][function] = true
		}
		for _, located := range sqlStatementsByFunction(source.file, constants) {
			if writesAnItemRow(located.statement) {
				account(located)
			}
		}
		for _, located := range unspelledTableWrites(source.file, constants) {
			account(located)
		}
	}
	slices.Sort(unlisted)
	if unlisted = slices.Compact(unlisted); len(unlisted) > 0 {
		t.Errorf("a writer of an item row is on no census entry (itemRowWriters):\n\t%s\n%s",
			strings.Join(unlisted, "\n\t"), itemOwnerLockRule)
	}
	var stale []string
	for file, functions := range itemRowWriters {
		for function := range functions {
			if !answered[file][function] {
				stale = append(stale, file+" "+function)
			}
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		t.Errorf("a census entry answers no writer of an item row, so it says nothing:\n\t%s\n"+
			"Delete it, or name the function that writes the row now.", strings.Join(stale, "\n\t"))
	}
}

// itemRowCallersTransaction is why a writer that runs inside another writer's transaction has no
// row of the lock table (itemRowLockTable, item_row_owner_lock_test.go): the caller's transaction
// holds the owner, so a row there would prove the caller's lock and say nothing about the callee.
const itemRowCallersTransaction = "runs inside another writer's transaction, which already holds " +
	"the owner, so a row of the lock table would prove the caller's lock and say nothing about " +
	"the callee. Covering it costs driving its caller - a version write and the settlement behind " +
	"it, or the block-index tick - and then telling the callee's statement from its caller's, " +
	"which one call on one backend does not tell"

// itemRowNoOwnerLock is why a writer that takes no owner lock has no row: there is nothing for it
// to wait for, and the census entry says why it takes none.
const itemRowNoOwnerLock = "takes no owner lock at all, which is its census entry's exception: " +
	"there is nothing for it to wait for, and a row asserting that it does not wait would prove " +
	"the exception rather than the rule"

// itemRowLockNoRow is the lock table's gap map: every writer of the census that the lock table
// (itemRowLockTable, item_row_owner_lock_test.go) does not drive, with why it has no row.
// Together with itemRowLockTable it accounts for the census exactly, which
// TestTheLockTablePlacesEveryWriterTheCensusNames holds it to: a writer of an item row added
// tomorrow is a row of that table, or it is named here with what its row would cost - the fixture
// it needs - and either way nobody has to read the table to find out which writers it left out.
var itemRowLockNoRow = map[string]map[string]string{
	// Writers whose statements run in a transaction some other writer opened and locked.
	"internal/dispatch/docs/anchor_refresh.go": {"refreshAnchors": itemRowCallersTransaction},
	"internal/dispatch/docs/approval_ask.go":   {"RewriteApprovalAsk": itemRowCallersTransaction},
	"internal/dispatch/docs/approval_carry.go": {"answerApprovalAsk": itemRowCallersTransaction},
	"internal/dispatch/docs/ask_blocks.go": {
		"createAskBlock":      itemRowCallersTransaction,
		"reconcileAskBlocks":  itemRowCallersTransaction,
		"restoreRetractedAsk": itemRowCallersTransaction,
	},
	"internal/dispatch/docs/ask_row.go": {
		"queueAskResolution": itemRowCallersTransaction,
		"WriteRestoredAsks":  itemRowCallersTransaction,
	},
	"internal/dispatch/refs/item_blocks.go": {
		"clearItemWindowsOwed": itemRowCallersTransaction,
		"markItemBlocksOwed":   itemRowCallersTransaction,
		"queueBlockRefs":       itemRowCallersTransaction,
		"queueBlocks":          itemRowCallersTransaction,
		"queueRecordIndexed":   itemRowCallersTransaction,
		"queueStateLock":       itemRowCallersTransaction,
	},
	"internal/dispatch/refs/refs.go": {"queueStamp": itemRowCallersTransaction},

	// Writers that take no owner lock, each by its census entry's own exception.
	"cmd/dispatch-seed/write_items.go": {
		"writeAsks":     itemRowNoOwnerLock,
		"writeComments": itemRowNoOwnerLock,
		"writeMessages": itemRowNoOwnerLock,
	},
	"internal/dispatch/api/broadcasts.go":       {"writeBroadcast": itemRowNoOwnerLock},
	"internal/dispatch/docs/decided_wording.go": {"restoreWordings": itemRowNoOwnerLock},
	"internal/dispatch/refs/rebuild.go": {
		"RebuildAll": itemRowNoOwnerLock,
		// The drain's settle runs under the owner lock the drain took for the row it settles.
		"settleOwedStatement": itemRowCallersTransaction,
	},
}

// TestTheLockTablePlacesEveryWriterTheCensusNames holds the lock table and the census to each
// other. The census names every writer of an item row (itemRowWriters); the lock table
// (itemRowLockTable, item_row_owner_lock_test.go) drives the ones a server runs in a transaction
// of their own, and itemRowLockNoRow names the rest with why they have none. Three things red
// here:
//
//   - a row naming a writer no census entry holds, which is a function renamed or taken out on
//     one side alone, leaving a row that drives something the census does not call a writer;
//   - a writer the census names that is on no row and in no gap, which is the serving writer
//     added to the census and not to the lock table - the stale table this check exists to
//     prevent;
//   - a writer both driven and excused, where the two lists disagree about what was paid for.
//
// It reads names alone and needs no database, as the census does: what the rows prove about locks
// is TestEveryWriterOnTheLockTableWaitsForTheOwnerLockOfTheRowItWrites's half.
func TestTheLockTablePlacesEveryWriterTheCensusNames(t *testing.T) {
	placed := map[string]string{}
	var unknown, twice []string
	place := func(file, function, where string) {
		writer := file + " " + function
		if _, named := itemRowWriters[file][function]; !named {
			unknown = append(unknown, writer+" ("+where+")")
		}
		if already, repeated := placed[writer]; repeated {
			twice = append(twice, writer+" ("+already+" and "+where+")")
			return
		}
		placed[writer] = where
	}
	for _, row := range itemRowLockTable {
		place(row.file, row.function, "the row "+row.name)
	}
	for file, functions := range itemRowLockNoRow {
		for function := range functions {
			place(file, function, "itemRowLockNoRow")
		}
	}
	slices.Sort(unknown)
	if len(unknown) > 0 {
		t.Errorf("the lock table or its gap map names a writer the census does not (itemRowWriters):\n\t%s\n"+
			"A writer is spelled there as the census spells it, the path within the module and the "+
			"function. Rename it on both sides, or take it out of both.", strings.Join(unknown, "\n\t"))
	}
	slices.Sort(twice)
	if len(twice) > 0 {
		t.Errorf("a writer is both driven by a row and excused from having one:\n\t%s\n"+
			"Each writer is placed once: drive it, or say why it has no row.", strings.Join(twice, "\n\t"))
	}
	var unplaced []string
	for file, functions := range itemRowWriters {
		for function := range functions {
			if _, found := placed[file+" "+function]; !found {
				unplaced = append(unplaced, file+" "+function)
			}
		}
	}
	slices.Sort(unplaced)
	if len(unplaced) > 0 {
		t.Errorf("a writer of an item row is on no row of the lock table and in no gap of it:\n\t%s\n"+
			"A writer a server runs gets a row of its own in itemRowLockTable, which holds it behind "+
			"the owner lock of the row it writes. One that cannot have a row yet goes in "+
			"itemRowLockNoRow with why - the fixture its row would cost, or the caller whose "+
			"transaction it runs in - so that what the lock table leaves out is read off its gap map "+
			"rather than counted against the census.", strings.Join(unplaced, "\n\t"))
	}
}

// writesAnItemRow reports whether one normalised statement writes an item row by name, or writes
// a table this scan cannot read, which may be one.
func writesAnItemRow(statement string) bool {
	return itemRowWrite.MatchString(statement) || formattedTableWrite.MatchString(statement)
}

// censusEntry is the entry of itemRowWriters that covers function, declared in the file at path,
// as the file and function keys that found it; the file is empty when no entry does. The census
// is keyed by the path within the module, so a file is matched by its suffix, as the lock-level
// guard's allowlist matches one (writePhaseOwnerRowLock).
func censusEntry(path, function string) (string, string) {
	for file, functions := range itemRowWriters {
		if !strings.HasSuffix(path, "/"+file) {
			continue
		}
		if _, listed := functions[function]; listed {
			return file, function
		}
	}
	return "", ""
}

// unspelledTableWrites is every write of this file whose table a runtime value fills: a `+` chain
// whose left side ends in `insert into`, `update` or `delete from` and whose next operand is one
// literalText cannot spell. The assembled statement holds no table word for such a write - the
// operand renders as a gap - so the shape is read off the expression, and the text reported is
// the left side that names the keyword. Unlike sqlStatementsByFunction this walks into a
// statement it has found: a write of this shape is a `+` chain inside the statement's own chain.
func unspelledTableWrites(file *ast.File, constants map[string]string) []locatedStatement {
	var writes []locatedStatement
	for _, declared := range file.Decls {
		function := ""
		if fn, ok := declared.(*ast.FuncDecl); ok {
			function = fn.Name.Name
		}
		ast.Inspect(declared, func(node ast.Node) bool {
			chain, ok := node.(*ast.BinaryExpr)
			if !ok || chain.Op != token.ADD {
				return true
			}
			left, leftOK := literalText(chain.X, file.Name.Name, constants)
			if !leftOK {
				return true
			}
			left = sqlWhitespace.ReplaceAllString(strings.ToLower(left), " ")
			if !writeKeywordTail.MatchString(left) {
				return true
			}
			if _, rightOK := literalText(chain.Y, file.Name.Name, constants); rightOK {
				return true
			}
			// The table the next operand fills is unreadable, so the write is reported as the
			// keyword it ends on, with what stands where its table should be.
			writes = append(writes, locatedStatement{
				function: function, statement: left + "<a table this scan cannot read>",
			})
			return true
		})
	}
	return writes
}
