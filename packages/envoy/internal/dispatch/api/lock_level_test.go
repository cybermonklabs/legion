//go:build !race

// The guard is static: it reads the module's source, so the race detector adds nothing to it,
// and leaving it out of the -race run spares that job compiling the guard and running its scan
// over every .go file of packages/envoy a second time.
//
// The tag is here for that saving rather than for the compiler. What this guard and the eight
// others share - moduleSource, moduleSources, stringConstants, sqlStatements, sqlStatementsIn,
// literalText, sqlWhitespace - is declared in module_source_test.go, which carries no tag because
// tests of this package use declarations of it untagged and so must have them under -race too
// (ask_asked_of_test.go:480, which normalises a migration's SQL with sqlWhitespace, and the
// item-writer census, which walks the module). Declarations alone compile wherever nothing calls
// them, so keeping them there costs the race build one file's compile and, of the walk, only what
// the census asks of it (module_source_test.go's header says why it is the one caller left in).
// The work stays behind the tag: the constant fixpoint and the statement walk this guard runs
// over the whole module, and
// TestNoForUpdateOnOwnerTables, which would otherwise run twice, once under the detector and
// once in the text-check run that names the eighteen tests of these nine guards and
// one_expression_guard_test.go. That file is where the module's second walk went: main had it
// as a subtest of who_waits_test.go, parsing the same tree in the race binary, and its subject
// is the module's text, which R15 of dispatch://INFRA-1066 has run in the text-check job rather
// than inside a product suite - so it is on this side of the tag now.

package api

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ownerLockTables are the tables the room lock's rule covers: the two owner rows of a document,
// the project they belong to, and asks, a foreign-key parent a settlement holds across the room
// lock. See lockDocumentRoom in internal/dispatch/docs/persistence.go.
var ownerLockTables = []string{"issues", "artifacts", "projects", "asks"}

// ownerRowTables are the tables whose rows are owners (store.LockOwner): an issue, a document no
// issue owns, a project. A writer serialises on the owner's advisory lock, and the only row locks
// on these tables are the write phase's own and the one the owner lock takes beside its advisory
// lock while a process of an earlier build, which serialises on the row, is live.
var ownerRowTables = []string{"issues", "artifacts", "projects"}

// ownerRowLockSites are the places allowed to lock an owner row, keyed by the file that declares
// them, each named as a statement's locatedStatement names where it is: a function, or a
// package-level value. They are the events' lockEventOwner, which takes the row `for no key
// update` when the transaction appends, under the owner's advisory lock it takes first; and the
// store's three owner lock levels, the package-level variables ownerLockExclusive,
// ownerLockShared and ownerLockTry, whose row locks are the statements the owner lock runs right
// after its advisory lock, before the read it guards, while its fleet read answers that a process
// of an earlier build is live (store.lockOwnerRows). Every other declaration and function of
// ownerlock.go is under the rule.
var ownerRowLockSites = map[string][]string{
	"internal/dispatch/events/broker.go":   {"lockEventOwner"},
	"internal/dispatch/store/ownerlock.go": {"ownerLockExclusive", "ownerLockShared", "ownerLockTry"},
}

var (
	forUpdateClause = regexp.MustCompile(`\bfor update\b`)
	rowLockClause   = regexp.MustCompile(`\bfor (?:update|no key update|share|key share)\b`)
	ownerLockTable  = regexp.MustCompile(`\b(?:` + strings.Join(ownerLockTables, "|") + `)\b`)
	ownerRowTable   = regexp.MustCompile(`\b(?:` + strings.Join(ownerRowTables, "|") + `)\b`)
)

