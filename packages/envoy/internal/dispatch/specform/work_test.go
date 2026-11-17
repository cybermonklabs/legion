package specform

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cputest"
)

// workGrowth is how many times the work of a judgment may grow when its text grows four times:
// work in proportion to the text grows about four times, and work for each pair of forms, links,
// texts or characters about sixteen.
const workGrowth = 6

// workSamples is how many times each text is judged, the two sizes in turn, so that a change of the
// machine's load reaches both. A busy machine can only add to the CPU time of a judgment, so the
// least of the samples is its work.
const workSamples = 3

// A judgment does work in proportion to its text, for each rule of the form and each shape that
// grows the text: many forms of one rule on one line, one long line, many short lines, many links,
// addresses and titles, many terms and many texts. Each row judges a text of n units and one of
// 4n, and the work of the larger grows less than workGrowth times. The work is the CPU time of the
// thread that judges (cputest.OfCall): a wait for a core is not in it, and work that allocates
// nothing is. A row whose judgment finds no finding of its rule at either size reads nothing, so
// it fails; a row of a rule "" is a shape that breaks no rule. A text of n units is 16 KB or more:
// Go's regexp reads a shorter text by a faster way, and the port's work for each byte grew 1.3
// times from a text of 8 KB to one of 32 KB for that reason alone.
func TestTheWorkOfAJudgmentGrowsInProportionToItsText(t *testing.T) {
	context := planted("ada.lovelace@example.com")
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	type judgment func(t *testing.T, n int) func() []Finding
	// checked judges one text of kind: before, n units, then after.
	checked := func(kind Kind, before, unit, after string) judgment {
		return func(_ *testing.T, n int) func() []Finding {
			in := Input{Texts: []Text{{Node: "n", Kind: kind, Body: before + strings.Repeat(unit, n) + after, Where: where}}}
			return func() []Finding { return Check(context, in) }
		}
	}
	// extracted reads a document of before, n units, each written with its number, and after, and
	// judges it.
	extracted := func(before, unit, after string) judgment {
		return func(t *testing.T, n int) func() []Finding {
			var markdown strings.Builder
			markdown.WriteString(before)
			for index := range n {
				fmt.Fprintf(&markdown, unit, index+1)
			}
			markdown.WriteString(after)
			document := markdown.String()
			return func() []Finding {
				read, err := ExtractDocument("spec.md", 1, document)
				if err != nil {
					t.Fatalf("read %d units of %q: %v", n, unit, err)
				}
				return Check(context, Input{Texts: read.Texts, Proposed: read.Terms})
			}
		}
	}
	const requirement, end = ":::requirement{#r}\nDispatch keeps", "\n:::\n"
	for _, row := range []struct {
		name  string
		rule  Rule
		n     int
		judge judgment
	}{
		{"many sentences on one line", ruleSentences, 1_000, checked(KindStatement, "", "Dispatch keeps it. ", "")},
		{"many short lines", ruleSentences, 1_000, checked(KindStatement, "", "Dispatch keeps it.\n", "")},
		{"one long sentence", ruleSentenceWords, 4_000, checked(KindStatement, "Dispatch keeps ", "word ", "it.")},
		{"many stops with no space after them", ruleStopNoSpace, 8_000, checked(KindClaim, "Dispatch reads ", "a.B ", "now.")},
		{"one word of many stops", "", 16_000, checked(KindClaim, "Dispatch reads ", ".", "A now.")},
		{"many semicolons", "ste100-semicolon", 16_000, checked(KindClaim, "Dispatch keeps it", ";", " and waits.")},
		{"many phrasal verbs", "ste100-phrasal-verb", 8_000, checked(KindClaim, "Legion will ", "spin up ", "the job.")},
		{"many marketing adjectives", "ste100-marketing-adjective", 8_000, checked(KindClaim, "Dispatch keeps a ", "seamless ", "log.")},
		{"many nominalizations", "ste100-nominalization", 4_000, checked(KindClaim, "Dispatch ", "performs a validation ", "of each upload.")},
		{"many synonyms of one verb", "ste100-synonym-rotation", 1_000, checked(KindClaim, "", "Check the config file. Then verify the output. ", "")},
		{"one list item that ends with and", "ste100-dangling-conjunction", 2_000, checked(KindClaim, "- Dispatch keeps ", "the record ", "and")},
		{"many references", ruleDispatchLink, 16_000, checked(KindName, "", "dispatch://ACME-1 ", "")},
		{"many keys", ruleDispatchLink, 8_000, checked(KindName, "", "ACME-1 ", "")},
		{"many ids", ruleDispatchLink, 4_000, checked(KindName, "", id+" ", "")},
		{"one reference before many closing brackets", ruleDispatchLink, 64_000, checked(KindName, "dispatch://ACME-1", ")", "")},
		{"many incidents", ruleIncident, 8_000, checked(KindName, "", "INC-1 ", "")},
		{"many numbers of a pull request", rulePullRequest, 8_000, checked(KindName, "", "#1 ", "")},
		{"many pull requests of a repository", rulePullRequest, 16_000, checked(KindName, "", "acme/widgets#1 ", "")},
		{"many addresses of a pull request", rulePullRequest, 4_000, checked(KindName, "", "github.com/acme/widgets/pull/1 ", "")},
		{"many email addresses", rulePerson, 8_000, checked(KindName, "", "ada@example.com ", "")},
		{"many handles", rulePerson, 8_000, checked(KindName, "", "@ada ", "")},
		{"many names of a person", rulePerson, 8_000, checked(KindName, "", "Ada Lovelace ", "")},
		{"many links", ruleDispatchLink, 2_000, extracted(requirement, " [it](dispatch://ACME-%d)", end)},
		{"many titles", ruleDispatchLink, 2_000, extracted(requirement, ` [it](https://example.com "ACME-%d")`, end)},
		{"many images", ruleDispatchLink, 2_000, extracted(requirement, " ![it](dispatch://ACME-%d)", end)},
		{"many terms", "ste100-semicolon", 1_000, extracted("# Terms\n\n", "- **term %d**: Dispatch keeps it; it waits.\n", "")},
		{"many statements", "ste100-semicolon", 1_000, extracted("", ":::requirement{#s-%d}\nDispatch keeps it; it waits.\n:::\n\n", "")},
		{"many statements of one id", "ste100-semicolon", 1_000, extracted("", ":::requirement{#s}\nDispatch keeps record %d; it waits.\n:::\n\n", "")},
		{"many footnotes", "ste100-semicolon", 1_000, extracted("", "[^%[1]d]: Dispatch keeps it; it waits.\n\n:::requirement{#s-%[1]d}\nDispatch keeps it[^%[1]d].\n:::\n\n", "")},
	} {
		t.Run(row.name, func(t *testing.T) {
			sizes := []int{row.n, 4 * row.n}
			judges := []func() []Finding{row.judge(t, sizes[0]), row.judge(t, sizes[1])}
			least := make([]time.Duration, len(sizes))
			for sample := range workSamples {
				for index, judge := range judges {
					var findings []Finding
					runtime.GC()
					took, _ := cputest.Measure(func(runs int) (time.Duration, error) { // a judgment reports no error
						return cputest.OfCall(func() {
							for range runs {
								findings = judge()
							}
						}), nil
					})
					if sample == 0 || took < least[index] {
						least[index] = took
					}
					if found := slices.ContainsFunc(findings, func(finding Finding) bool { return finding.Rule == row.rule }); row.rule != "" && !found {
						t.Fatalf("the judgment of %d units finds no finding of %s, so this row reads nothing: %.300v", sizes[index], row.rule, findings)
					} else if row.rule == "" && len(findings) > 0 {
						t.Fatalf("the judgment of %d units finds %.300v, want none", sizes[index], findings)
					}
				}
			}
			grows := float64(least[1]) / float64(least[0])
			t.Logf("%d units: %v; %d units: %v; %.1f times", sizes[0], least[0], sizes[1], least[1], grows)
			if grows >= workGrowth {
				t.Errorf("the work grew %.1f times, from %v for %d units to %v for %d, want less than %d times",
					grows, least[0], sizes[0], least[1], sizes[1], workGrowth)
			}
		})
	}
}
