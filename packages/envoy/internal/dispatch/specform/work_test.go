package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// workScale is how many times the larger text of a row of the work table is the smaller one, and
// workBound how many times the count of its judgment's work may grow. A count of work in
// proportion to the text grows about workScale times, less while a fixed cost counts; n log n work
// grows 16·log(16n)/log(n) times, 27.3 at 50 units; and work for each pair of units grows
// workScale² = 256 times, so a walk for each pair breaks the bound once its count is a fourteenth
// of the count of the rest of the smaller text's judgment ((32 - 16) / (256 - 32)). Counted at the
// table's n on 2026-11-28, the rows grew 12.72 times ("many addresses of a person in one
// sentence", of 12 units, a fifth of whose smaller count is a fixed cost) to 16.72 ("one statement
// that refers to many footnotes"), and the count of a correct judgment reads the same in each run.
// Each walk planted as a walk for each pair in code of a counted package failed each row that it
// is for, by 46.5 times or more: ste-lint's count of a column written as a loop of ste 241.4 to
// 248.6 times on three of its four rows, and the fourth by its counter's wrap (workVerdict); the
// list of distinct addresses and titles read through for each one 84.1 to 98.0; a footnote's count
// of sentences that does not stop once it is full 46.5, at the row's n of 750; and the
// instructions rule's split read again from each instruction to the end of the sentence 172.3 to
// 173.7. 32 is about twice the most that a row grew, more than n log n work grows at each n of
// the table but the least, and two thirds of the least that a planted walk grew.
const (
	workScale = 16
	workBound = 32
)

// workJudgment builds the text of n units, which is not the judgment's work, and returns the
// judgment of it.
type workJudgment func(t *testing.T, n int) func() []Finding

// workRow is one row of the work table: the shape of text that grows, the rule whose finding the
// judgment of each size gives ("" for a shape that breaks no rule), the n of its smaller text, and
// its judgment.
type workRow struct {
	name  string
	rule  Rule
	n     int
	judge workJudgment
}

// workRaceShrink is how many times smaller each row's n is under the race detector (raceEnabled).
// The detector's instrumentation makes the judgment that this build runs to check each row's
// findings 10 to 20 times the work, and at the plain build's n the table took 512 s under -race on
// 2 CPUs, against about 140 s plain. The count of a judgment is the same with the detector or
// without it, so the race build holds each row to the same verdict and bound (workVerdict) at its
// smaller n. The plain build is the one whose n each planted walk was measured at (workBound): at
// a smaller n a walk for each pair adds less to the count beside the rest of the judgment, so the
// plain build is the one that holds the walks.
const workRaceShrink = 12

// workRows is the work table in a build: each row of workTable at its n, or under the race
// detector (race) at its n over workRaceShrink, which is the one difference of the two builds.
func workRows(race bool) []workRow {
	rows := workTable()
	if race {
		for index := range rows {
			rows[index].n = max(1, rows[index].n/workRaceShrink)
		}
	}
	return rows
}

