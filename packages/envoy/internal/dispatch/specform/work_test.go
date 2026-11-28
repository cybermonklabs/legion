package specform

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cputest"
)

// workSamples is how many times each text is judged, the sizes in turn, so that a change of the
// machine's load reaches each. A busy machine can only add to the CPU time of a judgment, so the
// least of the samples is its work.
const workSamples = 3

// workScale is how many times the larger text of a row of the work table is the smaller one, and
// workBound how many times its work may grow: work in proportion to the text grows about 16 times,
// n log n work at most about 22 times for the smallest n of the table, and work for each pair of
// units about 256 times. So a walk for each pair breaks the bound once it is a fifteenth of the
// work of the smaller text. Measured on a machine of 288 cores for the table's present sizes: in 10
// plain runs of the table at a load average of 138 to 310 (the race build claims no bound), the
// worst growth was 28.7 (the row "many addresses of a pull request"); and with each walk that a
// row is for made a walk for each pair in turn, the least growth of a broken row was 39.0 (the
// ste-lint walk). With the three rows of the split, 10 more plain runs at a load average of 414
// to 978 gave a worst growth of 22.2 (the row "many claims that refer to one long footnote of two
// verbs for one action"), and each of those rows, a walk that it guards broken, grew 60.5 times
// or more in 5 of 5 runs (the walks below). Those numbers are of the user time of the thread
// alone; the table now reads its user and system time (cputest's thread clock). Read on both over
// the same windows, in 4 plain runs at a load average of 205 to 366, two of them in a cgroup full
// of page cache, one of which took 363 MiB of reclaim on the test's own thread, the worst growth
// was 21.1 to 21.8 on
// user and system time and 20.4 to 21.3 on user time alone.
const (
	workScale = 16
	workBound = 32
)

// workRaceShrink is how many times smaller each row's n is under the race detector, whose
// instrumentation makes the judgment 10 to 20 times the work (16 at the median row): the race
// build runs each row for about the plain build's time, its windows last about as long as the
// plain build's, and it claims no bound.
const workRaceShrink = 12

// workJudgment builds the judgment of a text of n units: the text is built once, before any
// window, and the judgment judges it.
type workJudgment func(t *testing.T, n int) func() []Finding

// workRow is one row of the work table: the shape of text that grows, the rule whose finding the
// judgment of each size gives ("" for a shape that breaks no rule), the n of its smaller text in
// the plain build, and its judgment.
type workRow struct {
	name  string
	rule  Rule
	n     int
	judge workJudgment
}

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
// text of n units and one of workScale times n, and the work of the larger grows less than
// workBound times (Growth): the smaller text is judged workScale times in one window and the
// larger once, so that both windows last about as long. The work is the CPU time, user and system,
// of the thread that judges (Growth, on cputest's thread clock): a wait for a core is not in it,
// nor the collector's mark workers; the kernel's work for the thread, such as the page faults of
// its allocation, is in it, and so is work that allocates nothing. The smaller window of each row
// lasts long enough that the collector's work and a cold cache are a small part of it. A row whose
// judgment finds no finding of its rule at either size reads nothing, so it fails; a row of a rule
// "" is a shape that breaks no rule. A row's n is the larger of two: twice the least n at which the
// walk that the row is for, made a walk for each pair, still failed the bound when its n was
// searched at steps of about 1.41, and the n at which the row's smaller window lasts about 110 ms
// on the machine of 288 cores, so that a machine somewhat faster still reads a window of tens of
// milliseconds. Each of the fifteen walks failed its rows in 5 of 5 runs at the table's n, each
// walk broken in turn: ste-lint's count of a column from the start of the line (many semicolons,
// many phrasal verbs, many marketing adjectives, many nominalizations); the list of distinct
// addresses and titles, read through for each one (many links, many titles, many images); the
// texts already kept for a node,
// read through for each new one (many purpose lines of one id); the links found, read through for
// each key (many references, many keys); the brackets before each closing bracket of a reference,
// counted again (one reference before many closing brackets); a word's letters before each stop,
// read again (one word of many stops); the names found, read through for each handle and each
// name (many handles, many names of a person); the pull requests found, read through for each
// form (many numbers of a pull request, many pull requests of a repository); a footnote's count of
// sentences that does not stop once it is full (many statements that refer to one chain of
// footnotes); goldmark's own footnote reference parser, which reads every definition for each
// reference (many footnotes, one statement that refers to many footnotes); the record of the
// footnotes a text was given, made new for each text (many statements that refer to one long
// footnote, to one footnote of many links, or to one chain of footnotes); the instructions rule's
// read of a sentence's words again at each word (one long procedure sentence, one sentence of many
// spans of code); its split of a sentence into one sentence for each instruction, read again from
// each instruction to the end of the sentence (one sentence of many instructions, also after a
// condition inside its first instruction, the two sentences of many instructions that it splits),
// 84.1 times or more; its reading of the conditions of a sentence whose split could change what it
// means, read again from each word (the same two rows), 60.5 times or more; its reading of the
// condition that opens a sentence as a sentence of its own, read again from each comma (one
// sentence of many instructions after the condition that opens it, which it does not split), 85.1
// times or more. Each of those last three walks was broken in the code that this table reads. A
// row's smaller text can be shorter than 16 KB, which Go's regexp reads a faster way, so that its
// growth is up to about 1.3 times more; the worst growth of the
// table (workScale) holds that. A walk for each pair whose step is a comparison of a few
// nanoseconds needs units that cost little else: so the footnotes of the rows "many footnotes" and
// "one statement that refers to many footnotes", and the statements of the first, hold a word or
// two each, and the row of purpose lines of one id times the reader alone (read), since the walk
// that it is for is the reader's, and the judgment of each line costs several times the reading of
// it.
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
		{"many statements that refer to one chain of footnotes", ruleSentences, 130,
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

// TestTheWorkOfAJudgmentGrowsInProportionToItsText runs each row of the work table (workTable) at
// the n of this build (workRows): under the race detector it runs each row's judgments, their
// findings and their windows, and claims no bound. The power of the table is in the plain build.
func TestTheWorkOfAJudgmentGrowsInProportionToItsText(t *testing.T) {
	for _, row := range workRows(raceEnabled) {
		t.Run(row.name, func(t *testing.T) {
			sizes := []int{row.n, workScale * row.n}
			judges := []func() []Finding{row.judge(t, sizes[0]), row.judge(t, sizes[1])}
			findings := make([][]Finding, len(sizes))
			growth, small, large := cputest.Growth(workSamples, workScale,
				cputest.Again(func() { findings[0] = judges[0]() }), cputest.Again(func() { findings[1] = judges[1]() }))
			for index, found := range findings {
				if has := slices.ContainsFunc(found, func(finding Finding) bool { return finding.Rule == row.rule }); row.rule != "" && !has {
					t.Fatalf("the judgment of %d units finds no finding of %s, so this row reads nothing: %.300v", sizes[index], row.rule, found)
				} else if row.rule == "" && len(found) > 0 {
					t.Fatalf("the judgment of %d units finds %.300v, want none", sizes[index], found)
				}
			}
			t.Logf("%d units %d times: %v; %d units once: %v; %.1f times", sizes[0], workScale, small, sizes[1], large, growth)
			if !raceEnabled && growth >= workBound {
				t.Errorf("the work grew %.1f times, from %v for %d runs of %d units to %v for one run of %d, want less than %d times",
					growth, small, workScale, sizes[0], large, sizes[1], workBound)
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