// TestNoForUpdateOnOwnerTables enforces two prohibitions over every statement of this module.
//
// The first is the one the room lock's rule states: no statement locks issues, artifacts,
// projects or asks `for update`. Under it a foreign key's `for key share` waits, and a durable
// writer holding the room lock then deadlocks against whoever holds the owner row.
//
// The second is the owner lock's: no statement takes any row lock - `for update`, `for no key
// update`, `for share` or `for key share` - on issues, artifacts or projects, outside the owner-row
// locks the owner lock's own code takes (ownerRowLockSites): the write phase's, and the one the
// read phase takes beside the advisory lock while a process of an earlier build is live. A writer
// serialises on its owner's advisory lock (store.LockOwner), which assigns no transaction id; a
// row lock does, so one taken in a transaction's read phase would hold that id across everything
// the read phase does, and an owner row locked by a writer that skips the advisory lock no longer
// excludes the read phase of one that takes it. A missed owner-row lock, or a new one, reds here.
// Asks are outside the second rule, which names issues, artifacts and projects alone, so a weaker
// level on an ask row would pass this check; none is taken now, since the ask rows a settlement
// reconciles are read plainly under the document owner's lock (docs/ask_blocks.go's loadAskBlocks).
//
// Its reach is the statement as this scan can assemble it: a lock clause spelled in a string
// literal, or in a chain of literals and package-level constants, `var`s included, resolved to
// a fixpoint so a constant written from another constant carries the inner text too. That is
// how the owner-table locks left in this module are written - lockEventOwner's three plain
// `select ... for no key update` - and the chain is what would reach a lock welded into a
// statement from a row shape of its own, as docs/ask_blocks.go's `select `+AskColumns+` ...`
// is assembled.
//
// An operand the scan cannot resolve - a call, a runtime value - contributes its own string
// literals instead, joined by a space and padded at both ends. A clause those literals spell is
// refused even when no one of them spells it whole: strings.Join([]string{"for", "update"}, " ")
// reds. The padding is a space rather than a boundary, so a clause that forms across the splice
// reds too: "select key from issues for" + strings.Join([]string{"update"}, "") does. A keyword
// split mid-word never forms, so strings.Repeat("for up", 1) + "date" is outside that reach, and
// keeping such a site off a row lock is a review matter rather than a guarantee this check
// makes.
//
// Row locks elsewhere are untouched: doc_checkpoints is read plainly under the document lock, and
// message_deliveries, comment_mentions, comment_deliveries, repo_projects and
// architecture_sources still use `for update`. A targeted message's own row is taken `for no key
// update` (message_delivery.go), so the session's reply, whose foreign key takes it `for key
// share`, never waits on it. The viewer's own state row takes none: its route serialises on the
// project's advisory owner lock (state.go, store.LockOwner).
//
// The match is statement-scoped and conservative, and it prints the statement rather than naming
// a table, because which row a statement locks is a property of the whole statement. A lock
// clause with no `of` locks rows in every table of the FROM, so an owner table reached through a
// join is really locked and that red is a true positive; Postgres refuses a lock on the nullable
// side of an outer join, so `left join artifacts ... for update` cannot exist in working code.
// Two shapes can red without a real offence, and each has its own tell in the printed statement.
// A lock `of <alias>` where the alias names a table outside this rule and an owner table is
// merely joined beside it: the tell is the `of <alias>` the statement carries, which names the
// row the lock really takes. A statement concatenated with an expression this scan cannot
// resolve, whose own string literals are folded in - a literal that never reaches the query can
// put an owner table's name into the scanned text, as `... doc_checkpoints ... for update` +
// strings.Repeat("issues", 0) prints `... for update issues`: the tell is a word the statement
// did not spell, an owner table outside its `from` or a lock clause the query never assembles.
func TestNoForUpdateOnOwnerTables(t *testing.T) {
	sources := moduleSources(t)
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var forUpdates, rowLocks []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, located := range sqlStatementsByFunction(source.file, constants) {
			offence := statementAt(path, located)
			if _, repeat := seen[offence]; repeat {
				continue
			}
			if locksOwnerTableForUpdate(located.statement) {
				seen[offence] = struct{}{}
				forUpdates = append(forUpdates, offence)
				continue
			}
			if locksOwnerRow(located.statement) && !ownerRowLockSite(path, located) {
				seen[offence] = struct{}{}
				rowLocks = append(rowLocks, offence)
			}
		}
	}
	if len(forUpdates) > 0 {
		t.Errorf("`for update` is refused on %s:\n\t%s\n"+
			"Take a weaker level - unless this transaction deletes the row or changes a key "+
			"column, which the weaker levels do not cover: then revisit the rule at lockDocumentRoom.",
			strings.Join(ownerLockTables, ", "), strings.Join(forUpdates, "\n\t"))
	}
	if len(rowLocks) > 0 {
		t.Errorf("a row lock on %s is refused outside the owner lock's own owner-row locks (events.lockEventOwner; store.ownerLockExclusive, ownerLockShared and ownerLockTry, the owner lock levels):\n\t%s\n"+
			"Take the owner's advisory lock (store.LockOwner, LockOwners, LockOwnersShared or TryLockOwners) "+
			"and read the row plainly in the statement after it.",
			strings.Join(ownerRowTables, ", "), strings.Join(rowLocks, "\n\t"))
	}
}

// locksOwnerTableForUpdate reports whether one normalised statement locks `for update` with an
// in-scope table in it. It matches within the whole statement rather than one line or one
// literal: the SQL is written across several lines of a raw string, and a row shape is often
// concatenated from constants, so the clause and its table routinely sit apart.
func locksOwnerTableForUpdate(statement string) bool {
	return forUpdateClause.MatchString(statement) && ownerLockTable.MatchString(statement)
}

// locksOwnerRow reports whether one normalised statement takes a row lock of any level with an
// owner table in it, matched as locksOwnerTableForUpdate matches.
func locksOwnerRow(statement string) bool {
	return rowLockClause.MatchString(statement) && ownerRowTable.MatchString(statement)
}

// ownerRowLockSite reports whether the statement located, declared in the file at path, is in a
// function or a package-level value the owner-row rule allows (ownerRowLockSites).
func ownerRowLockSite(path string, located locatedStatement) bool {
	for file, sites := range ownerRowLockSites {
		if !strings.HasSuffix(path, "/"+file) {
			continue
		}
		if located.function != "" && slices.Contains(sites, located.function) ||
			located.value != "" && slices.Contains(sites, located.value) {
			return true
		}
	}
	return false
}

// statementAt names a statement as a failure prints it: its file, the function or package-level
// value it is declared in when it is in one, and its text.
func statementAt(path string, located locatedStatement) string {
	switch {
	case located.function != "":
		path += " " + located.function
	case located.value != "":
		path += " " + located.value
	}
	return path + ": " + located.statement
}