// A judgment does work in proportion to its text, for each rule of the form and each shape that
// grows the text: many forms of one rule on one line, one long line, many short lines, many links,
// addresses and titles, many terms and many texts, and each part of a document that many texts
// share. The one part of a document that the reader lets many texts share is a footnote: many
// statements, term lines, component rows and purpose lines that refer to one long footnote, to one
// footnote of many links, or to one chain of footnotes, many claims that refer to one long footnote
// of two verbs for one action, which is one finding of the footnote, one statement that refers to a
// chain of footnotes, and one statement that refers to many footnotes, also built with no store at
// a size that no store holds, where the count of its sentences stops once it passes two. The
// footnotes of the chains hold a verb of a synonym group, which Dispatch carries into each text
// that holds them, as it carries their sentences. The reader refuses a link definition, which many
// links could share, and a link and an image hold their own address and title. Each row judges a
// text of n units and one of workScale times n, and the count of the work of the larger grows
// less than workBound times the count of the smaller (workVerdict). The count is of the statements
// that the judgment executes in this package and in every package outside the standard library
// that it imports (countedPackages: ste, the reader pmdoc, goldmark and its extensions, the
// server's reference grammar text, golang.org/x/text and the rest), each counted by the coverage
// counter of its block in a build of this package's tests (counted_work_test.go), less the
// statements of a run that builds the text and does not judge it: building the text, an extracted
// row's markdown among it, is not the judgment's work, and reading that markdown is. No load of
// the machine, no collector and no race detector moves the count, so each text is judged once here
// and once in the counted build. The race build judges each row at a smaller n (workRaceShrink),
// to the same verdict and bound. A row whose judgment finds no finding of its rule at either size
// reads nothing, so it fails; a row of a rule "" is a shape that breaks no rule. A row's n, the n
// of the plain build, is the n it had when this table read CPU time: the larger of twice the least
// n at which the walk that the row is for, made a walk for each pair, still failed that table's
// bound, and the n at which judging the row's smaller text 16 times took about 110 ms on a machine
// of 288 cores. The one row whose n is new is that of many statements that refer to one chain of
// footnotes: its n is about twice the least n at which its walk fails this table's bound
// (footnoteJudge.carry, below).
//
// The counters see each turn of every loop of the counted packages, and a call back into one (a
// strings.ContainsFunc of a function of this package counts each character it reads), but no work
// inside a call into the standard library, which counts as the one statement that calls it: a
// regular expression's search, strings.Index and strings.Count, utf8.RuneCountInString,
// slices.Contains, and the runtime's copies and maps. So the count sees a walk for each pair where
// the pass that the walk repeats is code of a counted package, and sees one statement a step where
// that pass is a call into the standard library. The fifteen walks that the rows are for, each
// broken into a walk for each pair once when this table read CPU time, to show that its rows
// failed, and four of them again for the count (workBound), are these, each with the code that
// holds it:
//
//   - ste-lint's count of a column from the start of the line (many semicolons, many phrasal verbs,
//     many marketing adjectives, many nominalizations): ste's columns.at, whose count is runes,
//     which is utf8.RuneCountInString, inside the standard library. Counted from the start of the
//     line through runes, the walk is one statement a finding, and these rows do not see it; they
//     hold the rest of ste's work on the line, its scans, its matches and its findings, where a
//     count of the column written as a loop of ste fails them. ste's
//     TestEachRuleThatWalksMatchesCountsEachCharacterOfALineOnce counts the characters that runes
//     reads, for the same four rules, so it holds that walk by a count.
//   - the list of distinct addresses and titles, read through for each one (many links, many
//     titles, many images): firstOfEach, of this package.
//   - the texts already kept for a node, read through for each new one (many purpose lines of one
//     id): extraction.add, of this package.
//   - the links found, read through for each key (many references, many keys): referenceForms, of
//     this package.
//   - the brackets before each closing bracket of a reference, counted again (one reference before
//     many closing brackets): text's trimReference, which counts the brackets once with
//     strings.Count, inside the standard library, and walks back over the closers in a loop of its
//     own. Counted again for each closer through strings.Count, the walk is one statement a
//     closer, and the row does not see it; it holds the walk back and the rest of the judgment of
//     the reference. text's TestExtractTrimsALongClosingRunInLinearTime holds that walk, on a run
//     of a mebibyte, to 3 s of wall time.
//   - a word's letters before each stop, read again (one word of many stops): joinedStops, of this
//     package.
//   - the names found, read through for each handle and each name (many handles, many names of a
//     person): personNames, of this package.
//   - the pull requests found, read through for each form (many numbers of a pull request, many
//     pull requests of a repository): referenceForms, of this package.
//   - a footnote's count of sentences that does not stop once it is full (many statements that
//     refer to one chain of footnotes): footnoteJudge.carry, of this package, whose check of the
//     footnotes that a count already holds is slices.Contains, inside the standard library. The
//     counters see three statements a step of that walk, not the footnotes that slices.Contains
//     reads, which were most of its CPU time: at the 130 statements the row had when this table
//     read CPU time, the walk grew the row's count 22.3 times. The walk adds about 3n² statements
//     to the row's 16,000 or so a statement, so it fails the bound from about n = 372 on, and the
//     row's n is 750, where it grew the count 46.5 times.
//   - goldmark's own footnote reference parser, which reads every definition for each reference
//     (many footnotes, one statement that refers to many footnotes): a loop of goldmark's
//     extension package, in place of which pmdoc's footnoteReferenceParser reads maps; both are
//     counted.
//   - the record of the footnotes a text was given, made new for each text (many statements that
//     refer to one long footnote, to one footnote of many links, or to one chain of footnotes):
//     footnoteJudge.walk, of this package.
//   - the instructions rule's read of a sentence's words again at each word (one long procedure
//     sentence, one sentence of many spans of code): sentenceWords and isInstruction, of this
//     package.
//   - its split of a sentence into one sentence for each instruction, read again from each
//     instruction to the end of the sentence (one sentence of many instructions, also after a
//     condition inside its first instruction, and after the condition that opens it):
//     instructionBreaks and eachInstruction, of this package.
//   - its reading of the conditions of a sentence, read again from each word (one sentence of many
//     instructions, also after a condition inside its first instruction), and of the condition
//     that opens a sentence, read again from each comma (one sentence of many instructions after
//     the condition that opens it): instructionStart and opensInstruction, of this package.
//     instructionStart finds the comma that ends an opening condition with slices.Index, inside
//     the standard library, so a search for that comma made again from each word would be one
//     statement a word.
//
// A walk for each pair whose step is a statement or two needs units whose judgment counts few
// statements else: so the footnotes of the rows "many footnotes" and "one statement that refers to
// many footnotes", and the statements of the first, hold a word or two each, and the row of
// purpose lines of one id counts the reader alone (read), since the walk that it is for is the
// reader's, and the judgment of each line counts several times the statements of its reading.
func workTable() []workRow {
	context := planted("ada.lovelace@example.com")
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	// checked judges one text of kind: before, n units, then after.
	checked := func(kind Kind, before, unit, after string) workJudgment {
		return func(_ *testing.T, n int) func() []Finding {
			in := Input{Texts: []Text{{Node: "n", Kind: kind, Body: before + strings.Repeat(unit, n) + after, Where: where}}}
			return func() []Finding { return check(context, in) }
		}
	}
	// extracted reads the document that markdown writes for n units, and judges it.
	extracted := func(markdown func(n int) string) workJudgment {
		return func(t *testing.T, n int) func() []Finding {
			document := markdown(n)
			return func() []Finding {
				read, err := ExtractDocument("spec.md", 1, document)
				if err != nil {
					t.Fatalf("read %d units: %v", n, err)
				}
				return check(context, Input{Texts: read.Texts, Proposed: read.Terms})
			}
		}
	}
	// read reads the document that markdown writes for n units, and judges none of its texts: a row
	// for a walk of the reader, which the judgment of each text, several times the work of its
	// reading, would hide. It fails unless the reader keeps the n texts as n texts of kind under one
	// node.
	read := func(node string, kind Kind, markdown func(n int) string) workJudgment {
		return func(t *testing.T, n int) func() []Finding {
			document := markdown(n)
			reading := func() Extracted {
				kept, err := ExtractDocument("spec.md", 1, document)
				if err != nil {
					t.Fatalf("read %d units: %v", n, err)
				}
				return kept
			}
			kept, last, want := reading(), "", fmt.Sprintf("%s (%d)", node, n)
			if len(kept.Texts) > 0 {
				last = kept.Texts[len(kept.Texts)-1].Node
			}
			if kept.Counts[kind] != n || len(kept.Texts) != n || last != want {
				t.Fatalf("the reader keeps %d texts of %d units, %d of kind %s, the last of node %q, want %d under one node, the last %q",
					len(kept.Texts), n, kept.Counts[kind], kind, last, n, want)
			}
			return func() []Finding {
				reading()
				return nil
			}
		}
	}
	// units is before, n units, each written with its number, and after.
	units := func(before, unit, after string) func(n int) string {
		return func(n int) string {
			var markdown strings.Builder
			markdown.WriteString(before)
			for index := range n {
				fmt.Fprintf(&markdown, unit, index+1)
			}
			markdown.WriteString(after)
			return markdown.String()
		}
	}
	// shared is one footnote of n units, each written with its number, and n units that refer to
	// it, each written with its number, after head.
	shared := func(note, head, unit string) func(n int) string {
		return func(n int) string {
			return units("[^shared]: ", note, "\n\n")(n) + units(head, unit, "")(n)
		}
	}
	// chain is n footnotes, each of which refers to the next, and each of which holds a verb of a
	// synonym group.
	chain := func(n int) string {
		var markdown strings.Builder
		for index := 1; index <= n; index++ {
			fmt.Fprintf(&markdown, "[^%d]: Dispatch checks record %[1]d[^%d].\n\n", index, index+1)
		}
		return markdown.String()
	}
	// noted is one statement that refers to n footnotes of one short sentence each, which a caller
	// that reads no store builds, such as a check of the files of a tree: no limit of the store
	// holds n. The count of the statement's sentences stops once it passes maxSentences, so the
	// work of the count does not grow with the footnotes that it has passed.
	noted := func(_ *testing.T, n int) func() []Finding {
		notes := make([]*Footnote, n)
		for index := range notes {
			notes[index] = &Footnote{Label: fmt.Sprint(index + 1), Body: "It."}
		}
		in := Input{Texts: []Text{{Node: "s", Kind: KindStatement, Body: "Dispatch keeps it.", Where: where, Notes: notes}}}
		return func() []Finding { return check(context, in) }
	}
	// procedureCheck is one check block whose procedure (its `run`) is run.
	procedureCheck := func(run string) string {
		return fmt.Sprintf(":::check{#v run=%q}\nDispatch keeps each record.\n:::\n", run)
	}
	const requirement, end = ":::requirement{#r}\nDispatch keeps", "\n:::\n"
	const statementUnit = ":::requirement{#s-%d}\nDispatch keeps record %[1]d[^shared].\n:::\n\n"
	return []workRow{
		{"many sentences on one line", ruleSentences, 270, checked(KindStatement, "", "Dispatch keeps it. ", "")},
		{"many short lines", ruleSentences, 280, checked(KindStatement, "", "Dispatch keeps it.\n", "")},
		{"one long sentence", ruleSentenceWords, 1_100, checked(KindStatement, "Dispatch keeps ", "word ", "it.")},
		{"many stops with no space after them", ruleStopNoSpace, 1_200, checked(KindClaim, "Dispatch reads ", "a.B ", "now.")},
		{"many stops with no space in one sentence, each in a word of its own", ruleStopNoSpace, 50,
			extracted(units(requirement, " a%d.B", end))},
		{"many addresses of a person in one sentence", rulePerson, 12, extracted(units(requirement, " a%d@example.com", end))},
		{"one word of many stops", "", 5_900, checked(KindClaim, "Dispatch reads ", ".", "A now.")},
		{"many semicolons", "ste100-semicolon", 4_100, checked(KindClaim, "Dispatch keeps it", ";", " and waits.")},
		{"many phrasal verbs", "ste100-phrasal-verb", 1_500, checked(KindClaim, "Legion will ", "spin up ", "the job.")},
		{"many marketing adjectives", "ste100-marketing-adjective", 1_500, checked(KindClaim, "Dispatch keeps a ", "seamless ", "log.")},
		{"many nominalizations", "ste100-nominalization", 1_700, checked(KindClaim, "Dispatch ", "performs a validation ", "of each upload.")},
		{"many synonyms of one verb", "ste100-synonym-rotation", 110, checked(KindClaim, "", "Check the config file. Then verify the output. ", "")},
		{"one list item that ends with and", "ste100-dangling-conjunction", 470, checked(KindClaim, "- Dispatch keeps ", "the record ", "and")},
		{"many references", ruleDispatchLink, 2_800, checked(KindName, "", "dispatch://ACME-1 ", "")},
		{"many keys", ruleDispatchLink, 2_000, checked(KindName, "", "ACME-1 ", "")},
		{"many ids", ruleDispatchLink, 340, checked(KindName, "", id+" ", "")},
		{"one reference before many closing brackets", ruleDispatchLink, 16_000, checked(KindName, "dispatch://ACME-1", ")", "")},
		{"many incidents", ruleIncident, 1_200, checked(KindName, "", "INC-1 ", "")},
		{"many numbers of a pull request", rulePullRequest, 2_900, checked(KindName, "", "#1 ", "")},
		{"many pull requests of a repository", rulePullRequest, 2_800, checked(KindName, "", "acme/widgets#1 ", "")},
		{"many addresses of a pull request", rulePullRequest, 420, checked(KindName, "", "github.com/acme/widgets/pull/1 ", "")},
		{"many email addresses", rulePerson, 670, checked(KindName, "", "ada@example.com ", "")},
		{"many handles", rulePerson, 2_000, checked(KindName, "", "@ada ", "")},
		{"many names of a person", rulePerson, 1_000, checked(KindName, "", "Ada Lovelace ", "")},
		{"many links", ruleDispatchLink, 2_000, extracted(units(requirement, " [it](dispatch://ACME-%d)", end))},
		{"many titles", ruleDispatchLink, 2_800, extracted(units(requirement, ` [it](https://example.com "ACME-%d")`, end))},
		{"many images", ruleDispatchLink, 2_000, extracted(units(requirement, " ![it](dispatch://ACME-%d)", end))},
		{"many terms", "ste100-semicolon", 120, extracted(units("# Terms\n\n", "- **term %d**: Dispatch keeps it; it waits.\n", ""))},
		{"many statements", "ste100-semicolon", 140, extracted(units("", ":::requirement{#s-%d}\nDispatch keeps it; it waits.\n:::\n\n", ""))},
		{"many purpose lines of one id", "", 1_400, read("x", KindPurpose, units("", "Purpose of x: Dispatch keeps record %d; it waits.\n\n", ""))},
		{"many footnotes", "ste100-semicolon", 1_400, extracted(units("", "[^%[1]d]: It; it.\n\n:::requirement{#s-%[1]d}\nIt[^%[1]d].\n:::\n\n", ""))},
		{"many statements that refer to one long footnote", "ste100-semicolon", 70,
			extracted(shared("Dispatch keeps record %d; it waits. ", "", statementUnit))},
		{"many term lines that refer to one long footnote", "", 50,
			extracted(shared("Dispatch keeps the record of answer %d for one year. ", "# Terms\n\n", "- **term %d**: One record of a claim[^shared].\n"))},
		{"many component rows that refer to one long footnote", "", 52,
			extracted(shared("Dispatch keeps the record of answer %d for one year. ", "| Id | Kind | Name | Purpose |\n| --- | --- | --- | --- |\n",
				"| `c%d` | component | Part %[1]d | It keeps one record[^shared]. |\n"))},
		{"many purpose lines that refer to one long footnote", "", 56,
			extracted(shared("Dispatch keeps the record of answer %d for one year. ", "", "Purpose of c%d: It keeps one record[^shared].\n\n"))},
		{"many statements that refer to one footnote of many links", ruleDispatchLink, 120,
			extracted(shared("[it](dispatch://ACME-%d) ", "", statementUnit))},
		{"many statements that refer to one chain of footnotes", ruleSentences, 750,
			extracted(func(n int) string {
				return chain(n) + units("", ":::requirement{#s-%d}\nDispatch keeps it[^1].\n:::\n\n", "")(n)
			})},
		{"many claims that refer to one long footnote of two verbs for one action", "ste100-synonym-rotation", 61,
			extracted(shared("Dispatch checks record %d and confirms it. ", "", ":::check{#c-%d}\nDispatch keeps record %[1]d[^shared].\n:::\n\n"))},
		{"one statement that refers to a chain of footnotes", ruleSentences, 140,
			extracted(func(n int) string { return ":::requirement{#s}\nDispatch keeps it[^1].\n:::\n\n" + chain(n) })},
		{"one statement that refers to many footnotes", ruleSentences, 1_000,
			extracted(func(n int) string {
				return units(":::requirement{#s}\nDispatch keeps it", "[^%d]", ".\n:::\n\n")(n) + units("", "[^%d]: It.\n\n", "")(n)
			})},
		{"one statement that refers to many footnotes, with no store", ruleSentences, 1_800, noted},
		{"one long procedure sentence", ruleSentenceWords, 4_000,
			extracted(func(n int) string { return procedureCheck("Run the test with " + strings.Repeat("word ", n) + "now.") })},
		{"one sentence of many spans of code", ruleInstructions, 1_000,
			extracted(func(n int) string { return procedureCheck("Run " + strings.Repeat("`x` and ", n) + "read the log.") })},
		{"one sentence of many instructions", ruleInstructions, 200,
			extracted(func(n int) string {
				return procedureCheck("Run the test, " + strings.Repeat("read the log, ", n) + "send the report.")
			})},
		{"one sentence of many instructions after a condition", ruleInstructions, 200,
			extracted(func(n int) string {
				return procedureCheck("Read the log before the run ends, " + strings.Repeat("read the log, ", n) + "send the report.")
			})},
		{"one sentence of many instructions after the condition that opens it", ruleInstructions, 200,
			extracted(func(n int) string {
				return procedureCheck("If the run fails, " + strings.Repeat("read the log, ", n) + "send the report.")
			})},
	}
}

