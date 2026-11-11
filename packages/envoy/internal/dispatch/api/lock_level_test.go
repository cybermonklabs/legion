//go:build !race

// The guard is static: it reads the module's source, so the race detector adds nothing to it,
// and leaving it out of the -race run spares that job compiling the guard and running its scan
// over every .go file of packages/envoy a second time.
//
// The tag is here for that saving rather than for the compiler. What this guard and the five
// others share - moduleSource, moduleSources, stringConstants, sqlStatements, literalText,
// sqlWhitespace - is declared in module_source_test.go, which carries no tag because one test
// main added to this package uses a declaration of it untagged and so must have it under -race
// too (ask_asked_of_test.go:480, which normalises a migration's SQL with sqlWhitespace).
// Declarations alone compile wherever nothing calls them, so keeping them there costs the race
// build one file's compile and no walk: every caller of moduleSources is behind this tag, so
// under the detector that walk has no caller at all. The work stays behind the tag: the
// constant fixpoint and the statement walk this guard runs over the whole module, and
// TestNoForUpdateOnOwnerTables, which would otherwise run twice, once under the detector and
// once in the text-check run that names the twelve tests of these six guards and
// one_expression_guard_test.go. That file is where the module's second walk went: main had it
// as a subtest of who_waits_test.go, parsing the same tree in the race binary, and its subject
// is the module's text, which R15 of dispatch://INFRA-1066 has run in the text-check job rather
// than inside a product suite - so it is on this side of the tag now.

package api

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ownerLockTables are the tables the room lock's rule covers: the two owner rows of a document,
// the project they belong to, and asks, a foreign-key parent a settlement holds across the room
// lock. See lockDocumentRoom in internal/dispatch/docs/persistence.go.
var ownerLockTables = []string{"issues", "artifacts", "projects", "asks"}

var (
	forUpdateClause = regexp.MustCompile(`\bfor update\b`)
	ownerLockTable  = regexp.MustCompile(`\b(?:` + strings.Join(ownerLockTables, "|") + `)\b`)
)

// TestNoForUpdateOnOwnerTables enforces the prohibition the room lock's rule states: no statement
// in this module locks issues, artifacts, projects or asks `for update`. It is the prohibition
// only. A weaker level on those tables is legal and deliberate - `for no key update` where a
// writer must serialise against other writers of the same row, `for key share` on an artifact
// whose anchors are being reconciled (api/anchors.go), `for share` on the asks a table edit
// checks (docs/mutation.go) - and none of them blocks the key share a foreign key takes. Only
// `for update` does, and under it a durable writer holding the room lock deadlocks against
// whoever holds the owner row.
//
// Its reach is the statement as this scan can assemble it: a `for update` spelled in a string
// literal, or in a chain of literals and package-level constants, `var`s included, resolved to
// a fixpoint so a constant written from another constant carries the inner text too. That is
// how every owner-table lock in this module is written, seventeen statements in all - eleven a
// plain `select ... for no key update`, three a plain `for key share` or `for share` at the
// weaker levels named above, and three a row shape welded in from constants at the ask reads:
// api/asks.go's `select `+askRowColumns+askRowFrom+` where a.id = $1 for no key update of a`,
// whose askRowColumns is docs.AskColumns plus its own columns, docs/ask_blocks.go's
// `select `+AskColumns, and docs/approval_ask.go's `select `+AskColumns+` ... for no key update`.
//
// An operand the scan cannot resolve - a call, a runtime value - contributes its own string
// literals instead, joined by a space and padded at both ends. A clause those literals spell is
// refused even when no one of them spells it whole: strings.Join([]string{"for", "update"}, " ")
// reds. The padding is a space rather than a boundary, so a clause that forms across the splice
// reds too: "select key from issues for" + strings.Join([]string{"update"}, "") does. A keyword
// split mid-word never forms, so strings.Repeat("for up", 1) + "date" is outside that reach, and
// keeping such a site off `for update` is a review matter rather than a guarantee this check
// makes.
//
// `for update` elsewhere is untouched: doc_checkpoints, comments, message_deliveries,
// comment_mentions, comment_deliveries, user_issue_state and architecture_sources all use it. A
// targeted message's own row is taken `for no key update` (message_delivery.go), so the session's
// reply, whose foreign key takes it `for key share`, never waits on it.
//
// The match is statement-scoped and conservative, and it prints the statement rather than naming
// a table, because which row a statement locks is a property of the whole statement. `for update`
// with no `of` locks rows in every table of the FROM, so an owner table reached through a join is
// really locked and that red is a true positive; Postgres refuses `for update` on the nullable
// side of an outer join, so `left join artifacts ... for update` cannot exist in working code.
// Two shapes can red without a real offence, and each has its own tell in the printed statement.
// `for update of <alias>` where the alias names a table outside this rule and an owner table is
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
	var offences []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		for _, statement := range sqlStatements(source.file, constants) {
			if !locksOwnerTableForUpdate(statement) {
				continue
			}
			offence := filepath.ToSlash(source.path) + ": " + statement
			if _, repeat := seen[offence]; repeat {
				continue
			}
			seen[offence] = struct{}{}
			offences = append(offences, offence)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("`for update` is refused on %s:\n\t%s\n"+
			"Take a weaker level - unless this transaction deletes the row or changes a key "+
			"column, which the weaker levels do not cover: then revisit the rule at lockDocumentRoom.",
			strings.Join(ownerLockTables, ", "), strings.Join(offences, "\n\t"))
	}
}

// locksOwnerTableForUpdate reports whether one normalised statement locks `for update` with an
// in-scope table in it. It matches within the whole statement rather than one line or one
// literal: the SQL is written across several lines of a raw string, and a row shape is often
// concatenated from constants, so the clause and its table routinely sit apart.
func locksOwnerTableForUpdate(statement string) bool {
	return forUpdateClause.MatchString(statement) && ownerLockTable.MatchString(statement)
}