// TestTheWorkOfAJudgmentGrowsInProportionToItsText judges each row of the work table (workTable) at
// the n of this build (workRows) and at workScale times it, here, where each judgment must find a
// finding of its row's rule, and in the counted build (countedWork), whose judgment must answer the
// same, and holds the two counts to workBound (workVerdict).
func TestTheWorkOfAJudgmentGrowsInProportionToItsText(t *testing.T) {
	for _, row := range workRows(raceEnabled) {
		t.Run(row.name, func(t *testing.T) {
			sizes := [2]int{row.n, workScale * row.n}
			var counts [2]countedJudgment
			for index, n := range sizes {
				found := row.judge(t, n)()
				if has := slices.ContainsFunc(found, func(finding Finding) bool { return finding.Rule == row.rule }); row.rule != "" && !has {
					t.Fatalf("the judgment of %d units finds no finding of %s, so this row reads nothing: %.300v", n, row.rule, found)
				} else if row.rule == "" && len(found) > 0 {
					t.Fatalf("the judgment of %d units finds %.300v, want none", n, found)
				}
				counts[index] = countedWork(t, row.name, n, findingsAnswer(found))
			}
			if line, ok := workVerdict(sizes, counts); ok {
				t.Log(line)
			} else {
				t.Error(line)
			}
		})
	}
}

// The race build runs the rows of the plain build, each at its n over workRaceShrink, and no
// other: workRows makes both from workTable, and this fails when a row is in one and not the
// other, or when the race build's n of a row is any other.
func TestTheWorkTableRunsEachRowUnderTheRaceDetector(t *testing.T) {
	plain, race := workRows(false), workRows(true)
	names := func(rows []workRow) []string {
		var named []string
		for _, row := range rows {
			named = append(named, row.name)
		}
		return named
	}
	if !slices.Equal(names(plain), names(race)) {
		t.Fatalf("the plain build's rows are %q, and the race build's %q, want the same rows", names(plain), names(race))
	}
	for index, row := range plain {
		if want := max(1, row.n/workRaceShrink); race[index].rule != row.rule || race[index].n != want {
			t.Errorf("row %q: the race build reads rule %q at n %d, want rule %q at n %d (%d over %d)",
				row.name, race[index].rule, race[index].n, row.rule, want, row.n, workRaceShrink)
		}
	}
}

// workVerdict is the verdict on the counts of a row's judgment at its two sizes, without a
// *testing.T to report through: one line of the counts, their ratio and the busiest block of each,
// and whether they pass. They fail where a count reads none, since a growth from nothing says
// nothing and a counter that sees nothing of the judgment passes any growth; where a block of the
// smaller text's run, grown as a walk for each pair grows (workScale²), reaches countedWrap, since
// the larger text's count of it can then be a remainder, which can read as any growth; where the
// larger text's count is below the smaller's, which is a counter that wrapped; and where the
// count grew workBound times or more.
func workVerdict(sizes [2]int, counts [2]countedJudgment) (string, bool) {
	small, large := counts[0], counts[1]
	line := fmt.Sprintf("%d units: %d statements; %d units: %d statements", sizes[0], small.statements, sizes[1], large.statements)
	switch {
	case small.statements == 0 || large.statements == 0:
		return line + ": a count of none, so the counters see nothing of the judgment, and its growth says nothing", false
	case small.busiest*workScale*workScale >= countedWrap:
		return line + fmt.Sprintf(": the block %s ran %d times at %d units, and %d times that, the growth of a walk for each pair, "+
			"reaches 2^32, where its counter wraps, so the count at %d units can be a remainder", small.busiestBlock, small.busiest,
			sizes[0], workScale*workScale, sizes[1]), false
	case large.statements < small.statements:
		return line + ": the larger text's count is below the smaller's, which is a counter that wrapped, not a reading", false
	}
	growth := float64(large.statements) / float64(small.statements)
	line += fmt.Sprintf("; %.2f times (busiest block %d, then %d)", growth, small.busiest, large.busiest)
	if growth >= workBound {
		return line + fmt.Sprintf(", %d times or more: the work grows faster than the text", workBound), false
	}
	return line + fmt.Sprintf(", less than %d", workBound), true
}

// workVerdict passes work in proportion to the text and fails work for each pair of its units, a
// count of none, a block whose counter can wrap at the larger text, and a larger count below the
// smaller: no counted run reads any of the last three on a correct judgment.
func TestTheWorkVerdictFailsAWalkForEachPairACountOfNoneAndACounterThatWraps(t *testing.T) {
	sizes := [2]int{1_000, workScale * 1_000}
	linear := func(n int) countedJudgment {
		return countedJudgment{statements: uint64(400*n + 30_000), busiest: uint64(3 * n)}
	}
	pairs := func(n int) countedJudgment {
		judgment := linear(n)
		judgment.statements += uint64(n * n)
		judgment.busiest = uint64(n * n / 2)
		return judgment
	}
	for _, test := range []struct {
		name   string
		counts [2]countedJudgment
		pass   bool
	}{
		{"work in proportion to the text", [2]countedJudgment{linear(sizes[0]), linear(sizes[1])}, true},
		{"work for each pair of units", [2]countedJudgment{pairs(sizes[0]), pairs(sizes[1])}, false},
		{"a count of none", [2]countedJudgment{{}, linear(sizes[1])}, false},
		{"a block whose counter can wrap", [2]countedJudgment{{statements: 1 << 28, busiest: countedWrap / (workScale * workScale)},
			{statements: 1 << 31}}, false},
		{"a larger count below the smaller", [2]countedJudgment{linear(sizes[1]), linear(sizes[0])}, false},
	} {
		line, pass := workVerdict(sizes, test.counts)
		if pass != test.pass {
			t.Errorf("%s: passed %t, want %t: %s", test.name, pass, test.pass, line)
		}
		t.Logf("%s: %s", test.name, line)
	}
}
